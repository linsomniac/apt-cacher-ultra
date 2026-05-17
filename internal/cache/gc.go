package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ReapedBlob is one (hash, size) tuple returned by RunBlobGCBatch's
// DELETE...RETURNING clause. The gc package's blob-pass loop iterates
// these to unlink pool/<hash[:2]>/<hash> after the writer tx commits,
// per SPEC4 §9.6.2 buffer-close-commit-unlink ordering. Returning size
// (rather than re-reading it) lets the gc package accumulate
// bytes_reclaimed without a follow-up SELECT race.
type ReapedBlob struct {
	Hash string
	Size int64
}

// blobGCInterTxHook is a test-only seam fired between RunBlobGCBatch's
// SELECT (candidate identification) and DELETE (predicate-guarded
// reap). Production keeps it nil.
//
// SPEC4 §12.3 chaos tests stub this hook to inject inline mutations on
// the same writer-tx connection (refcount bumps, FK-bearing INSERTs,
// no-ops simulating an aborted adoption) and verify the DELETE's
// reachability predicate filters appropriately. Mutations performed
// here run via the same tx handle, so they are visible to the DELETE
// in the same transaction — exactly mirroring the on-disk semantic of
// "another writer's commit landed between SELECT and DELETE" without
// needing a second SQLite connection.
//
// The hook receives the live tx so it can EXEC inline; returning a
// non-nil error aborts the batch with that error.
var blobGCInterTxHook func(ctx context.Context, tx *sql.Tx) error

// RunBlobGCBatch executes one writer-tx blob-GC batch per SPEC4 §9.6.2:
//
//  1. BEGIN
//  2. SELECT candidate hashes (predicate: refcount<=0, eligible clock,
//     three NOT EXISTS reachability clauses). The single-writer model
//     means no other writer can land between SELECT and DELETE inside
//     this tx. Test seam blobGCInterTxHook fires here for §12.3 chaos
//     coverage.
//  3. DELETE FROM blob WHERE hash IN (...) AND <full reachability
//     predicate> RETURNING hash, size — the WHERE clause re-applies
//     the SELECT predicate atomically with the DELETE so a row whose
//     reachability changed since SELECT (in production: never, by the
//     single-writer ordering; in chaos tests: via the inter-tx hook)
//     is filtered out.
//  4. iterate rows.Next(), accumulating (hash, size) into a Go slice
//  5. rows.Close() — required by SQLite before COMMIT (the RETURNING
//     cursor pins the tx)
//  6. COMMIT
//
// The on-disk os.Remove calls happen *after* COMMIT, outside this
// helper. Crash-safety derives from the ordering: a process killed
// between COMMIT and the unlink loop leaves a pool/<hash> file with no
// blob row, which the next startup §4.2 pool scan reaps.
//
// Returns the buffered (hash, size) tuples corresponding exactly to
// the rows the DELETE removed. On any DB error the tx rolls back and
// no reaped blobs are returned (the caller MUST NOT unlink anything).
//
// graceSeconds is `gc.blob_grace.Seconds()`; the SELECT predicate
// requires `refcount_zeroed_at < now - graceSeconds`. The full
// reachability predicate (3 NOT EXISTS clauses) is applied to both
// SELECT and DELETE — see SPEC4 §9.6.2 for the rationale.
func (c *Cache) RunBlobGCBatch(ctx context.Context, batchSize int, graceSeconds int64) ([]ReapedBlob, error) {
	if batchSize < 1 {
		return nil, fmt.Errorf("RunBlobGCBatch: batchSize must be >= 1, got %d", batchSize)
	}
	now := nowUnix()
	cutoff := now - graceSeconds

	const selectSQL = `
SELECT hash FROM blob
 WHERE refcount <= 0
   AND refcount_zeroed_at IS NOT NULL
   AND refcount_zeroed_at < ?
   AND NOT EXISTS (SELECT 1 FROM url_path
                    WHERE blob_hash = blob.hash)
   AND NOT EXISTS (SELECT 1 FROM snapshot_member
                    WHERE blob_hash = blob.hash)
   AND NOT EXISTS (SELECT 1 FROM suite_snapshot
                    WHERE inrelease_hash   = blob.hash
                       OR release_hash     = blob.hash
                       OR release_gpg_hash = blob.hash)
 ORDER BY refcount_zeroed_at
 LIMIT ?`

	var reaped []ReapedBlob
	werr := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("RunBlobGCBatch: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		// Step 1: SELECT candidate hashes inside the tx — the DELETE
		// re-applies the same predicate atomically, but doing the
		// SELECT here too means we know what to delete. Capture the
		// hash list into Go memory; sub-tens-of-KiB at batchSize=100.
		rows, err := tx.QueryContext(ctx, selectSQL, cutoff, batchSize)
		if err != nil {
			return fmt.Errorf("RunBlobGCBatch: select: %w", err)
		}
		var hashes []string
		for rows.Next() {
			var h string
			if err := rows.Scan(&h); err != nil {
				_ = rows.Close()
				return fmt.Errorf("RunBlobGCBatch: scan: %w", err)
			}
			hashes = append(hashes, h)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("RunBlobGCBatch: iter: %w", err)
		}
		_ = rows.Close()

		if len(hashes) == 0 {
			return tx.Commit()
		}

		// SPEC4 §12.3 test seam: chaos drivers inject inline mutations
		// here (refcount bumps, url_path INSERTs, no-ops) to exercise
		// the DELETE's predicate re-application. Production sets the
		// hook to nil. The mutations land on the same tx so the DELETE
		// sees them — semantically equivalent to a parallel writer's
		// commit landing between SELECT and DELETE.
		if hook := blobGCInterTxHook; hook != nil {
			if err := hook(ctx, tx); err != nil {
				return fmt.Errorf("RunBlobGCBatch: inter-tx hook: %w", err)
			}
		}

		// Step 2: DELETE...RETURNING with the same reachability
		// predicate. The hash IN (...) clause limits the DELETE to
		// the SELECT's candidate set; the WHERE clause filters out
		// any candidate whose reachability changed between SELECT
		// and now (single-writer model means the only mutator is
		// this writer goroutine, but adopting code is queued and
		// could be the immediately-next op — we still want the
		// guard for the chaos-test SELECT-fence-DELETE timing).
		//
		// Build the placeholder list in Go memory; we trust hashes
		// as they came directly from a SELECT on this trusted DB
		// column (already CHECK-validated to sha256-hex shape on
		// insert).
		args := make([]any, 0, len(hashes)+1)
		ph := make([]byte, 0, 2*len(hashes))
		for i, h := range hashes {
			if i > 0 {
				ph = append(ph, ',')
			}
			ph = append(ph, '?')
			args = append(args, h)
		}
		args = append(args, cutoff)

		deleteSQL := `
DELETE FROM blob
 WHERE hash IN (` + string(ph) + `)
   AND refcount <= 0
   AND refcount_zeroed_at IS NOT NULL
   AND refcount_zeroed_at < ?
   AND NOT EXISTS (SELECT 1 FROM url_path
                    WHERE blob_hash = blob.hash)
   AND NOT EXISTS (SELECT 1 FROM snapshot_member
                    WHERE blob_hash = blob.hash)
   AND NOT EXISTS (SELECT 1 FROM suite_snapshot
                    WHERE inrelease_hash   = blob.hash
                       OR release_hash     = blob.hash
                       OR release_gpg_hash = blob.hash)
RETURNING hash, size`

		drows, err := tx.QueryContext(ctx, deleteSQL, args...)
		if err != nil {
			return fmt.Errorf("RunBlobGCBatch: delete: %w", err)
		}
		for drows.Next() {
			var r ReapedBlob
			if err := drows.Scan(&r.Hash, &r.Size); err != nil {
				_ = drows.Close()
				return fmt.Errorf("RunBlobGCBatch: scan returning: %w", err)
			}
			reaped = append(reaped, r)
		}
		if err := drows.Err(); err != nil {
			_ = drows.Close()
			return fmt.Errorf("RunBlobGCBatch: iter returning: %w", err)
		}
		_ = drows.Close()

		return tx.Commit()
	})
	if werr != nil {
		return nil, werr
	}
	return reaped, nil
}

// SnapshotGCBatchResult counts the rows reaped by one snapshot-GC
// batch, broken down by reap class (sub-job A vs sub-job B per SPEC4
// §9.6.3) so the gc package can emit the gc_run_complete fields
// `orphan_candidates_reaped` and `displaced_reaped` separately.
type SnapshotGCBatchResult struct {
	OrphanReaped    int
	DisplacedReaped int
}

// Total is the sum of the two reap classes — feeds the loop's
// "are there more?" check (zero means the candidate set is drained).
func (r SnapshotGCBatchResult) Total() int {
	return r.OrphanReaped + r.DisplacedReaped
}

// RunSnapshotGCBatch executes one writer-tx snapshot-GC batch per
// SPEC4 §9.6.3:
//
//  1. BEGIN
//  2. SELECT (sub-job A ∪ sub-job B) candidate ids inside the tx — the
//     SELECT-DELETE liveness race is closed by single-writer ordering.
//  3. cascade DELETE: snapshot_member, package_hash, suite_snapshot
//     (children before parent for FK ordering with PRAGMA
//     foreign_keys = ON).
//  4. COMMIT
//
// staleGraceSeconds is the runtime-derived `max(total_timeout ×
// max_retries, 30m).Seconds()` from §9.6.3 sub-job A; keepDisplaced is
// `gc.keep_displaced` from config; batchSize is
// `gc.snapshot_batch_size`.
//
// Returns counts split by reap class. No on-disk side effects (the
// blobs the snapshot pinned are reaped by the blob pass that runs
// later in the same tick).
func (c *Cache) RunSnapshotGCBatch(ctx context.Context, batchSize int, staleGraceSeconds int64, keepDisplaced int) (SnapshotGCBatchResult, error) {
	if batchSize < 1 {
		return SnapshotGCBatchResult{}, fmt.Errorf("RunSnapshotGCBatch: batchSize must be >= 1, got %d", batchSize)
	}
	if keepDisplaced < 0 {
		return SnapshotGCBatchResult{}, fmt.Errorf("RunSnapshotGCBatch: keepDisplaced must be >= 0, got %d", keepDisplaced)
	}
	now := nowUnix()
	cutoff := now - staleGraceSeconds

	// AIDEV-NOTE: the union SELECT below mirrors SPEC4 §9.6.3 verbatim.
	// Sub-job B's NOT IN clause must be applied BEFORE the ROW_NUMBER
	// window function (so the current snapshot is excluded from the
	// per-suite ranking, not after) — otherwise keep_displaced=3 with
	// 1 current + 4 displaced keeps only 2 displaced. The (adopted_at
	// DESC, snapshot_id DESC) tie-breaker makes ranking deterministic
	// when two displacements share the same unix-second adopted_at.
	const selectSQL = `
SELECT snapshot_id, reap_class FROM (
  SELECT snapshot_id, 'orphan' AS reap_class FROM suite_snapshot
   WHERE adopted_at IS NULL
     AND heartbeat_at < ?
     AND snapshot_id NOT IN (SELECT current_snapshot_id
                               FROM suite_freshness
                              WHERE current_snapshot_id IS NOT NULL)

  UNION ALL

  SELECT snapshot_id, 'displaced' AS reap_class FROM (
    SELECT snapshot_id,
           ROW_NUMBER() OVER (
             PARTITION BY canonical_scheme, canonical_host, suite_path
             ORDER BY adopted_at DESC, snapshot_id DESC
           ) AS rn
      FROM suite_snapshot
     WHERE adopted_at IS NOT NULL
       AND snapshot_id NOT IN (SELECT current_snapshot_id
                                 FROM suite_freshness
                                WHERE current_snapshot_id IS NOT NULL)
  ) WHERE rn > ?
)
LIMIT ?`

	var result SnapshotGCBatchResult
	werr := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("RunSnapshotGCBatch: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		rows, err := tx.QueryContext(ctx, selectSQL, cutoff, keepDisplaced, batchSize)
		if err != nil {
			return fmt.Errorf("RunSnapshotGCBatch: select: %w", err)
		}
		var ids []int64
		var classes []string
		for rows.Next() {
			var (
				id  int64
				cls string
			)
			if err := rows.Scan(&id, &cls); err != nil {
				_ = rows.Close()
				return fmt.Errorf("RunSnapshotGCBatch: scan: %w", err)
			}
			ids = append(ids, id)
			classes = append(classes, cls)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("RunSnapshotGCBatch: iter: %w", err)
		}
		_ = rows.Close()

		if len(ids) == 0 {
			return tx.Commit()
		}

		// Cascade DELETE: children before parent. PRAGMA foreign_keys
		// = ON would FK-fail the suite_snapshot DELETE if the child
		// rows still pointed at it.
		args := make([]any, 0, len(ids))
		ph := make([]byte, 0, 2*len(ids))
		for i, id := range ids {
			if i > 0 {
				ph = append(ph, ',')
			}
			ph = append(ph, '?')
			args = append(args, id)
		}
		idList := string(ph)
		for _, stmt := range []string{
			`DELETE FROM snapshot_member WHERE snapshot_id IN (` + idList + `)`,
			`DELETE FROM package_hash    WHERE snapshot_id IN (` + idList + `)`,
			`DELETE FROM suite_snapshot  WHERE snapshot_id IN (` + idList + `)`,
		} {
			if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
				return fmt.Errorf("RunSnapshotGCBatch: cascade delete: %w", err)
			}
		}

		// Tally reap classes from the SELECT result — the DELETE's
		// RowsAffected on suite_snapshot equals len(ids) under
		// single-writer ordering, but we want the per-class split for
		// gc_run_complete telemetry.
		for _, cls := range classes {
			switch cls {
			case "orphan":
				result.OrphanReaped++
			case "displaced":
				result.DisplacedReaped++
			}
		}
		return tx.Commit()
	})
	if werr != nil {
		return SnapshotGCBatchResult{}, werr
	}
	return result, nil
}

// HashKnown reports whether a blob row exists for hash. Used by the
// SPEC4 §9.6.4 startup pool scan: a file at pool/<prefix>/<hash> with
// no blob row is an orphan (left behind by a process killed between
// COMMIT and os.Remove, or by a Phase-3 fetch that finalized but whose
// PutBlob never landed) and is reaped.
//
// Returns ErrInvalidHash if the hash is malformed; the caller should
// treat that as "leave the file alone, log gc_pool_malformed_name".
func (c *Cache) HashKnown(ctx context.Context, hash string) (bool, error) {
	if !validBlobHash(hash) {
		return false, fmt.Errorf("%w: %q", ErrInvalidHash, hash)
	}
	const q = `SELECT 1 FROM blob WHERE hash = ?`
	var one int
	err := c.db.QueryRowContext(ctx, q, hash).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("HashKnown: %w", err)
	}
	return true, nil
}

// HeartbeatBlobs refreshes refcount_zeroed_at = now on every blob row
// in `hashes` whose refcount is still <= 0 (i.e. has not yet been
// claimed by a CommitAdoption Step 4 increment). Returns nil
// immediately on an empty slice.
//
// The intent (SPEC4 §7.5.1 Rule 1 race-window extension): an adoption
// goroutine PutBlob's its member blobs in a sequential loop that may
// span minutes (large suites, slow upstreams, hot-prefetch loop with
// hot_prefetch_budget=0). Each PutBlob sets refcount_zeroed_at =
// created_at = now-at-INSERT-time. Without periodic refresh, a member
// blob fetched in the first minute of a 6-minute adoption ages past
// gc.blob_grace before CommitAdoption can insert its snapshot_member
// row — at which point the §9.6.2 reap predicate fires and the FK
// INSERT in CommitAdoption fails with ON CASCADE NULL or constraint
// failure.
//
// Calling HeartbeatBlobs from each §7.5.2 heartbeat site (including
// the periodic ticker) keeps the in-flight member blobs' grace clocks
// at "less than gc.heartbeat_interval old", well within
// gc.blob_grace. The WHERE refcount <= 0 predicate ensures Rule 2's
// strictly-positive crossing is preserved — once CommitAdoption Step
// 4 lands and refcount_zeroed_at is set to NULL, subsequent
// HeartbeatBlobs calls during the same writer-tick (or after) become
// no-ops on those rows.
//
// Hashes that don't validate are filtered out (and a stub error is
// returned with the count of skipped rows; callers may log or ignore).
// A hash list that's entirely invalid still produces no DB write.
func (c *Cache) HeartbeatBlobs(ctx context.Context, hashes []string) error {
	if len(hashes) == 0 {
		return nil
	}
	// Filter to known-valid hashes. Defense-in-depth: hashes here
	// come from in-memory state in the adoption goroutine, but the
	// validBlobHash gate matches the schema CHECK and prevents any
	// malformed value from entering the SQL IN-list.
	valid := make([]string, 0, len(hashes))
	for _, h := range hashes {
		if validBlobHash(h) {
			valid = append(valid, h)
		}
	}
	if len(valid) == 0 {
		return nil
	}
	// Build the IN clause. Only literal '?' placeholders are
	// emitted — the hashes go through ExecContext as bound args.
	args := make([]any, 0, len(valid)+1)
	args = append(args, nowUnix())
	ph := make([]byte, 0, 2*len(valid))
	for i, h := range valid {
		if i > 0 {
			ph = append(ph, ',')
		}
		ph = append(ph, '?')
		args = append(args, h)
	}
	q := `UPDATE blob SET refcount_zeroed_at = ? WHERE refcount <= 0 AND hash IN (` + string(ph) + `)`
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("HeartbeatBlobs: %w", err)
		}
		return nil
	})
}

// RunURLPathGCBatch executes one writer-tx URL-path-TTL batch:
//
//  1. BEGIN
//  2. SELECT up to batchSize url_path candidates whose last_requested_at
//     is older than now-ttlSeconds AND which are not vouched for by any
//     current snapshot via package_hash, snapshot_member, or
//     suite_snapshot. Rows with last_requested_at IS NULL (adoption-
//     pre-warmed but never served) are protected unconditionally.
//
//     "Vouched for by a current snapshot" means one of:
//       a. There exists a package_hash row with the same (scheme, host,
//          path) AND declared_sha256 = url_path.blob_hash on a current
//          snapshot. Path-AND-hash equality matters here — a row whose
//          cached bytes diverge from the snapshot's declared hash is
//          stale and reapable; the next client request would hit-path
//          evict it anyway (SPEC2 §6.1 step 5).
//       b. There exists a snapshot_member row with blob_hash =
//          url_path.blob_hash on a current snapshot. Hash-based check
//          (no path equality) because snapshot_member's path is suite-
//          relative; matching by hash is sufficient since the bytes are
//          what the snapshot vouches for. Covers cached Packages.gz,
//          Sources, pdiff Index members, etc.
//       c. There exists a suite_snapshot row whose inrelease_hash,
//          release_hash, or release_gpg_hash equals url_path.blob_hash
//          on a current snapshot. Covers cached InRelease, Release,
//          Release.gpg url_path rows — critically important because
//          freshness checks (SPEC2 §7.4) skip silently when these
//          url_path rows are absent.
//  3. DELETE the rows
//  4. For each deleted row with a non-NULL blob_hash, decrement
//     blob.refcount and set refcount_zeroed_at when the refcount
//     crosses to <= 0 — mirrors EvictURLPath's bookkeeping exactly so
//     the next blob-GC pass can reap the bytes once gc.blob_grace
//     elapses.
//  5. COMMIT
//
// ttlSeconds is `gc.url_path_ttl.Seconds()`. The caller MUST verify
// ttlSeconds > 0 before invoking; a 0 value means the TTL pass is
// disabled and RunURLPathGCBatch should not be called.
//
// Returns the number of url_path rows deleted. The returned count is
// the row count, not the blob count — multiple url_path rows can point
// at the same blob, so the resulting blob.refcount decrement count
// equals the row count, not distinct blob count.
//
// AIDEV-NOTE: this is the SPEC4 §5 fourth reap class (after blobs,
// orphan candidate snapshots, displaced snapshots). It does not unlink
// any pool bytes itself — that remains the blob pass's job, which fires
// on the next tick (or next batch within the same tick) once refcount
// has been decremented to <= 0 and the blob_grace window elapses.
//
// AIDEV-NOTE: a url_path row whose blob_hash IS NULL (PutURLPath landed
// but the fetch hasn't completed, or the upstream 404'd) cannot match
// any of the hash-based vouching subqueries — `= NULL` evaluates to
// NULL, so NOT EXISTS is true. Such rows are reapable on TTL elapse,
// which is correct: a failed-to-fetch URL mapping has no reason to
// persist forever, and the next request just re-resolves it.
func (c *Cache) RunURLPathGCBatch(ctx context.Context, batchSize int, ttlSeconds int64) (int, error) {
	if batchSize < 1 {
		return 0, fmt.Errorf("RunURLPathGCBatch: batchSize must be >= 1, got %d", batchSize)
	}
	if ttlSeconds <= 0 {
		return 0, fmt.Errorf("RunURLPathGCBatch: ttlSeconds must be > 0, got %d", ttlSeconds)
	}
	now := nowUnix()
	cutoff := now - ttlSeconds

	const selectSQL = `
SELECT canonical_scheme, canonical_host, path, blob_hash
  FROM url_path
 WHERE last_requested_at IS NOT NULL
   AND last_requested_at < ?
   AND NOT EXISTS (
         SELECT 1 FROM package_hash ph
          WHERE ph.canonical_scheme = url_path.canonical_scheme
            AND ph.canonical_host   = url_path.canonical_host
            AND ph.path             = url_path.path
            AND ph.declared_sha256  = url_path.blob_hash
            AND ph.snapshot_id IN (
                  SELECT current_snapshot_id FROM suite_freshness
                   WHERE current_snapshot_id IS NOT NULL
                )
       )
   AND NOT EXISTS (
         SELECT 1 FROM snapshot_member sm
          WHERE sm.blob_hash = url_path.blob_hash
            AND sm.snapshot_id IN (
                  SELECT current_snapshot_id FROM suite_freshness
                   WHERE current_snapshot_id IS NOT NULL
                )
       )
   AND NOT EXISTS (
         SELECT 1 FROM suite_snapshot ss
          WHERE (ss.inrelease_hash   = url_path.blob_hash
              OR ss.release_hash     = url_path.blob_hash
              OR ss.release_gpg_hash = url_path.blob_hash)
            AND ss.snapshot_id IN (
                  SELECT current_snapshot_id FROM suite_freshness
                   WHERE current_snapshot_id IS NOT NULL
                )
       )
 ORDER BY last_requested_at
 LIMIT ?`

	type row struct {
		scheme, host, path string
		blobHash           sql.NullString
	}

	var reaped int
	werr := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("RunURLPathGCBatch: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		rows, err := tx.QueryContext(ctx, selectSQL, cutoff, batchSize)
		if err != nil {
			return fmt.Errorf("RunURLPathGCBatch: select: %w", err)
		}
		var batch []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.scheme, &r.host, &r.path, &r.blobHash); err != nil {
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

		if len(batch) == 0 {
			return tx.Commit()
		}

		// AIDEV-NOTE: per-row DELETE + UPDATE rather than a bulk DELETE
		// because we need to decrement the matching blob.refcount only
		// when the url_path row actually went away (an idempotent
		// guard: if a concurrent EvictURLPath wins the row between
		// SELECT and DELETE, we must NOT double-decrement). Idempotency
		// here is the same property EvictURLPath relies on.
		for _, r := range batch {
			res, err := tx.ExecContext(ctx, `
DELETE FROM url_path
 WHERE canonical_scheme = ? AND canonical_host = ? AND path = ?`,
				r.scheme, r.host, r.path)
			if err != nil {
				return fmt.Errorf("RunURLPathGCBatch: delete: %w", err)
			}
			affected, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("RunURLPathGCBatch: rows affected: %w", err)
			}
			if affected == 0 {
				continue
			}
			reaped++
			if r.blobHash.Valid && r.blobHash.String != "" {
				if _, err := tx.ExecContext(ctx, `
UPDATE blob
   SET refcount = refcount - 1,
       refcount_zeroed_at = COALESCE(
         refcount_zeroed_at,
         IIF(refcount - 1 <= 0, ?, NULL)
       )
 WHERE hash = ?`,
					now, r.blobHash.String); err != nil {
					return fmt.Errorf("RunURLPathGCBatch: decrement refcount: %w", err)
				}
			}
		}
		return tx.Commit()
	})
	if werr != nil {
		return 0, werr
	}
	return reaped, nil
}
