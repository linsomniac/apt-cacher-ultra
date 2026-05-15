package admin

import (
	"testing"
)

func TestStatusTemplateFuncMapHasNewHelpers(t *testing.T) {
	fm := statusTemplateFuncMap()
	want := []string{
		"chunkHex",
		"sourceKind",
		"sourceKindLabel",
		"countBundled",
		"countSystem",
		"countCustom",
		"formatShortDuration",
		"outcomeBadgeClass",
	}
	for _, name := range want {
		if _, ok := fm[name]; !ok {
			t.Errorf("statusTemplateFuncMap missing helper %q (required by docs/admin-ui-spec.md §6.1)", name)
		}
	}
	// Existing helpers must remain — regression guard for the JSON-path
	// preservation rule in §0.4.
	for _, name := range []string{"unixTime", "formatBytes", "durationOf", "hitRatePct"} {
		if _, ok := fm[name]; !ok {
			t.Errorf("statusTemplateFuncMap regressed: missing existing helper %q", name)
		}
	}
}

// AIDEV-NOTE: tests in this file are the implementation contract for the
// helpers added in docs/admin-ui-spec.md §6.1. Cases mirror the examples
// in §6.1 and §14.1 verbatim — keep them in sync if the spec changes.

func TestChunkHex(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"empty", "", 4, ""},
		{
			"sha1_40hex",
			"DEADBEEFCAFEBABE0123456789ABCDEFFEDCBA98",
			4,
			"dead beef cafe babe 0123 4567 89ab cdef fedc ba98",
		},
		{
			"sha256_64hex",
			"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			4,
			"0123 4567 89ab cdef 0123 4567 89ab cdef 0123 4567 89ab cdef 0123 4567 89ab cdef",
		},
		{
			"non_hex_passthrough_returned_verbatim",
			"not-a-fingerprint",
			4,
			"not-a-fingerprint",
		},
		{
			"already_lowercase",
			"abcdef0123456789",
			4,
			"abcd ef01 2345 6789",
		},
		{
			"n_zero_returns_unchunked_lower",
			"ABCDEF",
			0,
			"abcdef",
		},
		{
			"odd_length_last_chunk_short",
			"abcde",
			2,
			"ab cd e",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := chunkHex(c.in, c.n)
			if got != c.want {
				t.Fatalf("chunkHex(%q, %d) = %q; want %q", c.in, c.n, got, c.want)
			}
		})
	}
}

func TestSourceKind(t *testing.T) {
	cases := []struct {
		path string
		kind string
	}{
		{"embedded:ubuntu-archive-keyring.gpg", "bundled"},
		{"embedded:debian-archive-keyring.gpg", "bundled"},
		{"/usr/share/keyrings/ubuntu-archive-keyring.gpg", "system"},
		{"/usr/share/keyrings/debian-archive-keyring.gpg", "system"},
		{"/etc/apt/keyrings/custom.gpg", "custom"},
		{"/etc/apt/trusted.gpg.d/foo.gpg", "custom"},
		{"", "custom"},
	}
	for _, c := range cases {
		if got := sourceKind(c.path); got != c.kind {
			t.Errorf("sourceKind(%q) = %q; want %q", c.path, got, c.kind)
		}
	}
}

func TestSourceKindLabel(t *testing.T) {
	cases := []struct {
		path  string
		label string
	}{
		{"embedded:foo", "BUNDLED"},
		{"/usr/share/keyrings/foo.gpg", "SYSTEM"},
		{"/etc/apt/keyrings/foo.gpg", "CUSTOM"},
	}
	for _, c := range cases {
		if got := sourceKindLabel(c.path); got != c.label {
			t.Errorf("sourceKindLabel(%q) = %q; want %q", c.path, got, c.label)
		}
	}
}

func TestFormatShortDuration(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0 ms"},
		{0.014, "14 ms"},
		{0.0139, "14 ms"},
		{0.5, "500 ms"},
		{0.999, "999 ms"},
		{1.2, "1.2 s"},
		{59.4, "59.4 s"},
		{60, "1m 0s"},
		{90, "1m 30s"},
		{3599, "59m 59s"},
		{3600, "1h 0m"},
		{5400, "1h 30m"},
		{-5, "0 ms"},
	}
	for _, c := range cases {
		if got := formatShortDuration(c.in); got != c.want {
			t.Errorf("formatShortDuration(%v) = %q; want %q", c.in, got, c.want)
		}
	}
}

func TestKeyringCounts(t *testing.T) {
	entries := []keyringEntry{
		{SourcePath: "embedded:ubuntu-archive-keyring.gpg"},
		{SourcePath: "embedded:debian-archive-keyring.gpg"},
		{SourcePath: "embedded:linux-mint-archive-keyring.gpg"},
		{SourcePath: "/usr/share/keyrings/ubuntu-archive-keyring.gpg"},
		{SourcePath: "/etc/apt/keyrings/custom-a.gpg"},
		{SourcePath: "/etc/apt/keyrings/custom-b.gpg"},
	}
	if got, want := countBundled(entries), 3; got != want {
		t.Errorf("countBundled = %d; want %d", got, want)
	}
	if got, want := countSystem(entries), 1; got != want {
		t.Errorf("countSystem = %d; want %d", got, want)
	}
	if got, want := countCustom(entries), 2; got != want {
		t.Errorf("countCustom = %d; want %d", got, want)
	}
	if got, want := countBundled(nil), 0; got != want {
		t.Errorf("countBundled(nil) = %d; want %d", got, want)
	}
}

func TestOutcomeBadgeClass(t *testing.T) {
	cases := []struct {
		outcome string
		want    string
	}{
		{"success", "b--ok"},
		{"gpg_failed", "b--crit"},
		{"fetch_failed", "b--crit"},
		{"parse_failed", "b--crit"},
		{"lagging", "b--warn"},
		{"watching", "b--warn"},
		{"", "b--stale"},
		{"future_unknown_outcome", "b--stale"},
	}
	for _, c := range cases {
		if got := outcomeBadgeClass(c.outcome); got != c.want {
			t.Errorf("outcomeBadgeClass(%q) = %q; want %q", c.outcome, got, c.want)
		}
	}
}
