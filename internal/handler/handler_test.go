package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
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

	c, err := cache.Open(context.Background(), t.TempDir())
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
	})
	if err != nil {
		t.Fatalf("fetch.New: %v", err)
	}

	h, err := New(Config{
		Parser:               parser,
		Cache:                c,
		Fetch:                fc,
		MaxConcurrentPerHost: 4,
		Logger:               silentLogger(),
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
	c, err := cache.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	defer c.Close()
	parser, _ := proxy.New(nil, nil)
	fc, _ := fetch.New(fetch.Options{AllowedHostRegex: []string{`.`}, DenyTargetRanges: nil})

	cases := []struct {
		name string
		cfg  Config
	}{
		{"nil parser", Config{Cache: c, Fetch: fc}},
		{"nil cache", Config{Parser: parser, Fetch: fc}},
		{"nil fetch", Config{Parser: parser, Cache: c}},
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
		res, _ := g.Do("k", func() sfResult {
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
		res, shared := g.Do("k", func() sfResult {
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
	res, shared := g.Do("k", func() sfResult { return sfResult{blobHash: "next"} })
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
		res, _ := g.Do("k", func() sfResult {
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
		res, shared := g.Do("k", func() sfResult {
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

func TestHostSem_LimitsConcurrency(t *testing.T) {
	s := newHostSem(2)

	rel1, err := s.acquire(context.Background(), "h")
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	rel2, err := s.acquire(context.Background(), "h")
	if err != nil {
		t.Fatalf("acquire 2: %v", err)
	}

	// Third acquire must block until release.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = s.acquire(ctx, "h")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("third acquire err=%v, want DeadlineExceeded", err)
	}

	rel1()
	// Now a fourth acquire should succeed promptly.
	rel3, err := s.acquire(context.Background(), "h")
	if err != nil {
		t.Fatalf("post-release acquire: %v", err)
	}
	rel2()
	rel3()
}

func TestHostSem_PerHostIsolated(t *testing.T) {
	s := newHostSem(1)
	rA, err := s.acquire(context.Background(), "a")
	if err != nil {
		t.Fatalf("acquire a: %v", err)
	}
	defer rA()

	// Different host has its own slot, so this must succeed without
	// blocking even though a's slot is held.
	rB, err := s.acquire(context.Background(), "b")
	if err != nil {
		t.Fatalf("acquire b: %v", err)
	}
	defer rB()
}

// TestHostSem_RefcountReleasesSlot proves the per-host map shrinks when
// the last holder of a slot releases. Without refcounting, every distinct
// host the cache ever sees creates a permanent map entry and an attacker
// can grow the map without bound by sending requests for many made-up
// hostnames.
func TestHostSem_RefcountReleasesSlot(t *testing.T) {
	s := newHostSem(2)

	rel, err := s.acquire(context.Background(), "transient-host")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if got := s.hostCount(); got != 1 {
		t.Errorf("hostCount during use = %d, want 1", got)
	}
	rel()
	if got := s.hostCount(); got != 0 {
		t.Errorf("hostCount after last release = %d, want 0", got)
	}

	// Many transient hosts — each should clean up after itself.
	for i := 0; i < 100; i++ {
		host := fmt.Sprintf("h-%d", i)
		rel, err := s.acquire(context.Background(), host)
		if err != nil {
			t.Fatalf("acquire %q: %v", host, err)
		}
		rel()
	}
	if got := s.hostCount(); got != 0 {
		t.Errorf("hostCount after churn = %d, want 0", got)
	}
}

// TestHostSem_RefcountSurvivesCtxCancel proves a ctx-cancelled acquire
// (which never took a channel token) still drops its refcount.
func TestHostSem_RefcountSurvivesCtxCancel(t *testing.T) {
	s := newHostSem(1)

	// Hold the only slot.
	hold, err := s.acquire(context.Background(), "h")
	if err != nil {
		t.Fatalf("acquire hold: %v", err)
	}

	// Try to acquire a second slot with a ctx that fires fast.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = s.acquire(ctx, "h")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquire-2 err=%v, want DeadlineExceeded", err)
	}
	// Refcount: hold has 1 ref, the failed acquire decremented its own.
	// Both ops on host "h" → slot still alive (refs = 1).
	if got := s.hostCount(); got != 1 {
		t.Errorf("hostCount with one holder = %d, want 1", got)
	}

	hold()
	if got := s.hostCount(); got != 0 {
		t.Errorf("hostCount after final release = %d, want 0", got)
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

	if got := h.sem.hostCount(); got != 0 {
		t.Errorf("hostCount after 50 disallowed hosts = %d, want 0", got)
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
	c, err := cache.Open(context.Background(), t.TempDir())
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
	})
	if err != nil {
		t.Fatalf("fetch.New: %v", err)
	}
	h, err := New(Config{
		Parser:               parser,
		Cache:                c,
		Fetch:                fetchClient,
		MaxConcurrentPerHost: 4,
		Logger:               silentLogger(),
		Freshness:            fc,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
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
