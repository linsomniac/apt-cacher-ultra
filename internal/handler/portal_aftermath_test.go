package handler

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"

	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// The captive portal's aftermath (docs/configuration.md, *Flat
// repositories*; e2e/flat's anchorless scenario): a client's miss during
// the outage caches the portal's page as a Release-less flat repository's
// InRelease, Release and Release.gpg, and the anchorless refresh deletes
// only the first two. When that repository later publishes an unsigned
// Release and adopts it, the page cached as its Release.gpg (or InRelease)
// is never served again: an unsigned snapshot's one anchor is its Release,
// and an adopted suite answers a path its snapshot lacks 404, never from
// the url_path row (spec §6.1, "the snapshot is the contract"; spec §10
// item 9 says the same of the inline form). So apt's "Signed file isn't
// valid, got 'NODATA'" for the page ends with the adoption.
func TestServeHTTP_AnUnsignedSnapshotNeverServesACachedPortalPage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		s := newFlipStack(t, 0)
		const suite = "/fury"
		portal := []byte("<html><body>Sign in to the guest network</body></html>\n")
		s.cacheRow(suite+"/Release.gpg", portal, 1)
		s.cacheRow(suite+"/InRelease", portal, 1)
		pkgs := []byte("Package: p\nArchitecture: amd64\nVersion: 1\nFilename: p_1_amd64.deb\nSize: 1\n" +
			"SHA256: 0000000000000000000000000000000000000000000000000000000000000001\n\n")
		s.members.put("http://"+flipSimHost+suite+"/Packages", pkgs)
		rel := []byte(fmt.Sprintf("Label: fury\nSHA256:\n %x %d Packages\n", sha256.Sum256(pkgs), len(pkgs)))
		s.track(suite)
		get := func(name string) *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			s.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+flipSimHost+suite+"/"+name, nil))
			return rec
		}
		if rec := get("Release.gpg"); rec.Code != http.StatusOK || rec.Body.String() != string(portal) {
			t.Fatalf("premise: before the adoption Release.gpg is %d, the portal's page %v; want the cached page",
				rec.Code, rec.Body.String() == string(portal))
		}
		ad, err := freshness.NewAdopter(freshness.AdoptionConfig{Cache: s.c, Fetcher: s.members, Verifier: plainVerifier{},
			HostLimiter: hostsem.New(8), Logger: simtest.DiscardLogger(), AdoptUnsigned: true})
		if err != nil {
			t.Fatal(err)
		}
		ref := freshness.SuiteRef{CanonicalScheme: "http", CanonicalHost: flipSimHost, SuitePath: suite}
		if err := ad.RunUnsigned(ctx, ref, rel, "", ""); err != nil {
			t.Fatal(err)
		}
		snap := s.current(suite)
		if snap == "" {
			t.Fatal("premise: the unsigned adoption committed no snapshot")
		}
		for _, name := range []string{"Release.gpg", "InRelease"} {
			if rec := get(name); rec.Code != http.StatusNotFound || rec.Body.String() == string(portal) {
				t.Fatalf("%s after the unsigned adoption: status %d, the portal's page %v; want 404 from the snapshot",
					name, rec.Code, rec.Body.String() == string(portal))
			}
		}
		if rec := get("Release"); rec.Code != http.StatusOK || rec.Body.String() != string(rel) ||
			rec.Header().Get(hdrXCacheSnapshot) != snap {
			t.Fatalf("Release: status %d, snapshot %q, the adopted Release %v; want 200 from snapshot %s",
				rec.Code, rec.Header().Get(hdrXCacheSnapshot), rec.Body.String() == string(rel), snap)
		}
	})
}
