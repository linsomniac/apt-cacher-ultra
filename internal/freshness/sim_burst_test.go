package freshness

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// Phase E DoD (overview "E:" carry-forward), the upgrade burst: 50
// never-adopted suites with client interest (each first adoption takes one
// virtual minute) and a trickle of changes to five signed suites, a pool of
// two. The first adoptions run one at a time and back to back (the last
// free slot is reserved for signed suites); a signed change never waits for
// more than one adoption; each first-adoption suite costs at most two
// anchor probes before its adoption starts (the tick's and the offer's);
// goroutines stay bounded.
//
// AIDEV-NOTE: the signed suites' anchors answer 1 s late, so an offer made
// at a slot release — on the tick grid, like the signed suites' checks —
// takes its slot before a signed check that runs at the same instant asks
// for one: same-instant goroutine order inside a bubble is not
// deterministic, and an offer that loses its slot to a concurrent check
// costs its suite one more probe (a lost offer, spec C4: it keeps its place
// and waits for the next release). The two-probe bound is for offers that
// meet no concurrent admission and suites no client checks (phase E plan,
// design note 26); TestSimCapacity_UpgradeBurstSameHost has both. The run
// lasts 80 virtual minutes (the last adoption starts at about 68): ruling
// PE-8's 5 s budget under -race, with margin.
func TestSimCapacity_UpgradeBurst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		c := simtest.OpenCache(t)
		members := &simMembers{fakeFetcher: newFakeFetcher(), stalled: map[string]bool{}, delay: map[string]time.Duration{}}
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
			HostLimiter: hostsem.New(8), MaxConcurrent: 2, MaxDuration: 30 * time.Minute, Logger: simtest.DiscardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		pkgsFor := func(label string) []byte {
			return fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_" + label + ".deb": testHex64})
		}
		pkgsURL := func(host string) string { return "http://" + host + rigSuite + "/main/binary-amd64/Packages" }
		anchorURL := func(host string) string { return "http://" + host + rigSuite + "/InRelease" }
		up := simtest.NewUpstream()
		var first []string
		for i := 0; i < 50; i++ {
			host := fmt.Sprintf("f%02d", i)
			seedStaleAnchorAt(t, c, host, rigSuite) // client interest now, a client-created anchor
			members.put(pkgsURL(host), pkgsFor(host))
			members.setDelay(pkgsURL(host), time.Minute)
			up.Set(anchorURL(host), simtest.Serve(signedRelease("first-"+host, 0,
				map[string][]byte{"main/binary-amd64/Packages": pkgsFor(host)})))
			first = append(first, host)
		}
		t0 := time.Now()
		changeAt := map[string]time.Duration{}
		v2 := map[string][]byte{}
		var signed []string
		for i := 0; i < 5; i++ {
			host := fmt.Sprintf("s%d", i)
			ref := SuiteRef{CanonicalScheme: "http", CanonicalHost: host, SuitePath: rigSuite}
			members.put(pkgsURL(host), pkgsFor("1-"+host))
			trackSuite(t, c, ref)
			v1 := signedRelease("v1-"+host, 0, map[string][]byte{"main/binary-amd64/Packages": pkgsFor("1-" + host)})
			if err := ad.Run(ctx, ref, v1, "", ""); err != nil {
				t.Fatal(err)
			}
			members.put(pkgsURL(host), pkgsFor("2-"+host))
			members.setDelay(pkgsURL(host), time.Minute)
			v2[host] = signedRelease("v2-"+host, 0, map[string][]byte{"main/binary-amd64/Packages": pkgsFor("2-" + host)})
			at := time.Duration(10+12*i)*time.Minute + 7*time.Second // a trickle, off the tick grid
			changeAt[host] = at
			v2body := v2[host]
			// The signed anchors answer 1 s late (see the AIDEV-NOTE above).
			up.Set(anchorURL(host), simtest.After(time.Second, func(ctx context.Context, etag string) (*fetch.ConditionalResult, error) {
				if time.Since(t0) < at {
					return simtest.Serve(v1)(ctx, etag)
				}
				return simtest.Serve(v2body)(ctx, etag)
			}))
			signed = append(signed, host)
		}
		members.recordStarts()

		runtime.Gosched()
		base := runtime.NumGoroutine()
		maxG := base
		_, stop := startSched(t, Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute,
			Cooldown: time.Minute, Adopter: ad, Logger: simtest.DiscardLogger(), rand: func() float64 { return 0.5 }})
		for time.Since(t0) < 80*time.Minute {
			time.Sleep(61 * time.Second) // off the 3 m 45 s tick grid
			maxG = max(maxG, runtime.NumGoroutine())
		}
		stop()

		members.mu.Lock()
		started := make(map[string]time.Time, len(members.started))
		for h, at := range members.started {
			started[h] = at
		}
		members.mu.Unlock()
		// (1) Every first adoption ran, one at a time, back to back: each
		// admission at least one adoption after the previous one and at
		// most two adoptions (a signed one may run in between) + D later.
		var starts []time.Time
		for _, host := range first {
			at, ok := started[host]
			if !ok {
				t.Fatalf("%s: its first adoption never started", host)
			}
			if sf, err := c.GetSuiteFreshness(ctx, "http", host, rigSuite); err != nil || sf.CurrentSnapshotID == nil {
				t.Fatalf("%s: not adopted after 3 h (err %v)", host, err)
			}
			starts = append(starts, at)
			// (3) At most two anchor probes before its adoption started.
			n := 0
			for _, call := range up.Calls(anchorURL(host)) {
				if !call.After(at) {
					n++
				}
			}
			if n > 2 {
				t.Errorf("%s: %d anchor probes before its adoption started, want <= 2", host, n)
			}
		}
		slices.SortFunc(starts, time.Time.Compare)
		for i := 1; i < len(starts); i++ {
			gap := starts[i].Sub(starts[i-1])
			if gap < time.Minute {
				t.Fatalf("first adoptions %d and %d overlapped (%v apart): the last free slot is reserved", i-1, i, gap)
			}
			if gap > 2*time.Minute+checkDeadline {
				t.Errorf("first adoptions %d and %d were %v apart, want back to back (<= %v)", i-1, i, gap, 2*time.Minute+checkDeadline)
			}
		}
		// (2) A signed change waits for at most one adoption: from the
		// first probe that saw it to its adoption's start.
		for _, host := range signed {
			at, ok := started[host]
			if !ok {
				t.Fatalf("%s: its v2 adoption never started", host)
			}
			var seen time.Time
			for _, call := range up.Calls(anchorURL(host)) {
				if call.Sub(t0) >= changeAt[host] {
					seen = call
					break
				}
			}
			if seen.IsZero() || at.Sub(seen) > time.Minute+checkDeadline {
				t.Errorf("%s: its change was seen at %v and its adoption started %v later, want <= one adoption + D",
					host, seen.Sub(t0), at.Sub(seen))
			}
		}
		// (4) Workers + Run + two adoptions, plus slack for transient
		// goroutines (the cache writer's work, timer callbacks).
		const slack = 8
		if limit := base + checkWorkers + 1 + 2 + slack; maxG > limit {
			t.Fatalf("goroutines peaked at %d, limit %d", maxG, limit)
		}
	})
}

// startsByURL wraps a member fetcher and records each URL's first fetch:
// the start of the adoption that fetches it.
type startsByURL struct {
	AdoptionFetcher
	mu sync.Mutex
	at map[string]time.Time
}

func (s *startsByURL) Fetch(ctx context.Context, target *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error) {
	s.mu.Lock()
	if _, seen := s.at[target.URL]; !seen {
		s.at[target.URL] = time.Now()
	}
	s.mu.Unlock()
	return s.AdoptionFetcher.Fetch(ctx, target, dst)
}

// Ruling PE-4 (design review I-5): the upgrade burst where production has
// it — shared hosts (an archive host carries several suites) — and with
// admissions that race: 3 hosts × (8 never-adopted + 2 signed) suites, a
// pool of two, commits that take 2 s, a client checking each never-adopted
// suite every 5 minutes until it is adopted, and no delay ordering the
// signed checks behind the offers. Asserted:
//  1. first adoptions run one at a time;
//  2. a signed change waits for at most one adoption (its commit included)
//     + D + one host turn (its host's check in flight, at most D);
//  3. each never-adopted suite's anchor probes before its adoption started
//     are at most 2 (the scheduler's) + its client checks + its lost offers
//     (offers another admission took first);
//  4. lost offers in all are at most the admissions in all — each lost
//     offer coincides with another admission (work-conserving);
//  5. goroutines stay bounded.
func TestSimCapacity_UpgradeBurstSameHost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		c := simtest.OpenCache(t)
		members := &simMembers{fakeFetcher: newFakeFetcher(), stalled: map[string]bool{}, delay: map[string]time.Duration{}}
		starts := &startsByURL{AdoptionFetcher: members, at: map[string]time.Time{}}
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: starts, Verifier: passThroughVerifier{},
			HostLimiter: hostsem.New(8), MaxConcurrent: 2, MaxDuration: 30 * time.Minute, Logger: simtest.DiscardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		const commit = 2 * time.Second
		ad.SetCommitDelayForTest(commit)
		pkgsFor := func(label string) []byte {
			return fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_" + label + ".deb": testHex64})
		}
		pkgsURL := func(s SuiteRef) string {
			return "http://" + s.CanonicalHost + s.SuitePath + "/main/binary-amd64/Packages"
		}
		anchorURL := func(s SuiteRef) string { return "http://" + s.CanonicalHost + s.SuitePath + "/InRelease" }
		up := simtest.NewUpstream()
		t0 := time.Now()
		var first, signed []SuiteRef
		changeAt := map[SuiteRef]time.Duration{}
		for h := 0; h < 3; h++ {
			host := fmt.Sprintf("h%d", h)
			for i := 0; i < 8; i++ {
				s := SuiteRef{CanonicalScheme: "http", CanonicalHost: host, SuitePath: fmt.Sprintf("/r/dists/f%d", i)}
				seedStaleAnchorAt(t, c, host, s.SuitePath) // client interest now, a client-created anchor
				label := host + s.SuitePath
				members.put(pkgsURL(s), pkgsFor(label))
				members.delay[pkgsURL(s)] = time.Minute
				up.Set(anchorURL(s), simtest.Serve(signedRelease("first "+label, 0,
					map[string][]byte{"main/binary-amd64/Packages": pkgsFor(label)})))
				first = append(first, s)
			}
			for i := 0; i < 2; i++ {
				s := SuiteRef{CanonicalScheme: "http", CanonicalHost: host, SuitePath: fmt.Sprintf("/r/dists/s%d", i)}
				label := host + s.SuitePath
				members.put(pkgsURL(s), pkgsFor("1 "+label))
				trackSuite(t, c, s)
				v1 := signedRelease("v1 "+label, 0, map[string][]byte{"main/binary-amd64/Packages": pkgsFor("1 " + label)})
				if err := ad.Run(ctx, s, v1, "", ""); err != nil {
					t.Fatal(err)
				}
				members.put(pkgsURL(s), pkgsFor("2 "+label))
				members.delay[pkgsURL(s)] = time.Minute
				v2 := signedRelease("v2 "+label, 0, map[string][]byte{"main/binary-amd64/Packages": pkgsFor("2 " + label)})
				at := time.Duration(5+6*len(signed))*time.Minute + 7*time.Second // a trickle through the burst
				changeAt[s] = at
				up.Set(anchorURL(s), func(ctx context.Context, etag string) (*fetch.ConditionalResult, error) {
					if time.Since(t0) < at {
						return simtest.Serve(v1)(ctx, etag)
					}
					return simtest.Serve(v2)(ctx, etag)
				})
				signed = append(signed, s)
			}
		}
		starts.mu.Lock()
		starts.at = map[string]time.Time{} // the signed v1 adoptions above do not count
		starts.mu.Unlock()

		var mu sync.Mutex
		lost := map[SuiteRef]int{}
		clients := map[SuiteRef][]time.Time{}
		started := func(s SuiteRef) (time.Time, bool) {
			starts.mu.Lock()
			defer starts.mu.Unlock()
			at, ok := starts.at[pkgsURL(s)]
			return at, ok
		}
		runtime.Gosched()
		base := runtime.NumGoroutine()
		maxG := base
		chk, err := New(Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute,
			Cooldown: time.Minute, Adopter: ad, Logger: simtest.DiscardLogger(), rand: func() float64 { return 0.5 }})
		if err != nil {
			t.Fatal(err)
		}
		chk.offerHook = func(ref SuiteRef, rep checkReport) {
			if isQueuedResult(rep.result) {
				mu.Lock()
				lost[ref]++
				mu.Unlock()
			}
		}
		runCtx, cancel := context.WithCancel(ctx)
		runDone := make(chan struct{})
		go func() { chk.Run(runCtx); close(runDone) }()
		clientDone := make(chan struct{})
		go func() { // the clients: every 5 minutes, apt update on each suite not adopted yet
			defer close(clientDone)
			for runCtx.Err() == nil {
				for _, s := range first {
					if _, ok := started(s); ok {
						continue
					}
					mu.Lock()
					clients[s] = append(clients[s], time.Now())
					mu.Unlock()
					chk.Check(runCtx, s.CanonicalScheme, s.CanonicalHost, s.SuitePath)
				}
				select {
				case <-runCtx.Done():
				case <-time.After(5 * time.Minute):
				}
			}
		}()
		for time.Since(t0) < 50*time.Minute {
			time.Sleep(61 * time.Second) // off the 3 m 45 s tick grid
			maxG = max(maxG, runtime.NumGoroutine())
		}
		cancel()
		<-runDone
		<-clientDone
		chk.WaitForAdoptions()

		mu.Lock()
		defer mu.Unlock()
		// (1) Every first adoption ran, one at a time.
		var firstStarts []time.Time
		totalLost := 0
		for _, s := range first {
			at, ok := started(s)
			if !ok {
				t.Fatalf("%v: its first adoption never started", s)
			}
			if sf, err := c.GetSuiteFreshness(ctx, "http", s.CanonicalHost, s.SuitePath); err != nil || sf.CurrentSnapshotID == nil {
				t.Fatalf("%v: not adopted after 50 min (err %v)", s, err)
			}
			firstStarts = append(firstStarts, at)
			// (3) Anchor probes before its adoption started.
			probes, checks := 0, 0
			for _, call := range up.Calls(anchorURL(s)) {
				if !call.After(at) {
					probes++
				}
			}
			for _, call := range clients[s] {
				if !call.After(at) {
					checks++
				}
			}
			if probes > 2+checks+lost[s] {
				t.Errorf("%v: %d anchor probes before its adoption, want <= 2 + %d client checks + %d lost offers",
					s, probes, checks, lost[s])
			}
			totalLost += lost[s]
		}
		slices.SortFunc(firstStarts, time.Time.Compare)
		for i := 1; i < len(firstStarts); i++ {
			if gap := firstStarts[i].Sub(firstStarts[i-1]); gap < time.Minute {
				t.Fatalf("first adoptions %d and %d overlapped (%v apart): the last free slot is reserved", i-1, i, gap)
			}
		}
		// (2) A signed change waits for at most one adoption + D + one host turn.
		for _, s := range signed {
			at, ok := started(s)
			if !ok {
				t.Fatalf("%v: its v2 adoption never started", s)
			}
			var seen time.Time
			for _, call := range up.Calls(anchorURL(s)) {
				if call.Sub(t0) >= changeAt[s] {
					seen = call
					break
				}
			}
			if limit := time.Minute + commit + 2*checkDeadline; seen.IsZero() || at.Sub(seen) > limit {
				t.Errorf("%v: its change was seen at %v and its adoption started %v later, want <= %v",
					s, seen.Sub(t0), at.Sub(seen), limit)
			}
			totalLost += lost[s]
		}
		// (4) Work-conserving: a lost offer needs another admission.
		if admissions := len(first) + len(signed); totalLost > admissions {
			t.Errorf("%d lost offers for %d admissions", totalLost, admissions)
		}
		// (5) Workers + Run + the clients + two adoptions, plus slack for
		// transient goroutines (the cache writer's work, timer callbacks).
		const slack = 8
		if limit := base + checkWorkers + 1 + 1 + 2 + slack; maxG > limit {
			t.Fatalf("goroutines peaked at %d, limit %d", maxG, limit)
		}
	})
}
