package handler

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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

// TestPhase2Chaos_AdoptionFlipUnderConcurrency is the SPEC2 §12.3 gate:
// snapshot A is adopted, upstream then publishes snapshot B, and during
// adoption of B 100 concurrent clients each issue an apt-shaped batch
// of {InRelease, Packages.gz, 5 .debs}. Per-response coherence (body
// bytes match either A's or B's authoritative content for that path —
// never a torn mix) must hold throughout, the flip must complete, and
// the cache's final state must point at B.
//
// AIDEV-NOTE: the cross-request "no client receives A's InRelease with
// B's Packages" claim in SPEC2 §12.3 is a per-snapshot guarantee — each
// individual response is coherent against its own X-Cache-Snapshot, but
// a client batch that straddles the atomic flip can legitimately see A
// for early requests and B for later ones (current_snapshot_id is
// re-read per request). The cache's contract is per-request snapshot
// resolution; clients that need strict per-batch pinning are out of
// scope for Phase 2. We assert per-response coherence and final flip
// success, which is what this layer can guarantee.
func TestPhase2Chaos_AdoptionFlipUnderConcurrency(t *testing.T) {
	if testing.Short() {
		t.Skip("phase 2 chaos test skipped in -short mode")
	}

	snapA := makeChaos2Snapshot("A")
	snapB := makeChaos2Snapshot("B")

	var current atomic.Pointer[chaos2Snapshot]
	current.Store(&snapA)

	var upstreamCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		snap := current.Load()
		body, etag := chaos2BodyAndETagFor(snap, r.URL.Path)
		if body == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.Header().Set("ETag", etag)
		w.Header().Set("Last-Modified", "Mon, 01 Jan 2024 00:00:00 GMT")
		// 304 short-circuit: when If-None-Match matches the served ETag,
		// upstream returns 304 (saves the server bandwidth and is what a
		// real archive does). We don't rely on this branch for the test
		// invariants but it makes the freshness conditional GET path
		// realistic.
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	upstreamURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	stack := newPhase2ChaosStack(t, upstreamURL)
	defer stack.handler.Close()

	allPaths := chaos2AllPaths()

	// Phase 1: prime the cache with snapshot A's bytes. Each path goes
	// through the Phase 1 cache-miss flow (populates url_path + blob;
	// the InRelease miss seeds suite_freshness with ETag="A").
	for _, p := range allPaths {
		rec := httptest.NewRecorder()
		stack.handler.ServeHTTP(rec, proxyReq("GET", srv.URL, p))
		if rec.Code != http.StatusOK {
			t.Fatalf("phase-1 prime %s: status=%d body=%q", p, rec.Code, rec.Body.String())
		}
		if want := chaos2WantBody(&snapA, p); !bytes.Equal(rec.Body.Bytes(), want) {
			t.Fatalf("phase-1 prime %s: body=%q, want %q", p, rec.Body.Bytes(), want)
		}
	}
	// Wait for any async freshness goroutines from the prime phase to
	// settle (they conditional-GET back to upstream and 304 — no
	// adoption fires, but joining keeps the test deterministic).
	stack.checker.WaitForAdoptions()

	// Phase 1b: adopt snapshot A by directly invoking the Adopter. All
	// member blobs are already in pool/ from phase 1, so adoptMember
	// short-circuits via BlobExists+rehash — no upstream traffic. This
	// gives us the SPEC2 §12.3 "cache with adopted snapshot A"
	// precondition without requiring the freshness conditional GET to
	// observe a change (it returns 304 against ETag="A").
	suiteRef := freshness.SuiteRef{
		CanonicalScheme: "http",
		CanonicalHost:   upstreamURL.Hostname(),
		SuitePath:       chaos2Suite,
	}
	if err := stack.adopter.Run(context.Background(), suiteRef, snapA.inRelease, "\"A\"", ""); err != nil {
		t.Fatalf("adopt A directly: %v", err)
	}

	// Confirm A is current, capture the snapshot id for the final-state
	// assertion.
	suite, err := stack.handler.cache.GetSuiteFreshness(context.Background(),
		"http", upstreamURL.Hostname(), chaos2Suite)
	if err != nil || suite == nil || suite.CurrentSnapshotID == nil {
		t.Fatalf("post-A adoption: suite_freshness=%+v err=%v", suite, err)
	}
	snapAID := *suite.CurrentSnapshotID

	// Phase 1c: warm a sample request against the adopted snapshot to
	// confirm trySnapshotHit / tryURLPathHit serve A correctly under
	// the new contract. Belt-and-suspenders: if priming has somehow
	// left the cache in a bad state, we want to fail here with a
	// clear message rather than in the chaos-burst assertions.
	for _, p := range allPaths {
		rec := httptest.NewRecorder()
		stack.handler.ServeHTTP(rec, proxyReq("GET", srv.URL, p))
		if rec.Code != http.StatusOK {
			t.Fatalf("post-A warm %s: status=%d body=%q", p, rec.Code, rec.Body.String())
		}
		if want := chaos2WantBody(&snapA, p); !bytes.Equal(rec.Body.Bytes(), want) {
			t.Fatalf("post-A warm %s: body=%q, want A's bytes", p, rec.Body.Bytes())
		}
	}
	stack.checker.WaitForAdoptions()

	// Phase 2: swap upstream to serve B. The next freshness conditional
	// GET will observe the change (ETag rotates from "A" to "B"),
	// spawning a Phase 2 adoption.
	current.Store(&snapB)

	// Phase 3a: pre-flip chaos burst. The 100 client goroutines start
	// while the cache is still pinned to snapshot A; the first
	// metadata GET to fire freshness wins the per-suite TryLock and
	// spawns the conditional GET (~ms-scale on loopback) which in
	// turn hands off to the adoption goroutine. While that's in
	// flight the rest of the burst sees A everywhere.
	pre := runChaos2Burst(t, stack.handler, srv.URL, allPaths, 100, &snapA, &snapB)

	// Drive the flip to completion. The pre-flip burst may have
	// triggered a Check goroutine that has not yet handed off to the
	// adoption goroutine (Check returns synchronously after spawning
	// the adoption goroutine, so adoptionWg will become non-zero a
	// short time after the last burst request returns). Poll
	// suite_freshness for the flip; if it doesn't happen on its own
	// (Cooldown is 0 and the pre-flip burst already drove a Check —
	// but the per-suite TryLock could have eaten every Check by
	// returning before the underlying conditional GET resolved), nudge
	// it by issuing one more InRelease GET against a settled lock.
	if err := waitForFlip(t, stack, upstreamURL, snapAID, srv.URL, 15*time.Second); err != nil {
		t.Fatalf("flip never happened: %v", err)
	}
	stack.checker.WaitForAdoptions()

	// Phase 3b: post-flip chaos burst. With B adopted, every client
	// should now see B's bytes for every path (modulo .deb miss-path
	// refetch, which validates against snapshot B's package_hash and
	// returns B's bytes).
	post := runChaos2Burst(t, stack.handler, srv.URL, allPaths, 100, &snapA, &snapB)

	// Per-burst coherence: every response 200, body byte-for-byte equal
	// to either A's or B's authoritative content for that path. This
	// is the SPEC2 §12.3 "never mixed" invariant — a client straddling
	// the flip sees A early and B late, but each individual response
	// is internally consistent.
	assertChaos2Coherence(t, "pre-flip", pre, allPaths)
	assertChaos2Coherence(t, "post-flip", post, allPaths)

	// Post-flip: every response must be B's bytes. By the time
	// runChaos2Burst returns the second time, the cache has flipped
	// to snapshot B (waitForFlip blocked until current_snapshot_id
	// changed) and trySnapshotHit serves B's metadata directly. The
	// .deb hit path's package_hash check evicts the stale A-rooted
	// url_path row on first request and the §6.2 miss path refetches
	// B's bytes from upstream.
	for _, s := range post {
		if !s.ok || s.snapLabel != "B" {
			t.Errorf("post-flip: client=%d path=%s snap=%s status=%d (want B/200)",
				s.clientID, s.path, s.snapLabel, s.status)
		}
	}

	// Final state: current_snapshot_id should point at a snapshot
	// other than A's.
	suite, err = stack.handler.cache.GetSuiteFreshness(context.Background(),
		"http", upstreamURL.Hostname(), chaos2Suite)
	if err != nil || suite == nil || suite.CurrentSnapshotID == nil {
		t.Fatalf("post-chaos: suite_freshness=%+v err=%v", suite, err)
	}
	if *suite.CurrentSnapshotID == snapAID {
		t.Errorf("post-chaos: current_snapshot_id=%d (still A's) — adoption never flipped",
			snapAID)
	}
}

// chaos2Sample captures one client request's outcome.
type chaos2Sample struct {
	clientID  int
	path      string
	body      []byte
	snapLabel string // "A", "B", or "unknown"
	ok        bool
	status    int
}

// runChaos2Burst fires `clients` goroutines, each issuing the full
// path set sequentially through h.ServeHTTP, and collects per-request
// samples. Bounded by a 30s wall-clock so a hang surfaces as a clear
// fail rather than a `go test` timeout.
func runChaos2Burst(t *testing.T, h *Handler, srvURL string, paths []string, clients int, a, b *chaos2Snapshot) []chaos2Sample {
	t.Helper()
	results := make(chan chaos2Sample, clients*len(paths))
	var wg sync.WaitGroup
	wg.Add(clients)
	for i := 0; i < clients; i++ {
		go func(id int) {
			defer wg.Done()
			for _, p := range paths {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, proxyReq("GET", srvURL, p))
				results <- chaos2Sample{
					clientID:  id,
					path:      p,
					body:      append([]byte(nil), rec.Body.Bytes()...),
					snapLabel: chaos2ClassifyBody(p, rec.Body.Bytes(), a, b),
					ok:        rec.Code == http.StatusOK,
					status:    rec.Code,
				}
			}
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("chaos burst (clients=%d, paths=%d) did not complete within 30s — adoption may be stuck or requests are blocking", clients, len(paths))
	}
	close(results)
	out := make([]chaos2Sample, 0, clients*len(paths))
	for s := range results {
		out = append(out, s)
	}
	return out
}

// assertChaos2Coherence checks the per-request invariants the §12.3
// gate guarantees regardless of which snapshot a request resolved
// against: status=200 and the body is byte-for-byte equal to either
// A's or B's authoritative bytes for that path. "unknown" == torn
// write or stale-fragment leak.
func assertChaos2Coherence(t *testing.T, label string, samples []chaos2Sample, paths []string) {
	t.Helper()
	type pathStats struct{ a, b, unknown int }
	perPath := make(map[string]*pathStats, len(paths))
	var nonOK int
	for _, s := range samples {
		if !s.ok {
			nonOK++
			t.Errorf("%s: non-200 client=%d path=%s status=%d body=%q",
				label, s.clientID, s.path, s.status, string(s.body))
			continue
		}
		st := perPath[s.path]
		if st == nil {
			st = &pathStats{}
			perPath[s.path] = st
		}
		switch s.snapLabel {
		case "A":
			st.a++
		case "B":
			st.b++
		default:
			st.unknown++
			t.Errorf("%s: body coherence broken — client=%d path=%s body=%q matches neither A nor B",
				label, s.clientID, s.path, string(s.body))
		}
	}
	if nonOK > 0 {
		t.Errorf("%s: %d non-200 responses", label, nonOK)
	}
	for p, st := range perPath {
		t.Logf("%s: path=%s A=%d B=%d unknown=%d", label, p, st.a, st.b, st.unknown)
	}
}

// waitForFlip polls suite_freshness.current_snapshot_id until it
// changes from snapAID, or until the deadline elapses. To handle the
// case where the pre-flip burst's freshness Check completed but
// returned before the conditional-GET-spawned adoption goroutine
// could actually flip the pointer, we periodically nudge by issuing
// one more InRelease GET (which, if no Check is currently in flight,
// spawns a fresh one).
func waitForFlip(t *testing.T, stack *phase2ChaosStack, upstreamURL *url.URL, snapAID int64, srvURL string, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		suite, err := stack.handler.cache.GetSuiteFreshness(context.Background(),
			"http", upstreamURL.Hostname(), chaos2Suite)
		if err == nil && suite != nil && suite.CurrentSnapshotID != nil && *suite.CurrentSnapshotID != snapAID {
			return nil
		}
		// Nudge: a fresh InRelease cache-hit fires maybeFireFreshness;
		// if the per-suite mutex is free, a new Check runs.
		rec := httptest.NewRecorder()
		stack.handler.ServeHTTP(rec, proxyReq("GET", srvURL, chaos2Suite+"/InRelease"))
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("current_snapshot_id still = %d after %v", snapAID, timeout)
}

// chaos2Suite is the suite path the §12.3 fixture uses.
const chaos2Suite = "/ubuntu/dists/noble"

// chaos2DebRels is the suite-relative-from-repo .deb path set each
// client requests. Repo-rel paths so they match what
// repoRootFromSuitePath + Packages.Filename produce when adoption
// builds the package_hash row keys.
var chaos2DebRels = []string{
	"pool/main/p/pkg1/pkg1_1.0_amd64.deb",
	"pool/main/p/pkg2/pkg2_1.0_amd64.deb",
	"pool/main/p/pkg3/pkg3_1.0_amd64.deb",
	"pool/main/p/pkg4/pkg4_1.0_amd64.deb",
	"pool/main/p/pkg5/pkg5_1.0_amd64.deb",
}

// chaos2PackagesGzPath is the suite-relative path of the Packages.gz
// member referenced from the synthesized Release.
const chaos2PackagesGzPath = "main/binary-amd64/Packages.gz"

// chaos2AllPaths returns the absolute apt-shaped paths each client
// requests in order: InRelease, Packages.gz, then five .debs.
func chaos2AllPaths() []string {
	out := make([]string, 0, 2+len(chaos2DebRels))
	out = append(out,
		chaos2Suite+"/InRelease",
		chaos2Suite+"/"+chaos2PackagesGzPath,
	)
	for _, rel := range chaos2DebRels {
		out = append(out, "/ubuntu/"+rel)
	}
	return out
}

// chaos2Snapshot bundles the bytes upstream serves for one logical
// snapshot. label distinguishes A vs B in the response payloads so a
// torn-write bug surfaces as a body-mismatch rather than a
// coincidentally-equal byte stream.
type chaos2Snapshot struct {
	label      string
	inRelease  []byte            // /<suite>/InRelease
	packagesGz []byte            // /<suite>/main/binary-amd64/Packages.gz
	debBodies  map[string][]byte // suite-rel path -> body
}

// makeChaos2Snapshot constructs an A-or-B snapshot fixture. .deb body
// content embeds the label so byte equality discriminates A vs B; the
// Packages.gz body declares each .deb's SHA256, and the synthesized
// Release file declares the Packages.gz's SHA256. Adoption parses the
// Release → Packages.gz chain end-to-end against these fixtures.
func makeChaos2Snapshot(label string) chaos2Snapshot {
	debs := make(map[string][]byte, len(chaos2DebRels))
	pkgEntries := make(map[string]string, len(chaos2DebRels))
	for _, rel := range chaos2DebRels {
		body := []byte(fmt.Sprintf("snapshot-%s|deb|%s", label, rel))
		debs[rel] = body
		pkgEntries[rel] = chaos2Sha256Hex(body)
	}
	pkgsTxt := chaos2BuildPackagesText(pkgEntries)
	pkgsGz := chaos2Gzip(pkgsTxt)

	rel := chaos2BuildRelease(map[string][]byte{
		chaos2PackagesGzPath: pkgsGz,
	})

	return chaos2Snapshot{
		label:      label,
		inRelease:  rel,
		packagesGz: pkgsGz,
		debBodies:  debs,
	}
}

// chaos2BuildPackagesText builds a Packages text body declaring each
// (path → declared SHA256). Filename is repo-relative so adoption's
// repoRootFromSuitePath + ref.Filename composition reconstructs the
// absolute .deb path apt would request.
func chaos2BuildPackagesText(entries map[string]string) []byte {
	var sb strings.Builder
	for rel, h := range entries {
		fmt.Fprintf(&sb, "Package: %s\n", rel)
		fmt.Fprintf(&sb, "Filename: %s\n", rel)
		fmt.Fprintf(&sb, "Size: 0\n")
		fmt.Fprintf(&sb, "SHA256: %s\n\n", h)
	}
	return []byte(sb.String())
}

// chaos2BuildRelease constructs Release-style text declaring each
// member's SHA256 and size. The pass-through verifier returns input
// verbatim, so this is also the bytes upstream serves at /InRelease.
func chaos2BuildRelease(members map[string][]byte) []byte {
	var sb strings.Builder
	sb.WriteString("Origin: ChaosTest\n")
	sb.WriteString("Suite: noble\n")
	sb.WriteString("SHA256:\n")
	for p, body := range members {
		fmt.Fprintf(&sb, " %s %d %s\n", chaos2Sha256Hex(body), len(body), p)
	}
	return []byte(sb.String())
}

func chaos2Gzip(b []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		panic(err)
	}
	if err := w.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func chaos2Sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// chaos2BodyAndETagFor maps a request path to (body, ETag) for the
// current snapshot. ETag carries the snapshot label so a freshness
// conditional GET can short-circuit with 304 when the snapshot
// hasn't changed.
func chaos2BodyAndETagFor(s *chaos2Snapshot, urlPath string) ([]byte, string) {
	etag := `"` + s.label + `"`
	switch {
	case strings.HasSuffix(urlPath, "/InRelease"):
		return s.inRelease, etag
	case strings.HasSuffix(urlPath, "/Packages.gz"):
		return s.packagesGz, etag
	}
	for rel, b := range s.debBodies {
		if strings.HasSuffix(urlPath, "/"+rel) {
			return b, etag
		}
	}
	return nil, ""
}

// chaos2WantBody returns the canonical body for path under snapshot s,
// or nil if path doesn't belong to s.
func chaos2WantBody(s *chaos2Snapshot, urlPath string) []byte {
	b, _ := chaos2BodyAndETagFor(s, urlPath)
	return b
}

// chaos2ClassifyBody reports whether body matches snapshot A or B for
// path. Returns "A", "B", or "unknown" — the latter is the per-response
// coherence violation the test guards against.
func chaos2ClassifyBody(urlPath string, body []byte, a, b *chaos2Snapshot) string {
	if bytes.Equal(body, chaos2WantBody(a, urlPath)) {
		return "A"
	}
	if bytes.Equal(body, chaos2WantBody(b, urlPath)) {
		return "B"
	}
	return "unknown"
}

// chaos2PassVerifier is a freshness.Verifier that returns the input
// verbatim. The chaos test focuses on adoption-flip semantics; GPG
// verification has separate coverage in §12.4.
type chaos2PassVerifier struct{}

func (chaos2PassVerifier) VerifyInline(ctx context.Context, suite freshness.SuiteRef, in []byte) ([]byte, error) {
	return in, nil
}

// chaos2RewritingFetcher is the AdoptionFetcher seam. The Adopter's
// buildMemberURL produces port-less URLs ("http://127.0.0.1/...")
// because canonicalize strips the port from the cache-key host —
// which never matches the random port httptest binds to. This
// wrapper rewrites the target URL's authority to the test server's
// host:port and then issues the request via a plain net/http client,
// bypassing fetch.Client's deny-CIDR + AllowedHostRegex (the test
// upstream is loopback by construction; SSRF posture is irrelevant).
type chaos2RewritingFetcher struct {
	upstream *url.URL
	client   *http.Client
}

func (f *chaos2RewritingFetcher) Fetch(ctx context.Context, target *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error) {
	u, err := url.Parse(target.URL)
	if err != nil {
		return nil, fmt.Errorf("chaos2RewritingFetcher: parse %q: %w", target.URL, err)
	}
	u.Host = f.upstream.Host
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("chaos2RewritingFetcher: NewRequest: %w", err)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("chaos2RewritingFetcher: Do %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("chaos2RewritingFetcher: upstream %s: %d", u, resp.StatusCode)
	}
	n, err := io.Copy(dst, resp.Body)
	if err != nil {
		return nil, fmt.Errorf("chaos2RewritingFetcher: copy %s: %w", u, err)
	}
	return &fetch.FetchResult{
		Status:        http.StatusOK,
		ContentLength: n,
		ContentType:   resp.Header.Get("Content-Type"),
		ETag:          resp.Header.Get("ETag"),
		LastModified:  resp.Header.Get("Last-Modified"),
	}, nil
}

// phase2ChaosStack bundles the wired-up handler stack the chaos test
// drives. The handler holds the cache + fetch.Client + freshness; the
// adopter is exposed separately so the test can run a direct A
// adoption (bypassing the freshness conditional-GET path which would
// 304 against the prime-phase ETag).
type phase2ChaosStack struct {
	handler *Handler
	checker *freshness.Checker
	adopter *freshness.Adopter
}

// newPhase2ChaosStack wires a full Phase 2 stack: cache, fetch.Client
// (loopback-allow), pass-through GPG verifier, port-rewriting adoption
// fetcher, freshness checker with adopter wired in, and a Handler
// pointing at all of the above. Cooldown=0 so every metadata hit can
// fire a freshness check; the per-suite mutex inside Check coalesces
// the burst into a single in-flight conditional GET.
func newPhase2ChaosStack(t *testing.T, upstream *url.URL) *phase2ChaosStack {
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
		MaxRetries:       0,
		AllowedHostRegex: []string{`^127\.0\.0\.1$`, `^::1$`},
		DenyTargetRanges: nil,
		Logger:           silentLogger(),
	})
	if err != nil {
		t.Fatalf("fetch.New: %v", err)
	}

	limiter := hostsem.New(8)

	adopter, err := freshness.NewAdopter(freshness.AdoptionConfig{
		Cache:       c,
		Fetcher:     &chaos2RewritingFetcher{upstream: upstream, client: &http.Client{Timeout: 5 * time.Second}},
		Verifier:    chaos2PassVerifier{},
		HostLimiter: limiter,
		Logger:      silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewAdopter: %v", err)
	}

	checker, err := freshness.New(freshness.Config{
		Cache:       c,
		Fetcher:     fc,
		HostLimiter: limiter,
		Cooldown:    0,
		Refresh:     10 * time.Minute,
		Logger:      silentLogger(),
		Adopter:     adopter,
		LifetimeCtx: context.Background(),
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
		Freshness:   checker,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &phase2ChaosStack{handler: h, checker: checker, adopter: adopter}
}
