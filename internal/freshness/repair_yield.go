package freshness

// A repair gives way (the 2026-11-14 publication-delay review).
//
// A repair or reconcile heals files of a snapshot that is already serving:
// an optional member an adoption skipped mid-sync, an index that was
// missing. An adoption publishes a new release. The repair used to hold
// its suite's mutex, its host's admission slot and a global one for as
// long as it ran — bounded only by adoption.max_duration — so that:
//   - its own suite could not notice a new release (every check ended
//     busy, with no request and no log line), and
//   - no other suite on its host could adopt one (changed_queued).
//
// Now the repair runs under a context that can be cancelled on request:
//   - an adoption that finds only repairs between it and a slot asks them
//     to give way and takes the slot (Checker.admitPastRepairs);
//   - a due check probes beside maintenance first; unchanged content lets
//     maintenance finish, while a change or inconclusive answer asks it to
//     give way to the ordinary check (Checker.repairGivesWay).
//
// A repair that gave way logs repair_yielded at INFO, counts in
// acu_repair_yielded_total{to}, and records nothing about the members it
// had not finished: being stopped is no evidence about upstream. The next
// confirmed-unchanged check starts it again; members it promoted stay.

import (
	"context"
	"errors"
	"sync"
	"time"
)

// repairYieldWait bounds the wait for a repair that was asked to give way.
// Its fetch ends with its context, so the wait is normally milliseconds;
// the bound is for a repair inside a cache write, which is not cut short.
// When it passes, the asker goes on as it would have without asking: the
// check ends busy, the adoption is queued.
const repairYieldWait = 5 * time.Second

// errRepairYielded is the cause a repair's context is cancelled with when
// it gives way; yielded tests for it.
var errRepairYielded = errors.New("the repair gave way")

// yielded reports whether ctx is a repair's context that was cancelled
// because the repair gave way (not its deadline, not a shutdown).
func yielded(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errRepairYielded)
}

// repairRun is one repair or reconcile goroutine in flight.
type repairRun struct {
	probeMu    sync.Mutex
	snapshotID int64
	started    time.Time // monotonic
	cancel     context.CancelCauseFunc
	// done is closed once the goroutine has released the suite's mutex.
	done chan struct{}

	mu sync.Mutex
	to string // what it gave way to — "adoption" or "check" — "" while it has not
}

// yield asks the repair to give way to to. Only the first call counts.
func (r *repairRun) yield(to string) {
	r.mu.Lock()
	first := r.to == ""
	if first {
		r.to = to
	}
	r.mu.Unlock()
	if first {
		r.cancel(errRepairYielded)
	}
}

// yieldedTo returns what the repair gave way to, "" if it has not.
func (r *repairRun) yieldedTo() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.to
}

// repairGivesWay cancels maintenance after checkDuringRepair determined that
// an ordinary locked check is needed. Unchanged probes and cooldown skips leave
// maintenance running; slow optional/required members can finish across ticks.
//
// AIDEV-NOTE: why a probe decides, and not the repair's age. Until the
// 2026-11-15 follow-up a repair gave way to any check of its suite once it
// had run for the cooldown: by then the check would ask upstream, and a new
// release might be waiting. Under steady client traffic that cost a member
// needing longer than the cooldown its download at every check; with
// periodic_refresh at one minute it would have cost it at every scheduled
// check, traffic or not, and the member would never heal. The check now
// asks upstream beside the repair (checkDuringRepair, maintenance_check.go)
// and comes here only when the anchor changed or the probe could not settle
// it: a pending change or failure record, a possible form upgrade, an
// upstream or database error. The members a repair did finish are promoted
// one by one and stay.
func (c *Checker) repairGivesWay(key string, run *repairRun) bool {
	select {
	case <-run.done:
		return true // it finished naturally; retry the suite mutex immediately
	default:
	}
	v, ok := c.repairs.Load(key)
	if !ok || v != run {
		return false
	}
	run.yield("check")
	t := time.NewTimer(repairYieldWait)
	defer t.Stop()
	select {
	case <-run.done:
		return true
	case <-t.C:
		c.logYieldTimeout(key)
	case <-c.lifetimeCtx.Done():
	}
	return false
}

// admitPastRepairs is admit's second try: when only repairs keep a suite
// on host from an admission slot, it asks them to give way, waits — at
// most repairYieldWait — for the leases they hold, and tries once more.
//
// AIDEV-NOTE: the one place a check waits for a slot, against spec C4's
// "never wait" (no suite mutex held while queued). The rule exists because
// a queue of adoptions can last hours; this wait is for a cancelled
// repair to unwind, is bounded by repairYieldWait, and is what turns
// "queued behind a repair, until the next tick or cooldown" into an
// adoption that starts now.
func (c *Checker) admitPastRepairs(host string, signedCurrent bool) (*lease, bool) {
	blockers := c.admission.yielders(host, signedCurrent)
	if len(blockers) == 0 {
		return nil, false
	}
	for _, yield := range c.admission.yieldFuncs(blockers) {
		yield("adoption")
	}
	t := time.NewTimer(repairYieldWait)
	defer t.Stop()
	for _, b := range blockers {
		select {
		case <-b.released:
		case <-t.C:
			unloggedPanic(func() {
				c.logger.Warn("repair_yield_timeout", "canonical_host", host, "to", "adoption", "wait_seconds", repairYieldWait.Seconds())
			})
			return nil, false
		case <-c.lifetimeCtx.Done():
			return nil, false
		}
	}
	return c.admission.tryAcquire(host, signedCurrent)
}

func (c *Checker) logYieldTimeout(key string) {
	if v, ok := c.work.Load(key); ok {
		w := v.(*backgroundWork)
		unloggedPanic(func() {
			c.logger.Warn("repair_yield_timeout", append(w.attrs(), "to", "check", "wait_seconds", repairYieldWait.Seconds())...)
		})
	}
}
