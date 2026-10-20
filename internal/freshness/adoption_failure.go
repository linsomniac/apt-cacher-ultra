package freshness

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/observability"
)

// failureRecordTimeout bounds the record write after an adoption ends
// (the adoption's own deadline may already have passed). The write is one
// UPDATE; its wait is the single writer's queue, which can hold another
// suite's CommitAdoption for up to commitTimeout. A shorter bound would
// lose the record behind a long commit, and the gate would not apply to
// the failing content. The suite mutex is held meanwhile, which is what
// makes the record exist before the next check (spec C7).
const failureRecordTimeout = commitTimeout + 30*time.Second

// recordAdoptionFailure writes the spec C7 failure record for req's
// content and logs it: adoption_refused for every refused attempt,
// adoption_deferred on a state transition (the first failure of a streak
// or a new reason), adoption_failing_persistently at streak milestones,
// and adoption_refused_persistently at the same milestones for a refusal
// whose streak is at least refusalEscalateAge old.
// It runs in the adoption goroutine before the suite mutex is released,
// so the next check already sees the record. Shutdown is not a failure:
// nothing is recorded once the lifetime context is done.
func (c *Checker) recordAdoptionFailure(req *adoptionRequest, reason string) {
	s := req.suite
	defer func() {
		if r := recover(); r != nil {
			unloggedPanic(func() { c.reportPanic("adoption_panicked", "record", s, r) })
		}
	}()
	if c.lifetimeCtx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(c.lifetimeCtx, failureRecordTimeout)
	defer cancel()
	cur, err := c.cache.GetSuiteFreshness(ctx, s.CanonicalScheme, s.CanonicalHost, s.SuitePath)
	if err != nil {
		// A suite reaped while its adoption ran (phase F) has nothing left
		// to gate: that is routine, not a fault.
		level := slog.LevelWarn
		if errors.Is(err, cache.ErrNotFound) {
			level = slog.LevelDebug
		}
		c.logger.Log(ctx, level, "adoption_failure_record_failed",
			"canonical_host", s.CanonicalHost, "suite_path", s.SuitePath, "err", err)
		return
	}
	now := c.now()
	count := cur.AdoptionFailCount + 1
	first := now.Unix()
	// Past-event rule (spec C4): a streak start in the future (backward
	// clock step) is infinitely old, so the streak's stamp restarts.
	if cur.AdoptionFailCount > 0 && cur.AdoptionFailFirstAt != nil && *cur.AdoptionFailFirstAt <= now.Unix() {
		first = *cur.AdoptionFailFirstAt
	}
	if restartsStreak(cur, req.key, reason) {
		first = now.Unix()
		if cur.AdoptionFailCount == 0 || cur.AdoptionFailReason == nil || *cur.AdoptionFailReason != reason {
			count = 1 // a new state: its retry schedule starts over too
		}
	}
	delay := jitterDelay(backoffDelay(count), c.rand())
	next := now.Add(delay).Unix()
	f := cache.AdoptionFailure{
		Hash: req.key, Reason: reason, Count: count, FirstAt: first, LastAt: now.Unix(),
		NextRetryAt: next, ETag: nonEmpty(req.etag), LastMod: nonEmpty(req.lastmod),
	}
	if req.crossForm {
		// Another anchor file's validators: never sent on the current
		// form's GET (design review I-1, retryGate.validators).
		f.ETag, f.LastMod = nil, nil
	}
	if c.recordHook != nil {
		c.recordHook()
	}
	if err := c.cache.RecordAdoptionFailure(ctx, s.CanonicalScheme, s.CanonicalHost, s.SuitePath, f); err != nil {
		// A shutdown that raced the failure ended the write: nothing is
		// recorded at shutdown (spec C7), so that is no fault (B-20).
		level := slog.LevelWarn
		if c.lifetimeCtx.Err() != nil {
			level = slog.LevelDebug
		}
		c.logger.Log(ctx, level, "adoption_failure_record_failed",
			"canonical_host", s.CanonicalHost, "suite_path", s.SuitePath, "err", err)
		return
	}
	if req.lateral {
		// Ruling PE-11: the lateral's own hold, which the detached form's
		// later failures (they replace the record) never extend.
		c.lateralHolds.Store(suiteKey(s.CanonicalScheme, s.CanonicalHost, s.SuitePath), lateralHold{key: req.key, retryAt: next})
	}
	refusal := cache.IsRefusalReason(reason)
	attrs := []any{
		"canonical_host", s.CanonicalHost, "suite_path", s.SuitePath,
		"reason", reason, "refusal", refusal, "fail_count", count,
		"retry_in_seconds", int64(delay / time.Second), "next_retry_unixtime", next,
	}
	if refusal {
		c.logger.Info("adoption_refused", attrs...)
	}
	// Spec C7: adoption_deferred marks a state transition, not every attempt.
	if cur.AdoptionFailCount == 0 || cur.AdoptionFailReason == nil || *cur.AdoptionFailReason != reason {
		c.logger.Info("adoption_deferred", attrs...)
	}
	switch {
	case !isPersistentMilestone(count):
	case !refusal:
		c.logger.Warn("adoption_failing_persistently",
			"canonical_host", s.CanonicalHost, "suite_path", s.SuitePath,
			"reason", reason, "fail_count", count, "failing_since_unixtime", first)
	case now.Unix()-first >= int64(refusalEscalateAge/time.Second):
		// A refusal is a state, not a failure, but one that has lasted a
		// day may be a silent freeze (a vendor that stopped emitting Date
		// is refused upstream_older forever): make it alertable.
		c.logger.Warn("adoption_refused_persistently",
			"canonical_host", s.CanonicalHost, "suite_path", s.SuitePath,
			"reason", reason, "fail_count", count, "failing_since_unixtime", first)
	}
}

// restartsStreak reports a confirmation record (spec C4, C5) whose
// confirmation stamp restarts: unsigned_unconfirmed or anchor_absent
// recorded for other content or after another reason. Their
// adoption_fail_first_at is the first observation of THIS state and
// content, which the confirmation gap is measured from ("two observations
// of the same Release bytes").
//
// AIDEV-NOTE: design note 18 as amended by the final fix wave (FW-3 (a)):
// only the confirmation stamp (first_at) restarts on new content; the
// count — the retry schedule — keeps growing while the reason stays the
// same, and restarts at 1 only with a new reason (recordAdoptionFailure).
// Restarting the count too kept a candidate whose bytes change at every
// observation (a Release generated per request, disagreeing backends) at
// a 2-minute retry forever: PE-3's pacing never held and it cost about
// 1,150 requests a day with no client (720 trees with clients), never
// escalating (final reviews I3, E9). Stable bytes still confirm at the
// first probe at least the gap after their first observation: the
// record's retry time, or the scheduler's refresh check (PE-3) when that
// comes first, so a grown count delays a confirmation by at most one
// periodic_refresh.
func restartsStreak(cur *cache.SuiteFreshness, key, reason string) bool {
	if reason != cache.ReasonUnsignedUnconfirmed && reason != cache.ReasonAnchorAbsent {
		return false
	}
	return cur.AdoptionFailCount == 0 || cur.AdoptionFailReason == nil || *cur.AdoptionFailReason != reason ||
		cur.AdoptionFailHash == nil || *cur.AdoptionFailHash != key
}

// refusalEscalateAge is how long a refusal streak lasts before it is
// escalated: WARN adoption_refused_persistently at the streak milestones,
// and a warn row and SUITES vital on the admin page (the admin package
// keeps its own copy of the 24 h). A future streak start counts as now
// (clock-step rule), so its age is 0.
const refusalEscalateAge = 24 * time.Hour

// isPersistentMilestone reports streak counts 3, 6, 12, 24, …, the
// adoption_failing_persistently thresholds (spec C7).
func isPersistentMilestone(n int64) bool {
	if n < 3 || n%3 != 0 {
		return false
	}
	q := n / 3
	return q&(q-1) == 0
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// recoverAdoption is the adoption goroutine's panic boundary (I-1). It
// must be deferred directly — recover() returns nil anywhere else — and
// it runs before the suite mutex is released. A panic is a failed
// attempt: counted, timed, shown in the admin ring, and recorded with
// reason panic so the retry schedule applies (spec C5, C7).
//
// AIDEV-NOTE: same trap as recoverGoroutine — `defer c.recoverAdoption(...)`
// only; wrapping it in a closure makes recover() return nil and the panic
// kills the daemon.
func (c *Checker) recoverAdoption(req *adoptionRequest, start time.Time) {
	r := recover()
	if r == nil {
		return
	}
	// The reporting runs under its own boundary (B-18): a panic there must
	// neither escape nor skip the record below.
	unloggedPanic(func() {
		c.reportPanic("adoption_panicked", "adoption", req.suite, r)
		d := time.Since(start)
		host := req.suite.CanonicalHost
		adoptionTotal.Inc("panicked", host)
		adoptionDurationSeconds.Observe(d.Seconds(), "panicked", host)
		if c.adoptionRing != nil {
			c.adoptionRing.Record(observability.AdoptionEvent{
				Host: host, SuitePath: req.suite.SuitePath, Outcome: "panicked", Reason: cache.ReasonPanic,
				CompletedUnixSec: c.now().Unix(), DurationSeconds: d.Seconds(),
			})
		}
	})
	c.recordAdoptionFailure(req, cache.ReasonPanic)
}
