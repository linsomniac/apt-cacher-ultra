// Package cache implements the SQLite + content-addressed disk pool that
// backs apt-cacher-ultra. See SPEC.md §4 for storage layout and §9.4 for
// the single-writer concurrency model.
package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
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
	//
	// AIDEV-NOTE: blob.hash carries a CHECK that pins it to lowercase
	// sha256 hex (length 64, [0-9a-f] only). url_path.blob_hash inherits
	// this validity through the foreign key — a malformed hash there
	// would fail FK before it could be used to construct a pool/ path,
	// guarding against path-traversal attacks via a corrupt or buggy
	// row. The Go API also calls validBlobHash() on every entry; both
	// layers must hold to defend the on-disk pool.
	`
CREATE TABLE blob (
  hash         TEXT PRIMARY KEY
                 CHECK (length(hash) = 64 AND hash NOT GLOB '*[^0-9a-f]*'),
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
//
// SPEC §10: emits a structured log line per applied migration so an
// operator watching the journal during a deploy sees exactly what
// happened on first contact with an old DB.
func migrate(ctx context.Context, db *sql.DB, logger *slog.Logger) error {
	current, err := readSchemaVersion(ctx, db)
	if err != nil {
		return fmt.Errorf("read schema_version: %w", err)
	}
	if current > CurrentSchemaVersion {
		return fmt.Errorf("database schema version %d is newer than this binary supports (%d); refusing to downgrade",
			current, CurrentSchemaVersion)
	}
	if current == CurrentSchemaVersion {
		logger.Debug("schema current", "version", current)
		return nil
	}
	for v := current; v < CurrentSchemaVersion; v++ {
		if err := applyMigration(ctx, db, v); err != nil {
			return fmt.Errorf("migration %d→%d: %w", v, v+1, err)
		}
		logger.Info("schema migrated", "from", v, "to", v+1)
	}
	return nil
}

// readSchemaVersion returns 0 when the schema_version table is absent
// (a fresh database). Distinguishes "table missing" from real errors —
// corruption, I/O failure, or a cancelled context must NOT be silently
// treated as a fresh DB, because that would set off a doomed migration
// against an already-broken store.
func readSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	// AIDEV-NOTE: probe sqlite_master rather than catching the
	// "no such table" error string. Driver-level error messages are not
	// part of the API contract; sqlite_master is.
	var name string
	err := db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'schema_version'`,
	).Scan(&name)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil // fresh DB — migration[0] will create the table
	case err != nil:
		return 0, fmt.Errorf("probe schema_version table: %w", err)
	}

	var v int
	err = db.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Table exists but no row — interrupted migration. Treat as v0
		// and let migration[0] repopulate. The INSERT in migration[0]
		// is the only writer of this row in v1.
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("read schema_version row: %w", err)
	}
	return v, nil
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
