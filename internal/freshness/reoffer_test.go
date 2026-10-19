package freshness

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
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
	if rep := r.checker.check(ctx, ref, checkOpts{}); rep.result != "first_adoption_queued" || rep.signedCurrent {
		t.Fatalf("no slot: %+v, want first_adoption_queued for a suite with no current snapshot", rep)
	}
	if rep := r.checker.check(ctx, ref, checkOpts{}); rep.result != resultSkipped {
		t.Fatalf("inside the cooldown: result %q, want %q", rep.result, resultSkipped)
	}
	held.release()
	if rep := r.checker.check(ctx, ref, checkOpts{bypassCooldown: true}); rep.result != "first_adoption" {
		t.Fatalf("bypassed cooldown with a free slot: result %q, want first_adoption", rep.result)
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
				simtest.Serve(signedRelease("v1-"+host, 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})))
		}
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
			HostLimiter: hostsem.New(8), MaxConcurrent: 1, Logger: simtest.DiscardLogger()})
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
			rels[suite] = signedRelease("v1"+suite, 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})
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
			HostLimiter: hostsem.New(8), MaxConcurrent: 1, Logger: simtest.DiscardLogger()})
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
		up.Set(anchor, simtest.Serve(signedRelease("v1", 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})))
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
			HostLimiter: hostsem.New(8), MaxConcurrent: 1, Logger: simtest.DiscardLogger()})
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
		up.Set(anchor, simtest.Serve(signedRelease("v1", 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})))
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
			HostLimiter: hostsem.New(8), MaxConcurrent: 1, Logger: simtest.DiscardLogger()})
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

// Spec C4: "A client-triggered check that is refused leaves the suite due;
// the next tick parks it." A client check that finds no slot leaves the
// change pending (inrelease_change_seen_at set, no failure record), which
// makes the suite due even though its last success is recent; the next
// tick checks it, cooldown bypassed, and parks it, so the slot released
// later is offered to it at once (final review I-1, probe 1: it used to
// wait until last_success_at + periodic_refresh, 15 min, with the slot
// idle).
func TestRun_ClientRefusedSuiteIsParkedByTheNextTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		c := simtest.OpenCache(t)
		up := simtest.NewUpstream()
		members := newFakeFetcher()
		const host, suite = "h", "/r/dists/x"
		seedStaleAnchorAt(t, c, host, suite)
		recent := time.Now().Add(-time.Minute).Unix()
		if err := c.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: host,
			SuitePath: suite, LastCheckAt: &recent, LastSuccessAt: &recent}); err != nil {
			t.Fatal(err)
		}
		pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": testHex64})
		members.put("http://h"+suite+"/main/binary-amd64/Packages", pkgs)
		anchor := "http://h" + suite + "/InRelease"
		up.Set(anchor, simtest.Serve(signedRelease("v1", 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})))
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
			HostLimiter: hostsem.New(8), MaxConcurrent: 1, Logger: simtest.DiscardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		chk, stop := startSched(t, Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8),
			Refresh: 15 * time.Minute, Cooldown: time.Minute, Adopter: ad, Logger: simtest.DiscardLogger()})
		held := mustAcquire(t, chk.admission, "other", true)
		time.Sleep(10 * time.Second)
		chk.Check(ctx, "http", host, suite) // T1: changed_queued
		time.Sleep(4 * time.Minute)         // the first tick (3m45s) has run
		if n := len(up.Calls(anchor)); n != 2 {
			t.Fatalf("%d anchor GETs by 4m10s, want 2 (the client check and the first tick's, cooldown bypassed)", n)
		}
		released := time.Now()
		held.release() // the slot frees at ~4m10s
		time.Sleep(checkDeadline + time.Minute)
		stop()
		sf, err := c.GetSuiteFreshness(ctx, "http", host, suite)
		if err != nil || sf.CurrentSnapshotID == nil {
			t.Fatalf("client-refused suite not adopted within D of the release (spec C4: the next tick parks it); err %v", err)
		}
		calls := up.Calls(anchor)
		if len(calls) != 3 {
			t.Fatalf("%d anchor GETs, want 3 (client check, first tick, the offer)", len(calls))
		}
		if d := calls[2].Sub(released); d > checkDeadline {
			t.Fatalf("offered %v after the release, want within %v", d, checkDeadline)
		}
	})
}

// Final review I-1, probe 2: a suite whose change a client check saw first,
// with client checks every 61 s keeping its last_check_at inside the
// cooldown, is parked by the scheduler like any other suite and goes
// before an idle suite whose change the scheduler saw later. It used to
// be cooldown-skipped by every tick, never parked, and adopted only after
// the idle suite.
func TestRun_ClientRefusedBusySuiteIsParkedInOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		c := simtest.OpenCache(t)
		up := simtest.NewUpstream()
		members := &simMembers{fakeFetcher: newFakeFetcher(), stalled: map[string]bool{}, delay: map[string]time.Duration{}}
		const suite = "/r/dists/x"
		for _, host := range []string{"s", "w"} {
			seedStaleAnchorAt(t, c, host, suite)
			pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_" + host + ".deb": testHex64})
			pu := "http://" + host + suite + "/main/binary-amd64/Packages"
			members.put(pu, pkgs)
			members.setDelay(pu, 10*time.Minute)
			up.Set("http://"+host+suite+"/InRelease",
				simtest.Serve(signedRelease("v1-"+host, 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})))
		}
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
			HostLimiter: hostsem.New(8), MaxConcurrent: 1, Logger: simtest.DiscardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		chk, stop := startSched(t, Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8),
			Refresh: 15 * time.Minute, Cooldown: time.Minute, Adopter: ad, Logger: simtest.DiscardLogger()})
		t0 := time.Now()
		held := mustAcquire(t, chk.admission, "other", true)
		clientsDone := make(chan struct{})
		go func() { // steady client traffic on S
			defer close(clientsDone)
			time.Sleep(10 * time.Second)
			for time.Since(t0) < 40*time.Minute {
				chk.Check(ctx, "http", "s", suite)
				time.Sleep(61 * time.Second)
			}
		}()
		time.Sleep(8 * time.Minute)
		held.release()
		adopted := map[string]time.Duration{}
		for time.Since(t0) < 40*time.Minute {
			time.Sleep(15 * time.Second)
			for _, h := range []string{"s", "w"} {
				if sf, err := c.GetSuiteFreshness(ctx, "http", h, suite); err == nil && sf.CurrentSnapshotID != nil && adopted[h] == 0 {
					adopted[h] = time.Since(t0)
				}
			}
		}
		<-clientsDone
		stop()
		if adopted["s"] == 0 || adopted["w"] == 0 {
			t.Fatalf("not both adopted in 40 min: %v", adopted)
		}
		if adopted["s"] > adopted["w"] {
			t.Errorf("S (seen first, clients active) adopted at %v, after idle W at %v", adopted["s"], adopted["w"])
		}
	})
}
