package freshness

// Spec C10 "In-flight adoptions" (D10): the checker tracks the signed
// adoptions in flight per canonical host, and strict mode waits for them
// (at most strictWait, at most strictWaitersPerHost waiters per host)
// before it refuses an unvouched .deb.

import (
	"context"
	"sync"
	"time"
)

const (
	// strictWait bounds how long a request waits for a host's adoptions
	// in flight (a C11 constant).
	strictWait = 15 * time.Second
	// strictWaitersPerHost caps the requests waiting per host; beyond it
	// the refusal is immediate (a C11 constant).
	strictWaitersPerHost = 64
	// strictRequestBudget bounds the wait by the request's age: it waits
	// at most strictRequestBudget minus the time since the request started
	// (design review M-10). The miss path still downloads the whole .deb
	// after the wait before its first byte, and apt gives up after 30 s
	// without one (a C11 constant).
	strictRequestBudget = 25 * time.Second
	// strictMinWait: a wait shorter than this is not started; the refusal
	// is immediate (a C11 constant).
	strictMinWait = time.Second
)

// hostAdoptions counts signed adoptions in flight per host and lets
// requests wait until a host has none.
//
// AIDEV-NOTE: only signed adoptions count (unsigned snapshots never vouch,
// so waiting for one would only delay the refusal), and a count drops only
// when the adoption goroutine has returned — after its commit — so a waiter
// that wakes re-reads a package_hash table that already holds the new
// snapshot's rows.
type hostAdoptions struct {
	mu      sync.Mutex
	n       map[string]int
	waiters map[string]int
	idle    map[string]chan struct{} // closed when the host's count drops to 0
}

func newHostAdoptions() *hostAdoptions {
	return &hostAdoptions{n: map[string]int{}, waiters: map[string]int{}, idle: map[string]chan struct{}{}}
}

// begin counts an adoption on host.
func (h *hostAdoptions) begin(host string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.n[host]++
}

// end uncounts an adoption on host and wakes its waiters when it was the
// last.
func (h *hostAdoptions) end(host string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.n[host]--; h.n[host] > 0 {
		return
	}
	delete(h.n, host)
	if ch, ok := h.idle[host]; ok {
		close(ch)
		delete(h.idle, host)
	}
}

// inFlight reports whether host has a signed adoption in flight.
func (h *hostAdoptions) inFlight(host string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n[host] > 0
}

// wait blocks until host has no signed adoption in flight, maxWait
// passes or ctx ends, and reports whether the caller may re-run its
// lookup. With nothing in flight it returns true at once — before the
// waiter cap, so a request that arrives as the last adoption ends, with
// waiters still unwinding, re-runs its lookup instead of being refused
// (deferred minor E-58). Otherwise false when the host already has
// strictWaitersPerHost waiters, or maxWait is below strictMinWait (the
// caller refuses at once), and false when ctx ended during the wait (the
// client left: design note 31, no retry — the caller answers
// client_canceled). A request that waits is counted
// (acu_strict_adoption_waits_total).
//
// AIDEV-NOTE: never return true on ctx.Done: the caller's re-run lookup
// would run with the dead context, fail, and fail open — the hit path
// served the unvouched .deb, which a MITM tunnel's writer (alive while
// Drain cancels the inner request's context) delivered (review fix round
// 1, Task 9). The handler also checks the context itself after any wait.
func (h *hostAdoptions) wait(ctx context.Context, host string, maxWait time.Duration) bool {
	h.mu.Lock()
	if h.n[host] == 0 {
		h.mu.Unlock()
		return true
	}
	if h.waiters[host] >= strictWaitersPerHost {
		h.mu.Unlock()
		return false
	}
	if maxWait < strictMinWait {
		h.mu.Unlock()
		return false
	}
	h.waiters[host]++
	strictWaitsTotal.Inc(host)
	ch, ok := h.idle[host]
	if !ok {
		ch = make(chan struct{})
		h.idle[host] = ch
	}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		if h.waiters[host]--; h.waiters[host] == 0 {
			delete(h.waiters, host)
		}
		h.mu.Unlock()
	}()
	t := time.NewTimer(maxWait)
	defer t.Stop()
	select {
	case <-ch:
	case <-t.C:
	case <-ctx.Done():
		return false
	}
	return true
}

// AdoptionsInFlight reports whether a signed adoption is in flight on the
// canonical host (spec C10: the miss path asks before it refuses).
func (c *Checker) AdoptionsInFlight(host string) bool { return c.inflight.inFlight(host) }

// WaitHostAdoptions waits until no signed adoption is in flight on the
// canonical host (spec C10) — at most min(15 s, 25 s − the time since the
// request started at start) — and reports whether the caller should re-run
// its lookup once: false when 64 requests already wait on the host or
// less than 1 s of that budget is left, and the caller refuses at once;
// false too when ctx ended during the wait, and the caller answers
// client_canceled.
func (c *Checker) WaitHostAdoptions(ctx context.Context, host string, start time.Time) bool {
	return c.inflight.wait(ctx, host, min(strictWait, strictRequestBudget-time.Since(start)))
}
