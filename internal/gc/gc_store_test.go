package gc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// fakeStore is a gcStore whose snapshot batches find nothing (A ∪ B
// drained) unless told to panic (batchPanics) or fail (batchErrs), whose
// sub-job C chunks report a reap and more for chunksMore calls and then
// none (or panic, chunkPanics, or fail, chunkErrs), and whose HashKnown
// knows no hash and panics for hashPanic. A batch or chunk call checks
// for its panic first, then calls its hook (batchHook, chunkHook), then
// fails or answers.
type fakeStore struct {
	mu          sync.Mutex
	batchPanics int           // RunSnapshotGCBatch panics on this many first calls
	batchErrs   int           // RunSnapshotGCBatch fails on this many calls after the batchPanics ones
	batchHook   func()        // called on every RunSnapshotGCBatch call that does not panic, before it fails or answers
	batches     int           // RunSnapshotGCBatch calls
	chunkRows   []int         // maxRows of each RunRetiredSnapshotGCChunk call
	chunkSleep  time.Duration // each chunk takes this long (virtual time in a bubble)
	chunksMore  int
	chunkPanics bool   // RunRetiredSnapshotGCChunk panics
	chunkErrs   bool   // RunRetiredSnapshotGCChunk fails
	chunkHook   func() // called on every RunRetiredSnapshotGCChunk call that does not panic, before it fails or answers; runs under f.mu: it must not call the fake
	hashPanic   string
}

func (f *fakeStore) RunSnapshotGCBatch(context.Context, int, int64, int, bool) (cache.SnapshotGCBatchResult, error) {
	f.mu.Lock()
	f.batches++
	n := f.batches
	f.mu.Unlock()
	if n <= f.batchPanics {
		panic("snapshot batch bug")
	}
	if f.batchHook != nil {
		f.batchHook()
	}
	if n <= f.batchPanics+f.batchErrs {
		return cache.SnapshotGCBatchResult{}, errors.New("snapshot batch fault")
	}
	return cache.SnapshotGCBatchResult{}, nil
}

func (f *fakeStore) RunRetiredSnapshotGCChunk(_ context.Context, maxRows int) (int, bool, error) {
	time.Sleep(f.chunkSleep)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.chunkPanics {
		panic("retired chunk bug")
	}
	if f.chunkHook != nil {
		f.chunkHook()
	}
	if f.chunkErrs {
		return 0, false, errors.New("retired chunk fault")
	}
	f.chunkRows = append(f.chunkRows, maxRows)
	if len(f.chunkRows) > 1000 {
		panic("runaway sub-job C loop")
	}
	if len(f.chunkRows) <= f.chunksMore {
		return 1, true, nil
	}
	return 0, false, nil
}

func (f *fakeStore) HashKnown(_ context.Context, hash string) (bool, error) {
	if hash == f.hashPanic {
		panic("hash lookup bug")
	}
	return false, nil
}

// storeGC is a GC over a real cache (for the passes outside the seam)
// whose gcStore is store, logging into the returned buffer.
func storeGC(t *testing.T, store gcStore, workers int, logger *slog.Logger) (*GC, *cache.Cache) {
	t.Helper()
	c := openTestCache(t)
	g, err := New(Config{Cache: c, Logger: logger, Enabled: true, Interval: time.Hour, BatchSize: 100,
		SnapshotBatchSize: 10, MaxTickDuration: time.Minute, BlobGrace: 5 * time.Minute, KeepDisplaced: 3,
		PoolScanWorkers: workers, HeartbeatStaleGrace: 30 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	g.store = store
	return g, c
}

// gcEvents decodes buf's JSON records whose msg is msg.
func gcEvents(t *testing.T, buf *bytes.Buffer, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		var m map[string]any
		if line != "" && json.Unmarshal([]byte(line), &m) == nil && m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}

// Deferred minor F-2: sub-job C asks the cache for its own chunk
// (maxRows 0: retiredChunkRows, 10,000 rows a transaction), never a
// larger number, on every call until nothing retired is left.
func TestSnapshotPass_SubJobCAsksForTheCachesOwnChunk(t *testing.T) {
	logger, _ := captureLogger()
	f := &fakeStore{chunksMore: 2}
	g, _ := storeGC(t, f, 2, logger)
	res, expired, err := g.runSnapshotPass(context.Background(), time.Now().Add(time.Hour), "periodic")
	if err != nil || expired || res.retired != 2 {
		t.Fatalf("pass = %+v, expired %v, %v; want 2 retired reaped", res, expired, err)
	}
	if !reflect.DeepEqual(f.chunkRows, []int{0, 0, 0}) {
		t.Fatalf("chunk calls asked for %v rows, want [0 0 0] (the cache's own chunk)", f.chunkRows)
	}
}

// Deferred minor F-1: sub-job C checks the tick's deadline before each
// chunk: with chunks of 30 s and a minute left, it runs two and reports
// the deadline (gc_tick_deadline_reached, which=snapshot), leaving the
// rest to the next tick.
func TestSnapshotPass_SubJobCStopsAtTheDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger, buf := captureLogger()
		f := &fakeStore{chunksMore: 100, chunkSleep: 30 * time.Second}
		g, _ := storeGC(t, f, 2, logger)
		_, expired, err := g.runSnapshotPass(context.Background(), time.Now().Add(time.Minute), "periodic")
		if err != nil || !expired || len(f.chunkRows) != 2 {
			t.Fatalf("expired %v, %v, %d chunks; want the deadline after 2 chunks", expired, err, len(f.chunkRows))
		}
		if ev := gcEvents(t, buf, "gc_tick_deadline_reached"); len(ev) != 1 || ev[0]["which"] != "snapshot" {
			t.Fatalf("deadline lines %v, want one for the snapshot pass", ev)
		}
	})
}

// Deferred minor F-3: the snapshot pass's result names its batch counts
// instead of embedding them, so it promotes no Total() that would leave
// out the retired reaps.
func TestSnapshotPassResult_PromotesNoTotal(t *testing.T) {
	if _, ok := reflect.TypeOf(snapshotPassResult{}).MethodByName("Total"); ok {
		t.Fatal("snapshotPassResult has a Total method: a promoted count without retired")
	}
}

// Deferred minor F-21 (design note 11), Review Focus 4: a panic in a
// periodic tick — here in the snapshot pass — is recovered and logged
// (gc_panicked, where=tick, with its stack), and the next ticks run on
// schedule: the blob pass the rotation moved to, then a full rotation
// whose snapshot pass runs again (review M-1).
func TestRun_RecoversATickPanicAndRunsTheNextTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger, buf := captureLogger()
		f := &fakeStore{batchPanics: 1}
		g, _ := storeGC(t, f, 2, logger)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { g.Run(ctx); close(done) }()
		time.Sleep(3*time.Hour + time.Minute) // three ticks
		cancel()
		<-done
		f.mu.Lock()
		batches := f.batches
		f.mu.Unlock()
		if batches < 2 {
			t.Fatalf("%d snapshot batches, want the third tick's too", batches)
		}
		ev := gcEvents(t, buf, "gc_panicked")
		if len(ev) != 1 || ev[0]["level"] != "ERROR" || ev[0]["where"] != "tick" || ev[0]["panic"] != "snapshot batch bug" {
			t.Fatalf("gc_panicked lines %v, want one at ERROR, where=tick", ev)
		}
		if stack, _ := ev[0]["stack"].(string); !strings.Contains(stack, "(*fakeStore).RunSnapshotGCBatch") {
			t.Fatalf("gc_panicked stack %q, want the panicking RunSnapshotGCBatch's frame", stack)
		}
		if runs := gcEvents(t, buf, "gc_run_complete"); len(runs) != 2 {
			t.Fatalf("%d gc_run_complete lines, want 2 (the ticks after the panic)", len(runs))
		}
	})
}

// The final review's R3 #2: a panic in the run that deletes the retired
// snapshots an hour after the start is recovered and logged (gc_panicked,
// where=retired, ERROR), and Run goes on: its first periodic tick still
// comes at gc.interval.
func TestRun_RecoversARetiredRunPanic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger, buf := captureLogger()
		f := &fakeStore{chunkPanics: true}
		g, _ := storeGC(t, f, 2, logger)
		g.cfg.Interval = 24 * time.Hour
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { g.Run(ctx); close(done) }()
		stop := func() { cancel(); <-done }
		time.Sleep(time.Hour + time.Minute)
		synctest.Wait()
		ev := gcEvents(t, buf, "gc_panicked")
		if len(ev) != 1 || ev[0]["level"] != "ERROR" || ev[0]["where"] != "retired" || ev[0]["panic"] != "retired chunk bug" {
			stop()
			t.Fatalf("gc_panicked lines %v, want one at ERROR, where=retired", ev)
		}
		f.mu.Lock()
		f.chunkPanics = false
		f.mu.Unlock()
		time.Sleep(23 * time.Hour)
		stop()
		if runs := gcEvents(t, buf, "gc_run_complete"); len(runs) != 1 || runs[0]["phase"] != "periodic" {
			t.Fatalf("gc_run_complete lines %v, want the first periodic tick's", runs)
		}
	})
}

// The final review's R3 re-review: the run that deletes the retired
// snapshots an hour after the start stops at max_tick_duration (1 min
// here), whatever the backlog: with chunks of 30 s, two chunks, one
// gc_tick_deadline_reached with phase=retired, and gc_retired_run_complete
// with deadline_reached; nothing more before the first periodic tick.
func TestRun_RetiredRunStopsAtMaxTickDuration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger, buf := captureLogger()
		f := &fakeStore{chunkSleep: 30 * time.Second, chunksMore: 500}
		g, _ := storeGC(t, f, 2, logger)
		g.cfg.Interval = 24 * time.Hour
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { g.Run(ctx); close(done) }()
		time.Sleep(2 * time.Hour)
		synctest.Wait()
		f.mu.Lock()
		calls := len(f.chunkRows)
		f.mu.Unlock()
		cancel()
		<-done
		runs := gcEvents(t, buf, "gc_retired_run_complete")
		if calls != 2 || len(runs) != 1 || runs[0]["deadline_reached"] != true || runs[0]["retired_reaped"] != float64(2) {
			t.Fatalf("chunk calls %d, gc_retired_run_complete lines %v: want 2 chunks and one line with "+
				"deadline_reached true and retired_reaped 2", calls, runs)
		}
		if dl := gcEvents(t, buf, "gc_tick_deadline_reached"); len(dl) != 1 || dl[0]["phase"] != phaseRetired {
			t.Fatalf("gc_tick_deadline_reached lines %v, want one with phase=retired", dl)
		}
	})
}

// Review M-1 of the H1 plan: a pass that panics on every tick — here the
// snapshot pass's batch — moves the rotation past itself, so the blob pass
// after it still runs: the tick after the panic is the blob pass's, and
// the one after that comes back to the url-path and snapshot passes.
func TestPeriodicTick_APanickingPassDoesNotStarveTheNext(t *testing.T) {
	logger, buf := captureLogger()
	f := &fakeStore{batchPanics: 1000}
	g, _ := storeGC(t, f, 2, logger)
	ctx := context.Background()
	g.periodicTick(ctx) // the url-path pass (off: no TTL), then the snapshot pass panics
	if g.nextPass != blobPass {
		t.Fatalf("after a snapshot-pass panic the rotation is at pass %d, want the blob pass (%d)", g.nextPass, blobPass)
	}
	g.periodicTick(ctx) // the blob pass
	if g.nextPass != urlPathPass {
		t.Fatalf("after the blob pass the rotation is at pass %d, want the url-path pass (%d)", g.nextPass, urlPathPass)
	}
	if runs := gcEvents(t, buf, "gc_run_complete"); len(runs) != 1 {
		t.Fatalf("%d gc_run_complete lines, want 1 (the blob pass's tick)", len(runs))
	}
	g.periodicTick(ctx) // the snapshot pass again
	ev := gcEvents(t, buf, "gc_panicked")
	if len(ev) != 2 {
		t.Fatalf("%d gc_panicked lines, want 2 (the snapshot pass, twice)", len(ev))
	}
	// Design note 11: the stack once per where for the GC — the first
	// tick's panic carries it, the second's does not.
	if _, ok := ev[0]["stack"]; !ok {
		t.Fatal("the first where=tick gc_panicked line has no stack")
	}
	if _, ok := ev[1]["stack"]; ok {
		t.Fatal("the second where=tick gc_panicked line has a stack, want it once per where")
	}
}

// Review I-1 of the H1 plan: a panic in the startup pass — which runs on
// main's goroutine before any listener — is recovered and returned as an
// error, which main logs (gc startup pass failed) before it serves; a
// panic at every start would otherwise be a crash loop that never serves.
// And it moves the rotation past the pass that panicked (re-review m-4).
func TestStartupPass_ReturnsAPanicAsAnError(t *testing.T) {
	logger, buf := captureLogger()
	f := &fakeStore{batchPanics: 1}
	g, _ := storeGC(t, f, 2, logger)
	err := g.StartupPass(context.Background())
	if err == nil || !strings.Contains(err.Error(), "gc startup: panicked: snapshot batch bug") {
		t.Fatalf("StartupPass = %v, want the panic as an error", err)
	}
	ev := gcEvents(t, buf, "gc_panicked")
	if len(ev) != 1 || ev[0]["level"] != "ERROR" || ev[0]["where"] != "startup" || ev[0]["panic"] != "snapshot batch bug" {
		t.Fatalf("gc_panicked lines %v, want one at ERROR, where=startup", ev)
	}
	// The first panic per where for this (fresh) GC carries its stack,
	// which names the function that panicked.
	if stack, _ := ev[0]["stack"].(string); !strings.Contains(stack, "(*fakeStore).RunSnapshotGCBatch") {
		t.Fatalf("gc_panicked stack %q, want the panicking RunSnapshotGCBatch's frame", stack)
	}
	// Re-review m-4: the rotation moved past the snapshot pass, so the
	// first periodic tick runs the blob pass, not the pass that panicked.
	if g.nextPass != blobPass {
		t.Fatalf("after a startup panic in the snapshot pass the rotation is at pass %d, want the blob pass (%d)", g.nextPass, blobPass)
	}
}

// Deferred minor F-21, Review Focus 4: a pool-scan job that panics is
// recovered and its worker goes on, so the scan ends — with one worker
// and more files than the job queue holds, a dead worker would leave the
// producer blocked for good — and every other orphan is still unlinked.
func TestPoolScan_AWorkerPanicNeverStallsTheScan(t *testing.T) {
	logger, buf := captureLogger()
	f := &fakeStore{}
	g, c := storeGC(t, f, 1, logger)
	var files []string
	for i := range 70 {
		name := fmt.Sprintf("ab%062x", i)
		files = append(files, writePoolFile(t, c, "ab", name, "orphan"))
	}
	f.hashPanic = filepath.Base(files[0])
	done := make(chan scanResult, 1)
	go func() {
		res, err := g.runPoolScan(context.Background())
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	var res scanResult
	select {
	case res = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the pool scan stalled after a worker's panic")
	}
	if res.orphansRepaired != 69 {
		t.Fatalf("orphans repaired %d, want 69 (all but the panicking job's)", res.orphansRepaired)
	}
	if _, err := os.Stat(files[0]); err != nil {
		t.Fatalf("the panicking job's file: %v, want it left", err)
	}
	if ev := gcEvents(t, buf, "gc_panicked"); len(ev) != 1 || ev[0]["where"] != "pool_scan_worker" {
		t.Fatalf("gc_panicked lines %v, want one, where=pool_scan_worker", ev)
	}
}

// panicOnMsg is a slog handler that panics when it handles msg.
type panicOnMsg struct {
	slog.Handler
	msg string
}

func (h panicOnMsg) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == h.msg {
		panic("log handler bug")
	}
	return h.Handler.Handle(ctx, r)
}

// Deferred minor F-21: a panic in the pool scan's producer (here its log
// handler, on a stray file at the pool's root) is recovered, the job
// queue still closes and the scan ends.
func TestPoolScan_AProducerPanicEndsTheScan(t *testing.T) {
	_, buf := captureLogger()
	logger := slog.New(panicOnMsg{Handler: slog.NewJSONHandler(buf, nil), msg: "gc_pool_unexpected_root_file"})
	g, c := storeGC(t, &fakeStore{}, 2, logger)
	if err := os.WriteFile(filepath.Join(c.Dir(), "pool", "stray"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := g.runPoolScan(context.Background()); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the pool scan stalled after its producer's panic")
	}
	if ev := gcEvents(t, buf, "gc_panicked"); len(ev) != 1 || ev[0]["where"] != "pool_scan_producer" {
		t.Fatalf("gc_panicked lines %v, want one, where=pool_scan_producer", ev)
	}
}

// slowBatchStore is fakeStore whose snapshot batches (A ∪ B, drained) each
// take batchSleep.
type slowBatchStore struct {
	*fakeStore
	batchSleep time.Duration
}

func (s slowBatchStore) RunSnapshotGCBatch(ctx context.Context, n int, g int64, k int, retire bool) (cache.SnapshotGCBatchResult, error) {
	time.Sleep(s.batchSleep)
	return s.fakeStore.RunSnapshotGCBatch(ctx, n, g, k, retire)
}

// Deferred minor F-1 (execution review G-8): sub-job C checks the tick's
// deadline before each chunk, the first included: when A ∪ B's last batch
// already ran past the deadline, C deletes nothing this tick and reports
// the deadline (the next tick resumes it). A check after each chunk would
// always run one chunk past the deadline — 10,000 rows, 120-240 ms of the
// writer in the 2026-10-28 probe (retiredChunkRows' AIDEV-NOTE), after
// max_tick_duration.
func TestSnapshotPass_SubJobCChecksTheDeadlineBeforeItsFirstChunk(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger, _ := captureLogger()
		f := &fakeStore{chunksMore: 100}
		g, _ := storeGC(t, slowBatchStore{fakeStore: f, batchSleep: 2 * time.Minute}, 2, logger)
		_, expired, err := g.runSnapshotPass(context.Background(), time.Now().Add(time.Minute), "periodic")
		if err != nil || !expired {
			t.Fatalf("expired %v, %v; want the deadline reported", expired, err)
		}
		if len(f.chunkRows) != 0 {
			t.Fatalf("%d sub-job C chunks after the deadline had passed, want none", len(f.chunkRows))
		}
	})
}

// Review M-1 of the H1 plan, every pass (execution review G-8): a tick
// that panicked moves the rotation past the pass in progress — url-path
// to snapshot, snapshot to blob, blob back to url-path — and a tick whose
// passes finished leaves runTick's own rotation as it is.
// TestPeriodicTick_APanickingPassDoesNotStarveTheNext reaches only the
// snapshot pass's panic (the url-path pass is off in its rig and the blob
// pass is not behind the seam).
func TestSkipPanickedPass_MovesPastEachPass(t *testing.T) {
	g := &GC{}
	for _, tc := range []struct{ at, want pass }{
		{urlPathPass, snapshotPass},
		{snapshotPass, blobPass},
		{blobPass, urlPathPass},
	} {
		g.nextPass = tc.at
		passesDone := false
		g.skipPanickedPass(&passesDone)
		if g.nextPass != tc.want {
			t.Errorf("a panic in pass %d: the rotation is at %d, want %d", tc.at, g.nextPass, tc.want)
		}
		g.nextPass = tc.at
		passesDone = true
		g.skipPanickedPass(&passesDone)
		if g.nextPass != tc.at {
			t.Errorf("a finished tick at pass %d: the rotation moved to %d", tc.at, g.nextPass)
		}
	}
}

// The H3 final review R3 #5: a pass that fails at every tick (a fault
// limited to its tables) moves the rotation on as a panicking one does, so
// the blob pass keeps reclaiming disk while the fault waits for its fix.
func TestPeriodicTick_AFailingPassDoesNotStarveTheNext(t *testing.T) {
	logger, buf := captureLogger()
	f := &fakeStore{batchErrs: 1000}
	g, _ := storeGC(t, f, 2, logger)
	ctx := context.Background()
	g.periodicTick(ctx) // the url-path pass (off: no TTL), then the snapshot pass fails
	if g.nextPass != blobPass {
		t.Fatalf("after a failed snapshot pass the rotation is at pass %d, want the blob pass (%d)", g.nextPass, blobPass)
	}
	g.periodicTick(ctx) // the blob pass
	if g.nextPass != urlPathPass {
		t.Fatalf("after the blob pass the rotation is at pass %d, want the url-path pass (%d)", g.nextPass, urlPathPass)
	}
	g.periodicTick(ctx) // the snapshot pass fails again
	if n := len(gcEvents(t, buf, "gc_tick_failed")); n != 2 {
		t.Fatalf("%d gc_tick_failed lines, want 2 (the snapshot pass, twice)", n)
	}
}

// A pass whose batch fails because the GC's context ended (shutdown) keeps
// the rotation where it is, and logs no gc_tick_failed: it did not fail.
func TestPeriodicTick_AnEndedContextKeepsTheRotation(t *testing.T) {
	logger, buf := captureLogger()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fakeStore{batchErrs: 1000, batchHook: cancel}
	g, _ := storeGC(t, f, 2, logger)
	g.periodicTick(ctx)
	if g.nextPass != snapshotPass {
		t.Fatalf("after a pass stopped by its context the rotation is at pass %d, want the snapshot pass (%d)", g.nextPass, snapshotPass)
	}
	if n := len(gcEvents(t, buf, "gc_tick_failed")); n != 0 {
		t.Fatalf("%d gc_tick_failed lines, want 0", n)
	}
}

// The startup pass's runTick moves the rotation past a failed pass too, and
// still returns the failure (main logs it).
func TestStartupPass_AFailingPassMovesTheRotation(t *testing.T) {
	logger, _ := captureLogger()
	g, _ := storeGC(t, &fakeStore{batchErrs: 1}, 2, logger)
	if err := g.StartupPass(context.Background()); err == nil {
		t.Fatal("StartupPass returned nil, want the snapshot pass's failure")
	}
	if g.nextPass != blobPass {
		t.Fatalf("the rotation is at pass %d, want the blob pass (%d)", g.nextPass, blobPass)
	}
}

// The H3 final review R3 #5, every pass, in the style of
// TestSkipPanickedPass_MovesPastEachPass: a pass that failed while the
// GC's context lived moves the rotation past itself — url-path to
// snapshot, snapshot to blob, blob back to url-path — and one stopped by
// an ended context (a shutdown) leaves it as it is; either way passFailed
// returns the pass's error, for runTick's caller to log or return.
func TestPassFailed_MovesPastEachPassUnlessTheContextEnded(t *testing.T) {
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	g := &GC{}
	for _, tc := range []struct {
		at    pass
		ended bool // the GC's context has ended (a shutdown)
		want  pass
	}{
		{urlPathPass, false, snapshotPass},
		{snapshotPass, false, blobPass},
		{blobPass, false, urlPathPass},
		{urlPathPass, true, urlPathPass},
		{snapshotPass, true, snapshotPass},
		{blobPass, true, blobPass},
	} {
		ctx := context.Background()
		if tc.ended {
			ctx = ended
		}
		fault := errors.New("x")
		g.nextPass = tc.at
		if err := g.passFailed(ctx, fault); !errors.Is(err, fault) {
			t.Errorf("pass %d failed (context ended: %v): passFailed returned %v, want the pass's error", tc.at, tc.ended, err)
		}
		if g.nextPass != tc.want {
			t.Errorf("pass %d failed (context ended: %v): the rotation is at %d, want %d", tc.at, tc.ended, g.nextPass, tc.want)
		}
	}
}

// The H3 final review R3 #5, the url-path pass's return in runTick (the
// rig of TestPeriodicTick_AFailingPassDoesNotStarveTheNext reaches only
// the snapshot pass's): on a real cache whose url_path table is renamed
// away, the way the handler tests break one table, the url-path pass
// fails, and one tick moves the rotation to the snapshot pass.
func TestPeriodicTick_AFailingURLPathPassMovesToTheSnapshotPass(t *testing.T) {
	logger, buf := captureLogger()
	g, c := storeGC(t, nil, 2, logger)
	g.store = c // the real cache behind the seam too, as New sets it
	g.cfg.URLPathTTL = time.Hour
	if _, err := dbOf(t, c).Exec(`ALTER TABLE url_path RENAME TO url_path_unreadable`); err != nil {
		t.Fatal(err)
	}
	g.periodicTick(context.Background())
	ev := gcEvents(t, buf, "gc_tick_failed")
	if len(ev) != 1 {
		t.Fatalf("gc_tick_failed lines %v, want one (the url-path pass's)", ev)
	}
	if msg, _ := ev[0]["err"].(string); !strings.Contains(msg, "url_path gc batch") {
		t.Fatalf("gc_tick_failed err %q, want the url-path batch's failure", msg)
	}
	if g.nextPass != snapshotPass {
		t.Fatalf("after a failed url-path pass the rotation is at pass %d, want the snapshot pass (%d)", g.nextPass, snapshotPass)
	}
}

// The H3 final review R3 #5, the blob pass's return in runTick: on a real
// cache whose blob table is renamed away, a tick that resumes at the blob
// pass fails there, and the rotation moves back to the url-path pass, so
// the next tick runs the passes before it first.
func TestPeriodicTick_AFailingBlobPassMovesToTheURLPathPass(t *testing.T) {
	logger, buf := captureLogger()
	g, c := storeGC(t, nil, 2, logger)
	g.store = c // the real cache behind the seam too, as New sets it
	if _, err := dbOf(t, c).Exec(`ALTER TABLE blob RENAME TO blob_unreadable`); err != nil {
		t.Fatal(err)
	}
	g.nextPass = blobPass
	g.periodicTick(context.Background())
	ev := gcEvents(t, buf, "gc_tick_failed")
	if len(ev) != 1 {
		t.Fatalf("gc_tick_failed lines %v, want one (the blob pass's)", ev)
	}
	if msg, _ := ev[0]["err"].(string); !strings.Contains(msg, "blob gc batch") {
		t.Fatalf("gc_tick_failed err %q, want the blob batch's failure", msg)
	}
	if g.nextPass != urlPathPass {
		t.Fatalf("after a failed blob pass the rotation is at pass %d, want the url-path pass (%d)", g.nextPass, urlPathPass)
	}
}

// The run that deletes the retired snapshots an hour after the start logs
// WARN gc_tick_failed for a chunk that failed while the GC's context
// lived, and none when the context ended during the chunk (a shutdown
// stopped the run; it did not fail). The test is the context's, as
// passFailed's is, not the error's shape: neither chunk error here wraps a
// context error.
func TestRetiredRun_LogsAFailureOnlyWhileTheContextLives(t *testing.T) {
	logger, buf := captureLogger()
	g, _ := storeGC(t, &fakeStore{chunkErrs: true}, 2, logger)
	g.retiredRun(context.Background())
	ev := gcEvents(t, buf, "gc_tick_failed")
	if len(ev) != 1 || ev[0]["level"] != "WARN" {
		t.Fatalf("a chunk failed while the context lived: gc_tick_failed lines %v, want one at WARN", ev)
	}
	if msg, _ := ev[0]["err"].(string); !strings.Contains(msg, "retired chunk fault") {
		t.Fatalf("gc_tick_failed err %q, want the chunk's failure", msg)
	}

	logger, buf = captureLogger()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g, _ = storeGC(t, &fakeStore{chunkErrs: true, chunkHook: cancel}, 2, logger)
	g.retiredRun(ctx)
	if ctx.Err() == nil {
		t.Fatal("the chunk's hook did not run: the run ended before its chunk")
	}
	if n := len(gcEvents(t, buf, "gc_tick_failed")); n != 0 {
		t.Fatalf("the context ended during the chunk: %d gc_tick_failed lines, want 0", n)
	}
	if runs := gcEvents(t, buf, "gc_retired_run_complete"); len(runs) != 1 {
		t.Fatalf("gc_retired_run_complete lines %v, want the run's one", runs)
	}
}
