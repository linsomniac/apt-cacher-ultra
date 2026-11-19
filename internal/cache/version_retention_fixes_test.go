package cache

import (
	"context"
	"strings"
	"testing"
)

// TestKeepNewestNVersionSet_EquivalenceClasses verifies the newest-N cap
// counts Debian-version EQUIVALENCE CLASSES, not raw strings: dpkg-equal
// spellings (e.g. "1.0" == "1.0-0") share one slot and are both kept, so an
// equal pair can't consume two slots and evict a genuinely older version.
func TestKeepNewestNVersionSet_EquivalenceClasses(t *testing.T) {
	// {2.0} and {1.0, 1.0-0} are two classes; n=2 keeps all three raw spellings.
	got := keepNewestNVersionSet([]string{"2.0", "1.0", "1.0-0"}, 2)
	for _, v := range []string{"2.0", "1.0", "1.0-0"} {
		if _, ok := got[v]; !ok {
			t.Errorf("n=2: expected %q kept; got %v", v, got)
		}
	}

	// n=1 keeps only the newest class {2.0}.
	got1 := keepNewestNVersionSet([]string{"2.0", "1.0", "1.0-0"}, 1)
	if _, ok := got1["2.0"]; !ok {
		t.Error("n=1: 2.0 must be kept")
	}
	if _, ok := got1["1.0"]; ok {
		t.Error("n=1: 1.0 must be dropped")
	}
	if _, ok := got1["1.0-0"]; ok {
		t.Error("n=1: 1.0-0 must be dropped")
	}

	// Equal spellings of the newest version share one slot, so a real older
	// version still fits within n=2.
	got2 := keepNewestNVersionSet([]string{"1.0", "1.0-0", "0.9"}, 2)
	if _, ok := got2["0.9"]; !ok {
		t.Error("0.9 must survive: equal spellings of 1.0 must share one slot")
	}
}

// TestInsertPackageHashTx_HealsPreV6EmptyVersion verifies the reconcile
// writer heals a pre-v6 (un-backfilled) package_hash row whose version
// defaulted to ” instead of treating it as a conflict — otherwise the
// documented POST /reconcile recovery would fail on every v6-upgraded cache.
func TestInsertPackageHashTx_HealsPreV6EmptyVersion(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	debHash := strings.Repeat("a", 64)
	const path = "/pool/p/pkg/pkg_1.2.3_amd64.deb"
	// Pre-v6 style row: version defaulted to ''. The list needs no suite
	// here: insertPackageHashTx works on the list it is given.
	list, err := c.InsertUnnamedPackageListForTest(ctx, []PackageHash{{CanonicalScheme: "http", CanonicalHost: "ex.test",
		Path: path, DeclaredSHA256: debHash, PackageName: "pkg", Architecture: "amd64"}})
	if err != nil {
		t.Fatal(err)
	}

	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	wrote, err := insertPackageHashTx(ctx, tx, list, PackageHash{
		CanonicalScheme: "http", CanonicalHost: "ex.test", Path: path,
		DeclaredSHA256: debHash,
		PackageName:    "pkg", Architecture: "amd64", Version: "1.2.3",
	})
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("insertPackageHashTx healed-version path: unexpected error: %v", err)
	}
	if !wrote {
		t.Error("a heal reports that it wrote nothing")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var got string
	if err := c.db.QueryRow(`SELECT version FROM package_list_entry WHERE path=? AND package_list_id=?`, path, list).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "1.2.3" {
		t.Errorf("version after heal = %q, want 1.2.3", got)
	}
}

// TestInsertPackageHashTx_LoudOnRealVersionConflict verifies that when BOTH
// the existing and incoming versions are non-empty and differ (same path,
// same hash), reconcile still fails loud — a real cross-index disagreement.
func TestInsertPackageHashTx_LoudOnRealVersionConflict(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	debHash := strings.Repeat("b", 64)
	const path = "/pool/p/pkg/pkg_1.0_amd64.deb"
	list, err := c.InsertUnnamedPackageListForTest(ctx, []PackageHash{{CanonicalScheme: "http", CanonicalHost: "ex.test",
		Path: path, DeclaredSHA256: debHash, PackageName: "pkg", Architecture: "amd64", Version: "1.0"}})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := insertPackageHashTx(ctx, tx, list, PackageHash{
		CanonicalScheme: "http", CanonicalHost: "ex.test", Path: path,
		DeclaredSHA256: debHash,
		PackageName:    "pkg", Architecture: "amd64", Version: "2.0",
	}); err == nil {
		t.Error("expected a loud error on conflicting non-empty versions, got nil")
	}
}
