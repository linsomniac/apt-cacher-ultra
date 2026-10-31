package freshness

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// Deferred minor E-6: a pooled blob whose SHA-256 matches but whose
// SHA-512 does not fails the member and logs adoption_member_mismatch
// with both full SHA-512 digests (the reason chip carries 12 characters).
func TestAdoptMember_ReusedBlobSHA512MismatchLogsTheFullDigests(t *testing.T) {
	ctx := context.Background()
	r := newUnsignedRig(t, 0, 0)
	body := []byte("pooled member bytes")
	if _, err := r.ad.writeBlobBytes(ctx, body); err != nil {
		t.Fatal(err)
	}
	declared := strings.Repeat("ab", 64)
	_, err := r.ad.adoptMember(ctx, r.suite, ReleaseMember{Path: "main/binary-amd64/Packages", SHA256: shaOf(body),
		SHA512: declared, Size: int64(len(body))}, false)
	if !errors.Is(err, ErrAdoptionMemberMismatch) {
		t.Fatalf("adoptMember = %v, want ErrAdoptionMemberMismatch", err)
	}
	recs := r.records("adoption_member_mismatch")
	if len(recs) != 1 || recs[0]["declared_sha512"] != declared || recs[0]["actual_sha512"] != sha512Of(body) {
		t.Fatalf("adoption_member_mismatch records %v; want one with both full SHA-512 digests", recs)
	}
}

// closingFetcher serves body and then closes the cache: the fetch
// succeeds and the PutBlob after it fails, a local fault.
type closingFetcher struct {
	c    *cache.Cache
	body []byte
}

func (f closingFetcher) Fetch(_ context.Context, _ *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error) {
	if _, err := dst.Write(f.body); err != nil {
		return nil, err
	}
	_ = f.c.Close()
	return &fetch.FetchResult{Status: 200, ContentLength: int64(len(f.body))}, nil
}

// Deferred minor E-12: the by-hash probe's local faults — the temp blob,
// the blob row — are ErrAdoptionDBFailed, as on the plain path, so the
// member fails at once instead of being downloaded again.
func TestTryByHashFetch_LocalFaultsAreDBFailures(t *testing.T) {
	ctx := context.Background()
	body := []byte("by-hash member bytes")
	m := ReleaseMember{Path: "main/binary-amd64/Packages", SHA256: shaOf(body), Size: int64(len(body))}
	probe := byHashProbePath(m)
	t.Run("temp blob", func(t *testing.T) {
		r := newUnsignedRig(t, 0, 0)
		if err := os.RemoveAll(filepath.Join(r.c.Dir(), "tmp")); err != nil {
			t.Fatal(err)
		}
		if _, err := r.ad.tryByHashFetch(ctx, r.suite, m, probe); !errors.Is(err, ErrAdoptionDBFailed) {
			t.Fatalf("tryByHashFetch = %v, want ErrAdoptionDBFailed", err)
		}
	})
	t.Run("blob row", func(t *testing.T) {
		c := simtest.OpenCache(t)
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: closingFetcher{c: c, body: body}, Verifier: passThroughVerifier{},
			HostLimiter: hostsem.New(8), Logger: slog.New(slog.DiscardHandler)})
		if err != nil {
			t.Fatal(err)
		}
		suite := SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/u/dists/s"}
		if _, err := ad.tryByHashFetch(ctx, suite, m, probe); !errors.Is(err, ErrAdoptionDBFailed) {
			t.Fatalf("tryByHashFetch = %v, want ErrAdoptionDBFailed", err)
		}
	})
}

// Deferred minor E-15 (spec C5 safeguard 5): an unsigned Release that
// declares a Sources file and a Packages.diff/Index writes no package_hash
// row — the Sources and pdiff builders do not run for the unsigned form —
// while the same Release adopted inline writes one row per builder (the
// fixture parses).
func TestRunUnsigned_SourcesAndPdiffGetNoPackageHash(t *testing.T) {
	ctx := context.Background()
	sources := []byte("Package: bash\nVersion: 5.1-2\nChecksums-Sha256:\n " + testHex64 +
		" 4080 bash_5.1-2.dsc\nDirectory: pool/main/b/bash\n\n")
	index := []byte("SHA256-Download:\n " + testHex64 + " 1500 2026-05-09-1234.56.gz\n")
	for _, unsigned := range []bool{true, false} {
		r := newUnsignedRig(t, 0, 0)
		members := map[string][]byte{"main/binary-amd64/Packages": r.pkgs, "main/source/Sources": sources,
			"main/binary-amd64/Packages.diff/Index": index}
		for p, b := range members {
			r.members.put("http://h/u/dists/s/"+p, b)
		}
		rel := hashRelease("Origin: e15\n", true, false, members, nil)
		var err error
		if unsigned {
			err = r.ad.RunUnsigned(ctx, r.suite, rel, "", "")
		} else {
			err = r.ad.Run(ctx, r.suite, simtest.ClearsignedFixture(string(rel)), "", "")
		}
		if err != nil {
			t.Fatalf("unsigned %v: %v", unsigned, err)
		}
		want := 3 // the .deb, the .dsc and the patch
		if unsigned {
			want = 0
		}
		if n := packageHashCount(t, r.c, r.current()); n != want {
			t.Errorf("unsigned %v: %d package_hash rows, want %d", unsigned, n, want)
		}
	}
}
