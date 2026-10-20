package cache

import (
	"context"
	"fmt"
	"testing"
)

// retire stamps retired_at on snapshot id (what a reap, a /forget or a
// legacy-audit quarantine does).
func retire(t *testing.T, c *Cache, id int64) {
	t.Helper()
	if _, err := c.db.Exec(`UPDATE suite_snapshot SET retired_at = 1 WHERE snapshot_id = ?`, id); err != nil {
		t.Fatal(err)
	}
}

// childRows counts snapshot id's snapshot_member, snapshot_skipped_member
// and package_hash rows.
func childRows(t *testing.T, c *Cache, id int64) int {
	t.Helper()
	var n int
	if err := c.db.QueryRow(`SELECT
  (SELECT COUNT(*) FROM snapshot_member WHERE snapshot_id = ?) +
  (SELECT COUNT(*) FROM snapshot_skipped_member WHERE snapshot_id = ?) +
  (SELECT COUNT(*) FROM package_hash WHERE snapshot_id = ?)`, id, id, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Spec C9 (phase A carry-forward): retired snapshots take no
// keep_displaced slot. With the current snapshot id5 and two retired
// displaced ones (id3, id4), keep_displaced = 2 keeps the two newest
// non-retired displaced snapshots (id2, id1) — before phase F the retired
// pair took both slots and sub-job B reaped id1 and id2.
func TestRunSnapshotGCBatch_RetiredSnapshotsTakeNoKeepDisplacedSlot(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	var ids []int64
	for i := 1; i <= 5; i++ {
		ids = append(ids, adoptCandidate(t, c, "http", "kd.example", "/p", seedBlob(t, c, fmt.Sprintf("kd r%d", i)), int64(i)*1000))
	}
	setCurrentSnapshot(t, c, "http", "kd.example", "/p", ids[4])
	retire(t, c, ids[2])
	retire(t, c, ids[3])
	defer stubNow(t, 1_000_000)()
	res, err := c.RunSnapshotGCBatch(ctx, 100, 1800, 2)
	if err != nil {
		t.Fatal(err)
	}
	if res.DisplacedReaped != 0 || res.OrphanReaped != 0 {
		t.Fatalf("sub-job B reaped %+v; want nothing (the retired snapshots are not ranked)", res)
	}
	for _, id := range ids {
		if !snapshotExists(t, c, id) {
			t.Fatalf("snapshot %d was reaped by sub-job B", id)
		}
	}
}

// Spec C9 sub-job C: retired snapshots are deleted with their members,
// skipped members and package_hash rows in bounded chunks, then the header
// row; a non-retired snapshot and the current one are untouched, and the
// sub-job says when nothing retired is left.
func TestRunRetiredSnapshotGCChunk_DeletesRetiredSnapshotsInChunks(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/r/dists/x")
	seedRow(t, c, key, 1)
	members := map[string]string{"InRelease": "old anchor"}
	for i := range 12 {
		members[fmt.Sprintf("main/i%02d", i)] = fmt.Sprintf("index %d", i)
	}
	old, hashes := adoptWith(t, c, key, members)
	for i := range 9 {
		seedPackageHash(t, c, "http", "h", fmt.Sprintf("/r/pool/p%d.deb", i), hashes["main/i00"], old, "p", "amd64", "1")
	}
	if _, err := c.db.Exec(`INSERT INTO snapshot_skipped_member (snapshot_id, path, declared_sha256, size, reason, skipped_at)
VALUES (?, 'main/skipped', ?, 1, '4xx', 1)`, old, hashes["main/i01"]); err != nil {
		t.Fatal(err)
	}
	cur, _ := adoptWith(t, c, key, map[string]string{"InRelease": "new anchor"})
	retire(t, c, old)
	if n := childRows(t, c, old); n != 13+9+1 {
		t.Fatalf("premise: %d child rows, want 23", n)
	}
	// Chunks of 10: two full chunks (20 rows), then the last 3 and the header.
	for i, want := range []struct {
		reaped, left int
	}{{0, 13}, {0, 3}, {1, 0}} {
		reaped, more, err := c.RunRetiredSnapshotGCChunk(ctx, 10)
		if err != nil || !more || reaped != want.reaped || childRows(t, c, old) != want.left || snapshotExists(t, c, old) != (want.reaped == 0) {
			t.Fatalf("chunk %d: reaped %d, more %v, err %v, %d rows left, header %v; want reaped %d and %d rows left",
				i+1, reaped, more, err, childRows(t, c, old), snapshotExists(t, c, old), want.reaped, want.left)
		}
	}
	if reaped, more, err := c.RunRetiredSnapshotGCChunk(ctx, 10); err != nil || more || reaped != 0 {
		t.Fatalf("with nothing retired left: reaped %d, more %v, err %v; want 0, false", reaped, more, err)
	}
	if !snapshotExists(t, c, cur) || childRows(t, c, cur) != 1 {
		t.Fatal("the current snapshot or its member was deleted")
	}
}

// Spec C9 / §6.1: sub-job C deletes the snapshots the legacy trust audit
// quarantined (retired), members and package_hash rows included.
func TestRunRetiredSnapshotGCChunk_DeletesAuditRetiredSnapshots(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/r/dists/x")
	seedRow(t, c, key, 1)
	id, hashes := adoptWith(t, c, key, map[string]string{"InRelease": "polluted", "main/Packages": "pkgs"})
	seedPackageHash(t, c, "http", "h", "/r/pool/p.deb", hashes["main/Packages"], id, "p", "amd64", "1")
	if _, err := c.ApplyLegacyAuditDecisions(ctx, []LegacyAuditDecision{{SnapshotID: id, Outcome: LegacyAuditQuarantine}}); err != nil {
		t.Fatal(err)
	}
	for {
		_, more, err := c.RunRetiredSnapshotGCChunk(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			break
		}
	}
	if snapshotExists(t, c, id) || childRows(t, c, id) != 0 {
		t.Fatalf("the quarantined snapshot: header %v, %d child rows; want both gone", snapshotExists(t, c, id), childRows(t, c, id))
	}
}

// Spec C9: sub-job C's chunk is 10,000 rows per writer transaction, a
// constant (exec review X29; a test that fills a chunk takes seconds under
// -race, so the constant itself is pinned).
func TestRunRetiredSnapshotGCChunk_TheChunkIsTenThousandRows(t *testing.T) {
	if retiredChunkRows != 10_000 {
		t.Fatalf("retiredChunkRows = %d, want spec C9's 10,000", retiredChunkRows)
	}
}
