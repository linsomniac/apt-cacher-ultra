// Package observability holds in-process observability state that is
// not authoritative cache state but is useful for the §9.7 admin
// listener's status page. The first inhabitant (Phase 5) is a
// fixed-capacity ring buffer of recent adoption events — covering
// both successes and failures — that the freshness package writes to
// alongside its existing log emits and metric increments.
//
// SPEC5 §9.7.7: the ring is process-local, dropped on restart, and
// not part of graceful-shutdown ordering. Its only purpose is the
// status page's "recent adoptions" view: the DB has
// suite_snapshot.adopted_at for successful adoptions only, so failure
// history would otherwise have no in-cache record. Without schema
// changes (Phase 5 commits to none), process memory is the only
// place to keep failure events.
package observability

import (
	"sync"
)

// AdoptionEvent records one completed adoption attempt — success or
// failure. Schema mirrors the SPEC5 §10.5 status-page JSON
// `recent_adoptions[]` field set, so the status-page handler can
// emit Snapshot results directly.
type AdoptionEvent struct {
	// Host is the canonical_host of the suite.
	Host string
	// SuitePath is the canonical suite path (e.g.
	// "ubuntu/dists/jammy").
	SuitePath string
	// Outcome is one of the §10.4.3 acu_adoption_total outcome
	// labels: success, parse_failed, gpg_failed, member_mismatch,
	// unpinned_suite, run_failed.
	Outcome string
	// Reason is a short snake_case tag describing why a non-success
	// outcome occurred. Empty on success. For gpg_failed it breaks
	// the bucket out into the specific verifier sentinel
	// (untrusted_signer, short_keyid, no_usable_signature,
	// missing_signature, ambiguous_keyid, crypto_verify_failed); for
	// other failure outcomes it mirrors Outcome. Surfaced via the
	// SPEC5 §10.5 status-page JSON additively as
	// `recent_adoptions[].reason`.
	Reason string
	// CompletedUnixSec is the unix-seconds timestamp the adoption
	// finished (success OR failure). For successful adoptions this
	// approximately equals suite_snapshot.adopted_at; for failures
	// there is no DB record so this field is the only timeline.
	CompletedUnixSec int64
	// DurationSeconds is the wallclock from the start of the
	// adoption goroutine to the completion log emit.
	DurationSeconds float64
}

// Ring is a thread-safe, fixed-capacity ring buffer of
// AdoptionEvents. Newest events overwrite oldest when full. The
// zero value is not usable; construct via NewRing.
//
// AIDEV-NOTE: Phase 5 uses one Ring shared across all freshness
// goroutines. The single mutex is not a bottleneck — Record is
// O(1) under the lock, and call rate is bounded by the adoption
// completion rate (typically <1/s aggregate across all suites).
type Ring struct {
	mu   sync.Mutex
	buf  []AdoptionEvent
	head int // index of the next slot to write
	full bool
	cap  int
}

// NewRing constructs a Ring with the given capacity. capacity must
// be > 0; New panics on a non-positive value (programming error,
// not a runtime path).
func NewRing(capacity int) *Ring {
	if capacity <= 0 {
		panic("observability: NewRing capacity must be > 0")
	}
	return &Ring{
		buf: make([]AdoptionEvent, capacity),
		cap: capacity,
	}
}

// Record appends an event to the ring. When the buffer is full,
// the oldest event is overwritten. O(1) under the lock; safe for
// concurrent callers.
func (r *Ring) Record(e AdoptionEvent) {
	r.mu.Lock()
	r.buf[r.head] = e
	r.head++
	if r.head == r.cap {
		r.head = 0
		r.full = true
	}
	r.mu.Unlock()
}

// Snapshot returns a newest-first copy of every event currently in
// the ring. The returned slice is independent of subsequent Record
// calls; callers may retain it without locking. An empty ring
// returns an empty (non-nil) slice.
//
// AIDEV-NOTE: SPEC5 §10.5: the status-page handler invokes Snapshot
// at request time. The cost is O(N) under the lock where N is the
// number of stored events (≤ capacity). For Phase 5's capacity=50,
// this is negligible compared to the rest of the status-page
// rendering work.
func (r *Ring) Snapshot() []AdoptionEvent {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.full && r.head == 0 {
		return []AdoptionEvent{}
	}

	var n int
	if r.full {
		n = r.cap
	} else {
		n = r.head
	}
	out := make([]AdoptionEvent, n)
	// Walk newest-first: start at head-1 (the most recently written
	// slot) and step backward, wrapping at -1 to cap-1. The two
	// branches handle "full ring" (read all cap entries) and "not
	// yet full" (read head entries).
	idx := r.head - 1
	if idx < 0 {
		idx = r.cap - 1
	}
	for i := 0; i < n; i++ {
		out[i] = r.buf[idx]
		idx--
		if idx < 0 {
			idx = r.cap - 1
		}
	}
	return out
}

// Len reports the number of events currently in the ring. Useful
// for the status-page "(empty since last process start)" cue
// (SPEC5 §9.7.3) without allocating a snapshot copy.
func (r *Ring) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.full {
		return r.cap
	}
	return r.head
}
