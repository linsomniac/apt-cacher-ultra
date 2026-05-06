package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// TestSuiteRelativePath_TrimsPrefix verifies the helper that converts
// req.Path into the suite-relative form snapshot_member.path uses.
func TestSuiteRelativePath_TrimsPrefix(t *testing.T) {
	cases := []struct {
		suitePath, fullPath, want string
	}{
		{"/ubuntu/dists/noble", "/ubuntu/dists/noble/InRelease", "InRelease"},
		{"/ubuntu/dists/noble", "/ubuntu/dists/noble/main/binary-amd64/Packages",
			"main/binary-amd64/Packages"},
		{"/dists/stable", "/dists/stable/Release", "Release"},
		{"/ubuntu/dists/noble", "/ubuntu/dists/noble", "/ubuntu/dists/noble"}, // exact match — no prefix to trim
		{"/ubuntu/dists/noble", "/other/path", "/other/path"},                 // unrelated path
	}
	for _, tc := range cases {
		got := suiteRelativePath(tc.suitePath, tc.fullPath)
		if got != tc.want {
			t.Errorf("suiteRelativePath(%q, %q) = %q, want %q",
				tc.suitePath, tc.fullPath, got, tc.want)
		}
	}
}

// TestServeHTTP_SnapshotHit_ServesFromSnapshotMember covers the §6.1
// happy path: an adopted suite serves metadata via snapshot_member, with
// X-Cache-Snapshot identifying the snapshot id.
func TestServeHTTP_SnapshotHit_ServesFromSnapshotMember(t *testing.T) {
	h := newTestHandler(t, nil, nil)

	// Seed a blob containing fake InRelease bytes, register a snapshot
	// at that blob, and make it the current snapshot for the suite.
	body := []byte("InRelease snapshot bytes")
	bodyHash := writeBlob(t, h, body)
	const (
		scheme = "http"
		host   = "127.0.0.1"
		suite  = "/ubuntu/dists/noble"
	)
	snapID := commitInlineSnapshot(t, h, scheme, host, suite, bodyHash, []cache.SnapshotMember{
		{Path: "InRelease", BlobHash: bodyHash, DeclaredSHA256: bodyHash},
	}, nil)

	rec := httptest.NewRecorder()
	r := newProxyReqHostPort("GET", scheme, host, "/ubuntu/dists/noble/InRelease")
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Cache"); got != "HIT" {
		t.Errorf("X-Cache=%q, want HIT", got)
	}
	if got := rec.Header().Get("X-Cache-Snapshot"); got != strconv.FormatInt(snapID, 10) {
		t.Errorf("X-Cache-Snapshot=%q, want %d", got, snapID)
	}
	if rec.Body.String() != string(body) {
		t.Errorf("body=%q, want %q", rec.Body.String(), body)
	}
}

// TestServeHTTP_SnapshotMissReturns404 covers the §6.1 "snapshot is the
// contract" rule: once a suite is adopted, a path absent from the
// snapshot must 404 — even if a Phase 1 url_path row exists for it.
func TestServeHTTP_SnapshotMissReturns404(t *testing.T) {
	upstreamCalls := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Write([]byte("upstream content"))
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, nil)
	scheme, host, port := splitURL(t, srv.URL)
	suite := "/dists/noble"

	// Adopt a snapshot containing only "InRelease".
	releaseBlob := writeBlob(t, h, []byte("real InRelease"))
	commitInlineSnapshot(t, h, scheme, host, suite, releaseBlob, []cache.SnapshotMember{
		{Path: "InRelease", BlobHash: releaseBlob, DeclaredSHA256: releaseBlob},
	}, nil)

	// Seed a Phase 1 url_path row for an unrelated metadata path under
	// the same suite. SPEC2 §6.1 says this row must NOT be allowed to
	// satisfy a request once the suite is adopted.
	priorBlob := writeBlob(t, h, []byte("phase1 stale Packages"))
	if err := h.cache.PutURLPath(context.Background(), cache.URLPath{
		CanonicalScheme: scheme,
		CanonicalHost:   hostKey(host, port),
		Path:            "/dists/noble/main/binary-amd64/Packages",
		BlobHash:        &priorBlob,
		UpstreamURL:     fmt.Sprintf("%s://%s%s/dists/noble/main/binary-amd64/Packages", scheme, host, port),
		IsMetadata:      true,
	}); err != nil {
		t.Fatalf("PutURLPath: %v", err)
	}

	// Request a path NOT in the snapshot.
	rec := httptest.NewRecorder()
	r := newProxyReqHostPort("GET", scheme, host+port, "/dists/noble/main/binary-amd64/Packages")
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d body=%q, want 404 (snapshot is the contract)",
			rec.Code, rec.Body.String())
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Errorf("upstream calls=%d, want 0 (snapshot 404 must not trigger upstream fetch)", got)
	}
}

// TestServeHTTP_PreSnapshotSuiteUsesURLPath verifies that a suite with a
// suite_freshness row but current_snapshot_id = NULL still uses the
// Phase 1 url_path lookup. This is the v1→v2 backward-compatible path.
func TestServeHTTP_PreSnapshotSuiteUsesURLPath(t *testing.T) {
	body := []byte("Phase 1 InRelease bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, nil)

	// First request: Phase 1 miss-fetch creates url_path + suite_freshness
	// rows. current_snapshot_id remains NULL (no adoption yet).
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, proxyReq("GET", srv.URL, "/dists/noble/InRelease"))
	if rec1.Code != http.StatusOK {
		t.Fatalf("miss: status=%d body=%q", rec1.Code, rec1.Body.String())
	}

	// Verify the suite_freshness row exists with NULL current_snapshot_id.
	scheme, host, port := splitURL(t, srv.URL)
	sf, err := h.cache.GetSuiteFreshness(context.Background(),
		scheme, hostKey(host, port), "/dists/noble")
	if err != nil {
		t.Fatalf("GetSuiteFreshness: %v", err)
	}
	if sf.CurrentSnapshotID != nil {
		t.Fatalf("expected NULL current_snapshot_id (no adoption); got %v",
			sf.CurrentSnapshotID)
	}

	// Second request: hits via url_path. X-Cache-Snapshot must be absent.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, proxyReq("GET", srv.URL, "/dists/noble/InRelease"))
	if rec2.Code != http.StatusOK {
		t.Fatalf("hit: status=%d", rec2.Code)
	}
	if got := rec2.Header().Get("X-Cache"); got != "HIT" {
		t.Errorf("X-Cache=%q, want HIT", got)
	}
	if got := rec2.Header().Get("X-Cache-Snapshot"); got != "" {
		t.Errorf("X-Cache-Snapshot=%q, want empty (no snapshot adopted)", got)
	}
}

// TestServeHTTP_DebHitNoPackageHashServesNormally covers the §6.1 step 3
// case: a .deb hit with zero declared rows is the Phase 1 trust-upstream
// regime — serve normally with X-Cache: HIT and no X-Cache-Snapshot.
func TestServeHTTP_DebHitNoPackageHashServesNormally(t *testing.T) {
	body := []byte("a fake .deb")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, nil)

	// Prime cache with miss-fetch.
	primer := httptest.NewRecorder()
	h.ServeHTTP(primer, proxyReq("GET", srv.URL, "/pool/main/h/hello/hello.deb"))
	if primer.Code != http.StatusOK {
		t.Fatalf("primer: %d", primer.Code)
	}

	// Hit serves cleanly; no package_hash row exists for this path.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/pool/main/h/hello/hello.deb"))
	if rec.Code != http.StatusOK {
		t.Fatalf("hit: status=%d", rec.Code)
	}
	if got := rec.Header().Get("X-Cache"); got != "HIT" {
		t.Errorf("X-Cache=%q, want HIT", got)
	}
	if got := rec.Header().Get("X-Cache-Snapshot"); got != "" {
		t.Errorf("X-Cache-Snapshot must be absent for non-snapshot serve, got %q", got)
	}
}

// TestServeHTTP_DebHitMatchingPackageHashServes covers §6.1 step 4: a
// .deb hit whose url_path.blob_hash matches the snapshot's declared
// hash serves the cached blob normally.
func TestServeHTTP_DebHitMatchingPackageHashServes(t *testing.T) {
	body := []byte("matching .deb bytes")
	bodyHash := sha256Hex(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, nil)
	scheme, host, port := splitURL(t, srv.URL)
	suite := "/dists/noble"
	debPath := "/pool/main/h/hello/hello.deb"
	canonHost := hostKey(host, port)

	// Prime cache with miss-fetch.
	primer := httptest.NewRecorder()
	h.ServeHTTP(primer, proxyReq("GET", srv.URL, debPath))
	if primer.Code != http.StatusOK {
		t.Fatalf("primer: %d", primer.Code)
	}

	// Adopt a snapshot whose package_hash row declares bodyHash for
	// this .deb path. DeclaredHashesForPath will match url_path.
	releaseBlob := writeBlob(t, h, []byte("InRelease for noble"))
	commitInlineSnapshot(t, h, scheme, canonHost, suite, releaseBlob,
		[]cache.SnapshotMember{
			{Path: "InRelease", BlobHash: releaseBlob, DeclaredSHA256: releaseBlob},
		},
		[]cache.PackageHash{
			{
				CanonicalScheme: scheme,
				CanonicalHost:   canonHost,
				Path:            debPath,
				DeclaredSHA256:  bodyHash,
			},
		})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, debPath))
	if rec.Code != http.StatusOK {
		t.Fatalf("hit: status=%d body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Cache"); got != "HIT" {
		t.Errorf("X-Cache=%q, want HIT", got)
	}
	// .deb hits via url_path don't emit X-Cache-Snapshot — the header
	// is reserved for snapshot_member-resolved metadata.
	if got := rec.Header().Get("X-Cache-Snapshot"); got != "" {
		t.Errorf("X-Cache-Snapshot=%q, want empty (.deb hits via url_path)", got)
	}
}

// TestServeHTTP_DebHitMismatchingPackageHashEvictsAndRefetchFailsClosed
// covers §6.1 step 5 + §6.2 .deb miss-path validation jointly: the
// hit-path eviction succeeds, but the subsequent miss-path re-fetch
// still receives the wrong bytes from upstream and must fail closed
// with 502 + Retry-After: 60 (ErrPackageHashMismatch). Without the §6.2
// validation, the bad bytes would be cached and served (the unsafe
// behavior codex flagged).
func TestServeHTTP_DebHitMismatchingPackageHashEvictsAndRefetchFailsClosed(t *testing.T) {
	body := []byte("upstream .deb bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, nil)
	scheme, host, port := splitURL(t, srv.URL)
	suite := "/dists/noble"
	debPath := "/pool/main/h/hello/hello.deb"
	canonHost := hostKey(host, port)

	// Prime cache with miss-fetch — url_path now points to sha(body).
	primer := httptest.NewRecorder()
	h.ServeHTTP(primer, proxyReq("GET", srv.URL, debPath))
	if primer.Code != http.StatusOK {
		t.Fatalf("primer: %d", primer.Code)
	}
	preRow, err := h.cache.LookupURL(context.Background(), scheme, canonHost, debPath)
	if err != nil {
		t.Fatalf("LookupURL pre-evict: %v", err)
	}
	if preRow.BlobHash == nil || *preRow.BlobHash == "" {
		t.Fatalf("expected blob_hash on pre-evict row")
	}

	// Seed a snapshot whose package_hash declares a DIFFERENT hash for
	// the same .deb path. The mismatch fires the §6.1 step-5 eviction.
	releaseBlob := writeBlob(t, h, []byte("InRelease for noble"))
	wrongHash := strings.Repeat("d", 64) // never matches actual content
	commitInlineSnapshot(t, h, scheme, canonHost, suite, releaseBlob,
		[]cache.SnapshotMember{
			{Path: "InRelease", BlobHash: releaseBlob, DeclaredSHA256: releaseBlob},
		},
		[]cache.PackageHash{
			{
				CanonicalScheme: scheme,
				CanonicalHost:   canonHost,
				Path:            debPath,
				DeclaredSHA256:  wrongHash,
			},
		})

	// First post-snapshot request: §6.1 step 5 evicts the stale row
	// and the miss path re-fetches; the re-fetched bytes still hash to
	// the original (the upstream is fixed in this test), but they don't
	// match the snapshot's declared hash. §6.2 fails closed: 502 + 60s.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, debPath))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("post-evict re-fetch: status=%d, want 502 (§6.2 fail-closed)", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After=%q, want 60", got)
	}

	// url_path was evicted and the re-fetch did NOT re-insert (because
	// validation rejected). LookupURL should now report ErrNotFound.
	if _, err := h.cache.LookupURL(context.Background(), scheme, canonHost, debPath); err == nil {
		t.Errorf("url_path row was re-inserted after validation failure (should remain evicted)")
	}
}

// TestServeHTTP_DebMissValidatesAgainstPackageHash covers §6.2 in
// isolation: a fresh fetch (no prior url_path row) for a .deb that
// disagrees with the snapshot's declared hash must 502 without ever
// inserting blob/url_path rows.
func TestServeHTTP_DebMissValidatesAgainstPackageHash(t *testing.T) {
	body := []byte("upstream .deb bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, nil)
	scheme, host, port := splitURL(t, srv.URL)
	debPath := "/pool/main/h/hello/hello.deb"
	canonHost := hostKey(host, port)

	// Seed a covering snapshot with the wrong declared hash, BEFORE any
	// fetch. The first request goes straight to the miss path and must
	// fail validation.
	releaseBlob := writeBlob(t, h, []byte("InRelease for noble"))
	wrongHash := strings.Repeat("e", 64)
	commitInlineSnapshot(t, h, scheme, canonHost, "/dists/noble", releaseBlob,
		[]cache.SnapshotMember{
			{Path: "InRelease", BlobHash: releaseBlob, DeclaredSHA256: releaseBlob},
		},
		[]cache.PackageHash{
			{CanonicalScheme: scheme, CanonicalHost: canonHost,
				Path: debPath, DeclaredSHA256: wrongHash},
		})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, debPath))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status=%d, want 502 (§6.2 hash validation)", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After=%q, want 60", got)
	}
	if _, err := h.cache.LookupURL(context.Background(), scheme, canonHost, debPath); err == nil {
		t.Errorf("url_path row inserted after validation failure (must not insert)")
	}
}

// TestServeHTTP_DebMissConflictingPackageHashFailsClosed covers §6.2
// step 4: two current snapshots with distinct declared hashes for the
// same .deb path. Even on a fresh fetch (no prior url_path row), the
// miss path must 502 + Retry-After: 60 without inserting.
//
// Pre-fetch reject: the handler refuses without contacting upstream —
// the conflict is at the snapshot layer and no observed bytes can
// resolve it, so dialing upstream just wastes bandwidth before the
// same fail-closed conclusion. upstreamCalls=0 locks that contract.
func TestServeHTTP_DebMissConflictingPackageHashFailsClosed(t *testing.T) {
	upstreamCalls := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Write([]byte("upstream content"))
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, nil)
	scheme, host, port := splitURL(t, srv.URL)
	debPath := "/pool/main/h/hello/hello.deb"
	canonHost := hostKey(host, port)

	rA := writeBlob(t, h, []byte("InRelease A"))
	rB := writeBlob(t, h, []byte("InRelease B"))
	commitInlineSnapshot(t, h, scheme, canonHost, "/dists/A", rA,
		[]cache.SnapshotMember{{Path: "InRelease", BlobHash: rA, DeclaredSHA256: rA}},
		[]cache.PackageHash{{CanonicalScheme: scheme, CanonicalHost: canonHost,
			Path: debPath, DeclaredSHA256: strings.Repeat("a", 64)}})
	commitInlineSnapshot(t, h, scheme, canonHost, "/dists/B", rB,
		[]cache.SnapshotMember{{Path: "InRelease", BlobHash: rB, DeclaredSHA256: rB}},
		[]cache.PackageHash{{CanonicalScheme: scheme, CanonicalHost: canonHost,
			Path: debPath, DeclaredSHA256: strings.Repeat("b", 64)}})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, debPath))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status=%d, want 502 (snapshot conflict)", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After=%q, want 60", got)
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Errorf("upstream calls=%d, want 0 (snapshot-level conflict rejects pre-fetch)", got)
	}
}

// TestServeHTTP_DebMissHashCollisionPreservesUnrelatedBlob covers the
// codex-flagged blob-collision risk in the .deb miss path: a fetched
// .deb whose hash happens to match an unrelated cached blob (mirror
// confusion, misrouted Remap rule, content-identical .deb at a
// different path) must not evict the unrelated blob.
//
// The pre-codex flow Finalize'd into pool/<observed>, then on hash
// mismatch called DiscardFinalizedBlob(observed) — but Finalize's
// dedup branch had already preserved the existing pool/<observed>
// rather than overwriting it, so DiscardFinalizedBlob removed the
// unrelated valid blob. FinalizeExpectingHash sidesteps the race by
// gating the rename on the declared hash before pool/ is touched.
//
// Setup: pre-existing pool blob X with content B (an unrelated valid
// blob). Snapshot declares Y (≠ X) for path P. Upstream serves B
// (hash collision). Expectation: 502 + Retry-After: 60, pool/X is
// preserved with content B.
func TestServeHTTP_DebMissHashCollisionPreservesUnrelatedBlob(t *testing.T) {
	body := []byte("colliding bytes that match an unrelated cached blob")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, nil)
	scheme, host, port := splitURL(t, srv.URL)
	debPath := "/pool/main/h/hello/hello.deb"
	canonHost := hostKey(host, port)

	// Pre-existing pool blob: an unrelated valid blob with the same
	// content the upstream will serve, simulating a collision.
	collidingHash := writeBlob(t, h, body)
	collidingPath := h.cache.BlobPath(collidingHash)
	collidingStat0, err := os.Stat(collidingPath)
	if err != nil {
		t.Fatalf("stat pre-existing blob: %v", err)
	}

	// Snapshot declares a different hash for debPath. Upstream's
	// response will hash to collidingHash, not the declared value.
	declaredHash := strings.Repeat("e", 64)
	if declaredHash == collidingHash {
		t.Fatalf("test hash collision: declared %q matches collidingHash", declaredHash)
	}
	releaseBlob := writeBlob(t, h, []byte("InRelease for noble"))
	commitInlineSnapshot(t, h, scheme, canonHost, "/dists/noble", releaseBlob,
		[]cache.SnapshotMember{
			{Path: "InRelease", BlobHash: releaseBlob, DeclaredSHA256: releaseBlob},
		},
		[]cache.PackageHash{
			{CanonicalScheme: scheme, CanonicalHost: canonHost,
				Path: debPath, DeclaredSHA256: declaredHash},
		})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, debPath))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status=%d, want 502 (hash mismatch)", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After=%q, want 60", got)
	}

	// The unrelated valid pool blob must still be intact: same path,
	// same size, same content.
	collidingStat1, err := os.Stat(collidingPath)
	if err != nil {
		t.Fatalf("pool blob removed by mismatch handling: %v "+
			"(this is the pre-codex bug — DiscardFinalizedBlob "+
			"evicted the unrelated valid blob)", err)
	}
	if collidingStat0.Size() != collidingStat1.Size() {
		t.Errorf("pool blob size changed: was %d, now %d",
			collidingStat0.Size(), collidingStat1.Size())
	}
	gotContent, err := os.ReadFile(collidingPath)
	if err != nil {
		t.Fatalf("read pool blob: %v", err)
	}
	if string(gotContent) != string(body) {
		t.Errorf("pool blob content corrupted: got %q, want %q",
			gotContent, body)
	}
	// And no url_path row was inserted for the rejected fetch.
	if _, err := h.cache.LookupURL(context.Background(), scheme, canonHost, debPath); err == nil {
		t.Errorf("url_path row inserted after collision rejection (must not insert)")
	}
}

// TestServeHTTP_DebMissAdoptionFlipMidFetchUsesPostFetchContract covers
// the codex finding from review of 24d002f: the §6.2 .deb hash check
// must use the post-fetch DeclaredHashesForPath result, not a pre-
// fetch snapshot. If adoption flips current_snapshot_id while the
// fetch is in flight, validating against the stale pre-fetch
// declaration would let bytes that match the OLD contract land in
// pool/ even though they no longer match any current snapshot's
// contract.
//
// Setup: the upstream HTTP handler commits a NEW snapshot for the
// suite (replacing the current_snapshot_id pointer) before returning
// the body. Pre-fetch declared = bodyHash (matches the bytes upstream
// will serve). Post-fetch declared = a different hash (the new
// snapshot's declaration).
//
//   - With pre-fetch validation: bytes match pre-fetch declared,
//     promote → BUG.
//   - With post-fetch validation (this commit): bytes don't match the
//     post-fetch declared, fail closed → correct.
func TestServeHTTP_DebMissAdoptionFlipMidFetchUsesPostFetchContract(t *testing.T) {
	body := []byte("v1 .deb bytes — match the pre-fetch declared hash")
	bodyHash := sha256Hex(body)
	newDeclared := strings.Repeat("b", 64)
	if newDeclared == bodyHash {
		t.Fatalf("test setup: newDeclared %q collides with bodyHash", newDeclared)
	}

	const (
		suite   = "/dists/noble"
		debPath = "/pool/main/h/hello/hello.deb"
	)

	var (
		hCapture            *Handler
		schemeC, canonHostC string
		flipDone            atomic.Bool
		upstreamCalls       atomic.Int32
		flipFailed          atomic.Bool
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Mid-fetch adoption flip: install a new snapshot whose
		// package_hash row for debPath declares newDeclared. After
		// CommitAdoption, suite_freshness.current_snapshot_id points
		// at the new candidate, so DeclaredHashesForPath now returns
		// newDeclared (and only newDeclared) for this path.
		if !flipDone.Load() && hCapture != nil {
			defer flipDone.Store(true)
			func() {
				defer func() {
					if r := recover(); r != nil {
						flipFailed.Store(true)
					}
				}()
				newReleaseBlob := writeBlob(t, hCapture, []byte("InRelease v2"))
				_ = commitInlineSnapshot(t, hCapture, schemeC, canonHostC, suite, newReleaseBlob,
					[]cache.SnapshotMember{
						{Path: "InRelease", BlobHash: newReleaseBlob, DeclaredSHA256: newReleaseBlob},
					},
					[]cache.PackageHash{
						{CanonicalScheme: schemeC, CanonicalHost: canonHostC,
							Path: debPath, DeclaredSHA256: newDeclared},
					})
			}()
		}
		upstreamCalls.Add(1)
		w.Write(body)
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, nil)
	hCapture = h
	scheme, host, port := splitURL(t, srv.URL)
	canonHost := hostKey(host, port)
	schemeC, canonHostC = scheme, canonHost

	// Pre-fetch state: snapshot S1 declares bodyHash for debPath.
	// If pre-fetch validation were authoritative, the request would
	// succeed (bytes hash to bodyHash, declaration is bodyHash).
	releaseBlob := writeBlob(t, h, []byte("InRelease v1"))
	commitInlineSnapshot(t, h, scheme, canonHost, suite, releaseBlob,
		[]cache.SnapshotMember{
			{Path: "InRelease", BlobHash: releaseBlob, DeclaredSHA256: releaseBlob},
		},
		[]cache.PackageHash{
			{CanonicalScheme: scheme, CanonicalHost: canonHost,
				Path: debPath, DeclaredSHA256: bodyHash},
		})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, debPath))

	if flipFailed.Load() {
		t.Fatalf("mid-fetch adoption flip raised a fatal in commitInlineSnapshot")
	}
	if !flipDone.Load() {
		t.Fatalf("mid-fetch flip did not run (upstream not called)")
	}

	// Post-fetch authoritative re-query sees newDeclared. Body hashes
	// to bodyHash ≠ newDeclared → fail closed.
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status=%d, want 502 (post-fetch declared changed mid-fetch)", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After=%q, want 60", got)
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Errorf("upstream calls=%d, want 1 (single fetch, post-fetch fail-closed)", got)
	}
	if _, err := h.cache.LookupURL(context.Background(), scheme, canonHost, debPath); err == nil {
		t.Errorf("url_path row inserted after post-fetch validation failure (must not insert)")
	}

	// Post-fetch declaration is newDeclared. Subsequent fetch (with
	// upstream still serving body=bodyHash) keeps failing closed —
	// no bug-window in which the wrong bytes survive.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, proxyReq("GET", srv.URL, debPath))
	if rec2.Code != http.StatusBadGateway {
		t.Errorf("retry status=%d, want 502 (still mismatching newDeclared)", rec2.Code)
	}
}

// TestServeHTTP_AdoptedSuiteSnapshotMemberDBError_FailsClosed covers
// the codex finding: a DB error on snapshot_member lookup for an
// adopted suite must NOT fall through to serveCacheMiss. We trigger
// the error by closing the cache mid-flight; subsequent reads return
// "database is closed". The handler must produce 502 + Retry-After: 30,
// not a phantom unverified upstream fetch.
//
// Closes the cache before the request fires; this leaves the handler
// holding a closed *cache.Cache. ServeHTTP doesn't spin up new fetches
// (no upstream is needed for this test), but the assertion "no fall-
// through to miss path" is the load-bearing one.
func TestServeHTTP_AdoptedSuiteSnapshotMemberDBError_FailsClosed(t *testing.T) {
	upstreamCalls := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Write([]byte("attacker bytes"))
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, nil)
	scheme, host, port := splitURL(t, srv.URL)
	canonHost := hostKey(host, port)

	// Adopt a snapshot, then close the cache so subsequent reads error.
	releaseBlob := writeBlob(t, h, []byte("real InRelease"))
	commitInlineSnapshot(t, h, scheme, canonHost, "/dists/noble", releaseBlob,
		[]cache.SnapshotMember{
			{Path: "InRelease", BlobHash: releaseBlob, DeclaredSHA256: releaseBlob},
		}, nil)

	// Close the cache. The handler still holds the *Cache pointer; the
	// closed underlying *sql.DB will return "database is closed" on
	// any further query. This simulates the "DB error on snapshot
	// lookup for an adopted suite" code path.
	if err := h.cache.Close(); err != nil {
		t.Fatalf("close cache: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/dists/noble/InRelease"))

	// Either 502 (snapshot lookup failed → fail-closed) or 502 from
	// fall-through to the miss-path that also fails on DB writes is
	// acceptable; what is NOT acceptable is a 200 with upstream bytes.
	if rec.Code == http.StatusOK {
		t.Errorf("status=200 — adopted suite served unverified bytes; got body=%q",
			rec.Body.String())
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Errorf("upstream calls=%d, want 0 (no fetch on adopted-suite DB error)", got)
	}
}

// TestServeHTTP_AdoptedSuiteMissingBlob_RefetchMatchServes covers the
// SPEC2 §6.2 metadata recovery happy path: snapshot_member row points
// at a blob whose pool/<blob> file has been removed (operator
// deletion, integrity-scanner corruption removal). A re-fetch from
// upstream returns the bytes that hash to declared_sha256, the cache
// validates the match, persists the file back into pool/, and serves
// it with X-Cache: MISS + X-Cache-Snapshot.
func TestServeHTTP_AdoptedSuiteMissingBlob_RefetchMatchServes(t *testing.T) {
	body := []byte("real InRelease")
	upstreamCalls := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Write(body)
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, nil)
	scheme, host, port := splitURL(t, srv.URL)
	canonHost := hostKey(host, port)
	suite := "/dists/noble"

	// Adopt a snapshot whose declared_sha256 matches body's hash.
	releaseBlob := writeBlob(t, h, body)
	snapID := commitInlineSnapshot(t, h, scheme, canonHost, suite, releaseBlob,
		[]cache.SnapshotMember{
			{Path: "InRelease", BlobHash: releaseBlob, DeclaredSHA256: releaseBlob},
		}, nil)

	// Remove the pool file (simulating at-rest scanner removal).
	if err := os.Remove(h.cache.BlobPath(releaseBlob)); err != nil {
		t.Fatalf("remove blob: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/dists/noble/InRelease"))

	if rec.Code != http.StatusOK {
		t.Errorf("status=%d, want 200 (recovery served); body=%q",
			rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != string(body) {
		t.Errorf("body=%q, want %q", got, body)
	}
	if got := rec.Header().Get("X-Cache"); got != "MISS" {
		t.Errorf("X-Cache=%q, want MISS (post-recovery fetch)", got)
	}
	if got := rec.Header().Get("X-Cache-Snapshot"); got != itoa(snapID) {
		t.Errorf("X-Cache-Snapshot=%q, want %d", got, snapID)
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Errorf("upstream calls=%d, want 1 (one recovery fetch)", got)
	}
	if _, err := os.Stat(h.cache.BlobPath(releaseBlob)); err != nil {
		t.Errorf("expected pool blob to be re-created post-recovery: %v", err)
	}
}

// TestServeHTTP_AdoptedSuiteMissingBlob_RefetchMismatchFailsClosed
// covers the recovery-mismatch surface: the pool blob is gone, the
// upstream is now serving content whose sha256 does not match the
// snapshot's declared hash (upstream rolled forward). The handler
// must not insert the bogus bytes; it returns 502 + Retry-After: 30
// and the next adoption flips us forward.
func TestServeHTTP_AdoptedSuiteMissingBlob_RefetchMismatchFailsClosed(t *testing.T) {
	upstreamCalls := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Write([]byte("attacker-supplied bytes"))
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, nil)
	scheme, host, port := splitURL(t, srv.URL)
	canonHost := hostKey(host, port)
	suite := "/dists/noble"

	// declared_sha256 is the hash of "real InRelease"; upstream returns
	// different bytes.
	releaseBlob := writeBlob(t, h, []byte("real InRelease"))
	commitInlineSnapshot(t, h, scheme, canonHost, suite, releaseBlob,
		[]cache.SnapshotMember{
			{Path: "InRelease", BlobHash: releaseBlob, DeclaredSHA256: releaseBlob},
		}, nil)

	if err := os.Remove(h.cache.BlobPath(releaseBlob)); err != nil {
		t.Fatalf("remove blob: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/dists/noble/InRelease"))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status=%d, want 502 (recovery hash mismatch)", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After=%q, want 30", got)
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Errorf("upstream calls=%d, want 1 (one recovery attempt)", got)
	}
	if _, err := os.Stat(h.cache.BlobPath(releaseBlob)); err == nil {
		t.Errorf("expected mismatched pool blob to be discarded; file remains")
	}
}

// TestServeHTTP_AdoptedSuiteMissingBlob_RefetchUpstream404FailsClosed
// covers codex finding 1: respondError would have routed an upstream
// 4xx as a passthrough status, which for an adopted snapshot member
// would let upstream "absence" override the snapshot's vouched
// "presence". The recovery responder maps every fetch error to
// 502 + Retry-After: 30; the upstream's 4xx is captured in
// X-Upstream-Status for diagnostics but does not become the wire
// status.
func TestServeHTTP_AdoptedSuiteMissingBlob_RefetchUpstream404FailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, nil)
	scheme, host, port := splitURL(t, srv.URL)
	canonHost := hostKey(host, port)
	suite := "/dists/noble"
	releaseBlob := writeBlob(t, h, []byte("real InRelease"))
	commitInlineSnapshot(t, h, scheme, canonHost, suite, releaseBlob,
		[]cache.SnapshotMember{
			{Path: "InRelease", BlobHash: releaseBlob, DeclaredSHA256: releaseBlob},
		}, nil)
	if err := os.Remove(h.cache.BlobPath(releaseBlob)); err != nil {
		t.Fatalf("remove blob: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/dists/noble/InRelease"))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status=%d, want 502 (upstream 404 must not pass through during recovery)", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After=%q, want 30", got)
	}
	if got := rec.Header().Get("X-Upstream-Status"); got != "404" {
		t.Errorf("X-Upstream-Status=%q, want 404 (diagnostic preserved)", got)
	}
}

// TestServeHTTP_AdoptedSuiteMissingBlob_StaleNotServed covers codex
// finding 1, second leg: respondError's upstream-unreachable path
// can serve HIT-STALE from url_path. For adopted-suite metadata
// recovery, the snapshot is the contract — a Phase 1 url_path row
// (even one that exists) must not satisfy the request when the snapshot
// pointer is set. Recovery responder always 502s on upstream-down,
// regardless of url_path state.
func TestServeHTTP_AdoptedSuiteMissingBlob_StaleNotServed(t *testing.T) {
	// Upstream is just closed; any fetch fails with connection-refused.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()

	h := newTestHandler(t, nil, nil)
	scheme, host, port := splitURL(t, srv.URL)
	canonHost := hostKey(host, port)
	suite := "/dists/noble"
	releaseBlob := writeBlob(t, h, []byte("real InRelease"))
	commitInlineSnapshot(t, h, scheme, canonHost, suite, releaseBlob,
		[]cache.SnapshotMember{
			{Path: "InRelease", BlobHash: releaseBlob, DeclaredSHA256: releaseBlob},
		}, nil)

	// Seed a stale url_path row for the same path. respondError's
	// upstream-unreachable path would otherwise serve this; the
	// recovery responder must not.
	stale := writeBlob(t, h, []byte("stale Phase 1 InRelease"))
	now := nowUnixForTest()
	if err := h.cache.PutURLPath(context.Background(), cache.URLPath{
		CanonicalScheme: scheme, CanonicalHost: canonHost,
		Path:        "/dists/noble/InRelease",
		BlobHash:    &stale,
		UpstreamURL: srv.URL + "/dists/noble/InRelease",
		IsMetadata:  true, LastFetchedAt: &now, RequestCount: 1,
	}); err != nil {
		t.Fatalf("PutURLPath stale: %v", err)
	}

	if err := os.Remove(h.cache.BlobPath(releaseBlob)); err != nil {
		t.Fatalf("remove snapshot blob: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/dists/noble/InRelease"))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status=%d, want 502 (must not stale-serve url_path during recovery)", rec.Code)
	}
	if rec.Body.String() == "stale Phase 1 InRelease" {
		t.Errorf("body returned stale Phase 1 bytes: %q", rec.Body.String())
	}
	if got := rec.Header().Get("X-Cache"); got == "HIT-STALE" {
		t.Errorf("X-Cache=%q (HIT-STALE), recovery must not stale-serve", got)
	}
}

// TestServeHTTP_AdoptedSuiteMissingBlob_MismatchPreservesUnrelatedBlob
// covers codex finding 3: when recovery's upstream returns bytes
// whose sha256 happens to equal an existing pool blob's hash (real
// world: misrouted Remap, mirror confusion), the post-Finalize
// DiscardFinalizedBlob would have removed the unrelated valid blob.
// FinalizeExpectingHash now gates on the declared hash before
// rename-or-dedup, so the unrelated blob's pool file is preserved.
func TestServeHTTP_AdoptedSuiteMissingBlob_MismatchPreservesUnrelatedBlob(t *testing.T) {
	collidingContent := []byte("collision-bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(collidingContent)
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, nil)
	scheme, host, port := splitURL(t, srv.URL)
	canonHost := hostKey(host, port)
	suite := "/dists/noble"

	// (a) Seed an unrelated, legitimate pool blob whose hash equals the
	// upstream's collision response. In the real-world bug scenario
	// this blob backs some other path's url_path or snapshot_member;
	// we just need it on disk.
	collidingBlob := writeBlob(t, h, collidingContent)
	collidingPath := h.cache.BlobPath(collidingBlob)
	if _, err := os.Stat(collidingPath); err != nil {
		t.Fatalf("expected colliding pool blob to exist: %v", err)
	}

	// (b) Set up the adopted snapshot whose declared_sha256 differs
	// from collidingBlob. Recovery will try to validate the upstream's
	// collision bytes against this declared hash — they don't match,
	// so the recovery 502s WITHOUT touching the colliding blob.
	releaseBlob := writeBlob(t, h, []byte("real InRelease"))
	commitInlineSnapshot(t, h, scheme, canonHost, suite, releaseBlob,
		[]cache.SnapshotMember{
			{Path: "InRelease", BlobHash: releaseBlob, DeclaredSHA256: releaseBlob},
		}, nil)
	// Remove the snapshot's pool blob to force recovery.
	if err := os.Remove(h.cache.BlobPath(releaseBlob)); err != nil {
		t.Fatalf("remove snapshot blob: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/dists/noble/InRelease"))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status=%d, want 502 (recovery hash mismatch)", rec.Code)
	}
	// The unrelated blob whose hash equals collidingContent's sha256
	// must still be present on disk.
	if _, err := os.Stat(collidingPath); err != nil {
		t.Errorf("collision blob removed by mismatched recovery: %v", err)
	}
}

// nowUnixForTest is a tiny helper to take a *int64 from a literal, used
// for seeding url_path rows that need LastFetchedAt set.
func nowUnixForTest() int64 { return 1735689600 } // 2025-01-01

// itoa is a small helper for X-Cache-Snapshot assertions, mirroring
// the integrity_test.go variant.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		pos--
		b[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

// TestServeHTTP_DebHitConflictingPackageHash502s covers §6.1 step 6: two
// or more distinct declared hashes for the same .deb path is fail-closed
// — 502 + Retry-After: 60 + log package_hash_conflict.
func TestServeHTTP_DebHitConflictingPackageHash502s(t *testing.T) {
	body := []byte("ambiguous .deb bytes")
	upstreamCalls := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Write(body)
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, nil)
	scheme, host, port := splitURL(t, srv.URL)
	debPath := "/pool/main/h/hello/hello.deb"
	canonHost := hostKey(host, port)

	// Prime url_path.
	primer := httptest.NewRecorder()
	h.ServeHTTP(primer, proxyReq("GET", srv.URL, debPath))
	if primer.Code != http.StatusOK {
		t.Fatalf("primer: %d", primer.Code)
	}
	primingCount := upstreamCalls.Load()

	// Two snapshots, two suites, each declaring a DIFFERENT hash for
	// the same .deb path. Both are current → DeclaredHashesForPath
	// returns 2 distinct hashes → conflict.
	rA := writeBlob(t, h, []byte("InRelease A"))
	rB := writeBlob(t, h, []byte("InRelease B"))
	hashA := strings.Repeat("a", 64)
	hashB := strings.Repeat("b", 64)
	commitInlineSnapshot(t, h, scheme, canonHost, "/dists/suiteA", rA,
		[]cache.SnapshotMember{
			{Path: "InRelease", BlobHash: rA, DeclaredSHA256: rA},
		},
		[]cache.PackageHash{
			{CanonicalScheme: scheme, CanonicalHost: canonHost,
				Path: debPath, DeclaredSHA256: hashA},
		})
	commitInlineSnapshot(t, h, scheme, canonHost, "/dists/suiteB", rB,
		[]cache.SnapshotMember{
			{Path: "InRelease", BlobHash: rB, DeclaredSHA256: rB},
		},
		[]cache.PackageHash{
			{CanonicalScheme: scheme, CanonicalHost: canonHost,
				Path: debPath, DeclaredSHA256: hashB},
		})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, debPath))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status=%d, want 502 (snapshot disagreement)", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After=%q, want 60", got)
	}
	// No additional upstream call — fail-closed before fetch.
	if got := upstreamCalls.Load(); got != primingCount {
		t.Errorf("upstream calls after conflict=%d, want %d (no extra fetch)",
			got, primingCount)
	}
}

// TestServeHTTP_DebHitAgreeingDuplicateHashesServes covers a subtle case
// of step 4: two snapshots both declaring the *same* hash for the same
// path. Two rows, one distinct value — the declaredAttrs distinct count
// is 1 and the request serves normally rather than 502'ing.
func TestServeHTTP_DebHitAgreeingDuplicateHashesServes(t *testing.T) {
	body := []byte("agreed .deb bytes")
	bodyHash := sha256Hex(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, nil)
	scheme, host, port := splitURL(t, srv.URL)
	debPath := "/pool/main/h/hello/hello.deb"
	canonHost := hostKey(host, port)

	primer := httptest.NewRecorder()
	h.ServeHTTP(primer, proxyReq("GET", srv.URL, debPath))
	if primer.Code != http.StatusOK {
		t.Fatalf("primer: %d", primer.Code)
	}

	// Two suites, both declaring the SAME hash. distinctDeclared
	// collapses to one entry — match path serves cleanly.
	rA := writeBlob(t, h, []byte("InRelease X"))
	rB := writeBlob(t, h, []byte("InRelease Y"))
	commitInlineSnapshot(t, h, scheme, canonHost, "/dists/suiteX", rA,
		[]cache.SnapshotMember{
			{Path: "InRelease", BlobHash: rA, DeclaredSHA256: rA},
		},
		[]cache.PackageHash{
			{CanonicalScheme: scheme, CanonicalHost: canonHost,
				Path: debPath, DeclaredSHA256: bodyHash},
		})
	commitInlineSnapshot(t, h, scheme, canonHost, "/dists/suiteY", rB,
		[]cache.SnapshotMember{
			{Path: "InRelease", BlobHash: rB, DeclaredSHA256: rB},
		},
		[]cache.PackageHash{
			{CanonicalScheme: scheme, CanonicalHost: canonHost,
				Path: debPath, DeclaredSHA256: bodyHash},
		})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, debPath))
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d, want 200 (duplicate-but-agreeing rows)", rec.Code)
	}
	if got := rec.Header().Get("X-Cache"); got != "HIT" {
		t.Errorf("X-Cache=%q, want HIT", got)
	}
}

// TestDistinctDeclared_CollapsesDuplicates is a unit test for the helper
// that drives the §6.1 dispatch — the *count* of distinct declared
// hashes is what determines the outcome.
func TestDistinctDeclared_CollapsesDuplicates(t *testing.T) {
	rows := []cache.DeclaredHash{
		{DeclaredSHA256: "aaa", SnapshotID: 1},
		{DeclaredSHA256: "bbb", SnapshotID: 2},
		{DeclaredSHA256: "aaa", SnapshotID: 3},
	}
	got := distinctDeclared(rows)
	if len(got) != 2 {
		t.Errorf("distinctDeclared len=%d, want 2: %v", len(got), got)
	}
	if got[0] != "aaa" || got[1] != "bbb" {
		t.Errorf("distinctDeclared order = %v, want [aaa bbb]", got)
	}

	if got := distinctDeclared(nil); got != nil {
		t.Errorf("distinctDeclared(nil) = %v, want nil", got)
	}
	if got := distinctDeclared([]cache.DeclaredHash{}); got != nil {
		t.Errorf("distinctDeclared(empty) = %v, want nil", got)
	}
}

// --- helpers --------------------------------------------------------------

// writeBlob seeds a blob in the handler's cache and returns its sha256.
// The blob is created via the same NewTempBlob/Finalize/PutBlob sequence
// production code uses, so the on-disk pool layout is identical to a
// real fetch.
func writeBlob(t *testing.T, h *Handler, content []byte) string {
	t.Helper()
	w, err := h.cache.NewTempBlob()
	if err != nil {
		t.Fatalf("NewTempBlob: %v", err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatalf("Write: %v", err)
	}
	hash, err := w.Finalize(int64(len(content)))
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if err := h.cache.PutBlob(context.Background(), hash, int64(len(content))); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	return hash
}

// commitInlineSnapshot creates and adopts a fresh inline (InRelease)
// snapshot. The caller passes the snapshot's members and package_hash
// rows; the snapshot id and CommitAdoption errors bubble through t.Fatal.
// snapshot_id is returned for tests that assert on X-Cache-Snapshot.
//
// The inrelease_hash is the bytes the snapshot was built from. It is
// also expected to appear as a member at "InRelease" so §6.1 can resolve
// the InRelease URL through snapshot_member.
func commitInlineSnapshot(t *testing.T, h *Handler,
	scheme, canonHost, suite, inreleaseHash string,
	members []cache.SnapshotMember,
	pkgHashes []cache.PackageHash,
) int64 {
	t.Helper()
	// Seed an empty suite_freshness row first so the FK target exists.
	// CommitAdoption flips the pointer in-place. (The schema FK on
	// current_snapshot_id is to suite_snapshot, but the adoption
	// transaction expects a writable row to update.)
	if err := h.cache.PutSuiteFreshness(context.Background(), cache.SuiteFreshness{
		CanonicalScheme: scheme,
		CanonicalHost:   canonHost,
		SuitePath:       suite,
	}); err != nil {
		t.Fatalf("PutSuiteFreshness: %v", err)
	}
	id, err := h.cache.InsertCandidateSnapshot(context.Background(),
		cache.SnapshotCandidate{
			CanonicalScheme: scheme,
			CanonicalHost:   canonHost,
			SuitePath:       suite,
			InReleaseHash:   &inreleaseHash,
		})
	if err != nil {
		t.Fatalf("InsertCandidateSnapshot: %v", err)
	}
	for i := range members {
		members[i].SnapshotID = id
	}
	for i := range pkgHashes {
		pkgHashes[i].SnapshotID = id
	}
	if err := h.cache.CommitAdoption(context.Background(), id, members, pkgHashes); err != nil {
		t.Fatalf("CommitAdoption: %v", err)
	}
	return id
}

// splitURL pulls (scheme, host, ":port") out of an httptest.Server URL.
// host is bare (no port); port is ":NNN" or "" if the URL omitted one.
func splitURL(t *testing.T, raw string) (scheme, host, port string) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse %q: %v", raw, err)
	}
	scheme = u.Scheme
	if h, p, err := splitHostPortStrict(u.Host); err == nil {
		return scheme, h, ":" + p
	}
	return scheme, u.Host, ""
}

// splitHostPortStrict returns host, port (no leading colon) or an error.
// httptest.Server URLs always have a port, but be defensive in case a
// future test feeds a bare-host URL.
func splitHostPortStrict(authority string) (host, port string, err error) {
	idx := strings.LastIndex(authority, ":")
	if idx == -1 {
		return "", "", fmt.Errorf("no port in %q", authority)
	}
	return authority[:idx], authority[idx+1:], nil
}

// hostKey reproduces the canonical host the handler stores in the
// cache for an httptest.Server (bare host, port stripped) — the same
// transform proxy.canonicalize applies. Used so test assertions look
// up cache rows under the right key.
func hostKey(host, port string) string {
	_ = port
	return strings.ToLower(host)
}

// newProxyReqHostPort builds a proxy-mode request whose URL has scheme
// and host explicitly set. The standard proxyReq helper uses srv.URL
// which already includes the port; this helper is for tests that synth
// a snapshot before any upstream is needed.
func newProxyReqHostPort(method, scheme, hostPort, path string) *http.Request {
	return httptest.NewRequest(method, scheme+"://"+hostPort+path, nil)
}

// sha256Hex is a tiny convenience for fixture builders.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
