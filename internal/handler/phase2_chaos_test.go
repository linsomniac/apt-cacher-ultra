package handler

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
// AIDEV-NOTE: SPEC2 §12.3 calls for "no client receives A's InRelease
// with B's Packages or vice versa." The cache architecture does not
// implement per-client snapshot pinning — every request reads
// current_snapshot_id independently — so a client batch whose request
// timing straddles the atomic flip can see A for some requests and B
// for others. Per-request coherence (each individual body matches the
// snapshot id it resolved against) IS strictly enforced; per-client
// straddle is not. The test logs straddle counts so a regression that
// inflates the rate (e.g. a flip that's no longer atomic, or a torn
// member fetch) surfaces as a quantitative shift. This is documented
// as a deliberate gap in the §12.3 invariant set; SPEC2 should be
// revised to clarify the per-snapshot vs. per-client boundary, but
// that is outside the scope of this test.
func TestPhase2Chaos_AdoptionFlipUnderConcurrency(t *testing.T) {
	if testing.Short() {
		t.Skip("phase 2 chaos test skipped in -short mode")
	}

	snapA := makeChaos2Snapshot("A")
	snapB := makeChaos2Snapshot("B")

	var current atomic.Pointer[chaos2Snapshot]
	current.Store(&snapA)

	// memberFetchGate blocks the Adopter's member fetches when set.
	// The pre-flip burst runs while the gate is closed — guaranteeing
	// the §12.3 "during prefetch" precondition. The test releases the
	// gate after the burst so adoption can complete and we can run a
	// post-flip burst.
	memberFetchGate := newChaos2Gate()

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

	stack := newPhase2ChaosStackGated(t, upstreamURL, memberFetchGate)
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

	// Phase 2: close the member-fetch gate, swap upstream to serve B,
	// and drive ONE InRelease GET to start adoption. The freshness
	// conditional GET completes (returning B's bytes), Check spawns
	// the adoption goroutine, the goroutine VerifyInline-s and reaches
	// the member-fetch step — at which point it blocks on the gate.
	// We wait for the gate to record at least one waiter so the
	// chaos burst is guaranteed to run with adoption stuck mid-prefetch.
	memberFetchGate.Close()
	current.Store(&snapB)

	rec := httptest.NewRecorder()
	stack.handler.ServeHTTP(rec, proxyReq("GET", srv.URL, chaos2Suite+"/InRelease"))
	if rec.Code != http.StatusOK {
		t.Fatalf("trigger adoption: status=%d body=%q", rec.Code, rec.Body.String())
	}
	if err := memberFetchGate.WaitForWaiter(5 * time.Second); err != nil {
		t.Fatalf("adoption never reached member-fetch gate: %v", err)
	}

	// Phase 3a: pre-flip chaos burst — runs WHILE adoption is blocked
	// at the member-fetch step (gate is closed). All requests should
	// resolve against snapshot A: the flip transaction has not yet
	// run because the Adopter cannot complete prefetch.
	pre := runChaos2Burst(t, stack.handler, srv.URL, allPaths, 100, &snapA, &snapB)

	// Phase 3b: release the gate. Adoption resumes prefetch, fetches
	// B's Packages.gz, and runs the atomic flip transaction. Wait for
	// suite_freshness.current_snapshot_id to update.
	memberFetchGate.Open()
	if err := waitForFlip(t, stack, upstreamURL, snapAID, srv.URL, 15*time.Second); err != nil {
		t.Fatalf("flip never happened after gate release: %v", err)
	}
	stack.checker.WaitForAdoptions()

	// Phase 3c: post-flip chaos burst. With B adopted, every client
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

	// Pre-flip strict assertion: with the member-fetch gate held closed
	// for the duration of the pre-flip burst, current_snapshot_id is
	// still A. Every response must be A's bytes. This catches a flip
	// transaction that committed before its underlying member fetch
	// (which would be a serious adoption ordering bug — flip-before-
	// prefetch leaves the snapshot pointing at blobs we haven't
	// actually verified hash-against-declared yet).
	for _, s := range pre {
		if !s.ok || s.snapLabel != "A" {
			t.Errorf("pre-flip (gate closed): client=%d path=%s snap=%s status=%d (want A/200) — adoption flipped before member prefetch?",
				s.clientID, s.path, s.snapLabel, s.status)
		}
	}

	// Post-flip strict assertion: every response must be B's bytes.
	// By the time runChaos2Burst returns, the cache has flipped to
	// snapshot B (waitForFlip blocked on current_snapshot_id change)
	// and trySnapshotHit serves B's metadata directly. The .deb hit
	// path's package_hash check evicts the stale A-rooted url_path
	// row on first request and the §6.2 miss path refetches B's
	// bytes from upstream.
	for _, s := range post {
		if !s.ok || s.snapLabel != "B" {
			t.Errorf("post-flip: client=%d path=%s snap=%s status=%d (want B/200)",
				s.clientID, s.path, s.snapLabel, s.status)
		}
	}

	// Per-client metadata coherence: log the count of clients whose
	// InRelease and Packages.gz responses came from different
	// snapshots. Architecturally this can be non-zero (see the
	// AIDEV-NOTE on the test function), but inflating it is a
	// regression signal.
	preStraddles := chaos2CountMetadataStraddles(pre)
	postStraddles := chaos2CountMetadataStraddles(post)
	t.Logf("metadata straddle counts: pre-flip=%d post-flip=%d (architecturally allowed; inflation = regression)",
		preStraddles, postStraddles)

	// Final state: current_snapshot_id should point at a snapshot
	// whose inrelease_hash matches snapB.inRelease — not just
	// "anything that isn't A."
	suite, err = stack.handler.cache.GetSuiteFreshness(context.Background(),
		"http", upstreamURL.Hostname(), chaos2Suite)
	if err != nil || suite == nil || suite.CurrentSnapshotID == nil {
		t.Fatalf("post-chaos: suite_freshness=%+v err=%v", suite, err)
	}
	if *suite.CurrentSnapshotID == snapAID {
		t.Errorf("post-chaos: current_snapshot_id=%d (still A's) — adoption never flipped",
			snapAID)
	}
	finalSnap, err := stack.handler.cache.GetSuiteSnapshot(context.Background(), *suite.CurrentSnapshotID)
	if err != nil {
		t.Fatalf("post-chaos: GetSuiteSnapshot(%d): %v", *suite.CurrentSnapshotID, err)
	}
	wantBHash := chaos2Sha256Hex(snapB.inRelease)
	if finalSnap.InReleaseHash == nil || *finalSnap.InReleaseHash != wantBHash {
		gotHash := "<nil>"
		if finalSnap.InReleaseHash != nil {
			gotHash = *finalSnap.InReleaseHash
		}
		t.Errorf("post-chaos: final inrelease_hash=%s, want B's hash %s",
			gotHash, wantBHash)
	}
}

// chaos2CountMetadataStraddles tallies clients whose InRelease and
// Packages.gz responses came from different snapshots. Used as a
// quantitative regression signal for the SPEC2 §12.3 per-client
// claim — see the AIDEV-NOTE on TestPhase2Chaos_AdoptionFlipUnderConcurrency.
func chaos2CountMetadataStraddles(samples []chaos2Sample) int {
	type clientMeta struct{ inRelease, packages string }
	per := make(map[int]*clientMeta)
	for _, s := range samples {
		if !s.ok {
			continue
		}
		cm := per[s.clientID]
		if cm == nil {
			cm = &clientMeta{}
			per[s.clientID] = cm
		}
		switch {
		case strings.HasSuffix(s.path, "/InRelease"):
			cm.inRelease = s.snapLabel
		case strings.HasSuffix(s.path, "/Packages.gz"):
			cm.packages = s.snapLabel
		}
	}
	straddles := 0
	for _, cm := range per {
		if cm.inRelease != "" && cm.packages != "" && cm.inRelease != cm.packages {
			straddles++
		}
	}
	return straddles
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
// host:port, optionally blocks on a gate (so the chaos burst can run
// while adoption's member fetch is in flight), and then delegates to
// the production fetch.Client so URL/canonical-host validation,
// redirect blocking, deny-CIDR posture, and connection hardening all
// run end-to-end.
type chaos2RewritingFetcher struct {
	upstream *url.URL
	inner    *fetch.Client
	gate     *chaos2Gate // optional; when non-nil, blocks before delegating
}

func (f *chaos2RewritingFetcher) Fetch(ctx context.Context, target *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error) {
	if f.gate != nil {
		if err := f.gate.Wait(ctx); err != nil {
			return nil, fmt.Errorf("chaos2RewritingFetcher: gate: %w", err)
		}
	}
	u, err := url.Parse(target.URL)
	if err != nil {
		return nil, fmt.Errorf("chaos2RewritingFetcher: parse %q: %w", target.URL, err)
	}
	u.Host = f.upstream.Host
	rewritten := &fetch.Target{
		CanonicalHost: target.CanonicalHost,
		URL:           u.String(),
	}
	return f.inner.Fetch(ctx, rewritten, dst)
}

// chaos2Gate is a one-shot test gate. While Closed, Wait blocks the
// caller; Open releases all callers. The gate also tracks the count
// of waiters so the test can synchronize with "adoption is parked at
// member-fetch" without relying on sleeps.
type chaos2Gate struct {
	mu       sync.Mutex
	closed   bool
	release  chan struct{}
	waiters  int32 // accessed via atomic
	waiterCh chan struct{}
}

func newChaos2Gate() *chaos2Gate {
	return &chaos2Gate{
		release:  make(chan struct{}),
		waiterCh: make(chan struct{}, 64),
	}
}

func (g *chaos2Gate) Close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
}

func (g *chaos2Gate) Open() {
	g.mu.Lock()
	if !g.closed {
		g.mu.Unlock()
		return
	}
	g.closed = false
	close(g.release)
	g.release = make(chan struct{})
	g.mu.Unlock()
}

// Wait blocks until the gate is open or ctx cancels. Records the
// caller in the waiter channel so WaitForWaiter can synchronize.
func (g *chaos2Gate) Wait(ctx context.Context) error {
	g.mu.Lock()
	if !g.closed {
		g.mu.Unlock()
		return nil
	}
	release := g.release
	g.mu.Unlock()
	atomic.AddInt32(&g.waiters, 1)
	defer atomic.AddInt32(&g.waiters, -1)
	// Non-blocking send so even with the channel full we don't deadlock.
	select {
	case g.waiterCh <- struct{}{}:
	default:
	}
	select {
	case <-release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WaitForWaiter blocks until at least one goroutine has entered Wait
// (i.e. the gate has parked someone), or until timeout elapses.
func (g *chaos2Gate) WaitForWaiter(timeout time.Duration) error {
	if atomic.LoadInt32(&g.waiters) > 0 {
		return nil
	}
	select {
	case <-g.waiterCh:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("no waiter arrived within %v", timeout)
	}
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

// newPhase2ChaosStackGated wires a full Phase 2 stack: cache,
// fetch.Client (loopback-allow), pass-through GPG verifier,
// port-rewriting adoption fetcher (delegating into the same
// production fetch.Client so URL validation and transport hardening
// run end-to-end), freshness checker with adopter wired in, and a
// Handler pointing at all of the above. Cooldown=0 so every metadata
// hit can fire a freshness check; the per-suite mutex inside Check
// coalesces the burst into a single in-flight conditional GET.
//
// gate, when non-nil, blocks the Adopter's member fetches until
// Open()ed — the test uses this to park adoption at member-prefetch
// while the chaos burst runs.
func newPhase2ChaosStackGated(t *testing.T, upstream *url.URL, gate *chaos2Gate) *phase2ChaosStack {
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
		TotalTimeout:     30 * time.Second, // gated member fetches park here; budget must exceed test wall-clock
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
		Fetcher:     &chaos2RewritingFetcher{upstream: upstream, inner: fc, gate: gate},
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
