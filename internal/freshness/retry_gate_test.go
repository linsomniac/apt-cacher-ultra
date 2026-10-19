package freshness

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
)

const (
	rigReleaseURL    = "http://h/r/dists/x/Release"
	rigReleaseGPGURL = "http://h/r/dists/x/Release.gpg"
)

// setIgnoreValidators makes the rig upstream answer 200 even when the
// request's validator matches (an upstream that ignores conditionals).
func (u *rigUpstream) setIgnoreValidators(v bool) { u.mu.Lock(); u.ignoreValidators = v; u.mu.Unlock() }

// failOnce leaves the rig with one recorded failure of an untrusted body.
func failOnce(t *testing.T, r *retryRig) []byte {
	t.Helper()
	bad := r.release("bad", 0)
	r.ver.reject(hashOf(bad))
	r.up.set(bad)
	r.check()
	if r.ver.calls() != 1 || r.row().AdoptionFailCount != 1 {
		t.Fatalf("setup: calls %d count %d", r.ver.calls(), r.row().AdoptionFailCount)
	}
	return bad
}

func lastResult(r *retryRig) string {
	ev := r.events("freshness_check")
	if len(ev) == 0 {
		return ""
	}
	res, _ := ev[len(ev)-1]["result"].(string)
	return res
}

// Before next_retry_at the check sends the failing content's validators;
// a 304 defers without an attempt and keeps the first-sighting stamp.
func TestGate_DeferredSendsFailingValidators(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopt()
	bad := failOnce(t, r)
	seen := r.row().InReleaseChangeSeenAt
	r.clock.advance(time.Minute)
	r.check()
	sent := r.up.etagsSent(rigAnchorURL)
	if got := sent[len(sent)-1]; got != rigETag(bad) {
		t.Fatalf("validator sent = %q, want the failing content's %q", got, rigETag(bad))
	}
	if r.ver.calls() != 1 || lastResult(r) != "changed_deferred" {
		t.Fatalf("calls %d result %q, want 1 changed_deferred", r.ver.calls(), lastResult(r))
	}
	if ev := r.events("freshness_check"); ev[len(ev)-1]["reason"] != "gpg_failed" {
		t.Fatalf("changed_deferred reason = %v, want gpg_failed", ev[len(ev)-1]["reason"])
	}
	row := r.row()
	if row.AdoptionFailCount != 1 || i64Of(row.InReleaseChangeSeenAt) != i64Of(seen) {
		t.Fatalf("deferral changed state: count %d seen %d (was %d)", row.AdoptionFailCount, i64Of(row.InReleaseChangeSeenAt), i64Of(seen))
	}
}

// An upstream that ignores validators and re-serves the failing bytes is
// deferred the same way.
func TestGate_SameContent200IsDeferred(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopt()
	failOnce(t, r)
	r.up.setIgnoreValidators(true)
	r.clock.advance(time.Minute)
	r.check()
	if r.ver.calls() != 1 || lastResult(r) != "changed_deferred" {
		t.Fatalf("calls %d result %q", r.ver.calls(), lastResult(r))
	}
}

// New content is attempted at once, whatever the schedule says.
func TestGate_NewContentIsAttemptedAtOnce(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopt()
	failOnce(t, r)
	r.up.set(r.release("fixed", 0))
	r.clock.advance(time.Minute)
	r.check()
	if r.ver.calls() != 2 {
		t.Fatalf("verifier calls = %d, want 2 (new content tried before next_retry_at)", r.ver.calls())
	}
	if row := r.row(); row.CurrentSnapshotID == nil || row.AdoptionFailCount != 0 {
		t.Fatalf("new content not adopted or record not cleared: %+v", row)
	}
}

// At next_retry_at the check sends no validators and retries.
//
// Deferred minor B-24: on an adopted suite, whose anchor has validators, so
// "none" is told apart from the current anchor's.
func TestGate_DueRetryHasNoValidators(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopt()
	if strOf(r.row().InReleaseETag) == "" {
		t.Fatal("setup: the adopted suite has no validators")
	}
	failOnce(t, r)
	r.clock.advance(2 * time.Minute)
	r.check()
	sent := r.up.etagsSent(rigAnchorURL)
	if got := sent[len(sent)-1]; got != "" {
		t.Fatalf("due retry sent validator %q, want none", got)
	}
	if r.ver.calls() != 2 || r.row().AdoptionFailCount != 2 {
		t.Fatalf("calls %d count %d, want 2 and 2", r.ver.calls(), r.row().AdoptionFailCount)
	}
}

// When upstream again serves the current snapshot's content, the record
// is cleared (spec C7).
func TestGate_CurrentContentClearsTheRecord(t *testing.T) {
	r := newRetryRig(t, 0)
	good := r.release("good", 0)
	r.up.set(good)
	r.check()
	if r.row().CurrentSnapshotID == nil {
		t.Fatal("setup: good content not adopted")
	}
	bad := r.release("bad", 0)
	r.ver.reject(hashOf(bad))
	r.up.set(bad)
	r.check()
	if r.row().AdoptionFailCount != 1 {
		t.Fatal("setup: no failure recorded")
	}
	r.up.set(good)
	r.clock.advance(time.Minute)
	r.check()
	if row := r.row(); row.AdoptionFailCount != 0 || row.InReleaseChangeSeenAt != nil || lastResult(r) != "unchanged" {
		t.Fatalf("record %d seen %v result %q, want cleared, nil, unchanged", row.AdoptionFailCount, row.InReleaseChangeSeenAt, lastResult(r))
	}
}

// A next_retry_at far in the future (backward clock step) is clamped to
// now + 1 h: a retry within the hour, not immediately (spec C7).
func TestGate_FutureNextRetryClampedToOneHour(t *testing.T) {
	r := newRetryRig(t, 0)
	failOnce(t, r)
	ctx := context.Background()
	if err := r.cache.SetAdoptionNextRetry(ctx, "http", rigHost, rigSuite, r.clock.now().Add(10*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	r.check()
	if want := r.clock.now().Add(time.Hour).Unix(); i64Of(r.row().AdoptionNextRetryAt) != want {
		t.Fatalf("next retry = %d, want clamped to %d", i64Of(r.row().AdoptionNextRetryAt), want)
	}
	r.clock.advance(59 * time.Minute)
	r.check()
	if r.ver.calls() != 1 {
		t.Fatalf("retried before the clamped hour: calls %d", r.ver.calls())
	}
	r.clock.advance(2 * time.Minute)
	r.check()
	if r.ver.calls() != 2 {
		t.Fatalf("no retry after the clamped hour: calls %d", r.ver.calls())
	}
}

// Spec C7 DoD: a burst of client-triggered checks right after a failure
// costs zero extra attempts. Cooldown 0, so every storm check reaches the
// gate (the cooldown alone would skip them and prove nothing); T1 checks
// of one suite serialize on its mutex, so they run one after another.
func TestGate_T1StormAfterFailureCostsNothing(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopt()
	bad := failOnce(t, r)
	before := len(r.up.etagsSent(rigAnchorURL))
	for i := 0; i < 50; i++ {
		r.check()
	}
	if r.ver.calls() != 1 {
		t.Fatalf("attempts after a T1 storm = %d, want 1", r.ver.calls())
	}
	sent := r.up.etagsSent(rigAnchorURL)[before:]
	if len(sent) != 50 {
		t.Fatalf("storm made %d conditional GETs, want 50", len(sent))
	}
	for i, e := range sent {
		if e != rigETag(bad) {
			t.Fatalf("storm check %d sent validator %q, want the failing content's", i, e)
		}
	}
	if lastResult(r) != "changed_deferred" {
		t.Fatalf("last result = %q, want changed_deferred", lastResult(r))
	}
}

// Spec C7: "Refusal states use the same record and schedule." After an
// upstream_older refusal, a storm of checks before the retry time costs no
// verification: each defers with reason upstream_older (final review T I2;
// a mirror stuck on older content must not cost one download and one
// verification per cooldown).
func TestGate_RefusalStormCostsNothing(t *testing.T) {
	r := newRetryRig(t, 0)
	d2 := r.clock.now().Add(-time.Hour).Unix()
	r.up.set(r.release("v2", d2))
	r.check()
	if r.row().CurrentSnapshotID == nil {
		t.Fatal("setup: v2 not adopted")
	}
	r.up.set(r.release("v1", d2-3600))
	r.check() // refused: upstream_older
	if strOf(r.row().AdoptionFailReason) != cache.ReasonUpstreamOlder {
		t.Fatalf("setup: record reason %q, want upstream_older", strOf(r.row().AdoptionFailReason))
	}
	before := r.ver.calls()
	for i := 0; i < 20; i++ {
		r.check()
	}
	if n := r.ver.calls() - before; n != 0 {
		t.Fatalf("20 checks after a refusal cost %d verifications, want 0 (refused content waits for its retry)", n)
	}
	ev := r.events("freshness_check")
	if last := ev[len(ev)-1]; last["result"] != "changed_deferred" || last["reason"] != cache.ReasonUpstreamOlder {
		t.Fatalf("last freshness_check = %v, want changed_deferred with reason upstream_older", last)
	}
}

// recordProbeCache wraps the checker's cache and, when the adoption
// goroutine writes its failure record, notes whether the suite mutex is
// still held at that moment.
type recordProbeCache struct {
	Cache
	chk              atomic.Pointer[Checker]
	key              string
	recorded, locked atomic.Bool
}

func (p *recordProbeCache) RecordAdoptionFailure(ctx context.Context, s, h, sp string, f cache.AdoptionFailure) error {
	p.recorded.Store(true)
	if v, ok := p.chk.Load().locks.Load(p.key); ok {
		if mu := v.(*sync.Mutex); mu.TryLock() {
			mu.Unlock()
		} else {
			p.locked.Store(true)
		}
	}
	return p.Cache.RecordAdoptionFailure(ctx, s, h, sp, f)
}

// Spec C7: the failure record is written before the suite mutex is
// released (error path; the panic path is pinned by
// TestCheck_AdoptionPanicRecoveredBeforeMutexRelease). Otherwise a check
// that wins the mutex in between sees no record and attempts the failing
// content again — the storm TestGate_T1StormAfterFailureCostsNothing
// cannot see, because the rig waits for each adoption (final review T I3).
func TestGate_RecordWrittenBeforeTheMutexIsReleased(t *testing.T) {
	r := newRetryRig(t, 0)
	probe := &recordProbeCache{Cache: r.cache, key: suiteKey("http", rigHost, rigSuite)}
	chk, err := New(Config{Cache: probe, Fetcher: r.up, HostLimiter: hostsem.New(8),
		Refresh: 15 * time.Minute, Adopter: r.adopter, Logger: discardLogger(),
		now: r.clock.now, rand: func() float64 { return 0.5 }})
	if err != nil {
		t.Fatal(err)
	}
	probe.chk.Store(chk)
	bad := r.release("bad", 0)
	r.ver.reject(hashOf(bad))
	r.up.set(bad)
	chk.Check(context.Background(), "http", rigHost, rigSuite)
	chk.WaitForAdoptions()
	if !probe.recorded.Load() {
		t.Fatal("setup: no failure recorded")
	}
	if !probe.locked.Load() {
		t.Fatal("the failure record was written after the suite mutex was released (spec C7: before)")
	}
}

// Phase 1 parity (overview Global Constraints): with no adopter the
// record is ignored, so a stale one left by an earlier run with adoption
// enabled changes neither the validators sent nor the result.
func TestGate_IgnoredWithoutAnAdopter(t *testing.T) {
	fc := newFakeCache()
	seedFakeAnchor(fc)
	anchorTag := "anchor-etag"
	fc.putSuite(cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", InReleaseETag: &anchorTag})
	now := int64(1_800_000_000)
	if err := fc.RecordAdoptionFailure(context.Background(), "http", "h", "/r/dists/x", cache.AdoptionFailure{
		Hash: testHex64, Reason: "gpg_failed", Count: 3, FirstAt: now, LastAt: now, NextRetryAt: now - 1}); err != nil {
		t.Fatal(err)
	}
	up := &rigUpstream{bodies: map[string][]byte{rigAnchorURL: []byte("new")}, sent: map[string][]string{}}
	logs := &lockedBuffer{}
	c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8),
		Logger: slog.New(slog.NewJSONHandler(logs, nil)), now: func() time.Time { return time.Unix(now, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		c.Check(context.Background(), "http", "h", "/r/dists/x")
	}
	if sent := up.etagsSent(rigAnchorURL); len(sent) != 2 || sent[0] != anchorTag || sent[1] != anchorTag {
		t.Fatalf("validators sent = %q, want the anchor's twice", sent)
	}
	if strings.Contains(logs.String(), "changed_deferred") {
		t.Fatalf("a Phase 1 check reported changed_deferred; log:\n%s", logs.String())
	}
}

// PR #8 limitation #1: an untrusted body checked every cooldown (60 s)
// for 12 h costs ~16 attempts, not 720.
func TestGate_DeterministicFailureIsBounded(t *testing.T) {
	r := newRetryRig(t, time.Minute)
	bad := r.release("bad", 0)
	r.ver.reject(hashOf(bad))
	r.up.set(bad)
	for elapsed := time.Duration(0); elapsed < 12*time.Hour; elapsed += time.Minute {
		r.check()
		r.clock.advance(time.Minute)
	}
	// Attempts at 0, 2, 6, 14, 30 and 62 min (6), then hourly at 122 … 662 (10): 16.
	if got := r.ver.calls(); got < 14 || got > 18 {
		t.Fatalf("attempts in 12 h = %d, want ~16", got)
	}
}

// inrelease_change_seen_at keeps the first sighting while a change is
// pending (spec C4), and validators are never taken from a changed body.
func TestChangeSeenAndValidatorsRules(t *testing.T) {
	r := newRetryRig(t, 0)
	before := r.row()
	bad := failOnce(t, r)
	first := r.row().InReleaseChangeSeenAt
	if first == nil || *first != r.clock.now().Unix() {
		t.Fatalf("first sighting = %v", first)
	}
	r.clock.advance(3 * time.Hour)
	bad2 := r.release("bad2", 0)
	r.ver.reject(hashOf(bad2))
	r.up.set(bad2)
	r.check()
	row := r.row()
	if i64Of(row.InReleaseChangeSeenAt) != *first {
		t.Fatalf("change_seen moved to %d, want first sighting %d", i64Of(row.InReleaseChangeSeenAt), *first)
	}
	if strOf(row.InReleaseETag) != strOf(before.InReleaseETag) || strOf(row.InReleaseETag) == rigETag(bad) {
		t.Fatalf("validators taken from a changed body: %q", strOf(row.InReleaseETag))
	}
}

// Detached suites: a re-signature behind a 304 waits for the retry; a
// re-signature delivered with a 200 is new content, tried at once.
func TestGate_DetachedResignature(t *testing.T) {
	for _, via200 := range []bool{false, true} {
		r := newRetryRig(t, 0)
		r.adoptDetached()
		relURL, sigURL := "http://h/r/dists/x/Release", "http://h/r/dists/x/Release.gpg"
		rel, sig1, sig2 := r.releaseText("det", 0), armoredSig("sig one"), armoredSig("sig two")
		r.ver.reject(contentKey(adoptionFormDetached, nil, rel, sig1))
		r.up.setAt(relURL, rel)
		r.up.setAt(sigURL, sig1)
		r.check()
		if r.row().AdoptionFailCount != 1 {
			t.Fatalf("via200=%v: setup: no detached failure", via200)
		}
		r.up.setAt(sigURL, sig2)
		r.up.setIgnoreValidators(via200)
		r.clock.advance(time.Minute)
		r.check()
		wantCalls := 1
		if via200 {
			wantCalls = 2
		}
		if r.ver.calls() != wantCalls {
			t.Fatalf("via200=%v: verifier calls = %d, want %d", via200, r.ver.calls(), wantCalls)
		}
		if !via200 {
			if lastResult(r) != "changed_deferred" || r.row().InReleaseChangeSeenAt == nil {
				t.Fatalf("detached 304 with a record: result %q seen %v, want changed_deferred and the first sighting kept",
					lastResult(r), r.row().InReleaseChangeSeenAt)
			}
			r.clock.advance(2 * time.Minute)
			r.check()
			if r.ver.calls() != 2 || r.row().CurrentSnapshotID == nil {
				t.Fatalf("re-signature not adopted at the retry: calls %d", r.ver.calls())
			}
		}
	}
}

// A detached upstream that ignores validators and re-serves the failing
// pair is deferred too: same content key.
func TestGate_DetachedSameContent200IsDeferred(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adoptDetached()
	rel, sig := r.releaseText("det", 0), armoredSig("sig one")
	r.ver.reject(contentKey(adoptionFormDetached, nil, rel, sig))
	r.up.setAt(rigReleaseURL, rel)
	r.up.setAt(rigReleaseGPGURL, sig)
	r.check()
	r.up.setIgnoreValidators(true)
	r.clock.advance(time.Minute)
	r.check()
	if r.ver.calls() != 1 || lastResult(r) != "changed_deferred" {
		t.Fatalf("calls %d result %q, want 1 changed_deferred", r.ver.calls(), lastResult(r))
	}
}

// When a detached upstream serves the current snapshot's pair again, the
// record is cleared (spec C7).
func TestGate_DetachedCurrentContentClearsTheRecord(t *testing.T) {
	r := newRetryRig(t, 0)
	seedDetachedAnchors(t, r.cache)
	good, goodSig := r.releaseText("good", 0), armoredSig("good sig")
	r.up.setAt(rigReleaseURL, good)
	r.up.setAt(rigReleaseGPGURL, goodSig)
	r.check()
	if r.row().CurrentSnapshotID == nil {
		t.Fatal("setup: good pair not adopted")
	}
	bad, badSig := r.releaseText("bad", 0), armoredSig("bad sig")
	r.ver.reject(contentKey(adoptionFormDetached, nil, bad, badSig))
	r.up.setAt(rigReleaseURL, bad)
	r.up.setAt(rigReleaseGPGURL, badSig)
	r.check()
	if r.row().AdoptionFailCount != 1 {
		t.Fatal("setup: no failure recorded")
	}
	r.up.setAt(rigReleaseURL, good)
	r.up.setAt(rigReleaseGPGURL, goodSig)
	r.clock.advance(time.Minute)
	r.check()
	if row := r.row(); row.AdoptionFailCount != 0 || lastResult(r) != "unchanged" {
		t.Fatalf("record %d result %q, want cleared and unchanged", row.AdoptionFailCount, lastResult(r))
	}
}

// seedDetachedAnchors removes the rig's InRelease anchor and caches stale
// Release + Release.gpg anchors (client-created), so checks take the
// detached path.
func seedDetachedAnchors(t *testing.T, c *cache.Cache) {
	t.Helper()
	ctx := context.Background()
	if err := c.EvictURLPath(ctx, "http", rigHost, rigSuite+"/InRelease"); err != nil {
		t.Fatal(err)
	}
	other := hashOf([]byte("stale Release.gpg"))
	if err := c.PutBlob(ctx, other, 1); err != nil {
		t.Fatal(err)
	}
	for path, h := range map[string]string{rigSuite + "/Release": testHex64, rigSuite + "/Release.gpg": other} {
		h := h
		if err := c.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: rigHost, Path: path,
			BlobHash: &h, UpstreamURL: "http://h" + path, IsMetadata: true, RequestCount: 1}); err != nil {
			t.Fatal(err)
		}
	}
}
