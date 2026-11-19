package cache

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
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
	return c.submitWrite(ctx, writeTest, func(ctx context.Context, conn *sql.Conn) error {
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
	return c.submitWrite(ctx, writeTest, func(ctx context.Context, conn *sql.Conn) error {
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
	return c.submitWrite(ctx, writeTest, func(ctx context.Context, conn *sql.Conn) error {
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
	return c.submitWrite(ctx, writeTest, func(ctx context.Context, conn *sql.Conn) error {
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

// HoldWriterForTest occupies the writer goroutine: it submits an op that
// blocks until release is called (or ctx is cancelled) and returns once
// that op is running, so every write submitted afterwards queues behind
// it. release is idempotent. Tests use it to put a write in the queue
// behind a busy writer (issue #14: a GC batch queued behind adoption
// commits). If the op never starts (the cache is closed, or ctx ends
// first) it returns that error and no hold. TEST-ONLY.
//
// AIDEV-NOTE: exported for internal/gc's start-deadline tests (issue #14
// Tier 1a). The op also ends on its ctx so a cancelled hold cannot wedge
// the writer, and so Close cannot hang on it once ctx is done.
func (c *Cache) HoldWriterForTest(ctx context.Context) (release func(), err error) {
	started := make(chan struct{})
	gate := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- c.submitWrite(ctx, writeTest, func(opCtx context.Context, _ *sql.Conn) error {
			close(started)
			select {
			case <-gate:
			case <-opCtx.Done():
			}
			return nil
		})
	}()
	select {
	case <-started:
		var once sync.Once
		return func() { once.Do(func() { close(gate) }) }, nil
	case err := <-done:
		if err == nil {
			// The op ran and already ended on its cancelled ctx.
			err = ctx.Err()
		}
		return nil, fmt.Errorf("HoldWriterForTest: no hold: %w", err)
	}
}

// PackageRowsOfSnapshotForTest returns the package rows the snapshot holds,
// in path order: what a test used to read from the table by snapshot id.
// TEST-ONLY.
//
// AIDEV-NOTE: tests read and seed package rows through this,
// InsertPackageRowForTest, GetPackageHash and DeletePackageHashForTest, not
// by SQL of their own, so that where the rows are kept is this file's
// business and not every test's (the package-list plan, Task 1).
func (c *Cache) PackageRowsOfSnapshotForTest(ctx context.Context, snapshotID int64) ([]PackageHash, error) {
	rows, err := c.db.QueryContext(ctx, `
SELECT canonical_scheme, canonical_host, path, declared_sha256, snapshot_id,
       package_name, architecture, version, flat_filename
  FROM package_hash WHERE snapshot_id = ? ORDER BY path`, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("PackageRowsOfSnapshotForTest: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PackageHash
	for rows.Next() {
		var p PackageHash
		if err := rows.Scan(&p.CanonicalScheme, &p.CanonicalHost, &p.Path, &p.DeclaredSHA256, &p.SnapshotID,
			&p.PackageName, &p.Architecture, &p.Version, &p.FlatFilename); err != nil {
			return nil, fmt.Errorf("PackageRowsOfSnapshotForTest scan: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("PackageRowsOfSnapshotForTest iter: %w", err)
	}
	return out, nil
}

// InsertPackageRowForTest adds one package row to the snapshot's rows,
// through the writer, with no validation beyond the schema's and none of a
// commit's bookkeeping (row.SnapshotID is ignored). TEST-ONLY: for fixtures
// that need a row no adoption would write.
func (c *Cache) InsertPackageRowForTest(ctx context.Context, snapshotID int64, row PackageHash) error {
	return c.submitWrite(ctx, writeTest, func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `
INSERT INTO package_hash (canonical_scheme, canonical_host, path, declared_sha256, snapshot_id,
                          package_name, architecture, version, flat_filename)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			row.CanonicalScheme, row.CanonicalHost, row.Path, row.DeclaredSHA256, snapshotID,
			row.PackageName, row.Architecture, row.Version, row.FlatFilename); err != nil {
			return fmt.Errorf("InsertPackageRowForTest: %w", err)
		}
		return nil
	})
}
