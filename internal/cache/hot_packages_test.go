package cache

import (
	"context"
	"fmt"
	"testing"
)

func hotSnapshot(t *testing.T, c *Cache, version int) (int64, []PackageHash, string) {
	t.Helper()
	anchor := seedBlob(t, c, fmt.Sprintf("hot snapshot %d", version))
	deb := seedBlob(t, c, fmt.Sprintf("hot package %d", version))
	id, _, err := c.InsertTrackedCandidateForTest(t.Context(), SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/dists/stable", InReleaseHash: &anchor})
	if err != nil {
		t.Fatal(err)
	}
	rows := []PackageHash{{CanonicalScheme: "http", CanonicalHost: "h", SnapshotID: id,
		Path: fmt.Sprintf("/pool/pkg_%d_amd64.deb", version), DeclaredSHA256: deb,
		PackageName: "pkg", Architecture: "amd64", Version: fmt.Sprint(version)}}
	return id, rows, anchor
}

func commitHotSnapshot(t *testing.T, c *Cache, id int64, rows []PackageHash, anchor string, warm bool) {
	t.Helper()
	var warmed []PrefetchedURLPath
	if warm {
		warmed = []PrefetchedURLPath{{CanonicalScheme: "http", CanonicalHost: "h",
			Path: rows[0].Path, BlobHash: rows[0].DeclaredSHA256, UpstreamURL: "http://h" + rows[0].Path}}
	}
	if err := c.CommitAdoption(t.Context(), id, []SnapshotMember{{SnapshotID: id, Path: "InRelease",
		BlobHash: anchor, DeclaredSHA256: anchor}}, nil, rows, warmed, true); err != nil {
		t.Fatal(err)
	}
}

func TestHotPackageSurvivesVersionChangesGCAndRestart(t *testing.T) {
	const day = int64(86400)
	const now = int64(1800000000)
	defer stubNow(t, now)()
	dir := t.TempDir()
	c, err := Open(t.Context(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	id1, ph1, a1 := hotSnapshot(t, c, 1)
	commitHotSnapshot(t, c, id1, ph1, a1, false)
	requested := now - 200*day
	if err := c.PutURLPath(t.Context(), URLPath{CanonicalScheme: "http", CanonicalHost: "h",
		Path: ph1[0].Path, BlobHash: &ph1[0].DeclaredSHA256, UpstreamURL: "http://h" + ph1[0].Path,
		LastRequestedAt: &requested, RequestCount: 1}); err != nil {
		t.Fatal(err)
	}
	id2, ph2, a2 := hotSnapshot(t, c, 2)
	hot, err := c.ComputeHotSet(t.Context(), "http", "h", id1, id2, ph2, 300*day, now, 3)
	if err != nil || len(hot) != 1 {
		t.Fatalf("version 2 hot set: %v %v", hot, err)
	}
	commitHotSnapshot(t, c, id2, ph2, a2, true)
	// Reclaim the old URL and the displaced package list (the list the
	// first adoption wrote, unnamed since the second: sub-job D's).
	if err := c.EvictURLPath(t.Context(), "http", "h", ph1[0].Path); err != nil {
		t.Fatal(err)
	}
	if rows := runPackageListGC(t, c); rows != 1 {
		t.Fatalf("GC deleted %d package rows, want the first adoption's 1", rows)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = Open(t.Context(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	id3, ph3, a3 := hotSnapshot(t, c, 3)
	hot, err = c.ComputeHotSet(t.Context(), "http", "h", id2, id3, ph3, 300*day, now, 3)
	if err != nil || len(hot) != 1 || hot[0].Path != ph3[0].Path {
		t.Fatalf("version 3 hot set: %v %v", hot, err)
	}
	commitHotSnapshot(t, c, id3, ph3, a3, true)
	var stamp int64
	if err := c.db.QueryRow(`SELECT last_requested_at FROM hot_package`).Scan(&stamp); err != nil || stamp != requested {
		t.Fatalf("prefetch changed client timestamp: %d %v", stamp, err)
	}
	up, err := c.LookupURL(t.Context(), "http", "h", ph3[0].Path)
	if err != nil || up.LastRequestedAt != nil || up.RequestCount != 0 {
		t.Fatalf("invented request: %+v %v", up, err)
	}
	for _, tc := range []struct{ window, at int64 }{{0, now}, {24 * 3600, now}, {300 * day, requested + 300*day + 1}} {
		hot, err = c.ComputeHotSet(t.Context(), "http", "h", id3, id3, ph3, tc.window, tc.at, 3)
		if err != nil || len(hot) != 0 {
			t.Fatalf("expired/disabled hot set: %v %v", hot, err)
		}
	}
	// An actual request extends interest; prefetching the new path did not.
	if err := c.TouchURLPath(t.Context(), "http", "h", ph3[0].Path); err != nil {
		t.Fatal(err)
	}
	hot, err = c.ComputeHotSet(t.Context(), "http", "h", id3, id3, ph3, day, now, 3)
	if err != nil || len(hot) != 1 {
		t.Fatalf("renewed hot set: %v %v", hot, err)
	}
	// Forgetting the suite must also forget its interest, without dangling rows.
	if _, err := c.db.Exec(`DELETE FROM suite_freshness WHERE canonical_host='h'`); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM hot_package`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("interest after forget: %d %v", count, err)
	}
}

// The schema-10 migration backfills hot_package from the requests clients
// already made, on a database that is really at schema 9 (package rows per
// snapshot, no hot_package); the schema-11 migration that follows keeps
// the row and puts the triggers on the package lists, so a later request
// stamp still reaches it.
func TestSchemaV10BackfillsRealClientInterest(t *testing.T) {
	db, dir := openCacheAtSchema(t, 9)
	f := &v10{t: t, db: db}
	key := SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/dists/stable"}
	f.suite(key)
	id := f.snapshot(key, "inline", "current")
	const path = "/pool/pkg_1_amd64.deb"
	deb := hashOf('d')
	f.row(id, PackageHash{CanonicalScheme: "http", CanonicalHost: "h", Path: path, DeclaredSHA256: deb,
		PackageName: "pkg", Architecture: "amd64", Version: "1"})
	stamp := int64(1700000000)
	f.requested("h", path, deb, stamp)
	if err := migrate(context.Background(), db, discardSlog()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	c, err := Open(t.Context(), dir, discardSlog())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	var got int64
	if err := c.db.QueryRow(`SELECT last_requested_at FROM hot_package WHERE package_name='pkg'`).Scan(&got); err != nil || got != stamp {
		t.Fatalf("backfill: %d %v", got, err)
	}
	// Backward wall-clock observations must not shorten the original window.
	stamp--
	if err := c.PutURLPath(t.Context(), URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: path,
		BlobHash: &deb, UpstreamURL: "http://h" + path, LastRequestedAt: &stamp, RequestCount: 2}); err != nil {
		t.Fatal(err)
	}
	if err := c.db.QueryRow(`SELECT last_requested_at FROM hot_package`).Scan(&got); err != nil || got != stamp+1 {
		t.Fatalf("backward timestamp: %d %v", got, err)
	}
	// A later one, through the schema-11 triggers, moves it forward.
	stamp += 100
	if err := c.PutURLPath(t.Context(), URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: path,
		BlobHash: &deb, UpstreamURL: "http://h" + path, LastRequestedAt: &stamp, RequestCount: 3}); err != nil {
		t.Fatal(err)
	}
	if err := c.db.QueryRow(`SELECT last_requested_at FROM hot_package`).Scan(&got); err != nil || got != stamp {
		t.Fatalf("forward timestamp: %d %v, want %d", got, err, stamp)
	}
}

func TestHotPackageFirstAdoptionAndScope(t *testing.T) {
	c := openCache(t)
	id, ph, anchor := hotSnapshot(t, c, 1)
	stamp := nowUnix()
	// Client traffic predating the first index adoption still counts once its
	// identity is known. A prefetched-but-never-requested path must not count.
	if err := c.PutURLPath(t.Context(), URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: ph[0].Path,
		BlobHash: &ph[0].DeclaredSHA256, UpstreamURL: "http://h" + ph[0].Path, LastRequestedAt: &stamp, RequestCount: 1}); err != nil {
		t.Fatal(err)
	}
	cold := ph[0]
	cold.PackageName, cold.Path = "cold", "/pool/cold.deb"
	commitHotSnapshot(t, c, id, append(ph, cold), anchor, false)
	check := func(scheme, host string, prior int64, candidates []PackageHash, want int) {
		t.Helper()
		got, err := c.ComputeHotSet(t.Context(), scheme, host, prior, id, candidates, 86400, stamp, 3)
		if err != nil || len(got) != want {
			t.Fatalf("%s/%s hot set: %v %v", scheme, host, got, err)
		}
	}
	check("http", "h", id, append(ph, cold), 1)
	check("https", "h", id, nil, 0)
	check("http", "another-host", id, nil, 0)
	foreignArch := ph[0]
	foreignArch.Architecture = "arm64"
	check("http", "h", id, []PackageHash{foreignArch}, 0)
	otherAnchor := seedBlob(t, c, "another suite")
	other, _, err := c.InsertTrackedCandidateForTest(t.Context(), SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/dists/other", InReleaseHash: &otherAnchor})
	if err != nil {
		t.Fatal(err)
	}
	check("http", "h", other, ph, 0)
}
