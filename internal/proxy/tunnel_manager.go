// SPEC6 §9.4 hijacked-CONNECT tunnel manager.
//
// AIDEV-NOTE: Critical-correctness. Go's net/http documents that
// http.Server.Shutdown does NOT close or wait for hijacked
// connections — once ServeCONNECT calls Hijack, the conn becomes
// the package's responsibility for the rest of its lifetime.
// Without this manager, a stalled CONNECT (client opens but never
// sends ClientHello) blocks shutdown indefinitely (or for the
// 30s default HandshakeTimeout, whichever is shorter), because
// activeWG.Wait inside Handler.Close cannot finish while a
// hijacked goroutine still holds its WaitGroup token.
//
// The manager owns:
//   - parentCtx: cancelled at shutdown step. Synthetic inner
//     request ctxs derive from this so shutdown propagates to the
//     inner GET via the standard r.Context() path.
//   - wg: counter of in-flight tunnels, incremented on Track and
//     decremented on Untrack.
//   - conns: registry of hijacked conns. On Drain deadline expiry
//     the manager iterates this under mu and Close()s every
//     still-tracked conn — that unblocks any goroutine wedged in
//     tls.Handshake / bufio.Read / etc., which then errors and
//     releases the WaitGroup.
package proxy

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"
)

// TunnelManager bridges Go's "hijacked conns are not waited on by
// Shutdown" gap. Construct one per ConnectHandler at startup;
// Drain it during the daemon's shutdown sequence between
// http.Server.Close and Handler.Close.
type TunnelManager struct {
	parentCtx context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup

	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

// NewTunnelManager returns a manager whose parent ctx is derived
// from `ctx`. Cancelling `ctx` propagates to the manager's parent
// ctx (and from there to every live tunnel's synthetic inner
// request); calling Drain also cancels parentCtx explicitly,
// which is the daemon's shutdown step path.
func NewTunnelManager(ctx context.Context) *TunnelManager {
	parent, cancel := context.WithCancel(ctx)
	return &TunnelManager{
		parentCtx: parent,
		cancel:    cancel,
		conns:     make(map[net.Conn]struct{}),
	}
}

// Context returns the parent ctx callers wire into the synthetic
// inner request. Its Done channel fires on shutdown, at which
// point handler-internal r.Context() reads short-circuit and the
// inner ResponseWriter side aborts. Returns the same ctx every
// call — it is safe to derive multiple children from it.
func (m *TunnelManager) Context() context.Context {
	return m.parentCtx
}

// Track registers a freshly-hijacked conn and increments the
// in-flight-tunnel counter. Caller MUST defer Untrack(conn) in
// the same goroutine — including on every error path between
// hijack and the natural tunnel-close return — or Drain will
// deadlock waiting for a counter that never decrements.
func (m *TunnelManager) Track(conn net.Conn) {
	m.wg.Add(1)
	m.mu.Lock()
	m.conns[conn] = struct{}{}
	m.mu.Unlock()
}

// Untrack removes a conn from the registry and decrements the
// in-flight counter. Idempotent against the registry: untracking
// a conn already removed by Drain's force-close path is a no-op
// for the map; the wg decrement still fires exactly once
// (paired 1:1 with Track).
func (m *TunnelManager) Untrack(conn net.Conn) {
	m.mu.Lock()
	delete(m.conns, conn)
	m.mu.Unlock()
	m.wg.Done()
}

// Drain implements the SPEC6 §9.4 shutdown protocol:
//
//  1. Cancel parentCtx. Every live tunnel's synthetic inner
//     request ctx fires Done.
//  2. Wait up to `budget` for the in-flight WG to drain to zero.
//  3. If `budget` expires, iterate the registry under mu and
//     Close() every still-tracked conn. This unblocks any
//     goroutine wedged in a Read / Write / TLS handshake; the
//     wedged goroutine errors out, returns from ServeCONNECT,
//     fires its deferred Untrack, and decrements the WG.
//  4. Wait up to `grace` for the WG to drain after the
//     force-close.
//
// Returns nil if the WG drained within budget+grace. Returns
// ErrDrainDeadline if grace also expired (one or more tunnels
// remained wedged after force-close — extremely unlikely in
// practice because closing a conn yields immediately on any
// pending Read/Write).
//
// Idempotent: subsequent calls re-cancel parentCtx (a no-op for
// an already-cancelled ctx) and re-walk the registry (which is
// empty after the first call's Untracks complete). Multiple
// concurrent Drains are NOT safe — call exactly once from the
// shutdown goroutine.
func (m *TunnelManager) Drain(budget, grace time.Duration) error {
	m.cancel()

	if waitWG(&m.wg, budget) {
		return nil
	}

	// Budget expired. Force-close every still-tracked conn so
	// wedged goroutines can unwind.
	m.mu.Lock()
	for conn := range m.conns {
		_ = conn.Close()
	}
	m.mu.Unlock()

	if waitWG(&m.wg, grace) {
		return nil
	}
	return ErrDrainDeadline
}

// ErrDrainDeadline indicates that the manager's grace window
// elapsed after force-closing all tracked conns and the WG still
// did not drain to zero. In normal operation this never returns;
// surfacing it is a signal that a tunnel goroutine is wedged in
// something that conn.Close does NOT unblock (e.g. a deadlocked
// handler write to a channel that has no reader). Operator
// action: investigate; the daemon proceeds with shutdown anyway
// because the deadlocked goroutine will be reaped by process
// exit.
var ErrDrainDeadline = errors.New("tunnel: drain deadline exceeded")

// waitWG waits up to `d` for wg.Wait to return. Returns true if
// the WG drained within the deadline, false if the timer fired
// first. The waiter goroutine outlives this function on the
// timeout path — that is intentional: it will exit when wg
// finally drains (which happens when force-close fires in the
// next Drain stage), and its only side effect is closing a local
// channel.
func waitWG(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}
