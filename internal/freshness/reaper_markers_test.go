package freshness

import (
	"context"
	"maps"
	"strconv"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
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
// last_seen_at). The sweep runs with gc.suite_idle_ttl = 0s too. Inside
// a synctest bubble the rig's pinned clock moves only with the bubble's
// clock, and this test sleeps nowhere, so the reap's stamp is exactly
// reapNow.
func TestReaper_SweepsOrphanMarkersAndKeepsFutureStamps(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		r := newReaperRig(t)
		r.seed("/idle/dists/s", reapNow-31*reapDay)
		r.mark("/idle/dists/s", reapNow-31*reapDay)
		r.mark("/gone/dists/s", reapNow-366*reapDay)
		r.mark("/ahead/dists/s", reapNow+30*reapDay)
		r.reaper.runOnce(ctx)
		// The reap stamps the base's markers MAX(last_seen_at, the cache's
		// clock) (design review M-9, ruling PF-10): kept, moved up to the
		// reap, never lowered.
		if r.exists("/idle/dists/s") || r.markerAt("/idle/dists/s") != reapNow {
			t.Fatalf("idle suite exists %v, its marker %d; want the suite reaped and its marker moved up to %d",
				r.exists("/idle/dists/s"), r.markerAt("/idle/dists/s"), reapNow)
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
	})
}

// clockStepsCounted returns acu_clock_steps_detected_total.
func clockStepsCounted() float64 {
	v, _ := strconv.ParseFloat(renderedMetric("acu_clock_steps_detected_total"), 64)
	return v
}

// The Task 9 review's Important 1: the sweep's future threshold is a fresh
// clock read, while its cutoff keeps the run's now. With the checker and
// the cache on one wall clock, as in production, a reap that takes 5 s
// stamps its base's marker 5 s after the run's start (M-9): the daily sweep
// that follows in the same run reports no clock step for it, and the
// counter does not move. A 10-day forward step during a run does not move
// the run's cutoff: an orphan 360 days old is not swept by that run (a
// fresh cutoff would sweep it, bypassing the forward-step guard, design
// review M-8).
func TestReaper_MarkersStampedDuringTheRunAreNoClockStep(t *testing.T) {
	for _, tc := range []struct {
		name string
		step int64
	}{{"a 5 s reap", 5}, {"a 10-day step during the run", 10 * reapDay}} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := newReaperRig(t)
				r.checker.now = wallclock.Now // one wall clock, as in production
				r.seed("/idle/dists/s", reapNow-31*reapDay)
				r.mark("/idle/dists/s", reapNow-31*reapDay)
				r.mark("/orphan/dists/s", reapNow-360*reapDay)
				r.reaper.reapHook = func(context.Context, cache.SuiteKey) { pinWallclock(t, reapNow+tc.step) }
				before := clockStepsCounted()
				r.reaper.runOnce(context.Background())
				if r.exists("/idle/dists/s") || r.markerAt("/idle/dists/s") != reapNow+tc.step {
					t.Fatalf("idle suite exists %v, its marker %d; want it reaped, its marker stamped at the reap (%d)",
						r.exists("/idle/dists/s"), r.markerAt("/idle/dists/s"), reapNow+tc.step)
				}
				if steps := r.events("clock_step_detected"); len(steps) != 0 || clockStepsCounted() != before {
					t.Fatalf("clock_step_detected lines %v, counter moved by %v; want none: the run stamped that marker",
						steps, clockStepsCounted()-before)
				}
				if r.markerAt("/orphan/dists/s") == -1 {
					t.Fatal("an orphan 360 days old at the run's start was swept: the cutoff moved with the clock")
				}
			})
		})
	}
}

// The Task 16 review's minor 7: the reaper's two clock-step reports — the
// furthest future last_requested_at and the furthest future marker — are
// bounded to one per change already (design review M-10), so they are
// logged directly: a check's report in the same minute, which takes the
// checker's one line a minute, does not swallow them. All three count.
func TestReaper_ClockStepReportsBypassTheRateLimit(t *testing.T) {
	r := newReaperRig(t)
	r.seed("/ahead/dists/s", reapNow+30*reapDay)
	r.mark("/marked/dists/s", reapNow+40*reapDay)
	before := clockStepsCounted()
	r.checker.clockStep("last_check_at", "http", "h", "/checked/dists/s", reapNow+reapDay, time.Unix(reapNow, 0))
	r.reaper.runOnce(context.Background())
	got := map[string]string{}
	for _, ev := range r.events("clock_step_detected") {
		got[ev["stamp"].(string)], _ = ev["suite_path"].(string)
	}
	want := map[string]string{"last_check_at": "/checked/dists/s", "last_requested_at": "/ahead/dists/s",
		"last_seen_at": "/marked/dists/s"}
	if !maps.Equal(got, want) || clockStepsCounted()-before != 3 {
		t.Fatalf("clock_step_detected lines %v, counted %v; want %v, 3", got, clockStepsCounted()-before, want)
	}
}

// Spec C9: the orphan-marker TTL is 365 days, to the second (the final
// review's tests m-4): an orphan last seen exactly 365 days before the
// run is kept — the cutoff is strict — and one seen a second earlier is
// swept.
func TestReaper_TheMarkerTTLIsAYear(t *testing.T) {
	r := newReaperRig(t)
	r.mark("/kept/dists/s", reapNow-365*reapDay)
	r.mark("/gone/dists/s", reapNow-365*reapDay-1)
	r.reaper.runOnce(context.Background())
	if r.markerAt("/kept/dists/s") == -1 || r.markerAt("/gone/dists/s") != -1 {
		t.Fatalf("seen 365 days ago: %d; 365 days and a second ago: %d; want kept and swept",
			r.markerAt("/kept/dists/s"), r.markerAt("/gone/dists/s"))
	}
}

// runReaper starts r's reaper under a context the test's end cancels,
// waiting for Run to return.
func (r *reaperRig) runReaper(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.reaper.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

// Design note 20, ruling PF-13 (final review concurrency I1): the marker
// sweep is the run's daily duty — due one dailyEvery after Run starts,
// not at the first run (a wrong boot clock has a day to be corrected),
// then a day after the previous sweep — while the hourly runs reap. A
// year-old orphan is kept by the runs 1 h to 23 h after the start and
// swept by the run at 24 h; another, marked then, is kept by the runs up
// to 23 h after that sweep and swept by the one a day after it, so
// dailyEvery is pinned to the run (the final review's tests m-4).
func TestReaper_TheMarkerSweepIsDueADayAfterTheStartThenDaily(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newReaperRig(t)
		r.mark("/first/dists/s", reapNow-366*reapDay)
		r.runReaper(t)
		time.Sleep(23*time.Hour + time.Second) // the runs at 1 h … 23 h
		if r.markerAt("/first/dists/s") == -1 {
			t.Fatal("swept within a day of the start: the first sweep is due a day after Run starts")
		}
		time.Sleep(time.Hour) // the run at 24 h
		if r.markerAt("/first/dists/s") != -1 {
			t.Fatal("the run a day after the start did not sweep")
		}
		r.mark("/later/dists/s", reapNow-366*reapDay)
		time.Sleep(23 * time.Hour) // the runs at 25 h … 47 h
		if r.markerAt("/later/dists/s") == -1 {
			t.Fatal("a run 23 h after the last sweep swept: the sweep is a daily duty")
		}
		time.Sleep(time.Hour) // the run at 48 h
		if r.markerAt("/later/dists/s") != -1 {
			t.Fatal("the run a day after the last sweep did not sweep")
		}
	})
}

// Ruling PF-13 (final review concurrency I1, its probe (a)): a start with
// the wall clock ahead — an RTC or NTP fault before the start, which the
// forward-step guard cannot see — has a day to be corrected before the
// first marker sweep. The clock starts 200 days ahead and is put right
// 23 hours later: no run sweeps the orphan last seen 200 days ago (400
// days by the wrong clock), and the first sweep, a day after the start,
// keeps it and still sweeps an orphan a year old.
func TestReaper_AClockAheadAtStartHasADayToBeCorrected(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newReaperRig(t)
		var ahead atomic.Int64
		ahead.Store(200 * reapDay)
		start := time.Now()
		r.checker.now = func() time.Time { return time.Unix(reapNow+ahead.Load(), 0).Add(time.Since(start)) }
		r.mark("/orphan/dists/s", reapNow-200*reapDay)
		r.mark("/old/dists/s", reapNow-366*reapDay)
		r.runReaper(t)
		time.Sleep(time.Hour + time.Second) // the first run
		if r.markerAt("/orphan/dists/s") == -1 {
			t.Fatal("an orphan 200 days old was swept an hour after a start whose wall clock was 200 days ahead")
		}
		time.Sleep(22 * time.Hour) // the runs up to 23 h
		if r.markerAt("/orphan/dists/s") == -1 {
			t.Fatal("an orphan 200 days old was swept within a day of a start whose wall clock was 200 days ahead")
		}
		ahead.Store(0)        // the clock is put right: a backward step, which needs no guard
		time.Sleep(time.Hour) // the run at 24 h sweeps
		if r.markerAt("/orphan/dists/s") == -1 {
			t.Fatal("the first sweep, after the clock was corrected, swept an orphan 200 days old")
		}
		if r.markerAt("/old/dists/s") != -1 {
			t.Fatal("the first sweep, a day after the start, kept an orphan a year old")
		}
	})
}

// Ruling PF-13 (its probe (b)): a wrong forward step of 70 days while the
// daemon runs never ages a marker. The guard detects it, and the sweeps
// after it take it off their cutoff (stepAhead): an orphan 300 days old,
// which looks 370 days old and more by the stepped clock, is kept by the
// three daily sweeps that follow, while a year-old orphan is swept.
func TestReaper_AForwardStepWhileRunningAgesNoMarker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newReaperRig(t)
		var step atomic.Int64
		start := time.Now()
		r.checker.now = func() time.Time { return time.Unix(reapNow+step.Load(), 0).Add(time.Since(start)) }
		r.mark("/orphan/dists/s", reapNow-300*reapDay)
		r.mark("/old/dists/s", reapNow-366*reapDay)
		r.runReaper(t)
		time.Sleep(12*time.Hour + time.Second) // the runs at 1 h … 12 h
		step.Store(70 * reapDay)               // a wrong forward step of 70 days
		time.Sleep(3 * 24 * time.Hour)         // the sweeps at 24 h, 48 h and 72 h
		if ev := r.events("clock_step_detected"); len(ev) != 1 || ev[0]["stamp"] != "wall_clock" {
			t.Fatalf("clock_step_detected = %v, want one with stamp=wall_clock", ev)
		}
		if r.markerAt("/old/dists/s") != -1 {
			t.Fatal("the sweeps after the step kept an orphan a year old")
		}
		if r.markerAt("/orphan/dists/s") == -1 {
			t.Fatal("an orphan 303 days old by the monotonic count was swept after a forward step of 70 days")
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
	reports := clockStepsCounted
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

// Design review M-8 and ruling PF-13 (final review concurrency I1): the
// forward-step guard defers the marker sweep, and a detected step never
// ages a marker. The run due to sweep a day after the start sees the wall
// clock jump ten days — enough to make an orphan five days short of the
// TTL look a year old — and does not sweep; the sweep stays due and runs
// an uptime floor later (25 h), then daily, each taking the step off its
// cutoff (stepAhead). So the orphan, 360 days old at the start, is kept
// until the monotonic clock has aged it past the year: kept by the sweep
// at 97 h (364 days), swept by the one at 121 h (365 days and an hour).
func TestReaper_AForwardStepDefersTheMarkerSweep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newReaperRig(t)
		var step atomic.Int64
		start := time.Now()
		r.checker.now = func() time.Time { return time.Unix(reapNow+step.Load(), 0).Add(time.Since(start)) }
		r.mark("/orphan/dists/s", reapNow-360*reapDay)
		r.runReaper(t)
		time.Sleep(23*time.Hour + time.Second) // the hourly runs to just before the first daily one
		step.Store(10 * reapDay)               // the wall clock jumps ten days
		time.Sleep(time.Hour)                  // the run at 24 h sees the step
		if ev := r.events("clock_step_detected"); len(ev) != 1 || ev[0]["stamp"] != "wall_clock" {
			t.Fatalf("clock_step_detected = %v, want one with stamp=wall_clock", ev)
		}
		time.Sleep(73 * time.Hour) // the sweeps at 25 h, 49 h, 73 h and 97 h
		if r.markerAt("/orphan/dists/s") == -1 {
			t.Fatal("an orphan was swept before the monotonic clock aged it a year: the forward step aged it")
		}
		time.Sleep(24 * time.Hour) // the sweep at 121 h
		if r.markerAt("/orphan/dists/s") != -1 {
			t.Fatal("an orphan aged past the year by the monotonic clock was kept")
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
