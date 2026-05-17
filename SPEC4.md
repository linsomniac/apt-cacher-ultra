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
   step 5). Phase 4 reaps candidates whose
   `suite_snapshot.heartbeat_at` (a new column written by the
   adoption goroutine after every member fetch, every hot-
   prefetch deb fetch, and on a periodic in-process ticker —
   §7.5.2) is older than
   `max(upstream.total_timeout × upstream.max_retries, 30m)`.
   The bound applies to *time between heartbeats*, not total
   adoption duration. With the periodic ticker (default
   `gc.heartbeat_interval = 60s`) running for the adoption
   goroutine's entire lifetime — covering Packages-parse,
   hot-set computation, and writer-queue waits in addition to
   the event-driven fetch sites — the heartbeat-gap is bounded
   by `heartbeat_interval + writer-queue depth`, which under any
   realistic deployment is two orders of magnitude under the
   30m grace floor. A stale heartbeat is therefore *presumed
   orphan*, not "provably orphan": the presumption fails only
   under a sustained writer-queue stall on the order of minutes
   (which would itself surface as elevated request latency long
   before GC fires).

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

Phase 1's `tmp/` mtime sweep carries forward unchanged. Phase 4
adds two new startup-time passes, sequenced *after* the existing
sweep and before listeners come up:

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

**Sequencing.** The actual integration boundary is `main`, not
`cache.Open`. `cache.Open` does (a) directory creation, (b)
SQLite open, (c) `migrate(...)`, (d) writer-goroutine start, and
returns. The post-open sweeps and Phase 4 passes are sequenced
in `cmd/apt-cacher-ultra/main.go` immediately after `cache.Open`
returns and before the freshness scheduler / integrity scanner /
HTTP listener are constructed:

```
in cache.Open:
  1. mkdir pool/, tmp/, staging/                (Phase 1)
  2. SQLite open + migrate                      (Phase 1)
  3. writer goroutine starts                    (Phase 1)
  -- cache.Open returns --

in main:
  0a. listeners bound (net.Listen)              (Phase 1, fail-fast)
  -- post-cache.Open, pre-Accept: --
  4. tmp/ mtime sweep                           (Phase 1, existing)
  5. pool/ orphan-file scan                     (Phase 4 NEW)
  6. one-shot GC pass                           (Phase 4 NEW)
  7. construct freshness checker, integrity scanner, handler
  8. start periodic schedulers (Phase 1/2/3/4)
  9. listeners begin Accept()                   (Phase 1)
```

The listening sockets are *bound* (the `net.Listen` syscalls)
ahead of `cache.Open` and steps 4–6 — this is a deliberate
fail-fast: if the configured listen port is already in use, the
daemon should exit before doing minutes of pool scanning. The
distinction worth knowing is that `net.Listen` only puts the
socket into the listening state; nothing reads from it until
`http.Server.Serve()` runs at step 9. During the startup window
(steps 4–8), inbound TCP connections accumulate in the kernel
SYN/accept queue but no goroutine `Accept()`s them — clients see
a normal TCP connect (no RST) and a delay until the daemon
finishes startup.

Steps 5 and 6 are blocking — the cache does not begin answering
requests until step 9. Operators with very large pools should
expect startup latency proportional to `pool/` size; the worker
pool and the `gc.pool_scan_workers` knob bound the I/O fanout
but not the wall-clock floor for the pass.

**`staging/` residue note.** Phase 2's adoption flow leaves
`staging/<snapshot_id>/` directories that should be cleaned up
as part of `runShared`'s normal exit path; they only persist as
residue when the previous process crashed or shutdown cancelled
mid-adoption (SPEC2 §9.5). There is no startup sweep over
`staging/` in the current codebase; SPEC4 does not introduce
one (out of scope for the Phase 4 contract). Residual
`staging/` directories are a Phase 2 follow-up tracked
separately; Phase 4 GC of `pool/` orphans does *not* cover
`staging/` because the two layouts are content-addressed
differently (`pool/` is `<hash[:2]>/<hash>`, `staging/` is
`<snapshot_id>/`). An operator who restarts after a crashed
adoption will see `staging/<snapshot_id>/` linger; manual
cleanup (or a future Phase 4.x add-on) is the path forward.

### 4.3 SQLite schema

Phase 4 schema is `schema_version = 4`. Migration v3 → v4 is
described in §4.3.2.

#### 4.3.1 Phase 4 schema delta

One new column on `blob`, one new column on `suite_snapshot`, two
new partial indexes:

```sql
-- The "since refcount reached 0" grace clock for blob GC. NULL
-- when refcount is strictly positive. Set to the unix epoch
-- second of the transition when refcount drops to ≤ 0; cleared
-- back to NULL when refcount transitions back to strictly
-- positive. New blob rows are born at refcount=0 by PutBlob, so
-- refcount_zeroed_at is set to created_at on insert (§7.5.1).
ALTER TABLE blob ADD COLUMN refcount_zeroed_at INTEGER;

-- Adoption-candidate liveness clock for snapshot GC. The
-- adoption goroutine writes this field at row creation and after
-- every member fetch and every hot-prefetch deb fetch. The
-- orphan-candidate reap predicate (§9.6.3) keys on it instead of
-- created_at: a fixed wall-clock grace from creation cannot bound
-- a still-running adoption (member-fetch phase + hot-prefetch
-- phase × hot_count is unbounded under hot_prefetch_budget=0s),
-- but a "time since last heartbeat" grace bounds only the
-- between-fetches gap, which is by construction <= one
-- upstream.total_timeout × max_retries.
ALTER TABLE suite_snapshot ADD COLUMN heartbeat_at INTEGER;

-- Covering partial index over the GC candidate set only. The
-- partial WHERE clause keeps the index small under steady state
-- (the candidate set is tiny relative to the full blob table);
-- including hash and size in the index columns makes the §9.6.2
-- SELECT covering — SQLite returns hash and size from the index
-- without touching the main table.
--
-- (Note: SQLite still has to scan the full blob table when
-- BUILDING the index — the partial-WHERE predicate is evaluated
-- per row at build time. Steady-state queries against the index
-- are O(candidate set), but migration-time index creation is
-- O(table size). See §4.3.2 cost prose.)
CREATE INDEX idx_blob_gc
  ON blob(refcount_zeroed_at, hash, size)
  WHERE refcount <= 0;

-- Index for §9.6.2's NOT EXISTS reachability check from blob GC.
-- The url_path PK is (canonical_scheme, canonical_host, path) —
-- it does NOT lead with blob_hash, so the bare predicate
-- `url_path WHERE blob_hash = ?` would full-scan url_path once
-- per blob in the candidate set. Without this index, blob GC's
-- per-batch cost is O(batch_size × |url_path|), which on a
-- realistic cache (millions of url_path rows) blows the per-tick
-- budget. The partial WHERE keeps the index small (NULL
-- blob_hash entries are url_path rows for un-fetched URLs that
-- can't possibly reference any blob).
CREATE INDEX idx_url_path_blob
  ON url_path(blob_hash)
  WHERE blob_hash IS NOT NULL;
```

No new tables. No changes to `snapshot_member`,
`package_hash`, `suite_freshness`, or `schema_version`. The
`suite_snapshot` blob FK columns (`inrelease_hash`, `release_hash`,
`release_gpg_hash`) are NOT separately indexed — `suite_snapshot`
holds one row per adoption (low hundreds of rows even on a
long-running cache), so the §9.6.2 NOT EXISTS subquery's full
scan of the table is cheaper than maintaining three more indexes.

The `<= 0` (not `= 0`) in the partial index predicate is
load-bearing: SPEC2 §6.1 step 5 documents that a transient
`refcount = -1` is reachable when an adoption transaction's
decrement races a §6.1 hit-path eviction's decrement. Both
decrements are correct per their own bookkeeping; the row should
still be reaped.

#### 4.3.2 Migration v3 → v4

```
migrations[3] = v3 → v4:
  ALTER TABLE blob           ADD COLUMN refcount_zeroed_at INTEGER;
  ALTER TABLE suite_snapshot ADD COLUMN heartbeat_at       INTEGER;
  UPDATE blob
     SET refcount_zeroed_at = created_at
   WHERE refcount <= 0;
  UPDATE suite_snapshot
     SET heartbeat_at = created_at
   WHERE heartbeat_at IS NULL;
  CREATE INDEX idx_blob_gc
    ON blob(refcount_zeroed_at, hash, size)
   WHERE refcount <= 0;
  CREATE INDEX idx_url_path_blob
    ON url_path(blob_hash)
   WHERE blob_hash IS NOT NULL;
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
- **`refcount_zeroed_at` backfill.** Pre-v4 rows at
  `refcount <= 0` have an unknown actual transition time. The
  migration sets `refcount_zeroed_at = created_at` — the
  conservative choice. If the row has been ≤ 0 the entire time it
  has existed, the grace has long since elapsed and the next GC
  tick reaps; if it transitioned to ≤ 0 yesterday and we
  attribute the transition to `created_at`, we still reap
  correctly, just one grace too soon rather than too late. This
  is a one-time pre-Phase-4 backlog drain; steady state has the
  column maintained by §7.5.1.
- **`heartbeat_at` backfill.** Pre-v4 candidate snapshot rows
  (`adopted_at IS NULL`) are by definition either still-running
  adoptions from a previous process or orphans from a previous
  process's failed adoption. In both cases the previous process is
  no longer running (we only run this migration on startup), so a
  pre-v4 candidate is provably an orphan. `heartbeat_at =
  created_at` sets the clock to the row's age; the orphan-
  candidate grace from §9.6.3 then reaps any candidate older than
  the grace on the next tick — correct behavior.
- **Index creation cost.** Two `CREATE INDEX` statements run.
  - `idx_blob_gc` scans every row of `blob` to evaluate the
    partial-index `WHERE refcount <= 0` predicate. The resulting
    index contains only matching rows, but the *build* is O(table
    size). For typical Phase 3 deployments (tens of thousands to
    low millions of blob rows) the build is well under a minute.
  - `idx_url_path_blob` scans every row of `url_path` to evaluate
    its partial-index `WHERE blob_hash IS NOT NULL` predicate.
    `url_path` typically has the largest row count of any table
    in the database (one row per cached URL: every .deb, every
    metadata file, every Release file ever fetched). For typical
    deployments this is hundreds of thousands to a few million
    rows — sub-minute on healthy storage, slower on degraded fs.

  Long-running caches with accumulated GC backlog (many millions
  of rows in either table) should expect tens of seconds to a few
  minutes per index. The migration is startup-blocking (the cache
  does not begin answering requests until it completes), parallel
  to prior-phase startup-blocking migrations.
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

# Number of blob rows reaped per per-tick batch. The §9.6.2 reap
# loop runs batches until either the candidate set empties, the
# per-tick wall-clock guard (max_tick_duration) trips, or the
# lifecycle context is cancelled. Default 100: each batch is one
# write transaction, finishes in milliseconds on a healthy WAL,
# and contributes minimal write-lock latency to concurrent
# adoptions / EvictURLPath.
batch_size            = 100

# Number of snapshot rows reaped per per-tick batch in §9.6.3
# (orphan candidates + displaced snapshots beyond keep-N). A
# separate knob from blob batch_size because each snapshot's
# cascade DELETE touches three tables and may remove tens of
# thousands of snapshot_member + package_hash rows (large debian
# main suites). Default 10: keeps each batch's writer-lock hold
# in the low milliseconds even on a startup-pass with a large
# pre-Phase-4 backlog. Operators with very small suites (one or
# two architectures, a few hundred packages) can safely raise
# this; operators with debian-multiverse-style coverage should
# leave it at the default.
snapshot_batch_size   = 10

# Hard upper bound on how long a single GC tick (periodic OR
# startup) is allowed to run. The reap loop checks the deadline
# between batches; a tick that exceeds the bound exits cleanly
# and the next ticker fire (or the periodic loop after the
# startup pass) picks up the remaining backlog. Default 5m.
# Without this bound, a startup pass with a large pre-Phase-4
# backlog could monopolize startup, and a periodic tick could
# overrun its interval on a degraded fs. 0s is rejected at load
# (a tick must have a deadline).
max_tick_duration     = "5m"

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

# Period of the in-process per-adoption heartbeat ticker (§7.5.2
# site 6). Each adoption goroutine launches a sidecar goroutine
# at runShared start; the sidecar wakes every heartbeat_interval
# and submits a HeartbeatSnapshot write, exiting when runShared
# returns. The interval bounds the heartbeat-gap during phases
# the five event-driven sites do not cover (Packages-parse,
# hot-set computation, writer-queue waits). Default 60s — small
# enough that 60s + worst-case writer-queue traversal stays well
# under the 30m grace floor; large enough that an adoption
# burst's heartbeat-write rate stays in the noise relative to
# the request-path write rate.
#
# Must be > 0s and strictly less than the runtime-derived
# heartbeat_stale_grace_effective (max(total_timeout ×
# max_retries, 30m)) — a heartbeat_interval >= grace is
# pathological (the ticker can't bound the gap if the gap can
# already exceed grace on a single missed tick). 0s is rejected
# at load.
heartbeat_interval    = "60s"

# URL-path TTL — the fourth reap class (§9.6.5). A url_path row
# whose last_requested_at is older than this AND whose
# (canonical_scheme, canonical_host, path) is not vouched for by
# any current snapshot's package_hash is deleted; the row's
# blob.refcount is decremented in the same writer-tx, after which
# the existing blob pass (§9.6.2) reaps the bytes once
# `blob_grace` elapses. Rows with `last_requested_at IS NULL`
# (rows pre-warmed by adoption but never served) are protected
# unconditionally so a hot-set entry the fleet has not yet
# fetched is not eagerly evicted. Default 168h (7 days) — the
# proxy's primary use case is to cache a *current* working set,
# and an "apt update" before "apt install" is a reasonable
# precondition for installing packages the cache may have aged
# out. Set to 0s to disable the URL-path pass entirely (the Phase
# 1-3 behavior — only hash-mismatch eviction at serve time and
# adoption-time refcount decrement reach url_path); 0s is the
# only permitted sub-1m value (values >0 but <1m are rejected at
# load to avoid request-burst thrashing).
url_path_ttl          = "168h"
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
- `gc.snapshot_batch_size` parses as int, ≥ 1. 0 is rejected at
  load (same rationale).
- `gc.max_tick_duration` parses as duration, > 0. 0 is rejected
  at load (a tick must have a deadline; see the example block).
- `gc.blob_grace` parses as duration, ≥ 1s. 0 is rejected at load
  (see the example block above). Sub-second values are also
  rejected: the §9.6.2 reap predicate works in unix-epoch-seconds
  (`int64(d.Seconds())`), and a value like `500ms` would silently
  truncate to 0, making refcount=0 blobs immediately reapable on
  the next tick — exactly the safety failure mode 0s names. The
  validation refuses anything below 1s for that reason.
- `gc.keep_displaced` parses as int, ≥ 0. 0 is permitted (means
  "no forensic retention").
- `gc.pool_scan_workers` parses as int, ≥ 1.
- `gc.url_path_ttl` parses as duration, ≥ 0. `0s` disables the
  URL-path pass entirely (the Phase 1-3 behavior — see §9.6.5).
  Values >0 are rejected if below 1m: a sub-minute TTL would let
  a transient request burst followed by a quiet minute reap rows
  that are still operationally hot, producing thrash with no
  bound on the resulting upstream re-fetch storm.
- `gc.heartbeat_interval` parses as duration, > 0. 0 is rejected
  at load (a 0-interval ticker is an infinite busy-loop). Loaders
  also reject two cross-key violations:
  - `heartbeat_interval >= heartbeat_stale_grace_effective`
    (the runtime-derived grace is `max(upstream.total_timeout ×
    upstream.max_retries, 30m)`); a heartbeat ticker that ticks
    slower than the grace can't bound the heartbeat-gap and would
    let GC reap a live adoption's *candidate snapshot* mid-fetch.
  - `2 × heartbeat_interval >= gc.blob_grace`. The §7.5.2 site 6
    ticker also calls `cache.HeartbeatBlobs` (see §7.5.2 below)
    which refreshes `blob.refcount_zeroed_at` for in-flight
    member blobs. The bound is `2×` (not `1×`): a single missed
    heartbeat (writer-queue stall, transient DB lock — both
    surface as `adoption_heartbeat_blobs_failed` Warn) extends
    the worst-case heartbeat-gap to `2 × heartbeat_interval`,
    and that gap must still fit inside `gc.blob_grace` so a
    single failure doesn't let an in-flight blob age past the
    §9.6.2 reap predicate before `CommitAdoption` Step 4 lands.
    Without this margin (e.g. `heartbeat_interval = 4m,
    blob_grace = 5m`), one missed heartbeat lets the blob age
    8m → past grace → reaped mid-adoption. The
    `gc_heartbeat_interval_unsafe` Error class names *both*
    cross-key violations; the validation error string identifies
    which bound was violated.

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
`PutBlob` is extended per §7.5.1 rule 1: the INSERT sets
`refcount_zeroed_at = created_at`, and the `ON CONFLICT(hash) DO
UPDATE` path refreshes `refcount_zeroed_at = now` whenever the
existing row is at `refcount <= 0` (closing the
"orphan-blob-reuse before FK-INSERT lands" race). No behavioral
surface change at the request level.

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
the behavioral level. Phase 4 makes two mechanical changes at the
SQL level:

1. Every refcount-mutating UPDATE also maintains
   `blob.refcount_zeroed_at` (§7.5.1).
2. The candidate snapshot row's `heartbeat_at` is written at row
   creation, after every member fetch, and after every hot-prefetch
   deb fetch (§7.5.2).

Neither change adds a transaction step, new serialization, or a new
failure mode.

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
ON CONFLICT(hash) DO UPDATE
   SET refcount_zeroed_at = excluded.refcount_zeroed_at
 WHERE blob.refcount <= 0;
```

The conflict path is **not** `DO NOTHING`. The reason is the
"reuse an orphan blob" race: an existing blob row may already be
sitting at `refcount <= 0` with a `refcount_zeroed_at` from
minutes or hours ago — old enough that its grace has already
expired. A fresh §6.2 miss-path or §7.5 adoption / hot-prefetch
that re-fetches that same content calls `PutBlob`; the §6.1
caller will then INSERT a new `url_path` row (or `CommitAdoption`
will INSERT new `snapshot_member` rows) sometime later. Between
the `PutBlob` ExecContext returning and the FK-bearing INSERT
committing, GC can run, see `refcount <= 0` AND already-expired
grace AND no FK references AND no `snapshot_member` references,
and reap the blob. The freshly-fetched content disappears under
the caller.

`ON CONFLICT(hash) DO UPDATE SET refcount_zeroed_at =
excluded.refcount_zeroed_at WHERE blob.refcount <= 0` restarts
the grace clock to "now" whenever an orphaned blob (`refcount
<= 0`) is reused, giving the caller a full `gc.blob_grace`
window to land its FK reference. The `WHERE blob.refcount <= 0`
predicate is on the conflict's UPDATE clause itself — when
`blob.refcount > 0` SQLite skips the UPDATE entirely (no
journal write, no row mutation, no writer-lock UPDATE step),
which is materially better than an `IIF` that writes the
column to its own existing value (the latter still takes the
write path and bumps the page-cache dirty bit). On the hot
path of a positive-refcount existing row colliding with a
re-fetch, the conflict resolves with zero writes.

The *other* invariants of `DO NOTHING` are preserved by the
`ON CONFLICT DO UPDATE` body's narrow column list — `refcount`,
`size`, and `created_at` are NOT in the SET list, so a
freshly-arriving conflict cannot stomp on the existing row's
refcount or rewrite its created_at. Only the GC clock moves,
and only when `refcount <= 0`.

Once the caller's FK-bearing INSERT lands and Rule 2 increments
`refcount` to a strictly positive value, Rule 2 sets
`refcount_zeroed_at = NULL` and the GC clock is removed entirely
— the per-conflict `now` value written here is just a window
extender, not a permanent state.

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

#### 7.5.2 Candidate snapshot heartbeat

The orphan-candidate reap predicate in §9.6.3 keys on
`suite_snapshot.heartbeat_at`, not on `created_at`. The candidate
row exists from the start of `runShared` (the
`InsertCandidateSnapshot` call at
`internal/freshness/adoption.go:384`) through the final
`CommitAdoption` flip — a window that includes sequential member
fetches, `Packages` parsing, and the hot-prefetch loop. Under
adversarial conditions (large Release file, slow members,
`hot_prefetch_budget = 0s`) this window has no fixed wall-clock
upper bound, so a `created_at`-based grace cannot be both safe
(never reap a still-running adoption) and useful (reap promptly
after failure).

The heartbeat reframes the problem: instead of bounding total
adoption duration, bound only the *time between heartbeats*.
Adoption writes the heartbeat at six sites — five event-driven
and one periodic — sequenced to cover each phase of `runShared`:

1. **Row creation.** `InsertCandidateSnapshot` sets
   `heartbeat_at = created_at` on the new row's INSERT (and on
   the reused-orphan path's UPDATE `refresh mutable cols on
   reuse` at `internal/cache/adoption.go:140-148`, extended to
   include `heartbeat_at = ?`).
2. **After every member fetch.** `adoptMember` returns; the
   adoption goroutine calls a new `cache.HeartbeatSnapshot(ctx,
   snapshot_id)` helper, which runs:
   ```sql
   UPDATE suite_snapshot SET heartbeat_at = ? WHERE snapshot_id = ?;
   ```
3. **After `buildPackageHashes` returns**
   (`internal/freshness/adoption.go:487`). Packages parsing of a
   debian-main suite at multiple architectures can be many
   tens of MiB of compressed input; on degraded CPU or storage
   the parse can take minutes. Without this heartbeat, the gap
   from the last member-fetch heartbeat through Packages parsing
   is unbounded by any fetch timeout.
4. **After every hot-prefetch deb fetch.** The §SPEC3 7.5 hot-
   prefetch loop calls the same `HeartbeatSnapshot` after each
   per-deb fetch terminates (success, failure, or cancel — every
   loop-iteration end). The first deb fetch in the loop is
   preceded by hot-set computation (Stage 1+2 SQL JOINs, ~ms
   typically) — small enough that no separate heartbeat is
   needed at hot-set computation time.
5. **Right before `CommitAdoption`**
   (`internal/freshness/adoption.go:507`, immediately before the
   call). Once the writer queue picks `CommitAdoption` up, the
   transaction either commits `adopted_at` (removing the row
   from the orphan-candidate predicate) or rolls back (leaving
   the row a candidate for reap on the next eligible tick). This
   heartbeat resets the grace clock at the latest possible
   moment before the adopted_at flip becomes the source of
   truth, defending against writer-queue depth between
   `runHotPrefetch` returning and `CommitAdoption` actually
   committing.

6. **Periodic in-process ticker.** A goroutine launched at the
   start of `runShared` (alongside, but logically *parallel to*,
   the main adoption flow) wakes every
   `gc.heartbeat_interval` (default 60s) and submits *two*
   writes: a `HeartbeatSnapshot` for the candidate row's
   `heartbeat_at`, and a `HeartbeatBlobs` for any in-flight
   member-blob hashes the adoption goroutine has accumulated
   in its per-adoption tracker (see "In-flight blob heartbeats"
   below). The ticker exits when `runShared` returns
   (cooperative cancel via the same context that propagates to
   the member-fetch loop). This site bounds the heartbeat-gap
   independently of which phase `runShared` is in — Packages-
   parse, hot-set computation, the gap from a member-fetch to
   the next sub-call, the gap between `runHotPrefetch`
   returning and `CommitAdoption` actually running. Sites 1–5
   keep their value as latency-fresh event-driven heartbeats
   (a heartbeat written immediately after a member fetch is
   more useful for monitoring than one that drifts up to 60s
   later); the ticker is the *floor* under them, not a
   replacement. A pathological case where every single event-
   driven site silently fails is still bounded by the ticker.

**In-flight blob heartbeats.** §7.5.1 Rule 1 sets
`refcount_zeroed_at = now` on every `PutBlob` of a member blob,
restarting that blob's `gc.blob_grace` clock. For an adoption
that fetches members sequentially over minutes (large suites,
slow upstreams, hot-prefetch loop with `hot_prefetch_budget=0`),
the *first* member blob's grace clock is the one most at risk:
without periodic refresh, it can age past `gc.blob_grace` before
`CommitAdoption` Step 4 runs and bumps `refcount`, at which
point the §9.6.2 reap predicate fires and the FK INSERT in
CommitAdoption fails (or worse, the file is unlinked under the
caller). `cache.HeartbeatBlobs(ctx, hashes)` runs:

```sql
UPDATE blob
   SET refcount_zeroed_at = ?
 WHERE hash IN (...) AND refcount <= 0;
```

The `refcount <= 0` predicate preserves Rule 2's strictly-
positive crossing — once `CommitAdoption` Step 4 lands and
`refcount_zeroed_at` is cleared, subsequent `HeartbeatBlobs`
calls for the same hashes become per-row no-ops. The ticker
site (and only the ticker site) drives `HeartbeatBlobs`;
sites 1–5 are concerned with the candidate row's liveness and
do not need to refresh blob clocks (the ticker's bound is
strictly tighter than any other heartbeat site by virtue of
running on a fixed cadence).

The cross-key validation
`2 × gc.heartbeat_interval < gc.blob_grace` (§5.2) makes this
safe across one transient failure: the ticker fires at least
twice per grace window, so the `refcount_zeroed_at` of any
in-flight member blob is bounded above by
`heartbeat_interval` since the last refresh under healthy
operation, and `2 × heartbeat_interval` if a single ticker
fire's `HeartbeatBlobs` write fails — both within
`gc.blob_grace`.

A `HeartbeatBlobs` write failure is logged at Warn under
`adoption_heartbeat_blobs_failed` and the adoption proceeds.
The next ticker fire retries; the 2× margin in the
heartbeat-vs-grace validation means one missed heartbeat does
not itself imperil any in-flight blob. Repeated failures
*can* let a blob age past grace (three consecutive misses
would push the bound to `3 × heartbeat_interval`); the loud
Warn line is the operator-visible signal to address the
underlying stall (writer-queue saturation, DB lock contention,
etc.) before it does.

**Bound on heartbeat-gap.** With the ticker running, the gap
between consecutive heartbeats is bounded by
`gc.heartbeat_interval + writer-queue depth`. The writer queue
holds `writeBufferSize = 256` slots (`internal/cache/cache.go`),
and the queue is FIFO with a single writer that runs each op to
completion before pulling the next; under healthy load the
through-rate is hundreds of ops/s and the queue is near-empty.
Even under a writer-bound stall (every slot full, slowest op
taking ~10s), queue-traversal time is bounded at low minutes.
With a 60s ticker and ~5m worst-case queue traversal, the
heartbeat-gap upper bound is ~6m — well under the §9.6.3 grace
formula's 30m floor.

The §9.6.3 grace bound
`max(upstream.total_timeout × upstream.max_retries, 30m)` thus
strictly exceeds the heartbeat-gap upper bound under any
realistic deployment. A genuinely-stalled adoption (process
killed mid-fetch, ctx cancel + drain, etc.) ages out within
`grace + heartbeat_interval ≈ grace + 60s`.

The presumption "stale heartbeat → orphan" fails only under a
*sustained* writer-queue stall on the order of multiple minutes,
which would itself surface as request-latency degradation
(every Phase 1 hit-path eviction, every adoption commit, every
freshness adoption goes through the same writer queue). This
is an operator-visible failure mode, not a silent wrong
adoption-reap.

`HeartbeatSnapshot` runs as a small standalone write (not
inside a larger transaction) — it does not block on or
serialize with `CommitAdoption`'s atomic flip. Heartbeat
write failures (e.g., disk full) do not abort the adoption;
they are logged at Warn under `adoption_heartbeat_failed` and
skipped. A skipped heartbeat is benign: the next heartbeat
(at the next site) restores liveness; or the adoption
completes and the row's `adopted_at` discriminator removes it
from the orphan-candidate predicate.

#### 7.5.3 Atomic flip transaction (Phase 3 carry-forward)

The transaction structure of `CommitAdoption` is unchanged from
SPEC3 §7.5. Steps 1–7 carry forward exactly. Steps 4 and 8 use
the §7.5.1 SQL above. No changes to step ordering, transaction
boundaries, or commit semantics.

#### 7.5.4 Hot-set computation and remaining adoption sub-sections

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
with adoption commits and request-path writes. Two distinct batch
sizes apply:

- `gc.batch_size = 100` (blob GC, §9.6.2): each batch's
  `DELETE...RETURNING` plus iterate-and-buffer takes low
  milliseconds on a healthy WAL.
- `gc.snapshot_batch_size = 10` (snapshot GC, §9.6.3): smaller
  because each cascade DELETE may touch tens of thousands of
  `snapshot_member` and `package_hash` rows on debian-main-scale
  suites; 10 snapshots × ~50K rows = ~500K deletes, still bounded
  to low hundreds of milliseconds on healthy storage.

Heartbeat writes (§7.5.2) also go through `submitWrite` but are
single-row UPDATEs and bound by their own queue position, not by
GC batch hold-time.

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

The 30s drain budget is unchanged. GC's worst-case writer-lock
hold inside one blob-GC batch is just the DELETE...RETURNING
transaction — `gc.batch_size = 100` row-deletes — typically low
milliseconds on a healthy WAL. The 100 `os.Remove` calls run
*after* the COMMIT, outside any lock (§9.6.2 ordering); at most
they delay the next batch's BEGIN, never another writer's. A
concurrent adoption commit can interleave between batches (the
ticker model holds no lock between iterations). Cancelling
mid-tick has no correctness consequences: in-flight transactions
commit or roll back atomically, in-flight `os.Remove` calls
complete (they don't take the SQL lock), partial-batch work is
re-picked-up next tick.

### 9.6 Garbage collection (NEW)

The Phase 4 GC subsystem runs as a single dedicated goroutine,
started in `main` after the §4.2 startup sequence (steps 5–6 —
pool scan and one-shot GC pass) completes and before listeners
come up (§4.2 step 9). It owns five reap classes — four
periodic, one startup-only.

#### 9.6.1 Goroutine lifecycle

```
on startup (in §4.2 step 6, blocking; §4.2 step 5 — the pool
scan — runs first and is independent of this loop):
  deadline := now + gc.max_tick_duration
  1. Run url_path TTL pass (§9.6.5, batched, deadline-bounded;
                            short-circuit if gc.url_path_ttl = 0s)
  2. Run snapshot GC pass  (§9.6.3, batched, deadline-bounded)
  3. Run blob GC pass      (§9.6.2, batched, deadline-bounded)

then, every gc.interval:
  deadline := now + gc.max_tick_duration
  4. Run url_path TTL pass
  5. Run snapshot GC pass
  6. Run blob GC pass

on lifecycleCtx cancel:
  exit at next per-batch boundary
```

**One per-tick deadline shared across all three passes.** The
deadline is computed once at tick start, before the URL-path
pass begins; all three passes see the same
`time.Now().After(deadline)` clock. If an earlier pass exhausts
the deadline (large startup backlog of url_path rows, orphan
candidates, or displaced snapshots), later passes receive an
already-expired deadline and exit immediately with
`gc_tick_deadline_reached`. The next tick re-runs all three
passes; under sustained backlog, earlier passes drain over
several ticks before later passes start reaping. This is
correct: the operator sees `gc_tick_deadline_reached` and can
raise `gc.max_tick_duration` or the relevant batch_size if
needed, and the steady state is unaffected (no realistic steady
state has a large enough backlog to monopolize a tick).

**Order matters within a tick.** URL-path TTL runs *first* so
its blob-refcount decrements land before the same-tick blob pass
evaluates reachability — a url_path row reaped at 00:00:01 can
produce a blob reap at 00:00:02 in the same tick rather than
waiting a full `gc.interval` for the next round. Snapshot GC
runs *between* url_path and blob because deleting
`snapshot_member` and `suite_snapshot` rows (§9.6.3) removes FK
references that the blob GC's NOT EXISTS predicate (§9.6.2)
consults. With this order, a tick that ages out a url_path row,
displaces snapshot `S`, and reaps blob `B` (referenced only by
the url_path row and by `S`'s `snapshot_member` row) drains all
three in one tick. Reversing any pair would leave intermediate
work pending for an extra tick.

Within each pass, every per-batch transaction completes
atomically; a `lifecycleCtx` cancel between batches is benign
(in-flight tx commits or rolls back atomically; the next start
picks up the remaining backlog).

#### 9.6.2 Blob GC pass

The pass enters with a per-tick deadline computed at tick start
as `now + gc.max_tick_duration`. The reap loop checks the
deadline (and `lifecycleCtx`) between batches and exits cleanly
on either condition.

```go
deadline := time.Now().Add(cfg.GC.MaxTickDuration)
for {
    if ctx.Err() != nil { return ctx.Err() }
    if time.Now().After(deadline) {
        // Log gc_tick_deadline_reached at Info (§10.2);
        // remaining backlog is picked up next tick.
        return nil
    }
    reaped, err := runOneBlobGCBatch(ctx, ...)
    if err != nil { return err }
    if reaped == 0 { return nil }   // no more candidates
}
```

**Reachability predicate.** Refcount tracks
`snapshot_member.blob_hash` references only — every increment in
§7.5.1 Rule 2 walks `snapshot_member` for a snapshot, and every
decrement in Rule 3 mirrors that walk in reverse (or follows a
`url_path` evict). The four FK paths into `blob` that refcount
does NOT track are: `url_path.blob_hash`, and the three
`suite_snapshot.{inrelease_hash, release_hash, release_gpg_hash}`
columns. SQLite's `PRAGMA foreign_keys = ON` would FK-fail a
DELETE while any of these four remain, and even if FKs were ever
weakened the rows would dangle. The reap predicate must therefore
exclude any blob still reachable through any of those four FK
paths via NOT EXISTS clauses. The `refcount <= 0` clause is
correct *as a proxy* for snapshot-member reachability (modulo the
documented `-1` race), but it is incomplete reachability on its
own.

`package_hash.declared_sha256` is **not** a foreign key into
`blob` — it is a hash check value materialized at adoption time
to validate request-path .deb fetches against the snapshot's
declared content. A blob's absence from the pool does not break
`package_hash`; the request-path lookup just falls through to a
§6.2 cache miss + refetch. So `package_hash` is correctly
omitted from the reachability predicate; refcount does not track
it and NOT EXISTS does not need to consult it. The
`package_hash` rows of a reaped snapshot are removed by §9.6.3's
cascade DELETE alongside the `snapshot_member` rows.

```sql
-- Per-batch SELECT (uses idx_blob_gc partial covering index for
-- the lead clauses; the NOT EXISTS sub-queries use indexes on
-- url_path.blob_hash (Phase 4 idx_url_path_blob),
-- snapshot_member.blob_hash (idx_snapshot_member_blob), and a
-- table scan for suite_snapshot's three FK columns):
SELECT hash, size FROM blob
 WHERE refcount <= 0
   AND refcount_zeroed_at IS NOT NULL
   AND refcount_zeroed_at < :now - :grace_seconds
   AND NOT EXISTS (
         SELECT 1 FROM url_path
          WHERE blob_hash = blob.hash)
   AND NOT EXISTS (
         SELECT 1 FROM snapshot_member
          WHERE blob_hash = blob.hash)
   AND NOT EXISTS (
         SELECT 1 FROM suite_snapshot
          WHERE inrelease_hash   = blob.hash
             OR release_hash     = blob.hash
             OR release_gpg_hash = blob.hash)
 ORDER BY refcount_zeroed_at
 LIMIT :batch_size;
```

Index plan:

- The `idx_blob_gc(refcount_zeroed_at, hash, size) WHERE
  refcount <= 0` partial covering index serves the lead three
  clauses (`refcount <= 0`, `refcount_zeroed_at IS NOT NULL`,
  `refcount_zeroed_at < ...`), and emits `hash` and `size`
  directly from the index — no main-table touch for the lead
  candidate set.
- The `url_path.blob_hash` NOT EXISTS uses the new
  `idx_url_path_blob` partial index on `url_path(blob_hash)` —
  required for performance (without it each candidate triggers
  a full url_path scan, which on a realistic cache makes blob
  GC catastrophically slow). See §4.3.1.
- The `snapshot_member.blob_hash` NOT EXISTS uses the
  pre-existing `idx_snapshot_member_blob`.
- The three `suite_snapshot` columns are NOT separately indexed.
  `suite_snapshot` holds one row per adoption (low hundreds of
  rows even on a long-running cache); SQLite full-scans the
  table for each candidate in the batch. With `batch_size = 100`
  blobs and ~hundreds of `suite_snapshot` rows, that's ~10k row
  comparisons per batch — sub-millisecond, well under any
  meaningful concurrent-write impact. A separate index here
  would cost more in maintenance write amplification (every
  adoption write would update three indexes) than it would save
  at GC time.

**Per-batch DELETE with RETURNING.** SQLite ≥ 3.35 supports
`DELETE ... RETURNING`. The DELETE re-applies the full
reachability predicate to defend against a concurrent
adoption / EvictURLPath / new url_path insert that became
visible between SELECT and DELETE; the RETURNING clause yields
the *exact* set of hashes (and their sizes) that the DELETE
removed.

The transaction ordering matters and must be exactly:

```
1. BEGIN
2. DELETE ... RETURNING hash, size  →  iterate rows.Next(),
   appending each (hash, size) to an in-memory buffer
3. rows.Close()                      ←  required by SQLite before
                                        COMMIT
4. COMMIT
5. for each (hash, size) in buffer:
       os.Remove(pool/<hash[:2]>/<hash>)
       on success: bytesReclaimed += size
       on ENOENT:  no-op (file already absent)
       on other:   gc_pool_unlink_failed Warn; unlinkErrors++
```

```sql
-- Step 2 (inside the tx):
DELETE FROM blob
 WHERE hash IN (...candidate_hashes_from_SELECT...)
   AND refcount <= 0
   AND refcount_zeroed_at IS NOT NULL
   AND refcount_zeroed_at < :now - :grace_seconds
   AND NOT EXISTS (SELECT 1 FROM url_path
                    WHERE blob_hash = blob.hash)
   AND NOT EXISTS (SELECT 1 FROM snapshot_member
                    WHERE blob_hash = blob.hash)
   AND NOT EXISTS (SELECT 1 FROM suite_snapshot
                    WHERE inrelease_hash   = blob.hash
                       OR release_hash     = blob.hash
                       OR release_gpg_hash = blob.hash)
RETURNING hash, size;
```

The buffer-close-commit-unlink ordering is load-bearing for
correctness under three concurrent failure modes:

1. **Tx commit failure / rollback after rows.Close().** If the
   COMMIT fails (statement-busy escalating, fs error during
   journal write, etc.), the DB rolls back the DELETE. Because
   no `os.Remove` calls have run yet, the pool is still
   consistent with the DB: rows the DELETE *would* have removed
   are still present, and so are their pool files. Bumping the
   unlink phase before COMMIT (e.g., unlinking inside the tx
   while iterating RETURNING) would leave the DB pointing at
   pool files that no longer exist on rollback — the next
   request that resolves to those hashes would read a
   non-existent file.

2. **rows.Close() must precede COMMIT.** SQLite's
   `DELETE...RETURNING` cursor pins the underlying tx; calling
   `tx.Commit()` while rows are still open returns "database
   table is locked" or similar driver-dependent errors. The
   driver-side fix is: read all RETURNING rows into Go memory
   *before* closing the rows iterator, *before* calling
   `tx.Commit()`. Buffer in a slice; the slice grows to at most
   `batch_size` entries, ~tens of KiB.

3. **Process crash between COMMIT and any `os.Remove`.** Caught
   by the next-startup §4.2 pool orphan scan: the DB has no row
   for the file, the scan unlinks it. No correctness impact;
   minor disk leak bounded by *time to next process restart*.

The unlink loop iterates the buffered RETURNING result — the only
information source that names exactly which files to remove.
Iterating the original SELECT result instead would unlink files
for blobs whose row survived the DELETE's race-defending WHERE
filter; that is the bug the prior review flagged.

```go
// Steps 1-4 above:
tx, err := db.BeginTx(ctx, nil)
if err != nil { return err }
defer tx.Rollback() // no-op after Commit succeeds

rows, err := tx.QueryContext(ctx, deleteSQL, ...)
if err != nil { return err }

type reaped struct { hash string; size int64 }
buf := make([]reaped, 0, batchSize)
for rows.Next() {
    var r reaped
    if err := rows.Scan(&r.hash, &r.size); err != nil {
        rows.Close()
        return err
    }
    buf = append(buf, r)
}
if err := rows.Err(); err != nil {
    rows.Close()
    return err
}
rows.Close()                          // step 3

if err := tx.Commit(); err != nil {   // step 4
    return err
}

// Step 5 — outside the tx, no DB lock held:
for _, r := range buf {
    p := filepath.Join(poolDir, r.hash[:2], r.hash)
    if err := os.Remove(p); err != nil {
        if !errors.Is(err, fs.ErrNotExist) {
            logger.Warn("gc_pool_unlink_failed",
                "hash", r.hash, "err", err, "operation", "reap")
            unlinkErrors++
            continue
        }
    }
    bytesReclaimed += r.size
}
```

The `ORDER BY refcount_zeroed_at` makes the per-tick reap work
oldest-first — fairer under sustained backlog than
batch-with-no-order.

**Crash safety.** A crash between `COMMIT` and any `os.Remove`
leaves `pool/<hash>` without a `blob` row — caught by the next
startup's §4.2 pool/ orphan scan. The reverse — DB row missing,
file present — is the *only* failure mode this code path can
produce, and it is harmless. A crash before `COMMIT` rolls back
atomically and no files are unlinked.

#### 9.6.3 Snapshot GC pass

Two sub-jobs identify candidate snapshot ids; reaped together in
a single per-batch transaction. Both reap rows that no
`current_snapshot_id` references.

The pass is **batched**, mirroring blob GC: the §9.6.1 per-tick
deadline is checked between batches, and each batch's cascade
DELETE is bounded by `gc.snapshot_batch_size` (default 10 — see
§5.1). The reason batching is required despite the small steady-
state per-tick volume is that a startup-pass against a long-
running cache (or a v3→v4 upgrade-then-startup) can present
hundreds of orphan candidates and tens of thousands of displaced
snapshots in one go; without batching, the unbounded cascade
DELETE can hold the writer lock long enough that the per-tick
deadline trips before blob GC has a chance to start.

```go
deadline := time.Now().Add(cfg.GC.MaxTickDuration)
for {
    if ctx.Err() != nil { return ctx.Err() }
    if time.Now().After(deadline) {
        // Log gc_tick_deadline_reached at Info (§10.2);
        // remaining backlog is picked up next tick.
        return nil
    }
    // runOneSnapshotGCBatch is submitted via Cache.submitWrite; the
    // SELECT and the three cascade DELETEs all run inside ONE writer
    // tx (see "liveness revalidation at delete time" below).
    reaped, err := runOneSnapshotGCBatch(ctx, ...)
    if err != nil { return err }
    if reaped == 0 { return nil }   // no more candidates
}
```

**Liveness revalidation at delete time.** The candidate SELECT
runs *inside* the same writer-locked transaction as the cascade
DELETEs, not before. Phase 1's single-writer SQLite (§9.4) means
no other writer goroutine can interleave between the SELECT and
the DELETEs — `HeartbeatSnapshot` writes (§7.5.2),
`CommitAdoption`'s atomic flip (Phase 2 §7.5), and any
`InsertCandidateSnapshot` for an orphan-reuse race all serialize
through `Cache.submitWrite`, the same channel that submits the
GC batch op. Two writes never run in parallel. So if the SELECT
includes snapshot `S`, and a heartbeat for `S` would have raced
in, that heartbeat's submit either landed before this tx (in
which case the SELECT inside this tx already sees the fresh
`heartbeat_at` and excludes `S`) or after this tx (in which case
`S` is reaped before the heartbeat can land, and the heartbeat
UPDATE then matches zero rows — the documented benign failure).

If the SELECT were submitted as a separate read and the candidate
ID list were then passed into a *second* submit for the DELETE,
the read connection (from the standard pool, not the writer
goroutine) would observe a snapshot that the writer might mutate
between submissions. That is the bug this revalidation closes.

Inside the single submitWrite op, then:

```go
// runOneSnapshotGCBatch — runs as one writer-tx op:
tx, err := conn.BeginTx(ctx, nil)
if err != nil { return 0, err }
defer tx.Rollback() // no-op after Commit succeeds

// SELECT candidate IDs WITH the full liveness predicate, inside
// the tx. Bind :now to a single fixed wall-clock value so the
// arithmetic for sub-job A's heartbeat-grace check is consistent
// across the SELECT and any follow-up assertions.
candidateIDs, classByID, err := selectSnapshotGCCandidates(ctx, tx, ...)
if err != nil { return 0, err }
if len(candidateIDs) == 0 {
    return 0, tx.Commit()         // empty tx; commit is a no-op
}

// Three cascade DELETEs by ID. The single-writer guarantee plus
// the in-tx SELECT means the predicate held at SELECT time still
// holds at DELETE time; we don't need to repeat the predicate in
// each DELETE's WHERE clause. (We *could* — it would be defensive
// and make the SQL self-documenting — but it would be redundant
// under the single-writer model.)
for _, stmt := range []string{
    "DELETE FROM snapshot_member WHERE snapshot_id IN (...)",
    "DELETE FROM package_hash    WHERE snapshot_id IN (...)",
    "DELETE FROM suite_snapshot  WHERE snapshot_id IN (...)",
} {
    if _, err := tx.ExecContext(ctx, stmt, ids...); err != nil {
        return 0, err
    }
}

return len(candidateIDs), tx.Commit()
```

The candidate-id select is the union of the two sub-jobs below,
already capped at `:snapshot_batch_size`:

```sql
-- Sub-job A: orphan candidates whose heartbeat is past grace
SELECT snapshot_id, 'orphan' AS reap_class FROM suite_snapshot
 WHERE adopted_at IS NULL
   AND heartbeat_at < :now - :heartbeat_stale_grace_seconds
   AND snapshot_id NOT IN (SELECT current_snapshot_id
                             FROM suite_freshness
                            WHERE current_snapshot_id IS NOT NULL)

UNION ALL

-- Sub-job B: displaced snapshots beyond keep-N (current already excluded)
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
) WHERE rn > :keep_displaced

LIMIT :snapshot_batch_size;
```

The `reap_class` column lets the per-batch DELETE step accumulate
counts for the §10 `gc_run_complete.orphan_candidates_reaped` and
`displaced_reaped` fields.

**Sub-job A — Orphan candidates.** The grace is
`max(upstream.total_timeout × upstream.max_retries, 30m)` —
derived from the runtime config, not a separate `[gc]` key. As
detailed in §7.5.2 this bounds the *time-between-heartbeats*,
not total adoption duration; with adoption writing `heartbeat_at`
at the five event-driven sites plus the periodic
`gc.heartbeat_interval` ticker in §7.5.2, the worst-case
heartbeat-gap is bounded by `heartbeat_interval + writer-queue
depth` — strictly within the grace bound under any deployment
that hasn't already failed-loud on writer-queue saturation.

A candidate row with `heartbeat_at` older than the grace is
*presumed* orphaned: the adoption goroutine that owned it
either crashed, was cancelled, or has stalled past any plausible
upstream-fetch timeout *and* past the periodic ticker's
heartbeat schedule. The §10.2
`adoption_heartbeat_failed` Warn is the early signal that the
presumption may not hold for a particular adoption (repeated
heartbeat-write failures can let a live adoption's
`heartbeat_at` age past grace); the operator can address that
loudly-visible failure mode before GC fires.

Pre-v4 candidate rows have `heartbeat_at = created_at` from the
migration backfill (§4.3.2). On the first post-migration GC
tick they are reaped if older than the grace — correct, since
they are by definition orphans (the previous process is no
longer running).

**Sub-job B — Displaced snapshots beyond keep-N.** The
`current_snapshot_id NOT IN ...` clause is applied **before**
the `ROW_NUMBER()` window function, not after. This matters for
correctness: applying the exclusion after the ranking would
include the current snapshot in the per-suite ordering, so with
`gc.keep_displaced = 3` and 5 adopted snapshots (1 current + 4
displaced), the rn = 1, 2, 3, 4, 5 ranks would assign rn = 1 to
the current snapshot; the `rn > 3` filter would then yield rows
4 and 5; after excluding the current (rank 1), only ranks 2 and
3 would survive — keeping only 2 displaced, not the configured
3. Excluding the current snapshot from the partition before
ranking gives ranks 1–4 to the four displaced snapshots; rn > 3
yields row 4; the three most recent displaced survive (correct).

The `ORDER BY adopted_at DESC, snapshot_id DESC` includes a
secondary sort key so that two displacements with the same
unix-second `adopted_at` (rare but reachable on a CommitAdoption
storm) get a deterministic ranking — the larger `snapshot_id` is
the more recently inserted (the column is `INTEGER PRIMARY KEY
AUTOINCREMENT` per Phase 2 schema, monotonic). Without the
tie-breaker, sort order on equal primary keys is implementation-
defined; an operator who runs `gc.keep_displaced = 3` could end
up keeping different rows on different runs.

The `keep_displaced` value is `gc.keep_displaced` from config
(default 3). 0 means "no forensic retention" and reaps every
displaced snapshot on the next tick.

**Per-batch cascade DELETE** (one transaction per batch; the
in-tx SELECT above plus the three DELETEs all run inside the same
`submitWrite` op, holding the writer lock from BEGIN to COMMIT):

```sql
BEGIN
  -- (Sub-job A ∪ Sub-job B SELECT, ID list captured into Go memory)
  DELETE FROM snapshot_member WHERE snapshot_id IN (?, ?, ...);
  DELETE FROM package_hash    WHERE snapshot_id IN (?, ?, ...);
  DELETE FROM suite_snapshot  WHERE snapshot_id IN (?, ?, ...);
COMMIT
```

The order within the tx is fixed: child tables (`snapshot_member`,
`package_hash`) before parent (`suite_snapshot`). With
`PRAGMA foreign_keys = ON` the reverse order would FK-fail on
the suite_snapshot DELETE before the children were gone.

The reaped count comes from each statement's `RowsAffected` —
not from RETURNING — because no on-disk side effects depend on
identifying which specific rows were removed (no pool unlink, no
buffer-then-act ordering). The aggregate counts feed §10's
`orphan_candidates_reaped` and `displaced_reaped` fields by
summing per-`reap_class` from the SELECT step.

**No refcount accounting.** Orphan candidates failed before
`CommitAdoption` Step 4 ever ran, so their `snapshot_member` rows
never bumped any blob refcounts; deleting them does nothing
refcount-wise. Displaced snapshots had their refcounts decremented
at displacement time (Phase 2 Step 8); deleting their rows now
also does nothing refcount-wise — the bytes were already
accounted for.

**Effect on blob reachability.** Although snapshot GC writes no
refcount UPDATEs, the cascade DELETE removes `snapshot_member`,
`package_hash`, and `suite_snapshot` rows that the blob-GC
predicate (§9.6.2) consults via NOT EXISTS (specifically:
`snapshot_member.blob_hash` and `suite_snapshot.{inrelease_hash,
release_hash, release_gpg_hash}`; `package_hash` is not in the
reachability predicate per §9.6.2). So snapshot GC running first
within a tick (§9.6.1 ordering) frees up FK references and lets
the same-tick blob GC pass reap the newly-unreferenced blobs.
Without the ordering, blobs reachable only through a just-
displaced snapshot's `snapshot_member` rows would wait one full
tick for blob reaping.

#### 9.6.4 Pool/ orphan-file scan (startup-only)

Already specified in §4.2 step 5. Walks `pool/<two-hex-prefix>/<hash>`
directories, runs `SELECT 1 FROM blob WHERE hash = ?` for each
file, unlinks files with no matching row. Worker pool sized at
`gc.pool_scan_workers`. Cancellable.

The scan's filename → hash extraction also enforces the same
sha256-hex shape the schema CHECKs (`length = 64, [0-9a-f]
only`); a file in `pool/` whose name doesn't satisfy that shape
is suspicious enough that it triggers a `gc_pool_malformed_name`
Warn (§10) — the file is left alone (don't delete files we don't
recognize), but the operator is told.

Additionally, the scan validates that the file's parent
directory matches `hash[:2]`. `Cache.BlobPath(hash)` always
resolves to `pool/<hash[:2]>/<hash>`; a file at
`pool/<wrong-prefix>/<hash>` is unreachable by request-path
lookups regardless of whether a `blob` row exists for that
hash. Such files are not reaped (the SELECT-by-hash already
treats them as "row exists" if any blob row matches), but they
*are* logged at Warn under `gc_pool_misplaced_file` (§10.2)
with `path`, `expected_prefix=hash[:2]`, and
`actual_prefix=<actual>`. Auto-repair (move the file or unlink
it) is rejected: a misplaced file may itself be the result of
a bug elsewhere (manual operator copy, a previous-version
filesystem layout) that the operator should diagnose before
the daemon mutates state. Logging-only is the conservative
posture.

#### 9.6.5 URL-path TTL pass

Phase 1-3 caches accumulate `url_path` rows monotonically: every
distinct (canonical_scheme, canonical_host, path) the proxy
serves (or that adoption pre-warms) writes a row, and the only
removal site is the SPEC2 §6.1 step 5 hash-mismatch eviction —
which fires only when a Phase 2 snapshot's `package_hash` row
disagrees with the cached `url_path.blob_hash`. For caches that
proxy a long tail of third-party repos with high path churn (CI
artifacts, rotating builds, packaging mistakes, paths upstream
404s but apt still requests), the table grows unbounded and
pins the referenced blobs indefinitely via the §9.6.2 NOT EXISTS
predicate — making them "still reachable from a `url_path`
row" and therefore not reapable even when no client has actually
hit the path in months.

The Phase 4 URL-path TTL pass closes that gap. A `url_path` row
is reapable iff:

- `last_requested_at IS NOT NULL` — rows pre-warmed by adoption
  but never served carry NULL here, and are protected
  unconditionally so a hot-set entry the fleet has not yet
  fetched is not eagerly evicted. The CommitAdoption Step 3a
  upsert specifically preserves NULL when the row was inserted
  by a pre-warm (no `last_requested_at = excluded.last_requested_at`
  in the DO UPDATE).
- `last_requested_at < now - gc.url_path_ttl` — outside the TTL
  window.
- The `(canonical_scheme, canonical_host, path)` triple does
  **not** appear in any `package_hash` row whose `snapshot_id`
  is in `(SELECT current_snapshot_id FROM suite_freshness WHERE
  current_snapshot_id IS NOT NULL)`. A path declared by any
  current snapshot's `Packages*` member is protected — it's part
  of the live working set the cache vouches for. Displaced
  snapshots' `package_hash` rows do **not** protect (those
  snapshots are themselves eligible for §9.6.3 reaping).

Per-batch: the writer-tx selects up to `gc.batch_size`
candidates, then per-row issues `DELETE FROM url_path` followed
by the same `UPDATE blob SET refcount = refcount - 1,
refcount_zeroed_at = COALESCE(refcount_zeroed_at, IIF(refcount -
1 <= 0, ?, NULL)) WHERE hash = ?` that EvictURLPath uses (Rule
3, SPEC4 §7.5.1) — preserving the existing grace-clock semantic
across both eviction paths. Per-row rather than bulk DELETE
because we need to skip refcount decrement when a concurrent
EvictURLPath wins the row between SELECT and DELETE (idempotency
guard inherited from EvictURLPath).

`gc.url_path_ttl = 0s` short-circuits the pass entirely
(`runURLPathPass` returns immediately); the per-tick deadline
is unaffected by the pass when disabled. This is the only
operationally-meaningful 0 value for any `gc.*` duration —
unlike `blob_grace = 0s` (unsafe; rejected at load), a 0 TTL
restores the Phase 1-3 behavior where url_path rows are
permanent unless serve-time hash-mismatch evicts them.

Telemetry: per-tick `url_path_rows_reaped` in `gc_run_complete`
(both startup and periodic phases); per-process
`acu_gc_url_path_rows_reaped_total` counter. The status page's
gc block carries the most-recent tick's count under
`gc.url_path_rows_reaped`.

Migration: no schema change. The Phase 1 schema's
`idx_url_path_last_req` (`CREATE INDEX idx_url_path_last_req ON
url_path(last_requested_at)`) already provides the b-tree the
SELECT predicate orders by; the additional NOT EXISTS subquery
runs against `package_hash`'s `(canonical_scheme,
canonical_host, path, snapshot_id)` primary key. Both indexes
pre-date Phase 4.

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
  - `blobs_reaped` — count of blob rows DELETEd this run by the
    §9.6.2 writer-tx COMMIT. This counts *DB* state and is
    independent of whether the post-COMMIT `os.Remove` succeeded;
    a non-`ErrNotExist` unlink failure leaks a pool/ file but does
    NOT resurrect the row, so the row is still counted as reaped
    (the leak is reported separately under `pool_unlink_errors`,
    and the next §9.6.4 pool scan repairs it).
  - `bytes_reclaimed` — sum of `blob.size` for those rows. Same
    semantics as `blobs_reaped`: counts what the DB COMMIT removed,
    independent of unlink result.
  - `orphan_candidates_reaped` — count of `suite_snapshot` rows
    DELETEd via §9.6.3 sub-job A
  - `displaced_reaped` — count of `suite_snapshot` rows DELETEd
    via §9.6.3 sub-job B
  - `pool_orphans_repaired` — count of pool/ files unlinked by
    the §4.2 startup scan (zero on `phase=periodic`)
  - `pool_orphan_bytes_repaired` — corresponding byte count
  - `pool_unlink_errors` — count of unlink errors (other than
    `ErrNotExist`) encountered this run
  - `deadline_reached` — bool, true if the §9.6.1 per-tick
    deadline fired in *either* the snapshot pass (§9.6.3) or
    the blob pass (§9.6.2) before that pass's candidate set
    drained (i.e. there is residual backlog the next tick will
    pick up). The tick's `gc_tick_deadline_reached` event names
    which sub-pass tripped via its `which` field.
  - `duration_ms` — wall-clock for the run

  The empty-tick case (`blobs_reaped=0 orphan_candidates_reaped=0
  displaced_reaped=0`) still emits the line: an operator scripting
  monitoring on "GC tick cadence" can use the line as a heartbeat.

- **`gc_tick_deadline_reached`** Info when the per-tick wall-clock
  budget (`gc.max_tick_duration`) trips between batches in
  *either* the snapshot pass or the blob pass. Fields:
  `phase` (`startup` / `periodic`), `which` (`snapshot` / `blob`
  — names which sub-pass was running when the deadline tripped),
  `batches_completed`, `bytes_reclaimed_this_tick`. Emitted
  *before* `gc_run_complete`; the same tick's
  `gc_run_complete.deadline_reached` is also `true`. Repeated
  occurrences across consecutive ticks indicate a steady-state
  reap rate insufficient for the pool's churn — operator should
  raise `gc.batch_size` (blob) / `gc.snapshot_batch_size` (or
  `gc.max_tick_duration`) and/or lower `gc.interval`.

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

- **`adoption_heartbeat_failed`** Warn when
  `cache.HeartbeatSnapshot` (§7.5.2) fails (e.g. fs full,
  database locked beyond timeout). Fields: `snapshot_id`, `err`.
  Adoption continues regardless; the next heartbeat (or the
  successful `CommitAdoption` that flips `adopted_at`) restores
  liveness signal. Repeated failures ahead of an in-flight
  adoption may make GC reap the candidate; this is the operator-
  visible signal of that risk.

- **`adoption_heartbeat_blobs_failed`** Warn when the §7.5.2
  site 6 ticker's `cache.HeartbeatBlobs` UPDATE fails. Fields:
  `snapshot_id`, `hash_count`, `err`. Same operational meaning
  as `adoption_heartbeat_failed` but for the *in-flight blob*
  grace clocks (the candidate row's heartbeat is on a separate
  write so the two failure modes are distinguishable). Repeated
  failures ahead of `CommitAdoption` Step 4 risk having a
  member blob aged past `gc.blob_grace` and reaped before its
  FK reference lands; the loud Warn line is the early signal.

- **`gc_heartbeat_interval_unsafe`** is the named error class
  surfaced by config validation when either of two cross-key
  bounds is violated:
  - `gc.heartbeat_interval >= heartbeat_stale_grace_effective`
    (= `max(upstream.total_timeout × upstream.max_retries, 30m)`)
    — a heartbeat ticker that ticks slower than the candidate
    grace can't bound the heartbeat-gap, so the §9.6.3 reap
    predicate's safety argument collapses.
  - `2 × gc.heartbeat_interval >= gc.blob_grace` — the §7.5.2
    site 6 ticker's `HeartbeatBlobs` write would not refresh
    in-flight member blobs often enough to survive a single
    missed heartbeat. The 2× factor accounts for one transient
    failure (stall, DB lock, etc.) without the blob aging past
    the §9.6.2 reap predicate before `CommitAdoption` Step 4
    lands.

  The validation error string includes both the configured
  `heartbeat_interval` and the violated bound, so the operator
  sees exactly which two values triggered rejection. Loaders
  reject the config; the daemon does not start (i.e. the error
  reaches the operator via the daemon's startup-failed exit
  message rather than via a structured slog event — the config
  logger is not yet installed at validation time). Refusing to
  start is safer than starting with a configuration that can
  silently reap live adoptions.

- **`gc_pool_scan_dir_failed`** Warn when the §9.6.4 startup
  scan can't read a `pool/<prefix>/` directory (e.g. EACCES,
  EIO). Fields: `prefix`, `err`. The scan continues with the
  remaining prefixes; the operator-visible signal is the Warn
  line plus a non-zero `pool_unlink_errors`-equivalent count
  on the next `gc_run_complete` (or, if scan failures dominate,
  a `pool_orphans_repaired` count smaller than the disk's true
  orphan count — only addressable manually).

- **`gc_pool_misplaced_file`** Warn when the §9.6.4 startup
  scan finds a file at `pool/<prefix>/<hash>` where
  `prefix != hash[:2]`. Fields: `path` (relative to `pool/`),
  `expected_prefix`, `actual_prefix`. The file is left in
  place — automatic repair (move-and-overwrite, or unlink) has
  too many ways to lose a still-referenced blob to the request
  path. Operator decides what to do.

### 10.3 Startup config dump (additions)

Phase 1's startup config dump (Phase 1 §10.3 carry-forward) adds
the `[gc]` block (all nine keys: `enabled`, `interval`,
`batch_size`, `snapshot_batch_size`, `max_tick_duration`,
`blob_grace`, `keep_displaced`, `pool_scan_workers`,
`heartbeat_interval`) verbatim, with one synthesized field:

- `gc.heartbeat_stale_grace_effective` — the runtime-derived
  grace `max(upstream.total_timeout × upstream.max_retries, 30m)`
  used by §9.6.3 sub-job A's stale-heartbeat reap predicate, so
  the operator can read the actual value without computing it
  themselves.

---

## 11. Failure-mode catalog (deltas)

Phase 1 + Phase 2 + Phase 3 catalog (SPEC §11 / SPEC2 §11 /
SPEC3 §11) carries forward exactly. Phase 4 adds:

| Condition | Behavior |
|---|---|
| GC DELETE commits, process killed before `os.Remove` | `pool/<hash>` file orphans. Caught by next startup §4.2 scan. No correctness impact; minor disk leak bounded by *time to next restart*. |
| GC SELECT finds candidates, parallel adoption / EvictURLPath / new url_path insert mutates one before DELETE runs | The DELETE's full WHERE predicate (`refcount <= 0` AND three NOT EXISTS reachability clauses) is re-evaluated atomically with the DELETE; rows that became reachable are filtered out. The DELETE's RETURNING clause yields exactly the rows actually removed; the §9.6.2 buffer-then-commit-then-unlink ordering means only those rows' files are unlinked. |
| Tx commit failure / rollback after RETURNING rows are buffered | The DB rolls back the DELETE atomically; no `os.Remove` calls have run yet (§9.6.2 step 5 is post-COMMIT). Pool stays consistent with DB. The next tick re-attempts the same candidates. |
| Reused orphan blob: PutBlob conflict → window before FK-bearing INSERT lands → GC sees stale `refcount_zeroed_at` past grace | The §7.5.1 Rule 1 `ON CONFLICT DO UPDATE` refreshes `refcount_zeroed_at = now` whenever an orphaned blob (`refcount <= 0`) is reused; the freshly-restarted grace clock guarantees a full `gc.blob_grace` window for the FK insert to land before reap is eligible. |
| `os.Remove` on reaped blob fails with EPERM / EROFS | `gc_pool_unlink_failed` Warn. DB row already DELETEd. File leaks until next startup scan. Operator-visible signal: the Warn line plus a non-zero `pool_unlink_errors` field on the next `gc_run_complete`. |
| Orphan candidate snapshot whose adoption stops heartbeating past the grace, then resumes and tries to commit | `CommitAdoption`'s final `UPDATE suite_snapshot SET adopted_at = ?` would update zero rows on a reaped candidate; downstream FK-bearing INSERTs fail. The adoption transaction rolls back; bytes already in `pool/` keep their refcount and become reapable on a later pass once nothing references them. The adoption goroutine logs `adoption_run_failed` Warn. The heartbeat-based grace makes this race vanishingly rare: stalls must exceed `max(total_timeout × max_retries, 30m)` of *no heartbeat updates*, not total adoption duration. |
| Repeated `adoption_heartbeat_failed` for the same in-flight adoption | The candidate's `heartbeat_at` ages; once stale-grace elapses the candidate is reapable. If the adoption resumes and writes a successful heartbeat before reap fires, the row stays. The Warn line is the operator-visible early signal; `gc.blob_grace` and `keep_displaced` decisions can be informed by its rate. |
| Snapshot GC SELECT picks ID `S`; concurrent adoption tries to heartbeat or commit `S` between SELECT and DELETE | Cannot occur under §9.6.3's single-writer-tx ordering: the SELECT and the three cascade DELETEs run in the same `submitWrite` op, and `HeartbeatSnapshot` / `CommitAdoption` / `InsertCandidateSnapshot` all serialize through the same writer goroutine. The interleaving's heartbeat write either lands before this tx (SELECT excludes `S`) or after this tx (DELETE has already removed `S`; the heartbeat UPDATE matches zero rows — benign). |
| Per-tick wall-clock budget (`gc.max_tick_duration`) trips during snapshot pass | Snapshot pass exits between batches. `gc_tick_deadline_reached{which="snapshot"}` Info + `gc_run_complete.deadline_reached = true`. Blob pass receives an already-expired deadline and exits immediately. Next tick re-runs both. |
| Per-tick wall-clock budget trips during blob pass | Blob pass exits between batches. `gc_tick_deadline_reached{which="blob"}` Info + `gc_run_complete.deadline_reached = true`. Next tick picks up the residual. |
| Pool/ scan worker fails to read a `pool/<prefix>/` directory | Per-worker error; logged at Warn under `gc_pool_scan_dir_failed` (§10.2) with `prefix` and `err`. The scan continues with other prefixes. |
| Pool/ scan worker finds a file at `pool/<prefix>/<hash>` whose name is a valid sha256-hex but whose prefix does not equal `hash[:2]` | Logged at Warn under `gc_pool_misplaced_file` (§10.2) with `path`, `expected_prefix`, `actual_prefix`. File is left in place; the operator decides whether to move (to its hash[:2]-named prefix) or unlink. The mismatched file is unreachable by `Cache.BlobPath(hash)`, so it cannot satisfy a request even if a `blob` row exists for that hash. |
| Migration v3 → v4 interrupted | Tx rolls back; next start retries from `schema_version = 3`. |

---

## 12. Test strategy (deltas)

Phase 1 + Phase 2 + Phase 3 test strategy (SPEC §12 / SPEC2 §12 /
SPEC3 §12) carries forward exactly. Phase 4 adds:

### 12.1 Unit tests (additions)

- **`refcount_zeroed_at` maintenance, all three rules.** Goldens
  for `PutBlob` (sets to `created_at` on insert), `CommitAdoption`
  Step 4 (clears on transition to >0, preserves on -1→0), Step 8 +
  `EvictURLPath` (sets on first ≤0 crossing, preserves on 0→-1).
  Each rule has at least one golden for each transition.
- **`PutBlob` ON CONFLICT DO UPDATE.** Three goldens for the
  conflict path:
  - existing row at refcount=0 with old `refcount_zeroed_at`
    (e.g. `now - 1h`); a fresh `PutBlob` with the same hash and
    `now`-valued args advances `refcount_zeroed_at` to `now`,
    leaves `refcount`, `size`, and `created_at` untouched;
  - existing row at refcount=5 (positive) with NULL
    `refcount_zeroed_at`; a fresh `PutBlob` leaves all columns
    untouched (the conflict's `WHERE blob.refcount <= 0` filter
    skips the UPDATE entirely — verified by reading the row
    after the call and asserting `refcount_zeroed_at IS NULL`
    AND `created_at` unchanged);
  - existing row at refcount=-1 (transient negative) with old
    `refcount_zeroed_at`; the conflict update advances it to
    `now` (the `<= 0` filter matches negative).
- **GC reap predicate, full reachability.** The §9.6.2 SELECT
  returns the right candidate set across:
  - refcount=0, zeroed_at = now-grace+1s — excluded (grace)
  - refcount=0, zeroed_at = now-grace-1s — included
  - refcount=0, zeroed_at IS NULL — excluded (legacy guard)
  - refcount=-1, zeroed_at = now-grace-1s — included
  - refcount=1, zeroed_at = now-grace-1s — excluded
  - refcount=0, eligible by clock, but a `url_path.blob_hash`
    references it — excluded (NOT EXISTS, served by
    `idx_url_path_blob`)
  - refcount=0, eligible by clock, but a `snapshot_member`
    references it — excluded (NOT EXISTS)
  - refcount=0, eligible by clock, but a
    `suite_snapshot.inrelease_hash` / `release_hash` /
    `release_gpg_hash` references it — excluded (NOT EXISTS;
    one golden per FK column)
- **GC DELETE...RETURNING ordering.** Two goldens:
  - SELECT-then-mutate-then-DELETE race: the candidate hash list
    passed to the DELETE includes one row whose refcount has
    been bumped back > 0 between phases; the RETURNING result
    excludes that hash; the in-memory buffer (which feeds the
    unlink loop) does not include the survivor.
  - Buffer-then-close-then-commit-then-unlink ordering: a fault
    injected at the post-DELETE pre-COMMIT point (forced
    rollback) leaves both DB rows and pool files untouched;
    ditto a fault at the post-COMMIT pre-unlink point leaves
    the DB consistent and produces a §4.2-recoverable orphan
    file. (Fault injection here is at the test-harness level —
    a hook between `tx.Commit()` returning and the unlink loop
    starting.)
- **Snapshot GC SELECTs.** Goldens for the orphan-candidate query
  (`heartbeat_at` predicate; correct grace arithmetic
  `max(total_timeout × max_retries, 30m)` from runtime config)
  and the displaced-snapshot query:
  - 5 adopted snapshots in one suite (1 current, 4 displaced),
    `keep_displaced = 3` → exactly 1 row reaped (the oldest
    displaced); the current is preserved.
  - `keep_displaced = 0` → all 4 displaced reaped; current
    preserved.
  - `keep_displaced = 0` with no current snapshot for the suite
    (suite_freshness has NULL current_snapshot_id) → all 5
    adopted reaped (correct: they're all displaced relative to
    "no current").
  - 3 snapshots with identical `adopted_at` and ascending
    `snapshot_id` (1, 2, 3); `keep_displaced = 1` → snapshots 1
    and 2 reaped; snapshot 3 preserved (the tie-break
    `snapshot_id DESC` makes 3 rank 1).
- **Snapshot GC batching.** Golden that the §9.6.3 reap loop,
  given a synthetic backlog larger than
  `gc.snapshot_batch_size × n_batches` fitting in the deadline,
  drains in multiple batches; each batch's per-tx
  `RowsAffected` sums to the total reaped count; the
  `gc_run_complete` line names the right `orphan_candidates_reaped`
  + `displaced_reaped` counts (separated by `reap_class` from
  the candidate-id select).
- **`HeartbeatSnapshot` semantics.** Goldens that
  `cache.HeartbeatSnapshot(ctx, snapshot_id)` updates only
  `heartbeat_at`, leaves all other columns untouched (especially
  `adopted_at`), and is a no-op (zero rows updated) on a
  snapshot_id that has been reaped. One golden per call site
  (post-member-fetch, post-buildPackageHashes, post-deb-fetch in
  the hot loop, pre-CommitAdoption) verifies the heartbeat fires
  exactly once per site.
- **Periodic heartbeat ticker.** Golden that the §7.5.2 site-6
  ticker, given a synthetic adoption that runs for
  `4 × gc.heartbeat_interval`, results in at least 3 ticker-
  emitted `HeartbeatSnapshot` writes (allowing for one
  start-of-run skew); the writes are observable in
  `suite_snapshot.heartbeat_at` advancing monotonically; the
  ticker exits cleanly when the adoption returns (no leaked
  goroutine — verified via `runtime.NumGoroutine` delta). A
  second golden injects a `runShared` that returns immediately
  and asserts the ticker either never ticks or ticks at most
  once (cancellation race window) — and exits.
- **`gc.heartbeat_interval` cross-key validation.** Goldens
  that `Validate` rejects:
  - `heartbeat_interval = 0s` → `gc.heartbeat_interval must be > 0`
  - `heartbeat_interval = 30m` with `upstream.total_timeout × max_retries = 5m`
    (so `grace_effective = 30m`) → `gc.heartbeat_interval must
    be strictly less than heartbeat_stale_grace_effective` (the
    Error event `gc_heartbeat_interval_unsafe` fires at load).
  - `heartbeat_interval = 60s` with default upstream config
    (`grace_effective = 30m`) → accepted.
- **Pool prefix-mismatch detection.** Golden that the §9.6.4
  startup scan, given a file at `pool/00/abcd...` (where the
  hash starts with `ab` and the prefix is `00`), emits exactly
  one `gc_pool_misplaced_file` Warn with the right `path`,
  `expected_prefix=ab`, `actual_prefix=00`; the file is NOT
  unlinked; the corresponding `blob` row (if any) is left
  alone; subsequent `BlobPath(hash)` resolves to the
  `pool/ab/abcd...` location regardless.
- **Migration v3 → v4.** Apply against a Phase 3 snapshot, verify
  schema; idempotent re-apply is a no-op; an interrupted migration
  rolls back cleanly; the backfill UPDATEs correctly populate
  `refcount_zeroed_at = created_at` for pre-v4 ≤0 rows AND
  `heartbeat_at = created_at` for pre-v4 candidate rows; both
  partial indexes (`idx_blob_gc`, `idx_url_path_blob`) are
  present after migration with their partial WHERE clauses
  preserved (verifiable via `sqlite_master.sql`).
- **Per-tick deadline (blob pass).** Golden that the §9.6.2 reap
  loop, given a synthetic backlog larger than `batch_size ×
  n_batches` fitting in the deadline, exits cleanly and emits
  `gc_tick_deadline_reached{which="blob"}`. Subsequent tick drains
  the remainder.
- **Per-tick deadline (snapshot pass).** Golden that the §9.6.3
  reap loop, given a synthetic snapshot backlog that exceeds
  the deadline mid-pass, exits cleanly and emits
  `gc_tick_deadline_reached{which="snapshot"}`. The same tick's
  blob pass receives an already-expired deadline and exits
  immediately with no batches run.
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

Property: a blob whose refcount or FK-reachability changes
during an in-flight adoption is *never* reaped if the change
makes it reachable, even if the change arrives between GC's
SELECT and GC's DELETE — and a freshly-arriving fetch that
re-uses an orphan blob never sees that blob disappear under it.
The §9.6.2 DELETE's full WHERE predicate (refcount + 3 NOT EXISTS
clauses) is the gate against the SELECT/DELETE race; the §7.5.1
Rule 1 `ON CONFLICT DO UPDATE` is the gate against the
PutBlob/FK-INSERT race. The buffered RETURNING clause yields
exactly the rows actually removed and is the only source of
truth for which files to unlink.

Driver, four variants exercised under the same harness:

**Variant A — refcount bump.**
1. Set up state where blob `B` has refcount=0,
   `refcount_zeroed_at < now - grace`, no FK references.
2. Block GC's DELETE statement at a fault-injection point (after
   SELECT, before transaction begin).
3. Run a parallel adoption that bumps `B.refcount` to 1 (commits
   a transaction that updates `B` per §7.5.1 rule 2).
4. Release GC's DELETE.
5. Assert `B`'s row still exists; `B`'s file still exists; the
   GC's RETURNING-buffered result did NOT include `B`.

**Variant B — `url_path` insert during the race.**
Same scaffolding, but the parallel writer instead inserts a new
`url_path` row pointing at `B` (no refcount bump). Assert the
NOT EXISTS clause filters `B` out of the DELETE; `B` is not
unlinked.

**Variant C — adoption aborts.**
Same scaffolding, but the parallel adoption *aborts* (the
goroutine cancels before `CommitAdoption`'s commit). Assert no
bump and no FK reference landed, and `B` is reaped per the
normal reap path; `B` appears in the RETURNING-buffered result;
the file is unlinked.

**Variant D — orphan-blob reuse via PutBlob conflict.**
1. Set up state where blob `B` has refcount=0,
   `refcount_zeroed_at < now - grace`, no FK references — i.e.
   `B` would be reapable if a GC tick fired right now.
2. Issue a `PutBlob` with `B`'s hash from a parallel goroutine
   (simulating a §6.2 cache miss or §7.5 adoption member-fetch
   that re-uses content already in pool). The conflict path
   refreshes `refcount_zeroed_at = now`.
3. Trigger a GC tick *before* the simulated FK-bearing INSERT
   commits (i.e. with `B` still at refcount=0 and no FK
   references — but with refreshed grace).
4. Assert `B` is NOT reaped (the §9.6.2 SELECT predicate
   `refcount_zeroed_at < now - grace` rejects the candidate
   because `now - grace < now`, the just-refreshed clock).
5. Then commit the FK-bearing INSERT, advance the test clock
   past `gc.blob_grace`, run another GC tick, and assert `B`
   is still alive (Rule 2 cleared `refcount_zeroed_at` to NULL
   when refcount went 0 → 1; the partial index excludes it
   from the candidate set).

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

A second variant covers the SELECT-DELETE liveness race: with
snapshot `S` at `heartbeat_at < now - grace` and no current
reference, fire a parallel `HeartbeatSnapshot(S)` and a
parallel `CommitAdoption` flip on `S` *while* the GC batch's
writer-tx is queued. Assert (a) the heartbeat / flip lands
either strictly before or strictly after the GC tx (never
mid-tx — the single-writer model precludes it), and (b) `S`
either survives reap (heartbeat won the race) or is reaped
(heartbeat lost) — with no observable in-between state.

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
the table (steady-state reap keeps the candidate set small);
assert `idx_url_path_blob` partial index size grows roughly
linearly with the count of url_path rows that have a non-NULL
blob_hash (i.e. the partial WHERE keeps it from including
unfetched-URL rows).

---

## 13. Project layout & tooling (deltas)

Phase 1 + Phase 2 + Phase 3 layout carries forward. Phase 4 adds
one new package and modifies three existing ones:

```
internal/
  gc/                    # NEW package
    gc.go                # the goroutine + tick loop + orchestration;
                         # per-tick deadline derived from gc.max_tick_duration
    blob.go              # §9.6.2 blob GC pass (full reachability
                         # predicate + DELETE...RETURNING)
    snapshot.go          # §9.6.3 snapshot GC pass
    pool_scan.go         # §9.6.4 startup pool/ orphan scan
    gc_test.go           # unit tests for §12.1
    integration_test.go  # integration tests for §12.2
    chaos_crash_mid_batch_test.go  # §12.6 chaos test
                         # (the gc-package-level chaos test;
                         # §12.3–§12.5 chaos tests live in
                         # internal/cache/ — see below — because
                         # they exercise package-private seams
                         # like blobGCInterTxHook and stubNow)
  cache/
    gc.go                # NEW — writer-tx handlers for the gc
                         # package's sub-job calls:
                         # RunBlobGCBatch (§9.6.2), inter-tx
                         # SELECT-vs-DELETE seam blobGCInterTxHook
                         # for §12.3 chaos tests, and
                         # RunSnapshotGCBatch (§9.6.3)
    schema.go            # migrations[3] (v3 → v4) appended:
                         # adds refcount_zeroed_at, heartbeat_at,
                         # idx_blob_gc, idx_url_path_blob;
                         # CurrentSchemaVersion bumped to 4
    queries.go           # PutBlob INSERT extended to ON CONFLICT
                         # DO UPDATE refresh refcount_zeroed_at
                         # when refcount<=0 (§7.5.1 rule 1);
                         # NEW HeartbeatSnapshot helper (§7.5.2)
    adoption.go          # CommitAdoption Step 4 + Step 8 SQL
                         # extended (§7.5.1 rules 2 + 3);
                         # EvictURLPath SQL extended (rule 3);
                         # InsertCandidateSnapshot extended to set
                         # heartbeat_at on insert and on the reuse
                         # path's refresh UPDATE (§7.5.2)
    gc_test.go           # cache-side unit tests for the §9.6.2 /
                         # §9.6.3 writer-tx handlers (subset of §12.1)
    chaos_blob_gc_race_test.go     # §12.3 chaos test (4 variants)
    chaos_evict_race_test.go       # §12.4 chaos test
    chaos_snapshot_race_test.go    # §12.5 chaos test (2 variants)
  freshness/
    adoption.go          # heartbeats at six §7.5.2 sites:
                         # 1) row creation (delegated to cache),
                         # 2) post-adoptMember (in the member loop),
                         # 3) post-buildPackageHashes,
                         # 4) post-deb-fetch in runHotPrefetch,
                         # 5) pre-CommitAdoption,
                         # 6) periodic gc.heartbeat_interval
                         #    ticker: a sidecar goroutine
                         #    launched at runShared start,
                         #    cancelled when runShared returns
  config/
    config.go            # [gc] block decoder + validation
                         # (9 keys including snapshot_batch_size
                         # and heartbeat_interval, with a
                         # cross-key guard that
                         # heartbeat_interval <
                         # heartbeat_stale_grace_effective)
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
   consecutive runs with no flakes for all four variants
   (refcount bump, url_path insert, adoption abort, orphan-blob
   reuse via PutBlob conflict).
3. The Phase 4 GC-vs-EvictURLPath chaos test (§12.4) passes 10
   consecutive runs.
4. The Phase 4 GC-vs-displacement chaos test (§12.5) passes 10
   consecutive runs for both variants (concurrent displacement
   + the SELECT-DELETE liveness race).
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
