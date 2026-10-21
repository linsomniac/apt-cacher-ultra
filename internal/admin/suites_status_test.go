package admin

import (
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

func p64(v int64) *int64    { return &v }
func pstr(s string) *string { return &s }

// Spec C7 "Visibility": failing and refused records render as text; the
// lag is measured from the first sighting against now.
func TestBuildSuiteEntries_RecordAndLag(t *testing.T) {
	const now = int64(1_800_000_000)
	failing := cache.SuiteWithAdoption{SuiteFreshness: cache.SuiteFreshness{
		CanonicalHost: "h", SuitePath: "/a", CheckFailCount: 2, LastRequestedAt: p64(now - 60),
		AdoptionFailReason: pstr("untrusted_signer"), AdoptionFailCount: 4,
		AdoptionFailFirstAt: p64(now - (3*86400 + 2*3600)), AdoptionNextRetryAt: p64(now + 41*60),
		InReleaseChangeSeenAt: p64(now - 5400)}, CurrentAnchorForm: "inline"}
	refused := cache.SuiteWithAdoption{SuiteFreshness: cache.SuiteFreshness{
		CanonicalHost: "h", SuitePath: "/b", AdoptionFailReason: pstr(cache.ReasonUpstreamOlder),
		AdoptionFailCount: 1, AdoptionFailFirstAt: p64(now - 60), AdoptionNextRetryAt: p64(now - 1)}}
	clean := cache.SuiteWithAdoption{SuiteFreshness: cache.SuiteFreshness{CanonicalHost: "h", SuitePath: "/c"}}

	got := buildSuiteEntries([]cache.SuiteWithAdoption{failing, refused, clean}, now)
	f, r, c := got[0], got[1], got[2]
	if !f.AdoptionFailing || f.AdoptionRefused || f.AdoptionStatus != "adoption failing 3d 2h · untrusted_signer · retry in 41m" {
		t.Errorf("failing entry = %+v", f)
	}
	if f.AnchorForm != "inline" || f.CheckFailCount != 2 || f.LastRequestedUnixTime == nil || f.AdoptionFailCount != 4 {
		t.Errorf("failing entry fields = %+v", f)
	}
	if f.Lagging != "(lagging 1h 30m)" || f.LagSeconds != 5400 || !f.LagWarn {
		t.Errorf("lag = %q / %d / warn %v, want (lagging 1h 30m) / 5400 / true", f.Lagging, f.LagSeconds, f.LagWarn)
	}
	if r.AdoptionFailing || !r.AdoptionRefused || r.AdoptionStatus != "refused: upstream_older · retry due" {
		t.Errorf("refused entry = %+v", r)
	}
	if c.AdoptionFailing || c.AdoptionRefused || c.AdoptionStatus != "" || c.Lagging != "" {
		t.Errorf("clean entry = %+v", c)
	}
}

// Deferred minor B-61: adoptionStatus nil-checks the record's reason
// itself, not trusting its caller's guard: a row with a failure count but
// no reason (refused or failing, escalated or not) summarises to "", where
// it used to dereference a nil pointer.
func TestAdoptionStatus_ANilReasonIsNoRecord(t *testing.T) {
	const now = int64(1_800_000_000)
	s := cache.SuiteFreshness{CanonicalHost: "h", SuitePath: "/a", AdoptionFailCount: 3,
		AdoptionFailFirstAt: p64(now - 2*86400), AdoptionNextRetryAt: p64(now + 60)}
	for _, tc := range []struct{ refused, long bool }{{false, false}, {true, false}, {true, true}} {
		if got := adoptionStatus(s, tc.refused, tc.long, now); got != "" {
			t.Errorf("adoptionStatus(refused %v, long %v) without a reason = %q; want \"\"", tc.refused, tc.long, got)
		}
	}
	s.AdoptionFailReason = pstr("untrusted_signer")
	if got := adoptionStatus(s, false, false, now); got != "adoption failing 2d 0h · untrusted_signer · retry in 1m" {
		t.Errorf("adoptionStatus with a reason = %q", got)
	}
}

// task-14-extra.md ruling (spec C7): a refusal is a state, not a
// failure. outcomeBadgeClass must render it b--warn, not b--crit.
func TestOutcomeBadgeClass_Refused(t *testing.T) {
	if got := outcomeBadgeClass("refused"); got != "b--warn" {
		t.Errorf("outcomeBadgeClass(%q) = %q; want %q", "refused", got, "b--warn")
	}
}

// task-14-extra.md: reasonTooltip must carry specific, non-passthrough
// text for every reason the phase B outcomes introduce, and db_failed's
// retry wording must cite the backoff schedule (spec C7 retry
// discipline) rather than "the next freshness tick".
func TestReasonTooltip_PhaseBOutcomes(t *testing.T) {
	for _, tag := range []string{
		"refused", "upstream_older", "rollback_refused",
		"release_date_in_future", "timed_out", "panic", "panicked",
	} {
		got := reasonTooltip(tag)
		if got == "" || got == tag {
			t.Errorf("reasonTooltip(%q) = %q; want a specific explanation, not passthrough", tag, got)
		}
	}
	if got := reasonTooltip("db_failed"); !strings.Contains(got, "backoff schedule") {
		t.Errorf("reasonTooltip(db_failed) = %q; want it to cite the backoff schedule", got)
	}
}

// task-14-extra.md: the recent-adoptions table marks a refused row
// data-state="warn" (not crit), a genuine failure stays crit, and the
// panel's failure count excludes the refused row.
func TestRecentAdoptionsRow_RefusedIsWarnNotFailure(t *testing.T) {
	m := newHealthyModel()
	m.RecentAdoptions = []adoptionEntry{
		{Host: "a.example.com", SuitePath: "dists/noble", Outcome: "refused", Reason: "upstream_older", CompletedUnixTime: 1700, DurationSeconds: 0.1},
		{Host: "b.example.com", SuitePath: "dists/noble", Outcome: "gpg_failed", Reason: "untrusted_signer", CompletedUnixTime: 1800, DurationSeconds: 0.1},
	}
	html := renderHTMLForGolden(t, m)

	refusedRowStart := strings.Index(html, `data-outcome="refused"`)
	gpgRowStart := strings.Index(html, `data-outcome="gpg_failed"`)
	if refusedRowStart < 0 || gpgRowStart < 0 {
		t.Fatalf("missing expected rows in rendered HTML")
	}
	refusedRow := htmlRowStart(html, refusedRowStart)
	gpgRow := htmlRowStart(html, gpgRowStart)
	if !strings.Contains(refusedRow, `data-state="warn"`) {
		t.Errorf("refused row should carry data-state=\"warn\"; row: %s", refusedRow)
	}
	if strings.Contains(refusedRow, `data-state="crit"`) {
		t.Errorf("refused row must not carry data-state=\"crit\"; row: %s", refusedRow)
	}
	if !strings.Contains(gpgRow, `data-state="crit"`) {
		t.Errorf("gpg_failed row should still carry data-state=\"crit\"; row: %s", gpgRow)
	}

	// The panel counters must not count the refused row as a failure.
	if !strings.Contains(html, `count-crit">1 FAILED`) {
		t.Errorf("expected exactly 1 FAILED (excluding the refused row); html excerpt:\n%s", excerpt(html, "IN RING", 200))
	}
	if !strings.Contains(html, `count-warn">1 REFUSED`) {
		t.Errorf("expected 1 REFUSED counted separately; html excerpt:\n%s", excerpt(html, "IN RING", 200))
	}
}

// htmlRowStart returns the substring starting at the <tr preceding the
// given offset, up to the next >, for row-scoped assertions; "" for an
// offset outside html (deferred minor B-62: a strings.Index miss, -1,
// fails the caller's assertion instead of panicking).
func htmlRowStart(html string, offset int) string {
	if offset < 0 || offset > len(html) {
		return ""
	}
	trStart := strings.LastIndex(html[:offset], "<tr")
	if trStart < 0 {
		return ""
	}
	end := strings.Index(html[trStart:], ">")
	if end < 0 {
		return html[trStart:]
	}
	return html[trStart : trStart+end+1]
}

// Controller ruling (task-14 review round 1, spec C7): a refusal reason
// where the cache already serves content at least as new as upstream
// (upstream_older, rollback_refused) must not accrue lag.
// RecordAdoptionFailure never clears inrelease_change_seen_at, and
// deferred checks keep the first sighting, so an unsuppressed row would
// show growing "(lagging …)" text and eventually push the Suites vital
// to warn and then crit for a suite that isn't actually behind. A refusal
// for a reason where the cache really is behind (the phase E reasons,
// represented here by unsigned_downgrade_refused) keeps today's lag
// behaviour.
func TestBuildSuiteEntries_RefusalLagSuppression(t *testing.T) {
	const now = int64(1_800_000_000)
	twoDaysAgo := now - 2*86400

	mk := func(reason string) cache.SuiteWithAdoption {
		return cache.SuiteWithAdoption{SuiteFreshness: cache.SuiteFreshness{
			CanonicalHost: "h", SuitePath: "/x", AdoptionFailReason: pstr(reason),
			AdoptionFailCount: 1, AdoptionFailFirstAt: p64(now - 60),
			InReleaseChangeSeenAt: p64(twoDaysAgo),
		}}
	}

	for _, reason := range []string{cache.ReasonUpstreamOlder, cache.ReasonRollbackRefused} {
		t.Run(reason, func(t *testing.T) {
			got := buildSuiteEntries([]cache.SuiteWithAdoption{mk(reason)}, now)
			e := got[0]
			if e.LagWarn || e.LagSeconds != 0 || e.Lagging != "" {
				t.Errorf("reason %q: lag = %q / %d / warn %v; want suppressed (empty / 0 / false)",
					reason, e.Lagging, e.LagSeconds, e.LagWarn)
			}
			m := htmlRenderModel{statusModel: statusModel{Suites: []suiteEntry{e}}, AdoptionEnabled: true}
			if got := vitalState("suites", m); got != "ok" {
				t.Errorf("reason %q: vitalState(suites) = %q; want ok", reason, got)
			}
			render := newHealthyModel()
			render.Suites = []suiteEntry{e}
			html := renderHTMLForGolden(t, render)
			if row := htmlRowStart(html, strings.Index(html, `data-label="Host" class="host">h<`)); strings.Contains(row, `data-state="warn"`) {
				t.Errorf("reason %q: rendered row unexpectedly carries data-state=\"warn\": %s", reason, row)
			}
		})
	}

	// A refusal reason where the cache really is behind keeps the
	// ordinary lag behaviour and can still push the vital to crit.
	got := buildSuiteEntries([]cache.SuiteWithAdoption{mk(cache.ReasonUnsignedDowngradeRefused)}, now)
	e := got[0]
	if !e.LagWarn || e.LagSeconds != 2*86400 || e.Lagging == "" {
		t.Errorf("unsigned_downgrade_refused: lag = %q / %d / warn %v; want lag preserved",
			e.Lagging, e.LagSeconds, e.LagWarn)
	}
	m := htmlRenderModel{statusModel: statusModel{Suites: []suiteEntry{e}}, AdoptionEnabled: true}
	if got := vitalState("suites", m); got != "crit" {
		t.Errorf("unsigned_downgrade_refused: vitalState(suites) = %q; want crit", got)
	}
}

// Final review S I1 / T I1: the aggregate notice's JS skips refused like
// success (a refusal is a state, not a failure: the ADOPTIONS vital and
// the panel counters already exclude it), and its hint table covers the
// phase B outcomes with db_failed citing the backoff schedule. The script
// is a static literal, so a string-level assertion on the rendered page
// pins it, as the golden tests pin markup.
func TestNoticeScript_RefusedIsNotAFailureAndHintsCoverPhaseB(t *testing.T) {
	html := renderHTMLForGolden(t, newHealthyModel())
	mustContain(t, html,
		`if(o==='success'||o==='refused'||!o)return;`,
		`'release_date_in_future':{text:`,
		`'timed_out':{text:`,
		`'panicked':{text:`,
	)
	i := strings.Index(html, `'db_failed':{text:'`)
	if i < 0 {
		t.Fatal("no db_failed hint")
	}
	hint := html[i:]
	hint = hint[:strings.Index(hint, "'}")]
	if !strings.Contains(hint, "backoff schedule") || strings.Contains(hint, "next freshness tick") {
		t.Errorf("db_failed hint = %q; want it to cite the backoff schedule", hint)
	}
}

// suiteRows builds the three rows the admin tests below render: a failing
// record, a fresh refusal (upstream_older, an hour old) and a refusal
// whose streak began 25 h ago, plus a lagging suite with no record.
func suiteRows(now int64) []cache.SuiteWithAdoption {
	return []cache.SuiteWithAdoption{
		{SuiteFreshness: cache.SuiteFreshness{CanonicalHost: "fail.example", SuitePath: "/a",
			AdoptionFailReason: pstr("untrusted_signer"), AdoptionFailCount: 4,
			AdoptionFailFirstAt: p64(now - 2*3600), AdoptionNextRetryAt: p64(now + 41*60)}},
		{SuiteFreshness: cache.SuiteFreshness{CanonicalHost: "refused.example", SuitePath: "/b",
			AdoptionFailReason: pstr(cache.ReasonUpstreamOlder), AdoptionFailCount: 2,
			AdoptionFailFirstAt: p64(now - 3600), AdoptionNextRetryAt: p64(now - 1),
			InReleaseChangeSeenAt: p64(now - 3*86400)}},
		{SuiteFreshness: cache.SuiteFreshness{CanonicalHost: "stuck.example", SuitePath: "/c",
			AdoptionFailReason: pstr(cache.ReasonUpstreamOlder), AdoptionFailCount: 30,
			AdoptionFailFirstAt: p64(now - 25*3600), AdoptionNextRetryAt: p64(now + 30*60),
			InReleaseChangeSeenAt: p64(now - 25*3600)}},
		{SuiteFreshness: cache.SuiteFreshness{CanonicalHost: "lag.example", SuitePath: "/d",
			InReleaseChangeSeenAt: p64(now - 2*86400)}},
		// Deferred minor B-59: a lag under an hour shows, but never warns.
		{SuiteFreshness: cache.SuiteFreshness{CanonicalHost: "young.example", SuitePath: "/e",
			InReleaseChangeSeenAt: p64(now - 30*60)}},
	}
}

// suiteRow returns the opening <tr …> tag of the Suites row for host.
func suiteRow(t *testing.T, html, host string) string {
	t.Helper()
	i := strings.Index(html, `data-label="Host" class="host">`+host+`<`)
	if i < 0 {
		t.Fatalf("no Suites row for %s", host)
	}
	return htmlRowStart(html, i)
}

// Final ruling A5: a refusal is a state, not a failure, but one whose
// streak began more than 24 h ago warns (row and SUITES vital, never
// crit), whatever refusalKeepsCurrent says, and its Adoption text says
// since when. Below 24 h nothing changes; a future streak start (clock
// step) counts as now.
func TestBuildSuiteEntries_OldRefusalEscalates(t *testing.T) {
	const now = int64(1_800_000_000)
	mk := func(firstAt int64) cache.SuiteWithAdoption {
		return cache.SuiteWithAdoption{SuiteFreshness: cache.SuiteFreshness{CanonicalHost: "h", SuitePath: "/x",
			AdoptionFailReason: pstr(cache.ReasonUpstreamOlder), AdoptionFailCount: 30,
			AdoptionFailFirstAt: p64(firstAt), AdoptionNextRetryAt: p64(now - 1),
			InReleaseChangeSeenAt: p64(now - 3*86400)}}
	}
	cases := []struct {
		name    string
		firstAt int64
		long    bool
		status  string
	}{
		{"23 h", now - 23*3600, false, "refused: upstream_older · retry due"},
		{"exactly 24 h", now - 24*3600, false, "refused: upstream_older · retry due"},
		{"25 h", now - 25*3600, true, "refused: upstream_older · since 1d 1h · retry due"},
		{"streak start in the future", now + 86400, false, "refused: upstream_older · retry due"},
	}
	for _, tc := range cases {
		e := buildSuiteEntries([]cache.SuiteWithAdoption{mk(tc.firstAt)}, now)[0]
		if e.RefusedLong != tc.long || e.AdoptionStatus != tc.status || e.AdoptionFailing || e.LagWarn {
			t.Errorf("%s: long %v status %q failing %v lagwarn %v; want long %v status %q, not failing, no lag warn",
				tc.name, e.RefusedLong, e.AdoptionStatus, e.AdoptionFailing, e.LagWarn, tc.long, tc.status)
		}
		m := htmlRenderModel{statusModel: statusModel{Suites: []suiteEntry{e}}, AdoptionEnabled: true}
		want := "ok"
		if tc.long {
			want = "warn"
		}
		if got := vitalState("suites", m); got != want {
			t.Errorf("%s: vitalState(suites) = %q; want %q", tc.name, got, want)
		}
	}
}

// Final review T M4: the Suites table renders the Adoption cell, the
// FAILING counts and the refusal escalation; the ADOPTIONS vital renders
// the REFUSED sub-line (a refused event is not counted as failed).
func TestSuitesTable_RendersRecordsAndCounts(t *testing.T) {
	const now = int64(1_800_000_000)
	m := newHealthyModel()
	m.Suites = buildSuiteEntries(suiteRows(now), now)
	m.RecentAdoptions = []adoptionEntry{
		{Host: "refused.example", SuitePath: "/b", Outcome: "refused", Reason: "upstream_older", CompletedUnixTime: 1700, DurationSeconds: 0.1},
		{Host: "fail.example", SuitePath: "/a", Outcome: "gpg_failed", Reason: "untrusted_signer", CompletedUnixTime: 1800, DurationSeconds: 0.1},
	}
	html := renderHTMLForGolden(t, m)
	mustContain(t, html,
		`<td data-label="Adoption" class="mono">adoption failing 2h 0m · untrusted_signer · retry in 41m</td>`,
		`<td data-label="Adoption" class="mono">refused: upstream_older · retry due</td>`,
		`<td data-label="Adoption" class="mono">refused: upstream_older · since 1d 1h · retry in 30m</td>`,
		// SUITES vital sub-line and panel eyebrow.
		`<b class="accent-warn">1 LAGGING</b> &middot; <b class="accent-warn">1 FAILING</b> &middot; <b class="accent-warn">1 REFUSED 24H+</b>`,
		`<span class="count-warn">1 FAILING</span>`,
		`<span class="count-warn">1 REFUSED 24H+</span>`,
		// ADOPTIONS vital sub-line: the refusal is counted apart from failures.
		`<b class="accent-crit">1 FAILED</b> &middot; <b class="accent-warn">1 REFUSED</b>`,
		`<span class="lagging">(lagging 30m)</span>`,
	)
	for host, warn := range map[string]bool{"fail.example": true, "refused.example": false, "stuck.example": true, "lag.example": true,
		"young.example": false} {
		if got := strings.Contains(suiteRow(t, html, host), `data-state="warn"`); got != warn {
			t.Errorf("%s row warn = %v, want %v", host, got, warn)
		}
	}
	if !strings.Contains(extractTag(html, "<article", `data-vital="suites"`), `data-state="crit"`) {
		t.Errorf("SUITES vital not crit with a 2-day lag; tag: %s", extractTag(html, "<article", `data-vital="suites"`))
	}
}

// Final review S I3 / ruling A4: with adoption disabled (the shipped
// default) a stored record is inert — its text gains "(adoption
// disabled)" and it marks no warn — rows keep their lag text without
// warning, and the SUITES vital is stale with "ADOPTION DISABLED —
// METADATA NOT REFRESHED", so the page is not DEGRADED forever. Enabled,
// the same rows keep their warn states.
func TestSuites_AdoptionDisabledIsNotDegraded(t *testing.T) {
	const now = int64(1_800_000_000)
	base := newHealthyModel().statusModel
	base.Suites = buildSuiteEntries(suiteRows(now), now)

	disabled := (&Server{cfg: Config{}}).buildHTMLRenderModel(base)
	if disabled.AdoptionEnabled {
		t.Fatal("setup: adoption should be disabled")
	}
	for _, e := range disabled.Suites {
		if e.AdoptionFailing || e.RefusedLong || e.LagWarn {
			t.Errorf("%s: failing %v long %v lagwarn %v with adoption disabled; want none", e.Host, e.AdoptionFailing, e.RefusedLong, e.LagWarn)
		}
		if e.AdoptionFailReason != nil && !strings.HasSuffix(e.AdoptionStatus, " (adoption disabled)") {
			t.Errorf("%s: status %q; want the (adoption disabled) suffix", e.Host, e.AdoptionStatus)
		}
	}
	if got := vitalState("suites", disabled); got != "stale" {
		t.Errorf("vitalState(suites) with adoption disabled = %q; want stale", got)
	}
	if v := verdictExplanation(disabled); strings.Contains(v, "Degraded") || strings.Contains(v, "Watching") {
		t.Errorf("verdict with adoption disabled = %q; want nominal", v)
	}
	html := renderHTMLForGolden(t, disabled)
	mustContain(t, html,
		`ADOPTION DISABLED &mdash; METADATA NOT REFRESHED`,
		`(lagging 2d 0h)`,
		`adoption failing 2h 0m · untrusted_signer · retry in 41m (adoption disabled)`,
	)
	if tag := extractTag(html, "<article", `data-vital="suites"`); !strings.Contains(tag, `data-state="stale"`) {
		t.Errorf("SUITES vital tag = %s; want data-state=\"stale\"", tag)
	}
	for _, host := range []string{"fail.example", "refused.example", "stuck.example", "lag.example"} {
		if row := suiteRow(t, html, host); strings.Contains(row, `data-state="warn"`) {
			t.Errorf("%s row warns with adoption disabled: %s", host, row)
		}
	}

	enabled := (&Server{cfg: Config{Keyring: &fakeKeyringProvider{snap: []KeyringEntrySnapshot{}}}}).buildHTMLRenderModel(base)
	if got := vitalState("suites", enabled); got != "crit" {
		t.Errorf("vitalState(suites) with adoption enabled = %q; want crit (2-day lag)", got)
	}
	for _, e := range enabled.Suites {
		if strings.HasSuffix(e.AdoptionStatus, "(adoption disabled)") {
			t.Errorf("%s: status %q carries the disabled suffix with adoption enabled", e.Host, e.AdoptionStatus)
		}
	}
}

func TestLaggingAnnotation_FromFirstSighting(t *testing.T) {
	const now = int64(100_000)
	cases := []struct {
		seen *int64
		want string
		lag  int64
	}{
		{nil, "", 0},
		{p64(now), "(lagging 0m)", 0},
		{p64(now - 1800), "(lagging 30m)", 1800},
		{p64(now - 90000), "(lagging 1d 1h)", 90000},
		{p64(now + 60), "", 0}, // a future stamp (clock step) shows no lag
	}
	for _, tc := range cases {
		got, lag := laggingAnnotation(tc.seen, now)
		if got != tc.want || lag != tc.lag {
			t.Errorf("laggingAnnotation(%v) = %q, %d; want %q, %d", tc.seen, got, lag, tc.want, tc.lag)
		}
	}
}

// Spec C7, C11 (the B carry-forward): suites[].flat is
// aptpath.IsFlatSuitePath of the row's suite path — a flat suite and its
// ./ twin are flat; a dists/ suite and a path no grammar produces are not.
func TestBuildSuiteEntries_Flat(t *testing.T) {
	const now = int64(1_800_000_000)
	want := map[string]bool{"/core:/stable:/v1.34/deb": true, "/repo/.": true, "/ubuntu/dists/noble": false,
		"/legacy//dists/x": false, "/repo/./.": false}
	var rows []cache.SuiteWithAdoption
	for p := range want {
		rows = append(rows, cache.SuiteWithAdoption{SuiteFreshness: cache.SuiteFreshness{CanonicalScheme: "http",
			CanonicalHost: "h", SuitePath: p}})
	}
	for _, e := range buildSuiteEntries(rows, now) {
		if e.Flat != want[e.SuitePath] {
			t.Errorf("%s: flat = %v, want %v", e.SuitePath, e.Flat, want[e.SuitePath])
		}
	}
}

// The status page tags a flat suite's path "flat" in the Suites table;
// a dists/ suite's cell is the path alone.
func TestSuitesTable_ShowsTheFlatTag(t *testing.T) {
	const now = int64(1_800_000_000)
	m := newHealthyModel()
	m.Suites = buildSuiteEntries([]cache.SuiteWithAdoption{
		{SuiteFreshness: cache.SuiteFreshness{CanonicalScheme: "https", CanonicalHost: "pkgs.k8s.io", SuitePath: "/core:/stable:/v1.34/deb"}},
		{SuiteFreshness: cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "archive.ubuntu.com", SuitePath: "/ubuntu/dists/noble"}},
	}, now)
	html := renderHTMLForGolden(t, m)
	mustContain(t, html,
		`<td data-label="Suite path" class="mono">/core:/stable:/v1.34/deb <span class="b b--neutral">flat</span></td>`,
		`<td data-label="Suite path" class="mono">/ubuntu/dists/noble</td>`,
	)
}

// Deferred minor B-62: an offset strings.Index did not find is no row.
func TestHTMLRowStart_AMissIsNoRow(t *testing.T) {
	for _, off := range []int{-1, 100} {
		if got := htmlRowStart("<tr a><td>x</td></tr>", off); got != "" {
			t.Errorf("htmlRowStart(…, %d) = %q, want \"\"", off, got)
		}
	}
}
