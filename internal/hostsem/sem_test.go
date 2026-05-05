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
