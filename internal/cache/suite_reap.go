package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// deleteOutcome is what deleteSuiteTx did.
type deleteOutcome int

const (
	// suiteKept: the row no longer qualifies; nothing was written.
	suiteKept deleteOutcome = iota
	// suiteDeleted: the row was deleted.
	suiteDeleted
	// suiteNoRow: there was no row.
	suiteNoRow
)

// deleteSuiteTx is the single suite-row delete (spec C3 cap eviction; phase
// F's ReapIdleSuite reuses it), run inside the caller's writer
// transaction as a compare-and-delete: it re-reads the row and deletes it
// only if the row still qualifies — last_requested_at at or before cutoff
// (a stamp in the future, after a backward clock step, never qualifies;
// nor does a NULL one) and current_snapshot_id equal to wantCurrent (nil:
// no current snapshot). It releases the current snapshot's pins (the
// Step-8 decrement), retires every snapshot of the suite, and deletes the
// row.
//
// AIDEV-NOTE: retiring the snapshots is load-bearing: CommitAdoption
// refuses a retired candidate, and retired rows leave the natural-key,
// rollback and retained-by-hash lookups, so a returning suite re-adopts
// unchanged content at once (spec C9). Every statement is keyed by the
// full (scheme, host, suite_path): a same-path suite of another host, or
// the other scheme's twin, is never touched
// (TestCreateSuite_EvictionTouchesOnlyTheEvictee,
// TestCreateSuite_EvictionLeavesTheOtherSchemesTwin). Keep it the single
// DELETE FROM suite_freshness (TestSuiteFreshnessHasOneDeleteSite, ruling
// R-F1; moved here from suite_rows.go, deferred minor D-14).
func deleteSuiteTx(ctx context.Context, tx *sql.Tx, key SuiteKey, wantCurrent *int64, cutoff, now int64) (deleteOutcome, error) {
	var current, requested sql.NullInt64
	err := tx.QueryRowContext(ctx, `
SELECT current_snapshot_id, last_requested_at FROM suite_freshness
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`,
		key.CanonicalScheme, key.CanonicalHost, key.SuitePath).Scan(&current, &requested)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return suiteNoRow, nil
	case err != nil:
		return suiteKept, fmt.Errorf("delete suite %s%s: read row: %w", key.CanonicalHost, key.SuitePath, err)
	}
	if !requested.Valid || requested.Int64 > cutoff {
		return suiteKept, nil
	}
	if current.Valid != (wantCurrent != nil) || (current.Valid && current.Int64 != *wantCurrent) {
		return suiteKept, nil
	}
	if current.Valid {
		if err := decrementSnapshotRefcountsTx(ctx, tx, current.Int64, now); err != nil {
			return suiteKept, fmt.Errorf("delete suite %s%s: %w", key.CanonicalHost, key.SuitePath, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE suite_snapshot SET retired_at = ?
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ? AND retired_at IS NULL`,
		now, key.CanonicalScheme, key.CanonicalHost, key.SuitePath); err != nil {
		return suiteKept, fmt.Errorf("delete suite %s%s: retire snapshots: %w", key.CanonicalHost, key.SuitePath, err)
	}
	if _, err := tx.ExecContext(ctx, `
DELETE FROM suite_freshness
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`,
		key.CanonicalScheme, key.CanonicalHost, key.SuitePath); err != nil {
		return suiteKept, fmt.Errorf("delete suite %s%s: %w", key.CanonicalHost, key.SuitePath, err)
	}
	return suiteDeleted, nil
}
