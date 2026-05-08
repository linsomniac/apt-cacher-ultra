// SPEC6 §10.2 mitm_clock_skew detector.
//
// Belt-and-suspenders against a system-clock jump mid-process: a
// freshly-generated leaf cert whose NotBefore is in the future
// relative to the cache's wall clock at the moment of issuance
// would surface to apt as a "not yet valid" TLS error (§11 F17).
// GenerateLeaf backdates NotBefore by 5 minutes, so this should be
// impossible under normal NTP — but a clock that jumps backward
// between the GenerateLeaf call and the post-issuance comparison
// can still trigger it.
package proxy

import (
	"crypto/tls"
	"time"
)

// CheckLeafClockSkew detects whether a freshly-generated leaf cert's
// NotBefore is in the future relative to `now`. When skew is found,
// it emits a §10.2 mitm_clock_skew Warn via logFn with the
// spec-mandated fields {host, not_before, now} and returns true.
// Returns false (and emits nothing) on the no-skew path or when the
// cert lacks a parsed Leaf.
//
// Caller wires this in immediately after GenerateLeaf returns, with
// `now` being a fresh time.Now() call (NOT the same value passed
// into GenerateLeaf — the whole point is to detect a clock that
// moved between the two reads).
func CheckLeafClockSkew(
	host string,
	leaf *tls.Certificate,
	now time.Time,
	logFn func(level, event string, fields map[string]any),
) bool {
	if leaf == nil || leaf.Leaf == nil {
		return false
	}
	if !leaf.Leaf.NotBefore.After(now) {
		return false
	}
	if logFn != nil {
		logFn("warn", "mitm_clock_skew", map[string]any{
			"host":       host,
			"not_before": leaf.Leaf.NotBefore.UTC().Format(time.RFC3339Nano),
			"now":        now.UTC().Format(time.RFC3339Nano),
		})
	}
	return true
}
