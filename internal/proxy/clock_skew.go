// SPEC6 §10.2 mitm_clock_skew detector.
//
// Belt-and-suspenders against a system-clock jump mid-process: a
// leaf cert whose NotBefore is in the future relative to the
// cache's wall clock at the moment of use would surface to apt as
// a "not yet valid" TLS error (§11 F17). GenerateLeaf backdates
// NotBefore by 5 minutes, so this should be impossible under
// normal NTP — but a clock that jumps backward AFTER the leaf was
// generated, while the leaf is still in the cache, leaves the
// cached entry triggering apt's rejection until the next eviction.
package proxy

import (
	"crypto/tls"
	"time"
)

// CheckLeafClockSkew detects whether a leaf cert's NotBefore is in
// the future relative to `now`. When skew is found, it emits a
// §10.2 mitm_clock_skew Warn via logFn with the spec-mandated
// fields {host, not_before, now} and returns true. Returns false
// (and emits nothing) on the no-skew path or when the cert lacks
// a parsed Leaf.
//
// Caller wires this in after every LeafCache.Get so the check
// catches BOTH the freshly-generated path AND the cache-reuse
// path. Cache reuse is the more practically detectable case —
// cached certs live for the configured leaf lifetime, so a
// backward clock jump that lands while a cert is cached produces
// a window during which apt rejections fire.
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
