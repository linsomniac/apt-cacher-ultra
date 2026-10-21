package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
