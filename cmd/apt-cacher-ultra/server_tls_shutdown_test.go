package main

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
)

func TestShutdownStopsListenersBeforeDNSCleanupFinishes(t *testing.T) {
	pre := metrics.Default.SnapshotNamesForTest()
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
	cfg.Cache.Listen, cfg.Admin.Listen = plain.Addr().String(), admin.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	certs, err := prepareServerTLS(ctx, cfg, silentBuildLogger(), nil)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	releaseCleanup := func() { once.Do(func() { close(release) }) }
	// Model the tracked certificate worker performing slow cleanup after cancel.
	certs.wg.Add(1)
	go func() { defer certs.wg.Done(); <-certs.ctx.Done(); close(entered); <-release }()
	done := make(chan struct{})
	var serveErr error
	go func() {
		defer close(done)
		serveErr = serveListeners(ctx, cfg, silentBuildLogger(), plain, nil, admin, nil, certs)
	}()
	t.Cleanup(func() {
		cancel()
		releaseCleanup()
		select {
		case <-done:
			if serveErr != nil {
				t.Errorf("shutdown: %v", serveErr)
			}
		case <-time.After(15 * time.Second):
			t.Error("shutdown did not finish")
		}
		certs.Close()
		metrics.Default.UnregisterAddedSinceForTest(pre)
	})
	if err := waitForDaemonReady(t, plain.Addr().String(), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("cleanup not started")
	}
	for _, ln := range []net.Listener{plain, admin} {
		deadline := time.Now().Add(2 * time.Second)
		for {
			conn, err := net.DialTimeout("tcp", ln.Addr().String(), 100*time.Millisecond)
			if err != nil {
				break
			}
			_ = conn.Close()
			if time.Now().After(deadline) {
				t.Fatal("application listener kept accepting during DNS cleanup")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	select {
	case <-done:
		t.Fatal("shutdown returned before certificate cleanup completed")
	default:
	}
	releaseCleanup()
}
