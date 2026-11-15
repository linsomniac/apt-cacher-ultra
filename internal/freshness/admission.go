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
//
// AIDEV-NOTE: a repair's lease can be asked to give way (lease.yield, set
// by Checker.spawnRepair; the 2026-11-14 publication-delay review). A
// repair heals an optional file of a snapshot that is already serving; an
// adoption publishes a new release. Before, a repair held its host's slot
// and a global one for as long as it ran — up to adoption.max_duration —
// and every adoption on that host, or beyond the pool, was queued behind
// it. Now an adoption that finds only repairs in its way asks them to stop
// (yielders, Checker.admit) and takes the slots they free; the scheduler's
// peek (freeFor) counts their slots as room for the same reason.
type admission struct {
	mu     sync.Mutex
	global int // 0 = unlimited
	inUse  int
	// hosts maps a canonical host to the lease holding its slot.
	hosts map[string]*lease
	// yielding holds the leases that hold a global slot and can give way.
	yielding map[*lease]struct{}
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
	// yield, when non-nil, asks the lease's holder to stop and release it:
	// set for a repair (setYield), nil for an adoption, which never gives
	// way. Guarded by a.mu.
	yield func(reason string)
	// released is closed once the lease holds nothing: what an adoption
	// that asked the holder to give way waits for.
	released chan struct{}
}

func newAdmission(global int) *admission {
	return &admission{global: global, hosts: map[string]*lease{}, yielding: map[*lease]struct{}{},
		freed: make(chan struct{}, 1)}
}

// tryAcquire takes a global and a host slot, or nothing.
func (a *admission) tryAcquire(host string, signedCurrent bool) (*lease, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.hosts[host] != nil {
		return nil, false
	}
	if a.global > 0 {
		free := a.global - a.inUse
		if free <= 0 || (a.global >= 2 && free == 1 && !signedCurrent) {
			return nil, false
		}
	}
	a.inUse++
	l := &lease{a: a, host: host, hostHeld: true, globalHeld: true, released: make(chan struct{})}
	a.hosts[host] = l
	return l, true
}

// setYield marks the lease as one whose holder gives way when asked: fn
// stops the holder, which then releases the lease. Nil-safe.
func (l *lease) setYield(fn func(reason string)) {
	if l == nil {
		return
	}
	l.a.mu.Lock()
	defer l.a.mu.Unlock()
	l.yield = fn
	if l.globalHeld {
		l.a.yielding[l] = struct{}{}
	}
}

// yielders returns the leases a suite on host needs gone to be admitted,
// when every one of them can give way: the repair holding the host's slot,
// and — when the global pool is then still short under the reserved-slot
// rule — repairs holding global slots elsewhere, as few as needed. It
// returns nil when nothing stands in the way, and nil when something that
// does not give way does (an adoption on the host, or a pool that
// adoptions alone exhaust): no repair is disturbed for nothing.
//
// The caller asks them to give way (yieldFuncs) and waits for their
// released channels before it tries again.
func (a *admission) yielders(host string, signedCurrent bool) []*lease {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*lease
	holder := a.hosts[host]
	if holder != nil {
		if holder.yield == nil {
			return nil
		}
		out = append(out, holder)
	}
	if a.global > 0 {
		need := 1
		if a.global >= 2 && !signedCurrent {
			need = 2 // the last free slot is reserved for signed suites
		}
		free := a.global - a.inUse
		if holder != nil && holder.globalHeld {
			free++
		}
		for l := range a.yielding {
			if free >= need {
				break
			}
			if l == holder {
				continue
			}
			out = append(out, l)
			free++
		}
		if free < need {
			return nil
		}
	}
	return out
}

// yieldFuncs returns the yield of each lease in ls that still has one,
// read under the mutex; the caller calls them outside it.
func (a *admission) yieldFuncs(ls []*lease) []func(string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	fns := make([]func(string), 0, len(ls))
	for _, l := range ls {
		if l.yield != nil {
			fns = append(fns, l.yield)
		}
	}
	return fns
}

// freeFor reports how many more leases a suite on host could take now:
// 0 while an adoption holds the host slot; otherwise the free global
// slots, minus the reserved one for a suite without a signed current
// snapshot when the pool has two or more; admissionUnlimited for an
// uncapped pool. Slots held by repairs count as free: a repair gives way
// to an adoption (yielders).
//
// AIDEV-NOTE: a read-only peek for the scheduler's offers. The offered
// check's admit is the authority — tryAcquire, and yielders when only
// repairs are in the way — so this arithmetic must mirror their refusal
// rules exactly (TestAdmission_FreeForAgreesWithTryAcquire pins it for
// leases that do not give way). A wrong peek costs one wasted GET per wake
// or tick (the lost offer re-parks held, or unheld and offered at once
// when a slot was released after its dispatch, B-45: each such re-offer
// needs a new release), never a slot and never a loop.
func (a *admission) freeFor(host string, signedCurrent bool) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if h := a.hosts[host]; h != nil && h.yield == nil {
		return 0
	}
	if a.global == 0 {
		return admissionUnlimited
	}
	free := a.global - a.inUse + len(a.yielding)
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

// dropHostLocked frees the lease's host slot. The caller holds a.mu.
func (l *lease) dropHostLocked() {
	if !l.hostHeld {
		return
	}
	if l.a.hosts[l.host] == l {
		delete(l.a.hosts, l.host)
	}
	l.hostHeld = false
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
	if freed {
		l.dropHostLocked()
		l.a.releases++
	}
	l.a.mu.Unlock()
	if freed {
		l.a.wake()
	}
}

// release frees whatever the lease still holds, closes released, and wakes
// the scheduler if that was anything. Idempotent, nil-safe.
func (l *lease) release() {
	if l == nil {
		return
	}
	l.a.mu.Lock()
	freed := l.hostHeld || l.globalHeld
	l.dropHostLocked()
	if l.globalHeld {
		l.a.inUse--
		l.globalHeld = false
		delete(l.a.yielding, l)
		// Every lease holds its global slot from tryAcquire to its first
		// release, so this runs exactly once per lease.
		close(l.released)
	}
	if freed {
		l.a.releases++
	}
	l.a.mu.Unlock()
	if freed {
		l.a.wake()
	}
}
