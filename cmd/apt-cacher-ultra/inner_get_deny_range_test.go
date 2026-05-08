package main

// SPEC6 §11 F15 + §15 #2 — deny-CIDR fires on inner GET via CONNECT
// tunnel.
//
// §11 F15: "Inner GET upstream fetch fails the SSRF deny-range gate
// at TCP-connect time. The inner GET response is whatever the existing
// Phase 1 fetcher returns (typically 502 with `outcome=upstream_denied`
// on the request log line); tunnel closes after inner response. The
// CONNECT itself succeeded (as designed; §1.1.3)."
//
// §12.4 maps F15 to "12.2 integration (deny-range fires on inner GET)".
//
// AIDEV-NOTE: SPEC drift (slated for §15 #17 spec-sync). The §11 F15
// wording above predates handler.go:1535's actual mapping. The
// fetcher returns ErrTargetDenied when the dialer's ControlContext
// blocks a deny-range IP; handler.respondFetchFailed dispatches
// ErrTargetDenied → http.StatusForbidden (403) + outcome=forbidden
// + fetchAttempted=true (distinguishes from the pre-flight host
// rejection's fetchAttempted=false). The "typically 502 /
// upstream_denied" wording in §11 F15 is hedged ("typically") and
// has no file:line citation, unlike F21/F22 which cite handler.go
// line ranges. The §15 #17 sweep should sync §11 F15 to the
// as-built. This test pins the as-built (403 + forbidden +
// fetchAttempted=true) — what the spec sweep will codify.
//
// Distinct from F21 (upstream invalid cert) and F22 (HTTPS→HTTP
// redirect): F15 fires DURING the dial step (post-DNS, pre-SYN), so
// no TLS handshake is ever attempted upstream. The deny is observable
// only through a successful inner TLS handshake against the cache
// (otherwise the inner GET never gets dispatched), so this test wires
// the whole path:
//
//   1. Boot daemon with TLS MITM enabled, AllowedHostRegex matching
//      "localhost" (CONNECT host gate + handler post-parse host
//      gate), and DenyTargetRanges containing 127.0.0.0/8 (so the
//      dial of localhost:443 → 127.0.0.1 trips the deny in
//      ControlContext).
//   2. Read the auto-generated CA from <cache.dir>/ca/ca.crt.
//   3. CONNECT to the cache for localhost:443; the cache hijacks and
//      mints a leaf cert for localhost on the fly.
//   4. Run a real tls.Client handshake against the hijacked conn,
//      trusting only the auto-CA. Handshake succeeds.
//   5. Send an HTTP/1.1 GET over the encrypted stream. The handler
//      synthesizes RequestURI=https://localhost/<path> + dispatches
//      → fetch.Fetch dials localhost:443 → DNS yields 127.0.0.1 →
//      ControlContext returns errDialDenied → fetch returns
//      ErrTargetDenied → handler.respondFetchFailed emits 403
//      + body "forbidden\n".
//
// Asserts:
//
//   - Inner response status = 403.
//   - Inner response body contains "forbidden".
//   - The request log line for the inner GET has outcome=forbidden,
//     status=403, mitm=true, AND the upstream_status field is
//     present (value 0). Per logRequest at handler.go:1764, the
//     upstream_status field is emitted ONLY when fetchAttempted=true.
//     Its presence here proves the deny-CIDR path fired (which sets
//     fetchAttempted=true) rather than the pre-flight host-allowlist
//     path (fetchAttempted=false). Without this disambiguation, both
//     ErrHostNotAllowed and ErrTargetDenied would surface the same
//     "forbidden" body.
//
// Mutates package-level shutdownTimeout so NOT t.Parallel.

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// prereadInnerConn presents a (conn, bufio.Reader) pair as a single
// net.Conn. After reading the CONNECT response, any bytes the bufio
// reader has buffered ahead of the TLS ClientHello must be drained
// from the buffer first; passing rawConn directly to tls.Client would
// lose them. Mirrors the prereadConn helper in
// internal/proxy/connect_test.go.
type prereadInnerConn struct {
	net.Conn
	br *bufio.Reader
}

func (c *prereadInnerConn) Read(p []byte) (int, error) {
	if c.br != nil && c.br.Buffered() > 0 {
		return c.br.Read(p)
	}
	return c.Conn.Read(p)
}

func TestServe_InnerGET_DenyRangeFiresAtConnectTime_Returns403(t *testing.T) {
	oldTimeout := shutdownTimeout
	shutdownTimeout = 500 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = oldTimeout })

	cacheDir := t.TempDir()
	cfg := minimalCfg(cacheDir, nil)
	// localhost: the CONNECT target host (DNS-resolves to 127.0.0.1
	// reliably across CI hosts). Two gates consult Upstream.AllowedHostRegex
	// here:
	//   - CONNECT FetchGate at internal/proxy/connect.go:460 (cache
	//     refuses to hijack non-allowlisted hosts).
	//   - Handler post-parse host check at handler.go:345 (the
	//     synthetic inner request must pass the same allowlist).
	cfg.Upstream.AllowedHostRegex = append(cfg.Upstream.AllowedHostRegex, `^localhost$`)
	// Re-enable the deny-range that minimalCfg cleared. 127.0.0.0/8
	// covers the DNS-resolved IP for localhost; the dialer's
	// ControlContext is the gate F15 exercises. ::1/128 covers the
	// IPv6 case if DNS yields an AAAA answer first.
	cfg.Upstream.DenyTargetRanges = []string{"127.0.0.0/8", "::1/128"}
	// Tighten retry budget so the test wall time stays small —
	// errDialDenied is non-retryable but the surrounding loop's
	// per-attempt budget would otherwise dominate.
	cfg.Upstream.ConnectTimeout.Duration = 2 * time.Second
	cfg.Upstream.TotalTimeout.Duration = 5 * time.Second
	cfg.Upstream.MaxRetries = 1

	cfg.TlsMitm.Enabled = true
	cfg.TlsMitm.AllowUnconstrainedCA = true
	cfg.TlsMitm.CertCacheSize = 16
	cfg.TlsMitm.LeafCertLifetime.Duration = time.Hour
	cfg.TlsMitm.CACertLifetime.Duration = 30 * 24 * time.Hour
	cfg.TlsMitm.LeafAlgorithm = "ecdsa-p256"

	var sb captureBuilder
	logger := slog.New(slog.NewJSONHandler(&sb, &slog.HandlerOptions{Level: slog.LevelInfo}))

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

	// Read the auto-generated CA cert. wireTlsMitm runs LoadOrGenerate
	// during startup before serveListeners accepts, so by the time
	// waitForDaemonReady returns the file is on disk.
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

	// Inner GET. The handler synthesizes RequestURI=https://localhost/path
	// → parser.Parse → CanonicalHost="localhost" → fetch.Fetch →
	// dialer hits 127.0.0.1 → deny.
	innerReq, _ := http.NewRequest(http.MethodGet, "/dists/noble/InRelease", nil)
	innerReq.Host = "localhost"
	if err := innerReq.Write(tlsClient); err != nil {
		t.Fatalf("write inner GET: %v", err)
	}
	innerResp, err := http.ReadResponse(bufio.NewReader(tlsClient), innerReq)
	if err != nil {
		t.Fatalf("read inner response: %v", err)
	}
	defer innerResp.Body.Close()
	innerBody, _ := io.ReadAll(innerResp.Body)

	if innerResp.StatusCode != http.StatusForbidden {
		t.Fatalf("inner status = %d, want %d; body=%q", innerResp.StatusCode, http.StatusForbidden, innerBody)
	}
	if !strings.Contains(string(innerBody), "forbidden") {
		t.Errorf("inner body = %q, want substring %q", innerBody, "forbidden")
	}

	// Cancel the daemon to flush any deferred goroutine state and
	// guarantee the request log line is in sb before we read.
	cancel()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Errorf("serveListeners: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("serveListeners did not return")
	}

	// Walk the captured slog stream for the inner GET's request log
	// line. Multiple "request" emits can arrive (the readiness probe
	// at start, plus the inner GET); pick the one whose path matches
	// the inner GET. Asserting only on this record avoids coupling to
	// unrelated emits.
	var innerRec map[string]any
	var innerLine string
	for _, line := range strings.Split(strings.TrimSpace(sb.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if msg, _ := rec["msg"].(string); msg != "request" {
			continue
		}
		if path, _ := rec["path"].(string); path != "/dists/noble/InRelease" {
			continue
		}
		innerRec = rec
		innerLine = line
	}
	if innerRec == nil {
		t.Fatalf("no request log line for inner GET path; full capture:\n%s", sb.String())
	}
	if outcome, _ := innerRec["outcome"].(string); outcome != "forbidden" {
		t.Errorf("inner request outcome = %q, want %q\n%s", outcome, "forbidden", innerLine)
	}
	if status, _ := innerRec["status"].(float64); int(status) != http.StatusForbidden {
		t.Errorf("inner request status = %v, want %d\n%s", status, http.StatusForbidden, innerLine)
	}
	if mitm, _ := innerRec["mitm"].(bool); !mitm {
		t.Errorf("inner request mitm marker = %v, want true\n%s", mitm, innerLine)
	}
	// upstream_status presence is the F15 differentiator: logRequest
	// emits this field ONLY when fetchAttempted=true. Its presence
	// proves the rejection happened DURING the dial (ErrTargetDenied
	// path at handler.go:1535-1542) rather than at the pre-flight
	// host-allowlist gate (handler.go:1525-1534, which sets
	// fetchAttempted=false). Both surface body="forbidden\n", so this
	// is the only signal that pins F15's specific path.
	if _, ok := innerRec["upstream_status"]; !ok {
		t.Errorf("inner request log lacks upstream_status field; deny-CIDR rejection should set fetchAttempted=true and emit the field\n%s", innerLine)
	}
}
