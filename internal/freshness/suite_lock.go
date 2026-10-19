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

// lockState is lockSuite's outcome.
type lockState int

const (
	lockOK    lockState = iota // held, and the lock map still holds this mutex
	lockBusy                   // another goroutine holds the suite's mutex
	lockStale                  // the mutex was free but the map no longer holds it (released again)
)

// lockSuite is the I-4 lock protocol (spec C3), the one place check,
// Reconcile and HoldSuite take a suite's mutex: load or store the suite's
// lock-map entry, TryLock it (never wait), then re-validate that the map
// still holds that same mutex. Only lockOK returns a held mutex; on
// lockBusy and lockStale the caller holds nothing. Callers that may store
// an entry must have read the suite's row first (the map stays bounded by
// the suite table).
//
// AIDEV-NOTE: the re-validation is what makes a deleter's drop safe. A
// deleter (cap eviction through HoldSuite's release(true), a check or
// Reconcile that finds its row gone, phase F's reaper) drops the entry
// while it still holds the mutex (unlockDropped); a caller that loaded the
// old mutex before the drop either fails TryLock or, once the deleter has
// unlocked, finds the map no longer holds it (lockStale), so it never runs
// beside a holder of a re-created suite's new entry. lockLoadedHook runs
// in the window this closes (tests).
func (c *Checker) lockSuite(key string) (*sync.Mutex, lockState) {
	entry, _ := c.locks.LoadOrStore(key, &sync.Mutex{})
	mu := entry.(*sync.Mutex)
	if c.lockLoadedHook != nil {
		c.lockLoadedHook(key)
	}
	if !mu.TryLock() {
		return nil, lockBusy
	}
	if cur, ok := c.locks.Load(key); !ok || cur != entry {
		mu.Unlock()
		return nil, lockStale
	}
	return mu, lockOK
}

// unlockDropped ends the hold of a suite whose row is gone: it drops the
// suite's lock-map entry if the map still holds mu, then unlocks mu.
//
// AIDEV-NOTE: the order is load-bearing — drop, then unlock. Unlocking
// first would let a caller that loaded mu earlier take it and pass
// lockSuite's re-validation; the drop would then remove an entry that live
// holder relies on, and a re-created suite could get a second mutex (two
// checks of one suite).
func (c *Checker) unlockDropped(key string, mu *sync.Mutex) {
	c.locks.CompareAndDelete(key, mu)
	mu.Unlock()
}

// HoldSuite takes the suite's mutex without waiting (cache.SuiteGuard): ok
// is false while a check, adoption, repair or reconcile holds it, and when
// lockSuite's re-validation finds the loaded mutex stale (another holder
// deleted the suite in the window). release(true) — the suite's row was
// deleted while held — purges its flip-gate entry, then drops its lock-map
// entry and unlocks (unlockDropped); release(false) only unlocks. Spec C3
// cap eviction uses it; phase F's reaper purges the same way.
//
// AIDEV-NOTE: both entries are dropped while the mutex is still held, so a
// check that loaded this mutex earlier fails lockSuite's post-TryLock
// re-validation (the map no longer holds it) instead of running beside a
// check of a re-created suite.
func (c *Checker) HoldSuite(key cache.SuiteKey) (func(deleted bool), bool) {
	k := suiteKey(key.CanonicalScheme, key.CanonicalHost, key.SuitePath)
	mu, st := c.lockSuite(k)
	if st != lockOK {
		return nil, false
	}
	var once sync.Once
	return func(deleted bool) {
		once.Do(func() {
			if !deleted {
				mu.Unlock()
				return
			}
			c.adopter.FlipGate().Purge(flipgate.Key(key.CanonicalScheme, key.CanonicalHost, key.SuitePath))
			c.unlockDropped(k, mu)
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
// suites a check or Reconcile is unwinding right now, plus the bounded
// residue of a read error: a check or Reconcile whose re-read fails keeps
// its entry (the row may well exist), and so does a cap eviction whose
// outcome read fails (cache afterCreate, one entry per such eviction).
func (c *Checker) LockCount() int {
	n := 0
	c.locks.Range(func(any, any) bool { n++; return true })
	return n
}
