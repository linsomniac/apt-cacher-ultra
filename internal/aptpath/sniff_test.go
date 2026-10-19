package aptpath

import (
	"strings"
	"testing"
)

func TestSniffInRelease(t *testing.T) {
	cases := []struct {
		head string
		want bool
	}{
		{"-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA512\n", true},
		{"-----BEGIN PGP SIGNED MESSAGE-----\r\nHash: SHA512\r\n", true},
		{"-----BEGIN PGP SIGNED MESSAGE----- \t\n", true},
		{"-----BEGIN PGP SIGNED MESSAGE-----", true},     // whole body is the marker line
		{" -----BEGIN PGP SIGNED MESSAGE-----\n", false}, // apt: the FIRST line must be the marker
		{"\n-----BEGIN PGP SIGNED MESSAGE-----\n", false},
		{"Origin: Plain\nSHA256:\n", false},
		{"-----BEGIN PGP SIGNED MESSAGE-----X\n", false},
		{"\xef\xbb\xbf-----BEGIN PGP SIGNED MESSAGE-----\n", false}, // a BOM is not stripped
		{"", false},
	}
	for _, c := range cases {
		if got := SniffInRelease([]byte(c.head)); got != c.want {
			t.Errorf("SniffInRelease(%q) = %v, want %v", c.head, got, c.want)
		}
	}
}

func TestSniffReleaseGPG(t *testing.T) {
	cases := []struct {
		head []byte
		want bool
	}{
		{[]byte("-----BEGIN PGP SIGNATURE-----\n\nwsBc"), true},
		{[]byte("\n  -----BEGIN PGP SIGNATURE-----\n"), true},
		// A binary signature packet: the signature version (3-6) at the
		// packet body's offset, which the header form gives (PD-10).
		{[]byte{0x88, 0x5e, 0x04}, true},                         // old format, one-octet length
		{[]byte{0x89, 0x01, 0x1c, 0x04}, true},                   // two-octet length
		{[]byte{0x8a, 0x00, 0x00, 0x01, 0x1c, 0x04}, true},       // four-octet length
		{[]byte{0x8b, 0x04}, true},                               // indeterminate length
		{[]byte{0xc2, 0x5e, 0x04}, true},                         // new format, one-octet length
		{[]byte{0xc2, 0xc0, 0x1c, 0x04}, true},                   // two-octet length
		{[]byte{0xc2, 0xdf, 0xff, 0x04}, true},                   // two-octet length, top of its range
		{[]byte{0xc2, 0xff, 0x00, 0x00, 0x01, 0x1c, 0x04}, true}, // five-octet length
		{[]byte{0xc2, 0xe0, 0x04}, true},                         // partial body length
		{[]byte{0xc2, 0xfe, 0x04}, true},                         // partial body length, top of its range
		{[]byte{0xc2, 0x5e, 0x03}, true},                         // version 3
		{[]byte{0xc2, 0x5e, 0x05}, true},                         // version 5
		{[]byte{0x88, 0x5e, 0x06}, true},                         // version 6
		{[]byte{0xc2, 0x5e, 0x02}, false},                        // version 2
		{[]byte{0x88, 0x5e, 0x07}, false},                        // version 7
		{[]byte{0x89, 0x04, 0x00, 0x00}, false},                  // a version where a length octet is
		{[]byte{0xc2, 0xc0, 0x04, 0x00}, false},
		{[]byte{0xc2, 0xff, 0x04, 0x00, 0x00, 0x00, 0x00}, false},
		// Truncated heads: the tag-only rule passed the first three.
		{[]byte{0x88, 0x01}, false},
		{[]byte{0x8b, 0x00}, false},
		{[]byte{0xc2, 0x01}, false},
		{[]byte{0x88}, false},
		{[]byte{0xc2}, false},
		{[]byte{0x8a, 0x00, 0x00, 0x01, 0x1c}, false},
		{[]byte{0xc2, 0xff, 0x00, 0x00, 0x01, 0x1c}, false},
		// Neighbouring tags with a valid length and version (A-17).
		{[]byte{0x87, 0x04}, false},
		{[]byte{0x8c, 0x5e, 0x04}, false}, // just above the old-format range (A-17)
		{[]byte{0xc1, 0x5e, 0x04}, false},
		{[]byte{0xc3, 0x5e, 0x04}, false}, // just above the new-format tag (A-17)
		// Text whose first byte is the UTF-8 lead byte 0xC2.
		{[]byte("\u00a0Not Found\n"), false},
		{[]byte("© 2026 Example. All rights reserved.\n"), false},
		{[]byte("<html>Not Found</html>"), false},
		{[]byte("-----BEGIN PGP SIGNED MESSAGE-----\n"), false},
		{nil, false},
	}
	for _, c := range cases {
		if got := SniffReleaseGPG(c.head); got != c.want {
			t.Errorf("SniffReleaseGPG(%q) = %v, want %v", c.head, got, c.want)
		}
	}
}

func TestMarkerBase(t *testing.T) {
	for in, want := range map[string]string{
		"/ubuntu/dists/noble": "/ubuntu/dists/noble",
		"/repo":               "/repo",
		"/repo/.":             "/repo",
		"/repo/./.":           "/repo/.", // only one trailing "/." is removed
	} {
		if got := MarkerBase(in); got != want {
			t.Errorf("MarkerBase(%q) = %q, want %q", in, got, want)
		}
	}
}

// Spec C3 Release sniff: a hash-block header line in the first 64 KiB.
func TestSniffRelease(t *testing.T) {
	inside := strings.Repeat("y", ReleaseSniffBytes-len("SHA256:\n")-1) + "\nSHA256:\n"
	outside := strings.Repeat("y", ReleaseSniffBytes) + "\nSHA256:\n"
	atEdge := strings.Repeat("y", ReleaseSniffBytes-len("SHA256:")-1) + "\nSHA256:\n tail"
	cases := []struct {
		name, head string
		want       bool
	}{
		{"SHA256 block", "Origin: Debian\nLabel: Debian\nSHA256:\n 0123 12 main/binary-amd64/Packages\n", true},
		{"MD5Sum block", "Origin: x\nMD5Sum:\n 0123 12 main/Sources\n", true},
		{"SHA1 block", "SHA1:\n 0123 12 Packages\n", true},
		{"SHA512 block, CRLF", "Origin: x\r\nSHA512:\r\n 0123 12 Packages\r\n", true},
		{"trailing blanks", "SHA256: \t\n", true},
		{"last line, no newline", "Origin: x\nSHA256:", true},
		{"header ends at 64 KiB", inside, true},
		{"colon at byte 65536", atEdge, true},
		{"header after 64 KiB", outside, false},
		{"HTML soft-404", "<html><body>Not Found</body></html>\n", false},
		{"value on the header line", "SHA256: 0123 12 Packages\n", false},
		{"indented", " SHA256:\n", false},
		{"lower case", "sha256:\n", false},
		{"no hash block", "Origin: x\nDescription: none\n", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		if got := SniffRelease([]byte(c.head)); got != c.want {
			t.Errorf("%s: SniffRelease = %v, want %v", c.name, got, c.want)
		}
	}
}

// Spec C3 flat index sniff: the compression magic for the extension, or
// plain text starting "Package:".
func TestSniffFlatIndex(t *testing.T) {
	cases := []struct {
		name, head string
		want       bool
	}{
		{"Packages", "Package: foo\nVersion: 1\n", true},
		{"Packages", "Package:foo\n", true},
		{"Sources", "Package: foo\nBinary: foo\n", true},
		{"Packages", "", false},
		{"Packages", " Package: foo\n", false},
		{"Packages", "<html>Not Found</html>", false},
		{"Packages.gz", "\x1f\x8b\x08\x00", true},
		{"Packages.gz", "Package: foo\n", false},
		{"Packages.gz", "\x1f", false},
		{"Packages.xz", "\xfd7zXZ\x00\x00", true},
		{"Packages.xz", "\x1f\x8b\x08\x00", false},
		{"Packages.bz2", "BZh91AY&SY", true},
		{"Packages.lzma", "\x5d\x00\x00\x80\x00", true},
		{"Packages.lz4", "\x04\x22\x4d\x18\x64", true},
		{"Sources.zst", "\x28\xb5\x2f\xfd\x04", true},
		{"Sources.gz", "\x1f\x8b\x08\x00", true},
		{"Packages.Z", "\x1f\x9d", false},
		{"Packages.diff/Index", "SHA1-Current: x\n", false},
		{"InRelease", "-----BEGIN PGP SIGNED MESSAGE-----\n", false},
	}
	for _, c := range cases {
		if got := SniffFlatIndex(c.name, []byte(c.head)); got != c.want {
			t.Errorf("SniffFlatIndex(%q, %q) = %v, want %v", c.name, c.head, got, c.want)
		}
	}
}

// Spec C3: what a request path is to its suite. A dists suite's root
// Packages is no flat index (R-D3: the flat branch waits for phase G's
// grammar, but the classifier knows it now).
func TestClassifySuiteFile(t *testing.T) {
	cases := []struct {
		suite, path string
		want        SuiteFile
	}{
		{"/u/dists/noble", "/u/dists/noble/InRelease", FileInRelease},
		{"/u/dists/noble", "/u/dists/noble/Release", FileRelease},
		{"/u/dists/noble", "/u/dists/noble/Release.gpg", FileReleaseGPG},
		{"/u/dists/noble", "/u/dists/noble/main/binary-amd64/Packages.gz", NotSuiteFile},
		{"/u/dists/noble", "/u/dists/noble/Packages.gz", NotSuiteFile},
		{"/u/dists/noble", "/u/dists/noble/main/InRelease", NotSuiteFile},
		{"/u/dists/noble", "/u/dists/noble", NotSuiteFile},
		{"/u/dists/noble", "/u/dists/nobleX/InRelease", NotSuiteFile},
		{"/repo/flat", "/repo/flat/InRelease", FileInRelease},
		{"/repo/flat", "/repo/flat/Packages.gz", FileFlatIndex},
		{"/repo/flat", "/repo/flat/Sources", FileFlatIndex},
		{"/repo/flat/.", "/repo/flat/./Packages.xz", FileFlatIndex},
		{"/repo/flat", "/repo/flat/Packages.diff/Index", NotSuiteFile},
		{"/repo/flat", "/repo/flat/Packages_1.0_all.deb", NotSuiteFile},
		{"/repo/distsy", "/repo/distsy/Packages.gz", FileFlatIndex},
		{"", "/x/InRelease", NotSuiteFile},
	}
	for _, c := range cases {
		if got := ClassifySuiteFile(c.suite, c.path); got != c.want {
			t.Errorf("ClassifySuiteFile(%q, %q) = %d, want %d", c.suite, c.path, got, c.want)
		}
	}
}

// Ruling PD-9: a suite path over 1 KiB is no suite (its files are served
// untracked); one of exactly 1 KiB is.
func TestClassifySuiteFile_SuitePathBound(t *testing.T) {
	at := "/u/dists/" + strings.Repeat("x", MaxSuitePathBytes-len("/u/dists/"))
	if got := ClassifySuiteFile(at, at+"/InRelease"); got != FileInRelease {
		t.Errorf("a %d-byte suite's InRelease = %d, want %d", len(at), got, FileInRelease)
	}
	over, flat := at+"y", "/r/"+strings.Repeat("f", MaxSuitePathBytes-len("/r/")+1)
	for _, p := range []string{over + "/InRelease", over + "/Release", over + "/Release.gpg", flat + "/Packages.gz"} {
		suite := p[:strings.LastIndexByte(p, '/')]
		if got := ClassifySuiteFile(suite, p); got != NotSuiteFile {
			t.Errorf("a %d-byte suite's %s = %d, want %d", len(suite), p[len(suite)+1:], got, NotSuiteFile)
		}
	}
}

// Spec C3: which suite files create a row (InRelease always; Release and
// flat indexes only with an adopter), earn a marker (signed anchors), and
// record client interest (all of them).
func TestSuiteFile_Rules(t *testing.T) {
	cases := []struct {
		f                                  SuiteFile
		createsP1, createsAd, marks, touch bool
		head                               int
	}{
		{NotSuiteFile, false, false, false, false, SniffHeadBytes},
		{FileInRelease, true, true, true, true, SniffHeadBytes},
		{FileRelease, false, true, false, true, ReleaseSniffBytes},
		{FileReleaseGPG, false, false, true, true, SniffHeadBytes},
		{FileFlatIndex, false, true, false, true, SniffHeadBytes},
	}
	for _, c := range cases {
		if got := c.f.CreatesRow(false); got != c.createsP1 {
			t.Errorf("%d.CreatesRow(no adopter) = %v, want %v", c.f, got, c.createsP1)
		}
		if got := c.f.CreatesRow(true); got != c.createsAd {
			t.Errorf("%d.CreatesRow(adopter) = %v, want %v", c.f, got, c.createsAd)
		}
		if got := c.f.Marks(); got != c.marks {
			t.Errorf("%d.Marks = %v, want %v", c.f, got, c.marks)
		}
		if got := c.f.Touches(); got != c.touch {
			t.Errorf("%d.Touches = %v, want %v", c.f, got, c.touch)
		}
		if got := c.f.HeadBytes(); got != c.head {
			t.Errorf("%d.HeadBytes = %d, want %d", c.f, got, c.head)
		}
	}
	sniffs := []struct {
		f    SuiteFile
		path string
		head string
	}{
		{FileInRelease, "/u/dists/s/InRelease", "-----BEGIN PGP SIGNED MESSAGE-----\n"},
		{FileRelease, "/u/dists/s/Release", "Origin: x\nSHA256:\n"},
		{FileReleaseGPG, "/u/dists/s/Release.gpg", "-----BEGIN PGP SIGNATURE-----\n"},
		{FileFlatIndex, "/repo/flat/Packages.gz", "\x1f\x8b\x08\x00"},
	}
	for _, s := range sniffs {
		if !s.f.Sniff(s.path, []byte(s.head)) {
			t.Errorf("%d.Sniff(%q) = false, want true", s.f, s.head)
		}
		if s.f.Sniff(s.path, []byte("<html>Not Found</html>")) {
			t.Errorf("%d.Sniff passed an HTML page", s.f)
		}
		if NotSuiteFile.Sniff(s.path, []byte(s.head)) {
			t.Errorf("NotSuiteFile.Sniff(%q) = true", s.head)
		}
	}
}

// Ruling R-D2: a marker line with trailing blanks passes apt's rule; the
// copy go-crypto decodes has the bare marker line, and nothing else moves.
func TestDecodableClearsign(t *testing.T) {
	const m = "-----BEGIN PGP SIGNED MESSAGE-----"
	cases := []struct{ in, want string }{
		{m + "\nHash: SHA256\n", m + "\nHash: SHA256\n"},
		{m + "\r\nHash: SHA256\r\n", m + "\r\nHash: SHA256\r\n"},
		{m + " \t\nHash: SHA256\n", m + "\nHash: SHA256\n"},
		{m + " \r\nHash: SHA256\r\n", m + "\nHash: SHA256\r\n"},
		{m, m},
		{m + "  ", m},
		{" " + m + "\n", " " + m + "\n"}, // fails apt's rule: left alone, refused later
		{"Origin: x\n", "Origin: x\n"},
	}
	for _, c := range cases {
		if got := string(DecodableClearsign([]byte(c.in))); got != c.want {
			t.Errorf("DecodableClearsign(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
