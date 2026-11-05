package gc

import (
	"context"
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
// tx that runs SELECT-inside-tx + cascade DELETE of the orphans, and of
// the displaced snapshots in the startup pass (phase phaseStartup); a
// periodic tick's batch retires the displaced snapshots instead
// (retireDisplaced: one UPDATE), for sub-job C to delete (DN-7 of the GC
// and upstream hardening plan). The SELECT-DELETE (or -UPDATE)
// liveness race is closed by single-writer ordering (heartbeat /
// CommitAdoption / InsertCandidateSnapshot writes serialize through
// the same writer goroutine — they cannot interleave between SELECT
// and DELETE inside this tx). Once A ∪ B is drained, sub-job C (spec C9)
// deletes the retired snapshots — those of reaped, evicted and forgotten
// suites, those the legacy trust audit quarantined, and the displaced
// snapshots a periodic tick's B retired — in chunks of at most 10,000
// rows per writer transaction, in the startup pass and the periodic ticks
// alike (ruling PF-15, re-ruled for issue #14), as does retiredRun, an
// hour after the start and hourly while a backlog lasts.
//
// AIDEV-NOTE: ruling PF-15 (final review concurrency m1), re-ruled by the
// maintainer, 2026-11-05 (issue #14; the final review 2's G2-I2). PF-15
// had the startup pass end its snapshot pass once A ∪ B drained, as if C
// were done: StartupPass runs before any listener serves, and C's backlog
// after a burst of reaps (a large suite's snapshots hold some 70-100k
// package_hash rows each, and a reap retires its current and displaced
// ones) could keep every listener closed up to max_tick_duration. But
// since DN-7 a periodic B only retires the displaced snapshots, and the
// startup B selects none that is retired, so with no C at a start a
// restart deleted nothing a periodic B had retired, nor a reaped suite's,
// and under a sustained displacement rate the retired backlog grew without
// bound where 1.0.x and main kept up, and a restart, the 1.0.x users'
// remedy, no longer cleared it. Now the startup pass runs C after A ∪ B,
// within the snapshot pass's deadline (passDeadlines: four fifths of
// max_tick_duration), and its blob pass reclaims their blobs in the same
// start. The cost: a start with a retired backlog may wait up to
// max_tick_duration before the listeners open, the bound ruling HD-7
// already accepts for B's one-transaction deletes at a start; C only
// fills the budget B leaves. C deletes every retired snapshot, a startup
// trust audit's quarantined ones included (spec C9: the audit runs before
// the startup pass, and a quarantined snapshot never serves or vouches
// again), and never one that is not retired, such as a displaced snapshot
// the audit still holds queued (B never ranks those: ruling R-H1). What a
// start leaves, the periodic ticks and retiredRun delete: hourly while a
// backlog lasts (Run). Retired rows never serve meanwhile.
//
// AIDEV-NOTE: rulings HD-4, HD-7 (DN-7 of the GC and upstream hardening
// plan; the H3 final review R3 #4). A periodic tick's B only retires the
// displaced snapshots beyond keep_displaced (retireDisplaced = phase !=
// phaseStartup), and C deletes them in this same pass, 10,000 rows a
// transaction: B's cascade DELETE held the writer — every miss's store,
// every flip's commit — for seconds on a large suite while the listeners
// served. The startup pass's B still deletes them: it runs before the
// freshness scheduler and the listeners (main), so its long transactions
// hold up no other writer and no request, and a daemon that never lives
// until C runs (min(1 h, gc.interval)) still deletes them, their blobs in
// this start's blob pass. When a periodic tick's pass stops before its C
// has deleted what its B retired — at its deadline, at a shutdown (ctx),
// or on a batch or chunk that failed or panicked — those snapshots are no
// longer B-selectable (retired_at); C deletes them: retiredRun an hour
// later when the pass stopped at its deadline (issue #14), the next start's
// startup pass (PF-15 re-ruled), or a later periodic tick's snapshot pass
// (the next one, after a failure too, since a failed pass no longer ends
// its tick: ruling HD-15; after a panic, the one after it, since the
// rotation moves past the snapshot pass). Such a snapshot counts twice: in
// displaced_reaped when B retires it, in retired_reaped when C deletes it.
//
// Returns:
//   - the cumulative counts (orphan, displaced, retired),
//   - whether the deadline tripped (true means the pass exited
//     between batches with backlog remaining; runTick then moves the
//     rotation to the blob pass, which runs in the same tick, under its
//     own share of the budget, and the next tick that reaches this pass
//     re-selects — A ∪ B, then C: issue #14),
//   - error iff a DB-level failure occurred (runTick then runs the blob
//     pass all the same: ruling HD-15).
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
			phase != phaseStartup,
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
	// Sub-job C, at a start too (ruling PF-15 re-ruled, issue #14).
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
// left). The snapshot pass runs it after sub-jobs A and B, at a start and
// in the periodic ticks; retiredRun runs it alone, an hour after the start
// and hourly while a backlog lasts.
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
// snapshots (retiredRun), if gc.interval is longer, and how long after a
// run or a periodic tick that left a backlog it runs again (issue #14).
const retiredRunAfterStart = time.Hour

// phaseRetired labels retiredRun's gc_tick_deadline_reached line.
const phaseRetired = "retired"

// retiredRun runs sub-job C alone, retiredRunAfterStart after the start
// (Run), within max_tick_duration, and logs INFO gc_retired_run_complete.
// It reports backlog: C stopped at its deadline with retired snapshots
// left, so Run runs it again an hour later (issue #14: hourly while a
// backlog lasts); false when it drained them, failed, panicked or was
// stopped by ctx. A panic is recovered (gc_panicked, where=retired); the
// periodic ticks are not affected.
//
// AIDEV-NOTE: the final review's R3 #2. Sub-job C ran only in the periodic
// ticks (the startup pass skipped it, ruling PF-15, until issue #14's
// re-ruling: it runs C now, within its budget), and the first tick comes
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
// blob pass: the next start's or the next periodic tick's. C also deletes
// the displaced snapshots a periodic tick's sub-job B retired (DN-7 of the
// GC and upstream hardening plan): normally that tick's own C does, and
// this run takes those still retired when a tick's pass stopped before its
// C had deleted them (its deadline, a shutdown, or a batch or chunk that
// failed or panicked), which the startup pass's B no longer selects (its C
// does, within its budget). Since issue #14 the startup pass also runs C,
// within its snapshot deadline, so this run takes what that start's budget
// left and what was retired since.
// Since issue #14 it also repeats hourly while a backlog lasts (Run's
// AIDEV-NOTE): once a process could not keep up with a sustained
// displacement rate.
func (g *GC) retiredRun(ctx context.Context) (backlog bool) {
	defer g.recoverPanic("retired")
	start := time.Now()
	batchesRun := 0
	retired, deadlineHit, err := g.runSubJobC(ctx,
		g.snapshotDeadline(start.Add(g.cfg.MaxTickDuration), phaseRetired, &batchesRun), &batchesRun)
	// AIDEV-NOTE: passFailed's test: a run stopped by the end of ctx (a
	// shutdown) did not fail, whatever its error's shape.
	if err != nil && ctx.Err() == nil {
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
	// deadlineHit is false after a failed chunk or a shutdown
	// (runSubJobC): no hourly retry storm.
	return deadlineHit
}
