package main

// SPEC6 §15 #11 DoD pin — every CONNECT-fired acu_mitm_* metric
// increments at least once from baseline when the integration
// daemon services a CONNECT.
//
// The unit tests in internal/proxy/mitm_metrics_test.go pin the
// recorder primitives in isolation (RecordCertIssued bumps the
// counter, etc.). This test pins the WIRING: that ServeCONNECT
// + the leaf-gen wrapper + the leaf-cache hooks actually call
// those recorders during a real CONNECT through the daemon.
//
// Coverage of the §10.3 metric table:
//
//   acu_mitm_connect_total{outcome}              ✓ delta ≥ 1
//   acu_mitm_connect_duration_seconds_count      ✓ delta ≥ 1
//   acu_mitm_cert_cache_lookups_total{outcome=miss}  ✓ delta ≥ 1
//   acu_mitm_cert_issued_total{algorithm=ecdsa-p256} ✓ delta ≥ 1
//   acu_mitm_cert_cache_capacity                 ✓ value > 0 (set at startup)
//   acu_mitm_ca_not_after_unixtime               ✓ value > 0 (set at startup)
//
// Deferred (not in this pin):
//
//   acu_mitm_handshake_duration_seconds — needs a fully-completed
//     TLS handshake which requires the §15 #2 HTTPS scaffold to
//     install the daemon CA into a test client trust store.
//   acu_mitm_cert_cache_size — gauge updated by the §10.3 30s
//     refresher; pinning here would force a refresh-interval
//     override + a poll loop.
//   acu_mitm_cert_evicted_total — pinned via the LRU-driven log
//     test in cert_cache_evicted_test.go (commit 2d88785) which
//     uses CertCacheSize=1 to force eviction.
//
// Mutates package-level shutdownTimeout, so NOT t.Parallel.
//
// Baseline + delta is the right shape because metrics.Default is
// shared across tests in this package; absolute values may have
// been bumped by a prior test in the run.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
)

func TestServe_MITMConnect_IncrementsAllMetrics(t *testing.T) {
	oldTimeout := shutdownTimeout
	shutdownTimeout = 500 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = oldTimeout })

	cacheDir := t.TempDir()
	cfg := minimalCfg(cacheDir, nil)
	cfg.Upstream.AllowedHostRegex = append(cfg.Upstream.AllowedHostRegex,
		`^metrics-pin\.test$`)
	cfg.TlsMitm.Enabled = true
	cfg.TlsMitm.AllowUnconstrainedCA = true
	cfg.TlsMitm.CertCacheSize = 32
	cfg.TlsMitm.LeafCertLifetime.Duration = time.Hour
	cfg.TlsMitm.CACertLifetime.Duration = 30 * 24 * time.Hour
	cfg.TlsMitm.LeafAlgorithm = "ecdsa-p256"

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	cacheLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cacheAddr := cacheLn.Addr().String()

	// Capture the baseline BEFORE the daemon starts. Other tests in
	// the package share metrics.Default; we assert delta, not
	// absolute, against this snapshot.
	baseline := scrapeMetrics(t)
	connectTotalBefore := sumCounterFamily(baseline, "acu_mitm_connect_total")
	connectCountBefore := readHistogramCount(baseline, "acu_mitm_connect_duration_seconds")
	lookupMissBefore := readCounterValue(baseline,
		`acu_mitm_cert_cache_lookups_total{outcome="miss"}`)
	issuedBefore := readCounterValue(baseline,
		`acu_mitm_cert_issued_total{algorithm="ecdsa-p256"}`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- serveListeners(ctx, cfg, logger, cacheLn, nil, nil)
	}()

	if err := waitForDaemonReady(t, cacheAddr, 10*time.Second); err != nil {
		t.Fatalf("daemon never became ready: %v", err)
	}

	// One CONNECT. The test client never sends ClientHello; the
	// handshake will eventually error (timeout / forced close
	// during shutdown). All the cert-path metrics fire BEFORE the
	// handshake (LeafCache.Get is called pre-handshake), so the
	// cert_issued + cert_cache_lookups deltas hit ≥1 by the time
	// shutdown runs.
	conn := openCONNECT(t, cacheAddr, "metrics-pin.test:443")
	defer conn.Close()

	// Drive shutdown so the connect_total + connect_duration
	// observation runs (those fire when ServeCONNECT exits).
	cancel()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Errorf("serveListeners: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("serveListeners did not return")
	}

	after := scrapeMetrics(t)
	connectTotalAfter := sumCounterFamily(after, "acu_mitm_connect_total")
	connectCountAfter := readHistogramCount(after, "acu_mitm_connect_duration_seconds")
	lookupMissAfter := readCounterValue(after,
		`acu_mitm_cert_cache_lookups_total{outcome="miss"}`)
	issuedAfter := readCounterValue(after,
		`acu_mitm_cert_issued_total{algorithm="ecdsa-p256"}`)

	if connectTotalAfter-connectTotalBefore < 1 {
		t.Errorf("acu_mitm_connect_total{...} delta = %v; want ≥1\nbefore: %v\nafter:  %v",
			connectTotalAfter-connectTotalBefore, connectTotalBefore, connectTotalAfter)
	}
	if connectCountAfter-connectCountBefore < 1 {
		t.Errorf("acu_mitm_connect_duration_seconds_count delta = %v; want ≥1",
			connectCountAfter-connectCountBefore)
	}
	if lookupMissAfter-lookupMissBefore < 1 {
		t.Errorf("acu_mitm_cert_cache_lookups_total{outcome=miss} delta = %v; want ≥1",
			lookupMissAfter-lookupMissBefore)
	}
	if issuedAfter-issuedBefore < 1 {
		t.Errorf("acu_mitm_cert_issued_total{algorithm=ecdsa-p256} delta = %v; want ≥1",
			issuedAfter-issuedBefore)
	}

	// Startup-set gauges. These are absolute, not delta, because
	// SetCertCacheCapacity / SetCANotAfterUnixtime overwrite (not
	// add). They must be non-zero.
	if cap := readGaugeValue(after, "acu_mitm_cert_cache_capacity"); cap < 1 {
		t.Errorf("acu_mitm_cert_cache_capacity = %v; want ≥1 (configured 32)", cap)
	}
	if na := readGaugeValue(after, "acu_mitm_ca_not_after_unixtime"); na < 1 {
		t.Errorf("acu_mitm_ca_not_after_unixtime = %v; want non-zero (CA was generated)", na)
	}
}

// scrapeMetrics renders the current state of metrics.Default. The
// CONNECT handler and leaf cache hooks register into this same
// global, so this is the source of truth for what the daemon
// observed.
func scrapeMetrics(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	metrics.Default.Render(&buf)
	return buf.String()
}

// sumCounterFamily totals the values of every series of a counter
// family — labels included. acu_mitm_connect_total has one series
// per outcome label; the test only cares that the family
// incremented in aggregate.
func sumCounterFamily(out, family string) float64 {
	var total float64
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, family) {
			continue
		}
		// Skip HELP/TYPE comment lines.
		if strings.HasPrefix(line, "# ") {
			continue
		}
		// Skip the bare-name line emitted for label-less counters
		// when this is a labeled family — a labeled family won't
		// have a non-braced exposition line, but be defensive.
		// Format: `<family>{labels} <value>` or `<family> <value>`.
		var val float64
		if i := strings.Index(line, "} "); i >= 0 {
			if _, err := fmt.Sscanf(line[i+2:], "%f", &val); err != nil {
				continue
			}
		} else if rest := strings.TrimPrefix(line, family); rest != line {
			rest = strings.TrimSpace(rest)
			if _, err := fmt.Sscanf(rest, "%f", &val); err != nil {
				continue
			}
		} else {
			continue
		}
		total += val
	}
	return total
}

// readCounterValue returns the value for an exact `<name>{labels}`
// prefix; 0 if not found.
func readCounterValue(out, prefix string) float64 {
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		// Ensure prefix match is exact (the next char after the
		// closing brace should be a space).
		if !strings.HasPrefix(line[len(prefix):], " ") {
			continue
		}
		var val float64
		if _, err := fmt.Sscanf(line[len(prefix)+1:], "%f", &val); err != nil {
			continue
		}
		return val
	}
	return 0
}

// readHistogramCount returns the `_count` series value for a
// histogram family.
func readHistogramCount(out, family string) float64 {
	want := family + "_count"
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, want) {
			continue
		}
		// Format: `<family>_count <value>` (no labels — our
		// histograms in mitm_metrics.go are unlabeled).
		rest := strings.TrimPrefix(line, want)
		rest = strings.TrimSpace(rest)
		var val float64
		if _, err := fmt.Sscanf(rest, "%f", &val); err != nil {
			continue
		}
		return val
	}
	return 0
}

// readGaugeValue returns the gauge value for an unlabeled metric.
func readGaugeValue(out, name string) float64 {
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, name) {
			continue
		}
		// Skip HELP/TYPE.
		if strings.HasPrefix(line, "# ") {
			continue
		}
		rest := strings.TrimPrefix(line, name)
		// Reject labeled series — gauges in this test are unlabeled.
		if strings.HasPrefix(rest, "{") {
			continue
		}
		rest = strings.TrimSpace(rest)
		var val float64
		if _, err := fmt.Sscanf(rest, "%f", &val); err != nil {
			continue
		}
		return val
	}
	return 0
}
