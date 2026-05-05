package freshness

import (
	"context"
	"time"
)

// minFastTickInterval is the floor for the periodic scheduler's tick
// interval. SPEC §7.4 prescribes a "fast tick" of periodic_refresh / 4;
// this floor keeps a misconfigured periodic_refresh of, say, 1 minute
// from spinning on the cache every 15s.
const minFastTickInterval = 5 * time.Second

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
