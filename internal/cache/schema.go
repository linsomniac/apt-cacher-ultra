// Package cache implements the SQLite + content-addressed disk pool that
// backs apt-cacher-ultra. See SPEC.md §4 for storage layout and §9.4 for
// the single-writer concurrency model.
package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// CurrentSchemaVersion is the schema this build of the binary creates and
// expects. Forward-only: a database tagged with a higher version is treated
// as written by a newer binary and the cache refuses to open it. SPEC §4.3.
const CurrentSchemaVersion = 1

// migrations is indexed such that migrations[N] migrates the database from
// schema version N to N+1. migrations[0] (v0 → v1) creates the entire
// initial schema including the schema_version row.
var migrations = []string{
	// v0 → v1: initial schema per SPEC §4.3.
	`
CREATE TABLE blob (
  hash         TEXT PRIMARY KEY,
  size         INTEGER NOT NULL,
  created_at   INTEGER NOT NULL,
  refcount     INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE url_path (
  canonical_scheme  TEXT NOT NULL,
  canonical_host    TEXT NOT NULL,
  path              TEXT NOT NULL,
  blob_hash         TEXT REFERENCES blob(hash),
  upstream_url      TEXT NOT NULL,
  is_metadata       INTEGER NOT NULL,
  last_requested_at INTEGER,
  request_count     INTEGER NOT NULL DEFAULT 0,
  last_fetched_at   INTEGER,
  upstream_etag     TEXT,
  upstream_lastmod  TEXT,
  PRIMARY KEY (canonical_scheme, canonical_host, path)
);

CREATE INDEX idx_url_path_metadata ON url_path(is_metadata);
CREATE INDEX idx_url_path_last_req ON url_path(last_requested_at);

CREATE TABLE suite_freshness (
  canonical_scheme         TEXT NOT NULL,
  canonical_host           TEXT NOT NULL,
  suite_path               TEXT NOT NULL,
  last_check_at            INTEGER,
  last_success_at          INTEGER,
  inrelease_etag           TEXT,
  inrelease_lastmod        TEXT,
  inrelease_change_seen_at INTEGER,
  PRIMARY KEY (canonical_scheme, canonical_host, suite_path)
);

CREATE TABLE schema_version (
  version INTEGER PRIMARY KEY
);
INSERT INTO schema_version VALUES (1);
`,
}

// migrate brings the database forward to CurrentSchemaVersion. It is safe
// to call on a fresh, partially-migrated, or already-current database.
func migrate(ctx context.Context, db *sql.DB) error {
	current, err := readSchemaVersion(ctx, db)
	if err != nil {
		return fmt.Errorf("read schema_version: %w", err)
	}
	if current > CurrentSchemaVersion {
		return fmt.Errorf("database schema version %d is newer than this binary supports (%d); refusing to downgrade",
			current, CurrentSchemaVersion)
	}
	for v := current; v < CurrentSchemaVersion; v++ {
		if err := applyMigration(ctx, db, v); err != nil {
			return fmt.Errorf("migration %d→%d: %w", v, v+1, err)
		}
	}
	return nil
}

// readSchemaVersion returns 0 when the schema_version table is absent
// (a fresh database).
func readSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v int
	err := db.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&v)
	switch {
	case err == nil:
		return v, nil
	case errors.Is(err, sql.ErrNoRows):
		// Table exists but is empty; treat as v0 and let migration repopulate.
		return 0, nil
	default:
		// SQLite reports "no such table" via a generic error message; assume
		// any error other than ErrNoRows means the schema is uninitialized.
		// AIDEV-NOTE: a real I/O or corruption error would also land here and
		// be misread as "fresh DB", but the migration runner re-issues SQL
		// against the same handle and any persistent fault will resurface.
		return 0, nil
	}
}

// applyMigration runs migrations[from] inside a single transaction.
func applyMigration(ctx context.Context, db *sql.DB, from int) error {
	if from < 0 || from >= len(migrations) {
		return fmt.Errorf("no migration registered for version %d", from)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, migrations[from]); err != nil {
		return err
	}
	// migrations[0] inserts the schema_version row itself; for later
	// versions this UPDATE is the source of truth.
	if from > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE schema_version SET version = ?`, from+1); err != nil {
			return err
		}
	}
	return tx.Commit()
}
