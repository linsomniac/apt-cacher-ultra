package cache

import (
	"context"
	"database/sql"
	"fmt"
)

// recordListHotPackagesSQL records, for the entries of one list, the
// interest clients already showed in their files: the newest request stamp
// of a url_path row that holds the entry's declared bytes, per package
// name and architecture. The hot_package row is the suite's (the first
// three parameters), which is what its foreign key requires. The %s is
// either empty or a term naming one path.
const recordListHotPackagesSQL = `INSERT INTO hot_package
 (canonical_scheme, canonical_host, suite_path, package_name, architecture, last_requested_at)
 SELECT ?, ?, ?, e.package_name, e.architecture, MAX(u.last_requested_at)
 FROM package_list_entry e
 JOIN url_path u ON u.canonical_scheme = e.canonical_scheme AND u.canonical_host = e.canonical_host
   AND u.path = e.path AND u.blob_hash = e.declared_sha256
 WHERE e.package_list_id = ? %s AND u.last_requested_at IS NOT NULL
   AND e.package_name <> '' AND e.architecture <> ''
 GROUP BY e.package_name, e.architecture
 ON CONFLICT(canonical_scheme, canonical_host, suite_path, package_name, architecture)
 DO UPDATE SET last_requested_at = MAX(hot_package.last_requested_at, excluded.last_requested_at)`

// recordListHotPackages records hot-package interest for entries of list
// listID, which key's suite names or is about to name in this transaction:
// every entry when paths is nil, otherwise the entries at paths (one probe
// each). It learns identities for requests made before the suite had the
// row; only real request stamps count, so warming never creates or extends
// interest.
//
// AIDEV-NOTE: a writer of entries calls this for the rows it wrote, and
// only those. The hot_package triggers cover every later request stamp,
// so an entry that did not change was recorded when it arrived (by the
// commit or the reconcile that wrote it) or by a trigger since. The one
// other caller is the commit's warm step, for the files it warmed: a
// warm changes a url_path row's bytes without a request stamp, so no
// trigger fires for it (CommitAdoption, after Step 3a). The
// suite's key is a parameter, not a join on suite_freshness, because a new
// list is not named by its suite until the commit's Step 6.
func recordListHotPackages(ctx context.Context, tx *sql.Tx, listID int64, key SuiteKey, paths []string) error {
	if paths == nil {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(recordListHotPackagesSQL, ""),
			key.CanonicalScheme, key.CanonicalHost, key.SuitePath, listID); err != nil {
			return fmt.Errorf("record list hot packages: %w", err)
		}
		return nil
	}
	if len(paths) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, fmt.Sprintf(recordListHotPackagesSQL, "AND e.path = ?"))
	if err != nil {
		return fmt.Errorf("record list hot packages: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	for _, path := range paths {
		if _, err := stmt.ExecContext(ctx, key.CanonicalScheme, key.CanonicalHost, key.SuitePath, listID, path); err != nil {
			return fmt.Errorf("record list hot packages: %q: %w", path, err)
		}
	}
	return nil
}
