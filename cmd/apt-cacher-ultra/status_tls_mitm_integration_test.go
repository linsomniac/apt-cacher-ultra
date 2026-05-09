package main

// SPEC6 §15 #12 DoD pin — production-side §10.4 wiring.
//
// internal/admin/admin_test.go pins the status-page rendering
// against a stubTLSMITMProvider — that proves the templates
// correctly format whatever the provider returns. This test
// pins the OTHER half: that the daemon's actual tlsMitmProvider
// (in main.go) returns live data from the running TLS MITM
// stack — CA fingerprint, CertCacheSize, LastIssued, etc.
//
// Drives the daemon with the admin server enabled, fires one
// CONNECT (which issues a cert and bumps LastCertIssued), then
// GETs /?format=json from the admin listener and asserts the
// `tls_mitm` payload reflects the cert that was just issued.
//
// Catches a class of regression that the stub-provider tests
// can't: someone removing `TLSMITM:` from admin.New args, or
// renaming a tlsMitmHandles accessor, or breaking
// proxy.NoteCertIssued / proxy.LastCertIssued plumbing.
//
// Mutates package-level shutdownTimeout, so NOT t.Parallel.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
)

func TestServe_StatusPage_TLSMITMSection_ReflectsLiveCertIssuance(t *testing.T) {
	oldTimeout := shutdownTimeout
	shutdownTimeout = 500 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = oldTimeout })

	// admin.New registers gauges into metrics.Default unconditionally.
	// Snapshot names before the daemon brings up the admin server; the
	// shutdown cleanup below cancels the daemon AND THEN unregisters,
	// so any refresher goroutine has finished writing to those gauges
	// before we drop them from the registry.
	preMetrics := metrics.Default.SnapshotNamesForTest()

	cacheDir := t.TempDir()
	cfg := minimalCfg(cacheDir, nil)
	cfg.Upstream.AllowedHostRegex = append(cfg.Upstream.AllowedHostRegex,
		`^status-pin\.test$`)
	cfg.TlsMitm.Enabled = true
	cfg.TlsMitm.AllowUnconstrainedCA = true
	cfg.TlsMitm.AllowedHostRegex = `^status-pin\.test$`
	cfg.TlsMitm.CertCacheSize = 16
	cfg.TlsMitm.LeafCertLifetime.Duration = time.Hour
	cfg.TlsMitm.CACertLifetime.Duration = 30 * 24 * time.Hour
	cfg.TlsMitm.LeafAlgorithm = "ecdsa-p256"
	// minimalCfg + Defaults() does NOT populate the §5.2 admin-block
	// presence-sensitive defaults (those are applied in Load() via
	// TOML's md.IsDefined). Set them here so the admin server can
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

	ctx, cancel := context.WithCancel(context.Background())

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- serveListeners(ctx, cfg, logger, cacheLn, nil, adminLn, nil)
	}()
	// Single shutdown cleanup that runs before the metric unregister
	// (t.Cleanup is LIFO). cancel() → wait serveDone → unregister.
	// Any t.Fatalf path before the explicit shutdown below still
	// follows this same ordering, which keeps the gauge refresher
	// from racing the registry unwind.
	t.Cleanup(func() {
		cancel()
		select {
		case <-serveDone:
		case <-time.After(15 * time.Second):
			t.Errorf("serveListeners did not return on cleanup")
		}
		metrics.Default.UnregisterAddedSinceForTest(preMetrics)
	})

	if err := waitForDaemonReady(t, cacheAddr, 10*time.Second); err != nil {
		t.Fatalf("daemon never became ready: %v", err)
	}

	// Issue exactly one cert by driving one CONNECT.
	conn := openCONNECT(t, cacheAddr, "status-pin.test:443")
	defer conn.Close()

	// Capture roughly when the cert was issued so we can sanity-
	// check the LastIssued timestamp later.
	capturedAt := time.Now()

	// GET /?format=json from the admin listener while the daemon
	// is still up — the snapshot must reflect live state. Bounded
	// timeout so a stalled admin handler fails the test instead of
	// hanging until the global -timeout deadline.
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("http://" + adminAddr + "/?format=json")
	if err != nil {
		t.Fatalf("GET admin /?format=json: %v", err)
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read admin response body: %v", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin /?format=json status=%d body=%s", resp.StatusCode, body)
	}

	var payload struct {
		TLSMITM struct {
			Enabled             bool   `json:"enabled"`
			CASource            string `json:"ca_source"`
			CAFingerprintSHA256 string `json:"ca_fingerprint_sha256"`
			CANotAfterUnixTime  int64  `json:"ca_not_after_unixtime"`
			EffectiveAllowlist  string `json:"effective_allowlist"`
			CertCache           struct {
				Size     int `json:"size"`
				Capacity int `json:"capacity"`
			} `json:"cert_cache"`
			LastIssued *struct {
				Host       string `json:"host"`
				AtUnixTime int64  `json:"at_unixtime"`
			} `json:"last_cert_issued"`
		} `json:"tls_mitm"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode admin JSON: %v\nbody:\n%s", err, body)
	}

	tm := payload.TLSMITM
	if !tm.Enabled {
		t.Fatalf("tls_mitm.enabled = false; want true\nbody:\n%s", body)
	}
	if tm.CASource != "generated" {
		t.Errorf("tls_mitm.ca_source = %q, want %q", tm.CASource, "generated")
	}
	// SPEC6 §10.4: CA SHA-256 fingerprint is hex-encoded — 64 chars.
	if len(tm.CAFingerprintSHA256) != 64 {
		t.Errorf("tls_mitm.ca_fingerprint_sha256 length = %d, want 64; got %q",
			len(tm.CAFingerprintSHA256), tm.CAFingerprintSHA256)
	}
	for _, c := range tm.CAFingerprintSHA256 {
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			t.Errorf("tls_mitm.ca_fingerprint_sha256 has non-hex char %q in %q", c, tm.CAFingerprintSHA256)
			break
		}
	}
	if tm.CANotAfterUnixTime <= time.Now().Unix() {
		t.Errorf("tls_mitm.ca_not_after_unixtime = %d, want > now (CA was just generated with 30d lifetime)",
			tm.CANotAfterUnixTime)
	}
	if tm.EffectiveAllowlist != `^status-pin\.test$` {
		t.Errorf("tls_mitm.effective_allowlist = %q, want %q",
			tm.EffectiveAllowlist, `^status-pin\.test$`)
	}
	if tm.CertCache.Capacity != 16 {
		t.Errorf("tls_mitm.cert_cache.capacity = %d, want 16", tm.CertCache.Capacity)
	}
	if tm.CertCache.Size != 1 {
		t.Errorf("tls_mitm.cert_cache.size = %d, want 1 (one CONNECT issued one cert)", tm.CertCache.Size)
	}
	if tm.LastIssued == nil {
		t.Fatalf("tls_mitm.last_cert_issued = null, want populated\nbody:\n%s", body)
	}
	if tm.LastIssued.Host != "status-pin.test" {
		t.Errorf("tls_mitm.last_cert_issued.host = %q, want %q",
			tm.LastIssued.Host, "status-pin.test")
	}
	// Cert was issued during the GET-then-test window. Allow ±5s
	// slack — clock granularity, scheduler jitter, etc.
	skew := tm.LastIssued.AtUnixTime - capturedAt.Unix()
	if skew < -5 || skew > 5 {
		t.Errorf("tls_mitm.last_cert_issued.at_unixtime = %d, captured ~ %d (skew=%ds, want ±5s)",
			tm.LastIssued.AtUnixTime, capturedAt.Unix(), skew)
	}

	// Spot-check no spurious extra top-level tls_mitm fields beyond
	// the documented set + hit_rate fields. (Strict §10.4 field set
	// audit already lives in admin_test.go; here we just guard
	// against a regression where production smuggles a debug field
	// into the public payload.)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	var tmRaw map[string]json.RawMessage
	if err := json.Unmarshal(raw["tls_mitm"], &tmRaw); err != nil {
		t.Fatalf("decode tls_mitm raw: %v", err)
	}
	allowedKeys := map[string]bool{
		"enabled": true, "ca_source": true, "ca_fingerprint_sha256": true,
		"ca_not_after_unixtime": true, "effective_allowlist": true,
		"cert_cache": true, "last_cert_issued": true,
		"cert_hit_rate_60s_percent": true, "cert_hit_rate_60s_observed": true,
	}
	for k := range tmRaw {
		if !allowedKeys[k] {
			t.Errorf("tls_mitm carries unexpected key %q (production-side smuggled a field?)", k)
		}
	}
	// Quick sanity: the body actually includes the section.
	if !strings.Contains(string(body), `"ca_fingerprint_sha256"`) {
		t.Errorf("body missing tls_mitm.ca_fingerprint_sha256 key; raw body:\n%s", body)
	}
}
