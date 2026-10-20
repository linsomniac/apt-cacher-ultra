package gc

import (
	"context"
	"fmt"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// snapshotPassResult counts one snapshot pass's reaps by class.
type snapshotPassResult struct {
	cache.SnapshotGCBatchResult
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
// most 10,000 rows per writer transaction.
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
	expired := func() bool {
		if time.Now().Before(deadline) {
			return false
		}
		g.cfg.Logger.Info("gc_tick_deadline_reached",
			"phase", phase,
			"which", "snapshot",
			"batches_completed", batchesRun,
			"bytes_reclaimed_this_tick", int64(0),
		)
		return true
	}
	for {
		if err := ctx.Err(); err != nil {
			return acc, false, nil
		}
		if expired() {
			return acc, true, nil
		}

		batch, err := g.cfg.Cache.RunSnapshotGCBatch(ctx,
			g.cfg.SnapshotBatchSize,
			staleGraceSeconds,
			g.cfg.KeepDisplaced,
		)
		if err != nil {
			return acc, false, fmt.Errorf("snapshot gc batch: %w", err)
		}
		batchesRun++
		acc.OrphanReaped += batch.OrphanReaped
		acc.DisplacedReaped += batch.DisplacedReaped

		if batch.Total() == 0 {
			break // A ∪ B drained
		}
	}
	// Sub-job C (spec C9).
	for {
		if err := ctx.Err(); err != nil {
			return acc, false, nil
		}
		if expired() {
			return acc, true, nil
		}
		reaped, more, err := g.cfg.Cache.RunRetiredSnapshotGCChunk(ctx, 0)
		if err != nil {
			return acc, false, fmt.Errorf("retired snapshot gc chunk: %w", err)
		}
		batchesRun++
		acc.retired += reaped
		if !more {
			return acc, false, nil
		}
	}
}
