package admin

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
)

func TestRefresherWaitsAfterSlowRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		starts := make(chan time.Time, 10)
		start := time.Now()
		done := make(chan struct{})
		go func() {
			defer close(done)
			runRefresherLoop(ctx, time.Second, func(context.Context) {
				starts <- time.Now()
				time.Sleep(2 * time.Second)
			})
		}()
		time.Sleep(3500 * time.Millisecond)
		synctest.Wait()
		if first := <-starts; first.Sub(start) != time.Second {
			t.Fatalf("first refresh started at %s", first)
		}
		select {
		case at := <-starts:
			t.Fatalf("slow refresh immediately repeated at %s", at)
		default:
		}
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()
		if second := <-starts; second.Sub(start) != 4*time.Second {
			t.Fatalf("second refresh must wait a full interval after completion: %s", second)
		}
		cancel()
		<-done
		select {
		case at := <-starts:
			t.Fatalf("refresh started after cancellation at %s", at)
		default:
		}
	})
}

func TestRefreshMetricsIncludeSuccessfulAndFailedWork(t *testing.T) {
	ctx := context.Background()
	c, err := cache.Open(ctx, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.PutBlob(ctx, strings.Repeat("a", 64), 123); err != nil {
		t.Fatal(err)
	}
	r := metrics.NewRegistry()
	s := &Server{
		cfg:    Config{Cache: c},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		gauges: newRefresherGauges(r, 32),
	}
	s.refreshCacheStats(ctx)
	var before strings.Builder
	r.Render(&before)
	if !strings.Contains(before.String(), `acu_admin_refresh_failures_total{stage="cache_stats"} 0`) {
		t.Fatal("successful query was not recorded")
	}
	var lastSuccess string
	for _, line := range strings.Split(before.String(), "\n") {
		if strings.HasPrefix(line, `acu_admin_refresh_last_success_unixtime{stage="cache_stats"}`) {
			lastSuccess = line
		}
	}
	if lastSuccess == "" {
		t.Fatal("successful query has no completion timestamp")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	s.refreshCacheStats(ctx)
	var after strings.Builder
	r.Render(&after)
	for _, want := range []string{
		`acu_admin_refresh_duration_seconds_count{stage="cache_stats"} 2`,
		`acu_admin_refresh_failures_total{stage="cache_stats"} 1`,
		"acu_blobs_db_count 1",
		"acu_blobs_db_total_bytes 123",
		lastSuccess,
	} {
		if !strings.Contains(after.String(), want) {
			t.Errorf("missing %q after failed refresh:\n%s", want, after.String())
		}
	}
}
