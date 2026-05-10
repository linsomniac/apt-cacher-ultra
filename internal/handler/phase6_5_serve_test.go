package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
)

// newPhase6_5Handler builds a Handler instrumented with a logger
// captured into logBuf. Mirrors newPhase3StrictHandlerCapturing but
// with adoption + strict-mode flags both off — the §6.2 hash-validation
// path runs regardless.
func newPhase6_5Handler(t *testing.T, srv *httptest.Server) (*Handler, *bytes.Buffer) {
	t.Helper()

	logBuf := &bytes.Buffer{}
	captureLogger := slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

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
		Parser:      parser,
		Cache:       c,
		Fetch:       fc,
		HostLimiter: hostsem.New(4),
		Logger:      captureLogger,
		Serve:       config.ServeConfig{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h, logBuf
}

// shaHex returns sha256(content) as 64 lowercase hex.
func shaHex(content []byte) string {
	s := sha256.Sum256(content)
	return hex.EncodeToString(s[:])
}

// TestServeHash_SourceDsc_MatchedBytes_ValidatedAndCached: SPEC6_5
// §6.2 / §6.2.1. A .dsc with a snapshot's package_hash row gets its
// upstream bytes hash-validated; the cached row then serves on hit.
func TestServeHash_SourceDsc_MatchedBytes_ValidatedAndCached(t *testing.T) {
	dscBody := []byte("Format: 3.0 (quilt)\nSource: bash\nVersion: 5.1-2\n")
	dscHash := shaHex(dscBody)

	var upstreamHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		_, _ = w.Write(dscBody)
	}))
	t.Cleanup(srv.Close)

	h, _ := newPhase6_5Handler(t, srv)
	scheme, host, port := splitURL(t, srv.URL)
	canonHost := hostKey(host, port)

	const dscPath = "/pool/main/b/bash/bash_5.1-2.dsc"
	releaseBlob := writeBlob(t, h, []byte("InRelease for noble"))
	commitInlineSnapshot(t, h, scheme, canonHost, "/dists/noble", releaseBlob,
		[]cache.SnapshotMember{
			{Path: "InRelease", BlobHash: releaseBlob, DeclaredSHA256: releaseBlob},
		},
		[]cache.PackageHash{
			{
				CanonicalScheme: scheme,
				CanonicalHost:   canonHost,
				Path:            dscPath,
				DeclaredSHA256:  dscHash,
				PackageName:     "bash",
				Architecture:    "source",
			},
		},
	)

	// First request: miss → fetch → hash-validate → store.
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, proxyReq("GET", srv.URL, dscPath))
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200; body=%q", rec1.Code, rec1.Body.String())
	}
	if rec1.Header().Get("X-Cache") != "MISS" {
		t.Errorf("first X-Cache = %q, want MISS", rec1.Header().Get("X-Cache"))
	}
	if got := upstreamHits.Load(); got != 1 {
		t.Errorf("upstream hits after first request = %d, want 1", got)
	}

	// Second request: cache hit, no upstream contact.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, proxyReq("GET", srv.URL, dscPath))
	if rec2.Code != http.StatusOK {
		t.Fatalf("second request status = %d, want 200", rec2.Code)
	}
	if rec2.Header().Get("X-Cache") != "HIT" {
		t.Errorf("second X-Cache = %q, want HIT", rec2.Header().Get("X-Cache"))
	}
	if got := upstreamHits.Load(); got != 1 {
		t.Errorf("upstream hits after second request = %d, want 1 (HIT must not fetch)", got)
	}
}

// TestServeHash_SourceDsc_HashMismatch_502: SPEC6_5 §6.2.1 / §11 H8.
// When the served bytes don't match the declared package_hash, the
// serve fails with 502 and outcome=package_hash_mismatch carrying
// path_class=source_dsc in the request log.
func TestServeHash_SourceDsc_HashMismatch_502(t *testing.T) {
	declaredHash := strings.Repeat("a", 64) // bogus; upstream serves something else
	wrongBody := []byte("not what the snapshot declared")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(wrongBody)
	}))
	t.Cleanup(srv.Close)

	h, logBuf := newPhase6_5Handler(t, srv)
	scheme, host, port := splitURL(t, srv.URL)
	canonHost := hostKey(host, port)

	const dscPath = "/pool/main/b/bash/bash_5.1-2.dsc"
	releaseBlob := writeBlob(t, h, []byte("InRelease for noble"))
	commitInlineSnapshot(t, h, scheme, canonHost, "/dists/noble", releaseBlob,
		[]cache.SnapshotMember{
			{Path: "InRelease", BlobHash: releaseBlob, DeclaredSHA256: releaseBlob},
		},
		[]cache.PackageHash{
			{
				CanonicalScheme: scheme,
				CanonicalHost:   canonHost,
				Path:            dscPath,
				DeclaredSHA256:  declaredHash,
				PackageName:     "bash",
				Architecture:    "source",
			},
		},
	)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, dscPath))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (hash mismatch)", rec.Code)
	}

	out := logBuf.String()
	if !strings.Contains(out, `"outcome":"package_hash_mismatch"`) {
		t.Errorf("expected outcome=package_hash_mismatch in request log, got:\n%s", out)
	}
	if !strings.Contains(out, `"path_class":"source_dsc"`) {
		t.Errorf("expected path_class=source_dsc in request log, got:\n%s", out)
	}
}

// TestServeHash_SourceDsc_StrictModeDoesNotRefuse: SPEC6_5 §1.2 /
// §15 #2. Even with refuse_unvouched_debs=true and a fully-covered
// snapshot, a .dsc whose package_hash row is MISSING must NOT be
// strict-refused. The §6.1 isDebPath gate stays .deb-only.
func TestServeHash_SourceDsc_StrictModeDoesNotRefuse(t *testing.T) {
	body := []byte("upstream .dsc body")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	var upstreamHits atomic.Int32
	h, _ := newPhase3StrictHandler(t, true, true, &upstreamHits)
	scheme, host, port := splitURL(t, srv.URL)
	canonHost := hostKey(host, port)
	adoptCoverageComplete(t, h, scheme, canonHost, "/dists/noble", true)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/pool/main/b/bash/bash_5.1-2.dsc"))
	if rec.Code != http.StatusOK {
		t.Errorf(".dsc strict-mode passthrough: status = %d, want 200", rec.Code)
	}
}

// TestServeHash_PdiffPatch_MatchedBytes_ValidatedAndCached:
// SPEC6_5 §6.2. pdiff patch files behave identically to source
// artifacts at the validation step.
func TestServeHash_PdiffPatch_MatchedBytes_ValidatedAndCached(t *testing.T) {
	patchBody := []byte("compressed pdiff patch contents")
	patchHash := shaHex(patchBody)

	var upstreamHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		_, _ = w.Write(patchBody)
	}))
	t.Cleanup(srv.Close)

	h, _ := newPhase6_5Handler(t, srv)
	scheme, host, port := splitURL(t, srv.URL)
	canonHost := hostKey(host, port)

	const patchPath = "/dists/noble/main/binary-amd64/Packages.diff/2026-05-09-1234.56.gz"
	releaseBlob := writeBlob(t, h, []byte("InRelease for noble"))
	commitInlineSnapshot(t, h, scheme, canonHost, "/dists/noble", releaseBlob,
		[]cache.SnapshotMember{
			{Path: "InRelease", BlobHash: releaseBlob, DeclaredSHA256: releaseBlob},
		},
		[]cache.PackageHash{
			{
				CanonicalScheme: scheme,
				CanonicalHost:   canonHost,
				Path:            patchPath,
				DeclaredSHA256:  patchHash,
				Architecture:    "amd64",
			},
		},
	)

	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, proxyReq("GET", srv.URL, patchPath))
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", rec1.Code)
	}

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, proxyReq("GET", srv.URL, patchPath))
	if rec2.Code != http.StatusOK {
		t.Fatalf("second request status = %d, want 200", rec2.Code)
	}
	if rec2.Header().Get("X-Cache") != "HIT" {
		t.Errorf("second X-Cache = %q, want HIT", rec2.Header().Get("X-Cache"))
	}
}

// TestServeHash_PdiffPatch_HashMismatch_502: SPEC6_5 §6.2.1 / §11 H9
// — parallel to the .dsc case for pdiff patch files.
func TestServeHash_PdiffPatch_HashMismatch_502(t *testing.T) {
	declaredHash := strings.Repeat("b", 64)
	wrongBody := []byte("not the patch the snapshot declared")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(wrongBody)
	}))
	t.Cleanup(srv.Close)

	h, logBuf := newPhase6_5Handler(t, srv)
	scheme, host, port := splitURL(t, srv.URL)
	canonHost := hostKey(host, port)

	const patchPath = "/dists/noble/main/binary-amd64/Packages.diff/2026-05-09-1234.56.gz"
	releaseBlob := writeBlob(t, h, []byte("InRelease for noble"))
	commitInlineSnapshot(t, h, scheme, canonHost, "/dists/noble", releaseBlob,
		[]cache.SnapshotMember{
			{Path: "InRelease", BlobHash: releaseBlob, DeclaredSHA256: releaseBlob},
		},
		[]cache.PackageHash{
			{
				CanonicalScheme: scheme,
				CanonicalHost:   canonHost,
				Path:            patchPath,
				DeclaredSHA256:  declaredHash,
				Architecture:    "amd64",
			},
		},
	)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, patchPath))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (hash mismatch on pdiff patch)", rec.Code)
	}

	out := logBuf.String()
	if !strings.Contains(out, `"outcome":"package_hash_mismatch"`) {
		t.Errorf("expected outcome=package_hash_mismatch, got:\n%s", out)
	}
	if !strings.Contains(out, `"path_class":"pdiff_patch"`) {
		t.Errorf("expected path_class=pdiff_patch, got:\n%s", out)
	}
	if !strings.Contains(out, `"architecture":"amd64"`) {
		t.Errorf("expected architecture=amd64 in pdiff_patch log, got:\n%s", out)
	}
}

// Suppress "imported and not used" if we ever drop a helper.
var _ = fmt.Sprintf
