package gc

// Issue #14 long-run harness (env-gated; excluded from CI by its gate).
//
// Run:  ACU_LONGRUN=1 go test ./internal/gc -run TestIssue14LongRun -v -timeout 30m
// Measures: per-"day" GC tick outcome (deadline reached, displaced reaped,
// blobs reaped, bytes reclaimed) under the scaled issue-14 workload.
// Knobs: ACU_REPRO_PKGS, ACU_REPRO_TICK_MS, ACU_REPRO_SUITES,
// ACU_REPRO_EXCESS, ACU_REPRO_PERDAY, ACU_REPRO_DAYS.
//
// AIDEV-NOTE: before/after measurements live in
// docs/superpowers/plans/2026-11-09-issue-14-gc-followup-evidence.md.
//
// Hypothesis: with the 1.0.x defaults (gc.interval=24h,
// gc.max_tick_duration=5m, gc.snapshot_batch_size=10, keep_displaced=3) a
// periodic tick can spend its whole budget in the snapshot pass deleting
// displaced snapshots (each carrying ~70k package_hash rows, deleted in one
// writer tx per batch of 10), so runTick returns before the blob pass — the
// only pass that frees disk — ever runs. The status page then shows exactly
// issue #14's screenshot: DEADLINE REACHED=true, DISPLACED REAPED>0,
// BLOBS REAPED=0, BYTES RECLAIMED=0. A restart's startup pass runs with no
// listener / freshness traffic contending for the single writer, so it gets
// further and eventually reaches the blob pass.
//
// The deadline is scaled down (ACU_REPRO_TICK_MS, default 3s) together with
// the per-snapshot package_hash count (ACU_REPRO_PKGS, default 70000) so
// the test finishes in a couple of minutes; the ratio is what matters.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func hexOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// adoptBig adopts one snapshot for suite with one unique member blob of
// blobSize bytes and nPkgs package_hash rows, like a real Ubuntu suite.
func adoptBig(t *testing.T, c *cache.Cache, suite string, seq, nPkgs, blobSize int) int64 {
	t.Helper()
	ctx := context.Background()
	content := make([]byte, blobSize)
	copy(content, fmt.Sprintf("%s-%d-index", suite, seq))
	member := putPoolBlob(t, c, string(content))
	anchor := putPoolBlob(t, c, fmt.Sprintf("InRelease %s %d", suite, seq))
	id, _, err := c.InsertTrackedCandidateForTest(ctx, cache.SnapshotCandidate{
		CanonicalScheme:         "http",
		CanonicalHost:           "archive.ubuntu.com",
		SuitePath:               suite,
		InReleaseHash:           &anchor,
		PackageCoverageComplete: true,
	})
	if err != nil {
		t.Fatalf("InsertCandidateSnapshot: %v", err)
	}
	pkgs := make([]cache.PackageHash, nPkgs)
	for i := range pkgs {
		pkgs[i] = cache.PackageHash{
			CanonicalScheme: "http",
			CanonicalHost:   "archive.ubuntu.com",
			Path:            fmt.Sprintf("/ubuntu/pool/main/p/pkg%d/pkg%d_1.%d_amd64.deb", i, i, seq),
			DeclaredSHA256:  hexOf(fmt.Sprintf("%s/%d/%d", suite, seq, i)),
			SnapshotID:      id,
			PackageName:     fmt.Sprintf("pkg%d", i),
			Architecture:    "amd64",
			Version:         fmt.Sprintf("1.%d", seq),
		}
	}
	members := []cache.SnapshotMember{{
		SnapshotID: id, Path: "main/binary-amd64/Packages.gz",
		BlobHash: member, DeclaredSHA256: member,
	}}
	if err := c.CommitAdoption(ctx, id, members, nil, pkgs, nil, true); err != nil {
		t.Fatalf("CommitAdoption: %v", err)
	}
	return id
}

func poolStats(t *testing.T, c *cache.Cache) (rows int, bytes int64, snaps int, phRows int) {
	t.Helper()
	db := dbOf(t, c)
	if err := db.QueryRow(`SELECT count(*), coalesce(sum(size),0) FROM blob`).Scan(&rows, &bytes); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM suite_snapshot`).Scan(&snaps); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM package_hash`).Scan(&phRows); err != nil {
		t.Fatal(err)
	}
	return
}

func TestIssue14LongRun(t *testing.T) {
	if os.Getenv("ACU_LONGRUN") == "" {
		t.Skip("set ACU_LONGRUN=1")
	}
	nPkgs := envInt("ACU_REPRO_PKGS", 70000)
	tick := time.Duration(envInt("ACU_REPRO_TICK_MS", 3000)) * time.Millisecond
	suites := envInt("ACU_REPRO_SUITES", 4)
	excess := envInt("ACU_REPRO_EXCESS", 6) // displaced beyond keep=3, per suite, on "day 1"
	perDay := envInt("ACU_REPRO_PERDAY", 4) // new adoptions per suite per "day"
	days := envInt("ACU_REPRO_DAYS", 3)
	const blobSize = 1 << 20

	c := openTestCache(t)
	logger, _ := captureLogger()
	db := dbOf(t, c)

	// Day 0: each suite has 1 current + 3 kept + `excess` displaced.
	seq := 0
	start := time.Now()
	for s := 0; s < suites; s++ {
		for i := 0; i < 1+3+excess; i++ {
			adoptBig(t, c, fmt.Sprintf("dists/suite%d", s), seq, nPkgs, blobSize)
			seq++
		}
	}
	t.Logf("seeded %d snapshots x %d package_hash rows in %s", seq, nPkgs, time.Since(start).Round(time.Millisecond))

	// Plus 50 blobs that are already reapable on their own (expired .debs
	// whose url_path rows are gone) — the snapshot pass has nothing to do
	// with them, yet they are freed only by the blob pass.
	for i := 0; i < 50; i++ {
		content := make([]byte, blobSize)
		copy(content, fmt.Sprintf("expired-deb-%d", i))
		seedReapableBlob(t, c, string(content))
	}
	// Every zero-ref blob is past blob_grace.
	if _, err := db.Exec(`UPDATE blob SET refcount_zeroed_at = 1 WHERE refcount <= 0`); err != nil {
		t.Fatal(err)
	}

	// Measure one snapshot batch alone (no contention), to calibrate.
	{
		b0 := time.Now()
		r, err := c.RunSnapshotGCBatch(context.Background(), 1, 3600, 3)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("calibration: one displaced snapshot (%d package_hash rows) deleted in %s (reaped %d)",
			nPkgs, time.Since(b0).Round(time.Millisecond), r.Total())
	}

	newGC := func() *GC {
		g, err := New(Config{
			Cache: c, Logger: logger, Enabled: true,
			Interval:              24 * time.Hour,
			BatchSize:             100,
			SnapshotBatchSize:     10,
			MaxTickDuration:       tick,
			BlobGrace:             5 * time.Minute,
			KeepDisplaced:         3,
			PoolScanWorkers:       2,
			HeartbeatStaleGrace:   30 * time.Minute,
			URLPathTTL:            168 * time.Hour,
			HoldWindow:            0,
			MaxVersionsPerPackage: 3,
		})
		if err != nil {
			t.Fatal(err)
		}
		return g
	}

	// report logs one run: its counts (displaced_package_rows_deleted is
	// sub-job D's, as gc_run_complete and LastRunSummary carry it, so the
	// evidence shows D's work per day), then the cache's totals.
	report := func(label string, res tickResult, d time.Duration) {
		rows, bytes, snaps, ph := poolStats(t, c)
		t.Logf("%-28s dur=%-7s deadline=%-5v retired=%-3d displaced_reaped=%-3d displaced_package_rows_deleted=%-7d blobs_reaped=%-3d bytes_reclaimed=%-6dKiB | blobs=%d (%d MiB) snapshots=%d package_hash=%d",
			label, d.Round(10*time.Millisecond), res.deadlineReached, res.retiredReaped, res.displacedReaped,
			res.displacedPackageRowsDeleted, res.blobsReaped, res.bytesReclaimed>>10, rows, bytes>>20, snaps, ph)
	}

	// Periodic ticks with contending writer traffic: the freshness
	// scheduler adopting new snapshots (each a long package_hash INSERT tx)
	// while the tick runs. Those adoptions are also the new displacements.
	g := newGC()
	g.nextPass = urlPathPass
	for day := 1; day <= days; day++ {
		var stop atomic.Bool
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perDay*suites; i++ {
				_ = stop.Load()
				adoptBig(t, c, fmt.Sprintf("dists/suite%d", i%suites), seq+i, nPkgs, blobSize)
			}
		}()
		t0 := time.Now()
		res, err := g.runTick(context.Background(), "periodic")
		d := time.Since(t0)
		stop.Store(true)
		wg.Wait()
		seq += perDay * suites
		if err != nil {
			t.Fatalf("runTick: %v", err)
		}
		report(fmt.Sprintf("day %d periodic (nextPass=%d)", day, g.nextPass), res, d)
		if _, err := db.Exec(`UPDATE blob SET refcount_zeroed_at = 1 WHERE refcount <= 0`); err != nil {
			t.Fatal(err)
		}
	}

	// "Restart": fresh GC (nextPass = url-path), StartupPass with nothing
	// else writing (listeners and freshness scheduler start after it).
	for r := 1; r <= 8; r++ {
		g2 := newGC()
		t0 := time.Now()
		if err := g2.StartupPass(context.Background()); err != nil {
			t.Fatalf("StartupPass: %v", err)
		}
		s, _ := g2.LastRunSummary()
		report(fmt.Sprintf("restart %d startup", r), tickResult{
			deadlineReached: s.DeadlineReached, displacedReaped: s.DisplacedReaped, retiredReaped: s.RetiredReaped,
			blobsReaped: s.BlobsReaped, bytesReclaimed: s.BytesReclaimed,
			displacedPackageRowsDeleted: s.DisplacedPackageRowsDeleted,
		}, time.Since(t0))
		if !s.DeadlineReached {
			break
		}
	}
}
