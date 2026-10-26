package main

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// Deferred minor B-64: a shutdown that waits on a running adoption commit
// says so once the wait outlasts the notice delay, and still returns
// exactly when the drain ends. (Moved from flip_wiring_test.go: deferred
// minor C-34.)
func TestWaitAdoptionDrain_ExplainsASlowDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs := &syncLogBuffer{}
		logger := slog.New(slog.NewJSONHandler(logs, nil))
		start := time.Now()
		waitAdoptionDrain(func() { time.Sleep(time.Second) }, adoptionDrainNotice, logger)
		if strings.Contains(logs.String(), "shutdown_waiting_for_adoptions") {
			t.Fatalf("a 1s drain was logged:\n%s", logs.String())
		}
		waitAdoptionDrain(func() { time.Sleep(time.Minute) }, adoptionDrainNotice, logger)
		if n := strings.Count(logs.String(), `"msg":"shutdown_waiting_for_adoptions"`); n != 1 {
			t.Fatalf("a 1m drain logged %d notices, want 1:\n%s", n, logs.String())
		}
		if d := time.Since(start); d != time.Second+time.Minute {
			t.Fatalf("waitAdoptionDrain returned after %v in all, want when the drains ended (1m1s)", d)
		}
	})
}

// Deferred minor C-30: a panicking drain wait is logged with its stack,
// and the shutdown carries on.
func TestWaitAdoptionDrain_LogsAPanicWithItsStack(t *testing.T) {
	logs := &syncLogBuffer{}
	waitAdoptionDrain(func() { panic("drain bug") }, time.Hour, slog.New(slog.NewJSONHandler(logs, nil)))
	out := logs.String()
	if !strings.Contains(out, `"msg":"shutdown_wait_panicked"`) || !strings.Contains(out, `"panic":"drain bug"`) ||
		!strings.Contains(out, `"stack":"goroutine`) {
		t.Fatalf("panic log = %s; want the panic and its stack", out)
	}
}

// Deferred minor C-28: the daemon starts and stops with adoption on and
// the flip barrier enabled (the gate closes at the signal, before the
// listeners drain). No daemon test reaches a flip in the barrier.
func TestServeListeners_ShutsDownWithTheBarrierOn(t *testing.T) {
	withKeyringDirs(t, []string{t.TempDir()})
	cfg := newAdoptionEnabledCfg(t, false)
	cfg.Adoption.FlipQuietPeriod.Duration = 10 * time.Second
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveListeners(ctx, cfg, silentBuildLogger(), ln, nil, nil, nil) }()
	waitServing(t, ln.Addr().String(), cancel, done)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveListeners: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the daemon did not stop within 15s with the barrier on")
	}
}
