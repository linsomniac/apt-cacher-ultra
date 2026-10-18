package handler

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// serve answers ledger requests through the handler (Request.Suite is the
// suite path): anchors are InRelease, indexes the suite's Packages. The
// cache held a request for whatever the serve took beyond its transfer.
func (s *flipStack) serve(anchorTransfer, indexTransfer time.Duration) simtest.ServeFunc {
	return func(rq simtest.Request) (string, time.Duration) {
		path, transfer := rq.Suite+"/main/binary-amd64/Packages", indexTransfer
		if rq.Anchor {
			path, transfer = rq.Suite+"/InRelease", anchorTransfer
		}
		start := time.Now()
		gen, code := s.get(path, transfer)
		if code != http.StatusOK {
			s.t.Errorf("GET %s = %d", path, code)
		}
		return gen, max(time.Since(start)-transfer, 0)
	}
}

// adoptDetached is adopt for the detached form: the snapshot's anchors are
// Release and Release.gpg.
func (s *flipStack) adoptDetached(suite string, gen int) {
	ref, rel := s.publish(suite, gen)
	sig := []byte(fmt.Sprintf("signature of %s generation %d", suite, gen))
	if err := s.ad.RunDetached(context.Background(), ref, rel, sig, "", ""); err != nil {
		s.t.Errorf("adopt %s generation %d (detached): %v", suite, gen, err)
	}
}

// serveDetached is serve for a detached suite: an anchor is Release and
// then Release.gpg, fetched as apt does on the client's one connection,
// and the anchor has arrived once both have. A Release.gpg from another
// snapshot than the Release before it is counted in sigMismatch.
func (s *flipStack) serveDetached(anchorTransfer, indexTransfer time.Duration, sigMismatch *atomic.Int64) simtest.ServeFunc {
	index := s.serve(anchorTransfer, indexTransfer)
	return func(rq simtest.Request) (string, time.Duration) {
		if !rq.Anchor {
			return index(rq)
		}
		start := time.Now()
		gen, code := s.get(rq.Suite+"/Release", anchorTransfer)
		held := max(time.Since(start)-anchorTransfer, 0)
		sigGen, sigCode := s.get(rq.Suite+"/Release.gpg", indexTransfer)
		if code != http.StatusOK || sigCode != http.StatusOK {
			s.t.Errorf("GET %s/Release = %d, Release.gpg = %d", rq.Suite, code, sigCode)
		}
		if sigGen != gen {
			sigMismatch.Add(1)
		}
		return gen, held
	}
}

// flipReaders starts, every period from offset until until, a client
// that runs one update of queue through serve.
func flipReaders(l *simtest.Ledger, serve simtest.ServeFunc, queue []simtest.Request, offset, period time.Duration, until time.Time, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(offset)
		for i := 0; time.Now().Before(until); i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				l.RunUpdate(i, queue, serve)
			}()
			time.Sleep(period)
		}
	}()
}

// recordAnchorHeld wraps serve and records how long each anchor was held.
func recordAnchorHeld(serve simtest.ServeFunc, held *[]time.Duration) simtest.ServeFunc {
	return func(rq simtest.Request) (string, time.Duration) {
		gen, h := serve(rq)
		if rq.Anchor {
			*held = append(*held, h)
		}
		return gen, h
	}
}

// Spec C8's purpose, end to end (§7: a busy suite, an anchor every 500 ms
// for 120 s across a flip): clients read the anchor over 1 s links and ask
// for its index right after. Without the barrier
// (flip_quiet_period = 0s, today's behaviour) the flip lands while two
// anchors are in flight, and those clients get the new index for the old
// anchor. With it (10s) phase A gives up at its cap, the hold holds new
// anchor requests until the in-flight ones are done and the suite has
// been quiet for 10 s, and no client straddles.
func TestSimFlip_BusySuiteStraddlesOnlyWithoutTheBarrier(t *testing.T) {
	busySuiteSim(t, false)
}

// The same for a detached suite (design review M16): each client fetches
// Release, then Release.gpg — never held — then the index. Release.gpg
// must come from its client's Release snapshot: apt asks for it right after
// Release, within the quiet period.
func TestSimFlip_DetachedBusySuiteStraddlesOnlyWithoutTheBarrier(t *testing.T) {
	busySuiteSim(t, true)
}

// busySuiteSim runs the busy-suite simulation at 0s and 10s, for the
// inline or the detached form.
func busySuiteSim(t *testing.T, detached bool) {
	for _, quiet := range []time.Duration{0, 10 * time.Second} {
		synctest.Test(t, func(t *testing.T) {
			s := newFlipStack(t, quiet)
			const suite = "/u/dists/busy"
			adopt := s.adopt
			var sigMismatch atomic.Int64
			serve := s.serve(time.Second, 100*time.Millisecond)
			if detached {
				adopt = s.adoptDetached
				serve = s.serveDetached(time.Second, 100*time.Millisecond, &sigMismatch)
			}
			adopt(suite, 1)
			before := s.gate.Stats()
			l := simtest.NewLedger(10 * time.Second)
			var wg sync.WaitGroup
			t0 := time.Now()
			flipReaders(l, serve, []simtest.Request{{Suite: suite, Anchor: true}},
				11*time.Millisecond, 500*time.Millisecond, t0.Add(120*time.Second), &wg)
			flipAt := 30*time.Second + 7*time.Millisecond
			time.Sleep(flipAt)
			adopt(suite, 2)
			committed := time.Since(t0)
			wg.Wait()

			straddles := l.Straddles()
			if quiet == 0 {
				if committed != flipAt {
					t.Fatalf("without the barrier the commit came %v after the flip started, want at once", committed-flipAt)
				}
				if len(straddles) != 2 {
					t.Fatalf("without the barrier: %d straddles, want the 2 clients whose anchors were in flight: %+v", len(straddles), straddles)
				}
				if n := sigMismatch.Load(); detached && n != 2 {
					t.Fatalf("without the barrier: %d Release.gpg responses from another snapshot than their Release, want those 2 clients'", n)
				}
				return
			}
			if len(straddles) != 0 {
				t.Fatalf("with the barrier: %d straddles, first %+v", len(straddles), straddles[0])
			}
			if n := sigMismatch.Load(); n != 0 {
				t.Fatalf("with the barrier: %d Release.gpg responses from another snapshot than their Release", n)
			}
			holdStart := flipAt + flipgate.PhaseAMax // the suite is never naturally quiet
			if committed < holdStart+quiet || committed > holdStart+flipgate.HoldMax {
				t.Fatalf("committed at %v, want within the hold from %v, after the quiet period", committed, holdStart)
			}
			if st := s.gate.Stats(); st.Holds-before.Holds != 1 || st.HoldsExpired != before.HoldsExpired || st.HeldRequests == before.HeldRequests {
				t.Fatalf("stats %+v (before %+v): want one hold, ended normally, that held requests", st, before)
			}
			if gens := l.AnchorGens(suite); len(gens) != 2 {
				t.Fatalf("clients saw anchor generations %v, want two (they crossed the flip)", gens)
			}
		})
	}
}

// Spec C8 DoD, the review's overlapping-hold trace (plan review
// 2026-10-05, finding 6) through the real handler, adopter and cache, as
// in internal/flipgate's model simulation: busy suites a and b on one
// host leave phase A at its cap 0.1 s apart; the trace client gets a's
// old anchor just before a's hold, spends 0.3 s on an unrelated download,
// then asks for b's anchor with a's index queued behind it. b is not
// holding yet (one hold at a time), so neither anchor waits and the a
// index arrives before a commits. 100 shared-queue clients cross both
// flips; b commits at least a quiet period after a.
func TestSimFlip_SharedQueueTraceEndToEnd(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newFlipStack(t, 10*time.Second)
		const a, b = "/mirror/dists/a", "/mirror/dists/b"
		s.adopt(a, 1)
		s.adopt(b, 1) // at once: a's setup hold held nothing (design note 17)
		before := s.gate.Stats()
		fast := s.serve(50*time.Millisecond, 10*time.Millisecond)
		l := simtest.NewLedger(10 * time.Second)
		var wg sync.WaitGroup
		t0 := time.Now()
		until := t0.Add(100 * time.Second)
		flipReaders(l, fast, []simtest.Request{{Suite: a, Anchor: true}}, 11*time.Millisecond, 2*time.Second, until, &wg)
		flipReaders(l, fast, []simtest.Request{{Suite: b, Anchor: true}}, 1011*time.Millisecond, 2*time.Second, until, &wg)
		wg.Add(1)
		go func() { // another b reader, on a slow link, still reading when b's hold could start
			defer wg.Done()
			time.Sleep(60950 * time.Millisecond)
			l.RunUpdate(1000, []simtest.Request{{Suite: b, Anchor: true}}, s.serve(6*time.Second, 10*time.Millisecond))
		}()
		var traceHeld []time.Duration
		wg.Add(1)
		go func() { // the trace client
			defer wg.Done()
			time.Sleep(60853 * time.Millisecond)
			l.RunUpdate(2000, []simtest.Request{{Suite: a, Anchor: true}, {Filler: 300 * time.Millisecond}, {Suite: b, Anchor: true}},
				recordAnchorHeld(fast, &traceHeld))
		}()
		for i := 0; i < 100; i++ { // shared-queue clients, both orders
			q := []simtest.Request{{Suite: a, Anchor: true}, {Suite: b, Anchor: true}}
			if i%2 == 1 {
				q[0], q[1] = q[1], q[0]
			}
			at := 51*time.Second + 7*time.Millisecond + time.Duration(i)*450*time.Millisecond
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(at)
				l.RunUpdate(3000+i, q, fast)
			}()
		}
		var aAt, bAt time.Time
		wg.Add(2)
		go func() {
			defer wg.Done()
			time.Sleep(time.Second + 3*time.Millisecond) // both suites are busy by now
			s.adopt(a, 2)
			aAt = time.Now()
		}()
		go func() {
			defer wg.Done()
			time.Sleep(time.Second + 103*time.Millisecond)
			s.adopt(b, 2)
			bAt = time.Now()
		}()
		wg.Wait()

		if len(traceHeld) != 2 || traceHeld[0] != 0 || traceHeld[1] != 0 {
			t.Fatalf("the trace client's anchors were held %v, want neither", traceHeld)
		}
		if st := l.Straddles(); len(st) != 0 {
			t.Fatalf("%d straddles, first %+v", len(st), st[0])
		}
		if bAt.Sub(aAt) < 10*time.Second {
			t.Fatalf("b committed %v after a, want at least the quiet period (one hold at a time, then cross-suite quiet)", bAt.Sub(aAt))
		}
		if st := s.gate.Stats(); st.Holds-before.Holds != 2 || st.HoldsExpired != before.HoldsExpired || st.HoldsSkipped != before.HoldsSkipped {
			t.Fatalf("stats %+v (before %+v): want two holds, none expired or skipped", st, before)
		}
		for _, k := range []string{a, b} {
			if gens := l.AnchorGens(k); len(gens) != 2 {
				t.Errorf("%s: clients saw anchor generations %v, want two (they crossed the flip)", k, gens)
			}
		}
		if n := l.Checked(); n < 250 {
			t.Errorf("only %d index responses checked", n)
		}
	})
}

// Spec C8 slow reader, end to end (final review, concurrency Important 1:
// the scenario this test had when it was SlowReaderEndsTheHoldAtHoldMax,
// now with a commit that takes 200 ms, as a real one does). A busy suite
// (an anchor every 500 ms over a 1 s link) and a reader on a 12 s or 25 s
// link that starts half a second before the hold keep the suite from
// going quiet before HoldMax − CommitMargin. No other hold ended, so the
// hold commits then, inside its bound, and answers the ~31 held requests
// from the new snapshot; only the slow reader — the late reader, counted
// in acu_flip_quiet_cut_short_total — may straddle (the 12 s one fetched
// its index before the commit, the 25 s one is still receiving the old
// anchor). Answering the held requests from the old snapshot at the bound,
// right before the commit, made 42 clients straddle.
func TestSimFlip_SlowReaderCommitsInsideTheHold(t *testing.T) {
	for _, tc := range []struct {
		slow time.Duration
		late int // straddles allowed: the slow reader's own
	}{{12 * time.Second, 0}, {25 * time.Second, 1}} {
		t.Run(tc.slow.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const commitLatency = 200 * time.Millisecond
				s := newFlipStack(t, 10*time.Second)
				const suite = "/u/dists/slow"
				s.adopt(suite, 1)
				s.ad.SetCommitDelayForTest(commitLatency)
				before := s.gate.Stats()
				names := []string{"acu_flip_hold_expired_total", "acu_flip_quiet_cut_short_total"}
				counters := sumMetricLines(t, names)
				l := simtest.NewLedger(10 * time.Second)
				var wg sync.WaitGroup
				t0 := time.Now()
				flipReaders(l, s.serve(time.Second, 10*time.Millisecond), []simtest.Request{{Suite: suite, Anchor: true}},
					11*time.Millisecond, 500*time.Millisecond, t0.Add(100*time.Second), &wg)
				wg.Add(1)
				go func() { // starts half a second before the hold
					defer wg.Done()
					time.Sleep(60503 * time.Millisecond)
					l.RunUpdate(1000, []simtest.Request{{Suite: suite, Anchor: true}}, s.serve(tc.slow, 10*time.Millisecond))
				}()
				flipAt := time.Second + 3*time.Millisecond
				time.Sleep(flipAt)
				s.adopt(suite, 2)
				committed := time.Since(t0)
				wg.Wait()

				st := l.Straddles()
				if len(st) != tc.late {
					t.Errorf("%d straddles, want %d (the slow reader's own); first %+v", len(st), tc.late, st[:min(len(st), 1)])
				}
				for _, x := range st {
					if x.Client != 1000 {
						t.Errorf("a client other than the slow reader straddled: %+v", x)
						break
					}
				}
				holdStart := flipAt + flipgate.PhaseAMax // the suite is never naturally quiet
				if want := holdStart + flipgate.HoldMax - flipgate.CommitMargin + commitLatency; committed != want || committed-holdStart > flipgate.HoldMax {
					t.Errorf("committed at %v, want %v (the commit starts at HoldMax − CommitMargin), within the hold's %v",
						committed, want, flipgate.HoldMax)
				}
				if gs := s.gate.Stats(); gs.Holds-before.Holds != 1 || gs.HoldsExpired != before.HoldsExpired ||
					gs.QuietCutShort-before.QuietCutShort != 1 || gs.HeldRequests-before.HeldRequests < 30 {
					t.Errorf("stats %+v (before %+v): want one hold, cut short, not expired, that held about 31 requests", gs, before)
				}
				after := sumMetricLines(t, names)
				if d := after["acu_flip_quiet_cut_short_total"] - counters["acu_flip_quiet_cut_short_total"]; d != 1 {
					t.Errorf("acu_flip_quiet_cut_short_total rose by %v, want 1", d)
				}
				if d := after["acu_flip_hold_expired_total"] - counters["acu_flip_hold_expired_total"]; d != 0 {
					t.Errorf("acu_flip_hold_expired_total rose by %v, want 0", d)
				}
				if gens := l.AnchorGens(suite); len(gens) != 2 {
					t.Errorf("clients saw anchor generations %v, want two (they crossed the flip)", gens)
				}
				if n := l.Checked(); n < 150 {
					t.Errorf("only %d index responses checked", n)
				}
			})
		})
	}
}

// Spec C8 under seeded chaos: three suites on one host; 160 clients over
// four minutes with random queues (a random set of the suites' anchors in
// random order, sometimes behind an unrelated download), random link
// speeds (up to 3 s) and start times; two flips per suite at random times,
// the second 30-90 s after the first was due. All random values are drawn
// before anything runs, so the run is reproducible. Every hold ends
// normally and no flip skips its hold, so no client may straddle; clients
// cross all three generations of every suite.
func TestSimFlip_SeededChaos(t *testing.T) {
	type clientPlan struct {
		at       time.Duration
		transfer time.Duration // its anchors' transfer time
		queue    []simtest.Request
	}
	rng := rand.New(rand.NewPCG(1, 2))
	suites := []string{"/m/dists/a", "/m/dists/b", "/m/dists/c"}
	plans := make([]clientPlan, 160)
	for i := range plans {
		p := clientPlan{
			at:       time.Duration(rng.Int64N(int64(240 * time.Second))),
			transfer: 10*time.Millisecond + time.Duration(rng.Int64N(int64(3*time.Second))),
		}
		for _, j := range rng.Perm(len(suites))[:1+rng.IntN(len(suites))] {
			if rng.IntN(3) == 0 {
				p.queue = append(p.queue, simtest.Request{Filler: time.Duration(rng.Int64N(int64(2 * time.Second)))})
			}
			p.queue = append(p.queue, simtest.Request{Suite: suites[j], Anchor: true})
		}
		plans[i] = p
	}
	flips := make([][2]time.Duration, len(suites))
	for i := range flips {
		first := 20*time.Second + time.Duration(rng.Int64N(int64(60*time.Second)))
		flips[i] = [2]time.Duration{first, first + 30*time.Second + time.Duration(rng.Int64N(int64(60*time.Second)))}
	}

	synctest.Test(t, func(t *testing.T) {
		s := newFlipStack(t, 10*time.Second)
		for _, su := range suites {
			s.adopt(su, 1)
		}
		before := s.gate.Stats()
		l := simtest.NewLedger(10 * time.Second)
		var wg sync.WaitGroup
		t0 := time.Now()
		for i, p := range plans {
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(p.at)
				l.RunUpdate(i, p.queue, s.serve(p.transfer, 10*time.Millisecond))
			}()
		}
		for i, su := range suites {
			wg.Add(1)
			go func() { // a suite's flips run one after the other
				defer wg.Done()
				for n, at := range flips[i] {
					time.Sleep(time.Until(t0.Add(at)))
					s.adopt(su, n+2)
				}
			}()
		}
		wg.Wait()

		st := s.gate.Stats()
		if st.Holds-before.Holds != 6 || st.HoldsExpired != before.HoldsExpired || st.HoldsSkipped != before.HoldsSkipped {
			t.Fatalf("stats %+v (before %+v): want six holds, none expired or skipped", st, before)
		}
		if got := l.Straddles(); len(got) != 0 {
			t.Fatalf("%d straddles, first %+v", len(got), got[0])
		}
		for _, su := range suites {
			if gens := l.AnchorGens(su); len(gens) != 3 {
				t.Errorf("%s: clients saw anchor generations %v, want three (they crossed both flips)", su, gens)
			}
		}
		if n := l.Checked(); n < 200 {
			t.Errorf("only %d index responses checked", n)
		}
	})
}
