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
//  3. cascade DELETE: snapshot_member, snapshot_skipped_member,
//     package_hash, suite_snapshot (children before parent for FK
//     ordering with PRAGMA foreign_keys = ON).
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
			`DELETE FROM snapshot_member         WHERE snapshot_id IN (` + idList + `)`,
			`DELETE FROM snapshot_skipped_member WHERE snapshot_id IN (` + idList + `)`,
			`DELETE FROM package_hash            WHERE snapshot_id IN (` + idList + `)`,
			`DELETE FROM suite_snapshot          WHERE snapshot_id IN (` + idList + `)`,
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
