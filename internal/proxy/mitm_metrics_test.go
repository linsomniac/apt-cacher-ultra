package proxy

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
)

// renderDefault captures the current Default registry's text-format
// output for assertion. Helper.
func renderDefault(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	metrics.Default.Render(&buf)
	return buf.String()
}

// TestMITMMetrics_AllRegistered proves the §15 disabled-mode parity
// contract: every acu_mitm_* metric appears in /metrics output even
// before any observation has happened. The registration is at
// package init, so this test just renders and asserts presence.
func TestMITMMetrics_AllRegistered(t *testing.T) {
	out := renderDefault(t)
	want := []string{
		"acu_mitm_connect_total",
		"acu_mitm_connect_duration_seconds",
		"acu_mitm_cert_cache_size",
		"acu_mitm_cert_cache_capacity",
		"acu_mitm_cert_cache_lookups_total",
		"acu_mitm_cert_issued_total",
		"acu_mitm_cert_evicted_total",
		"acu_mitm_ca_not_after_unixtime",
		"acu_mitm_handshake_duration_seconds",
	}
	for _, m := range want {
		if !strings.Contains(out, m) {
			t.Errorf("metric %q not registered (missing from /metrics output)", m)
		}
	}
}

// TestMITMMetrics_LookupRecorder verifies the hit/miss counter
// label routing.
func TestMITMMetrics_LookupRecorder(t *testing.T) {
	before := renderDefault(t)
	// Count current "_total acu_mitm_cert_cache_lookups_total{outcome=hit}"
	// occurrences as a baseline so this test composes with prior runs.
	hitsBefore := strings.Count(before, `acu_mitm_cert_cache_lookups_total{outcome="hit"}`)
	missesBefore := strings.Count(before, `acu_mitm_cert_cache_lookups_total{outcome="miss"}`)

	RecordCertCacheLookup(true)
	RecordCertCacheLookup(true)
	RecordCertCacheLookup(false)

	after := renderDefault(t)
	hitsAfter := strings.Count(after, `acu_mitm_cert_cache_lookups_total{outcome="hit"}`)
	missesAfter := strings.Count(after, `acu_mitm_cert_cache_lookups_total{outcome="miss"}`)

	// The line count itself doesn't increment per Inc — Render emits one line per series.
	// What we actually check: the counter VALUE for the hit/miss series rose.
	if !containsCounterAtLeast(after, `acu_mitm_cert_cache_lookups_total{outcome="hit"}`, 2) {
		t.Errorf("hit counter did not record 2 hits; output:\n%s", after)
	}
	if !containsCounterAtLeast(after, `acu_mitm_cert_cache_lookups_total{outcome="miss"}`, 1) {
		t.Errorf("miss counter did not record 1 miss; output:\n%s", after)
	}
	_, _, _, _ = hitsBefore, missesBefore, hitsAfter, missesAfter
}

// TestMITMMetrics_IssuedRecorder verifies the algorithm-labeled
// counter.
func TestMITMMetrics_IssuedRecorder(t *testing.T) {
	RecordCertIssued("ecdsa-p256")
	RecordCertIssued("ecdsa-p256")
	RecordCertIssued("rsa2048")
	out := renderDefault(t)
	if !containsCounterAtLeast(out, `acu_mitm_cert_issued_total{algorithm="ecdsa-p256"}`, 2) {
		t.Errorf("ecdsa-p256 counter did not record 2; output:\n%s", out)
	}
	if !containsCounterAtLeast(out, `acu_mitm_cert_issued_total{algorithm="rsa2048"}`, 1) {
		t.Errorf("rsa2048 counter did not record 1; output:\n%s", out)
	}
}

// TestMITMMetrics_EvictedRecorder verifies the reason-labeled
// counter routes lru and expired separately.
func TestMITMMetrics_EvictedRecorder(t *testing.T) {
	RecordCertEvicted("lru")
	RecordCertEvicted("expired")
	RecordCertEvicted("expired")
	out := renderDefault(t)
	if !containsCounterAtLeast(out, `acu_mitm_cert_evicted_total{reason="lru"}`, 1) {
		t.Errorf("lru counter did not record 1; output:\n%s", out)
	}
	if !containsCounterAtLeast(out, `acu_mitm_cert_evicted_total{reason="expired"}`, 2) {
		t.Errorf("expired counter did not record 2; output:\n%s", out)
	}
}

// TestMITMMetrics_GaugeSetters verifies gauge values reflect what
// SetCertCacheSize/Capacity/CANotAfterUnixtime write.
func TestMITMMetrics_GaugeSetters(t *testing.T) {
	SetCertCacheSize(42)
	SetCertCacheCapacity(256)
	SetCANotAfterUnixtime(1735689600) // 2025-01-01 UTC, arbitrary
	out := renderDefault(t)
	if !strings.Contains(out, "acu_mitm_cert_cache_size 42") {
		t.Errorf("cert_cache_size gauge not set to 42; output:\n%s", out)
	}
	if !strings.Contains(out, "acu_mitm_cert_cache_capacity 256") {
		t.Errorf("cert_cache_capacity gauge not set to 256; output:\n%s", out)
	}
	if !strings.Contains(out, "acu_mitm_ca_not_after_unixtime 1735689600") {
		t.Errorf("ca_not_after_unixtime gauge not set to 1735689600; output:\n%s", out)
	}
}

// containsCounterAtLeast checks that the prom-text output `out`
// contains a line of the form `<metricLabelPrefix> <value>` where
// value >= want. Because the Default registry is shared across
// tests, we cannot assert exact counts (other tests may have
// already incremented). The "at least" check is the right
// invariant — this test bumped by N, the absolute value just has
// to be at least N.
func containsCounterAtLeast(out, metricLabelPrefix string, want int) bool {
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		if !strings.HasPrefix(line, metricLabelPrefix) {
			continue
		}
		// Format: `name{labels} value`. The value follows the closing brace + space.
		idx := strings.Index(line, "} ")
		if idx < 0 {
			continue
		}
		valStr := line[idx+2:]
		// Handle integer values; floats like "2" are emitted as "2" by the formatter.
		var got int
		_, err := fmt.Sscanf(valStr, "%d", &got)
		if err != nil {
			// Try float in case the formatter emits "2.0" or similar.
			var f float64
			if _, err := fmt.Sscanf(valStr, "%f", &f); err != nil {
				continue
			}
			got = int(f)
		}
		if got >= want {
			return true
		}
	}
	return false
}
