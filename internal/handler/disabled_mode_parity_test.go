package handler

// SPEC6 §15 #4 disabled-mode parity DoD pin.
//
// With tls_mitm.enabled = false (the default newTestHandler shape:
// h.connect == nil), the spec mandates two request-path invariants
// in addition to the wire-form 405+Allow already pinned in
// connect_integration_test.go and the status-JSON shape pinned in
// internal/admin/admin_test.go:
//
//   - No `mitm_*` log line is emitted on the request path.
//   - No `acu_mitm_*` request-path metric increments. The metrics
//     are REGISTERED (TestMITMMetrics_AllRegistered in package proxy
//     covers that), but observation sites must stay quiet.
//
// These two assertions live here as one self-contained file because
// the scope is "request-path quiet" — the handler layer is the right
// boundary to exercise. Each test drives every ServeHTTP branch that
// an apt client could reach in disabled mode (GET miss + hit, HEAD,
// method-not-allowed, CONNECT-without-handler), then asserts the
// invariant.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
)

// driveDisabledModeBranches drives the four ServeHTTP branches an
// apt client can reach in disabled mode: GET miss, GET hit, HEAD,
// POST→405, CONNECT→405. The upstream serves a fixed body so the
// fetch path completes deterministically. Returns nothing — caller
// asserts post-conditions on logs/metrics.
func driveDisabledModeBranches(t *testing.T, h *Handler, upstreamURL string) {
	t.Helper()
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, proxyReq(http.MethodGet, upstreamURL, "/foo"))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET[%d] status = %d, want 200; body=%q", i, rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq(http.MethodHead, upstreamURL, "/foo"))
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq(http.MethodPost, upstreamURL, "/foo"))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", rec.Code)
	}
	rec = httptest.NewRecorder()
	connReq := httptest.NewRequest(http.MethodConnect, "http://example.com/", nil)
	connReq.RequestURI = "example.com:443"
	h.ServeHTTP(rec, connReq)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("CONNECT status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("CONNECT Allow = %q, want %q", got, "GET, HEAD")
	}
}

// TestDisabledMode_NoMITMLogsOnRequestPath pins the SPEC6 §15 #4
// "No `mitm_*` log lines are emitted at any point" clause for the
// request path. We install a JSON logger on a default newTestHandler
// (no CONNECT handler installed → MITM disabled), drive every branch
// ServeHTTP routes, then walk each captured record and reject any
// whose `msg` or `event` field begins with `mitm_`.
func TestDisabledMode_NoMITMLogsOnRequestPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "5")
		_, _ = w.Write([]byte("hello"))
	}))
	defer upstream.Close()

	// safeWriter wraps a strings.Builder for goroutine-safe writes —
	// touchAsync logs from a fresh goroutine in the GET-hit path.
	var sb strings.Builder
	logger := slog.New(slog.NewJSONHandler(&safeWriter{w: &sb}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := newTestHandler(t, nil, nil)
	h.logger = logger

	driveDisabledModeBranches(t, h, upstream.URL)

	// Walk each JSON log record. Both `msg` (slog's first arg of
	// Info/Warn/etc.) and `event` (some emitters use it as a
	// structured key) are checked — the spec rule targets the
	// emitted name, regardless of slog's encoding choice.
	for _, line := range strings.Split(strings.TrimSpace(sb.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("invalid JSON log line: %v\n%s", err, line)
		}
		for _, k := range []string{"msg", "event"} {
			v, ok := rec[k].(string)
			if !ok {
				continue
			}
			if strings.HasPrefix(v, "mitm_") {
				t.Errorf("disabled mode emitted %s=%q on request path; line: %s", k, v, line)
			}
		}
	}
}

// TestDisabledMode_NoMITMMetricsOnRequestPath pins the SPEC6 §15 #4
// "No `acu_mitm_*` metrics ever increment from a request path"
// clause. We snapshot the request-path metric values from the
// shared Default registry, drive every ServeHTTP branch, then
// assert no change.
//
// Gauges (cert_cache_size, cert_cache_capacity, ca_not_after_unixtime)
// are excluded from the invariant: they are written by main.go's
// startup wiring when MITM is enabled, not by the request path. In
// disabled mode they stay at the package-init zero value, which the
// proxy package's TestMITMMetrics_AllRegistered already covers.
func TestDisabledMode_NoMITMMetricsOnRequestPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "5")
		_, _ = w.Write([]byte("hello"))
	}))
	defer upstream.Close()

	h := newTestHandler(t, nil, nil)

	// Each entry is a metric line PREFIX. For a counter the line is
	// `<name>{labels} value` or `<name> value` (unlabeled). For a
	// histogram observation count it is `<name>_count{labels} value`
	// or `<name>_count value`. Summing across every series with the
	// given prefix gives a value that only grows on observation —
	// so a zero delta after the request burst proves no observation
	// fired.
	requestPathPrefixes := []string{
		"acu_mitm_connect_total",                    // ServeCONNECT outcome counter
		"acu_mitm_cert_cache_lookups_total",         // RecordCertCacheLookup
		"acu_mitm_cert_issued_total",                // RecordCertIssued
		"acu_mitm_cert_evicted_total",               // RecordCertEvicted
		"acu_mitm_connect_duration_seconds_count",   // histogram obs count
		"acu_mitm_handshake_duration_seconds_count", // histogram obs count
	}

	before := sumMetricLines(t, requestPathPrefixes)
	driveDisabledModeBranches(t, h, upstream.URL)
	after := sumMetricLines(t, requestPathPrefixes)

	for _, name := range requestPathPrefixes {
		if before[name] != after[name] {
			t.Errorf("disabled mode incremented %s: before=%g after=%g (delta=%g)",
				name, before[name], after[name], after[name]-before[name])
		}
	}
}

// sumMetricLines renders the Default registry and returns, for each
// requested name, the SUM of values across every series matching
// that name. A series matches when the line begins with `<name>{`
// (labeled), or `<name> ` (unlabeled). HELP / TYPE comments and
// lines for differently-named metrics are excluded.
func sumMetricLines(t *testing.T, names []string) map[string]float64 {
	t.Helper()
	var buf bytes.Buffer
	metrics.Default.Render(&buf)
	out := buf.String()
	sums := make(map[string]float64, len(names))
	for _, name := range names {
		for _, line := range strings.Split(out, "\n") {
			if !lineMatchesMetric(line, name) {
				continue
			}
			sp := strings.LastIndexByte(line, ' ')
			if sp < 0 {
				continue
			}
			var v float64
			if _, err := fmt.Sscanf(line[sp+1:], "%g", &v); err != nil {
				continue
			}
			sums[name] += v
		}
	}
	return sums
}

// lineMatchesMetric returns true iff `line` is a value line for the
// metric named `name` — i.e., starts with `<name>{` (labeled series)
// or `<name> ` (unlabeled value). HELP / TYPE comments and lines
// for differently-named metrics are excluded.
func lineMatchesMetric(line, name string) bool {
	if !strings.HasPrefix(line, name) {
		return false
	}
	rest := line[len(name):]
	return strings.HasPrefix(rest, "{") || strings.HasPrefix(rest, " ")
}
