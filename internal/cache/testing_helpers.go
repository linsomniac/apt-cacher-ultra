package cache

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
)

// DeleteSnapshotMembersForTest removes snapshot_member rows for the given
// paths from the specified snapshot. This is a TEST-ONLY helper (do not use
// in production code) that simulates a pre-fix degraded snapshot — one that
// was adopted without some requestable IndexTarget members — so reconcile
// tests can build the exact degraded state they need. Refcount accuracy is
// NOT maintained; use only in tests that do not inspect blob.refcount after
// calling this.
//
// AIDEV-NOTE: exported for use by internal/freshness reconcile tests that
// need to manufacture a degraded snapshot via the delete path (the Layer-2
// serve-contract guard prevents adopting one directly, which is the correct
// production behavior). Named *ForTest by convention; never call from
// non-test code.
func (c *Cache) DeleteSnapshotMembersForTest(ctx context.Context, snapshotID int64, paths ...string) error {
	if len(paths) == 0 {
		return nil
	}
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		// Build "?,?,..." placeholders for the IN clause.
		placeholders := strings.Repeat("?,", len(paths))
		placeholders = placeholders[:len(placeholders)-1] // trim trailing comma
		q := fmt.Sprintf(
			`DELETE FROM snapshot_member WHERE snapshot_id = ? AND path IN (%s)`,
			placeholders,
		)
		args := make([]any, 0, 1+len(paths))
		args = append(args, snapshotID)
		for _, p := range paths {
			args = append(args, p)
		}
		if _, err := conn.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("DeleteSnapshotMembersForTest: %w", err)
		}
		return nil
	})
}

// DeletePackageHashForTest removes the package_hash row identified by
// (scheme, host, debPath, snapshotID). This is a TEST-ONLY helper used by
// reconcile tests to simulate a snapshot whose index member AND its derived
// package_hash rows have been stripped — i.e. a truly clean degraded state
// where reconcile must produce both. Refcount accuracy is NOT maintained.
//
// AIDEV-NOTE: paired with DeleteSnapshotMembersForTest; both are needed when
// a test must prove that reconcile builds package_hash rows from scratch, not
// just that they survive from the initial adoption.
func (c *Cache) DeletePackageHashForTest(ctx context.Context, scheme, host, debPath string, snapshotID int64) error {
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx,
			`DELETE FROM package_hash WHERE canonical_scheme = ? AND canonical_host = ? AND path = ? AND snapshot_id = ?`,
			scheme, host, debPath, snapshotID); err != nil {
			return fmt.Errorf("DeletePackageHashForTest: %w", err)
		}
		return nil
	})
}

// ClearSignedSeenForTest deletes every marker. Tests that build a
// "pre-v7" state with v7 code use it to undo CommitAdoption's marks.
func (c *Cache) ClearSignedSeenForTest(ctx context.Context) error {
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `DELETE FROM suite_signed_seen`)
		return err
	})
}

// SetCommittedHookForTest sets committedHook (cache.go): fn runs on the
// writer goroutine right after a creation attempt's, ReapIdleSuite's or
// ForgetSuite's transaction commits, before its result is handed back.
// Set it before the operation it targets; tests outside the package use
// it to fail a submit whose delete committed (the reaper's settled reap,
// TestReaper_ASettledReapIsAReap).
func (c *Cache) SetCommittedHookForTest(fn func()) {
	c.committedHook = fn
}

// SeedSuiteForTest stores s's suite row the way fixtures relied on before
// spec C3 made every writer but CreateSuite UPDATE-only: it creates the
// row if absent (createSuiteTx, uncapped, interest stamp now) and writes
// s's six check columns with UpdateSuiteFreshness's statement, and its
// LastRequestedAt (the client-interest stamp) when set, in one writer
// transaction. Fixtures use it to build a tracked suite in a given
// state. This file is not a _test.go file, so the helper is compiled into
// the production binary; never call it from non-test code
// (TestOnlyTheHandlerCreatesSuiteRows fails on such a call). TEST-ONLY.
func (c *Cache) SeedSuiteForTest(ctx context.Context, s SuiteFreshness) error {
	key := SuiteKey{CanonicalScheme: s.CanonicalScheme, CanonicalHost: s.CanonicalHost, SuitePath: s.SuitePath}
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("SeedSuiteForTest: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := createSuiteTx(ctx, tx, SuiteSeed{SuiteKey: key, RequestedAt: nowUnix()}, SuiteCaps{}, nil, nowUnix()); err != nil {
			return fmt.Errorf("SeedSuiteForTest: %w", err)
		}
		if err := updateSuiteFreshness(ctx, tx, s); err != nil {
			return fmt.Errorf("SeedSuiteForTest: update: %w", err)
		}
		if s.LastRequestedAt != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE suite_freshness SET last_requested_at = ?
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`,
				*s.LastRequestedAt, s.CanonicalScheme, s.CanonicalHost, s.SuitePath); err != nil {
				return fmt.Errorf("SeedSuiteForTest: interest: %w", err)
			}
		}
		return tx.Commit()
	})
}

// EnsureSuiteForTest gives a suite an empty row (uncapped, interest stamp
// now) unless it has one: spec C3 lets only the handler create rows, and
// CommitAdoption (UPDATE-only) and the checker (I-4) need one. TEST-ONLY.
func (c *Cache) EnsureSuiteForTest(ctx context.Context, scheme, host, suitePath string) error {
	_, err := c.CreateSuite(ctx, SuiteSeed{
		SuiteKey:    SuiteKey{CanonicalScheme: scheme, CanonicalHost: host, SuitePath: suitePath},
		RequestedAt: nowUnix(),
	}, SuiteCaps{}, nil)
	return err
}

// InsertTrackedCandidateForTest is InsertCandidateSnapshot for a suite
// that EnsureSuiteForTest first gives a row, so the candidate can be
// committed (CommitAdoption Step 6 is UPDATE-only). TEST-ONLY.
func (c *Cache) InsertTrackedCandidateForTest(ctx context.Context, sc SnapshotCandidate) (int64, bool, error) {
	if err := c.EnsureSuiteForTest(ctx, sc.CanonicalScheme, sc.CanonicalHost, sc.SuitePath); err != nil {
		return 0, false, err
	}
	return c.InsertCandidateSnapshot(ctx, sc)
}

// MigrateToVersionForTest creates dir/cache.db at schema version (the
// migrations before it applied, no Go step after it), for upgrade
// fixtures a test then fills with an older binary's rows and opens with
// Open. TEST-ONLY.
func MigrateToVersionForTest(ctx context.Context, dir string, version int) error {
	db, err := openDB(filepath.Join(dir, "cache.db"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	for v := 0; v < version; v++ {
		if err := applyMigration(ctx, db, v); err != nil {
			return fmt.Errorf("MigrateToVersionForTest: migration %d: %w", v, err)
		}
	}
	return nil
}
