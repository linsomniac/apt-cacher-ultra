package cache

import (
	"slices"
	"strings"
	"testing"
)

// The package-row test helpers see the rows of a suite's current snapshot,
// however they got there: the commit's, and the ones a test seeds.
func TestPackageRowHelpers_SeeTheCurrentSnapshotsRows(t *testing.T) {
	c := openCache(t)
	ctx := t.Context()
	key := SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/ubuntu/dists/jammy"}
	seedRow(t, c, key, 1)
	id := adoptPackages(t, c, key, []PackageHash{
		pkgRow("/ubuntu/pool/b.deb", strings.Repeat("b", 64), ""),
		pkgRow("/ubuntu/pool/a.deb", strings.Repeat("a", 64), ""),
	}, true)
	seeded := pkgRow("/ubuntu/pool/c.deb", strings.Repeat("c", 64), "c.deb")
	if err := c.InsertPackageRowForTest(ctx, id, seeded); err != nil {
		t.Fatal(err)
	}
	rows, err := c.PackageRowsOfSnapshotForTest(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, r := range rows {
		paths = append(paths, r.Path)
	}
	if want := []string{"/ubuntu/pool/a.deb", "/ubuntu/pool/b.deb", "/ubuntu/pool/c.deb"}; !slices.Equal(paths, want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	seeded.SnapshotID = id
	if got := rows[2]; got != seeded {
		t.Fatalf("the seeded row reads back as %+v, want %+v", got, seeded)
	}
	// A snapshot that holds no rows has none.
	if rows, err := c.PackageRowsOfSnapshotForTest(ctx, id+1000); err != nil || len(rows) != 0 {
		t.Fatalf("rows of an unknown snapshot = %v, %v; want none", rows, err)
	}
}
