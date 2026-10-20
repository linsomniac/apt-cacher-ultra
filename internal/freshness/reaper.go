package freshness

// Spec C9 (limitation #3): the suite reaper. Under the freshness checker's
// lifecycle, with its own hourly ticker, it deletes suites no client asked
// for in gc.suite_idle_ttl — and unreachable legacy rows regardless of
// idleness — through the one suite-row delete (cache.ReapIdleSuite,
// deleteSuiteTx), and keeps the in-memory state bounded by the suite table.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// Spec C9 and C11 constants, not knobs.
const (
	// reapInterval is the reaper's own cadence, independent of gc.interval
	// and of the scheduler's tick.
	reapInterval = time.Hour
	// reapUptimeFloor is how long after the start the first run waits
	// (monotonic): clients returning after a downtime touch their suites
	// first, so a long stop never reaps an active suite. A forward step of
	// the wall clock defers reaping by as long (deferredByForwardStep).
	reapUptimeFloor = time.Hour
	// reapsPerRun bounds the suites one run tries to reap (a busy one
	// counts; it is retried next run).
	reapsPerRun = 25
	// forwardStepSlack is how far the wall clock may run ahead of the
	// monotonic clock between two runs before the reaper takes it for a
	// forward step (design review M-8).
	forwardStepSlack = time.Hour
	// suiteHoldTimeout bounds the work done under one suite's hold — the
	// re-read, the metadata batches and the final delete — as commitTimeout
	// bounds CommitAdoption (design review M-2): a wedged writer releases
	// the suite after it instead of holding it until shutdown.
	suiteHoldTimeout = 5 * time.Minute
)

// ReaperConfig configures a Reaper.
type ReaperConfig struct {
	Cache   *cache.Cache
	Checker *Checker
	// IdleTTL is gc.suite_idle_ttl: a suite no client asked for in this
	// long is reaped. Zero disables suite reaping (the run's other sweeps
	// still run).
	IdleTTL time.Duration
	Logger  *slog.Logger
}

// Reaper is spec C9's suite reaper and the work behind POST /forget (D9).
type Reaper struct {
	cache   *cache.Cache
	checker *Checker
	idleTTL time.Duration
	logger  *slog.Logger
	// interval and floor are reapInterval and reapUptimeFloor; tests
	// shorten them.
	interval, floor time.Duration
	// reapHook, when set (tests only), runs in reapSuite while the suite
	// is held, with the context its steps run on: a panic there stands in
	// for a bug between the hold and its release.
	reapHook func(context.Context, cache.SuiteKey)
	// lastWall and lastMono are the checker's wall clock and the monotonic
	// clock at the previous run, or at Run's start; deferUntil is the
	// monotonic time before which a run does not reap, after a forward
	// step of the wall clock (design review M-8). reported is the furthest
	// future interest stamp the previous run found (design review M-10).
	// Only the run's goroutine touches them.
	lastWall, lastMono, deferUntil time.Time
	reported                       futureStamp
}

// futureStamp is a suite's stamp found in the future.
type futureStamp struct {
	key   cache.SuiteKey
	stamp int64
}

// NewReaper returns a reaper over cfg.Cache whose suite holds and
// in-memory purges go through cfg.Checker (its HoldSuite, the
// cache.SuiteGuard cap eviction uses).
func NewReaper(cfg ReaperConfig) (*Reaper, error) {
	if cfg.Cache == nil {
		return nil, errors.New("freshness: reaper: nil Cache")
	}
	if cfg.Checker == nil {
		return nil, errors.New("freshness: reaper: nil Checker")
	}
	if cfg.IdleTTL < 0 {
		return nil, fmt.Errorf("freshness: reaper: idle TTL must not be negative, got %v", cfg.IdleTTL)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Reaper{cache: cfg.Cache, checker: cfg.Checker, idleTTL: cfg.IdleTTL, logger: logger,
		interval: reapInterval, floor: reapUptimeFloor}, nil
}

// Run runs the reaper until ctx ends: the first run once the uptime floor
// has passed, then one every interval after the previous run finished.
// Each run recovers its own panic (logged, freshness_check_panicked with
// where=reaper), so the next run still comes.
func (r *Reaper) Run(ctx context.Context) {
	r.lastWall, r.lastMono = r.checker.now(), time.Now()
	t := time.NewTimer(r.floor)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		r.safeRun(ctx)
		t.Reset(r.interval)
	}
}

// safeRun is one run under a recover.
func (r *Reaper) safeRun(ctx context.Context) {
	defer func() {
		if v := recover(); v != nil {
			unloggedPanic(func() { r.checker.reportPanic("freshness_check_panicked", "reaper", SuiteRef{}, v) })
		}
	}()
	r.runOnce(ctx)
}

// reapCandidate is a suite one run may reap.
type reapCandidate struct {
	key       cache.SuiteKey
	current   *int64 // the current snapshot the run listed (the compare-and-delete's)
	requested *int64 // last_requested_at as listed; oldest first
	reason    string // suite_reaped's reason: idle, non_canonical or junk
	cutoff    int64  // ReapIdleSuite's cutoff
}

// runOnce is one reaper run (spec C9): unless a forward step of the wall
// clock defers it, reap at most reapsPerRun of the candidates, those
// reaped regardless of idleness first, then the oldest; then sweep the
// reconcile memo against the current snapshots.
func (r *Reaper) runOnce(ctx context.Context) {
	rows, err := r.cache.ListSuitesWithAdoption(ctx)
	if err != nil {
		r.warn(ctx, "suite reaper: list suites failed", "err", err)
		return
	}
	now := r.checker.now()
	deferred := r.deferredByForwardStep(now, time.Now())
	if !deferred && r.idleTTL > 0 {
		cands := r.candidates(rows, now)
		for _, c := range cands[:min(len(cands), reapsPerRun)] {
			if ctx.Err() != nil {
				return
			}
			r.reapSuite(ctx, c)
		}
	}
	r.sweepReconcileMemo(rows)
}

// deferredByForwardStep is the reaper's guard against a forward step of
// the wall clock that no restart re-arms (design review M-8): a VM resumed
// after a long pause, a laptop after a long suspend, NTP stepping the
// clock ahead. It compares how far the wall clock (the checker's) and the
// monotonic clock advanced since the previous run, or Run's start; when
// the wall clock ran more than forwardStepSlack ahead it logs
// clock_step_detected (stamp=wall_clock) and defers reaping by the uptime
// floor, giving clients the hour a restart gives them to touch their
// suites. It reports whether this run is deferred.
//
// AIDEV-NOTE: the monotonic clock does not advance while a machine is
// suspended or a VM paused, and the wall clock does; the uptime floor
// covers only a fresh start. The wall clock is compared in whole seconds
// (Unix), never through time.Time.Sub, which uses the monotonic readings
// when both values carry one. A backward step needs no guard here: a
// future interest stamp is never idle (candidates).
func (r *Reaper) deferredByForwardStep(now, mono time.Time) bool {
	if !r.lastMono.IsZero() {
		elapsed := mono.Sub(r.lastMono)
		ahead := time.Duration(now.Unix()-r.lastWall.Unix())*time.Second - elapsed
		if ahead > forwardStepSlack {
			r.logger.Warn("clock_step_detected", "stamp", "wall_clock",
				"stored_unixtime", r.lastWall.Unix()+int64(elapsed/time.Second), "now_unixtime", now.Unix())
			r.deferUntil = mono.Add(r.floor)
		}
	}
	r.lastWall, r.lastMono = now, mono
	return mono.Before(r.deferUntil)
}

// candidates lists the suites a run may reap, in sortCandidates' order:
// rows whose suite path the grammar can no longer produce
// (aptpath.IsCanonicalSuitePath: unreachable legacy rows), regardless of
// idleness, and suites no client asked for in idleTTL.
//
// AIDEV-NOTE: the reap cutoff is a gate that retires things (spec C4
// clock-step rule): a last_requested_at in the future (a backward clock
// step) reads as now — never idle — and the furthest is reported through
// the checker's rate-limited clock_step_detected at the first run after a
// start and whenever the furthest changes (design review M-10: a dead
// suite's future stamp would otherwise be reported every hour until the
// clock passed it); a NULL stamp is never idle either (the v7 backfill and
// every creation set one). A canonical path with an odd codename such as
// /foo/dists/InRelease is reaped by idleness like any suite (phase A
// carry-forward).
func (r *Reaper) candidates(rows []cache.SuiteWithAdoption, now time.Time) []reapCandidate {
	idleCutoff := now.Add(-r.idleTTL).Unix()
	futureAfter := now.Unix() + int64(clockStepTolerance/time.Second)
	var out []reapCandidate
	var furthest futureStamp
	for i := range rows {
		s := &rows[i].SuiteFreshness
		c := reapCandidate{key: cache.SuiteKey{CanonicalScheme: s.CanonicalScheme, CanonicalHost: s.CanonicalHost,
			SuitePath: s.SuitePath}, current: s.CurrentSnapshotID, requested: s.LastRequestedAt}
		switch {
		case !aptpath.IsCanonicalSuitePath(s.SuitePath):
			c.reason, c.cutoff = "non_canonical", cache.ReapRegardless
		case s.LastRequestedAt != nil && *s.LastRequestedAt > futureAfter:
			if *s.LastRequestedAt > furthest.stamp {
				furthest = futureStamp{key: c.key, stamp: *s.LastRequestedAt}
			}
			continue
		case s.LastRequestedAt != nil && *s.LastRequestedAt < idleCutoff:
			// spec C9: last_requested_at < now − gc.suite_idle_ttl;
			// deleteSuiteTx keeps a stamp after its cutoff.
			c.reason, c.cutoff = "idle", idleCutoff-1
		default:
			continue
		}
		out = append(out, c)
	}
	if furthest.stamp != 0 && furthest != r.reported {
		r.checker.clockStep("last_requested_at", furthest.key.CanonicalScheme, furthest.key.CanonicalHost,
			furthest.key.SuitePath, furthest.stamp, now)
	}
	r.reported = furthest
	sortCandidates(out)
	return out
}

// sortCandidates orders the candidates reaped regardless of idleness
// (cutoff ReapRegardless: non-canonical and junk rows) first, then by the
// oldest last_requested_at (no stamp first), then by key, so a run's
// order is fixed.
//
// AIDEV-NOTE: ruling PF-4 (design review I-4). A junk suite's clients
// touch it at every update, so by its stamp it would sort after every idle
// suite and wait for an idle backlog to drain below reapsPerRun — days
// after an upgrade — while it fails those clients' updates. The classes
// reaped regardless are legacy rows, few and finite, so they cannot starve
// idle reaping.
func sortCandidates(cands []reapCandidate) {
	stamp := func(c reapCandidate) int64 {
		if c.requested == nil {
			return -1 << 63
		}
		return *c.requested
	}
	slices.SortStableFunc(cands, func(a, b reapCandidate) int {
		if ra, rb := a.cutoff == cache.ReapRegardless, b.cutoff == cache.ReapRegardless; ra != rb {
			if ra {
				return -1
			}
			return 1
		}
		if sa, sb := stamp(a), stamp(b); sa != sb {
			if sa < sb {
				return -1
			}
			return 1
		}
		ka := a.key.CanonicalScheme + "|" + a.key.CanonicalHost + "|" + a.key.SuitePath
		kb := b.key.CanonicalScheme + "|" + b.key.CanonicalHost + "|" + b.key.SuitePath
		switch {
		case ka < kb:
			return -1
		case ka > kb:
			return 1
		}
		return 0
	})
}

// reapSuite runs spec C9's steps for one candidate under the suite's
// mutex, within suiteHoldTimeout: the metadata url_path rows in batches
// (cache.DeleteSuiteMetadata), then the final compare-and-delete
// (cache.ReapIdleSuite). A busy suite is skipped (retried next run). It
// reports whether the suite was reaped.
//
// AIDEV-NOTE: ruling R-F1. The suite is held through the checker's
// HoldSuite — the SuiteGuard cap eviction uses — before any writer
// operation, and released with release(true) only once its row is gone:
// that drops the lock-map entry and the checker's per-suite state, and the
// flip-gate entry unless a re-creation's serve keeps it live (ruling
// R-F2); ReapIdleSuite drops the touch entry. Whether the row is gone is
// ReapIdleSuite's answer, which settles a failed submit by a fresh read
// (design review M-2). The re-read under the hold is I-4's re-validation:
// a suite a client asked for (or that adopted) since the listing keeps its
// metadata rows, and one that vanished meanwhile (cap eviction) has the
// entry HoldSuite stored for it dropped. A step that fails because the
// run's context ended (shutdown) is logged at DEBUG, not WARN (design
// review M-3); one cut short by suiteHoldTimeout is a failure (WARN).
func (r *Reaper) reapSuite(ctx context.Context, c reapCandidate) bool {
	release, ok := r.checker.HoldSuite(c.key)
	if !ok {
		r.logger.Debug("suite reaper: suite busy, retried next run",
			"canonical_host", c.key.CanonicalHost, "suite_path", c.key.SuitePath)
		return false
	}
	gone := false
	defer func() { release(gone) }()
	hctx, cancel := context.WithTimeout(ctx, suiteHoldTimeout)
	defer cancel()
	if r.reapHook != nil {
		r.reapHook(hctx, c.key)
	}
	cur, err := r.cache.GetSuiteFreshness(hctx, c.key.CanonicalScheme, c.key.CanonicalHost, c.key.SuitePath)
	switch {
	case errors.Is(err, cache.ErrNotFound):
		gone = true
		return false
	case err != nil:
		r.warn(ctx, "suite reaper: read suite row failed", "err", err,
			"canonical_host", c.key.CanonicalHost, "suite_path", c.key.SuitePath)
		return false
	case !sameSnapshot(cur.CurrentSnapshotID, c.current) ||
		(c.cutoff != cache.ReapRegardless && (cur.LastRequestedAt == nil || *cur.LastRequestedAt > c.cutoff)):
		return false // asked for, or adopted, since the listing
	}
	rows, err := r.cache.DeleteSuiteMetadata(hctx, c.key)
	if err != nil {
		r.warn(ctx, "suite reaper: delete metadata failed", "err", err, "metadata_rows", rows,
			"canonical_host", c.key.CanonicalHost, "suite_path", c.key.SuitePath)
		return false
	}
	var deleted bool
	deleted, gone, err = r.cache.ReapIdleSuite(hctx, c.key, c.current, c.cutoff)
	if err != nil {
		r.warn(ctx, "suite reaper: reap failed", "err", err,
			"canonical_host", c.key.CanonicalHost, "suite_path", c.key.SuitePath)
		return false
	}
	if !deleted {
		return false
	}
	suitesReapedTotal.Inc(c.reason)
	r.logger.Info("suite_reaped",
		"canonical_scheme", c.key.CanonicalScheme,
		"canonical_host", c.key.CanonicalHost,
		"suite_path", c.key.SuitePath,
		"reason", c.reason,
		"metadata_rows", rows,
	)
	return true
}

// warn logs a failed step of a run at WARN — or at DEBUG once ctx, the
// run's context, has ended: a shutdown in the middle of a run is no
// failure (design review M-3; the E-13 and FW-3 (d) class).
func (r *Reaper) warn(ctx context.Context, msg string, args ...any) {
	if ctx.Err() != nil {
		r.logger.Debug(msg, args...)
		return
	}
	r.logger.Warn(msg, args...)
}

// sameSnapshot reports whether two optional snapshot ids are equal.
func sameSnapshot(a, b *int64) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

// sweepReconcileMemo drops the adopter's reconcile memo entries
// (Adopter.reconciledSnapshots) of snapshots no suite has as its current
// one in rows (spec C9), so the memo stays bounded by the suite table.
// Without an adopter there is no memo.
func (r *Reaper) sweepReconcileMemo(rows []cache.SuiteWithAdoption) {
	if r.checker.adopter == nil {
		return
	}
	current := make(map[int64]bool, len(rows))
	for _, s := range rows {
		if s.CurrentSnapshotID != nil {
			current[*s.CurrentSnapshotID] = true
		}
	}
	r.checker.adopter.sweepReconcileMemo(current)
}
