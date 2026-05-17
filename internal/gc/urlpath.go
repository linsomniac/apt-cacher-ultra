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
func (g *GC) runURLPathPass(ctx context.Context, deadline time.Time, phase string, ttlSeconds int64) (int, bool, error) {
	if ttlSeconds <= 0 {
		return 0, false, nil
	}

	var (
		acc        int
		batchesRun int
	)
	for {
		if err := ctx.Err(); err != nil {
			return acc, false, nil
		}
		if !time.Now().Before(deadline) {
			g.cfg.Logger.Info("gc_tick_deadline_reached",
				"phase", phase,
				"which", "url_path",
				"batches_completed", batchesRun,
				"rows_reaped_this_tick", acc,
			)
			return acc, true, nil
		}

		n, err := g.cfg.Cache.RunURLPathGCBatch(ctx, g.cfg.BatchSize, ttlSeconds)
		if err != nil {
			return acc, false, fmt.Errorf("url_path gc batch: %w", err)
		}
		batchesRun++
		acc += n

		if n == 0 {
			return acc, false, nil
		}
	}
}
