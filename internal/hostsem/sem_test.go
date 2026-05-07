package hostsem

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestSem_LimitsConcurrency(t *testing.T) {
	s := New(2)

	rel1, err := s.Acquire(context.Background(), "h")
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	rel2, err := s.Acquire(context.Background(), "h")
	if err != nil {
		t.Fatalf("acquire 2: %v", err)
	}

	// Third Acquire must block until release.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = s.Acquire(ctx, "h")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("third acquire err=%v, want DeadlineExceeded", err)
	}

	rel1()
	// Now a fourth Acquire should succeed promptly.
	rel3, err := s.Acquire(context.Background(), "h")
	if err != nil {
		t.Fatalf("post-release acquire: %v", err)
	}
	rel2()
	rel3()
}

func TestSem_PerHostIsolated(t *testing.T) {
	s := New(1)
	rA, err := s.Acquire(context.Background(), "a")
	if err != nil {
		t.Fatalf("acquire a: %v", err)
	}
	defer rA()

	// Different host has its own slot, so this must succeed without
	// blocking even though a's slot is held.
	rB, err := s.Acquire(context.Background(), "b")
	if err != nil {
		t.Fatalf("acquire b: %v", err)
	}
	defer rB()
}

// TestSem_RefcountReleasesSlot proves the per-host map shrinks when
// the last holder of a slot releases. Without refcounting, every distinct
// host the cache ever sees creates a permanent map entry and an attacker
// can grow the map without bound by sending requests for many made-up
// hostnames.
func TestSem_RefcountReleasesSlot(t *testing.T) {
	s := New(2)

	rel, err := s.Acquire(context.Background(), "transient-host")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if got := s.HostCount(); got != 1 {
		t.Errorf("HostCount during use = %d, want 1", got)
	}
	rel()
	if got := s.HostCount(); got != 0 {
		t.Errorf("HostCount after last release = %d, want 0", got)
	}

	// Many transient hosts — each should clean up after itself.
	for i := 0; i < 100; i++ {
		host := fmt.Sprintf("h-%d", i)
		rel, err := s.Acquire(context.Background(), host)
		if err != nil {
			t.Fatalf("acquire %q: %v", host, err)
		}
		rel()
	}
	if got := s.HostCount(); got != 0 {
		t.Errorf("HostCount after churn = %d, want 0", got)
	}
}

// TestSem_DoubleReleaseIsNoop proves the release closure is safe to
// invoke more than once. Without sync.Once guarding, a second call
// would block indefinitely on the channel receive (the token was
// already returned by the first call and no one will put another in).
// Acquire's contract is "exactly once," but a defensive no-op on
// double-call prevents an accidental bug from deadlocking.
func TestSem_DoubleReleaseIsNoop(t *testing.T) {
	s := New(2)
	rel, err := s.Acquire(context.Background(), "h")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// First release: returns the token, drops the refcount.
	rel()

	// Second release must NOT block. Run with a deadline so a
	// regression manifests as a deadline rather than an indefinite
	// hang of the test process.
	done := make(chan struct{})
	go func() {
		rel()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("second release blocked; sync.Once guard missing?")
	}

	// Slot must still be cleaned up — the second release does not
	// fire dropRef, so the count should remain whatever the first
	// release left it (zero).
	if got := s.HostCount(); got != 0 {
		t.Errorf("HostCount after double-release = %d, want 0", got)
	}

	// Acquire must still work after a double-release.
	rel2, err := s.Acquire(context.Background(), "h")
	if err != nil {
		t.Fatalf("post-double-release acquire: %v", err)
	}
	rel2()
}

// TestSem_RefcountSurvivesCtxCancel proves a ctx-cancelled Acquire
// (which never took a channel token) still drops its refcount.
func TestSem_RefcountSurvivesCtxCancel(t *testing.T) {
	s := New(1)

	// Hold the only slot.
	hold, err := s.Acquire(context.Background(), "h")
	if err != nil {
		t.Fatalf("acquire hold: %v", err)
	}

	// Try to Acquire a second slot with a ctx that fires fast.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = s.Acquire(ctx, "h")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquire-2 err=%v, want DeadlineExceeded", err)
	}
	// Refcount: hold has 1 ref, the failed acquire decremented its own.
	// Both ops on host "h" → slot still alive (refs = 1).
	if got := s.HostCount(); got != 1 {
		t.Errorf("HostCount with one holder = %d, want 1", got)
	}

	hold()
	if got := s.HostCount(); got != 0 {
		t.Errorf("HostCount after final release = %d, want 0", got)
	}
}

// TestSem_SnapshotEmpty proves that a fresh Sem returns an empty
// (but non-nil) map.
func TestSem_SnapshotEmpty(t *testing.T) {
	s := New(8)
	snap := s.Snapshot()
	if snap == nil {
		t.Fatal("Snapshot returned nil; want empty map")
	}
	if len(snap) != 0 {
		t.Errorf("Snapshot len=%d, want 0", len(snap))
	}
}

// TestSem_SnapshotCountsHeldTokens proves the SPEC5 §9.3 formula:
// Inflight = len(slot.ch), Capacity = cap(slot.ch). Two acquires
// on host "a" and one on host "b" should report (2, N) and (1, N).
func TestSem_SnapshotCountsHeldTokens(t *testing.T) {
	const limit = 4
	s := New(limit)

	rA1, err := s.Acquire(context.Background(), "a")
	if err != nil {
		t.Fatalf("acquire a1: %v", err)
	}
	defer rA1()
	rA2, err := s.Acquire(context.Background(), "a")
	if err != nil {
		t.Fatalf("acquire a2: %v", err)
	}
	defer rA2()
	rB, err := s.Acquire(context.Background(), "b")
	if err != nil {
		t.Fatalf("acquire b: %v", err)
	}
	defer rB()

	snap := s.Snapshot()
	if got := len(snap); got != 2 {
		t.Fatalf("Snapshot len=%d, want 2", got)
	}
	if got, want := snap["a"], (HostStat{Inflight: 2, Capacity: limit}); got != want {
		t.Errorf("Snapshot[a]=%+v, want %+v", got, want)
	}
	if got, want := snap["b"], (HostStat{Inflight: 1, Capacity: limit}); got != want {
		t.Errorf("Snapshot[b]=%+v, want %+v", got, want)
	}
}

// TestSem_SnapshotExcludesWaiters proves that an acquirer blocked on
// a full slot does not contribute to Inflight — only callers that
// have actually taken a channel token count. SPEC5 §9.3.
func TestSem_SnapshotExcludesWaiters(t *testing.T) {
	const limit = 1
	s := New(limit)

	// Hold the only slot on host "h".
	rel, err := s.Acquire(context.Background(), "h")
	if err != nil {
		t.Fatalf("acquire holder: %v", err)
	}
	defer rel()

	// Start a waiter that will block until the holder releases.
	waiterAcquired := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		r, err := s.Acquire(ctx, "h")
		if err == nil {
			r()
			close(waiterAcquired)
		}
	}()

	// Give the goroutine time to enter the channel send and block.
	// It has incremented refs but not yet acquired the token.
	time.Sleep(20 * time.Millisecond)

	snap := s.Snapshot()
	got := snap["h"]
	if got.Inflight != 1 {
		t.Errorf("Inflight with one holder + one waiter = %d, want 1 (waiter must not count)", got.Inflight)
	}
	if got.Capacity != limit {
		t.Errorf("Capacity = %d, want %d", got.Capacity, limit)
	}
	// Wait for the waiter to time out so it doesn't leak.
	select {
	case <-waiterAcquired:
		t.Error("waiter unexpectedly acquired before holder released")
	case <-time.After(300 * time.Millisecond):
		// Expected: the 200ms ctx fired.
	}
}

// TestSem_SnapshotConcurrent exercises the lock semantics: a Snapshot
// taken while many goroutines acquire/release should not produce
// inconsistent values. Run under -race to detect data races on the
// returned map's contents.
func TestSem_SnapshotConcurrent(t *testing.T) {
	const limit = 8
	const goroutines = 50
	const iters = 200
	s := New(limit)

	done := make(chan struct{})
	go func() {
		// Reader: snapshot in a tight loop.
		for {
			select {
			case <-done:
				return
			default:
			}
			snap := s.Snapshot()
			for host, stat := range snap {
				// Spec invariant: 0 <= Inflight <= Capacity.
				if stat.Inflight < 0 || stat.Inflight > stat.Capacity {
					t.Errorf("Snapshot[%s] = %+v violates 0 <= Inflight <= Capacity",
						host, stat)
				}
			}
		}
	}()

	// Writers: acquire/release on a small set of hosts concurrently.
	hosts := []string{"a", "b", "c"}
	type job struct{}
	work := make(chan job, goroutines)
	for i := 0; i < goroutines; i++ {
		work <- job{}
	}
	close(work)

	finished := make(chan struct{}, goroutines)
	for i := 0; i < goroutines; i++ {
		go func(id int) {
			for range work {
				for j := 0; j < iters; j++ {
					host := hosts[(id+j)%len(hosts)]
					rel, err := s.Acquire(context.Background(), host)
					if err != nil {
						t.Errorf("acquire: %v", err)
						break
					}
					rel()
				}
			}
			finished <- struct{}{}
		}(i)
	}
	for i := 0; i < goroutines; i++ {
		<-finished
	}
	close(done)
}
