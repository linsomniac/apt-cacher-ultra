package freshness

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"runtime"
	"slices"
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
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// seedSimSuite caches an anchor and a suite row last checked and last
// succeeded at lastAt, with the given unknown-check streak.
func seedSimSuite(t *testing.T, c *cache.Cache, host, suite string, lastAt, failCount int64) {
	t.Helper()
	ctx := context.Background()
	bh := testHex64
	if err := c.PutBlob(ctx, bh, 1); err != nil {
		t.Fatal(err)
	}
	if err := c.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: host, Path: suite + "/InRelease",
		BlobHash: &bh, UpstreamURL: "http://" + host + suite + "/InRelease", IsMetadata: true}); err != nil {
		t.Fatal(err)
	}
	if err := c.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: host, SuitePath: suite,
		LastCheckAt: &lastAt, LastSuccessAt: &lastAt, CheckFailCount: failCount}); err != nil {
		t.Fatal(err)
	}
}

// Spec C4 DoD (1): H = 8 hosts, several stalling; a healthy suite S with
// n = 3 healthy (slow) suites ahead of it is checked within
// one tick + (n + 1) × ⌈H/4⌉ × D — while a persistent repair (R on h1)
// and suites whose upstream changes on every request (C on h2, C3 on h3)
// contend for the two adoption slots, checks continue everywhere and
// goroutines stay bounded. Repair and adoption goroutines must never hold
// a check worker (spec C4): while R's repair and C's adoption hold both
// slots, all checkWorkers workers are still checking.
//
// Timeline (virtual): the first tick (15 m) checks R, C and C3 first on
// their hosts. R's repair (two 10-minute member fetches) holds a slot
// until 35 m; C's adoption (one 10-minute member fetch) holds the other
// until 25 m. C3 answers 1 s later, finds no slot and is queued; it is
// offered on h3's first free turn after C's slot is released (25–27 m)
// and adopts for 10 minutes.
func TestSimScheduler_LatencyBoundEightHosts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		c := simtest.OpenCache(t)
		up := simtest.NewUpstream()
		members := &simMembers{fakeFetcher: newFakeFetcher(), stalled: map[string]bool{}, delay: map[string]time.Duration{}}
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
			HostLimiter: hostsem.New(8), MaxConcurrent: 2, RepairSkippedMembers: true, TolerateOptionalMemberFailures: true,
			Logger: simtest.DiscardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		t0 := time.Now()
		stale := t0.Add(-2 * time.Hour).Unix()
		// due marks an adopted suite healthy and due at the first tick:
		// checked and succeeded at stale. It leaves the snapshot alone.
		due := func(host, suite string) {
			if err := c.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: host,
				SuitePath: suite, LastCheckAt: &stale, LastSuccessAt: &stale}); err != nil {
				t.Fatal(err)
			}
		}
		for _, s := range []string{"/a1/dists/s", "/a2/dists/s", "/a3/dists/s"} {
			seedSimSuite(t, c, "x", s, stale, 0)
			up.Set("http://x"+s+"/InRelease", simtest.After(checkDeadline-time.Second, simtest.NotModified()))
		}
		const sPath = "/zz/dists/s"
		seedSimSuite(t, c, "x", sPath, stale, 0)
		up.Set("http://x"+sPath+"/InRelease", simtest.NotModified())
		var stalling []string
		for h := 1; h <= 7; h++ {
			for k := 0; k < 2; k++ {
				host, suite := fmt.Sprintf("h%d", h), fmt.Sprintf("/s%d/dists/s", k)
				seedSimSuite(t, c, host, suite, stale, 0)
				url := "http://" + host + suite + "/InRelease"
				up.Set(url, simtest.Stall())
				stalling = append(stalling, url)
			}
		}
		// R on h1: adopted, with a Contents member that never heals, so
		// every unchanged check of R spawns a repair; each repair pass
		// probes by-hash, then the canonical path, 10 minutes each. R is
		// healthy and due, and sorts before h1's stalling suites.
		good, bad := []byte(strings.Repeat("C", 100)), []byte(strings.Repeat("z", 90))
		pkgsR := fakePackagesStanzas(map[string]string{"pool/main/r/r/r_1.deb": testHex64})
		relR := datedRelease("r", 0, map[string][]byte{"main/binary-amd64/Packages": pkgsR, "Contents-amd64.gz": good})
		rBase := "http://h1/r/dists/s/"
		rByHash := rBase + "by-hash/SHA256/" + shaOf(good)
		members.put(rBase+"main/binary-amd64/Packages", pkgsR)
		members.put(rBase+"Contents-amd64.gz", bad)
		members.put(rByHash, bad)
		trackSuite(t, c, SuiteRef{CanonicalScheme: "http", CanonicalHost: "h1", SuitePath: "/r/dists/s"})
		if err := ad.Run(ctx, SuiteRef{CanonicalScheme: "http", CanonicalHost: "h1", SuitePath: "/r/dists/s"}, relR, "", ""); err != nil {
			t.Fatal(err)
		}
		due("h1", "/r/dists/s")
		members.delay[rBase+"Contents-amd64.gz"] = 10 * time.Minute
		members.delay[rByHash] = 10 * time.Minute
		up.Set(rBase+"InRelease", simtest.Serve(relR))
		// C on h2 and C3 on h3: adopted once (so the current snapshot is
		// signed and a re-adoption may take either slot), healthy and due,
		// sorting before their hosts' stalling suites. Upstream publishes a
		// new InRelease with a new Packages body on every request: a body
		// already in the pool would be reused without a fetch and the
		// adoption would take no time. Each adoption fetches its body in
		// 10 minutes. C3 answers after wait = 1 s, so at the first tick R's
		// repair and C's adoption have taken both slots when C3 asks.
		const cSuite = "/c/dists/s"
		changing := func(host string, wait time.Duration) (anchor, seeded string) {
			base := "http://" + host + cSuite + "/"
			pkgsURL := base + "main/binary-amd64/Packages"
			pkgs := func(n int64) []byte {
				return fakePackagesStanzas(map[string]string{fmt.Sprintf("pool/main/c/c/c_%s_%d.deb", host, n): testHex64})
			}
			rel := func(n int64) []byte {
				return datedRelease(fmt.Sprintf("c-%s-%d", host, n), 0, map[string][]byte{"main/binary-amd64/Packages": pkgs(n)})
			}
			members.put(pkgsURL, pkgs(0))
			trackSuite(t, c, SuiteRef{CanonicalScheme: "http", CanonicalHost: host, SuitePath: cSuite})
			if err := ad.Run(ctx, SuiteRef{CanonicalScheme: "http", CanonicalHost: host, SuitePath: cSuite}, rel(0), "", ""); err != nil {
				t.Fatal(err)
			}
			due(host, cSuite)
			members.delay[pkgsURL] = 10 * time.Minute
			var n atomic.Int64
			var b simtest.Behavior = func(ctx context.Context, etag string) (*fetch.ConditionalResult, error) {
				k := n.Add(1)
				members.put(pkgsURL, pkgs(k)) // before the check can spawn the adoption
				return simtest.Serve(rel(k))(ctx, etag)
			}
			if wait > 0 {
				b = simtest.After(wait, b)
			}
			up.Set(base+"InRelease", b)
			return base + "InRelease", hashOf(rel(0))
		}
		cURL, cSeeded := changing("h2", 0)
		c3URL, c3Seeded := changing("h3", time.Second)

		logs := &lockedBuffer{}
		runtime.Gosched()
		baseG := runtime.NumGoroutine()
		maxG := baseG
		const refresh = time.Hour
		chk, stop := startSched(t, Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: refresh,
			Adopter: ad, Logger: slog.New(slog.NewJSONHandler(logs, nil))})
		// sample advances to until (measured from t0), sampling goroutines
		// every 61 s: off the 15-minute tick grid.
		sample := func(until time.Duration) {
			for time.Since(t0)+61*time.Second <= until {
				time.Sleep(61 * time.Second)
				maxG = max(maxG, runtime.NumGoroutine())
			}
			time.Sleep(until - time.Since(t0))
		}
		// The worker bound: from 16 m 30 s to 24 m 30 s R's repair and C's
		// adoption hold both slots, and the stalling suites keep all four
		// workers busy. A repair or adoption run on a check worker would
		// leave at most three checks in flight.
		sample(16*time.Minute + 30*time.Second)
		if inUse, _ := chk.admission.usage(); inUse != 2 {
			t.Fatalf("at 16m30s %d adoption slots were in use, want 2 (R's repair and C's adoption)", inUse)
		}
		up.ResetMax()
		sample(24*time.Minute + 30*time.Second)
		if inUse, _ := chk.admission.usage(); inUse != 2 {
			t.Fatalf("at 24m30s %d adoption slots were in use, want 2 (R's repair and C's adoption)", inUse)
		}
		if total, _ := up.MaxInFlight(); total < checkWorkers {
			t.Errorf("while a repair and an adoption held both slots, at most %d checks ran at once, want %d"+
				" (a repair or adoption is holding a check worker)", total, checkWorkers)
		}
		sample(95 * 61 * time.Second) // ~1 h 37 min, off the tick grid
		stop()

		sCalls := up.Calls("http://x" + sPath + "/InRelease")
		if len(sCalls) == 0 {
			t.Fatal("S was never checked")
		}
		bound := refresh/4 + 4*2*checkDeadline // tick + (3+1) × ⌈8/4⌉ × D = 31 min
		if got := sCalls[0].Sub(t0); got > bound {
			t.Fatalf("S first checked after %v, bound %v", got, bound)
		}
		if a3 := up.Calls("http://x/a3/dists/s/InRelease"); len(a3) == 0 || !sCalls[0].After(a3[0]) {
			t.Fatal("S overtook a healthy suite queued ahead of it")
		}
		for _, u := range append(stalling, rBase+"InRelease", cURL, c3URL) {
			if n := len(up.Calls(u)); n < 2 {
				t.Errorf("%s checked %d times in ~1h37m, want its checks to continue (>= 2)", u, n)
			}
		}
		// Every unchanged check of R spawned a repair: a slot was always
		// free for it (C and C3 never hold both when R is checked).
		repairs := 0
		for _, u := range members.requested() {
			if u == rByHash {
				repairs++
			}
		}
		unchanged := countChecks(t, logs.String(), "h1", "/r/dists/s", "unchanged", "not_modified")
		if repairs < 2 || repairs != unchanged {
			t.Errorf("R's repair ran %d times for %d unchanged checks, want one per unchanged check (>= 2)", repairs, unchanged)
		}
		// The changing suites adopted new content, C3 only after it was
		// queued for a slot (and offered one when C's was released).
		for _, s := range []struct{ host, seeded string }{{"h2", cSeeded}, {"h3", c3Seeded}} {
			sf, err := c.GetSuiteFreshness(ctx, "http", s.host, cSuite)
			if err != nil || sf.CurrentSnapshotID == nil {
				t.Fatalf("%s: no current snapshot (err %v)", s.host, err)
			}
			snap, err := c.GetSuiteSnapshot(ctx, *sf.CurrentSnapshotID)
			if err != nil || snap.SingleAnchorHash == nil || *snap.SingleAnchorHash == s.seeded {
				t.Errorf("%s never adopted new content (err %v)", s.host, err)
			}
		}
		if n := countChecks(t, logs.String(), "h3", cSuite, "changed_queued"); n == 0 {
			t.Error("C3 was never queued for a slot: the slots were never contended")
		}
		// Workers, Run, two adoption slots, and slack for timers and the
		// cache writer's transient goroutines.
		if limit := baseG + checkWorkers + 1 + 2 + 16; maxG > limit {
			t.Fatalf("goroutines peaked at %d, limit %d", maxG, limit)
		}
	})
}

// countChecks counts the freshness_check lines of a JSON log for host's
// suite whose result is one of results.
func countChecks(t *testing.T, log, host, suite string, results ...string) int {
	t.Helper()
	n := 0
	for _, line := range strings.Split(log, "\n") {
		if line == "" {
			continue
		}
		var rec struct {
			Msg    string `json:"msg"`
			Host   string `json:"canonical_host"`
			Suite  string `json:"suite_path"`
			Result string `json:"result"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if rec.Msg == "freshness_check" && rec.Host == host && rec.Suite == suite && slices.Contains(results, rec.Result) {
			n++
		}
	}
	return n
}

// Spec C4 DoD (2): 500 stalling suites and one healthy suite on one host.
// After each stalling suite has failed once, the healthy one is checked
// within one tick + ⌈1/4⌉ × D of becoming due — and again on the first
// tick after a restart, because the class and streak are persisted.
// Mutations that must fail this test: ordering never-checked suites
// first, keeping the streak in memory, or keying the order on
// last_check_at.
func TestSimScheduler_FiveHundredStallingOneHealthy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		c := simtest.OpenCache(t)
		up := simtest.NewUpstream()
		stale := time.Now().Add(-2 * time.Hour).Unix()
		for i := 0; i < 500; i++ {
			suite := fmt.Sprintf("/s%03d/dists/x", i)
			seedSimSuite(t, c, "a", suite, stale, 0)
			up.Set("http://a"+suite+"/InRelease", simtest.Stall())
		}
		const sPath = "/zzz/dists/x"
		sURL := "http://a" + sPath + "/InRelease"
		seedSimSuite(t, c, "a", sPath, stale, 0)
		up.Set(sURL, simtest.NotModified())
		const refresh = time.Hour
		tick := refresh / 4
		cfg := Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: refresh, Logger: simtest.DiscardLogger()}
		_, stop := startSched(t, cfg)

		// Phase 1: every stalling suite fails once while still in the
		// healthy class (spec C4: a first failure is paid once):
		// 500 × 2 min ≈ 16 h 40 min.
		time.Sleep(17*time.Hour + 37*time.Minute)
		all, err := c.ListSuites(ctx)
		if err != nil {
			t.Fatal(err)
		}
		failing := 0
		for _, s := range all {
			if s.CheckFailCount > 0 {
				failing++
			}
		}
		if failing != 500 {
			t.Fatalf("after phase 1, %d suites are failing, want 500", failing)
		}

		// Phase 2: S is checked within one tick + D of every due time.
		before := len(up.Calls(sURL))
		if before == 0 {
			t.Fatal("S never checked in phase 1")
		}
		time.Sleep(3*time.Hour + 11*time.Minute)
		calls := up.Calls(sURL)[before-1:] // phase 1's last check, then phase 2's
		if n := len(calls) - 1; n < 2 {
			t.Fatalf("S checked %d times in phase 2, want at least 2", n)
		}
		for i := 1; i < len(calls); i++ {
			if gap := calls[i].Sub(calls[i-1]); gap > refresh+tick+checkDeadline {
				t.Fatalf("S waited %v between checks, bound %v", gap, refresh+tick+checkDeadline)
			}
		}

		// Phase 3: an hour of downtime, then a fresh checker on the same
		// cache. Twenty suites appear while it is down (never checked). On
		// the first tick S (healthy) goes before them and before the 500
		// (failing): persisted classes, not memory, decide the order.
		stop()
		for i := 0; i < 20; i++ {
			suite := fmt.Sprintf("/new%02d/dists/x", i)
			seedStaleAnchorAt(t, c, "a", suite)
			up.Set("http://a"+suite+"/InRelease", simtest.Stall())
		}
		time.Sleep(time.Hour + 3*time.Minute)
		runtime.Gosched()
		base := runtime.NumGoroutine()
		maxG := base
		restart := time.Now()
		_, stop2 := startSched(t, cfg)
		// Sample goroutines every 61 s (off the tick grid) through the
		// first tick after the restart, when 521 suites are due at once.
		for phase3 := tick + checkDeadline + time.Minute; time.Since(restart)+61*time.Second <= phase3; {
			time.Sleep(61 * time.Second)
			maxG = max(maxG, runtime.NumGoroutine())
		}
		time.Sleep(tick + checkDeadline + time.Minute - time.Since(restart))
		stop2()
		// Workers + Run, plus explicit slack for transient goroutines (the
		// cache writer's work): the burst is served by the pool, not by a
		// goroutine per due suite (overview DoD goroutine bound).
		const slack = 8
		if limit := base + checkWorkers + 1 + slack; maxG > limit {
			t.Fatalf("goroutines peaked at %d after the restart, limit %d", maxG, limit)
		}
		after := up.Calls(sURL)
		last := after[len(after)-1]
		if last.Before(restart) || last.Sub(restart) > tick+checkDeadline {
			t.Fatalf("after the restart S was checked at %v past the restart, bound %v", last.Sub(restart), tick+checkDeadline)
		}
	})
}

// simMembers is an AdoptionFetcher serving canned members, except that
// every fetch for a stalled host blocks until its context ends and a URL
// with a delay answers only after it. It tracks how many stalled fetches
// are in flight and, once started is set, each host's first member fetch
// (an adoption's admission). Set delays and started before the scheduler
// starts.
type simMembers struct {
	*fakeFetcher
	mu       sync.Mutex
	stalled  map[string]bool
	delay    map[string]time.Duration
	started  map[string]time.Time
	inFlight int
	maxStall int
}

func (m *simMembers) stall(host string) { m.mu.Lock(); m.stalled[host] = true; m.mu.Unlock() }

func (m *simMembers) Fetch(ctx context.Context, target *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error) {
	m.mu.Lock()
	stalled := m.stalled[target.CanonicalHost]
	d := m.delay[target.URL]
	if m.started != nil {
		if _, seen := m.started[target.CanonicalHost]; !seen {
			m.started[target.CanonicalHost] = time.Now()
		}
	}
	if stalled {
		m.inFlight++
		m.maxStall = max(m.maxStall, m.inFlight)
	}
	m.mu.Unlock()
	if !stalled {
		if d > 0 {
			tm := time.NewTimer(d)
			select {
			case <-ctx.Done():
				tm.Stop()
				return nil, ctx.Err()
			case <-tm.C:
			}
		}
		return m.fakeFetcher.Fetch(ctx, target, dst)
	}
	defer func() { m.mu.Lock(); m.inFlight--; m.mu.Unlock() }()
	<-ctx.Done()
	return nil, ctx.Err()
}

// Spec C4 DoD capacity simulation (revision 5): two slow-drip first
// adoptions, twenty signed suites changing at once (each adoption takes
// one virtual minute), one degraded suite, a pool of two. At most one
// first adoption runs at a time (the last free slot is reserved for
// signed suites); the signed suites go through the reserved slot back to
// back — each re-checked as soon as the previous one releases it, never
// probed by ticks while it waits; both drips end timed_out; the degraded
// suite's checks continue; goroutines stay bounded.
//
// AIDEV-NOTE: the signed suites' anchors answer 1 s late so the drips
// always ask for a slot first on the first tick: same-instant goroutine
// order inside a bubble is not deterministic, and without the delay about
// half the runs took a "pairs" path where no drip held a slot during the
// burst, leaving the reserved-slot claim unexercised (final review T I4).
// The 10-minute assertion pins the intended path.
func TestSimCapacity_SlowDripAndDegraded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		c := simtest.OpenCache(t)
		members := &simMembers{fakeFetcher: newFakeFetcher(), stalled: map[string]bool{}, delay: map[string]time.Duration{}}
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
			HostLimiter: hostsem.New(8), MaxConcurrent: 2, MaxDuration: 30 * time.Minute, Logger: simtest.DiscardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		// Every suite and version gets its own Packages body: a body already
		// in the pool is reused without a fetch, so the adoption would take
		// no time (or a drip would not drip).
		pkgsFor := func(label string) []byte {
			return fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_" + label + ".deb": testHex64})
		}
		rel := func(label string, pkgs []byte) []byte {
			return datedRelease(label, 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})
		}
		pkgsURL := func(host string) string { return "http://" + host + rigSuite + "/main/binary-amd64/Packages" }
		anchorURL := func(host string) string { return "http://" + host + rigSuite + "/InRelease" }
		up := simtest.NewUpstream()
		var normal []string
		v2 := map[string][]byte{}
		for i := 0; i < 20; i++ {
			host := fmt.Sprintf("n%02d", i)
			members.put(pkgsURL(host), pkgsFor("1-"+host))
			ref := SuiteRef{CanonicalScheme: "http", CanonicalHost: host, SuitePath: rigSuite}
			trackSuite(t, c, ref)
			if err := ad.Run(ctx, ref, rel("v1-"+host, pkgsFor("1-"+host)), "", ""); err != nil {
				t.Fatal(err)
			}
			members.put(pkgsURL(host), pkgsFor("2-"+host))
			members.delay[pkgsURL(host)] = time.Minute
			v2[host] = rel("v2-"+host, pkgsFor("2-"+host))
			up.Set(anchorURL(host), simtest.After(time.Second, simtest.Serve(v2[host])))
			normal = append(normal, host)
		}
		for _, host := range []string{"drip1", "drip2"} {
			seedStaleAnchorAt(t, c, host, rigSuite)
			members.put(pkgsURL(host), pkgsFor(host))
			members.stall(host)
			up.Set(anchorURL(host), simtest.Serve(rel("drip-"+host, pkgsFor(host))))
		}
		seedStaleAnchorAt(t, c, "deg", rigSuite)
		up.Set(anchorURL("deg"), simtest.Stall())
		members.started = map[string]time.Time{}

		runtime.Gosched()
		base := runtime.NumGoroutine()
		maxG := base
		t0 := time.Now()
		const refresh = 15 * time.Minute
		tick := refresh / 4
		_, stop := startSched(t, Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: refresh,
			Cooldown: time.Minute, Adopter: ad, Logger: simtest.DiscardLogger(), rand: func() float64 { return 0.5 }})
		// sample advances to until (measured from t0), sampling goroutines
		// every 61 s: off the 3 m 45 s tick grid.
		sample := func(until time.Duration) {
			for time.Since(t0)+61*time.Second <= until {
				time.Sleep(61 * time.Second)
				maxG = max(maxG, runtime.NumGoroutine())
			}
			time.Sleep(until - time.Since(t0))
		}

		// (0) Mid-burst, exactly one first adoption holds the general slot
		// while the signed suites take turns in the reserved one, and the
		// waiting suites are counted by the gauge.
		sample(10 * time.Minute)
		members.mu.Lock()
		inFlight := members.inFlight
		members.mu.Unlock()
		if inFlight != 1 {
			t.Fatalf("%d first adoptions running at 10 m, want exactly 1 (the burst must run beside one)", inFlight)
		}
		if g := renderedMetric("acu_freshness_waiting_for_slot"); g == "" || g == "0" {
			t.Fatalf("acu_freshness_waiting_for_slot = %q at 10 m, want > 0 while suites wait", g)
		}

		// (1) All twenty adopt v2 within one tick + 20 one-minute adoptions
		// + D (≈ 25 m 45 s; re-offering only on ticks would need ≈ 75 min),
		// plus the 20 × 1 s their anchors take to answer.
		bound := tick + 20*time.Minute + checkDeadline + 20*time.Second
		sample(bound)
		for _, host := range normal {
			sf, err := c.GetSuiteFreshness(ctx, "http", host, rigSuite)
			if err != nil || sf.CurrentSnapshotID == nil {
				t.Fatalf("%s: no current snapshot (err %v)", host, err)
			}
			snap, err := c.GetSuiteSnapshot(ctx, *sf.CurrentSnapshotID)
			if err != nil || snap.SingleAnchorHash == nil || *snap.SingleAnchorHash != hashOf(v2[host]) {
				t.Fatalf("%s had not adopted v2 %v after the start (bound: one tick + 20 × 1 min + D)", host, bound)
			}
		}
		sample(2*time.Hour + 7*time.Minute) // off the tick grid
		if g := renderedMetric("acu_freshness_waiting_for_slot"); g != "0" {
			t.Fatalf("acu_freshness_waiting_for_slot = %q at the end, want 0", g)
		}
		stop()

		members.mu.Lock()
		maxStall := members.maxStall
		started := maps.Clone(members.started)
		members.mu.Unlock()
		// (2) The reserved slot: one first adoption at a time.
		if maxStall != 1 {
			t.Fatalf("%d first adoptions ran at once, want 1 (the last free slot is reserved for signed suites)", maxStall)
		}
		for _, host := range []string{"drip1", "drip2"} {
			sf, err := c.GetSuiteFreshness(ctx, "http", host, rigSuite)
			if err != nil || strOf(sf.AdoptionFailReason) != "timed_out" {
				t.Fatalf("%s record reason = %q (err %v), want timed_out", host, strOf(sf.AdoptionFailReason), err)
			}
		}
		// (3) No probing while waiting: the first tick's probe and the
		// offer, nothing in between.
		var starts []time.Time
		for _, host := range normal {
			at, ok := started[host]
			if !ok {
				t.Fatalf("%s: its adoption never fetched a member", host)
			}
			starts = append(starts, at)
			n := 0
			for _, call := range up.Calls(anchorURL(host)) {
				if !call.After(at) {
					n++
				}
			}
			if n > 2 {
				t.Errorf("%s: %d anchor GETs before its adoption started, want <= 2", host, n)
			}
		}
		// (4) No slot idles while signed suites wait: consecutive
		// admissions are at most one adoption + D apart.
		slices.SortFunc(starts, time.Time.Compare)
		for i := 1; i < len(starts); i++ {
			if gap := starts[i].Sub(starts[i-1]); gap > time.Minute+checkDeadline {
				t.Errorf("admissions %d and %d were %v apart, want <= %v (a slot sat idle while suites waited)",
					i-1, i, gap, time.Minute+checkDeadline)
			}
		}
		// (5) The degraded suite keeps being checked (its backoff grows).
		if n := len(up.Calls(anchorURL("deg"))); n < 4 {
			t.Fatalf("degraded suite checked %d times in 2 h, want its checks to continue (>= 4)", n)
		}
		// (6) Workers + Run + two adoptions, plus explicit slack for
		// transient goroutines (the cache writer's work, timer callbacks).
		const slack = 8
		if limit := base + checkWorkers + 1 + 2 + slack; maxG > limit {
			t.Fatalf("goroutines peaked at %d, limit %d", maxG, limit)
		}
	})
}

// Phase A carry-forward: the burst after a forward or backward clock
// step (every suite due at once) stays within the pool's bounds and
// every suite is still checked.
func TestSimScheduler_PacingAfterClockSteps(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := simtest.OpenCache(t)
		up := simtest.NewUpstream()
		now := time.Now().Unix()
		var urls []string
		for h := 0; h < 6; h++ {
			for k := 0; k < 4; k++ {
				host, suite := fmt.Sprintf("h%d", h), fmt.Sprintf("/s%d/dists/x", k)
				seedSimSuite(t, c, host, suite, now, 0)
				url := "http://" + host + suite + "/InRelease"
				up.Set(url, simtest.After(30*time.Second, simtest.NotModified()))
				urls = append(urls, url)
			}
		}
		_, stop := startSched(t, Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute, Logger: simtest.DiscardLogger()})
		defer stop()
		phase := func(name string, offset time.Duration) {
			restore := wallclock.SetOffsetForTest(offset)
			defer restore()
			up.ResetMax()
			counts := make([]int, len(urls))
			for i, u := range urls {
				counts[i] = len(up.Calls(u))
			}
			time.Sleep(time.Hour + 37*time.Minute)
			if total, perHost := up.MaxInFlight(); total > checkWorkers || perHost > 1 {
				t.Errorf("%s: %d checks in flight overall, %d on one host; want <= %d and <= 1", name, total, perHost, checkWorkers)
			}
			for i, u := range urls {
				if len(up.Calls(u)) == counts[i] {
					t.Errorf("%s: %s was not checked", name, u)
				}
			}
		}
		phase("baseline", 0)
		phase("forward +30d", 30*24*time.Hour)
		phase("back to 0 (-30d step)", 0)
		phase("backward -2h", -2*time.Hour)
	})
}
