package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
)

// stubConnect is a tiny ConnectHandler used by integration tests. It
// records every CONNECT it receives and writes a fixed response.
type stubConnect struct {
	calls atomic.Int32
}

func (s *stubConnect) ServeCONNECT(w http.ResponseWriter, r *http.Request) {
	s.calls.Add(1)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("dispatched"))
}

// TestServeHTTP_CONNECTWithoutHandler_Returns405 proves the pre-
// Phase-6 behavior is preserved when tls_mitm.enabled = false: a
// CONNECT request hits the 405 branch with Allow: GET, HEAD.
func TestServeHTTP_CONNECTWithoutHandler_Returns405(t *testing.T) {
	h := newTestHandler(t, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodConnect, "http://example.com/", nil)
	req.RequestURI = "example.com:443"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q, want 'GET, HEAD'", got)
	}
}

// TestServeHTTP_CONNECTWithHandler_DispatchesToConnect proves that
// once SetConnectHandler is wired, ServeHTTP routes CONNECT to it
// — the 405 path is bypassed and the stub records the call.
func TestServeHTTP_CONNECTWithHandler_DispatchesToConnect(t *testing.T) {
	h := newTestHandler(t, nil, nil)
	stub := &stubConnect{}
	h.SetConnectHandler(stub)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodConnect, "http://example.com/", nil)
	req.RequestURI = "example.com:443"
	h.ServeHTTP(rec, req)

	if got := stub.calls.Load(); got != 1 {
		t.Errorf("connect calls = %d, want 1", got)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (from stub)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "dispatched") {
		t.Errorf("body = %q, want stub-written 'dispatched'", rec.Body.String())
	}
}

// TestAllowHeader_SwitchesOnConnectInstall proves the §2.5 Allow
// header reflects whether the CONNECT pipeline is wired:
// "GET, HEAD" without it, "GET, HEAD, CONNECT" with it. We exercise
// the switch via an unrelated bad-method request (PUT) so the
// dispatcher doesn't intercept.
func TestAllowHeader_SwitchesOnConnectInstall(t *testing.T) {
	h := newTestHandler(t, nil, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "http://example.com/x", nil))
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("without connect: Allow = %q, want 'GET, HEAD'", got)
	}

	h.SetConnectHandler(&stubConnect{})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "http://example.com/x", nil))
	if got := rec.Header().Get("Allow"); got != "GET, HEAD, CONNECT" {
		t.Errorf("with connect: Allow = %q, want 'GET, HEAD, CONNECT'", got)
	}
}

// TestLogRequest_MITMField_PresentForMITMContext proves §6.2.1
// integration: a request whose context carries the MITM marker
// gets `mitm: true` on its log line; a plain request does not.
//
// We capture the slog output via a JSON handler attached to a
// bytes.Buffer; the test then walks the captured records.
func TestLogRequest_MITMField_PresentForMITMContext(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h := newTestHandler(t, nil, nil)
	h.logger = logger // override

	// Plain request — no MITM marker.
	req1 := httptest.NewRequest(http.MethodPut, "http://example.com/x", nil)
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, req1)

	// MITM-flagged request — same shape, but ctx carries the marker.
	req2 := httptest.NewRequest(http.MethodPut, "http://example.com/x", nil)
	req2 = req2.WithContext(proxy.WithMITMContext(context.Background()))
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)

	// Walk the JSON lines and find each request log entry.
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected ≥2 log lines, got %d:\n%s", len(lines), buf.String())
	}
	var sawPlainNoMitm, sawTaggedWithMitm bool
	for _, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["msg"] != "request" {
			continue
		}
		_, hasMITM := rec["mitm"]
		// We can't tell apart the two requests by URL (both PUT /x), so
		// we use the per-request order: the first record without mitm
		// proves the plain path; any record WITH mitm:true proves the
		// MITM path.
		if !hasMITM {
			sawPlainNoMitm = true
		}
		if mv, ok := rec["mitm"].(bool); ok && mv {
			sawTaggedWithMitm = true
		}
	}
	if !sawPlainNoMitm {
		t.Error("plain request log line missing — or carried 'mitm' key it shouldn't")
	}
	if !sawTaggedWithMitm {
		t.Error("MITM-tagged request log line missing 'mitm: true'")
	}
}
