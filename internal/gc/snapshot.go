package gc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// snapshotPassResult counts one snapshot pass's reaps by class.
//
// AIDEV-NOTE: batch is a named field, never embedded (deferred minor
// F-3): embedded, SnapshotGCBatchResult's Total() was promoted and
// silently left out retired.
type snapshotPassResult struct {
	batch cache.SnapshotGCBatchResult
	// retired counts the retired snapshots sub-job C deleted (spec C9).
	retired int
}

// runSnapshotPass implements SPEC4 §9.6.3: a per-tick batched reap
// loop over orphan candidate snapshots (sub-job A) ∪ displaced
// snapshots beyond keep-N (sub-job B). Each batch is a single writer-
// tx that runs SELECT-inside-tx + cascade DELETE; the SELECT-DELETE
// liveness race is closed by single-writer ordering (heartbeat /
// CommitAdoption / InsertCandidateSnapshot writes serialize through
// the same writer goroutine — they cannot interleave between SELECT
// and DELETE inside this tx). Once A ∪ B is drained, sub-job C (spec C9)
// deletes the retired snapshots — those of reaped, evicted and forgotten
// suites, and those the legacy trust audit quarantined — in chunks of at
// most 10,000 rows per writer transaction; the startup pass (phase
// phaseStartup) runs no sub-job C, and its periodic ticks do (ruling
// PF-15), as does retiredRun, once, an hour after the start.
//
// AIDEV-NOTE: ruling PF-15 (final review concurrency m1). StartupPass runs
// before any listener serves, and retired snapshots never serve, so
// nothing waits on C there; but C's backlog after a burst of reaps (a
// large suite's snapshots hold some 70-100k package_hash rows each, and a
// reap retires its current and displaced ones) could keep every listener
// closed up to max_tick_duration. The startup pass therefore ends the
// snapshot pass once A ∪ B is drained, as if C were done, so its blob pass
// still runs. The periodic ticks run C, normally the first: its snapshot
// pass begins with A ∪ B, then C. Not always the first (the final fix
// wave's re-review, gc-sims minor 1): the passes resume in rotation
// (runTick), so when the startup pass's own blob pass hits its deadline,
// the first periodic tick resumes only that blob pass, and C runs at the
// second at the earliest; a periodic pass that hits its deadline defers C
// the same way. Retired rows wait for that tick: normally one GC interval
// after the startup pass, two or more when its blob pass hit its deadline.
// They never serve meanwhile. When gc.interval is longer than an hour,
// retiredRun runs C once an hour after the start too, so a daemon
// restarted more often than gc.interval still deletes them (the final
// review's R3 #2; retiredRun's AIDEV-NOTE).
//
// Returns:
//   - the cumulative counts (orphan, displaced, retired),
//   - whether the deadline tripped (true means the pass exited
//     between batches with backlog remaining; runTick then returns
//     without running the blob pass, and the next tick resumes this
//     pass — A ∪ B, then C — before the blob pass),
//   - error iff a DB-level failure occurred.
func (g *GC) runSnapshotPass(ctx context.Context, deadline time.Time, phase string) (snapshotPassResult, bool, error) {
	staleGraceSeconds := int64(g.cfg.HeartbeatStaleGrace.Seconds())

	var (
		acc        snapshotPassResult
		batchesRun int
	)
	expired := g.snapshotDeadline(deadline, phase, &batchesRun)
	for {
		if err := ctx.Err(); err != nil {
			return acc, false, nil
		}
		if expired() {
			return acc, true, nil
		}

		batch, err := g.store.RunSnapshotGCBatch(ctx,
			g.cfg.SnapshotBatchSize,
			staleGraceSeconds,
			g.cfg.KeepDisplaced,
		)
		if err != nil {
			return acc, false, fmt.Errorf("snapshot gc batch: %w", err)
		}
		batchesRun++
		acc.batch.OrphanReaped += batch.OrphanReaped
		acc.batch.DisplacedReaped += batch.DisplacedReaped

		if batch.Total() == 0 {
			break // A ∪ B drained
		}
	}
	if phase == phaseStartup {
		return acc, false, nil // no sub-job C before the listeners serve (ruling PF-15)
	}
	retired, deadlineHit, err := g.runSubJobC(ctx, expired, &batchesRun)
	acc.retired += retired
	return acc, deadlineHit, err
}

// snapshotDeadline returns the snapshot pass's deadline test: false before
// deadline; after it, true, once gc_tick_deadline_reached is logged.
func (g *GC) snapshotDeadline(deadline time.Time, phase string, batchesRun *int) func() bool {
	return func() bool {
		if time.Now().Before(deadline) {
			return false
		}
		g.cfg.Logger.Info("gc_tick_deadline_reached",
			"phase", phase,
			"which", "snapshot",
			"batches_completed", *batchesRun,
			"bytes_reclaimed_this_tick", int64(0),
		)
		return true
	}
}

// runSubJobC is snapshot GC sub-job C (spec C9): it deletes the retired
// snapshots, one per writer transaction in bounded chunks, until none is
// left, ctx ends, or expired reports the deadline (deadlineHit: backlog
// left). The snapshot pass runs it after sub-jobs A and B; retiredRun runs
// it alone, once, an hour after the start.
func (g *GC) runSubJobC(ctx context.Context, expired func() bool, batchesRun *int) (retired int, deadlineHit bool, err error) {
	for {
		if err := ctx.Err(); err != nil {
			return retired, false, nil
		}
		if expired() {
			return retired, true, nil
		}
		// 0: the cache's own chunk (retiredChunkRows, 10,000 rows a
		// transaction), never a larger number (deferred minor F-2).
		reaped, more, err := g.store.RunRetiredSnapshotGCChunk(ctx, 0)
		if err != nil {
			return retired, false, fmt.Errorf("retired snapshot gc chunk: %w", err)
		}
		*batchesRun++
		retired += reaped
		if !more {
			return retired, false, nil
		}
	}
}

// retiredRunAfterStart is when, after the start, Run deletes the retired
// snapshots once (retiredRun), if gc.interval is longer.
const retiredRunAfterStart = time.Hour

// phaseRetired labels retiredRun's gc_tick_deadline_reached line.
const phaseRetired = "retired"

// retiredRun runs sub-job C alone, once, retiredRunAfterStart after the
// start (Run), within max_tick_duration, and logs INFO
// gc_retired_run_complete. A panic is recovered (gc_panicked,
// where=retired); the periodic ticks are not affected.
//
// AIDEV-NOTE: the final review's R3 #2. Sub-job C ran only in the periodic
// ticks (the startup pass skips it, ruling PF-15), and the first tick comes
// gc.interval (24 h by default) after the start, so a daemon restarted
// more often — a laptop, a container recreated nightly, a host rebooted
// daily — never deleted retired snapshots: their members, skipped members
// and package_hash rows (some 70–100k a large suite's snapshot) and the
// blobs those keep reachable stayed for good. Not an earlier first
// periodic tick: a tick also runs the url_path pass, a full scan of
// url_path with the version-aware mirror guard, and the blob pass, both of
// which the startup pass ran an hour before — work a restart would then
// pay twice. This run is only C, bounded and chunked as in a tick, on Run's
// goroutine (never beside a tick). Its blobs are reclaimed by the next
// blob pass: the next start's or the next periodic tick's.
func (g *GC) retiredRun(ctx context.Context) {
	defer g.recoverPanic("retired")
	start := time.Now()
	batchesRun := 0
	retired, deadlineHit, err := g.runSubJobC(ctx,
		g.snapshotDeadline(start.Add(g.cfg.MaxTickDuration), phaseRetired, &batchesRun), &batchesRun)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		g.cfg.Logger.Warn("gc_tick_failed", "err", err)
	}
	if retired > 0 {
		gcRetiredReapedTotal.Add(float64(retired))
	}
	g.cfg.Logger.Info("gc_retired_run_complete",
		"retired_reaped", retired,
		"deadline_reached", deadlineHit,
		"duration_ms", time.Since(start).Milliseconds(),
	)
}
