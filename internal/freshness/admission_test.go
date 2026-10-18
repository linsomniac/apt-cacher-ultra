package freshness

import "testing"

func mustAcquire(t *testing.T, a *admission, host string, signed bool) *lease {
	t.Helper()
	l, ok := a.tryAcquire(host, signed)
	if !ok {
		t.Fatalf("tryAcquire(%q, signed=%v) refused", host, signed)
	}
	return l
}

func TestAdmission_PerHostAndGlobal(t *testing.T) {
	a := newAdmission(3)
	l1 := mustAcquire(t, a, "a", true)
	if _, ok := a.tryAcquire("a", true); ok {
		t.Fatal("a second slot on the same host")
	}
	mustAcquire(t, a, "b", true)
	mustAcquire(t, a, "c", true)
	if _, ok := a.tryAcquire("d", true); ok {
		t.Fatal("the global cap was exceeded")
	}
	l1.release()
	mustAcquire(t, a, "d", true)
	if g, h := a.usage(); g != 3 || h != 3 {
		t.Fatalf("usage = %d/%d, want 3/3", g, h)
	}
}

// Spec C4: with two or more global slots, the last free one is reserved
// for suites whose current snapshot is signed.
func TestAdmission_ReservedSlotForSignedSuites(t *testing.T) {
	a := newAdmission(2)
	u := mustAcquire(t, a, "a", false)
	if _, ok := a.tryAcquire("b", false); ok {
		t.Fatal("an unsigned-current suite took the reserved slot")
	}
	s := mustAcquire(t, a, "b", true)
	u.release()
	if _, ok := a.tryAcquire("c", false); ok {
		t.Fatal("an unsigned-current suite took the last free slot")
	}
	s.release()
	mustAcquire(t, a, "c", false)
}

func TestAdmission_NoReservationBelowTwoSlots(t *testing.T) {
	mustAcquire(t, newAdmission(1), "a", false)
}

func TestAdmission_UnlimitedGlobalKeepsPerHost(t *testing.T) {
	a := newAdmission(0)
	for i := 0; i < 100; i++ {
		mustAcquire(t, a, string(rune('A'+i%26))+string(rune('a'+i/26)), false)
	}
	if _, ok := a.tryAcquire("Aa", false); ok {
		t.Fatal("unlimited global slots must still allow one per host")
	}
}

func TestLease_ReleaseHostThenRelease(t *testing.T) {
	a := newAdmission(2)
	l := mustAcquire(t, a, "a", true)
	l.releaseHost()
	if g, h := a.usage(); g != 1 || h != 0 {
		t.Fatalf("after releaseHost usage = %d/%d, want 1/0", g, h)
	}
	other := mustAcquire(t, a, "a", true) // the host is free again; the global slot is not
	l.release()
	l.release()
	other.release()
	var nilLease *lease
	nilLease.release()
	nilLease.releaseHost()
	if g, h := a.usage(); g != 0 || h != 0 {
		t.Fatalf("usage = %d/%d, want 0/0", g, h)
	}
}
