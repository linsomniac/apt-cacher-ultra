package cache

import (
	"context"
	"testing"
)

// TestCommitAdoption_SyncsInReleaseAnchorBlobHash is the regression test
// for the freeze trap's root cause (SPEC3 §7.5.1 Step 3c). Adoption must
// sync the InRelease url_path anchor's blob_hash to the newly-adopted
// snapshot hash so the SPEC4 §5 GC guards (b)/(c) can vouch for it — while
// PRESERVING the existing row's (port-correct) upstream_url rather than
// clobbering it with portless reconstruction.
//
// AIDEV-NOTE: keep this with the version-aware-retention GC tests — a future
// edit that adds `upstream_url = excluded.upstream_url` to the anchor upsert
// would silently overwrite an operator's port-bearing anchor URL and refreeze
// the suite (the production freeze trap). This is the only coverage of that.
func TestCommitAdoption_SyncsInReleaseAnchorBlobHash(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, 1_700_000_000)()

	stale := seedBlob(t, c, "stale client-fetched inrelease")
	adopted := seedBlob(t, c, "adopted inrelease bytes")

	const host = "archive.test"
	const suite = "/ubuntu/dists/noble"
	const anchorPath = suite + "/InRelease"
	const customURL = "http://archive.test:8080/ubuntu/dists/noble/InRelease"

	// Pre-existing anchor row from a prior client miss: blob_hash points
	// at the STALE bytes and carries a port-bearing upstream_url adoption
	// must PRESERVE.
	if _, err := c.db.Exec(`INSERT INTO url_path
	  (canonical_scheme, canonical_host, path, blob_hash, upstream_url,
	   is_metadata, last_requested_at, request_count)
	  VALUES ('http', ?, ?, ?, ?, 1, ?, 5)`,
		host, anchorPath, stale, customURL, int64(1_699_000_000)); err != nil {
		t.Fatalf("seed anchor: %v", err)
	}

	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: host, SuitePath: suite,
		InReleaseHash: &adopted,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id, []SnapshotMember{
		{SnapshotID: id, Path: "InRelease", BlobHash: adopted, DeclaredSHA256: adopted},
	}, nil, nil, nil, false); err != nil {
		t.Fatalf("CommitAdoption: %v", err)
	}

	u, err := c.LookupURL(ctx, "http", host, anchorPath)
	if err != nil {
		t.Fatalf("anchor LookupURL: %v", err)
	}
	if u.BlobHash == nil || *u.BlobHash != adopted {
		t.Errorf("anchor blob_hash = %v, want adopted snapshot inrelease_hash %s", u.BlobHash, adopted)
	}
	if u.UpstreamURL != customURL {
		t.Errorf("anchor upstream_url = %q, want preserved %q", u.UpstreamURL, customURL)
	}
	if !u.IsMetadata {
		t.Error("anchor is_metadata must remain 1")
	}
}

// TestCommitAdoption_CreatesInReleaseAnchorWhenMissing covers the case
// where the anchor was already reaped before adoption (e.g. Layer A
// recovery is racing): adoption creates it with the reconstructed URL.
func TestCommitAdoption_CreatesInReleaseAnchorWhenMissing(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, 1_700_000_000)()

	adopted := seedBlob(t, c, "adopted inrelease bytes (no prior anchor)")
	const host = "archive.test"
	const suite = "/ubuntu/dists/noble"
	const anchorPath = suite + "/InRelease"

	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: host, SuitePath: suite,
		InReleaseHash: &adopted,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id, []SnapshotMember{
		{SnapshotID: id, Path: "InRelease", BlobHash: adopted, DeclaredSHA256: adopted},
	}, nil, nil, nil, false); err != nil {
		t.Fatalf("CommitAdoption: %v", err)
	}

	u, err := c.LookupURL(ctx, "http", host, anchorPath)
	if err != nil {
		t.Fatalf("anchor not created by adoption: %v", err)
	}
	if u.BlobHash == nil || *u.BlobHash != adopted {
		t.Errorf("created anchor blob_hash = %v, want %s", u.BlobHash, adopted)
	}
	if u.UpstreamURL != "http://"+host+anchorPath {
		t.Errorf("created anchor upstream_url = %q, want reconstructed %q", u.UpstreamURL, "http://"+host+anchorPath)
	}
	if !u.IsMetadata {
		t.Error("created anchor is_metadata must be 1")
	}
}
