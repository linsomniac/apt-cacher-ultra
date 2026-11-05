package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
	"github.com/linsomniac/apt-cacher-ultra/internal/servertls"
	"github.com/linsomniac/apt-cacher-ultra/internal/testutil/certfixture"
)

// Holding startup at a real log boundary makes port-closed assertions
// deterministic, including the boundary after every constructor has succeeded.
type startupBarrier struct {
	event            string
	entered, release chan struct{}
	once             sync.Once
}

func (h *startupBarrier) Enabled(context.Context, slog.Level) bool { return true }
func (h *startupBarrier) Handle(_ context.Context, r slog.Record) error {
	if r.Message == h.event {
		h.once.Do(func() { close(h.entered); <-h.release })
	}
	return nil
}
func (h *startupBarrier) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *startupBarrier) WithGroup(string) slog.Handler      { return h }

func freeAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}
func assertClosed(t *testing.T, addr string) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
	if err == nil {
		_ = c.Close()
		t.Fatalf("%s accepted TCP before startup completed", addr)
	}
}

func TestServeDelaysBindingUntilPrepared(t *testing.T) {
	for _, event := range []string{"apt-cacher-ultra starting", "startup_preparation_complete"} {
		for _, cancelStartup := range []bool{false, true} {
			t.Run(event+"/cancel="+map[bool]string{false: "false", true: "true"}[cancelStartup], func(t *testing.T) {
				pre := metrics.Default.SnapshotNamesForTest()
				defer metrics.Default.UnregisterAddedSinceForTest(pre)
				cfg := minimalCfg(t.TempDir(), nil)
				cfg.Cache.Listen = freeAddress(t)
				pair := certfixture.New(t)
				cfg.Cache.ListenTLS = freeAddress(t)
				cfg.Cache.TLSCert, cfg.Cache.TLSKey = pair.Write(t, t.TempDir())
				cfg.Admin.Enabled = true
				cfg.Admin.Listen = freeAddress(t)
				barrier := &startupBarrier{event: event, entered: make(chan struct{}), release: make(chan struct{})}
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan error, 1)
				go func() { done <- serve(ctx, cfg, slog.New(barrier)) }()
				var release sync.Once
				unblock := func() { release.Do(func() { close(barrier.release) }) }
				t.Cleanup(func() {
					cancel()
					unblock()
					select {
					case <-done:
					case <-time.After(10 * time.Second):
						t.Error("daemon did not stop")
					}
				})
				select {
				case <-barrier.entered:
				case err := <-done:
					t.Fatalf("startup failed: %v", err)
				case <-time.After(10 * time.Second):
					t.Fatal("startup barrier not reached")
				}
				assertClosed(t, cfg.Cache.Listen)
				assertClosed(t, cfg.Admin.Listen)
				assertClosed(t, cfg.Cache.ListenTLS)
				if cancelStartup {
					cancel()
				}
				unblock()
				if cancelStartup {
					select {
					case err := <-done:
						if !errors.Is(err, context.Canceled) {
							t.Fatalf("cancel error: %v", err)
						}
						done <- err
					case <-time.After(10 * time.Second):
						t.Fatal("canceled startup stuck")
					}
					assertClosed(t, cfg.Cache.Listen)
					assertClosed(t, cfg.Admin.Listen)
					assertClosed(t, cfg.Cache.ListenTLS)
				} else {
					client := &http.Client{Timeout: time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pair.Roots, MinVersion: tls.VersionTLS12}}}
					defer client.CloseIdleConnections()
					for _, addr := range []string{"http://" + cfg.Cache.Listen, "http://" + cfg.Admin.Listen, "https://" + cfg.Cache.ListenTLS} {
						deadline := time.Now().Add(10 * time.Second)
						for {
							resp, err := client.Get(addr + "/healthz")
							if err == nil {
								_ = resp.Body.Close()
								if resp.StatusCode == 200 {
									break
								}
							}
							if time.Now().After(deadline) {
								t.Fatalf("%s never became ready: %v", addr, err)
							}
							time.Sleep(10 * time.Millisecond)
						}
					}
				}
			})
		}
	}
}

func TestServeBindFailureClosesEarlierListeners(t *testing.T) {
	pre := metrics.Default.SnapshotNamesForTest()
	defer metrics.Default.UnregisterAddedSinceForTest(pre)
	cfg := minimalCfg(t.TempDir(), nil)
	cfg.Cache.Listen = freeAddress(t)
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = occupied.Close() }()
	cfg.Admin.Enabled = true
	cfg.Admin.Listen = occupied.Addr().String()
	if err := serve(context.Background(), cfg, newTestLogger()); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("want bind error: %v", err)
	}
	assertClosed(t, cfg.Cache.Listen)
	// Failure releases the lifetime cache lock, too.
	lock, err := lockDaemonCache(cfg.Cache.Dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = lock.Close()
}

func TestDaemonCacheLock(t *testing.T) {
	dir := t.TempDir()
	first, err := lockDaemonCache(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := lockDaemonCache(dir)
	if err == nil {
		_ = second.Close()
		t.Fatal("second daemon acquired same cache")
	}
	_ = first.Close()
	third, err := lockDaemonCache(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = third.Close()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	badDir := t.TempDir()
	if err := os.Symlink(target, filepath.Join(badDir, "daemon.lock")); err != nil {
		t.Fatal(err)
	}
	if f, err := lockDaemonCache(badDir); err == nil {
		_ = f.Close()
		t.Fatal("followed symlink")
	}
}

type readinessCert struct{ state string }

func (c *readinessCert) TLSConfig() *tls.Config            { return nil }
func (c *readinessCert) Snapshot() servertls.Snapshot      { return servertls.Snapshot{State: c.state} }
func (c *readinessCert) Run(context.Context, *slog.Logger) {}

func TestReadinessLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cert := &readinessCert{state: "usable"}
	state := &readiness{ctx: ctx, required: 2, certs: &serverTLS{sources: map[string]certificateSource{"cache": cert}}}
	if got := state.failure(); got != "startup" {
		t.Fatal(got)
	}
	state.accepting.Store(1)
	if got := state.failure(); got != "startup" {
		t.Fatal(got)
	}
	state.accepting.Store(2)
	for _, tc := range []struct{ state, want string }{{"usable", ""}, {"degraded", ""}, {"expired", "proxy_tls"}, {"not_yet_valid", "proxy_tls"}, {"unavailable", "proxy_tls"}, {"usable", ""}} {
		cert.state = tc.state
		if got := state.failure(); got != tc.want {
			t.Fatalf("%s: %s", tc.state, got)
		}
	}
	cancel()
	if got := state.failure(); got != "shutdown" {
		t.Fatal(got)
	}
}

func TestReadinessListenerFailure(t *testing.T) {
	state := &readiness{ctx: context.Background(), required: 1, certs: &serverTLS{}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	_, err = state.listener(ln).Accept()
	if err == nil || state.failure() != "shutdown" {
		t.Fatalf("failed listener reported ready: %v", err)
	}
	// A later listener starting must never reverse a failure.
	state.accepting.Store(1)
	if state.failure() != "shutdown" {
		t.Fatal("readiness resurrected")
	}
}

// A responsive admin listener must not report success while the proxy's Serve
// is still starting, or after its Accept loop fails (before main shuts down).
func TestAdminHealthTracksProxyServing(t *testing.T) {
	for _, event := range []string{"http listener accepting", "listener exited unexpectedly"} {
		t.Run(event, func(t *testing.T) {
			pre := metrics.Default.SnapshotNamesForTest()
			defer metrics.Default.UnregisterAddedSinceForTest(pre)
			cfg := minimalCfg(t.TempDir(), nil)
			cfg.Admin.Enabled = true
			listen := func() net.Listener {
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = ln.Close() })
				return ln
			}
			plain, admin := listen(), listen()
			gate := &startupBarrier{event: event, entered: make(chan struct{}), release: make(chan struct{})}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- serveListeners(ctx, cfg, slog.New(gate), plain, nil, admin, nil) }()
			var once sync.Once
			unblock := func() { once.Do(func() { close(gate.release) }) }
			t.Cleanup(func() {
				cancel()
				unblock()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Error("daemon stuck")
				}
			})
			client := &http.Client{Timeout: time.Second}
			defer client.CloseIdleConnections()
			want := "startup"
			if event == "listener exited unexpectedly" {
				if err := waitForDaemonReady(t, plain.Addr().String(), 10*time.Second); err != nil {
					t.Fatal(err)
				}
				_ = plain.Close()
				want = "shutdown"
			}
			select {
			case <-gate.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("barrier not reached")
			}
			resp, err := client.Get("http://" + admin.Addr().String() + "/healthz")
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != 503 || resp.Header.Get("X-Acu-Check-Failed") != want {
				t.Fatalf("got %d %v", resp.StatusCode, resp.Header)
			}
		})
	}
}

func TestProxyHealthWithAdminDisabled(t *testing.T) {
	cfg := minimalCfg(t.TempDir(), nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveListeners(ctx, cfg, newTestLogger(), ln, nil, nil, nil) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("daemon stuck")
		}
	})
	resp, body := getNoFollow(t, "http://"+ln.Addr().String()+"/healthz")
	if resp.StatusCode != 200 || body != "ok\n" {
		t.Fatalf("health: %d %q", resp.StatusCode, body)
	}
}

func TestReadinessExpiredMITMCA(t *testing.T) {
	state := &readiness{ctx: context.Background(), required: 1, certs: &serverTLS{}, mitm: &tlsMitmHandles{CANotAfterUnixTime: time.Now().Add(-time.Second).Unix()}}
	state.accepting.Store(1)
	if state.failure() != "mitm_ca" {
		t.Fatal("expired MITM CA reported ready")
	}
	state.mitm.CANotAfterUnixTime = time.Now().Add(time.Hour).Unix()
	if state.failure() != "" {
		t.Fatal("valid MITM CA reported unhealthy")
	}
}
