package flipgate

import (
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// gateModel is the cache as the barrier sees it (spec C8): suites whose
// generation a commit bumps; anchor serves that register with the gate
// before reading the generation (the handler's order) and stay in flight
// for their transfer; index serves that never wait.
type gateModel struct {
	g    *Gate
	gens map[string]*atomic.Int64
}

func newGateModel(g *Gate, keys ...string) *gateModel {
	m := &gateModel{g: g, gens: map[string]*atomic.Int64{}}
	for _, k := range keys {
		m.gens[k] = &atomic.Int64{}
		m.gens[k].Store(1)
	}
	return m
}

// serve answers ledger requests; an anchor of key takes anchor[key] to
// send, an index 10 ms.
func (m *gateModel) serve(anchor map[string]time.Duration) simtest.ServeFunc {
	return func(rq simtest.Request) (string, time.Duration) {
		gen := m.gens[rq.Suite]
		if !rq.Anchor {
			v := gen.Load()
			time.Sleep(10 * time.Millisecond)
			return strconv.FormatInt(v, 10), 0
		}
		start := time.Now()
		tok := m.g.AnchorStart(bg, rq.Suite)
		held := time.Since(start)
		v := gen.Load()
		time.Sleep(anchor[rq.Suite])
		tok.Done()
		return strconv.FormatInt(v, 10), held
	}
}

// commit is a flip's commit: the next generation becomes current.
func (m *gateModel) commit(key string) func() error {
	return func() error { m.gens[key].Add(1); return nil }
}

// clients starts, every period from offset until until, a client that
// runs one update of queue.
func clients(l *simtest.Ledger, serve simtest.ServeFunc, queue []simtest.Request, offset, period time.Duration, until time.Time, wg *sync.WaitGroup) {
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

// recordHeld wraps serve and records how long each anchor was held.
func recordHeld(serve simtest.ServeFunc, held *[]time.Duration) simtest.ServeFunc {
	return func(rq simtest.Request) (string, time.Duration) {
		gen, h := serve(rq)
		if rq.Anchor {
			*held = append(*held, h)
		}
		return gen, h
	}
}

// Spec C8 DoD, the review's overlapping-hold trace (plan review
// 2026-10-05, finding 6) in a shared-queue simulation. Busy suites A and
// B leave phase A at its cap 0.1 s apart. An apt client whose single
// queue serves both (mirror mode) receives A's old anchor just before
// A's hold starts, spends 0.3 s on an unrelated download, then asks for
// B's anchor with A's index queued behind it. Under concurrent holds B
// would hold that request until B committed — after A — and the client
// would fetch A's new index for A's old anchor ("another existing B
// anchor response" on a slow link keeps B from committing early). With
// one hold at a time B is not holding yet, so neither of the client's
// anchors waits and its A index arrives in time. Many more shared-queue
// clients run across both flips.
func TestSimFlip_OverlappingHoldTrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		m := newGateModel(g, "A", "B")
		fast := m.serve(map[string]time.Duration{"A": 50 * time.Millisecond, "B": 50 * time.Millisecond})
		l := simtest.NewLedger(testQuiet)
		var wg sync.WaitGroup
		t0 := time.Now()
		until := t0.Add(111 * time.Second)
		clients(l, fast, []simtest.Request{{Suite: "A", Anchor: true}}, 11*time.Millisecond, 500*time.Millisecond, until, &wg)
		clients(l, fast, []simtest.Request{{Suite: "B", Anchor: true}}, 261*time.Millisecond, 500*time.Millisecond, until, &wg)
		wg.Add(1)
		go func() { // another B reader, on a slow link, still reading when B's hold could start
			defer wg.Done()
			time.Sleep(60950 * time.Millisecond)
			l.RunUpdate(1000, []simtest.Request{{Suite: "B", Anchor: true}}, m.serve(map[string]time.Duration{"B": 6 * time.Second}))
		}()
		var traceHeld []time.Duration
		wg.Add(1)
		go func() { // the trace client
			defer wg.Done()
			time.Sleep(60853 * time.Millisecond)
			l.RunUpdate(2000, []simtest.Request{{Suite: "A", Anchor: true}, {Filler: 300 * time.Millisecond}, {Suite: "B", Anchor: true}},
				recordHeld(fast, &traceHeld))
		}()
		for i := 0; i < 150; i++ { // shared-queue clients, both orders
			q := []simtest.Request{{Suite: "A", Anchor: true}, {Suite: "B", Anchor: true}}
			if i%2 == 1 {
				q[0], q[1] = q[1], q[0]
			}
			at := 51*time.Second + 7*time.Millisecond + time.Duration(i)*300*time.Millisecond
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(at)
				l.RunUpdate(3000+i, q, fast)
			}()
		}
		time.Sleep(time.Second + 3*time.Millisecond) // both suites are busy by now
		outA := flipAsync(bg, g, "A", m.commit("A"))
		time.Sleep(100 * time.Millisecond)
		outB := flipAsync(bg, g, "B", m.commit("B"))
		a, b := <-outA, <-outB
		wg.Wait()

		if a.err != nil || b.err != nil || a.res.Outcome != OutcomeHeld || b.res.Outcome != OutcomeHeld {
			t.Fatalf("A: %+v, %v; B: %+v, %v; want both held", a.res, a.err, b.res, b.err)
		}
		if a.res.PhaseA != PhaseAMax || b.res.PhaseA != PhaseAMax {
			t.Fatalf("phase A took %v and %v, want both to give up at %v (busy suites)", a.res.PhaseA, b.res.PhaseA, PhaseAMax)
		}
		if b.res.HoldStart != a.res.HoldEnd || b.at.Sub(a.res.HoldEnd) < testQuiet {
			t.Fatalf("B held from %v and committed %v after A's hold ended (%v); want B's hold to start then and commit a quiet period later",
				b.res.HoldStart, b.at.Sub(a.res.HoldEnd), a.res.HoldEnd)
		}
		if len(traceHeld) != 2 || traceHeld[0] != 0 || traceHeld[1] != 0 {
			t.Fatalf("the trace client's anchors were held %v, want neither", traceHeld)
		}
		if s := l.Straddles(); len(s) != 0 {
			t.Fatalf("%d straddles, first %+v", len(s), s[0])
		}
		for _, k := range []string{"A", "B"} {
			if gens := l.AnchorGens(k); !slices.Equal(gens, []string{"1", "2"}) {
				t.Errorf("%s: clients saw anchor generations %v, want [1 2] (they crossed the flip)", k, gens)
			}
		}
		if n := l.Checked(); n < 400 {
			t.Errorf("only %d index responses checked", n)
		}
	})
}

// Spec C8 phase B (b): a client released from another suite's hold may
// still have this suite's index queued behind the held request. A's hold
// lasts ~15 s (its readers are on 6 s links); B leaves phase A at its cap
// while A holds, so it waits for the token. The trace client gets B's old
// anchor, then waits in A's hold for A's anchor, then asks for B's index
// — 18.5 s after B's anchor arrived, 12.5 s of that in A's hold. B is
// quiet by itself when A's hold ends; only the cross-suite quiet period
// keeps it from committing before that index arrives.
func TestSimFlip_ClientReleasedFromAnotherHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		m := newGateModel(g, "A", "B")
		serve := m.serve(map[string]time.Duration{"A": 6 * time.Second, "B": 50 * time.Millisecond})
		l := simtest.NewLedger(testQuiet)
		var wg sync.WaitGroup
		t0 := time.Now()
		clients(l, serve, []simtest.Request{{Suite: "A", Anchor: true}}, 11*time.Millisecond, 500*time.Millisecond, t0.Add(100*time.Second), &wg)
		clients(l, serve, []simtest.Request{{Suite: "B", Anchor: true}}, 261*time.Millisecond, 500*time.Millisecond, t0.Add(61*time.Second), &wg)
		var traceHeld []time.Duration
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(64003 * time.Millisecond)
			l.RunUpdate(2000, []simtest.Request{{Suite: "B", Anchor: true}, {Suite: "A", Anchor: true}}, recordHeld(serve, &traceHeld))
		}()
		time.Sleep(time.Second + 3*time.Millisecond) // both suites are busy by now
		outA := flipAsync(bg, g, "A", m.commit("A")) // holds from 61.003 s to 76.511 s
		time.Sleep(2 * time.Second)
		outB := flipAsync(bg, g, "B", m.commit("B")) // phase A gives up at 63.003 s, while A holds
		a, b := <-outA, <-outB
		wg.Wait()

		if a.err != nil || b.err != nil {
			t.Fatalf("A: %v; B: %v", a.err, b.err)
		}
		if len(traceHeld) != 2 || traceHeld[0] != 0 || traceHeld[1] == 0 {
			t.Fatalf("the trace client's anchors were held %v, want B's not held and A's held", traceHeld)
		}
		if b.res.HoldStart != a.res.HoldEnd || b.at.Sub(a.res.HoldEnd) != testQuiet {
			t.Fatalf("B held from %v and committed %v after A's hold ended; want its hold to start at A's end and commit the quiet period later",
				b.res.HoldStart, b.at.Sub(a.res.HoldEnd))
		}
		if s := l.Straddles(); len(s) != 0 {
			t.Fatalf("%d straddles, first %+v", len(s), s[0])
		}
	})
}

// Spec C8 DoD "five busy suites flipping within 0.1 s" (plan deviation
// 2). Each suite is read by a client every 500 ms over a slow link: an
// anchor takes 9.02 s to send, so a hold waits ~9 s for the reads in
// flight plus the 10 s quiet period (~19 s, near the 20 s bound). No
// suite is ever quiet, so all five leave phase A at its 60 s cap, 20 ms
// apart, and queue for the hold token in that order. Holds never overlap
// and end normally; four fit inside the fifth's 60 s token wait (three
// holds of under 20 s cannot exhaust a fourth's wait); the fifth commits
// unheld at its deadline and is counted; held suites never straddle.
func TestSimFlip_FiveBusySuites(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		keys := []string{"s1", "s2", "s3", "s4", "s5"}
		m := newGateModel(g, keys...)
		slow := map[string]time.Duration{}
		for _, k := range keys {
			slow[k] = 9020 * time.Millisecond
		}
		serve := m.serve(slow)
		l := simtest.NewLedger(testQuiet)
		skipped := counterValue(t, "acu_flip_hold_skipped_total")
		var wg sync.WaitGroup
		t0 := time.Now()
		until := t0.Add(150 * time.Second)
		for i, k := range keys {
			clients(l, serve, []simtest.Request{{Suite: k, Anchor: true}}, time.Duration(13*(i+1))*time.Millisecond, 500*time.Millisecond, until, &wg)
		}
		outs := make([]<-chan flipOut, len(keys))
		starts := make([]time.Time, len(keys))
		for i, k := range keys {
			time.Sleep(time.Until(t0.Add(time.Second + time.Duration(5+20*i)*time.Millisecond))) // the suites are busy by now
			starts[i] = time.Now()
			outs[i] = flipAsync(bg, g, k, m.commit(k))
		}
		res := make([]Result, len(keys))
		ends := make([]time.Time, len(keys))
		for i, ch := range outs {
			o := <-ch
			if o.err != nil {
				t.Fatalf("%s: %v", keys[i], o.err)
			}
			res[i], ends[i] = o.res, o.at
		}
		wg.Wait()

		for i := 0; i < 4; i++ {
			if res[i].Outcome != OutcomeHeld {
				t.Errorf("%s: outcome %s, want held", keys[i], res[i].Outcome)
			}
			if d := res[i].HoldEnd.Sub(res[i].HoldStart); d <= 0 || d > HoldMax {
				t.Errorf("%s held %v, want (0, %v]", keys[i], d, HoldMax)
			}
			if i > 0 && res[i].HoldStart.Before(res[i-1].HoldEnd) {
				t.Errorf("holds overlap: %s from %v, %s until %v", keys[i], res[i].HoldStart, keys[i-1], res[i-1].HoldEnd)
			}
		}
		r5 := res[4]
		if r5.Outcome != OutcomeHoldSkipped || r5.TokenWait != TokenWaitMax || !r5.HoldStart.IsZero() {
			t.Fatalf("s5: %+v, want an unheld commit after the %v token wait", r5, TokenWaitMax)
		}
		if got := ends[4].Sub(starts[4]); got != PhaseAMax+TokenWaitMax {
			t.Errorf("s5 committed %v after its flip started, want %v", got, PhaseAMax+TokenWaitMax)
		}
		if got := ends[4].Sub(res[0].HoldStart); got > TokenWaitMax+HoldMax {
			t.Errorf("s5 committed %v after the first hold started, want at most %v", got, TokenWaitMax+HoldMax)
		}
		for i, k := range keys {
			if d := ends[i].Sub(starts[i]); d > PhaseAMax+TokenWaitMax+HoldMax {
				t.Errorf("%s's flip took %v, more than %v", k, d, PhaseAMax+TokenWaitMax+HoldMax)
			}
		}
		if st := g.Stats(); st.Holds != 4 || st.HoldsSkipped != 1 || st.HoldsExpired != 0 {
			t.Errorf("stats = %+v, want 4 holds, 1 skipped, 0 expired", st)
		}
		if got := counterValue(t, "acu_flip_hold_skipped_total") - skipped; got != 1 {
			t.Errorf("acu_flip_hold_skipped_total rose by %v, want 1", got)
		}
		for _, s := range l.Straddles() {
			if s.Suite != "s5" {
				t.Errorf("held suite %s straddled: %+v", s.Suite, s)
			}
		}
		for _, k := range keys[:4] {
			if gens := l.AnchorGens(k); !slices.Equal(gens, []string{"1", "2"}) {
				t.Errorf("%s: clients saw anchor generations %v, want [1 2]", k, gens)
			}
		}
	})
}
