package aptpath

import (
	"slices"
	"testing"
)

// Spec C5 "Anchor forms and storage": the anchor file(s) of each form.
func TestAnchorPaths(t *testing.T) {
	for _, c := range []struct {
		form string
		want []string
	}{
		{"inline", []string{"InRelease"}},
		{"detached", []string{"Release", "Release.gpg"}},
		{"unsigned", []string{"Release"}},
		{"", nil},
		{"Inline", nil},
	} {
		if got := AnchorPaths(c.form); !slices.Equal(got, c.want) {
			t.Errorf("AnchorPaths(%q) = %q, want %q", c.form, got, c.want)
		}
	}
	// Callers may keep the slice: a second call is not affected by edits.
	AnchorPaths("detached")[0] = "x"
	if got := AnchorPaths("detached"); got[0] != "Release" {
		t.Errorf("AnchorPaths shares its result: %q", got)
	}
}
