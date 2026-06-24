package cache

import (
	"context"
	"database/sql"
	"fmt"
)

// URLPathGCBatchResult reports one cursor-paged url_path GC batch
// (version-aware retention design §3). A batch scans up to batchSize rows
// starting after the (scheme, host, path) cursor and, per row, takes at
// most one action: stamp dropped_at, clear dropped_at, or delete. The
// caller loops, advancing the cursor by (LastScheme, LastHost, LastPath),
// until Scanned == 0 (the table is exhausted for this tick). Cursor paging
// — rather than "loop until a batch deletes 0" — is what guarantees
// termination even though most rows are no-ops (already-retained, or
// in-grace-stamped): every url_path row is visited exactly once per tick
// regardless of whether it triggers an action.
type URLPathGCBatchResult struct {
	Scanned int
	Stamped int
	Cleared int
	Deleted int
	// Cursor: the last (scheme, host, path) scanned this batch. Pass back
	// as the after* arguments to continue. Unchanged from the input cursor
	// when Scanned == 0.
	LastScheme string
	LastHost   string
	LastPath   string
}

// RunURLPathGCBatch evaluates the three-rule retention union (recency OR
// newest-N mirror OR hold-grace) for one cursor page of url_path rows and
// applies the dropped_at lazy-stamp lifecycle:
//
//   - retained (rule 1 recency, rule 2 mirror, or a metadata-anchor /
//     snapshot_member guard): if dropped_at is set from a prior pass, clear
//     it (the row re-qualified); otherwise no-op.
//   - not retained: if hold_packages.window is 0, delete immediately; if
//     dropped_at is unset, stamp it (= now); if stamped and the grace has
//     elapsed, delete; if stamped and still in grace, no-op.
//
// Deletions decrement the matching blob.refcount exactly like EvictURLPath
// so the same-tick blob pass can reap the bytes once blob_grace elapses.
//
// ttlSeconds is gc.url_path_ttl (> 0 required — the caller short-circuits
// when 0). holdSeconds is hold_packages.window (>= 0; 0 = no grace).
// maxVersions is retention.max_versions_per_package (clamped to >= 1).
//
// Mirror rule (rule 2) detail: for the row's blob, find the held-snapshot
// package_hash rows whose (path, declared_sha256) match the url_path's
// (path, blob_hash). If any such row carries an empty version it is a
// non-binary artifact (Sources/pdiff/Contents) and keeps the legacy
// snapshot-reference guard (retained). Otherwise the row is retained iff
// its version ranks within the newest maxVersions distinct Debian versions
// of its (scheme, host, suite_path, package_name, architecture) across the
// held snapshots — evaluated per group and memoized for the batch. Ranking
// is done in Go (SQLite cannot order by Debian version) over only the
// groups the batch actually touches, so the cost is candidate-bounded.
func (c *Cache) RunURLPathGCBatch(ctx context.Context, batchSize int, ttlSeconds, holdSeconds int64, maxVersions int, afterScheme, afterHost, afterPath string) (URLPathGCBatchResult, error) {
	if batchSize < 1 {
		return URLPathGCBatchResult{}, fmt.Errorf("RunURLPathGCBatch: batchSize must be >= 1, got %d", batchSize)
	}
	if ttlSeconds <= 0 {
		return URLPathGCBatchResult{}, fmt.Errorf("RunURLPathGCBatch: ttlSeconds must be > 0, got %d", ttlSeconds)
	}
	if maxVersions < 1 {
		maxVersions = 1
	}
	now := nowUnix()
	ttlCutoff := now - ttlSeconds

	res := URLPathGCBatchResult{LastScheme: afterScheme, LastHost: afterHost, LastPath: afterPath}

	type candidate struct {
		scheme, host, path string
		blobHash           sql.NullString
		lastReq            sql.NullInt64
		droppedAt          sql.NullInt64
	}

	werr := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("RunURLPathGCBatch: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		rows, err := tx.QueryContext(ctx, `
SELECT canonical_scheme, canonical_host, path, blob_hash, last_requested_at, dropped_at
  FROM url_path
 WHERE (canonical_scheme, canonical_host, path) > (?, ?, ?)
 ORDER BY canonical_scheme, canonical_host, path
 LIMIT ?`, afterScheme, afterHost, afterPath, batchSize)
		if err != nil {
			return fmt.Errorf("RunURLPathGCBatch: select: %w", err)
		}
		var batch []candidate
		for rows.Next() {
			var r candidate
			if err := rows.Scan(&r.scheme, &r.host, &r.path, &r.blobHash, &r.lastReq, &r.droppedAt); err != nil {
				_ = rows.Close()
				return fmt.Errorf("RunURLPathGCBatch: scan: %w", err)
			}
			batch = append(batch, r)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("RunURLPathGCBatch: iter: %w", err)
		}
		_ = rows.Close()

		topNCache := make(map[topNKey]map[string]struct{})

		for _, r := range batch {
			res.Scanned++
			res.LastScheme, res.LastHost, res.LastPath = r.scheme, r.host, r.path

			retained, err := c.urlPathRetainedTx(ctx, tx, r.scheme, r.host, r.path, r.blobHash, r.lastReq, ttlCutoff, maxVersions, topNCache)
			if err != nil {
				return err
			}

			if retained {
				if r.droppedAt.Valid {
					if _, err := tx.ExecContext(ctx, `
UPDATE url_path SET dropped_at = NULL
 WHERE canonical_scheme = ? AND canonical_host = ? AND path = ? AND dropped_at IS NOT NULL`,
						r.scheme, r.host, r.path); err != nil {
						return fmt.Errorf("RunURLPathGCBatch: clear dropped_at: %w", err)
					}
					res.Cleared++
				}
				continue
			}

			// Not retained. Decide stamp / delete / leave-in-grace.
			expired := r.droppedAt.Valid && now-r.droppedAt.Int64 >= holdSeconds
			switch {
			case holdSeconds <= 0 || expired:
				if err := deleteURLPathRowTx(ctx, tx, r.scheme, r.host, r.path, r.blobHash, now); err != nil {
					return err
				}
				res.Deleted++
			case !r.droppedAt.Valid:
				if _, err := tx.ExecContext(ctx, `
UPDATE url_path SET dropped_at = ?
 WHERE canonical_scheme = ? AND canonical_host = ? AND path = ?`,
					now, r.scheme, r.host, r.path); err != nil {
					return fmt.Errorf("RunURLPathGCBatch: stamp dropped_at: %w", err)
				}
				res.Stamped++
			default:
				// Stamped and still within the hold grace — leave it.
			}
		}
		return tx.Commit()
	})
	if werr != nil {
		return URLPathGCBatchResult{}, werr
	}
	return res, nil
}

// topNKey identifies a per-suite (package_name, architecture) version
// ranking group, memoized within a single batch.
type topNKey struct {
	scheme, host, suite, name, arch string
}

// urlPathRetainedTx reports whether a url_path row is retained by the
// three-rule union plus the unchanged metadata-anchor / snapshot_member
// guards. It runs inside the writer tx so its verdict is consistent with
// the DELETE that may follow in the same tx (closes the SELECT→DELETE
// liveness race the same way the legacy pass did).
func (c *Cache) urlPathRetainedTx(ctx context.Context, tx *sql.Tx,
	scheme, host, path string, blobHash sql.NullString, lastReq sql.NullInt64,
	ttlCutoff int64, maxVersions int, topNCache map[topNKey]map[string]struct{}) (bool, error) {

	// Rule 1: recency. A row requested within the TTL is retained even if
	// it is an old (pinned) version no longer in the mirror set.
	if lastReq.Valid && lastReq.Int64 >= ttlCutoff {
		return true, nil
	}

	// Metadata guards b/c/d (unchanged from the legacy pass, current-
	// snapshot scoped): snapshot_member reachability, the InRelease/Release
	// anchor hashes, and the identity anchor that keeps freshness from
	// freezing on a low-traffic lull. These protect metadata only; .deb
	// retention is governed by the mirror rule below.
	var guarded int
	err := tx.QueryRowContext(ctx, `
SELECT
  EXISTS(SELECT 1 FROM snapshot_member sm
          WHERE sm.blob_hash = ?
            AND sm.snapshot_id IN (SELECT current_snapshot_id FROM suite_freshness WHERE current_snapshot_id IS NOT NULL))
  OR EXISTS(SELECT 1 FROM suite_snapshot ss
             WHERE (ss.inrelease_hash = ? OR ss.release_hash = ? OR ss.release_gpg_hash = ?)
               AND ss.snapshot_id IN (SELECT current_snapshot_id FROM suite_freshness WHERE current_snapshot_id IS NOT NULL))
  OR EXISTS(SELECT 1 FROM suite_freshness sf
              JOIN suite_snapshot ss ON ss.snapshot_id = sf.current_snapshot_id
             WHERE sf.canonical_scheme = ? AND sf.canonical_host = ?
               AND ((ss.inrelease_hash IS NOT NULL AND ? = sf.suite_path || '/InRelease')
                 OR (ss.release_hash   IS NOT NULL AND ? IN (sf.suite_path || '/Release', sf.suite_path || '/Release.gpg'))))`,
		blobHash, blobHash, blobHash, blobHash, scheme, host, path, path).Scan(&guarded)
	if err != nil {
		return false, fmt.Errorf("urlPathRetainedTx: guards: %w", err)
	}
	if guarded != 0 {
		return true, nil
	}

	// Rule 2: mirror. Needs a cached blob (path+hash match). A NULL/empty
	// blob_hash (failed fetch) cannot match any package_hash row, so it is
	// not mirror-retained.
	if !blobHash.Valid || blobHash.String == "" {
		return false, nil
	}

	// Scope to CURRENT snapshots only (the active published index per suite),
	// matching the legacy guard (a). Displaced/forensic snapshots must not
	// vouch a .deb or contribute versions to the newest-N ranking: a
	// displaced snapshot listing a version absent from the live index could
	// otherwise reap a still-published version, or keep a withdrawn one.
	// Just-superseded versions that leave the current index are covered by
	// the hold-grace window, not by displaced-snapshot membership.
	mrows, err := tx.QueryContext(ctx, `
SELECT ss.suite_path, ph.package_name, ph.architecture, ph.version
  FROM package_hash ph
  JOIN suite_snapshot ss ON ss.snapshot_id = ph.snapshot_id
 WHERE ph.canonical_scheme = ? AND ph.canonical_host = ? AND ph.path = ? AND ph.declared_sha256 = ?
   AND ph.snapshot_id IN (SELECT current_snapshot_id FROM suite_freshness WHERE current_snapshot_id IS NOT NULL)`,
		scheme, host, path, blobHash.String)
	if err != nil {
		return false, fmt.Errorf("urlPathRetainedTx: mirror match: %w", err)
	}
	type matchRow struct{ suite, name, arch, version string }
	var matches []matchRow
	for mrows.Next() {
		var m matchRow
		if err := mrows.Scan(&m.suite, &m.name, &m.arch, &m.version); err != nil {
			_ = mrows.Close()
			return false, fmt.Errorf("urlPathRetainedTx: mirror scan: %w", err)
		}
		matches = append(matches, m)
	}
	if err := mrows.Err(); err != nil {
		_ = mrows.Close()
		return false, fmt.Errorf("urlPathRetainedTx: mirror iter: %w", err)
	}
	_ = mrows.Close()

	for _, m := range matches {
		// Empty-version fallback: a matching current-snapshot package_hash row
		// with no version is a non-binary artifact (Sources/pdiff/Contents) or
		// a pre-v6 row — keep it via the legacy snapshot-reference guard (the
		// proven-safe pre-version behavior).
		if m.version == "" {
			return true, nil
		}
	}
	for _, m := range matches {
		set, err := c.topNVersionSetTx(ctx, tx, scheme, host, m.suite, m.name, m.arch, maxVersions, topNCache)
		if err != nil {
			return false, err
		}
		if _, ok := set[m.version]; ok {
			return true, nil
		}
	}
	return false, nil
}

// topNVersionSetTx returns the set of raw version strings in the newest
// maxVersions Debian-version equivalence classes of (scheme, host, suite,
// name, arch) within the CURRENT snapshots, memoized per batch via topNCache.
// Current-snapshot scope (not all held snapshots) keeps the ranking anchored
// to what the live index publishes — see the mirror-match comment above.
func (c *Cache) topNVersionSetTx(ctx context.Context, tx *sql.Tx,
	scheme, host, suite, name, arch string, maxVersions int,
	topNCache map[topNKey]map[string]struct{}) (map[string]struct{}, error) {
	k := topNKey{scheme, host, suite, name, arch}
	if set, ok := topNCache[k]; ok {
		return set, nil
	}
	vrows, err := tx.QueryContext(ctx, `
SELECT DISTINCT ph.version
  FROM package_hash ph
  JOIN suite_snapshot ss ON ss.snapshot_id = ph.snapshot_id
 WHERE ss.canonical_scheme = ? AND ss.canonical_host = ? AND ss.suite_path = ?
   AND ph.package_name = ? AND ph.architecture = ? AND ph.version <> ''
   AND ph.snapshot_id IN (SELECT current_snapshot_id FROM suite_freshness WHERE current_snapshot_id IS NOT NULL)`,
		scheme, host, suite, name, arch)
	if err != nil {
		return nil, fmt.Errorf("topNVersionSetTx: %w", err)
	}
	var versions []string
	for vrows.Next() {
		var v string
		if err := vrows.Scan(&v); err != nil {
			_ = vrows.Close()
			return nil, fmt.Errorf("topNVersionSetTx scan: %w", err)
		}
		versions = append(versions, v)
	}
	if err := vrows.Err(); err != nil {
		_ = vrows.Close()
		return nil, fmt.Errorf("topNVersionSetTx iter: %w", err)
	}
	_ = vrows.Close()

	set := keepNewestNVersionSet(versions, maxVersions)
	topNCache[k] = set
	return set, nil
}

// deleteURLPathRowTx removes one url_path row and decrements the matching
// blob refcount (idempotent on RowsAffected — single-writer means no
// concurrent eviction, but the guard mirrors EvictURLPath). Sets
// refcount_zeroed_at when the refcount crosses to <= 0 so the blob pass
// can reap the bytes after blob_grace.
func deleteURLPathRowTx(ctx context.Context, tx *sql.Tx, scheme, host, path string, blobHash sql.NullString, now int64) error {
	resq, err := tx.ExecContext(ctx, `
DELETE FROM url_path
 WHERE canonical_scheme = ? AND canonical_host = ? AND path = ?`,
		scheme, host, path)
	if err != nil {
		return fmt.Errorf("deleteURLPathRowTx: delete: %w", err)
	}
	affected, err := resq.RowsAffected()
	if err != nil {
		return fmt.Errorf("deleteURLPathRowTx: rows affected: %w", err)
	}
	if affected == 0 {
		return nil
	}
	if blobHash.Valid && blobHash.String != "" {
		if _, err := tx.ExecContext(ctx, `
UPDATE blob
   SET refcount = refcount - 1,
       refcount_zeroed_at = COALESCE(
         refcount_zeroed_at,
         IIF(refcount - 1 <= 0, ?, NULL)
       )
 WHERE hash = ?`,
			now, blobHash.String); err != nil {
			return fmt.Errorf("deleteURLPathRowTx: decrement refcount: %w", err)
		}
	}
	return nil
}
