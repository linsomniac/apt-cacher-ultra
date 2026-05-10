package freshness

import (
	"strings"
	"testing"
)

// TestParseSources_Happy: a real-shaped Sources stanza yields one
// SourceRef per Checksums-Sha256 entry, each with the suite-relative
// path = Directory + "/" + filename.
func TestParseSources_Happy(t *testing.T) {
	body := `Package: bash
Binary: bash, bash-static
Version: 5.1-2
Architecture: any
Format: 3.0 (quilt)
Files:
 d41d8cd98f00b204e9800998ecf8427e 4080 bash_5.1-2.dsc
 d41d8cd98f00b204e9800998ecf8427e 9952520 bash_5.1.orig.tar.xz
Checksums-Sha1:
 da39a3ee5e6b4b0d3255bfef95601890afd80709 4080 bash_5.1-2.dsc
 da39a3ee5e6b4b0d3255bfef95601890afd80709 9952520 bash_5.1.orig.tar.xz
Checksums-Sha256:
 9d2e1d4c8f3e1234567890abcdef1234567890abcdef1234567890abcdef9d2e 4080 bash_5.1-2.dsc
 7e2a8f9b3c1d234567890abcdef1234567890abcdef1234567890abcdef7e2a8 9952520 bash_5.1.orig.tar.xz
Directory: pool/main/b/bash

`
	refs, _, err := ParseSources([]byte(body))
	if err != nil {
		t.Fatalf("ParseSources: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("len(refs) = %d, want 2", len(refs))
	}
	expected := []SourceRef{
		{
			Path:        "pool/main/b/bash/bash_5.1-2.dsc",
			SHA256:      "9d2e1d4c8f3e1234567890abcdef1234567890abcdef1234567890abcdef9d2e",
			Size:        4080,
			PackageName: "bash",
		},
		{
			Path:        "pool/main/b/bash/bash_5.1.orig.tar.xz",
			SHA256:      "7e2a8f9b3c1d234567890abcdef1234567890abcdef1234567890abcdef7e2a8",
			Size:        9952520,
			PackageName: "bash",
		},
	}
	for i, want := range expected {
		got := refs[i]
		if got != want {
			t.Errorf("refs[%d] = %+v, want %+v", i, got, want)
		}
	}
}

// TestParseSources_MultipleStanzas: two stanzas yield two PackageName
// distinct sets of refs; per-stanza state must reset cleanly.
func TestParseSources_MultipleStanzas(t *testing.T) {
	body := `Package: alpha
Directory: pool/main/a/alpha
Checksums-Sha256:
 1111111111111111111111111111111111111111111111111111111111111111 100 alpha_1.dsc
 2222222222222222222222222222222222222222222222222222222222222222 200 alpha_1.tar.gz

Package: beta
Directory: pool/main/b/beta
Checksums-Sha256:
 3333333333333333333333333333333333333333333333333333333333333333 300 beta_2.dsc

`
	refs, _, err := ParseSources([]byte(body))
	if err != nil {
		t.Fatalf("ParseSources: %v", err)
	}
	if len(refs) != 3 {
		t.Fatalf("len(refs) = %d, want 3", len(refs))
	}
	pkgs := map[string]int{}
	for _, r := range refs {
		pkgs[r.PackageName]++
	}
	if pkgs["alpha"] != 2 || pkgs["beta"] != 1 {
		t.Errorf("per-package counts = %v, want alpha:2, beta:1", pkgs)
	}
}

// TestParseSources_SkipsStanzaWithNoSha256: SPEC6_5 §11 H4. A stanza
// whose only checksum block is MD5 (`Files:`) or SHA1
// (`Checksums-Sha1:`) drops its rows; other stanzas in the same file
// proceed.
func TestParseSources_SkipsStanzaWithNoSha256(t *testing.T) {
	body := `Package: legacy
Directory: pool/main/l/legacy
Files:
 d41d8cd98f00b204e9800998ecf8427e 100 legacy_1.dsc

Package: modern
Directory: pool/main/m/modern
Checksums-Sha256:
 4444444444444444444444444444444444444444444444444444444444444444 400 modern_1.dsc

`
	refs, _, err := ParseSources([]byte(body))
	if err != nil {
		t.Fatalf("ParseSources: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("len(refs) = %d, want 1 (legacy stanza must be skipped)", len(refs))
	}
	if refs[0].PackageName != "modern" {
		t.Errorf("expected only the modern stanza to survive, got %q", refs[0].PackageName)
	}
}

// TestParseSources_SkipsMissingPackageOrDirectory: stanzas missing
// either header are silently skipped.
func TestParseSources_SkipsMissingPackageOrDirectory(t *testing.T) {
	body := `Directory: pool/main/x/x
Checksums-Sha256:
 5555555555555555555555555555555555555555555555555555555555555555 500 x_1.dsc

Package: y
Checksums-Sha256:
 6666666666666666666666666666666666666666666666666666666666666666 600 y_1.dsc

Package: z
Directory: pool/main/z/z
Checksums-Sha256:
 7777777777777777777777777777777777777777777777777777777777777777 700 z_1.dsc

`
	refs, _, err := ParseSources([]byte(body))
	if err != nil {
		t.Fatalf("ParseSources: %v", err)
	}
	if len(refs) != 1 || refs[0].PackageName != "z" {
		t.Fatalf("got refs = %+v, want only z", refs)
	}
}

// TestParseSources_RejectsTraversalPath: SPEC6_5 §11 H11. A stanza
// whose Directory or filename contains `..` segments has its rows
// dropped; defensive even though signed input means upstream
// vouched for the bytes.
func TestParseSources_RejectsTraversalPath(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			"directory-traversal",
			`Package: bad
Directory: pool/../etc
Checksums-Sha256:
 9999999999999999999999999999999999999999999999999999999999999999 100 shadow

`,
		},
		{
			"filename-traversal-via-slash",
			`Package: bad
Directory: pool/main/b/bad
Checksums-Sha256:
 8888888888888888888888888888888888888888888888888888888888888888 100 ../etc/shadow

`,
		},
		{
			"filename-with-dotdot",
			`Package: bad
Directory: pool/main/b/bad
Checksums-Sha256:
 8888888888888888888888888888888888888888888888888888888888888888 100 ..

`,
		},
		{
			"filename-with-nul",
			`Package: bad
Directory: pool/main/b/bad
Checksums-Sha256:
 8888888888888888888888888888888888888888888888888888888888888888 100 bad` + "\x00" + `name

`,
		},
	}
	// Add a clean stanza so the file isn't entirely empty (which
	// would trigger the file-level error). The clean stanza must
	// survive; the traversal stanza must be dropped.
	clean := `Package: clean
Directory: pool/main/c/clean
Checksums-Sha256:
 0000000000000000000000000000000000000000000000000000000000000000 50 clean_1.dsc

`
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			refs, _, err := ParseSources([]byte(tc.body + clean))
			if err != nil {
				t.Fatalf("ParseSources: %v", err)
			}
			for _, r := range refs {
				if r.PackageName == "bad" {
					t.Errorf("traversal stanza leaked into output: %+v", r)
				}
			}
		})
	}
}

// TestParseSources_InvalidHashSkipped: a malformed (non-hex,
// wrong-length) sha256 entry is dropped; the well-formed entry from
// the same stanza survives.
func TestParseSources_InvalidHashSkipped(t *testing.T) {
	body := `Package: mixed
Directory: pool/main/m/mixed
Checksums-Sha256:
 NOTHEX 100 bad.dsc
 1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef 200 good.tar.gz

`
	refs, _, err := ParseSources([]byte(body))
	if err != nil {
		t.Fatalf("ParseSources: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("len(refs) = %d, want 1", len(refs))
	}
	if refs[0].Path != "pool/main/m/mixed/good.tar.gz" {
		t.Errorf("survivor = %s, want good.tar.gz", refs[0].Path)
	}
}

// TestParseSources_EmptyInput: an empty file returns nil, nil; a
// non-empty file with no usable stanzas returns the diagnostic error.
func TestParseSources_EmptyInput(t *testing.T) {
	refs, _, err := ParseSources([]byte(""))
	if err != nil {
		t.Errorf("empty input: got err %v, want nil", err)
	}
	if refs != nil {
		t.Errorf("empty input refs = %+v, want nil", refs)
	}
}

// TestParseSources_NoUsableStanzas: a non-empty file with all stanzas
// missing required headers returns a parse error so the caller can
// emit source_parse_failed Warn.
func TestParseSources_NoUsableStanzas(t *testing.T) {
	body := `Package: no-checksums
Directory: pool/main/n/no-checksums
Files:
 d41d8cd98f00b204e9800998ecf8427e 100 no_1.dsc

`
	_, _, err := ParseSources([]byte(body))
	if err == nil {
		t.Error("expected error for non-empty input with zero usable stanzas")
	}
}

// TestParseSources_MalformedHeader: a line with no colon is a fatal
// whole-file parse error.
func TestParseSources_MalformedHeader(t *testing.T) {
	body := "Package: ok\nDirectory pool/main\nChecksums-Sha256:\n abc 100 ok.dsc\n\n"
	_, _, err := ParseSources([]byte(body))
	if err == nil {
		t.Error("expected error for malformed header line")
	}
}

// TestParseSources_CaseInsensitiveHeaders: RFC822 / Debian policy:
// `package:` and `PACKAGE:` are equivalent.
func TestParseSources_CaseInsensitiveHeaders(t *testing.T) {
	body := `package: foo
DIRECTORY: pool/main/f/foo
Checksums-SHA256:
 abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890 100 foo_1.dsc

`
	refs, _, err := ParseSources([]byte(body))
	if err != nil {
		t.Fatalf("ParseSources: %v", err)
	}
	if len(refs) != 1 || refs[0].PackageName != "foo" {
		t.Errorf("case-insensitive parse failed: %+v", refs)
	}
}

// TestParseSources_ChecksumLineWhitespace: Checksums-Sha256 entries
// may use single or multiple spaces / tabs as separators.
func TestParseSources_ChecksumLineWhitespace(t *testing.T) {
	body := "Package: ws\nDirectory: pool/main/w/ws\nChecksums-Sha256:\n " +
		"abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890" +
		"\t100\t\tws_1.dsc\n\n"
	refs, _, err := ParseSources([]byte(body))
	if err != nil {
		t.Fatalf("ParseSources: %v", err)
	}
	if len(refs) != 1 {
		t.Errorf("tab-separated entry parse failed: %+v", refs)
	}
}

// TestParseSources_StanzaWithoutTrailingBlankLine: EOF without a
// trailing blank line still flushes the last stanza.
func TestParseSources_StanzaWithoutTrailingBlankLine(t *testing.T) {
	body := `Package: tail
Directory: pool/main/t/tail
Checksums-Sha256:
 abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890 100 tail_1.dsc`
	refs, _, err := ParseSources([]byte(body))
	if err != nil {
		t.Fatalf("ParseSources: %v", err)
	}
	if len(refs) != 1 || refs[0].PackageName != "tail" {
		t.Errorf("EOF-flush failed: %+v", refs)
	}
}

// TestParseSources_FieldsAfterChecksumBlockResetIt: a `Directory:`
// header arriving after a Checksums-Sha256 block must close the block
// so subsequent continuation lines (none here, but defensively) don't
// pollute the entries.
func TestParseSources_FieldsAfterChecksumBlockResetIt(t *testing.T) {
	body := `Package: order
Checksums-Sha256:
 abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890 100 order_1.dsc
Directory: pool/main/o/order
Format: 3.0 (quilt)
 this-continuation-belongs-to-Format-not-Checksums-Sha256

`
	refs, _, err := ParseSources([]byte(body))
	if err != nil {
		t.Fatalf("ParseSources: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("len(refs) = %d, want 1", len(refs))
	}
	if refs[0].Path != "pool/main/o/order/order_1.dsc" {
		t.Errorf("path = %s, want pool/main/o/order/order_1.dsc", refs[0].Path)
	}
}

// TestParseSources_StanzaCountReported: stats.StanzaCount totals
// every observed stanza, including the ones skipped for missing
// fields. Drives the SPEC6_5 §10.2 source_parsed Debug log.
func TestParseSources_StanzaCountReported(t *testing.T) {
	body := `Package: a
Directory: pool/main/a/a
Checksums-Sha256:
 1111111111111111111111111111111111111111111111111111111111111111 100 a.dsc

Package: b
Directory: pool/main/b/b
Files:
 d41d8cd98f00b204e9800998ecf8427e 100 b.dsc

Package: c
Directory: pool/main/c/c
Checksums-Sha256:
 2222222222222222222222222222222222222222222222222222222222222222 200 c.dsc

`
	refs, stats, err := ParseSources([]byte(body))
	if err != nil {
		t.Fatalf("ParseSources: %v", err)
	}
	if stats.StanzaCount != 3 {
		t.Errorf("stats.StanzaCount = %d, want 3 (a, b, c — including the SHA-less b)", stats.StanzaCount)
	}
	if len(refs) != 2 {
		t.Errorf("len(refs) = %d, want 2 (a + c; b skipped for missing SHA256)", len(refs))
	}
}

// TestParseSources_LargeStanzaCountCap: synthetic input exceeding
// MaxSourceStanzas yields a fatal parse error.
func TestParseSources_LargeStanzaCountCap(t *testing.T) {
	// Generate enough Checksums-Sha256 entries in one stanza to trip
	// the cap. One stanza with many entries is the cheapest way to
	// reach the cap in a unit test.
	var sb strings.Builder
	sb.WriteString("Package: many\n")
	sb.WriteString("Directory: pool/main/m/many\n")
	sb.WriteString("Checksums-Sha256:\n")
	for i := 0; i < MaxSourceStanzas+1; i++ {
		sb.WriteString(" abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890 100 many_")
		// Use a unique but path-validateable suffix.
		for j := i; j > 0; j /= 16 {
			sb.WriteByte("0123456789abcdef"[j%16])
		}
		if i == 0 {
			sb.WriteByte('0')
		}
		sb.WriteString(".tar.gz\n")
	}
	sb.WriteString("\n")
	_, _, err := ParseSources([]byte(sb.String()))
	if err == nil {
		t.Error("expected error for stanza-count cap exceeded")
	}
}
