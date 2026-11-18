package cache

import (
	"context"
	"database/sql"
)

// ConfirmSnapshotUnchanged records a probe made beside maintenance. Only its
// check stamps change; validators, pending content, failure records and snapshot
// identity remain untouched. A completed adoption makes the update a no-op.
func (c *Cache) ConfirmSnapshotUnchanged(ctx context.Context, key SuiteKey, snapshotID, now int64) (bool, error) {
	updated := false
	err := c.submitWrite(ctx, writeCheckUnchanged, func(ctx context.Context, conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `UPDATE suite_freshness
SET last_check_at = ?, last_success_at = ?, check_fail_count = 0
WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ? AND current_snapshot_id = ?
AND inrelease_change_seen_at IS NULL AND adoption_fail_count = 0
AND (last_check_at IS NULL OR last_check_at < ?)`, now, now,
			key.CanonicalScheme, key.CanonicalHost, key.SuitePath, snapshotID, now)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		updated = n != 0
		return err
	})
	if err != nil {
		return false, err
	}
	return updated, nil
}
