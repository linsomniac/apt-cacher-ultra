package cache

import (
	"context"
	"fmt"
	"testing"
)

// SPEC4 §12.4: GC + EvictURLPath race chaos test.
//
// Property: a blob whose refcount transitions 1 → 0 → -1 (adoption
// decrement then §6.1 hit-path eviction decrement) is reaped at the
// next eligible tick, with the grace clock counted from the 1 → 0
// transition (NOT restarted by the 0 → -1 transition).
//
// Driver verifies the COALESCE semantics of §7.5.1 Rule 3: the 0 → -1
// UPDATE preserves the existing refcount_zeroed_at. If the COALESCE
// were dropped (i.e. zeroed_at refreshed on every ≤0 crossing), the
// blob would survive past its true grace expiry — caught here by the
// blob GC failing to reap.
//
// The boundary is constructed so the test is *sensitive* to the bug:
// t1 = 1→0 timestamp, t2 = 0→-1 timestamp (well past t1), and the GC
// runs at t3 = t1 + grace + 1. Predicate:
//
//   - correct (zeroed_at = t1):  t1 < t3 - grace = t1 + 1  → reap
//   - broken  (zeroed_at = t2):  t2 < t3 - grace = t1 + 1  → keep
//
// 10-consecutive-runs gate per the Phase 2 / Phase 3 chaos test
// pattern.

// TestChaos_BlobGC_EvictRace_OneZeroNegOne walks the full 1 → 0 → -1
// chain and asserts the blob is reaped at the next tick after grace
// expires from the 1 → 0 timestamp.
func TestChaos_BlobGC_EvictRace_OneZeroNegOne(t *testing.T) {
	const (
		t1    = int64(1000) // 1→0 transition (Rule 3 sets zeroed_at = t1)
		t2    = int64(5000) // 0→-1 transition (Rule 3 COALESCE preserves)
		grace = int64(60)
	)
	t3 := t1 + grace + 1 // GC fires here; predicate cutoff = t1 + 1

	for run := 0; run < chaosRuns; run++ {
		t.Run(fmt.Sprintf("run-%02d", run), func(t *testing.T) {
			c := openCache(t)
			ctx := context.Background()

			// Step 0: lock the clock at t1. All adoption-time writes
			// see this fixed timestamp, so refcount_zeroed_at landings
			// are deterministic.
			restore := stubNow(t, t1)
			// Defer-by-closure: the closure reads `restore` at fire
			// time, not at defer-evaluation time, so any t.Fatalf
			// between here and test end still unwinds whichever
			// stubNow incarnation is currently active. Without this,
			// a fatal mid-test would leak the nowUnix stub into
			// later test files in the same package.
			defer func() { restore() }()

			// Three blobs: B (the test subject), and two distinct
			// InRelease blobs so suite_snapshot's natural key is
			// unique across the two adoptions.
			b := seedBlob(t, c, "evict-race blob B")
			r1 := seedBlob(t, c, "evict-race S1 InRelease")
			r2 := seedBlob(t, c, "evict-race S2 InRelease")

			// Step 1: adopt S1 with B as a member. Rule 2 bumps B's
			// refcount to 1 and clears refcount_zeroed_at.
			s1, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
				CanonicalScheme: "http",
				CanonicalHost:   "ev.example",
				SuitePath:       "/p",
				InReleaseHash:   &r1,
			})
			if err != nil {
				t.Fatalf("InsertCandidateSnapshot S1: %v", err)
			}
			if err := c.CommitAdoption(ctx, s1, []SnapshotMember{
				{SnapshotID: s1, Path: "M", BlobHash: b, DeclaredSHA256: b},
			}, nil,

				nil, nil, false); err != nil {
				t.Fatalf("CommitAdoption S1: %v", err)
			}
			if rc := blobRefcount(t, c, b); rc != 1 {
				t.Fatalf("B post-S1 refcount = %d, want 1", rc)
			}
			if z := blobZeroedAt(t, c, b); z != -1 {
				t.Fatalf("B post-S1 zeroed_at = %d, want NULL (Rule 2 clears on positive crossing)", z)
			}

			// Step 2: adopt S2 (B is NOT a member). S1 is displaced.
			// CommitAdoption Step 8 walks S1's snapshot_member rows
			// and decrements refcount; B goes 1 → 0; Rule 3 sets
			// zeroed_at = t1 (now via stubNow).
			s2, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
				CanonicalScheme: "http",
				CanonicalHost:   "ev.example",
				SuitePath:       "/p",
				InReleaseHash:   &r2,
			})
			if err != nil {
				t.Fatalf("InsertCandidateSnapshot S2: %v", err)
			}
			if err := c.CommitAdoption(ctx, s2, nil, nil, nil, nil, false); err != nil {
				t.Fatalf("CommitAdoption S2: %v", err)
			}
			if rc := blobRefcount(t, c, b); rc != 0 {
				t.Fatalf("B post-S2 refcount = %d, want 0", rc)
			}
			if z := blobZeroedAt(t, c, b); z != t1 {
				t.Fatalf("B post-S2 zeroed_at = %d, want %d (Rule 3 first ≤0 crossing)", z, t1)
			}

			// Step 3: plant a url_path pointing at B. PutURLPath does
			// NOT bump refcount — it just installs a FK reachability
			// anchor that EvictURLPath later removes.
			if err := c.PutURLPath(ctx, URLPath{
				CanonicalScheme: "http",
				CanonicalHost:   "ev.example",
				Path:            "/race-evict",
				BlobHash:        &b,
				UpstreamURL:     "http://ev.example/race-evict",
			}); err != nil {
				t.Fatalf("PutURLPath: %v", err)
			}

			// Step 4: advance the clock to t2 and run EvictURLPath.
			// Rule 3's COALESCE preserves the existing zeroed_at = t1
			// across the 0 → -1 transition. If the COALESCE were
			// missing, zeroed_at would be refreshed to t2.
			restore()
			restore = stubNow(t, t2)
			if err := c.EvictURLPath(ctx, "http", "ev.example", "/race-evict"); err != nil {
				t.Fatalf("EvictURLPath: %v", err)
			}
			if rc := blobRefcount(t, c, b); rc != -1 {
				t.Fatalf("B post-evict refcount = %d, want -1", rc)
			}
			if z := blobZeroedAt(t, c, b); z != t1 {
				t.Fatalf("B post-evict zeroed_at = %d, want %d preserved (COALESCE)", z, t1)
			}

			// Step 5: reap the displaced snapshot S1 so the blob GC's
			// NOT EXISTS (snapshot_member) clause is satisfied. Use
			// keep_displaced=0 so all displaced snapshots are eligible.
			// staleGraceSeconds is irrelevant for sub-job B (displaced
			// path) — pass any value; we use 30 minutes as the runtime
			// default would.
			snapRes, err := c.RunSnapshotGCBatch(ctx, 100, 30*60, 0)
			if err != nil {
				t.Fatalf("RunSnapshotGCBatch: %v", err)
			}
			if snapRes.DisplacedReaped != 1 {
				t.Fatalf("snapshot GC: DisplacedReaped = %d, want 1 (S1)", snapRes.DisplacedReaped)
			}
			if snapshotExists(t, c, s1) {
				t.Fatalf("S1 still present after snapshot GC")
			}

			// Step 6: advance the clock to t3 = t1 + grace + 1 and
			// run blob GC. Predicate: zeroed_at < t3 - grace
			// = t1 + 1. With the COALESCE-preserved t1, the predicate
			// holds; B is reaped.
			restore()
			restore = stubNow(t, t3)

			reaped, err := c.RunBlobGCBatch(ctx, 100, grace)
			if err != nil {
				t.Fatalf("RunBlobGCBatch: %v", err)
			}
			set := reapedHashes(reaped)
			if !set[b] {
				t.Fatalf("Variant 1→0→-1: B not reaped; reaped=%+v", reaped)
			}
			if chaosBlobRowExists(t, c, b) {
				t.Errorf("B's blob row still present after reap")
			}
		})
	}
}
