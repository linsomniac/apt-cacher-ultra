package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// Ruling PG-2 through the handler: on a strict host, an unvouched .deb
// whose name starts like an index — a package named icons-foo, a flat
// repository's Packages_1.0_all.deb — is a package, refused like any other
// (502, Retry-After 60, unvouched_deb_refused). Before the ruling it was
// classified as metadata: served, revalidated, never hash-checked.
func TestServeHTTP_StrictRefusesAnUnvouchedDebNamedLikeAnIndex(t *testing.T) {
	h, srv, logs := newPhase3StrictHandlerCapturing(t, true, true, nil)
	scheme, host, port := splitURL(t, srv.URL)
	adoptCoverageComplete(t, h, scheme, hostKey(host, port), "/dists/noble", true)
	paths := []string{"/pool/main/i/icons-foo/icons-foo_1.0_all.deb", "/flat/Packages_1.0_all.deb"}
	for _, p := range paths {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, proxyReq("GET", srv.URL, p))
		if rec.Code != http.StatusBadGateway || rec.Header().Get("Retry-After") != "60" {
			t.Errorf("%s: status %d, Retry-After %q; want the strict refusal (502, 60)", p, rec.Code, rec.Header().Get("Retry-After"))
		}
	}
	if n := len(logs.find("unvouched_deb_refused")); n != len(paths) {
		t.Fatalf("%d unvouched_deb_refused lines, want %d", n, len(paths))
	}
}

// Ruling PG-2's residual on the hit path: a url_path row an older binary
// stored with is_metadata = 1 for a pool artifact is still hash-checked —
// serving reads the request's classification, not the stored column — so
// on a strict host its unvouched hit is refused like any .deb, its cached
// bytes never served.
func TestServeHTTP_StrictRefusesAnOldMetadataRowOfADebOnItsHit(t *testing.T) {
	h, srv, logs := newPhase3StrictHandlerCapturing(t, true, true, nil)
	scheme, host, port := splitURL(t, srv.URL)
	canon := hostKey(host, port)
	adoptCoverageComplete(t, h, scheme, canon, "/dists/noble", true)
	const debPath = "/pool/main/i/icons-foo/icons-foo_1.0_all.deb"
	blob := writeBlob(t, h, []byte("an icons-foo .deb an old binary cached as metadata"))
	if err := h.cache.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: scheme, CanonicalHost: canon,
		Path: debPath, BlobHash: &blob, UpstreamURL: srv.URL + debPath, IsMetadata: true}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, debPath))
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "old binary cached") {
		t.Fatalf("status %d body %q; want the strict refusal and no cached bytes", rec.Code, rec.Body.String())
	}
	if n := len(logs.find("unvouched_deb_refused")); n != 1 {
		t.Fatalf("%d unvouched_deb_refused lines, want 1", n)
	}
}
