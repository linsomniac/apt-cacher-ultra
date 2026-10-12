package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SignedSeenMark is one signed-seen observation (spec C3).
type SignedSeenMark struct {
	CanonicalScheme string
	CanonicalHost   string
	Base            string // aptpath.MarkerBase(suite)
	SeenAt          int64
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// markSignedSeen upserts one marker, keeping the newest stamp.
//
// AIDEV-NOTE: the marker is the never-downgrade authority for unsigned
// adoption (spec C5). Writers: CommitAdoption (signed forms, in its
// transaction), the startup legacy audit and anchor pass (spec §6.1), and
// from phase D the handler's miss path (same writer operation as the
// anchor's url_path row, before the body is served). It is never written for an
// unsigned snapshot or a plain-text anchor.
func markSignedSeen(ctx context.Context, ex execer, scheme, host, base string, seenAt int64) error {
	if _, err := ex.ExecContext(ctx, `
INSERT INTO suite_signed_seen (canonical_scheme, canonical_host, base, last_seen_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(canonical_scheme, canonical_host, base) DO UPDATE SET
  last_seen_at = MAX(last_seen_at, excluded.last_seen_at)`,
		scheme, host, base, seenAt); err != nil {
		return fmt.Errorf("mark signed-seen %s://%s%s: %w", scheme, host, base, err)
	}
	return nil
}

// MarkSignedSeenBatch upserts markers in one writer transaction per 500.
func (c *Cache) MarkSignedSeenBatch(ctx context.Context, marks []SignedSeenMark) error {
	for len(marks) > 0 {
		n := min(len(marks), 500)
		chunk := marks[:n]
		marks = marks[n:]
		if err := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
			tx, err := conn.BeginTx(ctx, nil)
			if err != nil {
				return fmt.Errorf("MarkSignedSeenBatch: begin: %w", err)
			}
			defer func() { _ = tx.Rollback() }()
			for _, m := range chunk {
				if err := markSignedSeen(ctx, tx, m.CanonicalScheme, m.CanonicalHost, m.Base, m.SeenAt); err != nil {
					return err
				}
			}
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("MarkSignedSeenBatch: commit: %w", err)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// SignedSeen returns the marker's last_seen_at for (scheme, host, base).
func (c *Cache) SignedSeen(ctx context.Context, scheme, host, base string) (int64, bool, error) {
	var at int64
	err := c.db.QueryRowContext(ctx, `
SELECT last_seen_at FROM suite_signed_seen
 WHERE canonical_scheme = ? AND canonical_host = ? AND base = ?`, scheme, host, base).Scan(&at)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("SignedSeen: %w", err)
	}
	return at, true, nil
}
