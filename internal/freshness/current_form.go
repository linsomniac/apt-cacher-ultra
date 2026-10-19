package freshness

// Spec C4 Dispatch 1 and 2 with an adopter wired: a suite with a current
// snapshot moves between the signed forms when its anchor disappears or
// the other signed form appears, an unsigned suite upgrades as soon as a
// signature is served, and nothing ever moves a signed suite to unsigned.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
)

// pacedAbsent is ruling PE-2 for an adopted suite: while its active record
// is anchor_absent and not due, the check makes no request — the current
// anchor is asked for again at the record's retry time (2 min doubling to
// 1 h: about 24 requests a day once the absence has lasted an hour), not at
// every tick and every client check (T1). checkLocked applies it before
// any request, and suiteDue does not queue such a suite.
//
// AIDEV-NOTE: the record's schedule is the refusal's, not the failure
// backoff: the check_fail_count streak stays 0 (ruling R-E4). Cost: a
// returning anchor is noticed at the record's retry time (at most 1 h
// later) instead of at once. An EOL release removed upstream would
// otherwise cost 400–1,800 requests a day, forever.
func pacedAbsent(g retryGate) bool {
	return g.active && !g.due && g.reason == cache.ReasonAnchorAbsent
}

// pacedAbsentAnswer is ruling PE-10 (b): an adopted suite whose last
// answered check ended anchor_absent (absentAnswered) while a record of
// another reason holds — a failed adoption attempt's: the inline suite
// whose absent InRelease's lateral move failed, or a detached suite whose
// changed Release has no Release.gpg while a failing lateral InRelease
// holds the record (design review M-14) — is paced by that record exactly
// as pacedAbsent paces an anchor_absent one: no request before its retry
// time (checkLocked), and suiteDue does not queue it. The record shown
// keeps the attempt's reason (e.g. gpg_failed) and schedule.
//
// AIDEV-NOTE: "answered absent" is in-memory (absentAnswered), because no
// persisted column tells it from a queued result: both leave
// last_success_at older than last_check_at with the streak at 0. It is
// set and cleared only by the check tails (finishCheck: set for an
// anchor_absent result, cleared for any other; failCheck clears) and
// dropped with the suite's lock-map entry (unlockDropped). A restart loses
// it: the first check after the start asks upstream once and sets it
// again. An attempt check (the lateral move, at the record's retry time)
// clears it, so the next answered check inside the new window asks once
// more — the re-observation that lets the current form's own new content
// (a detached suite's signed Release) be adopted at once (spec C7) instead
// of waiting behind a failing lateral record forever.
func (c *Checker) pacedAbsentAnswer(key string, g retryGate) bool {
	if !g.active || g.due {
		return false
	}
	_, ok := c.absentAnswered.Load(key)
	return ok
}

// body returns the bytes of the anchor file name the tree fetched.
func (t probeTree) body(name string) []byte {
	switch name {
	case "Release":
		return t.release.res.Body
	case "Release.gpg":
		return t.sig.res.Body
	}
	return t.inRelease.res.Body
}

// sameAs reports whether the tree found exactly snap's form and anchor
// files: the anchor came back between the check's request and the tree.
func (t probeTree) sameAs(snap *cache.SuiteSnapshot) bool {
	form, ok := formOfSnapshot(snap)
	if !ok || !t.found || form != t.form {
		return false
	}
	for _, name := range aptpath.AnchorPaths(formName(t.form)) {
		h := snap.AnchorHash(name)
		sum := sha256.Sum256(t.body(name))
		if h == nil || *h != hex.EncodeToString(sum[:]) {
			return false
		}
	}
	return true
}

// anchorAbsent is spec C4 Dispatch 1's "current anchor absent" (inline:
// InRelease; detached: Release; unsigned: Release) for an adopted suite.
// The first absent observation records the refusal state anchor_absent
// (keyed anchorAbsentKey); once the record is due and the absence has
// lasted confirmGap (two observations at least max(cooldown, 5 min)
// apart), the probe tree runs without validators (anchorFoundAgain): the
// other signed form found is adopted (a lateral move or an upgrade, result
// form_changed); the current form and content found again is "unchanged"
// (the record is cleared); an unsigned suite's own form found again with
// new bytes is its update; anything else — an unsigned candidate of a
// signed suite, or nothing — is recorded again: never unsigned from signed.
// A record from an adoption attempt (the lateral move failed) keeps its own
// schedule: at its retry time the tree runs at once. Every outcome but a
// probe that ends unknown leaves last_success_at alone and resets the
// check_fail_count streak (ruling R-E4: an absent anchor is an answer, not
// an unknown end).
//
// AIDEV-NOTE: last_success_at stays old on purpose (spec C4: "visible;
// last_success_at not bumped"); the record paces the suite (pacedAbsent),
// so it costs one anchor request per retry, plus the probe tree once the
// absence is confirmed. A future adoption_fail_first_at never confirms
// (its age is negative) and re-recording restarts it — deviation 2, here
// for the absence's two observations.
func (c *Checker) anchorAbsent(ctx, fetchCtx context.Context, out *checkOutcome, cur *cache.SuiteFreshness, suite SuiteRef,
	now time.Time, nowUnix int64, form adoptionForm, o probeOutcome, gate retryGate, signedCurrent bool) *adoptionRequest {
	record := &adoptionRequest{suite: suite, key: anchorAbsentKey}
	absentRecord := gate.active && gate.reason == cache.ReasonAnchorAbsent
	runTree := false
	switch {
	case !gate.active:
		c.recordAdoptionFailure(record, cache.ReasonAnchorAbsent) // the first observation
	case !gate.due:
		// Inside the retry time of a record from an adoption attempt: the
		// first answered check after the attempt (an anchor_absent record
		// is paced before any request: pacedAbsent; this result paces the
		// later checks inside the window: pacedAbsentAnswer, ruling PE-10).
	case !absentRecord:
		runTree = true
	case cur.AdoptionFailFirstAt != nil && now.Sub(time.Unix(*cur.AdoptionFailFirstAt, 0)) >= c.confirmGap():
		runTree = true
	default:
		c.recordAdoptionFailure(record, cache.ReasonAnchorAbsent)
	}
	if runTree {
		rows, _, err := c.cachedAnchors(ctx, suite)
		if err != nil {
			c.logger.Warn("freshness: read anchor rows failed",
				"err", err, "canonical_host", suite.CanonicalHost, "suite_path", suite.SuitePath)
		}
		t := c.runProbeTree(fetchCtx, suite, c.anchorURLsFor(suite, rows))
		switch {
		case t.end != nil && t.end.cancelled():
			return nil
		case t.end != nil:
			c.failCheck(ctx, out, cur, suite, nowUnix, formName(form), fmt.Errorf("%s: %w", t.endName, t.end.err))
			return nil
		case t.found && (t.form != adoptionFormUnsigned || form == adoptionFormUnsigned):
			return c.anchorFoundAgain(ctx, out, cur, suite, nowUnix, form, t, signedCurrent)
		}
		c.recordAdoptionFailure(record, cache.ReasonAnchorAbsent)
	}
	cur.LastCheckAt = &nowUnix
	cur.CheckFailCount = 0
	c.finishCheck(ctx, out, cur, suite, checkEnd{form: formName(form), result: "anchor_absent", status: o.status,
		reason: cache.ReasonAnchorAbsent, signedCurrent: signedCurrent})
	return nil
}

// anchorFoundAgain handles a probe tree, run for an absent current anchor,
// that found a form the suite may adopt: a signed form, or — for an
// unsigned suite — the unsigned form again (design review M-16). The
// current snapshot's own anchors are "unchanged" (the record is cleared);
// anything else is a request: another form's (form_changed, a cross-form
// request), or new content of the current form (changed), an unsigned
// suite's update after the unsigned refusals (unsignedRefusal). A signature
// found for a suite without a signed current snapshot is marked first
// (Adopter.markSignatureSeen); a failed mark fails the check, and so does a
// failed read of the current snapshot (logged at WARN).
func (c *Checker) anchorFoundAgain(ctx context.Context, out *checkOutcome, cur *cache.SuiteFreshness, suite SuiteRef,
	nowUnix int64, form adoptionForm, t probeTree, signedCurrent bool) *adoptionRequest {
	if !signedCurrent && t.signatureSeen() {
		if err := c.adopter.markSignatureSeen(ctx, suite, nowUnix); err != nil {
			c.failCheck(ctx, out, cur, suite, nowUnix, formName(form), fmt.Errorf("mark the signature seen: %w", err))
			return nil
		}
	}
	snap, err := c.cache.GetSuiteSnapshot(ctx, *cur.CurrentSnapshotID)
	if err != nil {
		// Without the current snapshot an anchor that merely came back
		// would be taken for new content and re-adopted (review fix round
		// 1, Task 7): log it as the baseline's read is, and fail the check.
		c.logger.Warn("freshness: read current snapshot failed",
			"err", err,
			"snapshot_id", *cur.CurrentSnapshotID,
			"canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath,
		)
		c.failCheck(ctx, out, cur, suite, nowUnix, formName(form), fmt.Errorf("read current snapshot: %w", err))
		return nil
	}
	same := t.sameAs(snap)
	req := t.request(suite)
	req.crossForm = t.form != form
	refusal := ""
	if !same && t.form == adoptionFormUnsigned {
		var err error
		if refusal, err = c.unsignedRefusal(ctx, req); err != nil {
			c.failCheck(ctx, out, cur, suite, nowUnix, formName(form), err)
			return nil
		}
	}
	prevSuccess := cur.LastSuccessAt
	cur.LastCheckAt, cur.LastSuccessAt, cur.CheckFailCount = &nowUnix, &nowUnix, 0
	e := checkEnd{form: formName(t.form), status: 200, signedCurrent: signedCurrent}
	switch {
	case same:
		cur.InReleaseChangeSeenAt = nil
		e.result, e.clearRecord, req = "unchanged", true, nil
	case refusal != "":
		setChangeSeen(cur, nowUnix)
		e.result, e.reason, req = "changed_deferred", refusal, nil
	case t.form == form:
		setChangeSeen(cur, nowUnix)
		req, e.result = c.admit(out, cur, req, signedCurrent, prevSuccess, "changed", "changed_queued")
	default:
		setChangeSeen(cur, nowUnix)
		req, e.result = c.admit(out, cur, req, signedCurrent, prevSuccess, "form_changed", "changed_queued")
	}
	c.finishCheck(ctx, out, cur, suite, e)
	return req
}

// lateralInline is spec C4 Dispatch 1 for a detached current snapshot:
// InRelease is probed on every check, and present → an inline adoption
// request (a lateral move, result form_changed; a cross-form request). An
// absent or unknown InRelease, or any InRelease while a record is active
// and not due (ruling PE-10: no cross-form attempt inside a retry window;
// held when the record is that InRelease content's), leaves the check to
// the detached path (done = false): the current form's own request
// decides the result. The caller holds the host slot.
//
// AIDEV-NOTE: held must stop the detached path's "unchanged" from
// clearing the record: it belongs to the InRelease content, which
// upstream still serves, and clearing it would let every other check
// attempt that content again (spec C7's bounded attempts). The record
// keeps no validators (crossForm), so the detached path's GET carries the
// current Release's own (design review I-1).
// AIDEV-NOTE: ruling PE-10 (a): retry discipline beats cross-form
// eagerness. Holding only the record's own content let the two forms of a
// publish that fails in both take the one record in turn, so every check
// attempted one of them (up to 1,440 a day). Now no InRelease is attempted
// while any record is active and not due: with both forms failing the
// suite makes one attempt per form per retry window (the detached path
// still attempts its own new content at once, spec C7, while the record is
// the InRelease's), and a good InRelease waits at most the record's retry
// time (1 h) behind a failing detached record while the suite keeps
// serving its snapshot.
func (c *Checker) lateralInline(ctx, fetchCtx context.Context, out *checkOutcome, cur *cache.SuiteFreshness, suite SuiteRef,
	urls anchorURLs, gate retryGate, signedCurrent bool, nowUnix int64) (req *adoptionRequest, done, held bool) {
	o := c.probe(fetchCtx, suite, urls, "InRelease")
	if o.class != classPresent {
		return nil, false, false
	}
	req = probeTree{form: adoptionFormInline, found: true, inRelease: o}.request(suite)
	req.crossForm = true
	if gate.active && !gate.due {
		return nil, false, req.key == gate.hash
	}
	prevSuccess := cur.LastSuccessAt
	cur.LastCheckAt, cur.LastSuccessAt, cur.CheckFailCount = &nowUnix, &nowUnix, 0
	setChangeSeen(cur, nowUnix)
	e := checkEnd{form: "inline", status: 200, signedCurrent: signedCurrent}
	req, e.result = c.admit(out, cur, req, signedCurrent, prevSuccess, "form_changed", "changed_queued")
	c.finishCheck(ctx, out, cur, suite, e)
	return req, true, false
}

// upgradeProbe is the first half of spec C4 Dispatch 2 (current snapshot
// unsigned): InRelease present → an inline request; else Release.gpg
// present → Release (no validators) and a detached request — an upgrade,
// result form_changed (changed_deferred while the gate holds that content,
// changed_queued without a slot), a cross-form request. A signature found
// marks the suite's base before anything else (Adopter.markSignatureSeen,
// design review I-2: the suite's clients never fetch one, so only this
// probe can record that the suite was seen signed; a failed mark fails the
// check). Both absent → done = false: the unsigned check goes on. A probe
// that ends unknown is a failed check: a signature may be there, so the
// unsigned path does not run. The caller holds the host slot.
func (c *Checker) upgradeProbe(ctx, fetchCtx context.Context, out *checkOutcome, cur *cache.SuiteFreshness, suite SuiteRef,
	urls anchorURLs, gate retryGate, nowUnix int64) (req *adoptionRequest, done bool) {
	fail := func(name string, o probeOutcome) (*adoptionRequest, bool) {
		if !o.cancelled() {
			c.failCheck(ctx, out, cur, suite, nowUnix, "unsigned", fmt.Errorf("%s (%s): %w", name, o.class, o.err))
		}
		return nil, true
	}
	t := probeTree{found: true, inRelease: c.probe(fetchCtx, suite, urls, "InRelease")}
	switch t.inRelease.class {
	case classPresent:
		t.form = adoptionFormInline
	case classUnknown:
		return fail("InRelease", t.inRelease)
	default:
		if t.sig = c.probe(fetchCtx, suite, urls, "Release.gpg"); t.sig.class == classAbsent {
			return nil, false
		} else if t.sig.class == classUnknown {
			return fail("Release.gpg", t.sig)
		}
		if t.release = c.probe(fetchCtx, suite, urls, "Release"); t.release.class != classPresent {
			if t.release.err == nil {
				t.release.err = errors.New("not a Release")
			}
			return fail("Release", t.release)
		}
		t.form = adoptionFormDetached
	}
	if err := c.adopter.markSignatureSeen(ctx, suite, nowUnix); err != nil {
		c.failCheck(ctx, out, cur, suite, nowUnix, "unsigned", fmt.Errorf("mark the signature seen: %w", err))
		return nil, true
	}
	req = t.request(suite)
	req.crossForm = true
	prevSuccess := cur.LastSuccessAt
	cur.LastCheckAt, cur.LastSuccessAt, cur.CheckFailCount = &nowUnix, &nowUnix, 0
	setChangeSeen(cur, nowUnix)
	e := checkEnd{form: formName(t.form), status: 200}
	if gate.active && !gate.due && req.key == gate.hash {
		e.result, e.reason, req = "changed_deferred", gate.reason, nil
	} else {
		req, e.result = c.admit(out, cur, req, false, prevSuccess, "form_changed", "changed_queued")
	}
	c.finishCheck(ctx, out, cur, suite, e)
	return req, true
}

// checkUnsigned is spec C4 Dispatch 2 (current snapshot unsigned): the
// upgrade probes (adopter wired), then a conditional GET of Release
// against the snapshot's Release hash; changed → an unsigned adoption
// request — no re-confirmation (spec C5 safeguard 2: updates of an
// established unsigned suite adopt at once), but the unsigned refusals
// apply before a slot is taken (unsignedRefusal: signed history refuses
// it as unsigned_downgrade_refused, a member count over the cap as
// unsigned_too_large) and the knob (off: a failed check). A reaped Release
// anchor is reconstructed from the snapshot
// (TestCheck_UnsignedRecoversWhenReleaseAnchorMissing). Without an adopter
// it only observes, like the other forms.
func (c *Checker) checkUnsigned(ctx, fetchCtx context.Context, out *checkOutcome, cur *cache.SuiteFreshness, suite SuiteRef,
	now time.Time, nowUnix int64) (*adoptionRequest, *repairRequest) {
	scheme, host, suitePath := suite.CanonicalScheme, suite.CanonicalHost, suite.SuitePath
	releasePath := suitePath + releaseFilename
	row, err := c.cache.LookupURL(ctx, scheme, host, releasePath)
	switch {
	case errors.Is(err, cache.ErrNotFound):
		h := c.snapshotAnchorHash(ctx, *cur.CurrentSnapshotID, releaseFilename)
		row = c.reconstructAnchor(ctx, scheme, host, suitePath, releasePath, *cur.CurrentSnapshotID, h, nowUnix)
	case err != nil:
		c.logger.Warn("freshness: lookup Release url_path failed",
			"err", err, "canonical_host", host, "suite_path", suitePath)
		return nil, nil
	}
	cachedHash, _ := c.baseline(ctx, cur, suite, row, "Release")
	if cachedHash == "" {
		return nil, nil
	}
	gate := c.retryGateFor(cur, now)
	c.applyClamp(ctx, cur, gate, now)

	release, err := c.hostSem.Acquire(fetchCtx, host)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			c.logger.Debug("freshness: host limiter acquire aborted",
				"err", err, "canonical_host", host, "suite_path", suitePath)
			return nil, nil
		}
		c.failCheck(ctx, out, cur, suite, nowUnix, "unsigned", err)
		return nil, nil
	}
	defer release()

	if c.adopter != nil {
		urls := c.anchorURLsFor(suite, map[string]string{"Release": row.UpstreamURL})
		if req, done := c.upgradeProbe(ctx, fetchCtx, out, cur, suite, urls, gate, nowUnix); done {
			return req, nil
		}
	}

	etag, lastmod := gate.validators(cur)
	res, err := c.fetcher.Conditional(fetchCtx, &fetch.Target{CanonicalHost: host, URL: row.UpstreamURL}, etag, lastmod, c.maxBody)
	if c.adopter != nil {
		if o := classify(res, err, sniffRelease, etag != "" || lastmod != ""); o.class == classAbsent {
			return c.anchorAbsent(ctx, fetchCtx, out, cur, suite, now, nowUnix, adoptionFormUnsigned, o, gate, false), nil
		}
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			c.logger.Debug("freshness: check cancelled", "canonical_host", host, "suite_path", suitePath)
			return nil, nil
		}
		c.failCheck(ctx, out, cur, suite, nowUnix, "unsigned", err)
		return nil, nil
	}
	if res.Status != 200 && res.Status != 304 {
		c.failCheck(ctx, out, cur, suite, nowUnix, "unsigned", fmt.Errorf("%w %d", errUnexpectedStatus, res.Status))
		return nil, nil
	}

	newHash := bodyHash(res)
	var req *adoptionRequest
	e := checkEnd{form: "unsigned", status: res.Status}
	key := contentKey(adoptionFormUnsigned, nil, res.Body, nil)
	steady := res.Status == 304 || newHash == cachedHash
	deferred := !steady && gate.active && !gate.due && key == gate.hash
	if !steady && !deferred && c.adopter != nil {
		refusal, err := c.unsignedRefusal(ctx, &adoptionRequest{suite: suite, key: key, releaseBytes: res.Body,
			etag: res.ETag, lastmod: res.LastModified})
		if err != nil {
			c.failCheck(ctx, out, cur, suite, nowUnix, "unsigned", err)
			return nil, nil
		}
		if refusal != "" {
			deferred, gate.reason = true, refusal
		}
	}
	prevSuccess := cur.LastSuccessAt
	cur.LastCheckAt, cur.LastSuccessAt, cur.CheckFailCount = &nowUnix, &nowUnix, 0
	switch result, clear, ok := steadyResult(cur, res, newHash, cachedHash, gate, nowUnix); {
	case ok:
		e.result, e.clearRecord = result, clear
		if result == "changed_deferred" {
			e.reason = gate.reason
		}
	case deferred:
		setChangeSeen(cur, nowUnix)
		e.result, e.reason = "changed_deferred", gate.reason
	default:
		setChangeSeen(cur, nowUnix)
		req = &adoptionRequest{suite: suite, form: adoptionFormUnsigned, key: key, releaseBytes: res.Body,
			etag: res.ETag, lastmod: res.LastModified}
		req, e.result = c.admit(out, cur, req, false, prevSuccess, "changed", "changed_queued")
	}
	return req, c.finishCheck(ctx, out, cur, suite, e)
}
