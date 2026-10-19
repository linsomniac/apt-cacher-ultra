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
// signature: the armor header (after optional whitespace), or a binary
// signature packet — an old-format (0x88-0x8B) or new-format (0xC2)
// signature tag whose packet body starts with a signature version, 3 to 6
// (spec C3; RFC 9580 §4.2, §5.2).
//
// AIDEV-NOTE: the version byte is what keeps text out (ruling PD-10). A
// plain-text soft-404 starting with U+00A0 or "©" (UTF-8 lead byte 0xC2)
// passed the tag-only rule, and a false signed-seen marker blocks unsigned
// adoption of its suite for good (phase E). A non-conforming binary
// signature that fails here only earns no marker: the fail-open direction.
func SniffReleaseGPG(head []byte) bool {
	if bytes.HasPrefix(bytes.TrimLeft(head, " \t\r\n"), []byte(armorSigBegin)) {
		return true
	}
	off, ok := sigPacketBodyOffset(head)
	if !ok || len(head) <= off {
		return false
	}
	return head[off] >= 3 && head[off] <= 6
}

// sigPacketBodyOffset returns the offset of the packet body of the
// signature packet head starts with: the tag octet plus the length octets
// its header form gives (RFC 9580 §4.2). ok is false when head does not
// start with a signature packet tag (old format 0x88-0x8B, new format
// 0xC2 followed by a length octet).
func sigPacketBodyOffset(head []byte) (int, bool) {
	switch {
	case len(head) == 0:
		return 0, false
	case head[0] >= 0x88 && head[0] <= 0x8b:
		// Old format: the low two bits are the length type — one, two or
		// four length octets, or 3: an indeterminate length, none.
		return [4]int{2, 3, 5, 1}[head[0]&0x03], true
	case head[0] == 0xc2 && len(head) > 1:
		switch l := head[1]; {
		case l < 192: // one-octet length
			return 2, true
		case l <= 223: // two-octet length
			return 3, true
		case l == 255: // five-octet length
			return 6, true
		default: // 224-254: a partial body length, one octet
			return 2, true
		}
	}
	return 0, false
}

// MarkerBase returns the signed-seen marker base of a suite: the suite
// path with ONE trailing "/." removed, so the flat twins "/repo" and
// "/repo/." share a marker (spec C3). dists suites are unchanged.
func MarkerBase(suite string) string { return strings.TrimSuffix(suite, "/.") }

// ReleaseSniffBytes is how much of a body SniffRelease reads (spec C3:
// the first 64 KiB).
const ReleaseSniffBytes = 64 << 10

// SniffRelease reports whether head (the first bytes of a body, at most
// ReleaseSniffBytes are read) looks like a Release file: a "SHA256:",
// "SHA512:", "SHA1:" or "MD5Sum:" hash-block header line — the field name
// alone at the start of a line, trailing CR, space and tab ignored, as
// ParseRelease reads it — in the first 64 KiB (spec C3). An HTML error
// page or a plain-text stub has none.
func SniffRelease(head []byte) bool {
	if len(head) > ReleaseSniffBytes {
		head = head[:ReleaseSniffBytes]
	}
	for len(head) > 0 {
		line := head
		if i := bytes.IndexByte(head, '\n'); i >= 0 {
			line, head = head[:i], head[i+1:]
		} else {
			head = nil
		}
		switch string(bytes.TrimRight(line, "\r \t")) {
		case "SHA256:", "SHA512:", "SHA1:", "MD5Sum:":
			return true
		}
	}
	return false
}

// flatIndexMagic maps a flat Packages*/Sources* extension to its
// compression magic (spec C3 flat index sniff).
var flatIndexMagic = map[string][]byte{
	".gz":   {0x1f, 0x8b},
	".xz":   {0xfd, '7', 'z', 'X', 'Z', 0x00},
	".bz2":  {'B', 'Z', 'h'},
	".lzma": {0x5d, 0x00, 0x00},
	".lz4":  {0x04, 0x22, 0x4d, 0x18},
	".zst":  {0x28, 0xb5, 0x2f, 0xfd},
}

// SniffFlatIndex reports whether head passes the flat index sniff for the
// file named name (its last path segment, e.g. "Packages.xz"; spec C3):
// the compression magic for its extension, or for an uncompressed
// Packages/Sources plain text starting "Package:". Any other name fails.
func SniffFlatIndex(name string, head []byte) bool {
	ext, ok := flatIndexExt(name)
	if !ok {
		return false
	}
	if ext == "" {
		return bytes.HasPrefix(head, []byte("Package:"))
	}
	return bytes.HasPrefix(head, flatIndexMagic[ext])
}

// flatIndexExt returns the compression extension of a flat index file name
// (Packages or Sources, optionally with one of the flat grammar's
// extensions; "" for none).
func flatIndexExt(name string) (string, bool) {
	for _, stem := range []string{"Packages", "Sources"} {
		rest, ok := strings.CutPrefix(name, stem)
		if !ok {
			continue
		}
		if rest == "" {
			return "", true
		}
		if _, known := flatIndexMagic[rest]; known {
			return rest, true
		}
	}
	return "", false
}

// SuiteFile is what a request path is to its suite for spec C3: whether
// serving it creates the suite's row, writes its signed-seen marker, or
// records client interest.
type SuiteFile int

const (
	// NotSuiteFile: no row, no marker, no touch.
	NotSuiteFile SuiteFile = iota
	// FileInRelease is <suite>/InRelease.
	FileInRelease
	// FileRelease is <suite>/Release.
	FileRelease
	// FileReleaseGPG is <suite>/Release.gpg.
	FileReleaseGPG
	// FileFlatIndex is a flat suite's root Packages*/Sources* file. SuitePath
	// gives no flat suite before phase G, so the handler meets it only once
	// G adds the flat grammar there.
	FileFlatIndex
)

// MaxSuitePathBytes bounds the suite paths spec C3 tracks (ruling PD-9):
// a longer suite path is no suite, so no row, marker, touch or
// rejection-cache entry ever carries one, and every per-suite key in the
// tables and maps stays small.
const MaxSuitePathBytes = 1024

// ClassifySuiteFile reports what path is to suite, which must be
// SuitePath(path) ("" gives NotSuiteFile). A dists suite's root
// Packages*/Sources* is not a flat index, and a suite path longer than
// MaxSuitePathBytes gives NotSuiteFile: its files are served untracked.
func ClassifySuiteFile(suite, path string) SuiteFile {
	if suite == "" || len(suite) > MaxSuitePathBytes {
		return NotSuiteFile
	}
	name, ok := strings.CutPrefix(path, suite+"/")
	if !ok {
		return NotSuiteFile
	}
	switch name {
	case "InRelease":
		return FileInRelease
	case "Release":
		return FileRelease
	case "Release.gpg":
		return FileReleaseGPG
	}
	if _, ok := flatIndexExt(name); ok && !hasDistsSegment(suite) {
		return FileFlatIndex
	}
	return NotSuiteFile
}

// hasDistsSegment reports whether p has a "dists" segment: such a path is
// never flat (spec C2).
func hasDistsSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == "dists" {
			return true
		}
	}
	return false
}

// CreatesRow reports whether a sniff-passing serve of f creates its
// suite's row (spec C3): InRelease always; Release and flat indexes only
// with an adopter wired. Release.gpg never does.
func (f SuiteFile) CreatesRow(adopter bool) bool {
	switch f {
	case FileInRelease:
		return true
	case FileRelease, FileFlatIndex:
		return adopter
	}
	return false
}

// Marks reports whether a sniff-passing body of f is signed metadata that
// earns its suite's base a signed-seen marker (spec C3).
func (f SuiteFile) Marks() bool { return f == FileInRelease || f == FileReleaseGPG }

// Touches reports whether serving f records client interest in its suite
// (spec C3 TouchSuite): every suite file does.
func (f SuiteFile) Touches() bool { return f != NotSuiteFile }

// HeadBytes is how many leading bytes of a body f's sniff reads.
func (f SuiteFile) HeadBytes() int {
	if f == FileRelease {
		return ReleaseSniffBytes
	}
	return SniffHeadBytes
}

// Sniff reports whether head (at least HeadBytes of the body, or all of
// it) passes f's sniff (spec C3 table); NotSuiteFile never passes.
func (f SuiteFile) Sniff(path string, head []byte) bool {
	switch f {
	case FileInRelease:
		return SniffInRelease(head)
	case FileRelease:
		return SniffRelease(head)
	case FileReleaseGPG:
		return SniffReleaseGPG(head)
	case FileFlatIndex:
		return SniffFlatIndex(path[strings.LastIndexByte(path, '/')+1:], head)
	}
	return false
}

// DecodableClearsign returns b ready for go-crypto's clearsign decoder:
// when b's first line passes SniffInRelease but carries trailing blanks —
// which apt's rule ignores and RFC 4880 §6.2 allows after an armor header
// line, but go-crypto v1.4.1 refuses — it returns a copy whose first line
// is exactly the marker; otherwise b itself. Nothing else changes, so
// neither does the signed text.
//
// AIDEV-NOTE: ruling R-D2 (phase D): adoption accepts exactly apt's first
// line. Every decode of an InRelease envelope (the verifier, reconcile's
// inReleasePlaintext) goes through this, so a body apt accepts is never
// refused, nor later found undecodable, for its marker line alone.
func DecodableClearsign(b []byte) []byte {
	if !SniffInRelease(b) {
		return b
	}
	end := bytes.IndexByte(b, '\n')
	if end < 0 {
		end = len(b)
	}
	if end == len(clearsignBegin) || (end == len(clearsignBegin)+1 && b[end-1] == '\r') {
		return b
	}
	return append([]byte(clearsignBegin), b[end:]...)
}
