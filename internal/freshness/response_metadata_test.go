package freshness

import (
	"context"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
)

type datedMemberFetcher struct {
	inner *fakeFetcher
	date  string
}

func (f datedMemberFetcher) Fetch(ctx context.Context, t *fetch.Target, w fetch.FetchDst) (*fetch.FetchResult, error) {
	r, err := f.inner.Fetch(ctx, t, w)
	if err == nil {
		r.LastModified = f.date
	}
	return r, err
}

func TestAdoption_PersistsIndependentResponseDates(t *testing.T) {
	for _, byHash := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "by_hash"}[byHash], func(t *testing.T) {
			env := newAdoptionTestEnv(t)
			ctx := context.Background()
			const anchorDate = "Mon, 01 Jan 2024 00:00:00 GMT"
			const sigDate = "Tue, 02 Jan 2024 00:00:00 GMT"
			const memberDate = "Sun, 31 Dec 2023 00:00:00 GMT"
			env.adopter.fetcher = datedMemberFetcher{env.fetcher, memberDate}
			path := "main/binary-amd64/Packages"
			pkgs := fakePackagesStanzas(map[string]string{"pool/x.deb": testHex64})
			release, _ := makeRelease(map[string][]byte{path: pkgs})
			fetchPath := path
			if byHash {
				release, _ = makeReleaseByHash(path, pkgs)
				fetchPath = "main/binary-amd64/by-hash/SHA256/" + shaOf(pkgs)
			}
			env.fetcher.put(buildMemberURL(env.suite, fetchPath), pkgs)
			sig := []byte("signature bytes")
			req := &adoptionRequest{suite: env.suite, form: adoptionFormDetached, releaseBytes: release, sigBytes: sig, lastmod: anchorDate, sigLastmod: sigDate}
			if err := env.adopter.runRequest(ctx, req, runHooks{}); err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct{ path, hash, date string }{{"Release", shaOf(release), anchorDate}, {"Release.gpg", shaOf(sig), sigDate}, {path, shaOf(pkgs), memberDate}, {fetchPath, shaOf(pkgs), memberDate}} {
				got, err := env.cache.RepresentationLastModified(ctx, env.suite.CanonicalScheme, env.suite.CanonicalHost, env.suite.SuitePath+"/"+tc.path, tc.hash)
				if err != nil || got != tc.date {
					t.Fatalf("%s: %q (%v), want %q", tc.path, got, err, tc.date)
				}
			}
		})
	}
}
