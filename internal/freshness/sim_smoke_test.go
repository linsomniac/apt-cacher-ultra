package freshness

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// startSimChecker seeds one suite (/r/dists/x on host h) in a real cache
// and runs a checker's scheduler against a 304-answering upstream. The
// returned stop function cancels the scheduler and drains it; it is safe
// to call more than once and also runs at test cleanup.
func startSimChecker(t *testing.T, refresh time.Duration) (up *simtest.StaticConditional, stop func()) {
	t.Helper()
	c := simtest.OpenCache(t)
	ctx := context.Background()
	h := testHex64
	if err := c.PutBlob(ctx, h, 1); err != nil {
		t.Fatal(err)
	}
	if err := c.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h",
		Path: "/r/dists/x/InRelease", BlobHash: &h, UpstreamURL: "http://h/r/dists/x/InRelease", IsMetadata: true}); err != nil {
		t.Fatal(err)
	}
	if err := c.PutSuiteFreshness(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}); err != nil {
		t.Fatal(err)
	}
	up = &simtest.StaticConditional{Status: 304}
	chk, err := New(Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8),
		Cooldown: time.Minute, Refresh: refresh, Logger: simtest.DiscardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { chk.Run(runCtx); close(done) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			<-done
			chk.WaitForAdoptions()
		})
	}
	// A t.Fatal before the caller's stop must not leave Run running in
	// the bubble; registered after OpenCache, so it runs before the
	// cache closes.
	t.Cleanup(stop)
	return up, stop
}

// Phase A DoD: the real cache, checker and scheduler run for 30 virtual
// days inside a synctest bubble. Asserts liveness (checks keep happening)
// and a bound (no runaway checking).
func TestSimSmoke_30DaysPeriodicRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		up, stop := startSimChecker(t, 2*time.Hour)
		time.Sleep(30 * 24 * time.Hour)
		stop()
		// Ticks run every refresh/4 = 30m. The first tick (30m) checks the
		// never-succeeded suite; after that a check lands whenever
		// last_success_at is 2h old: 30m, 2h30m, ... = 360 checks in 30
		// days. synctest makes this exact.
		if got := up.Calls(); got != 360 {
			t.Fatalf("checks in 30 virtual days = %d, want exactly 360", got)
		}
	})
}

// Spec §7 C4: wall-clock steps of +30d, -30d and -2h must not stop
// periodic checks (liveness) nor cause runaway checking (bound).
func TestSimClockSteps_KeepChecking(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		up, stop := startSimChecker(t, 15*time.Minute)
		defer stop()
		phase := func(name string, offset time.Duration) {
			restore := wallclock.SetOffsetForTest(offset)
			defer restore()
			before := up.Calls()
			time.Sleep(12 * time.Hour)
			got := up.Calls() - before
			// One check per 15 min; the first tick after a step may be
			// lost to rounding at the step or gained by healing.
			if want := int32(12 * 4); got < want-2 || got > want+2 {
				t.Errorf("%s: %d checks in 12h, want %d±2", name, got, want)
			}
		}
		phase("baseline", 0)
		phase("forward +30d", 30*24*time.Hour)
		phase("back to 0 (-30d step)", 0)
		phase("backward -2h", -2*time.Hour)
	})
}
