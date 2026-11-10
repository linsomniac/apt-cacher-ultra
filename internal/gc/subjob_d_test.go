package gc

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// Issue #14 follow-up, Tier 2: sub-job D strips the package_hash rows of
// the displaced snapshots (cache.RunDisplacedPackageHashChunk), after A ∪ B
// and C, in the snapshot pass and in the retired run; and a sub-job that
// fails no longer stops the sub-jobs after it (ruling HD-15 within the
// pass), so a C stuck on one snapshot never stops D.

// The snapshot pass runs A ∪ B's batches, then C's chunks, then D's, each
// asking for the cache's own chunk (0), and the tick reports D's rows:
// displaced_package_rows_deleted in gc_run_complete, the last-run summary
// and acu_gc_displaced_package_rows_deleted_total.
func TestRunSnapshotPass_RunsDAfterC(t *testing.T) {
	logger, buf := captureLogger()
	f := &fakeStore{chunksMore: 2, dChunksMore: 2}
	g, _ := storeGC(t, f, 2, logger)
	before := gcMetricValue(t, "acu_gc_displaced_package_rows_deleted_total")
	g.periodicTick(context.Background())

	if want := []string{"batch", "C", "C", "C", "D", "D", "D"}; !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("store calls %v, want %v: A ∪ B, then C, then D", f.calls, want)
	}
	if !reflect.DeepEqual(f.dChunkRows, []int{0, 0, 0}) {
		t.Fatalf("D chunk calls asked for %v rows, want [0 0 0] (the cache's own chunk)", f.dChunkRows)
	}
	const want = 2 * fakeDChunkRows
	runs := gcEvents(t, buf, "gc_run_complete")
	if len(runs) != 1 || runs[0]["displaced_package_rows_deleted"] != float64(want) {
		t.Fatalf("gc_run_complete lines %v, want one with displaced_package_rows_deleted %d", runs, want)
	}
	if s, ok := g.LastRunSummary(); !ok || s.DisplacedPackageRowsDeleted != want {
		t.Fatalf("LastRunSummary = %+v, %v; want DisplacedPackageRowsDeleted %d", s, ok, want)
	}
	if got := gcMetricValue(t, "acu_gc_displaced_package_rows_deleted_total") - before; got != want {
		t.Fatalf("acu_gc_displaced_package_rows_deleted_total rose by %v, want %d", got, want)
	}
	if ev := gcEvents(t, buf, "gc_tick_failed"); len(ev) != 0 {
		t.Fatalf("gc_tick_failed lines %v, want none", ev)
	}
}

// subJobFailureCase is one row of the tests in which a sub-job of the
// snapshot pass fails: the fake's knobs, the store calls the pass makes,
// and the gc_tick_failed lines' err prefixes, in order (the first is the
// error runTick returns).
type subJobFailureCase struct {
	name   string
	store  *fakeStore
	calls  []string
	failed []string
}

// runSubJobFailureCase runs one periodic tick (runTick, so its error is
// seen) over tc.store with a dead blob seeded, and checks that every
// sub-job after the failed one ran, that each failed sub-job logged one
// WARN gc_tick_failed and runTick none more, that runTick returned the
// first failure, and that the rotation moved on: the blob pass ran in the
// same tick (the dead blob reaped) and then reset it to the url-path pass.
func runSubJobFailureCase(t *testing.T, tc subJobFailureCase) {
	t.Helper()
	logger, buf := captureLogger()
	g, c := storeGC(t, tc.store, 2, logger)
	dead := seedReapableBlob(t, c, "a dead blob")
	g.nextPass = snapshotPass

	_, err := g.runTick(context.Background(), "periodic")
	if !reflect.DeepEqual(tc.store.calls, tc.calls) {
		t.Fatalf("store calls %v, want %v", tc.store.calls, tc.calls)
	}
	if err == nil || !strings.Contains(err.Error(), tc.failed[0]) {
		t.Fatalf("runTick = %v, want the first failure, %q", err, tc.failed[0])
	}
	ev := gcEvents(t, buf, "gc_tick_failed")
	if len(ev) != len(tc.failed) {
		t.Fatalf("gc_tick_failed lines %v, want %d: one for each failed sub-job, none more", ev, len(tc.failed))
	}
	for i, want := range tc.failed {
		if msg, _ := ev[i]["err"].(string); ev[i]["level"] != "WARN" || !strings.Contains(msg, want) {
			t.Fatalf("gc_tick_failed line %d: %v, want a WARN whose err names %q", i, ev[i], want)
		}
	}
	if blobRowExists(t, dbOf(t, c), dead) {
		t.Fatal("the dead blob is still there: the blob pass did not run after the failed snapshot pass")
	}
	if g.nextPass != urlPathPass {
		t.Fatalf("the rotation is at pass %d, want the url-path pass (%d): the failed snapshot pass moved it on", g.nextPass, urlPathPass)
	}
}

// Review Focus 3: sub-job C fails on the same snapshot at every call; D
// still runs and strips (three calls: two that strip, one that finds none
// left), and the pass still counts as failed, with C's error. Each failed
// sub-job logs one WARN gc_tick_failed: one when only C fails, two when D
// fails too.
func TestRunSnapshotPass_AFailingCStillRunsD(t *testing.T) {
	for _, tc := range []subJobFailureCase{
		{
			name:   "only C fails",
			store:  &fakeStore{chunkErrs: true, dChunksMore: 2},
			calls:  []string{"batch", "C", "D", "D", "D"},
			failed: []string{"retired snapshot gc chunk"},
		},
		{
			name:   "D fails too",
			store:  &fakeStore{chunkErrs: true, dChunksMore: 2, dChunkErrs: true},
			calls:  []string{"batch", "C", "D"},
			failed: []string{"retired snapshot gc chunk", "displaced package_hash gc chunk"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) { runSubJobFailureCase(t, tc) })
	}
}

// Ruling HD-15 within the pass: an A ∪ B batch that fails no longer stops
// the pass: C and D run after it, and the pass returns the batch's error.
func TestRunSnapshotPass_AFailingBatchStillRunsCAndD(t *testing.T) {
	for _, tc := range []subJobFailureCase{
		{
			name:   "only the batch fails",
			store:  &fakeStore{batchErrs: 1, chunksMore: 2, dChunksMore: 2},
			calls:  []string{"batch", "C", "C", "C", "D", "D", "D"},
			failed: []string{"snapshot gc batch"},
		},
		{
			name:   "every sub-job fails",
			store:  &fakeStore{batchErrs: 1, chunkErrs: true, dChunkErrs: true},
			calls:  []string{"batch", "C", "D"},
			failed: []string{"snapshot gc batch", "retired snapshot gc chunk", "displaced package_hash gc chunk"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) { runSubJobFailureCase(t, tc) })
	}
}

// The retired run runs D after C, so the hourly runs strip too, and reports
// a backlog — Run re-arms it an hour later — when D stops at its deadline
// with rows left: two 30 s D chunks in its 1 min budget, then the deadline.
func TestRetiredRun_RunsDAndReArmsOnItsBacklog(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger, buf := captureLogger()
		f := &fakeStore{dChunksMore: 1000, dChunkSleep: 30 * time.Second}
		g, _ := storeGC(t, f, 2, logger)
		g.cfg.MaxTickDuration = time.Minute
		before := gcMetricValue(t, "acu_gc_displaced_package_rows_deleted_total")
		if !g.retiredRun(context.Background()) {
			t.Fatal("retiredRun = false, want a backlog: D stopped at its deadline with rows left")
		}
		if want := []string{"C", "D", "D"}; !reflect.DeepEqual(f.calls, want) {
			t.Fatalf("store calls %v, want %v: C, then D until its deadline", f.calls, want)
		}
		const want = 2 * fakeDChunkRows
		runs := gcEvents(t, buf, "gc_retired_run_complete")
		if len(runs) != 1 || runs[0]["displaced_package_rows_deleted"] != float64(want) || runs[0]["deadline_reached"] != true {
			t.Fatalf("gc_retired_run_complete lines %v, want one with displaced_package_rows_deleted %d and deadline_reached true", runs, want)
		}
		if got := gcMetricValue(t, "acu_gc_displaced_package_rows_deleted_total") - before; got != want {
			t.Fatalf("acu_gc_displaced_package_rows_deleted_total rose by %v, want %d", got, want)
		}
		if line := deadlineLine(t, buf, "snapshot"); line["phase"] != phaseRetired {
			t.Fatalf("deadline line %v, want the retired run's", line)
		}
		noTickFailed(t, buf)
	})
}

// The retired run gets the snapshot pass's treatment: a C that fails no
// longer stops D, and each logs its own WARN gc_tick_failed. A run that
// failed and drained D reports no backlog (no hourly retry storm).
func TestRetiredRun_AFailingCStillRunsD(t *testing.T) {
	for _, tc := range []struct {
		name   string
		store  *fakeStore
		calls  []string
		failed []string
	}{
		{"only C fails", &fakeStore{chunkErrs: true, dChunksMore: 2}, []string{"C", "D", "D", "D"},
			[]string{"retired chunk fault"}},
		{"D fails too", &fakeStore{chunkErrs: true, dChunkErrs: true}, []string{"C", "D"},
			[]string{"retired chunk fault", "displaced chunk fault"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logger, buf := captureLogger()
			g, _ := storeGC(t, tc.store, 2, logger)
			if g.retiredRun(context.Background()) {
				t.Fatal("retiredRun = true, want false: it failed and D drained")
			}
			if !reflect.DeepEqual(tc.store.calls, tc.calls) {
				t.Fatalf("store calls %v, want %v", tc.store.calls, tc.calls)
			}
			ev := gcEvents(t, buf, "gc_tick_failed")
			if len(ev) != len(tc.failed) {
				t.Fatalf("gc_tick_failed lines %v, want %d", ev, len(tc.failed))
			}
			for i, want := range tc.failed {
				if msg, _ := ev[i]["err"].(string); !strings.Contains(msg, want) {
					t.Fatalf("gc_tick_failed line %d err %q, want %q", i, msg, want)
				}
			}
			runs := gcEvents(t, buf, "gc_retired_run_complete")
			if len(runs) != 1 {
				t.Fatalf("gc_retired_run_complete lines %v, want one", runs)
			}
		})
	}
}

// A D chunk the writer never started before the deadline
// (cache.ErrWriteNotStarted) is the deadline, never a failure (issue #14
// Tier 1a's pattern): the snapshot pass returns deadlineHit and no error,
// the retired run reports a backlog, neither logs gc_tick_failed, and each
// logs one gc_tick_deadline_reached.
func TestSubJobD_AChunkNeverStartedIsTheDeadline(t *testing.T) {
	t.Run("snapshot pass", func(t *testing.T) {
		logger, buf := captureLogger()
		g, _ := storeGC(t, &fakeStore{dChunkNotStarted: true}, 2, logger)
		_, hit, err := g.runSnapshotPass(context.Background(), time.Now().Add(time.Hour), "periodic")
		if err != nil || !hit {
			t.Fatalf("runSnapshotPass: deadlineHit %v, %v; want the deadline, no error", hit, err)
		}
		noTickFailed(t, buf)
		deadlineLine(t, buf, "snapshot")
	})
	t.Run("retired run", func(t *testing.T) {
		logger, buf := captureLogger()
		g, _ := storeGC(t, &fakeStore{dChunkNotStarted: true}, 2, logger)
		if !g.retiredRun(context.Background()) {
			t.Fatal("retiredRun = false, want a backlog: D's chunk never started before the deadline")
		}
		noTickFailed(t, buf)
		deadlineLine(t, buf, "snapshot")
	})
}

// A shutdown (ctx cancelled) during a sub-job stops the sub-jobs after it:
// D never runs after a shutdown, and nothing logs gc_tick_failed (a shutdown
// is not a failure). A sub-job that failed because of it leaves the rotation
// as it is (passFailed's test).
func TestSubJobs_AShutdownStopsTheLaterSubJobs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		store  func(cancel func()) *fakeStore
		calls  []string
		failed bool // the sub-job failed because of the shutdown
	}{
		{"during A ∪ B", func(cancel func()) *fakeStore {
			return &fakeStore{batchErrs: 1, batchHook: cancel, dChunksMore: 2}
		}, []string{"batch"}, true},
		{"during C", func(cancel func()) *fakeStore {
			return &fakeStore{chunkErrs: true, chunkHook: cancel, dChunksMore: 2}
		}, []string{"batch", "C"}, true},
		{"during a C that succeeds", func(cancel func()) *fakeStore {
			return &fakeStore{chunksMore: 5, chunkHook: cancel, dChunksMore: 2}
		}, []string{"batch", "C"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logger, buf := captureLogger()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := tc.store(cancel)
			g, _ := storeGC(t, f, 2, logger)
			g.nextPass = snapshotPass
			_, _ = g.runTick(ctx, "periodic")
			if !reflect.DeepEqual(f.calls, tc.calls) {
				t.Fatalf("store calls %v, want %v: nothing after the shutdown", f.calls, tc.calls)
			}
			noTickFailed(t, buf)
			if tc.failed && g.nextPass != snapshotPass {
				t.Fatalf("the rotation is at pass %d, want the snapshot pass (%d): a shutdown leaves it", g.nextPass, snapshotPass)
			}
		})
	}
	t.Run("retired run, during C", func(t *testing.T) {
		logger, buf := captureLogger()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f := &fakeStore{chunkErrs: true, chunkHook: cancel, dChunksMore: 2}
		g, _ := storeGC(t, f, 2, logger)
		if g.retiredRun(ctx) {
			t.Fatal("retiredRun = true after a shutdown, want false: no hourly re-arm")
		}
		if want := []string{"C"}; !reflect.DeepEqual(f.calls, want) {
			t.Fatalf("store calls %v, want %v: no D after the shutdown", f.calls, want)
		}
		noTickFailed(t, buf)
	})
}

// On a real cache: a suite with three adopted snapshots, each holding
// package_hash rows, and keep_displaced = 3 (so B retires none). One
// periodic tick's D strips the two displaced snapshots' package_hash rows
// and nothing else: their headers and members stay, and the current
// snapshot's rows are intact; LastRunSummary reports the rows stripped.
func TestPeriodicTick_StripsDisplacedPackageRowsOnARealCache(t *testing.T) {
	logger, buf := captureLogger()
	g, c := storeGC(t, nil, 2, logger)
	g.store = c // the real cache behind the seam too, as New sets it
	g.cfg.KeepDisplaced = 3
	db := dbOf(t, c)
	const pkgs = 5
	var ids []int64
	for seq := range 3 { // two displaced, then the current snapshot
		ids = append(ids, adoptBig(t, c, "dists/strip", seq, pkgs, 64))
	}
	displaced, current := ids[:2], ids[2]
	count := func(table string, id int64) int {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM `+table+` WHERE snapshot_id = ?`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, id := range ids {
		if n := count("package_hash", id); n != pkgs {
			t.Fatalf("snapshot %d holds %d package_hash rows before the tick, want %d", id, n, pkgs)
		}
	}

	g.periodicTick(context.Background())

	for _, id := range displaced {
		if n := count("package_hash", id); n != 0 {
			t.Errorf("displaced snapshot %d still holds %d package_hash rows, want 0", id, n)
		}
		if !snapshotRowExists(t, db, id) {
			t.Errorf("displaced snapshot %d's header was deleted, want it kept (keep_displaced)", id)
		}
		if n := count("snapshot_member", id); n != 1 {
			t.Errorf("displaced snapshot %d holds %d members, want its 1 kept", id, n)
		}
	}
	if n := count("package_hash", current); n != pkgs {
		t.Errorf("the current snapshot holds %d package_hash rows, want its %d intact", n, pkgs)
	}
	if n := count("snapshot_member", current); n != 1 {
		t.Errorf("the current snapshot holds %d members, want 1", n)
	}
	want := len(displaced) * pkgs
	if s, ok := g.LastRunSummary(); !ok || s.DisplacedPackageRowsDeleted != want {
		t.Fatalf("LastRunSummary = %+v, %v; want DisplacedPackageRowsDeleted %d", s, ok, want)
	}
	if ev := gcEvents(t, buf, "gc_tick_failed"); len(ev) != 0 {
		t.Fatalf("gc_tick_failed lines %v, want none", ev)
	}
}
