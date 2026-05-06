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
// as written by a newer binary and the cache refuses to open it. SPEC §4.3,
// SPEC2 §4.3.
const CurrentSchemaVersion = 2

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
	// v1 → v2: Phase 2 snapshot model. Pure additive DDL (SPEC2 §4.3.2):
	//   - suite_snapshot: per-adoption header row (verified Release-text
	//     blob, etag/lastmod, optional release_gpg_hash for detached
	//     adoptions).
	//   - snapshot_member: (snapshot_id, path) → blob_hash + declared
	//     sha256. The "atomic flip" target.
	//   - package_hash: (host, .deb path, snapshot_id) → declared sha256,
	//     materialized at adoption to validate request-path .deb fetches.
	//   - suite_freshness gains current_snapshot_id pointing at the
	//     suite's adopted snapshot (NULL = pre-Phase-2 / not yet adopted).
	//
	// AIDEV-NOTE: this migration is forward-only and pure DDL — no row
	// rewrites, no behavior change. Pre-existing url_path/blob/
	// suite_freshness rows survive untouched. SPEC2 §4.3.2 "trusted-
	// until-replaced" carries the implication: existing pool blobs keep
	// serving via Phase 1 url_path lookup until/unless a §6.1 hit-path
	// or §7.5 adoption rehash promotes or rejects them.
	`
CREATE TABLE suite_snapshot (
  snapshot_id        INTEGER PRIMARY KEY AUTOINCREMENT,
  canonical_scheme   TEXT NOT NULL,
  canonical_host     TEXT NOT NULL,
  suite_path         TEXT NOT NULL,
  inrelease_hash     TEXT REFERENCES blob(hash),
  inrelease_etag     TEXT,
  inrelease_lastmod  TEXT,
  release_hash       TEXT REFERENCES blob(hash),
  release_gpg_hash   TEXT REFERENCES blob(hash),
  created_at         INTEGER NOT NULL,
  adopted_at         INTEGER,
  -- Exactly one of (inrelease_hash) or (release_hash AND release_gpg_hash)
  -- must be populated. Uses IS NULL / IS NOT NULL exclusively, which are
  -- not subject to the 3VL pitfalls of equality across NULLs. Without
  -- this CHECK, an all-NULL row would slip through (and even bypass the
  -- COALESCE-based UNIQUE index, since COALESCE(NULL, NULL) = NULL and
  -- SQLite treats NULLs as distinct for UNIQUE purposes).
  CHECK (
    (inrelease_hash IS NOT NULL AND release_hash IS NULL AND release_gpg_hash IS NULL)
    OR
    (inrelease_hash IS NULL AND release_hash IS NOT NULL AND release_gpg_hash IS NOT NULL)
  )
);

CREATE INDEX idx_suite_snapshot_suite
  ON suite_snapshot(canonical_scheme, canonical_host, suite_path);

CREATE UNIQUE INDEX idx_suite_snapshot_natural
  ON suite_snapshot(canonical_scheme, canonical_host, suite_path,
                    COALESCE(inrelease_hash, release_hash));

CREATE TABLE snapshot_member (
  snapshot_id      INTEGER NOT NULL REFERENCES suite_snapshot(snapshot_id),
  path             TEXT NOT NULL,
  blob_hash        TEXT NOT NULL REFERENCES blob(hash),
  declared_sha256  TEXT NOT NULL
                     CHECK (length(declared_sha256) = 64
                            AND declared_sha256 NOT GLOB '*[^0-9a-f]*'),
  PRIMARY KEY (snapshot_id, path)
);

CREATE INDEX idx_snapshot_member_blob
  ON snapshot_member(blob_hash);

CREATE TABLE package_hash (
  canonical_scheme TEXT NOT NULL,
  canonical_host   TEXT NOT NULL,
  path             TEXT NOT NULL,
  declared_sha256  TEXT NOT NULL
                     CHECK (length(declared_sha256) = 64
                            AND declared_sha256 NOT GLOB '*[^0-9a-f]*'),
  snapshot_id      INTEGER NOT NULL REFERENCES suite_snapshot(snapshot_id),
  PRIMARY KEY (canonical_scheme, canonical_host, path, snapshot_id)
);

CREATE INDEX idx_package_hash_snapshot
  ON package_hash(snapshot_id);

ALTER TABLE suite_freshness
  ADD COLUMN current_snapshot_id INTEGER REFERENCES suite_snapshot(snapshot_id);
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
