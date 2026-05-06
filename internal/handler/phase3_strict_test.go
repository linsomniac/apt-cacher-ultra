package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
)

// newPhase3StrictHandler builds a Handler wired with the SPEC3 §6.1
// strict-mode flags. refuseUnvouched and adoptionEnabled are explicit
// arguments so each test case can dial them independently.
//
// upstreamHits is incremented every time the upstream is contacted —
// the test asserts the strict-mode 502 happens BEFORE upstream is
// dialed (per SPEC3 §6.2 "no upstream connection initiated").
func newPhase3StrictHandler(t *testing.T,
	refuseUnvouched, adoptionEnabled bool,
	upstreamHits *atomic.Int32) (*Handler, *httptest.Server) {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if upstreamHits != nil {
			upstreamHits.Add(1)
		}
		_, _ = w.Write([]byte("upstream bytes"))
	}))
	t.Cleanup(srv.Close)

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
		MaxRetries:       1,
		AllowedHostRegex: []string{`^127\.0\.0\.1$`},
		DenyTargetRanges: nil,
		Logger:           silentLogger(),
	})
	if err != nil {
		t.Fatalf("fetch.New: %v", err)
	}

	h, err := New(Config{
		Parser:              parser,
		Cache:               c,
		Fetch:               fc,
		HostLimiter:         hostsem.New(4),
		Logger:              silentLogger(),
		Serve:               config.ServeConfig{},
		RefuseUnvouchedDebs: refuseUnvouched,
		AdoptionEnabled:     adoptionEnabled,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h, srv
}

// adoptCoverageComplete commits an inline snapshot and stamps
// package_coverage_complete to the requested value. SPEC3 §7.5.4:
// strict mode reads this bit on every current snapshot; tests need
// both true (strict refuses) and false (strict falls through) shapes.
func adoptCoverageComplete(t *testing.T, h *Handler,
	scheme, host, suite string, coverageComplete bool) int64 {
	t.Helper()
	releaseBlob := writeBlob(t, h, []byte("InRelease for "+suite))

	if err := h.cache.PutSuiteFreshness(context.Background(), cache.SuiteFreshness{
		CanonicalScheme: scheme,
		CanonicalHost:   host,
		SuitePath:       suite,
	}); err != nil {
		t.Fatalf("PutSuiteFreshness: %v", err)
	}
	id, _, err := h.cache.InsertCandidateSnapshot(context.Background(),
		cache.SnapshotCandidate{
			CanonicalScheme: scheme,
			CanonicalHost:   host,
			SuitePath:       suite,
			InReleaseHash:   &releaseBlob,
		})
	if err != nil {
		t.Fatalf("InsertCandidateSnapshot: %v", err)
	}
	if err := h.cache.CommitAdoption(context.Background(), id,
		[]cache.SnapshotMember{
			{SnapshotID: id, Path: "InRelease", BlobHash: releaseBlob, DeclaredSHA256: releaseBlob},
		},
		nil, nil, coverageComplete); err != nil {
		t.Fatalf("CommitAdoption: %v", err)
	}
	return id
}

// TestPhase3StrictMode_RefusesUnvouchedDebOnFullyCoveredHost is the
// SPEC3 §12.4 first case: cache running with adoption.enabled = true
// and integrity.refuse_unvouched_debs = true; host has a current
// snapshot with package_coverage_complete = 1; client requests a
// .deb path that no snapshot covers. Cache responds 502 + Retry-After:
// 60; no upstream connection made.
func TestPhase3StrictMode_RefusesUnvouchedDebOnFullyCoveredHost(t *testing.T) {
	var upstreamHits atomic.Int32
	h, srv := newPhase3StrictHandler(t, true, true, &upstreamHits)

	scheme, host, port := splitURL(t, srv.URL)
	canonHost := hostKey(host, port)

	// Adopt a fully-covered snapshot. The strict-mode predicate keys
	// on every current snapshot having package_coverage_complete = 1.
	adoptCoverageComplete(t, h, scheme, canonHost, "/dists/noble", true)

	// Request a .deb path that no package_hash row covers (it lives
	// outside any snapshot's known coverage). Strict mode must refuse.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/pool/main/u/unknown/unknown.deb"))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502 (strict-mode refusal)", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After=%q, want 60", got)
	}
	if got := upstreamHits.Load(); got != 0 {
		t.Errorf("upstream hits = %d, want 0 (SPEC3 §6.2 — no upstream connection initiated)", got)
	}
}

// TestPhase3StrictMode_FallsThroughOnIncompleteCoverage covers the
// SPEC3 §6.1 step 2b passthrough branch. A host whose current
// snapshot has package_coverage_complete = 0 falls through to
// trust-upstream regardless of refuse_unvouched_debs.
func TestPhase3StrictMode_FallsThroughOnIncompleteCoverage(t *testing.T) {
	var upstreamHits atomic.Int32
	h, srv := newPhase3StrictHandler(t, true, true, &upstreamHits)

	scheme, host, port := splitURL(t, srv.URL)
	canonHost := hostKey(host, port)

	// Snapshot has coverage_complete = 0 — strict mode must
	// fall through.
	adoptCoverageComplete(t, h, scheme, canonHost, "/dists/noble", false)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/pool/main/u/unknown/unknown.deb"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (passthrough on incomplete coverage)", rec.Code)
	}
	if got := upstreamHits.Load(); got == 0 {
		t.Errorf("upstream hits = 0; passthrough should have fetched upstream")
	}
}

// TestPhase3StrictMode_OffFallsThrough: integrity.refuse_unvouched_debs
// = false (default) keeps the unvouched .deb on the trust-upstream
// path regardless of coverage. SPEC3 §1.3 — strict mode is opt-in.
func TestPhase3StrictMode_OffFallsThrough(t *testing.T) {
	var upstreamHits atomic.Int32
	h, srv := newPhase3StrictHandler(t, false, true, &upstreamHits)

	scheme, host, port := splitURL(t, srv.URL)
	canonHost := hostKey(host, port)

	// Even with full coverage, strict-off must fall through.
	adoptCoverageComplete(t, h, scheme, canonHost, "/dists/noble", true)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/pool/main/u/unknown/unknown.deb"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (strict mode disabled)", rec.Code)
	}
	if got := upstreamHits.Load(); got == 0 {
		t.Errorf("upstream hits = 0; strict-off must reach upstream")
	}
}

// TestPhase3StrictMode_AdoptionDisabledIsInert: even with
// refuse_unvouched_debs = true, adoption.enabled = false is the
// operator's deliberate return to trust-upstream posture. The
// predicate must explicitly check adoption.enabled and not refuse,
// even if stale current_snapshot_id rows exist (SPEC3 §6.1, Q17).
func TestPhase3StrictMode_AdoptionDisabledIsInert(t *testing.T) {
	var upstreamHits atomic.Int32
	h, srv := newPhase3StrictHandler(t, true, false, &upstreamHits)

	scheme, host, port := splitURL(t, srv.URL)
	canonHost := hostKey(host, port)

	// Stale snapshot with full coverage (the kind a prior
	// adoption.enabled=true run would leave behind).
	adoptCoverageComplete(t, h, scheme, canonHost, "/dists/noble", true)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/pool/main/u/unknown/unknown.deb"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (adoption disabled — strict mode inert)", rec.Code)
	}
	if got := upstreamHits.Load(); got == 0 {
		t.Errorf("upstream hits = 0; adoption-disabled must reach upstream")
	}
}

// TestPhase3StrictMode_NoSnapshotsFallsThrough: a host with no
// current snapshots has no contract for strict mode to enforce. The
// predicate falls through to trust-upstream regardless of the flag.
func TestPhase3StrictMode_NoSnapshotsFallsThrough(t *testing.T) {
	var upstreamHits atomic.Int32
	h, srv := newPhase3StrictHandler(t, true, true, &upstreamHits)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/pool/main/u/unknown/unknown.deb"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (no current snapshots)", rec.Code)
	}
	if got := upstreamHits.Load(); got == 0 {
		t.Errorf("upstream hits = 0; cold cache must reach upstream")
	}
}

// TestPhase3StrictMode_HitPathRefusesAfterPriorPhase1Row covers the
// hit-path predicate (checkPackageHash). The .deb already has a
// url_path row from a Phase-1-style fetch, but the host has since
// adopted a full-coverage snapshot. The path is unvouched → strict
// mode 502 fires from the hit path (not the miss path).
func TestPhase3StrictMode_HitPathRefusesAfterPriorPhase1Row(t *testing.T) {
	var upstreamHits atomic.Int32
	h, srv := newPhase3StrictHandler(t, true, true, &upstreamHits)

	scheme, host, port := splitURL(t, srv.URL)
	canonHost := hostKey(host, port)
	const debP = "/pool/main/p/phase1/phase1.deb"

	// Phase 1 prime: url_path row + blob in cache.
	primer := httptest.NewRecorder()
	h.ServeHTTP(primer, proxyReq("GET", srv.URL, debP))
	if primer.Code != http.StatusOK {
		t.Fatalf("primer: %d", primer.Code)
	}

	// Now adopt a full-coverage snapshot that does NOT include this
	// .deb. The hit-path predicate fires.
	adoptCoverageComplete(t, h, scheme, canonHost, "/dists/noble", true)

	priorHits := upstreamHits.Load()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, debP))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502 (hit-path strict-mode refusal)", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After=%q, want 60", got)
	}
	// Strict-mode hit-path refusal must not contact upstream — the
	// row already pointed at a cached blob, but strict says "no."
	if got := upstreamHits.Load(); got != priorHits {
		t.Errorf("upstream hits delta = %d, want 0 (hit-path refusal must not fetch)",
			got-priorHits)
	}
}

// TestPhase3StrictMode_IndependentURLParse confirms the strict-mode
// predicate runs correctly for the proxy mode (absolute-URI request)
// path. This guards against forgetting to plumb the same predicate
// into both code branches.
func TestPhase3StrictMode_IndependentURLParse(t *testing.T) {
	var upstreamHits atomic.Int32
	h, srv := newPhase3StrictHandler(t, true, true, &upstreamHits)

	scheme, host, port := splitURL(t, srv.URL)
	canonHost := hostKey(host, port)
	adoptCoverageComplete(t, h, scheme, canonHost, "/dists/noble", true)

	// Construct a proxy-mode (absolute-URI) request manually so the
	// path-parse code path is exercised independently.
	upstreamURL, _ := url.Parse(srv.URL)
	req := httptest.NewRequest("GET", upstreamURL.String()+"/pool/main/u/unknown/unknown.deb", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("proxy-mode strict request: status=%d, want 502", rec.Code)
	}
}

// TestPhase3StrictMode_NonDebPathsPassthrough is the SPEC3 §6.1
// .deb-only gate: source tarballs (.tar.xz), debug debs (.udeb),
// and source descriptions (.dsc) must NOT be refused even under full
// coverage with strict mode on. Those paths are legitimately not in
// package_hash by design — package_hash covers binary .deb files
// only — so the strict predicate must not treat their absence as a
// refusal trigger.
func TestPhase3StrictMode_NonDebPathsPassthrough(t *testing.T) {
	var upstreamHits atomic.Int32
	h, srv := newPhase3StrictHandler(t, true, true, &upstreamHits)

	scheme, host, port := splitURL(t, srv.URL)
	canonHost := hostKey(host, port)
	adoptCoverageComplete(t, h, scheme, canonHost, "/dists/noble", true)

	cases := []string{
		"/pool/main/u/util/util_1.0.tar.xz",  // source tarball
		"/pool/main/u/util/util_1.0.tar.gz",  // older source tarball
		"/pool/main/u/util/util_1.0.dsc",     // source description
		"/pool/main/u/util/util_1.0_amd64.udeb", // debian-installer udeb
	}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			before := upstreamHits.Load()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, proxyReq("GET", srv.URL, p))
			if rec.Code != http.StatusOK {
				t.Errorf("non-.deb %s: status=%d, want 200 (must not be strict-refused)", p, rec.Code)
			}
			if upstreamHits.Load() == before {
				t.Errorf("non-.deb %s: did not reach upstream", p)
			}
		})
	}
}
