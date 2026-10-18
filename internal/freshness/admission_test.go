package freshness

import (
	"fmt"
	"testing"
)

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

// freeFor is a peek for the scheduler's offers; it must agree with
// tryAcquire's refusal rule exactly.
func TestAdmission_FreeForAgreesWithTryAcquire(t *testing.T) {
	for _, global := range []int{0, 1, 2, 3} {
		maxUse := global
		if global == 0 {
			maxUse = 3
		}
		for inUse := 0; inUse <= maxUse; inUse++ {
			for _, hostHeld := range []bool{false, true} {
				if hostHeld && inUse == 0 {
					continue
				}
				for _, signed := range []bool{false, true} {
					a := newAdmission(global)
					for i := 0; i < inUse; i++ {
						host := fmt.Sprintf("o%d", i)
						if hostHeld && i == 0 {
							host = "h"
						}
						mustAcquire(t, a, host, true)
					}
					peek := a.freeFor("h", signed) > 0
					_, ok := a.tryAcquire("h", signed)
					if peek != ok {
						t.Errorf("global=%d inUse=%d hostHeld=%v signed=%v: freeFor>0 = %v but tryAcquire = %v",
							global, inUse, hostHeld, signed, peek, ok)
					}
				}
			}
		}
	}
}

// Every release of a held slot wakes the scheduler; a repeated release
// does not; wakes never block and coalesce into one.
func TestAdmission_ReleaseWakesTheScheduler(t *testing.T) {
	a := newAdmission(2)
	woke := func() bool {
		select {
		case <-a.freed:
			return true
		default:
			return false
		}
	}
	l := mustAcquire(t, a, "a", true)
	l.releaseHost()
	if !woke() {
		t.Fatal("releaseHost did not wake the scheduler")
	}
	l.release()
	if !woke() {
		t.Fatal("releasing the global slot did not wake the scheduler")
	}
	l.release()
	if woke() {
		t.Fatal("a repeated release woke the scheduler")
	}
	for i := 0; i < 3; i++ {
		mustAcquire(t, a, "b", true).release()
	}
	if !woke() || woke() {
		t.Fatal("three wakes should coalesce into exactly one")
	}
}
