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
// given offset, up to the next >, for row-scoped assertions.
func htmlRowStart(html string, offset int) string {
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
