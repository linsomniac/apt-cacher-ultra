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
		{
			Filename:     "pool/main/f/foo/foo_1.2-3_amd64.deb",
			SHA256:       h1,
			Size:         12345,
			Package:      "foo",
			Architecture: "amd64",
			Version:      "1.2-3",
		},
		{
			Filename: "pool/main/b/bar/bar_2.0_amd64.deb",
			SHA256:   h2,
			Size:     67890,
			Package:  "bar",
			// Version absent from this stanza — parsed as "".
			// Architecture absent from this stanza — Phase 3 leaves
			// it as "" rather than rejecting; the SPEC3 §7.5.3
			// hot-set query filters empty values explicitly.
		},
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
		// spec C5 validateFilename
		{"control byte", "pool/foo\x1b.deb"},
		{"backslash", "pool\\foo.deb"},
		{"question mark", "pool/foo.deb?x"},
		{"hash", "pool/foo.deb#x"},
		{"empty segment", "pool//foo.deb"},
		{"leading dotdot", "../foo.deb"},
		{"only dots", "./."},
		{"too long", strings.Repeat("a", 4097)},
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
	// A Packages-shaped file in which no stanza names a Filename. It
	// declares no package file at all, so it is no Packages index:
	// adoption_parse_failed, so a downloaded-but-broken Packages file (an
	// error page, a truncated transfer) does not silently produce an
	// adoption with no .deb hash coverage. A body whose stanzas name
	// Filenames without a SHA256 parses (ruling R-G1:
	// TestParsePackages_OnlyStanzasWithoutSHA256Parse), and a blank body
	// (an empty component's index) parses to nothing
	// (TestParsePackages_AllWhitespaceIsZeroResult).
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

// TestParsePackages_ExtractsPackageAndArchitecture covers the SPEC3
// §7.5.2 parser additions. The hot-set query keys on (Package, Arch),
// so a stanza missing either must surface as an empty string (not
// dropped, since hash validation still wants the row).
func TestParsePackages_ExtractsPackageAndArchitecture(t *testing.T) {
	h := sha(1)
	body := "Package: nginx\n" +
		"Architecture: amd64\n" +
		"Filename: pool/main/n/nginx/nginx_1.2_amd64.deb\n" +
		"SHA256: " + h + "\n" +
		"\n" +
		// Stanza missing Architecture — Phase 3 leaves it empty.
		"Package: lonely\n" +
		"Filename: pool/main/l/lonely/lonely.deb\n" +
		"SHA256: " + h + "\n"
	got, err := ParsePackages([]byte(body))
	if err != nil {
		t.Fatalf("ParsePackages: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2: %+v", len(got), got)
	}
	if got[0].Package != "nginx" || got[0].Architecture != "amd64" {
		t.Errorf("first: pkg=%q arch=%q, want nginx/amd64", got[0].Package, got[0].Architecture)
	}
	if got[1].Package != "lonely" || got[1].Architecture != "" {
		t.Errorf("second: pkg=%q arch=%q, want lonely/\"\"", got[1].Package, got[1].Architecture)
	}
}

// TestParsePackages_CaseInsensitivePackageArchitecture: the field-name
// match should be case-insensitive (RFC822/Debian policy). A
// lowercased "package:" stanza must populate Package the same as
// "Package:".
func TestParsePackages_CaseInsensitivePackageArchitecture(t *testing.T) {
	h := sha(1)
	body := "package: nginx\n" +
		"architecture: amd64\n" +
		"filename: pool/main/n/nginx.deb\n" +
		"sha256: " + h + "\n"
	got, err := ParsePackages([]byte(body))
	if err != nil {
		t.Fatalf("ParsePackages: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	if got[0].Package != "nginx" || got[0].Architecture != "amd64" {
		t.Errorf("lowercased fields not parsed: %+v", got[0])
	}
}

// Spec C5 "Hash blocks": a stanza with a Filename but no SHA256 gives no
// row and is reported (no_sha256), in file order with repeats kept; a
// stanza without a Filename is not reported. ParsePackages returns the
// same rows.
func TestParsePackages_ReportsFilenameWithoutSHA256(t *testing.T) {
	h := sha(1)
	body := "Package: good\nArchitecture: amd64\nVersion: 1\nFilename: pool/good.deb\nSHA256: " + h + "\n\n" +
		"Package: sha512only\nFilename: pool/sha512only.deb\nSHA512: " + strings.Repeat("a", 128) + "\n\n" +
		"Package: md5only\nFilename: pool/md5only.deb\nMD5sum: 0123456789abcdef0123456789abcdef\n\n" +
		"Package: nofile\nSHA256: " + h + "\n\n" +
		"Package: md5only\nFilename: pool/md5only.deb\n"
	refs, noSHA256, err := parsePackages([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Filename != "pool/good.deb" {
		t.Fatalf("refs = %+v, want the one stanza with both fields", refs)
	}
	want := []string{"pool/sha512only.deb", "pool/md5only.deb", "pool/md5only.deb"}
	if strings.Join(noSHA256, ",") != strings.Join(want, ",") {
		t.Fatalf("no_sha256 = %q, want %q", noSHA256, want)
	}
	if got, err := ParsePackages([]byte(body)); err != nil || len(got) != 1 || got[0] != refs[0] {
		t.Fatalf("ParsePackages = %+v, %v; want parsePackages' rows", got, err)
	}
}

// Ruling R-G1 (the E carry-forward): a body whose stanzas all name a
// Filename but none a SHA256 — an index that declares only SHA512, SHA1 or
// MD5sum — parses with no rows and every Filename reported, so its
// snapshot adopts with incomplete coverage (no_sha256) instead of failing
// its parse at every check. ParsePackages returns no rows and no error.
func TestParsePackages_OnlyStanzasWithoutSHA256Parse(t *testing.T) {
	body := "Package: a\nFilename: ./a_1_amd64.deb\nSHA512: " + strings.Repeat("a", 128) + "\n\n" +
		"Package: b\nFilename: ./b_1_amd64.deb\nMD5sum: 0123456789abcdef0123456789abcdef\n"
	refs, noSHA256, err := parsePackages([]byte(body))
	if err != nil || len(refs) != 0 || strings.Join(noSHA256, ",") != "./a_1_amd64.deb,./b_1_amd64.deb" {
		t.Fatalf("parsePackages = %v, %q, %v; want no rows, both Filenames reported, no error", refs, noSHA256, err)
	}
	if refs, err := ParsePackages([]byte(body)); err != nil || len(refs) != 0 {
		t.Fatalf("ParsePackages = %v, %v; want no rows and no error", refs, err)
	}
}

// Ruling R-G1's other side: a body with no Filename stanza at all — an
// HTML error page, a foreign file — still fails its parse.
func TestParsePackages_NoFilenameStanzaStillFails(t *testing.T) {
	for name, body := range map[string]string{
		"HTML error page":          "<html><head><title>404 Not Found</title></head><body>Not Found</body></html>\n",
		"foreign file":             "Origin: Example\nLabel: Example\nSuite: stable\n",
		"SHA512 only, no Filename": "Package: a\nSHA512: " + strings.Repeat("a", 128) + "\n",
	} {
		if _, _, err := parsePackages([]byte(body)); err == nil {
			t.Errorf("%s: parsePackages succeeded, want a parse error", name)
		}
	}
}

// Spec C5 validateFilename: "." segments and "%" are allowed and kept
// literally — apt requests <base>/./x.deb, and a Filename's "%" is a
// literal byte that spec C1 re-encodes upstream.
func TestParsePackages_KeepsDotAndPercentFilenamesLiterally(t *testing.T) {
	body := "Package: cuda\nArchitecture: amd64\nVersion: 1\nFilename: ./cuda_12.6_amd64.deb\nSHA256: " + sha(1) + "\n\n" +
		"Package: pkg\nArchitecture: amd64\nVersion: 1:2\nFilename: pool/p/pkg_1%3a2_amd64.deb\nSHA256: " + sha(2) + "\n\n" +
		"Package: nested\nArchitecture: all\nVersion: 1\nFilename: a/./b/nested_1_all.deb\nSHA256: " + sha(3) + "\n"
	refs, err := ParsePackages([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range refs {
		got = append(got, r.Filename)
	}
	if want := "./cuda_12.6_amd64.deb,pool/p/pkg_1%3a2_amd64.deb,a/./b/nested_1_all.deb"; strings.Join(got, ",") != want {
		t.Fatalf("Filenames = %q, want %s", got, want)
	}
}

// Spec C5 validateFilename, the table: the shapes real flat repositories
// publish are accepted, and the spec's refusals stay refused.
func TestValidateFilename(t *testing.T) {
	for _, c := range []struct {
		f  string
		ok bool
	}{
		{"pool/main/f/foo/foo_1.0_amd64.deb", true},
		{"amd64/kubectl_1.34.1-1.1_amd64.deb", true}, // pkgs.k8s.io
		{"./cuda-toolkit_12.6.0-1_amd64.deb", true},  // NVIDIA CUDA, libnvidia-container
		{"binary/jenkins_2.479.3_all.deb", true},     // pkg.jenkins.io
		{"direct/jenkins_2.479.3_all.deb", true},
		{"files/sublime-text_build-4180_amd64.deb", true}, // download.sublimetext.com
		{"pkg_1%3a2.0_amd64.deb", true},                   // an epoch, %-encoded by the publisher
		{"a/./b/./c.deb", true},
		{".hidden.deb", true},
		{"..deb", true},
		{strings.Repeat("a", 4096), true},
		{"", false},
		{"/abs.deb", false},
		{"a\x00b.deb", false},
		{"a\x1fb.deb", false},
		{"a\x7fb.deb", false},
		{"a\\b.deb", false},
		{"a?b.deb", false},
		{"a#b.deb", false},
		{"a/../b.deb", false},
		{"..", false},
		{"a//b.deb", false},
		{"a/", false},
		{"./", false},
		{".", false},
		{"./.", false},
		{strings.Repeat("a", 4097), false},
	} {
		if err := validateFilename(c.f); (err == nil) != c.ok {
			t.Errorf("validateFilename(%q) = %v, want ok=%v", c.f, err, c.ok)
		}
	}
}

// MaxPackageStanzas bounds everything parsePackages keeps — rows and
// no_sha256 Filenames together (review fix round 2, Task 2) — so a hostile
// allowlisted upstream cannot grow memory past it with stanzas that lack
// a SHA256. Exactly the cap parses; one more fails, whichever kind it is.
func TestParsePackages_CapCountsStanzasWithoutSHA256(t *testing.T) {
	good := "Package: good\nArchitecture: amd64\nVersion: 1\nFilename: pool/good.deb\nSHA256: " + sha(1) + "\n\n"
	gaps := func(n int) string { return strings.Repeat("Filename: p\n\n", n) }
	for _, c := range []struct {
		name    string
		body    string
		refs    int
		gaps    int
		refused bool
	}{
		{"a row and gaps up to the cap", good + gaps(MaxPackageStanzas-1), 1, MaxPackageStanzas - 1, false},
		{"one gap past the cap", good + gaps(MaxPackageStanzas), 0, 0, true},
		{"one row past the cap", gaps(MaxPackageStanzas) + good, 0, 0, true},
		// Ruling R-G1: a body of gaps alone parses, up to the same cap.
		{"gaps alone up to the cap", gaps(MaxPackageStanzas), 0, MaxPackageStanzas, false},
		{"gaps alone past the cap", gaps(MaxPackageStanzas + 1), 0, 0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			refs, noSHA256, err := parsePackages([]byte(c.body))
			if c.refused {
				if err == nil || !strings.Contains(err.Error(), "exceeds "+strconvI(MaxPackageStanzas)+" stanzas") {
					t.Fatalf("err = %v, want the stanza cap", err)
				}
				return
			}
			if err != nil || len(refs) != c.refs || len(noSHA256) != c.gaps {
				t.Fatalf("%d rows, %d no_sha256, err %v; want %d and %d", len(refs), len(noSHA256), err, c.refs, c.gaps)
			}
		})
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
