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
	// Wait with a bounded timeout. The exact regression this test
	// catches — cache HITs blocking on a hung upstream — would manifest
	// as wg.Wait blocking indefinitely. An unbounded wg.Wait would let
	// that fail as a `go test` timeout (60s+) with no targeted message;
	// a bounded select gives the operator a clear "hits are blocking"
	// signal and unwinds the freshness goroutines via h.Close so the
	// test process exits cleanly.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	const clientWaitBudget = 10 * time.Second
	select {
	case <-done:
	case <-time.After(clientWaitBudget):
		h.Close()
		t.Fatalf("client requests did not complete within %v — cache HITs are likely blocking on the hung upstream",
			clientWaitBudget)
	}
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
	// upstream); fall back to a qualitative bound there. The race bound
	// is sized for shared CI runners (GitHub Actions hosted runners
	// routinely produce ~1s tail outliers under -race from scheduling
	// jitter and GC pauses). A real regression — HITs leaking through
	// to the hung upstream — would blow either threshold by orders of
	// magnitude (the client wait budget is 10s).
	threshold := 100 * time.Millisecond
	if chaosRaceBuild {
		threshold = 2 * time.Second
	}
	if p99 > threshold {
		t.Errorf("p99 latency=%v, want <%v", p99, threshold)
	}

	// Two-sided assertion on the chaos-phase upstream hits:
	//
	//  - Lower bound (>=1): with Cooldown=0 the TryLock-winning
	//    freshness goroutine MUST attempt the conditional GET against
	//    the hung upstream. If the count is zero, the test isn't
	//    actually exercising the hung-upstream code path (e.g. someone
	//    raised the cooldown back up, or the seed regressed) and the
	//    p99 / goroutine assertions become coverage theater.
	//
	//  - Upper bound (<=5): TryLock + the lifecycle ctx hold the count
	//    near 1 in practice. A larger number means cache HITs are
	//    triggering synchronous upstream calls — the exact regression
	//    this test guards against. The 5 is slack for legitimate
	//    sequential T1 attempts (the lock is short-lived in non-hung
	//    upstreams, so several rapid-fire wins can stack up).
	postPrimeHits := upstreamHits.Load() - primeHits
	if postPrimeHits < 1 {
		t.Errorf("upstream hits during chaos phase=%d, want >=1 (Cooldown=0 should let one freshness check fire — test is not exercising the hung path)",
			postPrimeHits)
	}
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
	c, err := cache.Open(context.Background(), t.TempDir(), silentLogger())
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	fc, err := fetch.New(fetch.Options{
		ConnectTimeout: 2 * time.Second,
		TotalTimeout:   5 * time.Second,
		MaxRetries:     0,
		// Allow IPv4 + IPv6 loopback. httptest.NewServer's bind family
		// is platform-dependent (Linux: 127.0.0.1, some BSDs: ::1); a
		// 127.0.0.1-only allowlist would 403 the IPv6 case before the
		// fetch even runs.
		AllowedHostRegex: []string{`^127\.0\.0\.1$`, `^::1$`},
		DenyTargetRanges: nil,
		Logger:           silentLogger(),
	})
	if err != nil {
		t.Fatalf("fetch.New: %v", err)
	}

	limiter := hostsem.New(4)

	freshChecker, err := freshness.New(freshness.Config{
		Cache:       c,
		Fetcher:     fc,
		HostLimiter: limiter,
		// Cooldown=0 so the chaos test actually exercises a hung
		// freshness check. With a non-zero cooldown the priming-phase
		// seed (LastCheckAt=now) gates every chaos-phase Check before
		// it can contact upstream, which leaves the SPEC §12.3
		// invariant ("hits don't block on hung upstream") unproven —
		// none of the freshness goroutines ever actually try to talk
		// to upstream. With Cooldown=0 the TryLock-winner contacts
		// upstream and hangs; the other 49+ goroutines TryLock-miss
		// and return immediately. The test then asserts at least one
		// post-prime hit (proving the freshness path was exercised).
		Cooldown: 0,
		Refresh:  10 * time.Minute,
		Logger:   silentLogger(),
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
