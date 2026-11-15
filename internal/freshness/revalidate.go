package freshness

// The checker's side of request-time revalidation
// (freshness.revalidate_wait; the 2026-11-14 publication-delay review).
//
// A suite's anchor (InRelease, Release) used to be served from the current
// snapshot first, and the client-triggered check ran after the response:
// the first `apt update` after an upstream publication always got the
// previous release, however long ago the cache had last asked upstream (up
// to periodic_refresh, plus the scheduler's tick, on a quiet cache), and
// only that request made the cache notice. The handler now runs the same
// check before it serves an anchor and waits for it — and, when the check
// finds a new release, for its adoption — for at most revalidate_wait.
// The check is the client-triggered check in every other respect: the
// cooldown gates it, and it goes on by itself when the request stops
// waiting. Nothing here weakens what is served: a request that does not
// wait long enough gets the current verified snapshot, as before.
//
// The checker contributes two things: how a check ended (CheckNow), and a
// way to wait for the check or adoption in flight on a suite (AwaitSuite),
// which suiteActivity tracks.

import (
	"context"
	"sync"
	"time"
)

// suiteActivity is one check of a suite, from the moment it holds the
// suite's mutex, together with the adoption it starts: what a request for
// the suite's anchor may wait for. A repair is not part of it — a repair
// publishes nothing a request could be waiting for.
type suiteActivity struct {
	started time.Time     // monotonic
	done    chan struct{} // closed when the check, and its adoption if it started one, has ended

	mu        sync.Mutex
	result    string // how the check ended: its freshness_check result, or failed; "" while it runs
	published bool   // the adoption the check started committed a new snapshot
}

func (a *suiteActivity) setResult(result string) {
	a.mu.Lock()
	a.result = result
	a.mu.Unlock()
}

func (a *suiteActivity) setPublished() {
	a.mu.Lock()
	a.published = true
	a.mu.Unlock()
}

func (a *suiteActivity) outcome() (result string, published bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.result, a.published
}

// beginActivity records that a check of the suite key holds its mutex.
// The caller holds that mutex, so there is at most one activity per suite.
func (c *Checker) beginActivity(key string) *suiteActivity {
	act := &suiteActivity{started: time.Now(), done: make(chan struct{})}
	c.active.Store(key, act)
	return act
}

// endActivity ends act: requests waiting on it go on. It is called after
// the suite's mutex is released — by check for a check that started no
// adoption, by the adoption goroutine otherwise — so a request that wakes
// reads a snapshot pointer nothing is about to switch.
func (c *Checker) endActivity(key string, act *suiteActivity) {
	c.active.CompareAndDelete(key, act)
	close(act.done)
}

// CheckNow runs one freshness check of the suite, exactly as Check does,
// and returns how it ended: the result its freshness_check line logged
// (not_modified, unchanged, changed, changed_queued, changed_deferred, …),
// or busy (another goroutine holds the suite), skipped (the cooldown, or
// nothing to check) or failed. A result of changed, form_changed or
// first_adoption means an adoption was started; AwaitSuite waits for it.
func (c *Checker) CheckNow(ctx context.Context, scheme, host, suitePath string) string {
	return c.check(ctx, SuiteRef{CanonicalScheme: scheme, CanonicalHost: host, SuitePath: suitePath}, checkOpts{}).result
}

// AwaitSuite waits while a check of the suite, or the adoption a check
// started, is in flight — for a request that arrived at start and may wait
// for wait at most — and reports what it waited for: check is that check's
// result ("" when nothing was in flight, or the check had not ended),
// published whether its adoption committed a new snapshot, and settled
// whether it had ended by the time AwaitSuite returned.
//
// The wait ends at start + wait, when ctx ends, and — for a request that
// arrives late — once the activity itself is wait old.
//
// AIDEV-NOTE: the second bound is what keeps a slow adoption from taxing
// every request. An adoption that needs longer than wait publishes too
// late for any request to see; without the bound each anchor request that
// arrived during it would still wait its full wait and then be served the
// old snapshot. With it only the requests of the adoption's first wait
// seconds wait at all. The requests that do not wait are served the old
// snapshot and register with the flip barrier, as before.
func (c *Checker) AwaitSuite(ctx context.Context, scheme, host, suitePath string, start time.Time, wait time.Duration) (check string, published, settled bool) {
	v, ok := c.active.Load(suiteKey(scheme, host, suitePath))
	if !ok {
		return "", false, true
	}
	act := v.(*suiteActivity)
	until := start.Add(wait)
	if byAge := act.started.Add(wait); byAge.Before(until) {
		until = byAge
	}
	if d := time.Until(until); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-act.done:
			settled = true
		case <-t.C:
		case <-ctx.Done():
		}
	} else {
		select {
		case <-act.done:
			settled = true
		default:
		}
	}
	check, published = act.outcome()
	return check, published, settled
}
