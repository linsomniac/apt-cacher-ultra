package handler

import "sync"

// sfResult is the value the leader of a singleflight call produces and
// every waiter on the same key reads.
//
// blobHash is the sha256 hex of the cached file after a successful
// fetch; on error it is empty. status is the upstream HTTP status (or 0
// when no response was received), used for X-Upstream-Status diagnostics.
//
// SPEC6_5 §2.3: when the post-fetch dispatch validated the body against
// a package_hash row's declared SHA256, validatedHash is set true and
// packageName carries that row's Package: column (empty for pdiff
// patches, which have no package name). Both fields are zero when no
// validation occurred (Phase 1 trust-upstream regime, or metadata).
type sfResult struct {
	blobHash      string
	size          int64
	status        int
	err           error
	validatedHash bool
	packageName   string
}

// sfCall is a single in-flight singleflight call. The leader populates
// res and signals wg; waiters block on wg.Wait then read res.
//
// waiters counts the number of joiners (shared==true returns) — read by
// the leader after fn returns to emit the SPEC §10 coalescing log line
// without a second mutex acquisition.
type sfCall struct {
	wg      sync.WaitGroup
	res     sfResult
	waiters int
}

// sfGroup coalesces concurrent calls for the same key into one execution
// of fn. The first caller (leader) runs fn; subsequent callers (waiters)
// block until fn returns and then receive the same result.
//
// AIDEV-NOTE: this is a tiny local implementation rather than a
// dependency on x/sync/singleflight. The only specialization we need
// over that package is sfResult — a typed, struct-shaped return that
// carries the blob hash, size, and upstream status. ~30 lines of code
// is not worth a module dependency.
type sfGroup struct {
	mu    sync.Mutex
	calls map[string]*sfCall
}

func newSFGroup() *sfGroup {
	return &sfGroup{calls: make(map[string]*sfCall)}
}

// sfTestHookAfterDone, if non-nil, is invoked between the leader's
// wg.Done and the map-entry deletion. Tests use this to deterministically
// exercise the cleanup window: a caller arriving during this hook proves
// that delete-before-Done would cause a duplicate fetch. Production code
// pays one nil check per leader call.
var sfTestHookAfterDone func()

// Do runs fn under key. If another call is already in flight for the
// same key, it blocks for the in-flight result and returns it with
// shared=true. Otherwise it runs fn, returns its result with
// shared=false (and waiters set to the number of joiners), and removes
// the call from the in-flight map so the next caller can lead.
//
// AIDEV-NOTE: ordering matters. We must signal wg.Done before deleting
// the map entry, not after. The previous order (delete first, Done
// second) had a race: a caller arriving between delete and Done would
// see no in-flight call and run fn a second time — defeating the
// coalescing guarantee. With Done first, that same caller still finds
// the (just-finished) call in the map, joins it via wg.Wait (which
// returns immediately), and reads the same result.
func (g *sfGroup) Do(key string, fn func() sfResult) (res sfResult, shared bool, waiters int) {
	g.mu.Lock()
	if c, ok := g.calls[key]; ok {
		c.waiters++
		g.mu.Unlock()
		c.wg.Wait()
		return c.res, true, 0
	}
	c := &sfCall{}
	c.wg.Add(1)
	g.calls[key] = c
	g.mu.Unlock()

	c.res = fn()
	c.wg.Done()
	if hook := sfTestHookAfterDone; hook != nil {
		hook()
	}

	g.mu.Lock()
	delete(g.calls, key)
	leaderWaiters := c.waiters
	g.mu.Unlock()
	return c.res, false, leaderWaiters
}
