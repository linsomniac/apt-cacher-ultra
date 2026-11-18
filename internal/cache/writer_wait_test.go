package cache

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/observability"
)

// What a write waited behind (2026-11-18). Every write in the daemon runs
// on one goroutine, in arrival order. These tests pin the bookkeeping that
// says, for a write that waited, what that goroutine ran meanwhile:
//   - per piece of work (WriterWait: adoption_finished's writer_ahead), and
//   - per write that waited long (the database_write_waited line).
//
// Time is a clock the test moves, so the seconds asserted are exact: an
// op "takes" what it advances the clock by.

// writerTestClock is a clock a test moves by hand.
type writerTestClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *writerTestClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *writerTestClock) Advance(d time.Duration) {
	f.mu.Lock()
	f.t = f.t.Add(d)
	f.mu.Unlock()
}

// useWriterClock gives c's writer bookkeeping a clock the test moves. Call
// it before the first write.
func useWriterClock(c *Cache) *writerTestClock {
	f := &writerTestClock{t: time.Unix(1_800_000_000, 0)}
	c.wr.now = f.Now
	return f
}

// waitUntil polls cond; it fails the test after five seconds.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// heldWrite is a write whose op blocks until end is called.
type heldWrite struct {
	started chan struct{} // closed once the writer runs the op
	end     func()        // lets the op return; idempotent
	done    chan error    // submitWrite's result
}

// queueHeld submits a write of kind whose op blocks until end. It returns
// once the request is in the writer's queue, or — on an idle writer —
// already running.
func queueHeld(t *testing.T, c *Cache, ctx context.Context, kind writeKind) *heldWrite {
	t.Helper()
	h := &heldWrite{started: make(chan struct{}), done: make(chan error, 1)}
	gate := make(chan struct{})
	var once sync.Once
	h.end = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(h.end) // a failing test must not leave Close waiting on the writer
	before := len(c.writes)
	go func() {
		h.done <- c.submitWrite(ctx, kind, func(context.Context, *sql.Conn) error {
			close(h.started)
			<-gate
			return nil
		})
	}()
	waitUntil(t, "the held write to be queued or running", func() bool {
		select {
		case <-h.started:
			return true
		default:
			return len(c.writes) == before+1
		}
	})
	return h
}

// runOnWriter puts an op of kind on the writer and returns once it runs.
func runOnWriter(t *testing.T, c *Cache, kind writeKind) *heldWrite {
	t.Helper()
	h := queueHeld(t, c, context.Background(), kind)
	select {
	case <-h.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the op never reached the writer")
	}
	return h
}

// queueWrite submits a write of kind whose op runs fn (nil: nothing) and
// returns once the request is in the writer's queue. The writer must be
// busy: on an idle writer the request would not stay queued.
func queueWrite(t *testing.T, c *Cache, ctx context.Context, kind writeKind, fn func()) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	before := len(c.writes)
	go func() {
		done <- c.submitWrite(ctx, kind, func(context.Context, *sql.Conn) error {
			if fn != nil {
				fn()
			}
			return nil
		})
	}()
	waitUntil(t, "the write to be queued", func() bool { return len(c.writes) == before+1 })
	return done
}

// wrote waits for a queued write's result and fails the test on an error.
func wrote(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("write: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the write never finished")
	}
}

// logRecords collects what a cache logged, one map of fields per line.
type logRecords struct {
	mu   sync.Mutex
	recs []map[string]any
}

type recordingHandler struct {
	into  *logRecords
	attrs []slog.Attr
}

func (h recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h recordingHandler) Handle(_ context.Context, r slog.Record) error {
	m := map[string]any{"msg": r.Message}
	for _, a := range h.attrs {
		m[a.Key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.Any()
		return true
	})
	h.into.mu.Lock()
	h.into.recs = append(h.into.recs, m)
	h.into.mu.Unlock()
	return nil
}

func (h recordingHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return recordingHandler{into: h.into, attrs: append(append([]slog.Attr{}, h.attrs...), as...)}
}

func (h recordingHandler) WithGroup(string) slog.Handler { return h }

// named returns the lines logged with msg, oldest first.
func (l *logRecords) named(msg string) []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, r := range l.recs {
		if r["msg"] == msg {
			out = append(out, r)
		}
	}
	return out
}

// openRecordingCache opens a cache whose log lines the test can read.
func openRecordingCache(t *testing.T) (*Cache, *logRecords) {
	t.Helper()
	recs := &logRecords{}
	c, err := Open(context.Background(), t.TempDir(), slog.New(recordingHandler{into: recs}))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, recs
}

// withWorkIdentityForTest returns a context that carries a piece of work's
// identity the way internal/freshness's contexts do.
func withWorkIdentityForTest(ctx context.Context, workID, workKind, adoptionID string) context.Context {
	ctx = observability.WithWaits(ctx, &observability.Waits{WorkID: workID, WorkKind: workKind})
	return observability.WithAdoptionTrace(ctx, &observability.AdoptionTrace{ID: adoptionID})
}

// A commit queues behind a GC batch that is already running and a second
// one that is queued. What it reports as ahead of it is the part of the
// first that ran after it queued, and all of the second.
func TestWriterWait_ReportsWhatTheWriterRanWhileAWriteWaited(t *testing.T) {
	c := openCache(t)
	clk := useWriterClock(c)
	ctx := context.Background()

	blobs := runOnWriter(t, c, writeGCBlobs)
	clk.Advance(time.Second) // the blob batch has run for a second
	snapshots := queueWrite(t, c, ctx, writeGCSnapshots, func() { clk.Advance(2 * time.Second) })
	var work WriterWait
	commit := queueWrite(t, c, WithWriterWait(ctx, &work), writeAdoptionCommit, func() { clk.Advance(time.Second) })

	clk.Advance(3 * time.Second) // and needs three more
	blobs.end()
	wrote(t, snapshots)
	wrote(t, commit)

	seconds, ahead := work.Total()
	if seconds != 5 {
		t.Errorf("waited %v s, want 5 (3 of the blob batch, 2 of the snapshot batch)", seconds)
	}
	want := map[string]WriterAhead{
		"gc_blobs":     {Count: 1, Seconds: 3}, // not 4: its first second was before the commit queued
		"gc_snapshots": {Count: 1, Seconds: 2},
	}
	if !reflect.DeepEqual(ahead, want) {
		t.Errorf("ahead = %+v, want %+v", ahead, want)
	}
}

// Two writes of one piece of work wait at the same time. The time both
// spent queued is counted once, and the first of them, which ran while the
// second still waited, is among what the second waited behind.
func TestWriterWait_WritesWaitingAtOnceCountTheTimeOnce(t *testing.T) {
	c := openCache(t)
	clk := useWriterClock(c)
	var work WriterWait
	ctx := WithWriterWait(context.Background(), &work)

	blobs := runOnWriter(t, c, writeGCBlobs)
	heartbeat := queueWrite(t, c, ctx, writeAdoptionHeartbeat, func() { clk.Advance(time.Second) })
	clk.Advance(2 * time.Second)
	commit := queueWrite(t, c, ctx, writeAdoptionCommit, nil)
	clk.Advance(2 * time.Second)
	blobs.end() // at 4 s; the heartbeat runs 4..5, the commit starts at 5
	wrote(t, heartbeat)
	wrote(t, commit)

	seconds, ahead := work.Total()
	if seconds != 5 {
		t.Errorf("waited %v s, want 5: one stretch from 0 to 5, not the heartbeat's 4 plus the commit's 3", seconds)
	}
	want := map[string]WriterAhead{
		"gc_blobs":           {Count: 1, Seconds: 4},
		"adoption_heartbeat": {Count: 1, Seconds: 1},
	}
	if !reflect.DeepEqual(ahead, want) {
		t.Errorf("ahead = %+v, want %+v", ahead, want)
	}
}

// A write that found the writer idle waited behind nothing, and is not
// reported as a long wait.
func TestWriterWait_AWriteThatDidNotWaitReportsNothing(t *testing.T) {
	c, logs := openRecordingCache(t)
	clk := useWriterClock(c)
	var work WriterWait
	ctx := WithWriterWait(context.Background(), &work)

	err := c.submitWrite(ctx, writeAdoptionCommit, func(context.Context, *sql.Conn) error {
		clk.Advance(30 * time.Second) // a long transaction is not a long wait
		return nil
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	seconds, ahead := work.Total()
	if seconds != 0 || len(ahead) != 0 {
		t.Errorf("Total = %v s, %+v; want 0 and nothing ahead", seconds, ahead)
	}
	if _, waiting := work.Waiting(); waiting {
		t.Error("Waiting reports a queued write after the only write ran")
	}
	if got := logs.named("database_write_waited"); len(got) != 0 {
		t.Errorf("database_write_waited logged for a write that did not wait: %+v", got)
	}
}

// While a write is still queued, the work can say how long it has waited,
// what the writer is running now and for how long, and what it has
// finished since.
func TestWriterWait_WhileQueuedNamesWhatTheWriterIsRunning(t *testing.T) {
	c := openCache(t)
	clk := useWriterClock(c)
	var work WriterWait
	ctx := context.Background()

	retired := runOnWriter(t, c, writeGCRetiredSnapshots)
	clk.Advance(time.Second)
	blobs := queueHeld(t, c, ctx, writeGCBlobs)
	if _, waiting := work.Waiting(); waiting {
		t.Fatal("Waiting reports a queued write before the work queued one")
	}
	commit := queueWrite(t, c, WithWriterWait(ctx, &work), writeAdoptionCommit, nil)
	clk.Advance(4 * time.Second)

	now, waiting := work.Waiting()
	if !waiting {
		t.Fatal("Waiting = false while the commit is queued")
	}
	want := WriterWaiting{Seconds: 4, Running: "gc_retired_snapshots", RunningSeconds: 5, Ahead: map[string]WriterAhead{}}
	if !reflect.DeepEqual(now, want) {
		t.Errorf("while the first op runs: %+v, want %+v", now, want)
	}

	retired.end()
	<-blobs.started
	now, waiting = work.Waiting()
	want = WriterWaiting{Seconds: 4, Running: "gc_blobs", RunningSeconds: 0,
		Ahead: map[string]WriterAhead{"gc_retired_snapshots": {Count: 1, Seconds: 4}}}
	if !waiting || !reflect.DeepEqual(now, want) {
		t.Errorf("once the second op runs: waiting=%v %+v, want %+v", waiting, now, want)
	}

	clk.Advance(2 * time.Second)
	blobs.end()
	wrote(t, commit)
	if _, waiting := work.Waiting(); waiting {
		t.Error("Waiting = true after the commit ran")
	}
	seconds, ahead := work.Total()
	wantAhead := map[string]WriterAhead{
		"gc_retired_snapshots": {Count: 1, Seconds: 4},
		"gc_blobs":             {Count: 1, Seconds: 2},
	}
	if seconds != 6 || !reflect.DeepEqual(ahead, wantAhead) {
		t.Errorf("Total = %v s, %+v; want 6 s, %+v", seconds, ahead, wantAhead)
	}
}

// A write whose caller stops waiting (its context ended) is no longer
// queued for the work, and the op still on the writer is what it waited
// behind.
func TestWriterWait_AWriteGivenUpStopsWaiting(t *testing.T) {
	c, logs := openRecordingCache(t)
	clk := useWriterClock(c)
	var work WriterWait
	ctx, cancel := context.WithCancel(WithWriterWait(context.Background(), &work))
	defer cancel()

	commit := runOnWriter(t, c, writeAdoptionCommit)
	touch := queueWrite(t, c, ctx, writeURLPathTouch, nil)
	clk.Advance(3 * time.Second)
	cancel()
	select {
	case err := <-touch:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the cancelled write = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled write never returned")
	}

	if _, waiting := work.Waiting(); waiting {
		t.Error("Waiting = true after the caller gave up")
	}
	seconds, ahead := work.Total()
	wantAhead := map[string]WriterAhead{"adoption_commit": {Count: 1, Seconds: 3}}
	if seconds != 3 || !reflect.DeepEqual(ahead, wantAhead) {
		t.Errorf("Total = %v s, %+v; want 3 s, %+v", seconds, ahead, wantAhead)
	}
	lines := logs.named("database_write_waited")
	if len(lines) != 1 {
		t.Fatalf("database_write_waited lines = %+v, want one", lines)
	}
	got := lines[0]
	if got["op"] != "url_path_touch" || got["waited_seconds"] != 3.0 || got["started"] != false {
		t.Errorf("line = %+v, want op url_path_touch, waited 3 s, not started", got)
	}
	if _, has := got["ran_seconds"]; has {
		t.Errorf("line has ran_seconds for a write that never ran: %+v", got)
	}
	if !reflect.DeepEqual(got["ahead"], wantAhead) {
		t.Errorf("line's ahead = %+v, want %+v", got["ahead"], wantAhead)
	}
	commit.end()
}

// A write that gives up at its start deadline is no longer queued either.
func TestWriterWait_AWriteAbandonedAtItsStartDeadlineStopsWaiting(t *testing.T) {
	c := openCache(t)
	useWriterClock(c)
	var work WriterWait
	ctx := WithWriteStartDeadline(WithWriterWait(context.Background(), &work), time.Now().Add(30*time.Millisecond))

	commit := runOnWriter(t, c, writeAdoptionCommit)
	err := c.submitWrite(ctx, writeGCBlobs, func(context.Context, *sql.Conn) error { return nil })
	if !errors.Is(err, ErrWriteNotStarted) {
		t.Fatalf("write = %v, want ErrWriteNotStarted", err)
	}
	if _, waiting := work.Waiting(); waiting {
		t.Error("Waiting = true after the write gave up at its start deadline")
	}
	commit.end()
}

// A write that waited two seconds or more is logged once it has run, with
// how long it waited, how long it then ran, and what was ahead of it.
func TestWriteWaited_IsLoggedWithWhatWasAhead(t *testing.T) {
	c, logs := openRecordingCache(t)
	clk := useWriterClock(c)
	ctx := context.Background()

	blobs := runOnWriter(t, c, writeGCBlobs)
	clk.Advance(time.Second)
	snapshots := queueWrite(t, c, ctx, writeGCSnapshots, func() { clk.Advance(2 * time.Second) })
	commit := queueWrite(t, c, ctx, writeAdoptionCommit, func() { clk.Advance(time.Second) })
	clk.Advance(3 * time.Second)
	blobs.end()
	wrote(t, snapshots)
	wrote(t, commit)

	byOp := map[string]map[string]any{}
	for _, l := range logs.named("database_write_waited") {
		byOp[l["op"].(string)] = l
	}
	if len(byOp) != 2 {
		t.Fatalf("database_write_waited lines = %+v, want one for gc_snapshots and one for adoption_commit", byOp)
	}
	want := map[string]map[string]any{
		"gc_snapshots": {"msg": "database_write_waited", "op": "gc_snapshots", "waited_seconds": 3.0, "started": true,
			"ran_seconds": 2.0, "suppressed": int64(0),
			"ahead": map[string]WriterAhead{"gc_blobs": {Count: 1, Seconds: 3}}},
		"adoption_commit": {"msg": "database_write_waited", "op": "adoption_commit", "waited_seconds": 5.0, "started": true,
			"ran_seconds": 1.0, "suppressed": int64(0),
			"ahead": map[string]WriterAhead{"gc_blobs": {Count: 1, Seconds: 3}, "gc_snapshots": {Count: 1, Seconds: 2}}},
	}
	if !reflect.DeepEqual(byOp, want) {
		t.Errorf("lines = %+v\nwant    %+v", byOp, want)
	}
}

// A wait just under two seconds is not logged.
func TestWriteWaited_AShortWaitIsNotLogged(t *testing.T) {
	c, logs := openRecordingCache(t)
	clk := useWriterClock(c)

	blobs := runOnWriter(t, c, writeGCBlobs)
	commit := queueWrite(t, c, context.Background(), writeAdoptionCommit, nil)
	clk.Advance(2*time.Second - time.Millisecond)
	blobs.end()
	wrote(t, commit)

	if got := logs.named("database_write_waited"); len(got) != 0 {
		t.Errorf("database_write_waited logged for a wait under two seconds: %+v", got)
	}
}

// Long waits of one kind of write are logged at most once in ten seconds;
// the next line says how many were left out.
func TestWriteWaited_LinesOfOneKindAreRateLimited(t *testing.T) {
	c, logs := openRecordingCache(t)
	clk := useWriterClock(c)

	// Each touch waits wait behind a held op, then runs.
	touchAfter := func(wait time.Duration) {
		t.Helper()
		hold := runOnWriter(t, c, writeGCBlobs)
		touch := queueWrite(t, c, context.Background(), writeURLPathTouch, nil)
		clk.Advance(wait)
		hold.end()
		wrote(t, touch)
	}
	touchAfter(2 * time.Second) // logged at 2 s
	touchAfter(3 * time.Second) // at 5 s: three seconds after the last line, left out
	touchAfter(8 * time.Second) // at 13 s: eleven seconds after it, logged

	lines := logs.named("database_write_waited")
	if len(lines) != 2 {
		t.Fatalf("database_write_waited lines = %+v, want two (the second wait is left out)", lines)
	}
	if lines[0]["waited_seconds"] != 2.0 || lines[0]["suppressed"] != int64(0) {
		t.Errorf("first line = %+v, want waited 2 s, suppressed 0", lines[0])
	}
	if lines[1]["waited_seconds"] != 8.0 || lines[1]["suppressed"] != int64(1) {
		t.Errorf("second line = %+v, want waited 8 s, suppressed 1", lines[1])
	}
}

// The line of a write that belongs to tracked work carries that work's
// identity, so it can be matched with adoption_finished.
func TestWriteWaited_CarriesTheWorkAndAdoptionIDs(t *testing.T) {
	c, logs := openRecordingCache(t)
	clk := useWriterClock(c)
	ctx := withWorkIdentityForTest(context.Background(), "work-1", "adoption", "adoption-1")

	blobs := runOnWriter(t, c, writeGCBlobs)
	commit := queueWrite(t, c, ctx, writeAdoptionCommit, nil)
	clk.Advance(2 * time.Second)
	blobs.end()
	wrote(t, commit)

	lines := logs.named("database_write_waited")
	if len(lines) != 1 {
		t.Fatalf("database_write_waited lines = %+v, want one", lines)
	}
	got := lines[0]
	if got["work_id"] != "work-1" || got["work_kind"] != "adoption" || got["adoption_id"] != "adoption-1" {
		t.Errorf("line = %+v, want work_id work-1, work_kind adoption, adoption_id adoption-1", got)
	}
}

// Every kind of write has its own name, in the log vocabulary's form, and
// docs/log-fields.md lists it: the names are what an operator reads in
// writer_ahead and in database_write_waited's op.
func TestWriteKinds_EachHasItsOwnDocumentedName(t *testing.T) {
	doc, err := os.ReadFile("../../docs/log-fields.md")
	if err != nil {
		t.Fatalf("read docs/log-fields.md: %v", err)
	}
	form := regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)
	seen := map[string]writeKind{}
	for k := writeKind(0); k < numWriteKinds; k++ {
		if k == writeTest {
			continue // a test's own write: no release code path submits one, so no operator reads it
		}
		name := k.String()
		if !form.MatchString(name) {
			t.Errorf("write kind %d is named %q, want a snake_case name", k, name)
			continue
		}
		if other, dup := seen[name]; dup {
			t.Errorf("write kinds %d and %d share the name %q", other, k, name)
		}
		seen[name] = k
		if !strings.Contains(string(doc), "`"+name+"`") {
			t.Errorf("docs/log-fields.md does not list the write kind `%s`", name)
		}
	}
}
