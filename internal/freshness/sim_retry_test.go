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
		Cooldown: time.Minute, Adopter: ad, Logger: simtest.DiscardLogger(), rand: func() float64 { return 0.5 }})
	return s
}

func (s *retrySim) release(label string, date int64) []byte {
	return datedRelease(label, date, map[string][]byte{"main/binary-amd64/Packages": s.pkgs})
}

func (s *retrySim) serve(body []byte) { s.up.Set(rigAnchorURL, simtest.Serve(body)) }

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
		if n := len(s.up.Calls(rigAnchorURL)); n < 90 {
			t.Fatalf("checks in a day = %d, want >= 90 (liveness)", n)
		}
	})
}

// Spec C7/C5 30-day simulation: an untrusted body (bounded attempts), new
// content (adopted within a refresh interval), an upstream rollback to
// older-dated content (never adopted, bounded refusals, record cleared
// when upstream recovers), and backward and forward clock steps (new
// content still adopted, checks continue).
func TestSim30Days_RetryDateRulesAndClockSteps(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
		time.Sleep(10*day + 7*time.Minute)
		if a := s.ver.calls(); a > 10*30 {
			t.Fatalf("untrusted body: %d attempts in 10 days, want <= 300", a)
		}
		if s.current(t) != "" {
			t.Fatal("untrusted content was adopted")
		}

		v2 := s.release("v2", time.Now().Unix())
		s.serve(v2)
		time.Sleep(window)
		if s.current(t) != hashOf(v2) {
			t.Fatal("new content not adopted within one refresh interval")
		}

		older := s.release("v1-older", t0.Add(day).Unix())
		s.serve(older)
		before := s.ver.calls()
		time.Sleep(8*day + 11*time.Minute)
		if s.current(t) != hashOf(v2) {
			t.Fatal("the suite moved back to older-dated content")
		}
		if a := s.ver.calls() - before; a < 3 || a > 8*30 {
			t.Fatalf("older content cost %d verifications in 8 days, want 3..240", a)
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
		time.Sleep(4*day + 13*time.Minute)
		v3 := s.release("v3", wallclock.Now().Unix())
		s.serve(v3)
		time.Sleep(window)
		if s.current(t) != hashOf(v3) {
			t.Fatal("after a backward clock step, new content was not adopted")
		}
		restore()

		restore = wallclock.SetOffsetForTest(30 * day)
		time.Sleep(4*day + 17*time.Minute)
		v4 := s.release("v4", wallclock.Now().Unix())
		s.serve(v4)
		time.Sleep(window)
		if s.current(t) != hashOf(v4) {
			t.Fatal("after a forward clock step, new content was not adopted")
		}
		restore()
		s.stop()
		if n := len(s.up.Calls(rigAnchorURL)); n < 28*10 {
			t.Fatalf("only %d checks over ~28 days, want >= 280 (liveness)", n)
		}
	})
}

// Spec C5: a flapping mirror alternating old and new content never moves
// the suite backwards.
//
// AIDEV-NOTE: v1 is served (and adopted) first, so re-adopting it is also
// refused by the rollback guard (rollback_refused): this simulation holds
// with the Date rule disabled. The Date rule alone is proved end to end
// by TestSim30Days' older phase (content never adopted before).
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
			bad := datedRelease("bad-"+host, 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})
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
		for b, n := range buckets {
			if n > 25 {
				t.Fatalf("%d retries landed in tick window %d, want them spread (<= 25 per window)", n, b)
			}
		}
	})
}
