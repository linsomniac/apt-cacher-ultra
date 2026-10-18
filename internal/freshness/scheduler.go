package freshness

import (
	"context"
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
// that serves stale metadata) and unadopted. A nil last_success_at is
// treated as "never succeeded" rather than "went stale" and is not
// counted, so brand-new suites don't trip the gauge. A last_success_at
// more than clockStepTolerance past now (backward clock step) counts as
// stale (spec C4). SPEC §7.4.
func staleSuiteCounts(suites []cache.SuiteFreshness, staleBefore, now int64) (adopted, unadopted int) {
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
		if s.CurrentSnapshotID != nil {
			adopted++
		} else {
			unadopted++
		}
	}
	return adopted, unadopted
}

// failingAdoptionCount counts suites whose adoption failure record is a
// failure rather than a refusal (spec C7 gauge).
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
	ref SuiteRef
	key string
}

type hostQueue struct {
	classes  [numSchedClasses][]schedItem
	inFlight bool
}

func (h *hostQueue) empty() bool {
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
// in a class queue or its check is in flight; a host is in `order` iff
// it has a queued suite or a check in flight; pop never returns a second
// suite of a host whose check is in flight. Memory is bounded by the
// suite table (each key at most once).
type schedQueues struct {
	hosts  map[string]*hostQueue
	order  []string // round-robin ring of hosts
	next   int      // index in order of the host whose turn is next
	queued map[string]struct{}
}

func newSchedQueues() *schedQueues {
	return &schedQueues{hosts: map[string]*hostQueue{}, queued: map[string]struct{}{}}
}

// enqueue appends s to the back of its class on its host, unless the
// suite is already queued or in flight. It reports whether s was added.
func (q *schedQueues) enqueue(s cache.SuiteFreshness) bool {
	key := suiteKey(s.CanonicalScheme, s.CanonicalHost, s.SuitePath)
	if _, dup := q.queued[key]; dup {
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
		ref: SuiteRef{CanonicalScheme: s.CanonicalScheme, CanonicalHost: s.CanonicalHost, SuitePath: s.SuitePath},
		key: key,
	})
	q.queued[key] = struct{}{}
	return true
}

// pop returns the next suite to check: hosts take turns round-robin, one
// check per turn, skipping hosts with a check in flight; within a host,
// the first suite of the best class. It marks the host in flight.
func (q *schedQueues) pop() (schedItem, bool) {
	n := len(q.order)
	for i := 0; i < n; i++ {
		idx := (q.next + i) % n
		h := q.hosts[q.order[idx]]
		if h.inFlight {
			continue
		}
		for c := range h.classes {
			if len(h.classes[c]) == 0 {
				continue
			}
			item := h.classes[c][0]
			h.classes[c][0] = schedItem{}
			h.classes[c] = h.classes[c][1:]
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
	if h, ok := q.hosts[item.ref.CanonicalHost]; ok {
		h.inFlight = false
	}
	q.prune()
}

// prune drops hosts with nothing queued and nothing in flight, keeping
// the round-robin position on the same host where it survives.
func (q *schedQueues) prune() {
	if len(q.order) == 0 {
		return
	}
	nextHost := q.order[q.next%len(q.order)]
	kept := q.order[:0]
	for _, host := range q.order {
		h := q.hosts[host]
		if h.inFlight || !h.empty() {
			kept = append(kept, host)
			continue
		}
		delete(q.hosts, host)
	}
	q.order = kept
	q.next = 0
	for i, host := range q.order {
		if host == nextHost {
			q.next = i
			break
		}
	}
}

// len is the number of suites queued or in flight.
func (q *schedQueues) len() int { return len(q.queued) }

// suiteDue reports whether the scheduler should queue s now (spec C4):
// its last success is older than the refresh interval or its adoption
// retry time has come, and it is not inside the backoff its unknown-check
// streak earns (2 min × 2^(k−1), at most 1 h). Clock-step rule: a
// last_success_at or last_check_at in the future is infinitely old.
func (c *Checker) suiteDue(s cache.SuiteFreshness, now time.Time) bool {
	nowU := now.Unix()
	future := nowU + int64(clockStepTolerance/time.Second)
	stale := s.LastSuccessAt == nil || *s.LastSuccessAt <= now.Add(-c.refresh).Unix()
	if s.LastSuccessAt != nil && *s.LastSuccessAt > future {
		c.clockStep("last_success_at", s.CanonicalScheme, s.CanonicalHost, s.SuitePath, *s.LastSuccessAt, now)
		stale = true
	}
	retryDue := s.AdoptionFailCount > 0 && s.AdoptionNextRetryAt != nil && *s.AdoptionNextRetryAt <= nowU
	if !stale && !retryDue {
		return false
	}
	if s.CheckFailCount > 0 && s.LastCheckAt != nil && *s.LastCheckAt <= future {
		if nowU < *s.LastCheckAt+int64(backoffDelay(s.CheckFailCount)/time.Second) {
			return false
		}
	}
	return true
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
// Panic safety: the queue operations are pure in-memory code with no
// interface calls; the panic-prone work (ListSuites, gauges, checks) runs
// under safeTick and checkRecovered.
func (c *Checker) Run(ctx context.Context) {
	c.rescheduleFailingAdoptions(ctx)
	if c.refresh <= 0 {
		return
	}
	interval := c.refresh / 4
	if interval < minFastTickInterval {
		interval = minFastTickInterval
	}
	work := make(chan schedItem)
	done := make(chan schedItem, checkWorkers)
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
	for {
		for idle > 0 {
			item, ok := q.pop()
			if !ok {
				break
			}
			select {
			case work <- item:
				idle--
			case <-ctx.Done():
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.safeTick(ctx, q)
		case item := <-done:
			q.done(item)
			idle++
		}
	}
}

// checkWorker runs queued checks until ctx ends. The done channel is
// buffered for every worker, so a send never blocks.
func (c *Checker) checkWorker(ctx context.Context, work <-chan schedItem, done chan<- schedItem) {
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-work:
			c.checkRecovered(ctx, item.ref)
			done <- item
		}
	}
}

// safeTick keeps a panic in a tick (ListSuites, gauges) from ending the
// scheduler; the next tick tries again.
func (c *Checker) safeTick(ctx context.Context, q *schedQueues) {
	defer func() {
		if r := recover(); r != nil {
			c.reportPanic("freshness_check_panicked", "scheduler", SuiteRef{}, r)
		}
	}()
	c.tick(ctx, q)
}

// tick refreshes the queues with every due suite and updates the gauges.
func (c *Checker) tick(ctx context.Context, q *schedQueues) {
	suites, err := c.cache.ListSuites(ctx)
	if err != nil {
		c.logger.Warn("freshness: list suites failed", "err", err)
		return
	}
	nowT := c.now()
	// SPEC §7.4 observability: an adopted suite stale past
	// staleSuiteFactor*refresh is the freeze signature.
	staleBefore := nowT.Add(-staleSuiteFactor * c.refresh).Unix()
	adoptedStale, unadoptedStale := staleSuiteCounts(suites, staleBefore, nowT.Unix())
	freshnessStaleSuites.Set(float64(adoptedStale), "true")
	freshnessStaleSuites.Set(float64(unadoptedStale), "false")
	if adoptedStale > 0 {
		c.logger.Warn("freshness: adopted suites stale beyond threshold",
			"adopted_stale", adoptedStale,
			"stale_factor", staleSuiteFactor,
			"refresh", c.refresh.String(),
		)
	}
	adoptionFailingSuites.Set(float64(failingAdoptionCount(suites)))
	var due []cache.SuiteFreshness
	for _, s := range suites {
		if c.suiteDue(s, nowT) {
			due = append(due, s)
		}
	}
	// Spec C4: FIFO by queue time. Suites that become due on the same
	// tick are queued in key order, so a suite's position is fixed.
	sort.Slice(due, func(i, j int) bool {
		return suiteKey(due[i].CanonicalScheme, due[i].CanonicalHost, due[i].SuitePath) <
			suiteKey(due[j].CanonicalScheme, due[j].CanonicalHost, due[j].SuitePath)
	})
	for _, s := range due {
		q.enqueue(s)
	}
}

// checkRecovered keeps one suite's panic from ending a worker. Check
// already converts a checkLocked panic into a failed check; this catches
// anything outside it.
func (c *Checker) checkRecovered(ctx context.Context, ref SuiteRef) {
	defer func() {
		if r := recover(); r != nil {
			c.reportPanic("freshness_check_panicked", "scheduler", ref, r)
		}
	}()
	c.Check(ctx, ref.CanonicalScheme, ref.CanonicalHost, ref.SuitePath)
}
