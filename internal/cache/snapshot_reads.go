package cache

// Read-side snapshot helpers (moved from adoption.go: deferred minor C-15).
// Reads use the connection pool freely.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// GetSuiteSnapshot returns the suite_snapshot row by id, or ErrNotFound.
// Read path; reads use the connection pool freely.
func (c *Cache) GetSuiteSnapshot(ctx context.Context, snapshotID int64) (*SuiteSnapshot, error) {
	const q = `
SELECT snapshot_id, canonical_scheme, canonical_host, suite_path,
       inrelease_hash, inrelease_etag, inrelease_lastmod,
       release_hash, release_gpg_hash, created_at, adopted_at,
       package_coverage_complete, anchor_form, release_date, retired_at
FROM suite_snapshot
WHERE snapshot_id = ?`
	var (
		s        SuiteSnapshot
		coverage int64
		form     string
	)
	err := c.db.QueryRowContext(ctx, q, snapshotID).Scan(
		&s.SnapshotID, &s.CanonicalScheme, &s.CanonicalHost, &s.SuitePath,
		&s.SingleAnchorHash, &s.InReleaseETag, &s.InReleaseLastMod,
		&s.ReleaseHash, &s.ReleaseGPGHash, &s.CreatedAt, &s.AdoptedAt,
		&coverage, &form, &s.ReleaseDate, &s.RetiredAt,
	)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("GetSuiteSnapshot: %w", err)
	}
	s.PackageCoverageComplete = coverage != 0
	s.Form = AnchorForm(form)
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

// DisplacedSnapshotMembers returns the snapshot_member rows at path in the
// suite's newest limit adopted, non-retired snapshots other than
// currentID — its displaced snapshots — newest first (spec C8 retained
// by-hash). limit bounds the snapshots searched, not the rows returned: a
// member only an older displaced snapshot holds is not found. limit <= 0
// returns nothing.
//
// AIDEV-NOTE: retired snapshots (quarantined at upgrade, or reaped from
// phase F) never serve, so the retired_at filter is load-bearing.
func (c *Cache) DisplacedSnapshotMembers(ctx context.Context, scheme, host, suitePath, path string, currentID int64, limit int) ([]SnapshotMember, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := c.db.QueryContext(ctx, `
SELECT m.snapshot_id, m.path, m.blob_hash, m.declared_sha256
  FROM (SELECT snapshot_id, adopted_at
          FROM suite_snapshot
         WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?
           AND adopted_at IS NOT NULL AND retired_at IS NULL AND snapshot_id <> ?
         ORDER BY adopted_at DESC, snapshot_id DESC
         LIMIT ?) s
  JOIN snapshot_member m ON m.snapshot_id = s.snapshot_id AND m.path = ?
 ORDER BY s.adopted_at DESC, s.snapshot_id DESC`,
		scheme, host, suitePath, currentID, limit, path)
	if err != nil {
		return nil, fmt.Errorf("DisplacedSnapshotMembers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SnapshotMember
	for rows.Next() {
		var m SnapshotMember
		if err := rows.Scan(&m.SnapshotID, &m.Path, &m.BlobHash, &m.DeclaredSHA256); err != nil {
			return nil, fmt.Errorf("DisplacedSnapshotMembers: scan: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("DisplacedSnapshotMembers: %w", err)
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
