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
//  2. Delete the snapshot_skipped_member row for skippedPath; zero
//     rows affected means the skip record is gone (already repaired,
//     or never recorded) and the call fails with ErrNotFound before
//     any member insert can half-apply.
//  3. Bump blob.refcount once per distinct blob hash among members
//     that the snapshot does not already reference — mirroring
//     CommitAdoption Step 4's "one bump per distinct blob" invariant
//     so the displacement decrement (Step 8) nets to zero.
//  4. Insert the snapshot_member rows (canonical path + by-hash
//     alias), validated like CommitAdoption Step 2.
//
// The caller has already verified the fetched bytes hash to the
// member's declared_sha256 (adoption-grade validation); members'
// BlobHash values must reference existing pool blobs (FK fails
// closed otherwise).
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

		// Step 2: consume the skip record.
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

		// Step 3: refcount bumps — once per distinct blob the snapshot
		// does not already pin. The NOT EXISTS runs before this call's
		// own member inserts (Step 4), so "already pins" means rows
		// from the original adoption or earlier repairs.
		seen := make(map[string]bool, len(members))
		for _, m := range members {
			if !validBlobHash(m.BlobHash) {
				return fmt.Errorf("RepairSkippedMember: member %q blob_hash %w", m.Path, ErrInvalidHash)
			}
			if !validBlobHash(m.DeclaredSHA256) {
				return fmt.Errorf("RepairSkippedMember: member %q declared_sha256 %w", m.Path, ErrInvalidHash)
			}
			if seen[m.BlobHash] {
				continue
			}
			seen[m.BlobHash] = true
			if _, err := tx.ExecContext(ctx, `
UPDATE blob
   SET refcount = refcount + 1,
       refcount_zeroed_at = IIF(refcount + 1 > 0, NULL, refcount_zeroed_at)
 WHERE hash = ?
   AND NOT EXISTS (SELECT 1 FROM snapshot_member
                    WHERE snapshot_id = ? AND blob_hash = ?)`,
				m.BlobHash, snapshotID, m.BlobHash); err != nil {
				return fmt.Errorf("RepairSkippedMember: bump refcount %s: %w", m.BlobHash, err)
			}
		}

		// Step 4: insert the member rows.
		for _, m := range members {
			if _, err := tx.ExecContext(ctx, `
INSERT INTO snapshot_member (snapshot_id, path, blob_hash, declared_sha256)
VALUES (?, ?, ?, ?)`,
				snapshotID, m.Path, m.BlobHash, m.DeclaredSHA256); err != nil {
				return fmt.Errorf("RepairSkippedMember: insert member %q: %w", m.Path, err)
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
