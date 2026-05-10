package freshness

import (
	"strings"
	"testing"
)

// TestParsePdiffIndex_Happy: a real-shaped Index yields one
// PdiffPatchRef per SHA256-Download entry. The SHA256-Patches block
// is intentionally ignored.
func TestParsePdiffIndex_Happy(t *testing.T) {
	body := `SHA256-Current:
 9d2e1d4c8f3e1234567890abcdef1234567890abcdef1234567890abcdef9d2e 12345678
SHA256-History:
 1111111111111111111111111111111111111111111111111111111111111111 100 2026-05-08-1200.00
 2222222222222222222222222222222222222222222222222222222222222222 200 2026-05-09-0600.30
SHA256-Patches:
 3333333333333333333333333333333333333333333333333333333333333333 5000 2026-05-09-1234.56
 4444444444444444444444444444444444444444444444444444444444444444 6000 2026-05-09-1800.00
SHA256-Download:
 5555555555555555555555555555555555555555555555555555555555555555 1500 2026-05-09-1234.56.gz
 6666666666666666666666666666666666666666666666666666666666666666 1700 2026-05-09-1800.00.gz
Canonical-Path: dists/noble/main/binary-amd64/Packages
`
	refs, err := ParsePdiffIndex([]byte(body))
	if err != nil {
		t.Fatalf("ParsePdiffIndex: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("len(refs) = %d, want 2 (SHA256-Download entries only)", len(refs))
	}
	expected := []PdiffPatchRef{
		{Filename: "2026-05-09-1234.56.gz", SHA256: "5555555555555555555555555555555555555555555555555555555555555555", Size: 1500},
		{Filename: "2026-05-09-1800.00.gz", SHA256: "6666666666666666666666666666666666666666666666666666666666666666", Size: 1700},
	}
	for i, want := range expected {
		if refs[i] != want {
			t.Errorf("refs[%d] = %+v, want %+v", i, refs[i], want)
		}
	}
}

// TestParsePdiffIndex_NoDownloadBlock: SPEC6_5 §11 H5. An Index with
// no SHA256-Download block returns (nil, nil) — the absence is not
// an error; it's a publication-shape variant that just produces no
// per-patch package_hash rows.
func TestParsePdiffIndex_NoDownloadBlock(t *testing.T) {
	body := `SHA256-Current:
 9d2e1d4c8f3e1234567890abcdef1234567890abcdef1234567890abcdef9d2e 12345678
SHA256-Patches:
 3333333333333333333333333333333333333333333333333333333333333333 5000 2026-05-09-1234.56
Canonical-Path: dists/noble/main/binary-amd64/Packages
`
	refs, err := ParsePdiffIndex([]byte(body))
	if err != nil {
		t.Errorf("expected nil err for missing SHA256-Download block, got %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("expected empty refs, got %+v", refs)
	}
}

// TestParsePdiffIndex_MalformedFilenameSkipped: SPEC6_5 §11 H6. An
// entry whose filename doesn't match the digit/dot/dash + .gz pattern
// is dropped; well-formed sibling entries survive.
func TestParsePdiffIndex_MalformedFilenameSkipped(t *testing.T) {
	body := `SHA256-Download:
 1111111111111111111111111111111111111111111111111111111111111111 100 not-a-pdiff-name.gz
 2222222222222222222222222222222222222222222222222222222222222222 200 ../etc/shadow
 3333333333333333333333333333333333333333333333333333333333333333 300 2026-05-09-1234.56.gz
 4444444444444444444444444444444444444444444444444444444444444444 400 2026-05-09-1234.56
 5555555555555555555555555555555555555555555555555555555555555555 500 ` + "\x00" + `weirdname.gz
`
	refs, err := ParsePdiffIndex([]byte(body))
	if err != nil {
		t.Fatalf("ParsePdiffIndex: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("len(refs) = %d, want 1 (only the well-formed entry survives)", len(refs))
	}
	if refs[0].Filename != "2026-05-09-1234.56.gz" {
		t.Errorf("survivor = %s, want 2026-05-09-1234.56.gz", refs[0].Filename)
	}
}

// TestParsePdiffIndex_InvalidHashSkipped: an entry with a non-hex or
// wrong-length SHA256 is dropped; the well-formed sibling survives.
func TestParsePdiffIndex_InvalidHashSkipped(t *testing.T) {
	body := `SHA256-Download:
 NOTHEX 100 2026-05-09-1234.56.gz
 abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890 200 2026-05-09-1800.00.gz
`
	refs, err := ParsePdiffIndex([]byte(body))
	if err != nil {
		t.Fatalf("ParsePdiffIndex: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("len(refs) = %d, want 1", len(refs))
	}
	if refs[0].Filename != "2026-05-09-1800.00.gz" {
		t.Errorf("survivor = %s, want 2026-05-09-1800.00.gz", refs[0].Filename)
	}
}

// TestParsePdiffIndex_HeaderClosesBlock: a non-continuation header
// line (e.g. Canonical-Path) appearing inside or after a
// SHA256-Download block must close the block so subsequent
// continuations don't pollute the entries.
func TestParsePdiffIndex_HeaderClosesBlock(t *testing.T) {
	body := `SHA256-Download:
 1111111111111111111111111111111111111111111111111111111111111111 100 2026-05-09-1234.56.gz
SHA256-Patches:
 2222222222222222222222222222222222222222222222222222222222222222 200 2026-05-09-1234.56
 3333333333333333333333333333333333333333333333333333333333333333 300 2026-05-09-1800.00.gz
`
	refs, err := ParsePdiffIndex([]byte(body))
	if err != nil {
		t.Fatalf("ParsePdiffIndex: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("len(refs) = %d, want 1 (Patches block must not be folded into Download)", len(refs))
	}
	if refs[0].Filename != "2026-05-09-1234.56.gz" {
		t.Errorf("got %s, want 2026-05-09-1234.56.gz", refs[0].Filename)
	}
}

// TestParsePdiffIndex_BlankLineClosesBlock: a blank line inside the
// document closes the open block (defensive — real Index files don't
// have stanza-style blanks, but the parser must not mis-state).
func TestParsePdiffIndex_BlankLineClosesBlock(t *testing.T) {
	body := `SHA256-Download:
 1111111111111111111111111111111111111111111111111111111111111111 100 2026-05-09-1234.56.gz

 2222222222222222222222222222222222222222222222222222222222222222 200 2026-05-09-1800.00.gz
`
	refs, err := ParsePdiffIndex([]byte(body))
	if err != nil {
		t.Fatalf("ParsePdiffIndex: %v", err)
	}
	if len(refs) != 1 {
		t.Errorf("len(refs) = %d, want 1 (blank line closed the block)", len(refs))
	}
}

// TestParsePdiffIndex_Empty: empty input returns (nil, nil).
func TestParsePdiffIndex_Empty(t *testing.T) {
	refs, err := ParsePdiffIndex([]byte(""))
	if err != nil {
		t.Errorf("empty input err = %v, want nil", err)
	}
	if refs != nil {
		t.Errorf("empty input refs = %+v, want nil", refs)
	}
}

// TestParsePdiffIndex_MalformedHeader: a header line lacking a colon
// is a fatal whole-file parse error.
func TestParsePdiffIndex_MalformedHeader(t *testing.T) {
	body := "Garbage line with no colon\n"
	_, err := ParsePdiffIndex([]byte(body))
	if err == nil {
		t.Error("expected error for header without colon")
	}
}

// TestParsePdiffIndex_LargeEntryCountCap: synthetic input exceeding
// MaxPdiffPatchEntries is a fatal parse error.
func TestParsePdiffIndex_LargeEntryCountCap(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("SHA256-Download:\n")
	for i := 0; i < MaxPdiffPatchEntries+1; i++ {
		sb.WriteString(" 1111111111111111111111111111111111111111111111111111111111111111 100 0.")
		// Generate a unique-but-pdiff-shaped suffix.
		for j := i + 1; j > 0; j /= 10 {
			sb.WriteByte("0123456789"[j%10])
		}
		sb.WriteString(".gz\n")
	}
	_, err := ParsePdiffIndex([]byte(sb.String()))
	if err == nil {
		t.Error("expected error for entry-count cap exceeded")
	}
}

// TestArchFromPdiffIndexPath: the index-path arch extractor maps
// binary-<arch>/Packages.diff/Index → <arch>, source/Sources.diff/Index
// → "source", and rejects non-Index paths and other shapes.
func TestArchFromPdiffIndexPath(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		wantArch string
		wantOK   bool
	}{
		{"amd64-packages-index", "main/binary-amd64/Packages.diff/Index", "amd64", true},
		{"arm64-packages-index", "main/binary-arm64/Packages.diff/Index", "arm64", true},
		{"i386-packages-index-d-i", "main/debian-installer/binary-i386/Packages.diff/Index", "i386", true},
		{"source-sources-index", "main/source/Sources.diff/Index", "source", true},
		// Non-Index leaves: these are individual patch files, not Index.
		{"patch-file-not-index", "main/binary-amd64/Packages.diff/2026-05-09-1234.56.gz", "", false},
		{"sources-patch-file", "main/source/Sources.diff/2026-05-09-1234.56.gz", "", false},
		// Plain Packages, not Index — out of scope for arch extraction.
		{"plain-packages-not-index", "main/binary-amd64/Packages.gz", "", false},
		// Path missing a binary-/source/ segment.
		{"index-without-arch-segment", "main/Packages.diff/Index", "", false},
		{"empty", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotArch, gotOK := archFromPdiffIndexPath(tc.path)
			if gotArch != tc.wantArch || gotOK != tc.wantOK {
				t.Errorf("archFromPdiffIndexPath(%q) = (%q, %v), want (%q, %v)",
					tc.path, gotArch, gotOK, tc.wantArch, tc.wantOK)
			}
		})
	}
}

// TestParsePdiffIndex_CaseInsensitiveHeader: SHA256-DOWNLOAD and
// sha256-download must be recognized too.
func TestParsePdiffIndex_CaseInsensitiveHeader(t *testing.T) {
	body := `sha256-download:
 1111111111111111111111111111111111111111111111111111111111111111 100 2026-05-09-1234.56.gz
`
	refs, err := ParsePdiffIndex([]byte(body))
	if err != nil {
		t.Fatalf("ParsePdiffIndex: %v", err)
	}
	if len(refs) != 1 {
		t.Errorf("case-insensitive header parse failed: %+v", refs)
	}
}
