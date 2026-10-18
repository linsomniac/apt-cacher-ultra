package freshness

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// startSched runs a checker's scheduler until the returned stop is called
// (also registered as a cleanup; calling it twice is safe).
func startSched(t *testing.T, cfg Config) (*Checker, func()) {
	t.Helper()
	chk, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { chk.Run(ctx); close(done) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			<-done
			chk.WaitForAdoptions()
		})
	}
	t.Cleanup(stop)
	return chk, stop
}

// check reports what it did (busy, skipped, changed_queued with the
// snapshot's signedness, changed, not_modified), and bypassCooldown lets
// the scheduler's slot-driven re-check through the cooldown.
func TestCheck_ReportsResultAndBypassesCooldown(t *testing.T) {
	r := newRetryRig(t, time.Hour)
	ctx := context.Background()
	ref := SuiteRef{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite}
	v, _ := r.checker.locks.LoadOrStore(suiteKey("http", rigHost, rigSuite), &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	if rep := r.checker.check(ctx, ref, checkOpts{}); rep.result != resultBusy {
		t.Fatalf("mutex held: result %q, want %q", rep.result, resultBusy)
	}
	mu.Unlock()

	held := mustAcquire(t, r.checker.admission, rigHost, true)
	r.up.set(r.release("v1", 0))
	if rep := r.checker.check(ctx, ref, checkOpts{}); rep.result != "changed_queued" || rep.signedCurrent {
		t.Fatalf("no slot: %+v, want changed_queued for a suite with no signed snapshot", rep)
	}
	if rep := r.checker.check(ctx, ref, checkOpts{}); rep.result != resultSkipped {
		t.Fatalf("inside the cooldown: result %q, want %q", rep.result, resultSkipped)
	}
	held.release()
	if rep := r.checker.check(ctx, ref, checkOpts{bypassCooldown: true}); rep.result != "changed" {
		t.Fatalf("bypassed cooldown with a free slot: result %q, want changed", rep.result)
	}
	r.checker.WaitForAdoptions()

	held = mustAcquire(t, r.checker.admission, rigHost, true)
	defer held.release()
	r.up.set(r.release("v2", 0))
	if rep := r.checker.check(ctx, ref, checkOpts{bypassCooldown: true}); rep.result != "changed_queued" || !rep.signedCurrent {
		t.Fatalf("no slot after adoption: %+v, want changed_queued with signedCurrent", rep)
	}
	r.up.set(r.release("v1", 0)) // upstream reverts to the adopted content
	if rep := r.checker.check(ctx, ref, checkOpts{bypassCooldown: true}); rep.result != "not_modified" {
		t.Fatalf("reverted upstream: result %q, want not_modified", rep.result)
	}
}

// Spec C4 slot-driven re-offer: a suite that found no slot waits without
// being probed by later ticks and is re-checked as soon as a slot is
// released, cooldown or not; offers return to zero, so a second waiting
// suite follows when the first adoption releases its slot.
func TestRun_ReoffersWhenASlotFrees(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		c := simtest.OpenCache(t)
		up := simtest.NewUpstream()
		members := newFakeFetcher()
		hosts := []string{"h1", "h2"}
		for i, host := range hosts {
			seedStaleAnchorAt(t, c, host, rigSuite)
			pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat(string(rune('a'+i)), 64)})
			members.put("http://"+host+rigSuite+"/main/binary-amd64/Packages", pkgs)
			up.Set("http://"+host+rigSuite+"/InRelease",
				simtest.Serve(datedRelease("v1-"+host, 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})))
		}
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
			HostLimiter: hostsem.New(8), MaxConcurrent: 1})
		if err != nil {
			t.Fatal(err)
		}
		chk, stop := startSched(t, Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8),
			Refresh: time.Hour, Cooldown: time.Hour, Adopter: ad, Logger: simtest.DiscardLogger()})
		held := mustAcquire(t, chk.admission, "other", true)
		anchor := func(host string) string { return "http://" + host + rigSuite + "/InRelease" }

		time.Sleep(16 * time.Minute) // the first tick (15 min) checks both; both wait
		for _, host := range hosts {
			if n := len(up.Calls(anchor(host))); n != 1 {
				t.Fatalf("%s: %d anchor GETs after the first tick, want 1", host, n)
			}
		}
		time.Sleep(15 * time.Minute) // another tick: waiting suites are not probed
		for _, host := range hosts {
			if n := len(up.Calls(anchor(host))); n != 1 {
				t.Fatalf("%s: %d anchor GETs while waiting, want still 1 (no re-probe)", host, n)
			}
		}
		released := time.Now()
		held.release()
		time.Sleep(time.Minute)
		stop()
		for _, host := range hosts {
			calls := up.Calls(anchor(host))
			if len(calls) != 2 {
				t.Fatalf("%s: %d anchor GETs, want 2 (one per offer)", host, len(calls))
			}
			if d := calls[1].Sub(released); d > checkDeadline {
				t.Fatalf("%s re-checked %v after the release, want within %v", host, d, checkDeadline)
			}
			if sf, err := c.GetSuiteFreshness(ctx, "http", host, rigSuite); err != nil || sf.CurrentSnapshotID == nil {
				t.Fatalf("%s not adopted after its slot was offered (err %v)", host, err)
			}
		}
	})
}

// An offer that loses its slot to someone else keeps its place: within a
// host, the suite that waited first is offered first again.
func TestRun_OfferThatLosesTheSlotKeepsItsPlace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		c := simtest.OpenCache(t)
		up := simtest.NewUpstream()
		members := newFakeFetcher()
		rels := map[string][]byte{}
		for i, suite := range []string{"/a/dists/x", "/b/dists/x"} {
			seedStaleAnchorAt(t, c, "h", suite)
			pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat(string(rune('a'+i)), 64)})
			members.put("http://h"+suite+"/main/binary-amd64/Packages", pkgs)
			rels[suite] = datedRelease("v1"+suite, 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})
		}
		var aCalls atomic.Int32
		up.Set("http://h/a/dists/x/InRelease", func(ctx context.Context, etag string) (*fetch.ConditionalResult, error) {
			if aCalls.Add(1) == 2 { // the first offer: slow, so the slot can be taken meanwhile
				return simtest.After(30*time.Second, simtest.Serve(rels["/a/dists/x"]))(ctx, etag)
			}
			return simtest.Serve(rels["/a/dists/x"])(ctx, etag)
		})
		up.Set("http://h/b/dists/x/InRelease", simtest.Serve(rels["/b/dists/x"]))
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
			HostLimiter: hostsem.New(8), MaxConcurrent: 1})
		if err != nil {
			t.Fatal(err)
		}
		chk, stop := startSched(t, Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8),
			Refresh: time.Hour, Cooldown: time.Hour, Adopter: ad, Logger: simtest.DiscardLogger()})
		held := mustAcquire(t, chk.admission, "other", true)
		time.Sleep(16 * time.Minute) // first tick: /a then /b wait on host h
		held.release()               // the offer goes to /a (oldest), whose GET takes 30 s
		time.Sleep(10 * time.Second)
		held = mustAcquire(t, chk.admission, "other", true) // someone else takes the slot
		time.Sleep(50 * time.Second)                        // /a's offer ends queued: it keeps the head
		held.release()
		time.Sleep(time.Minute)
		stop()
		if n := aCalls.Load(); n != 3 {
			t.Fatalf("/a anchor GETs = %d, want 3 (first tick, lost offer, second offer)", n)
		}
		a, errA := c.GetSuiteFreshness(ctx, "http", "h", "/a/dists/x")
		b, errB := c.GetSuiteFreshness(ctx, "http", "h", "/b/dists/x")
		if errA != nil || errB != nil || a.CurrentSnapshotID == nil || b.CurrentSnapshotID == nil {
			t.Fatalf("not both adopted: %v %v", errA, errB)
		}
		if *a.CurrentSnapshotID > *b.CurrentSnapshotID {
			t.Fatal("/b adopted before /a although /a waited first and kept its place")
		}
	})
}

// An offer that finds the suite busy (a client-triggered check holds its
// mutex) neither spins nor loses the suite: it is held, keeping its place,
// and offered again on the next wake or tick.
//
// AIDEV-NOTE: if the hold is removed this test never finishes: the Run
// loop and a worker re-offer the busy suite in a tight loop, so the
// bubble's clock never advances. Run mutants with -timeout.
func TestRun_BusyOfferIsRetriedAfterTheNextTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		c := simtest.OpenCache(t)
		up := simtest.NewUpstream()
		members := newFakeFetcher()
		seedStaleAnchorAt(t, c, "h", rigSuite)
		pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": testHex64})
		members.put("http://h"+rigSuite+"/main/binary-amd64/Packages", pkgs)
		anchor := "http://h" + rigSuite + "/InRelease"
		up.Set(anchor, simtest.Serve(datedRelease("v1", 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})))
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
			HostLimiter: hostsem.New(8), MaxConcurrent: 1})
		if err != nil {
			t.Fatal(err)
		}
		chk, stop := startSched(t, Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8),
			Refresh: time.Hour, Cooldown: time.Hour, Adopter: ad, Logger: simtest.DiscardLogger()})
		held := mustAcquire(t, chk.admission, "other", true)
		time.Sleep(16 * time.Minute) // the first tick (15 min): the suite waits
		v, _ := chk.locks.LoadOrStore(suiteKey("http", "h", rigSuite), &sync.Mutex{})
		mu := v.(*sync.Mutex)
		mu.Lock()      // a client-triggered check is running
		held.release() // the offer finds the suite busy
		time.Sleep(time.Minute)
		mu.Unlock() // the client check ends without taking the slot
		time.Sleep(time.Minute)
		if n := len(up.Calls(anchor)); n != 1 {
			t.Fatalf("%d anchor GETs before the next tick, want 1 (a busy offer is held)", n)
		}
		time.Sleep(13 * time.Minute) // the next tick (30 min) offers it again
		stop()
		if n := len(up.Calls(anchor)); n != 2 {
			t.Fatalf("%d anchor GETs, want 2 (the first tick and the offer after the hold)", n)
		}
		if sf, err := c.GetSuiteFreshness(ctx, "http", "h", rigSuite); err != nil || sf.CurrentSnapshotID == nil {
			t.Fatalf("not adopted after the hold ended (err %v)", err)
		}
	})
}

// A held suite is offered again on the next wake, not only on the next
// tick: a release anywhere in the pool unholds it.
func TestRun_HeldOfferIsRetriedOnTheNextWake(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := simtest.OpenCache(t)
		up := simtest.NewUpstream()
		members := newFakeFetcher()
		seedStaleAnchorAt(t, c, "h", rigSuite)
		pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": testHex64})
		members.put("http://h"+rigSuite+"/main/binary-amd64/Packages", pkgs)
		anchor := "http://h" + rigSuite + "/InRelease"
		up.Set(anchor, simtest.Serve(datedRelease("v1", 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})))
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
			HostLimiter: hostsem.New(8), MaxConcurrent: 1})
		if err != nil {
			t.Fatal(err)
		}
		chk, stop := startSched(t, Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8),
			Refresh: time.Hour, Cooldown: time.Hour, Adopter: ad, Logger: simtest.DiscardLogger()})
		held := mustAcquire(t, chk.admission, "other", true)
		time.Sleep(16 * time.Minute) // the first tick (15 min): the suite waits
		v, _ := chk.locks.LoadOrStore(suiteKey("http", "h", rigSuite), &sync.Mutex{})
		mu := v.(*sync.Mutex)
		mu.Lock()      // a client-triggered check is running
		held.release() // the offer finds the suite busy: held
		time.Sleep(time.Minute)
		mu.Unlock()
		mustAcquire(t, chk.admission, "other", true).release() // a release elsewhere wakes the scheduler
		time.Sleep(time.Minute)
		if n := len(up.Calls(anchor)); n != 2 {
			t.Fatalf("%d anchor GETs after the wake, want 2 (a held suite is offered on the next wake, not only on the next tick)", n)
		}
		stop()
	})
}
