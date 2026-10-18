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
// (the adoption's own deadline may already have passed).
const failureRecordTimeout = 30 * time.Second

// recordAdoptionFailure writes the spec C7 failure record for req's
// content and logs it: adoption_refused for every refused attempt,
// adoption_deferred on a state transition (the first failure of a streak
// or a new reason), adoption_failing_persistently at streak milestones.
// It runs in the adoption goroutine before the suite mutex is released,
// so the next check already sees the record. Shutdown is not a failure:
// nothing is recorded once the lifetime context is done.
func (c *Checker) recordAdoptionFailure(req *adoptionRequest, reason string) {
	s := req.suite
	defer func() {
		if r := recover(); r != nil {
			c.reportPanic("adoption_panicked", "record", s, r)
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
	delay := jitterDelay(backoffDelay(count), c.rand())
	next := now.Add(delay).Unix()
	f := cache.AdoptionFailure{
		Hash: req.key, Reason: reason, Count: count, FirstAt: first, LastAt: now.Unix(),
		NextRetryAt: next, ETag: nonEmpty(req.etag), LastMod: nonEmpty(req.lastmod),
	}
	if err := c.cache.RecordAdoptionFailure(ctx, s.CanonicalScheme, s.CanonicalHost, s.SuitePath, f); err != nil {
		c.logger.Warn("adoption_failure_record_failed",
			"canonical_host", s.CanonicalHost, "suite_path", s.SuitePath, "err", err)
		return
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
	if !refusal && isPersistentMilestone(count) {
		c.logger.Warn("adoption_failing_persistently",
			"canonical_host", s.CanonicalHost, "suite_path", s.SuitePath,
			"reason", reason, "fail_count", count, "failing_since_unixtime", first)
	}
}

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
	c.recordAdoptionFailure(req, cache.ReasonPanic)
}
