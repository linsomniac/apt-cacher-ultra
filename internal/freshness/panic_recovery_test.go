package freshness

import (
	"bytes"
	"context"
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

type panickingVerifier struct{}

func (panickingVerifier) VerifyInline(context.Context, SuiteRef, []byte) ([]byte, error) {
	panic("clearsign.Decode: zero-plaintext frame")
}

func (panickingVerifier) VerifyDetached(context.Context, SuiteRef, []byte, []byte) ([]byte, error) {
	panic("unexpected")
}

type panickingAdoptionFetcher struct{}

func (panickingAdoptionFetcher) Fetch(context.Context, *fetch.Target, fetch.FetchDst) (*fetch.FetchResult, error) {
	panic("member fetch bug")
}

// changedUpstream is a freshness Fetcher that always answers 200 with
// body, counting calls; with panic set it panics instead.
type changedUpstream struct {
	body  []byte
	calls atomic.Int32
	panic atomic.Bool
}

func (u *changedUpstream) Conditional(context.Context, *fetch.Target, string, string, int64) (*fetch.ConditionalResult, error) {
	u.calls.Add(1)
	if u.panic.Load() {
		panic("fetcher bug")
	}
	return &fetch.ConditionalResult{Status: 200, Body: u.body}, nil
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// seedStaleAnchor gives suite /r/dists/x on host h an InRelease anchor
// whose hash differs from anything upstream serves, so a check sees
// "changed".
func seedStaleAnchor(t *testing.T, c *cache.Cache) {
	t.Helper()
	ctx := context.Background()
	stale := testHex64
	if err := c.PutBlob(ctx, stale, 1); err != nil {
		t.Fatal(err)
	}
	if err := c.PutURLPath(ctx, cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "h", Path: "/r/dists/x/InRelease",
		BlobHash: &stale, UpstreamURL: "http://h/r/dists/x/InRelease", IsMetadata: true,
	}); err != nil {
		t.Fatal(err)
	}
}

// I-1: a panic inside the adoption goroutine must not kill the process,
// must be logged, and must release the suite mutex.
func TestCheck_AdoptionPanicIsRecovered(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ctx := context.Background()
	seedStaleAnchor(t, env.cache)
	ad, err := NewAdopter(AdoptionConfig{
		Cache: env.cache, Fetcher: env.fetcher, Verifier: panickingVerifier{}, HostLimiter: hostsem.New(8),
	})
	if err != nil {
		t.Fatal(err)
	}
	up := &changedUpstream{body: []byte("-----BEGIN PGP SIGNED MESSAGE-----\n")}
	logs := &lockedBuffer{}
	c, err := New(Config{Cache: env.cache, Fetcher: up, HostLimiter: hostsem.New(8), Adopter: ad,
		Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	c.Check(ctx, "http", "h", "/r/dists/x")
	c.WaitForAdoptions()
	if !strings.Contains(logs.String(), "adoption_panicked") {
		t.Fatalf("no adoption_panicked log; logs:\n%s", logs.String())
	}
	// Cooldown is 0, so a second Check proceeds only if the mutex was released.
	c.Check(ctx, "http", "h", "/r/dists/x")
	c.WaitForAdoptions()
	if got := up.calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (suite mutex leaked?)", got)
	}
}

// mutexProbeHandler records, when adoption_panicked is logged, whether
// the suite mutex is still held. Phase B records the failure at that
// point, which spec C5 requires to happen before the mutex is released.
type mutexProbeHandler struct {
	slog.Handler
	c       *Checker
	key     string
	logged  atomic.Bool
	heldNow atomic.Bool
}

func (p *mutexProbeHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message != "adoption_panicked" {
		return nil
	}
	p.logged.Store(true)
	if v, ok := p.c.locks.Load(p.key); ok {
		mu := v.(*sync.Mutex)
		if mu.TryLock() {
			mu.Unlock()
		} else {
			p.heldNow.Store(true)
		}
	}
	return nil
}

func (p *mutexProbeHandler) Enabled(context.Context, slog.Level) bool { return true }
func (p *mutexProbeHandler) WithAttrs([]slog.Attr) slog.Handler       { return p }
func (p *mutexProbeHandler) WithGroup(string) slog.Handler            { return p }

// The recover must be the last-registered defer: it runs while the suite
// mutex is still held, and WaitForAdoptions returns only after both.
func TestCheck_AdoptionPanicRecoveredBeforeMutexRelease(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ctx := context.Background()
	seedStaleAnchor(t, env.cache)
	ad, err := NewAdopter(AdoptionConfig{Cache: env.cache, Fetcher: env.fetcher, Verifier: panickingVerifier{}, HostLimiter: hostsem.New(8)})
	if err != nil {
		t.Fatal(err)
	}
	up := &changedUpstream{body: []byte("-----BEGIN PGP SIGNED MESSAGE-----\n")}
	ph := &mutexProbeHandler{key: suiteKey("http", "h", "/r/dists/x")}
	c, err := New(Config{Cache: env.cache, Fetcher: up, HostLimiter: hostsem.New(8), Adopter: ad, Logger: slog.New(ph)})
	if err != nil {
		t.Fatal(err)
	}
	ph.c = c
	c.Check(ctx, "http", "h", "/r/dists/x")
	c.WaitForAdoptions()
	if !ph.logged.Load() {
		t.Fatal("adoption_panicked not logged before WaitForAdoptions returned")
	}
	if !ph.heldNow.Load() {
		t.Fatal("adoption_panicked handled after the suite mutex was released")
	}
	v, _ := c.locks.Load(ph.key)
	if mu := v.(*sync.Mutex); !mu.TryLock() {
		t.Fatal("suite mutex still held after WaitForAdoptions returned")
	} else {
		mu.Unlock()
	}
}

// I-1: a panic in the repair goroutine is recovered and logged, and the
// suite mutex is released. Setup follows
// TestChecker_FreshTickRepairsSkippedMembers (unchanged upstream, one
// repairable skipped member).
func TestCheck_RepairPanicIsRecovered(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	env.suite = SuiteRef{CanonicalScheme: "http", CanonicalHost: "127.0.0.1", SuitePath: "/dists/noble"}
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat("a", 64)})
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": pkgs,
		"Contents-amd64.gz":          []byte(strings.Repeat("C", 100)),
	})
	base := "http://127.0.0.1/dists/noble/"
	env.fetcher.put(base+"main/binary-amd64/Packages", pkgs)
	env.fetcher.put(base+"Contents-amd64.gz", []byte(strings.Repeat("c", 90))) // stale: skipped at adoption
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/dists/noble/InRelease" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(releaseText)
	}))
	defer srv.Close()
	if err := env.cache.PutURLPath(ctx, cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1", Path: "/dists/noble/InRelease",
		UpstreamURL: srv.URL + "/dists/noble/InRelease", IsMetadata: true,
	}); err != nil {
		t.Fatal(err)
	}
	good, err := NewAdopter(AdoptionConfig{Cache: env.cache, Fetcher: env.fetcher, Verifier: passThroughVerifier{},
		HostLimiter: hostsem.New(8), TolerateOptionalMemberFailures: true, RepairSkippedMembers: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := good.Run(ctx, env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("adoption: %v", err)
	}
	if rows, _ := env.cache.ListRepairableSkippedMembers(ctx, currentSnapshotID(t, env)); len(rows) != 1 {
		t.Fatalf("fixture: want 1 repairable row, got %d", len(rows))
	}
	panicky, err := NewAdopter(AdoptionConfig{Cache: env.cache, Fetcher: panickingAdoptionFetcher{}, Verifier: passThroughVerifier{},
		HostLimiter: hostsem.New(8), TolerateOptionalMemberFailures: true, RepairSkippedMembers: true})
	if err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuffer{}
	checker, err := New(Config{Cache: env.cache, Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8),
		Adopter: panicky, Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	checker.Check(ctx, "http", "127.0.0.1", "/dists/noble")
	checker.WaitForAdoptions()
	if !strings.Contains(logs.String(), "adoption_panicked") || !strings.Contains(logs.String(), "where=repair") {
		t.Fatalf("no repair panic logged:\n%s", logs.String())
	}
	v, ok := checker.locks.Load(suiteKey("http", "127.0.0.1", "/dists/noble"))
	if !ok {
		t.Fatal("no suite mutex allocated")
	}
	if mu := v.(*sync.Mutex); !mu.TryLock() {
		t.Fatal("suite mutex leaked by the panicking repair goroutine")
	}
}

// recoverGoroutine is the recover used by the repair and on-demand
// reconcile goroutines (the adoption goroutine uses recoverAdoption).
func TestRecoverGoroutine_LogsAndSwallows(t *testing.T) {
	logs := &lockedBuffer{}
	c, err := New(Config{Cache: newFakeCache(), Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8),
		Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer c.recoverGoroutine("reconcile", SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"})
		panic("boom")
	}()
	<-done
	out := logs.String()
	for _, want := range []string{"adoption_panicked", "where=reconcile", "panic=boom", "suite_path=/r/dists/x"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}

// A panic in the synchronous part of Check releases the mutex too.
func TestCheck_SynchronousPanicReleasesMutex(t *testing.T) {
	fc := newFakeCache()
	stale := testHex64
	fc.putURL(cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: "/r/dists/x/InRelease",
		BlobHash: &stale, UpstreamURL: "http://h/r/dists/x/InRelease", IsMetadata: true})
	up := &changedUpstream{body: []byte("x")}
	up.panic.Store(true)
	logs := &lockedBuffer{}
	c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8),
		Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	c.Check(context.Background(), "http", "h", "/r/dists/x")
	up.panic.Store(false)
	c.Check(context.Background(), "http", "h", "/r/dists/x")
	if got := up.calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}
	if !strings.Contains(logs.String(), "freshness_check_panicked") {
		t.Fatalf("no freshness_check_panicked log:\n%s", logs.String())
	}
}

// Spec C4: a recovered panic counts as a failed check, so it bumps
// last_check_at and the cooldown gates the next trigger (otherwise every
// client metadata request would re-run it: one upstream GET and one
// ERROR line each).
func TestCheck_PanickingCheckIsCooldownGated(t *testing.T) {
	fc := newFakeCache()
	stale := testHex64
	fc.putURL(cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: "/r/dists/x/InRelease",
		BlobHash: &stale, UpstreamURL: "http://h/r/dists/x/InRelease", IsMetadata: true})
	up := &changedUpstream{body: []byte("x")}
	up.panic.Store(true)
	now := time.Unix(1_800_000_000, 0)
	c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8), Cooldown: time.Minute,
		Logger: discardLogger(), now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		c.Check(context.Background(), "http", "h", "/r/dists/x")
	}
	if got := up.calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (a panic must bump last_check_at)", got)
	}
	got, ok := fc.suite("http", "h", "/r/dists/x")
	if !ok || got.LastCheckAt == nil || *got.LastCheckAt != now.Unix() || got.LastSuccessAt != nil {
		t.Fatalf("suite after panic = %+v, want last_check_at=now and no last_success_at", got)
	}
}

// Spec C5: a deterministic panic logs its stack once per goroutine kind
// per process; later occurrences log one line without the stack.
func TestReportPanic_LogsStackOncePerKind(t *testing.T) {
	logs := &lockedBuffer{}
	c, err := New(Config{Cache: newFakeCache(), Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8),
		Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	s := SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}
	const kind = "test-kind-stack-once"
	// panicStackLogged is process-global: forget this kind so the test
	// also holds under -count=N.
	panicStackLogged.Delete(kind)
	c.reportPanic("adoption_panicked", kind, s, "first")
	c.reportPanic("adoption_panicked", kind, s, "second")
	if n := strings.Count(logs.String(), "stack="); n != 1 {
		t.Fatalf("stack attributes = %d, want 1:\n%s", n, logs.String())
	}
	if n := strings.Count(logs.String(), "adoption_panicked"); n != 2 {
		t.Fatalf("adoption_panicked lines = %d, want 2", n)
	}
}
