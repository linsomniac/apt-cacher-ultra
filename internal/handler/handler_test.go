package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
)

// silentLogger discards all log output so test failures do not get drowned
// in per-request log lines.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestHandler wires a fresh cache + fetch.Client + proxy.Parser into
// a Handler. mirror is optional and applied only when non-nil. The
// fetch.Client allows 127.0.0.1 (the httptest bind address) and skips
// deny-CIDR enforcement, so loopback fetches reach the test server.
func newTestHandler(t *testing.T, allow []string, mirror []config.MirrorRule) *Handler {
	t.Helper()

	parser, err := proxy.New(nil, mirror)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	c, err := cache.Open(context.Background(), t.TempDir(), silentLogger())
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if allow == nil {
		allow = []string{`^127\.0\.0\.1$`}
	}
	fc, err := fetch.New(fetch.Options{
		ConnectTimeout:   2 * time.Second,
		TotalTimeout:     5 * time.Second,
		MaxRetries:       2,
		AllowedHostRegex: allow,
		DenyTargetRanges: nil,
		Logger:           silentLogger(),
	})
	if err != nil {
		t.Fatalf("fetch.New: %v", err)
	}

	h, err := New(Config{
		Parser:      parser,
		Cache:       c,
		Fetch:       fc,
		HostLimiter: hostsem.New(4),
		Logger:      silentLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

// proxyReq builds an apt proxy-mode request: absolute-URI request line.
func proxyReq(method, srvURL, path string) *http.Request {
	return httptest.NewRequest(method, srvURL+path, nil)
}

// mirrorReq builds an apt mirror-mode request: abs_path request line.
func mirrorReq(method, path string) *http.Request {
	return httptest.NewRequest(method, path, nil)
}

func TestNew_NilDeps(t *testing.T) {
	c, err := cache.Open(context.Background(), t.TempDir(), silentLogger())
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	defer c.Close()
	parser, _ := proxy.New(nil, nil)
	fc, _ := fetch.New(fetch.Options{AllowedHostRegex: []string{`.`}, DenyTargetRanges: nil, Logger: silentLogger()})

	cases := []struct {
		name string
		cfg  Config
	}{
		{"nil parser", Config{Cache: c, Fetch: fc, HostLimiter: hostsem.New(4)}},
		{"nil cache", Config{Parser: parser, Fetch: fc, HostLimiter: hostsem.New(4)}},
		{"nil fetch", Config{Parser: parser, Cache: c, HostLimiter: hostsem.New(4)}},
		{"nil limiter", Config{Parser: parser, Cache: c, Fetch: fc}},
	}
	for _, tc := range cases {
		if _, err := New(tc.cfg); err == nil {
			t.Errorf("%s: expected error, got nil", tc.name)
		}
	}
}

func TestServeHTTP_GetMissThenHit(t *testing.T) {
	body := []byte("hello world")
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.Write(body)
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, nil)

	// First request: miss.
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, proxyReq("GET", srv.URL, "/foo"))
	if rec1.Code != http.StatusOK {
		t.Fatalf("miss: status=%d body=%q", rec1.Code, rec1.Body.String())
	}
	if got := rec1.Header().Get("X-Cache"); got != "MISS" {
		t.Errorf("X-Cache miss: %q, want MISS", got)
	}
	if rec1.Body.String() != string(body) {
		t.Errorf("miss body=%q, want %q", rec1.Body.String(), body)
	}

	// Second request: hit.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, proxyReq("GET", srv.URL, "/foo"))
	if rec2.Code != http.StatusOK {
		t.Fatalf("hit: status=%d body=%q", rec2.Code, rec2.Body.String())
	}
	if got := rec2.Header().Get("X-Cache"); got != "HIT" {
		t.Errorf("X-Cache hit: %q, want HIT", got)
	}
	if rec2.Body.String() != string(body) {
		t.Errorf("hit body=%q, want %q", rec2.Body.String(), body)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("upstream hits=%d, want 1 (second request must hit cache)", got)
	}
}

func TestServeHTTP_HEAD_HitNoBody(t *testing.T) {
	body := []byte("payload bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.Write(body)
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, nil)

	// Prime cache with GET.
	primer := httptest.NewRecorder()
	h.ServeHTTP(primer, proxyReq("GET", srv.URL, "/x"))
	if primer.Code != http.StatusOK {
		t.Fatalf("prime: %d %q", primer.Code, primer.Body.String())
	}

	// HEAD: same headers, empty body.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("HEAD", srv.URL, "/x"))
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD: %d", rec.Code)
	}
	if got := rec.Header().Get("X-Cache"); got != "HIT" {
		t.Errorf("X-Cache: %q, want HIT", got)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD body should be empty, got %d bytes", rec.Body.Len())
	}
	if got := rec.Header().Get("Content-Length"); got != fmt.Sprint(len(body)) {
		t.Errorf("Content-Length: %q, want %d", got, len(body))
	}
}

func TestServeHTTP_PostNotAllowed(t *testing.T) {
	h := newTestHandler(t, nil, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "http://127.0.0.1/x", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status=%d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
	if got := rec.Header().Get("Allow"); !strings.Contains(got, "GET") || !strings.Contains(got, "HEAD") {
		t.Errorf("Allow: %q, want GET, HEAD", got)
	}
}

func TestServeHTTP_BadURI(t *testing.T) {
	h := newTestHandler(t, nil, nil)
	rec := httptest.NewRecorder()
	// Construct a request the parser will reject (relative path that
	// matches no mirror, and no [[mirror]] is configured).
	r := httptest.NewRequest("GET", "/no-mirror/x", nil)
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d body=%q, want 400", rec.Code, rec.Body.String())
	}
}

func TestServeHTTP_HostNotAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("never reached"))
	}))
	defer srv.Close()

	// Allow list deliberately excludes 127.0.0.1.
	h := newTestHandler(t, []string{`^example\.com$`}, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/x"))
	if rec.Code != http.StatusForbidden {
		t.Errorf("status=%d body=%q, want 403", rec.Code, rec.Body.String())
	}
}

func TestServeHTTP_Upstream404Passthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/missing"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d body=%q, want 404", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Upstream-Status"); got != "404" {
		t.Errorf("X-Upstream-Status: %q, want 404", got)
	}
}

// TestServeHTTP_UpstreamNon4xxIs502 covers the codex-finding restriction:
// only upstream 4xx is "client said no, pass it through." Anything else
// classified as ErrUpstreamStatus (e.g. a 304 in response to a request
// that did not carry If-None-Match) is treated as cache-side unhealthy
// and surfaced as 502.
func TestServeHTTP_UpstreamNon4xxIs502(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotModified) // 304 — non-2xx, non-4xx, non-5xx
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/x"))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status=%d, want 502 (304 must not pass through)", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Errorf("Retry-After missing on 502")
	}
	// Diagnostic header still records the upstream's response.
	if got := rec.Header().Get("X-Upstream-Status"); got != "304" {
		t.Errorf("X-Upstream-Status=%q, want 304", got)
	}
}

func TestServeHTTP_Upstream5xxThen502(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "kaboom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/boom"))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status=%d body=%q, want 502", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Errorf("Retry-After should be set on 502")
	}
}

func TestServeHTTP_RangeOnHit(t *testing.T) {
	body := []byte("ABCDEFGHIJKL") // 12 bytes
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.Write(body)
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, nil)

	// Prime cache.
	primer := httptest.NewRecorder()
	h.ServeHTTP(primer, proxyReq("GET", srv.URL, "/blob"))
	if primer.Code != http.StatusOK {
		t.Fatalf("prime: %d", primer.Code)
	}

	// Range request — bytes 2-5 inclusive ("CDEF").
	r := proxyReq("GET", srv.URL, "/blob")
	r.Header.Set("Range", "bytes=2-5")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("Range: status=%d, want 206", rec.Code)
	}
	if rec.Body.String() != "CDEF" {
		t.Errorf("Range body=%q, want %q", rec.Body.String(), "CDEF")
	}
	if got := rec.Header().Get("Content-Range"); !strings.HasPrefix(got, "bytes 2-5/") {
		t.Errorf("Content-Range: %q, want prefix %q", got, "bytes 2-5/")
	}
}

func TestServeHTTP_MirrorMode(t *testing.T) {
	body := []byte("mirror payload")
	var seenPath atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath.Store(r.URL.Path)
		w.Write(body)
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, []config.MirrorRule{
		{Prefix: "/repo", Upstream: srv.URL},
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, mirrorReq("GET", "/repo/dists/stable/Release"))
	if rec.Code != http.StatusOK {
		t.Fatalf("mirror: status=%d body=%q", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != string(body) {
		t.Errorf("body=%q, want %q", rec.Body.String(), body)
	}
	if got := seenPath.Load(); got != "/dists/stable/Release" {
		t.Errorf("upstream path=%v, want %q (prefix /repo must be stripped)", got, "/dists/stable/Release")
	}
}

func TestServeHTTP_Coalesced(t *testing.T) {
	body := []byte("only-one-fetch")
	var hits atomic.Int32
	upstreamEntered := make(chan struct{})
	gate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			close(upstreamEntered)
		}
		<-gate
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.Write(body)
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, nil)

	results := make(chan *httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup

	// Leader.
	wg.Add(1)
	go func() {
		defer wg.Done()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/coalesce"))
		results <- rec
	}()

	// Wait for the leader's fetch to actually be in flight against the
	// upstream. At this point the leader is parked in fetch.Fetch and
	// the singleflight call is registered.
	select {
	case <-upstreamEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never received the leader's request")
	}

	// Waiter.
	wg.Add(1)
	go func() {
		defer wg.Done()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/coalesce"))
		results <- rec
	}()

	// AIDEV-NOTE: timing-sensitive sleep so the waiter has time to enter
	// sf.Do (cache lookup is sub-millisecond on every reasonable host).
	// If this becomes flaky on CI, raise to 200ms — there is no clean
	// "park-on-condition" hook without exposing handler internals.
	time.Sleep(100 * time.Millisecond)

	close(gate)
	wg.Wait()
	close(results)

	if got := hits.Load(); got != 1 {
		t.Errorf("upstream hits=%d, want 1 (singleflight should coalesce)", got)
	}

	var miss, coalesced int
	for r := range results {
		if r.Code != http.StatusOK {
			t.Errorf("status=%d body=%q", r.Code, r.Body.String())
			continue
		}
		if r.Body.String() != string(body) {
			t.Errorf("body=%q, want %q", r.Body.String(), body)
		}
		switch r.Header().Get("X-Cache") {
		case "MISS":
			miss++
		case "HIT-COALESCED":
			coalesced++
		default:
			t.Errorf("unexpected X-Cache=%q", r.Header().Get("X-Cache"))
		}
	}
	if miss != 1 || coalesced != 1 {
		t.Errorf("X-Cache distribution: miss=%d coalesced=%d, want 1/1", miss, coalesced)
	}
}

func TestServeHTTP_LookupErrorFallsThroughToFetch(t *testing.T) {
	// The tryCacheHit path treats an unexpected lookup error as
	// "drop into miss." This is hard to provoke with a real cache,
	// so we just confirm a normal-flow miss after a fresh open works
	// (regression check in case the fall-through logic regresses to
	// returning early).
	body := []byte("ok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/x"))
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestServeHTTP_RequestCountIncrementsOnHit(t *testing.T) {
	body := []byte("counted")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, nil)

	// Prime.
	primer := httptest.NewRecorder()
	h.ServeHTTP(primer, proxyReq("GET", srv.URL, "/c"))

	// Two more reads — count increments are async, so wait briefly
	// for the writer goroutine to flush.
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/c"))
	}

	// Poll for the row to reflect the increments. The TouchURLPath
	// goroutine submits to the cache writer asynchronously, so we
	// can't read the count immediately after ServeHTTP returns.
	deadline := time.Now().Add(2 * time.Second)
	var got int64
	for time.Now().Before(deadline) {
		row, err := h.cache.LookupURL(context.Background(), "http", "127.0.0.1", "/c")
		if err != nil {
			t.Fatalf("LookupURL: %v", err)
		}
		got = row.RequestCount
		if got >= 4 { // 1 from miss-write, +3 from hits
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("RequestCount=%d, want >=4", got)
}

func TestSFGroup_CoalescesAndCleansUp(t *testing.T) {
	g := newSFGroup()

	var calls atomic.Int32
	gate := make(chan struct{})

	leader := make(chan sfResult, 1)
	go func() {
		res, _, _ := g.Do("k", func() sfResult {
			calls.Add(1)
			<-gate
			return sfResult{blobHash: "hash", size: 10}
		})
		leader <- res
	}()

	// Wait for leader to start.
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}

	waiter := make(chan struct {
		res    sfResult
		shared bool
	}, 1)
	go func() {
		res, shared, _ := g.Do("k", func() sfResult {
			calls.Add(1)
			return sfResult{}
		})
		waiter <- struct {
			res    sfResult
			shared bool
		}{res, shared}
	}()

	// Give waiter a moment to register.
	time.Sleep(20 * time.Millisecond)

	close(gate)

	leaderRes := <-leader
	w := <-waiter

	if calls.Load() != 1 {
		t.Errorf("calls=%d, want 1 (waiter should not run fn)", calls.Load())
	}
	if !w.shared {
		t.Errorf("waiter shared=false, want true")
	}
	if w.res.blobHash != "hash" || w.res.size != 10 {
		t.Errorf("waiter got %+v, want hash=%q size=%d", w.res, "hash", 10)
	}
	if leaderRes.blobHash != "hash" {
		t.Errorf("leader got %+v, want hash=%q", leaderRes, "hash")
	}

	// After the call drains, the next caller for "k" leads (calls map
	// emptied).
	res, shared, _ := g.Do("k", func() sfResult { return sfResult{blobHash: "next"} })
	if shared {
		t.Errorf("shared=true after first call drained, want false")
	}
	if res.blobHash != "next" {
		t.Errorf("blobHash=%q, want next", res.blobHash)
	}
}

// TestSFGroup_ArrivalDuringCleanupWindow proves that a caller arriving
// after the leader's wg.Done but before the map cleanup still coalesces
// onto the leader's result. Without the Done-before-delete ordering,
// such a caller would lead a duplicate fn execution.
func TestSFGroup_ArrivalDuringCleanupWindow(t *testing.T) {
	g := newSFGroup()

	var fnCalls atomic.Int32
	leaderInCleanup := make(chan struct{}, 1)
	proceed := make(chan struct{})
	sfTestHookAfterDone = func() {
		select {
		case leaderInCleanup <- struct{}{}:
		default:
		}
		<-proceed
	}
	t.Cleanup(func() { sfTestHookAfterDone = nil })

	leaderResult := make(chan sfResult, 1)
	go func() {
		res, _, _ := g.Do("k", func() sfResult {
			fnCalls.Add(1)
			return sfResult{blobHash: "leader"}
		})
		leaderResult <- res
	}()

	select {
	case <-leaderInCleanup:
	case <-time.After(2 * time.Second):
		t.Fatal("leader never reached the cleanup hook")
	}

	type waiterRet struct {
		res    sfResult
		shared bool
	}
	waiterResult := make(chan waiterRet, 1)
	go func() {
		res, shared, _ := g.Do("k", func() sfResult {
			fnCalls.Add(1) // must NOT happen with the fix in place
			return sfResult{blobHash: "duplicate"}
		})
		waiterResult <- waiterRet{res, shared}
	}()

	// Give the waiter time to actually enter Do. With Done-before-delete,
	// it finds the still-mapped call and joins via wg.Wait.
	time.Sleep(50 * time.Millisecond)

	close(proceed)

	leader := <-leaderResult
	waiter := <-waiterResult

	if got := fnCalls.Load(); got != 1 {
		t.Errorf("fn calls=%d, want 1 (waiter must coalesce, not lead a duplicate)", got)
	}
	if !waiter.shared {
		t.Errorf("waiter shared=false, want true")
	}
	if waiter.res.blobHash != "leader" {
		t.Errorf("waiter blobHash=%q, want %q", waiter.res.blobHash, "leader")
	}
	if leader.blobHash != "leader" {
		t.Errorf("leader blobHash=%q, want %q", leader.blobHash, "leader")
	}
}

// TestServeHTTP_DisallowedHostShortCircuits asserts the handler's
// pre-fetch allowlist check rejects unknown hosts before allocating any
// per-host bookkeeping. This is the open-proxy DoS hardening from
// codex finding #2 — without it, an attacker could grow handler-side
// maps by sending requests for many distinct disallowed hostnames.
func TestServeHTTP_DisallowedHostShortCircuits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("never reached"))
	}))
	defer srv.Close()

	h := newTestHandler(t, []string{`^example\.com$`}, nil)

	// 50 distinct made-up hosts the allowlist will reject.
	for i := 0; i < 50; i++ {
		r := httptest.NewRequest("GET",
			fmt.Sprintf("http://attacker-%d.invalid/x", i), nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("attacker-%d: status=%d, want 403", i, rec.Code)
		}
	}

	if got := h.sem.HostCount(); got != 0 {
		t.Errorf("HostCount after 50 disallowed hosts = %d, want 0", got)
	}
}

// TestURLForLog_StripsUserinfo guards against credentials reaching log
// output via the request-URL field. The parser rejects userinfo for
// successful requests, but 400/405 log lines fire before the parser
// runs, so the sanitizer must do the right thing on its own.
func TestURLForLog_StripsUserinfo(t *testing.T) {
	cases := []struct {
		name string
		url  string
		// substrings that MUST NOT appear in the sanitized output
		mustNotContain []string
		// optional: substring that MUST appear (path survives)
		mustContain string
	}{
		{
			name:           "username only",
			url:            "http://abc123@archive.ubuntu.com/foo",
			mustNotContain: []string{"abc123", "@"},
			mustContain:    "/foo",
		},
		{
			name:           "username and password",
			url:            "http://abc123:hunter2@archive.ubuntu.com/foo",
			mustNotContain: []string{"abc123", "hunter2", "@"},
			mustContain:    "/foo",
		},
		{
			name:           "no userinfo passes through",
			url:            "http://archive.ubuntu.com/foo",
			mustNotContain: nil,
			mustContain:    "archive.ubuntu.com",
		},
		{
			name:           "mirror form",
			url:            "/ubuntu/foo",
			mustNotContain: nil,
			mustContain:    "/ubuntu/foo",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", tc.url, nil)
			got := urlForLog(r)
			for _, leak := range tc.mustNotContain {
				if strings.Contains(got, leak) {
					t.Errorf("urlForLog(%q) = %q, must not contain %q", tc.url, got, leak)
				}
			}
			if tc.mustContain != "" && !strings.Contains(got, tc.mustContain) {
				t.Errorf("urlForLog(%q) = %q, must contain %q", tc.url, got, tc.mustContain)
			}
		})
	}
}

// confirmFetchStatusError keeps the public-API contract front-and-center
// for handler maintainers: passthrough of upstream 4xx depends on the
// fetch package returning a *fetch.StatusError matchable via errors.Is.
func TestServeHTTP_FetchStatusErrorMatchable(t *testing.T) {
	se := &fetch.StatusError{Code: 451}
	if !errors.Is(se, fetch.ErrUpstreamStatus) {
		t.Errorf("fetch.StatusError must match ErrUpstreamStatus via errors.Is")
	}
	var got *fetch.StatusError
	if !errors.As(se, &got) {
		t.Errorf("fetch.StatusError must be reachable via errors.As")
	}
	if got != nil && got.Code != 451 {
		t.Errorf("StatusError.Code=%d, want 451", got.Code)
	}
}

// TestHandler_Close_AbortsInFlightFetch proves the SPEC §9.5 step 3 fix:
// Close() cancels the lifecycle ctx, which propagates into the in-flight
// upstream fetch and lets it return promptly. Without the fix, the fetch
// would ride out fetch.TotalTimeout (5s in newTestHandler) regardless of
// shutdown — and in production, the default 5m, which violates the SPEC
// drain-budget contract.
func TestHandler_Close_AbortsInFlightFetch(t *testing.T) {
	entered := make(chan struct{})
	var once sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		// Block until the request ctx is cancelled. If the fix is in
		// place, the handler's lifecycle ctx cancellation propagates
		// through fetch.Fetch into our request ctx via http.Transport.
		<-r.Context().Done()
	}))
	defer upstream.Close()

	h := newTestHandler(t, nil, nil)

	rr := httptest.NewRecorder()
	req := proxyReq(http.MethodGet, upstream.URL, "/path/to/file")

	served := make(chan struct{})
	go func() {
		h.ServeHTTP(rr, req)
		close(served)
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("upstream never observed the request")
	}

	closeStart := time.Now()
	h.Close()
	closeDur := time.Since(closeStart)

	// fetch.TotalTimeout is 5s in newTestHandler. If lifecycle
	// cancellation does not actually reach fetch, the fetch waits out
	// its full deadline and Close blocks for the same duration via
	// activeWG.Wait. A 2s ceiling proves the cancel path works.
	if closeDur > 2*time.Second {
		t.Errorf("Close took %v; expected <2s with lifecycle cancel reaching fetch", closeDur)
	}

	select {
	case <-served:
	case <-time.After(2 * time.Second):
		t.Fatalf("ServeHTTP did not return after Close")
	}

	// Cancelled fetch maps to 503 in respondError (ctx.Canceled branch).
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d, want 503 after lifecycle cancel", rr.Code)
	}
}

// TestHandler_Close_Idempotent verifies that calling Close twice is
// safe — lifecycleCancel is idempotent and activeWG.Wait returns
// immediately when the counter is already zero.
func TestHandler_Close_Idempotent(t *testing.T) {
	h := newTestHandler(t, nil, nil)
	h.Close()
	h.Close() // must not panic or hang
}

// recordingFreshness is a FreshnessChecker test double that records
// every Check() call and signals when a call has been observed.
type recordingFreshness struct {
	mu     sync.Mutex
	calls  []freshCall
	signal chan struct{}
}

type freshCall struct {
	scheme, host, suitePath string
}

func newRecordingFreshness() *recordingFreshness {
	return &recordingFreshness{signal: make(chan struct{}, 16)}
}

func (r *recordingFreshness) Check(_ context.Context, scheme, host, suitePath string) {
	r.mu.Lock()
	r.calls = append(r.calls, freshCall{scheme, host, suitePath})
	r.mu.Unlock()
	select {
	case r.signal <- struct{}{}:
	default:
	}
}

func (r *recordingFreshness) snapshot() []freshCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]freshCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// newTestHandlerWithFreshness wires the same dependencies as
// newTestHandler but plugs in a FreshnessChecker so T1 triggers can be
// observed.
func newTestHandlerWithFreshness(t *testing.T, fc FreshnessChecker) *Handler {
	t.Helper()
	parser, err := proxy.New(nil, nil)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	c, err := cache.Open(context.Background(), t.TempDir(), silentLogger())
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	fetchClient, err := fetch.New(fetch.Options{
		ConnectTimeout:   2 * time.Second,
		TotalTimeout:     5 * time.Second,
		MaxRetries:       2,
		AllowedHostRegex: []string{`^127\.0\.0\.1$`},
		DenyTargetRanges: nil,
		Logger:           silentLogger(),
	})
	if err != nil {
		t.Fatalf("fetch.New: %v", err)
	}
	h, err := New(Config{
		Parser:      parser,
		Cache:       c,
		Fetch:       fetchClient,
		HostLimiter: hostsem.New(4),
		Logger:      silentLogger(),
		Freshness:   fc,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

// TestServeHTTP_MissOnInReleaseSeedsFreshness asserts the codex fix:
// after a successful miss-fetch of a suite's InRelease, the cache
// upserts a suite_freshness row carrying the upstream validators —
// so the periodic scheduler picks the suite up immediately and the
// first follow-up freshness check can issue a real conditional GET
// (instead of an unconditional one because validators are nil).
func TestServeHTTP_MissOnInReleaseSeedsFreshness(t *testing.T) {
	body := []byte("Origin: Ubuntu\nSuite: noble\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Last-Modified", "Mon, 01 Jan 2024 00:00:00 GMT")
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, nil)
	defer h.Close()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/ubuntu/dists/noble/InRelease"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}

	got, err := h.cache.GetSuiteFreshness(context.Background(), "http", "127.0.0.1", "/ubuntu/dists/noble")
	if err != nil {
		t.Fatalf("GetSuiteFreshness after miss: %v", err)
	}
	if got.LastSuccessAt == nil {
		t.Errorf("seed missing last_success_at")
	}
	if got.InReleaseETag == nil || *got.InReleaseETag != `"v1"` {
		t.Errorf("seed etag = %v, want \"v1\"", got.InReleaseETag)
	}
	if got.InReleaseLastMod == nil || *got.InReleaseLastMod != "Mon, 01 Jan 2024 00:00:00 GMT" {
		t.Errorf("seed lastmod = %v", got.InReleaseLastMod)
	}
}

// TestServeHTTP_MissOnPackagesDoesNotSeedFreshness asserts the seed
// fires only for the InRelease anchor file, not for every metadata
// path under a suite.
func TestServeHTTP_MissOnPackagesDoesNotSeedFreshness(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("Packages content"))
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, nil)
	defer h.Close()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/ubuntu/dists/noble/main/binary-amd64/Packages"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}

	_, err := h.cache.GetSuiteFreshness(context.Background(), "http", "127.0.0.1", "/ubuntu/dists/noble")
	if !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound (Packages must not seed)", err)
	}
}

// TestServeHTTP_FreshnessTriggeredOnMetadataHit asserts that a cache
// hit on an InRelease (metadata under a suite) fires the SPEC §7.1 T1
// trigger with the right (scheme, host, suite_path).
func TestServeHTTP_FreshnessTriggeredOnMetadataHit(t *testing.T) {
	body := []byte("Origin: Ubuntu\nSuite: noble\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	rec := newRecordingFreshness()
	h := newTestHandlerWithFreshness(t, rec)
	defer h.Close()

	// First request caches the file (miss path; touchAsync runs but
	// freshness must not fire on miss).
	miss := httptest.NewRecorder()
	h.ServeHTTP(miss, proxyReq("GET", srv.URL, "/ubuntu/dists/noble/InRelease"))
	if miss.Code != http.StatusOK {
		t.Fatalf("miss status=%d", miss.Code)
	}

	// Second request: cache hit on metadata → T1 should fire.
	hit := httptest.NewRecorder()
	h.ServeHTTP(hit, proxyReq("GET", srv.URL, "/ubuntu/dists/noble/InRelease"))
	if hit.Code != http.StatusOK || hit.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("hit status=%d X-Cache=%q", hit.Code, hit.Header().Get("X-Cache"))
	}

	select {
	case <-rec.signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("freshness Check never fired after metadata hit")
	}

	calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1: %+v", len(calls), calls)
	}
	got := calls[0]
	if got.scheme != "http" || got.host != "127.0.0.1" || got.suitePath != "/ubuntu/dists/noble" {
		t.Errorf("call = %+v, want {http, 127.0.0.1, /ubuntu/dists/noble}", got)
	}
}

// TestServeHTTP_FreshnessNotTriggeredOnBlobHit asserts that a hit on a
// non-metadata path (a .deb) does not fire the T1 trigger.
func TestServeHTTP_FreshnessNotTriggeredOnBlobHit(t *testing.T) {
	body := []byte("fake .deb body")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	rec := newRecordingFreshness()
	h := newTestHandlerWithFreshness(t, rec)
	defer h.Close()

	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, proxyReq("GET", srv.URL, "/ubuntu/pool/main/p/pkg/file_1.0_amd64.deb"))
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: status=%d", i, w.Code)
		}
	}
	// Give any spurious goroutine a chance to run.
	time.Sleep(50 * time.Millisecond)
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Errorf("freshness fired on blob hit: %+v", calls)
	}
}

// TestServeHTTP_FreshnessNotTriggeredOnMiss asserts that the miss path
// does not fire the T1 trigger — the spec says "cached metadata file
// is requested", which presupposes the file is already cached.
func TestServeHTTP_FreshnessNotTriggeredOnMiss(t *testing.T) {
	body := []byte("InRelease bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	rec := newRecordingFreshness()
	h := newTestHandlerWithFreshness(t, rec)
	defer h.Close()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, proxyReq("GET", srv.URL, "/ubuntu/dists/noble/InRelease"))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	time.Sleep(50 * time.Millisecond)
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Errorf("freshness fired on miss: %+v", calls)
	}
}

// TestHandler_Close_DrainsFreshnessGoroutine verifies that
// Handler.Close waits for an in-flight freshness Check call. Without
// the activeWG.Add in maybeFireFreshness, Close could return while a
// goroutine is still using the cache — leading to a write to a closed
// DB at process shutdown.
func TestHandler_Close_DrainsFreshnessGoroutine(t *testing.T) {
	body := []byte("InRelease body")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	gate := make(chan struct{})
	// released is buffered so the goroutine's signal does not race with
	// the test's receive — without the buffer, a context switch between
	// the send (with default fallthrough) and the receive could lose
	// the event entirely.
	released := make(chan struct{}, 1)
	bf := blockingFreshness{gate: gate, released: released}

	h := newTestHandlerWithFreshness(t, bf)

	// Cache the InRelease so the next request hits.
	w1 := httptest.NewRecorder()
	h.ServeHTTP(w1, proxyReq("GET", srv.URL, "/ubuntu/dists/noble/InRelease"))
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, proxyReq("GET", srv.URL, "/ubuntu/dists/noble/InRelease"))

	// At this point a freshness goroutine is blocked on bf.gate.
	closeDone := make(chan struct{})
	go func() {
		h.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
		t.Fatalf("Close returned while freshness goroutine still blocked")
	case <-time.After(150 * time.Millisecond):
	}
	close(gate)
	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatalf("freshness goroutine never observed release")
	}
	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("Close did not return after freshness goroutine released")
	}
}

type blockingFreshness struct {
	gate     chan struct{}
	released chan struct{}
}

// Check intentionally ignores ctx so the test can prove that
// Handler.Close drains the goroutine via activeWG.Wait — not by
// piggybacking on lifecycleCancel waking a well-behaved Checker. If
// the activeWG.Add in maybeFireFreshness were missing, Close would
// return before close(gate) and the test would fail.
func (b blockingFreshness) Check(_ context.Context, _, _, _ string) {
	<-b.gate
	select {
	case b.released <- struct{}{}:
	default:
	}
}

// newTestHandlerWithServe wires the same dependencies as newTestHandler
// but with a custom config.ServeConfig and an optional custom logger.
// Pass logger=nil to keep silent test output.
func newTestHandlerWithServe(t *testing.T, serve config.ServeConfig, logger *slog.Logger) *Handler {
	t.Helper()
	parser, err := proxy.New(nil, nil)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	c, err := cache.Open(context.Background(), t.TempDir(), silentLogger())
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	fc, err := fetch.New(fetch.Options{
		ConnectTimeout:   2 * time.Second,
		TotalTimeout:     5 * time.Second,
		MaxRetries:       2,
		AllowedHostRegex: []string{`^127\.0\.0\.1$`},
		DenyTargetRanges: nil,
		Logger:           silentLogger(),
	})
	if err != nil {
		t.Fatalf("fetch.New: %v", err)
	}
	if logger == nil {
		logger = silentLogger()
	}
	h, err := New(Config{
		Parser:      parser,
		Cache:       c,
		Fetch:       fc,
		HostLimiter: hostsem.New(4),
		Logger:      logger,
		Serve:       serve,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

// TestServeHTTP_UpstreamDown_MetadataRetryAfter30 covers SPEC §6.4: when
// the cache cannot reach upstream and there is no cached metadata to
// serve stale, the 502 carries Retry-After: 30. apt's metadata fetches
// retry on a much shorter cadence than blob fetches, so the differentiated
// Retry-After matters — a uniform 60s would either delay metadata recovery
// or hammer the cache during blob recovery.
func TestServeHTTP_UpstreamDown_MetadataRetryAfter30(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "kaboom", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, nil) // ServeStaleWhenUpstreamDown=false (zero value)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/ubuntu/dists/noble/InRelease"))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After=%q, want %q (metadata)", got, "30")
	}
}

// TestServeHTTP_UpstreamDown_BlobRetryAfter60 covers the .deb half of
// the SPEC §6.4 differentiation. .debs are immutable bytes and a longer
// retry cool-off gives apt's per-package backoff room to spread load
// when upstream comes back.
func TestServeHTTP_UpstreamDown_BlobRetryAfter60(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "kaboom", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/ubuntu/pool/main/p/pkg/file_1.0_amd64.deb"))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After=%q, want %q (blob)", got, "60")
	}
}

// TestServeHTTP_UpstreamUnavailable_MetadataRetryAfter30 verifies the
// Retry-After differentiation also fires for ErrUpstreamUnavailable
// (transport/connect failures), not just 5xx.
func TestServeHTTP_UpstreamUnavailable_MetadataRetryAfter30(t *testing.T) {
	// Bind a local listener and immediately close it so the subsequent
	// connect fails with "connection refused" — drives ErrUpstreamUnavailable.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL
	srv.Close()

	h := newTestHandler(t, nil, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", url, "/ubuntu/dists/noble/InRelease"))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After=%q, want %q (metadata + transport failure)", got, "30")
	}
}

// stalePrime drives a successful first ServeHTTP that populates url_path
// + a real blob on disk for path. Returns the body bytes the upstream
// served so the caller can compare them against tryServeStale's output.
func stalePrime(t *testing.T, h *Handler, srvURL, path string) []byte {
	t.Helper()
	body := []byte("staled-bytes-" + path)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srvURL, path)) // server already started by caller
	if rec.Code != http.StatusOK {
		t.Fatalf("prime ServeHTTP status=%d body=%q", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != string(body) {
		// Server returned different bytes — caller mis-wired the upstream.
		t.Fatalf("prime body mismatch: got=%q want=%q", rec.Body.String(), body)
	}
	return body
}

// staleUpstream returns a server whose body for any GET is the same
// canonical bytes stalePrime expects. Used to seed cached entries with
// known-good blobs.
func staleUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := []byte("staled-bytes-" + r.URL.Path)
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestTryServeStale_ServesCachedRowAndBlob is the positive path for
// HIT-STALE: a row + blob exist in the cache and policy permits stale
// serves, so tryServeStale writes the cached bytes with X-Cache: HIT-STALE.
//
// Driven by a direct method call rather than ServeHTTP because the
// successful HIT path catches the request first and serves "HIT" — the
// only way to reach the stale path through ServeHTTP would be a race
// where the row materializes between tryCacheHit and respondError. Direct
// invocation isolates the SPEC §6.4 behavior under test.
func TestTryServeStale_ServesCachedRowAndBlob(t *testing.T) {
	srv := staleUpstream(t)

	h := newTestHandlerWithServe(t, config.ServeConfig{ServeStaleWhenUpstreamDown: true}, nil)
	defer h.Close()

	body := stalePrime(t, h, srv.URL, "/ubuntu/dists/noble/InRelease")

	// Build the proxy.Request the handler internals operate on.
	req, err := h.parser.Parse(srv.URL+"/ubuntu/dists/noble/InRelease", "127.0.0.1")
	if err != nil {
		t.Fatalf("parser.Parse: %v", err)
	}
	if !req.IsMetadata {
		t.Fatalf("test premise: InRelease must be metadata")
	}

	rec := httptest.NewRecorder()
	r := proxyReq("GET", srv.URL, "/ubuntu/dists/noble/InRelease")
	served := h.tryServeStale(rec, r, req, 0, time.Now())
	if !served {
		t.Fatalf("tryServeStale=false, want true (row+blob present)")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d, want 200", rec.Code)
	}
	if got := rec.Header().Get("X-Cache"); got != "HIT-STALE" {
		t.Errorf("X-Cache=%q, want HIT-STALE", got)
	}
	if rec.Body.String() != string(body) {
		t.Errorf("body=%q, want %q", rec.Body.String(), body)
	}
	if rec.Header().Get("X-Cache-Age") == "" {
		t.Errorf("X-Cache-Age must be set on HIT-STALE")
	}
}

// TestTryServeStale_DisabledByConfig: even if a cached entry exists,
// the stale-serve must not fire when policy is off. Operators can
// disable for environments where serving any potentially-stale bytes
// is unacceptable.
func TestTryServeStale_DisabledByConfig(t *testing.T) {
	srv := staleUpstream(t)

	h := newTestHandlerWithServe(t, config.ServeConfig{ServeStaleWhenUpstreamDown: false}, nil)
	defer h.Close()

	_ = stalePrime(t, h, srv.URL, "/ubuntu/dists/noble/InRelease")

	req, err := h.parser.Parse(srv.URL+"/ubuntu/dists/noble/InRelease", "127.0.0.1")
	if err != nil {
		t.Fatalf("parser.Parse: %v", err)
	}

	rec := httptest.NewRecorder()
	r := proxyReq("GET", srv.URL, "/ubuntu/dists/noble/InRelease")
	if served := h.tryServeStale(rec, r, req, 0, time.Now()); served {
		t.Errorf("tryServeStale=true with policy off, want false")
	}
}

// TestTryServeStale_NotMetadata: a .deb is never stale-eligible. apt
// verifies index hashes against InRelease, so a hash-mismatched .deb
// would reach the client and fail — better to 502 and let apt retry.
func TestTryServeStale_NotMetadata(t *testing.T) {
	srv := staleUpstream(t)

	h := newTestHandlerWithServe(t, config.ServeConfig{ServeStaleWhenUpstreamDown: true}, nil)
	defer h.Close()

	_ = stalePrime(t, h, srv.URL, "/ubuntu/pool/main/p/pkg/file_1.0_amd64.deb")

	req, err := h.parser.Parse(srv.URL+"/ubuntu/pool/main/p/pkg/file_1.0_amd64.deb", "127.0.0.1")
	if err != nil {
		t.Fatalf("parser.Parse: %v", err)
	}
	if req.IsMetadata {
		t.Fatalf("test premise: .deb must not be metadata")
	}

	rec := httptest.NewRecorder()
	r := proxyReq("GET", srv.URL, "/ubuntu/pool/main/p/pkg/file_1.0_amd64.deb")
	if served := h.tryServeStale(rec, r, req, 0, time.Now()); served {
		t.Errorf("tryServeStale=true on .deb, want false (blobs never stale-serve)")
	}
}

// TestTryServeStale_NoRow: empty cache → returns false, never writes to
// the response. The respondError caller falls through to 502.
func TestTryServeStale_NoRow(t *testing.T) {
	h := newTestHandlerWithServe(t, config.ServeConfig{ServeStaleWhenUpstreamDown: true}, nil)
	defer h.Close()

	req, err := h.parser.Parse("http://127.0.0.1/ubuntu/dists/noble/InRelease", "127.0.0.1")
	if err != nil {
		t.Fatalf("parser.Parse: %v", err)
	}

	rec := httptest.NewRecorder()
	r := proxyReq("GET", "http://127.0.0.1", "/ubuntu/dists/noble/InRelease")
	if served := h.tryServeStale(rec, r, req, 0, time.Now()); served {
		t.Errorf("tryServeStale=true with empty cache, want false")
	}
}

// TestTryServeStale_BlobMissing: row points at a blob that's no longer
// on disk (orphan row from manual delete or pruning). tryServeStale
// must report false and skip the response write — not 500 with an
// open-file error.
func TestTryServeStale_BlobMissing(t *testing.T) {
	srv := staleUpstream(t)

	h := newTestHandlerWithServe(t, config.ServeConfig{ServeStaleWhenUpstreamDown: true}, nil)
	defer h.Close()

	_ = stalePrime(t, h, srv.URL, "/ubuntu/dists/noble/InRelease")

	// Find the row the prime created, then nuke its blob from disk.
	row, err := h.cache.LookupURL(context.Background(), "http", "127.0.0.1", "/ubuntu/dists/noble/InRelease")
	if err != nil {
		t.Fatalf("LookupURL after prime: %v", err)
	}
	if row.BlobHash == nil {
		t.Fatalf("row missing blob hash after prime")
	}
	if err := os.Remove(h.cache.BlobPath(*row.BlobHash)); err != nil {
		t.Fatalf("remove blob: %v", err)
	}

	req, err := h.parser.Parse(srv.URL+"/ubuntu/dists/noble/InRelease", "127.0.0.1")
	if err != nil {
		t.Fatalf("parser.Parse: %v", err)
	}

	rec := httptest.NewRecorder()
	r := proxyReq("GET", srv.URL, "/ubuntu/dists/noble/InRelease")
	if served := h.tryServeStale(rec, r, req, 0, time.Now()); served {
		t.Errorf("tryServeStale=true with row but missing blob, want false")
	}
}

// TestTryServeStale_LogsWhenEnabled: serve.log_stale_serves controls
// whether each stale serve emits an info-level log line. SPEC §8 calls
// this out explicitly so operators can correlate cache behavior with
// upstream outages.
func TestTryServeStale_LogsWhenEnabled(t *testing.T) {
	srv := staleUpstream(t)

	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&safeWriter{w: &buf}, &slog.HandlerOptions{Level: slog.LevelInfo}))

	h := newTestHandlerWithServe(t, config.ServeConfig{
		ServeStaleWhenUpstreamDown: true,
		LogStaleServes:             true,
	}, logger)
	defer h.Close()

	_ = stalePrime(t, h, srv.URL, "/ubuntu/dists/noble/InRelease")

	req, err := h.parser.Parse(srv.URL+"/ubuntu/dists/noble/InRelease", "127.0.0.1")
	if err != nil {
		t.Fatalf("parser.Parse: %v", err)
	}

	rec := httptest.NewRecorder()
	r := proxyReq("GET", srv.URL, "/ubuntu/dists/noble/InRelease")
	if !h.tryServeStale(rec, r, req, 0, time.Now()) {
		t.Fatalf("tryServeStale=false, want true (positive path)")
	}

	out := buf.String()
	if !strings.Contains(out, "stale_serve") {
		t.Errorf("logger output missing stale_serve line:\n%s", out)
	}
	if !strings.Contains(out, "/ubuntu/dists/noble/InRelease") {
		t.Errorf("logger output missing path:\n%s", out)
	}
}

// TestTryServeStale_LogsSuppressedByConfig: with LogStaleServes=false
// (and ServeStaleWhenUpstreamDown=true), the stale serve still happens
// but no "stale_serve" line is emitted. The per-request line still
// fires, but with outcome=hit_stale.
func TestTryServeStale_LogsSuppressedByConfig(t *testing.T) {
	srv := staleUpstream(t)

	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&safeWriter{w: &buf}, &slog.HandlerOptions{Level: slog.LevelInfo}))

	h := newTestHandlerWithServe(t, config.ServeConfig{
		ServeStaleWhenUpstreamDown: true,
		LogStaleServes:             false,
	}, logger)
	defer h.Close()

	_ = stalePrime(t, h, srv.URL, "/ubuntu/dists/noble/InRelease")

	// Reset buf so the prime's miss-path log doesn't pollute the assert.
	buf.Reset()

	req, err := h.parser.Parse(srv.URL+"/ubuntu/dists/noble/InRelease", "127.0.0.1")
	if err != nil {
		t.Fatalf("parser.Parse: %v", err)
	}
	rec := httptest.NewRecorder()
	r := proxyReq("GET", srv.URL, "/ubuntu/dists/noble/InRelease")
	if !h.tryServeStale(rec, r, req, 0, time.Now()) {
		t.Fatalf("tryServeStale=false, want true")
	}

	out := buf.String()
	if strings.Contains(out, "stale_serve") {
		t.Errorf("LogStaleServes=false but \"stale_serve\" emitted:\n%s", out)
	}
	if !strings.Contains(out, "hit_stale") {
		t.Errorf("per-request outcome line missing \"hit_stale\":\n%s", out)
	}
}

// safeWriter serializes writes to a strings.Builder. slog.NewTextHandler
// can write from multiple goroutines (handler.logRequest is called in
// a fresh goroutine for touchAsync), and a bare strings.Builder isn't
// safe under that.
type safeWriter struct {
	mu sync.Mutex
	w  *strings.Builder
}

func (s *safeWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// TestServeHTTP_OrphanRowUpstreamDownReturns502 exercises the Phase 1
// public path most likely to hit respondUpstreamUnreachable in
// production: a row in url_path whose blob is no longer on disk
// (manual delete, prune script, fsck). tryCacheHit's BlobExists check
// fires false → fall-through to miss path → upstream returns an error
// → tryServeStale runs but BlobExists is *still* false → falls through
// to 502 with the SPEC §6.4 metadata Retry-After.
//
// This is a negative-path public test: the response is 502, not
// HIT-STALE. The positive HIT-STALE response through ServeHTTP requires
// a row+blob to materialize between tryCacheHit and tryServeStale (a
// benign concurrency race), which is what the direct-call
// TestTryServeStale_* unit tests cover. Phase 2 will introduce a
// "frozen consistent set" path where tryCacheHit explicitly defers to
// the stale machinery, at which point the positive ServeHTTP path
// becomes constructible end-to-end.
func TestServeHTTP_OrphanRowUpstreamDownReturns502(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// First request: 200 with body so the cache primes. Subsequent
		// requests: 503 to drive the miss-path through respondError.
		if hits.Add(1) == 1 {
			body := []byte("staled-bytes-" + r.URL.Path)
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			_, _ = w.Write(body)
			return
		}
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	h := newTestHandlerWithServe(t, config.ServeConfig{
		ServeStaleWhenUpstreamDown: true,
		LogStaleServes:             false,
	}, nil)
	defer h.Close()

	// Prime: row + blob in cache.
	primer := httptest.NewRecorder()
	h.ServeHTTP(primer, proxyReq("GET", srv.URL, "/ubuntu/dists/noble/InRelease"))
	if primer.Code != http.StatusOK {
		t.Fatalf("prime status=%d body=%q", primer.Code, primer.Body.String())
	}

	// Orphan the row by removing the blob from disk.
	row, err := h.cache.LookupURL(context.Background(), "http", "127.0.0.1", "/ubuntu/dists/noble/InRelease")
	if err != nil {
		t.Fatalf("LookupURL: %v", err)
	}
	if row.BlobHash == nil {
		t.Fatalf("primed row has no blob hash")
	}
	if err := os.Remove(h.cache.BlobPath(*row.BlobHash)); err != nil {
		t.Fatalf("remove blob to create orphan: %v", err)
	}

	// Request the same URL again. Expected path:
	//   tryCacheHit: row found, BlobExists=false → fall through.
	//   serveCacheMiss → fetch → 503 → ErrUpstreamServerError.
	//   respondError → respondUpstreamUnreachable → tryServeStale:
	//     row found, BlobExists=*still* false → returns false.
	//   Final: 502 + Retry-After: 30 (metadata).
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/ubuntu/dists/noble/InRelease"))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After=%q, want %q (orphan metadata 502)", got, "30")
	}
	if got := rec.Header().Get("X-Cache"); got == "HIT-STALE" {
		t.Errorf("X-Cache=HIT-STALE on orphan with no blob — must not serve")
	}
	// Upstream hits: 1 (prime) + N (fetch with retries on 503). N is
	// MaxRetries-dependent, so assert the qualitative invariant: prime
	// happened AND the second request actually reached upstream.
	if got := hits.Load(); got < 2 {
		t.Errorf("upstream hits=%d, want >=2 (prime + at least one retry attempt)", got)
	}
}

// newTestHandlerWithFetchOpts builds a handler around a fetch client whose
// timeouts and retry budget the caller supplies. Used by the SPEC §11 row 4
// timeout tests, which need a TotalTimeout shorter than the default 5s so
// the test does not pay 5s of real wall time per case.
func newTestHandlerWithFetchOpts(t *testing.T, fopts fetch.Options) *Handler {
	t.Helper()
	parser, err := proxy.New(nil, nil)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	c, err := cache.Open(context.Background(), t.TempDir(), silentLogger())
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if fopts.AllowedHostRegex == nil {
		fopts.AllowedHostRegex = []string{`^127\.0\.0\.1$`}
	}
	fc, err := fetch.New(fopts)
	if err != nil {
		t.Fatalf("fetch.New: %v", err)
	}
	h, err := New(Config{
		Parser:      parser,
		Cache:       c,
		Fetch:       fc,
		HostLimiter: hostsem.New(4),
		Logger:      silentLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

// TestServeHTTP_UpstreamTimeout_MetadataRetryAfter30 covers SPEC §11 row 4
// for metadata: a fetch that hits ctx.DeadlineExceeded (vs. 5xx, vs.
// connect-refused) maps to 502 + Retry-After: 30. The existing UpstreamDown
// + UpstreamUnavailable tests cover 5xx and transport failures respectively;
// this one closes the gap on the canonical "upstream is up but never
// responds" failure mode that motivated the §6.4 differentiation.
func TestServeHTTP_UpstreamTimeout_MetadataRetryAfter30(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block until the request context is cancelled (which happens
		// when fetch's TotalTimeout expires). Sleeping a fixed long
		// duration would leak goroutines on test failure; binding to
		// the request ctx makes the upstream stop the moment fetch
		// gives up.
		<-r.Context().Done()
	}))
	defer srv.Close()

	h := newTestHandlerWithFetchOpts(t, fetch.Options{
		ConnectTimeout: 2 * time.Second,
		TotalTimeout:   150 * time.Millisecond,
		MaxRetries:     0,
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/ubuntu/dists/noble/InRelease"))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After=%q, want %q (metadata + true timeout)", got, "30")
	}
}

// TestServeHTTP_UpstreamTimeout_BlobRetryAfter60 covers the .deb half of
// the SPEC §6.4 differentiation when the failure is a true timeout.
func TestServeHTTP_UpstreamTimeout_BlobRetryAfter60(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	h := newTestHandlerWithFetchOpts(t, fetch.Options{
		ConnectTimeout: 2 * time.Second,
		TotalTimeout:   150 * time.Millisecond,
		MaxRetries:     0,
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/ubuntu/pool/main/p/pkg/file_1.0_amd64.deb"))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After=%q, want %q (blob + true timeout)", got, "60")
	}
}

// TestRespondError_CacheWriteFailedLogsAndReturns502 covers SPEC §11 row 14
// at the handler boundary: when fetch surfaces ErrCacheWriteFailed, the
// handler emits 502 + Retry-After AND a slog.Error log line so the
// operator-visible signal distinguishes a cache-side fault from an
// upstream outage. Driven by a synthetic sfResult rather than ServeHTTP
// because runFetch's NewTempBlob is concrete; routing a real disk-full
// through the public path would require fault-injection plumbing that
// belongs to a future test-helper slice. The unit test pins the response
// shape and the slog.Error so the end-to-end behavior is unambiguous.
func TestRespondError_CacheWriteFailedLogsAndReturns502(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&safeWriter{w: &buf}, &slog.HandlerOptions{Level: slog.LevelInfo}))

	h := newTestHandlerWithServe(t, config.ServeConfig{}, logger)
	defer h.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()

	req, err := h.parser.Parse(srv.URL+"/ubuntu/dists/noble/InRelease", "127.0.0.1")
	if err != nil {
		t.Fatalf("parser.Parse: %v", err)
	}

	rec := httptest.NewRecorder()
	r := proxyReq("GET", srv.URL, "/ubuntu/dists/noble/InRelease")
	res := sfResult{err: fmt.Errorf("test wrap: %w", fetch.ErrCacheWriteFailed)}

	h.respondError(rec, r, req, res, time.Now())

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After=%q, want %q (metadata + cache write failure)", got, "30")
	}
	out := buf.String()
	if !strings.Contains(out, "level=ERROR") {
		t.Errorf("slog.Error not emitted for cache write failure:\n%s", out)
	}
	if !strings.Contains(out, "cache write failed") {
		t.Errorf("expected 'cache write failed' in log:\n%s", out)
	}
	if !strings.Contains(out, "outcome=cache_write_failed") {
		t.Errorf("per-request log outcome must be cache_write_failed:\n%s", out)
	}
}

// TestLogRequest_UpstreamStatusFieldShape pins the SPEC §10 contract that
// upstream_status is logged when (and only when) a fetch was attempted.
// The field's presence is the operator's filter for "did this request go
// to upstream"; emitting upstream_status=0 on hits would defeat that.
func TestLogRequest_UpstreamStatusFieldShape(t *testing.T) {
	t.Run("hit_omits_upstream_status", func(t *testing.T) {
		// Drive a true cache HIT: prime once, then expect the second
		// request to log without upstream_status (no fetch occurred).
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("primed"))
		}))
		defer srv.Close()

		var buf strings.Builder
		logger := slog.New(slog.NewTextHandler(&safeWriter{w: &buf}, &slog.HandlerOptions{Level: slog.LevelInfo}))
		h := newTestHandlerWithServe(t, config.ServeConfig{}, logger)
		defer h.Close()

		// Prime.
		rec1 := httptest.NewRecorder()
		h.ServeHTTP(rec1, proxyReq("GET", srv.URL, "/ubuntu/pool/p/pkg/x.deb"))
		if rec1.Code != 200 {
			t.Fatalf("prime status=%d, want 200", rec1.Code)
		}
		// Cache hit.
		buf.Reset()
		rec2 := httptest.NewRecorder()
		h.ServeHTTP(rec2, proxyReq("GET", srv.URL, "/ubuntu/pool/p/pkg/x.deb"))
		if rec2.Code != 200 {
			t.Fatalf("hit status=%d, want 200", rec2.Code)
		}
		out := buf.String()
		if !strings.Contains(out, "outcome=hit") {
			t.Fatalf("second request was not a hit:\n%s", out)
		}
		if strings.Contains(out, "upstream_status=") {
			t.Errorf("hit log must NOT carry upstream_status:\n%s", out)
		}
	})

	t.Run("miss_emits_upstream_status_200", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("hello"))
		}))
		defer srv.Close()

		var buf strings.Builder
		logger := slog.New(slog.NewTextHandler(&safeWriter{w: &buf}, &slog.HandlerOptions{Level: slog.LevelInfo}))
		h := newTestHandlerWithServe(t, config.ServeConfig{}, logger)
		defer h.Close()

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/ubuntu/pool/p/pkg/y.deb"))
		if rec.Code != 200 {
			t.Fatalf("status=%d, want 200", rec.Code)
		}
		out := buf.String()
		if !strings.Contains(out, "outcome=miss") {
			t.Fatalf("expected outcome=miss:\n%s", out)
		}
		if !strings.Contains(out, "upstream_status=200") {
			t.Errorf("miss log must carry upstream_status=200:\n%s", out)
		}
	})

	t.Run("method_not_allowed_omits_upstream_status", func(t *testing.T) {
		var buf strings.Builder
		logger := slog.New(slog.NewTextHandler(&safeWriter{w: &buf}, &slog.HandlerOptions{Level: slog.LevelInfo}))
		h := newTestHandlerWithServe(t, config.ServeConfig{}, logger)
		defer h.Close()

		rec := httptest.NewRecorder()
		r := proxyReq("POST", "http://127.0.0.1:80", "/ubuntu/anything")
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status=%d, want 405", rec.Code)
		}
		out := buf.String()
		if !strings.Contains(out, "outcome=method_not_allowed") {
			t.Errorf("expected outcome=method_not_allowed:\n%s", out)
		}
		if strings.Contains(out, "upstream_status=") {
			t.Errorf("pre-fetch rejection must NOT carry upstream_status:\n%s", out)
		}
	})
}

// TestSingleflight_LeaderLogsCoalescing pins SPEC §10's "singleflight
// coalescing" structured log: when waiters joined the leader's call, the
// leader emits a `singleflight coalesced` Info line with the waiter
// count.
func TestSingleflight_LeaderLogsCoalescing(t *testing.T) {
	g := newSFGroup()

	gate := make(chan struct{})
	leaderDone := make(chan sfResult, 1)

	go func() {
		res, _, _ := g.Do("k", func() sfResult {
			<-gate
			return sfResult{blobHash: "h"}
		})
		leaderDone <- res
	}()

	// Three waiters join while the leader holds the gate.
	const numWaiters = 3
	waiterDone := make(chan struct{}, numWaiters)
	// Wait briefly for leader to register.
	time.Sleep(10 * time.Millisecond)
	for i := 0; i < numWaiters; i++ {
		go func() {
			_, _, _ = g.Do("k", func() sfResult { return sfResult{} })
			waiterDone <- struct{}{}
		}()
	}
	// Give the waiters a moment to register on the leader's call.
	time.Sleep(50 * time.Millisecond)
	close(gate)
	<-leaderDone
	for i := 0; i < numWaiters; i++ {
		<-waiterDone
	}

	// Re-run via the handler.serveCacheMiss path to verify the log line
	// fires at the correct call site. We do this by replaying through a
	// real handler with a captured logger.
	//
	// AIDEV-NOTE: this proves the count is plumbed; the assertion below
	// reads the captured logger output, not the sfGroup directly.
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&safeWriter{w: &buf}, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h := newTestHandlerWithServe(t, config.ServeConfig{}, logger)
	defer h.Close()

	upstreamGate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-upstreamGate
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("body"))
	}))
	defer srv.Close()

	const N = 4
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/ubuntu/pool/p/pkg/sf.deb"))
		}()
	}
	// Give the requests time to enter the singleflight together. They
	// should all coalesce because the upstream gate is closed.
	time.Sleep(80 * time.Millisecond)
	close(upstreamGate)
	wg.Wait()

	out := buf.String()
	if !strings.Contains(out, "msg=\"singleflight coalesced\"") {
		t.Errorf("expected 'singleflight coalesced' log:\n%s", out)
	}
	// At least one waiter should have joined.
	if !strings.Contains(out, "waiters=") {
		t.Errorf("expected waiters= field in coalescing log:\n%s", out)
	}
}

// TestLogRequest_UpstreamStatus5xxExhaustion pins codex finding #2: when
// fetch retries an upstream 5xx until exhaustion, the wrapped
// ErrUpstreamUnavailable must still carry the *StatusError chain so the
// handler's errors.As lookup surfaces the real upstream code into both
// the X-Upstream-Status header and the SPEC §10 upstream_status log
// field. Pre-fix this test fails: status=0 leaks through and the field
// reads "upstream_status=0" instead of "upstream_status=503".
func TestLogRequest_UpstreamStatus5xxExhaustion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "kaboom", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&safeWriter{w: &buf}, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h := newTestHandlerWithServe(t, config.ServeConfig{}, logger)
	defer h.Close()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/ubuntu/pool/p/pkg/x.deb"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502", rec.Code)
	}
	if got := rec.Header().Get("X-Upstream-Status"); got != "503" {
		t.Errorf("X-Upstream-Status=%q, want %q (5xx must propagate through retry exhaustion)", got, "503")
	}
	out := buf.String()
	if !strings.Contains(out, "outcome=bad_gateway") {
		t.Fatalf("expected outcome=bad_gateway:\n%s", out)
	}
	if !strings.Contains(out, "upstream_status=503") {
		t.Errorf("upstream_status=503 must appear in request log (codex finding):\n%s", out)
	}
}

// TestLogRequest_UpstreamStatusZeroOnTimeout pins the SPEC §10 contract
// that fetch-attempted-but-no-response emits upstream_status=0 (rather
// than omitting the field). The presence of the field — not its value —
// is the operator's signal for "did this request reach upstream".
func TestLogRequest_UpstreamStatusZeroOnTimeout(t *testing.T) {
	// Bind a listener and immediately close it so the next connect
	// fails with "connection refused" — drives ErrUpstreamUnavailable
	// with no HTTP response received.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL
	srv.Close()

	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&safeWriter{w: &buf}, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h := newTestHandlerWithServe(t, config.ServeConfig{}, logger)
	defer h.Close()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", url, "/ubuntu/pool/p/pkg/y.deb"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502", rec.Code)
	}
	out := buf.String()
	if !strings.Contains(out, "outcome=bad_gateway") {
		t.Fatalf("expected outcome=bad_gateway:\n%s", out)
	}
	if !strings.Contains(out, "upstream_status=0") {
		t.Errorf("upstream_status=0 must be emitted when fetch attempted but no response arrived:\n%s", out)
	}
}
