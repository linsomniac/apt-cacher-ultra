package gpg

import "sync"

// onceSetMax is spec C9's bound on each once-logger set (a constant, not
// a knob).
const onceSetMax = 10_000

// onceSet is a once-logger's key set ("canonical_host|suite_path"),
// bounded by onceSetMax: a new key when the set is full starts it over.
// The zero value is ready to use.
//
// AIDEV-NOTE: spec C9 — suites come and go (the reaper deletes them), so
// an unbounded set would grow for the process lifetime. The cost of the
// reset is a once-line logged again for a suite seen before it.
type onceSet struct {
	mu   sync.Mutex
	keys map[string]struct{}
}

// first reports whether key is new to the set, and records it.
func (s *onceSet) first(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.keys[key]; ok {
		return false
	}
	if s.keys == nil || len(s.keys) >= onceSetMax {
		s.keys = make(map[string]struct{})
	}
	s.keys[key] = struct{}{}
	return true
}
