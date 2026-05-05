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
// AIDEV-NOTE: implemented as a map of buffered channels rather than a
// numeric mutex+cond because channel-as-semaphore composes naturally
// with context.Context cancellation in acquire.
type hostSem struct {
	mu    sync.Mutex
	limit int
	slots map[string]chan struct{}
}

func newHostSem(limit int) *hostSem {
	return &hostSem{
		limit: limit,
		slots: make(map[string]chan struct{}),
	}
}

// acquire blocks until a slot is free for host or ctx is cancelled. The
// returned release closure must be called exactly once when the work is
// done. A no-op release is returned on ctx cancellation so callers can
// safely defer it unconditionally.
func (s *hostSem) acquire(ctx context.Context, host string) (release func(), err error) {
	s.mu.Lock()
	ch, ok := s.slots[host]
	if !ok {
		ch = make(chan struct{}, s.limit)
		s.slots[host] = ch
	}
	s.mu.Unlock()

	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return func() {}, ctx.Err()
	}
}
