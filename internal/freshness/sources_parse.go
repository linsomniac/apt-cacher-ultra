package freshness

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// SourceRef is one (file, declared SHA256) pair extracted from a
// stanza's Checksums-Sha256 block in an apt Sources file. Because
// Sources files declare a `Directory:` once per stanza and list
// multiple files per source package (typically `.dsc`, an upstream
// tarball, and a debian patches/tarball), one stanza yields multiple
// SourceRef entries — each sharing the same PackageName but with
// distinct filenames.
type SourceRef struct {
	// Path is the suite-relative artifact path: <Directory>/<filename>.
	// E.g. "pool/main/b/bash/bash_5.1-2.dsc".
	Path string

	// SHA256 is the declared 64-lowercase-hex hash from the stanza's
	// Checksums-Sha256 block.
	SHA256 string

	// Size is the declared byte count from the same block; 0 when the
	// stanza omitted it (rare in well-formed Sources files).
	Size int64

	// PackageName is the source package name (the stanza's `Package:`
	// header). Stored on package_hash rows for the SPEC6_5 §10 status
	// surface.
	PackageName string
}

// MaxSourceStanzas caps how many SourceRefs ParseSources will return.
// Real-world debian-main Sources is roughly 32k stanzas × ~3 files
// each ≈ 100k entries; 1M leaves headroom and matches the
// MaxPackageStanzas posture.
const MaxSourceStanzas = 1_000_000

// SourcesParseStats reports counts ParseSources observed during a
// single pass. StanzaCount totals every observed stanza — including
// those skipped for missing required fields — so the SPEC6_5 §10.2
// `source_parsed` Debug log can report the per-Sources-file size of
// the input alongside the per-Sources-file row count.
type SourcesParseStats struct {
	StanzaCount int
}

// ParseSources parses an apt Sources file (uncompressed) and returns
// every (file, SHA256) pair declared in the stanza Checksums-Sha256
// blocks. SPEC6_5 §7.1 / §1.2: only the SHA256 form is honored. The
// MD5 (`Files:`) and SHA1 (`Checksums-Sha1:`) blocks are ignored —
// Phase 2's trust-SHA256-only posture refuses MD5/SHA1 fallback even
// for source artifacts.
//
// A stanza is silently skipped when:
//
//   - it lacks `Package:` or `Directory:`,
//   - it has no `Checksums-Sha256:` block (per SPEC6_5 §11 H4: source
//     stanzas with only MD5/SHA1 hashes drop their rows), or
//   - its declared path fails validateMemberPath (defense in depth
//     against `..` segments per SPEC6_5 §11 H11 — even though signed
//     input means the upstream has already vouched for the bytes,
//     we refuse to materialize a `package_hash` row for an unsafe
//     suite-relative path).
//
// Per-entry filename validation also rejects `/`, backslash, and NUL
// — a Sources `Checksums-Sha256` filename is a basename within the
// stanza's `Directory:`, so any path-separator-bearing filename is
// either malformed input or an attempt to escape the directory.
//
// Whole-file fatal errors (malformed header line, scanner buffer
// exhausted, or stanza-cap exceeded) return non-nil err. The caller
// emits SPEC6_5 §10.2 `source_parse_failed` Warn at the Sources-file
// granularity.
//
// AIDEV-NOTE: callers pass already-decompressed bytes — decompression
// of Sources.gz / Sources.xz is the fetch layer's job, not parsing.
// AIDEV-NOTE: field-name matching is case-insensitive per
// RFC822/Debian policy. Multi-line continuation (leading space/tab)
// is recognized; only the Checksums-Sha256 block carries data we keep.
func ParseSources(text []byte) ([]SourceRef, SourcesParseStats, error) {
	var out []SourceRef
	var stats SourcesParseStats
	scanner := bufio.NewScanner(bytes.NewReader(text))
	scanner.Buffer(make([]byte, 0, scanBufCap), scanBufCap)

	var (
		packageName   string
		directory     string
		sha256Entries []sourceChecksumEntry
		inSha256Block bool
		stanzaOpen    bool
		lineNo        int
	)

	flush := func() error {
		hadStanza := stanzaOpen
		defer func() {
			packageName = ""
			directory = ""
			sha256Entries = nil
			inSha256Block = false
			stanzaOpen = false
		}()
		if hadStanza {
			stats.StanzaCount++
		}
		if packageName == "" || directory == "" || len(sha256Entries) == 0 {
			return nil
		}
		if err := validateMemberPath(directory); err != nil {
			return nil
		}
		for _, e := range sha256Entries {
			if !validHexSHA256(e.sha256) {
				continue
			}
			if e.filename == "" ||
				strings.ContainsAny(e.filename, "/\\\x00") {
				continue
			}
			if e.filename == "." || e.filename == ".." {
				continue
			}
			full := directory + "/" + e.filename
			if err := validateMemberPath(full); err != nil {
				continue
			}
			if len(out) >= MaxSourceStanzas {
				return fmt.Errorf("exceeds %d source entries", MaxSourceStanzas)
			}
			out = append(out, SourceRef{
				Path:        full,
				SHA256:      e.sha256,
				Size:        e.size,
				PackageName: packageName,
			})
		}
		return nil
	}

	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return nil, stats, fmt.Errorf("sources: line %d: %w", lineNo, err)
			}
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if !inSha256Block {
				continue
			}
			if entry, ok := parseSourcesChecksumLine(line); ok {
				sha256Entries = append(sha256Entries, entry)
			}
			continue
		}
		idx := strings.IndexByte(line, ':')
		if idx <= 0 {
			return nil, stats, fmt.Errorf("sources: line %d: malformed header %q", lineNo, line)
		}
		stanzaOpen = true
		field := strings.TrimSpace(line[:idx])
		switch strings.ToLower(field) {
		case "package":
			packageName = strings.TrimSpace(line[idx+1:])
			inSha256Block = false
		case "directory":
			directory = strings.TrimSpace(line[idx+1:])
			inSha256Block = false
		case "checksums-sha256":
			inSha256Block = true
		default:
			inSha256Block = false
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, stats, fmt.Errorf("sources: scan: %w", err)
	}
	if err := flush(); err != nil {
		return nil, stats, fmt.Errorf("sources: tail: %w", err)
	}
	if len(out) == 0 && len(bytes.TrimSpace(text)) > 0 {
		return nil, stats, errors.New("sources: no stanzas with Package + Directory + Checksums-Sha256")
	}
	return out, stats, nil
}

type sourceChecksumEntry struct {
	sha256   string
	size     int64
	filename string
}

// parseSourcesChecksumLine parses one entry from a Checksums-Sha256
// block: " <sha256> <size> <filename>". Whitespace is collapsed via
// strings.Fields so tab/space variations all work. Returns the
// parsed entry on success; (zero, false) on any malformed shape.
func parseSourcesChecksumLine(line string) (sourceChecksumEntry, bool) {
	parts := strings.Fields(line)
	if len(parts) != 3 {
		return sourceChecksumEntry{}, false
	}
	n, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || n < 0 {
		return sourceChecksumEntry{}, false
	}
	return sourceChecksumEntry{
		sha256:   strings.ToLower(parts[0]),
		size:     n,
		filename: parts[2],
	}, true
}
