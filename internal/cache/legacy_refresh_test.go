package cache

import (
	"context"
	"errors"
	"testing"
)

func legacyFixture(t *testing.T) (*Cache, SuiteKey, string) {
	t.Helper()
	c := openCache(t)
	ctx := context.Background()
	key := SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}
	if err := c.SeedSuiteForTest(ctx, SuiteFreshness{CanonicalScheme: key.CanonicalScheme, CanonicalHost: key.CanonicalHost, SuitePath: key.SuitePath}); err != nil {
		t.Fatal(err)
	}
	hash := seedBlob(t, c, "old anchor")
	for _, p := range []string{key.SuitePath + "/InRelease", key.SuitePath + "/main/binary-amd64/Packages", key.SuitePath + "/main/by-hash/SHA256/" + hash, "/r/dists/y/InRelease", "/r/pool/x.deb"} {
		if err := c.PutURLPath(ctx, URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: p, BlobHash: &hash, UpstreamURL: "http://h" + p, IsMetadata: p != "/r/pool/x.deb"}); err != nil {
			t.Fatal(err)
		}
	}
	return c, key, hash
}

func TestLegacyRefresh_InvalidatesOnlyMutableSuiteRowsAndFencesFetch(t *testing.T) {
	c, key, hash := legacyFixture(t)
	ctx := context.Background()
	before, err := c.MetadataGeneration(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.InvalidateLegacyMetadata(ctx, key, key.SuitePath+"/InRelease", hash); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{key.SuitePath + "/InRelease", key.SuitePath + "/main/binary-amd64/Packages"} {
		if _, err := c.LookupURL(ctx, "http", "h", p); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: %v", p, err)
		}
	}
	for _, p := range []string{key.SuitePath + "/main/by-hash/SHA256/" + hash, "/r/dists/y/InRelease", "/r/pool/x.deb"} {
		if _, err := c.LookupURL(ctx, "http", "h", p); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
	row := URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: key.SuitePath + "/InRelease", BlobHash: &hash, UpstreamURL: "http://h" + key.SuitePath + "/InRelease", IsMetadata: true}
	if _, err := c.StoreFetched(ctx, FetchedRow{Row: row, Suite: key, Generation: &before}, SuiteCaps{}, nil); !errors.Is(err, ErrMetadataChanged) {
		t.Fatalf("stale writer: %v", err)
	}
	after, err := c.MetadataGeneration(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatal("generation did not advance")
	}
	if _, err := c.StoreFetched(ctx, FetchedRow{Row: row, Suite: key, Generation: &after}, SuiteCaps{}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyRefresh_TransactionFailureKeepsOldCache(t *testing.T) {
	c, key, hash := legacyFixture(t)
	ctx := context.Background()
	before, _ := c.MetadataGeneration(ctx, key)
	// Fail AFTER deletes, proving the whole publication is rolled back.
	if _, err := c.db.Exec(`CREATE TRIGGER fail_refresh BEFORE UPDATE OF metadata_generation ON suite_freshness BEGIN SELECT RAISE(ABORT,'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := c.InvalidateLegacyMetadata(ctx, key, key.SuitePath+"/InRelease", hash); err == nil {
		t.Fatal("expected failure")
	}
	if _, err := c.LookupURL(ctx, "http", "h", key.SuitePath+"/InRelease"); err != nil {
		t.Fatal(err)
	}
	after, _ := c.MetadataGeneration(ctx, key)
	if before != after {
		t.Fatal("failed refresh advanced generation")
	}
}

func TestLegacyRefresh_RefusesAdoptedOrChangedAnchor(t *testing.T) {
	for _, adopted := range []bool{false, true} {
		t.Run(map[bool]string{false: "changed", true: "adopted"}[adopted], func(t *testing.T) {
			c, key, hash := legacyFixture(t)
			ctx := context.Background()
			expected := hash
			if adopted {
				id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: key.SuitePath, InReleaseHash: &hash})
				if err != nil {
					t.Fatal(err)
				}
				if err := c.CommitAdoption(ctx, id, []SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: hash, DeclaredSHA256: hash}}, nil, nil, nil, false); err != nil {
					t.Fatal(err)
				}
			} else {
				expected = seedBlob(t, c, "different anchor")
			}
			if err := c.InvalidateLegacyMetadata(ctx, key, key.SuitePath+"/InRelease", expected); err == nil {
				t.Fatal("expected refusal")
			}
			if _, err := c.LookupURL(ctx, "http", "h", key.SuitePath+"/InRelease"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMetadataGeneration_SuiteRecreationCannotReuseToken(t *testing.T) {
	c, key, _ := legacyFixture(t)
	ctx := context.Background()
	before, _ := c.MetadataGeneration(ctx, key)
	if _, err := c.db.Exec(`DELETE FROM suite_freshness`); err != nil {
		t.Fatal(err)
	}
	if err := c.SeedSuiteForTest(ctx, SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: key.SuitePath}); err != nil {
		t.Fatal(err)
	}
	after, _ := c.MetadataGeneration(ctx, key)
	if before.Generation == after.Generation {
		t.Fatal("recreated suite reused generation")
	}
}

func TestLegacyRefresh_FlatSuiteIsolation(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/flat"}
	if err := c.SeedSuiteForTest(ctx, SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/flat"}); err != nil {
		t.Fatal(err)
	}
	hash := seedBlob(t, c, "Release")
	paths := []string{"/flat/Release", "/flat/Packages.gz", "/flat/./Release", "/flat/sub/Packages", "/flat/x.deb"}
	for _, path := range paths {
		if err := c.PutURLPath(ctx, URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: path, BlobHash: &hash, UpstreamURL: "http://h" + path, IsMetadata: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.InvalidateLegacyMetadata(ctx, key, "/flat/Release", hash); err != nil {
		t.Fatal(err)
	}
	for i, path := range paths {
		_, err := c.LookupURL(ctx, "http", "h", path)
		if i < 2 {
			if !errors.Is(err, ErrNotFound) {
				t.Fatal(path, err)
			}
		} else if err != nil {
			t.Fatal(path, err)
		}
	}
}
