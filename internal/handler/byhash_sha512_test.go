package handler

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// Phase E DoD (spec C5, C8): an adoption of a Release that declares SHA256
// and SHA512 writes both by-hash aliases, and the handler serves the
// SHA512 one apt requests — from the current snapshot and, once the next
// adoption has displaced it, from the displaced snapshot (retained by-hash).
func TestServeHTTP_SHA512ByHashFromAnAdoption(t *testing.T) {
	ctx := context.Background()
	c := simtest.OpenCache(t)
	members := &memberStore{bodies: map[string][]byte{}}
	ad, err := freshness.NewAdopter(freshness.AdoptionConfig{Cache: c, Fetcher: members, Verifier: plainVerifier{},
		HostLimiter: hostsem.New(8), Logger: simtest.DiscardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	parser, err := proxy.New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{Parser: parser, Cache: c, Fetch: noUpstream{}, Logger: silentLogger(),
		HostLimiter: hostsem.New(8), KeepDisplaced: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	const suite = "/r/dists/x"
	if err := c.EnsureSuiteForTest(ctx, "http", flipSimHost, suite); err != nil {
		t.Fatal(err)
	}
	ref := freshness.SuiteRef{CanonicalScheme: "http", CanonicalHost: flipSimHost, SuitePath: suite}
	// adopt publishes generation gen and adopts it; it returns the
	// Packages bytes, their SHA512 alias path and the snapshot id.
	adopt := func(gen int) ([]byte, string, string) {
		pkgs := []byte(fmt.Sprintf("Package: p\nArchitecture: amd64\nVersion: %d\nFilename: pool/p_%d_amd64.deb\nSize: 1\nSHA256: %064x\n\n",
			gen, gen, gen))
		members.put("http://"+flipSimHost+suite+"/main/binary-amd64/Packages", pkgs)
		s256, s512 := sha256.Sum256(pkgs), sha512.Sum512(pkgs)
		rel := fmt.Sprintf("Label: generation %d\nSHA256:\n %x %d main/binary-amd64/Packages\nSHA512:\n %x %d main/binary-amd64/Packages\n",
			gen, s256, len(pkgs), s512, len(pkgs))
		if err := ad.Run(ctx, ref, simtest.ClearsignedFixture(rel), "", ""); err != nil {
			t.Fatal(err)
		}
		sf, err := c.GetSuiteFreshness(ctx, "http", flipSimHost, suite)
		if err != nil || sf.CurrentSnapshotID == nil {
			t.Fatalf("generation %d not adopted: %+v, %v", gen, sf, err)
		}
		return pkgs, suite + "/main/binary-amd64/by-hash/SHA512/" + hex.EncodeToString(s512[:]),
			strconv.FormatInt(*sf.CurrentSnapshotID, 10)
	}
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+flipSimHost+path, nil))
		return rec
	}
	pkgs1, alias1, id1 := adopt(1)
	if rec := get(alias1); rec.Code != http.StatusOK || rec.Body.String() != string(pkgs1) || rec.Header().Get(hdrXCacheSnapshot) != id1 {
		t.Fatalf("current snapshot's SHA512 alias: %d %q from snapshot %q; want 200 with Packages from %s",
			rec.Code, rec.Body.String(), rec.Header().Get(hdrXCacheSnapshot), id1)
	}
	pkgs2, alias2, id2 := adopt(2)
	if rec := get(alias1); rec.Code != http.StatusOK || rec.Body.String() != string(pkgs1) || rec.Header().Get(hdrXCacheSnapshot) != id1 {
		t.Fatalf("displaced snapshot's SHA512 alias: %d %q from snapshot %q; want 200 with the old Packages from %s",
			rec.Code, rec.Body.String(), rec.Header().Get(hdrXCacheSnapshot), id1)
	}
	if rec := get(alias2); rec.Code != http.StatusOK || rec.Body.String() != string(pkgs2) || rec.Header().Get(hdrXCacheSnapshot) != id2 {
		t.Fatalf("new snapshot's SHA512 alias: %d from snapshot %q; want 200 from %s", rec.Code, rec.Header().Get(hdrXCacheSnapshot), id2)
	}
}
