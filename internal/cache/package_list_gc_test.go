package cache

import (
	"context"
	"fmt"
	"slices"
	"testing"
)

func entryCount(t *testing.T, c *Cache, listID int64) int {
	t.Helper()
	var n int
	if err := c.db.QueryRow(`SELECT count(*) FROM package_list_entry WHERE package_list_id = ?`, listID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func listExists(t *testing.T, c *Cache, listID int64) bool {
	t.Helper()
	var n int
	if err := c.db.QueryRow(`SELECT count(*) FROM package_list WHERE package_list_id = ?`, listID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func manyRows(n int, letter byte) []PackageHash {
	var out []PackageHash
	for i := 0; i < n; i++ {
		out = append(out, pkgRow(fmt.Sprintf("/ubuntu/pool/p%04d.deb", i), hashOf(letter), ""))
	}
	return out
}

// Sub-job D deletes a list no suite names, a chunk of entries a
// transaction, then its header, and never touches a list a suite names.
func TestPackageListGC_DeletesAnUnnamedListInChunks(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/ubuntu/dists/jammy")
	seedRow(t, c, key, 1)
	adoptPackages(t, c, key, manyRows(7, 'a'), true)
	live := listIDOf(t, c, key).Int64
	dead, err := c.InsertUnnamedPackageListForTest(ctx, manyRows(25, 'b'))
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []struct {
		rows   int
		more   bool
		left   int
		header bool
	}{{10, true, 15, true}, {10, true, 5, true}, {5, true, 0, false}, {0, false, 0, false}} {
		rows, more, err := c.RunPackageListGCChunk(ctx, 10)
		if err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		if rows != want.rows || more != want.more {
			t.Fatalf("chunk %d = (%d, %v), want (%d, %v)", i, rows, more, want.rows, want.more)
		}
		if got := entryCount(t, c, dead); got != want.left {
			t.Fatalf("chunk %d: %d entries of the unnamed list left, want %d", i, got, want.left)
		}
		if got := listExists(t, c, dead); got != want.header {
			t.Fatalf("chunk %d: the unnamed list's header exists = %v, want %v", i, got, want.header)
		}
		if got := entryCount(t, c, live); got != 7 || !listExists(t, c, live) {
			t.Fatalf("chunk %d: the live list has %d entries, want its 7 untouched", i, got)
		}
	}
}

// A list whose entry count is an exact multiple of the chunk loses its
// header in the next call, which deletes no entry.
func TestPackageListGC_AnEmptyUnnamedListLosesItsHeader(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	dead, err := c.InsertUnnamedPackageListForTest(ctx, manyRows(10, 'b'))
	if err != nil {
		t.Fatal(err)
	}
	if rows, more, err := c.RunPackageListGCChunk(ctx, 10); err != nil || rows != 10 || !more {
		t.Fatalf("first chunk = (%d, %v, %v), want (10, true)", rows, more, err)
	}
	if !listExists(t, c, dead) {
		t.Fatal("the header went with a full chunk; it waits for the next one")
	}
	if rows, more, err := c.RunPackageListGCChunk(ctx, 10); err != nil || rows != 0 || !more {
		t.Fatalf("second chunk = (%d, %v, %v), want (0, true): it deleted the header", rows, more, err)
	}
	if listExists(t, c, dead) {
		t.Fatal("the emptied list's header is still there")
	}
	if rows, more, err := c.RunPackageListGCChunk(ctx, 10); err != nil || rows != 0 || more {
		t.Fatalf("third chunk = (%d, %v, %v), want (0, false)", rows, more, err)
	}
}

func TestPackageListGC_LeavesEveryNamedList(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	var lists []int64
	for _, suite := range []string{"/a/dists/s", "/b/dists/s", "/c/dists/s"} {
		key := sk("h", suite)
		seedRow(t, c, key, 1)
		adoptPackages(t, c, key, []PackageHash{pkgRow(suite+"/x.deb", hashOf('a'), "")}, true)
		lists = append(lists, listIDOf(t, c, key).Int64)
	}
	if rows, more, err := c.RunPackageListGCChunk(ctx, 0); err != nil || rows != 0 || more {
		t.Fatalf("with no unnamed list: (%d, %v, %v), want (0, false)", rows, more, err)
	}
	for _, id := range lists {
		if entryCount(t, c, id) != 1 {
			t.Fatalf("list %d lost its row", id)
		}
	}
}

// What leaves a list unnamed: a rewrite, a suite's removal, a quarantine
// of its current snapshot. GC takes each; a suite nothing happened to
// keeps its list.
func TestPackageListGC_TakesTheListsOfARemovedAndOfAQuarantinedSuite(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	adopt := func(suite string) (SuiteKey, int64, int64) {
		key := sk("h", suite)
		seedRow(t, c, key, 1)
		id := adoptPackages(t, c, key, []PackageHash{pkgRow(suite+"/x.deb", hashOf('a'), ""), pkgRow(suite+"/y.deb", hashOf('b'), "")}, true)
		return key, id, listIDOf(t, c, key).Int64
	}
	forgotten, forgottenSnap, forgottenList := adopt("/forgotten/dists/s")
	quarantined, quarantinedSnap, quarantinedList := adopt("/quarantined/dists/s")
	kept, _, keptList := adopt("/kept/dists/s")
	rewritten, _, rewrittenOld := adopt("/rewritten/dists/s")
	adoptPackages(t, c, rewritten, []PackageHash{pkgRow("/rewritten/dists/s/z.deb", hashOf('c'), "")}, true)
	rewrittenNew := listIDOf(t, c, rewritten).Int64

	if _, err := c.ForgetSuite(ctx, forgotten, &forgottenSnap); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ApplyLegacyAuditDecisions(ctx, []LegacyAuditDecision{{SnapshotID: quarantinedSnap, Outcome: LegacyAuditQuarantine}}); err != nil {
		t.Fatal(err)
	}
	if got := listIDOf(t, c, quarantined); got.Valid {
		t.Fatalf("the quarantined suite still names list %d", got.Int64)
	}
	want := []int64{forgottenList, quarantinedList, rewrittenOld}
	slices.Sort(want)
	if got := unnamedLists(t, c); !slices.Equal(got, want) {
		t.Fatalf("unnamed lists = %v, want %v (forgotten, quarantined, rewritten)", got, want)
	}
	if rows := runPackageListGC(t, c); rows != 6 {
		t.Fatalf("GC deleted %d rows, want 6", rows)
	}
	if got := unnamedLists(t, c); len(got) != 0 {
		t.Fatalf("unnamed lists after GC = %v", got)
	}
	for name, id := range map[string]int64{"kept": keptList, "rewritten (new)": rewrittenNew} {
		if !listExists(t, c, id) || entryCount(t, c, id) == 0 {
			t.Fatalf("the %s suite's list %d did not survive GC", name, id)
		}
	}
	if got := listIDOf(t, c, kept); !got.Valid || got.Int64 != keptList {
		t.Fatalf("the kept suite names %v, want %d", got, keptList)
	}
}

// The oldest unnamed list goes first.
func TestPackageListGC_TakesTheLowestIdFirst(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	first, err := c.InsertUnnamedPackageListForTest(ctx, manyRows(3, 'a'))
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.InsertUnnamedPackageListForTest(ctx, manyRows(3, 'b'))
	if err != nil {
		t.Fatal(err)
	}
	if rows, _, err := c.RunPackageListGCChunk(ctx, 2); err != nil || rows != 2 {
		t.Fatalf("chunk = %d, %v", rows, err)
	}
	if entryCount(t, c, first) != 1 || entryCount(t, c, second) != 3 {
		t.Fatalf("entries left: first %d, second %d; want 1 and 3", entryCount(t, c, first), entryCount(t, c, second))
	}
}
