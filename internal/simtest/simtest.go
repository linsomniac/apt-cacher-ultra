// Package simtest holds helpers for virtual-clock simulations run inside
// testing/synctest bubbles with the real cache (spec §7). Only tests
// import it.
//
// AIDEV-NOTE: rules for code under simulation and for simulation authors:
//   - Code under simulation must block only on channels and timers, never
//     on a sync.Mutex held across a virtual sleep, and must use in-memory
//     fakes for network I/O (real sockets do not advance bubble time).
//   - Backward clock steps use wallclock.SetOffsetForTest. The offset is
//     process-global: never set it from a t.Parallel test. Nested calls
//     restore the previous offset.
//   - Bubble time starts at 2000-01-01 UTC, but pool file mtimes (used by
//     ServeContent's Last-Modified and by SweepTmp) are real host times, so
//     inside a bubble the pool mtime always looks newer than any stamp.
//   - Cost under -race (dev box): ~3.5 ms per freshness check and ~1 ms
//     per scheduler tick. Keep each simulation under ~5 s under -race by
//     choosing coarser intervals, not by dropping assertions.
package simtest

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
)

// OpenCache opens a real cache in a temp dir and closes it at cleanup,
// which runs inside the bubble before it ends.
func OpenCache(t *testing.T) *cache.Cache {
	t.Helper()
	c, err := cache.Open(context.Background(), t.TempDir(), DiscardLogger())
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// DiscardLogger drops all log output.
func DiscardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// StaticConditional is a freshness.Fetcher answering every conditional
// GET with Status (and Body when Status is 200), counting calls.
type StaticConditional struct {
	Status int
	Body   []byte
	calls  atomic.Int32
}

// Conditional implements freshness.Fetcher.
func (s *StaticConditional) Conditional(context.Context, *fetch.Target, string, string, int64) (*fetch.ConditionalResult, error) {
	s.calls.Add(1)
	return &fetch.ConditionalResult{Status: s.Status, Body: s.Body}, nil
}

// Calls returns how many conditional GETs were made.
func (s *StaticConditional) Calls() int32 { return s.calls.Load() }
