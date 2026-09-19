package gc

import (
	"context"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"
)

// A slow log sink holds the first tick open beyond its interval without
// adding a scheduler-only seam to production. The fake clock makes the
// regression deterministic and avoids a wall-clock delay in the suite.
func TestRun_WaitsFullIntervalAfterSlowTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const interval = time.Hour
		runs := make(chan time.Time, 10)
		release := make(chan struct{})
		h := &blockingRunHandler{runs: runs, release: release}
		g := &GC{cfg: Config{
			Enabled: true, Interval: interval, Logger: slog.New(h),
			// The expired budget keeps this scheduling test independent of
			// SQLite: no pass starts a batch, so no cache is needed.
			MaxTickDuration: -time.Second,
		}}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go g.Run(ctx)
		first := <-runs
		time.Sleep(2 * interval)
		close(release)
		synctest.Wait()
		time.Sleep(interval - time.Nanosecond)
		synctest.Wait()
		select {
		case at := <-runs:
			t.Fatalf("next tick started at %s, before a full interval after completion", at)
		default:
		}
		time.Sleep(time.Nanosecond)
		next := <-runs
		if want := first.Add(3 * interval); !next.Equal(want) {
			t.Errorf("next tick = %s, want %s", next, want)
		}
		cancel()
		synctest.Wait()
	})
}

type blockingRunHandler struct {
	runs    chan time.Time
	release chan struct{}
	blocked bool
}

func (h *blockingRunHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *blockingRunHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *blockingRunHandler) WithGroup(string) slog.Handler            { return h }
func (h *blockingRunHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "gc_run_complete" {
		h.runs <- time.Now()
		if !h.blocked {
			h.blocked = true
			<-h.release
		}
	}
	return nil
}
