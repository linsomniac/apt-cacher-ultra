package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
	"github.com/linsomniac/apt-cacher-ultra/internal/usage"
)

func TestUsageDaemonShutdownRestartAndAdmin(t *testing.T) {
	baseline := metrics.Default.SnapshotNamesForTest()
	defer metrics.Default.UnregisterAddedSinceForTest(baseline)
	const body = "package bytes"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
	defer upstream.Close()
	cfg := minimalCfg(t.TempDir(), []config.MirrorRule{{Prefix: "/repo", Upstream: upstream.URL + "/"}})
	cfg.Cache.DBDir = t.TempDir()
	cfg.Admin.Enabled = false
	cfg.TlsMitm.Enabled = false
	for run := 0; run < 2; run++ {
		var adminLn net.Listener
		if run == 1 {
			cfg.Admin.Enabled = true
			var err error
			adminLn, err = net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := serveUntilCleanup(t, ctx, cancel, func(ctx context.Context) error {
			return serveListeners(ctx, cfg, newTestLogger(), ln, nil, adminLn, nil)
		})
		waitUsageReady(t, ln.Addr().String())
		for range 2 {
			resp, got := getNoFollow(t, "http://"+ln.Addr().String()+"/repo/pool/test.deb")
			if resp.StatusCode != 200 || got != body {
				t.Fatalf("%d %q", resp.StatusCode, got)
			}
		}
		if adminLn != nil {
			resp, body := getNoFollow(t, "http://"+adminLn.Addr().String()+"/?format=json")
			var payload struct {
				Usage usage.Snapshot `json:"usage"`
			}
			if err := json.Unmarshal([]byte(body), &payload); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != 200 || payload.Usage.Lifetime.Requests != 4 || payload.Usage.Session.Requests != 2 {
				t.Fatalf("admin usage: %d, %+v", resp.StatusCode, payload)
			}
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("shutdown timeout")
		}
		c, err := cache.OpenWithDBDir(t.Context(), cfg.Cache.Dir, cfg.Cache.DBDir, nil)
		if err != nil {
			t.Fatal(err)
		}
		totals, err := c.LoadUsage(t.Context())
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		if err != nil {
			t.Fatal(err)
		}
		if totals.FetchedBytes != int64(len(body)) || totals.ServedBytes != int64(2*(run+1)*len(body)) || totals.Requests != int64(2*(run+1)) || totals.CacheHitRequests != int64(2*run+1) {
			t.Fatalf("run %d: %+v", run, totals)
		}
	}
}

// The older waitForDaemonReady helper sends GET / and accepts a 4xx; those
// requests correctly contribute to usage. Probe the actual readiness route
// here so the test also verifies that /healthz is excluded from accounting.
func waitUsageReady(t *testing.T, addr string) {
	t.Helper()
	client := &http.Client{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + addr + "/healthz")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("daemon readiness timeout")
}
