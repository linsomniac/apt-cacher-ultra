package freshness

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// markerAt returns base's marker stamp on host h over http, or -1.
func (r *reaperRig) markerAt(base string) int64 {
	r.t.Helper()
	at, ok, err := r.cache.SignedSeen(context.Background(), "http", "h", base)
	if err != nil {
		r.t.Fatal(err)
	}
	if !ok {
		return -1
	}
	return at
}

// mark writes base's marker on host h over http, stamped at.
func (r *reaperRig) mark(base string, at int64) {
	r.t.Helper()
	if err := r.cache.MarkSignedSeenBatch(context.Background(), []cache.SignedSeenMark{{CanonicalScheme: "http",
		CanonicalHost: "h", Base: base, SeenAt: at}}); err != nil {
		r.t.Fatal(err)
	}
}

// Spec C9 (ruling R-F5): a run sweeps the orphan markers last seen a year
// ago, while a reaped suite's marker survives the reap. A marker stamped
// in the future reads as now: it is kept as it is — never rewritten
// (ruling PF-1) — and reported as clock_step_detected (stamp
// last_seen_at). The sweep runs with gc.suite_idle_ttl = 0s too.
func TestReaper_SweepsOrphanMarkersAndKeepsFutureStamps(t *testing.T) {
	ctx := context.Background()
	r := newReaperRig(t)
	r.seed("/idle/dists/s", reapNow-31*reapDay)
	r.mark("/idle/dists/s", reapNow-31*reapDay)
	r.mark("/gone/dists/s", reapNow-366*reapDay)
	r.mark("/ahead/dists/s", reapNow+30*reapDay)
	r.reaper.runOnce(ctx)
	// The reap stamps the base's markers MAX(last_seen_at, the cache's
	// clock) (design review M-9): kept, and never lowered.
	if r.exists("/idle/dists/s") || r.markerAt("/idle/dists/s") < reapNow-31*reapDay {
		t.Fatalf("idle suite exists %v, its marker %d; want the suite reaped and its marker kept (at least %d)",
			r.exists("/idle/dists/s"), r.markerAt("/idle/dists/s"), reapNow-31*reapDay)
	}
	if r.markerAt("/gone/dists/s") != -1 || r.markerAt("/ahead/dists/s") != reapNow+30*reapDay {
		t.Fatalf("year-old orphan %d, future marker %d; want the orphan swept and the future stamp kept as it was (%d)",
			r.markerAt("/gone/dists/s"), r.markerAt("/ahead/dists/s"), reapNow+30*reapDay)
	}
	if ev := r.events("signed_seen_markers_swept"); len(ev) != 1 || ev[0]["count"] != float64(1) {
		t.Fatalf("signed_seen_markers_swept lines %v, want one with count 1", ev)
	}
	steps := r.events("clock_step_detected")
	if len(steps) != 1 || steps[0]["stamp"] != "last_seen_at" || steps[0]["suite_path"] != "/ahead/dists/s" ||
		steps[0]["stored_unixtime"] != float64(reapNow+30*reapDay) {
		t.Fatalf("clock_step_detected lines %v, want one for the future marker", steps)
	}
	r.reaper.idleTTL = 0
	r.mark("/old/dists/s", reapNow-400*reapDay)
	r.reaper.dailyAt = time.Time{} // the next day's run: the sweep is daily
	r.reaper.runOnce(ctx)
	if r.markerAt("/old/dists/s") != -1 {
		t.Fatal("gc.suite_idle_ttl = 0s stopped the marker sweep")
	}
}

// Spec C9: the orphan-marker TTL is 365 days — an orphan seen 364 days
// ago is kept, one seen 366 days ago is swept.
func TestReaper_TheMarkerTTLIsAYear(t *testing.T) {
	r := newReaperRig(t)
	r.mark("/kept/dists/s", reapNow-364*reapDay)
	r.mark("/gone/dists/s", reapNow-366*reapDay)
	r.reaper.runOnce(context.Background())
	if r.markerAt("/kept/dists/s") == -1 || r.markerAt("/gone/dists/s") != -1 {
		t.Fatalf("seen 364 days ago: %d; 366 days ago: %d; want kept and swept",
			r.markerAt("/kept/dists/s"), r.markerAt("/gone/dists/s"))
	}
}

// Design note 20 (ruling PF-4): the marker sweep is the run's daily duty
// — at the first run after a start, then a day apart — while the hourly
// runs reap: an orphan a year old is swept at the first run; another
// found an hour later waits for the run a day after the first.
func TestReaper_DailyDutiesRunAtStartAndADayApart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		r := newReaperRig(t)
		r.mark("/first/dists/s", reapNow-366*reapDay)
		r.reaper.runOnce(ctx)
		if r.markerAt("/first/dists/s") != -1 {
			t.Fatal("the first run after a start did not sweep")
		}
		r.mark("/later/dists/s", reapNow-366*reapDay)
		time.Sleep(time.Hour)
		r.reaper.runOnce(ctx)
		if r.markerAt("/later/dists/s") == -1 {
			t.Fatal("an hourly run swept: the sweep is a daily duty")
		}
		time.Sleep(23 * time.Hour)
		r.reaper.runOnce(ctx)
		if r.markerAt("/later/dists/s") != -1 {
			t.Fatal("the run a day after the first did not sweep")
		}
	})
}

// Design review M-10: a marker stamped in the future is reported at the
// first sweep after a start and again only when the furthest such marker
// changes (here a later sighting moves its stamp on), not at every daily
// sweep until the clock passes it. Every report counts in
// acu_clock_steps_detected_total.
func TestReaper_ReportsAFutureMarkerOnlyWhenItChanges(t *testing.T) {
	ctx := context.Background()
	r := newReaperRig(t)
	reports := func() float64 {
		v, _ := strconv.ParseFloat(renderedMetric("acu_clock_steps_detected_total"), 64)
		return v
	}
	r.mark("/ahead/dists/s", reapNow+30*reapDay)
	before := reports()
	for range 3 {
		r.reaper.dailyAt = time.Time{} // a day apart
		r.reaper.runOnce(ctx)
	}
	if got := reports() - before; got != 1 {
		t.Fatalf("three sweeps over one future marker: %v reports, want 1", got)
	}
	r.mark("/ahead/dists/s", reapNow+31*reapDay)
	r.reaper.dailyAt = time.Time{}
	r.reaper.runOnce(ctx)
	if got := reports() - before; got != 2 {
		t.Fatalf("after its stamp moved on: %v reports, want 2", got)
	}
}

// Design review M-8: the forward-step guard defers the marker sweep too.
// A daily sweep whose run sees the wall clock jump ten days — enough to
// make an orphan five days short of the TTL look a year old — does not
// sweep; the sweep stays due and runs an uptime floor later.
func TestReaper_AForwardStepDefersTheMarkerSweep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newReaperRig(t)
		var step atomic.Int64
		start := time.Now()
		r.checker.now = func() time.Time { return time.Unix(reapNow+step.Load(), 0).Add(time.Since(start)) }
		r.mark("/orphan/dists/s", reapNow-360*reapDay)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { r.reaper.Run(ctx); close(done) }()
		defer func() { cancel(); <-done }()
		time.Sleep(time.Hour + time.Second) // the first run, at the floor, sweeps: 360 days is not a year
		if r.markerAt("/orphan/dists/s") == -1 {
			t.Fatal("swept before the step")
		}
		time.Sleep(23 * time.Hour) // through the hourly runs to just before the next daily one
		step.Store(10 * reapDay)   // the wall clock jumps ten days
		time.Sleep(time.Hour)
		if r.markerAt("/orphan/dists/s") == -1 {
			t.Fatal("swept by the run that saw the forward step")
		}
		time.Sleep(time.Hour)
		if r.markerAt("/orphan/dists/s") != -1 {
			t.Fatal("not swept an uptime floor after the step")
		}
	})
}

// Deferred minor E-20 (phase E design note 30): the checker's marker
// writer writes at every probe that sights a signature, so the marker's
// last_seen_at — what the orphan sweep ages — is the last sighting.
func TestCheck_EverySightingAdvancesTheMarkerStamp(t *testing.T) {
	ctx := context.Background()
	r := newRetryRig(t, 0)
	in := r.release("v1", 0)
	r.up.set(in)
	r.ver.reject(contentKey(adoptionFormInline, in, nil, nil)) // stays unadopted: each probe sights it
	t0 := r.clock.now().Unix()
	r.check()
	if at, ok, err := r.cache.SignedSeen(ctx, "http", rigHost, rigSuite); err != nil || !ok || at != t0 {
		t.Fatalf("first sighting: last_seen_at %d (%v, %v), want %d", at, ok, err, t0)
	}
	r.clock.advance(time.Hour)
	r.check()
	if at, _, err := r.cache.SignedSeen(ctx, "http", rigHost, rigSuite); err != nil || at != t0+3600 {
		t.Fatalf("second sighting: last_seen_at %d (%v), want %d", at, err, t0+3600)
	}
}
