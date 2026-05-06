package freshness

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ReleaseMember is one row of the SHA256: block in a Release/InRelease
// file. Path is suite-relative, e.g. "main/binary-amd64/Packages".
type ReleaseMember struct {
	Path   string
	SHA256 string // lowercase 64 hex chars
	Size   int64
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

// ParseRelease extracts the SHA256: block from verified Release-equivalent
// text — the cleartext payload of an InRelease, or the body of a detached
// Release file. Members are returned in source order.
//
// MD5Sum: and SHA1: blocks are ignored on purpose; Phase 2 trusts SHA256
// only. A Release that lists no SHA256 block at all is an error.
//
// AIDEV-NOTE: callers have already cryptographically verified the input
// (§7.6 verify step). This function is a pure text parser — it consults
// no trust store and performs no signature operation. Path validation
// here is defense-in-depth against a malformed-but-signed upstream.
func ParseRelease(text []byte) ([]ReleaseMember, error) {
	scanner := bufio.NewScanner(bytes.NewReader(text))
	scanner.Buffer(make([]byte, 0, scanBufCap), scanBufCap)

	var out []ReleaseMember
	inSHA256 := false
	sawSHA256Block := false
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if line == "" {
			// Blank line ends the current block. apt's spec allows
			// (but does not require) one between sections.
			inSHA256 = false
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if !inSHA256 {
				continue
			}
			m, err := parseReleaseHashLine(line)
			if err != nil {
				return nil, fmt.Errorf("release: line %d: %w", lineNo, err)
			}
			if len(out) >= MaxReleaseMembers {
				return nil, fmt.Errorf("release: exceeds %d members", MaxReleaseMembers)
			}
			out = append(out, m)
			continue
		}
		// Header line. Trim trailing whitespace; the header label
		// itself never legitimately contains spaces.
		switch strings.TrimRight(line, " \t") {
		case "SHA256:":
			inSHA256 = true
			sawSHA256Block = true
		default:
			inSHA256 = false
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("release: scan: %w", err)
	}
	if !sawSHA256Block {
		return nil, errors.New("release: no SHA256 block")
	}
	if len(out) == 0 {
		return nil, errors.New("release: empty SHA256 block")
	}
	return out, nil
}

func parseReleaseHashLine(line string) (ReleaseMember, error) {
	// strings.Fields collapses runs of whitespace and strips the
	// leading indent. Real Release files pad the size column with
	// spaces for alignment; Fields sees through that.
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return ReleaseMember{}, fmt.Errorf("malformed line %q (want 3 fields, got %d)", line, len(fields))
	}
	hash := fields[0]
	if !validHexSHA256(hash) {
		return ReleaseMember{}, fmt.Errorf("invalid sha256: %q", hash)
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
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// validateMemberPath rejects suite-relative paths that could traverse
// out of the suite directory or are otherwise unsafe to use as on-disk
// or URL components. Reject ".." even though filepath.Clean would
// resolve it: a Release listing "a/../etc/shadow" is malformed by
// definition, and silently rewriting it would mask the bug from the
// adoption_parse_failed log.
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
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return errors.New("contains .. segment")
		}
	}
	return nil
}
