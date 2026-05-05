package freshness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
)

// fakeCache is the in-memory Cache used by these tests. The real cache
// involves SQLite, on-disk pool/, and a single-writer goroutine — none
// of which the freshness checker actually exercises beyond the four
// methods on the Cache interface.
type fakeCache struct {
	mu        sync.Mutex
	suites    map[string]cache.SuiteFreshness // key: scheme|host|path
	urls      map[string]cache.URLPath        // key: scheme|host|path
	listErr   error
	getErr    error
	putErr    error
	lookupErr error

	putCount atomic.Int64
}

func newFakeCache() *fakeCache {
	return &fakeCache{
		suites: make(map[string]cache.SuiteFreshness),
		urls:   make(map[string]cache.URLPath),
	}
}

func keyOf(scheme, host, path string) string {
	return scheme + "|" + host + "|" + path
}

func (f *fakeCache) GetSuiteFreshness(ctx context.Context, scheme, host, suitePath string) (*cache.SuiteFreshness, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	s, ok := f.suites[keyOf(scheme, host, suitePath)]
	if !ok {
		return nil, cache.ErrNotFound
	}
	cp := s
	return &cp, nil
}

func (f *fakeCache) PutSuiteFreshness(ctx context.Context, s cache.SuiteFreshness) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putCount.Add(1)
	if f.putErr != nil {
		return f.putErr
	}
	f.suites[keyOf(s.CanonicalScheme, s.CanonicalHost, s.SuitePath)] = s
	return nil
}

func (f *fakeCache) ListSuites(ctx context.Context) ([]cache.SuiteFreshness, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]cache.SuiteFreshness, 0, len(f.suites))
	for _, s := range f.suites {
		out = append(out, s)
	}
	return out, nil
}

func (f *fakeCache) LookupURL(ctx context.Context, scheme, host, path string) (*cache.URLPath, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}
	u, ok := f.urls[keyOf(scheme, host, path)]
	if !ok {
		return nil, cache.ErrNotFound
	}
	cp := u
	return &cp, nil
}

func (f *fakeCache) putURL(u cache.URLPath) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.urls[keyOf(u.CanonicalScheme, u.CanonicalHost, u.Path)] = u
}

func (f *fakeCache) putSuite(s cache.SuiteFreshness) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.suites[keyOf(s.CanonicalScheme, s.CanonicalHost, s.SuitePath)] = s
}

func (f *fakeCache) suite(scheme, host, suitePath string) (cache.SuiteFreshness, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.suites[keyOf(scheme, host, suitePath)]
	return s, ok
}

// hashOf returns the sha256 hex of body.
func hashOf(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// newTestFetcher builds a real *fetch.Client wrapping a permissive
// allowlist + httptest's loopback. We test through the actual fetch
// package because Conditional is the contract we want exercised.
func newTestFetcher(t *testing.T) *fetch.Client {
	t.Helper()
	c, err := fetch.New(fetch.Options{
		ConnectTimeout:   2 * time.Second,
		TotalTimeout:     5 * time.Second,
		MaxRetries:       0,
		AllowedHostRegex: []string{`^127\.0\.0\.1$`},
		DenyTargetRanges: nil,
	})
	if err != nil {
		t.Fatalf("fetch.New: %v", err)
	}
	return c
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestCheck_NoCachedInRelease_Skips(t *testing.T) {
	fc := newFakeCache()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream should not be contacted; got %s", r.URL.Path)
	}))
	defer srv.Close()

	c, err := New(Config{
		Cache:    fc,
		Fetcher:  newTestFetcher(t),
		Cooldown: 0,
		Logger:   discardLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.Check(context.Background(), "http", "127.0.0.1", "/dists/noble")
	if fc.putCount.Load() != 0 {
		t.Errorf("PutSuiteFreshness was called; want skip without write")
	}
}

func TestCheck_NotModified_BumpsTimestamps(t *testing.T) {
	fc := newFakeCache()
	hash := hashOf([]byte("the cached InRelease bytes"))
	bh := hash
	fc.putURL(cache.URLPath{
		CanonicalScheme: "http",
		CanonicalHost:   "127.0.0.1",
		Path:            "/dists/noble/InRelease",
		BlobHash:        &bh,
		IsMetadata:      true,
	})
	etag := `"v1"`
	prevCheck := int64(1000)
	fc.putSuite(cache.SuiteFreshness{
		CanonicalScheme: "http",
		CanonicalHost:   "127.0.0.1",
		SuitePath:       "/dists/noble",
		LastCheckAt:     &prevCheck,
		InReleaseETag:   &etag,
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != etag {
			t.Errorf("If-None-Match = %q, want %s", r.Header.Get("If-None-Match"), etag)
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	fc.putURL(cache.URLPath{
		CanonicalScheme: "http",
		CanonicalHost:   "127.0.0.1",
		Path:            "/dists/noble/InRelease",
		BlobHash:        &bh,
		UpstreamURL:     srv.URL + "/dists/noble/InRelease",
		IsMetadata:      true,
	})

	now := time.Unix(2000, 0)
	c, err := New(Config{
		Cache:    fc,
		Fetcher:  newTestFetcher(t),
		Cooldown: 60 * time.Second,
		Logger:   discardLogger(),
		now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.Check(context.Background(), "http", "127.0.0.1", "/dists/noble")

	got, ok := fc.suite("http", "127.0.0.1", "/dists/noble")
	if !ok {
		t.Fatalf("suite row missing")
	}
	if got.LastCheckAt == nil || *got.LastCheckAt != 2000 {
		t.Errorf("last_check_at = %v, want 2000", got.LastCheckAt)
	}
	if got.LastSuccessAt == nil || *got.LastSuccessAt != 2000 {
		t.Errorf("last_success_at = %v, want 2000", got.LastSuccessAt)
	}
	if got.InReleaseChangeSeenAt != nil {
		t.Errorf("inrelease_change_seen_at = %v, want nil", got.InReleaseChangeSeenAt)
	}
}

func TestCheck_Cooldown_Skips(t *testing.T) {
	fc := newFakeCache()
	bh := hashOf([]byte("x"))
	fc.putURL(cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1", Path: "/dists/noble/InRelease",
		BlobHash: &bh, UpstreamURL: "http://127.0.0.1/never", IsMetadata: true,
	})
	prev := int64(1990)
	fc.putSuite(cache.SuiteFreshness{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1", SuitePath: "/dists/noble",
		LastCheckAt: &prev,
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream should not be contacted in cooldown")
	}))
	defer srv.Close()

	now := time.Unix(2000, 0) // 10s later, well inside 60s cooldown
	c, err := New(Config{
		Cache:    fc,
		Fetcher:  newTestFetcher(t),
		Cooldown: 60 * time.Second,
		Logger:   discardLogger(),
		now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.Check(context.Background(), "http", "127.0.0.1", "/dists/noble")
	if fc.putCount.Load() != 0 {
		t.Errorf("PutSuiteFreshness called inside cooldown")
	}
}

func TestCheck_OK_ContentMatches_RefreshesValidators(t *testing.T) {
	fc := newFakeCache()
	body := []byte("Origin: Ubuntu\nSuite: noble\n")
	bh := hashOf(body)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v2"`)
		w.Header().Set("Last-Modified", "Tue, 02 Jan 2024 00:00:00 GMT")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	fc.putURL(cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1", Path: "/dists/noble/InRelease",
		BlobHash: &bh, UpstreamURL: srv.URL + "/dists/noble/InRelease", IsMetadata: true,
	})
	oldEtag := `"v1"`
	fc.putSuite(cache.SuiteFreshness{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1", SuitePath: "/dists/noble",
		InReleaseETag: &oldEtag,
	})

	now := time.Unix(3000, 0)
	c, _ := New(Config{
		Cache:    fc,
		Fetcher:  newTestFetcher(t),
		Cooldown: 0,
		Logger:   discardLogger(),
		now:      func() time.Time { return now },
	})
	c.Check(context.Background(), "http", "127.0.0.1", "/dists/noble")

	got, _ := fc.suite("http", "127.0.0.1", "/dists/noble")
	if got.InReleaseETag == nil || *got.InReleaseETag != `"v2"` {
		t.Errorf("etag not refreshed: %v", got.InReleaseETag)
	}
	if got.InReleaseChangeSeenAt != nil {
		t.Errorf("change_seen_at set spuriously: %v", got.InReleaseChangeSeenAt)
	}
	if got.LastSuccessAt == nil || *got.LastSuccessAt != 3000 {
		t.Errorf("last_success_at = %v, want 3000", got.LastSuccessAt)
	}
}

func TestCheck_OK_ContentChanged_RecordsObservation(t *testing.T) {
	fc := newFakeCache()
	cachedBody := []byte("OLD InRelease")
	upstreamBody := []byte("NEW InRelease")
	cachedHash := hashOf(cachedBody)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v9"`)
		_, _ = w.Write(upstreamBody)
	}))
	defer srv.Close()

	fc.putURL(cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1", Path: "/dists/noble/InRelease",
		BlobHash: &cachedHash, UpstreamURL: srv.URL + "/dists/noble/InRelease", IsMetadata: true,
	})

	now := time.Unix(4000, 0)
	c, _ := New(Config{
		Cache: fc, Fetcher: newTestFetcher(t),
		Cooldown: 0, Logger: discardLogger(),
		now: func() time.Time { return now },
	})
	c.Check(context.Background(), "http", "127.0.0.1", "/dists/noble")

	got, _ := fc.suite("http", "127.0.0.1", "/dists/noble")
	if got.InReleaseChangeSeenAt == nil || *got.InReleaseChangeSeenAt != 4000 {
		t.Errorf("change_seen_at = %v, want 4000", got.InReleaseChangeSeenAt)
	}
	if got.LastSuccessAt == nil || *got.LastSuccessAt != 4000 {
		t.Errorf("last_success_at = %v, want 4000", got.LastSuccessAt)
	}
	// Phase 1 explicitly does NOT adopt: validators should not move
	// when the body changed (we keep the old ones because the cached
	// blob is still the older content).
	if got.InReleaseETag != nil && *got.InReleaseETag == `"v9"` {
		t.Errorf("validators were adopted; Phase 1 must keep old validators")
	}
}

// TestCheck_ChangeSeenClearedOn304 covers the upstream-recovery path:
// a previous check observed an upstream-ahead InRelease (change_seen_at
// set), the next check returns 304 (upstream is back to matching the
// cached version, e.g. after a rollback). The diagnostic must clear so
// it accurately reflects current state.
func TestCheck_ChangeSeenClearedOn304(t *testing.T) {
	fc := newFakeCache()
	bh := hashOf([]byte("cached body"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	fc.putURL(cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1", Path: "/dists/noble/InRelease",
		BlobHash: &bh, UpstreamURL: srv.URL + "/dists/noble/InRelease", IsMetadata: true,
	})
	stale := int64(1234)
	fc.putSuite(cache.SuiteFreshness{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1", SuitePath: "/dists/noble",
		InReleaseChangeSeenAt: &stale,
	})

	c, _ := New(Config{
		Cache: fc, Fetcher: newTestFetcher(t),
		Logger: discardLogger(),
	})
	c.Check(context.Background(), "http", "127.0.0.1", "/dists/noble")

	got, _ := fc.suite("http", "127.0.0.1", "/dists/noble")
	if got.InReleaseChangeSeenAt != nil {
		t.Errorf("change_seen_at = %v, want cleared", got.InReleaseChangeSeenAt)
	}
}

// TestCheck_ChangeSeenClearedOn200Match covers the same recovery via
// the 200-bytes-match branch: upstream returned 200 (didn't honor
// conditional GET) but the body hashes to the same bytes the cache
// already holds. This too proves recovery from a prior change-seen
// observation, so the diagnostic must clear.
func TestCheck_ChangeSeenClearedOn200Match(t *testing.T) {
	fc := newFakeCache()
	body := []byte("steady state body")
	bh := hashOf(body)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	fc.putURL(cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1", Path: "/dists/noble/InRelease",
		BlobHash: &bh, UpstreamURL: srv.URL + "/dists/noble/InRelease", IsMetadata: true,
	})
	stale := int64(1234)
	fc.putSuite(cache.SuiteFreshness{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1", SuitePath: "/dists/noble",
		InReleaseChangeSeenAt: &stale,
	})

	c, _ := New(Config{
		Cache: fc, Fetcher: newTestFetcher(t),
		Logger: discardLogger(),
	})
	c.Check(context.Background(), "http", "127.0.0.1", "/dists/noble")

	got, _ := fc.suite("http", "127.0.0.1", "/dists/noble")
	if got.InReleaseChangeSeenAt != nil {
		t.Errorf("change_seen_at = %v, want cleared", got.InReleaseChangeSeenAt)
	}
}

func TestCheck_UpstreamError_BumpsCheckOnly(t *testing.T) {
	fc := newFakeCache()
	bh := hashOf([]byte("x"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	fc.putURL(cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1", Path: "/dists/noble/InRelease",
		BlobHash: &bh, UpstreamURL: srv.URL + "/dists/noble/InRelease", IsMetadata: true,
	})
	prevSuccess := int64(100)
	fc.putSuite(cache.SuiteFreshness{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1", SuitePath: "/dists/noble",
		LastSuccessAt: &prevSuccess,
	})

	now := time.Unix(5000, 0)
	c, _ := New(Config{
		Cache: fc, Fetcher: newTestFetcher(t),
		Cooldown: 0, Logger: discardLogger(),
		now: func() time.Time { return now },
	})
	c.Check(context.Background(), "http", "127.0.0.1", "/dists/noble")

	got, _ := fc.suite("http", "127.0.0.1", "/dists/noble")
	if got.LastCheckAt == nil || *got.LastCheckAt != 5000 {
		t.Errorf("last_check_at = %v, want 5000", got.LastCheckAt)
	}
	if got.LastSuccessAt == nil || *got.LastSuccessAt != 100 {
		t.Errorf("last_success_at = %v, want untouched 100", got.LastSuccessAt)
	}
}

func TestCheck_TryLock_SecondCallSkips(t *testing.T) {
	fc := newFakeCache()
	bh := hashOf([]byte("x"))

	// gate blocks the upstream handler so the first Check stays in
	// the conditional GET while the second tries to acquire the lock.
	gate := make(chan struct{})
	hits := atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-gate
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()
	defer close(gate)

	fc.putURL(cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1", Path: "/dists/noble/InRelease",
		BlobHash: &bh, UpstreamURL: srv.URL + "/dists/noble/InRelease", IsMetadata: true,
	})

	c, _ := New(Config{
		Cache: fc, Fetcher: newTestFetcher(t),
		Cooldown: 0, Logger: discardLogger(),
	})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.Check(context.Background(), "http", "127.0.0.1", "/dists/noble")
	}()
	// Wait for first Check to actually be in the upstream call.
	deadline := time.Now().Add(2 * time.Second)
	for hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if hits.Load() == 0 {
		gate <- struct{}{}
		t.Fatalf("first Check never reached upstream")
	}

	// Second Check should TryLock-fail and return immediately.
	done := make(chan struct{})
	go func() {
		c.Check(context.Background(), "http", "127.0.0.1", "/dists/noble")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("second Check blocked instead of TryLock-skipping")
	}

	// Release the first Check.
	gate <- struct{}{}
	wg.Wait()

	if hits.Load() != 1 {
		t.Errorf("upstream hit %d times; want 1 (TryLock should have skipped second)", hits.Load())
	}
}

func TestTick_FilersByLastSuccessAt(t *testing.T) {
	fc := newFakeCache()
	bh := hashOf([]byte("x"))

	// Two suites: "fresh" was just refreshed; "stale" was hours ago.
	fc.putURL(cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1", Path: "/fresh/InRelease",
		BlobHash: &bh, UpstreamURL: "http://127.0.0.1/never-called", IsMetadata: true,
	})
	fc.putURL(cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1", Path: "/stale/InRelease",
		BlobHash: &bh, IsMetadata: true,
	})
	now := time.Unix(10000, 0)
	freshT := now.Unix() - 60      // 1 min old
	staleT := now.Unix() - 60*60*2 // 2 hours old
	fc.putSuite(cache.SuiteFreshness{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1", SuitePath: "/fresh",
		LastSuccessAt: &freshT,
	})
	fc.putSuite(cache.SuiteFreshness{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1", SuitePath: "/stale",
		LastSuccessAt: &staleT,
	})

	hits := atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if !strings.Contains(r.URL.Path, "/stale/") {
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	// Update the stale-suite url_path to point at this server now that
	// we know the URL.
	fc.putURL(cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1", Path: "/stale/InRelease",
		BlobHash: &bh, UpstreamURL: srv.URL + "/stale/InRelease", IsMetadata: true,
	})

	c, _ := New(Config{
		Cache: fc, Fetcher: newTestFetcher(t),
		Cooldown: 0,
		Refresh:  15 * time.Minute,
		Logger:   discardLogger(),
		now:      func() time.Time { return now },
	})
	c.tick(context.Background())

	if hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1 (stale only)", hits.Load())
	}
}

func TestRun_StopsOnContextCancel(t *testing.T) {
	fc := newFakeCache()
	c, _ := New(Config{
		Cache: fc, Fetcher: newTestFetcher(t),
		Refresh: 100 * time.Millisecond, // forces minFastTickInterval
		Logger:  discardLogger(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return after ctx cancel")
	}
}

// TestCheck_HostLimiterBoundsConcurrency proves that two concurrent
// freshness checks for distinct suites on the same canonical host
// serialize through the shared per-host limiter — addresses the
// codex finding that conditional GETs otherwise bypass
// MaxConcurrentPerHost and create a memory-exhaustion path.
func TestCheck_HostLimiterBoundsConcurrency(t *testing.T) {
	fc := newFakeCache()
	bh := hashOf([]byte("x"))

	// Two suites on the same canonical host. Both have a cached
	// InRelease and matching url_path rows.
	for _, sp := range []string{"/a", "/b"} {
		fc.putURL(cache.URLPath{
			CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
			Path: sp + "/InRelease", BlobHash: &bh, IsMetadata: true,
		})
	}

	// Upstream blocks until released by the test, so we can observe
	// concurrency. inFlight tracks the peak number of simultaneous
	// handlers; with limit=1 it must stay at 1.
	var inFlight, peak atomic.Int64
	gate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := inFlight.Add(1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		<-gate
		inFlight.Add(-1)
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	// Patch the URLs now that we know the test server's address.
	for _, sp := range []string{"/a", "/b"} {
		fc.putURL(cache.URLPath{
			CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
			Path: sp + "/InRelease", BlobHash: &bh,
			UpstreamURL: srv.URL + sp + "/InRelease", IsMetadata: true,
		})
	}

	limit := hostsem.New(1)
	c, err := New(Config{
		Cache:       fc,
		Fetcher:     newTestFetcher(t),
		HostLimiter: limit,
		Logger:      discardLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var wg sync.WaitGroup
	for _, sp := range []string{"/a", "/b"} {
		wg.Add(1)
		go func(sp string) {
			defer wg.Done()
			c.Check(context.Background(), "http", "127.0.0.1", sp)
		}(sp)
	}

	// Both Checks try to acquire the limiter; the second blocks on
	// the upstream gate (via Acquire). Wait for at least one handler
	// to be in flight, then verify the second hasn't snuck in.
	deadline := time.Now().Add(2 * time.Second)
	for inFlight.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if inFlight.Load() == 0 {
		t.Fatalf("first conditional GET never reached upstream")
	}
	// Give the second goroutine ample time to bypass the limiter
	// (it must NOT) before we release the gate.
	time.Sleep(100 * time.Millisecond)
	if got := peak.Load(); got != 1 {
		t.Errorf("peak in-flight = %d, want 1 (limiter must serialize)", got)
	}
	close(gate)
	wg.Wait()

	if got := peak.Load(); got != 1 {
		t.Errorf("post-run peak = %d, want 1", got)
	}
}

func TestRun_RefreshZeroReturnsImmediately(t *testing.T) {
	fc := newFakeCache()
	c, _ := New(Config{
		Cache: fc, Fetcher: newTestFetcher(t),
		Refresh: 0,
		Logger:  discardLogger(),
	})
	done := make(chan struct{})
	go func() {
		c.Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("Run with refresh=0 did not return immediately")
	}
}

func TestNew_RejectsNilDeps(t *testing.T) {
	_, err := New(Config{})
	if err == nil || !strings.Contains(err.Error(), "Cache") {
		t.Errorf("expected nil Cache error, got %v", err)
	}
	_, err = New(Config{Cache: newFakeCache()})
	if err == nil || !strings.Contains(err.Error(), "Fetcher") {
		t.Errorf("expected nil Fetcher error, got %v", err)
	}
	_, err = New(Config{Cache: newFakeCache(), Fetcher: newTestFetcher(t), Cooldown: -1})
	if err == nil {
		t.Errorf("expected negative cooldown error")
	}
	_, err = New(Config{Cache: newFakeCache(), Fetcher: newTestFetcher(t), Refresh: -1})
	if err == nil {
		t.Errorf("expected negative refresh error")
	}
}

func TestCheck_DBReadFailure_LogsAndReturns(t *testing.T) {
	fc := newFakeCache()
	fc.getErr = fmt.Errorf("simulated DB read failure")

	c, _ := New(Config{
		Cache: fc, Fetcher: newTestFetcher(t),
		Logger: discardLogger(),
	})
	c.Check(context.Background(), "http", "127.0.0.1", "/dists/noble")
	if fc.putCount.Load() != 0 {
		t.Errorf("write attempted despite read error")
	}
}

// confirm errors.Is on errFromConditional doesn't accidentally reach
// the success branch — defense against future drift.
func TestCheck_4xxFromUpstream_BumpsCheckOnly(t *testing.T) {
	fc := newFakeCache()
	bh := hashOf([]byte("x"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	fc.putURL(cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1", Path: "/dists/noble/InRelease",
		BlobHash: &bh, UpstreamURL: srv.URL + "/dists/noble/InRelease", IsMetadata: true,
	})

	now := time.Unix(7777, 0)
	c, _ := New(Config{
		Cache: fc, Fetcher: newTestFetcher(t),
		Logger: discardLogger(),
		now:    func() time.Time { return now },
	})
	c.Check(context.Background(), "http", "127.0.0.1", "/dists/noble")

	got, ok := fc.suite("http", "127.0.0.1", "/dists/noble")
	if !ok {
		t.Fatalf("suite missing")
	}
	if got.LastCheckAt == nil || *got.LastCheckAt != 7777 {
		t.Errorf("last_check_at = %v, want 7777", got.LastCheckAt)
	}
	if got.LastSuccessAt != nil {
		t.Errorf("last_success_at = %v, want nil", got.LastSuccessAt)
	}
}

