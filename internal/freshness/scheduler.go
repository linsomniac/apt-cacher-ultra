package freshness

import (
	"context"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// minFastTickInterval is the floor for the periodic scheduler's tick
// interval. SPEC §7.4 prescribes a "fast tick" of periodic_refresh / 4;
// this floor keeps a misconfigured periodic_refresh of, say, 1 minute
// from spinning on the cache every 15s.
const minFastTickInterval = 5 * time.Second

// staleSuiteFactor multiplies periodic_refresh to set the "this suite has
// gone silently stale" threshold for the SPEC §7.4 freshness_stale_suites
// gauge. A healthy suite refreshes every periodic_refresh; 4× gives ample
// slack for transient upstream trouble before a suite is flagged. The
// production freeze went undetected for a week precisely because no such
// signal existed.
const staleSuiteFactor = 4

// staleSuiteCounts partitions suites whose last_success_at has aged past
// staleBefore into adopted (current_snapshot_id set — the dangerous case
// that serves stale metadata) and unadopted. A nil last_success_at is
// treated as "never succeeded" rather than "went stale" and is not
// counted, so brand-new suites don't trip the gauge. SPEC §7.4.
func staleSuiteCounts(suites []cache.SuiteFreshness, staleBefore int64) (adopted, unadopted int) {
	for _, s := range suites {
		if s.LastSuccessAt == nil || *s.LastSuccessAt >= staleBefore {
			continue
		}
		if s.CurrentSnapshotID != nil {
			adopted++
		} else {
			unadopted++
		}
	}
	return adopted, unadopted
}

// Run starts the SPEC §7.4 periodic refresh loop. It returns when ctx
// is cancelled. Refresh = 0 disables periodic refresh (Run returns
// immediately).
//
// Each tick scans suite_freshness and attempts a freshness check on any
// suite whose last_success_at is older than refresh. The cooldown gate
// inside Check deduplicates against any T1 check that fired since the
// previous tick. Per-suite checks run sequentially within a tick — the
// upstream-side bound (per-host concurrency in the fetch path) keeps
// this path well-behaved without a separate worker pool.
func (c *Checker) Run(ctx context.Context) {
	if c.refresh <= 0 {
		return
	}
	interval := c.refresh / 4
	if interval < minFastTickInterval {
		interval = minFastTickInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.tick(ctx)
		}
	}
}

// tick performs one pass of the periodic scheduler. Exported via Run;
// also called directly by tests to deterministically drive a single
// pass without waiting on a ticker.
func (c *Checker) tick(ctx context.Context) {
	suites, err := c.cache.ListSuites(ctx)
	if err != nil {
		c.logger.Warn("freshness: list suites failed", "err", err)
		return
	}
	// SPEC §7.4 observability: surface suites that have gone silently
	// stale (last_success_at past staleSuiteFactor*refresh). An adopted
	// stale suite is the freeze signature — it keeps serving its snapshot
	// while never re-checking upstream. The gauge is absolute per tick.
	staleBefore := c.now().Add(-staleSuiteFactor * c.refresh).Unix()
	adoptedStale, unadoptedStale := staleSuiteCounts(suites, staleBefore)
	freshnessStaleSuites.Set(float64(adoptedStale), "true")
	freshnessStaleSuites.Set(float64(unadoptedStale), "false")
	if adoptedStale > 0 {
		c.logger.Warn("freshness: adopted suites stale beyond threshold",
			"adopted_stale", adoptedStale,
			"stale_factor", staleSuiteFactor,
			"refresh", c.refresh.String(),
		)
	}

	threshold := c.now().Add(-c.refresh).Unix()
	for _, s := range suites {
		// Already-fresh suites (last_success_at within refresh window)
		// are skipped. The cooldown gate inside Check still applies
		// to anything we do attempt here.
		if s.LastSuccessAt != nil && *s.LastSuccessAt > threshold {
			continue
		}
		// Honor ctx cancellation between suites — Run is scoped to the
		// cache lifecycle and a long iteration over many suites must
		// not extend the shutdown window.
		if ctx.Err() != nil {
			return
		}
		c.Check(ctx, s.CanonicalScheme, s.CanonicalHost, s.SuitePath)
	}
}
