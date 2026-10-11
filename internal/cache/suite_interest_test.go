package cache

import (
	"context"
	"database/sql"
	"testing"
)

// A suite row created after v7 starts with a client-interest stamp
// (otherwise phase F could never reap it); later writes keep it.
func TestPutSuiteFreshness_StampsInterestOnInsertOnly(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	s := SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}
	restore := stubNow(t, 1_800_000_000)
	if err := c.PutSuiteFreshness(ctx, s); err != nil {
		t.Fatal(err)
	}
	restore()
	defer stubNow(t, 1_800_009_999)()
	later := int64(1_800_009_999)
	s.LastCheckAt = &later
	if err := c.PutSuiteFreshness(ctx, s); err != nil {
		t.Fatal(err)
	}
	var lr sql.NullInt64
	if err := c.db.QueryRow(`SELECT last_requested_at FROM suite_freshness WHERE suite_path = '/r/dists/x'`).Scan(&lr); err != nil {
		t.Fatal(err)
	}
	if !lr.Valid || lr.Int64 != 1_800_000_000 {
		t.Fatalf("last_requested_at = %v, want 1800000000 (set on insert, kept on update)", lr)
	}
}

// CommitAdoption's Step 6 can create the suite row too; it gets the stamp.
func TestCommitAdoption_NewSuiteRowGetsInterestStamp(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, 1_800_000_000)()
	adopted := seedBlob(t, c, "inrelease bytes (interest stamp)")
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/new", InReleaseHash: &adopted})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id, []SnapshotMember{
		{SnapshotID: id, Path: "InRelease", BlobHash: adopted, DeclaredSHA256: adopted},
	}, nil, nil, nil, false); err != nil {
		t.Fatalf("CommitAdoption: %v", err)
	}
	var lr sql.NullInt64
	if err := c.db.QueryRow(`SELECT last_requested_at FROM suite_freshness WHERE suite_path = '/r/dists/new'`).Scan(&lr); err != nil {
		t.Fatal(err)
	}
	if !lr.Valid || lr.Int64 != 1_800_000_000 {
		t.Fatalf("last_requested_at = %v, want 1800000000", lr)
	}
}
