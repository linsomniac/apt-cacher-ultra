package gc

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// Issue #14 Tier 1a (G2-RR1-M1): every GC pass gives its writes a start
// deadline, its own pass deadline, so a write still queued behind a busy
// writer at that deadline is abandoned (cache.ErrWriteNotStarted) and the
// pass ends there, as at its deadline, instead of waiting, running late,
// and eating the next pass's share of the budget. These tests hold the
// writer goroutine (cache.HoldWriterForTest) to queue a pass's write
// behind it.

// startDeadlineSlack is how far past its deadline a pass whose write was
// queued behind the held writer may return. The behaviour is a return at
// the deadline; the slack is for scheduling under -race only.
const startDeadlineSlack = 300 * time.Millisecond

// startDeadlineGC is a GC over c (its own gcStore), with budget as
// max_tick_duration.
func startDeadlineGC(t *testing.T, c *cache.Cache, logger *slog.Logger, budget time.Duration) *GC {
	t.Helper()
	g, err := New(Config{Cache: c, Logger: logger, Enabled: true, Interval: time.Hour, BatchSize: 100,
		SnapshotBatchSize: 10, MaxTickDuration: budget, BlobGrace: time.Second, KeepDisplaced: 3,
		PoolScanWorkers: 2, HeartbeatStaleGrace: 30 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// holdGCWriter occupies c's writer goroutine until at, or the test's end if
// sooner. The hold's ctx stays live (HoldWriterForTest's caveat); the
// cleanup releases it before openTestCache's Close runs (last in, first
// out), so a failing test never leaves Close waiting on it.
func holdGCWriter(t *testing.T, c *cache.Cache, at time.Time) {
	t.Helper()
	release, err := c.HoldWriterForTest(context.Background())
	if err != nil {
		t.Fatalf("HoldWriterForTest: %v", err)
	}
	timer := time.AfterFunc(time.Until(at), release)
	t.Cleanup(func() {
		timer.Stop()
		release()
	})
}

// noTickFailed fails the test if buf logged a gc_tick_failed line.
func noTickFailed(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	if ev := gcEvents(t, buf, "gc_tick_failed"); len(ev) != 0 {
		t.Fatalf("gc_tick_failed lines %v: a write not started before its deadline is the deadline, never a failure", ev)
	}
}

// deadlineLine returns buf's one gc_tick_deadline_reached line, failing the
// test unless there is exactly one and it is which's.
func deadlineLine(t *testing.T, buf *bytes.Buffer, which string) map[string]any {
	t.Helper()
	ev := gcEvents(t, buf, "gc_tick_deadline_reached")
	if len(ev) != 1 || ev[0]["which"] != which {
		t.Fatalf("gc_tick_deadline_reached lines %v, want the %s pass's alone", ev, which)
	}
	return ev[0]
}

// A snapshot batch queued behind the writer is abandoned at the snapshot
// deadline (start + 800 ms of a 1 s budget): the pass ends there, never
// having run the batch, and the blob pass, once the writer frees up 50 ms
// later, reaps the dead blob within the tick's budget. Before the fix the
// batch waited for the writer and then ran past the snapshot deadline.
func TestRunTick_ASnapshotBatchStuckBehindTheWriterEndsAtItsDeadline(t *testing.T) {
	c := openTestCache(t)
	logger, buf := captureLogger()
	dead := seedReapableBlob(t, c, "a dead blob behind a busy writer")
	g := startDeadlineGC(t, c, logger, time.Second)
	g.nextPass = snapshotPass // no url-path pass: the held writer stalls only the snapshot pass

	start := time.Now()
	deadlines := passDeadlines(start, g.cfg.MaxTickDuration)
	holdGCWriter(t, c, deadlines.snapshot.Add(50*time.Millisecond))
	res, err := g.runTick(context.Background(), "periodic")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("runTick: %v; a write not started before its deadline is not a failure", err)
	}
	if !res.snapshotDeadlineReached {
		t.Fatal("snapshotDeadlineReached = false, want true: the snapshot pass's batch never started before its deadline")
	}
	noTickFailed(t, buf)
	if line := deadlineLine(t, buf, "snapshot"); line["batches_completed"] != float64(0) {
		t.Fatalf("snapshot deadline line %v: want batches_completed 0, the batch abandoned, never run late", line)
	}
	if res.blobsReaped != 1 || blobRowExists(t, dbOf(t, c), dead) {
		t.Fatalf("blobs reaped %d: the blob pass did not reap the dead blob once the writer freed up", res.blobsReaped)
	}
	if limit := g.cfg.MaxTickDuration + startDeadlineSlack; elapsed > limit {
		t.Fatalf("the tick took %v, want at most %v", elapsed, limit)
	}
}

// A blob pass whose writes stay queued behind the writer past the tick's
// end ends at the tick's deadline: the representation prune and the first
// batch are abandoned, the pass reports its deadline, the rotation stays at
// the blob pass for the next tick, and the prune that never started is no
// failure. Before the fix the prune waited for the writer.
func TestRunTick_ABlobBatchStuckBehindTheWriterEndsAtTheTickDeadline(t *testing.T) {
	c := openTestCache(t)
	logger, buf := captureLogger()
	dead := seedReapableBlob(t, c, "a dead blob behind a writer busy past the tick")
	g := startDeadlineGC(t, c, logger, time.Second)
	g.nextPass = blobPass

	start := time.Now()
	deadlines := passDeadlines(start, g.cfg.MaxTickDuration)
	holdGCWriter(t, c, deadlines.blob.Add(time.Second))
	res, err := g.runTick(context.Background(), "periodic")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("runTick: %v; a prune or batch not started before its deadline is not a failure", err)
	}
	if !res.deadlineReached {
		t.Fatal("deadlineReached = false, want the blob pass's deadline")
	}
	if line := deadlineLine(t, buf, "blob"); line["batches_completed"] != float64(0) {
		t.Fatalf("blob deadline line %v: want batches_completed 0", line)
	}
	noTickFailed(t, buf)
	if g.nextPass != blobPass {
		t.Fatalf("rotation at pass %d, want the blob pass (%d): its deadline leaves the rotation there", g.nextPass, blobPass)
	}
	if res.blobsReaped != 0 || !blobRowExists(t, dbOf(t, c), dead) {
		t.Fatalf("blobs reaped %d: no batch should have run behind the held writer", res.blobsReaped)
	}
	if limit := g.cfg.MaxTickDuration + startDeadlineSlack; elapsed > limit {
		t.Fatalf("the tick took %v, want at most %v: the pass waited for the writer", elapsed, limit)
	}
}

// A snapshot pass whose first batch never started before its deadline
// returns at the deadline with nothing done and no error. The batch that
// never ran is a deadline, not a batch that found nothing (A ∪ B drained),
// so the pass does not count it or go on to sub-job C.
func TestRunSnapshotPass_AFirstBatchNeverStartedIsADeadlineNotADrain(t *testing.T) {
	c := openTestCache(t)
	logger, buf := captureLogger()
	g := startDeadlineGC(t, c, logger, time.Second)

	start := time.Now()
	deadline := start.Add(200 * time.Millisecond)
	holdGCWriter(t, c, deadline.Add(time.Second))
	res, hit, err := g.runSnapshotPass(context.Background(), deadline, "periodic")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("runSnapshotPass: %v; a batch not started before the deadline is not a failure", err)
	}
	if !hit {
		t.Fatal("deadlineHit = false, want true")
	}
	if res.batch.Total() != 0 || res.retired != 0 {
		t.Fatalf("counts %+v, want none", res)
	}
	if line := deadlineLine(t, buf, "snapshot"); line["batches_completed"] != float64(0) {
		t.Fatalf("snapshot deadline line %v: want batches_completed 0, the batch that never ran not counted", line)
	}
	if limit := deadline.Sub(start) + startDeadlineSlack; elapsed > limit {
		t.Fatalf("the pass took %v, want at most %v: it waited for the writer", elapsed, limit)
	}
}

// retiredRun's chunk that never started before its deadline (start +
// max_tick_duration) is its deadline: the run reports deadline_reached and
// a backlog, so Run re-arms it an hour later; a re-armed run that then
// finds nothing retired costs one SELECT. Before the fix the chunk waited
// for the writer, ran late, found nothing and reported no backlog.
func TestRetiredRun_AChunkNeverStartedReArms(t *testing.T) {
	c := openTestCache(t)
	logger, buf := captureLogger()
	g := startDeadlineGC(t, c, logger, 200*time.Millisecond)

	start := time.Now()
	holdGCWriter(t, c, start.Add(g.cfg.MaxTickDuration+time.Second))
	backlog := g.retiredRun(context.Background())
	elapsed := time.Since(start)
	if !backlog {
		t.Fatal("retiredRun = false, want a backlog: its chunk never started before the deadline")
	}
	runs := gcEvents(t, buf, "gc_retired_run_complete")
	if len(runs) != 1 || runs[0]["deadline_reached"] != true {
		t.Fatalf("gc_retired_run_complete lines %v, want one with deadline_reached true", runs)
	}
	noTickFailed(t, buf)
	if limit := g.cfg.MaxTickDuration + startDeadlineSlack; elapsed > limit {
		t.Fatalf("the run took %v, want at most %v: it waited for the writer", elapsed, limit)
	}
}

// A url-path batch queued behind the writer is abandoned at the url-path
// deadline: the pass ends there with no error, its cursor saved before the
// batch that never ran, so the next tick that reaches it rescans nothing it
// skipped. Before the fix the batch waited for the writer.
func TestRunURLPathPass_ABatchNeverStartedEndsAtTheDeadlineAndKeepsTheCursor(t *testing.T) {
	c := openTestCache(t)
	logger, buf := captureLogger()
	insertURLPathRow(t, dbOf(t, c), "/old.deb", 1)
	g := startDeadlineGC(t, c, logger, time.Second)
	g.urlPathCursor.scheme, g.urlPathCursor.host, g.urlPathCursor.path = "http", "example.test", "/a.deb"

	start := time.Now()
	deadline := start.Add(200 * time.Millisecond)
	holdGCWriter(t, c, deadline.Add(time.Second))
	res, hit, err := g.runURLPathPass(context.Background(), deadline, "periodic", 3600, 0, 3)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("runURLPathPass: %v; a batch not started before the deadline is not a failure", err)
	}
	if !hit || res != (urlPathResult{}) {
		t.Fatalf("deadline hit %v, counts %+v; want the deadline, nothing done", hit, res)
	}
	if line := deadlineLine(t, buf, "url_path"); line["batches_completed"] != float64(0) {
		t.Fatalf("url-path deadline line %v: want batches_completed 0", line)
	}
	if g.urlPathCursor.path != "/a.deb" {
		t.Fatalf("cursor %+v, want it kept at /a.deb, before the batch that never ran", g.urlPathCursor)
	}
	if limit := deadline.Sub(start) + startDeadlineSlack; elapsed > limit {
		t.Fatalf("the pass took %v, want at most %v: it waited for the writer", elapsed, limit)
	}
}
