package main

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
)

// Spec C11: gc.suite_idle_ttl reaches the reaper.
func TestReaperConfig_CarriesSuiteIdleTTL(t *testing.T) {
	cfg := minimalCfg(t.TempDir(), nil)
	cfg.GC.SuiteIdleTTL.Duration = 48 * time.Hour
	if rc := reaperConfig(cfg, nil, nil, silentBuildLogger()); rc.IdleTTL != 48*time.Hour {
		t.Fatalf("IdleTTL = %v, want gc.suite_idle_ttl (48h)", rc.IdleTTL)
	}
}

// Spec C9 (D9): the daemon wires the reaper as the admin listener's
// Forgetter — POST /forget for a suite the cache does not track answers
// 409 (busy or unknown), not 501. Mutates shutdownTimeout, so NOT
// t.Parallel.
func TestServe_ForgetIsWired(t *testing.T) {
	oldTimeout := shutdownTimeout
	shutdownTimeout = 500 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = oldTimeout })
	preMetrics := metrics.Default.SnapshotNamesForTest()

	cfg := minimalCfg(t.TempDir(), nil)
	cfg.Admin = config.AdminConfig{
		Enabled:         true,
		GaugeRefresh:    config.Duration{Duration: 50 * time.Millisecond},
		ReadTimeout:     config.Duration{Duration: 5 * time.Second},
		IdleTimeout:     config.Duration{Duration: 30 * time.Second},
		MetricSeriesCap: 1024,
	}
	cacheLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	adminLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = cacheLn.Close()
		t.Fatal(err)
	}
	cfg.Admin.Listen = adminLn.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- serveListeners(ctx, cfg, silentBuildLogger(), cacheLn, nil, adminLn, nil) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-serveDone:
		case <-time.After(15 * time.Second):
			t.Errorf("serveListeners did not return on cleanup")
		}
		metrics.Default.UnregisterAddedSinceForTest(preMetrics)
	})
	if err := waitForDaemonReady(t, cacheLn.Addr().String(), 10*time.Second); err != nil {
		t.Fatalf("daemon never became ready: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+cfg.Admin.Listen+"/forget",
		strings.NewReader("host=deb.example&suite=/debian/dists/stable"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-ACU-Admin", "1")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("POST /forget = %d, want 409 from the wired reaper", resp.StatusCode)
	}
}
