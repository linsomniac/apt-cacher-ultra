package proxy

// SPEC6 §9.4 TunnelManager focused unit tests. These cover the
// invariants the integration test in cmd/apt-cacher-ultra cannot
// cleanly isolate:
//
//   - Drain returns immediately when no goroutines are in flight.
//   - Drain.wg.Wait honors the budget when goroutines hold the
//     counter (the regression target for Finding 1: a Begin
//     racing with Drain must always be ordered before Drain
//     observes count==0).
//   - Force-close fires AFTER the budget — not before, not
//     later than budget+ε.
//   - End decrements correctly; Begin/End balance.
//   - RegisterConn / UnregisterConn keep the registry bounded.
//
// Uses net.Pipe for deterministic conn lifecycle without OS
// socket setup (works inside sandboxes that disallow listeners).

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

func TestTunnelManager_Drain_NoTunnels_ReturnsImmediately(t *testing.T) {
	t.Parallel()
	m := NewTunnelManager(context.Background())
	start := time.Now()
	if err := m.Drain(time.Second, time.Second); err != nil {
		t.Fatalf("Drain on empty manager: %v", err)
	}
	if dur := time.Since(start); dur > 50*time.Millisecond {
		t.Errorf("Drain on empty manager took %v; expected <50ms", dur)
	}
}

// TestTunnelManager_Drain_GracefulFinish covers the path where a
// goroutine completes within budget — Drain's wg.Wait returns
// before the budget timer fires, no force-close runs, and the
// registry is left empty by the goroutine's own UnregisterConn.
//
// Synchronization: `started` channel ensures Begin/RegisterConn
// have already executed before the test's Drain call. In
// production net/http's c.serve waits for the handler chain (and
// thus for ServeCONNECT's Begin) before Shutdown returns; this
// channel is the test analogue.
func TestTunnelManager_Drain_GracefulFinish(t *testing.T) {
	t.Parallel()
	m := NewTunnelManager(context.Background())
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })

	started := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		m.Begin()
		defer m.End()
		m.RegisterConn(a)
		defer m.UnregisterConn(a)
		close(started)
		// Simulate a tunnel that finishes in ~30ms.
		time.Sleep(30 * time.Millisecond)
		close(finished)
	}()

	<-started
	start := time.Now()
	if err := m.Drain(500*time.Millisecond, time.Second); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	dur := time.Since(start)
	<-finished
	if dur > 200*time.Millisecond {
		t.Errorf("Drain took %v on a graceful 30ms tunnel; expected <200ms", dur)
	}
}

// TestTunnelManager_Drain_ForceCloseAfterBudget covers the
// stalled-tunnel path: a goroutine holds the counter and is
// blocked reading from a registered conn that no one writes to.
// Drain must (a) wait the budget, (b) force-close the conn so
// the wedged goroutine errors out, (c) return nil within
// budget+grace.
//
// This is the focused regression pin for Finding 4 — the integration
// test asserts the daemon-level shape, this asserts the manager
// timing in isolation.
func TestTunnelManager_Drain_ForceCloseAfterBudget(t *testing.T) {
	t.Parallel()
	m := NewTunnelManager(context.Background())
	a, b := net.Pipe()
	t.Cleanup(func() { _ = b.Close() })

	// `started` ensures Begin/RegisterConn happen-before Drain.
	// In production net/http's c.serve waits for ServeCONNECT (and
	// thus its initial Begin) before Shutdown returns and the
	// daemon proceeds to Drain.
	started := make(chan struct{})
	readErr := make(chan error, 1)
	go func() {
		m.Begin()
		defer m.End()
		m.RegisterConn(a)
		defer m.UnregisterConn(a)
		close(started)
		// Block reading. Returns only when force-close runs (or
		// the parent ctx triggers an upstream cancel — neither
		// applies here).
		buf := make([]byte, 1)
		_, err := a.Read(buf)
		readErr <- err
	}()

	<-started
	const budget = 100 * time.Millisecond
	start := time.Now()
	if err := m.Drain(budget, 500*time.Millisecond); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	dur := time.Since(start)

	// Lower bound: Drain MUST wait the budget before force-closing.
	// Returning earlier would mean wg.Wait observed count==0,
	// which is exactly the Begin-before-Hijack race regressed.
	if dur < budget-20*time.Millisecond {
		t.Errorf("Drain returned in %v on a %v budget; expected ≥%v",
			dur, budget, budget-20*time.Millisecond)
	}
	// Upper bound: Drain shouldn't take much past budget — force-close
	// is microseconds, the wedged goroutine unwinds in tens of µs.
	if dur > budget+200*time.Millisecond {
		t.Errorf("Drain returned in %v on a %v budget; expected <%v",
			dur, budget, budget+200*time.Millisecond)
	}

	// And the goroutine actually errored out (force-close fired).
	select {
	case err := <-readErr:
		if err == nil {
			t.Errorf("expected Read to error after force-close, got nil")
		}
	case <-time.After(time.Second):
		t.Errorf("blocked Read goroutine did not unwind after force-close")
	}
}

// TestTunnelManager_BeginEnd_Balance asserts that Begin/End in
// concurrent goroutines correctly drains the WG. Sanity check
// that there's no off-by-one in the counter management.
func TestTunnelManager_BeginEnd_Balance(t *testing.T) {
	t.Parallel()
	m := NewTunnelManager(context.Background())
	const N = 50
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Begin()
			defer m.End()
			time.Sleep(10 * time.Millisecond)
		}()
	}
	wg.Wait()
	// All goroutines have called End. Drain on an empty WG should
	// return immediately.
	start := time.Now()
	if err := m.Drain(500*time.Millisecond, time.Second); err != nil {
		t.Fatalf("Drain after balanced Begin/End: %v", err)
	}
	if dur := time.Since(start); dur > 50*time.Millisecond {
		t.Errorf("Drain after balanced Begin/End took %v; expected <50ms", dur)
	}
}

// TestTunnelManager_ParentCtxCancel covers the parentCtx-cancels-
// on-master-cancel path: cancelling the ctx passed to
// NewTunnelManager propagates to the manager's Context() so
// inner-request ctxs derived from it fire Done. This is the
// shutdown-propagation contract; ServeCONNECT relies on it for
// the synthetic inner request ctx.
func TestTunnelManager_ParentCtxCancel(t *testing.T) {
	t.Parallel()
	parentCtx, cancel := context.WithCancel(context.Background())
	m := NewTunnelManager(parentCtx)
	mgrCtx := m.Context()

	select {
	case <-mgrCtx.Done():
		t.Fatalf("manager ctx fired Done before parent cancel")
	default:
	}

	cancel()

	select {
	case <-mgrCtx.Done():
		// expected
	case <-time.After(time.Second):
		t.Fatalf("manager ctx did not fire Done after parent cancel")
	}
}

// TestTunnelManager_DrainCancelsManagerCtx asserts that calling
// Drain (independent of master ctx) cancels the manager's
// Context(). The shutdown sequence in main.go relies on this for
// in-flight inner-GET propagation.
func TestTunnelManager_DrainCancelsManagerCtx(t *testing.T) {
	t.Parallel()
	m := NewTunnelManager(context.Background())
	mgrCtx := m.Context()

	if err := m.Drain(10*time.Millisecond, 50*time.Millisecond); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	select {
	case <-mgrCtx.Done():
		// expected
	default:
		t.Errorf("manager ctx did not fire Done after Drain")
	}
}
