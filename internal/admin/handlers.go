package admin

import (
	"context"
	"fmt"
	"net/http"
	"os"
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
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.cfg.Registry.Render(w)
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
		return
	}
	if err := s.checkCacheDirWritable(); err != nil {
		writeHealthzFail(w, "cache_dir")
		return
	}
	pingCtx, cancel := context.WithTimeout(r.Context(), 1*time.Second)
	defer cancel()
	if err := s.cfg.Cache.Ping(pingCtx); err != nil {
		writeHealthzFail(w, "db_ping")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
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

// handleStatus is a placeholder for the SPEC5 §9.7.3 status page.
// The full HTML/JSON content-negotiated render lands in a follow-up
// commit; this stub keeps the route alive end-to-end so the listener
// + auth + healthz + /metrics surface can be tested independently.
//
// AIDEV-TODO: render HTML via html/template + JSON via encoding/json,
// per SPEC5 §10.5 schema. Sourced from cache.ListSuitesWithAdoption,
// gc.GC.LastRunSummary, hostsem.Snapshot, observability.Ring.Snapshot.
// Apply the §9.7.3 5s per-DB-query timeout.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = fmt.Fprintf(w, `{"todo":"status page handler not yet implemented","uptime_seconds":%d}`+"\n",
			int64(time.Since(s.cfg.StartTime).Seconds()))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!DOCTYPE html>
<html><head><title>apt-cacher-ultra status</title></head>
<body><h1>apt-cacher-ultra</h1>
<p>Status page handler not yet implemented (SPEC5 §9.7.3 placeholder).</p>
<p><a href="/?format=json">View as JSON →</a></p>
<p><a href="/metrics">/metrics</a> &middot; <a href="/healthz">/healthz</a></p>
</body></html>
`))
}

// wantsJSON implements SPEC5 §9.7.3 content negotiation:
//   1. ?format=json query → JSON.
//   2. Accept: application/json AND not text/html → JSON.
//   3. Otherwise → HTML.
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
// Immediate first recompute, then loops at admin.gauge_refresh
// cadence until refresherStop is closed.
//
// AIDEV-NOTE: the refresher is wired to the metrics registry and
// performs the actual gauge updates via runRefreshOnce. The current
// commit installs a no-op recompute; counter-wiring (next commit)
// fills in the gauge-update logic. Keeps this file stable while
// the surrounding wiring lands.
func (s *Server) startRefresher() {
	s.mu.Lock()
	if s.refresherStop != nil {
		s.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	s.refresherStop = stop
	s.refresherDone = done
	s.mu.Unlock()

	period := s.cfg.Admin.GaugeRefresh.Duration
	go func() {
		defer close(done)
		// Immediate first recompute (SPEC5 §9.7.6).
		s.runRefreshOnce()
		t := time.NewTicker(period)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				s.runRefreshOnce()
			}
		}
	}()
}

// runRefreshOnce is the single recompute pass. Each gauge query
// runs under a 10s context.WithTimeout (SPEC5 §9.7.6 per-query
// timeout) — added in the counter-wiring commit. The pool-scan
// "in progress" guard is wired here.
func (s *Server) runRefreshOnce() {
	// SPEC5 §9.7.6 pool-scan in-progress guard. Skip the pool walk
	// if the prior invocation has not finished; other gauges still
	// recompute.
	if s.poolScanInProgress.CompareAndSwap(false, true) {
		defer s.poolScanInProgress.Store(false)
		// Pool walk — placeholder; counter wiring fills in.
	}
	// Other gauges — placeholder.
}
