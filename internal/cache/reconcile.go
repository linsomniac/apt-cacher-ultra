package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// insertPackageHashTx inserts a single package_hash row inside an open
// transaction. Used by both CommitAdoption (via the loop in adoption.go) and
// InsertReconciledMembers so the two paths stay in lockstep on column set and
// hash validation.
//
// AIDEV-NOTE: ON CONFLICT DO NOTHING here (vs CommitAdoption's plain INSERT)
// is intentional for the reconcile path: a retry of InsertReconciledMembers
// may hit rows already written by the first (partial) attempt. The PK
// (canonical_scheme, canonical_host, path, snapshot_id) is a natural uniqueness
// boundary — DO NOTHING is strictly additive and safe. CommitAdoption retains
// its plain INSERT because candidate snapshots are never partially committed.
func insertPackageHashTx(ctx context.Context, tx *sql.Tx, ph PackageHash) error {
	if !validBlobHash(ph.DeclaredSHA256) {
		return fmt.Errorf("insertPackageHashTx: %q declared_sha256 %w", ph.Path, ErrInvalidHash)
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO package_hash (canonical_scheme, canonical_host, path,
                          declared_sha256, snapshot_id,
                          package_name, architecture)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (canonical_scheme, canonical_host, path, snapshot_id) DO NOTHING`,
		ph.CanonicalScheme, ph.CanonicalHost, ph.Path,
		ph.DeclaredSHA256, ph.SnapshotID,
		ph.PackageName, ph.Architecture)
	if err != nil {
		return fmt.Errorf("insertPackageHashTx: insert %q: %w", ph.Path, err)
	}
	return nil
}

// InsertReconciledMembers inserts snapshot_member (+ by-hash alias) and
// package_hash rows into an EXISTING current snapshot — the SPEC6_8 in-place
// reconcile path. Unlike RepairSkippedMember it consumes no
// snapshot_skipped_member row; the caller (Adopter.ReconcileSnapshot) has
// already validated each member's bytes against the re-parsed, GPG-verified
// Release declaration. The transaction re-checks the snapshot is still the
// suite's current snapshot, so a reconcile that races a re-adoption is a safe
// no-op (ErrSnapshotNotCurrent). Member inserts are idempotent on the PK; a
// byte-identical row is skipped, a conflicting blob_hash on the same path is a
// loud error (mirrors the by-hash-alias invariant in CommitAdoption).
//
// AIDEV-NOTE: this is the SPEC6_8 §3 cache transaction — trust-sensitive.
// Two invariants must hold for every caller:
//  1. CURRENT-SNAPSHOT GUARD: the SELECT on suite_freshness below is the sole
//     authority that this snapshot is still serving clients; if it misses
//     (ErrSnapshotNotCurrent), the whole function returns without touching any
//     rows. Do not remove or weaken this check.
//  2. IDEMPOTENCY: the probe-before-insert on snapshot_member and the
//     ON CONFLICT DO NOTHING on package_hash mean the entire function is safe
//     to retry. A conflicting blob_hash on the same path is an explicit error,
//     not a silent no-op, because it would mean the caller is trying to
//     substitute a different blob than what already serves — that is corruption.
func (c *Cache) InsertReconciledMembers(ctx context.Context, snapshotID int64, members []SnapshotMember, packageHashes []PackageHash) error {
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("InsertReconciledMembers: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		// AIDEV-NOTE: current-snapshot guard — mirrors RepairSkippedMember
		// (skipped_member.go). A snapshot that is no longer current must not
		// receive new member rows: the displacement decrement (CommitAdoption
		// Step 8) already ran, so any blob refcount bumps here would leak.
		// ErrSnapshotNotCurrent is the sentinel callers check to distinguish
		// "lost the race" (benign) from a real error.
		var one int
		switch err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM suite_freshness WHERE current_snapshot_id = ?`, snapshotID).Scan(&one); {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: snapshot_id=%d", ErrSnapshotNotCurrent, snapshotID)
		case err != nil:
			return fmt.Errorf("InsertReconciledMembers: current guard: %w", err)
		}

		for _, m := range members {
			if m.SnapshotID != snapshotID {
				return fmt.Errorf("InsertReconciledMembers: member %q references snapshot %d, reconciling %d",
					m.Path, m.SnapshotID, snapshotID)
			}
			// AIDEV-NOTE: idempotency probe — check for an existing row before
			// inserting. A byte-identical row (same blob_hash) is silently
			// skipped (safe retry). A different blob_hash on the same path is
			// corruption and fails loud (the caller tried to substitute a
			// different blob than the one already serving under this path).
			var existing string
			switch err := tx.QueryRowContext(ctx,
				`SELECT blob_hash FROM snapshot_member WHERE snapshot_id = ? AND path = ?`,
				snapshotID, m.Path).Scan(&existing); {
			case err == nil:
				if existing != m.BlobHash {
					return fmt.Errorf("InsertReconciledMembers: path %q exists with blob %s, reconcile has %s",
						m.Path, existing, m.BlobHash)
				}
				continue // byte-identical: idempotent skip
			case errors.Is(err, sql.ErrNoRows):
				// fall through to insert
			default:
				return fmt.Errorf("InsertReconciledMembers: probe %q: %w", m.Path, err)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO snapshot_member (snapshot_id, path, blob_hash, declared_sha256) VALUES (?,?,?,?)`,
				m.SnapshotID, m.Path, m.BlobHash, m.DeclaredSHA256); err != nil {
				return fmt.Errorf("InsertReconciledMembers: insert member %q: %w", m.Path, err)
			}
		}

		// package_hash upserts — uses the shared helper which carries
		// ON CONFLICT DO NOTHING for retry-safety (see insertPackageHashTx).
		for _, ph := range packageHashes {
			if err := insertPackageHashTx(ctx, tx, ph); err != nil {
				return fmt.Errorf("InsertReconciledMembers: package_hash %q: %w", ph.Path, err)
			}
		}
		return tx.Commit()
	})
}
