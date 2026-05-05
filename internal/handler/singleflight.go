package handler

import "sync"

// sfResult is the value the leader of a singleflight call produces and
// every waiter on the same key reads.
//
// blobHash is the sha256 hex of the cached file after a successful
// fetch; on error it is empty. status is the upstream HTTP status (or 0
// when no response was received), used for X-Upstream-Status diagnostics.
type sfResult struct {
	blobHash string
	size     int64
	status   int
	err      error
}

// sfCall is a single in-flight singleflight call. The leader populates
// res and signals wg; waiters block on wg.Wait then read res.
type sfCall struct {
	wg  sync.WaitGroup
	res sfResult
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

// Do runs fn under key. If another call is already in flight for the
// same key, it blocks for the in-flight result and returns it with
// shared=true. Otherwise it runs fn, returns its result with
// shared=false, and removes the call from the in-flight map so the
// next caller can lead.
func (g *sfGroup) Do(key string, fn func() sfResult) (res sfResult, shared bool) {
	g.mu.Lock()
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.res, true
	}
	c := &sfCall{}
	c.wg.Add(1)
	g.calls[key] = c
	g.mu.Unlock()

	c.res = fn()

	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()
	c.wg.Done()
	return c.res, false
}
