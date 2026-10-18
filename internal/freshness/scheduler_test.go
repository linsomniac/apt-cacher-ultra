package freshness

import (
	"context"
	"fmt"
	"log/slog"
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
		item, ok := q.pop(admitAll)
		if !ok {
			return
		}
		c.checkRecovered(ctx, item)
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
		it, ok := q.pop(admitAll)
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
	i1, _ := q.pop(admitAll)
	i2, _ := q.pop(admitAll)
	i3, _ := q.pop(admitAll)
	if got := i1.ref.CanonicalHost + i2.ref.CanonicalHost + i3.ref.CanonicalHost; got != "abc" {
		t.Fatalf("first turn = %s, want abc", got)
	}
	if it, ok := q.pop(admitAll); ok {
		t.Fatalf("popped %v while every host had a check in flight", it.ref)
	}
	q.done(i1)
	if it, _ := q.pop(admitAll); it.ref.CanonicalHost+it.ref.SuitePath != "a/2" {
		t.Fatalf("after a finished: got %v, want a/2", it.ref)
	}
	q.done(i3)
	if it, _ := q.pop(admitAll); it.ref.CanonicalHost+it.ref.SuitePath != "c/2" {
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
	it, _ := q.pop(admitAll)
	q.done(it)
	if it, _ := q.pop(admitAll); it.ref.CanonicalHost != "b" {
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
	it, _ := q.pop(admitAll)
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
	it, _ := q.pop(admitAll)
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

// panicLogHandler is a slog handler that panics on the records panicOn
// selects: it makes reportPanic itself panic, i.e. a panic inside a
// recover path.
type panicLogHandler struct{ panicOn func(slog.Record) bool }

func (panicLogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h panicLogHandler) Handle(_ context.Context, r slog.Record) error {
	if h.panicOn(r) {
		panic("log handler bug: " + r.Message)
	}
	return nil
}

func (h panicLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h panicLogHandler) WithGroup(string) slog.Handler      { return h }

// A panic in the restart reschedule, which Run calls before the
// scheduler starts, is recovered: the scheduler still runs (global
// constraint: every goroutine under internal/freshness recovers panics).
func TestRun_SurvivesARescheduleRestartPanic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCache()
		seedFakeSuite(fc, "h", "/r/dists/x", nil)
		s, _ := fc.suite("http", "h", "/r/dists/x")
		reason := "gpg_failed"
		s.AdoptionFailCount, s.AdoptionFailReason = 1, &reason
		fc.putSuite(s)
		up := simtest.NewUpstream()
		up.Set("http://h/r/dists/x/InRelease", simtest.NotModified())
		ad, err := NewAdopter(AdoptionConfig{Cache: simtest.OpenCache(t), Fetcher: newFakeFetcher(),
			Verifier: passThroughVerifier{}, HostLimiter: hostsem.New(8)})
		if err != nil {
			t.Fatal(err)
		}
		logger := slog.New(panicLogHandler{panicOn: func(r slog.Record) bool { return r.Message == "adoption_retries_rescheduled" }})
		c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute,
			Adopter: ad, Logger: logger})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		exited := make(chan any, 1)
		go func() {
			defer func() { exited <- recover() }()
			c.Run(ctx)
		}()
		time.Sleep(10 * time.Minute)
		cancel()
		if r := <-exited; r != nil {
			t.Fatalf("a reschedule panic escaped Run: %v", r)
		}
		if len(up.Calls("http://h/r/dists/x/InRelease")) == 0 {
			t.Fatal("the scheduler did not run after a reschedule panic")
		}
	})
}

// A worker sends done for every item it received, even when the check's
// recover path itself panics (here reportPanic's log call): a dispatched
// item without its done would leave its host in flight and a worker slot
// lost for good.
func TestCheckWorker_SendsDoneWhenTheRecoverPathPanics(t *testing.T) {
	fc := newFakeCache()
	seedFakeSuite(fc, "h", "/r/dists/x", nil)
	up := &changedUpstream{}
	up.panic.Store(true) // the check panics; its recover handlers' log calls panic too
	logger := slog.New(panicLogHandler{panicOn: func(r slog.Record) bool { return r.Level >= slog.LevelError }})
	c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	work := make(chan schedItem)
	done := make(chan checkDone, 1)
	escaped := make(chan any, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		defer func() {
			if r := recover(); r != nil {
				escaped <- r
			}
		}()
		c.checkWorker(ctx, work, done)
	}()
	item := schedItem{ref: SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"},
		key: suiteKey("http", "h", "/r/dists/x")}
	work <- item
	select {
	case d := <-done:
		if d.item.key != item.key || d.report.result != "failed" {
			t.Fatalf("done = %+v, want the item with result failed", d)
		}
	case r := <-escaped:
		t.Fatalf("a panic escaped the worker (%v): its item got no done", r)
	}
	cancel()
	<-exited
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

var (
	admitAll  = func(string, bool) bool { return true }
	admitNone = func(string, bool) bool { return false }
)

// A parked (waiting) suite stays queued: no tick re-adds it, and its
// host is not pruned.
func TestSchedQueues_ParkKeepsTheSuiteQueued(t *testing.T) {
	q := newSchedQueues()
	one := int64(1)
	s := schedRow("a", "/1", &one, 0)
	q.enqueue(s)
	it, _ := q.pop(admitAll)
	q.park(it, true)
	if q.enqueue(s) {
		t.Fatal("a tick re-queued a waiting suite")
	}
	if q.len() != 1 || q.waiting() != 1 {
		t.Fatalf("len %d waiting %d, want 1 and 1", q.len(), q.waiting())
	}
	q.prune()
	if _, ok := q.hosts["a"]; !ok {
		t.Fatal("a host with a waiting suite was pruned")
	}
}

// Within a host the oldest waiting suite the freed capacity admits goes
// first; with no admission the host's class queues are served; a signed
// waiter behind an unsigned head is not blocked by it (reserved slot).
func TestSchedQueues_PopOffersAdmissibleWaitersFirst(t *testing.T) {
	one := int64(1)
	q := newSchedQueues()
	q.enqueue(schedRow("a", "/w", &one, 0))
	w, _ := q.pop(admitAll)
	q.park(w, true)
	q.enqueue(schedRow("a", "/h", &one, 0))
	it, _ := q.pop(admitNone)
	if it.ref.SuitePath != "/h" {
		t.Fatalf("with no free capacity got %v, want the healthy suite", it.ref)
	}
	q.done(it)
	if it, _ := q.pop(admitAll); it.ref.SuitePath != "/w" || !it.waiting {
		t.Fatalf("with free capacity got %+v, want the waiting suite", it)
	}

	q2 := newSchedQueues()
	q2.enqueue(schedRow("a", "/u", &one, 0))
	u, _ := q2.pop(admitNone)
	q2.park(u, false)
	q2.enqueue(schedRow("a", "/s", &one, 0))
	s, _ := q2.pop(admitNone)
	q2.park(s, true)
	signedOnly := func(_ string, signed bool) bool { return signed }
	if it, _ := q2.pop(signedOnly); it.ref.SuitePath != "/s" {
		t.Fatalf("the reserved slot went to %v, want the signed waiter", it.ref)
	}
}

// An offer that found no slot keeps its place at the head; a newly
// waiting suite goes to the back.
func TestSchedQueues_ReparkKeepsItsPlace(t *testing.T) {
	q := newSchedQueues()
	one := int64(1)
	for _, p := range []string{"/1", "/2"} {
		q.enqueue(schedRow("a", p, &one, 0))
		it, _ := q.pop(admitNone)
		q.park(it, true)
	}
	head, _ := q.pop(admitAll)
	if head.ref.SuitePath != "/1" {
		t.Fatalf("head = %v, want /1 (oldest first)", head.ref)
	}
	q.park(head, true)
	if it, _ := q.pop(admitAll); it.ref.SuitePath != "/1" {
		t.Fatalf("a re-parked head lost its place: got %v", it.ref)
	}
}

// Hosts with waiting suites take turns. Two suites per host: the four
// setup pops (a, b, a, b) leave the round-robin cursor back on a.
func TestSchedQueues_RoundRobinAcrossWaitingHosts(t *testing.T) {
	q := newSchedQueues()
	one := int64(1)
	for _, s := range [][2]string{{"a", "/1"}, {"a", "/2"}, {"b", "/1"}, {"b", "/2"}} {
		q.enqueue(schedRow(s[0], s[1], &one, 0))
	}
	for i := 0; i < 4; i++ {
		it, ok := q.pop(admitNone)
		if !ok {
			t.Fatal("setup: nothing to park")
		}
		q.park(it, true)
	}
	var got []string
	for i := 0; i < 4; i++ {
		it, _ := q.pop(admitAll)
		got = append(got, it.ref.CanonicalHost+it.ref.SuitePath)
		q.done(it)
	}
	if want := []string{"a/1", "b/1", "a/2", "b/2"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("offer order = %v, want %v", got, want)
	}
}

// An offer that found the suite busy keeps it queued and in place, but it
// is not offered again until unhold (the next wake or tick): offering it
// at once would spin while another goroutine holds the suite mutex. A
// waiter behind it is still offered.
func TestSchedQueues_BusyOfferIsHeldUntilTheNextWake(t *testing.T) {
	q := newSchedQueues()
	one := int64(1)
	s := schedRow("a", "/1", &one, 0)
	for _, row := range []cache.SuiteFreshness{s, schedRow("a", "/2", &one, 0)} {
		q.enqueue(row)
		it, _ := q.pop(admitNone)
		q.finish(checkDone{item: it, report: checkReport{result: "changed_queued", signedCurrent: true}})
	}
	head, _ := q.pop(admitAll)
	q.finish(checkDone{item: head, report: checkReport{result: resultBusy}})
	if q.enqueue(s) || q.len() != 2 || q.waiting() != 2 {
		t.Fatalf("a busy offer dropped the suite: len %d waiting %d, want 2 and 2", q.len(), q.waiting())
	}
	it, _ := q.pop(admitAll)
	if it.ref.SuitePath != "/2" {
		t.Fatalf("with /1 held got %v, want /2", it.ref)
	}
	q.done(it)
	if it, ok := q.pop(admitAll); ok {
		t.Fatalf("a held suite was offered before the next wake: %v", it.ref)
	}
	q.unhold()
	if it, _ := q.pop(admitAll); it.ref.SuitePath != "/1" || !it.waiting {
		t.Fatalf("after unhold got %+v, want /1 waiting", it)
	}
}

// An offer that again found no slot keeps its place but, like a busy
// one, is not offered again before the next wake or tick: the pop loop
// runs right after its result, and a peek that still said yes would
// otherwise re-offer it in a loop.
func TestSchedQueues_LostOfferIsHeldUntilTheNextWake(t *testing.T) {
	q := newSchedQueues()
	one := int64(1)
	q.enqueue(schedRow("a", "/1", &one, 0))
	it, _ := q.pop(admitNone)
	q.finish(checkDone{item: it, report: checkReport{result: "changed_queued"}})
	it, _ = q.pop(admitAll)
	if !it.waiting {
		t.Fatal("setup: the parked suite was not offered")
	}
	q.finish(checkDone{item: it, report: checkReport{result: "changed_queued"}})
	if it, ok := q.pop(admitAll); ok {
		t.Fatalf("a lost offer was re-offered before the next wake: %v", it.ref)
	}
	q.unhold()
	if it, ok := q.pop(admitAll); !ok || it.ref.SuitePath != "/1" || !it.waiting {
		t.Fatalf("after unhold got %+v, want /1 waiting", it)
	}
}

// Only a queued result parks; every other result ends the suite's turn
// (a deferred change waits for its C7 retry time, not for a slot).
func TestSchedQueues_OnlyQueuedResultsPark(t *testing.T) {
	one := int64(1)
	for _, result := range []string{"changed_deferred", "failed", resultSkipped, "not_modified", "unchanged", "changed"} {
		q := newSchedQueues()
		q.enqueue(schedRow("a", "/1", &one, 0))
		it, _ := q.pop(admitAll)
		q.finish(checkDone{item: it, report: checkReport{result: result}})
		if q.waiting() != 0 || q.len() != 0 {
			t.Errorf("%s: waiting %d len %d, want 0 and 0", result, q.waiting(), q.len())
		}
	}
}
