package gc

import (
	"context"
	"fmt"
	"time"
)

// runURLPathPass implements the URL-path TTL reap (SPEC4 §5 fourth
// class). Deletes url_path rows whose last_requested_at is older than
// gc.url_path_ttl and which are not vouched for by any current
// snapshot's package_hash. Each deletion decrements the matching
// blob's refcount in the same writer-tx so the subsequent same-tick
// blob pass sees the decremented values.
//
// Per-batch behavior mirrors the snapshot pass: SELECT-inside-tx +
// per-row DELETE + refcount UPDATE, batched at gc.batch_size, capped
// by the shared per-tick deadline.
//
// Returns the cumulative count of url_path rows reaped, a deadline-
// reached flag, and an error iff a DB-level failure occurred.
//
// AIDEV-NOTE: ordered FIRST in the tick (before snapshot + blob
// passes) so the refcount decrements this pass emits are visible to
// the same tick's blob pass, which means a url_path row reaped at
// 00:00:01 can produce a blob reap in the same tick rather than
// waiting a full gc.interval. Snapshot pass still runs second because
// snapshot DELETEs remove FK references the blob pass's NOT EXISTS
// reachability predicate consults — preserving the SPEC4 §9.6
// rationale for snapshot-before-blob.
func (g *GC) runURLPathPass(ctx context.Context, deadline time.Time, phase string, ttlSeconds, holdSeconds int64, maxVersions int) (int, bool, error) {
	if ttlSeconds <= 0 {
		return 0, false, nil
	}

	var (
		deleted    int // url_path rows actually reaped (feeds url_path_rows_reaped)
		stamped    int
		cleared    int
		batchesRun int
	)
	// Cursor over the url_path primary key (scheme, host, path). Each batch
	// scans rows strictly after this; advancing it guarantees the pass
	// terminates (every row visited once) even though most rows are no-ops.
	// It resumes from g.urlPathCursor (where a prior deadline-truncated tick
	// left off) and is reset to empty only once a pass fully drains.
	curScheme, curHost, curPath := g.urlPathCursor.scheme, g.urlPathCursor.host, g.urlPathCursor.path
	for {
		if err := ctx.Err(); err != nil {
			return deleted, false, nil
		}
		if !time.Now().Before(deadline) {
			// Persist the cursor so the next tick resumes here instead of
			// rescanning the prefix already visited this tick.
			g.urlPathCursor.scheme, g.urlPathCursor.host, g.urlPathCursor.path = curScheme, curHost, curPath
			g.cfg.Logger.Info("gc_tick_deadline_reached",
				"phase", phase,
				"which", "url_path",
				"batches_completed", batchesRun,
				"rows_reaped_this_tick", deleted,
				"rows_stamped_this_tick", stamped,
				"rows_cleared_this_tick", cleared,
			)
			return deleted, true, nil
		}

		res, err := g.cfg.Cache.RunURLPathGCBatch(ctx, g.cfg.BatchSize, ttlSeconds, holdSeconds, maxVersions, curScheme, curHost, curPath)
		if err != nil {
			return deleted, false, fmt.Errorf("url_path gc batch: %w", err)
		}
		batchesRun++
		deleted += res.Deleted
		stamped += res.Stamped
		cleared += res.Cleared

		// Scanned == 0 means the cursor reached the end of url_path — the
		// pass is drained; reset so the next tick starts a fresh full scan.
		if res.Scanned == 0 {
			g.urlPathCursor.scheme, g.urlPathCursor.host, g.urlPathCursor.path = "", "", ""
			return deleted, false, nil
		}
		curScheme, curHost, curPath = res.LastScheme, res.LastHost, res.LastPath
	}
}
