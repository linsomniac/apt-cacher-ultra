package freshness

// Spec C4 for flat suites: Dispatch 3's client-created index rows, and
// (Dispatch 4) the anchorless refresh of a flat, Release-less suite.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
)

// suiteKeyOf is the cache key of suite.
func suiteKeyOf(suite SuiteRef) cache.SuiteKey {
	return cache.SuiteKey{CanonicalScheme: suite.CanonicalScheme, CanonicalHost: suite.CanonicalHost, SuitePath: suite.SuitePath}
}

// flatIndexClientCreated returns a client-created root Packages* or
// Sources* url_path row of flat suite (aptpath.FileFlatIndex,
// request_count > 0), nil when it has none: spec C4 Dispatch 3 counts it
// beside the InRelease and Release rows, so a Release-less repository's
// suite — created by its index's serve — is not dormant (the E
// carry-forward; plan design note 8). A row hot prefetch or a
// reconstruction wrote has no request count. The row is returned, not a
// bool, because its stored upstream_url is the probe tree's URL model when
// no anchor row is cached (ruling PG-20, checkFirst).
func (a *Adopter) flatIndexClientCreated(ctx context.Context, suite SuiteRef) (*cache.URLPath, error) {
	rows, err := a.cache.ListSuiteMetadataRows(ctx, suiteKeyOf(suite))
	if err != nil {
		return nil, fmt.Errorf("read flat index rows: %w", err)
	}
	for i, r := range rows {
		if r.RequestCount > 0 && aptpath.ClassifySuiteFile(suite.SuitePath, r.Path) == aptpath.FileFlatIndex {
			return &rows[i], nil
		}
	}
	return nil, nil
}

// maxAnchorlessIndexBytes is the anchorless refresh's body cap (plan
// design note 12): fetch.Conditional reads a body into memory, and the
// Release-less repositories publish indexes of kilobytes to a few MiB. A
// body over it keeps the row (no content answer).
const maxAnchorlessIndexBytes = 64 << 20

// anchorlessBodySlots is how many anchorless bodies may be in memory at
// once, process-wide (plan design note 12, ruling PG-8): each anchorless
// conditional GET takes one of Checker.anchorlessBodies' slots.
const anchorlessBodySlots = 2

// anchorlessOverCapWarnEvery rate-limits the WARN "freshness: anchorless
// index over its size cap": at most one line per minute process-wide, with
// the count of the lines it suppressed (design review M-6; the handler's
// flat_host_root_unsupported does the same). The limiter is
// Checker.anchorlessOverCap, an observability.LogLimiter (ruling PG-22).
const anchorlessOverCapWarnEvery = time.Minute

// errAnchorlessUnknown fails an anchorless check whose index requests got
// no content answer.
var errAnchorlessUnknown = errors.New("every anchorless index request ended unknown")

// anchorlessEligible is spec C4 Dispatch 3's gate in front of the
// anchorless refresh (plan design note 9, deviation 1): a flat suite whose
// probe tree found neither InRelease nor Release refreshes anchorless only
// with no signed evidence. The cached signed-anchor re-mark runs first
// (remarkSignedAnchors: each sniff-passing InRelease or Release.gpg row of
// the base or its /. twin, under either scheme, marks the base), then
// never-downgrade's rule (cache.CheckUnsignedDowngrade: a marker for the
// base, or an adopted signed snapshot of the base or its twin) must find
// nothing. An error — a read, a pool file, the mark — is returned: the
// caller fails the check (fail closed, as the unsigned commit's re-mark
// does).
//
// AIDEV-NOTE: the anchorless refresh stores index bytes nothing verified,
// so it must never run for a repository the cache has seen signed:
// stripping a signed flat repository's InRelease and Release would
// otherwise turn it into an unverified one. A cached Release that passes
// its sniff is no signed evidence (deviation 1): the refresh deletes it
// once upstream answers it 404 (or 401/403 beside an index's content
// answer, ruling PG-28). adoption.adopt_unsigned off refuses the
// refresh after this gate (checkFirst, anchorlessDisabled; ruling PG-1),
// so a suite with signed evidence keeps its anchor_absent record whatever
// the knob.
func (a *Adopter) anchorlessEligible(ctx context.Context, suite SuiteRef) (bool, error) {
	if err := a.remarkSignedAnchors(ctx, suite); err != nil {
		return false, fmt.Errorf("re-mark the cached signed anchors: %w", err)
	}
	err := a.cache.CheckUnsignedDowngrade(ctx, suite.CanonicalHost, suite.SuitePath)
	switch {
	case errors.Is(err, cache.ErrUnsignedDowngradeRefused):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// anchorlessDisabled is ruling PG-1: with adoption.adopt_unsigned off, a
// flat suite the anchorless gate admitted is refused as an unsigned
// candidate is (unsignedRefusal). The check records unsigned_disabled,
// keyed anchorAbsentKey (a constant no content matches: there is no
// Release to hash) — the status page names the knob, and the record paces
// the suite (pacedFirst) — and is a successful check (last_success_at set,
// the streak reset), logged changed_deferred with form anchorless. No
// index is requested and no row is deleted, and no pending change is left
// behind (inrelease_change_seen_at cleared: the anchorless form never
// adopts, so nothing else would clear it). The record is written before
// finishCheck, as unsignedFirst orders it: UpdateSuiteFreshness writes no
// record column.
//
// AIDEV-NOTE: ruling PG-1 (maintainer, 2026-10-20): one meaning for the
// knob. The cache does not keep unsigned repositories fresh while it is
// off, and a Release-less repository is the most unsigned there is: it is
// served as the cache first stored its files, as a repository with an
// unsigned Release is (spec C5 safeguard 1). Turning the knob on resumes
// the refresh at the first check after the record's pacing (or
// periodic_refresh), which clears the record.
func (c *Checker) anchorlessDisabled(ctx context.Context, out *checkOutcome, cur *cache.SuiteFreshness, suite SuiteRef, nowUnix int64, t probeTree) {
	c.recordAdoptionFailure(&adoptionRequest{suite: suite, key: anchorAbsentKey}, cache.ReasonUnsignedDisabled)
	cur.LastCheckAt, cur.LastSuccessAt, cur.CheckFailCount = &nowUnix, &nowUnix, 0
	cur.InReleaseChangeSeenAt = nil
	c.finishCheck(ctx, out, cur, suite, checkEnd{form: "anchorless", result: "changed_deferred", status: t.release.status,
		reason: cache.ReasonUnsignedDisabled})
}

// anchorlessAnswer is what one anchorless index request did to its row.
type anchorlessAnswer int

const (
	// anchorlessUnknown: no content answer, and the row stays — a
	// transport error, the deadline, a status not listed, a body over the
	// cap, a local write error, a 304 to a request without validators, a
	// 401 or 403, or a 200 failing its sniff (design review M-7: a captive
	// portal or lost permissions is no answer).
	anchorlessUnknown anchorlessAnswer = iota
	// anchorlessKept: a content answer that keeps the row — a 304, a 200
	// with the cached bytes (only changed validators are stored).
	anchorlessKept
	// anchorlessReplaced: a 200 that passed its sniff replaced the blob.
	anchorlessReplaced
	// anchorlessDeleted: a 404 or 410; the row is to be deleted.
	anchorlessDeleted
	// anchorlessDropped: a changed plain Contents-* (no sniff, plan
	// deviation 2); the row is to be deleted, but its 200 is no content
	// answer — nothing tells it from a soft-404.
	anchorlessDropped
)

// answered reports whether a is a content answer (design review M-7).
func (a anchorlessAnswer) answered() bool {
	return a == anchorlessKept || a == anchorlessReplaced || a == anchorlessDeleted
}

// anchorlessRank orders the anchorless index names a refresh asks:
// Packages*, then Sources*, then Contents-* (design review M-3: a large,
// slow Contents-* asked first could take the check's deadline from the
// indexes apt needs).
func anchorlessRank(name string) int {
	switch {
	case strings.HasPrefix(name, "Packages"):
		return 0
	case strings.HasPrefix(name, "Sources"):
		return 1
	}
	return 2
}

// anchorlessRefresh is spec C4 Dispatch 4 (plan design note 10) for a flat
// suite whose probe tree t found neither InRelease nor Release and whose
// gate admitted it (anchorlessEligible), under the check's host slot and
// deadline (fetchCtx). Each cached url_path row of the suite whose member
// is an anchorless index (aptpath.IsAnchorlessIndex) gets one conditional
// GET (refreshAnchorlessRow), Packages* first, then Sources*, then
// Contents-* (anchorlessRank); then the rows to delete — those answered
// 404 or 410, a changed plain Contents-*, and the suite's cached InRelease
// and Release rows (anchors: read before the probe tree ran) whose probe
// in t answered 404 or 410, or 401 or 403 beside an index request's
// content answer (ruling PG-28) — go in one writer transaction, each only
// while it still holds the blob it was read with (cache.DeleteURLPathRows;
// an anchor a client's miss stored anew during its probe's window is
// kept).
// The check is answered — anchorless_refreshed when a row was replaced or
// deleted, anchorless_unchanged otherwise — unless index requests were
// sent and none got a content answer (anchorlessAnswer.answered: a 304, a
// 200 stored or matching the cached bytes, a 404 or 410): a failed check
// (the streak backs off). A suite with no cached index row sends no
// request and is answered: the anchors' 404s still apply. A delete that
// fails is logged and is no answer: its 404s and 410s stop counting, and
// a check they alone answered (no index request, or every content answer
// a delete) fails. An answered check is a success (last_success_at, the
// streak reset), clears the suite's failure or refusal record (it
// described an anchor upstream no longer serves) and its pending change
// (the anchorless form adopts nothing: a stamp left behind would queue
// the suite every tick, pendingChange). Its writes (the pool,
// RefreshURLPathBlob, DeleteURLPathRows) run under the lifecycle ctx, as
// the check's other writes do: a refresh that passes its deadline still
// commits what it fetched. A shutdown mid-refresh ends it silently; what
// it stored stays. A read error of the index rows is checkFirst's local
// fault: logged at WARN, nothing written, no backoff.
//
// AIDEV-NOTE: no snapshot, no vouching, no strict-mode effect (spec C4):
// the refresh only revalidates url_path rows a client's miss created, and
// a row it cannot answer for stays as it is. Its cache operations go
// through the adopter's *cache.Cache: the checker's Cache interface stays
// the subset its fakes implement.
func (c *Checker) anchorlessRefresh(ctx, fetchCtx context.Context, out *checkOutcome, cur *cache.SuiteFreshness, suite SuiteRef, nowUnix int64, t probeTree, anchors map[string]cache.URLPath) {
	host, suitePath := suite.CanonicalHost, suite.SuitePath
	rows, err := c.adopter.cache.ListSuiteMetadataRows(ctx, suiteKeyOf(suite))
	if err != nil {
		// Task 10 review M5: checkFirst's convention for a local read
		// fault — no write and no backoff, retried at the next check.
		if !c.endedByShutdown(ctx, suite, "read the anchorless index rows", err) {
			c.logger.Warn("freshness: read anchor rows failed",
				"err", err, "canonical_host", host, "suite_path", suitePath)
		}
		return
	}
	var (
		indexes, deletes             []cache.URLPath
		requests, answered, replaced int
		answeredDeletes              int
		lastErr                      error
		overCap                      bool
	)
	for _, row := range rows {
		if name := strings.TrimPrefix(row.Path, suitePath+"/"); aptpath.IsAnchorlessIndex(name) && row.BlobHash != nil {
			indexes = append(indexes, row)
		}
	}
	slices.SortStableFunc(indexes, func(a, b cache.URLPath) int {
		return anchorlessRank(strings.TrimPrefix(a.Path, suitePath+"/")) - anchorlessRank(strings.TrimPrefix(b.Path, suitePath+"/"))
	})
	for _, row := range indexes {
		requests++
		a, err := c.refreshAnchorlessRow(ctx, fetchCtx, suite, strings.TrimPrefix(row.Path, suitePath+"/"), row)
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			c.logger.Debug("freshness: check cancelled", "canonical_host", host, "suite_path", suitePath)
			return
		}
		switch a {
		case anchorlessReplaced:
			replaced++
		case anchorlessDeleted:
			answeredDeletes++
			deletes = append(deletes, row)
		case anchorlessDropped:
			deletes = append(deletes, row)
		}
		if a.answered() {
			answered++
		} else {
			lastErr = err
			overCap = overCap || errors.Is(err, fetch.ErrConditionalBodyTooLarge)
		}
	}
	for _, p := range []struct {
		name string
		o    probeOutcome
	}{{"InRelease", t.inRelease}, {"Release", t.release}} {
		// Design review M-4 for the anchors (Task 10 review M3): the row
		// as read before the probe, so a newer file a client's miss stored
		// during the probe's window fails the delete's blob compare and is
		// kept.
		//
		// AIDEV-NOTE: ruling PG-28. A 404 or 410 deletes the anchor
		// whatever the indexes answered; a 401 or 403 only beside an index
		// request's content answer in this check (answered counts them
		// before the delete): upstream then demonstrably serves the suite,
		// so the 401/403 is S3's or CloudFront's "no such key", and a
		// stale unsigned Release kept beside a refreshed Packages.gz would
		// give [trusted=yes] clients a Hash Sum mismatch that never
		// clears. Without one — upstream may be blocking the cache — the
		// consistent cached pair stays. A failed delete decides nothing
		// more for these than for the 404s (M4 below).
		row, ok := anchors[p.name]
		if ok && (p.o.status == 404 || p.o.status == 410 || (p.o.authDenied() && answered > 0)) {
			deletes = append(deletes, row)
		}
	}
	deleted, deleteFailed := 0, false
	if len(deletes) > 0 {
		n, err := c.adopter.cache.DeleteURLPathRows(ctx, deletes)
		if err != nil {
			if c.endedByShutdown(ctx, suite, "delete anchorless rows", err) {
				return
			}
			c.logger.Warn("freshness: delete anchorless rows failed",
				"err", err, "canonical_host", host, "suite_path", suitePath)
			// Task 10 review M4: a failed delete is no answer. The one
			// transaction deleted nothing, so its 404s and 410s stop
			// counting, as a failed RefreshURLPathBlob makes its row
			// unknown.
			answered -= answeredDeletes
			deleteFailed, lastErr = true, fmt.Errorf("delete anchorless rows: %w", err)
		}
		deleted = n
	}
	if overCap {
		c.noteAnchorlessOverCap(suite)
	}
	if (requests > 0 || deleteFailed) && answered == 0 {
		err := fmt.Errorf("%w: %w", errAnchorlessUnknown, lastErr)
		if requests == 0 {
			err = lastErr // no index request: the anchors' failed delete alone
		}
		c.failCheck(ctx, out, cur, suite, nowUnix, "anchorless", err)
		return
	}
	cur.LastCheckAt, cur.LastSuccessAt, cur.CheckFailCount = &nowUnix, &nowUnix, 0
	cur.InReleaseChangeSeenAt = nil
	e := checkEnd{form: "anchorless", result: "anchorless_unchanged", status: t.release.status, clearRecord: true}
	if replaced+deleted > 0 {
		e.result = "anchorless_refreshed"
	}
	c.finishCheck(ctx, out, cur, suite, e)
}

// noteAnchorlessOverCap logs WARN "freshness: anchorless index over its
// size cap" for suite, at most once per anchorlessOverCapWarnEvery
// process-wide, with the count of the lines it suppressed since the last
// one (design review M-6: minted suites must not flood the log).
func (c *Checker) noteAnchorlessOverCap(suite SuiteRef) {
	ok, suppressed := c.anchorlessOverCap.Allow(time.Now(), anchorlessOverCapWarnEvery)
	if !ok {
		return
	}
	c.logger.Warn("freshness: anchorless index over its size cap",
		"canonical_host", suite.CanonicalHost, "suite_path", suite.SuitePath, "max_bytes", maxAnchorlessIndexBytes,
		"suppressed", suppressed)
}

// refreshAnchorlessRow sends the anchorless index row's conditional GET
// (its stored upstream_url, upstream_etag and upstream_lastmod) under
// fetchCtx, holding one of the process-wide body slots, and applies the
// answer: a 304 keeps the row; a 200 whose body passes
// aptpath.SniffAnchorlessIndex replaces its blob (cache.RefreshURLPathBlob,
// after the body is in the pool); a 200 failing it keeps the row, counts
// acu_anchorless_sniff_rejected_total and is no content answer; a plain
// Contents-* (no sniff, plan deviation 2) is dropped when its bytes
// changed; a 200 with the cached bytes — a plain Contents-* too — stores
// only changed validators, never re-storing the blob; 404 and 410 delete
// it; 401 and 403 keep it, no content answer. Anything else is unknown,
// with err saying why.
//
// AIDEV-NOTE: ruling PG-8. fetch.Conditional holds the whole body in
// memory (io.ReadAll peaks near 2.25× the cap), and checks are bounded
// per host only — client-triggered checks are not bounded by the check
// workers — so without a global bound the memory grows with the number of
// hosts holding anchorless suites. The slot is taken under fetchCtx (a
// check waits within its own deadline) and released by the deferred
// receive once the body is in the pool or discarded, also when this
// panics. One Checker per process (cmd/apt-cacher-ultra builds one), so
// the bound is process-wide.
func (c *Checker) refreshAnchorlessRow(ctx, fetchCtx context.Context, suite SuiteRef, name string, row cache.URLPath) (anchorlessAnswer, error) {
	var etag, lastmod string
	if row.UpstreamETag != nil {
		etag = *row.UpstreamETag
	}
	if row.UpstreamLastMod != nil {
		lastmod = *row.UpstreamLastMod
	}
	// AIDEV-NOTE: a nested hold (Task 10 review M1, ruling PG-24). The
	// check already holds its host's slot (checkFirst: the hostLimiter the
	// handler's client misses share) while it waits here for one of the
	// two process-wide body slots, and a slow request keeps its body slot
	// until the check's deadline (checkDeadline: the fetch transport has
	// no response-header timeout). So two slow anchorless requests
	// anywhere can make other hosts' anchorless checks wait, each holding
	// a host slot idle, up to their deadlines. No deadlock: a body-slot
	// holder never waits for a host slot; bounded, and failed checks back
	// off. Follow-up for phase H (the phase G deferred file): a short
	// bounded wait for a body slot, or a per-request timeout well under
	// checkDeadline. The wait honours fetchCtx (the deadline, a shutdown):
	// TestCheck_AnchorlessSlotWaitEndsWithTheCheck pins it.
	select {
	case c.anchorlessBodies <- struct{}{}:
	case <-fetchCtx.Done():
		return anchorlessUnknown, fmt.Errorf("%s: wait for a body slot: %w", name, fetchCtx.Err())
	}
	defer func() { <-c.anchorlessBodies }()
	target := &fetch.Target{CanonicalHost: suite.CanonicalHost, URL: row.UpstreamURL}
	res, err := c.fetcher.Conditional(fetchCtx, target, etag, lastmod, maxAnchorlessIndexBytes)
	if err != nil {
		var se *fetch.StatusError
		if errors.As(err, &se) && (se.Code == 404 || se.Code == 410) {
			return anchorlessDeleted, nil
		}
		return anchorlessUnknown, fmt.Errorf("%s: %w", name, err)
	}
	switch {
	case res.Status == 304 && (etag != "" || lastmod != ""):
		return anchorlessKept, nil
	case res.Status != 200:
		return anchorlessUnknown, fmt.Errorf("%s: status %d: %w", name, res.Status, errUnexpectedStatus)
	}
	sum := sha256.Sum256(res.Body)
	hash, old := hex.EncodeToString(sum[:]), *row.BlobHash
	pass, sniffable := aptpath.SniffAnchorlessIndex(name, res.Body)
	switch {
	case !sniffable && hash != old:
		return anchorlessDropped, fmt.Errorf("%s: changed, with no sniff to judge it: dropped for the miss path", name)
	case sniffable && !pass:
		anchorlessSniffRejectedTotal.Inc(suite.CanonicalHost)
		c.logger.Debug("freshness: anchorless index failed its sniff, kept",
			"canonical_host", suite.CanonicalHost, "path", row.Path)
		return anchorlessUnknown, fmt.Errorf("%s: the 200's body failed its sniff", name)
	case hash == old:
		// Design review M-1: the cached bytes (a plain Contents-* too: no
		// sniff is needed to keep them). The blob stays as it is (no pool
		// re-hash, no PutBlob); changed validators are stored alone, so an
		// upstream that changed its ETag but not its bytes is asked
		// conditionally again from the next check on.
		if etag == res.ETag && lastmod == res.LastModified {
			return anchorlessKept, nil
		}
		if _, err := c.adopter.cache.RefreshURLPathBlob(ctx, suite.CanonicalScheme, suite.CanonicalHost, row.Path, old, old,
			nonEmpty(res.ETag), nonEmpty(res.LastModified)); err != nil {
			return anchorlessUnknown, fmt.Errorf("%s: store validators: %w", name, err)
		}
		return anchorlessKept, nil
	}
	stored, err := c.adopter.writeBlobBytes(ctx, res.Body)
	if err != nil {
		return anchorlessUnknown, fmt.Errorf("%s: store: %w", name, err)
	}
	updated, err := c.adopter.cache.RefreshURLPathBlob(ctx, suite.CanonicalScheme, suite.CanonicalHost, row.Path, old, stored,
		nonEmpty(res.ETag), nonEmpty(res.LastModified))
	if err != nil {
		return anchorlessUnknown, fmt.Errorf("%s: store: %w", name, err)
	}
	if updated {
		return anchorlessReplaced, nil
	}
	return anchorlessKept, nil
}
