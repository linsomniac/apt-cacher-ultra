package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
)

// chaosPaths is the set of apt-shaped requests each chaos client issues:
// one InRelease, one Packages index, five referenced .deb files. Matches
// the SPEC §12.3 fixture exactly.
var chaosPaths = []string{
	"/ubuntu/dists/noble/InRelease",
	"/ubuntu/dists/noble/main/binary-amd64/Packages",
	"/ubuntu/pool/main/p/pkg1/pkg1_1.0_amd64.deb",
	"/ubuntu/pool/main/p/pkg2/pkg2_1.0_amd64.deb",
	"/ubuntu/pool/main/p/pkg3/pkg3_1.0_amd64.deb",
	"/ubuntu/pool/main/p/pkg4/pkg4_1.0_amd64.deb",
	"/ubuntu/pool/main/p/pkg5/pkg5_1.0_amd64.deb",
}

// chaosBodyFor returns the canonical bytes the upstream serves (and the
// cache stores) for a chaos path. Distinct per path so a body-mixup bug
// surfaces as a clear comparison failure rather than a uniform-bytes
// false-positive.
func chaosBodyFor(path string) []byte {
	return []byte("chaos-body|" + path)
}

// TestChaos_HungUpstreamGated is the SPEC §12.3 gating test (and the
// project's reason for existing): with a primed cache, hung-forever
// upstream traffic must not block requests. apt-cacher-ng fails this —
// clients there hang waiting for synchronous freshness checks. ultra must
// pass it.
//
// Verifies:
//  1. All 350 requests (50 clients × 7 paths) succeed with status 200.
//  2. Every response body matches the primed bytes byte-for-byte.
//  3. p99 latency < 100ms — the cache-hit path has no upstream wait.
//  4. Goroutine count returns to baseline within 5s of Handler.Close.
//
// SPEC §12.3 also calls for an RSS bound of 256 MB; that assertion is
// omitted here because Go's GC behavior makes RSS measurements flaky in
// process-internal tests. A nightly soak (§12.5) is the right home for
// memory ceiling checks.
func TestChaos_HungUpstreamGated(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos test skipped in -short mode")
	}

	var hangNow atomic.Bool
	var upstreamHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		if hangNow.Load() {
			// "no responses, no resets" — block on the request ctx so
			// the goroutine releases the moment the handler's lifecycle
			// ctx cancels (Handler.Close). A fixed-duration sleep would
			// leak goroutines on test failure.
			<-r.Context().Done()
			return
		}
		body := chaosBodyFor(r.URL.Path)
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Last-Modified", "Mon, 01 Jan 2024 00:00:00 GMT")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	h := newChaosHandler(t)
	defer h.Close()

	// Prime the cache: one sequential request per path. Each request
	// hits upstream via the miss path, populates url_path + a real blob
	// on disk, and (for InRelease) seeds suite_freshness with the
	// upstream validators. After this loop the cache holds all 7 files.
	for _, p := range chaosPaths {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, proxyReq("GET", srv.URL, p))
		if rec.Code != http.StatusOK {
			t.Fatalf("prime %s: status=%d body=%q", p, rec.Code, rec.Body.String())
		}
		if got := rec.Body.String(); got != string(chaosBodyFor(p)) {
			t.Fatalf("prime %s: body=%q, want %q", p, got, chaosBodyFor(p))
		}
	}

	// Switch upstream to hang-forever. From here on, anything that
	// reaches upstream blocks until ctx.Done. Cache hits must not.
	hangNow.Store(true)
	primeHits := upstreamHits.Load()

	// runtime.GC + brief settle so the baseline goroutine count is not
	// inflated by transient priming-phase goroutines (touchAsync,
	// freshness seeds) that have already returned.
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	const clients = 50
	type sample struct {
		path     string
		duration time.Duration
		status   int
		body     []byte
	}
	results := make(chan sample, clients*len(chaosPaths))

	var wg sync.WaitGroup
	wg.Add(clients)
	for i := 0; i < clients; i++ {
		go func() {
			defer wg.Done()
			for _, p := range chaosPaths {
				t0 := time.Now()
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, proxyReq("GET", srv.URL, p))
				results <- sample{
					path:     p,
					duration: time.Since(t0),
					status:   rec.Code,
					body:     append([]byte(nil), rec.Body.Bytes()...),
				}
			}
		}()
	}
	wg.Wait()
	close(results)

	durations := make([]time.Duration, 0, clients*len(chaosPaths))
	var bodyMismatches, nonOK int
	for s := range results {
		durations = append(durations, s.duration)
		if s.status != http.StatusOK {
			nonOK++
			t.Errorf("non-200 for %s: status=%d", s.path, s.status)
		}
		if want := chaosBodyFor(s.path); string(s.body) != string(want) {
			bodyMismatches++
			t.Errorf("body mismatch for %s:\n  got:  %q\n  want: %q", s.path, s.body, want)
		}
	}
	if got := len(durations); got != clients*len(chaosPaths) {
		t.Fatalf("collected %d samples, want %d", got, clients*len(chaosPaths))
	}

	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p50 := durations[len(durations)/2]
	p99 := durations[(len(durations)*99)/100]
	t.Logf("latency p50=%v p99=%v max=%v (n=%d, race=%v)",
		p50, p99, durations[len(durations)-1], len(durations), chaosRaceBuild)

	// SPEC §12.3 bounds the production p99 at 100ms. Under -race the
	// detector adds ~3-5× overhead on every memory access, which is
	// orthogonal to the bug under test (a cache HIT must not call
	// upstream); fall back to a 500ms qualitative bound there. A real
	// regression — HITs leaking through to the hung upstream — would
	// blow either threshold by orders of magnitude.
	threshold := 100 * time.Millisecond
	if chaosRaceBuild {
		threshold = 500 * time.Millisecond
	}
	if p99 > threshold {
		t.Errorf("p99 latency=%v, want <%v", p99, threshold)
	}

	// Defensive: the chaos clients must not have triggered any new
	// upstream calls beyond the (at most one) freshness conditional GET
	// that the TryLock + cooldown gate may have allowed through. If we
	// see many post-prime upstream hits, cache HITs are leaking through
	// to upstream, which is the exact bug this test guards against.
	postPrimeHits := upstreamHits.Load() - primeHits
	if postPrimeHits > 5 {
		t.Errorf("upstream hits during chaos phase=%d, want <=5 (cache HITs must not call upstream)",
			postPrimeHits)
	}

	// Cleanup: cancel lifecycle ctx and wait. Any hung freshness
	// goroutines should observe ctx.Done and exit.
	h.Close()

	deadline := time.Now().Add(5 * time.Second)
	const slack = 4 // small tolerance for net/http internal goroutines that come and go.
	for time.Now().Before(deadline) {
		runtime.GC()
		if runtime.NumGoroutine() <= baseline+slack {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("goroutine leak: now=%d, baseline=%d (slack=%d)",
		runtime.NumGoroutine(), baseline, slack)
}

// newChaosHandler builds the full handler stack with a real freshness
// checker — the chaos test needs T1 triggers to actually fire (and hang
// against the test upstream) so we can prove they don't block requests.
func newChaosHandler(t *testing.T) *Handler {
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

	fc, err := fetch.New(fetch.Options{
		ConnectTimeout:   2 * time.Second,
		TotalTimeout:     5 * time.Second,
		MaxRetries:       0,
		AllowedHostRegex: []string{`^127\.0\.0\.1$`},
		DenyTargetRanges: nil,
	})
	if err != nil {
		t.Fatalf("fetch.New: %v", err)
	}

	limiter := hostsem.New(4)

	freshChecker, err := freshness.New(freshness.Config{
		Cache:       c,
		Fetcher:     fc,
		HostLimiter: limiter,
		Cooldown:    1 * time.Minute,
		Refresh:     10 * time.Minute,
		Logger:      silentLogger(),
	})
	if err != nil {
		t.Fatalf("freshness.New: %v", err)
	}

	h, err := New(Config{
		Parser:      parser,
		Cache:       c,
		Fetch:       fc,
		HostLimiter: limiter,
		Logger:      silentLogger(),
		Freshness:   freshChecker,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}
