package gc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// Issue #14 follow-up, Tier 1c: GC's failures and work are visible. A pass
// whose failure was logged (WARN gc_tick_failed) counts once in
// acu_gc_pass_failures_total{pass} and, in a startup pass or a periodic
// tick, names itself in LastRunSummary.FailedPasses (the status page's
// gc.last_run_failed_passes, which makes the GC vital crit); the blob
// pass's representation prune reports its rows and its duration.

// failurePasses are acu_gc_pass_failures_total's pass labels.
var failurePasses = []string{"url_path", "snapshot", "blob", "retired", "pool_scan"}

// passFailures is each pass label's acu_gc_pass_failures_total now.
func passFailures(t *testing.T) map[string]float64 {
	t.Helper()
	out := make(map[string]float64, len(failurePasses))
	for _, p := range failurePasses {
		out[p] = gcMetricValue(t, fmt.Sprintf(`acu_gc_pass_failures_total{pass=%q}`, p))
	}
	return out
}

// checkFailureCounts fails the test unless each pass label's counter rose
// from before by want[label] (0 for a label want leaves out).
func checkFailureCounts(t *testing.T, before map[string]float64, want map[string]float64) {
	t.Helper()
	after := passFailures(t)
	for _, p := range failurePasses {
		if got := after[p] - before[p]; got != want[p] {
			t.Errorf(`acu_gc_pass_failures_total{pass=%q} rose by %v, want %v`, p, got, want[p])
		}
	}
}

// Each failed pass of a tick counts once under its own label and is named
// once in FailedPasses, in tick order; the snapshot pass counts once
// however many of its sub-jobs failed (each logs its own gc_tick_failed),
// and a tick in which nothing failed names none.
func TestRunTick_RecordsEachFailedPass(t *testing.T) {
	for _, tc := range []struct {
		name     string
		store    *fakeStore
		want     []string
		warnings int // gc_tick_failed lines
	}{
		{"nothing fails", &fakeStore{}, nil, 0},
		{"the url-path pass", &fakeStore{urlErrs: true}, []string{"url_path"}, 1},
		{"the snapshot pass", &fakeStore{batchErrs: 1}, []string{"snapshot"}, 1},
		{"the snapshot pass's C and D", &fakeStore{chunkErrs: true, dChunkErrs: true}, []string{"snapshot"}, 2},
		{"the blob pass", &fakeStore{pruneErrs: true}, []string{"blob"}, 1},
		{"every pass", &fakeStore{urlErrs: true, batchErrs: 1, pruneErrs: true}, []string{"url_path", "snapshot", "blob"}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logger, buf := captureLogger()
			g, _ := storeGC(t, tc.store, 2, logger)
			g.cfg.URLPathTTL = time.Hour // the url-path pass runs
			before := passFailures(t)
			g.periodicTick(context.Background())

			s, ok := g.LastRunSummary()
			if !ok || !reflect.DeepEqual(s.FailedPasses, tc.want) {
				t.Fatalf("FailedPasses = %#v (summary %v), want %#v", s.FailedPasses, ok, tc.want)
			}
			if ev := gcEvents(t, buf, "gc_tick_failed"); len(ev) != tc.warnings {
				t.Fatalf("gc_tick_failed lines %v, want %d", ev, tc.warnings)
			}
			want := map[string]float64{}
			for _, p := range tc.want {
				want[p] = 1
			}
			checkFailureCounts(t, before, want)
		})
	}
}

// The startup pass records its failed passes as a periodic tick does.
func TestStartupPass_RecordsEachFailedPass(t *testing.T) {
	logger, _ := captureLogger()
	f := &fakeStore{batchErrs: 1, pruneErrs: true}
	g, _ := storeGC(t, f, 2, logger)
	before := passFailures(t)
	if err := g.StartupPass(context.Background()); err == nil {
		t.Fatal("StartupPass = nil, want the snapshot pass's failure")
	}
	s, ok := g.LastRunSummary()
	if want := []string{"snapshot", "blob"}; !ok || s.Phase != "startup" || !reflect.DeepEqual(s.FailedPasses, want) {
		t.Fatalf("summary %+v (%v), want a startup summary with FailedPasses %v", s, ok, want)
	}
	checkFailureCounts(t, before, map[string]float64{"snapshot": 1, "blob": 1})
}

// The controller's ruling on Task 5's review: a sub-job that failed and
// logged its gc_tick_failed line still counts, and still names its pass,
// when a shutdown lands in a later sub-job (recordFailure then finds the
// GC's context ended); a pass that only a shutdown stopped logged nothing,
// and counts nothing.
func TestRunTick_ALoggedFailureCountsThoughAShutdownFollows(t *testing.T) {
	for _, tc := range []struct {
		name     string
		store    func(cancel context.CancelFunc) *fakeStore
		want     []string
		warnings int
	}{
		{
			name: "the batch fails, then a shutdown during C",
			store: func(cancel context.CancelFunc) *fakeStore {
				return &fakeStore{batchErrs: 1, chunkHook: cancel}
			},
			want:     []string{"snapshot"},
			warnings: 1,
		},
		{
			name: "a shutdown fails the batch",
			store: func(cancel context.CancelFunc) *fakeStore {
				return &fakeStore{batchErrs: 1, batchHook: cancel}
			},
			want:     nil,
			warnings: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			logger, buf := captureLogger()
			g, _ := storeGC(t, tc.store(cancel), 2, logger)
			g.nextPass = snapshotPass
			before := passFailures(t)
			g.periodicTick(ctx)

			if ev := gcEvents(t, buf, "gc_tick_failed"); len(ev) != tc.warnings {
				t.Fatalf("gc_tick_failed lines %v, want %d", ev, tc.warnings)
			}
			s, ok := g.LastRunSummary()
			if !ok || !reflect.DeepEqual(s.FailedPasses, tc.want) {
				t.Fatalf("FailedPasses = %#v (summary %v), want %#v: one entry per logged failure", s.FailedPasses, ok, tc.want)
			}
			want := map[string]float64{}
			for _, p := range tc.want {
				want[p] = 1
			}
			checkFailureCounts(t, before, want)
		})
	}
}

// A failed retired run counts once under pass="retired", where its failure
// is logged, however many of its sub-jobs failed, and leaves the last-run
// summary as the last tick wrote it: a retired run writes none.
func TestRetiredRun_CountsAFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		dFails   bool
		warnings int
	}{
		{"C fails", false, 1},
		{"C and D fail", true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logger, buf := captureLogger()
			f := &fakeStore{}
			g, _ := storeGC(t, f, 2, logger)
			g.periodicTick(context.Background())
			summary, ok := g.LastRunSummary()
			if !ok {
				t.Fatal("no summary after the periodic tick")
			}
			f.mu.Lock()
			f.chunkErrs, f.dChunkErrs = true, tc.dFails
			f.mu.Unlock()
			buf.Reset()
			before := passFailures(t)
			g.retiredRun(context.Background())

			if ev := gcEvents(t, buf, "gc_tick_failed"); len(ev) != tc.warnings {
				t.Fatalf("gc_tick_failed lines %v, want %d", ev, tc.warnings)
			}
			checkFailureCounts(t, before, map[string]float64{"retired": 1})
			if after, ok := g.LastRunSummary(); !ok || !reflect.DeepEqual(after, summary) {
				t.Fatalf("summary after the retired run %+v, want the tick's, unchanged: %+v", after, summary)
			}
		})
	}
}

// A pool scan that fails (here pool/ is a file, so the scan cannot read
// it) ends the startup pass before its passes and counts under
// pass="pool_scan"; it writes no summary.
func TestStartupPass_APoolScanFailureIsCounted(t *testing.T) {
	logger, _ := captureLogger()
	g, c := storeGC(t, &fakeStore{}, 2, logger)
	pool := filepath.Join(c.Dir(), "pool")
	if err := os.RemoveAll(pool); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pool, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := passFailures(t)
	if err := g.StartupPass(context.Background()); err == nil {
		t.Fatal("StartupPass = nil, want the pool scan's failure")
	}
	checkFailureCounts(t, before, map[string]float64{"pool_scan": 1})
	if s, ok := g.LastRunSummary(); ok {
		t.Fatalf("summary %+v, want none: a failed pool scan ends the startup pass before its passes", s)
	}
}

// A write the writer never started before its pass's deadline is the
// deadline, never a failure (issue #14 Tier 1a): with the writer held past
// the tick's end, the snapshot pass's batch, the representation prune and
// the blob pass's batch are all abandoned, and the tick names no failed
// pass and counts none.
func TestRunTick_ANotStartedWriteIsNotAFailure(t *testing.T) {
	c := openTestCache(t)
	logger, buf := captureLogger()
	g := startDeadlineGC(t, c, logger, time.Second)
	g.nextPass = snapshotPass
	holdGCWriter(t, c, time.Now().Add(g.cfg.MaxTickDuration+500*time.Millisecond))
	before := passFailures(t)
	g.periodicTick(context.Background())

	noTickFailed(t, buf)
	s, ok := g.LastRunSummary()
	if !ok || len(s.FailedPasses) != 0 || !s.DeadlineReached {
		t.Fatalf("summary %+v (%v), want deadline_reached and no failed pass", s, ok)
	}
	checkFailureCounts(t, before, nil)
}

// seedPrunableDates is seedPruneFixture's cache plus n-1 more unowned,
// day-old dates: n prunable dates in all, and no reapable blob.
func seedPrunableDates(t *testing.T, c *cache.Cache, n int) {
	t.Helper()
	shared := seedPruneFixture(t, c, 0)
	for i := range n - 1 {
		if err := c.PutRepresentation(context.Background(), "http", "h", fmt.Sprintf("/orphan%d", i), shared,
			"Mon, 01 Jan 2024 00:00:00 GMT"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := dbOf(t, c).Exec(`UPDATE representation_metadata SET observed_at = 1`); err != nil {
		t.Fatal(err)
	}
}

// The blob pass's representation prune reports what it released and how
// long it took: gc_run_complete's representation_rows_pruned and
// representation_prune_ms, the last-run summary,
// acu_gc_representation_rows_pruned_total, and a pass="representation_prune"
// sample of acu_gc_pass_duration_seconds, in a periodic tick and in the
// startup pass alike.
func TestRunBlobPass_ReportsThePrunesCountAndDuration(t *testing.T) {
	for _, phase := range []string{"periodic", "startup"} {
		t.Run(phase, func(t *testing.T) {
			logger, buf := captureLogger()
			g, c := storeGC(t, &fakeStore{}, 2, logger)
			seedPrunableDates(t, c, 5)
			durations := fmt.Sprintf(`acu_gc_pass_duration_seconds_count{phase=%q,pass="representation_prune"}`, phase)
			beforeRows := gcMetricValue(t, "acu_gc_representation_rows_pruned_total")
			beforeDurations := gcMetricValue(t, durations)
			if phase == "startup" {
				if err := g.StartupPass(context.Background()); err != nil {
					t.Fatal(err)
				}
			} else {
				g.periodicTick(context.Background())
			}

			runs := gcEvents(t, buf, "gc_run_complete")
			if len(runs) != 1 || runs[0]["representation_rows_pruned"] != float64(5) {
				t.Fatalf("gc_run_complete lines %v, want one with representation_rows_pruned 5", runs)
			}
			if ms, ok := runs[0]["representation_prune_ms"].(float64); !ok || ms < 0 {
				t.Fatalf("gc_run_complete representation_prune_ms = %v, want a duration >= 0", runs[0]["representation_prune_ms"])
			}
			if s, ok := g.LastRunSummary(); !ok || s.RepresentationRowsPruned != 5 || s.RepresentationPruneSeconds <= 0 {
				t.Fatalf("summary %+v (%v), want RepresentationRowsPruned 5 and the prune's duration", s, ok)
			}
			if got := gcMetricValue(t, "acu_gc_representation_rows_pruned_total") - beforeRows; got != 5 {
				t.Fatalf("acu_gc_representation_rows_pruned_total rose by %v, want 5", got)
			}
			if got := gcMetricValue(t, durations) - beforeDurations; got != 1 {
				t.Fatalf("%s rose by %v, want 1: one prune a run", durations, got)
			}
		})
	}
}
