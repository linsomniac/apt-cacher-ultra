package freshness

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
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
		reason, _ := m["reason"].(string)
		n, ok := want[reason]
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
// the rows already deleted. Its ERROR follows the quarantine's WARN (the
// H1 final review's ops M-6), so the log reads in order.
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
		recs[0]["err"] != "pool is read-only" || recs[0]["snapshot_id"] != float64(rotten) ||
		recs[0]["canonical_host"] != "h" || recs[0]["suite_path"] != "/r/dists/rotten" {
		t.Fatalf("records %v, want one ERROR naming the snapshot, the blob and the error", recs)
	}
	out := logs.String()
	if q, f := strings.Index(out, `"msg":"legacy_snapshot_quarantined"`), strings.Index(out, `"msg":"legacy_anchor_discard_failed"`); q < 0 || q > f {
		t.Fatalf("legacy_snapshot_quarantined at %d, legacy_anchor_discard_failed at %d; want the quarantine line first", q, f)
	}
}

// Design note 2: two snapshots of one batch whose stored InRelease is the
// same rotted pool file (two suites caching the same bytes) are both
// anchor_corrupt, since both read the file before any removal. The first
// quarantine's drop takes every row of the blob (anchor_rows_deleted 2),
// the second's finds none left (0); the first removal deletes the file
// and the second finds it gone, which DiscardFinalizedBlob ignores, so
// nothing logs legacy_anchor_discard_failed.
func TestRunLegacyTrustAudit_TwoSnapshotsSharingARottedAnchorInOneBatch(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ctx := context.Background()
	c := env.cache
	shared := clearsignForTest(t, realisticRelease()+"Note: shared\n")
	first := adoptLegacy(t, env, "/r/dists/one", shared, "/r/pool/o_1_amd64.deb")
	second := adoptLegacy(t, env, "/r/dists/two", shared, "/r/pool/t_1_amd64.deb")
	snap, err := c.GetSuiteSnapshot(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	hash := *snap.SingleAnchorHash
	if snap2, err := c.GetSuiteSnapshot(ctx, second); err != nil || snap2.SingleAnchorHash == nil || *snap2.SingleAnchorHash != hash {
		t.Fatalf("second snapshot %+v, %v; want the same InRelease blob %s", snap2, err, hash)
	}
	rotBlob(t, c, hash)
	if err := c.ClearSignedSeenForTest(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.EnqueueLegacyTrustAudit(ctx, first, second); err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuffer{}
	res, err := RunLegacyTrustAudit(ctx, c, slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil || res.Quarantined != 2 {
		t.Fatalf("audit = %+v, %v; want both quarantined", res, err)
	}
	for _, p := range []string{"/r/dists/one/InRelease", "/r/dists/two/InRelease"} {
		if _, err := c.LookupURL(ctx, "http", "h", p); !errors.Is(err, cache.ErrNotFound) {
			t.Errorf("%s: %v, want deleted", p, err)
		}
	}
	if _, err := os.Stat(c.BlobPath(hash)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the shared rotted pool file: %v, want removed", err)
	}
	want := map[float64]float64{float64(first): 2, float64(second): 0}
	lines := logRecords(t, logs, "legacy_snapshot_quarantined")
	for _, m := range lines {
		id, _ := m["snapshot_id"].(float64)
		n, ok := want[id]
		if !ok || m["reason"] != "anchor_corrupt" || m["anchor_rows_deleted"] != n {
			t.Errorf("line %v, want reason anchor_corrupt and anchor_rows_deleted %v", m, n)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("%d quarantine lines, want 2", len(lines))
	}
	if recs := logRecords(t, logs, "legacy_anchor_discard_failed"); len(recs) != 0 {
		t.Fatalf("records %v, want none: the second removal finds the file already gone", recs)
	}
}

// The H1 final review's prod minor 3: two queued snapshots sharing one
// rotted InRelease in different batches. The first batch's quarantine
// deletes every row of the blob but leaves the file, since the second
// snapshot, still queued, names it: removed then, a lost commit of the
// second batch would leave that snapshot queued with its anchor missing,
// and when every queued inline anchor is missing the next start refuses
// ("is the pool mounted?"). The second snapshot's own audit finds the
// file corrupt, quarantines it and removes the file.
func TestRunLegacyTrustAudit_TwoSnapshotsSharingARottedAnchorAcrossBatches(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ctx := context.Background()
	c := env.cache
	shared := clearsignForTest(t, realisticRelease()+"Note: shared\n")
	first := adoptLegacy(t, env, "/r/dists/one", shared, "/r/pool/o_1_amd64.deb")
	second := adoptLegacy(t, env, "/r/dists/two", shared, "/r/pool/t_1_amd64.deb")
	snap, err := c.GetSuiteSnapshot(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	hash := *snap.SingleAnchorHash
	if snap2, err := c.GetSuiteSnapshot(ctx, second); err != nil || snap2.SingleAnchorHash == nil || *snap2.SingleAnchorHash != hash {
		t.Fatalf("second snapshot %+v, %v; want the same InRelease blob %s", snap2, err, hash)
	}
	rotBlob(t, c, hash)
	if err := c.ClearSignedSeenForTest(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.EnqueueLegacyTrustAudit(ctx, first, second); err != nil {
		t.Fatal(err)
	}
	prevSize, prevAfter := legacyAuditBatchSize, legacyAuditAfterBatch
	t.Cleanup(func() { legacyAuditBatchSize, legacyAuditAfterBatch = prevSize, prevAfter })
	legacyAuditBatchSize = 1
	legacyAuditAfterBatch = func(n int) error {
		if _, err := os.Stat(c.BlobPath(hash)); n == 1 && err != nil {
			t.Errorf("after the first batch the shared pool file: %v, want kept for the queued second snapshot", err)
		}
		return nil
	}
	logs := &lockedBuffer{}
	res, err := RunLegacyTrustAudit(ctx, c, slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil || res.Quarantined != 2 {
		t.Fatalf("audit = %+v, %v; want both quarantined", res, err)
	}
	for _, p := range []string{"/r/dists/one/InRelease", "/r/dists/two/InRelease"} {
		if _, err := c.LookupURL(ctx, "http", "h", p); !errors.Is(err, cache.ErrNotFound) {
			t.Errorf("%s: %v, want deleted", p, err)
		}
	}
	if _, err := os.Stat(c.BlobPath(hash)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the shared rotted pool file: %v, want removed after the second batch", err)
	}
	want := map[float64]float64{float64(first): 2, float64(second): 0}
	lines := logRecords(t, logs, "legacy_snapshot_quarantined")
	for _, m := range lines {
		id, _ := m["snapshot_id"].(float64)
		n, ok := want[id]
		if !ok || m["reason"] != "anchor_corrupt" || m["anchor_rows_deleted"] != n {
			t.Errorf("line %v, want reason anchor_corrupt and anchor_rows_deleted %v", m, n)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("%d quarantine lines, want 2", len(lines))
	}
	if recs := logRecords(t, logs, "legacy_anchor_discard_failed"); len(recs) != 0 {
		t.Fatalf("records %v, want none", recs)
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
