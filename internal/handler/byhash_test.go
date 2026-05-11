package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// SPEC2 §0 #4 by-hash content-addressed fallback. Pool/<hex> exists; no
// url_path or snapshot_member rows. The request /<repo>/dists/foo/main/
// by-hash/SHA256/<hex> must serve from pool/<hex> directly without
// contacting upstream.
func TestServeHTTP_ByHashContentAddressed_PoolHit(t *testing.T) {
	upstreamCalls := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		http.Error(w, "should not be reached", http.StatusInternalServerError)
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, nil)
	scheme, host, port := splitURL(t, srv.URL)

	body := []byte("by-hash Packages bytes")
	hash := writeBlob(t, h, body)

	// Provenance: stage a url_path row under the SAME host pointing
	// at this blob — represents a prior fetch via some other URL
	// (e.g. /dists/noble/main/binary-amd64/Packages.gz). The fallback
	// is gated on this same-host association so a cross-host SHA256
	// probe can't promiscuously serve pool blobs.
	priorPath := "/dists/noble/main/binary-amd64/Packages.gz"
	if err := h.cache.PutURLPath(context.Background(), cache.URLPath{
		CanonicalScheme: scheme,
		CanonicalHost:   hostKey(host, port),
		Path:            priorPath,
		BlobHash:        &hash,
		IsMetadata:      true,
	}); err != nil {
		t.Fatalf("PutURLPath: %v", err)
	}

	// No url_path row for the by-hash URL itself. No snapshot.
	path := "/dists/noble/main/binary-amd64/by-hash/SHA256/" + hash
	rec := httptest.NewRecorder()
	r := newProxyReqHostPort("GET", scheme, host+port, path)
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Cache"); got != "HIT" {
		t.Errorf("X-Cache=%q, want HIT", got)
	}
	// Content-addressed dedup is not a snapshot serve — header must be absent.
	if got := rec.Header().Get("X-Cache-Snapshot"); got != "" {
		t.Errorf("X-Cache-Snapshot=%q, want empty (content-addressed serve, no snapshot)", got)
	}
	if rec.Body.String() != string(body) {
		t.Errorf("body=%q, want %q", rec.Body.String(), body)
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Errorf("upstream calls=%d, want 0 (pool hit must not trigger fetch)", got)
	}

	// AIDEV-NOTE: the fallback path materializes a url_path row asynchronously.
	// The next request should hit tryURLPathHit instead of repeating the regex
	// match, but we verify by checking the row exists rather than racing on
	// the second request's classification.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		row, err := h.cache.LookupURL(context.Background(), scheme, hostKey(host, port), path)
		if err == nil && row != nil && row.BlobHash != nil && *row.BlobHash == hash {
			return // success
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("url_path row was not materialized after by-hash fallback serve")
}

// pool/<hex> missing → fallback returns false → falls through to
// serveCacheMiss which contacts upstream. Verifies the fallback does
// not produce false hits when the blob is genuinely absent.
func TestServeHTTP_ByHashContentAddressed_PoolMissFetchesUpstream(t *testing.T) {
	body := []byte("upstream supplied bytes")
	upstreamCalls := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, nil)
	scheme, host, port := splitURL(t, srv.URL)

	// Use a hash that doesn't correspond to any blob on disk. The
	// fallback must fail BlobExists and return false.
	bogusHash := strings.Repeat("0", 64)
	path := "/dists/noble/main/binary-amd64/by-hash/SHA256/" + bogusHash
	rec := httptest.NewRecorder()
	r := newProxyReqHostPort("GET", scheme, host+port, path)
	h.ServeHTTP(rec, r)

	// Fallback missed → upstream miss path served the bytes.
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != string(body) {
		t.Errorf("body=%q, want upstream bytes %q", rec.Body.String(), body)
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Errorf("upstream calls=%d, want 1 (pool miss must fetch)", got)
	}
}

// SPEC2 §6.1 contract preservation: an adopted suite must continue to
// route metadata through trySnapshotHit even when the request is a
// by-hash URL whose hash happens to exist in pool/. The fallback must
// NOT bypass the snapshot's "snapshot is the contract" rule.
func TestServeHTTP_ByHashContentAddressed_AdoptedSuiteUsesSnapshot(t *testing.T) {
	upstreamCalls := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		http.Error(w, "should not be reached", http.StatusInternalServerError)
	}))
	defer srv.Close()

	h := newTestHandler(t, nil, nil)
	scheme, host, port := splitURL(t, srv.URL)
	canonHost := hostKey(host, port)
	suite := "/dists/noble"

	// Adopt a snapshot containing only InRelease.
	releaseBlob := writeBlob(t, h, []byte("real InRelease"))
	commitInlineSnapshot(t, h, scheme, canonHost, suite, releaseBlob, []cache.SnapshotMember{
		{Path: "InRelease", BlobHash: releaseBlob, DeclaredSHA256: releaseBlob},
	}, nil)

	// Stage a stray pool blob whose hash is NOT a snapshot member.
	// Without the adopted-suite gate the by-hash fallback would happily
	// serve this — which would silently bypass the SPEC2 §6.1 contract.
	strayBody := []byte("stray pool bytes not vouched for")
	strayHash := writeBlob(t, h, strayBody)

	rec := httptest.NewRecorder()
	r := newProxyReqHostPort("GET", scheme, host+port,
		"/dists/noble/main/binary-amd64/by-hash/SHA256/"+strayHash)
	h.ServeHTTP(rec, r)

	// Adopted suite + path not in snapshot → 404 per §6.1.
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d body=%q, want 404 (adopted-suite contract must hold)",
			rec.Code, rec.Body.String())
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Errorf("upstream calls=%d, want 0 (snapshot 404 must not contact upstream)", got)
	}
}

// .deb / non-metadata requests are not by-hash in apt's wire protocol.
// Even if a request path happens to look like .../by-hash/SHA256/<hex>
// without being classified as metadata, the fallback must not fire.
// This test verifies the IsMetadata gate in tryCacheHit.
func TestServeHTTP_ByHashContentAddressed_NonMetadataIgnored(t *testing.T) {
	// The IsMetadata classifier in proxy/classify.go treats anything
	// containing "/by-hash/" as metadata, so a path matching the regex
	// is metadata by definition. This test instead verifies the deeper
	// invariant by constructing a non-by-hash path and confirming the
	// fallback does not match. (A full negative test for IsMetadata
	// belongs in the proxy package; here we just confirm the fallback
	// doesn't match arbitrary 64-hex segments outside the suffix form.)
	h := newTestHandler(t, nil, nil)
	body := []byte("blob bytes")
	_ = writeBlob(t, h, body) // Stage the blob; its hash should not be consulted.

	// pool/<hash> exists, but the request path is a regular pool URL,
	// not a by-hash suffix. The fallback regex must not match and the
	// request should miss-cache normally.
	upstreamCalls := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	scheme, host, port := splitURL(t, srv.URL)

	path := "/pool/main/h/hello/hello_2.10-3_amd64.deb"
	rec := httptest.NewRecorder()
	r := newProxyReqHostPort("GET", scheme, host+port, path)
	h.ServeHTTP(rec, r)

	// Status is fine either way; the assertion is that we did NOT short-
	// circuit through the fallback (regex doesn't match a .deb URL).
	if got := upstreamCalls.Load(); got != 1 {
		t.Errorf("upstream calls=%d, want 1 (non-by-hash path must not match fallback)", got)
	}
}

// Direct unit on the regex: trailing-only match, full 64-hex required,
// uppercase rejected (apt always lowercase-hexes).
func TestByHashSuffixRegex(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/dists/foo/main/by-hash/SHA256/" + strings.Repeat("a", 64), true},
		{"/dists/foo/by-hash/SHA256/" + strings.Repeat("0", 64), true},
		{"/by-hash/SHA256/" + strings.Repeat("f", 64), true},
		{"/dists/foo/main/by-hash/SHA256/short", false},
		{"/dists/foo/main/by-hash/SHA512/" + strings.Repeat("a", 128), false},
		{"/dists/foo/main/by-hash/SHA256/" + strings.Repeat("A", 64), false}, // uppercase rejected
		{"/dists/foo/main/by-hash/SHA256/" + strings.Repeat("a", 64) + "/extra", false},
		{"/regular/path.deb", false},
	}
	for _, tc := range cases {
		got := byHashSuffixRegex.MatchString(tc.path)
		if got != tc.want {
			t.Errorf("MatchString(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// Sanity: the cache LookupURL signature/behavior we depend on.
func TestServeHTTP_ByHashContentAddressed_LookupURLNotFoundShape(t *testing.T) {
	h := newTestHandler(t, nil, nil)
	_, err := h.cache.LookupURL(context.Background(), "http", "no.such.host", "/x")
	if !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("LookupURL on absent row: want ErrNotFound, got %v", err)
	}
}

// Security gate: a request under a host the allowlist does not cover
// must NOT serve from pool/<hex> via the by-hash fallback, even if the
// blob exists. Without this gate, a caller who learns a SHA256 from
// one cached host could request it under any unrelated host (passing
// the apt URL syntax check) and receive bytes — bypassing SPEC §6.6
// allowlist. Expected outcome: 403, identical to the existing
// allowlist rejection path for any other URL.
func TestServeHTTP_ByHashContentAddressed_DisallowedHostRejected(t *testing.T) {
	// Allow only 127.0.0.1; the by-hash request will use a different
	// hostname that does not match the regex.
	h := newTestHandler(t, []string{`^127\.0\.0\.1$`}, nil)
	body := []byte("private content")
	hash := writeBlob(t, h, body)

	// Stage a url_path row under the allowed host so HostHasBlob would
	// otherwise pass — proves the gate that fires is HostAllowed, not
	// the provenance check.
	if err := h.cache.PutURLPath(context.Background(), cache.URLPath{
		CanonicalScheme: "http",
		CanonicalHost:   "127.0.0.1",
		Path:            "/dists/foo/main/binary-amd64/Packages.gz",
		BlobHash:        &hash,
		IsMetadata:      true,
	}); err != nil {
		t.Fatalf("PutURLPath: %v", err)
	}

	rec := httptest.NewRecorder()
	r := newProxyReqHostPort("GET", "http", "evil.example.com",
		"/dists/foo/main/binary-amd64/by-hash/SHA256/"+hash)
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status=%d body=%q, want 403 (disallowed host must not bypass §6.6 via by-hash fallback)",
			rec.Code, rec.Body.String())
	}
	if rec.Body.String() == string(body) {
		t.Errorf("disallowed-host request leaked the cached bytes")
	}
}

// Security gate: same-host provenance. A blob in pool/<hex> that has
// NEVER been fetched under the requested host must not be served via
// the by-hash fallback. The dedup feature is for legitimate cross-
// SUITE / cross-URL hits within a single allowed host, not for
// promiscuous cross-HOST serving.
func TestServeHTTP_ByHashContentAddressed_RejectsCrossHostBlob(t *testing.T) {
	// Allow both hosts (so HostAllowed passes); the gate that fires is
	// HostHasBlob, not the allowlist.
	h := newTestHandler(t, []string{`^(host-a|host-b)\.example$`}, nil)

	body := []byte("blob fetched under host-a")
	hash := writeBlob(t, h, body)

	// Provenance: blob is associated with host-a only.
	if err := h.cache.PutURLPath(context.Background(), cache.URLPath{
		CanonicalScheme: "http",
		CanonicalHost:   "host-a.example",
		Path:            "/dists/foo/main/binary-amd64/Packages.gz",
		BlobHash:        &hash,
		IsMetadata:      true,
	}); err != nil {
		t.Fatalf("PutURLPath: %v", err)
	}

	// Serve attempt under host-b — same blob, different host. The
	// fallback must refuse and the request goes to the miss path.
	upstreamCalls := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		http.Error(w, "miss path probably can't reach here", http.StatusNotFound)
	}))
	defer srv.Close()
	_ = srv.URL

	rec := httptest.NewRecorder()
	r := newProxyReqHostPort("GET", "http", "host-b.example",
		"/dists/foo/main/binary-amd64/by-hash/SHA256/"+hash)
	h.ServeHTTP(rec, r)

	// Either upstream-attempt 502 (host-b.example is not resolvable)
	// or some other failure mode — the assertion is just that we did
	// NOT serve the cached body.
	if rec.Body.String() == string(body) {
		t.Errorf("cross-host by-hash request leaked the cached bytes (status=%d)", rec.Code)
	}
}

// Security gate: SuitePath is required. A by-hash-shaped path that
// doesn't sit under /dists/<suite>/ is not a legitimate apt request;
// the fallback must refuse before consulting the pool.
func TestServeHTTP_ByHashContentAddressed_RequiresSuitePath(t *testing.T) {
	h := newTestHandler(t, nil, nil)
	body := []byte("contents")
	hash := writeBlob(t, h, body)

	// Path uses by-hash form but lives outside any /dists/<suite>/.
	// proxy.SuitePath returns "" → req.SuitePath == "" → fallback must
	// not fire. Without an upstream the request will fail with 502;
	// what we care about is that the cached body is not served.
	rec := httptest.NewRecorder()
	r := newProxyReqHostPort("GET", "http", "127.0.0.1",
		"/oddly-shaped/by-hash/SHA256/"+hash)
	h.ServeHTTP(rec, r)

	if rec.Body.String() == string(body) {
		t.Errorf("non-suite by-hash path served cached bytes (status=%d)", rec.Code)
	}
}
