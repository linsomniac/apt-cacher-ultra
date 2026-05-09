package main

// SPEC6 §15 #10 DoD pin — mitm_connect integration reachability
// through the daemon CONNECT path.
//
// §10.2 contract:
//   mitm_connect: {host, port, client_addr, outcome, duration_ms,
//                  denied_gate (when outcome=denied_host),
//                  reason (free-text trailer, omitted when empty)}
//
//   Level Info on outcome=tunneled; Warn on every other outcome.
//
// internal/proxy/connect_test.go pins outcome enum + handler
// behavior at unit scope (with a stub LogFn). This test pins the
// daemon-side wiring: that proxy.NewConnectHandler's LogFn arg
// in main.go's wireTlsMitm threads through emitTlsMitmLog to
// slog.Logger so operators see the line in journal.
//
// Driven by a denied-host CONNECT: the test host matches the §6.6
// upstream allowlist but NOT the §5.1.2 tls_mitm signing-gate
// regex, so the handler returns 403 immediately and emits
// `mitm_connect` Warn with outcome=denied_host, denied_gate=
// signing — fast and deterministic, no TLS handshake involved.
//
// Mutates the package-level shutdownTimeout, so NOT t.Parallel.

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestServe_DeniedHostCONNECT_EmitsMITMConnectLog(t *testing.T) {
	oldTimeout := shutdownTimeout
	shutdownTimeout = 500 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = oldTimeout })

	cacheDir := t.TempDir()
	cfg := minimalCfg(cacheDir, nil)
	// Upstream gate would let the host through — but the signing
	// gate runs first, so denied_gate=signing wins.
	cfg.Upstream.AllowedHostRegex = append(cfg.Upstream.AllowedHostRegex,
		`^denied-by-signing\.test$`)
	cfg.TlsMitm.Enabled = true
	cfg.TlsMitm.AllowUnconstrainedCA = true
	// Force the signing gate to reject the test host. A non-empty
	// regex switches signingGate from nil (vacuous-true) to a real
	// predicate; the host below does not match.
	cfg.TlsMitm.AllowedHostRegex = `^never-match\.test$`
	cfg.TlsMitm.CertCacheSize = 16
	cfg.TlsMitm.LeafCertLifetime.Duration = time.Hour
	cfg.TlsMitm.CACertLifetime.Duration = 30 * 24 * time.Hour
	cfg.TlsMitm.LeafAlgorithm = "ecdsa-p256"

	// Capture Warn JSON. mitm_connect on a non-tunneled outcome is
	// Warn per §10.2; main.go's slog handler used elsewhere defaults
	// to Info, which would still capture Warn — but be explicit.
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
		serveDone <- serveListeners(ctx, cfg, logger, cacheLn, nil, nil, nil)
	}()

	if err := waitForDaemonReady(t, cacheAddr, 10*time.Second); err != nil {
		t.Fatalf("daemon never became ready: %v", err)
	}

	// Drive one CONNECT. Signing gate rejects → 403 + emit.
	conn, err := net.Dial("tcp", cacheAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := conn.Write([]byte("CONNECT denied-by-signing.test:443 HTTP/1.1\r\nHost: denied-by-signing.test:443\r\n\r\n")); err != nil {
		_ = conn.Close()
		t.Fatalf("write CONNECT: %v", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		_ = conn.Close()
		t.Fatalf("read CONNECT resp: %v", err)
	}
	_ = resp.Body.Close()
	_ = conn.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("CONNECT denied-host status = %d, want %d (signing gate should reject)",
			resp.StatusCode, http.StatusForbidden)
	}

	// Shutdown so all log writes have completed before we read.
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
		msg, _ := rec["msg"].(string)
		if msg != "mitm_connect" {
			continue
		}
		if found {
			t.Errorf("more than one mitm_connect emitted; one CONNECT must produce one log line\n%s", line)
		}
		found = true

		if outcome, _ := rec["outcome"].(string); outcome != "denied_host" {
			t.Errorf("mitm_connect.outcome = %q, want %q\n%s", outcome, "denied_host", line)
		}
		if gate, _ := rec["denied_gate"].(string); gate != "signing" {
			t.Errorf("mitm_connect.denied_gate = %q, want %q (signing gate runs first)\n%s", gate, "signing", line)
		}
		if host, _ := rec["host"].(string); host != "denied-by-signing.test" {
			t.Errorf("mitm_connect.host = %q, want %q (lower-cased CONNECT target)\n%s",
				host, "denied-by-signing.test", line)
		}
		// JSON numbers decode as float64; spec calls for integer ports.
		portRaw, present := rec["port"]
		if !present {
			t.Errorf("mitm_connect missing port field\n%s", line)
		} else if portF, ok := portRaw.(float64); !ok {
			t.Errorf("mitm_connect.port not numeric: %T %v\n%s", portRaw, portRaw, line)
		} else if int(portF) != 443 {
			t.Errorf("mitm_connect.port = %v, want 443\n%s", int(portF), line)
		}
		if ca, _ := rec["client_addr"].(string); ca == "" {
			t.Errorf("mitm_connect missing/empty client_addr\n%s", line)
		} else if !strings.Contains(ca, ":") {
			t.Errorf("mitm_connect.client_addr = %q, want host:port form\n%s", ca, line)
		}
		// duration_ms is int64 milliseconds per the §10.2 audit fix
		// in 73f9347. JSON wire decodes as float64.
		durRaw, present := rec["duration_ms"]
		if !present {
			t.Errorf("mitm_connect missing duration_ms field\n%s", line)
		} else if dur, ok := durRaw.(float64); !ok {
			t.Errorf("mitm_connect.duration_ms not numeric: %T %v\n%s", durRaw, durRaw, line)
		} else if dur < 0 {
			t.Errorf("mitm_connect.duration_ms = %v, want ≥0\n%s", dur, line)
		}
		// reason is a free-text trailer included for operator
		// debugging. Spec §10.2 doesn't enforce its content but the
		// signing-gate denial path is documented to populate it.
		if reason, _ := rec["reason"].(string); reason == "" {
			t.Errorf("mitm_connect missing reason field on denied_host outcome\n%s", line)
		}
		if level, _ := rec["level"].(string); level != "WARN" {
			t.Errorf("mitm_connect level = %q, want %q (non-tunneled outcomes are Warn)\n%s",
				level, "WARN", line)
		}
		// Field-set guard: nothing beyond the §10.2 set + slog
		// builtins. canonical_host is empty for denied_host+signing
		// (canonicalization runs after the signing gate) so the
		// current implementation omits it; if the implementation
		// switches to always-emit-empty-string, this assertion will
		// catch the change and the spec read can be revisited.
		for k := range rec {
			switch k {
			case "msg", "level", "time",
				"outcome", "host", "port", "client_addr", "duration_ms",
				"denied_gate", "reason":
				// ok
			default:
				t.Errorf("mitm_connect carries unexpected field %q\n%s", k, line)
			}
		}
	}
	if !found {
		t.Errorf("no mitm_connect log line emitted; one CONNECT must produce one log line\nlogs:\n%s", sb.String())
	}
}
