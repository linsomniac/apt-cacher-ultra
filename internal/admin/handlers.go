package admin

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// handleMetrics serves the /metrics endpoint in Prometheus text
// exposition format. SPEC5 §9.7.2.
//
// AIDEV-NOTE: the metrics package's Render builds each metric's
// output into a strings.Builder under the per-metric lock, then
// writes outside the lock — so a slow scraper does not block
// request-path Inc/Observe/Set. This handler just routes the bytes
// to the response writer.
//
// Write errors (broken pipe, scraper disconnected mid-render) are
// captured by a wrapping io.Writer and surfaced as
// admin_scrape_error Warn after Render returns (SPEC5 §9.7.2 /
// §10.2). The scrape counter still increments because the scrape
// was attempted — the failure mode is observable from both the log
// line and the latency histogram.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	start := time.Now()
	defer func() {
		s.self.scrapeTotal.Inc()
		s.self.scrapeDurationSeconds.Observe(time.Since(start).Seconds())
	}()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	ew := &errCapturingWriter{w: w}
	s.cfg.Registry.Render(ew)
	if ew.err != nil {
		s.logger.Warn("admin_scrape_error",
			"err", ew.err.Error(),
			"bytes_written", ew.n)
	}
}

// errCapturingWriter wraps an io.Writer to record the first error
// seen and the running byte count. Render's per-metric write loop
// swallows individual io.WriteString errors (the metrics package's
// API is non-erroring), so the wrapper is the only signal a
// broken-pipe scrape leaves behind.
//
// Subsequent Writes after the first error fast-fail, so the rest of
// the registry render is short-circuited cheaply rather than
// repeatedly hitting the dead conn.
type errCapturingWriter struct {
	w   http.ResponseWriter
	n   int64
	err error
}

func (e *errCapturingWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.w.Write(p)
	e.n += int64(n)
	if err != nil {
		e.err = err
	}
	return n, err
}

// handleHealthz serves the /healthz endpoint. SPEC5 §9.7.4.
//
// Checks (in order; first failure short-circuits):
//
//  1. Process not in graceful shutdown — atomic load, microseconds.
//  2. Cache directory writable — os.CreateTemp + write + remove.
//  3. DB pingable — c.Ping under a 1s deadline.
//
// On all-pass: 200 ok\n. On any failure: 503 degraded\n with
// X-Acu-Check-Failed naming the failing check.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if s.shuttingDown.Load() {
		writeHealthzFail(w, "shutdown")
		s.self.healthzTotal.Inc("degraded")
		return
	}
	if err := s.checkCacheDirWritable(); err != nil {
		writeHealthzFail(w, "cache_dir")
		s.self.healthzTotal.Inc("degraded")
		return
	}
	pingCtx, cancel := context.WithTimeout(r.Context(), 1*time.Second)
	defer cancel()
	if err := s.cfg.Cache.Ping(pingCtx); err != nil {
		writeHealthzFail(w, "db_ping")
		s.self.healthzTotal.Inc("degraded")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
	s.self.healthzTotal.Inc("ok")
}

// checkCacheDirWritable creates a unique-suffix temp file under
// the cache directory, writes one byte, and removes it. SPEC5
// §9.7.4 check 2: CreateTemp avoids the race a fixed filename
// would create when concurrent probes overlap.
func (s *Server) checkCacheDirWritable() error {
	dir := s.cfg.Cache.Dir()
	f, err := os.CreateTemp(dir, ".acu-healthz-*")
	if err != nil {
		return err
	}
	name := f.Name()
	if _, werr := f.Write([]byte{0}); werr != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return werr
	}
	if cerr := f.Close(); cerr != nil {
		_ = os.Remove(name)
		return cerr
	}
	return os.Remove(name)
}

// writeHealthzFail writes the SPEC5 §9.7.4 503 response.
func writeHealthzFail(w http.ResponseWriter, check string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Acu-Check-Failed", check)
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte("degraded\n"))
}

// handleStatus serves the SPEC5 §9.7.3 status page. Content
// negotiation (?format=json or Accept: application/json) routes to
// the JSON form; everything else renders HTML. Both forms share
// the §10.5 schema. The actual render lives in renderStatus
// (status.go); this handler is the route entry-point.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.renderStatus(w, r)
}

// wantsJSON implements SPEC5 §9.7.3 content negotiation:
//  1. ?format=json query → JSON.
//  2. Accept: application/json AND not text/html → JSON.
//  3. Otherwise → HTML.
func wantsJSON(r *http.Request) bool {
	if r.URL.Query().Get("format") == "json" {
		return true
	}
	accept := r.Header.Get("Accept")
	if accept == "" {
		return false
	}
	wantsAny := func(h, mime string) bool {
		for _, part := range strings.Split(h, ",") {
			p := strings.TrimSpace(part)
			if p == mime {
				return true
			}
			// Allow params like "application/json; charset=utf-8".
			if len(p) > len(mime) && p[:len(mime)] == mime &&
				(p[len(mime)] == ';' || p[len(mime)] == ' ') {
				return true
			}
		}
		return false
	}
	return wantsAny(accept, "application/json") && !wantsAny(accept, "text/html")
}

// startRefresher launches the SPEC5 §9.7.6 refresher goroutine.
// The caller (Serve) is responsible for running the FIRST refresh
// synchronously before this returns — startRefresher only handles
// the periodic-tick loop.
//
// The goroutine inherits a per-server context.Context that
// Shutdown cancels via refresherCancel; in-flight queries unblock
// promptly when the process is shutting down.
func (s *Server) startRefresher() {
	s.mu.Lock()
	if s.refresherStop != nil {
		s.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	s.refresherStop = stop
	s.refresherDone = done
	s.refresherCancel = cancel
	s.mu.Unlock()

	period := s.cfg.Admin.GaugeRefresh.Duration
	go func() {
		defer close(done)
		t := time.NewTicker(period)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				s.runRefreshOnce(ctx)
			}
		}
	}()
}

// runRefreshOnce is the single recompute pass. Each gauge query
// runs under its own 10s context.WithTimeout (SPEC5 §9.7.6 per-query
// timeout) parented on lifecycleCtx, so a hung query cannot block
// subsequent gauges from refreshing AND a shutdown unblocks every
// query promptly.
//
// On query error: the gauge keeps its prior value, and a
// refresher_query_failed Warn fires with metric_name, err, and
// duration_ms. The next loop iteration retries.
//
// AIDEV-NOTE: the §9.7.6 "refresh in progress" guard wraps ONLY the
// pool walk; other queries proceed even when the prior pool walk is
// still running. This bounds parallelism on the slow filesystem path
// without serializing the cheap DB queries behind it.
func (s *Server) runRefreshOnce(lifecycleCtx context.Context) {
	s.refreshCacheStats(lifecycleCtx)
	s.refreshSuiteStats(lifecycleCtx)
	s.refreshRepoCoverage(lifecycleCtx)
	s.refreshCacheSummary(lifecycleCtx)
	s.refreshHostsemGauges()
	s.refreshPoolDiskBytes(lifecycleCtx)
	s.refreshProcessMetrics()
}

// refreshProcessMetrics reads /proc/self/* and updates the SPEC5
// §10.4.7 process collector gauges + cpu seconds counter. The
// counter Add reflects delta since the prior reading because our
// metrics.Counter primitive only supports monotonic Add — the
// /proc CPU value is monotonic, so the delta is always >= 0
// modulo a clock_gettime regression we don't expect under Linux.
//
// Read errors set the affected fields to zero (per SPEC5 §13:
// process metrics zeroed on non-Linux / missing /proc), no Warn
// fires.
func (s *Server) refreshProcessMetrics() {
	stats, err := readProcStats()
	if err != nil {
		// Best-effort: keep the prior gauge values, no log spam.
		return
	}
	if delta := stats.cpuSeconds - s.proc.loadPriorCPU(); delta > 0 {
		s.proc.cpuSecondsTotal.Add(delta)
	}
	s.proc.storePriorCPU(stats.cpuSeconds)
	s.proc.residentMemoryBytes.Set(float64(stats.residentMemoryBytes))
	s.proc.virtualMemoryBytes.Set(float64(stats.virtualMemoryBytes))
	s.proc.openFDs.Set(float64(stats.openFDs))
	s.proc.maxFDs.Set(float64(stats.maxFDs))
}

// refreshCacheStats updates the four cache.GetCacheStats-derived
// gauges. One DB transaction, three queries — all inside a single
// 10s deadline because they share the helper.
func (s *Server) refreshCacheStats(parent context.Context) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	stats, err := s.cfg.Cache.GetCacheStats(ctx)
	if err != nil {
		s.logRefresherFailure("acu_blobs_db_count", err, time.Since(start))
		return
	}
	s.gauges.blobsDBCount.Set(float64(stats.BlobCount))
	s.gauges.blobsDBTotalBytes.Set(float64(stats.TotalBytes))
	s.gauges.blobsZeroRefcountBacklog.Set(float64(stats.ZeroRefcountBacklog))
	s.gauges.urlPathsTracked.Set(float64(stats.URLPathCount))
}

// refreshSuiteStats updates acu_suites_tracked, acu_snapshots_current,
// and acu_snapshots_displaced from cache.GetSuiteStats. Displaced is
// AdoptedTotal - WithCurrentSnapshot per SPEC5 §9.7.6.
func (s *Server) refreshSuiteStats(parent context.Context) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	st, err := s.cfg.Cache.GetSuiteStats(ctx)
	if err != nil {
		s.logRefresherFailure("acu_suites_tracked", err, time.Since(start))
		return
	}
	displaced := st.AdoptedTotal - st.WithCurrentSnapshot
	if displaced < 0 {
		displaced = 0
	}
	s.gauges.suitesTracked.Set(float64(st.Tracked))
	s.gauges.snapshotsCurrent.Set(float64(st.WithCurrentSnapshot))
	s.gauges.snapshotsDisplaced.Set(float64(displaced))
}

// refreshRepoCoverage recomputes the SPEC6_5 §2.4 repo_coverage
// payload and updates both the cached value (status renderer consumes
// it without re-querying the DB) and the SPEC6_5 §10.3
// acu_package_hash_rows_by_kind gauge.
//
// AIDEV-NOTE: this method runs four aggregates inside a single
// read-only transaction; see cache.GetRepoCoverage for the SQL
// rationale. The refresher pattern means a /?format=json scrape sees
// values stale by up to admin.gauge_refresh — operationally fine for
// this surface because the per-kind counts only change at adoption
// time (snapshot lifecycle), not per-request.
func (s *Server) refreshRepoCoverage(parent context.Context) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	rc, err := s.cfg.Cache.GetRepoCoverage(ctx)
	if err != nil {
		s.logRefresherFailure("acu_package_hash_rows_by_kind", err, time.Since(start))
		return
	}
	s.repoCoverage.Store(&rc)
	s.gauges.packageHashRowsByKind.Set(float64(rc.PackageHashRowsBinary), "binary")
	s.gauges.packageHashRowsByKind.Set(float64(rc.PackageHashRowsSource), "source")
	s.gauges.packageHashRowsByKind.Set(float64(rc.PackageHashRowsPdiff), "pdiff")
}

// refreshCacheSummary recomputes the SPEC6_5 §2.4
// cache_summary.by_host[*].by_architecture payload. Same refresh
// cadence and stale-tolerance semantics as refreshRepoCoverage.
//
// AIDEV-NOTE: the (host, arch) cardinality is bounded by real-world
// fleet shapes (≤ tens of hosts × ≤ tens of arches); the two-query
// pattern in cache.GetCacheSummaryByHostArch handles this in
// milliseconds for typical caches. A future scale-test escalation
// would push this onto its own deadline rather than the shared 10s.
func (s *Server) refreshCacheSummary(parent context.Context) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	summary, err := s.cfg.Cache.GetCacheSummaryByHostArch(ctx)
	if err != nil {
		s.logRefresherFailure("acu_cache_summary_by_host_arch", err, time.Since(start))
		return
	}
	s.cacheSummaryByHostArch.Store(&summary)
}

// refreshHostsemGauges populates acu_active_hosts plus the labeled
// per-host gauges. Reset is called on the labeled gauges first so a
// host that no longer has hostsem state stops reporting stale values
// (SPEC5 §9.7.6).
//
// hostsem.Snapshot returns a copy under its own lock; no DB or
// network IO involved, so no deadline.
func (s *Server) refreshHostsemGauges() {
	snap := s.cfg.HostLimiter.Snapshot()
	s.gauges.activeHosts.Set(float64(s.cfg.HostLimiter.HostCount()))
	s.gauges.perHostInflight.Reset()
	s.gauges.perHostCapacity.Reset()
	for host, st := range snap {
		s.gauges.perHostInflight.Set(float64(st.Inflight), host)
		s.gauges.perHostCapacity.Set(float64(st.Capacity), host)
	}
}

// refreshPoolDiskBytes spawns the SPEC5 §9.7.6 pool walk goroutine
// (or skips, if a prior walk is still running). The walk runs OFF
// the refresher loop's goroutine — a multi-GiB cache can take
// seconds to walk, and the SPEC's "other queries proceed normally"
// guarantee requires the cheap DB queries not to block on it.
//
// The walk uses CompareAndSwap as the single-walk guard (SPEC's
// "skip the next interval rather than starting a parallel walk")
// and is tracked by walkWg so Shutdown waits for it to drain.
//
// AIDEV-NOTE: do NOT pass parent's context.WithTimeout-wrapped ctx
// here — the walk needs the lifecycleCtx that Shutdown cancels.
// The 10s per-query deadline is irrelevant for the walk: a
// multi-GiB filesystem walk can legitimately exceed 10s, and
// SPEC5 §9.7.6 explicitly handles overrun via the in-progress
// guard, not a deadline.
func (s *Server) refreshPoolDiskBytes(lifecycleCtx context.Context) {
	if !s.poolScanInProgress.CompareAndSwap(false, true) {
		return
	}
	// Gate Add(1) on shutdownStarted under s.mu so it can't race with
	// Shutdown's walkWg.Wait. Without the lock, an Add(1) called from
	// Serve's synchronous initial refresh can run concurrently with a
	// Wait triggered by an early Shutdown (the test path) — that's the
	// "Add on zero counter concurrent with Wait" race sync.WaitGroup
	// explicitly forbids.
	s.mu.Lock()
	if s.shutdownStarted {
		s.mu.Unlock()
		s.poolScanInProgress.Store(false)
		return
	}
	s.walkWg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.walkWg.Done()
		defer s.poolScanInProgress.Store(false)
		s.runPoolWalk(lifecycleCtx)
	}()
}

// runPoolWalk performs the actual filepath.Walk and sets the gauge.
// Separated from refreshPoolDiskBytes for testability and to keep
// the spawn site small.
func (s *Server) runPoolWalk(ctx context.Context) {
	start := time.Now()
	poolDir := filepath.Join(s.cfg.Cache.Dir(), "pool")
	var total int64
	err := filepath.Walk(poolDir, func(_ string, info os.FileInfo, walkErr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if walkErr != nil {
			// Tolerate transient races (e.g. a blob unlinked
			// between walk and stat); the next pass picks up the
			// new state.
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		// Missing pool/ pre-Open is the normal case during the
		// startup window between admin Serve and cache.Open. Don't
		// log noisily. Same for ctx.Canceled during shutdown.
		if os.IsNotExist(err) || errors.Is(err, context.Canceled) {
			return
		}
		s.logRefresherFailure("acu_pool_disk_bytes", err, time.Since(start))
		return
	}
	s.gauges.poolDiskBytes.Set(float64(total))
}

// logRefresherFailure emits the SPEC5 §9.7.6 refresher_query_failed
// Warn. Centralized here so every gauge-recompute call site logs in
// the same shape.
func (s *Server) logRefresherFailure(metricName string, err error, dur time.Duration) {
	s.logger.Warn("refresher_query_failed",
		"metric_name", metricName,
		"err", err.Error(),
		"duration_ms", dur.Milliseconds(),
	)
}
