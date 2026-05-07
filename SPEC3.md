# apt-cacher-ultra — Phase 3 Specification

This document specifies the contract for Phase 3: hot-package proactive
refresh and opt-in `.deb` hash-validation strict mode. It is a delta
over [SPEC.md](SPEC.md) (Phase 1) and [SPEC2.md](SPEC2.md) (Phase 2).
Sections that carry forward unchanged say so explicitly and point at
SPEC.md / SPEC2.md; sections that change describe only the delta. The
companion document [PHASE-3-SCOPING.md](PHASE-3-SCOPING.md) records the
design rationale and the Q1–Q18 decisions that produced this spec.

Phase 3 is purely additive over Phase 2: a Phase 2 cache directory can
be upgraded in place by starting a Phase 3 binary against it; existing
snapshots and blobs serve unchanged through the migration, and
hot-package proactive refresh first kicks in on the *second* snapshot
transition after upgrade (see §4.3.2 migration and §7.5).

---

## 1. Goals & non-goals

### 1.1 Phase 3 goals

1. **Hot-package proactive refresh.** When a freshness check observes a
   new `InRelease`/`Release` for a suite that has at least one prior
   snapshot, the adoption flow identifies the `.deb` packages that
   clients have been requesting in the recent past, prefetches their
   new-snapshot versions into `pool/`, and only then performs the
   atomic flip. The url_path rows for the warmed debs are inserted
   inside the same SQLite transaction that flips
   `current_snapshot_id`, so readers never observe a warmed deb while
   the prior snapshot is still current. Best-effort under a
   configurable wall-clock budget: when a hot deb's upstream is
   unavailable, the flip still proceeds and the missing deb falls
   back to the normal cache-miss path on first request.
2. **`.deb` hash-validation strict mode (opt-in).** Phase 2 left
   unvouched `.deb` requests on the trust-upstream code path. Phase 3
   adds an opt-in `integrity.refuse_unvouched_debs` flag (default
   `false`) that refuses unvouched `.deb` GETs with `502` once a host
   has *provably complete* `package_hash` coverage. Two prerequisites
   ship alongside: `Packages.xz` parser support, and a per-snapshot
   `suite_snapshot.package_coverage_complete` boolean recorded at
   adoption time so the strict rule keys on coverage proof, not
   merely on row count.

### 1.2 Phase 3 non-goals (deferred)

Carried forward from earlier phases:

- Garbage collection of orphaned blobs from displaced snapshots
  (Phase 4 — the snapshot model produces orphans by design).
- Status page / `/metrics` endpoint (Phase 5).
- TLS MITM listener (Phase 6).
- Source-package caching, multi-arch beyond amd64, pdiff (Phase 6+).

Explicitly resolved during Phase 3 scoping:

- **`by-hash` routing for adopted suites.** Phase 2 already inserts
  `snapshot_member` rows for every member's by-hash alias path, and
  `trySnapshotHit` resolves through `snapshot_member` before
  `url_path`. By-hash GETs on adopted suites are already fast; no
  Phase 3 work. (Phase 2 Q7 carry-over, reclassified as already-done.)
- **Operator-triggered manual adoption** (admin endpoint or SIGHUP).
  Deferred to Phase 6+ as an optional enhancement.
- **Streaming-while-fetching as a singleflight optimization** and
  **per-byte upstream read timeouts.** Both deferred to
  [FUTURE-REVIEW.md §1](FUTURE-REVIEW.md). Both require multi-user,
  weeks-to-months observational data to characterize the signal.
- **Per-suite freshness cadence variation.** Deferred to
  [FUTURE-REVIEW.md §2](FUTURE-REVIEW.md).

### 1.3 Default-flip of `integrity.refuse_unvouched_debs`

Flipping the default of `integrity.refuse_unvouched_debs` from
`false` to `true` is *not* a Phase 3 goal. It is gated on real-world
coverage data: do production deployments observe
`unvouched_deb_passthrough_no_coverage` log lines (§10), and if so,
which upstream layouts produce them? Until that data is in, the
default is opt-in and a future-phase decision.

---

## 2. Wire contracts

### 2.1 Listener
Unchanged — see SPEC.md §2.1.

### 2.2 Proxy mode
Unchanged — see SPEC.md §2.2.

### 2.3 The `http://HTTPS///` convention
Unchanged.

### 2.4 Mirror mode
Unchanged.

### 2.5 Range requests
Unchanged.

### 2.6 HTTP methods
Unchanged.

### 2.7 Response headers (deltas)

Phase 2's `X-Cache-Snapshot` carries forward exactly. Phase 3 adds no
new response headers. The hot-package prefetch happens entirely on
the adoption goroutine and is invisible at the wire level — clients
hitting a warmed `.deb` post-flip see `X-Cache: HIT` exactly as they
would for any other cache hit.

---

## 3. URL canonicalization (Remap)
Unchanged — see SPEC.md §3.

---

## 4. Storage layout

### 4.1 Disk
Unchanged — see SPEC2 §4.1. Phase 3 hot-prefetch fetches go through
the same `tmp/` workspace as Phase 2 metadata-member fetches via
`NewTempBlob()`; the Phase 1 startup `tmp/` sweep continues to
reclaim abandoned hot-prefetch downloads from a previous crash.

### 4.2 Startup cleanup
Unchanged — see SPEC2 §4.2.

### 4.3 SQLite schema

Phase 3 schema is `schema_version = 3`. Migration v2 → v3 is
described in §4.3.2.

#### 4.3.1 Phase 3 schema delta

Existing-table changes:

```sql
-- Hot-set matching across snapshot transitions matches by binary
-- (package_name, architecture). Defaults are empty strings on the
-- ALTER so pre-v3 rows survive untouched. The hot-set query in §7.5.3
-- excludes rows with empty values explicitly.
ALTER TABLE package_hash ADD COLUMN package_name  TEXT NOT NULL DEFAULT '';
ALTER TABLE package_hash ADD COLUMN architecture  TEXT NOT NULL DEFAULT '';

-- Index ordering: (canonical_scheme, canonical_host, snapshot_id,
-- package_name, architecture). snapshot_id leads the trailing tuple
-- because the Stage-2 hot-set query (§7.5.3) filters by candidate
-- snapshot, and the same (Package, Arch) pair appears across many
-- snapshots over time. Without snapshot_id in the index, Stage-2
-- lookups would index on (scheme, host, package_name, architecture)
-- and then row-filter every match by snapshot_id — quadratic in
-- snapshot count.
CREATE INDEX idx_package_hash_pkg_arch
  ON package_hash(canonical_scheme, canonical_host,
                  snapshot_id, package_name, architecture);

-- Per-snapshot coverage proof for strict mode (§6.1, §7.5.4). Set by
-- adoption iff every Release-listed directory containing a Packages*
-- member had at least one parseable variant. Pre-v3 rows default to
-- 0 (treated as unverified by strict mode, fail-through).
ALTER TABLE suite_snapshot
  ADD COLUMN package_coverage_complete INTEGER NOT NULL DEFAULT 0
    CHECK (package_coverage_complete IN (0, 1));
```

No new tables. The hot-set state is derived at adoption time from
existing tables (`url_path`, `package_hash`); it is not persisted as
a separate "hot" table.

#### 4.3.2 Migration v2 → v3

```
migrations[2] = v2 → v3:
  ALTER TABLE package_hash ADD COLUMN package_name TEXT NOT NULL DEFAULT '';
  ALTER TABLE package_hash ADD COLUMN architecture TEXT NOT NULL DEFAULT '';
  CREATE INDEX idx_package_hash_pkg_arch ON package_hash(...);
  ALTER TABLE suite_snapshot ADD COLUMN package_coverage_complete
    INTEGER NOT NULL DEFAULT 0 CHECK (package_coverage_complete IN (0, 1));
  -- migrate.go bumps schema_version to 3 after success via the existing
  -- applyMigration UPDATE; the migration body must NOT include an
  -- INSERT or UPDATE on schema_version itself.
```

Properties:

- **Forward-only.** Phase 1's `migrate` already enforces this; v3 keeps
  the contract.
- **Pure DDL, no row rewrites.** The two `ALTER TABLE ADD COLUMN`
  statements complete in O(1) regardless of row count — SQLite stores
  the new columns' default values implicitly without rewriting
  existing rows.
- **Index creation scans existing rows.** The `CREATE INDEX
  idx_package_hash_pkg_arch` walks the existing `package_hash` table
  to build the B-tree. For typical Phase 2 deployments (a few suites
  × thousands of `.deb` paths × a handful of snapshots = O(100k)
  rows), index build is sub-second. For long-running caches with
  many active suites and accumulated snapshots, the row count can
  reach the millions and index creation takes tens of seconds.
- **Migration is startup-blocking.** The cache does not begin
  answering requests on `:3142` until the migration completes.
  Operators with very large caches should plan a maintenance window
  for the v2→v3 startup. A `schema_migrating` Info log line names
  the from/to versions at start; an operator scripting deploys
  against the journal can wait on that line plus the existing
  `schema migrated` Info line.
- **Atomic.** The migration framework runs each migration inside a
  single transaction (Phase 2 §4.3.2); an interrupted migration rolls
  back fully.
- **Pre-v3 `package_hash` rows are excluded from the hot-set query.**
  Empty `package_name` and `architecture` columns disqualify them
  via the Stage-1 predicate in §7.5.3. The first post-upgrade
  adoption populates name+arch on its candidate snapshot's rows;
  hot prefetch first kicks in on the *second* snapshot transition
  after upgrade. Pre-existing deployments lose at most one
  transition-cycle of warm-cache benefit.
- **Pre-v3 `suite_snapshot` rows have `package_coverage_complete = 0`.**
  Strict mode treats their hosts as fail-through, never fail-closed.
  Operators who want strict mode on a host with pre-v3 snapshots wait
  until the next adoption produces a v3 snapshot with the column
  populated.

### 4.4 Suite identification
Unchanged — see SPEC.md §4.4.

### 4.5 Classifying metadata vs. blob
Unchanged — see SPEC.md §4.5 / SPEC2 §4.5.

---

## 5. Configuration (TOML)

### 5.1 Example (deltas)

Existing sections (Phase 1 + Phase 2 keys) carry forward unchanged.
Phase 3 adds three keys:

```toml
[hot_packages]
# A .deb path is "hot" if a client has requested it within this
# window. Default 24h. A window of "0s" disables hot-package
# proactive refresh entirely (adoption falls back to Phase 2
# behavior). Presence-sensitive: the loader uses
# toml.MetaData.IsDefined to distinguish "key absent (use default)"
# from "operator wrote 0s explicitly" — see §5.2.
window = "24h"

[adoption]
# Existing keys (enabled, require_signature, require_pinned_signer)
# carry forward unchanged. Phase 3 adds:
hot_prefetch_budget = "5m"
# Wall-clock guard on the overall hot-prefetch phase. 0s = no
# wall-clock guard; hot prefetch runs until every hot deb has
# terminated (success or full-retry failure). Per-deb fetches still
# respect upstream.total_timeout × upstream.max_retries regardless
# of this setting; budget=0s does NOT mean "retry one deb forever."
# Startup warning emitted when set to 0s. Presence-sensitive (same
# handling as hot_packages.window).

[integrity]
# Existing keys (validate_at_rest_interval, validate_at_rest_workers)
# carry forward unchanged. Phase 3 adds:
refuse_unvouched_debs = false
# Opt-in strict mode for .deb requests. When true, refuse .deb GETs
# under a host whose current snapshots have proven complete coverage
# (every snapshot has package_coverage_complete = 1) but where the
# requested path is not vouched for. Hosts whose snapshots have
# package_coverage_complete = 0 fall back to trust-upstream
# regardless of this flag. Default false in Phase 3; default-flip to
# true is a future-phase decision (§1.3). Inert when
# adoption.enabled = false (startup warning emitted in that
# combination).
```

### 5.2 Config validation (deltas)

Phase 1 + Phase 2 validation carries forward. Phase 3 adds:

- `hot_packages.window` parses as duration, ≥ 0. Presence-sensitive:
  the loader applies the default of `24h` *before* `Defaults()` runs
  iff `toml.MetaData.IsDefined("hot_packages", "window")` returns
  false. Without this check, an operator-written `0s` would be
  clobbered to `24h` by `Defaults()` (the same pattern Phase 2 uses
  for `freshness.max_concurrent_adoptions`,
  `integrity.validate_at_rest_interval`, and
  `integrity.validate_at_rest_workers`).
- `adoption.hot_prefetch_budget` parses as duration, ≥ 0. Same
  presence-sensitive handling, default `5m`.
- `integrity.refuse_unvouched_debs` is bool, default `false`.

Loud configurations (warning logs at startup):

- `adoption.hot_prefetch_budget = "0s"` — names the unbounded
  worst-case wait (`hot_prefetch_budget_unbounded` Warn).
- `integrity.refuse_unvouched_debs = true` AND
  `adoption.enabled = false` — the strict flag is inert in this
  combination (`refuse_unvouched_debs_inert` Warn).

---

## 6. Request handling

### 6.1 The fast path: cache hit (deltas)

Phase 2's metadata fast path (`trySnapshotHit` then `tryURLPathHit`)
carries forward unchanged. Phase 3 modifies only the `.deb`
defense-in-depth check that runs after `tryURLPathHit` finds a row:

```
SPEC2 §6.1's existing flow (carried forward):
  1. row := SELECT blob_hash FROM url_path WHERE ...
  2. declared := DISTINCT declared_sha256 from package_hash for any
                 current snapshot covering this (host, path).
  3. Zero declared rows: → trust-upstream serve (Phase 1 fallback).
  4. One declared row matching row.blob_hash: serve.
  5. One declared row mismatching: evict url_path, fall through to §6.2.
  6. Two or more conflicting: 502 + log package_hash_conflict.

Phase 3 strict-mode delta inserts BEFORE step 3:

  2a. If integrity.refuse_unvouched_debs is true AND
      cfg.Adoption.Enabled is true AND the host has at least one
      current snapshot AND every current snapshot on the host has
      package_coverage_complete = 1 AND `declared` is empty (zero
      rows from step 2):
        return 502 + Retry-After: 60
        log unvouched_deb_refused (§10)
        do NOT fetch upstream

  2b. If integrity.refuse_unvouched_debs is true AND `declared` is
      empty AND the host has at least one current snapshot AND any
      current snapshot has package_coverage_complete = 0:
        log unvouched_deb_passthrough_no_coverage at most once per
        (host, path, hour)
        proceed to step 3 (trust-upstream serve)
```

The strict-mode predicate explicitly checks `cfg.Adoption.Enabled` —
flipping the master switch off is a deliberate operator return to
trust-upstream posture, and strict mode honors that even when stale
`current_snapshot_id` rows persist from prior runs (PHASE-3-SCOPING.md
Q17).

The same predicate also fires in the §6.2 miss path when Phase 1
fallback would otherwise insert an unvouched `.deb`. The miss path
checks `declared` after the fetch; Phase 3 adds an *upfront* check
of the same predicate before initiating the fetch, so a strict-mode
refusal does not consume upstream bandwidth.

### 6.2 Cache miss: singleflight fetch (deltas)

Phase 2 §6.2 carries forward. Phase 3 adds the upfront strict-mode
gate described in §6.1: a `.deb` miss whose canonical (host, path)
satisfies the strict-mode predicate (Adoption.Enabled, host has
fully-covered snapshot, path unvouched) returns `502` before the
upstream fetch is initiated.

For metadata under a suite with `current_snapshot_id`: unchanged —
Phase 2 §6.2 metadata recovery path continues to apply.

### 6.3 Resumable upstream fetch
Unchanged — see SPEC.md §6.3.

### 6.4 Cache miss with upstream down
Unchanged — see SPEC2 §6.4.

### 6.5 Hash validation
Unchanged — see SPEC2 §6.5. Phase 3's strict mode is layered *over*
hash validation, not in place of it: a `.deb` whose canonical path
*is* vouched for still validates against the declared hash on miss
(Phase 2 behavior); the strict mode only governs the *unvouched*
fallback.

### 6.6 Upstream allowlist
Unchanged — see SPEC.md §6.6.

---

## 7. Freshness and adoption

### 7.1 Triggers
Unchanged — see SPEC.md §7.1.

### 7.2 Algorithm
Unchanged — see SPEC2 §7.2.

### 7.3 Off the request path
Unchanged — see SPEC.md §7.3.

### 7.4 Periodic scheduler
Unchanged — see SPEC.md §7.4.

### 7.5 Adoption flow (Phase 3 deltas)

Phase 2 §7.5 carries forward as the adoption skeleton. Phase 3
inserts hot-package prefetch between the per-member fetch and the
atomic flip, and extends the flip transaction to also insert the
warmed debs' `url_path` rows.

```
runAdoption(suite, new_bytes, etag, lastmod, mode):

  // Phase 2 steps 0–4 unchanged: GPG verify, persist verified
  // bytes, parse, insert candidate suite_snapshot row.

  // Phase 2 steps 5–7 unchanged: prefetch + record metadata-member
  // and by-hash alias snapshot_member rows.

  // Phase 2 step 8 (parse Packages → package_hash) extends:
  //   - Parser also extracts Package and Architecture (§7.5.2).
  //   - buildPackageHashes records, per directory, whether at least
  //     one parseable variant was processed. The result is the
  //     candidate snapshot's package_coverage_complete value (§7.5.4).

  // Phase 3 step 9: compute hot set (§7.5.3). Empty if any of:
  //   - no prior current_snapshot_id for this suite,
  //   - hot_packages.window == 0s,
  //   - no eligible prior-snapshot package_hash rows have a
  //     fresh url_path.last_requested_at.

  // Phase 3 step 10: hot-deb prefetch loop. Sequential within this
  //   adoption (Phase 2 §3.2 sequential-members rule extended).
  prefetchedURLPaths := []
  log Info "adoption_hot_prefetch_started"
           (canonical_host, suite_path, hot_count, budget)
  // Use a SEPARATE prefetchCtx for the hot-prefetch loop only. Budget
  // expiry must cancel only hot fetches and then flip anyway — so
  // CommitAdoption below runs under the parent adoptionCtx (which is
  // tied to the freshness scheduler's LifetimeCtx and SIGTERM
  // cancellation, not to the hot-prefetch budget).
  prefetchCtx, prefetchCancel := adoptionCtx, func(){}
  if adoption.hot_prefetch_budget > 0s:
    prefetchCtx, prefetchCancel = withTimeout(adoptionCtx,
                                              adoption.hot_prefetch_budget)
  defer prefetchCancel()
  budgetElapsed := false
  for path, declared_sha256, upstream_url in hot_set:
    select on prefetchCtx.Done():
      // Distinguish "budget elapsed" from "shutdown cancellation".
      // adoption_hot_prefetch_partial fires ONLY on budget elapse,
      // listing the paths NOT YET ATTEMPTED at the moment of
      // cancellation. Per-deb failures and hash mismatches earlier
      // in the loop are reported via their own events
      // (hot_prefetch_deb_failed, hot_prefetch_hash_mismatch),
      // never via partial.
      if errors.Is(prefetchCtx.Err(), context.DeadlineExceeded):
        budgetElapsed = true
        log Warn "adoption_hot_prefetch_partial"
                 (canonical_host, suite_path, snapshot_id,
                  missing := remaining hot_set entries).
      // shutdown cancellation case: the parent adoptionCtx is also
      // cancelled, so CommitAdoption below would also fail. Phase 2
      // shutdown semantics (§9.5) apply — abandon the candidate.
      break.
    fetch via NewTempBlob → tmp/, using prefetchCtx. Per-fetch budget =
      upstream.total_timeout × upstream.max_retries (Phase 2 unchanged).
    on per-deb failure (full retries exhausted):
      log Warn "hot_prefetch_deb_failed" (path, err); continue to next.
    on hash mismatch with declared_sha256:
      discard the temp blob (do not promote to pool).
      log Warn "hot_prefetch_hash_mismatch" (path, declared, observed).
      continue to next.
    on success:
      Finalize promotes to pool/<declared_sha256> and inserts blob row.
      append (canonical_path, declared_sha256, upstream_url) to
        prefetchedURLPaths slice. Do NOT call PutURLPath here — the
        Phase 2 hit-path defense-in-depth check would observe a
        new blob_hash under the prior snapshot and trigger
        hit_path_hash_evicted (Phase 2 §6.1) on any concurrent GET.

  // Phase 3 step 11: atomic flip — extends Phase 2 §7.5.1.
  // Pass adoptionCtx (NOT prefetchCtx) so a budget-expired prefetch
  // does not cause CommitAdoption to fail. The contract is "cancel
  // hot fetches, then flip" — not "cancel hot fetches, then also
  // cancel the flip."
  CommitAdoption(adoptionCtx, snapshotID, members, packageHashes,
                 prefetchedURLPaths)
  // The new prefetchedURLPaths argument is inserted into url_path
  // inside the same transaction that flips current_snapshot_id.
  // Readers see all-or-nothing: either the new current_snapshot_id
  // is set AND the new url_path rows are visible, or neither.

  // Phase 3 step 12: log "adoption_hot_prefetch_complete"
  //   (canonical_host, suite_path, snapshot_id, hot_count,
  //    fetched, failed, mismatched).
  // Step continues with Phase 2 step 10's adoption_success.
```

The hot-prefetch loop is sequential within an adoption, bounded by
the existing `freshness.max_concurrent_adoptions` cap (Phase 2 §9.3.1)
that bounds total in-flight adoptions. No new semaphore.

#### 7.5.1 Atomic flip transaction (deltas)

Phase 2 §7.5.1 carries forward. Phase 3 extends the function
signature:

```go
CommitAdoption(ctx, snapshotID int64,
               members []SnapshotMember,
               packageHashes []PackageHash,
               prefetchedURLPaths []URLPath) error
```

The transaction body adds one step between the existing
`package_hash` insert (Phase 2 step 3) and the
`current_snapshot_id` flip (Phase 2 step 6):

```sql
-- Phase 3 step 3a: insert prefetchedURLPaths url_path rows (or update
-- in place if a row already exists at that path). Per inserted row:
INSERT INTO url_path
  (canonical_scheme, canonical_host, path, blob_hash, upstream_url,
   is_metadata, last_requested_at, request_count, last_fetched_at,
   upstream_etag, upstream_lastmod)
VALUES (?, ?, ?, ?, ?, 0, NULL, 0, ?, NULL, NULL)
ON CONFLICT(canonical_scheme, canonical_host, path) DO UPDATE SET
  blob_hash       = excluded.blob_hash,
  upstream_url    = excluded.upstream_url,
  last_fetched_at = excluded.last_fetched_at;
  -- last_requested_at and request_count are intentionally NOT in the
  -- DO UPDATE — preserve the prior row's hotness signal so the next
  -- adoption's hot-set computation (§7.5.3) still sees this path as
  -- still-hot.
```

This **deliberately diverges** from `PutURLPath`
(`internal/cache/queries.go:52-60`), which overwrites
`last_requested_at` and `request_count` from the new row's values.
Hot prefetch is a *cache-warming* write, not a *client-served* write
— the row's hotness signal lives across snapshot transitions and
must survive the upsert. Overwriting it on a hot prefetch would
erase the very evidence that made this path hot in the first place,
causing the next adoption's hot-set query to drop the path from the
hot set.

On a fresh INSERT (path not previously seen, common case for a new
package version's filename), the values clause sets
`request_count = 0` and `last_requested_at = NULL` — the new path
genuinely has zero client requests yet. On UPSERT (path matched a
prior row, e.g. an unversioned alias whose Filename stayed stable
across the version bump), the prior row's hotness columns are
preserved.

Phase 2 callers (`Run` and `RunDetached`) pass `nil` for
`prefetchedURLPaths`; the new slot is inert in their flow. Phase 3's
hot-prefetch caller passes a populated slice.

Refcount accounting: `PutURLPath` does not bump `blob.refcount` on
insert (the asymmetry documented in
`internal/cache/adoption.go:519-527`). Phase 3 honors the same
asymmetry — the prefetched url_path rows do not bump refcount; Phase 4
GC's `refcount <= 0` predicate continues to reap correctly.

#### 7.5.2 Packages parser additions

Phase 2's `ParsePackages` (`internal/freshness/packages_parse.go`)
extracts `Filename`, `SHA256`, and `Size` only. Phase 3 extends it
to also extract:

- **`Package`** — the *binary* package name (e.g. `nginx`,
  `linux-image-generic`). This is the apt `Package:` stanza field; it
  is not the source-package name (which lives in a separate optional
  `Source:` field). Hot-set matching is binary-package based — apt's
  URL routing keys on the binary package's filename, so the binary
  `Package` is the right identity for matching.
- **`Architecture`** — e.g. `amd64`, `arm64`, `all`.

Behavior for partial / conflicting fields:

- **Stanza missing `Package` or `Architecture`** but having
  `Filename` and `SHA256`: the row still populates `package_hash`
  for hash validation (its prior purpose), with `package_name = ''`
  and/or `architecture = ''`. Hot-set computation excludes such rows
  via the Stage-1 predicate (§7.5.3). Hash validation is unaffected.
- **Conflict across Packages variants**: SPEC2's contract is that
  variants are identical content. A real conflict (Packages.gz says
  `Architecture: amd64` for a Filename, Packages.xz says
  `Architecture: arm64` for the same) is an upstream pathology and
  treats the adoption as failed (`adoption_parse_failed`). Phase 3
  extends `buildPackageHashes` dedup to include `package_name` and
  `architecture` so a true conflict surfaces as a parse error
  rather than a silent overwrite.

Phase 3 also extends `isPackagesMember` to accept `Packages.xz` (in
addition to Phase 2's `Packages` and `Packages.gz`), and extends
`readPackagesBlob` with an xz reader path parallel to the existing
gzip path. Pure-Go xz library: `github.com/ulikunitz/xz`. Same
size-cap protections as the gzip path.

#### 7.5.3 Hot-set computation

The hot set is the list of `(path, declared_sha256, upstream_url)`
tuples to prefetch into `pool/`. It is computed at adoption time
from existing tables (`url_path`, `package_hash`); no separate "hot"
table is persisted.

**Stage 1: identify (Package, Arch) pairs hot in the prior snapshot.**

```sql
SELECT DISTINCT ph.package_name, ph.architecture
FROM   package_hash ph
JOIN   url_path up
  ON   up.canonical_scheme = ph.canonical_scheme
 AND   up.canonical_host   = ph.canonical_host
 AND   up.path             = ph.path
WHERE  ph.snapshot_id        = :prior_snapshot_id
  AND  ph.package_name      <> ''
  AND  ph.architecture      <> ''
  AND  up.last_requested_at >= :now - :hot_window_seconds;
```

Both empty-string predicates are required: pre-v3 rows have empty
defaults on either column independently, and the index in §4.3.1
covers both.

**Stage 2: resolve those pairs to new paths in the candidate snapshot.**

```sql
SELECT path, declared_sha256
FROM   package_hash
WHERE  canonical_scheme = :scheme
  AND  canonical_host   = :host
  AND  snapshot_id      = :candidate_snapshot_id
  AND  (package_name, architecture) IN (... stage-1 results ...);
```

The `(canonical_scheme, canonical_host, snapshot_id, package_name,
architecture)` index in §4.3.1 makes Stage 2 index-only.

A hot pair whose `(Package, Arch)` is no longer present in the
candidate snapshot (the upstream removed the package) does not
graduate to the prefetch list — there is no new path to fetch. The
cache continues serving the prior version from `pool/` until Phase 4
GC reaps it.

The `upstream_url` for each prefetch is constructed at adoption time
from the suite's canonical host plus the `package_hash.path`,
mirroring how Phase 2's metadata-member fetches construct upstream
URLs.

#### 7.5.4 `package_coverage_complete` semantics

The candidate snapshot's `package_coverage_complete = 1` *only* when
*all* of the following hold:

1. **The suite layout is `/dists/`-shaped.** Specifically,
   `repoRootFromSuitePath(suite_path)` returns `(_, true)`. Non-`/dists/`
   layouts cause `buildPackageHashes` to short-circuit with `(nil, nil)`
   (`internal/freshness/adoption.go:732-741`); strict mode cannot
   refuse `.deb`s under a snapshot whose Packages indices were never
   parsed.
2. **The Release lists at least one `Packages*` member.** A Release
   with literally zero binary indices (unusual but legal — e.g. a
   source-only suite) leaves nothing for strict mode to vouch from;
   set the column to `0` rather than vacuously to `1`.
3. **Every Release-listed directory containing a `Packages*` member
   contributed at least one parseable variant to `package_hash`.** A
   directory whose only `Packages*` member is unsupported (e.g.
   `Packages.bz2`, which Phase 3 does not parse) flips the column to
   `0` even if other directories had parseable variants — partial
   coverage is the case the per-snapshot boolean exists to detect.

Pseudocode for the detection (runs during Phase 2 step 8 in
`buildPackageHashes`):

```
if !repoRootFromSuitePath(suite_path).ok:
    coverage_complete := false
    log Info "package_coverage_incomplete"
             (canonical_host, suite_path, snapshot_id,
              reason := "non_dists_layout")
else:
    pkgDirs := group of (suite-relative directories that contain at
                         least one Release-listed path with basename
                         matching /^Packages($|\.)/)
    if pkgDirs is empty:
        coverage_complete := false
        log Info "package_coverage_incomplete"
                 (..., reason := "no_packages_members")
    else:
        coverage_complete := true
        missing_dirs := []
        for dir in pkgDirs:
            if no member in dir has a path that isPackagesMember
                                              accepts:
                coverage_complete := false
                missing_dirs.append(dir)
        if !coverage_complete:
            log Info "package_coverage_incomplete"
                     (..., reason := "unsupported_variants",
                      directories := missing_dirs)
INSERT INTO suite_snapshot (..., package_coverage_complete)
VALUES (..., coverage_complete ? 1 : 0);
```

The detection runs even when adoption has Phase 3 hot prefetch
disabled (`hot_packages.window = 0s`); the column is independent of
hot prefetch and is the per-snapshot proof strict mode (§6.1) keys
on.

#### 7.5.5 Failure handling

Phase 2 §7.5.2 carries forward. Phase 3 adds:

The Phase 3 hot-prefetch loop has three orthogonal per-deb failure
modes and one phase-level failure mode. Each fires its own log event;
none overlap.

- **Hot-deb fetch failure** (per-deb upstream retries exhausted): the
  loop logs `hot_prefetch_deb_failed` (per-deb) and proceeds to the
  next hot deb. The phase-level `adoption_hot_prefetch_partial` event
  does NOT fire for this case — the deb was attempted; it just failed.
  The flip proceeds normally with whatever was successfully warmed.
- **Hot-deb hash mismatch**: the temp blob is discarded (NOT promoted
  into `pool/`); the loop logs `hot_prefetch_hash_mismatch` (per-deb)
  and proceeds. This differs from a metadata-member mismatch
  (Phase 2 aborts adoption) because a single misbehaving `.deb`
  upstream cannot bring down adoption for the whole suite — the
  hot-prefetch contract is best-effort. The `.deb`'s declared hash
  is in `package_hash` regardless, so the post-flip request path
  will re-attempt the fetch and apply Phase 2's hash validation.
  The phase-level `adoption_hot_prefetch_partial` event does NOT
  fire for this case either.
- **Budget elapse** (`adoption.hot_prefetch_budget` deadline reached
  with hot debs still in the queue): the *prefetchCtx* is cancelled
  via context.DeadlineExceeded, in-flight fetches abort. The loop
  emits `adoption_hot_prefetch_partial` (phase-level, fires once)
  with the list of paths *not yet attempted* at the moment of
  cancellation. Paths that already failed (deb_failed) or mismatched
  (hash_mismatch) are not duplicated into the partial list — they
  already have their own event. The flip then runs under the parent
  `adoptionCtx` (NOT `prefetchCtx`) so budget elapse never causes
  `CommitAdoption` to fail; this is the load-bearing context-split
  in §7.5 step 10.
- **Shutdown cancellation** (parent `adoptionCtx` cancelled by SIGTERM
  or scheduler `LifetimeCtx`): unchanged from Phase 2 §9.5. The
  candidate snapshot is abandoned; `prefetchedURLPaths` is discarded;
  no flip happens. Pool blobs from the partial hot warm are
  orphaned (no `url_path` row points at them, no `snapshot_member`
  either). Phase 4 GC reaps them by `refcount = 0`.
- **Adoption abort** *outside* the hot-prefetch loop (metadata parse
  error, GPG failure, member-fetch failure during Phase 2 step 5):
  unchanged — Phase 2 abort semantics apply. `prefetchedURLPaths` is
  unused (the loop hadn't run yet, or its outputs are simply
  discarded); the candidate `suite_snapshot` row is not flipped.

The aggregate `adoption_hot_prefetch_complete` Info line is logged
*regardless* of which (if any) failure modes fired, with summary
counts (`fetched`, `failed`, `mismatched`, `unattempted`). Operators
see a single line per adoption transition that says exactly how the
hot-prefetch phase went.

### 7.6 GPG verification
Unchanged — see SPEC2 §7.6.

---

## 8. Stale-and-Valid-Until
Unchanged — see SPEC.md §8 / SPEC2 §8.

---

## 9. Concurrency & deadlines

### 9.1 Per-request
Unchanged — see SPEC.md §9.1.

### 9.2 Singleflight
Unchanged — see SPEC.md §9.2.

### 9.3 Per-host concurrency on upstream
Unchanged — see SPEC2 §9.3. The Phase 3 hot-prefetch loop consumes
the same `hostsem` slot as Phase 2 metadata-member fetches; sequential
within an adoption keeps the per-host fan-out exactly the same as
Phase 2.

### 9.4 SQLite concurrency
Unchanged — see SPEC.md §9.4.

### 9.5 Graceful shutdown
Unchanged shape — see SPEC2 §9.5. Phase 3 hot-deb fetches are
adoption-time fetches and inherit the same context cancellation
behavior: a SIGTERM mid-prefetch cancels in-flight `.deb` downloads,
the candidate `suite_snapshot` is abandoned (no flip), and the
startup `tmp/` sweep on next boot reclaims the abandoned
hot-prefetch downloads.

---

## 10. Logging (deltas)

Phase 1 + Phase 2 logging carries forward. Phase 3 adds:

### 10.1 Per-request line additions

- `outcome=unvouched_deb_refused` for §6.1's strict-mode 502 path.
  Distinguishes the strict-mode refusal from `bad_gateway` (Phase 1
  fetch failure) and from `package_hash_mismatch` (Phase 2 declared-
  hash failure).

### 10.2 New structured events

- **Hot-prefetch outcomes:**
  - `adoption_hot_prefetch_started` Info at the start of the loop:
    `canonical_host`, `suite_path`, `hot_count`, `budget_seconds`.
  - `adoption_hot_prefetch_complete` Info on loop completion:
    `canonical_host`, `suite_path`, `snapshot_id`, `hot_count`,
    `fetched`, `failed`, `mismatched`, `unattempted`. Always logged,
    even when `hot_count == 0` (so operators can confirm the loop
    ran). `unattempted` is non-zero only when the budget elapsed
    before the loop reached every entry; the four sum-bucket fields
    plus `fetched` always equal `hot_count`.
  - `adoption_hot_prefetch_partial` Warn when the budget elapsed
    with hot debs still in queue: `canonical_host`, `suite_path`,
    `snapshot_id`, `missing` (a JSON array of canonical paths that
    were *not yet attempted* at the moment of cancellation).
    **Fires only on budget elapse**, never on per-deb failures.
    Per-deb retry-exhaustion paths emit `hot_prefetch_deb_failed`
    individually; per-deb hash mismatches emit
    `hot_prefetch_hash_mismatch` individually; neither populates the
    `missing` list. The aggregate counts across all four cases are
    in `adoption_hot_prefetch_complete`.
  - `hot_prefetch_deb_failed` Warn per hot deb whose upstream
    fetch fails after `upstream.max_retries`: `canonical_host`,
    `path`, `err`. The loop continues.
  - `hot_prefetch_hash_mismatch` Warn per hot deb whose downloaded
    body's sha256 disagrees with `package_hash.declared_sha256`:
    `canonical_host`, `path`, `declared_sha256`, `observed_sha256`,
    `snapshot_id` (the candidate; it has `adopted_at = NULL` and
    will be flipped or abandoned). The temp blob is discarded.
- **Strict-mode events:**
  - `unvouched_deb_refused` Info on every §6.1 strict-mode 502:
    `canonical_host`, `path`, `current_snapshot_count`. Operator-
    facing: confirms the cache is doing what was asked for.
  - `unvouched_deb_passthrough_no_coverage` Info at most once per
    `(canonical_host, path, hour)` when the strict-mode flag is on
    but the host's coverage gate fails: `canonical_host`, `path`,
    `incomplete_snapshot_id`. Once-per-hour rate limiting prevents
    log spam under steady incomplete-coverage traffic; the operator
    surface is "which host's coverage is incomplete?" not "every
    individual request."
  - `refuse_unvouched_debs_inert` Warn at startup (once) when
    `integrity.refuse_unvouched_debs = true` AND
    `adoption.enabled = false` — the strict flag is inert in this
    combination.
  - `hot_prefetch_budget_unbounded` Warn at startup (once) when
    `adoption.hot_prefetch_budget = 0s` — flags the unbounded
    worst-case wait.
- **Coverage signal:**
  - `package_coverage_incomplete` Info on adoption when the
    candidate snapshot's `package_coverage_complete` is `0`:
    `canonical_host`, `suite_path`, `snapshot_id`, `directories`
    (a JSON array of suite-relative directories whose only
    `Packages*` member was an unsupported variant). Operator-facing
    — names the gap that strict mode will fail-through on.

### 10.3 Startup config dump (additions)

Append: `hot_packages_window`, `adoption_hot_prefetch_budget`,
`integrity_refuse_unvouched_debs`. The single boot log line tells
the operator exactly which Phase 3 policy this process is running
under.

---

## 11. Failure-mode catalog (deltas)

Phase 1 + Phase 2 rows carry forward. Phase 3 adds:

| Scenario | Phase 3 behavior |
|---|---|
| Adoption observes new `InRelease`; prior snapshot has hot debs; one hot deb's upstream URL hangs forever | After `adoption.hot_prefetch_budget` elapses, the in-flight fetch is cancelled and emits `hot_prefetch_deb_failed`. If the cancellation leaves any hot debs unattempted in the queue, `adoption_hot_prefetch_partial` Warn fires once listing those. The flip proceeds (`CommitAdoption` runs under `adoptionCtx`, not `prefetchCtx`); the failed/missing debs fall back to the cache-miss path on first post-flip request. |
| Adoption observes new `InRelease`; a hot deb's upstream returns bytes whose sha256 disagrees with the declared `package_hash` value | The temp blob is discarded; `hot_prefetch_hash_mismatch` Warn is logged; the loop continues. Adoption flip proceeds. The post-flip miss path will re-attempt the fetch and apply Phase 2 §6.5 hash validation (which produces the existing `hash_validation_failure` event if the upstream is still wrong). |
| Adoption observes new `InRelease`; `hot_packages.window = 0s` | Hot-set is empty; `adoption_hot_prefetch_started` logs `hot_count=0`; flip proceeds via Phase 2 path with `prefetchedURLPaths = nil`. |
| Adoption observes new `InRelease`; suite has no prior `current_snapshot_id` (cold-cache for this suite) | Hot-set is empty by construction; same outcome as the previous row. Hot prefetch first kicks in on the *second* snapshot transition. |
| Operator flips `hot_packages.window` to `0s` mid-run | Next adoption cycle skips hot prefetch; existing warmed url_path rows survive (they were committed in a prior flip); pre-existing url_path rows continue to serve. |
| Strict mode: `integrity.refuse_unvouched_debs = true`, host has fully-covered current snapshot, `.deb` request whose path is unvouched | `502 Bad Gateway` + `Retry-After: 60`; no upstream connection initiated; `unvouched_deb_refused` logged. |
| Strict mode: `refuse_unvouched_debs = true`, host has at least one current snapshot with `package_coverage_complete = 0` | Strict mode falls through to trust-upstream (Phase 2 behavior); `unvouched_deb_passthrough_no_coverage` logged at most once per (host, path, hour) so operators see which host is incomplete. |
| Strict mode: `refuse_unvouched_debs = true` but `adoption.enabled = false`; stale `current_snapshot_id` rows from prior runs persist | Strict mode is inert (predicate explicitly checks `cfg.Adoption.Enabled`); `.deb` requests fall through to trust-upstream (or to existing Phase 2 hash validation when a snapshot vouches for the path). Startup logged `refuse_unvouched_debs_inert` Warn. |
| Adoption parses a Release whose only `Packages*` variant in some directory is unsupported (e.g. `Packages.bz2`) | Adoption succeeds; `package_hash` is sparse (the unsupported directory contributes zero rows); candidate snapshot's `package_coverage_complete = 0`; `package_coverage_incomplete` Info names the affected directories. Strict mode for that host falls through to trust-upstream until a future Phase adds the variant. |
| Adoption sees a Packages stanza whose `Filename` declares `Architecture: amd64` in `Packages.gz` and `Architecture: arm64` in `Packages.xz` | `buildPackageHashes` dedup detects the conflict; adoption fails as `adoption_parse_failed`; prior snapshot continues to serve. The malformed Release would also be rejected by apt itself. |
| `v2 → v3` migration interrupted | Tx rolls back; next start retries from `schema_version = 2`. |
| Operator sets `hot_prefetch_budget = "0s"` | Hot prefetch runs until every deb has terminated (success or full-retry failure); per-deb budget is still `upstream.total_timeout × upstream.max_retries`. Worst-case wait is `N × per-deb budget` where N is hot-set size. Startup `hot_prefetch_budget_unbounded` Warn flags this. |

---

## 12. Test strategy (deltas)

Phase 1 + Phase 2 tests all carry forward and must continue to pass.
Phase 3 adds:

### 12.1 Unit tests (additions)

- **Packages parser extension.** `Package` and `Architecture`
  extraction across the paragraph format. Stanzas missing one or both
  fields. Real fixtures for `Packages` (plain), `Packages.gz`, and
  `Packages.xz` (the new variant). Mixed cases (some stanzas have
  fields, some don't).
- **`buildPackageHashes` dedup with name+arch.** Identical-content
  variants collapse cleanly; conflicting `(Package, Architecture)`
  for the same `Filename` across variants raises a parse error.
- **`package_coverage_complete` detection.** Fixtures where every
  directory has a parseable variant → `coverage_complete = 1`;
  fixtures where one directory's only variant is `Packages.bz2` →
  `coverage_complete = 0`; mixed scenarios.
- **Hot-set query.** Two-stage SQL against goldens with prior +
  candidate snapshots, varying `hot_packages.window` values, and
  pre-v3 rows mixed in. Asserts pre-v3 rows are excluded and the
  Stage-2 index is hit (verified via `EXPLAIN QUERY PLAN`).
- **`CommitAdoption` with `prefetchedURLPaths`.** Visibility
  assertions: a reader racing the transaction sees either the
  prior `current_snapshot_id` and *no* new url_path rows, OR the
  new `current_snapshot_id` and the new url_path rows — never
  partial. Goldens for refcount math when the prefetched url_paths
  upsert over a prior version's row.
- **Migration v2 → v3.** Apply against a Phase 2 snapshot, verify
  schema; idempotent re-apply is a no-op; an interrupted migration
  rolls back cleanly.
- **Config IsDefined.** `hot_packages.window = "0s"` and
  `adoption.hot_prefetch_budget = "0s"` round-trip through `Load`
  intact (not clobbered by `Defaults()`).

### 12.2 Integration tests (additions)

- Test cases for every row added to §11.
- Strict-mode interaction with §6.1 hit and §6.2 miss paths under
  varying combinations of `refuse_unvouched_debs`,
  `adoption.enabled`, and `package_coverage_complete`.

### 12.3 Phase 3 chaos test: hot-prefetch budget under upstream stall (the gate)

```
GIVEN
  a cache adopted at snapshot A on suite S, with prior client traffic
  having hit N hot .debs in the prior 24h (recorded in url_path)
  upstream now publishing snapshot B (new InRelease + new Packages
    referencing new versions of those N hot debs)
  exactly one of the N hot debs has its upstream URL hang forever
  the test fixture orders the hot set deterministically so the hung
    deb is FIRST in iteration order — this lets the assertion below
    pin both the deb_failed and partial events; without ordering,
    iteration position governs which event surfaces the hung path
  during prefetch, concurrent client GETs for the OLD versions of
    the hot debs continue to arrive at rate R
WHEN
  the cache observes the new InRelease (T2 fires)
  adoption begins; metadata members fetch (Phase 2 path); hot
    prefetch begins with a small budget (e.g. 30s)
THEN
  during prefetch (before the flip), client GETs for old-version paths
    serve from snapshot A normally (HIT) — the new url_path rows are
    NOT yet visible because they're queued for the flip transaction
  no concurrent GET observes a hash-mismatch eviction on the warmed
    debs (this is the visibility-race regression Phase 3 introduces
    if url_path is written pre-flip; the test guards explicitly
    against it)
  hot_prefetch_deb_failed Warn fires for the hung deb's path with
    the wrapped context-cancellation error (the hung fetch was
    attempted but cancelled when prefetchCtx hit the budget)
  adoption_hot_prefetch_partial Warn fires once with `missing` set to
    the canonical paths of the N-1 remaining (unattempted) hot debs
    — NOT the hung one (which is in deb_failed instead)
  adoption_hot_prefetch_complete Info fires with hot_count=N,
    fetched=0, failed=1, mismatched=0, unattempted=N-1
  adoption flips to B (CommitAdoption succeeds despite prefetchCtx
    being cancelled — this is the §7.5 step-10 context-split
    contract)
  immediately post-flip, the metadata for snapshot B serves from the
    snapshot via §6.1; the per-flip hot debs that did NOT yet fetch
    fall back to the cache-miss path on first post-flip request
  the hung hot deb produces a cache-miss-then-fetch-fail on first
    post-flip request (502 with Retry-After) — its url_path row was
    never inserted because its fetch never succeeded
  no client request mid-flip sees "B's InRelease + A's Packages" or
    vice versa (Phase 2 atomic-flip property carries forward)
```

A second variant uses `hot_prefetch_budget = 0s` and asserts that
adoption flips after `upstream.total_timeout × upstream.max_retries`
on the hung deb — bounded, not infinite — so a misconfigured budget
cannot indefinitely stall adoption. In this variant
`adoption_hot_prefetch_partial` does NOT fire (no wall-clock budget,
so no DeadlineExceeded path).

A third variant verifies the ordering inverse: the hung deb is LAST
in iteration order. Asserts: the first N-1 hot debs `fetched`
successfully; budget hits while the hung deb is in flight;
`hot_prefetch_deb_failed` fires for the hung one;
`adoption_hot_prefetch_partial` does NOT fire (queue empty at
cancel time); `adoption_hot_prefetch_complete` reports
fetched=N-1, failed=1, unattempted=0. This pins the contract
at both ends: partial only fires when there's genuine
unattempted-queue residue.

### 12.4 Phase 3 chaos test: strict mode with coverage gating

```
GIVEN
  cache running with adoption.enabled = true and
    integrity.refuse_unvouched_debs = true
  host H1 has current snapshot A1 with package_coverage_complete = 1
    (every Release-listed Packages directory had a parseable variant)
  host H2 has current snapshot A2 with package_coverage_complete = 0
    (a directory whose only Packages variant was an unsupported
    compression — fixture uses Packages.bz2)
WHEN
  client requests .deb path P_unknown_H1 (not in any snapshot on H1)
THEN
  cache responds 502 + Retry-After: 60
  unvouched_deb_refused logged
  no upstream connection made for P_unknown_H1
  the per-request log line outcome is unvouched_deb_refused

WHEN
  client requests .deb path P_unknown_H2 (host fails the coverage gate)
THEN
  cache falls through to trust-upstream behavior
  unvouched_deb_passthrough_no_coverage logged at most once per
    (host, path, hour)

WHEN
  integrity.refuse_unvouched_debs = false (default)
THEN
  P_unknown_H1 falls through to trust-upstream regardless of coverage

WHEN
  integrity.refuse_unvouched_debs = true and adoption.enabled = false
  (operator has flipped the master switch but stale snapshots persist)
THEN
  P_unknown_H1 falls through to trust-upstream — strict mode is inert
  startup emitted refuse_unvouched_debs_inert Warn at process boot
```

### 12.5 v2 → v3 migration end-to-end *(deliberately skipped)*

This test is **not implemented and will not be implemented**. The
Phase 2 → Phase 3 migration code path (`migrations[2]` in
`internal/cache/schema.go`) still exists and is exercised by unit
tests, but the end-to-end "old binary, new binary, same `cache_dir`"
harness was scoped out: the operator population for this build is a
single deployment running pre-release builds, and the operator has
elected to drop and re-create the cache directory across the v2 → v3
boundary rather than rely on in-place migration. The conservative
defaults the migration installs (`package_coverage_complete = 0`,
empty `package_name` / `architecture` on pre-v3 `package_hash` rows)
remain correct on paper; we just do not gate the release on the
integration test.

If a future deployment ever needs in-place upgrade from a v2 cache
directory, this section is the spec for the test that should be
written first.

### 12.6 Soak (manual / nightly)

Phase 1 + Phase 2 soak extends to: assert no leak in `tmp/` across
rolling adoptions with hot prefetch; no growth in candidate-snapshot
count beyond what Phase 4 would reap; confirm the strict-mode
once-per-(host, path, hour) rate-limit on
`unvouched_deb_passthrough_no_coverage` survives a 24h window without
unbounded log growth.

---

## 13. Project layout & tooling (deltas)

Phase 1 + Phase 2 layout carries forward. Phase 3 adds nothing new at
the package level — the changes land inside existing packages:

```
internal/
  freshness/
    adoption.go         # extends runAdoption with hot prefetch loop
                        # and CommitAdoption signature change
    packages_parse.go   # extends ParsePackages with Package +
                        # Architecture extraction
    hot_set.go          # NEW file in this package: the two-stage
                        # hot-set query + helpers
  cache/
    adoption.go         # CommitAdoption gains the prefetchedURLPaths
                        # parameter; new transaction step inserts
                        # those rows
    schema.go           # migrations[2] (v2 → v3) appended;
                        # CurrentSchemaVersion bumped to 3
  config/
    config.go           # IsDefined wiring for the two new
                        # presence-sensitive duration keys
  handler/
    handler.go          # tryURLPathHit and tryServeMiss extended
                        # with the strict-mode predicate
```

`go.mod` additions:

- `github.com/ulikunitz/xz` (pure-Go xz, used by
  `readPackagesBlob` for the new `Packages.xz` variant).

CI jobs from earlier phases carry forward. The `go test -race ./...`
job now includes the §12.3 and §12.4 chaos tests. The `e2e/` job
gains the deb-install harness's second-cycle adoption + hot-prefetch
check (§14 DoD #5); the v2 → v3 migration end-to-end test described
in §12.5 is deliberately not part of CI (see §12.5 for rationale).

---

## 14. Definition of done

Phase 3 is done when:

1. Every Phase 1 chaos test (SPEC §12.3) and Phase 2 chaos tests
   (SPEC2 §12.3, §12.4) continue to pass — Phase 3 must not regress
   prior behavior.
2. The Phase 3 hot-prefetch budget chaos test (§12.3) passes 10
   consecutive runs with no flakes, including both the default-budget
   and `budget = 0s` variants.
3. The Phase 3 strict-mode + coverage-gating chaos test (§12.4)
   passes 10 consecutive runs across all four cases (strict on with
   covered host, strict on with uncovered host, strict off, strict
   on with adoption disabled).
4. *(Deliberately dropped.)* The `v2 → v3` migration end-to-end test
   (§12.5) is **not** required for Phase 3 done. The migration code
   path itself remains in `internal/cache/schema.go` and its
   per-step semantics are covered by unit tests; the integration
   harness is scoped out because the only known v2 deployment is
   the pre-release one whose operator will drop and re-create the
   cache directory at the v2 → v3 boundary. See §12.5.
5. The `.deb` package builds, installs, and starts on Ubuntu 24.04 +
   26.04 with `adoption.enabled = true`, `hot_packages.window = 24h`,
   and `integrity.refuse_unvouched_debs = false` (the safe defaults),
   and the deb-install harness exercises at least one second-cycle
   adoption that successfully prefetches a hot deb. (Extends
   `e2e/deb/`.)
6. The cache is deployed to one production environment with
   `adoption.enabled = true` and `hot_packages.window = 24h` for at
   least one week. Monitoring shows:
   - `adoption_hot_prefetch_complete` events for every snapshot
     transition (proves the loop runs even when `hot_count = 0`);
   - `adoption_hot_prefetch_partial` events are bounded and traceable
     to specific hung upstream paths (not systemic);
   - bounded RSS / FD count;
   - hot-prefetch goroutines drain cleanly on graceful shutdown
     (no leaked tmp files at start-up sweep);
   - `package_coverage_incomplete` events, if any, name specific
     known upstream layouts and motivate the future-phase decision
     on default-flipping `refuse_unvouched_debs`.
7. SPEC3.md reflects the as-built reality (this document is updated
   as we go, not just before).
