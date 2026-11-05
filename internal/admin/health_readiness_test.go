package admin

import (
	"context"
	"net/http/httptest"
	"testing"
)

func TestHealthzUsesDaemonReadinessAndRechecksShutdown(t *testing.T) {
	reason := "startup"
	s, ln, c := newAdminServer(t, func(cfg *Config) {
		cfg.HealthCheck = func(context.Context) string { return reason }
	})
	defer func() { _ = ln.Close() }()
	defer func() { _ = c.Close() }()
	for _, failure := range []string{"startup", "proxy_tls", "shutdown", ""} {
		reason = failure
		rr := httptest.NewRecorder()
		s.buildHandler().ServeHTTP(rr, httptest.NewRequest("GET", "/healthz", nil))
		status := 200
		if reason != "" {
			status = 503
		}
		if rr.Code != status || rr.Header().Get("X-Acu-Check-Failed") != failure {
			t.Fatalf("%s: %d %v", failure, rr.Code, rr.Header())
		}
	}
	s.cfg.HealthCheck = func(context.Context) string { s.shuttingDown.Store(true); return "" }
	rr := httptest.NewRecorder()
	s.handleHealthz(rr, httptest.NewRequest("GET", "/healthz", nil))
	if rr.Code != 503 || rr.Header().Get("X-Acu-Check-Failed") != "shutdown" {
		t.Fatal("probe reported healthy during shutdown")
	}
}
