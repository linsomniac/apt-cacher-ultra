package freshness

import "testing"

// Deferred minor F-19: every adoption event the ring records — a
// success, and the panicked one recoverAdoption records — names the
// suite's scheme, which the status page shows as scheme://host.
func TestAdoptionRing_RecordsTheSuitesScheme(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopt()
	boom := r.release("boom", 0)
	r.ver.panicOn(hashOf(boom))
	r.up.set(boom)
	r.check()
	seen := map[string]bool{}
	for _, ev := range r.ring.Snapshot() {
		if ev.Scheme != "http" {
			t.Errorf("event %+v: scheme %q, want http", ev, ev.Scheme)
		}
		seen[ev.Outcome] = true
	}
	if !seen["success"] || !seen["panicked"] {
		t.Fatalf("ring outcomes %v, want a success and a panicked event", seen)
	}
}
