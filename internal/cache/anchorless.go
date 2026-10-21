package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
)

// suiteRowNames are the member names ListSuiteMetadataRows probes by key:
// spec C2's anchors and root indexes — Packages and Sources, plain or with
// one of the six compression extensions (the names
// unadoptedFlatIndexRowsSQL probes too).
var suiteRowNames = func() []string {
	names := []string{"InRelease", "Release", "Release.gpg"}
	for _, stem := range []string{"Packages", "Sources"} {
		for _, ext := range []string{"", ".gz", ".xz", ".bz2", ".lzma", ".lz4", ".zst"} {
			names = append(names, stem+ext)
		}
	}
	return names
}()

// listSuiteMetadataRowsSQL is ListSuiteMetadataRows' query: url_path's
// key probed once per suiteRowNames name under the suite, and one key
// range over the suite's Contents-* names ("Contents-" up to "Contents.",
// the next byte). Two SELECTs, not one with an OR: SQLite plans that OR
// as a range over the whole host.
var listSuiteMetadataRowsSQL = `
SELECT canonical_scheme, canonical_host, path, blob_hash, upstream_url,
       is_metadata, last_requested_at, request_count, last_fetched_at,
       upstream_etag, upstream_lastmod
  FROM url_path
 WHERE canonical_scheme = ? AND canonical_host = ?
   AND path IN (` + strings.TrimSuffix(strings.Repeat("?, ", len(suiteRowNames)), ", ") + `)
UNION ALL
SELECT canonical_scheme, canonical_host, path, blob_hash, upstream_url,
       is_metadata, last_requested_at, request_count, last_fetched_at,
       upstream_etag, upstream_lastmod
  FROM url_path
 WHERE canonical_scheme = ? AND canonical_host = ? AND path >= ? AND path < ?
 ORDER BY path`

// ListSuiteMetadataRows returns key's suite's anchor, root index and
// Contents-* url_path rows, in path order: the rows of key's scheme and
// host named InRelease, Release, Release.gpg, Packages*, Sources* or
// Contents-* directly under the suite whose aptpath.SuitePath is the suite
// — never the "/." twin's, a nested or sibling suite's, its .debs, its
// by-hash names or pdiff Index (no caller reads them), or a file no
// grammar maps to it. A key no grammar produces
// (aptpath.IsCanonicalSuitePath) has none. Spec C4: Dispatch 3 reads a
// flat suite's client-created index rows from it, and the anchorless
// refresh revalidates the indexes and deletes the anchors upstream
// dropped.
//
// AIDEV-NOTE: design review M-5 (phase G). The read probes url_path's key
// by name (17 names) plus one Contents-* range, never a range over the
// suite's directory: a Gemfury-style repository keeps its .debs in the
// suite's own directory, so a directory range read every cached .deb of
// the repository, twice per check. TestListSuiteMetadataRows_ProbesURLPathByItsKey
// pins the plan; keep suiteRowNames in step with spec C2's flat member
// grammar (aptpath.flatMemberRE).
func (c *Cache) ListSuiteMetadataRows(ctx context.Context, key SuiteKey) ([]URLPath, error) {
	if !aptpath.IsCanonicalSuitePath(key.SuitePath) {
		return nil, nil
	}
	args := make([]any, 0, len(suiteRowNames)+6)
	args = append(args, key.CanonicalScheme, key.CanonicalHost)
	for _, n := range suiteRowNames {
		args = append(args, key.SuitePath+"/"+n)
	}
	args = append(args, key.CanonicalScheme, key.CanonicalHost, key.SuitePath+"/Contents-", key.SuitePath+"/Contents.")
	rows, err := c.db.QueryContext(ctx, listSuiteMetadataRowsSQL, args...)
	if err != nil {
		return nil, fmt.Errorf("ListSuiteMetadataRows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []URLPath
	for rows.Next() {
		var (
			u    URLPath
			isMD int64
		)
		if err := rows.Scan(&u.CanonicalScheme, &u.CanonicalHost, &u.Path, &u.BlobHash, &u.UpstreamURL,
			&isMD, &u.LastRequestedAt, &u.RequestCount, &u.LastFetchedAt, &u.UpstreamETag, &u.UpstreamLastMod); err != nil {
			return nil, fmt.Errorf("ListSuiteMetadataRows: scan: %w", err)
		}
		if aptpath.SuitePath(u.Path) != key.SuitePath {
			continue
		}
		u.IsMetadata = isMD != 0
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListSuiteMetadataRows: %w", err)
	}
	return out, nil
}

// RefreshURLPathBlob is the anchorless refresh's write (spec C4 Dispatch
// 4: "a dedicated cache method"). It points the url_path row of scheme,
// host and path from blob old to blob newHash — already in the pool with
// its blob row: the caller wrote it outside the writer (ruling PD-5) —
// with the response's validators (nil stores NULL), and reports whether
// it did. It never writes last_requested_at or request_count, and it sets
// last_fetched_at only when the blob changes, to MAX(now, previous + 1)
// (plan design note 13: a replacement within the second of the previous
// fetch still moves Last-Modified forward). A row that changed since the
// caller read it — a miss stored newer bytes, url_path GC deleted it — is
// left alone (false): the refresh never overwrites what a client's own
// fetch just stored. Like the miss path's upsert it changes no blob
// refcount: blob GC finds a url_path row's blob through its NOT EXISTS
// clause.
//
// AIDEV-NOTE: ruling R-G3. url_path-served metadata answers Last-Modified
// = max(pool mtime, last_fetched_at) (the handler's urlPathModTime), and
// adoptionStampTx's first-adoption floor stays above it only while no
// pool file is newer than its row's last_fetched_at: the caller finalizes
// the pool file before this stamps the row, as runFetch does. Keep that
// order.
func (c *Cache) RefreshURLPathBlob(ctx context.Context, scheme, host, path, old, newHash string, etag, lastmod *string) (bool, error) {
	if !validBlobHash(old) || !validBlobHash(newHash) {
		return false, fmt.Errorf("RefreshURLPathBlob: %w", ErrInvalidHash)
	}
	now := nowUnix()
	updated := false
	err := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `
UPDATE url_path
   SET blob_hash        = ?,
       upstream_etag    = ?,
       upstream_lastmod = ?,
       last_fetched_at  = CASE WHEN blob_hash = ? THEN last_fetched_at
                               ELSE MAX(?, COALESCE(last_fetched_at, 0) + 1) END
 WHERE canonical_scheme = ? AND canonical_host = ? AND path = ? AND blob_hash = ?`,
			newHash, etag, lastmod, newHash, now, scheme, host, path, old)
		if err != nil {
			return fmt.Errorf("RefreshURLPathBlob: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("RefreshURLPathBlob: rows: %w", err)
		}
		updated = n > 0
		return nil
	})
	if err != nil {
		return false, err
	}
	return updated, nil
}

// DeleteURLPathRows deletes, in one writer transaction, each url_path row
// of rows (by its scheme, host and path) that still holds the blob it was
// read with (BlobHash; nil for a row without one), as DeleteSuiteMetadata
// deletes a suite's (deleteURLPathRowTx: the row's pin released once),
// and reports how many it deleted: the anchorless refresh's deletes (spec
// C4 Dispatch 4: an index or a cached anchor upstream answers 404 or 410,
// a changed plain Contents-*). No suite row is touched.
//
// AIDEV-NOTE: design review M-4. A row a client's miss stored anew between
// the refresh's read and this delete — a freshly published InRelease, new
// index bytes — holds another blob and is left alone, the compare
// RefreshURLPathBlob makes: the refresh never deletes what a client's own
// fetch just stored.
func (c *Cache) DeleteURLPathRows(ctx context.Context, rows []URLPath) (int, error) {
	n := 0
	err := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("DeleteURLPathRows: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		now, deleted := nowUnix(), 0
		for _, r := range rows {
			var blob sql.NullString
			err := tx.QueryRowContext(ctx, `
SELECT blob_hash FROM url_path WHERE canonical_scheme = ? AND canonical_host = ? AND path = ?`,
				r.CanonicalScheme, r.CanonicalHost, r.Path).Scan(&blob)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				continue
			case err != nil:
				return fmt.Errorf("DeleteURLPathRows: read %s://%s%s: %w", r.CanonicalScheme, r.CanonicalHost, r.Path, err)
			}
			if blob.Valid != (r.BlobHash != nil) || (r.BlobHash != nil && blob.String != *r.BlobHash) {
				continue
			}
			if err := deleteURLPathRowTx(ctx, tx, r.CanonicalScheme, r.CanonicalHost, r.Path, blob, now); err != nil {
				return fmt.Errorf("DeleteURLPathRows: %w", err)
			}
			deleted++
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("DeleteURLPathRows: commit: %w", err)
		}
		n = deleted
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}
