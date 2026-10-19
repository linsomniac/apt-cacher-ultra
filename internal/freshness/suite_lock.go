package freshness

import (
	"sync"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"
)

// The checker is the cache's SuiteGuard — spec C3 cap eviction runs under
// the evicted suite's mutex — and its ClockStepReporter (ruling PD-11).
var (
	_ cache.SuiteGuard        = (*Checker)(nil)
	_ cache.ClockStepReporter = (*Checker)(nil)
)

// HoldSuite takes the suite's mutex without waiting (cache.SuiteGuard): ok
// is false while a check, adoption, repair or reconcile holds it.
// release(true) — the suite's row was deleted while held — drops its
// lock-map entry and its flip-gate entry, then unlocks; release(false)
// only unlocks. Spec C3 cap eviction uses it; phase F's reaper purges the
// same way.
//
// AIDEV-NOTE: the entry is dropped while the mutex is still held, so a
// check that loaded this mutex earlier fails check's post-TryLock
// re-validation (the map no longer holds it) instead of running beside a
// check of a re-created suite.
func (c *Checker) HoldSuite(key cache.SuiteKey) (func(deleted bool), bool) {
	k := suiteKey(key.CanonicalScheme, key.CanonicalHost, key.SuitePath)
	muVal, _ := c.locks.LoadOrStore(k, &sync.Mutex{})
	mu := muVal.(*sync.Mutex)
	if c.lockLoadedHook != nil {
		c.lockLoadedHook(k)
	}
	if !mu.TryLock() {
		return nil, false
	}
	if cur, ok := c.locks.Load(k); !ok || cur != muVal {
		mu.Unlock()
		return nil, false
	}
	var once sync.Once
	return func(deleted bool) {
		once.Do(func() {
			if deleted {
				c.locks.CompareAndDelete(k, muVal)
				c.adopter.FlipGate().Purge(flipgate.Key(key.CanonicalScheme, key.CanonicalHost, key.SuitePath))
			}
			mu.Unlock()
		})
	}, true
}

// ReportClockStep reports a past-event stamp that cap eviction found in
// the future — a backward wall-clock step (cache.ClockStepReporter, ruling
// PD-11) — through the checker's rate-limited clock_step_detected and
// acu_clock_steps_detected_total, like every other gate (spec C4).
func (c *Checker) ReportClockStep(stamp string, key cache.SuiteKey, stored, now int64) {
	c.clockStep(stamp, key.CanonicalScheme, key.CanonicalHost, key.SuitePath, stored, time.Unix(now, 0))
}

// LockCount is the number of suites with a lock-map entry (tests and
// diagnostics). I-4 keeps it at most the number of suite rows, plus the
// suites a check is unwinding right now.
func (c *Checker) LockCount() int {
	n := 0
	c.locks.Range(func(any, any) bool { n++; return true })
	return n
}
