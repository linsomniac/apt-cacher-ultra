package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
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
// from phase D the handler: on a miss, StoreFetched (the same writer
// transaction as the anchor's url_path row, before the body is served);
// on a suite-row creation, the creating transaction (createSuiteTx's
// markCreated: SuiteSeed.SignedSeenAt and every cached signed anchor in
// SuiteSeed.Cached, ruling PD-5), and only for an unproven creation the
// handler's markCachedSignedAnchors backstop; from phase E the freshness
// checker too, through MarkSignedSeenBatch, when its probe finds a
// sniff-passing InRelease or Release.gpg for a suite without a signed
// current snapshot (spec C3 revision 8, design review I-2), and the
// adopter's re-mark before an unsigned commit (ruling R-E1). It is written
// only for suites with a suite_freshness row, and never for an unsigned
// snapshot or a plain-text anchor. Every suite-row delete (deleteSuiteTx:
// cap eviction, a reap, POST /forget) moves its base's existing markers'
// stamps up to the delete, in the transaction that deletes the row, and
// never creates one (stampBaseMarkersTx; design review M-9, ruling PF-10);
// POST /forget (ForgetSuite) and the orphan sweep delete markers.
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
					return fmt.Errorf("MarkSignedSeenBatch: %w", err) // deferred minor A-25
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

// rowQuerier is what refuseUnsignedDowngrade reads through: CommitAdoption's
// writer transaction, or the read pool.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// CheckUnsignedDowngrade is spec C5's never-downgrade rule
// (refuseUnsignedDowngrade) read outside a transaction, for the early
// exits: the check's before an unsigned request takes a slot, the
// adopter's before anything is downloaded. CommitAdoption re-reads the same
// rule inside its writer transaction; that read is the authority.
func (c *Cache) CheckUnsignedDowngrade(ctx context.Context, host, suite string) error {
	return refuseUnsignedDowngrade(ctx, c.db, host, suite)
}

// refuseUnsignedDowngrade is the never-downgrade rule for an unsigned
// candidate of suite (spec C5 safeguard 3): ErrUnsignedDowngradeRefused when
// the suite has signed history on host under either scheme —
//   - a signed-seen marker for its base (aptpath.MarkerBase: the /. twin
//     shares it), or
//   - an adopted, non-retired inline or detached snapshot of the suite or
//     its twin: the current snapshot, or a displaced one the legacy trust
//     audit left queued, and so unmarked, after an I/O fault.
//
// A retired snapshot (quarantined by the audit, or reaped) is no history:
// its trust was removed. A read error is returned as it is. (Moved here
// from adoption.go beside the markers it reads, deferred minor E-5.)
//
// AIDEV-NOTE: one rule and one SQL at both never-downgrade points, so the
// early exits can never admit what the commit refuses. Either scheme counts
// (design review M-7): a base seen signed over https protects the same base
// over plain http, the scheme a man-in-the-middle can strip. The displaced
// snapshot clause is design review M-8.
func refuseUnsignedDowngrade(ctx context.Context, q rowQuerier, host, suite string) error {
	base := aptpath.MarkerBase(suite)
	var scheme string
	err := q.QueryRowContext(ctx, `
SELECT canonical_scheme FROM suite_signed_seen
 WHERE canonical_scheme IN ('http', 'https') AND canonical_host = ? AND base = ?
 LIMIT 1`, host, base).Scan(&scheme)
	switch {
	case err == nil:
		return fmt.Errorf("%w: %s://%s%s has a signed-seen marker", ErrUnsignedDowngradeRefused, scheme, host, base)
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("unsigned downgrade check: read signed-seen markers: %w", err)
	}
	var path, form string
	err = q.QueryRowContext(ctx, `
SELECT canonical_scheme, suite_path, anchor_form FROM suite_snapshot
 WHERE canonical_scheme IN ('http', 'https') AND canonical_host = ? AND suite_path IN (?, ?)
   AND adopted_at IS NOT NULL AND retired_at IS NULL AND anchor_form IN ('inline', 'detached')
 LIMIT 1`, host, base, base+"/.").Scan(&scheme, &path, &form)
	switch {
	case err == nil:
		return fmt.Errorf("%w: %s://%s%s has an adopted %s snapshot", ErrUnsignedDowngradeRefused, scheme, host, path, form)
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("unsigned downgrade check: read signed snapshots: %w", err)
	}
	return nil
}
