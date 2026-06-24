# Version-aware retention & bounded prefetch

**Date:** 2026-06-23
**Status:** APPROVED by review-loop consensus (Codex + Claude, 3 rounds) — pending
final operator review, then implementation plan. (Antigravity was to be the third
reviewer but its CLI hung and was dropped at the operator's direction.)
**Author:** Sean Reifschneider (with Claude)

**Round 1 changes (from Codex review):** mirror guard now keeps path+hash and
ranks **distinct versions per suite** (not host-wide, not per-row); "held
snapshots" defined as live `suite_snapshot` rows (consistent with snapshot-GC);
hold-grace reworked to a lazily-stamped `url_path.dropped_at` that handles
demotion-out-of-top-N as well as fall-out (no cascade dependency); empty-version
rows (Sources/pdiff/Contents) keep the existing snapshot-reference guard; GC
ranking made candidate-bounded with in-transaction eligibility re-check; `version`
made an adoption invariant with cross-variant conflict detection; comparator
test matrix expanded; the risky one-shot SQL prune dropped in favour of
wipe+rebuild / gradual re-adoption.

**Round 2 changes (from Codex re-review):** specified the `dropped_at` batch
algorithm (classify unstamped-fail / re-qualified / expired / in-grace) and
changed the `runURLPathPass` loop to terminate on `progress == 0`
(stamp+clear+delete) instead of `deleted == 0`, with `hold_packages.window == 0`
meaning same-batch delete; extended the `version` write invariant to the second
writer (`reconcile.go insertPackageHashTx`, incl. its conflict check) and made
the **binary non-empty-version invariant** explicit (missing Version ⇒ skip +
coverage-incomplete, never `version=''`) so the empty-version fallback can't
re-open the leak; flagged url_path `dropped_at` index measurement.

## Problem

A production cache reached 32 GB and kept growing. Investigation of a copy
of the live DB + pool + logs found:

- The pool is 30.4 GB across 2,332 blobs. Only 765 MB is referenced by a
  live snapshot (refcount > 0). GC is running correctly (590 `gc_run_complete`
  events) but only 1.6 MB is actually reapable.
- **25.0 GB (1,153 blobs, 82% of the pool) are packages that were prefetched
  into the cache but never served to any client** (`url_path.request_count = 0`,
  `url_path.last_requested_at IS NULL`), and cannot be reclaimed by the current
  garbage collector.
- The biggest contributors: `download.docker.com` (16.7 GB), `artifacts.elastic.co`
  (8.2 GB — the entire `logstash` 8.0.0→8.13.4 history), `archive.ubuntu.com`
  (0.1 GB).

### Two compounding causes, one amplifier

1. **No concept of "newest version."** The daemon parses `Package` and
   `Architecture` from the index (`internal/freshness/packages_parse.go`) but
   never the `Version:` field. It cannot distinguish `docker-ce 24.0.7` from
   `docker-ce 19.03.0` — only that they share `package_name = docker-ce`.

2. **Fat upstream indexes list the entire back-catalog as "current."** Unlike
   Ubuntu (one version per package per suite), Docker/Elastic publish every
   historical version in a single `Packages` file. The current Docker snapshot
   references **420 versions of `docker-ce`**, 420 of `docker-ce-cli`, 188 of
   `containerd.io`. So:
   - Hot-prefetch (`ComputeHotSet` Stage 2, `internal/cache/queries.go`) matches
     a hot package by `(package_name, architecture)` and warms **every** path
     that matches — all 420 versions. Logs show repeated docker adoptions with
     `hot_count = 1109, fetched = 1109`.
   - The url_path-TTL GC's vouching guard (`internal/cache/gc.go` `RunURLPathGCBatch`,
     guard "a") protects **any** version present in the current snapshot's index,
     so none of the 420 ever age out — even versions a client requested normally.

3. **Amplifier — `hot_packages.window = 300 days`** (vs 24 h default) makes
   nearly every package name "hot," so every adoption re-warms the whole catalog.

### What is *not* the problem

- **Snapshot retention is fine.** 70 snapshots across 20 suites, max 5/suite,
  `keep_displaced = 3`. Snapshot metadata blobs total 1.1 GB.
- **GC is not broken** and is not leaking orphans (only 1.6 MB truly reapable;
  pool-scan reaps blob-less files).
- The hypothesis "we keep versions not referenced by any snapshot, just because
  they are hot" is true for only **32.5 MB / 44 blobs**. The other 25 GB **are**
  referenced by the current snapshot — because the fat upstream index lists them.

### Why the operator's mental model needs the per-package cap

"Keep versions referenced by held snapshots" is exactly right for thin-index
repos (Ubuntu: one version/snapshot → a few held snapshots → a few versions).
It provides **no bound** for fat-index repos, because one snapshot references
hundreds of versions. apt itself does not name "the version you want" — the
index publishes many versions and apt picks the **highest** by Debian version
comparison (subject to pins / explicit `pkg=version`). So caching the newest
few versions per package fully covers the default `apt install`/`upgrade` path;
older versions become on-demand fetches (only unresolvable if upstream is down
*and* an old version is explicitly pinned — softened by the hold window).

## Goal / non-goals

**Goal:** keep a warm mirror of the newest N versions of every package the site
actively uses, bounded so fat-index repos cannot accumulate their back-catalog.
Survive upstream outages for the versions that would actually be installed.

**Non-goals:**
- Changing what apt *sees*. The full upstream index is still served verbatim;
  only the cached `.deb` set is bounded.
- Backfilling existing data. The fix is forward-only (see Reclaim).
- Touching snapshot retention (`keep_displaced`) semantics.

## Design

### 1. Data model — schema v6

Bump `CurrentSchemaVersion` 5 → 6 (`internal/cache/schema.go`). Append one
forward-only, pure-DDL migration mirroring the v3 pattern that added
`package_name`/`architecture`:

```sql
ALTER TABLE package_hash ADD COLUMN version    TEXT NOT NULL DEFAULT '';
ALTER TABLE url_path      ADD COLUMN dropped_at INTEGER;  -- hold-grace clock (§3)
```

- `version` is parsed from the binary `Packages` `Version:` field
  (`packages_parse.go` → `PackageRef.Version`) and carried by `buildPackageHashes`.
  **Both** `package_hash` INSERT sites must write it: `CommitAdoption` Step 3
  (`internal/cache/adoption.go`) and the reconcile helper `insertPackageHashTx`
  (`internal/cache/reconcile.go`) — whose AIDEV-NOTE already mandates keeping the
  two column sets in lockstep. Reconcile's same-path conflict check must compare
  `version` in addition to `declared_sha256`. A cross-variant `Version`
  disagreement for one path is rejected like the existing Package/Architecture
  conflict (adoption invariant, H7-style).
- **Binary invariant (load-bearing):** every post-v6 binary `package_hash` row
  has a **non-empty** `version`. A binary `Packages` stanza missing `Version`
  (malformed index) is skipped and recorded coverage-incomplete — never inserted
  with `version = ''`. This guarantees `version = '' ⇒ non-binary`
  (Sources/pdiff/Contents, or pre-migration), which is exactly what the §3
  empty-version fallback relies on; without it a malformed binary row would slip
  into the fallback and re-open the fat-index leak for new data.
- Existing rows keep `version = ''`. These are **not** reclaimed by natural GC
  aging (an empty-version binary row cannot be ranked); the existing 25 GB is
  reclaimed **operationally** instead (see §6). Source / pdiff / Contents rows
  are legitimately `version = ''` and keep the existing snapshot-reference guard
  (see §3).
- Ranking is **per suite** (§3), so the supporting query joins
  `package_hash → suite_snapshot` on `snapshot_id` and groups by `suite_path`.
  Whether the existing `idx_package_hash_pkg_arch (canonical_scheme,
  canonical_host, snapshot_id, package_name, architecture)` suffices or a
  covering index is needed is a **measured** decision in the plan
  (`EXPLAIN QUERY PLAN` on the ~700 K-row table), not assumed.

Add `Version string` to the `PackageHash` struct (`internal/cache/types.go:105`).

### 2. Debian version comparator

New pure package (e.g. `internal/debversion`):

- `Compare(a, b string) int` implementing dpkg semantics: optional `epoch:`,
  upstream version, optional `-revision`; comparison alternates non-digit and
  digit runs; `~` sorts before everything including end-of-string; letters sort
  before non-letter punctuation per dpkg ordering.
- No external dependencies. A wrong comparator deletes the versions apt would
  install and keeps the wrong back-catalog, so it is **release-blocking**.
  Unit-tested against a truth table derived from `dpkg --compare-versions`,
  covering: epoch (`1:` vs none), last-hyphen Debian-revision split, missing
  revision, `~` (sorts before everything incl. end-of-string), `+`/punctuation
  vs letter ordering, leading-zero numeric runs, Ubuntu revisions
  (`-1ubuntu0.2`), and the real Docker (`5:24.0.7-1~ubuntu.22.04~jammy`) and
  Logstash (`1:8.13.4-1`) strings from this incident.

### 3. Retention model (the heart)

A cached `.deb` — its `url_path` row and backing blob — is **retained iff ANY**
of the following hold. Otherwise it is eligible for expiry.

1. **Recency (existing, kept):** `last_requested_at` is within `gc.url_path_ttl`.
   Keeps anything actively pulled (including a pinned old version) for the TTL.
2. **Mirror (new):** the row is among the kept versions for its package — precise
   definition below.
3. **Hold grace (new):** the row has been failing rules 1 and 2 for less than
   `hold_packages.window`, tracked by the `dropped_at` clock below.

This **replaces** url_path-TTL guard "a" (which vouched *any* version listed in
the current index) with the version-ranked mirror guard, and **removes the
`last_requested_at IS NULL` immortality** so prefetched-but-never-served rows
fall under rules 2 + 3. Guards "b" (`snapshot_member`), "c"/"d"
(`Release`/`InRelease` anchors) are **unchanged** — metadata anchors must still
survive (freshness-freeze trap). Only the package `.deb` guard "a" changes.

**Mirror rule, precisely (rule 2).** Applies only to rows with a **non-empty
`version`** (binary packages). For each suite the row's path is referenced by,
rank the **distinct Debian versions** of its `(package_name, architecture)`
across the **held snapshots of that suite**, newest first, and keep the top
`retention.max_versions_per_package`. A row is mirror-retained iff **both**:
   - its `(package_hash.path, declared_sha256)` matches the `url_path`'s
     `(path, blob_hash)` — preserves guard "a"'s path+hash check, so stale bytes
     that diverge from the snapshot's declared hash are **not** kept; **and**
   - its version is in the top-N **distinct** version set of **at least one**
     suite that references the path.

Two corrections baked in here:
- **Distinct versions, not rows** — duplicate `package_hash` rows for the same
  version (across components/snapshots) do not consume the cap.
- **Per suite, not host-wide** — rank by `(canonical_scheme, canonical_host,
  suite_path, package_name, architecture)`. Host-wide ranking could evict a
  suite's current install candidate because another suite on the same host
  (e.g. Debian `stable` vs `testing`) has newer versions; "retain if top-N in
  any referencing suite" prevents that.

**"Held snapshots"** = the `suite_snapshot` rows that currently exist for the
suite. `RunSnapshotGCBatch` already prunes these to `current + keep_displaced`,
so "currently existing" *is* the held set — the mirror query simply joins live
`suite_snapshot` rows and need not re-derive `RunSnapshotGCBatch`'s window
ranking. (If the mirror pass runs before snapshot-GC within a tick, a
soon-to-be-displaced snapshot is transiently still counted — harmless, only
slightly conservative for one tick.)

**Empty-version rows (Sources, pdiff, Contents, pre-migration leftovers).** Not
eligible for the mirror rule. They keep the **existing** snapshot-reference
guard — retained while referenced (path+hash) by a held snapshot. Safe: these
are small and not the leak, and they have no Debian binary version to rank.
(`package_hash` covers source artifacts and pdiff patches, and the request
handler validates non-metadata paths through `package_hash`, so this fallback
must remain intact.)

**Hold-grace clock (rule 3): `url_path.dropped_at`.** Lazily stamped **by the
url_path-GC pass itself**: when the pass first observes a row failing rules 1
and 2, it sets `dropped_at = now` instead of deleting; on a later pass, if the
row still fails 1 and 2 and `now - dropped_at >= hold_packages.window`, it is
deleted; if the row re-qualifies (re-enters top-N, or is requested),
`dropped_at` is cleared to NULL. This unifies the two ways a version leaves the
kept set — **demotion** out of top-N while its snapshot still exists (fat index,
no cascade fires) and **fall-out** of every snapshot — under one
observation-based clock, so it does **not** depend on the snapshot-GC cascade.
Rule 2's N-window is itself the intra-index transition grace ("keep 1.3.4 and
maybe 1.3.3"); `dropped_at` adds the timed grace beyond it.

**url_path-GC batch algorithm (with `dropped_at`).** The driver loop
(`internal/gc/urlpath.go` `runURLPathPass`) currently ends a pass when a batch
returns 0 *deleted* rows. A stamping/clearing batch deletes 0, so the batch must
report **progress = stamped + cleared + deleted**, and the loop must terminate on
`progress == 0` (not `deleted == 0`) — `RunURLPathGCBatch`'s result type changes
accordingly. Each selected candidate is classified, all inside the one writer-tx
with the eligibility re-check:
   - **unstamped, failing rules 1 + 2:** set `dropped_at = now` — or, if
     `hold_packages.window == 0`, delete immediately in the same batch;
   - **stamped, now passing rule 1 or 2:** clear `dropped_at = NULL`;
   - **stamped, still failing, `now - dropped_at >= hold_packages.window`:**
     delete (+ decrement the blob refcount, as today);
   - **stamped, still failing, still within grace:** not selected.
The SELECT excludes in-grace stamped rows, so the candidate set strictly shrinks
each batch and the pass terminates. `hold_packages.window == 0` therefore means
same-batch delete, never "stamp now, delete next tick."

Implementation constraints:
- **Candidate-bounded and in-transaction.** The url_path-GC keeps its
  SELECT-batch → DELETE-with-re-check shape in one writer tx. Version ranking is
  computed only for the `(name, arch)` of the rows in the current batch, and
  eligibility (rules 1–3, incl. the `dropped_at` stamp/clear) is **re-evaluated
  inside the writer transaction** before any delete — no keep-set computed
  outside the tx is trusted for the DELETE. This closes the SELECT→DELETE
  liveness race the same way the existing pass does.
- **Comparator in Go.** SQLite cannot run the Debian comparator inline; the
  batch loads the candidate `(name, arch)`'s versions and ranks in Go.
- The blob-GC pass (`RunBlobGCBatch`) is unchanged; it reaps a blob once no
  `url_path`/`snapshot_member`/anchor references it and `blob_grace` elapses.

### 4. Bounded prefetch

In `runHotPrefetch` / `ComputeHotSet`: for each hot `(package_name,
architecture)`, warm only the newest `retention.max_versions_per_package`
**distinct** versions present in the candidate snapshot (same ranking as the
mirror rule, so prefetch and retention agree on "kept"), skipping any blob
already cached. A normal adoption then fetches ~1 new `.deb` per package instead
of 420.

`hot_packages.window` keeps its current meaning (which package *names* are hot).
The site's 300-day production value becomes safe because the per-package cap
bounds the fan-out. No change to the window's semantics or default.

### 5. Configuration

| Knob | Default | Meaning |
|---|---|---|
| `retention.max_versions_per_package` | 3 | newest versions kept (and prefetched) per `(name, arch)` |
| `hold_packages.window` | 24h | grace (via `url_path.dropped_at`) before a row that has left the kept set is reaped |

Added to `internal/config/config.go` with validation (`max_versions_per_package
>= 1`; `hold_packages.window >= 0`, 0 = no grace) and logged in the startup
config dump alongside the existing `gc_*` / `hot_packages_window` keys.

**Recommended operator tuning for the affected hosts** (config only, not code):
lower `gc.url_path_ttl` 7d → 24h; optionally `gc.keep_displaced` 3 → 1. These
are independent of this change and reduce the recency-rule retention window.

### 6. Reclaiming the existing 25 GB (operational)

The fix is forward: new adoptions populate `version` and obey bounded prefetch +
retention, so growth stops on deploy. The existing 25 GB is **not** reclaimed by
natural GC aging — pre-migration `package_hash` rows have `version = ''` and the
mirror rule needs a version to rank. Reclaim it operationally:

- **Wipe + rebuild (recommended).** Stop the daemon, remove `cache.db` + `pool/`,
  restart. Simplest and unambiguous: no pre-migration empty-version rows survive,
  and the cache re-warms newest versions on demand. The site does not need old
  data, so the brief cold-cache period is acceptable.
- **Gradual via re-adoption (no downtime).** Leave the cache; as each suite's
  upstream `InRelease` next changes, re-adoption repopulates `version` and the
  mirror rule begins reaping that suite's back-catalog. Slower (days–weeks,
  upstream-paced) and uneven across suites.

A hand-written one-shot SQL prune was **considered and rejected**: to be correct
it would have to replicate the entire retention union (recency, path+hash,
per-suite top-N, anchors, `snapshot_member`/`suite_snapshot` reachability) and
the exact `EvictURLPath` refcount bookkeeping — too error-prone for a one-off.

## Testing (TDD)

- **Comparator:** truth-table unit tests vs `dpkg --compare-versions` (the full
  pitfall list in §2).
- **Retention rule:** fat-index keeps exactly newest-N **distinct** versions and
  expires the rest; thin-index (Ubuntu) unchanged; **per-suite** ranking keeps a
  suite's candidate when another suite on the same host has newer versions;
  path+hash mismatch (stale bytes) is **not** kept; recency overrides the cap for
  an actively-requested old version; empty-version (Sources/pdiff) rows keep the
  snapshot-reference guard; metadata anchors never reaped (freshness-freeze
  regression).
- **Hold-grace:** `dropped_at` is stamped on the first failing pass, cleared on
  re-qualification, and the row is reaped only after `hold_packages.window` —
  covering both demotion-out-of-top-N (snapshot still present) and
  fall-out-of-all-snapshots.
- **In-tx eligibility:** a row that re-qualifies between SELECT and DELETE
  (concurrent request / re-adoption) is not deleted (re-check inside the tx).
- **Version invariant:** a cross-variant `Version` disagreement for one path is
  detected and rejected at adoption.
- **Prefetch:** fan-out bounded to newest-N distinct versions per hot package;
  already-cached blobs skipped.
- **Migration:** v5 → v6 forward-only (`version`, `dropped_at`); fresh DB created
  at v6; empty-version rows fall through to the snapshot-reference guard.
- **Regression:** reproduce the docker-420-versions leak and assert the bounded
  cache converges to N versions.

## Affected code

- `internal/cache/schema.go` — v6 migration (`package_hash.version`,
  `url_path.dropped_at`), bump `CurrentSchemaVersion`.
- `internal/cache/types.go` — `PackageHash.Version`.
- `internal/freshness/packages_parse.go` — parse `Version:` → `PackageRef.Version`
  (binary stanzas); missing-Version → skip + coverage-incomplete (binary
  invariant).
- `internal/freshness/adoption.go` — `buildPackageHashes` carries `version`.
- `internal/cache/adoption.go` — `CommitAdoption` Step 3 `package_hash` INSERT
  includes `version`.
- `internal/cache/reconcile.go` — `insertPackageHashTx` INSERT includes `version`
  and its same-path conflict check compares `version` too.
- `internal/cache/queries.go` — `ComputeHotSet` newest-N distinct-version
  selection.
- `internal/freshness/hot_prefetch.go` — bounded warm.
- `internal/cache/gc.go` — `RunURLPathGCBatch` per-suite version-ranked mirror
  guard (path+hash, distinct versions), `dropped_at` lazy stamp/clear/reap,
  empty-version fallback to the snapshot-reference guard, remove NULL immortality;
  result type reports stamped/cleared/deleted.
- `internal/gc/urlpath.go` — `runURLPathPass` loop terminates on
  `progress == 0` (stamp+clear+delete), not `deleted == 0`.
- `internal/config/config.go` — new knobs + validation + startup log.
- `internal/debversion/` — new comparator package.

## Open items for the implementation plan

- Whether `idx_package_hash_pkg_arch` suffices for the per-suite
  distinct-version ranking or a covering index is warranted — decide by
  `EXPLAIN QUERY PLAN` on the ~700 K-row table, not by assumption.
- url_path candidate selection now filters on `dropped_at` (skip in-grace) as
  well as `last_requested_at`; measure whether the existing
  `idx_url_path_last_req` covers the new predicate or an index touching
  `dropped_at` is needed.
- Exact SQL shape of the candidate-bounded mirror evaluation (join order for
  `url_path → package_hash → suite_snapshot`) within the writer tx.
- The url_path candidate SELECT must also include **stamped rows that have
  re-qualified by recency** (i.e. `last_requested_at` now within TTL) so their
  `dropped_at` is cleared — don't let a recency re-qualification leave a stale
  stamp behind. (Codex non-blocking note.)
- In logs/metrics, keep **deleted-row accounting separate from progress
  accounting** (stamped/cleared are progress but not deletions) so the existing
  `rows_reaped_this_tick` semantics stay meaningful. (Codex non-blocking note.)
