package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// insertPackageHashTx inserts a single package_hash row inside an open
// transaction, for the InsertReconciledMembers path only.
//
// AIDEV-NOTE: CommitAdoption keeps its OWN inline copy of this INSERT
// (adoption.go Step 3); this is an independent copy that matches its column
// set + hash validation today, not a shared dependency. They are deliberately
// not refactored into one: CommitAdoption uses a plain INSERT (candidate
// snapshots are never partially committed, so a conflict there is a bug),
// while reconcile adds ON CONFLICT DO NOTHING because a retry of
// InsertReconciledMembers may legitimately re-hit rows the first (partial)
// attempt wrote. The PK (canonical_scheme, canonical_host, path, snapshot_id)
// is the natural uniqueness boundary — DO NOTHING is strictly additive and
// safe. If the package_hash column set ever changes, update BOTH copies.
func insertPackageHashTx(ctx context.Context, tx *sql.Tx, ph PackageHash) error {
	if !validBlobHash(ph.DeclaredSHA256) {
		return fmt.Errorf("insertPackageHashTx: %q declared_sha256 %w", ph.Path, ErrInvalidHash)
	}
	// Idempotent on retry, loud on disagreement — mirroring the snapshot_member
	// path below and adoption's own same-path/different-hash rule (which fails
	// with ErrAdoptionParseFailed, SPEC6_5 §11 H7). A byte-identical existing
	// row is a no-op; a DIFFERENT declared_sha256 for the same .deb path in
	// this snapshot is a real cross-index disagreement (an already-present
	// index vs the healed one) and must NOT silently keep the stale row — that
	// would fail-closed every strict-mode fetch of that .deb via the healed
	// index. (The previous ON CONFLICT DO NOTHING silently kept the stale row.)
	var existing string
	switch err := tx.QueryRowContext(ctx, `
SELECT declared_sha256 FROM package_hash
 WHERE canonical_scheme = ? AND canonical_host = ? AND path = ? AND snapshot_id = ?`,
		ph.CanonicalScheme, ph.CanonicalHost, ph.Path, ph.SnapshotID).Scan(&existing); {
	case err == nil:
		if existing != ph.DeclaredSHA256 {
			return fmt.Errorf("insertPackageHashTx: %q already declares %s, reconcile has %s",
				ph.Path, existing, ph.DeclaredSHA256)
		}
		return nil // byte-identical — idempotent no-op
	case errors.Is(err, sql.ErrNoRows):
		// not present — insert below
	default:
		return fmt.Errorf("insertPackageHashTx: probe %q: %w", ph.Path, err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO package_hash (canonical_scheme, canonical_host, path,
                          declared_sha256, snapshot_id,
                          package_name, architecture)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		ph.CanonicalScheme, ph.CanonicalHost, ph.Path,
		ph.DeclaredSHA256, ph.SnapshotID,
		ph.PackageName, ph.Architecture); err != nil {
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
//  2. IDEMPOTENCY: the probe-before-insert on BOTH snapshot_member and
//     package_hash (insertPackageHashTx) means the entire function is safe to
//     retry — a byte-identical row is a no-op. A conflicting blob_hash (or
//     conflicting package_hash declared_sha256) on the same path is an explicit
//     error, not a silent no-op, because it would mean the caller is trying to
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
			// Shape-check the hashes before any SQL — mirrors CommitAdoption
			// Step 4 (adoption.go). Defense-in-depth: a malformed hash should
			// fail loud here, not be smuggled past the schema CHECK or into a
			// BlobPath computation.
			if !validBlobHash(m.BlobHash) {
				return fmt.Errorf("InsertReconciledMembers: member %q blob_hash %w", m.Path, ErrInvalidHash)
			}
			if !validBlobHash(m.DeclaredSHA256) {
				return fmt.Errorf("InsertReconciledMembers: member %q declared_sha256 %w", m.Path, ErrInvalidHash)
			}
			// AIDEV-NOTE: idempotency probe — check for an existing row before
			// inserting. A byte-identical row (same blob_hash) is silently
			// skipped (safe retry) and must NOT bump refcount. A different
			// blob_hash on the same path is corruption and fails loud (the
			// caller tried to substitute a different blob than the one already
			// serving under this path).
			var existing string
			switch err := tx.QueryRowContext(ctx,
				`SELECT blob_hash FROM snapshot_member WHERE snapshot_id = ? AND path = ?`,
				snapshotID, m.Path).Scan(&existing); {
			case err == nil:
				if existing != m.BlobHash {
					return fmt.Errorf("InsertReconciledMembers: path %q exists with blob %s, reconcile has %s",
						m.Path, existing, m.BlobHash)
				}
				continue // byte-identical: idempotent skip, no refcount bump
			case errors.Is(err, sql.ErrNoRows):
				// fall through to insert
			default:
				return fmt.Errorf("InsertReconciledMembers: probe %q: %w", m.Path, err)
			}

			// AIDEV-NOTE: refcount bump — mirrors RepairSkippedMember Step 3
			// (skipped_member.go ~174) and CommitAdoption Step 4 (adoption.go
			// ~484). Each new snapshot_member is a fresh pin on its blob, so
			// blob.refcount MUST rise to match, or the displacement decrement
			// (CommitAdoption Step 8, ~541) over-decrements this snapshot's
			// blobs to below their true count when it is later displaced —
			// corrupting the GC ledger. The NOT EXISTS guard bumps at most
			// ONCE per distinct blob within this snapshot: it runs BEFORE this
			// iteration's own INSERT, so "already pins" means a row from the
			// original adoption, an earlier reconcile, or an earlier member in
			// THIS loop that shares the blob (e.g. a path + its by-hash alias).
			// The IIF clears refcount_zeroed_at only on the strictly-positive
			// crossing, identical to the mirrored functions.
			if _, err := tx.ExecContext(ctx, `
UPDATE blob
   SET refcount = refcount + 1,
       refcount_zeroed_at = IIF(refcount + 1 > 0, NULL, refcount_zeroed_at)
 WHERE hash = ?
   AND NOT EXISTS (SELECT 1 FROM snapshot_member
                    WHERE snapshot_id = ? AND blob_hash = ?)`,
				m.BlobHash, snapshotID, m.BlobHash); err != nil {
				return fmt.Errorf("InsertReconciledMembers: bump refcount %s: %w", m.BlobHash, err)
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
