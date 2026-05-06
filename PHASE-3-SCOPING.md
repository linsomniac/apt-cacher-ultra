# apt-cacher-ultra — Phase 3 Scoping

Status: **scoping in progress** (revision 2 — second-round review pass).
Last updated 2026-05-06. Next artifact: `SPEC3.md` modeled on
`SPEC2.md`'s structure, once the question table in §7 is fully closed
and any further review feedback has been incorporated.

This document gathers what Phase 3 is, the hooks Phase 1 and Phase 2
left in place for it, and the design decisions either resolved or still
open before this becomes a locked SPEC3.md (parallel to SPEC.md /
SPEC2.md). The companion document
[PHASE-2-SCOPING.md](PHASE-2-SCOPING.md) records the parallel exercise
for Phase 2.

---

## 1. Goals

Phase 1 made the cache-hit path bulletproof. Phase 2 closed the integrity
and freshness loops with atomic adoption + GPG verification + hash
validation. Phase 3 closes the **service-continuity loop**:

1. **Hot-package proactive refresh.** Packages that clients have
   actually been using are warmed *before* the cache flips to a new
   snapshot, on a best-effort basis. Adoption becomes hot-aware: a
   snapshot is held in the candidate state while hot packages prefetch;
   when every targeted hot package has been hash-validated (or a
   configurable budget elapses), the atomic flip proceeds. Crucially,
   hot-prefetch outputs (the `url_path` rows for warmed debs) are
   written *inside* the same SQLite transaction that flips
   `current_snapshot_id` — never visible to readers before the flip.
   This is best-effort, not absolute — the budget exists precisely so
   a single misbehaving upstream package cannot freeze adoption
   forever. When everything goes well, post-flip clients hitting the
   new snapshot for hot packages see HITs from local pool. When the
   upstream is partly broken, the flip still happens; missing hot
   packages serve via the normal cache-miss path on first request.
   See §3.2 for the exact contract.

2. **`.deb` hash-validation strict mode.** Phase 2 left unvouched
   `.deb` requests (a `.deb` whose canonical path is not referenced by
   any current snapshot's `package_hash`) on the trust-upstream code
   path. Phase 3 adds an opt-in strict mode that refuses unvouched
   `.deb` requests once a host has *provably complete* `package_hash`
   coverage to vouch from. Two prerequisites land in Phase 3 alongside
   the strict-mode flag: `Packages.xz` parser support (so the cache
   can populate `package_hash` from upstreams that ship only `.xz`
   indices), and a per-snapshot `package_coverage_complete` boolean
   recorded at adoption time so the strict rule only kicks in once a
   host's snapshots provably cover the namespace they claim to.
   (Phase 2 Q9 carry-over, refined.)

The two are independent. Hot prefetch ships first; strict mode and
its prerequisites ship as a separately-gated track behind their own
config flag (default `false` in Phase 3).

### 1.1 Non-goals (deferred)

Carried forward from earlier phases unchanged:

- Garbage collection of orphan blobs from displaced snapshots (Phase 4 —
  the snapshot model produces orphans by design and waits for GC).
- Status page / `/metrics` endpoint (Phase 5).
- TLS MITM listener (Phase 6).
- Source-package caching, multi-arch beyond amd64, pdiff (Phase 6+).

Resolved as already-addressed in Phase 2 (no Phase 3 work):

- **`by-hash` routing for adopted suites.** Phase 2 adoption already
  inserts `snapshot_member` rows for every member's by-hash alias path
  (`internal/freshness/adoption.go` `byHashAliasPath`), and the
  metadata fast path `trySnapshotHit` (`internal/handler/handler.go`)
  resolves through `snapshot_member` before `url_path`. By-hash GETs
  on adopted suites are already fast. (Phase 2 Q7 carry-over,
  reclassified.)

Resolved during Phase 3 scoping:

- **Operator-triggered manual adoption (admin endpoint or SIGHUP).**
  Deferred to Phase 6+ as an optional enhancement.
- **Streaming-while-fetching as a singleflight optimization.** Deferred
  to [FUTURE-REVIEW.md §1](FUTURE-REVIEW.md). The judgment call requires
  weeks-to-months of multi-user observational data.
- **Per-byte upstream read timeouts.** Same disposition as streaming;
  see [FUTURE-REVIEW.md §1](FUTURE-REVIEW.md).
- **Per-suite freshness cadence variation.** See
  [FUTURE-REVIEW.md §2](FUTURE-REVIEW.md).

---

## 2. What Phases 1 and 2 already left in place

Walking the existing code, prior phases deliberately seeded these hooks
that Phase 3 builds on:

| Prior-phase hook | Phase 3 use |
|---|---|
| `url_path.last_requested_at` and `request_count` columns (Phase 1) | Hotness signal source; hot-set computation reads `last_requested_at` against the configured window |
| `package_hash` table (Phase 2) | Hot-set lookup target; Phase 3 adds `package_name` and `architecture` columns to enable cross-snapshot matching |
| `suite_snapshot` table (Phase 2) | Phase 3 adds a `package_coverage_complete` column populated at adoption time so strict mode can gate per-snapshot |
| `CommitAdoption(ctx, snapshotID, members, packageHashes)` (`internal/cache/adoption.go`) | Signature grows in Phase 3 to also accept `prefetchedURLPaths []URLPath`. The function inserts those rows in the same transaction that does Steps 2–7, so warmed deb url_path rows become visible at flip time, never before |
| `<cache_dir>/tmp/` workspace and `NewTempBlob()` (`internal/cache/blob.go`) | Hot-deb fetches stage here, identical to Phase 2 metadata-member fetches. Existing SweepTmp on startup cleans up abandoned hot-prefetch tmp files automatically |
| `cache.PutBlob` and `cache.PutURLPath` (`internal/cache/queries.go`) | The blob is `PutBlob`-inserted at fetch finalize (Phase 2 path). The url_path row is *not* `PutURLPath`'d directly during prefetch; instead the (path, blob_hash, declared_sha256, upstream_url) tuple is collected in memory and handed to `CommitAdoption` for in-transaction insert. This avoids the Phase 2 hash-mismatch eviction that would fire if a new url_path row were visible while the old snapshot is still current |
| `freshness.max_concurrent_adoptions` global cap (Phase 2) | Naturally bounds hot-prefetch — hot prefetch happens *within* an adoption, so the existing cap suffices; no new semaphore |
| `adoption.enabled` master switch (Phase 2) | When false: no new adoptions run, but pre-existing `current_snapshot_id` rows from prior runs persist and continue to drive request handling. Phase 3 strict mode therefore explicitly checks `cfg.Adoption.Enabled` in its predicate (§3.3.2) — flipping the master switch off is a deliberate operator return to trust-upstream posture, and strict mode honors that even when stale current snapshots remain |
| Phase 2 presence-sensitive config pattern via `toml.MetaData.IsDefined` (`internal/config/config.go:195-203`) | Phase 3's new duration keys (`hot_packages.window`, `adoption.hot_prefetch_budget`) where `0` carries documented meaning use the same pattern; `Defaults()` would otherwise clobber an explicit `0` |

Phase 3 is mostly *additive* over Phase 2. The wire contracts (SPEC §2),
URL canonicalization (SPEC §3), per-host concurrency (SPEC §9.3),
graceful shutdown (SPEC §9.5), and the snapshot model (SPEC2 §4) all
carry forward unchanged.

---

## 3. Architectural sketch

### 3.1 Hot-package model

A path is **hot** if a client has requested it within the configured
window. Default window is 24 hours, configurable globally via
`hot_packages.window`. A window of `0s` disables hot-package proactive
refresh entirely; the cache falls back to Phase 2 adoption behavior
(no hot prefetch, no adoption gating) without needing a separate
master switch.

Hotness is a path-level property keyed on `url_path.last_requested_at`,
applying only to `.deb` paths (`is_metadata = 0`). Metadata freshness
already drives adoption via Phase 2's freshness checker; that loop is
unchanged.

**Scope: global, not per-suite.** A single window applies to every
suite/host. The case for per-suite hotness windows is in
[FUTURE-REVIEW.md §2](FUTURE-REVIEW.md).

### 3.2 Adoption-gated hot prefetch (the keystone)

```
On freshness check returning 200 with new InRelease/Release at upstream:

  1. GPG-verify (Phase 2). Parse new metadata. Persist candidate
     snapshot row + members + package_hash entries (Phase 2, with the
     parser extended to also extract Package and Architecture per
     §3.2.2). Compute and persist suite_snapshot.package_coverage_complete
     (§3.3.1).
  2. Fetch every metadata-member blob into pool (Phase 2 path:
     NewTempBlob → tmp/ → Finalize → pool/).
  3. NEW: Compute the hot set for this transition.
     (Empty if no prior snapshot exists, hot_packages.window is 0s, or
     no eligible prior-snapshot rows have a fresh last_requested_at —
     in any of those cases, skip directly to step 6 with an empty
     prefetchedURLPaths slice.)
  4. NEW: For each hot deb, sequentially:
        a. Fetch from upstream via NewTempBlob → tmp/.
        b. Hash-validate against the candidate snapshot's declared
           SHA256 in package_hash. On mismatch: discard, log
           hot_prefetch_hash_mismatch (WARN), move to next deb.
        c. PutBlob (the existing Phase 2 finalize path already does
           this on success). The blob is now in pool/.
        d. Append (canonical_path, blob_hash, declared_sha256,
           upstream_url) to an in-memory `prefetchedURLPaths` slice.
           Do NOT call PutURLPath here — that would make the new
           blob_hash observable while the prior snapshot is still
           current, triggering Phase 2 hit_path_hash_evicted bookkeeping
           in tryURLPathHit (handler.go:365-379) on any concurrent
           same-path GET.
        e. On per-deb fetch failure: respect the existing
           upstream.total_timeout × upstream.max_retries budget, log
           hot_prefetch_deb_failed (WARN), move to the next hot deb.
           Do NOT loop forever on a single deb.
  5. NEW: Adoption gate. The flip waits until either:
        (a) every hot deb has terminated (success or full-retry failure),
            OR
        (b) adoption.hot_prefetch_budget elapsed wall-clock since hot
            prefetch began (then any in-flight fetch is context-cancelled).
     A WARN line adoption_hot_prefetch_partial enumerates the missing
     paths so operators see exactly what the cache couldn't warm.
  6. Atomic flip transaction. Phase 3 extends CommitAdoption's signature:
        CommitAdoption(ctx, snapshotID, members, packageHashes,
                       prefetchedURLPaths)
     The new step inserts the prefetchedURLPaths url_path rows
     (INSERT ... ON CONFLICT DO UPDATE — same upsert semantics as
     PutURLPath) inside the same transaction as the Phase 2 steps
     2–7. Readers see all-or-nothing: either the new
     current_snapshot_id is set AND the new url_path rows are visible,
     or neither.
```

This is **best-effort warming**: when upstream cooperates, hot debs are
in pool and have url_path rows in place by the time the flip happens,
so post-flip client GETs hit the fast path immediately. When upstream
doesn't cooperate, the flip still happens; missing debs serve via the
normal cache-miss path on first request — a degraded outcome, not a
failed adoption.

#### 3.2.1 Hot-set computation across snapshot transitions

The new candidate snapshot lists *new* `.deb` paths in its Packages —
typically version bumps, where `foo_1.0_amd64.deb` was hot in the prior
snapshot and `foo_1.1_amd64.deb` is the corresponding new path. Naive
path-identity matching would miss the version-bump case (the most
common one). Phase 3 matches hot debs across snapshots by
`(canonical_scheme, canonical_host, package_name, architecture)`.

The hot-set query is two stages:

```sql
-- Stage 1: (Package, Arch) pairs hot in the prior snapshot. Both
-- columns must be non-empty — pre-v3 rows have empty defaults and
-- can't carry hot identity, so they're excluded explicitly.
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

-- Stage 2: new paths for those pairs in the candidate snapshot.
SELECT path, declared_sha256
FROM   package_hash
WHERE  canonical_scheme = :scheme
  AND  canonical_host   = :host
  AND  snapshot_id      = :candidate_snapshot_id
  AND  (package_name, architecture) IN (...);
```

A hot pair whose `(Package, Arch)` is no longer present in the new
snapshot doesn't graduate to the prefetch list — there is no new path
to fetch. The cache continues serving the prior version from pool
until Phase 4 GC reaps it.

The supporting index (in §4) is
`(canonical_scheme, canonical_host, snapshot_id, package_name,
architecture)` — `snapshot_id` is in the index because Stage 2
filters by candidate snapshot, and the same `(Package, Arch)` pair
appears in multiple snapshots over time.

#### 3.2.2 Parser changes required

Phase 2's `ParsePackages` (`internal/freshness/packages_parse.go`)
currently extracts `Filename`, `SHA256`, and `Size` only. Phase 3
extends it to also extract:

- **`Package`** — the **binary package name** (e.g. `nginx`,
  `linux-image-generic`). This is the apt `Package:` stanza field;
  it is *not* the source-package name (which lives in a separate
  optional `Source:` field). Hot-set matching is binary-package
  based — apt's URL routing keys on the binary package's filename,
  so the binary `Package` is the right identity for matching.
- **`Architecture`** — e.g. `amd64`, `arm64`, `all`.

Behavior for partial / conflicting fields:

- **Stanza missing `Package` or `Architecture`** but having `Filename`
  and `SHA256`: the row still populates `package_hash` for hash
  validation (its prior purpose), with `package_name = ''` and/or
  `architecture = ''`. Hot-set computation excludes such rows via the
  Stage 1 predicate. Hash validation is unaffected.
- **Conflict across Packages variants** (Packages.gz declares
  `Architecture: amd64` for a Filename, Packages.xz declares
  `Architecture: arm64` for the same Filename): SPEC2's contract is
  that variants are identical content. A real conflict is an upstream
  pathology and treats the adoption as failed (`adoption_parse_failed`,
  no candidate snapshot inserted). Phase 3 extends `buildPackageHashes`
  dedup to include `package_name` and `architecture` so a true conflict
  surfaces as a parse error rather than a silent overwrite.

#### 3.2.3 Budget semantics for `hot_prefetch_budget`

`adoption.hot_prefetch_budget` is a **wall-clock guard on the overall
hot-prefetch phase only**. It does not change the per-fetch budget:
each individual hot-deb fetch still respects `upstream.total_timeout`
× `upstream.max_retries`, just like every other adoption-time fetch
(SPEC §6.3). After a deb fully exhausts its retries, the loop
moves to the next deb — no individual deb is retried forever.

Concretely:

- `hot_prefetch_budget = 5m` (default): if hot prefetch as a whole
  hasn't completed within five minutes, the loop stops, in-flight
  fetches are context-cancelled, the flip proceeds with whatever was
  successfully warmed.
- `hot_prefetch_budget = 0s`: no wall-clock guard on the overall phase.
  Hot prefetch runs until every deb has terminated (success or
  full-retry failure). Worst-case wait is `N × upstream.total_timeout
  × upstream.max_retries` where N is the hot-set size. Adoption is
  still bounded — just by per-deb retries, not by wall-clock. This
  setting is opt-in only; a startup warning notes the unbounded
  worst-case wait.
- **Graceful shutdown** propagates context cancellation into in-flight
  hot-deb fetches via the same path as any other adoption-time fetch
  (SPEC §9.5). A shutdown mid-prefetch abandons the candidate snapshot;
  abandoned tmp files are reaped on next startup; the prior snapshot
  remains the contract.

Both `hot_packages.window = 0s` and `adoption.hot_prefetch_budget = 0s`
carry documented meaning, so the config loader uses
`toml.MetaData.IsDefined` (`internal/config/config.go:195-203` pattern)
to distinguish "key absent → use default" from "operator wrote `0s`
explicitly." Without this, `Defaults()` would clobber the explicit
zero. The Phase 2 keys `freshness.max_concurrent_adoptions = 0`,
`integrity.validate_at_rest_interval = 0`, and
`integrity.validate_at_rest_workers = 0` use the same pattern.

**Concurrency.** Hot debs are fetched sequentially within the adoption,
matching Phase 2 §3.2's "members within an adoption are fetched
sequentially" rule. The existing `freshness.max_concurrent_adoptions`
cap (default 2) bounds total in-flight adoption load; no new
semaphore.

**Cold-cache behavior.** First-ever adoption for a suite has no prior
snapshot's traffic to mine. Hot set is empty. Adoption proceeds as
plain Phase 2 (with `prefetchedURLPaths` empty in the
`CommitAdoption` call). Hot prefetch first kicks in on the *second*
snapshot transition.

**Post-upgrade behavior.** Existing `package_hash` rows from Phase 2
will have empty `package_name` and `architecture` after the v2→v3
migration. The hot-set query excludes them via the Stage 1 predicate.
The first post-upgrade adoption populates name+arch on its candidate
snapshot's rows; hot prefetch begins on the *next* adoption after that.
Acceptable — pre-existing deployments lose at most one transition-cycle
of warm-cache benefit.

### 3.3 `.deb` hash-validation strict mode (Phase 2 Q9 follow-up)

Phase 2 left unvouched `.deb` requests on the Phase 1 trust-upstream
code path: a `.deb` whose canonical path has zero `package_hash` rows
under the host's current snapshots is fetched and served unverified.

#### 3.3.1 Phase 3 prerequisites: parser/coverage improvements

A naive "strict if host has any snapshot" rule would 502 valid `.deb`
requests under hosts whose snapshots have legitimately incomplete
`package_hash` coverage. Today's `buildPackageHashes` returns sparse
or empty coverage for two cases:

1. The suite layout doesn't match `/dists/`. `repoRootFromSuitePath`
   returns `("", false)` and `buildPackageHashes` short-circuits to
   `(nil, nil)` (`internal/freshness/adoption.go:732-741`).
2. A directory's only `Packages*` variant uses a compression Phase 3
   doesn't parse. Phase 2 supports `Packages` and `Packages.gz` only
   (`internal/freshness/adoption.go:789-792`); a directory shipping
   only `Packages.xz` produces zero `package_hash` rows for that
   component/arch in Phase 2. Phase 3's added `.xz` support narrows
   this gap but doesn't close it — `Packages.bz2` or any other unseen
   variant still fails open silently.

Worse, *partial* coverage is also possible: a Release with five
components where four ship parseable variants and one ships only an
unsupported variant produces non-empty `package_hash` for that
snapshot but with the fifth component entirely missing. A simple
"any package_hash row exists" predicate would treat that snapshot as
"covered" and refuse valid `.deb`s from the gap.

Two enabling changes ship in Phase 3 ahead of (or alongside) the
strict-mode flag:

- **`Packages.xz` parsing.** Add an xz reader path to
  `readPackagesBlob` (parallel to the existing gzip path), gated
  behind a pure-Go xz dependency (`github.com/ulikunitz/xz` is the
  established choice — pure-Go, no cgo, no system dependency).
  Update `isPackagesMember` to accept `Packages.xz`. Apply the same
  size-cap protections as the gzip path.
- **Per-snapshot `package_coverage_complete` boolean.** A new
  `suite_snapshot.package_coverage_complete` column (INTEGER 0/1) is
  set during adoption: `1` iff every Release-listed directory
  containing at least one `Packages*` member had at least one
  parseable variant. A directory whose only `Packages*` members were
  unsupported variants (e.g. `Packages.bz2`) sets the column to `0`.
  Detection happens during `buildPackageHashes` by tracking
  parseable-variant outcomes per directory; a single skipped directory
  flips the snapshot-wide flag to `0`. The flag is the per-snapshot
  proof that strict mode can rely on.

#### 3.3.2 Strict-mode flag

A new config flag `integrity.refuse_unvouched_debs` (**default
`false`** in Phase 3) controls strict mode. The strict-mode predicate
fires only when **all** of the following hold:

- `cfg.Adoption.Enabled` is `true` (the operator has not opted back to
  trust-upstream posture; see below).
- The host has at least one current snapshot.
- **Every** current snapshot on the host has
  `package_coverage_complete = 1` (so the host's namespace is
  provably covered, not just non-empty).
- The requested path has zero `package_hash` rows under any current
  snapshot on the host.

When the predicate fires, the cache returns `502 Bad Gateway` +
`Retry-After: 60` and emits a structured log line
`unvouched_deb_refused` with `canonical_host`, `path`, and
`current_snapshot_count`.

A `.deb` GET on a host that *fails* the coverage gate (any current
snapshot has `package_coverage_complete = 0`) falls back to Phase 1+2
trust-upstream behavior, with an Info log line
`unvouched_deb_passthrough_no_coverage` once per (host, path, hour) —
not per request — so an operator investigating "why is strict mode
not strict everywhere?" can see which host's coverage is incomplete
without log spam.

**`adoption.enabled = false` is an explicit fall-through.** The Phase 2
master switch only gates whether a *new* `Adopter` is built
(`cmd/apt-cacher-ultra/main.go:245`); any pre-existing
`current_snapshot_id` rows from prior runs persist and continue to
drive `tryURLPathHit`'s defense-in-depth checks. An operator who has
flipped `adoption.enabled` off is asking for trust-upstream posture
again, and stale snapshots could refuse valid debs that the upstream
still has but that the stale snapshots don't list. Strict mode honors
that intent by checking `cfg.Adoption.Enabled` directly in its
predicate — the master switch off → strict mode inert, regardless of
stale current snapshots.

**Operator footgun protection.** At startup, log a single warning if
`integrity.refuse_unvouched_debs = true` AND `adoption.enabled = false`
— the strict flag is inert in that combination.

#### 3.3.3 Default-true is a future-phase question

Flipping the default of `integrity.refuse_unvouched_debs` from `false`
to `true` is gated on:

- Real-world coverage data: do production deployments observe
  `unvouched_deb_passthrough_no_coverage` log lines? If yes, the
  coverage gate is doing protective work and we should not
  default-true until the underlying coverage gap is closed.
- Coverage of additional layouts the field demands. If real upstreams
  ship Packages with neither `.gz` nor `.xz` (e.g. `.bz2`, plain only),
  parser support follows.

Default-flip is logged as a future review item once Phase 3 deploys
generate enough data to characterize what coverage actually looks
like in the field. Until then: opt-in only.

---

## 4. Schema migration v2 → v3

```sql
-- Phase 3 schema delta. Pure additive; no row rewrites.
-- AIDEV-NOTE: applyMigration runs UPDATE schema_version SET version = 3
-- automatically (internal/cache/schema.go:222). The migration body must
-- NOT include an INSERT or UPDATE on schema_version itself — that's the
-- framework's job. Only the v0→v1 migration writes the schema_version
-- row directly (it creates the table at the same time).

ALTER TABLE package_hash ADD COLUMN package_name  TEXT NOT NULL DEFAULT '';
ALTER TABLE package_hash ADD COLUMN architecture  TEXT NOT NULL DEFAULT '';

-- snapshot_id leads after (scheme, host) so the index serves Stage 2
-- of the hot-set query directly: filter by candidate snapshot first,
-- then probe (package_name, architecture). The same (Package, Arch)
-- pair appears in multiple snapshots over time, so omitting
-- snapshot_id from the index would force per-row filtering on every
-- Stage 2 lookup.
CREATE INDEX idx_package_hash_pkg_arch
  ON package_hash(canonical_scheme, canonical_host,
                  snapshot_id, package_name, architecture);

ALTER TABLE suite_snapshot
  ADD COLUMN package_coverage_complete INTEGER NOT NULL DEFAULT 0
    CHECK (package_coverage_complete IN (0, 1));
```

`CurrentSchemaVersion` in `internal/cache/schema.go` bumps from `2`
to `3` and the new migration string is appended to the `migrations`
slice at index 2. Forward-only.

Existing `package_hash` rows get empty strings on the new columns; the
hot-set query excludes them. Existing `suite_snapshot` rows get
`package_coverage_complete = 0` (the conservative default — pre-v3
snapshots are treated as having unverified coverage, which means
strict mode treats their hosts as fail-through, never fail-closed,
until a fresh adoption produces a snapshot with the column populated).

### 4.1 New configuration keys

```toml
[hot_packages]
# Phase 3: a path is "hot" if a client has requested it within this
# window. Default 24h. A window of "0s" disables hot-package
# proactive refresh entirely (adoption falls back to Phase 2 behavior).
# This key is presence-sensitive: an operator-written "0s" must not be
# clobbered by Defaults(). The loader uses toml.MetaData.IsDefined
# (config.go:195-203 pattern).
window = "24h"

[adoption]
# Phase 3: max wall-clock time spent on the hot-prefetch phase before
# flipping the snapshot anyway. Default 5m. 0s = no wall-clock guard;
# hot prefetch runs until every hot deb has terminated (success or
# full-retry failure). Per-deb fetches still respect upstream.total_timeout
# × upstream.max_retries regardless of this setting; budget=0s does NOT
# mean "retry one deb forever." Startup warning emitted when set to 0s.
# Presence-sensitive (same handling as hot_packages.window).
hot_prefetch_budget = "5m"

[integrity]
# Phase 3: opt-in strict mode for .deb requests. When true, refuse .deb
# requests under a host whose current snapshots have proven complete
# package_hash coverage (every snapshot has package_coverage_complete=1)
# but where the requested path is not vouched for. Hosts whose snapshots
# have package_coverage_complete=0 fall back to trust-upstream regardless
# of this flag. Default false in Phase 3 — default-flip to true is a
# future-phase decision once field data characterizes coverage gaps.
# Inert when adoption.enabled = false (startup warning emitted in that
# combination).
refuse_unvouched_debs = false
```

---

## 5. Chaos test for Phase 3 (the new gates)

Phase 3 adds two chaos-test gates over Phase 2's:

### 5.1 Hot-prefetch budget chaos

```
GIVEN
  a cache adopted at snapshot A on suite S, with prior client traffic
  having hit N hot .debs in the prior 24h
WHEN
  upstream publishes snapshot B (new InRelease + new Packages, with new
  versions of those N hot debs)
  exactly one of the N hot debs has its upstream URL hang forever
  (the hot-deb DOS scenario)
  during prefetch, concurrent client GETs for the OLD versions of those
  hot debs continue to arrive
THEN
  during prefetch (before the flip), client GETs for old-version paths
    serve from snapshot A normally (HIT) — the new url_path rows are
    NOT yet visible because they're queued for the flip transaction
  no concurrent GET observes a hash-mismatch eviction on the warmed
    debs (this is the regression Phase 3 introduces if url_path is
    written pre-flip — the test guards explicitly against it)
  adoption flips to B after at most adoption.hot_prefetch_budget
    (default 5m) — context-cancellation propagates to the hung fetch
  the flip is observable via current_snapshot_id transitioning A → B
  immediately post-flip, the warmed debs serve from cache HIT (both
    pool blob and url_path row are present in the same atomically
    committed state)
  the hung hot deb produces a cache-miss-then-fetch-fail on first
    client request after the flip (502 with Retry-After)
  adoption_hot_prefetch_partial WARN line lists exactly the hung deb's
    canonical path
```

A second test variant uses `hot_prefetch_budget = 0s` and asserts that
adoption flips after `upstream.total_timeout × upstream.max_retries`
on the hung deb — bounded, not infinite.

### 5.2 Unvouched .deb refusal with coverage gating

```
GIVEN
  cache running with adoption.enabled = true and
    integrity.refuse_unvouched_debs = true
  host H1 has current snapshot A1 with package_coverage_complete = 1
    (every Release-listed Packages directory had a parseable variant)
  host H2 has current snapshot A2 with package_coverage_complete = 0
    (a non-/dists/ layout, OR a directory whose only Packages variant
    was unsupported)
WHEN
  client requests .deb path P_unknown_H1 (not in any snapshot on H1)
THEN
  cache responds 502 + Retry-After: 60
  log line unvouched_deb_refused emitted
  no upstream connection made for P_unknown_H1

WHEN client requests .deb path P_unknown_H2 (host fails the coverage gate)
THEN
  cache falls through to trust-upstream — fetches and serves
    (or 502 if upstream is down, but for normal reasons, not
    unvouched_deb_refused)
  log line unvouched_deb_passthrough_no_coverage emitted at most once
    per (host, path, hour) — confirming the coverage gate is doing
    protective work

WHEN integrity.refuse_unvouched_debs = false (default)
THEN
  P_unknown_H1 falls through to trust-upstream regardless of coverage

WHEN integrity.refuse_unvouched_debs = true and adoption.enabled = false
   (operator has flipped adoption off but stale snapshots persist on H1)
THEN
  P_unknown_H1 falls through to trust-upstream — strict mode is inert
    because the master switch is off
  startup emitted a warning at process start about this combination
```

These tests carry forward Phase 2's chaos-test infrastructure
(`internal/handler/phase2_chaos_test.go`'s `chaos2Snapshot` /
`chaos2GateVerifier` helpers) — Phase 3 extends the same scaffolding.

---

## 6. Recommended sequencing

1. **Schema delta v2→v3.** Additive: `package_hash.package_name`,
   `package_hash.architecture`, the index, and
   `suite_snapshot.package_coverage_complete`. Tests: migration runs
   green; pre-v3 rows survive unchanged with the documented defaults;
   `CurrentSchemaVersion` bumped to 3.
2. **Parser changes (§3.2.2).** Extract `Package` and `Architecture`
   in `ParsePackages`. Update `buildPackageHashes` dedup to include
   both fields. No client-visible behavior yet (rows just gain content).
3. **`Packages.xz` parser support (§3.3.1).** Add xz reader to
   `readPackagesBlob`; update `isPackagesMember` to accept
   `Packages.xz`. Adoption coverage broadens silently. Tests: an
   .xz-only upstream fixture adopts with non-empty `package_hash`.
4. **`package_coverage_complete` population (§3.3.1).** Track
   parseable-variant outcomes per directory in `buildPackageHashes`;
   set the snapshot-wide flag at adoption time. No client-visible
   behavior yet — the column populates but nothing reads it.
5. **`CommitAdoption` signature extension.** Add the
   `prefetchedURLPaths []URLPath` parameter; insert those rows inside
   the existing transaction (between the package_hash insert and the
   `current_snapshot_id` flip is the natural slot). All Phase 2
   callers pass `nil` and behavior is unchanged. Unit-tested with
   non-nil to verify visibility timing (a reader who races the
   transaction sees either the prior state OR the new state, never
   a partial mix that includes new url_path rows but old
   `current_snapshot_id`).
6. **Strict mode (§3.3.2).** `integrity.refuse_unvouched_debs` flag
   (default false), `cfg.Adoption.Enabled` predicate, per-host
   coverage gate via `package_coverage_complete`, log lines. Independent
   of hot prefetch. Chaos test §5.2.
7. **Config IsDefined wiring (§3.2.3).** Extend
   `internal/config/config.go:195-203` block with the two new
   presence-sensitive duration keys before `Defaults()` runs. Tests
   assert `0s` round-trips intact through Load.
8. **Hot-set query infrastructure (§3.2.1).** Data-access layer for
   the two-stage query. Unit-tested against fake snapshots; no
   client-visible behavior yet.
9. **Hot-prefetch loop within adoption (§3.2).** Behind
   `hot_packages.window > 0s` and `adoption.hot_prefetch_budget`.
   Calls into the extended `CommitAdoption`. First step where adoption
   behavior changes. Chaos test §5.1.
10. **Soak / bake on a single deployment for one week** (modeled on
    SPEC2 §14 #7).

Each step is independently shippable.

---

## 7. Questions

| ID | Question | Resolution |
|---|---|---|
| Q1 | Hot-package definition | **Resolved.** Hot = client-requested within `hot_packages.window`. Default `24h`. Globally configurable. `0s` disables hot prefetch. Path-level (`.deb` paths only). |
| Q2 | Hotness scope (per-suite vs. global) | **Resolved.** Global. (§3.1) |
| Q3 | Refresh trigger model | **Resolved.** Adoption-driven, best-effort. The atomic flip is gated until either every hot deb has terminated (success or full-retry failure), OR `adoption.hot_prefetch_budget` has elapsed. Url_path inserts happen *inside* the flip transaction, never pre-flip. (§3.2) |
| Q4 | Per-suite freshness cadence | **Deferred to FUTURE-REVIEW.md §2.** |
| Q5 | Hot-set matching across snapshot transitions | **Resolved.** Match by `(canonical_scheme, canonical_host, package_name, architecture)`. Schema delta in §4 adds the necessary columns; parser changes in §3.2.2 populate them. The supporting index includes `snapshot_id` so Stage 2 lookups are index-only. (§3.2.1) |
| Q6 | Partial-hot budget semantics | **Resolved.** Wall-clock guard on the overall hot-prefetch phase only; per-deb fetches respect `upstream.total_timeout × upstream.max_retries`. Default `5m`. `0s` = no wall-clock guard, bounded by per-deb retry exhaustion. Shutdown context-cancels in-flight fetches. (§3.2.3) |
| Q7 | by-hash routing for adopted suites (Phase 2 Q7 carry-over) | **Already addressed in Phase 2** via `snapshot_member` by-hash aliases (`byHashAliasPath`) and `trySnapshotHit` resolving through `snapshot_member` before `url_path`. No Phase 3 work. (§1.1) |
| Q8 | `.deb` validation tightening (Phase 2 Q9 carry-over) | **Resolved.** Strict mode is opt-in via `integrity.refuse_unvouched_debs` (default `false`). Predicate: `Adoption.Enabled && host has snapshot && every snapshot package_coverage_complete=1 && path unvouched`. Default-flip to `true` is a future-phase decision. (§3.3) |
| Q9 | Concurrency budget for hot prefetch | **Resolved.** Same `hostsem` + same `freshness.max_concurrent_adoptions` cap. Sequential within an adoption. (§3.2.3) |
| Q10 | Cold-cache / first-adoption behavior | **Resolved.** Hot set is empty by definition; `prefetchedURLPaths` is nil at `CommitAdoption`. Adoption proceeds as plain Phase 2. Hot prefetch first kicks in on the second snapshot transition. (§3.2) |
| Q11 | Pre-v3 `package_hash` rows after migration | **Resolved.** Empty `package_name` / `architecture` defaults; hot-set query excludes via Stage 1 predicate. First post-upgrade adoption populates the columns; hot prefetch begins on the *next* adoption. (§3.2) |
| Q12 | Operator-triggered manual adoption | **Deferred to Phase 6+ as optional enhancement.** (§1.1) |
| Q13 | Streaming-while-fetching + per-byte read timeouts (Phase 2 Q1 carry-over) | **Deferred to FUTURE-REVIEW.md §1.** |
| Q14 | url_path materialization for prefetched hot debs | **Resolved.** The hot-prefetch loop collects a `prefetchedURLPaths []URLPath` slice in memory; `CommitAdoption`'s extended signature inserts those rows inside the same transaction that flips `current_snapshot_id`. Pre-flip `PutURLPath` is explicitly forbidden — it would expose new `blob_hash` to readers under the old snapshot and trigger Phase 2 hash-mismatch evictions. (§3.2 step 4d, step 6, hooks table) |
| Q15 | Strict-mode parser/coverage prerequisites | **Resolved.** Phase 3 ships `Packages.xz` parsing (closes one variant gap) and per-snapshot `suite_snapshot.package_coverage_complete` (closes the partial-coverage gap). Strict mode keys on the per-snapshot flag, not on `COUNT(*) > 0`. (§3.3.1) |
| Q16 | Visibility of pre-flip warmed debs | **Resolved.** Warmed debs are visible by content in `pool/` (a SHA256-equivalent client could in principle find them by hash), but no `url_path` row points at them until the flip transaction commits. The `tryURLPathHit` and `trySnapshotHit` paths see only post-flip state. (Hooks table; §3.2 step 4d) |
| Q17 | `adoption.enabled = false` interaction with strict mode | **Resolved.** Strict-mode predicate explicitly checks `cfg.Adoption.Enabled`. Operator-flipped-off master switch returns the cache to trust-upstream posture even when stale `current_snapshot_id` rows persist from prior runs. (§3.3.2) |
| Q18 | Presence-sensitive config defaults | **Resolved.** Both `hot_packages.window` and `adoption.hot_prefetch_budget` accept `0s` with documented meaning, so the loader uses `toml.MetaData.IsDefined` (Phase 2 pattern at `config.go:195-203`) before `Defaults()` runs. Sequencing step 7 wires this up; tests assert `0s` round-trips. (§3.2.3) |

---

## 8. What this document is, and what comes next

This document captures the architectural decisions for Phase 3. The
question table in §7 is fully resolved as of this revision pass; what
remains is for the maintainer to sign off on the corrections in this
revision (and any further review feedback).

Once signed off, the natural next artifact is **`SPEC3.md`**, modeled
on `SPEC2.md`'s structure: numbered sections for goals/non-goals,
schema delta (formal), wire contract additions, request handling
deltas, freshness-and-adoption state machine extension, hash
validation rules including the strict-mode gate, parser additions,
concurrency budgets, logging, failure-mode catalog, test strategy,
project layout deltas, and a definition of done.

When SPEC3.md lands, this scoping doc becomes a historical artifact —
the record of *why* the SPEC3 decisions came out the way they did —
and can either stay in-tree or be moved to `docs/history/`, at the
operator's preference.
