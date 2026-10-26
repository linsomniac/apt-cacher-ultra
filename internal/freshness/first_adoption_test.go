package freshness

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
)

// Spec C4 "Response classification", defined once: every status class.
func TestClassify_Table(t *testing.T) {
	pass := func([]byte) bool { return true }
	fail := func([]byte) bool { return false }
	status := func(code int) error { return &fetch.StatusError{Code: code} }
	cases := []struct {
		name        string
		res         *fetch.ConditionalResult
		err         error
		sniff       func([]byte) bool
		conditional bool
		want        probeClass
	}{
		{"200 passing the sniff", &fetch.ConditionalResult{Status: 200, Body: []byte("x")}, nil, pass, false, classPresent},
		{"200 failing the sniff (soft 404)", &fetch.ConditionalResult{Status: 200, Body: []byte("<html>")}, nil, fail, false, classAbsent},
		{"304 to a conditional request", &fetch.ConditionalResult{Status: 304}, nil, fail, true, classPresent},
		{"304 to an unconditional request", &fetch.ConditionalResult{Status: 304}, nil, pass, false, classUnknown},
		{"206", &fetch.ConditionalResult{Status: 206}, nil, pass, false, classUnknown},
		{"401", nil, status(401), pass, false, classAbsent},
		{"403", nil, status(403), pass, false, classAbsent},
		{"404", nil, status(404), pass, false, classAbsent},
		{"410", nil, status(410), pass, false, classAbsent},
		{"400", nil, status(400), pass, false, classUnknown},
		{"408", nil, status(408), pass, false, classUnknown},
		{"429", nil, status(429), pass, false, classUnknown},
		{"500", nil, status(500), pass, false, classUnknown},
		{"503", nil, status(503), pass, false, classUnknown},
		{"an unfollowed 3xx", nil, fmt.Errorf("%w: %w", fetch.ErrRedirectBlocked, status(302)), pass, false, classUnknown},
		{"transport error", nil, errors.New("connection reset by peer"), pass, false, classUnknown},
		{"host not allowed", nil, fetch.ErrHostNotAllowed, pass, false, classUnknown},
		{"body over its limit", nil, fetch.ErrConditionalBodyTooLarge, pass, false, classUnknown},
		{"check deadline", nil, context.DeadlineExceeded, pass, false, classUnknown},
		{"cancelled caller", nil, context.Canceled, pass, false, classUnknown},
	}
	for _, tc := range cases {
		o := classify(tc.res, tc.err, tc.sniff, tc.conditional)
		if o.class != tc.want {
			t.Errorf("%s: class %v, want %v", tc.name, o.class, tc.want)
		}
		if got, want := o.authDenied(), tc.name == "401" || tc.name == "403"; got != want {
			t.Errorf("%s: authDenied = %v, want %v", tc.name, got, want)
		}
		if got, want := o.cancelled(), tc.name == "cancelled caller"; got != want {
			t.Errorf("%s: cancelled = %v, want %v", tc.name, got, want)
		}
	}
}

// The probe tree's URLs: a sibling of a cached anchor row's upstream_url
// keeps its port and prefix; without a row, the suite identity.
func TestAnchorURLs(t *testing.T) {
	recon := func(name string) string { return "http://h/r/dists/x/" + name }
	u := anchorURLs{model: "http://h:8080/mirror/r/dists/x/Release", modelName: "Release", reconstruct: recon}
	if got := u.of("InRelease"); got != "http://h:8080/mirror/r/dists/x/InRelease" {
		t.Errorf("sibling = %q", got)
	}
	if got := (anchorURLs{reconstruct: recon}).of("Release.gpg"); got != "http://h/r/dists/x/Release.gpg" {
		t.Errorf("no model: %q", got)
	}
	if got := (anchorURLs{model: "http://h/odd", modelName: "InRelease", reconstruct: recon}).of("Release"); got != "http://h/r/dists/x/Release" {
		t.Errorf("a model not ending in its name: %q", got)
	}
}

// interestAt reseeds the rig suite's client interest at unix time at.
func (r *retryRig) interestAt(at int64) {
	r.t.Helper()
	if err := r.cache.SeedSuiteForTest(context.Background(), cache.SuiteFreshness{CanonicalScheme: "http",
		CanonicalHost: rigHost, SuitePath: rigSuite, LastRequestedAt: &at}); err != nil {
		r.t.Fatal(err)
	}
}

// Spec C4 Dispatch 3 (the phase E DoD's eager first adoption): a suite
// with client interest adopts on its first check even when upstream still
// serves what the client fetched — the probe tree sends no validators, so
// the suite's validators cannot turn it into a 304 that adopts nothing.
func TestCheckFirst_AdoptsUnchangedUpstreamOnTheFirstCheck(t *testing.T) {
	r := newRetryRig(t, 0)
	body := r.release("v1", 0)
	r.up.set(body)
	etag, interest := rigETag(body), r.clock.now().Unix()
	if err := r.cache.SeedSuiteForTest(context.Background(), cache.SuiteFreshness{CanonicalScheme: "http",
		CanonicalHost: rigHost, SuitePath: rigSuite, InReleaseETag: &etag, LastRequestedAt: &interest}); err != nil {
		t.Fatal(err)
	}
	r.check()
	if sent := r.up.etagsSent(rigAnchorURL); len(sent) != 1 || sent[0] != "" {
		t.Fatalf("validators sent = %q, want one probe without validators", sent)
	}
	ev := r.events("freshness_check")
	if len(ev) != 1 || ev[0]["result"] != "first_adoption" || ev[0]["form"] != "inline" {
		t.Fatalf("freshness_check = %v, want one first_adoption of the inline form", ev)
	}
	snap := r.currentSnapshot()
	if snap == nil || snap.Form != cache.FormInline || strOf(snap.SingleAnchorHash) != hashOf(body) {
		t.Fatalf("current snapshot %+v, want the inline v1", snap)
	}
	if r.ver.calls() != 1 {
		t.Fatalf("verifier calls = %d, want 1", r.ver.calls())
	}
}

// currentSnapshot returns the rig suite's current snapshot, or nil.
func (r *retryRig) currentSnapshot() *cache.SuiteSnapshot {
	r.t.Helper()
	row := r.row()
	if row.CurrentSnapshotID == nil {
		return nil
	}
	snap, err := r.cache.GetSuiteSnapshot(context.Background(), *row.CurrentSnapshotID)
	if err != nil {
		r.t.Fatal(err)
	}
	return snap
}

// Spec C4 Dispatch 3: each form the probe tree finds is adopted, and the
// probes stop at the first present signed anchor.
func TestCheckFirst_ProbeTreeAdoptsTheFormFound(t *testing.T) {
	t.Run("inline", func(t *testing.T) {
		r := newRetryRig(t, 0)
		r.up.set(r.release("v1", 0))
		r.up.setAt(rigReleaseURL, r.releaseText("v1", 0))
		r.check()
		if snap := r.currentSnapshot(); snap == nil || snap.Form != cache.FormInline {
			t.Fatalf("snapshot %+v, want inline", snap)
		}
		if n := len(r.up.etagsSent(rigReleaseURL)); n != 0 {
			t.Fatalf("Release probed %d times after a present InRelease, want 0", n)
		}
	})
	t.Run("detached", func(t *testing.T) {
		r := newRetryRig(t, 0)
		rel, sig := r.releaseText("v1", 0), armoredSig("v1 sig")
		r.up.setAt(rigReleaseURL, rel)
		r.up.setAt(rigReleaseGPGURL, sig)
		r.check()
		snap := r.currentSnapshot()
		if snap == nil || snap.Form != cache.FormDetached || strOf(snap.ReleaseHash) != hashOf(rel) || strOf(snap.ReleaseGPGHash) != hashOf(sig) {
			t.Fatalf("snapshot %+v, want the detached pair", snap)
		}
		if ev := r.events("freshness_check"); ev[len(ev)-1]["result"] != "first_adoption" || ev[len(ev)-1]["form"] != "detached" {
			t.Fatalf("last freshness_check = %v, want first_adoption detached", ev[len(ev)-1])
		}
	})
	t.Run("an HTML InRelease is absent", func(t *testing.T) {
		r := newRetryRig(t, 0)
		r.up.set([]byte("<html>moved</html>"))
		r.up.setAt(rigReleaseURL, r.releaseText("v1", 0))
		r.up.setAt(rigReleaseGPGURL, armoredSig("v1 sig"))
		r.check()
		if snap := r.currentSnapshot(); snap == nil || snap.Form != cache.FormDetached {
			t.Fatalf("snapshot %+v, want detached", snap)
		}
	})
}

// Spec C4 "Response classification" per anchor of the probe tree: absent
// moves on, unknown fails the check at once (no further probe, the streak
// extends), and a tree that finds no anchor fails it too — and records
// anchor_absent, so the status page shows it (ruling PE-2).
func TestCheckFirst_ClassificationPerAnchor(t *testing.T) {
	unknowns := map[string]error{
		"503":       &fetch.StatusError{Code: 503},
		"429":       &fetch.StatusError{Code: 429},
		"redirect":  fmt.Errorf("%w: %w", fetch.ErrRedirectBlocked, &fetch.StatusError{Code: 302}),
		"transport": errors.New("connection reset by peer"),
	}
	absents := map[string]error{
		"401": &fetch.StatusError{Code: 401}, "403": &fetch.StatusError{Code: 403},
		"404": &fetch.StatusError{Code: 404}, "410": &fetch.StatusError{Code: 410},
	}
	for name, err := range unknowns {
		for _, url := range []string{rigAnchorURL, rigReleaseURL, rigReleaseGPGURL} {
			t.Run("unknown "+name+" at "+url, func(t *testing.T) {
				r := newRetryRig(t, 0)
				r.up.setAt(rigReleaseURL, r.releaseText("v1", 0))
				r.up.setAt(rigReleaseGPGURL, armoredSig("v1 sig"))
				r.up.setFail(url, err)
				before := checkTotal(t, "failed", rigHost)
				r.check()
				row := r.row()
				if row.CheckFailCount != 1 || row.CurrentSnapshotID != nil || row.LastSuccessAt != nil {
					t.Fatalf("row %+v, want a failed check: streak 1, nothing adopted, no success", row)
				}
				// An unknown answer decides nothing: no record (an unknown
				// Release is not an absent anchor; final fix wave FW-6, C5).
				if row.AdoptionFailCount != 0 {
					t.Fatalf("record %q/%d after an unknown answer, want none", strOf(row.AdoptionFailReason), row.AdoptionFailCount)
				}
				if got := checkTotal(t, "failed", rigHost) - before; got != 1 {
					t.Fatalf("failed checks counted %v, want 1", got)
				}
				if url == rigAnchorURL && len(r.up.etagsSent(rigReleaseURL)) != 0 {
					t.Fatal("the tree went on after an unknown InRelease")
				}
			})
		}
	}
	for name, err := range absents {
		t.Run("absent "+name+" InRelease moves on", func(t *testing.T) {
			r := newRetryRig(t, 0)
			r.up.setFail(rigAnchorURL, err)
			r.up.setAt(rigReleaseURL, r.releaseText("v1", 0))
			r.up.setAt(rigReleaseGPGURL, armoredSig("v1 sig"))
			r.check()
			if snap := r.currentSnapshot(); snap == nil || snap.Form != cache.FormDetached {
				t.Fatalf("snapshot %+v, want detached", snap)
			}
			if r.row().CheckFailCount != 0 {
				t.Fatal("an absent anchor extended the streak")
			}
		})
	}
	t.Run("no anchor at all", func(t *testing.T) {
		r := newRetryRig(t, 0)
		r.up.setFail(rigReleaseURL, &fetch.StatusError{Code: 410})
		r.check()
		if row := r.row(); row.CheckFailCount != 1 || row.LastCheckAt == nil {
			t.Fatalf("row %+v, want a failed check", row)
		}
		if ev := r.events("freshness check failed"); len(ev) != 1 || ev[0]["form"] != "probe" {
			t.Fatalf("failed-check lines %v, want one with form probe", ev)
		}
		if row := r.row(); strOf(row.AdoptionFailReason) != cache.ReasonAnchorAbsent || row.AdoptionFailCount != 1 {
			t.Fatalf("record %q/%d, want anchor_absent/1", strOf(row.AdoptionFailReason), row.AdoptionFailCount)
		}
	})
}

// Spec C4 Dispatch 3: a never-adopted suite without client interest in the
// last 7 days, or without a client-created anchor row, makes no request and
// no write; an interest stamp up to 7 days in the future counts and is
// reported as a clock step, one further ahead is dormant (design review M-3:
// a symmetric window).
func TestCheckFirst_DormantSuitesMakeNoRequest(t *testing.T) {
	now := int64(1_800_000_000)
	for _, tc := range []struct {
		name    string
		prepare func(r *retryRig)
	}{
		{"interest exactly 7 days old", func(r *retryRig) { r.interestAt(now - 7*24*3600) }},
		{"interest 8 days old", func(r *retryRig) { r.interestAt(now - 8*24*3600) }},
		{"interest 8 days in the future", func(r *retryRig) { r.interestAt(now + 8*24*3600) }},
		{"a reconstructed anchor row (request_count 0)", func(r *retryRig) {
			stale := testHex64
			if err := r.cache.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: "http", CanonicalHost: rigHost,
				Path: rigSuite + "/InRelease", BlobHash: &stale, UpstreamURL: rigAnchorURL, IsMetadata: true}); err != nil {
				r.t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRetryRig(t, 0)
			r.up.set(r.release("v1", 0))
			tc.prepare(r)
			r.check()
			if n := len(r.up.etagsSent(rigAnchorURL)); n != 0 {
				t.Fatalf("%d requests for a dormant suite, want 0", n)
			}
			if row := r.row(); row.LastCheckAt != nil || len(r.events("freshness_check")) != 0 {
				t.Fatalf("a dormant suite was written or logged: %+v", row)
			}
		})
	}
	t.Run("interest in the future", func(t *testing.T) {
		r := newRetryRig(t, 0)
		r.up.set(r.release("v1", 0))
		r.interestAt(now + 3600)
		steps := func() float64 {
			v, _ := strconv.ParseFloat(renderedMetric("acu_clock_steps_detected_total"), 64)
			return v
		}
		before := steps()
		r.check()
		if r.currentSnapshot() == nil {
			t.Fatal("a suite whose interest stamp lies in the future was not adopted")
		}
		if steps()-before != 1 {
			t.Fatal("the future interest stamp was not reported as a clock step")
		}
	})
}

// Spec C4 Dispatch 3: the scheduler never queues a dormant never-adopted
// suite (zero requests), but still queues an adopted one without interest,
// and Phase 1 (no adopter) ignores interest.
func TestSuiteDue_DormantNeverAdoptedSuites(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	at := func(d time.Duration) *int64 { v := now.Add(d).Unix(); return &v }
	id := int64(1)
	stale := at(-2 * time.Hour)
	withAdopter, err := New(Config{Cache: newFakeCache(), Fetcher: &changedUpstream{}, HostLimiter: hostsem.New(1),
		Refresh: time.Hour, Adopter: &Adopter{}, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	phase1, err := New(Config{Cache: newFakeCache(), Fetcher: &changedUpstream{}, HostLimiter: hostsem.New(1),
		Refresh: time.Hour, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name             string
		s                cache.SuiteFreshness
		want, wantPhase1 bool
	}{
		{"never adopted, interested", cache.SuiteFreshness{LastSuccessAt: stale, LastRequestedAt: at(-time.Hour)}, true, true},
		{"never adopted, no interest stamp", cache.SuiteFreshness{LastSuccessAt: stale}, false, true},
		{"never adopted, interest 8 days old", cache.SuiteFreshness{LastSuccessAt: stale, LastRequestedAt: at(-8 * 24 * time.Hour)}, false, true},
		{"never adopted, retry due but dormant", cache.SuiteFreshness{LastSuccessAt: stale, LastRequestedAt: at(-8 * 24 * time.Hour),
			AdoptionFailCount: 1, AdoptionNextRetryAt: at(-time.Minute)}, false, true},
		{"never adopted, interest in the future", cache.SuiteFreshness{LastSuccessAt: stale, LastRequestedAt: at(time.Hour)}, true, true},
		{"never adopted, interest 8 days in the future", cache.SuiteFreshness{LastSuccessAt: stale, LastRequestedAt: at(8 * 24 * time.Hour)}, false, true},
		{"adopted, no interest", cache.SuiteFreshness{LastSuccessAt: stale, CurrentSnapshotID: &id}, true, true},
	}
	for _, tc := range cases {
		if got := withAdopter.suiteDue(tc.s, now); got != tc.want {
			t.Errorf("%s: suiteDue = %v, want %v", tc.name, got, tc.want)
		}
		if got := phase1.suiteDue(tc.s, now); got != tc.wantPhase1 {
			t.Errorf("%s (no adopter): suiteDue = %v, want %v", tc.name, got, tc.wantPhase1)
		}
	}
}

// Spec C7 in the probe tree: the probes carry no validators, so the gate
// compares content keys — the failing content is deferred until its retry
// time, new content is attempted at its first probe. A deferred suite is
// probed only by the scheduler's refresh check and at its retry (ruling
// PE-3), so each step comes a refresh interval after the last check, and a
// long retry time is set directly, as a long backoff would.
func TestCheckFirst_GateComparesContentKeys(t *testing.T) {
	r := newRetryRig(t, 0)
	bad := r.release("bad", 0)
	r.ver.reject(hashOf(bad))
	r.up.set(bad)
	r.check()
	if r.ver.calls() != 1 || r.row().AdoptionFailCount != 1 {
		t.Fatalf("setup: calls %d count %d", r.ver.calls(), r.row().AdoptionFailCount)
	}
	retryIn := func(d time.Duration) {
		t.Helper()
		if err := r.cache.SetAdoptionNextRetry(context.Background(), "http", rigHost, rigSuite, r.clock.now().Add(d).Unix()); err != nil {
			t.Fatal(err)
		}
	}
	retryIn(time.Hour)
	r.clock.advance(16 * time.Minute)
	r.check()
	if r.ver.calls() != 1 || lastResult(r) != "changed_deferred" {
		t.Fatalf("same content before its retry: calls %d result %q, want 1 changed_deferred", r.ver.calls(), lastResult(r))
	}
	if ev := r.events("freshness_check"); ev[len(ev)-1]["reason"] != "gpg_failed" {
		t.Fatalf("reason = %v, want gpg_failed", ev[len(ev)-1]["reason"])
	}
	for _, e := range r.up.etagsSent(rigAnchorURL) {
		if e != "" {
			t.Fatalf("a probe carried validator %q", e)
		}
	}
	r.clock.advance(45 * time.Minute) // past the retry time
	r.check()
	if r.ver.calls() != 2 {
		t.Fatalf("at the retry time: calls %d, want 2", r.ver.calls())
	}
	retryIn(time.Hour)
	r.up.set(r.release("fixed", 0))
	r.clock.advance(16 * time.Minute)
	r.check()
	if r.ver.calls() != 3 || r.currentSnapshot() == nil {
		t.Fatalf("new content: calls %d, adopted %v; want it attempted at its first probe and adopted", r.ver.calls(), r.currentSnapshot() != nil)
	}
}

// unsignedRelease serves an unsigned upstream to the rig: no InRelease
// (404), the Release text, and Release.gpg failing with sigErr (nil: 404).
func (r *retryRig) unsignedRelease(label string, sigErr error) []byte {
	rel := r.releaseText(label, 0)
	r.up.setAt(rigReleaseURL, rel)
	r.up.setFail(rigReleaseGPGURL, sigErr)
	return rel
}

// Spec C5 safeguard 2: the transition into unsigned needs two observations
// of the same Release bytes at least max(cooldown, 5 min) apart; the first
// records unsigned_unconfirmed, the streak (and adoption_fail_first_at)
// restarts when the bytes change, and the adoption is the unsigned form.
func TestCheckFirst_UnsignedTransitionIsConfirmed(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopter.adoptUnsigned = true
	t0 := r.clock.now().Unix()
	r.unsignedRelease("u1", nil)
	r.check()
	row := r.row()
	if lastResult(r) != "changed_deferred" || strOf(row.AdoptionFailReason) != cache.ReasonUnsignedUnconfirmed ||
		row.AdoptionFailCount != 1 || i64Of(row.AdoptionFailFirstAt) != t0 {
		t.Fatalf("first observation: result %q record %q/%d/%d", lastResult(r), strOf(row.AdoptionFailReason), row.AdoptionFailCount, i64Of(row.AdoptionFailFirstAt))
	}
	if ev := r.events("freshness_check"); ev[len(ev)-1]["reason"] != cache.ReasonUnsignedUnconfirmed || ev[len(ev)-1]["form"] != "unsigned" {
		t.Fatalf("freshness_check %v, want reason unsigned_unconfirmed and form unsigned", ev[len(ev)-1])
	}
	r.clock.advance(time.Minute)
	lines := len(r.events("freshness_check"))
	r.check() // before the record's retry time: paced (ruling PE-3), nothing probed or recorded
	if row := r.row(); row.AdoptionFailCount != 1 || lastResult(r) != "changed_deferred" || len(r.events("freshness_check")) != lines {
		t.Fatalf("inside the retry time: count %d result %q, %d new lines", row.AdoptionFailCount, lastResult(r),
			len(r.events("freshness_check"))-lines)
	}
	// New bytes restart the confirmation stamp, not the retry count (final
	// fix wave FW-3 (a), design note 18 as amended: restarting the count
	// too kept bytes that change at every observation at a 2-minute retry
	// forever). This test pinned "count 1" here before the ruling.
	r.unsignedRelease("u2", nil)
	r.clock.advance(time.Minute)
	t2 := r.clock.now().Unix()
	r.check()
	if row := r.row(); row.AdoptionFailCount != 2 || i64Of(row.AdoptionFailFirstAt) != t2 {
		t.Fatalf("new bytes: count %d first %d, want the count growing (2) and a new confirmation stamp at %d",
			row.AdoptionFailCount, i64Of(row.AdoptionFailFirstAt), t2)
	}
	r.clock.advance(4 * time.Minute) // due (the count-2 retry), but only 4 min after the first observation
	r.check()
	if row := r.row(); row.CurrentSnapshotID != nil || row.AdoptionFailCount != 3 || i64Of(row.AdoptionFailFirstAt) != t2 {
		t.Fatalf("4 min apart: adopted %v count %d first %d; want unconfirmed, count 3, the same first", row.CurrentSnapshotID != nil,
			row.AdoptionFailCount, i64Of(row.AdoptionFailFirstAt))
	}
	r.clock.advance(8 * time.Minute) // due again, 12 min after the first observation
	r.check()
	snap := r.currentSnapshot()
	if snap == nil || snap.Form != cache.FormUnsigned || lastResult(r) != "first_adoption" {
		t.Fatalf("confirmed: snapshot %+v result %q, want an unsigned first adoption", snap, lastResult(r))
	}
	if r.ver.calls() != 0 {
		t.Fatalf("verifier calls = %d for an unsigned adoption, want 0", r.ver.calls())
	}
}

// Spec C5 safeguard 2: a 401 or 403 on a signature file stretches the gap
// to an hour (WAF and S3-permission incidents).
func TestCheckFirst_UnsignedAfterADeniedSignatureWaitsAnHour(t *testing.T) {
	for _, denied := range []string{"InRelease", "Release.gpg"} {
		t.Run(denied, func(t *testing.T) {
			r := newRetryRig(t, 0)
			r.adopter.adoptUnsigned = true
			var sigErr error
			if denied == "InRelease" {
				r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 403})
			} else {
				sigErr = &fetch.StatusError{Code: 401}
			}
			r.unsignedRelease("u1", sigErr)
			for elapsed := time.Duration(0); elapsed < 59*time.Minute; elapsed += 2 * time.Minute {
				r.check()
				if r.row().CurrentSnapshotID != nil {
					t.Fatalf("adopted %v after the first observation, want an hour", elapsed)
				}
				r.clock.advance(2 * time.Minute)
			}
			r.clock.advance(65 * time.Minute) // past the hour and the record's retry time
			r.check()
			if snap := r.currentSnapshot(); snap == nil || snap.Form != cache.FormUnsigned {
				t.Fatalf("not adopted after the hour: %+v", snap)
			}
		})
	}
}

// Spec C5 safeguard 3 at eligibility: a suite whose base has a signed-seen
// marker is refused before a slot is taken or anything downloaded, and the
// refusal is a recorded state (changed_deferred, unsigned_downgrade_refused).
func TestCheckFirst_UnsignedRefusedWithSignedHistory(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopter.adoptUnsigned = true
	if err := r.cache.MarkSignedSeenBatch(context.Background(), []cache.SignedSeenMark{{CanonicalScheme: "http",
		CanonicalHost: rigHost, Base: rigSuite, SeenAt: r.clock.now().Unix()}}); err != nil {
		t.Fatal(err)
	}
	r.unsignedRelease("u1", nil)
	before := adoptionTotalFor(t, rigHost)
	r.check()
	row := r.row()
	if strOf(row.AdoptionFailReason) != cache.ReasonUnsignedDowngradeRefused || lastResult(r) != "changed_deferred" {
		t.Fatalf("record %q result %q, want unsigned_downgrade_refused and changed_deferred", strOf(row.AdoptionFailReason), lastResult(r))
	}
	if adoptionTotalFor(t, rigHost) != before {
		t.Fatal("an adoption ran for a suite with signed history")
	}
	r.clock.advance(2 * time.Hour) // never confirms
	r.check()
	if r.row().CurrentSnapshotID != nil {
		t.Fatal("an unsigned snapshot was adopted for a suite with signed history")
	}
}

// rigRequests is the number of requests the rig upstream got for the
// suite's three anchor files.
func (r *retryRig) rigRequests() int {
	return len(r.up.etagsSent(rigAnchorURL)) + len(r.up.etagsSent(rigReleaseURL)) + len(r.up.etagsSent(rigReleaseGPGURL))
}

// Ruling PE-12 (final fix wave FW-2): adoption.adopt_unsigned = false
// leaves a never-adopted unsigned repository unadopted as a paced, visible
// refusal state — unsigned_disabled (changed_deferred), never a failed
// check — and its record paces it (ruling PE-3): checks inside the
// record's window ask upstream nothing. Turning the knob back on does not
// skip the transition's confirmation: the refused bytes were never
// confirmed (design note 17's "confirmed once" excludes the refusal).
// This replaces TestCheckFirst_UnsignedDisabledIsAFailedCheck, which
// pinned the failed check PE-12 retires (E1: 30 client checks cost 90
// requests and 30 failed checks, with no record and no visible state).
func TestCheckFirst_UnsignedDisabledIsAPacedRefusal(t *testing.T) {
	r := newRetryRig(t, time.Minute)
	r.unsignedRelease("u1", nil)
	r.check()
	row := r.row()
	if row.CheckFailCount != 0 || row.CurrentSnapshotID != nil || strOf(row.AdoptionFailReason) != cache.ReasonUnsignedDisabled ||
		lastResult(r) != "changed_deferred" {
		t.Fatalf("check_fail_count %d, current %v, record %q, result %q; want the refusal unsigned_disabled and no failed check",
			row.CheckFailCount, row.CurrentSnapshotID != nil, strOf(row.AdoptionFailReason), lastResult(r))
	}
	if ev := r.events("freshness check failed"); len(ev) != 0 {
		t.Fatalf("failed-check lines %v, want none", ev)
	}
	if ev := r.events("adoption_refused"); len(ev) != 1 || ev[0]["reason"] != cache.ReasonUnsignedDisabled {
		t.Fatalf("adoption_refused lines %v, want one with reason unsigned_disabled", ev)
	}
	if !cache.IsRefusalReason(cache.ReasonUnsignedDisabled) {
		t.Fatal("unsigned_disabled is not a refusal reason")
	}
	before := r.rigRequests()
	r.clock.advance(61 * time.Second) // past the cooldown, inside the record's 2-minute window
	r.check()
	if n := r.rigRequests() - before; n != 0 {
		t.Fatalf("a client check inside the record's window made %d requests, want 0", n)
	}
	r.clock.advance(time.Minute) // the record is due: the tree runs and is refused again
	r.check()
	row = r.row()
	if r.rigRequests() == before || row.AdoptionFailCount != 2 || strOf(row.AdoptionFailReason) != cache.ReasonUnsignedDisabled ||
		row.CheckFailCount != 0 || row.CurrentSnapshotID != nil {
		t.Fatalf("at the retry: %d requests, record %q/%d, check_fail_count %d, adopted %v; want the tree and the refusal again",
			r.rigRequests()-before, strOf(row.AdoptionFailReason), row.AdoptionFailCount, row.CheckFailCount, row.CurrentSnapshotID != nil)
	}
	// The knob turned back on (a restart): at the next retry the same bytes
	// start the confirmation, they are not taken as confirmed.
	r.adopter.adoptUnsigned = true
	r.clock.advance(4 * time.Minute)
	r.check()
	if row := r.row(); row.CurrentSnapshotID != nil || strOf(row.AdoptionFailReason) != cache.ReasonUnsignedUnconfirmed {
		t.Fatalf("knob on: adopted %v, record %q; want unsigned_unconfirmed (refused bytes were never confirmed)",
			row.CurrentSnapshotID != nil, strOf(row.AdoptionFailReason))
	}
}

// Deviation from the C4 clock-step rule (design note): an
// adoption_fail_first_at in the future never confirms an unsigned
// transition; the next observation restarts the streak.
func TestCheckFirst_FutureConfirmationStartNeverConfirms(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopter.adoptUnsigned = true
	rel := r.unsignedRelease("u1", nil)
	now := r.clock.now()
	if err := r.cache.RecordAdoptionFailure(context.Background(), "http", rigHost, rigSuite, cache.AdoptionFailure{
		Hash: contentKey(adoptionFormUnsigned, nil, rel, nil), Reason: cache.ReasonUnsignedUnconfirmed, Count: 1,
		FirstAt: now.Add(2 * time.Hour).Unix(), LastAt: now.Unix(), NextRetryAt: now.Unix()}); err != nil {
		t.Fatal(err)
	}
	r.check()
	row := r.row()
	if row.CurrentSnapshotID != nil {
		t.Fatal("a future streak start confirmed the unsigned transition")
	}
	if i64Of(row.AdoptionFailFirstAt) != now.Unix() {
		t.Fatalf("first_at = %d, want the streak restarted at %d", i64Of(row.AdoptionFailFirstAt), now.Unix())
	}
}

// isQueuedResult: the scheduler parks first adoptions that found no slot.
func TestIsQueuedResult_FirstAdoption(t *testing.T) {
	for result, want := range map[string]bool{"changed_queued": true, "first_adoption_queued": true,
		"first_adoption": false, "changed": false, "changed_deferred": false} {
		if got := isQueuedResult(result); got != want {
			t.Errorf("isQueuedResult(%q) = %v, want %v", result, got, want)
		}
	}
}

// Ruling PE-3 (design review I-4): a never-adopted suite whose record is
// active and not due is probed only by the scheduler's refresh check (its
// last check at least periodic_refresh old) and at its due retry — a
// client check (T1) inside that window asks nothing, so a deferred suite
// no longer downloads its anchor in full at every client hit. A last check
// stamped in the future reads as infinitely old (clock-step rule).
func TestCheckFirst_DeferredSuiteIgnoresClientChecksUntilItsRetry(t *testing.T) {
	r := newRetryRig(t, 0)
	bad := r.release("bad", 0)
	r.ver.reject(hashOf(bad))
	r.up.set(bad)
	r.check() // the attempt fails: a record due in 2 minutes
	probes := func() int { return len(r.up.etagsSent(rigAnchorURL)) }
	if probes() != 1 || r.row().AdoptionFailCount != 1 {
		t.Fatalf("setup: %d probes, record count %d", probes(), r.row().AdoptionFailCount)
	}
	lines := len(r.events("freshness_check"))
	for range 10 { // client checks inside the window
		r.clock.advance(10 * time.Second)
		r.check()
	}
	if probes() != 1 || len(r.events("freshness_check")) != lines || r.ver.calls() != 1 {
		t.Fatalf("client checks inside the window: %d probes, %d lines, %d attempts; want none",
			probes()-1, len(r.events("freshness_check"))-lines, r.ver.calls()-1)
	}
	r.clock.advance(time.Minute + time.Second) // the retry is due
	r.check()
	if probes() != 2 || r.ver.calls() != 2 {
		t.Fatalf("the due retry: %d probes, %d attempts; want one tree and one attempt", probes()-1, r.ver.calls()-1)
	}
	// A long retry time (as a long backoff would set), then the scheduler's
	// refresh check: the last check is periodic_refresh (15 min) old.
	if err := r.cache.SetAdoptionNextRetry(context.Background(), "http", rigHost, rigSuite, r.clock.now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	r.clock.advance(14 * time.Minute)
	r.check()
	if probes() != 2 {
		t.Fatal("a check 14 minutes after the last one probed a deferred suite")
	}
	r.clock.advance(time.Minute)
	r.check()
	if probes() != 3 || lastResult(r) != "changed_deferred" {
		t.Fatalf("the refresh check: %d probes, result %q; want one more tree, changed_deferred", probes()-2, lastResult(r))
	}
	future := r.clock.now().Add(24 * time.Hour).Unix()
	row := r.row()
	row.LastCheckAt = &future
	if err := r.cache.UpdateSuiteFreshness(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	r.clock.advance(time.Minute)
	r.check()
	if probes() != 4 {
		t.Fatal("a deferred suite whose last check lies in the future was not probed")
	}
}

// Ruling PE-3's pacing exempts the scheduler's slot-driven re-offer (review
// fix round 1, Task 6). A deferred never-adopted suite whose refresh check
// found new content while no slot was free is parked; when a slot frees,
// the scheduler re-offers it (a waiting item, cooldown bypassed) seconds
// after that check, with its record still active and not due. Pacing that
// re-offer ended it skipped and dropped the suite from the waiting list,
// so admission stopped being work-conserving. The re-offer probes and
// admits; a client check at the same moment is still paced.
func TestCheckFirst_ReofferOfAParkedFirstAdoptionIsNotPaced(t *testing.T) {
	r := newRetryRig(t, 0)
	ctx := context.Background()
	ref := SuiteRef{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite}
	bad := r.release("bad", 0)
	r.ver.reject(hashOf(bad))
	r.up.set(bad)
	r.check() // the attempt fails: a record
	if r.row().AdoptionFailCount != 1 {
		t.Fatalf("setup: record count %d, want 1", r.row().AdoptionFailCount)
	}
	if err := r.cache.SetAdoptionNextRetry(ctx, "http", rigHost, rigSuite, r.clock.now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	probes := func() int { return len(r.up.etagsSent(rigAnchorURL)) }
	r.clock.advance(15 * time.Minute) // the scheduler's refresh check
	r.up.set(r.release("new", 0))
	held := mustAcquire(t, r.checker.admission, rigHost, true)
	if rep := r.checker.checkRecovered(ctx, schedItem{ref: ref}); rep.result != "first_adoption_queued" {
		t.Fatalf("refresh check with no slot: %q, want first_adoption_queued", rep.result)
	}
	before := probes()
	held.release()
	r.clock.advance(30 * time.Second) // a slot frees
	r.check()                         // a client check inside the window: still paced
	if got := probes() - before; got != 0 || r.row().CurrentSnapshotID != nil {
		t.Fatalf("a client check of the parked suite: %d probes, adopted %v; want it paced", got, r.row().CurrentSnapshotID != nil)
	}
	rep := r.checker.checkRecovered(ctx, schedItem{ref: ref, waiting: true}) // the re-offer
	r.checker.WaitForAdoptions()
	if rep.result != "first_adoption" || probes()-before != 1 || r.row().CurrentSnapshotID == nil {
		t.Fatalf("the re-offer: result %q, %d probes, adopted %v; want first_adoption, one probe tree, adopted",
			rep.result, probes()-before, r.row().CurrentSnapshotID != nil)
	}
}

// cancelAfter wraps the rig upstream: once url has been answered it
// cancels cancel — a shutdown landing right after that probe.
type cancelAfter struct {
	*rigUpstream
	url    string
	cancel context.CancelFunc
}

func (u *cancelAfter) Conditional(ctx context.Context, target *fetch.Target, etag, lastmod string, limit int64) (*fetch.ConditionalResult, error) {
	res, err := u.rigUpstream.Conditional(ctx, target, etag, lastmod, limit)
	if target.URL == u.url {
		u.cancel()
	}
	return res, err
}

// Final fix wave FW-3 (d) (final concurrency review M-1, its E8): a
// shutdown that lands between the probe that found a signature and the
// marker write ends the check quietly — DEBUG, no failed check, no
// failed-check line or count, no persist WARN — as a cancelled GET does: a
// cancelled context says nothing about upstream.
func TestCheckFirst_ShutdownAtTheMarkerWriteIsQuiet(t *testing.T) {
	r := newRetryRig(t, 0)
	r.up.set(r.release("v1", 0))
	ctx, cancel := context.WithCancel(context.Background())
	r.checker.fetcher = &cancelAfter{rigUpstream: r.up, url: rigAnchorURL, cancel: cancel}
	before := checkTotal(t, "failed", rigHost)
	r.checker.Check(ctx, "http", rigHost, rigSuite)
	r.checker.WaitForAdoptions()
	for _, msg := range []string{"freshness check failed", "freshness: persist failure-bump failed", "freshness_check"} {
		if ev := r.events(msg); len(ev) != 0 {
			t.Fatalf("%q lines %v after a shutdown, want none", msg, ev)
		}
	}
	if n := checkTotal(t, "failed", rigHost) - before; n != 0 {
		t.Fatalf("failed checks counted %v, want 0", n)
	}
	if row := r.row(); row.CheckFailCount != 0 || row.LastCheckAt != nil || row.CurrentSnapshotID != nil {
		t.Fatalf("row %+v, want nothing written", row)
	}
	ev := r.events("freshness_check_ended_by_shutdown")
	if len(ev) != 1 || ev[0]["level"] != "DEBUG" || ev[0]["step"] != "mark the signature seen" {
		t.Fatalf("shutdown lines %v, want one DEBUG line naming the marker write", ev)
	}
}

// Spec C3 revision 8 (ruling PE-5, design review I-2): a probe that finds
// a signature — InRelease, or Release.gpg — for a suite without a signed
// current snapshot marks the suite's base, stamped now, before anything is
// admitted, even when the signed adoption then fails; so stripping the
// signatures later (an upstream, or a man-in-the-middle, answering 404)
// never makes the suite an unsigned candidate. A marker write that fails
// fails the check, and nothing is attempted.
func TestCheckFirst_ProbedSignatureMarksTheBase(t *testing.T) {
	for _, form := range []string{"inline", "detached"} {
		t.Run(form, func(t *testing.T) {
			r := newRetryRig(t, 0)
			r.adopter.adoptUnsigned = true
			if form == "inline" {
				signed := r.release("signed", 0)
				r.ver.reject(hashOf(signed))
				r.up.set(signed)
			} else {
				rel, sig := r.releaseText("signed", 0), armoredSig("its signature")
				r.ver.reject(contentKey(adoptionFormDetached, nil, rel, sig))
				r.up.setAt(rigReleaseURL, rel)
				r.up.setAt(rigReleaseGPGURL, sig)
			}
			r.check()
			if r.currentSnapshot() != nil || r.ver.calls() != 1 {
				t.Fatalf("setup: the signed attempt must fail (verifier calls %d)", r.ver.calls())
			}
			seen, ok, err := r.cache.SignedSeen(context.Background(), "http", rigHost, rigSuite)
			if err != nil || !ok || seen != r.clock.now().Unix() {
				t.Fatalf("marker = %d, %v, %v; want the probe's sighting at %d", seen, ok, err, r.clock.now().Unix())
			}
			// The signatures are stripped (both 404); the Release text stays.
			r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 404})
			r.unsignedRelease("stripped", &fetch.StatusError{Code: 404})
			r.clock.advance(time.Hour)
			r.check()
			if row := r.row(); strOf(row.AdoptionFailReason) != cache.ReasonUnsignedDowngradeRefused || row.CurrentSnapshotID != nil {
				t.Fatalf("record %q, current %v; want unsigned_downgrade_refused and nothing adopted",
					strOf(row.AdoptionFailReason), row.CurrentSnapshotID)
			}
		})
	}
	t.Run("failed marker write", func(t *testing.T) {
		r := newRetryRig(t, 0)
		r.up.set(r.release("signed", 0))
		db, err := sql.Open("sqlite", filepath.Join(r.cache.Dir(), "cache.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		if _, err := db.Exec(`CREATE TRIGGER no_marks BEFORE INSERT ON suite_signed_seen
  BEGIN SELECT RAISE(ABORT, 'marker writes fail'); END`); err != nil {
			t.Fatal(err)
		}
		r.check()
		if row := r.row(); row.CheckFailCount != 1 || row.CurrentSnapshotID != nil || r.ver.calls() != 0 {
			t.Fatalf("row %+v, verifier calls %d: want a failed check and no attempt", row, r.ver.calls())
		}
	})
}

// Design review M-5 (spec C5 safeguard 4, before a slot): an unsigned
// Release declaring more than unsignedMemberCap members is refused at the
// check — unsigned_too_large recorded at its first observation, no
// confirmation, no slot, nothing fetched — and stays refused at its retry.
func TestCheckFirst_UnsignedTooLargeNeverTakesASlot(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopter.adoptUnsigned = true
	big := make(map[string][]byte, unsignedMemberCap+1)
	for i := 0; i <= unsignedMemberCap; i++ {
		big[fmt.Sprintf("main/i18n/Translation-%d", i)] = []byte{byte(i)}
	}
	r.up.setAt(rigReleaseURL, datedRelease("big", 0, big))
	before := adoptionTotalFor(t, rigHost)
	r.check()
	if row := r.row(); strOf(row.AdoptionFailReason) != cache.ReasonUnsignedTooLarge || lastResult(r) != "changed_deferred" {
		t.Fatalf("first observation: record %q result %q, want unsigned_too_large, changed_deferred",
			strOf(row.AdoptionFailReason), lastResult(r))
	}
	r.clock.advance(time.Hour)
	r.check()
	if row := r.row(); row.CurrentSnapshotID != nil || strOf(row.AdoptionFailReason) != cache.ReasonUnsignedTooLarge || row.AdoptionFailCount != 2 {
		t.Fatalf("at the retry: current %v record %q/%d; want still refused, count 2",
			row.CurrentSnapshotID, strOf(row.AdoptionFailReason), row.AdoptionFailCount)
	}
	if adoptionTotalFor(t, rigHost) != before || len(r.members.requested()) != 0 {
		t.Fatal("a too-large unsigned suite ran an adoption")
	}
}

// Spec C4 "Response classification": an unknown answer on Release.gpg (a
// 503) is a failed check — never "absent", so never an unsigned candidate —
// also with adoption.adopt_unsigned on. TestCheckFirst_ClassificationPerAnchor
// runs with the knob off, which turns a wrongly unsigned candidate into a
// failed check as well and hides the mix-up. (Proposed by the executability
// review; kills X01.)
func TestCheckFirst_UnknownSignatureIsNeverAnUnsignedCandidate(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopter.adoptUnsigned = true
	r.unsignedRelease("u1", &fetch.StatusError{Code: 503})
	before := checkTotal(t, "failed", rigHost)
	r.check()
	row := r.row()
	if row.CheckFailCount != 1 || row.CurrentSnapshotID != nil || row.AdoptionFailCount != 0 {
		t.Fatalf("row %+v, want a failed check: streak 1, no record, nothing adopted", row)
	}
	if got := checkTotal(t, "failed", rigHost) - before; got != 1 {
		t.Fatalf("failed checks counted %v, want 1", got)
	}
}

// Review Focus 2 / spec C3: a soft-404 (a 200 HTML page) on Release.gpg is
// absent, never taken for a signature: with Release present the tree finds
// an unsigned candidate, not the detached form. (Proposed by the
// executability review; kills X04.)
func TestCheckFirst_HTMLReleaseGPGIsNoSignature(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopter.adoptUnsigned = true
	r.unsignedRelease("u1", nil)
	r.up.setAt(rigReleaseGPGURL, []byte("<html><body>Not Found</body></html>\n"))
	r.check()
	row := r.row()
	if row.CurrentSnapshotID != nil || strOf(row.AdoptionFailReason) != cache.ReasonUnsignedUnconfirmed || r.ver.calls() != 0 {
		t.Fatalf("current %v, record %q, verifier calls %d: want the unsigned candidate's first observation, no detached attempt",
			row.CurrentSnapshotID, strOf(row.AdoptionFailReason), r.ver.calls())
	}
}

// Design note 14: a client-created Release.gpg row alone does not make a
// never-adopted suite active (apt fetches it only after Release): with its
// InRelease row reconstructed (request_count 0) the suite stays dormant.
// (Proposed by the executability review; kills X12.)
func TestCheckFirst_ReleaseGPGRowAloneIsNotClientInterest(t *testing.T) {
	r := newRetryRig(t, 0)
	r.up.set(r.release("v1", 0))
	ctx := context.Background()
	stale := testHex64
	sig, err := r.adopter.writeBlobBytes(ctx, armoredSig("a signature"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.cache.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: rigHost,
		Path: rigSuite + "/InRelease", BlobHash: &stale, UpstreamURL: rigAnchorURL, IsMetadata: true}); err != nil {
		t.Fatal(err)
	}
	if err := r.cache.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: rigHost,
		Path: rigSuite + "/Release.gpg", BlobHash: &sig, UpstreamURL: rigReleaseGPGURL, IsMetadata: true, RequestCount: 1}); err != nil {
		t.Fatal(err)
	}
	r.check()
	if n := len(r.up.etagsSent(rigAnchorURL)); n != 0 {
		t.Fatalf("%d requests for a suite whose only client-created anchor row is Release.gpg, want 0", n)
	}
}

// Final fix wave FW-6 (final tests review P14; the Task 6 reviewer's killer
// test): bytes refused as unsigned_downgrade_refused were never confirmed,
// so once the signed history is gone (phase F's orphan-marker sweep,
// POST /forget) the same bytes still need the two-observation
// confirmation (C5 safeguard 2): the "confirmed once" rule excludes the
// refusal (design note 17).
func TestCheckFirst_RefusedBytesAreConfirmedOnceTheMarkerIsGone(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopter.adoptUnsigned = true
	ctx := context.Background()
	if err := r.cache.MarkSignedSeenBatch(ctx, []cache.SignedSeenMark{{CanonicalScheme: "http",
		CanonicalHost: rigHost, Base: rigSuite, SeenAt: r.clock.now().Unix()}}); err != nil {
		t.Fatal(err)
	}
	r.unsignedRelease("u1", nil)
	r.check()
	if got := strOf(r.row().AdoptionFailReason); got != cache.ReasonUnsignedDowngradeRefused {
		t.Fatalf("setup: record %q, want unsigned_downgrade_refused", got)
	}
	if err := r.cache.ClearSignedSeenForTest(ctx); err != nil {
		t.Fatal(err)
	}
	r.clock.advance(3 * time.Minute) // the refusal's retry is due
	r.check()
	row := r.row()
	if row.CurrentSnapshotID != nil || strOf(row.AdoptionFailReason) != cache.ReasonUnsignedUnconfirmed {
		t.Fatalf("after the marker is gone: adopted %v, record %q; want a first observation (unsigned_unconfirmed)",
			row.CurrentSnapshotID != nil, strOf(row.AdoptionFailReason))
	}
}

// Design note 17 ("confirmed once"): bytes that passed the confirmation and
// then failed their adoption attempt are not confirmed again — at the
// record's retry time the attempt runs at once. (Proposed by the
// executability review; kills X13.)
func TestCheckFirst_ConfirmedUnsignedContentIsNotReconfirmed(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopter.adoptUnsigned = true
	r.members.put(rigPkgsURL, []byte("not the declared Packages"))
	r.unsignedRelease("u1", nil)
	r.check() // first observation: unsigned_unconfirmed
	r.clock.advance(6 * time.Minute)
	r.check() // confirmed; the attempt fails on the member
	row := r.row()
	if row.CurrentSnapshotID != nil || strOf(row.AdoptionFailReason) == cache.ReasonUnsignedUnconfirmed {
		t.Fatalf("setup: current %v record %q, want a failed attempt recorded", row.CurrentSnapshotID, strOf(row.AdoptionFailReason))
	}
	r.members.put(rigPkgsURL, r.pkgs) // the mirror synced
	r.clock.advance(time.Hour)        // past the attempt's retry time
	r.check()
	if snap := r.currentSnapshot(); snap == nil || snap.Form != cache.FormUnsigned {
		t.Fatalf("at the retry time: snapshot %+v, record %q; want the unsigned adoption at once",
			snap, strOf(r.row().AdoptionFailReason))
	}
}

// lookupFailCache fails LookupURL while fail is set.
type lookupFailCache struct {
	Cache
	fail atomic.Bool
}

func (c *lookupFailCache) LookupURL(ctx context.Context, scheme, host, path string) (*cache.URLPath, error) {
	if c.fail.Load() {
		return nil, fmt.Errorf("injected url_path read failure (%s)", path)
	}
	return c.Cache.LookupURL(ctx, scheme, host, path)
}

// Deferred minor E-26 (phase E design note 35): a never-adopted suite whose
// anchor rows cannot be read is logged at WARN and left alone — no
// request, no write, no backoff (a local fault is no failed check) — and
// the next check, the fault gone, adopts it.
func TestCheckFirst_AnchorRowReadErrorWritesNothing(t *testing.T) {
	r := newRetryRig(t, 0)
	r.up.set(r.release("v1", 0))
	fc := &lookupFailCache{Cache: r.cache}
	fc.fail.Store(true)
	var logs lockedBuffer
	chk, err := New(Config{Cache: fc, Fetcher: r.up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute,
		Adopter: r.adopter, Logger: slog.New(slog.NewJSONHandler(&logs, nil)), now: r.clock.now, rand: func() float64 { return 0.5 }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	before := r.row() // the whole row: no column may change (deferred minor F-8)
	chk.Check(ctx, "http", rigHost, rigSuite)
	chk.WaitForAdoptions()
	warns := logRecords(t, &logs, "freshness_read_anchor_rows_failed")
	if len(warns) != 1 || warns[0]["level"] != "WARN" || warns[0]["suite_path"] != rigSuite ||
		!strings.Contains(fmt.Sprint(warns[0]["err"]), "injected url_path read failure") {
		t.Fatalf("WARN records %v, want one naming the suite and the read error", warns)
	}
	for _, url := range []string{rigAnchorURL, rigReleaseURL, rigReleaseGPGURL} {
		if n := len(r.up.etagsSent(url)); n != 0 {
			t.Fatalf("%d upstream requests to %s, want none", n, url)
		}
	}
	if row := r.row(); !reflect.DeepEqual(row, before) {
		t.Fatalf("the read error wrote the row: %+v, want it unchanged: %+v", row, before)
	}
	fc.fail.Store(false)
	chk.Check(ctx, "http", rigHost, rigSuite)
	chk.WaitForAdoptions()
	if r.currentSnapshot() == nil {
		t.Fatal("the next check, the fault gone, did not adopt")
	}
}
