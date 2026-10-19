package freshness

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
)

// admitSigned admits only signed waiters (one free slot of two).
func admitSigned(_ string, signed bool) bool { return signed }

// parkTwo parks /1 then /2 on host a as waiters (signed as given).
func parkTwo(t *testing.T, q *schedQueues, signed bool) {
	t.Helper()
	one := int64(1)
	for _, p := range []string{"/1", "/2"} {
		q.enqueue(schedRow("a", p, &one, 0), false)
		it, _ := q.pop(admitNone)
		q.finish(checkDone{item: it, report: checkReport{result: "changed_queued", signedCurrent: signed}}, 0)
	}
}

// Deferred minor B-46: lost offers in a row on one host never invert the
// waiters' order: a re-parked waiter goes back to its place (oldest first).
func TestSchedQueues_LostOffersKeepTheWaitersOrder(t *testing.T) {
	q := newSchedQueues()
	parkTwo(t, q, true)
	for _, want := range []string{"/1", "/2"} { // both offers lost, one after the other
		it, _ := q.pop(admitAll)
		if it.ref.SuitePath != want {
			t.Fatalf("offered %s, want %s", it.ref.SuitePath, want)
		}
		q.finish(checkDone{item: it, report: checkReport{result: "changed_queued", signedCurrent: true}}, 0)
	}
	q.unhold()
	if it, _ := q.pop(admitAll); it.ref.SuitePath != "/1" {
		t.Fatalf("after two lost offers the head is %s, want /1 (the oldest)", it.ref.SuitePath)
	}
}

// Deferred minor B-45: an offer whose check found no slot is held unless a
// slot was released after it was dispatched — that release's wake may be
// spent already, so it is offered again at once.
func TestSchedQueues_LostOfferAfterAReleaseIsNotHeld(t *testing.T) {
	for _, tc := range []struct {
		name      string
		gen       uint64
		wantAgain bool
	}{
		{"no release since dispatch", 0, false},
		{"a release since dispatch", 1, true},
	} {
		q := newSchedQueues()
		one := int64(1)
		q.enqueue(schedRow("a", "/1", &one, 0), false)
		it, _ := q.pop(admitNone)
		q.finish(checkDone{item: it, report: checkReport{result: "changed_queued"}}, 0)
		it, _ = q.pop(admitAll) // the offer, dispatched at generation 0
		q.finish(checkDone{item: it, report: checkReport{result: "changed_queued"}}, tc.gen)
		if _, again := q.pop(admitAll); again != tc.wantAgain {
			t.Errorf("%s: offered again at once = %v, want %v", tc.name, again, tc.wantAgain)
		}
	}
}

// The release generation counts every release that frees something.
func TestAdmission_GenerationCountsReleases(t *testing.T) {
	a := newAdmission(2)
	l := mustAcquire(t, a, "h", true)
	l.releaseHost()
	l.releaseHost() // idempotent: frees nothing
	l.release()     // the global slot
	if g := a.generation(); g != 2 {
		t.Fatalf("generation %d, want 2", g)
	}
}

// Deferred minor B-66: a suite already queued as stale that a client
// check then leaves with a pending change bypasses the cooldown at its turn.
func TestSchedQueues_PendingChangeReachesAQueuedSuite(t *testing.T) {
	q := newSchedQueues()
	one := int64(1)
	s := schedRow("a", "/1", &one, 0)
	q.enqueue(s, false)
	if q.enqueue(s, true) {
		t.Fatal("a duplicate was queued")
	}
	it, _ := q.pop(admitAll)
	if !it.pendingChange {
		t.Fatal("the pending change marked on a queued suite was lost")
	}
	q.done(it)
	q.enqueue(s, false)
	if it, _ := q.pop(admitAll); it.pendingChange {
		t.Fatal("a pending mark outlived the suite's turn")
	}
}

// Overview "E:" carry-forward: a waiter parked as a first adoption whose
// suite a client check has since adopted (signed) is admitted as signed
// after the next tick, not only when two slots are free.
func TestTick_RefreshesTheWaitersSignedFlag(t *testing.T) {
	ctx := context.Background()
	fc := newFakeCache()
	recent := time.Unix(1_800_000_000, 0).Add(-time.Hour).Unix()
	seedFakeSuite(fc, "a", "/1", &recent)
	c, err := New(Config{Cache: fc, Fetcher: &changedUpstream{}, HostLimiter: hostsem.New(1), Refresh: time.Hour,
		Adopter: &Adopter{}, Logger: discardLogger(), now: func() time.Time { return time.Unix(1_800_000_000, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	q := newSchedQueues()
	one := int64(1)
	q.enqueue(schedRow("a", "/1", &one, 0), false)
	it, _ := q.pop(admitNone)
	q.finish(checkDone{item: it, report: checkReport{result: "first_adoption_queued"}}, 0)
	if _, ok := q.pop(admitSigned); ok {
		t.Fatal("setup: an unsigned waiter was admitted to the reserved slot")
	}
	id := int64(9)
	fc.putSnapshot(cache.SuiteSnapshot{SnapshotID: id, CanonicalScheme: "http", CanonicalHost: "a", SuitePath: "/1", Form: cache.FormInline})
	s, _ := fc.suite("http", "a", "/1")
	s.CurrentSnapshotID = &id
	fc.putSuite(s)
	c.tick(ctx, q)
	if it, ok := q.pop(admitSigned); !ok || !it.signed {
		t.Fatalf("after the tick: offered %v (%+v), want the now-signed waiter", ok, it)
	}
}

// Deferred minor B-38: the scheduler reads a future last_success_at as
// infinitely old without counting it on every tick; the check reports it.
func TestSuiteDue_FutureSuccessIsNotCountedPerTick(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	future := now.Add(time.Hour).Unix()
	c, err := New(Config{Cache: newFakeCache(), Fetcher: &changedUpstream{}, HostLimiter: hostsem.New(1),
		Refresh: time.Hour, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	steps := func() float64 {
		v, _ := strconv.ParseFloat(renderedMetric("acu_clock_steps_detected_total"), 64)
		return v
	}
	before := steps()
	for i := 0; i < 3; i++ {
		if !c.suiteDue(cache.SuiteFreshness{LastSuccessAt: &future}, now) {
			t.Fatal("a future last_success_at is not due")
		}
	}
	if steps() != before {
		t.Fatal("suiteDue counted the clock step")
	}
}

// Deferred minor D-23: when the queue bound cuts a tick's due list, the
// admitted suites keep key order (C4's FIFO by queue time).
func TestBoundDue_AdmittedSuitesKeepKeyOrder(t *testing.T) {
	now := int64(1_800_000_000)
	at := func(ago int64) *int64 { v := now - ago; return &v }
	due := []dueSuite{
		{key: "http|h|/a", s: cache.SuiteFreshness{LastCheckAt: at(100)}},
		{key: "http|h|/b", s: cache.SuiteFreshness{LastCheckAt: at(1000)}},
		{key: "http|h|/c", s: cache.SuiteFreshness{LastCheckAt: at(2000)}},
	}
	got := boundDue(due, newSchedQueues(), 2, now)
	if len(got) != 2 || got[0].key != "http|h|/b" || got[1].key != "http|h|/c" {
		t.Fatalf("admitted %v, want /b then /c (the two least recently checked, in key order)", got)
	}
}

// Deferred minor B-66 (proposed by the executability review, D5): a
// pending mark that reaches a suite while it is in flight (a client check
// leaves a pending change during the scheduled check) is dropped when the
// suite's turn ends (done), so it never makes the suite's NEXT queued
// turn bypass the cooldown. Kills "done keeps the pending mark", which
// TestSchedQueues_PendingChangeReachesAQueuedSuite cannot: there pop has
// already dropped the mark before done runs.
func TestSchedQueues_PendingMarkWhileInFlightEndsWithTheTurn(t *testing.T) {
	q := newSchedQueues()
	one := int64(1)
	s := schedRow("a", "/1", &one, 0)
	q.enqueue(s, false)
	it, ok := q.pop(admitAll)
	if !ok || it.pendingChange {
		t.Fatalf("setup: pop = %+v, %v; want the suite without a pending mark", it, ok)
	}
	if q.enqueue(s, true) { // in flight: a duplicate, the mark is noted
		t.Fatal("an in-flight suite was queued twice")
	}
	q.done(it)
	q.enqueue(s, false)
	next, ok := q.pop(admitAll)
	if !ok {
		t.Fatal("the suite was not queued again after its turn")
	}
	if next.pendingChange {
		t.Fatal("a pending mark noted while the suite was in flight outlived its turn")
	}
}
