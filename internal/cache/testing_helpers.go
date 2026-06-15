package cache

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// DeleteSnapshotMembersForTest removes snapshot_member rows for the given
// paths from the specified snapshot. This is a TEST-ONLY helper (do not use
// in production code) that simulates a pre-fix degraded snapshot — one that
// was adopted without some requestable IndexTarget members — so reconcile
// tests can build the exact degraded state they need. Refcount accuracy is
// NOT maintained; use only in tests that do not inspect blob.refcount after
// calling this.
//
// AIDEV-NOTE: exported for use by internal/freshness reconcile tests that
// need to manufacture a degraded snapshot via the delete path (the Layer-2
// serve-contract guard prevents adopting one directly, which is the correct
// production behavior). Named *ForTest by convention; never call from
// non-test code.
func (c *Cache) DeleteSnapshotMembersForTest(ctx context.Context, snapshotID int64, paths ...string) error {
	if len(paths) == 0 {
		return nil
	}
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		// Build "?,?,..." placeholders for the IN clause.
		placeholders := strings.Repeat("?,", len(paths))
		placeholders = placeholders[:len(placeholders)-1] // trim trailing comma
		q := fmt.Sprintf(
			`DELETE FROM snapshot_member WHERE snapshot_id = ? AND path IN (%s)`,
			placeholders,
		)
		args := make([]any, 0, 1+len(paths))
		args = append(args, snapshotID)
		for _, p := range paths {
			args = append(args, p)
		}
		if _, err := conn.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("DeleteSnapshotMembersForTest: %w", err)
		}
		return nil
	})
}
