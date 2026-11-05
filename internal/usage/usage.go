// Package usage collects bounded, anonymous application traffic totals.
package usage

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"
)

const CheckpointInterval = 30 * time.Second
const CheckpointTimeout = 5 * time.Second

// Totals is the durable representation. Nanoseconds are summed operation
// durations, not uptime; concurrent operations each contribute their duration.
type Totals struct {
	SinceUnix        int64 `json:"since_unixtime"`
	FetchedBytes     int64 `json:"fetched_bytes"`
	ServedBytes      int64 `json:"served_bytes"`
	CacheHitBytes    int64 `json:"cache_hit_bytes"`
	Requests         int64 `json:"requests"`
	CacheHitRequests int64 `json:"cache_hit_requests"`
	FetchNanoseconds int64 `json:"fetch_nanoseconds"`
	ServeNanoseconds int64 `json:"serve_nanoseconds"`
}

func (t Totals) Validate() error {
	if t.SinceUnix <= 0 || t.FetchedBytes < 0 || t.ServedBytes < 0 ||
		t.CacheHitBytes < 0 || t.CacheHitBytes > t.ServedBytes || t.Requests < 0 ||
		t.CacheHitRequests < 0 || t.CacheHitRequests > t.Requests ||
		t.FetchNanoseconds < 0 || t.ServeNanoseconds < 0 {
		return fmt.Errorf("usage: invalid stored totals")
	}
	return nil
}

type Store interface {
	LoadUsage(context.Context) (Totals, error)
	SaveUsage(context.Context, Totals) error
}

type Window struct {
	Totals
	NetSavedBytes      int64    `json:"net_saved_bytes"`
	AverageDownloadBPS *float64 `json:"average_download_bytes_per_second"`
	AverageUploadBPS   *float64 `json:"average_upload_bytes_per_second"`
	CacheHitPercent    *float64 `json:"cache_hit_percent"`
}

type Snapshot struct {
	Lifetime           Window `json:"lifetime"`
	Session            Window `json:"session"`
	LastCheckpointUnix int64  `json:"last_checkpoint_unixtime"`
	PersistenceError   bool   `json:"persistence_error"`
}

// Collector never performs I/O while holding mu. flushMu serializes absolute
// snapshots so a slow older checkpoint cannot overwrite a newer one.
type Collector struct {
	mu                       sync.Mutex
	flushMu                  sync.Mutex
	store                    Store
	lifetime, session, saved Totals
	lastCheckpoint           int64
	persistenceError         bool
}

// New loads existing totals. An empty store begins a new measurement period;
// unreadable/invalid totals are errors, never an implicit reset.
func New(ctx context.Context, store Store) (*Collector, error) {
	t, err := store.LoadUsage(ctx)
	if err != nil {
		return nil, fmt.Errorf("load usage: %w", err)
	}
	now := time.Now().Unix()
	if t == (Totals{}) {
		t.SinceUnix = now
	}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	c := &Collector{store: store, lifetime: t, session: Totals{SinceUnix: now}}
	if err := c.Flush(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func add(a, b int64) int64 {
	if b <= 0 {
		return a
	}
	if b > math.MaxInt64-a {
		return math.MaxInt64
	}
	return a + b
}

// RecordFetch accounts for one closed upstream response, including partial or
// discarded reads. Empty responses do not contribute to the average rate.
func (c *Collector) RecordFetch(bytes int64, elapsed time.Duration) {
	if c == nil || bytes <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range []*Totals{&c.lifetime, &c.session} {
		t.FetchedBytes = add(t.FetchedBytes, bytes)
		t.FetchNanoseconds = add(t.FetchNanoseconds, max(1, int64(elapsed)))
	}
}

// RecordServe counts a completed GET/HEAD and successful body bytes. Callers
// supply zero bytes for HEAD, 304 and unsuccessful responses.
func (c *Collector) RecordServe(bytes int64, elapsed time.Duration, hit bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range []*Totals{&c.lifetime, &c.session} {
		t.Requests = add(t.Requests, 1)
		t.ServedBytes = add(t.ServedBytes, bytes)
		if bytes > 0 {
			t.ServeNanoseconds = add(t.ServeNanoseconds, max(1, int64(elapsed)))
		}
		if hit {
			t.CacheHitRequests = add(t.CacheHitRequests, 1)
			t.CacheHitBytes = add(t.CacheHitBytes, bytes)
		}
	}
}

func window(t Totals) Window {
	w := Window{Totals: t, NetSavedBytes: t.ServedBytes - t.FetchedBytes}
	if t.FetchNanoseconds > 0 {
		v := float64(t.FetchedBytes) / (float64(t.FetchNanoseconds) / 1e9)
		w.AverageDownloadBPS = &v
	}
	if t.ServeNanoseconds > 0 {
		v := float64(t.ServedBytes) / (float64(t.ServeNanoseconds) / 1e9)
		w.AverageUploadBPS = &v
	}
	if t.Requests > 0 {
		v := 100 * float64(t.CacheHitRequests) / float64(t.Requests)
		w.CacheHitPercent = &v
	}
	return w
}

// Snapshot is an inexpensive coherent copy; nil collectors omit the optional
// admin block in embedders/tests that have not configured usage collection.
func (c *Collector) Snapshot() *Snapshot {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return &Snapshot{Lifetime: window(c.lifetime), Session: window(c.session),
		LastCheckpointUnix: c.lastCheckpoint, PersistenceError: c.persistenceError}
}

func (c *Collector) Flush(ctx context.Context) error {
	c.flushMu.Lock()
	defer c.flushMu.Unlock()
	c.mu.Lock()
	t := c.lifetime
	unchanged := t == c.saved
	c.mu.Unlock()
	if unchanged {
		return nil
	}
	err := c.store.SaveUsage(ctx, t)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.persistenceError = err != nil
	if err != nil {
		return fmt.Errorf("save usage: %w", err)
	}
	c.saved = t
	c.lastCheckpoint = time.Now().Unix()
	return nil
}

// Run checkpoints until canceled. The owner must join Run and call Flush once
// more AFTER all traffic producers drain and BEFORE closing the store.
func (c *Collector) Run(ctx context.Context, logger *slog.Logger) {
	ticker := time.NewTicker(CheckpointInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			flushCtx, cancel := context.WithTimeout(ctx, CheckpointTimeout)
			err := c.Flush(flushCtx)
			cancel()
			if err != nil && ctx.Err() == nil {
				logger.Warn("usage_checkpoint_failed", "err", err)
			}
		}
	}
}
