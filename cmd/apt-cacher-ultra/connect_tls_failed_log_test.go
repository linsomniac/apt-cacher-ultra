package main

// SPEC6 §11 F9 + §15 #2 — TLS handshake on hijacked conn fails
// integration pin.
//
// §11 F9: "TLS handshake on hijacked conn fails (client distrusts
// CA, TLS-version mismatch, cipher mismatch). Tunnel closes with
// `mitm_connect` Warn (`outcome=tls_failed`)."
//
// §12.4 maps F9 to "12.2 integration (TLS policy / version
// mismatch)".
//
// Driven by a real tls.Client handshake whose RootCAs pool is
// EMPTY — the handshake initiates, the cache sends its leaf cert,
// the client rejects with a "no trusted root" alert, and both
// sides see the failure. The cache's tls.Server.Handshake() must
// return non-nil non-deadline-exceeded → outcome=tls_failed.
//
// This is a more authentic reproduction than a garbage-bytes
// stream because it exercises a real TLS state-machine alert path
// rather than just a parse error on the very first byte. A real
// apt client distrusting the cache's CA produces this exact
// shape.
//
// Distinct from §11 F10 (handshake TIMEOUT — client sends nothing
// → deadline expires → outcome=tls_handshake_timeout): here the
// client is talkative but the conversation produces an alert.
//
// Mutates the package-level shutdownTimeout so NOT t.Parallel.

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestServe_HijackedHandshakeFails_EmitsTLSFailed(t *testing.T) {
	oldTimeout := shutdownTimeout
	shutdownTimeout = 500 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = oldTimeout })

	cacheDir := t.TempDir()
	cfg := minimalCfg(cacheDir, nil)
	// CONNECT target host must satisfy both the §6.6 fetch gate
	// (Upstream.AllowedHostRegex) and the §5.1.2 signing gate
	// (TlsMitm.AllowedHostRegex). With AllowUnconstrainedCA=true we
	// can leave TlsMitm.AllowedHostRegex empty (no Name Constraints
	// derivation) and rely on Upstream.AllowedHostRegex for both
	// gates per the daemon's wiring.
	cfg.Upstream.AllowedHostRegex = append(cfg.Upstream.AllowedHostRegex, `^example\.test$`)
	cfg.TlsMitm.Enabled = true
	cfg.TlsMitm.AllowUnconstrainedCA = true
	cfg.TlsMitm.CertCacheSize = 16
	cfg.TlsMitm.LeafCertLifetime.Duration = time.Hour
	cfg.TlsMitm.CACertLifetime.Duration = 30 * 24 * time.Hour
	cfg.TlsMitm.LeafAlgorithm = "ecdsa-p256"

	var sb captureBuilder
	logger := slog.New(slog.NewJSONHandler(&sb, &slog.HandlerOptions{Level: slog.LevelWarn}))

	cacheLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cacheAddr := cacheLn.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- serveListeners(ctx, cfg, logger, cacheLn, nil, nil)
	}()

	if err := waitForDaemonReady(t, cacheAddr, 10*time.Second); err != nil {
		t.Fatalf("daemon never became ready: %v", err)
	}

	// Open CONNECT, read 200, then run a real TLS handshake against
	// the hijacked conn with an EMPTY cert pool. The client sees the
	// cache's leaf cert, can't chain it to any trusted root, and
	// emits an alert. The cache's tls.Server.Handshake() returns
	// the corresponding error.
	rawConn, err := net.Dial("tcp", cacheAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer rawConn.Close()

	if _, err := rawConn.Write([]byte("CONNECT example.test:443 HTTP/1.1\r\nHost: example.test:443\r\n\r\n")); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	br := bufio.NewReader(rawConn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status=%d, want 200", resp.StatusCode)
	}

	tlsClient := tls.Client(rawConn, &tls.Config{
		ServerName: "example.test",
		RootCAs:    x509.NewCertPool(), // empty → distrust everything
	})
	hsCtx, hsCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer hsCancel()
	if err := tlsClient.HandshakeContext(hsCtx); err == nil {
		t.Fatalf("client-side handshake should have failed verification, got nil")
	}

	// Cancel the daemon to flush the handler's goroutine and
	// guarantee the warn line is in sb before we read.
	cancel()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Errorf("serveListeners: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("serveListeners did not return")
	}

	var found bool
	for _, line := range strings.Split(strings.TrimSpace(sb.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("invalid JSON log line: %v\n%s", err, line)
		}
		if msg, _ := rec["msg"].(string); msg != "mitm_connect" {
			continue
		}
		outcome, _ := rec["outcome"].(string)
		if outcome != "tls_failed" {
			// CONNECT may emit other mitm_connect lines (e.g. for the
			// initial denied-host gate path) — only the tls_failed
			// emit is what we're asserting reachability for.
			continue
		}
		if found {
			t.Errorf("more than one mitm_connect outcome=tls_failed; one CONNECT should produce exactly one\n%s", line)
		}
		found = true

		if level, _ := rec["level"].(string); level != "WARN" {
			t.Errorf("mitm_connect.level = %q, want WARN\n%s", level, line)
		}
		if host, _ := rec["host"].(string); host != "example.test" {
			t.Errorf("mitm_connect.host = %q, want %q\n%s", host, "example.test", line)
		}
		if reason, _ := rec["reason"].(string); !strings.HasPrefix(reason, "tls:") {
			t.Errorf("mitm_connect.reason = %q, want prefix %q\n%s", reason, "tls:", line)
		}
	}
	if !found {
		t.Errorf("no mitm_connect outcome=tls_failed log line emitted; the failed handshake should have produced one\nlogs:\n%s", sb.String())
	}
}
