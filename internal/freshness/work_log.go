package freshness

import (
	"context"
	"crypto/rand"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/observability"
)

// backgroundWork covers a suite lock owner or a read-only maintenance probe. Its
// watchdog never writes heartbeats to the database: a blocked writer must not
// silence the diagnostic that explains why publication stopped.
type backgroundWork struct {
	id, kind, adoptionID string
	suite                SuiteRef
	started              time.Time
	phase                atomic.Pointer[phaseMark]
	waits                observability.Waits
	// writer collects how long the work's database writes were queued
	// behind the cache's one writer goroutine, and what that goroutine ran
	// meanwhile (cache.WriterWait).
	writer         cache.WriterWait
	logger         *slog.Logger
	timer          *time.Timer
	timerMu        sync.Mutex
	stopped        bool
	once           sync.Once
	busyMu         sync.Mutex
	lastBusy       time.Time
	busySuppressed int
}

type workKey struct{}

func newBackgroundWork(logger *slog.Logger, suite SuiteRef, kind, adoptionID string, interval time.Duration) *backgroundWork {
	w := &backgroundWork{id: rand.Text(), kind: kind, adoptionID: adoptionID,
		suite: suite, started: time.Now(), logger: logger}
	w.waits.WorkID, w.waits.WorkKind = w.id, kind
	w.setPhase("prepare")
	if interval > 0 {
		w.timerMu.Lock()
		w.timer = time.AfterFunc(interval, func() { w.tick(interval) })
		w.timerMu.Unlock()
	}
	return w
}

// Short checks allocate no watchdog goroutine. finish and the callback serialize
// so no progress event can appear after the work's terminal record.
//
// AIDEV-NOTE: adoption_finished tells an attempt's whole story, but only
// once it has ended; an attempt that is stuck, or merely slow, was
// invisible until then (the 2026-11-14 publication-delay review: "the
// update is upstream and the cache does not have it" could not be told
// from "an adoption has been in its flip barrier for two minutes").
// adoption_in_progress first rode on the database heartbeat ticker
// (adoption_log.go, logProgress); it is logged from this timer so that a
// blocked database writer cannot silence it.
func (w *backgroundWork) tick(interval time.Duration) {
	w.timerMu.Lock()
	defer w.timerMu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			unloggedPanic(func() { logPanic(w.logger, "background_work_panicked", "work_watchdog", w.suite, r) })
		}
	}()
	if w.stopped {
		return
	}
	w.timer = time.AfterFunc(interval, func() { w.tick(interval) })
	if w.kind == "adoption" {
		w.logger.Info("adoption_in_progress", w.attrs()...)
	} else {
		w.logger.Info("background_work_in_progress", w.attrs()...)
	}
}

func (w *backgroundWork) setPhase(phase string) {
	w.phase.Store(&phaseMark{name: phase, since: time.Now()})
}

func (w *backgroundWork) context(ctx context.Context) context.Context {
	ctx = cache.WithWriterWait(context.WithValue(ctx, workKey{}, w), &w.writer)
	return observability.WithWaits(ctx, &w.waits)
}

func workPhase(ctx context.Context, phase string) {
	if w, _ := ctx.Value(workKey{}).(*backgroundWork); w != nil {
		w.setPhase(phase)
	}
}

// attrs describes the work as it is now. While one of its database writes
// is queued it also says for how long, what the writer is running, and
// what the writer has finished since (writerWaiting).
func (w *backgroundWork) attrs() []any {
	return append(w.identity(), w.writerWaiting()...)
}

// identity is the work's own fields, without its wait for the writer.
func (w *backgroundWork) identity() []any {
	p := w.phase.Load()
	a := []any{"work_id", w.id, "work_kind", w.kind, "canonical_scheme", w.suite.CanonicalScheme,
		"canonical_host", w.suite.CanonicalHost, "suite_path", w.suite.SuitePath,
		"phase", p.name, "phase_seconds", time.Since(p.since).Seconds(),
		"elapsed_seconds", time.Since(w.started).Seconds(), "waits", w.waits.Snapshot()}
	if w.adoptionID != "" {
		a = append(a, "adoption_id", w.adoptionID)
	}
	return a
}

// writerWaiting is the fields of a wait for the database writer that is
// still in progress; nil when none of the work's writes is queued.
//
// AIDEV-NOTE: this reads the cache's in-memory account of its writer, never
// the database: the watchdog's lines must keep coming while the writer is
// blocked (backgroundWork's own comment), and this is the line that then
// says what blocks it.
func (w *backgroundWork) writerWaiting() []any {
	now, waiting := w.writer.Waiting()
	if !waiting {
		return nil
	}
	a := []any{"writer_waiting_seconds", now.Seconds}
	if now.Running != "" {
		a = append(a, "writer_running", now.Running, "writer_running_seconds", now.RunningSeconds)
	}
	return append(a, "writer_ahead", now.Ahead)
}

// writerTotal is the fields of the work's finished waits for the database
// writer: for how long it had a write queued, and what the writer ran in
// that time.
func (w *backgroundWork) writerTotal() []any {
	seconds, ahead := w.writer.Total()
	return []any{"writer_wait_seconds", seconds, "writer_ahead", ahead}
}

func (w *backgroundWork) finish() {
	w.once.Do(func() {
		w.timerMu.Lock()
		w.stopped = true
		if w.timer != nil {
			w.timer.Stop()
		}
		w.timerMu.Unlock()
		// Short checks already have freshness_check; do not log every cooldown
		// hit. Repairs have a lifecycle even when they finish without a member.
		if w.kind == "adoption" || w.kind == "repair" || w.kind == "reconcile" || time.Since(w.started) >= time.Minute {
			unloggedPanic(func() {
				w.logger.Info("background_work_finished", append(w.identity(), w.writerTotal()...)...)
			})
		}
	})
}

func (c *Checker) startWork(suite SuiteRef, kind, adoptionID string) *backgroundWork {
	interval := time.Minute
	logger := c.logger
	if kind == "adoption" && c.adopter != nil {
		interval, logger = c.adopter.progressInterval, c.adopter.logger
	}
	w := newBackgroundWork(logger, suite, kind, adoptionID, interval)
	c.work.Store(suiteKey(suite.CanonicalScheme, suite.CanonicalHost, suite.SuitePath), w)
	return w
}

func (c *Checker) finishWork(w *backgroundWork) {
	w.finish()
	c.work.CompareAndDelete(suiteKey(w.suite.CanonicalScheme, w.suite.CanonicalHost, w.suite.SuitePath), w)
}

func (c *Checker) logBusy(ref SuiteRef) {
	reason := "suite_lock"
	if v, ok := c.work.Load(suiteKey(ref.CanonicalScheme, ref.CanonicalHost, ref.SuitePath)); ok {
		w := v.(*backgroundWork)
		reason = w.kind
		w.busyMu.Lock()
		defer w.busyMu.Unlock()
		if w.kind == "check" && time.Since(w.started) < time.Second {
			w.busySuppressed++
		} else if time.Since(w.lastBusy) >= time.Minute {
			a := append(w.attrs(), "reason", reason, "suppressed", w.busySuppressed)
			unloggedPanic(func() { c.logger.Info("freshness_check_busy", a...) })
			w.lastBusy, w.busySuppressed = time.Now(), 0
		} else {
			w.busySuppressed++
		}
	}
	freshnessSkippedTotal.Inc(reason, ref.CanonicalHost)
}
