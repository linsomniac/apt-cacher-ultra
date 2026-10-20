package cache

// Spec C9: the orphan signed-seen marker sweep (rulings R-F5, PF-1).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// markerSweepBatch bounds the markers one writer transaction of the sweep
// deletes.
const markerSweepBatch = 1000

// MarkerSweep is one SweepOrphanMarkers call's result.
type MarkerSweep struct {
	// Swept is how many orphan markers were deleted.
	Swept int
	// Furthest is the marker stamped furthest past now (more than
	// futureStampSlack: a clock step), with that stamp in SeenAt; nil when
	// none is. The sweep leaves its stamp as it is.
	Furthest *SignedSeenMark
}

// SweepOrphanMarkers is spec C9's orphan-marker sweep (ruling R-F5). It
// reads the marker stamped furthest past now + futureStampSlack, if any
// (Furthest; one read on the pool), then deletes, in writer transactions
// of at most markerSweepBatch, the markers last seen before cutoff whose
// base no suite row of their host has any more — not the base itself, not
// its "/." twin, under neither scheme — each delete re-checking that, and
// the stamp, in its transaction. On an error, Swept counts the batches
// already committed.
//
// AIDEV-NOTE: ruling PF-1 (design review I-1; it overrides R-F5's
// "healed"): the sweep never rewrites a stamp. A future stamp reads as
// now: it is never an orphan candidate (last_seen_at < cutoff excludes
// it) and it is reported (Furthest). The sweep cannot tell a stamp written
// while the clock was ahead from a clock that is behind now, and lowering
// the stamp in the second case deletes signed history once the clock is
// corrected (TestSweepOrphanMarkers_BackwardStepThenCorrectionSweepsNothing),
// so a future stamp ages out only once the clock passes it; markSignedSeen's
// MAX never lowers one either. Never-downgrade reads a base's markers under
// either scheme (spec C5, design review M-7), so a marker is an orphan only
// while no suite of its host, under either scheme, is its base or the
// base's twin: a live suite's marker is never swept, so no renewal write is
// needed (spec C3).
func (c *Cache) SweepOrphanMarkers(ctx context.Context, now, cutoff int64) (MarkerSweep, error) {
	var res MarkerSweep
	var m SignedSeenMark
	err := c.db.QueryRowContext(ctx, `
SELECT canonical_scheme, canonical_host, base, last_seen_at FROM suite_signed_seen
 WHERE last_seen_at > ?
 ORDER BY last_seen_at DESC LIMIT 1`, now+futureStampSlack).Scan(&m.CanonicalScheme, &m.CanonicalHost, &m.Base, &m.SeenAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return res, fmt.Errorf("SweepOrphanMarkers: read future stamps: %w", err)
	default:
		res.Furthest = &m
	}
	for {
		keys, err := c.orphanMarkers(ctx, cutoff)
		if err != nil {
			return res, err
		}
		if len(keys) == 0 {
			return res, nil
		}
		n, err := c.deleteOrphanMarkers(ctx, keys, cutoff)
		res.Swept += n
		if err != nil {
			return res, err
		}
		if n == 0 || len(keys) < markerSweepBatch {
			return res, nil
		}
	}
}

// orphanSuiteRowSQL is the orphan test, given a marker's host and base
// (twice: the base and its twin): a suite row of that host, under either
// scheme, at the base or at the base's "/." twin.
const orphanSuiteRowSQL = `
SELECT 1 FROM suite_freshness sf
 WHERE sf.canonical_scheme IN ('http', 'https') AND sf.canonical_host = %s
   AND sf.suite_path IN (%s, %s || '/.')`

// orphanMarkers lists up to markerSweepBatch orphan markers last seen
// before cutoff, in key order (one read on the pool).
func (c *Cache) orphanMarkers(ctx context.Context, cutoff int64) ([]SignedSeenMark, error) {
	rows, err := c.db.QueryContext(ctx, `
SELECT m.canonical_scheme, m.canonical_host, m.base FROM suite_signed_seen m
 WHERE m.last_seen_at < ?
   AND NOT EXISTS (`+fmt.Sprintf(orphanSuiteRowSQL, "m.canonical_host", "m.base", "m.base")+`)
 ORDER BY m.canonical_scheme, m.canonical_host, m.base
 LIMIT ?`, cutoff, markerSweepBatch)
	if err != nil {
		return nil, fmt.Errorf("SweepOrphanMarkers: list orphans: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SignedSeenMark
	for rows.Next() {
		var m SignedSeenMark
		if err := rows.Scan(&m.CanonicalScheme, &m.CanonicalHost, &m.Base); err != nil {
			return nil, fmt.Errorf("SweepOrphanMarkers: scan: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("SweepOrphanMarkers: list orphans: %w", err)
	}
	return out, nil
}

// deleteOrphanMarkers deletes keys in one writer transaction, each only if
// it is still an orphan last seen before cutoff, and returns how many it
// deleted.
func (c *Cache) deleteOrphanMarkers(ctx context.Context, keys []SignedSeenMark, cutoff int64) (int, error) {
	n := 0
	err := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("SweepOrphanMarkers: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		deleted := 0
		for _, k := range keys {
			r, err := tx.ExecContext(ctx, `
DELETE FROM suite_signed_seen
 WHERE canonical_scheme = ? AND canonical_host = ? AND base = ? AND last_seen_at < ?
   AND NOT EXISTS (`+fmt.Sprintf(orphanSuiteRowSQL, "?", "?", "?")+`)`,
				k.CanonicalScheme, k.CanonicalHost, k.Base, cutoff, k.CanonicalHost, k.Base, k.Base)
			if err != nil {
				return fmt.Errorf("SweepOrphanMarkers: delete %s://%s%s: %w", k.CanonicalScheme, k.CanonicalHost, k.Base, err)
			}
			m, err := r.RowsAffected()
			if err != nil {
				return fmt.Errorf("SweepOrphanMarkers: delete: %w", err)
			}
			deleted += int(m)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("SweepOrphanMarkers: commit: %w", err)
		}
		n = deleted
		return nil
	})
	if err != nil {
		return 0, err // n is the writer's until the op returns
	}
	return n, nil
}
