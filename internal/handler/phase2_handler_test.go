package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// TestServeHTTP_DebHitMismatchingPackageHashEvicts covers §6.1 step 5:
// a single declared hash that disagrees with url_path.blob_hash evicts
// the row, decrements refcount, logs hit_path_hash_evicted, and falls
// through to the miss path.
func TestServeHTTP_DebHitMismatchingPackageHashEvicts(t *testing.T) {
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
	originalBlobHash := *preRow.BlobHash

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

	// Hit triggers the mismatch path; eviction fires + miss re-fetches.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, debPath))
	if rec.Code != http.StatusOK {
		t.Fatalf("post-evict request: status=%d body=%q", rec.Code, rec.Body.String())
	}
	// The miss-path re-fetch sees the same upstream bytes, re-installs
	// the same url_path row. The response is X-Cache: MISS because the
	// eviction forced the request through serveCacheMiss.
	if got := rec.Header().Get("X-Cache"); got != "MISS" {
		t.Errorf("X-Cache=%q, want MISS (post-evict re-fetch)", got)
	}

	// url_path is repopulated; assert the row was evicted-and-rebuilt by
	// checking the blob still resolves to the same content (idempotent
	// re-fetch).
	postRow, err := h.cache.LookupURL(context.Background(), scheme, canonHost, debPath)
	if err != nil {
		t.Fatalf("LookupURL post-evict: %v", err)
	}
	if postRow.BlobHash == nil || *postRow.BlobHash != originalBlobHash {
		t.Errorf("post-evict blob hash = %v, want %s", postRow.BlobHash, originalBlobHash)
	}
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
