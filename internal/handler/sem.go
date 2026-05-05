package handler

import (
	"context"
	"sync"
)

// hostSem provides per-canonical-host counting semaphores. SPEC §9.3:
// during a refresh storm (50 clients hitting the same suite at the same
// instant), the singleflight collapses the per-key fan-out, but the
// per-host fan-out (different keys, same host) is still bounded only by
// the count of distinct keys. This semaphore bounds *that* dimension so
// the cache cannot accidentally DoS its own upstream.
//
// AIDEV-NOTE: implemented as a refcounted map of buffered channels.
// Refcounting is critical: without it, every distinct host the cache
// ever sees creates a permanent map entry, which an attacker could grow
// without bound by sending requests for many made-up hostnames. With
// refcounting, the slot is removed once the last in-flight or waiting
// caller releases. Memory is then bounded by O(currently-active hosts),
// not O(hosts ever seen).
type hostSem struct {
	mu    sync.Mutex
	limit int
	slots map[string]*hostSlot
}

// hostSlot is the per-host bookkeeping a hostSem keeps. ch holds the
// counting tokens; refs is the count of acquire calls (in flight or
// waiting) that still reference this slot. When refs drops to zero
// during release, the slot is deleted from the parent map.
type hostSlot struct {
	ch   chan struct{}
	refs int
}

func newHostSem(limit int) *hostSem {
	return &hostSem{
		limit: limit,
		slots: make(map[string]*hostSlot),
	}
}

// acquire blocks until a slot is free for host or ctx is cancelled. The
// returned release closure must be called exactly once when the work is
// done — including on error. A no-op release is returned on ctx
// cancellation so callers can defer it unconditionally.
func (s *hostSem) acquire(ctx context.Context, host string) (release func(), err error) {
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
// holdsToken is true, returns the token to the channel.
func (s *hostSem) releaserFor(host string, slot *hostSlot, holdsToken bool) func() {
	return func() {
		if holdsToken {
			<-slot.ch
		}
		s.dropRef(host, slot)
	}
}

// dropRef decrements slot.refs and removes the entry from the parent
// map when it reaches zero. The deletion is an exact-pointer match: if
// a fresh slot was created in the meantime under the same host name
// (someone else acquired after the previous holders all released),
// that new slot stays.
func (s *hostSem) dropRef(host string, slot *hostSlot) {
	s.mu.Lock()
	slot.refs--
	if slot.refs == 0 {
		if cur, ok := s.slots[host]; ok && cur == slot {
			delete(s.slots, host)
		}
	}
	s.mu.Unlock()
}

// hostCount reports the number of host entries currently in the map.
// Used by tests to assert refcount-driven cleanup.
func (s *hostSem) hostCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.slots)
}
