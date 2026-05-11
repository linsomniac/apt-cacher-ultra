package main

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/config"
)

// newTestLogger returns a logger that drops everything to /dev/null. The
// servers we wire up emit a handful of structured INFO lines per test;
// keeping them out of `go test` output makes failures legible.
func newTestLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// loadAutoCAPool reads the daemon's auto-generated CA from
// <cacheDir>/ca/ca.crt and returns a *x509.CertPool containing only
// that cert. Used by §15 #2 integration tests that drive a real
// tls.Client handshake against the cache's leaf cert: the client
// must trust the CA the cache used to sign the leaf, which is only
// the auto-CA materialized at startup by wireTlsMitm's
// LoadOrGenerate call. By the time waitForDaemonReady returns, the
// CA file is on disk.
func loadAutoCAPool(t *testing.T, cacheDir string) *x509.CertPool {
	t.Helper()
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
	return pool
}

// minimalCfg builds a defaulted *config.Config pointed at cacheDir, with
// upstream allowlist matching only loopback (so the test's httptest server
// is reachable) and an empty deny-CIDR list (so the default 127.0.0.0/8
// deny does not block the test fetcher). cfg.Cache.Listen is intentionally
// left empty — the test path goes through serveListeners with a bound :0
// listener, not through serve.
func minimalCfg(cacheDir string, mirrors []config.MirrorRule) *config.Config {
	cfg := &config.Config{
		Cache: config.CacheConfig{
			Dir: cacheDir,
		},
		Upstream: config.UpstreamConfig{
			AllowedHostRegex: []string{`^127\.0\.0\.1$`},
			DenyTargetRanges: []string{},
			// Match the production default (set by defaultConfig() in
			// the Load() path). Defaults() leaves bools alone, so tests
			// that bypass Load — like minimalCfg — must set this by
			// hand or get the Go zero (false), which would diverge from
			// what an operator deploying with no config sees.
			AllowHTTPSToHTTPRedirect: true,
		},
		Mirror: mirrors,
	}
	cfg.Defaults()
	return cfg
}

// TestServe_EndToEnd_MirrorFetchAndCache validates the full wire path from
// inbound apt request → handler → fetch → cache → response, plus a second
// hit serving from cache. It also exercises the §9.5 graceful shutdown
// path: cancelling ctx returns serveListeners cleanly within the drain
// budget.
func TestServe_EndToEnd_MirrorFetchAndCache(t *testing.T) {
	t.Parallel()

	const wantBody = "Package: hello\nVersion: 1.0\n"
	var upstreamHits atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/dists/noble/InRelease" {
			http.NotFound(w, r)
			return
		}
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, wantBody)
	}))
	defer upstream.Close()

	cfg := minimalCfg(t.TempDir(), []config.MirrorRule{
		{Prefix: "/ubuntu", Upstream: upstream.URL + "/"},
	})

	// Pre-bind so the test learns the cache's port without racing.
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

	// First request — cache miss, upstream is hit.
	resp, body := getNoFollow(t, "http://"+cacheAddr+"/ubuntu/dists/noble/InRelease")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first GET: status=%d body=%q", resp.StatusCode, body)
	}
	if body != wantBody {
		t.Fatalf("first GET: body=%q want=%q", body, wantBody)
	}
	if got := resp.Header.Get("X-Cache"); got != "MISS" {
		t.Fatalf("first GET: X-Cache=%q want MISS", got)
	}
	if got := upstreamHits.Load(); got != 1 {
		t.Fatalf("after first GET: upstream hits=%d want 1", got)
	}

	// Second request — cache hit; upstream must NOT be touched.
	resp, body = getNoFollow(t, "http://"+cacheAddr+"/ubuntu/dists/noble/InRelease")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second GET: status=%d body=%q", resp.StatusCode, body)
	}
	if body != wantBody {
		t.Fatalf("second GET: body=%q want=%q", body, wantBody)
	}
	if got := resp.Header.Get("X-Cache"); got != "HIT" {
		t.Fatalf("second GET: X-Cache=%q want HIT", got)
	}
	if got := upstreamHits.Load(); got != 1 {
		t.Fatalf("after second GET: upstream hits=%d want 1 (still)", got)
	}

	// Verify the blob actually landed under cache.dir/pool/.
	poolDir := filepath.Join(cfg.Cache.Dir, "pool")
	count := 0
	_ = filepath.Walk(poolDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info == nil || info.IsDir() {
			return nil
		}
		count++
		return nil
	})
	if count == 0 {
		t.Fatalf("expected at least one file under %s", poolDir)
	}
}

// TestServe_GracefulShutdown_DrainsInflight verifies that an in-flight
// upstream fetch finishes (and the response reaches the client) when ctx
// is cancelled mid-fetch. SPEC §9.5 step 2: "Wait up to 30s for in-flight
// requests to drain."
func TestServe_GracefulShutdown_DrainsInflight(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{})
	released := make(chan struct{})
	var once sync.Once
	const wantBody = "slow body"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		<-released
		_, _ = io.WriteString(w, wantBody)
	}))
	defer upstream.Close()

	cfg := minimalCfg(t.TempDir(), []config.MirrorRule{
		{Prefix: "/ubuntu", Upstream: upstream.URL + "/"},
	})

	cacheLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cacheAddr := cacheLn.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	var serveErr error
	go func() {
		defer wg.Done()
		serveErr = serveListeners(ctx, cfg, newTestLogger(), cacheLn, nil, nil, nil)
	}()

	// Kick off the slow request from another goroutine.
	respCh := make(chan struct {
		body string
		err  error
	}, 1)
	go func() {
		client := &http.Client{Timeout: 25 * time.Second}
		resp, err := client.Get("http://" + cacheAddr + "/ubuntu/dists/noble/InRelease")
		if err != nil {
			respCh <- struct {
				body string
				err  error
			}{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		respCh <- struct {
			body string
			err  error
		}{body: string(b)}
	}()

	// Wait until the upstream handler reports the fetch has arrived, so
	// we know the request is genuinely mid-flight before we cancel.
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("upstream never saw the in-flight request")
	}

	// Trigger graceful shutdown with the request still in flight.
	cancel()

	// Release the upstream so the in-flight fetch can finish during the
	// drain window.
	close(released)

	select {
	case got := <-respCh:
		if got.err != nil {
			t.Fatalf("client got error during graceful drain: %v", got.err)
		}
		if got.body != wantBody {
			t.Fatalf("client body=%q want=%q", got.body, wantBody)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("client never got response during graceful drain")
	}

	wg.Wait()
	if serveErr != nil {
		t.Fatalf("serveListeners: %v", serveErr)
	}
}

// TestServe_DisallowedHostReturnsForbidden confirms the SPEC §6.6 short
// circuit reaches the wire: a host that fails the allowlist returns 403
// without ever talking to upstream. Smoke-tests that the handler is wired
// to fetch.HostAllowed.
func TestServe_DisallowedHostReturnsForbidden(t *testing.T) {
	t.Parallel()

	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
	}))
	defer upstream.Close()

	cfg := minimalCfg(t.TempDir(), nil)
	// Restrict allowlist to a host the request will NOT match.
	cfg.Upstream.AllowedHostRegex = []string{`^example\.invalid$`}

	cacheLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cacheAddr := cacheLn.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = serveListeners(ctx, cfg, newTestLogger(), cacheLn, nil, nil, nil)
	}()
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})

	// Use a raw connection so we can write a proxy-form absolute-URI
	// request line. Go's http.Client rewrites the URL before it hits the
	// wire (it uses path-only request lines on a non-proxy client) so the
	// only way to send a literal `GET http://host/...` request line is to
	// speak HTTP/1.1 ourselves.
	conn, err := net.Dial("tcp", cacheAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = conn.Write([]byte("GET http://archive.ubuntu.com/ubuntu/InRelease HTTP/1.1\r\n" +
		"Host: archive.ubuntu.com\r\n" +
		"Connection: close\r\n" +
		"\r\n"))

	buf := make([]byte, 4096)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := conn.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read: %v", err)
	}
	resp := string(buf[:n])
	if !strings.HasPrefix(resp, "HTTP/1.1 403 ") {
		t.Fatalf("expected 403 status line, got: %q", firstLine(resp))
	}
	if got := upstreamHits.Load(); got != 0 {
		t.Fatalf("upstream was called %d times; expected 0", got)
	}
}

// TestServe_ReturnsListenError verifies the listener-error path (a port
// that becomes unusable mid-flight) propagates back from serveListeners.
// We simulate by closing the listener directly: Serve returns the close
// error and the goroutine reports it through errCh.
func TestServe_ReturnsListenError(t *testing.T) {
	t.Parallel()

	cfg := minimalCfg(t.TempDir(), nil)

	cacheLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- serveListeners(ctx, cfg, newTestLogger(), cacheLn, nil, nil, nil)
	}()

	// Give the goroutine a moment to enter Serve(), then close the listener
	// out from under it. Serve() will return a "use of closed network
	// connection" error which the goroutine should turn into errCh != nil.
	// AIDEV-NOTE: a 50ms delay races serve startup; if the listener closes
	// before Serve() runs the goroutine still surfaces the error via the
	// initial Serve() call returning the close error directly.
	time.Sleep(50 * time.Millisecond)
	_ = cacheLn.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("serveListeners returned nil after listener close; want non-nil")
		}
		if !strings.Contains(err.Error(), "http") {
			t.Fatalf("error should be wrapped with 'http' prefix: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("serveListeners did not return after listener close")
	}
}

// getNoFollow performs a GET without following redirects and returns the
// response and body. Fails the test on transport error.
func getNoFollow(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body %s: %v", url, err)
	}
	return resp, string(body)
}

func firstLine(s string) string {
	if i := strings.Index(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// TestServe_GracefulShutdown_KillsHungFetchAfterDrainBudget proves the
// SPEC §9.5 step 3 contract: when the drain budget elapses with a fetch
// still in flight, serveListeners cancels the fetch and returns instead
// of riding out fetch.TotalTimeout (5min default).
//
// We override shutdownTimeout to 200ms; without the lifecycle-ctx fix
// the hung fetch would block serveListeners for the full TotalTimeout
// (5min in production, 5s in tests via the default fetch.New). With
// the fix, serveListeners returns within ~1s of cancel.
func TestServe_GracefulShutdown_KillsHungFetchAfterDrainBudget(t *testing.T) {
	// Intentionally NOT t.Parallel: this test mutates the package-level
	// shutdownTimeout var, which other tests in this package read while
	// they run their own shutdown path. Running in parallel would race.

	oldTimeout := shutdownTimeout
	shutdownTimeout = 200 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = oldTimeout })

	entered := make(chan struct{})
	var once sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		// Block until the upstream's request ctx is cancelled, which
		// happens when the cache cancels its fetch (lifecycle ctx
		// cancel propagates through fetch.Fetch into the http
		// transport, which closes the conn, which fires the request
		// ctx). If the lifecycle fix is missing, this never fires
		// during the drain window and the test times out.
		<-r.Context().Done()
	}))
	defer upstream.Close()

	cfg := minimalCfg(t.TempDir(), []config.MirrorRule{
		{Prefix: "/ubuntu", Upstream: upstream.URL + "/"},
	})

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

	// Kick off the request. We do not care about its body — only that
	// it reaches the upstream so we know the fetch is in flight.
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		client := &http.Client{Timeout: 30 * time.Second}
		resp, _ := client.Get("http://" + cacheAddr + "/ubuntu/dists/noble/InRelease")
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("upstream never observed the in-flight request")
	}

	// Trigger shutdown. The fetch is hung; the 200ms drain budget will
	// expire; serveListeners must then cancel the fetch and return.
	shutdownStart := time.Now()
	cancel()

	select {
	case err := <-serveDone:
		dur := time.Since(shutdownStart)
		if err != nil {
			t.Errorf("serveListeners: %v", err)
		}
		// 5s ceiling is generous: drain budget 200ms + lifecycle
		// cancel propagation should be sub-second. fetch.TotalTimeout
		// is 5m by default config; if lifecycle cancel did not work,
		// we would block ~5m. 5s catches a regression cleanly.
		if dur > 5*time.Second {
			t.Errorf("serveListeners returned in %v; expected sub-second after drain timeout", dur)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("serveListeners did not return after drain timeout (lifecycle cancel broken?)")
	}

	// The client request returns with whatever response the cache
	// generated (5xx). Wait for cleanup so the t.Parallel goroutine
	// budget is honored.
	<-clientDone
}

// TestServe_GracefulShutdown_KillsSlowClientAfterDrainBudget proves the
// shutdown ordering: when a handler is wedged writing to a slow/non-
// reading client (cache-hit ServeContent), force-closing the http.Server
// before h.Close() is what unsticks the handler. lifecycleCancel does
// nothing for a write-blocked goroutine. If a future refactor moves
// h.Close() back ahead of Server.Close(), this test deadlocks instead
// of returning — the existing hung-upstream test would not catch that.
func TestServe_GracefulShutdown_KillsSlowClientAfterDrainBudget(t *testing.T) {
	// Intentionally NOT t.Parallel: mutates package-global shutdownTimeout.

	oldTimeout := shutdownTimeout
	shutdownTimeout = 200 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = oldTimeout })

	// Body sized to overflow a small TCP receive window with margin to
	// spare. We pin the slow client's SO_RCVBUF below to make this
	// reliable across kernel auto-tuning.
	const bodySize = 4 * 1024 * 1024
	bigBody := make([]byte, bodySize)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(bodySize))
		_, _ = w.Write(bigBody)
	}))
	defer upstream.Close()

	cfg := minimalCfg(t.TempDir(), []config.MirrorRule{
		{Prefix: "/ubuntu", Upstream: upstream.URL + "/"},
	})

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

	// Warm the cache so the slow-client request below is a cache HIT,
	// exercising the ServeContent path codex called out specifically.
	warm := &http.Client{Transport: &http.Transport{}, Timeout: 30 * time.Second}
	warmResp, err := warm.Get("http://" + cacheAddr + "/ubuntu/big.bin")
	if err != nil {
		t.Fatalf("warm get: %v", err)
	}
	if _, err := io.Copy(io.Discard, warmResp.Body); err != nil {
		t.Fatalf("warm read: %v", err)
	}
	_ = warmResp.Body.Close()
	warm.CloseIdleConnections()

	// Slow-reader: dial raw, pin a tiny receive buffer so the kernel
	// cannot absorb the whole body via auto-tuning, send the request,
	// and never read the response. The server's write into the conn
	// will block once the receive window collapses to zero.
	slow, err := net.Dial("tcp", cacheAddr)
	if err != nil {
		t.Fatalf("slow dial: %v", err)
	}
	defer func() { _ = slow.Close() }()
	if tc, ok := slow.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(8192)
	}

	if _, err := slow.Write([]byte("GET /ubuntu/big.bin HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatalf("slow write: %v", err)
	}

	// Brief delay for the handler to start serving and wedge on the
	// conn. We have no clean signal for "now blocked"; 300ms is
	// generous against scheduler jitter without slowing the test.
	time.Sleep(300 * time.Millisecond)

	shutdownStart := time.Now()
	cancel()

	select {
	case err := <-serveDone:
		dur := time.Since(shutdownStart)
		if err != nil {
			t.Errorf("serveListeners: %v", err)
		}
		// Drain budget is 200ms; force-close adds only ms. Anything
		// above ~5s indicates the slow-client path is no longer
		// being unstuck — i.e. a regression of the ordering fix.
		if dur > 5*time.Second {
			t.Errorf("serveListeners took %v after shutdown; ordering regression?", dur)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("serveListeners did not return after shutdown — slow-client deadlock")
	}
}
