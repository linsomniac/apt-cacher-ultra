package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

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
	// DropAnchorRows, with a quarantine, also deletes every url_path row
	// that holds the snapshot's stored InRelease blob — the audit's
	// anchor_corrupt verdict (ruling R-H2). Set with any other outcome it
	// rolls the batch back, as an invalid outcome does.
	DropAnchorRows bool
}

// LegacyAuditEffect is what ApplyLegacyAuditDecisions did to one
// quarantined snapshot.
type LegacyAuditEffect struct {
	// WasCurrent: it was its suite's current snapshot, so the pointer and
	// validators were cleared and its pins released.
	WasCurrent bool
	// AnchorRowsDeleted counts the url_path rows of its InRelease blob
	// that DropAnchorRows deleted.
	AnchorRowsDeleted int
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

// LegacyAuditQueueNamesAnchor reports whether a snapshot still queued in
// legacy_trust_audit has hash as its stored InRelease (inrelease_hash).
// A read: the audit asks it before removing a corrupt anchor's pool file,
// which waits while another queued snapshot names that file (the H1 final
// review's prod minor 3).
func (c *Cache) LegacyAuditQueueNamesAnchor(ctx context.Context, hash string) (bool, error) {
	var queued bool
	err := c.db.QueryRowContext(ctx, `
SELECT EXISTS (SELECT 1
                 FROM legacy_trust_audit q
                 JOIN suite_snapshot s ON s.snapshot_id = q.snapshot_id
                WHERE s.inrelease_hash = ?)`, hash).Scan(&queued)
	if err != nil {
		return false, fmt.Errorf("LegacyAuditQueueNamesAnchor: %w", err)
	}
	return queued, nil
}

// PinnedBlobsOutsideAuditQueue returns up to limit hashes, in hash order,
// of the pinned blobs (refcount > 0: a current snapshot's members) that no
// snapshot queued in legacy_trust_audit names as its InRelease. A read:
// the audit's pre-flight checks their pool files — blob GC never deletes a
// blob whose refcount is positive — to tell a lost or foreign pool from
// lost anchor files (the final review's R2 I-1).
func (c *Cache) PinnedBlobsOutsideAuditQueue(ctx context.Context, limit int) ([]string, error) {
	rows, err := c.db.QueryContext(ctx, `
SELECT hash
  FROM blob
 WHERE refcount > 0
   AND hash NOT IN (SELECT s.inrelease_hash
                      FROM legacy_trust_audit q
                      JOIN suite_snapshot s ON s.snapshot_id = q.snapshot_id
                     WHERE s.inrelease_hash IS NOT NULL)
 ORDER BY hash
 LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("PinnedBlobsOutsideAuditQueue: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("PinnedBlobsOutsideAuditQueue: scan: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("PinnedBlobsOutsideAuditQueue: %w", err)
	}
	return out, nil
}

// ApplyLegacyAuditDecisions applies one batch of audit decisions in ONE
// writer transaction (spec §6.1) and reports, for each quarantined
// snapshot, whether it was current and how many url_path rows of its
// InRelease blob it deleted (DropAnchorRows). An invalid outcome rolls the
// whole batch back.
//
// AIDEV-NOTE: every effect is idempotent — a quarantine finds an already
// handled snapshot non-current (no second refcount release) and already
// retired; a repeated marker keeps the newest stamp; a repeated anchor-row
// drop finds the rows gone — so a crash between batches never
// double-applies. Never release pins for a non-current snapshot:
// displacement already did. The anchor-row drop (ruling R-H2) is in the
// quarantine's own transaction on purpose: the dequeue commits with it,
// so a drop left to a later transaction that failed would never be
// retried, and the rows would serve the corrupt bytes for good.
func (c *Cache) ApplyLegacyAuditDecisions(ctx context.Context, decisions []LegacyAuditDecision) (map[int64]LegacyAuditEffect, error) {
	effects := make(map[int64]LegacyAuditEffect)
	err := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("ApplyLegacyAuditDecisions: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		now := nowUnix()
		for _, d := range decisions {
			if d.DropAnchorRows && d.Outcome != LegacyAuditQuarantine && d.Outcome != LegacyAuditQuarantineSigned {
				return fmt.Errorf("ApplyLegacyAuditDecisions: snapshot %d: anchor rows dropped without a quarantine (outcome %d)",
					d.SnapshotID, d.Outcome)
			}
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
				eff := LegacyAuditEffect{WasCurrent: cur}
				if d.DropAnchorRows {
					if eff.AnchorRowsDeleted, err = dropAnchorBlobRowsTx(ctx, tx, d.SnapshotID, now); err != nil {
						return err
					}
				}
				effects[d.SnapshotID] = eff
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
	return effects, nil
}

// dropAnchorBlobRowsTx deletes every url_path row — under any scheme and
// host — whose blob is snapshotID's stored InRelease (ruling R-H2: the
// audit found that pool file's bytes no longer match their hash), each
// through deleteURLPathRowTx, and returns how many it deleted. A snapshot
// without an InRelease hash, or gone, deletes nothing.
//
// AIDEV-NOTE: rows only, never the pool file here: the writer runs no
// pool I/O (ruling PD-5); RunLegacyTrustAudit removes the corrupt file
// itself, outside the writer, after the batch commits (review C-1).
// Every row that names the blob serves the same bad bytes (the pool is
// content-addressed: a flat suite and its /. twin, or two remapped
// hosts, share it), so all of them go, and the next client request for
// each is a miss that fetches a fresh copy
// (TestServeHTTP_ACorruptAnchorQuarantineRefetchesTheAnchor).
func dropAnchorBlobRowsTx(ctx context.Context, tx *sql.Tx, snapshotID, now int64) (int, error) {
	var hash sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT inrelease_hash FROM suite_snapshot WHERE snapshot_id = ?`, snapshotID).Scan(&hash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("drop anchor rows of %d: read snapshot: %w", snapshotID, err)
	case !hash.Valid || hash.String == "":
		return 0, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT canonical_scheme, canonical_host, path FROM url_path WHERE blob_hash = ?`, hash.String)
	if err != nil {
		return 0, fmt.Errorf("drop anchor rows of %d: %w", snapshotID, err)
	}
	type rowKey struct{ scheme, host, path string }
	var keys []rowKey
	for rows.Next() {
		var k rowKey
		if err := rows.Scan(&k.scheme, &k.host, &k.path); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("drop anchor rows of %d: scan: %w", snapshotID, err)
		}
		keys = append(keys, k)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("drop anchor rows of %d: %w", snapshotID, err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("drop anchor rows of %d: iterate: %w", snapshotID, err)
	}
	for _, k := range keys {
		if err := deleteURLPathRowTx(ctx, tx, k.scheme, k.host, k.path, hash, now); err != nil {
			return 0, fmt.Errorf("drop anchor rows of %d: %w", snapshotID, err)
		}
	}
	return len(keys), nil
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

// trackedSuiteAnchorsSQL is ListTrackedSuiteAnchors' query.
//
// AIDEV-NOTE: the final review's R3 I-1. CROSS JOIN makes SQLite drive
// from suite_freshness and probe url_path's unique key once per anchor
// name, as the cache_summary refresher's query and
// unadoptedSuiteAnchorsSQL do. With a plain JOIN the only WHERE term,
// u.blob_hash IS NOT NULL, made the planner drive from url_path through
// idx_url_path_blob (every cached file) and range-scan the host's suites
// for each: 32 s for 5,000 suites over 220,000 url_path rows, at every
// start before any listener serves, against 0.06 s now. Production never
// runs ANALYZE, so the plan is the same on every install; keep the CROSS
// JOIN and the join's url_path terms bare columns
// (TestListTrackedSuiteAnchors_ProbesURLPathByItsKey pins the plan).
const trackedSuiteAnchorsSQL = `
SELECT sf.canonical_scheme, sf.canonical_host, sf.suite_path, u.path, u.blob_hash,
       MAX(COALESCE(u.last_requested_at, 0), COALESCE(u.last_fetched_at, 0))
  FROM suite_freshness sf
 CROSS JOIN url_path u
    ON u.canonical_scheme = sf.canonical_scheme
   AND u.canonical_host   = sf.canonical_host
   AND u.path IN (sf.suite_path || '/InRelease', sf.suite_path || '/Release.gpg')
 WHERE u.blob_hash IS NOT NULL`

// ListTrackedSuiteAnchors returns every tracked suite's cached signed
// anchor rows (spec C3 startup marker pass). Rows without a blob or a
// stamp are skipped.
func (c *Cache) ListTrackedSuiteAnchors(ctx context.Context) ([]TrackedAnchor, error) {
	rows, err := c.db.QueryContext(ctx, trackedSuiteAnchorsSQL)
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
		if a.SeenAt > 0 { // the IN above picks InRelease and Release.gpg only (deferred minor A-26)
			out = append(out, a)
		}
	}
	return out, rows.Err()
}
