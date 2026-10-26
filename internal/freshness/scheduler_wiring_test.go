package freshness

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// Deferred minor E-38 (B-45's wiring in Run): an offer that ends queued
// because someone else took the slot during its check, where that other
// admission ended — and its wake was spent — before the offer's done was
// handled, is offered again at once, not at the next tick: Run hands
// finish the admission generation at the done, not the offer's own.
func TestRun_AnOfferWhoseWakeWasSpentIsReofferedAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := simtest.OpenCache(t)
		up := simtest.NewUpstream()
		members := newFakeFetcher()
		seedStaleAnchorAt(t, c, "h", rigSuite)
		pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": testHex64})
		members.put("http://h"+rigSuite+"/main/binary-amd64/Packages", pkgs)
		anchor := "http://h" + rigSuite + "/InRelease"
		rel := signedRelease("v1", 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
			HostLimiter: hostsem.New(8), MaxConcurrent: 1, Logger: simtest.DiscardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		chk, err := New(Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8),
			Refresh: time.Hour, Cooldown: time.Hour, Adopter: ad, Logger: simtest.DiscardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		var (
			calls atomic.Int32
			taken atomic.Pointer[lease]
			once  sync.Once
		)
		up.Set(anchor, func(ctx context.Context, etag string) (*fetch.ConditionalResult, error) {
			if calls.Add(1) == 2 { // the first offer: another admission takes the slot meanwhile
				if l, ok := chk.admission.tryAcquire("other", true); ok {
					taken.Store(l)
				}
			}
			return simtest.Serve(rel)(ctx, etag)
		})
		chk.offerHook = func(SuiteRef, checkReport) {
			once.Do(func() {
				// The other admission ends before the offer's done is
				// handled, and Run never sees its wake (spent first).
				if l := taken.Load(); l != nil {
					l.release()
				}
				select {
				case <-chk.admission.freed:
				default:
				}
			})
		}
		ctx, cancel := context.WithCancel(context.Background())
		exited := make(chan struct{})
		go func() { chk.Run(ctx); close(exited) }()
		defer func() { cancel(); <-exited; chk.WaitForAdoptions() }()
		held := mustAcquire(t, chk.admission, "other", true)
		time.Sleep(16 * time.Minute) // the first tick (15 min): the suite waits
		released := time.Now()
		held.release() // the offer, whose check loses the slot
		time.Sleep(2 * time.Minute)
		got := up.Calls(anchor)
		if len(got) != 3 || taken.Load() == nil {
			t.Fatalf("%d anchor GETs (slot taken %v), want 3: the first tick, the lost offer and the re-offer before the next tick",
				len(got), taken.Load() != nil)
		}
		if d := got[2].Sub(released); d > time.Second { // in the bubble, "at once" is the release's own instant
			t.Fatalf("re-offered %v after the release, want at once", d)
		}
	})
}

// Deferred minor E-39: the tick's signed flag goes both ways — a waiter
// parked while its suite was signed is admitted as unsigned once its
// current snapshot is not (the promotion is TestTick_RefreshesTheWaitersSignedFlag).
func TestTick_DemotesAWaiterWhoseSnapshotIsNoLongerSigned(t *testing.T) {
	fc := newFakeCache()
	recent := time.Unix(1_800_000_000, 0).Add(-time.Hour).Unix()
	seedFakeSuite(fc, "a", "/1", &recent)
	id := int64(9)
	fc.putSnapshot(cache.SuiteSnapshot{SnapshotID: id, CanonicalScheme: "http", CanonicalHost: "a", SuitePath: "/1", Form: cache.FormUnsigned})
	s, _ := fc.suite("http", "a", "/1")
	s.CurrentSnapshotID = &id
	fc.putSuite(s)
	c, err := New(Config{Cache: fc, Fetcher: &changedUpstream{}, HostLimiter: hostsem.New(1), Refresh: time.Hour,
		Adopter: &Adopter{}, Logger: discardLogger(), now: func() time.Time { return time.Unix(1_800_000_000, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	q := newSchedQueues()
	one := int64(1)
	q.enqueue(schedRow("a", "/1", &one, 0), false)
	it, _ := q.pop(admitNone)
	q.finish(checkDone{item: it, report: checkReport{result: "changed_queued", signedCurrent: true}}, 0)
	if w := q.hosts["a"].waiting; len(w) != 1 || !w[0].signed {
		t.Fatalf("setup: waiting %+v, want one signed waiter", w)
	}
	c.tick(context.Background(), q)
	if it, ok := q.pop(admitSigned); ok {
		t.Fatalf("after the tick: offered %+v to the reserved slot, want the waiter demoted to unsigned", it)
	}
	if w := q.hosts["a"].waiting; len(w) != 1 || w[0].signed {
		t.Fatalf("after the tick: waiting %+v, want the one waiter still parked, demoted to unsigned", w)
	}
}

// Deferred minor B-27: the restart spread covers [now, now +
// periodic_refresh): rand 0 is now itself, rand 0.999 the last second of
// the window (TestRescheduleFailingAdoptions_SpreadsFailingSuitesOnly
// pins 0.5).
func TestRescheduleFailingAdoptions_SpreadBounds(t *testing.T) {
	for _, tc := range []struct {
		u    float64
		want int64 // seconds after now, over the rig's 15 min refresh
	}{{0, 0}, {0.999, 899}} {
		r := newRetryRig(t, 0)
		seedRecords(t, r, r.clock.now().Add(50*time.Minute).Unix(), map[string]string{"/fail/dists/x": "gpg_failed"})
		r.checker.rand = func() float64 { return tc.u }
		r.checker.rescheduleFailingAdoptions(context.Background())
		if got, want := nextRetryOf(t, r, "/fail/dists/x"), r.clock.now().Unix()+tc.want; got != want {
			t.Errorf("rand %v: next retry = %d, want now + %d", tc.u, got, tc.want)
		}
	}
}

// adoptionCount returns acu_adoption_total for outcome on host (0 before
// the series' first increment; a value that does not parse fails t).
func adoptionCount(t *testing.T, outcome, host string) float64 {
	t.Helper()
	return renderedFloat(t, renderedMetricLabels("acu_adoption_total", `outcome="`+outcome+`"`, `host="`+host+`"`))
}

// Deferred minor E-42: when the log handler panics while an adoption's
// panic is reported, only the report is lost (and counted unlogged): the
// attempt is still counted panicked, shown in the admin ring and recorded.
func TestAdoptionPanicWithABrokenLoggerIsStillCounted(t *testing.T) {
	r := newRetryRig(t, 0)
	body := r.release("v1", 0)
	r.ver.panicOn(hashOf(body))
	r.up.set(body)
	r.checker.logger = slog.New(panicLogHandler{panicOn: func(rec slog.Record) bool { return rec.Message == "adoption_panicked" }})
	before, unloggedBefore := adoptionCount(t, "panicked", rigHost), unlogged()
	r.check()
	if got := adoptionCount(t, "panicked", rigHost) - before; got != 1 {
		t.Fatalf("acu_adoption_total{outcome=panicked} rose by %v, want 1", got)
	}
	if got := unlogged() - unloggedBefore; got != 1 {
		t.Fatalf("unlogged panics rose by %v, want 1 (the report alone)", got)
	}
	ev := r.ring.Snapshot()
	if len(ev) == 0 || ev[len(ev)-1].Outcome != "panicked" {
		t.Fatalf("ring %+v, want the panicked attempt last", ev)
	}
	if strOf(r.row().AdoptionFailReason) != cache.ReasonPanic {
		t.Fatalf("record reason %q, want panic", strOf(r.row().AdoptionFailReason))
	}
}

// escaped runs f and returns the value of a panic that escaped it.
func escaped(f func()) (v any) {
	defer func() { v = recover() }()
	f()
	return nil
}

// Review M-2 of the H1 plan: a panic in the adoption boundary's counters
// or ring event — here the clock the ring event reads — is contained by
// its own boundary and counted unlogged; it never escapes the adoption
// goroutine (the record's own recover takes the clock's second panic).
func TestRecoverAdoption_APanickingRingEventIsContained(t *testing.T) {
	r := newRetryRig(t, 0)
	r.checker.now = func() time.Time { panic("clock bug") }
	req := &adoptionRequest{suite: SuiteRef{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite}, key: testHex64}
	before := unlogged()
	if v := escaped(func() {
		defer r.checker.recoverAdoption(req, time.Now())
		panic("adoption bug")
	}); v != nil {
		t.Fatalf("a panic escaped the adoption boundary: %v", v)
	}
	if got := unlogged() - before; got != 1 {
		t.Fatalf("unlogged panics rose by %v, want 1 (the ring event's)", got)
	}
}

// Deferred minor E-43 (B-18's second site): a panic while a failure record
// is written is reported — adoption_panicked, where=record — under its own
// boundary, so a log handler that panics too is contained and counted.
func TestRecordAdoptionFailure_RecoversAPanic(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(fmt.Sprintf("broken logger=%v", broken), func(t *testing.T) {
			r := newRetryRig(t, 0)
			r.checker.recordHook = func() { panic("record bug") }
			logs := &lockedBuffer{}
			var h slog.Handler = slog.NewJSONHandler(logs, nil)
			if broken {
				h = panicLogHandler{panicOn: func(rec slog.Record) bool { return rec.Message == "adoption_panicked" }}
			}
			r.checker.logger = slog.New(h)
			before := unlogged()
			req := &adoptionRequest{suite: SuiteRef{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite}, key: testHex64}
			if v := escaped(func() { r.checker.recordAdoptionFailure(req, "gpg_failed") }); v != nil {
				t.Fatalf("a panic escaped the record: %v", v)
			}
			if broken {
				if got := unlogged() - before; got != 1 {
					t.Fatalf("unlogged panics rose by %v, want 1", got)
				}
				return
			}
			if recs := logRecords(t, logs, "adoption_panicked"); len(recs) != 1 || recs[0]["where"] != "record" || recs[0]["level"] != "ERROR" {
				t.Fatalf("adoption_panicked records %v, want one ERROR where=record", recs)
			}
		})
	}
}

// panickyCache panics on the reads safeTick and safeReschedule guard.
type panickyCache struct{ *fakeCache }

func (panickyCache) ListSuitesWithAdoption(context.Context) ([]cache.SuiteWithAdoption, error) {
	panic("list bug")
}

func (panickyCache) RescheduleFailingAdoptions(context.Context, int64, func() int64) (int, error) {
	panic("reschedule bug")
}

// Deferred minor E-45: the repair and reconcile goroutines' boundary
// (recoverGoroutine), the restart reschedule's and the tick's report a
// panic under a second boundary, so a log handler that panics too never
// escapes them (into Run, which has none): the panic is counted unlogged.
func TestPanicBoundaries_ABrokenLoggerNeverEscapes(t *testing.T) {
	logger := slog.New(panicLogHandler{panicOn: func(r slog.Record) bool { return r.Level >= slog.LevelError }})
	c, err := New(Config{Cache: panickyCache{newFakeCache()}, Fetcher: &changedUpstream{}, HostLimiter: hostsem.New(1),
		Refresh: time.Hour, Adopter: &Adopter{}, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	suite := SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}
	for _, tc := range []struct {
		name string
		f    func()
	}{
		{"recoverGoroutine", func() { defer c.recoverGoroutine("repair", suite); panic("repair bug") }},
		{"safeReschedule", func() { c.safeReschedule(context.Background()) }},
		{"safeTick", func() { c.safeTick(context.Background(), newSchedQueues()) }},
	} {
		before := unlogged()
		if v := escaped(tc.f); v != nil {
			t.Errorf("%s: a panic escaped: %v", tc.name, v)
			continue
		}
		if got := unlogged() - before; got != 1 {
			t.Errorf("%s: unlogged panics rose by %v, want 1", tc.name, got)
		}
	}
}

// Review M-2 of Task 9 (H1): a scheduled check that panics, under a log
// handler that panics too, is contained by checkLockedSafe, which counts
// the report it could not log (unloggedPanic) exactly once; the worker
// sends the item's done and goes on to the next item.
func TestCheckWorker_ACheckPanicWithABrokenLoggerIsCounted(t *testing.T) {
	fc := newFakeCache()
	seedFakeSuite(fc, "h", "/r/dists/x", nil)
	up := &changedUpstream{}
	up.panic.Store(true) // checkLocked panics; checkLockedSafe's report panics too
	logger := slog.New(panicLogHandler{panicOn: func(r slog.Record) bool { return r.Level >= slog.LevelError }})
	c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	work := make(chan schedItem)
	done := make(chan checkDone, 1)
	exited := make(chan struct{})
	go func() { defer close(exited); c.checkWorker(ctx, work, done) }()
	defer func() { cancel(); <-exited }()
	before := unlogged()
	work <- schedItem{ref: SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"},
		key: suiteKey("http", "h", "/r/dists/x")}
	if d := <-done; d.report.result != "failed" || up.calls.Load() != 1 {
		t.Fatalf("done %+v after %d upstream calls, want result failed after the one that panicked", d, up.calls.Load())
	}
	// The worker goes on: an untracked suite's check is skipped, logging nothing.
	work <- schedItem{ref: SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/other/dists/y"},
		key: suiteKey("http", "h", "/other/dists/y")}
	if d := <-done; d.report.result != resultSkipped {
		t.Fatalf("next item: done %+v, want result %s", d, resultSkipped)
	}
	if got := unlogged() - before; got != 1 {
		t.Fatalf("unlogged panics rose by %v, want 1 (checkLockedSafe's report)", got)
	}
}
