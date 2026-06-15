package cache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// commitTestSnapshotInRelease creates and commits a minimal snapshot for
// (http, x.example, /dists/s) carrying only an InRelease member, returning
// the adopted snapshot id. This mirrors the pattern used in skipped_member_test.go
// via commitSnapshotWithSkips — but without any skips, to give reconcile tests
// a clean base to INSERT new members into.
func commitTestSnapshotInRelease(t *testing.T, c *Cache) int64 {
	t.Helper()
	ctx := context.Background()
	rel := seedBlob(t, c, "InRelease body")
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "x.example",
		SuitePath:       "/dists/s",
		InReleaseHash:   &rel,
	})
	if err != nil {
		t.Fatalf("commitTestSnapshotInRelease: InsertCandidateSnapshot: %v", err)
	}
	members := []SnapshotMember{
		{SnapshotID: id, Path: "InRelease", BlobHash: rel, DeclaredSHA256: rel},
	}
	if err := c.CommitAdoption(ctx, id, members, nil, nil, nil, false); err != nil {
		t.Fatalf("commitTestSnapshotInRelease: CommitAdoption: %v", err)
	}
	return id
}

// writeTestBlob places a blob with the given synthetic hash into the pool at
// the correct path and registers it with PutBlob. This is used in reconcile
// tests that build SnapshotMember rows with pre-chosen hash values (all-"e"
// hex strings, etc.) rather than hashes derived from real content. The file
// written at pool/<hash[:2]>/<hash> contains data; there is no on-read
// content-hash verification in the cache layer.
func writeTestBlob(t *testing.T, c *Cache, hash string, data []byte) {
	t.Helper()
	ctx := context.Background()
	poolPath := c.BlobPath(hash) // panics on invalid hash — correct behaviour
	dir := filepath.Dir(poolPath)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("writeTestBlob: MkdirAll %s: %v", dir, err)
	}
	if err := os.WriteFile(poolPath, data, 0o640); err != nil {
		t.Fatalf("writeTestBlob: WriteFile %s: %v", poolPath, err)
	}
	if err := c.PutBlob(ctx, hash, int64(len(data))); err != nil {
		t.Fatalf("writeTestBlob: PutBlob(%s): %v", hash, err)
	}
}

// displaceCurrentSnapshot adopts a second snapshot for the same suite so that
// snapID is no longer current. The second snapshot uses a different InRelease
// blob to satisfy the natural-key UNIQUE index.
func displaceCurrentSnapshot(t *testing.T, c *Cache, _ int64) {
	t.Helper()
	ctx := context.Background()
	rel2 := seedBlob(t, c, "InRelease v2 body for displacement")
	id2, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "x.example",
		SuitePath:       "/dists/s",
		InReleaseHash:   &rel2,
	})
	if err != nil {
		t.Fatalf("displaceCurrentSnapshot: InsertCandidateSnapshot: %v", err)
	}
	if err := c.CommitAdoption(ctx, id2, []SnapshotMember{
		{SnapshotID: id2, Path: "InRelease", BlobHash: rel2, DeclaredSHA256: rel2},
	}, nil, nil, nil, false); err != nil {
		t.Fatalf("displaceCurrentSnapshot: CommitAdoption: %v", err)
	}
}

// errorsIs is a thin wrapper around errors.Is for test-assertion readability.
func errorsIs(err, target error) bool {
	return errors.Is(err, target)
}

func TestInsertReconciledMembers_InPlace(t *testing.T) {
	ctx := context.Background()
	c := openCache(t)

	snapID := commitTestSnapshotInRelease(t, c)

	blob := strings.Repeat("e", 64)
	writeTestBlob(t, c, blob, []byte("binary-all Packages bytes"))
	// A path plus its by-hash alias share ONE blob — exercises the
	// once-per-distinct-blob refcount guard.
	alias := "main/by-hash/SHA256/" + blob
	members := []SnapshotMember{
		{SnapshotID: snapID, Path: "main/binary-all/Packages", BlobHash: blob, DeclaredSHA256: blob},
		{SnapshotID: snapID, Path: alias, BlobHash: blob, DeclaredSHA256: blob},
	}
	if err := c.InsertReconciledMembers(ctx, snapID, members, nil); err != nil {
		t.Fatalf("InsertReconciledMembers: %v", err)
	}
	for _, p := range []string{"main/binary-all/Packages", alias} {
		if _, err := c.GetSnapshotMember(ctx, snapID, p); err != nil {
			t.Errorf("member %q not inserted: %v", p, err)
		}
	}
	// Two member rows over one distinct blob → exactly one refcount bump.
	if got := blobRefcount(t, c, blob); got != 1 {
		t.Errorf("refcount after insert = %d, want 1 (one bump for two members sharing a blob)", got)
	}
	// Idempotent re-insert is a no-op — and must NOT bump refcount again.
	if err := c.InsertReconciledMembers(ctx, snapID, members, nil); err != nil {
		t.Errorf("re-insert should be idempotent, got %v", err)
	}
	if got := blobRefcount(t, c, blob); got != 1 {
		t.Errorf("refcount after idempotent re-insert = %d, want 1 (no double bump)", got)
	}

	// The refcount must net to zero when the snapshot is later displaced:
	// CommitAdoption Step 8 decrements once per blob the prior snapshot
	// pinned. If the reconcile bump were missing, this would underflow to -1.
	displaceCurrentSnapshot(t, c, snapID)
	if got := blobRefcount(t, c, blob); got != 0 {
		t.Errorf("refcount after displacement = %d, want 0 (reconcile bump must net the displacement decrement)", got)
	}
}

// TestInsertReconciledMembers_RejectsConflictingPackageHash: a healed index
// that declares a DIFFERENT hash for a .deb path an already-present index
// already declared is a real cross-index disagreement — adoption fails on it
// (ErrAdoptionParseFailed, SPEC6_5 §11 H7), so reconcile must too, not
// silently keep the stale row (which would fail-closed every fetch of that
// .deb via the healed index). A byte-identical declaration stays idempotent.
func TestInsertReconciledMembers_RejectsConflictingPackageHash(t *testing.T) {
	ctx := context.Background()
	c := openCache(t)
	snapID := commitTestSnapshotInRelease(t, c)

	ph := func(path, sha string) PackageHash {
		return PackageHash{
			CanonicalScheme: "http", CanonicalHost: "h", Path: path,
			DeclaredSHA256: sha, SnapshotID: snapID, PackageName: "pkg", Architecture: "all",
		}
	}
	const debPath = "/ubuntu/pool/main/p/pkg/pkg_1_all.deb"
	hashA := strings.Repeat("a", 64)
	hashB := strings.Repeat("b", 64)

	// First reconcile declares debPath = hashA.
	if err := c.InsertReconciledMembers(ctx, snapID, nil, []PackageHash{ph(debPath, hashA)}); err != nil {
		t.Fatalf("initial package_hash insert: %v", err)
	}
	// Byte-identical re-insert is an idempotent no-op.
	if err := c.InsertReconciledMembers(ctx, snapID, nil, []PackageHash{ph(debPath, hashA)}); err != nil {
		t.Errorf("byte-identical package_hash re-insert should be a no-op, got %v", err)
	}
	// A conflicting hash for the same path is a loud error.
	if err := c.InsertReconciledMembers(ctx, snapID, nil, []PackageHash{ph(debPath, hashB)}); err == nil {
		t.Fatal("conflicting package_hash declaration was silently accepted — must be a loud error")
	}
	// The stored row still reads hashA — no silent overwrite.
	got, gerr := c.GetPackageHash(ctx, "http", "h", debPath, snapID)
	if gerr != nil {
		t.Fatalf("GetPackageHash: %v", gerr)
	}
	if got.DeclaredSHA256 != hashA {
		t.Errorf("stored hash = %s, want %s (conflict must not overwrite)", got.DeclaredSHA256, hashA)
	}
}

// TestInsertReconciledMembers_RejectsConflictingBlob locks the
// security-critical loud-error branch: a member whose path already exists with
// a DIFFERENT blob must be rejected (no silent overwrite of the serving blob).
func TestInsertReconciledMembers_RejectsConflictingBlob(t *testing.T) {
	ctx := context.Background()
	c := openCache(t)
	snapID := commitTestSnapshotInRelease(t, c)

	blobA := strings.Repeat("a", 64)
	blobB := strings.Repeat("b", 64)
	writeTestBlob(t, c, blobA, []byte("the bytes that are actually serving"))
	writeTestBlob(t, c, blobB, []byte("a different blob the caller must not be able to substitute"))

	path := "main/binary-all/Packages"
	// First insert lands blob A under the path.
	if err := c.InsertReconciledMembers(ctx, snapID, []SnapshotMember{
		{SnapshotID: snapID, Path: path, BlobHash: blobA, DeclaredSHA256: blobA},
	}, nil); err != nil {
		t.Fatalf("seed insert (blob A): %v", err)
	}
	if got := blobRefcount(t, c, blobA); got != 1 {
		t.Fatalf("blobA refcount after seed = %d, want 1", got)
	}

	// Now reconcile tries to put a DIFFERENT blob B at the same path.
	err := c.InsertReconciledMembers(ctx, snapID, []SnapshotMember{
		{SnapshotID: snapID, Path: path, BlobHash: blobB, DeclaredSHA256: blobB},
	}, nil)
	if err == nil {
		t.Fatal("expected error for conflicting blob on existing path; got nil")
	}

	// The stored row must still read blob A — no silent overwrite.
	got, err := c.GetSnapshotMember(ctx, snapID, path)
	if err != nil {
		t.Fatalf("GetSnapshotMember after rejected conflict: %v", err)
	}
	if got.BlobHash != blobA {
		t.Errorf("stored blob_hash = %s, want %s (the conflicting insert must not overwrite)", got.BlobHash, blobA)
	}
	// And blob B must not have gained a pin from the rolled-back transaction.
	if rc := blobRefcount(t, c, blobB); rc != 0 {
		t.Errorf("blobB refcount = %d, want 0 (rejected/rolled-back insert must not bump)", rc)
	}
}

func TestInsertReconciledMembers_NotCurrent(t *testing.T) {
	ctx := context.Background()
	c := openCache(t)
	snapID := commitTestSnapshotInRelease(t, c)
	displaceCurrentSnapshot(t, c, snapID) // point suite_freshness.current_snapshot_id elsewhere

	blob := strings.Repeat("e", 64)
	writeTestBlob(t, c, blob, []byte("x"))
	members := []SnapshotMember{{SnapshotID: snapID, Path: "main/binary-all/Packages", BlobHash: blob, DeclaredSHA256: blob}}
	err := c.InsertReconciledMembers(ctx, snapID, members, nil)
	if !errorsIs(err, ErrSnapshotNotCurrent) {
		t.Fatalf("want ErrSnapshotNotCurrent, got %v", err)
	}
}
