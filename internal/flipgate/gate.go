// Package flipgate is the spec C8 flip barrier (flat-repositories design,
// limitation #2). An adoption's commit — the switch of a suite's current
// snapshot — waits until every client that received the suite's old
// anchor (InRelease or Release) has had the quiet period to fetch the
// matching indexes:
//
//   - Phase A waits, at most PhaseAMax, for natural quiet: no anchor of
//     the suite in flight, none completed within the quiet period, no hold
//     in progress anywhere, and none that held requests ended within the
//     quiet period.
//   - The flip then takes the process-wide hold token (FIFO, one holder at
//     a time, waiting at most TokenWaitMax; on timeout it commits unheld).
//   - Phase B holds the suite's new anchor requests until it is quiet and
//     no other suite's hold that held requests ended within the quiet
//     period, commits, and answers the held requests from the new
//     snapshot — all within HoldMax of the grant. Once the cross-suite
//     condition holds, the wait for the suite's own quiet is cut short at
//     HoldMax − CommitMargin: the commit starts then (counted), so the
//     held requests still get the new snapshot. At HoldMax the hold ends
//     regardless (the backstop for a commit that has not started or not
//     finished).
//
// Close (the daemon's shutdown signal) ends the hold in progress and
// abandons every flip that has not started its commit.
//
// AIDEV-NOTE: the gate never waits while holding its mutex: every wait is
// a select on channels, timers and contexts, so it runs inside
// testing/synctest bubbles. A Token references its entry instance, so an
// entry that was purged, or expired and re-created, is never confused by
// a late Done. Every release is idempotent and the hold's end is deferred:
// a commit that fails or panics still ends its hold, answers its waiters
// and passes the token on. Code that a recover further up can unwind (the
// adoption's, net/http's, the expiry timer's own) unlocks the mutex in a
// defer, so a panic never leaves the gate locked.
package flipgate

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Spec C11 constants, not knobs.
//
// AIDEV-NOTE: MaxQuiet ≤ HoldMax − CommitMargin (so MaxQuiet < HoldMax) is
// load-bearing (TestNew_QuietBounds pins it): phase B (b) waits a quiet
// period after the last other hold that held requests, which ended at the
// latest when this hold started, so it is always satisfied by HoldMax −
// CommitMargin. Every hold therefore commits by then, inside its bound,
// and answers its held requests from the new snapshot; the straddle
// guarantee relies on it.
const (
	// MinQuiet and MaxQuiet bound a non-zero adoption.flip_quiet_period;
	// zero disables the barrier.
	MinQuiet = time.Second
	MaxQuiet = 15 * time.Second
	// PhaseAMax bounds phase A, the wait for natural quiet without holds.
	PhaseAMax = 60 * time.Second
	// TokenWaitMax bounds the wait for the process-wide hold token; a flip
	// that times out commits without a hold (acu_flip_hold_skipped_total).
	TokenWaitMax = 60 * time.Second
	// HoldMax bounds a hold, from the token's grant, and so every held
	// anchor request; it stays below apt's 30 s Acquire::http::Timeout.
	HoldMax = 20 * time.Second
	// CommitMargin is the end of a hold kept for its commit. Once no other
	// suite's hold that held requests ended within the quiet period (phase
	// B (b)), a hold waits for its own suite's quiet only until HoldMax −
	// CommitMargin, then commits; anchors still being sent or finished
	// within the quiet period then are the residual
	// (acu_flip_quiet_cut_short_total).
	//
	// AIDEV-NOTE: the time a commit normally needs, so that the held
	// waiters are answered from the new snapshot inside the 20 s bound,
	// not released to the old one right before the commit.
	CommitMargin = 5 * time.Second
	// AnchorTimeout is apt's 30 s Acquire::http::Timeout. apt's is an
	// inactivity timeout, but the barrier treats an anchor response still
	// being sent this long after it started as abandoned and stops waiting
	// for it, so one slow link cannot stall a flip or force a hold to
	// expire. A client that does finish it may straddle (a documented
	// residual, counted in acu_flip_slow_anchor_reads_total); its
	// completion still counts while no hold of its suite is in progress.
	AnchorTimeout = 30 * time.Second
	// MaxWaiters caps the requests waiting in one hold; a request beyond
	// it is served at once, counts as in flight and is counted
	// (acu_flip_waiters_over_cap_total).
	MaxWaiters = 256
)

var (
	// ErrQuietOutOfRange rejects a quiet period that is neither zero nor
	// within [MinQuiet, MaxQuiet].
	ErrQuietOutOfRange = errors.New("flipgate: flip quiet period must be 0s or between 1s and 15s")
	// ErrAborted reports that the flip's context ended, or the gate was
	// closed, before its commit started, so nothing was committed. Flip
	// wraps the cause: the context's error, or ErrClosed.
	ErrAborted = errors.New("flipgate: flip abandoned before the commit")
	// ErrClosed is the cause of a flip abandoned because Close ran.
	ErrClosed = errors.New("flipgate: gate closed")
)

// Config configures a Gate.
type Config struct {
	// Quiet is adoption.flip_quiet_period; zero disables the barrier.
	Quiet time.Duration
}

// Outcome says how a Flip ended (the adoption_flip_barrier outcome).
type Outcome string

// Flip outcomes.
const (
	OutcomeDisabled    Outcome = "disabled"     // no barrier: committed at once
	OutcomeHeld        Outcome = "held"         // committed within its hold
	OutcomeHoldExpired Outcome = "hold_expired" // the hold reached HoldMax before the commit finished
	OutcomeHoldSkipped Outcome = "hold_skipped" // no token within TokenWaitMax: committed unheld
	OutcomeAborted     Outcome = "aborted"      // ctx ended or the gate closed before the commit started: nothing committed
	// OutcomeQuietCutShort: committed within its hold at HoldMax −
	// CommitMargin, before the suite was quiet (its late readers may
	// straddle).
	OutcomeQuietCutShort Outcome = "quiet_cut_short"
)

// Result describes one Flip.
type Result struct {
	Outcome   Outcome
	PhaseA    time.Duration // waiting for natural quiet
	TokenWait time.Duration // waiting for the hold token
	HoldStart time.Time     // zero when no hold started
	HoldEnd   time.Time     // zero when no hold started
	Waiters   int           // anchor requests this flip's hold held
	Committed bool          // commit ran
}

// Stats counts what a gate did since New (tests, diagnostics).
type Stats struct {
	Flips        int // Flip calls that ran the barrier
	Holds        int // holds started
	HoldsExpired int // holds that reached HoldMax before their commit finished
	HoldsSkipped int // flips that committed unheld after waiting TokenWaitMax
	Aborted      int // flips whose context ended, or whose gate closed, before the commit started
	HeldRequests int // anchor requests that waited in a hold
	// QuietCutShort counts holds that committed at HoldMax − CommitMargin
	// before their suite was quiet.
	QuietCutShort int
}

// Gate is the flip barrier, keyed by Key. A nil *Gate is a disabled gate.
type Gate struct {
	quiet time.Duration
	// The spec C11 bounds, copied from the constants by New; unexported so
	// that only this package's unit tests shorten them.
	phaseAMax, tokenWait, holdMax time.Duration
	// onExpire is a test seam (nil in production): it runs, with the mutex
	// held, whenever a hold reaches its bound.
	onExpire func()
	// closedCh is closed by Close; flips waiting for the token watch it.
	closedCh chan struct{}

	mu        sync.Mutex
	entries   map[string]*entry
	changed   chan struct{} // closed on every change a flip may wait for; nil until a flip asks for it after a broadcast
	holder    *hold         // the hold in progress, nil if none
	queue     []*tokenWaiter
	lastEnd   time.Time // when the most recent hold that held requests ended (zero: none yet)
	lastEndBy *entry    // whose hold that was
	prevEnd   time.Time // the most recent such end by an entry other than lastEndBy
	lastSweep time.Time
	closed    bool
	stats     Stats
}

type entry struct {
	inflight map[*Token]struct{}
	lastDone time.Time // completion of the newest counted anchor serve
	hold     *hold     // this suite's hold, while phase B runs
	flips    int       // Flip calls in progress for this key
	lastUse  time.Time
}

type hold struct {
	e         *entry
	start     time.Time
	end       time.Time
	released  chan struct{} // closed when the hold ends
	waiting   int           // requests waiting now (MaxWaiters applies)
	heldTotal int           // requests that waited in this hold
	ended     bool
	expired   bool
}

type tokenWaiter struct {
	e       *entry
	granted chan struct{}
	hold    *hold // set under the gate's mutex before granted is closed
}

// Token is one anchor serve registered with the gate.
type Token struct {
	g     *Gate
	e     *entry
	start time.Time
	held  time.Duration // the wait in a hold; set before AnchorStart returns
	done  bool          // guarded by g.mu
}

// Held reports how long the request waited in a hold before AnchorStart
// returned (zero when it did not wait, or for a nil token). The handler
// logs it as the request line's flip_held_ms.
func (t *Token) Held() time.Duration {
	if t == nil {
		return 0
	}
	return t.held
}

// New returns a gate. A zero Quiet returns a disabled gate: AnchorStart
// registers nothing and Flip commits at once (the pre-C8 behaviour).
func New(cfg Config) (*Gate, error) {
	if q := cfg.Quiet; q != 0 && (q < MinQuiet || q > MaxQuiet) {
		return nil, fmt.Errorf("%w: %v", ErrQuietOutOfRange, cfg.Quiet)
	}
	return &Gate{
		quiet:     cfg.Quiet,
		phaseAMax: PhaseAMax,
		tokenWait: TokenWaitMax,
		holdMax:   HoldMax,
		closedCh:  make(chan struct{}),
		entries:   map[string]*entry{},
	}, nil
}

// Key is the gate key of a suite: the freshness checker's suite key.
func Key(scheme, host, suitePath string) string {
	return scheme + "|" + host + "|" + suitePath
}

// Enabled reports whether the barrier runs.
func (g *Gate) Enabled() bool { return g != nil && g.quiet > 0 }

// Quiet returns the quiet period (zero for a nil or disabled gate).
func (g *Gate) Quiet() time.Duration {
	if g == nil {
		return 0
	}
	return g.quiet
}

// Stats returns the gate's counters.
func (g *Gate) Stats() Stats {
	if g == nil {
		return Stats{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stats
}

// Len returns the number of suites the gate tracks.
func (g *Gate) Len() int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.entries)
}

// Purge drops key's entry when it is idle, and reports whether it did.
// Spec C8: an entry is purged when its suite's row is deleted — reaped,
// evicted or forgotten (the checker's HoldSuite release(true)). A live
// entry is kept: a flip of it in progress, an anchor serve of it in
// flight, or one that completed within the quiet period. It expires on
// its own once idle for twice the quiet period (sweepLocked).
//
// AIDEV-NOTE: ruling R-F2 (phase D final review, concurrency M4). The
// deleter's release(true) runs after its commit, so a re-creation of the
// suite can register a serve before it; dropping that entry would let the
// re-created suite's next flip skip the serve's quiet period — its client
// could fetch the new snapshot's indexes for the old anchor. Purging a
// live entry was never needed: a dropped entry's tokens touch only it, and
// an idle one goes with the sweep (TestGate_PurgeSkipsLiveEntries,
// TestHoldSuite_ReleaseKeepsARecreatedSuitesLiveGateEntry).
func (g *Gate) Purge(key string) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.entries[key]
	if e == nil || e.liveLocked(time.Now(), g.quiet) {
		return false
	}
	delete(g.entries, key)
	return true
}

// liveLocked reports whether a flip of e is in progress or holds, an
// anchor serve of it is in flight, or one completed within quiet.
func (e *entry) liveLocked(now time.Time, quiet time.Duration) bool {
	return e.flips > 0 || e.hold != nil || len(e.inflight) > 0 ||
		(!e.lastDone.IsZero() && now.Sub(e.lastDone) < quiet)
}

// Close ends the barrier at shutdown; main calls it at the signal, before
// the listeners drain (design note 18). The hold in progress ends — its
// waiters are answered from the snapshot current then — no hold starts
// again, and every Flip that has not started its commit, waiting now or
// called later, returns ErrAborted wrapping ErrClosed. A commit that has
// started runs to its end. Idempotent. A nil or disabled gate ignores it:
// with flip_quiet_period = 0s Flip commits at once, as before.
func (g *Gate) Close() {
	if !g.Enabled() {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	g.closed = true
	close(g.closedCh)
	if g.holder != nil {
		g.endHoldLocked(g.holder, false)
	}
	g.broadcastLocked()
}

// Closed reports whether Close has run; a nil or disabled gate, which
// ignores Close, is never closed. The adopter's first-adoption skip, which
// commits without Flip, asks it so that it honours the shutdown signal too.
func (g *Gate) Closed() bool {
	if !g.Enabled() {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closed
}

// AnchorStart registers an anchor serve of key (spec C8, handler side) and
// returns its token; Done must run once the response is written. It
// returns at once unless a flip of key is holding anchors: then it waits
// until that hold ends — at most HoldMax, and while ctx lasts — unless
// MaxWaiters requests already wait. A request whose ctx ends while it
// waits (its client left, or the handler is closing) is not registered:
// its token's Done does nothing. A disabled gate returns nil; Done on a
// nil Token is a no-op.
func (g *Gate) AnchorStart(ctx context.Context, key string) *Token {
	if !g.Enabled() {
		return nil
	}
	t, e, h := g.join(key)
	if t != nil {
		return t
	}
	joined := time.Now()
	timer := time.NewTimer(g.holdMax)
	defer timer.Stop()
	left := false
	select {
	case <-h.released:
	case <-timer.C:
	case <-ctx.Done():
		left = true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	h.waiting--
	now := time.Now()
	if left {
		return &Token{held: now.Sub(joined)}
	}
	t = g.registerLocked(e, now)
	t.held = now.Sub(joined)
	return t
}

// join registers an anchor serve of key and returns its token or, while a
// flip of key holds anchors and fewer than MaxWaiters requests wait in that
// hold, joins the hold instead: a nil token, with the entry and the hold.
// A request beyond the cap is served at once and counted.
func (g *Gate) join(key string) (*Token, *entry, *hold) {
	g.mu.Lock()
	defer g.mu.Unlock() // net/http recovers a panicking request: never leave the gate locked
	now := time.Now()
	e := g.entryLocked(key, now)
	h := e.hold
	if h == nil || h.waiting >= MaxWaiters {
		if h != nil {
			waitersOverCapTotal.Inc() // spec C8 residual: served at once
		}
		return g.registerLocked(e, now), nil, nil
	}
	h.waiting++
	h.heldTotal++
	g.stats.HeldRequests++
	return nil, e, h
}

// Done records the serve's completion and drops it from its suite's
// in-flight set. A serve that ran for AnchorTimeout or longer is counted
// (acu_flip_slow_anchor_reads_total) and records its completion only while
// no hold of its suite is in progress (design note 2). Safe on a nil or
// unregistered Token, and idempotent.
func (t *Token) Done() {
	if t == nil || t.g == nil {
		return
	}
	g := t.g
	g.mu.Lock()
	defer g.mu.Unlock()
	if t.done {
		return
	}
	t.done = true
	now := time.Now()
	delete(t.e.inflight, t)
	t.e.lastUse = now
	late := now.Sub(t.start) >= AnchorTimeout
	if late {
		slowAnchorReadsTotal.Inc()
	}
	// A late completion can only delay phase A by the quiet period; during
	// a hold it could push the hold into expiry, so it is ignored there.
	if (!late || t.e.hold == nil) && now.After(t.e.lastDone) {
		t.e.lastDone = now
	}
	g.broadcastLocked()
}

// Flip runs commit behind the barrier for key and reports how it went.
// ctx is the shutdown signal (the freshness checker's lifetime context),
// never the adoption's deadline (ruling R-C2): when it ends, or the gate
// is closed, before the commit starts, Flip returns ErrAborted and commits
// nothing; a commit that has started always runs to its end. commit's
// error is returned as is. While an enabled gate's Flip runs, it counts in
// acu_flip_barrier_active.
func (g *Gate) Flip(ctx context.Context, key string, commit func() error) (Result, error) {
	var res Result
	if err := ctx.Err(); err != nil {
		err = g.abort(&res, err)
		return res, err
	}
	if !g.Enabled() {
		res.Outcome, res.Committed = OutcomeDisabled, true
		return res, commit()
	}
	start := time.Now()
	barrierActive.Inc()
	defer barrierActive.Dec()
	g.mu.Lock()
	e := g.entryLocked(key, start)
	e.flips++
	g.stats.Flips++
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		e.flips--
		e.lastUse = time.Now()
	}()

	// Phase A: natural quiet, no holds.
	limit := start.Add(g.phaseAMax)
	for {
		g.mu.Lock()
		now := time.Now()
		closed := g.closed
		okLocal, atLocal := g.localQuietLocked(e, now)
		okHolds, atHolds := g.holdsQuietLocked(now)
		changed := g.changedLocked()
		g.mu.Unlock()
		if closed {
			res.PhaseA = time.Since(start)
			err := g.abort(&res, ErrClosed)
			return res, err
		}
		if (okLocal && okHolds) || !now.Before(limit) {
			break
		}
		if !waitFor(ctx, changed, earliest(atLocal, atHolds), limit) {
			res.PhaseA = time.Since(start)
			err := g.abort(&res, ctx.Err())
			return res, err
		}
	}
	res.PhaseA = time.Since(start)

	// The hold token.
	asked := time.Now()
	h, err := g.acquire(ctx, e)
	res.TokenWait = time.Since(asked)
	if err != nil {
		err = g.abort(&res, err)
		return res, err
	}
	if h == nil {
		// Spec C8 residual: no token within the wait, so commit unheld.
		if err := g.stopErr(ctx); err != nil {
			err = g.abort(&res, err)
			return res, err
		}
		g.mu.Lock()
		g.stats.HoldsSkipped++
		g.mu.Unlock()
		holdSkippedTotal.Inc()
		res.Outcome, res.Committed = OutcomeHoldSkipped, true
		return res, commit()
	}
	res.HoldStart = h.start
	err = g.runHold(ctx, e, h, commit, &res)
	return res, err
}

// acquire takes the hold token for e in FIFO order, waiting at most the
// token wait; the hold starts at the grant. It returns (nil, nil) when the
// wait timed out and (nil, err) when ctx ended or the gate closed first.
func (g *Gate) acquire(ctx context.Context, e *entry) (*hold, error) {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil, ErrClosed
	}
	if g.holder == nil && len(g.queue) == 0 {
		h := g.startHoldLocked(e, time.Now())
		g.mu.Unlock()
		return h, nil
	}
	w := &tokenWaiter{e: e, granted: make(chan struct{})}
	g.queue = append(g.queue, w)
	g.mu.Unlock()

	timer := time.NewTimer(g.tokenWait)
	defer timer.Stop()
	var cause error
	select {
	case <-w.granted:
		return w.hold, nil
	case <-timer.C:
	case <-ctx.Done():
		cause = ctx.Err()
	case <-g.closedCh:
		cause = ErrClosed
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if w.hold != nil {
		// Granted as it gave up: take it, or pass it straight on.
		if cause == nil {
			return w.hold, nil
		}
		g.endHoldLocked(w.hold, false)
		return nil, cause
	}
	for i, q := range g.queue {
		if q == w {
			g.queue = append(g.queue[:i], g.queue[i+1:]...)
			break
		}
	}
	return nil, cause
}

// runHold is phase B for hold h of entry e. Its deferred end releases the
// hold, its waiters and the token whatever happens, a panicking commit
// included (spec C5: the barrier releases its pending state through its
// own defer).
//
// AIDEV-NOTE: once phase B (b) holds, the hold waits for its suite's local
// quiet only until cut = expiry − CommitMargin; then it commits inside the
// hold (OutcomeQuietCutShort, acu_flip_quiet_cut_short_total), so its held
// requests are answered from the new snapshot. The late readers — anchor
// responses still being sent at the cut, or finished less than the quiet
// period before it, i.e. reads still running HoldMax − CommitMargin −
// quiet (5 s at the default 10 s) into the hold — are the residual: a
// client among them whose index request reaches the cache after the commit
// straddles. Releasing the held requests to the old snapshot at the bound,
// right before the commit, made nearly every one of them straddle (final
// review, concurrency Important 1). MaxQuiet ≤ HoldMax − CommitMargin means
// phase B (b) always holds by the cut, so the expiry below is only the
// backstop: for a commit that has not started by the bound (unreachable
// with the production constants) or that does not finish in time.
func (g *Gate) runHold(ctx context.Context, e *entry, h *hold, commit func() error, res *Result) error {
	expiry := h.start.Add(g.holdMax)
	cut := expiry.Add(-CommitMargin)
	defer func() {
		g.mu.Lock()
		defer g.mu.Unlock() // a panicking commit unwinds to the adoption's recover
		g.endHoldLocked(h, false)
		res.HoldEnd, res.Waiters = h.end, h.heldTotal
		if h.expired && (res.Outcome == OutcomeHeld || res.Outcome == OutcomeQuietCutShort) {
			res.Outcome = OutcomeHoldExpired
		}
	}()
	cutShort := false
	for {
		g.mu.Lock()
		now := time.Now()
		closed := g.closed
		okLocal, atLocal := g.localQuietLocked(e, now)
		okOthers, atOthers := g.otherHoldsQuietLocked(e, now)
		changed := g.changedLocked()
		g.mu.Unlock()
		if closed {
			return g.abort(res, ErrClosed)
		}
		if !now.Before(expiry) {
			// The backstop (spec C8): at the bound pending is cleared, the
			// waiters are answered from the current snapshot, and a commit
			// not yet started starts at once (counted).
			if g.expireHold(h) {
				holdExpiredTotal.Inc()
			}
			if err := g.stopErr(ctx); err != nil {
				return g.abort(res, err)
			}
			res.Outcome, res.Committed = OutcomeHoldExpired, true
			return commit()
		}
		if okOthers && (okLocal || !now.Before(cut)) {
			cutShort = !okLocal
			break
		}
		limit := expiry
		if now.Before(cut) {
			limit = cut
		}
		if !waitFor(ctx, changed, earliest(atLocal, atOthers), limit) {
			return g.abort(res, ctx.Err())
		}
	}
	if err := g.stopErr(ctx); err != nil {
		return g.abort(res, err)
	}
	res.Outcome, res.Committed = OutcomeHeld, true // the deferred end makes it hold_expired if the bound came first
	if cutShort {
		g.mu.Lock()
		g.stats.QuietCutShort++
		g.mu.Unlock()
		quietCutShortTotal.Inc()
		res.Outcome = OutcomeQuietCutShort
	}
	// Commit within the hold. A commit that outlasts the bound is never
	// interrupted, but the hold still ends at the bound.
	expire := time.AfterFunc(time.Until(expiry), func() {
		defer func() {
			if recover() != nil {
				gatePanicsTotal.Inc() // a gate bug: counted; the deferred unlock kept the gate usable
			}
		}()
		if g.expireHold(h) {
			holdExpiredTotal.Inc()
		}
	})
	defer expire.Stop()
	err := commit()
	expire.Stop() // design review M11: an expiry the timer has not fired by now did not happen
	return err
}

// expireHold ends h at its bound (runHold's loop and its timer) and
// reports whether this call ended it.
func (g *Gate) expireHold(h *hold) bool {
	g.mu.Lock()
	defer g.mu.Unlock() // the timer recovers a panic: never leave the gate locked
	if g.onExpire != nil {
		g.onExpire()
	}
	return g.endHoldLocked(h, true)
}

// stopErr reports why a flip must not start its commit: ErrClosed once the
// gate is closed, else ctx's error (nil while both last).
func (g *Gate) stopErr(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return ErrClosed
	}
	return ctx.Err()
}

// abort records a flip abandoned before its commit.
func (g *Gate) abort(res *Result, cause error) error {
	if g != nil {
		g.mu.Lock()
		g.stats.Aborted++
		g.mu.Unlock()
	}
	res.Outcome, res.Committed = OutcomeAborted, false
	return fmt.Errorf("%w: %w", ErrAborted, cause)
}

func (g *Gate) entryLocked(key string, now time.Time) *entry {
	g.sweepLocked(now)
	e := g.entries[key]
	if e == nil {
		e = &entry{inflight: map[*Token]struct{}{}}
		g.entries[key] = e
	}
	e.lastUse = now
	return e
}

// sweepLocked drops entries idle for twice the quiet period (spec C8), at
// most once per quiet period. An idle entry's last completion is older
// than the quiet period, so dropping it loses nothing a flip waits for.
func (g *Gate) sweepLocked(now time.Time) {
	if now.Sub(g.lastSweep) < g.quiet {
		return
	}
	g.lastSweep = now
	for k, e := range g.entries {
		if len(e.inflight) == 0 && e.hold == nil && e.flips == 0 && now.Sub(e.lastUse) >= 2*g.quiet {
			delete(g.entries, k)
		}
	}
}

func (g *Gate) registerLocked(e *entry, now time.Time) *Token {
	t := &Token{g: g, e: e, start: now}
	e.inflight[t] = struct{}{}
	e.lastUse = now
	return t
}

func (g *Gate) startHoldLocked(e *entry, now time.Time) *hold {
	h := &hold{e: e, start: now, released: make(chan struct{})}
	g.holder, e.hold = h, h
	g.stats.Holds++
	return h
}

// endHoldLocked ends h once and reports whether this call did: the
// suite's anchors are no longer held, the waiters proceed (reading the
// snapshot current then), and the token passes to the next waiting flip,
// whose hold starts now — unless the gate is closed.
//
// AIDEV-NOTE: lastEnd/lastEndBy/prevEnd answer "the most recent hold end
// by another suite" in O(1): holds never overlap, so the newest end by an
// entry other than x is lastEnd when lastEndBy != x, and prevEnd if not.
// Only a hold that held at least one request sets them: a hold that held
// no request delayed no client queue, so it never starts a cross-suite
// quiet period (design note 17). This must never panic: the expiry
// timer's recover counts a panic (acu_flip_gate_panics_total) but keeps
// neither its value nor its stack.
func (g *Gate) endHoldLocked(h *hold, expired bool) bool {
	if h.ended {
		return false
	}
	now := time.Now()
	h.ended, h.expired, h.end = true, expired, now
	if expired {
		g.stats.HoldsExpired++
	}
	if h.e.hold == h {
		h.e.hold = nil
	}
	h.e.lastUse = now
	close(h.released)
	if h.heldTotal > 0 {
		if g.lastEndBy != h.e {
			g.prevEnd = g.lastEnd
		}
		g.lastEnd, g.lastEndBy = now, h.e
	}
	if g.holder == h {
		g.holder = nil
		if len(g.queue) > 0 && !g.closed {
			w := g.queue[0]
			g.queue = g.queue[1:]
			w.hold = g.startHoldLocked(w.e, now)
			close(w.granted)
		}
	}
	g.broadcastLocked()
	return true
}

// changedLocked returns the channel the next change closes, creating it on
// demand. Only a flip asks for it, at each pass of its wait loop, before it
// knows whether it will wait (deferred minor C-2: a flip that then commits
// at once leaves the channel allocated until the next broadcast — one
// channel, harmless); a change while no flip has asked since the last
// broadcast allocates nothing.
func (g *Gate) changedLocked() <-chan struct{} {
	if g.changed == nil {
		g.changed = make(chan struct{})
	}
	return g.changed
}

// broadcastLocked wakes every waiting flip: each re-locks the gate and
// re-checks its own conditions.
//
// AIDEV-NOTE: one global channel for every suite (deferred minor C-3,
// closed in phase F): every anchor completion in the cache, of any suite,
// wakes every waiting flip, which re-scans its own suite's in-flight map
// and asks for a new channel and timer. The cost is bounded by the flips
// waiting at once — one or two under the default pool, since a flip keeps
// its global adoption slot through the barrier (ruling R-C1) — for at most
// the barrier's 140 s. Per-entry wake channels would remove it but change
// the wake mechanism every barrier guarantee rests on, for no measured
// gain; revisit if max_concurrent_adoptions grows large.
func (g *Gate) broadcastLocked() {
	if g.changed != nil {
		close(g.changed)
		g.changed = nil
	}
}

// localQuietLocked: no anchor of e in flight (ignoring any in flight for
// AnchorTimeout or longer) and none completed within the quiet period.
// If not, it also returns when to look again (zero: at the next change).
func (g *Gate) localQuietLocked(e *entry, now time.Time) (bool, time.Time) {
	var youngest time.Time
	for t := range e.inflight {
		if now.Sub(t.start) < AnchorTimeout && t.start.After(youngest) {
			youngest = t.start
		}
	}
	if !youngest.IsZero() {
		return false, youngest.Add(AnchorTimeout)
	}
	if !e.lastDone.IsZero() && now.Sub(e.lastDone) < g.quiet {
		return false, e.lastDone.Add(g.quiet)
	}
	return true, time.Time{}
}

// holdsQuietLocked is phase A (b): no hold in progress anywhere and none
// that held requests ended within the quiet period — such a hold delayed
// other suites' requests on a client's shared queue.
func (g *Gate) holdsQuietLocked(now time.Time) (bool, time.Time) {
	if g.holder != nil {
		return false, time.Time{}
	}
	if !g.lastEnd.IsZero() && now.Sub(g.lastEnd) < g.quiet {
		return false, g.lastEnd.Add(g.quiet)
	}
	return true, time.Time{}
}

// otherHoldsQuietLocked is phase B (b): no other suite's hold that held
// requests ended within the quiet period — clients released from it may
// still have this suite's requests queued behind the held one.
func (g *Gate) otherHoldsQuietLocked(e *entry, now time.Time) (bool, time.Time) {
	end := g.lastEnd
	if g.lastEndBy == e {
		end = g.prevEnd
	}
	if !end.IsZero() && now.Sub(end) < g.quiet {
		return false, end.Add(g.quiet)
	}
	return true, time.Time{}
}

// earliest returns the earlier of a and b, ignoring a zero time.
func earliest(a, b time.Time) time.Time {
	switch {
	case a.IsZero():
		return b
	case b.IsZero(), a.Before(b):
		return a
	}
	return b
}

// waitFor blocks until changed closes, the earlier of at (when set) and
// limit comes, or ctx ends; it reports false only when ctx ended.
func waitFor(ctx context.Context, changed <-chan struct{}, at, limit time.Time) bool {
	timer := time.NewTimer(time.Until(earliest(at, limit)))
	defer timer.Stop()
	select {
	case <-changed:
		return true
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
