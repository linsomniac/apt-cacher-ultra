package main

// SPEC6 §11 F22 + §15 #2 — HTTPS→HTTP redirect is not auto-followed.
//
// §11 F22: "Upstream sends a redirect from `https://` to `http://`
// (or any other 3xx). Inner GET fails with `outcome=bad_gateway`
// (handler.go:1547-1556 maps `fetch.ErrRedirectBlocked` → 502).
// The upstream's 3xx status code is preserved on the request log
// line as `upstream_status`. The cache does NOT silently follow
// the redirect or downgrade the inner request to HTTP; apt sees a
// 502 from the cache rather than the 3xx from upstream. Operators
// whose archive uses redirects configure a Remap rule pointing at
// the redirect target."
//
// §12.4 maps F22 to "12.2 integration (HTTPS→HTTP redirect not
// auto-followed)".
//
// Distinct from F21 (upstream invalid cert): here the TLS handshake
// SUCCEEDS — the seam is set to the test server's cert pool — and
// the rejection happens at the HTTP semantic layer when fetch sees
// the 3xx and refuses to follow. The two pins together prove that:
//
//   - When the server cert is unverified → 502 (F21 path, "bad
//     gateway" via the unclassified-error default).
//   - When the server cert is verified BUT the response is a 3xx
//     → 502 (this F22 path, "bad gateway" via ErrRedirectBlocked).
//
// Asserts:
//
//   - Daemon returns 502 (StatusBadGateway).
//   - Body contains "bad gateway".
//   - Retry-After header is set per handler.respondFetchFailed.
//   - Upstream's http.Handler IS invoked (TLS handshake succeeded
//     and the 301 was sent on the wire). Distinguishes this path
//     from F21's "handler never invoked" assertion — proves the
//     redirect is what triggered the 502, not a verification
//     failure.

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

func TestServe_HTTPSUpstream_Redirect_BadGateway(t *testing.T) {
	var upstreamHits atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		// 301 with a Location pointing to a different scheme. fetch's
		// CheckRedirect refuses ALL redirects regardless of target
		// scheme, so the exact target only matters for the upstream's
		// audit trail; what we care about is that the cache surfaces
		// a 502 instead of following.
		w.Header().Set("Location", "http://example.test/redirected"+r.URL.Path)
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	defer upstream.Close()

	// Trust the test server's cert so the TLS handshake completes and
	// the 3xx actually reaches fetch's CheckRedirect (the only place
	// that converts the 3xx into ErrRedirectBlocked → 502).
	pool := x509.NewCertPool()
	pool.AddCert(upstream.Certificate())
	restore := fetch.SetRootCAsForTest(pool)
	t.Cleanup(restore)

	cacheDir := t.TempDir()
	cfg := minimalCfg(cacheDir, []config.MirrorRule{
		{Prefix: "/ubuntu", Upstream: upstream.URL + "/"},
	})
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
		serveErr = serveListeners(ctx, cfg, newTestLogger(), cacheLn, nil, nil, nil)
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
	// The 3xx must reach the upstream — the cache should NOT have
	// short-circuited before the request line was sent. ErrRedirectBlocked
	// fires AFTER the 301 response is parsed by net/http; if the
	// counter reads 0, the test was actually exercising a different
	// failure mode (e.g. TLS verify) and the F22 invariant wasn't
	// pinned.
	if got := upstreamHits.Load(); got == 0 {
		t.Errorf("upstream Handler invoked %d time(s); F22 needs the request to actually reach upstream so the 3xx is what fails fetch", got)
	}
	// Sanity: the cache must NOT have served the redirect target's
	// path. If the daemon silently followed (regression on §6.4 /
	// fetch.CheckRedirect), the body would be a 200 from the redirect
	// target rather than the 502 above — already asserted, but the
	// Location-target was deliberately a host the daemon would have
	// rejected (^example\.test$ is not in the default allowlist), so
	// even if CheckRedirect were broken the host-allowed pre-check
	// would catch it. The dual-fence is intentional.
}
