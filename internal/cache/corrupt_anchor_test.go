package cache

import (
	"context"
	"errors"
	"os"
	"testing"
)

// putAnchorRow stores a url_path metadata row at scheme://host+path whose
// blob is hash.
func putAnchorRow(t *testing.T, c *Cache, scheme, host, path, hash string) {
	t.Helper()
	if err := c.PutURLPath(context.Background(), URLPath{CanonicalScheme: scheme, CanonicalHost: host, Path: path,
		BlobHash: &hash, UpstreamURL: scheme + "://" + host + path, IsMetadata: true}); err != nil {
		t.Fatal(err)
	}
}

// urlPathGone reports whether scheme://host+path has no url_path row.
func urlPathGone(t *testing.T, c *Cache, scheme, host, path string) bool {
	t.Helper()
	_, err := c.LookupURL(context.Background(), scheme, host, path)
	if err != nil && !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	return err != nil
}

// Ruling R-H2, Review Focus 1: an anchor_corrupt quarantine
// (DropAnchorRows) deletes, in its own transaction, every url_path row
// whose blob is the snapshot's InRelease — the row CommitAdoption seeded,
// the same path over https, another host's row of the same bytes — each
// row's pin released once, and leaves another blob's row, the blob row
// and the pool file alone (the audit removes it). A repeat finds nothing to do.
func TestApplyLegacyAuditDecisions_CorruptAnchorDropsEveryRowOfItsBlob(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, 1_800_000_000)()
	anchor := seedBlob(t, c, "a corrupt anchor")
	id := commitInline(t, c, "/r/dists/q", anchor)
	putAnchorRow(t, c, "https", "h", "/r/dists/q/InRelease", anchor)
	putAnchorRow(t, c, "http", "mirror", "/r/dists/q/InRelease", anchor)
	other := seedBlob(t, c, "another anchor")
	putAnchorRow(t, c, "http", "h", "/r/dists/other/InRelease", other)
	if err := c.EnqueueLegacyTrustAudit(ctx, id); err != nil {
		t.Fatal(err)
	}
	before := refcount(t, c, anchor)
	effects, err := c.ApplyLegacyAuditDecisions(ctx, []LegacyAuditDecision{
		{SnapshotID: id, Outcome: LegacyAuditQuarantine, DropAnchorRows: true}})
	if err != nil {
		t.Fatal(err)
	}
	if got := effects[id]; got != (LegacyAuditEffect{WasCurrent: true, AnchorRowsDeleted: 3}) {
		t.Fatalf("effect %+v, want current and 3 rows deleted", got)
	}
	for _, r := range [][3]string{{"http", "h", "/r/dists/q/InRelease"}, {"https", "h", "/r/dists/q/InRelease"},
		{"http", "mirror", "/r/dists/q/InRelease"}} {
		if !urlPathGone(t, c, r[0], r[1], r[2]) {
			t.Errorf("%s://%s%s kept, want deleted", r[0], r[1], r[2])
		}
	}
	if urlPathGone(t, c, "http", "h", "/r/dists/other/InRelease") {
		t.Fatal("another blob's row was deleted")
	}
	if got := refcount(t, c, anchor); got != before-4 {
		t.Fatalf("refcount %d, want %d (the current snapshot's pin and one per row, each released once)", got, before-4)
	}
	if _, err := os.Stat(c.BlobPath(anchor)); err != nil {
		t.Fatalf("pool file: %v, want kept (the cache does no pool I/O; the audit removes it)", err)
	}
	again, err := c.ApplyLegacyAuditDecisions(ctx, []LegacyAuditDecision{
		{SnapshotID: id, Outcome: LegacyAuditQuarantine, DropAnchorRows: true}})
	if err != nil || again[id] != (LegacyAuditEffect{}) || refcount(t, c, anchor) != before-4 {
		t.Fatalf("repeat: %+v, %v, refcount %d; want no effect", again[id], err, refcount(t, c, anchor))
	}
}

// Design note 2: DropAnchorRows belongs to a quarantine. Set with any
// other outcome it rolls the batch back: nothing dequeued, nothing
// marked, no row deleted.
func TestApplyLegacyAuditDecisions_DropAnchorRowsNeedsAQuarantine(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	anchor := seedBlob(t, c, "an anchor kept")
	id := commitInline(t, c, "/r/dists/k", anchor)
	if _, err := c.db.Exec(`DELETE FROM suite_signed_seen`); err != nil {
		t.Fatal(err)
	}
	if err := c.EnqueueLegacyTrustAudit(ctx, id); err != nil {
		t.Fatal(err)
	}
	for _, o := range []LegacyAuditOutcome{LegacyAuditKeep, LegacyAuditDrop} {
		if _, err := c.ApplyLegacyAuditDecisions(ctx, []LegacyAuditDecision{{SnapshotID: id, Outcome: o, DropAnchorRows: true}}); err == nil {
			t.Fatalf("outcome %d with DropAnchorRows: no error", o)
		}
	}
	if items, err := c.LegacyAuditBatch(ctx, 0, 0); err != nil || len(items) != 1 {
		t.Fatalf("queue %+v (%v), want the snapshot still queued", items, err)
	}
	_, ok, err := c.SignedSeen(ctx, "http", "h", "/r/dists/k")
	if err != nil {
		t.Fatal(err)
	}
	if ok || urlPathGone(t, c, "http", "h", "/r/dists/k/InRelease") {
		t.Fatal("a rolled-back batch marked the base or deleted the anchor row")
	}
}

// Ruling R-H2, the quarantine's own transaction (execution review G-1): the
// anchor-row drop and the quarantine commit or roll back together. A drop
// that fails — here a trigger refuses every url_path delete — leaves the
// batch unapplied: the snapshot still queued (the next start audits it
// again), still current, not retired, its pins not released, and its
// anchor row kept. A drop run in a later transaction would leave the
// snapshot dequeued and retired while its rows served the corrupt bytes
// for good.
func TestApplyLegacyAuditDecisions_AFailedDropUndoesTheQuarantine(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, 1_800_000_000)()
	anchor := seedBlob(t, c, "a corrupt anchor, kept by a failed drop")
	id := commitInline(t, c, "/r/dists/q", anchor)
	if err := c.EnqueueLegacyTrustAudit(ctx, id); err != nil {
		t.Fatal(err)
	}
	before := refcount(t, c, anchor)
	if _, err := c.db.Exec(`CREATE TRIGGER refuse_url_path_delete BEFORE DELETE ON url_path
BEGIN SELECT RAISE(ABORT, 'injected url_path delete failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ApplyLegacyAuditDecisions(ctx, []LegacyAuditDecision{
		{SnapshotID: id, Outcome: LegacyAuditQuarantine, DropAnchorRows: true}}); err == nil {
		t.Fatal("a failed anchor-row drop returned no error")
	}
	if items, err := c.LegacyAuditBatch(ctx, 0, 0); err != nil || len(items) != 1 || items[0].SnapshotID != id {
		t.Fatalf("queue %+v (%v), want snapshot %d still queued", items, err, id)
	}
	sf, err := c.GetSuiteFreshness(ctx, "http", "h", "/r/dists/q")
	if err != nil || sf.CurrentSnapshotID == nil || *sf.CurrentSnapshotID != id {
		t.Fatalf("current snapshot %v (%v), want %d: the quarantine must roll back with the drop", sf.CurrentSnapshotID, err, id)
	}
	var retired int
	if err := c.db.QueryRow(`SELECT retired_at IS NOT NULL FROM suite_snapshot WHERE snapshot_id = ?`, id).Scan(&retired); err != nil || retired != 0 {
		t.Fatalf("snapshot retired = %d (%v), want not retired", retired, err)
	}
	if got := refcount(t, c, anchor); got != before {
		t.Fatalf("refcount %d, want %d: no pin released by a rolled-back batch", got, before)
	}
	if urlPathGone(t, c, "http", "h", "/r/dists/q/InRelease") {
		t.Fatal("the anchor row is gone after a rolled-back batch")
	}
}

// The H1 final review's prod minor 3: LegacyAuditQueueNamesAnchor answers
// true only while a snapshot still queued for the audit stores hash as its
// InRelease. A snapshot not queued, another hash, or one the audit has
// dequeued answers false; a closed cache answers its error.
func TestLegacyAuditQueueNamesAnchor(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, 1_800_000_000)()
	anchor := seedBlob(t, c, "a shared anchor")
	first := commitInline(t, c, "/r/dists/one", anchor)
	second := commitInline(t, c, "/r/dists/two", anchor)
	other := seedBlob(t, c, "another anchor")
	ask := func(hash string, want bool) {
		t.Helper()
		if got, err := c.LegacyAuditQueueNamesAnchor(ctx, hash); err != nil || got != want {
			t.Fatalf("LegacyAuditQueueNamesAnchor(%s) = %v, %v; want %v", hash, got, err, want)
		}
	}
	ask(anchor, false) // nothing queued
	if err := c.EnqueueLegacyTrustAudit(ctx, first, second); err != nil {
		t.Fatal(err)
	}
	ask(anchor, true)
	ask(other, false)
	if _, err := c.ApplyLegacyAuditDecisions(ctx, []LegacyAuditDecision{
		{SnapshotID: first, Outcome: LegacyAuditQuarantine, DropAnchorRows: true}}); err != nil {
		t.Fatal(err)
	}
	ask(anchor, true) // second is still queued
	if _, err := c.ApplyLegacyAuditDecisions(ctx, []LegacyAuditDecision{
		{SnapshotID: second, Outcome: LegacyAuditQuarantine, DropAnchorRows: true}}); err != nil {
		t.Fatal(err)
	}
	ask(anchor, false)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.LegacyAuditQueueNamesAnchor(ctx, anchor); err == nil {
		t.Fatal("a closed cache answered no error")
	}
}
