package freshness

import (
	"context"
	"log/slog"
	"strings"
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
func TestGate_DueRetryHasNoValidators(t *testing.T) {
	r := newRetryRig(t, 0)
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
		seedDetachedAnchors(t, r.cache)
		relURL, sigURL := "http://h/r/dists/x/Release", "http://h/r/dists/x/Release.gpg"
		rel, sig1, sig2 := r.release("det", 0), []byte("sig one"), []byte("sig two")
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
	seedDetachedAnchors(t, r.cache)
	rel, sig := r.release("det", 0), []byte("sig one")
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
	good, goodSig := r.release("good", 0), []byte("good sig")
	r.up.setAt(rigReleaseURL, good)
	r.up.setAt(rigReleaseGPGURL, goodSig)
	r.check()
	if r.row().CurrentSnapshotID == nil {
		t.Fatal("setup: good pair not adopted")
	}
	bad, badSig := r.release("bad", 0), []byte("bad sig")
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
// Release + Release.gpg anchors, so checks take the detached path.
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
			BlobHash: &h, UpstreamURL: "http://h" + path, IsMetadata: true}); err != nil {
			t.Fatal(err)
		}
	}
}
