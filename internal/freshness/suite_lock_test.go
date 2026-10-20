package freshness

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

var lockRef = SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}
var lockKey = cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}

// gatedAdopter is an adopter over a real cache whose flip gate is enabled.
func gatedAdopter(t *testing.T) (*Adopter, *cache.Cache, *flipgate.Gate) {
	t.Helper()
	rc, err := cache.Open(context.Background(), t.TempDir(), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	gate, err := flipgate.New(flipgate.Config{Quiet: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ad, err := NewAdopter(AdoptionConfig{Cache: rc, Fetcher: newFakeFetcher(), Verifier: passThroughVerifier{},
		HostLimiter: hostsem.New(8), FlipGate: gate, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	return ad, rc, gate
}

func lockChecker(t *testing.T, c Cache, f Fetcher, ad *Adopter) *Checker {
	t.Helper()
	ck, err := New(Config{Cache: c, Fetcher: f, HostLimiter: hostsem.New(8), Logger: discardLogger(), Adopter: ad})
	if err != nil {
		t.Fatal(err)
	}
	return ck
}

// R-D1: HoldSuite takes a free suite's mutex without waiting and refuses a
// busy one; release(false) only unlocks, release(true) also drops the
// suite's lock-map and flip-gate entries (once) — the gate entry once its
// last serve is a quiet period old (ruling R-F2), hence the bubble.
func TestHoldSuite_TakesOnlyAFreeSuiteAndPurgesOnDelete(t *testing.T) {
	synctest.Test(t, testHoldSuiteTakesOnlyAFreeSuiteAndPurgesOnDelete)
}

func testHoldSuiteTakesOnlyAFreeSuiteAndPurgesOnDelete(t *testing.T) {
	ad, _, gate := gatedAdopter(t)
	fc := newFakeCache()
	fc.track("http", "h", "/r/dists/x")
	c := lockChecker(t, fc, &simtest.StaticConditional{Status: 304}, ad)
	ctx := context.Background()
	gate.AnchorStart(ctx, flipgate.Key("http", "h", "/r/dists/x")).Done()
	release, ok := c.HoldSuite(lockKey)
	if !ok {
		t.Fatal("a free suite was not held")
	}
	if _, ok := c.HoldSuite(lockKey); ok {
		t.Fatal("a held suite was held twice")
	}
	if rep := c.check(ctx, lockRef, checkOpts{}); rep.result != resultBusy {
		t.Fatalf("check of a held suite = %q, want %q", rep.result, resultBusy)
	}
	release(false)
	if c.LockCount() != 1 || gate.Len() != 1 {
		t.Fatalf("after release(false): %d lock entries, %d gate entries; want 1 and 1", c.LockCount(), gate.Len())
	}
	release, ok = c.HoldSuite(lockKey)
	if !ok {
		t.Fatal("a released suite was not held again")
	}
	time.Sleep(gate.Quiet()) // the serve's quiet period is over: the entry is idle
	release(true)
	release(true) // idempotent
	if c.LockCount() != 0 || gate.Len() != 0 {
		t.Fatalf("after release(true): %d lock entries, %d gate entries; want 0 and 0", c.LockCount(), gate.Len())
	}
}

// Ruling R-F2: a deleter's release(true) runs after its commit, so a
// re-creation of the suite can register an anchor serve first. That
// serve's flip-gate entry is live and survives the purge: the re-created
// suite's next flip still waits out the serve's quiet period. The
// deleter's lock-map entry goes.
func TestHoldSuite_ReleaseKeepsARecreatedSuitesLiveGateEntry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ad, _, gate := gatedAdopter(t)
		c := lockChecker(t, newFakeCache(), &simtest.StaticConditional{Status: 304}, ad)
		release, ok := c.HoldSuite(lockKey) // the deleter holds the suite
		if !ok {
			t.Fatal("a free suite was not held")
		}
		gk := flipgate.Key(lockKey.CanonicalScheme, lockKey.CanonicalHost, lockKey.SuitePath)
		tok := gate.AnchorStart(context.Background(), gk) // the re-created suite's serve
		release(true)                                     // the deleter's purge, after its commit
		if gate.Len() != 1 || c.LockCount() != 0 {
			t.Fatalf("after release(true): %d gate entries, %d lock entries; want the live gate entry kept, the lock entry dropped",
				gate.Len(), c.LockCount())
		}
		tok.Done()
		start := time.Now()
		if _, err := gate.Flip(context.Background(), gk, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		if waited := time.Since(start); waited != gate.Quiet() {
			t.Fatalf("the re-created suite's flip waited %v, want the serve's quiet period (%v)", waited, gate.Quiet())
		}
	})
}

// Spec C3 I-4: a check re-validates after TryLock that the lock map still
// holds its mutex. One that loaded the mutex of a suite evicted (and
// re-created) meanwhile is skipped, so it never runs beside a check that
// takes the new entry.
func TestCheck_RevalidatesItsLockAfterTryLock(t *testing.T) {
	fc := newFakeCache()
	seedFakeSuite(fc, "h", "/r/dists/x", nil)
	up := &changedUpstream{body: []byte("x")}
	c := lockChecker(t, fc, up, nil)
	c.lockLoadedHook = func(string) {
		c.lockLoadedHook = nil
		release, ok := c.HoldSuite(lockKey) // an eviction in the window
		if !ok {
			t.Error("HoldSuite in the window failed")
			return
		}
		release(true)
	}
	ctx := context.Background()
	if rep := c.check(ctx, lockRef, checkOpts{bypassCooldown: true}); rep.result != resultSkipped || up.calls.Load() != 0 {
		t.Fatalf("check on a stale mutex = %q with %d upstream calls; want skipped, 0", rep.result, up.calls.Load())
	}
	if rep := c.check(ctx, lockRef, checkOpts{bypassCooldown: true}); rep.result == resultSkipped || up.calls.Load() != 1 {
		t.Fatalf("next check = %q with %d upstream calls; want it to run", rep.result, up.calls.Load())
	}
}

// Spec C3 I-4: a check that finds its row gone after the pre-read writes
// nothing, calls no upstream, and drops its lock-map entry.
func TestCheck_VanishedRowDropsItsLockEntry(t *testing.T) {
	fc := newFakeCache()
	seedFakeSuite(fc, "h", "/r/dists/x", nil)
	up := &changedUpstream{body: []byte("x")}
	c := lockChecker(t, fc, up, nil)
	c.lockLoadedHook = func(string) {
		c.lockLoadedHook = nil
		fc.mu.Lock()
		delete(fc.suites, keyOf("http", "h", "/r/dists/x"))
		fc.mu.Unlock()
	}
	if rep := c.check(context.Background(), lockRef, checkOpts{bypassCooldown: true}); rep.result != resultSkipped {
		t.Fatalf("result = %q, want %q", rep.result, resultSkipped)
	}
	if up.calls.Load() != 0 || c.LockCount() != 0 {
		t.Fatalf("%d upstream calls, %d lock entries; want 0 and 0", up.calls.Load(), c.LockCount())
	}
	if _, ok := fc.suite("http", "h", "/r/dists/x"); ok {
		t.Fatal("the vanished row was written back")
	}
}

// Spec C3 I-4: Reconcile re-validates under the mutex — the lock map must
// still hold it and the row must still name the snapshot read before.
func TestReconcile_RevalidatesUnderTheMutex(t *testing.T) {
	ad, _, _ := gatedAdopter(t)
	fc := newFakeCache()
	id := int64(42)
	fc.putSuite(cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", CurrentSnapshotID: &id})
	c := lockChecker(t, fc, newTestFetcher(t), ad)
	ctx := context.Background()
	c.lockLoadedHook = func(string) { // re-adopted in the window
		c.lockLoadedHook = nil
		other := int64(43)
		fc.putSuite(cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", CurrentSnapshotID: &other})
	}
	if c.Reconcile(ctx, "http", "h", "/r/dists/x") {
		t.Fatal("reconciled a snapshot that is no longer current")
	}
	c.lockLoadedHook = func(string) { // evicted in the window
		c.lockLoadedHook = nil
		if release, ok := c.HoldSuite(lockKey); ok {
			release(true)
		}
	}
	if c.Reconcile(ctx, "http", "h", "/r/dists/x") {
		t.Fatal("reconciled on a stale mutex")
	}
	if !c.Reconcile(ctx, "http", "h", "/r/dists/x") {
		t.Fatal("an undisturbed reconcile did not start")
	}
	c.WaitForAdoptions()
}

// R-D1 end to end: cap eviction with the checker as the guard skips a busy
// suite and purges an evicted suite's lock-map and flip-gate entries (the
// gate entry once its last serve is a quiet period old, ruling R-F2; hence
// the bubble).
func TestCreateSuite_EvictionUnderTheChecker(t *testing.T) {
	synctest.Test(t, testCreateSuiteEvictionUnderTheChecker)
}

func testCreateSuiteEvictionUnderTheChecker(t *testing.T) {
	ad, rc, gate := gatedAdopter(t)
	c := lockChecker(t, rc, &simtest.StaticConditional{Status: 304}, ad)
	ctx := context.Background()
	k := func(s string) cache.SuiteKey {
		return cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: s}
	}
	old := time.Now().Unix() - 3*86400
	for _, s := range []string{"/a", "/b"} {
		if _, err := rc.CreateSuite(ctx, cache.SuiteSeed{SuiteKey: k(s), RequestedAt: old}, cache.SuiteCaps{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	release, ok := c.HoldSuite(k("/a")) // /a is busy (a check holds it)
	if !ok {
		t.Fatal("HoldSuite(/a)")
	}
	gate.AnchorStart(ctx, flipgate.Key("http", "h", "/b")).Done()
	time.Sleep(gate.Quiet()) // /b's serve is a quiet period old: its entry is idle
	res, err := rc.CreateSuite(ctx, cache.SuiteSeed{SuiteKey: k("/c"), RequestedAt: time.Now().Unix()}, cache.SuiteCaps{Global: 2}, c)
	if err != nil || res.Outcome != cache.SuiteCreated || res.Evicted == nil || *res.Evicted != k("/b") {
		t.Fatalf("CreateSuite = %+v, %v; want created, evicting /b (/a is busy)", res, err)
	}
	release(false)
	if c.LockCount() != 1 || gate.Len() != 0 {
		t.Fatalf("%d lock entries (want /a's only), %d gate entries (want 0)", c.LockCount(), gate.Len())
	}
}

// Spec C3 I-4: Reconcile re-validates that the row still exists — a row
// deleted in the window (lock map intact) starts nothing and, as in check,
// drops the lock-map entry Reconcile stored for it, so the map stays
// bounded by the suite table.
func TestReconcile_RevalidatesAVanishedRow(t *testing.T) {
	ad, _, _ := gatedAdopter(t)
	fc := newFakeCache()
	id := int64(42)
	fc.putSuite(cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", CurrentSnapshotID: &id})
	c := lockChecker(t, fc, newTestFetcher(t), ad)
	c.lockLoadedHook = func(string) {
		c.lockLoadedHook = nil
		fc.mu.Lock()
		delete(fc.suites, keyOf("http", "h", "/r/dists/x"))
		fc.mu.Unlock()
	}
	if c.Reconcile(context.Background(), "http", "h", "/r/dists/x") {
		t.Fatal("reconciled a suite whose row vanished in the window")
	}
	c.WaitForAdoptions()
	if n := c.LockCount(); n != 0 {
		t.Fatalf("lock entries = %d, want 0 (a vanished row's entry is dropped, as check drops it)", n)
	}
}

// R-D1 / I-4: HoldSuite re-validates after TryLock like check does — a
// hold that loaded the mutex of a suite another hold evicted in the window
// (its entry dropped) is refused, so two holders never share a suite.
func TestHoldSuite_RevalidatesItsLockAfterTryLock(t *testing.T) {
	fc := newFakeCache()
	fc.track("http", "h", "/r/dists/x")
	c := lockChecker(t, fc, newTestFetcher(t), nil)
	c.lockLoadedHook = func(string) {
		c.lockLoadedHook = nil
		release, ok := c.HoldSuite(lockKey) // an eviction in the window
		if !ok {
			t.Error("HoldSuite in the window failed")
			return
		}
		release(true)
	}
	if release, ok := c.HoldSuite(lockKey); ok {
		release(false)
		t.Fatal("a hold on a stale mutex was granted")
	}
	if c.LockCount() != 0 {
		t.Fatalf("lock entries = %d, want 0 (the refused hold leaves none)", c.LockCount())
	}
	release, ok := c.HoldSuite(lockKey)
	if !ok {
		t.Fatal("an undisturbed hold was refused")
	}
	release(false)
}

// FW-6 (final review, concurrency M3; tests Minor 10): check peeks the
// lock map before it reads the row, so a T1 on a suite whose mutex is held
// answers busy with no database read (as before phase D), and the row gate
// before LoadOrStore is an existence read (HasSuiteRow), not a full row
// read. An untracked suite still costs that one read, and gets no entry.
func TestCheck_ABusySuiteCostsNoRead(t *testing.T) {
	fc := newFakeCache()
	fc.track("http", "h", "/r/dists/x")
	var reads atomic.Int64
	fc.getErrFn = func() error { reads.Add(1); return nil }
	c := lockChecker(t, fc, newTestFetcher(t), nil)
	ctx := context.Background()
	release, ok := c.HoldSuite(lockKey)
	if !ok {
		t.Fatal("premise: the suite is held")
	}
	for range 20 {
		if rep := c.check(ctx, lockRef, checkOpts{}); rep.result != resultBusy {
			t.Fatalf("check of a held suite = %q, want %q", rep.result, resultBusy)
		}
	}
	if n := reads.Load(); n != 0 {
		t.Fatalf("20 busy checks read the suite row %d times, want 0", n)
	}
	release(false)
	none := SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/none"}
	if rep := c.check(ctx, none, checkOpts{}); rep.result != resultSkipped {
		t.Fatalf("check of an untracked suite = %q, want %q", rep.result, resultSkipped)
	}
	if n, e := reads.Load(), fc.existsCalls.Load(); n != 1 || e != 1 {
		t.Fatalf("an untracked suite's check: %d reads, %d of them HasSuiteRow; want one existence read", n, e)
	}
	if n := c.LockCount(); n != 1 {
		t.Fatalf("LockCount = %d, want 1 (the untracked suite gets no entry)", n)
	}
}

// I-4 (Task 7 minor 3; final review, concurrency m2): unlockDropped drops
// the suite's lock-map entry before it unlocks the mutex. A waiter that
// loaded the mutex before the drop and runs its TryLock between the two
// steps must fail it; then a newcomer, after the suite's re-creation,
// stores a fresh entry and is the only holder. Unlock-first lets the
// waiter take the old mutex and pass the re-validation, and the late drop
// then lets the newcomer hold a second mutex: two checks of one suite.
func TestUnlockDropped_DropsBeforeItUnlocks(t *testing.T) {
	fc := newFakeCache()
	fc.track("http", "h", "/r/dists/x")
	c := lockChecker(t, fc, newTestFetcher(t), nil)
	k := suiteKey("http", "h", "/r/dists/x")
	release, ok := c.HoldSuite(lockKey) // the deleter
	if !ok {
		t.Fatal("premise: the deleter holds the suite")
	}
	old, _ := c.locks.Load(k)
	loaded, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	c.lockLoadedHook = func(string) { // the waiter, between its load and its TryLock
		once.Do(func() {
			close(loaded)
			<-proceed
		})
	}
	type lockResult struct {
		mu *sync.Mutex
		st lockState
	}
	waiter := make(chan lockResult, 1)
	go func() {
		mu, st := c.lockSuite(k)
		waiter <- lockResult{mu, st}
	}()
	select {
	case <-loaded:
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter never loaded the mutex")
	}
	var w lockResult
	// A zero lockResult reads as lockOK; only a received one counts.
	got := false
	c.unlockDroppedHook = func(string) { // the deleter, between its two steps
		close(proceed)
		select {
		case w = <-waiter:
			got = true
		case <-time.After(5 * time.Second):
			t.Error("the waiter's TryLock did not return")
		}
	}
	release(true) // the deleter's row is gone: drop, then unlock
	c.unlockDroppedHook = nil
	if !got {
		t.FailNow()
	}
	if w.st == lockOK {
		defer w.mu.Unlock()
	}
	nmu, nst := c.lockSuite(k) // a newcomer, after the suite's re-creation
	if nst == lockOK {
		defer nmu.Unlock()
	}
	if w.st == lockOK && nst == lockOK {
		t.Fatal("two holders of one suite: the waiter kept the old mutex, the newcomer took a new one")
	}
	if w.st != lockBusy || nst != lockOK {
		t.Fatalf("waiter %v, newcomer %v; want the waiter busy (the deleter still held the mutex) and the newcomer ok", w.st, nst)
	}
	if any(nmu) == old {
		t.Fatal("the newcomer took the deleted suite's old mutex; want a fresh entry (the deleter drops the old one)")
	}
}

// Ruling PD-11: with the checker as the guard, cap eviction reports a
// future last_requested_at through the checker's rate-limited
// clock_step_detected (and acu_clock_steps_detected_total), and leaves the
// suite alone.
func TestCreateSuite_ReportsAFutureStampThroughTheChecker(t *testing.T) {
	_, rc, _ := gatedAdopter(t)
	logs := &lockedBuffer{}
	c, err := New(Config{Cache: rc, Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8),
		Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	future := time.Now().Unix() + 86400
	if _, err := rc.CreateSuite(ctx, cache.SuiteSeed{SuiteKey: lockKey, RequestedAt: future}, cache.SuiteCaps{}, nil); err != nil {
		t.Fatal(err)
	}
	steps := func() float64 {
		v, _ := strconv.ParseFloat(renderedMetric("acu_clock_steps_detected_total"), 64)
		return v
	}
	before := steps()
	res, err := rc.CreateSuite(ctx, cache.SuiteSeed{SuiteKey: cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h",
		SuitePath: "/r/dists/new"}, RequestedAt: time.Now().Unix()}, cache.SuiteCaps{Global: 1}, c)
	if err != nil || res.Outcome != cache.SuiteRejectedGlobal || res.Evicted != nil {
		t.Fatalf("CreateSuite = %+v, %v; want global_cap, nothing evicted", res, err)
	}
	if got := steps() - before; got != 1 {
		t.Fatalf("acu_clock_steps_detected_total rose by %v, want 1", got)
	}
	// Deferred minor D-24: ReportClockStep passes three strings by
	// position; each must reach its own field.
	out := logs.String()
	if !strings.Contains(out, "clock_step_detected") || !strings.Contains(out, "stamp=last_requested_at") ||
		!strings.Contains(out, "canonical_scheme=http ") || !strings.Contains(out, "canonical_host=h ") ||
		!strings.Contains(out, "suite_path=/r/dists/x ") {
		t.Fatalf("log = %q; want clock_step_detected for http://h/r/dists/x's last_requested_at", out)
	}
}
