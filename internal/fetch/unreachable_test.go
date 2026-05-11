package fetch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestUnreachableTracker_NilDisabled(t *testing.T) {
	// Cooldown <= 0 returns nil — every call is a no-op.
	if got := newUnreachableTracker(0, time.Second, nil); got != nil {
		t.Fatalf("cooldown=0 want nil tracker, got %v", got)
	}
	if got := newUnreachableTracker(-1, time.Second, nil); got != nil {
		t.Fatalf("cooldown<0 want nil tracker, got %v", got)
	}

	// Methods on nil receiver must not panic.
	var u *unreachableTracker
	u.markFailed("h")
	u.markOK("h")
	if cooling, _ := u.inCooldown("h"); cooling {
		t.Errorf("nil tracker reported cooldown active")
	}
}

func TestUnreachableTracker_Cooldown(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	clock := func() time.Time { return now }
	u := newUnreachableTracker(30*time.Second, 1*time.Second, clock)

	if cooling, _ := u.inCooldown("a.example"); cooling {
		t.Fatalf("fresh tracker reported cooldown active")
	}

	u.markFailed("a.example")
	cooling, probe := u.inCooldown("a.example")
	if !cooling || probe != time.Second {
		t.Fatalf("after markFailed: want cooling=true, probe=1s, got cooling=%v probe=%v", cooling, probe)
	}

	// Advance to just before cooldown expiry — still cooling.
	now = now.Add(29 * time.Second)
	if cooling, _ := u.inCooldown("a.example"); !cooling {
		t.Errorf("at +29s: want cooling=true, got false")
	}

	// Advance past cooldown — entry must be dropped.
	now = now.Add(2 * time.Second)
	if cooling, _ := u.inCooldown("a.example"); cooling {
		t.Errorf("at +31s: want cooling=false, got true")
	}
}

func TestUnreachableTracker_MarkOKClears(t *testing.T) {
	u := newUnreachableTracker(30*time.Second, time.Second, nil)
	u.markFailed("h")
	if cooling, _ := u.inCooldown("h"); !cooling {
		t.Fatal("expected cooldown after markFailed")
	}
	u.markOK("h")
	if cooling, _ := u.inCooldown("h"); cooling {
		t.Errorf("markOK did not clear cooldown")
	}
}

func TestUnreachableTracker_PerHostIsolated(t *testing.T) {
	u := newUnreachableTracker(30*time.Second, time.Second, nil)
	u.markFailed("a")
	if cooling, _ := u.inCooldown("b"); cooling {
		t.Errorf("host b inherited cooldown from host a")
	}
}

// stubDialFails returns a DialContext that simulates a dial-time timeout
// after stubDuration unless the context fires first. This lets a probe-
// path ctx (shorter deadline) win the race and return early, modeling
// the kernel dropping the SYN with no reply in offline conditions.
func stubDialFails(stubDuration time.Duration, calls *atomic.Int32) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		if calls != nil {
			calls.Add(1)
		}
		t := time.NewTimer(stubDuration)
		defer t.Stop()
		select {
		case <-t.C:
			return nil, &net.OpError{Op: "dial", Net: network, Err: errors.New("i/o timeout")}
		case <-ctx.Done():
			return nil, &net.OpError{Op: "dial", Net: network, Err: ctx.Err()}
		}
	}
}

// Integration-level: Fetch against a stub dialer that always times out.
// First call: one full-budget dial attempt (which marks the host
// failed) plus exactly one probe-retry that fast-fails — markFailed
// fires inside the dialer, so the second attempt already hits the
// cooldown path. This is the documented design (see AIDEV-NOTE in
// dialer.go wrapDialWithTracker): MaxRetries collapses to one probe
// for dial failures because retrying with the full ConnectTimeout
// against a host that already timed out is a hang. Subsequent Fetch
// calls within cooldown skip straight to the probe and bail.
func TestFetch_FastFailAfterDialTimeout(t *testing.T) {
	var dialCalls atomic.Int32
	const stubDuration = 60 * time.Millisecond

	c, err := New(Options{
		ConnectTimeout:          200 * time.Millisecond,
		TotalTimeout:            5 * time.Second,
		MaxRetries:              3,
		AllowedHostRegex:        []string{`^upstream\.invalid$`},
		DenyTargetRanges:        nil,
		UnreachableCooldown:     2 * time.Second,
		UnreachableProbeTimeout: 20 * time.Millisecond,
		dialContext:             stubDialFails(stubDuration, &dialCalls),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	target := &Target{CanonicalHost: "upstream.invalid", URL: "http://upstream.invalid/x"}
	dst := &bufDst{}

	t0 := time.Now()
	_, err = c.Fetch(context.Background(), target, dst)
	firstElapsed := time.Since(t0)
	if err == nil {
		t.Fatalf("first Fetch: expected error")
	}
	// First call: one full-budget dial + one probe retry. The full-
	// budget attempt is bounded by stubDuration (60ms here), the probe
	// by probe_timeout (20ms). Generous upper bound to keep CI reliable.
	if firstElapsed > 2*time.Second {
		t.Errorf("first Fetch: elapsed %v too long; expected ~stubDuration + probe", firstElapsed)
	}
	// Exactly 2 dials: initial attempt (records the cooldown) + one
	// probe retry that fast-fails. Not (1 + MaxRetries) because
	// markFailed fires before the second attempt's isRetryable check.
	if got := dialCalls.Load(); got != 2 {
		t.Errorf("first Fetch: want exactly 2 dials (initial + probe-retry), got %d", got)
	}

	// Second call within cooldown must be fast (single probe attempt
	// of probe_timeout, retries suppressed).
	dialCalls.Store(0)
	_ = dst.Truncate()
	t0 = time.Now()
	_, err = c.Fetch(context.Background(), target, dst)
	secondElapsed := time.Since(t0)
	if err == nil {
		t.Fatalf("second Fetch: expected error")
	}
	if !errors.Is(err, ErrUpstreamUnavailable) {
		t.Errorf("second Fetch: want ErrUpstreamUnavailable in chain, got %v", err)
	}
	if !errors.Is(err, ErrHostUnreachable) {
		t.Errorf("second Fetch: want ErrHostUnreachable in chain, got %v", err)
	}
	if got := dialCalls.Load(); got != 1 {
		t.Errorf("second Fetch: want exactly 1 dial attempt (no retries), got %d", got)
	}
	// Probe + small overhead. Generous upper bound for slow CI.
	if secondElapsed > 500*time.Millisecond {
		t.Errorf("second Fetch: elapsed %v too long; expected ~probe_timeout", secondElapsed)
	}
}

// Successful dial clears the marker — after a successful dial the next
// request to the same host returns to the full retry budget.
func TestFetch_SuccessClearsCooldown(t *testing.T) {
	var dialCalls atomic.Int32
	c, err := New(Options{
		ConnectTimeout:          200 * time.Millisecond,
		TotalTimeout:            5 * time.Second,
		MaxRetries:              0, // single attempt for fast test
		AllowedHostRegex:        []string{`^upstream\.invalid$`},
		DenyTargetRanges:        nil,
		UnreachableCooldown:     2 * time.Second,
		UnreachableProbeTimeout: 10 * time.Millisecond,
		dialContext:             stubDialFails(50*time.Millisecond, &dialCalls),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Drive a failure to set the cooldown marker.
	target := &Target{CanonicalHost: "upstream.invalid", URL: "http://upstream.invalid/x"}
	if _, err := c.Fetch(context.Background(), target, &bufDst{}); err == nil {
		t.Fatalf("first Fetch: expected error")
	}
	if cooling, _ := c.unreachable.inCooldown("upstream.invalid"); !cooling {
		t.Fatalf("after first failure: cooldown not active")
	}

	// markOK is what wrapDialWithTracker calls on a successful dial.
	// Simulate connectivity recovery by invoking it directly (a real
	// net.Conn from a stub would require a much heavier scaffold).
	c.unreachable.markOK("upstream.invalid")
	if cooling, _ := c.unreachable.inCooldown("upstream.invalid"); cooling {
		t.Errorf("markOK did not clear cooldown")
	}
}

// cooldown=0 disables the feature: behavior is identical to pre-fast-fail
// — full retry budget on every miss, and the returned error chain has no
// ErrHostUnreachable.
func TestFetch_CooldownZeroDisables(t *testing.T) {
	var dialCalls atomic.Int32
	c, err := New(Options{
		ConnectTimeout:          200 * time.Millisecond,
		TotalTimeout:            5 * time.Second,
		MaxRetries:              2,
		AllowedHostRegex:        []string{`^upstream\.invalid$`},
		DenyTargetRanges:        nil,
		UnreachableCooldown:     0, // disabled
		UnreachableProbeTimeout: 10 * time.Millisecond,
		dialContext:             stubDialFails(40*time.Millisecond, &dialCalls),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.unreachable != nil {
		t.Fatalf("cooldown=0 should leave Client.unreachable nil")
	}

	target := &Target{CanonicalHost: "upstream.invalid", URL: "http://upstream.invalid/x"}
	dialCalls.Store(0)
	_, err = c.Fetch(context.Background(), target, &bufDst{})
	if err == nil {
		t.Fatalf("expected error")
	}
	if errors.Is(err, ErrHostUnreachable) {
		t.Errorf("cooldown=0: ErrHostUnreachable should not surface, got %v", err)
	}
	// MaxRetries=2 means up to 3 attempts (initial + 2 retries).
	if got := dialCalls.Load(); got < 2 {
		t.Errorf("cooldown=0: want full retry budget, got %d dial attempts", got)
	}
}

// Negative config values are rejected at construction.
func TestNew_NegativeUnreachable(t *testing.T) {
	if _, err := New(Options{UnreachableCooldown: -1}); err == nil {
		t.Errorf("negative UnreachableCooldown: expected error")
	}
	if _, err := New(Options{UnreachableProbeTimeout: -1}); err == nil {
		t.Errorf("negative UnreachableProbeTimeout: expected error")
	}
}

// markFailed opportunistically prunes expired entries so the map size
// is bounded by "hosts that failed within the last cooldown window."
// Without this, an upstream-down event with a broad allowlist could
// grow the map indefinitely over the life of the process.
func TestUnreachableTracker_PruneExpired(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	clock := func() time.Time { return now }
	u := newUnreachableTracker(30*time.Second, time.Second, clock)

	// Mark 100 hosts failed at time t0.
	for i := 0; i < 100; i++ {
		u.markFailed(fmt.Sprintf("h-%d.example", i))
	}
	if got := len(u.last); got != 100 {
		t.Fatalf("after 100 markFailed: map size = %d, want 100", got)
	}

	// Advance past the cooldown and mark one fresh host failed. The
	// pruning sweep must drop all 100 stale entries; only the fresh
	// one remains.
	now = now.Add(60 * time.Second)
	u.markFailed("fresh.example")
	if got := len(u.last); got != 1 {
		t.Errorf("after advance + 1 markFailed: map size = %d, want 1 (sweep should have pruned the rest)", got)
	}
	if _, ok := u.last["fresh.example"]; !ok {
		t.Errorf("fresh entry missing after sweep")
	}
}
