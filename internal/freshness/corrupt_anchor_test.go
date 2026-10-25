package freshness

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// rotBlob overwrites the pool file of hash with other bytes: bit rot the
// audit finds as anchor_corrupt.
func rotBlob(t *testing.T, c *cache.Cache, hash string) {
	t.Helper()
	p := c.BlobPath(hash)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("bit-rotted bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Ruling R-H2: of the audit's verdicts only anchor_corrupt asks the
// quarantine to delete the anchor's url_path rows — not anchor_missing,
// not anchor_oversized (not read in full), not the content verdicts.
func TestAuditLegacySnapshot_OnlyACorruptAnchorDropsItsRows(t *testing.T) {
	env := newAdoptionTestEnv(t)
	c := env.cache
	good, err := env.adopter.writeBlobBytes(context.Background(), clearsignForTest(t, realisticRelease()))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := env.adopter.writeBlobBytes(context.Background(), []byte(realisticRelease()))
	if err != nil {
		t.Fatal(err)
	}
	corrupt, err := env.adopter.writeBlobBytes(context.Background(), []byte("an anchor that will rot"))
	if err != nil {
		t.Fatal(err)
	}
	rotBlob(t, c, corrupt)
	absent := hashOf([]byte("never written"))
	for _, tc := range []struct {
		hash   string
		reason string
		drop   bool
	}{{corrupt, "anchor_corrupt", true}, {absent, "anchor_missing", false}, {plain, "legacy_plaintext", false}, {good, "", false}} {
		h := tc.hash
		v, err := auditLegacySnapshot(c, cache.LegacyAuditItem{Form: cache.FormInline, SingleAnchorHash: &h})
		if err != nil || v.reason != tc.reason || v.dropAnchorRows != tc.drop {
			t.Errorf("%q: verdict %+v err=%v, want reason %q dropAnchorRows %v", tc.reason, v, err, tc.reason, tc.drop)
		}
	}
	prevCap := legacyAuditAnchorCap
	legacyAuditAnchorCap = 16
	t.Cleanup(func() { legacyAuditAnchorCap = prevCap })
	if v, err := auditLegacySnapshot(c, cache.LegacyAuditItem{Form: cache.FormInline, SingleAnchorHash: &good}); err != nil ||
		v.reason != "anchor_oversized" || v.dropAnchorRows {
		t.Errorf("oversized: verdict %+v err=%v, want anchor_oversized without dropAnchorRows", v, err)
	}
}

// Ruling R-H2 end to end in the audit: two pre-v7 inline suites, one
// whose stored InRelease rotted in the pool and one whose InRelease is
// over the read cap. Both are quarantined; the rotted one's url_path
// anchor row is deleted (anchor_rows_deleted 1 on its line) and its pool
// file removed (review C-1), so the next client request fetches a fresh
// copy; the oversized one, not proven bad, keeps its row and its pool file
// (anchor_rows_deleted 0).
func TestRunLegacyTrustAudit_CorruptAnchorDropsItsRowsOversizedKeepsThem(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ctx := context.Background()
	c := env.cache
	rotten := adoptLegacy(t, env, "/r/dists/rotten", clearsignForTest(t, realisticRelease()+"Note: rotten\n"), "/r/pool/r_1_amd64.deb")
	big := adoptLegacy(t, env, "/r/dists/big", clearsignForTest(t, realisticRelease()+"Note: big\n"), "/r/pool/b_1_amd64.deb")
	snap, err := c.GetSuiteSnapshot(ctx, rotten)
	if err != nil {
		t.Fatal(err)
	}
	rotBlob(t, c, *snap.SingleAnchorHash)
	bigSnap, err := c.GetSuiteSnapshot(ctx, big)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ClearSignedSeenForTest(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.EnqueueLegacyTrustAudit(ctx, rotten, big); err != nil {
		t.Fatal(err)
	}
	prevCap := legacyAuditAnchorCap
	legacyAuditAnchorCap = 1024 // the rotted file is shorter; the big InRelease is longer
	t.Cleanup(func() { legacyAuditAnchorCap = prevCap })
	logs := &lockedBuffer{}
	res, err := RunLegacyTrustAudit(ctx, c, slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil || res.Quarantined != 2 {
		t.Fatalf("audit = %+v, %v; want both quarantined", res, err)
	}
	if _, err := c.LookupURL(ctx, "http", "h", "/r/dists/rotten/InRelease"); !errors.Is(err, cache.ErrNotFound) {
		t.Fatalf("the rotted anchor's row: %v, want deleted", err)
	}
	if u, err := c.LookupURL(ctx, "http", "h", "/r/dists/big/InRelease"); err != nil || u.BlobHash == nil {
		t.Fatalf("the oversized anchor's row: %+v, %v; want kept", u, err)
	}
	if _, err := os.Stat(c.BlobPath(*snap.SingleAnchorHash)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the rotted anchor's pool file: %v, want removed", err)
	}
	if _, err := os.Stat(c.BlobPath(*bigSnap.SingleAnchorHash)); err != nil {
		t.Fatalf("the oversized anchor's pool file: %v, want kept", err)
	}
	want := map[string]float64{"anchor_corrupt": 1, "anchor_oversized": 0}
	lines := logRecords(t, logs, "legacy_snapshot_quarantined")
	for _, m := range lines {
		n, ok := want[m["reason"].(string)]
		if !ok || m["anchor_rows_deleted"] != n {
			t.Errorf("line %v, want anchor_rows_deleted %v for its reason", m, n)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("%d quarantine lines, want 2", len(lines))
	}
}

// Ruling R-H2, review C-1: a pool file the audit cannot remove is logged
// (ERROR legacy_anchor_discard_failed) and the quarantine proceeds: its
// url_path rows still go, so a changed upstream still heals it. The
// removal comes after the batch commits (re-review N-1): the seam sees
// the rows already deleted.
func TestRunLegacyTrustAudit_AFailedDiscardIsLoggedAndTheQuarantineProceeds(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ctx := context.Background()
	c := env.cache
	rotten := adoptLegacy(t, env, "/r/dists/rotten", clearsignForTest(t, realisticRelease()+"Note: rotten\n"), "/r/pool/r_1_amd64.deb")
	snap, err := c.GetSuiteSnapshot(ctx, rotten)
	if err != nil {
		t.Fatal(err)
	}
	hash := *snap.SingleAnchorHash
	rotBlob(t, c, hash)
	if err := c.ClearSignedSeenForTest(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.EnqueueLegacyTrustAudit(ctx, rotten); err != nil {
		t.Fatal(err)
	}
	prev := legacyDiscardAnchor
	legacyDiscardAnchor = func(c *cache.Cache, _ string) error {
		// Re-review N-1: the removal comes after the batch commits, so the
		// quarantine's row delete is already visible.
		if _, err := c.LookupURL(ctx, "http", "h", "/r/dists/rotten/InRelease"); !errors.Is(err, cache.ErrNotFound) {
			t.Error("pool file removed before the batch committed")
		}
		return errors.New("pool is read-only")
	}
	t.Cleanup(func() { legacyDiscardAnchor = prev })
	logs := &lockedBuffer{}
	res, err := RunLegacyTrustAudit(ctx, c, slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil || res.Quarantined != 1 {
		t.Fatalf("audit = %+v, %v; want the rotted snapshot quarantined", res, err)
	}
	if _, err := c.LookupURL(ctx, "http", "h", "/r/dists/rotten/InRelease"); !errors.Is(err, cache.ErrNotFound) {
		t.Fatalf("the rotted anchor's row: %v, want deleted", err)
	}
	recs := logRecords(t, logs, "legacy_anchor_discard_failed")
	if len(recs) != 1 || recs[0]["level"] != "ERROR" || recs[0]["blob_hash"] != hash ||
		recs[0]["err"] != "pool is read-only" || recs[0]["snapshot_id"] != float64(rotten) {
		t.Fatalf("records %v, want one ERROR naming the snapshot, the blob and the error", recs)
	}
}

// Ruling R-H2 (execution review G-3): an inline item without a stored
// InRelease hash is anchor_missing and asks for no row drop.
// RunLegacyTrustAudit's post-commit loop dereferences SingleAnchorHash to
// remove the pool file whenever DropAnchorRows is set, so a drop asked for
// here would also be a nil dereference on main's goroutine before anything
// serves.
func TestAuditLegacySnapshot_NoAnchorHashAsksForNoDrop(t *testing.T) {
	env := newAdoptionTestEnv(t)
	v, err := auditLegacySnapshot(env.cache, cache.LegacyAuditItem{Form: cache.FormInline, SingleAnchorHash: nil})
	if err != nil || v.outcome != cache.LegacyAuditQuarantine || v.reason != "anchor_missing" || v.dropAnchorRows {
		t.Fatalf("verdict %+v, err %v; want an anchor_missing quarantine without dropAnchorRows", v, err)
	}
}
