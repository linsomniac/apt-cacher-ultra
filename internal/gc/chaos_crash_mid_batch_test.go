package gc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// SPEC4 §12.6: crash mid-batch chaos test.
//
// Property: a process killed between RunBlobGCBatch's COMMIT and the
// post-COMMIT os.Remove unlinks leaves pool/<hash[:2]>/<hash> orphan
// — the DB has no row referencing the file, but the file is still on
// disk. The next startup's §9.6.4 pool/ orphan scan walks the
// directory, asks the DB whether each file's hash is referenced, and
// unlinks any file whose row is gone.
//
// Driver: seed N reapable blobs (refcount=0, refcount_zeroed_at=1)
// plus M referenced blobs (refcount=0 fresh, zeroed_at=now within
// grace), call c.RunBlobGCBatch directly so the DB COMMIT lands, and
// DO NOT call the post-COMMIT unlink loop in runBlobPass — that
// omission is the simulated crash boundary. Then run StartupPass,
// whose first step is the §9.6.4 pool scan.
//
// Asserts after StartupPass:
//   - reaped pool files unlinked
//   - referenced pool files survive
//   - gc_run_complete startup line names pool_orphans_repaired == N
//     and pool_orphan_bytes_repaired == sum(reaped sizes)
//   - on-disk pool file count equals blob row count (the spec text's
//     "pool size on disk after restart equals what's in the blob
//     table" property)
//
// 10-consecutive-runs gate per the Phase 4 chaos pattern.

const crashMidBatchRuns = 10

func TestChaos_CrashMidBatch_PoolOrphans_RepairedOnRestart(t *testing.T) {
	for run := 0; run < crashMidBatchRuns; run++ {
		t.Run(fmt.Sprintf("run-%02d", run), func(t *testing.T) {
			c := openTestCache(t)
			ctx := context.Background()
			db := dbOf(t, c)

			// Seed 3 reapable blobs (refcount=0/zeroed_at=1, well past
			// any positive grace) and 2 referenced blobs (refcount=0/
			// zeroed_at=now, kept by BlobGrace=1h). Bodies vary by run
			// and slot so each blob has a distinct hash.
			const (
				nReap = 3
				nKeep = 2
			)
			reaped := make([]string, 0, nReap)
			for i := 0; i < nReap; i++ {
				reaped = append(reaped,
					seedReapableBlob(t, c, fmt.Sprintf("V6 reap run-%02d slot-%d", run, i)))
			}
			kept := make([]string, 0, nKeep)
			for i := 0; i < nKeep; i++ {
				// putPoolBlob → NewTempBlob/Finalize/PutBlob lands at
				// refcount=0, zeroed_at=now via §7.5.1 Rule 1. With
				// BlobGrace=1h on the StartupPass tick, the predicate
				// `zeroed_at < now - 3600` is false; these survive.
				kept = append(kept,
					putPoolBlob(t, c, fmt.Sprintf("V6 keep run-%02d slot-%d AAAAA", run, i)))
			}

			// Sanity: on-disk and in-DB counts agree pre-crash.
			if pre := countPoolFiles(t, filepath.Join(c.Dir(), "pool")); pre != nReap+nKeep {
				t.Fatalf("pre-crash pool file count = %d, want %d", pre, nReap+nKeep)
			}

			// Step 1: simulate mid-batch crash. Call RunBlobGCBatch
			// directly — the DB tx COMMITs and the function returns
			// the (hash, size) tuples that would have been unlinked.
			// We DO NOT invoke runBlobPass's post-COMMIT os.Remove
			// loop; that omission is the SPEC4 §9.6.2 crash point.
			got, err := c.RunBlobGCBatch(ctx, 100, 1)
			if err != nil {
				t.Fatalf("RunBlobGCBatch: %v", err)
			}
			if len(got) != nReap {
				t.Fatalf("RunBlobGCBatch reaped %d, want %d", len(got), nReap)
			}

			// Step 2: documented benign split state — reaped blob rows
			// gone, but their pool files are still on disk.
			for _, h := range reaped {
				if blobRowExists(t, db, h) {
					t.Fatalf("post-COMMIT split: blob row %s still in DB", h)
				}
				if _, err := os.Stat(filepath.Join(c.Dir(), "pool", h[:2], h)); err != nil {
					t.Fatalf("post-COMMIT split: pool file %s missing: %v", h, err)
				}
			}
			// Referenced rows + files unchanged.
			for _, h := range kept {
				if !blobRowExists(t, db, h) {
					t.Fatalf("kept blob row %s missing pre-restart", h)
				}
				if _, err := os.Stat(filepath.Join(c.Dir(), "pool", h[:2], h)); err != nil {
					t.Fatalf("kept pool file missing pre-restart: %v", err)
				}
			}

			// Step 3: simulate next-process startup via StartupPass.
			// Order: runPoolScan first (§4.2 step 5), then a one-shot
			// runTick (§4.2 step 6). The pool scan should find the
			// nReap orphan files and unlink them.
			logger, buf := captureLogger()
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
			if err := g.StartupPass(ctx); err != nil {
				t.Fatalf("StartupPass: %v", err)
			}

			// Step 4 invariants.
			//
			// 4a — orphan pool files are unlinked.
			for _, h := range reaped {
				if _, err := os.Stat(filepath.Join(c.Dir(), "pool", h[:2], h)); !os.IsNotExist(err) {
					t.Errorf("orphan %s not repaired (stat err=%v)", h, err)
				}
			}
			// 4b — referenced files survive.
			for _, h := range kept {
				if _, err := os.Stat(filepath.Join(c.Dir(), "pool", h[:2], h)); err != nil {
					t.Errorf("referenced %s gone after StartupPass: %v", h, err)
				}
			}

			// 4c — gc_run_complete startup line counters.
			logs := buf.String()
			if !strings.Contains(logs, `"msg":"gc_run_complete"`) {
				t.Fatalf("no gc_run_complete event: %s", logs)
			}
			if !strings.Contains(logs, `"phase":"startup"`) {
				t.Errorf("logs do not name phase=startup: %s", logs)
			}
			wantOrphans := fmt.Sprintf(`"pool_orphans_repaired":%d`, nReap)
			if !strings.Contains(logs, wantOrphans) {
				t.Errorf("logs missing %q: %s", wantOrphans, logs)
			}
			var wantBytes int64
			for _, r := range got {
				wantBytes += r.Size
			}
			wantBytesField := fmt.Sprintf(`"pool_orphan_bytes_repaired":%d`, wantBytes)
			if !strings.Contains(logs, wantBytesField) {
				t.Errorf("logs missing %q: %s", wantBytesField, logs)
			}

			// 4d — pool size on disk equals blob table row count
			// (SPEC4 §12.6 driver assertion).
			poolCount := countPoolFiles(t, filepath.Join(c.Dir(), "pool"))
			var rowCount int
			if err := db.QueryRow(`SELECT count(*) FROM blob`).Scan(&rowCount); err != nil {
				t.Fatalf("count blob rows: %v", err)
			}
			if poolCount != rowCount {
				t.Errorf("post-restart pool file count = %d, blob row count = %d",
					poolCount, rowCount)
			}
			if rowCount != nKeep {
				t.Errorf("post-restart blob row count = %d, want %d (only kept blobs)",
					rowCount, nKeep)
			}
		})
	}
}

// countPoolFiles walks pool/ recursively and returns the number of
// regular files. Used by §12.6 to verify the on-disk-vs-DB invariant.
func countPoolFiles(t *testing.T, poolRoot string) int {
	t.Helper()
	var count int
	err := filepath.Walk(poolRoot, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", poolRoot, err)
	}
	return count
}
