package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrSnapshotAlreadyAdopted is returned by CommitAdoption when invoked
// on a snapshot whose adopted_at column is already set. Re-committing
// would double-bump refcounts for every member, so the writer refuses.
var ErrSnapshotAlreadyAdopted = errors.New("cache: snapshot already adopted")

// InsertCandidateSnapshot inserts a fresh suite_snapshot row with
// adopted_at = NULL and returns the auto-assigned snapshot_id. SPEC2
// §7.5 step 4. Caller uses the returned id to key the snapshot_member
// and package_hash rows it builds during prefetch (steps 5-8), then
// finalizes via CommitAdoption (step 9 / §7.5.1).
//
// All non-nil *Hash fields must point at blob.hash values the caller
// has already persisted via PutBlob; the FK constraints fail closed
// otherwise. The schema CHECK on suite_snapshot enforces the
// "exactly one of inline-or-detached" invariant; passing both modes
// or all-NULL produces a constraint-violation error.
func (c *Cache) InsertCandidateSnapshot(ctx context.Context, sc SnapshotCandidate) (int64, error) {
	const q = `
INSERT INTO suite_snapshot
  (canonical_scheme, canonical_host, suite_path,
   inrelease_hash, inrelease_etag, inrelease_lastmod,
   release_hash, release_gpg_hash, created_at, adopted_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`
	now := nowUnix()
	var id int64
	err := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, q,
			sc.CanonicalScheme, sc.CanonicalHost, sc.SuitePath,
			sc.InReleaseHash, sc.InReleaseETag, sc.InReleaseLastMod,
			sc.ReleaseHash, sc.ReleaseGPGHash, now)
		if err != nil {
			return fmt.Errorf("InsertCandidateSnapshot: %w", err)
		}
		id, err = res.LastInsertId()
		if err != nil {
			return fmt.Errorf("InsertCandidateSnapshot: last id: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// CommitAdoption performs the SPEC2 §7.5.1 atomic flip transaction:
//
//  1. Verify the snapshot is still a candidate (adopted_at IS NULL).
//  2. Insert all snapshot_member rows for snapshotID.
//  3. Insert all package_hash rows.
//  4. Bump blob.refcount for each distinct blob_hash referenced by the
//     new snapshot's members. Each blob is counted once even if many
//     member rows point at it.
//  5. Read the prior current_snapshot_id from suite_freshness (NULL if
//     this is the first adoption for the suite).
//  6. Upsert suite_freshness.current_snapshot_id to snapshotID. The
//     other suite_freshness columns are left untouched.
//  7. Stamp suite_snapshot.adopted_at = now.
//  8. If prior_id was non-NULL, decrement refcount for each distinct
//     blob_hash referenced by the prior snapshot's members. A blob
//     shared between old and new nets to zero (+1 then -1) so the
//     bookkeeping is correct under stable membership.
//
// All steps run in a single SQLite transaction on the writer
// connection. Either every reader after COMMIT sees the new snapshot
// (and every member it implies), or every reader sees the prior — no
// half-flipped state is observable.
//
// Members list each (path, blob_hash, declared_sha256) triple to
// insert. Caller is responsible for ensuring unique paths within the
// list; the snapshot_member primary key (snapshot_id, path) will
// surface duplicates as a constraint error and abort the transaction.
//
// PackageHashes is the per-.deb declared-hash assertion set. Pass an
// empty slice for suites with no .debs (e.g. metadata-only suites).
//
// AIDEV-NOTE: this is the load-bearing transaction of Phase 2. Any
// SQL error inside the body causes a Rollback — no partial flip can
// be observed by any reader. The prior_id lookup happens *inside* the
// transaction so a concurrent flip cannot race the bookkeeping.
func (c *Cache) CommitAdoption(ctx context.Context, snapshotID int64,
	members []SnapshotMember, packageHashes []PackageHash) error {
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("CommitAdoption: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		// Step 1: candidate state guard. Read suite identification too —
		// CommitAdoption needs canonical_scheme/host/suite_path to flip
		// the suite_freshness pointer, and reading them from the row is
		// strictly safer than trusting them from a separate caller
		// argument.
		var (
			scheme, host, suite string
			adoptedAt           sql.NullInt64
		)
		err = tx.QueryRowContext(ctx, `
SELECT canonical_scheme, canonical_host, suite_path, adopted_at
FROM suite_snapshot
WHERE snapshot_id = ?`, snapshotID).Scan(&scheme, &host, &suite, &adoptedAt)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("CommitAdoption: snapshot %d not found", snapshotID)
		case err != nil:
			return fmt.Errorf("CommitAdoption: read candidate: %w", err)
		}
		if adoptedAt.Valid {
			return ErrSnapshotAlreadyAdopted
		}

		// Step 2: insert snapshot_member rows. Each member is validated
		// against its declared invariants before we touch SQLite — a bad
		// row should fail loud, not be smuggled past hash validation.
		for _, m := range members {
			if !validBlobHash(m.BlobHash) {
				return fmt.Errorf("CommitAdoption: member %q blob_hash %w", m.Path, ErrInvalidHash)
			}
			if !validBlobHash(m.DeclaredSHA256) {
				return fmt.Errorf("CommitAdoption: member %q declared_sha256 %w", m.Path, ErrInvalidHash)
			}
			if _, err := tx.ExecContext(ctx, `
INSERT INTO snapshot_member (snapshot_id, path, blob_hash, declared_sha256)
VALUES (?, ?, ?, ?)`,
				snapshotID, m.Path, m.BlobHash, m.DeclaredSHA256); err != nil {
				return fmt.Errorf("CommitAdoption: insert member %q: %w", m.Path, err)
			}
		}

		// Step 3: insert package_hash rows.
		for _, p := range packageHashes {
			if !validBlobHash(p.DeclaredSHA256) {
				return fmt.Errorf("CommitAdoption: package_hash %q declared_sha256 %w", p.Path, ErrInvalidHash)
			}
			if _, err := tx.ExecContext(ctx, `
INSERT INTO package_hash (canonical_scheme, canonical_host, path,
                          declared_sha256, snapshot_id)
VALUES (?, ?, ?, ?, ?)`,
				p.CanonicalScheme, p.CanonicalHost, p.Path,
				p.DeclaredSHA256, snapshotID); err != nil {
				return fmt.Errorf("CommitAdoption: insert package_hash %q: %w", p.Path, err)
			}
		}

		// Step 4: bump refcount for blobs referenced by the new
		// snapshot. The IN-subquery dedupes blob_hash values
		// automatically: each blob row matches once regardless of how
		// many member rows point at it.
		if _, err := tx.ExecContext(ctx, `
UPDATE blob SET refcount = refcount + 1
 WHERE hash IN (SELECT blob_hash FROM snapshot_member WHERE snapshot_id = ?)`,
			snapshotID); err != nil {
			return fmt.Errorf("CommitAdoption: bump new refcounts: %w", err)
		}

		// Step 5: read prior current_snapshot_id from suite_freshness.
		// A missing row means the suite has never had a freshness
		// check (highly unusual since adoption is downstream of one,
		// but the upsert in step 6 handles it).
		var prior sql.NullInt64
		err = tx.QueryRowContext(ctx, `
SELECT current_snapshot_id FROM suite_freshness
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`,
			scheme, host, suite).Scan(&prior)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("CommitAdoption: read prior: %w", err)
		}

		// Step 6: upsert the pointer. Other suite_freshness columns are
		// preserved on conflict — adoption flips the snapshot pointer
		// only, not the freshness state.
		if _, err := tx.ExecContext(ctx, `
INSERT INTO suite_freshness
  (canonical_scheme, canonical_host, suite_path, current_snapshot_id)
VALUES (?, ?, ?, ?)
ON CONFLICT(canonical_scheme, canonical_host, suite_path) DO UPDATE SET
  current_snapshot_id = excluded.current_snapshot_id`,
			scheme, host, suite, snapshotID); err != nil {
			return fmt.Errorf("CommitAdoption: flip pointer: %w", err)
		}

		// Step 7: mark the new snapshot adopted.
		now := nowUnix()
		if _, err := tx.ExecContext(ctx, `
UPDATE suite_snapshot SET adopted_at = ? WHERE snapshot_id = ?`,
			now, snapshotID); err != nil {
			return fmt.Errorf("CommitAdoption: stamp adopted_at: %w", err)
		}

		// Step 8: decrement refcounts for blobs the prior snapshot
		// pinned. A blob shared between old and new gets +1 then -1
		// (net 0) — exactly the bookkeeping a Phase 4 GC will rely on.
		if prior.Valid {
			if _, err := tx.ExecContext(ctx, `
UPDATE blob SET refcount = refcount - 1
 WHERE hash IN (SELECT blob_hash FROM snapshot_member WHERE snapshot_id = ?)`,
				prior.Int64); err != nil {
				return fmt.Errorf("CommitAdoption: decrement prior refcounts: %w", err)
			}
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("CommitAdoption: commit: %w", err)
		}
		return nil
	})
}

// GetSuiteSnapshot returns the suite_snapshot row by id, or ErrNotFound.
// Read path; reads use the connection pool freely.
func (c *Cache) GetSuiteSnapshot(ctx context.Context, snapshotID int64) (*SuiteSnapshot, error) {
	const q = `
SELECT snapshot_id, canonical_scheme, canonical_host, suite_path,
       inrelease_hash, inrelease_etag, inrelease_lastmod,
       release_hash, release_gpg_hash, created_at, adopted_at
FROM suite_snapshot
WHERE snapshot_id = ?`
	var s SuiteSnapshot
	err := c.db.QueryRowContext(ctx, q, snapshotID).Scan(
		&s.SnapshotID, &s.CanonicalScheme, &s.CanonicalHost, &s.SuitePath,
		&s.InReleaseHash, &s.InReleaseETag, &s.InReleaseLastMod,
		&s.ReleaseHash, &s.ReleaseGPGHash, &s.CreatedAt, &s.AdoptedAt,
	)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("GetSuiteSnapshot: %w", err)
	}
	return &s, nil
}

// GetSnapshotMember returns the snapshot_member row for (snapshot_id, path),
// or ErrNotFound. Hot path: §6.1 metadata hit-path validation looks up
// every served byte through this query.
func (c *Cache) GetSnapshotMember(ctx context.Context, snapshotID int64, path string) (*SnapshotMember, error) {
	const q = `
SELECT snapshot_id, path, blob_hash, declared_sha256
FROM snapshot_member
WHERE snapshot_id = ? AND path = ?`
	var m SnapshotMember
	err := c.db.QueryRowContext(ctx, q, snapshotID, path).Scan(
		&m.SnapshotID, &m.Path, &m.BlobHash, &m.DeclaredSHA256,
	)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("GetSnapshotMember: %w", err)
	}
	return &m, nil
}

// ListSnapshotMembers returns every snapshot_member row for the snapshot,
// in no guaranteed order. Used by tests and adoption diagnostics.
func (c *Cache) ListSnapshotMembers(ctx context.Context, snapshotID int64) ([]SnapshotMember, error) {
	const q = `
SELECT snapshot_id, path, blob_hash, declared_sha256
FROM snapshot_member
WHERE snapshot_id = ?`
	rows, err := c.db.QueryContext(ctx, q, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("ListSnapshotMembers: %w", err)
	}
	defer rows.Close()
	var out []SnapshotMember
	for rows.Next() {
		var m SnapshotMember
		if err := rows.Scan(&m.SnapshotID, &m.Path, &m.BlobHash, &m.DeclaredSHA256); err != nil {
			return nil, fmt.Errorf("ListSnapshotMembers scan: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListSnapshotMembers iter: %w", err)
	}
	return out, nil
}

// GetPackageHash returns the package_hash row for (scheme, host, path,
// snapshot_id), or ErrNotFound. Hot path: §6.5 .deb hash validation.
func (c *Cache) GetPackageHash(ctx context.Context, scheme, host, path string, snapshotID int64) (*PackageHash, error) {
	const q = `
SELECT canonical_scheme, canonical_host, path, declared_sha256, snapshot_id
FROM package_hash
WHERE canonical_scheme = ? AND canonical_host = ? AND path = ? AND snapshot_id = ?`
	var p PackageHash
	err := c.db.QueryRowContext(ctx, q, scheme, host, path, snapshotID).Scan(
		&p.CanonicalScheme, &p.CanonicalHost, &p.Path, &p.DeclaredSHA256, &p.SnapshotID,
	)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("GetPackageHash: %w", err)
	}
	return &p, nil
}

// DeclaredHash is the (snapshot_id, declared_sha256) result of the §6.1 /
// §6.2 declared-hash query: every distinct hash that any current snapshot
// asserts for the .deb at (scheme, host, path), with one snapshot id per
// row. The same .deb path can legitimately appear in multiple suites'
// snapshots (e.g. noble + noble-updates referencing the same package), so
// callers must surface the conflict — fail closed rather than picking one
// — when more than one *distinct* declared_sha256 is returned.
type DeclaredHash struct {
	DeclaredSHA256 string
	SnapshotID     int64
}

// DeclaredHashesForPath returns every package_hash row covering this .deb
// path under any current snapshot. Joins package_hash → suite_freshness
// on current_snapshot_id so a row whose snapshot was displaced (orphaned
// rows from a prior adoption) does not count. SPEC2 §6.1 step 2 / §6.2.
//
// Returns rows in no defined order. Empty slice + nil error means "no
// snapshot covers this path" — Phase 1 trust-upstream regime.
//
// AIDEV-NOTE: this is the shared helper between §6.1 (hit-path validation)
// and §6.2 (miss-path validation). Both call sites need declared+snapshot
// pairs (the snapshot id is required for fail-closed conflict logging).
// The DISTINCT clause is over (declared_sha256, snapshot_id), so two
// snapshots that *agree* on a hash still produce two rows — one per
// snapshot. The handler-side helper distinctDeclared collapses those
// duplicate hashes when classifying "0 / 1 / 2+ distinct hashes" for
// the §6.1 hit-path dispatch; the conflict log surface shows every row
// (snapshot id included) so an operator can trace which adoptions
// disagreed.
func (c *Cache) DeclaredHashesForPath(ctx context.Context, scheme, host, path string) ([]DeclaredHash, error) {
	const q = `
SELECT DISTINCT p.declared_sha256, p.snapshot_id
  FROM package_hash p
  JOIN suite_freshness sf ON sf.current_snapshot_id = p.snapshot_id
 WHERE p.canonical_scheme = ?
   AND p.canonical_host   = ?
   AND p.path             = ?`
	rows, err := c.db.QueryContext(ctx, q, scheme, host, path)
	if err != nil {
		return nil, fmt.Errorf("DeclaredHashesForPath: %w", err)
	}
	defer rows.Close()
	var out []DeclaredHash
	for rows.Next() {
		var d DeclaredHash
		if err := rows.Scan(&d.DeclaredSHA256, &d.SnapshotID); err != nil {
			return nil, fmt.Errorf("DeclaredHashesForPath scan: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("DeclaredHashesForPath iter: %w", err)
	}
	return out, nil
}

// SnapshotMemberLookup is the result of the §6.1 metadata hit-path query:
// the snapshot id under which the path was found and the blob_hash the
// snapshot vouches for. The snapshot id is surfaced so the handler can
// emit the X-Cache-Snapshot diagnostic header SPEC2 §3 mandates.
type SnapshotMemberLookup struct {
	SnapshotID int64
	BlobHash   string
}

// LookupSnapshotMember resolves the §6.1 metadata hit path: given the
// suite's (scheme, host, suitePath) and a member path (suite-relative,
// e.g. "main/binary-amd64/Packages"), it joins suite_freshness to
// snapshot_member via current_snapshot_id and returns the matching blob.
//
// Returns ErrNotFound when:
//   - the suite has no suite_freshness row, OR
//   - the suite has a row but current_snapshot_id IS NULL (pre-Phase-2
//     regime — the caller should fall back to LookupURL), OR
//   - the snapshot exists but contains no member at this path.
//
// The caller is responsible for distinguishing "no snapshot in place" from
// "snapshot in place but path not in it"; this method collapses both into
// ErrNotFound. SPEC2 §6.1 mandates a snapshot-in-place + missing-member
// to 404 (no Phase 1 fallback), so the handler must check
// CurrentSnapshotID separately before classifying the miss.
//
// AIDEV-NOTE: the callers want both "is there a snapshot here?" (drives
// the §6.1 step-3 fallthrough decision) and "is this path a member of the
// snapshot?" (drives serve-vs-404). Returning the joined row in one query
// would conflate the two when CurrentSnapshotID is NULL. Use
// GetSuiteFreshness + GetSnapshotMember directly from the handler — see
// handler.tryCacheHit for the full §6.1 control flow. This helper is kept
// as a single-shot convenience for tests and is documented as such.
func (c *Cache) LookupSnapshotMember(ctx context.Context, scheme, host, suitePath, memberPath string) (*SnapshotMemberLookup, error) {
	const q = `
SELECT sf.current_snapshot_id, sm.blob_hash
  FROM suite_freshness sf
  JOIN snapshot_member sm ON sm.snapshot_id = sf.current_snapshot_id
 WHERE sf.canonical_scheme = ?
   AND sf.canonical_host   = ?
   AND sf.suite_path       = ?
   AND sm.path             = ?`
	var (
		snapID   int64
		blobHash string
	)
	err := c.db.QueryRowContext(ctx, q, scheme, host, suitePath, memberPath).Scan(&snapID, &blobHash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("LookupSnapshotMember: %w", err)
	}
	return &SnapshotMemberLookup{SnapshotID: snapID, BlobHash: blobHash}, nil
}

// IntegrityCandidate is a single (blob_hash, declared_sha256, snapshot_id,
// source) tuple emitted by ListIntegrityCandidates for the SPEC2 §6.5
// at-rest scan. The scanner reads pool/<BlobHash>, hashes it, and
// compares the result against BlobHash itself. SourceTable is one of
// "snapshot_member" or "package_hash" — surfaced in the
// at_rest_corruption log line per SPEC2 §10.2 ("first-found is
// reported"). For snapshot_member rows DeclaredSHA256 == BlobHash;
// for package_hash rows blob_hash is not stored on the row, so the
// query reuses declared_sha256 as the expected pool filename.
type IntegrityCandidate struct {
	BlobHash       string
	DeclaredSHA256 string
	SnapshotID     int64
	SourceTable    string
}

// ListIntegrityCandidates returns one row per distinct blob hash pinned
// by any current snapshot, drawn from snapshot_member and package_hash.
// Rows whose snapshot is no longer current (displaced by a later
// adoption) are excluded — the §6.5 scanner only verifies blobs the
// current contract still relies on. SPEC §10.2's "first-found is
// reported" semantic governs duplicates: when the same blob is pinned
// by both a snapshot_member and a package_hash, snapshot_member wins
// (it appears first in the UNION).
//
// Returns rows in no defined order. Empty slice + nil error means no
// snapshot covers any blob — a fresh deploy with no adoptions has
// nothing to scan, which is correct.
//
// AIDEV-NOTE: the integrity scanner is the only caller. The query
// excludes blob.refcount-0 rows transitively via the suite_freshness
// join: only blobs reachable from a current snapshot show up. Phase 4
// GC will reap orphans separately; the scanner does not race it.
func (c *Cache) ListIntegrityCandidates(ctx context.Context) ([]IntegrityCandidate, error) {
	const q = `
SELECT blob_hash, declared_sha256, snapshot_id, source FROM (
  SELECT sm.blob_hash       AS blob_hash,
         sm.declared_sha256 AS declared_sha256,
         sm.snapshot_id     AS snapshot_id,
         'snapshot_member'  AS source
    FROM snapshot_member sm
    JOIN suite_freshness sf ON sf.current_snapshot_id = sm.snapshot_id
  UNION ALL
  SELECT p.declared_sha256 AS blob_hash,
         p.declared_sha256 AS declared_sha256,
         p.snapshot_id     AS snapshot_id,
         'package_hash'    AS source
    FROM package_hash p
    JOIN suite_freshness sf ON sf.current_snapshot_id = p.snapshot_id
)`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("ListIntegrityCandidates: %w", err)
	}
	defer rows.Close()
	// AIDEV-NOTE: dedup by blob_hash in Go rather than SQL DISTINCT to
	// preserve "first-found wins" — snapshot_member rows precede
	// package_hash rows in the UNION ALL, so the first occurrence of
	// each hash is the snapshot_member tuple when both apply. SQL
	// DISTINCT over (blob_hash, source, ...) would emit two rows;
	// DISTINCT on blob_hash alone is undefined under SQLite when other
	// columns differ.
	seen := make(map[string]struct{})
	var out []IntegrityCandidate
	for rows.Next() {
		var ic IntegrityCandidate
		if err := rows.Scan(&ic.BlobHash, &ic.DeclaredSHA256, &ic.SnapshotID, &ic.SourceTable); err != nil {
			return nil, fmt.Errorf("ListIntegrityCandidates scan: %w", err)
		}
		if _, dup := seen[ic.BlobHash]; dup {
			continue
		}
		seen[ic.BlobHash] = struct{}{}
		out = append(out, ic)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListIntegrityCandidates iter: %w", err)
	}
	return out, nil
}

// EvictURLPath deletes the url_path row for (scheme, host, path) and
// decrements the prior blob's refcount in a single transaction. SPEC2
// §6.1 step 5: stale Phase 1 row covered by a Phase 2 snapshot has
// diverged from the snapshot's declared hash; the row must drop and the
// blob's refcount must fall by one for Phase 4 GC to reclaim the bytes.
//
// Idempotent: if the row is already gone (concurrent eviction, manual
// delete) the call is a no-op and returns nil — exactly one decrement
// must fire per refcount-bump, and racing the row delete is the only
// boundary at which the bookkeeping could double-count.
//
// Returns nil on success even when no row was deleted; callers that need
// to know whether they were the eviction winner should check before-and-
// after with LookupURL. Returns an error only on a real DB fault.
//
// AIDEV-NOTE: SPEC2 §4.3.1 names adoption as the only writer that bumps
// blob.refcount, while §6.1 step 5 (and §11) explicitly says eviction
// decrements. Taken together a Phase 1 blob whose url_path row is
// evicted via this path can land with a negative refcount, because
// PutURLPath does not currently bump on insert. Phase 4 GC checks
// `refcount <= 0` for sweep eligibility, so a transient -1 still reaps
// correctly; the literal decrement here matches what the SPEC text
// asks for. If a future phase tightens GC to an `= 0` predicate, this
// helper and PutURLPath will need to be adjusted in lockstep.
func (c *Cache) EvictURLPath(ctx context.Context, scheme, host, path string) error {
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("EvictURLPath: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		// Read the row's current blob_hash inside the tx so a concurrent
		// re-fetch that completes between read and delete does not race.
		var blobHash sql.NullString
		err = tx.QueryRowContext(ctx, `
SELECT blob_hash FROM url_path
 WHERE canonical_scheme = ? AND canonical_host = ? AND path = ?`,
			scheme, host, path).Scan(&blobHash)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("EvictURLPath: read row: %w", err)
		}

		res, err := tx.ExecContext(ctx, `
DELETE FROM url_path
 WHERE canonical_scheme = ? AND canonical_host = ? AND path = ?`,
			scheme, host, path)
		if err != nil {
			return fmt.Errorf("EvictURLPath: delete: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("EvictURLPath: rows affected: %w", err)
		}
		if affected == 0 {
			// Row vanished between SELECT and DELETE — concurrent eviction
			// won. Skip the refcount decrement: the winner already did it.
			return nil
		}
		if blobHash.Valid && blobHash.String != "" {
			if _, err := tx.ExecContext(ctx, `
UPDATE blob SET refcount = refcount - 1 WHERE hash = ?`,
				blobHash.String); err != nil {
				return fmt.Errorf("EvictURLPath: decrement refcount: %w", err)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("EvictURLPath: commit: %w", err)
		}
		return nil
	})
}
