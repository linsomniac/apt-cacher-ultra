package freshness

import "sync"

// admission is the one capacity discipline for adoption, repair and
// reconcile (spec C4): a global slot pool (max_concurrent_adoptions,
// 0 = no global limit) and one slot per canonical host. Callers
// TryAcquire before spawning and never wait for a slot, so no suite
// mutex is ever held while queued.
//
// AIDEV-NOTE: when the pool has two or more slots, the last free one is
// reserved for suites whose current snapshot is signed: security updates
// are never starved by first adoptions (or, from phase E, unsigned ones).
type admission struct {
	mu     sync.Mutex
	global int // 0 = unlimited
	inUse  int
	hosts  map[string]bool
	// freed wakes the scheduler when a global or host slot is released
	// (spec C4 slot-driven re-offer). Capacity 1 and sends never block: a
	// coalesced wake is enough, because the Run loop re-reads the pool
	// through freeFor rather than counting events. Never closed.
	freed chan struct{}
	// releases counts slot releases (a generation): an offer dispatched
	// before a release that then finds no slot is re-offered at once
	// rather than held (deferred minor B-45).
	releases uint64
}

// admissionUnlimited is what freeFor reports for a pool with no global cap.
const admissionUnlimited = 1 << 30

// lease holds a global and a host slot until released.
type lease struct {
	a          *admission
	host       string
	hostHeld   bool
	globalHeld bool
}

func newAdmission(global int) *admission {
	return &admission{global: global, hosts: map[string]bool{}, freed: make(chan struct{}, 1)}
}

// tryAcquire takes a global and a host slot, or nothing.
func (a *admission) tryAcquire(host string, signedCurrent bool) (*lease, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.hosts[host] {
		return nil, false
	}
	if a.global > 0 {
		free := a.global - a.inUse
		if free <= 0 || (a.global >= 2 && free == 1 && !signedCurrent) {
			return nil, false
		}
	}
	a.inUse++
	a.hosts[host] = true
	return &lease{a: a, host: host, hostHeld: true, globalHeld: true}, true
}

// freeFor reports how many more leases a suite on host could take now:
// 0 while the host slot is held; otherwise the free global slots, minus
// the reserved one for a suite without a signed current snapshot when the
// pool has two or more; admissionUnlimited for an uncapped pool.
//
// AIDEV-NOTE: a read-only peek for the scheduler's offers. tryAcquire is
// the authority, so this arithmetic must mirror its refusal rule exactly
// (TestAdmission_FreeForAgreesWithTryAcquire pins it). A wrong peek costs
// one wasted GET per wake or tick (the lost offer re-parks held), never a
// slot and never a loop.
func (a *admission) freeFor(host string, signedCurrent bool) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.hosts[host] {
		return 0
	}
	if a.global == 0 {
		return admissionUnlimited
	}
	free := a.global - a.inUse
	if a.global >= 2 && !signedCurrent {
		free--
	}
	return max(free, 0)
}

// wake tells the scheduler that capacity was released. Never blocks.
func (a *admission) wake() {
	select {
	case a.freed <- struct{}{}:
	default:
	}
}

// generation is the number of slot releases so far.
func (a *admission) generation() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.releases
}

// usage reports the global slots and hosts in use.
func (a *admission) usage() (global, hosts int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inUse, len(a.hosts)
}

// releaseHost frees the host slot when the member pipeline ends; the
// global slot stays held through the commit. Idempotent, nil-safe. A
// release that frees a slot wakes the scheduler: suites waiting on this
// host may now be admitted.
func (l *lease) releaseHost() {
	if l == nil {
		return
	}
	l.a.mu.Lock()
	freed := l.hostHeld
	if l.hostHeld {
		delete(l.a.hosts, l.host)
		l.hostHeld = false
		l.a.releases++
	}
	l.a.mu.Unlock()
	if freed {
		l.a.wake()
	}
}

// release frees whatever the lease still holds and wakes the scheduler
// if that was anything. Idempotent, nil-safe.
func (l *lease) release() {
	if l == nil {
		return
	}
	l.a.mu.Lock()
	freed := l.hostHeld || l.globalHeld
	if l.hostHeld {
		delete(l.a.hosts, l.host)
		l.hostHeld = false
	}
	if l.globalHeld {
		l.a.inUse--
		l.globalHeld = false
	}
	if freed {
		l.a.releases++
	}
	l.a.mu.Unlock()
	if freed {
		l.a.wake()
	}
}
