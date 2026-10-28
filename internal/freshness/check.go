package freshness

// The freshness check (spec C4), run with the suite mutex held: checkLocked
// reads the row, applies the cooldown and dispatches on the suite's anchor
// form. Every answered check ends in one tail, finishCheck, and every
// failed one in failCheck (ruling R-E3: record-clearing, the
// freshness_check line and its counter live in one place).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
)

// checkLocked is the body of Check, run with the per-suite mutex held.
// It fills out: out.req is non-nil iff the check found content to adopt
// (result changed, form_changed or first_adoption), the caller has an
// Adopter wired in (Phase 2 path) AND an admission slot was free
// (out.lease); the caller uses this signal to spawn the adoption
// goroutine. On a confirmed-unchanged result planRepair sets out.repair
// and out.lease when there is something to heal and a slot was free.
//
// Form dispatch (spec C4 Dispatch; SPEC2 §7.6.3):
//   - With an adopter wired, a suite with no current snapshot runs the
//     probe tree (checkFirst, Dispatch 3) or stays dormant.
//   - A suite with a current snapshot takes its form from the snapshot
//     row's anchor_form (snap.Form): inline, detached or unsigned
//     (Dispatch 1 and 2). The check then issues a conditional GET against
//     InRelease (inline) or Release (detached, unsigned).
//   - Without an adopter (Phase 1, observation only) a first-ever check
//     tries inline first; if the inline GET 404s AND a Release url_path
//     row exists, it falls back to the detached path, as before phase E
//     (TestCheck_Phase1DetachedFallback_OnInRelease404).
//
// Outcomes (all paths must update suite_freshness so cooldown applies
// to the next attempt):
//
//   - Cooldown gate fails: no DB write, return nil (reported as
//     skipped). The scheduler's slot-driven re-check of a waiting suite
//     and its check of a suite with a pending change bypass the gate
//     (opts.bypassCooldown, spec C4).
//   - Metadata url_path row absent: no DB write (we have nothing to
//     check against), return nil — first request for this suite will
//     land in url_path through the normal miss path.
//   - 304: last_check_at = last_success_at = now.
//   - 200, body hash matches cached: bump last_check_at, last_success_at,
//     and refresh validators (upstream may have rotated etag/lastmod
//     while bytes are unchanged); clear any adoption failure record.
//   - 200, body hash differs: bump last_check_at, last_success_at,
//     stamp inrelease_change_seen_at on first sighting only, return
//     *adoptionRequest.
//   - Same, but no admission slot is free (spec C4): "changed_queued"
//     (or first_adoption_queued for a first adoption), no request,
//     last_success_at left at its previous value; the
//     scheduler parks the suite until a slot it can use is released (a
//     client check's suite is parked by the next tick: its pending
//     change makes it due, see Checker.pendingChange — unless it holds a
//     failure record whose retry is not due: then once it goes stale, at
//     the record's retry or by a later client check that wins a slot,
//     spec C4's bound).
//   - Spec C7 gate (retryGateFor, adopter wired only): while a failure
//     record exists and next_retry_at has not come, the GET carries the
//     failing content's validators when the record holds its own (a
//     cross-form request records none, and then the current anchor's own
//     are sent: design review I-1), and a 304 to the record's validators
//     or the same content key is "changed_deferred" (no adoption); new
//     content is attempted at once; at next_retry_at the GET carries no
//     validators. An adopted suite whose record is anchor_absent or
//     unsigned_disabled (or whose anchor answered absent behind an
//     attempt's record) is paced before any request (pacedByRefusal,
//     pacedAbsentAnswer); a detached suite probes InRelease first
//     (lateralInline).
//   - Error (network, an error status that is not an absent answer, 5xx,
//     check deadline — also one hit while waiting for the host slot), a
//     success status other than 200 and 304, or a 304 to a GET without
//     validators: a failed check (failCheck) — last_check_at bumped,
//     check_fail_count extended (don't hammer a broken upstream), the
//     "freshness check failed" line and the failed counter;
//     last_success_at keeps its previous value. Every answered check
//     above resets check_fail_count to 0 (spec C4).
//   - Cancelled caller (shutdown), at the host slot, the GET or a local
//     step after it (endedByShutdown): no DB write; it says nothing about
//     upstream.
//   - With an adopter wired, absent answers (401/403/404/410, a 200
//     failing its sniff) on the current anchor are anchor_absent (ruling
//     R-E4), and refusal states are recorded, not failed (spec C7).
//
// SPEC2 wiring: the body-hash comparison uses the current snapshot's
// hash of the form's anchor (cache.SuiteSnapshot.AnchorHash: InRelease
// for inline, Release for detached and unsigned — the unsigned Release
// sits in the single-anchor slot) when the suite has an adopted
// snapshot (baseline). Without this, every
// freshness check after a successful adoption would observe "changed"
// against the stale url_path.blob_hash and thrash the adoption
// candidate-uniqueness constraint forever.
func (c *Checker) checkLocked(ctx, fetchCtx context.Context, out *checkOutcome, scheme, host, suitePath string, opts checkOpts) {
	now := c.now()
	nowUnix := now.Unix()

	cur, err := c.cache.GetSuiteFreshness(ctx, scheme, host, suitePath)
	switch {
	case errors.Is(err, cache.ErrNotFound):
		// I-4: the row vanished after check's pre-read (cap eviction, later
		// the reaper). Nothing to check and nothing to write; check drops
		// the lock-map entry.
		out.vanished = true
		return
	case err != nil:
		c.logger.Warn("freshness: read suite row failed",
			"err", err,
			"canonical_host", host,
			"suite_path", suitePath,
		)
		return
	}

	// Cooldown gate. Spec C4 clock-step rule: a past-event stamp found in
	// the future (backward wall-clock step) is treated as infinitely old,
	// never as "just checked" — otherwise the suite freezes for the size
	// of the step (security finding I-7).
	if cur.LastCheckAt != nil && c.cooldown > 0 {
		last := time.Unix(*cur.LastCheckAt, 0)
		switch {
		case last.After(now.Add(clockStepTolerance)):
			c.clockStep("last_check_at", scheme, host, suitePath, *cur.LastCheckAt, now)
		case opts.bypassCooldown:
			// The scheduler's slot-driven re-check, or its check of a
			// suite with a pending change (spec C4).
		case now.Sub(last) < c.cooldown:
			return // out.report stays resultSkipped
		}
	}

	// A last_success_at in the future made the suite due (suiteDue reads
	// it as infinitely old without reporting it on every tick, deferred
	// minor B-38). It is reported here, below the cooldown gate, so a
	// client check the cooldown turns away counts nothing: each check that
	// runs reports it until a successful answer rewrites the stamp
	// (deferred minor E-37).
	if cur.LastSuccessAt != nil && time.Unix(*cur.LastSuccessAt, 0).After(now.Add(clockStepTolerance)) {
		c.clockStep("last_success_at", scheme, host, suitePath, *cur.LastSuccessAt, now)
	}

	suite := SuiteRef{
		CanonicalScheme: scheme,
		CanonicalHost:   host,
		SuitePath:       suitePath,
	}

	// Spec C4 Dispatch 3: with an adopter wired, a suite with no current
	// snapshot runs the probe tree (or stays dormant).
	if c.adopter != nil && cur.CurrentSnapshotID == nil {
		out.req = c.checkFirst(ctx, fetchCtx, out, cur, suite, now, nowUnix, opts.bypassCooldown)
		return
	}
	// Ruling PE-2: an adopted suite in the refusal state anchor_absent is
	// paced by its record: no request before the record's retry time. So is
	// one in unsigned_disabled (ruling PE-12), and one whose last answered
	// check found its anchor absent while an attempt's record holds (ruling
	// PE-10: pacedAbsentAnswer).
	if c.adopter != nil {
		if gate := c.retryGateFor(cur, now); pacedByRefusal(gate) || c.pacedAbsentAnswer(suiteKey(scheme, host, suitePath), gate) {
			c.applyClamp(ctx, cur, gate, now)
			c.logger.Debug("freshness_refused_suite_paced_until_retry",
				"canonical_host", host, "suite_path", suitePath, "reason", gate.reason)
			return
		}
	}

	// Form preference: if there's a current snapshot, the form is its
	// anchor_form (snap.Form). Otherwise (no adopter: the Phase 1
	// observation) default to inline; the inline path triggers a detached
	// fallback on 404.
	//
	// AIDEV-NOTE: the final review's R1 #1. A failed read of the current
	// snapshot is a local fault, as ruling PG-24 makes checkFirst's: no
	// request, nothing written (no failCheck either: it says nothing about
	// upstream), and the next check reads again. Taken for "inline", as it
	// was, it sent a detached or unsigned suite down the inline path, which
	// records anchor_absent when a stale cached InRelease row answers 404,
	// or reconstructs an InRelease row the suite never had.
	form, err := c.detectForm(ctx, cur)
	if err != nil {
		if !c.endedByShutdown(ctx, suite, "read current snapshot", err) {
			c.logger.Warn("freshness_read_current_snapshot_failed",
				"err", err,
				"snapshot_id", *cur.CurrentSnapshotID,
				"canonical_host", host,
				"suite_path", suitePath,
			)
		}
		return
	}
	switch form {
	case adoptionFormUnsigned:
		out.req, out.repair = c.checkUnsigned(ctx, fetchCtx, out, cur, suite, now, nowUnix)
	case adoptionFormDetached:
		out.req, out.repair = c.checkDetached(ctx, fetchCtx, out, cur, suite, now, nowUnix)
	default:
		req, repair, fellBack := c.checkInline(ctx, fetchCtx, out, cur, suite, now, nowUnix)
		if fellBack {
			req, repair = c.checkDetached(ctx, fetchCtx, out, cur, suite, now, nowUnix)
		}
		out.req, out.repair = req, repair
	}
	if out.req == nil && out.repair != nil {
		c.planRepair(ctx, out)
	}
}

// detectForm reads the suite's current snapshot (if any) and returns
// its anchor_form (snap.Form): inline, detached or unsigned. First-ever
// suites (cur.CurrentSnapshotID == nil; only without an adopter, see
// checkFirst) and snapshots whose stored form adoption does not know
// default to inline; the inline-path fallback handles the boot-strap case
// for detached-only upstreams. A snapshot that fails to load is an error
// (the final review's R1 #1: never a guess at the form).
func (c *Checker) detectForm(ctx context.Context, cur *cache.SuiteFreshness) (adoptionForm, error) {
	if cur.CurrentSnapshotID == nil {
		return adoptionFormInline, nil
	}
	snap, err := c.cache.GetSuiteSnapshot(ctx, *cur.CurrentSnapshotID)
	if err != nil {
		return 0, err
	}
	if form, ok := formOfSnapshot(snap); ok {
		return form, nil
	}
	return adoptionFormInline, nil
}

// checkEnd is how an answered check ended, for finishCheck: the
// freshness_check line's form, result, upstream status and (for
// changed_deferred and refusals) reason; whether the adoption failure
// record is cleared; and whether the current snapshot is signed (the
// scheduler's reserved-slot rule).
type checkEnd struct {
	form          string
	result        string
	status        int
	reason        string
	clearRecord   bool
	signedCurrent bool
	// anchorlessRows is set by the anchorless refresh: its line carries
	// rows_replaced and rows_deleted, the url_path rows it replaced and
	// deleted (the final review's tests lens M-4).
	anchorlessRows            bool
	rowsReplaced, rowsDeleted int
}

// finishCheck is the one tail of every answered check (ruling R-E3): it
// persists the row, clears the failure record when upstream serves the
// current content again (spec C7), logs freshness_check, counts it,
// reports it to the scheduler, and returns the repair signal of a
// confirmed-unchanged suite with a current snapshot.
func (c *Checker) finishCheck(ctx context.Context, out *checkOutcome, cur *cache.SuiteFreshness, suite SuiteRef, e checkEnd) *repairRequest {
	host, suitePath := suite.CanonicalHost, suite.SuitePath
	if perr := c.cache.UpdateSuiteFreshness(ctx, *cur); perr != nil {
		c.logger.Warn("freshness: persist success failed",
			"err", perr,
			"canonical_host", host,
			"suite_path", suitePath,
		)
	}
	if e.clearRecord {
		if err := c.cache.ClearAdoptionFailure(ctx, suite.CanonicalScheme, host, suitePath); err != nil {
			c.logger.Warn("freshness_clear_adoption_failure_failed",
				"err", err, "canonical_host", host, "suite_path", suitePath)
		}
	}
	logAttrs := []any{
		"canonical_host", host,
		"suite_path", suitePath,
		"form", e.form,
		"result", e.result,
		"upstream_status", e.status,
	}
	if e.reason != "" {
		logAttrs = append(logAttrs, "reason", e.reason)
	}
	if e.anchorlessRows {
		logAttrs = append(logAttrs, "rows_replaced", e.rowsReplaced, "rows_deleted", e.rowsDeleted)
	}
	out.report = checkReport{result: e.result, signedCurrent: e.signedCurrent}
	// Ruling PE-10 (b): the answer the next check is paced by
	// (pacedAbsentAnswer).
	if key := suiteKey(suite.CanonicalScheme, host, suitePath); e.result == "anchor_absent" {
		c.absentAnswered.Store(key, struct{}{})
	} else {
		c.absentAnswered.Delete(key)
	}
	c.logger.Info("freshness_check", logAttrs...)
	freshnessCheckTotal.Inc(e.result, host)
	rp := repairIfUnchanged(e.result, suite, cur)
	if rp != nil {
		rp.signed = e.signedCurrent
	}
	return rp
}

// failCheck is the bookkeeping of a check that ended unknown (spec C4): a
// Conditional error, the check deadline hit while waiting for the host
// slot, or a success status other than 200 and 304 (a fetch.Conditional
// contract violation; ruling R-E3). SPEC §7.2: last_check_at is bumped to
// space out the next attempt and the streak extends; last_success_at is
// left alone — it carries the "we know upstream is fine" signal that
// periodic_refresh uses. It logs "freshness check failed" and counts
// acu_freshness_check_total{result="failed"}, and reports failed.
//
// AIDEV-NOTE: ctx is the caller's context, never fetchCtx: the deadline
// that ended the check must not fail the write that records it.
// AIDEV-NOTE: with an adopter wired, classify sends 401/403/404/410 and a
// 200 failing its sniff to absent before this point (ruling R-E4: an
// absent anchor resets the streak, so an anchor_absent suite never
// inherits the hourly backoff); only unknown ends and checks that find
// nothing they may adopt (no anchor at all) fail here. An unsigned
// candidate while adoption.adopt_unsigned is off is the refusal state
// unsigned_disabled, not a failed check (ruling PE-12, unsignedRefusal).
// Without an adopter (Phase 1) every Conditional error still extends the
// streak, as before phase E.
func (c *Checker) failCheck(ctx context.Context, out *checkOutcome, cur *cache.SuiteFreshness, suite SuiteRef, nowUnix int64, form string, err error) {
	host, suitePath := suite.CanonicalHost, suite.SuitePath
	c.absentAnswered.Delete(suiteKey(suite.CanonicalScheme, host, suitePath)) // ruling PE-10 (b)
	cur.LastCheckAt = &nowUnix
	cur.CheckFailCount++
	if perr := c.cache.UpdateSuiteFreshness(ctx, *cur); perr != nil {
		c.logger.Warn("freshness: persist failure-bump failed",
			"err", perr,
			"canonical_host", host,
			"suite_path", suitePath,
		)
	}
	c.logger.Info("freshness check failed",
		"err", err,
		"canonical_host", host,
		"suite_path", suitePath,
		"form", form,
		"result", "failed",
	)
	freshnessCheckTotal.Inc("failed", host)
	out.report.result = "failed"
}

// endedByShutdown reports whether a check step failed because the
// caller's context ended (shutdown), and then logs it at DEBUG: it says
// nothing about upstream, so the check returns without writing — no
// failCheck, no WARN, no failed-check count — as a cancelled GET does
// (t.end.cancelled(), "freshness_check_cancelled").
//
// AIDEV-NOTE: final fix wave FW-3 (d) (final concurrency review M-1): the
// local steps between the probes and the tail — the checker's marker
// write, the unsigned eligibility read, the current snapshot's read —
// failed the check on a cancelled context, so a routine restart under
// client traffic logged INFO "freshness check failed", WARN "freshness:
// persist failure-bump failed" and a failed check. ctx is the caller's
// context, not fetchCtx: the check deadline is no shutdown and still
// fails the check.
func (c *Checker) endedByShutdown(ctx context.Context, suite SuiteRef, step string, err error) bool {
	if ctx.Err() == nil {
		return false
	}
	c.logger.Debug("freshness_check_ended_by_shutdown", "step", step, "err", err,
		"canonical_scheme", suite.CanonicalScheme, "canonical_host", suite.CanonicalHost, "suite_path", suite.SuitePath)
	return true
}

// errUnexpectedStatus is a success status other than 200 and 304 from
// fetch.Conditional: a contract violation, a failed check (ruling R-E3).
var errUnexpectedStatus = errors.New("unexpected success status")

// errUnconditionalNotModified is a 304 answering a current form's GET
// that carried no validators: unknown (spec C4 classification, design note
// 21), a failed check.
var errUnconditionalNotModified = errors.New("304 Not Modified to a request without validators")

// unusableStatus returns the failed-check error for a current form's GET
// whose success status the check cannot use: anything but 200 and 304
// (ruling R-E3), and — with an adopter wired — a 304 to a request that
// carried no validators (conditional false). nil otherwise.
//
// AIDEV-NOTE: final fix wave FW-3 (c) (final spec review M5 (c)): spec C4
// makes a 304 present only as the answer to a conditional request.
// Taking an unconditional 304 as not_modified bumped last_success_at, so
// an upstream that answers 304 to unconditional GETs (sent when no
// validators are stored, or the record is due) froze the suite silently.
// Phase 1 (no adopter) runs unchanged, as spec C4 Dispatch requires.
func (c *Checker) unusableStatus(res *fetch.ConditionalResult, conditional bool) error {
	switch {
	case res.Status != 200 && res.Status != 304:
		return fmt.Errorf("%w %d", errUnexpectedStatus, res.Status)
	case res.Status == 304 && !conditional && c.adopter != nil:
		return errUnconditionalNotModified
	}
	return nil
}

// steadyResult handles, for every form (ruling R-E3: one 304/unchanged
// helper), the responses that say upstream still serves what the suite
// has: a 304 — which, when the failure record's own validators were sent
// (retryGate.recordValidators), says upstream still serves the failing
// content (spec C7 changed_deferred), and otherwise that it still serves
// the current anchor — and a 200 carrying the current anchor's bytes
// (newHash == cachedHash), whose validators are taken (the only response
// they are ever taken from, spec C4) and which clears a failure record
// (spec C7). ok is false for any other response.
func steadyResult(cur *cache.SuiteFreshness, res *fetch.ConditionalResult, newHash, cachedHash string, gate retryGate, nowUnix int64) (result string, clearRecord, ok bool) {
	switch {
	case res.Status == 304 && gate.recordValidators():
		setChangeSeen(cur, nowUnix)
		return "changed_deferred", false, true
	case res.Status == 304:
		// Upstream serves the cached anchor again; clear the pending
		// diagnostic (e.g. after an upstream rollback).
		cur.InReleaseChangeSeenAt = nil
		return "not_modified", false, true
	case res.Status == 200 && newHash == cachedHash:
		if res.ETag != "" {
			v := res.ETag
			cur.InReleaseETag = &v
		}
		if res.LastModified != "" {
			v := res.LastModified
			cur.InReleaseLastMod = &v
		}
		cur.InReleaseChangeSeenAt = nil
		return "unchanged", gate.active, true
	}
	return "", false, false
}

// baseline is what a check compares the current form's anchor file name
// ("InRelease" or "Release") with: the current snapshot's hash of that
// anchor — through the snapshot's form (cache.SuiteSnapshot.AnchorHash),
// since an unsigned snapshot keeps its Release in the single-anchor slot —
// else the url_path anchor's blob; and whether the current snapshot is
// signed (the reserved-slot rule, spec C4). hash is "" when there is no
// baseline: the check skips without a write (logged at DEBUG).
//
// AIDEV-NOTE: the snapshot is authoritative once a suite is adopted:
// comparing with the url_path blob instead would see "changed" after
// every adoption and thrash the candidate-uniqueness constraint.
func (c *Checker) baseline(ctx context.Context, cur *cache.SuiteFreshness, suite SuiteRef, row *cache.URLPath, name string) (hash string, signedCurrent bool) {
	if cur.CurrentSnapshotID != nil {
		snap, err := c.cache.GetSuiteSnapshot(ctx, *cur.CurrentSnapshotID)
		if err != nil {
			c.logger.Warn("freshness: snapshot lookup failed; falling back to url_path",
				"err", err,
				"snapshot_id", *cur.CurrentSnapshotID,
				"canonical_host", suite.CanonicalHost,
				"suite_path", suite.SuitePath,
			)
		} else {
			signedCurrent = signedForm(snap.Form)
			if h := snap.AnchorHash(name); h != nil {
				hash = *h
			}
		}
	}
	if hash == "" && row.BlobHash != nil {
		hash = *row.BlobHash
	}
	if hash == "" {
		c.logger.Debug("freshness_check_skipped_no_baseline_anchor_hash",
			"canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath,
			"anchor", name,
		)
	}
	return hash, signedCurrent
}

// signedForm reports whether form is a signed anchor form (inline or
// detached): what the reserved-slot rule (spec C4) and the scheduler's
// signed flag ask of a current snapshot. An unknown or empty form is not
// signed (deferred minor E-31: the one predicate; Reconcile used
// "not unsigned").
func signedForm(form cache.AnchorForm) bool {
	return form == cache.FormInline || form == cache.FormDetached
}

// admit takes an admission slot for req (spec C4: TryAcquire before
// spawning, never wait) and returns req with the result to log: result
// when a slot was free (out.lease holds it), or queued with no request
// when none was — the suite's last_success_at then keeps its previous
// value (prevSuccess). With no adopter wired nothing is admitted and req
// is returned as it is (Phase 1 observes and adopts nothing).
//
// AIDEV-NOTE: a queued result is what the scheduler parks a suite on
// (isQueuedResult). A client check's queued result leaves the change
// pending (change seen, no record), which makes the suite due at the next
// tick (Checker.pendingChange); that tick's check bypasses the cooldown
// and parks it.
func (c *Checker) admit(out *checkOutcome, cur *cache.SuiteFreshness, req *adoptionRequest, signedCurrent bool,
	prevSuccess *int64, result, queued string) (*adoptionRequest, string) {
	if c.adopter == nil {
		return req, result
	}
	l, ok := c.admission.tryAcquire(req.suite.CanonicalHost, signedCurrent)
	if !ok {
		cur.LastSuccessAt = prevSuccess
		return nil, queued
	}
	out.lease = l
	return req, result
}

// bodyHash is the sha256 hex of a 200 response's body ("" for a 304).
func bodyHash(res *fetch.ConditionalResult) string {
	if res.Status != 200 {
		return ""
	}
	sum := sha256.Sum256(res.Body)
	return hex.EncodeToString(sum[:])
}

// checkInline runs the conditional GET against InRelease and
// processes the result. Returns:
//   - (req, repair, false) once the check has ended (req is nil when
//     nothing is to be adopted, repair nil unless a member repair is
//     planned).
//   - (nil, nil, true) when the inline path got 404 AND the suite has no
//     current snapshot AND a Release url_path row exists — the
//     dispatcher then retries via checkDetached. We return
//     without persisting in the fallback case so the detached
//     attempt's persist is the only one that runs.
//
// The body of this function is the SPEC §7.2 inline algorithm; it has
// been factored out of checkLocked to give detached mode (SPEC2 §7.6.3)
// a parallel implementation. Mutates *cur in the non-fallback paths.
func (c *Checker) checkInline(ctx, fetchCtx context.Context, out *checkOutcome, cur *cache.SuiteFreshness, suite SuiteRef, now time.Time, nowUnix int64) (*adoptionRequest, *repairRequest, bool) {
	scheme, host, suitePath := suite.CanonicalScheme, suite.CanonicalHost, suite.SuitePath

	// Locate the InRelease url_path row to get the upstream URL plus
	// (Phase 1 fallback) the cached blob hash for byte-equality
	// comparison on 200.
	inReleasePath := suitePath + inReleaseFilename
	urlRow, err := c.cache.LookupURL(ctx, scheme, host, inReleasePath)
	switch {
	case errors.Is(err, cache.ErrNotFound):
		// Suite has freshness state but no cached InRelease url_path row.
		if cur.CurrentSnapshotID == nil {
			// First-ever / never-adopted suite. The detached fallback
			// path triggers on 404 from the upstream's /InRelease, which
			// requires us to actually issue the GET. If we don't even
			// have a url_path row to attempt the GET, try the symmetric
			// detached lookup directly.
			if c.hasReleaseURLRow(ctx, scheme, host, suitePath) {
				return nil, nil, true
			}
			c.logger.Debug("freshness: no cached InRelease url_path, skipping",
				"canonical_host", host,
				"suite_path", suitePath,
			)
			return nil, nil, false
		}
		// SPEC2 §7.4 freshness recovery. The suite HAS a current
		// snapshot but its InRelease anchor row was reaped (SPEC4 §5 GC
		// TTL during a low-traffic request lull). Without this branch
		// the check dead-ends here and the suite freezes forever: it
		// keeps serving the stale snapshot from snapshot_member and
		// never observes upstream rolling forward — the production
		// freeze trap. Reconstruct the upstream URL from the suite
		// identity, re-seed the anchor (pinned to the snapshot's adopted
		// hash so the SPEC4 §5 guards re-protect it), and fall through
		// to the conditional GET with the synthesized row.
		anchorHash := c.snapshotAnchorHash(ctx, *cur.CurrentSnapshotID, inReleaseFilename)
		urlRow = c.reconstructAnchor(ctx, scheme, host, suitePath, inReleasePath, *cur.CurrentSnapshotID, anchorHash, nowUnix)
	case err != nil:
		c.logger.Warn("freshness: lookup InRelease url_path failed",
			"err", err,
			"canonical_host", host,
			"suite_path", suitePath,
		)
		return nil, nil, false
	}

	cachedHash, signedCurrent := c.baseline(ctx, cur, suite, urlRow, "InRelease")
	if cachedHash == "" {
		return nil, nil, false
	}

	target := &fetch.Target{
		CanonicalHost: host,
		URL:           urlRow.UpstreamURL,
	}
	gate := c.retryGateFor(cur, now)
	c.applyClamp(ctx, cur, gate, now)
	etag, lastmod := gate.validators(cur)

	// SPEC §9.3: bound concurrent upstream calls to host. Without
	// this, distinct suites on the same host all run Conditional in
	// parallel, each capable of pulling MaxInReleaseBytes into
	// memory — a resource-exhaustion path under a refresh storm or
	// an adversarial allowlisted upstream. Sharing the limiter with
	// the handler's miss path means cache-miss fetches and
	// freshness checks contend for the same per-host budget.
	release, err := c.hostSem.Acquire(fetchCtx, host)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			c.logger.Debug("freshness: host limiter acquire aborted",
				"err", err,
				"canonical_host", host,
				"suite_path", suitePath,
			)
			return nil, nil, false
		}
		// Spec C4: the check deadline hit while waiting for the slot.
		c.failCheck(ctx, out, cur, suite, nowUnix, "inline", err)
		return nil, nil, false
	}
	defer release()

	res, err := c.fetcher.Conditional(fetchCtx, target, etag, lastmod, c.maxBody)
	if c.adopter != nil && cur.CurrentSnapshotID != nil {
		if o := classify(res, err, sniffInRelease, etag != "" || lastmod != ""); o.class == classAbsent {
			return c.anchorAbsent(ctx, fetchCtx, out, cur, suite, now, nowUnix, adoptionFormInline, o, gate, signedCurrent), nil, false
		}
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// A cancelled caller (shutdown) is not evidence about upstream;
			// the check deadline (DeadlineExceeded) is, and counts below.
			c.logger.Debug("freshness_check_cancelled",
				"canonical_scheme", suite.CanonicalScheme, "canonical_host", host, "suite_path", suitePath)
			return nil, nil, false
		}
		// 404 with no current snapshot AND a Release url_path row →
		// fall back to detached without persisting. The detached
		// attempt becomes the authoritative result for this Check.
		if cur.CurrentSnapshotID == nil && isStatusNotFound(err) &&
			c.hasReleaseURLRow(ctx, scheme, host, suitePath) {
			c.logger.Debug("freshness: InRelease 404 on first-ever check, falling back to detached",
				"canonical_host", host,
				"suite_path", suitePath,
			)
			return nil, nil, true
		}
		c.failCheck(ctx, out, cur, suite, nowUnix, "inline", err)
		return nil, nil, false
	}
	if serr := c.unusableStatus(res, etag != "" || lastmod != ""); serr != nil {
		// fetch.Conditional answers only 200 or 304 on success: anything
		// else is a contract violation, a failed check (ruling R-E3); so
		// is a 304 to a request without validators (FW-3 (c)).
		c.failCheck(ctx, out, cur, suite, nowUnix, "inline", serr)
		return nil, nil, false
	}

	prevSuccess := cur.LastSuccessAt
	cur.LastCheckAt = &nowUnix
	cur.LastSuccessAt = &nowUnix
	cur.CheckFailCount = 0

	// SPEC §10: every freshness attempt emits a structured log line. The
	// `result` enum is what operators pivot on — success-with-no-change
	// (the steady state) vs. success-with-change (the interesting one).
	newHash := bodyHash(res)
	e := checkEnd{form: "inline", status: res.Status, signedCurrent: signedCurrent}
	var req *adoptionRequest
	if result, clearRec, ok := steadyResult(cur, res, newHash, cachedHash, gate, nowUnix); ok {
		e.result, e.clearRecord = result, clearRec
	} else if gate.holds(newHash) {
		// An upstream that ignores validators re-served the failing
		// bytes: deferred, as steadyResult defers a 304 to the record's
		// own validators.
		setChangeSeen(cur, nowUnix)
		e.result = "changed_deferred"
	} else {
		// Upstream has a new InRelease. Record the diagnostic
		// regardless of whether an Adopter is wired — operators
		// monitoring a Phase 1 deployment still see the
		// divergence.
		setChangeSeen(cur, nowUnix)
		c.logger.Info("InRelease changed at upstream",
			"canonical_host", host,
			"suite_path", suitePath,
			"cached_hash", cachedHash,
			"upstream_hash", newHash,
		)
		e.result = "changed"
		// The body is bounded by maxBody (§7.2 cap, default 4 MiB),
		// so retaining it through the goroutine handoff is acceptable.
		req = &adoptionRequest{
			suite:   suite,
			form:    adoptionFormInline,
			key:     newHash,
			bytes:   res.Body,
			etag:    res.ETag,
			lastmod: res.LastModified,
		}
		req, e.result = c.admit(out, cur, req, signedCurrent, prevSuccess, "changed", "changed_queued")
	}
	if e.result == "changed_deferred" {
		e.reason = gate.reason
	}
	return req, c.finishCheck(ctx, out, cur, suite, e), false
}

// checkDetached runs the SPEC2 §7.6.3 detached-form freshness
// check: a conditional GET on Release for change detection, plus a
// fresh GET on Release.gpg when Release has changed. Mutates *cur.
//
// Logically symmetric to checkInline. The differences:
//   - Two url_path rows are required (Release AND Release.gpg).
//     Either missing → skip.
//   - On observed change, fetch Release.gpg in a second call so the
//     Adopter can verify the detached signature.
//   - The adoptionRequest carries form=detached + both bodies.
//
// AIDEV-NOTE: a Release that changed costs one Release.gpg GET even when
// the result is then deferred (its content key covers the signature), and
// an upstream that ignores validators pays it on every deferred check
// (deferred minor B-25: an accepted cost). The Release.gpg GET runs under
// fetchCtx, the check deadline (TestCheck_DetachedSignatureStallEndsAtTheCheckDeadline).
func (c *Checker) checkDetached(ctx, fetchCtx context.Context, out *checkOutcome, cur *cache.SuiteFreshness, suite SuiteRef, now time.Time, nowUnix int64) (*adoptionRequest, *repairRequest) {
	scheme, host, suitePath := suite.CanonicalScheme, suite.CanonicalHost, suite.SuitePath

	releasePath := suitePath + releaseFilename
	releaseGPGPath := suitePath + releaseGPGFilename

	releaseURL, err := c.cache.LookupURL(ctx, scheme, host, releasePath)
	switch {
	case errors.Is(err, cache.ErrNotFound):
		// SPEC2 §7.4 freshness recovery (detached): an adopted suite
		// whose Release anchor was reaped must recover, not freeze —
		// symmetric to the inline path. First-ever suites (no snapshot)
		// still skip.
		if cur.CurrentSnapshotID == nil {
			c.logger.Debug("freshness: no cached Release url_path, skipping detached",
				"canonical_host", host,
				"suite_path", suitePath,
			)
			return nil, nil
		}
		relHash := c.snapshotAnchorHash(ctx, *cur.CurrentSnapshotID, releaseFilename)
		releaseURL = c.reconstructAnchor(ctx, scheme, host, suitePath, releasePath, *cur.CurrentSnapshotID, relHash, nowUnix)
	case err != nil:
		c.logger.Warn("freshness: lookup Release url_path failed",
			"err", err,
			"canonical_host", host,
			"suite_path", suitePath,
		)
		return nil, nil
	}
	releaseGPGURL, err := c.cache.LookupURL(ctx, scheme, host, releaseGPGPath)
	switch {
	case errors.Is(err, cache.ErrNotFound):
		if cur.CurrentSnapshotID == nil {
			c.logger.Debug("freshness: no cached Release.gpg url_path, skipping detached",
				"canonical_host", host,
				"suite_path", suitePath,
			)
			return nil, nil
		}
		gpgHash := c.snapshotAnchorHash(ctx, *cur.CurrentSnapshotID, releaseGPGFilename)
		releaseGPGURL = c.reconstructAnchor(ctx, scheme, host, suitePath, releaseGPGPath, *cur.CurrentSnapshotID, gpgHash, nowUnix)
	case err != nil:
		c.logger.Warn("freshness: lookup Release.gpg url_path failed",
			"err", err,
			"canonical_host", host,
			"suite_path", suitePath,
		)
		return nil, nil
	}

	cachedHash, signedCurrent := c.baseline(ctx, cur, suite, releaseURL, "Release")
	if cachedHash == "" {
		return nil, nil
	}

	target := &fetch.Target{
		CanonicalHost: host,
		URL:           releaseURL.UpstreamURL,
	}
	gate := c.retryGateFor(cur, now)
	c.applyClamp(ctx, cur, gate, now)
	etag, lastmod := gate.validators(cur)

	releaseSlot, err := c.hostSem.Acquire(fetchCtx, host)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			c.logger.Debug("freshness: host limiter acquire aborted",
				"err", err,
				"canonical_host", host,
				"suite_path", suitePath,
			)
			return nil, nil
		}
		// Spec C4: the check deadline hit while waiting for the slot.
		c.failCheck(ctx, out, cur, suite, nowUnix, "detached", err)
		return nil, nil
	}
	defer releaseSlot()

	lateralHeld := false
	if c.adopter != nil && cur.CurrentSnapshotID != nil {
		urls := c.anchorURLsFor(suite, map[string]string{"Release": releaseURL.UpstreamURL})
		req, done, held := c.lateralInline(ctx, fetchCtx, out, cur, suite, urls, gate, signedCurrent, nowUnix)
		if done {
			return req, nil
		}
		lateralHeld = held
	}

	res, err := c.fetcher.Conditional(fetchCtx, target, etag, lastmod, c.maxBody)
	if c.adopter != nil && cur.CurrentSnapshotID != nil {
		if o := classify(res, err, sniffRelease, etag != "" || lastmod != ""); o.class == classAbsent {
			return c.anchorAbsent(ctx, fetchCtx, out, cur, suite, now, nowUnix, adoptionFormDetached, o, gate, signedCurrent), nil
		}
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// A cancelled caller (shutdown) is not evidence about upstream;
			// the check deadline (DeadlineExceeded) is, and counts below.
			c.logger.Debug("freshness_check_cancelled",
				"canonical_scheme", suite.CanonicalScheme, "canonical_host", host, "suite_path", suitePath)
			return nil, nil
		}
		c.failCheck(ctx, out, cur, suite, nowUnix, "detached", err)
		return nil, nil
	}
	if serr := c.unusableStatus(res, etag != "" || lastmod != ""); serr != nil {
		c.failCheck(ctx, out, cur, suite, nowUnix, "detached", serr)
		return nil, nil
	}

	prevSuccess := cur.LastSuccessAt
	cur.LastCheckAt = &nowUnix
	cur.LastSuccessAt = &nowUnix
	cur.CheckFailCount = 0

	newHash := bodyHash(res)
	e := checkEnd{form: "detached", status: res.Status, signedCurrent: signedCurrent}
	var req *adoptionRequest
	if result, clearRec, ok := steadyResult(cur, res, newHash, cachedHash, gate, nowUnix); ok {
		// Upstream still serves what the suite has (steadyResult): a 304
		// — changed_deferred when the record's own validators were sent, a
		// re-signature behind them waiting for the retry (spec C7) — or a
		// 200 of the current Release. A record the lateral InRelease still
		// holds is not cleared by the detached anchor's "unchanged".
		e.result, e.clearRecord = result, clearRec && !lateralHeld
	} else {
		setChangeSeen(cur, nowUnix)
		// The content key covers the signature, so Release.gpg is fetched
		// before the gate can compare (no validators: the signature is
		// bound to this Release).
		gpgTarget := &fetch.Target{
			CanonicalHost: host,
			URL:           releaseGPGURL.UpstreamURL,
		}
		gpgRes, gerr := c.fetcher.Conditional(fetchCtx, gpgTarget, "", "", defaultMaxReleaseGPGBytes)
		var sig probeOutcome
		if c.adopter != nil && cur.CurrentSnapshotID != nil {
			sig = classify(gpgRes, gerr, sniffReleaseGPG, false)
		}
		switch {
		case sig.class == classAbsent:
			// Design review M-14: the changed Release has no signature —
			// absent (ruling R-E4), never adopted. The refusal state
			// anchor_absent makes it visible and paces the suite
			// (pacedByRefusal); a record the lateral InRelease still holds
			// keeps its own reason and schedule, and paces the suite the
			// same way (pacedAbsentAnswer, ruling PE-10). last_success_at
			// stays, as for an absent current anchor (spec C4).
			if !lateralHeld {
				c.recordAdoptionFailure(&adoptionRequest{suite: suite, key: anchorAbsentKey}, cache.ReasonAnchorAbsent)
			}
			cur.LastSuccessAt = prevSuccess
			e.result, e.reason, e.status = "anchor_absent", cache.ReasonAnchorAbsent, sig.status
		case gerr != nil || gpgRes.Status != 200:
			// As before: record the change and retry on a later check;
			// last_success_at stays bumped because Release answered.
			if gerr != nil {
				c.logger.Warn("freshness: Release.gpg fetch failed; deferring adoption",
					"err", gerr, "canonical_host", host, "suite_path", suitePath)
			} else {
				c.logger.Warn("freshness: Release.gpg fetch returned non-200; deferring adoption",
					"status", gpgRes.Status, "canonical_host", host, "suite_path", suitePath)
			}
			e.result = "changed"
		default:
			key := contentKey(adoptionFormDetached, nil, res.Body, gpgRes.Body)
			if gate.holds(key) {
				e.result = "changed_deferred"
				break
			}
			c.logger.Info("Release changed at upstream",
				"canonical_host", host,
				"suite_path", suitePath,
				"cached_hash", cachedHash,
				"upstream_hash", newHash,
			)
			req = &adoptionRequest{
				suite:        suite,
				form:         adoptionFormDetached,
				key:          key,
				releaseBytes: res.Body,
				sigBytes:     gpgRes.Body,
				etag:         res.ETag,
				lastmod:      res.LastModified,
			}
			req, e.result = c.admit(out, cur, req, signedCurrent, prevSuccess, "changed", "changed_queued")
		}
	}
	if e.result == "changed_deferred" {
		e.reason = gate.reason
	}
	return req, c.finishCheck(ctx, out, cur, suite, e)
}
