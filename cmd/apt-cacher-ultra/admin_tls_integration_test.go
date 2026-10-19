package main

// Pins the admin listener TLS wiring through the production path:
// config.Admin.TLSCert / TLSKey flow into admin.New via main.go's
// serveListeners, and admin.Server.Serve takes the ServeTLS branch —
// so the daemon's admin port speaks HTTPS only. Also pins the
// operator-visible surface: the status page advertises the admin
// listener with an https:// scheme, and a plain-HTTP probe never
// gets a 2xx (Go's http.Server answers plain HTTP on a TLS listener
// with a cleartext 400, so the rejection assertion accepts either a
// transport error or a >=400 status).
//
// Mutates package-level shutdownTimeout, so NOT t.Parallel.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
	"github.com/linsomniac/apt-cacher-ultra/internal/testutil/tlscert"
)

func TestServe_AdminTLS(t *testing.T) {
	oldTimeout := shutdownTimeout
	shutdownTimeout = 500 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = oldTimeout })

	// admin.New registers gauges into metrics.Default unconditionally
	// at startup. Snapshot names BEFORE the daemon brings up the admin
	// server; the t.Cleanup below cancels the daemon AND THEN
	// unregisters added-since names so the gauge refresher has
	// finished writing before we drop them — same ordering pattern
	// as disabled_mode_parity_daemon_test.go:t.Cleanup.
	preMetrics := metrics.Default.SnapshotNamesForTest()

	cacheDir := t.TempDir()
	cfg := minimalCfg(cacheDir, nil)
	// minimalCfg + Defaults() leaves bools alone, so TlsMitm.Enabled
	// is already false; set it loudly to pin the no-MITM wiring path.
	cfg.TlsMitm.Enabled = false

	certPath, keyPath := testutil.WriteSelfSignedCert(t, t.TempDir())
	// Admin block — set the presence-sensitive defaults that Load()
	// applies via TOML's md.IsDefined but Defaults() does not (same
	// rationale as disabled_mode_parity_daemon_test.go).
	cfg.Admin = config.AdminConfig{
		Enabled:         true,
		TLSCert:         certPath,
		TLSKey:          keyPath,
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
		serveDone <- serveListeners(ctx, cfg, logger, cacheLn, nil, adminLn, nil, nil)
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

	// HTTPS client trusting the self-signed cert: /healthz → 200 ok.
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: testutil.CertPoolFromFile(t, certPath)},
		},
	}
	resp, err := client.Get("https://" + adminAddr + "/healthz")
	if err != nil {
		t.Fatalf("GET https admin /healthz: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read /healthz body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("https /healthz status = %d, want %d (body %q)", resp.StatusCode, http.StatusOK, body)
	}
	if string(body) != "ok\n" {
		t.Errorf("https /healthz body = %q, want %q", body, "ok\n")
	}

	// Plain HTTP against the TLS admin listener must not succeed.
	plainClient := &http.Client{Timeout: 5 * time.Second}
	plainResp, err := plainClient.Get("http://" + adminAddr + "/healthz")
	if err == nil {
		_ = plainResp.Body.Close()
		if plainResp.StatusCode < 400 {
			t.Errorf("plain HTTP against TLS admin listener: status = %d, want transport error or >= 400", plainResp.StatusCode)
		}
	}

	// The status page advertises the admin listener with an https://
	// scheme so operators copy-paste a URL that actually works.
	statusResp, err := client.Get("https://" + adminAddr + "/?format=json")
	if err != nil {
		t.Fatalf("GET https admin /?format=json: %v", err)
	}
	statusBody, err := io.ReadAll(statusResp.Body)
	_ = statusResp.Body.Close()
	if err != nil {
		t.Fatalf("read status JSON body: %v", err)
	}
	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("status JSON status=%d body=%s", statusResp.StatusCode, statusBody)
	}
	var statusPayload struct {
		Listeners []struct {
			Role string `json:"role"`
			Addr string `json:"addr"`
		} `json:"listeners"`
	}
	if err := json.Unmarshal(statusBody, &statusPayload); err != nil {
		t.Fatalf("decode status JSON: %v\nbody:\n%s", err, statusBody)
	}
	adminAdvertised := false
	for _, l := range statusPayload.Listeners {
		if l.Role == "admin" {
			adminAdvertised = true
			if want := "https://" + adminAddr; l.Addr != want {
				t.Errorf("status page admin listener addr = %q, want %q", l.Addr, want)
			}
		}
	}
	if !adminAdvertised {
		t.Errorf("status page listeners missing admin role; body:\n%s", statusBody)
	}
}
