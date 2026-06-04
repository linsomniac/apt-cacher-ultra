package admin

import (
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/observability"
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
		"vitalState",
		"verdictExplanation",
		"splitUID",
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

func TestSplitUID(t *testing.T) {
	cases := []struct {
		in    string
		name  string
		email string
	}{
		{"", "", ""},
		{"Name only", "Name only", ""},
		{"Display Name <email@example.com>", "Display Name", "email@example.com"},
		{"Ubuntu Archive Automatic Signing Key (2018) <ftpmaster@ubuntu.com>", "Ubuntu Archive Automatic Signing Key (2018)", "ftpmaster@ubuntu.com"},
		{"  Trim Me  <a@b>  ", "Trim Me", "a@b"},
		{"<email-only@x>", "", "email-only@x"},
		{"Name with > char in it <e@x>", "Name with > char in it", "e@x"},
		{"Bare <unterminated", "Bare <unterminated", ""},
	}
	for _, c := range cases {
		got := splitUID(c.in)
		if got.Name != c.name || got.Email != c.email {
			t.Errorf("splitUID(%q) = {Name:%q Email:%q}; want {Name:%q Email:%q}",
				c.in, got.Name, got.Email, c.name, c.email)
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

func TestDurationOf(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{-5, "0m"},
		{0, "0m"},
		{59, "0m"},
		{60, "1m"},
		{300, "5m"},
		{3540, "59m"},
		{3600, "1h 0m"},
		{3900, "1h 5m"},
		{86340, "23h 59m"}, // just under a day stays in hours
		{86400, "1d 0h"},   // exactly one day rolls over to days
		{90000, "1d 1h"},
		{223500, "2d 14h"}, // 62h 5m collapses to days + hours (minutes dropped)
	}
	for _, c := range cases {
		if got := durationOf(c.in); got != c.want {
			t.Errorf("durationOf(%d) = %q; want %q", c.in, got, c.want)
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

func ptrI64(v int64) *int64 { return &v }

func TestVitalState(t *testing.T) {
	// Helper to build a wrapper with sensible defaults.
	base := func() htmlRenderModel {
		return htmlRenderModel{
			statusModel: statusModel{
				Process: processInfo{UptimeSeconds: 3600, StartedUnixTime: 1_000_000_000},
				Cache:   cacheInfo{BytesUsed: 1024},
				GC:      &gcInfo{LastRunUnixTime: ptrI64(1_000_003_500)},
			},
			GCIntervalSeconds: 60,
		}
	}

	cases := []struct {
		name string
		mut  func(*htmlRenderModel)
		kind string
		want string
	}{
		{"cache_healthy", func(*htmlRenderModel) {}, "cache", "ok"},
		{"cache_stale_empty_bytes", func(m *htmlRenderModel) { m.Cache.BytesUsed = 0 }, "cache", "stale"},
		{"cache_watching_backlog", func(m *htmlRenderModel) { m.Cache.ActuallyReapableBlobs = 1001 }, "cache", "warn"},
		{"cache_crit_pool_unlink", func(m *htmlRenderModel) { m.GC.PoolUnlinkErrors = 1 }, "cache", "crit"},

		{"suites_stale_empty", func(*htmlRenderModel) {}, "suites", "stale"},
		{
			"suites_healthy_no_lag",
			func(m *htmlRenderModel) {
				m.Suites = []suiteEntry{{
					LastSuccessUnixTime:           ptrI64(1_000_000_000),
					InReleaseChangeSeenAtUnixTime: ptrI64(1_000_000_000),
				}}
			},
			"suites", "ok",
		},
		{
			"suites_watching_lag_under_24h",
			func(m *htmlRenderModel) {
				m.Suites = []suiteEntry{{
					LastSuccessUnixTime:           ptrI64(1_000_000_000),
					InReleaseChangeSeenAtUnixTime: ptrI64(1_000_000_000 + 3600),
				}}
			},
			"suites", "warn",
		},
		{
			"suites_crit_lag_over_24h",
			func(m *htmlRenderModel) {
				m.Suites = []suiteEntry{{
					LastSuccessUnixTime:           ptrI64(1_000_000_000),
					InReleaseChangeSeenAtUnixTime: ptrI64(1_000_000_000 + 24*3600 + 1),
				}}
			},
			"suites", "crit",
		},

		{
			"adoptions_stale_warmup",
			func(m *htmlRenderModel) { m.Process.UptimeSeconds = 60 },
			"adoptions", "stale",
		},
		{
			"adoptions_healthy_post_warmup",
			func(m *htmlRenderModel) {}, // uptime already 3600s, empty ring
			"adoptions", "ok",
		},
		{
			"adoptions_watching_at_10pct",
			func(m *htmlRenderModel) {
				m.RecentAdoptions = make([]adoptionEntry, 10)
				for i := range m.RecentAdoptions {
					m.RecentAdoptions[i].Outcome = "success"
				}
				m.RecentAdoptions[0].Outcome = "gpg_failed"
			},
			"adoptions", "warn",
		},
		{
			"adoptions_crit_at_50pct",
			func(m *htmlRenderModel) {
				m.RecentAdoptions = make([]adoptionEntry, 10)
				for i := range m.RecentAdoptions {
					if i < 5 {
						m.RecentAdoptions[i].Outcome = "gpg_failed"
					} else {
						m.RecentAdoptions[i].Outcome = "success"
					}
				}
			},
			"adoptions", "crit",
		},

		{
			"gc_stale_no_run",
			func(m *htmlRenderModel) { m.GC = &gcInfo{} },
			"gc", "stale",
		},
		{
			"gc_stale_nil_gc",
			func(m *htmlRenderModel) { m.GC = nil },
			"gc", "stale",
		},
		{"gc_healthy_recent", func(*htmlRenderModel) {}, "gc", "ok"},
		{
			"gc_watching_over_2x_interval",
			func(m *htmlRenderModel) {
				m.GCIntervalSeconds = 60
				m.GC = &gcInfo{LastRunUnixTime: ptrI64(1_000_000_000)}
				m.Process.StartedUnixTime = 1_000_000_000
				m.Process.UptimeSeconds = 200 // age 200 > 2*60
			},
			"gc", "warn",
		},
		{
			"gc_watching_suppressed_when_interval_zero",
			func(m *htmlRenderModel) {
				m.GCIntervalSeconds = 0
				m.GC = &gcInfo{LastRunUnixTime: ptrI64(1_000_000_000)}
				m.Process.StartedUnixTime = 1_000_000_000
				m.Process.UptimeSeconds = 999999
			},
			"gc", "ok",
		},
		{
			"gc_crit_deadline_reached",
			func(m *htmlRenderModel) {
				m.GC = &gcInfo{
					LastRunUnixTime:        ptrI64(1_000_003_500),
					LastRunDeadlineReached: true,
				}
			},
			"gc", "crit",
		},

		{
			"active_stale_warmup_empty",
			func(m *htmlRenderModel) { m.Process.UptimeSeconds = 100 },
			"active", "stale",
		},
		{"active_healthy_post_warmup_empty", func(*htmlRenderModel) {}, "active", "ok"},
		{
			"active_healthy_with_host",
			func(m *htmlRenderModel) {
				m.ActiveHosts = []activeHostInfo{{Host: "x", Inflight: 1, SlotCapacity: 4}}
			},
			"active", "ok",
		},

		{"unknown_kind_defaults_healthy", func(*htmlRenderModel) {}, "fnord", "ok"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			m := base()
			c.mut(&m)
			if got := vitalState(c.kind, m); got != c.want {
				t.Errorf("vitalState(%q) = %q; want %q", c.kind, got, c.want)
			}
		})
	}
}

func TestVerdictExplanation(t *testing.T) {
	healthy := htmlRenderModel{
		statusModel: statusModel{
			Process: processInfo{UptimeSeconds: 7200, StartedUnixTime: 1_000_000_000},
			Cache:   cacheInfo{BytesUsed: 1024},
			GC:      &gcInfo{LastRunUnixTime: ptrI64(1_000_006_000)},
		},
		GCIntervalSeconds: 3600,
	}
	if got := verdictExplanation(healthy); got == "" {
		t.Errorf("verdictExplanation(healthy) returned empty string")
	} else if !contains(got, "nominal") {
		t.Errorf("verdictExplanation(healthy) = %q; want phrasing including 'nominal'", got)
	}

	warming := healthy
	warming.Process.UptimeSeconds = 60
	warming.GC = &gcInfo{} // no LastRunUnixTime
	if got := verdictExplanation(warming); !contains(got, "Warming up") {
		t.Errorf("verdictExplanation(warming) = %q; want 'Warming up'", got)
	}

	watching := healthy
	watching.Cache.ActuallyReapableBlobs = 5000 // cache watching
	if got := verdictExplanation(watching); !contains(got, "Watching") {
		t.Errorf("verdictExplanation(watching) = %q; want 'Watching'", got)
	}

	degraded := healthy
	degraded.GC = &gcInfo{
		LastRunUnixTime:        ptrI64(1_000_006_000),
		LastRunDeadlineReached: true,
	}
	if got := verdictExplanation(degraded); !contains(got, "Degraded") {
		t.Errorf("verdictExplanation(degraded) = %q; want 'Degraded'", got)
	}

	// §5.1.1: adoption enabled + zero keys is a crit signal that must
	// flip the noscript verdict away from "nominal".
	keyringEmptyEnabled := healthy
	keyringEmptyEnabled.AdoptionEnabled = true
	keyringEmptyEnabled.Keyring = nil
	if got := verdictExplanation(keyringEmptyEnabled); !contains(got, "Degraded") {
		t.Errorf("verdictExplanation(adoption-enabled+no-keys) = %q; want 'Degraded'", got)
	}

	// Adoption disabled + empty keyring is the operator's choice and
	// must NOT push the verdict away from nominal.
	keyringEmptyDisabled := healthy
	keyringEmptyDisabled.AdoptionEnabled = false
	keyringEmptyDisabled.Keyring = nil
	if got := verdictExplanation(keyringEmptyDisabled); !contains(got, "nominal") {
		t.Errorf("verdictExplanation(adoption-disabled+no-keys) = %q; want 'nominal'", got)
	}
}

func TestKeyringCrit(t *testing.T) {
	cases := []struct {
		name    string
		enabled bool
		keys    []keyringEntry
		want    bool
	}{
		{"disabled_no_keys", false, nil, false},
		{"disabled_with_keys", false, []keyringEntry{{}}, false},
		{"enabled_no_keys", true, nil, true},
		{"enabled_empty_slice", true, []keyringEntry{}, true},
		{"enabled_with_keys", true, []keyringEntry{{}}, false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			m := htmlRenderModel{
				statusModel:     statusModel{Keyring: c.keys},
				AdoptionEnabled: c.enabled,
			}
			if got := keyringCrit(m); got != c.want {
				t.Errorf("keyringCrit(enabled=%v, keys=%d) = %v; want %v", c.enabled, len(c.keys), got, c.want)
			}
		})
	}
}

// fakeKeyringProvider lets the tests exercise both the
// adoption-disabled (nil snapshot) and adoption-enabled (non-nil
// possibly-empty snapshot) branches of buildHTMLRenderModel.
type fakeKeyringProvider struct {
	snap []KeyringEntrySnapshot
}

func (f *fakeKeyringProvider) KeyringSnapshot() []KeyringEntrySnapshot { return f.snap }

func TestBuildHTMLRenderModelAdoptionDetection(t *testing.T) {
	cases := []struct {
		name     string
		provider KeyringProvider
		want     bool
	}{
		{"no_provider_disabled", nil, false},
		{"provider_nil_snapshot_disabled", &fakeKeyringProvider{snap: nil}, false},
		{"provider_empty_slice_enabled", &fakeKeyringProvider{snap: []KeyringEntrySnapshot{}}, true},
		{"provider_with_entries_enabled", &fakeKeyringProvider{snap: []KeyringEntrySnapshot{{PrimaryFingerprint: "x"}}}, true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			s := &Server{cfg: Config{Keyring: c.provider}}
			w := s.buildHTMLRenderModel(statusModel{})
			if w.AdoptionEnabled != c.want {
				t.Errorf("AdoptionEnabled = %v; want %v", w.AdoptionEnabled, c.want)
			}
		})
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func TestHTMLRenderModelEmbedsStatusModel(t *testing.T) {
	// Regression guard: the wrapper must promote statusModel fields so
	// the existing template's `.Cache.BytesUsed` etc. continue to work
	// without rewriting the dot path.
	m := htmlRenderModel{
		statusModel: statusModel{
			Cache:           cacheInfo{BytesUsed: 42},
			RecentAdoptions: []adoptionEntry{{Outcome: "success"}},
		},
		AdoptionEnabled:   true,
		GCIntervalSeconds: 60,
	}
	if m.Cache.BytesUsed != 42 {
		t.Errorf("embedded promotion broken: m.Cache.BytesUsed = %d", m.Cache.BytesUsed)
	}
	if len(m.RecentAdoptions) != 1 {
		t.Errorf("embedded promotion broken: RecentAdoptions len = %d", len(m.RecentAdoptions))
	}
	if !m.AdoptionEnabled || m.GCIntervalSeconds != 60 {
		t.Errorf("wrapper fields unset: %+v", m)
	}
}

// TestOutcomeBadgeClass covers the real adoption outcome enum produced
// by internal/freshness/metrics.go's classifier (success / gpg_failed /
// parse_failed / member_mismatch / unpinned_suite / run_failed). The
// enum was changed during freshness work — earlier versions of this
// helper mapped a non-existent "fetch_failed" enum which never reached
// the admin page.
func TestOutcomeBadgeClass(t *testing.T) {
	cases := []struct {
		outcome string
		want    string
	}{
		{"success", "b--ok"},
		// All non-success classifier outcomes are crit.
		{"gpg_failed", "b--crit"},
		{"parse_failed", "b--crit"},
		{"member_mismatch", "b--crit"},
		{"unpinned_suite", "b--crit"},
		{"run_failed", "b--crit"},
		// SPEC5 §10.4.3 outcomes broken out of the run_failed catch-all.
		{"member_fetch_failed", "b--crit"},
		{"db_failed", "b--crit"},
		// Soft-state values (used outside the adoption row context).
		{"lagging", "b--warn"},
		{"warn", "b--warn"},
		// Empty input is the stale case (no outcome available yet).
		{"", "b--stale"},
		// Future enum values default to crit so an unknown failure
		// never silently renders as a calm color.
		{"future_unknown_outcome", "b--crit"},
	}
	for _, c := range cases {
		if got := outcomeBadgeClass(c.outcome); got != c.want {
			t.Errorf("outcomeBadgeClass(%q) = %q; want %q", c.outcome, got, c.want)
		}
	}
}

// TestReasonTooltip pins the human-readable hover text for the adoption
// reason chips. The two outcomes broken out of run_failed
// (member_fetch_failed, db_failed) must each return a specific,
// non-passthrough explanation; unknown tags pass through verbatim.
func TestReasonTooltip(t *testing.T) {
	for _, tag := range []string{"member_fetch_failed", "db_failed"} {
		got := reasonTooltip(tag)
		if got == "" || got == tag {
			t.Errorf("reasonTooltip(%q) = %q; want a specific explanation, not passthrough", tag, got)
		}
	}
	// Unknown tags pass through verbatim (forward-compat contract).
	if got := reasonTooltip("some_future_tag"); got != "some_future_tag" {
		t.Errorf("reasonTooltip(unknown) = %q; want passthrough", got)
	}
}

// TestBuildAdoptionEntries verifies the failing-member path + detail
// copy through from the observability ring into the status-page row,
// and that success rows leave both empty.
func TestBuildAdoptionEntries(t *testing.T) {
	events := []observability.AdoptionEvent{
		{
			Host: "packages.icinga.com", SuitePath: "/ubuntu/dists/icinga-jammy",
			Outcome: "member_fetch_failed", Reason: "member_fetch_failed",
			MemberPath: "Contents-amd64", Detail: "served 114572 vs declared 1664594",
			CompletedUnixSec: 1700, DurationSeconds: 0.2,
		},
		{
			Host: "archive.ubuntu.com", SuitePath: "/ubuntu/dists/noble",
			Outcome: "success", CompletedUnixSec: 1800, DurationSeconds: 0.5,
		},
	}
	got := buildAdoptionEntries(events)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].MemberPath != "Contents-amd64" || got[0].Detail != "served 114572 vs declared 1664594" {
		t.Errorf("failure row member fields not copied: %+v", got[0])
	}
	if got[1].MemberPath != "" || got[1].Detail != "" {
		t.Errorf("success row should have empty member fields, got %+v", got[1])
	}
}
