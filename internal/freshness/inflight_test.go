package freshness

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// Spec C10: a request waits until the host's last signed adoption in
// flight ends, at most strictWait; with nothing in flight it may re-run its
// lookup at once; a cancelled request stops waiting and may not re-run its
// lookup (design note 31: its client left, review fix round 1, Task 9).
func TestHostAdoptions_Wait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHostAdoptions()
		start := time.Now()
		if !h.wait(context.Background(), "a", strictWait) || time.Since(start) != 0 {
			t.Fatal("with nothing in flight the wait must return at once, re-running the lookup")
		}
		h.begin("a")
		h.begin("a")
		go func() {
			time.Sleep(time.Second)
			h.end("a")
			time.Sleep(time.Second)
			h.end("a")
		}()
		if !h.wait(context.Background(), "a", strictWait) || time.Since(start) != 2*time.Second {
			t.Fatalf("waited %v, want until the second adoption ended (2 s)", time.Since(start))
		}
		h.begin("a")
		start = time.Now()
		ok := h.wait(context.Background(), "a", strictWait)
		if d := time.Since(start); d != strictWait {
			t.Fatalf("waited %v, want the %v bound", d, strictWait)
		}
		if !ok {
			t.Fatal("a wait that reached its bound must re-run the lookup (and refuse if still unvouched)")
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(3 * time.Second); cancel() }()
		start = time.Now()
		ok = h.wait(ctx, "a", strictWait)
		if d := time.Since(start); d != 3*time.Second {
			t.Fatalf("a cancelled request waited %v, want 3 s", d)
		}
		if ok {
			t.Fatal("a cancelled request's wait returned true: its lookup would re-run with a dead context")
		}
		h.end("a")
		if h.inFlight("a") {
			t.Fatal("still in flight after every end")
		}
	})
}

// Design review M-10: a request waits at most min(15 s, 25 s − its age),
// and not at all (an immediate refusal, not counted as a wait) when less
// than 1 s of that budget is left: the miss path still downloads the .deb
// after the wait, inside apt's 30 s.
func TestChecker_WaitIsBoundedByTheRequestsAge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := &Checker{inflight: newHostAdoptions()}
		c.inflight.begin("a")
		for _, tc := range []struct {
			age, want time.Duration
			ok        bool
		}{
			{0, strictWait, true},
			{20 * time.Second, 5 * time.Second, true},
			{24*time.Second + 500*time.Millisecond, 0, false},
		} {
			before := hostCounter(t, "acu_strict_adoption_waits_total", "a")
			start := time.Now()
			if got := c.WaitHostAdoptions(context.Background(), "a", start.Add(-tc.age)); got != tc.ok {
				t.Fatalf("age %v: WaitHostAdoptions = %v, want %v", tc.age, got, tc.ok)
			}
			if d := time.Since(start); d != tc.want {
				t.Fatalf("age %v: waited %v, want %v", tc.age, d, tc.want)
			}
			if counted := hostCounter(t, "acu_strict_adoption_waits_total", "a") - before; counted != map[bool]float64{true: 1, false: 0}[tc.ok] {
				t.Fatalf("age %v: %v waits counted", tc.age, counted)
			}
		}
		c.inflight.end("a")
	})
}

// Spec C10: at most 64 waiters per host; the 65th is refused at once.
// acu_strict_adoption_waits_total counts the requests that waited: not the
// refused one, nor one with nothing in flight.
func TestHostAdoptions_WaiterCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHostAdoptions()
		beforeA, beforeB := hostCounter(t, "acu_strict_adoption_waits_total", "a"), hostCounter(t, "acu_strict_adoption_waits_total", "b")
		h.begin("a")
		var wg sync.WaitGroup
		for i := 0; i < strictWaitersPerHost; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); h.wait(context.Background(), "a", strictWait) }()
		}
		synctest.Wait()
		start := time.Now()
		if h.wait(context.Background(), "a", strictWait) || time.Since(start) != 0 {
			t.Fatal("the 65th waiter was not refused at once")
		}
		if h.wait(context.Background(), "b", strictWait) != true {
			t.Fatal("the cap is per host")
		}
		h.end("a")
		wg.Wait()
		if a, b := hostCounter(t, "acu_strict_adoption_waits_total", "a")-beforeA, hostCounter(t, "acu_strict_adoption_waits_total", "b")-beforeB; a != strictWaitersPerHost || b != 0 {
			t.Fatalf("waits counted a=%v b=%v, want %d and 0", a, b, strictWaitersPerHost)
		}
	})
}

// Deferred minor E-58: with nothing in flight a request re-runs its lookup
// at once even while the waiter cap is reached — the host's last adoption
// ended and its 64 waiters are still unwinding.
func TestHostAdoptions_NothingInFlightBeatsTheWaiterCap(t *testing.T) {
	h := newHostAdoptions()
	h.waiters["a"] = strictWaitersPerHost // an ended adoption's waiters, not yet unwound
	if !h.wait(context.Background(), "a", strictWait) {
		t.Fatal("a request with nothing in flight was refused for the waiter cap")
	}
}

// Spec C10: the checker counts a signed adoption in flight from its spawn
// until its goroutine returns (after the commit); an unsigned adoption is
// not counted (it never vouches).
func TestChecker_CountsSignedAdoptionsInFlight(t *testing.T) {
	for _, unsigned := range []bool{false, true} {
		r := newRetryRig(t, 0)
		var during bool
		r.adopter.beforeCommit = func() { during = r.checker.AdoptionsInFlight(rigHost) }
		if unsigned {
			r.adoptUnsigned("u1")
			r.unsignedRelease("u2", nil)
		} else {
			r.up.set(r.release("v1", 0))
		}
		r.check()
		if snap := r.currentSnapshot(); snap == nil || (unsigned && snap.Form != cache.FormUnsigned) {
			t.Fatalf("unsigned=%v: nothing adopted: %+v", unsigned, snap)
		}
		if during == unsigned {
			t.Fatalf("unsigned=%v: in flight during the commit = %v", unsigned, during)
		}
		if r.checker.AdoptionsInFlight(rigHost) {
			t.Fatalf("unsigned=%v: still in flight after the adoption returned", unsigned)
		}
	}
}

// Spec C11: phase E's constants are not knobs, and every other test uses
// them symbolically, so only this one sees a change of value. (Proposed by
// the executability review; kills X14–X18. The request budget and the
// minimum wait are design review M-10's.)
func TestSpecC11PhaseEConstants(t *testing.T) {
	for name, c := range map[string]struct{ got, want int64 }{
		"interest window":         {int64(interestWindow), int64(7 * 24 * time.Hour)},
		"confirmation gap floor":  {int64(minConfirmGap), int64(5 * time.Minute)},
		"denied-signature gap":    {int64(deniedConfirmGap), int64(time.Hour)},
		"strict wait":             {int64(strictWait), int64(15 * time.Second)},
		"strict waiters per host": {int64(strictWaitersPerHost), 64},
		"strict request budget":   {int64(strictRequestBudget), int64(25 * time.Second)},
		"strict minimum wait":     {int64(strictMinWait), int64(time.Second)},
		"unsigned member cap":     {int64(unsignedMemberCap), 1000},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d (spec C11)", name, c.got, c.want)
		}
	}
}
