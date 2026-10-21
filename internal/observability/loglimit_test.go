package observability

import (
	"sync"
	"testing"
	"time"
)

// The first call is allowed; calls inside the interval are refused and
// counted; the next allowed call reports the count and resets it; the
// interval runs from the last allowed call, not the last refused one; and
// a clock that steps backwards stays throttled.
func TestLogLimiter_AllowsOneLinePerInterval(t *testing.T) {
	var l LogLimiter
	t0 := time.Unix(1_800_000_000, 0)
	for _, c := range []struct {
		at         time.Duration
		ok         bool
		suppressed int
	}{
		{0, true, 0}, // the first call: a zero last
		{time.Second, false, 0},
		{59 * time.Second, false, 0},
		{-time.Hour, false, 0}, // a step backwards is still inside the interval
		{time.Minute, true, 3}, // exactly one interval later: allowed, three hidden
		{time.Minute + time.Second, false, 0},
		{2 * time.Minute, true, 1}, // the count restarted from zero
		{10 * time.Minute, true, 0},
	} {
		ok, suppressed := l.Allow(t0.Add(c.at), time.Minute)
		if ok != c.ok || suppressed != c.suppressed {
			t.Fatalf("Allow(t0%+v) = (%v, %d), want (%v, %d)", c.at, ok, suppressed, c.ok, c.suppressed)
		}
	}
}

// Concurrent callers (run with -race): of n calls at one instant exactly
// one is allowed, and the next interval's line reports the other n-1; of a
// second round spread over that interval, again exactly one.
func TestLogLimiter_ConcurrentCallsAllowOneAndCountTheRest(t *testing.T) {
	const n = 64
	var l LogLimiter
	t0 := time.Unix(1_800_000_000, 0)
	round := func(at func(i int) time.Time) (allowed, reported int) {
		var (
			mu sync.Mutex
			wg sync.WaitGroup
		)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ok, suppressed := l.Allow(at(i), time.Minute)
				mu.Lock()
				defer mu.Unlock()
				if ok {
					allowed++
					reported += suppressed
				}
			}()
		}
		wg.Wait()
		return allowed, reported
	}
	if allowed, reported := round(func(int) time.Time { return t0 }); allowed != 1 || reported != 0 {
		t.Fatalf("first round: %d allowed reporting %d suppressed, want 1 reporting 0", allowed, reported)
	}
	t1 := t0.Add(time.Minute)
	if allowed, reported := round(func(i int) time.Time { return t1.Add(time.Duration(i) * time.Millisecond) }); allowed != 1 || reported != n-1 {
		t.Fatalf("second round: %d allowed reporting %d suppressed, want 1 reporting %d", allowed, reported, n-1)
	}
	if ok, suppressed := l.Allow(t1.Add(2*time.Minute), time.Minute); !ok || suppressed != n-1 {
		t.Fatalf("after the second round: (%v, %d), want (true, %d)", ok, suppressed, n-1)
	}
}
