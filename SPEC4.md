# apt-cacher-ultra — Phase 4 Specification

This document specifies the contract for Phase 4: garbage collection
of orphan blobs and snapshots. It is a delta over [SPEC.md](SPEC.md)
(Phase 1), [SPEC2.md](SPEC2.md) (Phase 2), and [SPEC3.md](SPEC3.md)
(Phase 3). Sections that carry forward unchanged say so explicitly
and point at the prior spec; sections that change describe only the
delta. The companion document
[PHASE-4-SCOPING.md](PHASE-4-SCOPING.md) records the design
rationale and the eight-question scoping pass that produced this
spec.

Phase 4 is purely additive over Phase 3: no existing request path,
adoption path, freshness path, or wire contract changes. The new
behavior runs in a single dedicated goroutine, plus three small
maintenance edits to existing refcount-mutating SQL statements
(§7.5.1, §6.1, §6.2).

---

## 1. Goals & non-goals

### 1.1 Phase 4 goals

1. **Reap unreferenced `pool/` blobs.** The snapshot model produces
   orphans by design — every adoption that displaces a prior
   snapshot decrements the prior's blob refcounts (Phase 2
   `CommitAdoption` Step 8), and `EvictURLPath` decrements when a
   §6.1 hit-path eviction races a snapshot transition. Refcount
   math has been correct since Phase 2; nothing has been sweeping.
   Phase 4 sweeps. The reap predicate is a "since-refcount-reached-
   zero" grace clock (§4.3.1, §7.5.1), not a `created_at`-based
   one — the right signal is "how long has this been
   unreferenced," not "how long has this existed."

2. **Reap orphan candidate snapshots.** Phase 2 §7.5.2 documents
   that failed adoptions leave `suite_snapshot` rows with
   `adopted_at IS NULL` as "harmless residue" awaiting Phase 4 GC.
   Same for adoptions cancelled by graceful shutdown (SPEC2 §9.5
   step 5). Phase 4 reaps candidates older than
   `max(upstream.total_timeout × upstream.max_retries, 30m)` — a
   bound that strictly exceeds any in-flight adoption's
   worst-case duration.

3. **Reap displaced snapshots beyond a forensic retention window.**
   Once a `current_snapshot_id` flip displaces a prior snapshot,
   the refcount math already accounted for the bytes; the rows
   themselves (`suite_snapshot` + `snapshot_member` +
   `package_hash`) are unreferenced bookkeeping. Phase 4 keeps the
   `gc.keep_displaced` most recent displaced snapshots per suite
   (default 3) and reaps the rest. The retention is for
   debug-after-bad-rollout, not for serving traffic — adopted-then-
   displaced snapshots are never read by the request path.

4. **Repair `pool/` orphan files at startup.** A `pool/<hash>` file
   without a matching `blob` row indicates a prior crash mid-rename
   or mid-DELETE-and-unlink. Belt-and-suspenders one-shot scan at
   startup; not on the periodic ticker.

The four jobs share one `[gc]` configuration block, one periodic
goroutine, and one log-event family (§10).

### 1.2 Phase 4 non-goals (deferred)

Carried forward from earlier phases:

- Status page / `/metrics` endpoint (Phase 5).
- TLS MITM listener (Phase 6).
- Source-package caching, multi-arch beyond amd64, pdiff (Phase 6+).
- Streaming-while-fetching as a singleflight optimization. Deferred
  to [FUTURE-REVIEW.md §1](FUTURE-REVIEW.md).
- Per-byte upstream read timeouts. Deferred to
  [FUTURE-REVIEW.md §1](FUTURE-REVIEW.md).
- Per-suite freshness cadence variation. Deferred to
  [FUTURE-REVIEW.md §2](FUTURE-REVIEW.md).

Explicitly resolved during Phase 4 scoping:

- **Operator-triggered manual GC** (admin endpoint or SIGUSR1).
  Achievable as a Phase 4 add-on if needed but not part of the
  gating contract; the periodic ticker plus a startup pass is
  sufficient. Deferred to Phase 6+ as an optional enhancement.
- **Periodic `pool/` orphan-file rescan.** Walking a multi-GiB pool
  every hour is wasteful, and the only way an orphan file is
  created is a crash between §4.2's GC commit-and-unlink steps.
  Bounded by *time to next process restart*, which is the right
  cadence. Periodic rescan is rejected as solving a non-problem.

---

## 2. Wire contracts

Unchanged — see SPEC.md §2 / SPEC2 §2 / SPEC3 §2.7. Phase 4 adds no
new response headers and changes no request-path behavior. GC
runs entirely on a dedicated goroutine and is invisible at the wire
level.

---

## 3. URL canonicalization (Remap)

Unchanged — see SPEC.md §3.

---

## 4. Storage layout

### 4.1 Disk

Unchanged — see SPEC.md §4.1 / SPEC2 §4.1 / SPEC3 §4.1. The on-disk
layout (`pool/`, `tmp/`, `staging/`, `cache.db`, `cache.db-wal`,
`cache.db-shm`) is exactly Phase 3's. Phase 4 introduces no new
directories. Reaped pool files are unlinked in place; reaped DB
rows are deleted in place.

### 4.2 Startup cleanup

Phase 1's `tmp/` mtime sweep and Phase 2's `staging/` mtime sweep
carry forward unchanged. Phase 4 adds two new startup-time passes,
sequenced *after* the existing sweeps:

1. **Pool/ orphan-file scan.** Walks `pool/<two-hex-prefix>/<hash>`
   directories; for each file, runs
   `SELECT 1 FROM blob WHERE hash = ?`; if absent, unlinks the
   file. Rate-limited via a worker pool sized at
   `gc.pool_scan_workers` (default 4). Cancellable via
   `lifecycleCtx`. Counts unlinked files and reclaimed bytes into
   the startup `gc_run_complete` event (§10).

2. **One-shot blob + snapshot GC pass.** Runs the same per-tick
   logic described in §9.6, immediately after the pool scan. This
   drains any backlog accumulated while the previous process was
   stopped (most importantly: pre-Phase-4 deployments upgrading in
   place will see a large initial reap, since pre-v4 refcount=0
   blobs have `refcount_zeroed_at = created_at` per the migration).

Order matters: the pool scan runs first so its
`gc_pool_orphans_repaired` count reflects only pre-existing orphan
files (not files just created by GC's first periodic pass).

Sequencing within `cache.Open`:

```
1. SQLite open + migrate                      (Phase 1)
2. tmp/ mtime sweep                           (Phase 1)
3. staging/ mtime sweep                       (Phase 2)
4. pool/ orphan-file scan                     (Phase 4 NEW, §4.2)
5. one-shot GC pass                           (Phase 4 NEW, §4.2)
6. listeners come up                          (Phase 1)
7. periodic schedulers start                  (Phase 1, 2, 3, 4)
```

The cache does not begin answering requests until step 6 — the
startup GC pass is blocking, parallel to the existing migration
step. Operators with very large pools should expect startup latency
proportional to `pool/` size.

### 4.3 SQLite schema

Phase 4 schema is `schema_version = 4`. Migration v3 → v4 is
described in §4.3.2.

#### 4.3.1 Phase 4 schema delta

One new column on `blob`, one new partial index:

```sql
-- The "since refcount reached 0" grace clock. NULL when refcount is
-- strictly positive. Set to the unix epoch second of the transition
-- when refcount drops to ≤ 0; cleared back to NULL when refcount
-- transitions back to strictly positive. New blob rows are born
-- at refcount=0 by PutBlob, so refcount_zeroed_at is set to
-- created_at on insert (§7.5.1).
ALTER TABLE blob ADD COLUMN refcount_zeroed_at INTEGER;

-- Partial index over the GC candidate set only. Keeps the index
-- small under steady state (the candidate set is tiny relative to
-- the full blob table) and makes the §9.6 reap query index-only.
-- Ordering: (refcount_zeroed_at) so the SELECT can ORDER BY oldest
-- first without a separate sort step.
CREATE INDEX idx_blob_gc
  ON blob(refcount_zeroed_at)
  WHERE refcount <= 0;
```

No new tables. No changes to `url_path`, `suite_snapshot`,
`snapshot_member`, `package_hash`, `suite_freshness`, or
`schema_version`.

The `<= 0` (not `= 0`) in the partial index predicate is
load-bearing: SPEC2 §6.1 step 5 documents that a transient
`refcount = -1` is reachable when an adoption transaction's
decrement races a §6.1 hit-path eviction's decrement. Both
decrements are correct per their own bookkeeping; the row should
still be reaped.

#### 4.3.2 Migration v3 → v4

```
migrations[3] = v3 → v4:
  ALTER TABLE blob ADD COLUMN refcount_zeroed_at INTEGER;
  UPDATE blob SET refcount_zeroed_at = created_at
   WHERE refcount <= 0;
  CREATE INDEX idx_blob_gc ON blob(refcount_zeroed_at)
   WHERE refcount <= 0;
  -- migrate.go bumps schema_version to 4 after success via the
  -- existing applyMigration UPDATE; the migration body must NOT
  -- include an INSERT or UPDATE on schema_version itself.
```

Properties:

- **Forward-only.** Phase 1's `migrate` already enforces this; v4
  keeps the contract.
- **Atomic.** The migration framework runs each migration inside a
  single transaction; an interrupted migration rolls back fully,
  and the next start retries from `schema_version = 3`.
- **Backfill semantics.** Pre-v4 rows at `refcount <= 0` have an
  unknown actual transition time. The migration sets
  `refcount_zeroed_at = created_at` — the conservative choice. If
  the row has been ≤ 0 the entire time it has existed, the grace
  has long since elapsed and the next GC tick reaps; if it
  transitioned to ≤ 0 yesterday and we attribute the transition to
  `created_at`, we still reap correctly, just one grace too soon
  rather than too late. This is a one-time pre-Phase-4 backlog
  drain; steady state has the column maintained by §7.5.1.
- **Index creation cost.** The `CREATE INDEX idx_blob_gc` walks
  only rows matching `refcount <= 0`. For typical Phase 3
  deployments this is a tiny fraction of the blob table; the
  index build is sub-second. For long-running caches that have
  accumulated GC backlog, index creation scales with the orphan
  count, not the total pool size.
- **Pre-v4 deployments not gated.** The v3 → v4 migration end-to-
  end test described in §12.7 is **not** required for Phase 4
  done — same posture as v2 → v3 in SPEC3 §12.5. The sole
  pre-release deployment will drop and recreate the cache
  directory.

### 4.4 Suite identification

Unchanged — see SPEC.md §4.4.

### 4.5 Classifying metadata vs. blob

Unchanged — see SPEC.md §4.5 / SPEC2 §4.5.

---

## 5. Configuration (TOML)

### 5.1 Example (deltas)

Existing sections (Phase 1 + Phase 2 + Phase 3 keys) carry forward
unchanged. Phase 4 adds one new top-level block:

```toml
[gc]
# Master switch. When false, the goroutine is not started, the
# startup pool scan is skipped, and the startup GC pass is skipped.
# A startup gc_disabled Warn fires when false to surface the
# operator's choice (parallel to refuse_unvouched_debs_inert).
# Default true: the conservative grace clock plus batched cadence
# plus startup-pass design make the feature safe-by-default;
# opting out requires the same level of awareness as opting in
# to a feature with a real failure mode.
enabled               = true

# Cadence of the periodic GC tick. Default 1h. The startup pass
# (§4.2) runs once on boot regardless. 0s is invalid (use enabled =
# false to disable; an interval that says "never" is ambiguous).
interval              = "1h"

# Number of blob rows reaped per per-tick batch. The §9.6 reap
# loop runs batches until either the candidate set empties or a
# per-tick wall-clock guard trips. Default 100: each batch is one
# write transaction, finishes in milliseconds on a healthy WAL,
# and contributes minimal write-lock latency to concurrent
# adoptions / EvictURLPath.
batch_size            = 100

# The "since refcount reached 0" grace before a blob becomes
# reapable. The reap predicate is `refcount <= 0 AND
# refcount_zeroed_at IS NOT NULL AND refcount_zeroed_at < now -
# blob_grace`. Default 5m, matching tmp/'s mtime cutoff — the
# existing "is this in flight?" timescale in the system. A 0s
# grace makes refcount=0 blobs immediately reapable, which is
# unsafe (a fetch that just-finished but whose url_path INSERT
# hasn't committed yet would be reaped). 0s is rejected at config
# load.
blob_grace            = "5m"

# Per-suite forensic retention for displaced snapshots. The
# `gc.keep_displaced` most recent displaced snapshots per
# (canonical_scheme, canonical_host, suite_path) are preserved for
# operator inspection after a bad rollout; older displaced
# snapshots are reaped. Default 3. 0 disables retention entirely
# (every displaced snapshot is reapable on the next tick).
keep_displaced        = 3

# Worker pool size for the startup-only pool/ orphan-file scan.
# Default 4, parallel to integrity.validate_at_rest_workers. Must
# be >= 1.
pool_scan_workers     = 4
```

### 5.2 Config validation (deltas)

Phase 1 + Phase 2 + Phase 3 validation carries forward. Phase 4
adds:

- `gc.enabled` is bool, default `true`.
- `gc.interval` parses as duration, > 0. The cadence is purely a
  floor; the goroutine reads the value once at startup and the
  ticker uses it. There is no documented `0`-means-something
  semantics here, so this key is *not* presence-sensitive (no
  `IsDefined` check needed).
- `gc.batch_size` parses as int, ≥ 1. 0 is rejected at load
  (an interval-bounded loop with batch_size=0 is an infinite
  busy-loop).
- `gc.blob_grace` parses as duration, > 0. 0 is rejected at load
  (see the example block above).
- `gc.keep_displaced` parses as int, ≥ 0. 0 is permitted (means
  "no forensic retention").
- `gc.pool_scan_workers` parses as int, ≥ 1.

Loud configurations (warning logs at startup):

- `gc.enabled = false` — names the disabled state
  (`gc_disabled` Warn). The cache still works, but disk usage
  will grow unbounded as adoptions roll.

---

## 6. Request handling

Unchanged — see SPEC.md §6 / SPEC2 §6 / SPEC3 §6 with one
sub-section delta below.

### 6.1 The fast path: cache hit

Unchanged structurally — see SPEC3 §6.1. The §6.1 hit-path
eviction (the "evict url_path, fall through to §6.2" branch when
a Phase 1 cached blob disagrees with a current snapshot's
declared hash) has its refcount-decrementing UPDATE extended to
maintain `blob.refcount_zeroed_at` per §7.5.1 rule 3. The
behavioral surface is unchanged: same error path, same eviction
semantics, same log line.

### 6.2 Cache miss: singleflight fetch

Unchanged structurally — see SPEC3 §6.2. The miss-path's
`PutBlob` insert has its INSERT extended to set
`refcount_zeroed_at = created_at` per §7.5.1 rule 1. No behavioral
surface change.

### 6.3 Resumable upstream fetch

Unchanged — see SPEC.md §6.3.

### 6.4 Cache miss with upstream down

Unchanged — see SPEC.md §6.4.

### 6.5 Hash validation

Unchanged — see SPEC2 §6.5 / SPEC3 §6.5.

### 6.6 Upstream allowlist

Unchanged — see SPEC.md §6.6.

---

## 7. Freshness and adoption

### 7.1 Triggers

Unchanged — see SPEC2 §7.1 / SPEC3 §7.1.

### 7.2 Algorithm

Unchanged — see SPEC2 §7.2 / SPEC3 §7.2.

### 7.3 Off the request path

Unchanged — see SPEC2 §7.3.

### 7.4 Periodic scheduler

Unchanged — see SPEC2 §7.4.

### 7.5 Adoption flow (Phase 4 deltas)

Phase 3's adoption flow (SPEC3 §7.5) carries forward unchanged at
the behavioral level. Phase 4 makes one mechanical change at the
SQL level: every refcount-mutating UPDATE also maintains
`blob.refcount_zeroed_at`. No new transaction steps; no new
serialization; no new failure modes.

#### 7.5.1 Refcount maintenance rules

There are exactly three SQL sites that mutate `blob.refcount`. Each
is extended to maintain `refcount_zeroed_at` per a fixed rule.

**Rule 1 — `PutBlob` (insert path,
`internal/cache/queries.go:159`).** New blob rows are born at
`refcount = 0`. Set `refcount_zeroed_at = created_at` on insert so
the grace clock starts at birth. A fetch that completes the blob
write but whose `url_path` insert never commits (handler dies,
connection drops mid-finalize) is then reaped one grace later —
never "reaped on the very next tick."

```sql
INSERT INTO blob (hash, size, created_at, refcount, refcount_zeroed_at)
VALUES (?, ?, ?, 0, ?)            -- both timestamps = now
ON CONFLICT(hash) DO NOTHING;
```

The `ON CONFLICT(hash) DO NOTHING` carries forward unchanged: a
blob already in the pool keeps its existing refcount and existing
`refcount_zeroed_at` — exactly the asymmetry documented in
`internal/cache/queries.go:152-158`.

**Rule 2 — `CommitAdoption` Step 4 (refcount + 1,
`internal/cache/adoption.go:349-358`).** When `refcount + 1`
crosses to strictly positive, clear `refcount_zeroed_at`.

```sql
UPDATE blob
   SET refcount = refcount + 1,
       refcount_zeroed_at = IIF(refcount + 1 > 0, NULL, refcount_zeroed_at)
 WHERE hash IN (SELECT blob_hash FROM snapshot_member WHERE snapshot_id = ?);
```

The `IIF` is required: a `-1` blob becoming `0` is still ≤ 0, and
the existing `refcount_zeroed_at` should be preserved so the grace
clock continues from where it was rather than restarting. Only the
strictly-positive crossing clears the column.

**Rule 3 — `CommitAdoption` Step 8 + `EvictURLPath` (refcount - 1,
`internal/cache/adoption.go:397-407` and
`internal/cache/adoption.go:771`).** When `refcount - 1` crosses to
≤ 0 *for the first time*, set `refcount_zeroed_at = now`.

```sql
UPDATE blob
   SET refcount = refcount - 1,
       refcount_zeroed_at = COALESCE(
         refcount_zeroed_at,
         IIF(refcount - 1 <= 0, ?, NULL)
       )
 WHERE hash IN (...);
```

`COALESCE` preserves an existing `refcount_zeroed_at` on a
`0 → -1` transition (the clock should continue, not restart). The
inner `IIF` only writes a new timestamp on the first ≤ 0
crossing.

#### 7.5.2 Atomic flip transaction (Phase 3 carry-forward)

The transaction structure of `CommitAdoption` is unchanged from
SPEC3 §7.5. Steps 1–7 carry forward exactly. Steps 4 and 8 use
the §7.5.1 SQL above. No changes to step ordering, transaction
boundaries, or commit semantics.

#### 7.5.3 Hot-set computation and remaining adoption sub-sections

Unchanged — see SPEC3 §7.5.2 (Packages parser additions),
§7.5.3 (hot-set computation), §7.5.4 (`package_coverage_complete`
semantics), and §7.5.5 (failure handling).

### 7.6 GPG verification

Unchanged — see SPEC2 §7.6.

---

## 8. Stale-and-Valid-Until

Unchanged — see SPEC.md §8.

---

## 9. Concurrency & deadlines

### 9.1 Per-request

Unchanged — see SPEC.md §9.1.

### 9.2 Singleflight

Unchanged — see SPEC.md §9.2.

### 9.3 Per-host concurrency on upstream

Unchanged — see SPEC.md §9.3.

### 9.4 SQLite concurrency

Unchanged structurally — see SPEC.md §9.4. Phase 4's GC writes go
through the same writer goroutine (`Cache.submitWrite`); GC reads
use the standard connection pool. The only delta is that the
writer's per-tick GC batches (§9.6) compete for the write lock
with adoption commits and request-path writes; the
`gc.batch_size = 100` default keeps each batch's lock-hold time in
the low milliseconds.

### 9.5 Graceful shutdown

Phase 3's shutdown sequence (SPEC2 §9.5 / SPEC3 §9.5) carries
forward. Phase 4 adds one new step:

```
... (Phase 3 steps 1–6) ...
6. Cancel any in-flight upstream fetches.
6a. (NEW) Cancel the GC goroutine via lifecycleCtx. The goroutine
    exits at the next per-batch boundary; in-flight transactions
    commit or roll back atomically, in-flight pool/ unlinks
    complete. A partial batch is fine: the next start picks up
    where it left off.
7. Stop the at-rest integrity scanner.
8. Flush SQLite.
9. Exit.
```

The 30s drain budget is unchanged. GC's worst-case lock-hold
inside one tick is `gc.batch_size = 100` row-deletes plus 100
`os.Remove` calls — well under 1s on a healthy fs. Cancelling
mid-tick has no correctness consequences.

### 9.6 Garbage collection (NEW)

The Phase 4 GC subsystem runs as a single dedicated goroutine,
started after `cache.Open` completes its startup sequence (§4.2)
and before listeners come up. It owns four reap classes — three
periodic, one startup-only.

#### 9.6.1 Goroutine lifecycle

```
on startup (in §4.2 step 5, blocking):
  1. Run blob GC pass    (§9.6.2)
  2. Run snapshot GC pass (§9.6.3)

then, every gc.interval:
  3. Run blob GC pass
  4. Run snapshot GC pass

on lifecycleCtx cancel:
  exit at next per-batch boundary
```

Order matters within a tick. Snapshot GC's row deletes (§9.6.3)
do *not* decrement blob refcounts — the `snapshot_member` rows
already had their bumps reversed at displacement time (Phase 2
`CommitAdoption` Step 8 for displaced snapshots; never bumped at
all for orphan candidates whose adoption failed before Step 4).
Running blob GC *first* on each tick is therefore not a
correctness requirement — it's an ordering convention chosen for
operational symmetry: the `gc_run_complete` line reports counts
from the same logical tick boundary regardless of which class
contributed which.

#### 9.6.2 Blob GC pass

Per pass, loop until either the candidate set empties or a
per-tick wall-clock budget trips (the budget is implicit: the
next ticker fire pre-empts via `select` on the channel):

```sql
-- SELECT (read-only, uses idx_blob_gc):
SELECT hash, size FROM blob
 WHERE refcount <= 0
   AND refcount_zeroed_at IS NOT NULL
   AND refcount_zeroed_at < :now - :grace_seconds
 ORDER BY refcount_zeroed_at
 LIMIT :batch_size;
```

```sql
-- DELETE (one transaction, includes the SELECT predicate to
-- defend against a parallel adoption that just bumped refcount
-- back above 0 between our SELECT and our DELETE):
BEGIN
  DELETE FROM blob
   WHERE hash IN (...candidate_hashes...)
     AND refcount <= 0
     AND refcount_zeroed_at IS NOT NULL
     AND refcount_zeroed_at < :now - :grace_seconds;
COMMIT
```

Then for each row that was actually deleted (the DELETE's
`RowsAffected` may be less than the SELECT's count if the race
above fired), the corresponding `pool/<hash>` file is unlinked.
Errors from `os.Remove` for `ErrNotExist` are swallowed (a
concurrent unlink is impossible — only GC unlinks `pool/` —
but a pre-existing crash-mid-state leaving the file already gone
is benign). Non-`ErrNotExist` errors increment a per-tick error
counter and are logged at Warn under the `gc_pool_unlink_failed`
event.

The `ORDER BY refcount_zeroed_at` makes the per-tick reap work
oldest-first — fairer under sustained backlog than batch-with-no-
order.

**Crash safety.** A crash between `COMMIT` and `os.Remove` leaves
`pool/<hash>` without a `blob` row — caught by the next startup's
§4.2 pool/ orphan scan. The reverse — DB row missing, file present
— is impossible from this code path (DELETE commits before
`os.Remove` runs). A crash before `COMMIT` rolls back atomically.

#### 9.6.3 Snapshot GC pass

Two sub-jobs share the same per-tick transaction. Both reap rows
that no `current_snapshot_id` references.

**Sub-job A — Orphan candidate snapshots.**

```sql
SELECT snapshot_id FROM suite_snapshot
 WHERE adopted_at IS NULL
   AND created_at < :now - :orphan_candidate_grace_seconds
   AND snapshot_id NOT IN (SELECT current_snapshot_id
                             FROM suite_freshness
                            WHERE current_snapshot_id IS NOT NULL);
```

The grace is `max(upstream.total_timeout × upstream.max_retries,
30m)` — derived from the runtime config, not a separate `[gc]`
key. The bound strictly exceeds any in-flight adoption's
worst-case wall-clock duration, so a candidate older than the
grace is provably abandoned.

**Sub-job B — Displaced snapshots beyond keep-N.**

```sql
WITH ranked AS (
  SELECT snapshot_id,
         canonical_scheme, canonical_host, suite_path,
         ROW_NUMBER() OVER (
           PARTITION BY canonical_scheme, canonical_host, suite_path
           ORDER BY adopted_at DESC
         ) AS rn
  FROM suite_snapshot
  WHERE adopted_at IS NOT NULL
)
SELECT snapshot_id FROM ranked
 WHERE rn > :keep_displaced
   AND snapshot_id NOT IN (SELECT current_snapshot_id
                             FROM suite_freshness
                            WHERE current_snapshot_id IS NOT NULL);
```

The `keep_displaced` value is `gc.keep_displaced` from config
(default 3). 0 means "no forensic retention" and reaps every
displaced snapshot on the next tick.

**Cascade DELETE for both sub-jobs** (one transaction per pass,
not one per sub-job — both lists are unioned and deleted in a
single commit):

```sql
BEGIN
  DELETE FROM snapshot_member WHERE snapshot_id IN (...);
  DELETE FROM package_hash    WHERE snapshot_id IN (...);
  DELETE FROM suite_snapshot  WHERE snapshot_id IN (...);
COMMIT
```

**No refcount accounting.** Orphan candidates failed before
`CommitAdoption` Step 4 ever ran, so their `snapshot_member` rows
never bumped any blob refcounts; deleting them does nothing
refcount-wise. Displaced snapshots had their refcounts decremented
at displacement time (Phase 2 Step 8); deleting their rows now
also does nothing refcount-wise — the bytes were already
accounted for.

This is why §9.6.2's blob GC and §9.6.3's snapshot GC are
independent: snapshot GC frees no bytes directly; it just
reclaims metadata rows.

#### 9.6.4 Pool/ orphan-file scan (startup-only)

Already specified in §4.2 step 4. Walks `pool/<two-hex-prefix>/<hash>`
directories, runs `SELECT 1 FROM blob WHERE hash = ?` for each
file, unlinks files with no matching row. Worker pool sized at
`gc.pool_scan_workers`. Cancellable.

The scan's filename → hash extraction also enforces the same
sha256-hex shape the schema CHECKs (`length = 64, [0-9a-f]
only`); a file in `pool/` whose name doesn't satisfy that shape
is suspicious enough that it triggers a `gc_pool_malformed_name`
Warn (§10) — the file is left alone (don't delete files we don't
recognize), but the operator is told.

---

## 10. Logging (deltas)

Phase 1 + Phase 2 + Phase 3 logging (SPEC §10 / SPEC2 §10 /
SPEC3 §10) carries forward exactly. Phase 4 adds:

### 10.1 Per-request line additions

None. GC runs entirely off the request path.

### 10.2 New structured events

- **`gc_run_complete`** Info, emitted at the end of each periodic
  tick *and* at the end of the startup pass. Fields:
  - `phase` — `"startup"` or `"periodic"`
  - `blobs_reaped` — count of blob rows DELETEd this run
  - `bytes_reclaimed` — sum of `blob.size` for those rows
  - `orphan_candidates_reaped` — count of `suite_snapshot` rows
    DELETEd via §9.6.3 sub-job A
  - `displaced_reaped` — count of `suite_snapshot` rows DELETEd
    via §9.6.3 sub-job B
  - `pool_orphans_repaired` — count of pool/ files unlinked by
    the §4.2 startup scan (zero on `phase=periodic`)
  - `pool_orphan_bytes_repaired` — corresponding byte count
  - `pool_unlink_errors` — count of unlink errors (other than
    `ErrNotExist`) encountered this run
  - `duration_ms` — wall-clock for the run

  The empty-tick case (`blobs_reaped=0 orphan_candidates_reaped=0
  displaced_reaped=0`) still emits the line: an operator scripting
  monitoring on "GC tick cadence" can use the line as a heartbeat.

- **`gc_disabled`** Warn at startup when `gc.enabled = false`. The
  cache still works, but disk usage will grow unbounded as
  adoptions roll. Parallel to `refuse_unvouched_debs_inert` in
  SPEC3 §10.

- **`gc_pool_unlink_failed`** Warn when `os.Remove` on a reaped
  blob's `pool/<hash>` file returns a non-`ErrNotExist` error.
  Fields: `hash`, `err`, `operation` (`reap` or `pool_scan`).
  Common causes: filesystem permission change mid-run, fs read-only
  remount. The DB row was already DELETEd; the file is leaked
  until the next startup pool scan reaps it.

- **`gc_pool_malformed_name`** Warn when the §9.6.4 startup scan
  encounters a file under `pool/` whose name doesn't satisfy the
  sha256-hex CHECK shape. Fields: `path` (relative to `pool/`).
  The file is left in place; the operator decides what to do.

### 10.3 Startup config dump (additions)

Phase 1's startup config dump (Phase 1 §10.3 carry-forward) adds
the `[gc]` block (all six keys) verbatim, with one synthesized
field:

- `gc.orphan_candidate_grace_effective` — the runtime-derived
  grace
  `max(upstream.total_timeout × upstream.max_retries, 30m)` so the
  operator can read the actual value used by §9.6.3 sub-job A
  without computing it themselves.

---

## 11. Failure-mode catalog (deltas)

Phase 1 + Phase 2 + Phase 3 catalog (SPEC §11 / SPEC2 §11 /
SPEC3 §11) carries forward exactly. Phase 4 adds:

| Condition | Behavior |
|---|---|
| GC SELECT finds candidates, DELETE commits, process killed before `os.Remove` | `pool/<hash>` file orphans. Caught by next startup §4.2 scan. No correctness impact; minor disk leak bounded by *time to next restart*. |
| GC SELECT finds candidates, parallel adoption bumps refcount on one before DELETE runs | The DELETE's `refcount <= 0` predicate filters that row out; `RowsAffected` is less than the SELECT's count; only actually-deleted rows get unlinked. Correct. |
| `os.Remove` on reaped blob fails with EPERM / EROFS | `gc_pool_unlink_failed` Warn. DB row already DELETEd. File leaks until next startup scan. The operator-visible signal is the Warn line plus a non-zero `pool_unlink_errors` field on the next `gc_run_complete`. |
| Orphan candidate snapshot whose adoption hung past the grace, then completes after GC reaped it | `CommitAdoption`'s final `INSERT INTO snapshot_member` would fail FK on the now-deleted candidate row. The adoption transaction rolls back; the bytes already in `pool/` keep their refcount and become reapable on the next pass. The adoption goroutine logs `adoption_run_failed` Warn (Phase 2 §10.2 sentinel). The grace bound `max(total_timeout × max_retries, 30m)` exists precisely to make this race vanishingly rare. |
| Pool/ scan worker fails to read a `pool/<prefix>/` directory | Per-worker error; logged at Warn under a generic `pool_scan_dir_failed` event with `prefix` and `err`. The scan continues with other prefixes. |
| Migration v3 → v4 interrupted | Tx rolls back; next start retries from `schema_version = 3`. |

---

## 12. Test strategy (deltas)

Phase 1 + Phase 2 + Phase 3 test strategy (SPEC §12 / SPEC2 §12 /
SPEC3 §12) carries forward exactly. Phase 4 adds:

### 12.1 Unit tests (additions)

- **`refcount_zeroed_at` maintenance, all three rules.** Goldens
  for `PutBlob` (sets to `created_at`), `CommitAdoption` Step 4
  (clears on transition to >0, preserves on -1→0), Step 8 +
  `EvictURLPath` (sets on first ≤0 crossing, preserves on 0→-1).
  Each rule has at least one golden for each transition.
- **GC reap predicate.** `WHERE refcount <= 0 AND
  refcount_zeroed_at < now - grace` returns the right candidate
  set across:
  - refcount=0, zeroed_at = now-grace+1s — excluded
  - refcount=0, zeroed_at = now-grace-1s — included
  - refcount=0, zeroed_at IS NULL — excluded (legacy guard)
  - refcount=-1, zeroed_at = now-grace-1s — included
  - refcount=1, zeroed_at = now-grace-1s — excluded
- **Snapshot GC SELECTs.** Goldens for the orphan-candidate query
  (correct grace arithmetic from runtime config) and the
  displaced-snapshot query (ROW_NUMBER per-suite partition,
  keep_displaced = 0/1/3, current_snapshot_id correctly excluded).
- **Migration v3 → v4.** Apply against a Phase 3 snapshot, verify
  schema; idempotent re-apply is a no-op; an interrupted migration
  rolls back cleanly; the backfill UPDATE correctly populates
  `refcount_zeroed_at = created_at` for pre-v4 ≤0 rows.
- **Config IsDefined.** Not applicable — Phase 4 introduces no
  presence-sensitive duration keys (the `[gc]` keys all have safe
  defaults that don't collide with `0`).

### 12.2 Integration tests (additions)

- **GC end-to-end.** Run a synthetic adoption that displaces a
  prior snapshot; assert prior-snapshot blobs become refcount=0
  with `refcount_zeroed_at` set; advance the test clock past
  `gc.blob_grace`; trigger a GC tick; assert blobs are gone from
  both `blob` and `pool/`. Repeat with a hot blob that survives
  (refcount > 0 throughout).
- **Pool/ orphan scan.** Pre-populate `pool/<prefix>/<hash>` files
  whose hashes have no `blob` row; restart the process; assert
  the files are unlinked and `gc_run_complete` startup line names
  the right `pool_orphans_repaired` count.
- **Forensic retention.** Adopt 5 snapshots in sequence on the
  same suite with `gc.keep_displaced = 3`; advance the clock; run
  GC; assert exactly 3 displaced snapshots remain (the 3 most
  recent by `adopted_at`) plus the 1 current snapshot.

### 12.3 Phase 4 chaos test: GC + adoption race (the gate)

Property: a blob whose refcount is bumped by an in-flight
adoption is *never* reaped, even if the adoption's bump arrives
after GC's SELECT but before GC's DELETE. The §9.6.2 DELETE's
`refcount <= 0` predicate is the gate.

Driver:

1. Set up state where blob `B` has refcount=0 and
   `refcount_zeroed_at < now - grace`.
2. Block GC's DELETE statement at a fault-injection point (after
   SELECT, before transaction begin).
3. Run a parallel adoption that bumps `B.refcount` to 1 (this
   commits a transaction that updates `B` per §7.5.1 rule 2).
4. Release GC's DELETE.
5. Assert `B`'s row still exists.
6. Assert `pool/<B>` still exists on disk.

Inverted property test: same scaffolding, but the parallel
adoption *aborts* (the goroutine cancels before `CommitAdoption`'s
commit). Assert that no bump landed, and `B` is reaped per the
normal reap path.

Same 10-consecutive-runs gate as Phase 2 / Phase 3 chaos tests.

### 12.4 Phase 4 chaos test: GC + EvictURLPath race

Property: a blob whose refcount transitions `1 → 0 → -1`
(adoption decrement then §6.1 hit-path eviction decrement) is
reaped at the next eligible tick, with the grace clock counted
from the `1 → 0` transition (not restarted by the `0 → -1`
transition).

Driver verifies the `COALESCE` semantics of §7.5.1 rule 3 are
correct: the `0 → -1` UPDATE preserves the existing
`refcount_zeroed_at`.

10-consecutive-runs gate.

### 12.5 Phase 4 chaos test: GC + concurrent snapshot displacement

Property: a snapshot displaced *during* a GC tick is not reaped
*this* tick (its rows are not in §9.6.3's SELECT result yet —
the SELECT ran before the displacement transaction committed).
On the *next* tick, the displaced rows are eligible (assuming the
keep-N window does not cover them). The
`current_snapshot_id NOT IN` clause is the guarantee.

10-consecutive-runs gate.

### 12.6 Phase 4 chaos test: crash mid-batch

Property: process killed between §9.6.2's `COMMIT` and `os.Remove`
leaves a `pool/` orphan that the *next* startup repairs.

Driver: kill -9 the process at a fault-injection point; assert
pool size on disk after restart equals what's in the `blob`
table; assert `gc_run_complete` startup line names a non-zero
`pool_orphans_repaired`.

10-consecutive-runs gate.

### 12.7 v3 → v4 migration end-to-end *(deliberately skipped)*

This test is **not implemented and will not be implemented**, by
parallel to SPEC3 §12.5's posture on v2 → v3. The migration code
path (`migrations[3]` in `internal/cache/schema.go`) and its unit
tests (§12.1) remain; only the end-to-end "old binary, new binary,
same `cache_dir`" harness is scoped out. The sole pre-release
deployment will drop and recreate the cache directory at the
v3 → v4 boundary.

If a future deployment ever needs in-place upgrade from a v3
cache directory, this section is the spec for the test that
should be written first.

### 12.8 Soak (manual / nightly)

Phase 1 + Phase 2 + Phase 3 soak extends to: assert no leak in
`pool/` over 24h of rolling adoptions with GC enabled (the
`gc_run_complete` periodic events show non-zero `bytes_reclaimed`
on adoption-displacing ticks); assert bounded `blob` row count;
assert `idx_blob_gc` partial index size remains tiny relative to
the table (steady-state reap keeps the candidate set small).

---

## 13. Project layout & tooling (deltas)

Phase 1 + Phase 2 + Phase 3 layout carries forward. Phase 4 adds
one new package and modifies two existing ones:

```
internal/
  gc/                    # NEW package
    gc.go                # the goroutine + tick loop + orchestration
    blob.go              # §9.6.2 blob GC pass
    snapshot.go          # §9.6.3 snapshot GC pass
    pool_scan.go         # §9.6.4 startup pool/ orphan scan
    gc_test.go           # unit tests for §12.1
    chaos_test.go        # chaos tests §12.3–§12.6
  cache/
    schema.go            # migrations[3] (v3 → v4) appended;
                         # CurrentSchemaVersion bumped to 4
    queries.go           # PutBlob INSERT extended (§7.5.1 rule 1)
    adoption.go          # CommitAdoption Step 4 + Step 8 SQL
                         # extended (§7.5.1 rules 2 + 3);
                         # EvictURLPath SQL extended (rule 3)
  config/
    config.go            # [gc] block decoder + validation
```

`go.mod` additions: none. The pure-Go `database/sql` + SQLite
driver path covers everything Phase 4 needs.

CI jobs from earlier phases carry forward. The `go test -race ./...`
job now includes the §12.3–§12.6 chaos tests. The `e2e/` job does
not gain a new test (the v3 → v4 migration end-to-end test is
deliberately not part of CI; see §12.7 for rationale).

---

## 14. Definition of done

Phase 4 is done when:

1. Every Phase 1 chaos test (SPEC §12.3), Phase 2 chaos tests
   (SPEC2 §12.3, §12.4), and Phase 3 chaos tests (SPEC3 §12.3,
   §12.4) continue to pass — Phase 4 must not regress prior
   behavior.
2. The Phase 4 GC-vs-adoption chaos test (§12.3) passes 10
   consecutive runs with no flakes, including both the bump-wins
   and adoption-aborts variants.
3. The Phase 4 GC-vs-EvictURLPath chaos test (§12.4) passes 10
   consecutive runs.
4. The Phase 4 GC-vs-displacement chaos test (§12.5) passes 10
   consecutive runs.
5. The Phase 4 crash-mid-batch chaos test (§12.6) passes 10
   consecutive runs.
6. *(Deliberately dropped.)* The `v3 → v4` migration end-to-end
   test (§12.7) is **not** required for Phase 4 done. The
   migration code path itself remains in
   `internal/cache/schema.go` and its per-step semantics are
   covered by unit tests; the integration harness is scoped out
   because the only known v3 deployment is the pre-release one
   whose operator will drop and re-create the cache directory at
   the v3 → v4 boundary. See §12.7.
7. The cache is deployed to one production environment with
   `gc.enabled = true` and default `gc.*` values for at least one
   week. Monitoring shows:
   - `gc_run_complete` events at expected periodic cadence;
   - cumulative `bytes_reclaimed > 0` after a week of normal
     traffic with rolling adoptions (proves the loop reclaims
     real bytes, not just empties an empty queue);
   - bounded `blob` table row count over the week (does not grow
     unboundedly with time);
   - bounded `pool/` byte count;
   - GC goroutine drains cleanly on graceful shutdown (no leaked
     `pool/` orphans across the next-startup scan beyond the
     known crash-recovery cases);
   - no observed `gc_disabled` Warn in the journal (i.e. the
     operator did not opt out unintentionally);
   - `gc_pool_unlink_failed` and `gc_pool_malformed_name` Warns,
     if any, name specific paths and motivate operator
     investigation.
8. SPEC4.md reflects the as-built reality (this document is
   updated as we go, not just before).
