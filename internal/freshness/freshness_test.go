package freshness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// fakeCache is the in-memory Cache used by these tests. The real cache
// involves SQLite, on-disk pool/, and a single-writer goroutine — none
// of which the freshness checker actually exercises beyond the four
// methods on the Cache interface.
type fakeCache struct {
	mu        sync.Mutex
	suites    map[string]cache.SuiteFreshness // key: scheme|host|path
	urls      map[string]cache.URLPath        // key: scheme|host|path
	snapshots map[int64]cache.SuiteSnapshot   // key: snapshot_id
	listErr   error
	getErr    error
	getErrFn  func() error // consulted after getErr: a non-nil result fails that read
	putErr    error
	lookupErr error
	listHook  func() // called at the top of ListSuites, before the mutex

	putCount atomic.Int64
	// existsCalls counts SuiteExists reads (the I-4 row gate); they fail
	// like GetSuiteFreshness through getErr and getErrFn.
	existsCalls atomic.Int64
}

func newFakeCache() *fakeCache {
	return &fakeCache{
		suites:    make(map[string]cache.SuiteFreshness),
		urls:      make(map[string]cache.URLPath),
		snapshots: make(map[int64]cache.SuiteSnapshot),
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
	if f.getErrFn != nil {
		if err := f.getErrFn(); err != nil {
			return nil, err
		}
	}
	s, ok := f.suites[keyOf(scheme, host, suitePath)]
	if !ok {
		return nil, cache.ErrNotFound
	}
	cp := s
	return &cp, nil
}

// SuiteExists is the I-4 row gate's existence read; it fails like
// GetSuiteFreshness (getErr, then getErrFn), so read-failure tests reach
// the gate too.
func (f *fakeCache) SuiteExists(ctx context.Context, key cache.SuiteKey) (bool, error) {
	f.existsCalls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return false, f.getErr
	}
	if f.getErrFn != nil {
		if err := f.getErrFn(); err != nil {
			return false, err
		}
	}
	_, ok := f.suites[keyOf(key.CanonicalScheme, key.CanonicalHost, key.SuitePath)]
	return ok, nil
}

// SeedSuiteForTest stores s whole, creating the suite: the fixture
// upsert, like cache.(*Cache).SeedSuiteForTest.
func (f *fakeCache) SeedSuiteForTest(ctx context.Context, s cache.SuiteFreshness) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.suites[keyOf(s.CanonicalScheme, s.CanonicalHost, s.SuitePath)] = s
	return nil
}

// track gives a suite an empty row (spec C3: the checker checks only
// tracked suites).
func (f *fakeCache) track(scheme, host, suitePath string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := keyOf(scheme, host, suitePath)
	if _, ok := f.suites[k]; !ok {
		f.suites[k] = cache.SuiteFreshness{CanonicalScheme: scheme, CanonicalHost: host, SuitePath: suitePath}
	}
}

// interest stamps a tracked suite's client interest (last_requested_at,
// spec C4) at unix time at.
func (f *fakeCache) interest(scheme, host, suitePath string, at int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := keyOf(scheme, host, suitePath)
	s := f.suites[k]
	s.LastRequestedAt = &at
	f.suites[k] = s
}

// UpdateSuiteFreshness mirrors the production writer: UPDATE-only (an
// unknown suite is left alone) and only the check columns.
func (f *fakeCache) UpdateSuiteFreshness(ctx context.Context, s cache.SuiteFreshness) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putCount.Add(1)
	if f.putErr != nil {
		return f.putErr
	}
	k := keyOf(s.CanonicalScheme, s.CanonicalHost, s.SuitePath)
	cur, ok := f.suites[k]
	if !ok {
		return nil
	}
	cur.LastCheckAt, cur.LastSuccessAt = s.LastCheckAt, s.LastSuccessAt
	cur.InReleaseETag, cur.InReleaseLastMod = s.InReleaseETag, s.InReleaseLastMod
	cur.InReleaseChangeSeenAt, cur.CheckFailCount = s.InReleaseChangeSeenAt, s.CheckFailCount
	f.suites[k] = cur
	return nil
}

func (f *fakeCache) ListSuitesWithAdoption(ctx context.Context) ([]cache.SuiteWithAdoption, error) {
	if h := f.listHook; h != nil {
		h()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]cache.SuiteWithAdoption, 0, len(f.suites))
	for _, s := range f.suites {
		row := cache.SuiteWithAdoption{SuiteFreshness: s}
		if s.CurrentSnapshotID != nil {
			if snap, ok := f.snapshots[*s.CurrentSnapshotID]; ok {
				row.CurrentAnchorForm = string(snap.Form)
			}
		}
		out = append(out, row)
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

func (f *fakeCache) PutURLPath(ctx context.Context, u cache.URLPath) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return f.putErr
	}
	f.urls[keyOf(u.CanonicalScheme, u.CanonicalHost, u.Path)] = u
	return nil
}

func (f *fakeCache) GetSuiteSnapshot(ctx context.Context, snapshotID int64) (*cache.SuiteSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.snapshots[snapshotID]
	if !ok {
		return nil, cache.ErrNotFound
	}
	cp := s
	return &cp, nil
}

func (f *fakeCache) RecordAdoptionFailure(_ context.Context, scheme, host, suitePath string, r cache.AdoptionFailure) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := keyOf(scheme, host, suitePath)
	s, ok := f.suites[k]
	if !ok {
		return nil
	}
	hash, reason, first, last, next := r.Hash, r.Reason, r.FirstAt, r.LastAt, r.NextRetryAt
	s.AdoptionFailHash, s.AdoptionFailReason, s.AdoptionFailCount = &hash, &reason, r.Count
	s.AdoptionFailFirstAt, s.AdoptionFailLastAt, s.AdoptionNextRetryAt = &first, &last, &next
	s.AdoptionFailETag, s.AdoptionFailLastMod = r.ETag, r.LastMod
	f.suites[k] = s
	return nil
}

func (f *fakeCache) ClearAdoptionFailure(_ context.Context, scheme, host, suitePath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := keyOf(scheme, host, suitePath)
	if s, ok := f.suites[k]; ok {
		s.AdoptionFailHash, s.AdoptionFailReason, s.AdoptionFailCount = nil, nil, 0
		s.AdoptionFailFirstAt, s.AdoptionFailLastAt, s.AdoptionNextRetryAt = nil, nil, nil
		s.AdoptionFailETag, s.AdoptionFailLastMod = nil, nil
		f.suites[k] = s
	}
	return nil
}

func (f *fakeCache) SetAdoptionNextRetry(_ context.Context, scheme, host, suitePath string, next int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := keyOf(scheme, host, suitePath)
	if s, ok := f.suites[k]; ok && s.AdoptionFailCount > 0 {
		s.AdoptionNextRetryAt = &next
		f.suites[k] = s
	}
	return nil
}

func (f *fakeCache) RescheduleFailingAdoptions(_ context.Context, now int64, offset func() int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for k, s := range f.suites {
		if s.AdoptionFailCount < 1 || s.AdoptionFailReason == nil ||
			cache.IsRefusalReason(*s.AdoptionFailReason) || *s.AdoptionFailReason == cache.ReasonPanic {
			continue
		}
		next := now + offset()
		s.AdoptionNextRetryAt = &next
		f.suites[k] = s
		n++
	}
	return n, nil
}

func (f *fakeCache) putSnapshot(s cache.SuiteSnapshot) {
	// Production snapshots always carry a Form (the v7 trigger rejects
	// form-less rows), so a fixture without one models an impossible row.
	if s.Form == "" {
		panic("fakeCache.putSnapshot: fixture must set Form")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshots[s.SnapshotID] = s
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
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("fetch.New: %v", err)
	}
	return c
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestCheck_NoCachedInRelease_Skips: a tracked, never-adopted suite whose
// InRelease url_path row is gone (GC reaped it) is skipped without a
// write. The suite must be tracked: an untracked one stops at I-4's
// pre-check and never reaches this branch.
func TestCheck_NoCachedInRelease_Skips(t *testing.T) {
	fc := newFakeCache()
	fc.track("http", "127.0.0.1", "/dists/noble")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream should not be contacted; got %s", r.URL.Path)
	}))
	defer srv.Close()

	c, err := New(Config{
		Cache:       fc,
		Fetcher:     newTestFetcher(t),
		HostLimiter: hostsem.New(8),
		Cooldown:    0,
		Logger:      discardLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.Check(context.Background(), "http", "127.0.0.1", "/dists/noble")
	if fc.putCount.Load() != 0 {
		t.Errorf("UpdateSuiteFreshness was called; want skip without write")
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
		Cache:       fc,
		Fetcher:     newTestFetcher(t),
		HostLimiter: hostsem.New(8),
		Cooldown:    60 * time.Second,
		Logger:      discardLogger(),
		now:         func() time.Time { return now },
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
		Cache:       fc,
		Fetcher:     newTestFetcher(t),
		HostLimiter: hostsem.New(8),
		Cooldown:    60 * time.Second,
		Logger:      discardLogger(),
		now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.Check(context.Background(), "http", "127.0.0.1", "/dists/noble")
	if fc.putCount.Load() != 0 {
		t.Errorf("UpdateSuiteFreshness called inside cooldown")
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
		Cache:       fc,
		Fetcher:     newTestFetcher(t),
		HostLimiter: hostsem.New(8),
		Cooldown:    0,
		Logger:      discardLogger(),
		now:         func() time.Time { return now },
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
	fc.track("http", "127.0.0.1", "/dists/noble")
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
		Cache: fc, Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8),
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
		Cache: fc, Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8),
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
		Cache: fc, Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8),
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
		Cache: fc, Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8),
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
	fc.track("http", "127.0.0.1", "/dists/noble")
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
		Cache: fc, Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8),
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
		Cache: fc, Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8),
		Cooldown: 0,
		Refresh:  15 * time.Minute,
		Logger:   discardLogger(),
		now:      func() time.Time { return now },
	})
	drainTick(context.Background(), c)

	if hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1 (stale only)", hits.Load())
	}
}

// TestStaleSuiteCounts covers the SPEC §7.4 observability helper that
// powers the freshness_stale_suites gauge: partition suites whose
// last_success_at aged past the stale threshold into adopted vs
// unadopted, so a frozen ADOPTED suite (the silent freeze) is alertable.
func TestStaleSuiteCounts(t *testing.T) {
	const staleBefore = int64(1000)
	i := func(v int64) *int64 { return &v }
	mk := func(lastSuccess, snap *int64) cache.SuiteFreshness {
		return cache.SuiteFreshness{LastSuccessAt: lastSuccess, CurrentSnapshotID: snap}
	}
	const now = int64(3000)
	suites := []cache.SuiteFreshness{
		mk(i(2000), i(7)), // fresh adopted        -> not stale
		mk(i(500), i(7)),  // stale adopted        -> count adopted
		mk(i(999), i(7)),  // just-stale adopted   -> count adopted
		mk(i(400), nil),   // stale unadopted      -> count unadopted
		mk(nil, i(7)),     // never-succeeded      -> not "went stale", skip
		mk(i(9000), i(7)), // in the future (clock step) -> infinitely old: count adopted
		mk(i(3001), i(7)), // 1 s ahead, within tolerance -> fresh
		mk(i(3002), i(7)), // 2 s ahead: the tolerance edge -> fresh
		mk(i(3003), i(7)), // 3 s ahead -> infinitely old: count adopted
	}
	adopted, unadopted, refused := staleSuiteCounts(suites, staleBefore, now)
	if adopted != 4 {
		t.Errorf("adopted stale = %d, want 4", adopted)
	}
	if unadopted != 1 {
		t.Errorf("unadopted stale = %d, want 1", unadopted)
	}
	if refused != 0 {
		t.Errorf("refused stale = %d, want 0 (no suite has a record)", refused)
	}
}

func TestRun_StopsOnContextCancel(t *testing.T) {
	fc := newFakeCache()
	c, _ := New(Config{
		Cache: fc, Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8),
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
		fc.track("http", "127.0.0.1", sp)
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
		Cache: fc, Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8),
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
	_, err = New(Config{Cache: newFakeCache(), Fetcher: newTestFetcher(t)})
	if err == nil || !strings.Contains(err.Error(), "HostLimiter") {
		t.Errorf("expected nil HostLimiter error, got %v", err)
	}
	limiter := hostsem.New(8)
	_, err = New(Config{Cache: newFakeCache(), Fetcher: newTestFetcher(t), HostLimiter: limiter, Cooldown: -1})
	if err == nil {
		t.Errorf("expected negative cooldown error")
	}
	_, err = New(Config{Cache: newFakeCache(), Fetcher: newTestFetcher(t), HostLimiter: limiter, Refresh: -1})
	if err == nil {
		t.Errorf("expected negative refresh error")
	}
}

func TestCheck_DBReadFailure_LogsAndReturns(t *testing.T) {
	fc := newFakeCache()
	fc.getErr = fmt.Errorf("simulated DB read failure")

	c, _ := New(Config{
		Cache: fc, Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8),
		Logger: discardLogger(),
	})
	c.Check(context.Background(), "http", "127.0.0.1", "/dists/noble")
	if fc.putCount.Load() != 0 {
		t.Errorf("write attempted despite read error")
	}
}

// TestCheck_RowReadFailsAfterPreCheck_LogsAndSkips reaches checkLocked's
// own read-error branch: I-4's pre-check read succeeds (the suite is
// tracked), then checkLocked's read fails. The check logs the failure
// once, writes nothing, does not panic, keeps the lock-map entry (a read
// error is not a vanished row) and leaves the mutex free.
func TestCheck_RowReadFailsAfterPreCheck_LogsAndSkips(t *testing.T) {
	fc := newFakeCache()
	fc.track("http", "127.0.0.1", "/dists/noble")
	var reads atomic.Int64
	fc.getErrFn = func() error {
		if reads.Add(1) == 1 {
			return nil // I-4's pre-check
		}
		return fmt.Errorf("simulated DB read failure")
	}
	logs := &lockedBuffer{}
	c, err := New(Config{
		Cache: fc, Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8),
		Logger: slog.New(slog.NewTextHandler(logs, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ref := SuiteRef{CanonicalScheme: "http", CanonicalHost: "127.0.0.1", SuitePath: "/dists/noble"}
	if got := c.check(context.Background(), ref, checkOpts{}).result; got != resultSkipped {
		t.Errorf("result = %q, want %q", got, resultSkipped)
	}
	if n := reads.Load(); n < 2 {
		t.Fatalf("GetSuiteFreshness called %d times; want the pre-check and checkLocked's read", n)
	}
	if fc.putCount.Load() != 0 {
		t.Errorf("write attempted despite read error")
	}
	out := logs.String()
	if n := strings.Count(out, "freshness: read suite row failed"); n != 1 {
		t.Errorf("read-failure WARN logged %d times, want 1 (checkLocked's):\n%s", n, out)
	}
	if strings.Contains(out, "freshness_check_panicked") {
		t.Errorf("check panicked on a read error:\n%s", out)
	}
	if n := c.LockCount(); n != 1 {
		t.Errorf("LockCount = %d, want 1: a read error must not drop the entry of a suite that has a row", n)
	}
	fc.getErrFn = nil
	if got := c.check(context.Background(), ref, checkOpts{}).result; got == resultBusy {
		t.Errorf("the suite mutex is still held after the read-error check")
	}
}

// confirm errors.Is on errFromConditional doesn't accidentally reach
// the success branch — defense against future drift.
func TestCheck_4xxFromUpstream_BumpsCheckOnly(t *testing.T) {
	fc := newFakeCache()
	fc.track("http", "127.0.0.1", "/dists/noble")
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
		Cache: fc, Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8),
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

// TestCheck_UsesSnapshotHashForComparison verifies the SPEC2 wiring:
// when a suite has CurrentSnapshotID set, the body comparison uses
// suite_snapshot.inrelease_hash, not url_path.blob_hash. Without this
// branch, the freshness check after a successful adoption would see
// stale url_path data and re-trigger adoption forever.
func TestCheck_UsesSnapshotHashForComparison(t *testing.T) {
	fc := newFakeCache()
	body := []byte("post-adoption InRelease body")
	bodyHash := hashOf(body)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	now := time.Unix(8888, 0)
	then := now.Add(-1 * time.Hour).Unix()

	// Pre-seed: url_path.blob_hash points at a STALE pre-adoption blob.
	stale := strings.Repeat("0", 64)
	fc.putURL(cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
		Path: "/dists/noble/InRelease", BlobHash: &stale,
		UpstreamURL: srv.URL + "/dists/noble/InRelease", IsMetadata: true,
	})
	// Pre-seed suite_freshness with CurrentSnapshotID pointing at the
	// snapshot whose inrelease_hash IS bodyHash (post-adoption shape).
	snapID := int64(42)
	fc.putSuite(cache.SuiteFreshness{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
		SuitePath:         "/dists/noble",
		LastSuccessAt:     &then,
		CurrentSnapshotID: &snapID,
	})
	fc.putSnapshot(cache.SuiteSnapshot{
		SnapshotID:       snapID,
		SingleAnchorHash: &bodyHash,
		Form:             cache.FormInline,
	})

	c, err := New(Config{
		Cache: fc, Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8),
		Logger: discardLogger(),
		now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	c.Check(context.Background(), "http", "127.0.0.1", "/dists/noble")

	got, ok := fc.suite("http", "127.0.0.1", "/dists/noble")
	if !ok {
		t.Fatal("suite_freshness was not persisted; check did not run to success")
	}
	// Body matched snapshot's inrelease_hash → "unchanged" branch →
	// inrelease_change_seen_at must remain nil.
	if got.InReleaseChangeSeenAt != nil {
		t.Errorf("expected unchanged with snapshot match; change_seen_at=%v",
			got.InReleaseChangeSeenAt)
	}
}

// TestCheck_RecoversWhenAnchorMissingButSnapshotPresent is the regression
// test for the freshness-freeze trap. When GC has reaped the InRelease
// url_path anchor row but the suite still has a current snapshot, the
// checker must reconstruct the upstream URL, re-seed the anchor, and run
// the conditional GET — NOT silently dead-end at "no cached InRelease
// url_path" and freeze the suite forever. SPEC2 §7.4.
func TestCheck_RecoversWhenAnchorMissingButSnapshotPresent(t *testing.T) {
	fc := newFakeCache()
	newBody := []byte("rolled-forward InRelease body")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/dists/noble-updates/InRelease") {
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
		}
		w.WriteHeader(200)
		_, _ = w.Write(newBody)
	}))
	defer srv.Close()

	now := time.Unix(1_780_000_000, 0)
	then := now.Add(-8 * 24 * time.Hour).Unix() // frozen ~8 days ago

	// Adopted suite: current snapshot's inrelease_hash is the OLD bytes,
	// so the rolled-forward upstream body reads as "changed".
	staleHash := strings.Repeat("a", 64)
	snapID := int64(1481)
	fc.putSnapshot(cache.SuiteSnapshot{SnapshotID: snapID, SingleAnchorHash: &staleHash, Form: cache.FormInline})
	fc.putSuite(cache.SuiteFreshness{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
		SuitePath:         "/dists/noble-updates",
		LastCheckAt:       &then,
		LastSuccessAt:     &then,
		CurrentSnapshotID: &snapID,
	})
	// NOTE: deliberately NO url_path row for .../InRelease — GC reaped it.
	// This is the exact freeze scenario.

	c, err := New(Config{
		Cache: fc, Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8),
		Cooldown: 0, Logger: discardLogger(),
		now:            func() time.Time { return now },
		urlReconstruct: func(scheme, host, path string) string { return srv.URL + path },
	})
	if err != nil {
		t.Fatal(err)
	}
	c.Check(context.Background(), "http", "127.0.0.1", "/dists/noble-updates")

	// 1. No longer frozen: last_check_at / last_success_at advanced to now.
	got, ok := fc.suite("http", "127.0.0.1", "/dists/noble-updates")
	if !ok {
		t.Fatal("suite_freshness missing")
	}
	if got.LastCheckAt == nil || *got.LastCheckAt != now.Unix() {
		t.Fatalf("last_check_at not advanced: got %v want %d (suite still frozen)", got.LastCheckAt, now.Unix())
	}
	if got.LastSuccessAt == nil || *got.LastSuccessAt != now.Unix() {
		t.Errorf("last_success_at not advanced: got %v want %d", got.LastSuccessAt, now.Unix())
	}
	// 2. The change was observed (upstream rolled forward).
	if got.InReleaseChangeSeenAt == nil {
		t.Error("expected change_seen_at to be set on observed upstream change")
	}
	// 3. The anchor url_path row was re-seeded so normal operation resumes.
	u, err := fc.LookupURL(context.Background(), "http", "127.0.0.1", "/dists/noble-updates/InRelease")
	if err != nil {
		t.Fatalf("anchor url_path not re-seeded: %v", err)
	}
	if !u.IsMetadata {
		t.Error("re-seeded anchor should be is_metadata=1")
	}
	if u.BlobHash == nil || *u.BlobHash != staleHash {
		t.Errorf("re-seeded anchor blob_hash = %v, want snapshot inrelease_hash %s", u.BlobHash, staleHash)
	}
}

// TestCheck_DetachedRecoversWhenAnchorsMissing is the detached-mode
// counterpart to TestCheck_RecoversWhenAnchorMissingButSnapshotPresent:
// a suite with a detached current snapshot whose Release / Release.gpg
// anchors were reaped must reconstruct + re-seed them and run the check,
// not freeze. SPEC2 §7.4 / §7.6.3.
func TestCheck_DetachedRecoversWhenAnchorsMissing(t *testing.T) {
	fc := newFakeCache()
	releaseText := []byte("rolled-forward Release body")
	sigBody := []byte("Release.gpg bytes")
	var relHits, gpgHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dists/noble/Release":
			relHits.Add(1)
			w.WriteHeader(200)
			_, _ = w.Write(releaseText)
		case "/dists/noble/Release.gpg":
			gpgHits.Add(1)
			w.WriteHeader(200)
			_, _ = w.Write(sigBody)
		default:
			t.Errorf("unexpected fetch: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	now := time.Unix(1_780_000_000, 0)
	then := now.Add(-8 * 24 * time.Hour).Unix()
	staleRH := strings.Repeat("b", 64)
	rgh := hashOf([]byte("adopted gpg"))
	snapID := int64(1482)
	fc.putSnapshot(cache.SuiteSnapshot{
		SnapshotID: snapID, CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
		SuitePath: "/dists/noble", ReleaseHash: &staleRH, ReleaseGPGHash: &rgh,
		Form: cache.FormDetached,
	})
	fc.putSuite(cache.SuiteFreshness{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1", SuitePath: "/dists/noble",
		LastCheckAt: &then, LastSuccessAt: &then, CurrentSnapshotID: &snapID,
	})
	// NOTE: no Release / Release.gpg url_path rows — both reaped.

	c, err := New(Config{
		Cache: fc, Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8),
		Cooldown: 0, Logger: discardLogger(),
		now:            func() time.Time { return now },
		urlReconstruct: func(scheme, host, path string) string { return srv.URL + path },
	})
	if err != nil {
		t.Fatal(err)
	}
	c.Check(context.Background(), "http", "127.0.0.1", "/dists/noble")

	got, ok := fc.suite("http", "127.0.0.1", "/dists/noble")
	if !ok {
		t.Fatal("suite_freshness missing")
	}
	if got.LastCheckAt == nil || *got.LastCheckAt != now.Unix() {
		t.Fatalf("last_check_at not advanced (detached suite still frozen): got %v", got.LastCheckAt)
	}
	if relHits.Load() != 1 {
		t.Errorf("Release hits = %d, want 1 (recovery should issue the GET)", relHits.Load())
	}
	if _, err := fc.LookupURL(context.Background(), "http", "127.0.0.1", "/dists/noble/Release"); err != nil {
		t.Errorf("Release anchor not re-seeded: %v", err)
	}
	if _, err := fc.LookupURL(context.Background(), "http", "127.0.0.1", "/dists/noble/Release.gpg"); err != nil {
		t.Errorf("Release.gpg anchor not re-seeded: %v", err)
	}
}

// TestCheck_NoAdopter_ChangeOnlyLogged verifies Phase 1 behavior is
// unchanged when no Adopter is wired in.
func TestCheck_NoAdopter_ChangeOnlyLogged(t *testing.T) {
	fc := newFakeCache()
	fc.track("http", "127.0.0.1", "/dists/noble")
	body := []byte("new InRelease bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	stale := strings.Repeat("0", 64)
	fc.putURL(cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
		Path: "/dists/noble/InRelease", BlobHash: &stale,
		UpstreamURL: srv.URL + "/dists/noble/InRelease", IsMetadata: true,
	})
	now := time.Unix(9000, 0)
	c, _ := New(Config{
		Cache: fc, Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8),
		Logger: discardLogger(),
		now:    func() time.Time { return now },
	})
	c.Check(context.Background(), "http", "127.0.0.1", "/dists/noble")
	c.WaitForAdoptions() // no-op without Adopter, but exercises the API

	got, ok := fc.suite("http", "127.0.0.1", "/dists/noble")
	if !ok {
		t.Fatal("suite_freshness was not persisted")
	}
	if got.InReleaseChangeSeenAt == nil {
		t.Error("expected change_seen_at to be set on observed change")
	}
}

// TestCheck_AdopterInvokedOnChange verifies the wire-in: when the
// freshness check observes an upstream change, Check hands off to
// the Adopter via the goroutine + mutex handoff.
func TestCheck_AdopterInvokedOnChange(t *testing.T) {
	fc := newFakeCache()
	fc.track("http", "127.0.0.1", "/dists/noble")
	fc.interest("http", "127.0.0.1", "/dists/noble", 11000)
	memberBody := []byte("member content")
	memberHash := hashOf(memberBody)
	releaseText := fmt.Sprintf(
		"Origin: Test\nSuite: noble\nSHA256:\n %s %d main/Sources\n",
		memberHash, len(memberBody))
	body := simtest.ClearsignedFixture(releaseText)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	stale := strings.Repeat("0", 64)
	fc.putURL(cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
		Path: "/dists/noble/InRelease", BlobHash: &stale,
		UpstreamURL: srv.URL + "/dists/noble/InRelease", IsMetadata: true, RequestCount: 1,
	})

	// Real *cache.Cache for the Adopter side-effect; fakeCache for
	// the Checker. They don't share state.
	dir := t.TempDir()
	realCache, err := cache.Open(context.Background(), dir, discardLogger()) // A-30
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = realCache.Close() })
	trackSuite(t, realCache, SuiteRef{CanonicalScheme: "http", CanonicalHost: "127.0.0.1", SuitePath: "/dists/noble"})

	// Adopter's fetcher serves member content at the URL the Adopter
	// will construct from the suite. The Adopter builds URLs from
	// suite.CanonicalScheme + suite.CanonicalHost + suite.SuitePath
	// + memberPath — so no port (the Checker passes "127.0.0.1" as
	// the canonical host, port-less per Phase 1's host allowlist).
	// httptest's actual port is irrelevant here because the
	// AdoptionFetcher is a fake that matches URL strings, not a real
	// network client.
	memberURL := "http://127.0.0.1/dists/noble/main/Sources"
	memberFetcher := newFakeFetcher()
	memberFetcher.put(memberURL, memberBody)

	adopter, err := NewAdopter(AdoptionConfig{
		Cache:       realCache,
		Fetcher:     memberFetcher,
		Verifier:    passThroughVerifier{},
		HostLimiter: hostsem.New(8),
		Logger:      discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}

	c, err := New(Config{
		Cache: fc, Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8),
		Logger:  discardLogger(),
		now:     func() time.Time { return time.Unix(11000, 0) },
		Adopter: adopter,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.Check(context.Background(), "http", "127.0.0.1", "/dists/noble")
	c.WaitForAdoptions()

	// The Adopter wrote a suite_snapshot row to the real cache.
	snap, err := realCache.GetSuiteSnapshot(context.Background(), 1)
	if err != nil {
		t.Fatalf("expected adopter to have created snapshot 1: %v", err)
	}
	if snap.AdoptedAt == nil {
		t.Errorf("snapshot adopted_at not stamped — adoption goroutine didn't reach commit")
	}
	if snap.SingleAnchorHash == nil || *snap.SingleAnchorHash != hashOf(body) {
		t.Errorf("snapshot.inrelease_hash mismatch: got %v, want %s",
			snap.SingleAnchorHash, hashOf(body))
	}
}

// --- SPEC2 §7.6.3 detached-mode freshness checks ---

// TestCheck_DetachedKnownSuite_AdoptsOnReleaseChange covers the
// "form is detected from current snapshot" path: an existing suite
// whose snapshot has release_hash set conditional-GETs Release (NOT
// InRelease), and on observed change, fetches Release.gpg + spawns
// RunDetached.
func TestCheck_DetachedKnownSuite_AdoptsOnReleaseChange(t *testing.T) {
	fc := newFakeCache()

	memberBody := []byte("detached member content")
	memberHash := hashOf(memberBody)
	releaseText := []byte(fmt.Sprintf(
		"Origin: Test\nSuite: noble\nSHA256:\n %s %d main/Sources\n",
		memberHash, len(memberBody),
	))
	sigBody := armoredSig("placeholder Release.gpg bytes")

	staleHash := strings.Repeat("0", 64)

	var releaseHits, gpgHits atomic.Int32
	var gotInReleaseFetch atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dists/noble/Release":
			releaseHits.Add(1)
			w.WriteHeader(200)
			_, _ = w.Write(releaseText)
		case "/dists/noble/Release.gpg":
			gpgHits.Add(1)
			w.WriteHeader(200)
			_, _ = w.Write(sigBody)
		case "/dists/noble/InRelease":
			// Spec C4 Dispatch 1: a known-detached suite probes InRelease
			// on every check (a lateral move to inline); absent here.
			gotInReleaseFetch.Store(true)
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected fetch: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	// Seed url_path rows for Release and Release.gpg (both required
	// before the detached check runs).
	staleRH := staleHash
	fc.putURL(cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
		Path: "/dists/noble/Release", BlobHash: &staleRH,
		UpstreamURL: srv.URL + "/dists/noble/Release", IsMetadata: true,
	})
	fc.putURL(cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
		Path:        "/dists/noble/Release.gpg",
		UpstreamURL: srv.URL + "/dists/noble/Release.gpg",
		IsMetadata:  true,
	})

	// Snapshot row that flags the suite as detached: release_hash
	// non-nil, inrelease_hash nil.
	rh := staleHash
	rgh := hashOf([]byte("any old gpg hash"))
	fc.putSnapshot(cache.SuiteSnapshot{
		SnapshotID:      42,
		CanonicalScheme: "http",
		CanonicalHost:   "127.0.0.1",
		SuitePath:       "/dists/noble",
		ReleaseHash:     &rh,
		ReleaseGPGHash:  &rgh,
		Form:            cache.FormDetached,
	})
	id := int64(42)
	fc.putSuite(cache.SuiteFreshness{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
		SuitePath:         "/dists/noble",
		CurrentSnapshotID: &id,
	})

	// Adopter side: a real cache.Cache so RunDetached can write the
	// snapshot row, plus a fake AdoptionFetcher serving the declared
	// member.
	dir := t.TempDir()
	realCache, err := cache.Open(context.Background(), dir, discardLogger()) // A-30
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = realCache.Close() })
	trackSuite(t, realCache, SuiteRef{CanonicalScheme: "http", CanonicalHost: "127.0.0.1", SuitePath: "/dists/noble"})

	memberURL := "http://127.0.0.1/dists/noble/main/Sources"
	memberFetcher := newFakeFetcher()
	memberFetcher.put(memberURL, memberBody)

	adopter, err := NewAdopter(AdoptionConfig{
		Cache:       realCache,
		Fetcher:     memberFetcher,
		Verifier:    passThroughVerifier{},
		HostLimiter: hostsem.New(8),
		Logger:      discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}

	c, err := New(Config{
		Cache: fc, Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8),
		Logger:  discardLogger(),
		now:     func() time.Time { return time.Unix(11000, 0) },
		Adopter: adopter,
	})
	if err != nil {
		t.Fatal(err)
	}

	c.Check(context.Background(), "http", "127.0.0.1", "/dists/noble")
	c.WaitForAdoptions()

	if !gotInReleaseFetch.Load() {
		t.Error("known-detached suite did not probe InRelease")
	}
	if releaseHits.Load() != 1 {
		t.Errorf("Release hits = %d, want 1", releaseHits.Load())
	}
	if gpgHits.Load() != 1 {
		t.Errorf("Release.gpg hits = %d, want 1", gpgHits.Load())
	}

	snap, err := realCache.GetSuiteSnapshot(context.Background(), 1)
	if err != nil {
		t.Fatalf("expected adopter to have created snapshot 1: %v", err)
	}
	if snap.AdoptedAt == nil {
		t.Errorf("snapshot adopted_at not stamped — adoption goroutine didn't reach commit")
	}
	if snap.ReleaseHash == nil || *snap.ReleaseHash != hashOf(releaseText) {
		t.Errorf("snapshot.release_hash mismatch: got %v, want %s",
			snap.ReleaseHash, hashOf(releaseText))
	}
	if snap.ReleaseGPGHash == nil || *snap.ReleaseGPGHash != hashOf(sigBody) {
		t.Errorf("snapshot.release_gpg_hash mismatch: got %v, want %s",
			snap.ReleaseGPGHash, hashOf(sigBody))
	}
	if snap.SingleAnchorHash != nil {
		t.Errorf("snapshot.inrelease_hash should be nil in detached mode, got %s", *snap.SingleAnchorHash)
	}
}

// TestCheck_DetachedFallback_OnInRelease404 covers the bootstrap
// case: a fresh suite whose upstream returns 404 on InRelease must
// fall back to detached mode and adopt via Release + Release.gpg.
func TestCheck_DetachedFallback_OnInRelease404(t *testing.T) {
	fc := newFakeCache()
	fc.track("http", "127.0.0.1", "/dists/noble")
	fc.interest("http", "127.0.0.1", "/dists/noble", 12000)

	memberBody := []byte("fallback member content")
	memberHash := hashOf(memberBody)
	releaseText := []byte(fmt.Sprintf(
		"Origin: Test\nSuite: noble\nSHA256:\n %s %d main/Sources\n",
		memberHash, len(memberBody),
	))
	sigBody := armoredSig("placeholder Release.gpg bytes for fallback")

	var inReleaseHits, releaseHits, gpgHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dists/noble/InRelease":
			inReleaseHits.Add(1)
			w.WriteHeader(http.StatusNotFound)
		case "/dists/noble/Release":
			releaseHits.Add(1)
			w.WriteHeader(200)
			_, _ = w.Write(releaseText)
		case "/dists/noble/Release.gpg":
			gpgHits.Add(1)
			w.WriteHeader(200)
			_, _ = w.Write(sigBody)
		default:
			t.Errorf("unexpected fetch: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	// Seed url_path rows for all three. The InRelease row carries a
	// stale BlobHash so the conditional GET actually fires (without
	// it the "no baseline blob hash" guard would skip before the GET
	// happens, and the 404 → detached fallback trigger never runs).
	// This models a suite whose upstream USED to serve InRelease and
	// has since switched to detached-only.
	staleHash := strings.Repeat("0", 64)
	staleIR := staleHash
	fc.putURL(cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
		Path: "/dists/noble/InRelease", BlobHash: &staleIR,
		UpstreamURL: srv.URL + "/dists/noble/InRelease", IsMetadata: true,
	})
	staleRH := staleHash
	fc.putURL(cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
		Path: "/dists/noble/Release", BlobHash: &staleRH,
		UpstreamURL: srv.URL + "/dists/noble/Release", IsMetadata: true, RequestCount: 1,
	})
	fc.putURL(cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
		Path:        "/dists/noble/Release.gpg",
		UpstreamURL: srv.URL + "/dists/noble/Release.gpg", IsMetadata: true,
	})

	dir := t.TempDir()
	realCache, err := cache.Open(context.Background(), dir, discardLogger()) // A-30
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = realCache.Close() })
	trackSuite(t, realCache, SuiteRef{CanonicalScheme: "http", CanonicalHost: "127.0.0.1", SuitePath: "/dists/noble"})

	memberURL := "http://127.0.0.1/dists/noble/main/Sources"
	memberFetcher := newFakeFetcher()
	memberFetcher.put(memberURL, memberBody)

	adopter, err := NewAdopter(AdoptionConfig{
		Cache:       realCache,
		Fetcher:     memberFetcher,
		Verifier:    passThroughVerifier{},
		HostLimiter: hostsem.New(8),
		Logger:      discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}

	c, err := New(Config{
		Cache: fc, Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8),
		Logger:  discardLogger(),
		now:     func() time.Time { return time.Unix(12000, 0) },
		Adopter: adopter,
	})
	if err != nil {
		t.Fatal(err)
	}

	c.Check(context.Background(), "http", "127.0.0.1", "/dists/noble")
	c.WaitForAdoptions()

	if inReleaseHits.Load() != 1 {
		t.Errorf("InRelease hits = %d, want 1 (the 404 that triggers fallback)", inReleaseHits.Load())
	}
	if releaseHits.Load() != 1 {
		t.Errorf("Release hits = %d, want 1", releaseHits.Load())
	}
	if gpgHits.Load() != 1 {
		t.Errorf("Release.gpg hits = %d, want 1", gpgHits.Load())
	}

	snap, err := realCache.GetSuiteSnapshot(context.Background(), 1)
	if err != nil {
		t.Fatalf("expected adopter to have created snapshot 1: %v", err)
	}
	if snap.ReleaseHash == nil || *snap.ReleaseHash != hashOf(releaseText) {
		t.Errorf("snapshot.release_hash mismatch: got %v, want %s",
			snap.ReleaseHash, hashOf(releaseText))
	}
	if snap.SingleAnchorHash != nil {
		t.Errorf("fallback adoption set inrelease_hash; want detached form (nil)")
	}
}

// TestCheck_DetachedFallback_OnMissingInReleaseURLRow covers the
// other detached-fallback bootstrap path: the suite has Release +
// Release.gpg url_path rows but no InRelease row at all (real-world:
// upstream never served InRelease, so apt's first request 404'd and
// the handler created no row). The inline path's LookupURL returns
// ErrNotFound; the fallback fires without ever issuing a wasted
// InRelease GET.
func TestCheck_DetachedFallback_OnMissingInReleaseURLRow(t *testing.T) {
	fc := newFakeCache()
	fc.track("http", "127.0.0.1", "/dists/noble")
	fc.interest("http", "127.0.0.1", "/dists/noble", 14000)

	memberBody := []byte("missing-row member content")
	memberHash := hashOf(memberBody)
	releaseText := []byte(fmt.Sprintf(
		"Origin: Test\nSuite: noble\nSHA256:\n %s %d main/Sources\n",
		memberHash, len(memberBody),
	))
	sigBody := armoredSig("placeholder Release.gpg")

	var inReleaseHits, releaseHits, gpgHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dists/noble/InRelease":
			// Spec C4 Dispatch 3: the probe tree asks for InRelease
			// first, with or without a cached row.
			inReleaseHits.Add(1)
			w.WriteHeader(404)
		case "/dists/noble/Release":
			releaseHits.Add(1)
			w.WriteHeader(200)
			_, _ = w.Write(releaseText)
		case "/dists/noble/Release.gpg":
			gpgHits.Add(1)
			w.WriteHeader(200)
			_, _ = w.Write(sigBody)
		default:
			t.Errorf("unexpected fetch: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	// Only Release + Release.gpg url_path rows (the InRelease row is
	// deliberately absent, modelling an upstream that never served
	// InRelease).
	staleRH := strings.Repeat("0", 64)
	fc.putURL(cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
		Path: "/dists/noble/Release", BlobHash: &staleRH,
		UpstreamURL: srv.URL + "/dists/noble/Release", IsMetadata: true, RequestCount: 1,
	})
	fc.putURL(cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
		Path:        "/dists/noble/Release.gpg",
		UpstreamURL: srv.URL + "/dists/noble/Release.gpg", IsMetadata: true,
	})

	dir := t.TempDir()
	realCache, err := cache.Open(context.Background(), dir, discardLogger()) // A-30
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = realCache.Close() })
	trackSuite(t, realCache, SuiteRef{CanonicalScheme: "http", CanonicalHost: "127.0.0.1", SuitePath: "/dists/noble"})

	memberURL := "http://127.0.0.1/dists/noble/main/Sources"
	memberFetcher := newFakeFetcher()
	memberFetcher.put(memberURL, memberBody)

	adopter, err := NewAdopter(AdoptionConfig{
		Cache:       realCache,
		Fetcher:     memberFetcher,
		Verifier:    passThroughVerifier{},
		HostLimiter: hostsem.New(8),
		Logger:      discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}

	c, err := New(Config{
		Cache: fc, Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8),
		Logger:  discardLogger(),
		now:     func() time.Time { return time.Unix(14000, 0) },
		Adopter: adopter,
	})
	if err != nil {
		t.Fatal(err)
	}

	c.Check(context.Background(), "http", "127.0.0.1", "/dists/noble")
	c.WaitForAdoptions()

	if inReleaseHits.Load() != 1 {
		t.Errorf("InRelease hits = %d, want 1 (the probe tree's absent InRelease)", inReleaseHits.Load())
	}
	if releaseHits.Load() != 1 {
		t.Errorf("Release hits = %d, want 1", releaseHits.Load())
	}
	if gpgHits.Load() != 1 {
		t.Errorf("Release.gpg hits = %d, want 1", gpgHits.Load())
	}

	snap, err := realCache.GetSuiteSnapshot(context.Background(), 1)
	if err != nil {
		t.Fatalf("expected adopter to have created snapshot 1: %v", err)
	}
	if snap.ReleaseHash == nil {
		t.Errorf("snapshot.release_hash unset after detached fallback")
	}
	if snap.SingleAnchorHash != nil {
		t.Errorf("snapshot.inrelease_hash unexpectedly set in detached fallback")
	}
}

// TestCheck_404OnInRelease_NoFallback_WhenReleaseRowsMissing covers
// the negative case: a fresh suite that 404s on InRelease and lacks
// Release / Release.gpg url_path rows must record the failure rather
// than retry as detached. Without this, a transient InRelease 404 on
// an inline-only suite would log a misleading detached-mode fetch.
func TestCheck_404OnInRelease_NoFallback_WhenReleaseRowsMissing(t *testing.T) {
	fc := newFakeCache()
	fc.track("http", "127.0.0.1", "/dists/noble")

	var releaseHits, gpgHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dists/noble/InRelease":
			w.WriteHeader(http.StatusNotFound)
		case "/dists/noble/Release":
			releaseHits.Add(1)
			t.Errorf("Release fetched despite missing url_path row")
		case "/dists/noble/Release.gpg":
			gpgHits.Add(1)
			t.Errorf("Release.gpg fetched despite missing url_path row")
		default:
			t.Errorf("unexpected fetch: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	// Only the InRelease url_path row exists.
	bh := hashOf([]byte("baseline"))
	fc.putURL(cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
		Path: "/dists/noble/InRelease", BlobHash: &bh,
		UpstreamURL: srv.URL + "/dists/noble/InRelease", IsMetadata: true,
	})

	c, _ := New(Config{
		Cache: fc, Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8),
		Logger: discardLogger(),
		now:    func() time.Time { return time.Unix(13000, 0) },
	})
	c.Check(context.Background(), "http", "127.0.0.1", "/dists/noble")

	got, ok := fc.suite("http", "127.0.0.1", "/dists/noble")
	if !ok {
		t.Fatalf("suite missing")
	}
	if got.LastCheckAt == nil || *got.LastCheckAt != 13000 {
		t.Errorf("last_check_at = %v, want 13000", got.LastCheckAt)
	}
	if got.LastSuccessAt != nil {
		t.Errorf("last_success_at = %v, want nil (4xx is failure)", got.LastSuccessAt)
	}
	if releaseHits.Load() != 0 || gpgHits.Load() != 0 {
		t.Errorf("Release/Release.gpg fetched despite missing url_path rows: r=%d g=%d",
			releaseHits.Load(), gpgHits.Load())
	}
}

// TestCheck_UnchangedTick_ReconcilesDegraded is the SPEC6_8 end-to-end
// auto-heal test: an unchanged freshness tick (the suite is confirmed
// stable at upstream) triggers ReconcileSnapshot on the current degraded
// snapshot, healing a declared-but-absent requestable IndexTarget
// (binary-all/Packages) in place — no new snapshot, just the missing
// member added to the existing one.
//
// Setup mirrors TestChecker_FreshTickRepairsSkippedMembers in
// phase6_7_skip_repair_test.go: real *cache.Cache via newAdoptionTestEnv,
// the Checker's freshness conditional GET uses a real *fetch.Client
// against a loopback httptest.Server that serves the byte-identical
// InRelease (unchanged), while the Adopter uses env.fetcher (fakeFetcher)
// to serve binary-all once we enable it.
func TestCheck_UnchangedTick_ReconcilesDegraded(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)

	// Override the suite to use loopback so newTestFetcher's allowlist
	// (^127\.0\.0\.1$) permits the conditional GET.
	env.suite = SuiteRef{
		CanonicalScheme: "http",
		CanonicalHost:   "127.0.0.1",
		SuitePath:       "/ubuntu/dists/noble",
	}

	// Build an adopter with RepairSkippedMembers=true (gates reconcile too)
	// and Architectures=["amd64"] so binary-all is a requestable target.
	ad, err := NewAdopter(AdoptionConfig{
		Cache:                          env.cache,
		Fetcher:                        env.fetcher,
		Verifier:                       passThroughVerifier{},
		HostLimiter:                    hostsem.New(8),
		Architectures:                  []string{"amd64"},
		TolerateOptionalMemberFailures: true,
		RepairSkippedMembers:           true,
	})
	if err != nil {
		t.Fatalf("NewAdopter: %v", err)
	}

	// 1. Adopt a complete snapshot (binary-amd64 + binary-all).
	pkgsAmd64 := fakePackagesStanzas(map[string]string{
		"pool/main/a/a/a_1_amd64.deb": strings.Repeat("a", 64),
	})
	pkgsAll := fakePackagesStanzas(map[string]string{
		"pool/main/c/c/c_1_all.deb": strings.Repeat("c", 64),
	})
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": pkgsAmd64,
		"main/binary-all/Packages":   pkgsAll,
	})
	releaseText = simtest.ClearsignedFixture(string(releaseText)) // reconcile parses only clearsigned InRelease
	base := "http://127.0.0.1/ubuntu/dists/noble/"
	env.fetcher.put(base+"main/binary-amd64/Packages", pkgsAmd64)
	env.fetcher.put(base+"main/binary-all/Packages", pkgsAll)

	trackSuite(t, env.cache, env.suite)
	if err := ad.Run(ctx, env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("adoption setup: %v", err)
	}

	sf, err := env.cache.GetSuiteFreshness(ctx,
		env.suite.CanonicalScheme, env.suite.CanonicalHost, env.suite.SuitePath)
	if err != nil {
		t.Fatalf("GetSuiteFreshness after adoption: %v", err)
	}
	if sf.CurrentSnapshotID == nil {
		t.Fatal("no current snapshot after adoption")
	}
	snapID := *sf.CurrentSnapshotID

	// 2. Degrade the snapshot: delete binary-all member rows.
	binaryAllMember, err := env.cache.GetSnapshotMember(ctx, snapID, "main/binary-all/Packages")
	if err != nil {
		t.Fatalf("setup: GetSnapshotMember binary-all: %v", err)
	}
	aliasPath := "main/binary-all/by-hash/SHA256/" + binaryAllMember.DeclaredSHA256
	if err := env.cache.DeleteSnapshotMembersForTest(ctx, snapID,
		"main/binary-all/Packages", aliasPath); err != nil {
		t.Fatalf("setup: DeleteSnapshotMembersForTest: %v", err)
	}
	// Confirm degraded.
	if _, err := env.cache.GetSnapshotMember(ctx, snapID, "main/binary-all/Packages"); err == nil {
		t.Fatal("setup: binary-all should be absent after delete")
	}

	// 3. Build an HTTP server that serves the byte-identical InRelease
	// (unchanged). The conditional GET will see "unchanged" and trigger
	// the repair/reconcile goroutine.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ubuntu/dists/noble/InRelease" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(releaseText)
	}))
	defer srv.Close()

	// Seed the url_path anchor row so checkInline finds it and uses
	// the test server. The BlobHash must match sha256(releaseText) so the
	// body-equality comparison classifies the result as "unchanged".
	inReleaseHash := hashOf(releaseText)
	if err := env.cache.PutURLPath(ctx, cache.URLPath{
		CanonicalScheme: "http",
		CanonicalHost:   "127.0.0.1",
		Path:            "/ubuntu/dists/noble/InRelease",
		BlobHash:        &inReleaseHash,
		UpstreamURL:     srv.URL + "/ubuntu/dists/noble/InRelease",
		IsMetadata:      true,
	}); err != nil {
		t.Fatalf("PutURLPath: %v", err)
	}

	// 4. Build the Checker. env.cache satisfies the Cache interface.
	checker, err := New(Config{
		Cache:       env.cache,
		Fetcher:     newTestFetcher(t),
		HostLimiter: hostsem.New(8),
		Adopter:     ad,
		Logger:      discardLogger(),
		now:         func() time.Time { return time.Unix(11000, 0) },
	})
	if err != nil {
		t.Fatalf("New checker: %v", err)
	}

	// 5. The unchanged tick should trigger reconcile which heals binary-all.
	checker.Check(ctx, "http", "127.0.0.1", "/ubuntu/dists/noble")
	checker.WaitForAdoptions()

	// 6. Assert binary-all is now present in the snapshot.
	if _, err := env.cache.GetSnapshotMember(ctx, snapID, "main/binary-all/Packages"); err != nil {
		t.Errorf("unchanged tick did not reconcile binary-all: %v", err)
	}

	// 7. current_snapshot_id must be unchanged (in-place reconcile).
	sf2, err := env.cache.GetSuiteFreshness(ctx,
		env.suite.CanonicalScheme, env.suite.CanonicalHost, env.suite.SuitePath)
	if err != nil {
		t.Fatalf("GetSuiteFreshness after reconcile: %v", err)
	}
	if sf2.CurrentSnapshotID == nil || *sf2.CurrentSnapshotID != snapID {
		t.Errorf("snapshot id changed after reconcile: want %d, got %v", snapID, sf2.CurrentSnapshotID)
	}
}

// TestCheck_FailedTick_DoesNotReconcile: auto-reconcile must run ONLY on a
// tick that positively confirmed the upstream metadata is unchanged (repair
// != nil). On a tick that did NOT confirm unchanged — an upstream failure
// here, equivalently a cooldown skip — reconcile must NOT fire, so a
// persistently-degraded suite cannot drive uncooled-down upstream fetches on
// every client metadata request.
func TestCheck_FailedTick_DoesNotReconcile(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	env.suite = SuiteRef{CanonicalScheme: "http", CanonicalHost: "127.0.0.1", SuitePath: "/ubuntu/dists/noble"}

	ad, err := NewAdopter(AdoptionConfig{
		Cache:                          env.cache,
		Fetcher:                        env.fetcher,
		Verifier:                       passThroughVerifier{},
		HostLimiter:                    hostsem.New(8),
		Architectures:                  []string{"amd64"},
		TolerateOptionalMemberFailures: true,
		RepairSkippedMembers:           true,
	})
	if err != nil {
		t.Fatalf("NewAdopter: %v", err)
	}

	pkgsAmd64 := fakePackagesStanzas(map[string]string{"pool/main/a/a/a_1_amd64.deb": strings.Repeat("a", 64)})
	pkgsAll := fakePackagesStanzas(map[string]string{"pool/main/c/c/c_1_all.deb": strings.Repeat("c", 64)})
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": pkgsAmd64,
		"main/binary-all/Packages":   pkgsAll,
	})
	// Clearsigned so a reconcile that wrongly ran would heal binary-all:
	// reconcile refuses a plain-text InRelease before healing anything,
	// which would make this negative test pass vacuously.
	releaseText = simtest.ClearsignedFixture(string(releaseText))
	base := "http://127.0.0.1/ubuntu/dists/noble/"
	env.fetcher.put(base+"main/binary-amd64/Packages", pkgsAmd64)
	env.fetcher.put(base+"main/binary-all/Packages", pkgsAll) // member fetcher CAN serve it
	trackSuite(t, env.cache, env.suite)
	if err := ad.Run(ctx, env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("adoption setup: %v", err)
	}
	sf, _ := env.cache.GetSuiteFreshness(ctx, env.suite.CanonicalScheme, env.suite.CanonicalHost, env.suite.SuitePath)
	snapID := *sf.CurrentSnapshotID

	// Degrade: delete binary-all member + alias.
	bam, err := env.cache.GetSnapshotMember(ctx, snapID, "main/binary-all/Packages")
	if err != nil {
		t.Fatalf("setup GetSnapshotMember: %v", err)
	}
	if err := env.cache.DeleteSnapshotMembersForTest(ctx, snapID,
		"main/binary-all/Packages", "main/binary-all/by-hash/SHA256/"+bam.DeclaredSHA256); err != nil {
		t.Fatalf("setup delete: %v", err)
	}

	// InRelease conditional GET FAILS (500) → result "failed" → repair == nil.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	inReleaseHash := hashOf(releaseText)
	if err := env.cache.PutURLPath(ctx, cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
		Path: "/ubuntu/dists/noble/InRelease", BlobHash: &inReleaseHash,
		UpstreamURL: srv.URL + "/ubuntu/dists/noble/InRelease", IsMetadata: true,
	}); err != nil {
		t.Fatalf("PutURLPath: %v", err)
	}

	checker, err := New(Config{
		Cache: env.cache, Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8),
		Adopter: ad, Logger: discardLogger(), now: func() time.Time { return time.Unix(11000, 0) },
	})
	if err != nil {
		t.Fatalf("New checker: %v", err)
	}

	checker.Check(ctx, "http", "127.0.0.1", "/ubuntu/dists/noble")
	checker.WaitForAdoptions()

	// binary-all must STILL be absent — the failed tick did not confirm
	// unchanged, so reconcile must not have run.
	if _, err := env.cache.GetSnapshotMember(ctx, snapID, "main/binary-all/Packages"); err == nil {
		t.Error("reconcile ran on a non-confirmed (failed) tick — must only run when repair != nil")
	}
}

// TestCheck_UnchangedTick_LogsReconcileFailure: the tick-path reconcile
// must not swallow its error. R-1 made reconcile fail closed on a stored
// InRelease that carries the clearsign marker but no decodable frame; on
// the confirmed-unchanged tick that error has to reach the operator as the
// same Warn line the on-demand path (Checker.Reconcile) emits, otherwise
// such a snapshot silently stops healing. Harness as in
// TestCheck_UnchangedTick_ReconcilesDegraded; the snapshot comes from
// storeInlineSnapshot with an undecodable envelope.
func TestCheck_UnchangedTick_LogsReconcileFailure(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	env.suite = SuiteRef{CanonicalScheme: "http", CanonicalHost: "127.0.0.1", SuitePath: "/ubuntu/dists/noble"}

	ad, err := NewAdopter(AdoptionConfig{
		Cache:                env.cache,
		Fetcher:              env.fetcher,
		Verifier:             passThroughVerifier{},
		HostLimiter:          hostsem.New(8),
		Architectures:        []string{"amd64"},
		RepairSkippedMembers: true, // gates the tick-path reconcile too
	})
	if err != nil {
		t.Fatalf("NewAdopter: %v", err)
	}

	// Current inline snapshot whose stored envelope lost its END marker:
	// it still carries a parseable SHA256 block in the raw text, but no
	// longer decodes, so reconcile must fail closed.
	signed := clearsignForTest(t, "Origin: Test\nSuite: noble\nSHA256:\n "+
		strings.Repeat("a", 64)+" 10 main/binary-amd64/Packages\n")
	anchor := append([]byte{}, signed[:bytes.LastIndex(signed, []byte("-----END PGP SIGNATURE-----"))]...)
	snapID := storeInlineSnapshot(t, env, anchor)

	// Upstream serves the byte-identical anchor → "unchanged" → repair != nil.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ubuntu/dists/noble/InRelease" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(anchor)
	}))
	defer srv.Close()
	anchorHash := hashOf(anchor)
	if err := env.cache.PutURLPath(ctx, cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
		Path: "/ubuntu/dists/noble/InRelease", BlobHash: &anchorHash,
		UpstreamURL: srv.URL + "/ubuntu/dists/noble/InRelease", IsMetadata: true,
	}); err != nil {
		t.Fatalf("PutURLPath: %v", err)
	}

	var logBuf bytes.Buffer
	checker, err := New(Config{
		Cache: env.cache, Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8),
		Adopter: ad, Logger: slog.New(slog.NewJSONHandler(&logBuf, nil)),
		now: func() time.Time { return time.Unix(11000, 0) },
	})
	if err != nil {
		t.Fatalf("New checker: %v", err)
	}

	checker.Check(ctx, "http", "127.0.0.1", "/ubuntu/dists/noble")
	checker.WaitForAdoptions() // the reconcile goroutine has logged by now

	var found bool
	for _, line := range bytes.Split(logBuf.Bytes(), []byte("\n")) {
		var rec map[string]any
		if json.Unmarshal(line, &rec) != nil || rec["msg"] != "adoption_reconcile_failed" {
			continue
		}
		found = true
		errText, _ := rec["err"].(string)
		if rec["level"] != "WARN" || rec["stage"] != "decision" || rec["canonical_host"] != "127.0.0.1" ||
			rec["suite_path"] != "/ubuntu/dists/noble" || rec["snapshot_id"] != float64(snapID) ||
			!strings.Contains(errText, "not a valid clearsigned message") {
			t.Errorf("adoption_reconcile_failed record = %v; want WARN with host, suite path, snapshot_id %d and the decode error", rec, snapID)
		}
	}
	if !found {
		t.Fatalf("no adoption_reconcile_failed line on the tick path; log:\n%s", logBuf.String())
	}
}

// newCheckerForReconcile builds a Checker backed by an empty fakeCache (no
// known suites). Used by TestCheckerReconcile_UnknownSuiteAllocatesNoLock to
// verify that Reconcile returns false and does NOT allocate a per-suite lock
// entry for a forged/unknown (host, suite) — the codex lock-growth mitigation.
func newCheckerForReconcile(t *testing.T) *Checker {
	t.Helper()
	c, err := New(Config{
		Cache:       newFakeCache(),
		Fetcher:     newTestFetcher(t),
		HostLimiter: hostsem.New(8),
		Logger:      discardLogger(),
		now:         func() time.Time { return time.Unix(11000, 0) },
	})
	if err != nil {
		t.Fatalf("newCheckerForReconcile: %v", err)
	}
	return c
}

// TestCheckerReconcile_UnknownSuiteAllocatesNoLock verifies the critical
// SPEC6_8 lock-growth mitigation: Reconcile validates that the suite is known
// (has a current snapshot) BEFORE calling c.locks.LoadOrStore. A forged or
// unknown (host, suite) must return false without allocating a per-suite mutex
// entry — otherwise an unauthenticated caller could grow the lock map without
// bound.
func TestCheckerReconcile_UnknownSuiteAllocatesNoLock(t *testing.T) {
	c := newCheckerForReconcile(t) // real Cache interface (fakeCache), no suites
	if c.Reconcile(context.Background(), "https", "nope.example", "/dists/x") {
		t.Error("Reconcile on unknown suite returned true")
	}
	// The per-suite lock map must not have grown — LoadOrStore was never called.
	count := 0
	c.locks.Range(func(_, _ any) bool { count++; return true })
	if count != 0 {
		t.Errorf("lock map grew to %d for an unknown suite", count)
	}
}

// I-4 (spec C3): a suite without a row is not checked — no upstream
// request, no row created, no lock-map entry — however its T1 fires.
func TestCheck_UntrackedSuiteIsNotChecked(t *testing.T) {
	fc := newFakeCache()
	bh := hashOf([]byte("x"))
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()
	fc.putURL(cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1", Path: "/dists/minted/InRelease",
		BlobHash: &bh, UpstreamURL: srv.URL + "/dists/minted/InRelease", IsMetadata: true,
	})
	c, err := New(Config{Cache: fc, Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8), Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if rep := c.check(context.Background(), SuiteRef{CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
		SuitePath: "/dists/minted"}, checkOpts{bypassCooldown: true}); rep.result != resultSkipped {
		t.Fatalf("result = %q, want %q", rep.result, resultSkipped)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("upstream requests = %d, want 0", n)
	}
	if _, ok := fc.suite("http", "127.0.0.1", "/dists/minted"); ok {
		t.Fatal("the check created a suite row")
	}
	locks := 0
	c.locks.Range(func(any, any) bool { locks++; return true })
	if locks != 0 {
		t.Fatalf("lock map holds %d entries, want 0", locks)
	}
}
