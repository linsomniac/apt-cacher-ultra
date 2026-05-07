package cache

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
)

// SPEC4 §12.3: GC + adoption race chaos test (the gate).
//
// Property: a blob whose refcount or FK-reachability changes during
// an in-flight adoption is never reaped if the change makes it
// reachable, even if the change arrives between GC's SELECT and GC's
// DELETE. The §9.6.2 DELETE's full WHERE predicate (refcount + 3 NOT
// EXISTS clauses) is the gate. The §7.5.1 Rule 1 ON CONFLICT DO
// UPDATE is the gate against the PutBlob/FK-INSERT race.
//
// Test seam: the package-private blobGCInterTxHook fires between
// RunBlobGCBatch's SELECT and DELETE. Mutations performed inside the
// hook run on the same writer-tx connection — semantically identical
// to a parallel writer's commit landing between SELECT and DELETE in
// a hypothetical multi-writer architecture. Under the real
// single-writer model the predicate is dead-code defense-in-depth;
// the chaos test makes the defense exercisable.
//
// 10-consecutive-runs gate matches the Phase 2 / Phase 3 chaos test
// pattern: each variant is wrapped in a per-run subtest so a flaky
// iteration surfaces clearly in test output.

const (
	// chaosRuns is the gate threshold from SPEC4 §12.3 ("same
	// 10-consecutive-runs gate as Phase 2 / Phase 3 chaos tests").
	// Each variant runs this many times within one go-test invocation;
	// any single run failure fails the whole test.
	chaosRuns = 10

	// chaosBlobGrace is large enough that PutBlob's refresh is
	// observable in Variant D (predicate "now - chaosBlobGrace" must
	// remain firmly in the past after the refresh) and small enough
	// that the test driver's fast-forward UPDATE puts us safely past
	// it.
	chaosBlobGrace = int64(60) // seconds
)

// withInterTxHook installs hook for the duration of fn and clears it
// after. Restores any previously-installed hook on return so nested
// usage is safe (though tests should not nest).
func withInterTxHook(t *testing.T, hook func(ctx context.Context, tx *sql.Tx) error, fn func()) {
	t.Helper()
	prev := blobGCInterTxHook
	blobGCInterTxHook = hook
	defer func() { blobGCInterTxHook = prev }()
	fn()
}

// chaosSeedReapableBlob plants a blob row at refcount=0 with
// refcount_zeroed_at far enough in the past that, absent any
// reachability change, it would be reaped by the next GC tick. Also
// places a real file at pool/<prefix>/<hash> so we can assert the
// on-disk side effect.
func chaosSeedReapableBlob(t *testing.T, c *Cache, content string) string {
	t.Helper()
	hash := seedBlob(t, c, content)
	// PutBlob already created a pool file via Finalize. Drive
	// refcount_zeroed_at = 1 (epoch=1, far past) so the predicate
	// "refcount_zeroed_at < now - grace" trivially holds.
	forceBlobState(t, c, hash, 0, sql.NullInt64{Int64: 1, Valid: true})
	return hash
}

// poolFileExists is a tiny helper that returns true iff the
// pool/<prefix>/<hash> file is on disk. Distinct from the gc package
// integration_test.go's helper of the same purpose; lives here so
// chaos tests stay self-contained.
func poolFileExists(t *testing.T, c *Cache, hash string) bool {
	t.Helper()
	st, err := os.Stat(c.BlobPath(hash))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false
		}
		t.Fatalf("stat pool file %s: %v", hash, err)
	}
	return st.Mode().IsRegular()
}

// chaosBlobRowExists is a count-based existence check that can be
// called from the test driver after RunBlobGCBatch returns.
func chaosBlobRowExists(t *testing.T, c *Cache, hash string) bool {
	t.Helper()
	var n int
	if err := c.db.QueryRow(
		`SELECT count(*) FROM blob WHERE hash = ?`, hash,
	).Scan(&n); err != nil {
		t.Fatalf("count blob row for %s: %v", hash, err)
	}
	return n > 0
}

// reapedHashes flattens the reaped-blob slice to a set for
// containment checks.
func reapedHashes(reaped []ReapedBlob) map[string]bool {
	out := make(map[string]bool, len(reaped))
	for _, r := range reaped {
		out[r.Hash] = true
	}
	return out
}

// ---------------------------------------------------------------------------
// Variant A — refcount bump.
// ---------------------------------------------------------------------------

// TestChaos_BlobGC_AdoptionRace_VariantA_RefcountBump.
//
// Setup: blob B at refcount=0, refcount_zeroed_at far past, no FK
// references. GC begins a batch; the SELECT picks B up as a
// candidate; the inter-tx hook bumps B.refcount to 1 (mimicking a
// parallel adoption's §7.5.1 Rule 2 increment); the DELETE's
// refcount<=0 clause filters B out.
//
// Assert: B's row survives, B's pool file survives, B is NOT in the
// RETURNING-buffered result.
func TestChaos_BlobGC_AdoptionRace_VariantA_RefcountBump(t *testing.T) {
	for run := 0; run < chaosRuns; run++ {
		t.Run("", func(t *testing.T) {
			c := openCache(t)
			ctx := context.Background()

			b := chaosSeedReapableBlob(t, c, "variant A blob — refcount bump")

			// Sanity precondition: predicate should reap B if no hook
			// runs. The hook below mutates state to prove the predicate's
			// re-application is what defends B.
			var reaped []ReapedBlob
			withInterTxHook(t, func(ctx context.Context, tx *sql.Tx) error {
				// Bump B.refcount to 1 inline. Mirrors §7.5.1 Rule 2's
				// post-INSERT UPDATE: increment refcount and clear
				// refcount_zeroed_at on the strictly-positive crossing.
				_, err := tx.ExecContext(ctx, `
UPDATE blob
   SET refcount = refcount + 1,
       refcount_zeroed_at = NULL
 WHERE hash = ?`, b)
				return err
			}, func() {
				var err error
				reaped, err = c.RunBlobGCBatch(ctx, 100, chaosBlobGrace)
				if err != nil {
					t.Fatalf("RunBlobGCBatch: %v", err)
				}
			})

			// Invariant: B is NOT in the reap result (predicate's
			// refcount<=0 clause filtered it out).
			if reapedHashes(reaped)[b] {
				t.Fatalf("Variant A: B was reaped despite refcount bump; reaped=%+v", reaped)
			}
			if !chaosBlobRowExists(t, c, b) {
				t.Errorf("Variant A: B's blob row gone")
			}
			if !poolFileExists(t, c, b) {
				t.Errorf("Variant A: B's pool file unlinked")
			}

			// Postcondition: refcount is now 1 (the hook bumped it).
			if rc := blobRefcount(t, c, b); rc != 1 {
				t.Errorf("Variant A: post-state refcount = %d, want 1", rc)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Variant B — url_path insert during the race.
// ---------------------------------------------------------------------------

// TestChaos_BlobGC_AdoptionRace_VariantB_URLPathInsert.
//
// Setup: same as Variant A but the inter-tx mutation inserts a new
// url_path row pointing at B (no refcount bump). Assert: B is excluded
// from the DELETE by the NOT EXISTS (url_path) clause.
func TestChaos_BlobGC_AdoptionRace_VariantB_URLPathInsert(t *testing.T) {
	for run := 0; run < chaosRuns; run++ {
		t.Run("", func(t *testing.T) {
			c := openCache(t)
			ctx := context.Background()

			b := chaosSeedReapableBlob(t, c, "variant B blob — url_path insert")

			var reaped []ReapedBlob
			withInterTxHook(t, func(ctx context.Context, tx *sql.Tx) error {
				// Insert a url_path row pointing at B. Refcount NOT
				// bumped (mirroring the case where the freshness layer
				// caches a metadata path that resolved to a blob already
				// in pool but not yet pinned by any snapshot — pre-Phase
				// 4 this was the canonical orphan-creation path; Phase 4
				// closes it via PutBlob's ON CONFLICT, but the predicate
				// still has to defend if the order goes the other way).
				_, err := tx.ExecContext(ctx, `
INSERT INTO url_path
  (canonical_scheme, canonical_host, path, blob_hash, upstream_url,
   is_metadata, last_requested_at, request_count, last_fetched_at,
   upstream_etag, upstream_lastmod)
VALUES ('http', 'race.example', '/race-B', ?, 'http://race.example/race-B',
        1, NULL, 0, NULL, NULL, NULL)`, b)
				return err
			}, func() {
				var err error
				reaped, err = c.RunBlobGCBatch(ctx, 100, chaosBlobGrace)
				if err != nil {
					t.Fatalf("RunBlobGCBatch: %v", err)
				}
			})

			if reapedHashes(reaped)[b] {
				t.Fatalf("Variant B: B was reaped despite url_path insert; reaped=%+v", reaped)
			}
			if !chaosBlobRowExists(t, c, b) {
				t.Errorf("Variant B: B's blob row gone")
			}
			if !poolFileExists(t, c, b) {
				t.Errorf("Variant B: B's pool file unlinked")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Variant C — adoption aborts.
// ---------------------------------------------------------------------------

// TestChaos_BlobGC_AdoptionRace_VariantC_AdoptionAbort.
//
// Setup: same scaffolding, but the parallel adoption *aborts*. We
// model this by having the inter-tx hook return an error AFTER doing
// nothing — but that would abort the whole batch. The spec's actual
// scenario is "the goroutine cancels before CommitAdoption's commit"
// — i.e. NO mutation lands. A no-op hook is the right model: SELECT
// picked up B, the hook fires (the parallel goroutine ran but
// aborted), the DELETE's predicate is unchanged, B is reaped.
//
// Assert: B is reaped per the normal reap path; B appears in the
// RETURNING-buffered result; the file is unlinked.
func TestChaos_BlobGC_AdoptionRace_VariantC_AdoptionAbort(t *testing.T) {
	for run := 0; run < chaosRuns; run++ {
		t.Run("", func(t *testing.T) {
			c := openCache(t)
			ctx := context.Background()

			b := chaosSeedReapableBlob(t, c, "variant C blob — adoption abort")

			var reaped []ReapedBlob
			withInterTxHook(t, func(ctx context.Context, tx *sql.Tx) error {
				// No-op hook: simulates a parallel adoption that
				// reached the inter-tx point and then was cancelled
				// before its writer-tx committed any mutation. Under
				// single-writer ordering, this is just "the goroutine
				// woke up, decided not to commit, and went away" —
				// no DB state change visible to the DELETE predicate.
				return nil
			}, func() {
				var err error
				reaped, err = c.RunBlobGCBatch(ctx, 100, chaosBlobGrace)
				if err != nil {
					t.Fatalf("RunBlobGCBatch: %v", err)
				}
			})

			if !reapedHashes(reaped)[b] {
				t.Fatalf("Variant C: B not reaped despite no parallel mutation; reaped=%+v", reaped)
			}
			if chaosBlobRowExists(t, c, b) {
				t.Errorf("Variant C: B's blob row should be reaped")
			}
			// Note: RunBlobGCBatch does NOT unlink files (that's the gc
			// package's job after COMMIT). So we do not assert pool
			// file absence here; the file lives until the gc package
			// runs the unlink loop. The DB row's removal is the
			// invariant under test.
		})
	}
}

// ---------------------------------------------------------------------------
// Variant D — orphan-blob reuse via PutBlob conflict.
// ---------------------------------------------------------------------------

// TestChaos_BlobGC_AdoptionRace_VariantD_PutBlobConflict.
//
// Setup walked from §12.3 step 1 onward:
//
//  1. Plant B at refcount=0, refcount_zeroed_at far past — reapable.
//  2. PutBlob(B) — §7.5.1 Rule 1's ON CONFLICT DO UPDATE refreshes
//     refcount_zeroed_at = now.
//  3. GC tick fires before any FK-bearing INSERT commits. Predicate's
//     `refcount_zeroed_at < now - grace` must REJECT B because the
//     refresh moved zeroed_at to now (now < now - grace is false).
//  4. Then a snapshot_member INSERT commits referencing B (Rule 2
//     bumps refcount to 1, clears zeroed_at to NULL).
//  5. Advance the clock past gc.blob_grace; another GC tick. B must
//     STILL be alive: the partial index (refcount<=0) excludes B
//     because its refcount is now 1.
//
// No inter-tx hook needed — this variant tests the §7.5.1 Rule 1
// PutBlob conflict mechanism, not the SELECT/DELETE race.
func TestChaos_BlobGC_AdoptionRace_VariantD_PutBlobConflict(t *testing.T) {
	for run := 0; run < chaosRuns; run++ {
		t.Run("", func(t *testing.T) {
			c := openCache(t)
			ctx := context.Background()

			// Use a fixed nowUnix so we can reason about the predicate
			// boundary deterministically.
			const t0 = int64(1_700_000_000)
			restore := stubNow(t, t0)
			defer restore()

			b := chaosSeedReapableBlob(t, c, "variant D blob — PutBlob conflict")

			// Step 2: PutBlob(B) refreshes refcount_zeroed_at = t0.
			// (size matches the seedBlob's size; PutBlob's INSERT body
			// is overridden by ON CONFLICT DO UPDATE.)
			var size int64
			if err := c.db.QueryRow(`SELECT size FROM blob WHERE hash = ?`, b).Scan(&size); err != nil {
				t.Fatalf("read size: %v", err)
			}
			if err := c.PutBlob(ctx, b, size); err != nil {
				t.Fatalf("PutBlob refresh: %v", err)
			}
			if got := blobZeroedAt(t, c, b); got != t0 {
				t.Fatalf("refcount_zeroed_at after refresh = %d, want %d", got, t0)
			}

			// Step 3: GC tick before any FK-bearing INSERT. Predicate
			// `refcount_zeroed_at < t0 - grace` becomes `t0 < t0 -
			// grace` = false. B must not be reaped.
			reaped, err := c.RunBlobGCBatch(ctx, 100, chaosBlobGrace)
			if err != nil {
				t.Fatalf("RunBlobGCBatch (early): %v", err)
			}
			if reapedHashes(reaped)[b] {
				t.Fatalf("Variant D: B reaped despite refreshed zeroed_at; reaped=%+v", reaped)
			}
			if !chaosBlobRowExists(t, c, b) {
				t.Fatalf("Variant D: B's row gone after early tick")
			}

			// Step 4: snapshot_member INSERT (we model the tail of
			// CommitAdoption: insert a member row + Rule 2 refcount
			// bump in one mini-tx).
			snapshotID := chaosSeedAdoptedSnapshot(t, c, "http", "race.example", "/D", b)
			if _, err := c.db.Exec(
				`INSERT INTO snapshot_member (snapshot_id, path, blob_hash, declared_sha256)
				 VALUES (?, 'M', ?, ?)`, snapshotID, b, b); err != nil {
				t.Fatalf("insert snapshot_member: %v", err)
			}
			// Rule 2: bump refcount, clear zeroed_at.
			if _, err := c.db.Exec(`
UPDATE blob
   SET refcount = refcount + 1,
       refcount_zeroed_at = IIF(refcount + 1 > 0, NULL, refcount_zeroed_at)
 WHERE hash = ?`, b); err != nil {
				t.Fatalf("Rule 2 bump: %v", err)
			}

			// Step 5: advance the clock past the grace and run another
			// tick. B is now refcount=1 / zeroed_at=NULL — the partial
			// index excludes it. Reap result must not contain B.
			restore() // restore real time first
			restore = stubNow(t, t0+chaosBlobGrace*10)
			defer restore()

			reaped2, err := c.RunBlobGCBatch(ctx, 100, chaosBlobGrace)
			if err != nil {
				t.Fatalf("RunBlobGCBatch (late): %v", err)
			}
			if reapedHashes(reaped2)[b] {
				t.Fatalf("Variant D: B reaped after FK insert + grace; reaped=%+v", reaped2)
			}
			if !chaosBlobRowExists(t, c, b) {
				t.Errorf("Variant D: B's row gone after late tick")
			}
			if rc := blobRefcount(t, c, b); rc != 1 {
				t.Errorf("Variant D: refcount = %d, want 1", rc)
			}
			if got := blobZeroedAt(t, c, b); got != -1 {
				t.Errorf("Variant D: zeroed_at = %d, want NULL", got)
			}
		})
	}
}

// chaosSeedAdoptedSnapshot inserts a fully-adopted suite_snapshot row
// for the given (scheme, host, suite) and returns its id. Used by
// Variant D to provide a parent for the post-race snapshot_member
// INSERT.
func chaosSeedAdoptedSnapshot(t *testing.T, c *Cache, scheme, host, suite, inrelease string) int64 {
	t.Helper()
	res, err := c.db.Exec(`
INSERT INTO suite_snapshot
  (canonical_scheme, canonical_host, suite_path,
   inrelease_hash, created_at, adopted_at, package_coverage_complete, heartbeat_at)
VALUES (?, ?, ?, ?, 1, 1, 1, 1)`, scheme, host, suite, inrelease)
	if err != nil {
		t.Fatalf("seed adopted snapshot: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}
	return id
}
