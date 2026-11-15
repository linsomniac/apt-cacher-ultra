package freshness

import (
	"context"
	"crypto/rand"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

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
	logger               *slog.Logger
	timer                *time.Timer
	timerMu              sync.Mutex
	stopped              bool
	once                 sync.Once
	busyMu               sync.Mutex
	lastBusy             time.Time
	busySuppressed       int
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
	return observability.WithWaits(context.WithValue(ctx, workKey{}, w), &w.waits)
}

func workPhase(ctx context.Context, phase string) {
	if w, _ := ctx.Value(workKey{}).(*backgroundWork); w != nil {
		w.setPhase(phase)
	}
}

func (w *backgroundWork) attrs() []any {
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
			unloggedPanic(func() { w.logger.Info("background_work_finished", w.attrs()...) })
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
