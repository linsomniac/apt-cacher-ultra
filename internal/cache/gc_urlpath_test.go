package cache

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// AIDEV-NOTE: tests for RunURLPathGCBatch — the version-aware retention
// url_path pass. The batch is cursor-paged and applies the three-rule
// union (recency OR newest-N mirror OR hold-grace) plus the unchanged
// metadata-anchor / snapshot_member guards. Most tests pass holdSeconds=0
// (no grace) so a non-retained row is deleted in the same scan, which maps
// cleanly onto the old "reaped count" assertions; the dropped_at lifecycle
// has its own dedicated tests.

const testMaxVersions = 3

// putURLPathRow inserts a url_path row with a chosen last_requested_at,
// bypassing the cache helpers so tests can plant aged or NULL-stamped rows
// directly.
func putURLPathRow(t *testing.T, c *Cache, scheme, host, path, blobHash string, lastRequestedAt sql.NullInt64, isMetadata bool) {
	t.Helper()
	meta := 0
	if isMetadata {
		meta = 1
	}
	_, err := c.db.Exec(
		`INSERT INTO url_path
		   (canonical_scheme, canonical_host, path, blob_hash,
		    upstream_url, is_metadata, last_requested_at, request_count)
		   VALUES (?, ?, ?, ?, ?, ?, ?, 1)`,
		scheme, host, path, blobHash, scheme+"://"+host+path, meta, lastRequestedAt,
	)
	if err != nil {
		t.Fatalf("putURLPathRow(%s): %v", path, err)
	}
}

// urlPathDroppedAt reads back the dropped_at column (-1 == NULL).
func urlPathDroppedAt(t *testing.T, c *Cache, path string) int64 {
	t.Helper()
	var v sql.NullInt64
	if err := c.db.QueryRow(`SELECT dropped_at FROM url_path WHERE path=?`, path).Scan(&v); err != nil {
		t.Fatalf("read dropped_at(%s): %v", path, err)
	}
	if !v.Valid {
		return -1
	}
	return v.Int64
}

func urlPathExists(t *testing.T, c *Cache, path string) bool {
	t.Helper()
	var n int
	if err := c.db.QueryRow(`SELECT count(*) FROM url_path WHERE path=?`, path).Scan(&n); err != nil {
		t.Fatalf("count url_path(%s): %v", path, err)
	}
	return n > 0
}

// makeBlobPositiveRefcount sets blob.refcount to n and clears
// refcount_zeroed_at, matching CommitAdoption Step 4's positive crossing.
func makeBlobPositiveRefcount(t *testing.T, c *Cache, hash string, n int64) {
	t.Helper()
	if _, err := c.db.Exec(
		`UPDATE blob SET refcount = ?, refcount_zeroed_at = NULL WHERE hash = ?`,
		n, hash,
	); err != nil {
		t.Fatalf("makeBlobPositiveRefcount: %v", err)
	}
}

// drainURLPathGC runs cursor-paged batches until the table is exhausted
// and returns the aggregate counts.
func drainURLPathGC(t *testing.T, c *Cache, batchSize int, ttl, hold int64, maxV int) URLPathGCBatchResult {
	t.Helper()
	var agg URLPathGCBatchResult
	var s, h, p string
	// Share one memo across the pass to exercise the cross-batch newest-N
	// memo path. (The production driver shares a memo only when hold > 0;
	// these tests use no mid-pass mutation, so a shared memo is correct at
	// any hold and gives the batches deterministic ranking coverage.)
	memo := NewURLPathGCMemo()
	for {
		res, err := c.RunURLPathGCBatch(context.Background(), batchSize, ttl, hold, maxV, s, h, p, memo)
		if err != nil {
			t.Fatalf("RunURLPathGCBatch: %v", err)
		}
		agg.Scanned += res.Scanned
		agg.Stamped += res.Stamped
		agg.Cleared += res.Cleared
		agg.Deleted += res.Deleted
		if res.Scanned == 0 {
			return agg
		}
		s, h, p = res.LastScheme, res.LastHost, res.LastPath
	}
}

// seedCurrentSnapshot inserts a suite_freshness + suite_snapshot row and
// points current_snapshot_id at it. Returns the snapshot id.
func seedCurrentSnapshot(t *testing.T, c *Cache, scheme, host, suite, inrelease string, now int64) int64 {
	t.Helper()
	if _, err := c.db.Exec(`INSERT INTO suite_freshness
	  (canonical_scheme, canonical_host, suite_path)
	  VALUES (?, ?, ?)`, scheme, host, suite); err != nil {
		t.Fatalf("seed suite_freshness: %v", err)
	}
	res, err := c.db.Exec(`INSERT INTO suite_snapshot
	  (canonical_scheme, canonical_host, suite_path,
	   inrelease_hash, created_at, adopted_at, heartbeat_at, package_coverage_complete)
	  VALUES (?, ?, ?, ?, ?, ?, ?, 1)`, scheme, host, suite, inrelease, now, now, now)
	if err != nil {
		t.Fatalf("seed suite_snapshot: %v", err)
	}
	id, _ := res.LastInsertId()
	if _, err := c.db.Exec(`UPDATE suite_freshness SET current_snapshot_id = ?
	  WHERE canonical_scheme=? AND canonical_host=? AND suite_path=?`, id, scheme, host, suite); err != nil {
		t.Fatalf("set current_snapshot_id: %v", err)
	}
	return id
}

func seedPackageHash(t *testing.T, c *Cache, scheme, host, path, declared string, snapID int64, name, arch, version string) {
	t.Helper()
	if _, err := c.db.Exec(`INSERT INTO package_hash
	  (canonical_scheme, canonical_host, path, declared_sha256, snapshot_id, package_name, architecture, version)
	  VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, scheme, host, path, declared, snapID, name, arch, version); err != nil {
		t.Fatalf("seed package_hash(%s): %v", path, err)
	}
}

func TestRunURLPathGCBatch_DisabledWhenTTLZero(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	if _, err := c.RunURLPathGCBatch(ctx, 100, 0, 0, testMaxVersions, "", "", "", nil); err == nil {
		t.Fatal("RunURLPathGCBatch(ttlSeconds=0): want error, got nil")
	}
}

func TestRunURLPathGCBatch_ReapsAgedRow(t *testing.T) {
	c := openCache(t)
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()

	h := seedBlob(t, c, "aged blob")
	putURLPathRow(t, c, "http", "ex.test", "/p/aged.deb", h,
		sql.NullInt64{Int64: now - 10*86400, Valid: true}, false)

	agg := drainURLPathGC(t, c, 100, 7*86400, 0, testMaxVersions)
	if agg.Deleted != 1 {
		t.Errorf("deleted = %d, want 1", agg.Deleted)
	}
	if urlPathExists(t, c, "/p/aged.deb") {
		t.Error("url_path row survived")
	}
	if rc := blobRefcount(t, c, h); rc != -1 {
		t.Errorf("blob.refcount = %d, want -1 after decrement", rc)
	}
	if z := blobZeroedAt(t, c, h); z != now {
		t.Errorf("refcount_zeroed_at = %d, want %d (now)", z, now)
	}
}

func TestRunURLPathGCBatch_FreshRowProtected(t *testing.T) {
	c := openCache(t)
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()

	h := seedBlob(t, c, "fresh blob")
	putURLPathRow(t, c, "http", "ex.test", "/p/fresh.deb", h,
		sql.NullInt64{Int64: now - 86400, Valid: true}, false)

	agg := drainURLPathGC(t, c, 100, 7*86400, 0, testMaxVersions)
	if agg.Deleted != 0 {
		t.Errorf("deleted = %d, want 0 (row inside TTL)", agg.Deleted)
	}
}

// TestRunURLPathGCBatch_NullLastRequestedNoLongerProtected is the core
// leak-fix regression: a prefetched-but-never-served row (last_requested_at
// IS NULL) that is not vouched by any snapshot is now reapable, where it
// used to be immortal.
func TestRunURLPathGCBatch_NullLastRequestedNoLongerProtected(t *testing.T) {
	c := openCache(t)
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()

	h := seedBlob(t, c, "prewarm blob")
	putURLPathRow(t, c, "http", "ex.test", "/p/prewarm.deb", h, sql.NullInt64{}, false)

	agg := drainURLPathGC(t, c, 100, 7*86400, 0, testMaxVersions)
	if agg.Deleted != 1 {
		t.Errorf("deleted = %d, want 1 (never-served prefetched row must be reapable)", agg.Deleted)
	}
}

// TestRunURLPathGCBatch_MirrorKeepsNewestNReapsOlder is the heart of the
// fix: in a fat-index suite that lists many versions of one package, the
// newest N are retained (even prefetched/never-served) and older versions
// are reaped.
func TestRunURLPathGCBatch_MirrorKeepsNewestNReapsOlder(t *testing.T) {
	c := openCache(t)
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()

	scheme, host, suite := "http", "download.docker.test", "/linux/ubuntu/dists/jammy"
	ir := seedBlob(t, c, "docker inrelease")
	snapID := seedCurrentSnapshot(t, c, scheme, host, suite, ir, now)

	// Five versions of docker-ce, all listed in the current snapshot, all
	// prefetched (NULL last_requested). Newest 2 (3.0, 2.0) must survive.
	versions := []string{"1.0", "2.0", "1.10", "1.2", "3.0"}
	pathFor := map[string]string{}
	for _, v := range versions {
		p := "/pool/d/docker-ce/docker-ce_" + v + "_amd64.deb"
		pathFor[v] = p
		blob := seedBlob(t, c, "docker-ce "+v)
		putURLPathRow(t, c, scheme, host, p, blob, sql.NullInt64{}, false)
		seedPackageHash(t, c, scheme, host, p, blob, snapID, "docker-ce", "amd64", v)
	}

	agg := drainURLPathGC(t, c, 100, 7*86400, 0, 2)
	if agg.Deleted != 3 {
		t.Fatalf("deleted = %d, want 3 (older versions 1.10,1.2,1.0)", agg.Deleted)
	}
	for _, v := range []string{"3.0", "2.0"} {
		if !urlPathExists(t, c, pathFor[v]) {
			t.Errorf("newest version %s was reaped but must be kept", v)
		}
	}
	for _, v := range []string{"1.10", "1.2", "1.0"} {
		if urlPathExists(t, c, pathFor[v]) {
			t.Errorf("old version %s survived but must be reaped", v)
		}
	}
}

// TestRunURLPathGCBatch_MirrorCapAcrossBatchesWithMemo: a fat-index package
// whose cached versions span SEVERAL small batches must still be capped to
// newest-N correctly when one per-pass URLPathGCMemo is shared across the
// batches (the production path). Locks in that the cross-batch ranking memo
// doesn't corrupt or stale the newest-N decision. drainURLPathGC shares one
// memo across its batch loop, so a small batchSize exercises the span.
func TestRunURLPathGCBatch_MirrorCapAcrossBatchesWithMemo(t *testing.T) {
	c := openCache(t)
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()

	scheme, host, suite := "http", "download.docker.test", "/linux/ubuntu/dists/jammy"
	ir := seedBlob(t, c, "docker inrelease")
	snapID := seedCurrentSnapshot(t, c, scheme, host, suite, ir, now)

	// Six versions of one package, all current-snapshot-vouched, never
	// requested. With batchSize=2 they span three batches; newest 2 survive.
	versions := []string{"1.0", "2.0", "1.10", "1.2", "3.0", "2.5"}
	pathFor := map[string]string{}
	for _, v := range versions {
		p := "/pool/d/docker-ce/docker-ce_" + v + "_amd64.deb"
		pathFor[v] = p
		blob := seedBlob(t, c, "docker-ce "+v)
		putURLPathRow(t, c, scheme, host, p, blob, sql.NullInt64{}, false)
		seedPackageHash(t, c, scheme, host, p, blob, snapID, "docker-ce", "amd64", v)
	}

	agg := drainURLPathGC(t, c, 2, 7*86400, 0, 2) // batchSize 2, cap newest 2
	if agg.Deleted != 4 {
		t.Fatalf("deleted = %d, want 4 (older 1.0,1.2,1.10,2.0 reaped across batches)", agg.Deleted)
	}
	for _, v := range []string{"3.0", "2.5"} {
		if !urlPathExists(t, c, pathFor[v]) {
			t.Errorf("newest version %s was reaped but must be kept", v)
		}
	}
	for _, v := range []string{"1.0", "1.2", "1.10", "2.0"} {
		if urlPathExists(t, c, pathFor[v]) {
			t.Errorf("old version %s survived but must be reaped", v)
		}
	}
}

// TestRunURLPathGCBatch_MirrorOnCurrentSnapshotProtects: a single-version
// package listed in the current snapshot is top-N and kept (path+hash).
func TestRunURLPathGCBatch_MirrorOnCurrentSnapshotProtects(t *testing.T) {
	c := openCache(t)
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()

	scheme, host, suite := "http", "archive.test", "/ubuntu/dists/noble"
	h := seedBlob(t, c, "vouched aged blob")
	ir := seedBlob(t, c, "current inrelease")
	const path = "/ubuntu/pool/main/x/x.deb"
	putURLPathRow(t, c, scheme, host, path, h,
		sql.NullInt64{Int64: now - 30*86400, Valid: true}, false)
	snapID := seedCurrentSnapshot(t, c, scheme, host, suite, ir, now)
	seedPackageHash(t, c, scheme, host, path, h, snapID, "x", "amd64", "1.0")

	agg := drainURLPathGC(t, c, 100, 7*86400, 0, testMaxVersions)
	if agg.Deleted != 0 {
		t.Errorf("deleted = %d, want 0 (newest version vouched by current snapshot)", agg.Deleted)
	}
}

// TestRunURLPathGCBatch_DisplacedSnapshotDoesNotProtect: the mirror rule is
// scoped to CURRENT snapshots (the live published index), so a .deb present
// only in a displaced/forensic snapshot is NOT vouched and is reapable —
// matching the pre-version guard (a). Just-superseded versions get the
// hold-grace window, not indefinite displaced-snapshot retention.
func TestRunURLPathGCBatch_DisplacedSnapshotDoesNotProtect(t *testing.T) {
	c := openCache(t)
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()

	scheme, host, suite := "http", "archive.test", "/ubuntu/dists/noble"
	h := seedBlob(t, c, "displaced-vouched aged blob")
	ir := seedBlob(t, c, "displaced inrelease")
	const path = "/ubuntu/pool/main/y/y.deb"
	putURLPathRow(t, c, scheme, host, path, h,
		sql.NullInt64{Int64: now - 30*86400, Valid: true}, false)

	// Adopted but NOT current (displaced); current_snapshot_id stays NULL.
	if _, err := c.db.Exec(`INSERT INTO suite_freshness
	  (canonical_scheme, canonical_host, suite_path) VALUES (?, ?, ?)`, scheme, host, suite); err != nil {
		t.Fatalf("seed suite_freshness: %v", err)
	}
	res, err := c.db.Exec(`INSERT INTO suite_snapshot
	  (canonical_scheme, canonical_host, suite_path, inrelease_hash,
	   created_at, adopted_at, heartbeat_at, package_coverage_complete)
	  VALUES (?, ?, ?, ?, ?, ?, ?, 1)`, scheme, host, suite, ir, now, now, now)
	if err != nil {
		t.Fatalf("seed suite_snapshot: %v", err)
	}
	snapID, _ := res.LastInsertId()
	seedPackageHash(t, c, scheme, host, path, h, snapID, "y", "amd64", "1.0")

	agg := drainURLPathGC(t, c, 100, 7*86400, 0, testMaxVersions)
	if agg.Deleted != 1 {
		t.Errorf("deleted = %d, want 1 (displaced-only snapshot does not vouch a .deb)", agg.Deleted)
	}
}

func TestRunURLPathGCBatch_BatchSizeAndCursor(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()

	for i := 0; i < 7; i++ {
		h := seedBlob(t, c, "batch blob "+string(rune('a'+i)))
		putURLPathRow(t, c, "http", "ex.test",
			"/p/"+strings.Repeat("a", i+1)+".deb", h,
			sql.NullInt64{Int64: now - 30*86400, Valid: true}, false)
	}

	var s, h, p string
	scans := []int{}
	deletes := 0
	memo := NewURLPathGCMemo()
	for {
		res, err := c.RunURLPathGCBatch(ctx, 3, 7*86400, 0, testMaxVersions, s, h, p, memo)
		if err != nil {
			t.Fatalf("RunURLPathGCBatch: %v", err)
		}
		if res.Scanned == 0 {
			break
		}
		scans = append(scans, res.Scanned)
		deletes += res.Deleted
		s, h, p = res.LastScheme, res.LastHost, res.LastPath
	}
	want := []int{3, 3, 1}
	if len(scans) != len(want) {
		t.Fatalf("batch scan counts = %v, want %v", scans, want)
	}
	for i := range want {
		if scans[i] != want[i] {
			t.Errorf("batch %d scanned = %d, want %d", i, scans[i], want[i])
		}
	}
	if deletes != 7 {
		t.Errorf("total deleted = %d, want 7", deletes)
	}
}

func TestRunURLPathGCBatch_MirrorMismatchedHashDoesNotProtect(t *testing.T) {
	c := openCache(t)
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()

	scheme, host, suite := "http", "archive.test", "/ubuntu/dists/noble"
	stale := seedBlob(t, c, "stale cached bytes")
	current := seedBlob(t, c, "current declared bytes")
	ir := seedBlob(t, c, "inrelease for the protecting snapshot")
	const path = "/ubuntu/pool/main/z/z.deb"
	putURLPathRow(t, c, scheme, host, path, stale,
		sql.NullInt64{Int64: now - 30*86400, Valid: true}, false)
	snapID := seedCurrentSnapshot(t, c, scheme, host, suite, ir, now)
	// Current snapshot declares a DIFFERENT hash for this path.
	seedPackageHash(t, c, scheme, host, path, current, snapID, "z", "amd64", "1.0")

	agg := drainURLPathGC(t, c, 100, 7*86400, 0, testMaxVersions)
	if agg.Deleted != 1 {
		t.Errorf("deleted = %d, want 1 (cached blob_hash diverged from declared_sha256)", agg.Deleted)
	}
}

// TestRunURLPathGCBatch_EmptyVersionFallbackProtects: a Sources/pdiff-style
// package_hash row (version=”) keeps the legacy snapshot-reference guard.
func TestRunURLPathGCBatch_EmptyVersionFallbackProtects(t *testing.T) {
	c := openCache(t)
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()

	scheme, host, suite := "http", "archive.test", "/ubuntu/dists/noble"
	h := seedBlob(t, c, "source artifact bytes")
	ir := seedBlob(t, c, "current inrelease")
	const path = "/ubuntu/pool/main/s/src_1.0.dsc"
	putURLPathRow(t, c, scheme, host, path, h,
		sql.NullInt64{Int64: now - 30*86400, Valid: true}, false)
	snapID := seedCurrentSnapshot(t, c, scheme, host, suite, ir, now)
	seedPackageHash(t, c, scheme, host, path, h, snapID, "src", "source", "") // empty version

	agg := drainURLPathGC(t, c, 100, 7*86400, 0, testMaxVersions)
	if agg.Deleted != 0 {
		t.Errorf("deleted = %d, want 0 (empty-version row keeps snapshot-reference guard)", agg.Deleted)
	}
}

// TestRunURLPathGCBatch_EmptyVersionBinaryKept: per design §6 and the
// "Empty-version rows" rule, a version-less row of ANY kind — source, pdiff,
// Contents, OR a pre-v6 / malformed binary .deb (arch=amd64, version='') —
// that a CURRENT snapshot vouches is KEPT via the snapshot-reference guard. It
// has no Debian version to rank, so the newest-N cap cannot apply. The
// pre-migration 25 GB is reclaimed operationally (wipe, or re-adoption that
// backfills version), NOT by GC reaping version-less binaries; reaping them
// here would mass-evict still-published binaries during the migration window
// (round-4 finding). The gate is the package_hash.version, not the path suffix.
func TestRunURLPathGCBatch_EmptyVersionBinaryKept(t *testing.T) {
	c := openCache(t)
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()

	scheme, host, suite := "http", "archive.test", "/ubuntu/dists/noble"
	h := seedBlob(t, c, "versionless binary deb bytes")
	ir := seedBlob(t, c, "current inrelease")
	const path = "/ubuntu/pool/main/b/bad/bad_amd64.deb"
	putURLPathRow(t, c, scheme, host, path, h,
		sql.NullInt64{Int64: now - 30*86400, Valid: true}, false)
	snapID := seedCurrentSnapshot(t, c, scheme, host, suite, ir, now)
	seedPackageHash(t, c, scheme, host, path, h, snapID, "bad", "amd64", "") // binary, empty version

	agg := drainURLPathGC(t, c, 100, 7*86400, 0, testMaxVersions)
	if agg.Deleted != 0 {
		t.Errorf("deleted = %d, want 0 (version-less binary kept via snapshot-reference guard; reclaimed operationally)", agg.Deleted)
	}
}

// TestRunURLPathGCBatch_DdebVersionCapped: a .ddeb debug-symbol package is a
// versioned binary and MUST be subject to the newest-N version cap, exactly
// like .deb/.udeb. dbgsym repos (ddebs.ubuntu.com) are the most extreme
// fat-index class, so the cap MUST key on package_hash.version, not the path
// suffix — a path-suffix gate that excluded .ddeb would immortalize every
// version and reopen the disk leak (round-4 finding).
func TestRunURLPathGCBatch_DdebVersionCapped(t *testing.T) {
	c := openCache(t)
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()

	scheme, host, suite := "http", "ddebs.ubuntu.test", "/ubuntu/dists/noble"
	ir := seedBlob(t, c, "ddeb inrelease")
	snapID := seedCurrentSnapshot(t, c, scheme, host, suite, ir, now)

	versions := []string{"1.0", "2.0", "3.0"}
	pathFor := map[string]string{}
	for _, v := range versions {
		p := "/ubuntu/pool/main/f/foo/foo-dbgsym_" + v + "_amd64.ddeb"
		pathFor[v] = p
		blob := seedBlob(t, c, "foo-dbgsym "+v)
		putURLPathRow(t, c, scheme, host, p, blob, sql.NullInt64{}, false)
		seedPackageHash(t, c, scheme, host, p, blob, snapID, "foo-dbgsym", "amd64", v)
	}

	agg := drainURLPathGC(t, c, 100, 7*86400, 0, 1) // cap = newest 1
	if agg.Deleted != 2 {
		t.Fatalf("deleted = %d, want 2 (.ddeb subject to version cap; older 1.0,2.0 reaped)", agg.Deleted)
	}
	if !urlPathExists(t, c, pathFor["3.0"]) {
		t.Error("newest .ddeb 3.0 was reaped but must be kept")
	}
	for _, v := range []string{"1.0", "2.0"} {
		if urlPathExists(t, c, pathFor[v]) {
			t.Errorf("old .ddeb %s survived but must be reaped (cap bypassed)", v)
		}
	}
}

// TestRunURLPathGCBatch_BinaryPdiffPatchKept: a binary-arch pdiff patch
// file (arch="amd64", version='') referenced by the current snapshot is kept
// via the empty-version snapshot-reference fallback — same as a source-arch
// one — because the fallback is gated on the empty version, not the path or
// arch. A versioned binary in the same path tree would instead hit the cap.
func TestRunURLPathGCBatch_BinaryPdiffPatchKept(t *testing.T) {
	c := openCache(t)
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()

	scheme, host, suite := "http", "archive.test", "/ubuntu/dists/noble"
	h := seedBlob(t, c, "pdiff patch bytes")
	ir := seedBlob(t, c, "current inrelease")
	const path = "/ubuntu/dists/noble/main/binary-amd64/Packages.diff/2026-01-01.gz"
	putURLPathRow(t, c, scheme, host, path, h,
		sql.NullInt64{Int64: now - 30*86400, Valid: true}, false)
	snapID := seedCurrentSnapshot(t, c, scheme, host, suite, ir, now)
	// pdiff patch row: binary arch, empty version (buildPdiffHashes shape).
	seedPackageHash(t, c, scheme, host, path, h, snapID, "", "amd64", "")

	agg := drainURLPathGC(t, c, 100, 7*86400, 0, testMaxVersions)
	if agg.Deleted != 0 {
		t.Errorf("deleted = %d, want 0 (binary-arch pdiff patch kept via empty-version fallback)", agg.Deleted)
	}
}

// TestTouchURLPath_ClearsDroppedAt: a client request during the hold grace
// clears the drop stamp at the source, so the row can't be reaped at the
// original stamp's expiry even when hold_packages.window > gc.url_path_ttl.
func TestTouchURLPath_ClearsDroppedAt(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()

	h := seedBlob(t, c, "touch-clears blob")
	putURLPathRow(t, c, "http", "ex.test", "/p/touch.deb", h,
		sql.NullInt64{Int64: now - 30*86400, Valid: true}, false)
	// Stamp it via a hold-grace GC pass.
	_ = drainURLPathGC(t, c, 100, 7*86400, 86400, testMaxVersions)
	if urlPathDroppedAt(t, c, "/p/touch.deb") != now {
		t.Fatalf("expected dropped_at stamped to now")
	}
	if err := c.TouchURLPath(ctx, "http", "ex.test", "/p/touch.deb"); err != nil {
		t.Fatal(err)
	}
	if urlPathDroppedAt(t, c, "/p/touch.deb") != -1 {
		t.Error("TouchURLPath must clear dropped_at to NULL on a client request")
	}
}

// TestCommitAdoption_PrefetchUpsertClearsDroppedAt: re-warming a hot .deb via
// adoption (Step 3a url_path upsert) must clear any hold-grace drop stamp, the
// same invariant PutURLPath/TouchURLPath enforce. Otherwise a re-warmed blob
// keeps its stale pre-re-warm deadline; because the in-grace fast-path skips
// re-evaluating still-stamped rows, the row can be reaped at the OLD deadline
// before the operator-configured hold window elapses (round-4 finding).
func TestCommitAdoption_PrefetchUpsertClearsDroppedAt(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, 1_700_000_000)()

	const host = "archive.test"
	const suite = "/ubuntu/dists/noble"
	const path = "/ubuntu/pool/main/d/docker-ce/docker-ce_1.0_amd64.deb"

	stale := seedBlob(t, c, "stale prewarm blob")
	rewarmed := seedBlob(t, c, "rewarmed blob")
	ir := seedBlob(t, c, "adopted inrelease")

	// Pre-existing .deb row carrying a hold-grace drop deadline.
	putURLPathRow(t, c, "http", host, path, stale, sql.NullInt64{}, false)
	if _, err := c.db.Exec(`UPDATE url_path SET dropped_at = ? WHERE path = ?`,
		int64(1_699_000_000), path); err != nil {
		t.Fatalf("seed dropped_at: %v", err)
	}

	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: host, SuitePath: suite,
		InReleaseHash: &ir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id, nil, nil, nil,
		[]PrefetchedURLPath{{
			CanonicalScheme: "http", CanonicalHost: host, Path: path,
			BlobHash: rewarmed, UpstreamURL: "http://" + host + path,
		}}, false); err != nil {
		t.Fatalf("CommitAdoption: %v", err)
	}

	if got := urlPathDroppedAt(t, c, path); got != -1 {
		t.Errorf("dropped_at = %d, want -1 (NULL) after adoption re-warm", got)
	}
}

func TestRunURLPathGCBatch_InReleaseUrlPathOnCurrentSnapshotProtected(t *testing.T) {
	c := openCache(t)
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()

	scheme, host, suite := "http", "archive.test", "/ubuntu/dists/noble"
	ir := seedBlob(t, c, "current inrelease bytes")
	path := suite + "/InRelease"
	putURLPathRow(t, c, scheme, host, path, ir,
		sql.NullInt64{Int64: now - 30*86400, Valid: true}, true)
	seedCurrentSnapshot(t, c, scheme, host, suite, ir, now)

	agg := drainURLPathGC(t, c, 100, 7*86400, 0, testMaxVersions)
	if agg.Deleted != 0 {
		t.Errorf("deleted = %d, want 0 (InRelease anchor on current snapshot protected)", agg.Deleted)
	}
}

func TestRunURLPathGCBatch_DetachedReleaseAndGPGProtected(t *testing.T) {
	c := openCache(t)
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()

	scheme, host, suite := "http", "archive.test", "/ubuntu/dists/noble"
	rel := seedBlob(t, c, "detached release bytes")
	rgpg := seedBlob(t, c, "detached release.gpg bytes")
	putURLPathRow(t, c, scheme, host, suite+"/Release", rel,
		sql.NullInt64{Int64: now - 30*86400, Valid: true}, true)
	putURLPathRow(t, c, scheme, host, suite+"/Release.gpg", rgpg,
		sql.NullInt64{Int64: now - 30*86400, Valid: true}, true)

	if _, err := c.db.Exec(`INSERT INTO suite_freshness
	  (canonical_scheme, canonical_host, suite_path) VALUES (?, ?, ?)`, scheme, host, suite); err != nil {
		t.Fatalf("seed suite_freshness: %v", err)
	}
	res, err := c.db.Exec(`INSERT INTO suite_snapshot
	  (canonical_scheme, canonical_host, suite_path, release_hash, release_gpg_hash,
	   created_at, adopted_at, heartbeat_at, package_coverage_complete)
	  VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1)`, scheme, host, suite, rel, rgpg, now, now, now)
	if err != nil {
		t.Fatalf("seed suite_snapshot: %v", err)
	}
	snapID, _ := res.LastInsertId()
	if _, err := c.db.Exec(`UPDATE suite_freshness SET current_snapshot_id = ?
	  WHERE canonical_scheme=? AND canonical_host=? AND suite_path=?`, snapID, scheme, host, suite); err != nil {
		t.Fatalf("set current_snapshot_id: %v", err)
	}

	agg := drainURLPathGC(t, c, 100, 7*86400, 0, testMaxVersions)
	if agg.Deleted != 0 {
		t.Errorf("deleted = %d, want 0 (Release + Release.gpg anchors protected)", agg.Deleted)
	}
}

// TestRunURLPathGCBatch_InReleaseAnchorProtectedWhenBlobHashDiverges is the
// freshness-freeze regression (identity guard d): an aged InRelease anchor
// whose blob_hash diverged from the current snapshot's inrelease_hash must
// still be protected by path identity.
func TestRunURLPathGCBatch_InReleaseAnchorProtectedWhenBlobHashDiverges(t *testing.T) {
	c := openCache(t)
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()

	scheme, host, suite := "http", "archive.test", "/ubuntu/dists/noble"
	stale := seedBlob(t, c, "stale client-fetched inrelease")
	adopted := seedBlob(t, c, "adopted (newer) inrelease bytes")
	path := suite + "/InRelease"
	putURLPathRow(t, c, scheme, host, path, stale,
		sql.NullInt64{Int64: now - 30*86400, Valid: true}, true)
	seedCurrentSnapshot(t, c, scheme, host, suite, adopted, now) // inrelease_hash = adopted ≠ stale

	agg := drainURLPathGC(t, c, 100, 7*86400, 0, testMaxVersions)
	if agg.Deleted != 0 {
		t.Errorf("deleted = %d, want 0 (InRelease anchor protected by identity despite diverged blob_hash)", agg.Deleted)
	}
}

func TestRunURLPathGCBatch_OppositeFormAnchorNotProtected(t *testing.T) {
	c := openCache(t)
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()

	scheme, host, suite := "http", "archive.test", "/ubuntu/dists/noble"
	ir := seedBlob(t, c, "inline inrelease")
	staleRel := seedBlob(t, c, "stale opposite-form release")
	putURLPathRow(t, c, scheme, host, suite+"/InRelease", ir,
		sql.NullInt64{Int64: now - 30*86400, Valid: true}, true)
	putURLPathRow(t, c, scheme, host, suite+"/Release", staleRel,
		sql.NullInt64{Int64: now - 30*86400, Valid: true}, true)
	seedCurrentSnapshot(t, c, scheme, host, suite, ir, now) // inline (inrelease only)

	agg := drainURLPathGC(t, c, 100, 7*86400, 0, testMaxVersions)
	if agg.Deleted != 1 {
		t.Errorf("deleted = %d, want 1 (stale opposite-form /Release reaped)", agg.Deleted)
	}
	if !urlPathExists(t, c, suite+"/InRelease") {
		t.Error("active /InRelease anchor should survive")
	}
	if urlPathExists(t, c, suite+"/Release") {
		t.Error("opposite-form /Release anchor should be reaped")
	}
}

func TestRunURLPathGCBatch_SnapshotMemberHashProtects(t *testing.T) {
	c := openCache(t)
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()

	scheme, host, suite := "http", "archive.test", "/ubuntu/dists/noble"
	pkg := seedBlob(t, c, "Packages.gz bytes")
	ir := seedBlob(t, c, "inrelease for protecting snapshot")
	path := suite + "/main/binary-amd64/Packages.gz"
	putURLPathRow(t, c, scheme, host, path, pkg,
		sql.NullInt64{Int64: now - 30*86400, Valid: true}, true)
	snapID := seedCurrentSnapshot(t, c, scheme, host, suite, ir, now)
	if _, err := c.db.Exec(`INSERT INTO snapshot_member
	  (snapshot_id, path, blob_hash, declared_sha256)
	  VALUES (?, 'main/binary-amd64/Packages.gz', ?, ?)`, snapID, pkg, pkg); err != nil {
		t.Fatalf("seed snapshot_member: %v", err)
	}

	agg := drainURLPathGC(t, c, 100, 7*86400, 0, testMaxVersions)
	if agg.Deleted != 0 {
		t.Errorf("deleted = %d, want 0 (snapshot_member blob_hash on current snapshot protects)", agg.Deleted)
	}
}

func TestRunURLPathGCBatch_DecrementPreservesPositiveRefcount(t *testing.T) {
	c := openCache(t)
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()

	h := seedBlob(t, c, "multi-ref aged blob")
	makeBlobPositiveRefcount(t, c, h, 2)
	putURLPathRow(t, c, "http", "ex.test", "/p/multi.deb", h,
		sql.NullInt64{Int64: now - 30*86400, Valid: true}, false)

	agg := drainURLPathGC(t, c, 100, 7*86400, 0, testMaxVersions)
	if agg.Deleted != 1 {
		t.Errorf("deleted = %d, want 1", agg.Deleted)
	}
	if rc := blobRefcount(t, c, h); rc != 1 {
		t.Errorf("blob.refcount = %d, want 1 (decremented from 2)", rc)
	}
	if z := blobZeroedAt(t, c, h); z != -1 {
		t.Errorf("refcount_zeroed_at = %d, want -1/NULL (refcount still > 0)", z)
	}
}

// --- dropped_at hold-grace lifecycle ---

// TestRunURLPathGCBatch_HoldGraceStampsThenDeletes: with a hold window, an
// unretained row is first stamped (dropped_at = now), survives while in
// grace, and is deleted only once the grace elapses.
func TestRunURLPathGCBatch_HoldGraceStampsThenDeletes(t *testing.T) {
	c := openCache(t)
	const now = int64(2_000_000_000)
	const hold = int64(86400) // 24h grace
	restore := stubNow(t, now)

	h := seedBlob(t, c, "grace blob")
	putURLPathRow(t, c, "http", "ex.test", "/p/grace.deb", h,
		sql.NullInt64{Int64: now - 30*86400, Valid: true}, false)

	// Pass 1: stamp, do not delete.
	agg := drainURLPathGC(t, c, 100, 7*86400, hold, testMaxVersions)
	if agg.Stamped != 1 || agg.Deleted != 0 {
		t.Fatalf("pass1: stamped=%d deleted=%d, want stamped=1 deleted=0", agg.Stamped, agg.Deleted)
	}
	if urlPathDroppedAt(t, c, "/p/grace.deb") != now {
		t.Fatalf("dropped_at not stamped to now")
	}
	restore()

	// Still inside grace: no delete.
	restore2 := stubNow(t, now+hold-1)
	agg = drainURLPathGC(t, c, 100, 7*86400, hold, testMaxVersions)
	if agg.Deleted != 0 {
		t.Errorf("in-grace: deleted=%d, want 0", agg.Deleted)
	}
	restore2()

	// Grace elapsed: delete.
	defer stubNow(t, now+hold)()
	agg = drainURLPathGC(t, c, 100, 7*86400, hold, testMaxVersions)
	if agg.Deleted != 1 {
		t.Errorf("post-grace: deleted=%d, want 1", agg.Deleted)
	}
	if urlPathExists(t, c, "/p/grace.deb") {
		t.Error("row should be deleted after grace elapsed")
	}
}

// TestRunURLPathGCBatch_HoldGraceClearedOnRequalify: a stamped row that
// becomes retained again (recency) has its dropped_at cleared.
func TestRunURLPathGCBatch_HoldGraceClearedOnRequalify(t *testing.T) {
	c := openCache(t)
	const now = int64(2_000_000_000)
	const hold = int64(86400)
	restore := stubNow(t, now)

	h := seedBlob(t, c, "requalify blob")
	putURLPathRow(t, c, "http", "ex.test", "/p/requalify.deb", h,
		sql.NullInt64{Int64: now - 30*86400, Valid: true}, false)

	// Stamp it.
	_ = drainURLPathGC(t, c, 100, 7*86400, hold, testMaxVersions)
	if urlPathDroppedAt(t, c, "/p/requalify.deb") != now {
		t.Fatalf("expected stamped")
	}
	// A fresh client request makes it recency-retained again.
	if _, err := c.db.Exec(`UPDATE url_path SET last_requested_at=? WHERE path='/p/requalify.deb'`, now); err != nil {
		t.Fatal(err)
	}
	restore()

	defer stubNow(t, now+1)()
	agg := drainURLPathGC(t, c, 100, 7*86400, hold, testMaxVersions)
	if agg.Cleared != 1 {
		t.Errorf("cleared = %d, want 1 (re-qualified by recency)", agg.Cleared)
	}
	if urlPathDroppedAt(t, c, "/p/requalify.deb") != -1 {
		t.Error("dropped_at should be cleared to NULL after re-qualification")
	}
}
