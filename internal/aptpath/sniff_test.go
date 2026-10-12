package aptpath

import "testing"

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
		{[]byte{0x88, 0x01}, true}, // old-format signature packet
		{[]byte{0x8b, 0x00}, true},
		{[]byte{0xc2, 0x01}, true}, // new-format signature packet
		{[]byte{0x87, 0x00}, false},
		{[]byte{0xc1, 0x00}, false},
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
