package main

// SPEC6 §15 #4 daemon-level disabled-mode parity DoD pin.
//
// §15 #4 lists three intentional advertisement-only deltas that
// disabled-mode (`tls_mitm.enabled = false`, the default) reveals
// over a Phase 5 daemon:
//
//   1. CONNECT response: 405 with Allow: GET, HEAD.
//   2. Status JSON gains a top-level `tls_mitm` key with payload
//      {"enabled": false} — no other keys.
//   3. `acu_mitm_*` metrics ARE registered (so /metrics scrapes are
//      stable across enabled/disabled), but ALL counters and
//      histograms remain at zero. Gauges (cert_cache_size,
//      cert_cache_capacity, ca_not_after_unixtime) report zero.
//
// Each delta is already pinned at the unit level
// (internal/handler/disabled_mode_parity_test.go for log+metric
// silence + 405+Allow at the handler layer; internal/admin/admin_test.go
// for the JSON shape; internal/proxy/mitm_metrics_test.go for
// register-at-init). This test pins the SAME invariants through the
// production wiring path: config drives main.go's `if cfg.TlsMitm.Enabled
// { wireTlsMitm(...) }` gate at main.go:498, and we observe the
// daemon at the wire (cache port + admin port). A regression that
// accidentally calls wireTlsMitm in the disabled branch (or skips
// it in the enabled branch) wouldn't be caught by the unit tests
// alone — those tests don't cross the main.go boundary.
//
// Mutates package-level shutdownTimeout, so NOT t.Parallel.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
)

func TestServe_DisabledMode_AdvertisedDeltasOnly(t *testing.T) {
	oldTimeout := shutdownTimeout
	shutdownTimeout = 500 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = oldTimeout })

	// admin.New registers gauges into metrics.Default unconditionally
	// at startup. Snapshot names BEFORE the daemon brings up the admin
	// server; the t.Cleanup below cancels the daemon AND THEN
	// unregisters added-since names so the gauge refresher has
	// finished writing before we drop them — same ordering pattern
	// as status_tls_mitm_integration_test.go:t.Cleanup.
	preMetrics := metrics.Default.SnapshotNamesForTest()

	cacheDir := t.TempDir()
	cfg := minimalCfg(cacheDir, nil)
	// Sanity guard: this test's whole point is exercising the
	// `tls_mitm.enabled = false` branch, which is the default after
	// minimalCfg + Defaults(). If a future config refactor flips the
	// default the test would silently exercise the wrong branch.
	if cfg.TlsMitm.Enabled {
		t.Fatalf("test infrastructure broken: minimalCfg yields TlsMitm.Enabled=true; this test requires the disabled default")
	}
	// Admin block — minimalCfg + Defaults() does not populate the
	// presence-sensitive admin defaults that Load() applies via
	// TOML's md.IsDefined; set them here so the admin server can
	// build without a NewTicker(0) panic on GaugeRefresh.
	cfg.Admin = config.AdminConfig{
		Enabled:         true,
		GaugeRefresh:    config.Duration{Duration: 50 * time.Millisecond},
		ReadTimeout:     config.Duration{Duration: 5 * time.Second},
		IdleTimeout:     config.Duration{Duration: 30 * time.Second},
		MetricSeriesCap: 1024,
	}

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	cacheLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen cache: %v", err)
	}
	cacheAddr := cacheLn.Addr().String()
	adminLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = cacheLn.Close()
		t.Fatalf("listen admin: %v", err)
	}
	adminAddr := adminLn.Addr().String()
	cfg.Admin.Listen = adminAddr

	// Reset the three §15 #4 gauges to zero. metrics.Default is
	// package-shared: an §15 #11 enabled-mode test running earlier in
	// the same `go test` invocation leaves SetCertCacheCapacity /
	// SetCANotAfterUnixtime values sticky (gauge.Set retains the last
	// write). Reset them here so the post-daemon assertion can pin the
	// spec invariant DIRECTLY ("Gauges report zero" — SPEC6 §15 #4)
	// rather than the weaker delta-based shape. Prior callers (other
	// tests) re-set their own values via wireTlsMitm at startup; the
	// reset is local to this test's lifetime.
	proxy.SetCertCacheSize(0)
	proxy.SetCertCacheCapacity(0)
	proxy.SetCANotAfterUnixtime(0)

	// Snapshot the counter/histogram values BEFORE the daemon starts.
	// Counters and histograms are MONOTONIC — there's no "reset to 0"
	// API and writing 0 to a counter is a no-op. So those still need
	// the delta shape (any prior observations remain in the absolute
	// sum). The disabled daemon should produce zero DELTA, which is
	// what §15 #4 "no observation happens until enabled" demands.
	baseline := scrapeMetrics(t)

	ctx, cancel := context.WithCancel(context.Background())

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- serveListeners(ctx, cfg, logger, cacheLn, nil, adminLn)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-serveDone:
			if err != nil {
				t.Errorf("serveListeners: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Errorf("serveListeners did not return on cleanup")
		}
		metrics.Default.UnregisterAddedSinceForTest(preMetrics)
	})

	if err := waitForDaemonReady(t, cacheAddr, 10*time.Second); err != nil {
		t.Fatalf("daemon never became ready: %v", err)
	}

	// Delta 1: CONNECT → 405 with Allow: GET, HEAD.
	//
	// Raw TCP CONNECT (not openCONNECT, which asserts 200). In disabled
	// mode the handler's CONNECT-without-handler branch fires at
	// internal/handler/handler.go's ServeHTTP method-switch.
	rawConn, err := net.Dial("tcp", cacheAddr)
	if err != nil {
		t.Fatalf("dial cache: %v", err)
	}
	defer rawConn.Close()
	if err := rawConn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set CONNECT deadline: %v", err)
	}
	if _, err := rawConn.Write([]byte("CONNECT example.test:443 HTTP/1.1\r\nHost: example.test:443\r\n\r\n")); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	connectResp, err := http.ReadResponse(bufio.NewReader(rawConn), nil)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	_ = connectResp.Body.Close()
	if connectResp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("disabled-mode CONNECT status = %d, want %d", connectResp.StatusCode, http.StatusMethodNotAllowed)
	}
	if got := connectResp.Header.Get("Allow"); got != "GET, HEAD" {
		t.Errorf("disabled-mode CONNECT Allow = %q, want %q", got, "GET, HEAD")
	}

	// Delta 2: status JSON has top-level tls_mitm = {"enabled": false}
	// and ONLY that key. The "abbreviated shape" rule is in §10.4 and
	// reasserted in §15 #4. internal/admin/admin_test.go covers the
	// renderer with a stub provider; here we cover the production
	// wiring through admin.New + its tlsMitmProvider for the disabled
	// branch (a regression in the disabled-branch provider would slip
	// past the stub-driven unit tests).
	client := &http.Client{Timeout: 5 * time.Second}
	statusResp, err := client.Get("http://" + adminAddr + "/?format=json")
	if err != nil {
		t.Fatalf("GET admin /?format=json: %v", err)
	}
	statusBody, readErr := io.ReadAll(statusResp.Body)
	_ = statusResp.Body.Close()
	if readErr != nil {
		t.Fatalf("read status JSON body: %v", readErr)
	}
	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("status JSON status=%d body=%s", statusResp.StatusCode, statusBody)
	}
	var statusPayload map[string]any
	if err := json.Unmarshal(statusBody, &statusPayload); err != nil {
		t.Fatalf("decode status JSON: %v\nbody:\n%s", err, statusBody)
	}
	tlsMitmAny, ok := statusPayload["tls_mitm"]
	if !ok {
		t.Fatalf("status JSON missing tls_mitm top-level key; SPEC6 §10.4 mandates always-present\nbody:\n%s", statusBody)
	}
	tlsMitm, ok := tlsMitmAny.(map[string]any)
	if !ok {
		t.Fatalf("tls_mitm value type = %T, want object\nbody:\n%s", tlsMitmAny, statusBody)
	}
	enabled, ok := tlsMitm["enabled"].(bool)
	if !ok {
		t.Errorf("tls_mitm.enabled type = %T, want bool", tlsMitm["enabled"])
	} else if enabled {
		t.Errorf("tls_mitm.enabled = true; want false in disabled mode")
	}
	for k := range tlsMitm {
		if k != "enabled" {
			t.Errorf("disabled-mode tls_mitm has unexpected key %q; spec mandates abbreviated shape\nbody:\n%s", k, statusBody)
		}
	}

	// Delta 3a: every acu_mitm_* metric NAME is present in /metrics
	// output. Hit the HTTP endpoint (rather than scrapeMetrics's
	// in-process Render) because §15 #4 specifically calls out
	// "/metrics scrapes are stable across enabled/disabled" — the
	// HTTP path is what an external Prometheus scrape sees.
	metricsResp, err := client.Get("http://" + adminAddr + "/metrics")
	if err != nil {
		t.Fatalf("GET admin /metrics: %v", err)
	}
	metricsBody, readErr := io.ReadAll(metricsResp.Body)
	_ = metricsResp.Body.Close()
	if readErr != nil {
		t.Fatalf("read /metrics body: %v", readErr)
	}
	if metricsResp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status=%d body=%s", metricsResp.StatusCode, metricsBody)
	}
	wantMetrics := []string{
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
	metricsText := string(metricsBody)
	for _, m := range wantMetrics {
		if !metricRegistered(metricsText, m) {
			t.Errorf("/metrics output missing TYPE line for %q in disabled mode; SPEC6 §15 #4 mandates registration even when disabled", m)
		}
	}

	// Delta 3b: gauges read 0 in disabled mode. The setup block
	// reset these gauges to 0 before the daemon started; the spec
	// says wireTlsMitm at startup is the only writer (which doesn't
	// run in disabled mode), so 0 should hold. A registered-but-zero
	// gauge emits an exposition value line (`<name> 0`) — readGauge-
	// ValuePresent disambiguates that from "metric absent entirely"
	// (which would be a registration regression, separately caught
	// by 3a).
	after := scrapeMetrics(t)
	for _, gauge := range []string{
		"acu_mitm_cert_cache_size",
		"acu_mitm_cert_cache_capacity",
		"acu_mitm_ca_not_after_unixtime",
	} {
		v, present := readGaugeValuePresent(after, gauge)
		if !present {
			t.Errorf("disabled-mode gauge %s value line missing from scrape (registration regression?)", gauge)
			continue
		}
		if v != 0 {
			t.Errorf("disabled-mode gauge %s = %g, want 0 (SPEC6 §15 #4 'Gauges report zero')", gauge, v)
		}
	}

	// Delta 3c: counters and histograms have ZERO delta against
	// baseline. Because metrics.Default is package-shared, a prior
	// test in the run may have left non-zero absolute values; the
	// invariant the spec actually mandates is "no observation happens"
	// — i.e., zero delta from the disabled daemon's lifetime.
	for _, family := range []string{
		"acu_mitm_connect_total",
		"acu_mitm_cert_cache_lookups_total",
		"acu_mitm_cert_issued_total",
		"acu_mitm_cert_evicted_total",
	} {
		before := sumCounterFamily(baseline, family)
		afterVal := sumCounterFamily(after, family)
		if afterVal != before {
			t.Errorf("disabled-mode counter %s changed: before=%g after=%g (delta=%g)",
				family, before, afterVal, afterVal-before)
		}
	}
	for _, hist := range []string{
		"acu_mitm_connect_duration_seconds",
		"acu_mitm_handshake_duration_seconds",
	} {
		before := readHistogramCount(baseline, hist)
		afterVal := readHistogramCount(after, hist)
		if afterVal != before {
			t.Errorf("disabled-mode histogram %s_count changed: before=%g after=%g (delta=%g)",
				hist, before, afterVal, afterVal-before)
		}
	}
}

// metricRegistered reports whether the Prometheus exposition output
// `scrape` declares the given metric family via a `# TYPE <name> ...`
// line. This is stricter than substring matching: a substring hit
// could fall on a HELP/TYPE comment for a SIMILAR-named metric, or on
// a series of a metric whose family name happens to embed `name`. The
// TYPE line is the canonical "this metric is registered" signal in
// the exposition format.
func metricRegistered(scrape, name string) bool {
	prefix := "# TYPE " + name + " "
	for _, line := range strings.Split(scrape, "\n") {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// readGaugeValuePresent returns (value, true) if a gauge value line
// for `name` appears in `scrape`, or (0, false) if no value line is
// present. The "registered but zero" case (a gauge the producer has
// not Set since process start) still emits a `<name> 0` line, so
// `present=false` is a strong signal that the metric is unregistered
// — distinct from the readGaugeValue case where 0 is ambiguous.
func readGaugeValuePresent(scrape, name string) (float64, bool) {
	for _, line := range strings.Split(scrape, "\n") {
		if strings.HasPrefix(line, "# ") {
			continue
		}
		if !strings.HasPrefix(line, name) {
			continue
		}
		rest := line[len(name):]
		// Reject labeled lines and partial-name matches (e.g.
		// `<name>_count` for a histogram). Gauge value lines are
		// `<name> <value>` — the next byte is a space.
		if !strings.HasPrefix(rest, " ") {
			continue
		}
		var v float64
		if _, err := fmt.Sscanf(strings.TrimSpace(rest), "%g", &v); err != nil {
			continue
		}
		return v, true
	}
	return 0, false
}
