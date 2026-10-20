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
	// junkGrace is ruling R-F3's grace (design note 8): a legacy junk
	// suite's cached anchor rows must all have been fetched at least this
	// long ago, so a transient soft-404 is never taken for junk.
	junkGrace = 24 * time.Hour
	// markerTTL is spec C9's orphan-marker TTL: a signed-seen marker no
	// suite row of its host has for its base or the base's twin is swept
	// once it was last seen this long ago.
	markerTTL = 365 * 24 * time.Hour
	// dailyEvery is how often a run also sweeps the orphan markers (design
	// note 20, ruling PF-4): at the first run after a start, then a day
	// apart (monotonic).
	dailyEvery = 24 * time.Hour
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
	// sniffs holds the previous run's junk verdicts by blob (ruling R-F3);
	// only the run's goroutine touches it.
	sniffs map[sniffKey]bool
	// dailyAt is when the marker sweep last ran (monotonic; zero: due);
	// reportedMarker is the marker stamped furthest in the future that the
	// previous sweep found (design review M-10). Only the run's goroutine
	// touches them; tests zero dailyAt to force a sweep.
	dailyAt        time.Time
	reportedMarker cache.SignedSeenMark
}

// futureStamp is a suite's stamp found in the future.
type futureStamp struct {
	key   cache.SuiteKey
	stamp int64
}

// sniffKey keys a junk verdict: what a blob's head is to one kind of
// anchor never changes, since the pool is content-addressed.
type sniffKey struct {
	file aptpath.SuiteFile
	hash string
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
// clock defers it, reap at most reapsPerRun of the candidates — idle,
// non-canonical and legacy junk (junkCandidates, every run: ruling PF-4)
// — those reaped regardless of idleness first, then the oldest, and,
// when a day has passed since it last ran, sweep the orphan signed-seen
// markers (sweepMarkers); then sweep the reconcile memo against the
// current snapshots.
//
// AIDEV-NOTE: design note 20, ruling PF-4. Only the marker sweep is
// daily: it works in years (its 365-day TTL) and costs a writer
// transaction per batch. A run the forward-step guard defers neither
// reaps nor sweeps, and leaves the sweep due for the next run (design
// review M-8): a step makes markers look older too.
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
		for _, c := range cands[:min(len(cands), reapsPerRun)] {
			if ctx.Err() != nil {
				return
			}
			r.reapSuite(ctx, c)
		}
	}
	if !deferred && (r.dailyAt.IsZero() || time.Since(r.dailyAt) >= dailyEvery) {
		r.dailyAt = time.Now()
		r.sweepMarkers(ctx, now)
	}
	r.sweepReconcileMemo(rows)
}

// sweepMarkers runs spec C9's orphan-marker sweep (ruling R-F5,
// cache.SweepOrphanMarkers) with the cutoff now − markerTTL: it deletes
// the orphans last seen before the cutoff, logging
// signed_seen_markers_swept when it deleted any, and reports the marker
// stamped furthest in the future through the rate-limited
// clock_step_detected (stamp=last_seen_at) at the first sweep after a
// start and whenever that marker or its stamp changes (design review
// M-10). It never rewrites a stamp (ruling PF-1).
func (r *Reaper) sweepMarkers(ctx context.Context, now time.Time) {
	res, err := r.cache.SweepOrphanMarkers(ctx, now.Unix(), now.Add(-markerTTL).Unix())
	var furthest cache.SignedSeenMark
	if res.Furthest != nil {
		furthest = *res.Furthest
		if furthest != r.reportedMarker {
			r.checker.clockStep("last_seen_at", furthest.CanonicalScheme, furthest.CanonicalHost, furthest.Base,
				furthest.SeenAt, now)
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
// (ListUnadoptedSuiteAnchors) plus pool reads only for the blobs no
// earlier run sniffed. Two things keep a real suite: the grace (design
// note 8: a transient soft-404 that replaced a good anchor is younger; its
// next check or a later miss heals the row) and the content rule (ruling
// PF-3, design note 9: a plain-text Release cached as InRelease is a
// §6.1-quarantined plain-text suite phase E adopts unsigned, not junk).
// Adoption records are ignored: a failed adoption's record,
// unsigned_disabled or unsigned_too_large would otherwise keep an HTML
// anchor served forever (a suite without a snapshot has its url_path
// metadata served as cached, never revalidated). Only blobs read and
// sniffed are evidence: a missing or unreadable pool file keeps the suite
// (overview: an I/O fault is never evidence). The evidence is not read
// again under the hold, which re-reads only the current snapshot
// (reapSuite): between the scan and the hold a junk suite's anchor rows
// change only through an adoption, which that comparison catches, or
// through url_path GC or a miss that re-fetched a row whose pool file had
// gone, after which the reap costs the suite one fetch at its next
// request. The sniffs run here, on the reaper's goroutine, never on the
// writer (ruling PD-5).
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
	listed := map[cache.SuiteKey]bool{}
	for _, c := range have {
		listed[c.key] = true
	}
	cutoff := now.Add(-junkGrace).Unix()
	next := map[sniffKey]bool{}
	var fails junkReadFailures
	var out []reapCandidate
	for i := range rows {
		s := &rows[i].SuiteFreshness
		key := cache.SuiteKey{CanonicalScheme: s.CanonicalScheme, CanonicalHost: s.CanonicalHost, SuitePath: s.SuitePath}
		as := bySuite[key]
		if listed[key] || len(as) == 0 || !r.allJunk(key, as, cutoff, next, &fails) {
			continue
		}
		out = append(out, reapCandidate{key: key, requested: s.LastRequestedAt, reason: "junk", cutoff: cache.ReapRegardless})
	}
	r.sniffs = next
	if fails.n > 0 {
		r.warn(ctx, "suite reaper: junk anchor reads failed", "count", fails.n, "err", fails.first,
			"canonical_host", fails.host, "path", fails.path)
	}
	return out
}

// junkReadFailures counts one junk scan's failed pool reads, keeping the
// first, so a scan logs one line however many fail (design review M-4).
type junkReadFailures struct {
	n          int
	first      error
	host, path string
}

// allJunk reports whether every one of key's cached anchor rows holds a
// blob fetched at or before cutoff that is junk evidence (anchorEvidence).
// A row without a blob or a fetch stamp, a fetch stamp after cutoff (a
// future one included: the clock-step rule), or a pool file that is
// missing or does not read, is no evidence; a read that fails for another
// reason than a missing file is counted in fails. Each verdict goes into
// next; one the previous run reached (r.sniffs) is reused without reading
// the pool file again, so a run reads only the anchors it has not sniffed
// (at most 64 KiB each).
func (r *Reaper) allJunk(key cache.SuiteKey, as []cache.SuiteAnchorRow, cutoff int64, next map[sniffKey]bool, fails *junkReadFailures) bool {
	for _, a := range as {
		if a.BlobHash == nil || a.LastFetchedAt == nil || *a.LastFetchedAt > cutoff {
			return false
		}
		file := aptpath.ClassifySuiteFile(key.SuitePath, a.Path)
		k := sniffKey{file: file, hash: *a.BlobHash}
		junk, ok := r.sniffs[k]
		if !ok {
			n := file.HeadBytes()
			if file == aptpath.FileInRelease {
				n = aptpath.FileRelease.HeadBytes() // the Release sniff reads up to 64 KiB
			}
			head, err := readCapped(r.cache.BlobPath(*a.BlobHash), int64(n))
			if err != nil && !errors.Is(err, errAnchorTooLarge) {
				if !errors.Is(err, fs.ErrNotExist) {
					if fails.n == 0 {
						fails.first, fails.host, fails.path = err, key.CanonicalHost, a.Path
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

// anchorEvidence reports whether head, the start of a cached anchor of
// kind file at path, is junk evidence (ruling PF-3): it fails its own
// sniff and, for an InRelease, the Release sniff too — a plain-text
// Release cached as InRelease is a §6.1-quarantined plain-text suite on
// its way to an unsigned adoption through Release, not junk.
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
