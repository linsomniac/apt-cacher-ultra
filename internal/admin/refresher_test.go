package admin

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/gc"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
	"github.com/linsomniac/apt-cacher-ultra/internal/observability"
)

// TestRefresher_PopulatesCacheGaugesOnStartup confirms the §9.7.6
// invariant that the first /metrics scrape after startup sees
// populated values, not zeros — the refresher's "immediate first
// recompute" runs before Serve returns its first scrape body.
func TestRefresher_PopulatesCacheGaugesOnStartup(t *testing.T) {
	s, base, cleanup := startAdminServer(t)
	defer cleanup()

	// Seed a couple of blobs so the gauges have non-zero values.
	for i, body := range []string{"alpha bytes", "beta"} {
		w, err := s.cfg.Cache.NewTempBlob()
		if err != nil {
			t.Fatalf("NewTempBlob[%d]: %v", i, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("Write[%d]: %v", i, err)
		}
		hash, err := w.Finalize(int64(len(body)))
		if err != nil {
			t.Fatalf("Finalize[%d]: %v", i, err)
		}
		if err := s.cfg.Cache.PutBlob(context.Background(), hash, int64(len(body))); err != nil {
			t.Fatalf("PutBlob[%d]: %v", i, err)
		}
	}

	// startAdminServer configures GaugeRefresh=50ms, so the loop has
	// already iterated once or twice by the time we hit /metrics.
	// Wait one tick to be sure the seeded rows are visible.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp := mustGet(t, base+"/metrics")
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if strings.Contains(string(body), "acu_blobs_db_count 2") {
			return
		}
		time.Sleep(60 * time.Millisecond)
	}
	t.Errorf("acu_blobs_db_count did not reach 2 within deadline")
}

// TestRefresher_ScrapeContainsAllExpectedGauges enumerates every
// SPEC5 §10.4.6 / §10.4.7 / §10.4.2 gauge name and confirms each
// renders at least the HELP/TYPE preamble after the first refresh.
// Guards against accidental gauge-name drift between SPEC5 and
// the registration site in gauges.go.
func TestRefresher_ScrapeContainsAllExpectedGauges(t *testing.T) {
	_, base, cleanup := startAdminServer(t)
	defer cleanup()

	// Wait for the first refresh to populate at least the unlabeled
	// gauges (per-host gauges remain absent until a host is seen).
	want := []string{
		"acu_blobs_db_count",
		"acu_blobs_db_total_bytes",
		"acu_blobs_zero_refcount_backlog",
		"acu_url_paths_tracked",
		"acu_suites_tracked",
		"acu_snapshots_current",
		"acu_snapshots_displaced",
		"acu_pool_disk_bytes",
		"acu_active_hosts",
		"acu_build_info",
		"acu_process_start_unixtime",
		// SPEC5 §10.4.7 standard process metrics.
		"process_resident_memory_bytes",
		"process_virtual_memory_bytes",
		"process_open_fds",
		"process_max_fds",
		"process_start_time_seconds",
		// SPEC6_5 §10.3 per-kind package_hash gauge. Always present
		// from first refresh (metric declared even when no rows exist).
		"acu_package_hash_rows_by_kind",
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp := mustGet(t, base+"/metrics")
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		bodyStr := string(body)

		missing := []string{}
		for _, name := range want {
			if !strings.Contains(bodyStr, "# TYPE "+name+" gauge") {
				missing = append(missing, name)
			}
		}
		if len(missing) == 0 {
			return
		}
		time.Sleep(60 * time.Millisecond)
	}

	// Final attempt for diagnostic output.
	resp := mustGet(t, base+"/metrics")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	bodyStr := string(body)
	for _, name := range want {
		if !strings.Contains(bodyStr, "# TYPE "+name+" gauge") {
			t.Errorf("scrape missing gauge %q", name)
		}
	}
}

// TestRefresher_BuildInfoCarriesLabels confirms the §10.4.7
// info-shaped pattern: gauge=1, version/go_version/vcs_revision in
// labels. The startAdminServer helper sets BuildInfo{v0.test, ...}.
func TestRefresher_BuildInfoCarriesLabels(t *testing.T) {
	_, base, cleanup := startAdminServer(t)
	defer cleanup()

	resp := mustGet(t, base+"/metrics")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	if !strings.Contains(got, `acu_build_info{version="v0.test",go_version="go-test",vcs_revision="deadbeef"} 1`) {
		t.Errorf("acu_build_info series missing or wrong;\nfull body:\n%s", got)
	}
}

// TestRefresher_ProcessCPUCounterAppears confirms the §10.4.7
// process_cpu_seconds_total is exposed as a counter (not gauge)
// and renders at least the HELP/TYPE preamble. The actual cpu
// value is non-deterministic, so we only assert the metric is
// declared.
func TestRefresher_ProcessCPUCounterAppears(t *testing.T) {
	_, base, cleanup := startAdminServer(t)
	defer cleanup()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp := mustGet(t, base+"/metrics")
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if strings.Contains(string(body), "# TYPE process_cpu_seconds_total counter") {
			return
		}
		time.Sleep(60 * time.Millisecond)
	}
	t.Errorf("process_cpu_seconds_total counter not visible within deadline")
}

// TestRefresher_ProcessCPUCounterRendersSample asserts the
// process_cpu_seconds_total counter renders an actual sample
// line, not just HELP/TYPE. Codex finding: when CPU consumed is
// 0 (boot, non-Linux), Counter.Add(0) is needed to materialize
// the series. Without that primer, the metric appears as bare
// HELP/TYPE which some Prometheus tooling rejects.
func TestRefresher_ProcessCPUCounterRendersSample(t *testing.T) {
	_, base, cleanup := startAdminServer(t)
	defer cleanup()

	resp := mustGet(t, base+"/metrics")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	if !strings.Contains(got, "# TYPE process_cpu_seconds_total counter") {
		t.Errorf("missing TYPE line; body:\n%s", got)
	}
	// The series sample line begins with the metric name followed
	// by a space (no labels). Match either "process_cpu_seconds_total 0"
	// or any non-zero rendering.
	lines := strings.Split(got, "\n")
	sampleSeen := false
	for _, ln := range lines {
		if strings.HasPrefix(ln, "process_cpu_seconds_total ") {
			sampleSeen = true
			break
		}
	}
	if !sampleSeen {
		t.Errorf("process_cpu_seconds_total HELP/TYPE rendered with NO sample line — primer Add(0) missing?\nbody:\n%s", got)
	}
}

// TestRefresher_HostsemGaugesAppearAfterAcquire validates the
// §10.4.2 invariant: per-host gauges are populated from
// hostsem.Snapshot, and a host that holds a slot has its inflight
// reflected in the next refresh tick.
func TestRefresher_HostsemGaugesAppearAfterAcquire(t *testing.T) {
	s, base, cleanup := startAdminServer(t)
	defer cleanup()

	rel, err := s.cfg.HostLimiter.Acquire(context.Background(), "archive.example.com")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer rel()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp := mustGet(t, base+"/metrics")
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		got := string(body)
		if strings.Contains(got, `acu_per_host_inflight{host="archive.example.com"} 1`) &&
			strings.Contains(got, `acu_per_host_capacity{host="archive.example.com"} 8`) {
			return
		}
		time.Sleep(60 * time.Millisecond)
	}
	t.Errorf("per-host inflight=1 / capacity=8 not visible within deadline")
}

// TestRefresher_FirstScrapeSeesPopulatedGauges asserts the SPEC5
// §3.2 invariant that the first /metrics scrape sees populated
// values rather than the zero-state. Codex review found that the
// prior implementation ran the first refresh in the goroutine,
// racing with the HTTP server starting; the fix runs the first
// refresh synchronously in Serve before listener accept.
//
// We seed two blobs BEFORE startAdminServer's Serve goroutine
// runs, then immediately scrape. Because Serve runs runRefreshOnce
// synchronously before server.Serve, the very first scrape must
// observe acu_blobs_db_count=2 — no polling, no sleep.
func TestRefresher_FirstScrapeSeesPopulatedGauges(t *testing.T) {
	dir := t.TempDir()
	c, err := cache.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	defer func() { _ = c.Close() }()
	for _, body := range []string{"first", "second"} {
		w, err := c.NewTempBlob()
		if err != nil {
			t.Fatalf("NewTempBlob: %v", err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("Write: %v", err)
		}
		hash, err := w.Finalize(int64(len(body)))
		if err != nil {
			t.Fatalf("Finalize: %v", err)
		}
		if err := c.PutBlob(context.Background(), hash, int64(len(body))); err != nil {
			t.Fatalf("PutBlob: %v", err)
		}
	}

	g, err := gc.New(gc.Config{
		Cache:               c,
		Logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		Enabled:             true,
		Interval:            time.Hour,
		BatchSize:           100,
		SnapshotBatchSize:   10,
		MaxTickDuration:     time.Minute,
		BlobGrace:           time.Hour,
		KeepDisplaced:       3,
		PoolScanWorkers:     2,
		HeartbeatStaleGrace: 30 * time.Minute,
	})
	if err != nil {
		t.Fatalf("gc.New: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cfg := Config{
		Cache:       c,
		GC:          g,
		HostLimiter: hostsem.New(8),
		Ring:        observability.NewRing(50),
		Registry:    metrics.NewRegistry(),
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Admin: config.AdminConfig{
			Enabled:         true,
			GaugeRefresh:    config.Duration{Duration: time.Hour}, // ensure only the synchronous first refresh fires
			ReadTimeout:     config.Duration{Duration: 5 * time.Second},
			IdleTimeout:     config.Duration{Duration: 30 * time.Second},
			MetricSeriesCap: 1024,
		},
		StartTime: time.Now(),
		AdminAddr: ln.Addr().String(),
	}
	s, err := New(cfg)
	if err != nil {
		_ = ln.Close()
		t.Fatalf("admin.New: %v", err)
	}
	serveDone := make(chan struct{})
	go func() {
		_ = s.Serve(ln)
		close(serveDone)
	}()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
		<-serveDone
	}()

	// First scrape — no polling. With GaugeRefresh=1h, only the
	// synchronous first refresh in Serve has run. If that refresh
	// did NOT happen synchronously, this fails.
	resp := mustGet(t, "http://"+cfg.AdminAddr+"/metrics")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "acu_blobs_db_count 2") {
		t.Errorf("first scrape did not see acu_blobs_db_count=2 (synchronous first refresh broken);\nbody:\n%s", body)
	}
}

// TestRefresher_RunRefreshOnceDirect drives runRefreshOnce
// synchronously without the goroutine, so a unit test can assert
// gauge values without sleeping. Confirms the refresher does not
// panic on an empty cache (every gauge falls to its zero baseline).
func TestRefresher_RunRefreshOnceDirect(t *testing.T) {
	s, _, cleanup := startAdminServer(t)
	defer cleanup()

	// Drain the running refresher so it does not race against the
	// direct call. Shutdown() does this; tests that follow can
	// still scrape /metrics because the gauges remain in the
	// registry — the goroutine just stops updating them.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.Shutdown(ctx)

	// Now drive a fresh recompute by hand. With an empty seed cache,
	// every gauge legitimately settles at 0. Just verify it returns
	// without panic.
	s.runRefreshOnce(context.Background())
}
