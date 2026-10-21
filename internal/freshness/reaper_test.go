package freshness

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// reapNow is the reaper rig's wall clock (the checker's now).
const (
	reapNow = int64(1_800_000_000)
	reapDay = int64(86400)
)

// reaperRig is a real cache, an adopter whose flip gate is enabled (10 s
// quiet), a checker at the fixed wall clock reapNow, and a reaper with the
// default 30-day idle TTL, all logging JSON to logs. The cache's clock
// (wallclock) is pinned to reapNow too, running on from there, so no rig
// test depends on the real date (the Task 9 review's Important 2: the
// cache's stamps — M-9's marker stamp, a creation's — met reapNow's once
// the real date passed them). Outside a synctest bubble the pinned clock
// runs on in real time, so a test that compares a stamp the cache writes
// (a marker moved up by a reap or a forget, a retirement) exactly, or
// tightly, with reapNow runs inside a bubble, where the pinned clock
// moves only with the bubble's clock — a sleep or a timer — never with the
// machine's speed (the Task 9 re-reviews). No rig test may use
// t.Parallel.
type reaperRig struct {
	t       *testing.T
	cache   *cache.Cache
	adopter *Adopter
	gate    *flipgate.Gate
	checker *Checker
	reaper  *Reaper
	logs    *lockedBuffer
}

func newReaperRig(t *testing.T) *reaperRig {
	t.Helper()
	pinWallclock(t, reapNow)
	ad, rc, gate := gatedAdopter(t)
	logs := &lockedBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	chk, err := New(Config{Cache: rc, Fetcher: &simtest.StaticConditional{Status: 304}, HostLimiter: hostsem.New(8),
		Adopter: ad, Logger: logger, now: func() time.Time { return time.Unix(reapNow, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	rp, err := NewReaper(ReaperConfig{Cache: rc, Checker: chk, IdleTTL: 30 * 24 * time.Hour, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	return &reaperRig{t: t, cache: rc, adopter: ad, gate: gate, checker: chk, reaper: rp, logs: logs}
}

// pinWallclock sets the wall clock (wallclock.Now: the cache's clock, and
// the checker's in production) to the Unix time at for the rest of t, from
// where it runs on; t must not be parallel. The offset is measured against
// wallclock.Now itself, so a temporary overlay that shifts wallclock's base
// (to prove that no test depends on the real date) leaves the pin exact.
func pinWallclock(t *testing.T, at int64) {
	t.Helper()
	restore := wallclock.SetOffsetForTest(0)
	wallclock.SetOffsetForTest(time.Unix(at, 0).Sub(wallclock.Now()))
	t.Cleanup(restore)
}

func reapKey(suite string) cache.SuiteKey {
	return cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: suite}
}

// seed gives suite a row (through CreateSuite, so it has a touch entry),
// its client-interest stamp, a lock-map entry and a flip-gate entry whose
// serve the caller lets age, and a cached InRelease url_path row.
func (r *reaperRig) seed(suite string, requested int64) {
	r.t.Helper()
	ctx := context.Background()
	if err := r.cache.EnsureSuiteForTest(ctx, "http", "h", suite); err != nil {
		r.t.Fatal(err)
	}
	if err := r.cache.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h",
		SuitePath: suite, LastRequestedAt: &requested}); err != nil {
		r.t.Fatal(err)
	}
	release, ok := r.checker.HoldSuite(reapKey(suite))
	if !ok {
		r.t.Fatalf("%s: busy", suite)
	}
	release(false)
	r.gate.AnchorStart(ctx, flipgate.Key("http", "h", suite)).Done()
	h, err := r.adopter.writeBlobBytes(ctx, []byte("cached anchor of "+suite))
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.cache.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: suite + "/InRelease",
		BlobHash: &h, UpstreamURL: "http://h" + suite + "/InRelease", IsMetadata: true, RequestCount: 1}); err != nil {
		r.t.Fatal(err)
	}
}

// adopt gives suite a current snapshot whose one member is its anchor.
func (r *reaperRig) adopt(suite, anchor string) int64 {
	r.t.Helper()
	ctx := context.Background()
	h, err := r.adopter.writeBlobBytes(ctx, []byte(anchor))
	if err != nil {
		r.t.Fatal(err)
	}
	id, _, err := r.cache.InsertCandidateSnapshot(ctx, cache.SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h",
		SuitePath: suite, InReleaseHash: &h})
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.cache.CommitAdoption(ctx, id, []cache.SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: h,
		DeclaredSHA256: h}}, nil, nil, nil, false); err != nil {
		r.t.Fatal(err)
	}
	return id
}

func (r *reaperRig) exists(suite string) bool {
	r.t.Helper()
	ok, err := r.cache.HasSuiteRow(context.Background(), reapKey(suite))
	if err != nil {
		r.t.Fatal(err)
	}
	return ok
}

// events returns the rig's JSON log records whose msg is msg.
func (r *reaperRig) events(msg string) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(r.logs.String(), "\n") {
		var m map[string]any
		if line != "" && json.Unmarshal([]byte(line), &m) == nil && m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}

// reapedTotal returns acu_suites_reaped_total for reason (design review
// M-16) as metrics.Default renders it.
func reapedTotal(t *testing.T, reason string) float64 {
	t.Helper()
	var buf bytes.Buffer
	metrics.Default.Render(&buf)
	for _, line := range strings.Split(buf.String(), "\n") {
		if v, ok := strings.CutPrefix(line, `acu_suites_reaped_total{reason="`+reason+`"} `); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				t.Fatal(err)
			}
			return f
		}
	}
	return 0
}

// Spec C9: one run reaps the suites no client asked for in
// gc.suite_idle_ttl, and rows whose path the grammar cannot produce
// regardless of idleness; a canonical path with an odd codename is reaped
// by idleness only. A stamp in the future (a backward clock step) reads as
// now — never idle — and is reported. A reaped suite's snapshots are
// retired, its metadata url_path rows deleted, and its lock-map, flip-gate
// and touch entries purged (ruling R-F1); each reap logs suite_reaped and
// counts in acu_suites_reaped_total by reason (design review M-16). Flat
// suites (phase G, the F carry-forward) are canonical and reaped by
// idleness like any: /flat, used a day ago, stays; the idle /repo goes
// and its ./ twin, used, stays with its own rows (ruling R-G4).
func TestReaper_ReapsIdleAndNonCanonicalSuitesAndPurgesThem(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newReaperRig(t)
		reaped := map[string]int64{
			"/r/dists/idle":        reapNow - 31*reapDay,
			"/legacy//dists/x":     reapNow, // non-canonical: an empty segment
			"/foo/dists/InRelease": reapNow - 31*reapDay,
			"/repo":                reapNow - 31*reapDay,
		}
		kept := map[string]int64{
			"/r/dists/fresh":       reapNow - 29*reapDay,
			"/r/dists/future":      reapNow + 2*reapDay,
			"/bar/dists/InRelease": reapNow - reapDay,
			"/flat":                reapNow - reapDay,
			"/repo/.":              reapNow - 29*reapDay,
		}
		for s, at := range reaped {
			r.seed(s, at)
		}
		for s, at := range kept {
			r.seed(s, at)
		}
		snap := r.adopt("/r/dists/idle", "idle anchor")
		idle0, nonCanonical0 := reapedTotal(t, "idle"), reapedTotal(t, "non_canonical")
		// One wall clock for the checker and the cache, as in production:
		// the sleep below moves both, so the reap's marker stamp (M-9) is
		// no future stamp to the run's sweep.
		r.checker.now = wallclock.Now
		time.Sleep(r.gate.Quiet()) // the seeded serves are a quiet period old: their gate entries are idle
		r.reaper.runOnce(context.Background())
		for s := range reaped {
			if r.exists(s) {
				t.Errorf("%s was not reaped", s)
			}
			if _, err := r.cache.LookupURL(context.Background(), "http", "h", s+"/InRelease"); err == nil && s != "/legacy//dists/x" {
				t.Errorf("%s/InRelease was not deleted with its suite", s)
			}
		}
		for s := range kept {
			if !r.exists(s) {
				t.Errorf("%s was reaped", s)
			}
			if _, err := r.cache.LookupURL(context.Background(), "http", "h", s+"/InRelease"); err != nil {
				t.Errorf("%s/InRelease: %v; want it kept with its suite", s, err)
			}
		}
		if got, err := r.cache.GetSuiteSnapshot(context.Background(), snap); err != nil || got.RetiredAt == nil {
			t.Fatalf("the reaped suite's snapshot = %+v, %v; want retired", got, err)
		}
		if l, g, tm := r.checker.LockCount(), r.gate.Len(), r.cache.TouchedSuites(); l != len(kept) || g != len(kept) || tm != len(kept) {
			t.Fatalf("lock map %d, flip gate %d, touch map %d entries; want %d each (the kept suites')", l, g, tm, len(kept))
		}
		reasons := map[string]string{}
		for _, ev := range r.events("suite_reaped") {
			reasons[ev["suite_path"].(string)] = ev["reason"].(string)
		}
		want := map[string]string{"/r/dists/idle": "idle", "/legacy//dists/x": "non_canonical", "/foo/dists/InRelease": "idle",
			"/repo": "idle"}
		if fmt.Sprint(reasons) != fmt.Sprint(want) {
			t.Fatalf("suite_reaped reasons = %v, want %v", reasons, want)
		}
		if ev := r.events("clock_step_detected"); len(ev) != 1 || ev[0]["suite_path"] != "/r/dists/future" || ev[0]["stamp"] != "last_requested_at" {
			t.Fatalf("clock_step_detected = %v, want one for /r/dists/future's last_requested_at", ev)
		}
		if di, dn := reapedTotal(t, "idle")-idle0, reapedTotal(t, "non_canonical")-nonCanonical0; di != 3 || dn != 1 {
			t.Fatalf("acu_suites_reaped_total rose by idle %v, non_canonical %v; want 3 and 1", di, dn)
		}
	})
}

// Spec C9 and the C4 clock-step rule: candidates reads a NULL interest
// stamp as never idle and a future one as now (never idle). The order
// here — the non-canonical row first — is ruling PF-4's (rows reaped
// regardless of idleness first, then the oldest), though this data, whose
// non-canonical row is also the oldest, cannot tell it from oldest-first:
// TestReaperCandidates_ReapedRegardlessGoFirst does.
func TestReaperCandidates_NullAndFutureStampsAreNeverIdle(t *testing.T) {
	r := newReaperRig(t)
	at := func(v int64) *int64 { return &v }
	row := func(suite string, requested *int64) cache.SuiteWithAdoption {
		return cache.SuiteWithAdoption{SuiteFreshness: cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h",
			SuitePath: suite, LastRequestedAt: requested}}
	}
	cands := r.reaper.candidates([]cache.SuiteWithAdoption{
		row("/r/dists/null", nil),
		row("/r/dists/future", at(reapNow+reapDay)),
		row("/r/dists/old", at(reapNow-40*reapDay)),
		row("/legacy//dists/x", at(reapNow-50*reapDay)),
		row("/r/dists/edge", at(reapNow-30*reapDay)), // exactly the TTL: not older than it
	}, time.Unix(reapNow, 0))
	var got []string
	for _, c := range cands {
		got = append(got, c.reason+" "+c.key.SuitePath)
	}
	if want := []string{"non_canonical /legacy//dists/x", "idle /r/dists/old"}; !slices.Equal(got, want) {
		t.Fatalf("candidates = %q, want %q", got, want)
	}
}

// Ruling PF-4 (design review I-4): the candidates reaped regardless of
// idleness — non-canonical rows here, legacy junk too (Task 7) — sort
// before the idle ones whatever their stamps, so an idle backlog of
// reapsPerRun or more never holds them back; idle ones go oldest first.
func TestReaperCandidates_ReapedRegardlessGoFirst(t *testing.T) {
	r := newReaperRig(t)
	at := func(v int64) *int64 { return &v }
	row := func(suite string, requested *int64) cache.SuiteWithAdoption {
		return cache.SuiteWithAdoption{SuiteFreshness: cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h",
			SuitePath: suite, LastRequestedAt: requested}}
	}
	cands := r.reaper.candidates([]cache.SuiteWithAdoption{
		row("/r/dists/old", at(reapNow-40*reapDay)),
		row("/legacy//dists/x", at(reapNow)), // its clients still ask for it
		row("/r/dists/older", at(reapNow-50*reapDay)),
	}, time.Unix(reapNow, 0))
	var got []string
	for _, c := range cands {
		got = append(got, c.reason+" "+c.key.SuitePath)
	}
	if want := []string{"non_canonical /legacy//dists/x", "idle /r/dists/older", "idle /r/dists/old"}; !slices.Equal(got, want) {
		t.Fatalf("candidates = %q, want %q", got, want)
	}
}

// Design note 4: of several future interest stamps a run reports only the
// furthest — one clock_step_detected line, logged directly
// (clockStepUnlimited), not one per stamp.
func TestReaperCandidates_ReportsTheFurthestFutureStamp(t *testing.T) {
	r := newReaperRig(t)
	at := func(v int64) *int64 { return &v }
	row := func(suite string, requested *int64) cache.SuiteWithAdoption {
		return cache.SuiteWithAdoption{SuiteFreshness: cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h",
			SuitePath: suite, LastRequestedAt: requested}}
	}
	r.reaper.candidates([]cache.SuiteWithAdoption{
		row("/r/dists/near", at(reapNow+reapDay)),
		row("/r/dists/far", at(reapNow+3*reapDay)),
		row("/r/dists/mid", at(reapNow+2*reapDay)),
	}, time.Unix(reapNow, 0))
	if ev := r.events("clock_step_detected"); len(ev) != 1 || ev[0]["suite_path"] != "/r/dists/far" {
		t.Fatalf("clock_step_detected = %v, want one for /r/dists/far", ev)
	}
}

// Design review M-10: a dead suite's future stamp is reported at the
// first run after a start and again only when the furthest future stamp
// changes (or comes back after a run found none), not every hour until
// the clock passes it. Every report counts in
// acu_clock_steps_detected_total, so the count shows the reports.
func TestReaperCandidates_ReportsAFutureStampOnlyWhenItChanges(t *testing.T) {
	r := newReaperRig(t)
	rows := func(stamp int64) []cache.SuiteWithAdoption {
		return []cache.SuiteWithAdoption{{SuiteFreshness: cache.SuiteFreshness{CanonicalScheme: "http",
			CanonicalHost: "h", SuitePath: "/r/dists/dead", LastRequestedAt: &stamp}}}
	}
	reports := func() float64 {
		v, _ := strconv.ParseFloat(renderedMetric("acu_clock_steps_detected_total"), 64)
		return v
	}
	before := reports()
	for range 3 {
		r.reaper.candidates(rows(reapNow+5*365*reapDay), time.Unix(reapNow, 0))
	}
	if got := reports() - before; got != 1 {
		t.Fatalf("three runs over one future stamp: %v reports, want 1", got)
	}
	r.reaper.candidates(rows(reapNow+6*365*reapDay), time.Unix(reapNow, 0))
	if got := reports() - before; got != 2 {
		t.Fatalf("after the furthest stamp changed: %v reports, want 2", got)
	}
	r.reaper.candidates(nil, time.Unix(reapNow, 0))
	r.reaper.candidates(rows(reapNow+6*365*reapDay), time.Unix(reapNow, 0))
	if got := reports() - before; got != 3 {
		t.Fatalf("after a run without it: %v reports, want 3", got)
	}
}

// Spec C9: a run tries at most 25 idle suites (reapsPerRun; since ruling
// PF-14 the suites reaped regardless of idleness have a budget of their
// own, TestReaper_EachClassHasItsOwnBudget), oldest last_requested_at
// first; the next run takes the rest.
func TestReaper_AtMost25PerRunOldestFirst(t *testing.T) {
	r := newReaperRig(t)
	for i := range 30 {
		r.seed(fmt.Sprintf("/r/dists/s%02d", i), reapNow-31*reapDay-int64(30-i)*3600) // s00 oldest
	}
	r.reaper.runOnce(context.Background())
	for i := range 30 {
		if got, want := r.exists(fmt.Sprintf("/r/dists/s%02d", i)), i >= 25; got != want {
			t.Fatalf("after one run: s%02d exists %v, want %v (the 25 oldest reaped)", i, got, want)
		}
	}
	r.reaper.runOnce(context.Background())
	if n := len(r.events("suite_reaped")); n != 30 {
		t.Fatalf("after two runs: %d reaped, want all 30", n)
	}
}

// Ruling PF-14 (final review concurrency m2): the candidates reaped
// regardless of idleness and the idle ones have budgets of their own. With
// 251 non-canonical rows (legacy rows whose clients still ask for them)
// and 26 idle suites, one run tries exactly regardlessReapsPerRun (250) of
// the first, in key order, and reapsPerRun (25) of the second, the
// oldest; the next run takes the one left of each. The first class is
// tried first (the final fix wave's re-review, reaper minor 2): every
// non_canonical suite_reaped line comes before every idle one. A busy
// candidate counts against its own class's budget: with the first
// non-canonical row held, the run tries 249 others of its class, and
// still 25 idle ones.
func TestReaper_EachClassHasItsOwnBudget(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(fmt.Sprintf("busy %v", busy), func(t *testing.T) {
			r := newReaperRig(t)
			ctx := context.Background()
			row := func(suite string, requested int64) {
				t.Helper()
				if err := r.cache.EnsureSuiteForTest(ctx, "http", "h", suite); err != nil {
					t.Fatal(err)
				}
				if err := r.cache.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h",
					SuitePath: suite, LastRequestedAt: &requested}); err != nil {
					t.Fatal(err)
				}
			}
			legacy := func(i int) string { return fmt.Sprintf("/legacy//dists/x%03d", i) }
			idle := func(i int) string { return fmt.Sprintf("/r/dists/s%02d", i) }
			for i := range 251 {
				row(legacy(i), reapNow)
			}
			for i := range 26 {
				row(idle(i), reapNow-31*reapDay-int64(26-i)) // s00 oldest
			}
			if busy {
				release, ok := r.checker.HoldSuite(reapKey(legacy(0)))
				if !ok {
					t.Fatal("premise: the first legacy row is free")
				}
				defer release(false)
			}
			r.reaper.runOnce(ctx)
			reasons := map[any]int{}
			for _, ev := range r.events("suite_reaped") {
				reasons[ev["reason"]]++
			}
			wantRegardless := 250 // the literals pin the constants (a mutant of one must fail)
			if busy {
				wantRegardless-- // the held row was tried: its class's budget paid for it
			}
			if reasons["non_canonical"] != wantRegardless || reasons["idle"] != 25 {
				t.Fatalf("one run reaped %v; want %d non_canonical and 25 idle", reasons, wantRegardless)
			}
			var order []string
			for _, ev := range r.events("suite_reaped") {
				order = append(order, fmt.Sprint(ev["reason"]))
			}
			want := append(slices.Repeat([]string{"non_canonical"}, wantRegardless), slices.Repeat([]string{"idle"}, 25)...)
			if !slices.Equal(order, want) {
				t.Fatalf("suite_reaped lines in log order: the first idle one at %d of %d; want the %d non_canonical first",
					slices.Index(order, "idle"), len(order), wantRegardless)
			}
			if !r.exists(legacy(250)) || !r.exists(idle(25)) {
				t.Fatalf("the last of each class reaped by the first run (legacy %v, idle %v); want it left for the next",
					!r.exists(legacy(250)), !r.exists(idle(25)))
			}
			if busy != r.exists(legacy(0)) {
				t.Fatalf("the held legacy row exists %v; want it kept only while held", r.exists(legacy(0)))
			}
			if !busy {
				r.reaper.runOnce(ctx)
				if r.exists(legacy(250)) || r.exists(idle(25)) {
					t.Fatal("the next run left the last of a class")
				}
			}
		})
	}
}

// Spec C9: a suite whose mutex is held (a check, an adoption) is skipped
// and reaped by the next run once free; a suite asked for, or adopted,
// between the listing and the hold keeps its metadata rows.
func TestReaper_SkipsABusySuiteAndRetriesNextRun(t *testing.T) {
	r := newReaperRig(t)
	r.seed("/r/dists/busy", reapNow-31*reapDay)
	release, ok := r.checker.HoldSuite(reapKey("/r/dists/busy"))
	if !ok {
		t.Fatal("hold")
	}
	r.reaper.runOnce(context.Background())
	if !r.exists("/r/dists/busy") {
		t.Fatal("a busy suite was reaped")
	}
	// The busy line names the scheme too (the Task 6 review's minor 4:
	// both schemes' copies of a suite can be tracked).
	if ev := r.events("suite reaper: suite busy, retried next run"); len(ev) != 1 || ev[0]["canonical_scheme"] != "http" ||
		ev[0]["suite_path"] != "/r/dists/busy" {
		t.Fatalf("busy lines = %v, want one naming http and /r/dists/busy", ev)
	}
	release(false)
	r.reaper.runOnce(context.Background())
	if r.exists("/r/dists/busy") {
		t.Fatal("the suite was not reaped once free")
	}
	// Asked for after the listing: the re-read under the hold keeps it whole.
	r.seed("/r/dists/back", reapNow-31*reapDay)
	r.reaper.reapHook = func(context.Context, cache.SuiteKey) {
		now := reapNow
		if err := r.cache.SeedSuiteForTest(context.Background(), cache.SuiteFreshness{CanonicalScheme: "http",
			CanonicalHost: "h", SuitePath: "/r/dists/back", LastRequestedAt: &now}); err != nil {
			t.Error(err)
		}
	}
	r.reaper.runOnce(context.Background())
	if !r.exists("/r/dists/back") {
		t.Fatal("a suite asked for during the run was reaped")
	}
	if _, err := r.cache.LookupURL(context.Background(), "http", "h", "/r/dists/back/InRelease"); err != nil {
		t.Fatalf("a suite asked for during the run lost its anchor row: %v", err)
	}
}

// Design note 5 (I-4's re-validation under the hold): a suite adopted
// between the run's listing and its hold is left whole — its row, its new
// current snapshot and its metadata url_path rows. Without the re-read's
// snapshot comparison the reaper deletes the anchors of a suite that the
// final compare-and-delete then keeps.
func TestReaper_KeepsTheMetadataOfASuiteAdoptedSinceTheListing(t *testing.T) {
	ctx := context.Background()
	r := newReaperRig(t)
	r.seed("/r/dists/x", reapNow-31*reapDay)
	var adopted int64
	r.reaper.reapHook = func(context.Context, cache.SuiteKey) {
		r.reaper.reapHook = nil
		adopted = r.adopt("/r/dists/x", "adopted since the listing")
	}
	r.reaper.runOnce(ctx)
	sf, err := r.cache.GetSuiteFreshness(ctx, "http", "h", "/r/dists/x")
	if err != nil || sf.CurrentSnapshotID == nil || *sf.CurrentSnapshotID != adopted {
		t.Fatalf("row %+v, %v; want it kept with the snapshot adopted since the listing (%d)", sf, err, adopted)
	}
	if _, err := r.cache.LookupURL(ctx, "http", "h", "/r/dists/x/InRelease"); err != nil {
		t.Fatalf("the suite adopted since the listing lost its anchor row: %v", err)
	}
}

// Spec C9 step 2: the final step re-checks the interest stamp in its own
// transaction — a client that asks for the suite while its metadata rows
// are being deleted keeps it (the rows it lost come back on its misses). A
// trigger stands in for the client's touch, landing in the metadata batch
// that deletes the suite's InRelease row.
func TestReaper_AClientAskingDuringTheMetadataBatchesKeepsTheSuite(t *testing.T) {
	ctx := context.Background()
	r := newReaperRig(t)
	r.seed("/r/dists/x", reapNow-31*reapDay)
	db, err := sql.Open("sqlite", filepath.Join(r.cache.Dir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(fmt.Sprintf(`CREATE TRIGGER touch_during_reap AFTER DELETE ON url_path
WHEN OLD.path = '/r/dists/x/InRelease'
BEGIN UPDATE suite_freshness SET last_requested_at = %d WHERE suite_path = '/r/dists/x'; END`, reapNow)); err != nil {
		t.Fatal(err)
	}
	r.reaper.runOnce(ctx)
	if !r.exists("/r/dists/x") || len(r.events("suite_reaped")) != 0 {
		t.Fatalf("row %v, %d suite_reaped lines; want the suite asked for during its metadata deletion kept",
			r.exists("/r/dists/x"), len(r.events("suite_reaped")))
	}
}

// Design note 5: a suite whose row vanished between the run's listing and
// its hold (cap eviction, POST /forget) has the lock-map entry the hold
// stored dropped (release(true)), so the map stays bounded by the table.
func TestReaper_DropsTheLockEntryOfASuiteGoneSinceTheListing(t *testing.T) {
	ctx := context.Background()
	r := newReaperRig(t)
	r.seed("/r/dists/x", reapNow-31*reapDay) // leaves a lock-map entry
	r.reaper.reapHook = func(_ context.Context, k cache.SuiteKey) {
		r.reaper.reapHook = nil
		if res, err := r.cache.ForgetSuite(ctx, k, nil); err != nil || !res.Deleted { // gone under another deleter
			t.Errorf("ForgetSuite = %+v, %v", res, err)
		}
	}
	r.reaper.runOnce(ctx)
	if r.exists("/r/dists/x") || r.checker.LockCount() != 0 {
		t.Fatalf("row %v, %d lock entries; want the vanished suite's entry dropped", r.exists("/r/dists/x"), r.checker.LockCount())
	}
	if n := len(r.events("suite_reaped")); n != 0 {
		t.Fatalf("%d suite_reaped lines for a suite the reaper did not delete", n)
	}
}

// Design note 5: a suite the run keeps — asked for since the listing — is
// released as kept (release(false)): its lock-map entry, and with it the
// checker's per-suite state, stays.
func TestReaper_AKeptSuiteKeepsItsLockEntry(t *testing.T) {
	ctx := context.Background()
	r := newReaperRig(t)
	r.seed("/r/dists/back", reapNow-31*reapDay) // leaves a lock-map entry
	key := suiteKey("http", "h", "/r/dists/back")
	r.checker.absentAnswered.Store(key, struct{}{})
	r.reaper.reapHook = func(context.Context, cache.SuiteKey) {
		r.reaper.reapHook = nil
		now := reapNow
		if err := r.cache.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "http",
			CanonicalHost: "h", SuitePath: "/r/dists/back", LastRequestedAt: &now}); err != nil {
			t.Error(err)
		}
	}
	r.reaper.runOnce(ctx)
	if !r.exists("/r/dists/back") || r.checker.LockCount() != 1 {
		t.Fatalf("row %v, %d lock entries; want the kept suite and its entry", r.exists("/r/dists/back"), r.checker.LockCount())
	}
	if _, ok := r.checker.absentAnswered.Load(key); !ok {
		t.Fatal("the kept suite's per-suite state was dropped")
	}
}

// Design review M-2: the work under one suite's hold runs within
// suiteHoldTimeout — 5 min, CommitAdoption's bound — so a wedged writer
// releases the suite instead of holding it until shutdown.
func TestReaper_BoundsTheWorkUnderAHoldAtFiveMinutes(t *testing.T) {
	r := newReaperRig(t)
	r.seed("/r/dists/a", reapNow-31*reapDay)
	var deadline, at time.Time
	var ok bool
	r.reaper.reapHook = func(ctx context.Context, _ cache.SuiteKey) { deadline, ok = ctx.Deadline(); at = time.Now() }
	start := time.Now()
	r.reaper.runOnce(context.Background())
	if !ok || deadline.Before(start.Add(5*time.Minute)) || deadline.After(at.Add(5*time.Minute)) {
		t.Fatalf("the held steps' deadline = %v (set %v), want 5 min after the hold", deadline, ok)
	}
}

// Design review M-2 and M-3 (the Task 6 review's minor 1): the held steps
// run on the bounded context, not the run's — a hold that outlives
// suiteHoldTimeout cuts the re-read short — and a step cut short by the
// bound is a failure logged at WARN (with the suite's scheme), never the
// DEBUG a shutdown gets. The suite is kept, its hold given back, and the
// next run reaps it.
func TestReaper_AHoldTimeoutIsAWarnedFailureAndTheNextRunReaps(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newReaperRig(t)
		r.seed("/r/dists/a", reapNow-31*reapDay)
		slow := true
		r.reaper.reapHook = func(context.Context, cache.SuiteKey) {
			if slow {
				slow = false
				time.Sleep(suiteHoldTimeout + time.Second) // a wedged step: the bound passes
			}
		}
		r.reaper.runOnce(context.Background())
		if !r.exists("/r/dists/a") {
			t.Fatal("a suite whose hold timed out was reaped: the held steps did not run on the bounded context")
		}
		ev := r.events("suite reaper: read suite row failed")
		if len(ev) != 1 || ev[0]["level"] != "WARN" || ev[0]["canonical_scheme"] != "http" || ev[0]["suite_path"] != "/r/dists/a" {
			t.Fatalf("read failures = %v, want one at WARN naming http and /r/dists/a", ev)
		}
		r.reaper.runOnce(context.Background())
		if r.exists("/r/dists/a") {
			t.Fatal("the next run did not reap the suite: the timed-out hold was not given back")
		}
	})
}

// Design review M-8 (the Task 6 review's minor 1): a forward step is the
// wall clock running more than forwardStepSlack — an hour — ahead of the
// monotonic clock between two runs. The whole-second truncation of the
// wall clock (±1 s) and a lead of up to the hour itself are no step.
func TestReaperForwardStep_TheSlackIsAnHour(t *testing.T) {
	r := newReaperRig(t)
	wall, mono := time.Unix(reapNow, 0), time.Now()
	for _, tc := range []struct {
		ahead    time.Duration
		deferred bool
	}{{time.Second, false}, {59 * time.Minute, false}, {time.Hour, false}, {time.Hour + time.Second, true}} {
		r.reaper.lastWall, r.reaper.lastMono, r.reaper.deferUntil = wall, mono, time.Time{}
		if got := r.reaper.deferredByForwardStep(wall.Add(time.Hour+tc.ahead), mono.Add(time.Hour)); got != tc.deferred {
			t.Errorf("the wall clock %v ahead after an hour: deferred %v, want %v", tc.ahead, got, tc.deferred)
		}
	}
}

// The Task 6 review's minor 2 (controller ruling): a panic while one
// candidate is held is recovered per candidate, as checkRecovered recovers
// a check's: it is logged (freshness_check_panicked, where=reaper) with
// that suite — its scheme too (the Task 6 re-review's out-of-scope 1: an
// http copy and an https copy of a suite are told apart) — the hold is
// given back, and the run goes on: the next candidate is reaped, and the
// memo and marker sweeps still run.
func TestReaper_APanicSkipsOnlyItsCandidate(t *testing.T) {
	r := newReaperRig(t)
	r.seed("/r/dists/a", reapNow-32*reapDay) // the oldest: tried first
	r.seed("/r/dists/b", reapNow-31*reapDay)
	r.reaper.reapHook = func(_ context.Context, k cache.SuiteKey) {
		if k.SuitePath == "/r/dists/a" {
			panic("reaper bug")
		}
	}
	r.adopter.reconciledSnapshots.Store(int64(1_000_000), struct{}{}) // no suite's current snapshot
	r.mark("/gone/dists/s", reapNow-366*reapDay)                      // a year-old orphan
	r.reaper.safeRun(context.Background())
	if !r.exists("/r/dists/a") || r.exists("/r/dists/b") {
		t.Fatalf("a exists %v, b exists %v; want the panicking candidate kept and the next one reaped",
			r.exists("/r/dists/a"), r.exists("/r/dists/b"))
	}
	if ev := r.events("freshness_check_panicked"); len(ev) != 1 || ev[0]["where"] != "reaper" ||
		ev[0]["canonical_scheme"] != "http" || ev[0]["canonical_host"] != "h" || ev[0]["suite_path"] != "/r/dists/a" {
		t.Fatalf("panic lines = %v, want one with where=reaper naming http://h/r/dists/a", ev)
	}
	if _, ok := r.adopter.reconciledSnapshots.Load(int64(1_000_000)); ok {
		t.Fatal("the panic stopped the run's reconcile memo sweep")
	}
	if r.markerAt("/gone/dists/s") != -1 {
		t.Fatal("the panic stopped the run's marker sweep")
	}
	release, ok := r.checker.HoldSuite(reapKey("/r/dists/a"))
	if !ok {
		t.Fatal("the panicking candidate's hold was not given back")
	}
	release(false)
}

// panicOnMsg is a slog handler that panics on a record whose message is
// msg and passes every other record to its inner handler.
type panicOnMsg struct {
	slog.Handler
	msg string
}

func (h panicOnMsg) Handle(ctx context.Context, rec slog.Record) error {
	if rec.Message == h.msg {
		panic("log handler bug")
	}
	return h.Handler.Handle(ctx, rec)
}

// The Task 6 review's minor 2 (controller ruling): a panic that strikes
// after the suite's row is gone releases the hold as gone — the cache's
// fresh answer — so the deleted suite's lock-map entry does not leak.
func TestReaper_APanicAfterTheDeleteReleasesTheSuiteAsGone(t *testing.T) {
	r := newReaperRig(t)
	r.seed("/r/dists/a", reapNow-31*reapDay)
	r.reaper.logger = slog.New(panicOnMsg{Handler: r.reaper.logger.Handler(), msg: "suite_reaped"})
	r.reaper.safeRun(context.Background())
	if r.exists("/r/dists/a") {
		t.Fatal("premise: the suite was deleted before the panic")
	}
	if ev := r.events("freshness_check_panicked"); len(ev) != 1 || ev[0]["suite_path"] != "/r/dists/a" {
		t.Fatalf("panic lines = %v, want one naming /r/dists/a", ev)
	}
	if n := r.checker.LockCount(); n != 0 {
		t.Fatalf("%d lock-map entries after the panic; want the deleted suite's dropped (release(true))", n)
	}
}

// The Task 6 re-review's New Breakage 1: the release after a panic takes
// the cache's fresh answer, not what reapSuite last saw. Here the row is
// deleted (a forget through the cache, standing in for any commit) and the
// panic strikes before reapSuite learns it: the hold is released as gone,
// so the deleted suite's lock-map entry and per-suite state are dropped.
func TestReaper_APanicAfterTheRowWentReleasesTheSuiteAsGone(t *testing.T) {
	r := newReaperRig(t)
	r.seed("/r/dists/a", reapNow-31*reapDay) // leaves a lock-map entry
	key := suiteKey("http", "h", "/r/dists/a")
	r.checker.absentAnswered.Store(key, struct{}{})
	r.reaper.reapHook = func(_ context.Context, k cache.SuiteKey) {
		r.reaper.reapHook = nil
		if res, err := r.cache.ForgetSuite(context.Background(), k, nil); err != nil || !res.Deleted {
			t.Errorf("ForgetSuite = %+v, %v; want the row deleted", res, err)
		}
		panic("after the row is gone")
	}
	r.reaper.safeRun(context.Background())
	if r.exists("/r/dists/a") {
		t.Fatal("premise: the row is gone before the panic")
	}
	if ev := r.events("freshness_check_panicked"); len(ev) != 1 || ev[0]["suite_path"] != "/r/dists/a" {
		t.Fatalf("panic lines = %v, want one naming /r/dists/a", ev)
	}
	if n := r.checker.LockCount(); n != 0 {
		t.Fatalf("%d lock-map entries after the panic; want the gone suite's dropped (release(true) from the fresh read)", n)
	}
	if _, ok := r.checker.absentAnswered.Load(key); ok {
		t.Fatal("the gone suite's per-suite state was kept")
	}
}

// The other direction (design note 23: "a kept one's is never dropped"):
// a panic while the row is present releases the hold as kept
// (release(false)), so the suite's lock-map entry and per-suite state stay.
func TestReaper_APanicWhileTheRowIsPresentKeepsTheSuitesState(t *testing.T) {
	r := newReaperRig(t)
	r.seed("/r/dists/a", reapNow-31*reapDay) // leaves a lock-map entry
	key := suiteKey("http", "h", "/r/dists/a")
	r.checker.absentAnswered.Store(key, struct{}{})
	r.reaper.reapHook = func(context.Context, cache.SuiteKey) { panic("before any step") }
	r.reaper.safeRun(context.Background())
	if !r.exists("/r/dists/a") {
		t.Fatal("premise: the row is present")
	}
	if ev := r.events("freshness_check_panicked"); len(ev) != 1 || ev[0]["suite_path"] != "/r/dists/a" {
		t.Fatalf("panic lines = %v, want one naming /r/dists/a", ev)
	}
	if n := r.checker.LockCount(); n != 1 {
		t.Fatalf("%d lock-map entries after the panic; want the present suite's kept (release(false))", n)
	}
	if _, ok := r.checker.absentAnswered.Load(key); !ok {
		t.Fatal("the present suite's per-suite state was dropped")
	}
}

// The Task 6 review's minor 3 (controller ruling): a reap whose submit
// failed after its transaction committed — the cache settles it gone by a
// fresh read (design review M-2) — is a reap: suite_reaped with
// settled=true, counted in acu_suites_reaped_total, and no failure line.
// The run's context ends right after the commit (a shutdown), before the
// result is handed back.
func TestReaper_ASettledReapIsAReap(t *testing.T) {
	r := newReaperRig(t)
	r.seed("/r/dists/a", reapNow-31*reapDay)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resume := make(chan struct{})
	var once sync.Once
	r.cache.SetCommittedHookForTest(func() { once.Do(func() { cancel(); <-resume }) })
	idle0 := reapedTotal(t, "idle")
	r.reaper.runOnce(ctx)
	close(resume)
	if r.exists("/r/dists/a") {
		t.Fatal("premise: the delete committed")
	}
	ev := r.events("suite_reaped")
	if len(ev) != 1 || ev[0]["settled"] != true || ev[0]["reason"] != "idle" || ev[0]["suite_path"] != "/r/dists/a" {
		t.Fatalf("suite_reaped lines = %v, want one for /r/dists/a with settled=true", ev)
	}
	if fails := r.events("suite reaper: reap failed"); len(fails) != 0 {
		t.Fatalf("reap failures = %v, want none: the reap committed", fails)
	}
	if d := reapedTotal(t, "idle") - idle0; d != 1 {
		t.Fatalf("acu_suites_reaped_total{reason=idle} rose by %v, want 1", d)
	}
	if n := r.checker.LockCount(); n != 0 {
		t.Fatalf("%d lock-map entries; want the settled suite's dropped", n)
	}
}

// Design review M-3: a run cut short by shutdown — the daemon's context
// ends while a suite is held — logs the failed step at DEBUG, not WARN,
// and keeps the suite.
func TestReaper_AShutdownMidRunIsNoFailure(t *testing.T) {
	r := newReaperRig(t)
	r.seed("/r/dists/a", reapNow-31*reapDay)
	ctx, cancel := context.WithCancel(context.Background())
	r.reaper.reapHook = func(context.Context, cache.SuiteKey) { cancel() }
	r.reaper.runOnce(ctx)
	if !r.exists("/r/dists/a") {
		t.Fatal("the cancelled run reaped the suite")
	}
	if ev := r.events("suite reaper: read suite row failed"); len(ev) != 1 || ev[0]["level"] != "DEBUG" {
		t.Fatalf("read failures = %v, want one at DEBUG", ev)
	}
}

// Spec C9: the first run waits out the monotonic uptime floor (1 h), then
// one runs every hour.
func TestReaper_RunsAfterTheUptimeFloorThenHourly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newReaperRig(t)
		r.seed("/r/dists/a", reapNow-31*reapDay)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { r.reaper.Run(ctx); close(done) }()
		defer func() { cancel(); <-done }()
		time.Sleep(time.Hour - time.Second)
		if !r.exists("/r/dists/a") {
			t.Fatal("reaped before the uptime floor")
		}
		time.Sleep(2 * time.Second)
		if r.exists("/r/dists/a") {
			t.Fatal("not reaped by the first run, at the floor")
		}
		r.seed("/r/dists/b", reapNow-31*reapDay)
		time.Sleep(time.Hour - 2*time.Second)
		if !r.exists("/r/dists/b") {
			t.Fatal("reaped before the next hourly run")
		}
		time.Sleep(2 * time.Second)
		if r.exists("/r/dists/b") {
			t.Fatal("not reaped by the next hourly run")
		}
	})
}

// Design note 2: gc.suite_idle_ttl = 0s reaps nothing — neither an idle
// suite nor one reaped regardless of idleness (split from the cadence
// test: the Task 6 review's minor 6).
func TestReaper_AnIdleTTLOfZeroReapsNothing(t *testing.T) {
	r := newReaperRig(t)
	r.reaper.idleTTL = 0
	r.seed("/r/dists/a", reapNow-365*reapDay)
	r.seed("/legacy//dists/x", reapNow)
	r.reaper.runOnce(context.Background())
	if !r.exists("/r/dists/a") || !r.exists("/legacy//dists/x") {
		t.Fatal("gc.suite_idle_ttl = 0s reaped a suite")
	}
}

// Design review M-8: a forward step of the wall clock that no restart
// re-arms (a VM resumed after a long pause, a laptop after a long
// suspend) makes suites look idle at once. The reaper sees the wall clock
// run ahead of the monotonic clock between two runs, reports it
// (clock_step_detected, stamp=wall_clock) and reaps nothing until an
// uptime floor later, giving clients the hour a restart gives them.
func TestReaper_AForwardStepDefersReapingByTheFloor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newReaperRig(t)
		var step atomic.Int64
		start := time.Now()
		r.checker.now = func() time.Time { return time.Unix(reapNow+step.Load(), 0).Add(time.Since(start)) }
		r.seed("/r/dists/a", reapNow-reapDay) // asked for yesterday
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { r.reaper.Run(ctx); close(done) }()
		defer func() { cancel(); <-done }()
		time.Sleep(time.Hour + time.Second) // the first run, at the floor: nothing idle
		if !r.exists("/r/dists/a") {
			t.Fatal("reaped before the step")
		}
		step.Store(40 * reapDay) // the wall clock jumps 40 days while the process runs
		time.Sleep(time.Hour)
		if !r.exists("/r/dists/a") {
			t.Fatal("reaped by the run that saw the forward step")
		}
		if ev := r.events("clock_step_detected"); len(ev) != 1 || ev[0]["stamp"] != "wall_clock" {
			t.Fatalf("clock_step_detected = %v, want one with stamp=wall_clock", ev)
		}
		time.Sleep(time.Hour)
		if r.exists("/r/dists/a") {
			t.Fatal("not reaped an uptime floor after the step")
		}
	})
}

// Design review M-8: only the wall clock running ahead of the monotonic
// clock is a step — two runs further apart than forwardStepSlack (a slow
// run, a long interval) are not.
func TestReaper_RunsFarApartAreNoForwardStep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newReaperRig(t)
		start := time.Now()
		r.checker.now = func() time.Time { return time.Unix(reapNow, 0).Add(time.Since(start)) }
		r.reaper.interval = 3 * time.Hour
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { r.reaper.Run(ctx); close(done) }()
		defer func() { cancel(); <-done }()
		time.Sleep(time.Hour + time.Second) // the first run
		r.seed("/r/dists/a", reapNow-31*reapDay)
		time.Sleep(3 * time.Hour) // the next run, three hours later
		if r.exists("/r/dists/a") {
			t.Fatal("a run three hours after the previous one took the interval for a forward step")
		}
		if ev := r.events("clock_step_detected"); len(ev) != 0 {
			t.Fatalf("clock_step_detected = %v, want none", ev)
		}
	})
}

// Spec C9: each run drops the reconcile memo's entries of snapshots that
// are no suite's current one.
func TestReaper_SweepsTheReconcileMemo(t *testing.T) {
	r := newReaperRig(t)
	r.seed("/r/dists/a", reapNow)
	cur := r.adopt("/r/dists/a", "a anchor")
	r.adopter.reconciledSnapshots.Store(cur, struct{}{})
	r.adopter.reconciledSnapshots.Store(cur+100, struct{}{}) // a displaced (or reaped) snapshot
	r.reaper.runOnce(context.Background())
	var left []int64
	r.adopter.reconciledSnapshots.Range(func(k, _ any) bool { left = append(left, k.(int64)); return true })
	if !slices.Equal(left, []int64{cur}) {
		t.Fatalf("memo = %v, want only the current snapshot %d", left, cur)
	}
}

// Design note 2: gc.suite_idle_ttl = 0s keeps the reconcile memo sweep.
func TestReaper_SweepsTheReconcileMemoWithReapingDisabled(t *testing.T) {
	r := newReaperRig(t)
	r.reaper.idleTTL = 0
	r.seed("/r/dists/a", reapNow)
	cur := r.adopt("/r/dists/a", "a anchor")
	r.adopter.reconciledSnapshots.Store(cur, struct{}{})
	r.adopter.reconciledSnapshots.Store(cur+100, struct{}{})
	r.reaper.runOnce(context.Background())
	if _, ok := r.adopter.reconciledSnapshots.Load(cur + 100); ok {
		t.Fatal("gc.suite_idle_ttl = 0s stopped the reconcile memo sweep")
	}
	if _, ok := r.adopter.reconciledSnapshots.Load(cur); !ok {
		t.Fatal("the current snapshot's memo entry was dropped")
	}
}

// The reaper's goroutine recovers panics (I-1 for internal/freshness): a
// panic while a suite is held releases the hold, is logged
// (freshness_check_panicked, where=reaper), and the next run still comes.
func TestReaper_RecoversAPanicAndReleasesTheHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newReaperRig(t)
		r.reaper.floor, r.reaper.interval = time.Minute, time.Minute
		r.seed("/r/dists/a", reapNow-31*reapDay)
		panics := 1
		r.reaper.reapHook = func(context.Context, cache.SuiteKey) {
			if panics > 0 {
				panics--
				panic("reaper bug")
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { r.reaper.Run(ctx); close(done) }()
		defer func() { cancel(); <-done }()
		time.Sleep(time.Minute + time.Second)
		if ev := r.events("freshness_check_panicked"); len(ev) != 1 || ev[0]["where"] != "reaper" {
			t.Fatalf("panic lines = %v, want one with where=reaper", ev)
		}
		if !r.exists("/r/dists/a") {
			t.Fatal("the panicking run reaped the suite")
		}
		time.Sleep(time.Minute)
		if r.exists("/r/dists/a") {
			t.Fatal("the next run did not reap the suite: the panic left it held")
		}
	})
}

// Spec C9 / phase B carry-forward: a suite reaped while it waits in the
// scheduler for an adoption slot is flushed by its next offer — no row,
// so the check is skipped and the entry done — and leaves no lock-map
// entry; acu_freshness_waiting_for_slot counts it only until then.
func TestReaper_AReapedWaiterFlushesOnItsNextOffer(t *testing.T) {
	r := newReaperRig(t)
	r.checker.refresh = 15 * time.Minute
	r.seed("/r/dists/w", reapNow-31*reapDay)
	r.adopt("/r/dists/w", "w anchor") // adopted, never checked: due at once
	ctx := context.Background()
	q := newSchedQueues()
	r.checker.tick(ctx, q)
	it, ok := q.pop(admitNone)
	if !ok {
		t.Fatal("nothing queued")
	}
	q.finish(checkDone{item: it, report: checkReport{result: "changed_queued", signedCurrent: true}}, 0) // waits for a slot
	r.reaper.runOnce(ctx)
	if r.exists("/r/dists/w") || q.waiting() != 1 {
		t.Fatalf("premise: reaped %v, waiting %d; want the reaped suite still parked", !r.exists("/r/dists/w"), q.waiting())
	}
	it, ok = q.pop(admitAll)
	if !ok || !it.waiting {
		t.Fatalf("offer = %+v, %v; want the waiter", it, ok)
	}
	rep := r.checker.checkRecovered(ctx, it)
	q.finish(checkDone{item: it, report: rep}, 0)
	if rep.result != resultSkipped || q.len() != 0 || q.waiting() != 0 || r.checker.LockCount() != 0 {
		t.Fatalf("after the offer: result %q, queued %d, waiting %d, locks %d; want skipped, 0, 0, 0",
			rep.result, q.len(), q.waiting(), r.checker.LockCount())
	}
}
