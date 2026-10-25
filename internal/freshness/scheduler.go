package freshness

import (
	"cmp"
	"context"
	"math"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// minFastTickInterval is the floor for the periodic scheduler's tick
// interval. SPEC §7.4 prescribes a "fast tick" of periodic_refresh / 4;
// this floor keeps a misconfigured periodic_refresh of, say, 1 minute
// from spinning on the cache every 15s.
const minFastTickInterval = 5 * time.Second

// staleSuiteFactor multiplies periodic_refresh to set the "this suite has
// gone silently stale" threshold for the SPEC §7.4 freshness_stale_suites
// gauge. A healthy suite refreshes every periodic_refresh; 4× gives ample
// slack for transient upstream trouble before a suite is flagged. The
// production freeze went undetected for a week precisely because no such
// signal existed.
const staleSuiteFactor = 4

// staleSuiteCounts partitions suites whose last_success_at has aged past
// staleBefore into adopted (current_snapshot_id set — the dangerous case
// that serves stale metadata) and unadopted, and counts the adopted ones
// in a refusal state that paces the suite by its record (absent, a subset
// of adopted: anchor_absent and unsigned_disabled, recordPacesAdopted;
// such a suite's last_success_at ages between its record's retries on
// purpose, spec C4). A nil
// last_success_at is treated as "never succeeded" rather than "went
// stale" and is not counted, so brand-new suites don't trip the gauge. A
// last_success_at more than clockStepTolerance past now (backward clock
// step) counts as stale (spec C4). SPEC §7.4.
//
// AIDEV-NOTE: only the refusals that pace an adopted suite by their
// record — anchor_absent (PE-2) and unsigned_disabled (PE-12), i.e.
// recordPacesAdopted — are left out of the stale WARN (design note 33 as
// narrowed by the final fix wave, FW-3 (b), and its re-review): such a
// suite is checked only at its record's retries (up to 75 min apart), so
// its last success ages with no failed check
// (TestTick_StaleWarnLeavesOutAnUnsignedDisabledSuitePacedByItsRecord).
// Every other refusal bumps last_success_at at each answered check, so a
// refused suite that is stale has failing checks — a real freeze. Leaving
// out every refusal reason (Task 7) hid it, a regression from phase D
// (TestTick_StaleWarnCountsARefusedSuiteWhoseChecksFail).
func staleSuiteCounts(suites []cache.SuiteFreshness, staleBefore, now int64) (adopted, unadopted, absent int) {
	futureAfter := now + int64(clockStepTolerance/time.Second)
	for _, s := range suites {
		if s.LastSuccessAt == nil {
			continue
		}
		// Spec C4: a success stamp in the future is infinitely old, so a
		// suite frozen across a backward clock step still raises the gauge.
		if *s.LastSuccessAt >= staleBefore && *s.LastSuccessAt <= futureAfter {
			continue
		}
		switch {
		case s.CurrentSnapshotID == nil:
			unadopted++
		case s.AdoptionFailCount > 0 && s.AdoptionFailReason != nil && recordPacesAdopted(*s.AdoptionFailReason):
			adopted++
			absent++
		default:
			adopted++
		}
	}
	return adopted, unadopted, absent
}

// failingAdoptionCount counts suites whose adoption failure record is a
// failure rather than a refusal (spec C7 gauge). tick reports 0 instead
// when no adopter is wired: a record left by an earlier run with adoption
// enabled is inert (Phase 1 parity).
func failingAdoptionCount(suites []cache.SuiteFreshness) int {
	n := 0
	for _, s := range suites {
		if s.AdoptionFailCount > 0 && s.AdoptionFailReason != nil && !cache.IsRefusalReason(*s.AdoptionFailReason) {
			n++
		}
	}
	return n
}

// checkWorkers is the fixed size of the T2 check pool (spec C4, a
// constant).
const checkWorkers = 4

// schedClass orders the suites queued on one host (spec C4), from
// persisted columns so the order survives restarts.
type schedClass int

const (
	classHealthy      schedClass = iota // checked before, check_fail_count = 0
	classNeverChecked                   // last_check_at IS NULL
	classFailing                        // check_fail_count >= 1
	numSchedClasses
)

func classOf(s cache.SuiteFreshness) schedClass {
	switch {
	case s.CheckFailCount > 0:
		return classFailing
	case s.LastCheckAt == nil:
		return classNeverChecked
	}
	return classHealthy
}

type schedItem struct {
	ref     SuiteRef
	key     string
	waiting bool // parked after a queued result; its re-check bypasses the cooldown
	signed  bool // for waiting items: the current snapshot is signed (reserved-slot rule)
	held    bool // waiting, but its last offer found the suite busy or no slot: not offered until unhold
	// pendingChange: queued with a change pending and no failure record
	// (Checker.pendingChange), typically a client check that found no slot;
	// its check bypasses the cooldown, so the tick parks it (spec C4).
	pendingChange bool
	// waitSeq orders the waiting list: when the suite first became
	// waiting (deferred minor B-46: re-parks keep that order).
	waitSeq uint64
	// gen is the admission generation when the item was dispatched
	// (deferred minor B-45).
	gen uint64
}

type hostQueue struct {
	classes  [numSchedClasses][]schedItem
	waiting  []schedItem // suites waiting for an admission slot, oldest first
	inFlight bool
}

func (h *hostQueue) empty() bool {
	if len(h.waiting) > 0 {
		return false
	}
	for _, q := range h.classes {
		if len(q) > 0 {
			return false
		}
	}
	return true
}

// schedQueues holds the scheduler's per-host queues of due suites. The
// Run goroutine owns it exclusively, so it takes no locks.
//
// AIDEV-NOTE: invariants: a suite key is in `queued` iff the suite sits
// in a class queue, or in its host's waiting list, or its check is in
// flight; a suite becomes waiting only through the result of a scheduler
// check and leaves only through the next one (never by a tick), and ticks
// never re-add a waiting suite (dedupe); an offer that ends busy re-parks
// held, and so does one that ends queued unless a slot was released after
// it was dispatched (the admission generation moved on: B-45, finish),
// which re-parks it unheld so it is offered at once; a held waiter is
// offered again only after unhold, which the Run loop calls on every tick
// and every wake, so a held suite is never stranded, and no suite is
// re-offered in a loop (an unheld re-park needs a new release each time); a
// host is in `order` iff it has a queued or waiting suite or a check in
// flight; pop never returns a second suite of a host whose check is in
// flight. Memory is bounded by the suite table (each key at most once).
type schedQueues struct {
	hosts  map[string]*hostQueue
	order  []string // round-robin ring of hosts
	next   int      // index in order of the host whose turn is next
	queued map[string]struct{}
	// pending holds the keys of class-queued suites a later tick found
	// with a pending change (deferred minor B-66): pop sets the item's
	// pendingChange from it.
	pending map[string]struct{}
	waitSeq uint64 // the last waitSeq handed out
}

func newSchedQueues() *schedQueues {
	return &schedQueues{hosts: map[string]*hostQueue{}, queued: map[string]struct{}{}, pending: map[string]struct{}{}}
}

// enqueue appends s to the back of its class on its host, unless the
// suite is already queued or in flight. It reports whether s was added.
// pendingChange marks the item's check to bypass the cooldown (see
// schedItem).
func (q *schedQueues) enqueue(s cache.SuiteFreshness, pendingChange bool) bool {
	key := suiteKey(s.CanonicalScheme, s.CanonicalHost, s.SuitePath)
	if _, dup := q.queued[key]; dup {
		// A suite queued earlier as merely stale that a client check
		// has since left with a pending change keeps its place but
		// bypasses the cooldown when its turn comes (B-66). A waiting or
		// in-flight suite ignores the mark (pop and done drop it).
		if pendingChange {
			q.pending[key] = struct{}{}
		}
		return false
	}
	h, ok := q.hosts[s.CanonicalHost]
	if !ok {
		h = &hostQueue{}
		q.hosts[s.CanonicalHost] = h
		q.order = append(q.order, s.CanonicalHost)
	}
	cls := classOf(s)
	h.classes[cls] = append(h.classes[cls], schedItem{
		ref:           SuiteRef{CanonicalScheme: s.CanonicalScheme, CanonicalHost: s.CanonicalHost, SuitePath: s.SuitePath},
		key:           key,
		pendingChange: pendingChange,
	})
	q.queued[key] = struct{}{}
	return true
}

// park keeps item queued as waiting for an admission slot once its check
// ended queued (or busy while it was already waiting): at the back of its
// host's waiting list when it just became waiting, and back at its own
// place — ordered by waitSeq, when it first became waiting (B-46) — when it
// was already waiting, so an offer that found no slot keeps its place
// among the waiters. The host's check is over. The key stays in queued,
// so no tick re-adds it.
func (q *schedQueues) park(item schedItem, signed bool) {
	h, ok := q.hosts[item.ref.CanonicalHost]
	if !ok { // never: an in-flight item's host is kept; fail safe, not fatal
		q.done(item)
		return
	}
	h.inFlight = false
	if !item.waiting {
		q.waitSeq++
		item.waitSeq = q.waitSeq
	}
	item.waiting, item.signed = true, signed
	// Oldest first: a re-parked waiter goes back to its place, so lost or
	// busy offers in a row never invert the order (B-46).
	i, _ := slices.BinarySearchFunc(h.waiting, item.waitSeq, func(w schedItem, seq uint64) int {
		return cmp.Compare(w.waitSeq, seq)
	})
	h.waiting = slices.Insert(h.waiting, i, item)
}

// refreshSigned sets every waiter's signed flag from signed (keys of
// suites whose current snapshot is signed, read by the tick): a waiter
// parked as a first adoption that a client check has since adopted is
// admitted under the reserved-slot rule as the signed suite it now is
// (overview "E:" carry-forward: the flag was the parking check's).
func (q *schedQueues) refreshSigned(signed map[string]bool) {
	for _, h := range q.hosts {
		for i := range h.waiting {
			h.waiting[i].signed = signed[h.waiting[i].key]
		}
	}
}

// finish files a finished check (spec C4 slot-driven re-offer): a queued
// result parks the suite to wait for a slot — keeping its place, held
// until the next wake or tick, if it was already waiting (a lost offer);
// an offer that found the suite busy (another goroutine holds its mutex)
// parks it held too — re-offering at once would spin while the mutex is
// held; any other result ends the suite's turn.
func (q *schedQueues) finish(d checkDone, gen uint64) {
	switch {
	case isQueuedResult(d.report.result):
		// An offer that again found no slot (someone took it between the
		// peek and tryAcquire, or the peek was wrong) is held like a busy
		// one: re-offered on the next wake or tick, never at once — the
		// pop loop runs right after this done, and a peek that still says
		// yes would otherwise re-offer it in a loop. Unless a slot was
		// released after it was dispatched (gen moved on, B-45): that
		// release's wake may already be spent, so it is offered at once —
		// each such re-offer needs a new release, so it cannot loop.
		d.item.held = d.item.waiting && gen == d.item.gen
		q.park(d.item, d.report.signedCurrent)
	case d.item.waiting && d.report.result == resultBusy:
		d.item.held = true
		q.park(d.item, d.item.signed)
	default:
		q.done(d.item)
	}
}

// unhold makes held waiters offerable again. The Run loop calls it on
// every tick and every wake.
func (q *schedQueues) unhold() {
	for _, h := range q.hosts {
		for i := range h.waiting {
			h.waiting[i].held = false
		}
	}
}

// waiting is the number of suites parked for a slot (gauge).
func (q *schedQueues) waiting() int {
	n := 0
	for _, h := range q.hosts {
		n += len(h.waiting)
	}
	return n
}

// pop returns the next suite to check: hosts take turns round-robin, one
// check per turn, skipping hosts with a check in flight. Within a host it
// first offers free admission capacity to the oldest waiting suite admit
// allows that is not held — looking past an inadmissible head, so a
// signed waiter is never blocked by a first adoption ahead of it — and
// otherwise takes the first suite of the best class. It marks the host in
// flight.
func (q *schedQueues) pop(admit func(host string, signed bool) bool) (schedItem, bool) {
	n := len(q.order)
	for i := 0; i < n; i++ {
		idx := (q.next + i) % n
		host := q.order[idx]
		h := q.hosts[host]
		if h.inFlight {
			continue
		}
		for j, w := range h.waiting {
			if !w.held && admit(host, w.signed) {
				h.waiting = slices.Delete(h.waiting, j, j+1)
				h.inFlight = true
				q.next = (idx + 1) % n
				return w, true
			}
		}
		for c := range h.classes {
			if len(h.classes[c]) == 0 {
				continue
			}
			item := h.classes[c][0]
			h.classes[c][0] = schedItem{}
			h.classes[c] = h.classes[c][1:]
			if _, ok := q.pending[item.key]; ok {
				item.pendingChange = true
				delete(q.pending, item.key)
			}
			if len(h.classes[c]) == 0 {
				h.classes[c] = nil // let the backing array go
			}
			h.inFlight = true
			q.next = (idx + 1) % n
			return item, true
		}
	}
	return schedItem{}, false
}

// done records that item's check finished: its host may run its next
// suite and the suite may be queued again.
func (q *schedQueues) done(item schedItem) {
	delete(q.queued, item.key)
	delete(q.pending, item.key)
	if h, ok := q.hosts[item.ref.CanonicalHost]; ok {
		h.inFlight = false
	}
	q.prune()
}

// prune drops hosts with nothing queued and nothing in flight. The turn
// stays with the host whose turn it was, or, when that host is dropped,
// passes to the first surviving host after it (deferred minor B-40).
func (q *schedQueues) prune() {
	if len(q.order) == 0 {
		return
	}
	nextIdx := q.next % len(q.order)
	kept := q.order[:0]
	newNext := -1
	for i, host := range q.order {
		h := q.hosts[host]
		if h.inFlight || !h.empty() {
			if newNext < 0 && i >= nextIdx {
				newNext = len(kept)
			}
			kept = append(kept, host)
			continue
		}
		delete(q.hosts, host)
	}
	q.order = kept
	q.next = max(newNext, 0) // none after it: wrap to the first
}

// len is the number of suites queued or in flight.
func (q *schedQueues) len() int { return len(q.queued) }

// has reports whether key is queued, waiting or in flight.
func (q *schedQueues) has(key string) bool {
	_, ok := q.queued[key]
	return ok
}

// suiteDue reports whether the scheduler should queue s now (spec C4):
// its last success is older than the refresh interval, or its adoption
// retry time has come, or it has a pending change (pendingChange), and it
// is not inside the backoff its unknown-check streak earns (2 min ×
// 2^(k−1), at most 1 h). Clock-step rule: a last_success_at or
// last_check_at in the future is infinitely old. Without an adopter
// nothing is ever attempted, so only staleness counts (Phase 1 parity,
// mirroring retryGateFor); with one, a never-adopted suite is due only
// while it has client interest (spec C4 Dispatch 3).
func (c *Checker) suiteDue(s cache.SuiteFreshness, now time.Time) bool {
	nowU := now.Unix()
	future := nowU + int64(clockStepTolerance/time.Second)
	// Ruling PE-2: an adopted suite in the refusal state anchor_absent (or,
	// ruling PE-12, unsigned_disabled) is due only at its record's retry
	// time (pacedByRefusal), whatever its last_success_at; so is one whose
	// last answered check ended anchor_absent while an attempt's record
	// holds (ruling PE-10, pacedAbsentAnswer). A next_retry_at beyond the
	// legitimate horizon (a backward clock step) is not paced here: once the
	// suite is due (its last_success_at is old), the check clamps it
	// (retryGateFor).
	if c.adopter != nil && s.CurrentSnapshotID != nil && s.AdoptionFailCount > 0 && s.AdoptionNextRetryAt != nil &&
		*s.AdoptionNextRetryAt > nowU && *s.AdoptionNextRetryAt <= nowU+int64(retryHorizon/time.Second) {
		if s.AdoptionFailReason != nil && recordPacesAdopted(*s.AdoptionFailReason) {
			return false
		}
		if _, ok := c.absentAnswered.Load(suiteKey(s.CanonicalScheme, s.CanonicalHost, s.SuitePath)); ok {
			return false
		}
	}
	stale := s.LastSuccessAt == nil || *s.LastSuccessAt <= now.Add(-c.refresh).Unix() ||
		*s.LastSuccessAt > future // infinitely old; checkLocked reports it once (B-38)
	retryDue := c.adopter != nil && s.AdoptionFailCount > 0 && s.AdoptionNextRetryAt != nil && *s.AdoptionNextRetryAt <= nowU
	if !stale && !retryDue && !c.pendingChange(s) {
		return false
	}
	// Spec C4 Dispatch 3: with an adopter wired, a never-adopted suite
	// without client interest in the last 7 days is dormant and never
	// queued (its check would make no request anyway). A stamp up to 7 days
	// in the future counts as interested (interested: a symmetric window);
	// checkFirst reports the clock step.
	//
	// AIDEV-NOTE: deferred minor E-25, accepted. An interested never-adopted
	// suite without a client-created anchor or flat index row is still
	// queued at every tick (suiteDue reads no url_path row): its check
	// reads the suite row and its anchor rows (and a flat suite's metadata
	// rows) and ends dormant at DEBUG, with no request. Bounded by the tick
	// interval and the 7-day interest window; testing for the rows here
	// would put the same reads in the tick, for every such suite.
	if c.adopter != nil && s.CurrentSnapshotID == nil {
		if ok, _ := interested(s.LastRequestedAt, now); !ok {
			return false
		}
	}
	if s.CheckFailCount > 0 && s.LastCheckAt != nil && *s.LastCheckAt <= future {
		if nowU < *s.LastCheckAt+int64(backoffDelay(s.CheckFailCount)/time.Second) {
			return false
		}
	}
	return true
}

// pendingChange reports whether s has an upstream change that no attempt
// has dealt with yet: inrelease_change_seen_at is set and there is no
// failure record (a record has its own retry time). With an adopter wired
// this is the state a check that found no admission slot leaves behind
// (spec C4: "A client-triggered check that is refused leaves the suite
// due; the next tick parks it"); a successful adoption clears the stamp
// in its flip transaction, and a failed one records a failure.
//
// AIDEV-NOTE: the rule is persisted state, not a hand-off from Check to
// the Run loop, so it survives restarts (an adoption abandoned at
// shutdown is not recorded, so its change is pending again at the first
// tick). Its costs: a suite whose adoption is running is queued every
// tick and its check ends busy (a TryLock, no GET); a detached suite
// whose Release.gpg fetch keeps failing is re-fetched every tick; and a
// failed adoption whose record could not be written is re-attempted every
// tick — all bounded by the tick interval.
func (c *Checker) pendingChange(s cache.SuiteFreshness) bool {
	return c.adopter != nil && s.InReleaseChangeSeenAt != nil && s.AdoptionFailCount == 0
}

// Run starts the SPEC §7.4 periodic refresh loop (T2): a fair scheduler
// (spec C4) with per-host queues of due suites, refreshed every tick and
// served by checkWorkers workers, one check per host at a time, hosts
// taking turns. It returns when ctx is cancelled, after its workers.
// Before anything else it reschedules failing adoptions (spec C7 restart
// rule). Refresh = 0 disables periodic refresh.
//
// Latency bound (spec C4, simulation-tested): a due suite S is checked
// within one tick interval + (n + 1) × ⌈H/4⌉ × checkDeadline, where H is
// the number of hosts with queued suites and n the suites ahead of S in
// its host's queue (healthy ones only when S is healthy).
//
// A scheduler check that finds no adoption slot leaves its suite waiting
// in its host's queue; whenever a slot is released the loop re-checks the
// oldest waiting suite the freed capacity admits, cooldown bypassed (spec
// C4 slot-driven re-offer). A client check that finds no slot leaves the
// change pending, so the next tick queues the suite (suiteDue) and its
// check, cooldown bypassed, parks it: such a suite starts waiting within
// one tick. Admission is therefore work-conserving: N suites
// queued behind k usable slots are admitted within one tick + ⌈N/k⌉ ×
// (A + D), A = adoption.max_duration + the spec C8 flip barrier (≤ 140 s:
// flipgate.PhaseAMax + TokenWaitMax + HoldMax; an adoption keeps its
// global slot through it) + commitTimeout. An offered re-check
// uses its host's turn like any check, so n in the latency bound also
// counts the host's waiting suites offered before S (at most one per
// adoption on that host).
//
// Panic safety: the queue operations are pure in-memory code with no
// interface calls; the panic-prone work (the restart reschedule,
// ListSuitesWithAdoption, gauges, checks) runs under safeReschedule, safeTick and
// checkRecovered, and checkWorker sends a done for every item even if
// checkRecovered's own recover handler panics.
func (c *Checker) Run(ctx context.Context) {
	c.safeReschedule(ctx)
	if c.refresh <= 0 {
		return
	}
	interval := c.refresh / 4
	if interval < minFastTickInterval {
		interval = minFastTickInterval
	}
	work := make(chan schedItem)
	done := make(chan checkDone, checkWorkers)
	var wg sync.WaitGroup
	for i := 0; i < checkWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.checkWorker(ctx, work, done)
		}()
	}
	defer wg.Wait()
	t := time.NewTicker(interval)
	defer t.Stop()
	q := newSchedQueues()
	idle := checkWorkers
	// offers counts waiting items dispatched and not yet done: slot offers
	// in flight. An offer is made only while the pool has room beyond the
	// offers already made, so no offer is dispatched that cannot succeed
	// given those in flight.
	offers := 0
	admit := func(host string, signed bool) bool { return c.admission.freeFor(host, signed) > offers }
	for {
		// AIDEV-NOTE: the pop loop runs after every event, done included.
		// A slot's wake can be received before the done of the check it
		// refused (both channels are buffered and select picks at random);
		// the suite is still offered, right after finish parks it. A
		// suite whose refused check was itself an offer is parked held
		// and waits for the next wake or tick (at most one tick) — unless
		// a slot was released after that offer was dispatched (the
		// admission generation moved on, B-45): that release's wake may
		// already be spent, so finish parks it unheld and this loop offers
		// it at once.
		for idle > 0 {
			item, ok := q.pop(admit)
			if !ok {
				break
			}
			select {
			case work <- withGen(item, c.admission.generation()):
				idle--
				if item.waiting {
					offers++
				}
			case <-ctx.Done():
				return
			}
		}
		freshnessWaitingForSlot.Set(float64(q.waiting()))
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			q.unhold()
			c.safeTick(ctx, q)
		case <-c.admission.freed:
			// Capacity was released: the pop loop above offers it to the
			// waiting suites (spec C4 slot-driven re-offer).
			q.unhold()
		case d := <-done:
			idle++
			if d.item.waiting {
				offers--
				if c.offerHook != nil {
					c.offerHook(d.item.ref, d.report)
				}
			}
			q.finish(d, c.admission.generation())
		}
	}
}

// safeReschedule keeps a panic in the restart reschedule (spec C7) from
// stopping the scheduler before it starts.
func (c *Checker) safeReschedule(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			c.reportPanic("freshness_check_panicked", "reschedule", SuiteRef{}, r)
		}
	}()
	c.rescheduleFailingAdoptions(ctx)
}

// withGen stamps item with the admission generation at dispatch (B-45).
func withGen(item schedItem, gen uint64) schedItem {
	item.gen = gen
	return item
}

// checkDone is a worker's report of one scheduled check.
type checkDone struct {
	item   schedItem
	report checkReport
}

// checkWorker runs queued checks until ctx ends. done is buffered for
// every worker, so a send never blocks.
//
// AIDEV-NOTE: every item received gets its done, even if checkRecovered's
// own recover handler panics (reportPanic formats and logs): a dispatched
// item without its done would leave its host marked in flight and one
// worker slot lost for good. The inner recover cannot log (logging is
// what may have panicked), so the item just reports failed and
// acu_freshness_unlogged_panics_total counts it (deferred minor B-44).
func (c *Checker) checkWorker(ctx context.Context, work <-chan schedItem, done chan<- checkDone) {
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-work:
			rep := checkReport{result: "failed"}
			func() {
				defer func() {
					if recover() != nil {
						freshnessUnloggedPanics.Inc()
					}
				}()
				rep = c.checkRecovered(ctx, item)
			}()
			done <- checkDone{item: item, report: rep}
		}
	}
}

// safeTick keeps a panic in a tick (ListSuitesWithAdoption, gauges) from ending the
// scheduler; the next tick tries again.
func (c *Checker) safeTick(ctx context.Context, q *schedQueues) {
	defer func() {
		if r := recover(); r != nil {
			c.reportPanic("freshness_check_panicked", "scheduler", SuiteRef{}, r)
		}
	}()
	c.tick(ctx, q)
}

// tick refreshes the queues with every due suite, the waiting suites'
// signed flags (the reserved-slot rule), and the gauges.
func (c *Checker) tick(ctx context.Context, q *schedQueues) {
	rows, err := c.cache.ListSuitesWithAdoption(ctx)
	if err != nil {
		c.logger.Warn("freshness: list suites failed", "err", err)
		return
	}
	suites := make([]cache.SuiteFreshness, len(rows))
	signed := make(map[string]bool)
	for i, r := range rows {
		suites[i] = r.SuiteFreshness
		if signedForm(cache.AnchorForm(r.CurrentAnchorForm)) {
			signed[suiteKey(r.CanonicalScheme, r.CanonicalHost, r.SuitePath)] = true
		}
	}
	q.refreshSigned(signed)
	nowT := c.now()
	// SPEC §7.4 observability: an adopted suite stale past
	// staleSuiteFactor*refresh is the freeze signature.
	staleBefore := nowT.Add(-staleSuiteFactor * c.refresh).Unix()
	adoptedStale, unadoptedStale, absentStale := staleSuiteCounts(suites, staleBefore, nowT.Unix())
	freshnessStaleSuites.Set(float64(adoptedStale), "true")
	freshnessStaleSuites.Set(float64(unadoptedStale), "false")
	// Design review M-13: a suite paced by its record (anchor_absent,
	// unsigned_disabled) lets its last_success_at age between retries on
	// purpose and is visible on its own — the status page, adoption_refused
	// and, after a day, adoption_refused_persistently — so the WARN leaves
	// it out; the gauge counts it. Any other refusal stale here has failing
	// checks, and the WARN counts it (final fix wave FW-3 (b)). Without an adopter records are inert and count as stale.
	if c.adopter == nil {
		absentStale = 0
	}
	// paced_stale is the count left out (deferred minor E-30): with
	// adopted_stale it adds up to the gauge's adopted="true" value.
	if warnStale := adoptedStale - absentStale; warnStale > 0 {
		c.logger.Warn("freshness: adopted suites stale beyond threshold",
			"adopted_stale", warnStale,
			"paced_stale", absentStale,
			"stale_factor", staleSuiteFactor,
			"refresh", c.refresh.String(),
		)
	}
	failing := 0
	if c.adopter != nil {
		failing = failingAdoptionCount(suites)
	}
	adoptionFailingSuites.Set(float64(failing))
	var due []dueSuite
	for _, s := range suites {
		if c.suiteDue(s, nowT) {
			due = append(due, dueSuite{key: suiteKey(s.CanonicalScheme, s.CanonicalHost, s.SuitePath), s: s})
		}
	}
	// Spec C4: FIFO by queue time. Suites that become due on the same
	// tick are queued in key order, so a suite's position is fixed. Keys
	// are built once per suite, not per comparison (deferred minor B-41).
	sort.Slice(due, func(i, j int) bool { return due[i].key < due[j].key })
	if c.maxSuites > 0 {
		// Spec C4: at most freshness.max_suites suites queued, waiting or
		// in flight; the rest are queued by a later tick.
		due = boundDue(due, q, c.maxSuites, nowT.Unix())
	}
	for _, d := range due {
		q.enqueue(d.s, c.pendingChange(d.s))
	}
}

// dueSuite is a suite a tick found due, with its queue key built once
// (deferred minor B-41).
type dueSuite struct {
	key string
	s   cache.SuiteFreshness
}

// boundDue applies spec C4's queue bound — at most maxSuites suites
// queued, waiting or in flight — to a tick's due list (key order): it
// returns the due suites not queued yet that fit and, when not all of them
// fit, the least recently checked of them (never checked first; a
// last_check_at in the future counts as never, the clock-step rule), still
// in key order.
//
// AIDEV-NOTE: design review M12. Admitting by key alone would let suites
// late in key order wait behind early-key ones that keep becoming due (a
// failing suite re-enters after every backoff); the bound binds only above
// the cap (a lowered max_suites, an upgraded table). The sort runs only
// when the list is cut.
func boundDue(due []dueSuite, q *schedQueues, maxSuites int, now int64) []dueSuite {
	fresh := make([]dueSuite, 0, len(due))
	for _, d := range due {
		if !q.has(d.key) {
			fresh = append(fresh, d)
		}
	}
	room := maxSuites - q.len()
	if len(fresh) <= room {
		return fresh
	}
	if room <= 0 {
		return nil
	}
	lastCheck := func(d dueSuite) int64 {
		if d.s.LastCheckAt == nil || *d.s.LastCheckAt > now+int64(clockStepTolerance/time.Second) {
			return math.MinInt64
		}
		return *d.s.LastCheckAt
	}
	byCheck := slices.Clone(fresh)
	slices.SortStableFunc(byCheck, func(a, b dueSuite) int { return cmp.Compare(lastCheck(a), lastCheck(b)) })
	admit := make(map[string]bool, room)
	for _, d := range byCheck[:room] {
		admit[d.key] = true
	}
	out := make([]dueSuite, 0, room)
	for _, d := range fresh {
		if admit[d.key] {
			out = append(out, d)
		}
	}
	return out
}

// checkRecovered keeps one suite's panic from ending a worker; a panic
// reports as a failed check. check already converts a checkLocked panic
// into a failed check; this catches anything outside it. A waiting item's
// re-check bypasses the cooldown (spec C4 slot-driven re-offer), and so
// does the check of a suite queued with a pending change: client checks
// may keep its last_check_at inside the cooldown, and the tick must still
// park it (spec C4).
func (c *Checker) checkRecovered(ctx context.Context, item schedItem) (rep checkReport) {
	defer func() {
		if r := recover(); r != nil {
			c.reportPanic("freshness_check_panicked", "scheduler", item.ref, r)
			rep = checkReport{result: "failed"}
		}
	}()
	return c.check(ctx, item.ref, checkOpts{bypassCooldown: item.waiting || item.pendingChange})
}
