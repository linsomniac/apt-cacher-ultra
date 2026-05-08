package main

// SPEC6 §11 F21 + §15 #2 — upstream invalid-cert integration pin.
//
// §11 F21: "Upstream HTTPS server presents an invalid cert (chain
// failure, expired, hostname mismatch). Inner GET fetch fails with
// the existing Phase 1 fetcher behavior — `outcome=bad_gateway` on
// the inner request log line; the cache does NOT relax verification
// (§5.4)."
//
// §12.4 maps F21 to "12.2 integration (upstream invalid cert)".
//
// Drives the path with a plain-HTTP CONNECT-less mirror — the same
// fetch.Client transport, just routed via a [[mirror]] rule. CONNECT
// wiring is not required to exercise the upstream-cert verification
// invariant: any HTTPS upstream that the cache's transport doesn't
// trust trips the same x509-verify failure regardless of whether
// the client got there via plain GET or via a hijacked CONNECT
// inner GET. The shared-transport invariant is what we're pinning;
// CONNECT-specific F-rows (F9, F15) are separate slices.
//
// The seam is set to an EMPTY *x509.CertPool — this guarantees
// verification fails deterministically against the httptest TLS
// server's self-signed cert, regardless of what's in the host's
// system root store. Without the seam, the test would pass on most
// hosts (the httptest CA is ephemeral and never in system roots),
// but a sufficiently customized CI box could insert it and turn
// this test into a false negative. Pinning to an empty pool removes
// that risk entirely.
//
// Asserts:
//
//   - Daemon returns 502 (StatusBadGateway).
//   - Response body contains the "bad gateway" sentinel.
//   - Retry-After header is set per handler.respondFetchFailed.
//   - Upstream's http.Handler is never invoked — the TLS handshake
//     fails before the server sees a request line.

import (
	"context"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
)

func TestServe_HTTPSUpstream_InvalidCert_Returns502(t *testing.T) {
	// Empty pool → no CA trusted. fetch's transport thus rejects any
	// cert, including the httptest TLS server's self-signed leaf.
	// Restored on test teardown.
	restore := fetch.SetRootCAsForTest(x509.NewCertPool())
	t.Cleanup(restore)

	var upstreamHits atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// If we ever reach here, the TLS handshake unexpectedly
		// succeeded — the test should fail by counter assertion below.
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("should not be served"))
	}))
	defer upstream.Close()

	// Mirror the test upstream's HTTPS URL under /ubuntu so a plain
	// GET to the daemon's listener at /ubuntu/<path> fans out to
	// upstream.URL/<path> over TLS.
	cacheDir := t.TempDir()
	cfg := minimalCfg(cacheDir, []config.MirrorRule{
		{Prefix: "/ubuntu", Upstream: upstream.URL + "/"},
	})
	// Tighten the per-fetch budget so 3 retries × handshake-fail still
	// fits in test wall time. 30s default is overkill for a TLS error
	// that resolves in microseconds, but the retry loop's backoff would
	// otherwise stretch this to several seconds.
	cfg.Upstream.ConnectTimeout.Duration = 2 * time.Second
	cfg.Upstream.TotalTimeout.Duration = 5 * time.Second
	cfg.Upstream.MaxRetries = 1

	cacheLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cacheAddr := cacheLn.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	var serveErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		serveErr = serveListeners(ctx, cfg, newTestLogger(), cacheLn, nil, nil)
	}()

	t.Cleanup(func() {
		cancel()
		wg.Wait()
		if serveErr != nil {
			t.Errorf("serveListeners: %v", serveErr)
		}
	})

	resp, body := getNoFollow(t, "http://"+cacheAddr+"/ubuntu/dists/noble/InRelease")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%q", resp.StatusCode, body)
	}
	if !strings.Contains(body, "bad gateway") {
		t.Errorf("body = %q, want substring %q", body, "bad gateway")
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Errorf("Retry-After header missing; handler.respondFetchFailed must set one")
	}
	if got := upstreamHits.Load(); got != 0 {
		t.Errorf("upstream Handler invoked %d time(s); TLS verification failure should fire BEFORE the request line is delivered", got)
	}
}
