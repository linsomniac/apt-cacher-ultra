package gc

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// SPEC4 §12.2 integration tests.
//
// These exercise the gc package against a real cache.Cache, real
// SQLite, and real pool/ files — no fakes for the layers under
// test. The cache package's writer goroutine, schema CHECKs, and FK
// cascade behavior are part of what's being tested.
//
// Clock manipulation:
//   - Tests do not stub cache.nowUnix (it's package-private).
//   - Instead they fast-forward refcount_zeroed_at by direct UPDATE
//     on the cache file, which is safe between submitWrite-bracketed
//     operations because the writer is idle after the prior op
//     returns.

// putPoolBlob writes content into the cache's pool/, returns its
// hash, and registers it via PutBlob so the row exists at refcount=0
// with refcount_zeroed_at=now. Mirrors the §6.2 cache-miss path that
// would land a blob during normal operation.
func putPoolBlob(t *testing.T, c *cache.Cache, content string) string {
	t.Helper()
	w, err := c.NewTempBlob()
	if err != nil {
		t.Fatalf("NewTempBlob: %v", err)
	}
	if _, err := w.Write([]byte(content)); err != nil {
		t.Fatalf("write: %v", err)
	}
	hash, err := w.Finalize(int64(len(content)))
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if err := c.PutBlob(context.Background(), hash, int64(len(content))); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	return hash
}

// adoptSnapshot inserts a candidate snapshot for (scheme, host, suite)
// pinning members[i] = (path "M<i>", blobHash blobHashes[i]) and then
// commits it. inreleaseHash is the natural-key seed; pass a unique
// blob hash per call so the suite_snapshot natural key is distinct.
// Returns the snapshot id.
func adoptSnapshot(t *testing.T, c *cache.Cache, scheme, host, suite, inreleaseHash string, blobHashes []string) int64 {
	t.Helper()
	ctx := context.Background()
	id, _, err := c.InsertCandidateSnapshot(ctx, cache.SnapshotCandidate{
		CanonicalScheme:         scheme,
		CanonicalHost:           host,
		SuitePath:               suite,
		InReleaseHash:           &inreleaseHash,
		PackageCoverageComplete: true,
	})
	if err != nil {
		t.Fatalf("InsertCandidateSnapshot: %v", err)
	}
	members := make([]cache.SnapshotMember, len(blobHashes))
	for i, h := range blobHashes {
		members[i] = cache.SnapshotMember{
			SnapshotID:     id,
			Path:           fmt.Sprintf("M%d", i),
			BlobHash:       h,
			DeclaredSHA256: h,
		}
	}
	if err := c.CommitAdoption(ctx, id, members, nil, nil, true); err != nil {
		t.Fatalf("CommitAdoption: %v", err)
	}
	return id
}

// blobRefcount returns (refcount, refcount_zeroed_at) for a blob row.
// The sql.NullInt64 carries the NULL distinction directly. Helper for
// end-to-end assertions.
func blobRefcount(t *testing.T, db *sql.DB, hash string) (int64, sql.NullInt64) {
	t.Helper()
	var refcount int64
	var zeroedAt sql.NullInt64
	err := db.QueryRow(
		`SELECT refcount, refcount_zeroed_at FROM blob WHERE hash = ?`, hash,
	).Scan(&refcount, &zeroedAt)
	if err != nil {
		t.Fatalf("blobRefcount(%s): %v", hash, err)
	}
	return refcount, zeroedAt
}

// fastForwardZeroedAt sets refcount_zeroed_at to 1 (epoch=1, "long
// ago") on the named blob so any positive grace is satisfied on the
// next GC tick. Must be called after the writer has finished any
// in-flight op.
func fastForwardZeroedAt(t *testing.T, db *sql.DB, hash string) {
	t.Helper()
	if _, err := db.Exec(
		`UPDATE blob SET refcount_zeroed_at = 1 WHERE hash = ?`, hash,
	); err != nil {
		t.Fatalf("fastForwardZeroedAt(%s): %v", hash, err)
	}
}

// blobRowExists is true iff the blob row is still in the table.
func blobRowExists(t *testing.T, db *sql.DB, hash string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM blob WHERE hash = ?`, hash,
	).Scan(&n); err != nil {
		t.Fatalf("blobRowExists(%s): %v", hash, err)
	}
	return n > 0
}

// snapshotRowExists is true iff the suite_snapshot row is still
// present.
func snapshotRowExists(t *testing.T, db *sql.DB, id int64) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM suite_snapshot WHERE snapshot_id = ?`, id,
	).Scan(&n); err != nil {
		t.Fatalf("snapshotRowExists(%d): %v", id, err)
	}
	return n > 0
}

// ---------------------------------------------------------------------------
// SPEC4 §12.2: GC end-to-end.
// ---------------------------------------------------------------------------

// TestGCEndToEnd_DisplacedSnapshot_DeadBlobReaped covers the §12.2
// "GC end-to-end" scenario:
//
//  1. Adopt S1 with members [B1, B2].
//  2. Adopt S2 with members [B1, B3]; S1 becomes displaced.
//  3. Assert B2 reached refcount=0 with refcount_zeroed_at set
//     (Rule 3, §7.5.1).
//  4. Fast-forward refcount_zeroed_at past gc.blob_grace.
//  5. Run a GC tick with keep_displaced=0 so S1 is reapable.
//  6. Assert B2's blob row is gone, B2's pool file is unlinked,
//     AND the hot blob B1 (still pinned by current S2) survives —
//     both as a row and on disk.
//  7. Assert B3 (also pinned by S2) survives.
func TestGCEndToEnd_DisplacedSnapshot_DeadBlobReaped(t *testing.T) {
	c := openTestCache(t)
	logger, buf := captureLogger()
	db := dbOf(t, c)

	const (
		scheme = "http"
		host   = "deb.example"
		suite  = "/dists/noble"
	)

	// Three pool blobs: B1 shared between snapshots, B2 only in S1
	// (displaced), B3 only in S2 (current).
	b1 := putPoolBlob(t, c, "B1 contents — shared between S1 and S2")
	b2 := putPoolBlob(t, c, "B2 contents — only S1, will be reaped")
	b3 := putPoolBlob(t, c, "B3 contents — only S2, survives")

	// Distinct InRelease bytes per snapshot so suite_snapshot's
	// natural key is unique. The InRelease hash itself is a
	// reachability anchor (§9.6.2 NOT EXISTS clause), so use
	// dedicated blobs that won't be tested for reap.
	inrel1 := putPoolBlob(t, c, "InRelease bytes for S1")
	inrel2 := putPoolBlob(t, c, "InRelease bytes for S2")

	s1 := adoptSnapshot(t, c, scheme, host, suite, inrel1, []string{b1, b2})
	s2 := adoptSnapshot(t, c, scheme, host, suite, inrel2, []string{b1, b3})

	// Assertion 3a: B2 should be at refcount=0 with refcount_zeroed_at set
	// (Rule 3 ran when S1 was displaced).
	if rc, z := blobRefcount(t, db, b2); rc != 0 || !z.Valid {
		t.Fatalf("B2 post-displace: refcount=%d zeroed_at=%v want 0/valid", rc, z)
	}
	// B1 is shared: +1 from S2 then -1 from S1 displacement. Net stays at
	// the prior value (1). zeroed_at should be NULL.
	if rc, z := blobRefcount(t, db, b1); rc != 1 || z.Valid {
		t.Fatalf("B1 shared: refcount=%d zeroed_at=%v want 1/NULL", rc, z)
	}
	// B3 introduced fresh by S2: refcount=1, zeroed_at NULL.
	if rc, z := blobRefcount(t, db, b3); rc != 1 || z.Valid {
		t.Fatalf("B3 current: refcount=%d zeroed_at=%v want 1/NULL", rc, z)
	}

	// Step 4: fast-forward the grace clock for B2 (and the orphan
	// inrel1, which is no longer current). Without this the GC tick
	// runs before any plausible grace and reaps nothing.
	fastForwardZeroedAt(t, db, b2)
	fastForwardZeroedAt(t, db, inrel1)

	// Step 5: GC tick with keep_displaced=0 so S1 is reapable.
	g, err := New(Config{
		Cache:               c,
		Logger:              logger,
		Enabled:             true,
		Interval:            time.Hour,
		BatchSize:           100,
		SnapshotBatchSize:   10,
		MaxTickDuration:     time.Minute,
		BlobGrace:           time.Second, // any > 0 satisfies; B2's zeroed_at=1 is far past
		KeepDisplaced:       0,
		PoolScanWorkers:     2,
		HeartbeatStaleGrace: 30 * time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := g.runTick(context.Background(), "test-end-to-end")
	if err != nil {
		t.Fatalf("runTick: %v", err)
	}

	// Step 6 assertions. Exact counts: only S1 should reap (S2 is
	// current); only B2 and inrel1 should reap. B1 is hot (refcount=1),
	// B3 is current (refcount=1), inrel2 is anchored by S2's
	// suite_snapshot.inrelease_hash AND fresh zeroed_at.
	if res.displacedReaped != 1 {
		t.Errorf("displacedReaped = %d, want 1 (only S1 should reap)", res.displacedReaped)
	}
	if res.blobsReaped != 2 {
		t.Errorf("blobsReaped = %d, want 2 (B2 + inrel1 should reap)", res.blobsReaped)
	}
	if snapshotRowExists(t, db, s1) {
		t.Errorf("S1 snapshot row %d still present after GC tick", s1)
	}
	if !snapshotRowExists(t, db, s2) {
		t.Errorf("S2 snapshot row %d (current) was reaped", s2)
	}
	if blobRowExists(t, db, b2) {
		t.Errorf("B2 blob row still present after reap")
	}
	if _, err := os.Stat(c.BlobPath(b2)); !os.IsNotExist(err) {
		t.Errorf("B2 pool file still on disk: stat err=%v", err)
	}

	// Step 7: hot blobs survive.
	if !blobRowExists(t, db, b1) {
		t.Errorf("B1 (hot, pinned by S2) was reaped")
	}
	if _, err := os.Stat(c.BlobPath(b1)); err != nil {
		t.Errorf("B1 pool file gone: %v", err)
	}
	if !blobRowExists(t, db, b3) {
		t.Errorf("B3 (current) was reaped")
	}
	if _, err := os.Stat(c.BlobPath(b3)); err != nil {
		t.Errorf("B3 pool file gone: %v", err)
	}

	// Sanity: gc_run_complete event would be emitted by Run/StartupPass,
	// not runTick directly. Just verify the deadline event did NOT fire
	// (the tick should have completed within budget).
	if strings.Contains(buf.String(), `"msg":"gc_tick_deadline_reached"`) {
		t.Errorf("unexpected gc_tick_deadline_reached: %s", buf.String())
	}
}

// TestGCEndToEnd_HotBlobNeverZeroed: a blob that stays pinned by a
// current snapshot through multiple adoptions never reaches refcount=0
// — its refcount_zeroed_at is NULL throughout, the §9.6.2 partial index
// excludes it, and it is never a GC candidate. Specific check that
// the §7.5.1 Rule 2/3 dance produces the right NULL invariant under
// stable membership.
func TestGCEndToEnd_HotBlobNeverZeroed(t *testing.T) {
	c := openTestCache(t)
	logger, _ := captureLogger()
	db := dbOf(t, c)

	const (
		scheme = "http"
		host   = "deb.example"
		suite  = "/dists/noble"
	)

	// Single pinned blob carried across three adoptions.
	hot := putPoolBlob(t, c, "Hot blob — pinned across all adoptions")
	inrel1 := putPoolBlob(t, c, "InRelease 1")
	inrel2 := putPoolBlob(t, c, "InRelease 2")
	inrel3 := putPoolBlob(t, c, "InRelease 3")

	adoptSnapshot(t, c, scheme, host, suite, inrel1, []string{hot})
	adoptSnapshot(t, c, scheme, host, suite, inrel2, []string{hot})
	adoptSnapshot(t, c, scheme, host, suite, inrel3, []string{hot})

	// hot must be at refcount=1 with zeroed_at=NULL after each adoption.
	if rc, z := blobRefcount(t, db, hot); rc != 1 || z.Valid {
		t.Fatalf("hot post-3-adoptions: refcount=%d zeroed_at=%v want 1/NULL", rc, z)
	}

	// Run GC with grace=1s, keep_displaced=0. The hot blob is pinned;
	// even if S1 and S2 are reaped (they are), hot is in S3's
	// snapshot_member list, refcount=1, never enters the candidate set.
	g, err := New(Config{
		Cache:               c,
		Logger:              logger,
		Enabled:             true,
		Interval:            time.Hour,
		BatchSize:           100,
		SnapshotBatchSize:   10,
		MaxTickDuration:     time.Minute,
		BlobGrace:           time.Second,
		KeepDisplaced:       0,
		PoolScanWorkers:     2,
		HeartbeatStaleGrace: 30 * time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := g.runTick(context.Background(), "test-hot"); err != nil {
		t.Fatalf("runTick: %v", err)
	}

	if !blobRowExists(t, db, hot) {
		t.Errorf("hot blob reaped despite being pinned by current snapshot")
	}
	if _, err := os.Stat(c.BlobPath(hot)); err != nil {
		t.Errorf("hot pool file gone: %v", err)
	}
	// Refcount untouched.
	if rc, z := blobRefcount(t, db, hot); rc != 1 || z.Valid {
		t.Errorf("hot post-GC: refcount=%d zeroed_at=%v want 1/NULL", rc, z)
	}
}

// ---------------------------------------------------------------------------
// SPEC4 §12.2: pool/ orphan scan startup.
// ---------------------------------------------------------------------------

// TestLastRunSummary_BeforeAndAfter exercises the SPEC5 §9.6
// accessor: returns (zero, false) before any run completes, and
// (populated, true) after StartupPass.
func TestLastRunSummary_BeforeAndAfter(t *testing.T) {
	c := openTestCache(t)
	logger, _ := captureLogger()

	g, err := New(Config{
		Cache:               c,
		Logger:              logger,
		Enabled:             true,
		Interval:            time.Hour,
		BatchSize:           100,
		SnapshotBatchSize:   10,
		MaxTickDuration:     time.Minute,
		BlobGrace:           time.Hour,
		KeepDisplaced:       3,
		PoolScanWorkers:     2,
		HeartbeatStaleGrace: 30 * time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Pre-run: (zero, false).
	if summary, ok := g.LastRunSummary(); ok {
		t.Errorf("LastRunSummary before run: ok=true, want false (summary=%+v)", summary)
	}

	if err := g.StartupPass(context.Background()); err != nil {
		t.Fatalf("StartupPass: %v", err)
	}

	summary, ok := g.LastRunSummary()
	if !ok {
		t.Fatal("LastRunSummary after StartupPass: ok=false, want true")
	}
	if summary.Phase != "startup" {
		t.Errorf("Phase = %q, want startup", summary.Phase)
	}
	if summary.AtUnixTime <= 0 {
		t.Errorf("AtUnixTime = %d, want positive", summary.AtUnixTime)
	}
	if summary.DurationSeconds < 0 {
		t.Errorf("DurationSeconds = %f, want >= 0", summary.DurationSeconds)
	}
	// Empty cache → zero counters.
	if summary.BlobsReaped != 0 {
		t.Errorf("BlobsReaped = %d, want 0 on empty cache", summary.BlobsReaped)
	}
}

// TestLastRunSummary_IsCopy verifies the returned struct is
// independent of subsequent runs — the caller can retain it without
// re-locking.
func TestLastRunSummary_IsCopy(t *testing.T) {
	g := &GC{}
	g.recordLastRun(LastRunSummary{Phase: "startup", BlobsReaped: 5})

	s1, _ := g.LastRunSummary()
	g.recordLastRun(LastRunSummary{Phase: "periodic", BlobsReaped: 99})

	if s1.Phase != "startup" || s1.BlobsReaped != 5 {
		t.Errorf("first snapshot mutated by second recordLastRun: %+v", s1)
	}
	s2, _ := g.LastRunSummary()
	if s2.Phase != "periodic" || s2.BlobsReaped != 99 {
		t.Errorf("second snapshot wrong: %+v", s2)
	}
}

// TestStartupPass_PoolOrphans_ReapedAndCounted plants three orphan
// pool files (no blob row) at correct prefixes and one referenced
// pool file (blob row exists). StartupPass runs the pool scan + a
// one-shot GC tick; the gc_run_complete log line names
// pool_orphans_repaired=3 and the bytes count matches; the
// referenced file survives.
func TestStartupPass_PoolOrphans_ReapedAndCounted(t *testing.T) {
	c := openTestCache(t)
	logger, buf := captureLogger()

	// 3 orphan files at correct prefixes — different prefixes so the
	// pool walker iterates more than one directory.
	type orphan struct {
		hash string
		body string
	}
	orphans := []orphan{
		{"a0" + strings.Repeat("0", 62), "orphan one"},
		{"b1" + strings.Repeat("1", 62), "orphan two — slightly bigger body"},
		{"c2" + strings.Repeat("2", 62), "orphan three — bigger again 0123456789"},
	}
	var totalOrphanBytes int64
	for _, o := range orphans {
		writePoolFile(t, c, o.hash[:2], o.hash, o.body)
		totalOrphanBytes += int64(len(o.body))
	}

	// One referenced file via NewTempBlob/Finalize/PutBlob.
	referenced := putPoolBlob(t, c, "referenced — must survive")

	g, err := New(Config{
		Cache:               c,
		Logger:              logger,
		Enabled:             true,
		Interval:            time.Hour,
		BatchSize:           100,
		SnapshotBatchSize:   10,
		MaxTickDuration:     time.Minute,
		BlobGrace:           time.Hour, // referenced is at refcount=0/zeroed_at=now; grace=1h prevents reap
		KeepDisplaced:       3,
		PoolScanWorkers:     2,
		HeartbeatStaleGrace: 30 * time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := g.StartupPass(context.Background()); err != nil {
		t.Fatalf("StartupPass: %v", err)
	}

	// Orphan files unlinked.
	for _, o := range orphans {
		path := filepath.Join(c.Dir(), "pool", o.hash[:2], o.hash)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("orphan %s still on disk: stat err=%v", o.hash, err)
		}
	}
	// Referenced file present.
	if _, err := os.Stat(c.BlobPath(referenced)); err != nil {
		t.Errorf("referenced %s gone: %v", referenced, err)
	}

	// gc_run_complete log line names the right counters.
	logs := buf.String()
	if !strings.Contains(logs, `"msg":"gc_run_complete"`) {
		t.Fatalf("no gc_run_complete event: %s", logs)
	}
	wantOrphans := fmt.Sprintf(`"pool_orphans_repaired":%d`, len(orphans))
	if !strings.Contains(logs, wantOrphans) {
		t.Errorf("logs missing %q: %s", wantOrphans, logs)
	}
	wantBytes := fmt.Sprintf(`"pool_orphan_bytes_repaired":%d`, totalOrphanBytes)
	if !strings.Contains(logs, wantBytes) {
		t.Errorf("logs missing %q: %s", wantBytes, logs)
	}
	if !strings.Contains(logs, `"phase":"startup"`) {
		t.Errorf("logs do not name phase=startup: %s", logs)
	}
}

// ---------------------------------------------------------------------------
// SPEC4 §12.2: forensic retention via gc.keep_displaced.
// ---------------------------------------------------------------------------

// TestForensicRetention_KeepDisplacedThree adopts five snapshots in
// sequence on the same suite, then runs GC with keep_displaced=3.
// After the tick, exactly four suite_snapshot rows must remain — the
// 1 current plus the 3 most-recently-displaced, ordered by
// (adopted_at DESC, snapshot_id DESC) per §9.6.3 sub-job B.
//
// Adoptions all happen within the same wall-clock second on a fast
// host, so adopted_at ties are resolved by snapshot_id descending —
// which is monotonically increasing, i.e. ordering by adoption order.
// The expectation is therefore: keep S5 (current), and S4, S3, S2 as
// the displaced 3-most-recent. S1 is reaped.
func TestForensicRetention_KeepDisplacedThree(t *testing.T) {
	c := openTestCache(t)
	logger, _ := captureLogger()
	db := dbOf(t, c)

	const (
		scheme = "http"
		host   = "deb.example"
		suite  = "/dists/noble"
	)

	// Five distinct candidate snapshots, each carrying its own
	// InRelease blob (so the natural key is unique). Members can be
	// empty — forensic retention is about the snapshot rows, not the
	// blobs they pinned. (An empty members list means CommitAdoption's
	// Rule 2 / Rule 3 have nothing to do, simplifying the test.)
	const N = 5
	ids := make([]int64, N)
	for i := 0; i < N; i++ {
		inrel := putPoolBlob(t, c, fmt.Sprintf("InRelease bytes for S%d", i+1))
		ids[i] = adoptSnapshot(t, c, scheme, host, suite, inrel, nil)
	}
	// All five rows are present pre-GC.
	for i, id := range ids {
		if !snapshotRowExists(t, db, id) {
			t.Fatalf("S%d (id=%d) missing pre-GC", i+1, id)
		}
	}

	// Sanity: only the last id is the current_snapshot_id.
	var currentID sql.NullInt64
	if err := db.QueryRow(
		`SELECT current_snapshot_id FROM suite_freshness
		  WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`,
		scheme, host, suite,
	).Scan(&currentID); err != nil {
		t.Fatalf("read current: %v", err)
	}
	if !currentID.Valid || currentID.Int64 != ids[N-1] {
		t.Fatalf("current_snapshot_id = %v, want %d", currentID, ids[N-1])
	}

	g, err := New(Config{
		Cache:               c,
		Logger:              logger,
		Enabled:             true,
		Interval:            time.Hour,
		BatchSize:           100,
		SnapshotBatchSize:   10,
		MaxTickDuration:     time.Minute,
		BlobGrace:           time.Hour,
		KeepDisplaced:       3,
		PoolScanWorkers:     2,
		HeartbeatStaleGrace: 30 * time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := g.runTick(context.Background(), "test-retention")
	if err != nil {
		t.Fatalf("runTick: %v", err)
	}

	// Exactly one displaced reap (S1).
	if res.displacedReaped != 1 {
		t.Errorf("displacedReaped = %d, want 1 (only S1 should reap)", res.displacedReaped)
	}
	// Survivors: S2, S3, S4, S5. S1 reaped.
	if snapshotRowExists(t, db, ids[0]) {
		t.Errorf("S1 (id=%d) survived; should be reaped", ids[0])
	}
	for i := 1; i < N; i++ {
		if !snapshotRowExists(t, db, ids[i]) {
			t.Errorf("S%d (id=%d) missing; should survive", i+1, ids[i])
		}
	}

	// Total row count: exactly 4 (1 current + 3 displaced).
	var total int
	if err := db.QueryRow(`SELECT count(*) FROM suite_snapshot`).Scan(&total); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	if total != 4 {
		t.Errorf("total suite_snapshot rows = %d, want 4 (1 current + 3 displaced)", total)
	}
}

// TestForensicRetention_KeepDisplacedZero_ReapsAllDisplaced drives the
// keep_displaced=0 boundary: every non-current snapshot is reapable.
// After three adoptions and one GC tick, only the current snapshot
// remains.
func TestForensicRetention_KeepDisplacedZero_ReapsAllDisplaced(t *testing.T) {
	c := openTestCache(t)
	logger, _ := captureLogger()
	db := dbOf(t, c)

	const (
		scheme = "http"
		host   = "deb.example"
		suite  = "/dists/noble"
	)

	const N = 3
	ids := make([]int64, N)
	for i := 0; i < N; i++ {
		inrel := putPoolBlob(t, c, fmt.Sprintf("InRelease %d", i))
		ids[i] = adoptSnapshot(t, c, scheme, host, suite, inrel, nil)
	}

	g, err := New(Config{
		Cache:               c,
		Logger:              logger,
		Enabled:             true,
		Interval:            time.Hour,
		BatchSize:           100,
		SnapshotBatchSize:   10,
		MaxTickDuration:     time.Minute,
		BlobGrace:           time.Hour,
		KeepDisplaced:       0,
		PoolScanWorkers:     2,
		HeartbeatStaleGrace: 30 * time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := g.runTick(context.Background(), "test-keep-zero")
	if err != nil {
		t.Fatalf("runTick: %v", err)
	}
	// Two displaced (S1, S2); current S3 must survive.
	if res.displacedReaped != 2 {
		t.Errorf("displacedReaped = %d, want 2", res.displacedReaped)
	}
	for i := 0; i < N-1; i++ {
		if snapshotRowExists(t, db, ids[i]) {
			t.Errorf("S%d (id=%d) survived under keep_displaced=0", i+1, ids[i])
		}
	}
	if !snapshotRowExists(t, db, ids[N-1]) {
		t.Errorf("current S%d (id=%d) was reaped", N, ids[N-1])
	}
}
