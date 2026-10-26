package freshness

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
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
// http://<host><suite>/InRelease. Its client interest (spec C4) is
// lastSuccess, or — nil — the real wall clock's now: a checker whose now
// is more than the 7-day interest window from it sees the suite dormant,
// so such a test passes lastSuccess (deferred minor E-49) — which also
// stamps LastSuccessAt and LastCheckAt — or, for a suite never checked,
// seeds its row with fc.putSuite instead.
func seedFakeSuite(fc *fakeCache, host, suite string, lastSuccess *int64) {
	bh := testHex64
	fc.putURL(cache.URLPath{CanonicalScheme: "http", CanonicalHost: host, Path: suite + "/InRelease",
		BlobHash: &bh, UpstreamURL: "http://" + host + suite + "/InRelease", IsMetadata: true, RequestCount: 1})
	// Client interest (spec C4): at the last success, else now.
	interest := time.Now().Unix()
	if lastSuccess != nil {
		interest = *lastSuccess
	}
	fc.putSuite(cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: host, SuitePath: suite,
		LastSuccessAt: lastSuccess, LastCheckAt: lastSuccess, LastRequestedAt: &interest})
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
	q.enqueue(schedRow("a", "/f1", &one, 2), false)
	q.enqueue(schedRow("a", "/n1", nil, 0), false)
	q.enqueue(schedRow("a", "/h1", &one, 0), false)
	q.enqueue(schedRow("a", "/h2", &one, 0), false)
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
		q.enqueue(schedRow(s[0], s[1], &one, 0), false)
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
		q.enqueue(schedRow(s[0], s[1], &one, 0), false)
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

// renderedMetric returns the value of the unlabelled series name as
// metrics.Default renders it, or "" if it is absent.
func renderedMetric(name string) string {
	var buf bytes.Buffer
	metrics.Default.Render(&buf)
	for _, line := range strings.Split(buf.String(), "\n") {
		if v, ok := strings.CutPrefix(line, name+" "); ok {
			return v
		}
	}
	return ""
}

// The spec C7 alert gauge: a tick counts failing records, not refusals
// (final review T M3). Without an adopter the same records are inert
// (Phase 1 parity, final review S I3): the gauge reports 0 and a due
// retry does not queue the suite.
func TestTick_AdoptionFailingSuitesGauge(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)
	recent := now.Add(-time.Minute).Unix()
	fc := newFakeCache()
	for _, rec := range []struct{ suite, reason string }{{"/fail", "gpg_failed"}, {"/refused", cache.ReasonUpstreamOlder}} {
		seedFakeSuite(fc, "h", rec.suite, &recent)
		if err := fc.RecordAdoptionFailure(ctx, "http", "h", rec.suite, cache.AdoptionFailure{
			Hash: testHex64, Reason: rec.reason, Count: 2, FirstAt: recent, LastAt: recent, NextRetryAt: recent}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name    string
		adopter *Adopter
		gauge   string
		queued  int
	}{
		{"with an adopter", &Adopter{}, "1", 2}, // both retries are due
		{"without an adopter", nil, "0", 0},
	} {
		c, err := New(Config{Cache: fc, Fetcher: &changedUpstream{}, HostLimiter: hostsem.New(1),
			Refresh: time.Hour, Adopter: tc.adopter, Logger: discardLogger(), now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		q := newSchedQueues()
		c.tick(ctx, q)
		if got := renderedMetric("acu_adoption_failing_suites"); got != tc.gauge {
			t.Errorf("%s: acu_adoption_failing_suites = %q, want %s", tc.name, got, tc.gauge)
		}
		if q.len() != tc.queued {
			t.Errorf("%s: %d suites queued, want %d", tc.name, q.len(), tc.queued)
		}
	}
}

func TestSchedQueues_NoDuplicatesWhileQueuedOrInFlight(t *testing.T) {
	q := newSchedQueues()
	one := int64(1)
	s := schedRow("a", "/1", &one, 0)
	if !q.enqueue(s, false) || q.enqueue(s, false) {
		t.Fatal("a queued suite was queued twice")
	}
	it, _ := q.pop(admitAll)
	if q.enqueue(s, false) {
		t.Fatal("a suite was queued while its check was in flight")
	}
	q.done(it)
	if !q.enqueue(s, false) {
		t.Fatal("a suite could not be queued again after its check")
	}
}

func TestSchedQueues_PrunesIdleHosts(t *testing.T) {
	q := newSchedQueues()
	one := int64(1)
	q.enqueue(schedRow("a", "/1", &one, 0), false)
	q.enqueue(schedRow("b", "/1", &one, 0), false)
	it, _ := q.pop(admitAll)
	q.done(it)
	if len(q.hosts) != 1 || len(q.order) != 1 || q.len() != 1 {
		t.Fatalf("hosts %d order %d queued %d, want 1/1/1", len(q.hosts), len(q.order), q.len())
	}
}

// Deferred minor B-40: when the host whose turn is next is pruned, the
// turn passes to the first surviving host after it, not back to the first.
func TestSchedQueues_PruneHandsTheTurnToTheNextHost(t *testing.T) {
	q := newSchedQueues()
	one := int64(1)
	for _, h := range []string{"a", "b", "c", "d"} {
		q.enqueue(schedRow(h, "/1", &one, 0), false)
	}
	c := q.hosts["c"]
	for i := range c.classes {
		c.classes[i] = nil
	}
	delete(q.queued, suiteKey("http", "c", "/1"))
	q.next = 2 // c's turn
	q.prune()
	if got := q.order[q.next]; got != "d" || len(q.order) != 3 {
		t.Fatalf("after pruning c: order %v, turn %q; want d's turn", q.order, got)
	}
	q.next = 2 // d's turn, the last host
	q.hosts["d"].classes = [numSchedClasses][]schedItem{}
	delete(q.queued, suiteKey("http", "d", "/1"))
	q.prune()
	if got := q.order[q.next]; got != "a" {
		t.Fatalf("after pruning the last host: turn %q, want a's (wrapped)", got)
	}
}

// Deferred minor B-43: an ordinary scheduler check — neither a waiter's
// re-offer nor a pending change — honours the cooldown; a waiter's does not.
func TestCheckRecovered_OrdinaryCheckHonoursTheCooldown(t *testing.T) {
	fc := newFakeCache()
	recent := time.Now().Unix()
	seedFakeSuite(fc, "h", "/r/dists/x", &recent)
	up := &changedUpstream{body: []byte("x")}
	c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(1), Cooldown: time.Hour, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ref := SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}
	item := schedItem{ref: ref, key: suiteKey("http", "h", "/r/dists/x")}
	if rep := c.checkRecovered(context.Background(), item); rep.result != resultSkipped || up.calls.Load() != 0 {
		t.Fatalf("ordinary check inside the cooldown = %q with %d upstream calls; want skipped, 0", rep.result, up.calls.Load())
	}
	item.waiting = true
	if c.checkRecovered(context.Background(), item); up.calls.Load() != 1 {
		t.Fatalf("a waiter's re-check made %d upstream calls, want 1 (it bypasses the cooldown)", up.calls.Load())
	}
}

func TestSuiteDue(t *testing.T) {
	// suiteDue only asks whether an adopter is wired.
	c, err := New(Config{Cache: newFakeCache(), Fetcher: &changedUpstream{}, HostLimiter: hostsem.New(1),
		Refresh: time.Hour, Adopter: &Adopter{}, Logger: discardLogger()})
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
		// Spec C4: a refused client check leaves the suite due.
		{"pending change, no record", cache.SuiteFreshness{LastSuccessAt: at(-time.Minute), InReleaseChangeSeenAt: at(-time.Minute)}, true},
		{"pending change with a record waits for its retry", cache.SuiteFreshness{LastSuccessAt: at(-time.Minute), InReleaseChangeSeenAt: at(-time.Minute),
			AdoptionFailCount: 1, AdoptionNextRetryAt: at(time.Minute)}, false},
		{"pending change inside the unknown backoff", cache.SuiteFreshness{LastSuccessAt: at(-time.Minute), InReleaseChangeSeenAt: at(-time.Minute),
			LastCheckAt: at(-time.Minute), CheckFailCount: 1}, false},
	}
	for _, tc := range cases {
		if tc.s.LastRequestedAt == nil {
			tc.s.LastRequestedAt = at(-time.Minute) // client interest (spec C4)
		}
		if got := c.suiteDue(tc.s, now); got != tc.want {
			t.Errorf("%s: suiteDue = %v, want %v", tc.name, got, tc.want)
		}
	}

	// Phase 1 parity: with no adopter nothing is attempted, so neither a
	// pending change nor a record left by an earlier run makes a suite due.
	p1, err := New(Config{Cache: newFakeCache(), Fetcher: &changedUpstream{}, HostLimiter: hostsem.New(1),
		Refresh: time.Hour, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	phase1 := []struct {
		name string
		s    cache.SuiteFreshness
		want bool
	}{
		{"fresh", cache.SuiteFreshness{LastSuccessAt: at(-time.Minute)}, false},
		{"stale", cache.SuiteFreshness{LastSuccessAt: at(-2 * time.Hour)}, true},
		{"pending change", cache.SuiteFreshness{LastSuccessAt: at(-time.Minute), InReleaseChangeSeenAt: at(-time.Minute)}, false},
		{"adoption retry due", cache.SuiteFreshness{LastSuccessAt: at(-time.Minute), AdoptionFailCount: 1, AdoptionNextRetryAt: at(-time.Second)}, false},
	}
	for _, tc := range phase1 {
		if got := p1.suiteDue(tc.s, now); got != tc.want {
			t.Errorf("no adopter, %s: suiteDue = %v, want %v", tc.name, got, tc.want)
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

// Deferred minor B-39: with fewer hosts than workers, pop skips the hosts
// whose check is in flight: at most one check per host, so three hosts
// never have four checks in flight, and every suite is still checked.
func TestRun_InFlightHostsAreSkipped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCache()
		up := simtest.NewUpstream()
		var urls []string
		for h := range 3 {
			for s := range 3 {
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
		if total, perHost := up.MaxInFlight(); total != 3 || perHost != 1 {
			t.Fatalf("max in flight = %d overall, %d per host; want 3 (one per host) and 1", total, perHost)
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
			Verifier: passThroughVerifier{}, HostLimiter: hostsem.New(8),
			Logger: discardLogger(),
		})
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
// recover path itself panics (here reportPanic's log call in
// checkRecovered): a dispatched item without its done would leave its host
// in flight and a worker slot lost for good. That panic cannot be logged,
// so acu_freshness_unlogged_panics_total counts it (deferred minor B-44).
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
	// checkLockedSafe contains (and counts) a panic in its own recover
	// handler, so the panic must come from outside it to reach
	// checkRecovered's handler: a lock-map entry that is not a mutex fails
	// check's type assertion.
	c.locks.Store(suiteKey("http", "h", "/r/dists/x"), struct{}{})
	unlogged := func() float64 {
		var f float64
		if v := renderedMetric("acu_freshness_unlogged_panics_total"); v != "" {
			if _, err := fmt.Sscan(v, &f); err != nil {
				t.Fatal(err)
			}
		}
		return f
	}
	before := unlogged()
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
	if got := unlogged() - before; got != 1 {
		t.Errorf("acu_freshness_unlogged_panics_total rose by %v, want 1 (deferred minor B-44)", got)
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
	q.enqueue(s, false)
	it, _ := q.pop(admitAll)
	q.park(it, true)
	if q.enqueue(s, false) {
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
	q.enqueue(schedRow("a", "/w", &one, 0), false)
	w, _ := q.pop(admitAll)
	q.park(w, true)
	q.enqueue(schedRow("a", "/h", &one, 0), false)
	it, _ := q.pop(admitNone)
	if it.ref.SuitePath != "/h" {
		t.Fatalf("with no free capacity got %v, want the healthy suite", it.ref)
	}
	q.done(it)
	if it, _ := q.pop(admitAll); it.ref.SuitePath != "/w" || !it.waiting {
		t.Fatalf("with free capacity got %+v, want the waiting suite", it)
	}

	q2 := newSchedQueues()
	q2.enqueue(schedRow("a", "/u", &one, 0), false)
	u, _ := q2.pop(admitNone)
	q2.park(u, false)
	q2.enqueue(schedRow("a", "/s", &one, 0), false)
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
		q.enqueue(schedRow("a", p, &one, 0), false)
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
		q.enqueue(schedRow(s[0], s[1], &one, 0), false)
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
		q.enqueue(row, false)
		it, _ := q.pop(admitNone)
		q.finish(checkDone{item: it, report: checkReport{result: "changed_queued", signedCurrent: true}}, 0)
	}
	head, _ := q.pop(admitAll)
	q.finish(checkDone{item: head, report: checkReport{result: resultBusy}}, 0)
	if q.enqueue(s, false) || q.len() != 2 || q.waiting() != 2 {
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
	if it, _ := q.pop(admitAll); it.ref.SuitePath != "/1" || !it.waiting || !it.signed {
		t.Fatalf("after unhold got %+v, want /1 waiting, still signed (deferred minor B-42)", it)
	}
}

// An offer that again found no slot keeps its place but, like a busy
// one, is not offered again before the next wake or tick: the pop loop
// runs right after its result, and a peek that still said yes would
// otherwise re-offer it in a loop.
func TestSchedQueues_LostOfferIsHeldUntilTheNextWake(t *testing.T) {
	q := newSchedQueues()
	one := int64(1)
	q.enqueue(schedRow("a", "/1", &one, 0), false)
	it, _ := q.pop(admitNone)
	q.finish(checkDone{item: it, report: checkReport{result: "changed_queued"}}, 0)
	it, _ = q.pop(admitAll)
	if !it.waiting {
		t.Fatal("setup: the parked suite was not offered")
	}
	q.finish(checkDone{item: it, report: checkReport{result: "changed_queued"}}, 0)
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
		q.enqueue(schedRow("a", "/1", &one, 0), false)
		it, _ := q.pop(admitAll)
		q.finish(checkDone{item: it, report: checkReport{result: result}}, 0)
		if q.waiting() != 0 || q.len() != 0 {
			t.Errorf("%s: waiting %d len %d, want 0 and 0", result, q.waiting(), q.len())
		}
	}
}

// Spec C4: the scheduler keeps at most freshness.max_suites suites queued,
// waiting or in flight; later ticks refill to the bound.
func TestTick_QueuesAtMostMaxSuites(t *testing.T) {
	fc := newFakeCache()
	for i := range 30 {
		seedFakeSuite(fc, fmt.Sprintf("h%d", i%5), fmt.Sprintf("/s%d/dists/x", i), nil)
	}
	c, err := New(Config{Cache: fc, Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute,
		MaxSuites: 10, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	q := newSchedQueues()
	c.tick(ctx, q)
	if q.len() != 10 {
		t.Fatalf("queued after one tick = %d, want 10", q.len())
	}
	c.tick(ctx, q)
	if q.len() != 10 {
		t.Fatalf("queued after two ticks = %d, want 10", q.len())
	}
	for range 4 {
		it, ok := q.pop(admitAll)
		if !ok {
			t.Fatal("nothing to pop")
		}
		q.done(it)
	}
	c.tick(ctx, q)
	if q.len() != 10 {
		t.Fatalf("queued after four checks and a tick = %d, want 10", q.len())
	}
}

// R-D1 / I-4: a suite evicted while it waits in the scheduler for a slot
// flushes on its next offer — no row, so the check is skipped and the
// entry is done — with no lock-map entry left behind.
func TestScheduler_FlushesAnEvictedWaiter(t *testing.T) {
	fc := newFakeCache()
	seedFakeSuite(fc, "h", "/r/dists/x", nil)
	c := lockChecker(t, fc, newTestFetcher(t), nil)
	ctx := context.Background()
	q := newSchedQueues()
	c.tick(ctx, q)
	it, ok := q.pop(admitNone)
	if !ok {
		t.Fatal("nothing queued")
	}
	q.finish(checkDone{item: it, report: checkReport{result: "changed_queued"}}, 0) // waits for a slot
	fc.mu.Lock()
	delete(fc.suites, keyOf("http", "h", "/r/dists/x")) // evicted meanwhile
	fc.mu.Unlock()
	it, ok = q.pop(admitAll)
	if !ok || !it.waiting {
		t.Fatalf("offer = %+v, %v; want the waiter", it, ok)
	}
	rep := c.checkRecovered(ctx, it)
	q.finish(checkDone{item: it, report: rep}, 0)
	if rep.result != resultSkipped || q.len() != 0 || q.waiting() != 0 || c.LockCount() != 0 {
		t.Fatalf("after the offer: result %q, queued %d, waiting %d, locks %d; want skipped, 0, 0, 0",
			rep.result, q.len(), q.waiting(), c.LockCount())
	}
}

// Design review M12: when the bound cuts a tick's due list, the least
// recently checked suites get in — never checked first, and a
// last_check_at in the future (a clock step) counts as never — not the
// first by key, so a suite late in key order cannot starve behind early
// ones that keep becoming due; ties and the admitted ones keep key order.
func TestTick_BoundAdmitsTheLeastRecentlyChecked(t *testing.T) {
	fc := newFakeCache()
	recent, future := time.Now().Unix()-100, time.Now().Unix()+86400
	for i := range 6 {
		s := cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: fmt.Sprintf("/s%d/dists/x", i)}
		switch i {
		case 0:
			s.LastCheckAt = &future // a backward clock step: never checked
		case 1, 2:
			s.LastCheckAt = &recent // checked a moment ago and still due
		}
		fc.putSuite(s)
	}
	c, err := New(Config{Cache: fc, Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute,
		MaxSuites: 3, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	q := newSchedQueues()
	c.tick(context.Background(), q)
	var got []string
	for it, ok := q.pop(admitAll); ok; it, ok = q.pop(admitAll) {
		got = append(got, it.ref.SuitePath)
		q.done(it) // one check per host at a time
	}
	if want := []string{"/s0/dists/x", "/s3/dists/x", "/s4/dists/x"}; !slices.Equal(got, want) {
		t.Fatalf("queued %v, want %v (the first three of the never-checked ones)", got, want)
	}
}
