package cache

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// blobZeroedAt reads blob.refcount_zeroed_at for a hash. Returns -1 if NULL.
// Phase 4 GC tests use this to assert the SPEC4 §7.5.1 grace-clock
// bookkeeping: Rule 1 (PutBlob), Rule 2 (CommitAdoption Step 4), Rule 3
// (CommitAdoption Step 8 + EvictURLPath).
func blobZeroedAt(t *testing.T, c *Cache, hash string) int64 {
	t.Helper()
	var z sql.NullInt64
	err := c.db.QueryRow(`SELECT refcount_zeroed_at FROM blob WHERE hash = ?`, hash).Scan(&z)
	if err != nil {
		t.Fatalf("read refcount_zeroed_at of %s: %v", hash, err)
	}
	if !z.Valid {
		return -1
	}
	return z.Int64
}

// blobCreatedAt reads blob.created_at for a hash. Used by Rule 1 goldens
// to verify the grace clock starts at "now-at-INSERT-time".
func blobCreatedAt(t *testing.T, c *Cache, hash string) int64 {
	t.Helper()
	var v int64
	if err := c.db.QueryRow(`SELECT created_at FROM blob WHERE hash = ?`, hash).Scan(&v); err != nil {
		t.Fatalf("read created_at of %s: %v", hash, err)
	}
	return v
}

// stubNow installs a deterministic nowUnix and returns a restorer. Tests
// use this to advance time across rule transitions without sleeping.
func stubNow(t *testing.T, ts int64) func() {
	t.Helper()
	prev := nowUnix
	nowUnix = func() int64 { return ts }
	return func() { nowUnix = prev }
}

// ---------------------------------------------------------------------------
// SPEC4 §7.5.1 Rule 1: refcount_zeroed_at = created_at on PutBlob INSERT.
// ---------------------------------------------------------------------------

func TestPutBlob_Rule1_SetsRefcountZeroedAtToCreatedAt(t *testing.T) {
	c := openCache(t)

	const fixedNow = int64(1_700_000_000)
	defer stubNow(t, fixedNow)()

	h := seedBlob(t, c, "phase4 rule1 golden")

	if got := blobCreatedAt(t, c, h); got != fixedNow {
		t.Errorf("created_at = %d, want %d", got, fixedNow)
	}
	if got := blobZeroedAt(t, c, h); got != fixedNow {
		t.Errorf("refcount_zeroed_at = %d, want %d (must equal created_at on insert)", got, fixedNow)
	}
}

// ---------------------------------------------------------------------------
// SPEC4 §7.5.1 Rule 2: CommitAdoption Step 4 — clears refcount_zeroed_at on
// strictly-positive crossing; preserves on -1→0.
// ---------------------------------------------------------------------------

func TestCommitAdoption_Rule2_ClearsZeroedAtOnPositiveCrossing(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, 1_700_000_000)()

	r := seedBlob(t, c, "rule2 release")
	m := seedBlob(t, c, "rule2 member")

	// Pre-state: PutBlob set refcount_zeroed_at = now for both blobs.
	if got := blobZeroedAt(t, c, m); got == -1 {
		t.Fatalf("pre-condition failed: refcount_zeroed_at = NULL on freshly-inserted blob")
	}

	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "h.example",
		SuitePath:       "/p",
		InReleaseHash:   &r,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id, []SnapshotMember{
		{SnapshotID: id, Path: "InRelease", BlobHash: r, DeclaredSHA256: r},
		{SnapshotID: id, Path: "M", BlobHash: m, DeclaredSHA256: m},
	}, nil, nil, false); err != nil {
		t.Fatalf("CommitAdoption: %v", err)
	}

	// After +1 (0→1), Rule 2 clears refcount_zeroed_at to NULL.
	if got := blobZeroedAt(t, c, m); got != -1 {
		t.Errorf("post-commit refcount_zeroed_at = %d, want NULL after 0→1 crossing", got)
	}
	if got := blobRefcount(t, c, m); got != 1 {
		t.Errorf("post-commit refcount = %d, want 1", got)
	}
}

func TestCommitAdoption_Rule2_PreservesZeroedAtOnNegativeToZero(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	// Phase 1: stub now=1000; seed blobs and adopt snapshot 1 (refcount=1).
	restore := stubNow(t, 1000)
	r1 := seedBlob(t, c, "neg2zero rel1")
	m := seedBlob(t, c, "neg2zero member")
	id1, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "n2z.example",
		SuitePath:       "/p",
		InReleaseHash:   &r1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id1, []SnapshotMember{
		{SnapshotID: id1, Path: "InRelease", BlobHash: r1, DeclaredSHA256: r1},
		{SnapshotID: id1, Path: "M", BlobHash: m, DeclaredSHA256: m},
	}, nil, nil, false); err != nil {
		t.Fatalf("commit #1: %v", err)
	}
	restore()

	// Phase 2: stub now=2000; adopt snapshot 2 NOT containing m. The Step
	// 8 decrement on the prior snapshot's blobs takes m from 1→0 and
	// stamps refcount_zeroed_at = 2000 (Rule 3 first-≤0 crossing).
	restore = stubNow(t, 2000)
	r2 := seedBlob(t, c, "neg2zero rel2")
	id2, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "n2z.example",
		SuitePath:       "/p",
		InReleaseHash:   &r2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id2, []SnapshotMember{
		{SnapshotID: id2, Path: "InRelease", BlobHash: r2, DeclaredSHA256: r2},
	}, nil, nil, false); err != nil {
		t.Fatalf("commit #2: %v", err)
	}
	restore()

	// Sanity: m at refcount=0 with refcount_zeroed_at = 2000.
	if got := blobRefcount(t, c, m); got != 0 {
		t.Fatalf("after commit2: refcount(m) = %d, want 0", got)
	}
	if got := blobZeroedAt(t, c, m); got != 2000 {
		t.Fatalf("after commit2: refcount_zeroed_at(m) = %d, want 2000", got)
	}

	// Force m to refcount=-1 by EvictURLPath of a synthetic url_path row
	// pointing at it. (We need refcount=-1 for the next test step.)
	if err := c.PutURLPath(ctx, URLPath{
		CanonicalScheme: "http",
		CanonicalHost:   "n2z.example",
		Path:            "/dummy",
		BlobHash:        &m,
		UpstreamURL:     "http://n2z.example/dummy",
	}); err != nil {
		t.Fatal(err)
	}
	restore = stubNow(t, 3000)
	if err := c.EvictURLPath(ctx, "http", "n2z.example", "/dummy"); err != nil {
		t.Fatal(err)
	}
	restore()

	// Now refcount(m) = -1; refcount_zeroed_at preserved (Rule 3 0→-1 path).
	if got := blobRefcount(t, c, m); got != -1 {
		t.Fatalf("after evict: refcount(m) = %d, want -1", got)
	}
	if got := blobZeroedAt(t, c, m); got != 2000 {
		t.Fatalf("after evict: refcount_zeroed_at = %d, want 2000 preserved", got)
	}

	// Phase 3: adopt snapshot 3 that *does* include m. Step 4 increments
	// refcount -1→0. Rule 2 must PRESERVE refcount_zeroed_at (= 2000) —
	// the row is still ≤ 0, the grace clock continues.
	restore = stubNow(t, 4000)
	r3 := seedBlob(t, c, "neg2zero rel3")
	id3, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "n2z2.example",
		SuitePath:       "/p",
		InReleaseHash:   &r3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id3, []SnapshotMember{
		{SnapshotID: id3, Path: "InRelease", BlobHash: r3, DeclaredSHA256: r3},
		{SnapshotID: id3, Path: "M", BlobHash: m, DeclaredSHA256: m},
	}, nil, nil, false); err != nil {
		t.Fatalf("commit #3: %v", err)
	}
	restore()

	if got := blobRefcount(t, c, m); got != 0 {
		t.Errorf("after -1→0 bump: refcount(m) = %d, want 0", got)
	}
	if got := blobZeroedAt(t, c, m); got != 2000 {
		t.Errorf("after -1→0 bump: refcount_zeroed_at = %d, want 2000 preserved (Rule 2 only clears on strictly-positive crossing)", got)
	}
}

// ---------------------------------------------------------------------------
// SPEC4 §7.5.1 Rule 3: CommitAdoption Step 8 / EvictURLPath — set on first
// ≤0 crossing; preserve on 0→-1.
// ---------------------------------------------------------------------------

func TestCommitAdoption_Rule3_SetsZeroedAtOnFirstZeroCrossing(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	// Adopt snapshot 1 with member m (refcount 0→1; Rule 2 clears clock).
	restore := stubNow(t, 1000)
	r1 := seedBlob(t, c, "rule3 rel1")
	m := seedBlob(t, c, "rule3 member")
	id1, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "r3.example",
		SuitePath:       "/p",
		InReleaseHash:   &r1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id1, []SnapshotMember{
		{SnapshotID: id1, Path: "InRelease", BlobHash: r1, DeclaredSHA256: r1},
		{SnapshotID: id1, Path: "M", BlobHash: m, DeclaredSHA256: m},
	}, nil, nil, false); err != nil {
		t.Fatalf("commit #1: %v", err)
	}
	restore()

	// After +1: refcount=1, refcount_zeroed_at=NULL.
	if got := blobZeroedAt(t, c, m); got != -1 {
		t.Fatalf("pre-decrement refcount_zeroed_at = %d, want NULL", got)
	}

	// Adopt snapshot 2 displacing snapshot 1; m is no longer a member.
	restore = stubNow(t, 5000)
	r2 := seedBlob(t, c, "rule3 rel2")
	id2, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "r3.example",
		SuitePath:       "/p",
		InReleaseHash:   &r2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id2, []SnapshotMember{
		{SnapshotID: id2, Path: "InRelease", BlobHash: r2, DeclaredSHA256: r2},
	}, nil, nil, false); err != nil {
		t.Fatalf("commit #2: %v", err)
	}
	restore()

	// Step 8 decrements m (1→0). Rule 3: first ≤0 crossing → set to now=5000.
	if got := blobRefcount(t, c, m); got != 0 {
		t.Errorf("post-displace refcount(m) = %d, want 0", got)
	}
	if got := blobZeroedAt(t, c, m); got != 5000 {
		t.Errorf("post-displace refcount_zeroed_at = %d, want 5000 (first ≤0 crossing)", got)
	}
}

func TestCommitAdoption_Rule3_PreservesZeroedAtOnZeroToNegative(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	// Get m to refcount=0 with refcount_zeroed_at=1000.
	restore := stubNow(t, 1000)
	m := seedBlob(t, c, "rule3-z2n member")
	restore()

	// Verify pre-state: refcount=0, refcount_zeroed_at=1000.
	if got := blobZeroedAt(t, c, m); got != 1000 {
		t.Fatalf("pre: refcount_zeroed_at = %d, want 1000", got)
	}

	// EvictURLPath of a synthetic url_path → refcount goes 0→-1.
	if err := c.PutURLPath(ctx, URLPath{
		CanonicalScheme: "http",
		CanonicalHost:   "z2n.example",
		Path:            "/dummy",
		BlobHash:        &m,
		UpstreamURL:     "http://z2n.example/dummy",
	}); err != nil {
		t.Fatal(err)
	}
	restore = stubNow(t, 7777)
	if err := c.EvictURLPath(ctx, "http", "z2n.example", "/dummy"); err != nil {
		t.Fatal(err)
	}
	restore()

	if got := blobRefcount(t, c, m); got != -1 {
		t.Errorf("post-evict refcount(m) = %d, want -1", got)
	}
	// Rule 3 COALESCE: existing refcount_zeroed_at preserved (= 1000), NOT
	// overwritten with the evict-time now (7777).
	if got := blobZeroedAt(t, c, m); got != 1000 {
		t.Errorf("post-evict refcount_zeroed_at = %d, want 1000 preserved on 0→-1 crossing", got)
	}
}

func TestEvictURLPath_Rule3_SetsZeroedAtOnFirstZeroCrossing(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	// Adopt a snapshot that pins blob m at refcount=1.
	restore := stubNow(t, 1000)
	r := seedBlob(t, c, "evict-rule3 rel")
	m := seedBlob(t, c, "evict-rule3 member")
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "ev3.example",
		SuitePath:       "/p",
		InReleaseHash:   &r,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id, []SnapshotMember{
		{SnapshotID: id, Path: "InRelease", BlobHash: r, DeclaredSHA256: r},
		{SnapshotID: id, Path: "M", BlobHash: m, DeclaredSHA256: m},
	}, nil, nil, false); err != nil {
		t.Fatalf("commit: %v", err)
	}
	restore()

	// PutURLPath does not bump refcount; pinning is via the snapshot.
	// m: refcount=1, refcount_zeroed_at=NULL.
	if err := c.PutURLPath(ctx, URLPath{
		CanonicalScheme: "http",
		CanonicalHost:   "ev3.example",
		Path:            "/dummy",
		BlobHash:        &m,
		UpstreamURL:     "http://ev3.example/dummy",
	}); err != nil {
		t.Fatal(err)
	}

	// EvictURLPath at now=8888: refcount 1→0, Rule 3 sets clock to 8888.
	restore = stubNow(t, 8888)
	if err := c.EvictURLPath(ctx, "http", "ev3.example", "/dummy"); err != nil {
		t.Fatal(err)
	}
	restore()

	if got := blobRefcount(t, c, m); got != 0 {
		t.Errorf("post-evict refcount(m) = %d, want 0", got)
	}
	if got := blobZeroedAt(t, c, m); got != 8888 {
		t.Errorf("post-evict refcount_zeroed_at = %d, want 8888 (first ≤0 crossing)", got)
	}
}

// ---------------------------------------------------------------------------
// SPEC4 §12.1: PutBlob ON CONFLICT DO UPDATE — three goldens for the
// conflict path (refcount=0 advances; refcount>0 untouched; refcount<0
// advances).
// ---------------------------------------------------------------------------

func TestPutBlob_OnConflict_Refcount0_AdvancesZeroedAt(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	// Insert at now=1000 (created_at and refcount_zeroed_at both = 1000).
	restore := stubNow(t, 1000)
	h := seedBlob(t, c, "conflict-r0 body")
	restore()

	createdAt := blobCreatedAt(t, c, h)
	if createdAt != 1000 {
		t.Fatalf("created_at = %d, want 1000", createdAt)
	}

	// Re-PutBlob at now=4600 (1h+ later). Rule 1 path: refcount=0 row's
	// refcount_zeroed_at advances to 4600; created_at and refcount stay.
	restore = stubNow(t, 4600)
	if err := c.PutBlob(ctx, h, int64(len("conflict-r0 body"))); err != nil {
		t.Fatalf("PutBlob (conflict): %v", err)
	}
	restore()

	if got := blobZeroedAt(t, c, h); got != 4600 {
		t.Errorf("refcount_zeroed_at after conflict UPDATE = %d, want 4600", got)
	}
	if got := blobCreatedAt(t, c, h); got != 1000 {
		t.Errorf("created_at after conflict = %d, want 1000 (must not change)", got)
	}
	if got := blobRefcount(t, c, h); got != 0 {
		t.Errorf("refcount after conflict = %d, want 0 (must not change)", got)
	}
}

func TestPutBlob_OnConflict_PositiveRefcount_LeavesAllColumnsUntouched(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	// Insert at now=1000, then bump to refcount=5 by direct DB write
	// (skipping CommitAdoption to avoid noise; we want a pure positive row
	// with refcount_zeroed_at NULL — the natural state after Rule 2).
	restore := stubNow(t, 1000)
	h := seedBlob(t, c, "conflict-pos body")
	restore()

	// Force refcount=5, refcount_zeroed_at=NULL — emulates a snapshot
	// already pinning this blob.
	if _, err := c.db.Exec(`UPDATE blob SET refcount = 5, refcount_zeroed_at = NULL WHERE hash = ?`, h); err != nil {
		t.Fatalf("force refcount=5: %v", err)
	}

	createdAtPre := blobCreatedAt(t, c, h)

	// Re-PutBlob at now=9999. Conflict's WHERE refcount <= 0 filter
	// should skip the UPDATE entirely.
	restore = stubNow(t, 9999)
	if err := c.PutBlob(ctx, h, int64(len("conflict-pos body"))); err != nil {
		t.Fatalf("PutBlob (conflict on positive): %v", err)
	}
	restore()

	if got := blobRefcount(t, c, h); got != 5 {
		t.Errorf("refcount after conflict-on-positive = %d, want 5 unchanged", got)
	}
	if got := blobZeroedAt(t, c, h); got != -1 {
		t.Errorf("refcount_zeroed_at after conflict-on-positive = %d, want NULL (UPDATE was skipped)", got)
	}
	if got := blobCreatedAt(t, c, h); got != createdAtPre {
		t.Errorf("created_at = %d, want %d (must be unchanged)", got, createdAtPre)
	}
}

func TestPutBlob_OnConflict_NegativeRefcount_AdvancesZeroedAt(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	restore := stubNow(t, 1000)
	h := seedBlob(t, c, "conflict-neg body")
	restore()

	// Force refcount=-1, refcount_zeroed_at=2000 — emulates a transient
	// negative state after a Rule 3 0→-1 evict.
	if _, err := c.db.Exec(
		`UPDATE blob SET refcount = -1, refcount_zeroed_at = 2000 WHERE hash = ?`,
		h,
	); err != nil {
		t.Fatalf("force refcount=-1: %v", err)
	}

	// Re-PutBlob at now=6000. Conflict's WHERE refcount <= 0 matches the
	// negative row; refcount_zeroed_at advances to 6000.
	restore = stubNow(t, 6000)
	if err := c.PutBlob(ctx, h, int64(len("conflict-neg body"))); err != nil {
		t.Fatalf("PutBlob (conflict on negative): %v", err)
	}
	restore()

	if got := blobZeroedAt(t, c, h); got != 6000 {
		t.Errorf("refcount_zeroed_at after conflict-on-negative = %d, want 6000", got)
	}
	if got := blobRefcount(t, c, h); got != -1 {
		t.Errorf("refcount after conflict-on-negative = %d, want -1 unchanged", got)
	}
	if got := blobCreatedAt(t, c, h); got != 1000 {
		t.Errorf("created_at = %d, want 1000 unchanged", got)
	}
}

// ---------------------------------------------------------------------------
// SPEC4 §9.6.2: GC reap predicate full reachability.
// ---------------------------------------------------------------------------

// gcReapTestEnv prepares a cache and returns a graceSeconds value useful
// across the reap-predicate goldens.
func gcReapTestEnv(t *testing.T) (*Cache, int64) {
	t.Helper()
	c := openCache(t)
	return c, 3600 // 1h grace
}

// forceBlobState sets refcount and refcount_zeroed_at directly. The cache
// API only supports certain transitions; the reap-predicate goldens need
// to drive the row to specific corners (e.g. refcount=0 zeroed_at=NULL is
// the legacy guard case).
func forceBlobState(t *testing.T, c *Cache, hash string, refcount int64, zeroedAt sql.NullInt64) {
	t.Helper()
	if zeroedAt.Valid {
		if _, err := c.db.Exec(
			`UPDATE blob SET refcount = ?, refcount_zeroed_at = ? WHERE hash = ?`,
			refcount, zeroedAt.Int64, hash); err != nil {
			t.Fatalf("forceBlobState: %v", err)
		}
	} else {
		if _, err := c.db.Exec(
			`UPDATE blob SET refcount = ?, refcount_zeroed_at = NULL WHERE hash = ?`,
			refcount, hash); err != nil {
			t.Fatalf("forceBlobState: %v", err)
		}
	}
}

func TestRunBlobGCBatch_GraceBoundary_ExcludesInsideGrace(t *testing.T) {
	c, grace := gcReapTestEnv(t)
	ctx := context.Background()

	defer stubNow(t, 10_000)()

	h := seedBlob(t, c, "inside-grace")
	// refcount=0, zeroed_at = now-grace+1 → cutoff is now-grace; predicate
	// requires zeroed_at < cutoff. Equality at cutoff is excluded too.
	forceBlobState(t, c, h, 0, sql.NullInt64{Int64: 10_000 - grace + 1, Valid: true})

	got, err := c.RunBlobGCBatch(ctx, 100, grace)
	if err != nil {
		t.Fatalf("RunBlobGCBatch: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d reaped, want 0 (inside grace)", len(got))
	}
	if _, err := c.GetBlob(ctx, h); err != nil {
		t.Errorf("blob row gone: %v (must remain)", err)
	}
}

func TestRunBlobGCBatch_GraceBoundary_IncludesOutsideGrace(t *testing.T) {
	c, grace := gcReapTestEnv(t)
	ctx := context.Background()

	defer stubNow(t, 10_000)()

	h := seedBlob(t, c, "outside-grace")
	forceBlobState(t, c, h, 0, sql.NullInt64{Int64: 10_000 - grace - 1, Valid: true})

	got, err := c.RunBlobGCBatch(ctx, 100, grace)
	if err != nil {
		t.Fatalf("RunBlobGCBatch: %v", err)
	}
	if len(got) != 1 || got[0].Hash != h {
		t.Errorf("reaped = %+v, want exactly [%s]", got, h)
	}
}

func TestRunBlobGCBatch_NullZeroedAt_Excluded(t *testing.T) {
	c, grace := gcReapTestEnv(t)
	ctx := context.Background()

	defer stubNow(t, 10_000)()

	h := seedBlob(t, c, "null-zeroed-at legacy")
	forceBlobState(t, c, h, 0, sql.NullInt64{Valid: false})

	got, err := c.RunBlobGCBatch(ctx, 100, grace)
	if err != nil {
		t.Fatalf("RunBlobGCBatch: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d reaped, want 0 (NULL refcount_zeroed_at — legacy guard)", len(got))
	}
}

func TestRunBlobGCBatch_NegativeRefcount_EligibleByClock_Included(t *testing.T) {
	c, grace := gcReapTestEnv(t)
	ctx := context.Background()

	defer stubNow(t, 10_000)()

	h := seedBlob(t, c, "neg-refcount eligible")
	forceBlobState(t, c, h, -1, sql.NullInt64{Int64: 10_000 - grace - 1, Valid: true})

	got, err := c.RunBlobGCBatch(ctx, 100, grace)
	if err != nil {
		t.Fatalf("RunBlobGCBatch: %v", err)
	}
	if len(got) != 1 || got[0].Hash != h {
		t.Errorf("reaped = %+v, want exactly [%s]", got, h)
	}
}

func TestRunBlobGCBatch_PositiveRefcount_Excluded(t *testing.T) {
	c, grace := gcReapTestEnv(t)
	ctx := context.Background()

	defer stubNow(t, 10_000)()

	h := seedBlob(t, c, "pos-refcount excluded")
	forceBlobState(t, c, h, 1, sql.NullInt64{Int64: 10_000 - grace - 1, Valid: true})

	got, err := c.RunBlobGCBatch(ctx, 100, grace)
	if err != nil {
		t.Fatalf("RunBlobGCBatch: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("reaped = %+v, want 0 (refcount > 0)", got)
	}
}

func TestRunBlobGCBatch_URLPathReference_Excluded(t *testing.T) {
	c, grace := gcReapTestEnv(t)
	ctx := context.Background()

	defer stubNow(t, 10_000)()

	h := seedBlob(t, c, "url-path-ref")
	if err := c.PutURLPath(ctx, URLPath{
		CanonicalScheme: "http",
		CanonicalHost:   "u.example",
		Path:            "/x",
		BlobHash:        &h,
		UpstreamURL:     "http://u.example/x",
	}); err != nil {
		t.Fatal(err)
	}
	// Force the row to look reapable by refcount/clock — only the
	// url_path NOT EXISTS clause should save it.
	forceBlobState(t, c, h, 0, sql.NullInt64{Int64: 10_000 - grace - 1, Valid: true})

	got, err := c.RunBlobGCBatch(ctx, 100, grace)
	if err != nil {
		t.Fatalf("RunBlobGCBatch: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("reaped = %+v, want 0 (url_path FK reference)", got)
	}
}

func TestRunBlobGCBatch_SnapshotMemberReference_Excluded(t *testing.T) {
	c, grace := gcReapTestEnv(t)
	ctx := context.Background()

	defer stubNow(t, 10_000)()

	r := seedBlob(t, c, "sm-rel")
	m := seedBlob(t, c, "sm-member")
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "sm.example",
		SuitePath:       "/p",
		InReleaseHash:   &r,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id, []SnapshotMember{
		{SnapshotID: id, Path: "InRelease", BlobHash: r, DeclaredSHA256: r},
		{SnapshotID: id, Path: "M", BlobHash: m, DeclaredSHA256: m},
	}, nil, nil, false); err != nil {
		t.Fatal(err)
	}

	// Force m to refcount=0 with eligible clock so only the
	// snapshot_member NOT EXISTS clause prevents the reap.
	forceBlobState(t, c, m, 0, sql.NullInt64{Int64: 10_000 - grace - 1, Valid: true})

	got, err := c.RunBlobGCBatch(ctx, 100, grace)
	if err != nil {
		t.Fatalf("RunBlobGCBatch: %v", err)
	}
	for _, b := range got {
		if b.Hash == m {
			t.Errorf("member blob %s reaped despite snapshot_member reference", m)
		}
	}
}

func TestRunBlobGCBatch_SuiteSnapshotInReleaseRef_Excluded(t *testing.T) {
	c, grace := gcReapTestEnv(t)
	ctx := context.Background()

	defer stubNow(t, 10_000)()

	r := seedBlob(t, c, "ss-inrel-ref")
	if _, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "ss-ir.example",
		SuitePath:       "/p",
		InReleaseHash:   &r,
	}); err != nil {
		t.Fatal(err)
	}
	// Make r reapable by refcount/clock; only suite_snapshot.inrelease_hash
	// should save it.
	forceBlobState(t, c, r, 0, sql.NullInt64{Int64: 10_000 - grace - 1, Valid: true})

	got, err := c.RunBlobGCBatch(ctx, 100, grace)
	if err != nil {
		t.Fatalf("RunBlobGCBatch: %v", err)
	}
	for _, b := range got {
		if b.Hash == r {
			t.Errorf("InRelease blob %s reaped despite suite_snapshot.inrelease_hash reference", r)
		}
	}
}

func TestRunBlobGCBatch_SuiteSnapshotReleaseRef_Excluded(t *testing.T) {
	c, grace := gcReapTestEnv(t)
	ctx := context.Background()

	defer stubNow(t, 10_000)()

	rh := seedBlob(t, c, "ss-rel-ref")
	gh := seedBlob(t, c, "ss-rel-ref-gpg")
	if _, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "ss-r.example",
		SuitePath:       "/p",
		ReleaseHash:     &rh,
		ReleaseGPGHash:  &gh,
	}); err != nil {
		t.Fatal(err)
	}
	forceBlobState(t, c, rh, 0, sql.NullInt64{Int64: 10_000 - grace - 1, Valid: true})

	got, err := c.RunBlobGCBatch(ctx, 100, grace)
	if err != nil {
		t.Fatalf("RunBlobGCBatch: %v", err)
	}
	for _, b := range got {
		if b.Hash == rh {
			t.Errorf("Release blob %s reaped despite suite_snapshot.release_hash reference", rh)
		}
	}
}

func TestRunBlobGCBatch_SuiteSnapshotReleaseGPGRef_Excluded(t *testing.T) {
	c, grace := gcReapTestEnv(t)
	ctx := context.Background()

	defer stubNow(t, 10_000)()

	rh := seedBlob(t, c, "ss-gpg-rel")
	gh := seedBlob(t, c, "ss-gpg-ref")
	if _, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "ss-g.example",
		SuitePath:       "/p",
		ReleaseHash:     &rh,
		ReleaseGPGHash:  &gh,
	}); err != nil {
		t.Fatal(err)
	}
	forceBlobState(t, c, gh, 0, sql.NullInt64{Int64: 10_000 - grace - 1, Valid: true})

	got, err := c.RunBlobGCBatch(ctx, 100, grace)
	if err != nil {
		t.Fatalf("RunBlobGCBatch: %v", err)
	}
	for _, b := range got {
		if b.Hash == gh {
			t.Errorf("Release.gpg blob %s reaped despite suite_snapshot.release_gpg_hash reference", gh)
		}
	}
}

// ---------------------------------------------------------------------------
// SPEC4 §9.6.2: per-tick deadline / batching golden — given more candidates
// than batch_size, the loop drains across multiple batches.
// ---------------------------------------------------------------------------

func TestRunBlobGCBatch_DrainsAcrossBatches(t *testing.T) {
	c, grace := gcReapTestEnv(t)
	ctx := context.Background()

	defer stubNow(t, 10_000)()

	// Seed 7 reapable blobs.
	hashes := make([]string, 7)
	for i := 0; i < 7; i++ {
		// Use a small varying body to produce distinct hashes.
		hashes[i] = seedBlob(t, c, "drain-batch "+strings.Repeat("X", i+1))
		forceBlobState(t, c, hashes[i], 0, sql.NullInt64{Int64: 10_000 - grace - 10 - int64(i), Valid: true})
	}

	// Batch size = 3, so we expect 3 calls returning 3, 3, 1 rows
	// respectively, then a fourth call returning 0.
	totals := []int{}
	for i := 0; i < 5; i++ {
		got, err := c.RunBlobGCBatch(ctx, 3, grace)
		if err != nil {
			t.Fatalf("batch %d: %v", i, err)
		}
		totals = append(totals, len(got))
		if len(got) == 0 {
			break
		}
	}

	// Sum must equal 7 across at most 4 batches.
	sum := 0
	for _, n := range totals {
		sum += n
	}
	if sum != 7 {
		t.Errorf("totals = %v, sum = %d, want 7", totals, sum)
	}
	// All blob rows gone.
	for _, h := range hashes {
		_, err := c.GetBlob(ctx, h)
		if err == nil {
			t.Errorf("blob %s still present after drain", h)
		}
	}
}

// ---------------------------------------------------------------------------
// SPEC4 §9.6.3: snapshot GC SELECTs.
// ---------------------------------------------------------------------------

// adoptCandidate inserts an adopted (i.e. adopted_at IS NOT NULL)
// suite_snapshot row directly into the DB to avoid the ~6-step adoption
// pipeline overhead for snapshot-GC display goldens. The rows live in
// suite_snapshot only — no members, no package_hash, no suite_freshness
// pointer (caller wires that separately if needed).
func adoptCandidate(t *testing.T, c *Cache, scheme, host, suite string, inrel string, adoptedAt int64) int64 {
	t.Helper()
	res, err := c.db.Exec(`
INSERT INTO suite_snapshot
  (canonical_scheme, canonical_host, suite_path,
   inrelease_hash, created_at, adopted_at, package_coverage_complete, heartbeat_at)
VALUES (?, ?, ?, ?, ?, ?, 0, ?)`,
		scheme, host, suite, inrel, adoptedAt, adoptedAt, adoptedAt)
	if err != nil {
		t.Fatalf("adoptCandidate: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

// orphanCandidate inserts an unadopted (adopted_at IS NULL) suite_snapshot
// row used by the orphan-candidate goldens.
func orphanCandidate(t *testing.T, c *Cache, scheme, host, suite string, inrel string, heartbeatAt int64) int64 {
	t.Helper()
	res, err := c.db.Exec(`
INSERT INTO suite_snapshot
  (canonical_scheme, canonical_host, suite_path,
   inrelease_hash, created_at, adopted_at, package_coverage_complete, heartbeat_at)
VALUES (?, ?, ?, ?, ?, NULL, 0, ?)`,
		scheme, host, suite, inrel, heartbeatAt, heartbeatAt)
	if err != nil {
		t.Fatalf("orphanCandidate: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

// setCurrentSnapshot inserts/updates suite_freshness with the given
// current_snapshot_id. Pass 0 to mark the suite as having no current.
func setCurrentSnapshot(t *testing.T, c *Cache, scheme, host, suite string, snapshotID int64) {
	t.Helper()
	var arg any
	if snapshotID == 0 {
		arg = nil
	} else {
		arg = snapshotID
	}
	if _, err := c.db.Exec(`
INSERT INTO suite_freshness (canonical_scheme, canonical_host, suite_path, current_snapshot_id)
VALUES (?, ?, ?, ?)
ON CONFLICT(canonical_scheme, canonical_host, suite_path) DO UPDATE SET
  current_snapshot_id = excluded.current_snapshot_id`,
		scheme, host, suite, arg); err != nil {
		t.Fatalf("setCurrentSnapshot: %v", err)
	}
}

// snapshotExists is a small read helper for assertions.
func snapshotExists(t *testing.T, c *Cache, id int64) bool {
	t.Helper()
	var n int
	err := c.db.QueryRow(`SELECT 1 FROM suite_snapshot WHERE snapshot_id = ?`, id).Scan(&n)
	if err == nil {
		return true
	}
	return false
}

func TestRunSnapshotGCBatch_FiveAdopted_KeepDisplaced3_ReapsOldest(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	r1 := seedBlob(t, c, "kd3 r1")
	r2 := seedBlob(t, c, "kd3 r2")
	r3 := seedBlob(t, c, "kd3 r3")
	r4 := seedBlob(t, c, "kd3 r4")
	r5 := seedBlob(t, c, "kd3 r5")

	id1 := adoptCandidate(t, c, "http", "kd3.example", "/p", r1, 1000)
	id2 := adoptCandidate(t, c, "http", "kd3.example", "/p", r2, 2000)
	id3 := adoptCandidate(t, c, "http", "kd3.example", "/p", r3, 3000)
	id4 := adoptCandidate(t, c, "http", "kd3.example", "/p", r4, 4000)
	id5 := adoptCandidate(t, c, "http", "kd3.example", "/p", r5, 5000)
	setCurrentSnapshot(t, c, "http", "kd3.example", "/p", id5)

	defer stubNow(t, 1_000_000)() // way past the heartbeat grace; orphan path won't fire

	res, err := c.RunSnapshotGCBatch(ctx, 100, 1800 /*30m*/, 3)
	if err != nil {
		t.Fatalf("RunSnapshotGCBatch: %v", err)
	}
	// Ranking with snapshot 5 excluded:
	//   id4 rank 1, id3 rank 2, id2 rank 3, id1 rank 4 → only id1 reaped.
	if res.DisplacedReaped != 1 {
		t.Errorf("DisplacedReaped = %d, want 1", res.DisplacedReaped)
	}
	if res.OrphanReaped != 0 {
		t.Errorf("OrphanReaped = %d, want 0", res.OrphanReaped)
	}
	if snapshotExists(t, c, id1) {
		t.Errorf("snapshot id1 should be reaped")
	}
	for _, id := range []int64{id2, id3, id4, id5} {
		if !snapshotExists(t, c, id) {
			t.Errorf("snapshot %d should be preserved", id)
		}
	}
}

func TestRunSnapshotGCBatch_KeepDisplaced0_ReapsAllDisplaced(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	r1 := seedBlob(t, c, "kd0 r1")
	r2 := seedBlob(t, c, "kd0 r2")
	r3 := seedBlob(t, c, "kd0 r3")
	r4 := seedBlob(t, c, "kd0 r4")
	r5 := seedBlob(t, c, "kd0 r5")

	id1 := adoptCandidate(t, c, "http", "kd0.example", "/p", r1, 1000)
	id2 := adoptCandidate(t, c, "http", "kd0.example", "/p", r2, 2000)
	id3 := adoptCandidate(t, c, "http", "kd0.example", "/p", r3, 3000)
	id4 := adoptCandidate(t, c, "http", "kd0.example", "/p", r4, 4000)
	id5 := adoptCandidate(t, c, "http", "kd0.example", "/p", r5, 5000)
	setCurrentSnapshot(t, c, "http", "kd0.example", "/p", id5)

	defer stubNow(t, 1_000_000)()

	res, err := c.RunSnapshotGCBatch(ctx, 100, 1800, 0)
	if err != nil {
		t.Fatalf("RunSnapshotGCBatch: %v", err)
	}
	if res.DisplacedReaped != 4 {
		t.Errorf("DisplacedReaped = %d, want 4", res.DisplacedReaped)
	}
	if !snapshotExists(t, c, id5) {
		t.Errorf("current snapshot id5 should be preserved")
	}
	for _, id := range []int64{id1, id2, id3, id4} {
		if snapshotExists(t, c, id) {
			t.Errorf("snapshot %d should be reaped", id)
		}
	}
}

func TestRunSnapshotGCBatch_NullCurrent_AllAdoptedReaped(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	r1 := seedBlob(t, c, "nc r1")
	r2 := seedBlob(t, c, "nc r2")
	r3 := seedBlob(t, c, "nc r3")
	r4 := seedBlob(t, c, "nc r4")
	r5 := seedBlob(t, c, "nc r5")

	id1 := adoptCandidate(t, c, "http", "nc.example", "/p", r1, 1000)
	id2 := adoptCandidate(t, c, "http", "nc.example", "/p", r2, 2000)
	id3 := adoptCandidate(t, c, "http", "nc.example", "/p", r3, 3000)
	id4 := adoptCandidate(t, c, "http", "nc.example", "/p", r4, 4000)
	id5 := adoptCandidate(t, c, "http", "nc.example", "/p", r5, 5000)
	// suite_freshness with NULL current_snapshot_id.
	setCurrentSnapshot(t, c, "http", "nc.example", "/p", 0)

	defer stubNow(t, 1_000_000)()

	res, err := c.RunSnapshotGCBatch(ctx, 100, 1800, 0)
	if err != nil {
		t.Fatalf("RunSnapshotGCBatch: %v", err)
	}
	// All 5 adopted are displaced relative to "no current".
	if res.DisplacedReaped != 5 {
		t.Errorf("DisplacedReaped = %d, want 5", res.DisplacedReaped)
	}
	for _, id := range []int64{id1, id2, id3, id4, id5} {
		if snapshotExists(t, c, id) {
			t.Errorf("snapshot %d should be reaped (no current → all displaced)", id)
		}
	}
}

func TestRunSnapshotGCBatch_TieBreakOnSnapshotIDDesc(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	r1 := seedBlob(t, c, "tb r1")
	r2 := seedBlob(t, c, "tb r2")
	r3 := seedBlob(t, c, "tb r3")

	// Three adopted snapshots in one suite, all with adopted_at=5000;
	// snapshot_id rises 1<2<3 by INSERT order.
	id1 := adoptCandidate(t, c, "http", "tb.example", "/p", r1, 5000)
	id2 := adoptCandidate(t, c, "http", "tb.example", "/p", r2, 5000)
	id3 := adoptCandidate(t, c, "http", "tb.example", "/p", r3, 5000)
	// No current snapshot — tie-break decides which 1 of 3 is preserved
	// when keep_displaced=1.
	setCurrentSnapshot(t, c, "http", "tb.example", "/p", 0)

	defer stubNow(t, 1_000_000)()

	res, err := c.RunSnapshotGCBatch(ctx, 100, 1800, 1)
	if err != nil {
		t.Fatalf("RunSnapshotGCBatch: %v", err)
	}
	if res.DisplacedReaped != 2 {
		t.Errorf("DisplacedReaped = %d, want 2", res.DisplacedReaped)
	}
	// id3 has the highest snapshot_id; the (adopted_at DESC, snapshot_id
	// DESC) tie-break makes it rank 1.
	if !snapshotExists(t, c, id3) {
		t.Errorf("snapshot id3 (highest id) should be preserved by tie-break")
	}
	for _, id := range []int64{id1, id2} {
		if snapshotExists(t, c, id) {
			t.Errorf("snapshot %d should be reaped (tie-break ranks below id3)", id)
		}
	}
}

func TestRunSnapshotGCBatch_OrphanReap_HeartbeatStale(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	defer stubNow(t, 100_000)()

	r := seedBlob(t, c, "orphan-stale rel")
	// heartbeat_at = 50_000; staleGrace = 30_000; cutoff = 70_000;
	// 50_000 < 70_000 → eligible.
	id := orphanCandidate(t, c, "http", "or.example", "/p", r, 50_000)

	res, err := c.RunSnapshotGCBatch(ctx, 100, 30_000, 0)
	if err != nil {
		t.Fatalf("RunSnapshotGCBatch: %v", err)
	}
	if res.OrphanReaped != 1 {
		t.Errorf("OrphanReaped = %d, want 1", res.OrphanReaped)
	}
	if snapshotExists(t, c, id) {
		t.Errorf("orphan snapshot %d should be reaped", id)
	}
}

func TestRunSnapshotGCBatch_OrphanReap_HeartbeatFresh_Excluded(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	defer stubNow(t, 100_000)()

	r := seedBlob(t, c, "orphan-fresh rel")
	// heartbeat_at = 90_000; staleGrace = 30_000; cutoff = 70_000;
	// 90_000 >= 70_000 → not eligible.
	id := orphanCandidate(t, c, "http", "of.example", "/p", r, 90_000)

	res, err := c.RunSnapshotGCBatch(ctx, 100, 30_000, 0)
	if err != nil {
		t.Fatalf("RunSnapshotGCBatch: %v", err)
	}
	if res.OrphanReaped != 0 {
		t.Errorf("OrphanReaped = %d, want 0 (heartbeat fresh)", res.OrphanReaped)
	}
	if !snapshotExists(t, c, id) {
		t.Errorf("orphan snapshot %d should be preserved", id)
	}
}

// orphanReap predicate must NOT consider the current snapshot id pointer
// — even if heartbeat_at is stale, the row is current.
func TestRunSnapshotGCBatch_OrphanReap_ExcludesCurrentSnapshot(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	defer stubNow(t, 100_000)()

	r := seedBlob(t, c, "orphan-current rel")
	id := orphanCandidate(t, c, "http", "oc.example", "/p", r, 1)
	setCurrentSnapshot(t, c, "http", "oc.example", "/p", id)

	res, err := c.RunSnapshotGCBatch(ctx, 100, 30_000, 0)
	if err != nil {
		t.Fatalf("RunSnapshotGCBatch: %v", err)
	}
	if res.OrphanReaped != 0 {
		t.Errorf("OrphanReaped = %d, want 0 (snapshot is current despite NULL adopted_at + stale heartbeat)", res.OrphanReaped)
	}
	if !snapshotExists(t, c, id) {
		t.Errorf("current snapshot %d must not be reaped", id)
	}
}

// ---------------------------------------------------------------------------
// SPEC4 §7.5.2: HeartbeatSnapshot semantics.
// ---------------------------------------------------------------------------

func TestHeartbeatSnapshot_UpdatesOnlyHeartbeatAt(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	restore := stubNow(t, 1000)
	r := seedBlob(t, c, "hb test rel")
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "hb.example",
		SuitePath:       "/p",
		InReleaseHash:   &r,
	})
	if err != nil {
		t.Fatal(err)
	}
	restore()

	// Capture pre-state.
	pre := snapshotRow(t, c, id)

	restore = stubNow(t, 4444)
	if err := c.HeartbeatSnapshot(ctx, id); err != nil {
		t.Fatalf("HeartbeatSnapshot: %v", err)
	}
	restore()

	post := snapshotRow(t, c, id)
	if post.heartbeatAt != 4444 {
		t.Errorf("heartbeat_at = %d, want 4444", post.heartbeatAt)
	}
	if post.createdAt != pre.createdAt {
		t.Errorf("created_at changed: was %d, now %d", pre.createdAt, post.createdAt)
	}
	if post.adoptedAt != pre.adoptedAt {
		t.Errorf("adopted_at changed: was %v, now %v", pre.adoptedAt, post.adoptedAt)
	}
	if post.inrelease != pre.inrelease {
		t.Errorf("inrelease_hash changed: was %v, now %v", pre.inrelease, post.inrelease)
	}
}

func TestHeartbeatSnapshot_NoOpOnReapedID(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	// Use a snapshot_id that has never existed.
	if err := c.HeartbeatSnapshot(ctx, 99_999); err != nil {
		t.Errorf("HeartbeatSnapshot on nonexistent id: got %v, want nil (zero rows updated is benign)", err)
	}
}

type snapshotRowState struct {
	createdAt   int64
	adoptedAt   sql.NullInt64
	heartbeatAt int64
	inrelease   sql.NullString
}

func snapshotRow(t *testing.T, c *Cache, id int64) snapshotRowState {
	t.Helper()
	var s snapshotRowState
	err := c.db.QueryRow(`
SELECT created_at, adopted_at, heartbeat_at, inrelease_hash
FROM suite_snapshot WHERE snapshot_id = ?`, id).Scan(
		&s.createdAt, &s.adoptedAt, &s.heartbeatAt, &s.inrelease)
	if err != nil {
		t.Fatalf("snapshotRow: %v", err)
	}
	return s
}

// ---------------------------------------------------------------------------
// SPEC4 §7.5.2 / Rule 1: HeartbeatBlobs semantics.
// ---------------------------------------------------------------------------

func TestHeartbeatBlobs_RefreshesZeroedAtForRefcountZero(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	restore := stubNow(t, 1000)
	h := seedBlob(t, c, "hb-blob refresh")
	restore()

	if got := blobZeroedAt(t, c, h); got != 1000 {
		t.Fatalf("pre: refcount_zeroed_at = %d, want 1000", got)
	}

	restore = stubNow(t, 5555)
	if err := c.HeartbeatBlobs(ctx, []string{h}); err != nil {
		t.Fatalf("HeartbeatBlobs: %v", err)
	}
	restore()

	if got := blobZeroedAt(t, c, h); got != 5555 {
		t.Errorf("refcount_zeroed_at = %d, want 5555 (heartbeat advanced)", got)
	}
}

func TestHeartbeatBlobs_PreservesNullForPositiveRefcount(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	h := seedBlob(t, c, "hb-blob positive")
	// Force refcount=5, refcount_zeroed_at=NULL — simulates the natural
	// post-Rule-2 state of a blob pinned by an active snapshot.
	if _, err := c.db.Exec(`UPDATE blob SET refcount = 5, refcount_zeroed_at = NULL WHERE hash = ?`, h); err != nil {
		t.Fatal(err)
	}

	defer stubNow(t, 7777)()
	if err := c.HeartbeatBlobs(ctx, []string{h}); err != nil {
		t.Fatalf("HeartbeatBlobs: %v", err)
	}

	// Rule 2 invariant: positive-refcount row must keep refcount_zeroed_at
	// NULL. WHERE refcount <= 0 in HeartbeatBlobs filters this row out.
	if got := blobZeroedAt(t, c, h); got != -1 {
		t.Errorf("refcount_zeroed_at = %d, want NULL preserved on positive-refcount row", got)
	}
}

func TestHeartbeatBlobs_EmptySliceNoOp(t *testing.T) {
	c := openCache(t)
	if err := c.HeartbeatBlobs(context.Background(), nil); err != nil {
		t.Errorf("HeartbeatBlobs(nil): %v", err)
	}
	if err := c.HeartbeatBlobs(context.Background(), []string{}); err != nil {
		t.Errorf("HeartbeatBlobs([]): %v", err)
	}
}

func TestHeartbeatBlobs_SkipsInvalidHashes(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	restore := stubNow(t, 1000)
	good := seedBlob(t, c, "hb-blob mixed valid")
	restore()

	bad := strings.Repeat("z", 64) // not [0-9a-f]
	short := "abcdef"

	restore = stubNow(t, 9999)
	if err := c.HeartbeatBlobs(ctx, []string{bad, good, short}); err != nil {
		t.Fatalf("HeartbeatBlobs (mixed): %v", err)
	}
	restore()

	// Good hash advanced.
	if got := blobZeroedAt(t, c, good); got != 9999 {
		t.Errorf("good blob refcount_zeroed_at = %d, want 9999", got)
	}
}

func TestHeartbeatBlobs_AllInvalidNoOp(t *testing.T) {
	c := openCache(t)
	if err := c.HeartbeatBlobs(context.Background(), []string{"not-a-hash", strings.Repeat("Z", 64)}); err != nil {
		t.Errorf("HeartbeatBlobs (all invalid): %v", err)
	}
}

// ---------------------------------------------------------------------------
// SPEC4 §9.6.4: HashKnown for the startup pool scan.
// ---------------------------------------------------------------------------

func TestHashKnown_PresentAndAbsent(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	h := seedBlob(t, c, "hashknown body")
	// Present.
	known, err := c.HashKnown(ctx, h)
	if err != nil {
		t.Fatalf("HashKnown(present): %v", err)
	}
	if !known {
		t.Errorf("HashKnown(present) = false, want true")
	}

	// Absent.
	other := strings.Repeat("d", 64)
	known, err = c.HashKnown(ctx, other)
	if err != nil {
		t.Fatalf("HashKnown(absent): %v", err)
	}
	if known {
		t.Errorf("HashKnown(absent) = true, want false")
	}
}

func TestHashKnown_RejectsMalformedHash(t *testing.T) {
	c := openCache(t)
	if _, err := c.HashKnown(context.Background(), "bogus"); err == nil {
		t.Error("HashKnown(bogus): expected error, got nil")
	}
}

// ---------------------------------------------------------------------------
// SPEC4 §4.3.2: migration v3 → v4.
// ---------------------------------------------------------------------------

// openV3Cache opens a cache rooted at t.TempDir() and runs migrations
// 0→1→2→3, leaving schema_version = 3. Used by the v3→v4 migration
// goldens.
func openV3Cache(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"pool", "tmp", "staging"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	db, err := openDB(filepath.Join(dir, "cache.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	for v := 0; v < 3; v++ {
		if err := applyMigration(context.Background(), db, v); err != nil {
			_ = db.Close()
			t.Fatalf("applyMigration v%d→v%d: %v", v, v+1, err)
		}
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, dir
}

func TestMigration_V3ToV4_AddsColumnsAndIndexes(t *testing.T) {
	db, _ := openV3Cache(t)
	ctx := context.Background()

	// Pristine v3: new columns / indexes do not exist yet.
	for _, tc := range []struct{ table, col string }{
		{"blob", "refcount_zeroed_at"},
		{"suite_snapshot", "heartbeat_at"},
	} {
		if hasColumn(t, db, tc.table, tc.col) {
			t.Errorf("v3 db already has %s.%s; expected pristine v3", tc.table, tc.col)
		}
	}
	for _, idx := range []string{"idx_blob_gc", "idx_url_path_blob"} {
		if hasIndex(t, db, idx) {
			t.Errorf("v3 db already has index %s; expected pristine v3", idx)
		}
	}

	if err := applyMigration(ctx, db, 3); err != nil {
		t.Fatalf("applyMigration v3→v4: %v", err)
	}

	for _, tc := range []struct{ table, col string }{
		{"blob", "refcount_zeroed_at"},
		{"suite_snapshot", "heartbeat_at"},
	} {
		if !hasColumn(t, db, tc.table, tc.col) {
			t.Errorf("after migration, %s.%s missing", tc.table, tc.col)
		}
	}
	for _, idx := range []string{"idx_blob_gc", "idx_url_path_blob"} {
		if !hasIndex(t, db, idx) {
			t.Errorf("after migration, index %s missing", idx)
		}
	}

	v, err := readSchemaVersion(ctx, db)
	if err != nil {
		t.Fatalf("readSchemaVersion: %v", err)
	}
	if v != 4 {
		t.Errorf("schema_version = %d, want 4", v)
	}
}

// SPEC4 §4.3.2 backfill: pre-v4 rows at refcount <= 0 must have
// refcount_zeroed_at populated to created_at; pre-v4 suite_snapshot rows
// must have heartbeat_at = created_at.
func TestMigration_V3ToV4_BackfillsRefcountZeroedAt(t *testing.T) {
	db, _ := openV3Cache(t)
	ctx := context.Background()

	zero := strings.Repeat("a", 64)
	pos := strings.Repeat("b", 64)
	neg := strings.Repeat("c", 64)
	if _, err := db.Exec(`INSERT INTO blob (hash, size, created_at, refcount) VALUES (?, 1, 100, 0)`, zero); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO blob (hash, size, created_at, refcount) VALUES (?, 1, 200, 1)`, pos); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO blob (hash, size, created_at, refcount) VALUES (?, 1, 300, -1)`, neg); err != nil {
		t.Fatal(err)
	}

	if err := applyMigration(ctx, db, 3); err != nil {
		t.Fatalf("applyMigration: %v", err)
	}

	for _, tc := range []struct {
		hash string
		want sql.NullInt64
	}{
		{zero, sql.NullInt64{Int64: 100, Valid: true}}, // refcount <= 0 → backfilled to created_at
		{pos, sql.NullInt64{Valid: false}},             // refcount > 0 → NULL (Rule 2 invariant)
		{neg, sql.NullInt64{Int64: 300, Valid: true}},  // refcount <= 0 → backfilled to created_at
	} {
		var got sql.NullInt64
		if err := db.QueryRow(`SELECT refcount_zeroed_at FROM blob WHERE hash = ?`, tc.hash).Scan(&got); err != nil {
			t.Fatalf("read refcount_zeroed_at(%s): %v", tc.hash, err)
		}
		if got.Valid != tc.want.Valid || (got.Valid && got.Int64 != tc.want.Int64) {
			t.Errorf("blob %s: got %+v, want %+v", tc.hash, got, tc.want)
		}
	}
}

func TestMigration_V3ToV4_BackfillsHeartbeatAt(t *testing.T) {
	db, _ := openV3Cache(t)
	ctx := context.Background()

	rh := strings.Repeat("a", 64)
	if _, err := db.Exec(`INSERT INTO blob (hash, size, created_at) VALUES (?, 1, 100)`, rh); err != nil {
		t.Fatal(err)
	}
	res, err := db.Exec(`INSERT INTO suite_snapshot
	    (canonical_scheme, canonical_host, suite_path, inrelease_hash, created_at, package_coverage_complete)
	    VALUES ('http', 'h.example', '/p', ?, 12345, 0)`, rh)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()

	if err := applyMigration(ctx, db, 3); err != nil {
		t.Fatalf("applyMigration: %v", err)
	}

	var hb int64
	if err := db.QueryRow(`SELECT heartbeat_at FROM suite_snapshot WHERE snapshot_id = ?`, id).Scan(&hb); err != nil {
		t.Fatalf("read heartbeat_at: %v", err)
	}
	if hb != 12345 {
		t.Errorf("heartbeat_at = %d, want 12345 (= created_at)", hb)
	}
}

func TestMigration_V3ToV4_PartialIndexHasWhereClause(t *testing.T) {
	db, _ := openV3Cache(t)
	ctx := context.Background()
	if err := applyMigration(ctx, db, 3); err != nil {
		t.Fatalf("applyMigration: %v", err)
	}

	for _, tc := range []struct {
		index, want string
	}{
		{"idx_blob_gc", "WHERE refcount <= 0"},
		{"idx_url_path_blob", "WHERE blob_hash IS NOT NULL"},
	} {
		var sqlText sql.NullString
		err := db.QueryRow(
			`SELECT sql FROM sqlite_master WHERE type='index' AND name=?`, tc.index,
		).Scan(&sqlText)
		if err != nil {
			t.Fatalf("read sql for %s: %v", tc.index, err)
		}
		if !sqlText.Valid {
			t.Errorf("%s: sqlite_master.sql is NULL", tc.index)
			continue
		}
		if !strings.Contains(sqlText.String, tc.want) {
			t.Errorf("%s sql does not contain %q: %s", tc.index, tc.want, sqlText.String)
		}
	}
}

// TestMigration_V3ToV4_AtomicRollback forces a real mid-flight failure
// on a v3 DB and asserts every prior step rolls back atomically. We
// pre-create `idx_url_path_blob` — which is the LAST statement in
// migrations[3] — so the migration successfully ADDs both columns,
// runs both backfill UPDATEs, and CREATEs `idx_blob_gc` before
// hitting "index idx_url_path_blob already exists". The transaction
// must roll back: schema_version stays at 3, both new columns are
// absent, both new indexes are absent (the pre-existing
// `idx_url_path_blob` we planted survives because it lived outside
// the transaction).
func TestMigration_V3ToV4_AtomicRollback(t *testing.T) {
	db, _ := openV3Cache(t)
	ctx := context.Background()

	// Pre-create the last index migrations[3] tries to add. v3 already
	// has url_path.blob_hash so the index is constructible.
	if _, err := db.Exec(`CREATE INDEX idx_url_path_blob ON url_path(blob_hash) WHERE blob_hash IS NOT NULL`); err != nil {
		t.Fatalf("seed pre-existing index: %v", err)
	}

	err := applyMigration(ctx, db, 3)
	if err == nil {
		t.Fatal("expected applyMigration v3→v4 to fail on duplicate index, got nil")
	}

	// Atomic rollback assertions: schema_version unchanged, both new
	// columns absent, idx_blob_gc absent (it was created before the
	// failing statement and must have been rolled back).
	v, verr := readSchemaVersion(ctx, db)
	if verr != nil {
		t.Fatalf("readSchemaVersion: %v", verr)
	}
	if v != 3 {
		t.Errorf("schema_version after failed migration = %d, want 3", v)
	}
	if hasColumn(t, db, "blob", "refcount_zeroed_at") {
		t.Error("blob.refcount_zeroed_at survived failed migration; ADD COLUMN was not rolled back")
	}
	if hasColumn(t, db, "suite_snapshot", "heartbeat_at") {
		t.Error("suite_snapshot.heartbeat_at survived failed migration; ADD COLUMN was not rolled back")
	}
	if hasIndex(t, db, "idx_blob_gc") {
		t.Error("idx_blob_gc survived failed migration; CREATE INDEX was not rolled back")
	}
	// The pre-existing idx_url_path_blob (planted outside the
	// migration tx) must still exist — it's not part of the rollback.
	if !hasIndex(t, db, "idx_url_path_blob") {
		t.Error("pre-existing idx_url_path_blob was incorrectly removed")
	}
}

// TestMigration_V3ToV4_FailedRetryDoesNotCorruptVersion: a separate
// idempotency test — re-running migrations[3] after a successful
// migration produces a SQL error (duplicate column) but leaves
// schema_version at 4, with no half-applied state.
func TestMigration_V3ToV4_FailedRetryDoesNotCorruptVersion(t *testing.T) {
	db, _ := openV3Cache(t)
	ctx := context.Background()

	if err := applyMigration(ctx, db, 3); err != nil {
		t.Fatalf("first applyMigration v3→v4: %v", err)
	}
	if err := applyMigration(ctx, db, 3); err == nil {
		t.Fatal("re-applying v3→v4 unexpectedly succeeded (ADD COLUMN should duplicate)")
	}
	v, _ := readSchemaVersion(ctx, db)
	if v != 4 {
		t.Errorf("schema_version after failed re-apply = %d, want 4 (failed retry must not corrupt version)", v)
	}
}

// migrate (the orchestrator) is idempotent if the DB is already at
// CurrentSchemaVersion: it returns nil without re-running migrations[3].
func TestMigrate_V4_IdempotentReapply(t *testing.T) {
	c := openCache(t) // already at v4
	v1, _ := readSchemaVersion(context.Background(), c.db)
	if v1 != CurrentSchemaVersion {
		t.Fatalf("fresh cache at v%d, want v%d", v1, CurrentSchemaVersion)
	}
	// migrate() noop on current version.
	if err := migrate(context.Background(), c.db, c.logger); err != nil {
		t.Fatalf("migrate (idempotent): %v", err)
	}
	v2, _ := readSchemaVersion(context.Background(), c.db)
	if v2 != CurrentSchemaVersion {
		t.Errorf("schema_version after idempotent migrate = %d, want %d", v2, CurrentSchemaVersion)
	}
}
