package gc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// representationPruneRows is the most stored upstream dates one tick's
// representation prune releases (runBlobPass), in one writer transaction.
//
// AIDEV-NOTE: the controller's ruling of 2026-11-06 on the prune's
// capacity. With gc.batch_size (100) a tick, 100 a day at the default
// gc.interval, a busy cache's inflow outran it (every index blob also lives
// under its by-hash URL, and new index versions arrive daily), the table
// grew without bound, and the scan's cost with it. Measured (the merge's
// prune task: 200,000 owned day-old dates plus 10,000 prunable, no -race):
// one 10,000-row transaction took 565-665 ms, where the scan alone takes
// about 550 ms; chunks of 100 took 52.9 s for the same 10,000 (101 calls,
// each rescanning). So one transaction, not chunks. Re-measure before
// raising it; no config key.
const representationPruneRows = 10_000

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
//     between batches with backlog remaining, or at a batch the writer
//     never started before the deadline, its start deadline (issue #14
//     Tier 1a: notStarted); the next tick picks up the rest),
//   - the count of os.Remove errors (other than ErrNotExist) — fed
//     into gc_run_complete.pool_unlink_errors,
//   - error iff a DB-level failure occurred (ctx cancellation alone
//     returns nil with deadline=false; the caller distinguishes via
//     ctx.Err()): a failed batch's, or else the representation prune's,
//     which does not stop the batches.
func (g *GC) runBlobPass(ctx context.Context, deadline time.Time, phase string) (blobResult, bool, int, error) {
	graceSeconds := int64(g.cfg.BlobGrace.Seconds())

	var (
		res           blobResult
		unlinkErrors  int
		batchesRun    int
		bytesThisTick int64
		// pruneErr is the representation prune's failure, the pass's
		// error unless a batch fails too.
		pruneErr error
	)

	// The representation prune, once a tick: here, at the start of the blob
	// pass, which runs in every tick (and in the startup pass), once, before
	// its first batch, and only while its deadline has not passed. One writer
	// transaction, at most representationPruneRows dates, oldest first.
	//
	// AIDEV-NOTE: the merge review's M-I1 (the maintainer, 2026-11-06).
	// main's prune ran inside every RunBlobGCBatch transaction, and its scan
	// costs 0.56-0.64 s at 200,000 url_path rows whatever it finds, so a
	// pass of 100 batches held the writer a minute longer and reaped less in
	// its share of the budget, the throughput issue #14's fix is about. Here,
	// not a sub-job of its own: the blob pass is the one pass every tick
	// reaches (issue #14), its deadline bounds the prune's start, and dates
	// pin no content (a reaped blob's go by ON DELETE CASCADE), so a prune
	// that falls behind costs rows, never disk or a pass; but rows that pile
	// up make every later scan slower, so one tick's capacity must stay far
	// above a busy cache's daily inflow (representationPruneRows' AIDEV-NOTE:
	// gc.batch_size, 100 a day at the defaults, was too low). A failed prune
	// does not stop the batches: they run, and the prune's error is the blob
	// pass's (one WARN gc_tick_failed for the pass, by runTick's failed,
	// ruling HD-15), unless a batch fails too, whose error it then is. So a
	// failed prune moves the rotation as a failed blob pass does, even when
	// the batches stopped at their deadline.
	//
	// Since 2026-11-09 (issue #14 Tier 1a, G2-RR1-M1) the prune and the
	// batches must also start by the pass's deadline (notStarted's
	// AIDEV-NOTE), so a prune queued behind a busy writer no longer runs
	// past it. A prune the writer never started before the deadline is no
	// failure (never pruneErr): it never ran, and the batches' deadline
	// check then ends the pass at its deadline.
	wctx := cache.WithWriteStartDeadline(ctx, deadline)
	if ctx.Err() == nil && time.Now().Before(deadline) {
		if _, err := g.store.PruneRepresentationMetadata(wctx, representationPruneRows); err != nil && !notStarted(err) {
			pruneErr = fmt.Errorf("representation metadata prune: %w", err)
		}
	}

	// reached is the pass's deadline branch: between batches, and for a
	// batch the writer never started before the deadline (notStarted).
	reached := func() (blobResult, bool, int, error) {
		g.cfg.Logger.Info("gc_tick_deadline_reached",
			"phase", phase,
			"which", "blob",
			"batches_completed", batchesRun,
			"bytes_reclaimed_this_tick", bytesThisTick,
		)
		return res, true, unlinkErrors, pruneErr
	}
	for {
		if err := ctx.Err(); err != nil {
			// Cooperative cancel — return what we have; not an error
			// for the caller (graceful shutdown). The caller checks
			// ctx.Err() if it cares about the distinction.
			return res, false, unlinkErrors, nil
		}
		if !time.Now().Before(deadline) {
			return reached()
		}

		reaped, err := g.cfg.Cache.RunBlobGCBatch(wctx, g.cfg.BatchSize, graceSeconds)
		if notStarted(err) {
			return reached() // the batch never ran: nothing to unlink
		}
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
			return res, false, unlinkErrors, pruneErr
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
