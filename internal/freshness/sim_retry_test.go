package freshness

import (
	"context"
	"fmt"
	mrand "math/rand/v2"
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

// retrySim runs the real scheduler, checker, adopter and cache for suite
// /r/dists/x on host h inside a synctest bubble.
type retrySim struct {
	c    *cache.Cache
	up   *simtest.Upstream
	ver  *rigVerifier
	pkgs []byte
	stop func()
}

func startRetrySim(t *testing.T, refresh time.Duration) *retrySim {
	t.Helper()
	return startRetrySimRand(t, refresh, func() float64 { return 0.5 })
}

// startRetrySimRand is startRetrySim with the scheduler's random seam rnd
// (the retry jitter: 0.5 is none).
func startRetrySimRand(t *testing.T, refresh time.Duration, rnd func() float64) *retrySim {
	t.Helper()
	c := simtest.OpenCache(t)
	seedStaleAnchorAt(t, c, rigHost, rigSuite)
	s := &retrySim{c: c, up: simtest.NewUpstream(),
		ver:  &rigVerifier{rejected: map[string]bool{}, panics: map[string]bool{}, entered: make(chan struct{}, 1)},
		pkgs: fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": testHex64})}
	members := newFakeFetcher()
	members.put(rigPkgsURL, s.pkgs)
	ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: s.ver, HostLimiter: hostsem.New(8), MaxConcurrent: 2,
		Logger: simtest.DiscardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	_, s.stop = startSched(t, Config{Cache: c, Fetcher: s.up, HostLimiter: hostsem.New(8), Refresh: refresh,
		Cooldown: time.Minute, Adopter: ad, Logger: simtest.DiscardLogger(), rand: rnd})
	return s
}

func (s *retrySim) release(label string, date int64) []byte {
	return signedRelease(label, date, map[string][]byte{"main/binary-amd64/Packages": s.pkgs})
}

func (s *retrySim) serve(body []byte) { s.up.Set(rigAnchorURL, simtest.Serve(body)) }

// touch records a client's request for the suite (its interest stamp).
func (s *retrySim) touch(t *testing.T) {
	t.Helper()
	if err := s.c.TouchSuite(context.Background(), cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite}); err != nil {
		t.Fatal(err)
	}
}

func (s *retrySim) row(t *testing.T) cache.SuiteFreshness {
	t.Helper()
	r, err := s.c.GetSuiteFreshness(context.Background(), "http", rigHost, rigSuite)
	if err != nil {
		t.Fatal(err)
	}
	return *r
}

// current returns the hash of the current snapshot's anchor, or "".
func (s *retrySim) current(t *testing.T) string {
	t.Helper()
	r := s.row(t)
	if r.CurrentSnapshotID == nil {
		return ""
	}
	snap, err := s.c.GetSuiteSnapshot(context.Background(), *r.CurrentSnapshotID)
	if err != nil || snap.SingleAnchorHash == nil {
		t.Fatalf("current snapshot unreadable: %v", err)
	}
	return *snap.SingleAnchorHash
}

// PR #8 limitation #1 reproduced under the real scheduler: an untrusted
// InRelease costs about 30 attempts a day (main: about 1,440 at the
// default 60 s cooldown), while checks keep running every refresh.
func TestSimLim1_DeterministicFailureAttemptsPerDay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := startRetrySim(t, 15*time.Minute)
		bad := s.release("untrusted", 0)
		s.ver.reject(hashOf(bad))
		s.serve(bad)
		time.Sleep(24*time.Hour + 7*time.Minute)
		s.stop()
		if a := s.ver.calls(); a < 18 || a > 30 {
			t.Fatalf("attempts in a day = %d, want 18..30", a)
		}
		// About one check per refresh (96 a day) plus the retries: a
		// deferred check must count as a success, or the suite would be
		// due every tick (4× the checks; final review T M6).
		if n := len(s.up.Calls(rigAnchorURL)); n < 90 || n > 110 {
			t.Fatalf("checks in a day = %d, want 90..110 (liveness, and no check every tick)", n)
		}
	})
}

// Deferred minor B-54: limitation #1's attempt count at both ends of the
// ±25 % retry jitter — more attempts with the shortest delays, fewer with
// the longest, both far below main's 1,440 a day.
func TestSimLim1_AttemptsAcrossTheJitter(t *testing.T) {
	attempts := map[string]int{}
	for _, leg := range []struct {
		name     string
		u        float64
		min, max int
	}{{"shortest", 0, 28, 40}, {"longest", 0.999, 16, 26}} {
		synctest.Test(t, func(t *testing.T) {
			s := startRetrySimRand(t, 15*time.Minute, func() float64 { return leg.u })
			bad := s.release("untrusted", 0)
			s.ver.reject(hashOf(bad))
			s.serve(bad)
			time.Sleep(24*time.Hour + 7*time.Minute)
			s.stop()
			attempts[leg.name] = s.ver.calls()
			if a := s.ver.calls(); a < leg.min || a > leg.max {
				t.Fatalf("%s jitter: %d attempts in a day, want %d..%d", leg.name, a, leg.min, leg.max)
			}
		})
	}
	if attempts["shortest"] <= attempts["longest"] {
		t.Fatalf("attempts %v: the shortest delays must give more attempts", attempts)
	}
}

// Spec C7/C5 multi-week simulation, about 20 virtual days with per-day
// bounds: an untrusted body (bounded attempts, 10 days), new content
// (adopted within a refresh interval), an upstream rollback to older-dated
// content (never adopted, bounded refusals, record cleared when upstream
// recovers; 5 days), and backward and forward clock steps (new content
// still adopted, checks continue; 2 days each).
//
// AIDEV-NOTE: the name states no day count (ruling PE-9: this was
// TestSim30Days_RetryDateRulesAndClockSteps until phase E's review fix
// round 1). The run spans about 20 virtual days (27 before phase E). The
// untrusted phase keeps its 10 days: the 7-day interest window lapses
// inside it, so its floor catches a suite gone dormant without the day-5
// touch (B-57). The older-content phase runs 5 days and each clock step 2,
// every bound at the same per-day rate, so the run stays well under the
// 5 s -race budget (executability review D6, ruling PE-8: about 3.6–4 s).
// Re-time it under -race before lengthening any phase.
func TestSimRetry_LongRunDateRulesAndClockSteps(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// The offset is process-global: a Fatal between a clock step and
		// its inline restore() must not leak the step into later tests.
		t.Cleanup(wallclock.SetOffsetForTest(0))
		// 2 h 40 m keeps the run well under 5 s under -race while one tick
		// (40 m) stays below the 1 h retry cap, so the attempt bounds keep
		// their teeth; every phase boundary below stays off the tick grid.
		const refresh = 160 * time.Minute
		const day = 24 * time.Hour
		window := refresh + refresh/4 + 7*time.Minute // one refresh + one tick, off the tick grid
		s := startRetrySim(t, refresh)
		t0 := time.Now()

		bad := s.release("untrusted", t0.Unix())
		s.ver.reject(hashOf(bad))
		s.serve(bad)
		time.Sleep(5 * day)
		s.touch(t) // a client asks again: the suite stays interesting
		time.Sleep(5*day + 7*time.Minute)
		if a := s.ver.calls(); a < 10*15 || a > 10*30 { // deferred minor B-57: a floor too
			t.Fatalf("untrusted body: %d attempts in 10 days, want 150..300", a)
		}
		if s.current(t) != "" {
			t.Fatal("untrusted content was adopted")
		}

		v2 := s.release("v2", time.Now().Unix())
		s.touch(t) // apt update: a client asks for the suite again
		s.serve(v2)
		time.Sleep(window)
		if s.current(t) != hashOf(v2) {
			t.Fatal("new content not adopted within one refresh interval")
		}

		older := s.release("v1-older", t0.Add(day).Unix())
		s.serve(older)
		before := s.ver.calls()
		time.Sleep(5*day + 11*time.Minute)
		if s.current(t) != hashOf(v2) {
			t.Fatal("the suite moved back to older-dated content")
		}
		if a := s.ver.calls() - before; a < 3 || a > 5*30 {
			t.Fatalf("older content cost %d verifications in 5 days, want 3..150", a)
		}
		if r := strOf(s.row(t).AdoptionFailReason); r != cache.ReasonUpstreamOlder {
			t.Fatalf("record reason = %q, want upstream_older", r)
		}
		s.serve(v2)
		time.Sleep(window)
		if r := s.row(t); r.AdoptionFailCount != 0 {
			t.Fatalf("record not cleared once upstream served the current content again: count %d reason %q",
				r.AdoptionFailCount, strOf(r.AdoptionFailReason))
		}

		restore := wallclock.SetOffsetForTest(-2 * time.Hour)
		time.Sleep(2*day + 13*time.Minute)
		v3 := s.release("v3", wallclock.Now().Unix())
		s.serve(v3)
		time.Sleep(window)
		if s.current(t) != hashOf(v3) {
			t.Fatal("after a backward clock step, new content was not adopted")
		}
		restore()

		restore = wallclock.SetOffsetForTest(30 * day)
		time.Sleep(2*day + 17*time.Minute)
		v4 := s.release("v4", wallclock.Now().Unix())
		s.serve(v4)
		time.Sleep(window)
		if s.current(t) != hashOf(v4) {
			t.Fatal("after a forward clock step, new content was not adopted")
		}
		restore()
		s.stop()
		days := int(time.Since(t0) / day)
		if n := len(s.up.Calls(rigAnchorURL)); n < days*10 {
			t.Fatalf("only %d checks over %d days, want >= %d (liveness)", n, days, days*10)
		}
	})
}

// Deferred minor B-58: with no Date on either side the Date rule cannot
// refuse the older content; the rollback guard alone keeps the suite on
// v2, and records it as the refusal state rollback_refused, not a failure.
func TestSimFlappingMirror_UndatedContentIsARefusedRollback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := startRetrySim(t, 30*time.Minute)
		v1, v2 := s.release("v1", 0), s.release("v2", 0)
		s.serve(v1)
		time.Sleep(34 * time.Minute) // v1 adopted (the first tick)
		if s.current(t) != hashOf(v1) {
			t.Fatal("setup: v1 not adopted")
		}
		var n atomic.Int64
		s.up.Set(rigAnchorURL, func(ctx context.Context, etag string) (*fetch.ConditionalResult, error) {
			if n.Add(1)%2 == 1 {
				return simtest.Serve(v2)(ctx, etag)
			}
			return simtest.Serve(v1)(ctx, etag)
		})
		seenV2 := false
		reasons := map[string]bool{}
		for i := 0; i < 24*4; i++ { // sample every 15 min for a day, off the tick grid
			time.Sleep(15 * time.Minute)
			switch s.current(t) {
			case hashOf(v2):
				seenV2 = true
			case hashOf(v1):
				if seenV2 {
					t.Fatalf("flipped back to v1 after %v", time.Duration(i+1)*15*time.Minute)
				}
			}
			if r := s.row(t).AdoptionFailReason; r != nil {
				reasons[*r] = true
			}
		}
		s.stop()
		if !seenV2 || s.current(t) != hashOf(v2) {
			t.Fatal("never settled on v2")
		}
		// v2 served again clears the record (spec C7), so it comes and goes;
		// whenever it is there, it is the refusal state.
		if len(reasons) != 1 || !reasons[cache.ReasonRollbackRefused] {
			t.Fatalf("record reasons %v, want only rollback_refused", reasons)
		}
	})
}

// Deferred minor B-56: clock steps while a failure record is active. A
// backward step leaves next_retry_at far ahead: the retry still comes
// within the clamped hour (spec C7). A forward step makes it long overdue:
// the retry comes at the next tick. In both, content dated by the stepped
// clock is adopted, and content dated more than 10 s ahead of it is the
// failure release_date_in_future, not an adoption.
func TestSimRetry_ClockStepsWithAnActiveRecord(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t.Cleanup(wallclock.SetOffsetForTest(0))
		const refresh = 40 * time.Minute
		tick := refresh / 4
		s := startRetrySim(t, refresh)
		failFor := func(label string) {
			t.Helper()
			bad := s.release(label, wallclock.Now().Unix())
			s.ver.reject(hashOf(bad))
			s.serve(bad)
			for i := 0; i < 5 && s.row(t).AdoptionFailCount < 4; i++ {
				time.Sleep(time.Hour) // the streak grows: next retry 16 min, then 32
			}
			if s.row(t).AdoptionFailCount < 4 {
				t.Fatalf("%s: streak %d, want >= 4", label, s.row(t).AdoptionFailCount)
			}
		}
		// Backward: the record's next_retry_at lands 2 h in the future.
		failFor("bad-before-the-backward-step")
		restore := wallclock.SetOffsetForTest(-2 * time.Hour)
		before := s.ver.calls()
		time.Sleep(time.Hour + tick + time.Minute)
		if s.ver.calls() == before {
			t.Fatal("after a backward step, no retry within the clamped hour")
		}
		ahead := s.release("ahead-of-the-stepped-clock", wallclock.Now().Add(time.Hour).Unix())
		s.serve(ahead)
		time.Sleep(refresh + tick + time.Minute) // the suite is due again once stale
		if s.current(t) == hashOf(ahead) || strOf(s.row(t).AdoptionFailReason) != "release_date_in_future" {
			t.Fatalf("content dated an hour ahead: current %q reason %q, want refused as release_date_in_future",
				s.current(t), strOf(s.row(t).AdoptionFailReason))
		}
		good := s.release("good-after-the-backward-step", wallclock.Now().Unix())
		s.serve(good)
		time.Sleep(refresh + tick + time.Minute)
		if s.current(t) != hashOf(good) {
			t.Fatal("after a backward step, new content was not adopted")
		}
		restore()
		// Forward: the record's next_retry_at is 30 days overdue.
		failFor("bad-before-the-forward-step")
		restore = wallclock.SetOffsetForTest(30 * 24 * time.Hour)
		before = s.ver.calls()
		time.Sleep(tick + time.Minute)
		if s.ver.calls() == before {
			t.Fatal("after a forward step, the overdue retry did not come at the next tick")
		}
		good = s.release("good-after-the-forward-step", wallclock.Now().Unix())
		s.serve(good)
		time.Sleep(refresh + tick + time.Minute)
		if s.current(t) != hashOf(good) {
			t.Fatal("after a forward step, new content was not adopted")
		}
		restore()
		s.stop()
	})
}

// Spec C5: a flapping mirror alternating old and new content never moves
// the suite backwards.
//
// AIDEV-NOTE: v1 is served (and adopted) first, so re-adopting it is also
// refused by the rollback guard (rollback_refused): this simulation holds
// with the Date rule disabled. The Date rule alone is proved end to end
// by TestSimRetry_LongRunDateRulesAndClockSteps' older phase (content
// never adopted before).
func TestSimFlappingMirror_NeverFlipsBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := startRetrySim(t, 30*time.Minute)
		now := time.Now()
		v1 := s.release("v1", now.Add(-2*time.Hour).Unix())
		v2 := s.release("v2", now.Add(-time.Hour).Unix())
		var n atomic.Int64
		s.up.Set(rigAnchorURL, func(ctx context.Context, etag string) (*fetch.ConditionalResult, error) {
			if n.Add(1)%2 == 1 {
				return simtest.Serve(v1)(ctx, etag)
			}
			return simtest.Serve(v2)(ctx, etag)
		})
		seenV2 := false
		time.Sleep(4 * time.Minute)   // samples off the 7 m 30 s tick grid (deferred minor B-58)
		for i := 0; i < 3*24*4; i++ { // sample every 15 min for 3 days
			time.Sleep(15 * time.Minute)
			switch s.current(t) {
			case hashOf(v2):
				seenV2 = true
			case hashOf(v1):
				if seenV2 {
					t.Fatalf("flipped back to v1 after %v", time.Duration(i+1)*15*time.Minute)
				}
			}
		}
		s.stop()
		if !seenV2 || s.current(t) != hashOf(v2) {
			t.Fatal("never settled on v2")
		}
	})
}

// Spec C7 "Restart": failing suites retry spread over the refresh
// interval after a restart, not all on one tick.
func TestSimRestartSpread(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		c := simtest.OpenCache(t)
		up := simtest.NewUpstream()
		ver := &rigVerifier{rejected: map[string]bool{}, panics: map[string]bool{}, entered: make(chan struct{}, 1)}
		pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": testHex64})
		members := newFakeFetcher()
		now := time.Now().Unix()
		var urls []string
		for i := 0; i < 50; i++ {
			host := fmt.Sprintf("h%02d", i)
			seedSimSuite(t, c, host, rigSuite, now, 0) // fresh: only the retry makes it due
			bad := signedRelease("bad-"+host, 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})
			ver.reject(hashOf(bad))
			url := "http://" + host + rigSuite + "/InRelease"
			up.Set(url, simtest.Serve(bad))
			urls = append(urls, url)
			if err := c.RecordAdoptionFailure(ctx, "http", host, rigSuite, cache.AdoptionFailure{
				Hash: hashOf(bad), Reason: "gpg_failed", Count: 5, FirstAt: now - 3600, LastAt: now - 60,
				NextRetryAt: now + 50*60}); err != nil {
				t.Fatal(err)
			}
		}
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: ver, HostLimiter: hostsem.New(8), MaxConcurrent: 2,
			Logger: simtest.DiscardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		rnd := mrand.New(mrand.NewPCG(1, 2))
		var mu sync.Mutex
		const refresh = time.Hour
		start := time.Now()
		_, stop := startSched(t, Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: refresh,
			Adopter: ad, Logger: simtest.DiscardLogger(),
			rand: func() float64 { mu.Lock(); defer mu.Unlock(); return rnd.Float64() }})
		time.Sleep(refresh + 7*time.Minute)
		stop()
		buckets := map[int64]int{}
		retried := 0
		for _, u := range urls {
			calls := up.Calls(u)
			if len(calls) == 0 {
				continue
			}
			retried++
			buckets[int64(calls[0].Sub(start)/(refresh/4))]++
		}
		if retried != 50 {
			t.Fatalf("%d of 50 failing suites retried within the refresh interval after the restart", retried)
		}
		// Deferred minor B-55: each was attempted once — an adoption, not
		// only an upstream request.
		if n := ver.calls(); n != 50 {
			t.Fatalf("%d adoption attempts after the restart, want 50", n)
		}
		for b, n := range buckets {
			if n > 25 {
				t.Fatalf("%d retries landed in tick window %d, want them spread (<= 25 per window)", n, b)
			}
		}
	})
}

// Design note 18 as amended by the final fix wave (FW-3 (a); final spec
// review I3, concurrency E9): an unsigned candidate whose Release bytes
// differ at every observation (generated per request, or load-balanced
// backends that disagree) restarts only its confirmation stamp, not its
// retry count, so it backs off and PE-3's pacing holds: under the real
// scheduler, with no client traffic, it is probed about once per refresh
// interval, like a stable unsigned suite — not at every 2-minute retry
// (383 Release fetches a day before the fix). It never confirms.
func TestSimCost_NeverConfirmingUnsignedCandidateIsPaced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := simtest.OpenCache(t)
		members := newFakeFetcher()
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: &dodVerifier{reject: map[string]bool{},
			calls: map[SuiteRef][]time.Time{}}, HostLimiter: hostsem.New(8), MaxConcurrent: 2, AdoptUnsigned: true,
			Logger: simtest.DiscardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		up := simtest.NewUpstream()
		const host, refresh = "u", 15 * time.Minute
		pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo.deb": testHex64})
		var gen atomic.Int64
		for _, suite := range []string{"/r/dists/stable", "/r/dists/churn"} {
			members.put("http://"+host+suite+"/main/binary-amd64/Packages", pkgs)
			seedStaleAnchorAt(t, c, host, suite)
		}
		stable := []byte(fmt.Sprintf("Origin: stable\nSHA256:\n %s %d main/binary-amd64/Packages\n", hashOf(pkgs), len(pkgs)))
		up.Set("http://"+host+"/r/dists/stable/Release", func(context.Context, string) (*fetch.ConditionalResult, error) {
			return &fetch.ConditionalResult{Status: 200, Body: stable}, nil
		})
		up.Set("http://"+host+"/r/dists/churn/Release", func(context.Context, string) (*fetch.ConditionalResult, error) {
			body := []byte(fmt.Sprintf("Origin: churn\nX-Generated: %d\nSHA256:\n %s %d main/binary-amd64/Packages\n",
				gen.Add(1), hashOf(pkgs), len(pkgs)))
			return &fetch.ConditionalResult{Status: 200, Body: body}, nil
		})
		_, stop := startSched(t, Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: refresh,
			Cooldown: time.Minute, Adopter: ad, Logger: simtest.DiscardLogger(), rand: func() float64 { return 0.5 }})
		const horizon = 24 * time.Hour
		time.Sleep(horizon)
		stop()
		stableN := len(up.Calls("http://" + host + "/r/dists/stable/Release"))
		churnN := len(up.Calls("http://" + host + "/r/dists/churn/Release"))
		// One probe tree per refresh interval (PE-3), plus the first
		// retries (2, 4, 8 minutes) before the backoff passes the interval.
		if bound := int(horizon/refresh) + 4; churnN > bound {
			t.Errorf("a never-confirming unsigned candidate was probed %d times in %v (stable suite: %d), want at most %d",
				churnN, horizon, stableN, bound)
		}
		row, err := c.GetSuiteFreshness(context.Background(), "http", host, "/r/dists/churn")
		if err != nil {
			t.Fatal(err)
		}
		if row.CurrentSnapshotID != nil || strOf(row.AdoptionFailReason) != cache.ReasonUnsignedUnconfirmed || row.AdoptionFailCount < 10 {
			t.Errorf("churning suite: adopted %v, record %q/%d; want unconfirmed with a growing count",
				row.CurrentSnapshotID != nil, strOf(row.AdoptionFailReason), row.AdoptionFailCount)
		}
	})
}
