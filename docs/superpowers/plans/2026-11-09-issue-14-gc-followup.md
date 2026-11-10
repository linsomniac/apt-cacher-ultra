# Issue #14 follow-up (GC Tiers 1 and 2) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make GC cheaper, bounded and visible:

- strip displaced snapshots' `package_hash` rows (sub-job D);
- never start a GC writer op after its pass's deadline;
- have startup retire displaced snapshots rather than delete them in one
  transaction;
- surface failed passes and the representation prune.

**Architecture:**

- `internal/cache` gains a start deadline on `submitWrite` (a context
  value, plus a pending/running/abandoned state per request) and a
  chunked `RunDisplacedPackageHashChunk`.
- `internal/gc` wraps every store call in its pass's start deadline,
  treats `ErrWriteNotStarted` as "deadline reached", runs D after C (in
  the snapshot pass and in `retiredRun`), and records failures.
- `internal/admin` shows the new last-run fields and turns the GC vital
  crit on a failed pass.

**Tech Stack:** Go 1.27, SQLite (modernc), the repo's own metrics package
(`internal/metrics`), html/template.

**Spec:** `docs/superpowers/specs/2026-11-09-issue-14-gc-followup-design.md`
(read it; the audit table and the anti-goals there are binding).

## Global Constraints

- No schema change, no new migration; schema stays version 9. Never edit migration files.
- Never remove an `AIDEV-` comment. Update the notes whose rulings change (HD-7, PF-15, DN-7, HD-4, the issue #14 note in `vitalState`) to say what changed, when (2026-11-09), and why.
- `retiredChunkRows` stays 10,000 and `representationPruneRows` stays 10,000. D uses `retiredChunkRows`.
- The start deadline gates only *starting* an op. An op that has started runs under its own ctx exactly as today.
- `ErrWriteNotStarted` in a GC pass is "deadline reached", never a failure: no `gc_tick_failed`, no `passFailed`, no failure counter.
- The GC vital is `crit` when the last run's `FailedPasses` is non-empty (the maintainer, 2026-11-09).
- Every behaviour change is test-first. Run `go test -race` on the touched packages before each commit, and `make test-race` and `make lint` at the end.
- Commits end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Work on `fix/issue-14-followup`; never merge or push.
- Each user-visible change adds a line to `Changelog.md` (Unreleased: 1.1.0) and to `docs/release-notes/1.1.0.md`, in the task that makes it.

## Review Focus

1. **The start-deadline timer fires at the same instant the writer dequeues the request.** The op must run exactly once or never, and the submitter must report what actually happened. Task 1 hammers this under `-race`.
2. **A GC pass whose very first writer op is abandoned.** The pass must report deadline reached with zero counts. It must not report "drained", or B and C would look finished and the retired run would not re-arm. Covered in Task 2.
3. **Sub-job C fails on the same snapshot every call.** D must still run and shrink the table, and the pass must still count as failed. Covered in Task 5.
4. **A flat suite's current snapshot sharing a `.deb` path with a displaced snapshot of another suite.** The strip must not change `DeclaredHashesForPath`, either the exact lookup or the suffix fallback. Covered in Task 4's reader-equivalence test.
5. **A shutdown (ctx cancelled) while a GC op waits on the start deadline.** It returns `ctx.Err()`, it is not a failure, and nothing is logged as one. Covered in Task 1 (cache) and Task 2 (gc).

---

### Task 0: Commit the long-run harness and the benchmark, and record the baseline

**Files:**
- Create: `internal/gc/issue14_longrun_test.go`, from `/tmp/claude-1000/-home-sean-projects-apt-cacher-ultra/6e0b682e-b7d1-4662-903e-9099b4bf6ce7/scratchpad/vmain/internal/gc/issue14_repro_test.go`
- Create: `internal/cache/package_hash_bench_test.go`, from `/tmp/claude-1000/-home-sean-projects-apt-cacher-ultra/6e0b682e-b7d1-4662-903e-9099b4bf6ce7/scratchpad/vbench/internal/cache/zz_issue14_bench_test.go`
- Create: `docs/superpowers/plans/2026-11-09-issue-14-gc-followup-evidence.md`

**Interfaces:** Produces the env gates `ACU_LONGRUN=1` (the long-run test `TestIssue14LongRun`) and `ACU_BENCH=1` (`TestPackageHashBench`, with the knobs `ACU_BENCH_K`, `ACU_BENCH_REPS` and `ACU_BENCH_VARIANTS`).

- [ ] **Step 1:** Copy both files. Rename the tests as above and gate them on the env variables with `t.Skip`.
  - Add a header comment to each: what it measures, how to run it, and that it is excluded from CI by its gate.
  - Add an `AIDEV-NOTE` to each pointing to the evidence file.
  - Rename the repro's env variable from `ACU_REPRO` to `ACU_LONGRUN`.
- [ ] **Step 2:** Check that both compile and skip by default. Run `go vet ./internal/gc ./internal/cache && go test ./internal/gc -run 'TestIssue14LongRun' -v`. Expected: `--- SKIP`.
- [ ] **Step 3:** Run the baseline.
  - Run `ACU_LONGRUN=1 go test ./internal/gc -run TestIssue14LongRun -v -timeout 30m`.
  - Run `ACU_BENCH=1 ACU_BENCH_K=1,4 ACU_BENCH_VARIANTS=base go test ./internal/cache -run TestPackageHashBench -v -timeout 60m`.
  - Paste the `RESULT` and per-day lines into the evidence file, under "Before (18f652e + harness)", with the date and the machine.
- [ ] **Step 4:** Commit: `test: env-gated issue #14 long-run harness and package_hash benchmark (baseline)`.

### Task 1: The writer's start deadline

**Files:**
- Modify: `internal/cache/cache.go`. Add an atomic state to `writeReq`; update `submitWrite` (≈:256) and `writer` (≈:306) to use it.
- Modify: `internal/cache/testing_helpers.go`. Add `HoldWriterForTest`.
- Test: `internal/cache/write_start_deadline_test.go`

**Interfaces:**
- Produces:
  - `func WithWriteStartDeadline(ctx context.Context, t time.Time) context.Context`
  - `var ErrWriteNotStarted = errors.New("cache: write not started before its start deadline")`
  - `func (c *Cache) HoldWriterForTest(ctx context.Context) (release func(), err error)`. This submits an op that blocks until `release` is called, and returns once that op is running.

- [ ] **Step 1: Write the failing tests** in `write_start_deadline_test.go` (package `cache`):
  - `TestSubmitWrite_AQueuedOpAbandonedAtItsStartDeadlineNeverRuns`
    - Hold the writer.
    - Submit an op that sets a flag, with a start deadline 50 ms ahead.
    - Assert `errors.Is(err, ErrWriteNotStarted)` and that it returned within 50 ms plus 200 ms of slack.
    - Release the writer, then flush with a no-op write. Assert the flag is still false.
  - `TestSubmitWrite_AnOpStartedBeforeItsDeadlineRunsToCompletion`
    - The op blocks for 150 ms on a timer, then returns `errSentinel`. Its start deadline is 50 ms out, and the writer is free.
    - Assert that the returned error is `errSentinel` and that the op's ctx was not cancelled.
  - `TestSubmitWrite_StartDeadlineRaceRunsAtMostOnceAndReportsTruthfully`
    - Repeat 2,000 times: put a no-op 0–2 ms hold ahead, use a start deadline near now, and count runs with an atomic counter.
    - Assert that `err == nil` exactly when the op ran, and `ErrWriteNotStarted` exactly when it did not.
  - `TestSubmitWrite_NoStartDeadlineBehavesAsBefore`
    - A held writer and a submitter ctx cancelled at 50 ms: assert `context.Canceled` (today's path).
  - `TestSubmitWrite_ShutdownWhileWaitingForTheStartDeadlineIsCtxErr`
    - Hold the writer, use a start deadline 1 s out, and cancel ctx at 50 ms.
    - Assert `context.Canceled`, not `ErrWriteNotStarted`.
- [ ] **Step 2:** Run `go test -race ./internal/cache -run 'TestSubmitWrite_' -v`. Expected: FAIL (undefined `WithWriteStartDeadline`).
- [ ] **Step 3: Implement.**
  - `writeReq` gets a `state atomic.Int32` (pending=0, running=1, abandoned=2).
  - In the writer: `if req.state.CompareAndSwap(pending, running) { req.res <- req.op(req.ctx, conn) } else { req.res <- ErrWriteNotStarted }`.
  - In `submitWrite`, read the deadline from ctx (an unexported key). If there is one, add a `time.Timer` case to **both** selects. On fire, `CompareAndSwap(pending, abandoned)`:
    - if it succeeds, return `ErrWriteNotStarted`;
    - otherwise stop selecting on the timer and keep waiting for `res`, `closeCh` or `ctx.Done()`.
  - If the deadline has already passed on entry, return `ErrWriteNotStarted` without enqueueing.
  - Add an `AIDEV-NOTE` on `submitWrite` with the invariant: gates start only; never cancels a running op; why (issue #14 G2-RR1-M1).
- [ ] **Step 4:** Run `go test -race ./internal/cache -run 'TestSubmitWrite_' -count=3 -v`. Expected: PASS. Then run `go test -race ./internal/cache`. Expected: PASS.
- [ ] **Step 5:** Commit: `cache: a write can carry a start deadline (issue #14)`.

### Task 2: GC passes give up on a write they cannot start in time

**Files:**
- Modify: `internal/gc/snapshot.go`, `internal/gc/blob.go`, `internal/gc/urlpath.go`, `internal/gc/gc.go` (`runTick` doc and AIDEV-NOTE).
- Test: `internal/gc/start_deadline_test.go`

**Interfaces:**
- Consumes: Task 1's `cache.WithWriteStartDeadline`, `cache.ErrWriteNotStarted` and `(*cache.Cache).HoldWriterForTest`.
- Produces: none new.

- [ ] **Step 1: Write the failing tests** (package `gc`, over a real cache with `openTestCache`):
  - `TestRunTick_ASnapshotBatchStuckBehindTheWriterEndsAtItsDeadline`
    - Set up `MaxTickDuration = 1s`, `g.nextPass = snapshotPass` and one reapable blob (`seedReapableBlob`).
    - Hold the writer, and release it 50 ms after the snapshot deadline (passDeadlines: start + 800 ms).
    - Assert:
      - `tick.snapshotDeadlineReached` is true and `runTick` returns a nil error;
      - there is no `gc_tick_failed` in the log;
      - the blob pass reaped the blob;
      - the tick took ≤ 1 s + 300 ms.
  - `TestRunTick_ABlobBatchStuckBehindTheWriterEndsAtTheTickDeadline`
    - Set `g.nextPass = blobPass` and hold the writer past the tick's end.
    - Assert that `blobDeadline` is reported, the rotation stays at `blobPass`, there is no failure, and the prune was not counted as a failure.
  - `TestRunSnapshotPass_AFirstBatchNeverStartedIsADeadlineNotADrain`
    - Hold the writer through the deadline.
    - Assert that the pass returns `deadlineHit=true`, zero counts and a nil error.
  - `TestRetiredRun_AChunkNeverStartedReArms`
    - Use a held writer and `MaxTickDuration = 200ms`.
    - Assert that `retiredRun` returns `backlog=true` and that `gc_retired_run_complete` has `deadline_reached=true`.
- [ ] **Step 2:** Run `go test -race ./internal/gc -run 'StuckBehindTheWriter|NeverStarted' -v`. Expected: FAIL. Today the batch waits and then runs.
- [ ] **Step 3: Implement.**
  - Each pass derives `wctx := cache.WithWriteStartDeadline(ctx, deadline)` once and passes `wctx` to every store or cache write call:
    - url-path batches;
    - `RunSnapshotGCBatch` and the C chunks (and the D chunks in Task 5);
    - `PruneRepresentationMetadata` and `RunBlobGCBatch`;
    - `retiredRun`'s chunks, with its own deadline.
  - At each call site, `errors.Is(err, cache.ErrWriteNotStarted)` takes the pass's existing deadline branch. That logs `gc_tick_deadline_reached`, returns `deadlineHit=true` and returns no error.
  - Add a helper `func notStarted(err error) bool` in `gc.go`.
  - A prune that was not started is not `pruneErr`.
  - Update `runTick`'s issue #14 AIDEV-NOTE: a batch can no longer overrun its pass while it waits behind the writer. The overrun G2-RR1-M1 describes is now bounded by one already-started op.
- [ ] **Step 4:** Run `go test -race ./internal/gc` and `go test -race ./internal/cache`. Expected: PASS.
- [ ] **Step 5:** Commit: `gc: a pass gives up a write it cannot start before its deadline (issue #14)`.

### Task 3: Startup B retires; C deletes (supersedes HD-7's one-transaction delete)

**Files:**
- Modify: `internal/cache/gc.go`. Remove the `retireDisplaced` parameter from `RunSnapshotGCBatch` (≈:255); B always retires; update its doc and the HD-4/HD-7 AIDEV-NOTE.
- Modify: `internal/gc/gc.go` (the `gcStore` interface ≈:571; the `phaseStartup` doc ≈:165; `runTick`'s `phase` paragraph), `internal/gc/snapshot.go` (the call and the PF-15/HD-4/HD-7 notes), `internal/gc/metrics.go` (the help for `acu_gc_displaced_reaped_total`), and `internal/gc/gc_store_test.go` (the `fakeStore` signature).
- Modify: `internal/cache/gc_test.go`, and any other caller found by `grep -rn RunSnapshotGCBatch`.
- Docs:
  - `docs/configuration.md`: `snapshot_batch_size` and `max_tick_duration`.
  - `docs/admin-ui-spec.md`: the `gc.retired_reaped` row.
  - `docs/log-fields.md`: the `displaced_reaped` and `retired_reaped` rows.

**Interfaces:**
- Produces: `func (c *Cache) RunSnapshotGCBatch(ctx context.Context, batchSize int, staleGraceSeconds int64, keepDisplaced int) (SnapshotGCBatchResult, error)`. `gcStore` changes to match.

- [ ] **Step 1: Write the failing test.**
  - Add `TestStartupPass_RetiresTheDisplacedAndCDeletesThem` in `internal/gc/retired_test.go`:
    - Seed a suite with a current snapshot and 5 displaced snapshots (`KeepDisplaced=3`), each with members pinning a reapable blob.
    - Run `StartupPass`. Assert:
      - the 2 oldest displaced snapshots are gone;
      - `displaced_reaped=2` **and** `retired_reaped=2`;
      - their blobs are reaped in the same start.
  - Change `TestRunSnapshotGCBatch_DeletesTheDisplacedWhenNotRetiring` to `TestRunSnapshotGCBatch_AlwaysRetiresTheDisplaced`, asserting `retired_at` is set and the rows are kept.
- [ ] **Step 2:** Run the tests. Expected: they fail to compile or fail, because startup still deletes.
- [ ] **Step 3: Implement.**
  - Remove the delete-displaced branch and the parameter. Orphans are still deleted in the batch.
  - Update every note that says "the startup pass still deletes" to: since 2026-11-09 (issue #14 follow-up), startup retires too. The reason: C runs at startup (PF-15 re-ruled), and the start deadline now bounds every startup op.
  - Update the docs listed above. Startup displaced snapshots now count twice, like periodic ones.
- [ ] **Step 4:** Run `go test -race ./internal/cache ./internal/gc`. Expected: PASS.
- [ ] **Step 5:** Commit: `gc: startup retires displaced snapshots; sub-job C deletes them (issue #14)`.

### Task 4: Sub-job D in the cache, and the reader-equivalence pin

**Files:**
- Modify: `internal/cache/gc.go`. Add `RunDisplacedPackageHashChunk` after `RunRetiredSnapshotGCChunk`.
- Test: `internal/cache/displaced_package_hash_test.go`

**Interfaces:**
- Produces: `func (c *Cache) RunDisplacedPackageHashChunk(ctx context.Context, maxRows int) (rows int, more bool, err error)`. `maxRows <= 0` means `retiredChunkRows` (via `effectiveChunkRows`). `more` is false only when no candidate snapshot is left at the start of the call.

- [ ] **Step 1: Write the failing tests.**
  - `TestRunDisplacedPackageHashChunk_StripsOnlyAdoptedNonCurrentUnretired`. Fixture: one suite with:
    - a current snapshot;
    - 2 displaced snapshots;
    - 1 retired snapshot;
    - 1 unadopted candidate (no rows, since only CommitAdoption inserts rows);
    - 1 displaced snapshot queued in `legacy_trust_audit` (use `seedDisplacedAt(..., queued=true)` from `f24_gc_test.go`).

    Each adopted one has `package_hash`, `snapshot_member` and `snapshot_skipped_member` rows. Loop until `more == false`, then assert:
    - `package_hash` rows are 0 for the 2 displaced and the queued one;
    - the current and the retired snapshots are unchanged;
    - every `snapshot_member` and `snapshot_skipped_member` row and every `suite_snapshot` header is unchanged.
  - `TestRunDisplacedPackageHashChunk_ChunksALargeSnapshot`. A displaced snapshot with 25 rows and `maxRows=10`: the calls return (10, true), (10, true), (5, true), (0, false).
  - `TestRunDisplacedPackageHashChunk_NothingToDo`. An empty cache returns (0, false, nil).
  - `TestPackageHashReaders_AnswerTheSameAfterTheStrip`. Fixture:
    - two signed dists suites and one signed flat suite on the same host;
    - each has a current snapshot and 2 displaced ones;
    - displaced snapshots declare *different* hashes for paths the current ones also list, plus paths only they list;
    - one `.deb` path is shared across suites;
    - one flat row in a displaced snapshot only;
    - `url_path` rows with `last_requested_at` for the hot set.

    Record the answers of all of these, strip to completion, and assert the answers are deep-equal before and after:
    - `DeclaredHashesForPath`, for every seeded path plus a flat-fallback path;
    - `ListIntegrityCandidates`, sorted;
    - `GetRepoCoverage`;
    - `GetCacheSummaryByHostArch`;
    - `ComputeHotSet`, with each suite's current snapshot as the prior;
    - the url-path retention decision, `urlPathRetainedTx`, called for every seeded url_path inside a read-only transaction that is rolled back.

    This test pins the spec's audit table.
- [ ] **Step 2:** Run `go test -race ./internal/cache -run 'DisplacedPackageHash|AnswerTheSameAfterTheStrip' -v`. Expected: FAIL (undefined).
- [ ] **Step 3: Implement.** Model it on `RunRetiredSnapshotGCChunk`: one `submitWrite` transaction, select then delete.

  ```sql
  SELECT ss.snapshot_id FROM suite_snapshot ss
   WHERE ss.adopted_at IS NOT NULL AND ss.retired_at IS NULL
     AND ss.snapshot_id NOT IN (SELECT current_snapshot_id FROM suite_freshness WHERE current_snapshot_id IS NOT NULL)
     AND EXISTS (SELECT 1 FROM package_hash p WHERE p.snapshot_id = ss.snapshot_id)
   ORDER BY ss.snapshot_id LIMIT 1
  ```

  Then run C's `DELETE … WHERE rowid IN (SELECT rowid FROM package_hash WHERE snapshot_id = ? LIMIT ?)`.
  - Add an `AIDEV-NOTE` holding the audit argument in brief:
    - every reader is current-only or reads the pre-flip prior;
    - displaced snapshots never become current again;
    - the blob reachability predicate does not read `package_hash`;
    - the reader-equivalence test pins this, and a new reader of displaced rows must change it.
  - Run `EXPLAIN QUERY PLAN` on the SELECT and confirm that the EXISTS uses `idx_package_hash_snapshot`. Put the plan in the note.
- [ ] **Step 4:** Run `go test -race ./internal/cache`. Expected: PASS.
- [ ] **Step 5:** Commit: `cache: sub-job D strips displaced snapshots' package_hash rows in chunks (issue #14)`.

### Task 5: Run D in the snapshot pass and the retired run; sub-job failures don't stop later sub-jobs

**Files:**
- Modify: `internal/gc/snapshot.go`:
  - `snapshotPassResult` gets `packageRowsStripped int`;
  - add `runSubJobD`;
  - rework `runSnapshotPass`'s error flow;
  - `retiredRun` runs C then D and re-arms when either hits its deadline.
- Modify: `internal/gc/gc.go`: the `gcStore` method, `tickResult.displacedPackageRowsDeleted`, `LastRunSummary.DisplacedPackageRowsDeleted`, and the `gc_run_complete` fields in `StartupPass` and `periodicTick`.
- Modify: `internal/gc/metrics.go`: add the counter `acu_gc_displaced_package_rows_deleted_total`.
- Modify: `internal/gc/gc_store_test.go`. `fakeStore` gets `RunDisplacedPackageHashChunk` with `dChunksMore int`, `dChunkErrs bool` and `dChunks int`.
- Docs:
  - `docs/log-fields.md`: `displaced_package_rows_deleted` in `gc_run_complete` and `gc_retired_run_complete`;
  - `docs/configuration.md`: the `keep_displaced` row, which now keeps headers and index files, not package lists;
  - the `KeepDisplaced` comment in `internal/config/config.go`.

**Interfaces:**
- Consumes: Task 4's `RunDisplacedPackageHashChunk`, and Task 2's `notStarted`.
- Produces:
  - `gcStore.RunDisplacedPackageHashChunk(ctx context.Context, maxRows int) (rows int, more bool, err error)`
  - `LastRunSummary.DisplacedPackageRowsDeleted int`
  - the log field `displaced_package_rows_deleted`

- [ ] **Step 1: Write the failing tests** (`internal/gc/subjob_d_test.go`):
  - `TestRunSnapshotPass_RunsDAfterC`. Using `fakeStore`, assert the call order: batches, C chunks, then D chunks. Assert that `displaced_package_rows_deleted` in `gc_run_complete` equals the fake's rows.
  - `TestRunSnapshotPass_AFailingCStillRunsD`. With `chunkErrs=true` and `dChunksMore=2`, assert:
    - D ran 3 calls;
    - the pass returns C's error;
    - there are exactly two WARN `gc_tick_failed` lines if D fails too, and one if only C fails;
    - the rotation moves on.
  - `TestRunSnapshotPass_AFailingBatchStillRunsCAndD`. Same shape, with `batchErrs=1`.
  - `TestRetiredRun_RunsDAndReArmsOnItsBacklog`. D hits the deadline (`dChunksMore` large, a small `MaxTickDuration`, and a chunk sleep inside a synctest bubble). Assert backlog=true.
  - `TestPeriodicTick_StripsDisplacedPackageRowsOnARealCache`. Use a real cache, a suite with 3 snapshots each holding package rows, and `KeepDisplaced=3`. After one tick, assert:
    - the displaced snapshots' `package_hash` count is 0;
    - their headers and members still exist;
    - the current snapshot's rows are intact;
    - `LastRunSummary.DisplacedPackageRowsDeleted` equals the stripped count.
- [ ] **Step 2:** Run `go test -race ./internal/gc -run 'RunsDAfterC|StillRunsD|RunsDAndReArms|StripsDisplacedPackageRows' -v`. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - `runSnapshotPass` runs A ∪ B until drained or until its deadline, or until it fails. On failure it logs its own WARN `gc_tick_failed`, then goes on to C and then D, each under the same `expired` and the same start deadline. It returns the first error.
  - So that each sub-job failure is logged once, `runTick`'s `failed` must not log the snapshot pass's error a second time. Restructure so that the snapshot pass returns `(result, deadlineHit, firstErr, logged bool)`, or move the logging into the sub-jobs and have `runTick` skip it for that pass. The builder picks one; the test counts the WARN lines.
  - Extend `runSnapshotPass`'s AIDEV-NOTE: HD-15 applies within the pass (2026-11-09) because a C stuck on one snapshot would otherwise stop D forever, defeating the strip.
  - `retiredRun` gets the same treatment.
- [ ] **Step 4:** Run `go test -race ./internal/gc`. Expected: PASS.
- [ ] **Step 5:** Commit: `gc: run sub-job D in the snapshot pass and the retired run (issue #14)`.

### Task 6: Visibility — failed passes and the representation prune

**Files:**
- Modify: `internal/gc/metrics.go`: add `acu_gc_pass_failures_total{pass}` and `acu_gc_representation_rows_pruned_total`. The prune's duration goes to the existing `acu_gc_pass_duration_seconds{phase,pass="representation_prune"}`.
- Modify: `internal/gc/gc.go`:
  - `LastRunSummary` gets `FailedPasses []string`, `RepresentationRowsPruned int` and `RepresentationPruneSeconds float64`;
  - `runTick`'s `failed` takes a pass name, and the snapshot pass's logged failures are counted too;
  - `StartupPass`: pool-scan failure → `pass="pool_scan"`;
  - the `gc_run_complete` fields.
- Modify: `internal/gc/blob.go`: record the prune's count and duration in `blobResult`.
- Modify: `internal/gc/snapshot.go`: `retiredRun` counts `pass="retired"`.
- Modify: `internal/admin/status.go`:
  - `gcInfo` gets `LastRunFailedPasses []string \`json:"last_run_failed_passes"\``, `LastRunDisplacedPackageRowsDeleted int \`json:"last_run_displaced_package_rows_deleted"\`` and `LastRunRepresentationRowsPruned int \`json:"last_run_representation_rows_pruned"\``;
  - `gcInfoFromSummary`;
  - `vitalState("gc")`.
- Modify: `internal/admin/status_template.go`. In the GC panel, add rows for *Failed passes* (`b--crit` badges per pass, `b--ok` "none"), *Package rows stripped* and *Representation rows pruned*. Update the issue #14 AIDEV-NOTE in the template.
- Test: `internal/gc/visibility_test.go`, `internal/admin/status_test.go` (`TestVitalState` cases), and the JSON contract or golden test files the admin package uses. Regenerate goldens with its documented flag.
- Docs:
  - `docs/admin-ui-spec.md` §9.1: the GC row's crit becomes `.GC.LastRunFailedPasses` non-empty; rewrite the "GC (issue #14)" paragraph and add the new fields to the gc.* table.
  - `docs/log-fields.md`: the new `gc_run_complete` fields.
  - Prometheus metric docs, wherever `acu_gc_deadline_reached_total` is documented (grep).

**Interfaces:**
- Consumes: Task 5's `LastRunSummary.DisplacedPackageRowsDeleted`.
- Produces: the JSON fields named above and the metric names above.

- [ ] **Step 1: Write the failing tests:**
  - `TestRunTick_RecordsEachFailedPass`. Make url_path, snapshot and blob fail in separate cases (fakeStore `batchErrs`, `pruneErrs`, …). Assert `FailedPasses` equals `["snapshot"]`, and so on, and that the counter rose by 1 for that label.
  - `TestRetiredRun_CountsAFailure`. Use `chunkErrs`. Assert `acu_gc_pass_failures_total{pass="retired"}` +1 and that `LastRunSummary` is unchanged.
  - `TestStartupPass_APoolScanFailureIsCounted`. Make the pool dir unreadable, or use the existing pool-scan failure seam (grep `runPoolScan` tests). Assert `{pass="pool_scan"}` +1.
  - `TestRunTick_ANotStartedWriteIsNotAFailure`. Use Task 2's held-writer setup and assert that `FailedPasses` is empty and the counter is unchanged.
  - `TestRunBlobPass_ReportsThePrunesCountAndDuration`. Seed 5 prunable dates with `seedPruneFixture`. Assert `gc_run_complete` has `representation_rows_pruned=5` and `representation_prune_ms` ≥ 0, and that the counter rose by 5.
  - `TestVitalState` new cases:
    - a `GC` with `LastRunFailedPasses=["blob"]` → `"crit"`;
    - with a deadline reached and no failure → `"warn"` (unchanged);
    - a failure plus a reached deadline → `"crit"`.
  - A JSON contract test: the full `gc` block carries the three new keys, and the abbreviated pre-first-run form still emits only `last_run_unixtime`.
- [ ] **Step 2:** Run `go test -race ./internal/gc ./internal/admin`. Expected: FAIL.
- [ ] **Step 3: Implement** as listed in Files. `FailedPasses` uses the labels `url_path`, `snapshot` and `blob`, in tick order, without duplicates. Rewrite the `vitalState` issue #14 note to say: a reached deadline stays warn; a failed pass is crit since 2026-11-09 (the maintainer), even though a transient failure reads crit until the next tick or start.
- [ ] **Step 4:** Run `go test -race ./internal/gc ./internal/admin`. Expected: PASS.
- [ ] **Step 5:** Commit: `gc, admin: count and show failed GC passes; report the representation prune (issue #14)`.

### Task 7: Decisions, Changelog, release notes, and the after-measurement

**Files:**
- Modify: `docs/open-decisions.md`:
  - *Decided*: a new entry, "Issue #14 follow-up: displaced snapshots' package rows stripped, GC writes start-bounded, failed passes crit" (the maintainer, 2026-11-09). It names the superseded parts of HD-7 and of §9.1's "no GC state is crit", gives the pins (the test names from Tasks 1–6) and gives the before/after numbers.
  - Move "No metric counts a failed GC pass" to *Decided* (pointing at the new entry).
  - In "The representation prune's scan grows…", note that the metric has been added and the keyset is still open.
  - In the issue #14 *Decided* entry's "Not done" sentence, add "see the 2026-11-09 follow-up".
- Modify: `Changelog.md` and `docs/release-notes/1.1.0.md`. Make sure the lines from Tasks 3, 5 and 6 exist. The release notes cover:
  - the `keep_displaced` meaning change;
  - the startup counts change (displaced counted twice);
  - the new metrics and the crit GC vital, with the alert line `increase(acu_gc_pass_failures_total[1d]) > 0`;
  - that the database file keeps its size (the pages are reused; `VACUUM` is optional, offline).
- Modify: `docs/superpowers/plans/2026-11-09-issue-14-gc-followup-evidence.md`: the "After" section.

- [ ] **Step 1:** Re-run the audit grep: `grep -rn "package_hash" --include=*.go internal cmd | grep -v _test.go`. Compare the result with the spec's audit table. Expected: no reader that is not in the table.
- [ ] **Step 2:** Run the after-measurement with Task 0's commands. Record it under "After". The benchmark's "after" runs at the post-strip table size: `ACU_BENCH_K=1` represents the stripped steady state (current snapshots only), so compare base k=4 (before) with k=1 (after).
- [ ] **Step 3:** Write the docs above.
- [ ] **Step 4:** Run the full verification: `make test-race` and `make lint`. Expected: both pass. Report any failure verbatim and do not paper over it.
- [ ] **Step 5:** Commit: `docs: record the issue #14 follow-up decisions and measurements`.
