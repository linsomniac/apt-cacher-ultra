package tlsmitm

import (
	"crypto/tls"
	"errors"
	"fmt"
	"sync"
	"time"
)

// EvictReason names a §10.2 mitm_cert_cache_evicted Info reason. The
// strings exactly match the spec's enum values.
type EvictReason string

const (
	EvictReasonLRU     EvictReason = "lru"
	EvictReasonExpired EvictReason = "expired"
)

// EvictHook is fired by the cache after a cert entry is removed. The
// hook runs OUTSIDE the cache mutex; implementations should not take
// any lock that the gen function might also take.
type EvictHook func(host string, reason EvictReason, ageSeconds float64)

// GenFunc is the cert-generation callback the cache invokes on miss.
// It is called per literal host (the lower-cased, IDNA-normalized
// CONNECT target — SPEC6 §5.1.3) and is expected to return a fully
// constructed *tls.Certificate or an error. Errors are propagated to
// every singleflight waiter and NOT cached; the next call retries.
type GenFunc func(host string) (*tls.Certificate, error)

// Cache is the bounded LRU + per-host singleflight leaf cert cache
// SPEC6 §5.1.3 / §9.1 specifies. The zero value is unusable; build
// one with NewCache.
//
// Concurrency: all operations are safe for concurrent use. The
// generation function is invoked outside the cache mutex so two
// CONNECTs to two different hosts never serialize on cert generation.
type Cache struct {
	mu       sync.Mutex
	capacity int
	items    map[string]*entry
	// Doubly-linked list with sentinel head/tail. head.next is the
	// most-recently-used entry; tail.prev is the least-recently-used.
	head entry
	tail entry

	// gen is the cert-generation callback. Bound at construction.
	gen GenFunc

	// onEvict, when non-nil, fires after each eviction. Set via
	// SetOnEvict; mutated under mu so reads can use the value safely.
	onEvict EvictHook

	// now is the wall-clock source for expiry checks. Bound at
	// construction; tests inject a controllable clock.
	now func() time.Time

	// sf coalesces concurrent generations of the same host so a
	// thundering herd of CONNECTs to a single host issues exactly one
	// cert per cache miss.
	sf sfGroup
}

type entry struct {
	host       string
	cert       *tls.Certificate
	insertedAt time.Time
	prev, next *entry
}

// NewCache returns a Cache with the given LRU capacity (entries) and
// generation callback. Capacity must be ≥ 1; gen must be non-nil.
//
// The wall-clock source defaults to time.Now and may be overridden by
// SetClockForTest.
func NewCache(capacity int, gen GenFunc) (*Cache, error) {
	if capacity < 1 {
		return nil, fmt.Errorf("tlsmitm: cache capacity must be ≥ 1, got %d", capacity)
	}
	if gen == nil {
		return nil, errors.New("tlsmitm: nil GenFunc")
	}
	c := &Cache{
		capacity: capacity,
		items:    make(map[string]*entry, capacity),
		gen:      gen,
		now:      time.Now,
		sf:       sfGroup{calls: make(map[string]*sfCall)},
	}
	c.head.next = &c.tail
	c.tail.prev = &c.head
	return c, nil
}

// SetOnEvict installs (or replaces) the eviction callback. Pass nil to
// disable. The hook fires AFTER the entry is removed and outside the
// cache mutex.
func (c *Cache) SetOnEvict(h EvictHook) {
	c.mu.Lock()
	c.onEvict = h
	c.mu.Unlock()
}

// SetClockForTest overrides the wall-clock source. Tests that need to
// drive expiry deterministically use this; production code does not.
func (c *Cache) SetClockForTest(now func() time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

// Capacity returns the configured LRU capacity.
func (c *Cache) Capacity() int { return c.capacity }

// Size returns the current number of entries in the cache.
func (c *Cache) Size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// Get returns a cached cert for `host`, generating one on miss. On a
// hit the entry is moved to the most-recently-used position. Expired
// entries are evicted lazily on lookup and regenerated.
//
// Concurrent Get calls for the SAME host coalesce — exactly one
// invocation of GenFunc runs across them — but two different hosts
// generate concurrently.
func (c *Cache) Get(host string) (*tls.Certificate, error) {
	if host == "" {
		return nil, errors.New("tlsmitm: cache Get: empty host")
	}

	// Fast hit path under the mutex.
	c.mu.Lock()
	if e, ok := c.items[host]; ok {
		if !c.isExpiredLocked(e) {
			c.touchLocked(e)
			cert := e.cert
			c.mu.Unlock()
			return cert, nil
		}
		hook, host, age := c.detachLocked(e, EvictReasonExpired)
		c.mu.Unlock()
		if hook != nil {
			hook(host, EvictReasonExpired, age)
		}
	} else {
		c.mu.Unlock()
	}

	// Miss path — singleflight per host.
	cert, err := c.sf.Do(host, func() (*tls.Certificate, error) {
		// Re-check under the lock: another goroutine may have populated
		// the entry while we were waiting on the singleflight.
		c.mu.Lock()
		if e, ok := c.items[host]; ok && !c.isExpiredLocked(e) {
			c.touchLocked(e)
			cert := e.cert
			c.mu.Unlock()
			return cert, nil
		}
		c.mu.Unlock()

		cert, err := c.gen(host)
		if err != nil {
			return nil, err
		}
		c.insert(host, cert)
		return cert, nil
	})
	return cert, err
}

// insert adds a freshly-generated cert under host, evicting the LRU
// entry if the cache is at capacity. Fires onEvict outside the mutex.
func (c *Cache) insert(host string, cert *tls.Certificate) {
	c.mu.Lock()

	// Replace existing entry (rare — only happens if a re-check missed
	// a concurrent insert). Drop the old one so the LRU bookkeeping
	// stays consistent.
	if old, ok := c.items[host]; ok {
		c.unlinkLocked(old)
		delete(c.items, host)
	}

	var hook EvictHook
	var evictedHost string
	var evictedAge float64
	var evicted bool
	if len(c.items) >= c.capacity {
		victim := c.tail.prev
		if victim != &c.head {
			hook, evictedHost, evictedAge = c.detachLocked(victim, EvictReasonLRU)
			evicted = true
		}
	}

	e := &entry{
		host:       host,
		cert:       cert,
		insertedAt: c.now(),
	}
	c.items[host] = e
	c.pushFrontLocked(e)
	c.mu.Unlock()

	if evicted && hook != nil {
		hook(evictedHost, EvictReasonLRU, evictedAge)
	}
}

// detachLocked removes `e` from the LRU list and the items map, and
// returns the eviction hook plus its arguments so the caller can fire
// it outside the mutex. Caller must hold c.mu.
func (c *Cache) detachLocked(e *entry, _ EvictReason) (EvictHook, string, float64) {
	c.unlinkLocked(e)
	delete(c.items, e.host)
	age := c.now().Sub(e.insertedAt).Seconds()
	if age < 0 {
		age = 0
	}
	return c.onEvict, e.host, age
}

func (c *Cache) unlinkLocked(e *entry) {
	if e.prev != nil {
		e.prev.next = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	}
	e.prev = nil
	e.next = nil
}

// pushFrontLocked inserts `e` immediately after the head sentinel.
func (c *Cache) pushFrontLocked(e *entry) {
	e.prev = &c.head
	e.next = c.head.next
	c.head.next.prev = e
	c.head.next = e
}

// touchLocked moves `e` to the most-recently-used position.
func (c *Cache) touchLocked(e *entry) {
	c.unlinkLocked(e)
	c.pushFrontLocked(e)
}

// isExpiredLocked reports whether `e`'s cert is past its NotAfter
// according to the cache's clock source.
func (c *Cache) isExpiredLocked(e *entry) bool {
	if e.cert == nil || e.cert.Leaf == nil {
		return true
	}
	return c.now().After(e.cert.Leaf.NotAfter)
}

// sfCall is one in-flight singleflight execution.
//
// Mirrors the design of internal/handler/singleflight.go but typed for
// *tls.Certificate so the cache doesn't carry a generic-interface
// boxing on the hot path.
type sfCall struct {
	wg     sync.WaitGroup
	result *tls.Certificate
	err    error
}

type sfGroup struct {
	mu    sync.Mutex
	calls map[string]*sfCall
}

// Do coalesces concurrent calls under `key`. The first caller (the
// leader) runs fn; subsequent callers (waiters) block until fn returns
// and read the same result. Failures are NOT cached — once Do returns,
// the call is removed from the in-flight map and the next caller leads
// a fresh execution.
//
// Done-before-delete ordering: a waiter that arrives between
// wg.Done() and the map delete still observes the just-finished call
// (because we publish the result before deleting), so coalescing is
// guaranteed even at the race boundary.
//
// Panic-safety: a panic inside fn is recovered and surfaced as an
// error to every waiter. Without this, a panicking gen would leave
// wg.Done unreached and the call entry in the map, wedging every
// future Get for `key` on wg.Wait() forever — a per-host DoS from
// one bad generation path.
func (g *sfGroup) Do(key string, fn func() (*tls.Certificate, error)) (cert *tls.Certificate, err error) {
	g.mu.Lock()
	if call, ok := g.calls[key]; ok {
		g.mu.Unlock()
		call.wg.Wait()
		return call.result, call.err
	}
	call := &sfCall{}
	call.wg.Add(1)
	g.calls[key] = call
	g.mu.Unlock()

	defer func() {
		if r := recover(); r != nil {
			cert = nil
			err = fmt.Errorf("tlsmitm: cert generation panicked: %v", r)
		}
		// Publish the result BEFORE wg.Done so waiters that wake
		// immediately read the just-finished values, and BEFORE
		// the map delete so a caller arriving in the wg.Done →
		// delete window still observes the finished call.
		call.result, call.err = cert, err
		call.wg.Done()

		g.mu.Lock()
		delete(g.calls, key)
		g.mu.Unlock()
	}()

	cert, err = fn()
	return
}
