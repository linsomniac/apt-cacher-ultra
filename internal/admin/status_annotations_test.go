package admin

import (
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/gc"
	"github.com/linsomniac/apt-cacher-ultra/internal/observability"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// Phase G ruling PG-14, Review Focus 5 (design note 5): suites[].anchorless
// marks a flat suite the cache refreshes without a Release — adoption
// enabled, flat, no current snapshot, a successful check, no record, no
// pending change — and nothing else: not a dists/ suite, not with
// adoption disabled, not an adopted, never-checked or refused suite, and
// not once a first adoption is under way (its pending change).
func TestSuiteEntries_Anchorless(t *testing.T) {
	now := int64(1_800_000_000)
	ok, id := now-60, int64(9)
	anchorless := func(mut func(*suiteEntry)) suiteEntry {
		e := suiteEntry{Scheme: "https", Host: "fury.example", SuitePath: "/org/deb", Flat: true, LastSuccessUnixTime: &ok}
		if mut != nil {
			mut(&e)
		}
		return e
	}
	for _, tc := range []struct {
		name    string
		e       suiteEntry
		enabled bool
		want    bool
	}{
		{"a Release-less flat suite", anchorless(nil), true, true},
		{"adoption disabled", anchorless(nil), false, false},
		{"a dists/ suite", anchorless(func(e *suiteEntry) { e.Flat, e.SuitePath = false, "/r/dists/x" }), true, false},
		{"adopted", anchorless(func(e *suiteEntry) { e.CurrentSnapshotID = &id }), true, false},
		{"never answered", anchorless(func(e *suiteEntry) { e.LastSuccessUnixTime = nil }), true, false},
		{"a record (unsigned_unconfirmed)", anchorless(func(e *suiteEntry) { e.AdoptionFailCount = 1 }), true, false},
		{"a first adoption under way", anchorless(func(e *suiteEntry) { e.InReleaseChangeSeenAtUnixTime = &ok }), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			es := []suiteEntry{tc.e}
			annotateSuiteEntries(es, tc.enabled, nil, now)
			if es[0].Anchorless != tc.want {
				t.Fatalf("anchorless %v, want %v", es[0].Anchorless, tc.want)
			}
			if want := map[bool]string{true: "refreshed without a Release"}[tc.want]; es[0].AdoptionStatus != want {
				t.Fatalf("Adoption cell %q, want %q", es[0].AdoptionStatus, want)
			}
		})
	}
}

// Deferred minor B-63 (design note 6; reviews M-3 and M-5): a suite whose
// checks keep ending unknown says so, and when its backoff from the last
// check ends — "backoff over" once it has — and without a backoff
// function only the count. A suite the scheduler does not queue on that
// backoff shows the count alone: a never-adopted suite no client asked
// for in 7 days or ever, an adopted one its anchor_absent or
// unsigned_disabled record paces. So does a suite never checked, and
// one whose last check lies further ahead than the clock-step tolerance
// (suiteDue reads it as infinitely old: no backoff holds it; Task 4
// review M-2). A record's own text is kept apart.
func TestSuiteEntries_FailingChecksSayWhenTheBackoffEnds(t *testing.T) {
	now := int64(1_800_000_000)
	backoff := func(k int64) time.Duration { return time.Duration(k) * time.Minute }
	recent, old, stale, ahead, edge, beyond := now-60, now-600, now-8*24*3600, now+3600, now+2, now+3
	reason, absent, unsignedDisabled := "gpg_failed", "anchor_absent", "unsigned_disabled"
	snap := int64(7)
	es := []suiteEntry{
		{SuitePath: "/a", CheckFailCount: 3, LastCheckUnixTime: &recent, LastRequestedUnixTime: &recent},
		{SuitePath: "/b", CheckFailCount: 2, LastCheckUnixTime: &old, CurrentSnapshotID: &snap},
		{SuitePath: "/c", CheckFailCount: 0, LastCheckUnixTime: &recent, LastRequestedUnixTime: &recent},
		{SuitePath: "/d", CheckFailCount: 1, LastCheckUnixTime: &recent, LastRequestedUnixTime: &recent,
			AdoptionStatus: "adoption failing 1m · gpg_failed · retry due", AdoptionFailReason: &reason, AdoptionFailCount: 1},
		{SuitePath: "/dormant", CheckFailCount: 2, LastCheckUnixTime: &old, LastRequestedUnixTime: &stale},
		{SuitePath: "/paced", CheckFailCount: 1, LastCheckUnixTime: &old, CurrentSnapshotID: &snap,
			AdoptionFailReason: &absent, AdoptionFailCount: 2},
		// Task 4 review M-3: no last_requested at all is dormant too; an
		// unsigned_disabled record paces as anchor_absent does; a suite
		// never checked has no backoff to end.
		{SuitePath: "/unasked", CheckFailCount: 2, LastCheckUnixTime: &old},
		{SuitePath: "/unsigned", CheckFailCount: 1, LastCheckUnixTime: &old, CurrentSnapshotID: &snap,
			AdoptionFailReason: &unsignedDisabled, AdoptionFailCount: 1},
		{SuitePath: "/unchecked", CheckFailCount: 3, LastRequestedUnixTime: &recent},
		// Task 4 review M-2: a last check beyond the clock-step tolerance
		// holds no backoff; one within it still does. /beyond, one second
		// past the 2 s tolerance, pins its upper bound (the H1 final
		// review's tests M-1).
		{SuitePath: "/future", CheckFailCount: 2, LastCheckUnixTime: &ahead, LastRequestedUnixTime: &recent},
		{SuitePath: "/tolerated", CheckFailCount: 2, LastCheckUnixTime: &edge, LastRequestedUnixTime: &recent},
		{SuitePath: "/beyond", CheckFailCount: 2, LastCheckUnixTime: &beyond, LastRequestedUnixTime: &recent},
	}
	annotateSuiteEntries(es, true, backoff, now)
	for i, want := range []string{
		"checks failing ×3 · backoff ends in 2m",
		"checks failing ×2 · backoff over",
		"",
		"checks failing ×1 · backoff over",
		"checks failing ×2",
		"checks failing ×1",
		"checks failing ×2",
		"checks failing ×1",
		"checks failing ×3",
		"checks failing ×2",
		"checks failing ×2 · backoff ends in 2m",
		"checks failing ×2",
	} {
		if es[i].CheckStatus != want {
			t.Errorf("%s: %q, want %q", es[i].SuitePath, es[i].CheckStatus, want)
		}
	}
	if es[3].AdoptionStatus != "adoption failing 1m · gpg_failed · retry due" {
		t.Fatalf("the record's text became %q", es[3].AdoptionStatus)
	}
	nb := []suiteEntry{{SuitePath: "/e", CheckFailCount: 4, LastCheckUnixTime: &recent, LastRequestedUnixTime: &recent}}
	annotateSuiteEntries(nb, true, nil, now)
	if nb[0].CheckStatus != "checks failing ×4" {
		t.Fatalf("without a backoff: %q, want the count alone", nb[0].CheckStatus)
	}
	off := []suiteEntry{{SuitePath: "/off", CheckFailCount: 2, LastCheckUnixTime: &old, LastRequestedUnixTime: &stale}}
	annotateSuiteEntries(off, false, backoff, now)
	if off[0].CheckStatus != "checks failing ×2 · backoff over" {
		t.Fatalf("adoption disabled: %q, want the backoff (no dormancy without an adopter)", off[0].CheckStatus)
	}
}

// The F carry-forward: the gc block copies each field of the last run's
// summary it shows — retired_reaped (sub-job C, phase F) among them. The
// literal pins each mapping; the reflection pass (Task 4 review M-4)
// fails on any gcInfo field that a summary with every field set leaves
// zero, so a field the block gains without its copy fails here too. Every
// gcInfo field comes from the summary (LastRunUnixTime from AtUnixTime),
// so none is excluded.
func TestGCInfoFromSummary_CopiesEveryShownField(t *testing.T) {
	g := gcInfoFromSummary(gc.LastRunSummary{Phase: "periodic", AtUnixTime: 1_800_000_000, DurationSeconds: 1.5,
		BlobsReaped: 2, BytesReclaimed: 3, OrphanCandidatesReaped: 4, DisplacedReaped: 5, RetiredReaped: 6,
		URLPathRowsReaped: 7, PoolOrphansRepaired: 8, PoolOrphanBytesRepaired: 9, PoolUnlinkErrors: 10, DeadlineReached: true})
	want := gcInfo{LastRunPhase: "periodic", LastRunDurationSeconds: 1.5, LastRunBlobsReaped: 2, LastRunBytesReclaimed: 3,
		OrphanCandidatesReaped: 4, DisplacedReaped: 5, RetiredReaped: 6, URLPathRowsReaped: 7, PoolOrphansRepaired: 8,
		PoolOrphanBytesRepaired: 9, PoolUnlinkErrors: 10, LastRunDeadlineReached: true}
	if g.LastRunUnixTime == nil || *g.LastRunUnixTime != 1_800_000_000 {
		t.Fatalf("last run %v, want 1800000000", g.LastRunUnixTime)
	}
	g.LastRunUnixTime = nil
	if *g != want {
		t.Fatalf("gc block %+v, want %+v", *g, want)
	}

	var full gc.LastRunSummary
	sv := reflect.ValueOf(&full).Elem()
	for i := range sv.NumField() {
		switch f := sv.Field(i); f.Kind() {
		case reflect.String:
			f.SetString("x")
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Int, reflect.Int64:
			f.SetInt(int64(i + 1))
		case reflect.Float64:
			f.SetFloat(float64(i) + 0.5)
		default:
			t.Fatalf("gc.LastRunSummary.%s is a %s: teach this test to set it", sv.Type().Field(i).Name, f.Kind())
		}
	}
	gv := reflect.ValueOf(gcInfoFromSummary(full)).Elem()
	for i := range gv.NumField() {
		if gv.Field(i).IsZero() {
			t.Errorf("gcInfo.%s is zero for a summary with every field set: gcInfoFromSummary does not copy it", gv.Type().Field(i).Name)
		}
	}
}

// Deferred minor F-19: the adoption ring carries the suite's scheme to
// recent_adoptions[].scheme.
func TestAdoptionEntries_CarryTheScheme(t *testing.T) {
	got := buildAdoptionEntries([]observability.AdoptionEvent{{Scheme: "http", Host: "h", SuitePath: "/r/dists/x", Outcome: "success"}})
	if len(got) != 1 || got[0].Scheme != "http" {
		t.Fatalf("entries %+v, want the scheme copied", got)
	}
}

// The HTML of the cells the sweep adds: the Last requested column, the anchorless
// suite's and the failing suite's Adoption cells, scheme://host in Recent
// adoption outcomes, Retired reaped in the GC panel, and the Suites empty
// state in the admin-ui-spec's words (deferred minor D-50).
func TestStatusHTML_ShowsTheSweepsCells(t *testing.T) {
	m := newHealthyModel()
	requested := int64(1_700_000_000)
	m.Suites = []suiteEntry{
		{Scheme: "https", Host: "fury.example", SuitePath: "/org/deb", Flat: true, Anchorless: true,
			AdoptionStatus: "refreshed without a Release", LastRequestedUnixTime: &requested},
		{Scheme: "http", Host: "h", SuitePath: "/r/dists/x", AdoptionStatus: "refused: upstream_older · retry due",
			CheckStatus: "checks failing ×3 · backoff ends in 2m"},
	}
	m.RecentAdoptions = []adoptionEntry{{Scheme: "http", Host: "archive.ubuntu.com", SuitePath: "dists/noble", Outcome: "success"}}
	m.GC.RetiredReaped = 6
	html := renderHTMLForGolden(t, m)
	mustContain(t, html,
		"<th>Last requested<details", // the suites header; Hot URL paths' is a bare <th>Last requested</th>
		`<td data-label="Last requested" class="time">`+string(formatUnixTimePtrTag(&requested))+`</td>`,
		`<td data-label="Adoption" class="mono">refreshed without a Release</td>`,
		`<td data-label="Adoption" class="mono">refused: upstream_older · retry due &middot; checks failing ×3 · backoff ends in 2m</td>`,
		`<td data-label="Host" class="host">http://archive.ubuntu.com</td>`,
		`<div class="k">Retired reaped</div>`,
		`<div class="v"><code>6</code></div>`,
	)
	m.Suites = nil
	if html := renderHTMLForGolden(t, m); !strings.Contains(html, "A suite is tracked once a client requests its InRelease through the proxy") {
		t.Fatal("the Suites empty state does not use the admin-ui-spec's copy")
	}
}

// Phase G ruling PG-14 through the server (design note 5; execution review
// G-4): the status page derives suites[].anchorless from the persisted row
// and the server's own adoption signal. A never-adopted flat suite whose
// last check succeeded, with no record and no pending change, reads
// anchorless: true in the JSON and "refreshed without a Release" in the
// HTML once adoption is enabled (a keyring provider with a non-nil
// snapshot), and false without it. TestSuiteEntries_Anchorless calls
// annotateSuiteEntries directly; this checks that buildStatusModel calls
// it, with the server's signal.
func TestStatus_AnchorlessFlatSuiteThroughTheServer(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "adoption enabled", false: "adoption disabled"}[enabled], func(t *testing.T) {
			var opts []adminOpt
			if enabled {
				opts = append(opts, func(cfg *Config) { cfg.Keyring = &stubKeyringProvider{entries: []KeyringEntrySnapshot{}} })
			}
			s, base, cleanup := startAdminServer(t, opts...)
			defer cleanup()
			ts := int64(1_700_000_000)
			if err := s.cfg.Cache.SeedSuiteForTest(context.Background(), cache.SuiteFreshness{CanonicalScheme: "https",
				CanonicalHost: "fury.example", SuitePath: "/org/deb", LastCheckAt: &ts, LastSuccessAt: &ts}); err != nil {
				t.Fatal(err)
			}
			resp := mustGet(t, base+"/?format=json")
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			var got struct {
				Suites []struct {
					SuitePath  string `json:"suite_path"`
					Flat       bool   `json:"flat"`
					Anchorless bool   `json:"anchorless"`
				} `json:"suites"`
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("decode: %v\n%s", err, body)
			}
			if len(got.Suites) != 1 || !got.Suites[0].Flat || got.Suites[0].Anchorless != enabled {
				t.Fatalf("adoption enabled %v: suites %+v, want the flat suite with anchorless %v", enabled, got.Suites, enabled)
			}
			hresp := mustGet(t, base+"/")
			html, _ := io.ReadAll(hresp.Body)
			_ = hresp.Body.Close()
			if strings.Contains(string(html), "refreshed without a Release") != enabled {
				t.Fatalf("adoption enabled %v: the HTML's \"refreshed without a Release\" shown %v", enabled, !enabled)
			}
		})
	}
}

func withCheckBackoff(f func(failCount int64) time.Duration) adminOpt {
	return func(cfg *Config) { cfg.CheckBackoff = f }
}

// Deferred minor B-63 through the server (Task 4 review M-1): the status
// page hands Config.CheckBackoff to annotateSuiteEntries, so a suite whose
// checks keep failing, last checked just now, shows when its backoff
// ends. TestSuiteEntries_FailingChecksSayWhenTheBackoffEnds calls
// annotateSuiteEntries directly; this checks buildStatusModel's half of
// the wiring (main's CheckBackoff: freshness.CheckBackoff is the other).
func TestStatus_FailingChecksBackoffThroughTheServer(t *testing.T) {
	s, base, cleanup := startAdminServer(t, withCheckBackoff(func(int64) time.Duration { return time.Hour }))
	defer cleanup()
	last := wallclock.Now().Unix()
	if err := s.cfg.Cache.SeedSuiteForTest(context.Background(), cache.SuiteFreshness{CanonicalScheme: "http",
		CanonicalHost: "h", SuitePath: "/r/dists/x", LastCheckAt: &last, CheckFailCount: 2}); err != nil {
		t.Fatal(err)
	}
	resp := mustGet(t, base+"/")
	html, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if want := "checks failing ×2 · backoff ends in "; !strings.Contains(string(html), want) {
		t.Fatalf("the suites table lacks %q (the count alone shown: %v)", want, strings.Contains(string(html), "checks failing ×2"))
	}
}
