package cache

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// SPEC4 §12.5: GC + concurrent snapshot displacement chaos test.
//
// Two variants exercise the single-writer ordering invariants
// surrounding RunSnapshotGCBatch:
//
//   - Variant 1 (displacement during tick): a snapshot displaced
//     concurrently with a GC tick is observed in exactly one of two
//     final states — survives (GC saw it as current) or reaped (GC
//     saw it as displaced). No torn state. The
//     `current_snapshot_id NOT IN` clause in §9.6.3's SELECT is the
//     guarantee.
//
//   - Variant 2 (heartbeat liveness race): an orphan candidate `S`
//     with stale heartbeat under concurrent HeartbeatSnapshot,
//     CommitAdoption flip, and GC tick converges on one of two final
//     states — `S` survives (heartbeat or adoption won the race) or
//     `S` is reaped (GC ran while `S` was still orphan + stale).
//     Single-writer ordering precludes any in-between state.
//
// Both variants exercise the chaos property by issuing the
// concurrent operations through the cache's writer-goroutine queue,
// which serializes them. The test verifies that across N=10 runs
// per variant, the final observable state always matches one of the
// valid orderings.

// TestChaos_SnapshotGC_DisplacementDuringTick_VariantOne walks
// Variant 1: S1 is current; goroutines concurrently run GC (with
// keep_displaced=0) and CommitAdoption(S2) which would displace S1.
// The §9.6.3 SELECT's `NOT IN current_snapshot_id` clause guarantees
// that whichever op runs first determines whether S1 is reaped.
func TestChaos_SnapshotGC_DisplacementDuringTick_VariantOne(t *testing.T) {
	for run := 0; run < chaosRuns; run++ {
		t.Run(fmt.Sprintf("run-%02d", run), func(t *testing.T) {
			c := openCache(t)
			ctx := context.Background()

			// Each subtest uses unique InRelease bytes so suite_snapshot's
			// natural key (scheme, host, suite_path, COALESCE(inrelease,
			// release)) is distinct across runs in this single openCache.
			r1 := seedBlob(t, c, fmt.Sprintf("V1 r1 run-%02d", run))
			r2 := seedBlob(t, c, fmt.Sprintf("V1 r2 run-%02d", run))

			s1, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
				CanonicalScheme: "http",
				CanonicalHost:   "v1.example",
				SuitePath:       "/p",
				InReleaseHash:   &r1,
			})
			if err != nil {
				t.Fatalf("InsertCandidateSnapshot S1: %v", err)
			}
			if err := c.CommitAdoption(ctx, s1, nil, nil, nil, nil, false); err != nil {
				t.Fatalf("CommitAdoption S1: %v", err)
			}
			s2, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
				CanonicalScheme: "http",
				CanonicalHost:   "v1.example",
				SuitePath:       "/p",
				InReleaseHash:   &r2,
			})
			if err != nil {
				t.Fatalf("InsertCandidateSnapshot S2: %v", err)
			}

			// Concurrent ops: GC tick + CommitAdoption(S2). Both submit
			// through the cache's writer queue, which serializes them.
			var (
				wg        sync.WaitGroup
				gcErr     error
				commitErr error
				gcResult  SnapshotGCBatchResult
			)
			wg.Add(2)
			go func() {
				defer wg.Done()
				gcResult, gcErr = c.RunSnapshotGCBatch(ctx, 100, 30*60, 0)
			}()
			go func() {
				defer wg.Done()
				commitErr = c.CommitAdoption(ctx, s2, nil, nil, nil, nil, false)
			}()
			wg.Wait()

			if gcErr != nil {
				t.Fatalf("RunSnapshotGCBatch: %v", gcErr)
			}
			if commitErr != nil {
				t.Fatalf("CommitAdoption: %v", commitErr)
			}

			// Final state: S2 is always current. S1 is either still
			// present-but-displaced (GC ran before CommitAdoption) or
			// reaped (GC ran after).
			s1Exists := snapshotExists(t, c, s1)
			s2Exists := snapshotExists(t, c, s2)
			if !s2Exists {
				t.Fatalf("S2 (current) was reaped — torn state")
			}

			currentID := readCurrentSnapshotID(t, c, "http", "v1.example", "/p")
			if currentID != s2 {
				t.Errorf("current_snapshot_id = %d, want S2=%d", currentID, s2)
			}

			// Either ordering is valid; check the gcResult matches the
			// final state. If S1 reaped (gcResult.DisplacedReaped >= 1),
			// then s1Exists must be false.
			switch {
			case s1Exists:
				// GC ran before displacement: S1 was current at SELECT
				// time, NOT IN candidate set, not reaped.
				if gcResult.DisplacedReaped > 0 {
					t.Errorf("S1 exists but GC reported displacedReaped=%d — inconsistent",
						gcResult.DisplacedReaped)
				}
				t.Logf("ordering: GC-first (S1 survived as displaced)")
			case !s1Exists:
				// GC ran after displacement: S1 was already displaced
				// by the time SELECT ran, so it WAS in candidate set,
				// reaped via cascade.
				if gcResult.DisplacedReaped != 1 {
					t.Errorf("S1 reaped but GC reported displacedReaped=%d, want 1",
						gcResult.DisplacedReaped)
				}
				t.Logf("ordering: Commit-first (S1 reaped)")
			}
		})
	}
}

// TestChaos_SnapshotGC_HeartbeatLivenessRace_VariantTwo walks Variant
// 2: orphan S with stale heartbeat under concurrent HeartbeatSnapshot,
// CommitAdoption, and RunSnapshotGCBatch. Final state must be one of:
//
//   - S survives, possibly adopted/current (heartbeat or adoption won)
//   - S reaped, CommitAdoption returned "not found", HeartbeatSnapshot
//     no-op'd silently (UPDATE with no matching row is not an error)
//
// Single-writer ordering means the three submitWrite ops execute in
// SOME serial order; six possible orderings collapse to those two
// outcomes.
func TestChaos_SnapshotGC_HeartbeatLivenessRace_VariantTwo(t *testing.T) {
	const heartbeatStaleGrace = int64(30 * 60) // 30 minutes
	for run := 0; run < chaosRuns; run++ {
		t.Run(fmt.Sprintf("run-%02d", run), func(t *testing.T) {
			c := openCache(t)
			ctx := context.Background()

			// Set up S as orphan with stale heartbeat. heartbeat_at=1
			// ensures the orphan-reap predicate `heartbeat_at < cutoff`
			// holds for any reasonable wall-clock now.
			inrel := seedBlob(t, c, fmt.Sprintf("V2 inrelease run-%02d", run))
			s := orphanCandidate(t, c, "http", "v2.example", "/p", inrel, 1)

			var (
				wg        sync.WaitGroup
				hbErr     error
				commitErr error
				gcErr     error
				gcResult  SnapshotGCBatchResult
			)
			wg.Add(3)
			go func() {
				defer wg.Done()
				hbErr = c.HeartbeatSnapshot(ctx, s)
			}()
			go func() {
				defer wg.Done()
				commitErr = c.CommitAdoption(ctx, s, nil, nil, nil, nil, false)
			}()
			go func() {
				defer wg.Done()
				gcResult, gcErr = c.RunSnapshotGCBatch(ctx, 100, heartbeatStaleGrace, 0)
			}()
			wg.Wait()

			if gcErr != nil {
				t.Fatalf("RunSnapshotGCBatch: %v", gcErr)
			}
			if hbErr != nil {
				// HeartbeatSnapshot's UPDATE is a no-op when the row is
				// gone; it should never return an error.
				t.Fatalf("HeartbeatSnapshot: %v", hbErr)
			}

			sExists := snapshotExists(t, c, s)

			switch {
			case sExists:
				// Outcomes 1-4: S survives. CommitAdoption ran or did
				// not (depending on order with HB), but if it ran it
				// succeeded. If CommitAdoption ran AFTER GC (which would
				// have reaped S), S wouldn't exist — contradiction. So
				// commitErr must be nil here.
				if commitErr != nil {
					t.Errorf("S survives but CommitAdoption errored: %v", commitErr)
				}
				// GC must have NOT reaped S (either heartbeat refreshed
				// before GC's SELECT, or S became current before GC's
				// SELECT).
				if gcResult.OrphanReaped > 0 {
					t.Errorf("S survives but GC reported orphanReaped=%d — inconsistent",
						gcResult.OrphanReaped)
				}
				// S is adopted (commit succeeded) — verify.
				if !snapshotIsAdopted(t, c, s) {
					t.Errorf("S survives but adopted_at IS NULL — adopt did not run?")
				}
				// And it's current.
				currentID := readCurrentSnapshotID(t, c, "http", "v2.example", "/p")
				if currentID != s {
					t.Errorf("S survives but current_snapshot_id = %d, want %d",
						currentID, s)
				}
				t.Logf("ordering: S survived (heartbeat or adoption beat GC)")

			case !sExists:
				// Outcomes 5-6: GC reaped S before any other op landed.
				// CommitAdoption must have failed (row gone).
				if commitErr == nil {
					t.Errorf("S reaped but CommitAdoption succeeded — torn state")
				}
				if gcResult.OrphanReaped != 1 {
					t.Errorf("S reaped but GC reported orphanReaped=%d, want 1",
						gcResult.OrphanReaped)
				}
				t.Logf("ordering: GC-first (S reaped as orphan)")
			}
		})
	}
}

// readCurrentSnapshotID returns suite_freshness.current_snapshot_id
// for the given (scheme, host, suite). Returns 0 if no freshness row
// or if current is NULL. Used by §12.5 invariant assertions.
func readCurrentSnapshotID(t *testing.T, c *Cache, scheme, host, suite string) int64 {
	t.Helper()
	var id int64
	err := c.db.QueryRow(`
SELECT COALESCE(current_snapshot_id, 0) FROM suite_freshness
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`,
		scheme, host, suite).Scan(&id)
	if err != nil {
		// sql.ErrNoRows means no freshness row — treat as 0 for the
		// "no current" case.
		return 0
	}
	return id
}

// snapshotIsAdopted returns true iff suite_snapshot.adopted_at IS NOT
// NULL for the given id.
func snapshotIsAdopted(t *testing.T, c *Cache, id int64) bool {
	t.Helper()
	var n int
	err := c.db.QueryRow(
		`SELECT count(*) FROM suite_snapshot
		  WHERE snapshot_id = ? AND adopted_at IS NOT NULL`, id,
	).Scan(&n)
	if err != nil {
		t.Fatalf("snapshotIsAdopted: %v", err)
	}
	return n > 0
}
