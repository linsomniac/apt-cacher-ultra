package acme

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/testutil/certfixture"
)

// TestPebbleInteroperability runs only with explicitly installed test binaries.
// CI pins both binaries to github.com/letsencrypt/pebble/v2 v2.10.1. No public
// authority is contacted; Pebble validates our real HTTP-01 handler locally.
func TestPebbleInteroperability(t *testing.T) {
	bin := os.Getenv("ACU_PEBBLE_BIN_DIR")
	if bin == "" {
		t.Skip("set ACU_PEBBLE_BIN_DIR to run independent ACME interoperability test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	address := func() string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		return addr
	}
	apiAddr, managementAddr, dnsAddr, dnsManagement := address(), address(), address(), address()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ch := StartChallenge(ln, testName)
	defer ch.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	httpPort, _ := strconv.Atoi(port)
	dir := t.TempDir()
	pair := certfixture.New(t)
	cert, key := pair.Write(t, dir)
	settings := map[string]any{"pebble": map[string]any{"listenAddress": apiAddr, "managementListenAddress": managementAddr, "certificate": cert, "privateKey": key, "httpPort": httpPort, "tlsPort": 0, "profiles": map[string]any{"default": map[string]any{"description": "48 hour test certificate", "validityPeriod": 172800}}, "retryAfter": map[string]int{"authz": 1, "order": 1}}}
	data, _ := json.Marshal(settings)
	cfgPath := filepath.Join(dir, "pebble.json")
	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	start := func(name string, args ...string) {
		log, err := os.Create(filepath.Join(dir, name+".log"))
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(ctx, filepath.Join(bin, name), args...)
		cmd.Env = append(os.Environ(), "PEBBLE_VA_NOSLEEP=1", "PEBBLE_WFE_NONCEREJECT=0", "PEBBLE_AUTHZREUSE=0")
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			_ = log.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			_ = log.Close()
			if t.Failed() {
				data, _ := os.ReadFile(log.Name())
				t.Logf("%s: %s", name, data)
			}
		})
	}
	start("pebble-challtestsrv", "-dnsserver", dnsAddr, "-defaultIPv6", "", "-management", dnsManagement, "-doh", "", "-http01", "", "-tlsalpn01", "", "-https01", "")
	start("pebble", "-config", cfgPath, "-dnsserver", dnsAddr, "-strict")
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pair.Roots, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	directory := "https://" + apiAddr + "/dir"
	for {
		resp, err := client.Get(directory)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("Pebble did not become ready")
		case <-time.After(50 * time.Millisecond):
		}
	}
	cfg := Config{DirectoryURL: directory, DNSName: testName, StateDir: filepath.Join(dir, "state"), AcceptTerms: true, Challenge: ch, Transport: transport}
	m, err := NewManager(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.EnsureCertificate(ctx); err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get("https://" + managementAddr + "/roots/0")
	if err != nil {
		t.Fatal(err)
	}
	pem, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		t.Fatal("invalid Pebble issuing root")
	}
	intermediates := x509.NewCertPool()
	for _, der := range m.Current().Certificate[1:] {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		intermediates.AddCert(c)
	}
	if _, err := m.Current().Leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, DNSName: testName}); err != nil {
		t.Fatal(err)
	}
	first := m.Current().Leaf
	var rec certificateRecord
	if err := m.store.read(m.store.certName, &rec); err != nil {
		t.Fatal(err)
	}
	rec.RenewAt = time.Now().Add(-time.Second)
	if err := m.store.write(m.store.certName, rec); err != nil {
		t.Fatal(err)
	}
	if err := m.ensure(ctx, true); err != nil {
		t.Fatal(err)
	}
	if first.Equal(m.Current().Leaf) {
		t.Fatal("renewal did not change certificate")
	}
	cfg.Transport = roundTrip(func(*http.Request) (*http.Response, error) {
		t.Error("restart contacted authority")
		return nil, errors.New("offline")
	})
	recovered, err := NewManager(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if err := recovered.EnsureCertificate(ctx); err != nil {
		t.Fatal(err)
	}
	if !recovered.Current().Leaf.Equal(m.Current().Leaf) {
		t.Fatal("restart lost renewed certificate")
	}
}
