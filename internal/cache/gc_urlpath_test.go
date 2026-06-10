package cache

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// AIDEV-NOTE: tests for RunURLPathGCBatch — the SPEC4 §5 fourth GC
// reap class. Mirrors the gc_test.go style: stubNow + seedBlob +
// seedURLPath, assert reaped counts and refcount bookkeeping.

// putURLPathRow inserts a url_path row with a chosen last_requested_at,
// bypassing the cache helpers so tests can plant aged or NULL-stamped
// rows directly.
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

// makeBlobPositiveRefcount sets blob.refcount to n and clears
// refcount_zeroed_at, matching the bookkeeping CommitAdoption Step 4
// applies on a strictly-positive refcount crossing (Rule 2). Tests use
// this to set up rows whose pre-decrement refcount is > 0 so the
// URL-path pass's COALESCE + IIF logic can be exercised at the "still
// > 0 after decrement" boundary.
func makeBlobPositiveRefcount(t *testing.T, c *Cache, hash string, n int64) {
	t.Helper()
	if _, err := c.db.Exec(
		`UPDATE blob SET refcount = ?, refcount_zeroed_at = NULL WHERE hash = ?`,
		n, hash,
	); err != nil {
		t.Fatalf("makeBlobPositiveRefcount: %v", err)
	}
}

func TestRunURLPathGCBatch_DisabledWhenTTLZero(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	if _, err := c.RunURLPathGCBatch(ctx, 100, 0); err == nil {
		t.Fatal("RunURLPathGCBatch(ttlSeconds=0): want error, got nil")
	}
}

func TestRunURLPathGCBatch_ReapsAgedRow(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()

	h := seedBlob(t, c, "aged blob")
	// last_requested_at = 10 days ago; ttl = 7 days → reapable.
	putURLPathRow(t, c, "http", "ex.test", "/p/aged.deb", h,
		sql.NullInt64{Int64: now - 10*86400, Valid: true}, false)

	got, err := c.RunURLPathGCBatch(ctx, 100, 7*86400)
	if err != nil {
		t.Fatalf("RunURLPathGCBatch: %v", err)
	}
	if got != 1 {
		t.Errorf("reaped = %d, want 1", got)
	}

	// Row should be gone.
	var n int
	if err := c.db.QueryRow(`SELECT count(*) FROM url_path WHERE path='/p/aged.deb'`).Scan(&n); err != nil {
		t.Fatalf("count url_path: %v", err)
	}
	if n != 0 {
		t.Errorf("url_path row survived: count=%d", n)
	}

	// Refcount decremented; refcount_zeroed_at set on 0→-1 crossing.
	if rc := blobRefcount(t, c, h); rc != -1 {
		t.Errorf("blob.refcount = %d, want -1 after decrement", rc)
	}
	if z := blobZeroedAt(t, c, h); z != now {
		t.Errorf("refcount_zeroed_at = %d, want %d (now)", z, now)
	}
}

func TestRunURLPathGCBatch_FreshRowProtected(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()

	h := seedBlob(t, c, "fresh blob")
	// last_requested_at = 1 day ago; ttl = 7 days → protected.
	putURLPathRow(t, c, "http", "ex.test", "/p/fresh.deb", h,
		sql.NullInt64{Int64: now - 86400, Valid: true}, false)

	got, err := c.RunURLPathGCBatch(ctx, 100, 7*86400)
	if err != nil {
		t.Fatalf("RunURLPathGCBatch: %v", err)
	}
	if got != 0 {
		t.Errorf("reaped = %d, want 0 (row inside TTL)", got)
	}
}

func TestRunURLPathGCBatch_NullLastRequestedProtected(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()

	h := seedBlob(t, c, "prewarm blob")
	// last_requested_at = NULL (adoption pre-warmed, never served).
	putURLPathRow(t, c, "http", "ex.test", "/p/prewarm.deb", h,
		sql.NullInt64{}, false)

	got, err := c.RunURLPathGCBatch(ctx, 100, 7*86400)
	if err != nil {
		t.Fatalf("RunURLPathGCBatch: %v", err)
	}
	if got != 0 {
		t.Errorf("reaped = %d, want 0 (last_requested_at IS NULL is unconditionally protected)", got)
	}
}

func TestRunURLPathGCBatch_PackageHashOnCurrentSnapshotProtects(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()

	h := seedBlob(t, c, "vouched aged blob")
	ir := seedBlob(t, c, "current inrelease")
	const path = "/ubuntu/pool/main/x/x.deb"
	putURLPathRow(t, c, "http", "archive.test", path, h,
		sql.NullInt64{Int64: now - 30*86400, Valid: true}, false)

	// Plant a suite_freshness + suite_snapshot + package_hash row that
	// vouches for the same (scheme, host, path) on a current snapshot.
	// suite_freshness must come first (snapshot row references suite via
	// scheme/host/suite_path tuple; suite_freshness column carries the
	// current_snapshot_id pointer we set after the snapshot insert).
	if _, err := c.db.Exec(`INSERT INTO suite_freshness
	  (canonical_scheme, canonical_host, suite_path)
	  VALUES ('http', 'archive.test', '/ubuntu/dists/noble')`); err != nil {
		t.Fatalf("seed suite_freshness: %v", err)
	}
	res, err := c.db.Exec(`INSERT INTO suite_snapshot
	  (canonical_scheme, canonical_host, suite_path,
	   inrelease_hash, created_at, adopted_at, heartbeat_at,
	   package_coverage_complete)
	  VALUES ('http', 'archive.test', '/ubuntu/dists/noble',
	          ?, ?, ?, ?, 1)`, ir, now, now, now)
	if err != nil {
		t.Fatalf("seed suite_snapshot: %v", err)
	}
	snapID, _ := res.LastInsertId()
	if _, err := c.db.Exec(`UPDATE suite_freshness SET current_snapshot_id = ?
	  WHERE canonical_scheme='http' AND canonical_host='archive.test'
	    AND suite_path='/ubuntu/dists/noble'`, snapID); err != nil {
		t.Fatalf("set current_snapshot_id: %v", err)
	}
	if _, err := c.db.Exec(`INSERT INTO package_hash
	  (canonical_scheme, canonical_host, path,
	   declared_sha256, snapshot_id, package_name, architecture)
	  VALUES ('http', 'archive.test', ?, ?, ?, 'x', 'amd64')`,
		path, h, snapID); err != nil {
		t.Fatalf("seed package_hash: %v", err)
	}

	got, err := c.RunURLPathGCBatch(ctx, 100, 7*86400)
	if err != nil {
		t.Fatalf("RunURLPathGCBatch: %v", err)
	}
	if got != 0 {
		t.Errorf("reaped = %d, want 0 (vouched by current snapshot's package_hash)", got)
	}
}

func TestRunURLPathGCBatch_PackageHashOnDisplacedSnapshotDoesNotProtect(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()

	h := seedBlob(t, c, "displaced-vouched aged blob")
	ir := seedBlob(t, c, "displaced inrelease")
	const path = "/ubuntu/pool/main/y/y.deb"
	putURLPathRow(t, c, "http", "archive.test", path, h,
		sql.NullInt64{Int64: now - 30*86400, Valid: true}, false)

	// Seed a snapshot that is adopted but NOT current (displaced).
	// current_snapshot_id stays NULL.
	if _, err := c.db.Exec(`INSERT INTO suite_freshness
	  (canonical_scheme, canonical_host, suite_path)
	  VALUES ('http', 'archive.test', '/ubuntu/dists/noble')`); err != nil {
		t.Fatalf("seed suite_freshness: %v", err)
	}
	res, err := c.db.Exec(`INSERT INTO suite_snapshot
	  (canonical_scheme, canonical_host, suite_path,
	   inrelease_hash, created_at, adopted_at, heartbeat_at,
	   package_coverage_complete)
	  VALUES ('http', 'archive.test', '/ubuntu/dists/noble',
	          ?, ?, ?, ?, 1)`, ir, now, now, now)
	if err != nil {
		t.Fatalf("seed suite_snapshot: %v", err)
	}
	snapID, _ := res.LastInsertId()
	if _, err := c.db.Exec(`INSERT INTO package_hash
	  (canonical_scheme, canonical_host, path,
	   declared_sha256, snapshot_id, package_name, architecture)
	  VALUES ('http', 'archive.test', ?, ?, ?, 'y', 'amd64')`,
		path, h, snapID); err != nil {
		t.Fatalf("seed package_hash: %v", err)
	}

	got, err := c.RunURLPathGCBatch(ctx, 100, 7*86400)
	if err != nil {
		t.Fatalf("RunURLPathGCBatch: %v", err)
	}
	if got != 1 {
		t.Errorf("reaped = %d, want 1 (displaced snapshot's package_hash does NOT protect)", got)
	}
}

func TestRunURLPathGCBatch_BatchSizeLimits(t *testing.T) {
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

	first, err := c.RunURLPathGCBatch(ctx, 3, 7*86400)
	if err != nil {
		t.Fatalf("RunURLPathGCBatch first: %v", err)
	}
	if first != 3 {
		t.Errorf("first batch reaped = %d, want 3", first)
	}
	second, err := c.RunURLPathGCBatch(ctx, 3, 7*86400)
	if err != nil {
		t.Fatalf("RunURLPathGCBatch second: %v", err)
	}
	if second != 3 {
		t.Errorf("second batch reaped = %d, want 3", second)
	}
	third, err := c.RunURLPathGCBatch(ctx, 3, 7*86400)
	if err != nil {
		t.Fatalf("RunURLPathGCBatch third: %v", err)
	}
	if third != 1 {
		t.Errorf("third batch reaped = %d, want 1 (drain)", third)
	}
}

func TestRunURLPathGCBatch_PackageHashMismatchedHashDoesNotProtect(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()

	stale := seedBlob(t, c, "stale cached bytes")
	current := seedBlob(t, c, "current declared bytes")
	ir := seedBlob(t, c, "inrelease for the protecting snapshot")
	const path = "/ubuntu/pool/main/z/z.deb"
	putURLPathRow(t, c, "http", "archive.test", path, stale,
		sql.NullInt64{Int64: now - 30*86400, Valid: true}, false)

	if _, err := c.db.Exec(`INSERT INTO suite_freshness
	  (canonical_scheme, canonical_host, suite_path)
	  VALUES ('http', 'archive.test', '/ubuntu/dists/noble')`); err != nil {
		t.Fatalf("seed suite_freshness: %v", err)
	}
	res, err := c.db.Exec(`INSERT INTO suite_snapshot
	  (canonical_scheme, canonical_host, suite_path,
	   inrelease_hash, created_at, adopted_at, heartbeat_at,
	   package_coverage_complete)
	  VALUES ('http', 'archive.test', '/ubuntu/dists/noble',
	          ?, ?, ?, ?, 1)`, ir, now, now, now)
	if err != nil {
		t.Fatalf("seed suite_snapshot: %v", err)
	}
	snapID, _ := res.LastInsertId()
	if _, err := c.db.Exec(`UPDATE suite_freshness SET current_snapshot_id = ?
	  WHERE canonical_scheme='http' AND canonical_host='archive.test'
	    AND suite_path='/ubuntu/dists/noble'`, snapID); err != nil {
		t.Fatalf("set current_snapshot_id: %v", err)
	}
	// package_hash on the current snapshot declares a DIFFERENT hash
	// than what url_path caches. Stale row is reapable.
	if _, err := c.db.Exec(`INSERT INTO package_hash
	  (canonical_scheme, canonical_host, path,
	   declared_sha256, snapshot_id, package_name, architecture)
	  VALUES ('http', 'archive.test', ?, ?, ?, 'z', 'amd64')`,
		path, current, snapID); err != nil {
		t.Fatalf("seed package_hash: %v", err)
	}

	got, err := c.RunURLPathGCBatch(ctx, 100, 7*86400)
	if err != nil {
		t.Fatalf("RunURLPathGCBatch: %v", err)
	}
	if got != 1 {
		t.Errorf("reaped = %d, want 1 (cached blob_hash diverged from current package_hash.declared_sha256)", got)
	}
}

// TestRunURLPathGCBatch_InReleaseUrlPathOnCurrentSnapshotProtected guards
// against silent freshness skips. The freshness checker (SPEC2 §7.4)
// calls cache.LookupURL on suite_path+"/InRelease" and silently returns
// nil when the row is absent — so reaping an aged but still-current
// InRelease url_path row would stop periodic refreshes for that suite.
func TestRunURLPathGCBatch_InReleaseUrlPathOnCurrentSnapshotProtected(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()

	ir := seedBlob(t, c, "current inrelease bytes")
	const path = "/ubuntu/dists/noble/InRelease"
	putURLPathRow(t, c, "http", "archive.test", path, ir,
		sql.NullInt64{Int64: now - 30*86400, Valid: true}, true)

	if _, err := c.db.Exec(`INSERT INTO suite_freshness
	  (canonical_scheme, canonical_host, suite_path)
	  VALUES ('http', 'archive.test', '/ubuntu/dists/noble')`); err != nil {
		t.Fatalf("seed suite_freshness: %v", err)
	}
	res, err := c.db.Exec(`INSERT INTO suite_snapshot
	  (canonical_scheme, canonical_host, suite_path,
	   inrelease_hash, created_at, adopted_at, heartbeat_at,
	   package_coverage_complete)
	  VALUES ('http', 'archive.test', '/ubuntu/dists/noble',
	          ?, ?, ?, ?, 1)`, ir, now, now, now)
	if err != nil {
		t.Fatalf("seed suite_snapshot: %v", err)
	}
	snapID, _ := res.LastInsertId()
	if _, err := c.db.Exec(`UPDATE suite_freshness SET current_snapshot_id = ?
	  WHERE canonical_scheme='http' AND canonical_host='archive.test'
	    AND suite_path='/ubuntu/dists/noble'`, snapID); err != nil {
		t.Fatalf("set current_snapshot_id: %v", err)
	}

	got, err := c.RunURLPathGCBatch(ctx, 100, 7*86400)
	if err != nil {
		t.Fatalf("RunURLPathGCBatch: %v", err)
	}
	if got != 0 {
		t.Errorf("reaped = %d, want 0 (InRelease url_path on current snapshot must be protected)", got)
	}
}

// TestRunURLPathGCBatch_DetachedReleaseAndGPGProtected verifies the
// detached-mode metadata anchors (Release + Release.gpg) are also
// protected. Mirror of the InRelease test for SPEC2 §7.6.3 caches.
func TestRunURLPathGCBatch_DetachedReleaseAndGPGProtected(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()

	rel := seedBlob(t, c, "detached release bytes")
	rgpg := seedBlob(t, c, "detached release.gpg bytes")
	putURLPathRow(t, c, "http", "archive.test", "/ubuntu/dists/noble/Release", rel,
		sql.NullInt64{Int64: now - 30*86400, Valid: true}, true)
	putURLPathRow(t, c, "http", "archive.test", "/ubuntu/dists/noble/Release.gpg", rgpg,
		sql.NullInt64{Int64: now - 30*86400, Valid: true}, true)

	if _, err := c.db.Exec(`INSERT INTO suite_freshness
	  (canonical_scheme, canonical_host, suite_path)
	  VALUES ('http', 'archive.test', '/ubuntu/dists/noble')`); err != nil {
		t.Fatalf("seed suite_freshness: %v", err)
	}
	res, err := c.db.Exec(`INSERT INTO suite_snapshot
	  (canonical_scheme, canonical_host, suite_path,
	   release_hash, release_gpg_hash,
	   created_at, adopted_at, heartbeat_at,
	   package_coverage_complete)
	  VALUES ('http', 'archive.test', '/ubuntu/dists/noble',
	          ?, ?, ?, ?, ?, 1)`, rel, rgpg, now, now, now)
	if err != nil {
		t.Fatalf("seed suite_snapshot: %v", err)
	}
	snapID, _ := res.LastInsertId()
	if _, err := c.db.Exec(`UPDATE suite_freshness SET current_snapshot_id = ?
	  WHERE canonical_scheme='http' AND canonical_host='archive.test'
	    AND suite_path='/ubuntu/dists/noble'`, snapID); err != nil {
		t.Fatalf("set current_snapshot_id: %v", err)
	}

	got, err := c.RunURLPathGCBatch(ctx, 100, 7*86400)
	if err != nil {
		t.Fatalf("RunURLPathGCBatch: %v", err)
	}
	if got != 0 {
		t.Errorf("reaped = %d, want 0 (Release + Release.gpg url_path on current snapshot must be protected)", got)
	}
}

// TestCommitAdoption_SyncsInReleaseAnchorBlobHash is the regression test
// for the freeze trap's root cause (SPEC3 §7.5.1 Step 3c). Adoption must
// sync the InRelease url_path anchor's blob_hash to the newly-adopted
// snapshot hash so the SPEC4 §5 GC guards (b)/(c) can vouch for it — while
// PRESERVING the existing row's (port-correct) upstream_url rather than
// clobbering it with portless reconstruction.
func TestCommitAdoption_SyncsInReleaseAnchorBlobHash(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, 1_700_000_000)()

	stale := seedBlob(t, c, "stale client-fetched inrelease")
	adopted := seedBlob(t, c, "adopted inrelease bytes")

	const host = "archive.test"
	const suite = "/ubuntu/dists/noble"
	const anchorPath = suite + "/InRelease"
	const customURL = "http://archive.test:8080/ubuntu/dists/noble/InRelease"

	// Pre-existing anchor row from a prior client miss: blob_hash points
	// at the STALE bytes and carries a port-bearing upstream_url adoption
	// must PRESERVE.
	if _, err := c.db.Exec(`INSERT INTO url_path
	  (canonical_scheme, canonical_host, path, blob_hash, upstream_url,
	   is_metadata, last_requested_at, request_count)
	  VALUES ('http', ?, ?, ?, ?, 1, ?, 5)`,
		host, anchorPath, stale, customURL, int64(1_699_000_000)); err != nil {
		t.Fatalf("seed anchor: %v", err)
	}

	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: host, SuitePath: suite,
		InReleaseHash: &adopted,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id, []SnapshotMember{
		{SnapshotID: id, Path: "InRelease", BlobHash: adopted, DeclaredSHA256: adopted},
	}, nil,

		nil, nil, false); err != nil {
		t.Fatalf("CommitAdoption: %v", err)
	}

	u, err := c.LookupURL(ctx, "http", host, anchorPath)
	if err != nil {
		t.Fatalf("anchor LookupURL: %v", err)
	}
	if u.BlobHash == nil || *u.BlobHash != adopted {
		t.Errorf("anchor blob_hash = %v, want adopted snapshot inrelease_hash %s", u.BlobHash, adopted)
	}
	if u.UpstreamURL != customURL {
		t.Errorf("anchor upstream_url = %q, want preserved %q", u.UpstreamURL, customURL)
	}
	if !u.IsMetadata {
		t.Error("anchor is_metadata must remain 1")
	}
}

// TestCommitAdoption_CreatesInReleaseAnchorWhenMissing covers the case
// where the anchor was already reaped before adoption (e.g. Layer A
// recovery is racing): adoption creates it with the reconstructed URL.
func TestCommitAdoption_CreatesInReleaseAnchorWhenMissing(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, 1_700_000_000)()

	adopted := seedBlob(t, c, "adopted inrelease bytes (no prior anchor)")
	const host = "archive.test"
	const suite = "/ubuntu/dists/noble"
	const anchorPath = suite + "/InRelease"

	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: host, SuitePath: suite,
		InReleaseHash: &adopted,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id, []SnapshotMember{
		{SnapshotID: id, Path: "InRelease", BlobHash: adopted, DeclaredSHA256: adopted},
	}, nil,

		nil, nil, false); err != nil {
		t.Fatalf("CommitAdoption: %v", err)
	}

	u, err := c.LookupURL(ctx, "http", host, anchorPath)
	if err != nil {
		t.Fatalf("anchor not created by adoption: %v", err)
	}
	if u.BlobHash == nil || *u.BlobHash != adopted {
		t.Errorf("created anchor blob_hash = %v, want %s", u.BlobHash, adopted)
	}
	if u.UpstreamURL != "http://"+host+anchorPath {
		t.Errorf("created anchor upstream_url = %q, want reconstructed %q", u.UpstreamURL, "http://"+host+anchorPath)
	}
	if !u.IsMetadata {
		t.Error("created anchor is_metadata must be 1")
	}
}

// TestRunURLPathGCBatch_InReleaseAnchorProtectedWhenBlobHashDiverges is
// the regression test for the freshness-freeze trap (SPEC4 §5 guard (d)).
//
// In production the InRelease anchor's url_path.blob_hash points at the
// last CLIENT-fetched bytes, while the adopted snapshot's inrelease_hash
// is a DIFFERENT (re-adopted) blob — CommitAdoption historically never
// synced the anchor. The hash-equality guard (c) therefore can NOT
// protect the anchor, so an aged anchor is reaped and the suite freezes
// (the checker silently skips when the row is absent — SPEC2 §7.4). The
// identity guard (d) must protect the anchor by path for any suite with
// a current snapshot, regardless of blob_hash.
//
// NOTE: the sibling _OnCurrentSnapshotProtected test masks this defect —
// it seeds url_path.blob_hash == snapshot.inrelease_hash, the exact
// equality production never satisfies.
func TestRunURLPathGCBatch_InReleaseAnchorProtectedWhenBlobHashDiverges(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()

	stale := seedBlob(t, c, "stale client-fetched inrelease")
	adopted := seedBlob(t, c, "adopted (newer) inrelease bytes")
	const path = "/ubuntu/dists/noble/InRelease"
	// Anchor aged past the 7-day TTL, blob_hash = the STALE client blob.
	putURLPathRow(t, c, "http", "archive.test", path, stale,
		sql.NullInt64{Int64: now - 30*86400, Valid: true}, true)

	if _, err := c.db.Exec(`INSERT INTO suite_freshness
	  (canonical_scheme, canonical_host, suite_path)
	  VALUES ('http', 'archive.test', '/ubuntu/dists/noble')`); err != nil {
		t.Fatalf("seed suite_freshness: %v", err)
	}
	// Current snapshot's inrelease_hash = the ADOPTED blob (≠ stale).
	res, err := c.db.Exec(`INSERT INTO suite_snapshot
	  (canonical_scheme, canonical_host, suite_path,
	   inrelease_hash, created_at, adopted_at, heartbeat_at,
	   package_coverage_complete)
	  VALUES ('http', 'archive.test', '/ubuntu/dists/noble', ?, ?, ?, ?, 1)`,
		adopted, now, now, now)
	if err != nil {
		t.Fatalf("seed suite_snapshot: %v", err)
	}
	snapID, _ := res.LastInsertId()
	if _, err := c.db.Exec(`UPDATE suite_freshness SET current_snapshot_id = ?
	  WHERE canonical_scheme='http' AND canonical_host='archive.test'
	    AND suite_path='/ubuntu/dists/noble'`, snapID); err != nil {
		t.Fatalf("set current_snapshot_id: %v", err)
	}

	got, err := c.RunURLPathGCBatch(ctx, 100, 7*86400)
	if err != nil {
		t.Fatalf("RunURLPathGCBatch: %v", err)
	}
	if got != 0 {
		t.Errorf("reaped = %d, want 0 (InRelease anchor of a current snapshot must be protected by identity even when blob_hash diverged)", got)
	}
}

// TestRunURLPathGCBatch_OppositeFormAnchorNotProtected verifies the
// identity guard (d) protects only the suite's ACTIVE metadata form: an
// inline current snapshot must NOT keep a stale /Release (or
// /Release.gpg) row alive past the TTL, and a detached snapshot must NOT
// keep a stale /InRelease row. Otherwise opposite-form anchors (and the
// blobs they pin) leak forever.
func TestRunURLPathGCBatch_OppositeFormAnchorNotProtected(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()

	ir := seedBlob(t, c, "inline inrelease")
	staleRel := seedBlob(t, c, "stale opposite-form release")
	// Inline suite: snapshot carries inrelease_hash only.
	putURLPathRow(t, c, "http", "archive.test", "/ubuntu/dists/noble/InRelease", ir,
		sql.NullInt64{Int64: now - 30*86400, Valid: true}, true)
	// Opposite-form (Release) anchor, aged, NOT vouched by any guard but
	// (the over-broad) identity guard.
	putURLPathRow(t, c, "http", "archive.test", "/ubuntu/dists/noble/Release", staleRel,
		sql.NullInt64{Int64: now - 30*86400, Valid: true}, true)

	if _, err := c.db.Exec(`INSERT INTO suite_freshness
	  (canonical_scheme, canonical_host, suite_path)
	  VALUES ('http', 'archive.test', '/ubuntu/dists/noble')`); err != nil {
		t.Fatalf("seed suite_freshness: %v", err)
	}
	res, err := c.db.Exec(`INSERT INTO suite_snapshot
	  (canonical_scheme, canonical_host, suite_path, inrelease_hash,
	   created_at, adopted_at, heartbeat_at, package_coverage_complete)
	  VALUES ('http','archive.test','/ubuntu/dists/noble', ?, ?, ?, ?, 1)`,
		ir, now, now, now)
	if err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	snapID, _ := res.LastInsertId()
	if _, err := c.db.Exec(`UPDATE suite_freshness SET current_snapshot_id=?
	  WHERE canonical_host='archive.test' AND suite_path='/ubuntu/dists/noble'`, snapID); err != nil {
		t.Fatalf("set current_snapshot_id: %v", err)
	}

	got, err := c.RunURLPathGCBatch(ctx, 100, 7*86400)
	if err != nil {
		t.Fatalf("RunURLPathGCBatch: %v", err)
	}
	if got != 1 {
		t.Errorf("reaped = %d, want 1 (stale opposite-form /Release reaped; active /InRelease kept)", got)
	}
	var n int
	_ = c.db.QueryRow(`SELECT count(*) FROM url_path WHERE path='/ubuntu/dists/noble/InRelease'`).Scan(&n)
	if n != 1 {
		t.Errorf("active /InRelease anchor should survive: count=%d", n)
	}
	_ = c.db.QueryRow(`SELECT count(*) FROM url_path WHERE path='/ubuntu/dists/noble/Release'`).Scan(&n)
	if n != 0 {
		t.Errorf("opposite-form /Release anchor should be reaped: count=%d", n)
	}
}

// TestRunURLPathGCBatch_SnapshotMemberHashProtects covers cached
// Packages.gz / Sources / pdiff Index members: they are tracked in
// snapshot_member by hash, and aged url_path rows pointing at those
// hashes must stay so subsequent hit-path lookups continue to serve.
func TestRunURLPathGCBatch_SnapshotMemberHashProtects(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()

	pkg := seedBlob(t, c, "Packages.gz bytes")
	ir := seedBlob(t, c, "inrelease for protecting snapshot")
	const path = "/ubuntu/dists/noble/main/binary-amd64/Packages.gz"
	putURLPathRow(t, c, "http", "archive.test", path, pkg,
		sql.NullInt64{Int64: now - 30*86400, Valid: true}, true)

	if _, err := c.db.Exec(`INSERT INTO suite_freshness
	  (canonical_scheme, canonical_host, suite_path)
	  VALUES ('http', 'archive.test', '/ubuntu/dists/noble')`); err != nil {
		t.Fatalf("seed suite_freshness: %v", err)
	}
	res, err := c.db.Exec(`INSERT INTO suite_snapshot
	  (canonical_scheme, canonical_host, suite_path,
	   inrelease_hash, created_at, adopted_at, heartbeat_at,
	   package_coverage_complete)
	  VALUES ('http', 'archive.test', '/ubuntu/dists/noble',
	          ?, ?, ?, ?, 1)`, ir, now, now, now)
	if err != nil {
		t.Fatalf("seed suite_snapshot: %v", err)
	}
	snapID, _ := res.LastInsertId()
	if _, err := c.db.Exec(`UPDATE suite_freshness SET current_snapshot_id = ?
	  WHERE canonical_scheme='http' AND canonical_host='archive.test'
	    AND suite_path='/ubuntu/dists/noble'`, snapID); err != nil {
		t.Fatalf("set current_snapshot_id: %v", err)
	}
	if _, err := c.db.Exec(`INSERT INTO snapshot_member
	  (snapshot_id, path, blob_hash, declared_sha256)
	  VALUES (?, 'main/binary-amd64/Packages.gz', ?, ?)`,
		snapID, pkg, pkg); err != nil {
		t.Fatalf("seed snapshot_member: %v", err)
	}

	got, err := c.RunURLPathGCBatch(ctx, 100, 7*86400)
	if err != nil {
		t.Fatalf("RunURLPathGCBatch: %v", err)
	}
	if got != 0 {
		t.Errorf("reaped = %d, want 0 (snapshot_member blob_hash on current snapshot must protect)", got)
	}
}

func TestRunURLPathGCBatch_DecrementPreservesPositiveRefcount(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()

	h := seedBlob(t, c, "multi-ref aged blob")
	// Set refcount to 2 and clear refcount_zeroed_at, simulating Rule 2
	// (CommitAdoption Step 4 positive crossing). A single url_path
	// eviction drops refcount to 1 — not <= 0, so refcount_zeroed_at
	// must remain NULL.
	makeBlobPositiveRefcount(t, c, h, 2)

	putURLPathRow(t, c, "http", "ex.test", "/p/multi.deb", h,
		sql.NullInt64{Int64: now - 30*86400, Valid: true}, false)

	got, err := c.RunURLPathGCBatch(ctx, 100, 7*86400)
	if err != nil {
		t.Fatalf("RunURLPathGCBatch: %v", err)
	}
	if got != 1 {
		t.Errorf("reaped = %d, want 1", got)
	}
	if rc := blobRefcount(t, c, h); rc != 1 {
		t.Errorf("blob.refcount = %d, want 1 (decremented from 2)", rc)
	}
	if z := blobZeroedAt(t, c, h); z != -1 {
		t.Errorf("refcount_zeroed_at = %d, want -1/NULL (refcount still > 0)", z)
	}
}
