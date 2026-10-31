package freshness

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
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
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
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
		BlobHash: &bh, UpstreamURL: "http://" + host + suite + "/InRelease", IsMetadata: true, RequestCount: 1}); err != nil {
		t.Fatal(err)
	}
	if err := c.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: host, SuitePath: suite,
		LastCheckAt: &lastAt, LastSuccessAt: &lastAt, CheckFailCount: failCount}); err != nil {
		t.Fatal(err)
	}
}

// seedSimSuites is seedSimSuite, fail count 0, for many suites of host:
// one transaction on the cache's database, where seedSimSuite takes three
// writer calls a suite (deferred minor C-38: seeding 501 suites that way
// took 2.2 s of the -race budget). One stamp differs: each suite's
// last_requested_at is lastAt, where seedSimSuite's is the instant it
// seeds (createSuiteTx stamps now).
//
// AIDEV-NOTE: a second, test-only connection that bypasses the cache's
// writer and has none of its pragmas (busy_timeout, foreign_keys): call it
// before the scheduler starts, while nothing else writes the cache, or
// either side's write can fail with SQLITE_BUSY.
func seedSimSuites(t *testing.T, c *cache.Cache, host string, suites []string, lastAt int64) {
	t.Helper()
	if err := c.PutBlob(context.Background(), testHex64, 1); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(c.Dir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, suite := range suites {
		if _, err := tx.Exec(`INSERT INTO url_path (canonical_scheme, canonical_host, path, blob_hash, upstream_url, is_metadata, request_count)
 VALUES ('http', ?, ?, ?, ?, 1, 1)`, host, suite+"/InRelease", testHex64, "http://"+host+suite+"/InRelease"); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO suite_freshness (canonical_scheme, canonical_host, suite_path, last_check_at, last_success_at, last_requested_at)
 VALUES ('http', ?, ?, ?, ?, ?)`, host, suite, lastAt, lastAt, lastAt); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
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
		relR := signedRelease("r", 0, map[string][]byte{"main/binary-amd64/Packages": pkgsR, "Contents-amd64.gz": good})
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
		members.setDelay(rBase+"Contents-amd64.gz", 10*time.Minute)
		members.setDelay(rByHash, 10*time.Minute)
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
				return signedRelease(fmt.Sprintf("c-%s-%d", host, n), 0, map[string][]byte{"main/binary-amd64/Packages": pkgs(n)})
			}
			members.put(pkgsURL, pkgs(0))
			trackSuite(t, c, SuiteRef{CanonicalScheme: "http", CanonicalHost: host, SuitePath: cSuite})
			if err := ad.Run(ctx, SuiteRef{CanonicalScheme: "http", CanonicalHost: host, SuitePath: cSuite}, rel(0), "", ""); err != nil {
				t.Fatal(err)
			}
			due(host, cSuite)
			members.setDelay(pkgsURL, 10*time.Minute)
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
		// Workers, Run, two adoption slots, and slack for timers and the
		// cache writer's transient goroutines (8, as in the capacity
		// simulation: deferred minor B-47).
		baseG := baseGoroutines()
		limit := baseG + checkWorkers + 1 + 2 + 8
		maxG := baseG
		const refresh = time.Hour
		chk, stop := startSched(t, Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: refresh,
			Adopter: ad, Logger: slog.New(slog.NewJSONHandler(logs, nil)),
			rand: func() float64 { return 0.5 }}) // spec §7 fixed seeds (deferred minor B-48)
		// sample advances to until (measured from t0), sampling goroutines
		// every 61 s: off the 15-minute tick grid.
		sample := func(until time.Duration) {
			for time.Since(t0)+61*time.Second <= until {
				time.Sleep(61 * time.Second)
				maxG = max(maxG, sampleGoroutines(limit))
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
		// The goroutine bound (limit, above).
		if maxG > limit {
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
		suites := make([]string, 500)
		for i := range suites {
			suites[i] = fmt.Sprintf("/s%03d/dists/x", i)
			up.Set("http://a"+suites[i]+"/InRelease", simtest.Unavailable())
		}
		const sPath = "/zzz/dists/x"
		sURL := "http://a" + sPath + "/InRelease"
		seedSimSuites(t, c, "a", append(slices.Clone(suites), sPath), stale)
		up.Set(sURL, simtest.NotModified())
		const refresh = time.Hour
		tick := refresh / 4
		cfg := Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: refresh, Logger: simtest.DiscardLogger()}
		_, stop := startSched(t, cfg)

		// Phase 1: every one of the 500 fails once while still in the
		// healthy class (spec C4: a first failure is paid once). Each
		// answers its first check 503, an unknown end that takes no time,
		// so the first tick fails all 500; from then on they stall
		// (deferred minor C-38: 500 stalling first checks took 16 h 40 min
		// of virtual time, and their ticks most of the -race budget).
		time.Sleep(tick + time.Minute)
		for _, suite := range suites {
			up.Set("http://a"+suite+"/InRelease", simtest.Stall())
		}
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
		// Workers + Run, plus explicit slack for transient goroutines (the
		// cache writer's work): the burst is served by the pool, not by a
		// goroutine per due suite (overview DoD goroutine bound).
		const slack = 8
		base := baseGoroutines()
		limit := base + checkWorkers + 1 + slack
		maxG := base
		restart := time.Now()
		_, stop2 := startSched(t, cfg)
		// Sample goroutines every 61 s (off the tick grid) through the
		// first tick after the restart, when 521 suites are due at once.
		for phase3 := tick + checkDeadline + time.Minute; time.Since(restart)+61*time.Second <= phase3; {
			time.Sleep(61 * time.Second)
			maxG = max(maxG, sampleGoroutines(limit))
		}
		time.Sleep(tick + checkDeadline + time.Minute - time.Since(restart))
		stop2()
		if maxG > limit {
			t.Fatalf("goroutines peaked at %d after the restart, limit %d", maxG, limit)
		}
		after := up.Calls(sURL)
		last := after[len(after)-1]
		if last.Before(restart) || last.Sub(restart) > tick+checkDeadline {
			t.Fatalf("after the restart S was checked at %v past the restart, bound %v", last.Sub(restart), tick+checkDeadline)
		}
	})
}

// Deferred minor B-53 (phase B DoD): the stalling-host bound with an
// adopter wired and adoptions contending on the stalling host, at a fifth
// of the 500-suite simulation's size (that one, without an adopter, fits
// the -race budget only coarsened: deferred minor C-38).
//
// The contention is real (review fix round 1, Task 14): one host limiter
// is shared by the checker and the adopter, as in production
// (cmd/apt-cacher-ultra/main.go, at its default 8 per host), and host a
// carries, besides the 100 stalling suites and the healthy S, two changing
// candidates C and C2 — each publishes a new InRelease on every request and
// each adoption fetches its Packages for 10 minutes — and R, whose Contents
// member never heals, so every unchanged check of R asks for a 20-minute
// repair. One adoption or repair holds a host's admission slot at a time.
// At the first tick (key order: C, R, C2) C's adoption takes the slot, R's
// repair is deferred and C2 ends changed_queued and is parked; C2 is
// re-offered when the slot is released, while the stalling checks hold
// host a's turns ahead of S.
//
// Asserted: some candidate ended changed_queued, a repair was deferred, and
// C2 adopted new content (the parked re-offer path); once the 100 stalling
// suites have failed, the healthy S is checked within refresh + one tick +
// D every time; the candidates keep adopting; goroutines stay bounded.
func TestSimScheduler_StallingHostWithAdoptionContention(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		c := simtest.OpenCache(t)
		up := simtest.NewUpstream()
		members := &simMembers{fakeFetcher: newFakeFetcher(), stalled: map[string]bool{}, delay: map[string]time.Duration{}}
		lim := hostsem.New(8) // shared by the checker and the adopter, as in production
		starts := &startsByURL{AdoptionFetcher: members, at: map[string]time.Time{}}
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: starts, Verifier: passThroughVerifier{},
			HostLimiter: lim, MaxConcurrent: 2, MaxDuration: 30 * time.Minute, RepairSkippedMembers: true,
			TolerateOptionalMemberFailures: true, Logger: simtest.DiscardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		stale := time.Now().Add(-2 * time.Hour).Unix()
		for i := 0; i < 100; i++ {
			suite := fmt.Sprintf("/s%03d/dists/x", i)
			seedSimSuite(t, c, "a", suite, stale, 0)
			up.Set("http://a"+suite+"/InRelease", simtest.Stall())
		}
		adopt := func(suite string, pkgs []byte) []byte {
			t.Helper()
			ref := SuiteRef{CanonicalScheme: "http", CanonicalHost: "a", SuitePath: suite}
			members.put("http://a"+suite+"/main/binary-amd64/Packages", pkgs)
			trackSuite(t, c, ref)
			rel := signedRelease("v0"+suite, 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})
			if err := ad.Run(ctx, ref, rel, "", ""); err != nil {
				t.Fatal(err)
			}
			return rel
		}
		const sPath, cPath, rPath, c2Path = "/zzz/dists/x", "/ccc/dists/x", "/ddd/dists/x", "/eee/dists/x"
		sURL := "http://a" + sPath + "/InRelease"
		relS := adopt(sPath, fakePackagesStanzas(map[string]string{"pool/s_1.deb": testHex64}))
		// S's upstream answers its unchanged InRelease: 200 with the same
		// bytes to the first GET (S was adopted without validators), 304 to
		// the conditional ones after it. A bare 304 to a GET without
		// validators is a failed check (final fix wave FW-3 (c)), which
		// would make S a failing suite.
		up.Set(sURL, simtest.Serve(relS))
		// changing adopts suite and makes upstream publish a new InRelease,
		// with a new Packages body fetched in 10 minutes, on every request
		// (a body already in the pool would be reused without a fetch).
		changing := func(suite string) (seeded string) {
			pkgs := func(n int64) []byte {
				return fakePackagesStanzas(map[string]string{fmt.Sprintf("pool/c%s_%d.deb", suite[1:4], n): testHex64})
			}
			rel := func(n int64) []byte {
				return signedRelease(fmt.Sprintf("c%s%d", suite, n), 0, map[string][]byte{"main/binary-amd64/Packages": pkgs(n)})
			}
			ref := SuiteRef{CanonicalScheme: "http", CanonicalHost: "a", SuitePath: suite}
			pkgsURL := "http://a" + suite + "/main/binary-amd64/Packages"
			members.put(pkgsURL, pkgs(0))
			trackSuite(t, c, ref)
			if err := ad.Run(ctx, ref, rel(0), "", ""); err != nil {
				t.Fatal(err)
			}
			members.setDelay(pkgsURL, 10*time.Minute)
			var n atomic.Int64
			up.Set("http://a"+suite+"/InRelease", func(ctx context.Context, etag string) (*fetch.ConditionalResult, error) {
				k := n.Add(1)
				members.put(pkgsURL, pkgs(k)) // before the check can spawn the adoption
				return simtest.Serve(rel(k))(ctx, etag)
			})
			return hashOf(rel(0))
		}
		changing(cPath)
		c2Seeded := changing(c2Path)
		// R: adopted, with a Contents member that never heals, so every
		// unchanged check of R asks for a repair, which probes by-hash, then
		// the canonical path, 10 minutes each.
		good, bad := []byte(strings.Repeat("C", 100)), []byte(strings.Repeat("z", 90))
		pkgsR := fakePackagesStanzas(map[string]string{"pool/r_1.deb": testHex64})
		relR := signedRelease("r", 0, map[string][]byte{"main/binary-amd64/Packages": pkgsR, "Contents-amd64.gz": good})
		rBase := "http://a" + rPath + "/"
		rByHash := rBase + "by-hash/SHA256/" + shaOf(good)
		members.put(rBase+"main/binary-amd64/Packages", pkgsR)
		members.put(rBase+"Contents-amd64.gz", bad)
		members.put(rByHash, bad)
		rRef := SuiteRef{CanonicalScheme: "http", CanonicalHost: "a", SuitePath: rPath}
		trackSuite(t, c, rRef)
		if err := ad.Run(ctx, rRef, relR, "", ""); err != nil {
			t.Fatal(err)
		}
		members.setDelay(rBase+"Contents-amd64.gz", 10*time.Minute)
		members.setDelay(rByHash, 10*time.Minute)
		up.Set(rBase+"InRelease", simtest.Serve(relR))
		cAdoptions := func() float64 { // deferred minor E-51: a value that does not parse fails the test
			return renderedFloat(t, renderedMetricLabels("acu_adoption_total", `outcome="success"`, `host="a"`))
		}
		startAdoptions := cAdoptions()
		starts.mu.Lock()
		starts.at = map[string]time.Time{} // the adoptions above do not count
		starts.mu.Unlock()
		startQueued := checkTotal(t, "changed_queued", "a")
		startDeferred := hostCounter(t, "acu_repair_deferred_total", "a")
		const refresh = time.Hour
		tick := refresh / 4
		const slack = 8
		base := baseGoroutines()
		limit := base + checkWorkers + 1 + 2 + slack
		maxG := base
		// offerHook (on the Run goroutine): offers that admitted C2, the
		// parked re-offer path (review fix round 2, Task 14). Only an
		// admitting offer counts — result changed, C2's adoption spawned —
		// not a busy, skipped or unchanged one (final fix wave FW-6, the
		// Task 14 round-2 re-review minor).
		var c2Offered atomic.Int32
		_, stop := startSchedHooked(t, Config{Cache: c, Fetcher: up, HostLimiter: lim, Refresh: refresh,
			Adopter: ad, Logger: simtest.DiscardLogger(), rand: func() float64 { return 0.5 }},
			func(ref SuiteRef, rep checkReport) {
				if ref.SuitePath == c2Path && rep.result == "changed" {
					c2Offered.Add(1)
				}
			})
		// Phase 1: every stalling suite fails once (100 × D on one host).
		// AIDEV-NOTE: the loop has a virtual deadline (final fix wave FW-4
		// (e)): 100 × D, the stalling host's checks, plus a refresh interval
		// for the ticks and the other suites' turns. Without it a regression
		// that keeps the count from reaching 100 (one more failing suite, or
		// one stalling suite never checked) hung the package until go
		// test's 10-minute timeout, as FW-3 (c)'s first draft did here.
		phase1Deadline := time.Now().Add(100*checkDeadline + refresh)
		for time.Sleep(61 * time.Second); ; time.Sleep(61 * time.Second) {
			maxG = max(maxG, sampleGoroutines(limit))
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
			if failing == 100 {
				break
			}
			if time.Now().After(phase1Deadline) {
				t.Fatalf("phase 1: %d suites failing %v after the start, want the 100 stalling ones by %v",
					failing, time.Since(phase1Deadline.Add(-(100*checkDeadline + refresh))), 100*checkDeadline+refresh)
			}
		}
		before := len(up.Calls(sURL))
		if before == 0 {
			t.Fatal("S never checked in phase 1")
		}
		// Phase 2: three hours of contention.
		for end := time.Now().Add(3*time.Hour + 11*time.Minute); time.Now().Before(end); time.Sleep(61 * time.Second) {
			maxG = max(maxG, sampleGoroutines(limit))
		}
		stop()
		calls := up.Calls(sURL)[before-1:]
		if len(calls) < 3 {
			t.Fatalf("S checked %d times in phase 2, want at least 2", len(calls)-1)
		}
		for i := 1; i < len(calls); i++ {
			if gap := calls[i].Sub(calls[i-1]); gap > refresh+tick+checkDeadline {
				t.Fatalf("S waited %v between checks, bound %v", gap, refresh+tick+checkDeadline)
			}
		}
		if got := cAdoptions() - startAdoptions; got < 3 {
			t.Fatalf("C and C2 adopted %v times, want them to keep adopting (>= 3)", got)
		}
		t.Logf("phase 1 and 2: %v changed_queued checks, %v deferred repairs, %v adoptions on host a",
			checkTotal(t, "changed_queued", "a")-startQueued, hostCounter(t, "acu_repair_deferred_total", "a")-startDeferred,
			cAdoptions()-startAdoptions)
		// The contention was real: a candidate found host a's admission
		// slot taken and was queued, a repair was deferred, and the parked
		// C2 was re-offered and adopted new content.
		if got := checkTotal(t, "changed_queued", "a") - startQueued; got < 1 {
			t.Fatalf("%v checks on host a ended changed_queued, want at least 1: the slots were never contended", got)
		}
		if got := hostCounter(t, "acu_repair_deferred_total", "a") - startDeferred; got < 1 {
			t.Fatalf("%v of R's repairs were deferred, want at least 1", got)
		}
		sf, err := c.GetSuiteFreshness(ctx, "http", "a", c2Path)
		if err != nil || sf.CurrentSnapshotID == nil {
			t.Fatalf("C2: no current snapshot (err %v)", err)
		}
		if snap, err := c.GetSuiteSnapshot(ctx, *sf.CurrentSnapshotID); err != nil || strOf(snap.SingleAnchorHash) == c2Seeded {
			t.Fatalf("C2 never adopted new content (err %v): its parked re-offer never admitted it", err)
		}
		// C2 was parked by the first tick's check and admitted by the
		// re-offer when C's adoption released host a's slot: within that
		// adoption (10 minutes) and one host turn behind a stalling check
		// (D), not at a later tick.
		c2Calls := up.Calls("http://a" + c2Path + "/InRelease")
		starts.mu.Lock()
		c2Start, ok := starts.at["http://a"+c2Path+"/main/binary-amd64/Packages"]
		starts.mu.Unlock()
		if len(c2Calls) == 0 || !ok {
			t.Fatalf("C2: %d checks, adoption started %v", len(c2Calls), ok)
		}
		if wait := c2Start.Sub(c2Calls[0]); wait > 10*time.Minute+checkDeadline {
			t.Fatalf("C2 parked at %v was admitted %v later, want within one adoption + D (%v)",
				c2Calls[0], wait, 10*time.Minute+checkDeadline)
		}
		// …and it was parked: an offer (a waiting suite's slot-driven
		// re-check) admitted it, in every goroutine order (under -race a
		// suite dropped instead of parked could still be admitted in time
		// by a later check).
		if n := c2Offered.Load(); n < 1 {
			t.Fatalf("no offer admitted C2 (%d): it was never parked and re-offered", n)
		}
		if maxG > limit {
			t.Fatalf("goroutines peaked at %d, limit %d", maxG, limit)
		}
	})
}

// baseGoroutines is a simulation's goroutine baseline, read after
// synctest.Wait: every other goroutine of the bubble is durably blocked
// (final fix wave FW-4 (b)).
func baseGoroutines() int {
	synctest.Wait()
	return runtime.NumGoroutine()
}

// sampleGoroutines is one goroutine sample of a simulation bounded by
// limit: runtime.NumGoroutine read after synctest.Wait, and, when that
// read is over limit, read once more after the world was stopped
// (runtime.ReadMemStats) and another Wait.
//
// AIDEV-NOTE: final fix wave FW-4 (b), superseding the ledger's "baseline
// delta" ruling (the limits were already relative to a baseline).
// NumGoroutine is a process-wide, lock-free sum: sampled while the
// bubble's goroutines still did real work — an adoption in a syscall,
// database/sql's per-query awaitDone and interruptOnDone goroutines
// starting and exiting — it read 22–49 where a goroutine profile taken at
// once showed 12–15, all expected (9 failures in 36 runs under CPU load).
// Waiting first let them settle (0 in 60). What is left is a goroutine
// that has just exited — a bubble goroutine (a restart ends several:
// Reaper.Run, GC.Run, the cache's writer, database/sql's
// connectionOpener), whose exit takes it out of the bubble's count, so
// Wait returns, while the runtime has not yet put it on a free list
// (gdestroy's gfput), so NumGoroutine still counts it, the longer while
// its thread is descheduled under load (the final fix wave's re-review,
// gc-sims minor 5, from the runtime's source). Phase F's final review
// (tests I-1) caught 6 such reads in 64 restart-simulation runs (load1
// about 20), each a dump of exactly the expected goroutines taken right
// after the second read of 9 against 8. A re-read after a bare second
// Wait, microseconds later, did not absorb it; one after a stop-the-world
// does, since the stop cannot complete until the exiting goroutine's
// thread reaches the scheduler, after gfput (the reviewer's overlay: 0
// failures in 64 runs, 4 transients absorbed, and 0 in 108 with the stop
// between the reads). A real leak stays over the limit on both reads.
func sampleGoroutines(limit int) int {
	synctest.Wait()
	n := runtime.NumGoroutine()
	if n > limit {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms) // stops the world: an exited goroutine's count settles
		synctest.Wait()
		n = runtime.NumGoroutine()
	}
	return n
}

// renderedMetricLabels returns the value of the series name whose labels
// include every one of labels, as metrics.Default renders it ("" if none).
func renderedMetricLabels(name string, labels ...string) string {
	var buf bytes.Buffer
	metrics.Default.Render(&buf)
	for _, line := range strings.Split(buf.String(), "\n") {
		if !strings.HasPrefix(line, name+"{") {
			continue
		}
		ok := true
		for _, l := range labels {
			ok = ok && strings.Contains(line, l)
		}
		if ok {
			return line[strings.LastIndexByte(line, ' ')+1:]
		}
	}
	return ""
}

// simMembers is an AdoptionFetcher serving canned members, except that
// every fetch for a stalled host blocks until its context ends and a URL
// with a delay answers only after it. It tracks how many stalled fetches
// are in flight and, once recordStarts was called, each host's first
// member fetch (an adoption's admission). Its maps are written only
// under its lock: by stall, setDelay, recordStarts, and Fetch's record of
// each host's first start (deferred minor B-51).
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

// setDelay makes every fetch of url answer only after d.
func (m *simMembers) setDelay(url string, d time.Duration) {
	m.mu.Lock()
	m.delay[url] = d
	m.mu.Unlock()
}

// recordStarts starts recording each host's first member fetch.
func (m *simMembers) recordStarts() { m.mu.Lock(); m.started = map[string]time.Time{}; m.mu.Unlock() }

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
			return signedRelease(label, 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})
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
			members.setDelay(pkgsURL(host), time.Minute)
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
		members.recordStarts()

		// (6) Workers + Run + two adoptions, plus explicit slack for
		// transient goroutines (the cache writer's work, timer callbacks).
		const slack = 8
		base := baseGoroutines()
		limit := base + checkWorkers + 1 + 2 + slack
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
				maxG = max(maxG, sampleGoroutines(limit))
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
		// (6) The goroutine bound (limit, above).
		if maxG > limit {
			t.Fatalf("goroutines peaked at %d, limit %d", maxG, limit)
		}
	})
}

// Phase A carry-forward: the burst after a forward or backward clock
// step (every suite due at once) stays within the pool's bounds and
// every suite is still checked. The steps: +30 d, -30 d, and spec §7's
// -2 h and +2 h (deferred minor B-49 added the +2 h step).
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
		phase("forward +2h (back to 0)", 0)
	})
}
