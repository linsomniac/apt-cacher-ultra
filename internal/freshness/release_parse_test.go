package freshness

import (
	"errors"
	"strings"
	"testing"
)

// hash returns a 64-char lowercase hex string for tests. The bytes are
// arbitrary but vary so we can confirm the parser preserves order.
func sha(b byte) string {
	out := make([]byte, 64)
	for i := range out {
		out[i] = "0123456789abcdef"[(int(b)+i)%16]
	}
	return string(out)
}

func TestParseRelease_HappyPath(t *testing.T) {
	h1 := sha(1)
	h2 := sha(2)
	h3 := sha(3)
	body := "Origin: Ubuntu\n" +
		"Suite: noble\n" +
		"Codename: noble\n" +
		"SHA256:\n" +
		" " + h1 + " 4533 main/binary-amd64/Packages\n" +
		" " + h2 + " 1234 main/binary-amd64/Packages.gz\n" +
		" " + h3 + " 87 main/source/Sources\n"
	got, err := ParseRelease([]byte(body))
	if err != nil {
		t.Fatalf("ParseRelease: %v", err)
	}
	want := []ReleaseMember{
		{Path: "main/binary-amd64/Packages", SHA256: h1, Size: 4533},
		{Path: "main/binary-amd64/Packages.gz", SHA256: h2, Size: 1234},
		{Path: "main/source/Sources", SHA256: h3, Size: 87},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d members, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("member[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseRelease_IgnoresMD5AndSHA1(t *testing.T) {
	h1 := sha(1)
	body := "MD5Sum:\n" +
		" 0123456789abcdef0123456789abcdef 4533 ignored.bin\n" +
		"SHA1:\n" +
		" 0123456789abcdef0123456789abcdef01234567 4533 ignored.bin\n" +
		"SHA256:\n" +
		" " + h1 + " 4533 main/Packages\n"
	got, err := ParseRelease([]byte(body))
	if err != nil {
		t.Fatalf("ParseRelease: %v", err)
	}
	if len(got) != 1 || got[0].Path != "main/Packages" || got[0].SHA256 != h1 {
		t.Errorf("expected only the SHA256 entry, got %+v", got)
	}
}

func TestParseRelease_NoSHA256Block(t *testing.T) {
	body := "Origin: Ubuntu\nSuite: noble\nMD5Sum:\n 0123456789abcdef0123456789abcdef 4533 x\n"
	_, err := ParseRelease([]byte(body))
	if err == nil || !strings.Contains(err.Error(), "no SHA256") {
		t.Fatalf("want no-SHA256 error, got %v", err)
	}
}

func TestParseRelease_EmptySHA256Block(t *testing.T) {
	body := "Origin: Ubuntu\nSHA256:\nDescription: foo\n"
	_, err := ParseRelease([]byte(body))
	if err == nil || !strings.Contains(err.Error(), "empty SHA256") {
		t.Fatalf("want empty-SHA256 error, got %v", err)
	}
}

func TestParseRelease_BlockEndsAtBlankLine(t *testing.T) {
	h1 := sha(1)
	h2 := sha(2)
	body := "SHA256:\n" +
		" " + h1 + " 1 a\n" +
		"\n" +
		" " + h2 + " 2 b\n"
	got, err := ParseRelease([]byte(body))
	if err != nil {
		t.Fatalf("ParseRelease: %v", err)
	}
	if len(got) != 1 || got[0].Path != "a" {
		t.Errorf("expected only first member; second is post-blank-line: got %+v", got)
	}
}

func TestParseRelease_BadHash(t *testing.T) {
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
			body := "SHA256:\n " + tc.hash + " 4 path\n"
			_, err := ParseRelease([]byte(body))
			if err == nil || !strings.Contains(err.Error(), "invalid sha256") {
				t.Fatalf("want invalid-sha256 error, got %v", err)
			}
		})
	}
}

func TestParseRelease_BadSize(t *testing.T) {
	cases := []struct{ name, size, errFrag string }{
		{"non-numeric", "abc", "invalid size"},
		{"negative", "-1", "negative size"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := "SHA256:\n " + sha(1) + " " + tc.size + " path\n"
			_, err := ParseRelease([]byte(body))
			if err == nil || !strings.Contains(err.Error(), tc.errFrag) {
				t.Fatalf("want %q error, got %v", tc.errFrag, err)
			}
		})
	}
}

func TestParseRelease_MalformedLine(t *testing.T) {
	body := "SHA256:\n " + sha(1) + " only-two-fields\n"
	_, err := ParseRelease([]byte(body))
	if err == nil || !strings.Contains(err.Error(), "malformed line") {
		t.Fatalf("want malformed-line error, got %v", err)
	}
}

func TestParseRelease_BadPaths(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"absolute", "/etc/shadow"},
		{"dotdot in middle", "main/../../etc/shadow"},
		{"dotdot at start", "../etc/shadow"},
		// Defense-in-depth around isMetadataSelfPath: paths that
		// downstream URL/path normalization would resolve to one of
		// the filtered names ("Release", "InRelease", "Release.gpg")
		// must be rejected by the parser, not the filter. Otherwise
		// an upstream could ship "./Release" with a stub-size hash,
		// the filter would miss it (exact-string compare), and the
		// member fetcher would deadend at a content-length mismatch
		// the same way the original bug did.
		{"dot segment at start", "./Release"},
		{"dot segment in middle", "main/./Packages"},
		{"empty segment", "main//Packages"},
		{"backslash separator", "main\\Packages"},
		{"percent-encoded dot", "%2e/Release"},
		{"percent-encoded gpg dot", "Release%2egpg"},
		{"percent-encoded slash", "main%2fPackages"},
		{"bare percent", "main/Pack%ages"},
	}
	h := sha(1)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := "SHA256:\n " + h + " 1 " + tc.path + "\n"
			_, err := ParseRelease([]byte(body))
			if err == nil || !strings.Contains(err.Error(), "invalid path") {
				t.Fatalf("want invalid-path error, got %v", err)
			}
		})
	}
}

// TestParseRelease_CapsParsedRowsNotJustRetained confirms the
// MaxReleaseMembers cap counts EVERY parsed SHA256 row, including
// metadata-self entries that get filtered. Without this an adversary
// could pad a (signed) Release with millions of "Release" lines —
// each one filtered, none counted toward the cap — and force the
// parser to walk the entire payload before the freshness check's
// per-body byte limit caught it. The body limit is the primary
// gate; this is the row-level secondary bound.
func TestParseRelease_CapsParsedRowsNotJustRetained(t *testing.T) {
	hSelf := sha(1)
	hReal := sha(2)
	var sb strings.Builder
	sb.WriteString("SHA256:\n")
	// MaxReleaseMembers self-references — all filtered, but each
	// must count. Adding even one more row past the cap should
	// trip the error.
	for i := 0; i < MaxReleaseMembers; i++ {
		sb.WriteString(" " + hSelf + " 188 Release\n")
	}
	sb.WriteString(" " + hReal + " 4533 main/binary-amd64/Packages\n")
	_, err := ParseRelease([]byte(sb.String()))
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("want exceeds-cap error after MaxReleaseMembers self-references, got %v", err)
	}
}

func TestParseRelease_PathWithNUL(t *testing.T) {
	body := "SHA256:\n " + sha(1) + " 1 a\x00b\n"
	_, err := ParseRelease([]byte(body))
	if err == nil || !strings.Contains(err.Error(), "invalid path") {
		t.Fatalf("want invalid-path error for NUL, got %v", err)
	}
}

func TestParseRelease_TabIndentedContinuation(t *testing.T) {
	// Most real Release files use space-indent, but apt's parser accepts
	// any leading whitespace. Stay lenient on input.
	h := sha(1)
	body := "SHA256:\n\t" + h + " 4 path\n"
	got, err := ParseRelease([]byte(body))
	if err != nil {
		t.Fatalf("ParseRelease: %v", err)
	}
	if len(got) != 1 || got[0].Path != "path" {
		t.Errorf("tab-indented entry not parsed: %+v", got)
	}
}

func TestParseRelease_SHA256HeaderTrailingWhitespace(t *testing.T) {
	h := sha(1)
	body := "SHA256:   \n " + h + " 4 path\n"
	got, err := ParseRelease([]byte(body))
	if err != nil {
		t.Fatalf("ParseRelease: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("trailing whitespace on SHA256 header broke parse: %+v", got)
	}
}

// TestParseRelease_AlignedSizeColumn confirms the parser handles
// real-world Release files that pad the size column with spaces for
// alignment — strings.Fields collapses the run of spaces.
func TestParseRelease_AlignedSizeColumn(t *testing.T) {
	h1 := sha(1)
	h2 := sha(2)
	body := "SHA256:\n" +
		" " + h1 + "       42 short\n" +
		" " + h2 + " 12345678 long\n"
	got, err := ParseRelease([]byte(body))
	if err != nil {
		t.Fatalf("ParseRelease: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d members, want 2", len(got))
	}
	if got[0].Size != 42 || got[1].Size != 12345678 {
		t.Errorf("padded sizes parsed wrong: %+v", got)
	}
}

// TestParseRelease_LargeRealistic confirms the parser handles a 30k-line
// Release without choking on the default Scanner buffer or running out
// of slack on the member cap. This is also the "real Ubuntu Release"
// shape proxy.
func TestParseRelease_LargeRealistic(t *testing.T) {
	const n = 30_000
	var b strings.Builder
	b.WriteString("Origin: Ubuntu\nSuite: noble\nSHA256:\n")
	h := sha(7)
	for i := 0; i < n; i++ {
		b.WriteString(" ")
		b.WriteString(h)
		b.WriteString(" 4533 main/binary-amd64/by-hash/SHA256/")
		b.WriteString(h)
		b.WriteString("\n")
	}
	got, err := ParseRelease([]byte(b.String()))
	if err != nil {
		t.Fatalf("ParseRelease: %v", err)
	}
	if len(got) != n {
		t.Errorf("got %d members, want %d", len(got), n)
	}
}

func TestParseRelease_OverMaxMembers(t *testing.T) {
	// Synthesize MaxReleaseMembers + 1 entries to confirm the cap fires.
	// Build with strings.Builder for speed.
	var b strings.Builder
	b.WriteString("SHA256:\n")
	h := sha(9)
	for i := 0; i <= MaxReleaseMembers; i++ {
		b.WriteString(" ")
		b.WriteString(h)
		b.WriteString(" 1 a\n")
	}
	_, err := ParseRelease([]byte(b.String()))
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("want over-max error, got %v", err)
	}
}

// TestParseRelease_EmptyInput confirms an empty Release is rejected with
// a clear error rather than silently returning zero members.
func TestParseRelease_EmptyInput(t *testing.T) {
	_, err := ParseRelease(nil)
	if err == nil {
		t.Fatalf("want error on empty input")
	}
	if !errors.Is(err, err) { // sanity, just checking err propagates
		t.Errorf("err shape: %v", err)
	}
}

// TestParseRelease_DropsMetadataSelfReferences confirms that Release,
// InRelease, and Release.gpg entries inside the SHA256 block are
// silently dropped. apt-ftparchive's `release` subcommand walks the
// suite directory and includes the generated Release as a member of
// itself (at "stub size" — headers only, before the hash blocks were
// appended). The on-disk file we'd refetch is the FULL output with a
// different size and hash, so naively iterating these entries dead-ends
// at a content-length mismatch and silently aborts adoption. apt itself
// never refetches Release/Release.gpg/InRelease via the SHA256 block,
// so dropping these is faithful to apt semantics.
//
// Real-world fixture: this test mirrors what `apt-ftparchive release`
// actually emits — the Release line lists 188 bytes (the stub) while
// the on-disk Release would be ~1.5 KiB.
func TestParseRelease_DropsMetadataSelfReferences(t *testing.T) {
	hRelease := sha(1)
	hInRelease := sha(2)
	hReleaseGPG := sha(3)
	hPackages := sha(4)
	hPackagesGz := sha(5)
	body := "Origin: ChaosTest\n" +
		"Suite: noble\n" +
		"SHA256:\n" +
		" " + hRelease + " 188 Release\n" +
		" " + hInRelease + " 200 InRelease\n" +
		" " + hReleaseGPG + " 100 Release.gpg\n" +
		" " + hPackages + " 4533 main/binary-amd64/Packages\n" +
		" " + hPackagesGz + " 1234 main/binary-amd64/Packages.gz\n"
	got, err := ParseRelease([]byte(body))
	if err != nil {
		t.Fatalf("ParseRelease: %v", err)
	}
	want := []ReleaseMember{
		{Path: "main/binary-amd64/Packages", SHA256: hPackages, Size: 4533},
		{Path: "main/binary-amd64/Packages.gz", SHA256: hPackagesGz, Size: 1234},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d members, want %d (metadata-self refs should be dropped); got=%+v",
			len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("member[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	// Belt-and-suspenders: explicitly assert none of the three
	// metadata-self paths leaked through.
	for _, m := range got {
		switch m.Path {
		case "Release", "InRelease", "Release.gpg":
			t.Errorf("metadata-self path leaked through: %+v", m)
		}
	}
}

// TestParseRelease_PreservesNonRootRelease confirms the filter is
// scoped to the bare metadata names. A path like
// "main/source/Release" is component-content (some archives ship a
// per-component Release, distinct from the suite Release), and the
// filter must not strip it.
func TestParseRelease_PreservesNonRootRelease(t *testing.T) {
	h1 := sha(1)
	h2 := sha(2)
	body := "SHA256:\n" +
		" " + h1 + " 188 Release\n" +
		" " + h2 + " 444 main/source/Release\n"
	got, err := ParseRelease([]byte(body))
	if err != nil {
		t.Fatalf("ParseRelease: %v", err)
	}
	if len(got) != 1 || got[0].Path != "main/source/Release" || got[0].SHA256 != h2 {
		t.Errorf("expected only main/source/Release, got %+v", got)
	}
}
