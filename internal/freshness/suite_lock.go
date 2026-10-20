package freshness

import (
	"sync"

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
// Reconcile that finds its row gone, the reaper, POST /forget) drops the
// entry while it still holds the mutex (unlockDropped); a caller that
// loaded the old mutex before the drop either fails TryLock or, once the
// deleter has unlocked, finds the map no longer holds it (lockStale), so
// it never runs beside a holder of a re-created suite's new entry.
// lockLoadedHook runs in the window this closes (tests).
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
// checks of one suite). unlockDroppedHook runs between the two steps
// (tests): TestUnlockDropped_DropsBeforeItUnlocks lets such a caller's
// TryLock run there and fails on two holders if the order flips. The
// reaper and POST /forget drop through this same helper.
func (c *Checker) unlockDropped(key string, mu *sync.Mutex) {
	c.locks.CompareAndDelete(key, mu)
	c.absentAnswered.Delete(key) // ruling PE-10 (b): the suite's row is gone
	c.lateralHolds.Delete(key)   // ruling PE-11
	if c.unlockDroppedHook != nil {
		c.unlockDroppedHook(key)
	}
	mu.Unlock()
}

// HoldSuite takes the suite's mutex without waiting (cache.SuiteGuard): ok
// is false while anything else holds it — a check (or its busy peek), an
// adoption, repair or reconcile, or another HoldSuite (a racing cap
// eviction, the reaper, POST /forget) — and when lockSuite's re-validation
// finds the loaded mutex stale (another holder deleted the suite in the
// window; deferred minor D-25). release(true) — the suite's row was
// deleted while held — purges its flip-gate entry if it is idle (ruling
// R-F2, flipgate.Gate.Purge: a live entry — a flip in progress, an anchor
// serve in flight, or any use within the quiet period — is kept, and
// expires on its own), then drops its lock-map entry and the checker's
// per-suite state and unlocks (unlockDropped); release(false) only
// unlocks. Spec C3 cap eviction, the reaper and POST /forget use it
// (ruling R-F1).
//
// AIDEV-NOTE: the lock-map entry is dropped while the mutex is still held
// (the flip-gate purge runs under it too), so a check that loaded this
// mutex earlier fails lockSuite's post-TryLock re-validation (the map no
// longer holds it) instead of running beside a check of a re-created
// suite. The flip-gate entry is not always gone after release(true): a
// live one is kept (ruling R-F2), since a re-creation of the suite may
// already have registered a serve on it
// (TestHoldSuite_ReleaseKeepsARecreatedSuitesLiveGateEntry); it expires
// once idle for twice the quiet period.
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
