// Package hostsem provides per-canonical-host counting semaphores used
// to bound concurrent upstream connections to a single host across all
// of the cache's outbound paths.
//
// SPEC §9.3: during a refresh storm (50 clients hitting the same suite
// at the same instant), the singleflight collapses the per-key fan-out,
// but the per-host fan-out (different keys, same host) is still bounded
// only by the count of distinct keys. This semaphore bounds *that*
// dimension so the cache cannot accidentally DoS its own upstream.
//
// A single Sem instance is shared between the handler (cache-miss
// fetches) and the freshness checker (conditional GETs). Sharing —
// rather than giving each path its own semaphore — keeps the SPEC
// per-host budget honored regardless of which code path is generating
// the upstream pressure.
package hostsem

import (
	"context"
	"sync"
)

// Sem is a per-host counting semaphore.
//
// AIDEV-NOTE: implemented as a refcounted map of buffered channels.
// Refcounting is critical: without it, every distinct host the cache
// ever sees creates a permanent map entry, which an attacker could grow
// without bound by sending requests for many made-up hostnames. With
// refcounting, the slot is removed once the last in-flight or waiting
// caller releases. Memory is then bounded by O(currently-active hosts),
// not O(hosts ever seen).
type Sem struct {
	mu    sync.Mutex
	limit int
	slots map[string]*hostSlot
}

// hostSlot is the per-host bookkeeping a Sem keeps. ch holds the
// counting tokens; refs is the count of acquire calls (in flight or
// waiting) that still reference this slot. When refs drops to zero
// during release, the slot is deleted from the parent map.
type hostSlot struct {
	ch   chan struct{}
	refs int
}

// New constructs a Sem with the given per-host slot count. A non-positive
// limit means "no concurrency": Acquire never returns a token.
func New(limit int) *Sem {
	if limit < 0 {
		limit = 0
	}
	return &Sem{
		limit: limit,
		slots: make(map[string]*hostSlot),
	}
}

// Acquire blocks until a slot is free for host or ctx is cancelled. The
// returned release closure must be called exactly once when the work is
// done — including on error. The closure is wrapped in sync.Once so an
// accidental second call is a no-op rather than a deadlock (a second
// `<-slot.ch` would block forever waiting for a token that wasn't
// returned). A no-op release is returned on ctx cancellation so callers
// can defer it unconditionally.
func (s *Sem) Acquire(ctx context.Context, host string) (release func(), err error) {
	s.mu.Lock()
	slot, ok := s.slots[host]
	if !ok {
		slot = &hostSlot{ch: make(chan struct{}, s.limit)}
		s.slots[host] = slot
	}
	slot.refs++
	s.mu.Unlock()

	select {
	case slot.ch <- struct{}{}:
		return s.releaserFor(host, slot, true), nil
	case <-ctx.Done():
		// Did not actually take a token; refs needs to be decremented
		// without a corresponding channel-receive.
		s.dropRef(host, slot)
		return func() {}, ctx.Err()
	}
}

// releaserFor returns the closure that drops one refcount and, if
// holdsToken is true, returns the token to the channel. The body is
// guarded by sync.Once so a double-call is a no-op rather than a
// deadlock; see the Acquire doc comment for the rationale.
func (s *Sem) releaserFor(host string, slot *hostSlot, holdsToken bool) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			if holdsToken {
				<-slot.ch
			}
			s.dropRef(host, slot)
		})
	}
}

// dropRef decrements slot.refs and removes the entry from the parent
// map when it reaches zero. The deletion is an exact-pointer match: if
// a fresh slot was created in the meantime under the same host name
// (someone else acquired after the previous holders all released),
// that new slot stays.
func (s *Sem) dropRef(host string, slot *hostSlot) {
	s.mu.Lock()
	slot.refs--
	if slot.refs == 0 {
		if cur, ok := s.slots[host]; ok && cur == slot {
			delete(s.slots, host)
		}
	}
	s.mu.Unlock()
}

// HostCount reports the number of host entries currently in the map.
// Used by tests to assert refcount-driven cleanup.
func (s *Sem) HostCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.slots)
}

// HostStat captures a single host's slot occupancy and capacity at
// the moment Snapshot was called.
type HostStat struct {
	// Inflight is the count of currently-held tokens — i.e. callers
	// that have a release closure they have not yet invoked.
	Inflight int
	// Capacity is the configured per-host slot count.
	Capacity int
}

// Snapshot returns a point-in-time map of every active host's
// (inflight, capacity) tuple. Hosts with zero refcount have already
// been removed from the per-Sem map by dropRef, so the keys are
// exactly the set of hosts with at least one acquire-or-waiter
// outstanding. Waiters do not count toward Inflight: the channel
// send happens only when a token is actually acquired (Acquire
// blocks at `slot.ch <- struct{}{}`), so len(slot.ch) is the count
// of currently-held tokens, not waiters + holders.
//
// AIDEV-NOTE: SPEC5 §9.3 / §10.4.2 — the formula is
// Inflight = len(slot.ch), Capacity = cap(slot.ch). The buffered
// channel fills as concurrency rises (Acquire SENDS on success,
// Release RECEIVES). cap(slot.ch) equals s.limit; either is correct.
//
// Callers should treat the returned map as read-only. The lock is
// held only for the duration of the for-range copy; the cost is one
// allocation plus one memory copy per active host. Expected use:
// the §9.7.6 refresher goroutine at admin.gauge_refresh cadence,
// and the status-page handler at request time.
func (s *Sem) Snapshot() map[string]HostStat {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]HostStat, len(s.slots))
	for host, slot := range s.slots {
		out[host] = HostStat{
			Inflight: len(slot.ch),
			Capacity: cap(slot.ch),
		}
	}
	return out
}
