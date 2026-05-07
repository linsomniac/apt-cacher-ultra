# apt-cacher-ultra — Phase 4 Scoping

Status: **scoping in progress** (revision 1).
Last updated 2026-05-06. Next artifact: `SPEC4.md` modeled on
`SPEC3.md`'s structure, once any further review feedback has been
incorporated.

This document gathers what Phase 4 is, the hooks Phases 1, 2, and 3
left in place for it, and the design decisions resolved during this
scoping pass before this becomes a locked SPEC4.md (parallel to
SPEC.md / SPEC2.md / SPEC3.md). Companion documents
[PHASE-2-SCOPING.md](PHASE-2-SCOPING.md) and
[PHASE-3-SCOPING.md](PHASE-3-SCOPING.md) record the parallel
exercises for earlier phases.

---

## 1. Goals

Phase 1 made the cache-hit path bulletproof. Phase 2 closed the
integrity and freshness loops with atomic adoption, GPG verification,
and per-member hash validation. Phase 3 closed the service-continuity
loop with hot-package proactive refresh and opt-in strict mode.
Phase 4 closes the **storage-reclamation loop**:

1. **Reap unreferenced `pool/` blobs.** The snapshot model in §4
   produces orphans by design — every adoption that displaces a prior
   snapshot decrements the prior's blob refcounts (Phase 2
   `CommitAdoption` step 8), and `EvictURLPath` decrements when a
   §6.1 hit-path eviction races a snapshot transition. Refcount math
   has been correct since Phase 2; nothing has been sweeping. Phase 4
   sweeps.

2. **Reap orphan candidate snapshots.** Phase 2 §7.5.2 documents that
   failed adoptions leave `suite_snapshot` rows with
   `adopted_at IS NULL` as "harmless residue" awaiting Phase 4 GC.
   Same for adoptions cancelled by graceful shutdown (SPEC2 §9.5
   step 5). Without GC these accumulate one row per failed/cancelled
   adoption per suite, indefinitely.

3. **Reap displaced snapshots beyond a small forensic retention.**
   When a `current_snapshot_id` flip displaces a prior snapshot, the
   refcount math already accounted for the bytes; the rows themselves
   (`suite_snapshot`, `snapshot_member`, `package_hash`) are
   unreferenced bookkeeping. Hold the **N most recent displaced
   snapshots per suite** for forensic SELECT-after-bad-rollout, reap
   the rest. (Default `keep_displaced = 3`.)

4. **Repair `pool/` orphan files at startup.** A `pool/<hash>` file
   without a matching `blob` row indicates a prior crash mid-rename
   or mid-DELETE. Belt-and-suspenders sweep at startup only; not on
   the periodic ticker (the cost is O(pool size), unsuitable for
   per-hour cadence).

The four jobs are independent, but share a single `[gc]`
configuration block, a single periodic goroutine, and a single
log-event family.

### 1.1 Non-goals (deferred)

Carried forward from earlier phases unchanged:

- Status page / `/metrics` endpoint (Phase 5).
- TLS MITM listener (Phase 6).
- Source-package caching, multi-arch beyond amd64, pdiff (Phase 6+).
- Streaming-while-fetching as a singleflight optimization. Deferred
  to [FUTURE-REVIEW.md §1](FUTURE-REVIEW.md).
- Per-byte upstream read timeouts. Deferred to
  [FUTURE-REVIEW.md §1](FUTURE-REVIEW.md).
- Per-suite freshness cadence variation. Deferred to
  [FUTURE-REVIEW.md §2](FUTURE-REVIEW.md).
- Operator-triggered manual GC (admin endpoint or SIGUSR1).
  Achievable as a Phase 4 add-on if needed but not part of the
  gating contract; the periodic ticker plus a startup pass is
  sufficient.

### 1.2 Resolved during Phase 4 scoping

The eight design questions below were resolved with the operator
during scoping. Each resolution is normative for SPEC4.

- **Grace period clock.** Starts at the *refcount reaches 0
  transition*, not at `blob.created_at`. A new column
  `blob.refcount_zeroed_at` records the timestamp; reaping requires
  `refcount <= 0 AND refcount_zeroed_at IS NOT NULL AND
  refcount_zeroed_at < now - gc.blob_grace`. Default grace 5m.
  Rationale: a freshly-created blob held momentarily at refcount=0
  before its referencing url_path row commits is no different from a
  legitimately-orphaned blob *already 24 hours old* — the right
  signal is "how long has this been unreferenced," not "how long has
  this existed." (See §3.2.1.)
- **Pacing.** Periodic ticker every `gc.interval` (default `1h`),
  per-tick batch of `gc.batch_size` blobs (default `500`). Plus a
  one-shot pass on startup, post-cleanup, before listeners come up.
- **Pool scan.** Startup-only, once. No periodic re-scan.
- **Orphan-candidate-snapshot grace.**
  `max(upstream.total_timeout × upstream.max_retries, 30m)`. The
  arithmetic bound covers the worst case for an in-flight adoption
  that is genuinely still working.
- **Displaced-snapshot retention.** Keep the `gc.keep_displaced`
  most recent displaced snapshots per `(canonical_scheme,
  canonical_host, suite_path)`. Default `3`.
- **Schema migration.** A new column on `blob` plus a partial index
  is required. Migration v3 → v4 written for shape-consistency with
  prior migrations, but not gated as a DoD requirement on a
  real-world v3 deployment — the sole pre-release deployment will
  drop and recreate the cache directory at the v3 → v4 boundary.
  (Same posture as v2 → v3.)
- **Default-on.** `gc.enabled = true` ships as the default. The
  conservative grace + batched cadence + startup-pass design makes
  the feature safe-by-default; opting *out* requires the same level
  of operator awareness as opting *in* to a feature with a real
  failure mode.
- **Metrics events.** `gc_run_complete` Info per pass with the
  fields enumerated in §3.5. Symmetric with `adoption_*` events; no
  new format.

---

## 2. What Phases 1, 2, and 3 already left in place

Walking the existing code, prior phases deliberately seeded these
hooks that Phase 4 builds on:

| Prior-phase hook | Phase 4 use |
|---|---|
| `blob.refcount` column with `DEFAULT 0` (Phase 1, `internal/cache/schema.go:39`) | The reap predicate's first clause. SPEC §4.3 explicitly notes "populated for Phase 4 GC." |
| `CommitAdoption` Step 4 (`internal/cache/adoption.go:349-358`): bumps refcount for every distinct blob in the new snapshot | The increment side of the bookkeeping is already correct. Phase 4 only reads it. |
| `CommitAdoption` Step 8 (`internal/cache/adoption.go:397-407`): decrements refcount for the prior snapshot's distinct blobs | The decrement side; same as above. The Phase 4 column `refcount_zeroed_at` will be maintained alongside this UPDATE. |
| `EvictURLPath` (`internal/cache/adoption.go:771`): decrements refcount on §6.1 hit-path eviction | Third write site that maintains `refcount_zeroed_at`. Already documented (`adoption.go:725-727`) that transient `refcount = -1` is possible from race-with-flip — `<= 0` is the correct sweep predicate. |
| `PutBlob`'s `INSERT ... DEFAULT 0` semantics (`internal/cache/queries.go:159-175`) | New blobs are born at refcount=0 with no references yet. Phase 4 sets `refcount_zeroed_at = created_at` on insert so the grace clock starts at birth (otherwise a fetch that completes but whose url_path insert dies before commit would be reaped on the next GC tick — wrong). |
| `tmp/` and `staging/` 5-minute mtime sweep (Phase 1 SPEC §4.2 / Phase 2 SPEC2 §4.2) | Already running; Phase 4 inherits unchanged. The `pool/` orphan-file scan is *new* — `tmp/`/`staging/` cover in-flight artifacts; `pool/` covers committed-but-unreferenced files. |
| Single-writer SQLite model (SPEC §9.4) and the `c.submitWrite` channel (`internal/cache/cache.go`) | GC writes go through the same writer goroutine. No new lock, no new contention. Reads use the existing connection pool. |
| `suite_snapshot.adopted_at` (Phase 2; `NULL` while candidate, set at flip) | The discriminator between orphan candidate (`adopted_at IS NULL`) and displaced (`adopted_at IS NOT NULL`). Phase 4 reaps both classes by different predicates. |
| `suite_freshness.current_snapshot_id` (Phase 2) | The "is this snapshot current?" query. Phase 4 GC's snapshot-reap predicate joins on `NOT IN (SELECT current_snapshot_id FROM suite_freshness ...)`. |
| `lifecycleCtx` graceful-shutdown wiring (SPEC §9.5 / SPEC2 §9.5) | GC goroutine respects ctx cancel and exits cleanly mid-batch. A partial batch is fine — the next start picks up where it left off. |
| Phase 2/3 chaos-test harness (`phase2_chaos_test.go`, `phase3_chaos_test.go`) | Phase 4's chaos tests slot in alongside as `phase4_chaos_test.go`. |

Phase 4 is **additive over Phase 3** at the schema level (one new
column, one new index) and **additive at the operational level**
(new periodic goroutine, no changes to existing request paths,
adoption paths, or freshness paths). The wire contracts (SPEC §2),
URL canonicalization (SPEC §3), per-host concurrency (SPEC §9.3),
graceful shutdown semantics (SPEC §9.5), the snapshot model
(SPEC2 §4), and the hot-package model (SPEC3 §7.5) all carry
forward unchanged.

---

## 3. Architectural sketch

### 3.1 Reaping cycle

A single goroutine, started after Phase 1's startup-time `tmp/`
sweep and Phase 2's `staging/` sweep complete, drives all four
reap classes:

```
on startup:
  1. pool/ orphan-file scan          (§3.4, one-shot)
  2. blob GC pass                    (§3.2)
  3. snapshot GC pass                (§3.3)

every gc.interval:
  4. blob GC pass
  5. snapshot GC pass
```

Order matters within a tick. Snapshot GC's row deletes (§3.3)
decrement blob refcounts on the newly-orphaned `snapshot_member` and
`package_hash` references; running blob GC *first* on each tick
means those just-decremented blobs wait one full tick before the
grace can elapse. That's correct — they may have been decremented
within the last second; the grace clock should run forward from
*this* tick, not from the last tick.

The goroutine respects `lifecycleCtx`. A partial batch on cancel is
fine: each batched DELETE is its own transaction; row-deletes that
already committed reduce the work for the next start; row-deletes
that hadn't committed roll back atomically.

### 3.2 Blob GC

#### 3.2.1 The "since refcount reached 0" grace clock

The reap predicate is:

```sql
SELECT hash, size FROM blob
WHERE refcount <= 0
  AND refcount_zeroed_at IS NOT NULL
  AND refcount_zeroed_at < :now - :grace_seconds
ORDER BY refcount_zeroed_at
LIMIT :batch_size;
```

The `<= 0` (not `= 0`) is load-bearing: SPEC2 §6.1 step 5 documents
that a transient `refcount = -1` is reachable when an adoption
transaction's decrement races a §6.1 hit-path eviction's decrement.
Both decrements are correct per their own bookkeeping; the row
should still be reaped.

The `refcount_zeroed_at IS NOT NULL` clause is belt-and-suspenders:
the column should always be populated when refcount ≤ 0, but the
explicit check guards against a hypothetical future code path that
decrements refcount without maintaining the column.

The `ORDER BY refcount_zeroed_at` makes the per-tick reap work
oldest-first — fairer under sustained load than batch-with-no-order.

#### 3.2.2 Schema delta — `blob.refcount_zeroed_at`

```sql
ALTER TABLE blob ADD COLUMN refcount_zeroed_at INTEGER;

CREATE INDEX idx_blob_gc
  ON blob(refcount_zeroed_at)
  WHERE refcount <= 0;
```

The partial index keeps the index small (it covers only the
candidate set) and makes the §3.2.1 query index-only.

Maintenance rules at the three existing write sites:

1. **`PutBlob` (insert path).** New blob is born at refcount=0;
   set `refcount_zeroed_at = now` so the grace clock starts at
   birth. A fetch that completes the blob write but whose url_path
   insert never commits (handler dies, connection drops) is then
   reaped one grace later — never "reaped on the very next tick."

2. **`CommitAdoption` Step 4 (refcount + 1).** When `refcount + 1`
   crosses to strictly positive, clear `refcount_zeroed_at`. SQL:
   ```sql
   UPDATE blob
      SET refcount = refcount + 1,
          refcount_zeroed_at = IIF(refcount + 1 > 0, NULL, refcount_zeroed_at)
    WHERE hash IN (SELECT blob_hash FROM snapshot_member WHERE snapshot_id = ?);
   ```
   The IIF is required: if a `-1` blob becomes `0` (still ≤ 0),
   leave the existing `refcount_zeroed_at` so the grace clock
   continues from where it was, rather than restarting.

3. **`CommitAdoption` Step 8 + `EvictURLPath` (refcount - 1).**
   When `refcount - 1` crosses to ≤ 0 *for the first time*, set
   `refcount_zeroed_at = now`. SQL:
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
   `0 → -1` transition (the clock should continue, not restart).
   The IIF inside `COALESCE` only sets the column on the first ≤ 0
   crossing.

#### 3.2.3 Reap algorithm

Per tick:

```go
for {
    candidates := SELECT hash, size ... LIMIT batch_size      // §3.2.1
    if len(candidates) == 0 { break }

    // Single transaction: delete all DB rows together.
    BEGIN
      DELETE FROM blob
       WHERE hash IN (...candidates.hash...)
         AND refcount <= 0
         AND refcount_zeroed_at < :now - :grace
    COMMIT

    // Then unlink files for the rows that were actually deleted.
    // RowsAffected may be less than len(candidates) if a concurrent
    // adoption bumped refcount between SELECT and DELETE — that
    // blob stays. Unlink only the survivors of the DELETE.
    for each (hash, size) actually deleted:
        os.Remove(pool/<hash>)
        gc.bytes_reclaimed += size

    if reached(per-tick limit) { break }
}
```

The DELETE is parametrized on the same `refcount <= 0 AND
refcount_zeroed_at < ?` predicate as the SELECT to defend against
the race window — a parallel adoption can bump refcount and clear
`refcount_zeroed_at` between our SELECT and our DELETE, and the
DELETE's WHERE filters that case out.

A concurrent unlink (impossible — only GC unlinks `pool/`) would be
moot anyway.

**Crash safety.** A crash between DELETE-commit and `os.Remove`
leaves a `pool/<hash>` file with no `blob` row — a `pool/` orphan,
caught by the startup scan (§3.4). The reverse — DB row missing,
file present — is safe: the next request for that path re-fetches
and re-validates; the file is shadow-overwritten by `os.Rename`
landing on the same destination (Phase 1 already handles this).

A crash between SELECT and DELETE commits nothing. No state changes.

### 3.3 Snapshot GC

#### 3.3.1 Orphan candidate snapshots

A `suite_snapshot` row is an *orphan candidate* when:

```sql
adopted_at IS NULL
AND created_at < now - :orphan_candidate_grace
AND snapshot_id NOT IN (SELECT current_snapshot_id
                          FROM suite_freshness
                         WHERE current_snapshot_id IS NOT NULL)
```

The grace is
`max(upstream.total_timeout × upstream.max_retries, 30m)` so an
in-flight adoption that legitimately takes the full retry budget is
never mid-flight reaped.

Reap order:

```
BEGIN
  DELETE FROM snapshot_member WHERE snapshot_id IN (...orphans...);
  DELETE FROM package_hash    WHERE snapshot_id IN (...orphans...);
  DELETE FROM suite_snapshot  WHERE snapshot_id IN (...orphans...);
COMMIT
```

Note: candidate snapshots that *failed before* `CommitAdoption`
never bumped any refcounts (Step 4 runs *inside* the transaction
that flips the pointer), so deleting their `snapshot_member` /
`package_hash` rows does not require any refcount accounting. The
referenced blobs were already accounted-for by `PutBlob`'s
refcount=0 default and the §3.2 grace clock.

#### 3.3.2 Displaced snapshots

A `suite_snapshot` row is *displaced* when:

```sql
adopted_at IS NOT NULL
AND snapshot_id NOT IN (SELECT current_snapshot_id
                          FROM suite_freshness
                         WHERE current_snapshot_id IS NOT NULL)
```

Of those, the **`gc.keep_displaced` most recent per suite** are
preserved for forensic inspection. The exact query (one batch per
suite, then DELETE outside the keep-set):

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
   AND snapshot_id NOT IN (SELECT current_snapshot_id FROM suite_freshness
                            WHERE current_snapshot_id IS NOT NULL);
```

The displaced snapshot's `snapshot_member` rows already had their
blobs decremented at displacement time (Phase 2 `CommitAdoption`
Step 8), so the §3.3.1 cascade DELETE is enough — no additional
refcount math.

DELETE order is the same as §3.3.1.

### 3.4 Pool/ orphan-file scan (startup-only)

Walks `pool/<two-hex-prefix>/<hash>` directories on startup.
For each file, runs:

```sql
SELECT 1 FROM blob WHERE hash = ?
```

If absent, unlinks the file and accumulates the byte count into
`gc_pool_orphans_repaired`. Rate-limited via a worker pool sized at
`gc.pool_scan_workers` (default 4, parallel to
`integrity.validate_at_rest_workers`). Cancellable via
`lifecycleCtx`.

The scan is intentionally one-shot per process: walking a multi-GiB
pool every hour is wasteful, and the only way an orphan file is
created is a crash between §3.2.3's DELETE and `os.Remove` — a rare
event whose latency-to-cleanup is bounded by *time to next process
restart*, not *time to next GC tick*.

### 3.5 Operator controls and observability

Configuration block:

```toml
[gc]
enabled               = true     # master switch
interval              = "1h"     # periodic cadence
batch_size            = 500      # blobs reaped per tick
blob_grace            = "5m"     # since refcount reached 0
keep_displaced        = 3        # per-suite forensic retention
pool_scan_workers     = 4        # startup-only pool/ scan parallelism
```

Single log event family:

- **`gc_run_complete`** Info, emitted at the end of each tick *and*
  at the end of the startup pass (with `phase=startup` discriminator
  for the latter). Fields:
  - `blobs_reaped` — count of blob rows DELETEd this run
  - `bytes_reclaimed` — sum of `blob.size` for those rows
  - `snapshots_reaped` — count of `suite_snapshot` rows DELETEd
    (sum of orphan + displaced; structured into separate fields
    `orphan_candidates_reaped` and `displaced_reaped`)
  - `pool_orphans_repaired` — count of orphan files unlinked
    (zero except on `phase=startup` runs)
  - `pool_orphan_bytes_repaired` — corresponding byte count
  - `duration_ms` — wall-clock for the run
  - `phase` — `periodic` or `startup`

One conditional log event:

- **`gc_disabled`** Warn at startup when `gc.enabled = false`. The
  cache still works, but disk usage will grow unbounded as
  adoptions roll. The warning is the only operational signal that
  the operator made a deliberate choice (parallel to
  `refuse_unvouched_debs_inert` in SPEC3 §10).

---

## 4. Schema migration v3 → v4

### 4.1 Migration

Pure additive DDL:

```sql
ALTER TABLE blob ADD COLUMN refcount_zeroed_at INTEGER;

UPDATE blob SET refcount_zeroed_at = created_at
 WHERE refcount <= 0;
-- Rationale: pre-v4 rows at refcount<=0 have an unknown actual
-- transition time. Setting it to created_at is the conservative
-- choice — it grants the maximum possible grace (the row has
-- existed since created_at; if it's been ≤ 0 the whole time, the
-- grace has already long elapsed and the next GC tick reaps; if
-- it transitioned to ≤ 0 yesterday and we attribute the transition
-- to created_at, we still reap correctly, just one grace too soon
-- rather than too late). This is a one-time pre-Phase-4 backlog
-- drain; steady state has the column maintained by §3.2.2.

CREATE INDEX idx_blob_gc
  ON blob(refcount_zeroed_at)
  WHERE refcount <= 0;
```

Idempotent. Forward-only. Per-step semantics unit-tested; the
end-to-end "v3 binary, v4 binary, same `cache_dir`" harness is
**not** required for Phase 4 done — same posture as v2 → v3 in
SPEC3 §12.5. The sole pre-release deployment will drop and recreate
the cache directory.

### 4.2 New configuration keys

See §3.5. All under a new `[gc]` block. None are presence-sensitive
(no key carries documented `0`-means-something semantics that would
collide with `Defaults()`); standard `Defaults()`-then-`Decode`
loader pattern suffices.

---

## 5. Chaos tests for Phase 4 (the new gates)

### 5.1 GC + adoption race

Property: a blob whose refcount is bumped by an in-flight adoption
is *never* reaped, even if the adoption's bump arrives after GC's
SELECT. The §3.2.3 DELETE's `refcount <= 0` predicate is the gate.

Driver:
1. Set up state where blob `B` has refcount=0 and
   `refcount_zeroed_at < now - grace`.
2. Block GC's SELECT-then-DELETE between the two with a hook.
3. Run a parallel adoption that bumps `B.refcount` to 1.
4. Release GC's DELETE.
5. Assert `B` survives.

Inverted property test: same scaffolding, but the adoption *aborts*
(the goroutine cancels before `CommitAdoption`'s commit). Assert
that the adoption never bumped, and `B` is reaped.

### 5.2 GC + EvictURLPath race

Property: a blob whose refcount transitions `1 → 0 → -1` (adoption
decrement then §6.1 hit-path eviction decrement) is reaped at the
next eligible tick, with the grace clock counted from the `1 → 0`
transition (not restarted by the `0 → -1` transition).

Driver verifies the `COALESCE` semantics of §3.2.2 rule 3 are
correct: the `0 → -1` UPDATE preserves the existing
`refcount_zeroed_at`.

### 5.3 GC + concurrent snapshot displacement

Property: a snapshot displaced *during* a GC tick is not reaped
*this* tick — its rows are not in the §3.3.2 SELECT result yet.
Next tick, the displaced rows are eligible (assuming the keep-N
window does not cover them). The `current_snapshot_id NOT IN`
clause is the guarantee.

### 5.4 Crash mid-batch

Property: process killed between §3.2.3's `COMMIT` and `os.Remove`
leaves a pool/ orphan that the *next* startup repairs.

Driver: kill -9 the process at a fault-injection point;
assert pool size on disk after restart equals what's in the `blob`
table; assert `gc_run_complete` startup line names a non-zero
`pool_orphans_repaired`.

10 consecutive runs gates each chaos test, mirroring the Phase 3
DoD pattern.

---

## 6. Recommended sequencing

The four reap classes are independent. Suggested implementation
order:

1. **Schema migration v3 → v4** (`migrations[3]` in
   `internal/cache/schema.go`); bump `CurrentSchemaVersion = 4`.
2. **`refcount_zeroed_at` maintenance** at the three write sites
   (PutBlob, CommitAdoption Steps 4 and 8, EvictURLPath). This is
   the load-bearing change — every other Phase 4 piece reads what
   these three write.
3. **Blob GC** (§3.2). Goroutine, periodic ticker, batched
   DELETE/unlink. Ship and observe in isolation before adding
   snapshot GC.
4. **Snapshot GC** (§3.3). Orphan-candidate and displaced-snapshot
   sweeps. Ship together — they share the snapshot-row DELETE
   helper.
5. **Pool/ orphan-file scan** (§3.4). Startup-only. Sequenced after
   Phase 1 `tmp/` and Phase 2 `staging/` sweeps in `cache.Open`.
6. **Chaos tests** (§5). Land alongside §3.2 and §3.3 respectively.
7. **Config plumbing** (§3.5) and `gc_run_complete` /
   `gc_disabled` log events. These are scaffolding — written first
   in the actual implementation order, but listed last here because
   they're trivial and obvious.

---

## 7. Questions

All eight questions raised at scoping kickoff are resolved (§1.2).
No open questions remain. If implementation surfaces new tensions
the answers should be added here as resolutions, then promoted to
SPEC4.md.

---

## 8. What this document is, and what comes next

This is the design exploration. The next artifact is `SPEC4.md` —
the locked specification, modeled on `SPEC3.md`'s structure
(numbered sections matching SPEC.md / SPEC2.md / SPEC3.md, with
deltas only). After that, implementation in the order of §6.

The Phase 4 DoD will mirror the Phase 3 DoD's structure:

1. Phase 1 / Phase 2 / Phase 3 chaos tests don't regress.
2. The §5.1 blob-GC-vs-adoption chaos test passes 10 consecutive
   runs.
3. The §5.2 GC-vs-evict chaos test passes 10 consecutive runs.
4. The §5.3 GC-vs-displacement chaos test passes 10 consecutive
   runs.
5. The §5.4 crash-mid-batch chaos test passes 10 consecutive runs.
6. *(Deliberately dropped, by parallel to SPEC3 §12.5.)* The v3 → v4
   migration end-to-end test is **not** required for Phase 4 done.
7. The cache is deployed to one production environment with
   `gc.enabled = true` and default `gc.*` for at least one week.
   Monitoring shows:
   - `gc_run_complete` events at expected cadence;
   - cumulative `bytes_reclaimed > 0` after a week of normal traffic
     with rolling adoptions;
   - bounded `blob` table row count (does not grow unboundedly with
     time);
   - bounded `pool/` byte count;
   - GC goroutines drain cleanly on graceful shutdown (no leaked
     `pool/` orphans across the next-startup sweep);
   - no observed `gc_disabled` Warn (i.e. the operator did not opt
     out unintentionally).
8. SPEC4.md reflects the as-built reality (this document is
   updated as we go, not just before).
