package proxy

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock returns a closure that yields the value of `t` at the
// moment the closure is called. The mutex protects against the
// stats internal lock + the test's external mutation racing.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// TestConnectStats_ClassifySuccess covers the §5.3 success class.
// All three outcomes increment the success counter.
func TestConnectStats_ClassifySuccess(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	s := NewConnectStats()
	s.SetClockForTest(clk.Now)

	s.Record(OutcomeTunneled)
	s.Record(OutcomeInnerMethodRejected)
	s.Record(OutcomeInnerStreamFailed)

	successes, failures := s.Last30Min()
	if successes != 3 {
		t.Errorf("successes = %d, want 3", successes)
	}
	if failures != 0 {
		t.Errorf("failures = %d, want 0", failures)
	}
}

// TestConnectStats_ClassifyFailure covers the §5.3 failure class.
func TestConnectStats_ClassifyFailure(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	s := NewConnectStats()
	s.SetClockForTest(clk.Now)

	s.Record(OutcomeTLSFailed)
	s.Record(OutcomeTLSHandshakeTimeout)
	s.Record(OutcomeCertGenFailed)

	successes, failures := s.Last30Min()
	if successes != 0 {
		t.Errorf("successes = %d, want 0", successes)
	}
	if failures != 3 {
		t.Errorf("failures = %d, want 3", failures)
	}
}

// TestConnectStats_ClassifyIgnored proves pre-TLS rejections do NOT
// land in either bucket — they are configuration / client errors
// that pre-date the CA-distribution question.
func TestConnectStats_ClassifyIgnored(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	s := NewConnectStats()
	s.SetClockForTest(clk.Now)

	s.Record(OutcomeBadTarget)
	s.Record(OutcomeBadHost)
	s.Record(OutcomeIPLiteralHost)
	s.Record(OutcomeBadPort)
	s.Record(OutcomeDeniedHost)

	successes, failures := s.Last30Min()
	if successes != 0 || failures != 0 {
		t.Errorf("ignored outcomes leaked into stats: successes=%d failures=%d", successes, failures)
	}
}

// TestConnectStats_BucketRotation_OldEntriesDecay: an event
// recorded 31 minutes ago must NOT appear in Last30Min after the
// clock advances past it.
func TestConnectStats_BucketRotation_OldEntriesDecay(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	s := NewConnectStats()
	s.SetClockForTest(clk.Now)

	// Record at t=0.
	s.Record(OutcomeTLSFailed)
	if _, f := s.Last30Min(); f != 1 {
		t.Fatalf("at t=0: failures = %d, want 1", f)
	}

	// Advance 31 minutes.
	clk.Set(clk.now.Add(31 * time.Minute))
	if _, f := s.Last30Min(); f != 0 {
		t.Errorf("at t=+31m: failures = %d, want 0 (old bucket should have decayed)", f)
	}
}

// TestConnectStats_BucketRotation_RecentEntriesKept: an event in
// the last 30 minutes is still counted after the clock advances.
func TestConnectStats_BucketRotation_RecentEntriesKept(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	s := NewConnectStats()
	s.SetClockForTest(clk.Now)

	s.Record(OutcomeTLSFailed)
	clk.Set(clk.now.Add(29 * time.Minute))
	if _, f := s.Last30Min(); f != 1 {
		t.Errorf("at t=+29m: failures = %d, want 1 (still inside window)", f)
	}
}

// TestConnectStats_CircularReuse: a 60-minute gap between two
// events on the same wall-minute (mod 30) must not double-count
// the old event — the bucket's `minute` mismatch must zero it.
func TestConnectStats_CircularReuse(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	s := NewConnectStats()
	s.SetClockForTest(clk.Now)

	s.Record(OutcomeTLSFailed) // bucket idx = 0 mod 30 = 0
	clk.Set(clk.now.Add(60 * time.Minute))
	s.Record(OutcomeTLSFailed) // 60 mod 30 = 0 again, same bucket

	successes, failures := s.Last30Min()
	if successes != 0 {
		t.Errorf("successes = %d, want 0", successes)
	}
	if failures != 1 {
		t.Errorf("failures = %d, want 1 (old event in same bucket must NOT survive circular reuse)", failures)
	}
}

// TestConnectStats_ConcurrentRecord proves Record is safe under
// concurrent goroutines — primarily a -race regression check.
func TestConnectStats_ConcurrentRecord(t *testing.T) {
	s := NewConnectStats()
	const N = 100
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			s.Record(OutcomeTunneled)
		}()
	}
	wg.Wait()
	successes, _ := s.Last30Min()
	if successes != N {
		t.Errorf("successes = %d, want %d", successes, N)
	}
}

// TestRunUndistributedCAWatch_FiresOnFailureWithoutSuccess proves
// the §5.3 predicate: failures >= 1 AND successes == 0 → emit.
func TestRunUndistributedCAWatch_FiresOnFailureWithoutSuccess(t *testing.T) {
	s := NewConnectStats()
	s.Record(OutcomeTLSFailed)

	var emits atomic.Int32
	emit := func(successes, failures int) {
		emits.Add(1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunUndistributedCAWatch(ctx, s, 30*time.Millisecond, emit)
	}()

	// Wait long enough for at least one tick.
	time.Sleep(80 * time.Millisecond)
	cancel()
	<-done

	if emits.Load() == 0 {
		t.Errorf("expected ≥1 emit on failures=1 successes=0, got 0")
	}
}

// TestRunUndistributedCAWatch_DoesNotFireOnQuiet: failures==0 AND
// successes==0 (no traffic at all) must NOT emit. Quiet
// deployments should not false-alarm.
func TestRunUndistributedCAWatch_DoesNotFireOnQuiet(t *testing.T) {
	s := NewConnectStats()

	var emits atomic.Int32
	emit := func(successes, failures int) { emits.Add(1) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunUndistributedCAWatch(ctx, s, 30*time.Millisecond, emit)
	}()

	time.Sleep(80 * time.Millisecond)
	cancel()
	<-done

	if emits.Load() != 0 {
		t.Errorf("expected 0 emits on quiet stats, got %d", emits.Load())
	}
}

// TestRunUndistributedCAWatch_DoesNotFireOnAnySuccess: a single
// success in the window suppresses the warning, regardless of how
// many failures co-exist with it.
func TestRunUndistributedCAWatch_DoesNotFireOnAnySuccess(t *testing.T) {
	s := NewConnectStats()
	s.Record(OutcomeTLSFailed)
	s.Record(OutcomeTLSFailed)
	s.Record(OutcomeTunneled) // one success neutralizes the predicate

	var emits atomic.Int32
	emit := func(successes, failures int) { emits.Add(1) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunUndistributedCAWatch(ctx, s, 30*time.Millisecond, emit)
	}()

	time.Sleep(80 * time.Millisecond)
	cancel()
	<-done

	if emits.Load() != 0 {
		t.Errorf("expected 0 emits with at least one success in window, got %d", emits.Load())
	}
}

// TestRunUndistributedCAWatch_NilArgs short-circuits — neither
// nil stats nor a nil emitter should panic.
func TestRunUndistributedCAWatch_NilArgs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	RunUndistributedCAWatch(ctx, nil, 1*time.Second, func(int, int) {})
	RunUndistributedCAWatch(ctx, NewConnectStats(), 1*time.Second, nil)
	RunUndistributedCAWatch(ctx, NewConnectStats(), 0, func(int, int) {})
}
