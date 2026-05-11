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
		res           blobResult
		unlinkErrors  int
		batchesRun    int
		bytesThisTick int64
	)
	for {
		if err := ctx.Err(); err != nil {
			// Cooperative cancel — return what we have; not an error
			// for the caller (graceful shutdown). The caller checks
			// ctx.Err() if it cares about the distinction.
			return res, false, unlinkErrors, nil
		}
		if !time.Now().Before(deadline) {
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
			return res, false, unlinkErrors, fmt.Errorf("blob gc batch: %w", err)
		}
		batchesRun++
		if len(reaped) == 0 {
			// Candidate set is drained.
			return res, false, unlinkErrors, nil
		}

		// SPEC4 §10.2: blobs_reaped/bytes_reclaimed count the rows
		// that RunBlobGCBatch's COMMIT removed from the blob table.
		// Tally now — independent of the post-COMMIT unlink result
		// — so that an unlink failure (which leaks a pool/ file but
		// does NOT resurrect the row) is reported truthfully:
		// blobs_reaped names what is gone from the DB, and
		// pool_unlink_errors names the disk-side leak the next pool
		// scan will repair.
		for _, r := range reaped {
			res.count++
			res.bytes += r.Size
			bytesThisTick += r.Size
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
			// pool/<prefix>/. The blob row is already gone (tallied
			// above); the file is leaked until the next §9.6.4
			// pool scan reaps it. Count this toward
			// pool_unlink_errors — `gc_run_complete` reports the
			// disk-side leak count, and a skipped unlink is a leak
			// just like an os.Remove that returned EIO.
			if !validHashLite(r.Hash) {
				g.cfg.Logger.Warn("gc_pool_unlink_skipped_invalid_hash",
					"hash", r.Hash,
					"size", r.Size,
				)
				unlinkErrors++
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
				}
				// ENOENT is benign — file already absent. The DB
				// row is gone now; the disk catches up.
			}
		}
	}
}
