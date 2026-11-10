package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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
		// No representation prune here (the merge review's M-I1, the
		// maintainer, 2026-11-06): internal/gc runs PruneRepresentationMetadata
		// once a tick, at the start of its blob pass.

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

// SnapshotGCBatchResult counts the snapshots one snapshot-GC batch reaped
// or retired, broken down by reap class (sub-job A vs sub-job B per SPEC4
// §9.6.3) so the gc package can emit the gc_run_complete fields
// `orphan_candidates_reaped` and `displaced_reaped` separately.
type SnapshotGCBatchResult struct {
	OrphanReaped int
	// DisplacedReaped counts the displaced snapshots the batch retired
	// (sub-job C deletes them, and counts them again).
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
//  3. cascade DELETE of sub-job A's orphan candidates: snapshot_member,
//     snapshot_skipped_member, package_hash, suite_snapshot (children
//     before parent for FK ordering with PRAGMA foreign_keys = ON).
//  4. sub-job B's displaced snapshots get retired_at = now in one UPDATE,
//     their rows kept: sub-job C (RunRetiredSnapshotGCChunk) deletes them
//     in chunks (DN-7 of the GC and upstream hardening plan; at a start
//     too since 2026-11-09, issue #14 follow-up Tier 1b).
//  5. COMMIT
//
// staleGraceSeconds is the runtime-derived `max(total_timeout ×
// max_retries, 30m).Seconds()` from §9.6.3 sub-job A; keepDisplaced is
// `gc.keep_displaced` from config; batchSize is
// `gc.snapshot_batch_size`. The startup pass and the periodic ticks call
// it alike (internal/gc runSnapshotPass).
//
// Returns counts split by reap class. No on-disk side effects (the
// blobs a deleted snapshot pinned are reaped by the blob pass that runs
// later in the same tick or start, once sub-job C, later in the same
// snapshot pass, has deleted the snapshots this batch retired).
func (c *Cache) RunSnapshotGCBatch(ctx context.Context, batchSize int, staleGraceSeconds int64, keepDisplaced int) (SnapshotGCBatchResult, error) {
	if batchSize < 1 {
		return SnapshotGCBatchResult{}, fmt.Errorf("RunSnapshotGCBatch: batchSize must be >= 1, got %d", batchSize)
	}
	if keepDisplaced < 0 {
		return SnapshotGCBatchResult{}, fmt.Errorf("RunSnapshotGCBatch: keepDisplaced must be >= 0, got %d", keepDisplaced)
	}
	now := nowUnix()
	cutoff := now - staleGraceSeconds

	// AIDEV-NOTE: the union SELECT below mirrors SPEC4 §9.6.3, plus spec
	// C9's retired_at exclusion and ruling R-H1's legacy_trust_audit
	// exclusion (below).
	// Sub-job B's NOT IN clause must be applied BEFORE the ROW_NUMBER
	// window function (so the current snapshot is excluded from the
	// per-suite ranking, not after) — otherwise keep_displaced=3 with
	// 1 current + 4 displaced keeps only 2 displaced. The (adopted_at
	// DESC, snapshot_id DESC) tie-breaker makes ranking deterministic
	// when two displacements share the same unix-second adopted_at.
	// Retired snapshots — those of a suite reaped, evicted at a cap or
	// forgotten, those the legacy trust audit quarantined, and the displaced
	// snapshots a startup or periodic sub-job B retired (ruling HD-4; the
	// startup B too since 2026-11-09, issue #14 follow-up Tier 1b) — are
	// ranked out the same way, before the window function (spec C9): they
	// never take one of a suite's keep_displaced slots, B never selects one
	// again (so a batch never selects what an earlier one retired), and
	// sub-job C (RunRetiredSnapshotGCChunk) deletes them.
	//
	// AIDEV-NOTE: ruling R-H1 (deferred minor F-24). A snapshot still
	// queued in legacy_trust_audit — one the audit deferred after an I/O
	// fault — is ranked out too and never deleted here: never-downgrade
	// counts it as signed history (refuseUnsignedDowngrade's displaced
	// clause), the audit left its base unmarked, and only the audit can
	// judge it. Deleted, it was the base's only signed history gone, and an
	// unsigned candidate was admitted (with keep_displaced = 0); the next
	// audit would only Drop it. Kept, it waits for the verdict: Keep marks
	// the base, a quarantine retires it, and B or C then deletes it.
	// Never PF-12's marker instead: the audit may still find it plain text,
	// which must get no marker (spec §6.1). Cost (review M-9 of the H1
	// plan): a snapshot the audit defers at every start (a pool file that
	// keeps failing to read, or a classifier panic) keeps its members,
	// package rows and blobs for the life of its suite; the remedy is the
	// ERROR legacy_trust_audit_deferred at every start — fix the pool, or
	// POST /forget the suite. The one deliberate marker for such a
	// snapshot is PF-12's: a reap, eviction or forget of its suite marks
	// its base (markSnapshotHistoryTx) before retiring it.
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
       AND retired_at IS NULL
       AND snapshot_id NOT IN (SELECT current_snapshot_id
                                 FROM suite_freshness
                                WHERE current_snapshot_id IS NOT NULL)
       AND snapshot_id NOT IN (SELECT snapshot_id FROM legacy_trust_audit)
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
		// The ids split by reap class: an orphan is deleted, a displaced
		// snapshot retired. The SELECT yields only the two classes.
		var orphans, displaced []any
		for rows.Next() {
			var (
				id  int64
				cls string
			)
			if err := rows.Scan(&id, &cls); err != nil {
				_ = rows.Close()
				return fmt.Errorf("RunSnapshotGCBatch: scan: %w", err)
			}
			if cls == "displaced" {
				displaced = append(displaced, id)
			} else {
				orphans = append(orphans, id)
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("RunSnapshotGCBatch: iter: %w", err)
		}
		_ = rows.Close()

		// Cascade DELETE: children before parent. PRAGMA foreign_keys
		// = ON would FK-fail the suite_snapshot DELETE if the child
		// rows still pointed at it. An orphan candidate holds a header
		// only — only CommitAdoption inserts child rows, in the
		// transaction that sets adopted_at — so its delete is short (DN-7).
		if len(orphans) > 0 {
			idList := placeholders(len(orphans))
			for _, stmt := range []string{
				`DELETE FROM snapshot_member         WHERE snapshot_id IN (` + idList + `)`,
				`DELETE FROM snapshot_skipped_member WHERE snapshot_id IN (` + idList + `)`,
				`DELETE FROM package_hash            WHERE snapshot_id IN (` + idList + `)`,
				`DELETE FROM suite_snapshot          WHERE snapshot_id IN (` + idList + `)`,
			} {
				if _, err := tx.ExecContext(ctx, stmt, orphans...); err != nil {
					return fmt.Errorf("RunSnapshotGCBatch: cascade delete: %w", err)
				}
			}
		}
		if len(displaced) > 0 {
			// AIDEV-NOTE: rulings HD-4/HD-7 (the H3 final review R3 #4),
			// HD-7 superseded in part 2026-11-09 (issue #14 follow-up Tier 1b).
			// B used to delete a displaced snapshot's rows here, in this one
			// writer transaction: six snapshots of 70,000 package_hash rows
			// held the writer 4.7 s in a probe, so a full batch held every
			// write (a miss's store, a flip's commit) about 8 s. It now only
			// retires them, and sub-job C deletes them in chunks
			// (retiredChunkRows a transaction), in the same snapshot pass.
			// Every reader that serves, vouches or judges history filters
			// retired_at or reads current snapshots only, so a retired
			// displaced snapshot is invisible to them exactly as a deleted one
			// was, and B's selection (retired_at IS NULL) never selects it
			// again. HD-7 had the startup pass's B still delete (a
			// retireDisplaced=false mode): it ran before the scheduler and the
			// listeners, so its long transactions held up no other writer, and
			// a daemon that never lived until C ran still deleted its displaced
			// snapshots at every start. That mode is gone, one code path:
			// since PF-15's re-ruling (issue #14) the startup pass runs C
			// after B, and the start deadline (Tier 1a) bounds every startup
			// write, so a start's displaced snapshots are retired here and
			// deleted by that start's C like a periodic tick's (counted twice:
			// displaced_reaped, then retired_reaped). The periodic ticks keep
			// the retired backlog small only while one budget's C deletes what
			// a day's B retires; beyond that it grew without bound (issue #14,
			// the final review 2's G2-I2), so internal/gc repeats C hourly
			// while a backlog lasts and runs it at every start (its Run and
			// runSnapshotPass).
			res, err := tx.ExecContext(ctx,
				`UPDATE suite_snapshot SET retired_at = ? WHERE retired_at IS NULL AND snapshot_id IN (`+placeholders(len(displaced))+`)`,
				append([]any{now}, displaced...)...)
			if err != nil {
				return fmt.Errorf("RunSnapshotGCBatch: retire displaced: %w", err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("RunSnapshotGCBatch: retire displaced: %w", err)
			}
			if n != int64(len(displaced)) {
				return fmt.Errorf("RunSnapshotGCBatch: retired %d displaced snapshots, want %d", n, len(displaced))
			}
		}

		// The per-class split, from the SELECT result, is for the
		// gc_run_complete telemetry.
		result = SnapshotGCBatchResult{OrphanReaped: len(orphans), DisplacedReaped: len(displaced)}
		return tx.Commit()
	})
	if werr != nil {
		return SnapshotGCBatchResult{}, werr
	}
	return result, nil
}

// placeholders returns n comma-separated SQL parameter markers ("?,?,?")
// for an IN list; the values go in as bound arguments.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// retiredChunkRows is spec C9's chunk for snapshot-GC sub-job C: the rows
// one writer transaction deletes.
//
// AIDEV-NOTE: measured (phase F design review, M-15): one 10,000-row
// package_hash chunk held the writer about 80 ms over a 600,000-row table
// with its four indexes. Measured again 2026-10-28, Task 2's probe of the
// GC and upstream hardening plan: 120-240 ms a chunk, median 149-191 ms,
// over ~700,000 package_hash rows, no -race, a loaded 12-thread laptop.
// Re-measure before raising it.
const retiredChunkRows = 10_000

// effectiveChunkRows is the rows one sub-job C (or, since 2026-11-09,
// sub-job D: issue #14 follow-up Tier 2) transaction deletes for a
// requested maxRows: retiredChunkRows for 0 or less — what the snapshot
// pass asks for — and maxRows otherwise (deferred minor F-2: the default
// is pinned here without a 10,001-row fixture, which takes some 5 s under
// -race; the gc package pins that the pass asks for 0).
func effectiveChunkRows(maxRows int) int {
	if maxRows <= 0 {
		return retiredChunkRows
	}
	return maxRows
}

// RunRetiredSnapshotGCChunk is one writer transaction of snapshot-GC
// sub-job C (spec C9): it takes the oldest retired snapshot, deletes up to
// maxRows of its rows — snapshot_member, then snapshot_skipped_member,
// then package_hash — and, when none is left, its suite_snapshot header.
// It reports how many headers it deleted (0 or 1) and more = false only
// when no retired snapshot was left to work on; the snapshot pass calls it
// until then (or its deadline). maxRows <= 0 means retiredChunkRows.
//
// AIDEV-NOTE: a retired snapshot never serves, vouches or becomes current
// again (CommitAdoption refuses it; every reader filters retired_at), and
// its suite holds no adoption of it — every retirer holds the suite's
// mutex (spec C9), but for sub-job B (RunSnapshotGCBatch:
// DN-7 of the GC and upstream hardening plan), which
// holds none: B retires only adopted, non-current snapshots, and
// CommitAdoption, the only writer that points current_snapshot_id at a
// snapshot, commits only an unadopted candidate, so a displaced snapshot B
// retired never becomes current again — so it is deleted with no grace.
// Its pins were released when it stopped being current (displacement, the
// reap's or quarantine's Step-8 decrement); deleting its rows and header
// only drops the references the blob pass's reachability predicate reads,
// so its blobs go at the next blob pass once blob_grace has passed. Three
// callers delete them: a periodic tick's snapshot pass and the startup
// pass's (ruling PF-15, re-ruled for issue #14), whose blobs normally go
// in the same tick's or start's blob pass, and internal/gc's retired run
// (gc.interval over an hour; the final review's R3 #2), an hour after a
// start and again hourly while a run or a tick's snapshot pass ends at its
// deadline with a backlog left (issue #14), which runs no blob pass, so
// its blobs go at the next periodic tick or start. A pass or run that hits
// its deadline leaves the rest to a later one: a retired snapshot goes in
// the tick that retired it, or within the hours after it, the hourly runs
// draining a backlog at up to one max_tick_duration an hour; before issue
// #14's fix a retirement rate above one tick's capacity grew the backlog
// without bound (the final review 2's G2-I2). Chunking bounds the
// writer's hold on a large suite (tens of thousands of package_hash
// rows).
func (c *Cache) RunRetiredSnapshotGCChunk(ctx context.Context, maxRows int) (reaped int, more bool, err error) {
	maxRows = effectiveChunkRows(maxRows)
	// The op writes gotReaped/gotMore, never the named results: on a
	// ctx/close path submitWrite returns before the op ends (see
	// InsertCandidateSnapshot).
	var gotReaped int
	var gotMore bool
	werr := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("RunRetiredSnapshotGCChunk: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		var id int64
		err = tx.QueryRowContext(ctx, `
SELECT snapshot_id FROM suite_snapshot
 WHERE retired_at IS NOT NULL
   AND snapshot_id NOT IN (SELECT current_snapshot_id FROM suite_freshness WHERE current_snapshot_id IS NOT NULL)
 ORDER BY snapshot_id
 LIMIT 1`).Scan(&id)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return tx.Commit() // nothing retired is left: gotMore stays false
		case err != nil:
			return fmt.Errorf("RunRetiredSnapshotGCChunk: select: %w", err)
		}
		left := int64(maxRows)
		for _, table := range []string{"snapshot_member", "snapshot_skipped_member", "package_hash"} {
			res, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE rowid IN (SELECT rowid FROM `+table+
				` WHERE snapshot_id = ? LIMIT ?)`, id, left)
			if err != nil {
				return fmt.Errorf("RunRetiredSnapshotGCChunk: delete %s: %w", table, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("RunRetiredSnapshotGCChunk: delete %s: %w", table, err)
			}
			if left -= n; left == 0 {
				break // the chunk is full: the header waits for the next one
			}
		}
		done := 0
		if left > 0 { // every child table came up short: no row of id is left
			res, err := tx.ExecContext(ctx, `DELETE FROM suite_snapshot WHERE snapshot_id = ? AND retired_at IS NOT NULL`, id)
			if err != nil {
				return fmt.Errorf("RunRetiredSnapshotGCChunk: delete snapshot %d: %w", id, err)
			}
			// A header delete that deletes nothing means the SELECT's and the
			// DELETE's predicates disagree: fail loudly (the pass logs it)
			// rather than report done and re-select the same snapshot until
			// the deadline on every tick (the Task 4 review's minor 3).
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("RunRetiredSnapshotGCChunk: delete snapshot %d: %w", id, err)
			}
			if n != 1 {
				return fmt.Errorf("RunRetiredSnapshotGCChunk: delete snapshot %d: deleted %d rows, want 1", id, n)
			}
			done = 1
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("RunRetiredSnapshotGCChunk: commit: %w", err)
		}
		gotReaped, gotMore = done, true
		return nil
	})
	if werr != nil {
		return 0, false, werr
	}
	return gotReaped, gotMore, nil
}

// displacedPackageHashSelectSQL is sub-job D's candidate SELECT: the
// oldest adopted, non-retired, non-current snapshot that still has a
// package_hash row.
const displacedPackageHashSelectSQL = `
SELECT ss.snapshot_id FROM suite_snapshot ss
 WHERE ss.adopted_at IS NOT NULL AND ss.retired_at IS NULL
   AND ss.snapshot_id NOT IN (SELECT current_snapshot_id FROM suite_freshness WHERE current_snapshot_id IS NOT NULL)
   AND EXISTS (SELECT 1 FROM package_hash p WHERE p.snapshot_id = ss.snapshot_id)
 ORDER BY ss.snapshot_id
 LIMIT 1`

// RunDisplacedPackageHashChunk is one writer transaction of snapshot-GC
// sub-job D (issue #14 follow-up, Tier 2): it takes the oldest displaced
// snapshot — adopted, not retired, not any suite's current one — that
// still has package_hash rows, and deletes up to maxRows of them. Nothing
// else goes: the snapshot's header, snapshot_member and
// snapshot_skipped_member rows stay (forensics, the rollback check and
// retained by-hash serving read them), and so a displaced snapshot kept by
// gc.keep_displaced keeps its headers and index files, not its package
// list. It reports the rows it deleted and more = false only when no such
// snapshot was left at the start of the call; the snapshot pass calls it
// until then (or its deadline). maxRows <= 0 means retiredChunkRows. A
// snapshot still queued in legacy_trust_audit is not excluded: the audit
// never reads package_hash. Unadopted candidates and orphans hold no rows
// (only CommitAdoption inserts them, in the transaction that adopts), and
// the adopted_at term keeps D off them regardless.
//
// AIDEV-NOTE: the audit argument (spec "Audit: who reads package_hash",
// re-verified 2026-11-09). Deleting a displaced snapshot's package_hash
// rows changes no reader's answer:
//   - every reader is current-only — vouching (declaredExactSQL,
//     declaredBySuffixSQL), ListIntegrityCandidates, the url_path mirror
//     match and topNVersionSetTx, coverage and host/arch stats — or reads
//     a snapshot that is current when it reads it: ComputeHotSet's stage 1
//     reads the pre-flip prior (still current until the flip that follows
//     displaces it), reconcile's insertPackageHashTx probe reads the
//     snapshot InsertReconciledMembers checks is current in the same
//     writer transaction. flatRowsExistSQL (Open) may open false when only
//     displaced flat rows existed, which is correct: they are never read.
//     GetPackageHash has no non-test caller.
//   - a displaced snapshot never becomes current again: CommitAdoption, the
//     only writer that points current_snapshot_id at a snapshot, commits
//     only an unadopted candidate (the AIDEV-NOTE at
//     RunRetiredSnapshotGCChunk).
//   - the blob pass's reachability predicate does not read package_hash
//     (url_path, snapshot_member and suite_snapshot only), and the legacy
//     trust audit, the rollback check and by-hash serving from a displaced
//     snapshot read suite_snapshot and snapshot_member.
//
// TestPackageHashReaders_AnswerTheSameAfterTheStrip
// (displaced_package_hash_test.go) pins this. A new reader of displaced
// package_hash rows must change it — and D — first.
//
// AIDEV-NOTE: EXPLAIN QUERY PLAN of displacedPackageHashSelectSQL
// (2026-11-09; TestRunDisplacedPackageHashChunk_TheExistsUsesTheSnapshotIndex
// pins the EXISTS line):
//
//	SCAN ss
//	LIST SUBQUERY 1
//	SCAN suite_freshness
//	CREATE BLOOM FILTER
//	SEARCH p EXISTS USING COVERING INDEX idx_package_hash_snapshot (snapshot_id=?)
//
// suite_snapshot is walked in snapshot_id order (headers only: a few per
// suite, plus the retired ones C has not deleted yet) and each adopted,
// non-retired, non-current header costs one probe of
// idx_package_hash_snapshot, so a displaced snapshot already stripped
// (kept by keep_displaced) costs one index probe a call, never a
// package_hash scan. No index was added (no schema change).
//
// The chunk's DELETE is sub-job C's shape (rowid IN (… LIMIT ?)), so one
// transaction holds the writer for one chunk of one snapshot, as C's does.
func (c *Cache) RunDisplacedPackageHashChunk(ctx context.Context, maxRows int) (rows int, more bool, err error) {
	maxRows = effectiveChunkRows(maxRows)
	// The op writes gotRows/gotMore, never the named results: on a ctx/close
	// path submitWrite returns before the op ends (see InsertCandidateSnapshot).
	var gotRows int
	var gotMore bool
	werr := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("RunDisplacedPackageHashChunk: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		var id int64
		err = tx.QueryRowContext(ctx, displacedPackageHashSelectSQL).Scan(&id)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return tx.Commit() // nothing displaced holds rows: gotMore stays false
		case err != nil:
			return fmt.Errorf("RunDisplacedPackageHashChunk: select: %w", err)
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM package_hash WHERE rowid IN (SELECT rowid FROM package_hash
 WHERE snapshot_id = ? LIMIT ?)`, id, maxRows)
		if err != nil {
			return fmt.Errorf("RunDisplacedPackageHashChunk: delete snapshot %d: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("RunDisplacedPackageHashChunk: delete snapshot %d: %w", id, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("RunDisplacedPackageHashChunk: commit: %w", err)
		}
		gotRows, gotMore = int(n), true
		return nil
	})
	if werr != nil {
		return 0, false, werr
	}
	return gotRows, gotMore, nil
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
