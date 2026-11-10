package freshness

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// Issue #29: source is included by the default empty architecture filter.
// A pdiff is no substitute for a full index; one full compressed variant is.
func TestMissingRequestableIndexGroups_DefaultSources(t *testing.T) {
	for _, suite := range []string{testDistsSuite, testFlatSuite} {
		t.Run(suite, func(t *testing.T) {
			path := "Sources"
			if suite == testDistsSuite {
				path = "main/source/Sources"
			}
			declared := []ReleaseMember{{Path: path}, {Path: path + ".gz"}, {Path: path + ".diff/Index"}}
			for _, tc := range []struct {
				name    string
				fetched []ReleaseMember
				allow   map[string]struct{}
				want    []string
			}{
				{"default missing", nil, nil, []string{path}},
				{"pdiff only", declared[2:], nil, []string{path}},
				{"compressed full index", declared[1:2], nil, nil},
				{"explicitly included", nil, map[string]struct{}{"source": {}}, []string{path}},
				{"explicitly excluded", nil, map[string]struct{}{"amd64": {}}, nil},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if got := missingRequestableIndexGroups(suite, declared, tc.fetched, tc.allow); !reflect.DeepEqual(got, tc.want) {
						t.Fatalf("missing groups = %v, want %v", got, tc.want)
					}
				})
			}
		})
	}
}

func TestAdopter_DefaultSources404PreservesSnapshot(t *testing.T) {
	for _, suitePath := range []string{testDistsSuite, testFlatSuite} {
		t.Run(suitePath, func(t *testing.T) {
			ctx := context.Background()
			env := newAdoptionTestEnv(t)
			suite := env.suite
			suite.SuitePath = suitePath
			trackSuite(t, env.cache, suite)
			ad := newReconcileAdopter(t, env, nil)
			base := suite.CanonicalScheme + "://" + suite.CanonicalHost + suitePath + "/"
			path := "Sources.gz"
			binaryPath := "Packages.gz"
			if suitePath == testDistsSuite {
				path = "main/source/Sources.gz"
				binaryPath = "main/binary-amd64/Packages.gz"
			}
			binary := gzipBytes(fakePackagesStanzas(map[string]string{"pool/main/a/a_1_amd64.deb": strings.Repeat("c", 64)}))
			env.fetcher.put(base+binaryPath, binary)
			old := sourceRecoveryFixture("a")
			release, hashes := makeRelease(map[string][]byte{path: old, binaryPath: binary})
			env.fetcher.put(base+path, old)
			if err := ad.Run(ctx, suite, simtest.ClearsignedFixture(string(release)), "", ""); err != nil {
				t.Fatal(err)
			}
			id := currentID(t, env.cache, suite)
			replacement := sourceRecoveryFixture("b")
			release, _ = makeRelease(map[string][]byte{path: replacement, binaryPath: binary})
			env.fetcher.fail404(base + path)
			if err := ad.Run(ctx, suite, simtest.ClearsignedFixture(string(release)), "", ""); !errors.Is(err, ErrAdoptionGuardFailed) {
				t.Fatalf("adoption = %v, want missing-index guard failure", err)
			}
			if got := currentID(t, env.cache, suite); got != id {
				t.Fatalf("snapshot changed from %d to %d despite unavailable Sources", id, got)
			}
			if got := snapshotPaths(t, env.cache, id)[path]; got != hashes[path] {
				t.Fatalf("previous Sources hash = %q, want %q", got, hashes[path])
			}
			// A later successful adoption may replace the preserved snapshot.
			env.fetcher.mu.Lock()
			delete(env.fetcher.errFor, base+path)
			env.fetcher.mu.Unlock()
			env.fetcher.put(base+path, replacement)
			if err := ad.Run(ctx, suite, simtest.ClearsignedFixture(string(release)), "", ""); err != nil {
				t.Fatal(err)
			}
			if got := currentID(t, env.cache, suite); got == id {
				t.Fatal("recovered upstream did not produce a new snapshot")
			}
		})
	}
}

func TestReconcileSnapshot_DefaultSourcesRecovery(t *testing.T) {
	for _, force := range []bool{false, true} {
		for _, suitePath := range []string{testDistsSuite, testFlatSuite} {
			t.Run(suitePath+map[bool]string{false: "/automatic", true: "/forced"}[force], func(t *testing.T) {
				ctx := context.Background()
				env := newAdoptionTestEnv(t)
				suite := env.suite
				suite.SuitePath = suitePath
				trackSuite(t, env.cache, suite)
				ad := newReconcileAdopter(t, env, nil)
				base := suite.CanonicalScheme + "://" + suite.CanonicalHost + suitePath + "/"
				path := "Sources.gz"
				if suitePath == testDistsSuite {
					path = "main/source/Sources.gz"
				}
				source := sourceRecoveryFixture("a")
				release, hashes := makeRelease(map[string][]byte{path: source})
				env.fetcher.put(base+path, source)
				if err := ad.Run(ctx, suite, simtest.ClearsignedFixture(string(release)), "", ""); err != nil {
					t.Fatal(err)
				}
				id := currentID(t, env.cache, suite)
				// Model an incomplete snapshot committed by an older version.
				if err := env.cache.DeleteSnapshotMembersForTest(ctx, id, path, byHashPath(path, "SHA256", hashes[path])); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(env.cache.BlobPath(hashes[path])); err != nil {
					t.Fatal(err)
				}
				if _, pending, err := ad.reconcilePending(ctx, suite, id); err != nil || !pending {
					t.Fatalf("reconcile pending = %v, %v; want missing Sources detected", pending, err)
				}
				// A temporary upstream failure must leave the group pending,
				// so the unchanged snapshot can heal on a later check.
				env.fetcher.fail404(base + path)
				if n, err := ad.ReconcileSnapshot(ctx, suite, id, force); err != nil || n != 0 {
					t.Fatalf("unavailable upstream: reconcile = %d, %v", n, err)
				}
				if _, pending, err := ad.reconcilePending(ctx, suite, id); err != nil || !pending {
					t.Fatalf("reconcile pending after 404 = %v, %v", pending, err)
				}
				env.fetcher.mu.Lock()
				delete(env.fetcher.errFor, base+path)
				env.fetcher.mu.Unlock()
				if n, err := ad.ReconcileSnapshot(ctx, suite, id, force); err != nil || n != 1 {
					t.Fatalf("reconcile = %d, %v; want one source index restored", n, err)
				}
				if currentID(t, env.cache, suite) != id || snapshotPaths(t, env.cache, id)[path] != hashes[path] {
					t.Fatal("Sources not restored with its declared hash in the same snapshot")
				}
				if _, pending, err := ad.reconcilePending(ctx, suite, id); err != nil || pending {
					t.Fatalf("reconcile pending after repair = %v, %v", pending, err)
				}
			})
		}
	}
}

func sourceRecoveryFixture(hashChar string) []byte {
	return gzipBytes([]byte("Package: a\nDirectory: pool/main/a/a\nChecksums-Sha256:\n " + strings.Repeat(hashChar, 64) + " 1 a_1.dsc\n\n"))
}
