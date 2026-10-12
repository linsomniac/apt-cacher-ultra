package aptpath

import (
	"bytes"
	"strings"
)

// SniffHeadBytes is how many leading bytes of a body the sniffs need.
const SniffHeadBytes = 512

const (
	clearsignBegin = "-----BEGIN PGP SIGNED MESSAGE-----"
	armorSigBegin  = "-----BEGIN PGP SIGNATURE-----"
)

// SniffInRelease reports whether head (the first bytes of a body) passes
// apt's InRelease rule: the FIRST line is exactly the clearsign marker,
// ignoring trailing CR, space and tab (spec C3). Leading whitespace or a
// BOM fails, as in apt.
//
// AIDEV-NOTE: keep this exactly apt's rule — never strip leading
// whitespace or a BOM. A sniff is a cheap signal of shape, not proof of a
// signature: whatever relies on it for trust (signed-seen markers) must
// not relax it beyond what apt itself would accept as clearsigned.
func SniffInRelease(head []byte) bool {
	line := head
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return string(bytes.TrimRight(line, "\r \t")) == clearsignBegin
}

// SniffReleaseGPG reports whether head looks like a detached OpenPGP
// signature: the armor header (after optional whitespace) or a binary
// signature packet tag (old format 0x88-0x8B, new format 0xC2).
func SniffReleaseGPG(head []byte) bool {
	if bytes.HasPrefix(bytes.TrimLeft(head, " \t\r\n"), []byte(armorSigBegin)) {
		return true
	}
	if len(head) == 0 {
		return false
	}
	b := head[0]
	return (b >= 0x88 && b <= 0x8b) || b == 0xc2
}

// MarkerBase returns the signed-seen marker base of a suite: the suite
// path with ONE trailing "/." removed, so the flat twins "/repo" and
// "/repo/." share a marker (spec C3). dists suites are unchanged.
func MarkerBase(suite string) string { return strings.TrimSuffix(suite, "/.") }
