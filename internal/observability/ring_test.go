package observability

import (
	"sync"
	"testing"
)

func eventN(n int64) AdoptionEvent {
	return AdoptionEvent{
		Host:             "h",
		SuitePath:        "p",
		Outcome:          "success",
		CompletedUnixSec: n,
		DurationSeconds:  float64(n),
	}
}

func TestRing_NewRingPanicsOnZero(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewRing(0) should panic")
		}
	}()
	NewRing(0)
}

func TestRing_NewRingPanicsOnNegative(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewRing(-1) should panic")
		}
	}()
	NewRing(-1)
}

func TestRing_EmptySnapshot(t *testing.T) {
	r := NewRing(50)
	snap := r.Snapshot()
	if snap == nil {
		t.Fatal("Snapshot of empty ring returned nil; want empty slice")
	}
	if len(snap) != 0 {
		t.Errorf("len = %d, want 0", len(snap))
	}
	if r.Len() != 0 {
		t.Errorf("Len = %d, want 0", r.Len())
	}
}

// TestRing_FillsToCapNoWrap exercises the "not yet full" path:
// record fewer events than the capacity, verify all appear newest
// first.
func TestRing_FillsToCapNoWrap(t *testing.T) {
	r := NewRing(5)
	for i := int64(1); i <= 3; i++ {
		r.Record(eventN(i))
	}
	if r.Len() != 3 {
		t.Errorf("Len = %d, want 3", r.Len())
	}
	snap := r.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("len(snap) = %d, want 3", len(snap))
	}
	// Newest-first: 3, 2, 1.
	for i, want := range []int64{3, 2, 1} {
		if snap[i].CompletedUnixSec != want {
			t.Errorf("snap[%d].CompletedUnixSec = %d, want %d",
				i, snap[i].CompletedUnixSec, want)
		}
	}
}

// TestRing_OverwritesOldestWhenFull exercises the wrap path: record
// more events than the capacity; the oldest should be dropped, the
// rest in newest-first order.
func TestRing_OverwritesOldestWhenFull(t *testing.T) {
	r := NewRing(3)
	for i := int64(1); i <= 5; i++ {
		r.Record(eventN(i))
	}
	if r.Len() != 3 {
		t.Errorf("Len after 5 records into cap=3 = %d, want 3", r.Len())
	}
	snap := r.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("len(snap) = %d, want 3", len(snap))
	}
	// Newest-first across the wrap: 5, 4, 3 (1 and 2 dropped).
	for i, want := range []int64{5, 4, 3} {
		if snap[i].CompletedUnixSec != want {
			t.Errorf("snap[%d].CompletedUnixSec = %d, want %d",
				i, snap[i].CompletedUnixSec, want)
		}
	}
}

// TestRing_ExactlyFull exercises the boundary: record exactly cap
// events, verify the not-wrapped-yet branch returns all in
// newest-first order.
func TestRing_ExactlyFull(t *testing.T) {
	r := NewRing(50)
	for i := int64(1); i <= 50; i++ {
		r.Record(eventN(i))
	}
	if r.Len() != 50 {
		t.Errorf("Len = %d, want 50", r.Len())
	}
	snap := r.Snapshot()
	if len(snap) != 50 {
		t.Fatalf("len(snap) = %d, want 50", len(snap))
	}
	if snap[0].CompletedUnixSec != 50 {
		t.Errorf("snap[0] = %d, want 50 (newest)", snap[0].CompletedUnixSec)
	}
	if snap[49].CompletedUnixSec != 1 {
		t.Errorf("snap[49] = %d, want 1 (oldest still present)", snap[49].CompletedUnixSec)
	}
}

// TestRing_RecordPlusOneAfterFull exercises one wrap step exactly:
// fill cap=3 with [1,2,3], record 4 → snapshot should be [4,3,2].
func TestRing_RecordPlusOneAfterFull(t *testing.T) {
	r := NewRing(3)
	r.Record(eventN(1))
	r.Record(eventN(2))
	r.Record(eventN(3))
	r.Record(eventN(4))
	snap := r.Snapshot()
	for i, want := range []int64{4, 3, 2} {
		if snap[i].CompletedUnixSec != want {
			t.Errorf("snap[%d] = %d, want %d",
				i, snap[i].CompletedUnixSec, want)
		}
	}
}

// TestRing_SnapshotIsIndependent proves the returned slice survives
// subsequent Record calls without being mutated. The status-page
// handler relies on this: it calls Snapshot, releases the lock,
// then renders without re-locking.
func TestRing_SnapshotIsIndependent(t *testing.T) {
	r := NewRing(3)
	r.Record(eventN(1))
	r.Record(eventN(2))
	snap := r.Snapshot()

	// Subsequent records must not mutate snap.
	r.Record(eventN(3))
	r.Record(eventN(99))

	if snap[0].CompletedUnixSec != 2 || snap[1].CompletedUnixSec != 1 {
		t.Errorf("snapshot mutated by later Record calls: %+v", snap)
	}
}

// TestRing_ConcurrentRecordSnapshot exercises the mutex semantics
// under -race. No assertion on contents — just that no race detector
// fires and Snapshot's internal copy stays internally consistent.
func TestRing_ConcurrentRecordSnapshot(t *testing.T) {
	r := NewRing(50)
	var wg sync.WaitGroup
	const writers = 20
	const reads = 200

	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				r.Record(eventN(int64(id*1000 + j)))
			}
		}(i)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < reads; j++ {
			snap := r.Snapshot()
			// Invariant: every snapshot has length in [0, cap].
			if len(snap) > 50 {
				t.Errorf("snapshot len out of range: %d", len(snap))
				return
			}
		}
	}()

	wg.Wait()
}
