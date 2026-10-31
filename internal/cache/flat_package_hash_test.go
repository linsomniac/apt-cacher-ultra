package cache

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// adoptPackages commits an inline snapshot of key (which needs a row) whose
// anchor is a blob naming the suite and its first row (so a second call
// with other rows adopts new content), with phs as its package_hash rows
// (their snapshot ids filled in here) and coverage complete as given, and
// returns its id.
func adoptPackages(t *testing.T, c *Cache, key SuiteKey, phs []PackageHash, complete bool) int64 {
	t.Helper()
	ctx := context.Background()
	label := "InRelease of " + key.CanonicalScheme + "://" + key.CanonicalHost + key.SuitePath
	if len(phs) > 0 {
		label += " for " + phs[0].Path
	}
	anchor := seedBlob(t, c, label)
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{CanonicalScheme: key.CanonicalScheme,
		CanonicalHost: key.CanonicalHost, SuitePath: key.SuitePath, InReleaseHash: &anchor})
	if err != nil {
		t.Fatal(err)
	}
	for i := range phs {
		phs[i].SnapshotID = id
	}
	members := []SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: anchor, DeclaredSHA256: anchor}}
	if err := c.CommitAdoption(ctx, id, members, nil, phs, nil, complete); err != nil {
		t.Fatal(err)
	}
	return id
}

// flatFilenameOf reads the flat_filename of path's package_hash row in
// snapshot id.
func flatFilenameOf(t *testing.T, c *Cache, path string, id int64) string {
	t.Helper()
	var f string
	err := c.db.QueryRow(`SELECT flat_filename FROM package_hash WHERE path = ? AND snapshot_id = ?`, path, id).Scan(&f)
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("no package_hash row for %s in snapshot %d", path, id)
	}
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// pkgRow is a package_hash row of host h: path, declared hash, flat name.
func pkgRow(path, sha, flat string) PackageHash {
	return PackageHash{CanonicalScheme: "http", CanonicalHost: "h", Path: path, DeclaredSHA256: sha,
		PackageName: "p", Architecture: "amd64", Version: "1", FlatFilename: flat}
}

// Spec C6: both package_hash writers — CommitAdoption's Step 3 and
// reconcile's insertPackageHashTx — persist a flat row's flat_filename,
// and a dists row keeps "".
func TestPackageHash_FlatFilenamePersists(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	flat, dists := sk("h", "/repo"), sk("h", "/r/dists/x")
	seedRow(t, c, flat, 1)
	seedRow(t, c, dists, 1)
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	flatID := adoptPackages(t, c, flat, []PackageHash{pkgRow("/repo/./x_1_amd64.deb", a, "x_1_amd64.deb")}, true)
	distsID := adoptPackages(t, c, dists, []PackageHash{pkgRow("/r/pool/y_1_amd64.deb", b, "")}, true)
	if got := flatFilenameOf(t, c, "/repo/./x_1_amd64.deb", flatID); got != "x_1_amd64.deb" {
		t.Fatalf("CommitAdoption's flat row: flat_filename %q, want x_1_amd64.deb", got)
	}
	if got := flatFilenameOf(t, c, "/r/pool/y_1_amd64.deb", distsID); got != "" {
		t.Fatalf("CommitAdoption's dists row: flat_filename %q, want \"\"", got)
	}
	healed := pkgRow("/repo/amd64/z_1_amd64.deb", b, "amd64/z_1_amd64.deb")
	healed.SnapshotID = flatID
	if err := c.InsertReconciledMembers(ctx, flatID, nil, []PackageHash{healed}); err != nil {
		t.Fatal(err)
	}
	if got := flatFilenameOf(t, c, "/repo/amd64/z_1_amd64.deb", flatID); got != "amd64/z_1_amd64.deb" {
		t.Fatalf("insertPackageHashTx's flat row: flat_filename %q, want amd64/z_1_amd64.deb", got)
	}
}
