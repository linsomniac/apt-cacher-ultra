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
}

// lease holds a global and a host slot until released.
type lease struct {
	a          *admission
	host       string
	hostHeld   bool
	globalHeld bool
}

func newAdmission(global int) *admission {
	return &admission{global: global, hosts: map[string]bool{}}
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

// usage reports the global slots and hosts in use.
func (a *admission) usage() (global, hosts int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inUse, len(a.hosts)
}

// releaseHost frees the host slot when the member pipeline ends; the
// global slot stays held through the commit. Idempotent, nil-safe.
func (l *lease) releaseHost() {
	if l == nil {
		return
	}
	l.a.mu.Lock()
	defer l.a.mu.Unlock()
	if l.hostHeld {
		delete(l.a.hosts, l.host)
		l.hostHeld = false
	}
}

// release frees whatever the lease still holds. Idempotent, nil-safe.
func (l *lease) release() {
	if l == nil {
		return
	}
	l.a.mu.Lock()
	defer l.a.mu.Unlock()
	if l.hostHeld {
		delete(l.a.hosts, l.host)
		l.hostHeld = false
	}
	if l.globalHeld {
		l.a.inUse--
		l.globalHeld = false
	}
}
