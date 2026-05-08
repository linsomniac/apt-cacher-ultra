package main

// SPEC6 §15 #11 — acu_mitm_handshake_duration_seconds metric pin.
//
// §15 #11: "Every `acu_mitm_*` metric in §10.3 increments at least
// once during the §12.2 integration suite." The existing metrics
// integration pin (TestServe_MITMConnect_IncrementsAllMetrics in
// mitm_metrics_integration_test.go) covers six of the seven
// §10.3 metrics but explicitly defers
// `acu_mitm_handshake_duration_seconds` because the existing pin
// drives an INCOMPLETE handshake (the test client never sends a
// ClientHello). The handshake-duration histogram is observed only
// after a successful tls.Server.Handshake() at
// internal/proxy/connect.go:544 — so reaching it requires a real
// tls.Client handshake against the cache's leaf cert, which in
// turn requires the §15 #2 HTTPS scaffold to install the
// auto-generated CA into a test client's trust store.
//
// The §15 #2 scaffold (inner_get_deny_range_test.go committed as
// part of the F15 pin) already proves the auto-CA → tls.Client
// pipeline works end-to-end. This test reuses the same pattern
// for a different purpose: drive a successful handshake, then
// close — no inner GET needed. The metric fires the moment the
// handshake completes, BEFORE the inner-GET read loop runs.
//
// Asserts: acu_mitm_handshake_duration_seconds_count increments
// by ≥ 1 across the test's duration.
//
// Mutates package-level shutdownTimeout — NOT t.Parallel.

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestServe_MITMConnect_HandshakeDurationMetricObserved(t *testing.T) {
	oldTimeout := shutdownTimeout
	shutdownTimeout = 500 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = oldTimeout })

	cacheDir := t.TempDir()
	cfg := minimalCfg(cacheDir, nil)
	cfg.Upstream.AllowedHostRegex = append(cfg.Upstream.AllowedHostRegex, `^localhost$`)
	cfg.TlsMitm.Enabled = true
	cfg.TlsMitm.AllowUnconstrainedCA = true
	cfg.TlsMitm.CertCacheSize = 16
	cfg.TlsMitm.LeafCertLifetime.Duration = time.Hour
	cfg.TlsMitm.CACertLifetime.Duration = 30 * 24 * time.Hour
	cfg.TlsMitm.LeafAlgorithm = "ecdsa-p256"

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	cacheLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cacheAddr := cacheLn.Addr().String()

	// Capture the baseline before the daemon starts. metrics.Default
	// is shared across tests in this package — assert delta, not
	// absolute, against this snapshot.
	baseline := scrapeMetrics(t)
	hsCountBefore := readHistogramCount(baseline, "acu_mitm_handshake_duration_seconds")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- serveListeners(ctx, cfg, logger, cacheLn, nil, nil)
	}()

	if err := waitForDaemonReady(t, cacheAddr, 10*time.Second); err != nil {
		t.Fatalf("daemon never became ready: %v", err)
	}

	// Read the auto-generated CA — wireTlsMitm runs LoadOrGenerate
	// at startup, so the file is on disk by the time the daemon
	// accepts connections.
	caPath := filepath.Join(cacheDir, "ca", "ca.crt")
	caBytes, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatalf("read auto-CA at %s: %v", caPath, err)
	}
	block, _ := pem.Decode(caBytes)
	if block == nil {
		t.Fatalf("no PEM block in %s", caPath)
	}
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse auto-CA: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	rawConn, err := net.Dial("tcp", cacheAddr)
	if err != nil {
		t.Fatalf("dial cache: %v", err)
	}
	defer rawConn.Close()

	// Bound the whole interaction so a regression can't hang the
	// test.
	if err := rawConn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatalf("set rawConn deadline: %v", err)
	}

	if _, err := rawConn.Write([]byte("CONNECT localhost:443 HTTP/1.1\r\nHost: localhost:443\r\n\r\n")); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	br := bufio.NewReader(rawConn)
	connectResp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	_ = connectResp.Body.Close()
	if connectResp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", connectResp.StatusCode)
	}

	tlsClient := tls.Client(&prereadInnerConn{Conn: rawConn, br: br}, &tls.Config{
		ServerName: "localhost",
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
	})
	hsCtx, hsCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer hsCancel()
	if err := tlsClient.HandshakeContext(hsCtx); err != nil {
		t.Fatalf("inner TLS handshake: %v", err)
	}
	// Successful handshake → server-side observed
	// mitmHandshakeDurationSeconds at internal/proxy/connect.go:544.
	// The metric write happens in the cache's ServeCONNECT goroutine
	// before the inner-GET read loop, so the observation is in
	// metrics.Default by now (the call site is synchronous from
	// the cache's perspective). No inner GET needed for this pin.

	// Close the TLS conn, then drive shutdown to flush any further
	// per-tunnel observations (connect_total, connect_duration).
	_ = tlsClient.Close()
	cancel()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Errorf("serveListeners: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("serveListeners did not return")
	}

	after := scrapeMetrics(t)
	hsCountAfter := readHistogramCount(after, "acu_mitm_handshake_duration_seconds")
	if hsCountAfter-hsCountBefore < 1 {
		t.Errorf("acu_mitm_handshake_duration_seconds_count delta = %v; want ≥1\nbefore=%v after=%v\n--- after dump ---\n%s",
			hsCountAfter-hsCountBefore, hsCountBefore, hsCountAfter, after)
	}
}
