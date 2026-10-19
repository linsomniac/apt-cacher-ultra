package cache

import (
	"context"
	"errors"
	"testing"
)

func refcount(t *testing.T, c *Cache, hash string) int64 {
	t.Helper()
	b, err := c.GetBlob(context.Background(), hash)
	if err != nil {
		t.Fatal(err)
	}
	return b.RefCount
}

// commitInline adopts an inline snapshot of suite with the given extra
// members and returns its id.
func commitInline(t *testing.T, c *Cache, suite, anchor string, extra ...SnapshotMember) int64 {
	t.Helper()
	ctx := context.Background()
	id, _, err := c.InsertTrackedCandidateForTest(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: suite, InReleaseHash: &anchor})
	if err != nil {
		t.Fatal(err)
	}
	members := append([]SnapshotMember{{Path: "InRelease", BlobHash: anchor, DeclaredSHA256: anchor}}, extra...)
	for i := range members {
		members[i].SnapshotID = id
	}
	if err := c.CommitAdoption(ctx, id, members, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	return id
}

// Upstream validators as a real archive passes them through (an R-1 body
// tampered in transit keeps the honest upstream's ETag/Last-Modified).
const (
	auditETag    = `"5f2a1c-63c1e0d4b8e00"`
	auditLastMod = "Sat, 04 Oct 2026 12:34:56 GMT"
)

// seedValidators stores auditETag/auditLastMod on suite's freshness row,
// preserving its other columns (SeedSuiteForTest never touches the
// current pointer).
func seedValidators(t *testing.T, c *Cache, suite string) {
	t.Helper()
	ctx := context.Background()
	sf, err := c.GetSuiteFreshness(ctx, "http", "h", suite)
	if err != nil {
		t.Fatal(err)
	}
	etag, lastmod := auditETag, auditLastMod
	sf.InReleaseETag, sf.InReleaseLastMod = &etag, &lastmod
	if err := c.SeedSuiteForTest(ctx, *sf); err != nil {
		t.Fatal(err)
	}
}

// applyOne applies a single decision and reports whether it was current.
func applyOne(t *testing.T, c *Cache, id int64, o LegacyAuditOutcome) bool {
	t.Helper()
	cur, err := c.ApplyLegacyAuditDecisions(context.Background(), []LegacyAuditDecision{{SnapshotID: id, Outcome: o}})
	if err != nil {
		t.Fatal(err)
	}
	return cur[id]
}

// Spec §6.1: quarantining a current snapshot clears the pointer, releases
// its pins exactly once, retires it and dequeues it; a repeat is a no-op.
func TestApplyLegacyAuditDecisions_QuarantineCurrentReleasesPinsOnce(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, 1_800_000_000)()
	anchor := seedBlob(t, c, "quarantine anchor")
	pkgs := seedBlob(t, c, "quarantine packages")
	id := commitInline(t, c, "/r/dists/q", anchor,
		SnapshotMember{Path: "main/binary-amd64/Packages", BlobHash: pkgs, DeclaredSHA256: pkgs})
	if err := c.EnqueueLegacyTrustAudit(ctx, id); err != nil {
		t.Fatal(err)
	}
	seedValidators(t, c, "/r/dists/q")
	beforeA, beforeP := refcount(t, c, anchor), refcount(t, c, pkgs)

	if !applyOne(t, c, id, LegacyAuditQuarantine) {
		t.Fatal("quarantine did not report the snapshot as current")
	}
	if a, p := refcount(t, c, anchor), refcount(t, c, pkgs); a != beforeA-1 || p != beforeP-1 {
		t.Fatalf("refcounts after quarantine = %d, %d; want %d, %d", a, p, beforeA-1, beforeP-1)
	}
	sf, err := c.GetSuiteFreshness(ctx, "http", "h", "/r/dists/q")
	if err != nil || sf.CurrentSnapshotID != nil {
		t.Fatalf("suite still has a current snapshot: %+v (%v)", sf, err)
	}
	// Spec §6.1: the suite's validators go too. Kept, the next conditional
	// GET would carry them, the honest upstream would answer 304, and the
	// suite would never re-adopt.
	if sf.InReleaseETag != nil || sf.InReleaseLastMod != nil {
		t.Fatalf("validators survived the quarantine: etag=%v lastmod=%v", sf.InReleaseETag, sf.InReleaseLastMod)
	}
	if s, err := c.GetSuiteSnapshot(ctx, id); err != nil || s.RetiredAt == nil {
		t.Fatalf("snapshot not retired: %+v (%v)", s, err)
	}
	if items, err := c.LegacyAuditBatch(ctx, 0, 10); err != nil || len(items) != 0 {
		t.Fatalf("queue = %+v (%v), want empty", items, err)
	}
	// Idempotent: a second quarantine changes nothing.
	if applyOne(t, c, id, LegacyAuditQuarantine) {
		t.Fatal("second quarantine reported the snapshot as current")
	}
	if a := refcount(t, c, anchor); a != beforeA-1 {
		t.Fatalf("second quarantine changed refcount to %d", a)
	}
	// The suite can adopt the same content again: retired rows leave the
	// natural key (TestCommitAdoption_RefusesRetiredCandidate pins that the
	// retired row itself can never be committed).
	if _, _, err := c.InsertTrackedCandidateForTest(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/q", InReleaseHash: &anchor}); err != nil {
		t.Fatalf("re-adoption candidate refused: %v", err)
	}
}

// A displaced snapshot's pins were released at displacement: quarantine
// must release nothing — neither the displaced snapshot's blobs nor the
// current snapshot's.
func TestApplyLegacyAuditDecisions_QuarantineDisplacedKeepsRefcounts(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, 1_800_000_000)()
	oldAnchor := seedBlob(t, c, "displaced anchor v1")
	oldPkgs := seedBlob(t, c, "displaced packages v1")
	old := commitInline(t, c, "/r/dists/d", oldAnchor,
		SnapshotMember{Path: "main/binary-amd64/Packages", BlobHash: oldPkgs, DeclaredSHA256: oldPkgs})
	newAnchor := seedBlob(t, c, "displaced anchor v2")
	cur := commitInline(t, c, "/r/dists/d", newAnchor)
	// The validators belong to the current snapshot: quarantining the
	// displaced one must leave them alone.
	seedValidators(t, c, "/r/dists/d")
	before := map[string]int64{}
	for _, h := range []string{oldAnchor, oldPkgs, newAnchor} {
		before[h] = refcount(t, c, h)
	}
	// CommitAdoption marks signed commits; clear the markers so the
	// assertion below observes LegacyAuditQuarantineSigned alone.
	if _, err := c.db.Exec(`DELETE FROM suite_signed_seen`); err != nil {
		t.Fatal(err)
	}
	if applyOne(t, c, old, LegacyAuditQuarantineSigned) {
		t.Fatal("displaced snapshot reported as current")
	}
	for h, want := range before {
		if got := refcount(t, c, h); got != want {
			t.Errorf("refcount of %s changed %d -> %d (a double release)", h[:8], want, got)
		}
	}
	sf, err := c.GetSuiteFreshness(ctx, "http", "h", "/r/dists/d")
	if err != nil || sf.CurrentSnapshotID == nil || *sf.CurrentSnapshotID != cur {
		t.Fatalf("current pointer changed: %+v (%v)", sf, err)
	}
	if sf.InReleaseETag == nil || *sf.InReleaseETag != auditETag ||
		sf.InReleaseLastMod == nil || *sf.InReleaseLastMod != auditLastMod {
		t.Fatalf("the current snapshot's validators changed: etag=%v lastmod=%v", sf.InReleaseETag, sf.InReleaseLastMod)
	}
	if _, ok, _ := c.SignedSeen(ctx, "http", "h", "/r/dists/d"); !ok {
		t.Fatal("LegacyAuditQuarantineSigned wrote no marker")
	}
}

// One batch is one transaction: an invalid decision rolls back the
// decisions before it.
func TestApplyLegacyAuditDecisions_BatchIsAtomic(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	id := commitInline(t, c, "/r/dists/atomic", seedBlob(t, c, "atomic anchor"))
	if err := c.EnqueueLegacyTrustAudit(ctx, id); err != nil {
		t.Fatal(err)
	}
	_, err := c.ApplyLegacyAuditDecisions(ctx, []LegacyAuditDecision{
		{SnapshotID: id, Outcome: LegacyAuditQuarantine},
		{SnapshotID: id, Outcome: 0}, // invalid
	})
	if err == nil {
		t.Fatal("invalid outcome accepted")
	}
	sf, err := c.GetSuiteFreshness(ctx, "http", "h", "/r/dists/atomic")
	if err != nil || sf.CurrentSnapshotID == nil || *sf.CurrentSnapshotID != id {
		t.Fatalf("partial batch applied: %+v (%v)", sf, err)
	}
	if items, _ := c.LegacyAuditBatch(ctx, 0, 10); len(items) != 1 {
		t.Fatalf("queue = %+v, want the item still queued", items)
	}
}

// An unsigned snapshot is never trust-bearing: keeping it writes no marker.
func TestApplyLegacyAuditDecisions_KeepNeverMarksUnsigned(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	h := seedBlob(t, c, "unsigned Release (audit)")
	id, _, err := c.InsertTrackedCandidateForTest(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/u", InReleaseHash: &h, Form: FormUnsigned})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id, []SnapshotMember{{SnapshotID: id, Path: "Release", BlobHash: h, DeclaredSHA256: h}}, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	applyOne(t, c, id, LegacyAuditKeep)
	if _, ok, _ := c.SignedSeen(ctx, "http", "h", "/r/dists/u"); ok {
		t.Fatal("an unsigned snapshot was marked signed-seen")
	}
}

func TestCommitAdoption_RefusesRetiredCandidate(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	anchor := seedBlob(t, c, "retired candidate anchor")
	id, _, err := c.InsertTrackedCandidateForTest(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/rc", InReleaseHash: &anchor})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`UPDATE suite_snapshot SET retired_at = 1 WHERE snapshot_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	err = c.CommitAdoption(ctx, id, []SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: anchor, DeclaredSHA256: anchor}}, nil, nil, nil, false)
	if !errors.Is(err, ErrSnapshotRetired) {
		t.Fatalf("CommitAdoption(retired) err = %v, want ErrSnapshotRetired", err)
	}
}

func TestApplyLegacyAuditDecisions_KeepMarksWithAdoptionStamp(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	restore := stubNow(t, 1_700_000_000)
	signed := commitInline(t, c, "/r/dists/s", seedBlob(t, c, "complete anchor s"))
	restore()
	defer stubNow(t, 1_800_000_000)()
	if _, err := c.db.Exec(`DELETE FROM suite_signed_seen`); err != nil {
		t.Fatal(err)
	}
	if err := c.EnqueueLegacyTrustAudit(ctx, signed, 999_999); err != nil {
		t.Fatal(err)
	}
	items, err := c.LegacyAuditBatch(ctx, 0, 0)
	if err != nil || len(items) != 2 || items[1].SnapshotID != 999_999 || !items[1].Missing || items[0].Missing {
		t.Fatalf("queue = %+v (%v), want the snapshot and the missing 999999", items, err)
	}
	if after, err := c.LegacyAuditBatch(ctx, signed, 0); err != nil || len(after) != 1 || after[0].SnapshotID != 999_999 {
		t.Fatalf("items after %d = %+v (%v), want only 999999", signed, after, err)
	}
	if _, err := c.ApplyLegacyAuditDecisions(ctx, []LegacyAuditDecision{
		{SnapshotID: signed, Outcome: LegacyAuditKeep},
		{SnapshotID: 999_999, Outcome: LegacyAuditDrop},
	}); err != nil {
		t.Fatal(err)
	}
	if at, ok, _ := c.SignedSeen(ctx, "http", "h", "/r/dists/s"); !ok || at != 1_700_000_000 {
		t.Fatalf("marker = %d, %v; want the adoption stamp 1700000000", at, ok)
	}
	if items, _ := c.LegacyAuditBatch(ctx, 0, 0); len(items) != 0 {
		t.Fatalf("queue = %+v, want empty", items)
	}
}

func TestListTrackedSuiteAnchors_OnlyTrackedSignedAnchorNames(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	h := seedBlob(t, c, "tracked anchor blob")
	at := int64(1_799_990_000)
	put := func(path string) {
		t.Helper()
		if err := c.PutURLPath(ctx, URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: path, BlobHash: &h,
			UpstreamURL: "http://h" + path, IsMetadata: true, LastRequestedAt: &at}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.SeedSuiteForTest(ctx, SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/t"}); err != nil {
		t.Fatal(err)
	}
	put("/r/dists/t/InRelease")
	put("/r/dists/t/Release.gpg")
	put("/r/dists/t/Release")           // not a signed anchor name
	put("/r/dists/untracked/InRelease") // no suite_freshness row
	got, err := c.ListTrackedSuiteAnchors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("anchors = %+v, want the tracked InRelease and Release.gpg only", got)
	}
	for _, a := range got {
		if a.SuitePath != "/r/dists/t" || a.SeenAt != at {
			t.Errorf("anchor %+v, want suite /r/dists/t seen at %d", a, at)
		}
	}
}
