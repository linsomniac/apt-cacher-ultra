package cache

import (
	"context"
	"database/sql"
	"fmt"
)

func recordSnapshotHotPackages(ctx context.Context, tx *sql.Tx, snapshotID int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO hot_package
 (canonical_scheme, canonical_host, suite_path, package_name, architecture, last_requested_at)
 SELECT ph.canonical_scheme, ph.canonical_host, s.suite_path,
        ph.package_name, ph.architecture, MAX(u.last_requested_at)
 FROM package_hash ph JOIN suite_snapshot s ON s.snapshot_id = ph.snapshot_id
 JOIN url_path u ON u.canonical_scheme = ph.canonical_scheme AND u.canonical_host = ph.canonical_host
   AND u.path = ph.path AND u.blob_hash = ph.declared_sha256
 WHERE ph.snapshot_id = ? AND u.last_requested_at IS NOT NULL
   AND ph.package_name <> '' AND ph.architecture <> ''
 GROUP BY ph.canonical_scheme, ph.canonical_host, s.suite_path, ph.package_name, ph.architecture
 ON CONFLICT(canonical_scheme, canonical_host, suite_path, package_name, architecture)
 DO UPDATE SET last_requested_at = MAX(hot_package.last_requested_at, excluded.last_requested_at)`, snapshotID)
	if err != nil {
		return fmt.Errorf("record snapshot hot packages: %w", err)
	}
	return nil
}
