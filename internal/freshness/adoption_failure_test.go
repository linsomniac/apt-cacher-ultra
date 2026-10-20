package freshness

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// Spec C7: a failed adoption records its content key, reason, streak,
// next retry and the failing content's validators.
func TestAdoptionFailure_RecordsKeyReasonScheduleAndValidators(t *testing.T) {
	r := newRetryRig(t, 0)
	bad := r.release("bad", 0)
	r.ver.reject(hashOf(bad))
	r.up.set(bad)
	r.check()
	if r.ver.calls() != 1 {
		t.Fatalf("verifier calls = %d, want 1", r.ver.calls())
	}
	if sent := r.up.etagsSent(rigAnchorURL); len(sent) != 1 || sent[0] != "" {
		t.Fatalf("validators sent = %q, want one request without validators", sent)
	}

	row := r.row()
	now := r.clock.now().Unix()
	if row.AdoptionFailCount != 1 || strOf(row.AdoptionFailReason) != "gpg_failed" || strOf(row.AdoptionFailHash) != hashOf(bad) {
		t.Fatalf("record = count %d reason %q hash %q", row.AdoptionFailCount, strOf(row.AdoptionFailReason), strOf(row.AdoptionFailHash))
	}
	if strOf(row.AdoptionFailETag) != rigETag(bad) {
		t.Fatalf("failing validators = %q, want %q", strOf(row.AdoptionFailETag), rigETag(bad))
	}
	if i64Of(row.AdoptionNextRetryAt) != now+int64(2*time.Minute/time.Second) {
		t.Fatalf("next retry = %d, want now+120 (no jitter at rand 0.5)", i64Of(row.AdoptionNextRetryAt))
	}
	if i64Of(row.AdoptionFailFirstAt) != now || i64Of(row.AdoptionFailLastAt) != now {
		t.Fatalf("first/last = %d/%d, want %d", i64Of(row.AdoptionFailFirstAt), i64Of(row.AdoptionFailLastAt), now)
	}
	ev := r.events("adoption_deferred")
	if len(ev) != 1 || ev[0]["reason"] != "gpg_failed" || ev[0]["fail_count"] != float64(1) || ev[0]["refusal"] != false {
		t.Fatalf("adoption_deferred = %v", ev)
	}
}

// The streak and its start survive new failures; the persistent warning
// fires at 3, 6, 12, ... only.
func TestAdoptionFailure_StreakAndPersistentWarnings(t *testing.T) {
	r := newRetryRig(t, 0)
	bad := r.release("bad", 0)
	r.ver.reject(hashOf(bad))
	r.up.set(bad)
	first := r.clock.now().Unix()
	for i := 0; i < 6; i++ {
		r.check()
		r.clock.advance(2 * time.Hour) // past any next_retry_at
	}
	row := r.row()
	if row.AdoptionFailCount != 6 || i64Of(row.AdoptionFailFirstAt) != first {
		t.Fatalf("count %d first %d, want 6 and %d", row.AdoptionFailCount, i64Of(row.AdoptionFailFirstAt), first)
	}
	var counts []float64
	for _, e := range r.events("adoption_failing_persistently") {
		counts = append(counts, e["fail_count"].(float64))
	}
	if len(counts) != 2 || counts[0] != 3 || counts[1] != 6 {
		t.Fatalf("persistent warnings at %v, want [3 6]", counts)
	}
	if n := len(r.events("adoption_deferred")); n != 1 {
		t.Fatalf("adoption_deferred lines = %d, want 1 (state transitions only)", n)
	}
}

// Past-event rule (spec C4): a streak start in the future (backward
// clock step) is infinitely old, so the next failure restarts its stamp
// instead of reporting "failing 0m" until the clock catches up.
func TestAdoptionFailure_FutureStreakStartIsReset(t *testing.T) {
	r := newRetryRig(t, 0)
	bad := r.release("bad", 0)
	r.ver.reject(hashOf(bad))
	r.up.set(bad)
	now := r.clock.now().Unix()
	if err := r.cache.RecordAdoptionFailure(context.Background(), "http", rigHost, rigSuite, cache.AdoptionFailure{
		Hash: hashOf(bad), Reason: "gpg_failed", Count: 2, FirstAt: now + 86400, LastAt: now + 86400, NextRetryAt: now - 1}); err != nil {
		t.Fatal(err)
	}
	r.check()
	if row := r.row(); row.AdoptionFailCount != 3 || i64Of(row.AdoptionFailFirstAt) != now {
		t.Fatalf("count %d first %d, want 3 and %d", row.AdoptionFailCount, i64Of(row.AdoptionFailFirstAt), now)
	}
}

func TestIsPersistentMilestone(t *testing.T) {
	for n := int64(0); n <= 100; n++ {
		want := n == 3 || n == 6 || n == 12 || n == 24 || n == 48 || n == 96
		if got := isPersistentMilestone(n); got != want {
			t.Errorf("isPersistentMilestone(%d) = %v, want %v", n, got, want)
		}
	}
}

// Spec C7: a shutdown context cancellation is not recorded.
func TestAdoptionFailure_ShutdownIsNotRecorded(t *testing.T) {
	r := newRetryRig(t, 0)
	r.ver.blockNext()
	r.up.set(r.release("slow", 0))
	go r.checker.Check(context.Background(), "http", rigHost, rigSuite)
	<-r.ver.entered
	r.cancelLifetime()
	r.checker.WaitForAdoptions()
	if row := r.row(); row.AdoptionFailCount != 0 {
		t.Fatalf("shutdown recorded a failure: %+v", row)
	}
	if n := len(r.events("adoption_failure_record_failed")); n != 0 {
		t.Fatalf("shutdown logged %d adoption_failure_record_failed lines, want 0", n)
	}
}

// Final review spec m9: a failure record whose suite row is missing at
// the record's read is logged at WARN, not DEBUG. Under ruling R-F1 every
// suite-row deleter holds the suite's mutex, which the adoption holds
// until its record is written, so a missing row there is a bug, never a
// routine reap during the adoption.
func TestAdoptionFailure_AMissingRowAtTheRecordIsAWarning(t *testing.T) {
	r := newRetryRig(t, 0)
	r.checker.recordAdoptionFailure(&adoptionRequest{suite: SuiteRef{CanonicalScheme: "http", CanonicalHost: rigHost,
		SuitePath: "/r/dists/gone"}, key: "k"}, "verify_failed")
	recs := r.events("adoption_failure_record_failed")
	if len(recs) != 1 || recs[0]["level"] != "WARN" || recs[0]["suite_path"] != "/r/dists/gone" ||
		!strings.Contains(fmt.Sprint(recs[0]["err"]), "not found") {
		t.Fatalf("adoption_failure_record_failed lines %v; want one at WARN for the missing row", recs)
	}
}

// Spec C5 I-1 + C7: a panicking adoption is a recorded failure with
// reason panic, shown in the ring, and the suite mutex is released.
func TestAdoptionFailure_PanicIsRecorded(t *testing.T) {
	r := newRetryRig(t, 0)
	boom := r.release("boom", 0)
	r.ver.panicOn(hashOf(boom))
	r.up.set(boom)
	r.check()
	if row := r.row(); row.AdoptionFailCount != 1 || strOf(row.AdoptionFailReason) != cache.ReasonPanic {
		t.Fatalf("record = %d %q, want 1 %q", row.AdoptionFailCount, strOf(row.AdoptionFailReason), cache.ReasonPanic)
	}
	panicked := 0
	for _, ev := range r.ring.Snapshot() { // deferred minor B-21: by outcome, not position
		if ev.Outcome == "panicked" && ev.Reason == cache.ReasonPanic {
			panicked++
		}
	}
	if panicked != 1 {
		t.Fatalf("ring = %+v, want one panicked event", r.ring.Snapshot())
	}
	v, _ := r.checker.locks.Load(suiteKey("http", rigHost, rigSuite))
	mu := v.(*sync.Mutex)
	if !mu.TryLock() {
		t.Fatal("suite mutex still held after a panic")
	}
	mu.Unlock()
}

// Deferred minor B-19: a reason change mid-streak keeps the streak and
// logs adoption_deferred again (a state transition), with the retry
// attributes; the failing content's Last-Modified is recorded; a detached
// failure is keyed sha256(sha256(Release) || sha256(Release.gpg)).
func TestAdoptionFailure_ReasonChangeValidatorsAndDetachedKey(t *testing.T) {
	r := newRetryRig(t, 0)
	// An adopted suite: a never-adopted one would not attempt the new
	// content before its record's retry (ruling PE-3).
	r.up.set(r.release("v1", 0))
	r.check()
	if r.row().CurrentSnapshotID == nil {
		t.Fatal("setup: v1 not adopted")
	}
	bad := r.release("bad", 0)
	r.ver.reject(hashOf(bad))
	r.up.set(bad)
	r.check() // gpg_failed
	future := r.release("from the future", r.clock.now().Add(time.Hour).Unix())
	r.up.set(future)
	r.clock.advance(time.Minute)
	r.check() // new content, attempted at once: release_date_in_future
	row := r.row()
	if row.AdoptionFailCount != 2 || strOf(row.AdoptionFailReason) == "gpg_failed" {
		t.Fatalf("record %d %q, want the streak at 2 with the new reason", row.AdoptionFailCount, strOf(row.AdoptionFailReason))
	}
	deferred := r.events("adoption_deferred")
	if len(deferred) != 2 || deferred[1]["reason"] != strOf(row.AdoptionFailReason) || deferred[1]["fail_count"] != float64(2) {
		t.Fatalf("adoption_deferred lines %v, want two, the second for the new reason at count 2", deferred)
	}
	if deferred[1]["retry_in_seconds"] != float64(4*60) || deferred[1]["next_retry_unixtime"] != float64(i64Of(row.AdoptionNextRetryAt)) {
		t.Fatalf("retry attributes %v, want retry_in_seconds 240 and next_retry_unixtime %d", deferred[1], i64Of(row.AdoptionNextRetryAt))
	}
	// The failing content's Last-Modified is kept with its record.
	r.checker.recordAdoptionFailure(&adoptionRequest{suite: SuiteRef{CanonicalScheme: "http", CanonicalHost: rigHost,
		SuitePath: rigSuite}, key: testHex64, lastmod: "Mon, 01 Jan 2024 00:00:00 GMT"}, "gpg_failed")
	if got := strOf(r.row().AdoptionFailLastMod); got != "Mon, 01 Jan 2024 00:00:00 GMT" {
		t.Fatalf("AdoptionFailLastMod = %q", got)
	}

	d := newRetryRig(t, 0)
	seedDetachedAnchors(t, d.cache)
	rel, sig := d.releaseText("det", 0), armoredSig("det sig")
	key := contentKey(adoptionFormDetached, nil, rel, sig)
	d.ver.reject(key)
	d.up.setAt(rigReleaseURL, rel)
	d.up.setAt(rigReleaseGPGURL, sig)
	d.check()
	if got := strOf(d.row().AdoptionFailHash); got != key {
		t.Fatalf("detached record keyed %s, want %s", got, key)
	}
}

// Refusals use the record and schedule but are not failures: logged as
// adoption_refused and never as adoption_failing_persistently.
func TestAdoptionFailure_RefusalIsNotAFailure(t *testing.T) {
	r := newRetryRig(t, 0)
	d2 := r.clock.now().Add(-time.Hour).Unix()
	r.up.set(r.release("v2", d2))
	r.check()
	if r.row().CurrentSnapshotID == nil {
		t.Fatal("setup: v2 not adopted")
	}
	r.up.set(r.release("v1", d2-3600))
	for i := 0; i < 3; i++ {
		r.check()
		r.clock.advance(2 * time.Hour)
	}
	row := r.row()
	if row.AdoptionFailCount != 3 || strOf(row.AdoptionFailReason) != cache.ReasonUpstreamOlder {
		t.Fatalf("record = %d %q, want 3 upstream_older", row.AdoptionFailCount, strOf(row.AdoptionFailReason))
	}
	if n := len(r.events("adoption_refused")); n != 3 {
		t.Fatalf("adoption_refused lines = %d, want 3 (one per refused attempt)", n)
	}
	if n := len(r.events("adoption_deferred")); n != 1 {
		t.Fatalf("adoption_deferred lines = %d, want 1 (state transitions only)", n)
	}
	if n := len(r.events("adoption_failing_persistently")); n != 0 {
		t.Fatalf("a refusal raised %d persistent-failure warnings", n)
	}
}

// Final review T M1 / S M8: a refused attempt logs nothing at WARN; the
// backstop adoption_run_failed line drops to INFO for a refusal
// (adoption_refused carries the reason and timing).
func TestAdoptionFailure_RefusalLogsNoWarn(t *testing.T) {
	r := newRetryRig(t, 0)
	d2 := r.clock.now().Add(-time.Hour).Unix()
	r.up.set(r.release("v2", d2))
	r.check()
	if r.row().CurrentSnapshotID == nil {
		t.Fatal("setup: v2 not adopted")
	}
	mark := len(r.logs.String())
	r.up.set(r.release("v1", d2-3600))
	r.check() // refused: upstream_older
	if strOf(r.row().AdoptionFailReason) != cache.ReasonUpstreamOlder {
		t.Fatalf("setup: record reason %q, want upstream_older", strOf(r.row().AdoptionFailReason))
	}
	tail := r.logs.String()[mark:]
	if strings.Contains(tail, `"level":"WARN"`) {
		t.Fatalf("a refused attempt logged at WARN:\n%s", tail)
	}
	ev := r.events("adoption_run_failed")
	if len(ev) != 1 || ev[0]["level"] != "INFO" {
		t.Fatalf("adoption_run_failed = %v, want one INFO line", ev)
	}
}

// Final review ruling A5: a refusal is a state, not a failure, but one
// that has lasted 24 h (a vendor that stopped emitting Date freezes the
// suite silently otherwise) logs WARN adoption_refused_persistently at the
// streak milestones. A future streak start (backward clock step) restarts
// the streak, so its age is 0.
func TestAdoptionFailure_OldRefusalWarnsAtMilestones(t *testing.T) {
	r := newRetryRig(t, 0)
	ctx := context.Background()
	ref := SuiteRef{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite}
	now := r.clock.now()
	cases := []struct {
		name   string
		count  int64         // the stored streak; the attempt makes it count+1
		age    time.Duration // now - stored first_at
		reason string
		warn   bool
	}{
		{"milestone, 23 h old", 23, 23 * time.Hour, cache.ReasonUpstreamOlder, false},
		{"milestone, 25 h old", 47, 25 * time.Hour, cache.ReasonUpstreamOlder, true},
		{"no milestone, 30 h old", 48, 30 * time.Hour, cache.ReasonUpstreamOlder, false},
		{"milestone, streak start in the future", 23, -24 * time.Hour, cache.ReasonUpstreamOlder, false},
		{"rollback refusal, milestone, 2 days old", 95, 48 * time.Hour, cache.ReasonRollbackRefused, true},
		{"failure, milestone, 2 days old", 47, 48 * time.Hour, "gpg_failed", false},
	}
	for _, tc := range cases {
		first := now.Add(-tc.age).Unix()
		if err := r.cache.RecordAdoptionFailure(ctx, "http", rigHost, rigSuite, cache.AdoptionFailure{
			Hash: testHex64, Reason: tc.reason, Count: tc.count, FirstAt: first, LastAt: first, NextRetryAt: now.Unix()}); err != nil {
			t.Fatal(err)
		}
		before := len(r.events("adoption_refused_persistently"))
		r.checker.recordAdoptionFailure(&adoptionRequest{suite: ref, key: testHex64}, tc.reason)
		evs := r.events("adoption_refused_persistently")
		if got := len(evs) > before; got != tc.warn {
			t.Errorf("%s: adoption_refused_persistently logged = %v, want %v", tc.name, got, tc.warn)
			continue
		}
		if !tc.warn {
			continue
		}
		e := evs[len(evs)-1]
		if e["level"] != "WARN" || e["reason"] != tc.reason || e["fail_count"] != float64(tc.count+1) ||
			e["failing_since_unixtime"] != float64(first) || e["canonical_host"] != rigHost || e["suite_path"] != rigSuite {
			t.Errorf("%s: adoption_refused_persistently = %v", tc.name, e)
		}
	}
}

// Design note 18 as amended (final fix wave FW-3 (a)): a confirmation
// record (unsigned_unconfirmed, anchor_absent) restarts its confirmation
// stamp on new content or a new reason, and its retry count only on a new
// reason — new content under the same reason keeps the count growing, so
// the retry schedule backs off; the same content keeps both.
func TestAdoptionFailure_ConfirmationStampRestartsAndTheCountGrows(t *testing.T) {
	r := newRetryRig(t, 0)
	suite := SuiteRef{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite}
	t0 := r.clock.now().Unix()
	if err := r.cache.RecordAdoptionFailure(context.Background(), "http", rigHost, rigSuite, cache.AdoptionFailure{
		Hash: "k0", Reason: "gpg_failed", Count: 5, FirstAt: t0 - 3600, LastAt: t0, NextRetryAt: t0}); err != nil {
		t.Fatal(err)
	}
	step := func(key, reason string, wantCount, wantFirst int64) {
		t.Helper()
		r.checker.recordAdoptionFailure(&adoptionRequest{suite: suite, key: key}, reason)
		row := r.row()
		if row.AdoptionFailCount != wantCount || i64Of(row.AdoptionFailFirstAt) != wantFirst {
			t.Fatalf("%s %s: count %d first %d, want %d and %d", reason, key, row.AdoptionFailCount,
				i64Of(row.AdoptionFailFirstAt), wantCount, wantFirst)
		}
	}
	step("k1", cache.ReasonUnsignedUnconfirmed, 1, t0) // a new reason: both restart
	r.clock.advance(time.Minute)
	t1 := r.clock.now().Unix()
	step("k2", cache.ReasonUnsignedUnconfirmed, 2, t1) // new bytes: the stamp restarts, the count grows
	r.clock.advance(time.Minute)
	step("k2", cache.ReasonUnsignedUnconfirmed, 3, t1) // the same bytes: both continue
	r.clock.advance(time.Minute)
	t3 := r.clock.now().Unix()
	step(anchorAbsentKey, cache.ReasonAnchorAbsent, 1, t3) // a new reason again
}
