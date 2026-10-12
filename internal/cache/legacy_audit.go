package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
)

// LegacyAuditItem is one queued pre-v7 snapshot (spec §6.1). Missing is
// set when the snapshot row no longer exists (snapshot GC reaped it).
type LegacyAuditItem struct {
	SnapshotID       int64
	CanonicalScheme  string
	CanonicalHost    string
	SuitePath        string
	Form             AnchorForm
	SingleAnchorHash *string
	Missing          bool
}

// LegacyAuditOutcome is the audit's decision for one queued snapshot.
// The zero value is invalid on purpose.
type LegacyAuditOutcome int

const (
	// LegacyAuditKeep: signed and clean — mark signed-seen, dequeue.
	LegacyAuditKeep LegacyAuditOutcome = iota + 1
	// LegacyAuditDrop: the snapshot row is gone — dequeue.
	LegacyAuditDrop
	// LegacyAuditQuarantine: remove all trust, no marker.
	LegacyAuditQuarantine
	// LegacyAuditQuarantineSigned: remove all trust, but the envelope
	// proves the upstream signs — mark signed-seen.
	LegacyAuditQuarantineSigned
)

// LegacyAuditDecision pairs a queued snapshot with its outcome.
type LegacyAuditDecision struct {
	SnapshotID int64
	Outcome    LegacyAuditOutcome
}

// LegacyAuditBatch returns up to limit queued items with snapshot_id >
// afterID, in snapshot_id order; limit <= 0 returns all of them. Paging by
// id lets the audit move past items it deferred (left queued).
func (c *Cache) LegacyAuditBatch(ctx context.Context, afterID int64, limit int) ([]LegacyAuditItem, error) {
	if limit <= 0 {
		limit = -1 // SQLite: no limit
	}
	rows, err := c.db.QueryContext(ctx, `
SELECT q.snapshot_id, s.snapshot_id IS NULL,
       COALESCE(s.canonical_scheme, ''), COALESCE(s.canonical_host, ''), COALESCE(s.suite_path, ''),
       COALESCE(s.anchor_form, ''), s.inrelease_hash
  FROM legacy_trust_audit q
  LEFT JOIN suite_snapshot s ON s.snapshot_id = q.snapshot_id
 WHERE q.snapshot_id > ?
 ORDER BY q.snapshot_id
 LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("LegacyAuditBatch: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []LegacyAuditItem
	for rows.Next() {
		var it LegacyAuditItem
		var form string
		if err := rows.Scan(&it.SnapshotID, &it.Missing, &it.CanonicalScheme, &it.CanonicalHost,
			&it.SuitePath, &form, &it.SingleAnchorHash); err != nil {
			return nil, fmt.Errorf("LegacyAuditBatch: scan: %w", err)
		}
		it.Form = AnchorForm(form)
		out = append(out, it)
	}
	return out, rows.Err()
}

// ApplyLegacyAuditDecisions applies one batch of audit decisions in ONE
// writer transaction (spec §6.1) and reports which quarantined snapshots
// were current. An invalid outcome rolls the whole batch back.
//
// AIDEV-NOTE: every effect is idempotent — a quarantine finds an already
// handled snapshot non-current (no second refcount release) and already
// retired; a repeated marker keeps the newest stamp — so a crash between
// batches never double-applies. Never release pins for a non-current
// snapshot: displacement already did.
func (c *Cache) ApplyLegacyAuditDecisions(ctx context.Context, decisions []LegacyAuditDecision) (map[int64]bool, error) {
	wasCurrent := make(map[int64]bool)
	err := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("ApplyLegacyAuditDecisions: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		now := nowUnix()
		for _, d := range decisions {
			switch d.Outcome {
			case LegacyAuditKeep:
				if err := markSnapshotBaseTx(ctx, tx, d.SnapshotID); err != nil {
					return err
				}
			case LegacyAuditDrop:
			case LegacyAuditQuarantine, LegacyAuditQuarantineSigned:
				cur, err := quarantineSnapshotTx(ctx, tx, d.SnapshotID, now)
				if err != nil {
					return err
				}
				wasCurrent[d.SnapshotID] = cur
				if d.Outcome == LegacyAuditQuarantineSigned {
					if err := markSnapshotBaseTx(ctx, tx, d.SnapshotID); err != nil {
						return err
					}
				}
			default:
				return fmt.Errorf("ApplyLegacyAuditDecisions: snapshot %d: invalid outcome %d", d.SnapshotID, d.Outcome)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM legacy_trust_audit WHERE snapshot_id = ?`, d.SnapshotID); err != nil {
				return fmt.Errorf("ApplyLegacyAuditDecisions: dequeue %d: %w", d.SnapshotID, err)
			}
		}
		return tx.Commit()
	})
	if err != nil {
		return nil, err
	}
	return wasCurrent, nil
}

// quarantineSnapshotTx removes all trust from a snapshot: if it is a
// suite's current snapshot, the pointer and that suite's validators are
// cleared and its pins released; it is retired.
func quarantineSnapshotTx(ctx context.Context, tx *sql.Tx, snapshotID, now int64) (wasCurrent bool, err error) {
	res, err := tx.ExecContext(ctx, `
UPDATE suite_freshness
   SET current_snapshot_id = NULL, inrelease_etag = NULL,
       inrelease_lastmod = NULL, inrelease_change_seen_at = NULL
 WHERE current_snapshot_id = ?`, snapshotID)
	if err != nil {
		return false, fmt.Errorf("quarantine %d: clear pointer: %w", snapshotID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("quarantine %d: rows: %w", snapshotID, err)
	}
	if n > 0 {
		if err := decrementSnapshotRefcountsTx(ctx, tx, snapshotID, now); err != nil {
			return false, fmt.Errorf("quarantine %d: %w", snapshotID, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE suite_snapshot SET retired_at = ? WHERE snapshot_id = ? AND retired_at IS NULL`, now, snapshotID); err != nil {
		return false, fmt.Errorf("quarantine %d: retire: %w", snapshotID, err)
	}
	return n > 0, nil
}

// markSnapshotBaseTx records the signed-seen marker for a snapshot's base
// when its suite is tracked (spec C3), stamped with when it was adopted
// (or created, for a never-adopted candidate). An unsigned snapshot is
// never trust-bearing and is never marked.
func markSnapshotBaseTx(ctx context.Context, tx *sql.Tx, snapshotID int64) error {
	var scheme, host, suite string
	var stamp int64
	err := tx.QueryRowContext(ctx, `
SELECT s.canonical_scheme, s.canonical_host, s.suite_path, COALESCE(s.adopted_at, s.created_at)
  FROM suite_snapshot s
 WHERE s.snapshot_id = ?
   AND s.anchor_form <> 'unsigned'
   AND EXISTS (SELECT 1 FROM suite_freshness f
                WHERE f.canonical_scheme = s.canonical_scheme
                  AND f.canonical_host = s.canonical_host
                  AND f.suite_path = s.suite_path)`, snapshotID).Scan(&scheme, &host, &suite, &stamp)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil // gone or untracked: no marker
	case err != nil:
		return fmt.Errorf("read snapshot %d for marker: %w", snapshotID, err)
	}
	return markSignedSeen(ctx, tx, scheme, host, aptpath.MarkerBase(suite), stamp)
}

// EnqueueLegacyTrustAudit queues snapshots for the startup audit. The v7
// migration enqueues every adopted pre-v7 snapshot itself; this exists for tests
// and for operator tooling that wants a snapshot re-audited.
func (c *Cache) EnqueueLegacyTrustAudit(ctx context.Context, ids ...int64) error {
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		for _, id := range ids {
			if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO legacy_trust_audit (snapshot_id) VALUES (?)`, id); err != nil {
				return fmt.Errorf("EnqueueLegacyTrustAudit: %w", err)
			}
		}
		return nil
	})
}

// TrackedAnchor is a cached InRelease or Release.gpg url_path row of a
// suite that has a suite_freshness row. SeenAt is the newer of the row's
// last request and last fetch (CommitAdoption-seeded anchors have no
// request stamp).
type TrackedAnchor struct {
	CanonicalScheme string
	CanonicalHost   string
	SuitePath       string
	Path            string
	BlobHash        string
	SeenAt          int64
}

// ListTrackedSuiteAnchors returns every tracked suite's cached signed
// anchor rows (spec C3 startup marker pass). Rows without a blob or a
// stamp are skipped.
func (c *Cache) ListTrackedSuiteAnchors(ctx context.Context) ([]TrackedAnchor, error) {
	rows, err := c.db.QueryContext(ctx, `
SELECT sf.canonical_scheme, sf.canonical_host, sf.suite_path, u.path, u.blob_hash,
       MAX(COALESCE(u.last_requested_at, 0), COALESCE(u.last_fetched_at, 0))
  FROM suite_freshness sf
  JOIN url_path u
    ON u.canonical_scheme = sf.canonical_scheme
   AND u.canonical_host   = sf.canonical_host
   AND u.path IN (sf.suite_path || '/InRelease', sf.suite_path || '/Release.gpg')
 WHERE u.blob_hash IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("ListTrackedSuiteAnchors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []TrackedAnchor
	for rows.Next() {
		var a TrackedAnchor
		if err := rows.Scan(&a.CanonicalScheme, &a.CanonicalHost, &a.SuitePath, &a.Path, &a.BlobHash, &a.SeenAt); err != nil {
			return nil, fmt.Errorf("ListTrackedSuiteAnchors: scan: %w", err)
		}
		if a.SeenAt > 0 && (strings.HasSuffix(a.Path, "/InRelease") || strings.HasSuffix(a.Path, "/Release.gpg")) {
			out = append(out, a)
		}
	}
	return out, rows.Err()
}
