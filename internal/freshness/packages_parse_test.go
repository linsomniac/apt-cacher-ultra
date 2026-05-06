package freshness

import (
	"strings"
	"testing"
)

func TestParsePackages_HappyPath(t *testing.T) {
	h1 := sha(11)
	h2 := sha(22)
	body := "Package: foo\n" +
		"Version: 1.2-3\n" +
		"Architecture: amd64\n" +
		"Filename: pool/main/f/foo/foo_1.2-3_amd64.deb\n" +
		"Size: 12345\n" +
		"SHA256: " + h1 + "\n" +
		"Description: A foo package\n" +
		"\n" +
		"Package: bar\n" +
		"Filename: pool/main/b/bar/bar_2.0_amd64.deb\n" +
		"Size: 67890\n" +
		"SHA256: " + h2 + "\n"
	got, err := ParsePackages([]byte(body))
	if err != nil {
		t.Fatalf("ParsePackages: %v", err)
	}
	want := []PackageRef{
		{Filename: "pool/main/f/foo/foo_1.2-3_amd64.deb", SHA256: h1, Size: 12345},
		{Filename: "pool/main/b/bar/bar_2.0_amd64.deb", SHA256: h2, Size: 67890},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParsePackages_TrailingStanzaNoBlankLine(t *testing.T) {
	h := sha(1)
	body := "Package: foo\n" +
		"Filename: pool/main/f/foo/foo.deb\n" +
		"SHA256: " + h + "\n"
	got, err := ParsePackages([]byte(body))
	if err != nil {
		t.Fatalf("ParsePackages: %v", err)
	}
	if len(got) != 1 || got[0].Filename != "pool/main/f/foo/foo.deb" {
		t.Errorf("trailing stanza without blank line not parsed: %+v", got)
	}
}

func TestParsePackages_SkipsStanzaWithoutFilename(t *testing.T) {
	h := sha(1)
	// First stanza has SHA256 but no Filename — skipped silently.
	// Second stanza has both — kept.
	body := "Package: foo\n" +
		"SHA256: " + h + "\n" +
		"\n" +
		"Package: bar\n" +
		"Filename: pool/main/b/bar.deb\n" +
		"SHA256: " + h + "\n"
	got, err := ParsePackages([]byte(body))
	if err != nil {
		t.Fatalf("ParsePackages: %v", err)
	}
	if len(got) != 1 || got[0].Filename != "pool/main/b/bar.deb" {
		t.Errorf("expected only the bar stanza: %+v", got)
	}
}

func TestParsePackages_SkipsStanzaWithoutSHA256(t *testing.T) {
	h := sha(1)
	body := "Package: foo\n" +
		"Filename: pool/main/f/foo.deb\n" +
		"\n" +
		"Package: bar\n" +
		"Filename: pool/main/b/bar.deb\n" +
		"SHA256: " + h + "\n"
	got, err := ParsePackages([]byte(body))
	if err != nil {
		t.Fatalf("ParsePackages: %v", err)
	}
	if len(got) != 1 || got[0].Filename != "pool/main/b/bar.deb" {
		t.Errorf("expected only the bar stanza: %+v", got)
	}
}

func TestParsePackages_BadSHA256(t *testing.T) {
	cases := []struct {
		name, hash string
	}{
		{"too short", "abc"},
		{"too long", strings.Repeat("a", 65)},
		{"uppercase", strings.Repeat("A", 64)},
		{"non-hex", strings.Repeat("g", 64)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := "Package: foo\nFilename: pool/foo.deb\nSHA256: " + tc.hash + "\n"
			_, err := ParsePackages([]byte(body))
			if err == nil || !strings.Contains(err.Error(), "invalid sha256") {
				t.Fatalf("want invalid-sha256 error, got %v", err)
			}
		})
	}
}

func TestParsePackages_BadFilename(t *testing.T) {
	cases := []struct{ name, fn string }{
		{"absolute", "/etc/shadow"},
		{"dotdot", "pool/../etc/shadow"},
		{"contains NUL", "pool/foo\x00.deb"},
	}
	h := sha(1)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := "Package: foo\nFilename: " + tc.fn + "\nSHA256: " + h + "\n"
			_, err := ParsePackages([]byte(body))
			if err == nil || !strings.Contains(err.Error(), "invalid filename") {
				t.Fatalf("want invalid-filename error, got %v", err)
			}
		})
	}
}

func TestParsePackages_BadSize(t *testing.T) {
	cases := []struct{ name, size, frag string }{
		{"non-numeric", "abc", "invalid size"},
		{"negative", "-1", "negative size"},
	}
	h := sha(1)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := "Package: foo\nFilename: pool/foo.deb\nSize: " + tc.size + "\nSHA256: " + h + "\n"
			_, err := ParsePackages([]byte(body))
			if err == nil || !strings.Contains(err.Error(), tc.frag) {
				t.Fatalf("want %q error, got %v", tc.frag, err)
			}
		})
	}
}

func TestParsePackages_MultilineDescription(t *testing.T) {
	h := sha(1)
	body := "Package: foo\n" +
		"Filename: pool/main/f/foo.deb\n" +
		"SHA256: " + h + "\n" +
		"Description: A foo\n" +
		" line two\n" +
		" line three\n" +
		" .\n" +
		" line five after blank-equivalent\n" +
		"Tag: section::misc\n"
	got, err := ParsePackages([]byte(body))
	if err != nil {
		t.Fatalf("ParsePackages: %v", err)
	}
	if len(got) != 1 || got[0].Filename != "pool/main/f/foo.deb" {
		t.Errorf("multiline Description broke parse: %+v", got)
	}
}

func TestParsePackages_CaseInsensitiveFieldNames(t *testing.T) {
	h := sha(1)
	body := "filename: pool/foo.deb\nsha256: " + h + "\nsize: 100\n"
	got, err := ParsePackages([]byte(body))
	if err != nil {
		t.Fatalf("ParsePackages: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	if got[0].Filename != "pool/foo.deb" || got[0].SHA256 != h || got[0].Size != 100 {
		t.Errorf("lowercased field names not parsed: %+v", got[0])
	}
}

func TestParsePackages_EmptyInputIsZeroResult(t *testing.T) {
	got, err := ParsePackages(nil)
	if err != nil {
		t.Errorf("empty input should not error, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d, want 0", len(got))
	}
}

func TestParsePackages_AllWhitespaceIsZeroResult(t *testing.T) {
	got, err := ParsePackages([]byte("\n\n\t  \n"))
	if err != nil {
		t.Errorf("whitespace-only input should not error, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d, want 0", len(got))
	}
}

func TestParsePackages_NonEmptyButNoUsableStanzas(t *testing.T) {
	// A well-formed Packages-shaped file in which no stanza declares
	// both Filename and SHA256. This is structurally legal apt input
	// (every field is recognized) but contributes zero rows to
	// package_hash. SPEC2 §7.5 step 8 expects "for each pkg with
	// Filename and SHA256" — zero pkgs means zero rows. Treat this
	// as adoption_parse_failed so a downloaded-but-broken Packages
	// file (apt error page rendered through gzip's gibberish, a
	// truncated transfer, or a legitimate but empty component) does
	// not silently produce an adoption with no .deb hash coverage.
	body := "Package: foo\n" +
		"Version: 1.0\n" +
		"Architecture: amd64\n" +
		"\n" +
		"Package: bar\n" +
		"Version: 2.0\n"
	_, err := ParsePackages([]byte(body))
	if err == nil || !strings.Contains(err.Error(), "no stanzas") {
		t.Fatalf("want no-stanzas error, got %v", err)
	}
}

func TestParsePackages_MalformedHeaderLine(t *testing.T) {
	body := "Package: foo\nthis-line-has-no-colon\nFilename: pool/foo.deb\n"
	_, err := ParsePackages([]byte(body))
	if err == nil || !strings.Contains(err.Error(), "malformed header") {
		t.Fatalf("want malformed-header error, got %v", err)
	}
}

func TestParsePackages_SizeOptional(t *testing.T) {
	h := sha(1)
	body := "Package: foo\nFilename: pool/foo.deb\nSHA256: " + h + "\n"
	got, err := ParsePackages([]byte(body))
	if err != nil {
		t.Fatalf("ParsePackages: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	if got[0].Size != 0 {
		t.Errorf("missing Size should yield 0, got %d", got[0].Size)
	}
}

// TestParsePackages_LargeRealistic confirms the parser handles a
// 50k-stanza Packages file (≈ Ubuntu noble main amd64 size) without
// blowing the scanner buffer or running afoul of the cap.
func TestParsePackages_LargeRealistic(t *testing.T) {
	const n = 50_000
	h := sha(7)
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("Package: pkg")
		b.WriteString(strconvI(i))
		b.WriteString("\n")
		b.WriteString("Filename: pool/main/p/pkg")
		b.WriteString(strconvI(i))
		b.WriteString(".deb\n")
		b.WriteString("Size: 1000\n")
		b.WriteString("SHA256: ")
		b.WriteString(h)
		b.WriteString("\n\n")
	}
	got, err := ParsePackages([]byte(b.String()))
	if err != nil {
		t.Fatalf("ParsePackages: %v", err)
	}
	if len(got) != n {
		t.Errorf("got %d, want %d", len(got), n)
	}
}

// strconvI is a tiny helper to keep strconv off the test dependency
// list — only the parser test needs int formatting and strings is
// already imported.
func strconvI(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	n := i
	if n < 0 {
		n = -n
	}
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if i < 0 {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
