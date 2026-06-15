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
	members := []SnapshotMember{
		{SnapshotID: snapID, Path: "main/binary-all/Packages", BlobHash: blob, DeclaredSHA256: blob},
	}
	if err := c.InsertReconciledMembers(ctx, snapID, members, nil); err != nil {
		t.Fatalf("InsertReconciledMembers: %v", err)
	}
	if _, err := c.GetSnapshotMember(ctx, snapID, "main/binary-all/Packages"); err != nil {
		t.Errorf("member not inserted: %v", err)
	}
	// Idempotent re-insert is a no-op.
	if err := c.InsertReconciledMembers(ctx, snapID, members, nil); err != nil {
		t.Errorf("re-insert should be idempotent, got %v", err)
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
