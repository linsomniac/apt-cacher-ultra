package gc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// Spec C9 sub-job C and §7 "blobs reclaimed within two GC ticks": a suite
// reaped (or forgotten) leaves retired snapshots; the next tick's snapshot
// pass deletes them (retired_reaped in gc_run_complete and LastRunSummary)
// and the same tick's blob pass reclaims their blobs once blob_grace has
// passed.
func TestRunTick_SubJobCReapsRetiredSnapshotsAndTheirBlobs(t *testing.T) {
	c := openTestCache(t)
	logger, buf := captureLogger()
	db := dbOf(t, c)
	ctx := context.Background()
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
	for _, h := range []string{anchor, b1, b2} {
		fastForwardZeroedAt(t, db, h)
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
	if snapshotRowExists(t, db, id) {
		t.Fatal("the retired snapshot survived the tick")
	}
	for _, h := range []string{anchor, b1, b2} {
		if blobRowExists(t, db, h) || fileExists(t, c.BlobPath(h)) {
			t.Fatalf("blob %s survived the tick that deleted its retired snapshot", h)
		}
	}
	if s, ok := g.LastRunSummary(); !ok || s.RetiredReaped != 1 {
		t.Fatalf("LastRunSummary = %+v, %v; want retired_reaped 1", s, ok)
	}
	var line map[string]any
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, `"msg":"gc_run_complete"`) && json.Unmarshal([]byte(l), &line) == nil {
			break
		}
	}
	if line["retired_reaped"] != float64(1) {
		t.Fatalf("gc_run_complete = %v; want retired_reaped 1", line)
	}
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
