package freshness

// Spec C4 Dispatch 3: eager first adoption. A suite with no current
// snapshot adopts on its first check, in whichever anchor form its
// upstream publishes, when a client has asked for it recently; a dormant
// one costs no upstream request.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

const (
	// interestWindow is spec C4's eager-adoption interest window (a C11
	// constant, independent of the GC settings); the v7 backfill
	// (cache.interestWindowSeconds) uses the same 7 days.
	interestWindow = 7 * 24 * time.Hour

	// minConfirmGap is the floor of the unsigned transition's and the
	// absent anchor's confirmation gap, max(cooldown, 5 min) (spec C4, C5).
	minConfirmGap = 5 * time.Minute

	// deniedConfirmGap is the unsigned confirmation gap when a signature
	// file answered 401 or 403 (WAF and S3-permission incidents, spec C5
	// safeguard 2).
	deniedConfirmGap = time.Hour
)

// errNoAnchor ends the probe tree of a suite whose upstream has neither
// InRelease nor Release: a failed check (spec C4 Dispatch 3; the anchorless
// refresh of flat suites is phase G's).
var errNoAnchor = errors.New("no anchor: InRelease and Release are absent")

// confirmGap is max(cooldown, 5 min).
func (c *Checker) confirmGap() time.Duration { return max(c.cooldown, minConfirmGap) }

// interested reports spec C4's client interest: last_requested_at within
// the 7-day interest window of now, in either direction. future reports a
// stamp beyond the clock-step tolerance ahead of now (a backward clock
// step), which the check reports.
//
// AIDEV-NOTE: the window is symmetric (design review M-3). A stamp up to
// 7 d ahead keeps the suites that were interested before a backward step
// of at most the window; one further ahead is dormant until a client
// touches the suite again (TouchSuite rewrites the stamp with now, within
// its coalescing hour). Reading any future stamp as interested would turn
// every suite touched during a year-ahead boot into a first adoption.
func interested(lastRequested *int64, now time.Time) (ok, future bool) {
	if lastRequested == nil {
		return false, false
	}
	last := time.Unix(*lastRequested, 0)
	d := now.Sub(last)
	if d < 0 {
		d = -d
	}
	return d < interestWindow, last.After(now.Add(clockStepTolerance))
}

// pacedFirst is ruling PE-3 (design review I-4): a never-adopted suite
// whose failure or refusal record is active and not due, and whose last
// check is younger than periodic_refresh, is not probed — a client check
// (T1) inside that window asks nothing. The scheduler's stale check (the
// last check at least periodic_refresh old) and the due retry still run
// the probe tree, so new content of a failing suite is attempted within
// one refresh interval. The scheduler's slot-driven re-offer of a parked
// suite is exempt (checkFirst's offered; review fix round 1, Task 6). A
// last_check_at in the future reads as infinitely old (spec C4 clock-step
// rule: a gate that delays work never freezes on it); with periodic
// refresh off (0) nothing is paced.
//
// AIDEV-NOTE: the probe tree sends no validators (it must adopt an
// unchanged upstream, spec C4), so without this pacing every client check
// of a deferred suite downloaded its anchor in full — up to hundreds of
// MiB a day per suite for a missing vendor key. Never pace the re-offer:
// it runs seconds after the check that parked the suite (record still
// active and not due), and a paced re-offer ends skipped, which drops the
// suite from the waiting list (work-conserving admission, PE-4's "the
// offered re-check that admits it"). Re-offers are bounded by slot
// turnover, not by client traffic, so exempting them costs nothing PE-3
// was ruled to stop.
func (c *Checker) pacedFirst(cur *cache.SuiteFreshness, gate retryGate, now time.Time) bool {
	if !gate.active || gate.due || cur.LastCheckAt == nil {
		return false
	}
	last := time.Unix(*cur.LastCheckAt, 0)
	return !last.After(now.Add(clockStepTolerance)) && now.Sub(last) < c.refresh
}

// cachedAnchors reads suite's cached anchor url_path rows, by file name
// (anchorURLMap gives their upstream URLs), and whether a client created
// one of the InRelease or Release rows (request_count > 0; a reconstructed
// row has none, spec C4 "never reconstructed"). The rows are read before
// the probe tree runs: the anchorless refresh deletes an anchor only while
// it still holds the blob read here (design review M-4).
func (c *Checker) cachedAnchors(ctx context.Context, suite SuiteRef) (rows map[string]cache.URLPath, clientCreated bool, err error) {
	rows = make(map[string]cache.URLPath, 3)
	for _, name := range []string{"InRelease", "Release", "Release.gpg"} {
		row, err := c.cache.LookupURL(ctx, suite.CanonicalScheme, suite.CanonicalHost, suite.SuitePath+"/"+name)
		switch {
		case errors.Is(err, cache.ErrNotFound):
			continue
		case err != nil:
			return nil, false, fmt.Errorf("read %s: %w", name, err)
		}
		rows[name] = *row
		if name != "Release.gpg" && row.RequestCount > 0 {
			clientCreated = true
		}
	}
	return rows, clientCreated, nil
}

// anchorURLMap is the upstream URL of each of rows, by file name
// (anchorURLsFor's input).
func anchorURLMap(rows map[string]cache.URLPath) map[string]string {
	urls := make(map[string]string, len(rows))
	for name, row := range rows {
		urls[name] = row.UpstreamURL
	}
	return urls
}

// probeTree is spec C4's probe tree, every probe without validators:
// InRelease present → inline; absent → Release: present → Release.gpg:
// present → detached, absent → unsigned. found is false when InRelease and
// Release are both absent; end is set when a probe ended the tree unknown
// (or the caller was cancelled), with the name of its file.
type probeTree struct {
	form                    adoptionForm
	found                   bool
	inRelease, release, sig probeOutcome
	end                     *probeOutcome
	endName                 string
}

func (c *Checker) runProbeTree(fetchCtx context.Context, suite SuiteRef, urls anchorURLs) probeTree {
	var t probeTree
	stop := func(o *probeOutcome, name string) probeTree {
		t.end, t.endName = o, name
		return t
	}
	t.inRelease = c.probe(fetchCtx, suite, urls, "InRelease")
	switch t.inRelease.class {
	case classPresent:
		t.form, t.found = adoptionFormInline, true
		return t
	case classUnknown:
		return stop(&t.inRelease, "InRelease")
	}
	t.release = c.probe(fetchCtx, suite, urls, "Release")
	switch t.release.class {
	case classAbsent:
		return t
	case classUnknown:
		return stop(&t.release, "Release")
	}
	t.sig = c.probe(fetchCtx, suite, urls, "Release.gpg")
	switch t.sig.class {
	case classPresent:
		t.form, t.found = adoptionFormDetached, true
	case classAbsent:
		t.form, t.found = adoptionFormUnsigned, true
	default:
		return stop(&t.sig, "Release.gpg")
	}
	return t
}

// request builds the adoption request for the form the tree found. The
// validators are the anchor's own (Release's for the detached and unsigned
// forms): the flip transaction stores them as the suite's (spec C4
// "Validators").
func (t probeTree) request(suite SuiteRef) *adoptionRequest {
	switch t.form {
	case adoptionFormDetached:
		rel, sig := t.release.res, t.sig.res
		return &adoptionRequest{suite: suite, form: adoptionFormDetached,
			key:          contentKey(adoptionFormDetached, nil, rel.Body, sig.Body),
			releaseBytes: rel.Body, sigBytes: sig.Body, etag: rel.ETag, lastmod: rel.LastModified}
	case adoptionFormUnsigned:
		rel := t.release.res
		return &adoptionRequest{suite: suite, form: adoptionFormUnsigned,
			key:          contentKey(adoptionFormUnsigned, nil, rel.Body, nil),
			releaseBytes: rel.Body, etag: rel.ETag, lastmod: rel.LastModified}
	}
	b := t.inRelease.res
	return &adoptionRequest{suite: suite, form: adoptionFormInline,
		key: contentKey(adoptionFormInline, b.Body, nil, nil), bytes: b.Body, etag: b.ETag, lastmod: b.LastModified}
}

// signatureDenied reports a 401/403 on either signature file probed.
func (t probeTree) signatureDenied() bool { return t.inRelease.authDenied() || t.sig.authDenied() }

// signatureSeen reports a signature file probed present: InRelease (its
// clearsign envelope passed the sniff) or Release.gpg.
func (t probeTree) signatureSeen() bool {
	return t.inRelease.class == classPresent || t.sig.class == classPresent
}

// checkFirst is spec C4 Dispatch 3 (adopter wired, no current snapshot). A
// suite without client interest in the last 7 days, or without a
// client-created InRelease or Release url_path row — or, for a flat suite,
// root Packages*/Sources* row (Adopter.flatIndexClientCreated) — is
// dormant: no request and no write. The scheduler skips only a suite
// without interest (suiteDue): one with interest but no client-created row
// is queued at every tick and ends here, dormant, after its row reads
// (deferred minor E-25; a flat suite's reads include its index rows). A
// suite whose record is active and not due is paced (pacedFirst): no
// request between the scheduler's refresh checks, except the scheduler's
// slot-driven re-offer of the parked suite (offered: the check's
// bypassCooldown, which the scheduler sets for a waiting item and for a
// pending-change item; the latter has no record when it is queued, so it is
// not paced: at most one probe more, when a client check's failed adoption
// recorded one between the enqueue and the dequeue; deferred minor E-34).
// Otherwise the probe tree runs without validators and the form found is
// adopted — result first_adoption, or first_adoption_queued with no slot —
// through the spec C7 gate (content keys only: the probes carry no
// validators) and, for the unsigned form, spec C5's eligibility, member cap
// and transition confirmation (unsignedFirst). A probe that finds a
// signature marks the suite's base first (Adopter.markSignatureSeen; a
// failed mark fails the check). A probe that ends unknown and a tree that
// finds no anchor (which also records anchor_absent, so the status page
// shows it: ruling PE-2) are failed checks — except that a flat suite whose
// tree finds no anchor and that has no signed evidence
// (Adopter.anchorlessEligible) runs the anchorless refresh instead
// (anchorlessRefresh, spec C4 Dispatch 4), or, while
// adoption.adopt_unsigned is off, is refused unsigned_disabled
// (anchorlessDisabled, ruling PG-1); an unsigned candidate while
// adoption.adopt_unsigned is off is the refusal unsigned_disabled, recorded
// and paced (ruling PE-12). A read error of the anchor rows is logged at
// WARN with no write and no backoff: a local fault, retried at the next
// check (at most one per cooldown and per tick); so is a read error of a
// flat suite's index rows, before the probe tree or in the anchorless
// refresh. A suite with no cached anchor row but a client-created flat
// index row is probed beside the index row's stored upstream_url (ruling
// PG-20).
//
// AIDEV-NOTE: a first or unsigned request passes signedCurrent = false to
// admit, so it never takes the last free slot that the reserved-slot rule
// keeps for suites with a signed current snapshot (spec C4). The
// freshness_check line of a tree result carries upstream_status 200, the
// status of the anchor the form was found by; the absent probes before it
// are not logged.
func (c *Checker) checkFirst(ctx, fetchCtx context.Context, out *checkOutcome, cur *cache.SuiteFreshness, suite SuiteRef, now time.Time, nowUnix int64, offered bool) *adoptionRequest {
	host, suitePath := suite.CanonicalHost, suite.SuitePath
	ok, future := interested(cur.LastRequestedAt, now)
	if future {
		c.clockStep("last_requested_at", suite.CanonicalScheme, host, suitePath, *cur.LastRequestedAt, now)
	}
	if !ok {
		c.logger.Debug("freshness: never-adopted suite without recent interest, dormant",
			"canonical_host", host, "suite_path", suitePath)
		return nil
	}
	anchors, clientCreated, err := c.cachedAnchors(ctx, suite)
	var index *cache.URLPath // a client-created flat index row (Dispatch 3)
	if err == nil && !clientCreated && aptpath.IsFlatSuitePath(suitePath) {
		index, err = c.adopter.flatIndexClientCreated(ctx, suite)
		clientCreated = index != nil
	}
	if err != nil {
		c.logger.Warn("freshness: read anchor rows failed",
			"err", err, "canonical_host", host, "suite_path", suitePath)
		return nil
	}
	if !clientCreated {
		c.logger.Debug("freshness: never-adopted suite without a client-created anchor or flat index row, dormant",
			"canonical_host", host, "suite_path", suitePath)
		return nil
	}

	gate := c.retryGateFor(cur, now)
	c.applyClamp(ctx, cur, gate, now)
	if !offered && c.pacedFirst(cur, gate, now) {
		c.logger.Debug("freshness: deferred never-adopted suite paced until its retry or the next refresh check",
			"canonical_host", host, "suite_path", suitePath, "reason", gate.reason)
		return nil
	}

	release, err := c.hostSem.Acquire(fetchCtx, host)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			c.logger.Debug("freshness: host limiter acquire aborted",
				"err", err, "canonical_host", host, "suite_path", suitePath)
			return nil
		}
		c.failCheck(ctx, out, cur, suite, nowUnix, "probe", err)
		return nil
	}
	defer release()

	urls := c.anchorURLsFor(suite, anchorURLMap(anchors))
	if urls.model == "" && index != nil {
		// Ruling PG-20 (spec C1): a suite active only through its
		// client-fetched flat index is probed beside that row's stored
		// upstream_url, which keeps a non-default port the port-less
		// canonical host (and so urlReconstruct) drops.
		urls.model, urls.modelName = index.UpstreamURL, strings.TrimPrefix(index.Path, suitePath+"/")
	}
	t := c.runProbeTree(fetchCtx, suite, urls)
	switch {
	case t.end != nil && t.end.cancelled():
		c.logger.Debug("freshness: check cancelled", "canonical_host", host, "suite_path", suitePath)
		return nil
	case t.end != nil:
		c.failCheck(ctx, out, cur, suite, nowUnix, "probe", fmt.Errorf("%s: %w", t.endName, t.end.err))
		return nil
	case !t.found:
		// Spec C4 Dispatch 3: a flat suite without signed evidence is a
		// Release-less repository, refreshed anchorless (Dispatch 4). Ruling
		// PG-1: the knob stops the anchorless refresh as it stops an
		// unsigned adoption — refused unsigned_disabled, a successful check
		// paced by its record (pacedFirst), no conditional GET and no
		// delete. The gate runs first, so a suite seen signed is the
		// anchor_absent failure below whatever the knob.
		if aptpath.IsFlatSuitePath(suitePath) {
			eligible, err := c.adopter.anchorlessEligible(ctx, suite)
			if err != nil {
				if !c.endedByShutdown(ctx, suite, "the anchorless gate", err) {
					c.failCheck(ctx, out, cur, suite, nowUnix, "anchorless", err)
				}
				return nil
			}
			switch {
			case eligible && !c.adopter.AdoptsUnsigned():
				c.anchorlessDisabled(ctx, out, cur, suite, nowUnix, t)
				return nil
			case eligible:
				c.anchorlessRefresh(ctx, fetchCtx, out, cur, suite, nowUnix, t, anchors)
				return nil
			}
		}
		// Ruling PE-2: still a failed check (spec C4; the streak backs
		// off), and the refusal state anchor_absent shows it on the status
		// page; the record also paces the suite (pacedFirst).
		c.recordAdoptionFailure(&adoptionRequest{suite: suite, key: anchorAbsentKey}, cache.ReasonAnchorAbsent)
		c.failCheck(ctx, out, cur, suite, nowUnix, "probe", errNoAnchor)
		return nil
	}
	if t.signatureSeen() {
		if err := c.adopter.markSignatureSeen(ctx, suite, nowUnix); err != nil {
			if !c.endedByShutdown(ctx, suite, "mark the signature seen", err) {
				c.failCheck(ctx, out, cur, suite, nowUnix, "probe", fmt.Errorf("mark the signature seen: %w", err))
			}
			return nil
		}
	}
	req := t.request(suite)
	// Spec C7: the same content as the record's, before its retry time.
	deferred, reason := gate.active && !gate.due && req.key == gate.hash, gate.reason
	if !deferred && t.form == adoptionFormUnsigned {
		proceed, refusal, err := c.unsignedFirst(ctx, cur, req, t, gate, now)
		if err != nil {
			if !c.endedByShutdown(ctx, suite, "unsigned eligibility", err) {
				c.failCheck(ctx, out, cur, suite, nowUnix, "unsigned", err)
			}
			return nil
		}
		deferred, reason = !proceed, refusal
	}

	prevSuccess := cur.LastSuccessAt
	cur.LastCheckAt, cur.LastSuccessAt, cur.CheckFailCount = &nowUnix, &nowUnix, 0
	setChangeSeen(cur, nowUnix)
	e := checkEnd{form: formName(t.form), status: 200}
	if deferred {
		e.result, e.reason, req = "changed_deferred", reason, nil
	} else {
		req, e.result = c.admit(out, cur, req, false, prevSuccess, "first_adoption", "first_adoption_queued")
	}
	c.finishCheck(ctx, out, cur, suite, e)
	return req
}

// unsignedFirst applies spec C5's safeguards to an unsigned candidate of
// a never-adopted suite, before a slot is taken: the knob (off: refused,
// recorded as unsigned_disabled, ruling PE-12), never-downgrade (signed
// history, see Adopter.unsignedEligible: refused, recorded as
// unsigned_downgrade_refused), the member cap (unsigned_too_large; design
// review M-5: a too-large suite never takes a slot) and the transition
// confirmation — two observations of the same Release bytes at least
// confirmGap apart, an hour when a signature file answered 401/403
// (unsigned_unconfirmed until then; adoption_fail_first_at, the
// confirmation stamp, restarts whenever the bytes or the reason change,
// and the retry count only with the reason — restartsStreak, final fix
// wave FW-3 (a)). proceed is true for a confirmed candidate; otherwise
// reason is the refusal just recorded.
//
// AIDEV-NOTE: content that already passed confirmation (a record for the
// same bytes from an adoption attempt: any reason but unsigned_unconfirmed,
// unsigned_downgrade_refused and unsigned_disabled — refusals decided
// before the confirmation, ruling PE-12 adding the last) is not confirmed
// again, so a failing unsigned first adoption keeps the C7 schedule
// instead of restarting its confirmation per attempt; bytes refused while
// the knob was off start their confirmation when it is turned back on. A
// future adoption_fail_first_at never confirms (its age is negative) and
// re-recording restarts the stamp (recordAdoptionFailure's past-event
// rule): a deviation from the C4 clock-step rule, which would read it as
// infinitely old and skip the confirmation.
func (c *Checker) unsignedFirst(ctx context.Context, cur *cache.SuiteFreshness, req *adoptionRequest, t probeTree, gate retryGate, now time.Time) (proceed bool, reason string, err error) {
	if refusal, err := c.unsignedRefusal(ctx, req); err != nil || refusal != "" {
		return false, refusal, err
	}
	sameContent := gate.active && gate.hash == req.key
	if sameContent && !refusedBeforeConfirmation(gate.reason) {
		return true, "", nil
	}
	gap := c.confirmGap()
	if t.signatureDenied() {
		gap = deniedConfirmGap
	}
	if sameContent && gate.reason == cache.ReasonUnsignedUnconfirmed && cur.AdoptionFailFirstAt != nil &&
		now.Sub(time.Unix(*cur.AdoptionFailFirstAt, 0)) >= gap {
		return true, "", nil
	}
	c.recordAdoptionFailure(req, cache.ReasonUnsignedUnconfirmed)
	return false, cache.ReasonUnsignedUnconfirmed, nil
}

// refusedBeforeConfirmation reports a record reason decided before the
// unsigned transition's confirmation ran, so its bytes were never
// confirmed (design note 17): unsigned_unconfirmed itself, and the
// eligibility refusals unsigned_downgrade_refused and unsigned_disabled
// (ruling PE-12).
func refusedBeforeConfirmation(reason string) bool {
	switch reason {
	case cache.ReasonUnsignedUnconfirmed, cache.ReasonUnsignedDowngradeRefused, cache.ReasonUnsignedDisabled:
		return true
	}
	return false
}
