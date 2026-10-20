package cache

import (
	"context"
	"fmt"
	"time"
)

// AIDEV-NOTE: cap eviction (spec C3), split out of suite_rows.go
// (deferred minor D-14). The delete itself, deleteSuiteTx, lives in
// suite_reap.go beside the reaper's and POST /forget's use of it.

// Cap eviction (spec C3): a suite is evictable when it has no current
// snapshot and no client has asked for it for suiteEvictIdle; at most
// suiteEvictCandidates are tried per creation (a busy one is skipped).
const (
	suiteEvictIdle       = 24 * time.Hour
	suiteEvictCandidates = 8
	// futureStampSlack is how far ahead of now a last_requested_at must be
	// to count as a backward clock step (stamps are whole seconds; the
	// checker's clockStepTolerance).
	futureStampSlack = 2
)

// ClockStepReporter is an optional SuiteGuard capability
// (*freshness.Checker has it): cap eviction reports a suite whose
// last_requested_at lies in the future — a backward wall-clock step, which
// deleteSuiteTx treats as not idle — through spec C4's rate-limited
// clock_step_detected (ruling PD-11).
type ClockStepReporter interface {
	ReportClockStep(stamp string, key SuiteKey, stored, now int64)
}

// heldSuite is an eviction candidate whose in-memory mutex the caller
// holds (through a SuiteGuard) while the writer operation runs.
type heldSuite struct {
	key     SuiteKey
	release func(deleted bool)
	// settled: settle has run. Only the creating goroutine touches it.
	settled bool
}

// settle releases the hold once (SuiteGuard: release exactly once); later
// calls do nothing. It is marked settled before release runs, so a release
// that panics is never called again.
func (h *heldSuite) settle(deleted bool) {
	if h.settled {
		return
	}
	h.settled = true
	h.release(deleted)
}

// holdEvictionCandidate returns a held eviction candidate when the table is
// at, above or within suiteEvictCandidates of the global cap (a read
// outside the writer; createSuiteTx re-checks everything, and afterCreate
// releases an unused candidate as not deleted — or as deleted when its row
// is gone, a concurrent creation having evicted it; deferred minor D-12),
// or nil: the least recently requested of at most suiteEvictCandidates
// evictable suites whose mutex guard takes at once (a nil guard takes the
// first, with a no-op release). busy reports that it found candidates but
// every one was busy. A guard that is a ClockStepReporter is told of a
// suite whose last_requested_at lies in the future. Its errors name no
// operation: creationAttempt prefixes the caller's (deferred minor D-15).
//
// AIDEV-NOTE: holding a candidate from caps.Global-suiteEvictCandidates on
// (design review M5) keeps creations that race at the cap boundary — each
// saw room outside the writer — from being refused while idle suites could
// make room: TryLock keeps their candidates distinct, so each one that
// runs after the table filled evicts its own. A candidate kept by the
// writer is retried with a fresh list, and a refusal with every candidate
// busy is cached for a minute, not ten (CreateResult.CandidatesBusy; final
// review, concurrency M2): either can happen in a full, healthy cache.
func (c *Cache) holdEvictionCandidate(ctx context.Context, caps SuiteCaps, guard SuiteGuard) (held *heldSuite, busy bool, err error) {
	if caps.Global <= 0 {
		return nil, false, nil
	}
	var n int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM suite_freshness`).Scan(&n); err != nil {
		return nil, false, fmt.Errorf("hold eviction candidate: count suites: %w", err)
	}
	if n < caps.Global-suiteEvictCandidates {
		return nil, false, nil
	}
	now := nowUnix()
	if r, ok := guard.(ClockStepReporter); ok {
		c.reportFutureInterest(ctx, r, now)
	}
	cutoff := now - int64(suiteEvictIdle/time.Second)
	rows, err := c.db.QueryContext(ctx, `
SELECT canonical_scheme, canonical_host, suite_path
  FROM suite_freshness
 WHERE current_snapshot_id IS NULL AND last_requested_at <= ?
 ORDER BY last_requested_at, canonical_scheme, canonical_host, suite_path
 LIMIT ?`, cutoff, suiteEvictCandidates)
	if err != nil {
		return nil, false, fmt.Errorf("hold eviction candidate: list eviction candidates: %w", err)
	}
	var cands []SuiteKey
	for rows.Next() {
		var k SuiteKey
		if err := rows.Scan(&k.CanonicalScheme, &k.CanonicalHost, &k.SuitePath); err != nil {
			_ = rows.Close()
			return nil, false, fmt.Errorf("hold eviction candidate: scan eviction candidate: %w", err)
		}
		cands = append(cands, k)
	}
	if err := rows.Close(); err != nil {
		return nil, false, fmt.Errorf("hold eviction candidate: list eviction candidates: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("hold eviction candidate: list eviction candidates: %w", err)
	}
	for _, k := range cands {
		if guard == nil {
			return &heldSuite{key: k, release: func(bool) {}}, false, nil
		}
		if release, ok := guard.HoldSuite(k); ok {
			return &heldSuite{key: k, release: release}, false, nil
		}
	}
	return nil, len(cands) > 0, nil
}

// reportFutureInterest tells r of the suite without a current snapshot
// whose last_requested_at lies furthest in the future, if any lies more
// than futureStampSlack ahead: cap eviction treats it as not idle, and
// spec C4 asks every gate that meets a future past-event stamp to say so
// (r rate-limits the WARN). A read error reports nothing.
func (c *Cache) reportFutureInterest(ctx context.Context, r ClockStepReporter, now int64) {
	var k SuiteKey
	var stored int64
	if err := c.db.QueryRowContext(ctx, `
SELECT canonical_scheme, canonical_host, suite_path, last_requested_at
  FROM suite_freshness
 WHERE current_snapshot_id IS NULL AND last_requested_at > ?
 ORDER BY last_requested_at DESC
 LIMIT 1`, now+futureStampSlack).Scan(&k.CanonicalScheme, &k.CanonicalHost, &k.SuitePath, &stored); err == nil {
		r.ReportClockStep("last_requested_at", k, stored, now)
	}
}
