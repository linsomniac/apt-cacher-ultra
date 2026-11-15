package handler

// Request-time revalidation (freshness.revalidate_wait; the 2026-11-14
// publication-delay review).
//
// The cache used to answer a suite's anchor (InRelease, Release) from its
// current snapshot at once and ask upstream afterwards. The first
// `apt update` after a publication therefore always showed the previous
// release, and on a quiet cache the snapshot it showed could be as old as
// the background refresh interval plus a scheduler tick; only that request
// made the cache look. Now the handler runs the suite's check before it
// serves the anchor and waits for it — and, when the check finds a new
// release, for its adoption — for at most revalidate_wait. Whatever is
// not finished by then goes on in the background and the current,
// verified snapshot is served, exactly as before: the wait can make a
// response fresher, never less safe, and an upstream that is down or
// slow costs a request at most the wait.
//
// The request line of every anchor request also says what the client got
// and what the cache knew (anchorTrace): the next "the update was upstream
// but not in the cache" report can be answered from the log.

import (
	"context"
	"net/http"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// isAnchor reports whether req asks for a suite's anchor:
// <suite>/InRelease or <suite>/Release (Release.gpg is not one: it is
// never held, and never served ahead of its Release).
func isAnchor(req *proxy.Request) bool {
	if !req.IsMetadata || req.SuitePath == "" {
		return false
	}
	return req.Path == req.SuitePath+inReleaseSuffix || req.Path == req.SuitePath+releaseSuffix
}

// anchorTrace is what an anchor request learned about its suite, for its
// request line. One per anchor request, carried in the request's context
// (anchorTraceKey) and in its suiteReq.
type anchorTrace struct {
	// Revalidation: reval is its outcome ("" when none ran, or the check
	// was turned away by the cooldown), revalCheck the freshness check's
	// result behind it, revalWait how long the request waited.
	reval      string
	revalCheck string
	revalWait  time.Duration

	// What was served: snapshotID is the snapshot the anchor came from (0
	// when it did not come from one); checkedAgo is the seconds since the
	// suite's last successful upstream check (-1: never, or unknown);
	// behind is the seconds since the cache first saw upstream content it
	// has not published (-1: it knows of none).
	snapshotID int64
	checkedAgo int64
	behind     int64
}

type anchorTraceKey struct{}

// The revalidation outcomes, the request line's reval field (a closed
// set; docs/log-fields.md).
const (
	// revalCurrent: upstream was asked and has nothing newer than the
	// snapshot served.
	revalCurrent = "current"
	// revalPublished: a newer release was published while the request
	// waited; the response is from it.
	revalPublished = "published"
	// revalPending: the cache knows of newer upstream content that was
	// not published within the wait — an adoption still running, queued
	// for a slot, or failing — and served the previous release.
	revalPending = "pending"
	// revalUnknown: upstream could not be asked within the wait — the
	// check failed, or had not finished — so the snapshot served is not
	// confirmed current.
	revalUnknown = "unknown"
	// revalOther: the check ended in a state none of the above describes
	// (a refusal, an absent anchor); reval_check names it.
	revalOther = "other"
)

// revalOutcome maps how a check ended, and what became of the adoption it
// started, to the request line's reval value ("" for none: the check was
// turned away without asking upstream).
func revalOutcome(check string, published bool) string {
	switch {
	case published:
		return revalPublished
	case check == "not_modified" || check == "unchanged":
		return revalCurrent
	case check == "skipped":
		return ""
	case check == "failed" || check == "" || check == "busy":
		return revalUnknown
	case check == "changed" || check == "form_changed" || check == "first_adoption" ||
		check == "changed_queued" || check == "first_adoption_queued" || check == "changed_deferred":
		return revalPending
	}
	return revalOther
}

// revalidateAnchor runs the suite's freshness check before its anchor is
// served and waits, at most h.revalidateWait from start, for the check
// and for the adoption of a new release (see the file comment). It fills
// sr.anchor's revalidation fields. The caller checks afterwards whether
// the client left during the wait.
//
// AIDEV-NOTE: it runs BEFORE anchorStart registers the serve with the flip
// barrier, and that order is load-bearing. A request waiting here is not
// yet a serve of the old anchor, so the adoption it waits for finds its
// suite quiet and switches; registered first, the request would be "an
// anchor in flight", the barrier would wait for it while it waited for
// the barrier, and both would only end at the wait's bound. After the
// wait the request registers and is served whatever is current then.
//
// AIDEV-NOTE: the check runs on its own goroutine (as the check after a
// response does: activeWG, the lifecycle context), so it goes on when the
// request stops waiting; the request only watches for its result.
func (h *Handler) revalidateAnchor(r *http.Request, req *proxy.Request, start time.Time, sr *suiteReq) {
	if h.revalidator == nil || h.revalidateWait <= 0 || sr.anchor == nil {
		return
	}
	tr := sr.anchor
	sr.revalidated = true
	done := make(chan string, 1)
	h.activeWG.Add(1)
	go func() {
		defer h.activeWG.Done()
		// AIDEV-NOTE: I-1, as maybeFireFreshness: a panic in the check must
		// not kill the daemon. The request is told the check failed.
		defer func() {
			if rec := recover(); rec != nil {
				h.logGoroutinePanic("freshness_check_panicked", &t1PanicStackLogged, rec,
					"where", "t1", "canonical_scheme", req.CanonicalScheme, "canonical_host", req.CanonicalHost,
					"suite_path", req.SuitePath)
				done <- "failed"
			}
		}()
		done <- h.revalidator.CheckNow(h.lifecycleCtx, req.CanonicalScheme, req.CanonicalHost, req.SuitePath)
	}()

	timer := time.NewTimer(h.revalidateWait)
	defer timer.Stop()
	var check string
	select {
	case check = <-done:
	case <-timer.C:
		// The check is still asking upstream (or waiting for a host slot).
		tr.reval, tr.revalWait = revalUnknown, time.Since(start)
		return
	case <-r.Context().Done():
		return
	case <-h.lifecycleCtx.Done():
		return
	}
	published := false
	switch check {
	case "skipped", "not_modified", "unchanged", "failed":
		// Nothing is in flight that this request could wait for.
	default:
		// The check started an adoption, or found the suite busy with
		// another request's check or adoption, or left a change pending:
		// wait for what is in flight, within the same bound.
		awaited, pub, _ := h.revalidator.AwaitSuite(r.Context(), req.CanonicalScheme, req.CanonicalHost, req.SuitePath,
			start, h.revalidateWait)
		published = pub
		if check == "busy" && awaited != "" {
			check = awaited // the check this request waited for, not its own turned-away one
		}
	}
	if out := revalOutcome(check, published); out != "" {
		tr.reval, tr.revalCheck, tr.revalWait = out, check, time.Since(start)
	}
}

// noteAnchorServe records, for the request line, which snapshot an anchor
// is served from and what the suite's row says about its freshness.
func noteAnchorServe(sr *suiteReq, suite *cache.SuiteFreshness, snapshotID int64) {
	if sr == nil || sr.anchor == nil {
		return
	}
	tr := sr.anchor
	now := wallclock.Unix()
	tr.snapshotID = snapshotID
	if suite.LastSuccessAt != nil {
		tr.checkedAgo = max(now-*suite.LastSuccessAt, 0)
	}
	if suite.InReleaseChangeSeenAt != nil {
		tr.behind = max(now-*suite.InReleaseChangeSeenAt, 0)
	}
}

// anchorLogAttrs returns the request line's fields for an anchor request
// (docs/log-fields.md): present only when they say something.
func anchorLogAttrs(ctx context.Context) []any {
	tr, _ := ctx.Value(anchorTraceKey{}).(*anchorTrace)
	if tr == nil {
		return nil
	}
	var attrs []any
	if tr.snapshotID != 0 {
		attrs = append(attrs, "snapshot_id", tr.snapshotID)
	}
	if tr.checkedAgo >= 0 {
		attrs = append(attrs, "checked_ago_s", tr.checkedAgo)
	}
	if tr.behind >= 0 {
		attrs = append(attrs, "behind_s", tr.behind)
	}
	if tr.reval != "" {
		attrs = append(attrs, "reval", tr.reval, "reval_check", tr.revalCheck, "reval_wait_ms", tr.revalWait.Milliseconds())
	}
	return attrs
}
