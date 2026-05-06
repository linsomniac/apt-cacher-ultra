package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrNotFound is returned when a row is absent. Callers should distinguish
// "miss" (caller's normal flow) from real DB errors.
var ErrNotFound = errors.New("cache: not found")

// LookupURL returns the url_path row for (canonicalScheme, canonicalHost,
// path), or ErrNotFound. Reads use the connection pool directly; this is
// the hot path on every cache hit.
func (c *Cache) LookupURL(ctx context.Context, scheme, host, path string) (*URLPath, error) {
	const q = `
SELECT canonical_scheme, canonical_host, path, blob_hash, upstream_url,
       is_metadata, last_requested_at, request_count, last_fetched_at,
       upstream_etag, upstream_lastmod
FROM url_path
WHERE canonical_scheme = ? AND canonical_host = ? AND path = ?`
	var u URLPath
	var isMD int64
	err := c.db.QueryRowContext(ctx, q, scheme, host, path).Scan(
		&u.CanonicalScheme, &u.CanonicalHost, &u.Path,
		&u.BlobHash, &u.UpstreamURL, &isMD,
		&u.LastRequestedAt, &u.RequestCount, &u.LastFetchedAt,
		&u.UpstreamETag, &u.UpstreamLastMod,
	)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("LookupURL: %w", err)
	}
	u.IsMetadata = isMD != 0
	return &u, nil
}

// PutURLPath inserts-or-replaces the url_path row. blob_hash, last_*,
// upstream_etag, and upstream_lastmod are all carried through — pass nil
// pointers for fields that should remain SQL NULL.
func (c *Cache) PutURLPath(ctx context.Context, u URLPath) error {
	const q = `
INSERT INTO url_path
  (canonical_scheme, canonical_host, path, blob_hash, upstream_url,
   is_metadata, last_requested_at, request_count, last_fetched_at,
   upstream_etag, upstream_lastmod)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(canonical_scheme, canonical_host, path) DO UPDATE SET
  blob_hash         = excluded.blob_hash,
  upstream_url      = excluded.upstream_url,
  is_metadata       = excluded.is_metadata,
  last_requested_at = excluded.last_requested_at,
  request_count     = excluded.request_count,
  last_fetched_at   = excluded.last_fetched_at,
  upstream_etag     = excluded.upstream_etag,
  upstream_lastmod  = excluded.upstream_lastmod`
	isMD := int64(0)
	if u.IsMetadata {
		isMD = 1
	}
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, q,
			u.CanonicalScheme, u.CanonicalHost, u.Path, u.BlobHash, u.UpstreamURL,
			isMD, u.LastRequestedAt, u.RequestCount, u.LastFetchedAt,
			u.UpstreamETag, u.UpstreamLastMod)
		if err != nil {
			return fmt.Errorf("PutURLPath: %w", err)
		}
		return nil
	})
}

// TouchURLPath records that a request was just served from cache for this
// (scheme, host, path). Increments request_count and updates
// last_requested_at. Cheap, hot-path operation; safe to fire-and-forget
// on cache hits.
func (c *Cache) TouchURLPath(ctx context.Context, scheme, host, path string) error {
	const q = `
UPDATE url_path
SET request_count = request_count + 1,
    last_requested_at = ?
WHERE canonical_scheme = ? AND canonical_host = ? AND path = ?`
	now := nowUnix()
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, q, now, scheme, host, path)
		if err != nil {
			return fmt.Errorf("TouchURLPath: %w", err)
		}
		return nil
	})
}

// HostHasBlob reports whether (scheme, host) has any url_path row
// pointing at blobHash. Used by the by-hash content-addressed fallback
// to verify that the blob was previously fetched through this host's
// allowlist before serving it under a new URL — without this gate, a
// caller who learns a SHA256 from one host's content could request it
// under any other allowlisted host and receive bytes that were never
// vouched for by that host. ErrInvalidHash on a malformed hash; nil
// (false, no error) when the row simply does not exist.
func (c *Cache) HostHasBlob(ctx context.Context, scheme, host, blobHash string) (bool, error) {
	if !validBlobHash(blobHash) {
		return false, fmt.Errorf("%w: %q", ErrInvalidHash, blobHash)
	}
	const q = `
SELECT 1 FROM url_path
WHERE canonical_scheme = ? AND canonical_host = ? AND blob_hash = ?
LIMIT 1`
	var one int
	err := c.db.QueryRowContext(ctx, q, scheme, host, blobHash).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("HostHasBlob: %w", err)
	}
	return true, nil
}

// GetBlob returns the blob row for hash, or ErrNotFound. Returns
// ErrInvalidHash without touching the DB if the hash is malformed.
func (c *Cache) GetBlob(ctx context.Context, hash string) (*Blob, error) {
	if !validBlobHash(hash) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidHash, hash)
	}
	const q = `SELECT hash, size, created_at, refcount FROM blob WHERE hash = ?`
	var b Blob
	err := c.db.QueryRowContext(ctx, q, hash).Scan(&b.Hash, &b.Size, &b.CreatedAt, &b.RefCount)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("GetBlob: %w", err)
	}
	return &b, nil
}

// PutBlob inserts a blob row. If a row with this hash already exists, it
// is left untouched (created_at and refcount must stay stable). Use this
// after a successful BlobWriter.Finalize.
//
// The schema also CHECKs that hash matches sha256-hex shape, so this Go
// validation is defense-in-depth. We surface ErrInvalidHash before
// submitting to the writer goroutine so a buggy caller fails immediately.
func (c *Cache) PutBlob(ctx context.Context, hash string, size int64) error {
	if !validBlobHash(hash) {
		return fmt.Errorf("%w: %q", ErrInvalidHash, hash)
	}
	const q = `
INSERT INTO blob (hash, size, created_at, refcount)
VALUES (?, ?, ?, 0)
ON CONFLICT(hash) DO NOTHING`
	now := nowUnix()
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, q, hash, size, now)
		if err != nil {
			return fmt.Errorf("PutBlob: %w", err)
		}
		return nil
	})
}

// GetSuiteFreshness returns the suite_freshness row for the suite, or
// ErrNotFound when no check has ever been recorded.
func (c *Cache) GetSuiteFreshness(ctx context.Context, scheme, host, suitePath string) (*SuiteFreshness, error) {
	const q = `
SELECT canonical_scheme, canonical_host, suite_path,
       last_check_at, last_success_at,
       inrelease_etag, inrelease_lastmod, inrelease_change_seen_at,
       current_snapshot_id
FROM suite_freshness
WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`
	var s SuiteFreshness
	err := c.db.QueryRowContext(ctx, q, scheme, host, suitePath).Scan(
		&s.CanonicalScheme, &s.CanonicalHost, &s.SuitePath,
		&s.LastCheckAt, &s.LastSuccessAt,
		&s.InReleaseETag, &s.InReleaseLastMod, &s.InReleaseChangeSeenAt,
		&s.CurrentSnapshotID,
	)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("GetSuiteFreshness: %w", err)
	}
	return &s, nil
}

// ListSuites returns every suite_freshness row, in no guaranteed order.
// Used by the periodic freshness scheduler (SPEC §7.4) to pick suites
// whose last_success_at is older than periodic_refresh.
//
// AIDEV-NOTE: this is a full-table scan with no LIMIT. Phase 1 deployments
// are expected to track tens — at worst low hundreds — of suites
// (canonical_host × suite_codename), so the scan is cheap. If a future
// phase pushes this number into the thousands, the caller should switch
// to a chunked scan keyed on (canonical_scheme, canonical_host, suite_path).
func (c *Cache) ListSuites(ctx context.Context) ([]SuiteFreshness, error) {
	const q = `
SELECT canonical_scheme, canonical_host, suite_path,
       last_check_at, last_success_at,
       inrelease_etag, inrelease_lastmod, inrelease_change_seen_at,
       current_snapshot_id
FROM suite_freshness`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("ListSuites: %w", err)
	}
	defer rows.Close()
	var out []SuiteFreshness
	for rows.Next() {
		var s SuiteFreshness
		if err := rows.Scan(
			&s.CanonicalScheme, &s.CanonicalHost, &s.SuitePath,
			&s.LastCheckAt, &s.LastSuccessAt,
			&s.InReleaseETag, &s.InReleaseLastMod, &s.InReleaseChangeSeenAt,
			&s.CurrentSnapshotID,
		); err != nil {
			return nil, fmt.Errorf("ListSuites scan: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListSuites iter: %w", err)
	}
	return out, nil
}

// PutSuiteFreshness upserts the per-suite freshness state.
func (c *Cache) PutSuiteFreshness(ctx context.Context, s SuiteFreshness) error {
	const q = `
INSERT INTO suite_freshness
  (canonical_scheme, canonical_host, suite_path,
   last_check_at, last_success_at,
   inrelease_etag, inrelease_lastmod, inrelease_change_seen_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(canonical_scheme, canonical_host, suite_path) DO UPDATE SET
  last_check_at            = excluded.last_check_at,
  last_success_at          = excluded.last_success_at,
  inrelease_etag           = excluded.inrelease_etag,
  inrelease_lastmod        = excluded.inrelease_lastmod,
  inrelease_change_seen_at = excluded.inrelease_change_seen_at`
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, q,
			s.CanonicalScheme, s.CanonicalHost, s.SuitePath,
			s.LastCheckAt, s.LastSuccessAt,
			s.InReleaseETag, s.InReleaseLastMod, s.InReleaseChangeSeenAt)
		if err != nil {
			return fmt.Errorf("PutSuiteFreshness: %w", err)
		}
		return nil
	})
}
