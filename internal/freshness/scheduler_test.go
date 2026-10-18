package freshness

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// drainTick runs one scheduler tick and then every queued check
// synchronously, in the order the worker pool would start them.
func drainTick(ctx context.Context, c *Checker) {
	q := newSchedQueues()
	c.tick(ctx, q)
	for {
		item, ok := q.pop()
		if !ok {
			return
		}
		c.checkRecovered(ctx, item.ref)
		q.done(item)
	}
}

// seedFakeSuite gives fc a suite with a cached InRelease anchor at
// http://<host><suite>/InRelease.
func seedFakeSuite(fc *fakeCache, host, suite string, lastSuccess *int64) {
	bh := testHex64
	fc.putURL(cache.URLPath{CanonicalScheme: "http", CanonicalHost: host, Path: suite + "/InRelease",
		BlobHash: &bh, UpstreamURL: "http://" + host + suite + "/InRelease", IsMetadata: true})
	fc.putSuite(cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: host, SuitePath: suite,
		LastSuccessAt: lastSuccess, LastCheckAt: lastSuccess})
}

func schedRow(host, suite string, lastCheck *int64, fails int64) cache.SuiteFreshness {
	return cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: host, SuitePath: suite,
		LastCheckAt: lastCheck, CheckFailCount: fails}
}

func popAll(q *schedQueues) []string {
	var out []string
	for {
		it, ok := q.pop()
		if !ok {
			return out
		}
		out = append(out, it.ref.CanonicalHost+it.ref.SuitePath)
		q.done(it)
	}
}

// Spec C4: within a host, healthy before never-checked before failing;
// first in, first out inside a class.
func TestSchedQueues_ClassesThenFIFO(t *testing.T) {
	q := newSchedQueues()
	one := int64(1)
	q.enqueue(schedRow("a", "/f1", &one, 2))
	q.enqueue(schedRow("a", "/n1", nil, 0))
	q.enqueue(schedRow("a", "/h1", &one, 0))
	q.enqueue(schedRow("a", "/h2", &one, 0))
	got := popAll(q)
	want := []string{"a/h1", "a/h2", "a/n1", "a/f1"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// Spec C4: one check per host turn, round-robin, and never two suites of
// one host in flight.
func TestSchedQueues_RoundRobinOneInFlightPerHost(t *testing.T) {
	q := newSchedQueues()
	one := int64(1)
	for _, s := range [][2]string{{"a", "/1"}, {"a", "/2"}, {"b", "/1"}, {"c", "/1"}, {"c", "/2"}} {
		q.enqueue(schedRow(s[0], s[1], &one, 0))
	}
	i1, _ := q.pop()
	i2, _ := q.pop()
	i3, _ := q.pop()
	if got := i1.ref.CanonicalHost + i2.ref.CanonicalHost + i3.ref.CanonicalHost; got != "abc" {
		t.Fatalf("first turn = %s, want abc", got)
	}
	if it, ok := q.pop(); ok {
		t.Fatalf("popped %v while every host had a check in flight", it.ref)
	}
	q.done(i1)
	if it, _ := q.pop(); it.ref.CanonicalHost+it.ref.SuitePath != "a/2" {
		t.Fatalf("after a finished: got %v, want a/2", it.ref)
	}
	q.done(i3)
	if it, _ := q.pop(); it.ref.CanonicalHost+it.ref.SuitePath != "c/2" {
		t.Fatalf("after c finished: got %v, want c/2", it.ref)
	}
}

// Rotation: after host a's turn, host b goes next even though a still
// has a suite queued.
func TestSchedQueues_RotatesHosts(t *testing.T) {
	q := newSchedQueues()
	one := int64(1)
	for _, s := range [][2]string{{"a", "/1"}, {"a", "/2"}, {"b", "/1"}, {"b", "/2"}} {
		q.enqueue(schedRow(s[0], s[1], &one, 0))
	}
	it, _ := q.pop()
	q.done(it)
	if it, _ := q.pop(); it.ref.CanonicalHost != "b" {
		t.Fatalf("after a's turn got %v, want host b (round-robin)", it.ref)
	}
}

// Suites that become due on the same tick are queued in key order
// (spec C4: a suite's position is fixed at queue time).
func TestTick_QueuesSameTickSuitesInKeyOrder(t *testing.T) {
	fc := newFakeCache()
	for _, s := range []string{"/f", "/b", "/e", "/a", "/d", "/c"} {
		seedFakeSuite(fc, "h", s, nil)
	}
	c, err := New(Config{Cache: fc, Fetcher: &changedUpstream{}, HostLimiter: hostsem.New(1), Refresh: time.Hour, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	q := newSchedQueues()
	c.tick(context.Background(), q)
	if got, want := popAll(q), []string{"h/a", "h/b", "h/c", "h/d", "h/e", "h/f"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("queue order = %v, want %v", got, want)
	}
}

func TestFailingAdoptionCount_ExcludesRefusals(t *testing.T) {
	fail, older := "gpg_failed", cache.ReasonUpstreamOlder
	suites := []cache.SuiteFreshness{
		{AdoptionFailCount: 2, AdoptionFailReason: &fail},
		{AdoptionFailCount: 1, AdoptionFailReason: &older},
		{},
	}
	if got := failingAdoptionCount(suites); got != 1 {
		t.Fatalf("failingAdoptionCount = %d, want 1 (refusals excluded)", got)
	}
}

func TestSchedQueues_NoDuplicatesWhileQueuedOrInFlight(t *testing.T) {
	q := newSchedQueues()
	one := int64(1)
	s := schedRow("a", "/1", &one, 0)
	if !q.enqueue(s) || q.enqueue(s) {
		t.Fatal("a queued suite was queued twice")
	}
	it, _ := q.pop()
	if q.enqueue(s) {
		t.Fatal("a suite was queued while its check was in flight")
	}
	q.done(it)
	if !q.enqueue(s) {
		t.Fatal("a suite could not be queued again after its check")
	}
}

func TestSchedQueues_PrunesIdleHosts(t *testing.T) {
	q := newSchedQueues()
	one := int64(1)
	q.enqueue(schedRow("a", "/1", &one, 0))
	q.enqueue(schedRow("b", "/1", &one, 0))
	it, _ := q.pop()
	q.done(it)
	if len(q.hosts) != 1 || len(q.order) != 1 || q.len() != 1 {
		t.Fatalf("hosts %d order %d queued %d, want 1/1/1", len(q.hosts), len(q.order), q.len())
	}
}

func TestSuiteDue(t *testing.T) {
	c, err := New(Config{Cache: newFakeCache(), Fetcher: &changedUpstream{}, HostLimiter: hostsem.New(1),
		Refresh: time.Hour, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	at := func(d time.Duration) *int64 { v := now.Add(d).Unix(); return &v }
	cases := []struct {
		name string
		s    cache.SuiteFreshness
		want bool
	}{
		{"never succeeded", cache.SuiteFreshness{}, true},
		{"fresh", cache.SuiteFreshness{LastSuccessAt: at(-time.Minute)}, false},
		{"stale", cache.SuiteFreshness{LastSuccessAt: at(-2 * time.Hour)}, true},
		{"exactly one refresh old", cache.SuiteFreshness{LastSuccessAt: at(-time.Hour)}, true},
		{"success in the future (clock step)", cache.SuiteFreshness{LastSuccessAt: at(time.Hour)}, true},
		{"success 2 s ahead is within tolerance", cache.SuiteFreshness{LastSuccessAt: at(2 * time.Second)}, false},
		{"failing inside backoff", cache.SuiteFreshness{LastSuccessAt: at(-2 * time.Hour), LastCheckAt: at(-time.Minute), CheckFailCount: 1}, false},
		{"failing past backoff", cache.SuiteFreshness{LastSuccessAt: at(-2 * time.Hour), LastCheckAt: at(-3 * time.Minute), CheckFailCount: 1}, true},
		{"long streak waits the hour", cache.SuiteFreshness{LastSuccessAt: at(-2 * time.Hour), LastCheckAt: at(-59 * time.Minute), CheckFailCount: 9}, false},
		{"failing check stamp in the future", cache.SuiteFreshness{LastSuccessAt: at(-2 * time.Hour), LastCheckAt: at(time.Hour), CheckFailCount: 9}, true},
		{"adoption retry due", cache.SuiteFreshness{LastSuccessAt: at(-time.Minute), AdoptionFailCount: 1, AdoptionNextRetryAt: at(-time.Second)}, true},
		{"adoption retry due exactly now", cache.SuiteFreshness{LastSuccessAt: at(-time.Minute), AdoptionFailCount: 1, AdoptionNextRetryAt: at(0)}, true},
		{"adoption retry not yet due", cache.SuiteFreshness{LastSuccessAt: at(-time.Minute), AdoptionFailCount: 1, AdoptionNextRetryAt: at(time.Minute)}, false},
	}
	for _, tc := range cases {
		if got := c.suiteDue(tc.s, now); got != tc.want {
			t.Errorf("%s: suiteDue = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Spec C4: four workers, at most one check in flight per host.
func TestRun_FourWorkersOneCheckPerHost(t *testing.T) {
	if checkWorkers != 4 {
		t.Fatalf("checkWorkers = %d, want 4 (spec C4/C11)", checkWorkers)
	}
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCache()
		up := simtest.NewUpstream()
		var urls []string
		for h := 0; h < 10; h++ {
			for s := 0; s < 3; s++ {
				host, suite := fmt.Sprintf("h%d", h), fmt.Sprintf("/s%d/dists/x", s)
				seedFakeSuite(fc, host, suite, nil)
				url := "http://" + host + suite + "/InRelease"
				up.Set(url, simtest.After(time.Minute, simtest.NotModified()))
				urls = append(urls, url)
			}
		}
		c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute, Logger: discardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { c.Run(ctx); close(done) }()
		time.Sleep(time.Hour)
		cancel()
		<-done
		if total, perHost := up.MaxInFlight(); total != checkWorkers || perHost != 1 {
			t.Fatalf("max in flight = %d overall, %d per host; want %d and 1", total, perHost, checkWorkers)
		}
		for _, u := range urls {
			if len(up.Calls(u)) == 0 {
				t.Errorf("%s never checked", u)
			}
		}
	})
}

// The carry-forward from phase A: a panic in a tick (before any check)
// must not stop the scheduler.
func TestRun_SurvivesATickPanic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCache()
		seedFakeSuite(fc, "h", "/r/dists/x", nil)
		var panicked atomic.Bool
		fc.listHook = func() {
			if panicked.CompareAndSwap(false, true) {
				panic("list bug")
			}
		}
		up := simtest.NewUpstream()
		up.Set("http://h/r/dists/x/InRelease", simtest.NotModified())
		c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute, Logger: discardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { c.Run(ctx); close(done) }()
		time.Sleep(10 * time.Minute)
		cancel()
		<-done
		if !panicked.Load() || len(up.Calls("http://h/r/dists/x/InRelease")) == 0 {
			t.Fatalf("panicked=%v calls=%d: the scheduler stopped after a tick panic", panicked.Load(), len(up.Calls("http://h/r/dists/x/InRelease")))
		}
	})
}

// Run returns only after its workers: a check still unwinding after the
// cancel delays Run's return.
func TestRun_WaitsForItsWorkers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCache()
		seedFakeSuite(fc, "h", "/r/dists/x", nil)
		up := simtest.NewUpstream()
		up.Set("http://h/r/dists/x/InRelease", func(ctx context.Context, _ string) (*fetch.ConditionalResult, error) {
			<-ctx.Done()
			time.Sleep(time.Second) // slow to unwind
			return nil, ctx.Err()
		})
		c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute, Logger: discardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { c.Run(ctx); close(done) }()
		time.Sleep(4*time.Minute + 30*time.Second)
		start := time.Now()
		cancel()
		<-done
		if d := time.Since(start); d != time.Second {
			t.Fatalf("Run returned %v after cancel, want 1s (after its worker)", d)
		}
	})
}

// Shutdown cancels in-flight checks instead of waiting out their deadline.
func TestRun_ReturnsPromptlyOnCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCache()
		seedFakeSuite(fc, "h", "/r/dists/x", nil)
		up := simtest.NewUpstream()
		up.Set("http://h/r/dists/x/InRelease", simtest.Stall())
		c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute, Logger: discardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { c.Run(ctx); close(done) }()
		time.Sleep(4*time.Minute + 30*time.Second) // the first tick (3m45s) started a stalled check
		start := time.Now()
		cancel()
		<-done
		if d := time.Since(start); d != 0 {
			t.Fatalf("Run took %v after cancel, want 0 (no wait for the check deadline)", d)
		}
	})
}
