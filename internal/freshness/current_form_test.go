package freshness

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// adoptUnsigned makes the rig suite's current snapshot an unsigned
// Release labelled label (adopted directly; the check-side confirmation
// is TestCheckFirst_UnsignedTransitionIsConfirmed's) and serves it with
// no signature files.
func (r *retryRig) adoptUnsigned(label string) []byte {
	r.t.Helper()
	r.adopter.adoptUnsigned = true
	rel := r.unsignedRelease(label, nil)
	if err := r.adopter.RunUnsigned(context.Background(), SuiteRef{CanonicalScheme: "http", CanonicalHost: rigHost,
		SuitePath: rigSuite}, rel, "", ""); err != nil {
		r.t.Fatal(err)
	}
	if snap := r.currentSnapshot(); snap == nil || snap.Form != cache.FormUnsigned {
		r.t.Fatalf("rig: unsigned %s not adopted: %+v", label, snap)
	}
	return rel
}

// Spec C4 Dispatch 1 (lateral): a detached suite probes InRelease on every
// check and moves to inline as soon as one is served.
func TestCheck_DetachedSuiteMovesToInlineWhenInReleaseAppears(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adoptDetached()
	r.check() // InRelease absent: the detached check stands
	if lastResult(r) != "not_modified" && lastResult(r) != "unchanged" {
		t.Fatalf("result %q, want the detached suite unchanged", lastResult(r))
	}
	if n := len(r.up.etagsSent(rigAnchorURL)); n != 2 {
		t.Fatalf("InRelease probed %d times over two checks, want once per check", n)
	}
	in := r.release("v2", 0)
	r.up.set(in)
	r.check()
	snap := r.currentSnapshot()
	if lastResult(r) != "form_changed" || snap.Form != cache.FormInline || strOf(snap.SingleAnchorHash) != hashOf(in) {
		t.Fatalf("result %q snapshot %+v, want form_changed to the inline v2", lastResult(r), snap)
	}
}

// Spec C7 in the lateral probe: an InRelease that failed stays deferred
// until its retry time, and the detached anchor's "unchanged" does not
// clear its record (otherwise every other check would attempt it). The
// record holds no validators (a cross-form request), so the detached GET
// carries the current Release's own, and its 304 is not_modified (design
// review I-1).
func TestCheck_LateralFailingContentKeepsItsRecord(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adoptDetached()
	bad := r.release("bad", 0)
	r.ver.reject(hashOf(bad))
	r.up.set(bad)
	r.check()
	if r.ver.calls() != 1 || r.row().AdoptionFailCount != 1 {
		t.Fatalf("setup: calls %d count %d", r.ver.calls(), r.row().AdoptionFailCount)
	}
	for i := 0; i < 3; i++ {
		r.clock.advance(20 * time.Second)
		r.check()
	}
	if r.ver.calls() != 1 || r.row().AdoptionFailCount != 1 {
		t.Fatalf("before the retry time: calls %d count %d, want 1 and the record kept", r.ver.calls(), r.row().AdoptionFailCount)
	}
	if lastResult(r) != "not_modified" {
		t.Fatalf("result %q, want not_modified: the detached anchor answered its own validators", lastResult(r))
	}
	r.clock.advance(2 * time.Minute)
	r.check()
	if r.ver.calls() != 2 {
		t.Fatalf("at the retry time: calls %d, want 2", r.ver.calls())
	}
}

// Spec C4 Dispatch 1: an absent current anchor is confirmed by two
// observations at least max(cooldown, 5 min) apart, then the probe tree
// adopts the other signed form (lateral). Each absent observation is an
// answer: the streak stays 0 (ruling R-E4), last_success_at stays old and
// the state is visible as anchor_absent.
func TestCheck_AbsentAnchorMovesLaterallyAfterTwoObservations(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopt()
	adopted := r.row().LastSuccessAt
	r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 503}) // an unknown end first: streak 1
	r.clock.advance(time.Minute)
	r.check()
	if r.row().CheckFailCount != 1 {
		t.Fatalf("setup: streak %d, want 1", r.row().CheckFailCount)
	}
	r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 404})
	r.up.setAt(rigReleaseURL, r.releaseText("v2", 0))
	r.up.setAt(rigReleaseGPGURL, armoredSig("v2 sig"))
	r.clock.advance(time.Minute)
	r.check()
	row := r.row()
	if lastResult(r) != "anchor_absent" || strOf(row.AdoptionFailReason) != cache.ReasonAnchorAbsent || row.AdoptionFailCount != 1 {
		t.Fatalf("first observation: result %q record %q/%d", lastResult(r), strOf(row.AdoptionFailReason), row.AdoptionFailCount)
	}
	if row.CheckFailCount != 0 || i64Of(row.LastSuccessAt) != i64Of(adopted) || row.LastCheckAt == nil {
		t.Fatalf("row %+v: want streak 0, last_success_at kept, last_check_at bumped", row)
	}
	if ev := r.events("freshness_check"); ev[len(ev)-1]["reason"] != cache.ReasonAnchorAbsent || ev[len(ev)-1]["upstream_status"] != float64(404) {
		t.Fatalf("freshness_check %v, want reason anchor_absent and upstream_status 404", ev[len(ev)-1])
	}
	r.clock.advance(time.Minute)
	r.check() // inside the record's retry time
	if n := len(r.up.etagsSent(rigReleaseURL)); n != 0 || r.row().AdoptionFailCount != 1 {
		t.Fatalf("before the retry time: %d Release probes, count %d; want none and 1", n, r.row().AdoptionFailCount)
	}
	r.clock.advance(time.Minute) // due, 2 min after the first observation
	r.check()
	if n := len(r.up.etagsSent(rigReleaseURL)); n != 0 || r.row().AdoptionFailCount != 2 {
		t.Fatalf("2 min apart: %d Release probes, count %d; want none and 2", n, r.row().AdoptionFailCount)
	}
	r.clock.advance(4 * time.Minute) // due, 6 min after the first observation
	r.check()
	snap := r.currentSnapshot()
	if lastResult(r) != "form_changed" || snap.Form != cache.FormDetached {
		t.Fatalf("confirmed: result %q snapshot %+v, want form_changed to detached", lastResult(r), snap)
	}
	if row := r.row(); row.AdoptionFailCount != 0 {
		t.Fatalf("record %q/%d left after the lateral move", strOf(row.AdoptionFailReason), row.AdoptionFailCount)
	}
}

// Spec C4: never unsigned from signed. A signed suite whose signatures
// all disappear keeps its snapshot, stays visibly anchor_absent and is
// re-probed on the C7 schedule — even with adoption.adopt_unsigned on.
func TestCheck_AbsentAnchorNeverMovesToUnsigned(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopt()
	r.adopter.adoptUnsigned = true
	before := r.currentSnapshot()
	r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 404})
	r.unsignedRelease("dropped", nil)
	for i := 0; i < 20; i++ {
		r.clock.advance(3 * time.Minute)
		r.check()
	}
	snap := r.currentSnapshot()
	if snap.SnapshotID != before.SnapshotID || snap.Form != cache.FormInline {
		t.Fatalf("snapshot %+v, want the signed one kept", snap)
	}
	if lastResult(r) != "anchor_absent" || strOf(r.row().AdoptionFailReason) != cache.ReasonAnchorAbsent {
		t.Fatalf("result %q record %q, want anchor_absent", lastResult(r), strOf(r.row().AdoptionFailReason))
	}
	if trees := len(r.up.etagsSent(rigReleaseGPGURL)); trees < 2 || trees > 8 {
		t.Fatalf("%d probe trees in an hour, want the C7 schedule's 2..8", trees)
	}
}

// Review Focus 3: a detached repository whose upstream publishes a new
// Release but drops Release.gpg (a vendor that stopped signing, or a
// signature lost in a mirror sync) keeps its signed snapshot and is never
// adopted unsigned — with adoption.adopt_unsigned on and long past the
// unsigned confirmation gap. An absent signature (404) is the refusal
// state anchor_absent, paced by its record (design review M-14: before,
// it cost three requests and a WARN per tick, forever); a signature fetch
// that fails unknown (503) is retried at every check, with a WARN.
func TestCheck_DetachedSuiteThatDropsItsSignatureNeverGoesUnsigned(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		r := newRetryRig(t, 0)
		r.adoptDetached()
		r.adopter.adoptUnsigned = true
		before := r.currentSnapshot()
		r.up.setAt(rigReleaseURL, r.releaseText("v2", 0))
		r.up.setFail(rigReleaseGPGURL, &fetch.StatusError{Code: 404})
		gpgBefore, success := len(r.up.etagsSent(rigReleaseGPGURL)), r.row().LastSuccessAt
		for i := 0; i < 6; i++ {
			r.clock.advance(10 * time.Minute)
			r.check()
		}
		if snap := r.currentSnapshot(); snap.SnapshotID != before.SnapshotID || snap.Form != cache.FormDetached {
			t.Fatalf("snapshot %+v, want the signed detached one kept", snap)
		}
		if lastResult(r) != "anchor_absent" || r.ver.calls() != 0 || strOf(r.row().AdoptionFailReason) != cache.ReasonAnchorAbsent {
			t.Fatalf("result %q, %d verifications, record %q; want anchor_absent, none, anchor_absent", lastResult(r),
				r.ver.calls(), strOf(r.row().AdoptionFailReason))
		}
		// Due at +2, +4, +8, +16 and +32 min after each observation: the
		// check at 50 min is inside the record's retry time.
		if n := len(r.up.etagsSent(rigReleaseGPGURL)) - gpgBefore; n != 5 {
			t.Fatalf("%d signature requests over six checks, want 5 (one check paced)", n)
		}
		if n := len(r.events("freshness: Release.gpg fetch failed; deferring adoption")); n != 0 {
			t.Fatalf("%d WARN lines for an absent signature, want none (a refusal state)", n)
		}
		if got := i64Of(r.row().LastSuccessAt); got != i64Of(success) {
			t.Fatalf("last_success_at moved to %d, want it kept at %d (an absent anchor, spec C4)", got, i64Of(success))
		}
	})
	t.Run("unknown", func(t *testing.T) {
		r := newRetryRig(t, 0)
		r.adoptDetached()
		r.adopter.adoptUnsigned = true
		before := r.currentSnapshot()
		r.up.setAt(rigReleaseURL, r.releaseText("v2", 0))
		r.up.setFail(rigReleaseGPGURL, &fetch.StatusError{Code: 503})
		for i := 0; i < 6; i++ {
			r.clock.advance(10 * time.Minute)
			r.check()
		}
		if snap := r.currentSnapshot(); snap.SnapshotID != before.SnapshotID || snap.Form != cache.FormDetached {
			t.Fatalf("snapshot %+v, want the signed detached one kept", snap)
		}
		if lastResult(r) != "changed" || r.ver.calls() != 0 || r.row().AdoptionFailCount != 0 {
			t.Fatalf("result %q, %d verifications, record %q/%d; want changed, none, none", lastResult(r), r.ver.calls(),
				strOf(r.row().AdoptionFailReason), r.row().AdoptionFailCount)
		}
		if n := len(r.events("freshness: Release.gpg fetch failed; deferring adoption")); n != 6 {
			t.Fatalf("%d WARN lines over six checks, want one per check", n)
		}
	})
}

// Spec C4 "Response classification" on the current anchor: a soft 404
// (an HTML 200) and 401/403/410 are absent; a 5xx is unknown (a failed
// check, the streak extends).
func TestCheck_CurrentAnchorClassification(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(r *retryRig)
		want    string
	}{
		{"HTML 200", func(r *retryRig) { r.up.set([]byte("<html>Not here</html>")) }, "anchor_absent"},
		{"403", func(r *retryRig) { r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 403}) }, "anchor_absent"},
		{"410", func(r *retryRig) { r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 410}) }, "anchor_absent"},
		{"503", func(r *retryRig) { r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 503}) }, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRetryRig(t, 0)
			r.adopt()
			tc.prepare(r)
			r.clock.advance(time.Minute)
			r.check()
			row := r.row()
			switch tc.want {
			case "failed":
				if row.CheckFailCount != 1 || row.AdoptionFailCount != 0 {
					t.Fatalf("row %+v, want a failed check without a record", row)
				}
			default:
				if lastResult(r) != tc.want || row.CheckFailCount != 0 {
					t.Fatalf("result %q streak %d, want %s and 0", lastResult(r), row.CheckFailCount, tc.want)
				}
			}
		})
	}
}

// The probe tree finds the current snapshot's own anchor again (it came
// back between the check's request and the tree): nothing to adopt.
func TestProbeTree_SameAsTheCurrentSnapshot(t *testing.T) {
	in := []byte("-----BEGIN PGP SIGNED MESSAGE-----\nx")
	h := hashOf(in)
	snap := &cache.SuiteSnapshot{Form: cache.FormInline, SingleAnchorHash: &h}
	tree := probeTree{form: adoptionFormInline, found: true, inRelease: probeOutcome{class: classPresent,
		res: &fetch.ConditionalResult{Status: 200, Body: in}}}
	if !tree.sameAs(snap) {
		t.Fatal("the current inline anchor was not recognised")
	}
	other := tree
	other.inRelease.res = &fetch.ConditionalResult{Status: 200, Body: []byte("-----BEGIN PGP SIGNED MESSAGE-----\ny")}
	if other.sameAs(snap) {
		t.Fatal("other content counted as the current anchor")
	}
	if (probeTree{form: adoptionFormDetached, found: true}).sameAs(snap) {
		t.Fatal("another form counted as the current anchor")
	}
}

// Spec C4 Dispatch 2: an unsigned suite upgrades as soon as either
// signature file is served (result form_changed).
func TestCheck_UnsignedSuiteUpgradesWhenSigned(t *testing.T) {
	t.Run("InRelease", func(t *testing.T) {
		r := newRetryRig(t, 0)
		r.adoptUnsigned("u1")
		in := r.release("signed", 0)
		r.up.setFail(rigAnchorURL, nil)
		r.up.set(in)
		r.check()
		if snap := r.currentSnapshot(); lastResult(r) != "form_changed" || snap.Form != cache.FormInline {
			t.Fatalf("result %q snapshot %+v, want form_changed to inline", lastResult(r), snap)
		}
	})
	t.Run("Release.gpg", func(t *testing.T) {
		r := newRetryRig(t, 0)
		rel := r.adoptUnsigned("u1")
		r.up.setFail(rigReleaseGPGURL, nil)
		r.up.setAt(rigReleaseGPGURL, armoredSig("now signed"))
		r.check()
		snap := r.currentSnapshot()
		if lastResult(r) != "form_changed" || snap.Form != cache.FormDetached || strOf(snap.ReleaseHash) != hashOf(rel) {
			t.Fatalf("result %q snapshot %+v, want form_changed to the detached pair", lastResult(r), snap)
		}
	})
	t.Run("an unknown signature probe fails the check", func(t *testing.T) {
		r := newRetryRig(t, 0)
		r.adoptUnsigned("u1")
		r.unsignedRelease("u2", nil)
		r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 503})
		before := r.currentSnapshot().SnapshotID
		r.check()
		if row := r.row(); row.CheckFailCount != 1 || r.currentSnapshot().SnapshotID != before {
			t.Fatalf("row %+v, want a failed check and no unsigned update", row)
		}
	})
}

// Spec C5 safeguard 2: updates of an established unsigned suite adopt at
// once (no confirmation), unless a signed-seen marker appeared for its base
// (never downgrade: refused, recorded as unsigned_downgrade_refused).
func TestCheck_UnsignedSuiteUpdates(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adoptUnsigned("u1")
	u2 := r.unsignedRelease("u2", nil)
	r.check()
	if snap := r.currentSnapshot(); lastResult(r) != "changed" || strOf(snap.SingleAnchorHash) != hashOf(u2) || snap.Form != cache.FormUnsigned {
		t.Fatalf("result %q snapshot %+v, want u2 adopted at once", lastResult(r), snap)
	}
	if err := r.cache.MarkSignedSeenBatch(context.Background(), []cache.SignedSeenMark{{CanonicalScheme: "http",
		CanonicalHost: rigHost, Base: rigSuite, SeenAt: r.clock.now().Unix()}}); err != nil {
		t.Fatal(err)
	}
	r.unsignedRelease("u3", nil)
	r.clock.advance(time.Minute)
	r.check()
	if snap := r.currentSnapshot(); strOf(snap.SingleAnchorHash) != hashOf(u2) ||
		strOf(r.row().AdoptionFailReason) != cache.ReasonUnsignedDowngradeRefused || lastResult(r) != "changed_deferred" {
		t.Fatalf("with a marker: snapshot %+v record %q result %q, want u2 kept and the refusal", snap,
			strOf(r.row().AdoptionFailReason), lastResult(r))
	}
}

// Spec §7 (C5): an unsigned suite whose Release anchor row was reaped
// recovers it from the snapshot (the reader goes through the form: the
// unsigned Release hash sits in the single-anchor slot) and stays fresh.
func TestCheck_UnsignedRecoversWhenReleaseAnchorMissing(t *testing.T) {
	r := newRetryRig(t, 0)
	rel := r.adoptUnsigned("u1")
	ctx := context.Background()
	if err := r.cache.EvictURLPath(ctx, "http", rigHost, rigSuite+"/Release"); err != nil {
		t.Fatal(err)
	}
	r.check()
	row, err := r.cache.LookupURL(ctx, "http", rigHost, rigSuite+"/Release")
	if err != nil || strOf(row.BlobHash) != hashOf(rel) {
		t.Fatalf("Release anchor row %+v (%v), want it re-seeded with the snapshot's hash", row, err)
	}
	if res := lastResult(r); res != "unchanged" {
		t.Fatalf("result %q, want unchanged", res)
	}
}

// The reserved-slot rule (spec C4) for repair and reconcile: only a suite
// whose current snapshot is signed may take the last free slot (overview
// "E:" carry-forward: signedCurrent = form != unsigned).
func TestReconcile_UnsignedSuiteNeverTakesTheReservedSlot(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adoptUnsigned("u1")
	held := mustAcquire(t, r.checker.admission, "other", true)
	defer held.release()
	if r.checker.Reconcile(context.Background(), "http", rigHost, rigSuite) {
		t.Fatal("an unsigned suite's reconcile took the reserved slot")
	}
	signed := newRetryRig(t, 0)
	signed.adopt()
	heldSigned := mustAcquire(t, signed.checker.admission, "other", true)
	defer heldSigned.release()
	if !signed.checker.Reconcile(context.Background(), "http", rigHost, rigSuite) {
		t.Fatal("a signed suite's reconcile was refused the reserved slot")
	}
	signed.checker.WaitForAdoptions()
}

// finishCheck hands the repair the current snapshot's signedness.
func TestFinishCheck_RepairCarriesSignedCurrent(t *testing.T) {
	fc := newFakeCache()
	fc.track("http", "h", "/r/dists/x")
	c, err := New(Config{Cache: fc, Fetcher: &changedUpstream{}, HostLimiter: hostsem.New(1), Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	id := int64(3)
	for _, signed := range []bool{true, false} {
		cur := &cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", CurrentSnapshotID: &id}
		rp := c.finishCheck(context.Background(), &checkOutcome{}, cur, SuiteRef{CanonicalScheme: "http", CanonicalHost: "h",
			SuitePath: "/r/dists/x"}, checkEnd{form: "inline", result: "unchanged", status: 200, signedCurrent: signed})
		if rp == nil || rp.signed != signed {
			t.Fatalf("signedCurrent %v: repair %+v", signed, rp)
		}
	}
}

// hostCounter returns the value of the counter name for host, as
// metrics.Default renders it (0 when absent).
func hostCounter(t *testing.T, name, host string) float64 {
	t.Helper()
	var buf bytes.Buffer
	metrics.Default.Render(&buf)
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(line, name+"{") && strings.Contains(line, `host="`+host+`"`) {
			v, err := strconv.ParseFloat(line[strings.LastIndexByte(line, ' ')+1:], 64)
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
	}
	return 0
}

// The reserved-slot rule for the tick's repair (spec C4; overview "E:"
// carry-forward): a degraded unsigned suite's repair never takes the last
// free slot, a signed suite's may.
func TestCheck_RepairOfAnUnsignedSuiteNeverTakesTheReservedSlot(t *testing.T) {
	for _, form := range []cache.AnchorForm{cache.FormUnsigned, cache.FormInline} {
		t.Run(string(form), func(t *testing.T) {
			r := newRetryRig(t, 0)
			r.adopter.tolerateOptionalMembers, r.adopter.repairSkippedMembers, r.adopter.adoptUnsigned = true, true, true
			good := []byte(strings.Repeat("C", 100))
			text := datedRelease("degraded", 0, map[string][]byte{"main/binary-amd64/Packages": r.pkgs, "Contents-amd64.gz": good})
			r.members.put("http://h/r/dists/x/Contents-amd64.gz", []byte(strings.Repeat("z", 90))) // a stale optional member
			ref := SuiteRef{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite}
			if form == cache.FormUnsigned {
				r.up.setAt(rigReleaseURL, text)
				if err := r.adopter.RunUnsigned(context.Background(), ref, text, "", ""); err != nil {
					t.Fatal(err)
				}
			} else {
				in := asInRelease(text)
				r.up.set(in)
				if err := r.adopter.Run(context.Background(), ref, in, "", ""); err != nil {
					t.Fatal(err)
				}
			}
			held := mustAcquire(t, r.checker.admission, "other", true)
			defer held.release()
			before := hostCounter(t, "acu_repair_deferred_total", rigHost)
			r.check()
			deferred := hostCounter(t, "acu_repair_deferred_total", rigHost) - before
			if want := map[cache.AnchorForm]float64{cache.FormUnsigned: 1, cache.FormInline: 0}[form]; deferred != want {
				t.Fatalf("result %q: %v repairs deferred, want %v", lastResult(r), deferred, want)
			}
		})
	}
}

// Design review I-1: a record's validators belong to the anchor file its
// request fetched. A detached suite whose lateral InRelease keeps failing
// (a key not imported), behind a CDN that sends Last-Modified but no ETag,
// keeps adopting its new Release: the lateral record holds no validators,
// so the Release GET carries the current Release's own — never the
// InRelease's Last-Modified, which answers 304 for a Release of the same
// publish and froze the suite.
func TestCheck_DetachedSuiteKeepsAdoptingWhileItsLateralInReleaseFails(t *testing.T) {
	r := newRetryRig(t, 0)
	r.up.setLastModified(rigReleaseURL, "2026-01-01T00:00:00Z")
	r.up.setLastModified(rigReleaseGPGURL, "2026-01-01T00:00:00Z")
	r.adoptDetached()
	// One publish: an InRelease the cache cannot verify, a new Release and its signature.
	in := r.release("v2", 0)
	r.ver.reject(hashOf(in))
	r.up.set(in)
	rel := r.releaseText("v2", 0)
	r.up.setAt(rigReleaseURL, rel)
	r.up.setAt(rigReleaseGPGURL, armoredSig("v2 sig"))
	for _, url := range []string{rigAnchorURL, rigReleaseURL, rigReleaseGPGURL} {
		r.up.setLastModified(url, "2026-02-01T00:00:00Z")
	}
	r.clock.advance(time.Minute)
	r.check() // the lateral attempt fails
	if row := r.row(); r.ver.calls() != 1 || row.AdoptionFailCount != 1 || row.AdoptionFailLastMod != nil || row.AdoptionFailETag != nil {
		t.Fatalf("setup: calls %d, record %d with validators %v/%v; want the lateral failure recorded without validators",
			r.ver.calls(), row.AdoptionFailCount, strOf(row.AdoptionFailETag), strOf(row.AdoptionFailLastMod))
	}
	r.clock.advance(time.Minute) // before the record's retry time
	r.check()
	if snap := r.currentSnapshot(); snap.Form != cache.FormDetached || strOf(snap.ReleaseHash) != hashOf(rel) {
		t.Fatalf("snapshot %+v, result %q; want the detached v2 adopted while the lateral InRelease is deferred",
			snap, lastResult(r))
	}
}

// Spec C3 revision 8 (ruling PE-5, design review I-2): an unsigned suite
// whose upstream starts signing is marked as seen signed by the upgrade
// probe — even when the upgrade then fails (a key not imported) — so a
// later strip of both signature files (a man-in-the-middle answering 404)
// never updates it unsigned: refused as unsigned_downgrade_refused, the
// snapshot kept. A marker write that fails fails the check.
func TestCheck_UnsignedSuiteSeenSignedNeverUpdatesUnsigned(t *testing.T) {
	t.Run("stripped after a failed upgrade", func(t *testing.T) {
		r := newRetryRig(t, 0)
		u1 := r.adoptUnsigned("u1")
		signed := r.release("now signed", 0)
		r.ver.reject(hashOf(signed))
		r.up.set(signed)
		r.check() // the upgrade probe marks the base; the upgrade fails
		if r.ver.calls() != 1 || r.currentSnapshot().Form != cache.FormUnsigned {
			t.Fatalf("setup: calls %d, snapshot %+v; want a failed upgrade", r.ver.calls(), r.currentSnapshot())
		}
		if _, ok, err := r.cache.SignedSeen(context.Background(), "http", rigHost, rigSuite); err != nil || !ok {
			t.Fatalf("no marker after the probe saw the signature (ok %v, err %v)", ok, err)
		}
		r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 404})
		r.unsignedRelease("u2, modified", &fetch.StatusError{Code: 404})
		r.clock.advance(time.Hour)
		r.check()
		if snap := r.currentSnapshot(); strOf(snap.SingleAnchorHash) != hashOf(u1) ||
			strOf(r.row().AdoptionFailReason) != cache.ReasonUnsignedDowngradeRefused {
			t.Fatalf("snapshot %+v, record %q; want u1 kept and unsigned_downgrade_refused", snap, strOf(r.row().AdoptionFailReason))
		}
	})
	t.Run("failed marker write", func(t *testing.T) {
		r := newRetryRig(t, 0)
		r.adoptUnsigned("u1")
		r.up.set(r.release("now signed", 0))
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
		if row := r.row(); row.CheckFailCount != 1 || r.ver.calls() != 0 || r.currentSnapshot().Form != cache.FormUnsigned {
			t.Fatalf("row %+v, verifier calls %d: want a failed check, no upgrade attempt", row, r.ver.calls())
		}
	})
}

// Spec C4 Dispatch 2 ("if either is present → mark the base"), final fix
// wave FW-1 (a): the upgrade probe marks the base as soon as a
// sniff-passing Release.gpg is present, before it probes Release, so a
// Release probe that ends unknown cannot lose the sighting — and once the
// signature is stripped the suite is never updated unsigned. Both final
// reviewers' probes (spec C1, tests Critical 1).
func TestCheck_UpgradeProbeMarksTheSignatureBeforeProbingRelease(t *testing.T) {
	r := newRetryRig(t, 0)
	u1 := r.adoptUnsigned("u1")
	r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 404})
	r.up.setAt(rigReleaseGPGURL, armoredSig("a signature appears"))
	r.up.setFail(rigReleaseGPGURL, nil)
	r.up.setFail(rigReleaseURL, &fetch.StatusError{Code: 503})
	r.clock.advance(time.Minute)
	r.check()
	if _, ok, err := r.cache.SignedSeen(context.Background(), "http", rigHost, rigSuite); err != nil || !ok {
		t.Fatalf("no marker after the probe saw a sniff-passing Release.gpg and Release answered 503 (ok %v, err %v)", ok, err)
	}
	if row := r.row(); row.CheckFailCount != 1 {
		t.Fatalf("check_fail_count %d, want the unknown Release to fail the check", row.CheckFailCount)
	}
	// The signature is stripped and upstream publishes new unsigned bytes.
	r.up.setFail(rigReleaseURL, nil)
	r.unsignedRelease("u2", &fetch.StatusError{Code: 404})
	r.clock.advance(time.Hour)
	r.check()
	if snap := r.currentSnapshot(); snap.Form != cache.FormUnsigned || strOf(snap.SingleAnchorHash) != hashOf(u1) ||
		strOf(r.row().AdoptionFailReason) != cache.ReasonUnsignedDowngradeRefused {
		t.Fatalf("snapshot %+v, record %q; want u1 kept and unsigned_downgrade_refused", snap, strOf(r.row().AdoptionFailReason))
	}
}

// FW-1 (a) with ruling R-E4: a present Release.gpg is marked, and an
// absent Release behind it (the unsigned suite's current anchor) is
// anchor_absent — an answer that resets the streak and records the refusal
// state — not a failed check.
func TestCheck_UpgradeProbeAbsentReleaseIsAnchorAbsent(t *testing.T) {
	for name, releaseErr := range map[string]error{"404": &fetch.StatusError{Code: 404}, "403": &fetch.StatusError{Code: 403}} {
		t.Run(name, func(t *testing.T) {
			r := newRetryRig(t, 0)
			u1 := r.adoptUnsigned("u1")
			r.up.setAt(rigReleaseGPGURL, armoredSig("a signature appears"))
			r.up.setFail(rigReleaseGPGURL, nil)
			r.up.setFail(rigReleaseURL, releaseErr)
			r.check()
			if _, ok, err := r.cache.SignedSeen(context.Background(), "http", rigHost, rigSuite); err != nil || !ok {
				t.Fatalf("no marker (ok %v, err %v)", ok, err)
			}
			row := r.row()
			if row.CheckFailCount != 0 || lastResult(r) != "anchor_absent" || strOf(row.AdoptionFailReason) != cache.ReasonAnchorAbsent {
				t.Fatalf("check_fail_count %d, result %q, record %q; want anchor_absent with the streak reset",
					row.CheckFailCount, lastResult(r), strOf(row.AdoptionFailReason))
			}
			if len(r.events("freshness check failed")) != 0 {
				t.Fatal("an absent Release was logged as a failed check")
			}
			if snap := r.currentSnapshot(); strOf(snap.SingleAnchorHash) != hashOf(u1) {
				t.Fatalf("snapshot %+v, want u1 kept", snap)
			}
		})
	}
}

// Ruling PE-12 (final fix wave FW-2): with adoption.adopt_unsigned turned
// off, an established unsigned suite keeps serving its last snapshot,
// visibly refused (unsigned_disabled, changed_deferred), never as a
// failed check, and is never updated unsigned. Its record paces it: a
// check inside the record's window asks upstream nothing and the
// scheduler does not queue it; at the retry time it is refused again.
// Turning the knob back on updates it at its next retry, at once (an
// update of an established unsigned suite needs no confirmation). E2
// before PE-12: 30 checks, 30 full Release downloads, 60 signature probes,
// 30 failed checks and no visible state.
func TestCheck_UnsignedSuiteWithTheKnobOffIsAPacedRefusal(t *testing.T) {
	r := newRetryRig(t, time.Minute)
	u1 := r.adoptUnsigned("u1")
	r.adopter.adoptUnsigned = false
	u2 := r.unsignedRelease("u2", nil)
	r.clock.advance(61 * time.Second)
	r.check()
	row := r.row()
	if row.CheckFailCount != 0 || strOf(row.AdoptionFailReason) != cache.ReasonUnsignedDisabled || lastResult(r) != "changed_deferred" {
		t.Fatalf("check_fail_count %d, record %q, result %q; want the refusal unsigned_disabled and no failed check",
			row.CheckFailCount, strOf(row.AdoptionFailReason), lastResult(r))
	}
	if snap := r.currentSnapshot(); strOf(snap.SingleAnchorHash) != hashOf(u1) {
		t.Fatalf("snapshot %+v, want u1 kept", snap)
	}
	if ev := r.events("freshness check failed"); len(ev) != 0 {
		t.Fatalf("failed-check lines %v, want none", ev)
	}
	if ev := r.events("adoption_refused"); len(ev) != 1 || ev[0]["reason"] != cache.ReasonUnsignedDisabled {
		t.Fatalf("adoption_refused lines %v, want one with reason unsigned_disabled", ev)
	}
	before := r.rigRequests()
	r.clock.advance(61 * time.Second) // past the cooldown, inside the record's 2-minute window
	r.check()
	if n := r.rigRequests() - before; n != 0 {
		t.Fatalf("a check inside the record's window made %d requests, want 0", n)
	}
	stale := r.row()
	old := r.clock.now().Add(-time.Hour).Unix()
	stale.LastSuccessAt = &old
	if r.checker.suiteDue(stale, r.clock.now()) {
		t.Fatal("the scheduler queues an unsigned_disabled suite inside its record's window")
	}
	r.clock.advance(time.Minute) // the record is due: refused again
	r.check()
	row = r.row()
	if r.rigRequests() == before || row.AdoptionFailCount != 2 || strOf(row.AdoptionFailReason) != cache.ReasonUnsignedDisabled ||
		row.CheckFailCount != 0 || strOf(r.currentSnapshot().SingleAnchorHash) != hashOf(u1) {
		t.Fatalf("at the retry: %d requests, record %q/%d, check_fail_count %d; want the refusal again and u1 kept",
			r.rigRequests()-before, strOf(row.AdoptionFailReason), row.AdoptionFailCount, row.CheckFailCount)
	}
	r.adopter.adoptUnsigned = true // the knob back on (a restart)
	r.clock.advance(4 * time.Minute)
	r.check()
	if snap := r.currentSnapshot(); strOf(snap.SingleAnchorHash) != hashOf(u2) || snap.Form != cache.FormUnsigned {
		t.Fatalf("knob on, at the retry: snapshot %+v, record %q; want u2 adopted", snap, strOf(r.row().AdoptionFailReason))
	}
}

// Ruling PE-2: an adopted suite in anchor_absent is paced by its record —
// a client check (T1) inside the record's retry time asks nothing, and the
// scheduler does not queue it (suiteDue) until that time. A next_retry_at
// beyond the legitimate horizon (a backward clock step) is not paced: once
// the suite is due (its last success is old), the check clamps it to an
// hour and asks nothing.
func TestCheck_AbsentAnchorIsPacedByItsRecord(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopt()
	r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 404})
	r.clock.advance(time.Minute)
	r.check() // the first observation: anchor_absent, due in 2 minutes
	gets := func() int { return len(r.up.etagsSent(rigAnchorURL)) }
	before, lines := gets(), len(r.events("freshness_check"))
	for range 5 {
		r.clock.advance(20 * time.Second)
		r.check()
	}
	if gets() != before || len(r.events("freshness_check")) != lines {
		t.Fatalf("client checks inside the retry time: %d requests, %d lines; want none", gets()-before,
			len(r.events("freshness_check"))-lines)
	}
	// A long refusal backoff (as a later observation sets), and the last
	// success older than periodic_refresh: stale, but paced by the record.
	if err := r.cache.SetAdoptionNextRetry(context.Background(), "http", rigHost, rigSuite,
		r.clock.now().Add(30*time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	r.clock.advance(16 * time.Minute)
	if r.checker.suiteDue(r.row(), r.clock.now()) {
		t.Fatal("the scheduler queues an anchor_absent suite inside its record's retry time")
	}
	r.check()
	if gets() != before {
		t.Fatal("a check inside the record's retry time made a request")
	}
	r.clock.advance(15 * time.Minute) // past the retry time
	if !r.checker.suiteDue(r.row(), r.clock.now()) {
		t.Fatal("an anchor_absent suite is not due at its record's retry time")
	}
	r.check() // the anchor's GET, and — the absence confirmed — the tree, whose first probe is InRelease
	if gets() != before+2 {
		t.Fatalf("the due check made %d InRelease requests, want 2", gets()-before)
	}
	if err := r.cache.SetAdoptionNextRetry(context.Background(), "http", rigHost, rigSuite,
		r.clock.now().Add(24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	r.clock.advance(16 * time.Minute) // the last success is older than periodic_refresh
	if !r.checker.suiteDue(r.row(), r.clock.now()) {
		t.Fatal("a next_retry_at beyond the horizon kept the suite from being due")
	}
	r.check()
	if gets() != before+2 || i64Of(r.row().AdoptionNextRetryAt) != r.clock.now().Add(time.Hour).Unix() {
		t.Fatalf("beyond the horizon: %d requests, next_retry_at %d; want none and now + 1 h", gets()-before-2,
			i64Of(r.row().AdoptionNextRetryAt))
	}
}

// Design review M-5: an established unsigned suite whose new Release
// declares more than unsignedMemberCap members is refused at the check —
// unsigned_too_large, no slot, no adoption — and keeps its snapshot.
func TestCheck_UnsignedSuiteTooLargeUpdateIsRefused(t *testing.T) {
	r := newRetryRig(t, 0)
	u1 := r.adoptUnsigned("u1")
	big := make(map[string][]byte, unsignedMemberCap+1)
	for i := 0; i <= unsignedMemberCap; i++ {
		big[fmt.Sprintf("main/i18n/Translation-%d", i)] = []byte{byte(i)}
	}
	r.up.setAt(rigReleaseURL, datedRelease("big", 0, big))
	before := adoptionTotalFor(t, rigHost)
	r.check()
	if snap := r.currentSnapshot(); strOf(snap.SingleAnchorHash) != hashOf(u1) ||
		strOf(r.row().AdoptionFailReason) != cache.ReasonUnsignedTooLarge || lastResult(r) != "changed_deferred" {
		t.Fatalf("snapshot %+v record %q result %q; want u1 kept, unsigned_too_large, changed_deferred", snap,
			strOf(r.row().AdoptionFailReason), lastResult(r))
	}
	if adoptionTotalFor(t, rigHost) != before {
		t.Fatal("a too-large unsigned update ran an adoption")
	}
}

// Design review M-16: an unsigned suite whose Release was absent and is
// found again by the probe tree — its own form — is "unchanged" when the
// bytes are the snapshot's (the record is cleared) and an unsigned update
// otherwise (result changed, adopted at once, after the unsigned refusals:
// signed history refuses it); it is never re-recorded as anchor_absent.
func TestCheck_AbsentUnsignedReleaseThatComesBack(t *testing.T) {
	for _, tc := range []struct {
		name           string
		update, marker bool
		result         string
	}{
		{"same bytes", false, false, "unchanged"},
		{"new bytes", true, false, "changed"},
		{"new bytes with signed history", true, true, "changed_deferred"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRetryRig(t, 0)
			u1 := r.adoptUnsigned("u1")
			back := u1
			if tc.update {
				back = r.releaseText("u2", 0)
			}
			var n atomic.Int32
			up := simtest.NewUpstream()
			up.Set(rigReleaseURL, func(ctx context.Context, etag string) (*fetch.ConditionalResult, error) {
				if n.Add(1) <= 2 { // the check's GET twice; the tree's probe finds it
					return nil, &fetch.StatusError{Code: 404}
				}
				return simtest.Serve(back)(ctx, etag)
			})
			chk, err := New(Config{Cache: r.cache, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute,
				Adopter: r.adopter, Logger: slog.New(slog.NewJSONHandler(r.logs, nil)), // lastResult reads its lines
				now: r.clock.now, rand: func() float64 { return 0.5 }})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			chk.Check(ctx, "http", rigHost, rigSuite) // absent: anchor_absent recorded
			chk.WaitForAdoptions()
			if row := r.row(); strOf(row.AdoptionFailReason) != cache.ReasonAnchorAbsent {
				t.Fatalf("setup: record %q, want anchor_absent", strOf(row.AdoptionFailReason))
			}
			if tc.marker {
				if err := r.cache.MarkSignedSeenBatch(ctx, []cache.SignedSeenMark{{CanonicalScheme: "http",
					CanonicalHost: rigHost, Base: rigSuite, SeenAt: r.clock.now().Unix()}}); err != nil {
					t.Fatal(err)
				}
			}
			r.clock.advance(10 * time.Minute) // past the confirmation gap and the record's retry time
			chk.Check(ctx, "http", rigHost, rigSuite)
			chk.WaitForAdoptions()
			row, snap := r.row(), r.currentSnapshot()
			if res := lastResult(r); res != tc.result {
				t.Fatalf("result %q, want %q", res, tc.result)
			}
			if tc.marker {
				if strOf(row.AdoptionFailReason) != cache.ReasonUnsignedDowngradeRefused || strOf(snap.SingleAnchorHash) != hashOf(u1) {
					t.Fatalf("record %q, snapshot %+v; want unsigned_downgrade_refused and u1 kept", strOf(row.AdoptionFailReason), snap)
				}
				return
			}
			if row.AdoptionFailCount != 0 || snap.Form != cache.FormUnsigned || strOf(snap.SingleAnchorHash) != hashOf(back) {
				t.Fatalf("record %q/%d, snapshot %+v; want no record and the unsigned snapshot of the Release served",
					strOf(row.AdoptionFailReason), row.AdoptionFailCount, snap)
			}
		})
	}
}

// Design review I-2 on the absent-anchor path of an unsigned suite: when
// the probe tree finds a signature (InRelease appeared between the check's
// probes and the tree), the suite's base is marked before the upgrade is
// admitted — so it stays marked when the upgrade fails.
func TestCheck_AbsentUnsignedReleaseTreeFindsASignature(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adoptUnsigned("u1")
	signed := r.release("now signed", 0)
	r.ver.reject(hashOf(signed))
	var n atomic.Int32
	up := simtest.NewUpstream() // Release and Release.gpg: 404
	up.Set(rigAnchorURL, func(ctx context.Context, etag string) (*fetch.ConditionalResult, error) {
		if n.Add(1) <= 2 { // the upgrade probes of both checks; the tree's probe finds it
			return nil, &fetch.StatusError{Code: 404}
		}
		return simtest.Serve(signed)(ctx, etag)
	})
	chk, err := New(Config{Cache: r.cache, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute,
		Adopter: r.adopter, Logger: discardLogger(), now: r.clock.now, rand: func() float64 { return 0.5 }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	chk.Check(ctx, "http", rigHost, rigSuite) // Release absent: anchor_absent recorded
	chk.WaitForAdoptions()
	r.clock.advance(10 * time.Minute)
	chk.Check(ctx, "http", rigHost, rigSuite) // the tree finds InRelease: the upgrade, which fails
	chk.WaitForAdoptions()
	if r.ver.calls() != 1 || r.currentSnapshot().Form != cache.FormUnsigned {
		t.Fatalf("setup: calls %d, snapshot %+v; want one failed upgrade", r.ver.calls(), r.currentSnapshot())
	}
	if _, ok, err := r.cache.SignedSeen(ctx, "http", rigHost, rigSuite); err != nil || !ok {
		t.Fatalf("no marker after the tree found a signature (ok %v, err %v)", ok, err)
	}
}

// Deviation 2, for anchor_absent (design review M-19): a future
// adoption_fail_first_at never confirms the absence — the tree does not
// run — and re-recording restarts it at now (recordAdoptionFailure's
// past-event rule), so a backward clock step costs one more gap, never a
// skipped observation.
func TestCheck_AbsentAnchorWithAFutureFirstAtWaitsAnotherGap(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopt()
	r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 404})
	r.up.setAt(rigReleaseURL, r.releaseText("v2", 0))
	r.up.setAt(rigReleaseGPGURL, armoredSig("v2 sig"))
	now := r.clock.now()
	if err := r.cache.RecordAdoptionFailure(context.Background(), "http", rigHost, rigSuite, cache.AdoptionFailure{
		Hash: anchorAbsentKey, Reason: cache.ReasonAnchorAbsent, Count: 1,
		FirstAt: now.Add(2 * time.Hour).Unix(), LastAt: now.Unix(), NextRetryAt: now.Unix()}); err != nil {
		t.Fatal(err)
	}
	r.check()
	if n := len(r.up.etagsSent(rigReleaseURL)); n != 0 {
		t.Fatalf("the tree ran (%d Release probes) on an absence first observed in the future", n)
	}
	if first := i64Of(r.row().AdoptionFailFirstAt); first != now.Unix() {
		t.Fatalf("first_at = %d, want it restarted at %d", first, now.Unix())
	}
	r.clock.advance(6 * time.Minute) // past the gap and the record's retry time
	r.check()
	if snap := r.currentSnapshot(); lastResult(r) != "form_changed" || snap.Form != cache.FormDetached {
		t.Fatalf("result %q snapshot %+v, want the lateral move one gap later", lastResult(r), snap)
	}
}

// Design review M-13: an adopted suite stale in a refusal state
// (anchor_absent keeps its last success old on purpose) counts in the
// stale gauge but not in the stale WARN, which keeps alerting on a suite
// stale for any other reason.
func TestStaleSuiteCounts_RefusalStates(t *testing.T) {
	id, old := int64(7), int64(500)
	absent, failing := cache.ReasonAnchorAbsent, "gpg_failed"
	suites := []cache.SuiteFreshness{
		{LastSuccessAt: &old, CurrentSnapshotID: &id, AdoptionFailCount: 3, AdoptionFailReason: &absent},
		{LastSuccessAt: &old, CurrentSnapshotID: &id, AdoptionFailCount: 3, AdoptionFailReason: &failing},
		{LastSuccessAt: &old, CurrentSnapshotID: &id},
		{LastSuccessAt: &old, AdoptionFailCount: 1, AdoptionFailReason: &absent},
	}
	adopted, unadopted, refused := staleSuiteCounts(suites, 1000, 3000)
	if adopted != 3 || unadopted != 1 || refused != 1 {
		t.Fatalf("adopted %d unadopted %d refused %d, want 3, 1, 1", adopted, unadopted, refused)
	}
}

func TestTick_StaleWarnLeavesOutRefusalStates(t *testing.T) {
	for _, absent := range []bool{true, false} {
		r := newRetryRig(t, 0)
		r.adopt()
		if absent {
			r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 404}) // anchor_absent
		} else {
			r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 503}) // failed checks
		}
		r.clock.advance(time.Minute)
		r.check()
		r.clock.advance(time.Hour) // past 4 × periodic_refresh
		r.checker.tick(context.Background(), newSchedQueues())
		warns := r.events("freshness: adopted suites stale beyond threshold")
		if absent && len(warns) != 0 {
			t.Fatalf("anchor_absent: %d stale WARNs, want none", len(warns))
		}
		if !absent && (len(warns) != 1 || warns[0]["adopted_stale"] != float64(1)) {
			t.Fatalf("failing upstream: stale WARNs %v, want one with adopted_stale 1", warns)
		}
	}
}

// Design note 23 / design review M-14: a changed Release whose signature
// is absent does not take the record a still-served lateral InRelease
// holds — that content keeps its own schedule.
func TestCheck_AbsentSignatureKeepsTheLateralRecord(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adoptDetached()
	bad := r.release("bad", 0)
	r.ver.reject(hashOf(bad))
	r.up.set(bad)
	r.clock.advance(time.Minute)
	r.check() // the lateral attempt fails: a record for the InRelease content
	r.up.setAt(rigReleaseURL, r.releaseText("v2", 0))
	r.up.setFail(rigReleaseGPGURL, &fetch.StatusError{Code: 404})
	r.clock.advance(20 * time.Second) // before the record's retry time: the lateral content is held
	r.check()
	if row := r.row(); lastResult(r) != "anchor_absent" || strOf(row.AdoptionFailReason) == cache.ReasonAnchorAbsent ||
		strOf(row.AdoptionFailHash) != hashOf(bad) {
		t.Fatalf("result %q record %q; want anchor_absent reported and the lateral content's record kept", lastResult(r),
			strOf(row.AdoptionFailReason))
	}
}

// Design note 24: an unsigned suite never updates past a signature that may
// be there — an unknown Release.gpg probe fails the check exactly as an
// unknown InRelease does (TestCheck_UnsignedSuiteUpgradesWhenSigned covers
// only the latter). (Proposed by the executability review; kills X06.)
func TestCheck_UnsignedSuiteUnknownReleaseGPGFailsTheCheck(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adoptUnsigned("u1")
	r.unsignedRelease("u2", &fetch.StatusError{Code: 503})
	before := r.currentSnapshot().SnapshotID
	r.check()
	if row := r.row(); row.CheckFailCount != 1 || r.currentSnapshot().SnapshotID != before {
		t.Fatalf("row %+v, current %d: want a failed check and no unsigned update", row, r.currentSnapshot().SnapshotID)
	}
}

// Spec C4 reserved slot: an unsigned suite (its current snapshot vouches
// nothing) never takes the last free slot — neither for an update of its
// unsigned Release nor for an upgrade to a signed form; both queue.
// (Proposed by the executability review; kills X07, X08.)
func TestCheck_UnsignedSuiteNeverTakesTheReservedSlotToAdopt(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		r := newRetryRig(t, 0) // MaxConcurrent: 2
		r.adoptUnsigned("u1")
		if upgrade {
			r.up.set(r.release("now signed", 0)) // InRelease present: the upgrade
		} else {
			r.unsignedRelease("u2", nil) // an update of the unsigned Release
		}
		held := mustAcquire(t, r.checker.admission, "other", true) // one of two slots taken
		r.check()
		held.release()
		if lastResult(r) != "changed_queued" {
			t.Fatalf("upgrade=%v: result %q, want changed_queued (the last slot is reserved for signed suites)", upgrade, lastResult(r))
		}
	}
}

// Spec C4 Dispatch 1 / design note 23: an anchor that was absent and is
// found again by the probe tree — the same form and the same bytes as the
// current snapshot — is "unchanged" and clears the anchor_absent record;
// it is not a form change that re-adopts the current content. The
// upstream answers the anchor's own GET 404 on the first two checks and
// serves it again to the tree's probe (an anchor that comes back within a
// check). (Proposed by the executability review; kills X25, which the plan
// pinned at the predicate only.)
func TestCheck_AbsentAnchorThatComesBackIsUnchanged(t *testing.T) {
	r := newRetryRig(t, 0)
	v1 := r.release("v1", 0)
	r.up.set(v1)
	r.check() // the first adoption: inline v1
	snap := r.currentSnapshot()
	if snap == nil {
		t.Fatal("setup: v1 not adopted")
	}
	var n atomic.Int32
	up := simtest.NewUpstream()
	up.Set(rigAnchorURL, func(ctx context.Context, etag string) (*fetch.ConditionalResult, error) {
		if n.Add(1) <= 2 {
			return nil, &fetch.StatusError{Code: 404}
		}
		return simtest.Serve(v1)(ctx, etag)
	})
	chk, err := New(Config{Cache: r.cache, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute,
		Adopter: r.adopter, Logger: discardLogger(), now: r.clock.now, rand: func() float64 { return 0.5 }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	chk.Check(ctx, "http", rigHost, rigSuite) // absent: anchor_absent recorded
	chk.WaitForAdoptions()
	if row := r.row(); strOf(row.AdoptionFailReason) != cache.ReasonAnchorAbsent {
		t.Fatalf("setup: record %q, want anchor_absent", strOf(row.AdoptionFailReason))
	}
	r.clock.advance(10 * time.Minute) // past the confirmation gap and the record's retry time
	chk.Check(ctx, "http", rigHost, rigSuite)
	chk.WaitForAdoptions()
	row := r.row()
	if row.AdoptionFailCount != 0 || row.CurrentSnapshotID == nil || *row.CurrentSnapshotID != snap.SnapshotID {
		t.Fatalf("row %+v: want the record cleared and snapshot %d still current", row, snap.SnapshotID)
	}
}

// Rulings PE-10 (a) and PE-11 (review fix rounds 1 and 2, Task 7): a
// detached suite whose upstream serves one publish that fails in both
// signed forms (a signing key not imported) is attempted once per form,
// then at most once per form per lateral retry window: the lateral
// InRelease when its own hold (the memo of its last failure) comes due,
// the detached path's content at the next check after it (new content
// against the lateral's record, spec C7). Before PE-10 the two forms took
// the one record in turn and every check was an attempt (6 in six checks
// 20 s apart, 1 a minute after that).
func TestCheck_BothSignedFormsFailingAreAttemptedOncePerFormPerRetryWindow(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adoptDetached()
	in := r.release("v2", 0)
	rel, sig := r.releaseText("v2", 0), armoredSig("v2 sig")
	kIn, kDet := contentKey(adoptionFormInline, in, nil, nil), contentKey(adoptionFormDetached, nil, rel, sig)
	r.ver.reject(kIn)
	r.ver.reject(kDet)
	r.up.set(in)
	r.up.setAt(rigReleaseURL, rel)
	r.up.setAt(rigReleaseGPGURL, sig)
	for range 6 { // the reviewer's probe: six checks 20 s apart
		r.clock.advance(20 * time.Second)
		r.check()
	}
	if r.ver.calls() != 2 {
		t.Fatalf("six checks 20 s apart made %d attempts, want 2 (one per form)", r.ver.calls())
	}
	// The lateral's window: the retry time written at its last failure
	// (the second check's record was the detached one, so read it from the
	// first attempt: 20 s + 2 min).
	lateralRetry := r.clock.now().Add(-100 * time.Second).Unix()
	detachedSinceLateral := 1
	for i := range 240 { // four hours of checks a minute apart
		calls := r.ver.calls()
		r.clock.advance(time.Minute)
		r.check()
		n := r.ver.calls() - calls
		after := r.row()
		switch {
		case n > 1:
			t.Fatalf("check %d made %d attempts", i, n)
		case n == 1 && strOf(after.AdoptionFailHash) == kIn:
			if now := r.clock.now().Unix(); now < lateralRetry {
				t.Fatalf("check %d: the lateral InRelease was attempted %d s before its own retry time", i, lateralRetry-now)
			}
			lateralRetry, detachedSinceLateral = i64Of(after.AdoptionNextRetryAt), 0
		case n == 1 && strOf(after.AdoptionFailHash) == kDet:
			if detachedSinceLateral++; detachedSinceLateral > 1 {
				t.Fatalf("check %d: the detached content was attempted twice in one lateral window", i)
			}
		case n == 1:
			t.Fatalf("check %d: an attempt recorded %s, neither form's content", i, strOf(after.AdoptionFailHash))
		}
	}
	// Windows of 2 min doubling to 1 h, one attempt per form per lateral
	// window: 2 + 12 in the four hours (one a minute without the rulings).
	t.Logf("%d attempts in four hours and two minutes", r.ver.calls())
	if got := r.ver.calls(); got > 14 {
		t.Fatalf("%d attempts in four hours and two minutes, want at most 14", got)
	}
}

// churnDetached makes the rig's detached form publish new failing content
// (a Release and its signature the verifier rejects) labelled i.
func churnDetached(r *retryRig, i int) {
	rel, sig := r.releaseText(fmt.Sprintf("d%d", i), 0), armoredSig(fmt.Sprintf("d%d sig", i))
	r.ver.reject(contentKey(adoptionFormDetached, nil, rel, sig))
	r.up.setAt(rigReleaseURL, rel)
	r.up.setAt(rigReleaseGPGURL, sig)
}

// Ruling PE-11 (review fix round 2, Task 7; the re-review's probe): the
// lateral's hold is its own, so the detached form's failures never extend
// it. A detached suite whose upstream publishes failing detached content
// every 30 minutes keeps its record from ever coming due; a good InRelease
// that appears then is adopted at the first check that sees it. Under
// PE-10 (a) it waited behind the record forever (12 detached attempts in
// the next 6 h, never adopted).
func TestCheck_GoodInReleaseIsNotStarvedByChurningFailingDetachedContent(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adoptDetached()
	pub := 0
	churnDetached(r, pub)
	minute := 0
	step := func() {
		minute++
		r.clock.advance(time.Minute)
		if minute%30 == 0 {
			pub++
			churnDetached(r, pub)
		}
		r.check()
	}
	for range 3 * 60 { // three hours of failing detached publishes
		step()
	}
	if row := r.row(); row.AdoptionFailCount < 5 || i64Of(row.AdoptionNextRetryAt) <= r.clock.now().Unix() {
		t.Fatalf("setup: record count %d, retry at %d; want a long-failing record not due", row.AdoptionFailCount,
			i64Of(row.AdoptionNextRetryAt))
	}
	r.up.set(r.release("good", 0)) // a good InRelease appears
	step()
	if snap := r.currentSnapshot(); snap.Form != cache.FormInline {
		t.Fatalf("snapshot form %q after the first check that saw the good InRelease (result %q), want it adopted (inline)",
			snap.Form, lastResult(r))
	}
}

// Ruling PE-11: a failing lateral InRelease is retried within its own
// window — the C7 delay written at its failure — however often the
// detached form publishes failing content (which replaces the record and
// pushes its retry time).
func TestCheck_FailingLateralInReleaseIsRetriedWithinItsOwnWindow(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adoptDetached()
	bad := r.release("bad", 0)
	kIn := contentKey(adoptionFormInline, bad, nil, nil)
	r.ver.reject(kIn)
	r.up.set(bad)
	pub := 0
	churnDetached(r, pub)
	var lateral []int64 // when the lateral was attempted
	var windows []int64 // the retry time written at each lateral failure
	for minute := 1; minute <= 4*60; minute++ {
		r.clock.advance(time.Minute)
		if minute%30 == 0 {
			pub++
			churnDetached(r, pub)
		}
		calls := r.ver.calls()
		r.check()
		if row := r.row(); r.ver.calls() > calls && strOf(row.AdoptionFailHash) == kIn {
			lateral = append(lateral, r.clock.now().Unix())
			windows = append(windows, i64Of(row.AdoptionNextRetryAt))
		}
	}
	if len(lateral) < 4 {
		t.Fatalf("the failing lateral InRelease was attempted %d times in four hours, want it retried in its own windows (>= 4)", len(lateral))
	}
	for i := 1; i < len(lateral); i++ {
		if lateral[i] < windows[i-1] || lateral[i] > windows[i-1]+60 {
			t.Fatalf("lateral attempt %d at %d, want at its previous failure's retry time %d (within one check)", i, lateral[i], windows[i-1])
		}
	}
}

// Ruling PE-11: the lateral's hold is in memory, so a restart costs at most
// one extra lateral attempt per suite. Both forms fail until the retry
// windows reach the 1 h cap; right after a lateral failure and the
// detached failure that follows it (the record is the detached content's,
// the lateral's own hold has an hour to run), a checker that keeps its
// memory makes no lateral attempt in the next 50 minutes, and a restarted
// one — which never saw the lateral fail — makes exactly one, then holds it
// by the memo of that failure.
func TestCheck_ARestartCostsAtMostOneExtraLateralAttempt(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "kept", true: "restarted"}[restart], func(t *testing.T) {
			r := newRetryRig(t, 0)
			r.adoptDetached()
			in := r.release("v2", 0)
			rel, sig := r.releaseText("v2", 0), armoredSig("v2 sig")
			kIn, kDet := contentKey(adoptionFormInline, in, nil, nil), contentKey(adoptionFormDetached, nil, rel, sig)
			r.ver.reject(kIn)
			r.ver.reject(kDet)
			r.up.set(in)
			r.up.setAt(rigReleaseURL, rel)
			r.up.setAt(rigReleaseGPGURL, sig)
			// attempted reports the key a check attempted ("" for none).
			attempted := func(check func()) string {
				calls := r.ver.calls()
				check()
				if r.ver.calls() == calls {
					return ""
				}
				return strOf(r.row().AdoptionFailHash)
			}
			lastLateral := false
		setup:
			for i := 0; ; i++ {
				if i > 600 {
					t.Fatal("setup: the windows never reached the cap")
				}
				r.clock.advance(time.Minute)
				switch attempted(r.check) {
				case kIn:
					lastLateral = true
				case kDet:
					if lastLateral && r.row().AdoptionFailCount >= 8 {
						break setup // a lateral failure, then the detached one, at the cap
					}
					lastLateral = false
				}
			}
			chk := r.checker
			if restart {
				var err error
				chk, err = New(Config{Cache: r.cache, Fetcher: r.up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute,
					Adopter: r.adopter, Logger: discardLogger(), now: r.clock.now, rand: func() float64 { return 0.5 }})
				if err != nil {
					t.Fatal(err)
				}
			}
			lateral := 0
			for range 50 { // checks a minute apart, inside the lateral's hour
				r.clock.advance(time.Minute)
				if attempted(func() {
					chk.Check(context.Background(), "http", rigHost, rigSuite)
					chk.WaitForAdoptions()
				}) == kIn {
					lateral++
				}
			}
			if want := map[bool]int{false: 0, true: 1}[restart]; lateral != want {
				t.Fatalf("restarted %v: the lateral InRelease was attempted %d times in 50 minutes, want %d", restart, lateral, want)
			}
		})
	}
}

// Ruling PE-11: the lateral hold memo is dropped with the suite's
// lock-map entry, like the absent answer (its row is gone).
func TestLateralHold_DroppedWithTheLockEntry(t *testing.T) {
	r := newRetryRig(t, 0)
	key := suiteKey("http", rigHost, rigSuite)
	r.checker.lateralHolds.Store(key, lateralHold{key: "k", retryAt: r.clock.now().Add(time.Hour).Unix()})
	release, ok := r.checker.HoldSuite(cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite})
	if !ok {
		t.Fatal("HoldSuite refused")
	}
	release(true)
	if _, ok := r.checker.lateralHolds.Load(key); ok {
		t.Fatal("dropping the suite's lock-map entry kept its lateral hold")
	}
}

// pacedWindowRun runs ticks scheduler ticks (refresh/4 apart) on r, checking
// the suite whenever suiteDue queues it, and returns the scheduler checks
// and the upstream requests (counted by reqs) they made. It fails t when a
// record's retry window sees more than one check that made a request: the
// first answered check after an attempt re-observes the absence (it must,
// or the current form's own new content would wait behind a failing record
// forever); every later check inside that window is paced (ruling PE-10 b).
func pacedWindowRun(t *testing.T, r *retryRig, ticks int, reqs func() int) (checks, requests int) {
	t.Helper()
	inWindow := map[string]int{}
	start := reqs()
	for range ticks {
		r.clock.advance(15 * time.Minute / 4)
		row := r.row()
		if !r.checker.suiteDue(row, r.clock.now()) {
			continue
		}
		checks++
		g := r.checker.retryGateFor(&row, r.clock.now())
		n := reqs()
		r.check()
		if reqs() > n && g.active && !g.due {
			id := fmt.Sprintf("%s/%d", g.hash, row.AdoptionFailCount)
			if inWindow[id]++; inWindow[id] > 1 {
				t.Fatalf("record %s: %d checks inside its retry window made requests, want at most 1", id, inWindow[id])
			}
		}
	}
	return checks, reqs() - start
}

// Ruling PE-10 (b): design review M-14 with a held lateral record — a
// detached suite whose changed Release comes without its Release.gpg while
// a failing lateral InRelease holds the record. The check reports
// anchor_absent and the record keeps the attempt's reason (gpg_failed, not
// anchor_absent: it is the lateral content's), and the record paces the
// suite as an anchor_absent record would: a client check inside its window
// makes no request and the scheduler does not queue it. Before the ruling
// the suite stayed stale and unpaced (20 scheduler checks and 50 requests
// in two hours, each with full bodies).
func TestCheck_AbsentSignatureWithAHeldLateralRecordIsPacedByIt(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adoptDetached()
	bad := r.release("bad", 0)
	r.ver.reject(hashOf(bad))
	r.up.set(bad)
	r.clock.advance(time.Minute)
	r.check() // the lateral attempt fails: a record for the InRelease content
	r.up.setAt(rigReleaseURL, r.releaseText("v2", 0))
	r.up.setFail(rigReleaseGPGURL, &fetch.StatusError{Code: 404})
	reqs := func() int {
		return len(r.up.etagsSent(rigAnchorURL)) + len(r.up.etagsSent(rigReleaseURL)) + len(r.up.etagsSent(rigReleaseGPGURL))
	}
	r.clock.advance(20 * time.Second)
	r.check() // M-14 behind the held lateral record
	if row := r.row(); lastResult(r) != "anchor_absent" || strOf(row.AdoptionFailReason) != "gpg_failed" ||
		strOf(row.AdoptionFailHash) != hashOf(bad) {
		t.Fatalf("result %q, record %q for %s; want anchor_absent shown and the lateral attempt's record (gpg_failed) kept",
			lastResult(r), strOf(row.AdoptionFailReason), strOf(row.AdoptionFailHash))
	}
	// A long retry time (as the record's backoff reaches), so the window
	// outlasts periodic_refresh.
	if err := r.cache.SetAdoptionNextRetry(context.Background(), "http", rigHost, rigSuite, r.clock.now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	n := reqs()
	for range 3 {
		r.clock.advance(20 * time.Second)
		r.check()
	}
	if got := reqs() - n; got != 0 {
		t.Fatalf("client checks inside the record's window made %d requests, want none", got)
	}
	r.clock.advance(16 * time.Minute) // stale (periodic_refresh 15 min), the record still not due
	if r.checker.suiteDue(r.row(), r.clock.now()) {
		t.Fatal("the scheduler queues the stale suite inside the record's window")
	}
	r.check()
	if got := reqs() - n; got != 0 {
		t.Fatalf("a check of the stale suite inside the record's window made %d requests, want none", got)
	}
	checks, requests := pacedWindowRun(t, r, 32, reqs) // two hours
	t.Logf("two hours: %d scheduler checks, %d upstream requests, %d attempts", checks, requests, r.ver.calls())
	if checks > 8 || requests > 12 {
		t.Fatalf("two hours: %d scheduler checks, %d upstream requests; want at most 8 and 12 (paced by the record)", checks, requests)
	}
}

// Ruling PE-10 (b): an inline suite whose InRelease disappeared and whose
// lateral move to the detached form found by the probe tree failed. The
// attempt's record (not anchor_absent) holds; the absent current anchor is
// asked for again only at its retry times. Before the ruling the suite sat
// stale and unpaced: 30 scheduler checks and 35 InRelease requests in three
// hours (7 and 12 when paced by an anchor_absent record).
func TestCheck_AbsentAnchorWithAFailedLateralMoveIsPacedByItsRecord(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopt()
	r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 404})
	rel, sig := r.releaseText("v2", 0), armoredSig("v2 sig")
	r.ver.reject(contentKey(adoptionFormDetached, nil, rel, sig))
	r.up.setAt(rigReleaseURL, rel)
	r.up.setAt(rigReleaseGPGURL, sig)
	reqs := func() int { return len(r.up.etagsSent(rigAnchorURL)) }
	checks, requests := pacedWindowRun(t, r, 48, reqs) // three hours
	t.Logf("three hours: %d scheduler checks, %d InRelease requests, %d attempts", checks, requests, r.ver.calls())
	if r.ver.calls() == 0 || strOf(r.row().AdoptionFailReason) == cache.ReasonAnchorAbsent {
		t.Fatalf("setup: %d attempts, record %q; want the lateral move attempted and its record held",
			r.ver.calls(), strOf(r.row().AdoptionFailReason))
	}
	if checks > 12 || requests > 17 {
		t.Fatalf("three hours: %d scheduler checks, %d InRelease requests; want at most 12 and 17 (paced by the record)", checks, requests)
	}
	if r.ver.calls() > 6 {
		t.Fatalf("%d lateral attempts in three hours, want at most 6", r.ver.calls())
	}
}

// snapReadFailCache fails GetSuiteSnapshot while fail is set.
type snapReadFailCache struct {
	Cache
	fail atomic.Bool
}

func (c *snapReadFailCache) GetSuiteSnapshot(ctx context.Context, id int64) (*cache.SuiteSnapshot, error) {
	if c.fail.Load() {
		return nil, fmt.Errorf("injected snapshot read failure (id %d)", id)
	}
	return c.Cache.GetSuiteSnapshot(ctx, id)
}

// Review fix round 1, Task 7 Important 2: anchorFoundAgain cannot tell an
// anchor that merely came back from new content without the current
// snapshot. A read error is logged at WARN and fails the check (as the
// baseline's read is logged): nothing is admitted, so the current anchor
// is never re-adopted as "changed".
func TestCheck_AnchorFoundAgainFailsOnASnapshotReadError(t *testing.T) {
	r := newRetryRig(t, 0)
	v1 := r.release("v1", 0)
	r.up.set(v1)
	r.check() // the first adoption: inline v1
	snap := r.currentSnapshot()
	if snap == nil {
		t.Fatal("setup: v1 not adopted")
	}
	fc := &snapReadFailCache{Cache: r.cache}
	var n atomic.Int32
	up := simtest.NewUpstream()
	up.Set(rigAnchorURL, func(ctx context.Context, etag string) (*fetch.ConditionalResult, error) {
		switch n.Add(1) {
		case 1, 2: // the anchor's own GET on both checks
			return nil, &fetch.StatusError{Code: 404}
		default: // the tree's probe on the second check: the anchor came back
			fc.fail.Store(true)
			return simtest.Serve(v1)(ctx, etag)
		}
	})
	var logs lockedBuffer
	chk, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute,
		Adopter: r.adopter, Logger: slog.New(slog.NewJSONHandler(&logs, nil)), now: r.clock.now, rand: func() float64 { return 0.5 }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	chk.Check(ctx, "http", rigHost, rigSuite) // absent: anchor_absent recorded
	chk.WaitForAdoptions()
	r.clock.advance(10 * time.Minute) // past the confirmation gap and the record's retry time
	calls := r.ver.calls()
	chk.Check(ctx, "http", rigHost, rigSuite)
	chk.WaitForAdoptions()
	fc.fail.Store(false)
	warns := logRecords(t, &logs, "freshness: read current snapshot failed")
	if len(warns) != 1 || warns[0]["level"] != "WARN" || warns[0]["snapshot_id"] != float64(snap.SnapshotID) {
		t.Fatalf("WARN records %v, want one naming snapshot %d", warns, snap.SnapshotID)
	}
	failed := logRecords(t, &logs, "freshness check failed")
	if len(failed) != 1 || !strings.Contains(fmt.Sprint(failed[0]["err"]), "read current snapshot") {
		t.Fatalf("failed-check records %v, want the check failed on the snapshot read", failed)
	}
	if row := r.row(); r.ver.calls() != calls || row.CheckFailCount != 1 || row.CurrentSnapshotID == nil ||
		*row.CurrentSnapshotID != snap.SnapshotID {
		t.Fatalf("attempts %d, row %+v; want no attempt, the streak at 1 and snapshot %d still current",
			r.ver.calls()-calls, row, snap.SnapshotID)
	}
}

// Ruling PE-10 (b) never starves the current form: a detached suite paced
// behind a failing lateral InRelease (M-14) whose upstream then publishes a
// signed Release adopts it at the first check after the record's retry —
// the lateral attempt's check clears the absent answer, so the next check
// asks once more and the detached path adopts its own new content at once
// (spec C7), the record notwithstanding.
func TestCheck_PacedAbsentSignatureStillAdoptsANewSignedRelease(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adoptDetached()
	bad := r.release("bad", 0)
	r.ver.reject(hashOf(bad))
	r.up.set(bad)
	r.clock.advance(time.Minute)
	r.check() // the lateral attempt fails: a record for the InRelease content
	r.up.setAt(rigReleaseURL, r.releaseText("v2", 0))
	r.up.setFail(rigReleaseGPGURL, &fetch.StatusError{Code: 404})
	r.clock.advance(20 * time.Second)
	r.check() // M-14 behind the held lateral record: paced from here
	rel := r.releaseText("v3", 0)
	r.up.setAt(rigReleaseURL, rel)
	r.up.setFail(rigReleaseGPGURL, nil)
	r.up.setAt(rigReleaseGPGURL, armoredSig("v3 sig"))
	r.clock.advance(20 * time.Second)
	r.check() // inside the window: paced, v3 not seen yet
	if snap := r.currentSnapshot(); strOf(snap.ReleaseHash) == hashOf(rel) {
		t.Fatal("setup: a paced check adopted v3")
	}
	r.clock.advance(2 * time.Minute)
	r.check() // the record's retry: the lateral content is attempted again (and fails)
	calls := r.ver.calls()
	r.clock.advance(20 * time.Second)
	r.check() // the next check asks once more and adopts v3
	if snap := r.currentSnapshot(); snap.Form != cache.FormDetached || strOf(snap.ReleaseHash) != hashOf(rel) || r.ver.calls() != calls+1 {
		t.Fatalf("snapshot %+v after %d attempts, result %q; want v3 adopted by the check after the lateral retry",
			snap, r.ver.calls()-calls, lastResult(r))
	}
}

// Ruling PE-10 (b): the check tails set and clear the absent answer
// (pacedAbsentAnswer): an anchor_absent result sets it, any other answered
// result and a failed check clear it, and so does dropping the suite's
// lock-map entry (its row is gone).
func TestCheckTails_AbsentAnswer(t *testing.T) {
	r := newRetryRig(t, 0)
	ctx := context.Background()
	ref := SuiteRef{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite}
	key := suiteKey("http", rigHost, rigSuite)
	answered := func() bool { _, ok := r.checker.absentAnswered.Load(key); return ok }
	cur := r.row()
	var out checkOutcome
	r.checker.finishCheck(ctx, &out, &cur, ref, checkEnd{form: "inline", result: "anchor_absent", status: 404})
	if !answered() {
		t.Fatal("an anchor_absent result did not set the absent answer")
	}
	r.checker.finishCheck(ctx, &out, &cur, ref, checkEnd{form: "inline", result: "unchanged", status: 200})
	if answered() {
		t.Fatal("an unchanged result kept the absent answer")
	}
	r.checker.finishCheck(ctx, &out, &cur, ref, checkEnd{form: "inline", result: "anchor_absent", status: 404})
	r.checker.failCheck(ctx, &out, &cur, ref, r.clock.now().Unix(), "inline", errors.New("unknown end"))
	if answered() {
		t.Fatal("a failed check kept the absent answer")
	}
	r.checker.finishCheck(ctx, &out, &cur, ref, checkEnd{form: "inline", result: "anchor_absent", status: 404})
	release, ok := r.checker.HoldSuite(cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite})
	if !ok {
		t.Fatal("HoldSuite refused")
	}
	release(true) // the row was deleted while held
	if answered() {
		t.Fatal("dropping the suite's lock-map entry kept its absent answer")
	}
}

// Ruling PE-10 (a): held means the record is the lateral InRelease's own
// content, not merely that a record is active. A detached suite whose own
// failing content holds the record clears it when upstream serves the
// current Release again (spec C7 "unchanged"), even while a failing
// InRelease is still served.
func TestCheck_DetachedUnchangedClearsItsOwnRecordWhileAnInReleaseIsServed(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adoptDetached()
	in := r.release("v2", 0)
	rel, sig := r.releaseText("v2", 0), armoredSig("v2 sig")
	r.ver.reject(contentKey(adoptionFormInline, in, nil, nil))
	r.ver.reject(contentKey(adoptionFormDetached, nil, rel, sig))
	r.up.set(in)
	r.up.setAt(rigReleaseURL, rel)
	r.up.setAt(rigReleaseGPGURL, sig)
	for range 2 { // the lateral attempt, then the detached one: each fails
		r.clock.advance(20 * time.Second)
		r.check()
	}
	if row := r.row(); r.ver.calls() != 2 || strOf(row.AdoptionFailHash) != contentKey(adoptionFormDetached, nil, rel, sig) {
		t.Fatalf("setup: %d attempts, record %s; want the detached content's record", r.ver.calls(), strOf(row.AdoptionFailHash))
	}
	r.up.setAt(rigReleaseURL, r.releaseText("base", 0)) // upstream rolls the Release back to the current one
	r.up.setAt(rigReleaseGPGURL, armoredSig("base sig"))
	r.clock.advance(20 * time.Second)
	r.check()
	if row := r.row(); lastResult(r) != "unchanged" || row.AdoptionFailCount != 0 {
		t.Fatalf("result %q, record count %d; want unchanged and the detached record cleared", lastResult(r), row.AdoptionFailCount)
	}
}

// Ruling PE-2's pacing is persisted, so it survives a restart: a checker
// that never saw the absence (ruling PE-10's in-memory absent answer is
// empty) still asks nothing inside an anchor_absent record's retry time,
// and its scheduler does not queue the stale suite.
func TestCheck_AbsentAnchorIsPacedByItsRecordAfterARestart(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopt()
	r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 404})
	r.clock.advance(time.Minute)
	r.check() // the first observation: anchor_absent
	if err := r.cache.SetAdoptionNextRetry(context.Background(), "http", rigHost, rigSuite,
		r.clock.now().Add(30*time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(Config{Cache: r.cache, Fetcher: r.up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute,
		Adopter: r.adopter, Logger: discardLogger(), now: r.clock.now, rand: func() float64 { return 0.5 }})
	if err != nil {
		t.Fatal(err)
	}
	gets := func() int { return len(r.up.etagsSent(rigAnchorURL)) }
	before := gets()
	r.clock.advance(16 * time.Minute) // stale, the record not due
	if restarted.suiteDue(r.row(), r.clock.now()) {
		t.Fatal("after a restart the scheduler queues an anchor_absent suite inside its record's retry time")
	}
	restarted.Check(context.Background(), "http", rigHost, rigSuite)
	restarted.WaitForAdoptions()
	if gets() != before {
		t.Fatalf("after a restart a check inside the record's retry time made %d requests, want none", gets()-before)
	}
}

// Ruling PE-11: only the failed content is held. A detached suite whose
// lateral InRelease failed, and whose upstream then replaces that InRelease
// with good content inside the failure's retry window, adopts the new one
// at the first check that sees it — it is neither the memo's content nor
// the record's. (Under PE-10 it waited for the record's retry time.)
func TestCheck_NewLateralInReleaseIsAttemptedAtOnce(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adoptDetached()
	bad := r.release("bad", 0)
	r.ver.reject(hashOf(bad))
	r.up.set(bad)
	r.clock.advance(time.Minute)
	r.check() // the lateral attempt fails: its memo and its record
	if r.ver.calls() != 1 || strOf(r.row().AdoptionFailHash) != hashOf(bad) {
		t.Fatalf("setup: %d attempts, record %s", r.ver.calls(), strOf(r.row().AdoptionFailHash))
	}
	r.up.set(r.release("good", 0))
	r.clock.advance(20 * time.Second) // inside the failure's 2-minute window
	r.check()
	if snap := r.currentSnapshot(); snap.Form != cache.FormInline {
		t.Fatalf("snapshot form %q, result %q; want the new InRelease adopted at once", snap.Form, lastResult(r))
	}
}

// Ruling PE-11, the clock-step rule: a lateral hold whose retry time lies
// beyond the legitimate horizon (a backward wall-clock step) is clamped to
// now + 1 h, as a record's next_retry_at is — never a frozen lateral.
func TestLateralHeld_ClampsARetryTimeBeyondTheHorizon(t *testing.T) {
	r := newRetryRig(t, 0)
	key := suiteKey("http", rigHost, rigSuite)
	now := r.clock.now().Unix()
	r.checker.lateralHolds.Store(key, lateralHold{key: "k", retryAt: now + 30*86400})
	if !r.checker.lateralHeld(key, "k", now) {
		t.Fatal("a held lateral was not held")
	}
	if r.checker.lateralHeld(key, "other", now) {
		t.Fatal("other content was held by the memo")
	}
	if r.checker.lateralHeld(key, "k", now+int64(retryMaxDelay/time.Second)) {
		t.Fatal("a retry time 30 days ahead held the lateral past now + 1 h")
	}
}
