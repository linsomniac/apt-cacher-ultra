package gc

import (
	"context"
	"fmt"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// runSnapshotPass implements SPEC4 §9.6.3: a per-tick batched reap
// loop over orphan candidate snapshots (sub-job A) ∪ displaced
// snapshots beyond keep-N (sub-job B). Each batch is a single writer-
// tx that runs SELECT-inside-tx + cascade DELETE; the SELECT-DELETE
// liveness race is closed by single-writer ordering (heartbeat /
// CommitAdoption / InsertCandidateSnapshot writes serialize through
// the same writer goroutine — they cannot interleave between SELECT
// and DELETE inside this tx).
//
// Returns:
//   - the cumulative SnapshotGCBatchResult (orphan + displaced
//     counts),
//   - whether the deadline tripped (true means the pass exited
//     between batches with backlog remaining; the next tick picks
//     up the rest, AND the same tick's blob pass receives an
//     already-expired deadline and exits immediately),
//   - error iff a DB-level failure occurred.
func (g *GC) runSnapshotPass(ctx context.Context, deadline time.Time, phase string) (cache.SnapshotGCBatchResult, bool, error) {
	staleGraceSeconds := int64(g.cfg.HeartbeatStaleGrace.Seconds())

	var (
		acc         cache.SnapshotGCBatchResult
		batchesRun  int
		deadlineHit bool
	)
	for {
		if err := ctx.Err(); err != nil {
			return acc, false, nil
		}
		if !time.Now().Before(deadline) {
			deadlineHit = true
			g.cfg.Logger.Info("gc_tick_deadline_reached",
				"phase", phase,
				"which", "snapshot",
				"batches_completed", batchesRun,
				"bytes_reclaimed_this_tick", int64(0),
			)
			return acc, true, nil
		}

		batch, err := g.cfg.Cache.RunSnapshotGCBatch(ctx,
			g.cfg.SnapshotBatchSize,
			staleGraceSeconds,
			g.cfg.KeepDisplaced,
		)
		if err != nil {
			return acc, deadlineHit, fmt.Errorf("snapshot gc batch: %w", err)
		}
		batchesRun++
		acc.OrphanReaped += batch.OrphanReaped
		acc.DisplacedReaped += batch.DisplacedReaped

		if batch.Total() == 0 {
			// Candidate set drained.
			return acc, deadlineHit, nil
		}
	}
}
