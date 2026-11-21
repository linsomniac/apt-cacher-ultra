package cache

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
)

var publicationSeq atomic.Int64

// signedCandidate inserts a signed (inline) candidate of key with an
// anchor no other candidate has, and returns its id and its one member.
func signedCandidate(t *testing.T, c *Cache, key SuiteKey) (int64, []SnapshotMember) {
	t.Helper()
	anchor := seedBlob(t, c, fmt.Sprintf("publication %d of %s%s", publicationSeq.Add(1), key.CanonicalHost, key.SuitePath))
	id, _, err := c.InsertCandidateSnapshot(context.Background(), SnapshotCandidate{CanonicalScheme: key.CanonicalScheme,
		CanonicalHost: key.CanonicalHost, SuitePath: key.SuitePath, InReleaseHash: &anchor})
	if err != nil {
		t.Fatal(err)
	}
	return id, []SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: anchor, DeclaredSHA256: anchor}}
}

// publish adopts rows as key's next publication the way the adopter does:
// the difference to the live list is read first, then handed to the commit.
func publish(t *testing.T, c *Cache, key SuiteKey, rows []PackageHash) (int64, PackageListWrite) {
	t.Helper()
	ctx := context.Background()
	id, members := signedCandidate(t, c, key)
	diff, err := c.DiffPackageList(ctx, key, rows)
	if err != nil {
		t.Fatalf("DiffPackageList: %v", err)
	}
	w, err := c.CommitAdoptionDiff(ctx, id, members, nil, rows, nil, true, diff)
	if err != nil {
		t.Fatalf("CommitAdoptionDiff: %v", err)
	}
	return id, w
}

// requireLive fails unless key's live list is exactly want (every stored
// column; the order of want does not matter).
func requireLive(t *testing.T, c *Cache, key SuiteKey, want []PackageHash) {
	t.Helper()
	got := liveRowsOf(t, c, key)
	sorted := slices.Clone(want)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	if len(got) != len(sorted) {
		t.Fatalf("the live list holds %d rows, want %d", len(got), len(sorted))
	}
	for i := range got {
		g, w := got[i], sorted[i]
		g.SnapshotID, w.SnapshotID = 0, 0
		if g != w {
			t.Fatalf("live row %d = %+v, want %+v", i, g, w)
		}
	}
}

func numbered(n int, letter byte) []PackageHash {
	out := make([]PackageHash, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, PackageHash{CanonicalScheme: "http", CanonicalHost: "h", Path: fmt.Sprintf("/ubuntu/pool/p%06d.deb", i),
			DeclaredSHA256: hashOf(letter), PackageName: fmt.Sprintf("p%06d", i), Architecture: "amd64", Version: "1"})
	}
	return out
}

func listRowids(t *testing.T, c *Cache, listID int64) string {
	t.Helper()
	var ids string
	if err := c.db.QueryRow(`SELECT coalesce(group_concat(rowid), '') FROM
 (SELECT rowid FROM package_list_entry WHERE package_list_id = ? ORDER BY path)`, listID).Scan(&ids); err != nil {
		t.Fatal(err)
	}
	return ids
}

// The point of the design: a publication that changed a few rows writes
// those rows into the suite's list, in place.
func TestCommitDiff_ASmallChangeIsWrittenInPlace(t *testing.T) {
	c := openCache(t)
	key := sk("h", "/ubuntu/dists/jammy")
	seedRow(t, c, key, 1)
	rows := numbered(1000, 'a')
	if _, w := publish(t, c, key, rows); w.Mode != PackageListRewrite || w.Reason != "first" || w.Added != 1000 {
		t.Fatalf("the first publication's write = %+v, want a rewrite (first) of 1000 rows", w)
	}
	list, gen := listIDOf(t, c, key), generationOf(t, c, key)

	next := slices.Clone(rows[3:]) // 3 removed
	next[0].DeclaredSHA256, next[1].DeclaredSHA256 = hashOf('b'), hashOf('c')
	next[2].Version, next[3].Version = "2", "3" // 4 changed: two a hash, two a version only
	for i := 0; i < 5; i++ {                    // 5 added
		next = append(next, PackageHash{CanonicalScheme: "http", CanonicalHost: "h", Path: fmt.Sprintf("/ubuntu/pool/new%d.deb", i),
			DeclaredSHA256: hashOf('d'), PackageName: "new", Architecture: "amd64", Version: "1"})
	}
	id, w := publish(t, c, key, next)
	if want := (PackageListWrite{Mode: PackageListDifference, Added: 5, Changed: 4, Removed: 3}); w != want {
		t.Fatalf("the write = %+v, want %+v", w, want)
	}
	requireLive(t, c, key, next)
	if got := listIDOf(t, c, key); got != list {
		t.Fatalf("the suite's list changed from %v to %v: a difference is written in place", list, got)
	}
	if got := generationOf(t, c, key); got != gen+1 {
		t.Fatalf("generation = %d, want %d", got, gen+1)
	}
	if got := unnamedLists(t, c); len(got) != 0 {
		t.Fatalf("unnamed lists after a difference: %v, want none (nothing for GC)", got)
	}
	// Readers see the new snapshot with the new rows.
	rowsOf, err := c.DeclaredHashesForPath(context.Background(), "http", "h", next[0].Path)
	if err != nil || len(rowsOf) != 1 || rowsOf[0].DeclaredSHA256 != hashOf('b') || rowsOf[0].SnapshotID != id {
		t.Fatalf("DeclaredHashesForPath(a changed path) = %+v, %v; want the new hash under snapshot %d", rowsOf, err, id)
	}
	if got := declared(t, c, rows[0].Path); got != nil {
		t.Fatalf("a removed path is still vouched: %v", got)
	}
}

func TestCommitDiff_AnIdenticalListWritesNoRow(t *testing.T) {
	c := openCache(t)
	key := sk("h", "/ubuntu/dists/jammy")
	seedRow(t, c, key, 1)
	rows := numbered(200, 'a')
	publish(t, c, key, rows)
	list, gen := listIDOf(t, c, key), generationOf(t, c, key)
	before := listRowids(t, c, list.Int64)
	_, w := publish(t, c, key, slices.Clone(rows))
	if want := (PackageListWrite{Mode: PackageListDifference}); w != want {
		t.Fatalf("the write = %+v, want %+v", w, want)
	}
	if got := listRowids(t, c, list.Int64); got != before || listIDOf(t, c, key) != list {
		t.Fatal("an identical publication rewrote rows of the list")
	}
	if got := generationOf(t, c, key); got != gen+1 {
		t.Fatalf("generation = %d, want %d: every commit bumps it", got, gen+1)
	}
	requireLive(t, c, key, rows)
}

// An empty live list and an empty publication: nothing to write, in place.
func TestCommitDiff_AnEmptyListStaysInPlace(t *testing.T) {
	c := openCache(t)
	key := sk("h", "/meta/dists/only")
	seedRow(t, c, key, 1)
	publish(t, c, key, nil)
	list := listIDOf(t, c, key)
	if _, w := publish(t, c, key, nil); w.Mode != PackageListDifference || listIDOf(t, c, key) != list {
		t.Fatalf("an empty publication over an empty list: %+v, list %v (was %v)", w, listIDOf(t, c, key), list)
	}
}

// The rewrite path is taken once more than half the new list differs: at
// half, a difference costs what a rewrite does (the plan's measurement).
func TestCommitDiff_MoreThanHalfRewrites(t *testing.T) {
	for _, tc := range []struct {
		changed int
		mode    string
	}{{50, PackageListDifference}, {51, PackageListRewrite}} {
		t.Run(fmt.Sprintf("%d of 100 changed", tc.changed), func(t *testing.T) {
			c := openCache(t)
			key := sk("h", "/ubuntu/dists/jammy")
			seedRow(t, c, key, 1)
			rows := numbered(100, 'a')
			publish(t, c, key, rows)
			list := listIDOf(t, c, key)
			next := slices.Clone(rows)
			for i := 0; i < tc.changed; i++ {
				next[i].DeclaredSHA256 = hashOf('b')
			}
			_, w := publish(t, c, key, next)
			if w.Mode != tc.mode {
				t.Fatalf("the write = %+v, want mode %s", w, tc.mode)
			}
			requireLive(t, c, key, next)
			if tc.mode == PackageListRewrite {
				if w.Reason != "large" || w.Added != 100 || w.Changed != 0 || w.Removed != 0 {
					t.Fatalf("the rewrite = %+v, want reason large and 100 rows added", w)
				}
				if got := listIDOf(t, c, key); got == list || !slices.Equal(unnamedLists(t, c), []int64{list.Int64}) {
					t.Fatalf("after a rewrite: list %v (was %v), unnamed %v; want a new list and the old one unnamed", got, list, unnamedLists(t, c))
				}
			} else if w.Changed != tc.changed || listIDOf(t, c, key) != list {
				t.Fatalf("the difference = %+v, list %v (was %v)", w, listIDOf(t, c, key), list)
			}
		})
	}
}

// A difference is applied only to the list, at the generation, it was
// computed against. Anything that wrote the list in between — a
// reconcile's row, another adoption — makes the commit write the whole
// list, which is exact whatever the list holds.
func TestCommitDiff_AListThatChangedSinceTheReadIsRewritten(t *testing.T) {
	for _, between := range []string{"a reconcile", "another adoption"} {
		t.Run(between, func(t *testing.T) {
			c := openCache(t)
			ctx := context.Background()
			key := sk("h", "/ubuntu/dists/jammy")
			seedRow(t, c, key, 1)
			rows := numbered(100, 'a')
			current, _ := publish(t, c, key, rows)

			next := slices.Clone(rows)
			next[0].DeclaredSHA256 = hashOf('b')
			id, members := signedCandidate(t, c, key)
			diff, err := c.DiffPackageList(ctx, key, next)
			if err != nil || diff == nil || diff.Size() != 1 {
				t.Fatalf("DiffPackageList = %+v, %v; want one changed row", diff, err)
			}
			if between == "a reconcile" {
				healed := PackageHash{CanonicalScheme: "http", CanonicalHost: "h", Path: "/ubuntu/pool/healed.deb",
					DeclaredSHA256: hashOf('e'), PackageName: "healed", Architecture: "amd64", Version: "1"}
				if err := c.InsertReconciledMembers(ctx, current, nil, []PackageHash{healed}); err != nil {
					t.Fatal(err)
				}
			} else {
				other := slices.Clone(rows)
				other[5].DeclaredSHA256 = hashOf('f')
				oid, omembers := signedCandidate(t, c, key)
				if err := c.CommitAdoption(ctx, oid, omembers, nil, other, nil, true); err != nil {
					t.Fatal(err)
				}
			}
			w, err := c.CommitAdoptionDiff(ctx, id, members, nil, next, nil, true, diff)
			if err != nil {
				t.Fatal(err)
			}
			if w.Mode != PackageListRewrite || w.Reason != "list_changed" || w.Added != 100 {
				t.Fatalf("the write = %+v, want a rewrite (list_changed) of 100 rows", w)
			}
			requireLive(t, c, key, next) // exactly the new rows: no healed row, no row of the other adoption
		})
	}
}

// The generation's own case: another adoption's difference was applied in
// place between this difference's read and its commit. The list keeps its
// id, so the generation alone says it was written. Applied all the same,
// this difference would leave a list that is neither publication's, with
// the right number of rows and every statement hitting its one row: no
// tripwire notices, and the cache would vouch the first publication's hash
// for a package of the second.
func TestCommitDiff_ADifferenceReadBeforeAnotherWasAppliedInPlaceIsNotApplied(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/ubuntu/dists/jammy")
	seedRow(t, c, key, 1)
	rows := numbered(100, 'a')
	publish(t, c, key, rows)
	list := listIDOf(t, c, key)

	// Two publications of that list: one changes row 0, the other row 1 and
	// has row 0 as it was. Both differences are read before either commits.
	first, second := slices.Clone(rows), slices.Clone(rows)
	first[0].DeclaredSHA256 = hashOf('b')
	second[1].DeclaredSHA256 = hashOf('c')
	firstID, firstMembers := signedCandidate(t, c, key)
	secondID, secondMembers := signedCandidate(t, c, key)
	firstDiff, err := c.DiffPackageList(ctx, key, first)
	if err != nil || firstDiff == nil || firstDiff.Size() != 1 {
		t.Fatalf("DiffPackageList for the first = %+v, %v; want one changed row", firstDiff, err)
	}
	secondDiff, err := c.DiffPackageList(ctx, key, second)
	if err != nil || secondDiff == nil || secondDiff.Size() != 1 {
		t.Fatalf("DiffPackageList for the second = %+v, %v; want one changed row", secondDiff, err)
	}
	w, err := c.CommitAdoptionDiff(ctx, firstID, firstMembers, nil, first, nil, true, firstDiff)
	if err != nil || w.Mode != PackageListDifference || listIDOf(t, c, key) != list {
		t.Fatalf("the first commit = %+v, %v, list %v (was %v); want its difference applied in place", w, err, listIDOf(t, c, key), list)
	}
	w, err = c.CommitAdoptionDiff(ctx, secondID, secondMembers, nil, second, nil, true, secondDiff)
	if err != nil {
		t.Fatal(err)
	}
	if w.Mode != PackageListRewrite || w.Reason != "list_changed" {
		t.Errorf("the second commit = %+v; want a rewrite (list_changed): its difference was read before the first was applied", w)
	}
	requireLive(t, c, key, second) // row 0 as the second publication has it, not as the first left it
}

// The count check: a difference that leaves the list with the wrong number
// of rows is undone and the list written whole. The adoption succeeds, the
// list is exact, and the result says what happened.
func TestCommitDiff_AWrongDifferenceIsCaughtAndTheListRewritten(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/ubuntu/dists/jammy")
	seedRow(t, c, key, 1)
	rows := numbered(100, 'a')
	publish(t, c, key, rows)

	next := slices.Clone(rows[2:]) // two removed
	next[0].DeclaredSHA256 = hashOf('b')
	id, members := signedCandidate(t, c, key)
	diff, err := c.DiffPackageList(ctx, key, next)
	if err != nil || len(diff.Removed) != 2 {
		t.Fatalf("DiffPackageList = %+v, %v", diff, err)
	}
	diff.Removed = diff.Removed[:1] // the defect: one removed path is missing
	w, err := c.CommitAdoptionDiff(ctx, id, members, nil, next, nil, true, diff)
	if err != nil {
		t.Fatalf("a wrong difference failed the adoption: %v", err)
	}
	if w.Mode != PackageListRewrite || w.Reason != "mismatch" || w.Added != 98 || w.Expected != 98 || w.Found != 99 || w.Detail == "" {
		t.Fatalf("the write = %+v, want a rewrite (mismatch) of 98 rows, expected 98, found 99, with a detail", w)
	}
	requireLive(t, c, key, next)
	suite, err := c.GetSuiteFreshness(ctx, "http", "h", key.SuitePath)
	if err != nil || suite.CurrentSnapshotID == nil || *suite.CurrentSnapshotID != id {
		t.Fatalf("the snapshot is not current after the rewrite: %+v, %v", suite, err)
	}
}

// The flip is one transaction: a commit that fails after it applied a
// difference leaves the list, and its generation, as they were.
func TestCommitDiff_AFailingCommitLeavesTheListAsItWas(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/ubuntu/dists/jammy")
	seedRow(t, c, key, 1)
	rows := numbered(50, 'a')
	prior, _ := publish(t, c, key, rows)
	list, gen := listIDOf(t, c, key), generationOf(t, c, key)
	before := listRowids(t, c, list.Int64)

	next := slices.Clone(rows[1:])
	next[0].DeclaredSHA256 = hashOf('b')
	next = append(next, PackageHash{CanonicalScheme: "http", CanonicalHost: "h", Path: "/ubuntu/pool/new.deb",
		DeclaredSHA256: hashOf('c'), PackageName: "new", Architecture: "amd64", Version: "1"})
	id, members := signedCandidate(t, c, key)
	diff, err := c.DiffPackageList(ctx, key, next)
	if err != nil {
		t.Fatal(err)
	}
	// The warmed-file step runs after the package list is written.
	bad := []PrefetchedURLPath{{CanonicalScheme: "http", CanonicalHost: "h", Path: "/ubuntu/pool/new.deb", BlobHash: "not a hash", UpstreamURL: "http://h/x"}}
	if _, err := c.CommitAdoptionDiff(ctx, id, members, nil, next, bad, true, diff); !errors.Is(err, ErrInvalidHash) {
		t.Fatalf("CommitAdoptionDiff = %v, want ErrInvalidHash", err)
	}
	requireLive(t, c, key, rows)
	if listIDOf(t, c, key) != list || generationOf(t, c, key) != gen || listRowids(t, c, list.Int64) != before {
		t.Fatal("a failed commit changed the suite's list, its generation or its rows")
	}
	suite, err := c.GetSuiteFreshness(ctx, "http", "h", key.SuitePath)
	if err != nil || suite.CurrentSnapshotID == nil || *suite.CurrentSnapshotID != prior {
		t.Fatalf("the prior snapshot is no longer current: %+v, %v", suite, err)
	}
	// The same difference still applies: nothing wrote the list.
	if w, err := c.CommitAdoptionDiff(ctx, id, members, nil, next, nil, true, diff); err != nil || w.Mode != PackageListDifference {
		t.Fatalf("the retry = %+v, %v; want the difference applied", w, err)
	}
	requireLive(t, c, key, next)
}

// An unsigned candidate has no package rows: its commit names no list,
// whatever difference it is handed.
func TestCommitDiff_AnUnsignedCommitNamesNoList(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/plain/dists/s")
	seedRow(t, c, key, 1)
	rel := seedBlob(t, c, "an unsigned Release of /plain")
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h",
		SuitePath: key.SuitePath, InReleaseHash: &rel, Form: FormUnsigned})
	if err != nil {
		t.Fatal(err)
	}
	stray := &PackageListDiff{ListID: 1, Added: numbered(3, 'a')}
	w, err := c.CommitAdoptionDiff(ctx, id, []SnapshotMember{{SnapshotID: id, Path: "Release", BlobHash: rel, DeclaredSHA256: rel}},
		nil, numbered(3, 'a'), nil, false, stray)
	if err != nil {
		t.Fatal(err)
	}
	if want := (PackageListWrite{Mode: PackageListNone}); w != want {
		t.Fatalf("the write = %+v, want %+v", w, want)
	}
	var entries int
	if err := c.db.QueryRow(`SELECT count(*) FROM package_list_entry`).Scan(&entries); err != nil || entries != 0 || listIDOf(t, c, key).Valid {
		t.Fatalf("an unsigned commit wrote %d package rows (%v) or named a list (%v)", entries, err, listIDOf(t, c, key))
	}
}

// Review focus 1: a difference larger than SQLite's bound-parameter limit
// (32,766) commits: rows are written one statement each, never as a list
// of parameters.
func TestCommitDiff_FortyThousandRemovedPathsCommit(t *testing.T) {
	if testing.Short() {
		t.Skip("120,000 rows")
	}
	c := openCache(t)
	key := sk("h", "/ubuntu/dists/jammy")
	seedRow(t, c, key, 1)
	rows := numbered(120_000, 'a')
	publish(t, c, key, rows)
	next := rows[40_000:]
	_, w := publish(t, c, key, next)
	if want := (PackageListWrite{Mode: PackageListDifference, Removed: 40_000}); w != want {
		t.Fatalf("the write = %+v, want %+v", w, want)
	}
	var n int
	if err := c.db.QueryRow(`SELECT count(*) FROM package_list_entry WHERE package_list_id = ?`, listIDOf(t, c, key).Int64).Scan(&n); err != nil || n != 80_000 {
		t.Fatalf("the live list holds %d rows (%v), want 80,000", n, err)
	}
}

// Review focus 2: the merge needs SQLite's ORDER BY path and Go's string
// order to be one order. A list with paths that sort differently under
// other collations differs from itself by nothing.
func TestDiffPackageList_AgreesWithSQLitesOrder(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/ubuntu/dists/jammy")
	seedRow(t, c, key, 1)
	var rows []PackageHash
	for _, p := range []string{"/z", "/a~b", "/A", "/é", "/a+b", "/a", "/a.b", "/B", "/a/b", "/a b", "/ä", "/a-b", "/a_b", "/aB", "/ab"} {
		rows = append(rows, PackageHash{CanonicalScheme: "http", CanonicalHost: "h", Path: p, DeclaredSHA256: hashOf('a'),
			PackageName: "p", Architecture: "amd64", Version: "1"})
	}
	publish(t, c, key, rows)
	diff, err := c.DiffPackageList(ctx, key, rows)
	if err != nil {
		t.Fatal(err)
	}
	if diff == nil || diff.Size() != 0 || diff.TooLarge {
		t.Fatalf("a list against itself differs: %+v", diff)
	}
	if list, gen := listIDOf(t, c, key), generationOf(t, c, key); diff.ListID != list.Int64 || diff.Generation != gen {
		t.Fatalf("the difference names list %d at generation %d, want %d at %d", diff.ListID, diff.Generation, list.Int64, gen)
	}
}

func TestDiffPackageList_ASuiteWithoutAListHasNoDifference(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/ubuntu/dists/jammy")
	if diff, err := c.DiffPackageList(ctx, key, numbered(3, 'a')); err != nil || diff != nil {
		t.Fatalf("for a suite without a row: %+v, %v; want nil, nil", diff, err)
	}
	seedRow(t, c, key, 1)
	if diff, err := c.DiffPackageList(ctx, key, numbered(3, 'a')); err != nil || diff != nil {
		t.Fatalf("for a suite that names no list: %+v, %v; want nil, nil", diff, err)
	}
}

// A difference over the limit is reported as too large, without its rows.
func TestDiffPackageList_GivesUpPastHalfTheNewList(t *testing.T) {
	c := openCache(t)
	key := sk("h", "/ubuntu/dists/jammy")
	seedRow(t, c, key, 1)
	publish(t, c, key, numbered(100, 'a'))
	diff, err := c.DiffPackageList(context.Background(), key, numbered(100, 'b'))
	if err != nil || diff == nil || !diff.TooLarge || diff.Size() != 0 || diff.ListID != listIDOf(t, c, key).Int64 {
		t.Fatalf("DiffPackageList = %+v, %v; want TooLarge, no rows, the list's id", diff, err)
	}
}

// Review focus 5: two new rows with one path. The difference refuses them
// by name; a commit given them fails and writes nothing.
func TestDiffPackageList_TwoRowsWithOnePathFail(t *testing.T) {
	c := openCache(t)
	key := sk("h", "/ubuntu/dists/jammy")
	seedRow(t, c, key, 1)
	publish(t, c, key, numbered(10, 'a'))
	rows := numbered(10, 'a')
	rows = append(rows, rows[4])
	_, err := c.DiffPackageList(context.Background(), key, rows)
	if !errors.Is(err, ErrDuplicatePackagePath) || !strings.Contains(err.Error(), rows[4].Path) {
		t.Fatalf("DiffPackageList = %v, want ErrDuplicatePackagePath naming %s", err, rows[4].Path)
	}
}

func TestCommitDiff_TwoRowsWithOnePathFail(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/ubuntu/dists/jammy")
	seedRow(t, c, key, 1)
	rows := numbered(10, 'a')
	prior, _ := publish(t, c, key, rows)
	list, gen := listIDOf(t, c, key), generationOf(t, c, key)
	dup := append(slices.Clone(rows), rows[4])
	dup[10].DeclaredSHA256 = hashOf('b')
	id, members := signedCandidate(t, c, key)
	if _, err := c.CommitAdoptionDiff(ctx, id, members, nil, dup, nil, true, nil); err == nil {
		t.Fatal("a commit of two rows with one path succeeded")
	}
	requireLive(t, c, key, rows)
	if listIDOf(t, c, key) != list || generationOf(t, c, key) != gen || len(unnamedLists(t, c)) != 0 {
		t.Fatal("a failed commit changed the suite's list or left a list behind")
	}
	suite, err := c.GetSuiteFreshness(ctx, "http", "h", key.SuitePath)
	if err != nil || suite.CurrentSnapshotID == nil || *suite.CurrentSnapshotID != prior {
		t.Fatalf("the prior snapshot is no longer current: %+v, %v", suite, err)
	}
}

// A malformed declared hash in a changed or an added row fails the commit
// as it does on the rewrite path.
func TestCommitDiff_AMalformedHashInTheDifferenceFails(t *testing.T) {
	for _, where := range []string{"changed", "added"} {
		t.Run(where, func(t *testing.T) {
			c := openCache(t)
			ctx := context.Background()
			key := sk("h", "/ubuntu/dists/jammy")
			seedRow(t, c, key, 1)
			rows := numbered(10, 'a')
			publish(t, c, key, rows)
			next := slices.Clone(rows)
			if where == "changed" {
				next[3].DeclaredSHA256 = "not a hash"
			} else {
				next = append(next, PackageHash{CanonicalScheme: "http", CanonicalHost: "h", Path: "/ubuntu/pool/new.deb", DeclaredSHA256: "not a hash"})
			}
			id, members := signedCandidate(t, c, key)
			diff, err := c.DiffPackageList(ctx, key, next)
			if err != nil || diff.Size() != 1 {
				t.Fatalf("DiffPackageList = %+v, %v", diff, err)
			}
			if _, err := c.CommitAdoptionDiff(ctx, id, members, nil, next, nil, true, diff); !errors.Is(err, ErrInvalidHash) {
				t.Fatalf("CommitAdoptionDiff = %v, want ErrInvalidHash", err)
			}
			requireLive(t, c, key, rows)
		})
	}
}

// The commit records hot-package interest for the rows it wrote — a
// changed row and a new one whose file a client already asked for — and
// leaves an unchanged row's record alone: it was recorded when the row
// arrived, and the triggers have kept it since.
func TestCommitDiff_RecordsInterestForChangedAndNewRows(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/ubuntu/dists/jammy")
	seedRow(t, c, key, 1)
	row := func(name string, letter byte) PackageHash {
		return PackageHash{CanonicalScheme: "http", CanonicalHost: "h", Path: "/ubuntu/pool/" + name + ".deb",
			DeclaredSHA256: hashOf(letter), PackageName: name, Architecture: "amd64", Version: "1"}
	}
	// Enough unchanged rows that the publication below is a difference.
	rows := append(numbered(20, '0'), row("unchanged", '1'), row("changed", '2'))
	publish(t, c, key, rows)

	request := func(name string, letter byte, at int64) {
		t.Helper()
		sha := hashOf(letter)
		if _, err := c.db.Exec(`INSERT OR IGNORE INTO blob (hash, size, created_at) VALUES (?, 1, 0)`, sha); err != nil {
			t.Fatal(err)
		}
		if err := c.PutURLPath(ctx, URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: "/ubuntu/pool/" + name + ".deb", BlobHash: &sha,
			UpstreamURL: "http://up/" + name, LastRequestedAt: &at, RequestCount: 1, LastFetchedAt: &at}); err != nil {
			t.Fatal(err)
		}
	}
	// Clients asked for: the unchanged file; the changed file's NEW bytes
	// (which the live list does not declare yet); the file about to be added.
	request("unchanged", '1', 5000)
	request("changed", '9', 6000)
	request("added", '3', 7000)
	if _, ok := hotStamp(t, c, key, "changed"); ok {
		t.Fatal("premise: the changed file's new bytes already count")
	}
	if _, ok := hotStamp(t, c, key, "added"); ok {
		t.Fatal("premise: the file not yet listed already counts")
	}
	// Mark the unchanged row's record: a commit that visited the row again
	// would raise it back to the request's stamp.
	if _, err := c.db.Exec(`UPDATE hot_package SET last_requested_at = 1 WHERE package_name = 'unchanged'`); err != nil {
		t.Fatal(err)
	}

	next := append(numbered(20, '0'), row("unchanged", '1'), row("changed", '9'), row("added", '3'))
	if _, w := publish(t, c, key, next); w.Mode != PackageListDifference || w.Changed != 1 || w.Added != 1 {
		t.Fatalf("the write = %+v, want a difference of one changed and one added row", w)
	}
	for name, want := range map[string]int64{"changed": 6000, "added": 7000, "unchanged": 1} {
		if got, ok := hotStamp(t, c, key, name); !ok || got != want {
			t.Errorf("hot_package of %s = %d, %v; want %d", name, got, ok, want)
		}
	}
}

// CommitAdoption, without a difference, writes the whole list as before.
func TestCommitAdoption_WithoutADifferenceStillRewrites(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/ubuntu/dists/jammy")
	seedRow(t, c, key, 1)
	rows := numbered(10, 'a')
	publish(t, c, key, rows)
	list := listIDOf(t, c, key)
	id, members := signedCandidate(t, c, key)
	if err := c.CommitAdoption(ctx, id, members, nil, rows, nil, true); err != nil {
		t.Fatal(err)
	}
	if got := listIDOf(t, c, key); !got.Valid || got == list {
		t.Fatalf("the suite's list = %v, want a new one (was %v)", got, list)
	}
	requireLive(t, c, key, rows)
	// The same through CommitAdoptionDiff, which says why.
	id, members = signedCandidate(t, c, key)
	w, err := c.CommitAdoptionDiff(ctx, id, members, nil, rows, nil, true, nil)
	if err != nil || w.Mode != PackageListRewrite || w.Reason != "no_difference" || w.Added != 10 {
		t.Fatalf("CommitAdoptionDiff without a difference = %+v, %v", w, err)
	}
}

// The other tripwires: a difference whose statements do not do what it
// says — an update that hits no row, an insert of a path the list has —
// is undone and the list written whole, even where the row count would
// have come out right.
func TestCommitDiff_ADifferenceThatDoesNotFitItsListIsRewritten(t *testing.T) {
	for _, defect := range []string{"a changed path the list does not have", "an added path the list already has"} {
		t.Run(defect, func(t *testing.T) {
			c := openCache(t)
			ctx := context.Background()
			key := sk("h", "/ubuntu/dists/jammy")
			seedRow(t, c, key, 1)
			rows := numbered(100, 'a')
			publish(t, c, key, rows)

			next := slices.Clone(rows)
			next[7].DeclaredSHA256 = hashOf('b')
			id, members := signedCandidate(t, c, key)
			diff, err := c.DiffPackageList(ctx, key, next)
			if err != nil || len(diff.Changed) != 1 {
				t.Fatalf("DiffPackageList = %+v, %v", diff, err)
			}
			if defect == "a changed path the list does not have" {
				// The count stays 100: only the statement's own result shows it.
				diff.Changed[0].Path = "/ubuntu/pool/ghost.deb"
			} else {
				diff.Added, diff.Changed = diff.Changed, nil
			}
			w, err := c.CommitAdoptionDiff(ctx, id, members, nil, next, nil, true, diff)
			if err != nil {
				t.Fatalf("a wrong difference failed the adoption: %v", err)
			}
			if w.Mode != PackageListRewrite || w.Reason != "mismatch" || w.Added != 100 || w.Detail == "" {
				t.Fatalf("the write = %+v, want a rewrite (mismatch) of 100 rows with a detail", w)
			}
			requireLive(t, c, key, next)
		})
	}
}

// A difference belongs to the rows it was computed for. Handed to a commit
// of other rows — another publication of the same size, or the same slice
// changed since — it is not applied: the commit writes the list whole and
// reports the defect, and the list is exactly the rows the commit was given.
func TestCommitDiff_ADifferenceForOtherRowsIsNotApplied(t *testing.T) {
	for _, how := range []string{"other rows of the same count", "the same slice, changed since"} {
		t.Run(how, func(t *testing.T) {
			c := openCache(t)
			ctx := context.Background()
			key := sk("h", "/ubuntu/dists/jammy")
			seedRow(t, c, key, 1)
			rows := numbered(100, 'a')
			publish(t, c, key, rows)

			forDiff := slices.Clone(rows)
			forDiff[3].DeclaredSHA256 = hashOf('b')
			id, members := signedCandidate(t, c, key)
			diff, err := c.DiffPackageList(ctx, key, forDiff)
			if err != nil || diff.Size() != 1 {
				t.Fatalf("DiffPackageList = %+v, %v", diff, err)
			}
			committed := forDiff
			if how == "other rows of the same count" {
				committed = slices.Clone(rows)
				committed[40].DeclaredSHA256, committed[41].Version = hashOf('c'), "7"
			} else {
				committed[60].DeclaredSHA256 = hashOf('d')
			}
			w, err := c.CommitAdoptionDiff(ctx, id, members, nil, committed, nil, true, diff)
			if err != nil {
				t.Fatal(err)
			}
			if w.Mode != PackageListRewrite || w.Reason != "mismatch" || w.Added != 100 || w.Detail == "" {
				t.Fatalf("the write = %+v, want a rewrite (mismatch) of 100 rows with a detail", w)
			}
			requireLive(t, c, key, committed)
		})
	}
}

// The digest that binds a difference to its rows covers every column a
// list stores: rows that differ in any one of them are other rows. Walked
// over PackageHash's fields, so a field added to the type fails here until
// it is digested or named below as not stored; and counted against the
// table, so a column added there without a field does too.
func TestDigestPackageRows_EveryStoredColumnCounts(t *testing.T) {
	notStored := map[string]string{
		"SnapshotID": "a list belongs to a suite, not to a snapshot: package_list_entry has no such column",
	}
	base := PackageHash{CanonicalScheme: "http", CanonicalHost: "h", Path: "/pool/p.deb", DeclaredSHA256: hashOf('a'),
		SnapshotID: 7, PackageName: "p", Architecture: "amd64", Version: "1", FlatFilename: "p.deb"}
	of := func(r PackageHash) packageRowsDigest { return digestPackageRows([]PackageHash{r}) }
	stored := 0
	typ := reflect.TypeOf(base)
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		changed := base
		switch v := reflect.ValueOf(&changed).Elem().Field(i); v.Kind() {
		case reflect.String:
			v.SetString(v.String() + "x")
		case reflect.Int64:
			v.SetInt(v.Int() + 1)
		default:
			t.Fatalf("PackageHash.%s is a %s: this test does not know how to change it", name, v.Kind())
		}
		if _, skip := notStored[name]; skip {
			continue
		}
		stored++
		if of(changed) == of(base) {
			t.Errorf("rows that differ only in %s have the same digest: a difference computed for one would be applied to a commit of the other", name)
		}
	}
	// Every column of the table but the list's id is one of those fields.
	c := openCache(t)
	var columns int
	if err := c.db.QueryRow(`SELECT count(*) FROM pragma_table_info('package_list_entry') WHERE name <> 'package_list_id'`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != stored {
		t.Errorf("package_list_entry stores %d columns beside its list's id, PackageHash has %d stored fields: the digest must cover each column", columns, stored)
	}
	// A field's end counts: the same letters split differently are other rows.
	a, b := base, base
	a.PackageName, a.Architecture = "ab", "c"
	b.PackageName, b.Architecture = "a", "bc"
	if of(a) == of(b) {
		t.Error(`("ab","c") and ("a","bc") have the same digest`)
	}
}

// The binding is to the rows, not to their order: the same rows in another
// order are the same publication, and its difference is applied.
func TestCommitDiff_TheSameRowsInAnotherOrderAreTheSameRows(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/ubuntu/dists/jammy")
	seedRow(t, c, key, 1)
	rows := numbered(100, 'a')
	publish(t, c, key, rows)
	next := slices.Clone(rows)
	next[3].DeclaredSHA256 = hashOf('b')
	id, members := signedCandidate(t, c, key)
	diff, err := c.DiffPackageList(ctx, key, next)
	if err != nil {
		t.Fatal(err)
	}
	reordered := slices.Clone(next)
	slices.Reverse(reordered)
	w, err := c.CommitAdoptionDiff(ctx, id, members, nil, reordered, nil, true, diff)
	if err != nil || w.Mode != PackageListDifference || w.Changed != 1 {
		t.Fatalf("the write = %+v, %v; want the difference applied", w, err)
	}
	requireLive(t, c, key, next)
}

// A statement of the difference that fails, as it runs, for a reason that
// is not the difference's — here a trigger that raises an arithmetic
// error; in the field a full disk or an I/O error — fails the commit, as
// it would on the rewrite
// path, and leaves everything as it was. Only a constraint failure (an
// added path the list already has) says the difference itself is wrong
// and is answered by writing the list whole (the final review's M-1): the
// adopter reports that as a defect, which a full disk is not.
func TestCommitDiff_AStatementErrorThatIsNotAConstraintFailsTheCommit(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/ubuntu/dists/jammy")
	seedRow(t, c, key, 1)
	rows := numbered(100, 'a')
	prior, _ := publish(t, c, key, rows)
	list, gen := listIDOf(t, c, key), generationOf(t, c, key)
	// The fault must come when the statement runs, not when it is prepared
	// (a prepare error already failed the commit): abs() of the smallest
	// integer is SQLite's documented run-time "integer overflow", an error
	// that is not a constraint failure.
	if _, err := c.db.Exec(`CREATE TRIGGER break_delete BEFORE DELETE ON package_list_entry
 BEGIN SELECT abs(-9223372036854775807 - 1 + OLD.rowid * 0); END`); err != nil {
		t.Fatal(err)
	}
	next := slices.Clone(rows[1:]) // one removed: the difference deletes a row
	id, members := signedCandidate(t, c, key)
	diff, err := c.DiffPackageList(ctx, key, next)
	if err != nil || len(diff.Removed) != 1 {
		t.Fatalf("DiffPackageList = %+v, %v", diff, err)
	}
	w, err := c.CommitAdoptionDiff(ctx, id, members, nil, next, nil, true, diff)
	if err == nil {
		t.Fatalf("a difference whose delete failed committed: %+v; want the commit to fail", w)
	}
	if !strings.Contains(err.Error(), "integer overflow") {
		t.Fatalf("the commit's error = %v, want the statement's own (integer overflow)", err)
	}
	requireLive(t, c, key, rows)
	if listIDOf(t, c, key) != list || generationOf(t, c, key) != gen || len(unnamedLists(t, c)) != 0 {
		t.Fatal("a failed commit changed the suite's list or its generation, or left a list behind")
	}
	suite, err := c.GetSuiteFreshness(ctx, "http", "h", key.SuitePath)
	if err != nil || suite.CurrentSnapshotID == nil || *suite.CurrentSnapshotID != prior {
		t.Fatalf("the prior snapshot is no longer current: %+v, %v", suite, err)
	}
	// With the fault gone the same difference applies.
	if _, err := c.db.Exec(`DROP TRIGGER break_delete`); err != nil {
		t.Fatal(err)
	}
	if w, err := c.CommitAdoptionDiff(ctx, id, members, nil, next, nil, true, diff); err != nil || w.Mode != PackageListDifference {
		t.Fatalf("the retry = %+v, %v; want the difference applied", w, err)
	}
	requireLive(t, c, key, next)
}

// A file a client asked for whose cached bytes were not the declared ones
// at the time (so the request recorded no interest), and that the adoption
// then warms to the declared bytes: the commit records the interest for
// the warmed path, on both commit paths, although its list entry did not
// change. Warming writes the url_path row without a request stamp, so no
// trigger fires for it, and no later commit visits an unchanged entry
// (the final review's M-2). The stamp recorded is the client's own:
// warming never invents interest.
func TestCommitDiff_RecordsInterestForAWarmedFileOfAnUnchangedEntry(t *testing.T) {
	for _, mode := range []string{PackageListDifference, PackageListRewrite} {
		t.Run(mode, func(t *testing.T) {
			c := openCache(t)
			ctx := context.Background()
			key := sk("h", "/ubuntu/dists/jammy")
			seedRow(t, c, key, 1)
			warm := PackageHash{CanonicalScheme: "http", CanonicalHost: "h", Path: "/ubuntu/pool/warm.deb",
				DeclaredSHA256: hashOf('1'), PackageName: "warm", Architecture: "amd64", Version: "1"}
			never := PackageHash{CanonicalScheme: "http", CanonicalHost: "h", Path: "/ubuntu/pool/never.deb",
				DeclaredSHA256: hashOf('2'), PackageName: "never", Architecture: "amd64", Version: "1"}
			rows := append(numbered(20, '0'), warm, never)
			publish(t, c, key, rows)

			// A client asked for warm.deb while the cache held other bytes for it.
			for _, h := range []string{hashOf('1'), hashOf('2'), hashOf('9')} {
				if _, err := c.db.Exec(`INSERT OR IGNORE INTO blob (hash, size, created_at) VALUES (?, 1, 0)`, h); err != nil {
					t.Fatal(err)
				}
			}
			stale, at := hashOf('9'), int64(5000)
			if err := c.PutURLPath(ctx, URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: warm.Path, BlobHash: &stale,
				UpstreamURL: "http://up/warm", LastRequestedAt: &at, RequestCount: 1, LastFetchedAt: &at}); err != nil {
				t.Fatal(err)
			}
			if _, ok := hotStamp(t, c, key, "warm"); ok {
				t.Fatal("premise: a request for bytes the list does not declare recorded interest")
			}

			// The next publication leaves both entries as they are and warms
			// both files; never.deb was never asked for.
			next := slices.Clone(rows)
			next[0].Version = "2"
			id, members := signedCandidate(t, c, key)
			var diff *PackageListDiff
			if mode == PackageListDifference {
				var err error
				if diff, err = c.DiffPackageList(ctx, key, next); err != nil {
					t.Fatal(err)
				}
			}
			warmed := []PrefetchedURLPath{
				{CanonicalScheme: "http", CanonicalHost: "h", Path: warm.Path, BlobHash: warm.DeclaredSHA256, UpstreamURL: "http://up/warm"},
				{CanonicalScheme: "http", CanonicalHost: "h", Path: never.Path, BlobHash: never.DeclaredSHA256, UpstreamURL: "http://up/never"},
			}
			w, err := c.CommitAdoptionDiff(ctx, id, members, nil, next, warmed, true, diff)
			if err != nil || w.Mode != mode {
				t.Fatalf("the write = %+v, %v; want mode %s", w, err, mode)
			}
			if got, ok := hotStamp(t, c, key, "warm"); !ok || got != at {
				t.Fatalf("interest in the warmed, once-requested file = %d, %v; want the client's stamp %d", got, ok, at)
			}
			if _, ok := hotStamp(t, c, key, "never"); ok {
				t.Fatal("warming a file nobody asked for recorded interest")
			}
			// The warm kept the client's stamp on the row and added none.
			up, err := c.LookupURL(ctx, "http", "h", never.Path)
			if err != nil || up.LastRequestedAt != nil || up.RequestCount != 0 {
				t.Fatalf("the warmed, never-requested row = %+v, %v; want no request recorded", up, err)
			}
		})
	}
}
