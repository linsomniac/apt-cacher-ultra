package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
	"github.com/linsomniac/apt-cacher-ultra/internal/testutil/fakeacme"
)

func TestManagedTLSBothListenersAndShutdown(t *testing.T) {
	pre := metrics.Default.SnapshotNamesForTest()
	srv := fakeacme.New(t)
	listen := func() net.Listener {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		return ln
	}
	plain, secure, adminLn := listen(), listen(), listen()
	challengeAddr := listen()
	addr := challengeAddr.Addr().String()
	_ = challengeAddr.Close()
	cfg := minimalCfg(t.TempDir(), nil)
	cfg.TlsMitm.Enabled = true
	cfg.TlsMitm.AllowUnconstrainedCA = true
	cfg.TlsMitm.LeafAlgorithm = "ecdsa-p256"
	cfg.TlsMitm.CertCacheSize = 16
	cfg.TlsMitm.LeafCertLifetime.Duration = time.Hour
	cfg.TlsMitm.CACertLifetime.Duration = 30 * 24 * time.Hour
	cfg.TlsMitm.AllowedHostRegex = `^cache\.example\.net$`
	cfg.Cache.Listen = plain.Addr().String()
	cfg.Cache.ListenTLS = secure.Addr().String()
	cfg.Cache.UseACME = true
	cfg.Admin = config.AdminConfig{Enabled: true, Listen: adminLn.Addr().String(), UseACME: true, GaugeRefresh: config.Duration{Duration: time.Second}, ReadTimeout: config.Duration{Duration: 5 * time.Second}, IdleTimeout: config.Duration{Duration: 30 * time.Second}, MetricSeriesCap: 1024}
	cfg.ACME = config.ACMEConfig{Enabled: true, DirectoryURL: srv.DirectoryURL(), DNSName: "cache.example.net", Listen: addr, AcceptTerms: true}
	srv.SetChallengeBaseURL("http://" + addr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime, err := prepareServerTLS(ctx, cfg, slog.New(slog.DiscardHandler), srv.Transport())
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	old := runtime.manager.Current().Leaf
	// Make the committed certificate due before the daemon starts its renewal worker.
	paths, err := filepath.Glob(filepath.Join(cfg.Cache.Dir, "acme", "*", "*.certificate.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("bundle paths %v %v", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	record["renew_at"], _ = json.Marshal(time.Now().Add(-time.Second))
	data, _ = json.Marshal(record)
	if err := os.WriteFile(paths[0], data, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- serveListeners(ctx, cfg, slog.New(slog.DiscardHandler), plain, secure, adminLn, nil, runtime)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("shutdown: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("daemon failed to drain")
		}
		metrics.Default.UnregisterAddedSinceForTest(pre)
	})
	if err := waitForDaemonReady(t, plain.Addr().String(), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.manager.Current().Leaf.Equal(old) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if runtime.manager.Current().Leaf.Equal(old) {
		t.Fatal("renewal not published")
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(srv.CACertPEM())
	for _, ln := range []net.Listener{secure, adminLn} {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", ln.Addr().String(), &tls.Config{RootCAs: roots, ServerName: cfg.ACME.DNSName, MinVersion: tls.VersionTLS12})
		if err != nil {
			t.Fatal(err)
		}
		got := conn.ConnectionState().PeerCertificates[0]
		_ = conn.Close()
		if got.Equal(old) || !got.Equal(runtime.manager.Current().Leaf) {
			t.Fatal("listener did not adopt renewal")
		}
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: cfg.ACME.DNSName, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	resp, err := client.Get("https://" + adminLn.Addr().String() + "/?format=json")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("status %d %s %v", resp.StatusCode, body, err)
	}
	var status struct {
		ServerTLS map[string]struct {
			State string `json:"state"`
		} `json:"server_tls"`
	}
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatal(err)
	}
	if len(status.ServerTLS) != 2 || status.ServerTLS["admin"].State != "usable" {
		t.Fatalf("status %s", body)
	}
}

func TestChallengePortConflictBeforeAccountCreation(t *testing.T) {
	srv := fakeacme.New(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	cfg := minimalCfg(t.TempDir(), nil)
	cfg.Admin.Enabled = true
	cfg.Admin.UseACME = true
	cfg.ACME = config.ACMEConfig{Enabled: true, DirectoryURL: srv.DirectoryURL(), DNSName: "cache.example.net", Listen: ln.Addr().String(), AcceptTerms: true}
	runtime, err := prepareServerTLS(context.Background(), cfg, slog.New(slog.DiscardHandler), srv.Transport())
	if err == nil {
		runtime.Close()
		t.Fatal("occupied challenge port accepted")
	}
	if srv.AccountCount() != 0 || srv.IssueCount() != 0 {
		t.Fatal("contacted CA before reserving listeners")
	}
}
