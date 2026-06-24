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
// SPEC2 §4.3, SPEC3 §4.3, SPEC4 §4.3.
const CurrentSchemaVersion = 6

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
	// v2 → v3: Phase 3 hot-package proactive refresh + opt-in strict-mode
	// proof column. SPEC3 §4.3.2.
	//
	// Pure additive DDL:
	//   - package_hash gains binary (Package, Architecture) so Phase 3
	//     hot-set matching can JOIN across snapshots by package identity
	//     (SPEC3 §7.5.3 Stage 1/2). Pre-v3 rows default to empty strings;
	//     the hot-set query filters them out explicitly.
	//   - idx_package_hash_pkg_arch covers Stage 2: the (scheme, host,
	//     snapshot_id, package_name, architecture) tuple used to resolve
	//     hot pairs to candidate-snapshot paths in O(log N). snapshot_id
	//     leads name+arch in the trailing tuple because the same
	//     (Package, Arch) pair appears across many snapshots over time.
	//   - suite_snapshot.package_coverage_complete is the per-snapshot
	//     proof strict mode (§6.1) keys on. Pre-v3 rows default to 0
	//     (unverified); strict mode for those hosts falls through to
	//     trust-upstream until a v3-populated snapshot adopts.
	//
	// AIDEV-NOTE: this migration is forward-only and pure DDL. ALTER ADD
	// COLUMN is O(1) — SQLite stores the new defaults implicitly. The
	// CREATE INDEX scans existing package_hash rows: sub-second for typical
	// caches, tens of seconds for very large ones. Operators with
	// long-running deployments should expect the v2→v3 startup to block
	// until the index build finishes; the schema_migrating Info line at
	// migrate() entry tells journal-driven deploy scripting which
	// from→to is in flight.
	`
ALTER TABLE package_hash ADD COLUMN package_name TEXT NOT NULL DEFAULT '';
ALTER TABLE package_hash ADD COLUMN architecture TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_package_hash_pkg_arch
  ON package_hash(canonical_scheme, canonical_host,
                  snapshot_id, package_name, architecture);

ALTER TABLE suite_snapshot
  ADD COLUMN package_coverage_complete INTEGER NOT NULL DEFAULT 0
    CHECK (package_coverage_complete IN (0, 1));
`,
	// v3 → v4: Phase 4 GC subsystem schema delta. SPEC4 §4.3.2.
	//
	// Pure additive DDL plus two backfill UPDATEs:
	//   - blob.refcount_zeroed_at: the "since refcount reached 0" grace
	//     clock for blob GC (§9.6.2 reap predicate). Backfilled to
	//     created_at on rows already at refcount <= 0 — conservative
	//     (might reap one grace too soon, never one too late).
	//   - suite_snapshot.heartbeat_at: adoption-candidate liveness clock
	//     for snapshot GC (§9.6.3 sub-job A). Backfilled to created_at
	//     on every row; pre-v4 candidate rows are by definition orphans
	//     (the previous process is gone), so the next tick reaps them
	//     after the heartbeat-stale grace.
	//   - idx_blob_gc: partial covering index for the blob GC SELECT.
	//     The (refcount_zeroed_at, hash, size) column list serves the
	//     ORDER BY refcount_zeroed_at + lets SQLite emit hash and size
	//     directly from the index. The partial WHERE refcount <= 0
	//     keeps the index small (steady-state candidate set is tiny).
	//   - idx_url_path_blob: partial index on url_path(blob_hash)
	//     required by the blob GC NOT EXISTS subquery — without it,
	//     each candidate triggers a full url_path scan.
	//
	// AIDEV-NOTE: forward-only and atomic per the framework. Index
	// builds scan the underlying tables once each — sub-minute on
	// healthy fs even for million-row caches; minutes on long-running
	// caches with accumulated GC backlog. The migration is startup-
	// blocking. The schema_migrating Info line at migrate() entry
	// pairs with the schema migrated success line, giving operators
	// a journal pair to script the maintenance window against.
	`
ALTER TABLE blob           ADD COLUMN refcount_zeroed_at INTEGER;
ALTER TABLE suite_snapshot ADD COLUMN heartbeat_at       INTEGER;

UPDATE blob
   SET refcount_zeroed_at = created_at
 WHERE refcount <= 0;

UPDATE suite_snapshot
   SET heartbeat_at = created_at
 WHERE heartbeat_at IS NULL;

CREATE INDEX idx_blob_gc
  ON blob(refcount_zeroed_at, hash, size)
 WHERE refcount <= 0;

CREATE INDEX idx_url_path_blob
  ON url_path(blob_hash)
 WHERE blob_hash IS NOT NULL;
`,
	// v4 → v5: snapshot_skipped_member — the SPEC6_7 record of Release
	// members an adoption declared but did not fetch (4xx publication
	// artifacts, optional-member integrity skips). Pure additive DDL.
	//
	// The repair pass (SPEC6_7 §3) re-attempts rows whose reason is
	// 'optional_member_integrity' on freshness ticks; on success the
	// member is promoted into snapshot_member and its row here is
	// deleted. Rows die with their snapshot (snapshot-GC cascade).
	//
	// AIDEV-NOTE: declared_sha256 carries the same lowercase-hex CHECK
	// as blob.hash / snapshot_member.declared_sha256 — it is the trust
	// anchor a later repair fetch validates against, so a malformed
	// value must fail at the DB layer too, not just in Go.
	`
CREATE TABLE snapshot_skipped_member (
  snapshot_id      INTEGER NOT NULL REFERENCES suite_snapshot(snapshot_id),
  path             TEXT NOT NULL,
  declared_sha256  TEXT NOT NULL
                     CHECK (length(declared_sha256) = 64
                            AND declared_sha256 NOT GLOB '*[^0-9a-f]*'),
  size             INTEGER NOT NULL,
  reason           TEXT NOT NULL,
  detail           TEXT,
  skipped_at       INTEGER NOT NULL,
  retry_count      INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (snapshot_id, path)
);
`,
	// v5 → v6: version-aware retention (SPEC: version-aware-retention design).
	// Pure additive DDL, forward-only.
	//
	//   - package_hash.version: the Debian Version: string parsed from the
	//     binary Packages stanza. The retention mirror rule ranks the
	//     distinct versions of a (package_name, architecture) per suite and
	//     keeps the newest N (retention.max_versions_per_package). Existing
	//     rows default to '' — the "non-binary or pre-migration" marker; the
	//     mirror rule only applies to non-empty versions, so empty-version
	//     rows fall through to the existing snapshot-reference guard. Post-v6
	//     binary rows are guaranteed non-empty by the adoption builder.
	//   - url_path.dropped_at: the hold-grace clock. The url_path GC pass
	//     lazily stamps it when a row first fails both the recency and mirror
	//     guards, clears it when the row re-qualifies, and reaps the row once
	//     now - dropped_at >= hold_packages.window. NULL means "currently
	//     retained / not yet observed failing".
	//
	// AIDEV-NOTE: no backfill — the existing leak is reclaimed operationally
	// (wipe+rebuild or gradual re-adoption), not by ranging empty-version
	// rows. See the design doc §6.
	`
ALTER TABLE package_hash ADD COLUMN version    TEXT NOT NULL DEFAULT '';
ALTER TABLE url_path     ADD COLUMN dropped_at INTEGER;
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
		// schema_migrating Info pairs with the existing "schema migrated"
		// line on success, giving operators a journal pair (start/end) to
		// wait on during a deploy. SPEC3 §4.3.2: the v2→v3 index build
		// scans existing package_hash rows and may take tens of seconds
		// on large caches; the start line is what an `until` loop waits
		// for before timing the maintenance window.
		logger.Info("schema_migrating", "from", v, "to", v+1)
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
