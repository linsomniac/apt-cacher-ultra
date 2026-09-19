package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/config"
)

// Exercise the real TOML-to-fetcher wiring. The origin's reserved .invalid
// hostname cannot be fetched directly; only the configured proxy can supply it.
func TestServe_UpstreamProxy_ConfigFetchCacheAndSafeStartupLog(t *testing.T) {
	const (
		username       = "upstream-proxy-test-user"
		password       = "upstream-proxy-test-password"
		body           = "cached through the configured upstream proxy\n"
		origin         = "http://repository.invalid/pool/main/p/package_1_all.deb"
		rejectedOrigin = "http://repository.invalid/pool/main/p/package_2_all.deb"
	)
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
	var proxyHits atomic.Int32
	upstreamProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		if r.Method != http.MethodGet || (r.RequestURI != origin && r.RequestURI != rejectedOrigin) || r.URL.String() != r.RequestURI {
			t.Errorf("unexpected proxy request = %s %q (URL %q)", r.Method, r.RequestURI, r.URL.String())
			http.Error(w, "unexpected proxy target", http.StatusBadRequest)
			return
		}
		if r.Header.Get("Proxy-Authorization") != wantAuth {
			t.Error("upstream proxy did not receive configured Basic credentials")
			http.Error(w, "proxy credentials required", http.StatusProxyAuthRequired)
			return
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("proxy credentials also appeared as origin Authorization")
		}
		if r.RequestURI == rejectedOrigin {
			w.Header().Set("Proxy-Authenticate", `Basic realm="upstream-only"`)
			http.Error(w, "upstream-only authentication challenge", http.StatusProxyAuthRequired)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(upstreamProxy.Close)
	proxyURL, err := url.Parse(upstreamProxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxyURL.User = url.UserPassword(username, password)

	cacheDir := t.TempDir()
	configPath := filepath.Join(cacheDir, "config.toml")
	configText := fmt.Sprintf(`[cache]
dir = %q
[upstream]
proxy = %q
allowed_host_regex = ['^repository\.invalid$']
connect_timeout = "1s"
total_timeout = "5s"
[tls_mitm]
enabled = false
[admin]
enabled = false
[[mirror]]
prefix = "/repository"
upstream = "http://repository.invalid/"
`, cacheDir, proxyURL.String())
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cacheLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var logs captureBuilder
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- serveListeners(ctx, cfg, logger, cacheLn, nil, nil, nil)
	}()
	var once sync.Once
	shutdown := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-serveDone:
				if err != nil {
					t.Errorf("serveListeners: %v", err)
				}
			case <-time.After(15 * time.Second):
				t.Error("serveListeners did not stop")
			}
		})
	}
	t.Cleanup(shutdown)

	cacheURL := "http://" + cacheLn.Addr().String() + "/repository/pool/main/p/package_1_all.deb"
	for _, wantCache := range []string{"MISS", "HIT"} {
		resp, gotBody := getNoFollow(t, cacheURL)
		if resp.StatusCode != http.StatusOK || gotBody != body {
			t.Fatalf("GET: status = %d, body = %q; want 200, %q", resp.StatusCode, gotBody, body)
		}
		if got := resp.Header.Get("X-Cache"); got != wantCache {
			t.Errorf("X-Cache = %q, want %q", got, wantCache)
		}
	}
	if got := proxyHits.Load(); got != 1 {
		t.Errorf("upstream proxy requests = %d, want one cache miss", got)
	}
	resp, rejectedBody := getNoFollow(t, strings.Replace(cacheURL, "package_1_all.deb", "package_2_all.deb", 1))
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("upstream 407 produced cache status = %d, want 502", resp.StatusCode)
	}
	if resp.Header.Get("Proxy-Authenticate") != "" || strings.Contains(rejectedBody, "upstream-only") {
		t.Error("cache forwarded upstream proxy authentication challenge to client")
	}
	if got := proxyHits.Load(); got != 2 {
		t.Errorf("upstream proxy requests = %d, want one success and one non-retried 407", got)
	}
	shutdown()

	logText := logs.String()
	for _, secret := range []string{username, password, proxyURL.String(), wantAuth} {
		if strings.Contains(logText, secret) {
			t.Error("daemon log disclosed proxy URL or credentials")
		}
	}
	var foundStartup bool
	for _, line := range strings.Split(strings.TrimSpace(logText), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode daemon log: %v", err)
		}
		if record["msg"] == "apt-cacher-ultra starting" {
			foundStartup = true
			if record["upstream_proxy_enabled"] != true {
				t.Errorf("startup upstream_proxy_enabled = %v, want true", record["upstream_proxy_enabled"])
			}
		}
	}
	if !foundStartup {
		t.Error("daemon startup configuration log missing")
	}
}
