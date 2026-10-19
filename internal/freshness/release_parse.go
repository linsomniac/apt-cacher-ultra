package freshness

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ReleaseMember is one member a Release/InRelease declares, its SHA256:
// and SHA512: entries merged (spec C5 "Hash blocks"). Path is
// suite-relative, e.g. "main/binary-amd64/Packages". At least one hash is
// set.
type ReleaseMember struct {
	Path   string
	SHA256 string // lowercase 64 hex chars; "" when only SHA512 is declared
	SHA512 string // lowercase 128 hex chars; "" when not declared
	Size   int64  // the SHA512 entry's size when there is one, else the SHA256 entry's
}

// MaxReleaseMembers caps how many entries ParseRelease will return. A
// real Ubuntu noble Release with by-hash on holds ~30k entries; the cap
// is loose enough for any plausible archive but bounds the memory cost
// of a hostile (but allowlisted) upstream that returns a synthetic
// 100M-line Release through a successful GPG-signed adoption.
const MaxReleaseMembers = 1_000_000

// scanBufCap is the per-line ceiling for the bufio.Scanner. Real Release
// lines are well under 1 KiB, but the scanner reads the entire file as
// a stream of lines and a single oversized line (e.g. an unintentional
// missing newline that joins two sections) would otherwise return
// bufio.ErrTooLong with no diagnostic. 1 MiB is comfortable headroom.
const scanBufCap = 1 << 20

// ParseRelease extracts the SHA256: and SHA512: blocks from verified
// Release-equivalent text — the cleartext payload of an InRelease, or the
// body of a detached or unsigned Release file — merging them per member
// path, as apt does (spec C5 "Hash blocks"): a member carries every hash
// declared for it, and its size comes from the SHA512 entry when there is
// one, else from the SHA256 entry. Members are returned in the order of
// their first entry.
//
// MD5Sum: and SHA1: blocks are ignored on purpose: a member declared only
// there is not returned. A Release with neither a SHA256 nor a SHA512
// block is an error, and so is a member declared twice in one block with
// a different hash or size (a malformed line in either block fails the
// parse, as before).
//
// AIDEV-NOTE: SHA512-only members (libnvidia-container's Release has only
// a SHA512 block) carry SHA256 == "": every consumer goes through the
// shared member verifier (member_verify.go), which derives the SHA-256 of
// the verified bytes, never through m.SHA256 alone.
//
// Metadata-self entries — "Release", "InRelease", "Release.gpg" — are
// silently dropped. apt-ftparchive's `release` subcommand emits the
// generated Release into its OWN SHA256 block as a directory-walk
// artifact, listing it at "stub size" (headers only, ~188 bytes) with
// the corresponding stub hash; the on-disk file we'd refetch is the
// FULL output (~1.5 KiB) with a different hash, so naively iterating
// these entries would dead-end at a content-length mismatch. apt
// itself never refetches Release/Release.gpg/InRelease via the SHA256
// block — it has those bytes already from the freshness fetch — so
// dropping them is faithful to apt semantics. Adoption inserts the
// metadata-self snapshot_member row(s) explicitly in step 6 of
// runShared — InRelease for inline mode, Release + Release.gpg for
// detached mode.
//
// AIDEV-NOTE: callers have already cryptographically verified the input
// (§7.6 verify step). This function is a pure text parser — it consults
// no trust store and performs no signature operation. Path validation
// here is defense-in-depth against a malformed-but-signed upstream.
func ParseRelease(text []byte) ([]ReleaseMember, error) {
	scanner := bufio.NewScanner(bytes.NewReader(text))
	scanner.Buffer(make([]byte, 0, scanBufCap), scanBufCap)

	var out []ReleaseMember
	at := make(map[string]int) // member path → its index in out
	type entry struct {
		hash string
		size int64
	}
	seen := map[string]map[string]entry{"SHA256": {}, "SHA512": {}} // per block, the first entry per path
	parsedRows := 0                                                 // distinct from len(out) — counts filtered entries too
	block := ""                                                     // "SHA256" or "SHA512" inside one of those blocks
	sawBlock := false
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if line == "" {
			// Blank line ends the current block. apt's spec allows
			// (but does not require) one between sections.
			block = ""
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if block == "" {
				continue
			}
			m, err := parseReleaseHashLine(line, block)
			if err != nil {
				return nil, fmt.Errorf("release: line %d: %w", lineNo, err)
			}
			// Cap on parsed rows, NOT on retained rows: an upstream
			// that pads a Release with a million metadata-self
			// entries would otherwise never trip the cap (they all
			// get filtered) yet still pay the per-line parse cost.
			// The caller-side body bound (~4 MiB for inline
			// InRelease) is the primary DoS gate; this is a
			// secondary bound that holds even if a future detached-
			// mode path streams a larger Release file.
			parsedRows++
			if parsedRows > MaxReleaseMembers {
				return nil, fmt.Errorf("release: exceeds %d members", MaxReleaseMembers)
			}
			if isMetadataSelfPath(m.Path) {
				continue
			}
			hash := m.SHA256
			if block == "SHA512" {
				hash = m.SHA512
			}
			if prev, dup := seen[block][m.Path]; dup {
				if prev.hash != hash || prev.size != m.Size {
					return nil, fmt.Errorf("release: line %d: %s declared twice in the %s block with different values", lineNo, m.Path, block)
				}
				continue
			}
			seen[block][m.Path] = entry{hash, m.Size}
			i, ok := at[m.Path]
			if !ok {
				i = len(out)
				at[m.Path] = i
				out = append(out, ReleaseMember{Path: m.Path})
			}
			switch block {
			case "SHA256":
				out[i].SHA256 = hash
				if out[i].SHA512 == "" {
					out[i].Size = m.Size
				}
			case "SHA512":
				out[i].SHA512 = hash
				out[i].Size = m.Size
			}
			continue
		}
		// Header line. Trim trailing whitespace; the header label
		// itself never legitimately contains spaces.
		switch strings.TrimRight(line, " \t") {
		case "SHA256:":
			block, sawBlock = "SHA256", true
		case "SHA512:":
			block, sawBlock = "SHA512", true
		default:
			block = ""
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("release: scan: %w", err)
	}
	if !sawBlock {
		return nil, errors.New("release: no SHA256 or SHA512 block")
	}
	if len(out) == 0 {
		return nil, errors.New("release: empty SHA256 and SHA512 blocks")
	}
	return out, nil
}

// parseAcquireByHash reports whether the Release/InRelease header
// advertises "Acquire-By-Hash: yes" — apt's signal that the archive
// serves every listed member under <dir>/by-hash/SHA256/<sha256>.
// Adoption uses this (REC 2) to fetch members from their content-
// addressed URL, which is immune to the publication race where the
// mutable path serves a newer revision than the InRelease declared.
//
// Only top-level header lines are consulted; indented lines (the
// SHA256/MD5Sum hash blocks) are skipped so a member literally named
// "Acquire-By-Hash: yes" can't spoof the flag. The value comparison is
// case-insensitive against "yes" (apt accepts yes/no).
func parseAcquireByHash(text []byte) bool {
	scanner := bufio.NewScanner(bytes.NewReader(text))
	scanner.Buffer(make([]byte, 0, scanBufCap), scanBufCap)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(key), "Acquire-By-Hash") {
			return strings.EqualFold(strings.TrimSpace(val), "yes")
		}
	}
	return false
}

// parseReleaseHashLine parses one entry of block ("SHA256" or "SHA512"):
// the hash lands in the member's field of that algorithm.
func parseReleaseHashLine(line, block string) (ReleaseMember, error) {
	// strings.Fields collapses runs of whitespace and strips the
	// leading indent. Real Release files pad the size column with
	// spaces for alignment; Fields sees through that.
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return ReleaseMember{}, fmt.Errorf("malformed line %q (want 3 fields, got %d)", line, len(fields))
	}
	hash := fields[0]
	valid := validHexSHA256
	if block == "SHA512" {
		valid = validHexSHA512
	}
	if !valid(hash) {
		return ReleaseMember{}, fmt.Errorf("invalid %s: %q", strings.ToLower(block), hash)
	}
	size, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return ReleaseMember{}, fmt.Errorf("invalid size %q: %w", fields[1], err)
	}
	if size < 0 {
		return ReleaseMember{}, fmt.Errorf("negative size: %d", size)
	}
	path := fields[2]
	if err := validateMemberPath(path); err != nil {
		return ReleaseMember{}, fmt.Errorf("invalid path %q: %w", path, err)
	}
	if block == "SHA512" {
		return ReleaseMember{Path: path, SHA512: hash, Size: size}, nil
	}
	return ReleaseMember{Path: path, SHA256: hash, Size: size}, nil
}

// validHexSHA256 reports whether s is exactly 64 lowercase hex characters.
//
// AIDEV-NOTE: kept local to the freshness package on purpose. The cache
// package has an identical helper; importing it just for this would
// couple parsing to storage and add no value. Both are tiny.
func validHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// validHexSHA512 reports whether s is exactly 128 lowercase hex characters.
func validHexSHA512(s string) bool {
	return len(s) == 128 && validHexSHA256(s[:64]) && validHexSHA256(s[64:])
}

// isMetadataSelfPath reports whether p names a metadata file the
// upstream emitted as a self-reference inside its own SHA256 block.
// These are not member files apt ever refetches via the block — see
// the doc comment on ParseRelease.
func isMetadataSelfPath(p string) bool {
	return p == "Release" || p == "Release.gpg" || p == "InRelease"
}

// validateMemberPath rejects suite-relative paths that could traverse
// out of the suite directory or are otherwise unsafe to use as on-disk
// or URL components. Reject ".." and "." even though filepath.Clean
// would resolve them: a Release listing "a/../etc/shadow" or
// "./Release" is malformed by definition, and silently rewriting it
// would mask the bug from the adoption_parse_failed log AND open an
// aliasing path past isMetadataSelfPath (e.g. "./Release" canonicalizes
// to "Release" downstream but won't match the exact-string filter).
//
// Reject empty path segments ("main//Packages") for the same reason —
// Go's path package and most HTTP servers normalize repeated slashes,
// so the on-disk fetch would alias to a different declared entry.
//
// Reject backslashes outright. Real apt repositories use forward
// slashes only; a backslash here can only be either a Windows-path
// confusion vector or an attempt to encode a separator the parser
// won't honor but a downstream component might.
//
// Reject percent-encoding entirely. Release files quote nothing —
// every byte is literal — so a percent sign in a path field is a
// signal that an upstream tried to smuggle a separator or dot
// segment past the literal-string checks above (e.g. "%2e/Release"
// or "Release%2egpg").
//
// Reject "?" and "#" outright. buildMemberURL composes the upstream
// URL textually (suite + "/" + relPath), so a member path of
// "Release?x" or "Release#y" fetches the literal "Release" file with
// a query/fragment delimiter — aliasing past the exact-string
// metadata-self filter and triggering a content-length mismatch
// against the stub size apt-ftparchive declares for the self-
// reference entry. Real Release files never contain these characters
// in member paths.
func validateMemberPath(p string) error {
	if p == "" {
		return errors.New("empty")
	}
	if strings.HasPrefix(p, "/") {
		return errors.New("absolute path not allowed")
	}
	if strings.ContainsRune(p, 0) {
		return errors.New("contains NUL")
	}
	if strings.ContainsRune(p, '\\') {
		return errors.New("contains backslash")
	}
	if strings.ContainsRune(p, '%') {
		return errors.New("contains percent-encoded byte")
	}
	if strings.ContainsAny(p, "?#") {
		return errors.New("contains URL delimiter")
	}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "..":
			return errors.New("contains .. segment")
		case ".":
			return errors.New("contains . segment")
		case "":
			return errors.New("contains empty segment")
		}
	}
	return nil
}
