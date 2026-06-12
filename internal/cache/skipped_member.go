package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SkipReasonOptionalMemberIntegrity is the snapshot_skipped_member
// reason recorded when [adoption].tolerate_optional_member_failures
// skips a non-IndexTarget member over an integrity/availability
// failure (size/content-length mismatch, hash mismatch, transport
// error, non-404 status). This is the only reason class the SPEC6_7
// §3 repair pass re-attempts: its dominant real-world trigger is a
// round-robin mirror serving the previous publication generation
// mid-sync, which heals within minutes — unlike "4xx" skips, which
// are near-always permanent publication artifacts (members the
// Release declares but the archive never serves).
const SkipReasonOptionalMemberIntegrity = "optional_member_integrity"

// ErrSnapshotNotCurrent is returned by RepairSkippedMember when the
// snapshot is no longer the suite's current snapshot. Repairing a
// displaced snapshot would bump blob refcounts that no future
// displacement decrements (Step 8 already ran for it), leaking the
// blob forever — so the writer refuses. Callers treat this as benign:
// a newer snapshot has taken over and carries its own skip records.
var ErrSnapshotNotCurrent = errors.New("cache: snapshot not current")

// ListRepairableSkippedMembers returns the snapshot's skipped-member
// rows whose reason is SkipReasonOptionalMemberIntegrity — the
// transient class the freshness-tick repair pass re-attempts. Rows
// with other reasons ("4xx" publication artifacts,
// "arch_not_in_allowlist" config-driven skips) are deliberately
// excluded: re-fetching those would generate guaranteed-failure
// upstream traffic on every tick. Ordered by path for deterministic
// repair logs.
func (c *Cache) ListRepairableSkippedMembers(ctx context.Context, snapshotID int64) ([]SkippedMember, error) {
	const q = `
SELECT snapshot_id, path, declared_sha256, size, reason,
       COALESCE(detail, ''), skipped_at, retry_count
  FROM snapshot_skipped_member
 WHERE snapshot_id = ? AND reason = ?
 ORDER BY path`
	rows, err := c.db.QueryContext(ctx, q, snapshotID, SkipReasonOptionalMemberIntegrity)
	if err != nil {
		return nil, fmt.Errorf("ListRepairableSkippedMembers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SkippedMember
	for rows.Next() {
		var s SkippedMember
		if err := rows.Scan(&s.SnapshotID, &s.Path, &s.DeclaredSHA256, &s.Size,
			&s.Reason, &s.Detail, &s.SkippedAt, &s.RetryCount); err != nil {
			return nil, fmt.Errorf("ListRepairableSkippedMembers scan: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListRepairableSkippedMembers iter: %w", err)
	}
	return out, nil
}

// RepairSkippedMember promotes a previously-skipped member into the
// snapshot in one writer transaction (SPEC6_7 §3):
//
//  1. Guard: the snapshot must still be some suite's current snapshot
//     (suite_freshness.current_snapshot_id = snapshotID), else
//     ErrSnapshotNotCurrent — see that sentinel for why repairing a
//     displaced snapshot is forbidden.
//  2. Load the snapshot_skipped_member row for skippedPath (absent →
//     ErrNotFound: already repaired, or never recorded) and bind every
//     member to its persisted signed declaration: a repairable
//     member's bytes verified equal to the declaration, so BlobHash
//     and DeclaredSHA256 must BOTH equal the row's declared_sha256,
//     each member must reference this snapshot, and the canonical
//     skippedPath must be among the inserted paths. The transaction is
//     the integrity boundary — it refuses rows the declaration it is
//     consuming does not vouch for, rather than trusting the caller.
//  3. Bump blob.refcount once iff the snapshot does not already pin
//     the blob (binding makes all members share one blob) — mirroring
//     CommitAdoption Step 4's "one bump per distinct blob" invariant
//     so the displacement decrement (Step 8) nets to zero.
//  4. Insert the snapshot_member rows (canonical path + by-hash
//     alias). An already-present byte-identical row is idempotently
//     accepted: identical same-directory members share a by-hash alias
//     path, which the original adoption (or an earlier repair) may
//     have committed already — failing on it would roll back the
//     skip-row consume and leave the member unrepairable for the
//     snapshot's lifetime. A same-path row with a DIFFERENT blob or
//     declaration is corruption and fails the whole transaction.
//
// Members' BlobHash values must reference existing pool blobs (FK
// fails closed otherwise).
func (c *Cache) RepairSkippedMember(ctx context.Context, snapshotID int64, skippedPath string, members []SnapshotMember) error {
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("RepairSkippedMember: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		// Step 1: current-snapshot guard.
		var one int
		err = tx.QueryRowContext(ctx, `
SELECT 1 FROM suite_freshness WHERE current_snapshot_id = ?`, snapshotID).Scan(&one)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: snapshot_id=%d", ErrSnapshotNotCurrent, snapshotID)
		case err != nil:
			return fmt.Errorf("RepairSkippedMember: current guard: %w", err)
		}

		// Step 2: load the skip record and bind members to its
		// declaration.
		var declared string
		err = tx.QueryRowContext(ctx, `
SELECT declared_sha256 FROM snapshot_skipped_member
 WHERE snapshot_id = ? AND path = ?`, snapshotID, skippedPath).Scan(&declared)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("RepairSkippedMember: skip row %q: %w", skippedPath, ErrNotFound)
		case err != nil:
			return fmt.Errorf("RepairSkippedMember: load skip row: %w", err)
		}
		if !validBlobHash(declared) {
			// The table CHECK constraint makes this unreachable; fail
			// closed anyway.
			return fmt.Errorf("RepairSkippedMember: skip row %q declared_sha256 %w", skippedPath, ErrInvalidHash)
		}
		canonicalSeen := false
		for _, m := range members {
			if m.SnapshotID != snapshotID {
				return fmt.Errorf("RepairSkippedMember: member %q references snapshot %d, repairing %d",
					m.Path, m.SnapshotID, snapshotID)
			}
			if m.BlobHash != declared {
				return fmt.Errorf("RepairSkippedMember: member %q blob %s does not match declaration %s",
					m.Path, m.BlobHash, declared)
			}
			if m.DeclaredSHA256 != declared {
				return fmt.Errorf("RepairSkippedMember: member %q declares %s, skip row declares %s",
					m.Path, m.DeclaredSHA256, declared)
			}
			if m.Path == skippedPath {
				canonicalSeen = true
			}
		}
		if !canonicalSeen {
			return fmt.Errorf("RepairSkippedMember: members omit the canonical path %q", skippedPath)
		}

		// Consume the row. One row is guaranteed by the SELECT above
		// (same transaction); the affected check is belt-and-braces.
		res, err := tx.ExecContext(ctx, `
DELETE FROM snapshot_skipped_member WHERE snapshot_id = ? AND path = ?`,
			snapshotID, skippedPath)
		if err != nil {
			return fmt.Errorf("RepairSkippedMember: delete skip row: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("RepairSkippedMember: rows affected: %w", err)
		}
		if affected == 0 {
			return fmt.Errorf("RepairSkippedMember: skip row %q: %w", skippedPath, ErrNotFound)
		}

		// Step 3: refcount bump — once iff the snapshot does not
		// already pin the blob. The NOT EXISTS runs before this call's
		// own member inserts (Step 4), so "already pins" means rows
		// from the original adoption or earlier repairs.
		if _, err := tx.ExecContext(ctx, `
UPDATE blob
   SET refcount = refcount + 1,
       refcount_zeroed_at = IIF(refcount + 1 > 0, NULL, refcount_zeroed_at)
 WHERE hash = ?
   AND NOT EXISTS (SELECT 1 FROM snapshot_member
                    WHERE snapshot_id = ? AND blob_hash = ?)`,
			declared, snapshotID, declared); err != nil {
			return fmt.Errorf("RepairSkippedMember: bump refcount %s: %w", declared, err)
		}

		// Step 4: insert the member rows; tolerate byte-identical rows
		// that already exist (shared by-hash alias), reject mismatches.
		for _, m := range members {
			res, err := tx.ExecContext(ctx, `
INSERT INTO snapshot_member (snapshot_id, path, blob_hash, declared_sha256)
VALUES (?, ?, ?, ?)
ON CONFLICT (snapshot_id, path) DO NOTHING`,
				snapshotID, m.Path, m.BlobHash, m.DeclaredSHA256)
			if err != nil {
				return fmt.Errorf("RepairSkippedMember: insert member %q: %w", m.Path, err)
			}
			inserted, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("RepairSkippedMember: insert member %q rows affected: %w", m.Path, err)
			}
			if inserted == 0 {
				var haveBlob, haveDeclared string
				if err := tx.QueryRowContext(ctx, `
SELECT blob_hash, declared_sha256 FROM snapshot_member
 WHERE snapshot_id = ? AND path = ?`, snapshotID, m.Path).Scan(&haveBlob, &haveDeclared); err != nil {
					return fmt.Errorf("RepairSkippedMember: probe existing member %q: %w", m.Path, err)
				}
				if haveBlob != m.BlobHash || haveDeclared != m.DeclaredSHA256 {
					return fmt.Errorf("RepairSkippedMember: member %q already present with blob %s declared %s, want blob %s declared %s",
						m.Path, haveBlob, haveDeclared, m.BlobHash, m.DeclaredSHA256)
				}
			}
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("RepairSkippedMember: commit: %w", err)
		}
		return nil
	})
}

// BumpSkippedMemberRetry increments retry_count on a skipped-member
// row after a failed repair attempt. Zero rows affected (row already
// repaired or reaped) is benign — the bookkeeping exists only for
// operator visibility into how long a member has been failing.
func (c *Cache) BumpSkippedMemberRetry(ctx context.Context, snapshotID int64, path string) error {
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `
UPDATE snapshot_skipped_member SET retry_count = retry_count + 1
 WHERE snapshot_id = ? AND path = ?`, snapshotID, path); err != nil {
			return fmt.Errorf("BumpSkippedMemberRetry: %w", err)
		}
		return nil
	})
}
