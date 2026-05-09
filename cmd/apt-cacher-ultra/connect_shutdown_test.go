package main

// SPEC6 §15 #16 graceful-shutdown DoD pin for the CONNECT pipeline.
//
// The spec line covers three claims about graceful shutdown when
// `tls_mitm.enabled = true`:
//
//   1. In-flight CONNECT tunnels drain.
//   2. No leaked goroutines after shutdown completes.
//   3. No orphan `pool/` blobs from cancelled inner GETs.
//
// This file pins claims (1) and (2) with a real CONNECT through
// serveListeners: hijacked-connection lifecycle works end-to-end with
// activeWG, lifecycleCancel, and the §9.5 drain budget. Claim (3) is
// vacuously satisfied here — no inner GET runs because we never send
// a TLS ClientHello, so nothing reaches the fetch/finalize/PutBlob
// path that could leak a pool blob. The pool walk is kept as a
// regression guard against a hypothetical future regression where the
// CONNECT pipeline alone (cert generation, hijack accounting) somehow
// landed bytes in pool/.
//
// Claim (3) is pinned non-vacuously by
// shutdown_orphan_blob_test.go's
// TestServe_GracefulShutdown_FetchMidStream_NoOrphanBlob, which
// triggers a real cache-miss fetch against a slow-body upstream,
// cancels the daemon ctx mid-stream, and asserts pool/ + temp/ are
// empty after shutdown. That test uses the HTTP fetch path (the
// MITM-tunneled inner-GET trigger requires privileged port-443
// binding to exercise end-to-end), but the underlying
// atomic-finalize-on-cancel contract — h.lifecycleCtx cancels →
// fetch.Fetch returns ctx error → temp blob's defer cleans up
// before any rename to pool/ runs — is path-independent. The MITM
// inner-GET case shares the same serveCacheMiss → runFetch →
// fetch.Fetch → cache.PutBlob pipeline, so the contract pinned
// there covers it.

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestServe_GracefulShutdown_DrainsInflightCONNECT pins SPEC6 §15 #16
// for the CONNECT path. Sequence:
//
//  1. Boot the daemon with `tls_mitm.enabled = true` (auto-CA under
//     `<cache.dir>/ca`, AllowUnconstrainedCA = true since the regex
//     is empty).
//  2. Open a TCP CONNECT to the cache and read the
//     `200 Connection established` line. The cache-side ServeCONNECT
//     goroutine is now hijacked into `tls.Conn.Handshake()` waiting
//     for our ClientHello — i.e. an in-flight CONNECT tunnel.
//  3. Cancel the daemon's ctx, triggering the §9.5 shutdown sequence.
//     `plainSrv.Shutdown` returns immediately (hijacked conns aren't
//     tracked); `plainSrv.Close` closes the listener but ALSO doesn't
//     touch hijacked conns. The drain blocks at `h.Close → activeWG.Wait`
//     because the CONNECT goroutine still holds the WaitGroup token.
//  4. After a brief settling delay (so the daemon has reached
//     `activeWG.Wait`), close the test conn from our side. The
//     daemon's `tlsConn.Handshake` reader sees the EOF, ServeCONNECT
//     returns, activeWG decrements, and serveListeners returns.
//
// Asserts:
//
//   - serveListeners returns within a small slack of the cancel
//     (the drain path is not blocked indefinitely).
//   - Goroutine count returns to baseline (no CONNECT-pipeline leak).
//   - `pool/` is empty (no inner-GET fetch ever ran, so no orphan).
//
// This test mutates the package-level shutdownTimeout var, mirroring
// TestServe_GracefulShutdown_KillsHungFetchAfterDrainBudget — therefore
// NOT t.Parallel.
func TestServe_GracefulShutdown_DrainsInflightCONNECT(t *testing.T) {
	oldTimeout := shutdownTimeout
	shutdownTimeout = 200 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = oldTimeout })

	cacheDir := t.TempDir()
	cfg := minimalCfg(cacheDir, nil)
	// Allow the synthetic CONNECT target host through the §6.6 fetch
	// gate. CONNECT's FetchGate is fetch.HostAllowed against the
	// canonicalized literal host, so the regex must match
	// "example.test" exactly. Without this, ServeCONNECT denies at
	// the gate (403) and never reaches the hijack.
	cfg.Upstream.AllowedHostRegex = append(cfg.Upstream.AllowedHostRegex, `^example\.test$`)
	// Enable MITM. AllowUnconstrainedCA = true is required because we
	// leave AllowedHostRegex empty (no §5.1.1.1 Name Constraints to
	// derive). The auto-CA materializes under <cache.dir>/ca per §4.2.
	cfg.TlsMitm.Enabled = true
	cfg.TlsMitm.AllowUnconstrainedCA = true
	cfg.TlsMitm.CertCacheSize = 16
	cfg.TlsMitm.LeafCertLifetime.Duration = time.Hour
	cfg.TlsMitm.CACertLifetime.Duration = 30 * 24 * time.Hour
	cfg.TlsMitm.LeafAlgorithm = "ecdsa-p256"

	cacheLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cacheAddr := cacheLn.Addr().String()

	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- serveListeners(ctx, cfg, newTestLogger(), cacheLn, nil, nil, nil)
	}()

	// Wait for the daemon's serve loop to be accepting+handling — the
	// listener is bound before serveListeners runs, so a Dial succeeds
	// from the moment we returned from net.Listen, but the http.Server
	// behind it isn't ready until cache.Open + GC startup pass + handler
	// wiring finish. Probe by sending a no-op HEAD/GET that we expect
	// to fail (no mirror, no allowlist match → 4xx) and waiting for the
	// status line.
	if err := waitForDaemonReady(t, cacheAddr, 10*time.Second); err != nil {
		t.Fatalf("daemon never became ready: %v", err)
	}

	// Open an in-flight CONNECT tunnel.
	conn, err := net.Dial("tcp", cacheAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := conn.Write([]byte("CONNECT example.test:443 HTTP/1.1\r\nHost: example.test:443\r\n\r\n")); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status=%d, want 200", resp.StatusCode)
	}

	// Tunnel is open. ServeCONNECT is now in tls.Conn.Handshake() and
	// holds the activeWG token. Trigger graceful shutdown.
	shutdownStart := time.Now()
	cancel()

	// Brief settling delay so the daemon enters activeWG.Wait before
	// our conn close races it. Without this delay the test still
	// passes (the daemon just waits longer in activeWG.Wait), but
	// having the daemon visibly blocked makes the test scenario
	// cleaner to reason about under failure.
	time.Sleep(50 * time.Millisecond)

	// Close our end. Daemon's tlsConn.Handshake read returns EOF,
	// ServeCONNECT returns, activeWG decrements, h.Close unblocks.
	_ = conn.Close()

	select {
	case err := <-serveDone:
		if err != nil {
			t.Errorf("serveListeners: %v", err)
		}
		dur := time.Since(shutdownStart)
		// Generous: drain budget 200ms + handshake-error propagation
		// after our close + remainder of shutdown teardown should be
		// under a second. 5s is a regression ceiling.
		if dur > 5*time.Second {
			t.Errorf("serveListeners returned in %v; expected sub-second after CONNECT close", dur)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("serveListeners did not return after CONNECT shutdown — hijacked-conn drain may be wedged")
	}

	// Goroutine-leak assertion. Slack tracks the chaos_test.go
	// precedent for the same daemon stack: 4 absorbs the residue of
	// net/http internal goroutines (close-notify writers, transport
	// idle-conn reapers) that exit one tick later than activeWG.
	// On failure we dump full stacks so the operator can see WHICH
	// goroutines leaked rather than only a delta count.
	deadline := time.Now().Add(2 * time.Second)
	const slack = 4
	var nowCount int
	for time.Now().Before(deadline) {
		runtime.GC()
		nowCount = runtime.NumGoroutine()
		if nowCount <= baseline+slack {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if nowCount > baseline+slack {
		buf := make([]byte, 1<<16)
		n := runtime.Stack(buf, true)
		t.Errorf("goroutine leak after CONNECT shutdown: now=%d baseline=%d (slack=%d)\n--- live goroutines ---\n%s",
			nowCount, baseline, slack, buf[:n])
	}

	// Pool-state regression guard (NOT the SPEC6 §15 #16 orphan-blob
	// pin). The spec's "no orphan pool/ blobs from cancelled inner
	// GETs" claim is pinned non-vacuously by
	// shutdown_orphan_blob_test.go's
	// TestServe_GracefulShutdown_FetchMidStream_NoOrphanBlob, which
	// runs a real cache-miss fetch against a slow-body upstream,
	// cancels mid-stream, and walks pool/ + tmp/. Here we only
	// assert that the CONNECT pipeline alone (cert generation,
	// hijack accounting) did NOT touch pool/ — currently impossible
	// by code review, the walk guards against any future code path
	// that violates that invariant. Walk errors fail the test
	// rather than being silently ignored, so a missing or unreadable
	// pool/ dir cannot mask a real regression.
	poolDir := filepath.Join(cacheDir, "pool")
	count := 0
	walkErr := filepath.Walk(poolDir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info == nil || info.IsDir() {
			return nil
		}
		count++
		return nil
	})
	if walkErr != nil {
		t.Errorf("walk pool/: %v", walkErr)
	}
	if count != 0 {
		t.Errorf("CONNECT pipeline left files in pool/: count=%d, want 0", count)
	}
}

// TestServe_GracefulShutdown_StalledCONNECT_DrainBudget pins the
// SPEC6 §9.4 / §15 #16 contract that a hijacked CONNECT whose
// client goes silent (no ClientHello, no close) does NOT outlast
// the shutdown drain budget.
//
// The §9.4 tunnel manager (sync.WaitGroup of in-flight tunnels,
// parent ctx cancelled at shutdown, and conn registry iterated
// under mutex on deadline expiry to force-close still-tracked
// conns) is what makes this work: without it, h.Close →
// activeWG.Wait blocks for the full 20s default HandshakeTimeout
// (SPEC6 §9.2) from internal/proxy/connect.go because the
// hijacked conn still holds the activeWG token and
// net/http.Server.Shutdown does not touch hijacked conns.
//
// Test client: opens CONNECT, reads 200, then leaves the conn
// open without sending any TLS bytes (the daemon's tlsConn is now
// blocked in Handshake reading from the hijacked conn). Asserts
// serveListeners returns inside drainBudget + grace + slack —
// proves the manager's force-close at deadline expiry actually
// fires and unblocks the wedged goroutine.
//
// Mutates the package-level shutdownTimeout var, so NOT
// t.Parallel.
func TestServe_GracefulShutdown_StalledCONNECT_DrainBudget(t *testing.T) {
	oldTimeout := shutdownTimeout
	shutdownTimeout = 200 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = oldTimeout })

	cacheDir := t.TempDir()
	cfg := minimalCfg(cacheDir, nil)
	cfg.Upstream.AllowedHostRegex = append(cfg.Upstream.AllowedHostRegex, `^example\.test$`)
	cfg.TlsMitm.Enabled = true
	cfg.TlsMitm.AllowUnconstrainedCA = true
	cfg.TlsMitm.CertCacheSize = 16
	cfg.TlsMitm.LeafCertLifetime.Duration = time.Hour
	cfg.TlsMitm.CACertLifetime.Duration = 30 * 24 * time.Hour
	cfg.TlsMitm.LeafAlgorithm = "ecdsa-p256"

	cacheLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cacheAddr := cacheLn.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- serveListeners(ctx, cfg, newTestLogger(), cacheLn, nil, nil, nil)
	}()

	if err := waitForDaemonReady(t, cacheAddr, 10*time.Second); err != nil {
		t.Fatalf("daemon never became ready: %v", err)
	}

	conn, err := net.Dial("tcp", cacheAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// Keep the test conn alive for the whole test; only Close at the
	// end so shutdown DOES NOT see a client-side EOF — that would
	// unblock the daemon's tlsConn.Handshake naturally and bypass
	// the §9.4 force-close path we want to exercise.
	defer conn.Close()

	if _, err := conn.Write([]byte("CONNECT example.test:443 HTTP/1.1\r\nHost: example.test:443\r\n\r\n")); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status=%d, want 200", resp.StatusCode)
	}

	// Tunnel hijacked. ServeCONNECT is now in tls.Conn.Handshake()
	// reading from the hijacked conn; the test client does NOT send
	// any TLS bytes and does NOT close. Without the §9.4 manager,
	// shutdown would block for the full 20s default
	// HandshakeTimeout (§9.2). With it, the manager force-closes
	// the conn at drain-budget expiry (200ms here) and shutdown
	// completes.
	shutdownStart := time.Now()
	cancel()

	// drainBudget (200ms) + 1s grace + slack for non-CONNECT
	// shutdown teardown (gauge refresher waits, GC drain, etc.).
	// 5s is a generous regression ceiling — observed shutdown takes
	// well under a second on a 200ms budget. The 15s outer fail is
	// only reached if the §9.4 force-close path is broken.
	select {
	case err := <-serveDone:
		if err != nil {
			t.Errorf("serveListeners: %v", err)
		}
		dur := time.Since(shutdownStart)
		if dur > 5*time.Second {
			t.Errorf("serveListeners returned in %v; expected sub-second on a 200ms drain budget", dur)
		}
		// Lower bound: shutdown must wait at LEAST most of the
		// drain budget before force-closing. Returning earlier
		// means Drain.wg.Wait fired with count==0 (the §9.4
		// Begin-before-Hijack race regressed) or that the budget
		// was bypassed somewhere. 150ms gives 50ms slack on a
		// 200ms budget for clock-resolution noise; force-close
		// itself takes microseconds.
		const minDur = 150 * time.Millisecond
		if dur < minDur {
			t.Errorf("serveListeners returned in %v on a %v drain budget; expected ≥%v (Begin-before-Hijack race may have regressed)",
				dur, shutdownTimeout, minDur)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("serveListeners did not return — §9.4 force-close at drain-budget expiry may be wedged")
	}
}

// waitForDaemonReady polls the cache listener until a fresh request
// receives a complete HTTP response. The daemon's listener is bound
// BEFORE serveListeners runs (net.Listen happens in the test before
// the go-routine starts), so a Dial succeeds immediately — but the
// http.Server isn't accepting on it until cache.Open / GC startup
// pass / handler wiring complete. A real request that gets ANY HTTP
// response line back (even a 4xx) confirms the wiring is live.
func waitForDaemonReady(t *testing.T, addr string, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		// Minimal probe: a relative URI without any [[mirror]] match
		// is rejected as bad_request (400). What we care about is that
		// the daemon RESPONDS — we don't care which status code.
		_ = c.SetDeadline(time.Now().Add(500 * time.Millisecond))
		if _, err := c.Write([]byte("GET /__readiness_probe HTTP/1.1\r\nHost: probe\r\nConnection: close\r\n\r\n")); err != nil {
			_ = c.Close()
			time.Sleep(20 * time.Millisecond)
			continue
		}
		buf := make([]byte, 32)
		n, _ := c.Read(buf)
		_ = c.Close()
		if n > 0 {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("daemon at %s never produced an HTTP response within %s", addr, timeout)
}
