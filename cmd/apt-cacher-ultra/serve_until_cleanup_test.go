package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// serveUntilCleanup starts serve on ctx and registers a t.Cleanup that
// cancels it and waits for it to return (15 s at most). Any t.Fatal* after
// the start — the readiness wait's or a later one — then stops the daemon
// before the cleanups the test registered earlier (the shutdownTimeout
// restore, the TempDir removal) run: the H1 final review's parked item 14,
// done with the ca_constraints_log_test.go pattern. done carries serve's
// result for a test that reads it at its end; the cleanup waits on stopped,
// which the goroutine closes after sending, so a test that already read
// done costs it nothing. When the test failed and serve had returned
// before any cancel (a bind or configuration error, say), the cleanup logs
// serve's error: a readiness failure then says why the daemon never
// answered. A test that passed logs nothing, even when serve returned
// before any cancel (TestServe_ReturnsListenError's does, by design).
//
// AIDEV-NOTE: every cmd test that runs serveListeners in a goroutine drains
// it on every exit. The 17 with no drain of their own (defer cancel(), the
// result read last) start it with this helper: the 12 with a readiness
// wait, reaper_wiring_test.go's
// TestServeListeners_StartsTheReaperWhateverGCSays, and main_test.go's
// three TestServe_GracefulShutdown_* tests and TestServe_ReturnsListenError.
// The others keep their own: a t.Cleanup, registered right after the
// start, that cancels and waits, on serveDone in ca_constraints_log_test.go,
// ca_lifecycle_log_test.go, disabled_mode_parity_daemon_test.go,
// legacy_audit_wiring_test.go, reaper_wiring_test.go's
// TestServe_ForgetIsWired, status_tls_mitm_integration_test.go and
// upstream_proxy_test.go, or on a WaitGroup in main_test.go's EndToEnd and
// DisallowedHost tests, upstream_invalid_cert_test.go and
// upstream_redirect_test.go; suite_caps_wiring_test.go and
// shutdown_drain_test.go cancel and wait right after waitServing, which
// drains on its own timeout. A new daemon test starts with this helper.
// TestServeUntilCleanup pins it through a fake testing.TB whose cleanups
// it runs as a t.Fatal would.
func serveUntilCleanup(t testing.TB, ctx context.Context, cancel context.CancelFunc,
	serve func(context.Context) error) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	stopped := make(chan struct{})
	var serveErr error
	var early bool
	go func() {
		err := serve(ctx)
		serveErr, early = err, ctx.Err() == nil
		done <- err
		close(stopped)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(15 * time.Second):
			t.Errorf("serveListeners had not returned 15s after the cancel")
			return
		}
		if early && t.Failed() {
			t.Logf("serveListeners returned before any cancel: %v", serveErr)
		}
	})
	return done
}

// fakeTB records what serveUntilCleanup asks of a testing.TB: its cleanups
// (runCleanups runs them, last first, as the testing package does when a
// test returns or calls t.Fatal), its Errorf and Logf lines, and the
// Failed answer the test sets.
type fakeTB struct {
	testing.TB
	cleanups []func()
	errs     []string
	logs     []string
	failed   bool
}

func (f *fakeTB) Helper()           {}
func (f *fakeTB) Cleanup(fn func()) { f.cleanups = append(f.cleanups, fn) }
func (f *fakeTB) Errorf(format string, args ...any) {
	f.errs = append(f.errs, fmt.Sprintf(format, args...))
}
func (f *fakeTB) Logf(format string, args ...any) {
	f.logs = append(f.logs, fmt.Sprintf(format, args...))
}
func (f *fakeTB) Failed() bool { return f.failed }

func (f *fakeTB) runCleanups() {
	for i := len(f.cleanups) - 1; i >= 0; i-- {
		f.cleanups[i]()
	}
}

// TestServeUntilCleanup pins serveUntilCleanup: a test that fails before
// its end leaves a daemon that its cleanup cancels and waits for; a test
// that read done at its end costs the cleanup nothing; and a daemon that
// returned before any cancel has its error logged when the test failed,
// and only then.
func TestServeUntilCleanup(t *testing.T) {
	// slowServe is a daemon that serves until ctx is cancelled, then takes
	// 200 ms to stop, and sets stopped when it has.
	slowServe := func(stopped *atomic.Bool) func(context.Context) error {
		return func(ctx context.Context) error {
			<-ctx.Done()
			time.Sleep(200 * time.Millisecond)
			stopped.Store(true)
			return nil
		}
	}

	t.Run("a test that fails before its end", func(t *testing.T) {
		ft := &fakeTB{TB: t, failed: true}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var stopped atomic.Bool
		serveUntilCleanup(ft, ctx, cancel, slowServe(&stopped))
		ft.runCleanups() // the test's t.Fatal: done is never read
		if !stopped.Load() {
			t.Fatal("the cleanup returned before the daemon had stopped")
		}
		if len(ft.errs) != 0 || len(ft.logs) != 0 {
			t.Fatalf("errors %q, logs %q; want none", ft.errs, ft.logs)
		}
	})

	t.Run("a test that read done at its end", func(t *testing.T) {
		ft := &fakeTB{TB: t}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var stopped atomic.Bool
		done := serveUntilCleanup(ft, ctx, cancel, slowServe(&stopped))
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("serve returned %v", err)
		}
		start := time.Now()
		ft.runCleanups()
		if took := time.Since(start); took > time.Second || len(ft.errs) != 0 {
			t.Fatalf("the cleanup took %v and reported %q; want at once, nothing", took, ft.errs)
		}
	})

	t.Run("a daemon that returned before any cancel", func(t *testing.T) {
		ft := &fakeTB{TB: t, failed: true}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := serveUntilCleanup(ft, ctx, cancel, func(context.Context) error {
			return errors.New("listen tcp 127.0.0.1:1: bind: address already in use")
		})
		<-done // as a readiness wait that timed out would find it
		ft.runCleanups()
		if len(ft.logs) != 1 || !strings.Contains(ft.logs[0], "returned before any cancel: listen tcp") {
			t.Fatalf("logs %q; want serve's error, once", ft.logs)
		}
	})

	t.Run("a passing test whose daemon returned before any cancel", func(t *testing.T) {
		ft := &fakeTB{TB: t}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := serveUntilCleanup(ft, ctx, cancel, func(context.Context) error {
			return errors.New("http: accept tcp 127.0.0.1:1: use of closed network connection")
		})
		<-done // as TestServe_ReturnsListenError reads it
		ft.runCleanups()
		if len(ft.errs) != 0 || len(ft.logs) != 0 {
			t.Fatalf("errors %q, logs %q; want none", ft.errs, ft.logs)
		}
	})
}
