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
// first deleted header (the Task 4 review's minor 1). The startup pass
// runs sub-jobs A and B — a candidate whose heartbeat went stale, and
// another suite's fourth displaced snapshot beyond keep_displaced = 3, are
// deleted (the final fix wave's re-review, gc-sims minor 2), the displaced
// one retired and then deleted by its sub-job C (since 2026-11-09, issue
// #14 follow-up Tier 1b; B used to delete it) — and its blob pass, which
// reclaims an unreferenced orphan blob; with nothing else retired yet its
// sub-job C deletes only that one (TestStartupPass_DeletesRetiredSnapshotsAndReapsTheirBlobs
// pins what it deletes since issue #14). The suite is forgotten after the
// start; the first periodic tick's snapshot pass deletes its retired
// snapshots (retired_reaped in its gc_run_complete and LastRunSummary) and
// the same tick's blob pass reclaims their blobs once blob_grace has
// passed.
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
		blobs := []string{oldAnchor, oldMember, anchor, b1, b2}
		orphan := putPoolBlob(t, c, "an orphan no row references")
		fastForwardZeroedAt(t, db, orphan)
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
			t.Fatalf("the startup pass deleted a snapshot of a tracked suite (displaced kept %v, current kept %v)",
				snapshotRowExists(t, db, old), snapshotRowExists(t, db, id))
		}
		if blobRowExists(t, db, orphan) || fileExists(t, c.BlobPath(orphan)) {
			t.Fatal("the startup pass kept an orphan blob: its blob pass did not run")
		}
		if s, ok := g.LastRunSummary(); !ok || s.Phase != "startup" || s.RetiredReaped != 1 ||
			s.OrphanCandidatesReaped != 1 || s.DisplacedReaped != 1 {
			t.Fatalf("startup LastRunSummary = %+v, %v; want sub-jobs A and B run (orphan_candidates_reaped 1, "+
				"displaced_reaped 1) and C delete the one B retired (retired_reaped 1)", s, ok)
		}
		if snapshotRowExists(t, db, stale) || snapshotRowExists(t, db, displaced) {
			t.Fatalf("after the startup pass: stale candidate kept %v, displaced snapshot beyond keep_displaced kept %v; "+
				"want both deleted (sub-jobs A and B)", snapshotRowExists(t, db, stale), snapshotRowExists(t, db, displaced))
		}
		key := cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}
		if _, err := c.DeleteSuiteMetadata(ctx, key); err != nil { // the reaper's step 1: the seeded anchor row
			t.Fatal(err)
		}
		if res, err := c.ForgetSuite(ctx, key, &id); err != nil || !res.Deleted {
			t.Fatalf("ForgetSuite = %+v, %v", res, err)
		}
		for _, h := range blobs {
			fastForwardZeroedAt(t, db, h)
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
		if got["startup"] != float64(1) || got["periodic"] != float64(2) {
			t.Fatalf("gc_run_complete retired_reaped by phase = %v; want startup 1 (the displaced one B retired), periodic 2", got)
		}
	})
}

// The final review's R3 #2: a daemon restarted more often than
// gc.interval (24 h by default, here) never reached a periodic tick, so
// sub-job C never ran and a reaped or forgotten suite's retired snapshots
// stayed for good. With gc.interval longer than an hour, Run deletes them
// an hour after the start, and since issue #14 (ruling PF-15 re-ruled by
// the maintainer, 2026-11-05) every startup pass deletes them too. A suite
// forgotten after the first start's startup pass leaves a retired snapshot
// that survives that start, which stops a second short of its hour; the
// second start's startup pass deletes it (retired_reaped 1 in its
// gc_run_complete). A suite forgotten after the second start's startup
// pass is deleted an hour into that start, which logs
// gc_retired_run_complete; no periodic tick ran (no periodic
// gc_run_complete). Another suite's displaced snapshot beyond
// keep_displaced is retired by the first start's startup pass, and deleted
// by its sub-job C in the same pass (since 2026-11-09, issue #14 follow-up
// Tier 1b; ruling HD-7 had that pass delete it in B), so that pass's
// retired_reaped is 1 too.
func TestRun_DeletesRetiredSnapshotsAnHourAfterTheStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := openTestCache(t)
		logger, buf := captureLogger()
		db := dbOf(t, c)
		ctx := context.Background()
		adoptAndForget := func(suite string) func() int64 {
			id := adoptSnapshot(t, c, "http", "h", suite, putPoolBlob(t, c, suite+" anchor"),
				[]string{putPoolBlob(t, c, suite+" member")})
			return func() int64 {
				t.Helper()
				key := cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: suite}
				if _, err := c.DeleteSuiteMetadata(ctx, key); err != nil { // the reaper's step 1: the seeded anchor row
					t.Fatal(err)
				}
				if res, err := c.ForgetSuite(ctx, key, &id); err != nil || !res.Deleted {
					t.Fatalf("ForgetSuite = %+v, %v", res, err)
				}
				return id
			}
		}
		forgetX, forgetY := adoptAndForget("/r/dists/x"), adoptAndForget("/r/dists/y")
		var id, y int64
		var displaced int64 // the oldest of five: beyond the current one and keep_displaced = 3
		for i := range 5 {
			if sid := adoptSnapshot(t, c, "http", "h", "/r/dists/z", putPoolBlob(t, c, fmt.Sprintf("z anchor %d", i)),
				[]string{putPoolBlob(t, c, fmt.Sprintf("z member %d", i))}); i == 0 {
				displaced = sid
			}
		}
		cfg := Config{Cache: c, Logger: logger, Enabled: true, Interval: 24 * time.Hour, BatchSize: 100,
			SnapshotBatchSize: 10, MaxTickDuration: time.Minute, BlobGrace: time.Minute, KeepDisplaced: 3,
			PoolScanWorkers: 2, HeartbeatStaleGrace: 30 * time.Minute}
		start := func(runFor time.Duration, afterStartupPass func()) {
			t.Helper()
			g, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := g.StartupPass(ctx); err != nil {
				t.Fatal(err)
			}
			afterStartupPass()
			life, cancel := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() { g.Run(life); close(done) }()
			time.Sleep(runFor)
			cancel()
			<-done
		}
		start(time.Hour-time.Second, func() { id = forgetX() })
		if !snapshotRowExists(t, db, id) {
			t.Fatal("the retired snapshot was deleted before an hour had passed")
		}
		if snapshotRowExists(t, db, displaced) {
			t.Fatal("the displaced snapshot beyond keep_displaced survived the startup pass, which retires and deletes it")
		}
		start(time.Hour+time.Second, func() {
			if snapshotRowExists(t, db, id) {
				t.Fatal("the retired snapshot survived the second start's startup pass")
			}
			y = forgetY()
		})
		if snapshotRowExists(t, db, y) {
			t.Fatal("the snapshot retired after the second start's startup pass survived that start's first hour")
		}
		var retiredRuns []map[string]any
		var startupRetired []any
		for _, l := range strings.Split(buf.String(), "\n") {
			var line map[string]any
			if l == "" || json.Unmarshal([]byte(l), &line) != nil {
				continue
			}
			switch line["msg"] {
			case "gc_retired_run_complete":
				retiredRuns = append(retiredRuns, line)
			case "gc_run_complete":
				if line["phase"] != "startup" {
					t.Errorf("a periodic tick ran: %v", line)
				}
				startupRetired = append(startupRetired, line["retired_reaped"])
			}
		}
		if fmt.Sprint(startupRetired) != "[1 1]" {
			t.Fatalf("the startup passes' retired_reaped %v, want [1 1]: the first deletes the displaced snapshot its B retired, the second the forgotten suite's", startupRetired)
		}
		if len(retiredRuns) != 1 || retiredRuns[0]["level"] != "INFO" || retiredRuns[0]["retired_reaped"] != float64(1) ||
			retiredRuns[0]["deadline_reached"] != false {
			t.Fatalf("gc_retired_run_complete lines %v, want one at INFO: retired_reaped 1, deadline_reached false", retiredRuns)
		}
	})
}

// Issue #14 (the final review 2's G2-I2; ruling PF-15 re-ruled by the
// maintainer, 2026-11-05): the startup pass runs sub-job C after A ∪ B,
// within its snapshot deadline, so a restart deletes the retired snapshots
// again — a forgotten suite's, the displaced one a periodic tick's B
// retired beyond keep_displaced, and one the startup trust audit
// quarantined (spec C9: C deletes every retired snapshot) — and the same
// start's blob pass reclaims their members. It leaves what is not retired:
// a displaced snapshot the audit still holds queued (deferred after an I/O
// fault), which B never ranks (ruling R-H1). The retired run an hour after
// the start then finds nothing.
func TestStartupPass_DeletesRetiredSnapshotsAndReapsTheirBlobs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := openTestCache(t)
		logger, buf := captureLogger()
		db := dbOf(t, c)
		ctx := context.Background()
		var members []string // of the snapshots retired below
		member := func(name string) []string {
			h := putPoolBlob(t, c, name)
			return []string{h}
		}
		// A forgotten suite's snapshot.
		forgottenMember := member("forgotten member")
		forgotten := adoptSnapshot(t, c, "http", "h", "/r/dists/x", putPoolBlob(t, c, "x anchor"), forgottenMember)
		members = append(members, forgottenMember...)
		key := cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}
		if _, err := c.DeleteSuiteMetadata(ctx, key); err != nil {
			t.Fatal(err)
		}
		if res, err := c.ForgetSuite(ctx, key, &forgotten); err != nil || !res.Deleted {
			t.Fatalf("ForgetSuite = %+v, %v", res, err)
		}
		// Five of suite z, the oldest retired by a periodic B (no C after it).
		var displaced int64
		for i := range 5 {
			m := member(fmt.Sprintf("z member %d", i))
			if id := adoptSnapshot(t, c, "http", "h", "/r/dists/z", putPoolBlob(t, c, fmt.Sprintf("z anchor %d", i)), m); i == 0 {
				displaced = id
				members = append(members, m...)
			}
		}
		if res, err := c.RunSnapshotGCBatch(ctx, 10, 1800, 3); err != nil || res.DisplacedReaped != 1 {
			t.Fatalf("a periodic B: %+v, %v; want one displaced snapshot retired", res, err)
		}
		// Suite w: its displaced snapshot quarantined by the startup audit.
		quarantinedMember := member("w member 0")
		quarantined := adoptSnapshot(t, c, "http", "h", "/r/dists/w", putPoolBlob(t, c, "w anchor 0"), quarantinedMember)
		members = append(members, quarantinedMember...)
		adoptSnapshot(t, c, "http", "h", "/r/dists/w", putPoolBlob(t, c, "w anchor 1"), member("w member 1"))
		// Five of suite q, the oldest still queued for the audit.
		var queued int64
		for i := range 5 {
			if id := adoptSnapshot(t, c, "http", "h", "/r/dists/q", putPoolBlob(t, c, fmt.Sprintf("q anchor %d", i)),
				member(fmt.Sprintf("q member %d", i))); i == 0 {
				queued = id
			}
		}
		for _, id := range []int64{quarantined, queued} {
			if _, err := db.Exec(`INSERT INTO legacy_trust_audit (snapshot_id) VALUES (?)`, id); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := c.ApplyLegacyAuditDecisions(ctx, []cache.LegacyAuditDecision{
			{SnapshotID: quarantined, Outcome: cache.LegacyAuditQuarantine},
		}); err != nil {
			t.Fatal(err)
		}
		for _, h := range members {
			fastForwardZeroedAt(t, db, h)
		}

		g, err := New(Config{Cache: c, Logger: logger, Enabled: true, Interval: 24 * time.Hour, BatchSize: 100,
			SnapshotBatchSize: 10, MaxTickDuration: time.Minute, BlobGrace: time.Minute, KeepDisplaced: 3,
			PoolScanWorkers: 2, HeartbeatStaleGrace: 30 * time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		if err := g.StartupPass(ctx); err != nil {
			t.Fatal(err)
		}
		for name, id := range map[string]int64{"forgotten": forgotten, "retired displaced": displaced, "quarantined": quarantined} {
			if snapshotRowExists(t, db, id) {
				t.Fatalf("the %s snapshot %d survived the startup pass", name, id)
			}
		}
		if !snapshotRowExists(t, db, queued) {
			t.Fatal("the startup pass deleted the displaced snapshot still queued for the audit (not retired: R-H1)")
		}
		for _, h := range members {
			if blobRowExists(t, db, h) || fileExists(t, c.BlobPath(h)) {
				t.Fatalf("blob %s of a retired snapshot survived the startup pass's blob pass", h)
			}
		}
		if s, ok := g.LastRunSummary(); !ok || s.Phase != "startup" || s.RetiredReaped != 3 {
			t.Fatalf("startup LastRunSummary = %+v, %v; want retired_reaped 3", s, ok)
		}
		runFor(g, time.Hour+time.Minute)
		if runs := gcEvents(t, buf, "gc_retired_run_complete"); len(runs) != 1 || runs[0]["retired_reaped"] != float64(0) {
			t.Fatalf("gc_retired_run_complete lines %v, want one an hour after the start, with nothing left to delete", runs)
		}
	})
}

// Issue #14 follow-up, Tier 1b (2026-11-09; supersedes ruling HD-7's
// one-transaction delete at a start): the startup pass's sub-job B retires
// the displaced snapshots beyond keep_displaced like a periodic tick's, and
// its sub-job C deletes them in the same pass, so each counts twice (in
// displaced_reaped when retired, in retired_reaped when deleted) and the
// same start's blob pass reaps the blobs they pinned.
func TestStartupPass_RetiresTheDisplacedAndCDeletesThem(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := openTestCache(t)
		logger, buf := captureLogger()
		db := dbOf(t, c)
		ctx := context.Background()
		// A current snapshot and 5 displaced ones in all: six adopted, the
		// two oldest beyond keep_displaced = 3, each pinning a blob of its own.
		var ids []int64
		var members []string
		for i := range 6 {
			m := putPoolBlob(t, c, fmt.Sprintf("z member %d", i))
			ids = append(ids, adoptSnapshot(t, c, "http", "h", "/r/dists/z", putPoolBlob(t, c, fmt.Sprintf("z anchor %d", i)), []string{m}))
			members = append(members, m)
		}
		for _, h := range members {
			fastForwardZeroedAt(t, db, h)
		}
		g, err := New(Config{Cache: c, Logger: logger, Enabled: true, Interval: 24 * time.Hour, BatchSize: 100,
			SnapshotBatchSize: 10, MaxTickDuration: time.Minute, BlobGrace: time.Minute, KeepDisplaced: 3,
			PoolScanWorkers: 2, HeartbeatStaleGrace: 30 * time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		if err := g.StartupPass(ctx); err != nil {
			t.Fatal(err)
		}
		for i, id := range ids {
			beyond := i < 2
			if snapshotRowExists(t, db, id) == beyond {
				t.Fatalf("snapshot %d (beyond keep_displaced: %v) exists %v after the startup pass", id, beyond, !beyond)
			}
			if h := members[i]; beyond == (blobRowExists(t, db, h) || fileExists(t, c.BlobPath(h))) {
				t.Fatalf("blob %s of snapshot %d (beyond keep_displaced: %v) survived %v", h, id, beyond, !beyond)
			}
		}
		if s, ok := g.LastRunSummary(); !ok || s.Phase != "startup" || s.DisplacedReaped != 2 || s.RetiredReaped != 2 {
			t.Fatalf("startup LastRunSummary = %+v, %v; want displaced_reaped 2 and retired_reaped 2", s, ok)
		}
		if ev := gcEvents(t, buf, "gc_run_complete"); len(ev) != 1 || ev[0]["displaced_reaped"] != float64(2) || ev[0]["retired_reaped"] != float64(2) {
			t.Fatalf("gc_run_complete lines %v, want one with displaced_reaped 2 and retired_reaped 2", ev)
		}
	})
}

// DN-7 of the GC and upstream hardening plan (rulings HD-4, HD-7; the H3
// final review R3 #4): one periodic tick's snapshot pass retires the
// displaced snapshots beyond keep_displaced (sub-job B, one short UPDATE)
// and deletes them in chunks (sub-job C), and the same tick's blob pass
// reclaims their blobs once blob_grace has passed. Its gc_run_complete
// counts them twice — in displaced_reaped when retired, in retired_reaped
// when deleted — and so does LastRunSummary (the status page's GC
// figures).
func TestPeriodicTick_RetiresAndDeletesTheDisplacedSnapshots(t *testing.T) {
	c := openTestCache(t)
	logger, buf := captureLogger()
	db := dbOf(t, c)
	var (
		ids   []int64
		blobs [][2]string // each snapshot's anchor and member
	)
	for i := range 6 { // the current one and five displaced: two beyond keep_displaced = 3
		anchor, member := putPoolBlob(t, c, fmt.Sprintf("anchor %d", i)), putPoolBlob(t, c, fmt.Sprintf("member %d", i))
		ids = append(ids, adoptSnapshot(t, c, "http", "h", "/r/dists/x", anchor, []string{member}))
		blobs = append(blobs, [2]string{anchor, member})
	}
	for _, b := range blobs[:2] { // unpinned since their snapshots' displacement: past blob_grace
		fastForwardZeroedAt(t, db, b[0])
		fastForwardZeroedAt(t, db, b[1])
	}
	g, err := New(Config{Cache: c, Logger: logger, Enabled: true, Interval: time.Hour, BatchSize: 100,
		SnapshotBatchSize: 10, MaxTickDuration: time.Minute, BlobGrace: time.Minute, KeepDisplaced: 3,
		PoolScanWorkers: 2, HeartbeatStaleGrace: 30 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	g.periodicTick(context.Background())
	for i, id := range ids {
		beyond := i < 2
		if snapshotRowExists(t, db, id) == beyond {
			t.Fatalf("after one periodic tick, snapshot %d (beyond keep_displaced: %v) kept %v", id, beyond, !beyond)
		}
		for _, h := range blobs[i] {
			if blobRowExists(t, db, h) == beyond || fileExists(t, c.BlobPath(h)) == beyond {
				t.Fatalf("blob %s of snapshot %d (beyond keep_displaced: %v): row %v, file %v; want both gone only beyond it",
					h, id, beyond, blobRowExists(t, db, h), fileExists(t, c.BlobPath(h)))
			}
		}
	}
	runs := gcEvents(t, buf, "gc_run_complete")
	if len(runs) != 1 || runs[0]["phase"] != "periodic" || runs[0]["displaced_reaped"] != float64(2) ||
		runs[0]["retired_reaped"] != float64(2) {
		t.Fatalf("gc_run_complete lines %v; want the periodic tick's, displaced_reaped 2 (retired) and "+
			"retired_reaped 2 (deleted)", runs)
	}
	if s, ok := g.LastRunSummary(); !ok || s.DisplacedReaped != 2 || s.RetiredReaped != 2 {
		t.Fatalf("LastRunSummary = %+v, %v; want displaced_reaped 2 and retired_reaped 2", s, ok)
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
