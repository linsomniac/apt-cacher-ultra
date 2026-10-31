package cache

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"
)

// touchEvery is spec C3's touch coalescing: at most one last_requested_at
// write per suite per hour.
const touchEvery = time.Hour

// touchSweepAt is the coalescing map size at which entries older than
// touchEvery (which no longer suppress anything) are dropped — at most once
// per touchSweepEvery, unless the map has doubled since the last sweep
// (design review M9: a sweep is O(map) under the touch mutex).
const (
	touchSweepAt    = 4096
	touchSweepEvery = time.Minute
)

// suiteTouches is the in-memory coalescing state of TouchSuite: when each
// tracked suite's last_requested_at was last written, in monotonic time.
type suiteTouches struct {
	mu   sync.Mutex
	last map[SuiteKey]time.Time
	// swept is when the map was last swept, and kept how many entries the
	// sweep left (the doubling rule).
	swept time.Time
	kept  int
}

// TouchSuite records client interest in a suite (spec C3): it sets
// last_requested_at = now on the suite's existing row (UPDATE-only), at
// most once per suite per hour. A touch within the hour of the previous
// write, or of the row's creation, writes nothing. The coalescing map
// holds only suites whose row the write found (and, for the length of one
// write, a suite being touched); it is purged when this cache deletes a
// suite row.
//
// AIDEV-NOTE: suite_freshness.last_requested_at is the SUITE's interest
// stamp (eager-adoption window, eviction, reaping), distinct from
// url_path.last_requested_at; only TouchSuite and the row's creation write
// it, from client-serving paths. Coalescing uses monotonic time, so a wall
// clock step neither suppresses nor multiplies writes. Residual (design
// review M11, ruling PD-11): after a forward wall-clock step of about 23 h
// or more, an active suite keeps its pre-step stamp until its next due
// touch, up to touchEvery later, and meanwhile — without a current
// snapshot — looks idle for 24 h to cap eviction; bounded by the hour and
// self-healing.
func (c *Cache) TouchSuite(ctx context.Context, key SuiteKey) error {
	if !c.claimTouch(key) {
		return nil
	}
	now := nowUnix()
	found := false
	err := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `
UPDATE suite_freshness SET last_requested_at = ?
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`,
			now, key.CanonicalScheme, key.CanonicalHost, key.SuitePath)
		if err != nil {
			return fmt.Errorf("TouchSuite: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("TouchSuite: rows: %w", err)
		}
		found = n > 0
		return nil
	})
	// Never read found after an error: the operation may still run. A
	// failed touch, or one that found no row, leaves no entry, so the next
	// serve tries again and an untracked suite never stays in the map.
	if err != nil || !found {
		c.forgetTouch(key)
		return err
	}
	return nil
}

// TouchedSuites is the size of the touch-coalescing map (tests and
// diagnostics).
func (c *Cache) TouchedSuites() int {
	c.touches.mu.Lock()
	defer c.touches.mu.Unlock()
	return len(c.touches.last)
}

// claimTouch reports whether key's touch is due — none written within
// touchEvery — and if so records it now, so concurrent touches of one
// suite write once.
func (c *Cache) claimTouch(key SuiteKey) bool {
	c.touches.mu.Lock()
	defer c.touches.mu.Unlock()
	if t, ok := c.touches.last[key]; ok && time.Since(t) < touchEvery {
		return false
	}
	c.noteTouchLocked(key)
	return true
}

// noteTouch records that key's last_requested_at was just written (by its
// creation).
func (c *Cache) noteTouch(key SuiteKey) {
	c.touches.mu.Lock()
	defer c.touches.mu.Unlock()
	c.noteTouchLocked(key)
}

func (c *Cache) noteTouchLocked(key SuiteKey) {
	if c.touches.last == nil {
		c.touches.last = make(map[SuiteKey]time.Time)
	}
	if n := len(c.touches.last); n >= touchSweepAt &&
		(n >= 2*c.touches.kept || time.Since(c.touches.swept) >= touchSweepEvery) {
		for k, t := range c.touches.last {
			if time.Since(t) >= touchEvery {
				delete(c.touches.last, k)
			}
		}
		c.touches.swept, c.touches.kept = time.Now(), len(c.touches.last)
	}
	c.touches.last[key] = time.Now()
}

// forgetTouch drops key's entry: its row is gone, or its touch failed.
func (c *Cache) forgetTouch(key SuiteKey) {
	c.touches.mu.Lock()
	defer c.touches.mu.Unlock()
	delete(c.touches.last, key)
}
