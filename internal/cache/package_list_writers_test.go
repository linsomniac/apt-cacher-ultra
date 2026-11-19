package cache

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
)

func generationOf(t *testing.T, c *Cache, key SuiteKey) int64 {
	t.Helper()
	var g int64
	if err := c.db.QueryRow(`SELECT package_list_generation FROM suite_freshness
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`,
		key.CanonicalScheme, key.CanonicalHost, key.SuitePath).Scan(&g); err != nil {
		t.Fatalf("package_list_generation of %v: %v", key, err)
	}
	return g
}

// livePaths is the suite's live list, as the readers reach it.
func liveRowsOf(t *testing.T, c *Cache, key SuiteKey) []PackageHash {
	t.Helper()
	var current sql.NullInt64
	if err := c.db.QueryRow(`SELECT current_snapshot_id FROM suite_freshness
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`,
		key.CanonicalScheme, key.CanonicalHost, key.SuitePath).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if !current.Valid {
		return nil
	}
	rows, err := c.PackageRowsOfSnapshotForTest(context.Background(), current.Int64)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// A signed commit leaves its suite naming a list that holds exactly the
// rows it was given: a new list each time, the previous one unnamed. An
// unsigned commit leaves the suite naming none. Both bump the generation.
func TestCommitAdoption_ASignedCommitNamesANewListAndAnUnsignedOneNone(t *testing.T) {
	c := openCache(t)
	key := sk("h", "/ubuntu/dists/jammy")
	seedRow(t, c, key, 1)
	if got := listIDOf(t, c, key); got.Valid || generationOf(t, c, key) != 0 {
		t.Fatalf("a new suite names list %v at generation %d, want none at 0", got, generationOf(t, c, key))
	}

	adoptPackages(t, c, key, []PackageHash{pkgRow("/ubuntu/pool/a.deb", hashOf('a'), ""), pkgRow("/ubuntu/pool/b.deb", hashOf('b'), "")}, true)
	first := listIDOf(t, c, key)
	if !first.Valid || generationOf(t, c, key) != 1 {
		t.Fatalf("after the first adoption: list %v, generation %d; want a list at 1", first, generationOf(t, c, key))
	}
	if got := pathsOf(liveRowsOf(t, c, key)); !slices.Equal(got, []string{"/ubuntu/pool/a.deb", "/ubuntu/pool/b.deb"}) {
		t.Fatalf("live rows = %v", got)
	}

	adoptPackages(t, c, key, []PackageHash{pkgRow("/ubuntu/pool/b.deb", hashOf('c'), "")}, true)
	second := listIDOf(t, c, key)
	if !second.Valid || second.Int64 <= first.Int64 || generationOf(t, c, key) != 2 {
		t.Fatalf("after the second adoption: list %v (first %v), generation %d; want a newer list at 2", second, first, generationOf(t, c, key))
	}
	live := liveRowsOf(t, c, key)
	if len(live) != 1 || live[0].Path != "/ubuntu/pool/b.deb" || live[0].DeclaredSHA256 != hashOf('c') {
		t.Fatalf("live rows after the second adoption = %+v, want only b.deb with its new hash", live)
	}
	if got := unnamedLists(t, c); !slices.Equal(got, []int64{first.Int64}) {
		t.Fatalf("unnamed lists = %v, want the first adoption's %d", got, first.Int64)
	}

	// A signed commit with no package rows names an empty list.
	empty := sk("h", "/meta/dists/only")
	seedRow(t, c, empty, 1)
	adoptPackages(t, c, empty, nil, false)
	if id := listIDOf(t, c, empty); !id.Valid || entryCount(t, c, id.Int64) != 0 {
		t.Fatalf("a signed suite without package rows names %v, want an empty list", id)
	}

	// An unsigned commit names none, and writes none of the rows it is given.
	plain := sk("h", "/plain/dists/s")
	seedRow(t, c, plain, 1)
	ctx := context.Background()
	rel := seedBlob(t, c, "an unsigned Release")
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h",
		SuitePath: plain.SuitePath, InReleaseHash: &rel, Form: FormUnsigned})
	if err != nil {
		t.Fatal(err)
	}
	before := 0
	if err := c.db.QueryRow(`SELECT count(*) FROM package_list_entry`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id, []SnapshotMember{{SnapshotID: id, Path: "Release", BlobHash: rel, DeclaredSHA256: rel}},
		nil, []PackageHash{pkgRow("/plain/pool/p.deb", hashOf('d'), "")}, nil, false); err != nil {
		t.Fatal(err)
	}
	after := 0
	if err := c.db.QueryRow(`SELECT count(*) FROM package_list_entry`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if got := listIDOf(t, c, plain); got.Valid || generationOf(t, c, plain) != 1 || after != before {
		t.Fatalf("after an unsigned commit: list %v, generation %d, %d rows written; want no list, 1, 0",
			got, generationOf(t, c, plain), after-before)
	}
	if got := declared(t, c, "/plain/pool/p.deb"); got != nil {
		t.Fatalf("an unsigned snapshot vouches: %v", got)
	}
}

// A commit for a suite whose row is gone fails as it always did.
func TestCommitAdoption_ASuiteWithoutARowIsStillErrSuiteGone(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	anchor := seedBlob(t, c, "anchor")
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h",
		SuitePath: "/gone/dists/s", InReleaseHash: &anchor})
	if err != nil {
		t.Fatal(err)
	}
	err = c.CommitAdoption(ctx, id, []SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: anchor, DeclaredSHA256: anchor}},
		nil, []PackageHash{pkgRow("/gone/pool/p.deb", hashOf('d'), "")}, nil, false)
	if err == nil || !strings.Contains(err.Error(), ErrSuiteGone.Error()) {
		t.Fatalf("CommitAdoption for a suite without a row = %v, want ErrSuiteGone", err)
	}
	var lists int
	if err := c.db.QueryRow(`SELECT count(*) FROM package_list`).Scan(&lists); err != nil || lists != 0 {
		t.Fatalf("%d lists were left behind (%v), want none: the commit rolled back", lists, err)
	}
}

// Reconcile adds a recovered index's rows to the suite's live list, in
// place, and bumps the generation only when it wrote.
func TestInsertReconciledMembers_AddsToTheLiveListAndBumpsTheGeneration(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/ubuntu/dists/jammy")
	seedRow(t, c, key, 1)
	id := adoptPackages(t, c, key, []PackageHash{pkgRow("/ubuntu/pool/a.deb", hashOf('a'), "")}, true)
	list, gen := listIDOf(t, c, key), generationOf(t, c, key)

	healed := pkgRow("/ubuntu/pool/healed.deb", hashOf('b'), "")
	if err := c.InsertReconciledMembers(ctx, id, nil, []PackageHash{healed}); err != nil {
		t.Fatal(err)
	}
	if got := listIDOf(t, c, key); got != list {
		t.Fatalf("the reconcile changed the suite's list from %v to %v", list, got)
	}
	if got := generationOf(t, c, key); got != gen+1 {
		t.Fatalf("generation after a reconcile that wrote = %d, want %d", got, gen+1)
	}
	if got := pathsOf(liveRowsOf(t, c, key)); !slices.Equal(got, []string{"/ubuntu/pool/a.deb", "/ubuntu/pool/healed.deb"}) {
		t.Fatalf("live rows = %v", got)
	}
	if got := declared(t, c, "/ubuntu/pool/healed.deb"); !slices.Equal(got, []string{"b@/ubuntu/dists/jammy"}) {
		t.Fatalf("the healed row vouches %v", got)
	}

	// The same rows again: nothing written, the generation stays.
	if err := c.InsertReconciledMembers(ctx, id, nil, []PackageHash{healed}); err != nil {
		t.Fatal(err)
	}
	if got := generationOf(t, c, key); got != gen+1 {
		t.Fatalf("generation after an idempotent reconcile = %d, want %d unchanged", got, gen+1)
	}
	// A disagreeing hash is still loud, and writes nothing.
	clash := pkgRow("/ubuntu/pool/healed.deb", hashOf('c'), "")
	if err := c.InsertReconciledMembers(ctx, id, nil, []PackageHash{pkgRow("/ubuntu/pool/new.deb", hashOf('d'), ""), clash}); err == nil {
		t.Fatal("a reconcile with a conflicting hash succeeded")
	}
	if got := pathsOf(liveRowsOf(t, c, key)); len(got) != 2 || generationOf(t, c, key) != gen+1 {
		t.Fatalf("after a failed reconcile: rows %v, generation %d; want both unchanged", got, generationOf(t, c, key))
	}
}

// A suite whose current snapshot is unsigned names no list, and a
// reconcile cannot give it package rows.
func TestInsertReconciledMembers_RefusesRowsForASuiteWithoutAList(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/plain/dists/s")
	seedRow(t, c, key, 1)
	rel := seedBlob(t, c, "an unsigned Release")
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h",
		SuitePath: key.SuitePath, InReleaseHash: &rel, Form: FormUnsigned})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id, []SnapshotMember{{SnapshotID: id, Path: "Release", BlobHash: rel, DeclaredSHA256: rel}},
		nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	err = c.InsertReconciledMembers(ctx, id, nil, []PackageHash{pkgRow("/plain/pool/p.deb", hashOf('a'), "")})
	if err == nil || !strings.Contains(err.Error(), "no package list") {
		t.Fatalf("InsertReconciledMembers with rows for a suite without a list = %v, want a refusal", err)
	}
	var n int
	if err := c.db.QueryRow(`SELECT count(*) FROM package_list_entry`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d rows written (%v), want none", n, err)
	}
	// Without package rows the reconcile has nothing to refuse.
	if err := c.InsertReconciledMembers(ctx, id, nil, nil); err != nil {
		t.Fatalf("a reconcile without package rows: %v", err)
	}
}

// A file requested before its index was recovered gets its package's
// interest recorded by the reconcile that adds the row: no later commit
// visits an unchanged row.
func TestInsertReconciledMembers_RecordsInterestForTheRowsItInserts(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/ubuntu/dists/jammy")
	seedRow(t, c, key, 1)
	id := adoptPackages(t, c, key, []PackageHash{pkgRow("/ubuntu/pool/a.deb", hashOf('a'), "")}, true)

	sha := hashOf('b')
	if _, err := c.db.Exec(`INSERT INTO blob (hash, size, created_at) VALUES (?, 1, 0)`, sha); err != nil {
		t.Fatal(err)
	}
	at := int64(4242)
	if err := c.PutURLPath(ctx, URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: "/ubuntu/pool/healed.deb", BlobHash: &sha,
		UpstreamURL: "http://up/healed", LastRequestedAt: &at, RequestCount: 1, LastFetchedAt: &at}); err != nil {
		t.Fatal(err)
	}
	if _, ok := hotStamp(t, c, key, "healed"); ok {
		t.Fatal("interest was recorded before any list declared the file")
	}
	healed := PackageHash{CanonicalScheme: "http", CanonicalHost: "h", Path: "/ubuntu/pool/healed.deb", DeclaredSHA256: sha,
		PackageName: "healed", Architecture: "amd64", Version: "1"}
	if err := c.InsertReconciledMembers(ctx, id, nil, []PackageHash{healed}); err != nil {
		t.Fatal(err)
	}
	if got, ok := hotStamp(t, c, key, "healed"); !ok || got != at {
		t.Fatalf("interest after the reconcile = %d, %v; want %d", got, ok, at)
	}
}

// A quarantine of a suite's current snapshot takes the suite's list with
// the pointer: nothing vouches, and the list is left for GC.
func TestQuarantine_ClearsTheSuitesList(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/ubuntu/dists/jammy")
	seedRow(t, c, key, 1)
	id := adoptPackages(t, c, key, []PackageHash{pkgRow("/ubuntu/pool/a.deb", hashOf('a'), "")}, true)
	list, gen := listIDOf(t, c, key), generationOf(t, c, key)
	if _, err := c.ApplyLegacyAuditDecisions(ctx, []LegacyAuditDecision{{SnapshotID: id, Outcome: LegacyAuditQuarantine}}); err != nil {
		t.Fatal(err)
	}
	if got := listIDOf(t, c, key); got.Valid {
		t.Fatalf("the suite still names list %d after its snapshot's quarantine", got.Int64)
	}
	if got := generationOf(t, c, key); got != gen+1 {
		t.Fatalf("generation = %d, want %d", got, gen+1)
	}
	if got := declared(t, c, "/ubuntu/pool/a.deb"); got != nil {
		t.Fatalf("a quarantined snapshot's row vouches: %v", got)
	}
	if got := unnamedLists(t, c); !slices.Equal(got, []int64{list.Int64}) {
		t.Fatalf("unnamed lists = %v, want %d", got, list.Int64)
	}
}
