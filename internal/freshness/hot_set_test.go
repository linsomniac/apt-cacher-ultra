package freshness

import (
	"context"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// Deferred minor A-15: the hot set's upstream URL is aptpath's, apt's own
// encoding — a '+' or '~' in a pool path is percent-encoded, as apt
// requests it.
func TestComputeHotSet_EncodesTheUpstreamURLAsApt(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ctx := context.Background()
	c := env.cache
	suite := SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}
	blob := func(n int) string {
		h := strings.Repeat(string(rune('a'+n)), 64)
		if err := c.PutBlob(ctx, h, 1); err != nil {
			t.Fatal(err)
		}
		return h
	}
	anchorOld, anchorNew, debOld, debNew := blob(0), blob(1), blob(2), blob(3)
	const oldPath, newPath = "/r/pool/main/g/g++/g++_1~rc1_amd64.deb", "/r/pool/main/g/g++/g++_2~rc1_amd64.deb"
	prior, _, err := c.InsertTrackedCandidateForTest(ctx, cache.SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h",
		SuitePath: suite.SuitePath, InReleaseHash: &anchorOld})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, prior,
		[]cache.SnapshotMember{{SnapshotID: prior, Path: "InRelease", BlobHash: anchorOld, DeclaredSHA256: anchorOld}}, nil,
		[]cache.PackageHash{{CanonicalScheme: "http", CanonicalHost: "h", Path: oldPath, DeclaredSHA256: debOld,
			SnapshotID: prior, PackageName: "g++", Architecture: "amd64", Version: "1~rc1"}}, nil, true); err != nil {
		t.Fatal(err)
	}
	now := env.adopter.now().Unix()
	if err := c.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: oldPath, BlobHash: &debOld,
		UpstreamURL: "http://h/r/pool/main/g/g%2b%2b/g%2b%2b_1%7erc1_amd64.deb", LastRequestedAt: &now, RequestCount: 1}); err != nil {
		t.Fatal(err)
	}
	cand, _, err := c.InsertCandidateSnapshot(ctx, cache.SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h",
		SuitePath: suite.SuitePath, InReleaseHash: &anchorNew})
	if err != nil {
		t.Fatal(err)
	}
	hot, err := env.adopter.computeHotSet(ctx, suite, cand, []cache.PackageHash{{CanonicalScheme: "http", CanonicalHost: "h",
		Path: newPath, DeclaredSHA256: debNew, SnapshotID: cand, PackageName: "g++", Architecture: "amd64", Version: "2~rc1"}}, 86400)
	if err != nil {
		t.Fatal(err)
	}
	const want = "http://h/r/pool/main/g/g%2b%2b/g%2b%2b_2%7erc1_amd64.deb"
	if len(hot) != 1 || hot[0].path != newPath || hot[0].upstreamURL != want {
		t.Fatalf("hot set %+v, want %s at %s", hot, newPath, want)
	}
}
