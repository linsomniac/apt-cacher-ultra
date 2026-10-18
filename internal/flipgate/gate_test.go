package flipgate

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
)

// testQuiet is the default adoption.flip_quiet_period.
const testQuiet = 10 * time.Second

var bg = context.Background()

func newGate(t *testing.T, quiet time.Duration) *Gate {
	t.Helper()
	g, err := New(Config{Quiet: quiet})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func newTestGate(t *testing.T) *Gate {
	t.Helper()
	return newGate(t, testQuiet)
}

func noCommit() error { return nil }

// flipOut is one Flip's result and when it returned.
type flipOut struct {
	res Result
	err error
	at  time.Time
}

// flipAsync runs Flip in its own goroutine.
func flipAsync(ctx context.Context, g *Gate, key string, commit func() error) <-chan flipOut {
	ch := make(chan flipOut, 1)
	go func() {
		res, err := g.Flip(ctx, key, commit)
		ch <- flipOut{res: res, err: err, at: time.Now()}
	}()
	return ch
}

// busyReaders starts an anchor read of key every period, from offset
// until until, each taking transfer.
func busyReaders(g *Gate, key string, offset, period, transfer time.Duration, until time.Time, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(offset)
		for time.Now().Before(until) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				tok := g.AnchorStart(bg, key)
				time.Sleep(transfer)
				tok.Done()
			}()
			time.Sleep(period)
		}
	}()
}

// counterValue reads an unlabelled counter from the default registry (0
// before its first increment).
func counterValue(t *testing.T, name string) float64 {
	t.Helper()
	var buf bytes.Buffer
	metrics.Default.Render(&buf)
	for _, line := range strings.Split(buf.String(), "\n") {
		if v, ok := strings.CutPrefix(line, name+" "); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				t.Fatal(err)
			}
			return f
		}
	}
	return 0
}

// adoption.flip_quiet_period is 0s or within [1s, 15s] (controller
// ruling): a sub-second period gives almost no protection.
func TestNew_QuietBounds(t *testing.T) {
	for _, q := range []time.Duration{-time.Second, time.Nanosecond, MinQuiet - time.Nanosecond, MaxQuiet + time.Nanosecond} {
		if _, err := New(Config{Quiet: q}); !errors.Is(err, ErrQuietOutOfRange) {
			t.Errorf("Quiet %v: err = %v, want ErrQuietOutOfRange", q, err)
		}
	}
	for _, q := range []time.Duration{0, MinQuiet, MaxQuiet} {
		if _, err := New(Config{Quiet: q}); err != nil {
			t.Errorf("Quiet %v: %v", q, err)
		}
	}
	// Phase B (b) can always be satisfied inside a hold only because a
	// whole quiet period fits in one (design review M18) — before the
	// commit margin, so that every hold commits by HoldMax − CommitMargin
	// and answers its held requests from the new snapshot (final review,
	// concurrency Important 1).
	if MaxQuiet >= HoldMax {
		t.Errorf("MaxQuiet %v must stay below HoldMax %v", MaxQuiet, HoldMax)
	}
	if MaxQuiet > HoldMax-CommitMargin {
		t.Errorf("MaxQuiet %v must not exceed HoldMax %v − CommitMargin %v", MaxQuiet, HoldMax, CommitMargin)
	}
}

// The gate is keyed by the freshness checker's suite key, so the reaper
// (phase F) can purge an entry with the key it already has.
func TestKey_IsTheFreshnessSuiteKey(t *testing.T) {
	if got := Key("http", "h", "/r/dists/x"); got != "http|h|/r/dists/x" {
		t.Fatalf("Key = %q, want http|h|/r/dists/x", got)
	}
}

// flip_quiet_period = 0s is today's behaviour (spec C8): nothing waits,
// nothing is tracked. A nil gate behaves the same.
func TestDisabledGate_IsTodaysBehaviour(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for name, g := range map[string]*Gate{"nil gate": nil, "quiet 0s": newGate(t, 0)} {
			if g.Enabled() {
				t.Fatalf("%s: enabled", name)
			}
			tok := g.AnchorStart(bg, "k")
			if tok != nil {
				t.Fatalf("%s: AnchorStart registered a serve", name)
			}
			tok.Done()
			start := time.Now()
			committed := false
			res, err := g.Flip(bg, "k", func() error { committed = true; return nil })
			if err != nil || !committed || res.Outcome != OutcomeDisabled || time.Since(start) != 0 {
				t.Fatalf("%s: Flip = %+v, %v, committed %v after %v; want an immediate commit", name, res, err, committed, time.Since(start))
			}
			if g.Len() != 0 {
				t.Fatalf("%s: %d entries, want 0", name, g.Len())
			}
		}
	})
}

// Spec §7 C8 "idle zero-wait": a suite nobody is reading flips at once.
func TestFlip_IdleSuiteCommitsAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		start := time.Now()
		res, err := g.Flip(bg, "k", noCommit)
		if err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d != 0 {
			t.Fatalf("idle flip took %v, want 0", d)
		}
		if res.Outcome != OutcomeHeld || !res.Committed || res.HoldStart.IsZero() || res.HoldEnd != res.HoldStart {
			t.Fatalf("result = %+v, want a zero-length hold that committed", res)
		}
	})
}

// Phase A (spec C8): the flip waits until no anchor of the suite is in
// flight and none completed within the quiet period.
func TestFlip_WaitsForQuietAfterTheLastAnchor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		tok := g.AnchorStart(bg, "k")
		start := time.Now()
		out := flipAsync(bg, g, "k", noCommit)
		time.Sleep(4 * time.Second)
		synctest.Wait()
		select {
		case o := <-out:
			t.Fatalf("committed while an anchor was in flight: %+v", o.res)
		default:
		}
		tok.Done()
		o := <-out
		if o.err != nil {
			t.Fatal(o.err)
		}
		if got := o.at.Sub(start); got != 14*time.Second {
			t.Fatalf("flip returned after %v, want 14s (4s in flight + the 10s quiet period)", got)
		}
		if o.res.PhaseA != 14*time.Second {
			t.Fatalf("phase A took %v, want 14s", o.res.PhaseA)
		}
	})
}

// Design note 2: an anchor response still being sent 30 s after it
// started is treated as abandoned — the barrier stops waiting for it, so
// one slow link cannot stall a flip or force a hold to expire — and is
// counted (spec C8 residual). Its completion still counts while no hold of
// the suite is in progress, but not during a hold.
func TestFlip_IgnoresAnchorsInFlightPastAptsTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		slowReads := counterValue(t, "acu_flip_slow_anchor_reads_total")
		slow := g.AnchorStart(bg, "k")
		start := time.Now()
		if _, err := g.Flip(bg, "k", noCommit); err != nil {
			t.Fatal(err)
		}
		if got := time.Since(start); got != AnchorTimeout {
			t.Fatalf("flip returned after %v, want %v (the in-flight read is ignored then)", got, AnchorTimeout)
		}
		time.Sleep(15 * time.Second)
		slow.Done() // completes 45 s after it started, no hold in progress: it counts
		start = time.Now()
		if _, err := g.Flip(bg, "k", noCommit); err != nil {
			t.Fatal(err)
		}
		if got := time.Since(start); got != testQuiet {
			t.Fatalf("a completion after apt's timeout, outside a hold, delayed the next flip by %v, want %v", got, testQuiet)
		}
		slow = g.AnchorStart(bg, "k") // 55 s
		slowStart := time.Now()
		release := make(chan struct{})
		out := flipAsync(bg, g, "k", func() error { <-release; return nil }) // holds from 85 s, when slow is ignored
		time.Sleep(40 * time.Second)
		slow.Done()    // 95 s, during the hold: ignored
		close(release) // the hold ends at 95 s
		if o := <-out; o.err != nil || o.res.HoldStart.Sub(slowStart) != AnchorTimeout {
			t.Fatalf("flip = %+v, %v; want a hold from %v after the read started", o.res, o.err, AnchorTimeout)
		}
		start = time.Now()
		if _, err := g.Flip(bg, "k", noCommit); err != nil {
			t.Fatal(err)
		}
		if got := time.Since(start); got != 0 {
			t.Fatalf("a completion after apt's timeout, during a hold, delayed the next flip by %v", got)
		}
		if got := counterValue(t, "acu_flip_slow_anchor_reads_total") - slowReads; got != 2 {
			t.Fatalf("acu_flip_slow_anchor_reads_total rose by %v, want 2", got)
		}
	})
}

// A busy suite never goes quiet: phase A gives up after 60 s; the hold
// then holds new anchor requests, commits once the suite is quiet, and
// answers the held requests from the new generation (spec C8).
func TestFlip_BusySuiteIsHeldAndAnsweredFromTheNewGeneration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		var gen atomic.Int64
		gen.Store(1)
		type served struct {
			start, done time.Time
			gen         int64
		}
		var mu sync.Mutex
		var reads []served
		var wg sync.WaitGroup
		until := time.Now().Add(100 * time.Second)
		wg.Add(1)
		go func() { // a client every 500 ms, each anchor read taking 100 ms
			defer wg.Done()
			for time.Now().Before(until) {
				wg.Add(1)
				go func() {
					defer wg.Done()
					start := time.Now()
					tok := g.AnchorStart(bg, "k")
					v := gen.Load()
					time.Sleep(100 * time.Millisecond)
					tok.Done()
					mu.Lock()
					reads = append(reads, served{start: start, done: time.Now(), gen: v})
					mu.Unlock()
				}()
				time.Sleep(500 * time.Millisecond)
			}
		}()
		time.Sleep(250 * time.Millisecond) // off the request grid
		flipStart := time.Now()
		var committedAt time.Time
		res, err := g.Flip(bg, "k", func() error { committedAt = time.Now(); gen.Store(2); return nil })
		if err != nil {
			t.Fatal(err)
		}
		if res.PhaseA != PhaseAMax || res.Outcome != OutcomeHeld {
			t.Fatalf("result = %+v, want phase A to give up after %v and the hold to end normally", res, PhaseAMax)
		}
		// The last read before the hold started at 60.0 s and ended at
		// 60.1 s; the commit follows the 10 s quiet period.
		if got, want := committedAt.Sub(flipStart), PhaseAMax+9850*time.Millisecond; got != want {
			t.Fatalf("committed %v after the flip started, want %v", got, want)
		}
		wg.Wait()
		held := 0
		for _, s := range reads {
			switch {
			case s.start.Before(res.HoldStart):
				if s.gen != 1 {
					t.Errorf("a read that started before the hold saw generation %d", s.gen)
				}
			case s.start.Before(committedAt):
				held++
				if s.gen != 2 {
					t.Errorf("a held read saw generation %d, want 2 (the new snapshot)", s.gen)
				}
				if w := s.done.Sub(s.start) - 100*time.Millisecond; w > HoldMax {
					t.Errorf("a read was held %v, more than %v", w, HoldMax)
				}
			}
		}
		if held == 0 || res.Waiters != held {
			t.Fatalf("%d reads were held, Result.Waiters = %d", held, res.Waiters)
		}
	})
}

// Phase B is bounded by 20 s (spec §7 C8 "slow reader"): a read slower
// than the hold (but faster than apt's timeout) is in flight when the
// hold starts, so the suite never goes quiet. No other hold ended, so
// phase B (b) holds; the hold waits for local quiet only until HoldMax −
// CommitMargin (15 s), then commits inside the hold and answers the held
// requests from the new snapshot. The slow reader is the late reader: the
// counted residual (acu_flip_quiet_cut_short_total); the hold never
// reaches its bound.
func TestFlip_SlowReaderCutsTheQuietShortInsideTheHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		expired := counterValue(t, "acu_flip_hold_expired_total")
		cutShort := counterValue(t, "acu_flip_quiet_cut_short_total")
		var wg sync.WaitGroup
		for _, at := range []time.Duration{0, 20 * time.Second, 40 * time.Second, 59500 * time.Millisecond} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(at)
				tok := g.AnchorStart(bg, "k")
				time.Sleep(25 * time.Second)
				tok.Done()
			}()
		}
		waited := make(chan time.Duration, 1)
		go func() { // arrives during the hold
			time.Sleep(70 * time.Second)
			start := time.Now()
			g.AnchorStart(bg, "k").Done()
			waited <- time.Since(start)
		}()
		synctest.Wait() // the first slow read is registered
		start := time.Now()
		var committedAt time.Time
		res, err := g.Flip(bg, "k", func() error { committedAt = time.Now(); return nil })
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != OutcomeQuietCutShort || !res.Committed || res.HoldEnd.Sub(res.HoldStart) != HoldMax-CommitMargin {
			t.Fatalf("result = %+v, want a hold that committed after %v, before the suite was quiet", res, HoldMax-CommitMargin)
		}
		if got := committedAt.Sub(start); got != PhaseAMax+HoldMax-CommitMargin {
			t.Fatalf("committed %v after the start, want %v", got, PhaseAMax+HoldMax-CommitMargin)
		}
		if w := <-waited; w != 5*time.Second {
			t.Fatalf("the held request waited %v, want 5s (until the commit at 15s into the hold)", w)
		}
		if st := g.Stats(); st.HoldsExpired != 0 || st.QuietCutShort != 1 {
			t.Fatalf("stats = %+v, want one hold cut short and none expired", st)
		}
		if got := counterValue(t, "acu_flip_quiet_cut_short_total") - cutShort; got != 1 {
			t.Fatalf("acu_flip_quiet_cut_short_total rose by %v, want 1", got)
		}
		if got := counterValue(t, "acu_flip_hold_expired_total") - expired; got != 0 {
			t.Fatalf("acu_flip_hold_expired_total rose by %v, want 0", got)
		}
		wg.Wait()
	})
}

// The expiry stays the backstop for a commit that has not started by the
// bound. In production phase B (b) is always satisfied by HoldMax −
// CommitMargin (MaxQuiet ≤ HoldMax − CommitMargin); this test shortens the
// hold below the quiet period so that it is not: b's hold starts when a's
// hold, which held a request, ends, and the cross-suite quiet period
// outlasts b's 8 s hold. At the bound b's hold ends, the request waiting in
// it is answered, and the commit starts then (counted).
func TestFlip_ExpiryBackstopStartsACommitNotYetStarted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		g.phaseAMax, g.holdMax = time.Second, 8*time.Second
		expired := counterValue(t, "acu_flip_hold_expired_total")
		release := make(chan struct{})
		outA := flipAsync(bg, g, "a", func() error { <-release; return nil }) // idle: holds at once
		synctest.Wait()
		go func() { g.AnchorStart(bg, "a").Done() }() // waits in a's hold: a's hold held a request
		time.Sleep(500 * time.Millisecond)
		outB := flipAsync(bg, g, "b", noCommit) // phase A gives up at 1.5 s; then it queues for the token
		time.Sleep(2500 * time.Millisecond)
		close(release) // a's hold ends at 3 s; b's starts then
		waited := make(chan time.Duration, 1)
		go func() {
			time.Sleep(2 * time.Second)
			start := time.Now()
			g.AnchorStart(bg, "b").Done() // 5 s: waits in b's hold
			waited <- time.Since(start)
		}()
		if a := <-outA; a.err != nil || a.res.Outcome != OutcomeHeld {
			t.Fatalf("a: %+v, %v; want a held commit", a.res, a.err)
		}
		b := <-outB
		if b.err != nil || b.res.Outcome != OutcomeHoldExpired || !b.res.Committed {
			t.Fatalf("b: %+v, %v; want a hold that expired before its commit started", b.res, b.err)
		}
		if b.res.HoldEnd.Sub(b.res.HoldStart) != 8*time.Second || b.at.Sub(b.res.HoldStart) != 8*time.Second {
			t.Fatalf("b held from %v until %v and committed at %v; want the hold to end, and the commit to start, at its 8s bound",
				b.res.HoldStart, b.res.HoldEnd, b.at)
		}
		if w := <-waited; w != 6*time.Second {
			t.Fatalf("the request held in b's hold waited %v, want 6s (until the bound)", w)
		}
		if got := counterValue(t, "acu_flip_hold_expired_total") - expired; got != 1 {
			t.Fatalf("acu_flip_hold_expired_total rose by %v, want 1", got)
		}
	})
}

// A commit slower than the hold: the hold still ends at 20 s — pending is
// cleared and held requests are answered from the snapshot current then
// (the old one) — and the commit is never interrupted (spec C8 residual,
// F-2 on very large suites).
func TestFlip_SlowCommitEndsTheHoldAt20s(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		var gen atomic.Int64
		gen.Store(1)
		seen := make(chan int64, 1)
		go func() {
			time.Sleep(5 * time.Second)
			tok := g.AnchorStart(bg, "k") // waits in the hold
			seen <- gen.Load()
			tok.Done()
		}()
		start := time.Now()
		res, err := g.Flip(bg, "k", func() error {
			time.Sleep(25 * time.Second)
			gen.Store(2)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if v := <-seen; v != 1 {
			t.Fatalf("a request released when the hold expired saw generation %d, want 1 (the commit had not finished)", v)
		}
		if res.Outcome != OutcomeHoldExpired || res.HoldEnd.Sub(res.HoldStart) != HoldMax || time.Since(start) != 25*time.Second {
			t.Fatalf("result = %+v after %v, want the hold to end at %v and the commit to finish at 25s", res, time.Since(start), HoldMax)
		}
	})
}

// One hold at a time, granted in request order; the next hold starts
// when the previous one ends and commits no sooner than the quiet period
// after that end. b's own readers stop at 55 s, so when its hold starts
// (~70 s) only the cross-suite rule keeps it from committing at once:
// a's hold held requests, and clients released from it may still have
// b's requests queued behind them (spec C8 phase B (b)). b's hold holds
// nothing, so it starts no cross-suite quiet period (design note 17); c
// is kept a quiet period past b's end by its own reads, still in flight
// when its hold starts.
func TestFlip_HoldsAreExclusiveFIFOAndQuietAfterEachOther(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		var wg sync.WaitGroup
		t0 := time.Now()
		keys := []string{"a", "b", "c"}
		for i, k := range keys {
			until := t0.Add(120 * time.Second)
			if k == "b" {
				until = t0.Add(55 * time.Second)
			}
			busyReaders(g, k, time.Duration(7*i)*time.Millisecond, 500*time.Millisecond, 50*time.Millisecond, until, &wg)
		}
		var outs []<-chan flipOut
		for _, k := range keys {
			time.Sleep(100 * time.Millisecond) // a, b, c ask for the token 0.1 s apart
			outs = append(outs, flipAsync(bg, g, k, noCommit))
		}
		var res []Result
		for i, ch := range outs {
			o := <-ch
			if o.err != nil || o.res.Outcome != OutcomeHeld {
				t.Fatalf("%s: %+v, %v", keys[i], o.res, o.err)
			}
			res = append(res, o.res)
		}
		for i := 1; i < len(res); i++ {
			if res[i].HoldStart != res[i-1].HoldEnd {
				t.Errorf("%s's hold started at %v, want %s's hold end %v (exclusive, in token order)",
					keys[i], res[i].HoldStart, keys[i-1], res[i-1].HoldEnd)
			}
			if d := res[i].HoldEnd.Sub(res[i-1].HoldEnd); d < testQuiet {
				t.Errorf("%s committed %v after %s's hold ended, want at least the quiet period", keys[i], d, keys[i-1])
			}
		}
		wg.Wait()
	})
}

// While another suite holds, a flip stays in phase A until that hold has
// ended and the quiet period has passed, so its own hold is short (spec
// C8 phase A (b)).
func TestFlip_PhaseAWaitsOutAnotherSuitesHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		var wg sync.WaitGroup
		busyReaders(g, "a", 0, 500*time.Millisecond, 50*time.Millisecond, time.Now().Add(90*time.Second), &wg)
		time.Sleep(250 * time.Millisecond)
		outA := flipAsync(bg, g, "a", noCommit) // holds from 60.25 s
		time.Sleep(65 * time.Second)
		resB, err := g.Flip(bg, "b", noCommit) // b is idle
		if err != nil {
			t.Fatal(err)
		}
		a := <-outA
		if resB.HoldStart.Before(a.res.HoldEnd.Add(testQuiet)) {
			t.Errorf("b's hold started %v after a's ended, want at least the quiet period",
				resB.HoldStart.Sub(a.res.HoldEnd))
		}
		if d := resB.HoldEnd.Sub(resB.HoldStart); d != 0 {
			t.Errorf("b held its anchors for %v; phase A should have waited out the quiet period instead", d)
		}
		wg.Wait()
	})
}

// A flip that cannot get the hold token within the wait commits without
// a hold and is counted (spec C8 residual). The production wait is 60 s;
// TestSimFlip_FiveBusySuites runs it.
func TestFlip_HoldTokenTimeoutCommitsUnheld(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		g.phaseAMax, g.tokenWait = time.Second, 5*time.Second
		skipped := counterValue(t, "acu_flip_hold_skipped_total")
		var wg sync.WaitGroup
		until := time.Now().Add(30 * time.Second)
		busyReaders(g, "a", 0, 500*time.Millisecond, 50*time.Millisecond, until, &wg)
		busyReaders(g, "b", 7*time.Millisecond, 500*time.Millisecond, 50*time.Millisecond, until, &wg)
		wg.Add(1)
		go func() { // a read slower than the hold keeps a's hold going until it is cut short
			defer wg.Done()
			time.Sleep(900 * time.Millisecond)
			tok := g.AnchorStart(bg, "a")
			time.Sleep(25 * time.Second)
			tok.Done()
		}()
		synctest.Wait()                         // the first reads are registered
		outA := flipAsync(bg, g, "a", noCommit) // phase A gives up at 1 s; holds 1 s..16 s
		time.Sleep(10 * time.Millisecond)
		start := time.Now()
		res, err := g.Flip(bg, "b", noCommit) // phase A until 1.01 s, then 5 s for the token
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != OutcomeHoldSkipped || !res.Committed || res.TokenWait != 5*time.Second || !res.HoldStart.IsZero() {
			t.Fatalf("result = %+v, want an unheld commit after the 5s token wait", res)
		}
		if d := time.Since(start); d != 6*time.Second {
			t.Fatalf("b committed after %v, want 6s", d)
		}
		if got := counterValue(t, "acu_flip_hold_skipped_total") - skipped; got != 1 {
			t.Fatalf("acu_flip_hold_skipped_total rose by %v, want 1", got)
		}
		if st := g.Stats(); st.HoldsSkipped != 1 {
			t.Fatalf("stats = %+v, want one skipped hold", st)
		}
		g.mu.Lock()
		queued := len(g.queue)
		g.mu.Unlock()
		if queued != 0 {
			t.Fatalf("%d flips still queued for the token after timing out", queued)
		}
		if a := <-outA; a.err != nil || a.res.Outcome != OutcomeQuietCutShort {
			t.Fatalf("a: %+v, %v; want its hold to run until it is cut short", a.res, a.err)
		}
		wg.Wait()
	})
}

// At most MaxWaiters requests wait in one hold; the next one is served at
// once (spec C8 waiter cap). All waiters are answered when the hold ends.
func TestAnchorStart_WaiterCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		release := make(chan struct{})
		out := flipAsync(bg, g, "k", func() error { <-release; return nil }) // idle: holds at once
		synctest.Wait()
		var returned atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < MaxWaiters; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				g.AnchorStart(bg, "k").Done()
				returned.Add(1)
			}()
		}
		synctest.Wait()
		if n := returned.Load(); n != 0 {
			t.Fatalf("%d of %d waiters returned during the hold", n, MaxWaiters)
		}
		overCap := counterValue(t, "acu_flip_waiters_over_cap_total")
		extra := g.AnchorStart(bg, "k") // beyond the cap: served at once
		extra.Done()
		if st := g.Stats(); st.HeldRequests != MaxWaiters {
			t.Fatalf("stats = %+v, want %d held requests", st, MaxWaiters)
		}
		if got := counterValue(t, "acu_flip_waiters_over_cap_total") - overCap; got != 1 {
			t.Fatalf("acu_flip_waiters_over_cap_total rose by %v, want 1", got)
		}
		close(release)
		wg.Wait()
		if n := returned.Load(); n != MaxWaiters {
			t.Fatalf("%d waiters answered after the hold, want %d", n, MaxWaiters)
		}
		if o := <-out; o.err != nil {
			t.Fatal(o.err)
		}
	})
}

// A held request whose client goes away stops waiting at once and is not
// counted: its read never happens (spec §7 C8 "client cancel").
func TestAnchorStart_CancelledWaiterLeavesUncounted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		release := make(chan struct{})
		out := flipAsync(bg, g, "k", func() error { <-release; return nil })
		synctest.Wait()
		ctx, cancel := context.WithCancel(bg)
		got := make(chan *Token, 1)
		go func() { got <- g.AnchorStart(ctx, "k") }()
		synctest.Wait()
		cancel()
		tok := <-got
		tok.Done()
		g.mu.Lock()
		e := g.entries["k"]
		inflight, lastDone := len(e.inflight), e.lastDone
		g.mu.Unlock()
		if inflight != 0 || !lastDone.IsZero() {
			t.Fatalf("a cancelled waiter was counted: %d in flight, last completion %v", inflight, lastDone)
		}
		close(release)
		if o := <-out; o.err != nil {
			t.Fatal(o.err)
		}
	})
}

func assertAborted(t *testing.T, res Result, err error) {
	t.Helper()
	if !errors.Is(err, ErrAborted) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want ErrAborted wrapping context.Canceled", err)
	}
	if res.Outcome != OutcomeAborted || res.Committed {
		t.Fatalf("result = %+v, want aborted and not committed", res)
	}
}

// Shutdown (the flip's context ending) abandons a flip in any phase:
// nothing commits, a started hold ends and its waiters are answered, and
// the token passes on (spec C8, ruling R-C2).
func TestFlip_ContextEndAbortsEveryPhase(t *testing.T) {
	mustNotCommit := func(t *testing.T) func() error {
		return func() error { t.Error("committed after shutdown"); return nil }
	}
	t.Run("before", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			g := newTestGate(t)
			ctx, cancel := context.WithCancel(bg)
			cancel()
			res, err := g.Flip(ctx, "k", mustNotCommit(t))
			assertAborted(t, res, err)
			if st := g.Stats(); st.Holds != 0 || st.Aborted != 1 {
				t.Fatalf("stats = %+v", st)
			}
		})
	})
	t.Run("phase A", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			g := newTestGate(t)
			tok := g.AnchorStart(bg, "k")
			defer tok.Done()
			ctx, cancel := context.WithCancel(bg)
			time.AfterFunc(3*time.Second, cancel)
			start := time.Now()
			res, err := g.Flip(ctx, "k", mustNotCommit(t))
			assertAborted(t, res, err)
			if d := time.Since(start); d != 3*time.Second || g.Stats().Holds != 0 {
				t.Fatalf("aborted after %v with %d holds, want 3s and none", d, g.Stats().Holds)
			}
		})
	})
	t.Run("token wait", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			g := newTestGate(t)
			g.phaseAMax = time.Second
			var wg sync.WaitGroup
			defer wg.Wait()
			slow := g.AnchorStart(bg, "a") // keeps a's hold going until it is cut short at 16 s
			wg.Add(1)
			go func() { defer wg.Done(); time.Sleep(25 * time.Second); slow.Done() }()
			outA := flipAsync(bg, g, "a", noCommit) // holds from 1 s
			time.Sleep(500 * time.Millisecond)
			reading := g.AnchorStart(bg, "b") // b is busy: its phase A gives up at 1.5 s
			wg.Add(1)
			go func() { defer wg.Done(); time.Sleep(2 * time.Second); reading.Done() }()
			ctx, cancel := context.WithCancel(bg)
			time.AfterFunc(2500*time.Millisecond, cancel) // at 3 s, while b waits for the token
			res, err := g.Flip(ctx, "b", mustNotCommit(t))
			assertAborted(t, res, err)
			g.mu.Lock()
			queued := len(g.queue)
			g.mu.Unlock()
			if queued != 0 {
				t.Fatalf("an aborted flip stayed in the token queue (%d queued)", queued)
			}
			if a := <-outA; a.err != nil || a.res.Outcome != OutcomeQuietCutShort {
				t.Fatalf("a: %+v, %v; want it unaffected", a.res, a.err)
			}
		})
	})
	t.Run("phase B", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			g := newTestGate(t)
			g.phaseAMax = time.Second
			var wg sync.WaitGroup
			defer wg.Wait()
			slow := g.AnchorStart(bg, "k") // in flight until 5 s: phase B waits for it
			wg.Add(1)
			go func() { defer wg.Done(); time.Sleep(5 * time.Second); slow.Done() }()
			waited := make(chan time.Duration, 1)
			go func() {
				time.Sleep(2 * time.Second)
				start := time.Now()
				g.AnchorStart(bg, "k").Done()
				waited <- time.Since(start)
			}()
			ctx, cancel := context.WithCancel(bg)
			time.AfterFunc(3*time.Second, cancel)
			res, err := g.Flip(ctx, "k", mustNotCommit(t))
			assertAborted(t, res, err)
			if res.HoldStart.IsZero() {
				t.Fatal("the hold had not started; this case is meant to abort phase B")
			}
			if w := <-waited; w != time.Second {
				t.Fatalf("a held request waited %v, want 1s (released by the abort)", w)
			}
			g.mu.Lock()
			holder := g.holder
			g.mu.Unlock()
			if holder != nil {
				t.Fatal("the token is still held after the abort")
			}
		})
	})
}

// A failed commit still ends the hold: waiters are answered and the
// token is free again (spec §7 C8 "commit failure releases waiters").
func TestFlip_CommitFailureEndsTheHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		boom := errors.New("commit failed")
		waited := make(chan time.Duration, 1)
		go func() {
			time.Sleep(time.Second)
			start := time.Now()
			g.AnchorStart(bg, "k").Done()
			waited <- time.Since(start)
		}()
		res, err := g.Flip(bg, "k", func() error { time.Sleep(3 * time.Second); return boom })
		if !errors.Is(err, boom) || res.Outcome != OutcomeHeld || !res.Committed {
			t.Fatalf("Flip = %+v, %v; want the commit's error from a held flip", res, err)
		}
		if w := <-waited; w != 2*time.Second {
			t.Fatalf("the held request waited %v, want 2s", w)
		}
		g.mu.Lock()
		holder := g.holder
		g.mu.Unlock()
		if holder != nil {
			t.Fatal("the token is still held after a failed commit")
		}
	})
}

// A panicking commit still ends the hold through Flip's own defer (spec
// C5), and the panic reaches the caller (the adoption's recover).
func TestFlip_CommitPanicEndsTheHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		waited := make(chan time.Duration, 1)
		go func() {
			time.Sleep(time.Second)
			start := time.Now()
			g.AnchorStart(bg, "k").Done()
			waited <- time.Since(start)
		}()
		func() {
			defer func() {
				if r := recover(); r != "commit bug" {
					t.Fatalf("recovered %v, want the commit's panic", r)
				}
			}()
			_, _ = g.Flip(bg, "k", func() error { time.Sleep(2 * time.Second); panic("commit bug") })
		}()
		if w := <-waited; w != time.Second {
			t.Fatalf("the held request waited %v, want 1s", w)
		}
		g.mu.Lock()
		holder, flips := g.holder, g.entries["k"].flips
		g.mu.Unlock()
		if holder != nil || flips != 0 {
			t.Fatalf("after the panic: holder %v, %d flips in progress; want neither", holder, flips)
		}
	})
}

// Entries expire after twice the quiet period idle (spec C8); an entry
// with a serve in flight never does.
func TestGate_IdleEntriesExpire(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		g.AnchorStart(bg, "a").Done()        // the first call sweeps (an empty map)
		reading := g.AnchorStart(bg, "busy") // in flight for the whole test
		defer reading.Done()
		time.Sleep(19 * time.Second)
		g.AnchorStart(bg, "b").Done() // sweeps: a has been idle 19 s, under 20 s
		if n := g.Len(); n != 3 {
			t.Fatalf("%d entries at 19 s, want 3", n)
		}
		time.Sleep(12 * time.Second)
		g.AnchorStart(bg, "c").Done() // sweeps: a idle 31 s goes, b idle 12 s stays
		g.mu.Lock()
		_, a := g.entries["a"]
		_, busy := g.entries["busy"]
		n := len(g.entries)
		g.mu.Unlock()
		if a || !busy || n != 3 {
			t.Fatalf("after the second sweep: a kept %v, busy kept %v, %d entries; want a gone, busy kept, 3", a, busy, n)
		}
	})
}

// Purge drops an idle entry (phase F's reaper calls it); a token of the
// dropped instance stays harmless, and a flip in progress keeps its entry.
func TestGate_PurgeKeepsOldTokensHarmless(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		old := g.AnchorStart(bg, "k")
		if !g.Purge("k") || g.Len() != 0 {
			t.Fatalf("Purge did not drop the entry (%d left)", g.Len())
		}
		old.Done()
		if g.Len() != 0 {
			t.Fatal("a purged entry's token re-created it")
		}
		start := time.Now()
		if _, err := g.Flip(bg, "k", noCommit); err != nil || time.Since(start) != 0 {
			t.Fatalf("a fresh entry was delayed %v by a purged entry's serve (err %v)", time.Since(start), err)
		}
		release := make(chan struct{})
		out := flipAsync(bg, g, "k2", func() error { <-release; return nil })
		synctest.Wait()
		if g.Purge("k2") {
			t.Fatal("purged an entry whose flip is in progress")
		}
		close(release)
		if o := <-out; o.err != nil {
			t.Fatal(o.err)
		}
	})
}

func TestCounters_AreRegistered(t *testing.T) {
	var buf bytes.Buffer
	metrics.Default.Render(&buf)
	for _, name := range []string{"acu_flip_hold_skipped_total", "acu_flip_hold_expired_total", "acu_flip_quiet_cut_short_total",
		"acu_flip_waiters_over_cap_total", "acu_flip_slow_anchor_reads_total", "acu_flip_gate_panics_total"} {
		if !strings.Contains(buf.String(), "# TYPE "+name+" counter") {
			t.Errorf("%s is not registered", name)
		}
	}
	if !strings.Contains(buf.String(), "# TYPE acu_flip_barrier_active gauge") {
		t.Error("acu_flip_barrier_active is not registered")
	}
}

// Spec §7 C8 DoD "phase-B expiry (commit or in-flight reader > 20 s)
// counted": a commit slower than the hold is counted like a slow reader.
func TestFlip_SlowCommitExpiryIsCounted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		expired := counterValue(t, "acu_flip_hold_expired_total")
		res, err := g.Flip(bg, "k", func() error { time.Sleep(25 * time.Second); return nil })
		if err != nil || res.Outcome != OutcomeHoldExpired {
			t.Fatalf("Flip = %+v, %v; want a hold that expired during the commit", res, err)
		}
		if st := g.Stats(); st.HoldsExpired != 1 {
			t.Fatalf("stats = %+v, want one expired hold", st)
		}
		if got := counterValue(t, "acu_flip_hold_expired_total") - expired; got != 1 {
			t.Fatalf("acu_flip_hold_expired_total rose by %v, want 1", got)
		}
	})
}

// A grant that lands at the instant the token wait times out is taken (or
// passed on), never leaked: once both flips have returned the token is free
// and nothing is queued. The two events share one virtual instant, so the
// order is up to the scheduler; 200 rounds make the late-grant path all but
// certain.
func TestFlip_GrantAtTheTokenTimeoutNeverLeaksTheHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		g.phaseAMax, g.tokenWait = 0, 5*time.Second
		for i := 0; i < 200; i++ {
			outA := flipAsync(bg, g, "a", func() error { time.Sleep(5 * time.Second); return nil }) // holds 5 s
			synctest.Wait()
			outB := flipAsync(bg, g, "b", noCommit) // its 5 s token wait ends with a's hold
			if a := <-outA; a.err != nil {
				t.Fatal(a.err)
			}
			b := <-outB
			if b.err != nil {
				t.Fatal(b.err)
			}
			g.mu.Lock()
			holder, queued := g.holder, len(g.queue)
			g.mu.Unlock()
			if holder != nil || queued != 0 {
				t.Fatalf("round %d: b %s; holder %v and %d queued after both flips returned", i, b.res.Outcome, holder, queued)
			}
			time.Sleep(time.Minute) // every quiet period is over
		}
	})
}

// The idle sweep never drops the entry of a flip in progress. a's hold
// holds a request, so k's flip waits in phase A for that hold and the quiet
// period after it (until 30 s); a sweep at 25 s finds k's entry idle for
// 24 s. A client that starts reading k's anchor at 26 s must still hold
// k's commit back.
func TestGate_SweepKeepsTheEntryOfAFlipInProgress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		release := make(chan struct{})
		outA := flipAsync(bg, g, "a", func() error { <-release; return nil }) // holds from 0 s to its 20 s bound
		synctest.Wait()
		go func() { g.AnchorStart(bg, "a").Done() }() // held until a's hold ends
		time.Sleep(time.Second)
		outK := flipAsync(bg, g, "k", noCommit)
		time.Sleep(24 * time.Second)
		g.AnchorStart(bg, "x").Done() // 25 s: sweeps
		time.Sleep(time.Second)
		reading := g.AnchorStart(bg, "k") // 26 s
		time.Sleep(14 * time.Second)
		readDone := time.Now()
		reading.Done() // 40 s
		k := <-outK
		if k.err != nil {
			t.Fatal(k.err)
		}
		if k.at.Before(readDone.Add(testQuiet)) {
			t.Fatalf("k committed %v after the read it must wait for ended, want at least the quiet period", k.at.Sub(readDone))
		}
		close(release)
		if a := <-outA; a.err != nil {
			t.Fatal(a.err)
		}
	})
}

// A completion refreshes the entry's idle clock: an anchor read slower than
// twice the quiet period that has just finished is not swept, so a flip
// right after it still waits out the quiet period.
func TestGate_SweepKeepsAJustFinishedSlowRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		slow := g.AnchorStart(bg, "k") // 0 s (this call sweeps an empty map)
		time.Sleep(25 * time.Second)
		slow.Done() // 25 s: under apt's 30 s timeout, so it counts
		time.Sleep(time.Second)
		g.AnchorStart(bg, "x").Done() // 26 s: sweeps
		start := time.Now()
		if _, err := g.Flip(bg, "k", noCommit); err != nil {
			t.Fatal(err)
		}
		if got := time.Since(start); got != 9*time.Second {
			t.Fatalf("flip waited %v, want 9s (the quiet period after the 25 s completion)", got)
		}
	})
}

// The waiter cap counts the requests waiting now: a waiter whose client left
// frees its place, so a later request still waits in the hold.
func TestAnchorStart_WaiterCapCountsOnlyCurrentWaiters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		release := make(chan struct{})
		out := flipAsync(bg, g, "k", func() error { <-release; return nil }) // idle: holds at once
		synctest.Wait()
		ctx, cancel := context.WithCancel(bg)
		var wg sync.WaitGroup
		for i := 0; i < MaxWaiters; i++ {
			c := bg
			if i == 0 {
				c = ctx
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				g.AnchorStart(c, "k").Done()
			}()
		}
		synctest.Wait()
		cancel() // one waiting client leaves
		synctest.Wait()
		late := make(chan struct{})
		go func() {
			g.AnchorStart(bg, "k").Done()
			close(late)
		}()
		synctest.Wait()
		select {
		case <-late:
			t.Fatalf("a request was served during the hold with only %d others waiting", MaxWaiters-1)
		default:
		}
		close(release)
		wg.Wait()
		<-late
		if o := <-out; o.err != nil {
			t.Fatal(o.err)
		}
	})
}

// Design note 17 (design review I1): a hold that held no request delayed
// no client's queue, so it starts no cross-suite quiet period: an idle
// suite that flips a second after another idle suite's flip commits at
// once.
func TestFlip_HoldsThatHeldNothingDoNotDelayOtherFlips(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		if res, err := g.Flip(bg, "a", noCommit); err != nil || res.Waiters != 0 {
			t.Fatalf("a: %+v, %v; want a hold that held nothing", res, err)
		}
		time.Sleep(time.Second)
		start := time.Now()
		res, err := g.Flip(bg, "b", noCommit)
		if err != nil {
			t.Fatal(err)
		}
		if res.PhaseA != 0 || time.Since(start) != 0 {
			t.Fatalf("b: phase A %v, committed after %v; want both 0 (a's hold held nothing)", res.PhaseA, time.Since(start))
		}
	})
}

// Design note 18 (design review M3): main closes the gate at the shutdown
// signal. Every flip that has not started its commit — in phase A, waiting
// for the token, or called later — returns ErrAborted wrapping ErrClosed at
// once; the hold in progress ends and answers its waiters, and no hold
// starts again; a commit already running finishes.
func TestGate_CloseAbortsEveryFlipNotYetCommitting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		g.phaseAMax = time.Second
		release := make(chan struct{})
		committing := flipAsync(bg, g, "a", func() error { <-release; return nil }) // idle: holds at once
		synctest.Wait()
		held := make(chan time.Time, 1)
		go func() { g.AnchorStart(bg, "a").Done(); held <- time.Now() }() // waits in a's hold
		busyB := g.AnchorStart(bg, "b")
		defer busyB.Done()
		queued := flipAsync(bg, g, "b", noCommit) // phase A gives up at 1 s; then it queues for the token
		time.Sleep(1500 * time.Millisecond)
		busyC := g.AnchorStart(bg, "c")
		defer busyC.Done()
		inPhaseA := flipAsync(bg, g, "c", noCommit) // in phase A until 2.5 s
		time.Sleep(500 * time.Millisecond)
		closedAt := time.Now()
		g.Close()
		for name, ch := range map[string]<-chan flipOut{"queued for the token": queued, "in phase A": inPhaseA} {
			o := <-ch
			if !errors.Is(o.err, ErrAborted) || !errors.Is(o.err, ErrClosed) || o.res.Committed || !o.at.Equal(closedAt) {
				t.Errorf("%s: %+v, %v, %v after Close; want ErrAborted wrapping ErrClosed at once and no commit",
					name, o.res, o.err, o.at.Sub(closedAt))
			}
		}
		if at := <-held; !at.Equal(closedAt) {
			t.Errorf("the held request was answered %v after Close, want at once", at.Sub(closedAt))
		}
		if _, err := g.Flip(bg, "d", func() error { t.Error("committed after Close"); return nil }); !errors.Is(err, ErrClosed) {
			t.Errorf("a flip after Close: %v, want ErrAborted wrapping ErrClosed", err)
		}
		close(release)
		if o := <-committing; o.err != nil || o.res.Outcome != OutcomeHeld || !o.res.Committed {
			t.Fatalf("the commit in progress: %+v, %v; want it to finish", o.res, o.err)
		}
		if st := g.Stats(); st.Holds != 1 || st.Aborted != 3 {
			t.Fatalf("stats = %+v, want one hold (none started after Close) and three aborted flips", st)
		}
	})
}

// Final review, tests Minor 2 (operability): acu_flip_barrier_active counts
// the flips in the barrier now — waiting for quiet, for the token, holding
// or committing — and a flip leaves it however it ends. A disabled gate's
// flips never enter it.
func TestGate_BarrierActiveGauge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		before := counterValue(t, "acu_flip_barrier_active")
		reading := g.AnchorStart(bg, "a") // a's flip waits in phase A
		outA := flipAsync(bg, g, "a", noCommit)
		release := make(chan struct{})
		outB := flipAsync(bg, g, "b", func() error { <-release; return nil }) // b holds and commits until released
		synctest.Wait()
		if got := counterValue(t, "acu_flip_barrier_active") - before; got != 2 {
			t.Fatalf("acu_flip_barrier_active rose by %v with two flips in the barrier, want 2", got)
		}
		if _, err := newGate(t, 0).Flip(bg, "c", noCommit); err != nil {
			t.Fatal(err)
		}
		close(release)
		reading.Done()
		for _, ch := range []<-chan flipOut{outA, outB} {
			if o := <-ch; o.err != nil {
				t.Fatal(o.err)
			}
		}
		ctx, cancel := context.WithCancel(bg)
		cancel()
		if _, err := g.Flip(ctx, "d", noCommit); !errors.Is(err, ErrAborted) {
			t.Fatalf("a flip whose context had ended: %v, want ErrAborted", err)
		}
		if got := counterValue(t, "acu_flip_barrier_active") - before; got != 0 {
			t.Fatalf("acu_flip_barrier_active is %v above its start once every flip returned, want 0", got)
		}
	})
}

// Token.Held is how long the request waited in a hold (the request line's
// flip_held_ms): zero when it did not wait, the wait until the hold ended
// when it did, the wait until it left when its client went away.
func TestToken_HeldIsTheWaitInAHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		var nilTok *Token
		if nilTok.Held() != 0 {
			t.Fatal("a nil token was held")
		}
		unheld := g.AnchorStart(bg, "k")
		unheld.Done()
		if unheld.Held() != 0 {
			t.Fatalf("an unheld request: Held = %v, want 0", unheld.Held())
		}
		time.Sleep(time.Minute) // its quiet period is over
		release := make(chan struct{})
		out := flipAsync(bg, g, "k", func() error { <-release; return nil }) // idle: holds at once
		synctest.Wait()
		held := make(chan *Token, 1)
		go func() { held <- g.AnchorStart(bg, "k") }()
		ctx, cancel := context.WithCancel(bg)
		left := make(chan *Token, 1)
		go func() { left <- g.AnchorStart(ctx, "k") }()
		time.Sleep(time.Second)
		cancel() // the second client leaves after 1 s
		l := <-left
		time.Sleep(2 * time.Second)
		close(release) // the hold ends at 3 s
		h := <-held
		h.Done()
		l.Done()
		if h.Held() != 3*time.Second || l.Held() != time.Second {
			t.Fatalf("Held = %v (answered) and %v (left), want 3s and 1s", h.Held(), l.Held())
		}
		if o := <-out; o.err != nil {
			t.Fatal(o.err)
		}
	})
}

// Closed reports Close (the adopter's first-adoption skip asks it, so a
// skipped flip honours the shutdown signal too); a nil or disabled gate,
// which ignores Close, is never closed.
func TestGate_ClosedReportsClose(t *testing.T) {
	var nilGate *Gate
	disabled := newGate(t, 0)
	disabled.Close()
	if nilGate.Closed() || disabled.Closed() {
		t.Fatal("a nil or disabled gate reports closed")
	}
	g := newTestGate(t)
	if g.Closed() {
		t.Fatal("closed before Close")
	}
	g.Close()
	if !g.Closed() {
		t.Fatal("not closed after Close")
	}
}

// Design review M6 (exec review observation 4): a panic in the hold-expiry
// timer — a gate bug, injected here — is counted, and the timer's deferred
// unlock keeps the gate usable: the commit's return still ends the hold
// through the flip's own deferred end, and the next flip runs.
func TestFlip_PanicInTheExpiryTimerLeavesTheGateUsable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		g.onExpire = func() { panic("injected gate bug") }
		panics := counterValue(t, "acu_flip_gate_panics_total")
		out := flipAsync(bg, g, "k", func() error { time.Sleep(25 * time.Second); return nil })
		time.Sleep(21 * time.Second) // the expiry timer fired, and panicked, at 20 s
		if !g.mu.TryLock() {
			g.mu.Unlock() // let the flip finish
			t.Fatal("a panic in the expiry timer left the gate locked")
		}
		g.mu.Unlock()
		o := <-out
		if o.err != nil || !o.res.Committed || o.res.HoldEnd.Sub(o.res.HoldStart) != 25*time.Second {
			t.Fatalf("Flip = %+v, %v; want the commit's return at 25 s to end the hold", o.res, o.err)
		}
		if got := counterValue(t, "acu_flip_gate_panics_total") - panics; got != 1 {
			t.Fatalf("acu_flip_gate_panics_total rose by %v, want 1", got)
		}
		start := time.Now()
		if _, err := g.Flip(bg, "k2", noCommit); err != nil || time.Since(start) != 0 {
			t.Fatalf("the next flip: err %v after %v, want an immediate commit", err, time.Since(start))
		}
	})
}
