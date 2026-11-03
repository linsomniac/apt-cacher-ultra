package usage

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type memoryStore struct {
	mu        sync.Mutex
	totals    Totals
	err       error
	ambiguous bool
	writes    int
}

func (s *memoryStore) LoadUsage(context.Context) (Totals, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.totals, s.err
}
func (s *memoryStore) SaveUsage(_ context.Context, v Totals) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	if s.err == nil || s.ambiguous {
		s.totals = v
	}
	return s.err
}

func collector(t *testing.T, s Store) *Collector {
	t.Helper()
	c, err := New(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestTotalsRatesAndRestart(t *testing.T) {
	s := &memoryStore{}
	c := collector(t, s)
	empty := c.Snapshot()
	if empty.Lifetime.AverageDownloadBPS != nil || empty.Lifetime.CacheHitPercent != nil {
		t.Fatal(empty)
	}
	c.RecordFetch(100, 2*time.Second)
	c.RecordFetch(0, time.Hour)
	c.RecordServe(100, time.Second, false)
	c.RecordServe(100, time.Second, true)
	c.RecordServe(0, time.Hour, true) // HEAD contributes a request, not a speed sample
	w := c.Snapshot().Lifetime
	if w.FetchedBytes != 100 || w.ServedBytes != 200 || w.CacheHitBytes != 100 ||
		w.Requests != 3 || w.CacheHitRequests != 2 || w.NetSavedBytes != 100 ||
		*w.AverageDownloadBPS != 50 || *w.AverageUploadBPS != 100 {
		t.Fatal(w)
	}
	if err := c.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	restarted := collector(t, s).Snapshot()
	if restarted.Lifetime.Totals != w.Totals || restarted.Session.Requests != 0 || restarted.Session.AverageDownloadBPS != nil {
		t.Fatal(restarted)
	}
	if _, err := json.Marshal(restarted); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointFailureAndAmbiguousCommit(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(map[bool]string{false: "failure", true: "lost acknowledgement"}[ambiguous], func(t *testing.T) {
			s := &memoryStore{}
			c := collector(t, s)
			c.RecordFetch(200, time.Second)
			s.err, s.ambiguous = errors.New("disk unavailable"), ambiguous
			if c.Flush(t.Context()) == nil || !c.Snapshot().PersistenceError {
				t.Fatal("missing failure")
			}
			c.RecordFetch(100, time.Second)
			c.RecordServe(50, time.Second, false)
			s.err = nil
			if err := c.Flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			if s.totals.FetchedBytes != 300 || c.Snapshot().PersistenceError || c.Snapshot().Lifetime.NetSavedBytes != -250 {
				t.Fatal(c.Snapshot())
			}
			writes := s.writes
			if err := c.Flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			if writes != s.writes {
				t.Fatal("idle checkpoint wrote to store")
			}
		})
	}
}

func TestConcurrentRecordSnapshotAndFlush(t *testing.T) {
	s := &memoryStore{}
	c := collector(t, s)
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			for range 1000 {
				c.RecordFetch(2, time.Millisecond)
				c.RecordServe(3, time.Millisecond, true)
				v := c.Snapshot().Lifetime
				if v.CacheHitRequests != v.Requests || v.CacheHitBytes != v.ServedBytes {
					t.Error("incoherent snapshot")
				}
			}
		})
	}
	wg.Go(func() {
		for range 100 {
			if err := c.Flush(t.Context()); err != nil {
				t.Error(err)
			}
		}
	})
	wg.Wait()
	if err := c.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s.totals.FetchedBytes != 24000 || s.totals.ServedBytes != 36000 || s.totals.Requests != 12000 {
		t.Fatal(s.totals)
	}
}

func TestSaturatingCountersAndInvalidStore(t *testing.T) {
	s := &memoryStore{totals: Totals{SinceUnix: 1, FetchedBytes: math.MaxInt64 - 1, FetchNanoseconds: math.MaxInt64 - 1}}
	c := collector(t, s)
	c.RecordFetch(100, time.Second)
	w := c.Snapshot().Lifetime
	if w.FetchedBytes != math.MaxInt64 || w.FetchNanoseconds != math.MaxInt64 || w.NetSavedBytes != -math.MaxInt64 {
		t.Fatal(w)
	}
	if _, err := json.Marshal(w); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Totals{{SinceUnix: -1}, {SinceUnix: 1, Requests: -1}, {SinceUnix: 1, CacheHitBytes: 1}, {SinceUnix: 1, CacheHitRequests: 1}} {
		if _, err := New(t.Context(), &memoryStore{totals: bad}); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	if _, err := New(t.Context(), &memoryStore{err: errors.New("read failure")}); err == nil {
		t.Fatal("silent reset")
	}
}

func TestPeriodicCheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &memoryStore{}
		c := collector(t, s)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { defer close(done); c.Run(ctx, slog.New(slog.DiscardHandler)) }()
		c.RecordServe(10, time.Second, true)
		time.Sleep(CheckpointInterval)
		synctest.Wait()
		stored, _ := s.LoadUsage(t.Context())
		if stored.ServedBytes != 10 {
			t.Fatal(stored)
		}
		cancel()
		<-done
	})
}
