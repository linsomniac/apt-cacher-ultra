package handler

// Request-time revalidation (freshness.revalidate_wait; the 2026-11-14
// publication-delay review): the cache asks upstream before it serves a
// suite's anchor, and waits — within a bound — for the answer and for the
// adoption of a new release. Real handler, checker, adopter and cache
// against an httptest upstream (the phase 3 chaos fixtures).

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"
	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
)

// revalStack is a handler wired to a real checker and adopter, with
// request-time revalidation set to wait (0: off), an optional flip
// barrier, and the handler's request lines captured.
type revalStack struct {
	*phase3ChaosStack
	srv      *httptest.Server
	current  *atomic.Pointer[chaos3Snapshot]
	stalls   *chaos3StallSet
	suite    freshness.SuiteRef
	requests *chaos3LogBuf
	a, b     chaos3Snapshot
}

func newRevalStack(t *testing.T, wait, quiet time.Duration) *revalStack {
	t.Helper()
	specs := chaos3DebSpecs()
	s := &revalStack{a: makeChaos3Snapshot("A", specs), b: makeChaos3Snapshot("B", specs),
		current: &atomic.Pointer[chaos3Snapshot]{}, stalls: newChaos3StallSet(), requests: &chaos3LogBuf{}}
	s.current.Store(&s.a)
	s.srv = chaos3Server(t, s.current, s.stalls)
	upstream, err := url.Parse(s.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	s.suite = chaos3SuiteRef(upstream)

	parser, err := proxy.New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := cache.Open(context.Background(), t.TempDir(), silentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	fc, err := fetch.New(fetch.Options{
		ConnectTimeout: 2 * time.Second, TotalTimeout: 30 * time.Second,
		AllowedHostRegex: []string{`^127\.0\.0\.1$`, `^::1$`}, Logger: silentLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	limiter := hostsem.New(8)
	var gate *flipgate.Gate
	if quiet > 0 {
		if gate, err = flipgate.New(flipgate.Config{Quiet: quiet}); err != nil {
			t.Fatal(err)
		}
	}
	logBuf := &chaos3LogBuf{}
	adopter, err := freshness.NewAdopter(freshness.AdoptionConfig{
		Cache: c, Fetcher: &chaos2RewritingFetcher{upstream: upstream, inner: fc}, Verifier: chaos2PassVerifier{},
		HostLimiter: limiter, FlipGate: gate,
		Logger: slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	life, stop := context.WithCancel(context.Background())
	checker, err := freshness.New(freshness.Config{
		Cache: c, Fetcher: fc, HostLimiter: limiter, Refresh: 10 * time.Minute,
		Logger: silentLogger(), Adopter: adopter, LifetimeCtx: life,
	})
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{
		Parser: parser, Cache: c, Fetch: fc, HostLimiter: limiter,
		Logger:    slog.New(slog.NewJSONHandler(s.requests, nil)),
		Freshness: checker, Revalidator: checker, RevalidateWait: wait, FlipGate: gate,
	})
	if err != nil {
		t.Fatal(err)
	}
	s.phase3ChaosStack = &phase3ChaosStack{handler: h, checker: checker, adopter: adopter, logBuf: logBuf}
	t.Cleanup(func() {
		s.stalls.UnstallAll()
		stop()
		checker.WaitForAdoptions()
	})
	chaos3PrimeAndAdoptA(t, s.phase3ChaosStack, s.srv, &s.a, s.suite)
	return s
}

// get asks the cache for path and returns the response and how long it took.
func (s *revalStack) get(path string) (*httptest.ResponseRecorder, time.Duration) {
	rec := httptest.NewRecorder()
	start := time.Now()
	s.handler.ServeHTTP(rec, proxyReq("GET", s.srv.URL, path))
	return rec, time.Since(start)
}

// lastRequest returns the newest request line for path.
func (s *revalStack) lastRequest(t *testing.T, path string) map[string]any {
	t.Helper()
	recs := s.requests.findAll("request")
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i]["path"] == path {
			return recs[i]
		}
	}
	t.Fatalf("no request line for %s", path)
	return nil
}

const revalAnchor = chaos3Suite + "/InRelease"

// The case the cache used to lose every time: upstream has a new release,
// and the very first request for the suite's InRelease gets it — not the
// previous one, followed by an adoption nobody waited for.
func TestRevalidate_FirstRequestAfterAnUpstreamChangeGetsTheNewRelease(t *testing.T) {
	s := newRevalStack(t, 10*time.Second, 0)
	s.current.Store(&s.b)

	rec, _ := s.get(revalAnchor)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), s.b.inRelease) {
		t.Fatalf("the first request after the publication got the previous release's InRelease")
	}
	// What follows is the new release too: the snapshot switched.
	pkgs, _ := s.get(chaos3Suite + "/main/binary-amd64/Packages.gz")
	if !bytes.Equal(pkgs.Body.Bytes(), s.b.packagesGz) {
		t.Errorf("the index after the new InRelease is not the new release's")
	}
	line := s.lastRequest(t, revalAnchor)
	if line["reval"] != "published" || line["reval_check"] != "changed" {
		t.Errorf("request line reval=%v reval_check=%v, want published/changed: %v", line["reval"], line["reval_check"], line)
	}
	if _, ok := line["reval_wait_ms"]; !ok {
		t.Errorf("request line has no reval_wait_ms: %v", line)
	}
}

// With nothing new upstream the request costs one conditional request and
// is served the snapshot, which the line says is current.
func TestRevalidate_UnchangedUpstreamServesTheSnapshot(t *testing.T) {
	s := newRevalStack(t, 10*time.Second, 0)
	rec, _ := s.get(revalAnchor)
	if !bytes.Equal(rec.Body.Bytes(), s.a.inRelease) {
		t.Fatalf("body is not the adopted InRelease")
	}
	line := s.lastRequest(t, revalAnchor)
	if line["reval"] != "current" || line["reval_check"] != "not_modified" {
		t.Errorf("request line reval=%v reval_check=%v, want current/not_modified", line["reval"], line["reval_check"])
	}
	if id, _ := line["snapshot_id"].(float64); id <= 0 {
		t.Errorf("request line snapshot_id = %v, want the snapshot served", line["snapshot_id"])
	}
	if _, ok := line["checked_ago_s"]; !ok {
		t.Errorf("request line has no checked_ago_s: %v", line)
	}
	if _, ok := line["behind_s"]; ok {
		t.Errorf("request line has behind_s although nothing newer is known: %v", line)
	}
}

// An upstream that does not answer never holds a client past the wait:
// the current, verified snapshot is served, and the line says the cache
// could not ask in time.
func TestRevalidate_SilentUpstreamDoesNotHoldTheRequest(t *testing.T) {
	s := newRevalStack(t, 200*time.Millisecond, 0)
	s.stalls.Stall(revalAnchor)

	rec, took := s.get(revalAnchor)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), s.a.inRelease) {
		t.Fatalf("status %d: the adopted InRelease was not served while upstream was silent", rec.Code)
	}
	if took < 150*time.Millisecond || took > 5*time.Second {
		t.Fatalf("the request took %s, want about the 200ms wait", took)
	}
	line := s.lastRequest(t, revalAnchor)
	if line["reval"] != "unknown" {
		t.Errorf("request line reval=%v, want unknown", line["reval"])
	}
}

// A new release whose adoption outlasts the wait: the request is served
// the previous release at the bound, the line says a newer one is pending,
// and the adoption goes on and publishes by itself.
func TestRevalidate_SlowAdoptionServesThePreviousRelease(t *testing.T) {
	s := newRevalStack(t, 200*time.Millisecond, 0)
	prior := currentSnapshot(t, s)
	s.current.Store(&s.b)
	s.stalls.Stall(chaos3Suite + "/main/binary-amd64/Packages.gz")

	rec, took := s.get(revalAnchor)
	if !bytes.Equal(rec.Body.Bytes(), s.a.inRelease) {
		t.Fatalf("the request was not served the previous release while the new one was still being adopted")
	}
	if took < 150*time.Millisecond || took > 5*time.Second {
		t.Fatalf("the request took %s, want about the 200ms wait", took)
	}
	line := s.lastRequest(t, revalAnchor)
	if line["reval"] != "pending" || line["reval_check"] != "changed" {
		t.Errorf("request line reval=%v reval_check=%v, want pending/changed", line["reval"], line["reval_check"])
	}
	if _, ok := line["behind_s"]; !ok {
		t.Errorf("request line has no behind_s although a newer release is known: %v", line)
	}
	s.stalls.UnstallAll()
	s.checker.WaitForAdoptions()
	if now := currentSnapshot(t, s); now == prior {
		t.Fatal("the adoption did not publish once upstream answered")
	}
}

// Only a suite's anchor waits: its indexes and packages are served as
// before, whatever upstream is doing.
func TestRevalidate_OnlyAnchorsWait(t *testing.T) {
	s := newRevalStack(t, 5*time.Second, 0)
	s.stalls.Stall(revalAnchor)
	for _, p := range []string{chaos3Suite + "/main/binary-amd64/Packages.gz", debCanonicalPath(s.a.debSpecs[0])} {
		rec, took := s.get(p)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d", p, rec.Code)
		}
		if took > time.Second {
			t.Errorf("%s took %s: only InRelease and Release wait for upstream", p, took)
		}
		if line := s.lastRequest(t, p); line["reval"] != nil {
			t.Errorf("%s: request line has reval=%v", p, line["reval"])
		}
	}
}

// Off (0s) is the behaviour before: the anchor is served at once from the
// current snapshot, and the check runs after the response.
func TestRevalidate_OffServesFirstAndChecksAfter(t *testing.T) {
	s := newRevalStack(t, 0, 0)
	prior := currentSnapshot(t, s)
	s.current.Store(&s.b)
	rec, _ := s.get(revalAnchor)
	if !bytes.Equal(rec.Body.Bytes(), s.a.inRelease) {
		t.Fatalf("with revalidation off the first request should get the current snapshot")
	}
	if line := s.lastRequest(t, revalAnchor); line["reval"] != nil {
		t.Errorf("request line has reval=%v with revalidation off", line["reval"])
	}
	deadline := time.Now().Add(10 * time.Second)
	for currentSnapshot(t, s) == prior {
		if time.Now().After(deadline) {
			t.Fatal("the check after the response did not adopt the new release")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// With the flip barrier on, a request that waits for an adoption does not
// count as a serve of the old anchor, so it does not hold the switch back:
// the new release is published within the quiet period the earlier serves
// need, and the waiting request gets it.
func TestRevalidate_WaitingRequestDoesNotHoldTheFlipBarrier(t *testing.T) {
	s := newRevalStack(t, 10*time.Second, time.Second)
	time.Sleep(1100 * time.Millisecond) // the priming serves are a quiet period old
	s.current.Store(&s.b)

	rec, took := s.get(revalAnchor)
	if !bytes.Equal(rec.Body.Bytes(), s.b.inRelease) {
		t.Fatalf("the waiting request got the previous release (after %s)", took)
	}
	if took > 5*time.Second {
		t.Errorf("the request took %s: it held its own suite's switch back", took)
	}
}

func currentSnapshot(t *testing.T, s *revalStack) int64 {
	t.Helper()
	sf, err := s.handler.cache.GetSuiteFreshness(context.Background(), s.suite.CanonicalScheme, s.suite.CanonicalHost, s.suite.SuitePath)
	if err != nil || sf.CurrentSnapshotID == nil {
		t.Fatalf("no current snapshot (err %v)", err)
	}
	return *sf.CurrentSnapshotID
}

// With adoption disabled the same check runs before the response: a
// changed anchor drops the suite's cached metadata, so the request that
// found the change is itself served the new file, fetched from upstream.
func TestRevalidate_AdoptionDisabledFirstRequestGetsTheNewRelease(t *testing.T) {
	specs := chaos3DebSpecs()
	a, b := makeChaos3Snapshot("A", specs), makeChaos3Snapshot("B", specs)
	current := &atomic.Pointer[chaos3Snapshot]{}
	current.Store(&a)
	stalls := newChaos3StallSet()
	srv := chaos3Server(t, current, stalls)

	parser, err := proxy.New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := cache.Open(context.Background(), t.TempDir(), silentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	fc, err := fetch.New(fetch.Options{
		ConnectTimeout: 2 * time.Second, TotalTimeout: 30 * time.Second,
		AllowedHostRegex: []string{`^127\.0\.0\.1$`, `^::1$`}, Logger: silentLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	limiter := hostsem.New(8)
	checker, err := freshness.New(freshness.Config{
		Cache: c, Fetcher: fc, HostLimiter: limiter, Refresh: 10 * time.Minute, Logger: silentLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{
		Parser: parser, Cache: c, Fetch: fc, HostLimiter: limiter, Logger: silentLogger(),
		Freshness: checker, Revalidator: checker, RevalidateWait: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, proxyReq("GET", srv.URL, path))
		return rec
	}
	for _, p := range []string{revalAnchor, chaos3Suite + "/main/binary-amd64/Packages.gz"} {
		if rec := get(p); rec.Code != http.StatusOK {
			t.Fatalf("prime %s: status %d", p, rec.Code)
		}
	}
	if rec := get(revalAnchor); !bytes.Equal(rec.Body.Bytes(), a.inRelease) {
		t.Fatalf("setup: the cached InRelease is not served")
	}

	current.Store(&b)
	rec := get(revalAnchor)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), b.inRelease) {
		t.Fatalf("status %d: the first request after the publication got the previous InRelease", rec.Code)
	}
	if rec := get(chaos3Suite + "/main/binary-amd64/Packages.gz"); !bytes.Equal(rec.Body.Bytes(), b.packagesGz) {
		t.Errorf("the index after the new InRelease is not the new release's")
	}
}
