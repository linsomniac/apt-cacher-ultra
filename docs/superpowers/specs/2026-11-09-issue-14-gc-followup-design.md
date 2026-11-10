# Issue #14 follow-up: cheaper, bounded, visible GC (Tiers 1 and 2)

Date: 2026-11-09. Branch: `fix/issue-14-followup` (off `main` 18f652e,
which already has PR #28's prepared-statement adoption inserts).

## Why

Issue #14's root cause, a snapshot-pass deadline that ended the tick
before the blob pass, was fixed on `main` by the hardening branch
(`docs/open-decisions.md`, *Decided*, "Issue #14: GC frees disk again…").
The maintainer's cache works, but apt-cacher-ultra uses far more resources
than other apt cachers. Three things remain:

1. **The `package_hash` table is large, and each row costs more as it
   grows.** Every adoption writes the suite's whole package list again, so
   a 70k-package suite costs 70k inserts per snapshot and, later, 70k
   deletes. A Debian trixie snapshot is 420k rows (issue #27).
   Benchmark (scratch, laptop SSD, warm cache; PR #28's prepared inserts):

   | Table rows | insert 70k | delete 70k (10k chunks) | delete, no path PK |
   |---|---|---|---|
   | 0.84M | 1.3 s | 0.85 s | 0.52 s |
   | 3.4M | 1.6 s | 1.27 s | 0.52 s |
   | 10M | 6.9 s | 5.7 s | 0.55 s |

   The path primary-key index is the part that grows. With
   `gc.keep_displaced = 3`, about three quarters of the table is
   displaced snapshots' rows, and **no production query reads them**
   (audit below).
2. **A GC batch can wait for the writer behind adoption commits, then run
   late and eat the blob pass's share.** This is the final review 2's
   G2-RR1-M1, in the issue #14 entry: ticks of 4.9 s and 6.5 s against a
   3 s budget. Cause: `submitWrite` (`internal/cache/cache.go`) queues
   the op, and the GC passes its shutdown context, not a deadline, so the
   batch runs whenever the writer reaches it.
3. **A failing GC pass is invisible.** Only WARN `gc_tick_failed` reports
   it. The open-decisions entry "No metric counts a failed GC pass" covers
   this. The representation prune also reports neither its count nor its
   duration ("The representation prune's scan grows with the cache").

The maintainer's guidance: robustness over speed, every behaviour proven
by tests, and do the boring thing.

## Goals

- G1. Displaced snapshots stop holding `package_hash` rows. Expected
  result: the table shrinks to about the current snapshots' size (about
  4× smaller at `keep_displaced = 3`), and every insert and delete gets
  cheaper.
- G2. No GC writer operation starts after its pass's deadline. A busy
  writer makes a pass end at its deadline; it never eats the next pass's
  share.
- G3. Every GC writer operation is bounded and chunked, at startup as in a
  periodic tick: one code path for displaced snapshots.
- G4. A failed GC pass is visible: a metric, the status page, and the
  last-run summary. The representation prune reports its count and
  duration.

## Non-goals

- Tier 3, a per-suite current package set written by diff. It needs a
  schema migration. It is revisited after this work, using measurements
  from this work.
- Any schema change or new migration. Schema stays at version 9.
- Changing `retiredChunkRows` (10,000; ruling HD-11) or
  `representationPruneRows`.
- Keyset-paginating the representation prune's scan. This work adds the
  metric that tells us whether it is needed.
- Letting sub-job C step past a snapshot whose chunk fails at every call
  (open item). Not requested.
- A `cache_size` pragma. It helped only at 10M rows (about 2.5×, noisy),
  so it is deferred to the Tier 3 revisit, to be measured on a real
  cache first.
- `VACUUM`. The database file keeps its high-water size; the freed pages
  are reused.

## Anti-goals (a failure even if it "works")

- Any reader that serves, vouches, reconciles, ranks, warms or reports
  giving a different answer after the strip than before it.
- A GC operation that holds the writer for seconds while listeners serve.
- A GC writer operation interrupted mid-transaction by the start deadline
  (wasted work, or a large operation that never completes). The start
  deadline gates only *starting*. An operation that has started runs to
  completion under the existing context rules (shutdown still cancels).
- Removing or rewording away any `AIDEV-` note. Notes whose rulings
  change are *updated* to say what changed and why (HD-7, PF-15, DN-7,
  HD-4).

## Audit: who reads `package_hash` (re-verified 2026-11-09)

All SQL on the table lives in `internal/cache`. Readers:

| Reader | File | Snapshot scope |
|---|---|---|
| `declaredExactSQL`, `declaredBySuffixSQL` (vouching) | `adoption.go` | current only (join `suite_freshness.current_snapshot_id`) |
| `flatRowsExistSQL` (primes `Cache.flatRows` at Open) | `adoption.go` | any row. If only displaced flat rows existed, the flag would open false, which is correct: those rows are never readable. |
| `ListIntegrityCandidates` | `adoption.go` | current only |
| `GetPackageHash` | `adoption.go` | **test-only** (no non-test caller) |
| `insertPackageHashTx` probe | `reconcile.go` | the reconciled snapshot, which `InsertReconciledMembers` checks is current inside the same writer transaction (`ErrSnapshotNotCurrent`) |
| mirror match, `topNVersionSetTx` | `gc_urlpath.go` | current only |
| coverage, host/arch stats | `queries.go` | current only |
| `ComputeHotSet` stage 1 | `queries.go` | `priorSnapshotID`, read just before the flip, while it is still current (`hot_set.go` `priorCurrentSnapshotID`). A strip that raced it would only empty one hot set (a missed warm), and it can't race it: the strip takes only non-current snapshots, and a snapshot is displaced only by the flip that follows. |
| blob reachability (blob pass) | `gc.go` | does **not** read `package_hash` (url_path, snapshot_member, suite_snapshot only) |
| legacy trust audit, rollback check, by-hash from displaced | — | read suite_snapshot / snapshot_member, not `package_hash` |

Invariant relied on: a displaced (adopted, non-current) snapshot never
becomes current again. `CommitAdoption` commits only an unadopted
candidate (the AIDEV-NOTE at `RunRetiredSnapshotGCChunk`).

The builder must re-run this audit (grep every `package_hash` in non-test
Go) and fail the plan if a new reader has appeared.

## Design

### Tier 2: sub-job D strips displaced snapshots' package rows

A new snapshot-GC sub-job, **D**, in `internal/cache/gc.go`, shaped like
`RunRetiredSnapshotGCChunk`:

- `RunDisplacedPackageHashChunk(ctx, maxRows) (rows int, more bool, err error)`
  runs one writer transaction. It selects the oldest snapshot
  (`ORDER BY snapshot_id`) that is adopted (`adopted_at IS NOT NULL`),
  not retired, not current (`NOT IN (SELECT current_snapshot_id …)`), and
  has at least one `package_hash` row (`EXISTS`, served by
  `idx_package_hash_snapshot`). It then deletes up to
  `retiredChunkRows` of that snapshot's `package_hash` rows with C's
  statement shape (`DELETE … WHERE rowid IN (SELECT rowid … WHERE
  snapshot_id = ? LIMIT ?)`).
- It deletes only `package_hash` rows. The snapshot header,
  `snapshot_member`, and `snapshot_skipped_member` stay: forensics, the
  rollback check, and retained by-hash serving use them.
- Snapshots held by `legacy_trust_audit` are **not** excluded: the audit
  never reads `package_hash`.
- Unadopted candidates and orphans are never touched. Their rows don't
  exist: only `CommitAdoption` inserts them, in the transaction that
  adopts.

The snapshot pass runs A ∪ B, then C, then D, all under the snapshot
deadline. Ruling HD-15 extends to the sub-jobs (builder's call): if one
fails, it logs its own WARN `gc_tick_failed`, and the sub-jobs after it
still run. The pass returns its first failure, which marks it failed
(and counts as `pass="snapshot"`). This way a C stuck on one snapshot
never stops D, which would defeat G1. A failed D counts as a failed
snapshot pass. `retiredRun` does the same with C and D. D comes last because C frees disk (blobs) and D frees only
rows. `retiredRun` also runs D after C, so the hourly runs strip too.
`retiredRun` re-arms when either C or D stops at its deadline with work
left. `retiredRun`, `runSnapshotPass` and `Run` get their AIDEV-NOTEs
updated.

Meaning change: `gc.keep_displaced` now keeps displaced snapshots'
headers and index files, not their package lists. Update
`docs/configuration.md` (`keep_displaced`), the configuration comment in
`internal/config/config.go`, and the release notes and Changelog. The
maintainer approved this change 2026-11-09.

Telemetry: `gc_run_complete` and `gc_retired_run_complete` gain
`displaced_package_rows_deleted`. A counter
`acu_gc_displaced_package_rows_deleted_total` is added, and
`LastRunSummary`, status JSON and the GC panel get the same figure.
Document it in `docs/log-fields.md` and `docs/admin-ui-spec.md`.

Upgrade effect: on the first 1.1.0 start (or the first start with this
change), the startup snapshot pass strips the existing backlog within its
deadline (four fifths of `max_tick_duration`). The hourly retired runs
and the periodic ticks take the rest. Listeners open after the startup
pass, as today (HD-7's bound).

### Tier 1a: GC writer operations carry a start deadline

Add to `internal/cache`:

- `WithWriteStartDeadline(ctx, t time.Time) context.Context` and
  `ErrWriteNotStarted`.
- `submitWrite` reads the start deadline from ctx. Each `writeReq` gets
  an atomic state: pending, running, or abandoned.
  - The writer moves a request from pending to running before it calls
    the op. If the request is already abandoned, the writer skips it and
    sends `ErrWriteNotStarted`.
  - The submitter waits on `res`, `closeCh`, `ctx.Done()`, and a timer at
    the start deadline. When the timer fires, the submitter tries to move
    the request from pending to abandoned. If that succeeds, it returns
    `ErrWriteNotStarted`. If it fails, the op is running, and the
    submitter keeps waiting for `res` as before.
  - A request with no start deadline behaves exactly as today.
- The op's own ctx is unchanged. The start deadline never cancels a
  running op.

In `internal/gc`, each pass wraps the ctx of its store calls with its own
pass deadline:

- url-path batches
- A ∪ B batches
- C and D chunks
- the representation prune
- blob batches

`retiredRun` uses its own deadline. `ErrWriteNotStarted` means "deadline
reached", not "pass failed": the pass returns as at its deadline (it logs
`gc_tick_deadline_reached`, sets `deadline_reached`, and moves the
rotation), and it never calls `passFailed`. Match it with `errors.Is`
through the store wrappers.

### Tier 1b: startup B retires, then C deletes

`RunSnapshotGCBatch` loses `retireDisplaced`. B always retires, and C
deletes in chunks, at startup as in a periodic tick. This is safe
because the startup pass has run C since PF-15's re-ruling. It also
leaves one code path, and the start deadline now bounds the startup pass
too.

Update the AIDEV-NOTEs that state HD-7 ("the startup pass still
deletes"): `internal/cache/gc.go` (`RunSnapshotGCBatch`), `internal/gc`
(`runSnapshotPass`, `phaseStartup`, `runTick`), and `docs/`
(configuration `snapshot_batch_size`, admin-ui-spec `gc.retired_reaped`,
log-fields, the `acu_gc_displaced_reaped_total` help). Startup
displaced snapshots now count twice (in `displaced_reaped` when retired,
in `retired_reaped` when deleted), as periodic ones do.

In `docs/open-decisions.md`, under *Decided*, add one entry for this
work that names the superseded part of HD-7, and update the open entries
it closes: "No metric counts a failed GC pass" moves to *Decided*. The
representation-prune entry gains "metric added; keyset still open".

### Tier 1c: visibility

- Add `acu_gc_pass_failures_total{pass}`, with pass ∈ {url_path,
  snapshot, blob, retired, pool_scan}. It is incremented where
  `gc_tick_failed` is logged (`runTick`'s `failed`, and `retiredRun`) and
  where `StartupPass` returns a pool-scan error. (Builder's call:
  pool_scan is counted; it writes no summary.)
- `LastRunSummary.FailedPasses []string`, filled by `runTick` only. The
  summary stays one per tick or startup pass (builder's call). A failed
  `retiredRun` reaches the counter and its WARN, not the summary. So
  the status page shows a failure until the next periodic tick or start
  replaces the summary. It goes to
  status JSON as `gc.last_run_failed_passes`, and to the GC panel as a
  *Failed passes* row (a badge per pass; "none" when empty).
- **GC vital: a failed pass on the last run is `crit`** (maintainer,
  2026-11-09). This replaces §9.1's "no GC state is crit". It needs
  matching server (`vitalState`) and client-side JS verdicts, and
  `docs/admin-ui-spec.md` §9.1.
- Representation prune: the blob pass records the pruned count and the
  prune's duration. `gc_run_complete` gains `representation_rows_pruned`
  and `representation_prune_ms`. Add the counter
  `acu_gc_representation_rows_pruned_total` and a
  `pass="representation_prune"` sample of the existing
  `acu_gc_pass_duration_seconds` (or a new histogram if that one's labels
  don't fit; the builder decides).

## Testing (robustness first)

TDD per item; a failing test first.

- **Start deadline (cache):**
  - A request queued behind a blocked writer op returns
    `ErrWriteNotStarted` at its deadline, and its op never runs.
  - An op that started before the deadline runs to completion and
    returns its result.
  - Race the deadline against dequeue many times under `-race`: the op
    runs exactly once or never, and the submitter's answer always matches
    what happened.
  - No start deadline: existing behaviour, covered by the existing tests.
- **Start deadline (gc):** a tick whose snapshot batch is stuck behind a
  held writer ends the snapshot pass at its deadline (deadline, not
  failure), and the blob pass runs within the tick's budget. This is the
  regression test for G2-RR1-M1.
- **D:**
  - It strips only adopted, non-retired, non-current snapshots. It never
    touches the current snapshot, candidates, orphans, retired
    snapshots, or other tables.
  - Chunking works across a snapshot larger than one chunk.
  - `more` is false when nothing is left.
  - A suite's displaced snapshots stay present, with their members.
- **Reader equivalence:** build a cache with current and displaced
  snapshots (dists and flat suites, signed). Record every reader's answer
  in the audit table, strip, and assert the answers are identical. This
  pins the audit.
- **Startup B:** the startup pass retires the displaced snapshots and C
  deletes them within the deadline, and its blob pass reaps their blobs.
  Update the HD-7 tests to the new behaviour, renamed to say so.
- **Visibility:**
  - Pass-failure counter and `FailedPasses` for each pass, and for
    `retiredRun`.
  - The vital goes crit on a failed pass (server and JS verdict tests,
    as the existing ones).
  - Prune count and duration are logged and in the metrics.
- **Long-run (builder's call: committed, env-gated):** two scratch tests
  go into the repo, and both skip unless their variable is set.
  - The issue #14 repro harness:
    `internal/gc/issue14_longrun_test.go`, run with `ACU_LONGRUN=1`.
    Source: `/tmp/claude-1000/-home-sean-projects-apt-cacher-ultra/6e0b682e-b7d1-4662-903e-9099b4bf6ce7/scratchpad/vmain/internal/gc/issue14_repro_test.go`.
  - The `package_hash` benchmark:
    `internal/cache/package_hash_bench_test.go`, run with `ACU_BENCH=1`.
    Source: `/tmp/claude-1000/-home-sean-projects-apt-cacher-ultra/6e0b682e-b7d1-4662-903e-9099b4bf6ce7/scratchpad/vbench/internal/cache/zz_issue14_bench_test.go`.
  - Commit them first, in a commit before any behaviour change. The
    "before" numbers come from that commit (`main` 18f652e plus the
    harness), the "after" numbers from the branch's end. Record both in
    the open-decisions entry.
- `go test -race ./...`, `make lint` (or the repo's equivalent), and e2e
  where the repo's Makefile runs it.

## Left to the builder

- Function and file names beyond those given, and whether D shares a
  helper with C.
- Whether the start-deadline plumbing is a ctx value (proposed) or an
  explicit parameter, provided no op is interrupted after it starts.
- The exact status-panel markup, following the existing badges.
- Commit granularity: one commit per tier item is suggested.

## Decided by the maintainer (2026-11-09)

- A failed pass on the last run makes the GC vital `crit` (DEGRADED). The
  maintainer accepted the cost: a transient failure stays crit until the
  next periodic tick or start replaces the summary (up to `gc.interval`).
  Record this under *Decided* in `docs/open-decisions.md`. It reverses
  §9.1's "no GC state is crit", which the issue #14 AIDEV-NOTE in
  `vitalState` states; update that note to say so, don't remove it.
- Design approved; proceed to plan and implementation on the branch.
