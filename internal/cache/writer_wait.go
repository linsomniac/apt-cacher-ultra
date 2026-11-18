package cache

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/observability"
)

// What a write waited behind (2026-11-18).
//
// Every write in the daemon runs on one goroutine, in arrival order
// (writer, cache.go). A write that waits there was, until now, a number
// with no cause: an adoption's log said its commit phase took twelve
// seconds, not whether that was its own transaction or a queue, and never
// what the queue held. This file is the bookkeeping that answers it.

// writeKind says what a write is for. Its name (String) is log vocabulary:
// database_write_waited's op and the keys of writer_ahead
// (docs/log-fields.md lists every one; TestWriteKinds_EachHasItsOwnDocumentedName).
//
// AIDEV-NOTE: a new submitWrite call site takes a new kind, with a name
// and a docs row, unless an existing kind already means exactly that
// write: the names are only useful while each says one thing.
type writeKind uint8

const (
	writeAdoptionCandidate writeKind = iota
	writeAdoptionHeartbeat
	writeAdoptionBlobHeartbeat
	writeAdoptionCommit
	writeAdoptionFailureRecord
	writeAdoptionFailureClear
	writeAdoptionRetrySet
	writeAdoptionRetryReschedule
	writeReconcileMembers
	writeRepairMember
	writeRepairRetryBump
	writeCheckUnchanged
	writeSuiteCreate
	writeSuiteFreshnessUpdate
	writeSuiteTouch
	writeSuiteReap
	writeSuiteReapURLPaths
	writeSuiteForget
	writeFetchedStore
	writeURLPathPut
	writeURLPathTouch
	writeURLPathRefresh
	writeURLPathEvict
	writeURLPathDelete
	writeBlobPut
	writeRepresentationPut
	writeRepresentationPrune
	writeSignedSeenMark
	writeMarkerSweep
	writeGCBlobs
	writeGCSnapshots
	writeGCRetiredSnapshots
	writeGCDisplacedPackages
	writeGCURLPaths
	writeLegacyAuditEnqueue
	writeLegacyAuditApply
	writeLegacyMetadataInvalidate
	writeUsageSave
	// writeTest is a test's own write (testing_helpers.go and the tests of
	// this package). No release code path submits one.
	writeTest
	numWriteKinds
)

var writeKindNames = [numWriteKinds]string{
	writeAdoptionCandidate:        "adoption_candidate",
	writeAdoptionHeartbeat:        "adoption_heartbeat",
	writeAdoptionBlobHeartbeat:    "adoption_blob_heartbeat",
	writeAdoptionCommit:           "adoption_commit",
	writeAdoptionFailureRecord:    "adoption_failure_record",
	writeAdoptionFailureClear:     "adoption_failure_clear",
	writeAdoptionRetrySet:         "adoption_retry_set",
	writeAdoptionRetryReschedule:  "adoption_retry_reschedule",
	writeReconcileMembers:         "reconcile_members",
	writeRepairMember:             "repair_member",
	writeRepairRetryBump:          "repair_retry_bump",
	writeCheckUnchanged:           "check_unchanged",
	writeSuiteCreate:              "suite_create",
	writeSuiteFreshnessUpdate:     "suite_freshness_update",
	writeSuiteTouch:               "suite_touch",
	writeSuiteReap:                "suite_reap",
	writeSuiteReapURLPaths:        "suite_reap_url_paths",
	writeSuiteForget:              "suite_forget",
	writeFetchedStore:             "fetched_store",
	writeURLPathPut:               "url_path_put",
	writeURLPathTouch:             "url_path_touch",
	writeURLPathRefresh:           "url_path_refresh",
	writeURLPathEvict:             "url_path_evict",
	writeURLPathDelete:            "url_path_delete",
	writeBlobPut:                  "blob_put",
	writeRepresentationPut:        "representation_put",
	writeRepresentationPrune:      "representation_prune",
	writeSignedSeenMark:           "signed_seen_mark",
	writeMarkerSweep:              "marker_sweep",
	writeGCBlobs:                  "gc_blobs",
	writeGCSnapshots:              "gc_snapshots",
	writeGCRetiredSnapshots:       "gc_retired_snapshots",
	writeGCDisplacedPackages:      "gc_displaced_packages",
	writeGCURLPaths:               "gc_url_paths",
	writeLegacyAuditEnqueue:       "legacy_audit_enqueue",
	writeLegacyAuditApply:         "legacy_audit_apply",
	writeLegacyMetadataInvalidate: "legacy_metadata_invalidate",
	writeUsageSave:                "usage_save",
	writeTest:                     "test",
}

func (k writeKind) String() string {
	if k < numWriteKinds {
		return writeKindNames[k]
	}
	return "unknown"
}

// WriterAhead is what the writer ran of one kind of write while another
// write waited: how many operations, and for how long in all.
type WriterAhead struct {
	Count   uint64  `json:"count"`
	Seconds float64 `json:"seconds"`
}

// WriterWaiting is what a piece of work with a write queued can say now.
type WriterWaiting struct {
	// Seconds is how long the work has had a write queued.
	Seconds float64
	// Running is the kind of write the writer is running, and
	// RunningSeconds for how long ("" and 0: between two writes).
	Running        string
	RunningSeconds float64
	// Ahead is what the writer has finished since the work's write queued.
	Ahead map[string]WriterAhead
}

// writeWaitedLogAfter is how long a write must have waited for the writer
// to be logged as database_write_waited, and writeWaitedLogEvery how often
// at most that line is logged for one kind of write (constants, not
// knobs). A stalled writer has hundreds of client writes queued behind it;
// each of them then crosses the first bound.
const (
	writeWaitedLogAfter = 2 * time.Second
	writeWaitedLogEvery = 10 * time.Second
)

// writerTotals is what the writer has run, per kind of write.
type writerTotals struct {
	count [numWriteKinds]uint64
	nanos [numWriteKinds]time.Duration
}

// since returns what t holds beyond base.
func (t *writerTotals) since(base *writerTotals) writerTotals {
	var d writerTotals
	for k := range t.count {
		d.count[k] = t.count[k] - base.count[k]
		d.nanos[k] = t.nanos[k] - base.nanos[k]
	}
	return d
}

func (t *writerTotals) add(o *writerTotals) {
	for k := range t.count {
		t.count[k] += o.count[k]
		t.nanos[k] += o.nanos[k]
	}
}

// report returns t by kind name: the kinds with at least one operation.
// Never nil.
func (t *writerTotals) report() map[string]WriterAhead {
	out := map[string]WriterAhead{}
	for k, n := range t.count {
		if n > 0 {
			out[writeKind(k).String()] = WriterAhead{Count: n, Seconds: max(t.nanos[k], 0).Seconds()}
		}
	}
	return out
}

// writerState is the writer goroutine's account of what it has run.
//
// AIDEV-NOTE: how a wait's "ahead" is exact. A waiter keeps the totals of
// the moment it queued, and those include the part already run of the op
// then on the writer (its time, not its count: totals). When the writer
// starts the waiter's op nothing is running, so the totals of that moment
// minus the waiter's are exactly what occupied the writer while it
// waited: every op that finished in between, once, with only the part of
// the first that came after the waiter queued. The writer is one
// goroutine, so nothing else can be in that difference.
type writerState struct {
	// now is time.Now; a test's clock (useWriterClock).
	now func() time.Time

	mu           sync.Mutex
	done         writerTotals // the ops that have finished
	busy         bool         // an op is running
	running      writeKind
	runningSince time.Time
}

// totals returns the time and what the writer has run by then: the ops
// that finished, plus the time already spent in the one running. With
// countRunning that op also counts as one of its kind: for a waiter that
// stops waiting while it runs, it is an op that was ahead.
func (s *writerState) totals(countRunning bool) (time.Time, writerTotals) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	t := s.done
	if s.busy {
		t.nanos[s.running] += now.Sub(s.runningSince)
		if countRunning {
			t.count[s.running]++
		}
	}
	return now, t
}

// view returns the time, the ops that have finished, and the op running
// now (busy false: none).
func (s *writerState) view() (now time.Time, done writerTotals, running writeKind, since time.Time, busy bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now(), s.done, s.running, s.runningSince, s.busy
}

// beginOp marks an op of kind as running. It returns the time and what
// the writer had run until then (nothing is running at that moment).
func (s *writerState) beginOp(kind writeKind) (time.Time, writerTotals) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.busy, s.running, s.runningSince = true, kind, now
	return now, s.done
}

// endOp adds the op that was running to the totals and returns how long
// it ran.
func (s *writerState) endOp() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	ran := s.now().Sub(s.runningSince)
	s.done.count[s.running]++
	s.done.nanos[s.running] += ran
	s.busy = false
	return ran
}

// writeWait is one write's wait for the writer.
type writeWait struct {
	kind     writeKind
	queuedAt time.Time
	base     writerTotals // what the writer had run at queuedAt
	work     *WriterWait  // the work the write belongs to; nil: none collects
	// over is set by whoever ends the wait: the writer when it starts the
	// op, or the submitter when it gives up first. Only the winner of that
	// CAS accounts for the wait.
	over atomic.Bool

	// The writer's side, set before it sends the result. The submitter
	// reads them only after it received that result, never after giving
	// up (the op may then still be running).
	waited time.Duration
	ran    time.Duration
	ahead  *writerTotals // set when waited >= writeWaitedLogAfter
}

// queued starts a write's wait: it notes what the writer has run so far,
// and tells the work the write belongs to, if its context names one.
func (s *writerState) queued(ctx context.Context, kind writeKind) *writeWait {
	w := &writeWait{kind: kind}
	if work, _ := ctx.Value(writerWaitKey{}).(*WriterWait); work != nil {
		w.work = work
		w.queuedAt, w.base = work.enter(s)
	} else {
		w.queuedAt, w.base = s.totals(false)
	}
	return w
}

// started is the writer goroutine's: it marks w's op as running and, when
// the submitter is still waiting, ends the wait there.
func (s *writerState) started(w *writeWait) {
	now, done := s.beginOp(w.kind)
	if !w.over.CompareAndSwap(false, true) {
		return // the submitter gave up; it has accounted for its wait
	}
	w.waited = now.Sub(w.queuedAt)
	if w.work != nil {
		w.work.leave(now, &done)
	}
	if w.waited >= writeWaitedLogAfter {
		ahead := done.since(&w.base)
		w.ahead = &ahead
	}
}

// finished is the writer goroutine's: w's op has returned.
func (s *writerState) finished(w *writeWait) {
	w.ran = s.endOp()
}

// writeGaveUp ends w's wait for a submitter that stops waiting before the
// writer started its op: its context ended, its start deadline passed, or
// the cache is closing. A no-op when the writer has started the op. With
// log, a wait long enough is logged (not at a close: a shutdown is no
// news).
func (c *Cache) writeGaveUp(ctx context.Context, w *writeWait, log bool) {
	if !w.over.CompareAndSwap(false, true) {
		return
	}
	now, t := c.wr.totals(true)
	waited := now.Sub(w.queuedAt)
	if w.work != nil {
		w.work.leave(now, &t)
	}
	if log && waited >= writeWaitedLogAfter {
		ahead := t.since(&w.base)
		c.logWriteWaited(ctx, w.kind, waited, false, 0, &ahead)
	}
}

// writeReturned is the submitter's, once it has received its write's
// result: it logs a wait that was long.
func (c *Cache) writeReturned(ctx context.Context, w *writeWait) {
	if !w.over.Load() {
		// A result without a start: the writer is draining at a close.
		c.writeGaveUp(ctx, w, false)
		return
	}
	if w.ahead != nil {
		c.logWriteWaited(ctx, w.kind, w.waited, true, w.ran, w.ahead)
	}
}

// writeWaitedLog limits database_write_waited to one line per kind of
// write in writeWaitedLogEvery.
type writeWaitedLog struct {
	mu         sync.Mutex
	logged     [numWriteKinds]bool
	last       [numWriteKinds]time.Time
	suppressed [numWriteKinds]int
}

// logWriteWaited logs database_write_waited for a write of kind that
// waited: how long, whether the writer then started it and for how long
// it ran, and what the writer ran while it waited.
func (c *Cache) logWriteWaited(ctx context.Context, kind writeKind, waited time.Duration, started bool, ran time.Duration, ahead *writerTotals) {
	now := c.wr.now()
	l := &c.waitedLog
	l.mu.Lock()
	if l.logged[kind] && now.Sub(l.last[kind]) < writeWaitedLogEvery {
		l.suppressed[kind]++
		l.mu.Unlock()
		return
	}
	suppressed := l.suppressed[kind]
	l.logged[kind], l.last[kind], l.suppressed[kind] = true, now, 0
	l.mu.Unlock()

	attrs := []any{"op", kind.String(), "waited_seconds", waited.Seconds(), "started", started}
	if started {
		attrs = append(attrs, "ran_seconds", ran.Seconds())
	}
	attrs = append(attrs, "ahead", ahead.report(), "suppressed", suppressed)
	logger := observability.WorkLogger(ctx, observability.AdoptionLogger(ctx, c.logger))
	logger.LogAttrs(ctx, slog.LevelInfo, "database_write_waited", argsToAttrs(attrs)...)
}

// argsToAttrs turns alternating keys and values into attributes.
func argsToAttrs(args []any) []slog.Attr {
	out := make([]slog.Attr, 0, len(args)/2)
	for i := 0; i+1 < len(args); i += 2 {
		out = append(out, slog.Any(args[i].(string), args[i+1]))
	}
	return out
}

// WriterWait collects, for one piece of work, how long its writes were
// queued behind the writer and what the writer ran meanwhile. The zero
// value is ready; give it to the work's context with WithWriterWait.
//
// AIDEV-NOTE: time is counted once however many of the work's writes wait
// at the same moment (an adoption's heartbeat ticker beside its commit):
// a stretch opens when the first of them queues and closes when the last
// stops waiting, and what the writer ran is taken over that stretch. So
// Total's seconds never exceed the work's wall time, and a write of the
// work itself that ran while another still waited is among what that one
// waited behind.
type WriterWait struct {
	mu     sync.Mutex
	wr     *writerState // the writer its writes queue for; set by the first
	queued int          // writes of the work waiting now
	since  time.Time    // when queued last left zero
	base   writerTotals // what the writer had run then
	waited time.Duration
	ahead  writerTotals // over the stretches that have closed
}

type writerWaitKey struct{}

// WithWriterWait returns a ctx whose writes report their waits to w.
func WithWriterWait(ctx context.Context, w *WriterWait) context.Context {
	return context.WithValue(ctx, writerWaitKey{}, w)
}

// enter notes that a write of the work queues for s. It returns the time
// and what the writer had run by then.
//
// AIDEV-NOTE: lock order is WriterWait.mu, then writerState.mu (here and
// in Waiting); nothing takes them the other way round. The totals are
// read under w.mu so that two of the work's writes cannot open and close a
// stretch out of order.
func (w *WriterWait) enter(s *writerState) (time.Time, writerTotals) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now, t := s.totals(false)
	w.wr = s
	if w.queued == 0 {
		w.since, w.base = now, t
	}
	w.queued++
	return now, t
}

// leave notes that a write of the work stopped waiting at now, when the
// writer had run t.
func (w *WriterWait) leave(now time.Time, t *writerTotals) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.queued--
	if w.queued > 0 {
		return
	}
	w.waited += now.Sub(w.since)
	ahead := t.since(&w.base)
	w.ahead.add(&ahead)
}

// Total returns how long the work had at least one write queued, and what
// the writer ran in that time, by kind of write. A wait still in progress
// is not in it (Waiting).
func (w *WriterWait) Total() (seconds float64, ahead map[string]WriterAhead) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.waited.Seconds(), w.ahead.report()
}

// Waiting reports the work's wait in progress: false when none of its
// writes is queued.
func (w *WriterWait) Waiting() (WriterWaiting, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.queued == 0 {
		return WriterWaiting{}, false
	}
	now, done, running, since, busy := w.wr.view()
	// The op that was running when the stretch opened is in base with the
	// time it had run by then; until it finishes, done lacks it, and
	// report leaves a kind with no finished op out.
	finished := done.since(&w.base)
	out := WriterWaiting{Seconds: now.Sub(w.since).Seconds(), Ahead: finished.report()}
	if busy {
		out.Running, out.RunningSeconds = running.String(), now.Sub(since).Seconds()
	}
	return out, true
}
