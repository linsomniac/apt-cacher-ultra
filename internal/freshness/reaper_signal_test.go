package freshness

import (
	"context"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// gaugeValue returns an unlabelled metric's value as metrics.Default
// renders it, failing the test when the series is missing or unparsable.
func gaugeValue(t *testing.T, name string) float64 {
	t.Helper()
	v, err := strconv.ParseFloat(renderedMetric(name), 64)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return v
}

// Deferred minor F-13 (design note 7): a completed reaper run logs
// suite_reaper_run_complete — the candidates it tried, the suites it
// reaped, the markers its daily sweep swept, whether the forward-step
// guard held it back — and sets acu_suite_reaper_last_run_unixtime; a run
// whose listing failed does neither. Deferred minor F-17 (design note 10):
// the forward step that holds a run back is counted in
// acu_clock_steps_detected_total like every clock-step report.
func TestReaper_ACompletedRunLogsItsLineAndSetsTheGauge(t *testing.T) {
	r := newReaperRig(t)
	ctx := context.Background()
	r.seed("/r/dists/old", reapNow-32*reapDay)
	r.seed("/r/dists/fresh", reapNow)
	r.mark("/gone/dists/s", reapNow-366*reapDay) // a year-old orphan marker
	reaperLastRunUnixtime.Set(1)
	r.reaper.runOnce(ctx) // outside Run the daily sweep is due
	runs := r.events("suite_reaper_run_complete")
	if len(runs) != 1 || runs[0]["level"] != "INFO" || runs[0]["tried"] != float64(1) || runs[0]["reaped"] != float64(1) ||
		runs[0]["markers_swept"] != float64(1) || runs[0]["deferred"] != false || runs[0]["duration_ms"] == nil {
		t.Fatalf("run lines %v, want one: tried 1, reaped 1, markers_swept 1, not deferred", runs)
	}
	if got := gaugeValue(t, "acu_suite_reaper_last_run_unixtime"); got != float64(reapNow) {
		t.Fatalf("last run gauge %v, want %d", got, reapNow)
	}

	reaperLastRunUnixtime.Set(1)
	ended, cancel := context.WithCancel(ctx)
	cancel()
	r.reaper.runOnce(ended) // the listing fails: not a completed run
	if n := len(r.events("suite_reaper_run_complete")); n != 1 {
		t.Fatalf("%d run lines after a failed listing, want still 1", n)
	}
	if got := gaugeValue(t, "acu_suite_reaper_last_run_unixtime"); got != 1 {
		t.Fatalf("last run gauge %v after a failed listing, want it untouched", got)
	}

	steps := func() float64 { // a counter has no series before its first count
		v, _ := strconv.ParseFloat(renderedMetric("acu_clock_steps_detected_total"), 64)
		return v
	}
	steps0 := steps()
	r.reaper.lastWall, r.reaper.lastMono = time.Unix(reapNow, 0).Add(-3*time.Hour), time.Now() // 3 h ahead
	r.seed("/r/dists/old2", reapNow-40*reapDay)
	r.reaper.runOnce(ctx)
	runs = r.events("suite_reaper_run_complete")
	if len(runs) != 2 || runs[1]["deferred"] != true || runs[1]["tried"] != float64(0) || !r.exists("/r/dists/old2") {
		t.Fatalf("run lines %v, old2 kept %v; want a deferred run that tried nothing", runs, r.exists("/r/dists/old2"))
	}
	if d := steps() - steps0; d != 1 {
		t.Fatalf("acu_clock_steps_detected_total grew by %v at the forward step, want 1", d)
	}
	if got := gaugeValue(t, "acu_suite_reaper_last_run_unixtime"); got != float64(reapNow) { // it was 1
		t.Fatalf("last run gauge %v after a deferred run, want %d: a deferred run is complete", got, reapNow)
	}
}

// Deferred minor F-18 (design note 9): suite_reaped names what judged it —
// for an idle suite the last-request stamp the cutoff judged, for a junk
// suite the cached rows it judged junk.
func TestReaper_SuiteReapedNamesItsEvidence(t *testing.T) {
	r := newReaperRig(t)
	idleAt := reapNow - 32*reapDay
	r.seed("/r/dists/idle", idleAt)
	r.junkSuite("/r/dists/junk")
	r.cacheAnchor("/r/dists/junk", "InRelease", softPage, reapNow-2*reapDay)
	r.cacheAnchor("/r/dists/junk", "Release", softPage, reapNow-2*reapDay)
	r.reaper.runOnce(context.Background())
	got := map[string]map[string]any{}
	for _, m := range r.events("suite_reaped") {
		got[m["suite_path"].(string)] = m
	}
	if m := got["/r/dists/idle"]; m == nil || m["reason"] != "idle" || m["last_requested_unixtime"] != float64(idleAt) ||
		m["evidence"] != nil {
		t.Fatalf("idle line %v, want last_requested_unixtime %d and no evidence", m, idleAt)
	}
	if m := got["/r/dists/junk"]; m == nil || m["reason"] != "junk" ||
		!reflect.DeepEqual(m["evidence"], []any{"/r/dists/junk/InRelease", "/r/dists/junk/Release"}) || m["last_requested_unixtime"] != nil {
		t.Fatalf("junk line %v, want the two anchors as evidence", m)
	}
}

// Deferred minor F-25: a panic in the flip-gate purge of a deleted suite's
// release still drops the lock-map entry and unlocks the suite — the
// unlock is deferred — so no later deleter finds the mutex leaked; the
// panic goes on to the caller.
func TestHoldSuite_APanickingPurgeStillUnlocks(t *testing.T) {
	r := newReaperRig(t)
	key := cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}
	release, ok := r.checker.HoldSuite(key)
	if !ok {
		t.Fatal("premise: the suite is held")
	}
	r.checker.holdPurgeHook = func() { panic("purge bug") }
	var got any
	func() {
		defer func() { got = recover() }()
		release(true)
	}()
	r.checker.holdPurgeHook = nil
	if got != "purge bug" {
		t.Fatalf("recovered %v, want the purge's panic", got)
	}
	if n := r.checker.LockCount(); n != 0 {
		t.Fatalf("%d lock-map entries after the panicking release, want 0: the deleted suite's entry dropped", n)
	}
	again, ok := r.checker.HoldSuite(key)
	if !ok {
		t.Fatal("the suite's mutex leaked: a later hold was refused")
	}
	again(false)
}

// Deferred minor F-13 (design note 7; execution review G-7): a run that a
// shutdown cuts short after its listing — here while it holds its one
// candidate — is not a completed run: no suite_reaper_run_complete line,
// and the liveness gauge keeps its old value.
// TestReaper_ACompletedRunLogsItsLineAndSetsTheGauge cancels before the
// listing, so it never reaches runOnce's final check.
func TestReaper_ARunCutShortAfterItsListingIsNotComplete(t *testing.T) {
	r := newReaperRig(t)
	r.seed("/r/dists/old", reapNow-32*reapDay)
	reaperLastRunUnixtime.Set(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.reaper.reapHook = func(context.Context, cache.SuiteKey) { cancel() } // the shutdown comes mid-run
	r.reaper.runOnce(ctx)
	if runs := r.events("suite_reaper_run_complete"); len(runs) != 0 {
		t.Fatalf("run lines %v after a run a shutdown cut short, want none", runs)
	}
	if got := gaugeValue(t, "acu_suite_reaper_last_run_unixtime"); got != 1 {
		t.Fatalf("last run gauge %v after a run a shutdown cut short, want it untouched", got)
	}
}

// Deferred minor F-13 (execution review G-7): tried counts every candidate
// the run tried, reaped or not — here one its hold found busy (another
// deleter holds the suite).
func TestReaper_RunLineCountsABusyCandidateAsTried(t *testing.T) {
	r := newReaperRig(t)
	r.seed("/r/dists/old", reapNow-32*reapDay)
	release, ok := r.checker.HoldSuite(reapKey("/r/dists/old"))
	if !ok {
		t.Fatal("premise: the suite is held")
	}
	r.reaper.runOnce(context.Background())
	release(false)
	runs := r.events("suite_reaper_run_complete")
	if len(runs) != 1 || runs[0]["tried"] != float64(1) || runs[0]["reaped"] != float64(0) || !r.exists("/r/dists/old") {
		t.Fatalf("run lines %v, old kept %v; want tried 1, reaped 0 (the busy suite kept)", runs, r.exists("/r/dists/old"))
	}
}

// Deferred minor F-18 for a flat suite (execution review G-7): a junk
// verdict reached through the combined rule (no anchors, every root index
// junk, no signed evidence) names the index rows it judged as its
// evidence.
func TestReaper_AFlatJunkSuiteNamesItsIndexesAsEvidence(t *testing.T) {
	r := newReaperRig(t)
	old := reapNow - 2*reapDay
	r.junkSuite("/junk")
	r.cacheAnchor("/junk", "Packages.gz", softPage, old)
	r.cacheAnchor("/junk", "Sources", softPage, old)
	r.reaper.runOnce(context.Background())
	var line map[string]any
	for _, m := range r.events("suite_reaped") {
		if m["suite_path"] == "/junk" {
			line = m
		}
	}
	if line == nil || line["reason"] != "junk" {
		t.Fatalf("suite_reaped lines %v, want /junk reaped as junk", r.events("suite_reaped"))
	}
	ev, _ := line["evidence"].([]any)
	got := map[string]bool{}
	for _, p := range ev {
		s, _ := p.(string)
		got[s] = true
	}
	if want := map[string]bool{"/junk/Packages.gz": true, "/junk/Sources": true}; len(ev) != 2 || !reflect.DeepEqual(got, want) {
		t.Fatalf("evidence %v, want the two index rows, each once", line["evidence"])
	}
}

// Deferred minor F-18 (Task 6 review minor 4): a flat suite whose junk
// anchors decide alone — its base has signed evidence, so its good index
// does not count (ruling PG-7) — names only the anchors as its evidence.
func TestReaper_ASignedFlatSuiteNamesOnlyItsAnchorsAsEvidence(t *testing.T) {
	r := newReaperRig(t)
	old := reapNow - 2*reapDay
	r.junkSuite("/anchors-signed")
	r.cacheAnchor("/anchors-signed", "InRelease", softPage, old)
	r.cacheAnchor("/anchors-signed", "Packages.gz", "\x1f\x8b\x08\x00 an index", old) // a good index
	orig := r.reaper.signedEvidence
	t.Cleanup(func() { r.reaper.signedEvidence = orig })
	r.reaper.signedEvidence = func(context.Context, string, string) error { return cache.ErrUnsignedDowngradeRefused }
	r.reaper.runOnce(context.Background())
	var line map[string]any
	for _, m := range r.events("suite_reaped") {
		if m["suite_path"] == "/anchors-signed" {
			line = m
		}
	}
	if line == nil || line["reason"] != "junk" {
		t.Fatalf("suite_reaped lines %v, want /anchors-signed reaped as junk", r.events("suite_reaped"))
	}
	if !reflect.DeepEqual(line["evidence"], []any{"/anchors-signed/InRelease"}) {
		t.Fatalf("evidence %v, want only the anchor that decided", line["evidence"])
	}
}

// Deferred minor F-18 (Task 6 review minor 4): a flat suite whose anchors
// and indexes are all junk — the two rules agree — names both sets, the
// anchors first, as its evidence.
func TestReaper_AFlatSuiteJunkThroughoutNamesBothSetsAsEvidence(t *testing.T) {
	r := newReaperRig(t)
	old := reapNow - 2*reapDay
	r.junkSuite("/everything")
	r.cacheAnchor("/everything", "InRelease", softPage, old)
	r.cacheAnchor("/everything", "Packages.gz", softPage, old)
	r.reaper.runOnce(context.Background())
	var line map[string]any
	for _, m := range r.events("suite_reaped") {
		if m["suite_path"] == "/everything" {
			line = m
		}
	}
	if line == nil || line["reason"] != "junk" {
		t.Fatalf("suite_reaped lines %v, want /everything reaped as junk", r.events("suite_reaped"))
	}
	if !reflect.DeepEqual(line["evidence"], []any{"/everything/InRelease", "/everything/Packages.gz"}) {
		t.Fatalf("evidence %v, want the anchor and the index", line["evidence"])
	}
}
