package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
)

// Spec C11: freshness.max_suites reaches the checker (the scheduler's
// queue bound, spec C4).
func TestCheckerConfig_CarriesMaxSuites(t *testing.T) {
	cfg := minimalCfg(t.TempDir(), nil)
	cfg.Freshness.MaxSuites = 1234
	fc := checkerConfig(context.Background(), cfg, nil, nil, nil, silentBuildLogger(), nil, nil)
	if fc.MaxSuites != 1234 {
		t.Fatalf("MaxSuites = %d, want freshness.max_suites (1234)", fc.MaxSuites)
	}
	if fc.Cooldown != cfg.Freshness.Cooldown.Duration || fc.Refresh != cfg.Freshness.PeriodicRefresh.Duration {
		t.Fatalf("cooldown/refresh = %v/%v, want the config's", fc.Cooldown, fc.Refresh)
	}
}

// probeClient bounds each startup probe, so a hung request ends inside
// waitServing's 15 s deadline (deferred minor D-49).
var probeClient = &http.Client{Timeout: 2 * time.Second}

// waitServing polls the daemon on addr until it answers a request. After
// 15 s it cancels the daemon and waits (another 15 s at most) for
// serveListeners to return on done, so nothing outlives the test and writes
// to its logs (deferred minor D-46), then fails t. The start-and-probe loop
// of every test that runs the daemon to its listener (deferred minor D-49).
func waitServing(t *testing.T, addr string, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := probeClient.Get("http://" + addr + "/healthz-probe")
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		if time.Now().After(deadline) {
			cancel()
			select {
			case <-done:
			case <-time.After(15 * time.Second):
			}
			t.Fatalf("daemon not serving within 15s: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startupLogs runs the daemon on a config of cacheDir plus extra TOML until
// it serves, shuts it down, and returns its log lines, decoded.
func startupLogs(t *testing.T, cacheDir, extra string) []map[string]any {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	text := "[cache]\ndir = \"" + cacheDir + "\"\n" + extra + "[tls_mitm]\nenabled = false\n[admin]\nenabled = false\n"
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var logs captureBuilder
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveListeners(ctx, cfg, slog.New(slog.NewJSONHandler(&logs, nil)), ln, nil, nil, nil) }()
	// Wait until the daemon serves (the startup line and the startup
	// checks precede it), then shut it down.
	waitServing(t, ln.Addr().String(), cancel, done)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serveListeners: %v", err)
	}
	var recs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		recs = append(recs, rec)
	}
	return recs
}

// The startup line reports freshness.max_suites, and (deferred minor C-35)
// adoption.flip_quiet_period, with their configured values; an empty
// suite table reaches no cap.
func TestServeListeners_StartupLineReportsTheSuiteCap(t *testing.T) {
	recs := startupLogs(t, t.TempDir(), "[freshness]\nmax_suites = 1234\n[adoption]\nflip_quiet_period = \"5s\"\n")
	for _, rec := range recs {
		if rec["msg"] == "suite_caps_exceeded" {
			t.Errorf("suite_caps_exceeded on an empty cache: %v", rec)
		}
	}
	for _, rec := range recs {
		if rec["msg"] != "apt-cacher-ultra starting" {
			continue
		}
		if rec["freshness_max_suites"] != float64(1234) {
			t.Errorf("freshness_max_suites = %v, want 1234", rec["freshness_max_suites"])
		}
		if rec["adoption_flip_quiet_period"] != float64(5*time.Second) {
			t.Errorf("adoption_flip_quiet_period = %v, want 5s in nanoseconds", rec["adoption_flip_quiet_period"])
		}
		return
	}
	t.Fatal("startup line not found")
}

// Rulings PD-8, PD-12: a suite table that already refuses new suites is
// reported at startup — here two suites of one host against max_suites =
// 10, whose per-host cap is one.
func TestServeListeners_WarnsWhenTheSuiteCapsAreReached(t *testing.T) {
	cacheDir := t.TempDir()
	ctx := context.Background()
	c, err := cache.Open(ctx, cacheDir, newTestLogger()) // no migration noise (final review, tests Minor 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, suite := range []string{"/a/dists/x", "/b/dists/x"} {
		if err := c.EnsureSuiteForTest(ctx, "http", "h.example", suite); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	for _, rec := range startupLogs(t, cacheDir, "[freshness]\nmax_suites = 10\n") {
		if rec["msg"] != "suite_caps_exceeded" {
			continue
		}
		if rec["level"] != "WARN" || rec["suites"] != float64(2) || rec["max_suites"] != float64(10) ||
			rec["hosts_at_cap"] != float64(1) || rec["per_host_cap"] != float64(1) ||
			rec["fullest_host"] != "h.example" || rec["fullest_host_suites"] != float64(2) {
			t.Fatalf("suite_caps_exceeded = %v; want 2 suites of max 10, one host at its cap of 1 (h.example, 2)", rec)
		}
		return
	}
	t.Fatal("suite_caps_exceeded not logged")
}

// Ruling PD-12: suite_caps_exceeded fires when new suites are refused — the
// table above the global cap, or a host at or above its per-host cap. A
// table at exactly the global cap is normal operation (a new suite evicts
// an idle one, and is refused only when none qualifies) and logs nothing.
func TestWarnSuiteCaps_AboveTheGlobalCapOrAtAHostCap(t *testing.T) {
	const maxSuites = 20 // per-host cap 2

	// spread returns n suites' hosts, each suite on its own host.
	spread := func(n int) []string {
		var hosts []string
		for i := range n {
			hosts = append(hosts, fmt.Sprintf("h%d.example", i))
		}
		return hosts
	}
	for _, tc := range []struct {
		name  string
		hosts []string // one suite per entry
		warn  bool
	}{
		{"exactly at the global cap", spread(maxSuites), false},
		{"above the global cap", spread(maxSuites + 1), true},
		{"a host at its cap", []string{"h.example", "h.example"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			c, err := cache.Open(ctx, t.TempDir(), silentBuildLogger())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Close() })
			for i, h := range tc.hosts {
				if err := c.EnsureSuiteForTest(ctx, "http", h, fmt.Sprintf("/s%d/dists/x", i)); err != nil {
					t.Fatal(err)
				}
			}
			var logs captureBuilder
			warnSuiteCaps(ctx, c, maxSuites, slog.New(slog.NewJSONHandler(&logs, nil)))
			if got := strings.Contains(logs.String(), `"msg":"suite_caps_exceeded"`); got != tc.warn {
				t.Fatalf("suite_caps_exceeded logged = %v, want %v (log %q)", got, tc.warn, logs.String())
			}
		})
	}
}

// Spec C3: the handler gets freshness.max_suites and the checker as its
// cap-eviction guard; with no checker the guard is a nil interface, never
// a typed nil.
func TestHandlerConfig_CarriesTheSuiteCapsAndGuard(t *testing.T) {
	cfg := minimalCfg(t.TempDir(), nil)
	cfg.Freshness.MaxSuites = 1234
	c, fetcher, hosts := newAdoptionWiringDeps(t)
	ck, err := freshness.New(checkerConfig(context.Background(), cfg, c, fetcher, hosts, silentBuildLogger(), nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	hc := handlerConfig(cfg, nil, c, fetcher, hosts, silentBuildLogger(), ck, nil)
	if hc.MaxSuites != 1234 {
		t.Fatalf("MaxSuites = %d, want freshness.max_suites (1234)", hc.MaxSuites)
	}
	if g, ok := hc.SuiteGuard.(*freshness.Checker); !ok || g != ck {
		t.Fatalf("SuiteGuard = %T %v, want the checker", hc.SuiteGuard, hc.SuiteGuard)
	}
	if hc := handlerConfig(cfg, nil, c, fetcher, hosts, silentBuildLogger(), nil, nil); hc.SuiteGuard != nil {
		t.Fatalf("no checker, yet SuiteGuard = %T (a typed nil would be called)", hc.SuiteGuard)
	}
}
