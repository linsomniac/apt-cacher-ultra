package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"testing"
	"time"
)

// makeLeafForSkew builds a minimal *tls.Certificate with a parsed Leaf
// whose NotBefore is set to `notBefore`. Tests use this in place of a
// real GenerateLeaf round-trip — the skew detector only reads
// leaf.Leaf.NotBefore.
func makeLeafForSkew(notBefore time.Time) *tls.Certificate {
	return &tls.Certificate{
		Leaf: &x509.Certificate{NotBefore: notBefore},
	}
}

// TestCheckLeafClockSkew_NoSkewNoEmit pins the steady-state contract:
// a leaf whose NotBefore is at-or-before `now` produces no log line
// and returns false.
func TestCheckLeafClockSkew_NoSkewNoEmit(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	// 5-minute backdate, the GenerateLeaf default — definitely not
	// in the future.
	leaf := makeLeafForSkew(now.Add(-5 * time.Minute))

	called := false
	skewed := CheckLeafClockSkew("apt.example.com", leaf, now, func(level, event string, fields map[string]any) {
		called = true
	})
	if skewed {
		t.Errorf("CheckLeafClockSkew returned true on no-skew leaf")
	}
	if called {
		t.Errorf("logFn invoked on no-skew leaf")
	}
}

// TestCheckLeafClockSkew_NotBeforeEqualsNowNoEmit pins the boundary:
// not_before == now is NOT in the future, so no emission.
func TestCheckLeafClockSkew_NotBeforeEqualsNowNoEmit(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	leaf := makeLeafForSkew(now)

	called := false
	skewed := CheckLeafClockSkew("apt.example.com", leaf, now, func(level, event string, fields map[string]any) {
		called = true
	})
	if skewed || called {
		t.Errorf("CheckLeafClockSkew flagged not_before == now as skewed (skewed=%v, called=%v)", skewed, called)
	}
}

// TestCheckLeafClockSkew_FutureNotBeforeEmits is the F17 detection
// case: a leaf whose NotBefore is strictly after `now` triggers a
// mitm_clock_skew Warn with the spec-mandated fields.
func TestCheckLeafClockSkew_FutureNotBeforeEmits(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	notBefore := now.Add(2 * time.Minute) // 2 minutes in the future
	leaf := makeLeafForSkew(notBefore)

	type capturedLog struct {
		level  string
		event  string
		fields map[string]any
	}
	var captured []capturedLog
	skewed := CheckLeafClockSkew("apt.example.com", leaf, now, func(level, event string, fields map[string]any) {
		captured = append(captured, capturedLog{level: level, event: event, fields: fields})
	})

	if !skewed {
		t.Fatalf("CheckLeafClockSkew did not detect future not_before")
	}
	if len(captured) != 1 {
		t.Fatalf("logFn called %d times, want 1", len(captured))
	}
	got := captured[0]
	if got.level != "warn" {
		t.Errorf("level = %q, want \"warn\"", got.level)
	}
	if got.event != "mitm_clock_skew" {
		t.Errorf("event = %q, want \"mitm_clock_skew\"", got.event)
	}
	// Field set per §10.2: {host, not_before, now}. No extra, no missing.
	if got.fields["host"] != "apt.example.com" {
		t.Errorf("fields[host] = %v, want apt.example.com", got.fields["host"])
	}
	wantNotBefore := notBefore.UTC().Format(time.RFC3339Nano)
	if got.fields["not_before"] != wantNotBefore {
		t.Errorf("fields[not_before] = %v, want %s", got.fields["not_before"], wantNotBefore)
	}
	wantNow := now.UTC().Format(time.RFC3339Nano)
	if got.fields["now"] != wantNow {
		t.Errorf("fields[now] = %v, want %s", got.fields["now"], wantNow)
	}
	for k := range got.fields {
		switch k {
		case "host", "not_before", "now":
		default:
			t.Errorf("unexpected field %q in mitm_clock_skew event", k)
		}
	}
}

// TestCheckLeafClockSkew_NilLogFnSafe pins: a nil logFn must not panic
// even when skew is present. Returns the skew bool either way so a
// caller could still observe the condition.
func TestCheckLeafClockSkew_NilLogFnSafe(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	leaf := makeLeafForSkew(now.Add(time.Minute))
	// Must not panic.
	if !CheckLeafClockSkew("apt.example.com", leaf, now, nil) {
		t.Errorf("CheckLeafClockSkew should still return true when logFn is nil")
	}
}

// TestCheckLeafClockSkew_NilLeafSafe pins: a nil cert (or one missing
// Leaf) yields no emission and returns false — no panic, no spurious
// Warn from a degenerate input.
func TestCheckLeafClockSkew_NilLeafSafe(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	called := false
	logFn := func(level, event string, fields map[string]any) { called = true }

	if CheckLeafClockSkew("apt.example.com", nil, now, logFn) {
		t.Errorf("nil leaf reported as skewed")
	}
	if CheckLeafClockSkew("apt.example.com", &tls.Certificate{}, now, logFn) {
		t.Errorf("leaf with nil Leaf field reported as skewed")
	}
	if called {
		t.Errorf("logFn invoked on nil/empty leaf")
	}
}
