package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Refusal reasons (spec C7): states that use the adoption failure record
// and its retry schedule but are not failures. They are excluded from the
// failing-suites gauge and from adoption_failing_persistently, and the
// admin page shows them as "refused: <reason>".
const (
	ReasonUpstreamOlder            = "upstream_older"
	ReasonRollbackRefused          = "rollback_refused"
	ReasonUnsignedDowngradeRefused = "unsigned_downgrade_refused"
	ReasonUnsignedUnconfirmed      = "unsigned_unconfirmed"
	ReasonUnsignedTooLarge         = "unsigned_too_large"
	ReasonAnchorAbsent             = "anchor_absent"
	// ReasonUnsignedDisabled: adoption.adopt_unsigned is false, so an
	// unsigned candidate is not adopted (phase E ruling PE-12): a
	// never-adopted suite stays unadopted, an established unsigned suite
	// keeps its last snapshot. A paced, visible state, never a failed check.
	ReasonUnsignedDisabled = "unsigned_disabled"

	// ReasonPanic marks the record of an adoption that panicked. It is a
	// failure, but the restart reset leaves it alone (spec C7): retrying
	// a deterministic panic at every start helps nobody.
	ReasonPanic = "panic"
)

var refusalReasons = []string{
	ReasonUpstreamOlder, ReasonRollbackRefused, ReasonUnsignedDowngradeRefused,
	ReasonUnsignedUnconfirmed, ReasonUnsignedTooLarge, ReasonAnchorAbsent, ReasonUnsignedDisabled,
}

// IsRefusalReason reports whether a persisted adoption_fail_reason is a
// refusal state rather than a failure.
func IsRefusalReason(reason string) bool { return slices.Contains(refusalReasons, reason) }

// ErrInvalidAdoptionFailure rejects a record without a content key, a
// reason or a positive streak count, before any SQL runs.
var ErrInvalidAdoptionFailure = errors.New("cache: invalid adoption failure record")

// AdoptionFailure is one write of the spec C7 failure record. Count and
// FirstAt describe the streak since the last success; NextRetryAt is when
// the next attempt of the same content may run; ETag/LastMod are the
// failing content's validators, which the checker sends until then.
type AdoptionFailure struct {
	Hash        string
	Reason      string
	Count       int64
	FirstAt     int64
	LastAt      int64
	NextRetryAt int64
	ETag        *string
	LastMod     *string
}

// clearAdoptionFailureSet resets the record. CommitAdoption's flip
// appends it to its UPDATE, so a successful adoption clears the record in
// the same transaction that makes the snapshot current.
const clearAdoptionFailureSet = `adoption_fail_hash     = NULL,
  adoption_fail_reason   = NULL,
  adoption_fail_count    = 0,
  adoption_fail_first_at = NULL,
  adoption_fail_last_at  = NULL,
  adoption_next_retry_at = NULL,
  adoption_fail_etag     = NULL,
  adoption_fail_lastmod  = NULL`

// RecordAdoptionFailure writes the failure record of an existing suite
// row. It never creates a row: a suite reaped in the meantime records
// nothing (UPDATE-only, spec C3/C7).
func (c *Cache) RecordAdoptionFailure(ctx context.Context, scheme, host, suitePath string, f AdoptionFailure) error {
	if f.Hash == "" || f.Reason == "" || f.Count < 1 {
		return fmt.Errorf("RecordAdoptionFailure: %w: hash, reason and a positive count are required", ErrInvalidAdoptionFailure)
	}
	const q = `
UPDATE suite_freshness
   SET adoption_fail_hash     = ?,
       adoption_fail_reason   = ?,
       adoption_fail_count    = ?,
       adoption_fail_first_at = ?,
       adoption_fail_last_at  = ?,
       adoption_next_retry_at = ?,
       adoption_fail_etag     = ?,
       adoption_fail_lastmod  = ?
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, q,
			f.Hash, f.Reason, f.Count, f.FirstAt, f.LastAt, f.NextRetryAt, f.ETag, f.LastMod,
			scheme, host, suitePath); err != nil {
			return fmt.Errorf("RecordAdoptionFailure: %w", err)
		}
		return nil
	})
}

// ClearAdoptionFailure resets the record of an existing suite row: the
// checker calls it when upstream again serves the current snapshot's
// content (spec C7). UPDATE-only.
func (c *Cache) ClearAdoptionFailure(ctx context.Context, scheme, host, suitePath string) error {
	q := `UPDATE suite_freshness SET ` + clearAdoptionFailureSet + `
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, q, scheme, host, suitePath); err != nil {
			return fmt.Errorf("ClearAdoptionFailure: %w", err)
		}
		return nil
	})
}

// SetAdoptionNextRetry moves the next retry time of an existing record:
// the checker clamps a next_retry_at left far in the future by a
// backward clock step (spec C7), and tests make a retry due at once.
// A row without a record is left alone.
func (c *Cache) SetAdoptionNextRetry(ctx context.Context, scheme, host, suitePath string, next int64) error {
	const q = `
UPDATE suite_freshness SET adoption_next_retry_at = ?
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?
   AND adoption_fail_count > 0`
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, q, next, scheme, host, suitePath); err != nil {
			return fmt.Errorf("SetAdoptionNextRetry: %w", err)
		}
		return nil
	})
}

// RescheduleFailingAdoptions gives every failing suite one prompt retry
// at checker start: next_retry_at = now + offset(), offset drawn per row
// (spec C7 "Restart"). Refusal states and panic records keep their
// schedule. Returns the number of rows rescheduled. One transaction.
// offset runs on the writer goroutine, possibly after this call returned
// on a cancelled context, so it must be safe to call from another
// goroutine (math/rand/v2's top-level functions are; deferred minor B-2).
func (c *Cache) RescheduleFailingAdoptions(ctx context.Context, now int64, offset func() int64) (int, error) {
	skip := append(slices.Clone(refusalReasons), ReasonPanic)
	args := make([]any, len(skip))
	for i, r := range skip {
		args[i] = r
	}
	sel := `SELECT canonical_scheme, canonical_host, suite_path FROM suite_freshness
 WHERE adoption_fail_count > 0
   AND COALESCE(adoption_fail_reason, '') NOT IN (` + strings.TrimSuffix(strings.Repeat("?,", len(skip)), ",") + `)`
	const upd = `UPDATE suite_freshness SET adoption_next_retry_at = ?
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`
	n := 0
	err := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("RescheduleFailingAdoptions: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		rows, err := tx.QueryContext(ctx, sel, args...)
		if err != nil {
			return fmt.Errorf("RescheduleFailingAdoptions: select: %w", err)
		}
		type key struct{ scheme, host, suite string }
		var keys []key
		for rows.Next() {
			var k key
			if err := rows.Scan(&k.scheme, &k.host, &k.suite); err != nil {
				_ = rows.Close()
				return fmt.Errorf("RescheduleFailingAdoptions: scan: %w", err)
			}
			keys = append(keys, k)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("RescheduleFailingAdoptions: close: %w", err)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("RescheduleFailingAdoptions: iterate: %w", err)
		}
		for _, k := range keys {
			if _, err := tx.ExecContext(ctx, upd, now+offset(), k.scheme, k.host, k.suite); err != nil {
				return fmt.Errorf("RescheduleFailingAdoptions: update: %w", err)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("RescheduleFailingAdoptions: commit: %w", err)
		}
		n = len(keys)
		return nil
	})
	// AIDEV-NOTE: on a ctx/close path, submitWrite can return before the
	// writer goroutine finishes running op (cache.go submitWrite: the
	// ctx.Done()/closeCh branches return without waiting for req.res) —
	// the writer may still be assigning n with no happens-before edge to
	// this read, a data race. Never read n after a non-nil err.
	if err != nil {
		return 0, err
	}
	return n, nil
}
