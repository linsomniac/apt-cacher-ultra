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
func pruneRepresentationMetadataTx(ctx context.Context, tx *sql.Tx, now int64, limit int) (int, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM representation_metadata WHERE rowid IN (
 SELECT r.rowid FROM representation_metadata r
 WHERE r.observed_at < ?
   AND NOT EXISTS (SELECT 1 FROM url_path u WHERE u.canonical_scheme = r.canonical_scheme
     AND u.canonical_host = r.canonical_host AND u.path = r.path AND u.blob_hash = r.blob_hash)
   AND NOT EXISTS (SELECT 1 FROM snapshot_member m JOIN suite_snapshot s ON s.snapshot_id = m.snapshot_id
     WHERE m.blob_hash = r.blob_hash AND s.canonical_scheme = r.canonical_scheme
       AND s.canonical_host = r.canonical_host AND s.suite_path || '/' || m.path = r.path)
 ORDER BY r.observed_at LIMIT ?)`, now-24*60*60, limit)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// PruneRepresentationMetadata runs pruneRepresentationMetadataTx in one
// writer transaction and reports how many dates it released: at most limit,
// oldest first.
//
// AIDEV-NOTE: the merge review's M-I1 (the maintainer, 2026-11-06). The
// prune ran at the top of every RunBlobGCBatch transaction, once per blob
// batch on the single writer; its scan walks every day-old date that is
// still owned before it finds a prunable one (0.56-0.64 s a call at
// 200,000 url_path rows), so a blob pass of 100 batches held the writer a
// minute longer. internal/gc now calls this once a tick, at the start of
// its blob pass (runBlobPass), and the startup pass does so once too.
func (c *Cache) PruneRepresentationMetadata(ctx context.Context, limit int) (int, error) {
	if limit < 1 {
		return 0, fmt.Errorf("PruneRepresentationMetadata: limit must be >= 1, got %d", limit)
	}
	var pruned int
	err := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("PruneRepresentationMetadata: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		n, err := pruneRepresentationMetadataTx(ctx, tx, nowUnix(), limit)
		if err != nil {
			return fmt.Errorf("PruneRepresentationMetadata: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("PruneRepresentationMetadata: commit: %w", err)
		}
		pruned = n
		return nil
	})
	if err != nil {
		return 0, err
	}
	return pruned, nil
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
