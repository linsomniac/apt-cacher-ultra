package gc

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// captureLogger returns a *slog.Logger writing JSON to a buffer that
// tests inspect for SPEC4-named events (gc_pool_misplaced_file,
// gc_tick_deadline_reached, etc.). Returns the logger and the buffer
// pointer so callers can assert on records after the work completes.
func captureLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), buf
}

// openTestCache opens a fresh cache.Cache rooted at t.TempDir(), with
// the cache package's nowUnix unstubbed (tests that need it use the
// package-private hooks via cache_test.go directly when in-package).
// Here in the gc package we cannot stub cache.nowUnix; tests that need
// to fast-forward refcount_zeroed_at or heartbeat_at write directly to
// the DB.
func openTestCache(t *testing.T) *cache.Cache {
	t.Helper()
	dir := t.TempDir()
	c, err := cache.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// writePoolFile writes content to pool/<prefix>/<filename> under the
// cache directory. Bypasses cache.NewTempBlob/Finalize so tests can plant
// arbitrary names in arbitrary prefix dirs, including misplaced files.
func writePoolFile(t *testing.T, c *cache.Cache, prefix, filename, content string) string {
	t.Helper()
	dir := filepath.Join(c.Dir(), "pool", prefix)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	full := filepath.Join(dir, filename)
	if err := os.WriteFile(full, []byte(content), 0o640); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
	return full
}

// fileExists reports whether path exists (and is a regular file).
func fileExists(t *testing.T, path string) bool {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	return st.Mode().IsRegular()
}

// ---------------------------------------------------------------------------
// SPEC4 §9.6.4: pool prefix-mismatch detection.
// ---------------------------------------------------------------------------

// TestPoolScan_PrefixMismatch_LogsAndPreservesFile: a file at
// pool/00/<hash starts with "ab"...> emits gc_pool_misplaced_file with
// expected_prefix=ab, actual_prefix=00; the file is NOT unlinked; any
// blob row for that hash is left alone.
func TestPoolScan_PrefixMismatch_LogsAndPreservesFile(t *testing.T) {
	c := openTestCache(t)
	logger, buf := captureLogger()

	// 64-char hex hash starting with "ab".
	misplacedHash := "ab" + strings.Repeat("c", 62)
	planted := writePoolFile(t, c, "00", misplacedHash, "misplaced content")

	// No blob row exists for this hash — would normally be reaped as an
	// orphan, but the prefix-mismatch check must catch it first.
	g, err := New(Config{
		Cache:               c,
		Logger:              logger,
		Enabled:             true,
		Interval:            time.Hour,
		BatchSize:           100,
		SnapshotBatchSize:   10,
		MaxTickDuration:     time.Minute,
		BlobGrace:           5 * time.Minute,
		KeepDisplaced:       3,
		PoolScanWorkers:     2,
		HeartbeatStaleGrace: 30 * time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := g.runPoolScan(context.Background()); err != nil {
		t.Fatalf("runPoolScan: %v", err)
	}

	// File must still be on disk.
	if !fileExists(t, planted) {
		t.Errorf("misplaced file %s was unlinked; SPEC4 §9.6.4 requires preservation", planted)
	}

	// Log line must have the right shape.
	logs := buf.String()
	if !strings.Contains(logs, `"msg":"gc_pool_misplaced_file"`) {
		t.Errorf("logs do not contain gc_pool_misplaced_file: %s", logs)
	}
	if !strings.Contains(logs, `"expected_prefix":"ab"`) {
		t.Errorf("logs do not name expected_prefix=ab: %s", logs)
	}
	if !strings.Contains(logs, `"actual_prefix":"00"`) {
		t.Errorf("logs do not name actual_prefix=00: %s", logs)
	}
}

// TestPoolScan_PrefixMismatch_DoesNotInteractWithBlobRow: even if a blob
// row exists for the misplaced hash, the prefix check fires first and the
// row remains untouched.
func TestPoolScan_PrefixMismatch_DoesNotInteractWithBlobRow(t *testing.T) {
	c := openTestCache(t)
	logger, _ := captureLogger()

	// Seed a real blob via the cache API so a row exists; capture its
	// hash. Then plant a duplicate file at the WRONG prefix.
	w, err := c.NewTempBlob()
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("blob row + misplaced duplicate")
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	hash, err := w.Finalize(int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.PutBlob(context.Background(), hash, int64(len(body))); err != nil {
		t.Fatal(err)
	}

	// Plant a misplaced duplicate at pool/zz/... — wait, "zz" isn't valid
	// hex; the prefix dir name doesn't have to match a hex prefix the
	// scanner cares about, only the FIRST TWO CHARS OF the filename
	// versus the parent dir. Use "00" as the wrong prefix dir.
	wrongPrefix := "00"
	if hash[:2] == wrongPrefix {
		wrongPrefix = "ff"
	}
	planted := writePoolFile(t, c, wrongPrefix, hash, "duplicate")

	g, err := New(Config{
		Cache:               c,
		Logger:              logger,
		Enabled:             true,
		Interval:            time.Hour,
		BatchSize:           100,
		SnapshotBatchSize:   10,
		MaxTickDuration:     time.Minute,
		BlobGrace:           5 * time.Minute,
		KeepDisplaced:       3,
		PoolScanWorkers:     2,
		HeartbeatStaleGrace: 30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.runPoolScan(context.Background()); err != nil {
		t.Fatalf("runPoolScan: %v", err)
	}

	// Misplaced file untouched.
	if !fileExists(t, planted) {
		t.Errorf("misplaced duplicate at %s should be preserved", planted)
	}
	// Original blob row still present.
	if _, err := c.GetBlob(context.Background(), hash); err != nil {
		t.Errorf("blob row gone: %v", err)
	}
	// And the canonical pool file still present.
	if _, err := os.Stat(c.BlobPath(hash)); err != nil {
		t.Errorf("canonical pool file vanished: %v", err)
	}
}

// TestPoolScan_OrphanFile_Reaped: a pool file at the correct prefix with
// no matching blob row is unlinked, and orphans_repaired counter advances.
func TestPoolScan_OrphanFile_Reaped(t *testing.T) {
	c := openTestCache(t)
	logger, _ := captureLogger()

	// Plant a file at pool/<correct-prefix>/<unknown hash>.
	hash := "ff" + strings.Repeat("0", 62)
	planted := writePoolFile(t, c, "ff", hash, "orphan body")

	g, err := New(Config{
		Cache:               c,
		Logger:              logger,
		Enabled:             true,
		Interval:            time.Hour,
		BatchSize:           100,
		SnapshotBatchSize:   10,
		MaxTickDuration:     time.Minute,
		BlobGrace:           5 * time.Minute,
		KeepDisplaced:       3,
		PoolScanWorkers:     2,
		HeartbeatStaleGrace: 30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := g.runPoolScan(context.Background())
	if err != nil {
		t.Fatalf("runPoolScan: %v", err)
	}

	if fileExists(t, planted) {
		t.Errorf("orphan %s should be unlinked", planted)
	}
	if res.orphansRepaired != 1 {
		t.Errorf("orphansRepaired = %d, want 1", res.orphansRepaired)
	}
	if res.orphanBytesRepaired != int64(len("orphan body")) {
		t.Errorf("orphanBytesRepaired = %d, want %d", res.orphanBytesRepaired, len("orphan body"))
	}
}

// TestPoolScan_MalformedFilename_Skipped: a pool file with a non-hex
// filename emits gc_pool_malformed_name and is left in place.
func TestPoolScan_MalformedFilename_Skipped(t *testing.T) {
	c := openTestCache(t)
	logger, buf := captureLogger()

	planted := writePoolFile(t, c, "ab", "not-a-hash", "junk")

	g, err := New(Config{
		Cache:               c,
		Logger:              logger,
		Enabled:             true,
		Interval:            time.Hour,
		BatchSize:           100,
		SnapshotBatchSize:   10,
		MaxTickDuration:     time.Minute,
		BlobGrace:           5 * time.Minute,
		KeepDisplaced:       3,
		PoolScanWorkers:     2,
		HeartbeatStaleGrace: 30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.runPoolScan(context.Background()); err != nil {
		t.Fatalf("runPoolScan: %v", err)
	}

	if !fileExists(t, planted) {
		t.Errorf("malformed-name file %s was unlinked; should be preserved", planted)
	}
	if !strings.Contains(buf.String(), `"msg":"gc_pool_malformed_name"`) {
		t.Errorf("logs do not contain gc_pool_malformed_name: %s", buf.String())
	}
}

// ---------------------------------------------------------------------------
// SPEC4 §9.6.2 / §9.6.3: per-tick deadline.
// ---------------------------------------------------------------------------

// seedReapableBlob seeds a blob row that satisfies the §9.6.2 reap
// predicate: refcount=0, refcount_zeroed_at far in the past so any
// reasonable grace is satisfied. Returns the hash.
func seedReapableBlob(t *testing.T, c *cache.Cache, content string) string {
	t.Helper()
	w, err := c.NewTempBlob()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(w, bytes.NewReader([]byte(content))); err != nil {
		t.Fatal(err)
	}
	hash, err := w.Finalize(int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.PutBlob(context.Background(), hash, int64(len(content))); err != nil {
		t.Fatal(err)
	}
	// Force refcount=0 (already) with refcount_zeroed_at far enough in
	// the past to satisfy any test grace.
	dbHandle := dbOf(t, c)
	if _, err := dbHandle.Exec(
		`UPDATE blob SET refcount = 0, refcount_zeroed_at = 1 WHERE hash = ?`,
		hash); err != nil {
		t.Fatal(err)
	}
	return hash
}

// dbOf reaches into the cache to expose its *sql.DB for direct tests.
// We cannot reach c.db (lowercase, package-private) from outside; tests
// that need it open their own *sql.DB on the cache file. Return that.
func dbOf(t *testing.T, c *cache.Cache) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(c.Dir(), "cache.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestRunBlobPass_DeadlineReached: with MaxTickDuration=0 (already
// expired by the first per-batch check), the blob pass returns
// deadline=true after zero batches and emits gc_tick_deadline_reached.
func TestRunBlobPass_DeadlineReached_EmitsEvent(t *testing.T) {
	c := openTestCache(t)
	logger, buf := captureLogger()

	// Seed enough blobs that, absent a deadline, multiple batches would
	// run. Here MaxTickDuration is 1ns and we sleep briefly so the
	// per-batch pre-check returns deadlineHit immediately.
	for i := 0; i < 5; i++ {
		seedReapableBlob(t, c, "deadline blob "+strings.Repeat("X", i+1))
	}

	g, err := New(Config{
		Cache:               c,
		Logger:              logger,
		Enabled:             true,
		Interval:            time.Hour,
		BatchSize:           2,
		SnapshotBatchSize:   10,
		MaxTickDuration:     time.Nanosecond,
		BlobGrace:           1 * time.Second,
		KeepDisplaced:       3,
		PoolScanWorkers:     2,
		HeartbeatStaleGrace: 30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Pass an already-elapsed deadline; the runBlobPass first thing in
	// the loop is a deadline check.
	deadline := time.Now().Add(-1 * time.Hour)
	res, hit, _, err := g.runBlobPass(context.Background(), deadline, "test")
	if err != nil {
		t.Fatalf("runBlobPass: %v", err)
	}
	if !hit {
		t.Errorf("expected deadlineHit=true, got false")
	}
	if res.count != 0 {
		t.Errorf("count = %d, want 0 (deadline before first batch)", res.count)
	}

	if !strings.Contains(buf.String(), `"msg":"gc_tick_deadline_reached"`) {
		t.Errorf("logs do not contain gc_tick_deadline_reached: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"which":"blob"`) {
		t.Errorf(`logs do not name which="blob": %s`, buf.String())
	}
}

// TestRunBlobPass_DrainAcrossBatches_NoDeadline: with a generous
// deadline and BatchSize=2 and 5 reapable blobs, the loop runs 3
// batches and reports total=5.
func TestRunBlobPass_DrainAcrossBatches_NoDeadline(t *testing.T) {
	c := openTestCache(t)
	logger, _ := captureLogger()

	const N = 5
	hashes := make([]string, N)
	for i := 0; i < N; i++ {
		hashes[i] = seedReapableBlob(t, c, "drain "+strings.Repeat("X", i+1))
	}

	g, err := New(Config{
		Cache:               c,
		Logger:              logger,
		Enabled:             true,
		Interval:            time.Hour,
		BatchSize:           2,
		SnapshotBatchSize:   10,
		MaxTickDuration:     time.Minute,
		BlobGrace:           1 * time.Second,
		KeepDisplaced:       3,
		PoolScanWorkers:     2,
		HeartbeatStaleGrace: 30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Minute)
	res, hit, _, err := g.runBlobPass(context.Background(), deadline, "test")
	if err != nil {
		t.Fatalf("runBlobPass: %v", err)
	}
	if hit {
		t.Errorf("deadline tripped unexpectedly")
	}
	if res.count != N {
		t.Errorf("count = %d, want %d", res.count, N)
	}
	// All blob rows gone.
	for _, h := range hashes {
		if _, err := c.GetBlob(context.Background(), h); err == nil {
			t.Errorf("blob %s still present", h)
		}
	}
}

// TestRunBlobPass_UnlinkFailure_StillTalliesReap: SPEC4 §10.2 names
// `blobs_reaped` as "blob rows DELETEd this run" — independent of the
// post-COMMIT unlink result. A non-ENOENT unlink failure leaves a
// pool/ file leaked, which `pool_unlink_errors` reports separately;
// the row was DELETEd in the writer-tx COMMIT and `blobs_reaped` /
// `bytes_reclaimed` must reflect that. Engineered failure mode:
// replace the pool file with a non-empty directory at the same path
// AFTER seeding so os.Remove returns ENOTEMPTY, which works for any
// uid (root included) since ENOTEMPTY is enforced by the syscall
// itself, not DAC.
func TestRunBlobPass_UnlinkFailure_StillTalliesReap(t *testing.T) {
	c := openTestCache(t)
	logger, buf := captureLogger()

	const body = "blob whose unlink will fail"
	hash := seedReapableBlob(t, c, body)

	// Replace pool/<prefix>/<hash> file with a non-empty directory at
	// the same path. After this, os.Remove(pool/<prefix>/<hash>)
	// returns ENOTEMPTY because the entry is a directory containing
	// blocker.
	poolPath := filepath.Join(c.Dir(), "pool", hash[:2], hash)
	if err := os.Remove(poolPath); err != nil {
		t.Fatalf("remove pool file: %v", err)
	}
	if err := os.Mkdir(poolPath, 0o755); err != nil {
		t.Fatalf("mkdir at pool path: %v", err)
	}
	if err := os.WriteFile(filepath.Join(poolPath, "blocker"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	g, err := New(Config{
		Cache:               c,
		Logger:              logger,
		Enabled:             true,
		Interval:            time.Hour,
		BatchSize:           10,
		SnapshotBatchSize:   10,
		MaxTickDuration:     time.Minute,
		BlobGrace:           1 * time.Second,
		KeepDisplaced:       3,
		PoolScanWorkers:     2,
		HeartbeatStaleGrace: 30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Minute)
	res, _, unlinkErrors, err := g.runBlobPass(context.Background(), deadline, "test")
	if err != nil {
		t.Fatalf("runBlobPass: %v", err)
	}

	// The row WAS deleted by the COMMIT — count and bytes reflect that.
	if res.count != 1 {
		t.Errorf("res.count = %d, want 1 (row was DELETEd)", res.count)
	}
	if res.bytes != int64(len(body)) {
		t.Errorf("res.bytes = %d, want %d", res.bytes, len(body))
	}
	if unlinkErrors != 1 {
		t.Errorf("unlinkErrors = %d, want 1", unlinkErrors)
	}
	logs := buf.String()
	if !strings.Contains(logs, `"msg":"gc_pool_unlink_failed"`) {
		t.Errorf(`logs do not contain gc_pool_unlink_failed: %s`, logs)
	}
}

// TestRunSnapshotPass_DeadlineReached: an already-elapsed deadline
// produces deadlineHit=true with no batches run.
func TestRunSnapshotPass_DeadlineReached_EmitsEvent(t *testing.T) {
	c := openTestCache(t)
	logger, buf := captureLogger()

	g, err := New(Config{
		Cache:               c,
		Logger:              logger,
		Enabled:             true,
		Interval:            time.Hour,
		BatchSize:           100,
		SnapshotBatchSize:   2,
		MaxTickDuration:     time.Nanosecond,
		BlobGrace:           1 * time.Second,
		KeepDisplaced:       3,
		PoolScanWorkers:     2,
		HeartbeatStaleGrace: 30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(-1 * time.Hour)
	_, hit, err := g.runSnapshotPass(context.Background(), deadline, "test")
	if err != nil {
		t.Fatalf("runSnapshotPass: %v", err)
	}
	if !hit {
		t.Errorf("expected deadlineHit=true, got false")
	}
	if !strings.Contains(buf.String(), `"msg":"gc_tick_deadline_reached"`) {
		t.Errorf("logs do not contain gc_tick_deadline_reached: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"which":"snapshot"`) {
		t.Errorf(`logs do not name which="snapshot": %s`, buf.String())
	}
}

// TestRunTick_SnapshotDeadlineCascadesToBlobPass: §9.6.1 spec —
// snapshot pass first; if it exhausts the deadline, the blob pass runs
// against an already-expired deadline and exits with zero batches.
func TestRunTick_SnapshotDeadlineCascadesToBlobPass(t *testing.T) {
	c := openTestCache(t)
	logger, buf := captureLogger()

	// Seed a reapable blob; a healthy tick would normally reap it, but
	// the cascade makes the blob pass return immediately.
	seedReapableBlob(t, c, "cascade test blob")

	g, err := New(Config{
		Cache:               c,
		Logger:              logger,
		Enabled:             true,
		Interval:            time.Hour,
		BatchSize:           100,
		SnapshotBatchSize:   10,
		MaxTickDuration:     time.Nanosecond, // expires before snapshot pass first batch
		BlobGrace:           1 * time.Second,
		KeepDisplaced:       3,
		PoolScanWorkers:     2,
		HeartbeatStaleGrace: 30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := g.runTick(context.Background(), "test-cascade")
	if err != nil {
		t.Fatalf("runTick: %v", err)
	}
	if !res.deadlineReached {
		t.Errorf("deadlineReached = false, want true")
	}
	if res.blobsReaped != 0 {
		t.Errorf("blobsReaped = %d, want 0 (cascade should have starved blob pass)", res.blobsReaped)
	}

	// Both snapshot and blob deadline events should fire — but if the
	// snapshot pass returns the moment its first deadline check trips,
	// the blob pass also gets a deadline check before any work, so we
	// expect at least the "blob" deadline event (and likely "snapshot"
	// too).
	logs := buf.String()
	if !strings.Contains(logs, `"which":"blob"`) {
		t.Errorf(`expected which="blob" deadline event: %s`, logs)
	}
}

// ---------------------------------------------------------------------------
// SPEC4 §9.6.2: integration-level reachability exclusion via the
// gc.RunBlobGCBatch wrapper. This is a pure reachability test — the
// real SELECT-then-mutate-then-DELETE race needs a writer-tx
// interleaving seam (deferred to §12.3 chaos tests, where a controlled
// hook between SELECT and DELETE inside a single batch can mutate
// reachability and prove the DELETE re-application excludes the
// survivor). Here we only verify that pre-placed FK references prevent
// reaping — exercising the SELECT predicate.
// ---------------------------------------------------------------------------

// TestRunBlobGCBatch_ExcludesUnreachableBlobs: a row that LOOKS reapable
// on its own (refcount=0, eligible clock) but is referenced by a
// snapshot_member or suite_snapshot FK must be excluded. Cross-cuts
// the per-FK goldens in cache/gc_test.go to verify the gc-package
// wrapper passes through the exclusion semantics.
func TestRunBlobGCBatch_ExcludesUnreachableBlobs(t *testing.T) {
	c := openTestCache(t)
	ctx := context.Background()

	// Reapable: row with no FK references.
	reapable := seedReapableBlob(t, c, "reap-this")

	// Survivor: row that LOOKS reapable on its own (refcount=0, eligible
	// clock) but is referenced by a snapshot_member. The cache.RunBlobGCBatch
	// SELECT predicate already filters via NOT EXISTS, so we plant the FK
	// and verify the SELECT doesn't surface it.
	survivor := seedReapableBlob(t, c, "should-survive")
	relBlob := seedReapableBlob(t, c, "release blob for survivor test")
	// Don't force survivor unreachable via Rule 2 yet — instead, build a
	// snapshot pinning it as a member.
	dbHandle := dbOf(t, c)
	res, err := dbHandle.Exec(`
INSERT INTO suite_snapshot
  (canonical_scheme, canonical_host, suite_path,
   inrelease_hash, created_at, adopted_at, package_coverage_complete, heartbeat_at)
VALUES ('http', 'race.example', '/p', ?, 1, 1, 0, 1)`, relBlob)
	if err != nil {
		t.Fatal(err)
	}
	snapID, _ := res.LastInsertId()
	if _, err := dbHandle.Exec(`
INSERT INTO snapshot_member (snapshot_id, path, blob_hash, declared_sha256)
VALUES (?, 'M', ?, ?)`, snapID, survivor, survivor); err != nil {
		t.Fatal(err)
	}
	// Same for relBlob — pin it via inrelease_hash already in the
	// suite_snapshot row above; that's already in place.

	got, err := c.RunBlobGCBatch(ctx, 100, 1)
	if err != nil {
		t.Fatalf("RunBlobGCBatch: %v", err)
	}
	for _, b := range got {
		if b.Hash == survivor {
			t.Errorf("survivor %s reaped despite snapshot_member FK", survivor)
		}
		if b.Hash == relBlob {
			t.Errorf("relBlob %s reaped despite suite_snapshot.inrelease_hash FK", relBlob)
		}
	}
	// reapable is unprotected → must be in the reaped set.
	found := false
	for _, b := range got {
		if b.Hash == reapable {
			found = true
		}
	}
	if !found {
		t.Errorf("expected %s to be reaped; reaped set: %+v", reapable, got)
	}
}

// ---------------------------------------------------------------------------
// SPEC4 §4.3.2: GC.New validates Config.
// ---------------------------------------------------------------------------

func TestNew_RejectsInvalidConfig(t *testing.T) {
	logger, _ := captureLogger()
	c := openTestCache(t)
	base := Config{
		Cache:               c,
		Logger:              logger,
		Enabled:             true,
		Interval:            time.Hour,
		BatchSize:           100,
		SnapshotBatchSize:   10,
		MaxTickDuration:     time.Minute,
		BlobGrace:           5 * time.Minute,
		KeepDisplaced:       3,
		PoolScanWorkers:     4,
		HeartbeatStaleGrace: 30 * time.Minute,
	}
	cases := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"interval", func(c *Config) { c.Interval = 0 }, "Interval"},
		{"batch_size", func(c *Config) { c.BatchSize = 0 }, "BatchSize"},
		{"snapshot_batch_size", func(c *Config) { c.SnapshotBatchSize = 0 }, "SnapshotBatchSize"},
		{"max_tick", func(c *Config) { c.MaxTickDuration = 0 }, "MaxTickDuration"},
		{"blob_grace", func(c *Config) { c.BlobGrace = 0 }, "BlobGrace"},
		{"keep_displaced", func(c *Config) { c.KeepDisplaced = -1 }, "KeepDisplaced"},
		{"pool_workers", func(c *Config) { c.PoolScanWorkers = 0 }, "PoolScanWorkers"},
		{"heartbeat_grace", func(c *Config) { c.HeartbeatStaleGrace = 0 }, "HeartbeatStaleGrace"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mut(&cfg)
			_, err := New(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want error naming %q", err, tc.want)
			}
		})
	}
}

func TestNew_DisabledShortCircuits(t *testing.T) {
	logger, _ := captureLogger()
	c := openTestCache(t)
	g, err := New(Config{Cache: c, Logger: logger, Enabled: false})
	if err != nil {
		t.Errorf("disabled config rejected: %v", err)
	}
	if err := g.StartupPass(context.Background()); err != nil {
		t.Errorf("disabled StartupPass: %v", err)
	}
	// Run() should return immediately.
	done := make(chan struct{})
	var ran atomic.Bool
	go func() {
		g.Run(context.Background())
		ran.Store(true)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Errorf("disabled Run did not return quickly")
	}
}
