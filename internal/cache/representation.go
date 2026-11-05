package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// HTTPDate validates untrusted metadata before storage or serving. Never
// manufacture an upstream modification time from a local filesystem clock.
func HTTPDate(value string) string {
	if len(value) > 128 {
		return ""
	}
	t, err := http.ParseTime(value)
	if err != nil || t.IsZero() || t.After(time.Unix(nowUnix(), 0)) {
		return ""
	}
	return t.UTC().Format(http.TimeFormat)
}

const putRepresentationSQL = `INSERT INTO representation_metadata
 (canonical_scheme, canonical_host, path, blob_hash, last_modified, observed_at) VALUES (?, ?, ?, ?, ?, ?)
 ON CONFLICT(canonical_scheme, canonical_host, path, blob_hash)
 DO UPDATE SET last_modified = excluded.last_modified, observed_at = excluded.observed_at`

// PutRepresentation records a date only for this URL and these exact bytes.
// Call after integrity validation and PutBlob. An empty date replaces an old
// value too: a later response without a usable date must not revive it.
func (c *Cache) PutRepresentation(ctx context.Context, scheme, host, path, hash, lastmod string) error {
	if !validBlobHash(hash) {
		return ErrInvalidHash
	}
	date := HTTPDate(lastmod)
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, putRepresentationSQL, scheme, host, path, hash, date, nowUnix())
		return err
	})
}

// pruneRepresentationMetadataTx releases dates whose URL and snapshot owners
// have gone away, even when another URL keeps the shared blob alive. A day's
// grace protects staged adoption records (normally staged for at most 30m).
// Metadata never pins content. The batch bound also applies when no blobs are
// eligible for deletion, so a live deduplicated blob cannot retain dates forever.
func pruneRepresentationMetadataTx(ctx context.Context, tx *sql.Tx, now int64, limit int) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM representation_metadata WHERE rowid IN (
 SELECT r.rowid FROM representation_metadata r
 WHERE r.observed_at < ?
   AND NOT EXISTS (SELECT 1 FROM url_path u WHERE u.canonical_scheme = r.canonical_scheme
     AND u.canonical_host = r.canonical_host AND u.path = r.path AND u.blob_hash = r.blob_hash)
   AND NOT EXISTS (SELECT 1 FROM snapshot_member m JOIN suite_snapshot s ON s.snapshot_id = m.snapshot_id
     WHERE m.blob_hash = r.blob_hash AND s.canonical_scheme = r.canonical_scheme
       AND s.canonical_host = r.canonical_host AND s.suite_path || '/' || m.path = r.path)
 ORDER BY r.observed_at LIMIT ?)`, now-24*60*60, limit)
	return err
}

func (c *Cache) RepresentationLastModified(ctx context.Context, scheme, host, path, hash string) (string, error) {
	var date string
	err := c.db.QueryRowContext(ctx, `SELECT last_modified FROM representation_metadata
 WHERE canonical_scheme = ? AND canonical_host = ? AND path = ? AND blob_hash = ?`,
		scheme, host, path, hash).Scan(&date)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("representation metadata: %w", err)
	}
	return HTTPDate(date), nil
}
