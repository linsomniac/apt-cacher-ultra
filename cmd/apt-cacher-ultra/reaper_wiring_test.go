package main

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
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
		case err := <-serveDone:
			if err != nil {
				t.Errorf("serveListeners: %v", err) // the Task 11 review's minor 4
			}
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

// The Task 11 review's Important 1 (design note 1): the daemon starts the
// reaper whatever gc.enabled and freshness.periodic_refresh say — here GC
// is off and the checker's scheduler returns at once — on the lifetime
// context, and joins it before serveListeners returns (freshWG). The
// swapped runReaper records its start, then waits for its context and
// lingers before returning, so a reaper started outside the wait group
// would still be running when serveListeners returned. Mutates runReaper
// and shutdownTimeout, so NOT t.Parallel.
func TestServeListeners_StartsTheReaperWhateverGCSays(t *testing.T) {
	oldTimeout, oldRun := shutdownTimeout, runReaper
	shutdownTimeout = 500 * time.Millisecond
	started := make(chan *freshness.Reaper, 1)
	var returned atomic.Bool
	runReaper = func(r *freshness.Reaper, ctx context.Context) {
		started <- r
		<-ctx.Done()
		time.Sleep(200 * time.Millisecond)
		returned.Store(true)
	}
	t.Cleanup(func() { shutdownTimeout, runReaper = oldTimeout, oldRun })
	preMetrics := metrics.Default.SnapshotNamesForTest()
	t.Cleanup(func() { metrics.Default.UnregisterAddedSinceForTest(preMetrics) })

	cfg := minimalCfg(t.TempDir(), nil)
	cfg.GC.Enabled = false
	cfg.Freshness.PeriodicRefresh.Duration = 0
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveDone := make(chan error, 1)
	go func() { serveDone <- serveListeners(ctx, cfg, silentBuildLogger(), ln, nil, nil, nil) }()
	select {
	case r := <-started:
		if r == nil {
			t.Fatal("runReaper was called with a nil reaper")
		}
	case err := <-serveDone:
		t.Fatalf("serveListeners returned (%v) without starting the reaper", err)
	case <-time.After(15 * time.Second):
		t.Fatal("the daemon did not start the reaper with gc.enabled = false and periodic_refresh = 0")
	}
	cancel()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("serveListeners: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serveListeners did not return")
	}
	if !returned.Load() {
		t.Fatal("serveListeners returned before the reaper did: it is not joined (freshWG)")
	}
}

// The Task 11 review's minor 1: the startup line reports
// gc.suite_idle_ttl, and adoption.adopt_unsigned, with their configured
// values.
func TestServeListeners_StartupLineReportsTheReapingKnobs(t *testing.T) {
	recs := startupLogs(t, t.TempDir(), "[gc]\nsuite_idle_ttl = \"48h\"\n[adoption]\nadopt_unsigned = false\n")
	for _, rec := range recs {
		if rec["msg"] != "apt-cacher-ultra starting" {
			continue
		}
		if rec["gc_suite_idle_ttl"] != float64(48*time.Hour) {
			t.Errorf("gc_suite_idle_ttl = %v, want 48h in nanoseconds", rec["gc_suite_idle_ttl"])
		}
		if rec["adoption_adopt_unsigned"] != false {
			t.Errorf("adoption_adopt_unsigned = %v, want false", rec["adoption_adopt_unsigned"])
		}
		return
	}
	t.Fatal("startup line not found")
}
