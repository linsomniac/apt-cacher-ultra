package cache

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// openV4Cache opens a cache directory and runs migrations 0→1→2→3→4,
// leaving the database at exactly schema v4 so tests can exercise the
// v4→v5 migration in isolation. Mirrors openV3Cache (gc_test.go).
func openV4Cache(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"pool", "tmp", "staging"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	db, err := openDB(filepath.Join(dir, "cache.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	for v := 0; v < 4; v++ {
		if err := applyMigration(context.Background(), db, v); err != nil {
			_ = db.Close()
			t.Fatalf("applyMigration v%d→v%d: %v", v, v+1, err)
		}
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, dir
}

func hasTable(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var got string
	err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, name,
	).Scan(&got)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false
	case err != nil:
		t.Fatalf("probe table %s: %v", name, err)
	}
	return true
}

func TestMigration_V4ToV5_AddsSkippedMemberTable(t *testing.T) {
	db, _ := openV4Cache(t)
	ctx := context.Background()

	if hasTable(t, db, "snapshot_skipped_member") {
		t.Fatal("v4 db already has snapshot_skipped_member; expected pristine v4")
	}

	if err := applyMigration(ctx, db, 4); err != nil {
		t.Fatalf("applyMigration v4→v5: %v", err)
	}

	if !hasTable(t, db, "snapshot_skipped_member") {
		t.Error("after migration, snapshot_skipped_member table missing")
	}
	v, err := readSchemaVersion(ctx, db)
	if err != nil {
		t.Fatalf("readSchemaVersion: %v", err)
	}
	if v != 5 {
		t.Errorf("schema_version = %d, want 5", v)
	}
}

// commitSnapshotWithSkips is the shared fixture: one adopted (current)
// snapshot for (http, x.example, /dists/s) carrying a single member
// (InRelease) plus the provided skipped-member rows. Returns the
// snapshot id and the InRelease blob hash.
func commitSnapshotWithSkips(t *testing.T, c *Cache, skipped []SkippedMember) int64 {
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
		t.Fatalf("InsertCandidateSnapshot: %v", err)
	}
	for i := range skipped {
		skipped[i].SnapshotID = id
	}
	members := []SnapshotMember{
		{SnapshotID: id, Path: "InRelease", BlobHash: rel, DeclaredSHA256: rel},
	}
	if err := c.CommitAdoption(ctx, id, members, skipped, nil, nil, false); err != nil {
		t.Fatalf("CommitAdoption: %v", err)
	}
	return id
}

func TestCommitAdoption_RecordsSkippedMembers(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	declaredA := strings.Repeat("a", 64)
	declaredB := strings.Repeat("b", 64)
	id := commitSnapshotWithSkips(t, c, []SkippedMember{
		{
			Path:           "main/Contents-amd64.gz",
			DeclaredSHA256: declaredA,
			Size:           4406247,
			Reason:         SkipReasonOptionalMemberIntegrity,
			Detail:         "served 4360619 vs declared 4406247",
		},
		{
			Path:           "main/Contents-amd64", // uncompressed decoy
			DeclaredSHA256: declaredB,
			Size:           123,
			Reason:         "4xx",
			Detail:         "",
		},
	})

	// Only the integrity-class row is repairable; the 4xx row is a
	// permanent publication artifact and must not surface here.
	rows, err := c.ListRepairableSkippedMembers(ctx, id)
	if err != nil {
		t.Fatalf("ListRepairableSkippedMembers: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d repairable rows, want 1 (%v)", len(rows), rows)
	}
	got := rows[0]
	if got.Path != "main/Contents-amd64.gz" {
		t.Errorf("path = %q, want main/Contents-amd64.gz", got.Path)
	}
	if got.DeclaredSHA256 != declaredA {
		t.Errorf("declared_sha256 = %s, want %s", got.DeclaredSHA256, declaredA)
	}
	if got.Size != 4406247 {
		t.Errorf("size = %d, want 4406247", got.Size)
	}
	if got.Reason != SkipReasonOptionalMemberIntegrity {
		t.Errorf("reason = %q, want %q", got.Reason, SkipReasonOptionalMemberIntegrity)
	}
	if got.Detail != "served 4360619 vs declared 4406247" {
		t.Errorf("detail = %q", got.Detail)
	}
	if got.RetryCount != 0 {
		t.Errorf("retry_count = %d, want 0", got.RetryCount)
	}
}

func TestCommitAdoption_RejectsMalformedSkippedHash(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	rel := seedBlob(t, c, "InRelease body")
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "x.example",
		SuitePath:       "/dists/s",
		InReleaseHash:   &rel,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = c.CommitAdoption(ctx, id,
		[]SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: rel, DeclaredSHA256: rel}},
		[]SkippedMember{{SnapshotID: id, Path: "x", DeclaredSHA256: "nothex", Reason: "4xx"}},
		nil, nil, false)
	if err == nil {
		t.Fatal("expected error on malformed skipped declared_sha256; got nil")
	}
}

func TestRepairSkippedMember_InsertsMembersAndClearsSkip(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	// The repaired bytes arrive later (e.g. mirror finished syncing);
	// seed them now to stand in for the post-fetch PutBlob.
	contents := seedBlob(t, c, "Contents-amd64.gz bytes")
	id := commitSnapshotWithSkips(t, c, []SkippedMember{
		{
			Path:           "main/Contents-amd64.gz",
			DeclaredSHA256: contents,
			Size:           23,
			Reason:         SkipReasonOptionalMemberIntegrity,
			Detail:         "served 1 vs declared 23",
		},
	})

	alias := "main/by-hash/SHA256/" + contents
	err := c.RepairSkippedMember(ctx, id, "main/Contents-amd64.gz", []SnapshotMember{
		{SnapshotID: id, Path: "main/Contents-amd64.gz", BlobHash: contents, DeclaredSHA256: contents},
		{SnapshotID: id, Path: alias, BlobHash: contents, DeclaredSHA256: contents},
	})
	if err != nil {
		t.Fatalf("RepairSkippedMember: %v", err)
	}

	// Both rows resolve through the §6.1 member lookup.
	for _, p := range []string{"main/Contents-amd64.gz", alias} {
		if _, err := c.GetSnapshotMember(ctx, id, p); err != nil {
			t.Errorf("GetSnapshotMember(%q): %v", p, err)
		}
	}
	// One distinct new blob → exactly one refcount bump despite two rows.
	if got := blobRefcount(t, c, contents); got != 1 {
		t.Errorf("refcount = %d, want 1", got)
	}
	// Skip row consumed.
	rows, err := c.ListRepairableSkippedMembers(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("skip row still present after repair: %v", rows)
	}
}

func TestRepairSkippedMember_RefusesNonCurrentSnapshot(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	contents := seedBlob(t, c, "Contents bytes")
	id := commitSnapshotWithSkips(t, c, []SkippedMember{
		{
			Path:           "main/Contents-amd64.gz",
			DeclaredSHA256: contents,
			Size:           14,
			Reason:         SkipReasonOptionalMemberIntegrity,
		},
	})

	// Displace: adopt a second snapshot for the same suite.
	rel2 := seedBlob(t, c, "InRelease v2")
	id2, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "x.example",
		SuitePath:       "/dists/s",
		InReleaseHash:   &rel2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id2, []SnapshotMember{
		{SnapshotID: id2, Path: "InRelease", BlobHash: rel2, DeclaredSHA256: rel2},
	}, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}

	err = c.RepairSkippedMember(ctx, id, "main/Contents-amd64.gz", []SnapshotMember{
		{SnapshotID: id, Path: "main/Contents-amd64.gz", BlobHash: contents, DeclaredSHA256: contents},
	})
	if !errors.Is(err, ErrSnapshotNotCurrent) {
		t.Fatalf("err = %v, want ErrSnapshotNotCurrent", err)
	}
	// Nothing inserted, no refcount movement.
	if _, err := c.GetSnapshotMember(ctx, id, "main/Contents-amd64.gz"); !errors.Is(err, ErrNotFound) {
		t.Errorf("member row inserted despite refusal (err=%v)", err)
	}
	if got := blobRefcount(t, c, contents); got != 0 {
		t.Errorf("refcount = %d, want 0", got)
	}
}

func TestRepairSkippedMember_NoDoubleBumpForBlobAlreadyInSnapshot(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	// The skipped member's bytes turn out to be identical to a blob the
	// snapshot already carries under another path (e.g. .gz and plain
	// variants with identical content). The repair must NOT bump again:
	// displacement decrements once per distinct blob, so a double bump
	// would leak the blob forever.
	shared := seedBlob(t, c, "shared bytes")
	rel := seedBlob(t, c, "InRelease body")
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "x.example",
		SuitePath:       "/dists/s",
		InReleaseHash:   &rel,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = c.CommitAdoption(ctx, id,
		[]SnapshotMember{
			{SnapshotID: id, Path: "InRelease", BlobHash: rel, DeclaredSHA256: rel},
			{SnapshotID: id, Path: "other/Path", BlobHash: shared, DeclaredSHA256: shared},
		},
		[]SkippedMember{{
			SnapshotID:     id,
			Path:           "main/Contents-amd64.gz",
			DeclaredSHA256: shared,
			Size:           12,
			Reason:         SkipReasonOptionalMemberIntegrity,
		}},
		nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := blobRefcount(t, c, shared); got != 1 {
		t.Fatalf("pre-repair refcount = %d, want 1", got)
	}

	if err := c.RepairSkippedMember(ctx, id, "main/Contents-amd64.gz", []SnapshotMember{
		{SnapshotID: id, Path: "main/Contents-amd64.gz", BlobHash: shared, DeclaredSHA256: shared},
	}); err != nil {
		t.Fatalf("RepairSkippedMember: %v", err)
	}
	if got := blobRefcount(t, c, shared); got != 1 {
		t.Errorf("post-repair refcount = %d, want 1 (no double bump)", got)
	}

	// Displace the snapshot; the shared blob must land at exactly 0.
	rel2 := seedBlob(t, c, "InRelease v2")
	id2, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "x.example",
		SuitePath:       "/dists/s",
		InReleaseHash:   &rel2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id2, []SnapshotMember{
		{SnapshotID: id2, Path: "InRelease", BlobHash: rel2, DeclaredSHA256: rel2},
	}, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	if got := blobRefcount(t, c, shared); got != 0 {
		t.Errorf("post-displacement refcount = %d, want 0", got)
	}
}

func TestBumpSkippedMemberRetry(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	declared := strings.Repeat("d", 64)
	id := commitSnapshotWithSkips(t, c, []SkippedMember{
		{
			Path:           "main/Contents-amd64.gz",
			DeclaredSHA256: declared,
			Size:           7,
			Reason:         SkipReasonOptionalMemberIntegrity,
		},
	})

	for i := 0; i < 2; i++ {
		if err := c.BumpSkippedMemberRetry(ctx, id, "main/Contents-amd64.gz"); err != nil {
			t.Fatalf("BumpSkippedMemberRetry #%d: %v", i+1, err)
		}
	}
	rows, err := c.ListRepairableSkippedMembers(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].RetryCount != 2 {
		t.Errorf("retry_count = %d, want 2", rows[0].RetryCount)
	}
}

func TestSnapshotGC_ReapsSkippedMemberRows(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	declared := strings.Repeat("e", 64)
	id := commitSnapshotWithSkips(t, c, []SkippedMember{
		{
			Path:           "main/Contents-amd64.gz",
			DeclaredSHA256: declared,
			Size:           7,
			Reason:         SkipReasonOptionalMemberIntegrity,
		},
	})

	// Displace, then GC with keep_displaced = 0.
	rel2 := seedBlob(t, c, "InRelease v2")
	id2, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "x.example",
		SuitePath:       "/dists/s",
		InReleaseHash:   &rel2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id2, []SnapshotMember{
		{SnapshotID: id2, Path: "InRelease", BlobHash: rel2, DeclaredSHA256: rel2},
	}, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}

	res, err := c.RunSnapshotGCBatch(ctx, 10, 3600, 0)
	if err != nil {
		t.Fatalf("RunSnapshotGCBatch: %v", err)
	}
	if res.DisplacedReaped != 1 {
		t.Fatalf("displaced_reaped = %d, want 1", res.DisplacedReaped)
	}
	var n int
	if err := c.db.QueryRow(
		`SELECT count(*) FROM snapshot_skipped_member WHERE snapshot_id = ?`, id,
	).Scan(&n); err != nil {
		t.Fatalf("count skip rows: %v", err)
	}
	if n != 0 {
		t.Errorf("snapshot_skipped_member rows for reaped snapshot = %d, want 0", n)
	}
}
