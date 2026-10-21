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
	"io/fs"
	"log/slog"
	"math"
	"slices"
	"strings"
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
	// reapsPerRun bounds the idle suites one run tries to reap (a busy one
	// counts; it is retried next run).
	reapsPerRun = 25
	// regardlessReapsPerRun bounds, apart from reapsPerRun, the suites one
	// run tries to reap regardless of idleness — non-canonical and legacy
	// junk rows — tried before the idle ones (ruling PF-14; a busy one
	// counts against this budget, and is retried next run).
	regardlessReapsPerRun = 250
	// forwardStepSlack is how far the wall clock may run ahead of the
	// monotonic clock between two runs before the reaper takes it for a
	// forward step (design review M-8).
	forwardStepSlack = time.Hour
	// suiteHoldTimeout bounds the work done under one suite's hold — the
	// re-read, the metadata batches and the final delete — as commitTimeout
	// bounds CommitAdoption (design review M-2): a wedged writer releases
	// the suite after it instead of holding it until shutdown.
	suiteHoldTimeout = 5 * time.Minute
	// junkGrace is ruling R-F3's grace (design note 8): a legacy junk
	// suite's cached anchor rows must all have been fetched at least this
	// long ago, so a transient soft-404 is never taken for junk.
	junkGrace = 24 * time.Hour
	// markerTTL is spec C9's orphan-marker TTL: a signed-seen marker no
	// suite row of its host has for its base or the base's twin is swept
	// once it was last seen this long ago.
	markerTTL = 365 * 24 * time.Hour
	// dailyEvery is how often a run also sweeps the orphan markers (design
	// note 20, ruling PF-4): the first sweep is due one dailyEvery after
	// Run starts (ruling PF-13: a wrong boot clock has a day to be
	// corrected), then a day apart (monotonic).
	dailyEvery = 24 * time.Hour
	// maxStepAhead is where stepAhead saturates: the largest Duration
	// (math.MaxInt64 ns, some 292 years); see deferredByForwardStep.
	maxStepAhead = time.Duration(math.MaxInt64)
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
	// reapHook, when set (tests only), runs in reapHeld and Forget while
	// the suite is held, with the context its steps run on: a panic there
	// stands in for a bug between the hold and its release. It is a test
	// seam set before Run and Forget start and only read afterwards —
	// Forget reads it on net/http's goroutines, not the run's.
	reapHook func(context.Context, cache.SuiteKey)
	// lastWall and lastMono are the checker's wall clock and the monotonic
	// clock at the previous run, or at Run's start; deferUntil is the
	// monotonic time before which a run does not reap, after a forward
	// step of the wall clock (design review M-8); stepAhead is the sum of
	// every forward step the guard detected — how far the wall clock ran
	// ahead of the monotonic clock — which the marker sweep's cutoff takes
	// off (ruling PF-13), saturating at maxStepAhead. reported is the
	// furthest future interest stamp the previous run found (design review
	// M-10). Only the run's goroutine touches them.
	lastWall, lastMono, deferUntil time.Time
	stepAhead                      time.Duration
	reported                       futureStamp
	// sniffs holds the junk verdicts the last completed scan reached, keyed
	// by (anchor kind, blob) (ruling R-F3) and, for a flat index, its name
	// (its extension picks the sniff; ruling R-G2); only the run's
	// goroutine touches it.
	sniffs map[sniffKey]bool
	// readHead reads the head of a pool file for the junk scan (readCapped;
	// tests count its reads or end the run's context at one).
	readHead func(path string, limit int64) ([]byte, error)
	// flatIndexRows reads the unadopted flat suites' root index rows for
	// the junk scan (cache.ListUnadoptedFlatIndexRows), and signedEvidence
	// reads a flat suite's signed evidence (cache.CheckUnsignedDowngrade;
	// ruling PG-7); tests count their calls or fail them.
	flatIndexRows  func(ctx context.Context) ([]cache.SuiteAnchorRow, error)
	signedEvidence func(ctx context.Context, host, suitePath string) error
	// dailyAt is when the marker sweep last ran, or when Run started
	// (monotonic; zero: due — a run outside Run); reportedMarker is the
	// marker stamped furthest in the future that the previous sweep found
	// (design review M-10). Only the run's goroutine touches them; tests
	// zero dailyAt to force a sweep.
	dailyAt        time.Time
	reportedMarker cache.SignedSeenMark
}

// futureStamp is a suite's stamp found in the future.
type futureStamp struct {
	key   cache.SuiteKey
	stamp int64
}

// sniffKey keys a junk verdict: what a blob's head is to one kind of
// anchor never changes, since the pool is content-addressed. A flat
// index's verdict also depends on its name — its extension picks the
// flat index sniff (aptpath.SniffFlatIndex) — so name is the member's
// name for aptpath.FileFlatIndex and "" for an anchor (ruling R-G2).
type sniffKey struct {
	file aptpath.SuiteFile
	hash string
	name string
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
		interval: reapInterval, floor: reapUptimeFloor, readHead: readCapped,
		flatIndexRows: cfg.Cache.ListUnadoptedFlatIndexRows, signedEvidence: cfg.Cache.CheckUnsignedDowngrade}, nil
}

// Run runs the reaper until ctx ends: the first run once the uptime floor
// has passed, then one every interval after the previous run finished;
// the first marker sweep is due one dailyEvery after Run starts (ruling
// PF-13). A panic while a suite is held is recovered per candidate
// (reapSuite); each run also recovers any other panic of its own (logged,
// freshness_check_panicked with where=reaper), so the next run still
// comes.
func (r *Reaper) Run(ctx context.Context) {
	r.lastWall, r.lastMono = r.checker.now(), time.Now()
	r.dailyAt = r.lastMono
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

// safeRun is one run under a recover: the backstop for a panic outside a
// held candidate (the listing, the junk scan, the sweeps), which ends the
// run; reapSuite recovers a held candidate's own.
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
// clock defers it, reap the candidates runBudgets allows — at most
// regardlessReapsPerRun non-canonical and legacy junk rows
// (junkCandidates, every run: ruling PF-4) first, then at most
// reapsPerRun idle suites, the oldest first (ruling PF-14) — and,
// when a day has passed since it last ran (or since Run started), sweep
// the orphan signed-seen markers (sweepMarkers); then sweep the reconcile
// memo against the current snapshots.
//
// AIDEV-NOTE: design note 20, rulings PF-4 and PF-13. Only the marker
// sweep is daily: it works in years (its 365-day TTL) and costs a writer
// transaction per batch. Its first run is due a day after Run starts, not
// at the first run (PF-13): a start with the wall clock ahead (an RTC or
// NTP fault before the start, which the guard cannot see) would make
// orphan markers look older, and a swept marker cannot come back, so a
// wrong boot clock has a day to be corrected. The cost: a daemon
// restarted at least once a day (before its first sweep comes due) never
// sweeps (orphan markers are tiny rows; the safe side), and a wrong step
// that outlives a restart uncorrected for a day still ages markers by it
// (documented residual). A run the forward-step guard defers neither reaps
// nor sweeps, and leaves the sweep due for the next run (design review
// M-8); a detected step no longer ages markers at all (stepAhead,
// sweepMarkers).
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
		cands = append(cands, r.junkCandidates(ctx, rows, cands, now)...)
		sortCandidates(cands)
		for _, c := range runBudgets(cands) {
			if ctx.Err() != nil {
				return
			}
			r.reapSuite(ctx, c)
		}
	}
	if !deferred && (r.dailyAt.IsZero() || time.Since(r.dailyAt) >= dailyEvery) {
		r.dailyAt = time.Now()
		r.sweepMarkers(ctx, now, r.checker.now())
	}
	r.sweepReconcileMemo(rows)
}

// sweepMarkers runs spec C9's orphan-marker sweep (ruling R-F5,
// cache.SweepOrphanMarkers) with the cutoff now − stepAhead − markerTTL,
// now being the run's and stepAhead the forward steps the guard detected
// (ruling PF-13): it deletes the orphans last seen before the cutoff, logging
// signed_seen_markers_swept when it deleted any, and reports the marker
// stamped more than clockStepTolerance past fresh — a clock read taken
// for the sweep — furthest in the future through clock_step_detected
// (stamp=last_seen_at; counted, and logged directly: clockStepUnlimited)
// at the first sweep after a start and whenever that marker or its stamp
// changes (design review M-10). It never rewrites a stamp (ruling PF-1).
//
// AIDEV-NOTE: two clocks on purpose (the Task 9 review's Important 1).
// Markers are stamped while the run goes on — its own reaps move their
// bases' markers up to the cache's clock (M-9, ruling PF-10) and the
// checker's probes re-mark at every sighting — so the future threshold is
// the fresh read: against the run's start, a run whose reaps took more
// than 2 s would report its own stamps as a clock step. The cutoff keeps
// the run's now, which the forward-step guard checked: a fresh one would
// let a step during the run sweep markers the guard would have deferred
// (design review M-8). stepAhead is ruling PF-13 (final review concurrency
// I1): a detected forward step never ages a marker. The guard's hour
// gives clients time to touch their suites, but an orphan marker's suite
// has had no client for a month, so nothing would refresh the marker in
// that hour, and a swept marker — the never-downgrade record of a base
// seen signed — cannot come back. After a genuine suspend this only keeps
// markers longer, the safe side.
func (r *Reaper) sweepMarkers(ctx context.Context, now, fresh time.Time) {
	// Two Adds, never the one Duration sum −stepAhead − markerTTL, which
	// would wrap once stepAhead nears maxStepAhead (deferredByForwardStep).
	res, err := r.cache.SweepOrphanMarkers(ctx, fresh.Unix(), now.Add(-markerTTL).Add(-r.stepAhead).Unix())
	var furthest cache.SignedSeenMark
	if res.Furthest != nil {
		furthest = *res.Furthest
		if furthest != r.reportedMarker {
			r.checker.clockStepUnlimited("last_seen_at", furthest.CanonicalScheme, furthest.CanonicalHost, furthest.Base,
				furthest.SeenAt, fresh)
		}
	}
	r.reportedMarker = furthest
	if res.Swept > 0 {
		r.logger.Info("signed_seen_markers_swept", "count", res.Swept)
	}
	if err != nil {
		r.warn(ctx, "suite reaper: marker sweep failed", "err", err, "swept", res.Swept)
	}
}

// deferredByForwardStep is the reaper's guard against a forward step of
// the wall clock that no restart re-arms (design review M-8): a VM resumed
// after a long pause, a laptop after a long suspend, NTP stepping the
// clock ahead. It compares how far the wall clock (the checker's) and the
// monotonic clock advanced since the previous run, or Run's start; when
// the wall clock ran more than forwardStepSlack ahead it logs
// clock_step_detected (stamp=wall_clock), defers reaping by the uptime
// floor, giving clients the hour a restart gives them to touch their
// suites, and adds the step to stepAhead, which the marker sweep's cutoff
// takes off (ruling PF-13). It reports whether this run is deferred.
//
// AIDEV-NOTE: the monotonic clock does not advance while a machine is
// suspended or a VM paused, and the wall clock does; the uptime floor
// covers only a fresh start. The wall clock is compared in whole seconds
// (Unix), never through time.Time.Sub, which uses the monotonic readings
// when both values carry one. A backward step needs no guard here: a
// future interest stamp is never idle (candidates). stepAhead only grows,
// for the life of the process: a step later corrected backwards, or a
// genuine suspend, then keeps markers longer — the safe side, since the
// sweep's deletions are permanent and reaping's are not (a reaped suite
// re-adopts at its next request). It saturates at maxStepAhead (the final
// fix wave's re-review, reaper minor 3): a sum that wrapped negative would
// put the sweep's cutoff centuries ahead and sweep every orphan marker. At
// the bound it stays there, and the cutoff, which subtracts markerTTL and
// stepAhead in two time.Time.Add calls (sweepMarkers), lies centuries
// back: the sweep deletes nothing.
func (r *Reaper) deferredByForwardStep(now, mono time.Time) bool {
	if !r.lastMono.IsZero() {
		elapsed := mono.Sub(r.lastMono)
		ahead := time.Duration(now.Unix()-r.lastWall.Unix())*time.Second - elapsed
		if ahead > forwardStepSlack {
			r.logger.Warn("clock_step_detected", "stamp", "wall_clock",
				"stored_unixtime", r.lastWall.Unix()+int64(elapsed/time.Second), "now_unixtime", now.Unix())
			r.deferUntil = mono.Add(r.floor)
			// 0 <= stepAhead <= maxStepAhead, so the difference cannot wrap.
			r.stepAhead += min(ahead, maxStepAhead-r.stepAhead)
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
// clock_step_detected (counted, and logged directly: clockStepUnlimited)
// at the first run after a start and whenever the furthest changes
// (design review M-10: a dead suite's future stamp would otherwise be
// reported every hour until the clock passed it). Only an idle candidate
// is kept by its future stamp: a non-canonical row is reaped regardless
// and not reported, and a junk suite is reported and still reaped as junk
// (junkCandidates never reads the stamp). A NULL stamp is never idle
// either (the v7 backfill and every creation set one). A canonical path
// with an odd codename such as /foo/dists/InRelease is reaped by idleness
// like any suite (phase A carry-forward).
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
		r.checker.clockStepUnlimited("last_requested_at", furthest.key.CanonicalScheme, furthest.key.CanonicalHost,
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
// AIDEV-NOTE: rulings PF-4 (design review I-4) and PF-14 (final review
// concurrency m2). A junk suite's clients touch it at every update, so by
// its stamp it would sort after every idle suite and wait for an idle
// backlog to drain — days after an upgrade — while it fails those
// clients' updates; so the classes reaped regardless go first. PF-4
// assumed them few; the upgraded table the docs describe (more than 5,000
// suites from an upstream that answers missing InRelease files with an
// HTML page) is that class, which one shared budget of 25 would drain in
// some 200 runs with no idle reaping meanwhile. So each class has its own
// budget (runBudgets): regardlessReapsPerRun of these, cheap reaps (no
// snapshot; a junk suite has at most three anchor rows, a non-canonical
// row deletes no metadata), and reapsPerRun idle suites in the same run.
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

// runBudgets returns the candidates one run tries, from cands in
// sortCandidates' order (those reaped regardless of idleness first): at
// most regardlessReapsPerRun of those, then at most reapsPerRun idle ones
// (ruling PF-14). A try counts whether the candidate is reaped, kept or
// busy.
func runBudgets(cands []reapCandidate) []reapCandidate {
	n := 0
	for n < len(cands) && cands[n].cutoff == cache.ReapRegardless {
		n++
	}
	regardless, idle := cands[:n], cands[n:]
	out := make([]reapCandidate, 0, min(len(regardless), regardlessReapsPerRun)+min(len(idle), reapsPerRun))
	out = append(out, regardless[:min(len(regardless), regardlessReapsPerRun)]...)
	return append(out, idle[:min(len(idle), reapsPerRun)]...)
}

// reapSuite runs spec C9's steps for one candidate under the suite's
// mutex, within suiteHoldTimeout (reapHeld): the metadata url_path rows in
// batches (cache.DeleteSuiteMetadata), then the final compare-and-delete
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
// (design review M-2); a failed submit settled gone is a reap (logged
// suite_reaped with settled=true, and counted: the Task 6 review's minor
// 3). The re-read under the hold is I-4's re-validation: a suite a client
// asked for (or that adopted) since the listing keeps its metadata rows,
// and one that vanished meanwhile (cap eviction) has the entry HoldSuite
// stored for it dropped. A step that fails because the run's context
// ended (shutdown) is logged at DEBUG, not WARN (design review M-3); one
// cut short by suiteHoldTimeout is a failure (WARN). A panic while the
// suite is held is recovered here, per candidate, as checkRecovered
// recovers a check (the Task 6 review's minor 2): it is logged with the
// suite (freshness_check_panicked, where=reaper), the hold is released
// with the cache's fresh answer — the panic may have struck after a
// commit — and the run goes on to its next candidate and its sweeps.
func (r *Reaper) reapSuite(ctx context.Context, c reapCandidate) (reaped bool) {
	release, ok := r.checker.HoldSuite(c.key)
	if !ok {
		r.logger.Debug("suite reaper: suite busy, retried next run", "canonical_scheme", c.key.CanonicalScheme,
			"canonical_host", c.key.CanonicalHost, "suite_path", c.key.SuitePath)
		return false
	}
	gone := false
	defer func() {
		if v := recover(); v != nil {
			gone, reaped = r.settledGone(c.key), false
			unloggedPanic(func() {
				r.checker.reportPanic("freshness_check_panicked", "reaper", SuiteRef{CanonicalScheme: c.key.CanonicalScheme,
					CanonicalHost: c.key.CanonicalHost, SuitePath: c.key.SuitePath}, v)
			})
		}
		release(gone)
	}()
	hctx, cancel := context.WithTimeout(ctx, suiteHoldTimeout)
	defer cancel()
	h := r.reapHeld(hctx, c)
	gone = h.gone
	settled := h.err != nil && h.gone // a failed submit whose delete committed (the cache's fresh read)
	if h.err != nil && !settled {
		args := []any{"err", h.err, "canonical_scheme", c.key.CanonicalScheme, "canonical_host", c.key.CanonicalHost,
			"suite_path", c.key.SuitePath}
		if h.step == stepDeleteMetadata {
			args = append(args, "metadata_rows", h.rows)
		}
		r.warn(ctx, h.step, args...)
		return false
	}
	if !h.deleted && !settled {
		return false
	}
	suitesReapedTotal.Inc(c.reason)
	attrs := []any{
		"canonical_scheme", c.key.CanonicalScheme,
		"canonical_host", c.key.CanonicalHost,
		"suite_path", c.key.SuitePath,
		"reason", c.reason,
		"metadata_rows", h.rows,
	}
	if settled {
		attrs = append(attrs, "settled", true)
	}
	r.logger.Info("suite_reaped", attrs...)
	return true
}

// The reaper's held steps, as their failure lines name them.
const (
	stepReadRow        = "suite reaper: read suite row failed"
	stepDeleteMetadata = "suite reaper: delete metadata failed"
	stepReap           = "suite reaper: reap failed"
)

// heldReap is what reapHeld did under a suite's hold.
type heldReap struct {
	step    string // the failed step's line (stepReadRow, …); "" when none failed
	err     error
	rows    int  // metadata url_path rows deleted
	deleted bool // ReapIdleSuite deleted the row
	gone    bool // the suite has no row: the re-read's answer, or ReapIdleSuite's
}

// reapHeld runs c's held steps on hctx, reapSuite's bounded context: the
// re-read (I-4's re-validation), the metadata batches and the final
// compare-and-delete. It logs nothing: reapSuite logs on the run's
// context, which tells a shutdown (DEBUG) from a step cut short by
// suiteHoldTimeout (WARN).
//
// AIDEV-NOTE: the steps are a function of hctx alone (the Task 6 review's
// minor 1): with the run's context in scope, one step run on it instead of
// hctx would escape design review M-2's bound unseen — the tests time out
// only the first step (TestReaper_AHoldTimeoutIsAWarnedFailureAndTheNextRunReaps)
// and check only the hook's deadline
// (TestReaper_BoundsTheWorkUnderAHoldAtFiveMinutes). Keep the run's
// context out of this function.
func (r *Reaper) reapHeld(hctx context.Context, c reapCandidate) (h heldReap) {
	if r.reapHook != nil {
		r.reapHook(hctx, c.key)
	}
	cur, err := r.cache.GetSuiteFreshness(hctx, c.key.CanonicalScheme, c.key.CanonicalHost, c.key.SuitePath)
	switch {
	case errors.Is(err, cache.ErrNotFound):
		h.gone = true
		return h
	case err != nil:
		h.step, h.err = stepReadRow, err
		return h
	case !sameSnapshot(cur.CurrentSnapshotID, c.current) ||
		(c.cutoff != cache.ReapRegardless && (cur.LastRequestedAt == nil || *cur.LastRequestedAt > c.cutoff)):
		return h // asked for, or adopted, since the listing
	}
	if h.rows, err = r.cache.DeleteSuiteMetadata(hctx, c.key); err != nil {
		h.step, h.err = stepDeleteMetadata, err
		return h
	}
	if h.deleted, h.gone, err = r.cache.ReapIdleSuite(hctx, c.key, c.current, c.cutoff); err != nil {
		h.step, h.err = stepReap, err
	}
	return h
}

// settleReadTimeout bounds settledGone's read.
const settleReadTimeout = 5 * time.Second

// settledGone reports whether key's row is gone, read on a fresh context
// (the run's, or Forget's, may have ended): after a panic under the hold,
// the cache's answer decides how the hold is released, since the panic
// may have struck after a commit. A read error answers false — present, the
// conservative release (the cache's settle answers so too) — and so does
// a panic in the read: settledGone runs in the panic recovery of
// reapSuite and of Forget, and whatever it does the hold must still be
// released.
func (r *Reaper) settledGone(key cache.SuiteKey) (gone bool) {
	defer func() {
		if recover() != nil {
			gone = false
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), settleReadTimeout)
	defer cancel()
	ok, err := r.cache.HasSuiteRow(ctx, key)
	return err == nil && !ok
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

// junkCandidates returns ruling R-F3's legacy junk suites that are not
// already in have, as candidates reaped regardless of idleness: a suite
// without a current snapshot whose cached anchor rows (InRelease, Release,
// Release.gpg) — at least one — were all fetched at least junkGrace ago
// and are all junk evidence (anchorEvidence). A read failure of the anchor
// rows skips the rule for this run; pool reads that fail are logged once,
// with their count (design review M-4).
//
// AIDEV-NOTE: legacy junk is a suite a v6 binary created for an HTML page
// served as InRelease: every anchor serve with a status below 400 touches
// it, so idleness never reaps it, and the hot path does not sniff (ruling
// R-F3). The scan runs every run (ruling PF-4): such a suite fails its
// clients' updates, and the scan is one indexed read on the pool
// (ListUnadoptedSuiteAnchors) plus pool reads only for the blobs whose
// verdict neither this scan nor the last completed one reached (the memo,
// below). Two things keep a real suite: the grace (design note 8: a
// transient soft-404 that replaced a good anchor is younger; its next
// check or a later miss heals the row) and the content rule (ruling PF-3,
// design note 9: a plain-text Release cached as InRelease is a
// §6.1-quarantined plain-text suite phase E adopts unsigned, not junk).
// Adoption records are ignored: a failed adoption's record,
// unsigned_disabled or unsigned_too_large would otherwise keep an HTML
// anchor served forever (a suite without a snapshot has its url_path
// metadata served as cached, never revalidated). Only blobs read and
// sniffed are evidence: a missing or unreadable pool file keeps the suite
// (overview: an I/O fault is never evidence) unless this scan or the last
// completed one reached the blob's verdict — a blob the last completed
// scan sniffed or reused keeps its verdict, since the pool is
// content-addressed; a completed scan keeps only the verdicts it reached
// (r.sniffs = next), and allJunk stops at a suite's first row that is no
// evidence, so an older verdict that scan did not reach is gone — and an
// anchor no sniff applies to (aptpath.NotSuiteFile: a suite path over
// MaxSuitePathBytes) is never evidence and never read. Ruling R-G2, as
// amended by ruling PG-7 (suiteJunk): an unadopted flat suite whose base
// has no signed evidence (cache.CheckUnsignedDowngrade finds no marker and
// no signed snapshot of the base or its twin) is judged on its anchor rows
// and its cached root Packages*/Sources* rows together
// (cache.ListUnadoptedFlatIndexRows, one more indexed read in a run that
// holds an unadopted flat suite: hasUnadoptedFlatSuite) —
// junk only when it has at least one such row and every one is junk
// evidence by the same rules (aptpath.SniffFlatIndex for an index's name;
// fetched at least junkGrace ago; whatever its record); a flat suite with
// signed evidence, and every dists/ suite, is judged by its anchors alone.
// The signed-evidence read is lazy, per candidate: only a flat suite the
// two rules judge differently reads it. A failed read of the index rows
// leaves every unadopted flat suite unjudged for the run, and a failed
// signed-evidence read the suite (both logged). The evidence is not
// read again under the hold, which re-reads only the current snapshot
// (reapHeld): between the scan and the hold a junk suite's anchor rows
// change only through an adoption, which that comparison catches, or
// through url_path GC, a miss that re-fetched a row whose pool file had
// gone, or — for an unadopted flat suite's anchor and index rows — the
// anchorless refresh replacing or deleting them, after which the reap
// costs the suite one fetch at its next request. The sniffs run here, on the reaper's goroutine, never on the
// writer (ruling PD-5). The scan checks ctx per suite: a shutdown does not
// wait for the first run's reads after a restart (up to 64 KiB per anchor
// not yet sniffed); cut short, it returns no candidate and keeps the memo
// the last completed scan left.
func (r *Reaper) junkCandidates(ctx context.Context, rows []cache.SuiteWithAdoption, have []reapCandidate, now time.Time) []reapCandidate {
	anchors, err := r.cache.ListUnadoptedSuiteAnchors(ctx)
	if err != nil {
		r.warn(ctx, "suite reaper: list unadopted anchors failed", "err", err)
		return nil
	}
	bySuite := map[cache.SuiteKey][]cache.SuiteAnchorRow{}
	for _, a := range anchors {
		bySuite[a.SuiteKey] = append(bySuite[a.SuiteKey], a)
	}
	// Ruling R-G2's evidence for flat suites (as amended by ruling PG-7),
	// read only when there is one to judge.
	//
	// AIDEV-NOTE: ruling R-G5 (deferred minor F-12) with ruling PG-7's
	// gate: a run with no unadopted flat suite reads no index rows
	// (hasUnadoptedFlatSuite), so a cache of dists/ suites pays nothing for
	// R-G2 — the 90-day reaping simulation
	// (TestDoD_NinetyDaysOfReapingWithRestarts) keeps its 4.5 s budget
	// under -race (an unconditional read cost it about 20 %).
	byIndexes := map[cache.SuiteKey][]cache.SuiteAnchorRow{}
	indexesRead := true
	if hasUnadoptedFlatSuite(rows) {
		indexes, err := r.flatIndexRows(ctx)
		if err != nil {
			r.warn(ctx, "suite reaper: list unadopted flat indexes failed", "err", err)
			indexesRead = false
		}
		for _, a := range indexes {
			byIndexes[a.SuiteKey] = append(byIndexes[a.SuiteKey], a)
		}
	}
	listed := map[cache.SuiteKey]bool{}
	for _, c := range have {
		listed[c.key] = true
	}
	cutoff := now.Add(-junkGrace).Unix()
	next := map[sniffKey]bool{}
	var fails junkReadFailures
	var evidenceFails int
	var evidenceErr error
	var evidenceKey cache.SuiteKey
	var out []reapCandidate
	for i := range rows {
		if ctx.Err() != nil {
			return nil // a shutdown (the Task 7 review's minor 7): r.sniffs keeps the last completed scan's verdicts
		}
		s := &rows[i].SuiteFreshness
		key := cache.SuiteKey{CanonicalScheme: s.CanonicalScheme, CanonicalHost: s.CanonicalHost, SuitePath: s.SuitePath}
		flat := unadoptedFlat(s)
		if listed[key] || (flat && !indexesRead) {
			continue // a failed index read: no verdict for an unadopted flat suite this run
		}
		junk, err := r.suiteJunk(ctx, key, flat, bySuite[key], byIndexes[key], cutoff, next, &fails)
		if err != nil {
			if evidenceFails == 0 {
				evidenceErr, evidenceKey = err, key
			}
			evidenceFails++
			continue // an I/O fault is never evidence: no verdict this run
		}
		if !junk {
			continue
		}
		// current stays nil: the compare under the hold (reapHeld) and the
		// compare-and-delete in ReapIdleSuite then require a suite still
		// unadopted, the only kind the junk rule reaps.
		out = append(out, reapCandidate{key: key, current: nil, requested: s.LastRequestedAt, reason: "junk",
			cutoff: cache.ReapRegardless})
	}
	r.sniffs = next
	if fails.n > 0 {
		r.warn(ctx, "suite reaper: junk anchor reads failed", "count", fails.n, "err", fails.first,
			"canonical_scheme", fails.scheme, "canonical_host", fails.host, "path", fails.path)
	}
	if evidenceFails > 0 {
		r.warn(ctx, "suite reaper: read signed evidence failed", "count", evidenceFails, "err", evidenceErr,
			"canonical_scheme", evidenceKey.CanonicalScheme, "canonical_host", evidenceKey.CanonicalHost,
			"suite_path", evidenceKey.SuitePath)
	}
	return out
}

// suiteJunk reports whether key's evidence rows are all junk evidence
// (allJunk): its anchor rows and — for an unadopted flat suite (flat)
// whose base has no signed evidence — its root index rows with them
// (ruling PG-7, amending R-G2). The anchors-alone verdict and the combined
// one are reached first; the signed-evidence read (signedEvidence:
// cache.CheckUnsignedDowngrade, no third "signed" predicate — ruling PG-3)
// runs only when they differ, so a dists/ suite, and a flat suite the two
// rules judge alike, never reads it. A non-nil error is that read's
// failure: no verdict.
//
// AIDEV-NOTE: ruling PG-7. Anchors alone would reap a healthy Release-less
// suite on a host that answers soft-404s for InRelease and Release every
// day, losing its good index during an outage; they stay the rule only
// where they heal something — a signed suite with a junk anchor (signed
// clients reject the junk, and anchor serves keep the suite touched, so
// only the junk reap repairs it).
func (r *Reaper) suiteJunk(ctx context.Context, key cache.SuiteKey, flat bool, anchors, indexes []cache.SuiteAnchorRow,
	cutoff int64, next map[sniffKey]bool, fails *junkReadFailures) (bool, error) {
	anchorsJunk := len(anchors) > 0 && r.allJunk(key, anchors, cutoff, next, fails)
	if !flat {
		return anchorsJunk, nil
	}
	combined := (len(anchors) == 0 || anchorsJunk) && len(anchors)+len(indexes) > 0 &&
		(len(indexes) == 0 || r.allJunk(key, indexes, cutoff, next, fails))
	if combined == anchorsJunk {
		return anchorsJunk, nil
	}
	switch err := r.signedEvidence(ctx, key.CanonicalHost, key.SuitePath); {
	case errors.Is(err, cache.ErrUnsignedDowngradeRefused):
		return anchorsJunk, nil // signed evidence: the anchors alone
	case err != nil:
		return false, err
	}
	return combined, nil
}

// hasUnadoptedFlatSuite reports whether rows hold an unadopted flat
// suite: one ruling R-G2 (as amended by ruling PG-7) may judge by its root
// index rows.
func hasUnadoptedFlatSuite(rows []cache.SuiteWithAdoption) bool {
	for i := range rows {
		if unadoptedFlat(&rows[i].SuiteFreshness) {
			return true
		}
	}
	return false
}

// unadoptedFlat reports whether s is an unadopted flat suite, the suites
// junkCandidates judges with their root index rows.
//
// AIDEV-NOTE: the gate (hasUnadoptedFlatSuite) and the per-row test in
// junkCandidates must agree (the Task 12 review's Minor 2): a suite the
// row test calls flat while the gate read no index rows would be judged
// with none, and a junk-anchored suite with a good index reaped on its
// anchors alone. One predicate keeps them equal.
func unadoptedFlat(s *cache.SuiteFreshness) bool {
	return s.CurrentSnapshotID == nil && aptpath.IsFlatSuitePath(s.SuitePath)
}

// junkReadFailures counts one junk scan's failed pool reads, keeping the
// first — its error, and its suite's scheme and host and the anchor's
// path — so a scan logs one line however many fail (design review M-4).
type junkReadFailures struct {
	n                  int
	first              error
	scheme, host, path string
}

// allJunk reports whether every one of key's evidence rows as — its cached
// anchor rows, or a flat suite's root index rows (ruling R-G2 as amended
// by PG-7; suiteJunk) — holds a blob fetched at or before cutoff that is
// junk evidence (anchorEvidence).
// A row without a blob or a fetch stamp, a fetch stamp after cutoff (a
// future one included: the clock-step rule), an anchor no sniff applies
// to (aptpath.NotSuiteFile), or a pool file that is missing or does not
// read, is no evidence; a read that fails for another reason than a
// missing file is counted in fails. Each verdict goes into next, keyed by
// (anchor kind, blob, and a flat index's name); one this scan (next) or
// the last completed scan
// (r.sniffs) reached is reused without reading the pool file again, so a
// scan reads each blob it has not sniffed once, however many suites share
// it (at most 64 KiB each). A failed read reaches no verdict and is not
// remembered, so fails.n counts failed reads, not pool files: one
// unreadable blob that N suites share is read, and counted, once for each
// of them whose scan reaches it — at most N times, since a suite already
// listed (a non-canonical or idle candidate), or one whose earlier anchor
// row is no evidence, never reaches it (the WARN's count).
func (r *Reaper) allJunk(key cache.SuiteKey, as []cache.SuiteAnchorRow, cutoff int64, next map[sniffKey]bool, fails *junkReadFailures) bool {
	for _, a := range as {
		if a.BlobHash == nil || a.LastFetchedAt == nil || *a.LastFetchedAt > cutoff {
			return false
		}
		file := aptpath.ClassifySuiteFile(key.SuitePath, a.Path)
		if file == aptpath.NotSuiteFile {
			return false // no sniff applies: never evidence, never read (the Task 7 review's minor 5)
		}
		k := sniffKey{file: file, hash: *a.BlobHash}
		if file == aptpath.FileFlatIndex {
			k.name = a.Path[strings.LastIndexByte(a.Path, '/')+1:] // its extension picks the sniff
		}
		junk, ok := next[k]
		if !ok {
			junk, ok = r.sniffs[k]
		}
		if !ok {
			n := file.HeadBytes()
			if file == aptpath.FileInRelease {
				n = aptpath.FileRelease.HeadBytes() // the Release sniff reads up to 64 KiB
			}
			head, err := r.readHead(r.cache.BlobPath(*a.BlobHash), int64(n))
			if err != nil && !errors.Is(err, errAnchorTooLarge) {
				if !errors.Is(err, fs.ErrNotExist) {
					if fails.n == 0 {
						fails.first, fails.scheme, fails.host, fails.path = err, key.CanonicalScheme, key.CanonicalHost, a.Path
					}
					fails.n++
				}
				return false
			}
			junk = anchorEvidence(file, a.Path, head)
		}
		next[k] = junk
		if !junk {
			return false
		}
	}
	return true
}

// anchorEvidence reports whether head, the start of a cached anchor (or,
// ruling R-G2, flat index) of kind file at path, is junk evidence (ruling
// PF-3): it fails its own sniff and, for an InRelease, the Release sniff
// too — a plain-text Release cached as InRelease is a §6.1-quarantined
// plain-text suite on its way to an unsigned adoption through Release,
// not junk.
func anchorEvidence(file aptpath.SuiteFile, path string, head []byte) bool {
	if file.Sniff(path, head) {
		return false
	}
	return file != aptpath.FileInRelease || !aptpath.FileRelease.Sniff(path, head)
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
