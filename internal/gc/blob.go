package gc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// runBlobPass implements SPEC4 §9.6.2: a per-tick batched reap loop
// that walks the candidate set in oldest-first order
// (ORDER BY refcount_zeroed_at). Each batch is one writer-tx with
// SELECT + DELETE...RETURNING + buffer; os.Remove unlinks happen
// post-COMMIT, outside any DB lock.
//
// Returns:
//   - the cumulative reap count (rows DELETEd this pass) and bytes
//     reclaimed (from the DELETE's RETURNING clause; equals the sum
//     of `blob.size` for the rows actually removed),
//   - whether the deadline tripped (true means the pass exited
//     between batches with backlog remaining; the next tick picks
//     up the rest),
//   - the count of os.Remove errors (other than ErrNotExist) — fed
//     into gc_run_complete.pool_unlink_errors,
//   - error iff a DB-level failure occurred (ctx cancellation alone
//     returns nil with deadline=false; the caller distinguishes via
//     ctx.Err()).
func (g *GC) runBlobPass(ctx context.Context, deadline time.Time, phase string) (blobResult, bool, int, error) {
	graceSeconds := int64(g.cfg.BlobGrace.Seconds())

	var (
		res            blobResult
		unlinkErrors   int
		batchesRun     int
		bytesThisTick  int64
		deadlineHit    bool
	)
	for {
		if err := ctx.Err(); err != nil {
			// Cooperative cancel — return what we have; not an error
			// for the caller (graceful shutdown). The caller checks
			// ctx.Err() if it cares about the distinction.
			return res, false, unlinkErrors, nil
		}
		if !time.Now().Before(deadline) {
			deadlineHit = true
			g.cfg.Logger.Info("gc_tick_deadline_reached",
				"phase", phase,
				"which", "blob",
				"batches_completed", batchesRun,
				"bytes_reclaimed_this_tick", bytesThisTick,
			)
			return res, true, unlinkErrors, nil
		}

		reaped, err := g.cfg.Cache.RunBlobGCBatch(ctx, g.cfg.BatchSize, graceSeconds)
		if err != nil {
			// A DB error on one batch surfaces and the pass exits.
			// Subsequent ticks will retry. Don't try to keep going
			// — a persistent error here is a signal the operator
			// needs to see.
			return res, deadlineHit, unlinkErrors, fmt.Errorf("blob gc batch: %w", err)
		}
		batchesRun++
		if len(reaped) == 0 {
			// Candidate set is drained.
			return res, deadlineHit, unlinkErrors, nil
		}

		// Post-COMMIT unlink loop. SPEC4 §9.6.2 buffer-close-commit-
		// unlink ordering: the only information source for which
		// files to remove is the DELETE's RETURNING result, which
		// the cache layer captured before the COMMIT.
		for _, r := range reaped {
			// Defense-in-depth: hashes here came from the DB
			// blob.hash column whose CHECK constraint pins it to
			// 64-char lowercase hex, but a corrupt DB or a future
			// schema change that weakened the CHECK could let a
			// malformed hash through to the unlink path. Skip and
			// log rather than building a path that could escape
			// pool/<prefix>/.
			if !validHashLite(r.Hash) {
				g.cfg.Logger.Warn("gc_pool_unlink_skipped_invalid_hash",
					"hash", r.Hash,
					"size", r.Size,
				)
				continue
			}
			path := g.poolPath(r.Hash)
			if err := os.Remove(path); err != nil {
				if !errors.Is(err, fs.ErrNotExist) {
					g.cfg.Logger.Warn("gc_pool_unlink_failed",
						"hash", r.Hash,
						"err", err.Error(),
						"operation", "reap",
					)
					unlinkErrors++
					continue
				}
				// ENOENT is benign — file already absent. The DB
				// row is gone now; the disk catches up.
			}
			res.count++
			res.bytes += r.Size
			bytesThisTick += r.Size
		}
	}
}
