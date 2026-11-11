package gc

// AIDEV-NOTE: tests for the startup GC split (2026-11-11, the maintainer's
// decision: listeners open before the startup GC pass). StartupScan, the
// pool orphan scan, still runs before any listener (main); the one-shot
// startup pass runs first on Run's goroutine, after the listeners opened,
// so a restart with a GC backlog serves within seconds and never runs that
// pass beside a periodic tick or a retired run. StartupPass (scan + pass in
// one call) stays for the tests that drive a whole start.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// StartupScan runs the pool orphan scan alone: the orphan file goes, and
// no GC pass runs (no gc_run_complete line, no store call, no last-run
// summary) until Run.
func TestStartupScan_RunsOnlyThePoolScan(t *testing.T) {
	logger, buf := captureLogger()
	f := &fakeStore{}
	g, c := storeGC(t, f, 2, logger)
	hash := "ff" + strings.Repeat("0", 62)
	planted := writePoolFile(t, c, "ff", hash, "orphan body")

	if err := g.StartupScan(context.Background()); err != nil {
		t.Fatalf("StartupScan: %v", err)
	}
	if fileExists(t, planted) {
		t.Fatalf("orphan %s is still there: StartupScan did not run the pool scan", planted)
	}
	if ev := gcEvents(t, buf, "gc_run_complete"); len(ev) != 0 {
		t.Fatalf("gc_run_complete lines %v, want none: the startup pass runs in Run", ev)
	}
	if len(f.calls) != 0 {
		t.Fatalf("store calls %v, want none before Run", f.calls)
	}
	if _, ok := g.LastRunSummary(); ok {
		t.Fatal("a last-run summary after StartupScan alone, want none until the startup pass ran")
	}
}

// After StartupScan, Run runs the startup pass at once, before its loop:
// one gc_run_complete line with phase startup, carrying the scan's count,
// its blob pass reaping a dead blob, its last-run summary the startup's,
// and no periodic tick within gc.interval.
func TestRun_RunsTheStartupPassFirstAfterAStartupScan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger, buf := captureLogger()
		g, c := storeGC(t, &fakeStore{}, 2, logger)
		g.cfg.Interval = 24 * time.Hour
		writePoolFile(t, c, "ff", "ff"+strings.Repeat("0", 62), "orphan body")
		if err := g.StartupScan(context.Background()); err != nil {
			t.Fatalf("StartupScan: %v", err)
		}
		// Seeded after the scan: fakeStore's HashKnown knows no hash, so
		// the scan would take its pool file for an orphan.
		dead := seedReapableBlob(t, c, "a dead blob")
		runFor(g, 30*time.Minute)
		runs := gcEvents(t, buf, "gc_run_complete")
		if len(runs) != 1 || runs[0]["phase"] != "startup" || runs[0]["pool_orphans_repaired"] != float64(1) || runs[0]["blobs_reaped"] != float64(1) {
			t.Fatalf("gc_run_complete lines %v, want one startup line with pool_orphans_repaired 1 and blobs_reaped 1", runs)
		}
		if blobRowExists(t, dbOf(t, c), dead) {
			t.Fatal("the dead blob is still there: the startup pass's blob pass did not run")
		}
		if s, ok := g.LastRunSummary(); !ok || s.Phase != "startup" || s.PoolOrphansRepaired != 1 {
			t.Fatalf("last-run summary %+v (%v), want the startup pass's, with the scan's orphan", s, ok)
		}
	})
}

// Run without StartupScan runs no startup pass (the tests that drive Run's
// loop alone, and a start whose pool scan failed): within 30 minutes no
// gc_run_complete line.
func TestRun_NoStartupPassWithoutAStartupScan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger, buf := captureLogger()
		g, _ := storeGC(t, &fakeStore{}, 2, logger)
		g.cfg.Interval = 24 * time.Hour
		runFor(g, 30*time.Minute)
		if runs := gcEvents(t, buf, "gc_run_complete"); len(runs) != 0 {
			t.Fatalf("gc_run_complete lines %v, want none", runs)
		}
	})
}

// A failed pool scan ends the start's GC before its passes, as before the
// split: StartupScan returns the error and counts pool_scan, and Run runs
// no startup pass (the periodic ticks and retired runs go on as usual).
func TestRun_NoStartupPassAfterAFailedStartupScan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger, buf := captureLogger()
		f := &fakeStore{}
		g, c := storeGC(t, f, 2, logger)
		g.cfg.Interval = 24 * time.Hour
		if err := os.RemoveAll(filepath.Join(c.Dir(), "pool")); err != nil {
			t.Fatal(err)
		}
		before := gcMetricValue(t, `acu_gc_pass_failures_total{pass="pool_scan"}`)
		err := g.StartupScan(context.Background())
		if err == nil || !strings.Contains(err.Error(), "gc startup: pool scan") {
			t.Fatalf("StartupScan = %v, want the pool scan's failure", err)
		}
		if got := gcMetricValue(t, `acu_gc_pass_failures_total{pass="pool_scan"}`) - before; got != 1 {
			t.Fatalf("acu_gc_pass_failures_total{pass=\"pool_scan\"} rose by %v, want 1", got)
		}
		runFor(g, 30*time.Minute)
		if runs := gcEvents(t, buf, "gc_run_complete"); len(runs) != 0 || len(f.calls) != 0 {
			t.Fatalf("gc_run_complete lines %v, store calls %v; want none after a failed scan", runs, f.calls)
		}
	})
}

// A startup pass that fails is logged by Run, WARN "gc startup pass
// failed" with its first failure, as main logged StartupPass's error; the
// pass still logs its gc_run_complete line.
func TestRun_LogsAFailedStartupPass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger, buf := captureLogger()
		g, _ := storeGC(t, &fakeStore{batchErrs: 1}, 2, logger)
		g.cfg.Interval = 24 * time.Hour
		if err := g.StartupScan(context.Background()); err != nil {
			t.Fatalf("StartupScan: %v", err)
		}
		runFor(g, 30*time.Minute)
		ev := gcEvents(t, buf, "gc startup pass failed")
		if len(ev) != 1 || ev[0]["level"] != "WARN" {
			t.Fatalf("gc startup pass failed lines %v, want one WARN", ev)
		}
		if msg, _ := ev[0]["err"].(string); !strings.Contains(msg, "snapshot gc batch") {
			t.Fatalf("gc startup pass failed err %q, want the snapshot batch's failure", msg)
		}
		if runs := gcEvents(t, buf, "gc_run_complete"); len(runs) != 1 || runs[0]["phase"] != "startup" {
			t.Fatalf("gc_run_complete lines %v, want the startup pass's", runs)
		}
	})
}

// A startup pass that panics is recovered on Run's goroutine (gc_panicked,
// where=startup, and the WARN), and Run's loop goes on: the periodic tick
// comes at gc.interval (2 h here).
func TestRun_RecoversAStartupPassPanic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger, buf := captureLogger()
		g, _ := storeGC(t, &fakeStore{batchPanics: 1}, 2, logger)
		g.cfg.Interval = 2 * time.Hour
		if err := g.StartupScan(context.Background()); err != nil {
			t.Fatalf("StartupScan: %v", err)
		}
		runFor(g, 2*time.Hour+30*time.Minute)
		if ev := gcEvents(t, buf, "gc_panicked"); len(ev) != 1 || ev[0]["where"] != "startup" {
			t.Fatalf("gc_panicked lines %v, want one, where=startup", ev)
		}
		if ev := gcEvents(t, buf, "gc startup pass failed"); len(ev) != 1 {
			t.Fatalf("gc startup pass failed lines %v, want one", ev)
		}
		runs := gcEvents(t, buf, "gc_run_complete")
		if len(runs) != 1 || runs[0]["phase"] != "periodic" {
			t.Fatalf("gc_run_complete lines %v, want the periodic tick's alone (the panicked startup pass writes none)", runs)
		}
	})
}

// Run's schedule starts after the startup pass: with a retired backlog the
// startup pass works its whole 1 min budget, and the periodic tick comes
// gc.interval (2 h) after it ended, not after Run began.
func TestRun_ItsScheduleStartsAfterTheStartupPass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger, buf := captureLogger()
		g, _ := storeGC(t, &fakeStore{chunksMore: 1000, chunkSleep: 10 * time.Second}, 2, logger)
		g.cfg.Interval = 2 * time.Hour
		if err := g.StartupScan(context.Background()); err != nil {
			t.Fatalf("StartupScan: %v", err)
		}
		runFor(g, 2*time.Hour+30*time.Second)
		runs := gcEvents(t, buf, "gc_run_complete")
		if len(runs) != 1 || runs[0]["phase"] != "startup" || runs[0]["deadline_reached"] != true {
			t.Fatalf("gc_run_complete lines %v, want only the startup pass's, which reached its deadline", runs)
		}
	})
}

// A shutdown while the startup pass works ends it and Run promptly: within
// one 10 s chunk of the cancel, never the pass's 1 min budget.
func TestRun_AShutdownDuringTheStartupPassReturnsPromptly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger, buf := captureLogger()
		g, _ := storeGC(t, &fakeStore{chunksMore: 1000, chunkSleep: 10 * time.Second}, 2, logger)
		g.cfg.Interval = 24 * time.Hour
		if err := g.StartupScan(context.Background()); err != nil {
			t.Fatalf("StartupScan: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { g.Run(ctx); close(done) }()
		time.Sleep(15 * time.Second)
		cancel()
		start := time.Now()
		<-done
		if took := time.Since(start); took > 10*time.Second {
			t.Fatalf("Run returned %s after the cancel, want within one chunk (10s)", took)
		}
		noTickFailed(t, buf)
		if ev := gcEvents(t, buf, "gc startup pass failed"); len(ev) != 0 {
			t.Fatalf("gc startup pass failed lines %v, want none: a shutdown is not a failure", ev)
		}
	})
}

// A shutdown during the pool scan (it stops part way and returns no error)
// leaves no startup pass pending: Run, started anyway, runs none.
func TestStartupScan_ACancelledScanLeavesNoStartupPass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger, buf := captureLogger()
		f := &fakeStore{}
		g, _ := storeGC(t, f, 2, logger)
		g.cfg.Interval = 24 * time.Hour
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := g.StartupScan(ctx); err != nil {
			t.Fatalf("StartupScan after a shutdown: %v, want nil", err)
		}
		runFor(g, 30*time.Minute)
		if runs := gcEvents(t, buf, "gc_run_complete"); len(runs) != 0 || len(f.calls) != 0 {
			t.Fatalf("gc_run_complete lines %v, store calls %v; want no startup pass after a cancelled scan", runs, f.calls)
		}
	})
}
