package fetch

import (
	"errors"
	"sync"
	"time"
)

// ErrHostUnreachable is returned by the dialer when a probe attempt to a
// host already in the unreachability cooldown window also fails. It is
// intentionally non-retryable (see isRetryable): if we just probed the
// host and the probe failed, hammering with retries adds latency without
// changing the outcome. Wrapped onto the underlying network error so
// errors.Is(err, ErrHostUnreachable) is the dispatch key, while errors.As
// still recovers the underlying *net.OpError if a caller wants it.
//
// AIDEV-NOTE: SPEC §1 "never hang" is preserved here by short-circuiting
// dial when this host just timed out within unreachable_cooldown. The
// 502/Retry-After contract clients see is unchanged — only the wallclock
// to reach it shrinks from connect_timeout × max_retries to one short
// probe.
var ErrHostUnreachable = errors.New("fetch: host known unreachable (within cooldown)")

// unreachableTracker remembers, per canonical-host, the timestamp of the
// most recent dial-time failure. Used by the wrapped dialer in dialer.go
// to (a) shorten the connect deadline for the next probe attempt and
// (b) suppress retries on dial failures while the cooldown is active.
//
// Zero-cooldown disables the feature entirely; New(Options{}) signals
// this by leaving Client.unreachable = nil and every helper short-circuits
// on the nil receiver.
type unreachableTracker struct {
	cooldown     time.Duration
	probeTimeout time.Duration
	now          func() time.Time

	mu   sync.Mutex
	last map[string]time.Time
}

func newUnreachableTracker(cooldown, probeTimeout time.Duration, now func() time.Time) *unreachableTracker {
	if cooldown <= 0 {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	return &unreachableTracker{
		cooldown:     cooldown,
		probeTimeout: probeTimeout,
		now:          now,
		last:         make(map[string]time.Time),
	}
}

// inCooldown reports whether host is currently within the cooldown window
// and, if so, the probe deadline that should bound the next dial attempt.
// A stale entry past the cooldown is dropped opportunistically.
func (u *unreachableTracker) inCooldown(host string) (bool, time.Duration) {
	if u == nil {
		return false, 0
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	t, ok := u.last[host]
	if !ok {
		return false, 0
	}
	if u.now().Sub(t) >= u.cooldown {
		delete(u.last, host)
		return false, 0
	}
	return true, u.probeTimeout
}

// markFailed records that host's most recent dial attempt failed. The
// next dial within cooldown becomes a probe.
//
// Opportunistically sweeps expired entries on every call so the map
// size is bounded by "hosts that failed within the last cooldown
// window" — without this, an upstream-down event at a deployment with
// a broad allowed_host_regex (or an attacker pointing the cache at
// many distinct allowlisted-but-currently-broken hostnames) would let
// the map accumulate entries for the life of the process. The sweep is
// O(N) under the mutex, but markFailed only fires after a connect-
// timeout-scale wait, so the amortized cost is negligible.
func (u *unreachableTracker) markFailed(host string) {
	if u == nil || host == "" {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	now := u.now()
	u.last[host] = now
	for k, t := range u.last {
		if k == host {
			continue
		}
		if now.Sub(t) >= u.cooldown {
			delete(u.last, k)
		}
	}
}

// markOK clears any cooldown for host. Called on a successful dial so the
// next request returns to the normal ConnectTimeout / retry budget.
func (u *unreachableTracker) markOK(host string) {
	if u == nil || host == "" {
		return
	}
	u.mu.Lock()
	delete(u.last, host)
	u.mu.Unlock()
}
