package gc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// Spec C9 sub-job C and §7 "blobs reclaimed within two periodic GC ticks":
// a suite reaped (or forgotten) leaves retired snapshots — here two, the
// current one and the one it displaced, so the pass must loop past its
// first deleted header (the Task 4 review's minor 1). The startup pass,
// which runs before any listener serves, leaves them and their blobs
// (ruling PF-15: no sub-job C there, retired_reaped 0), still runs
// sub-jobs A and B — a candidate whose heartbeat went stale, and another
// suite's fourth displaced snapshot beyond keep_displaced = 3, are deleted
// (the final fix wave's re-review, gc-sims minor 2) — and still runs its
// blob pass, which reclaims an unreferenced orphan blob. Since that pass
// finishes every pass (none reaches its deadline), the first periodic
// tick's snapshot pass deletes the retired snapshots (retired_reaped in
// its gc_run_complete and LastRunSummary) and the same tick's blob pass
// reclaims their blobs once blob_grace has passed.
func TestRunTick_SubJobCReapsRetiredSnapshotsAndTheirBlobs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := openTestCache(t)
		logger, buf := captureLogger()
		db := dbOf(t, c)
		ctx := context.Background()
		oldAnchor, oldMember := putPoolBlob(t, c, "old anchor"), putPoolBlob(t, c, "old member")
		old := adoptSnapshot(t, c, "http", "h", "/r/dists/x", oldAnchor, []string{oldMember})
		anchor := putPoolBlob(t, c, "anchor")
		b1, b2 := putPoolBlob(t, c, "member one"), putPoolBlob(t, c, "member two")
		id := adoptSnapshot(t, c, "http", "h", "/r/dists/x", anchor, []string{b1, b2})
		key := cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}
		if _, err := c.DeleteSuiteMetadata(ctx, key); err != nil { // the reaper's step 1: the seeded anchor row
			t.Fatal(err)
		}
		if res, err := c.ForgetSuite(ctx, key, &id); err != nil || !res.Deleted {
			t.Fatalf("ForgetSuite = %+v, %v", res, err)
		}
		blobs := []string{oldAnchor, oldMember, anchor, b1, b2}
		orphan := putPoolBlob(t, c, "an orphan no row references")
		for _, h := range append(blobs, orphan) {
			fastForwardZeroedAt(t, db, h)
		}
		staleAnchor := putPoolBlob(t, c, "a stale candidate's anchor")
		stale, _, err := c.InsertTrackedCandidateForTest(ctx, cache.SnapshotCandidate{CanonicalScheme: "http",
			CanonicalHost: "h", SuitePath: "/r/dists/y", InReleaseHash: &staleAnchor})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE suite_snapshot SET heartbeat_at = 1 WHERE snapshot_id = ?`, stale); err != nil {
			t.Fatal(err)
		}
		var displaced int64 // the oldest of five: beyond the current one and keep_displaced = 3
		for i := range 5 {
			if id := adoptSnapshot(t, c, "http", "h", "/r/dists/z", putPoolBlob(t, c, fmt.Sprintf("z anchor %d", i)), nil); i == 0 {
				displaced = id
			}
		}
		g, err := New(Config{Cache: c, Logger: logger, Enabled: true, Interval: time.Hour, BatchSize: 100,
			SnapshotBatchSize: 10, MaxTickDuration: time.Minute, BlobGrace: time.Minute, KeepDisplaced: 3,
			PoolScanWorkers: 2, HeartbeatStaleGrace: 30 * time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		if err := g.StartupPass(ctx); err != nil {
			t.Fatal(err)
		}
		if !snapshotRowExists(t, db, old) || !snapshotRowExists(t, db, id) {
			t.Fatalf("the startup pass ran sub-job C (displaced kept %v, current kept %v)", snapshotRowExists(t, db, old),
				snapshotRowExists(t, db, id))
		}
		for _, h := range blobs {
			if !blobRowExists(t, db, h) || !fileExists(t, c.BlobPath(h)) {
				t.Fatalf("blob %s of a retired snapshot reclaimed by the startup pass", h)
			}
		}
		if blobRowExists(t, db, orphan) || fileExists(t, c.BlobPath(orphan)) {
			t.Fatal("the startup pass kept an orphan blob: ending the snapshot pass before sub-job C skipped its blob pass")
		}
		if s, ok := g.LastRunSummary(); !ok || s.Phase != "startup" || s.RetiredReaped != 0 ||
			s.OrphanCandidatesReaped != 1 || s.DisplacedReaped != 1 {
			t.Fatalf("startup LastRunSummary = %+v, %v; want retired_reaped 0, and sub-jobs A and B run "+
				"(orphan_candidates_reaped 1, displaced_reaped 1)", s, ok)
		}
		if snapshotRowExists(t, db, stale) || snapshotRowExists(t, db, displaced) {
			t.Fatalf("after the startup pass: stale candidate kept %v, displaced snapshot beyond keep_displaced kept %v; "+
				"want both deleted (sub-jobs A and B)", snapshotRowExists(t, db, stale), snapshotRowExists(t, db, displaced))
		}
		life, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { g.Run(life); close(done) }()
		time.Sleep(time.Hour + time.Second) // the first periodic tick
		cancel()
		<-done
		if snapshotRowExists(t, db, old) || snapshotRowExists(t, db, id) {
			t.Fatalf("a retired snapshot survived the first periodic tick (displaced %v, current %v)",
				snapshotRowExists(t, db, old), snapshotRowExists(t, db, id))
		}
		for _, h := range blobs {
			if blobRowExists(t, db, h) || fileExists(t, c.BlobPath(h)) {
				t.Fatalf("blob %s survived the tick that deleted its retired snapshot", h)
			}
		}
		if s, ok := g.LastRunSummary(); !ok || s.Phase != "periodic" || s.RetiredReaped != 2 {
			t.Fatalf("LastRunSummary = %+v, %v; want the periodic tick's, retired_reaped 2", s, ok)
		}
		got := map[any]any{}
		for _, l := range strings.Split(buf.String(), "\n") {
			var line map[string]any
			if strings.Contains(l, `"msg":"gc_run_complete"`) && json.Unmarshal([]byte(l), &line) == nil {
				got[line["phase"]] = line["retired_reaped"]
			}
		}
		if got["startup"] != float64(0) || got["periodic"] != float64(2) {
			t.Fatalf("gc_run_complete retired_reaped by phase = %v; want startup 0, periodic 2", got)
		}
	})
}

// Deferred minor A-3: the GC's run stamps (gc_run_complete's
// acu_gc_last_run_unixtime and LastRunSummary.AtUnixTime, at startup and
// on a periodic tick) come from the shared wall clock, which simulations
// step, not from time.Now.
func TestGC_StampsFollowTheWallClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		restore := wallclock.SetOffsetForTest(48 * time.Hour)
		defer restore()
		c := openTestCache(t)
		logger, _ := captureLogger()
		g, err := New(Config{Cache: c, Logger: logger, Enabled: true, Interval: time.Hour, BatchSize: 100,
			SnapshotBatchSize: 10, MaxTickDuration: time.Minute, BlobGrace: time.Minute, KeepDisplaced: 3,
			PoolScanWorkers: 2, HeartbeatStaleGrace: 30 * time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		if err := g.StartupPass(context.Background()); err != nil {
			t.Fatal(err)
		}
		if s, _ := g.LastRunSummary(); s.AtUnixTime != time.Now().Add(48*time.Hour).Unix() {
			t.Fatalf("startup stamp = %d, want the wall clock's %d", s.AtUnixTime, time.Now().Add(48*time.Hour).Unix())
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { g.Run(ctx); close(done) }()
		time.Sleep(time.Hour + time.Second)
		cancel()
		<-done
		if s, _ := g.LastRunSummary(); s.Phase != "periodic" || s.AtUnixTime != time.Now().Add(48*time.Hour-time.Second).Unix() {
			t.Fatalf("periodic summary = %+v; want the wall clock's stamp at the tick, %d", s, time.Now().Add(48*time.Hour-time.Second).Unix())
		}
	})
}
