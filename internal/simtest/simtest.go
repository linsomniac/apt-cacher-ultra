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
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
// GET with Status and Body, counting calls.
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

// Behavior answers one conditional GET for an Upstream route.
type Behavior func(ctx context.Context, etag string) (*fetch.ConditionalResult, error)

// NotModified always answers 304.
func NotModified() Behavior {
	return func(context.Context, string) (*fetch.ConditionalResult, error) {
		return &fetch.ConditionalResult{Status: 304}, nil
	}
}

// Serve answers 200 with body and its ETag, or 304 when the request's
// validator equals that ETag.
func Serve(body []byte) Behavior {
	tag := ETag(body)
	return func(_ context.Context, etag string) (*fetch.ConditionalResult, error) {
		if etag == tag {
			return &fetch.ConditionalResult{Status: 304, ETag: tag}, nil
		}
		return &fetch.ConditionalResult{Status: 200, Body: body, ETag: tag}, nil
	}
}

// Stall blocks until the request context ends: a host that accepts the
// connection and never answers.
func Stall() Behavior {
	return func(ctx context.Context, _ string) (*fetch.ConditionalResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
}

// After waits d, or until the request context ends, then answers with
// next.
func After(d time.Duration, next Behavior) Behavior {
	return func(ctx context.Context, etag string) (*fetch.ConditionalResult, error) {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
		}
		return next(ctx, etag)
	}
}

// Unavailable answers 503 at once: an unknown end that costs no time.
func Unavailable() Behavior {
	return func(context.Context, string) (*fetch.ConditionalResult, error) {
		return nil, &fetch.StatusError{Code: 503}
	}
}

// ETag is the validator Serve gives body.
func ETag(body []byte) string {
	s := sha256.Sum256(body)
	return "e-" + hex.EncodeToString(s[:6])
}

// Upstream is a freshness.Fetcher that routes by URL to a Behavior
// (unknown URLs answer 404), records the time of every request, and
// tracks how many requests are in flight, overall and per canonical host.
type Upstream struct {
	mu         sync.Mutex
	routes     map[string]Behavior
	calls      map[string][]time.Time
	inFlight   map[string]int
	total      int
	maxTotal   int
	maxPerHost int
}

// NewUpstream returns an Upstream with no routes.
func NewUpstream() *Upstream {
	return &Upstream{routes: map[string]Behavior{}, calls: map[string][]time.Time{}, inFlight: map[string]int{}}
}

// Set routes url to b, replacing any earlier route.
func (u *Upstream) Set(url string, b Behavior) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.routes[url] = b
}

// Conditional implements freshness.Fetcher.
func (u *Upstream) Conditional(ctx context.Context, target *fetch.Target, etag, _ string, _ int64) (*fetch.ConditionalResult, error) {
	host := target.CanonicalHost
	u.mu.Lock()
	b, ok := u.routes[target.URL]
	u.calls[target.URL] = append(u.calls[target.URL], time.Now())
	u.total++
	u.inFlight[host]++
	u.maxTotal = max(u.maxTotal, u.total)
	u.maxPerHost = max(u.maxPerHost, u.inFlight[host])
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		u.total--
		u.inFlight[host]--
		u.mu.Unlock()
	}()
	if !ok {
		return nil, &fetch.StatusError{Code: 404}
	}
	return b(ctx, etag)
}

// Calls returns the times url was requested, in order.
func (u *Upstream) Calls(url string) []time.Time {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]time.Time(nil), u.calls[url]...)
}

// MaxInFlight returns the most requests ever in flight at once, overall
// and on any one host, since creation or the last ResetMax.
func (u *Upstream) MaxInFlight() (total, perHost int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.maxTotal, u.maxPerHost
}

// ResetMax restarts the maxima from the requests in flight now.
func (u *Upstream) ResetMax() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.maxTotal, u.maxPerHost = u.total, 0
	for _, n := range u.inFlight {
		u.maxPerHost = max(u.maxPerHost, n)
	}
}
