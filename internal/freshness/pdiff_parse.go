package freshness

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// PdiffPatchRef is one (filename, declared SHA256) entry from the
// SHA256-Download block of a Packages.diff/Index or Sources.diff/Index
// file. Filename is the basename only (e.g. "2026-05-09-1234.56.gz");
// the suite-relative path is computed by the caller as
// dirname-of-Index + "/" + Filename per SPEC6_5 §7.3.
type PdiffPatchRef struct {
	Filename string
	SHA256   string
	Size     int64
}

// MaxPdiffPatchEntries caps how many SHA256-Download entries
// ParsePdiffIndex will emit. Real Packages.diff/Index files list
// roughly 7–14 patches (one per recent apt-ftparchive run); 1024 is
// generous headroom and bounds the memory cost of a hostile-but-signed
// upstream feeding us a synthetic Index through a successful adoption.
const MaxPdiffPatchEntries = 1024

// pdiffPatchFilenameRE enforces the SPEC6_5 §6.1 / §11 H6 patch-name
// shape: digits, dots, and dashes followed by `.gz`. Captures the real
// apt-ftparchive output (e.g. `2026-05-09-1234.56.gz`) without
// admitting `/`-bearing or traversal-shaped filenames. The class
// excludes `..` because two literal dots in a row is still just two
// dots — and a filename containing `..` does not encode path
// traversal once the outer dirname-join is performed (the resulting
// member path is `<dir>/..gz`, not `<parent>/gz`).
var pdiffPatchFilenameRE = regexp.MustCompile(`^[0-9.-]+\.gz$`)

// ParsePdiffIndex reads a Packages.diff/Index or Sources.diff/Index
// file and returns the SHA256-Download block as PdiffPatchRef entries.
// SPEC6_5 §7.3: only the SHA256-Download form is honored — that's the
// compressed, on-the-wire form apt actually fetches. The
// SHA256-Patches block (uncompressed form) is ignored because apt
// decompresses client-side after fetch and the cache never serves
// the uncompressed form.
//
// Per SPEC6_5 §11 H5: an Index with no SHA256-Download block returns
// (nil, nil) — empty refs are NOT a parse error. The Index file
// itself is still adopted as a snapshot_member by the outer adoption
// loop; the absent SHA256-Download just means no `package_hash` rows
// are inserted for patch files.
//
// Per SPEC6_5 §11 H6: entries whose filename doesn't match the
// digit/dot/dash + `.gz` shape are silently skipped. The cap on
// total emitted refs is MaxPdiffPatchEntries.
//
// AIDEV-NOTE: callers pass already-decompressed bytes — Index files
// are normally served uncompressed, so this is usually a no-op. If a
// future repo publishes a compressed Index variant the fetch layer
// would handle decompression, not this parser.
// AIDEV-NOTE: field-name matching is case-insensitive per RFC822 /
// Debian policy. Multi-line continuation (leading space/tab) is
// recognized; only the SHA256-Download block carries data we keep.
func ParsePdiffIndex(text []byte) ([]PdiffPatchRef, error) {
	var out []PdiffPatchRef
	scanner := bufio.NewScanner(bytes.NewReader(text))
	scanner.Buffer(make([]byte, 0, scanBufCap), scanBufCap)

	inDownloadBlock := false
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if line == "" {
			inDownloadBlock = false
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if !inDownloadBlock {
				continue
			}
			ref, ok := parsePdiffDownloadLine(line)
			if !ok {
				continue
			}
			if !pdiffPatchFilenameRE.MatchString(ref.Filename) {
				continue
			}
			if len(out) >= MaxPdiffPatchEntries {
				return nil, fmt.Errorf("exceeds %d pdiff patch entries", MaxPdiffPatchEntries)
			}
			out = append(out, ref)
			continue
		}
		idx := strings.IndexByte(line, ':')
		if idx <= 0 {
			return nil, fmt.Errorf("pdiff index: line %d: malformed header %q", lineNo, line)
		}
		field := strings.TrimSpace(line[:idx])
		if strings.EqualFold(field, "SHA256-Download") {
			inDownloadBlock = true
		} else {
			inDownloadBlock = false
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("pdiff index: scan: %w", err)
	}
	return out, nil
}

// parsePdiffDownloadLine parses one line from a SHA256-Download block:
// " <sha256> <size> <filename>". Whitespace tolerance via Fields.
// Returns the ref on success; (zero, false) on any malformed shape.
// Hash hex shape is validated via validHexSHA256 (the §6.2 contract);
// filename shape is validated by the caller against the regex.
func parsePdiffDownloadLine(line string) (PdiffPatchRef, bool) {
	parts := strings.Fields(line)
	if len(parts) != 3 {
		return PdiffPatchRef{}, false
	}
	hash := strings.ToLower(parts[0])
	if !validHexSHA256(hash) {
		return PdiffPatchRef{}, false
	}
	n, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || n < 0 {
		return PdiffPatchRef{}, false
	}
	return PdiffPatchRef{
		Filename: parts[2],
		SHA256:   hash,
		Size:     n,
	}, true
}

// archFromPdiffIndexPath extracts the architecture tag for a pdiff
// Index path per SPEC6_5 §7.3: `binary-<arch>/Packages.diff/Index`
// yields the binary arch; `source/Sources.diff/Index` yields the
// pseudo-arch "source". Returns ("", false) when the path doesn't
// match either shape.
func archFromPdiffIndexPath(p string) (arch string, ok bool) {
	if m := archFilterBinaryRE.FindStringSubmatch(p); m != nil &&
		strings.HasSuffix(p, "/Packages.diff/Index") {
		return m[1], true
	}
	if archFilterSourceRE.MatchString(p) &&
		strings.HasSuffix(p, "/Sources.diff/Index") {
		return "source", true
	}
	return "", false
}
