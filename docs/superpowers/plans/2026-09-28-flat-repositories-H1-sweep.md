# Flat repositories — Phase H1: The sweep — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the flat-repository branch's open work before its end-to-end tests and docs (H2, H3): F-24's never-downgrade gap with `gc.keep_displaced = 0` (ruling R-H1), the maintainer's corrupt-anchor decision (ruling R-H2), snake_case names for every prose log message this branch added, with a guard that fails a new one (rulings R-H3, R-H6), the overview's H carry-forwards (the admin `retired_reaped`, the anchorless refresh made visible, the log-fields gaps, the 500-suite simulation under budget), and a disposition for every one of the 136 deferred minors of phases A–G, after which the seven deferred files are deleted.

**Architecture:** No new package, no new config key, no schema change. `internal/cache` gains one predicate in snapshot-GC sub-job B (a snapshot still queued for the legacy trust audit is never deleted, Task 1) and one effect in the audit's quarantine transaction (an `anchor_corrupt` quarantine deletes the `url_path` rows that hold the corrupt blob, and the audit removes the corrupt pool file before the batch commits, Task 2). The renames are mechanical (Task 3). The status page and JSON gain additive fields (`suites[].anchorless`, `recent_adoptions[].scheme`, `gc.retired_reaped`) and two HTML cells (Task 4); the admin endpoints share one form parser (Task 5); the reaper gains a per-run line and a liveness gauge (Task 6); GC's goroutines and its startup pass recover panics behind a narrow store seam (Task 7). The rest are bounded fixes in the files the deferred items name, each with the test its item asked for (Tasks 8–16); Task 17 is the definition of done and Task 18 the documentation, spec revision 12, the overview and the deletion of the seven deferred files.

**Tech Stack:** Go 1.25 (toolchain 1.26), modernc SQLite, `testing/synctest`.

**Spec:** `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (revision 11; Task 18 records this plan's rulings and design notes as revision 12) — §6.1 (the legacy trust audit; R-H2 and F-24 change it), C4 (Dispatch 3–4 for the anchorless state the status page now shows), C5 (GD-2's Sources wording), C7 (the status page), C9 (the reaper's run line; sub-job B), C11 (events, metrics, constants), §7, §9's H row, §10 (GD-5 joins the follow-ups). Overview: `docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md` — Global Constraints, the "H:" blocks under "Carry-forwards" and "PR #8 parity", the "Open decision" block. Deferred minors: `…-A-deferred.md` through `…-G-deferred.md` (136 IDs; every one has a row in the disposition table below). H2 (`e2e/flat`) and H3 (README, release notes, strict-mode docs, the live-upstream smoke, the final full-diff review) are planned separately; this plan touches neither `e2e/` nor `README.md`.

## Revision 2026-10-25: the two design reviews

The draft (eadfd80) had a design review of its code tasks (one Critical, one Important, eleven Minor) and one of its dispositions, docs and spec (one Important, eleven Minor); the controller's rulings on them are binding. Every finding is adopted; every task this revision changes was replayed on a fresh copy of eadfd80 (red and green steps, mutation rows, the module-wide checks).

- **Code C-1** (R-H2 did not heal an upstream that still serves the same `InRelease`: the miss path's `Finalize` keeps an existing `pool/<hash>`): the audit also removes the corrupt pool file with `DiscardFinalizedBlob`, on main's goroutine before the batch commits, never on the writer; a failed removal is ERROR `legacy_anchor_discard_failed` and the quarantine proceeds; the handler test runs an unchanged and a changed upstream, two GETs each (Task 2; R-H2, design note 2, Global Constraints, Review Focus 1, spec §6.1, `docs/configuration.md`, "For H3"). The controller's planner brief had said R-H2 "never touches the blob file itself"; that was the controller's own elaboration, not the maintainer's words, and the controller amended it: the maintainer chose the row delete so that the next request fetches a fresh copy, which with an unchanged upstream only the removal achieves; the integrity scanner and the adopter already remove corrupt pool files this way; the corruption is a hash mismatch of bytes read, not an I/O fault; and nothing serves during the audit.
- **Code I-1** (`GC.StartupPass` unrecovered: a crash loop that never serves): it returns a panic as an error, which main logs before it serves (Task 7, design note 11).
- **Code M-1** (a pass that panics every tick starves the passes after it): the rotation moves past it (Task 7). **M-2** (E-42 took the counters out of every boundary): a second boundary (Task 9). **M-3** (B-63's "next scheduled check" overstates): "backoff ends in …" / "backoff over" (Task 4, design note 6). **M-4** (R-H7's resolution): a sentence (Task 13). **M-5** (simulations near the budget): Task 17 records the three with `-p 1` and the CPU count and flags any above 4.0 s. **M-6** (the guard's messages): they say where to fix (Task 3). **M-7** (`evidence` joined by spaces): a list (Task 6). **M-8** (`source_paths_skipped` at every adoption): INFO (Task 10). **M-9** (persistently deferred snapshots keep their rows): the R-H1 AIDEV-NOTE and the upgrade text name the remedy and PF-12's one exception (Tasks 1, 18). **M-10**: an AIDEV-NOTE pins `parseSuiteForm` after the guard (Task 5). **M-11** (`anchorless` before the first check): the one-check exception is named (Task 4, design note 5, `docs/admin-ui-spec.md`). The three underspecified points are settled: the discard failure (by the ruling), `periodicTick`'s line, metrics and summary staying inside the recovered function (Task 7), `anchor_rows_deleted` on every quarantine line, 0 but for `anchor_corrupt` (`docs/log-fields.md`).
- **Sweep I-1** (C-39's race shows in the code): C-39 becomes a test-only fix (Task 15): both chaos tests repeat the trigger until the gate has a waiter; red with the suite held across the first trigger GET.
- **Sweep M-1** (counts) and **M-2** (spec C5 already said what the plan called a correction): corrected (counts, deviations, design note 12, GD-2's row, Task 10). **M-3** (B-10's reason): apt's tag parser trims the same way (the reviewer's apt 3.3.2 probe, not re-run here). **M-4** (GD-4's reason) and **M-5** (B-63 for a dormant or record-paced suite, which shows the count alone): adopted (Task 4). **M-6** (the 11 renamed DEBUG events lost their meaning): six are renamed to carry it, the 11 share one `docs/log-fields.md` row, and every renamed event is documented (Task 3). **M-7** (configuration.md's scheme wording), **M-8** (overview gaps), **M-9** (where a deleted ID's text lives): adopted (Task 18). **M-10** (R-H5 is a controller ruling): the overview and spec say "for the maintainer's confirmation", and the final report asks. **M-11** (nothing checks the new events are documented): Task 18 adds `TestEveryEventIsDocumented` — every snake_case event needs a `docs/log-fields.md` row, `main`'s 87 without one listed — and a DoD row; it found one more gap, phase E's `adoption_repair_declarations_unavailable`, now documented.

Counts: fix 111, done 2, declined 23 (C-39 moved from declined to fix).

## Global Constraints

Everything in the overview's Global Constraints applies. In particular:

- No schema change: every fix here reads or writes v7's columns (`legacy_trust_audit`, `url_path`, `suite_freshness`, `suite_snapshot`). Never edit an entry of `migrations`.
- Production SQL never uses SQLite clock functions; persisted stamps come from `wallclock` (the cache's `nowUnix`, the checker's `now`). In-memory timing is monotonic.
- `internal/cache` never imports `internal/freshness` or `internal/proxy`; `internal/admin` does not import `internal/freshness` (the check backoff reaches it as a function in `admin.Config`, Task 4). The writer goroutine never runs foreign code or pool I/O: R-H2's row delete is SQL in the quarantine's own writer transaction, and its pool-file removal runs on the audit's goroutine (main's, before anything serves), as the integrity scanner's does, never on the writer.
- Every phase F rule about suite-row deletes holds (PF-10, PF-12, R-F1): this plan adds no suite-row delete. R-H2 deletes `url_path` rows through `deleteURLPathRowTx`, the one row delete that releases a row's pin once.
- An I/O fault is never evidence: R-H2 acts only on bytes the audit read and hashed (`anchor_corrupt`); `anchor_oversized` (not read in full) and a read error (deferred) change nothing they did not change before.
- Tests that call `wallclock.SetOffsetForTest` never use `t.Parallel()`. Simulations run inside `testing/synctest` bubbles, finish in under 4.5 s under `-race` on a quiet machine (ruling PE-8), sample goroutines only after `synctest.Wait()`, and silence loggers (`simtest.DiscardLogger()`, `discardLogger()`, `silentLogger()`), or capture them. Ruling R-H4: every simulation a task touches is measured before and after, and the numbers go in the task.
- Every goroutine under `internal/freshness` and `internal/gc`, and every new handler or admin goroutine, recovers panics. Task 7 adds the recovers `internal/gc` lacked (F-21), and the startup pass, which runs on main's goroutine, returns its panic as an error (review I-1); no task starts a new goroutine outside a recover.
- `go test -race ./...` and `golangci-lint run ./...` are clean; `gofmt -l .` lists only the five known drift files (`cmd/apt-cacher-ultra/ca_print.go`, `internal/cache/cache_test.go`, `internal/cache/gc_urlpath_test.go`, `internal/freshness/hot_prefetch_test.go`, `internal/proxy/connect_stats.go`) at the end of every task. Never run `gofmt -w` on those five; edit them by hand (Tasks 14 and 15 do, for D-9 and D-56).
- `AIDEV-NOTE`s on every changed invariant; existing `AIDEV-*` comments are updated, never removed. A comment that cites a deferred minor ("deferred minor D-25") keeps its citation when the deferred files are deleted (design note 16).
- Commit trailer: the implementer's own harness attribution line (phase A ruling); otherwise `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

Conventions in this plan:

- Each task's **Interfaces** block lists what it uses from other tasks and existing test files. Never redefine a helper: a second definition in the same package does not compile.
- Each commit step starts with `gofmt -l <files>`; it must print nothing. Run `gofmt -w` on the non-drift files a step edits before that check; never on the five drift files.
- A "verify failure" step whose expected result is a compile failure is complete when the compiler names the undefined identifiers listed; such a step runs `go test -gcflags=-e …` so every error is listed.
- Mutation checks are run one at a time, each reverted before the next; record each failing test line in the commit message. Every red step, green run and mutation row in this plan was replayed on a scratch copy of the tree at 6786529 with the tasks before it applied; each row names the test line that failed there. (The branch's next commit, c8e6fe4, adds only the H2 plan. The revision of 2026-10-25 replayed each task it changed on a fresh copy of eadfd80, this plan's draft, whose code is 6786529's.)
- Edits to existing files are given as exact `old`/`new` blocks (or a whole function that replaces the one of the same name); each `old` block occurs once in the file as it stands after the previous tasks.
- Event names in code, tests and docs are the snake_case names of Task 3 from Task 3 on; the tasks before it use the prose names as they stand.
- **Existing tests that change.** Task 2 changes `ApplyLegacyAuditDecisions`' result to `map[int64]LegacyAuditEffect` (`applyOne` and the freshness audit read `.WasCurrent`). Task 3 renames the 43 messages in every test that matches them, and points `TestCheck_HealthySuiteAsksForNoRepairSlot`'s absence check at the new event (it matched a fragment, so it would have passed vacuously). Task 4 regenerates `TestJSONContractPreserved`'s golden (three additive fields) and changes `suites_healthy_no_lag`'s fixture (B-60). Task 5 changes the admin's `http.Error` bodies to one trailing newline (F-6; the tests trim newlines already) and `TestEndpoint_Forget_LogsAcceptedRequestsAndFailures`'s key (F-16). Task 9 makes `TestAdoptionFailureRecordCutByShutdownIsNotAWarning` assert its one DEBUG line (E-43) and changes `TestDoD_ActiveSuitesAdoptOnceDormantSuitesStaySilent`'s tick (E-56). Task 11 changes `TestServeHTTP_StrictWaitRefusals`' first loop and `TestServeHTTP_CoalescedCreatingMissFiresT1Once` (E-59, E-66). Task 13 rewrites `TestSimScheduler_FiveHundredStallingOneHealthy`'s first phase and setup (C-38) without dropping an assertion, checks every five minutes instead of two in `TestCheck_BothSignedFormsFailingUnderJitterStayWithinTheirBounds` (R-H7), and tightens the simulations its items name (E-50–E-53, E-55, F-10, F-11; `internal/flipgate`'s `clients` helper, C-7). Task 14 rewrites `TestInsertCandidateSnapshot_Rejects*` (D-9) and adds rows or assertions to the tests its items name. Task 15 gives silent loggers to the tests it lists, moves `TestOpenCache_LogsNothing`'s assertion after the cleanups (D-60) and reads a decoded record in `TestWarnSuiteCaps_ZeroCapAndReadFailure` (E-73). Task 15 also makes `TestPhase2Chaos_AdoptionFlipUnderConcurrency` and `TestPhase2DetachedChaos_AdoptionFlipUnderConcurrency` hold the suite across their first trigger GET and trigger until the gate parks the adoption (C-39). Task 16 renames or re-documents the tests its items name and moves `deferred_e_test.go`'s nine tests verbatim (E-46). Any other existing test that fails means the change is wrong.

## Rulings and design notes

Controller rulings (binding):

- **R-H1 — F-24, the never-downgrade gap with `gc.keep_displaced = 0`.** Snapshot-GC sub-job B (`RunSnapshotGCBatch`'s displaced class) never selects a snapshot still queued in `legacy_trust_audit`; it is excluded before the `ROW_NUMBER()` ranking, as retired rows are, so it takes no `keep_displaced` slot either. Chosen over PF-12's `signedHistorySnapshotSQL` upsert before the delete, for three reasons. (1) The audit is the authority on legacy trust, and a deferred snapshot is one it has not judged yet: deleting it turns the next audit's verdict into `LegacyAuditDrop`, and the upsert would write a marker for a snapshot the audit may still find to be plain text or corrupt — no marker, by §6.1's design, so that such a suite later adopts unsigned without a false refusal. Kept, the snapshot waits for the verdict: Keep marks the base, a quarantine retires it, and sub-job B or C then deletes it. (2) Never-downgrade's rule stays in one place and unchanged: `refuseUnsignedDowngrade`'s displaced clause already counts such a snapshot as signed history (design review M-8); the fix only stops GC from removing what the rule reads. (3) It is one predicate on a table that is empty after the first start, save for snapshots an I/O fault deferred. Cost: a snapshot whose anchor stays unreadable at every start keeps its rows until its suite is reaped or forgotten (then `markSnapshotHistoryTx` marks its base and sub-job C deletes it) — bounded by the pre-v7 adopted snapshots. Pinned by a probe that fails at HEAD (Task 1).
- **R-H2 — a corrupt stored anchor (the maintainer's decision, 2026-10-24).** An `anchor_corrupt` quarantine also deletes every `url_path` row whose `blob_hash` is the snapshot's stored `InRelease` hash — under any scheme and host, since a content-addressed pool file is shared and every row that names it serves the same bad bytes — through `deleteURLPathRowTx` (each row's pin released once). The audit also removes the corrupt pool file with `Cache.DiscardFinalizedBlob`, the integrity scanner's remedy, on main's goroutine before the batch's `ApplyLegacyAuditDecisions`, never on the writer (PD-5): the miss path's `Finalize` keeps an existing `pool/<hash>`, so refetching an upstream `InRelease` that has not changed — the usual bit rot — would store onto, and serve, the rotted file. A corrupt file becomes a missing one, which the next request heals. Removing it before the commit is crash-safe: a lost commit re-audits the snapshot as `anchor_missing`. A failed removal is logged (ERROR `legacy_anchor_discard_failed`) and the quarantine proceeds; its rows still go, so a changed upstream still heals (review C-1; the revision note says why the controller amended its own wording here). **In the quarantine's own transaction**, not right after: `ApplyLegacyAuditDecisions` dequeues the snapshot in that transaction, so a delete in a later one that a crash or a failed write lost would never be retried — the audit would not see the snapshot again — and the rows would serve the bad bytes for good, the state before this ruling. In one transaction the quarantine and the delete commit or roll back together, and §6.1's "each batch commits with its outcomes, so an interrupted audit resumes without repeating effects" holds for the delete too. `anchor_oversized` (not read in full, so not proven bad) and `anchor_missing` are unchanged: a row whose pool file is missing already falls through to the miss path (`serveBlobWithHeaders` reports not served), so it heals at the next request. The decision carries a new field (`LegacyAuditDecision.DropAnchorRows`) and the result reports what was deleted (`LegacyAuditEffect.AnchorRowsDeleted`, logged as `anchor_rows_deleted` on `legacy_snapshot_quarantined`). Task 2; spec §6.1 revision 12; the overview's open decision is closed.
- **R-H3 — snake_case event names.** The 19 prose events the overview lists are renamed, each keeping its level and fields (table below; Task 3). Ruling R-H6 extends the rename to the other 24 prose messages this branch added; the 78 on `main` are left (design note 3).
- **R-H4 — the simulation budget.** Every simulation a task touches is measured under `-race` before and after; the numbers are in each task. `TestSimScheduler_FiveHundredStallingOneHealthy` goes from 7.0 s to 2.9 s (Task 13, C-38).
- **R-H5 — GD-5, decided for 1.1.0: keep the current behaviour.** An adopted unsigned flat suite whose `Release` stays absent keeps its coherent snapshot: it shows "refused: anchor_absent", logs `adoption_refused_persistently`, and `POST /forget` recovers it, as *Flat repositories* documents. Reasons: it is rare in the field; the kept snapshot keeps serving cached content when an upstream dies outright, where an automatic drop would turn that into 404s; an automatic drop would add a state transition with trust and serving consequences late in the release. Cost if wrong: an operator runs `/forget` on a rare suite after an alert. A controller ruling on a question that concerns the bytes clients are served, so it stands for the maintainer's confirmation (review sweep M-10; the final report asks): Task 18 records it in spec §10 (item 12: decided for 1.1.0, keep; revisit if seen in the field) and among the overview's decided entries; GD-5's row is declined.
- **R-H6 — every prose log message this branch added is renamed**, not only R-H3's 19: the 24 of the second table below. They are unreleased, so renaming is free now; after 1.1.0 a rename would break operators' log alerts. R-H3's rules hold: each keeps its level and fields; `docs/log-fields.md`, the tests and the AIDEV-NOTEs that quote one change with it; the prose messages `main` already logs stay. Task 3's guard covers them and more: every log message of production code must be snake_case or one of `main`'s 78 prose messages, listed in the test, so a new prose message fails the build (design note 3).
- **R-H7 — `TestCheck_BothSignedFormsFailingUnderJitterStayWithinTheirBounds` falls under PE-8's intent**: `go test -race` stays fast and free of timing flakes, and an 8 s test is the risk a 4-vCPU runner exposes. A synctest bubble cannot shorten it — the rig's clock is already virtual and the test never waits on wall time; its 7.6–8.0 s under `-race` are CPU, 1,440 checks of about 5 ms — so it is coarsened under 4.5 s with every assertion kept: a check every five minutes instead of two, 3.39–3.46 s (Task 13 Step 7).

| # | Prose message (phase) | Event | Emitted at |
|---|---|---|---|
| 1 | `suite onboarding failed` (D) | `suite_onboarding_failed` | `internal/handler/suite_rows.go` |
| 2 | `suite signed-seen mark failed` (D) | `suite_signed_seen_mark_failed` | `internal/handler/suite_rows.go` |
| 3 | `suite cap usage read failed` (D) | `suite_cap_usage_read_failed` | `cmd/apt-cacher-ultra/main.go` |
| 4 | `freshness: read anchor rows failed` (E) | `freshness_read_anchor_rows_failed` | `internal/freshness/first_adoption.go`, `current_form.go` |
| 5 | `freshness: read current snapshot failed` (E) | `freshness_read_current_snapshot_failed` | `internal/freshness/current_form.go` |
| 6 | `adoption: served anchor read failed; the flip barrier runs` (E) | `adoption_served_anchor_read_failed` | `internal/freshness/flip_barrier.go` |
| 7 | `adoption: signed-anchor re-mark failed` (E) | `adoption_signed_anchor_remark_failed` | `internal/freshness/adoption.go` |
| 8 | `suite reaper: list suites failed` (F) | `suite_reaper_list_suites_failed` | `internal/freshness/reaper.go` |
| 9 | `suite reaper: read suite row failed` (F) | `suite_reaper_read_suite_row_failed` | `internal/freshness/reaper.go` (`stepReadRow`) |
| 10 | `suite reaper: delete metadata failed` (F) | `suite_reaper_delete_metadata_failed` | `internal/freshness/reaper.go` (`stepDeleteMetadata`) |
| 11 | `suite reaper: reap failed` (F) | `suite_reaper_reap_failed` | `internal/freshness/reaper.go` (`stepReap`) |
| 12 | `suite reaper: list unadopted anchors failed` (F) | `suite_reaper_list_unadopted_anchors_failed` | `internal/freshness/reaper.go` |
| 13 | `suite reaper: junk anchor reads failed` (F) | `suite_reaper_junk_anchor_reads_failed` | `internal/freshness/reaper.go` |
| 14 | `suite reaper: marker sweep failed` (F) | `suite_reaper_marker_sweep_failed` | `internal/freshness/reaper.go` |
| 15 | `admin: forget failed` (F) | `admin_forget_failed` | `internal/admin/handlers.go` (two calls) |
| 16 | `freshness: anchorless index over its size cap, dropped for the miss path` (G) | `freshness_anchorless_index_over_cap` | `internal/freshness/anchorless.go` |
| 17 | `freshness: delete anchorless rows failed` (G) | `freshness_delete_anchorless_rows_failed` | `internal/freshness/anchorless.go` |
| 18 | `suite reaper: list unadopted flat indexes failed` (G) | `suite_reaper_list_unadopted_flat_indexes_failed` | `internal/freshness/reaper.go` |
| 19 | `suite reaper: read signed evidence failed` (G) | `suite_reaper_read_signed_evidence_failed` | `internal/freshness/reaper.go` |

The naming rule: the prose's subject (`freshness`, `adoption`, `suite reaper`, `admin`, `suite`) and its words, lower-case, joined by `_`, without punctuation or a trailing clause (`; the flip barrier runs`), so the old text finds the new name; row 16 shortens its clause to `over_cap`.

Ruling R-H6's 24, by the same rule. Rows 28 and 41 shorten a long clause (`… or the next refresh check`, `, retried next run`), rows 32 and 33 drop an article, and rows 23, 29–31 and 34 keep their meaning in other words (review sweep M-6: `…_sniff_failed_kept`, `…_dormant_no_client_created_row`, `…_dormant_no_recent_interest`, `freshness_check_skipped_no_baseline_anchor_hash`, `…_paced_until_retry`); row 21, the `adoption.require_signature = false` startup notice, whose one-sentence message was its explanation, becomes `adoption_require_signature_disabled` and gets a `docs/log-fields.md` row:

| # | Prose message | Event | Level | Emitted at |
|---|---|---|---|---|
| 20 | `adoption: read prior snapshot failed` | `adoption_read_prior_snapshot_failed` | WARN | `internal/freshness/adoption.go` |
| 21 | `adoption.require_signature = false: an empty keyring is allowed at startup; …` | `adoption_require_signature_disabled` | WARN | `cmd/apt-cacher-ultra/main.go` |
| 22 | `displaced snapshot lookup failed` | `displaced_snapshot_lookup_failed` | WARN | `internal/handler/handler.go` |
| 23 | `freshness: anchorless index failed its sniff, kept` | `freshness_anchorless_index_sniff_failed_kept` | DEBUG | `internal/freshness/anchorless.go` |
| 24 | `freshness: check cancelled` | `freshness_check_cancelled` | DEBUG | `internal/freshness/check.go` (×3), `current_form.go`, `first_adoption.go`, `anchorless.go` |
| 25 | `freshness: check ended by shutdown` | `freshness_check_ended_by_shutdown` | DEBUG | `internal/freshness/check.go` (`endedByShutdown`) |
| 26 | `freshness: clamp adoption retry failed` | `freshness_clamp_adoption_retry_failed` | WARN | `internal/freshness/freshness.go` |
| 27 | `freshness: clear adoption failure failed` | `freshness_clear_adoption_failure_failed` | WARN | `internal/freshness/check.go` |
| 28 | `freshness: deferred never-adopted suite paced until its retry or the next refresh check` | `freshness_deferred_never_adopted_suite_paced_until_retry` | DEBUG | `internal/freshness/first_adoption.go` |
| 29 | `freshness: never-adopted suite without a client-created anchor or flat index row, dormant` | `freshness_never_adopted_suite_dormant_no_client_created_row` | DEBUG | `internal/freshness/first_adoption.go` |
| 30 | `freshness: never-adopted suite without recent interest, dormant` | `freshness_never_adopted_suite_dormant_no_recent_interest` | DEBUG | `internal/freshness/first_adoption.go` |
| 31 | `freshness: no baseline anchor hash, skipping` | `freshness_check_skipped_no_baseline_anchor_hash` | DEBUG | `internal/freshness/check.go` (`baseline`) |
| 32 | `freshness: persist failure bump after a panic failed` | `freshness_persist_failure_bump_after_panic_failed` | WARN | `internal/freshness/freshness.go` |
| 33 | `freshness: read suite row after a panic failed` | `freshness_read_suite_row_after_panic_failed` | WARN | `internal/freshness/freshness.go` |
| 34 | `freshness: refused suite paced until its retry` | `freshness_refused_suite_paced_until_retry` | DEBUG | `internal/freshness/check.go` |
| 35 | `freshness: repair deferred, no adoption slot` | `freshness_repair_deferred_no_adoption_slot` | DEBUG | `internal/freshness/freshness.go` |
| 36 | `legacy trust audit: cached anchor skipped` | `legacy_trust_audit_cached_anchor_skipped` | DEBUG | `internal/freshness/legacy_audit.go` |
| 37 | `shutdown wait panicked` | `shutdown_wait_panicked` | ERROR | `cmd/apt-cacher-ultra/main.go` |
| 38 | `snapshot adopted_at lookup failed` | `snapshot_adopted_at_lookup_failed` | WARN | `internal/handler/handler.go` |
| 39 | `suite cached anchor read failed` | `suite_cached_anchor_read_failed` | WARN | `internal/handler/suite_rows.go` |
| 40 | `suite file head read failed` | `suite_file_head_read_failed` | WARN | `internal/handler/handler.go` |
| 41 | `suite reaper: suite busy, retried next run` | `suite_reaper_suite_busy` | DEBUG | `internal/freshness/reaper.go` |
| 42 | `suite signed-seen backstop read failed` | `suite_signed_seen_backstop_read_failed` | WARN | `internal/handler/suite_rows.go` |
| 43 | `suite touch failed` | `suite_touch_failed` | DEBUG | `internal/handler/suite_rows.go` |

Design notes (choices the spec leaves open — the boring, bounded option, with the reason):

1. **F-24's predicate** is `AND snapshot_id NOT IN (SELECT snapshot_id FROM legacy_trust_audit)` inside the displaced class's inner `SELECT`, before `ROW_NUMBER()` (ruling R-H1). Sub-job A (orphans: `adopted_at IS NULL`) needs none: the audit queues adopted snapshots only. Sub-job C needs none: a quarantine dequeues the snapshot in the transaction that retires it.
2. **R-H2's mechanics.** `LegacyAuditDecision` gains `DropAnchorRows bool`, valid only with `LegacyAuditQuarantine` or `LegacyAuditQuarantineSigned` (set with any other outcome it rolls the batch back, as an invalid outcome does). The freshness audit sets it for `anchor_corrupt` alone (`legacyVerdict.dropAnchorRows`). `ApplyLegacyAuditDecisions` returns `map[int64]LegacyAuditEffect{WasCurrent bool; AnchorRowsDeleted int}` for the quarantined snapshots (it returned `map[int64]bool`; two call sites read it). The rows are found by `blob_hash` over `idx_url_path_blob` (one indexed read), the snapshot's hash read inside the transaction (`suite_snapshot.inrelease_hash`; a snapshot without one deletes nothing). The pool file goes in `RunLegacyTrustAudit`, right after the verdict and before the decision joins its batch, through a seam (`legacyDiscardAnchor`, which tests swap to inject a failure); `internal/cache` itself still does no pool I/O, so the cache test's "pool file kept" stands. Two snapshots of one batch that share the rotted file: the first removes it and the second reads it missing (`anchor_missing`), whose rows the first's delete already took (review C-1).
3. **R-H3's and R-H6's scope, as the code stands.** Renamed: the 43 messages of the two tables. Left: the 78 prose messages `main` already logs, each listed in Task 3's guard (`mainProseMessages`). Found by an AST walk of every log call's message — slog's methods and the repository's forwarding helpers — in the non-test Go files at 6786529 and at 536a1ab, `main`'s base: 118 prose messages at 6786529 (the reaper's three step constants are counted at their one call site), 84 on `main`, 78 of them in both; the six only on `main` this branch already reworded or dropped ("For H3" lists them). The guard (`TestLogMessagesAreSnakeCase`) checks a message that is a string literal or a constant of its package; one that is neither must be a parameter of a forwarding helper listed in `logSinks` — `(*Reaper).warn`, `logPanic`, `reportPanic`, `logGoroutinePanic`, `emitTlsMitmLog`, and `internal/proxy`'s `LogFn` callbacks, whose call sites are checked in turn — or be listed in `fieldMessages` with the constants it takes (the reaper's `h.step`), so a new helper, or a message held in a variable, fails it until listed. Each allowlisted message must still be logged, so the list shrinks with a later rename. The failure messages say where to fix (review M-6). Every renamed event has a `docs/log-fields.md` row: the 11 DEBUG lines it never documented share one, each with the meaning its prose carried (review sweep M-6), and `TestSweptEventNames` reads a row — a backquoted name in a table row's first cell — not a mention. Task 18 extends the row rule to every snake_case event (`TestEveryEventIsDocumented`, review sweep M-11). The guard's own first catch was this plan's: Task 7's GC helper first named `reportPanic` collided with the freshness forwarder's name in `logSinks`, so it is `logGCPanic`. Before ruling R-H6 this note read: not renamed — added by this branch, not listed by the overview (a question for the maintainer): `adoption: read prior snapshot failed`, the `adoption.require_signature = false: …` startup notice, `displaced snapshot lookup failed`, `freshness: anchorless index failed its sniff, kept`, `freshness: check cancelled`, `freshness: check ended by shutdown`, `freshness: clamp adoption retry failed`, `freshness: clear adoption failure failed`, `freshness: deferred never-adopted suite paced until its retry or the next refresh check`, `freshness: never-adopted suite without a client-created anchor or flat index row, dormant`, `freshness: never-adopted suite without recent interest, dormant`, `freshness: no baseline anchor hash, skipping`, `freshness: persist failure bump after a panic failed`, `freshness: read suite row after a panic failed`, `freshness: refused suite paced until its retry`, `freshness: repair deferred, no adoption slot`, `legacy trust audit: cached anchor skipped`, `shutdown wait panicked`, `snapshot adopted_at lookup failed`, `suite cached anchor read failed`, `suite file head read failed`, `suite reaper: suite busy, retried next run`, `suite signed-seen backstop read failed`, `suite touch failed`. Not renamed — on `main` already: 78 messages, among them `freshness check failed`, `freshness: adopted suites stale beyond threshold`, `freshness: read suite row failed`, `InRelease changed at upstream`, `touch failed`, `singleflight coalesced`, `adoption: commit failed`, `schema migrated`, `fetch retry`. (Found by an AST walk of every slog call's message literal in the non-test Go files at 6786529 and at 536a1ab, `main`'s base.)
4. **F-16's field.** The two unreleased admin lines (`admin_forget_requested`, `admin_forget_failed`) name the scheme `canonical_scheme`, as every other line about a suite does. `admin_reconcile_requested` is on `main` with `scheme`; it keeps `scheme` and gains `canonical_scheme` beside it (additive: a filter on either finds it). Task 5.
5. **`suites[].anchorless` (PG-14)** is derived, not stored: adoption enabled, a flat suite, no current snapshot, `last_success_at` set, no adoption record, `inrelease_change_seen_at` NULL. It holds because every other answered check of a never-adopted flat suite leaves a mark the derivation excludes: a first adoption (queued or running) sets `inrelease_change_seen_at` (`checkFirst`), a refusal or failure writes a record (`unsigned_unconfirmed`, `unsigned_disabled`, `anchor_absent`, …), a committed one sets the current snapshot; only an answered anchorless check clears the pending change and the record together (`anchorlessRefresh`). With adoption disabled no anchorless check runs, and the Phase-1 inline check bumps `last_success_at` of a flat suite with an `InRelease` too, so the field is false there. One exception lasts a check: a flat suite whose row an adoption-disabled `InRelease` miss created has `last_success_at` set (`suiteSeed` stamps it), so the field reads true from the moment adoption is enabled until that suite's first check (review M-11; a note, since the window is one check). JSON `anchorless` (boolean, additive); the HTML Adoption cell reads `refreshed without a Release`. No schema change, no in-memory state, true across restarts. An AIDEV-NOTE at the derivation names the facts it relies on. Task 4.
6. **B-63's text** is `checks failing ×k · backoff ends in <d>` in the Adoption cell, after the record's text and ` · ` when there is one, or `checks failing ×k · backoff over` once that time has passed (reviews M-3 and sweep M-5): k is `check_fail_count`, d is `last_check_at + backoff(k) − now`, backoff being the scheduler's own `backoffDelay` reached through `admin.Config.CheckBackoff func(failCount int64) time.Duration` (`freshness.CheckBackoff`, wired by `main`; nil: the count alone). That is only the scheduler's floor — the earliest it may check: a client's T1 check may come sooner (spec C4: cooldown only), and `suiteDue` also wants staleness, a due retry or a pending change. A suite the scheduler does not queue on that backoff shows the count alone (`unscheduled`): with adoption enabled, a never-adopted suite no client asked for within the 7-day interest window, and an adopted suite its `anchor_absent` or `unsigned_disabled` record paces (rulings PE-2, PE-12; its record's text shows its retry). `unscheduled` mirrors `suiteDue` and freshness's `interestWindow` in `internal/admin`, which does not import `internal/freshness`; an AIDEV-NOTE ties the two. A suite waiting for a slot has no persisted state to show (the scheduler's in-memory queue) and is left as it is. HTML only (`json:"-"`): the JSON already carries `check_fail_count` and `last_check_unixtime`. Task 4.
7. **F-13's signal.** Each completed run logs `suite_reaper_run_complete` (INFO: `tried`, `reaped`, `markers_swept`, `deferred`, `duration_ms`) and sets the unlabelled gauge `acu_suite_reaper_last_run_unixtime`. A run whose listing failed is not complete (its WARN says why) and sets neither, so an alert on the gauge's age catches a reaper that stopped. `deferred=true` is a run the forward-step guard held back (no reap, no sweep). One line an hour, as `gc_run_complete` is per tick. Task 6.
8. **F-15's line** is DEBUG `suite_not_forgotten` (`canonical_scheme`, `canonical_host`, `suite_path`, `reason`: `no_suite` — no row, before or under the hold —, `busy` — the hold was refused —, or `changed` — the final compare-and-delete found the row changed), logged by `(*freshness.Reaper).Forget`, which knows; the HTTP answer still tells neither (spec C9). Task 5.
9. **F-18's fields.** `suite_reaped` gains `last_requested_unixtime` for `reason=idle` (the listed stamp the cutoff judged) and `evidence` for `reason=junk` (the judged rows' paths, a list in the order judged — a JSON array, since a path may hold a space (review M-7): at most the three anchors and the 14 root index names). Task 6.
10. **F-17.** The reaper's forward-step report (`clock_step_detected stamp=wall_clock`) is counted in `acu_clock_steps_detected_total`, whose HELP text becomes "Wall-clock steps detected: a past-event timestamp found in the future (a backward step), or the wall clock run ahead of the monotonic clock between reaper runs (a forward step)." The report is once per detected step, so the count stays bounded. Task 6.
11. **F-21's recovers.** `(*gc.GC).Run` recovers a panic per periodic tick and goes on to the next tick, and a tick's panic moves the pass rotation past the pass in progress (`skipPanickedPass`), so a pass that panics every tick cannot starve the passes after it — the blob pass keeps reclaiming disk (review M-1); the line, metrics and last-run summary stay inside the recovered function, so `acu_gc_last_run_unixtime` ages after a panic. `StartupPass` recovers one too and returns it as an error (`gc startup: panicked: …`), which main logs (`gc startup pass failed`) before it serves; a panic there would otherwise recur at every start (review I-1; `recoverPanicInto`, a sibling of `recoverPanic` that sets the error, both deferred directly); the pool scan recovers per job in each worker and in its producer, whose deferred `close(jobs)` still ends the scan. Each logs ERROR `gc_panicked` (`where`: `tick`, `startup`, `pool_scan_worker`, `pool_scan_producer`; `panic`; `stack` once per `where` per process). A worker never exits on a panic: if every worker died, the producer would block on `jobs` until shutdown and the startup pass would never end. Task 7.
12. **GD-2.** `ParseSources` keeps `validateMemberPath` for `Directory` and each file path (ruling PG-21: no widening of what a `dists/` `Sources` materialises), as spec C5 already says (revision 10, PG-21); revision 12 adds the line (review sweep M-2). What was silent becomes visible: `SourcesParseStats` counts the skipped paths and keeps the first, and `buildSourceHashes` logs INFO `source_paths_skipped` (INFO, not WARN: it repeats at every adoption and the operator cannot act on it, review M-8) (`canonical_scheme`, `canonical_host`, `suite_path`, `snapshot_id`, `member_path`, `count`, `first`: `source_parsed`'s fields) once per `Sources` file that skipped any. A skipped path is one `validateMemberPath` refused — every file of a stanza whose `Directory` fails, each file whose own path fails; an entry dropped as malformed (a bad hash, a `/` in its filename, a filename of `.` or `..`) is not counted, as before. Task 10.
13. **E-24** is documented, not gated: a never-adopted check that ends unknown leaves no record, so each client check past the cooldown re-runs the probe tree (a `Release` download included); spec C4 keeps T1 on the cooldown alone, and the permanent case (the knob off) is already a paced refusal (PE-12). `docs/configuration.md` says so (Task 18).
14. **E-31.** One predicate, `signedForm(cache.AnchorForm) bool` (inline or detached), in `baseline` and `Reconcile`; `Reconcile`'s failed snapshot read, which counts the suite unsigned (fail-closed), is logged WARN `reconcile_snapshot_read_failed` (`err`, `snapshot_id`, `canonical_scheme`, `canonical_host`, `suite_path`). Task 8.
15. **C-38's coarsening** keeps every assertion: the 500 suites answer their first check 503 — an unknown end that takes no virtual time, so the first tick fails all 500 (they were 500 stalling checks, 16 h 40 min of ticks: about two thirds of the run) — and stall from then on, so phases 2 and 3 run as before; the 501 suites are seeded in one transaction (`seedSimSuites`; 1,503 writer calls took 2.2 s). Measured 6.96–7.05 s → 2.92–2.95 s. `simtest.Unavailable()` is the 503 behaviour. Task 13.
16. **Deferred-minor citations.** 142 comments cite a deferred minor by ID (89 in production code). They stay when the deferred files are deleted: the phase plans that took each item still name it, and the files' history is in git. Only F-7's two comments, which record nothing but history, change (Task 6), and two that point at a deferred file by name and say what it no longer holds: `wallclock_stamps_test.go`'s A-3 note and `anchorless.go`'s GD-1 AIDEV-NOTE, which now records the decline (Task 18). Task 18's overview note says where the IDs now resolve.
17. **E-46** moves the nine tests of `internal/freshness/deferred_e_test.go` beside their subjects and deletes the file, whose name points at a document this plan deletes. Task 16.
18. **F-1/F-2's seam.** `internal/gc` reaches the cache's snapshot-pass and pool-scan calls through an unexported interface, `gcStore` (`RunSnapshotGCBatch`, `RunRetiredSnapshotGCChunk`, `HashKnown`), set from `Config.Cache` in `New`; tests replace it. The other passes keep `g.cfg.Cache`. Task 7.
19. **F-6's bodies.** The admin's `http.Error` messages carry no trailing newline (`http.Error` adds one), so every plain-text error body ends in exactly one; the success bodies written with `io.WriteString` keep theirs. `OPTIONS /reconcile` answers `Allow: POST, OPTIONS`, as `/forget` does. Task 5.
20. **F-5's answer.** A body `ParseForm` cannot parse is 400 `malformed form body`; only an `*http.MaxBytesError` is 413 `request body too large`. Task 5.
21. **E-13.** A failed served-anchor read that a shutdown caused (`p.stopErr(ctx) != nil`, or the error is `context.Canceled`) logs DEBUG, not WARN, as the check's local steps do (`endedByShutdown`, FW-3 (d)). Task 10.
22. **E-61 is declined** (table): `wait` wakes only when the host's count reaches zero because the handler re-runs its lookup once; woken at the first `end`, a request whose snapshot commits with a later adoption on the host would re-run too early and be refused.
23. **GD-5 is declined by ruling R-H5**, a controller ruling for the maintainer's confirmation (review sweep M-10): whether an adopted unsigned flat suite whose `Release` stays absent should drop its snapshot and refresh anchorless was decided for 1.1.0 — keep the behaviour *Flat repositories* documents; revisit if seen in the field. Task 18 records it in spec §10 item 12 and among the overview's decided entries.

Rulings of earlier phases this plan applies: PD-1 (a file changed only by a mechanical rename does not count as touched for the deferred-minor rule), PD-5 (no pool I/O on the writer), PE-7 (C-21), PE-8 (the 4.5 s budget), PF-11 (one field added to one log line touches that function, not the file), PF-12 (the marker a delete writes, which R-H1 deliberately does not use), PG-3 (A-56), PG-21 (GD-2), PG-26 (GD-3), PG-33 (GD-4, GD-5).

## Deliberate deviations from the spec

None. Revision 12 records one correction where the spec and the code disagreed — spec C3's and `docs/configuration.md`'s "no longer checked for changes or shown on the status page" for a detached-only repository, true only for one first seen after the upgrade (D-52) — and one behaviour change, spec §6.1's "its `url_path` anchor row is kept" (R-H2, with the pool file's removal: review C-1). Spec C5 already says `ParseSources` keeps `validateMemberPath` (revision 10, PG-21); revision 12 adds that the skipped paths are logged (review sweep M-2).

## Review Focus (H1 share)

The input classes this plan's changes meet that the spec implies and no earlier test exercised, most likely first; each line's test is added to the task that owns the code:

1. **A corrupt anchor whose upstream has not changed**, the usual bit rot, and **one corrupt pool file named by several rows** (a flat suite and its `/.` twin, or two hosts remapped to one archive, cache the same `InRelease` bytes): the audit removes the rotted file, so the client's next request misses, refetches the same bytes onto a fresh file and hits it after (review C-1), and R-H2 deletes every row that names the blob, under both schemes, releases each row's pin once, and leaves another blob's rows alone — Task 2 (`TestServeHTTP_ACorruptAnchorQuarantineRefetchesTheAnchor`'s `upstream unchanged` case, `TestApplyLegacyAuditDecisions_CorruptAnchorDropsEveryRowOfItsBlob`).
2. **A deferred snapshot the audit later finds plain text** (with `keep_displaced = 0`): sub-job B keeps it while it is queued, so it still refuses an unsigned candidate; once the audit quarantines it as plain text it writes no marker, so the suite may then adopt unsigned — the trust R-H1 must neither lose nor invent — Task 1 (`TestRunSnapshotGCBatch_KeepsTheAuditsVerdict`).
3. **A form body with a stray `%`** (`curl -d 'suite=/a%zz'`, a script that forgets `--data-urlencode`): 400 `malformed form body` from both mutating endpoints, never 413 — Task 5 (`TestMutatingEndpoints_MalformedBodyIs400`).
4. **A panic in a GC tick or the startup pass** (a future bug in sub-job C, now in GC's goroutine, or one persisted data triggers at every start): the tick is logged, the rotation moves past the pass, and the next tick runs; the startup pass reports it as an error and the daemon serves; a pool-scan worker that panics never stalls the startup pass — Task 7 (`TestRun_RecoversATickPanicAndRunsTheNextTick`, `TestPeriodicTick_APanickingPassDoesNotStarveTheNext`, `TestStartupPass_ReturnsAPanicAsAnError`, `TestPoolScan_AWorkerPanicNeverStallsTheScan`).
5. **A Release-less suite whose upstream starts publishing a `Release`**: the status page stops calling it `refreshed without a Release` as soon as the first adoption is under way (the pending change set), and never shows it for a `dists/` suite or with adoption disabled — Task 4 (`TestSuiteEntries_Anchorless`).

## File map

| Area | Files | Task |
|---|---|---|
| Sub-job B and the audit queue | `internal/cache/gc.go` (`RunSnapshotGCBatch`), `internal/cache/f24_gc_test.go` (new) | 1 |
| The corrupt anchor | `internal/cache/legacy_audit.go` (`LegacyAuditDecision.DropAnchorRows`, `LegacyAuditEffect`, `dropAnchorBlobRowsTx`), `legacy_audit_test.go` (`applyOne`), `corrupt_anchor_test.go` (new); `internal/freshness/legacy_audit.go` (`legacyVerdict.dropAnchorRows`, `legacyDiscardAnchor` and the pool file's removal, the quarantine line), `corrupt_anchor_test.go` (new); `internal/handler/corrupt_anchor_test.go` (new) | 2 |
| Event names | `internal/handler/{handler,suite_rows}.go`, `cmd/apt-cacher-ultra/main.go`, `internal/freshness/{adoption,anchorless,check,current_form,first_adoption,flip_barrier,freshness,legacy_audit,reaper}.go`, `internal/admin/handlers.go`, the tests that match them, `docs/log-fields.md`, `docs/configuration.md`; `internal/observability/event_names_test.go` (new: both guards) | 3 |
| Status page and JSON | `internal/admin/status.go` (`suiteEntry.Anchorless`, `CheckStatus`, `gcInfo.RetiredReaped`, `adoptionEntry.Scheme`), `status_template.go`, `server.go` (`Config.CheckBackoff`), `status_annotations_test.go` (new), `status_test.go` (B-60), `json_contract_test.go`, `testdata/json_contract_golden.json`; `internal/observability/ring.go` (`AdoptionEvent.Scheme`); `internal/freshness/freshness.go`, `adoption_failure.go` (the ring's scheme), `retry.go` (`CheckBackoff`), `adoption_ring_test.go` (new); `cmd/apt-cacher-ultra/main.go` (wiring) | 4 |
| Admin endpoints | `internal/admin/handlers.go` (`suiteForm`, `parseSuiteForm`), `server.go` (`OPTIONS /reconcile`), `forget_test.go` (F-16), `endpoint_form_test.go` (new); `internal/freshness/forget.go` (`suite_not_forgotten`), `forget_refusal_test.go` (new) | 5 |
| The reaper | `internal/freshness/reaper.go`, `metrics.go`, `suite_lock.go` (F-25), `freshness.go` (`holdPurgeHook`), `clockstep.go`, `unsigned.go` (F-7), `reaper_signal_test.go` (new) | 6 |
| GC | `internal/gc/gc.go` (`gcStore`, `StartupPass`, `periodicTick`, `skipPanickedPass`, `recoverPanic`, `recoverPanicInto`, `logGCPanic`, `gc_panicked`), `snapshot.go` (F-3), `pool_scan.go` (recovers), `gc_store_test.go` (new); `internal/cache/gc.go` (`effectiveChunkRows`), `retired_chunk_test.go` (new) | 7 |
| The check path | `internal/freshness/freshness.go` (`retryGate.holds`, `Reconcile`), `check.go` (`signedForm`), `current_form.go`, `first_adoption.go`, `probe.go` (a comment), `scheduler.go` (E-25 note, E-30 field), `check_path_test.go` (new) | 8 |
| Scheduler and panic boundaries | `internal/freshness/scheduler.go` (`tickInterval`), `freshness.go` (`recoverGoroutine`), `adoption_failure.go` (`recoverAdoption`), `freshness_test.go` (a comment), `deferred_e_test.go` (E-43), `dod_e_test.go` (E-56), `scheduler_wiring_test.go` (new) | 9 |
| Adoption members, the served anchor, Sources | `internal/freshness/member_verify.go` (E-10), `flip_barrier.go` (E-13), `sources_parse.go`, `adoption.go` (GD-2), `adoption_paths_test.go` (new) | 10 |
| Strict-mode wait | `internal/freshness/inflight.go`, `inflight_test.go`; `internal/handler/strict_wait_test.go`, `seed_t1_test.go` | 11 |
| Release Date | `internal/freshness/release_date.go`, `release_date_test.go` | 12 |
| Simulations | `internal/freshness/sim_sched_test.go` (C-38, E-51), `sim_retry_test.go` (E-50, E-52, E-53), `dod_e_test.go` (E-55), `dod_f_test.go` (F-10, F-11); `internal/simtest/simtest.go` (`Unavailable`), `aptclient.go` (C-8), `aptclient_test.go` (C-5); `internal/flipgate/sim_test.go` (C-7); `internal/freshness/current_form_test.go` (R-H7) | 13 |
| Test gaps | `internal/wallclock/wallclock.go` (doc), `wallclock_test.go`; `internal/aptpath/imports_test.go`, `encode_test.go`, `composers_test.go`; `internal/freshness/hot_set_test.go` (new), `reconcile_r1_test.go`, `first_adoption_test.go`, `suite_lock_test.go`; `internal/cache/schema_v7_test.go`, `sqlclock_test.go`, `gc_test.go` (C-12), `flip_state_test.go`, `cache_test.go` (drift: by hand), `adoption_failure_test.go`, `unsigned_form_test.go`; `internal/config/config_test.go`; `internal/gpg/failclosed_test.go`; `internal/simtest/clearsign.go`, `clearsign_test.go` | 14 |
| Test output and flakes | `internal/handler/phase2_chaos_test.go` (`holdSuiteBriefly`, `triggerUntilParked`, C-39), `phase2_detached_chaos_test.go`; `internal/admin/handlers.go` (`startRefresher`, F-23), `admin_test.go` (`startAdminServer`, `newAdminServer`), `refresher_test.go`, `shutdown_order_test.go` (new); `internal/gc/gc_test.go`; `internal/cache/open_cache_test.go`, `connpool_test.go`; `cmd/apt-cacher-ultra/suite_caps_wiring_test.go`, `shutdown_drain_test.go`, `legacy_audit_wiring_test.go`, `upstream_invalid_cert_test.go`, `adopt_unsigned_wiring_test.go`; the `cache.Open(…, nil)` and logger-less adopter sites of eight `internal/freshness` test files (`hot_prefetch_test.go`, a drift file, by hand) | 15 |
| Comment nits; E-46 | `internal/proxy/proxy.go`, `internal/cache/legacy_audit.go`, `internal/freshness/legacy_audit.go`, `internal/handler/phase2_handler_test.go`, `fetch_seam_test.go`, `phase2_migration_test.go`, `internal/freshness/sim_burst_test.go`, `cmd/apt-cacher-ultra/main_test.go`; `internal/freshness/deferred_e_test.go` (deleted) and the six files its tests move to | 16 |
| DoD | none: the DoD map and the module-wide checks | 17 |
| Documentation | `internal/observability/event_docs_test.go` (new: every event has a row), `docs/log-fields.md`, `docs/configuration.md`, `docs/admin-ui-spec.md`, `packaging/config/config.toml.default`, the spec, the overview, two comments (`internal/handler/wallclock_stamps_test.go`, `internal/freshness/anchorless.go`), the seven deferred files (deleted) | 18 |

No migration, no new package, no new dependency, no new config key.

## Disposition of every deferred minor

`fix (Tn)`: Task n changes the code, test or doc (a behaviour change has its own test). `done`: already resolved, with the evidence checked in the code at 6786529. `declined`: a ruling — the reason, then the cost if wrong. No row is left to the maintainer: GD-5, the one business decision, is declined by ruling R-H5.

| ID | Item | Disposition |
|---|---|---|
| A-1 | `wallclock_test.go`: inline `restore()` | fix (T14): `t.Cleanup(restore)` beside the inline call |
| A-2 | no guard that production never calls `SetOffsetForTest`; LIFO doc | fix (T14): AST guard over `cmd/` and `internal/`; the doc says restores run LIFO |
| A-8 | `TestCacheDoesNotImportProxy` checks direct imports only | fix (T14): the limitation commented, with the command that checks the rest |
| A-9 | all-bytes loop skips NUL | fix (T14): from 0 (NUL round-trips: replayed) |
| A-10 | `proxy.go` control-byte invariant is a plain comment | fix (T16): an AIDEV-NOTE |
| A-11 | opt-in fixture test's message omits got/want | fix (T14) |
| A-12 | `encode.go` fallback is silent (deviation 2) | done: `BuildUpstreamURL`'s comment names overview deviation 2 and why there is no counter (`internal/aptpath/encode.go:59-66`) |
| A-13 | HTTPS-magic host has no charset validation | declined: pre-existing on `main`; a host `url.URL` cannot carry already fails the fetch before any upstream byte, and a charset rule would refuse working names (IDN, `_`) without closing what allow-all mode admits by design (MITM defaults). Cost if wrong: a malformed HTTPS-magic request gets 502 where 400 would be clearer |
| A-14 | the concatenation guard's regex is narrow | fix (T14): `"(https?)?://"\s*\+` and `Sprintf("…%s://`, two named exemptions |
| A-15 | `hot_set.go`'s URL has no direct test | fix (T14): `computeHotSet` on a `+`/`~` path yields apt's encoding |
| A-19 | `migrationGoSteps[6]` swap without a "no `t.Parallel`" note | fix (T14) |
| A-20 | `CURRENT_TIMESTAMP` redundant with `CURRENT_TIME` | fix (T14): dropped, the substring rule commented |
| A-21 | schema test does not assert the index is partial | fix (T14): `pragma_index_list(…).partial = 1` |
| A-26 | `ListTrackedSuiteAnchors` re-filters what SQL guarantees | fix (T16): the suffix re-filter removed (the stamp test stays) |
| A-27 | doc says "missing", means "missing or empty" | fix (T16) |
| A-55 | "Review Focus 2" said to be item 1 | done: the overview's Review Focus item 2 is "Paths with unusual bytes" (overview line 49), so `fetch_seam_test.go:90` is right as it stands |
| A-56 | `handler.Fetcher` vs `freshness.Fetcher` naming | declined: naming only; a rename churns `handler`, `freshness` and `simtest` for no change in behaviour (PG-3). Cost if wrong: a reader confuses two interfaces whose doc comments tell them apart |
| B-9 | Unicode whitespace in the Date parser | fix (T12): ASCII fields and trim, as apt's `isspace` |
| B-10 | `TrimSpace(key)` accepts `Date :` | declined: no divergence — apt's tag parser also trims blanks before the colon (apt 3.3.2's `apt-sortpkgs` reads `Package : foo` as `Package`: review sweep M-3's probe; Release files go through the same parser), so `TrimSpace(key)` is parity; `TestReleaseDate`'s padded-key case pins it. Cost if wrong: none |
| B-11 | `parseAptDate` shadows `ok` | fix (T12) |
| B-12 | `aptStoi` aliases `aptSignedInt` | declined: the two names mirror two apt functions (`istream >> int`, `std::stoi`) with one Go behaviour, keeping the port readable against apt. Cost if wrong: a three-line function |
| B-13 | `Date` read beyond the first paragraph | declined: this package's test verifiers hand the parsers the whole clearsign envelope as its plaintext (armor header and blank line first), so a first-paragraph reader finds no Date in their fixtures — the replay failed eight tests (`TestAdopter_SHA512OnlyReleaseAdopts`, the date-rule sims, the refusal tests); real Release files are one paragraph and the real verifier passes the signed text alone. An AIDEV-NOTE at `releaseDate` records it (T12). Cost if wrong: a Release whose Date sits in a second paragraph is date-checked where apt is not |
| B-14 | the AIDEV-NOTE blames `aptDigits` for the RFC 850 divergence | fix (T12): the note names the RFC 850 day's own bound |
| B-27 | restart test only at `rand = 0.5` | fix (T9): `rand` 0, 0.5 and 0.999 |
| B-36 | four overlapping checker constructors | declined: test structure only; they differ in seeding and logger, and merging rewrites 15 tests for no change in what any proves. Cost if wrong: four 10-line constructors |
| B-52 | phase B plan's Task 12 text stale | declined: an executed plan records what ran (historical by convention). Cost if wrong: none — later plans copy interfaces from the overview |
| B-60 | `suites_healthy_no_lag` fixture sets a change stamp | fix (T4) |
| B-63 | status page cannot say why a failing suite is not updating | fix (T4): `checks failing ×k · backoff ends in …` / `backoff over`, the count alone for a dormant never-adopted or record-paced suite (design note 6; reviews M-3, sweep M-5); a suite waiting for a slot is not shown (in-memory queue only) |
| C-5 | two ledger properties unpinned | fix (T13): a held anchor then an 11 s filler is not checked; out-of-window updates leave `AnchorGens` empty |
| C-6 | flipgate/handler periodic-client helpers repeated | declined: test helpers across three packages; consolidating churns three suites' simulations for no change in what they prove. Cost if wrong: four copies of two short helpers |
| C-7 | colliding client ids in `flipgate/sim_test.go` | fix (T13): an id base per group |
| C-8 | `RunUpdate`/`AnchorGens` docs | fix (T13) |
| C-9 | `flip_quiet_period` rejection loop checks only `err != nil` | fix (T14): the error names the key and bounds |
| C-12 | `stubNow` registers no cleanup | fix (T14): `stubNow` registers `t.Cleanup` |
| C-13 | `putAnchor` written three times | fix (T14): one file-level helper |
| C-21 | handler context fast path | declined: ruling PE-7 — it needs new `flipgate` API, has no measured gain and touches the barrier's registration race. Cost if wrong: one `EXISTS` read a miss already pays |
| C-36 | admin-ui-spec: adoption duration includes the barrier | fix (T18) |
| C-37 | conffile comments for `max_duration`, `keep_displaced` | fix (T18) |
| C-38 | 500-suite simulation over budget | fix (T13): 7.0 s → 2.9 s (design note 15) |
| C-39 | one unexplained chaos-test failure | fix (T15): phase 2's trigger in both chaos tests repeats its anchor GET every 20 ms until the member-fetch gate has a waiter (at most 5 s), as `waitForFlip` nudges — a single T1 is dropped (`resultBusy`, the check's TryLock peek) while a phase-1c T1 check still holds the suite after its 304, so no adoption starts; red: the suite held across the first trigger GET (review sweep I-1; a test-only fix) |
| D-6 | fail-closed accept leg is LF-only and does not ask the sniff | fix (T14): a CRLF envelope with blanks on the marker line verifies, and `SniffInRelease` accepts it |
| D-7 | `ClearsignedFixture` accepts a `-` line | fix (T14): it panics; the doc names the trimmed blanks |
| D-9 | `Rejects*` tests satisfied by the helper's error | fix (T14): `InsertCandidateSnapshot` directly, after a checked `EnsureSuiteForTest` (drift file: by hand) |
| D-10 | the reschedule's `COALESCE` untested | fix (T14): a count-3, NULL-reason row is rescheduled |
| D-26 | `DecodesAptsMarkerLine` never asserts its replace matched | fix (T14) |
| D-28 | local `staleBody` shadows the helper | fix (T16): `staleAnchor` |
| D-29 | `recFetcher` doc | fix (T16) |
| D-30 | plain `"attacker bytes"` without a PD-2 comment | fix (T16) |
| D-46 | `startupLogs` does not drain `done` on timeout | fix (T15) |
| D-48 | legacy-audit wiring test prints migration lines | fix (T15) |
| D-49 | `shutdown_drain_test.go` repeats `startupLogs`; untimed `http.Get` | fix (T15): a timed client; the loop reuses `startupLogs`' |
| D-50 | Suites empty-state text | fix (T4): the admin-ui-spec's copy |
| D-52 | detached-only wording in the docs and spec C3 | fix (T18) |
| D-55 | the (D) H item sits outside Carry-forwards | fix (T18): moved under Carry-forwards' **H:**, closed by R-H3 |
| D-56 | pre-existing test-output noise | fix (T15): silent loggers at every `cache.Open(…, nil)` and logger-less adopter of the named packages; verified by `-v` greps |
| D-57 | `TestProfilingRoutesOptInAndPrivate` cleanup flake | fix (T15, with F-23) |
| D-58 | TLS handshake noise in the invalid-cert test | fix (T15): a discarding `ErrorLog` |
| D-60 | `TestOpenCache_LogsNothing` asserts before `Close` | fix (T15) |
| E-1 | url_path GC guard (d)'s detached arm unpinned | fix (T14): a detached row in the form table |
| E-3 | `derefS` duplicates `derefStr` | fix (T14) |
| E-10 | `memberWriter` embeds `*cache.BlobWriter` | fix (T10): an unexported field with forwarded methods, so `Finalize` is not in its method set |
| E-11 | SHA512-only size mismatch and the two parse errors untested | fix (T10) |
| E-13 | served-anchor WARN on a routine restart | fix (T10): DEBUG at shutdown (design note 21) |
| E-14 | the C-16 WARN and the A-49 DEBUG unasserted | fix (T10) |
| E-21 | detached snapshot baseline unpinned | fix (T8) |
| E-22 | the GET prelude repeated three times | declined: a refactor of the check's most-tested path with no behaviour change; the three GETs differ in target, validators and sniff. Cost if wrong: a change to the prelude must be made in three places (as PG-38 left it) |
| E-23 | `check.go` nits (`clear`, stale comments, doc tuple) | fix (T8) |
| E-24 | an unknown never-adopted check is not paced | fix (T18): documented (design note 13) |
| E-25 | `suiteDue` queues a dormant-by-rows suite every tick | fix (T8): accepted in an AIDEV-NOTE at `suiteDue` |
| E-27 | survivor C3: `probe`'s `conditional = false` | fix (T8): an unconditional 304 is unknown |
| E-28 | survivors X2/X3: `crossForm` marking | fix (T8): the absent-anchor tree's and the upgrade probe's failure records keep no validators |
| E-29 | held cross-form content fetched in full each check | declined: pacing it adds a third pacing rule beside PE-2 and PE-10 in the check's busiest branch, the freeze-trap class this branch guards against; the cost is bounded and visible. Cost if wrong: one `InRelease` GET without validators per cooldown for an unsigned suite whose upstream also publishes a signature the cache does not trust (its record says so on the status page) |
| E-30 | stale WARN's `adopted_stale` vs the gauge | fix (T8): `paced_stale` on the line |
| E-31 | two "signed current" predicates; an unlogged read error | fix (T8): `signedForm`, and `reconcile_snapshot_read_failed` (design note 14) |
| E-32 | the hold predicate spelled out six times | fix (T8): `retryGate.holds(key)`; the answered-request block stays (E-22's reason) |
| E-33 | `hostCounter` lives in `current_form_test.go` | declined: test placement only. Cost if wrong: a reader looks for a helper in a neighbouring file |
| E-35 | contradicting comment in `waitForMigrationFlip` | fix (T16) |
| E-37 | future `last_success_at` reported before the cooldown gate | fix (T8): reported below the gate |
| E-38 | `Run`'s B-45 wiring untested | fix (T9): a release consumed before an offer's done re-offers at once (kills `finish(d, d.item.gen)`; the dispatch stamp's mutant needs a wrong peek no test can build without a seam — noted in the task) |
| E-39 | `refreshSigned` demotion untested | fix (T9) |
| E-40 | small stale docs in the scheduler and tests | fix (T9) |
| E-41 | `sim_burst_test.go` docstring | fix (T16) |
| E-42 | a panicking log handler skips the panic's counters | fix (T9): the report, then the counters and the ring event, each under its own boundary (review M-2) |
| E-43 | the record's recover and B-20's DEBUG unasserted | fix (T9) |
| E-45 | `recoverGoroutine`, `safeReschedule`, `safeTick` lack a second boundary | fix (T9): `unloggedPanic` around each report; `checkWorker` uses it |
| E-46 | `deferred_e_test.go` grouped by provenance | fix (T16): its nine tests moved beside their subjects |
| E-48 | hand-built armored signature in the handler tests | declined: a cross-package test fixture with one user. Cost if wrong: two definitions of a sniff-passing `Release.gpg` drift apart in tests |
| E-50 | the retry simulation's floor carried by its failing phases | fix (T13): each clock-step stretch has its own floor (≥ 16) |
| E-51 | `renderedMetricLabels` drops the `ParseFloat` error | fix (T13): the error fails the test (the "fifth helper" stays: test structure) |
| E-52 | B-56's doc claims both legs check a future date | fix (T13): the forward leg checks it too |
| E-53 | `time.Sleep(5 * day)` lands on the tick grid | fix (T13): split off the grid, total unchanged |
| E-54 | a redundant assertion | declined: it restates the property in words and costs nothing. Cost if wrong: none |
| E-55 | dormant "zero upstream requests" counts `InRelease` only | fix (T13): the three anchor URLs |
| E-56 | the tick hard-coded in `dod_e_test.go` | fix (T9): `tickInterval(refresh)`, which `Run` uses |
| E-58 | the 64-waiter cap checked before "nothing in flight" | fix (T11): reordered |
| E-59 | two strict-wait survivors | fix (T11): the refusal's `current_snapshot_count`; no wait on a hit with nothing in flight |
| E-61 | `wait` wakes only at a count of zero | declined: design note 22. Cost if wrong: a request whose snapshot committed early waits up to the 15 s bound, only on a strict host with overlapping signed adoptions |
| E-62 | parameter `max` shadows the builtin | fix (T11): `maxWait` |
| E-66 | X27 depends on a real 300 ms window | fix (T11): `holdHostSlotsUntilJoined`, no window |
| E-69 | `seed_t1_test.go` repetition and placement | declined: test structure only. Cost if wrong: one setup written twice |
| E-73 | a JSON substring match depends on key order | fix (T15): the line decoded |
| E-74 | `minimalCfg` runs with `AdoptUnsigned = false` | fix (T16): said in `minimalCfg`'s doc |
| E-75 | C-38's "7.9 s in phase E" | fix (T13, T18): C-38 is re-measured (design note 15) and its file deleted |
| E-76 | the E plan's executed blocks keep superseded text | declined: historical by convention. Cost if wrong: none — later plans copy interfaces from the overview |
| E-77 | commit `1ddade9`'s message | declined: history is not rewritten; later commits follow the convention. Cost if wrong: none |
| F-1 | sub-job C's deadline check untested | fix (T7) |
| F-2 | the chunk size depends on the `<= 0` default and the pass's 0 | fix (T7): the pass asks for 0 (fake store), and a 10,001-row snapshot takes two chunks |
| F-3 | the embedded result promotes `Total()` | fix (T7): a named field |
| F-4 | `/forget` copies `/reconcile`'s parsing | fix (T5): `parseSuiteForm` |
| F-5 | every `ParseForm` error is 413 | fix (T5): 400 for a malformed body (design note 20) |
| F-6 | `OPTIONS /reconcile`'s `Allow`; `\n\n` bodies | fix (T5) (design note 19) |
| F-7 | history-only comments | fix (T6) |
| F-8 | "no request, no write" pinned on three columns | fix (T14): the whole row compared |
| F-9 | D-24 sentinels depend on text-handler order | fix (T14): decoded JSON records |
| F-10 | DoD simulations sample goroutines only at boots | fix (T13): a daily sample mid-boot |
| F-11 | `gcd`'s deadline on the 6 h grid | fix (T13): moved off it |
| F-13 | the reaper has no run signal | fix (T6) (design note 7) |
| F-14 | no last-requested column | fix (T4) |
| F-15 | a 409 from `/forget` logs nothing | fix (T5) (design note 8) |
| F-16 | admin lines key `scheme` | fix (T5) (design note 4) |
| F-17 | the forward-step report is not counted | fix (T6) (design note 10) |
| F-18 | `suite_reaped` names no evidence or stamp | fix (T6) (design note 9) |
| F-19 | adoption outcomes show Host without its scheme | fix (T4) |
| F-20 | admin test servers print migration lines | fix (T15) |
| F-21 | GC's goroutines recover no panics | fix (T7) (design note 11): and the startup pass returns its panic as an error, and a panicking pass moves the rotation past itself (reviews I-1, M-1) |
| F-22 | pool reads have no per-read timeout | declined: a read blocked in the kernel cannot be cancelled; abandoning it leaks a goroutine per read for as long as the filesystem hangs, and every pool reader (serves, GC, adoption) hangs the same way — a hung pool is a host failure the service manager's stop timeout handles. Cost if wrong: on a hung pool filesystem the reaper's run and its shutdown wait with every other pool reader |
| F-23 | profiling test cleanup flake | fix (T15): the cleanup waits for `Shutdown` (30 s) and reports a slow one |
| F-24 | sub-job B deletes audit-deferred signed history | fix (T1) (ruling R-H1) |
| F-25 | `Purge` before `unlockDropped` inside `once.Do` | fix (T6): a deferred unlock |
| F-26 | `sortCandidates`' AIDEV-NOTE says regardless reaps have no snapshot | fix (T6) |
| GD-1 | the anchorless body-slot wait holds a host slot | declined: bounded by `checkDeadline`, no deadlock (a body-slot holder never waits for a host slot), failed checks back off; a short wait or a per-request timeout would make anchorless checks fail under contention and back off — a new failure mode for a rare idle slot. Cost if wrong: one of a host's slots idles up to 2 min while two slow anchorless bodies elsewhere hold the process's body slots |
| GD-2 | `ParseSources` uses `validateMemberPath` | fix (T10, T18): kept (PG-21), spec C5 gains the line, INFO `source_paths_skipped` (design note 12; reviews sweep M-2, M-8) |
| GD-3 | the flat minted-suites simulation never evicts | declined: twin eviction is pinned at the cache layer (`TestCreateSuite_EvictionLeavesTheOtherSchemesTwin`), the handler's eviction path is the same `CreateSuite` for both layouts and phase D's sibling simulation evicts (ruling PG-26). Cost if wrong: a flat-only handler eviction regression is caught by the cache-level test alone |
| GD-4 | one GET per cached `Contents-*` row per check | declined: the rows are bounded by the distinct names clients requested (apt-file asks one per architecture) and expire with `gc.url_path_ttl`; `Packages*`/`Sources*` go first and the deadline bounds the rest; a cap would leave the rows past it unrefreshed, so a client that asks for one is served a stale file — trading a bounded request cost behind a misbehaving upstream for stale bytes served to every client (ranking by `last_requested_at`, indexed by `idx_url_path_last_req`, needs no rotation stamp, but still leaves the tail stale; review sweep M-4). Cost if wrong: behind an upstream that answers 200 for any path, each check of that suite sends one GET per made-up name a client asked for within `url_path_ttl` |
| GD-5 | an unsigned flat suite whose `Release` disappears | declined: ruling R-H5 — keep for 1.1.0: rare in the field; the kept snapshot keeps serving cached content when an upstream dies outright, where a drop would turn that into 404s; a drop adds a trust and serving state transition late in the release. Cost if wrong: an operator runs `/forget` on a rare suite after an alert. A controller ruling, for the maintainer's confirmation (review sweep M-10). Recorded in spec §10 and the overview (T18) |
| GD-6 | `canonical_scheme` and shutdown level on the absent-anchor tree's read failure | fix (T8) |

Counts: fix 111, done 2, declined 23 (136); no row is the maintainer's (GD-5: ruling R-H5, for the maintainer's confirmation).

## For H3 (README and the 1.1.0 release notes)

What H1 changes that H3 must mention:

- **Upgrade (release notes, *Upgrading an existing cache*):** the legacy trust audit now also deletes the cached `url_path` rows of an `InRelease` whose pool file no longer matches its hash (`anchor_corrupt`) and removes the corrupt pool file, so clients fetch a fresh copy at their next update instead of being served bytes apt rejects, also when upstream still serves the same file; `legacy_snapshot_quarantined` carries `anchor_rows_deleted`, and ERROR `legacy_anchor_discard_failed` names a pool file the audit could not remove (then delete it by hand). A snapshot the audit cannot read at all keeps its rows and files, with ERROR `legacy_trust_audit_deferred` at every start, until the pool is fixed or the suite is forgotten. With `gc.keep_displaced = 0`, snapshot GC no longer deletes a snapshot the audit deferred (F-24: it kept never-downgrade from losing signed history). A detached-only repository (`Release` and `Release.gpg`, no `InRelease`) that an earlier release tracked stays tracked; only one first seen after the upgrade goes untracked while adoption is disabled (D-52 corrects phase D's wording; a release note copied from it must say the same).
- **Admin (README's admin section, release notes):** the status JSON gains `suites[].anchorless`, `recent_adoptions[].scheme` and `gc.retired_reaped` (additive); the page gains a *Last requested* column, `refreshed without a Release` and `checks failing ×k · backoff ends in …` (or `backoff over`) in the Adoption cell, and `scheme://host` in Recent adoption outcomes. `POST /reconcile` and `POST /forget` answer 400 (not 413) to a body that is not a valid form, every error body ends in one newline, and `OPTIONS /reconcile` names `POST`.
- **Logs and metrics (release notes):** the 43 renamed events (the tables under R-H3 and R-H6: old → new) are unreleased (nothing to migrate), but the release notes' list of new events uses the new names. Six prose messages `main` logs are already gone from this branch, reworded or dropped by phases A–G, and operators' alerts may match them: the `adoption.require_signature = false: unsigned upstream metadata would be adopted (auditable per SPEC2 §5.1)` startup WARN (now `adoption_require_signature_disabled`), `freshness: no baseline blob hash, skipping` and `freshness: no baseline Release blob hash, skipping` (the branch logs `freshness_check_skipped_no_baseline_anchor_hash` where they were), `freshness: persist anomaly failed`, `freshness: unexpected success status` and `seed suite_freshness failed`. From 1.1.0 on, every new log message is snake_case (Task 3's guard); new: `suite_reaper_run_complete`, `suite_not_forgotten` (DEBUG), `gc_panicked` (also for the startup pass, which no longer stops the daemon on a panic), `legacy_anchor_discard_failed`, `reconcile_snapshot_read_failed`, `source_paths_skipped` (INFO), and the gauge `acu_suite_reaper_last_run_unixtime`; `acu_clock_steps_detected_total` now also counts the reaper's forward steps; `admin_reconcile_requested` gains `canonical_scheme` beside `scheme`; `suite_reaped` gains `last_requested_unixtime` and `evidence` (a list of paths); the stale-suites WARN gains `paced_stale`.
- **For the maintainer's confirmation:** GD-5 is decided by a controller ruling (R-H5: keep; *Flat repositories* already documents the behaviour); every prose message this branch added is renamed (R-H6).

## Tasks

1. Sub-job B keeps snapshots the legacy trust audit has not judged (F-24, ruling R-H1)
2. An `anchor_corrupt` quarantine deletes the rows of the corrupt blob, and the audit removes the blob (ruling R-H2, review C-1)
3. snake_case event names and a guard against new prose messages (rulings R-H3, R-H6)
4. The status page and JSON: `retired_reaped`, the anchorless refresh, failing checks, last requested, the adoption scheme (carry-forwards; B-60, B-63, D-50, F-14, F-19)
5. The admin endpoints: one form parser, 400 for a malformed body, `OPTIONS`, `canonical_scheme`, a DEBUG line for a 409 (F-4, F-5, F-6, F-15, F-16)
6. The reaper's run signal, its evidence fields, the forward step counted, the hold's unlock (F-7, F-13, F-17, F-18, F-25, F-26)
7. GC: a store seam, sub-job C's deadline and chunk, recovered panics (F-1, F-2, F-3, F-21)
8. The check path (E-21, E-23, E-25, E-27, E-28, E-30, E-31, E-32, E-37, GD-6)
9. The scheduler and the panic boundaries (B-27, E-38, E-39, E-40, E-42, E-43, E-45, E-56)
10. Adoption members, the served-anchor read, `Sources` paths (E-10, E-11, E-13, E-14, GD-2)
11. The strict-mode wait (E-58, E-59, E-62, E-66)
12. The Release `Date` parser (B-9, B-11, B-14; B-13's note)
13. Simulations and their harness, and the jitter test under the budget (C-5, C-7, C-8, C-38, E-50–E-53, E-55, E-75, F-10, F-11; rulings R-H4, R-H7)
14. Test gaps in the cache, `aptpath`, `wallclock`, `gpg`, `config` and `simtest` (A-1, A-2, A-8, A-9, A-11, A-14, A-15, A-19–A-21, C-9, C-12, C-13, D-6, D-7, D-9, D-10, D-26, E-1, E-3, F-8, F-9)
15. Test output and flakes (C-39, D-46, D-48, D-49, D-56, D-57, D-58, D-60, E-73, F-20, F-23)
16. Comment nits and `deferred_e_test.go`'s move (A-10, A-26, A-27, D-28, D-29, D-30, E-35, E-41, E-46, E-74)
17. Phase H1's definition of done
18. Documentation, spec revision 12, the overview, the deferred files (C-36, C-37, D-52, D-55, E-24)

---

### Task 1: Sub-job B keeps snapshots the legacy trust audit has not judged (F-24, ruling R-H1)

The overview's "Phase H takes F-24 early". With `gc.keep_displaced = 0` (allowed), snapshot-GC sub-job B deleted a displaced, adopted inline or detached snapshot the legacy trust audit had deferred after a pool read fault — unmarked, and the base's only signed history (`seedSignedHistory`'s shape: the current snapshot was quarantined and retired) — while the suite row stayed; `refuseUnsignedDowngrade` then found neither a marker nor a signed snapshot and admitted an unsigned candidate, and the next audit could only `Drop` the vanished snapshot. The probe of the phase F final re-review (`SP/rr-final/cache/zz_rr_probe_test.go`) is the first test below, as an assertion: it fails at HEAD. The fix is ruling R-H1's one predicate (design note 1).

**Files:**
- Create: `internal/cache/f24_gc_test.go`
- Modify: `internal/cache/gc.go` (`RunSnapshotGCBatch`: its `selectSQL` and AIDEV-NOTE)

**Interfaces:**
- Consumes: `openCache`, `stubNow`, `sk`, `evictNow`, `seedRow`, `rowExists` (`suite_evict_test.go`), `seedSignedHistory`, `hostMarker` (`suite_reap_test.go`), `seedBlob`, `snapshotExists` (`gc_test.go`), `ApplyLegacyAuditDecisions`, `RunRetiredSnapshotGCChunk`, `EnqueueLegacyTrustAudit`.
- Produces: test helpers `commitUnsignedCandidate(t, c, key SuiteKey) error`, `seedDisplacedAt(t, c, key SuiteKey, adoptedAt int64, queued bool) int64`.

- [ ] **Step 1: Write the failing tests**

Create `internal/cache/f24_gc_test.go`:

```go
package cache

import (
	"context"
	"errors"
	"strconv"
	"testing"
)

// commitUnsignedCandidate tries to commit an unsigned candidate of key and
// returns CommitAdoption's error (spec C5: the never-downgrade authority).
func commitUnsignedCandidate(t *testing.T, c *Cache, key SuiteKey) error {
	t.Helper()
	ctx := context.Background()
	rel := seedBlob(t, c, "an unsigned Release for "+key.SuitePath)
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{CanonicalScheme: key.CanonicalScheme,
		CanonicalHost: key.CanonicalHost, SuitePath: key.SuitePath, InReleaseHash: &rel, Form: FormUnsigned})
	if err != nil {
		t.Fatal(err)
	}
	return c.CommitAdoption(ctx, id, []SnapshotMember{{SnapshotID: id, Path: "Release", BlobHash: rel, DeclaredSHA256: rel}},
		nil, nil, nil, false)
}

// Deferred minor F-24, ruling R-H1 (the phase F final re-review's probe):
// with gc.keep_displaced = 0, sub-job B deleted a displaced, adopted,
// audit-deferred signed snapshot — the base's only signed history, since
// the audit left it unmarked — while the suite row stayed, and never-
// downgrade then admitted an unsigned candidate. A snapshot still queued
// in legacy_trust_audit is never in B's displaced class: it is kept, and
// the unsigned candidate is refused, early and inside the commit.
func TestRunSnapshotGCBatch_KeepsAuditDeferredSignedHistory(t *testing.T) {
	for _, form := range []AnchorForm{FormInline, FormDetached} {
		t.Run(string(form), func(t *testing.T) {
			c := openCache(t)
			ctx := context.Background()
			defer stubNow(t, evictNow)()
			key := sk("h", "/r/dists/x")
			id := seedSignedHistory(t, c, key, form, false, evictNow)
			if err := c.CheckUnsignedDowngrade(ctx, "h", "/r/dists/x"); !errors.Is(err, ErrUnsignedDowngradeRefused) {
				t.Fatalf("premise: CheckUnsignedDowngrade = %v, want refused by the displaced snapshot", err)
			}
			res, err := c.RunSnapshotGCBatch(ctx, 100, 0, 0)
			if err != nil || res.DisplacedReaped != 0 || res.OrphanReaped != 0 {
				t.Fatalf("RunSnapshotGCBatch(keep_displaced 0) = %+v, %v; want nothing reaped", res, err)
			}
			if !snapshotExists(t, c, id) || !rowExists(t, c, key) {
				t.Fatalf("snapshot %d kept %v, row kept %v; want both", id, snapshotExists(t, c, id), rowExists(t, c, key))
			}
			if err := c.CheckUnsignedDowngrade(ctx, "h", "/r/dists/x"); !errors.Is(err, ErrUnsignedDowngradeRefused) {
				t.Fatalf("after sub-job B: CheckUnsignedDowngrade = %v, want refused", err)
			}
			if err := commitUnsignedCandidate(t, c, key); !errors.Is(err, ErrUnsignedDowngradeRefused) {
				t.Fatalf("after sub-job B: unsigned commit = %v, want ErrUnsignedDowngradeRefused", err)
			}
		})
	}
}

// Ruling R-H1, Review Focus 2: kept, the deferred snapshot waits for the
// audit's verdict, and sub-job B (or C) deletes it only afterwards. Keep:
// its base is marked, so once B deletes it the unsigned candidate is
// still refused (by the marker). A quarantine (plain text, no marker by
// §6.1's design): it is retired and sub-job C deletes it, and no marker
// was invented, so the suite may then adopt unsigned.
func TestRunSnapshotGCBatch_KeepsTheAuditsVerdict(t *testing.T) {
	t.Run("keep", func(t *testing.T) {
		c := openCache(t)
		ctx := context.Background()
		defer stubNow(t, evictNow)()
		key := sk("h", "/r/dists/x")
		id := seedSignedHistory(t, c, key, FormInline, false, evictNow)
		if _, err := c.ApplyLegacyAuditDecisions(ctx, []LegacyAuditDecision{{SnapshotID: id, Outcome: LegacyAuditKeep}}); err != nil {
			t.Fatal(err)
		}
		if res, err := c.RunSnapshotGCBatch(ctx, 100, 0, 0); err != nil || res.DisplacedReaped != 1 || snapshotExists(t, c, id) {
			t.Fatalf("after Keep: RunSnapshotGCBatch = %+v, %v, snapshot kept %v; want it reaped", res, err, snapshotExists(t, c, id))
		}
		if hostMarker(t, c, "http", "/r/dists/x") < 0 {
			t.Fatal("Keep wrote no marker")
		}
		if err := commitUnsignedCandidate(t, c, key); !errors.Is(err, ErrUnsignedDowngradeRefused) {
			t.Fatalf("after Keep and sub-job B: unsigned commit = %v, want refused by the marker", err)
		}
	})
	t.Run("quarantine", func(t *testing.T) {
		c := openCache(t)
		ctx := context.Background()
		defer stubNow(t, evictNow)()
		key := sk("h", "/r/dists/x")
		id := seedSignedHistory(t, c, key, FormInline, false, evictNow)
		if _, err := c.ApplyLegacyAuditDecisions(ctx, []LegacyAuditDecision{{SnapshotID: id, Outcome: LegacyAuditQuarantine}}); err != nil {
			t.Fatal(err)
		}
		if res, err := c.RunSnapshotGCBatch(ctx, 100, 0, 0); err != nil || res.DisplacedReaped != 0 {
			t.Fatalf("a retired snapshot: RunSnapshotGCBatch = %+v, %v; want nothing (sub-job C's)", res, err)
		}
		if n, more, err := c.RunRetiredSnapshotGCChunk(ctx, 0); err != nil || n != 1 || !more || snapshotExists(t, c, id) {
			t.Fatalf("RunRetiredSnapshotGCChunk = %d, %v, %v; want the quarantined snapshot deleted", n, more, err)
		}
		if hostMarker(t, c, "http", "/r/dists/x") >= 0 {
			t.Fatal("a marker for a quarantined plain-text snapshot")
		}
		if err := commitUnsignedCandidate(t, c, key); err != nil {
			t.Fatalf("after the quarantine: unsigned commit = %v, want admitted (no signed history)", err)
		}
	})
}

// seedDisplacedAt gives key's row one more adopted, displaced inline
// snapshot adopted at adoptedAt, queued for the audit when queued, and
// returns its id. The row must exist (seedRow).
func seedDisplacedAt(t *testing.T, c *Cache, key SuiteKey, adoptedAt int64, queued bool) int64 {
	t.Helper()
	h := seedBlob(t, c, "displaced anchor of "+key.SuitePath+" at "+strconv.FormatInt(adoptedAt, 10))
	res, err := c.db.Exec(`INSERT INTO suite_snapshot
  (canonical_scheme, canonical_host, suite_path, inrelease_hash, anchor_form, created_at, adopted_at, heartbeat_at)
  VALUES (?, ?, ?, ?, 'inline', ?, ?, ?)`, key.CanonicalScheme, key.CanonicalHost, key.SuitePath, h, adoptedAt, adoptedAt, adoptedAt)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	if queued {
		if err := c.EnqueueLegacyTrustAudit(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// Design note 1: a queued snapshot leaves B's displaced class before the
// ROW_NUMBER ranking, as retired rows do, so it takes no keep_displaced
// slot: with keep_displaced = 1 the newest unqueued displaced snapshot
// keeps the slot, the older one goes, and the queued one stays. And an
// unqueued displaced snapshot is still reaped at keep_displaced = 0.
func TestRunSnapshotGCBatch_AQueuedSnapshotTakesNoKeepSlot(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, evictNow)()
	key := sk("h", "/r/dists/x")
	seedRow(t, c, key, evictNow)
	oldest := seedDisplacedAt(t, c, key, evictNow-3000, false)
	older := seedDisplacedAt(t, c, key, evictNow-2000, false)
	queued := seedDisplacedAt(t, c, key, evictNow-1000, true)
	if res, err := c.RunSnapshotGCBatch(ctx, 100, 0, 1); err != nil || res.DisplacedReaped != 1 {
		t.Fatalf("RunSnapshotGCBatch(keep_displaced 1) = %+v, %v; want one reaped", res, err)
	}
	if snapshotExists(t, c, oldest) || !snapshotExists(t, c, older) || !snapshotExists(t, c, queued) {
		t.Fatalf("kept: oldest %v, older %v, queued %v; want older and queued (the queued one takes no slot)",
			snapshotExists(t, c, oldest), snapshotExists(t, c, older), snapshotExists(t, c, queued))
	}
	if res, err := c.RunSnapshotGCBatch(ctx, 100, 0, 0); err != nil || res.DisplacedReaped != 1 || snapshotExists(t, c, older) {
		t.Fatalf("RunSnapshotGCBatch(keep_displaced 0) = %+v, %v; want the unqueued one reaped", res, err)
	}
	if !snapshotExists(t, c, queued) {
		t.Fatal("the queued snapshot was reaped at keep_displaced 0")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 -run 'TestRunSnapshotGCBatch_KeepsAuditDeferredSignedHistory|TestRunSnapshotGCBatch_KeepsTheAuditsVerdict|TestRunSnapshotGCBatch_AQueuedSnapshotTakesNoKeepSlot' ./internal/cache/`
Expected: FAIL, with exactly these lines (the probe fails at HEAD; `KeepsTheAuditsVerdict` passes: it pins the verdict paths the fix must keep):

```
--- FAIL: TestRunSnapshotGCBatch_KeepsAuditDeferredSignedHistory/inline
    f24_gc_test.go:45: RunSnapshotGCBatch(keep_displaced 0) = {OrphanReaped:0 DisplacedReaped:1}, <nil>; want nothing reaped
--- FAIL: TestRunSnapshotGCBatch_KeepsAuditDeferredSignedHistory/detached
    f24_gc_test.go:45: RunSnapshotGCBatch(keep_displaced 0) = {OrphanReaped:0 DisplacedReaped:1}, <nil>; want nothing reaped
--- FAIL: TestRunSnapshotGCBatch_AQueuedSnapshotTakesNoKeepSlot
    f24_gc_test.go:146: RunSnapshotGCBatch(keep_displaced 1) = {OrphanReaped:0 DisplacedReaped:2}, <nil>; want one reaped
```

- [ ] **Step 3: Rank audit-queued snapshots out of sub-job B**

In `internal/cache/gc.go` (`RunSnapshotGCBatch`), extend the comment above `selectSQL`:

old:
```go
	// Retired snapshots (those of a suite reaped, evicted at a cap or
	// forgotten, and those the legacy trust audit quarantined) are ranked
	// out the same way, before the window function (spec C9): they never
	// take one of a suite's keep_displaced slots, and sub-job C
	// (RunRetiredSnapshotGCChunk) deletes them.
	const selectSQL = `
```
new:
```go
	// Retired snapshots (those of a suite reaped, evicted at a cap or
	// forgotten, and those the legacy trust audit quarantined) are ranked
	// out the same way, before the window function (spec C9): they never
	// take one of a suite's keep_displaced slots, and sub-job C
	// (RunRetiredSnapshotGCChunk) deletes them.
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
```

and in the displaced class's inner `SELECT`:

old:
```go
     WHERE adopted_at IS NOT NULL
       AND retired_at IS NULL
       AND snapshot_id NOT IN (SELECT current_snapshot_id
                                 FROM suite_freshness
                                WHERE current_snapshot_id IS NOT NULL)
  ) WHERE rn > ?
```
new:
```go
     WHERE adopted_at IS NOT NULL
       AND retired_at IS NULL
       AND snapshot_id NOT IN (SELECT current_snapshot_id
                                 FROM suite_freshness
                                WHERE current_snapshot_id IS NOT NULL)
       AND snapshot_id NOT IN (SELECT snapshot_id FROM legacy_trust_audit)
  ) WHERE rn > ?
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -count=1 -run 'TestRunSnapshotGCBatch|TestRunRetiredSnapshotGCChunk|SnapshotGC' ./internal/cache/`
Expected: `ok  	github.com/linsomniac/apt-cacher-ultra/internal/cache`

Run: `go test -race -count=1 ./internal/cache/ ./internal/gc/`
Expected: both `ok` (replay: 157 s and 11 s).

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutant | Fails |
|---|---|
| Delete the `legacy_trust_audit` predicate line | `f24_gc_test.go:45` (both forms) and `f24_gc_test.go:146` |
| Move it after the ranking: `) WHERE rn > ? AND snapshot_id NOT IN (SELECT snapshot_id FROM legacy_trust_audit)` | `f24_gc_test.go:146: RunSnapshotGCBatch(keep_displaced 1) = {OrphanReaped:0 DisplacedReaped:2}, <nil>; want one reaped` |

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/cache/gc.go internal/cache/f24_gc_test.go
git add internal/cache/gc.go internal/cache/f24_gc_test.go
git commit -m "$(cat <<'EOF'
Snapshot GC: keep snapshots the legacy trust audit has not judged

With gc.keep_displaced = 0 sub-job B deleted a displaced snapshot the
audit had deferred after a pool read fault — unmarked, the base's only
signed history — and never-downgrade then admitted an unsigned candidate
(deferred minor F-24). A snapshot still queued in legacy_trust_audit now
leaves B's displaced class before the ranking, as retired rows do, and
waits for the audit's verdict (ruling R-H1).

Mutants: dropping the predicate fails f24_gc_test.go:45 and :146; moving
it after the ranking fails f24_gc_test.go:146.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

### Task 2: An `anchor_corrupt` quarantine deletes the rows of the corrupt blob, and the audit removes the blob (ruling R-H2, review C-1)

The maintainer's decision (2026-10-24) on the overview's open decision: when the legacy trust audit quarantines a snapshot for `anchor_corrupt` — the stored `InRelease` blob's SHA-256 does not match its recorded hash — it also deletes the `url_path` rows that point at that blob, in the quarantine's own transaction, and the audit removes the corrupt pool file before the batch commits, so the next client request fetches a fresh copy instead of being served bytes apt rejects. The removal is review C-1's: without it, an upstream that still serves the same `InRelease` — the usual bit rot — is refetched onto the rotted file, which the miss path's `Finalize` keeps (its dedup branch), and served from it. It runs on main's goroutine, never on the writer; a failed removal is logged (ERROR `legacy_anchor_discard_failed`) and the quarantine proceeds. `anchor_oversized` and `anchor_missing` are unchanged (ruling R-H2, design note 2). Today the rows stay, and the hit path serves the rotted file (the handler test's red line, for an unchanged and a changed upstream alike).

**Files:**
- Create: `internal/cache/corrupt_anchor_test.go`, `internal/freshness/corrupt_anchor_test.go`, `internal/handler/corrupt_anchor_test.go`
- Modify: `internal/cache/legacy_audit.go` (`LegacyAuditDecision.DropAnchorRows`, `LegacyAuditEffect`, `ApplyLegacyAuditDecisions`, new `dropAnchorBlobRowsTx`), `internal/cache/legacy_audit_test.go` (`applyOne`), `internal/freshness/legacy_audit.go` (`legacyVerdict.dropAnchorRows`, the seam `legacyDiscardAnchor`, the pool file's removal, the decision, the quarantine line, `auditLegacySnapshot`'s corrupt branch)

**Interfaces:**
- Consumes: `(*cache.Cache).DiscardFinalizedBlob` (the integrity scanner's remedy); cache tests' `openCache`, `stubNow`, `seedBlob`, `commitInline`, `refcount` (`legacy_audit_test.go`); `deleteURLPathRowTx` (`gc_urlpath.go`); freshness tests' `newAdoptionTestEnv`, `adoptLegacy`, `realisticRelease`, `clearsignForTest`, `hashOf`, `lockedBuffer`, `logRecords`, `legacyAuditAnchorCap`; handler tests' `newSuiteRig`, `writeBlob`, `commitInlineSnapshot`, `rowInRelease`, `upFile`, `sha256Hex`, `silentLogger`.
- Produces: `cache.LegacyAuditDecision.DropAnchorRows bool`; `cache.LegacyAuditEffect{WasCurrent bool; AnchorRowsDeleted int}`; `(*cache.Cache).ApplyLegacyAuditDecisions(ctx, []LegacyAuditDecision) (map[int64]LegacyAuditEffect, error)`; unexported `dropAnchorBlobRowsTx(ctx, tx, snapshotID, now int64) (int, error)`; the field `anchor_rows_deleted` on `legacy_snapshot_quarantined`; the event `legacy_anchor_discard_failed` (ERROR: `snapshot_id`, `canonical_host`, `suite_path`, `blob_hash`, `err`); the test seam `legacyDiscardAnchor func(*cache.Cache, string) error`; test helpers `putAnchorRow`, `urlPathGone` (cache), `rotBlob` (freshness).

- [ ] **Step 1: Write the failing tests**

Create `internal/cache/corrupt_anchor_test.go`:

```go
package cache

import (
	"context"
	"errors"
	"os"
	"testing"
)

// putAnchorRow stores a url_path metadata row at scheme://host+path whose
// blob is hash.
func putAnchorRow(t *testing.T, c *Cache, scheme, host, path, hash string) {
	t.Helper()
	if err := c.PutURLPath(context.Background(), URLPath{CanonicalScheme: scheme, CanonicalHost: host, Path: path,
		BlobHash: &hash, UpstreamURL: scheme + "://" + host + path, IsMetadata: true}); err != nil {
		t.Fatal(err)
	}
}

// urlPathGone reports whether scheme://host+path has no url_path row.
func urlPathGone(t *testing.T, c *Cache, scheme, host, path string) bool {
	t.Helper()
	_, err := c.LookupURL(context.Background(), scheme, host, path)
	if err != nil && !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	return err != nil
}

// Ruling R-H2, Review Focus 1: an anchor_corrupt quarantine
// (DropAnchorRows) deletes, in its own transaction, every url_path row
// whose blob is the snapshot's InRelease — the row CommitAdoption seeded,
// the same path over https, another host's row of the same bytes — each
// row's pin released once, and leaves another blob's row, the blob row
// and the pool file (GC's) alone. A repeat finds nothing to do.
func TestApplyLegacyAuditDecisions_CorruptAnchorDropsEveryRowOfItsBlob(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, 1_800_000_000)()
	anchor := seedBlob(t, c, "a corrupt anchor")
	id := commitInline(t, c, "/r/dists/q", anchor)
	putAnchorRow(t, c, "https", "h", "/r/dists/q/InRelease", anchor)
	putAnchorRow(t, c, "http", "mirror", "/r/dists/q/InRelease", anchor)
	other := seedBlob(t, c, "another anchor")
	putAnchorRow(t, c, "http", "h", "/r/dists/other/InRelease", other)
	if err := c.EnqueueLegacyTrustAudit(ctx, id); err != nil {
		t.Fatal(err)
	}
	before := refcount(t, c, anchor)
	effects, err := c.ApplyLegacyAuditDecisions(ctx, []LegacyAuditDecision{
		{SnapshotID: id, Outcome: LegacyAuditQuarantine, DropAnchorRows: true}})
	if err != nil {
		t.Fatal(err)
	}
	if got := effects[id]; got != (LegacyAuditEffect{WasCurrent: true, AnchorRowsDeleted: 3}) {
		t.Fatalf("effect %+v, want current and 3 rows deleted", got)
	}
	for _, r := range [][3]string{{"http", "h", "/r/dists/q/InRelease"}, {"https", "h", "/r/dists/q/InRelease"},
		{"http", "mirror", "/r/dists/q/InRelease"}} {
		if !urlPathGone(t, c, r[0], r[1], r[2]) {
			t.Errorf("%s://%s%s kept, want deleted", r[0], r[1], r[2])
		}
	}
	if urlPathGone(t, c, "http", "h", "/r/dists/other/InRelease") {
		t.Fatal("another blob's row was deleted")
	}
	if got := refcount(t, c, anchor); got != before-4 {
		t.Fatalf("refcount %d, want %d (the current snapshot's pin and one per row, each released once)", got, before-4)
	}
	if _, err := os.Stat(c.BlobPath(anchor)); err != nil {
		t.Fatalf("pool file: %v, want kept (GC owns blob removal)", err)
	}
	again, err := c.ApplyLegacyAuditDecisions(ctx, []LegacyAuditDecision{
		{SnapshotID: id, Outcome: LegacyAuditQuarantine, DropAnchorRows: true}})
	if err != nil || again[id] != (LegacyAuditEffect{}) || refcount(t, c, anchor) != before-4 {
		t.Fatalf("repeat: %+v, %v, refcount %d; want no effect", again[id], err, refcount(t, c, anchor))
	}
}

// Design note 2: DropAnchorRows belongs to a quarantine. Set with any
// other outcome it rolls the batch back: nothing dequeued, nothing
// marked, no row deleted.
func TestApplyLegacyAuditDecisions_DropAnchorRowsNeedsAQuarantine(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	anchor := seedBlob(t, c, "an anchor kept")
	id := commitInline(t, c, "/r/dists/k", anchor)
	if _, err := c.db.Exec(`DELETE FROM suite_signed_seen`); err != nil {
		t.Fatal(err)
	}
	if err := c.EnqueueLegacyTrustAudit(ctx, id); err != nil {
		t.Fatal(err)
	}
	for _, o := range []LegacyAuditOutcome{LegacyAuditKeep, LegacyAuditDrop} {
		if _, err := c.ApplyLegacyAuditDecisions(ctx, []LegacyAuditDecision{{SnapshotID: id, Outcome: o, DropAnchorRows: true}}); err == nil {
			t.Fatalf("outcome %d with DropAnchorRows: no error", o)
		}
	}
	if items, err := c.LegacyAuditBatch(ctx, 0, 0); err != nil || len(items) != 1 {
		t.Fatalf("queue %+v (%v), want the snapshot still queued", items, err)
	}
	if _, ok, _ := c.SignedSeen(ctx, "http", "h", "/r/dists/k"); ok || urlPathGone(t, c, "http", "h", "/r/dists/k/InRelease") {
		t.Fatal("a rolled-back batch marked the base or deleted the anchor row")
	}
}
```

Create `internal/freshness/corrupt_anchor_test.go`:

```go
package freshness

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// rotBlob overwrites the pool file of hash with other bytes: bit rot the
// audit finds as anchor_corrupt.
func rotBlob(t *testing.T, c *cache.Cache, hash string) {
	t.Helper()
	p := c.BlobPath(hash)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("bit-rotted bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Ruling R-H2: of the audit's verdicts only anchor_corrupt asks the
// quarantine to delete the anchor's url_path rows — not anchor_missing,
// not anchor_oversized (not read in full), not the content verdicts.
func TestAuditLegacySnapshot_OnlyACorruptAnchorDropsItsRows(t *testing.T) {
	env := newAdoptionTestEnv(t)
	c := env.cache
	good, err := env.adopter.writeBlobBytes(context.Background(), clearsignForTest(t, realisticRelease()))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := env.adopter.writeBlobBytes(context.Background(), []byte(realisticRelease()))
	if err != nil {
		t.Fatal(err)
	}
	corrupt, err := env.adopter.writeBlobBytes(context.Background(), []byte("an anchor that will rot"))
	if err != nil {
		t.Fatal(err)
	}
	rotBlob(t, c, corrupt)
	absent := hashOf([]byte("never written"))
	for _, tc := range []struct {
		hash   string
		reason string
		drop   bool
	}{{corrupt, "anchor_corrupt", true}, {absent, "anchor_missing", false}, {plain, "legacy_plaintext", false}, {good, "", false}} {
		h := tc.hash
		v, err := auditLegacySnapshot(c, cache.LegacyAuditItem{Form: cache.FormInline, SingleAnchorHash: &h})
		if err != nil || v.reason != tc.reason || v.dropAnchorRows != tc.drop {
			t.Errorf("%q: verdict %+v err=%v, want reason %q dropAnchorRows %v", tc.reason, v, err, tc.reason, tc.drop)
		}
	}
	prevCap := legacyAuditAnchorCap
	legacyAuditAnchorCap = 16
	t.Cleanup(func() { legacyAuditAnchorCap = prevCap })
	if v, err := auditLegacySnapshot(c, cache.LegacyAuditItem{Form: cache.FormInline, SingleAnchorHash: &good}); err != nil ||
		v.reason != "anchor_oversized" || v.dropAnchorRows {
		t.Errorf("oversized: verdict %+v err=%v, want anchor_oversized without dropAnchorRows", v, err)
	}
}

// Ruling R-H2 end to end in the audit: two pre-v7 inline suites, one
// whose stored InRelease rotted in the pool and one whose InRelease is
// over the read cap. Both are quarantined; the rotted one's url_path
// anchor row is deleted (anchor_rows_deleted 1 on its line) and its pool
// file removed (review C-1), so the next client request fetches a fresh
// copy; the oversized one, not proven bad, keeps its row and its pool file
// (anchor_rows_deleted 0).
func TestRunLegacyTrustAudit_CorruptAnchorDropsItsRowsOversizedKeepsThem(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ctx := context.Background()
	c := env.cache
	rotten := adoptLegacy(t, env, "/r/dists/rotten", clearsignForTest(t, realisticRelease()+"Note: rotten\n"), "/r/pool/r_1_amd64.deb")
	big := adoptLegacy(t, env, "/r/dists/big", clearsignForTest(t, realisticRelease()+"Note: big\n"), "/r/pool/b_1_amd64.deb")
	snap, err := c.GetSuiteSnapshot(ctx, rotten)
	if err != nil {
		t.Fatal(err)
	}
	rotBlob(t, c, *snap.SingleAnchorHash)
	bigSnap, err := c.GetSuiteSnapshot(ctx, big)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ClearSignedSeenForTest(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.EnqueueLegacyTrustAudit(ctx, rotten, big); err != nil {
		t.Fatal(err)
	}
	prevCap := legacyAuditAnchorCap
	legacyAuditAnchorCap = 1024 // the rotted file is shorter; the big InRelease is longer
	t.Cleanup(func() { legacyAuditAnchorCap = prevCap })
	logs := &lockedBuffer{}
	res, err := RunLegacyTrustAudit(ctx, c, slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil || res.Quarantined != 2 {
		t.Fatalf("audit = %+v, %v; want both quarantined", res, err)
	}
	if _, err := c.LookupURL(ctx, "http", "h", "/r/dists/rotten/InRelease"); !errors.Is(err, cache.ErrNotFound) {
		t.Fatalf("the rotted anchor's row: %v, want deleted", err)
	}
	if u, err := c.LookupURL(ctx, "http", "h", "/r/dists/big/InRelease"); err != nil || u.BlobHash == nil {
		t.Fatalf("the oversized anchor's row: %+v, %v; want kept", u, err)
	}
	if _, err := os.Stat(c.BlobPath(*snap.SingleAnchorHash)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the rotted anchor's pool file: %v, want removed", err)
	}
	if _, err := os.Stat(c.BlobPath(*bigSnap.SingleAnchorHash)); err != nil {
		t.Fatalf("the oversized anchor's pool file: %v, want kept", err)
	}
	want := map[string]float64{"anchor_corrupt": 1, "anchor_oversized": 0}
	lines := logRecords(t, logs, "legacy_snapshot_quarantined")
	for _, m := range lines {
		n, ok := want[m["reason"].(string)]
		if !ok || m["anchor_rows_deleted"] != n {
			t.Errorf("line %v, want anchor_rows_deleted %v for its reason", m, n)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("%d quarantine lines, want 2", len(lines))
	}
}

// Ruling R-H2, review C-1: a pool file the audit cannot remove is logged
// (ERROR legacy_anchor_discard_failed) and the quarantine proceeds: its
// url_path rows still go, so a changed upstream still heals it.
func TestRunLegacyTrustAudit_AFailedDiscardIsLoggedAndTheQuarantineProceeds(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ctx := context.Background()
	c := env.cache
	rotten := adoptLegacy(t, env, "/r/dists/rotten", clearsignForTest(t, realisticRelease()+"Note: rotten\n"), "/r/pool/r_1_amd64.deb")
	snap, err := c.GetSuiteSnapshot(ctx, rotten)
	if err != nil {
		t.Fatal(err)
	}
	hash := *snap.SingleAnchorHash
	rotBlob(t, c, hash)
	if err := c.ClearSignedSeenForTest(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.EnqueueLegacyTrustAudit(ctx, rotten); err != nil {
		t.Fatal(err)
	}
	prev := legacyDiscardAnchor
	legacyDiscardAnchor = func(*cache.Cache, string) error { return errors.New("pool is read-only") }
	t.Cleanup(func() { legacyDiscardAnchor = prev })
	logs := &lockedBuffer{}
	res, err := RunLegacyTrustAudit(ctx, c, slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil || res.Quarantined != 1 {
		t.Fatalf("audit = %+v, %v; want the rotted snapshot quarantined", res, err)
	}
	if _, err := c.LookupURL(ctx, "http", "h", "/r/dists/rotten/InRelease"); !errors.Is(err, cache.ErrNotFound) {
		t.Fatalf("the rotted anchor's row: %v, want deleted", err)
	}
	recs := logRecords(t, logs, "legacy_anchor_discard_failed")
	if len(recs) != 1 || recs[0]["level"] != "ERROR" || recs[0]["blob_hash"] != hash ||
		recs[0]["err"] != "pool is read-only" || recs[0]["snapshot_id"] != float64(rotten) {
		t.Fatalf("records %v, want one ERROR naming the snapshot, the blob and the error", recs)
	}
}
```

Create `internal/handler/corrupt_anchor_test.go`:

```go
package handler

import (
	"context"
	"net/http"
	"os"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// Ruling R-H2 (the maintainer, 2026-10-24; review C-1) through the
// handler: a pre-v7 inline suite whose stored InRelease rotted in the pool
// is quarantined at startup (anchor_corrupt); the audit deletes the
// url_path row that served those bytes and removes the rotted pool file,
// so a client's next request is a miss that fetches a fresh copy from
// upstream, and the one after it a hit on that copy. Both when upstream
// still serves the same InRelease — the usual bit rot: the miss path's
// Finalize keeps an existing pool file of the same hash, so only the
// removal heals it — and when it has moved on.
func TestServeHTTP_ACorruptAnchorQuarantineRefetchesTheAnchor(t *testing.T) {
	old := simtest.ClearsignedFixture("Origin: old\nSuite: c\n")
	for _, tc := range []struct {
		name     string
		upstream []byte
	}{{"upstream unchanged", old}, {"upstream changed", rowInRelease}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newSuiteRig(t, true, 0)
			ctx := context.Background()
			hash := writeBlob(t, r.h, old)
			id := commitInlineSnapshot(t, r.h, "http", "127.0.0.1", "/r/dists/c", hash,
				[]cache.SnapshotMember{{Path: "InRelease", BlobHash: hash, DeclaredSHA256: hash}}, nil)
			p := r.c.BlobPath(hash)
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("bit-rotted bytes"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := r.c.ClearSignedSeenForTest(ctx); err != nil { // a pre-v7 cache has no markers
				t.Fatal(err)
			}
			if err := r.c.EnqueueLegacyTrustAudit(ctx, id); err != nil { // as the v7 migration does
				t.Fatal(err)
			}
			if res, err := freshness.RunLegacyTrustAudit(ctx, r.c, silentLogger()); err != nil || res.Quarantined != 1 {
				t.Fatalf("audit = %+v, %v; want the rotted snapshot quarantined", res, err)
			}
			r.serve("/r/dists/c/InRelease", upFile{body: tc.upstream})
			for _, want := range []string{"MISS", "HIT"} {
				rec := r.get("/r/dists/c/InRelease")
				if rec.Code != http.StatusOK || rec.Header().Get("X-Cache") != want || rec.Body.String() != string(tc.upstream) {
					t.Fatalf("status %d, X-Cache %q, body %q; want a %s serving upstream's InRelease",
						rec.Code, rec.Header().Get("X-Cache"), rec.Body.String(), want)
				}
			}
			u, err := r.c.LookupURL(ctx, "http", "127.0.0.1", "/r/dists/c/InRelease")
			if err != nil || u.BlobHash == nil || *u.BlobHash != sha256Hex(tc.upstream) {
				t.Fatalf("anchor row %+v, %v; want the fresh copy stored", u, err)
			}
		})
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -gcflags=-e -count=1 -run 'TestApplyLegacyAuditDecisions' ./internal/cache/`
Expected: build failure naming `unknown field DropAnchorRows in struct literal of type LegacyAuditDecision` (corrupt_anchor_test.go:51, :74, :95) and `undefined: LegacyAuditEffect` (:55, :75).

Run: `go test -gcflags=-e -count=1 -run 'CorruptAnchor|OnlyACorruptAnchor|AFailedDiscard' ./internal/freshness/`
Expected: build failure naming `v.dropAnchorRows undefined (type legacyVerdict has no field or method dropAnchorRows)` (corrupt_anchor_test.go:53, :61) and `undefined: legacyDiscardAnchor` (:147, :148, :149).

Run: `go test -count=1 -v -run TestServeHTTP_ACorruptAnchorQuarantineRefetchesTheAnchor ./internal/handler/`
Expected: FAIL — both subtests, `upstream_unchanged` and `upstream_changed`, with `corrupt_anchor_test.go:55: status 200, X-Cache "HIT", body "bit-rotted bytes"; want a MISS serving upstream's InRelease` (today the rotted file is served from its row).

- [ ] **Step 3: Implement the drop in the quarantine's transaction (`internal/cache/legacy_audit.go`)**

Replace `LegacyAuditDecision`:

old:
```go
// LegacyAuditDecision pairs a queued snapshot with its outcome.
type LegacyAuditDecision struct {
	SnapshotID int64
	Outcome    LegacyAuditOutcome
}
```
new:
```go
// LegacyAuditDecision pairs a queued snapshot with its outcome.
type LegacyAuditDecision struct {
	SnapshotID int64
	Outcome    LegacyAuditOutcome
	// DropAnchorRows, with a quarantine, also deletes every url_path row
	// that holds the snapshot's stored InRelease blob — the audit's
	// anchor_corrupt verdict (ruling R-H2). Set with any other outcome it
	// rolls the batch back, as an invalid outcome does.
	DropAnchorRows bool
}

// LegacyAuditEffect is what ApplyLegacyAuditDecisions did to one
// quarantined snapshot.
type LegacyAuditEffect struct {
	// WasCurrent: it was its suite's current snapshot, so the pointer and
	// validators were cleared and its pins released.
	WasCurrent bool
	// AnchorRowsDeleted counts the url_path rows of its InRelease blob
	// that DropAnchorRows deleted.
	AnchorRowsDeleted int
}
```

Replace `ApplyLegacyAuditDecisions`' doc, signature and loop head (through the `wasCurrent[d.SnapshotID] = cur` line):

old:
```go
// ApplyLegacyAuditDecisions applies one batch of audit decisions in ONE
// writer transaction (spec §6.1) and reports which quarantined snapshots
// were current. An invalid outcome rolls the whole batch back.
//
// AIDEV-NOTE: every effect is idempotent — a quarantine finds an already
// handled snapshot non-current (no second refcount release) and already
// retired; a repeated marker keeps the newest stamp — so a crash between
// batches never double-applies. Never release pins for a non-current
// snapshot: displacement already did.
func (c *Cache) ApplyLegacyAuditDecisions(ctx context.Context, decisions []LegacyAuditDecision) (map[int64]bool, error) {
	wasCurrent := make(map[int64]bool)
	err := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("ApplyLegacyAuditDecisions: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		now := nowUnix()
		for _, d := range decisions {
			switch d.Outcome {
			case LegacyAuditKeep:
				if err := markSnapshotBaseTx(ctx, tx, d.SnapshotID); err != nil {
					return err
				}
			case LegacyAuditDrop:
			case LegacyAuditQuarantine, LegacyAuditQuarantineSigned:
				cur, err := quarantineSnapshotTx(ctx, tx, d.SnapshotID, now)
				if err != nil {
					return err
				}
				wasCurrent[d.SnapshotID] = cur
```
new:
```go
// ApplyLegacyAuditDecisions applies one batch of audit decisions in ONE
// writer transaction (spec §6.1) and reports, for each quarantined
// snapshot, whether it was current and how many url_path rows of its
// InRelease blob it deleted (DropAnchorRows). An invalid outcome rolls the
// whole batch back.
//
// AIDEV-NOTE: every effect is idempotent — a quarantine finds an already
// handled snapshot non-current (no second refcount release) and already
// retired; a repeated marker keeps the newest stamp; a repeated anchor-row
// drop finds the rows gone — so a crash between batches never
// double-applies. Never release pins for a non-current snapshot:
// displacement already did. The anchor-row drop (ruling R-H2) is in the
// quarantine's own transaction on purpose: the dequeue commits with it,
// so a drop left to a later transaction that failed would never be
// retried, and the rows would serve the corrupt bytes for good.
func (c *Cache) ApplyLegacyAuditDecisions(ctx context.Context, decisions []LegacyAuditDecision) (map[int64]LegacyAuditEffect, error) {
	effects := make(map[int64]LegacyAuditEffect)
	err := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("ApplyLegacyAuditDecisions: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		now := nowUnix()
		for _, d := range decisions {
			if d.DropAnchorRows && d.Outcome != LegacyAuditQuarantine && d.Outcome != LegacyAuditQuarantineSigned {
				return fmt.Errorf("ApplyLegacyAuditDecisions: snapshot %d: anchor rows dropped without a quarantine (outcome %d)",
					d.SnapshotID, d.Outcome)
			}
			switch d.Outcome {
			case LegacyAuditKeep:
				if err := markSnapshotBaseTx(ctx, tx, d.SnapshotID); err != nil {
					return err
				}
			case LegacyAuditDrop:
			case LegacyAuditQuarantine, LegacyAuditQuarantineSigned:
				cur, err := quarantineSnapshotTx(ctx, tx, d.SnapshotID, now)
				if err != nil {
					return err
				}
				eff := LegacyAuditEffect{WasCurrent: cur}
				if d.DropAnchorRows {
					if eff.AnchorRowsDeleted, err = dropAnchorBlobRowsTx(ctx, tx, d.SnapshotID, now); err != nil {
						return err
					}
				}
				effects[d.SnapshotID] = eff
```

and its return, adding `dropAnchorBlobRowsTx` after the function:

old:
```go
	if err != nil {
		return nil, err
	}
	return wasCurrent, nil
}
```
new:
```go
	if err != nil {
		return nil, err
	}
	return effects, nil
}

// dropAnchorBlobRowsTx deletes every url_path row — under any scheme and
// host — whose blob is snapshotID's stored InRelease (ruling R-H2: the
// audit found that pool file's bytes no longer match their hash), each
// through deleteURLPathRowTx, and returns how many it deleted. A snapshot
// without an InRelease hash, or gone, deletes nothing.
//
// AIDEV-NOTE: rows only, never the pool file: GC owns blob removal, and
// the writer runs no pool I/O (ruling PD-5). Every row that names the
// blob serves the same bad bytes (the pool is content-addressed: a flat
// suite and its /. twin, or two remapped hosts, share it), so all of them
// go, and the next client request for each is a miss that fetches a fresh
// copy (TestServeHTTP_ACorruptAnchorQuarantineRefetchesTheAnchor).
func dropAnchorBlobRowsTx(ctx context.Context, tx *sql.Tx, snapshotID, now int64) (int, error) {
	var hash sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT inrelease_hash FROM suite_snapshot WHERE snapshot_id = ?`, snapshotID).Scan(&hash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("drop anchor rows of %d: read snapshot: %w", snapshotID, err)
	case !hash.Valid || hash.String == "":
		return 0, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT canonical_scheme, canonical_host, path FROM url_path WHERE blob_hash = ?`, hash.String)
	if err != nil {
		return 0, fmt.Errorf("drop anchor rows of %d: %w", snapshotID, err)
	}
	type rowKey struct{ scheme, host, path string }
	var keys []rowKey
	for rows.Next() {
		var k rowKey
		if err := rows.Scan(&k.scheme, &k.host, &k.path); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("drop anchor rows of %d: scan: %w", snapshotID, err)
		}
		keys = append(keys, k)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("drop anchor rows of %d: %w", snapshotID, err)
	}
	for _, k := range keys {
		if err := deleteURLPathRowTx(ctx, tx, k.scheme, k.host, k.path, hash, now); err != nil {
			return 0, fmt.Errorf("drop anchor rows of %d: %w", snapshotID, err)
		}
	}
	return len(keys), nil
}
```

In `internal/cache/legacy_audit_test.go`, `applyOne` reads the new result: its last line `return cur[id]` becomes `return cur[id].WasCurrent`.

- [ ] **Step 4: The audit asks for it on `anchor_corrupt` only, and removes the pool file (`internal/freshness/legacy_audit.go`)**

old:
```go
	// legacyClassify is auditLegacySnapshot's classifier; tests swap it to
	// inject a panic.
	legacyClassify = classifyLegacyInline
)
```
new:
```go
	// legacyClassify is auditLegacySnapshot's classifier; tests swap it to
	// inject a panic.
	legacyClassify = classifyLegacyInline
	// legacyDiscardAnchor removes a corrupt anchor's pool file (ruling
	// R-H2); tests swap it to inject a failure.
	legacyDiscardAnchor = func(c *cache.Cache, hash string) error { return c.DiscardFinalizedBlob(hash) }
)
```

old:
```go
type legacyVerdict struct {
	outcome cache.LegacyAuditOutcome
	reason  string // set for quarantines
}
```
new:
```go
type legacyVerdict struct {
	outcome cache.LegacyAuditOutcome
	reason  string // set for quarantines
	// dropAnchorRows: the quarantine also deletes the url_path rows of the
	// stored InRelease blob (anchor_corrupt only; ruling R-H2).
	dropAnchorRows bool
}
```

old:
```go
			decisions = append(decisions, cache.LegacyAuditDecision{SnapshotID: it.SnapshotID, Outcome: v.outcome})
```
new:
```go
			if v.dropAnchorRows {
				// AIDEV-NOTE: ruling R-H2 as amended by review C-1 (phase H1).
				// The rotted pool file goes too, not only its url_path rows:
				// the miss path's Finalize keeps an existing pool/<hash>, so
				// refetching an upstream InRelease that has not changed (the
				// usual bit rot) would store onto, and serve, the rotted file.
				// A corrupt file becomes a missing one, which the next request
				// heals. It runs here, on main's goroutine before anything
				// serves, before the batch commits and never on the writer
				// (PD-5), as the integrity scanner's DiscardFinalizedBlob does.
				// A commit lost after the removal re-audits the snapshot as
				// anchor_missing at the next start (rows kept, file missing:
				// healed at the next request). A failed removal is logged and
				// the quarantine proceeds: its rows still go.
				if err := legacyDiscardAnchor(c, *it.SingleAnchorHash); err != nil {
					logger.Error("legacy_anchor_discard_failed",
						"snapshot_id", it.SnapshotID,
						"canonical_host", it.CanonicalHost,
						"suite_path", it.SuitePath,
						"blob_hash", *it.SingleAnchorHash,
						"err", err,
					)
				}
			}
			decisions = append(decisions, cache.LegacyAuditDecision{SnapshotID: it.SnapshotID, Outcome: v.outcome,
				DropAnchorRows: v.dropAnchorRows})
```

(`dropAnchorRows` is set only on the `anchor_corrupt` branch, after `SingleAnchorHash` was read, so the dereference is safe.)

old:
```go
			wasCurrent, err := c.ApplyLegacyAuditDecisions(ctx, decisions)
```
new:
```go
			effects, err := c.ApplyLegacyAuditDecisions(ctx, decisions)
```

old:
```go
						"anchor_form", string(decided[i].Form),
						"was_current", wasCurrent[d.SnapshotID],
					)
```
new:
```go
						"anchor_form", string(decided[i].Form),
						"was_current", effects[d.SnapshotID].WasCurrent,
						"anchor_rows_deleted", effects[d.SnapshotID].AnchorRowsDeleted,
					)
```

old:
```go
	sum := sha256.Sum256(blob)
	if hex.EncodeToString(sum[:]) != *it.SingleAnchorHash {
		return quarantine("anchor_corrupt")
	}
```
new:
```go
	sum := sha256.Sum256(blob)
	if hex.EncodeToString(sum[:]) != *it.SingleAnchorHash {
		// AIDEV-NOTE: ruling R-H2 (the maintainer, 2026-10-24; review C-1).
		// The bytes were read and hashed, so this is evidence: the
		// quarantine also deletes the url_path rows that serve this pool
		// file, and RunLegacyTrustAudit removes the file itself, so the next
		// client request fetches a fresh copy instead of bytes apt rejects.
		// anchor_oversized (not read in full) and anchor_missing (a missing
		// pool file already falls through to the miss path) keep theirs.
		return legacyVerdict{outcome: cache.LegacyAuditQuarantine, reason: "anchor_corrupt", dropAnchorRows: true}, nil
	}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -count=1 -run 'TestApplyLegacyAuditDecisions' ./internal/cache/ && go test -count=1 -run 'TestAuditLegacySnapshot|TestRunLegacyTrustAudit' ./internal/freshness/ && go test -count=1 -run 'TestServeHTTP_ACorruptAnchorQuarantineRefetchesTheAnchor|Legacy|Audit' ./internal/handler/`
Expected: three `ok` lines.

Run: `go test -race -count=1 ./internal/cache/ ./internal/freshness/ ./internal/handler/ ./cmd/...`
Expected: all `ok`.

- [ ] **Step 6: Mutation checks** (one at a time, each reverted)

| Mutant | Fails |
|---|---|
| No removal (`if v.dropAnchorRows {` before the discard becomes `if false {`; review C-1's red line) | `handler/corrupt_anchor_test.go:55: status 200, X-Cache "MISS", body "bit-rotted bytes"; want a MISS serving upstream's InRelease` (subtest `upstream_unchanged` only: a changed upstream still heals through the row delete), `freshness/corrupt_anchor_test.go:109: the rotted anchor's pool file: <nil>, want removed`, `freshness/corrupt_anchor_test.go:161: records [], want one ERROR naming the snapshot, the blob and the error` |
| The removal for every inline quarantine (`if v.outcome == cache.LegacyAuditQuarantine && it.SingleAnchorHash != nil {`) | `freshness/corrupt_anchor_test.go:112: the oversized anchor's pool file: stat …: no such file or directory, want kept` |
| A failed removal not logged (`… err != nil && false {`) | `freshness/corrupt_anchor_test.go:161: records [], want one ERROR naming the snapshot, the blob and the error` |
| `dropAnchorRows: false` in the `anchor_corrupt` verdict | `freshness/corrupt_anchor_test.go:54` (`dropAnchorRows:false … want … true`), `:103: the rotted anchor's row: <nil>, want deleted`, `:156` (the same, in the failed-removal test), `handler/corrupt_anchor_test.go:55: status 200, X-Cache "HIT", body "bit-rotted bytes"` (both subtests) |
| The row query scoped to one scheme and host (`… AND canonical_scheme = 'http' AND canonical_host = 'h'`) | `cache/corrupt_anchor_test.go:56: effect {WasCurrent:true AnchorRowsDeleted:1}, want current and 3 rows deleted` |
| The `DropAnchorRows` validation removed (`if false {`) | `cache/corrupt_anchor_test.go:96: outcome 1 with DropAnchorRows: no error` |
| `anchor_oversized` drops its rows too | `freshness/corrupt_anchor_test.go:62` and `:106: the oversized anchor's row: <nil>, cache: not found; want kept` |

The first four rows were replayed in the revision of 2026-10-25 (a fresh copy of eadfd80 with Tasks 1–16 applied); the cache rows are unchanged from the draft's replay.

- [ ] **Step 7: Commit**

```bash
gofmt -l internal/cache/legacy_audit.go internal/cache/legacy_audit_test.go internal/cache/corrupt_anchor_test.go internal/freshness/legacy_audit.go internal/freshness/corrupt_anchor_test.go internal/handler/corrupt_anchor_test.go
git add internal/cache/legacy_audit.go internal/cache/legacy_audit_test.go internal/cache/corrupt_anchor_test.go internal/freshness/legacy_audit.go internal/freshness/corrupt_anchor_test.go internal/handler/corrupt_anchor_test.go
git commit -m "$(cat <<'EOF'
Legacy audit: a corrupt anchor's quarantine deletes its url_path rows

When the audit quarantines a pre-v7 snapshot for anchor_corrupt (the
stored InRelease no longer matches its hash) it now also deletes every
url_path row that names that pool file, in the quarantine's own writer
transaction, so the next client request fetches a fresh copy instead of
bytes apt rejects (the maintainer's decision, ruling R-H2). The audit also
removes the corrupt pool file, on main's goroutine before the batch
commits: the miss path keeps an existing pool file of the same hash, so an
upstream still serving the same InRelease was refetched onto the rotted
file (review C-1). A failed removal logs ERROR legacy_anchor_discard_failed
and the quarantine proceeds. anchor_oversized and anchor_missing are
unchanged. legacy_snapshot_quarantined gains anchor_rows_deleted.

Mutants: no removal fails handler corrupt_anchor_test.go:55 (upstream
unchanged) and freshness corrupt_anchor_test.go:109, :161; a removal for
every quarantine fails :112; an unlogged failure fails :161; no drop fails
freshness :54, :103, :156 and handler :55; a scheme/host-scoped drop fails
cache corrupt_anchor_test.go:56; no validation fails :96; an oversized drop
fails freshness :62 and :106.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

### Task 3: snake_case event names (rulings R-H3 and R-H6)

The 43 prose log messages this branch added — the 19 of the table under ruling R-H3 and the 24 of ruling R-H6's — become snake_case events, each keeping its level and fields, in the code that emits them, the tests that match them, the AIDEV-NOTEs and comments that quote them, `docs/log-fields.md` and `docs/configuration.md`. `docs/log-fields.md` gains a row for `adoption_require_signature_disabled`, whose prose message was its only explanation, and one shared row for the 11 renamed DEBUG lines it never documented, each with the meaning its prose carried; six of those are named to keep that meaning (review sweep M-6, design note 3). The overview's and the spec's quotes of the current names change in Task 18 (the overview's carry-forward items, which list the old names as what H renames, and the spec's revision history keep them); the executed phase plans keep theirs (historical by convention). Two guard tests: `TestSweptEventNames` pins the 43 renames three ways — no old name in any Go file, each new name in production code, a `docs/log-fields.md` row for each (a backquoted name in a table row's first cell) — so a later prose name or a typo fails it; `TestLogMessagesAreSnakeCase` requires every log message of production code to be snake_case or one of the 78 prose messages `main` already logs, listed in the test, so a new prose message fails the build (ruling R-H6, design note 3). Their failure messages say where to fix (review M-6). The renames are unreleased: nothing outside the repository depends on them.

**Files:**
- Create: `internal/observability/event_names_test.go`
- Modify (by the script in Step 3; the list is what it changes at 6786529): `internal/handler/handler.go`, `suite_rows.go`, `suite_hit_test.go`, `suite_rows_test.go`; `cmd/apt-cacher-ultra/main.go`, `adopt_unsigned_wiring_test.go`, `shutdown_drain_test.go`; `internal/freshness/adoption.go`, `anchorless.go`, `check.go`, `current_form.go`, `first_adoption.go`, `flip_barrier.go`, `freshness.go`, `legacy_audit.go`, `reaper.go`, `anchorless_test.go`, `capacity_test.go`, `check_streak_test.go`, `current_form_test.go`, `first_adoption_test.go`, `flat_first_adoption_test.go`, `flat_reap_test.go`, `reaper_junk_test.go`, `reaper_test.go`, `unsigned_test.go`; `internal/admin/handlers.go`, `forget_test.go`; `docs/log-fields.md`, `docs/configuration.md`

**Interfaces:**
- Consumes: nothing new.
- Produces: the 43 event names (the tables under R-H3 and R-H6), used by every later task; the guard's `logSinks` (a later helper that forwards a message parameter is added there, and a later helper must not reuse a listed name for anything else — Task 7's GC helper is `logGCPanic` for that reason) and `mainProseMessages` (a later rename of one of `main`'s prose messages removes it there); the test helpers `logMessages(t) map[string]string` (every production log message with the first place it is logged) and `documentedEvents(t) map[string]bool` (the names `docs/log-fields.md` gives a row), which Task 18's `TestEveryEventIsDocumented` uses.

- [ ] **Step 1: Write the failing guard tests**

Create `internal/observability/event_names_test.go`:

```go
package observability

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// sweptEvent is a prose log message the flat-repository branch added and
// the snake_case event it became.
type sweptEvent struct{ old, event string }

// sweptEvents are the 43 renamed messages: the 19 the overview listed
// (ruling R-H3), then the 24 it did not (ruling R-H6).
var sweptEvents = []sweptEvent{
	{"suite onboarding failed", "suite_onboarding_failed"},
	{"suite signed-seen mark failed", "suite_signed_seen_mark_failed"},
	{"suite cap usage read failed", "suite_cap_usage_read_failed"},
	{"freshness: read anchor rows failed", "freshness_read_anchor_rows_failed"},
	{"freshness: read current snapshot failed", "freshness_read_current_snapshot_failed"},
	{"adoption: served anchor read failed; the flip barrier runs", "adoption_served_anchor_read_failed"},
	{"adoption: signed-anchor re-mark failed", "adoption_signed_anchor_remark_failed"},
	{"suite reaper: list suites failed", "suite_reaper_list_suites_failed"},
	{"suite reaper: read suite row failed", "suite_reaper_read_suite_row_failed"},
	{"suite reaper: delete metadata failed", "suite_reaper_delete_metadata_failed"},
	{"suite reaper: reap failed", "suite_reaper_reap_failed"},
	{"suite reaper: list unadopted anchors failed", "suite_reaper_list_unadopted_anchors_failed"},
	{"suite reaper: junk anchor reads failed", "suite_reaper_junk_anchor_reads_failed"},
	{"suite reaper: marker sweep failed", "suite_reaper_marker_sweep_failed"},
	{"admin: forget failed", "admin_forget_failed"},
	{"freshness: anchorless index over its size cap, dropped for the miss path", "freshness_anchorless_index_over_cap"},
	{"freshness: delete anchorless rows failed", "freshness_delete_anchorless_rows_failed"},
	{"suite reaper: list unadopted flat indexes failed", "suite_reaper_list_unadopted_flat_indexes_failed"},
	{"suite reaper: read signed evidence failed", "suite_reaper_read_signed_evidence_failed"},
	// ruling R-H6
	{"adoption: read prior snapshot failed", "adoption_read_prior_snapshot_failed"},
	{"adoption.require_signature = false: an empty keyring is allowed at startup; plain-text or malformed InRelease files are never adopted as signed metadata, and with an empty keyring signed suites cannot adopt until keys are installed or adoption.accept_any_signer is set", "adoption_require_signature_disabled"},
	{"displaced snapshot lookup failed", "displaced_snapshot_lookup_failed"},
	{"freshness: anchorless index failed its sniff, kept", "freshness_anchorless_index_sniff_failed_kept"},
	{"freshness: check cancelled", "freshness_check_cancelled"},
	{"freshness: check ended by shutdown", "freshness_check_ended_by_shutdown"},
	{"freshness: clamp adoption retry failed", "freshness_clamp_adoption_retry_failed"},
	{"freshness: clear adoption failure failed", "freshness_clear_adoption_failure_failed"},
	{"freshness: deferred never-adopted suite paced until its retry or the next refresh check", "freshness_deferred_never_adopted_suite_paced_until_retry"},
	{"freshness: never-adopted suite without a client-created anchor or flat index row, dormant", "freshness_never_adopted_suite_dormant_no_client_created_row"},
	{"freshness: never-adopted suite without recent interest, dormant", "freshness_never_adopted_suite_dormant_no_recent_interest"},
	{"freshness: no baseline anchor hash, skipping", "freshness_check_skipped_no_baseline_anchor_hash"},
	{"freshness: persist failure bump after a panic failed", "freshness_persist_failure_bump_after_panic_failed"},
	{"freshness: read suite row after a panic failed", "freshness_read_suite_row_after_panic_failed"},
	{"freshness: refused suite paced until its retry", "freshness_refused_suite_paced_until_retry"},
	{"freshness: repair deferred, no adoption slot", "freshness_repair_deferred_no_adoption_slot"},
	{"legacy trust audit: cached anchor skipped", "legacy_trust_audit_cached_anchor_skipped"},
	{"shutdown wait panicked", "shutdown_wait_panicked"},
	{"snapshot adopted_at lookup failed", "snapshot_adopted_at_lookup_failed"},
	{"suite cached anchor read failed", "suite_cached_anchor_read_failed"},
	{"suite file head read failed", "suite_file_head_read_failed"},
	{"suite reaper: suite busy, retried next run", "suite_reaper_suite_busy"},
	{"suite signed-seen backstop read failed", "suite_signed_seen_backstop_read_failed"},
	{"suite touch failed", "suite_touch_failed"},
}

// mainProseMessages are the prose messages main already logs. They are
// released (an operator's alert may match one), so they are the only
// messages that may stay prose (ruling R-H6). Renaming one removes it here.
var mainProseMessages = []string{
	"admin listener accepting",
	"admin shutdown returned error",
	"adoption.accept_any_signer = true: signatures are decoded but not cryptographically verified for unpinned suites; apt clients on the fleet remain the authoritative trust anchor (auditable per SPEC2 §5.1)",
	"adoption: commit failed",
	"adoption: hot-set computation failed (skipping prefetch)",
	"adoption: insert candidate failed",
	"adoption: natural key already adopted",
	"adoption: persist InRelease failed",
	"adoption: persist Release failed",
	"adoption: persist Release.gpg failed",
	"adoption: reusing orphaned candidate",
	"apt-cacher-ultra starting",
	"apt-cacher-ultra stopped",
	"blob existence check failed",
	"blob open failed",
	"blob stat failed",
	"blob written",
	"by-hash provenance lookup failed",
	"by-hash url_path insert failed",
	"cache close failed",
	"cache lookup failed",
	"cache write failed",
	"evict url_path failed",
	"fetch retry",
	"freshness: adopted suites stale beyond threshold",
	"freshness check failed",
	"freshness: host limiter acquire aborted",
	"freshness: InRelease 404 on first-ever check, falling back to detached",
	"freshness: list suites failed",
	"freshness: lookup InRelease url_path failed",
	"freshness: lookup Release.gpg url_path failed",
	"freshness: lookup Release url_path failed",
	"freshness: metadata anchor missing for adopted suite; reconstructing",
	"freshness: no cached InRelease url_path, skipping",
	"freshness: no cached Release.gpg url_path, skipping detached",
	"freshness: no cached Release url_path, skipping detached",
	"freshness: persist failure-bump failed",
	"freshness: persist success failed",
	"freshness: read suite row failed",
	"freshness: Release.gpg fetch failed; deferring adoption",
	"freshness: Release.gpg fetch returned non-200; deferring adoption",
	"freshness: re-seed metadata anchor failed",
	"freshness: snapshot lookup failed; falling back to url_path",
	"gc startup pass failed",
	"http force-close returned error",
	"http listener accepting",
	"https force-close returned error",
	"http shutdown returned error",
	"https listener accepting",
	"https shutdown returned error",
	"InRelease changed at upstream",
	"integrity: blob missing",
	"integrity: discard blob failed",
	"integrity: hash blob failed",
	"integrity: scan failed",
	"keyring: parse embedded failed; skipping",
	"keyring: parse failed; skipping file",
	"keyring: read dir failed",
	"keyring: stat dir failed",
	"listener exited unexpectedly",
	"package_hash lookup failed",
	"package_hash validate lookup failed",
	"phase2 adoption enabled",
	"post-recovery blob open failed",
	"post-recovery blob stat failed",
	"Release changed at upstream",
	"schema current",
	"schema migrated",
	"shutdown signal received",
	"singleflight coalesced",
	"snapshot_member lookup failed",
	"startup failed",
	"startup tmp sweep failed",
	"strict-mode coverage lookup failed",
	"suite_freshness lookup failed",
	"touch failed",
	"tunnel manager drain returned error",
	"unclassified fetch error",
}

// logSinks maps every function that logs one of its arguments as a
// record's message to that argument's index: slog's methods and this
// repository's forwarding helpers. A new helper that forwards a message
// parameter must be added here, so its callers' messages are checked.
var logSinks = map[string]int{
	"Debug": 0, "Info": 0, "Warn": 0, "Error": 0,
	"DebugContext": 1, "InfoContext": 1, "WarnContext": 1, "ErrorContext": 1,
	"Log": 2, "LogAttrs": 2,
	"warn":              1, // (*freshness.Reaper).warn
	"logPanic":          1, // freshness
	"reportPanic":       0, // (*freshness.Checker).reportPanic
	"logGoroutinePanic": 0, // (*handler.Handler).logGoroutinePanic
	"emitTlsMitmLog":    2, // cmd/apt-cacher-ultra: internal/proxy's events
	"LogFn":             1, // internal/proxy and tlsmitm event callbacks
	"logFn":             1,
}

// fieldMessages are the message arguments that are neither a literal, a
// constant nor a forwarding helper's parameter, each with the constants
// its value comes from.
var fieldMessages = map[string][]string{
	"internal/freshness/reaper.go h.step": {"stepReadRow", "stepDeleteMetadata", "stepReap"},
}

var snakeCase = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)

type goFile struct {
	rel  string // relative to the repository root
	src  string
	file *ast.File
}

var guardFset = token.NewFileSet()

// parseTree parses every Go file under internal/ and cmd/ but this
// package's guard tests.
func parseTree(t *testing.T) []goFile {
	t.Helper()
	root := filepath.Join("..", "..")
	var files []goFile
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "event_names_test.go") {
				return err
			}
			src, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			f, err := parser.ParseFile(guardFset, p, src, 0)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			files = append(files, goFile{rel: filepath.ToSlash(rel), src: string(src), file: f})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return files
}

// logMessages returns every log message of production code (internal/
// and cmd/, test files aside) with the first place it is logged: the
// message argument of each logSinks call, when it is a string literal or
// a string constant of its package; the constants fieldMessages lists for
// a field. Any other message argument is reported (t.Errorf) unless it is
// a parameter of a forwarding helper logSinks lists, whose own callers
// are checked.
func logMessages(t *testing.T) map[string]string {
	t.Helper()
	files := parseTree(t)
	consts := map[string]map[string]string{} // package directory -> name -> value
	for _, f := range files {
		if strings.HasSuffix(f.rel, "_test.go") {
			continue
		}
		dir := filepath.Dir(f.rel)
		if consts[dir] == nil {
			consts[dir] = map[string]string{}
		}
		for _, decl := range f.file.Decls {
			g, ok := decl.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, spec := range g.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if s, err := strconv.Unquote(lit.Value); err == nil {
							consts[dir][name.Name] = s
						}
					}
				}
			}
		}
	}
	msgs := map[string]string{}
	add := func(where, msg string) {
		if _, ok := msgs[msg]; !ok {
			msgs[msg] = where
		}
	}
	for _, f := range files {
		if strings.HasSuffix(f.rel, "_test.go") {
			continue
		}
		dir := filepath.Dir(f.rel)
		var stack []ast.Node
		ast.Inspect(f.file, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				if pkg, ok := fn.X.(*ast.Ident); ok && pkg.Name == "http" {
					return true // http.Error writes a response body
				}
				name = fn.Sel.Name
			case *ast.Ident:
				name = fn.Name
			default:
				return true
			}
			idx, ok := logSinks[name]
			if !ok || len(call.Args) <= idx {
				return true
			}
			arg := call.Args[idx]
			where := f.rel + ":" + strconv.Itoa(guardFset.Position(arg.Pos()).Line)
			switch a := arg.(type) {
			case *ast.BasicLit:
				if a.Kind == token.STRING {
					s, _ := strconv.Unquote(a.Value)
					add(where, s)
					return true
				}
			case *ast.Ident:
				if v, ok := consts[dir][a.Name]; ok {
					add(where, v)
					return true
				}
				if forwardsParam(stack, a.Name) {
					return true
				}
			}
			key := f.rel + " " + types.ExprString(arg)
			names, ok := fieldMessages[key]
			if !ok {
				t.Errorf("%s: log message %s is neither a literal, a constant nor a listed forwarding helper's parameter; "+
					"add the helper to logSinks (or the expression to fieldMessages) in internal/observability/event_names_test.go",
					where, types.ExprString(arg))
				return true
			}
			for _, c := range names {
				v, ok := consts[dir][c]
				if !ok {
					t.Errorf("%s: constant %s of fieldMessages[%q] not found", where, c, key)
					continue
				}
				add(where+" ("+c+")", v)
			}
			return true
		})
	}
	return msgs
}

// Rulings R-H3 and R-H6 (phase H1): the 43 messages are snake_case. No Go
// file under internal/ or cmd/ — tests included — carries an old prose
// message; each new name is a string literal of production code (the
// reaper's step names are constants, so a literal, not only a call
// argument, counts); and docs/log-fields.md gives each one a row.
func TestSweptEventNames(t *testing.T) {
	literals := map[string]bool{}
	for _, f := range parseTree(t) {
		for _, e := range sweptEvents {
			if strings.Contains(f.src, e.old) {
				t.Errorf("%s still says %q", f.rel, e.old)
			}
		}
		if strings.HasSuffix(f.rel, "_test.go") {
			continue
		}
		ast.Inspect(f.file, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil {
					literals[s] = true
				}
			}
			return true
		})
	}
	documented := documentedEvents(t)
	for _, e := range sweptEvents {
		if !literals[e.event] {
			t.Errorf("no production code emits %s (was %q)", e.event, e.old)
		}
		if !documented[e.event] {
			t.Errorf("docs/log-fields.md has no row for %s", e.event)
		}
	}
}

// documentedEvents returns the names docs/log-fields.md gives a row: each
// backquoted name in the first cell of a table row (one row may name
// several events, as the suite reaper's failures share one). A mention
// elsewhere — in another row's meaning, say — is no row.
func documentedEvents(t *testing.T) map[string]bool {
	t.Helper()
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "log-fields.md"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, line := range strings.Split(string(doc), "\n") {
		cells := strings.SplitN(line, "|", 3)
		if !strings.HasPrefix(line, "| ") || len(cells) < 3 {
			continue
		}
		parts := strings.Split(cells[1], "`")
		for i := 1; i < len(parts); i += 2 {
			out[parts[i]] = true
		}
	}
	return out
}

// Ruling R-H6 (phase H1): every log message of production code is a
// snake_case event or one of main's prose messages, so a new prose message
// fails the build (logMessages says which messages it can read). Each
// allowlisted prose message must still be logged, so the list never
// outlives a rename.
func TestLogMessagesAreSnakeCase(t *testing.T) {
	allowed := map[string]bool{}
	for _, m := range mainProseMessages {
		allowed[m] = true
	}
	msgs := logMessages(t)
	for msg, where := range msgs {
		if !snakeCase.MatchString(msg) && !allowed[msg] {
			t.Errorf("%s: log message %q is not snake_case (and not one of main's prose messages); "+
				"use a snake_case event name and document it in docs/log-fields.md", where, msg)
		}
	}
	for _, m := range mainProseMessages {
		if _, ok := msgs[m]; !ok {
			t.Errorf("main's prose message %q is no longer logged: remove it from mainProseMessages", m)
		}
	}
}

// forwardsParam reports whether name is a parameter of the innermost
// function enclosing the top of stack, and that function forwards it: a
// declared function named in logSinks, or a function literal that is the
// value of a key named in logSinks (internal/proxy's LogFn callbacks).
func forwardsParam(stack []ast.Node, name string) bool {
	for i := len(stack) - 1; i >= 0; i-- {
		var params *ast.FieldList
		forwarder := false
		switch fn := stack[i].(type) {
		case *ast.FuncDecl:
			params, forwarder = fn.Type.Params, hasSink(fn.Name.Name)
		case *ast.FuncLit:
			params = fn.Type.Params
			if i > 0 {
				if kv, ok := stack[i-1].(*ast.KeyValueExpr); ok {
					if key, ok := kv.Key.(*ast.Ident); ok {
						forwarder = hasSink(key.Name)
					}
				}
			}
		default:
			continue
		}
		if !forwarder {
			return false
		}
		for _, field := range params.List {
			for _, p := range field.Names {
				if p.Name == name {
					return true
				}
			}
		}
		return false
	}
	return false
}

func hasSink(name string) bool {
	_, ok := logSinks[name]
	return ok
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -count=1 -run 'TestSweptEventNames|TestLogMessagesAreSnakeCase' ./internal/observability/`
Expected: FAIL with 202 error lines:
- 73 `event_names_test.go:348: <file> still says "<prose>"`, one per file and old message; the first is `internal/admin/forget_test.go still says "admin: forget failed"`;
- 43 `event_names_test.go:366: no production code emits <event> (was "<prose>")`;
- 43 `event_names_test.go:369: docs/log-fields.md has no row for <event>`;
- 43 `event_names_test.go:411: <file>:<line>: log message "<prose>" is not snake_case (and not one of main's prose messages); use a snake_case event name and document it in docs/log-fields.md`, one per message at the first place it is logged (in no fixed order: the messages come from a map; the reaper's three step constants are reported at their one `h.step` site, as `reaper.go:524 (stepReap)`).

No other line: at this commit every non-literal message is a listed helper's parameter or `h.step`, and each of `main`'s 78 is still logged.

- [ ] **Step 3: Rename (one script, from the repository root), then five hand edits**

```bash
#!/usr/bin/env bash
# Rulings R-H3 and R-H6: replace the 43 prose log messages this branch
# added with their snake_case event names in every Go file under internal/
# and cmd/ and in the two operator docs that quote them. Run from the
# repository root.
set -euo pipefail
pairs=(
  'suite onboarding failed|suite_onboarding_failed'
  'suite signed-seen mark failed|suite_signed_seen_mark_failed'
  'suite cap usage read failed|suite_cap_usage_read_failed'
  'freshness: read anchor rows failed|freshness_read_anchor_rows_failed'
  'freshness: read current snapshot failed|freshness_read_current_snapshot_failed'
  'adoption: served anchor read failed; the flip barrier runs|adoption_served_anchor_read_failed'
  'adoption: signed-anchor re-mark failed|adoption_signed_anchor_remark_failed'
  'suite reaper: list suites failed|suite_reaper_list_suites_failed'
  'suite reaper: read suite row failed|suite_reaper_read_suite_row_failed'
  'suite reaper: delete metadata failed|suite_reaper_delete_metadata_failed'
  'suite reaper: reap failed|suite_reaper_reap_failed'
  'suite reaper: list unadopted anchors failed|suite_reaper_list_unadopted_anchors_failed'
  'suite reaper: junk anchor reads failed|suite_reaper_junk_anchor_reads_failed'
  'suite reaper: marker sweep failed|suite_reaper_marker_sweep_failed'
  'admin: forget failed|admin_forget_failed'
  'freshness: anchorless index over its size cap, dropped for the miss path|freshness_anchorless_index_over_cap'
  'freshness: delete anchorless rows failed|freshness_delete_anchorless_rows_failed'
  'suite reaper: list unadopted flat indexes failed|suite_reaper_list_unadopted_flat_indexes_failed'
  'suite reaper: read signed evidence failed|suite_reaper_read_signed_evidence_failed'
  # ruling R-H6: the 24 the overview did not list
  'adoption: read prior snapshot failed|adoption_read_prior_snapshot_failed'
  'adoption.require_signature = false: an empty keyring is allowed at startup; plain-text or malformed InRelease files are never adopted as signed metadata, and with an empty keyring signed suites cannot adopt until keys are installed or adoption.accept_any_signer is set|adoption_require_signature_disabled'
  'displaced snapshot lookup failed|displaced_snapshot_lookup_failed'
  'freshness: anchorless index failed its sniff, kept|freshness_anchorless_index_sniff_failed_kept'
  'freshness: check cancelled|freshness_check_cancelled'
  'freshness: check ended by shutdown|freshness_check_ended_by_shutdown'
  'freshness: clamp adoption retry failed|freshness_clamp_adoption_retry_failed'
  'freshness: clear adoption failure failed|freshness_clear_adoption_failure_failed'
  'freshness: deferred never-adopted suite paced until its retry or the next refresh check|freshness_deferred_never_adopted_suite_paced_until_retry'
  'freshness: never-adopted suite without a client-created anchor or flat index row, dormant|freshness_never_adopted_suite_dormant_no_client_created_row'
  'freshness: never-adopted suite without recent interest, dormant|freshness_never_adopted_suite_dormant_no_recent_interest'
  'freshness: no baseline anchor hash, skipping|freshness_check_skipped_no_baseline_anchor_hash'
  'freshness: persist failure bump after a panic failed|freshness_persist_failure_bump_after_panic_failed'
  'freshness: read suite row after a panic failed|freshness_read_suite_row_after_panic_failed'
  'freshness: refused suite paced until its retry|freshness_refused_suite_paced_until_retry'
  'freshness: repair deferred, no adoption slot|freshness_repair_deferred_no_adoption_slot'
  'legacy trust audit: cached anchor skipped|legacy_trust_audit_cached_anchor_skipped'
  'shutdown wait panicked|shutdown_wait_panicked'
  'snapshot adopted_at lookup failed|snapshot_adopted_at_lookup_failed'
  'suite cached anchor read failed|suite_cached_anchor_read_failed'
  'suite file head read failed|suite_file_head_read_failed'
  'suite reaper: suite busy, retried next run|suite_reaper_suite_busy'
  'suite signed-seen backstop read failed|suite_signed_seen_backstop_read_failed'
  'suite touch failed|suite_touch_failed'
)
args=()
for p in "${pairs[@]}"; do args+=(-e "${p%%|*}"); done
files=$(grep -rlF "${args[@]}" --include='*.go' internal cmd | grep -v '/event_names_test.go$'; echo docs/log-fields.md docs/configuration.md)
for p in "${pairs[@]}"; do
  old=${p%%|*} new=${p##*|}
  OLD="$old" NEW="$new" perl -pi -e 's/\Q$ENV{OLD}\E/$ENV{NEW}/g' $files
done
```

Then four quotes the script cannot match, because they quote part of a message or split it across lines (the replay's race run caught the first), and two new rows:

`internal/freshness/anchorless_test.go` (`TestCheck_AnchorlessAnswersByStatus`, which counts the over-cap line by a fragment):

old:
```go
			if got := strings.Count(r.logs.String(), "anchorless index over its size cap"); got != map[bool]int{true: 1}[tc.failLog] {
```
new:
```go
			if got := strings.Count(r.logs.String(), "freshness_anchorless_index_over_cap"); got != map[bool]int{true: 1}[tc.failLog] {
```

`internal/freshness/anchorless.go` (`anchorlessOverCapWarnEvery`'s doc):

old:
```go
// anchorlessOverCapWarnEvery rate-limits the WARN "freshness: anchorless
// index over its size cap, dropped for the miss path": at most one line per
// minute process-wide, with
```
new:
```go
// anchorlessOverCapWarnEvery rate-limits the WARN
// freshness_anchorless_index_over_cap: at most one line per
// minute process-wide, with
```

`internal/freshness/capacity_test.go` (`TestCheck_HealthySuiteAsksForNoRepairSlot`, whose absence check matches a fragment and would pass vacuously after the rename):

old:
```go
	if strings.Contains(r.logs.String(), "repair deferred, no adoption slot") {
```
new:
```go
	if strings.Contains(r.logs.String(), "freshness_repair_deferred_no_adoption_slot") {
```

`docs/log-fields.md` (the reaper row's fields): `` `metadata_rows` (for `delete metadata failed`: rows deleted before the error) `` becomes `` `metadata_rows` (for `suite_reaper_delete_metadata_failed`: rows deleted before the error) ``.

`docs/log-fields.md`, after the row of `adoption_read_prior_snapshot_failed` (the operational events table), the new row:

```
| `adoption_require_signature_disabled` | WARN | none | Logged at start when `adoption.require_signature = false`: an empty keyring is allowed at startup. Plain-text or malformed `InRelease` files are never adopted as signed metadata either way, and with an empty keyring signed suites cannot adopt until keys are installed or `adoption.accept_any_signer` is set. |
```

and after the row of `freshness_read_current_snapshot_failed`, the 11 renamed DEBUG lines' shared row (review sweep M-6):

```
| `freshness_check_cancelled`, `freshness_check_ended_by_shutdown`, `freshness_check_skipped_no_baseline_anchor_hash`, `freshness_refused_suite_paced_until_retry`, `freshness_deferred_never_adopted_suite_paced_until_retry`, `freshness_never_adopted_suite_dormant_no_recent_interest`, `freshness_never_adopted_suite_dormant_no_client_created_row`, `freshness_anchorless_index_sniff_failed_kept`, `freshness_repair_deferred_no_adoption_slot`, `legacy_trust_audit_cached_anchor_skipped`, `suite_touch_failed` | DEBUG | `canonical_host`; `suite_path`, except for the sniff and the cached anchor, which name their file as `path`; `step` and `err` (`freshness_check_ended_by_shutdown`); `anchor` (`freshness_check_skipped_no_baseline_anchor_hash`); `reason` (the two `…_paced_until_retry`); `err` (`legacy_trust_audit_cached_anchor_skipped`, `suite_touch_failed`) | Routine outcomes, each named by what its message said before phase H1: `freshness_check_cancelled` — a check's request was cancelled (its context ended, as at shutdown), so it wrote nothing; `freshness_check_ended_by_shutdown` — a shutdown ended the check's local `step`: nothing written, not a failed check; `freshness_check_skipped_no_baseline_anchor_hash` — the suite has no cached hash of its `anchor` to compare with, so the check is skipped; `freshness_refused_suite_paced_until_retry` — an adopted suite its record paces (`reason`) is not probed before the record's retry time; `freshness_deferred_never_adopted_suite_paced_until_retry` — a never-adopted suite whose attempt was deferred is not probed until its record's retry or its next refresh check; `freshness_never_adopted_suite_dormant_no_recent_interest` — a never-adopted suite no client asked for in 7 days is dormant; `freshness_never_adopted_suite_dormant_no_client_created_row` — a never-adopted suite without a client-created anchor or flat index row is dormant; `freshness_anchorless_index_sniff_failed_kept` — an anchorless refresh's 200 for the index at `path` failed its sniff, so the cached row is kept (`acu_anchorless_sniff_rejected_total{host}`); `freshness_repair_deferred_no_adoption_slot` — a repair found no adoption slot and waits for a later unchanged check (`acu_repair_deferred_total{host}`); `legacy_trust_audit_cached_anchor_skipped` — the startup marker pass could not read the cached anchor at `path` and went on without it; `suite_touch_failed` — a suite's client-interest stamp could not be written; a later serve writes it. |
```

Then: `gofmt -l internal cmd` must list only the five drift files (the renames change string values, never an alignment). Read the `docs/log-fields.md` diff: the nine `suite_reaper_*` failure events share their row as the prose names did, and each other renamed event keeps its row. `grep -rnE 'over its size cap|served anchor read failed|signed-anchor re-mark|repair deferred, no adoption|backstop read failed|suite busy, retried' internal cmd docs/log-fields.md docs/configuration.md` must find nothing but `event_names_test.go`'s table and `anchorless.go:73`'s "The cached signed-anchor re-mark runs first" (prose about the step, not its event; the other fragments that remain — `forget failed\n`, `delete anchorless rows:` — are an HTTP body and an error prefix, not events; `touch failed` is `main`'s own message).

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -count=1 -run 'TestSweptEventNames|TestLogMessagesAreSnakeCase' ./internal/observability/`
Expected: `ok`.

Run: `go test -race -count=1 ./internal/... ./cmd/...`
Expected: all `ok` (the tests that matched a prose name now match its event).

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutant | Fails |
|---|---|
| `stepReap = "suite reaper: reap failed"` restored in `reaper.go` | `event_names_test.go:348: internal/freshness/reaper.go still says "suite reaper: reap failed"`, `event_names_test.go:366: no production code emits suite_reaper_reap_failed (was "suite reaper: reap failed")` and `event_names_test.go:411: internal/freshness/reaper.go:524 (stepReap): log message "suite reaper: reap failed" is not snake_case …` (the reaper's own tests assert only that no such line is logged) |
| `admin_forget_failed` misspelt `admin_forget_fail` in the WARN call of `handlers.go` | `forget_test.go:289: admin_forget_failed lines []; want one at WARN with the error` (the DEBUG call still emits the name, and the misspelling is snake_case, so the guards pass: the per-call tests own each call) |
| `suite_reaper_marker_sweep_failed` reverted to its prose in its `docs/log-fields.md` row | `event_names_test.go:369: docs/log-fields.md has no row for suite_reaper_marker_sweep_failed` |
| `freshness_check_cancelled` reverted to its prose in the shared DEBUG row's first cell (its meaning still names it) | `event_names_test.go:369: docs/log-fields.md has no row for freshness_check_cancelled` |
| The shared DEBUG row removed | eleven `event_names_test.go:369: docs/log-fields.md has no row for …` lines, one per event of the row |
| A new prose message: `gc_run_complete` logged as `gc run complete` by the startup pass in `internal/gc/gc.go` | `event_names_test.go:411: internal/gc/gc.go:289: log message "gc run complete" is not snake_case (and not one of main's prose messages); use a snake_case event name and document it in docs/log-fields.md` |
| A message in a variable: `msg := "suite_touch_failed"; h.logger.Debug(msg, …)` in `suite_rows.go` | `event_names_test.go:319: internal/handler/suite_rows.go:538: log message msg is neither a literal, a constant nor a listed forwarding helper's parameter; add the helper to logSinks (or the expression to fieldMessages) in internal/observability/event_names_test.go` |
| `main`'s `touch failed` renamed `touch_failed` in `suite_rows.go` | `event_names_test.go:417: main's prose message "touch failed" is no longer logged: remove it from mainProseMessages` |
| `"touch failed"` dropped from `mainProseMessages` (the test file: its lines shift up by one) | `event_names_test.go:410: internal/handler/suite_rows.go:532: log message "touch failed" is not snake_case (and not one of main's prose messages); …` |
| `logGoroutinePanic` dropped from `logSinks` (as above) | `event_names_test.go:318: internal/handler/handler.go:1404: log message event is neither a literal, a constant nor a listed forwarding helper's parameter; …` |

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/observability/event_names_test.go internal/handler/handler.go internal/handler/suite_rows.go cmd/apt-cacher-ultra/main.go internal/freshness/adoption.go internal/freshness/anchorless.go internal/freshness/check.go internal/freshness/current_form.go internal/freshness/first_adoption.go internal/freshness/flip_barrier.go internal/freshness/freshness.go internal/freshness/legacy_audit.go internal/freshness/reaper.go internal/admin/handlers.go
git add -A internal cmd docs/log-fields.md docs/configuration.md
git commit -m "$(cat <<'EOF'
Logs: snake_case names for every prose message this branch added

The 43 prose log messages of this branch (the overview's 19 - suite
onboarding failed, the suite reaper's steps, admin: forget failed, ... -
and 24 more: freshness: check cancelled, suite touch failed, ...) become
snake_case events in the style of the existing ones, each keeping its
level and fields, in the code, the tests, the comments, docs/log-fields.md
and docs/configuration.md (rulings R-H3 and R-H6). They are unreleased.
The 11 DEBUG lines log-fields never documented share one row, each with
the meaning its prose carried, and six are named to keep it. One guard
test pins that no old name is left, that production code emits each new
one and that log-fields gives it a row; another fails any log message that
is not snake_case, the 78 prose messages main already logs excepted.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

### Task 4: The status page and JSON: `retired_reaped`, the anchorless refresh, failing checks, last requested, the adoption scheme (carry-forwards; B-60, B-63, D-50, F-14, F-19)

Two overview carry-forwards and five deferred items, all on the admin status page: the F carry-forward (`gc.LastRunSummary.RetiredReaped` is not copied into the gc block: `gcInfoFromSummary` copies it), the G carry-forward (the anchorless refresh visible: `suites[].anchorless`, design note 5), B-63 (failing checks say so and when their backoff ends, the count alone for a suite the backoff does not schedule; design note 6), F-14 (a *Last requested* column), F-19 (`scheme://host` in Recent adoption outcomes, through `AdoptionEvent.Scheme`), D-50 (the Suites empty state in the admin-ui-spec's words) and B-60 (a fixture that contradicted its name). Every JSON change is additive to the locked contract (`TestJSONContractPreserved`'s golden gains three fields). `docs/admin-ui-spec.md` follows in Task 18.

**Files:**
- Create: `internal/admin/status_annotations_test.go`, `internal/freshness/adoption_ring_test.go`
- Modify: `internal/observability/ring.go` (`AdoptionEvent.Scheme`); `internal/freshness/freshness.go`, `adoption_failure.go` (the two `Record` calls), `retry.go` (`CheckBackoff`); `internal/admin/server.go` (`Config.CheckBackoff`), `status.go` (`suiteEntry.Anchorless`, `CheckStatus`, `gcInfo.RetiredReaped`, `adoptionEntry.Scheme`, `annotateSuiteEntries`, `adoptionEnabled`, `gcInfoFromSummary`), `status_template.go`, `status_test.go` (B-60), `json_contract_test.go`, `testdata/json_contract_golden.json` (regenerated); `cmd/apt-cacher-ultra/main.go` (wiring)

**Interfaces:**
- Consumes: admin tests' `newHealthyModel`, `renderHTMLForGolden`, `mustContain`, `formatUnixTimePtrTag`; freshness tests' `newRetryRig`, `retryRig.adopt`, `release`, `ver.panicOn`, `up.set`, `ring`, `hashOf`.
- Produces: `observability.AdoptionEvent.Scheme string`; `freshness.CheckBackoff(failCount int64) time.Duration`; `admin.Config.CheckBackoff func(failCount int64) time.Duration`; JSON `suites[].anchorless` (bool), `recent_adoptions[].scheme` (string), `gc.retired_reaped` (int); unexported `annotateSuiteEntries(es []suiteEntry, adoptionEnabled bool, checkBackoff func(int64) time.Duration, now int64)`, `(*Server).adoptionEnabled() bool`, `gcInfoFromSummary(gc.LastRunSummary) *gcInfo`.

- [ ] **Step 1: Write the failing tests**

Create `internal/admin/status_annotations_test.go`:

```go
package admin

import (
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/gc"
	"github.com/linsomniac/apt-cacher-ultra/internal/observability"
)

// Phase G ruling PG-14, Review Focus 5 (design note 5): suites[].anchorless
// marks a flat suite the cache refreshes without a Release — adoption
// enabled, flat, no current snapshot, a successful check, no record, no
// pending change — and nothing else: not a dists/ suite, not with
// adoption disabled, not an adopted, never-checked or refused suite, and
// not once a first adoption is under way (its pending change).
func TestSuiteEntries_Anchorless(t *testing.T) {
	now := int64(1_800_000_000)
	ok, id := now-60, int64(9)
	anchorless := func(mut func(*suiteEntry)) suiteEntry {
		e := suiteEntry{Scheme: "https", Host: "fury.example", SuitePath: "/org/deb", Flat: true, LastSuccessUnixTime: &ok}
		if mut != nil {
			mut(&e)
		}
		return e
	}
	for _, tc := range []struct {
		name    string
		e       suiteEntry
		enabled bool
		want    bool
	}{
		{"a Release-less flat suite", anchorless(nil), true, true},
		{"adoption disabled", anchorless(nil), false, false},
		{"a dists/ suite", anchorless(func(e *suiteEntry) { e.Flat, e.SuitePath = false, "/r/dists/x" }), true, false},
		{"adopted", anchorless(func(e *suiteEntry) { e.CurrentSnapshotID = &id }), true, false},
		{"never answered", anchorless(func(e *suiteEntry) { e.LastSuccessUnixTime = nil }), true, false},
		{"a record (unsigned_unconfirmed)", anchorless(func(e *suiteEntry) { e.AdoptionFailCount = 1 }), true, false},
		{"a first adoption under way", anchorless(func(e *suiteEntry) { e.InReleaseChangeSeenAtUnixTime = &ok }), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			es := []suiteEntry{tc.e}
			annotateSuiteEntries(es, tc.enabled, nil, now)
			if es[0].Anchorless != tc.want {
				t.Fatalf("anchorless %v, want %v", es[0].Anchorless, tc.want)
			}
			if want := map[bool]string{true: "refreshed without a Release"}[tc.want]; es[0].AdoptionStatus != want {
				t.Fatalf("Adoption cell %q, want %q", es[0].AdoptionStatus, want)
			}
		})
	}
}

// Deferred minor B-63 (design note 6; reviews M-3 and M-5): a suite whose
// checks keep ending unknown says so, and when its backoff from the last
// check ends — "backoff over" once it has — and without a backoff
// function only the count. A suite the scheduler does not queue on that
// backoff shows the count alone: a never-adopted suite no client asked
// for in 7 days, an adopted one its anchor_absent record paces. A
// record's own text is kept apart.
func TestSuiteEntries_FailingChecksSayWhenTheBackoffEnds(t *testing.T) {
	now := int64(1_800_000_000)
	backoff := func(k int64) time.Duration { return time.Duration(k) * time.Minute }
	recent, old, stale := now-60, now-600, now-8*24*3600
	reason, absent := "gpg_failed", "anchor_absent"
	snap := int64(7)
	es := []suiteEntry{
		{SuitePath: "/a", CheckFailCount: 3, LastCheckUnixTime: &recent, LastRequestedUnixTime: &recent},
		{SuitePath: "/b", CheckFailCount: 2, LastCheckUnixTime: &old, CurrentSnapshotID: &snap},
		{SuitePath: "/c", CheckFailCount: 0, LastCheckUnixTime: &recent, LastRequestedUnixTime: &recent},
		{SuitePath: "/d", CheckFailCount: 1, LastCheckUnixTime: &recent, LastRequestedUnixTime: &recent,
			AdoptionStatus: "adoption failing 1m · gpg_failed · retry due", AdoptionFailReason: &reason, AdoptionFailCount: 1},
		{SuitePath: "/dormant", CheckFailCount: 2, LastCheckUnixTime: &old, LastRequestedUnixTime: &stale},
		{SuitePath: "/paced", CheckFailCount: 1, LastCheckUnixTime: &old, CurrentSnapshotID: &snap,
			AdoptionFailReason: &absent, AdoptionFailCount: 2},
	}
	annotateSuiteEntries(es, true, backoff, now)
	for i, want := range []string{
		"checks failing ×3 · backoff ends in 2m",
		"checks failing ×2 · backoff over",
		"",
		"checks failing ×1 · backoff over",
		"checks failing ×2",
		"checks failing ×1",
	} {
		if es[i].CheckStatus != want {
			t.Errorf("%s: %q, want %q", es[i].SuitePath, es[i].CheckStatus, want)
		}
	}
	if es[3].AdoptionStatus != "adoption failing 1m · gpg_failed · retry due" {
		t.Fatalf("the record's text became %q", es[3].AdoptionStatus)
	}
	nb := []suiteEntry{{SuitePath: "/e", CheckFailCount: 4, LastCheckUnixTime: &recent, LastRequestedUnixTime: &recent}}
	annotateSuiteEntries(nb, true, nil, now)
	if nb[0].CheckStatus != "checks failing ×4" {
		t.Fatalf("without a backoff: %q, want the count alone", nb[0].CheckStatus)
	}
	off := []suiteEntry{{SuitePath: "/off", CheckFailCount: 2, LastCheckUnixTime: &old, LastRequestedUnixTime: &stale}}
	annotateSuiteEntries(off, false, backoff, now)
	if off[0].CheckStatus != "checks failing ×2 · backoff over" {
		t.Fatalf("adoption disabled: %q, want the backoff (no dormancy without an adopter)", off[0].CheckStatus)
	}
}

// The F carry-forward: the gc block copies each field of the last run's
// summary it shows — retired_reaped (sub-job C, phase F) among them.
func TestGCInfoFromSummary_CopiesEveryShownField(t *testing.T) {
	g := gcInfoFromSummary(gc.LastRunSummary{Phase: "periodic", AtUnixTime: 1_800_000_000, DurationSeconds: 1.5,
		BlobsReaped: 2, BytesReclaimed: 3, OrphanCandidatesReaped: 4, DisplacedReaped: 5, RetiredReaped: 6,
		URLPathRowsReaped: 7, PoolOrphansRepaired: 8, PoolOrphanBytesRepaired: 9, PoolUnlinkErrors: 10, DeadlineReached: true})
	want := gcInfo{LastRunPhase: "periodic", LastRunDurationSeconds: 1.5, LastRunBlobsReaped: 2, LastRunBytesReclaimed: 3,
		OrphanCandidatesReaped: 4, DisplacedReaped: 5, RetiredReaped: 6, URLPathRowsReaped: 7, PoolOrphansRepaired: 8,
		PoolOrphanBytesRepaired: 9, PoolUnlinkErrors: 10, LastRunDeadlineReached: true}
	if g.LastRunUnixTime == nil || *g.LastRunUnixTime != 1_800_000_000 {
		t.Fatalf("last run %v, want 1800000000", g.LastRunUnixTime)
	}
	g.LastRunUnixTime = nil
	if *g != want {
		t.Fatalf("gc block %+v, want %+v", *g, want)
	}
}

// Deferred minor F-19: the adoption ring carries the suite's scheme to
// recent_adoptions[].scheme.
func TestAdoptionEntries_CarryTheScheme(t *testing.T) {
	got := buildAdoptionEntries([]observability.AdoptionEvent{{Scheme: "http", Host: "h", SuitePath: "/r/dists/x", Outcome: "success"}})
	if len(got) != 1 || got[0].Scheme != "http" {
		t.Fatalf("entries %+v, want the scheme copied", got)
	}
}

// The HTML of the cells the sweep adds: the Last requested column, the anchorless
// suite's and the failing suite's Adoption cells, scheme://host in Recent
// adoption outcomes, Retired reaped in the GC panel, and the Suites empty
// state in the admin-ui-spec's words (deferred minor D-50).
func TestStatusHTML_ShowsTheSweepsCells(t *testing.T) {
	m := newHealthyModel()
	requested := int64(1_700_000_000)
	m.Suites = []suiteEntry{
		{Scheme: "https", Host: "fury.example", SuitePath: "/org/deb", Flat: true, Anchorless: true,
			AdoptionStatus: "refreshed without a Release", LastRequestedUnixTime: &requested},
		{Scheme: "http", Host: "h", SuitePath: "/r/dists/x", AdoptionStatus: "refused: upstream_older · retry due",
			CheckStatus: "checks failing ×3 · backoff ends in 2m"},
	}
	m.RecentAdoptions = []adoptionEntry{{Scheme: "http", Host: "archive.ubuntu.com", SuitePath: "dists/noble", Outcome: "success"}}
	m.GC.RetiredReaped = 6
	html := renderHTMLForGolden(t, m)
	mustContain(t, html,
		"<th>Last requested",
		`<td data-label="Last requested" class="time">`+string(formatUnixTimePtrTag(&requested))+`</td>`,
		`<td data-label="Adoption" class="mono">refreshed without a Release</td>`,
		`<td data-label="Adoption" class="mono">refused: upstream_older · retry due &middot; checks failing ×3 · backoff ends in 2m</td>`,
		`<td data-label="Host" class="host">http://archive.ubuntu.com</td>`,
		`<div class="k">Retired reaped</div>`,
		`<div class="v"><code>6</code></div>`,
	)
	m.Suites = nil
	if html := renderHTMLForGolden(t, m); !strings.Contains(html, "A suite is tracked once a client requests its InRelease through the proxy") {
		t.Fatal("the Suites empty state does not use the admin-ui-spec's copy")
	}
}
```

Create `internal/freshness/adoption_ring_test.go`:

```go
package freshness

import "testing"

// Deferred minor F-19: every adoption event the ring records — a
// success, and the panicked one recoverAdoption records — names the
// suite's scheme, which the status page shows as scheme://host.
func TestAdoptionRing_RecordsTheSuitesScheme(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopt()
	boom := r.release("boom", 0)
	r.ver.panicOn(hashOf(boom))
	r.up.set(boom)
	r.check()
	seen := map[string]bool{}
	for _, ev := range r.ring.Snapshot() {
		if ev.Scheme != "http" {
			t.Errorf("event %+v: scheme %q, want http", ev, ev.Scheme)
		}
		seen[ev.Outcome] = true
	}
	if !seen["success"] || !seen["panicked"] {
		t.Fatalf("ring outcomes %v, want a success and a panicked event", seen)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -gcflags=-e -count=1 -run 'TestSuiteEntries_|TestGCInfoFromSummary|TestAdoptionEntries_CarryTheScheme|TestStatusHTML_ShowsTheSweepsCells' ./internal/admin/`
Expected: build failure naming `undefined: annotateSuiteEntries`, `es[0].Anchorless undefined (type suiteEntry has no field or method Anchorless)`, `es[i].CheckStatus undefined`, `undefined: gcInfoFromSummary`, `unknown field RetiredReaped in struct literal of type gcInfo`, `unknown field Scheme in struct literal of type observability.AdoptionEvent`, `got[0].Scheme undefined (type adoptionEntry has no field or method Scheme)`.

Run: `go test -gcflags=-e -count=1 -run TestAdoptionRing ./internal/freshness/`
Expected: build failure naming `ev.Scheme undefined (type observability.AdoptionEvent has no field or method Scheme)`.

- [ ] **Step 3: The ring's scheme and the exported backoff**

`internal/observability/ring.go` (`AdoptionEvent`, first field):

old:
```go
type AdoptionEvent struct {
	// Host is the canonical_host of the suite.
	Host string
```
new:
```go
type AdoptionEvent struct {
	// Scheme is the canonical_scheme of the suite (http or https): an
	// http suite and its https namesake share host and path, and POST
	// /forget takes the scheme (deferred minor F-19). Surfaced as
	// recent_adoptions[].scheme.
	Scheme string
	// Host is the canonical_host of the suite.
	Host string
```

`internal/freshness/freshness.go` (the adoption goroutine's `Record`):

old:
```go
			c.adoptionRing.Record(observability.AdoptionEvent{
				Host:             req.suite.CanonicalHost,
```
new:
```go
			c.adoptionRing.Record(observability.AdoptionEvent{
				Scheme:           req.suite.CanonicalScheme,
				Host:             req.suite.CanonicalHost,
```

`internal/freshness/adoption_failure.go` (`recoverAdoption`'s `Record`):

old:
```go
			c.adoptionRing.Record(observability.AdoptionEvent{
				Host: host, SuitePath: req.suite.SuitePath, Outcome: "panicked", Reason: cache.ReasonPanic,
```
new:
```go
			c.adoptionRing.Record(observability.AdoptionEvent{
				Scheme: req.suite.CanonicalScheme, Host: host, SuitePath: req.suite.SuitePath, Outcome: "panicked", Reason: cache.ReasonPanic,
```

`internal/freshness/retry.go`, before `backoffDelay`:

```go
// CheckBackoff is the scheduler's backoff after a streak of failCount
// checks that ended unknown (check_fail_count, spec C4/C11): no failing
// suite is queued before last_check_at + CheckBackoff(failCount). The
// admin status page shows when that backoff ends (deferred minor B-63)
// through admin.Config.CheckBackoff, which main sets to this.
func CheckBackoff(failCount int64) time.Duration { return backoffDelay(failCount) }
```

- [ ] **Step 4: The admin model**

`internal/admin/server.go`, at the end of `Config`:

```go
	// CheckBackoff is OPTIONAL: the scheduler's backoff after a streak
	// of failed checks (freshness.CheckBackoff, which cmd wires), for the
	// suites table's "checks failing ×k · backoff ends in …" (deferred
	// minor B-63). nil: the text is the count alone. A
	// function, not an import: admin does not import internal/freshness.
	CheckBackoff func(failCount int64) time.Duration
```

`internal/admin/status.go` — imports gain `"strconv"` and `"github.com/linsomniac/apt-cacher-ultra/internal/gc"`. `suiteEntry`: replace the `Flat` field line by

```go
	Flat bool `json:"flat"`
	// Anchorless marks a flat suite the cache refreshes without a Release
	// (spec C4 Dispatch 4, phase G ruling PG-14): derived, see
	// annotateSuiteEntries. Additive to the locked JSON contract; the HTML
	// Adoption cell reads "refreshed without a Release".
	Anchorless                       bool   `json:"anchorless"`
```

and after `AdoptionStatus  string \`json:"-"\``:

```go
	// CheckStatus is "checks failing ×k · backoff ends in …" for a suite
	// whose checks keep ending unknown (deferred minor B-63);
	// annotateSuiteEntries sets it, and the Adoption cell shows it after
	// AdoptionStatus.
	CheckStatus string `json:"-"`
```

(gofmt realigns the `AdoptionFailing`/`AdoptionStatus`/`LagSeconds` group). `gcInfo` gains, after `DisplacedReaped`:

```go
	RetiredReaped           int     `json:"retired_reaped"` // snapshot-GC sub-job C (spec C9); additive
```

`adoptionEntry` gains a first field, and `buildAdoptionEntries` copies it (`Scheme: e.Scheme,`):

```go
	// Scheme is the suite's canonical scheme, http or https (deferred
	// minor F-19): the HTML Host cell shows scheme://host, as the suites
	// table does, so a host copied from it into POST /forget carries its
	// scheme. Additive to the locked JSON contract.
	Scheme    string `json:"scheme"`
```

In `buildStatusModel`, right after the `model := statusModel{…}` literal:

```go
	annotateSuiteEntries(model.Suites, s.adoptionEnabled(), s.cfg.CheckBackoff, wallclock.Now().Unix())
```

and its gc block becomes:

old:
```go
	gci := &gcInfo{}
	if summary, ok := s.cfg.GC.LastRunSummary(); ok {
		ts := summary.AtUnixTime
		gci.LastRunUnixTime = &ts
		gci.LastRunPhase = summary.Phase
		gci.LastRunBlobsReaped = summary.BlobsReaped
		gci.LastRunBytesReclaimed = summary.BytesReclaimed
		gci.LastRunDeadlineReached = summary.DeadlineReached
		gci.LastRunDurationSeconds = summary.DurationSeconds
		gci.OrphanCandidatesReaped = summary.OrphanCandidatesReaped
		gci.DisplacedReaped = summary.DisplacedReaped
		gci.URLPathRowsReaped = summary.URLPathRowsReaped
		gci.PoolOrphansRepaired = summary.PoolOrphansRepaired
		gci.PoolOrphanBytesRepaired = summary.PoolOrphanBytesRepaired
		gci.PoolUnlinkErrors = summary.PoolUnlinkErrors
	}
	model.GC = gci

	return model, "", nil
}
```
new:
```go
	gci := &gcInfo{}
	if summary, ok := s.cfg.GC.LastRunSummary(); ok {
		gci = gcInfoFromSummary(summary)
	}
	model.GC = gci

	return model, "", nil
}

// gcInfoFromSummary copies the last GC run's summary into the status
// page's gc block, field by field.
//
// AIDEV-NOTE: a field gc.LastRunSummary gains is not shown until it is
// copied here (retired_reaped waited from phase F to H1: the F
// carry-forward); TestGCInfoFromSummary_CopiesEveryShownField pins the
// copy of each field the block has.
func gcInfoFromSummary(summary gc.LastRunSummary) *gcInfo {
	ts := summary.AtUnixTime
	return &gcInfo{
		LastRunUnixTime:         &ts,
		LastRunPhase:            summary.Phase,
		LastRunBlobsReaped:      summary.BlobsReaped,
		LastRunBytesReclaimed:   summary.BytesReclaimed,
		LastRunDeadlineReached:  summary.DeadlineReached,
		LastRunDurationSeconds:  summary.DurationSeconds,
		OrphanCandidatesReaped:  summary.OrphanCandidatesReaped,
		DisplacedReaped:         summary.DisplacedReaped,
		RetiredReaped:           summary.RetiredReaped,
		URLPathRowsReaped:       summary.URLPathRowsReaped,
		PoolOrphansRepaired:     summary.PoolOrphansRepaired,
		PoolOrphanBytesRepaired: summary.PoolOrphanBytesRepaired,
		PoolUnlinkErrors:        summary.PoolUnlinkErrors,
	}
}
```

Before `laggingAnnotation`, add:

```go
// annotateSuiteEntries derives what the suites table shows beyond the
// rows' own fields: Anchorless (JSON and HTML: "refreshed without a
// Release" in the Adoption cell) and CheckStatus (HTML: "checks failing
// ×k · backoff ends in d", or "… backoff over" once it has: the earliest
// the scheduler may check the suite — a client's T1 may come sooner, a
// not-yet-stale suite later; the count alone for a suite the scheduler
// does not queue on its backoff (unscheduled) or without a backoff
// function). adoptionEnabled is the status page's own signal
// (Server.adoptionEnabled).
//
// AIDEV-NOTE: phase G ruling PG-14 (design note 5 of the H1 plan).
// Anchorless is derived, not stored: adoption enabled, a flat suite, no
// current snapshot, a successful check, no adoption record and no pending
// change. Only an answered anchorless check ends in that state
// (anchorlessRefresh clears the pending change and the record together):
// a first adoption, queued or running, leaves inrelease_change_seen_at
// set (checkFirst), a refusal or failure writes a record, a commit sets
// the current snapshot. With adoption disabled no anchorless check runs,
// and the Phase-1 check bumps last_success_at for an InRelease too, so
// the field is false there. One exception, for one check: a flat suite
// whose row an adoption-disabled InRelease miss created has
// last_success_at set (suiteSeed stamps it), so after adoption is
// enabled the field reads true until the suite's first check (review M-11
// of the H1 plan). If the freshness side changes any of these, change the
// derivation (TestSuiteEntries_Anchorless).
func annotateSuiteEntries(es []suiteEntry, adoptionEnabled bool, checkBackoff func(int64) time.Duration, now int64) {
	for i := range es {
		e := &es[i]
		e.Anchorless = adoptionEnabled && e.Flat && e.CurrentSnapshotID == nil && e.LastSuccessUnixTime != nil &&
			e.AdoptionFailCount == 0 && e.InReleaseChangeSeenAtUnixTime == nil
		if e.Anchorless {
			e.AdoptionStatus = "refreshed without a Release"
		}
		if e.CheckFailCount <= 0 {
			continue
		}
		e.CheckStatus = "checks failing ×" + strconv.FormatInt(e.CheckFailCount, 10)
		if checkBackoff == nil || e.LastCheckUnixTime == nil || unscheduled(e, adoptionEnabled, now) {
			continue
		}
		next := *e.LastCheckUnixTime + int64(checkBackoff(e.CheckFailCount)/time.Second)
		if next > now {
			e.CheckStatus += " · backoff ends in " + durationOf(next-now)
		} else {
			e.CheckStatus += " · backoff over"
		}
	}
}

// statusInterestWindow mirrors freshness's interestWindow (spec C4
// Dispatch 3), which admin does not import.
//
// AIDEV-NOTE: unscheduled mirrors the scheduler's suiteDue (review M-5 of
// the H1 plan): change both together.
const statusInterestWindow = 7 * 24 * time.Hour

// unscheduled reports a suite the scheduler does not queue on its failing
// checks' backoff: with adoption enabled, a never-adopted suite no client
// asked for within the interest window (dormant), and an adopted suite
// its anchor_absent or unsigned_disabled record paces (rulings PE-2,
// PE-12; the record's own text shows its retry). The Adoption cell then
// shows the failing count alone: "backoff over" would promise a check that
// never comes.
func unscheduled(e *suiteEntry, adoptionEnabled bool, now int64) bool {
	if !adoptionEnabled {
		return false
	}
	if e.CurrentSnapshotID == nil {
		if e.LastRequestedUnixTime == nil {
			return true
		}
		d := now - *e.LastRequestedUnixTime
		if d < 0 {
			d = -d // a stamp in the future counts as interest within the window, as interested reads it
		}
		return d >= int64(statusInterestWindow/time.Second)
	}
	return e.AdoptionFailCount > 0 && e.AdoptionFailReason != nil &&
		(*e.AdoptionFailReason == cache.ReasonAnchorAbsent || *e.AdoptionFailReason == cache.ReasonUnsignedDisabled)
}

// adoptionEnabled is the status page's signal that adoption is on: a
// keyring provider wired whose snapshot is non-nil (see
// buildHTMLRenderModel's AIDEV-NOTE).
func (s *Server) adoptionEnabled() bool {
	return s.cfg.Keyring != nil && s.cfg.Keyring.KeyringSnapshot() != nil
}
```

and in `buildHTMLRenderModel`:

old:
```go
	w := htmlRenderModel{statusModel: m}
	if s.cfg.Keyring != nil && s.cfg.Keyring.KeyringSnapshot() != nil {
		w.AdoptionEnabled = true
	}
```
new:
```go
	w := htmlRenderModel{statusModel: m, AdoptionEnabled: s.adoptionEnabled()}
```

- [ ] **Step 5: The template**

`internal/admin/status_template.go`, the suites table's header:

old:
```html
              <th>Host</th><th>Suite path</th><th>Last check</th><th>Last success</th>
              <th class="num">Snapshot</th>
```
new:
```html
              <th>Host</th><th>Suite path</th><th>Last check</th><th>Last success</th>
              <th>Last requested<details class="col-hint"><summary aria-label="What does Last requested mean?">i</summary><div class="col-hint__body">When a client last asked for this suite. A suite no client asks for in gc.suite_idle_ttl (720 h by default) is reaped.</div></details></th>
              <th class="num">Snapshot</th>
```

its row:

old:
```html
                <td data-label="Last success" class="time">{{unixTimePtr .LastSuccessUnixTime}}</td>
                <td data-label="Snapshot" class="num mono">{{i64Ptr .CurrentSnapshotID}}</td>
```
new:
```html
                <td data-label="Last success" class="time">{{unixTimePtr .LastSuccessUnixTime}}</td>
                <td data-label="Last requested" class="time">{{unixTimePtr .LastRequestedUnixTime}}</td>
                <td data-label="Snapshot" class="num mono">{{i64Ptr .CurrentSnapshotID}}</td>
```

the Adoption cell and the empty state:

old:
```html
                <td data-label="Adoption" class="mono">{{.AdoptionStatus}}</td>
              </tr>
            {{end}}
            </tbody>
          </table>
        </div>
        {{else}}<div class="empty"><div class="empty__head">NO SUITES TRACKED YET</div><div class="empty__body">Suites populate after the first adoption cycle.</div></div>{{end}}
```
new:
```html
                <td data-label="Adoption" class="mono">{{.AdoptionStatus}}{{if and .AdoptionStatus .CheckStatus}} &middot; {{end}}{{.CheckStatus}}</td>
              </tr>
            {{end}}
            </tbody>
          </table>
        </div>
        {{else}}<div class="empty"><div class="empty__head">NO SUITES TRACKED YET</div><div class="empty__body">A suite is tracked once a client requests its InRelease through the proxy (with adoption enabled, also its Release, or, for a flat repository, its root <code>Packages</code> or <code>Sources</code>).</div></div>{{end}}
```

Recent adoption outcomes' Host cell:

old:
```html
                <td data-label="Host" class="host">{{.Host}}</td>
                <td data-label="Suite path" class="mono">{{.SuitePath}}</td>
                <td data-label="Outcome">
```
new:
```html
                <td data-label="Host" class="host">{{if .Scheme}}{{.Scheme}}://{{end}}{{.Host}}</td>
                <td data-label="Suite path" class="mono">{{.SuitePath}}</td>
                <td data-label="Outcome">
```

the GC panel, after Displaced reaped:

old:
```html
          <div class="k">Displaced reaped</div>
          <div class="v"><code>{{.GC.DisplacedReaped}}</code></div>
```
new:
```html
          <div class="k">Displaced reaped</div>
          <div class="v"><code>{{.GC.DisplacedReaped}}</code></div>
          <div class="k">Retired reaped</div>
          <div class="v"><code>{{.GC.RetiredReaped}}</code></div>
```

- [ ] **Step 6: The wiring, the fixtures and the golden**

`cmd/apt-cacher-ultra/main.go`, in `admin.New`'s `Config`, after `Forgetter: reaper,`: `CheckBackoff: freshness.CheckBackoff,` (gofmt aligns it).

`internal/admin/status_test.go` (B-60), the `suites_healthy_no_lag` case:

old:
```go
			"suites_healthy_no_lag",
			func(m *htmlRenderModel) {
				m.Suites = []suiteEntry{{
					LastSuccessUnixTime:           ptrI64(1_000_000_000),
					InReleaseChangeSeenAtUnixTime: ptrI64(1_000_000_000),
				}}
			},
```
new:
```go
			"suites_healthy_no_lag",
			func(m *htmlRenderModel) {
				// No change pending (deferred minor B-60: a change stamp
				// here meant one seen and not yet adopted, not "no lag").
				m.Suites = []suiteEntry{{LastSuccessUnixTime: ptrI64(1_000_000_000)}}
			},
```

`internal/admin/json_contract_test.go` (`populatedJSONContractFixture`): after the flat suite's entry, a third suite

```go
			// Phase G ruling PG-14 (phase H1): suites[].anchorless, a
			// Release-less flat suite the cache refreshes without one.
			{
				Scheme:              "https",
				Host:                "fury.example",
				SuitePath:           "/org/deb",
				Flat:                true,
				Anchorless:          true,
				LastCheckUnixTime:   &suiteCheck,
				LastSuccessUnixTime: &suiteCheck,
			},
```

in its `GC`, after `DisplacedReaped: 1,`: `RetiredReaped: 4, // the F carry-forward (phase H1)`; and its three `RecentAdoptions` entries gain `Scheme: "http"`, `Scheme: "http"` and `Scheme: "https"` (in that order) as their first field. Then regenerate: `go test ./internal/admin -run TestJSONContractPreserved -update.jsoncontract`. The golden's diff is additive only: `"anchorless": false` in the two existing suites, the third suite, `"retired_reaped": 4` after `displaced_reaped`, and `"scheme"` first in each recent adoption (`json_contract_minimal_golden.json` is unchanged).

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test -count=1 ./internal/admin/ && go test -count=1 -run 'TestAdoptionRing|TestAdoptionFailure' ./internal/freshness/`
Expected: two `ok` lines.

Run: `go test -race -count=1 ./internal/admin/ ./internal/freshness/ ./internal/observability/ ./cmd/...`
Expected: all `ok`.

- [ ] **Step 8: Mutation checks** (one at a time, each reverted)

| Mutant | Fails |
|---|---|
| Drop `&& e.InReleaseChangeSeenAtUnixTime == nil` from the derivation | `status_annotations_test.go:46: anchorless true, want false` (subtest `a first adoption under way`) |
| `e.Anchorless = e.Flat && …` (adoption enabled ignored) | `status_annotations_test.go:46: anchorless true, want false` (subtest `adoption disabled`) |
| `next := now + int64(…)` (the last check ignored) | `status_annotations_test.go:88: /a: "checks failing ×3 · backoff ends in 3m", want "checks failing ×3 · backoff ends in 2m"` (and `/b`, `/d`, and `:102` for adoption disabled) |
| `unscheduled` ignores a dormant suite (its never-adopted branch returns false; reviews M-3, sweep M-5) | `status_annotations_test.go:88: /dormant: "checks failing ×2 · backoff over", want "checks failing ×2"` |
| `unscheduled` ignores a record that paces (`return false && …`) | `status_annotations_test.go:88: /paced: "checks failing ×1 · backoff over", want "checks failing ×1"` |
| `unscheduled` without its `!adoptionEnabled` guard | `status_annotations_test.go:102: adoption disabled: "checks failing ×2", want the backoff (no dormancy without an adopter)` |
| `RetiredReaped: summary.RetiredReaped,` dropped from `gcInfoFromSummary` | `status_annotations_test.go:120: gc block {… RetiredReaped:0 …}, want {… RetiredReaped:6 …}` |
| `Scheme:` dropped from `recoverAdoption`'s `Record` | `adoption_ring_test.go:18: event {Scheme: … Outcome:panicked …}: scheme "", want http` |
| The adoptions table's Host cell without the scheme | `status_annotations_test.go:149: rendered HTML missing "<td data-label=\"Host\" class=\"host\">http://archive.ubuntu.com</td>"` |

- [ ] **Step 9: Commit**

```bash
gofmt -l internal/observability/ring.go internal/freshness/freshness.go internal/freshness/adoption_failure.go internal/freshness/retry.go internal/freshness/adoption_ring_test.go internal/admin/server.go internal/admin/status.go internal/admin/status_template.go internal/admin/status_test.go internal/admin/status_annotations_test.go internal/admin/json_contract_test.go cmd/apt-cacher-ultra/main.go
git add internal/observability/ring.go internal/freshness/freshness.go internal/freshness/adoption_failure.go internal/freshness/retry.go internal/freshness/adoption_ring_test.go internal/admin/server.go internal/admin/status.go internal/admin/status_template.go internal/admin/status_test.go internal/admin/status_annotations_test.go internal/admin/json_contract_test.go internal/admin/testdata/json_contract_golden.json cmd/apt-cacher-ultra/main.go
git commit -m "$(cat <<'EOF'
Status page: retired_reaped, the anchorless refresh, failing checks

The gc block copies retired_reaped (the F carry-forward); suites[] gains
the derived anchorless field and the Adoption cell says "refreshed
without a Release" (the G carry-forward, ruling PG-14); a suite whose
checks keep failing says so and when its backoff ends, the count alone
for a dormant or record-paced suite the backoff does not schedule (B-63,
reviews M-3 and M-5, through admin.Config.CheckBackoff =
freshness.CheckBackoff); the
table gains a Last requested column (F-14); Recent adoption outcomes show
scheme://host from the ring's new Scheme (F-19); the Suites empty state
uses the admin-ui-spec's words (D-50); a contradicting fixture is fixed
(B-60). Every JSON change is additive; the golden is regenerated.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

### Task 5: The admin endpoints: one form parser, 400 for a malformed body, `OPTIONS`, `canonical_scheme`, a DEBUG line for a 409 (F-4, F-5, F-6, F-15, F-16)

Phase F's deferred admin items, which G-1 (2026-10-24) left: `/forget` and `/reconcile` share one form parser (F-4) that answers a malformed body 400 and only an oversized one 413 (F-5, design note 20); `OPTIONS /reconcile` names `POST` and every error body ends in one newline (F-6, design note 19); the admin lines name the scheme `canonical_scheme` — `admin_reconcile_requested`, which `main` already logs with `scheme`, keeps that key beside the new one (F-16, design note 4); and a forget that deletes nothing says why at DEBUG (F-15, design note 8). `docs/log-fields.md` follows in Task 18.

**Files:**
- Create: `internal/admin/endpoint_form_test.go`, `internal/freshness/forget_refusal_test.go`
- Modify: `internal/admin/handlers.go` (`suiteForm`, `parseSuiteForm`, `handleReconcile`, `handleForget`, `requireMutationGuard`'s bodies), `internal/admin/server.go` (`OPTIONS`), `internal/admin/forget_test.go` (`TestEndpoint_Forget_LogsAcceptedRequestsAndFailures`: `canonical_scheme`), `internal/freshness/forget.go` (`notForgotten`)

**Interfaces:**
- Consumes: admin tests' `guardedEndpoints`, `postForm`, `startAdminServer`, `withReconciler`, `withForgetter`, `withLogger`, `stubReconciler`, `stubForgetter`, `lockedBuffer`; freshness tests' `newReaperRig`, `reaperRig.seed`, `reaperRig.events`, `reapKey`, `reapNow`, `Checker.lockLoadedHook`.
- Produces: unexported `suiteForm{scheme, host, suite string}`, `parseSuiteForm(w, r) (suiteForm, bool)`, `(*Reaper).notForgotten(key cache.SuiteKey, reason string)`; the DEBUG event `suite_not_forgotten`; `canonical_scheme` on `admin_forget_requested`, `admin_forget_failed` and (beside `scheme`) `admin_reconcile_requested`.

- [ ] **Step 1: Write the failing tests**

Create `internal/admin/endpoint_form_test.go`:

```go
package admin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// postAdminBody sends POST url with body as a form and X-ACU-Admin: 1,
// as a script does, and returns the status and the response body.
func postAdminBody(t *testing.T, url, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-ACU-Admin", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	text, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(text)
}

// Deferred minor F-5, Review Focus 3: a body that is not a valid form (a
// stray "%", as from a script that forgets --data-urlencode) is 400
// "malformed form body" from both mutating endpoints, never 413; only a
// body over 8 KiB is 413. Either way the endpoint's work never runs, and
// the body ends in one newline (F-6).
func TestMutatingEndpoints_MalformedBodyIs400(t *testing.T) {
	for _, ep := range guardedEndpoints {
		t.Run(ep.path, func(t *testing.T) {
			base, calls, cleanup := ep.start(t)
			defer cleanup()
			if status, body := postAdminBody(t, base+ep.path, "host=h&suite=/a%zz"); status != http.StatusBadRequest ||
				body != "malformed form body\n" {
				t.Fatalf("a malformed body: %d %q, want 400 \"malformed form body\\n\"", status, body)
			}
			if status, body := postAdminBody(t, base+ep.path, "host=h&suite=/s&pad="+strings.Repeat("x", 9<<10)); status != http.StatusRequestEntityTooLarge ||
				body != "request body too large\n" {
				t.Fatalf("a body over 8 KiB: %d %q, want 413 \"request body too large\\n\"", status, body)
			}
			if n := calls(); n != 0 {
				t.Fatalf("the endpoint ran %d times, want never", n)
			}
		})
	}
}

// Deferred minor F-6: every error body of the mutating endpoints ends in
// exactly one newline (http.Error adds it; the messages carry none) — the
// guard's 403, a missing parameter's 400, a refusal's 409 and a 501 — and
// OPTIONS /reconcile names POST, as OPTIONS /forget does.
func TestMutatingEndpoints_BodiesEndInOneNewlineAndOptionsNamePost(t *testing.T) {
	checkBody := func(what, body string) {
		t.Helper()
		if !strings.HasSuffix(body, "\n") || strings.HasSuffix(body, "\n\n") {
			t.Errorf("%s: body %q, want exactly one trailing newline", what, body)
		}
	}
	_, base, cleanup := startAdminServer(t, withReconciler(&stubReconciler{result: false}),
		withForgetter(&stubForgetter{})) // a zero result: not deleted, 409
	defer cleanup()
	for _, path := range []string{"/reconcile", "/forget"} {
		_, body := postForm(t, base+path, nil) // no X-ACU-Admin: the guard's 403
		checkBody(path+" 403", body)
		status, body := postAdminBody(t, base+path, "host=h")
		if status != http.StatusBadRequest {
			t.Fatalf("%s without suite: %d, want 400", path, status)
		}
		checkBody(path+" 400", body)
		status, body = postAdminBody(t, base+path, "host=h&suite=/s")
		if status != http.StatusConflict {
			t.Fatalf("%s refused: %d, want 409", path, status)
		}
		checkBody(path+" 409", body)
		req, err := http.NewRequest(http.MethodOptions, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Allow") != "POST, OPTIONS" {
			t.Fatalf("OPTIONS %s: %d, Allow %q; want 204 and \"POST, OPTIONS\"", path, resp.StatusCode, resp.Header.Get("Allow"))
		}
	}
	_, unwired, cleanup2 := startAdminServer(t)
	defer cleanup2()
	for _, path := range []string{"/reconcile", "/forget"} {
		status, body := postAdminBody(t, unwired+path, "host=h&suite=/s")
		if status != http.StatusNotImplemented {
			t.Fatalf("%s unwired: %d, want 501", path, status)
		}
		checkBody(path+" 501", body)
	}
}

// Deferred minor F-16 (design note 4): admin_reconcile_requested keeps
// scheme, which it had on main, and gains canonical_scheme beside it, so
// a filter on either finds it.
func TestEndpoint_Reconcile_LogsBothSchemeFields(t *testing.T) {
	var buf lockedBuffer
	stub := &stubReconciler{result: true}
	_, base, cleanup := startAdminServer(t, withReconciler(stub), withLogger(slog.New(slog.NewJSONHandler(&buf, nil))))
	defer cleanup()
	if status, _ := postAdminBody(t, base+"/reconcile", "scheme=http&host=h&suite=/s"); status != http.StatusAccepted {
		t.Fatalf("status %d, want 202", status)
	}
	var found bool
	for _, line := range strings.Split(buf.String(), "\n") {
		var m map[string]any
		if line == "" || json.Unmarshal([]byte(line), &m) != nil || m["msg"] != "admin_reconcile_requested" {
			continue
		}
		found = true
		if m["scheme"] != "http" || m["canonical_scheme"] != "http" || m["canonical_host"] != "h" || m["suite_path"] != "/s" {
			t.Fatalf("admin_reconcile_requested %v, want scheme and canonical_scheme http", m)
		}
	}
	if !found {
		t.Fatal("no admin_reconcile_requested line")
	}
}

// Deferred minor F-4: one parser for both endpoints — host, suite and
// scheme trimmed, an empty scheme https.
func TestParseSuiteForm(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/forget", strings.NewReader("host=+h+&suite=%20/s%20&scheme="))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	f, ok := parseSuiteForm(httptest.NewRecorder(), r)
	if !ok || f != (suiteForm{scheme: "https", host: "h", suite: "/s"}) {
		t.Fatalf("parseSuiteForm = %+v, %v; want https h /s", f, ok)
	}
}
```

Create `internal/freshness/forget_refusal_test.go`:

```go
package freshness

import (
	"context"
	"testing"
)

// Deferred minor F-15 (design note 8): a forget that deletes nothing —
// answered 409 alike, with no existence oracle (spec C9) — says why at
// DEBUG (suite_not_forgotten): no_suite for a suite without a row, busy
// for one held, no_suite again for one gone under the hold.
func TestReaper_ForgetSaysWhyItForgotNothing(t *testing.T) {
	ctx := context.Background()
	r := newReaperRig(t)
	if res, err := r.reaper.Forget(ctx, "http", "h", "/r/dists/none"); res.Deleted || err != nil {
		t.Fatalf("unknown: %+v, %v", res, err)
	}
	r.seed("/r/dists/busy", reapNow)
	release, held := r.checker.HoldSuite(reapKey("/r/dists/busy"))
	if !held {
		t.Fatal("premise: the suite is held")
	}
	if res, err := r.reaper.Forget(ctx, "http", "h", "/r/dists/busy"); res.Deleted || err != nil {
		t.Fatalf("busy: %+v, %v", res, err)
	}
	release(false)
	r.seed("/r/dists/gone", reapNow)
	r.checker.lockLoadedHook = func(string) {
		r.checker.lockLoadedHook = nil
		if res, err := r.cache.ForgetSuite(ctx, reapKey("/r/dists/gone"), nil); err != nil || !res.Deleted {
			t.Errorf("ForgetSuite = %+v, %v", res, err)
		}
	}
	if res, err := r.reaper.Forget(ctx, "http", "h", "/r/dists/gone"); res.Deleted || err != nil {
		t.Fatalf("gone under the hold: %+v, %v", res, err)
	}
	want := map[string]string{"/r/dists/none": "no_suite", "/r/dists/busy": "busy", "/r/dists/gone": "no_suite"}
	lines := r.events("suite_not_forgotten")
	for _, m := range lines {
		if m["level"] != "DEBUG" || m["canonical_scheme"] != "http" || m["canonical_host"] != "h" ||
			m["reason"] != want[m["suite_path"].(string)] {
			t.Errorf("line %v, want DEBUG with the reason %q", m, want[m["suite_path"].(string)])
		}
	}
	if len(lines) != 3 {
		t.Fatalf("%d suite_not_forgotten lines, want 3", len(lines))
	}
	if _, err := r.reaper.Forget(ctx, "http", "h", "/r/dists/none"); err != nil || len(r.events("suite_forgotten")) != 0 {
		t.Fatalf("a forget that deleted nothing: err %v, or it logged suite_forgotten", err)
	}
}
```

In `internal/admin/forget_test.go` (`TestEndpoint_Forget_LogsAcceptedRequestsAndFailures`), the two assertions name the new key (an existing test that changes, F-16):

old:
```go
			if len(req) != 1 || req[0]["level"] != "INFO" || req[0]["scheme"] != scheme || req[0]["canonical_host"] != "h" ||
				req[0]["suite_path"] != "/s" {
```
new:
```go
			// Deferred minor F-16: canonical_scheme, as on every other line
			// about a suite, and no scheme key.
			if len(req) != 1 || req[0]["level"] != "INFO" || req[0]["canonical_scheme"] != scheme || req[0]["scheme"] != nil ||
				req[0]["canonical_host"] != "h" || req[0]["suite_path"] != "/s" {
```

old:
```go
			if len(failed) != 1 || failed[0]["level"] != "WARN" || failed[0]["err"] != "disk I/O error" ||
				failed[0]["canonical_host"] != "h" || failed[0]["suite_path"] != "/s" {
```
new:
```go
			if len(failed) != 1 || failed[0]["level"] != "WARN" || failed[0]["err"] != "disk I/O error" ||
				failed[0]["canonical_scheme"] != scheme || failed[0]["canonical_host"] != "h" || failed[0]["suite_path"] != "/s" {
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -gcflags=-e -count=1 -run 'TestMutatingEndpoints|TestEndpoint_Reconcile_LogsBothSchemeFields|TestParseSuiteForm' ./internal/admin/`
Expected: build failure naming `undefined: parseSuiteForm` and `undefined: suiteForm` (endpoint_form_test.go:143, :144).

Run: `go test -count=1 -run TestReaper_ForgetSaysWhy ./internal/freshness/`
Expected: FAIL — `forget_refusal_test.go:46: 0 suite_not_forgotten lines, want 3`.

- [ ] **Step 3: One parser, one newline (`internal/admin/handlers.go`)**

Before `forgottenText`, add:

```go
// suiteForm is the form both mutating endpoints take.
type suiteForm struct {
	scheme, host, suite string
}

// parseSuiteForm reads the form body of POST /reconcile and POST /forget
// (deferred minor F-4: one parser for both): at most 8 KiB — more is 413
// "request body too large" — and a body ParseForm cannot parse (a stray
// "%", say) is 400 "malformed form body" (deferred minor F-5); host,
// suite and scheme are trimmed, and an empty scheme is https. When it
// reports false it has answered.
//
// AIDEV-NOTE: http.Error appends the newline: messages carry none, so
// every error body ends in exactly one (deferred minor F-6). Call it only
// after requireMutationGuard (review M-10 of the H1 plan): it reads and
// limits the body, and a 400 or 413 before the guard would answer an
// unauthenticated cross-origin request with more than the guard's 403.
func parseSuiteForm(w http.ResponseWriter, r *http.Request) (suiteForm, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "malformed form body", http.StatusBadRequest)
		}
		return suiteForm{}, false
	}
	f := suiteForm{scheme: strings.TrimSpace(r.FormValue("scheme")), host: strings.TrimSpace(r.FormValue("host")),
		suite: strings.TrimSpace(r.FormValue("suite"))}
	if f.scheme == "" {
		f.scheme = "https"
	}
	return f, true
}
```

`handleReconcile`'s body from its nil check to its 409:

old:
```go
	if s.cfg.Reconciler == nil {
		http.Error(w, "reconcile not available\n", http.StatusNotImplemented)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "request body too large\n", http.StatusRequestEntityTooLarge)
		return
	}
	host := strings.TrimSpace(r.FormValue("host"))
	suite := strings.TrimSpace(r.FormValue("suite"))
	scheme := strings.TrimSpace(r.FormValue("scheme"))
	if scheme == "" {
		scheme = "https"
	}
	if host == "" || suite == "" {
		http.Error(w, "host and suite are required\n", http.StatusBadRequest)
		return
	}
	s.logger.Info("admin_reconcile_requested", "scheme", scheme, "canonical_host", host, "suite_path", suite)
	if !s.cfg.Reconciler.Reconcile(r.Context(), scheme, host, suite) {
		http.Error(w, "reconcile not triggered (busy, unknown, no current snapshot, or no free adoption slot)\n", http.StatusConflict)
		return
	}
```
new:
```go
	if s.cfg.Reconciler == nil {
		http.Error(w, "reconcile not available", http.StatusNotImplemented)
		return
	}
	f, ok := parseSuiteForm(w, r)
	if !ok {
		return
	}
	if f.host == "" || f.suite == "" {
		http.Error(w, "host and suite are required", http.StatusBadRequest)
		return
	}
	// scheme stays beside canonical_scheme: this line is on main with
	// scheme, and log filters may use it (deferred minor F-16).
	s.logger.Info("admin_reconcile_requested", "scheme", f.scheme, "canonical_scheme", f.scheme, "canonical_host", f.host,
		"suite_path", f.suite)
	if !s.cfg.Reconciler.Reconcile(r.Context(), f.scheme, f.host, f.suite) {
		http.Error(w, "reconcile not triggered (busy, unknown, no current snapshot, or no free adoption slot)", http.StatusConflict)
		return
	}
```

and its doc's 400 line: `//   - 400 Bad Request: host or suite missing.` becomes `//   - 400 Bad Request: host or suite missing, or a body that is not a` + `//     valid form (deferred minor F-5).`.

`handleForget` from its nil check to `admin_forget_requested`:

old:
```go
	if s.cfg.Forgetter == nil {
		http.Error(w, "forget not available\n", http.StatusNotImplemented)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "request body too large\n", http.StatusRequestEntityTooLarge)
		return
	}
	host := strings.TrimSpace(r.FormValue("host"))
	suite := strings.TrimSpace(r.FormValue("suite"))
	scheme := strings.TrimSpace(r.FormValue("scheme"))
	if scheme == "" {
		scheme = "https"
	}
	if host == "" || suite == "" || (scheme != "http" && scheme != "https") {
		http.Error(w, "host and suite are required; scheme is http or https\n", http.StatusBadRequest)
		return
	}
	if strings.Contains(host, "://") {
		// The status page's Host cell shows scheme://host; copied whole
		// into host it matches no row (the Task 10 review's minor 4).
		http.Error(w, "host is the host name alone: pass the scheme in scheme (http or https) and the host without it\n",
			http.StatusBadRequest)
		return
	}
	s.logger.Info("admin_forget_requested", "scheme", scheme, "canonical_host", host, "suite_path", suite)
```
new:
```go
	if s.cfg.Forgetter == nil {
		http.Error(w, "forget not available", http.StatusNotImplemented)
		return
	}
	f, ok := parseSuiteForm(w, r)
	if !ok {
		return
	}
	scheme, host, suite := f.scheme, f.host, f.suite
	if host == "" || suite == "" || (scheme != "http" && scheme != "https") {
		http.Error(w, "host and suite are required; scheme is http or https", http.StatusBadRequest)
		return
	}
	if strings.Contains(host, "://") {
		// The status page's Host cell shows scheme://host; copied whole
		// into host it matches no row (the Task 10 review's minor 4).
		http.Error(w, "host is the host name alone: pass the scheme in scheme (http or https) and the host without it",
			http.StatusBadRequest)
		return
	}
	s.logger.Info("admin_forget_requested", "canonical_scheme", scheme, "canonical_host", host, "suite_path", suite)
```

and the rest of the file's error bodies and the two failure lines:

| old | new |
|---|---|
| `s.logger.Debug("admin_forget_failed", "err", err, "scheme", scheme, …)` | `s.logger.Debug("admin_forget_failed", "err", err, "canonical_scheme", scheme, …)` |
| `s.logger.Warn("admin_forget_failed", "err", err, "scheme", scheme, …)` | `s.logger.Warn("admin_forget_failed", "err", err, "canonical_scheme", scheme, …)` |
| `"shutting down; retry once the daemon is back\n"` | `"shutting down; retry once the daemon is back"` |
| `"forget failed\n"` | `"forget failed"` |
| `"column shows it)\n", http.StatusConflict)` | `"column shows it)", http.StatusConflict)` |
| `"this endpoint requires the header X-ACU-Admin: 1\n"` | `"this endpoint requires the header X-ACU-Admin: 1"` |
| `"requests from another origin are refused: Sec-Fetch-Site must be same-origin or none\n",` | `"requests from another origin are refused: Sec-Fetch-Site must be same-origin or none",` |
| `"or set admin.htpasswd_file\n", http.StatusForbidden)` | `"or set admin.htpasswd_file", http.StatusForbidden)` |

`handleForget`'s doc, the 400 entry: `//   - 400 Bad Request: host or suite missing, another scheme, or a host` … `it: the scheme goes in scheme).` becomes `//   - 400 Bad Request: host or suite missing, another scheme, a host` / `//     holding a scheme (scheme://host, as the status page's Host cell shows` / `//     it: the scheme goes in scheme), or a body that is not a valid form` / `//     (deferred minor F-5).`. After the edits `grep -n 'http.Error(w, "[^"]*\\n"' internal/admin/*.go` finds no production line.

- [ ] **Step 4: `OPTIONS /reconcile` (`internal/admin/server.go`)**

old:
```go
			} else if r.URL.Path == "/forget" {
				w.Header().Set("Allow", "POST, OPTIONS")
```
new:
```go
			} else if r.URL.Path == "/forget" || r.URL.Path == "/reconcile" {
				// The mutating endpoints (deferred minor F-6: /reconcile
				// answered the read-only set).
				w.Header().Set("Allow", "POST, OPTIONS")
```

- [ ] **Step 5: Why nothing was forgotten (`internal/freshness/forget.go`)**

In `Forget`, each early return that forgets nothing logs its reason first:

| old | new |
|---|---|
| `case errors.Is(err, cache.ErrNotFound):` / `return cache.ForgetResult{}, nil` (the pre-hold read) | the same, with `r.notForgotten(key, "no_suite")` before the return |
| `if !ok {` / `return cache.ForgetResult{}, nil` (after `HoldSuite`) | the same, with `r.notForgotten(key, "busy")` before the return |
| `gone = true` / `return cache.ForgetResult{}, nil` (the re-read under the hold) | the same, with `r.notForgotten(key, "no_suite")` before the return |

and after `ForgetSuite`:

old:
```go
	if !res.Deleted {
		return res, nil
	}
```
new:
```go
	if !res.Deleted {
		reason := "changed"
		if res.Gone {
			reason = "no_suite"
		}
		r.notForgotten(key, reason)
		return res, nil
	}
```

At the end of the file:

```go
// notForgotten logs DEBUG suite_not_forgotten with why Forget deleted
// nothing (deferred minor F-15): no_suite (no row, before the hold or
// under it), busy (the hold was refused) or changed (the final
// compare-and-delete found the row changed: no writer of the current
// snapshot runs under the hold, so this is the defensive case). POST
// /forget answers 409 for all of them alike — no existence oracle (spec
// C9) — and an operator who turns DEBUG on can tell them apart here.
func (r *Reaper) notForgotten(key cache.SuiteKey, reason string) {
	r.logger.Debug("suite_not_forgotten", "canonical_scheme", key.CanonicalScheme, "canonical_host", key.CanonicalHost,
		"suite_path", key.SuitePath, "reason", reason)
}
```

`changed` has no test: nothing writes a suite's current snapshot while it is held, so no seam reaches the branch; it is logged for completeness.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test -count=1 ./internal/admin/ && go test -count=1 -run 'TestReaper_Forget' ./internal/freshness/`
Expected: two `ok` lines.

Run: `go test -race -count=1 ./internal/admin/ ./internal/freshness/ ./cmd/...`
Expected: all `ok`.

- [ ] **Step 7: Mutation checks** (one at a time, each reverted)

| Mutant | Fails |
|---|---|
| `if errors.As(err, &tooLarge) || true {` (every parse error 413) | `endpoint_form_test.go:47: a malformed body: 413 "request body too large\n", want 400 "malformed form body\n"` (both endpoints) |
| `OPTIONS`: `r.URL.Path == "/forget" {` alone | `endpoint_form_test.go:97: OPTIONS /reconcile: 204, Allow "GET, HEAD, OPTIONS"; want 204 and "POST, OPTIONS"` |
| `"forget not available\n"` restored | `endpoint_form_test.go:107: /forget 501: body "forget not available\n\n", want exactly one trailing newline` |
| `admin_reconcile_requested` without `canonical_scheme` | `endpoint_form_test.go:130: admin_reconcile_requested map[… scheme:http …], want scheme and canonical_scheme http` |
| `r.notForgotten(key, "no_suite")` for the busy return | `forget_refusal_test.go:42: line map[… reason:no_suite suite_path:/r/dists/busy …], want DEBUG with the reason "busy"` |

- [ ] **Step 8: Commit**

```bash
gofmt -l internal/admin/handlers.go internal/admin/server.go internal/admin/forget_test.go internal/admin/endpoint_form_test.go internal/freshness/forget.go internal/freshness/forget_refusal_test.go
git add internal/admin/handlers.go internal/admin/server.go internal/admin/forget_test.go internal/admin/endpoint_form_test.go internal/freshness/forget.go internal/freshness/forget_refusal_test.go
git commit -m "$(cat <<'EOF'
Admin: one form parser for /reconcile and /forget, 400 for a bad body

Both mutating endpoints read their form through parseSuiteForm (F-4),
which answers a malformed body 400 and only an oversized one 413 (F-5);
OPTIONS /reconcile names POST and every error body ends in one newline
(F-6); the forget lines name the scheme canonical_scheme, and
admin_reconcile_requested, on main with scheme, gains it beside that
(F-16); a forget that deletes nothing logs suite_not_forgotten at DEBUG
with no_suite, busy or changed (F-15). The HTTP answers still tell busy
from unknown apart nowhere.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

### Task 6: The reaper's run signal, its evidence fields, the forward step counted, the hold's unlock (F-7, F-13, F-17, F-18, F-25, F-26)

Phase F's operability items for the reaper and two robustness ones: a completed run logs `suite_reaper_run_complete` and sets `acu_suite_reaper_last_run_unixtime` (F-13, design note 7); the forward-step report is counted (F-17, design note 10); `suite_reaped` names the stamp an idle reap judged and the rows a junk reap judged (F-18, design note 9); a panicking purge never leaks a suite's mutex (F-25); `sortCandidates`' AIDEV-NOTE is made exact (F-26); and two comments that record only history lose it (F-7). `docs/log-fields.md` follows in Task 18.

**Files:**
- Create: `internal/freshness/reaper_signal_test.go`
- Modify: `internal/freshness/reaper.go` (`runOnce`, `sweepMarkers`, `deferredByForwardStep`, `reapCandidate.evidence`, `reapSuite`'s line, `junkCandidates`, `suiteJunk`, new `rowPaths`, `sortCandidates`' AIDEV-NOTE), `metrics.go` (`reaperLastRunUnixtime`, the counter's HELP, the inventory), `suite_lock.go` (`HoldSuite`'s release), `freshness.go` (`Checker.holdPurgeHook`), `clockstep.go`, `unsigned.go` (F-7)

**Interfaces:**
- Consumes: freshness tests' `newReaperRig`, `reaperRig.seed`, `mark`, `junkSuite`, `cacheAnchor`, `events`, `exists`, `reapNow`, `reapDay`, `softPage`, `renderedMetric`; the event `suite_reaper_marker_sweep_failed` (Task 3).
- Produces: the event `suite_reaper_run_complete`; the gauge `acu_suite_reaper_last_run_unixtime`; `suite_reaped`'s `last_requested_unixtime` and `evidence`; `(*Reaper).sweepMarkers(ctx, now, fresh time.Time) int` (it returned nothing); `(*Reaper).suiteJunk(…) (bool, []string, error)`; unexported `rowPaths(junk bool, sets ...[]cache.SuiteAnchorRow) []string`; the tests-only `Checker.holdPurgeHook func()`; test helper `gaugeValue(t, name) float64`.

- [ ] **Step 1: Write the failing tests**

Create `internal/freshness/reaper_signal_test.go`:

```go
package freshness

import (
	"context"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// gaugeValue returns an unlabelled metric's value as metrics.Default
// renders it, failing the test when the series is missing or unparsable.
func gaugeValue(t *testing.T, name string) float64 {
	t.Helper()
	v, err := strconv.ParseFloat(renderedMetric(name), 64)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return v
}

// Deferred minor F-13 (design note 7): a completed reaper run logs
// suite_reaper_run_complete — the candidates it tried, the suites it
// reaped, the markers its daily sweep swept, whether the forward-step
// guard held it back — and sets acu_suite_reaper_last_run_unixtime; a run
// whose listing failed does neither. Deferred minor F-17 (design note 10):
// the forward step that holds a run back is counted in
// acu_clock_steps_detected_total like every clock-step report.
func TestReaper_ACompletedRunLogsItsLineAndSetsTheGauge(t *testing.T) {
	r := newReaperRig(t)
	ctx := context.Background()
	r.seed("/r/dists/old", reapNow-32*reapDay)
	r.seed("/r/dists/fresh", reapNow)
	r.mark("/gone/dists/s", reapNow-366*reapDay) // a year-old orphan marker
	reaperLastRunUnixtime.Set(1)
	r.reaper.runOnce(ctx) // outside Run the daily sweep is due
	runs := r.events("suite_reaper_run_complete")
	if len(runs) != 1 || runs[0]["level"] != "INFO" || runs[0]["tried"] != float64(1) || runs[0]["reaped"] != float64(1) ||
		runs[0]["markers_swept"] != float64(1) || runs[0]["deferred"] != false || runs[0]["duration_ms"] == nil {
		t.Fatalf("run lines %v, want one: tried 1, reaped 1, markers_swept 1, not deferred", runs)
	}
	if got := gaugeValue(t, "acu_suite_reaper_last_run_unixtime"); got != float64(reapNow) {
		t.Fatalf("last run gauge %v, want %d", got, reapNow)
	}

	reaperLastRunUnixtime.Set(1)
	ended, cancel := context.WithCancel(ctx)
	cancel()
	r.reaper.runOnce(ended) // the listing fails: not a completed run
	if n := len(r.events("suite_reaper_run_complete")); n != 1 {
		t.Fatalf("%d run lines after a failed listing, want still 1", n)
	}
	if got := gaugeValue(t, "acu_suite_reaper_last_run_unixtime"); got != 1 {
		t.Fatalf("last run gauge %v after a failed listing, want it untouched", got)
	}

	steps := func() float64 { // a counter has no series before its first count
		v, _ := strconv.ParseFloat(renderedMetric("acu_clock_steps_detected_total"), 64)
		return v
	}
	steps0 := steps()
	r.reaper.lastWall, r.reaper.lastMono = time.Unix(reapNow, 0).Add(-3*time.Hour), time.Now() // 3 h ahead
	r.seed("/r/dists/old2", reapNow-40*reapDay)
	r.reaper.runOnce(ctx)
	runs = r.events("suite_reaper_run_complete")
	if len(runs) != 2 || runs[1]["deferred"] != true || runs[1]["tried"] != float64(0) || !r.exists("/r/dists/old2") {
		t.Fatalf("run lines %v, old2 kept %v; want a deferred run that tried nothing", runs, r.exists("/r/dists/old2"))
	}
	if d := steps() - steps0; d != 1 {
		t.Fatalf("acu_clock_steps_detected_total grew by %v at the forward step, want 1", d)
	}
}

// Deferred minor F-18 (design note 9): suite_reaped names what judged it —
// for an idle suite the last-request stamp the cutoff judged, for a junk
// suite the cached rows it judged junk.
func TestReaper_SuiteReapedNamesItsEvidence(t *testing.T) {
	r := newReaperRig(t)
	idleAt := reapNow - 32*reapDay
	r.seed("/r/dists/idle", idleAt)
	r.junkSuite("/r/dists/junk")
	r.cacheAnchor("/r/dists/junk", "InRelease", softPage, reapNow-2*reapDay)
	r.cacheAnchor("/r/dists/junk", "Release", softPage, reapNow-2*reapDay)
	r.reaper.runOnce(context.Background())
	got := map[string]map[string]any{}
	for _, m := range r.events("suite_reaped") {
		got[m["suite_path"].(string)] = m
	}
	if m := got["/r/dists/idle"]; m == nil || m["reason"] != "idle" || m["last_requested_unixtime"] != float64(idleAt) ||
		m["evidence"] != nil {
		t.Fatalf("idle line %v, want last_requested_unixtime %d and no evidence", m, idleAt)
	}
	if m := got["/r/dists/junk"]; m == nil || m["reason"] != "junk" ||
		!reflect.DeepEqual(m["evidence"], []any{"/r/dists/junk/InRelease", "/r/dists/junk/Release"}) || m["last_requested_unixtime"] != nil {
		t.Fatalf("junk line %v, want the two anchors as evidence", m)
	}
}

// Deferred minor F-25: a panic in the flip-gate purge of a deleted suite's
// release still drops the lock-map entry and unlocks the suite — the
// unlock is deferred — so no later deleter finds the mutex leaked; the
// panic goes on to the caller.
func TestHoldSuite_APanickingPurgeStillUnlocks(t *testing.T) {
	r := newReaperRig(t)
	key := cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}
	release, ok := r.checker.HoldSuite(key)
	if !ok {
		t.Fatal("premise: the suite is held")
	}
	r.checker.holdPurgeHook = func() { panic("purge bug") }
	var got any
	func() {
		defer func() { got = recover() }()
		release(true)
	}()
	r.checker.holdPurgeHook = nil
	if got != "purge bug" {
		t.Fatalf("recovered %v, want the purge's panic", got)
	}
	again, ok := r.checker.HoldSuite(key)
	if !ok {
		t.Fatal("the suite's mutex leaked: a later hold was refused")
	}
	again(false)
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -gcflags=-e -count=1 -run 'TestReaper_ACompletedRun|TestReaper_SuiteReapedNames|TestHoldSuite_APanicking' ./internal/freshness/`
Expected: build failure naming `undefined: reaperLastRunUnixtime` (reaper_signal_test.go:36, :47) and `r.checker.holdPurgeHook undefined (type *Checker has no field or method holdPurgeHook)` (:111, :117).

- [ ] **Step 3: The run's line and gauge, the forward step counted (`internal/freshness/reaper.go`, `metrics.go`)**

Replace `runOnce` (its doc gains a paragraph):

old:
```go
// M-8); a detected step no longer ages markers at all (stepAhead,
// sweepMarkers).
func (r *Reaper) runOnce(ctx context.Context) {
	rows, err := r.cache.ListSuitesWithAdoption(ctx)
	if err != nil {
		r.warn(ctx, "suite_reaper_list_suites_failed", "err", err)
		return
	}
	now := r.checker.now()
	deferred := r.deferredByForwardStep(now, time.Now())
	if !deferred && r.idleTTL > 0 {
		cands := r.candidates(rows, now)
		cands = append(cands, r.junkCandidates(ctx, rows, cands, now)...)
		sortCandidates(cands)
		for _, c := range runBudgets(cands) {
			if ctx.Err() != nil {
				return
			}
			r.reapSuite(ctx, c)
		}
	}
	if !deferred && (r.dailyAt.IsZero() || time.Since(r.dailyAt) >= dailyEvery) {
		r.dailyAt = time.Now()
		r.sweepMarkers(ctx, now, r.checker.now())
	}
	r.sweepReconcileMemo(rows)
}
```
new:
```go
// M-8); a detected step no longer ages markers at all (stepAhead,
// sweepMarkers).
//
// AIDEV-NOTE: deferred minor F-13 (design note 7 of the H1 plan). A run
// that completes — the listing read, the reaps tried, the sweeps done, or
// held back by the forward-step guard — logs suite_reaper_run_complete
// and sets acu_suite_reaper_last_run_unixtime; one whose listing failed or
// that a shutdown cut short does neither, so an alert on the gauge's age
// catches a reaper that stopped completing runs (GC's
// acu_gc_last_run_unixtime is the model).
func (r *Reaper) runOnce(ctx context.Context) {
	start := time.Now()
	rows, err := r.cache.ListSuitesWithAdoption(ctx)
	if err != nil {
		r.warn(ctx, "suite_reaper_list_suites_failed", "err", err)
		return
	}
	now := r.checker.now()
	deferred := r.deferredByForwardStep(now, time.Now())
	tried, reaped, swept := 0, 0, 0
	if !deferred && r.idleTTL > 0 {
		cands := r.candidates(rows, now)
		cands = append(cands, r.junkCandidates(ctx, rows, cands, now)...)
		sortCandidates(cands)
		for _, c := range runBudgets(cands) {
			if ctx.Err() != nil {
				return
			}
			tried++
			if r.reapSuite(ctx, c) {
				reaped++
			}
		}
	}
	if !deferred && (r.dailyAt.IsZero() || time.Since(r.dailyAt) >= dailyEvery) {
		r.dailyAt = time.Now()
		swept = r.sweepMarkers(ctx, now, r.checker.now())
	}
	r.sweepReconcileMemo(rows)
	if ctx.Err() != nil {
		return // cut short by a shutdown: not a completed run
	}
	reaperLastRunUnixtime.Set(float64(r.checker.now().Unix()))
	r.logger.Info("suite_reaper_run_complete", "tried", tried, "reaped", reaped, "markers_swept", swept,
		"deferred", deferred, "duration_ms", time.Since(start).Milliseconds())
}
```

`sweepMarkers` reports what it swept: its signature becomes `func (r *Reaper) sweepMarkers(ctx context.Context, now, fresh time.Time) int`, and after its last `if err != nil { r.warn(…) }` block it returns `res.Swept`.

In `deferredByForwardStep`, the report is counted:

old:
```go
		if ahead > forwardStepSlack {
			r.logger.Warn("clock_step_detected", "stamp", "wall_clock",
```
new:
```go
		if ahead > forwardStepSlack {
			clockStepsTotal.Inc() // counted like every report (deferred minor F-17)
			r.logger.Warn("clock_step_detected", "stamp", "wall_clock",
```

and its doc's `clock_step_detected (stamp=wall_clock), defers reaping by the uptime` becomes `clock_step_detected (stamp=wall_clock) and counts it in` / `acu_clock_steps_detected_total (deferred minor F-17), defers reaping by the uptime`.

`internal/freshness/metrics.go`:

old:
```go
	clockStepsTotal = metrics.NewCounter(
		"acu_clock_steps_detected_total",
		"Past-event timestamps found in the future (backward wall-clock step).",
	)
```
new:
```go
	clockStepsTotal = metrics.NewCounter(
		"acu_clock_steps_detected_total",
		"Wall-clock steps detected: a past-event timestamp found in the future (a backward step), "+
			"or the wall clock run ahead of the monotonic clock between reaper runs (a forward step).",
	)

	// reaperLastRunUnixtime is when the suite reaper last completed a run
	// (deferred minor F-13): an alert on its age catches a reaper that
	// stopped, as acu_gc_last_run_unixtime does for GC.
	reaperLastRunUnixtime = metrics.NewGauge(
		"acu_suite_reaper_last_run_unixtime",
		"Unix-seconds timestamp of the suite reaper's last completed run (spec C9).",
	)
```

and the inventory AIDEV-NOTE's `acu_anchorless_sniff_rejected_total (spec C4 Dispatch 4, counter by` / `host). The flip barrier's` becomes `… counter by` / `host); acu_suite_reaper_last_run_unixtime (spec C9, deferred minor` / `F-13, unlabelled gauge). The flip barrier's`.

- [ ] **Step 4: `suite_reaped`'s evidence and stamp (`internal/freshness/reaper.go`)**

`reapCandidate` gains, after `cutoff`:

```go
	// evidence is a junk candidate's judged rows' paths, in the order
	// judged: suite_reaped's evidence (deferred minor F-18).
	evidence []string
```

In `reapSuite`:

old:
```go
	if settled {
		attrs = append(attrs, "settled", true)
	}
	r.logger.Info("suite_reaped", attrs...)
```
new:
```go
	switch {
	case c.reason == "idle" && c.requested != nil:
		attrs = append(attrs, "last_requested_unixtime", *c.requested) // the stamp the cutoff judged (F-18)
	case c.reason == "junk":
		attrs = append(attrs, "evidence", c.evidence) // the rows judged junk (F-18): a list, since a path may hold a space
	}
	if settled {
		attrs = append(attrs, "settled", true)
	}
	r.logger.Info("suite_reaped", attrs...)
```

In `junkCandidates`, `junk, err := r.suiteJunk(…)` becomes `junk, evidence, err := r.suiteJunk(…)`, and the candidate it appends gains `evidence: evidence` (after `cutoff: cache.ReapRegardless`). `suiteJunk` returns the paths it judged:

old:
```go
func (r *Reaper) suiteJunk(ctx context.Context, key cache.SuiteKey, flat bool, anchors, indexes []cache.SuiteAnchorRow,
	cutoff int64, next map[sniffKey]bool, fails *junkReadFailures) (bool, error) {
	anchorsJunk := len(anchors) > 0 && r.allJunk(key, anchors, cutoff, next, fails)
	if !flat {
		return anchorsJunk, nil
	}
	combined := (len(anchors) == 0 || anchorsJunk) && len(anchors)+len(indexes) > 0 &&
		(len(indexes) == 0 || r.allJunk(key, indexes, cutoff, next, fails))
	if combined == anchorsJunk {
		return anchorsJunk, nil
	}
	switch err := r.signedEvidence(ctx, key.CanonicalHost, key.SuitePath); {
	case errors.Is(err, cache.ErrUnsignedDowngradeRefused):
		return anchorsJunk, nil // signed evidence: the anchors alone
	case err != nil:
		return false, err
	}
	return combined, nil
}
```
new:
```go
func (r *Reaper) suiteJunk(ctx context.Context, key cache.SuiteKey, flat bool, anchors, indexes []cache.SuiteAnchorRow,
	cutoff int64, next map[sniffKey]bool, fails *junkReadFailures) (bool, []string, error) {
	anchorsJunk := len(anchors) > 0 && r.allJunk(key, anchors, cutoff, next, fails)
	if !flat {
		return anchorsJunk, rowPaths(anchorsJunk, anchors), nil
	}
	combined := (len(anchors) == 0 || anchorsJunk) && len(anchors)+len(indexes) > 0 &&
		(len(indexes) == 0 || r.allJunk(key, indexes, cutoff, next, fails))
	if combined == anchorsJunk {
		return anchorsJunk, rowPaths(anchorsJunk, anchors, indexes), nil
	}
	switch err := r.signedEvidence(ctx, key.CanonicalHost, key.SuitePath); {
	case errors.Is(err, cache.ErrUnsignedDowngradeRefused):
		return anchorsJunk, rowPaths(anchorsJunk, anchors), nil // signed evidence: the anchors alone
	case err != nil:
		return false, nil, err
	}
	return combined, rowPaths(combined, anchors, indexes), nil
}

// rowPaths returns the paths of the rows of sets, in order, when junk —
// the evidence a junk verdict judged — and nil otherwise.
func rowPaths(junk bool, sets ...[]cache.SuiteAnchorRow) []string {
	if !junk {
		return nil
	}
	var out []string
	for _, rows := range sets {
		for _, a := range rows {
			out = append(out, a.Path)
		}
	}
	return out
}
```

and its doc gains, after `(ruling PG-7, amending R-G2)`: `, and, for a junk verdict, the paths of the` / `// rows it judged (suite_reaped's evidence: deferred minor F-18).`

- [ ] **Step 5: The hold's unlock, the AIDEV-NOTE, the history comments**

`internal/freshness/suite_lock.go` (`HoldSuite`):

old:
```go
			if !deleted {
				mu.Unlock()
				return
			}
			c.adopter.FlipGate().Purge(flipgate.Key(key.CanonicalScheme, key.CanonicalHost, key.SuitePath))
			c.unlockDropped(k, mu)
```
new:
```go
			if !deleted {
				mu.Unlock()
				return
			}
			// Deferred, so a panic in the purge still drops the entry and
			// unlocks: the mutex never leaks to every later deleter
			// (deferred minor F-25).
			defer c.unlockDropped(k, mu)
			if c.holdPurgeHook != nil {
				c.holdPurgeHook()
			}
			c.adopter.FlipGate().Purge(flipgate.Key(key.CanonicalScheme, key.CanonicalHost, key.SuitePath))
```

`internal/freshness/freshness.go` (`Checker`), after `unlockDroppedHook func(key string)`:

```go
	// holdPurgeHook, when set (tests only), runs in a HoldSuite release of
	// a deleted suite right before the flip-gate purge (deferred minor
	// F-25: a panic there must not leak the suite's mutex).
	holdPurgeHook func()
```

`internal/freshness/reaper.go` (`sortCandidates`' AIDEV-NOTE, F-26):

old:
```go
// budget (runBudgets): regardlessReapsPerRun of these, cheap reaps (no
// snapshot; a junk suite has at most three anchor rows, a non-canonical
// row deletes no metadata), and reapsPerRun idle suites in the same run.
```
new:
```go
// budget (runBudgets): regardlessReapsPerRun of these, cheap reaps for the
// reaper (a junk suite has no current snapshot and at most three anchor
// rows and fourteen root index rows; a non-canonical row deletes no
// metadata, though a legacy one may carry a current snapshot, which its
// reap retires — sub-job C deletes that snapshot's rows later, in chunks,
// so 250 such reaps a run grow C's backlog, not the reaper's own work:
// deferred minor F-26), and reapsPerRun idle suites in the same run.
```

`internal/freshness/clockstep.go` (F-7): drop the line `// Moved here from suite_lock.go beside clockStep (deferred minor D-25).` from `ReportClockStep`'s doc. `internal/freshness/unsigned.go` (F-7): `// checker's refusals before a slot is taken (unsignedRefusal, here since` / `// deferred minor E-33): the unsigned form adopts a Release no signature` becomes `// checker's refusals before a slot is taken (unsignedRefusal): the` / `// unsigned form adopts a Release no signature`.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test -count=1 ./internal/freshness/`
Expected: `ok` (replay: 42 s without `-race`).

Run: `go test -race -count=1 ./internal/freshness/`
Expected: `ok`. Simulations this task touches through the reaper (`TestDoD_NinetyDaysOfReapingWithRestarts`, `TestDoD_ProductionCadenceAcrossTwelveHourlyRestarts`): measured in Task 13's table (one more INFO line an hour costs them nothing measurable).

- [ ] **Step 7: Mutation checks** (one at a time, each reverted)

| Mutant | Fails |
|---|---|
| `reaperLastRunUnixtime.Set(…)` removed | `reaper_signal_test.go:44: last run gauge 1, want 1800000000` |
| `clockStepsTotal.Inc()` removed from `deferredByForwardStep` | `reaper_signal_test.go:71: acu_clock_steps_detected_total grew by 0 at the forward step, want 1` |
| The idle `last_requested_unixtime` attribute removed | `reaper_signal_test.go:92: idle line map[… reason:idle …], want last_requested_unixtime 1797235200 and no evidence` |
| `HoldSuite`'s release back to `Purge` then `unlockDropped`, no `defer` | `reaper_signal_test.go:123: the suite's mutex leaked: a later hold was refused` |
| `evidence` joined by spaces again (`strings.Join(c.evidence, " ")`; review M-7) | `reaper_signal_test.go:97: junk line map[… evidence:/r/dists/junk/InRelease /r/dists/junk/Release …], want the two anchors as evidence` |

- [ ] **Step 8: Commit**

```bash
gofmt -l internal/freshness/reaper.go internal/freshness/metrics.go internal/freshness/suite_lock.go internal/freshness/freshness.go internal/freshness/clockstep.go internal/freshness/unsigned.go internal/freshness/reaper_signal_test.go
git add internal/freshness/reaper.go internal/freshness/metrics.go internal/freshness/suite_lock.go internal/freshness/freshness.go internal/freshness/clockstep.go internal/freshness/unsigned.go internal/freshness/reaper_signal_test.go
git commit -m "$(cat <<'EOF'
Reaper: a run line and a liveness gauge; suite_reaped names its evidence

A completed reaper run logs suite_reaper_run_complete and sets
acu_suite_reaper_last_run_unixtime, so a reaper that stops is visible
(F-13); the forward-step report counts in acu_clock_steps_detected_total
(F-17); suite_reaped names an idle reap's stamp and a junk reap's rows,
a list since a path may hold a space (F-18, review M-7); a panicking flip-gate purge no longer leaks a suite's mutex: the
unlock is deferred (F-25); sortCandidates' AIDEV-NOTE is exact about
legacy snapshots (F-26); two history-only comments lose the history
(F-7).

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

### Task 7: GC: a store seam, sub-job C's deadline and chunk, recovered panics (F-1, F-2, F-3, F-21; reviews I-1, M-1)

Sub-job C (phase F) runs in GC's goroutine, which recovers no panic (F-21, design note 11); its deadline check (F-1) and its chunk size (F-2: mutants C22 and C23 survived) are untested; and its result embeds a struct whose promoted `Total()` leaves out the retired reaps (F-3). A narrow seam, `gcStore` (design note 18), lets tests reach C's calls without a 10,001-row fixture — such a fixture took 4.9 s under `-race` in the replay — and the cache's default chunk becomes a small pure function. Two design-review findings complete the recovers: the startup pass, which runs on main's goroutine before any listener, returns a panic as an error, which main logs before it serves, instead of crash-looping at every start (review I-1); and a tick's panic moves the pass rotation past the pass that panicked, so a pass that panics on every tick cannot starve the passes after it (review M-1). `docs/log-fields.md` gains `gc_panicked` in Task 18.

**Files:**
- Create: `internal/gc/gc_store_test.go`, `internal/cache/retired_chunk_test.go`
- Modify: `internal/gc/gc.go` (`GC.store`, `New`, `StartupPass`, `Run`, new `periodicTick`, `skipPanickedPass`, `gcStore`, `gcPanicStacks`, `recoverPanic`, `recoverPanicInto`, `logGCPanic`, `runTick`'s two counts), `internal/gc/snapshot.go` (`snapshotPassResult.batch`, `g.store`), `internal/gc/pool_scan.go` (rewritten: `scanCounters`, `scanPoolFile`, the producer's recover), `internal/cache/gc.go` (`effectiveChunkRows`)

**Interfaces:**
- Consumes: gc tests' `captureLogger`, `openTestCache`, `writePoolFile`.
- Produces: unexported `gcStore` interface (`RunSnapshotGCBatch`, `RunRetiredSnapshotGCChunk`, `HashKnown`), `GC.store`, `(*GC).StartupPass(ctx) (err error)` (a panic as an error), `(*GC).periodicTick(ctx)`, `(*GC).skipPanickedPass(passesDone *bool)`, `(*GC).recoverPanic(where string)`, `(*GC).recoverPanicInto(where string, err *error)`, `(*GC).logGCPanic(where string, v any)` (named so: `reportPanic` is a forwarding helper's name in Task 3's `logSinks`, and the guard would read this one's `where` as a message), `(*GC).scanPoolFile(ctx, poolRoot, prefix, name string, counts *scanCounters)`, `scanCounters`; `cache.effectiveChunkRows(maxRows int) int`; the ERROR event `gc_panicked` (`where`: `tick`, `startup`, `pool_scan_worker`, `pool_scan_producer`; `panic`, `stack`).

- [ ] **Step 1: Write the failing tests**

Create `internal/gc/gc_store_test.go`:

```go
package gc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// fakeStore is a gcStore whose snapshot batches find nothing (A ∪ B
// drained) unless told to panic, whose sub-job C chunks report a reap and
// more for chunksMore calls and then none, and whose HashKnown knows no
// hash and panics for hashPanic.
type fakeStore struct {
	mu          sync.Mutex
	batchPanics int           // RunSnapshotGCBatch panics on this many first calls
	batches     int           // RunSnapshotGCBatch calls
	chunkRows   []int         // maxRows of each RunRetiredSnapshotGCChunk call
	chunkSleep  time.Duration // each chunk takes this long (virtual time in a bubble)
	chunksMore  int
	hashPanic   string
}

func (f *fakeStore) RunSnapshotGCBatch(context.Context, int, int64, int) (cache.SnapshotGCBatchResult, error) {
	f.mu.Lock()
	f.batches++
	n := f.batches
	f.mu.Unlock()
	if n <= f.batchPanics {
		panic("snapshot batch bug")
	}
	return cache.SnapshotGCBatchResult{}, nil
}

func (f *fakeStore) RunRetiredSnapshotGCChunk(_ context.Context, maxRows int) (int, bool, error) {
	time.Sleep(f.chunkSleep)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chunkRows = append(f.chunkRows, maxRows)
	if len(f.chunkRows) > 1000 {
		panic("runaway sub-job C loop")
	}
	if len(f.chunkRows) <= f.chunksMore {
		return 1, true, nil
	}
	return 0, false, nil
}

func (f *fakeStore) HashKnown(_ context.Context, hash string) (bool, error) {
	if hash == f.hashPanic {
		panic("hash lookup bug")
	}
	return false, nil
}

// storeGC is a GC over a real cache (for the passes outside the seam)
// whose gcStore is store, logging into the returned buffer.
func storeGC(t *testing.T, store gcStore, workers int, logger *slog.Logger) (*GC, *cache.Cache) {
	t.Helper()
	c := openTestCache(t)
	g, err := New(Config{Cache: c, Logger: logger, Enabled: true, Interval: time.Hour, BatchSize: 100,
		SnapshotBatchSize: 10, MaxTickDuration: time.Minute, BlobGrace: 5 * time.Minute, KeepDisplaced: 3,
		PoolScanWorkers: workers, HeartbeatStaleGrace: 30 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	g.store = store
	return g, c
}

// gcEvents decodes buf's JSON records whose msg is msg.
func gcEvents(t *testing.T, buf *bytes.Buffer, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		var m map[string]any
		if line != "" && json.Unmarshal([]byte(line), &m) == nil && m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}

// Deferred minor F-2: sub-job C asks the cache for its own chunk
// (maxRows 0: retiredChunkRows, 10,000 rows a transaction), never a
// larger number, on every call until nothing retired is left.
func TestSnapshotPass_SubJobCAsksForTheCachesOwnChunk(t *testing.T) {
	logger, _ := captureLogger()
	f := &fakeStore{chunksMore: 2}
	g, _ := storeGC(t, f, 2, logger)
	res, expired, err := g.runSnapshotPass(context.Background(), time.Now().Add(time.Hour), "periodic")
	if err != nil || expired || res.retired != 2 {
		t.Fatalf("pass = %+v, expired %v, %v; want 2 retired reaped", res, expired, err)
	}
	if !reflect.DeepEqual(f.chunkRows, []int{0, 0, 0}) {
		t.Fatalf("chunk calls asked for %v rows, want [0 0 0] (the cache's own chunk)", f.chunkRows)
	}
}

// Deferred minor F-1: sub-job C checks the tick's deadline before each
// chunk: with chunks of 30 s and a minute left, it runs two and reports
// the deadline (gc_tick_deadline_reached, which=snapshot), leaving the
// rest to the next tick.
func TestSnapshotPass_SubJobCStopsAtTheDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger, buf := captureLogger()
		f := &fakeStore{chunksMore: 100, chunkSleep: 30 * time.Second}
		g, _ := storeGC(t, f, 2, logger)
		_, expired, err := g.runSnapshotPass(context.Background(), time.Now().Add(time.Minute), "periodic")
		if err != nil || !expired || len(f.chunkRows) != 2 {
			t.Fatalf("expired %v, %v, %d chunks; want the deadline after 2 chunks", expired, err, len(f.chunkRows))
		}
		if ev := gcEvents(t, buf, "gc_tick_deadline_reached"); len(ev) != 1 || ev[0]["which"] != "snapshot" {
			t.Fatalf("deadline lines %v, want one for the snapshot pass", ev)
		}
	})
}

// Deferred minor F-3: the snapshot pass's result names its batch counts
// instead of embedding them, so it promotes no Total() that would leave
// out the retired reaps.
func TestSnapshotPassResult_PromotesNoTotal(t *testing.T) {
	if _, ok := reflect.TypeOf(snapshotPassResult{}).MethodByName("Total"); ok {
		t.Fatal("snapshotPassResult has a Total method: a promoted count without retired")
	}
}

// Deferred minor F-21 (design note 11), Review Focus 4: a panic in a
// periodic tick — here in the snapshot pass — is recovered and logged
// (gc_panicked, where=tick, with its stack), and the next ticks run on
// schedule: the blob pass the rotation moved to, then a full rotation
// whose snapshot pass runs again (review M-1).
func TestRun_RecoversATickPanicAndRunsTheNextTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger, buf := captureLogger()
		f := &fakeStore{batchPanics: 1}
		g, _ := storeGC(t, f, 2, logger)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { g.Run(ctx); close(done) }()
		time.Sleep(3*time.Hour + time.Minute) // three ticks
		cancel()
		<-done
		f.mu.Lock()
		batches := f.batches
		f.mu.Unlock()
		if batches < 2 {
			t.Fatalf("%d snapshot batches, want the third tick's too", batches)
		}
		ev := gcEvents(t, buf, "gc_panicked")
		if len(ev) != 1 || ev[0]["level"] != "ERROR" || ev[0]["where"] != "tick" || ev[0]["panic"] != "snapshot batch bug" {
			t.Fatalf("gc_panicked lines %v, want one at ERROR, where=tick", ev)
		}
		if runs := gcEvents(t, buf, "gc_run_complete"); len(runs) != 2 {
			t.Fatalf("%d gc_run_complete lines, want 2 (the ticks after the panic)", len(runs))
		}
	})
}

// Review M-1 of the H1 plan: a pass that panics on every tick — here the
// snapshot pass's batch — moves the rotation past itself, so the blob pass
// after it still runs: the tick after the panic is the blob pass's, and
// the one after that comes back to the url-path and snapshot passes.
func TestPeriodicTick_APanickingPassDoesNotStarveTheNext(t *testing.T) {
	logger, buf := captureLogger()
	f := &fakeStore{batchPanics: 1000}
	g, _ := storeGC(t, f, 2, logger)
	ctx := context.Background()
	g.periodicTick(ctx) // the url-path pass (off: no TTL), then the snapshot pass panics
	if g.nextPass != blobPass {
		t.Fatalf("after a snapshot-pass panic the rotation is at pass %d, want the blob pass (%d)", g.nextPass, blobPass)
	}
	g.periodicTick(ctx) // the blob pass
	if g.nextPass != urlPathPass {
		t.Fatalf("after the blob pass the rotation is at pass %d, want the url-path pass (%d)", g.nextPass, urlPathPass)
	}
	if runs := gcEvents(t, buf, "gc_run_complete"); len(runs) != 1 {
		t.Fatalf("%d gc_run_complete lines, want 1 (the blob pass's tick)", len(runs))
	}
	g.periodicTick(ctx) // the snapshot pass again
	if ev := gcEvents(t, buf, "gc_panicked"); len(ev) != 2 {
		t.Fatalf("%d gc_panicked lines, want 2 (the snapshot pass, twice)", len(ev))
	}
}

// Review I-1 of the H1 plan: a panic in the startup pass — which runs on
// main's goroutine before any listener — is recovered and returned as an
// error, which main logs (gc startup pass failed) before it serves; a
// panic at every start would otherwise be a crash loop that never serves.
func TestStartupPass_ReturnsAPanicAsAnError(t *testing.T) {
	logger, buf := captureLogger()
	f := &fakeStore{batchPanics: 1}
	g, _ := storeGC(t, f, 2, logger)
	err := g.StartupPass(context.Background())
	if err == nil || !strings.Contains(err.Error(), "gc startup: panicked: snapshot batch bug") {
		t.Fatalf("StartupPass = %v, want the panic as an error", err)
	}
	ev := gcEvents(t, buf, "gc_panicked")
	if len(ev) != 1 || ev[0]["level"] != "ERROR" || ev[0]["where"] != "startup" || ev[0]["panic"] != "snapshot batch bug" {
		t.Fatalf("gc_panicked lines %v, want one at ERROR, where=startup", ev)
	}
}

// Deferred minor F-21, Review Focus 4: a pool-scan job that panics is
// recovered and its worker goes on, so the scan ends — with one worker
// and more files than the job queue holds, a dead worker would leave the
// producer blocked for good — and every other orphan is still unlinked.
func TestPoolScan_AWorkerPanicNeverStallsTheScan(t *testing.T) {
	logger, buf := captureLogger()
	f := &fakeStore{}
	g, c := storeGC(t, f, 1, logger)
	var files []string
	for i := range 70 {
		name := fmt.Sprintf("ab%062x", i)
		files = append(files, writePoolFile(t, c, "ab", name, "orphan"))
	}
	f.hashPanic = filepath.Base(files[0])
	done := make(chan scanResult, 1)
	go func() {
		res, err := g.runPoolScan(context.Background())
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	var res scanResult
	select {
	case res = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the pool scan stalled after a worker's panic")
	}
	if res.orphansRepaired != 69 {
		t.Fatalf("orphans repaired %d, want 69 (all but the panicking job's)", res.orphansRepaired)
	}
	if _, err := os.Stat(files[0]); err != nil {
		t.Fatalf("the panicking job's file: %v, want it left", err)
	}
	if ev := gcEvents(t, buf, "gc_panicked"); len(ev) != 1 || ev[0]["where"] != "pool_scan_worker" {
		t.Fatalf("gc_panicked lines %v, want one, where=pool_scan_worker", ev)
	}
}

// panicOnMsg is a slog handler that panics when it handles msg.
type panicOnMsg struct {
	slog.Handler
	msg string
}

func (h panicOnMsg) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == h.msg {
		panic("log handler bug")
	}
	return h.Handler.Handle(ctx, r)
}

// Deferred minor F-21: a panic in the pool scan's producer (here its log
// handler, on a stray file at the pool's root) is recovered, the job
// queue still closes and the scan ends.
func TestPoolScan_AProducerPanicEndsTheScan(t *testing.T) {
	_, buf := captureLogger()
	logger := slog.New(panicOnMsg{Handler: slog.NewJSONHandler(buf, nil), msg: "gc_pool_unexpected_root_file"})
	g, c := storeGC(t, &fakeStore{}, 2, logger)
	if err := os.WriteFile(filepath.Join(c.Dir(), "pool", "stray"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := g.runPoolScan(context.Background()); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the pool scan stalled after its producer's panic")
	}
	if ev := gcEvents(t, buf, "gc_panicked"); len(ev) != 1 || ev[0]["where"] != "pool_scan_producer" {
		t.Fatalf("gc_panicked lines %v, want one, where=pool_scan_producer", ev)
	}
}
```

Create `internal/cache/retired_chunk_test.go`:

```go
package cache

import "testing"

// Deferred minor F-2 (its mutant C22): a sub-job C chunk asked for 0 rows
// or fewer — what the snapshot pass asks for — deletes retiredChunkRows
// (10,000) rows at most in its transaction, and any other request its own
// number. One transaction of a whole 600,000-row snapshot would hold the
// writer about 5 s.
func TestEffectiveChunkRows(t *testing.T) {
	for _, tc := range []struct{ asked, want int }{{0, 10_000}, {-1, 10_000}, {1, 1}, {500, 500}} {
		if got := effectiveChunkRows(tc.asked); got != tc.want {
			t.Errorf("effectiveChunkRows(%d) = %d, want %d", tc.asked, got, tc.want)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -gcflags=-e -count=1 ./internal/gc/`
Expected: build failure naming `undefined: gcStore` (gc_store_test.go:69) and `g.store undefined (type *GC has no field or method store)` (:78).

Run: `go test -gcflags=-e -count=1 -run TestEffectiveChunkRows ./internal/cache/`
Expected: build failure naming `undefined: effectiveChunkRows`.

- [ ] **Step 3: The cache's default chunk (`internal/cache/gc.go`)**

Before `RunRetiredSnapshotGCChunk`'s doc:

```go
// effectiveChunkRows is the rows one sub-job C transaction deletes for a
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
```

and its first lines:

old:
```go
func (c *Cache) RunRetiredSnapshotGCChunk(ctx context.Context, maxRows int) (reaped int, more bool, err error) {
	if maxRows <= 0 {
		maxRows = retiredChunkRows
	}
```
new:
```go
func (c *Cache) RunRetiredSnapshotGCChunk(ctx context.Context, maxRows int) (reaped int, more bool, err error) {
	maxRows = effectiveChunkRows(maxRows)
```

- [ ] **Step 4: The seam, the named result, the recovers (`internal/gc`)**

`gc.go`: imports gain `"runtime/debug"`. `GC` gains, after `cfg Config`:

```go
	// store is cfg.Cache behind gcStore (the snapshot pass and the pool
	// scan); tests replace it.
	store gcStore
```

and `New`'s two returns become `return &GC{cfg: cfg, store: cfg.Cache}, nil`. `StartupPass`:

old:
```go
// When Enabled = false, returns nil immediately.
func (g *GC) StartupPass(ctx context.Context) error {
	if !g.cfg.Enabled {
		return nil
	}
```
new:
```go
// When Enabled = false, returns nil immediately.
//
// A panic is recovered and returned as an error (gc_panicked,
// where=startup), which main logs before it serves: this pass runs on
// main's goroutine before any listener, and a panic that persisted data
// triggers would otherwise recur at every start (review I-1 of the H1
// plan).
func (g *GC) StartupPass(ctx context.Context) (err error) {
	defer g.recoverPanicInto("startup", &err)
	if !g.cfg.Enabled {
		return nil
	}
```

(main already logs a returned error as WARN `gc startup pass failed` and serves on.) In `runTick`, `res.orphanCandidatesReaped += snap.OrphanReaped` and `res.displacedReaped += snap.DisplacedReaped` read `snap.batch.OrphanReaped` and `snap.batch.DisplacedReaped`. `Run`'s tick case becomes:

```go
		case <-t.C:
			if ctx.Err() != nil {
				return
			}
			g.periodicTick(ctx)
			t.Reset(g.cfg.Interval)
		}
```

and the case's former body — every line from `start := time.Now()` through the `g.recordLastRun(LastRunSummary{…})` call, unchanged, one tab less indented — moves into a new method after `Run`; the line, the metrics and the last-run summary stay inside it, so a tick that panicked leaves `acu_gc_last_run_unixtime` as it was:

```go
// periodicTick is one periodic tick: the passes (runTick), its
// gc_run_complete line, metrics and last-run summary. A panic is
// recovered here — logged gc_panicked, where=tick — and the next tick
// comes as scheduled (deferred minor F-21: sub-job C runs in this
// goroutine since phase F). The line, the metrics and the last-run
// summary stay inside the recovered function, so a tick that panicked
// leaves acu_gc_last_run_unixtime as it was: its age is the alert. A
// panic in a pass moves the rotation past that pass (skipPanickedPass).
func (g *GC) periodicTick(ctx context.Context) {
	passesDone := false
	defer g.skipPanickedPass(&passesDone)
	defer g.recoverPanic("tick")
	start := time.Now()
	tick, err := g.runTick(ctx, "periodic")
	passesDone = true
	// … the rest of the former case body, unchanged, through g.recordLastRun(…) …
}
```

After `periodicTick`:

```go
// gcStore is what the snapshot pass and the pool scan call on the cache
// (design note 18 of the H1 plan): a seam, so tests reach sub-job C's
// deadline and chunk size without a 10,000-row fixture (deferred minors
// F-1, F-2) and make a tick or a pool-scan job panic (F-21). New sets it
// to Config.Cache; the url_path and blob passes keep Config.Cache.
type gcStore interface {
	RunSnapshotGCBatch(ctx context.Context, batchSize int, staleGraceSeconds int64, keepDisplaced int) (cache.SnapshotGCBatchResult, error)
	RunRetiredSnapshotGCChunk(ctx context.Context, maxRows int) (reaped int, more bool, err error)
	HashKnown(ctx context.Context, hash string) (bool, error)
}

// skipPanickedPass is deferred by periodicTick before its recoverPanic,
// so it runs after it: when the passes did not finish (a recovered panic),
// it moves the rotation past the pass in progress — g.nextPass, which
// runTick advances only when a pass completes — so a pass that panics on
// every tick cannot starve the passes after it: the blob pass keeps
// reclaiming disk while a sub-job's bug waits for its fix (review M-1 of
// the H1 plan).
func (g *GC) skipPanickedPass(passesDone *bool) {
	if *passesDone {
		return
	}
	switch g.nextPass {
	case urlPathPass:
		g.nextPass = snapshotPass
	case snapshotPass:
		g.nextPass = blobPass
	default:
		g.nextPass = urlPathPass
	}
}

// gcPanicStacks holds the wheres whose gc_panicked line carried a stack.
var gcPanicStacks sync.Map

// recoverPanic is deferred by each unit of work of GC's goroutines — a
// periodic tick, a pool-scan job, the pool-scan producer: it logs a
// recovered panic as ERROR gc_panicked (where, panic, and the stack the
// first time per where for the process) and lets its goroutine go on
// (deferred minor F-21; the overview's rule for internal/gc).
//
// AIDEV-NOTE: recover works only because this method IS the deferred
// function (defer g.recoverPanic(where)); a wrapping closure would make
// recover return nil and the panic would kill the daemon. A logger that
// panics in turn is contained too: that panic is dropped.
func (g *GC) recoverPanic(where string) {
	v := recover()
	if v == nil {
		return
	}
	g.logGCPanic(where, v)
}

// recoverPanicInto is recoverPanic for a function that returns an error
// (StartupPass): it also sets *err, so the caller sees the panic as a
// failure. Like recoverPanic it works only as the deferred function itself
// (defer g.recoverPanicInto(where, &err)).
func (g *GC) recoverPanicInto(where string, err *error) {
	v := recover()
	if v == nil {
		return
	}
	*err = fmt.Errorf("gc %s: panicked: %v", where, v)
	g.logGCPanic(where, v)
}

// logGCPanic logs a recovered panic as ERROR gc_panicked; a logger that
// panics in turn is contained: that panic is dropped.
func (g *GC) logGCPanic(where string, v any) {
	defer func() { _ = recover() }()
	attrs := []any{"where", where, "panic", fmt.Sprint(v)}
	if _, seen := gcPanicStacks.LoadOrStore(where, true); !seen {
		attrs = append(attrs, "stack", string(debug.Stack()))
	}
	g.cfg.Logger.Error("gc_panicked", attrs...)
}
```

`snapshot.go`:

old:
```go
// snapshotPassResult counts one snapshot pass's reaps by class.
type snapshotPassResult struct {
	cache.SnapshotGCBatchResult
	// retired counts the retired snapshots sub-job C deleted (spec C9).
	retired int
}
```
new:
```go
// snapshotPassResult counts one snapshot pass's reaps by class.
//
// AIDEV-NOTE: batch is a named field, never embedded (deferred minor
// F-3): embedded, SnapshotGCBatchResult's Total() was promoted and
// silently left out retired.
type snapshotPassResult struct {
	batch cache.SnapshotGCBatchResult
	// retired counts the retired snapshots sub-job C deleted (spec C9).
	retired int
}
```

and in `runSnapshotPass`: `g.cfg.Cache.RunSnapshotGCBatch(` becomes `g.store.RunSnapshotGCBatch(`; `acc.OrphanReaped += …` and `acc.DisplacedReaped += …` become `acc.batch.OrphanReaped += …` and `acc.batch.DisplacedReaped += …`; and

old:
```go
		reaped, more, err := g.cfg.Cache.RunRetiredSnapshotGCChunk(ctx, 0)
```
new:
```go
		// 0: the cache's own chunk (retiredChunkRows, 10,000 rows a
		// transaction), never a larger number (deferred minor F-2).
		reaped, more, err := g.store.RunRetiredSnapshotGCChunk(ctx, 0)
```

`pool_scan.go` is replaced by:

```go
package gc

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

// scanResult bundles the outcome of one pool/ orphan scan for the
// gc_run_complete startup line.
type scanResult struct {
	orphansRepaired     int
	orphanBytesRepaired int64
	unlinkErrors        int
}

// scanCounters are one pool scan's tallies, shared by its workers.
type scanCounters struct {
	orphans, orphanBytes, unlinkErrors atomic.Int64
}

// runPoolScan implements SPEC4 §9.6.4: walk pool/<two-hex-prefix>/
// directories with a worker pool; for each file, ask the cache
// whether a blob row exists for that hash; if not, unlink it; if
// the filename doesn't satisfy sha256-hex shape, log
// gc_pool_malformed_name and leave the file alone; if the parent
// directory doesn't equal hash[:2], log gc_pool_misplaced_file and
// leave the file alone.
//
// Cancellable via ctx; partial progress is preserved (rows reaped
// before cancel are real). Returns a non-nil error only on a fatal
// failure (e.g. cannot read pool/ root).
func (g *GC) runPoolScan(ctx context.Context) (scanResult, error) {
	poolRoot := filepath.Join(g.cfg.Cache.Dir(), "pool")

	prefixEntries, err := os.ReadDir(poolRoot)
	if err != nil {
		// pool/ should always exist (cache.Open creates it). A read
		// error here is fatal for the scan but not for the daemon —
		// surface it; main decides whether to keep going.
		return scanResult{}, err
	}

	type job struct {
		prefix string
		name   string
	}
	jobs := make(chan job, 64)

	var (
		counts scanCounters
		wg     sync.WaitGroup
	)

	worker := func() {
		defer wg.Done()
		for j := range jobs {
			g.scanPoolFile(ctx, poolRoot, j.prefix, j.name, &counts)
		}
	}

	for i := 0; i < g.cfg.PoolScanWorkers; i++ {
		wg.Add(1)
		go worker()
	}

	// Producer: walk pool/<prefix>/<file>. Errors on a single
	// prefix are logged at Warn but the scan continues — losing
	// the contents of one prefix is better than aborting the whole
	// scan.
	feedDone := make(chan struct{})
	go func() {
		defer close(feedDone)
		defer close(jobs)
		// Runs first: a panic still closes jobs and feedDone, so the
		// workers drain and the scan ends (deferred minor F-21).
		defer g.recoverPanic("pool_scan_producer")
		for _, p := range prefixEntries {
			if err := ctx.Err(); err != nil {
				return
			}
			if !p.IsDir() {
				// Stray file at pool/<name> — not under a prefix
				// directory at all. Log and skip; the operator
				// decides what to do.
				g.cfg.Logger.Warn("gc_pool_unexpected_root_file",
					"path", p.Name(),
				)
				continue
			}
			prefixPath := filepath.Join(poolRoot, p.Name())
			files, err := os.ReadDir(prefixPath)
			if err != nil {
				g.cfg.Logger.Warn("gc_pool_scan_dir_failed",
					"prefix", p.Name(),
					"err", err.Error(),
				)
				continue
			}
			for _, f := range files {
				if f.IsDir() {
					// Unexpected — pool layout is two-deep only.
					// Log and skip.
					g.cfg.Logger.Warn("gc_pool_unexpected_subdir",
						"path", filepath.Join(p.Name(), f.Name()),
					)
					continue
				}
				select {
				case jobs <- job{prefix: p.Name(), name: f.Name()}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	<-feedDone
	wg.Wait()

	return scanResult{
		orphansRepaired:     int(counts.orphans.Load()),
		orphanBytesRepaired: counts.orphanBytes.Load(),
		unlinkErrors:        int(counts.unlinkErrors.Load()),
	}, nil
}

// scanPoolFile is one pool-scan job, a worker's turn: the file
// pool/<prefix>/<name> is checked and, when no blob row names it,
// unlinked, tallied in counts.
//
// AIDEV-NOTE: deferred minor F-21. A panic is recovered per job
// (gc_panicked, where=pool_scan_worker) and the worker goes on to the
// next: a worker that died would leave the producer blocked on jobs once
// every worker had, until shutdown, and the startup pass would never end
// (TestPoolScan_AWorkerPanicNeverStallsTheScan).
func (g *GC) scanPoolFile(ctx context.Context, poolRoot, prefix, name string, counts *scanCounters) {
	defer g.recoverPanic("pool_scan_worker")
	if err := ctx.Err(); err != nil {
		// Drain remaining jobs without doing work; the outer cancel
		// will close jobs and the worker's range loop exits.
		return
	}
	rel := filepath.Join(prefix, name)
	abs := filepath.Join(poolRoot, prefix, name)

	// Validate filename shape before any DB lookup. HashKnown rejects
	// malformed hashes, but routing through it would produce a
	// misleading "ErrInvalidHash" log line — gc_pool_malformed_name is
	// the right surface.
	if !validHashLite(name) {
		g.cfg.Logger.Warn("gc_pool_malformed_name",
			"path", rel,
		)
		return
	}

	// Prefix consistency check (SPEC4 §9.6.4): a file at
	// pool/<wrong-prefix>/<hash> is unreachable by Cache.BlobPath
	// regardless of whether a blob row exists. Logging-only;
	// auto-repair (move or unlink) is rejected per SPEC4 §11.
	if expected := name[:2]; expected != prefix {
		g.cfg.Logger.Warn("gc_pool_misplaced_file",
			"path", rel,
			"expected_prefix", expected,
			"actual_prefix", prefix,
		)
		return
	}

	known, err := g.store.HashKnown(ctx, name)
	if err != nil {
		// HashKnown's only documented error is the validBlobHash gate
		// (caught above) plus DB-level failures. Surface as Warn;
		// don't unlink on uncertainty.
		g.cfg.Logger.Warn("gc_pool_scan_lookup_failed",
			"path", rel,
			"err", err.Error(),
		)
		return
	}
	if known {
		// File is referenced; leave it alone.
		return
	}

	// Capture size before unlinking for the gc_run_complete bytes
	// count. A stat failure is non-fatal — proceed with unlink, just
	// don't tally bytes for this file.
	var size int64
	if st, err := os.Stat(abs); err == nil {
		size = st.Size()
	}

	if err := os.Remove(abs); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			g.cfg.Logger.Warn("gc_pool_unlink_failed",
				"hash", name,
				"err", err.Error(),
				"operation", "pool_scan",
			)
			counts.unlinkErrors.Add(1)
			return
		}
		// ENOENT — somebody else already removed it. Don't tally as
		// orphan-repaired (we didn't do the work); also don't tally as
		// error.
		return
	}
	counts.orphans.Add(1)
	counts.orphanBytes.Add(size)
}

// validHashLite is a local copy of cache.validBlobHash's predicate;
// duplicated here so the pool scan can decide between
// gc_pool_malformed_name and a call to Cache.HashKnown without
// round-tripping through the cache package's ErrInvalidHash error
// (which would produce a less informative log line).
func validHashLite(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
```

After the edits `grep -n 'go func' internal/gc/*.go | grep -v _test.go` lists the producer only, and it recovers; the workers' `go worker()` recovers per job.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -count=1 ./internal/gc/ && go test -count=1 -run 'TestEffectiveChunkRows|TestRunRetiredSnapshotGCChunk' ./internal/cache/`
Expected: two `ok` lines.

Run: `go test -race -count=1 ./internal/gc/ ./internal/cache/ ./internal/admin/ ./cmd/...`
Expected: all `ok`.

- [ ] **Step 6: Mutation checks** (one at a time, each reverted)

| Mutant | Fails |
|---|---|
| `effectiveChunkRows` returns `1 << 30` for 0 or less (C22) | `retired_chunk_test.go:13: effectiveChunkRows(0) = 1073741824, want 10000` |
| The pass asks `RunRetiredSnapshotGCChunk(ctx, 1<<30)` (C23) | `gc_store_test.go:107: chunk calls asked for [1073741824 1073741824 1073741824] rows, want [0 0 0]` |
| Sub-job C's `if expired()` check removed (F-1) | `gc_store_test.go:122: expired false, <nil>, 101 chunks; want the deadline after 2 chunks` |
| `defer g.recoverPanic("tick")` removed | the test binary panics: `panic: snapshot batch bug` from `TestRun_RecoversATickPanicAndRunsTheNextTick` |
| `defer g.recoverPanic("pool_scan_worker")` removed | the test binary panics: `panic: hash lookup bug` from `TestPoolScan_AWorkerPanicNeverStallsTheScan` |
| `defer g.recoverPanicInto("startup", &err)` removed (review I-1) | the test binary panics: `panic: snapshot batch bug [recovered, repanicked]` from `TestStartupPass_ReturnsAPanicAsAnError` |
| `recoverPanicInto` sets no error (`*err = …` dropped) | `gc_store_test.go:207: StartupPass = <nil>, want the panic as an error` |
| `defer g.skipPanickedPass(&passesDone)` removed (review M-1) | `gc_store_test.go:182: after a snapshot-pass panic the rotation is at pass 1, want the blob pass (2)` |
| `passesDone = true` removed (every tick skips a pass) | `gc_store_test.go:186: after the blob pass the rotation is at pass 1, want the url-path pass (0)` |

- [ ] **Step 7: Commit**

```bash
gofmt -l internal/gc/gc.go internal/gc/snapshot.go internal/gc/pool_scan.go internal/gc/gc_store_test.go internal/cache/gc.go internal/cache/retired_chunk_test.go
git add internal/gc/gc.go internal/gc/snapshot.go internal/gc/pool_scan.go internal/gc/gc_store_test.go internal/cache/gc.go internal/cache/retired_chunk_test.go
git commit -m "$(cat <<'EOF'
GC: recover panics in its goroutines; pin sub-job C's deadline and chunk

A panic in a periodic tick, a pool-scan job or the pool-scan producer is
recovered and logged gc_panicked; the next tick runs, the scan ends
(F-21; sub-job C runs in GC's goroutine since phase F). A tick's panic
moves the pass rotation past the pass that panicked, so the passes after
it still run (review M-1), and the startup pass, which runs on main's
goroutine before any listener, returns its panic as an error that main
logs before it serves, instead of crash-looping at every start (review
I-1). Through a narrow
gcStore seam, tests pin that sub-job C stops at the tick's deadline (F-1)
and asks the cache for its own 10,000-row chunk, and the cache's default
is a pure function with its own test (F-2: mutants C22 and C23). The
pass's result names its batch counts instead of embedding a type whose
Total() left out the retired reaps (F-3).

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

### Task 8: The check path: pins, one hold predicate, one signed predicate, quieter clock steps (E-21, E-23, E-25, E-27, E-28, E-30, E-31, E-32, E-37, GD-6)

Ten small items on the check path. Four pin behaviour no test reached (E-21, E-27, E-28 — tests only); two give a repeated predicate one spelling (E-32 `retryGate.holds`, E-31 `signedForm`, which also logs `Reconcile`'s fail-closed read); E-37 stops a cooldown-refused client check from counting a clock step; E-30 and GD-6 add one field each to a log line (PF-11) and GD-6 its shutdown level; E-23 and E-25 are comments. `docs/log-fields.md` gains `reconcile_snapshot_read_failed`, `paced_stale` and `freshness_read_anchor_rows_failed`'s `canonical_scheme` in Task 18.

**Files:**
- Create: `internal/freshness/check_path_test.go`
- Modify: `internal/freshness/freshness.go` (`retryGate.holds`, `Reconcile`), `internal/freshness/check.go` (`checkLocked`, `baseline`, new `signedForm`, `checkInline`'s doc and steady branch, `checkDetached`'s steady comment and hold), `internal/freshness/current_form.go` (`anchorAbsent`, `lateralInline`, `upgradeProbe`, `checkUnsigned`), `internal/freshness/first_adoption.go` (`checkFirst`), `internal/freshness/probe.go` (`probe`'s comment), `internal/freshness/scheduler.go` (`suiteDue`'s AIDEV-NOTE, `tick`)

**Interfaces:**
- Consumes: test helpers `newRetryRig`, `lockedBuffer`, `logRecords` (`check_tail_test.go`), `snapReadFailCache` (`current_form_test.go`), `mustAcquire`, `seedFakeAnchor`, `changedUpstream`, `renderedMetric`, `renderedMetricLabels`, `renderedFloat` (`reconcile_test.go`: a value that does not parse fails the test, E-51's point), `armoredSig`, `contentKey`.
- Produces: `(retryGate).holds(key string) bool`; `signedForm(form cache.AnchorForm) bool`; WARN `reconcile_snapshot_read_failed` (`err`, `snapshot_id`, `canonical_scheme`, `canonical_host`, `suite_path`); field `paced_stale` on `freshness: adopted suites stale beyond threshold`; field `canonical_scheme` on `anchorAbsent`'s `freshness_read_anchor_rows_failed`.

- [ ] **Step 1: Write the failing tests**

Create `internal/freshness/check_path_test.go`:

```go
package freshness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// Deferred minor E-21: the detached twin of
// TestCheck_UsesSnapshotHashForComparison. A detached suite's Release is
// compared with its current snapshot's release_hash, not the url_path
// anchor's blob: the same Release is unchanged, and its signature is never
// fetched.
func TestCheck_UsesDetachedSnapshotHashForComparison(t *testing.T) {
	fc := newFakeCache()
	body, sig := []byte("Origin: Test\nSuite: x\n"), armoredSig("the signature")
	rel, sigHash, stale := hashOf(body), hashOf(sig), testHex64
	id := int64(7)
	fc.putSnapshot(cache.SuiteSnapshot{SnapshotID: id, CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x",
		Form: cache.FormDetached, ReleaseHash: &rel, ReleaseGPGHash: &sigHash})
	fc.putSuite(cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", CurrentSnapshotID: &id})
	for path, h := range map[string]*string{"/r/dists/x/Release": &stale, "/r/dists/x/Release.gpg": &sigHash} {
		fc.putURL(cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: path, BlobHash: h,
			UpstreamURL: "http://h" + path, IsMetadata: true})
	}
	up := simtest.NewUpstream()
	up.Set("http://h/r/dists/x/Release", func(context.Context, string) (*fetch.ConditionalResult, error) {
		return &fetch.ConditionalResult{Status: 200, Body: body}, nil
	})
	up.Set("http://h/r/dists/x/Release.gpg", func(context.Context, string) (*fetch.ConditionalResult, error) {
		return &fetch.ConditionalResult{Status: 200, Body: sig}, nil
	})
	c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8), Logger: discardLogger(),
		now: func() time.Time { return time.Unix(1_800_000_000, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	rep := c.check(context.Background(), SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}, checkOpts{})
	if rep.result != "unchanged" {
		t.Fatalf("result %q, want unchanged (the snapshot's Release, not the stale url_path blob)", rep.result)
	}
	if n := len(up.Calls("http://h/r/dists/x/Release.gpg")); n != 0 {
		t.Fatalf("%d Release.gpg fetches, want none for an unchanged Release", n)
	}
	if row, _ := fc.suite("http", "h", "/r/dists/x"); row.InReleaseChangeSeenAt != nil {
		t.Fatalf("inrelease_change_seen_at = %d, want nil", *row.InReleaseChangeSeenAt)
	}
}

// Deferred minor E-32: the spec C7 hold — an active record, not yet due,
// for this very content — has one spelling.
func TestRetryGate_Holds(t *testing.T) {
	const k = "key-of-the-failing-content"
	for _, tc := range []struct {
		name string
		gate retryGate
		key  string
		want bool
	}{
		{"active, not due, same content", retryGate{active: true, hash: k}, k, true},
		{"no record", retryGate{hash: k}, k, false},
		{"due", retryGate{active: true, due: true, hash: k}, k, false},
		{"other content", retryGate{active: true, hash: k}, "other", false},
	} {
		if got := tc.gate.holds(tc.key); got != tc.want {
			t.Errorf("%s: holds = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Deferred minor E-27 (survivor C3): a probe sends no validators, so a 304
// answering it is a protocol violation — unknown, never present.
func TestProbe_A304IsUnknown(t *testing.T) {
	up := simtest.NewUpstream()
	up.Set("http://h/r/dists/x/InRelease", func(context.Context, string) (*fetch.ConditionalResult, error) {
		return &fetch.ConditionalResult{Status: 304}, nil
	})
	c, err := New(Config{Cache: newFakeCache(), Fetcher: up, HostLimiter: hostsem.New(1), Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	suite := SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}
	o := c.probe(context.Background(), suite, c.anchorURLsFor(suite, nil), "InRelease")
	if o.class != classUnknown || o.status != 304 || !errors.Is(o.err, errUnexpectedStatus) {
		t.Fatalf("probe = %v (status %d, err %v), want unknown: a 304 to a request without validators", o.class, o.status, o.err)
	}
}

// Deferred minor E-28 (survivors X2, X3; design review I-1): a cross-form
// request's failure record keeps no validators — they belong to another
// anchor file than the one the current form conditional-GETs — for the
// unsigned suite's upgrade probe and for the probe tree of an absent
// anchor (lateralInline's was pinned already). The rig upstream gives
// every 200 an ETag.
func TestCheck_CrossFormFailureRecordsKeepNoValidators(t *testing.T) {
	noValidators := func(t *testing.T, r *retryRig) {
		t.Helper()
		row := r.row()
		if strOf(row.AdoptionFailReason) != "gpg_failed" {
			t.Fatalf("setup: record %q/%d, want the failed attempt's", strOf(row.AdoptionFailReason), row.AdoptionFailCount)
		}
		if row.AdoptionFailETag != nil || row.AdoptionFailLastMod != nil {
			t.Fatalf("record validators %q / %q, want none", strOf(row.AdoptionFailETag), strOf(row.AdoptionFailLastMod))
		}
	}
	t.Run("the upgrade probe", func(t *testing.T) {
		r := newRetryRig(t, 0)
		r.adoptUnsigned("u1")
		in := r.release("signed", 0)
		r.ver.reject(hashOf(in))
		r.up.setFail(rigAnchorURL, nil)
		r.up.set(in)
		r.check()
		noValidators(t, r)
	})
	t.Run("the absent anchor's probe tree", func(t *testing.T) {
		r := newRetryRig(t, 0)
		r.adopt()
		rel, sig := r.releaseText("v2", 0), armoredSig("v2 sig")
		r.ver.reject(contentKey(adoptionFormDetached, nil, rel, sig))
		r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 404})
		r.up.setAt(rigReleaseURL, rel)
		r.up.setAt(rigReleaseGPGURL, sig)
		r.clock.advance(time.Minute)
		r.check()                        // the first observation
		r.clock.advance(6 * time.Minute) // due, and past the confirmation gap: the tree runs
		r.check()
		noValidators(t, r)
	})
}

// Deferred minor E-30: the stale WARN names the stale suites it leaves out
// (paced by their record), so its two counts add up to the gauge's
// adopted="true" value.
func TestTick_StaleWarnNamesThePacedSuitesLeftOut(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	old, retry := now.Add(-5*time.Hour).Unix(), now.Add(time.Hour).Unix()
	absent, id := cache.ReasonAnchorAbsent, int64(3)
	fc := newFakeCache()
	fc.putSuite(cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/paced", LastSuccessAt: &old,
		CurrentSnapshotID: &id, AdoptionFailCount: 1, AdoptionFailReason: &absent, AdoptionFailHash: &anchorAbsentKey,
		AdoptionNextRetryAt: &retry})
	fc.putSuite(cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/frozen", LastSuccessAt: &old,
		CurrentSnapshotID: &id})
	var logs lockedBuffer
	c, err := New(Config{Cache: fc, Fetcher: &changedUpstream{}, HostLimiter: hostsem.New(1), Refresh: time.Hour,
		Adopter: &Adopter{}, Logger: slog.New(slog.NewJSONHandler(&logs, nil)), now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	c.tick(context.Background(), newSchedQueues())
	warns := logRecords(t, &logs, "freshness: adopted suites stale beyond threshold")
	if len(warns) != 1 || warns[0]["adopted_stale"] != float64(1) || warns[0]["paced_stale"] != float64(1) {
		t.Fatalf("stale WARNs %v, want one with adopted_stale 1 and paced_stale 1", warns)
	}
	if got := renderedMetricLabels("acu_freshness_stale_suites", `adopted="true"`); got != "2" {
		t.Fatalf(`acu_freshness_stale_suites{adopted="true"} = %q, want 2`, got)
	}
}

// Deferred minor E-31: one predicate for a signed current snapshot; an
// unknown or empty form is not signed.
func TestSignedForm(t *testing.T) {
	for form, want := range map[cache.AnchorForm]bool{cache.FormInline: true, cache.FormDetached: true, cache.FormUnsigned: false, "": false} {
		if got := signedForm(form); got != want {
			t.Errorf("signedForm(%q) = %v, want %v", form, got, want)
		}
	}
}

// Deferred minor E-31: Reconcile counts a current snapshot it cannot read
// as unsigned — it never takes the reserved slot — and says so at WARN.
func TestReconcile_AnUnreadableSnapshotIsLoggedAndCountsUnsigned(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopt()
	fc := &snapReadFailCache{Cache: r.cache}
	fc.fail.Store(true)
	var logs lockedBuffer
	chk, err := New(Config{Cache: fc, Fetcher: r.up, HostLimiter: hostsem.New(8),
		Adopter: r.adopter, Logger: slog.New(slog.NewJSONHandler(&logs, nil)), now: r.clock.now})
	if err != nil {
		t.Fatal(err)
	}
	held := mustAcquire(t, chk.admission, "other", true)
	defer held.release()
	if chk.Reconcile(context.Background(), "http", rigHost, rigSuite) {
		t.Fatal("a reconcile whose snapshot could not be read took the reserved slot")
	}
	warns := logRecords(t, &logs, "reconcile_snapshot_read_failed")
	if len(warns) != 1 || warns[0]["level"] != "WARN" || warns[0]["snapshot_id"] != float64(*r.row().CurrentSnapshotID) ||
		warns[0]["canonical_scheme"] != "http" || warns[0]["suite_path"] != rigSuite ||
		!strings.Contains(fmt.Sprint(warns[0]["err"]), "injected snapshot read failure") {
		t.Fatalf("WARN records %v, want one naming the snapshot, the suite and the error", warns)
	}
}

// Deferred minor E-37: a future last_success_at is reported by a check
// that runs, not by a client check the cooldown turns away.
func TestCheck_FutureLastSuccessIsReportedBelowTheCooldown(t *testing.T) {
	var clock atomic.Int64
	clock.Store(1_800_000_000)
	future, recent := clock.Load()+3600, clock.Load()-10
	fc := newFakeCache()
	seedFakeAnchor(fc)
	fc.putSuite(cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x",
		LastSuccessAt: &future, LastCheckAt: &recent})
	c, err := New(Config{Cache: fc, Fetcher: &changedUpstream{}, HostLimiter: hostsem.New(1), Cooldown: time.Minute,
		Logger: discardLogger(), now: func() time.Time { return time.Unix(clock.Load(), 0) }})
	if err != nil {
		t.Fatal(err)
	}
	steps := func() float64 { return renderedFloat(t, renderedMetric("acu_clock_steps_detected_total")) }
	before := steps()
	c.Check(context.Background(), "http", "h", "/r/dists/x")
	if got := steps() - before; got != 0 {
		t.Fatalf("a check inside the cooldown counted %v clock steps, want 0", got)
	}
	clock.Add(120) // past the cooldown
	c.Check(context.Background(), "http", "h", "/r/dists/x")
	if got := steps() - before; got != 1 {
		t.Fatalf("the check past the cooldown counted %v clock steps, want 1", got)
	}
}

// releaseGPGFailCache fails LookupURL for a Release.gpg row while fail is
// set, calling onFail first.
type releaseGPGFailCache struct {
	Cache
	fail   atomic.Bool
	onFail func()
}

func (c *releaseGPGFailCache) LookupURL(ctx context.Context, scheme, host, path string) (*cache.URLPath, error) {
	if c.fail.Load() && strings.HasSuffix(path, "/Release.gpg") {
		if c.onFail != nil {
			c.onFail()
		}
		return nil, errors.New("injected Release.gpg row read failure")
	}
	return c.Cache.LookupURL(ctx, scheme, host, path)
}

// Deferred minor GD-6: when an adopted suite's absent anchor sends it down
// the probe tree and its cached anchor rows cannot be read, the WARN names
// the scheme (an http suite and its https namesake differ), and a read a
// shutdown cut short is DEBUG, as checkFirst's is (ruling PG-35).
func TestAnchorAbsent_AnchorRowReadFailure(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprintf("shutdown=%v", shutdown), func(t *testing.T) {
			r := newRetryRig(t, 0)
			r.adopt()
			fc := &releaseGPGFailCache{Cache: r.cache}
			var logs lockedBuffer
			chk, err := New(Config{Cache: fc, Fetcher: r.up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute,
				Adopter: r.adopter, Logger: slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
				now: r.clock.now, rand: func() float64 { return 0.5 }})
			if err != nil {
				t.Fatal(err)
			}
			r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 404})
			r.clock.advance(time.Minute)
			chk.Check(context.Background(), "http", rigHost, rigSuite) // the first observation
			chk.WaitForAdoptions()
			r.clock.advance(6 * time.Minute) // due, past the confirmation gap: the tree reads the rows
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if shutdown {
				fc.onFail = cancel
			}
			fc.fail.Store(true)
			chk.Check(ctx, "http", rigHost, rigSuite)
			chk.WaitForAdoptions()
			warns := logRecords(t, &logs, "freshness_read_anchor_rows_failed")
			ended := logRecords(t, &logs, "freshness_check_ended_by_shutdown")
			if shutdown {
				if len(warns) != 0 || len(ended) != 1 || ended[0]["step"] != "read the anchor rows" {
					t.Fatalf("WARNs %v, shutdown lines %v; want no WARN and one DEBUG for the read", warns, ended)
				}
				return
			}
			if len(warns) != 1 || warns[0]["level"] != "WARN" || warns[0]["canonical_scheme"] != "http" ||
				warns[0]["suite_path"] != rigSuite || !strings.Contains(fmt.Sprint(warns[0]["err"]), "injected Release.gpg row read failure") {
				t.Fatalf("WARN records %v, want one naming the scheme, the suite and the error", warns)
			}
		})
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -gcflags=-e -count=1 -run 'TestCheck_UsesDetachedSnapshot' ./internal/freshness/`
Expected: build failure naming `tc.gate.holds undefined (type retryGate has no field or method holds)` (check_path_test.go:76) and `undefined: signedForm` (:176). E-21, E-27 and E-28 are pins of behaviour that holds: they pass once the package builds, and Step 6's mutants are their red.

- [ ] **Step 3: One hold predicate, one signed predicate (E-32, E-31)**

`freshness.go`, after `recordValidators`:

```go
// holds reports whether the failure record holds back content key: a
// record is active, not yet due, and was written for that very content
// (spec C7 "Gate": the same content before the retry time is deferred).
// The one spelling of the predicate (deferred minor E-32).
func (g retryGate) holds(key string) bool {
	return g.active && !g.due && key == g.hash
}
```

The six sites read it: `check.go` (`checkInline`: `} else if gate.holds(newHash) {`; `checkDetached`: `if gate.holds(key) {`), `current_form.go` (`lateralInline`: `heldByRecord := gate.holds(req.key)`; `upgradeProbe`: `if gate.holds(req.key) {`; `checkUnsigned`: `deferred := !steady && gate.holds(key)`), `first_adoption.go` (`checkFirst`: `deferred, reason := gate.holds(req.key), gate.reason`). Afterwards `grep -n 'gate.active && !gate.due' internal/freshness/*.go | grep -v _test` prints nothing. The answered-request block stays repeated (E-22's reason: declined).

`check.go`, before `admit`:

```go
// signedForm reports whether form is a signed anchor form (inline or
// detached): what the reserved-slot rule (spec C4) and the scheduler's
// signed flag ask of a current snapshot. An unknown or empty form is not
// signed (deferred minor E-31: the one predicate; Reconcile used
// "not unsigned").
func signedForm(form cache.AnchorForm) bool {
	return form == cache.FormInline || form == cache.FormDetached
}
```

used by `baseline` (`signedCurrent = signedForm(snap.Form)`), by `tick` (`if signedForm(cache.AnchorForm(r.CurrentAnchorForm)) {`) and by `Reconcile`:

old:
```go
	// The reserved-slot rule (spec C4): only a signed current snapshot may
	// take the last free slot; a snapshot that cannot be read counts as
	// unsigned.
	signed := false
	if snap, err := c.cache.GetSuiteSnapshot(ctx, snapID); err == nil {
		signed = snap.Form != cache.FormUnsigned
	}
```
new:
```go
	// The reserved-slot rule (spec C4): only a signed current snapshot may
	// take the last free slot; a snapshot that cannot be read counts as
	// unsigned (fail-closed), logged (deferred minor E-31).
	signed := false
	if snap, err := c.cache.GetSuiteSnapshot(ctx, snapID); err != nil {
		c.logger.Warn("reconcile_snapshot_read_failed",
			"err", err, "snapshot_id", snapID,
			"canonical_scheme", scheme, "canonical_host", host, "suite_path", suitePath)
	} else {
		signed = signedForm(snap.Form)
	}
```

- [ ] **Step 4: The clock step below the cooldown (E-37), the comments (E-23, E-25, E-27), the two fields (E-30, GD-6)**

`checkLocked`: the block `// A last_success_at in the future made the suite due …` with its `if cur.LastSuccessAt != nil && … { c.clockStep("last_success_at", …) }` moves from above the cooldown gate to just below it (after the gate's closing brace, before `suite := SuiteRef{…}`), its comment reading:

```go
	// A last_success_at in the future made the suite due (suiteDue reads
	// it as infinitely old without reporting it on every tick, deferred
	// minor B-38). It is reported here, below the cooldown gate, so a
	// client check the cooldown turns away counts nothing: each check that
	// runs reports it until a successful answer rewrites the stamp
	// (deferred minor E-37).
```

E-23, `check.go`: the three `result, clear, ok := steadyResult(…)` (two in `check.go`, one in `current_form.go`'s `checkUnsigned`) become `result, clearRec, ok`, with `clearRec` read where `clear` was. `checkInline`'s doc lists the 3-tuples:

```go
// checkInline runs the conditional GET against InRelease and
// processes the result. Returns:
//   - (req, repair, false) once the check has ended (req is nil when
//     nothing is to be adopted, repair nil unless a member repair is
//     planned).
//   - (nil, nil, true) when the inline path got 404 AND the suite has no
```

(the rest of the doc unchanged). The comment under `} else if gate.holds(newHash) {` reads `// An upstream that ignores validators re-served the failing` / `// bytes: deferred, as steadyResult defers a 304 to the record's` / `// own validators.`. `checkDetached`'s steady-result comment reads:

```go
		// Upstream still serves what the suite has (steadyResult): a 304
		// — changed_deferred when the record's own validators were sent, a
		// re-signature behind them waiting for the retry (spec C7) — or a
		// 200 of the current Release. A record the lateral InRelease still
		// holds is not cleared by the detached anchor's "unchanged".
```

E-25, `scheduler.go` `suiteDue`, the existing `// Spec C4 Dispatch 3: …` comment above the interest test gains:

```go
	//
	// AIDEV-NOTE: deferred minor E-25, accepted. An interested never-adopted
	// suite without a client-created anchor or flat index row is still
	// queued at every tick (suiteDue reads no url_path row): its check
	// reads the suite row and its anchor rows (and a flat suite's metadata
	// rows) and ends dormant at DEBUG, with no request. Bounded by the tick
	// interval and the 7-day interest window; testing for the rows here
	// would put the same reads in the tick, for every such suite.
```

E-27, `probe.go` `probe`, above `return classify(res, err, sniff, false)`: `// false: a probe sends no validators, so a 304 is unknown (deferred` / `// minor E-27).`

E-30, `scheduler.go` `tick`:

old:
```go
	if warnStale := adoptedStale - absentStale; warnStale > 0 {
		c.logger.Warn("freshness: adopted suites stale beyond threshold",
			"adopted_stale", warnStale,
```
new:
```go
	// paced_stale is the count left out (deferred minor E-30): with
	// adopted_stale it adds up to the gauge's adopted="true" value.
	if warnStale := adoptedStale - absentStale; warnStale > 0 {
		c.logger.Warn("freshness: adopted suites stale beyond threshold",
			"adopted_stale", warnStale,
			"paced_stale", absentStale,
```

(`paced_stale`, not the item's suggested `absent_stale`: the count is every suite paced by its record — `anchor_absent` and `unsigned_disabled` — as `recordPacesAdopted` says.)

GD-6, `current_form.go` `anchorAbsent`:

old:
```go
		rows, _, err := c.cachedAnchors(ctx, suite)
		if err != nil {
			c.logger.Warn("freshness_read_anchor_rows_failed",
				"err", err, "canonical_host", suite.CanonicalHost, "suite_path", suite.SuitePath)
		}
```
new:
```go
		rows, _, err := c.cachedAnchors(ctx, suite)
		// Deferred minor GD-6: the scheme names the suite, and a read a
		// shutdown cut short is DEBUG, as checkFirst's is (ruling PG-35).
		if err != nil && !c.endedByShutdown(ctx, suite, "read the anchor rows", err) {
			c.logger.Warn("freshness_read_anchor_rows_failed",
				"err", err, "canonical_scheme", suite.CanonicalScheme,
				"canonical_host", suite.CanonicalHost, "suite_path", suite.SuitePath)
		}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -count=1 ./internal/freshness/ ./internal/admin/`
Expected: two `ok` lines.

Run: `go test -race -count=1 ./internal/freshness/ ./cmd/...`
Expected: all `ok`.

- [ ] **Step 6: Mutation checks** (one at a time, each reverted)

| Mutant | Fails |
|---|---|
| `baseline` ignores the snapshot hash for `Release` (`… h != nil && name != "Release"`) (E-21) | `check_path_test.go:51: result "changed", want unchanged …` |
| `holds` drops `!g.due` (E-32) | `check_path_test.go:77: due: holds = true, want false` |
| `probe` passes `conditional = true` (E-27, C3) | `check_path_test.go:96: probe = present (status 304, err <nil>), want unknown …` |
| `upgradeProbe` sets `req.crossForm = false` (E-28, X3) | `check_path_test.go:125: record validators "e-…" / "<nil>", want none` |
| `anchorFoundAgain` sets `req.crossForm = false` (E-28, X2) | `check_path_test.go:139: record validators "e-…" / "<nil>", want none` |
| the `paced_stale` field removed (E-30) | `check_path_test.go:165: stale WARNs [map[adopted_stale:1 …]], want one with adopted_stale 1 and paced_stale 1` |
| `signedForm` returns `form != cache.FormUnsigned` (E-31) | `check_path_test.go:177: signedForm("") = true, want false` |
| `Reconcile`'s failed read logged at DEBUG (E-31) | `check_path_test.go:204: WARN records [], want one …` |
| `Reconcile` counts a failed read as signed (E-31) | `check_path_test.go:198: a reconcile whose snapshot could not be read took the reserved slot` |
| the future-`last_success_at` report put back above the cooldown gate (E-37) | `check_path_test.go:230: a check inside the cooldown counted 1 clock steps, want 0` |
| `anchorAbsent`'s WARN without `canonical_scheme` (GD-6) | `check_path_test.go:297: WARN records [map[canonical_host:h …]], want one naming the scheme …` |
| `anchorAbsent` logs the read failure without `endedByShutdown` (GD-6) | `check_path_test.go:291: WARNs [map[… canonical_scheme:http …]], shutdown lines []; want no WARN and one DEBUG for the read` |

- [ ] **Step 7: Commit**

```bash
gofmt -l internal/freshness/check.go internal/freshness/current_form.go internal/freshness/first_adoption.go internal/freshness/freshness.go internal/freshness/probe.go internal/freshness/scheduler.go internal/freshness/check_path_test.go
git add internal/freshness/check.go internal/freshness/current_form.go internal/freshness/first_adoption.go internal/freshness/freshness.go internal/freshness/probe.go internal/freshness/scheduler.go internal/freshness/check_path_test.go
git commit -m "$(cat <<'EOF'
Check path: one hold and one signed predicate, quieter clock steps, pins

The spec C7 hold has one spelling, retryGate.holds (E-32), and a signed
current snapshot one predicate, signedForm, which Reconcile now shares;
its fail-closed snapshot read is logged reconcile_snapshot_read_failed
(E-31). A client check the cooldown turns away no longer counts a future
last_success_at as a clock step (E-37). The stale WARN carries
paced_stale (E-30); anchorAbsent's anchor-row read failure names the
scheme and is DEBUG at shutdown (GD-6). Tests pin the detached snapshot
baseline (E-21), the probe's unconditional 304 (E-27) and the cross-form
records' missing validators (E-28). Comments: checkInline's doc and two
stale comments (E-23), suiteDue's dormant-by-rows cost (E-25).

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

### Task 9: The scheduler's wiring and the panic boundaries (B-27, E-38, E-39, E-40, E-42, E-43, E-45, E-56)

Three panic boundaries report a panic with no second recover, so a panicking log handler escapes them (E-45: `recoverGoroutine`, `safeReschedule`, `safeTick`), and the adoption's boundary loses its counters and ring event along with the report (E-42: the report gets its own boundary, and the counters and ring event another, so neither can escape — review M-2); `checkWorker` open-codes `unloggedPanic`. The rest pin wiring no test reached (E-38, E-39, E-43, B-27) and fix stale docs (E-40) and a hard-coded tick (E-56). No log line or metric changes: `acu_freshness_unlogged_panics_total` already counts what a boundary cannot log.

**Files:**
- Create: `internal/freshness/scheduler_wiring_test.go`
- Modify: `internal/freshness/scheduler.go` (new `tickInterval`, `Run`, `schedItem.held`'s doc, `schedQueues.pending`'s doc, `enqueue`'s comment, `finish`'s doc, `safeReschedule`, `checkWorker`, `safeTick`), `internal/freshness/freshness.go` (`recoverGoroutine`), `internal/freshness/adoption_failure.go` (`recoverAdoption`), `internal/freshness/freshness_test.go` (`listHook`'s comment), `internal/freshness/deferred_e_test.go` (`TestAdoptionFailureRecordCutByShutdownIsNotAWarning`), `internal/freshness/dod_e_test.go` (the tick)

**Interfaces:**
- Consumes: test helpers `seedStaleAnchorAt`, `fakePackagesStanzas`, `signedRelease`, `passThroughVerifier`, `mustAcquire`, `seedFakeSuite`, `schedRow`, `admitNone`, `admitSigned`, `seedRecords`, `nextRetryOf`, `renderedMetricLabels`, `renderedFloat` (`reconcile_test.go`), `unlogged` (`deferred_e_test.go`; Task 16 moves it), `panicLogHandler`, `lockedBuffer`, `logRecords`; `Checker.offerHook`, `admission.freed`, `admission.tryAcquire`.
- Produces: `tickInterval(refresh time.Duration) time.Duration`; test helpers `escaped(f func()) any`, `adoptionCount(t *testing.T, outcome, host string) float64`, `panickyCache`.

- [ ] **Step 1: Write the failing tests**

Create `internal/freshness/scheduler_wiring_test.go`:

```go
package freshness

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// Deferred minor E-38 (B-45's wiring in Run): an offer that ends queued
// because someone else took the slot during its check, where that other
// admission ended — and its wake was spent — before the offer's done was
// handled, is offered again at once, not at the next tick: Run hands
// finish the admission generation at the done, not the offer's own.
func TestRun_AnOfferWhoseWakeWasSpentIsReofferedAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := simtest.OpenCache(t)
		up := simtest.NewUpstream()
		members := newFakeFetcher()
		seedStaleAnchorAt(t, c, "h", rigSuite)
		pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": testHex64})
		members.put("http://h"+rigSuite+"/main/binary-amd64/Packages", pkgs)
		anchor := "http://h" + rigSuite + "/InRelease"
		rel := signedRelease("v1", 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
			HostLimiter: hostsem.New(8), MaxConcurrent: 1, Logger: simtest.DiscardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		chk, err := New(Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8),
			Refresh: time.Hour, Cooldown: time.Hour, Adopter: ad, Logger: simtest.DiscardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		var (
			calls atomic.Int32
			taken atomic.Pointer[lease]
			once  sync.Once
		)
		up.Set(anchor, func(ctx context.Context, etag string) (*fetch.ConditionalResult, error) {
			if calls.Add(1) == 2 { // the first offer: another admission takes the slot meanwhile
				if l, ok := chk.admission.tryAcquire("other", true); ok {
					taken.Store(l)
				}
			}
			return simtest.Serve(rel)(ctx, etag)
		})
		chk.offerHook = func(SuiteRef, checkReport) {
			once.Do(func() {
				// The other admission ends before the offer's done is
				// handled, and Run never sees its wake (spent first).
				if l := taken.Load(); l != nil {
					l.release()
				}
				select {
				case <-chk.admission.freed:
				default:
				}
			})
		}
		ctx, cancel := context.WithCancel(context.Background())
		exited := make(chan struct{})
		go func() { chk.Run(ctx); close(exited) }()
		defer func() { cancel(); <-exited; chk.WaitForAdoptions() }()
		held := mustAcquire(t, chk.admission, "other", true)
		time.Sleep(16 * time.Minute) // the first tick (15 min): the suite waits
		released := time.Now()
		held.release() // the offer, whose check loses the slot
		time.Sleep(2 * time.Minute)
		got := up.Calls(anchor)
		if len(got) != 3 || taken.Load() == nil {
			t.Fatalf("%d anchor GETs (slot taken %v), want 3: the first tick, the lost offer and the re-offer before the next tick",
				len(got), taken.Load() != nil)
		}
		if d := got[2].Sub(released); d > checkDeadline {
			t.Fatalf("re-offered %v after the release, want at once", d)
		}
	})
}

// Deferred minor E-39: the tick's signed flag goes both ways — a waiter
// parked while its suite was signed is admitted as unsigned once its
// current snapshot is not (the promotion is TestTick_RefreshesTheWaitersSignedFlag).
func TestTick_DemotesAWaiterWhoseSnapshotIsNoLongerSigned(t *testing.T) {
	fc := newFakeCache()
	recent := time.Unix(1_800_000_000, 0).Add(-time.Hour).Unix()
	seedFakeSuite(fc, "a", "/1", &recent)
	id := int64(9)
	fc.putSnapshot(cache.SuiteSnapshot{SnapshotID: id, CanonicalScheme: "http", CanonicalHost: "a", SuitePath: "/1", Form: cache.FormUnsigned})
	s, _ := fc.suite("http", "a", "/1")
	s.CurrentSnapshotID = &id
	fc.putSuite(s)
	c, err := New(Config{Cache: fc, Fetcher: &changedUpstream{}, HostLimiter: hostsem.New(1), Refresh: time.Hour,
		Adopter: &Adopter{}, Logger: discardLogger(), now: func() time.Time { return time.Unix(1_800_000_000, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	q := newSchedQueues()
	one := int64(1)
	q.enqueue(schedRow("a", "/1", &one, 0), false)
	it, _ := q.pop(admitNone)
	q.finish(checkDone{item: it, report: checkReport{result: "changed_queued", signedCurrent: true}}, 0)
	if w := q.hosts["a"].waiting; len(w) != 1 || !w[0].signed {
		t.Fatalf("setup: waiting %+v, want one signed waiter", w)
	}
	c.tick(context.Background(), q)
	if it, ok := q.pop(admitSigned); ok {
		t.Fatalf("after the tick: offered %+v to the reserved slot, want the waiter demoted to unsigned", it)
	}
}

// Deferred minor B-27: the restart spread covers [now, now +
// periodic_refresh): rand 0 is now itself, rand 0.999 the last second of
// the window (TestRescheduleFailingAdoptions_SpreadsFailingSuitesOnly
// pins 0.5).
func TestRescheduleFailingAdoptions_SpreadBounds(t *testing.T) {
	for _, tc := range []struct {
		u    float64
		want int64 // seconds after now, over the rig's 15 min refresh
	}{{0, 0}, {0.999, 899}} {
		r := newRetryRig(t, 0)
		seedRecords(t, r, r.clock.now().Add(50*time.Minute).Unix(), map[string]string{"/fail/dists/x": "gpg_failed"})
		r.checker.rand = func() float64 { return tc.u }
		r.checker.rescheduleFailingAdoptions(context.Background())
		if got, want := nextRetryOf(t, r, "/fail/dists/x"), r.clock.now().Unix()+tc.want; got != want {
			t.Errorf("rand %v: next retry = %d, want now + %d", tc.u, got, tc.want)
		}
	}
}

// adoptionCount returns acu_adoption_total for outcome on host (0 before
// the series' first increment; a value that does not parse fails t).
func adoptionCount(t *testing.T, outcome, host string) float64 {
	t.Helper()
	return renderedFloat(t, renderedMetricLabels("acu_adoption_total", `outcome="`+outcome+`"`, `host="`+host+`"`))
}

// Deferred minor E-42: when the log handler panics while an adoption's
// panic is reported, only the report is lost (and counted unlogged): the
// attempt is still counted panicked, shown in the admin ring and recorded.
func TestAdoptionPanicWithABrokenLoggerIsStillCounted(t *testing.T) {
	r := newRetryRig(t, 0)
	body := r.release("v1", 0)
	r.ver.panicOn(hashOf(body))
	r.up.set(body)
	r.checker.logger = slog.New(panicLogHandler{panicOn: func(rec slog.Record) bool { return rec.Message == "adoption_panicked" }})
	before, unloggedBefore := adoptionCount(t, "panicked", rigHost), unlogged()
	r.check()
	if got := adoptionCount(t, "panicked", rigHost) - before; got != 1 {
		t.Fatalf("acu_adoption_total{outcome=panicked} rose by %v, want 1", got)
	}
	if got := unlogged() - unloggedBefore; got != 1 {
		t.Fatalf("unlogged panics rose by %v, want 1 (the report alone)", got)
	}
	ev := r.ring.Snapshot()
	if len(ev) == 0 || ev[len(ev)-1].Outcome != "panicked" {
		t.Fatalf("ring %+v, want the panicked attempt last", ev)
	}
	if strOf(r.row().AdoptionFailReason) != cache.ReasonPanic {
		t.Fatalf("record reason %q, want panic", strOf(r.row().AdoptionFailReason))
	}
}

// escaped runs f and returns the value of a panic that escaped it.
func escaped(f func()) (v any) {
	defer func() { v = recover() }()
	f()
	return nil
}

// Review M-2 of the H1 plan: a panic in the adoption boundary's counters
// or ring event — here the clock the ring event reads — is contained by
// its own boundary and counted unlogged; it never escapes the adoption
// goroutine (the record's own recover takes the clock's second panic).
func TestRecoverAdoption_APanickingRingEventIsContained(t *testing.T) {
	r := newRetryRig(t, 0)
	r.checker.now = func() time.Time { panic("clock bug") }
	req := &adoptionRequest{suite: SuiteRef{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite}, key: testHex64}
	before := unlogged()
	if v := escaped(func() {
		defer r.checker.recoverAdoption(req, time.Now())
		panic("adoption bug")
	}); v != nil {
		t.Fatalf("a panic escaped the adoption boundary: %v", v)
	}
	if got := unlogged() - before; got != 1 {
		t.Fatalf("unlogged panics rose by %v, want 1 (the ring event's)", got)
	}
}

// Deferred minor E-43 (B-18's second site): a panic while a failure record
// is written is reported — adoption_panicked, where=record — under its own
// boundary, so a log handler that panics too is contained and counted.
func TestRecordAdoptionFailure_RecoversAPanic(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(fmt.Sprintf("broken logger=%v", broken), func(t *testing.T) {
			r := newRetryRig(t, 0)
			r.checker.recordHook = func() { panic("record bug") }
			logs := &lockedBuffer{}
			var h slog.Handler = slog.NewJSONHandler(logs, nil)
			if broken {
				h = panicLogHandler{panicOn: func(rec slog.Record) bool { return rec.Message == "adoption_panicked" }}
			}
			r.checker.logger = slog.New(h)
			before := unlogged()
			req := &adoptionRequest{suite: SuiteRef{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite}, key: testHex64}
			if v := escaped(func() { r.checker.recordAdoptionFailure(req, "gpg_failed") }); v != nil {
				t.Fatalf("a panic escaped the record: %v", v)
			}
			if broken {
				if got := unlogged() - before; got != 1 {
					t.Fatalf("unlogged panics rose by %v, want 1", got)
				}
				return
			}
			if recs := logRecords(t, logs, "adoption_panicked"); len(recs) != 1 || recs[0]["where"] != "record" || recs[0]["level"] != "ERROR" {
				t.Fatalf("adoption_panicked records %v, want one ERROR where=record", recs)
			}
		})
	}
}

// panickyCache panics on the reads safeTick and safeReschedule guard.
type panickyCache struct{ *fakeCache }

func (panickyCache) ListSuitesWithAdoption(context.Context) ([]cache.SuiteWithAdoption, error) {
	panic("list bug")
}

func (panickyCache) RescheduleFailingAdoptions(context.Context, int64, func() int64) (int, error) {
	panic("reschedule bug")
}

// Deferred minor E-45: the repair and reconcile goroutines' boundary
// (recoverGoroutine), the restart reschedule's and the tick's report a
// panic under a second boundary, so a log handler that panics too never
// escapes them (into Run, which has none): the panic is counted unlogged.
func TestPanicBoundaries_ABrokenLoggerNeverEscapes(t *testing.T) {
	logger := slog.New(panicLogHandler{panicOn: func(r slog.Record) bool { return r.Level >= slog.LevelError }})
	c, err := New(Config{Cache: panickyCache{newFakeCache()}, Fetcher: &changedUpstream{}, HostLimiter: hostsem.New(1),
		Refresh: time.Hour, Adopter: &Adopter{}, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	suite := SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}
	for _, tc := range []struct {
		name string
		f    func()
	}{
		{"recoverGoroutine", func() { defer c.recoverGoroutine("repair", suite); panic("repair bug") }},
		{"safeReschedule", func() { c.safeReschedule(context.Background()) }},
		{"safeTick", func() { c.safeTick(context.Background(), newSchedQueues()) }},
	} {
		before := unlogged()
		if v := escaped(tc.f); v != nil {
			t.Errorf("%s: a panic escaped: %v", tc.name, v)
			continue
		}
		if got := unlogged() - before; got != 1 {
			t.Errorf("%s: unlogged panics rose by %v, want 1", tc.name, got)
		}
	}
}
```

In `internal/freshness/deferred_e_test.go`, `TestAdoptionFailureRecordCutByShutdownIsNotAWarning` asserts exactly one DEBUG line (E-43): its doc's second line reads `// is not a WARN: it is logged once, at DEBUG (deferred minor E-43).`, and

old:
```go
	for _, rec := range r.events("adoption_failure_record_failed") {
		if rec["level"] == "WARN" {
			t.Fatalf("a record cut short by shutdown logged WARN: %v", rec)
		}
	}
```
new:
```go
	recs := r.events("adoption_failure_record_failed")
	if len(recs) != 1 || recs[0]["level"] != "DEBUG" {
		t.Fatalf("adoption_failure_record_failed lines %v; want one at DEBUG for the write shutdown cut short", recs)
	}
```

(It still passes a demotion keyed on the write's context instead of the lifetime's: that context derives from the lifetime's, so no test tells them apart without a seam — noted, not chased.)

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 -run 'TestRun_AnOfferWhoseWake|TestTick_DemotesAWaiter|TestRescheduleFailingAdoptions_SpreadBounds|TestAdoptionPanicWithABrokenLoggerIsStillCounted|TestRecordAdoptionFailure_RecoversAPanic|TestPanicBoundaries_' ./internal/freshness/`
Expected: FAIL with
```
scheduler_wiring_test.go:159: acu_adoption_total{outcome=panicked} rose by 0, want 1
scheduler_wiring_test.go:265: recoverGoroutine: a panic escaped: log handler bug: adoption_panicked
scheduler_wiring_test.go:265: safeReschedule: a panic escaped: log handler bug: freshness_check_panicked
scheduler_wiring_test.go:265: safeTick: a panic escaped: log handler bug: freshness_check_panicked
```
The other five pin wiring that holds (E-38, E-39, E-43's record site, B-27, and review M-2's ring event, which today's single boundary contains): Step 6's mutants are their red.

- [ ] **Step 3: The boundaries (E-42, E-45)**

`adoption_failure.go` `recoverAdoption`:

old:
```go
	// The reporting runs under its own boundary (B-18): a panic there must
	// neither escape nor skip the record below.
	unloggedPanic(func() {
		c.reportPanic("adoption_panicked", "adoption", req.suite, r)
		d := time.Since(start)
		host := req.suite.CanonicalHost
		adoptionTotal.Inc("panicked", host)
		adoptionDurationSeconds.Observe(d.Seconds(), "panicked", host)
		if c.adoptionRing != nil {
			c.adoptionRing.Record(observability.AdoptionEvent{
				Scheme: req.suite.CanonicalScheme, Host: host, SuitePath: req.suite.SuitePath, Outcome: "panicked", Reason: cache.ReasonPanic,
				CompletedUnixSec: c.now().Unix(), DurationSeconds: d.Seconds(),
			})
		}
	})
	c.recordAdoptionFailure(req, cache.ReasonPanic)
```
new:
```go
	// The report, then the counters and the ring event, each run under
	// their own boundary (B-18, deferred minor E-42; review M-2 of the H1
	// plan): a panicking log handler must not skip the counters, and
	// neither may escape the adoption goroutine or skip the record below,
	// which recovers its own panics.
	unloggedPanic(func() { c.reportPanic("adoption_panicked", "adoption", req.suite, r) })
	unloggedPanic(func() {
		d := time.Since(start)
		host := req.suite.CanonicalHost
		adoptionTotal.Inc("panicked", host)
		adoptionDurationSeconds.Observe(d.Seconds(), "panicked", host)
		if c.adoptionRing != nil {
			c.adoptionRing.Record(observability.AdoptionEvent{
				Scheme: req.suite.CanonicalScheme, Host: host, SuitePath: req.suite.SuitePath, Outcome: "panicked", Reason: cache.ReasonPanic,
				CompletedUnixSec: c.now().Unix(), DurationSeconds: d.Seconds(),
			})
		}
	})
	c.recordAdoptionFailure(req, cache.ReasonPanic)
```

`freshness.go` `recoverGoroutine`: its last line becomes `unloggedPanic(func() { c.reportPanic("adoption_panicked", where, suite, r) })`, and its doc's paragraph ends `… before the mutex is released (spec C5). The report runs under its own boundary (unloggedPanic, deferred minor E-45): a panicking log handler must not escape it.` `scheduler.go`: in `safeReschedule` and `safeTick` the `c.reportPanic(…)` inside the deferred recover becomes `unloggedPanic(func() { c.reportPanic(…) })` (arguments unchanged); in `checkWorker`, the open-coded

```go
			func() {
				defer func() {
					if recover() != nil {
						freshnessUnloggedPanics.Inc()
					}
				}()
				rep = c.checkRecovered(ctx, item)
			}()
```

becomes `unloggedPanic(func() { rep = c.checkRecovered(ctx, item) })` (same behaviour; its AIDEV-NOTE stands).

- [ ] **Step 4: The tick helper and the docs (E-56, E-40)**

`scheduler.go`, after `minFastTickInterval`:

```go
// tickInterval is the scheduler's tick for refresh: SPEC §7.4's fast
// tick, refresh / 4, floored at minFastTickInterval. Run's ticker and the
// tests that wait for a tick read it here (deferred minor E-56).
func tickInterval(refresh time.Duration) time.Duration {
	return max(refresh/4, minFastTickInterval)
}
```

`Run`'s four lines computing `interval` become `interval := tickInterval(c.refresh)`, and `dod_e_test.go`'s `const tick = 15 * time.Minute / 4 // the scheduler's tick: periodic_refresh / 4` becomes `tick := tickInterval(15 * time.Minute) // the scheduler's own tick (deferred minor E-56)`.

E-40: `schedItem.held`'s comment reads `// waiting, but its last offer found the suite busy, or no slot and no release since its dispatch (B-45): not offered until unhold`. `schedQueues.pending`'s comment:

```go
	// pending holds the keys of queued suites a later tick found with a
	// pending change (deferred minor B-66). enqueue marks every duplicate,
	// waiting and in-flight ones too; pop moves a class-queued suite's mark
	// to its item (pendingChange), and done drops whatever mark is left
	// when the suite's turn ends, so a mark never reaches a later turn.
```

`enqueue`'s `A waiting or in-flight suite ignores the mark (pop and done drop it).` reads `A waiting or in-flight suite ignores the mark; done drops it when the suite's turn ends.` `finish`'s doc:

```go
// finish files a finished check (spec C4 slot-driven re-offer): a queued
// result parks the suite to wait for a slot — keeping its place if it was
// already waiting (a lost offer), held until the next wake or tick unless
// a slot was released after the offer was dispatched (gen, the admission
// generation now, differs from the item's: B-45), which parks it unheld,
// offered at once; an offer that found the suite busy (another goroutine
// holds its mutex) parks it held too — re-offering at once would spin
// while the mutex is held; any other result ends the suite's turn.
```

`freshness_test.go`: `listHook`'s comment names `ListSuitesWithAdoption`.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -count=1 ./internal/freshness/`
Expected: `ok`.

Run: `go test -race -count=1 ./internal/freshness/`
Expected: `ok`. `TestRun_AnOfferWhoseWakeWasSpentIsReofferedAtOnce` is a synctest sim: under `-race` it takes well under the 4.5 s budget (measured in Task 13's R-H4 table).

- [ ] **Step 6: Mutation checks** (one at a time, each reverted)

| Mutant | Fails |
|---|---|
| `Run` passes `q.finish(d, d.item.gen)` (E-38) | `scheduler_wiring_test.go:82: 2 anchor GETs (slot taken true), want 3: …` |
| `refreshSigned` never demotes (`… = h.waiting[i].signed \|\| signed[…]`) (E-39) | `scheduler_wiring_test.go:118: after the tick: offered {… signed:true …} to the reserved slot, want the waiter demoted to unsigned` |
| the restart offset `+ 1` (B-27, lower bound) | `scheduler_wiring_test.go:136: rand 0: next retry = 1800000001, want now + 0` |
| the restart offset over `spread+1` (B-27, upper bound) | `scheduler_wiring_test.go:136: rand 0.999: next retry = 1800000900, want now + 899` |
| the panicked counter back under the report's boundary (E-42) | `scheduler_wiring_test.go:159: acu_adoption_total{outcome=panicked} rose by 0, want 1` |
| the counters and the ring event outside any boundary (review M-2: E-42's report-only boundary of the draft) | `scheduler_wiring_test.go:193: a panic escaped the adoption boundary: clock bug` |
| the record site's bare `reportPanic` (E-43) | `scheduler_wiring_test.go:217: a panic escaped the record: log handler bug: adoption_panicked` |
| the record write's `adoption_failure_record_failed` line removed (E-43) | `deferred_e_test.go:143: adoption_failure_record_failed lines []; want one at DEBUG …` |
| `recoverGoroutine`'s bare `reportPanic` (E-45) | `scheduler_wiring_test.go:265: recoverGoroutine: a panic escaped: log handler bug: adoption_panicked` |
| `safeReschedule`'s bare `reportPanic` (E-45) | `scheduler_wiring_test.go:265: safeReschedule: a panic escaped: …` |
| `safeTick`'s bare `reportPanic` (E-45) | `scheduler_wiring_test.go:265: safeTick: a panic escaped: …` |

E-38's other mutant — dropping the generation stamp at dispatch (`withGen`) — survives: it shows only when the peek before an offer was wrong, which no test can stage without a seam in `admission` (noted for the reviewer; the stamp is one line beside its AIDEV-NOTE).

- [ ] **Step 7: Commit**

```bash
gofmt -l internal/freshness/scheduler.go internal/freshness/freshness.go internal/freshness/adoption_failure.go internal/freshness/scheduler_wiring_test.go internal/freshness/deferred_e_test.go internal/freshness/dod_e_test.go internal/freshness/freshness_test.go
git add internal/freshness/scheduler.go internal/freshness/freshness.go internal/freshness/adoption_failure.go internal/freshness/scheduler_wiring_test.go internal/freshness/deferred_e_test.go internal/freshness/dod_e_test.go internal/freshness/freshness_test.go
git commit -m "$(cat <<'EOF'
Scheduler: second panic boundaries; pin B-45's wiring and the demotion

recoverGoroutine, safeReschedule and safeTick report a panic under
unloggedPanic, so a panicking log handler no longer escapes into Run;
checkWorker uses the helper it open-coded (E-45). recoverAdoption puts
the report under one boundary and the counters and ring event under
another, so they survive a broken logger (E-42) and their own panic
never escapes the adoption goroutine (review M-2). Tests pin Run's B-45
wiring (E-38), refreshSigned's demotion (E-39), the record site's
boundary and the shutdown DEBUG line (E-43), and the restart spread's
bounds (B-27). tickInterval is the scheduler's tick for Run and tests
(E-56); stale scheduler docs fixed (E-40).

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

### Task 10: Members, the served anchor and Sources (E-10, E-11, E-13, E-14, GD-2)

A member's fetch destination stops exposing `Finalize` (E-10); a SHA512-only size mismatch and the release parser's two hash-block errors get exact tests (E-11); the served-anchor read before the flip barrier logs DEBUG when a shutdown cut it short (E-13, design note 21), and it and the startup marker pass's skipped anchor get the tests they lacked (E-14); `ParseSources` counts the paths `validateMemberPath` refuses and the adoption says so once per `Sources` file, at INFO, since the line repeats at every adoption and the operator cannot act on it (GD-2, design note 12, review M-8). Task 18 adds `source_paths_skipped` and `adoption_served_anchor_read_failed`'s DEBUG case to `docs/log-fields.md` and the line to spec C5, which already says `ParseSources` keeps `validateMemberPath` (revision 10; review sweep M-2).

**Files:**
- Create: `internal/freshness/adoption_paths_test.go`
- Modify: `internal/freshness/member_verify.go` (`memberWriter`, `verifyMember`), `internal/freshness/flip_barrier.go` (imports, `servedAnchorUnchanged`), `internal/freshness/sources_parse.go` (`SourcesParseStats`, `ParseSources`), `internal/freshness/adoption.go` (`buildSourceHashes`)

**Interfaces:**
- Consumes: test helpers `newAdoptionTestEnv`, `sha512Of`, `shaOf`, `lockedBuffer`, `logRecords`, `newFakeFetcher`, `passThroughVerifier`, `fakeSourcesStanzas`, `fakeSourceStanza`, `makeRelease`, `trackSuite`.
- Produces: `memberWriter{blob *cache.BlobWriter; sha512 hash.Hash}` with methods `Write`, `Written`, `Abort`, `Truncate` (no `Finalize*`, no `HashSum`); `SourcesParseStats.PathsSkipped int`, `SourcesParseStats.FirstSkipped string`; WARN `source_paths_skipped` (`canonical_scheme`, `canonical_host`, `suite_path`, `snapshot_id`, `member_path`, `count`, `first`); `adoption_served_anchor_read_failed` at DEBUG on shutdown.

- [ ] **Step 1: Write the failing tests**

Create `internal/freshness/adoption_paths_test.go`:

```go
package freshness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
)

// Deferred minor E-10: a memberWriter is a fetch destination that cannot
// finalize itself — only verifyMember moves its bytes into pool/.
func TestMemberWriter_HasNoFinalize(t *testing.T) {
	var _ fetch.FetchDst = (*memberWriter)(nil)
	typ := reflect.TypeOf(&memberWriter{})
	for _, name := range []string{"Finalize", "FinalizeExpectingHash"} {
		if _, ok := typ.MethodByName(name); ok {
			t.Errorf("memberWriter has %s: a caller could skip verifyMember", name)
		}
	}
}

// Deferred minor E-11: a SHA512-only member whose bytes match the hash but
// not the declared size fails (Finalize's size check) and leaves nothing
// in the pool.
func TestVerifyMember_SHA512OnlySizeMismatch(t *testing.T) {
	env := newAdoptionTestEnv(t)
	body := []byte("Package: p\n")
	w, err := newMemberWriter(env.cache)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	_, err = verifyMember(w, ReleaseMember{Path: "p", SHA512: sha512Of(body)}, int64(len(body))+1, nil)
	if !errors.Is(err, cache.ErrSizeMismatch) {
		t.Fatalf("err = %v, want cache.ErrSizeMismatch", err)
	}
	if exists, xerr := env.cache.BlobExists(shaOf(body)); xerr != nil || exists {
		t.Fatalf("in pool %v (%v), want nothing", exists, xerr)
	}
}

// Deferred minor E-11: ParseRelease's two hash-block errors, in full (they
// were pinned by a prefix only), for each block kind.
func TestParseRelease_HashBlockErrors(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"no block", "Origin: Ubuntu\nMD5Sum:\n 0123456789abcdef0123456789abcdef 4533 x\n", "release: no SHA256 or SHA512 block"},
		{"empty SHA256", "Origin: Ubuntu\nSHA256:\nDescription: foo\n", "release: empty SHA256 and SHA512 blocks"},
		{"empty SHA512", "Origin: Ubuntu\nSHA512:\nDescription: foo\n", "release: empty SHA256 and SHA512 blocks"},
	} {
		if _, err := ParseRelease([]byte(tc.body)); err == nil || err.Error() != tc.want {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

// Deferred minors E-13 and E-14 (C-16): a failed read of the served anchor
// before the flip barrier is logged at WARN — and at DEBUG when a shutdown
// cut it short; either way the barrier runs (false).
func TestServedAnchorUnchanged_ReadFailure(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprintf("shutdown=%v", shutdown), func(t *testing.T) {
			c, err := cache.Open(context.Background(), t.TempDir(), discardLogger())
			if err != nil {
				t.Fatal(err)
			}
			logs := &lockedBuffer{}
			a, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: newFakeFetcher(), Verifier: passThroughVerifier{},
				HostLimiter: hostsem.New(8), Logger: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
			if err != nil {
				t.Fatal(err)
			}
			p := &adoptionPayload{form: adoptionFormInline, inlineHash: testHex64}
			if shutdown {
				stop, cancel := context.WithCancel(context.Background())
				cancel()
				p.stop = stop
			} else {
				_ = c.Close() // a read that fails for another reason than shutdown
			}
			suite := SuiteRef{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite}
			if a.servedAnchorUnchanged(context.Background(), suite, p) {
				t.Fatal("a failed read answered unchanged: the barrier would be skipped")
			}
			want := "WARN"
			if shutdown {
				want = "DEBUG"
			}
			recs := logRecords(t, logs, "adoption_served_anchor_read_failed")
			if len(recs) != 1 || recs[0]["level"] != want || recs[0]["path"] != rigSuite+"/InRelease" {
				t.Fatalf("records %v, want one at %s naming the anchor", recs, want)
			}
		})
	}
}

// Deferred minor E-14 (A-49): the startup marker pass says, at DEBUG,
// which cached anchor it skipped because its pool file could not be read.
func TestMarkFromCachedSignedAnchors_LogsASkippedAnchor(t *testing.T) {
	ctx := context.Background()
	c, err := cache.Open(ctx, t.TempDir(), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	// A tracked suite's InRelease row, last used at 1, whose blob row has no
	// pool file.
	stale, used := testHex64, int64(1)
	if err := c.PutBlob(ctx, stale, 1); err != nil {
		t.Fatal(err)
	}
	if err := c.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: rigHost, Path: rigSuite + "/InRelease",
		BlobHash: &stale, UpstreamURL: "http://h/r/dists/x/InRelease", IsMetadata: true, LastFetchedAt: &used}); err != nil {
		t.Fatal(err)
	}
	if err := c.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite}); err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuffer{}
	n, err := markFromCachedSignedAnchors(ctx, c, slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil || n != 0 {
		t.Fatalf("markers = %d, %v; want none", n, err)
	}
	recs := logRecords(t, logs, "legacy_trust_audit_cached_anchor_skipped")
	if len(recs) != 1 || recs[0]["level"] != "DEBUG" || recs[0]["path"] != rigSuite+"/InRelease" || recs[0]["err"] == nil {
		t.Fatalf("records %v, want one DEBUG naming the anchor and the error", recs)
	}
}

// Deferred minor GD-2 (ruling PG-21): ParseSources keeps validateMemberPath
// for a stanza's Directory and its files, and counts what it leaves out —
// every file of a stanza whose Directory fails, each file whose own path
// fails — keeping the first.
func TestParseSources_CountsTheSkippedPaths(t *testing.T) {
	h := strings.Repeat("a", 64)
	text := "Package: a\nDirectory: ./pool/a\nChecksums-Sha256:\n " + h + " 1 a_1.dsc\n " + h + " 2 a_1.tar.gz\n\n" +
		"Package: b\nDirectory: pool/main/b/b\nChecksums-Sha256:\n " + h + " 3 b_1.dsc\n " + h + " 4 b_1%2b.tar.gz\n"
	refs, stats, err := ParseSources([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Path != "pool/main/b/b/b_1.dsc" {
		t.Fatalf("refs %+v, want b_1.dsc alone", refs)
	}
	if stats.PathsSkipped != 3 || stats.FirstSkipped != "./pool/a/a_1.dsc" {
		t.Fatalf("skipped %d, first %q; want 3 and ./pool/a/a_1.dsc", stats.PathsSkipped, stats.FirstSkipped)
	}
}

// Deferred minor GD-2: an adoption whose Sources file had paths skipped
// says so once for that file (source_paths_skipped, INFO: review M-8);
// the suite
// still adopts with the rows that passed.
func TestAdopter_SourcesWithSkippedPathsLogsOnce(t *testing.T) {
	logs := &lockedBuffer{}
	c, err := cache.Open(context.Background(), t.TempDir(), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ff := newFakeFetcher()
	ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: ff, Verifier: passThroughVerifier{}, HostLimiter: hostsem.New(8),
		Logger: slog.New(slog.NewJSONHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	suite := SuiteRef{CanonicalScheme: "http", CanonicalHost: "archive.ubuntu.com", SuitePath: "/ubuntu/dists/noble"}
	srcBody := fakeSourcesStanzas([]fakeSourceStanza{
		{pkg: "bash", directory: "pool/main/b/bash", files: map[string]string{"bash_5.1-2.dsc": strings.Repeat("a", 64)}},
		{pkg: "odd", directory: "pool/main/o/odd%2b", files: map[string]string{"odd_1.dsc": strings.Repeat("b", 64)}},
	})
	releaseText, _ := makeRelease(map[string][]byte{"main/source/Sources": srcBody})
	ff.put("http://archive.ubuntu.com/ubuntu/dists/noble/main/source/Sources", srcBody)
	trackSuite(t, c, suite)
	if err := ad.Run(context.Background(), suite, releaseText, "etag-1", ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	recs := logRecords(t, logs, "source_paths_skipped")
	if len(recs) != 1 || recs[0]["level"] != "INFO" || recs[0]["member_path"] != "main/source/Sources" ||
		recs[0]["count"] != float64(1) || recs[0]["first"] != "pool/main/o/odd%2b/odd_1.dsc" || recs[0]["canonical_scheme"] != "http" {
		t.Fatalf("source_paths_skipped records %v, want one INFO for the Sources file, count 1, first the odd path", recs)
	}
	sf, err := c.GetSuiteFreshness(context.Background(), "http", suite.CanonicalHost, suite.SuitePath)
	if err != nil || sf.CurrentSnapshotID == nil {
		t.Fatalf("not adopted (%v)", err)
	}
	if _, err := c.GetPackageHash(context.Background(), "http", suite.CanonicalHost, "/ubuntu/pool/main/b/bash/bash_5.1-2.dsc", *sf.CurrentSnapshotID); err != nil {
		t.Fatalf("the valid source row: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -gcflags=-e -count=1 -run 'TestMemberWriter_HasNoFinalize' ./internal/freshness/`
Expected: build failure naming `stats.PathsSkipped undefined (type SourcesParseStats has no field or method PathsSkipped)` and `stats.FirstSkipped undefined` (adoption_paths_test.go:153–154). Once it builds before Step 3, `TestMemberWriter_HasNoFinalize`, `TestServedAnchorUnchanged_ReadFailure/shutdown=true` and the two GD-2 tests fail; the E-11 and E-14 tests pin behaviour that holds (Step 6's mutants are their red).

- [ ] **Step 3: The member writer (E-10)**

`member_verify.go`: `memberWriter` holds the BlobWriter in a field:

```go
// memberWriter is the fetch destination of a member: the cache's
// BlobWriter (which hashes SHA-256 as it writes) with a SHA-512 hasher
// beside it, so a SHA-512 declaration is checked before the bytes reach
// pool/.
//
// AIDEV-NOTE: the BlobWriter is a field, never embedded (deferred minor
// E-10): memberWriter's method set has no Finalize or
// FinalizeExpectingHash, so only verifyMember — which checks every
// declared hash first — can move a member's bytes into pool/.
type memberWriter struct {
	blob   *cache.BlobWriter
	sha512 hash.Hash
}
```

`newMemberWriter` returns `&memberWriter{blob: w, sha512: sha512.New()}`; `Write` and `Truncate` call `w.blob.Write` and `w.blob.Truncate`; two forwarders join them, after `Write`:

```go
// Written is the byte count written so far (fetch.FetchDst).
func (w *memberWriter) Written() int64 { return w.blob.Written() }

// Abort removes the temp blob; a no-op once verifyMember finalized it.
func (w *memberWriter) Abort() error { return w.blob.Abort() }
```

and `verifyMember` reaches the BlobWriter through the field: `rehashPooled(w.blob.HashSum())`, `return w.blob.Finalize(size)`, `got, err := w.blob.FinalizeExpectingHash(m.SHA256, size)`. `adoption.go`'s two callers (`tryByHashFetch`, `fetchMemberURL`) use only `Abort`, `Written` and the `fetch.FetchDst` methods, and compile unchanged.

- [ ] **Step 4: The served anchor's level (E-13) and the skipped Sources paths (GD-2)**

`flip_barrier.go`: imports gain `"log/slog"`. In `servedAnchorUnchanged`'s doc, `one other than "no row" is logged (deferred minor C-16).` reads `one other than "no row" is logged (deferred minor C-16): at WARN, or at DEBUG when a shutdown cut the read short, as the check's local steps are (deferred minor E-13, FW-3 (d)).`, and

old:
```go
	if err != nil && !errors.Is(err, cache.ErrNotFound) {
		a.logger.Warn("adoption_served_anchor_read_failed",
			"canonical_host", suite.CanonicalHost, "suite_path", suite.SuitePath, "path", path, "err", err)
	}
```
new:
```go
	if err != nil && !errors.Is(err, cache.ErrNotFound) {
		level := slog.LevelWarn
		if p.stopErr(ctx) != nil || errors.Is(err, context.Canceled) {
			level = slog.LevelDebug
		}
		a.logger.Log(ctx, level, "adoption_served_anchor_read_failed",
			"canonical_host", suite.CanonicalHost, "suite_path", suite.SuitePath, "path", path, "err", err)
	}
```

`sources_parse.go`: `SourcesParseStats` gains

```go
	// PathsSkipped counts the file paths (<Directory>/<filename>) left
	// out because the Directory or the path failed validateMemberPath
	// (deferred minor GD-2: kept narrower than validateFilename, ruling
	// PG-21), and FirstSkipped is the first of them, so the caller can
	// say so (source_paths_skipped) instead of dropping them silently.
	PathsSkipped int
	FirstSkipped string
```

`ParseSources`'s doc bullet on `validateMemberPath` ends `… for an unsafe suite-relative path). Such paths — every file of a stanza whose Directory fails, and each file whose own path fails — are counted in SourcesParseStats.PathsSkipped (deferred minor GD-2).` In `flush`, the early `if err := validateMemberPath(directory); err != nil { return nil }` becomes `dirOK := validateMemberPath(directory) == nil`, and the per-entry path check:

old:
```go
			full := directory + "/" + e.filename
			if err := validateMemberPath(full); err != nil {
				continue
			}
```
new:
```go
			full := directory + "/" + e.filename
			if !dirOK || validateMemberPath(full) != nil {
				stats.PathsSkipped++
				if stats.FirstSkipped == "" {
					stats.FirstSkipped = full
				}
				continue
			}
```

(the same rows as before; a file whose hash or filename is malformed is still dropped before this, uncounted).

`adoption.go` `buildSourceHashes`: its doc's `SPEC6_5 §11 H3/H4/H11 per-stanza skips happen inside ParseSources silently — … per-stanza visibility is a future phase.` reads `SPEC6_5 §11 H3/H4 per-stanza skips happen inside ParseSources silently — operators see the per-Sources-file granularity at the source_parsed Debug log; the H11 skips (a path validateMemberPath refuses) are counted and logged once per Sources file at INFO, source_paths_skipped (deferred minor GD-2; INFO, review M-8: it repeats at every adoption and the operator cannot act on it).` After the `source_parsed` DEBUG call:

```go
		if stats.PathsSkipped > 0 {
			// Ruling PG-21 keeps validateMemberPath for a Sources file's
			// paths: one with a "%" or a "." segment gets no source row.
			a.logger.Info("source_paths_skipped",
				"canonical_scheme", suite.CanonicalScheme,
				"canonical_host", suite.CanonicalHost,
				"suite_path", suite.SuitePath,
				"snapshot_id", snapshotID,
				"member_path", m.Path,
				"count", stats.PathsSkipped,
				"first", stats.FirstSkipped,
			)
		}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -count=1 ./internal/freshness/`
Expected: `ok`.

Run: `go test -race -count=1 ./internal/freshness/`
Expected: `ok`.

- [ ] **Step 6: Mutation checks** (one at a time, each reverted)

| Mutant | Fails |
|---|---|
| a `Finalize` method forwarded on `memberWriter` (E-10) | `adoption_paths_test.go:24: memberWriter has Finalize: a caller could skip verifyMember` |
| the SHA512-only branch calls `w.blob.Finalize(0)` (E-11) | `adoption_paths_test.go:44: err = <nil>, want cache.ErrSizeMismatch` |
| `release: empty SHA256 blocks` as the second message (E-11) | `adoption_paths_test.go:60: empty SHA256: err = release: empty SHA256 blocks, want "release: empty SHA256 and SHA512 blocks"` |
| the shutdown demotion removed (E-13) | `adoption_paths_test.go:99: records [map[… level:WARN …]], want one at DEBUG naming the anchor` |
| the served-anchor line sent to a discarding logger (E-14, C-16) | `adoption_paths_test.go:99: records [], want one at WARN naming the anchor` (and at DEBUG) |
| the marker pass's skipped-anchor line sent to a discarding logger (E-14, A-49) | `adoption_paths_test.go:134: records [], want one DEBUG naming the anchor and the error` |
| a failed `Directory` skips its files uncounted (GD-2) | `adoption_paths_test.go:154: skipped 1, first "pool/main/b/b/b_1%2b.tar.gz"; want 3 and ./pool/a/a_1.dsc` |
| `source_paths_skipped` never logged (GD-2) | `adoption_paths_test.go:188: source_paths_skipped records [], want one INFO …` |
| `source_paths_skipped` back at WARN (review M-8) | `adoption_paths_test.go:188: source_paths_skipped records [map[… level:WARN …]], want one INFO …` |

- [ ] **Step 7: Commit**

```bash
gofmt -l internal/freshness/member_verify.go internal/freshness/flip_barrier.go internal/freshness/sources_parse.go internal/freshness/adoption.go internal/freshness/adoption_paths_test.go
git add internal/freshness/member_verify.go internal/freshness/flip_barrier.go internal/freshness/sources_parse.go internal/freshness/adoption.go internal/freshness/adoption_paths_test.go
git commit -m "$(cat <<'EOF'
Adoption: unfinalizable member writer, quiet served-anchor shutdown, Sources skips

memberWriter holds its BlobWriter in a field, so only verifyMember can
finalize a member (E-10). The served-anchor read before the flip barrier
logs DEBUG when a shutdown cut it short (E-13), and it and the marker
pass's skipped anchor are now asserted (E-14). ParseSources keeps
validateMemberPath (ruling PG-21) but counts the paths it refuses, and
the adoption logs INFO source_paths_skipped once per Sources file (GD-2;
INFO, review M-8: it repeats at every adoption).
Tests pin a SHA512-only size mismatch and the parser's two hash-block
errors in full (E-11).

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

### Task 11: The strict wait (E-58, E-59, E-62, E-66)

A request that arrives as a host's last adoption ends, while that adoption's 64 waiters still unwind, is refused for the waiter cap instead of re-running its lookup (E-58: the checks are reordered; E-62 renames the `max` parameter in the same function). Two strict-wait mutants survived (E-59), and one handler test depended on a real 300 ms window (E-66). No log line, metric or document changes.

**Files:**
- Modify: `internal/freshness/inflight.go` (`hostAdoptions.wait`), `internal/freshness/inflight_test.go` (a new test), `internal/handler/strict_wait_test.go` (`TestServeHTTP_StrictWaitRefusals`'s first loop; a new test), `internal/handler/seed_t1_test.go` (`TestServeHTTP_CoalescedCreatingMissFiresT1Once`)

**Interfaces:**
- Consumes: handler test helpers `strictWaitRig`, `fakeAdoptions`, `writeBlob`, `proxyReq`, `waitDebPath`, `holdHostSlotsUntilJoined`, `splitURL`, `hostKey`, `newSuiteRig`, `slowUpstream`, `staticUpstream`, `rowInRelease`.
- Produces: nothing new (`hostAdoptions.wait(ctx, host, maxWait)` keeps its signature but the parameter's name).

- [ ] **Step 1: Write the failing tests**

`internal/freshness/inflight_test.go`, before `TestChecker_CountsSignedAdoptionsInFlight`:

```go
// Deferred minor E-58: with nothing in flight a request re-runs its lookup
// at once even while the waiter cap is reached — the host's last adoption
// ended and its 64 waiters are still unwinding.
func TestHostAdoptions_NothingInFlightBeatsTheWaiterCap(t *testing.T) {
	h := newHostAdoptions()
	h.waiters["a"] = strictWaitersPerHost // an ended adoption's waiters, not yet unwound
	if !h.wait(context.Background(), "a", strictWait) {
		t.Fatal("a request with nothing in flight was refused for the waiter cap")
	}
}
```

`internal/handler/strict_wait_test.go`, `TestServeHTTP_StrictWaitRefusals`'s first loop (E-59, the retried refusal's count):

old:
```go
				if rec["msg"] == "unvouched_deb_refused" {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("%d unvouched_deb_refused events, want 1", n)
			}
```
new:
```go
				if rec["msg"] == "unvouched_deb_refused" {
					n++
					// Deferred minor E-59: the refusal after a wait carries
					// the count its first attempt read (the rig's one
					// snapshot), as the immediate refusal does.
					if rec["current_snapshot_count"] != float64(1) {
						t.Fatalf("unvouched_deb_refused current_snapshot_count = %v, want 1", rec["current_snapshot_count"])
					}
				}
			}
			if n != 1 {
				t.Fatalf("%d unvouched_deb_refused events, want 1", n)
			}
```

and at the end of the file (E-59, the hit path's pre-check):

```go
// Deferred minor E-59: on the hit path, a refusal with nothing in flight
// on the host does not wait (AdoptionsInFlight is asked first) and is
// immediate.
func TestServeHTTP_StrictHitWithNothingInFlightDoesNotWait(t *testing.T) {
	a := &fakeAdoptions{inFlight: false, ok: true}
	h, _, srvURL, scheme, canon := strictWaitRig(t, a)
	deb := []byte("a cached .deb")
	blob := writeBlob(t, h, deb)
	if err := h.cache.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: scheme, CanonicalHost: canon,
		Path: waitDebPath, BlobHash: &blob, UpstreamURL: srvURL + waitDebPath}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srvURL, waitDebPath))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want the strict-mode refusal", rec.Code)
	}
	if n := a.waits.Load(); n != 0 {
		t.Fatalf("waited %d times, want 0 (nothing in flight)", n)
	}
}
```

`internal/handler/seed_t1_test.go` (E-66):

old:
```go
// nothing. (Proposed by the executability review; kills X27.)
func TestServeHTTP_CoalescedCreatingMissFiresT1Once(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	var fetches atomic.Int32
	r.h.fetch = slowUpstream{staticUpstream: staticUpstream{r.srv.URL + "/r/dists/s/InRelease": rowInRelease},
		delay: 300 * time.Millisecond, fetches: &fetches}
```
new:
```go
// nothing. (Proposed by the executability review; kills X27.) The leader
// waits at its host slot until the second miss has joined it, so the two
// coalesce without a wall-clock window (deferred minor E-66).
func TestServeHTTP_CoalescedCreatingMissFiresT1Once(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	var fetches atomic.Int32
	r.h.fetch = slowUpstream{staticUpstream: staticUpstream{r.srv.URL + "/r/dists/s/InRelease": rowInRelease},
		fetches: &fetches}
	_, host, port := splitURL(t, r.srv.URL)
	holdHostSlotsUntilJoined(t, r.h, hostKey(host, port), 0)
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 -run 'TestHostAdoptions_' ./internal/freshness/`
Expected: FAIL with `inflight_test.go:129: a request with nothing in flight was refused for the waiter cap`.

The three handler changes pin behaviour that holds (`go test -count=1 -run 'TestServeHTTP_StrictWaitRefusals|TestServeHTTP_StrictHitWithNothingInFlightDoesNotWait|TestServeHTTP_CoalescedCreatingMissFiresT1Once' ./internal/handler/` prints `ok`); Step 5's mutants are their red.

- [ ] **Step 3: Reorder the wait (E-58, E-62)**

`inflight.go` `hostAdoptions.wait`:

old:
```go
func (h *hostAdoptions) wait(ctx context.Context, host string, max time.Duration) bool {
	h.mu.Lock()
	if h.waiters[host] >= strictWaitersPerHost {
		h.mu.Unlock()
		return false
	}
	if h.n[host] == 0 {
		h.mu.Unlock()
		return true
	}
	if max < strictMinWait {
```
new:
```go
func (h *hostAdoptions) wait(ctx context.Context, host string, maxWait time.Duration) bool {
	h.mu.Lock()
	if h.n[host] == 0 {
		h.mu.Unlock()
		return true
	}
	if h.waiters[host] >= strictWaitersPerHost {
		h.mu.Unlock()
		return false
	}
	if maxWait < strictMinWait {
```

`t := time.NewTimer(max)` becomes `t := time.NewTimer(maxWait)`, and the doc's first paragraph reads:

```go
// wait blocks until host has no signed adoption in flight, maxWait
// passes or ctx ends, and reports whether the caller may re-run its
// lookup. With nothing in flight it returns true at once — before the
// waiter cap, so a request that arrives as the last adoption ends, with
// waiters still unwinding, re-runs its lookup instead of being refused
// (deferred minor E-58). Otherwise false when the host already has
// strictWaitersPerHost waiters, or maxWait is below strictMinWait (the
// caller refuses at once), and false when ctx ended during the wait (the
// client left: design note 31, no retry — the caller answers
// client_canceled). A request that waits is counted
// (acu_strict_adoption_waits_total).
```

(its AIDEV-NOTE paragraph unchanged). Afterwards `grep -n '\bmax\b' internal/freshness/inflight.go` prints nothing.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -count=1 ./internal/freshness/ ./internal/handler/`
Expected: two `ok` lines.

Run: `go test -race -count=5 -run 'TestServeHTTP_CoalescedCreatingMissFiresT1Once|TestServeHTTP_StrictWaitRefusals|TestServeHTTP_StrictHitWithNothingInFlightDoesNotWait' ./internal/handler/ && go test -race -count=1 ./internal/freshness/ ./internal/handler/`
Expected: all `ok` (the replay ran the X27 test 50 times without `-race` and 5 with it, no failure).

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutant | Fails |
|---|---|
| the waiter cap checked first again (E-58) | `inflight_test.go:129: a request with nothing in flight was refused for the waiter cap` |
| the retryable result without `snapshotCount` (E-59) | `strict_wait_test.go:233: unvouched_deb_refused current_snapshot_count = 0, want 1` |
| the hit path's `AdoptionsInFlight` pre-check removed (E-59) | `strict_wait_test.go:512: waited 1 times, want 0 (nothing in flight)` |
| a coalesced waiter fires T1 too (`if res.created && h.adoptionEnabled {`, X27) | `seed_t1_test.go:319: T1 calls [{http 127.0.0.1 /r/dists/s} {http 127.0.0.1 /r/dists/s}], want exactly one (the creating leader's)` |

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/freshness/inflight.go internal/freshness/inflight_test.go internal/handler/strict_wait_test.go internal/handler/seed_t1_test.go
git add internal/freshness/inflight.go internal/freshness/inflight_test.go internal/handler/strict_wait_test.go internal/handler/seed_t1_test.go
git commit -m "$(cat <<'EOF'
Strict wait: nothing in flight before the waiter cap; pin two survivors

A request that arrives as the host's last adoption ends re-runs its
lookup instead of being refused for waiters still unwinding (E-58); the
parameter max is maxWait (E-62). Tests pin the retried refusal's
current_snapshot_count and the hit path's in-flight pre-check (E-59),
and the coalesced creating miss no longer depends on a real 300 ms
window (E-66).

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

### Task 12: The Release `Date` parser (B-9, B-11, B-14; B-13's note)

apt's whitespace is C's `isspace`, ASCII only; the parser split and trimmed on Unicode whitespace (B-9). `parseAptDate` shadows `ok` (B-11), and its AIDEV-NOTE blames `aptDigits` for the RFC 850 day's divergence (B-14). B-13 is declined (table): reading the first paragraph only broke eight tests whose verifiers pass the clearsign envelope through, and an AIDEV-NOTE says so where the next reader would look.

**Files:**
- Modify: `internal/freshness/release_date.go` (new `aptSpace`, `releaseDate`, `parseAptDate`, its AIDEV-NOTE), `internal/freshness/release_date_test.go` (a new test)

**Interfaces:**
- Consumes: `rfcExample` (`release_date_test.go`).
- Produces: `const aptSpace = " \t\n\v\f\r"` (unexported).

- [ ] **Step 1: Write the failing test**

At the end of `internal/freshness/release_date_test.go`:

```go
// Deferred minor B-9: apt's isspace is ASCII, so a no-break space neither
// separates the date's fields nor pads the value (the date is unreadable:
// 0), while any ASCII blank does both.
func TestReleaseDate_ASCIIWhitespaceOnly(t *testing.T) {
	for text, want := range map[string]int64{
		"Date: Sun,\u00a006 Nov 1994 08:49:37 GMT\n":   0,
		"Date: Sun, 06 Nov 1994 08:49:37 GMT\u00a0\n":  0,
		"Date:\tSun,\t06\tNov\v1994\f08:49:37 GMT\t\n": rfcExample,
	} {
		if got := releaseDate([]byte(text)); got != want {
			t.Errorf("releaseDate(%q) = %d, want %d", text, got, want)
		}
	}
}
```

(`\u00a0` is the escape in the source, not the character: an editor that turns it into a raw no-break space makes `gofmt` re-align the map and hides what the test is about.)

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -count=1 -run 'TestReleaseDate' ./internal/freshness/`
Expected: FAIL with
```
release_date_test.go:120: releaseDate("Date: Sun,\u00a006 Nov 1994 08:49:37 GMT\n") = 784111777, want 0
release_date_test.go:120: releaseDate("Date: Sun, 06 Nov 1994 08:49:37 GMT\u00a0\n") = 784111777, want 0
```

- [ ] **Step 3: ASCII whitespace, no shadowing, the notes**

`release_date.go`, before `releaseDate`:

```go
// aptSpace is C's isspace in the "C" locale, apt's whitespace: ASCII
// only, so a no-break space or other Unicode space is an ordinary
// character (deferred minor B-9).
const aptSpace = " \t\n\v\f\r"
```

`releaseDate`'s doc gains, after its first paragraph:

```go
//
// AIDEV-NOTE: every top-level line is read, where apt reads the first
// paragraph only (deferred minor B-13, declined in phase H): real Release
// files are one paragraph, and this package's test verifiers hand the
// parsers the whole clearsign envelope — its armor header and blank line
// first — so a first-paragraph reader finds no Date in their fixtures.
```

and its body's field test reads:

```go
		key, val, found := strings.Cut(line, ":")
		if !found || !strings.EqualFold(strings.Trim(key, aptSpace), "Date") {
			continue
		}
		if t, ok := parseAptDate(strings.Trim(val, aptSpace)); ok {
```

(`TrimSpace` → `Trim(…, aptSpace)` twice; the Cut's `ok` is `found`, so the inner `ok` shadows nothing). In `parseAptDate`, `f := strings.Fields(s)` becomes `f := strings.FieldsFunc(s, func(r rune) bool { return strings.ContainsRune(aptSpace, r) })`, and the RFC 850 branch's year (B-11):

old:
```go
		yy, ok := aptStoi(parts[2])
		if !ok {
			return 0, false
		}
		year = 1900 + yy
```
new:
```go
		var yy int
		if yy, ok = aptStoi(parts[2]); !ok {
			return 0, false
		}
		year = 1900 + yy
```

The AIDEV-NOTE's divergence paragraph (B-14):

old:
```go
// Two deliberate divergences remain, both confined to the day token in
// RFC 1123/asctime form (aptDigits, which is unsigned and requires the
// whole token to be digits): apt reads it with istream >>, which stops at
// the first non-digit, so a token such as "6," parses there as 6, where
// this rejects it outright; and an RFC 850 day above 65535 is rejected
// instead of wrapping. Real Release files never reach either difference.
```
new:
```go
// Two deliberate divergences remain, both in the day token. In RFC 1123
// and asctime form (aptDigits, unsigned, the whole token digits), apt
// reads it with istream >>, which stops at the first non-digit, so a token
// such as "6," parses there as 6, where this rejects it outright. In RFC
// 850 form (aptStoi, then parseAptDate's day > 65535 bound), apt keeps
// std::stoi's result in an unsigned short, so a day above 65535 wraps
// there and is rejected here (deferred minor B-14: not aptDigits). Real
// Release files never reach either difference.
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -count=1 ./internal/freshness/`
Expected: `ok` (every existing date test, the CRLF and padded-key cases of `TestReleaseDate` included, still passes).

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutant | Fails |
|---|---|
| `parseAptDate` back on `strings.Fields` (B-9) | `release_date_test.go:120: releaseDate("Date: Sun,\u00a006 Nov 1994 08:49:37 GMT\n") = 784111777, want 0` (and the trailing one) |
| the value trimmed with `strings.TrimSpace` (B-9) | `release_date_test.go:120: releaseDate("Date: Sun, 06 Nov 1994 08:49:37 GMT\u00a0\n") = 784111777, want 0` |

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/freshness/release_date.go internal/freshness/release_date_test.go
git add internal/freshness/release_date.go internal/freshness/release_date_test.go
git commit -m "$(cat <<'EOF'
Release Date: ASCII whitespace as apt; no shadowed ok; exact notes

The Date parser splits and trims on C's isspace, as apt does, so a
no-break space is an ordinary character (B-9); parseAptDate's RFC 850
branch no longer shadows ok (B-11); its AIDEV-NOTE puts the RFC 850
day's divergence on the unsigned-short bound, not aptDigits (B-14), and
releaseDate's records why it still reads every paragraph (B-13).

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

### Task 13: Simulations and their harness (C-5, C-7, C-8, C-38, E-50–E-53, E-55, E-75, F-10, F-11; rulings R-H4, R-H7)

The 500-suite simulation was over the `-race` budget (C-38; E-75: re-measured here, 7.19–7.33 s at HEAD); it is coarsened without dropping an assertion (design note 15). The ledger gains its two missing tests and its docs (C-5, C-8); the flip simulations' client ids stop colliding (C-7); the retry simulations get per-stretch floors, a forward-leg future-date check and an off-grid split (E-50, E-52, E-53); a metrics read stops swallowing a parse error (E-51); the DoD simulations count every anchor URL of a dormant suite (E-55), sample goroutines mid-boot (F-10) and put `gcd`'s deadline off the reaper's 6 h grid (F-11). Ruling R-H7: `TestCheck_BothSignedFormsFailingUnderJitterStayWithinTheirBounds`, a real-time test on the rig's virtual clock rather than a simulation, checks every five minutes instead of two, every assertion kept (Step 7). Only `internal/simtest/simtest.go` (`Unavailable`) and `aptclient.go`'s docs are non-test code.

**Files:**
- Modify: `internal/simtest/simtest.go` (new `Unavailable`), `internal/simtest/aptclient.go` (docs), `internal/simtest/aptclient_test.go` (two tests), `internal/flipgate/sim_test.go` (`clients` and its six calls), `internal/freshness/sim_sched_test.go` (new `seedSimSuites`, `TestSimScheduler_FiveHundredStallingOneHealthy`, `TestSimScheduler_StallingHostWithAdoptionContention`'s doc and `cAdoptions`), `internal/freshness/sim_retry_test.go` (`TestSimRetry_LongRunDateRulesAndClockSteps`, `TestSimRetry_ClockStepsWithAnActiveRecord`), `internal/freshness/dod_e_test.go` (`TestDoD_ActiveSuitesAdoptOnceDormantSuitesStaySilent`), `internal/freshness/dod_f_test.go` (`TestDoD_NinetyDaysOfReapingWithRestarts`, `TestDoD_ProductionCadenceAcrossTwelveHourlyRestarts`), `internal/freshness/current_form_test.go` (`TestCheck_BothSignedFormsFailingUnderJitterStayWithinTheirBounds`, R-H7)

**Interfaces:**
- Consumes: `renderedFloat` (`reconcile_test.go`), `sampleGoroutines`, `simtest.Stall`, `simtest.NotModified`.
- Produces: `simtest.Unavailable() Behavior`; test helper `seedSimSuites(t *testing.T, c *cache.Cache, host string, suites []string, lastAt int64)`; `clients(l, serve, queue, idBase int, offset, period, until, wg)`.

- [ ] **Step 1: Measure the touched simulations before (ruling R-H4)**

Run, three times: `go test -race -count=1 -v -run 'TestSimScheduler_FiveHundredStallingOneHealthy$|TestSimScheduler_StallingHostWithAdoptionContention$|TestSimRetry_LongRunDateRulesAndClockSteps$|TestSimRetry_ClockStepsWithAnActiveRecord$|TestDoD_ActiveSuitesAdoptOnceDormantSuitesStaySilent$|TestDoD_NinetyDaysOfReapingWithRestarts$|TestDoD_ProductionCadenceAcrossTwelveHourlyRestarts$|TestRun_AnOfferWhoseWakeWasSpentIsReofferedAtOnce$|TestCheck_BothSignedFormsFailingUnderJitterStayWithinTheirBounds$' ./internal/freshness/` and `go test -race -count=1 -v -run 'TestSimFlip_' ./internal/flipgate/`, noting each `--- PASS` time. The replay's figures are the "before" column of Step 8's table.

- [ ] **Step 2: The ledger's tests and docs (C-5, C-8)**

`internal/simtest/aptclient_test.go`: imports gain `"strconv"`; before `TestLedger_IgnoresIndexesOutsideTheWindow`:

```go
// Deferred minor C-5: an anchor's own hold does not shorten its index's
// window. The anchor arrives after a 5 s hold and the client then spends
// 11 s on a download: its index request goes out 11 s after the anchor
// arrived — outside the window, not checked (deducting the anchor's own
// hold would make it 6 s, inside).
func TestLedger_AnAnchorsOwnHoldIsNotDeducted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		serve := func(rq Request) (string, time.Duration) {
			if rq.Anchor {
				time.Sleep(5 * time.Second)
				return "1", 5 * time.Second
			}
			return "2", 0
		}
		l := NewLedger(10 * time.Second)
		l.RunUpdate(1, []Request{{Suite: "a", Anchor: true}, {Filler: 11 * time.Second}}, serve)
		if l.Checked() != 0 || len(l.Straddles()) != 0 {
			t.Fatalf("checked %d, straddles %v; want neither", l.Checked(), l.Straddles())
		}
	})
}

// Deferred minor C-5: AnchorGens, the proof that clients crossed a flip,
// holds the generations of checked pairs only: updates whose index fell
// outside the window add none.
func TestLedger_AnchorGensHoldsCheckedPairsOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gen := 0
		serve := func(rq Request) (string, time.Duration) {
			if rq.Anchor {
				gen++
			}
			return strconv.Itoa(gen), 0
		}
		l := NewLedger(10 * time.Second)
		l.RunUpdate(1, []Request{{Suite: "a", Anchor: true}, {Filler: 11 * time.Second}}, serve)
		l.RunUpdate(2, []Request{{Suite: "a", Anchor: true}, {Filler: 12 * time.Second}}, serve)
		if g := l.AnchorGens("a"); len(g) != 0 {
			t.Fatalf("AnchorGens = %v after updates outside the window, want none", g)
		}
		l.RunUpdate(3, []Request{{Suite: "a", Anchor: true}}, serve)
		if g := l.AnchorGens("a"); !slices.Equal(g, []string{"3"}) {
			t.Fatalf("AnchorGens = %v, want [3] (the one checked pair)", g)
		}
	})
}
```

Both pass at HEAD (they pin behaviour that holds; Step 8's mutants are their red). `aptclient.go`: `RunUpdate`'s doc reads `// RunUpdate runs one update for client, starting from queue. It blocks` / `// for the whole update — in real time outside a synctest bubble — and is` / `// safe for concurrent use: simulations run many clients' updates at once` / `// (deferred minor C-8).`; `AnchorGens`'s first line reads `// AnchorGens returns, sorted as strings ("10" before "9"), the anchor` / `// generations of suite's checked pairs: two or more prove its clients` / `// crossed a flip (deferred minor C-8).`

- [ ] **Step 3: The flip simulations' client ids (C-7)**

`internal/flipgate/sim_test.go`: `clients` takes an id base after `queue` and numbers its clients from it:

```go
// clients starts, every period from offset until until, a client that
// runs one update of queue. Its clients are numbered from idBase, so groups
// running at once have disjoint ids and a Straddle's Client names one
// client (deferred minor C-7).
func clients(l *simtest.Ledger, serve simtest.ServeFunc, queue []simtest.Request, idBase int, offset, period time.Duration, until time.Time, wg *sync.WaitGroup) {
```

with `l.RunUpdate(idBase+i, queue, serve)` in its loop. The calls pass `0` and `10000` for the two groups of `TestSimFlip_OverlappingHoldTrace` and of `TestSimFlip_ClientReleasedFromAnotherHold`, `0` in `TestSimFlip_SlowReaderCommitsInsideTheHold` (its single extra client keeps id 1000, above that group's 200), and `10000*i` in `TestSimFlip_FiveBusySuites`' loop over the keys.

- [ ] **Step 4: The 500-suite simulation (C-38, design note 15)**

`internal/simtest/simtest.go`, after `NotModified`'s neighbour behaviours:

```go
// Unavailable answers 503 at once: an unknown end that costs no time.
func Unavailable() Behavior {
	return func(context.Context, string) (*fetch.ConditionalResult, error) {
		return nil, &fetch.StatusError{Code: 503}
	}
}
```

`internal/freshness/sim_sched_test.go`: imports gain `"database/sql"` and `"path/filepath"` (and lose `"strconv"`, Step 6); after `seedSimSuite`:

```go
// seedSimSuites is seedSimSuite, fail count 0, for many suites of host:
// one transaction on the cache's database, where seedSimSuite takes three
// writer calls a suite (deferred minor C-38: seeding 501 suites that way
// took 2.2 s of the -race budget).
func seedSimSuites(t *testing.T, c *cache.Cache, host string, suites []string, lastAt int64) {
	t.Helper()
	if err := c.PutBlob(context.Background(), testHex64, 1); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(c.Dir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, suite := range suites {
		if _, err := tx.Exec(`INSERT INTO url_path (canonical_scheme, canonical_host, path, blob_hash, upstream_url, is_metadata, request_count)
 VALUES ('http', ?, ?, ?, ?, 1, 1)`, host, suite+"/InRelease", testHex64, "http://"+host+suite+"/InRelease"); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO suite_freshness (canonical_scheme, canonical_host, suite_path, last_check_at, last_success_at, last_requested_at)
 VALUES ('http', ?, ?, ?, ?, ?)`, host, suite, lastAt, lastAt, lastAt); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
```

(a test-only second connection, used before the scheduler starts, with no cache write in flight.) In `TestSimScheduler_FiveHundredStallingOneHealthy`:

old:
```go
		for i := 0; i < 500; i++ {
			suite := fmt.Sprintf("/s%03d/dists/x", i)
			seedSimSuite(t, c, "a", suite, stale, 0)
			up.Set("http://a"+suite+"/InRelease", simtest.Stall())
		}
		const sPath = "/zzz/dists/x"
		sURL := "http://a" + sPath + "/InRelease"
		seedSimSuite(t, c, "a", sPath, stale, 0)
```
new:
```go
		suites := make([]string, 500)
		for i := range suites {
			suites[i] = fmt.Sprintf("/s%03d/dists/x", i)
			up.Set("http://a"+suites[i]+"/InRelease", simtest.Unavailable())
		}
		const sPath = "/zzz/dists/x"
		sURL := "http://a" + sPath + "/InRelease"
		seedSimSuites(t, c, "a", append(slices.Clone(suites), sPath), stale)
```

and phase 1:

old:
```go
		// Phase 1: every stalling suite fails once while still in the
		// healthy class (spec C4: a first failure is paid once):
		// 500 × 2 min ≈ 16 h 40 min.
		time.Sleep(17*time.Hour + 37*time.Minute)
```
new:
```go
		// Phase 1: every one of the 500 fails once while still in the
		// healthy class (spec C4: a first failure is paid once). Each
		// answers its first check 503, an unknown end that takes no time,
		// so the first tick fails all 500; from then on they stall
		// (deferred minor C-38: 500 stalling first checks took 16 h 40 min
		// of virtual time, and their ticks most of the -race budget).
		time.Sleep(tick + time.Minute)
		for _, suite := range suites {
			up.Set("http://a"+suite+"/InRelease", simtest.Stall())
		}
```

Every assertion after it (500 failing; S's gaps in phase 2; the restart's order and goroutine bound in phase 3) is unchanged. `TestSimScheduler_StallingHostWithAdoptionContention`'s doc: `(that one, without an adopter, is already over the 5 s -race budget: deferred minor C-38, phase H)` reads `(that one, without an adopter, fits the -race budget only coarsened: deferred minor C-38)`.

- [ ] **Step 5: The retry simulations (E-50, E-52, E-53)**

`internal/freshness/sim_retry_test.go`, `TestSimRetry_LongRunDateRulesAndClockSteps`: the untrusted phase's sleeps:

old:
```go
		time.Sleep(5 * day)
		s.touch(t) // a client asks again: the suite stays interesting
		time.Sleep(5*day + 7*time.Minute)
```
new:
```go
		time.Sleep(5*day - 7*time.Minute) // off the tick grid (deferred minor E-53)
		s.touch(t)                        // a client asks again: the suite stays interesting
		time.Sleep(5*day + 14*time.Minute)
```

(the phase still ends at 10 days 7 minutes). Before the backward step:

```go
		// Each clock-step stretch has its own liveness floor (deferred
		// minor E-50: the run's floor below was carried by the failing
		// phases): 2 days at one check per refresh is about 18.
		stretchChecks := func(from int, step string) {
			t.Helper()
			if n := len(s.up.Calls(rigAnchorURL)) - from; n < 16 {
				t.Fatalf("after a %s clock step: %d checks in its 2 days, want >= 16", step, n)
			}
		}
		from := len(s.up.Calls(rigAnchorURL))
```

then `stretchChecks(from, "backward")` just before the backward stretch's `restore()`, `from = len(s.up.Calls(rigAnchorURL))` before `restore = wallclock.SetOffsetForTest(30 * day)`, and `stretchChecks(from, "forward")` before that stretch's `restore()` (the replay counted 19 and 20). `TestSimRetry_ClockStepsWithAnActiveRecord`'s forward leg, after `t.Fatal("after a forward step, the overdue retry did not come at the next tick")`'s block (E-52, making its doc true):

```go
		// Deferred minor E-52: the forward leg checks a future date too.
		ahead = s.release("ahead-of-the-forward-stepped-clock", wallclock.Now().Add(time.Hour).Unix())
		s.serve(ahead)
		time.Sleep(refresh + tick + time.Minute)
		if s.current(t) == hashOf(ahead) || strOf(s.row(t).AdoptionFailReason) != "release_date_in_future" {
			t.Fatalf("after a forward step, content dated an hour ahead: current %q reason %q, want refused as release_date_in_future",
				s.current(t), strOf(s.row(t).AdoptionFailReason))
		}
```

- [ ] **Step 6: The metrics read, the dormant suites, the DoD samples and deadline (E-51, E-55, F-10, F-11)**

`sim_sched_test.go` `TestSimScheduler_StallingHostWithAdoptionContention`:

old:
```go
		cAdoptions := func() float64 {
			v, _ := strconv.ParseFloat(renderedMetricLabels("acu_adoption_total", `outcome="success"`, `host="a"`), 64)
			return v
		}
```
new:
```go
		cAdoptions := func() float64 { // deferred minor E-51: a value that does not parse fails the test
			return renderedFloat(t, renderedMetricLabels("acu_adoption_total", `outcome="success"`, `host="a"`))
		}
```

(`renderedMetricLabels` stays: E-51's "fifth helper" is test structure, declined in the item.) `dod_e_test.go`:

old:
```go
		for _, s := range dormant {
			if n := len(up.Calls(anchorURL(s))); n != 0 {
				t.Fatalf("dormant %v: %d upstream requests, want 0", s, n)
			}
		}
```
new:
```go
		for _, s := range dormant { // every anchor file, not InRelease alone (deferred minor E-55)
			n := 0
			for _, name := range []string{"InRelease", "Release", "Release.gpg"} {
				n += len(up.Calls("http://" + s.CanonicalHost + s.SuitePath + "/" + name))
			}
			if n != 0 {
				t.Fatalf("dormant %v: %d upstream requests, want 0", s, n)
			}
		}
```

`dod_f_test.go` `TestDoD_NinetyDaysOfReapingWithRestarts`, after the `day == 31` check:

```go
			// A daily sample mid-boot, not only right after a restart: a
			// goroutine that piles up during a boot and exits at its stop
			// shows here (deferred minor F-10).
			if n := sampleGoroutines(base); n > base {
				t.Fatalf("day %d, 09:00, mid-boot: %d goroutines, the first boot had %d", day, n, base)
			}
```

`TestDoD_ProductionCadenceAcrossTwelveHourlyRestarts` (F-11): its doc's `(by the 48 h boot's startup pass, 6 hours before it: … past it) is still reaped, its marker kept;` reads `(by the 48 h boot's startup pass, 90 minutes before it: the four days are shorter than the 168 h TTL, so the row is seeded already past it) is still reaped, its marker kept — at 50 h, the boot's second hourly run, where a 2-, 3- or 6-hourly reaper would reap it at 51, 52 or 55 h (deferred minor F-11: a deadline of 54 h was reaped at 55 h by all four);`. The seed:

old:
```go
		// gcd's deadline: 54 h; its cached InRelease, unused for 8 days,
		// is past url_path_ttl, and its base was last seen signed then.
		seed(gcd, start.Add(54*time.Hour-ttl))
```
new:
```go
		// gcd's deadline: 49 h 30 m, off the reaper's 6 h grid; its cached
		// InRelease, unused for 8 days, is past url_path_ttl, and its base
		// was last seen signed then.
		seed(gcd, start.Add(49*time.Hour+30*time.Minute-ttl))
```

and the two `half == 4` cases:

old:
```go
					time.Sleep(6 * time.Hour)
				case half == 4 && step == 1: // 54 h: the deadline; the run at 55 h reaps it
					time.Sleep(30 * time.Minute)
					if !b.exists(t, gcd) {
						t.Fatalf("%s reaped before its deadline", gcd)
					}
					time.Sleep(time.Hour)
					if b.exists(t, gcd) {
						t.Fatalf("%s not reaped although url_path GC took its anchor", gcd)
					}
					if at, ok, err := b.c.SignedSeen(ctx, "http", "h", gcd); err != nil || !ok || at < old {
						t.Fatalf("%s's marker after the reap: %d, %v, %v; want it kept", gcd, at, ok, err)
					}
					time.Sleep(4*time.Hour + 30*time.Minute)
```
new:
```go
					// The boot's runs: 49 h (the floor; before the 49 h 30 m
					// deadline), then 50 h, which reaps it.
					time.Sleep(time.Hour + 59*time.Minute)
					if !b.exists(t, gcd) {
						t.Fatalf("%s reaped before the run after its deadline", gcd)
					}
					time.Sleep(2 * time.Minute)
					if b.exists(t, gcd) {
						t.Fatalf("%s not reaped by the hourly run after its deadline, although url_path GC took its anchor", gcd)
					}
					if at, ok, err := b.c.SignedSeen(ctx, "http", "h", gcd); err != nil || !ok || at < old {
						t.Fatalf("%s's marker after the reap: %d, %v, %v; want it kept", gcd, at, ok, err)
					}
					time.Sleep(3*time.Hour + 59*time.Minute) // back on the six-hour grid: 54 h
```

(the 48 h case keeps its anchor assertion above this; the 54 h step now takes the `default` six hours).

- [ ] **Step 7: The jitter test under the budget (ruling R-H7)**

`internal/freshness/current_form_test.go`, `TestCheck_BothSignedFormsFailingUnderJitterStayWithinTheirBounds`. Its rig's clock is virtual (`r.clock.advance`): the test never waits on wall time, and its 7.6–8.0 s under `-race` are 1,440 checks of about 5 ms each (SQLite reads and writes per check), which a `testing/synctest` bubble would not shorten. Fewer checks keep every assertion: with the same seed, a check every five minutes gives 48 lateral windows (the floor is 30), 15 with two detached attempts (the floor is one), none early and none with three; every two minutes gave 48, 18, none and none. (Every three, four and six minutes took 5.2 s, 4.1 s and 2.9 s, the last with 10 windows of two.) The cost is resolution: a lateral hold released early by less than the five-minute cadence passes unseen; the mutants below release it 10 and about 27 minutes early (review M-4, which the comment records).

old:
```go
// attempt. (About a quarter more attempts than once per form per window:
// accepted, no further machinery.)
func TestCheck_BothSignedFormsFailingUnderJitterStayWithinTheirBounds(t *testing.T) {
```
new:
```go
// attempt. (About a quarter more attempts than once per form per window:
// accepted, no further machinery.)
//
// Ruling R-H7 (phase H1): a check every five minutes, not two. The rig's
// clock is virtual, so the test's time is CPU — about 5 ms a check under
// -race, 1,440 checks took 7.6-8.0 s — and a synctest bubble would not
// shorten it. 576 checks keep every assertion with the same seed (48
// lateral windows, 15 with two detached attempts, none early) and kill the
// lateral-hold mutants the two-minute cadence killed; an early release
// shorter than the five-minute cadence is below this test's resolution
// (review M-4).
func TestCheck_BothSignedFormsFailingUnderJitterStayWithinTheirBounds(t *testing.T) {
```

old:
```go
	for m := 0; m < 2*24*60; m += 2 { // two days, a check every two minutes
		r.clock.advance(2 * time.Minute)
```
new:
```go
	for m := 0; m < 2*24*60; m += 5 { // two days, a check every five minutes
		r.clock.advance(5 * time.Minute)
```

- [ ] **Step 8: Run and measure after (rulings R-H4, R-H7)**

Run: `go test -count=1 ./internal/simtest/ ./internal/flipgate/ ./internal/freshness/`
Expected: three `ok` lines.

Run Step 1's two commands three times again. Every time must be under 4.5 s (PE-8). The replay (min–max of three runs, `-race`, this machine):

| Simulation | Before | After |
|---|---|---|
| `TestSimScheduler_FiveHundredStallingOneHealthy` | 7.19–7.33 s | 2.93 s |
| `TestSimScheduler_StallingHostWithAdoptionContention` | 3.48–3.53 s | 3.51–4.23 s (one call changed; the 4.23 s run overlapped another `-race` build) |
| `TestSimRetry_LongRunDateRulesAndClockSteps` | 3.76–3.91 s | 3.75–3.87 s |
| `TestSimRetry_ClockStepsWithAnActiveRecord` | 0.45–0.47 s | 0.48–0.51 s |
| `TestDoD_ActiveSuitesAdoptOnceDormantSuitesStaySilent` | 1.28–1.39 s | 1.25–1.39 s |
| `TestDoD_NinetyDaysOfReapingWithRestarts` | 3.85–4.05 s | 3.73–3.81 s |
| `TestDoD_ProductionCadenceAcrossTwelveHourlyRestarts` | 1.02–1.37 s | 1.06–1.07 s |
| `TestRun_AnOfferWhoseWakeWasSpentIsReofferedAtOnce` (Task 9) | — | 0.31–0.34 s |
| `TestSimFlip_*` (four) | ≤ 0.07 s | ≤ 0.09 s |
| `TestCheck_BothSignedFormsFailingUnderJitterStayWithinTheirBounds` (R-H7; not a simulation) | 7.63–8.00 s | 3.39–3.46 s |

- [ ] **Step 9: Mutation checks** (one at a time, each reverted)

| Mutant | Fails |
|---|---|
| the anchor's arrival records `held: heldBefore` (C-5) | `aptclient_test.go:55: checked 1, straddles [{1 a 1 2 6s}]; want neither` |
| `record` notes the generation before the window check (C-5) | `aptclient_test.go:76: AnchorGens = [1 2] after updates outside the window, want none` |
| never-checked suites ordered first (`classNeverChecked` before `classHealthy`; C-38, the test's own named mutant) | `sim_sched_test.go:412: after the restart S was checked at -2h0m0s past the restart, bound 17m0s` |
| `classOf` ignores the streak (C-38) | `sim_sched_test.go:370: S checked 0 times in phase 2, want at least 2` |
| the reaper starts a goroutine per run that exits at the stop (F-10) | `dod_f_test.go:363: day 0, 09:00, mid-boot: 10 goroutines, the first boot had 8` |
| `reapInterval = 2 * time.Hour` (F-11) | `dod_f_test.go:569: /r/dists/gcd not reaped by the hourly run after its deadline, although url_path GC took its anchor` |
| `lateralHeld` never holds (`if h.key != key || nowUnix >= h.retryAt {` becomes `if true {`; R-H7) | `current_form_test.go:1311: +30 min: the lateral InRelease was attempted 1605 s before its own retry time` |
| `lateralHeld` releases ten minutes early (`nowUnix >= h.retryAt-600`; R-H7) | `current_form_test.go:1311: +50 min: the lateral InRelease was attempted 405 s before its own retry time` |

E-50's floors have margins of 3 and 4 checks. The replay tried two single-site production mutants a clock step could expose — `suiteDue` reading a future `last_success_at` as fresh, and the cooldown gate taking a future `last_check_at` as "just checked" — and both survive this simulation, its new floors included (the I-7 and B-38 unit tests catch them), so the floors stand as liveness guards, as the run floor does. E-52's case and E-55's sum pin behaviour across a forward step and for the two other anchor files; a mutant reaching them only there would need a clock or URL the code does not have.

- [ ] **Step 10: Commit**

```bash
gofmt -l internal/simtest/simtest.go internal/simtest/aptclient.go internal/simtest/aptclient_test.go internal/flipgate/sim_test.go internal/freshness/sim_sched_test.go internal/freshness/sim_retry_test.go internal/freshness/dod_e_test.go internal/freshness/dod_f_test.go internal/freshness/current_form_test.go
git add internal/simtest/simtest.go internal/simtest/aptclient.go internal/simtest/aptclient_test.go internal/flipgate/sim_test.go internal/freshness/sim_sched_test.go internal/freshness/sim_retry_test.go internal/freshness/dod_e_test.go internal/freshness/dod_f_test.go internal/freshness/current_form_test.go
git commit -m "$(cat <<'EOF'
Simulations: the 500-suite run under budget; sharper floors, ids, samples

The 500-suite scheduler simulation answers its stalling suites' first
checks 503 and seeds them in one transaction: 7.2 s -> 2.9 s under
-race, every assertion kept (C-38). The ledger gains tests for an
anchor's own hold and for AnchorGens, and its docs say what RunUpdate
and AnchorGens promise (C-5, C-8); flip simulation groups get disjoint
client ids (C-7). The retry simulation floors each clock-step stretch,
splits a sleep off the tick grid, and its sibling checks a future date
after a forward step (E-50, E-52, E-53); a metrics read fails on a bad
value (E-51); dormant suites count every anchor URL (E-55); the 90-day
DoD samples goroutines daily mid-boot (F-10) and the production-cadence
DoD puts gcd's deadline off the 6 h grid (F-11). The both-forms jitter
test checks every five minutes, not two: 7.6-8.0 s -> 3.4 s, every
assertion kept (R-H7).

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

### Task 14: Test gaps in the cache, `aptpath`, `wallclock`, `gpg`, `config` and `simtest` (A-1, A-2, A-8, A-9, A-11, A-14, A-15, A-19–A-21, C-9, C-12, C-13, D-6, D-7, D-9, D-10, D-26, E-1, E-3, F-8, F-9)

Twenty-two test-only items, one per package step, and two small non-test changes: `wallclock.SetOffsetForTest`'s doc (A-2) and `simtest.ClearsignedFixture`'s panic (D-7). Every behaviour change is D-7's; the rest pin behaviour that holds (Step 9's mutants are their red) or tidy a test. `internal/cache/cache_test.go` is a gofmt drift file: its edit (Step 5, D-9) is made by hand — exact replacements, never `gofmt -w`.

**Files:**
- Create: `internal/freshness/hot_set_test.go`
- Modify: `internal/wallclock/wallclock.go` (doc), `internal/wallclock/wallclock_test.go`, `internal/aptpath/imports_test.go`, `internal/aptpath/encode_test.go`, `internal/aptpath/composers_test.go`, `internal/cache/schema_v7_test.go`, `internal/cache/sqlclock_test.go`, `internal/cache/gc_test.go` (`stubNow`), `internal/cache/flip_state_test.go`, `internal/cache/cache_test.go` (by hand), `internal/cache/adoption_failure_test.go`, `internal/cache/unsigned_form_test.go`, `internal/config/config_test.go`, `internal/gpg/failclosed_test.go`, `internal/simtest/clearsign.go`, `internal/simtest/clearsign_test.go`, `internal/freshness/reconcile_r1_test.go`, `internal/freshness/first_adoption_test.go`, `internal/freshness/suite_lock_test.go`

**Interfaces:**
- Consumes: cache test helpers `openCache`, `seedBlob`, `stubNow`, `afSuite`, `commitCandidate`, `adoptedAtOf`, `seedFormSnapshot`, `putURLPathRow`, `drainURLPathGC`, `urlPathExists`, `testMaxVersions`, `derefStr`; freshness test helpers `newAdoptionTestEnv`, `newRetryRig`, `lockedBuffer`, `logRecords`, `rigReleaseURL`, `rigReleaseGPGURL`; gpg test helpers `newTestEntity`, `newKeyring`, `clearsignWith`, `fakeReleasePlaintext`, `clearsignBeginMarker`, `newSuite`, `silentLogger`; `aptpath.AnchorPaths`.
- Produces: cache test helper `putAnchor(t *testing.T, c *Cache, file string, fetched int64)`; `stubNow` registers its restore with `t.Cleanup`; `simtest.ClearsignedFixture` panics on a line of text that begins with `-`.

- [ ] **Step 1: `wallclock` (A-1, A-2)**

`wallclock.go`, `SetOffsetForTest`'s doc:

old:
```go
// SetOffsetForTest shifts Now by d and returns a function that restores
// the previous offset. Only tests call it.
```
new:
```go
// SetOffsetForTest shifts Now by d and returns a function that restores
// the previous offset. Only tests call it (TestSetOffsetForTest_OnlyTestsCallIt
// walks cmd/ and internal/). Restores run in the reverse order of the calls
// — last in, first out, as deferred calls and t.Cleanup run them — since
// each restores the offset in force when its call was made (deferred
// minor A-2).
```

`wallclock_test.go`: imports gain `"go/ast"`, `"go/parser"`, `"go/token"`, `"io/fs"`, `"path/filepath"`, `"strings"`; in `TestSetOffsetForTest_ShiftsAndRestores`, after `restore := SetOffsetForTest(-2 * time.Hour)`: `t.Cleanup(restore) // a failing assertion must not leak the offset (deferred minor A-1)` (its inline `restore()` stays: it is what the test checks; a second restore is harmless). Before `TestNow_HasNoMonotonicReading`:

```go
// Deferred minor A-2: production code never steps the wall clock — only
// tests call SetOffsetForTest.
func TestSetOffsetForTest_OnlyTestsCallIt(t *testing.T) {
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join("..", "..", dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fn := call.Fun.(type) {
				case *ast.SelectorExpr:
					ok = fn.Sel.Name == "SetOffsetForTest"
				case *ast.Ident:
					ok = fn.Name == "SetOffsetForTest"
				default:
					ok = false
				}
				if ok {
					t.Errorf("%s calls SetOffsetForTest outside a test", path)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
```

- [ ] **Step 2: `aptpath` (A-8, A-9, A-11, A-14)**

`imports_test.go`, `TestCacheDoesNotImportProxy`'s doc gains:

```go
//
// It checks cache's direct imports only (deferred minor A-8): a package
// cache imports could pull proxy in. `go list -deps ./internal/cache/ |
// grep internal/proxy` checks the whole set (empty at 6786529, where
// cache's module imports are aptpath, debversion and wallclock).
```

`encode_test.go`: `TestBuildUpstreamURL_AllBytesRoundTrip`'s loop starts at 0 — `for b := 0; b < 256; b++ { // NUL included (deferred minor A-9)` (NUL round-trips: replayed). `TestBuildUpstreamURL_FixtureDB`'s loop (opt-in, `ACU_URLPATH_FIXTURE_DB`):

old:
```go
	for _, p := range paths {
		u, err := url.Parse(BuildUpstreamURL("http", "h", p))
		if err != nil || u.Path != p {
			t.Errorf("round trip failed for %q: %v", p, err)
		}
		if strings.ContainsAny(p, "+~") && !strings.Contains(BuildUpstreamURL("http", "h", p), "%") {
			t.Errorf("expected apt encoding for %q", p)
		}
	}
```
new:
```go
	for _, p := range paths { // deferred minor A-11: each message says what was got
		raw := BuildUpstreamURL("http", "h", p)
		u, err := url.Parse(raw)
		switch {
		case err != nil:
			t.Errorf("url.Parse(%q) for path %q: %v", raw, p, err)
		case u.Path != p:
			t.Errorf("round trip of %q: got %q (URL %q)", p, u.Path, raw)
		}
		if strings.ContainsAny(p, "+~") && !strings.Contains(raw, "%") {
			t.Errorf("path %q: got %q, want apt's percent-encoding", p, raw)
		}
	}
```

`composers_test.go`:

old:
```go
// '#', '?' and '%' (S-3).
func TestNoAdHocUpstreamURLConcatenation(t *testing.T) {
	concat := regexp.MustCompile(`"://"\s*\+`)
```
new:
```go
// '#', '?' and '%' (S-3). The pattern covers "://" +, "http://" +,
// "https://" + and a Sprintf format with %s:// (deferred minor A-14);
// exempt are the two that build no URL from a decoded path.
func TestNoAdHocUpstreamURLConcatenation(t *testing.T) {
	concat := regexp.MustCompile(`"(https?)?://"\s*\+|Sprintf\("[^"]*%s://`)
	exempt := map[string]bool{
		// The proxy's own address, for apt's configuration.
		filepath.Join("..", "..", "cmd", "apt-cacher-ultra", "print_apt_conf.go"): true,
		// The CONNECT tunnel's inner request: host and RequestURI as the
		// client sent them, re-parsed by net/http like any request line.
		filepath.Join("..", "..", "internal", "proxy", "connect.go"): true,
	}
```

and `if concat.Match(b) {` reads `if concat.Match(b) && !exempt[path] {`. (At 6786529 the widened pattern matches those two files only: `print_apt_conf.go:70` and `connect.go:591`.)

- [ ] **Step 3: the hot set's URL (A-15)**

Create `internal/freshness/hot_set_test.go` (`hot_prefetch_test.go` is a drift file):

```go
package freshness

import (
	"context"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// Deferred minor A-15: the hot set's upstream URL is aptpath's, apt's own
// encoding — a '+' or '~' in a pool path is percent-encoded, as apt
// requests it.
func TestComputeHotSet_EncodesTheUpstreamURLAsApt(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ctx := context.Background()
	c := env.cache
	suite := SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}
	blob := func(n int) string {
		h := strings.Repeat(string(rune('a'+n)), 64)
		if err := c.PutBlob(ctx, h, 1); err != nil {
			t.Fatal(err)
		}
		return h
	}
	anchorOld, anchorNew, debOld, debNew := blob(0), blob(1), blob(2), blob(3)
	const oldPath, newPath = "/r/pool/main/g/g++/g++_1~rc1_amd64.deb", "/r/pool/main/g/g++/g++_2~rc1_amd64.deb"
	prior, _, err := c.InsertTrackedCandidateForTest(ctx, cache.SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h",
		SuitePath: suite.SuitePath, InReleaseHash: &anchorOld})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, prior,
		[]cache.SnapshotMember{{SnapshotID: prior, Path: "InRelease", BlobHash: anchorOld, DeclaredSHA256: anchorOld}}, nil,
		[]cache.PackageHash{{CanonicalScheme: "http", CanonicalHost: "h", Path: oldPath, DeclaredSHA256: debOld,
			SnapshotID: prior, PackageName: "g++", Architecture: "amd64", Version: "1~rc1"}}, nil, true); err != nil {
		t.Fatal(err)
	}
	now := env.adopter.now().Unix()
	if err := c.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: oldPath, BlobHash: &debOld,
		UpstreamURL: "http://h/r/pool/main/g/g%2b%2b/g%2b%2b_1%7erc1_amd64.deb", LastRequestedAt: &now, RequestCount: 1}); err != nil {
		t.Fatal(err)
	}
	cand, _, err := c.InsertCandidateSnapshot(ctx, cache.SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h",
		SuitePath: suite.SuitePath, InReleaseHash: &anchorNew})
	if err != nil {
		t.Fatal(err)
	}
	hot, err := env.adopter.computeHotSet(ctx, suite, cand, []cache.PackageHash{{CanonicalScheme: "http", CanonicalHost: "h",
		Path: newPath, DeclaredSHA256: debNew, SnapshotID: cand, PackageName: "g++", Architecture: "amd64", Version: "2~rc1"}}, 86400)
	if err != nil {
		t.Fatal(err)
	}
	const want = "http://h/r/pool/main/g/g%2b%2b/g%2b%2b_2%7erc1_amd64.deb"
	if len(hot) != 1 || hot[0].path != newPath || hot[0].upstreamURL != want {
		t.Fatalf("hot set %+v, want %s at %s", hot, newPath, want)
	}
}
```

- [ ] **Step 4: the schema and clock guards, `stubNow`, the served anchors (A-19, A-20, A-21, C-12, C-13)**

`schema_v7_test.go` `TestSchemaV7_GoStepFailureRollsBackMigration`, before `orig := migrationGoSteps[6]`:

```go
	// migrationGoSteps is package state: never t.Parallel here, nor in any
	// test that migrates while this one has it swapped (deferred minor A-19).
```

`TestSchemaV7_TriggerRejectsEmptyFormAndUniqueIndexIsPartial`'s last check:

old:
```go
	// The flat_filename partial index exists.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_package_hash_flat_filename'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("flat_filename index missing (%v)", err)
	}
```
new:
```go
	// The flat_filename partial index exists, and is partial (deferred minor A-21).
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_package_hash_flat_filename'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("flat_filename index missing (%v)", err)
	}
	var partial int
	if err := db.QueryRow(`SELECT partial FROM pragma_index_list('package_hash') WHERE name = 'idx_package_hash_flat_filename'`).Scan(&partial); err != nil || partial != 1 {
		t.Fatalf("flat_filename index partial = %d (%v), want 1", partial, err)
	}
```

`sqlclock_test.go`: the forbidden list drops `"CURRENT_TIMESTAMP"`, with the comment above it `// A substring match: CURRENT_TIME also catches CURRENT_TIMESTAMP` / `// (deferred minor A-20).`

`gc_test.go` (C-12):

```go
func stubNow(t *testing.T, ts int64) func() {
	t.Helper()
	prev := nowUnix
	nowUnix = func() int64 { return ts }
	restore := func() { nowUnix = prev }
	// A test that fails before its own restore runs leaves no stub behind
	// (deferred minor C-12); restoring twice is harmless.
	t.Cleanup(restore)
	return restore
}
```

`flip_state_test.go` (C-13): before `TestCommitAdoption_FirstAdoptionPassesTheServedAnchors`:

```go
// putAnchor caches afSuite's anchor file, served (fetched) at fetched
// (deferred minor C-13: three tests wrote it out).
func putAnchor(t *testing.T, c *Cache, file string, fetched int64) {
	t.Helper()
	h := seedBlob(t, c, "served "+file)
	if err := c.PutURLPath(context.Background(), URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: afSuite + file,
		BlobHash: &h, UpstreamURL: "http://h" + afSuite + file, IsMetadata: true, LastFetchedAt: &fetched}); err != nil {
		t.Fatal(err)
	}
}
```

and the three tests use it: the first drops its `ctx` and closure for `putAnchor(t, c, "/InRelease", 1_400_000)`, `putAnchor(t, c, "/Release", 1_500_000)`, `putAnchor(t, c, "/Release.gpg", 1_600_000)`; `TestCommitAdoption_FirstAdoptionKeepsNowPastOlderAnchors`'s seed and `PutURLPath` become `putAnchor(t, c, "/InRelease", 900_000)`; `TestCommitAdoption_FirstAdoptionPassesEachServedAnchorFile`'s become `putAnchor(t, c, file, 1_600_000)`.

- [ ] **Step 5: candidate inserts, the reasonless record, the guard (d) forms, `derefS` (D-9, D-10, E-1, E-3)**

`cache_test.go` (by hand). `TestInsertCandidateSnapshot_RejectsDanglingBlobFK`:

old:
```go
	_, _, err := c.InsertTrackedCandidateForTest(ctx, SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "archive.ubuntu.com",
		SuitePath:       "/ubuntu/dists/noble",
		InReleaseHash:   &dangling,
	})
	if err == nil {
		t.Fatal("expected FK violation, got nil")
	}
```
new:
```go
	// Deferred minor D-9: the suite row first, checked, so only the insert
	// itself can fail below.
	if err := c.EnsureSuiteForTest(ctx, "http", "archive.ubuntu.com", "/ubuntu/dists/noble"); err != nil {
		t.Fatal(err)
	}
	_, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "archive.ubuntu.com",
		SuitePath:       "/ubuntu/dists/noble",
		InReleaseHash:   &dangling,
	})
	if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("err = %v, want the blob FK violation", err)
	}
```

`TestInsertCandidateSnapshot_RejectsBothModes` and `_RejectsAllNull` the same way: `if err := c.EnsureSuiteForTest(ctx, "http", "x.example", "/p"); err != nil { t.Fatal(err) }` before the insert, `c.InsertCandidateSnapshot` in place of `c.InsertTrackedCandidateForTest`, and the assertion `if !errors.Is(err, ErrInvalidAnchorForm) { t.Fatalf("err = %v, want ErrInvalidAnchorForm for both modes", err) }` (respectively `… for no anchor`) — the form is refused before any SQL (`candidateForm`), which the old "expected CHECK violation" messages misnamed.

`adoption_failure_test.go`, before `TestRescheduleFailingAdoptions_SkipsRefusalsAndPanics` (D-10):

```go
// Deferred minor D-10: a record with a count but no reason (a NULL the API
// never writes) is a failure, so the restart reschedules it: COALESCE makes
// the NULL an empty string — without it, NULL NOT IN (…) is NULL and the
// row is skipped.
func TestRescheduleFailingAdoptions_ReschedulesAReasonlessRecord(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	if err := c.SeedSuiteForTest(ctx, SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/n"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`UPDATE suite_freshness SET adoption_fail_count = 3, adoption_fail_reason = NULL,
 adoption_next_retry_at = 9999999999 WHERE suite_path = '/n'`); err != nil {
		t.Fatal(err)
	}
	if n, err := c.RescheduleFailingAdoptions(ctx, 5_000, func() int64 { return 7 }); err != nil || n != 1 {
		t.Fatalf("RescheduleFailingAdoptions = %d, %v; want 1, nil", n, err)
	}
	s, err := c.GetSuiteFreshness(ctx, "http", "h", "/n")
	if err != nil || s.AdoptionNextRetryAt == nil || *s.AdoptionNextRetryAt != 5_007 {
		t.Fatalf("next retry = %v (%v), want 5007", s.AdoptionNextRetryAt, err)
	}
}
```

(Write "an empty string", not two quote marks: gofmt's doc-comment pass turns `''` into a typographic quote.)

`unsigned_form_test.go`: imports gain `"slices"` and `"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"`; `derefS` is deleted and `TestSuiteSnapshot_AnchorHash` calls `derefStr` (E-3: the same function in `suite_rows_test.go`). `TestRunURLPathGCBatch_IdentityGuardFollowsTheAnchorForm` covers the three forms, its kept set derived from `aptpath.AnchorPaths` (E-1):

```go
// Spec C5 / §7: url_path GC's identity guard (d) keeps the anchor files of
// the current snapshot's form (aptpath.AnchorPaths) — an unsigned suite's
// <suite>/Release, not the <suite>/InRelease its single-anchor slot would
// suggest; a detached suite's Release and Release.gpg (deferred minor E-1:
// the detached arm was unpinned) — and no other. Guards (b)/(c) cannot
// decide here: the rows hold other blobs than the snapshot's.
func TestRunURLPathGCBatch_IdentityGuardFollowsTheAnchorForm(t *testing.T) {
	c := openCache(t)
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()
	anchor := seedBlob(t, c, "the snapshot's anchor")
	sig := seedBlob(t, c, "the snapshot's signature")
	old := sql.NullInt64{Int64: now - 30*86400, Valid: true}
	forms := []AnchorForm{FormUnsigned, FormInline, FormDetached}
	names := []string{"InRelease", "Release", "Release.gpg"}
	for _, form := range forms {
		suite := "/u/dists/" + string(form)
		if form == FormDetached {
			seedFormSnapshot(t, c, "http", "h", suite, form, nil, &anchor, &sig, false)
		} else {
			seedFormSnapshot(t, c, "http", "h", suite, form, &anchor, nil, nil, false)
		}
		for _, name := range names {
			putURLPathRow(t, c, "http", "h", suite+"/"+name, seedBlob(t, c, "served "+suite+"/"+name), old, true)
		}
	}
	drainURLPathGC(t, c, 100, 7*86400, 0, testMaxVersions)
	for _, form := range forms {
		for _, name := range names {
			path := "/u/dists/" + string(form) + "/" + name
			if got, want := urlPathExists(t, c, path), slices.Contains(aptpath.AnchorPaths(string(form)), name); got != want {
				t.Errorf("%s kept = %v, want %v", path, got, want)
			}
		}
	}
}
```

- [ ] **Step 6: `config` and `gpg` (C-9, D-6)**

`config_test.go` `TestAdoptionFlipQuietPeriod_DefaultAndBounds`:

old:
```go
	for _, bad := range []string{"16s", "-1s", "500ms", "999ms"} {
		if _, err := load("[adoption]\nflip_quiet_period = \"" + bad + "\"\n"); err == nil {
			t.Errorf("flip_quiet_period = %s accepted, want a validation error", bad)
		}
	}
```
new:
```go
	for _, bad := range []string{"16s", "-1s", "500ms", "999ms"} { // each one Validate's, naming the key (deferred minor C-9)
		_, err := load("[adoption]\nflip_quiet_period = \"" + bad + "\"\n")
		if err == nil || !strings.Contains(err.Error(), "adoption.flip_quiet_period must be 0s or between 1s and 15s") {
			t.Errorf("flip_quiet_period = %s: err %v, want the key's bounds", bad, err)
		}
	}
```

`failclosed_test.go`, before `TestVerifyDetached_EmptyKeyringSaysSo` (D-6):

```go
// Deferred minor D-6: the accept leg end to end for an envelope whose
// every line ends in CRLF and whose marker line carries trailing blanks —
// DecodableClearsign then hands go-crypto an LF marker line before CRLF
// lines: it verifies, and the handler's sniff accepts it too.
func TestVerifyInline_AcceptsACRLFEnvelopeWithABlankPaddedMarker(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	v, err := NewVerifier(VerifierConfig{Keyring: newKeyring(t, signer), RequireSignature: true, Logger: silentLogger()})
	if err != nil {
		t.Fatal(err)
	}
	crlf := bytes.ReplaceAll(clearsignWith(t, signer, []byte(fakeReleasePlaintext)), []byte("\n"), []byte("\r\n"))
	body := bytes.Replace(crlf, []byte(clearsignBeginMarker+"\r\n"), []byte(clearsignBeginMarker+" \t\r\n"), 1)
	if bytes.Equal(body, crlf) {
		t.Fatal("test premise: the marker line was not rewritten")
	}
	if !aptpath.SniffInRelease(body) {
		t.Fatal("the handler's sniff refuses a body adoption accepts")
	}
	out, err := v.VerifyInline(context.Background(), newSuite(), body)
	if err != nil || len(out) == 0 {
		t.Fatalf("VerifyInline = %q, %v; want the signed text", out, err)
	}
}
```

- [ ] **Step 7: the clearsign fixture (D-7, the one behaviour change)**

`clearsign_test.go`, before `TestClearsignedFixture`:

```go
// Deferred minor D-7: text with a line that begins with "-" panics — its
// decoded plaintext would differ from it.
func TestClearsignedFixture_PanicsOnADashLine(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal(`no panic for a line beginning with "-"`)
		}
	}()
	ClearsignedFixture("Origin: x\n- y\n")
}
```

Run: `go test -count=1 -run TestClearsignedFixture ./internal/simtest/`
Expected: FAIL with `clearsign_test.go:16: no panic for a line beginning with "-"`.

`clearsign.go`: imports become `"fmt"` and `"strings"`; the doc and the check:

```go
// ClearsignedFixture wraps text — the Release text of an InRelease — in a
// clearsign envelope (spec §7 "Fixtures"): the first line is the marker
// apt requires, so the handler's sniff (spec C3) counts the body as signed
// metadata; clearsign.Decode returns text — less its final newline, and
// with each line's trailing spaces and tabs trimmed — as the plaintext, so
// reconcile parses it; a pass-through verifier hands the whole envelope to
// ParseRelease, which skips the armor lines. The signature is a fixed
// fake: tests that verify signatures sign for real. A line of text that
// begins with "-" would need dash-escaping, and its decoded plaintext would
// differ: the fixture panics on one (deferred minor D-7).
func ClearsignedFixture(text string) []byte {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "-") {
			panic(fmt.Sprintf("simtest.ClearsignedFixture: line %q begins with \"-\"", line))
		}
	}
	return []byte("-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\n" +
		strings.TrimSuffix(text, "\n") + "\n" + fakeSignatureBlock)
}
```

(No caller passes such a line: the whole module's tests pass after it.)

- [ ] **Step 8: `freshness` (D-26, F-8, F-9)**

`reconcile_r1_test.go` `TestReparseSnapshotRelease_DecodesAptsMarkerLine`, after `blanks := bytes.Replace(…)`:

```go
	if bytes.Equal(blanks, signed) { // deferred minor D-26
		t.Fatal("test premise: the marker line was not rewritten")
	}
```

`first_adoption_test.go` (imports gain `"reflect"`), `TestCheckFirst_AnchorRowReadErrorWritesNothing`: `before := r.row() // the whole row: no column may change (deferred minor F-8)` just before the first `chk.Check`, and

old:
```go
	if n := len(r.up.etagsSent(rigAnchorURL)); n != 0 {
		t.Fatalf("%d upstream requests, want none", n)
	}
	if row := r.row(); row.LastCheckAt != nil || row.CheckFailCount != 0 || row.AdoptionFailCount != 0 {
		t.Fatalf("the read error wrote the row: %+v", row)
	}
```
new:
```go
	for _, url := range []string{rigAnchorURL, rigReleaseURL, rigReleaseGPGURL} {
		if n := len(r.up.etagsSent(url)); n != 0 {
			t.Fatalf("%d upstream requests to %s, want none", n, url)
		}
	}
	if row := r.row(); !reflect.DeepEqual(row, before) {
		t.Fatalf("the read error wrote the row: %+v, want it unchanged: %+v", row, before)
	}
```

(`reflect.DeepEqual` follows the row's pointers, so equal stamps in fresh allocations compare equal.) `suite_lock_test.go` (imports lose `"strings"`), `TestCreateSuite_ReportsAFutureStampThroughTheChecker`: the checker logs through `slog.NewJSONHandler(logs, nil)`, and

old:
```go
	out := logs.String()
	if !strings.Contains(out, "clock_step_detected") || !strings.Contains(out, "stamp=last_requested_at") ||
		!strings.Contains(out, "canonical_scheme=http ") || !strings.Contains(out, "canonical_host=h ") ||
		!strings.Contains(out, "suite_path=/r/dists/x ") {
		t.Fatalf("log = %q; want clock_step_detected for http://h/r/dists/x's last_requested_at", out)
	}
```
new:
```go
	recs := logRecords(t, logs, "clock_step_detected")
	if len(recs) != 1 || recs[0]["stamp"] != "last_requested_at" || recs[0]["canonical_scheme"] != "http" ||
		recs[0]["canonical_host"] != "h" || recs[0]["suite_path"] != "/r/dists/x" {
		t.Fatalf("clock_step_detected records %v; want one for http://h/r/dists/x's last_requested_at", recs)
	}
```

(its comment `// position; each must reach its own field.` continues `— read from the decoded` / `// record, not the text (deferred minor F-9).`).

- [ ] **Step 9: Run everything; mutation checks**

Run: `gofmt -l cmd internal`
Expected: the five drift files only.

Run: `go test -count=1 ./...`
Expected: every package `ok`.

Run: `go test -race -count=1 ./internal/wallclock/ ./internal/aptpath/ ./internal/cache/ ./internal/config/ ./internal/gpg/ ./internal/simtest/ ./internal/freshness/`
Expected: all `ok`.

Mutants (one at a time, each reverted):

| Mutant | Fails |
|---|---|
| a production `init` in `freshness/clockstep.go` calling `wallclock.SetOffsetForTest(0)` (A-2) | `wallclock_test.go:69: ../../internal/freshness/clockstep.go calls SetOffsetForTest outside a test` |
| `hot_set.go` builds `"http://" + suite.CanonicalHost + r.Path` (A-14, A-15) | `composers_test.go:39: ../../internal/freshness/hot_set.go: builds a URL by concatenation; …` and `hot_set_test.go:56: hot set [{… upstreamURL:http://h/r/pool/main/g/g++/g++_2~rc1_amd64.deb}], want … at http://h/r/pool/main/g/g%2b%2b/g%2b%2b_2%7erc1_amd64.deb` |
| the flat_filename index without its `WHERE` (A-21; scratch only — migrations are never edited) | `schema_v7_test.go:282: flat_filename index partial = 0 (<nil>), want 1` |
| `Duration.UnmarshalText` refuses a negative itself (C-9) | `config_test.go:920: flip_quiet_period = -1s: err … negative duration "-1s", want the key's bounds` |
| `DecodableClearsign` returns `b` unchanged (D-6) | `failclosed_test.go:183: VerifyInline = "", InRelease clearsign envelope is malformed; want the signed text` |
| the fixture's dash check disabled (D-7) | `clearsign_test.go:16: no panic for a line beginning with "-"` |
| `candidateForm` takes any `InReleaseHash` as single (D-9) | `cache_test.go:1672: err = InsertCandidateSnapshot: constraint failed: CHECK constraint failed: …` |
| the reschedule without `COALESCE` (D-10) | `adoption_failure_test.go:233: RescheduleFailingAdoptions = 0, <nil>; want 1, nil` |
| guard (d)'s `'detached'` misspelt (E-1) | `unsigned_form_test.go:349: /u/dists/detached/Release kept = false, want true` (and `Release.gpg`) |
| `checkFirst`'s read-error branch writes `inrelease_change_seen_at` (F-8) | `first_adoption_test.go:1012: the read error wrote the row: {… InReleaseChangeSeenAt:0x… …}, want it unchanged: …` |

A-1, A-8, A-9, A-11, A-19, A-20, C-12, C-13, D-26, E-3 and F-9 tidy tests; no production mutant reaches them.

- [ ] **Step 10: Commit**

```bash
gofmt -l internal/wallclock internal/aptpath internal/config internal/gpg internal/simtest internal/freshness/hot_set_test.go internal/freshness/reconcile_r1_test.go internal/freshness/first_adoption_test.go internal/freshness/suite_lock_test.go internal/cache/schema_v7_test.go internal/cache/sqlclock_test.go internal/cache/gc_test.go internal/cache/flip_state_test.go internal/cache/adoption_failure_test.go internal/cache/unsigned_form_test.go
git add internal/wallclock/wallclock.go internal/wallclock/wallclock_test.go internal/aptpath/imports_test.go internal/aptpath/encode_test.go internal/aptpath/composers_test.go internal/freshness/hot_set_test.go internal/cache/schema_v7_test.go internal/cache/sqlclock_test.go internal/cache/gc_test.go internal/cache/flip_state_test.go internal/cache/cache_test.go internal/cache/adoption_failure_test.go internal/cache/unsigned_form_test.go internal/config/config_test.go internal/gpg/failclosed_test.go internal/simtest/clearsign.go internal/simtest/clearsign_test.go internal/freshness/reconcile_r1_test.go internal/freshness/first_adoption_test.go internal/freshness/suite_lock_test.go
git commit -m "$(cat <<'EOF'
Tests: close twenty-two deferred gaps across cache, aptpath, gpg and more

A guard that only tests step the wall clock and a cleanup for the one
inline restore (A-1, A-2); aptpath's import note, NUL in the all-bytes
loop, got/want in the fixture test and a URL guard that also catches
"http(s)://" + and Sprintf("%s://") (A-8, A-9, A-11, A-14); a direct
test of the hot set's apt encoding (A-15); schema, clock and stub
hygiene (A-19, A-20, A-21, C-12, C-13); exact errors for candidate
inserts and a reasonless record rescheduled (D-9, D-10); guard (d)
pinned for all three forms (E-1, E-3); config's bound named (C-9); a
CRLF envelope verified end to end (D-6); the clearsign fixture refuses
a dash line (D-7); and three freshness tests made exact (D-26, F-8,
F-9).

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

### Task 15: Test output and flakes (C-39, D-46, D-48, D-49, D-56, D-57, D-58, D-60, E-73, F-20, F-23)

The test output of `internal/admin`, `internal/freshness`, `internal/gc`, `internal/cache` and `cmd` is made silent but for the tests' own `t.Log` lines (D-48, D-56, D-58, F-20): the replay's `go test -v` lines that are not go test's own went from 2132, 603, 518, 34 and 30 to 18, 5, 0, 20 and 0 — every one left a `t.Logf`. The daemon-start loop of the `cmd` tests gets one helper with a timed client that drains the daemon on failure (D-46, D-49); a log assertion reads the decoded record (E-73) and another runs after `Close` (D-60); the chaos tests' phase-2 trigger repeats until the gate parks the adoption (C-39, a race the code shows: review sweep I-1). The intermittent `TestProfilingRoutesOptInAndPrivate` cleanup failure (D-57, F-23) has two fixes, one in production code: the admin server's refresher no longer starts after a `Shutdown` that came during `Serve`'s first refresh (nothing would ever stop it), and the test server's cleanup waits for `Shutdown` and `Serve` before closing the cache. That this ends the flake is not shown: it failed 1 in 60 loaded runs at phase F's base; see Step 8.

**Files:**
- Create: `internal/admin/shutdown_order_test.go`
- Modify: `internal/handler/phase2_chaos_test.go` (new `holdSuiteBriefly`, `triggerUntilParked`; phase 2's trigger), `internal/handler/phase2_detached_chaos_test.go` (phase 2's trigger), `internal/admin/handlers.go` (`startRefresher`), `internal/admin/admin_test.go` (`startAdminServer`, new `newAdminServer`), `internal/admin/refresher_test.go`, `internal/gc/gc_test.go`, `internal/cache/open_cache_test.go`, `internal/cache/connpool_test.go`, `cmd/apt-cacher-ultra/suite_caps_wiring_test.go` (new `probeClient`, `waitServing`), `cmd/apt-cacher-ultra/shutdown_drain_test.go`, `cmd/apt-cacher-ultra/legacy_audit_wiring_test.go`, `cmd/apt-cacher-ultra/upstream_invalid_cert_test.go`, `cmd/apt-cacher-ultra/adopt_unsigned_wiring_test.go`, and in `internal/freshness`: `adoption_test.go`, `adoption_date_test.go`, `capacity_test.go`, `freshness_test.go`, `hot_prefetch_test.go` (a drift file: by hand), `panic_recovery_test.go`, `phase6_7_skip_repair_test.go`, `scheduler_test.go`

**Interfaces:**
- Consumes: `discardLogger()` (freshness), `silentBuildLogger()` (cmd), `discardSlog()` (cache), `slog.DiscardHandler`.
- Produces: handler test helpers `holdSuiteBriefly(t *testing.T, stack *phase2ChaosStack, host string, d time.Duration)` and `triggerUntilParked(t *testing.T, stack *phase2ChaosStack, gate *chaos2Gate, srvURL, path string, timeout time.Duration) error`; admin test helper `newAdminServer(t *testing.T, opts ...adminOpt) (*Server, net.Listener, *cache.Cache)`; cmd test helper `waitServing(t *testing.T, addr string, cancel context.CancelFunc, done <-chan error)` and `probeClient`.

- [ ] **Step 1: Measure the noise before**

Run, for each of `./internal/admin ./internal/freshness ./internal/gc ./internal/cache ./cmd/apt-cacher-ultra`: `go test -count=1 -v <pkg> 2>&1 | grep -v -E '^(=== (RUN|PAUSE|CONT|NAME)|--- (PASS|SKIP)|PASS$|ok  )|^ +--- (PASS|SKIP)' | wc -l`
Expected (the replay, at Task 14's end): 2132, 603, 518, 34, 30 — schema-migration INFO lines from caches opened with the default logger, adoption lines from adopters built without one, two TLS handshake errors, and the tests' own `t.Logf`s.

- [ ] **Step 2: Silent loggers (D-48, D-56, F-20)**

The rule, applied by hand to every match (the replay's script made exactly these edits):

1. Every `cache.Open(<ctx>, <dir>, nil)` in a `_test.go` file of `internal/freshness` (23 calls: `adoption_test.go` ×16, `capacity_test.go` ×2, `hot_prefetch_test.go` ×5 — the last by hand, below), `internal/admin` (`admin_test.go` — `startAdminServer`, F-20 — and `refresher_test.go`), `internal/gc` (`gc_test.go`'s `openTestCache`) and `cmd/apt-cacher-ultra` (`legacy_audit_wiring_test.go` ×3, D-48) passes the package's silent logger instead of `nil`: `discardLogger()` in freshness, `slog.New(slog.DiscardHandler)` in admin and gc, `silentBuildLogger()` in cmd. In `internal/cache`, `connpool_test.go`'s `Open(context.Background(), t.TempDir(), nil)` passes `discardSlog()`.
2. Every `AdoptionConfig{…}` literal in an `internal/freshness` test that sets no `Logger` gains `Logger: discardLogger()` (22 literals: `adoption_test.go` ×2, `adoption_date_test.go`, `capacity_test.go` ×4, `freshness_test.go` ×3, `hot_prefetch_test.go`, `panic_recovery_test.go` ×2, `phase6_7_skip_repair_test.go` ×8, `scheduler_test.go`): in a one-line literal as `, Logger: discardLogger()` before its `}`; in a literal whose `}` closes a field line (`HostLimiter: hostsem.New(8), MaxConcurrent: 1})`) as `,` then a `Logger: discardLogger(),` line and the `})` on its own line; otherwise as a `Logger: discardLogger(),` line before the closing brace.
3. `hot_prefetch_test.go`'s four `&Adopter{cache: c, logger: slog.Default()}` (and `logger := slog.Default()`) use `discardLogger()`, and its `"log/slog"` import goes.

Then `gofmt -w` the changed files except `hot_prefetch_test.go`, a drift file: its edits are exact insertions, the new `Logger:` field aligned by hand with its block (`Logger:            discardLogger(),`, values at column 20). Afterwards:

Run: `grep -rn 'cache.Open(.*, nil)' --include='*_test.go' internal cmd; grep -n 'Open(.*, nil)' internal/cache/*_test.go; grep -n 'slog.Default()' internal/freshness/*_test.go`
Expected: no output.

- [ ] **Step 3: The daemon-start helper (D-46, D-49)**

`cmd/apt-cacher-ultra/suite_caps_wiring_test.go`, before `startupLogs`:

```go
// probeClient bounds each startup probe, so a hung request ends inside
// waitServing's 15 s deadline (deferred minor D-49).
var probeClient = &http.Client{Timeout: 2 * time.Second}

// waitServing polls the daemon on addr until it answers a request. After
// 15 s it cancels the daemon and waits (another 15 s at most) for
// serveListeners to return on done, so nothing outlives the test and writes
// to its logs (deferred minor D-46), then fails t. The start-and-probe loop
// of every test that runs the daemon to its listener (deferred minor D-49).
func waitServing(t *testing.T, addr string, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := probeClient.Get("http://" + addr + "/healthz-probe")
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		if time.Now().After(deadline) {
			cancel()
			select {
			case <-done:
			case <-time.After(15 * time.Second):
			}
			t.Fatalf("daemon not serving within 15s: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
```

`startupLogs`' and `shutdown_drain_test.go`'s `TestServeListeners_ShutsDownWithTheBarrierOn`'s poll loops (`deadline := …` through the loop's closing brace) each become `waitServing(t, ln.Addr().String(), cancel, done)`; `shutdown_drain_test.go`'s imports lose `"net/http"`.

- [ ] **Step 4: The TLS lines, the decoded record, the assertion after `Close` (D-58, E-73, D-60)**

`upstream_invalid_cert_test.go` (imports gain `"io"` and `"log"`): the upstream is `httptest.NewUnstartedServer(<the same handler>)`, then

```go
	// Every handshake fails by design: its "TLS handshake error" line is
	// noise in the test output (deferred minor D-58).
	upstream.Config.ErrorLog = log.New(io.Discard, "", 0)
	upstream.StartTLS()
	defer upstream.Close()
```

(`internal/fetch`'s remaining lines are `fetch retry` INFO lines from its own default logger — not a package D-56 names, left.) `adopt_unsigned_wiring_test.go` (imports gain `"encoding/json"`), `TestWarnSuiteCaps_ZeroCapAndReadFailure`:

old:
```go
	if !strings.Contains(logs.String(), `"level":"WARN","msg":"suite_cap_usage_read_failed"`) {
		t.Fatalf("a failed cap read was not a WARN:\n%s", logs.String())
	}
```
new:
```go
	warned := false // the decoded record, not a key-order-dependent substring (deferred minor E-73)
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == "suite_cap_usage_read_failed" && rec["level"] == "WARN" {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("a failed cap read was not a WARN:\n%s", logs.String())
	}
```

`internal/cache/open_cache_test.go` `TestOpenCache_LogsNothing`:

old:
```go
	t.Run("open", func(t *testing.T) {
		var buf bytes.Buffer
		captureDefaultLog(t, &buf)
		openCache(t)
		if buf.Len() != 0 {
			t.Fatalf("openCache logged:\n%s", buf.String())
		}
	})
```
new:
```go
	var buf bytes.Buffer
	t.Run("open", func(t *testing.T) {
		captureDefaultLog(t, &buf)
		openCache(t)
	})
	// Read after the subtest's cleanups: openCache's Close ran while the
	// capture was still on, so a line Close logs is caught too (deferred
	// minor D-60).
	if buf.Len() != 0 {
		t.Fatalf("openCache logged:\n%s", buf.String())
	}
```

- [ ] **Step 5: The refresher after `Shutdown` (F-23, the production change)**

Create `internal/admin/shutdown_order_test.go`:

```go
package admin

import (
	"context"
	"testing"
	"time"
)

// Deferred minor F-23: a Shutdown that comes before the refresher starts —
// while Serve's synchronous first refresh still runs — leaves no refresher
// to start, since nothing would ever stop it.
func TestServer_NoRefresherStartsAfterShutdown(t *testing.T) {
	s, ln, c := newAdminServer(t)
	defer func() { _ = c.Close() }()
	defer func() { _ = ln.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	s.startRefresher() // what Serve does once its first refresh returns
	s.mu.Lock()
	started := s.refresherDone != nil
	s.mu.Unlock()
	if started {
		t.Fatal("a refresher started after Shutdown: nothing stops it")
	}
}
```

`internal/admin/admin_test.go`: `startAdminServer`'s body up to and including `s, err := New(cfg)` and its error check becomes `newAdminServer` (returning `s, ln, c`), and `startAdminServer` is:

```go
func startAdminServer(t *testing.T, opts ...adminOpt) (*Server, string, func()) {
	t.Helper()
	s, ln, c := newAdminServer(t, opts...)
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = s.Serve(ln)
	}()
	cleanup := func() {
		// Deferred minor F-23: the cache closes only once the server is
		// done — Shutdown, its refresher included, and Serve, whose first
		// refresh runs before it listens — or the TempDir cleanup can meet
		// a WAL file still open (directory not empty). Thirty seconds, and
		// a slow shutdown is reported, not ignored.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Errorf("admin Shutdown: %v", err)
		}
		select {
		case <-served:
		case <-ctx.Done():
			t.Errorf("admin Serve still running 30s after Shutdown")
		}
		_ = c.Close()
	}
	return s, "http://" + ln.Addr().String(), cleanup
}

// newAdminServer constructs an admin Server over a real cache and gc.GC,
// with an ephemeral listener it does not serve.
func newAdminServer(t *testing.T, opts ...adminOpt) (*Server, net.Listener, *cache.Cache) {
```

Run: `go test -count=1 -run TestServer_NoRefresherStartsAfterShutdown ./internal/admin/`
Expected: FAIL with `shutdown_order_test.go:26: a refresher started after Shutdown: nothing stops it`.

`internal/admin/handlers.go` `startRefresher`:

old:
```go
	s.mu.Lock()
	if s.refresherStop != nil {
		s.mu.Unlock()
		return
	}
```
new:
```go
	s.mu.Lock()
	// A Shutdown that came first — while Serve's synchronous first
	// refresh ran — had no refresher to stop, and nothing would stop this
	// one: start none (deferred minor F-23).
	if s.refresherStop != nil || s.shutdownStarted {
		s.mu.Unlock()
		return
	}
```

- [ ] **Step 6: The chaos tests' phase-2 trigger (C-39, review sweep I-1)**

Both chaos tests start phase 2's adoption with one anchor GET. Its T1 check is dropped (`resultBusy`: the check's TryLock peek) while a phase-1c T1 check still holds the suite after its 304 — `stack.checker.WaitForAdoptions()` waits for adoptions, not for those checks — and then no adoption starts and `WaitForWaiter(5 * time.Second)` fails: the "unexplained" failure C-39 recorded, a window load widens. `waitForFlip` already nudges every 20 ms for this reason; phase 2's trigger now does too, and the tests hold the suite for 100 ms across the first trigger GET, so the window the fix closes is certain in every run. A test-only change.

At the end of `internal/handler/phase2_chaos_test.go`:

```go
// holdSuiteBriefly holds the chaos suite's mutex for d, from a goroutine,
// as a phase-1c T1 check still finishing its writes would (deferred minor
// C-39). It retries the hold (HoldSuite does not wait) for up to 5 s.
func holdSuiteBriefly(t *testing.T, stack *phase2ChaosStack, host string, d time.Duration) {
	t.Helper()
	key := cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: host, SuitePath: chaos2Suite}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if release, ok := stack.checker.HoldSuite(key); ok {
			go func() {
				time.Sleep(d)
				release(false)
			}()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("could not hold the chaos suite")
		}
		time.Sleep(time.Millisecond)
	}
}

// triggerUntilParked GETs path every 20 ms until the member-fetch gate
// parks an adoption, for at most timeout (deferred minor C-39): one GET's
// T1 check is dropped (resultBusy) while anything else holds the suite,
// and then no adoption starts — the reason waitForFlip nudges too. Every
// answer must be 200.
func triggerUntilParked(t *testing.T, stack *phase2ChaosStack, gate *chaos2Gate, srvURL, path string, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		rec := httptest.NewRecorder()
		stack.handler.ServeHTTP(rec, proxyReq("GET", srvURL, path))
		if rec.Code != http.StatusOK {
			t.Fatalf("trigger adoption %s: status=%d body=%q", path, rec.Code, rec.Body.String())
		}
		if gate.WaitForWaiter(20*time.Millisecond) == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no waiter arrived within %v", timeout)
		}
	}
}
```

`phase2_chaos_test.go`, phase 2:

old:
```go
	memberFetchGate.Close()
	current.Store(&snapB)

	rec := httptest.NewRecorder()
	stack.handler.ServeHTTP(rec, proxyReq("GET", srv.URL, chaos2Suite+"/InRelease"))
	if rec.Code != http.StatusOK {
		t.Fatalf("trigger adoption: status=%d body=%q", rec.Code, rec.Body.String())
	}
	if err := memberFetchGate.WaitForWaiter(5 * time.Second); err != nil {
		t.Fatalf("adoption never reached member-fetch gate: %v", err)
	}
```
new:
```go
	memberFetchGate.Close()
	current.Store(&snapB)

	// Deferred minor C-39: a phase-1c T1 check may still hold the suite
	// when the trigger's T1 check peeks, and that check is then dropped
	// (resultBusy: the check's TryLock peek), so no adoption starts. Hold
	// the suite for 100 ms across the first trigger GET, making the window
	// certain, and trigger until the gate parks the adoption.
	holdSuiteBriefly(t, stack, upstreamURL.Hostname(), 100*time.Millisecond)
	if err := triggerUntilParked(t, stack, memberFetchGate, srv.URL, chaos2Suite+"/InRelease", 5*time.Second); err != nil {
		t.Fatalf("adoption never reached member-fetch gate: %v", err)
	}
```

`phase2_detached_chaos_test.go`, phase 2:

old:
```go
	memberFetchGate.Close()
	current.Store(&snapB)

	rec := httptest.NewRecorder()
	stack.handler.ServeHTTP(rec, proxyReq("GET", srv.URL, chaos2Suite+"/Release"))
	if rec.Code != http.StatusOK {
		t.Fatalf("trigger detached adoption: status=%d body=%q", rec.Code, rec.Body.String())
	}
	if err := memberFetchGate.WaitForWaiter(5 * time.Second); err != nil {
		t.Fatalf("detached adoption never reached member-fetch gate: %v", err)
	}
```
new:
```go
	memberFetchGate.Close()
	current.Store(&snapB)

	// Deferred minor C-39: a phase-1c T1 check may still hold the suite
	// when the trigger's T1 check peeks, and that check is then dropped
	// (resultBusy: the check's TryLock peek), so no adoption starts. Hold
	// the suite for 100 ms across the first trigger GET, making the window
	// certain, and trigger until the gate parks the adoption.
	holdSuiteBriefly(t, stack, upstreamURL.Hostname(), 100*time.Millisecond)
	if err := triggerUntilParked(t, stack, memberFetchGate, srv.URL, chaos2Suite+"/Release", 5*time.Second); err != nil {
		t.Fatalf("detached adoption never reached member-fetch gate: %v", err)
	}
```

Red first: with the hold in place but the single trigger GET kept (the old blocks with the hold's two statements added before them), run `go test -race -count=1 -run 'TestPhase2Chaos_AdoptionFlipUnderConcurrency$|TestPhase2DetachedChaos_AdoptionFlipUnderConcurrency$' ./internal/handler/`
Expected: FAIL —
```
phase2_chaos_test.go:185: adoption never reached member-fetch gate: no waiter arrived within 5s
phase2_detached_chaos_test.go:178: detached adoption never reached member-fetch gate: no waiter arrived within 5s
```

Then with the new blocks, the same command: both `PASS` (replay: 3.16 s and 3.68 s). And `go test -race -count=20 -run '…the same two…' ./internal/handler/`: `ok` (replay: 133.7 s for the forty runs).

- [ ] **Step 7: Run and measure after**

Run: `go test -count=1 ./...`
Expected: every package `ok`.

Run Step 1's command again.
Expected: 18, 5, 0, 20, 0 — each remaining line a test's `t.Logf` (`budget_test.go`'s sizes and contrasts, four `current_form_test.go` lines and one `sim_sched_test.go` line, the chaos tests' orderings).

Run: `go test -race -count=1 ./internal/admin/ ./internal/gc/ ./internal/cache/ ./internal/freshness/ ./cmd/...`
Expected: all `ok`.

- [ ] **Step 8: Mutation checks** (one at a time, each reverted)

| Mutant | Fails |
|---|---|
| `Cache.Close` logs `slog.Info("cache closing")` (D-60) | `open_cache_test.go:41: openCache logged: …` |
| `startRefresher` without `\|\| s.shutdownStarted` (F-23) | `shutdown_order_test.go:26: a refresher started after Shutdown: nothing stops it` |
| Phase 2's trigger back to one GET, the hold kept (C-39) | `phase2_chaos_test.go:185: adoption never reached member-fetch gate: no waiter arrived within 5s` and `phase2_detached_chaos_test.go:178: detached adoption never reached member-fetch gate: …` (Step 6's red) |

D-46 and D-49 change only a failure path; D-48, D-56, D-58 and F-20 are pinned by Step 7's counts; E-73 tolerates a key order no mutant changes. Whether F-23's two fixes end the profiling test's flake cannot be shown here: it reproduced 1 in 60 loaded runs at phase F's base and never at its head; the race they close (the cache closed while `Serve`'s first refresh, which `Shutdown` did not wait for, still ran) is the likely cause the item names, not a proven one.

- [ ] **Step 9: Commit**

```bash
gofmt -l internal/admin internal/gc internal/cache/open_cache_test.go internal/cache/connpool_test.go cmd/apt-cacher-ultra/suite_caps_wiring_test.go cmd/apt-cacher-ultra/shutdown_drain_test.go cmd/apt-cacher-ultra/legacy_audit_wiring_test.go cmd/apt-cacher-ultra/upstream_invalid_cert_test.go cmd/apt-cacher-ultra/adopt_unsigned_wiring_test.go internal/freshness/adoption_test.go internal/freshness/adoption_date_test.go internal/freshness/capacity_test.go internal/freshness/freshness_test.go internal/freshness/panic_recovery_test.go internal/freshness/phase6_7_skip_repair_test.go internal/freshness/scheduler_test.go internal/handler/phase2_chaos_test.go internal/handler/phase2_detached_chaos_test.go
git add internal/handler/phase2_chaos_test.go internal/handler/phase2_detached_chaos_test.go internal/admin/handlers.go internal/admin/admin_test.go internal/admin/refresher_test.go internal/admin/shutdown_order_test.go internal/gc/gc_test.go internal/cache/open_cache_test.go internal/cache/connpool_test.go cmd/apt-cacher-ultra/suite_caps_wiring_test.go cmd/apt-cacher-ultra/shutdown_drain_test.go cmd/apt-cacher-ultra/legacy_audit_wiring_test.go cmd/apt-cacher-ultra/upstream_invalid_cert_test.go cmd/apt-cacher-ultra/adopt_unsigned_wiring_test.go internal/freshness/adoption_test.go internal/freshness/adoption_date_test.go internal/freshness/capacity_test.go internal/freshness/freshness_test.go internal/freshness/hot_prefetch_test.go internal/freshness/panic_recovery_test.go internal/freshness/phase6_7_skip_repair_test.go internal/freshness/scheduler_test.go
git commit -m "$(cat <<'EOF'
Tests: silent output, one daemon-start helper; no refresher after Shutdown

Caches opened in tests take a silent logger and adopters get one, so
go test -v prints only the tests' own lines in admin, freshness, gc,
cache and cmd (D-48, D-56, F-20; 2132/603/518/34/30 -> 18/5/0/20/0);
the invalid-cert test's server discards its handshake errors (D-58).
The cmd tests start the daemon through waitServing, with a timed client
and a drain on failure (D-46, D-49). A log assertion decodes its record
(E-73) and another runs after Close (D-60). The admin server no longer
starts a refresher after a Shutdown that came during Serve's first
refresh, and the test server's cleanup waits for Shutdown and Serve
before closing the cache (D-57, F-23). The chaos tests' phase-2 trigger
repeats every 20 ms until the gate parks the adoption, with the suite
held across its first GET: one GET's T1 check was dropped while a
phase-1c check still held the suite, and no adoption started (C-39).

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

### Task 16: Comment nits and `deferred_e_test.go`'s move (A-10, A-26, A-27, D-28, D-29, D-30, E-35, E-41, E-46, E-74)

Comment and naming fixes, one redundant filter removed (A-26: behaviour-neutral, the SQL already guarantees it), and the move of `internal/freshness/deferred_e_test.go`'s ten functions beside their subjects (E-46): the file's name points at a document Task 18 deletes. No test is added or lost: `go test -list . ./internal/freshness/` prints the same 682 names before and after.

**Files:**
- Delete: `internal/freshness/deferred_e_test.go`
- Modify: `internal/proxy/proxy.go`, `internal/cache/legacy_audit.go` (`ListTrackedSuiteAnchors`, imports), `internal/freshness/legacy_audit.go` (`RunLegacyTrustAudit`'s doc), `internal/handler/phase2_handler_test.go`, `internal/handler/fetch_seam_test.go`, `internal/handler/phase2_migration_test.go`, `internal/freshness/sim_burst_test.go`, `cmd/apt-cacher-ultra/main_test.go`, and the E-46 targets `internal/freshness/adoption_failure_test.go`, `panic_recovery_test.go`, `legacy_audit_test.go`, `retry_test.go`, `reconcile_test.go`, `flip_barrier_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: nothing new; `unlogged()` (Task 9's tests use it) now lives in `adoption_failure_test.go`.

- [ ] **Step 1: Record the freshness test list**

Run: `go test -list . ./internal/freshness/ | sort > /tmp/h1-list-before.txt; wc -l < /tmp/h1-list-before.txt`
Expected: 682 (after Tasks 1–15).

- [ ] **Step 2: Production comments and the filter (A-10, A-26, A-27)**

`internal/proxy/proxy.go`, the control-byte loop's comment becomes an anchor:

```go
	// AIDEV-NOTE: spec C1 — a decoded control byte is never a legitimate
	// apt path (apt percent-encodes them) and must not reach the cache key
	// (deferred minor A-10).
```

`internal/cache/legacy_audit.go` `ListTrackedSuiteAnchors`: `if a.SeenAt > 0 && (strings.HasSuffix(a.Path, "/InRelease") || strings.HasSuffix(a.Path, "/Release.gpg")) {` becomes `if a.SeenAt > 0 { // the IN above picks InRelease and Release.gpg only (deferred minor A-26)`, and the file's `"strings"` import goes (its only use). `internal/freshness/legacy_audit.go` `RunLegacyTrustAudit`'s doc: `An error means the database failed or the pool is missing: the` reads `An error means the database failed or the pool is missing or` / `// empty (preflightLegacyAudit; deferred minor A-27): the` (A-27 names `cache/legacy_audit.go:48`; the sentence it means is this one, in the freshness file).

- [ ] **Step 3: Test comments and names (D-28, D-29, D-30, E-35, E-41, E-74)**

`internal/handler/phase2_handler_test.go`: the local `staleBody` (three uses, the first `staleBody := simtest.ClearsignedFixture("stale Phase 1 InRelease")`) is `staleAnchor`, the first use commented `// not staleBody: that is the package's helper (deferred minor D-28)`; `TestServeHTTP_AdoptedSuiteSnapshotMemberDBError_FailsClosed`'s upstream writes `simtest.ClearsignedFixture("attacker bytes")` under

```go
		// An InRelease, so a clearsign envelope like its sibling's (ruling
		// PD-2; deferred minor D-30) — though never read: the test asserts
		// no upstream call.
```

`internal/handler/fetch_seam_test.go`: `recFetcher`'s doc ends `… and answers every request with body — but an …/InRelease target with` / `// inRelease, when that is set (deferred minor D-29).` `internal/handler/phase2_migration_test.go`, `waitForMigrationFlip`'s doc (E-35: its first half contradicted its second):

```go
// waitForMigrationFlip polls suite_freshness.current_snapshot_id until
// the current snapshot's InRelease is want, nudging with InRelease GETs to
// keep maybeFireFreshness firing in case the per-suite TryLock missed
// earlier attempts. The first post-migration hit already adopts A (spec C4
// eager first adoption), so a non-NULL pointer alone proves nothing: it
// waits for the snapshot whose InRelease is want (deferred minor E-35).
```

`internal/freshness/sim_burst_test.go`, `TestSimCapacity_UpgradeBurstSameHost`'s item 4:

```go
//  4. lost offers in all are at most the admissions in all — each lost
//     offer but hx's coincides with another admission (work-conserving;
//     hx's coincides with the test's synthetic lease, which the admission
//     count leaves out: deferred minor E-41) — and at least one: hx's,
//     which offerHook counts exactly (its probes are the tick's, its lost
//     offers' and the winning offer's);
```

`cmd/apt-cacher-ultra/main_test.go`, `minimalCfg`'s doc ends `… not through serve. Unlike the production defaults, adoption.` / `// adopt_unsigned is false here (the struct literal leaves it unset): a cmd` / `// test that expects the default sets it (deferred minor E-74).`

- [ ] **Step 4: Move `deferred_e_test.go`'s functions (E-46)**

Each function, with its doc comment, moves verbatim — Task 9's version of `TestAdoptionFailureRecordCutByShutdownIsNotAWarning` included — to the end of the file of its subject:

| Function | To |
|---|---|
| `unlogged` (helper), `TestAdoptionPanicWithABrokenLoggerIsContained`, `TestAdoptionFailureRecordCutByShutdownIsNotAWarning`, `TestAdoptionFailureRecordReadCutByShutdownIsNotAWarning` | `adoption_failure_test.go` |
| `TestHeartbeatPanicIsLoggedWithItsStackOnce` | `panic_recovery_test.go` |
| `TestRunLegacyTrustAudit_UnsignedKeepIsNotSigned`, `TestRunLegacyTrustAudit_ErrorsCarryThePrefix` | `legacy_audit_test.go` |
| `TestJitterDelay_ClampsTheRandomValue` | `retry_test.go` (`jitterDelay` is in `retry.go`) |
| `TestCheck_ReconcileOnTheTickParsesTheReleaseOnce` | `reconcile_test.go` |
| `TestRunRequest_CancelledWithoutAStopContextCommitsNothing` | `flip_barrier_test.go` (`stopErr`'s rule) |

Then delete `deferred_e_test.go`. The targets' imports gain what the moved code uses: `adoption_failure_test.go` `"log/slog"` and `"strconv"`; `panic_recovery_test.go` `"testing/synctest"` and `…/internal/simtest`; `retry_test.go` `"math"`; `reconcile_test.go` `"sync/atomic"`. `gofmt -w` the six targets (none is a drift file).

- [ ] **Step 5: Verify**

Run: `go test -list . ./internal/freshness/ | sort | diff /tmp/h1-list-before.txt -`
Expected: no output.

Run: `go test -count=1 ./internal/freshness/ ./internal/cache/ ./internal/handler/ ./internal/proxy/ ./cmd/...`
Expected: all `ok`.

Run: `grep -rn 'deferred_e_test' --include='*.go' . ; gofmt -l cmd internal`
Expected: no `deferred_e_test` reference; the five drift files only.

No mutation step: nothing here changes behaviour (A-26's filter repeated the SQL's `IN`; its `SeenAt > 0` test stays).

- [ ] **Step 6: Commit**

```bash
git rm internal/freshness/deferred_e_test.go
git add internal/proxy/proxy.go internal/cache/legacy_audit.go internal/freshness/legacy_audit.go internal/handler/phase2_handler_test.go internal/handler/fetch_seam_test.go internal/handler/phase2_migration_test.go internal/freshness/sim_burst_test.go cmd/apt-cacher-ultra/main_test.go internal/freshness/adoption_failure_test.go internal/freshness/panic_recovery_test.go internal/freshness/legacy_audit_test.go internal/freshness/retry_test.go internal/freshness/reconcile_test.go internal/freshness/flip_barrier_test.go
git commit -m "$(cat <<'EOF'
Comment nits; deferred_e_test.go's tests move beside their subjects

proxy.go's control-byte invariant is an AIDEV-NOTE (A-10);
ListTrackedSuiteAnchors drops a suffix filter its SQL already applies
(A-26); RunLegacyTrustAudit's doc says missing or empty (A-27); test
names and docs fixed (D-28, D-29, D-30, E-35, E-41, E-74). The ten
functions of deferred_e_test.go, a file named for a document phase H
deletes, move verbatim to the test files of their subjects (E-46).

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

### Task 17: Phase H1's definition of done

No new code: the brief's mandatory DoD, mapped to the tests that prove each item, and the checks that span the tasks, run on the tree at the end of Task 16. Run Step 1 before Task 18, which deletes the seven files it reads. Every output below is the replay's (the revision of 2026-10-25: a fresh copy of eadfd80, whose code is 6786529's, with Tasks 1–16 applied).

**Files:** none. The task changes no file and has no commit; its outputs go in the phase's final report.

**Interfaces:**
- Consumes: the disposition table above; every task's tests.
- Produces: nothing.

**The DoD map:**

| DoD item | Task | Proof |
|---|---|---|
| Every deferred ID has a disposition, and every `fix` row maps to a task | the table; 1, 4–16, 18 | Step 1 |
| F-24's probe fails at HEAD and passes after Task 1 | 1 | `TestRunSnapshotGCBatch_KeepsAuditDeferredSignedHistory`: Task 1 Step 2 (red at 6786529), Step 4 (green) |
| R-H2's tests: the rows of a corrupt anchor are gone after the audit, a client request misses and refetches, an oversized anchor keeps its rows | 2 | `TestApplyLegacyAuditDecisions_CorruptAnchorDropsEveryRowOfItsBlob`, `TestApplyLegacyAuditDecisions_DropAnchorRowsNeedsAQuarantine`, `TestAuditLegacySnapshot_OnlyACorruptAnchorDropsItsRows`, `TestRunLegacyTrustAudit_CorruptAnchorDropsItsRowsOversizedKeepsThem`, `TestServeHTTP_ACorruptAnchorQuarantineRefetchesTheAnchor` |
| R-H3's and R-H6's renames, with `docs/log-fields.md` updated; a new prose message fails the build | 3 | `TestSweptEventNames` (no old name in any Go file, each new name in production code, each with a `docs/log-fields.md` row), `TestLogMessagesAreSnakeCase` (every message snake_case or one of `main`'s 78) |
| The admin `retired_reaped` beside `displaced_reaped`: JSON, GC panel, `docs/admin-ui-spec.md` | 4, 18 | `TestGCInfoFromSummary_CopiesEveryShownField`, `TestStatusHTML_ShowsTheSweepsCells`, `TestJSONContractPreserved` (its golden regenerated in Task 4); admin-ui-spec §4.6 (Task 18) |
| The anchorless refresh visible in `suites[]` and on the page, additive | 4 | `TestSuiteEntries_Anchorless`, `TestStatusHTML_ShowsTheSweepsCells`, `TestJSONContractPreserved` |
| `docs/log-fields.md`'s gaps: `adoption_member_skipped` (with `declared_sha512`), `snapshot_index_target_404` (the layout-aware architecture), `admin_reconcile_requested` | 18 | Task 18's log-fields diff; `admin_reconcile_requested`'s fields are pinned by `TestEndpoint_Reconcile_LogsBothSchemeFields` (Task 5) |
| C-38 under the budget, no assertion dropped | 13 | `TestSimScheduler_FiveHundredStallingOneHealthy`: Step 5 |
| `go test -race ./...` and `golangci-lint run ./...` clean; `gofmt -l .` shows only the five drift files | all | Steps 2–4 |
| Every simulation under 4.5 s with `-race` | 13 (R-H4) | Step 5 |
| `TestCheck_BothSignedFormsFailingUnderJitterStayWithinTheirBounds` under 4.5 s with `-race`, every assertion kept (R-H7) | 13 | Task 13 Step 8's table; Step 5 |
| GD-5 recorded as decided (R-H5), for the maintainer's confirmation | 18 | spec §10 item 12, the overview's decided entries |
| The docs of H1's own changes (`docs/log-fields.md`, `docs/configuration.md`, `docs/admin-ui-spec.md`, the conffile) and spec revision 12 | 18 | Task 18 Steps 2–7; `TestEveryEventIsDocumented` (every snake_case event has a `docs/log-fields.md` row, `main`'s 87 without one listed; review sweep M-11) |
| R-H2 heals an unchanged upstream (review C-1) | 2 | `TestServeHTTP_ACorruptAnchorQuarantineRefetchesTheAnchor`'s `upstream unchanged` case; Task 2 Step 6's first mutant |
| The seven deferred files deleted; the overview updated | 18 | Task 18 Steps 8 and 9 |

- [ ] **Step 1: Every deferred ID has exactly one row, and every `fix` row's ID is named in its task**

Run (from the repository root):

```bash
grep -hoE '^- \*\*[A-G]D?-[0-9]+\*\*' docs/superpowers/plans/2026-09-28-flat-repositories-[A-G]-deferred.md | sed -E 's/^- \*\*(.*)\*\*$/\1/' | sort > /tmp/h1-deferred-ids
awk '/^## Disposition of every deferred minor/,/^## For H3/' docs/superpowers/plans/2026-09-28-flat-repositories-H1-sweep.md | grep -oE '^\| [A-G]D?-[0-9]+ \|' | sed -E 's/^\| (.*) \|$/\1/' | sort > /tmp/h1-table-ids
wc -l < /tmp/h1-deferred-ids; sort -u /tmp/h1-table-ids | wc -l; diff /tmp/h1-deferred-ids /tmp/h1-table-ids && echo "same IDs"
awk '/^## Disposition of every deferred minor/,/^## For H3/' docs/superpowers/plans/2026-09-28-flat-repositories-H1-sweep.md | grep -E '^\| [A-G]D?-[0-9]+ \|' | awk -F'|' '{print $4}' | grep -oE '^ *(fix|done|declined|maintainer)' | sed -E 's/^ +//' | sort | uniq -c
```

Expected:

```
136
136
same IDs
     23 declined
      2 done
    111 fix
```

Then run:

```bash
python3 - docs/superpowers/plans/2026-09-28-flat-repositories-H1-sweep.md <<'EOF'
import re, sys
plan = open(sys.argv[1], encoding="utf-8").read()
table = plan.split("## Disposition of every deferred minor", 1)[1].split("## For H3", 1)[0]
tasks = {int(m.group(1)): m.group(0) for m in re.finditer(r"^### Task (\d+):.*?(?=^### Task |\Z)", plan, re.M | re.S)}
rows = bad = 0
for m in re.finditer(r"^\| ([A-G]D?-\d+) \|[^|]*\| fix \(T(\d+)", table, re.M):
    rows += 1
    if not re.search(r"(?<![\w-])" + re.escape(m.group(1)) + r"(?!\d)", tasks.get(int(m.group(2)), "")):
        bad += 1
        print(f"{m.group(1)}: fix (T{m.group(2)}), not named in Task {m.group(2)}")
print(f"{rows} fix rows, {bad} not named in their task")
EOF
```

Expected: `111 fix rows, 0 not named in their task`.

- [ ] **Step 2: The race detector over the module**

Run: `go test -race -count=1 ./...`
Expected: every package `ok` (or `no test files`), no `FAIL`. Replay: 21 packages `ok`, no `FAIL` (its `-v` twin printed no `--- FAIL` line).

- [ ] **Step 3: The linter**

Run: `golangci-lint run ./...`
Expected: `0 issues.`

- [ ] **Step 4: Formatting**

Run: `gofmt -l .`
Expected: exactly the five drift files of the Global Constraints:

```
cmd/apt-cacher-ultra/ca_print.go
internal/cache/cache_test.go
internal/cache/gc_urlpath_test.go
internal/freshness/hot_prefetch_test.go
internal/proxy/connect_stats.go
```

- [ ] **Step 5: Every simulation under 4.5 s with `-race`**

On a quiet machine (no other build or test running), run:

```bash
go test -race -p 1 -count=3 -v -run '^(TestSim[A-Za-z0-9_]*|TestDoD_[A-Za-z0-9_]*|TestServeHTTP_Minted[A-Za-z0-9_]*AtScale|TestLedger_[A-Za-z0-9_]*|TestCheck_BothSignedFormsFailingUnderJitterStayWithinTheirBounds)$' ./internal/flipgate/ ./internal/freshness/ ./internal/handler/ ./internal/simtest/ 2>&1 | grep -E '^--- ' | sort -t'(' -k2 -rn | head -20
```

Expected: PASS, every line under 4.5 s. Then record, for the release notes and the next coarsening, the machine's CPU count (`nproc`) and the three simulations closest to the budget — `TestSimScheduler_StallingHostWithAdoptionContention`, `TestDoD_NinetyDaysOfReapingWithRestarts`, `TestSimRetry_LongRunDateRulesAndClockSteps` — from this run (`-p 1`); any above 4.0 s is flagged as the next candidate for coarsening (review M-5: R-H7's own reason, the risk a 4-vCPU runner exposes, applies to them). In Step 2's module-wide run packages run in parallel and simulations take up to twice as long; the budget is this step's. A line over 4.5 s means a busy machine first: rerun that test with `-count=5`; an over-budget time that repeats on a quiet machine fails the DoD.

Replay (the revision of 2026-10-25, a quiet machine with 12 CPUs, load below 1): 50 tests, three runs each, all PASS. The slowest (min–max): `TestServeHTTP_MintedSuitesAtScale` 3.89–3.96 s (untouched by H1; phase G recorded 3.93–4.06 s), `TestDoD_NinetyDaysOfReapingWithRestarts` 3.69–3.76 s, `TestSimRetry_LongRunDateRulesAndClockSteps` 3.63–3.70 s, `TestSimFlip_DetachedBusySuiteStraddlesOnlyWithoutTheBarrier` 3.48–3.56 s, `TestSimScheduler_StallingHostWithAdoptionContention` 3.51–3.55 s, `TestSimCapacity_UpgradeBurst` 3.24–3.27 s, `TestSimFlappingMirror_NeverFlipsBack` 3.14–3.25 s, `TestSimScheduler_FiveHundredStallingOneHealthy` 2.95–2.97 s (C-38); every other one under 3 s. `TestCheck_BothSignedFormsFailingUnderJitterStayWithinTheirBounds` (R-H7), measured on its own after its coarsening: 3.39–3.46 s. Review M-5's three: none above 4.0 s on this machine; `TestServeHTTP_MintedSuitesAtScale`, the closest to the budget, is phase D's and untouched here. A 4-vCPU runner is slower: the first to coarsen there are the Minted test and the 90-day DoD.

### Task 18: Documentation, spec revision 12, the overview, the deferred files (C-36, C-37, D-52, D-55, E-24)

The operator documentation of H1's own changes (Task 3 renamed the events in the docs already), spec revision 12, the overview's H1 part, two code comments that name a deferred file, and the deletion of the seven deferred files. A guard test comes first: every snake_case event production code logs must have a `docs/log-fields.md` row, `main`'s 87 without one listed (review sweep M-11); it fails until this task's rows land, and it found one more gap, phase E's `adoption_repair_declarations_unavailable`. README and the release notes are H3's ("For H3" above lists what they must say).

**Every deferred minor, as this plan leaves it.** All 136 have their row in the disposition table (Task 17 Step 1 checked it before this task deletes the files). Taken in this task: C-36 (`docs/admin-ui-spec.md` §4.6: a Recent Adoptions duration includes the flip barrier's wait), C-37 (the conffile's `max_duration` and `keep_displaced` comments), D-52 (the detached-only wording in `docs/configuration.md` and spec C3), D-55 (the overview's (D) H item moves under Carry-forwards' **H:**, closed by R-H3), E-24 (documented in *Eager first adoption*, design note 13); and the documentation halves of E-75 (spec: C-38's measurement is Task 13's), GD-2 (spec C5) and GD-5 (declined by ruling R-H5, for the maintainer's confirmation: spec §10 item 12 and the overview record the decision).

**Files:**
- Create: `internal/observability/event_docs_test.go`
- Modify: `docs/configuration.md`, `docs/log-fields.md`, `docs/admin-ui-spec.md`, `packaging/config/config.toml.default` (comments only), `docs/superpowers/specs/2026-09-27-flat-repositories-design.md`, `docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md`, `internal/handler/wallclock_stamps_test.go` (a comment), `internal/freshness/anchorless.go` (an AIDEV-NOTE)
- Delete: `docs/superpowers/plans/2026-09-28-flat-repositories-{A,B,C,D,E,F,G}-deferred.md`

**Interfaces:**
- Consumes: Task 3's test helpers `logMessages(t)`, `documentedEvents(t)` and `snakeCase`; the names every earlier task produced, as built: the events `suite_reaper_run_complete`, `suite_not_forgotten`, `gc_panicked` (with `where=startup`), `legacy_anchor_discard_failed`, `reconcile_snapshot_read_failed`, `source_paths_skipped` (INFO); the fields `anchor_rows_deleted`, `paced_stale`, `last_requested_unixtime` and `evidence` (`suite_reaped`; a list), `canonical_scheme` (the admin lines); the gauge `acu_suite_reaper_last_run_unixtime`; the JSON fields `suites[].anchorless`, `recent_adoptions[].scheme`, `gc.retired_reaped`; the status texts `checks failing ×k · backoff ends in …` / `backoff over`.
- Produces: `TestEveryEventIsDocumented` and its list `mainEventsWithoutARow`, which a later rename or a new row of one of `main`'s events shrinks.

Each diff below was produced against the tree at the end of Task 17 (Tasks 1–16 applied) and applies with `git apply` (`patch -p1` also does); a hunk that does not apply means an earlier task's docs differ from the replay's — then edit by hand to the `+` text.

- [ ] **Step 1: Write the failing docs test**

Create `internal/observability/event_docs_test.go`:

```go
package observability

import "testing"

// mainEventsWithoutARow are the snake_case events main already logs that
// have no docs/log-fields.md row (some are described in its prose
// sections; they predate the flat-repository branch). Every other event
// must have a row there (review sweep M-11 of the H1 plan); giving one of
// these a row removes it here.
var mainEventsWithoutARow = []string{
	"admin_authenticated",
	"admin_cpu_profile_finished",
	"admin_cpu_profile_started",
	"admin_disabled",
	"admin_profile_write_failed",
	"admin_refresh_complete",
	"admin_request",
	"admin_scrape_error",
	"admin_status_render_failed",
	"admin_unauthenticated_non_loopback",
	"adoption_blocked_serve_target_missing",
	"adoption_byhash_fallback",
	"adoption_form_drift",
	"adoption_gpg_failed",
	"adoption_heartbeat_blobs_failed",
	"adoption_heartbeat_failed",
	"adoption_hot_prefetch_complete",
	"adoption_hot_prefetch_partial",
	"adoption_hot_prefetch_started",
	"adoption_member_repair_failed",
	"adoption_member_repaired",
	"adoption_member_retry",
	"adoption_parse_failed",
	"adoption_reconcile_member_failed",
	"adoption_reconcile_snapshot_displaced",
	"adoption_repair_bump_failed",
	"adoption_repair_pass",
	"adoption_repair_snapshot_displaced",
	"adoption_required_index_target_missing",
	"adoption_short_keyid_fallback",
	"adoption_snapshot_reconciled",
	"adoption_success",
	"adoption_unpinned_suite",
	"adoption_untrusted_signer",
	"adoption_unverified_signer",
	"at_rest_corruption",
	"at_rest_scan_finished",
	"at_rest_scan_started",
	"freshness_check",
	"gc_disabled",
	"gc_pool_malformed_name",
	"gc_pool_misplaced_file",
	"gc_pool_scan_dir_failed",
	"gc_pool_scan_lookup_failed",
	"gc_pool_unexpected_root_file",
	"gc_pool_unexpected_subdir",
	"gc_pool_unlink_failed",
	"gc_pool_unlink_skipped_invalid_hash",
	"gc_run_complete",
	"gc_tick_deadline_reached",
	"gc_tick_failed",
	"hash_validation_failure",
	"hit_path_hash_evicted",
	"hot_prefetch_budget_unbounded",
	"hot_prefetch_deb_failed",
	"hot_prefetch_hash_mismatch",
	"htpasswd_reload_failed",
	"metrics_series_cap_reached",
	"mitm_ca_generated",
	"mitm_ca_generation_failed",
	"mitm_ca_load_failed",
	"mitm_ca_loaded",
	"mitm_ca_lock_timeout",
	"mitm_ca_name_constraints_skipped",
	"mitm_ca_unconstrained_refused",
	"mitm_cert_cache_evicted",
	"mitm_cert_issued",
	"mitm_clock_skew",
	"mitm_connect",
	"package_hash_incomplete_binary_skipped",
	"package_hash_miss",
	"pdiff_index_parse_failed",
	"pdiff_index_parsed",
	"refresher_query_failed",
	"refuse_unvouched_debs_inert",
	"request",
	"schema_migrating",
	"snapshot_member_refetch_mismatch",
	"source_hash_conflict",
	"source_parse_failed",
	"source_parsed",
	"stale_serve",
	"tls_mitm_enabled",
	"tls_mitm_enabled_ca_undistributed",
	"tls_mitm_narrowing_regex_set",
	"unvouched_deb_passthrough_no_coverage",
	"unvouched_deb_refused",
}

// Review sweep M-11 of the H1 plan: every snake_case event production code
// logs has a docs/log-fields.md row (documentedEvents), main's events
// without one excepted; each of those is still logged and still without a
// row, so the list stays exact.
func TestEveryEventIsDocumented(t *testing.T) {
	documented := documentedEvents(t)
	known := map[string]bool{}
	for _, e := range mainEventsWithoutARow {
		known[e] = true
	}
	msgs := logMessages(t)
	for msg, where := range msgs {
		if !snakeCase.MatchString(msg) {
			continue
		}
		switch {
		case !documented[msg] && !known[msg]:
			t.Errorf("%s: event %s has no row in docs/log-fields.md; add one", where, msg)
		case documented[msg] && known[msg]:
			t.Errorf("%s has a docs/log-fields.md row now: remove it from mainEventsWithoutARow", msg)
		}
	}
	for _, e := range mainEventsWithoutARow {
		if _, ok := msgs[e]; !ok {
			t.Errorf("%s is no longer logged: remove it from mainEventsWithoutARow", e)
		}
	}
}
```

Run: `go test -count=1 -run TestEveryEventIsDocumented ./internal/observability/`
Expected: FAIL with ten lines `event_docs_test.go:117: <file>:<line>: event <event> has no row in docs/log-fields.md; add one`, for `admin_reconcile_requested`, `adoption_member_skipped`, `adoption_repair_declarations_unavailable`, `gc_panicked`, `legacy_anchor_discard_failed`, `reconcile_snapshot_read_failed`, `snapshot_index_target_404`, `source_paths_skipped`, `suite_not_forgotten` and `suite_reaper_run_complete` (in no fixed order).

- [ ] **Step 2: `docs/configuration.md`**

*Upgrading an existing cache*: a detached-only repository is untracked only when first seen after the upgrade (D-52); a corrupt stored `InRelease`'s cached copies are deleted, under any scheme or host, and the corrupt file removed, so an unchanged upstream heals too (R-H2, reviews C-1 and sweep M-7); a snapshot the audit cannot read at all keeps its rows and files until the pool is fixed or the suite forgotten (review M-9). *Eager first adoption*: E-24's paragraph (design note 13). The `keep_displaced` row: F-24 (R-H1). *Suite reaping*: the run line and its gauge (F-13). *Forgetting a suite*: 400 for a body that is not a form, 413 for one over 8 KiB (F-5), and a 409's DEBUG line (F-15).

````diff
diff --git a/docs/configuration.md b/docs/configuration.md
--- a/docs/configuration.md
+++ b/docs/configuration.md
@@ -220,9 +220,10 @@
 plain-text "InRelease" (no clearsign line; apt rejects it too) no longer
 tracks a suite, and neither does a repository that publishes only `Release`
 and `Release.gpg` (for example pkg.jenkins.io). Earlier releases tracked
-such a repository from its first `Release` hit; it is now served as before
-but no longer checked for changes or shown on the status page while
-adoption is disabled.
+such a repository from its first `Release` hit; one first seen after the
+upgrade is now served as before but not tracked — not checked for changes
+or shown on the status page — while adoption is disabled, and one an
+earlier release already tracks stays tracked.
 
 At most `max_suites` suites are tracked and at most `max_suites / 10` per
 host. At the total cap, a new suite makes room by evicting the least
@@ -301,6 +302,14 @@
 `Release` (see *Flat repositories*), or, while `adoption.adopt_unsigned`
 is off, shows "refused: unsigned_disabled" instead.
 
+A first check that ends without an answer the cache can act on — a 5xx or
+a timeout while it probes, `Release.gpg` failing after a 200 `Release`,
+say — is a failed check: the scheduler backs off from it, but it records
+no adoption failure, so a check a client triggers past the `cooldown`
+probes again, a full `Release` download included. Client checks are bound
+by the `cooldown` alone; the lasting case, an unsigned `Release` while
+`adoption.adopt_unsigned` is off, is a refusal, which its record paces.
+
 First adoptions never take the last free adoption slot, which is kept for
 suites that already serve a signed snapshot (see *Capacity and
 fairness*): with the default `max_concurrent_adoptions = 2` a first
@@ -687,7 +696,19 @@
 false`, or one carrying declarations outside its signature — or whose pool
 file is missing or corrupt, is quarantined: it stops serving and vouching,
 its suite is served from the ordinary cache, and a `legacy_snapshot_quarantined`
-line names it. A suite quarantined for declarations outside its signature
+line names it. When the stored `InRelease` is corrupt — its bytes no longer
+match the hash recorded for them — the audit also deletes every cached copy
+of it (each `url_path` row that holds that file, under any scheme or host;
+the line's `anchor_rows_deleted` counts them) and removes the corrupt file,
+so the next client update fetches a fresh one from upstream instead of
+being served bytes apt rejects, also when upstream still serves the same
+file (`legacy_anchor_discard_failed` says when the file could not be
+removed); a missing file, or one too large for the audit to read
+(`anchor_oversized`, not proven bad), keeps its rows. A snapshot whose
+pool file cannot be read at all is deferred, not judged: an ERROR
+`legacy_trust_audit_deferred` names it at every start, and it keeps its
+rows and files until the pool is fixed or the suite is forgotten
+(`POST /forget`). A suite quarantined for declarations outside its signature
 adopts again on its first freshness check, because its tampered copy never
 matches a clean upstream; one quarantined because its stored file was lost
 or corrupt waits for the next upstream change, and may have no record that
@@ -819,7 +840,7 @@
 | `snapshot_batch_size` | integer | `10` | Maximum snapshots removed per batch. Snapshot deletion can cascade through many member rows. At least `1`. |
 | `max_tick_duration` | duration | `"5m"` | Budget for startup/periodic GC passes, checked between batches; remaining work resumes later. The startup pass, which runs before the listeners serve, leaves the retired snapshots (of a reaped, evicted or forgotten suite, or one the upgrade's trust audit quarantined) to the periodic passes, normally the first one after the start (see *Suite reaping*); it deletes only a retired one never adopted whose heartbeat went stale, as it does any abandoned adoption candidate. Must be positive. |
 | `blob_grace` | duration | `"5m"` | Grace after a blob's reference count reaches zero before deletion. At least `"1s"`, and greater than twice `heartbeat_interval`. |
-| `keep_displaced` | integer | `3` | Recent displaced snapshots kept per suite for inspection. `0` retains none; must be nonnegative. Does not by itself retain superseded package binaries. Also bounds retained by-hash: a by-hash request the current snapshot lacks is served from the newest `keep_displaced` + 1 displaced snapshots of its suite, and `0` turns that off (see *Flip consistency*). |
+| `keep_displaced` | integer | `3` | Recent displaced snapshots kept per suite for inspection. `0` retains none; must be nonnegative. A displaced snapshot the upgrade's trust audit has not judged yet (one whose pool file it could not read: see *Upgrading an existing cache*) is kept, and takes none of the `keep_displaced` places, until the audit decides it: it may be the suite's only evidence of a signature. Does not by itself retain superseded package binaries. Also bounds retained by-hash: a by-hash request the current snapshot lacks is served from the newest `keep_displaced` + 1 displaced snapshots of its suite, and `0` turns that off (see *Flip consistency*). |
 | `pool_scan_workers` | integer | `4` | Workers for the startup orphan-file scan. At least `1`. |
 | `heartbeat_interval` | duration | `"60s"` | Refresh in-progress adoption liveness and blob protection. Positive and subject to both bounds below. |
 | `url_path_ttl` | duration | `"168h"` | Requests within this window protect cached URL rows. Older or never-requested rows are evaluated against snapshot/version retention and hold grace. `"0s"` disables URL-path expiration; other values must be at least `"1m"`. |
@@ -865,7 +886,10 @@
 tree — a flat repository's root files, never its `./` twin's — in
 batches of 1,000 files), retires its snapshots and deletes its
 row (INFO `suite_reaped`; `acu_suites_reaped_total{reason}` counts the
-reaps). The reaper deletes no package, but a reaped suite's packages
+reaps). Each run that completes logs INFO `suite_reaper_run_complete` and
+sets the gauge `acu_suite_reaper_last_run_unixtime`: an alert on the
+gauge's age — more than two hours, say — catches a reaper that stopped.
+The reaper deletes no package, but a reaped suite's packages
 lose the version retention its snapshot gave them
 (`retention.max_versions_per_package` counts versions in current
 snapshots only; a package another suite's snapshot lists keeps that
@@ -1071,8 +1095,9 @@
     --data-urlencode 'suite=/ubuntu/24.04/prod/dists/noble'
 # 202 Accepted = forgotten (the work is done when the answer comes); the body
 #   names the suite, the signed-path records deleted and the suites that kept them
-# 400 = host or suite missing, a scheme other than http or https, or a host
-#   holding scheme:// (pass the scheme in scheme)
+# 400 = host or suite missing, a scheme other than http or https, a host
+#   holding scheme:// (pass the scheme in scheme), or a body that is not a form
+# 413 = a body over 8 KiB
 # 403 = the header is missing, a browser request from another origin, or,
 #   without htpasswd_file, a host name other than localhost or listen's (use the address)
 # 409 = busy (being checked or adopted: try again) or no such suite (check the
@@ -1090,7 +1115,9 @@
 request refused with 400, 403 or 413 is not), and each suite forgotten
 as INFO `suite_forgotten` — with `settled=true` when its final delete
 committed although the request then failed (500, or 503 at shutdown)
-and a fresh read then found the row gone.
+and a fresh read then found the row gone. A 409 is logged at DEBUG as
+`suite_not_forgotten`, whose `reason` says which case it was (`no_suite`,
+`busy`; `changed` should never occur).
 
 ## `[serve]`: stale responses
 
````

- [ ] **Step 3: `docs/log-fields.md`**

The unlogged-panics counter names the boundaries Task 9 added (E-45, review M-2); the reaper's gauge (F-13); the table's intro names `internal/gc` (F-21); the stale-suites WARN's `paced_stale` (E-30); `freshness_read_anchor_rows_failed` carries `canonical_scheme` on every line and is DEBUG at shutdown on every path (GD-6); `adoption_member_skipped` and `snapshot_index_target_404` (the G carry-forward), `admin_reconcile_requested` (never documented; with F-16's `canonical_scheme`); `reconcile_snapshot_read_failed` (E-31); `clock_step_detected`'s forward step counted (F-17) and its cooldown wording (E-37); `legacy_snapshot_quarantined`'s `anchor_rows_deleted` and the pool file's removal (R-H2, review C-1), with the new ERROR `legacy_anchor_discard_failed`; `adoption_served_anchor_read_failed` at DEBUG on shutdown (E-13); `suite_reaped`'s fields, `evidence` a list (F-18, review M-7); `canonical_scheme` on the two forget lines (F-16) and `suite_not_forgotten` (F-15); `suite_reaper_run_complete` (F-13); `gc_panicked`, with `where=startup` and the rotation (F-21, reviews I-1 and M-1); `source_paths_skipped` at INFO (GD-2, review M-8); `adoption_repair_declarations_unavailable`, a phase E DEBUG line `TestEveryEventIsDocumented` found undocumented.

````diff
diff --git a/docs/log-fields.md b/docs/log-fields.md
--- a/docs/log-fields.md
+++ b/docs/log-fields.md
@@ -193,9 +193,11 @@
 (suites refused within the last 10 minutes, or the last minute for a busy
 refusal; updated within a minute). The
 scheduler adds the unlabelled counter `acu_freshness_unlogged_panics_total`
-(recovered panics — of a scheduled check, an adoption, its failure record
-or the adoption heartbeat — that could not be logged because reporting
-them panicked too: a bug, please report it).
+(recovered panics — of a scheduled check, a scheduler tick, the start-time
+retry spread, an adoption, its counters and admin ring event, its failure
+record, the adoption heartbeat, a repair or reconcile, a suite reaper run or
+`POST /forget` — that could not be logged because reporting them panicked
+too, or that struck the counting itself: a bug, please report it).
 `acu_adoption_duration_seconds` includes the barrier's wait (at most
 140 s). Eager adoption (spec C4, C10) adds `acu_repair_deferred_total{host}`
 (repairs and reconciles of an unchanged suite deferred because no adoption
@@ -206,7 +208,11 @@
 wait is part of the request line's `duration_ms`). Suite reaping (spec C9)
 adds the counter `acu_suites_reaped_total{reason}` (suites the suite
 reaper deleted, by `reason`: `idle`, `non_canonical`, `junk`, settled
-reaps included; see `suite_reaped`) and the counter `acu_gc_retired_reaped_total` (retired
+reaps included; see `suite_reaped`), the unlabelled gauge
+`acu_suite_reaper_last_run_unixtime` (when the suite reaper's last
+completed run ended, as a Unix time — see `suite_reaper_run_complete`;
+absent until the first, an hour after the start: alert on its age, as on
+`acu_gc_last_run_unixtime`'s) and the counter `acu_gc_retired_reaped_total` (retired
 snapshots that snapshot GC deleted: those of reaped, forgotten and
 evicted suites, and those the upgrade's trust audit quarantined), whose
 per-pass count the `gc_run_complete` line carries as `retired_reaped`
@@ -315,9 +321,9 @@
 failure and refusal records and their retry schedule, wall-clock step
 detection, the startup legacy trust audit, the flip barrier, suite rows
 (cap refusals, evictions, reaping and `POST /forget`), a slow shutdown
-drain and the HTTP servers' own diagnostics (logged by
-`internal/freshness`, `internal/handler`, `internal/admin` and the
-daemon's `main`).
+drain, garbage collection's recovered panics and the HTTP servers' own
+diagnostics (logged by `internal/freshness`, `internal/handler`,
+`internal/admin`, `internal/gc` and the daemon's `main`).
 
 | Event | Level | Fields | Meaning |
 |---|---|---|---|
@@ -338,22 +344,25 @@
 | `adoption_repair_list_failed` | WARN | `canonical_host`, `suite_path`, `snapshot_id`, `err` | The repair decision could not list the snapshot's repairable skipped members; no repair runs this check, the next unchanged check asks again. |
 | `freshness_clamp_adoption_retry_failed` | WARN | `canonical_host`, `suite_path`, `err` | A retry time left in the future by a backward clock step could not be clamped; the next check tries again. |
 | `freshness_clear_adoption_failure_failed` | WARN | `canonical_host`, `suite_path`, `err` | Upstream serves the current content again but the failure record could not be cleared; the next check tries again. |
-| `freshness: adopted suites stale beyond threshold` | WARN | `adopted_stale`, `stale_factor`, `refresh` | Logged at each scheduler tick while adopted suites have had no successful check for `stale_factor` × `periodic_refresh` (4 × 15 min by default), or carry a success stamp in the future: the signature of a freeze. Suites in a refusal state that paces them by their record (`anchor_absent`, `unsigned_disabled`) are left out of `adopted_stale`: such a suite is checked only at its record's retries (up to 75 min apart), so its last success ages on purpose; the status page and `adoption_refused` show them. A suite in any other refusal state counts, since its last success ages only when its checks fail. The gauge `acu_freshness_stale_suites{adopted}` counts every stale suite, those included. With adoption disabled, records are inert and every stale adopted suite counts. |
+| `freshness: adopted suites stale beyond threshold` | WARN | `adopted_stale`, `paced_stale`, `stale_factor`, `refresh` | Logged at each scheduler tick while adopted suites have had no successful check for `stale_factor` × `periodic_refresh` (4 × 15 min by default), or carry a success stamp in the future: the signature of a freeze. Suites in a refusal state that paces them by their record (`anchor_absent`, `unsigned_disabled`) are left out of `adopted_stale` and counted in `paced_stale` instead: such a suite is checked only at its record's retries (up to 75 min apart), so its last success ages on purpose; the status page and `adoption_refused` show them. Not logged while `adopted_stale` is 0, whatever `paced_stale` is. A suite in any other refusal state counts, since its last success ages only when its checks fail. The gauge `acu_freshness_stale_suites{adopted}` counts every stale suite, those included: its `adopted="true"` value is `adopted_stale` + `paced_stale`. With adoption disabled, records are inert, every stale adopted suite counts in `adopted_stale` and `paced_stale` is 0. |
 | `freshness: read suite row failed` | WARN | `err`, `canonical_host`, `suite_path` | A freshness check could not read the suite's row: either the existence read a check makes before it takes the suite's lock (a suite is checked only while it has a row), or the full read after it. The check is skipped and writes nothing; the next trigger (a client hit's check or the scheduler) tries again. |
-| `freshness_read_anchor_rows_failed` | WARN | `err`, `canonical_scheme` (not yet on the line of an adopted suite whose current anchor is absent), `canonical_host`, `suite_path` | A check could not read the suite's cached anchor rows (`url_path`). A suite without a snapshot is not probed this time — no request, nothing written; the next check tries again. A suite whose current anchor is absent probes the other forms at their canonical URLs instead. A flat suite without a snapshot has its cached `Packages`, `Sources` and `Contents-*` rows read once, with its anchor rows, before any request — they serve both the test for a client's fetch and the anchorless refresh — and a failure of that read (`err` starts `read flat index rows:`) logs the same line, with the same result. Nothing is stamped, so every check of a suite without a snapshot past the `cooldown` since its last recorded check reads and logs again; none sends a request. For a suite without a snapshot, a read that a shutdown cut short is not logged here (DEBUG only). |
+| `freshness_read_anchor_rows_failed` | WARN | `err`, `canonical_scheme`, `canonical_host`, `suite_path` | A check could not read the suite's cached anchor rows (`url_path`). A suite without a snapshot is not probed this time — no request, nothing written; the next check tries again. A suite whose current anchor is absent probes the other forms at their canonical URLs instead. A flat suite without a snapshot has its cached `Packages`, `Sources` and `Contents-*` rows read once, with its anchor rows, before any request — they serve both the test for a client's fetch and the anchorless refresh — and a failure of that read (`err` starts `read flat index rows:`) logs the same line, with the same result. Nothing is stamped, so every check of a suite without a snapshot past the `cooldown` since its last recorded check reads and logs again; none sends a request. A read that a shutdown cut short is not logged here (DEBUG only). |
 | `freshness_read_current_snapshot_failed` | WARN | `err`, `snapshot_id`, `canonical_host`, `suite_path` | A check whose absent anchor's probe tree found a form it may adopt could not read the suite's current snapshot, so it cannot tell an anchor that merely came back from new content: the check fails (`freshness check failed`) rather than re-adopting the current content. A shutdown that ends the read is not logged here. |
 | `freshness_check_cancelled`, `freshness_check_ended_by_shutdown`, `freshness_check_skipped_no_baseline_anchor_hash`, `freshness_refused_suite_paced_until_retry`, `freshness_deferred_never_adopted_suite_paced_until_retry`, `freshness_never_adopted_suite_dormant_no_recent_interest`, `freshness_never_adopted_suite_dormant_no_client_created_row`, `freshness_anchorless_index_sniff_failed_kept`, `freshness_repair_deferred_no_adoption_slot`, `legacy_trust_audit_cached_anchor_skipped`, `suite_touch_failed` | DEBUG | `canonical_host`; `suite_path`, except for the sniff and the cached anchor, which name their file as `path`; `step` and `err` (`freshness_check_ended_by_shutdown`); `anchor` (`freshness_check_skipped_no_baseline_anchor_hash`); `reason` (the two `…_paced_until_retry`); `err` (`legacy_trust_audit_cached_anchor_skipped`, `suite_touch_failed`) | Routine outcomes, each named by what its message said before phase H1: `freshness_check_cancelled` — a check's request was cancelled (its context ended, as at shutdown), so it wrote nothing; `freshness_check_ended_by_shutdown` — a shutdown ended the check's local `step`: nothing written, not a failed check; `freshness_check_skipped_no_baseline_anchor_hash` — the suite has no cached hash of its `anchor` to compare with, so the check is skipped; `freshness_refused_suite_paced_until_retry` — an adopted suite its record paces (`reason`) is not probed before the record's retry time; `freshness_deferred_never_adopted_suite_paced_until_retry` — a never-adopted suite whose attempt was deferred is not probed until its record's retry or its next refresh check; `freshness_never_adopted_suite_dormant_no_recent_interest` — a never-adopted suite no client asked for in 7 days is dormant; `freshness_never_adopted_suite_dormant_no_client_created_row` — a never-adopted suite without a client-created anchor or flat index row is dormant; `freshness_anchorless_index_sniff_failed_kept` — an anchorless refresh's 200 for the index at `path` failed its sniff, so the cached row is kept (`acu_anchorless_sniff_rejected_total{host}`); `freshness_repair_deferred_no_adoption_slot` — a repair found no adoption slot and waits for a later unchanged check (`acu_repair_deferred_total{host}`); `legacy_trust_audit_cached_anchor_skipped` — the startup marker pass could not read the cached anchor at `path` and went on without it; `suite_touch_failed` — a suite's client-interest stamp could not be written; a later serve writes it. |
 | `adoption_member_mismatch` | WARN | `canonical_host`, `suite_path`, `path`, and `declared_sha256`/`actual_sha256` — or `declared_sha512`/`actual_sha512` when the member's SHA512 is the hash that failed | A member's bytes did not match the hash its `Release` declares (every declared SHA256 and SHA512 is checked), and the adoption fails (outcome `member_mismatch`). Fetched bytes never reach the pool. A pool file reused under its declared SHA256 whose SHA512 differs from the declared one — the `Release`'s two declarations disagree, so a fetch would bring the same bytes — is logged with `declared_sha512`/`actual_sha512`, is not fetched again, and is kept. |
+| `adoption_member_skipped` | WARN | `canonical_host`, `suite_path`, `path`, `declared_sha256`, `declared_sha512` (each empty when the `Release` does not declare that hash), `reason`; `upstream_status` (`4xx`, `4xx_index_target`), `architecture` (`arch_not_in_allowlist`), `detail` (`optional_member_integrity`) | An adoption left one of the `Release`'s members out of the snapshot and went on: `4xx` — upstream answered 404 or 410 for a member apt can do without (a `Contents-*` the archive ships compressed only, say); `4xx_index_target` — the same for a `Packages` or `Sources` index (on a flat suite, its root ones): `apt update` fails for that architecture for the snapshot's lifetime, the snapshot answers its requests 404 (`snapshot_index_target_404`), and `adoption.required_architectures` can refuse such a snapshot instead; `arch_not_in_allowlist` — `adoption.architectures` leaves the member's architecture out (never requested); `optional_member_integrity` — a member apt does not need for `apt update` failed its fetch or hash check and `adoption.tolerate_optional_member_failures` is on (the default; an index's failure stays fatal), `detail` saying how; with `adoption.repair_skipped_members` such a member — unless its `Release` declares it by SHA512 only — is fetched again by a later unchanged check. A skipped member is never served from the snapshot. Counted in `acu_adoption_members_skipped_total{reason}`. |
 | `adoption_reconcile_failed` | WARN | `canonical_host`, `suite_path`, `snapshot_id`, `stage` (`decision`: deciding whether an unchanged suite needs a reconcile; `heal`: the reconcile run after an unchanged check; `on_demand`: `POST /reconcile`), `err` | Reconciling a snapshot's declared index files failed — for example a stored anchor that no longer decodes — so nothing was healed this time; it is tried again on a later unchanged check. |
+| `reconcile_snapshot_read_failed` | WARN | `err`, `snapshot_id`, `canonical_scheme`, `canonical_host`, `suite_path` | `POST /reconcile` could not read the suite's current snapshot to tell whether it is signed; it counts the snapshot as unsigned (the safe direction: it may not take the last free adoption slot, which only a signed snapshot may) and goes on. |
 | `freshness_read_suite_row_after_panic_failed` | WARN | `err`, `canonical_host`, `suite_path` | After a recovered check panic, the best-effort bump of `last_check_at`/`check_fail_count` could not even read the suite's row; the row is left as it was. |
 | `freshness_persist_failure_bump_after_panic_failed` | WARN | `err`, `canonical_host`, `suite_path` | After a recovered check panic, `last_check_at` and `check_fail_count` were bumped in memory but the write failed; the row is left as it was. |
-| `clock_step_detected` | WARN | `stamp`, `canonical_scheme`, `canonical_host`, `suite_path`, `stored_unixtime`, `now_unixtime` | A stored timestamp is more than 2 s in the future (the wall clock stepped backwards); the gate treated it as old. Rate-limited to one line per minute, except the suite reaper's reports (`stamp=last_requested_at` and `stamp=last_seen_at` from the reaper, and `stamp=wall_clock`), which it bounds itself and logs directly, so a check's report in the same minute cannot hide them. For `stamp=adoption_next_retry_at` the stamp is not treated as old: it is clamped to now + 1 h. For `stamp=last_requested_at` (a suite's client-interest stamp, read when the suite cap looks for an idle suite to evict, by the suite reaper, and by the check of a suite without a snapshot) the suite is not evicted, nor reaped as idle — a future stamp never counts as idle, though a non-canonical or junk row is reaped regardless (see `suite_reaped`); the reaper reports the furthest such stamp of a canonical suite at the first run that reaps after a start and again only when it changes — and the check counts a stamp up to 7 days ahead as recent interest; further ahead, the suite is dormant until a client asks for it again. For `stamp=last_seen_at` (a signed-seen record's stamp, read once a day by the suite reaper's sweep of records no suite uses; `suite_path` is the record's repository path, the furthest — of records with the same stamp, the first by scheme, host and path — reported at the first sweep after a start — a day after it — and when it changes; "in the future" is measured against the time of the sweep itself, so the records the run's own reaps moved up minutes earlier are no step) the record counts as seen now — it is never swept — and its stamp is left as it is: it ages out only once the clock has passed it (rewriting it to the stepped-back now would lose signed history once the clock is corrected). For `stamp=wall_clock` (the suite reaper; no suite fields) the wall clock moved more than an hour further than the monotonic clock between two of the reaper's runs — a forward step without a restart, as when a virtual machine or a laptop resumes; `stored_unixtime` is where the wall clock should be, `now_unixtime` where it is — and the reaper deletes nothing, no suite and no record, for an hour, and leaves the step out of the records' age for as long as it runs, so the record sweep never deletes a record early because of it; this one is not rate-limited and not counted in `acu_clock_steps_detected_total`. `acu_clock_steps_detected_total` counts detections, not distinct clock steps or distinct stamps: the same future stamp is detected again every time it is read — every check for a future `last_check_at` or `last_success_at` (the scheduler reads a future `last_success_at` without reporting it), every check of a suite without a snapshot for a future `last_requested_at`, and every creation attempt within eight rows of the global cap for the future `last_requested_at` its eviction scan re-reads (the line stays rate-limited) — so alert on its rate, not its value. |
-| `legacy_snapshot_quarantined` | WARN | `reason` (`legacy_plaintext`, `malformed_envelope`, `unsigned_declarations`, `unparseable`, `anchor_missing`, `anchor_oversized`, `anchor_corrupt`, `unexpected_form`), `snapshot_id`, `canonical_host`, `suite_path`, `anchor_form`, `was_current` | The startup trust audit could not prove a pre-upgrade snapshot signed and clean; it no longer serves or vouches, and its suite is served unadopted until it re-adopts. |
+| `clock_step_detected` | WARN | `stamp`, `canonical_scheme`, `canonical_host`, `suite_path`, `stored_unixtime`, `now_unixtime` | A stored timestamp is more than 2 s in the future (the wall clock stepped backwards); the gate treated it as old. Rate-limited to one line per minute, except the suite reaper's reports (`stamp=last_requested_at` and `stamp=last_seen_at` from the reaper, and `stamp=wall_clock`), which it bounds itself and logs directly, so a check's report in the same minute cannot hide them. For `stamp=adoption_next_retry_at` the stamp is not treated as old: it is clamped to now + 1 h. For `stamp=last_requested_at` (a suite's client-interest stamp, read when the suite cap looks for an idle suite to evict, by the suite reaper, and by the check of a suite without a snapshot) the suite is not evicted, nor reaped as idle — a future stamp never counts as idle, though a non-canonical or junk row is reaped regardless (see `suite_reaped`); the reaper reports the furthest such stamp of a canonical suite at the first run that reaps after a start and again only when it changes — and the check counts a stamp up to 7 days ahead as recent interest; further ahead, the suite is dormant until a client asks for it again. For `stamp=last_seen_at` (a signed-seen record's stamp, read once a day by the suite reaper's sweep of records no suite uses; `suite_path` is the record's repository path, the furthest — of records with the same stamp, the first by scheme, host and path — reported at the first sweep after a start — a day after it — and when it changes; "in the future" is measured against the time of the sweep itself, so the records the run's own reaps moved up minutes earlier are no step) the record counts as seen now — it is never swept — and its stamp is left as it is: it ages out only once the clock has passed it (rewriting it to the stepped-back now would lose signed history once the clock is corrected). For `stamp=wall_clock` (the suite reaper; no suite fields) the wall clock moved more than an hour further than the monotonic clock between two of the reaper's runs — a forward step without a restart, as when a virtual machine or a laptop resumes; `stored_unixtime` is where the wall clock should be, `now_unixtime` where it is — and the reaper deletes nothing, no suite and no record, for an hour, and leaves the step out of the records' age for as long as it runs, so the record sweep never deletes a record early because of it; this one is not rate-limited, and is counted in `acu_clock_steps_detected_total` once per detected step. `acu_clock_steps_detected_total` counts detections, not distinct clock steps or distinct stamps: the same future stamp is detected again every time it is read — every check for a future `last_check_at`, every check the cooldown lets through for a future `last_success_at` (a client's check the cooldown turns away does not report it, nor does the scheduler, which reads it as old), every check of a suite without a snapshot for a future `last_requested_at`, and every creation attempt within eight rows of the global cap for the future `last_requested_at` its eviction scan re-reads (the line stays rate-limited) — so alert on its rate, not its value. |
+| `legacy_snapshot_quarantined` | WARN | `reason` (`legacy_plaintext`, `malformed_envelope`, `unsigned_declarations`, `unparseable`, `anchor_missing`, `anchor_oversized`, `anchor_corrupt`, `unexpected_form`), `snapshot_id`, `canonical_host`, `suite_path`, `anchor_form`, `was_current`, `anchor_rows_deleted` | The startup trust audit could not prove a pre-upgrade snapshot signed and clean; it no longer serves or vouches, and its suite is served unadopted until it re-adopts. For `anchor_corrupt` — the stored `InRelease`'s bytes no longer hash to its recorded SHA-256 — the same transaction also deletes every cached `url_path` row that holds that blob, under any scheme or host (`anchor_rows_deleted` counts them; 0 for every other reason), and the audit removes the corrupt pool file itself (before the batch commits), so the next client request fetches a fresh copy from upstream instead of being served bytes apt rejects — also when upstream still serves the same `InRelease`, since the miss path keeps an existing pool file of the same hash (a failed removal: `legacy_anchor_discard_failed`). `anchor_oversized` (an anchor over the audit's 8 MiB read, not proven bad) keeps its rows, as does `anchor_missing` (no bytes to serve: the next request fetches anyway). A pool file that cannot be read is no evidence: the snapshot is deferred (`legacy_trust_audit_deferred`). |
+| `legacy_anchor_discard_failed` | ERROR | `snapshot_id`, `canonical_host`, `suite_path`, `blob_hash`, `err` | The startup trust audit could not remove the pool file of an `anchor_corrupt` snapshot; the quarantine went ahead and its `url_path` rows were deleted, so a client request refetches the `InRelease` — but if upstream still serves the same file, the fetch keeps the rotted pool file of that hash and serves it. Delete `pool/<first two characters of blob_hash>/<blob_hash>` under the cache directory once the cause (permissions, a read-only mount) is fixed; an eager re-adoption of the suite, with adoption enabled once a client asks, also evicts it (`pool_corruption_during_adoption`). |
 | `legacy_trust_audit_deferred` | ERROR | `snapshot_id`, `canonical_host`, `suite_path`, `err`, `stack` (present only for a recovered panic) | A pre-upgrade snapshot's pool file could not be read (not "missing"), or auditing it panicked (`err` carries the panic value); the snapshot is left as it was and retried at the next start. |
 | `legacy_trust_audit_progress` | INFO | `audited`, `quarantined`, `deferred` | One batch of up to 100 queued snapshots was audited and committed; the fields are running totals across this start's batches. Logged on the first start after an upgrade, and on later starts while deferred snapshots remain queued or after an interrupted audit. |
 | `legacy_trust_audit_completed` | INFO | `audited`, `signed`, `quarantined`, `deferred`, `anchor_markers`, `duration` | The startup trust audit finished (every start; `audited` is 0 once the upgrade queue has drained). |
 | `adoption_flip_barrier` | INFO | `canonical_host`, `suite_path`, `snapshot_id`, `outcome` (`held`, `quiet_cut_short`, `hold_expired`, `hold_skipped`, `not_needed`, `commit_failed`, `refused`), `phase_a_seconds`, `token_wait_seconds`, `hold_seconds`, `held_requests` (the last four absent when the barrier was skipped for a first adoption) | An adoption committed behind the spec C8 flip barrier: `held` within its hold, once its suite was quiet; `quiet_cut_short` within its hold at 15 s, before its suite was quiet — the held requests got the new snapshot, a client still reading the old anchor then (or done with it within `flip_quiet_period`) may straddle (`acu_flip_quiet_cut_short_total`); `hold_expired` when the hold reached 20 s before the commit finished — the held requests were answered from the old snapshot (`acu_flip_hold_expired_total`); `hold_skipped` after 60 s without the hold token (`acu_flip_hold_skipped_total`); `not_needed` for a first adoption whose anchor clients were already being served; `commit_failed` when the commit ran and failed — the snapshot did not switch, the timing fields still describe the barrier, and `adoption_run_failed` gives the error; `refused` when the commit refused an unsigned candidate because the suite has signed history (spec C5's recheck inside the commit) — the snapshot did not switch, and `adoption_refused` gives the reason, `unsigned_downgrade_refused`. Not logged when `adoption.flip_quiet_period = 0s`, nor for a flip abandoned at shutdown (`adoption_abandoned_at_shutdown` says so). |
-| `adoption_served_anchor_read_failed` | WARN | `canonical_host`, `suite_path`, `path`, `err` | Before a first adoption's commit, the adopter could not read the anchor the cache serves for the suite (when clients already get the anchor being adopted, the barrier is not needed); the barrier runs — the safe direction. A missing row is not logged. |
+| `adoption_served_anchor_read_failed` | WARN; DEBUG when a shutdown cut the read short | `canonical_host`, `suite_path`, `path`, `err` | Before a first adoption's commit, the adopter could not read the anchor the cache serves for the suite (when clients already get the anchor being adopted, the barrier is not needed); the barrier runs — the safe direction. A missing row is not logged. |
 | `adoption_abandoned_at_shutdown` | INFO | `canonical_host`, `suite_path`, `form`, `err` | An adoption was abandoned at shutdown — waiting in the flip barrier (or about to skip it) when the daemon closed the barrier at the signal, or in any step before its commit once the daemon's lifetime context ended: nothing was committed or recorded, the change is adopted after the restart. Not a failure: no `adoption_run_failed`, not counted in `acu_adoption_total` or `acu_adoption_duration_seconds`, not shown in the admin page's recent adoptions. |
 | `displaced_snapshot_lookup_failed` | WARN | `err`, `canonical_host`, `path` | Retained by-hash could not read the suite's displaced snapshots; the request is answered 404, as before spec C8. |
 | `snapshot_adopted_at_lookup_failed` | WARN | `err`, `snapshot_id` | A snapshot `InRelease`/`Release`/`Release.gpg` could not read its snapshot's `adopted_at` (spec C8 `Last-Modified`), so this response carries the pool file's mtime instead and its `Last-Modified` may not grow with the last flip. Not logged when the request had already ended. |
@@ -365,13 +374,17 @@
 | `suite_evicted` | INFO | `canonical_host`, `suite_path` (the evicted suite), `for_host`, `for_suite` | At or above the global cap, a suite with no adopted snapshot that had not been requested for 24 hours was evicted for a new one; its signed-seen records survive, moved up to the time of the eviction (as for `suite_reaped`). Above the cap (after `freshness.max_suites` was lowered, or after an upgrade) the new suite is still refused (`suite_seed_rejected`) until the table is back under the cap. |
 | `suite_caps_exceeded` | WARN | `suites`, `max_suites`, `hosts_at_cap`, `per_host_cap`, `fullest_host`, `fullest_host_suites` | At start, the suite table holds more than `freshness.max_suites` suites (a lowered `max_suites`, or an upgraded cache), or a host holds a tenth of it or more: new suites — for a host at its cap, that host's — are refused. Above the total cap each refused attempt still evicts one suite idle for 24 hours, so the table shrinks back; a host at its cap evicts nothing for its own new suites; its suites go when the suite reaper removes those not requested for `gc.suite_idle_ttl`, or when total-cap eviction for any host's new suite picks one of its idle suites. Not logged for a table at exactly the total cap: that is normal operation for a full cache, where a new suite evicts an idle one (no adopted snapshot, not requested for 24 hours) and is refused only when none qualifies (`suite_seed_rejected`). Logged once per start. |
 | `suite_cap_usage_read_failed` | WARN | `err` | The startup check behind `suite_caps_exceeded` could not count the suites; the caps apply either way. |
-| `suite_reaped` | INFO | `canonical_scheme`, `canonical_host`, `suite_path`, `reason` (`idle`, `non_canonical`, `junk`), `metadata_rows`, `settled` (present, `true`, only for a settled reap) | The suite reaper (spec C9; hourly, from an hour after the start; a run tries at most 250 `non_canonical` and `junk` suites, first, and at most 25 `idle` ones) deleted a suite: its metadata `url_path` rows (`metadata_rows`, in batches of 1,000, the anchors last), then, in one transaction that re-checks that the suite was not adopted — nor, for `idle`, asked for — since the run listed it, its snapshots (retired: snapshot GC deletes them) and its row. `idle`: no client asked for it in `gc.suite_idle_ttl`; `non_canonical`: a row an earlier release created for a path this version no longer tracks (`metadata_rows` is 0: such a path's cached files are left to `gc.url_path_ttl`); `junk`: a suite without a snapshot whose cached anchor files (`InRelease`, `Release`, `Release.gpg`) all fail their content check — an HTML page, say; a plain `Release` cached as `InRelease` passes — each fetched at least a day ago, whatever its adoption record says — for a flat suite the cache has never seen signed, its cached root `Packages*` and `Sources*` files are judged with its anchor files, and it is junk only when it has at least one such file and each fails its check (an HTML page cached as `Packages.gz`, say); a flat suite seen signed is judged by its anchor files alone. Its signed-seen records survive, moved up to the time of the reap (a later stamp is kept): every suite-row delete — a reap, a cap eviction, a forget that leaves the records — does so, so a record's year (`signed_seen_markers_swept`) counts from the end of its path's tracking; and a suite with an adopted signed snapshot (`inline` or `detached`, not retired) gets its path's record under its own scheme before its snapshots are retired — created if missing, as for a snapshot the upgrade's trust audit could not read yet — so that history survives too. `settled=true`: the final transaction committed but the writer call reported an error, and a fresh read found the row gone — it is a reap all the same. Counted in `acu_suites_reaped_total{reason}`, settled reaps included. |
-| `admin_forget_requested` | INFO | `scheme`, `canonical_host`, `suite_path` | `POST /forget` accepted a request (logged by `internal/admin`): it passed the guard (`X-ACU-Admin: 1`, `Sec-Fetch-Site` and, without `htpasswd_file`, the `Host` check), the 8 KiB body limit and the parameter checks, and the forget starts. A request refused with 400, 403 or 413 is not logged. `scheme` is the suite's canonical scheme (the other lines about a suite name it `canonical_scheme`). The outcome follows: `suite_forgotten`, `admin_forget_failed`, no line for a busy or unknown suite (409), or, after a panic while the suite is held, `freshness_check_panicked` with `where=forget` (a panic before the hold leaves only net/http's ERROR line). |
+| `suite_reaped` | INFO | `canonical_scheme`, `canonical_host`, `suite_path`, `reason` (`idle`, `non_canonical`, `junk`), `metadata_rows`, `last_requested_unixtime` (`idle` only, when the suite has one: the client-interest stamp the cutoff judged), `evidence` (`junk` only: the paths of the cached files judged, a list in the order judged — a JSON array, `[a b]` in the text format), `settled` (present, `true`, only for a settled reap) | The suite reaper (spec C9; hourly, from an hour after the start; a run tries at most 250 `non_canonical` and `junk` suites, first, and at most 25 `idle` ones) deleted a suite: its metadata `url_path` rows (`metadata_rows`, in batches of 1,000, the anchors last), then, in one transaction that re-checks that the suite was not adopted — nor, for `idle`, asked for — since the run listed it, its snapshots (retired: snapshot GC deletes them) and its row. `idle`: no client asked for it in `gc.suite_idle_ttl`; `non_canonical`: a row an earlier release created for a path this version no longer tracks (`metadata_rows` is 0: such a path's cached files are left to `gc.url_path_ttl`); `junk`: a suite without a snapshot whose cached anchor files (`InRelease`, `Release`, `Release.gpg`) all fail their content check — an HTML page, say; a plain `Release` cached as `InRelease` passes — each fetched at least a day ago, whatever its adoption record says — for a flat suite the cache has never seen signed, its cached root `Packages*` and `Sources*` files are judged with its anchor files, and it is junk only when it has at least one such file and each fails its check (an HTML page cached as `Packages.gz`, say); a flat suite seen signed is judged by its anchor files alone. Its signed-seen records survive, moved up to the time of the reap (a later stamp is kept): every suite-row delete — a reap, a cap eviction, a forget that leaves the records — does so, so a record's year (`signed_seen_markers_swept`) counts from the end of its path's tracking; and a suite with an adopted signed snapshot (`inline` or `detached`, not retired) gets its path's record under its own scheme before its snapshots are retired — created if missing, as for a snapshot the upgrade's trust audit could not read yet — so that history survives too. `settled=true`: the final transaction committed but the writer call reported an error, and a fresh read found the row gone — it is a reap all the same. Counted in `acu_suites_reaped_total{reason}`, settled reaps included. |
+| `admin_forget_requested` | INFO | `canonical_scheme`, `canonical_host`, `suite_path` | `POST /forget` accepted a request (logged by `internal/admin`): it passed the guard (`X-ACU-Admin: 1`, `Sec-Fetch-Site` and, without `htpasswd_file`, the `Host` check), the 8 KiB body limit and the parameter checks, and the forget starts. A request refused with 400, 403 or 413 is not logged. The outcome follows: `suite_forgotten`, `admin_forget_failed`, `suite_not_forgotten` (DEBUG) for a busy or unknown suite (409), or, after a panic while the suite is held, `freshness_check_panicked` with `where=forget` (a panic before the hold leaves only net/http's ERROR line). |
 | `suite_forgotten` | INFO | `canonical_scheme`, `canonical_host`, `suite_path`, `metadata_rows`, `markers_deleted`, `anchor_rows`; `settled` (present, `true`, only for a settled forget, whose line carries `metadata_rows` but not `markers_deleted` or `anchor_rows`) | `POST /forget` deleted a suite as the reaper would, regardless of idleness, and — only when no other suite of the repository path, under either scheme or as its `/.` twin, has a row — `markers_deleted` signed-seen records of the path and `anchor_rows` cached `InRelease`, `Release` and `Release.gpg` of the path and its twin under either scheme. Its request is logged first as `admin_forget_requested`. `settled=true`: the final transaction committed but the writer call reported an error, and a fresh read found the row gone — a forget all the same (the records and cached anchors deleted as above when no other suite of the path has a row; how many is not known); the request is then answered 500 with WARN `admin_forget_failed`, or 503 with that line at DEBUG when the admin listener's shutdown cut it short. |
-| `admin_forget_failed` | WARN | `err`, `scheme`, `canonical_host`, `suite_path` | `POST /forget` failed on a database error, or its work took longer than 5 minutes, and answered 500. Metadata batches deleted before the error stay deleted (the anchors go last); the request can be repeated (409 then means the suite is gone after all, or busy). A forget whose final delete committed before the error is logged `suite_forgotten` with `settled=true` just before this line when a fresh read then finds the row gone (a read that fails, or lands before a commit still in flight, finds it present: then no such line). Logged at DEBUG instead, and answered 503, when the admin listener began shutting down while the forget ran: shutdown cuts it short. |
+| `suite_not_forgotten` | DEBUG | `canonical_scheme`, `canonical_host`, `suite_path`, `reason` (`no_suite`: the suite has no row, before the hold or under it; `busy`: the hold was refused — a check, an adoption, a repair or reconcile, or another deleter holds the suite; `changed`: the final delete found the suite's row changed since it was read under the hold, which no writer should do) | `POST /forget` deleted nothing and answered 409, whose body tells neither case from the other (spec C9); this line says which. |
+| `admin_forget_failed` | WARN | `err`, `canonical_scheme`, `canonical_host`, `suite_path` | `POST /forget` failed on a database error, or its work took longer than 5 minutes, and answered 500. Metadata batches deleted before the error stay deleted (the anchors go last); the request can be repeated (409 then means the suite is gone after all, or busy). A forget whose final delete committed before the error is logged `suite_forgotten` with `settled=true` just before this line when a fresh read then finds the row gone (a read that fails, or lands before a commit still in flight, finds it present: then no such line). Logged at DEBUG instead, and answered 503, when the admin listener began shutting down while the forget ran: shutdown cuts it short. |
+| `admin_reconcile_requested` | INFO | `scheme`, `canonical_scheme` (the same value: `scheme` is kept for filters written before `canonical_scheme`), `canonical_host`, `suite_path` | `POST /reconcile` accepted a request (logged by `internal/admin`): it passed the guard (as `POST /forget`'s), the 8 KiB body limit and the parameter checks, and the reconcile is asked for. It is answered 202 when it starts (in the background; a failure logs `adoption_reconcile_failed` with `stage=on_demand`) and 409 when it does not — the suite busy, unknown or without a current snapshot, or no adoption slot free. A request refused with 400, 403, 413 or 501 is not logged. |
 | `signed_seen_markers_swept` | INFO | `count` | The suite reaper's daily sweep (the first a day after the start, then a day apart) deleted `count` signed-seen records whose repository path has no suite row (for it or its `/.` twin, under either scheme) and that were last seen — or a suite of their path last deleted: reaped, evicted at the cap, or forgotten while another suite of the path kept the record — more than 365 days ago, by the clock less the forward steps the reaper detected while running (`clock_step_detected` with `stamp=wall_clock`), so such a step never ages a record. Not logged when it deleted none. |
+| `suite_reaper_run_complete` | INFO | `tried`, `reaped`, `markers_swept`, `deferred`, `duration_ms` | A suite reaper run completed (hourly, from an hour after the start): `tried` suites held and judged within the run's budgets, `reaped` of them deleted (each with its `suite_reaped` line), `markers_swept` signed-seen records the daily sweep deleted (0 when no sweep was due), `deferred` `true` when a detected forward step of the wall clock held the run back — no reap, no sweep (see `clock_step_detected` with `stamp=wall_clock`). It sets `acu_suite_reaper_last_run_unixtime`. A run whose listing failed (`suite_reaper_list_suites_failed`) or that a shutdown cut short logs neither, so an alert on the gauge's age catches a reaper that stopped completing runs. Logged with `gc.suite_idle_ttl = 0s` too (nothing is reaped then; the daily sweep still runs). |
 | `suite_reaper_list_suites_failed`, `suite_reaper_read_suite_row_failed`, `suite_reaper_delete_metadata_failed`, `suite_reaper_reap_failed`, `suite_reaper_list_unadopted_anchors_failed`, `suite_reaper_list_unadopted_flat_indexes_failed`, `suite_reaper_junk_anchor_reads_failed`, `suite_reaper_read_signed_evidence_failed`, `suite_reaper_marker_sweep_failed` | WARN | `err`; `canonical_scheme`, `canonical_host` and `suite_path` for a suite's step; `metadata_rows` (for `suite_reaper_delete_metadata_failed`: rows deleted before the error); `count`, `canonical_scheme`, `canonical_host` and `path` for the junk anchor reads (how many anchor or flat index reads failed in the run's junk scan — a failed read is not remembered, so one unreadable pool file that N suites share counts once for each of them whose scan reaches it, up to N times — and the first: its suite's scheme and host and the file's path); `count`, `canonical_scheme`, `canonical_host` and `suite_path` for the signed-evidence reads (how many flat suites' reads failed in the run's junk scan, and the first); `swept` (records deleted before the error) | A step of a suite reaper run failed. A failed listing ends the run: nothing is reaped and nothing swept (a due record sweep runs at the next run) — except the flat indexes' (read only in a run that holds a flat suite without a snapshot), which leaves every flat suite without a snapshot unjudged as junk for the run; a failed read of whether a flat suite was seen signed (read only for one its anchor files and its index files judge differently) leaves that suite unjudged for the run; any other failed step leaves the rest of the run to go on: a suite whose step failed — a database error, or its work cut short after 5 minutes — keeps its row and is retried at the next run (the metadata batches deleted before the error stay deleted, the anchors last); a pool file that exists but does not read keeps its suite (a missing one is not logged); the junk scan runs again at the next run, the record sweep the next day. A step that fails because the daemon is stopping is logged at DEBUG instead. |
 | `suite_reaper_suite_busy` | DEBUG | `canonical_scheme`, `canonical_host`, `suite_path` | A suite the reaper would reap was held — by a check, an adoption, a repair or reconcile, or another deleter (cap eviction, `POST /forget`) — or deleted meanwhile; it is tried again at the next run if it still qualifies, and counts toward the run's budget for its kind (25 `idle` suites; 250 `non_canonical` and `junk` ones). |
+| `gc_panicked` | ERROR | `where` (`tick`: a periodic GC tick; `startup`: the startup pass; `pool_scan_worker`: one file of the startup pool scan; `pool_scan_producer`: the pool scan's directory walk), `panic`, `stack` (first occurrence per `where` per process only) | A panic in garbage collection was recovered (logged by `internal/gc`; a bug, please report it). A tick's panic ends that tick — its `gc_run_complete` line and last-run summary are not written, so `acu_gc_last_run_unixtime` ages — moves the pass rotation past the pass that panicked, so the passes after it still run, and the next tick comes as scheduled; the startup pass's ends the startup pass, which main logs (`gc startup pass failed`) before it serves; a worker's ends that file's check, which the next start's scan repeats, and the worker goes on to the next file; the walk's ends the walk, and the scan finishes the files already queued. |
 | `handler_goroutine_panicked` | ERROR | `where` (`touch`, `seed_rejections`), `canonical_scheme`, `canonical_host` and `path` (`touch` only), `panic`, `stack` (first occurrence per `where` per process only) | A panic in one of the handler's background goroutines — the touch goroutine, or the cap-rejection cache's sweep timer — was recovered (a bug, please report it). |
 | net/http's own lines: `http: panic serving …`, `http: TLS handshake error from …` and the like | ERROR on the admin listener; WARN on the proxy listeners | none: `msg` is net/http's text — for a panic, `http: panic serving <client address>: <panic>` followed by the stack | The Go HTTP server's own diagnostics, logged through the daemon's structured log at the level each listener sets (left to itself, net/http would write them through Go's standard log package, which the daemon also sends to its log, but at INFO). On the admin listener every such line is ERROR, above all a handler panic net/http recovered: after a `POST /forget` panic, which is logged first as `freshness_check_panicked` with `where=forget`, the request ends without an answer and gets no `admin_request` line. On the proxy listeners (plain and TLS) they are WARN, a TLS handshake error included. |
 | `suite_onboarding_failed` | WARN | `err`, `canonical_host`, `suite_path` | A cache hit on an untracked suite's anchor could not create its row (including the 5 s bound it shares with the backstop that may follow, and a shutdown that cut it short); the anchor is served and the next hit retries. |
@@ -380,8 +393,11 @@
 | `suite_cached_anchor_read_failed` | WARN | `err`, `canonical_host`, `suite_path`, `path` | Creating a suite's row, the handler could not read one of the suite's cached signed anchors (its `url_path` row or its pool file; an absent row is not logged). The row is created anyway, and the anchor is read again and marked right after the write, before the response — also when the client left, or the request is answered from a new snapshot, while it waited for a flip (`suite_signed_seen_backstop_read_failed` if that fails too). Between the write and that mark the suite is tracked with the anchor unmarked; if the mark fails, it stays so until the startup pass at the next start. |
 | `suite_signed_seen_backstop_read_failed` | WARN | `err`, `canonical_host`, `suite_path`, `path` | After creating a suite's row, the handler's second read of a cached signed anchor (see `suite_cached_anchor_read_failed`; also run when the anchor changed during the creation) failed — its `url_path` row (within the backstop's 5 s bound, which covers its database steps; after onboarding the bound is shared with the creation, and on a miss it ends at most 25 s after the request arrived) or its pool file (a local read of a few KiB, not bounded); an absent row is not logged. The response is served; the anchor stays unmarked until the startup pass marks it at the next start. |
 | `snapshot_member_blob_missing` | WARN | `canonical_host`, `path`, `snapshot_id`, `blob_hash`, `snapshot_displaced` (present for a displaced snapshot's member) | A snapshot member's pool file is gone: a current member is re-fetched and checked against its declared hash; a displaced one (retained by-hash) answers 404. |
+| `snapshot_index_target_404` | WARN | `canonical_host`, `suite_path`, `path`, `architecture`, `snapshot_id` | A client asked an adopted suite for a `Packages` or `Sources` index its current snapshot does not hold (adoption skipped it — `adoption_member_skipped` with `4xx_index_target` — or its `Release` does not list it), and was answered 404 from the snapshot: `apt update` fails for it until a snapshot holds the file. `architecture` is the index's: from its `binary-<arch>/` directory, `source` for a `Sources` index — and for a flat suite, whose root `Packages` have no architecture directory, `all` for a `Packages*` and `source` for a `Sources*`. Counted in `acu_serve_snapshot_index_target_404_total{architecture}`, whose label buckets anything but a known architecture into `other` (the log line keeps the raw path). |
 | `pool_corruption_during_adoption` | WARN | a member that declares a SHA256: `canonical_host`, `suite_path`, `path`, `declared_sha256`, `actual_sha256`; a member the Release declares by SHA512 only: `canonical_host`, `suite_path`, `path`, `declared_sha512`, `derived_sha256` (the SHA-256 of the fetched bytes, verified against `declared_sha512`), `actual_sha256`; an anchor or index the adoption stores from memory: `hash`, `actual_sha256`, `context` (`writeBlobBytes`) | An adoption was about to reuse a pool file whose bytes no longer hash to its name (corrupted at rest). The file is evicted and replaced by freshly verified bytes; it is never bound to the snapshot, and the `.deb` hashes it declares never vouch. A SHA512-only member is fetched at every adoption, and an existing pool file under its derived SHA-256 is re-hashed before it is kept. Counted by `acu_pool_corruption_during_adoption_total`. |
 | `package_coverage_incomplete` | INFO | `canonical_host`, `suite_path`, `snapshot_id`, `reason`; `directories` (`unsupported_variants`), `no_sha256_stanzas` (`no_sha256`), `skipped_rows` (`all_binary_stanzas_malformed`) | A signed snapshot's package coverage could not be proven complete (SPEC3 §7.5.4), so strict mode (`integrity.refuse_unvouched_debs`) stays off for its host. `reason`: `non_dists_layout` (the suite path is neither `dists/`-shaped nor a flat repository's: no path the cache tracks since flat repositories are adopted), `no_packages_members` (its Release lists no `Packages` index; for a flat suite, no root one), `unsupported_variants` (a directory with a `Packages` index has no variant that was fetched and parsed; `directories` names them), `no_sha256` (spec C5: a `Packages` stanza with a `Filename` but no `SHA256`, for example one that declares only `SHA512` or `MD5sum`; it vouches nothing, and `no_sha256_stanzas` counts the distinct paths), `all_binary_stanzas_malformed` (every binary stanza lacks `Package`, `Architecture` or `Version`). A reconcile that heals `Packages` indexes recomputes the rows for them and may log it too; a reconcile never changes a snapshot's coverage. |
+| `source_paths_skipped` | INFO | `canonical_scheme`, `canonical_host`, `suite_path`, `snapshot_id`, `member_path` (the `Sources` file), `count`, `first` | A `Sources` index named source files whose path the cache does not accept — a `Directory`, or a file in it, with a `%`, `?` or `#`, a `.`, `..` or empty segment, or a leading `/` (a file name of `.` or `..`, or with a `/`, a backslash or a NUL byte, or a malformed hash, is dropped as malformed and not counted) — so those files get no hash row in the snapshot (spec C5: `Sources` paths keep the member-path rule): `count` paths, `first` the first of them. The adoption goes on; such a file is served as any unvouched file is. One line per `Sources` file that skipped any, at every adoption: INFO, since the operator cannot act on it. |
+| `adoption_repair_declarations_unavailable` | DEBUG | `canonical_host`, `suite_path`, `snapshot_id`, `err` | A repair of skipped members could not re-read the declarations of the snapshot's `Release` (its stored anchor), so it checks each member against its skip record's SHA-256 alone and writes no SHA512 alias (spec C5); a reconcile reports an unreadable anchor at WARN (`adoption_reconcile_failed`). |
 | `package_hash_conflict` | ERROR | `canonical_host`, `path`, `declared` (one entry per declaring row — `declared_sha256`, `snapshot_id`, `suite_path`: a flat suite and its `./` twin, or two channels a suffix fallback ties, appear by name); `row_blob_hash` on a cache hit; `fetched` (`false` before a download, `true` after it, then with `observed_sha256`) on a miss | Two or more current signed snapshots of the host declare different SHA-256s for the requested path (spec C6 step 3), so the request is refused — 502 with `Retry-After: 60`, request outcome `package_hash_conflict` — rather than answered with one of them. A flat suite's twins disagree this way, under both, between their adoptions of a `Packages` whose `.deb` upstream rewrote in place (see *Flat repositories* in configuration.md). |
 | `package_hash lookup failed` | WARN | `err`, `canonical_host`, `path` | On a `.deb` cache hit the vouching hashes (`package_hash`) could not be read: the cached file is served without the hash check, and strict mode (`integrity.refuse_unvouched_debs`) fails open for it, so a database fault does not refuse every package. A request whose client left is answered 503 (`client_canceled`) instead, never served, and logs nothing here. |
 | `strict-mode coverage lookup failed` | WARN | `err`, `canonical_host` | Strict mode could not read its host's current snapshots and their coverage: it fails open for the request (the `.deb` is served or fetched as if strict mode were off). A request whose client left is answered 503 (`client_canceled`) on the hit path and logs nothing here. |
````

- [ ] **Step 4: `docs/admin-ui-spec.md`**

§4.5 gains `anchorless` (PG-14, the G carry-forward; additive; its one-check exception, review M-11), the *Last requested* column under `last_requested_unixtime` (F-14), and the Adoption column's two new texts (`refreshed without a Release`; B-63's `checks failing ×k · backoff ends in …` / `backoff over`, the count alone for a dormant or record-paced suite: design note 6, reviews M-3 and sweep M-5); new §4.6: `gc.retired_reaped` (the F carry-forward), `recent_adoptions[].scheme` (F-19), and the duration sentence (C-36); §5.9's example and the per-section copy note (D-50: the page now renders the spec's copy).

````diff
diff --git a/docs/admin-ui-spec.md b/docs/admin-ui-spec.md
--- a/docs/admin-ui-spec.md
+++ b/docs/admin-ui-spec.md
@@ -350,8 +350,9 @@
 | `scheme` | string | The suite's canonical scheme, `http` or `https` (phase F, ruling PF-7): a repository can be tracked under both, as two rows with the same `host` and `suite_path`, and `POST /forget` (`docs/configuration.md`, *Forgetting a suite*) takes it, defaulting to `https`. The Suites table's Host column shows it as `scheme://host`. |
 | `flat` | boolean | `true` for a flat repository's suite — a canonical suite path with no `dists` segment, its `./` twin included (spec C2) — and `false` for a `dists/` suite (phase G). The Suites table's Suite path cell shows a neutral `flat` badge after the path. The tag is not selectable (`user-select: none`), so copying the Suite path cell — as `/forget`'s 409 asks — copies the path alone. |
 | `anchor_form` | string | The current snapshot's anchor form — `inline`, `detached` or `unsigned` (a `Release` no signature covers: it vouches no `.deb`); empty when the suite has no current snapshot. |
-| `last_requested_unixtime` | int64 or `null` | When a client last showed interest in the suite (spec C3): written when the suite's row is created, then by coalesced touches — at most one write per suite per hour — from serves of its anchors (`InRelease`, `Release`, `Release.gpg`; status below 400, 304 and HEAD included), of its flat indexes, and of `.deb`s its current snapshot vouched for. A `dists/` suite's index requests (`main/binary-amd64/Packages.gz`) never move it. It decides cap eviction: at the `freshness.max_suites` cap, a suite with no current snapshot whose stamp is 24 hours old or more can be evicted for a new one (`suite_evicted`). |
-| `check_fail_count` | int64 | Consecutive failed checks since the last answered check: ended unknown (transport error, deadline, unexpected status), or found nothing they may adopt (a suite without a snapshot whose upstream has neither `InRelease` nor `Release`, which also records `anchor_absent`; a signature the check found but could not record as seen), or — a flat repository without a `Release` (phase G) — refreshed its cached index files with no content answer (an HTML page for each, 401, 403). An unsigned `Release` while `adoption.adopt_unsigned` is off is the refusal `unsigned_disabled`, not a failed check, and so is a flat repository without a `Release` while the knob is off. Drives the scheduler's healthy → never-checked → failing ordering and its 2 min → 1 h backoff. Resets to 0 on every answered check, an absent current anchor (`anchor_absent`) included. |
+| `anchorless` | boolean | `true` for a flat repository without a snapshot whose last answered check refreshed its cached index files without a `Release` (spec C4 Dispatch 4; phase H1) — with adoption enabled, no current snapshot, a last success, no failure or refusal record and no pending change, the marks every other answered check of such a suite leaves; `false` otherwise, and always while adoption is disabled. One exception lasts one check: a flat suite whose row was created while adoption was disabled has a last success already, so it reads `true` from the moment adoption is enabled until its first check. It tells such a repository from a dormant, never-adopted suite, which also has no snapshot and no record. The Adoption cell reads `refreshed without a Release`. |
+| `last_requested_unixtime` | int64 or `null` | When a client last showed interest in the suite (spec C3): written when the suite's row is created, then by coalesced touches — at most one write per suite per hour — from serves of its anchors (`InRelease`, `Release`, `Release.gpg`; status below 400, 304 and HEAD included), of its flat indexes, and of `.deb`s its current snapshot vouched for. A `dists/` suite's index requests (`main/binary-amd64/Packages.gz`) never move it. It decides cap eviction: at the `freshness.max_suites` cap, a suite with no current snapshot whose stamp is 24 hours old or more can be evicted for a new one (`suite_evicted`), and reaping: a suite no client asks for in `gc.suite_idle_ttl` is reaped (`suite_reaped`). The Suites table's *Last requested* column shows it (phase H1). |
+| `check_fail_count` | int64 | Consecutive failed checks since the last answered check: ended unknown (transport error, deadline, unexpected status), or found nothing they may adopt (a suite without a snapshot whose upstream has neither `InRelease` nor `Release`, which also records `anchor_absent`; a signature the check found but could not record as seen), or — a flat repository without a `Release` (phase G) — refreshed its cached index files with no content answer (an HTML page for each, 401, 403). An unsigned `Release` while `adoption.adopt_unsigned` is off is the refusal `unsigned_disabled`, not a failed check, and so is a flat repository without a `Release` while the knob is off. Drives the scheduler's healthy → never-checked → failing ordering and its 2 min → 1 h backoff. Resets to 0 on every answered check, an absent current anchor (`anchor_absent`) included. While above 0 the Adoption cell says so (below). |
 | `adoption_fail_reason` | string or `null` | The current failure or refusal reason; `null` when no record exists. |
 | `adoption_fail_count` | int64 | Consecutive adoption attempts in the current failure/refusal streak; `0` when no record exists. |
 | `adoption_fail_first_unixtime` | int64 or `null` | When the current streak started; for `unsigned_unconfirmed` and `anchor_absent`, when the current bytes (or absence) were first observed — it restarts on new content while `adoption_fail_count` keeps growing. |
@@ -362,7 +363,7 @@
 `keyring[]` was: no existing key changes shape or meaning (`scheme` too:
 the HTML Host cell gains the `scheme://` prefix, the JSON `host` is
 unchanged; `flat` too: the HTML Suite path cell gains its badge, the JSON
-`suite_path` is unchanged).
+`suite_path` is unchanged; `anchorless` too).
 
 **Adoption column.** The Suites table's "Adoption" column renders the
 record above as one line, via the server-side `adoptionStatus` helper:
@@ -370,9 +371,23 @@
 `"refused: upstream_older · retry due"` for a refusal, and
 `"refused: upstream_older · since 1d 1h · retry due"` for a refusal whose
 streak began more than 24 h ago. Empty when the suite has no failure or
-refusal record. While adoption is disabled, the text gains the suffix
+refusal record — or `"refreshed without a Release"` for an `anchorless`
+suite (phase H1). While adoption is disabled, the text gains the suffix
 ` (adoption disabled)`: the record was stored while adoption was enabled
-and is inert now.
+and is inert now. A suite whose checks keep failing (`check_fail_count`
+above 0) adds `"checks failing ×3 · backoff ends in 12m"` — after the
+record's text and ` · ` when there is one: the count, and when the
+scheduler's backoff (2 min doubling to 1 h, from `last_check_unixtime`)
+ends, or `"backoff over"` once it has (phase H1). That is the earliest
+the scheduler may check the suite: a client's request may trigger a
+check sooner, after the `cooldown`, and a suite whose last success is
+recent is checked later. A suite the scheduler does not queue on its
+backoff shows the count alone: with adoption enabled, a never-adopted
+suite no client asked for within 7 days (dormant), and an adopted suite
+its `anchor_absent` or `unsigned_disabled` record paces (the record's
+own text shows its retry). A suite waiting for an adoption slot is not
+shown as such (that queue is the scheduler's, in memory). The JSON carries the parts as
+they are (`check_fail_count`, `last_check_unixtime`), not this text.
 
 **Unsigned and absent-anchor refusals (phase E).** `unsigned_unconfirmed`
 is an unsigned repository waiting for its second observation
@@ -429,6 +444,19 @@
 refreshes metadata, so the lag is truthful but expected. The SUITES vital
 is `stale` (§9.1). The JSON keeps every field as stored.
 
+### 4.6 GC and Recent Adoptions fields (phase H1)
+
+Two more fields, additive to the locked JSON contract as §4.5's are:
+
+| Field | Type | Meaning |
+|---|---|---|
+| `gc.retired_reaped` | int | Retired snapshots the last GC run deleted — those of reaped, forgotten and evicted suites, and those the upgrade's trust audit quarantined (spec C9, sub-job C) — beside `displaced_reaped`; the GC panel shows it as *Retired reaped*. Always 0 after the startup run, which leaves them to the periodic runs. |
+| `recent_adoptions[].scheme` | string | The adopted suite's canonical scheme, `http` or `https`; the Recent Adoptions table's Host cell shows `scheme://host`, as the Suites table does. Empty for an event recorded without one. |
+
+A Recent Adoptions row's duration is the attempt's wall-clock time,
+and includes the flip barrier's wait (spec C8: at most 140 s) for an
+adoption that commits behind it, as `acu_adoption_duration_seconds` does.
+
 ---
 
 ## 5. Component contracts
@@ -826,15 +854,16 @@
 ```html
 <div class="empty empty--{stale|crit}" role="status">
   <span class="empty__eyebrow">NO SUITES TRACKED YET</span>
-  <p class="empty__body">Adoption begins when a client requests an
-  InRelease through the proxy.</p>
+  <p class="empty__body">A suite is tracked once a client requests its
+  InRelease through the proxy (…).</p>
 </div>
 ```
 
 `.empty` carries the 4px left stripe when state is non-stale (the
 keyring critical-empty case is the principal example).
 
-Per-section copy:
+Per-section copy (the page renders these words; its Suites body was a
+different sentence until phase H1):
 
 | Section          | Eyebrow                  | Body                                                                                                  |
 |------------------|--------------------------|-------------------------------------------------------------------------------------------------------|
````

- [ ] **Step 5: `packaging/config/config.toml.default` (C-37)**

Comments only: `max_duration` never cuts the flip barrier short; `keep_displaced` also bounds retained by-hash.

````diff
diff --git a/packaging/config/config.toml.default b/packaging/config/config.toml.default
--- a/packaging/config/config.toml.default
+++ b/packaging/config/config.toml.default
@@ -104,7 +104,9 @@
 # Longest an adoption, repair or reconcile may run. An abandoned adoption
 # is retried on the normal schedule, a repair or reconcile on the next
 # unchanged check. This deadline never cuts the final commit short; the
-# commit has its own 5-minute bound.
+# commit has its own 5-minute bound. Nor does it cut the flip barrier
+# short (see flip_quiet_period: at most 140 s), which only a shutdown
+# ends early.
 max_duration          = "30m"
 # How long the cache keeps a suite's snapshot after a client last finished
 # receiving its InRelease/Release before switching to a new snapshot, so
@@ -158,7 +160,10 @@
 snapshot_batch_size = 10    # snapshot deletion batch bound; >= 1
 max_tick_duration   = "5m"  # > 0; tick budget checked between batches
 blob_grace          = "5m"  # >= 1s after a blob's refcount reaches zero
-keep_displaced      = 3     # displaced snapshots kept per suite; >= 0
+# Displaced snapshots kept per suite; >= 0. A by-hash request the current
+# snapshot lacks is served from the newest keep_displaced + 1 of them
+# (retained by-hash); 0 turns that off.
+keep_displaced      = 3
 pool_scan_workers   = 4     # startup orphan-scan parallelism; >= 1
 # Must be > 0, with 2 * heartbeat_interval < blob_grace, and also less
 # than max(upstream.total_timeout * upstream.max_retries, 30m).
````

- [ ] **Step 6: The spec, revision 12**

The revision entry; C3's detached-only sentence (D-52); C5's `Sources` paths, logged at INFO (GD-2, review M-8); C7's new fields and texts (B-63's wording: reviews M-3, sweep M-5); C9's reaper signal and fields (F-13, F-17, F-18), sub-job B's audit-queued predicate (R-H1) and GC's recovered panics, the startup pass's and the rotation's included (F-21, reviews I-1, M-1), `/forget`'s 409 line and 400 (F-5, F-15); C10's wait order (E-58); C11's new events, fields and gauge, and the 43 renames (R-H3, R-H6); §6.1's `anchor_corrupt` delete in the quarantine's transaction (R-H2), the pool file's removal before the commit (review C-1) and the kept-row sentence; §9's H row split into H1–H3; §10 item 12 (GD-5, decided for 1.1.0 by ruling R-H5, for the maintainer's confirmation: review sweep M-10).

````diff
diff --git a/docs/superpowers/specs/2026-09-27-flat-repositories-design.md b/docs/superpowers/specs/2026-09-27-flat-repositories-design.md
--- a/docs/superpowers/specs/2026-09-27-flat-repositories-design.md
+++ b/docs/superpowers/specs/2026-09-27-flat-repositories-design.md
@@ -1,6 +1,6 @@
 # Flat repositories, eager adoption, and suite lifecycle — design
 
-- **Date:** 2026-09-27 (revision 11, 2026-10-24: the maintainer's G-1 decision — `POST /reconcile` takes `/forget`'s guard, and without `htpasswd_file` both refuse a `Host` that is not an IP address, `localhost` or `admin.listen`'s host name; the read-only endpoints keep no `Host` check; revision 10, 2026-10-20/21/23: phase G plan rulings — a `Packages` file with no SHA256 stanza parses and adopts with incomplete coverage (`no_sha256`), and a body with no stanza naming a `Filename` still fails its parse (R-G1); every writer of a `url_path` row's `last_fetched_at` stamps it after the pool file is final, so the first adoption's `adopted_at` floor stays above the `max(pool mtime, last_fetched_at)` `Last-Modified` of url_path-served metadata, and blob mtimes are not folded into that floor (R-G3); the anchorless gate is signed evidence — the cached signed-anchor re-mark, then never-downgrade's rule — not every sniff-passing anchor row (phase G deviation 1); a plain `Contents-*` that changed is deleted, not kept, in the anchorless refresh (phase G deviation 2); the anchorless body cap is 64 MiB; a replaced row's `last_fetched_at` is `MAX(now, previous + 1)`; `flat_host_root_unsupported` is a WARN, one line a minute for the process with a `suppressed` count; a Filename made only of `.` segments is refused; `suites[].flat`; after the phase G plan reviews (rulings PG-1–PG-18): with `adopt_unsigned` off, a suite the anchorless gate admits is refused as an unsigned candidate is — the record `unsigned_disabled`, a successful check logged `changed_deferred` with form `anchorless`, paced by its record, no index request and no delete (its probes still run) — so a Release-less flat repository is served as first cached; the gate runs before the knob (PG-1, PG-10, PG-17); a Debian pool artifact (`.deb`, `.udeb`, `.ddeb`, `.dsc`, `.buildinfo`, `.changes`, `.diff.gz`, or a name with `_` and a `.tar.` component) is never metadata, so strict mode vouches and refuses an `icons-foo_1.0_all.deb` like any `.deb` (PG-2, PG-16); the sniff counter is `acu_anchorless_sniff_rejected_total{host}`, renamed from C4's first `anchorless_sniff_rejected` (PG-5); a flat suite whose base has no signed evidence is judged junk on its cached anchor rows and root `Packages*`/`Sources*` rows together — junk only when it has at least one such row and every one is junk evidence — while a flat suite with signed evidence, like every `dists/` suite, is judged by its anchors alone; the signed-evidence read is lazy, per candidate, a read error gives no verdict for the run, and the index rows are read only in a run that has an unadopted flat suite (R-G2 as amended by PG-7, R-G5); at most two anchorless bodies are in memory at once for the process (PG-8); an anchorless check is answered — a success that clears the record and the pending change — when an index request got a content answer (a 304, a 200 stored or matching the cached bytes, a 404 or 410) or when the suite has no index row to ask for, and is a failed check otherwise: a sniff-rejected 200, a changed plain `Contents-*`, a 401 or a 403 is no content answer (design review M-7, PG-12, PG-15); the refresh asks `Packages*`, then `Sources*`, then `Contents-*` (M-3), stores only the changed validators of a 200 with the cached bytes, a plain `Contents-*`'s too (M-1), deletes a row only while it holds the blob the refresh read (M-4), and logs its over-cap WARN at most once a minute for the process with a `suppressed` count (M-6); a replaced row's `last_fetched_at` can lead the clock as `adopted_at` can (M-8); the package-hash lookups join the suite's `suite_freshness` row by its key (M-2); C11's constants name the body cap, the two body slots and the two one-a-minute WARNs (M-10); the residuals of C3, C4 and C6 — constraints across a host's tenants, the twins' conflict window, an upstream that answers 403 for a missing key, an empty plain `Packages` — are documented, with no twin tiebreak (PG-13); after the phase G task reviews (rulings PG-19–PG-24): a flat suite active only through a client-created root index row, with no cached anchor row to model its probes on, probes its `InRelease` and `Release` at sibling URLs of that row's stored `upstream_url`, so a non-default port survives (PG-20); the anchorless refresh deletes a cached `InRelease` or `Release` only while it holds the blob read before the probe tree ran, so a client's fetch during the probe is kept (PG-24); a delete that fails is no answer — its 404s and 410s stop counting, so a check they alone answered fails (PG-24); a read error of the refresh's index rows is a local fault, logged with nothing written and no backoff (PG-24); an unchanged plain `Contents-*` answers the check without a sniff, so a captive portal's page cached as one answers it during the outage — a documented residual (PG-24); `flat` is the admin `suites[]` field, not a new log field (PG-23); `flat_host_root_unsupported` is logged only for a host the allowlist admits; a `Packages` stanza's Filename without a SHA256 is counted for `no_sha256`, never validated, and becomes no row, path or log value; after the phase G final review (rulings PG-28–PG-40): the anchorless refresh also deletes a cached `InRelease` or `Release` whose probe answered 401 or 403 when an index request of the same check got a content answer — S3's and CloudFront's answer for a missing key — while 404 and 410 delete it whatever the indexes answered (PG-28); a 200 over the 64 MiB cap drops its row for the miss path, no content answer, and its WARN names the file, replacing C4's first rule, which kept the row, and the fetcher fails a body whose `Content-Length` exceeds the cap without reading it (PG-29), and that WARN is logged only once the delete commits (PG-39); the suffix fallback stops at a dot-cleaned path longer than 5,121 bytes and skips a split whose base is longer than a suite path or whose name is longer than a Filename, which no row can match (PG-30); it runs only while the cache holds a flat `package_hash` row — a flag primed when the cache opens, whose failed read fails the start, set before any transaction that inserts such a row commits and never cleared while it runs (PG-31, PG-36); a never-adopted flat suite's metadata rows are read once, with its anchor rows, before the host slot and the probe tree, for Dispatch 3 and the refresh, so a read fault is a local fault before any request, and a shutdown during that read is no fault (PG-32, amending PG-24; PG-35); `package_hash_conflict` names each row's suite path (PG-33); the anchorless check line counts the rows it replaced and deleted (`rows_replaced`, `rows_deleted`), a failed check's error leads with the decisive reason and names the other after `; also `, and phase G's new WARNs carry `canonical_scheme` (PG-33, PG-37; the pre-G emitter of `freshness: read anchor rows failed` gains it in phase H: PG-34); the PG-1 clause above says no index request (its probes still run); C5 records that `ParseSources` keeps `validateMemberPath` (PG-21); the limits C2 and C6 call documented, and an adopted unsigned flat suite that stops publishing its `Release`, are documented in *Flat repositories* (PG-33, PG-38); revision 9, 2026-10-20: phase F plan rulings — the reaper and `POST /forget` delete a suite row only through phase D's compare-and-delete, holding the suite through the `SuiteGuard`, so one delete path remains (R-F1); `Gate.Purge` keeps a live entry (R-F2); the reaper also reaps legacy junk rows (R-F3); the orphan-marker sweep reads a future `last_seen_at` as now, and a marker is an orphan only when neither its base nor its `/.` twin has a row under either scheme, the rule `/forget`'s marker deletion follows too (R-F5); after the phase F plan reviews (rulings PF-1–PF-9), task reviews (rulings PF-10, PF-11) and the final review (rulings PF-12–PF-15): the sweep never rewrites a stamp (PF-1, phase F deviation 3); `/forget` deletes the cached anchors of the base and its twin under both schemes with the markers (PF-2); junk is a cached anchor that fails its sniff, an `InRelease` passing the `Release` sniff (a plain-text Release) excepted, whatever the suite's record, fetched at least 24 h ago (PF-3); the junk scan runs every run and the candidates reaped regardless of idleness go first (PF-4); with `adopt_unsigned` off an established unsigned suite keeps its coherent snapshot — R-F4's retirement into the ordinary cache is dropped (PF-5); the `/forget` guard stops a cross-site form or fetch, not DNS rebinding, and Host-header validation joins G-1 (PF-6); `/forget`'s answers say what happened and the status page shows each suite's scheme (PF-7); a forward step of the wall clock defers reaping and the sweep by the uptime floor, a suite's held work is bounded at 5 minutes, a reap stamps its base's markers, reaps are counted by reason (PF-8); every suite-row delete — cap eviction, a reap, a forget — moves the base's markers up to `MAX(last_seen_at, now)` in its transaction, so an orphan's year counts from the end of its path's tracking (PF-10, widening PF-8's reap stamp); a hand edit that adds one field to one log line touches that function, not the whole file, for the deferred-minor rule (PF-11); the marker sweep runs once a day; `0s` disables reaping, not the reaper's other duties; the idle cutoff is strict and a NULL or future stamp is never idle; `/forget` takes an optional `scheme`, runs to completion on a context a client cannot cancel, and answers 400, 403 and 500 besides 202 and 409; sub-job C deletes one retired snapshot per transaction without a grace; a re-adoption's `adopted_at` stays above every snapshot of its suite, retired ones included; the 90-day simulation runs the reaper every 6 h and restarts every fifth day, and a four-day simulation runs the production cadence (phase F deviation 2); after the phase F task reviews: a non-canonical suite key deletes no metadata; the sweep judges a future marker against a fresh clock read while its cutoff stays the run's, and the reaper's two clock-step reports are logged directly, outside the one-line-a-minute limit; a panic under a suite's hold is recovered for that suite alone, and a delete whose writer call failed after its commit is a reap; `/forget` refuses a `host` holding a scheme (400), and a shutdown of the admin listener cuts it short (503); after the phase F final review: before it retires a suite's snapshots, every suite-row delete turns never-downgrade's snapshot clause into a marker — when the suite has an adopted, non-retired inline or detached snapshot it upserts its base's marker under the suite's scheme, the one marker a delete creates (PF-12); a forward step the reaper detects never ages a marker (the sweep's cutoff takes off every step detected since the reaper started), and the first sweep is due a day after the reaper starts, so a start with the clock ahead has a day to be corrected (PF-13); the classes reaped regardless of idleness have their own budget of 250 a run, tried first, beside the 25 idle suites (PF-14); the startup GC pass runs no sub-job C (PF-15); a reaped suite's packages lose the version retention its snapshot gave them; a `/forget` panic is logged with its suite (`where=forget`), and a forget whose writer call failed after its commit is logged (`suite_forgotten`, `settled=true`) when a fresh read finds the row gone; revision 8, 2026-10-19: phase E plan rulings — before an unsigned commit the adopter re-marks the suite's cached signed anchors and refuses when it cannot; a refusal inside the commit is a refusal state with its own sentinel and the barrier outcome `refused`; one check tail, and an unexpected success status is a failed check; absent answers reset `check_fail_count`; `anchor_absent` is recorded at the first absent observation, and a future `adoption_fail_first_at` never confirms an unsigned transition; the probe tree's content-key gate, the confirmation details, the interest and client-created-row rules, a 304 only to a conditional request, SHA512 verification before the pool; strict mode waits for signed adoptions only; the scheduler bound's admission order; after the phase E plan reviews (rulings PE-1–PE-8): a probe that finds a signature marks the suite's base, and the re-mark also reads the `/.` twin's rows; never-downgrade reads the markers of both schemes and also refuses while an adopted, non-retired signed snapshot of the base exists; a deferred never-adopted suite and an adopted `anchor_absent` suite are paced by their record; a never-adopted suite whose tree finds no anchor also records `anchor_absent`, and so does a detached suite whose changed `Release` has no signature, unless a failed lateral attempt's record holds, which keeps its reason and paces the suite the same way; a cross-form request records no validators; the interest window is symmetric; the unsigned content key is domain-separated; the unsigned member cap is applied before a slot is taken; strict mode's wait, and the miss path's backstop, end at most 25 s after the request started; the probe bound counts client checks and lost offers; after the phase E task reviews (rulings PE-9–PE-11): the long-run retry simulation names no day count; a suite whose anchor answered absent behind a failed attempt's record is paced by that record; a detached suite's lateral `InRelease` is held only by its own last failure (an in-memory memo) or by the record of its own content, so the detached form's failures never postpone it and a new `InRelease` is attempted at the first check that sees it; a `Packages` stanza without a SHA256 makes coverage incomplete (`no_sha256`) and counts toward the stanza cap; a hit-path client that left during the strict wait is answered `client_canceled`; revision 7, 2026-10-18: phase D plan rulings — adoption accepts exactly apt's first `InRelease` line, and a marker line with trailing blanks decodes; a binary `Release.gpg` passes its sniff only with a signature version (3–6) where its packet header puts the body; suite paths over 1024 bytes are not tracked; cap eviction holds the evictee's mutex through the checker, tries at most eight candidates, evicts at most one suite per creation attempt at or above the global cap (an attempt above it is still refused) and deletes through the compare-and-delete the reaper reuses, and a startup WARN names a table over its caps; the rejection cache holds at most 4,096 suites; `suite_seed_rejected` is rate-limited to one line a minute; the C3 metric names; the hit-path invariant covers tracked suites, and every row creation marks the suite's cached signed anchors in its own transaction; with no adopter a refresh miss re-stamps its row as before; the T1 on a creating miss lands with phase E; an attempt a shutdown ends before the barrier is abandoned like one in it; revision 6, 2026-10-18: phase C plan rulings — the C8 barrier keeps the adoption's global slot and does not honour `adoption.max_duration` but ends on shutdown (the daemon closes it at the signal), so the admission bound's *A* includes it; a hold that held no request starts no cross-suite quiet period; `flip_quiet_period` is `0s` or `1s` to `15s`; every C8 residual has its own counter; the hold-token residual and the five-suite DoD corrected (four hold, the fifth commits unheld); retained by-hash serves SHA512 aliases from phase C; exact C2 predicate and C3 `Release.gpg` sniff wording; after phase C's final review: once cross-suite quiet holds, a hold commits at 15 s (20 s less a 5 s commit margin) instead of answering its waiters from the old snapshot at 20 s — its late readers are a counted residual — and snapshot anchors carry `adopted_at` alone as `Last-Modified`; revision 5, 2026-10-17: slot-driven admission in C4 — a suite that finds no slot waits and is re-checked when one is released; the admission bound — after the phase B plan review; revision 4, 2026-10-05/11: legacy trust audit, signed-seen ordering, fail-closed inline verification, flip hold token, fair scheduler — after the 2026-10-05 plan review, a follow-up review, and a three-lens review of grounding, alternatives and residual risk; 2026-10-17: PR #8 parity notes in C10, §7 and §9)
+- **Date:** 2026-09-27 (revision 12, 2026-10-24: phase H1, the sweep — snapshot GC sub-job B never deletes, nor ranks, a snapshot still queued for §6.1's audit (R-H1); an `anchor_corrupt` quarantine deletes, in its own transaction, the `url_path` rows that hold the corrupt `InRelease` blob (`anchor_rows_deleted`), while `anchor_oversized` and `anchor_missing` keep theirs (R-H2, the maintainer's decision of 2026-10-24); the 43 prose log messages this branch added are snake_case events, and only main's released prose messages may stay prose (R-H3, R-H6); after the plan's design reviews, the audit also removes the corrupt pool file (review C-1: the miss path keeps an existing pool file of the same hash, so an upstream still serving the same `InRelease` was refetched onto the rotted file; `legacy_anchor_discard_failed`), GC's startup pass returns a panic as an error and a panicking pass moves the rotation past itself, and `source_paths_skipped` is INFO; C3's detached-only suite is untracked only when first seen (deferred minor D-52); C5's skipped `Sources` paths are logged (`source_paths_skipped`, GD-2); C7's status page gains `anchorless`, failing checks with the end of their backoff, *Last requested*, `gc.retired_reaped` and the adoption ring's scheme; C9's reaper logs each completed run (`suite_reaper_run_complete`) and sets `acu_suite_reaper_last_run_unixtime`, `suite_reaped` carries `last_requested_unixtime` and `evidence`, the forward-step report counts in `acu_clock_steps_detected_total`, `/forget`'s 409 is logged at DEBUG (`suite_not_forgotten`) and a body that is not a form is 400, and GC recovers its goroutines' panics (`gc_panicked`); C10's strict wait re-runs at once when nothing is in flight, before the waiter cap; C11 lists the new events, fields and gauge; §9's H row names H1–H3; §10 records GD-5, decided for 1.1.0 by ruling R-H5 for the maintainer's confirmation: keep; revision 11, 2026-10-24: the maintainer's G-1 decision — `POST /reconcile` takes `/forget`'s guard, and without `htpasswd_file` both refuse a `Host` that is not an IP address, `localhost` or `admin.listen`'s host name; the read-only endpoints keep no `Host` check; revision 10, 2026-10-20/21/23: phase G plan rulings — a `Packages` file with no SHA256 stanza parses and adopts with incomplete coverage (`no_sha256`), and a body with no stanza naming a `Filename` still fails its parse (R-G1); every writer of a `url_path` row's `last_fetched_at` stamps it after the pool file is final, so the first adoption's `adopted_at` floor stays above the `max(pool mtime, last_fetched_at)` `Last-Modified` of url_path-served metadata, and blob mtimes are not folded into that floor (R-G3); the anchorless gate is signed evidence — the cached signed-anchor re-mark, then never-downgrade's rule — not every sniff-passing anchor row (phase G deviation 1); a plain `Contents-*` that changed is deleted, not kept, in the anchorless refresh (phase G deviation 2); the anchorless body cap is 64 MiB; a replaced row's `last_fetched_at` is `MAX(now, previous + 1)`; `flat_host_root_unsupported` is a WARN, one line a minute for the process with a `suppressed` count; a Filename made only of `.` segments is refused; `suites[].flat`; after the phase G plan reviews (rulings PG-1–PG-18): with `adopt_unsigned` off, a suite the anchorless gate admits is refused as an unsigned candidate is — the record `unsigned_disabled`, a successful check logged `changed_deferred` with form `anchorless`, paced by its record, no index request and no delete (its probes still run) — so a Release-less flat repository is served as first cached; the gate runs before the knob (PG-1, PG-10, PG-17); a Debian pool artifact (`.deb`, `.udeb`, `.ddeb`, `.dsc`, `.buildinfo`, `.changes`, `.diff.gz`, or a name with `_` and a `.tar.` component) is never metadata, so strict mode vouches and refuses an `icons-foo_1.0_all.deb` like any `.deb` (PG-2, PG-16); the sniff counter is `acu_anchorless_sniff_rejected_total{host}`, renamed from C4's first `anchorless_sniff_rejected` (PG-5); a flat suite whose base has no signed evidence is judged junk on its cached anchor rows and root `Packages*`/`Sources*` rows together — junk only when it has at least one such row and every one is junk evidence — while a flat suite with signed evidence, like every `dists/` suite, is judged by its anchors alone; the signed-evidence read is lazy, per candidate, a read error gives no verdict for the run, and the index rows are read only in a run that has an unadopted flat suite (R-G2 as amended by PG-7, R-G5); at most two anchorless bodies are in memory at once for the process (PG-8); an anchorless check is answered — a success that clears the record and the pending change — when an index request got a content answer (a 304, a 200 stored or matching the cached bytes, a 404 or 410) or when the suite has no index row to ask for, and is a failed check otherwise: a sniff-rejected 200, a changed plain `Contents-*`, a 401 or a 403 is no content answer (design review M-7, PG-12, PG-15); the refresh asks `Packages*`, then `Sources*`, then `Contents-*` (M-3), stores only the changed validators of a 200 with the cached bytes, a plain `Contents-*`'s too (M-1), deletes a row only while it holds the blob the refresh read (M-4), and logs its over-cap WARN at most once a minute for the process with a `suppressed` count (M-6); a replaced row's `last_fetched_at` can lead the clock as `adopted_at` can (M-8); the package-hash lookups join the suite's `suite_freshness` row by its key (M-2); C11's constants name the body cap, the two body slots and the two one-a-minute WARNs (M-10); the residuals of C3, C4 and C6 — constraints across a host's tenants, the twins' conflict window, an upstream that answers 403 for a missing key, an empty plain `Packages` — are documented, with no twin tiebreak (PG-13); after the phase G task reviews (rulings PG-19–PG-24): a flat suite active only through a client-created root index row, with no cached anchor row to model its probes on, probes its `InRelease` and `Release` at sibling URLs of that row's stored `upstream_url`, so a non-default port survives (PG-20); the anchorless refresh deletes a cached `InRelease` or `Release` only while it holds the blob read before the probe tree ran, so a client's fetch during the probe is kept (PG-24); a delete that fails is no answer — its 404s and 410s stop counting, so a check they alone answered fails (PG-24); a read error of the refresh's index rows is a local fault, logged with nothing written and no backoff (PG-24); an unchanged plain `Contents-*` answers the check without a sniff, so a captive portal's page cached as one answers it during the outage — a documented residual (PG-24); `flat` is the admin `suites[]` field, not a new log field (PG-23); `flat_host_root_unsupported` is logged only for a host the allowlist admits; a `Packages` stanza's Filename without a SHA256 is counted for `no_sha256`, never validated, and becomes no row, path or log value; after the phase G final review (rulings PG-28–PG-40): the anchorless refresh also deletes a cached `InRelease` or `Release` whose probe answered 401 or 403 when an index request of the same check got a content answer — S3's and CloudFront's answer for a missing key — while 404 and 410 delete it whatever the indexes answered (PG-28); a 200 over the 64 MiB cap drops its row for the miss path, no content answer, and its WARN names the file, replacing C4's first rule, which kept the row, and the fetcher fails a body whose `Content-Length` exceeds the cap without reading it (PG-29), and that WARN is logged only once the delete commits (PG-39); the suffix fallback stops at a dot-cleaned path longer than 5,121 bytes and skips a split whose base is longer than a suite path or whose name is longer than a Filename, which no row can match (PG-30); it runs only while the cache holds a flat `package_hash` row — a flag primed when the cache opens, whose failed read fails the start, set before any transaction that inserts such a row commits and never cleared while it runs (PG-31, PG-36); a never-adopted flat suite's metadata rows are read once, with its anchor rows, before the host slot and the probe tree, for Dispatch 3 and the refresh, so a read fault is a local fault before any request, and a shutdown during that read is no fault (PG-32, amending PG-24; PG-35); `package_hash_conflict` names each row's suite path (PG-33); the anchorless check line counts the rows it replaced and deleted (`rows_replaced`, `rows_deleted`), a failed check's error leads with the decisive reason and names the other after `; also `, and phase G's new WARNs carry `canonical_scheme` (PG-33, PG-37; the pre-G emitter of `freshness: read anchor rows failed` gains it in phase H: PG-34); the PG-1 clause above says no index request (its probes still run); C5 records that `ParseSources` keeps `validateMemberPath` (PG-21); the limits C2 and C6 call documented, and an adopted unsigned flat suite that stops publishing its `Release`, are documented in *Flat repositories* (PG-33, PG-38); revision 9, 2026-10-20: phase F plan rulings — the reaper and `POST /forget` delete a suite row only through phase D's compare-and-delete, holding the suite through the `SuiteGuard`, so one delete path remains (R-F1); `Gate.Purge` keeps a live entry (R-F2); the reaper also reaps legacy junk rows (R-F3); the orphan-marker sweep reads a future `last_seen_at` as now, and a marker is an orphan only when neither its base nor its `/.` twin has a row under either scheme, the rule `/forget`'s marker deletion follows too (R-F5); after the phase F plan reviews (rulings PF-1–PF-9), task reviews (rulings PF-10, PF-11) and the final review (rulings PF-12–PF-15): the sweep never rewrites a stamp (PF-1, phase F deviation 3); `/forget` deletes the cached anchors of the base and its twin under both schemes with the markers (PF-2); junk is a cached anchor that fails its sniff, an `InRelease` passing the `Release` sniff (a plain-text Release) excepted, whatever the suite's record, fetched at least 24 h ago (PF-3); the junk scan runs every run and the candidates reaped regardless of idleness go first (PF-4); with `adopt_unsigned` off an established unsigned suite keeps its coherent snapshot — R-F4's retirement into the ordinary cache is dropped (PF-5); the `/forget` guard stops a cross-site form or fetch, not DNS rebinding, and Host-header validation joins G-1 (PF-6); `/forget`'s answers say what happened and the status page shows each suite's scheme (PF-7); a forward step of the wall clock defers reaping and the sweep by the uptime floor, a suite's held work is bounded at 5 minutes, a reap stamps its base's markers, reaps are counted by reason (PF-8); every suite-row delete — cap eviction, a reap, a forget — moves the base's markers up to `MAX(last_seen_at, now)` in its transaction, so an orphan's year counts from the end of its path's tracking (PF-10, widening PF-8's reap stamp); a hand edit that adds one field to one log line touches that function, not the whole file, for the deferred-minor rule (PF-11); the marker sweep runs once a day; `0s` disables reaping, not the reaper's other duties; the idle cutoff is strict and a NULL or future stamp is never idle; `/forget` takes an optional `scheme`, runs to completion on a context a client cannot cancel, and answers 400, 403 and 500 besides 202 and 409; sub-job C deletes one retired snapshot per transaction without a grace; a re-adoption's `adopted_at` stays above every snapshot of its suite, retired ones included; the 90-day simulation runs the reaper every 6 h and restarts every fifth day, and a four-day simulation runs the production cadence (phase F deviation 2); after the phase F task reviews: a non-canonical suite key deletes no metadata; the sweep judges a future marker against a fresh clock read while its cutoff stays the run's, and the reaper's two clock-step reports are logged directly, outside the one-line-a-minute limit; a panic under a suite's hold is recovered for that suite alone, and a delete whose writer call failed after its commit is a reap; `/forget` refuses a `host` holding a scheme (400), and a shutdown of the admin listener cuts it short (503); after the phase F final review: before it retires a suite's snapshots, every suite-row delete turns never-downgrade's snapshot clause into a marker — when the suite has an adopted, non-retired inline or detached snapshot it upserts its base's marker under the suite's scheme, the one marker a delete creates (PF-12); a forward step the reaper detects never ages a marker (the sweep's cutoff takes off every step detected since the reaper started), and the first sweep is due a day after the reaper starts, so a start with the clock ahead has a day to be corrected (PF-13); the classes reaped regardless of idleness have their own budget of 250 a run, tried first, beside the 25 idle suites (PF-14); the startup GC pass runs no sub-job C (PF-15); a reaped suite's packages lose the version retention its snapshot gave them; a `/forget` panic is logged with its suite (`where=forget`), and a forget whose writer call failed after its commit is logged (`suite_forgotten`, `settled=true`) when a fresh read finds the row gone; revision 8, 2026-10-19: phase E plan rulings — before an unsigned commit the adopter re-marks the suite's cached signed anchors and refuses when it cannot; a refusal inside the commit is a refusal state with its own sentinel and the barrier outcome `refused`; one check tail, and an unexpected success status is a failed check; absent answers reset `check_fail_count`; `anchor_absent` is recorded at the first absent observation, and a future `adoption_fail_first_at` never confirms an unsigned transition; the probe tree's content-key gate, the confirmation details, the interest and client-created-row rules, a 304 only to a conditional request, SHA512 verification before the pool; strict mode waits for signed adoptions only; the scheduler bound's admission order; after the phase E plan reviews (rulings PE-1–PE-8): a probe that finds a signature marks the suite's base, and the re-mark also reads the `/.` twin's rows; never-downgrade reads the markers of both schemes and also refuses while an adopted, non-retired signed snapshot of the base exists; a deferred never-adopted suite and an adopted `anchor_absent` suite are paced by their record; a never-adopted suite whose tree finds no anchor also records `anchor_absent`, and so does a detached suite whose changed `Release` has no signature, unless a failed lateral attempt's record holds, which keeps its reason and paces the suite the same way; a cross-form request records no validators; the interest window is symmetric; the unsigned content key is domain-separated; the unsigned member cap is applied before a slot is taken; strict mode's wait, and the miss path's backstop, end at most 25 s after the request started; the probe bound counts client checks and lost offers; after the phase E task reviews (rulings PE-9–PE-11): the long-run retry simulation names no day count; a suite whose anchor answered absent behind a failed attempt's record is paced by that record; a detached suite's lateral `InRelease` is held only by its own last failure (an in-memory memo) or by the record of its own content, so the detached form's failures never postpone it and a new `InRelease` is attempted at the first check that sees it; a `Packages` stanza without a SHA256 makes coverage incomplete (`no_sha256`) and counts toward the stanza cap; a hit-path client that left during the strict wait is answered `client_canceled`; revision 7, 2026-10-18: phase D plan rulings — adoption accepts exactly apt's first `InRelease` line, and a marker line with trailing blanks decodes; a binary `Release.gpg` passes its sniff only with a signature version (3–6) where its packet header puts the body; suite paths over 1024 bytes are not tracked; cap eviction holds the evictee's mutex through the checker, tries at most eight candidates, evicts at most one suite per creation attempt at or above the global cap (an attempt above it is still refused) and deletes through the compare-and-delete the reaper reuses, and a startup WARN names a table over its caps; the rejection cache holds at most 4,096 suites; `suite_seed_rejected` is rate-limited to one line a minute; the C3 metric names; the hit-path invariant covers tracked suites, and every row creation marks the suite's cached signed anchors in its own transaction; with no adopter a refresh miss re-stamps its row as before; the T1 on a creating miss lands with phase E; an attempt a shutdown ends before the barrier is abandoned like one in it; revision 6, 2026-10-18: phase C plan rulings — the C8 barrier keeps the adoption's global slot and does not honour `adoption.max_duration` but ends on shutdown (the daemon closes it at the signal), so the admission bound's *A* includes it; a hold that held no request starts no cross-suite quiet period; `flip_quiet_period` is `0s` or `1s` to `15s`; every C8 residual has its own counter; the hold-token residual and the five-suite DoD corrected (four hold, the fifth commits unheld); retained by-hash serves SHA512 aliases from phase C; exact C2 predicate and C3 `Release.gpg` sniff wording; after phase C's final review: once cross-suite quiet holds, a hold commits at 15 s (20 s less a 5 s commit margin) instead of answering its waiters from the old snapshot at 20 s — its late readers are a counted residual — and snapshot anchors carry `adopted_at` alone as `Last-Modified`; revision 5, 2026-10-17: slot-driven admission in C4 — a suite that finds no slot waits and is re-checked when one is released; the admission bound — after the phase B plan review; revision 4, 2026-10-05/11: legacy trust audit, signed-seen ordering, fail-closed inline verification, flip hold token, fair scheduler — after the 2026-10-05 plan review, a follow-up review, and a three-lens review of grounding, alternatives and residual risk; 2026-10-17: PR #8 parity notes in C10, §7 and §9)
 - **Status:** draft for maintainer review
 - **Branch:** `feature/flat-repos` (from `main` @ 536a1ab)
 - **Origin:** reimplementation of the problem reported in PR #8 ("Refresh flat
@@ -359,9 +359,11 @@
   `check_fail_count = 0` — with an UPDATE in the same writer operation,
   unless the suite has a current snapshot (phase D, ruling PD-6). A
   detached-only suite (`Release` and `Release.gpg`, no `InRelease`) is not
-  tracked: before phase D its `Release` hit's T1 created a row through the
-  old upsert, so it loses its Phase-1 change observations and its admin row
-  (documented).
+  tracked when first seen: before phase D its `Release` hit's T1 created a
+  row through the old upsert, so a newly seen one loses its Phase-1 change
+  observations and its admin row (documented); one an earlier release
+  already tracks stays tracked — its `Release` serves touch it, so neither
+  eviction nor the reaper removes it (phase H1, deferred minor D-52).
 
 A creation by a hit (legacy onboarding) leaves the check stamps and
 validators NULL in both modes, so its first check runs at once. A plain-text
@@ -922,6 +924,12 @@
   `?`, `#`, `..` segment, empty segment, length > 4096, and a Filename made
   only of `.` segments (it names no file; phase G design note 5).
 
+A `Sources` path `validateMemberPath` refuses — every file of a stanza
+whose `Directory` fails it, and each file whose own path does — gets no
+row, as before; it is no longer silent: `ParseSources` counts such paths
+and keeps the first, and the adoption logs INFO `source_paths_skipped`
+once per `Sources` file that skipped any (phase H1, deferred minor GD-2).
+
 A stanza's Filename without a SHA256 is counted for `no_sha256`, not
 validated: it becomes no row, no path and no log value (phase G, the Task 3
 review).
@@ -1269,7 +1277,16 @@
 
 **Visibility.** Admin `suites[]` gains `anchor_form`, `flat`,
 `last_requested_unixtime`, and the record ("adoption failing 3d 2h ·
-untrusted_signer · retry in 41m", or "refused: upstream_older"). "Lagging" is
+untrusted_signer · retry in 41m", or "refused: upstream_older"). Phase H1
+adds `anchorless` — a flat suite refreshed without a `Release`, derived
+from persisted state alone: a flat suite, adoption enabled, no current snapshot, a last
+success, no record, no pending change (C4 Dispatch 4) — shown as
+"refreshed without a Release"; "checks failing ×k · backoff ends in …"
+while `check_fail_count` > 0 (the scheduler's backoff from `last_check_at`:
+the earliest it may check; the count alone for a dormant never-adopted
+suite and one its record paces, which the backoff does not schedule); a
+*Last requested* column; `gc.retired_reaped`; and the adoption ring's
+`scheme`, shown as `scheme://host`. "Lagging" is
 computed from the first-seen `inrelease_change_seen_at` against `now`. Gauge
 `acu_adoption_failing_suites`; INFO `adoption_deferred` on state transitions;
 WARN `adoption_failing_persistently` at streak counts 3, 6, 12, …; the
@@ -1566,7 +1583,14 @@
 a restart, uncorrected for a day, still ages markers by it, and a daemon
 restarted at least once a day (before its first sweep comes due) never
 sweeps (orphan markers are small rows: the safe side). Reaps are
-counted by reason (`acu_suites_reaped_total{reason}`). The gpg
+counted by reason (`acu_suites_reaped_total{reason}`); `suite_reaped`
+carries the judged stamp of an idle reap (`last_requested_unixtime`) and
+the judged files of a junk one (`evidence`), and a completed run — its
+listing read, its reaps tried, its sweeps done, or held back by the
+forward-step guard — logs INFO `suite_reaper_run_complete` and sets the
+gauge `acu_suite_reaper_last_run_unixtime`, whose age an operator alerts
+on (phase H1). The guard's forward-step report counts in
+`acu_clock_steps_detected_total` like every report. The gpg
 once-logger sets start over when a new key finds 10,000 entries.
 Snapshot GC gains sub-job C,
 in the periodic ticks only — the startup pass, which runs before the
@@ -1582,7 +1606,16 @@
 members and `package_hash` rows in bounded chunks (10,000 rows per writer
 transaction), then the header row; it needs no grace, since a retired
 snapshot is never a candidate again (the collision lookup ignores it and
-`CommitAdoption` refuses it).
+`CommitAdoption` refuses it). Sub-job B never deletes a snapshot still
+queued for §6.1's audit (one the audit deferred after a read fault), nor
+counts it in its `keep_displaced` ranking: with `keep_displaced = 0` such a
+displaced snapshot can be its base's only signed history, which
+never-downgrade reads until the audit decides it (ruling R-H1, phase H1;
+the predicate sits before the ranking, so a queued snapshot takes no
+place). GC recovers a panic per periodic tick and per pool-scan job —
+ERROR `gc_panicked` — and goes on; a tick's panic moves the pass rotation
+past the pass that panicked, and the startup pass returns its panic as an
+error, which the daemon logs before it serves (phase H1).
 
 **Migration backfill** of `last_requested_at`: the newest client
 `last_requested_at` of the suite's anchor `url_path` rows, else migration time
@@ -1613,9 +1646,12 @@
 suites (no existence oracle; the body says what to check: the status
 page, which now shows each suite's scheme, shows `scheme://host` in its
 Host cell — the scheme goes in `scheme`, default `https`, the rest in
-`host` — and the suite in its Suite path column; ruling PF-7), 400 for a
-missing `host` or `suite`, another scheme, or a `host` holding `://`
-(the Host cell pasted whole), 500 for a database error (logged), and 503
+`host` — and the suite in its Suite path column; ruling PF-7; the log
+says which, at DEBUG: `suite_not_forgotten` with `reason` `no_suite`,
+`busy` or `changed`, phase H1), 400 for a
+missing `host` or `suite`, another scheme, a `host` holding `://`
+(the Host cell pasted whole), or a body that is not a form (413 only for
+one over 8 KiB; phase H1), 500 for a database error (logged), and 503
 when the admin listener's shutdown cut it short (each metadata batch and
 the final delete are their own transactions, so a forget cut short is
 safe to repeat). A final delete that committed although its writer call
@@ -1667,7 +1703,10 @@
 again, a client that left during the wait gets no retry, and the refusal
 is logged once per fetch, by the singleflight leader (design review O1).
 At most 64 waiters per host, each waiting request counting (coalesced
-requests for one `.deb` included); beyond that the refusal is immediate.
+requests for one `.deb` included); beyond that the refusal is immediate —
+unless nothing is in flight any more, which is checked first: a request
+that arrives as the host's last adoption ends, while its waiters still
+unwind, re-runs its lookup (phase H1, deferred minor E-58).
 
 Documented consequences: a client fetching N unvouched packages one
 after another during a long adoption waits up to 15 s for each (M-11); an
@@ -1701,10 +1740,23 @@
 `handler_goroutine_panicked`, `adoption_panicked`, `clock_step_detected`,
 `flat_host_root_unsupported`, `legacy_snapshot_quarantined`,
 `legacy_trust_audit_progress`, `legacy_trust_audit_deferred`,
-`legacy_trust_audit_completed`; new fields `anchor_form`,
+`legacy_trust_audit_completed`; phase H1's `suite_reaper_run_complete`,
+`suite_not_forgotten` (DEBUG), `gc_panicked`, `legacy_anchor_discard_failed`,
+`reconcile_snapshot_read_failed` and `source_paths_skipped`, and the 43
+lines this branch added with prose messages, renamed to snake_case event
+names, each keeping its level and fields (`suite_onboarding_failed`,
+`suite_reaper_list_suites_failed`, `admin_forget_failed`,
+`freshness_anchorless_index_over_cap`, `freshness_check_cancelled`,
+`suite_touch_failed` and their siblings; rulings R-H3 and R-H6: no release
+logged the prose; a guard test fails any new prose message, main's released
+ones excepted); new fields `anchor_form`,
 `snapshot_displaced`, `flip_held_ms`, phase G's `rows_replaced` and
 `rows_deleted` (the anchorless `freshness_check` line, C4 Dispatch 4) and
-`suite_path` in `package_hash_conflict`'s `declared` entries (C6) (`flat` is
+`suite_path` in `package_hash_conflict`'s `declared` entries (C6), and phase
+H1's `anchor_rows_deleted` (`legacy_snapshot_quarantined`, §6.1),
+`last_requested_unixtime` and `evidence` (`suite_reaped`), `paced_stale`
+(the stale-suites WARN), `canonical_scheme` on `admin_forget_requested`,
+`admin_forget_failed` and beside `admin_reconcile_requested`'s `scheme` (`flat` is
 the admin `suites[]` field, C7, not a log field: every suite line's
 `suite_path` shows the layout; phase G ruling PG-23). New metrics include
 the unlabelled
@@ -1717,14 +1769,16 @@
 `acu_strict_adoption_waits_total{host}`, and phase F's unlabelled counter
 `acu_gc_retired_reaped_total` and `acu_suites_reaped_total{reason}` (three
 values: `idle`, `non_canonical`, `junk`), and phase G's
-`acu_anchorless_sniff_rejected_total{host}` (C4 Dispatch 4).
+`acu_anchorless_sniff_rejected_total{host}` (C4 Dispatch 4), and phase H1's
+unlabelled gauge `acu_suite_reaper_last_run_unixtime` (C9).
 New metrics carry no suite labels: they are unlabelled, host-labelled or,
 for `acu_suites_reaped_total`, labelled by its three reasons. They are
 listed in the metrics inventory comments of `internal/freshness/metrics.go`
 and `internal/handler/metrics.go`, except `acu_gc_retired_reaped_total`,
 declared with the other `gc_run_complete` counters in
 `internal/gc/metrics.go`.
-`gc_run_complete` and `LastRunSummary` gain `retired_reaped`.
+`gc_run_complete` and `LastRunSummary` gain `retired_reaped`, which the
+admin status JSON's `gc` block carries from phase H1.
 
 | Key | Default | Rule |
 |---|---|---|
@@ -1902,8 +1956,26 @@
 - **Quarantine** (`cache.ApplyLegacyAuditDecisions`, in its batch's writer transaction):
   if the snapshot is a suite's current snapshot, clear `current_snapshot_id`
   and that suite's validators and run the Step-8 refcount decrement for it;
-  set `retired_at`; dequeue. WARN `legacy_snapshot_quarantined` (reason,
-  snapshot, host, suite, `was_current`); counter
+  set `retired_at`; dequeue. For `anchor_corrupt` the same transaction also
+  deletes every `url_path` row whose `blob_hash` is the snapshot's stored
+  `InRelease` hash — under any scheme or host, through the writer — so
+  the next client request fetches a fresh copy from upstream instead of
+  being served bytes apt rejects (the maintainer's decision of 2026-10-24,
+  ruling R-H2, phase H1). In the transaction, not after it: the dequeue
+  commits with the delete, so a crash between them cannot dequeue the
+  snapshot with its corrupt rows still served. The audit also removes the
+  corrupt pool file (`DiscardFinalizedBlob`, the integrity scanner's
+  remedy), on its own goroutine before the batch commits, never on the
+  writer: the miss path keeps an existing pool file of the same hash, so
+  an upstream still serving the same `InRelease` would otherwise be
+  fetched onto, and served from, the rotted file (phase H1, review C-1). A
+  commit lost after the removal re-audits the snapshot as `anchor_missing`
+  at the next start; a failed removal is logged (ERROR
+  `legacy_anchor_discard_failed`) and the quarantine goes ahead.
+  `anchor_oversized` (over the 8 MiB read, not proven bad) and
+  `anchor_missing` (nothing to serve) keep their rows, and a read error is
+  still no evidence. WARN `legacy_snapshot_quarantined` (reason,
+  snapshot, host, suite, `was_current`, `anchor_rows_deleted`); counter
   `acu_legacy_snapshots_quarantined_total`. A quarantined snapshot never
   serves, never vouches or counts for strict mode (both read current
   snapshots only), is excluded from retained by-hash (non-retired only), can
@@ -1926,7 +1998,7 @@
 and duration.
 
 A quarantined suite falls back to Phase 1 serving (its `url_path` anchor row
-is kept) until it re-adopts. Re-adoption happens on the first check that
+is kept — after `anchor_corrupt`, deleted: R-H2 above) until it re-adopts. Re-adoption happens on the first check that
 observes anchor bytes different from the cached ones (phases A–D) or by
 eager first adoption (phase E). For an R-1 quarantine that is the first
 post-upgrade check: the cached anchor holds the tampered envelope, which a
@@ -2159,7 +2231,7 @@
 | **E. Eager adoption and anchor forms** | C4 probe tree, classification, form transitions, `anchor_absent`, eager first adoption with interest window, T1-on-seed; C5 unsigned form with safeguards; SHA512 merge, shared verify helper, aliases (served from displaced snapshots by phase C's retained by-hash); C10 in-flight wait. | Production-shaped fixture: active never-adopted suites adopt once, dormant ones make zero requests; classification table; never-downgrade at both points, including the in-transaction recheck race; a §6.1-quarantined plain-text suite adopts unsigned without a downgrade refusal; unsigned never changes posture; nvidia-shaped fixture adopts; SHA512 by-hash served; in-flight wait on hit and miss paths. |
 | **F. Suite reaping** | C9 retirement, reaper (own ticker), batched metadata deletion, sub-job C, memory purge, marker TTL, `POST /forget`. | 90-day simulation at `gc.interval = 24h` with restarts; return-before-GC and `/forget` re-adoption tests; forward-step self-heal; never-downgrade after reap; `/forget` CSRF test; twins untouched. |
 | **G. Flat repositories** | C2 flat grammar; flat IndexTargets (layout-aware, adoption and handler sides); `validateFilename`; C6 materialisation, fallback, coverage; anchorless refresh; flat root-index touch; flat pdiff arch labels. | PR #8's evidence: a miss on `/…/deb/InRelease` and on `/…/deb/./InRelease` creates one suite row each and fires T1 under it, a hit fires T1 again, and `/…/deb/Packages_1.0_all.deb`, host-root `/InRelease` and `/…/deb//InRelease` create none (no check fires for either path on `main`); `/core:/stable:/v1.34/deb/InRelease` → suite `/core:/stable:/v1.34/deb`; every real Filename shape adopts and vouches; flat `Packages` failure fatal; strict matrix; anchorless refresh tests; flat legacy onboarding on a v6 fixture (an untrusted flat suite makes ≤ 1 attempt per backoff step); reaping `/repo` leaves `/repo/.` intact; PR #8's remaining tests ported with the expected results in the plan overview's "PR #8 parity" section. |
-| **H. End to end and docs** | `e2e/flat/`; documentation; config file; final full-diff review. | `make e2e-flat` green on all images with the grep assertions, including PR #8's end-to-end table (one signed flat repository configured as `deb <uri>/ /` on one client and `deb <uri> ./` on another: both report 2.0 within one cooldown of the v2 publish, and with the upstream stopped two fresh clients install 2.0 from the snapshots); a manual live-upstream smoke (plan overview, "PR #8 parity") before the release that ships G; docs complete. |
+| **H. End to end and docs** | H1: the sweep — every deferred review minor of phases A–G, the trust audit's corrupt anchors (R-H2), snapshot GC's audit-queued snapshots (R-H1), snake_case event names (R-H3, R-H6), the docs for its own changes; H2: `e2e/flat/`; H3: documentation (README, release notes), config file, final full-diff review. | `make e2e-flat` green on all images with the grep assertions, including PR #8's end-to-end table (one signed flat repository configured as `deb <uri>/ /` on one client and `deb <uri> ./` on another: both report 2.0 within one cooldown of the v2 publish, and with the upstream stopped two fresh clients install 2.0 from the snapshots); a manual live-upstream smoke (plan overview, "PR #8 parity") before the release that ships G; docs complete. |
 
 ## 10. Out of scope / follow-ups
 
@@ -2194,6 +2266,17 @@
     `Host: 127.0.0.1` (phase F, ruling PF-6, had left it open).
 11. First contact costs two upstream GETs of the anchor (the miss and the
     eager probe).
+12. An adopted unsigned flat suite whose upstream stops publishing its
+    `Release` keeps its last snapshot, visibly `anchor_absent`, whose old
+    `Packages` may name `.deb`s upstream has since deleted (404), until
+    `POST /forget` turns it into a Release-less suite the anchorless
+    refresh serves (C4 Dispatch 1–2, documented in *Flat repositories*).
+    Whether such a suite should drop its snapshot and refresh anchorless
+    once its `Release` has stayed absent was decided for 1.1.0 by a
+    controller ruling, for the maintainer's confirmation: keep; revisit if
+    seen in the field (phase H1, ruling R-H5, deferred minor GD-5): it is rare, the kept snapshot keeps serving cached content when
+    an upstream dies outright, where a drop would turn that into 404s, and
+    a drop is a new state transition with trust and serving consequences.
 
 ## 11. Risks
 
````

- [ ] **Step 7: The overview**

The spec reference becomes revision 12 (revision 11 left it at 10); the deferred-minors constraint says where the IDs resolve now — H1's disposition table, the phase plans, and the deleted files through git (design note 16, review sweep M-9) — and drops "or the maintainer's" (review sweep M-1); the phase table's H row becomes H1 (in progress; it becomes "done (commit …)" after the final review, as phase G's did), H2 and H3, whose row names the strict-mode docs; the Cross-phase interfaces gain H1's names; each Carry-forwards **H:** item H1 closes is marked done and each H3 item says so — the two "PR #8 parity" H: items too (the e2e item is left for H2's plan, which marks it) — and the (D) item moves there from "PR #8 parity" (D-55), so the (E) item says "with phase D's (above)"; the block heading becomes "Decisions (spec-level; none open)": the decision on corrupt anchors is decided and done (R-H2), and GD-5 is recorded as decided for 1.1.0 by ruling R-H5, for the maintainer's confirmation (review sweep M-8, M-10); the H1 interface line names the 43 renames, `legacy_anchor_discard_failed` and the guards, and the two cross-phase lines that quote a message as current name its event (the carry-forward items keep the old names they list).

````diff
diff --git a/docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md b/docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md
--- a/docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md
+++ b/docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md
@@ -8,7 +8,7 @@
 
 **Tech Stack:** Go 1.25 (toolchain 1.26), modernc SQLite, `github.com/ProtonMail/go-crypto`, `testing/synctest`, Docker for e2e.
 
-**Spec:** `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (revision 10). Plan reviews: 2026-09-28 (phase A executability and tests) and `2026-10-05-flat-repositories-plan-review.md` (upgrade trust and later-phase contracts; its findings map to the "Review findings 2026-10-05" table below). Every task implements a named spec section; when this plan and the spec disagree, the spec wins unless the plan records a deliberate, explained deviation (listed below).
+**Spec:** `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (revision 12). Plan reviews: 2026-09-28 (phase A executability and tests) and `2026-10-05-flat-repositories-plan-review.md` (upgrade trust and later-phase contracts; its findings map to the "Review findings 2026-10-05" table below). Every task implements a named spec section; when this plan and the spec disagree, the spec wins unless the plan records a deliberate, explained deviation (listed below).
 
 ## Global Constraints
 
@@ -28,7 +28,7 @@
 - Trust is never inferred from "adopted": only a decodable clearsigned `InRelease` or a verified detached pair is signed. No phase may reintroduce a text fallback in `VerifyInline`, write a signed-seen marker for a plain-text or undecodable anchor, or let a retired snapshot serve, vouch or become current. (Reconcile's own plain-text fallback in `inReleasePlaintext` survives phase A only for test fixtures and is removed in D; production cannot reach it.)
 - An I/O fault is never evidence: code that removes trust (audit, quarantine, reaping) acts on "file does not exist" or on content it read and checked, and on any other read error defers (leaves the item queued, changes nothing, logs ERROR). Refusing to start is reserved for faults that would misjudge everything (database errors, a missing or empty pool).
 - **Release boundary (maintainer decision, 2026-10-17):** phases A–H ship together, with other changes, in the 1.1.0 release candidate. Nothing from this branch is released earlier, so no phase needs to be independently releasable; each phase still ends green (tests, lint, gofmt) so the branch stays bisectable. The phase order is unchanged.
-- Deferred review minors are listed per phase: `2026-09-28-flat-repositories-A-deferred.md` (IDs A-1…A-57), `2026-09-28-flat-repositories-B-deferred.md` (IDs B-1…B-68), `2026-09-28-flat-repositories-C-deferred.md` (IDs C-1…C-40), `2026-09-28-flat-repositories-D-deferred.md` (IDs D-1…D-60), `2026-09-28-flat-repositories-E-deferred.md` (IDs E-1…E-77), `2026-09-28-flat-repositories-F-deferred.md` (IDs F-1…F-26) and `2026-09-28-flat-repositories-G-deferred.md` (IDs GD-1…GD-6; `GD`, not `G`, so they never collide with spec §10's `G-1`: phase G ruling PG-25). Each later phase plan includes the items, from all seven files, in files it touches (an item tagged with a phase letter is owned by that phase); phase H sweeps the rest and deletes the seven files. Phase H takes F-24 (a pre-existing never-downgrade gap with `gc.keep_displaced = 0`) early.
+- Deferred review minors are listed per phase: `2026-09-28-flat-repositories-A-deferred.md` (IDs A-1…A-57), `2026-09-28-flat-repositories-B-deferred.md` (IDs B-1…B-68), `2026-09-28-flat-repositories-C-deferred.md` (IDs C-1…C-40), `2026-09-28-flat-repositories-D-deferred.md` (IDs D-1…D-60), `2026-09-28-flat-repositories-E-deferred.md` (IDs E-1…E-77), `2026-09-28-flat-repositories-F-deferred.md` (IDs F-1…F-26) and `2026-09-28-flat-repositories-G-deferred.md` (IDs GD-1…GD-6; `GD`, not `G`, so they never collide with spec §10's `G-1`: phase G ruling PG-25). Each later phase plan includes the items, from all seven files, in files it touches (an item tagged with a phase letter is owned by that phase); phase H sweeps the rest and deletes the seven files. Phase H takes F-24 (a pre-existing never-downgrade gap with `gc.keep_displaced = 0`) early. *Done in phase H1:* every item has a disposition in `2026-09-28-flat-repositories-H1-sweep.md` (fixed, already done, or declined with its reason), and the seven files are deleted; a code comment that cites an item by ID resolves through H1's disposition table (the 136 swept there), the phase plan that took it (the rest), and the deleted files themselves: `git log --diff-filter=D -- 'docs/superpowers/plans/*-deferred.md'` names the commit, and `git show <commit>^:<path>` prints a file (H1 design note 16).
 - Every phase-A task must stay justified by PR #8 (its core problem or one of its four limitations), a named maintainer decision, or a confirmed defect; the review-findings table below records the mapping for the 2026-10-05 changes.
 - Commit messages end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
 - Credit: the flat-repository problem was reported and first analysed by Venkat (@venkatamutyala) in PR #8, and this work is based on that PR. They are credited in `Contributors.md` (added 2026-10-17, after phase A); every phase keeps that credit and its PR #8 reference.
@@ -65,7 +65,9 @@
 | E. Eager adoption and anchor forms | `…-E-forms.md` | C4 probe tree, C5 unsigned/SHA512, C10 wait | B, D | done (commit 9ab48c4) |
 | F. Suite reaping | `…-F-reaping.md` | C9 | D, E | done (commit dfdbb90) |
 | G. Flat repositories | `…-G-flat.md` | C2 flat, C5 flat IndexTargets/Filenames, C6, C4 anchorless | A–F | done (commit e2d7ada) |
-| H. End to end and docs | `…-H-e2e-docs.md` | §7 e2e, §8 | all | to write after G |
+| H1. The sweep | `…-H1-sweep.md` | every deferred minor of A–G; §6.1 (R-H2) and C9 sub-job B (R-H1); C11 names (R-H3, R-H6); the docs of its own changes | A–G | in progress (plan written 2026-10-24) |
+| H2. End to end | `…-H2-e2e.md` | §7 e2e | H1 | planned |
+| H3. Docs and release | to write | §8 (README), release notes, the strict-mode docs (`docs/configuration.md`), the live-upstream smoke, final full-diff review | H1, H2 | to write after H2 |
 
 ## Cross-phase interfaces (names later phases rely on)
 
@@ -91,10 +93,11 @@
 
 - B: `cache.RecordAdoptionFailure`, `cache.ClearAdoptionFailure`, `freshness.contentKey`, `freshness.backoffDelay`. `adoption_next_retry_at` is a future-event stamp: B clamps it to now + 1 h and never applies the past-event ("infinitely old") rule to it — a detection is only logged through `clockStep` (phase B plan, deviation 6); B tests that a future `next_retry_at` after a backward step yields a retry within 1 h, not immediately. B also records panicked adoptions in the admin adoption ring. **Scheduler fairness (review finding 7 and the three-lens review, spec C4):** per-host queues of due suites refreshed every tick (no duplicates, persist across ticks), 4 workers, at most one check in flight per host, hosts take turns round-robin one check per turn; within a host healthy (`check_fail_count = 0`) → never-checked → failing, first in first out by queue time inside each class; backoff 2 min → 1 h computed from the persisted `check_fail_count` and `last_check_at` (every check maintains the streak in the UPDATE that already bumps `last_check_at`); goroutines and queue memory bounded. Latency bound: one tick interval + (n + 1) × ⌈H/4⌉ × 2 min, n = suites ahead of S in its host's queue (healthy ones only when S is healthy). B's DoD includes synctest simulations with **stalling** responders (not refused connections, which the fetch client's unreachable-host cooldown already collapses): (1) H = 8 hosts, several stalling, a healthy suite with n = 3 healthy suites ahead — assert the bound; (2) 500 stalling suites and 1 healthy suite on one host — the healthy suite is checked within one tick + ⌈H/4⌉ × 2 min, and again on the first tick after a simulated restart (a mutation that orders never-checked suites first, keeps the streak in memory, or keys the order on `last_check_at` must fail it); both with persistent repair and changing-candidate contention, continuing checks and a goroutine bound; the capacity simulation exercises both check and adoption admission. (An equivalent pull model — a free worker asks the cache for the next due suite — is acceptable if B's plan finds the queue bookkeeping error-prone.) **Slot-driven admission (spec C4 rev 5, phase B Task 11b):** `(*admission).freeFor(host, signedCurrent) int` (a peek that mirrors `tryAcquire`) and its `freed` wake channel; `checkOpts{bypassCooldown}`, `checkReport{result, signedCurrent}`, `(*Checker).check(ctx, ref, opts) checkReport` behind `Check`; `isQueuedResult(result)`; scheduler `park`, `finish`, `unhold`, `pop(admit)`; gauge `acu_freshness_waiting_for_slot`. A scheduler check that ends queued leaves its suite waiting in its host's queue; every slot release re-offers the freed capacity to the oldest waiting suite it admits, cooldown bypassed; a busy offer is held until the next wake or tick.
 - C: package `flipgate` — `Gate` (`New(Config{Quiet})`, `Enabled`, `Quiet`, `Stats`, `Len`, `Purge(key)`, `Close()` (shutdown; `main` calls it at the signal), `Closed()`, `AnchorStart(ctx, key) *Token`, `Flip(ctx, key, commit) (Result, error)`), `Key(scheme, host, suitePath)` (the freshness suite key), `Token.Done`, `Token.Held`, outcomes `held`/`quiet_cut_short`/`hold_expired`/`hold_skipped`/`aborted`/`disabled`, `ErrAborted`, `ErrClosed`, the spec C11 constants (`PhaseAMax`, `TokenWaitMax`, `HoldMax`, `CommitMargin`, `AnchorTimeout`, `MaxWaiters`, `MinQuiet`, `MaxQuiet`; `MaxQuiet ≤ HoldMax − CommitMargin` is pinned), the counters `acu_flip_hold_skipped_total`, `acu_flip_quiet_cut_short_total`, `acu_flip_hold_expired_total`, `acu_flip_waiters_over_cap_total`, `acu_flip_slow_anchor_reads_total`, `acu_flip_gate_panics_total` and the gauge `acu_flip_barrier_active`. `freshness.AdoptionConfig.FlipGate` and the nil-safe `(*Adopter).FlipGate()` (the handler shares the adopter's gate: `handler.Config.FlipGate`); `(*Adopter).SetCommitDelayForTest` (test seam: a commit's latency in simulations); `handler.Config.KeepDisplaced`; `cache.DisplacedSnapshotMembers`; `config.AdoptionConfig.FlipQuietPeriod`; the events `adoption_flip_barrier` (outcome `commit_failed` when the commit fails) and `adoption_abandoned_at_shutdown` (a flip abandoned at the signal: INFO, never an adoption outcome); the request fields `snapshot_displaced` and `flip_held_ms`; `acu_serve_byhash_displaced_total{host}`. Snapshot anchors' `Last-Modified` is `adopted_at` alone (never the pool mtime). Rulings: an adoption keeps its global slot through the barrier and frees its host slot before it (R-C1); the barrier's waits ignore `adoption.max_duration` and end on shutdown, committing nothing (R-C2); the admission bound's *A* includes the barrier's ≤ 140 s (R-C3). Five busy suites flipping at once: four hold, the fifth commits unheld (phase C plan, deviation 2).
-- D: `cache.CreateSuite`, `cache.UpdateSuiteFreshness`, `cache.TouchSuite`, `cache.ErrSuiteGone` (the UPDATE-only `CommitAdoption` Step 6, spec C3), `aptpath.SniffRelease`, `aptpath.SniffFlatIndex` (the `InRelease`/`Release.gpg` sniffs and marker writes already exist from A). As built: `cache.CreateSuite(ctx, SuiteSeed, SuiteCaps, SuiteGuard) (CreateResult, error)` (`SuiteSeed.Cached` carries the suite's pre-read cached anchors, `CreateResult.Unproven` asks for the backstop mark, `CreateResult.CandidatesBusy` flags a transient all-busy refusal), `cache.StoreFetched(ctx, FetchedRow, SuiteCaps, SuiteGuard) (FetchedResult, error)` (`FetchedRow.Restamp`: the adoption-disabled refresh miss), `cache.SuiteExists` (renamed `HasSuiteRow` in phase F), `cache.SuiteCapUsage`, `cache.TouchSuite` / `TouchedSuites`, `cache.SuiteGuard` (implemented by `(*freshness.Checker).HoldSuite`) and the optional `cache.ClockStepReporter` (the checker reports eviction's future stamps), unexported `deleteSuiteTx(ctx, tx, key, wantCurrent *int64, cutoff, now) (deleteOutcome, error)` (F's `ReapIdleSuite` reuses it), `freshness.Config.MaxSuites`, `config.FreshnessConfig.MaxSuites`, `aptpath.ClassifySuiteFile` / `SuiteFile`, `aptpath.MaxSuitePathBytes`, `aptpath.DecodableClearsign`, `simtest.ClearsignedFixture`, `(*freshness.Checker).LockCount`. **Signed-seen marker on the miss path (review finding 5, its follow-up and the three-lens review, spec C3):** anchor rows (`InRelease`, `Release`) are created in the writer operation that stores the `url_path` row, before the body is written (sniff → `url_path` + `CreateSuite` + marker → write body → T1, today's seed-before-serve order (the T1 on a creating miss: phase D plan, design note 2; phase E plan, Task 10)); the marker for a sniff-passing `InRelease`/`Release.gpg` is written in that same operation and nowhere else in the handler — no hit-path write (except that every row creation, on a miss or on a hit, marks the suite's cached signed anchors in its own transaction: phase D plan, deviation 2, ruling PD-5), no wait, no coalescing map, no failure counter; a failed operation serves no body (502, as today). `Release.gpg` creates no row. **The backstop and its residual:** the one marker write outside a creating or storing transaction is the handler's `markCachedSignedAnchors`, which runs when a creation is unproven (`CreateResult.Unproven`: one of the suite's cached `InRelease`/`Release.gpg` rows changed, appeared or could not be read between the handler's pre-read and the creating transaction). It reads, sniffs and marks that anchor right after the commit and the late registration, before the response, bounded at 5 s. So an unproven creation's suite is tracked with that anchor unmarked from the commit until the backstop runs, and — if the backstop fails (WARN `suite signed-seen mark failed` or `suite signed-seen backstop read failed`) — until the startup marker pass at the next start (spec C3 accepts it; E carry-forward below). Tests: the marker row exists before the first body byte on first contact and on a refresh miss; a failed writer operation serves no body; the hit-path invariant (after misses, adoptions, `reconstructAnchor` and restarts, every sniff-passing anchor row of a tracked suite has a marker (deviation 1)); FIFO ordering against a later unsigned commit. D also removes reconcile's plain-text fallback in `inReleasePlaintext` once `clearsignedFixture()` replaces the plain-text test fixtures (flip `TestReparseSnapshotRelease_PlainTextInReleaseStillParses` to `_PlainTextInReleaseIsAnError`).
+- D: `cache.CreateSuite`, `cache.UpdateSuiteFreshness`, `cache.TouchSuite`, `cache.ErrSuiteGone` (the UPDATE-only `CommitAdoption` Step 6, spec C3), `aptpath.SniffRelease`, `aptpath.SniffFlatIndex` (the `InRelease`/`Release.gpg` sniffs and marker writes already exist from A). As built: `cache.CreateSuite(ctx, SuiteSeed, SuiteCaps, SuiteGuard) (CreateResult, error)` (`SuiteSeed.Cached` carries the suite's pre-read cached anchors, `CreateResult.Unproven` asks for the backstop mark, `CreateResult.CandidatesBusy` flags a transient all-busy refusal), `cache.StoreFetched(ctx, FetchedRow, SuiteCaps, SuiteGuard) (FetchedResult, error)` (`FetchedRow.Restamp`: the adoption-disabled refresh miss), `cache.SuiteExists` (renamed `HasSuiteRow` in phase F), `cache.SuiteCapUsage`, `cache.TouchSuite` / `TouchedSuites`, `cache.SuiteGuard` (implemented by `(*freshness.Checker).HoldSuite`) and the optional `cache.ClockStepReporter` (the checker reports eviction's future stamps), unexported `deleteSuiteTx(ctx, tx, key, wantCurrent *int64, cutoff, now) (deleteOutcome, error)` (F's `ReapIdleSuite` reuses it), `freshness.Config.MaxSuites`, `config.FreshnessConfig.MaxSuites`, `aptpath.ClassifySuiteFile` / `SuiteFile`, `aptpath.MaxSuitePathBytes`, `aptpath.DecodableClearsign`, `simtest.ClearsignedFixture`, `(*freshness.Checker).LockCount`. **Signed-seen marker on the miss path (review finding 5, its follow-up and the three-lens review, spec C3):** anchor rows (`InRelease`, `Release`) are created in the writer operation that stores the `url_path` row, before the body is written (sniff → `url_path` + `CreateSuite` + marker → write body → T1, today's seed-before-serve order (the T1 on a creating miss: phase D plan, design note 2; phase E plan, Task 10)); the marker for a sniff-passing `InRelease`/`Release.gpg` is written in that same operation and nowhere else in the handler — no hit-path write (except that every row creation, on a miss or on a hit, marks the suite's cached signed anchors in its own transaction: phase D plan, deviation 2, ruling PD-5), no wait, no coalescing map, no failure counter; a failed operation serves no body (502, as today). `Release.gpg` creates no row. **The backstop and its residual:** the one marker write outside a creating or storing transaction is the handler's `markCachedSignedAnchors`, which runs when a creation is unproven (`CreateResult.Unproven`: one of the suite's cached `InRelease`/`Release.gpg` rows changed, appeared or could not be read between the handler's pre-read and the creating transaction). It reads, sniffs and marks that anchor right after the commit and the late registration, before the response, bounded at 5 s. So an unproven creation's suite is tracked with that anchor unmarked from the commit until the backstop runs, and — if the backstop fails (WARN `suite_signed_seen_mark_failed` or `suite_signed_seen_backstop_read_failed`) — until the startup marker pass at the next start (spec C3 accepts it; E carry-forward below). Tests: the marker row exists before the first body byte on first contact and on a refresh miss; a failed writer operation serves no body; the hit-path invariant (after misses, adoptions, `reconstructAnchor` and restarts, every sniff-passing anchor row of a tracked suite has a marker (deviation 1)); FIFO ordering against a later unsigned commit. D also removes reconcile's plain-text fallback in `inReleasePlaintext` once `clearsignedFixture()` replaces the plain-text test fixtures (flip `TestReparseSnapshotRelease_PlainTextInReleaseStillParses` to `_PlainTextInReleaseIsAnError`).
 - E: `freshness` probe classification (`classPresent`, `classAbsent`, `classUnknown`), `Adopter.RunUnsigned`, `aptpath.AnchorPaths(form)` (spec C5). E's unsigned candidates set `Form: cache.FormUnsigned` explicitly, and its tests assert that an unsigned adoption stores `anchor_form = 'unsigned'` and vouches no `.deb`. E also resolves the two `AIDEV-TODO(phase E)` raw-SQL readers (url_path GC guard, `CommitAdoption` anchor seed: for `FormUnsigned`, Step 3c seeds `<suite>/Release`, not `/InRelease`). E's docs note the marker denial-of-service residual (spec C5 threat notes) at `adopt_unsigned`. **In-transaction never-downgrade (review finding 5, spec C5):** `CommitAdoption` re-reads the marker for `aptpath.MarkerBase(suite)` inside its writer transaction and refuses an unsigned candidate (no pointer change, no rows) when one exists or the prior current snapshot is signed; E's DoD pauses an eligible unsigned adoption before commit, serves a signed anchor and completes the marker write (also via the `/.` twin, once G exists — E adds the twin variant as a G DoD item), then resumes and asserts refusal. **Upgrade (review findings 2–3):** a phase-A-quarantined plain-text suite (fixture from A's `buildLegacyFixture` shape) adopts in the unsigned form through `Release` with no upstream change and no `unsigned_downgrade_refused`; a never-adopted suite whose signed anchor was served under a v6 binary is refused unsigned adoption on its first post-upgrade check (A's startup pass wrote its marker). As built: `freshness.classify(res, err, sniff, conditional) probeOutcome` (classes `classPresent`, `classAbsent`, `classUnknown`) and the probe tree `runProbeTree`; `(*Adopter).RunUnsigned(ctx, suite, release, etag, lastmod) error` and `(*Adopter).AdoptsUnsigned() bool`; `aptpath.AnchorPaths(form string) []string` and `(*cache.SuiteSnapshot).AnchorHash(name string) *string`; `cache.ErrUnsignedDowngradeRefused` (the refusal inside the commit) and `freshness.ErrAdoptionUnsignedDowngradeRefused`, `ErrAdoptionUnsignedTooLarge`; `(*freshness.Checker).AdoptionsInFlight(host) bool` and `WaitHostAdoptions(ctx, host, start) bool` (the `handler.AdoptionWaiter` the handler gets as `handler.Config.Adoptions`); `config.AdoptionConfig.AdoptUnsigned` and `freshness.AdoptionConfig.AdoptUnsigned`; the `freshness.Cache` method `ListSuitesWithAdoption` (in place of `ListSuites`); the flip barrier's code in `internal/freshness/flip_barrier.go`; the metrics `acu_repair_deferred_total{host}` and `acu_strict_adoption_waits_total{host}`; spec C5's `no_sha256` coverage rule (E fix round 1): the unexported `freshness.parsePackages` reports the stanzas with a `Filename` but no `SHA256` (`ParsePackages` keeps its signature), and `buildPackageHashes` makes such a snapshot's coverage incomplete (`package_coverage_incomplete`, reason `no_sha256`); the stanzas it reports count toward `MaxPackageStanzas`. G's flat parity row that cites it needs no new code for a file that mixes such stanzas with complete ones; a file in which no stanza has a SHA256 is G's open question (below). The `/.` twin variant of the in-commit race test is G's (below).
 - F: `cache.ReapIdleSuite`, admin `POST /forget`. Non-canonical legacy suite rows (`!aptpath.IsCanonicalSuitePath`) are reaped regardless of idleness (spec C9); the plan review found none in production. The reaper's refcount release reuses `decrementSnapshotRefcountsTx`; snapshot GC sub-job C also deletes the snapshots phase A's audit retired, and sub-job B's `keep_displaced` ranking excludes retired rows so they never take retention slots. The reaper sweeps a signed-seen marker only when no suite row exists for its base or `/.` twin and it is older than 365 days. F's DoD includes a signed snapshot adopted under the phase-A binary, reaped by the F binary, then returning: never-downgrade still refuses unsigned adoption (markers outlive reaping). As built: `cache.ReapIdleSuite(ctx, SuiteKey, wantCurrent *int64, cutoff int64) (deleted, gone bool, err error)` (a failed submit settled by a fresh read, and a settled reap logged `suite_reaped` with `settled=true` and counted; every suite-row delete — cap eviction, a reap, a forget — moves the base's markers up to `MAX(last_seen_at, now)`, ruling PF-10, and, before it retires the suite's snapshots, upserts its base's marker under the suite's scheme when the suite has an adopted, non-retired inline or detached snapshot: unexported `markSnapshotHistoryTx`, with never-downgrade's snapshot clause shared as `signedHistorySnapshotSQL`, the one marker a delete creates, ruling PF-12), `cache.ForgetSuite(ctx, SuiteKey, wantCurrent *int64) (ForgetResult{Deleted, Gone, MarkersDeleted, AnchorRowsDeleted, KeptBy}, error)` (with the markers it deletes the cached anchors of the base and its `/.` twin under both schemes, ruling PF-2), `cache.DeleteSuiteMetadata(ctx, SuiteKey) (int, error)` (spec C9 step 1; a key `aptpath.IsCanonicalSuitePath` refuses deletes nothing), `cache.ReapRegardless` (`deleteSuiteTx`'s cutoff for a delete regardless of idleness), `cache.ListUnadoptedSuiteAnchors` (`SuiteAnchorRow`), `cache.SweepOrphanMarkers(ctx, now, cutoff int64) (MarkerSweep{Swept, Furthest}, error)` (it never rewrites a stamp, ruling PF-1; the reaper passes a fresh `now` for `Furthest` and, for the cutoff, the run's clock less the forward steps it detected and 365 days, ruling PF-13), `(*cache.Cache).RunRetiredSnapshotGCChunk(ctx, maxRows int) (reaped int, more bool, err error)` (sub-job C), `(*cache.URLPath).LastUse() int64` (the stamp's one Go definition), `(*cache.Cache).HasSuiteRow` (renamed from `SuiteExists`); cap eviction moved to `internal/cache/suite_evict.go` and `deleteSuiteTx`, the one production `DELETE FROM suite_freshness`, to `suite_reap.go` beside two of its three callers, `ReapIdleSuite` and `ForgetSuite` (cap eviction's `createSuiteTx` stays in `suite_rows.go`; ruling R-F1). `cache.RetireSuiteSnapshots` was dropped with R-F4's knob-off retirement (ruling PF-5): phase E's PE-12 carry-forward is resolved by PF-5 — with `adoption.adopt_unsigned = false` an established unsigned suite keeps its coherent snapshot, refused `unsigned_disabled`, until the knob is back on or the suite is forgotten or reaped (forgotten or reaped with the knob still off, it is then a never-adopted suite), and the docs say plainly that the cache then does not keep unsigned repositories fresh (the maintainer confirmed it, 2026-10-20). `freshness.Reaper`, `NewReaper(ReaperConfig{Cache, Checker, IdleTTL, Logger})`, `(*Reaper).Run(ctx)` (started by `main` beside `Checker.Run`; at most 250 candidates reaped regardless of idleness first, then at most 25 idle ones in the same run — `regardlessReapsPerRun`, `reapsPerRun`, `runBudgets`, ruling PF-14 —, the junk scan every run, the marker sweep daily, the first a day after `Run` starts, a forward step of the wall clock deferring reaping and the sweep by the uptime floor and taken off the sweep's cutoff for the life of the process — `stepAhead`, ruling PF-13 —, a suite's held work bounded at 5 minutes, a panic under a suite's hold recovered for that suite alone; started through `main`'s `runReaper` seam) and `(*Reaper).Forget(ctx, scheme, host, suitePath) (cache.ForgetResult, error)` (a panic under its hold logged `freshness_check_panicked` with `where=forget`, released as the cache then answers, then re-panicked to net/http, whose `http: panic serving` line the admin server's `ErrorLog` routes through slog at ERROR; a forget whose writer call failed after its commit logged `suite_forgotten` with `settled=true`); `admin.Config.Forgetter` (`admin.Forgetter`; a forget in flight is cut short by the admin listener's `Shutdown` and answered 503, and a `host` holding `://` is refused with 400), `requireMutationGuard`, and the status JSON's additive `suites[].scheme` (the HTML Host cell shows `scheme://host`; ruling PF-7); `config.GCConfig.SuiteIdleTTL` (`gc.suite_idle_ttl`); `gc.LastRunSummary.RetiredReaped`, `acu_gc_retired_reaped_total` and `acu_suites_reaped_total{reason}`; the startup GC pass runs no sub-job C, the periodic ticks do (`gc.phaseStartup`, `runSnapshotPass`; ruling PF-15); `flipgate.(*Gate).Purge` keeps a live entry (ruling R-F2). Events `suite_reaped`, `suite_forgotten` (`anchor_rows`, `settled`), `admin_forget_requested`, `signed_seen_markers_swept`; `freshness_check_panicked` gains `where=reaper` (naming the suite when it struck under a suite's hold) and `where=forget`, and `clock_step_detected` the stamps `last_seen_at` and `wall_clock`; the reaper's clock-step reports are logged outside the checker's one-line-a-minute limit; the reaper's busy line, held-step WARNs and junk-read WARN, and every `freshness_check_panicked` and `adoption_panicked` line, and the handler's `handler_goroutine_panicked` `where=touch` line, carry `canonical_scheme`; the startup line gains `gc_suite_idle_ttl` and `adoption_adopt_unsigned`. Rulings R-F1–R-F5 and PF-1–PF-15 and deviations 1 (withdrawn: it concerned the dropped retirement), 2 (the coarser 90-day simulation, a four-day one at the production cadence beside it) and 3 (the sweep never rewrites a future stamp) are in the phase F plan and spec revision 9.
-- G: `aptpath.IsFlatSuitePath`, `aptpath.FlatBase`, `aptpath.DotClean`, `freshness.validateFilename`, and the admin `suites[].flat` field (phase B plan, deviation 3). Legacy flat onboarding also writes signed-seen markers for sniff-passing cached flat anchors (the startup pass only covers suites that already have rows), and the `/.` twin variant of E's in-transaction recheck test runs here. **PR #8 evidence in G's DoD:** `TestServeHTTP_FlatRepositoryFreshness` (a miss on `/…/deb/InRelease` and on `/…/deb/./InRelease` creates one suite row each and fires T1 under it; a later hit fires T1 again; `/…/deb/Packages_1.0_all.deb`, host-root `/InRelease` and `/…/deb//InRelease` create none), the pkgs.k8s.io shape `/core:/stable:/v1.34/deb/InRelease` → `/core:/stable:/v1.34/deb`, flat `Packages` failure fatal, and the mixed-host strict matrix. G also ports the rest of PR #8's tests (see "PR #8 parity" below). As built: `aptpath.IsFlatSuitePath`, `FlatBase`, `DotClean` and `IsFlatHostRootPath` (the flat grammar in `aptpath.SuitePath`), `aptpath.IsAnchorlessIndex` and `SniffAnchorlessIndex`; `cache.PackageHash.FlatFilename` (written by `CommitAdoption` and reconcile), `DeclaredHashesForPath`'s suffix fallback over `idx_package_hash_flat_filename` (at most 32 segments, the longest ancestor-or-self base), `cache.ListSuiteMetadataRows`, `RefreshURLPathBlob`, `DeleteURLPathRows` and `ListUnadoptedFlatIndexRows`; `freshness.validateFilename`, the layout-aware `isIndexTarget`, `indexTargetGroup`, `archFromFilteredPath`, `missingRequestableIndexGroups` and `missingRequestableMembers` (each takes the suite path), `flatIndexClientCreated` (Dispatch 3's flat index row, found among the metadata rows `checkFirst` reads once before the probe tree: ruling PG-32), `(*Adopter).anchorlessEligible` and `(*Checker).anchorlessRefresh` (Dispatch 3–4; results `anchorless_refreshed`/`anchorless_unchanged`, form `anchorless`; `acu_anchorless_sniff_rejected_total{host}`), the reaper's flat index junk evidence — a flat suite with no signed evidence is judged on its anchor and root index rows together, a flat suite with signed evidence on its anchors alone (ruling R-G2 as amended by PG-7: `(*Reaper).suiteJunk`, the signed-evidence read lazy, per candidate) — with the index rows read only when a run has an unadopted flat suite (`hasUnadoptedFlatSuite`); with `adoption.adopt_unsigned` off, a suite the anchorless gate admits is refused `unsigned_disabled`, a success logged `changed_deferred` with form `anchorless` (`(*Checker).anchorlessDisabled`, ruling PG-1); at most two anchorless bodies in memory for the process (`anchorlessBodySlots`, ruling PG-8), and an anchorless check that asks for files is a success only with a content answer (design review M-7); `proxy.IsMetadata` never classifies a Debian pool artifact as metadata (`isPoolArtifact` in `internal/proxy/classify.go`, ruling PG-2); the handler's layout-aware `isIndexTargetPath` and `indexTargetArch`, `flat_host_root_unsupported` (one WARN a minute, only for a host the allowlist admits) and `urlPathModTime` (`Last-Modified = max(pool mtime, last_fetched_at)` at url_path metadata serves); the admin `suites[].flat`. From the fix round after the task reviews: `observability.LogLimiter` (`Allow(now, every time.Duration) (ok bool, suppressed int)`; its zero value is ready, and the interval is each call site's constant), the one copy of the one-line-a-minute limiter, used by `suite_seed_rejected`, `flat_host_root_unsupported` and the anchorless over-cap WARN (ruling PG-22); `aptpath.FlatIndexNames()`, the 14 root `Packages*`/`Sources*` names and the one list `cache.ListSuiteMetadataRows` and `ListUnadoptedFlatIndexRows` probe by; `flatIndexClientCreated` returns the client-created index row, and a flat suite active only through it, with no cached anchor row, probes its `InRelease` and `Release` at sibling URLs of that row's stored `upstream_url` (ruling PG-20); `(*Checker).cachedAnchors` returns the anchor rows, read before the probe tree, and the anchorless refresh deletes a cached `InRelease` or `Release` only while it holds the blob read then (`anchorURLMap` gives `anchorURLsFor` their URLs); a failed `DeleteURLPathRows` is no answer — its 404s and 410s stop counting, so a check they alone answered fails — and a failed read of a flat suite's metadata rows — once, before the host slot and the probe tree — is a local fault (WARN `freshness: read anchor rows failed`, no request, nothing written, no backoff) (rulings PG-24, PG-32); the reaper's `unadoptedFlat`, the one predicate its index-read gate and its per-suite test share. From the final fix wave: the anchorless refresh deletes a cached `InRelease` or `Release` whose probe answered 401 or 403 beside an index request's content answer in the same check (ruling PG-28); an index whose 200 is over the cap is dropped for the miss path (`anchorlessDropped`; its WARN, renamed `freshness: anchorless index over its size cap, dropped for the miss path`, names the file) and `fetch.Conditional` fails a body whose `Content-Length` exceeds its cap without reading it (ruling PG-29); `aptpath.MaxFilenameBytes` (4,096: `validateFilename`'s cap and the fallback's name bound), `maxFallbackPathBytes` and the bounded, substring-built `flatSplits` (ruling PG-30); `Cache.flatRows`, primed at `Open` by `flatRowsExistSQL` (a failed read fails `Open`: ruling PG-36) and set by `noteFlatRows` before `CommitAdoption` or `InsertReconciledMembers` submits a flat row, so `DeclaredHashesForPath` skips the fallback while it is false (ruling PG-31); `anchorlessRefresh` takes the metadata rows `checkFirst` read (ruling PG-32); `anchorlessFailure` (the failed check's decisive reason first, the other after `; also `); `rows_replaced`/`rows_deleted` on the anchorless check line (`checkEnd`); `suite_path` in `package_hash_conflict`'s `declared` entries (`declaredAttrs`, ruling PG-33); `canonical_scheme` on phase G's new WARNs; the tests-only `Checker.anchorlessStoredHook`. Rulings R-G1–R-G5 (R-G2 as amended by PG-7) and PG-1–PG-40 (PG-19–PG-24 on the task reviews; PG-25–PG-27 the fix round's deferred IDs, its one skip and the spec revision; PG-28–PG-33 the final review's; PG-34–PG-40 its fix waves') and deviations 1 (the anchorless gate reads signed evidence) and 2 (a changed plain `Contents-*` is deleted) are in the phase G plan, and the behaviour they settle in spec revision 10; the items phase G leaves are in `…-G-deferred.md` (GD-1–GD-6).
+- G: `aptpath.IsFlatSuitePath`, `aptpath.FlatBase`, `aptpath.DotClean`, `freshness.validateFilename`, and the admin `suites[].flat` field (phase B plan, deviation 3). Legacy flat onboarding also writes signed-seen markers for sniff-passing cached flat anchors (the startup pass only covers suites that already have rows), and the `/.` twin variant of E's in-transaction recheck test runs here. **PR #8 evidence in G's DoD:** `TestServeHTTP_FlatRepositoryFreshness` (a miss on `/…/deb/InRelease` and on `/…/deb/./InRelease` creates one suite row each and fires T1 under it; a later hit fires T1 again; `/…/deb/Packages_1.0_all.deb`, host-root `/InRelease` and `/…/deb//InRelease` create none), the pkgs.k8s.io shape `/core:/stable:/v1.34/deb/InRelease` → `/core:/stable:/v1.34/deb`, flat `Packages` failure fatal, and the mixed-host strict matrix. G also ports the rest of PR #8's tests (see "PR #8 parity" below). As built: `aptpath.IsFlatSuitePath`, `FlatBase`, `DotClean` and `IsFlatHostRootPath` (the flat grammar in `aptpath.SuitePath`), `aptpath.IsAnchorlessIndex` and `SniffAnchorlessIndex`; `cache.PackageHash.FlatFilename` (written by `CommitAdoption` and reconcile), `DeclaredHashesForPath`'s suffix fallback over `idx_package_hash_flat_filename` (at most 32 segments, the longest ancestor-or-self base), `cache.ListSuiteMetadataRows`, `RefreshURLPathBlob`, `DeleteURLPathRows` and `ListUnadoptedFlatIndexRows`; `freshness.validateFilename`, the layout-aware `isIndexTarget`, `indexTargetGroup`, `archFromFilteredPath`, `missingRequestableIndexGroups` and `missingRequestableMembers` (each takes the suite path), `flatIndexClientCreated` (Dispatch 3's flat index row, found among the metadata rows `checkFirst` reads once before the probe tree: ruling PG-32), `(*Adopter).anchorlessEligible` and `(*Checker).anchorlessRefresh` (Dispatch 3–4; results `anchorless_refreshed`/`anchorless_unchanged`, form `anchorless`; `acu_anchorless_sniff_rejected_total{host}`), the reaper's flat index junk evidence — a flat suite with no signed evidence is judged on its anchor and root index rows together, a flat suite with signed evidence on its anchors alone (ruling R-G2 as amended by PG-7: `(*Reaper).suiteJunk`, the signed-evidence read lazy, per candidate) — with the index rows read only when a run has an unadopted flat suite (`hasUnadoptedFlatSuite`); with `adoption.adopt_unsigned` off, a suite the anchorless gate admits is refused `unsigned_disabled`, a success logged `changed_deferred` with form `anchorless` (`(*Checker).anchorlessDisabled`, ruling PG-1); at most two anchorless bodies in memory for the process (`anchorlessBodySlots`, ruling PG-8), and an anchorless check that asks for files is a success only with a content answer (design review M-7); `proxy.IsMetadata` never classifies a Debian pool artifact as metadata (`isPoolArtifact` in `internal/proxy/classify.go`, ruling PG-2); the handler's layout-aware `isIndexTargetPath` and `indexTargetArch`, `flat_host_root_unsupported` (one WARN a minute, only for a host the allowlist admits) and `urlPathModTime` (`Last-Modified = max(pool mtime, last_fetched_at)` at url_path metadata serves); the admin `suites[].flat`. From the fix round after the task reviews: `observability.LogLimiter` (`Allow(now, every time.Duration) (ok bool, suppressed int)`; its zero value is ready, and the interval is each call site's constant), the one copy of the one-line-a-minute limiter, used by `suite_seed_rejected`, `flat_host_root_unsupported` and the anchorless over-cap WARN (ruling PG-22); `aptpath.FlatIndexNames()`, the 14 root `Packages*`/`Sources*` names and the one list `cache.ListSuiteMetadataRows` and `ListUnadoptedFlatIndexRows` probe by; `flatIndexClientCreated` returns the client-created index row, and a flat suite active only through it, with no cached anchor row, probes its `InRelease` and `Release` at sibling URLs of that row's stored `upstream_url` (ruling PG-20); `(*Checker).cachedAnchors` returns the anchor rows, read before the probe tree, and the anchorless refresh deletes a cached `InRelease` or `Release` only while it holds the blob read then (`anchorURLMap` gives `anchorURLsFor` their URLs); a failed `DeleteURLPathRows` is no answer — its 404s and 410s stop counting, so a check they alone answered fails — and a failed read of a flat suite's metadata rows — once, before the host slot and the probe tree — is a local fault (WARN `freshness_read_anchor_rows_failed`, no request, nothing written, no backoff) (rulings PG-24, PG-32); the reaper's `unadoptedFlat`, the one predicate its index-read gate and its per-suite test share. From the final fix wave: the anchorless refresh deletes a cached `InRelease` or `Release` whose probe answered 401 or 403 beside an index request's content answer in the same check (ruling PG-28); an index whose 200 is over the cap is dropped for the miss path (`anchorlessDropped`; its WARN, now `freshness_anchorless_index_over_cap`, names the file) and `fetch.Conditional` fails a body whose `Content-Length` exceeds its cap without reading it (ruling PG-29); `aptpath.MaxFilenameBytes` (4,096: `validateFilename`'s cap and the fallback's name bound), `maxFallbackPathBytes` and the bounded, substring-built `flatSplits` (ruling PG-30); `Cache.flatRows`, primed at `Open` by `flatRowsExistSQL` (a failed read fails `Open`: ruling PG-36) and set by `noteFlatRows` before `CommitAdoption` or `InsertReconciledMembers` submits a flat row, so `DeclaredHashesForPath` skips the fallback while it is false (ruling PG-31); `anchorlessRefresh` takes the metadata rows `checkFirst` read (ruling PG-32); `anchorlessFailure` (the failed check's decisive reason first, the other after `; also `); `rows_replaced`/`rows_deleted` on the anchorless check line (`checkEnd`); `suite_path` in `package_hash_conflict`'s `declared` entries (`declaredAttrs`, ruling PG-33); `canonical_scheme` on phase G's new WARNs; the tests-only `Checker.anchorlessStoredHook`. Rulings R-G1–R-G5 (R-G2 as amended by PG-7) and PG-1–PG-40 (PG-19–PG-24 on the task reviews; PG-25–PG-27 the fix round's deferred IDs, its one skip and the spec revision; PG-28–PG-33 the final review's; PG-34–PG-40 its fix waves') and deviations 1 (the anchorless gate reads signed evidence) and 2 (a changed plain `Contents-*` is deleted) are in the phase G plan, and the behaviour they settle in spec revision 10; the items phase G leaves are in `…-G-deferred.md` (GD-1–GD-6).
+- H1: the admin JSON's `suites[].anchorless`, `recent_adoptions[].scheme` and `gc.retired_reaped`; `cache.LegacyAuditDecision.DropAnchorRows` and `cache.LegacyAuditEffect` (R-H2); the events `suite_reaper_run_complete`, `suite_not_forgotten`, `gc_panicked`, `reconcile_snapshot_read_failed`, `source_paths_skipped`, `legacy_anchor_discard_failed` and the 43 snake_case names of rulings R-H3 and R-H6, and the guard tests that keep every new log message snake_case and documented (`internal/observability/event_names_test.go`, `event_docs_test.go`); the gauge `acu_suite_reaper_last_run_unixtime`; `freshness.CheckBackoff` and `admin.Config.CheckBackoff`; `simtest.Unavailable`.
 - H: PR #8's end-to-end table in `e2e/flat`: one signed flat repository configured as `deb <uri>/ /` on one client and `deb <uri> ./` on another; both report 2.0 within one cooldown of the v2 publish, and with the upstream stopped two fresh clients install 2.0 from the snapshots. H also runs the live-upstream smoke and documents the strict-mode consequences listed under "PR #8 parity" below.
 
 ## Carry-forwards (from phase A and B reviews)
@@ -103,23 +106,25 @@
 
 **H:**
 
-- *(E)* The prose WARN names phase E adds — `freshness: read anchor rows failed`, `freshness: read current snapshot failed`, `adoption: served anchor read failed; the flip barrier runs` and `adoption: signed-anchor re-mark failed` (documented as they are) — may become snake_case in H's documentation sweep, with phase D's (listed under "PR #8 parity").
-- *(E)* `README.md` (spec §8): unsigned and SHA512-only repositories, eager first adoption and the `adopt_unsigned` threat notes, beside the flat forms H documents.
-- *(F)* `README.md` (spec §8, "GC and `/forget`"): suite reaping and `POST /forget` beside *Recovering a degraded repository*, and `/forget` in the sentence that names the admin interface's `POST /reconcile`; `docs/configuration.md` documents both (*Suite reaping* under `[gc]`, *Forgetting a suite* under `[admin]`).
-- *(F)* The admin status page and JSON show no reaping: `gc.LastRunSummary.RetiredReaped` (spec C11) is not copied into the admin GC section, which copies its fields one by one (`internal/admin/status.go`); add `retired_reaped` beside `displaced_reaped` in the JSON, the GC panel and `docs/admin-ui-spec.md` (phase F plan, design note 15).
-- *(F)* The 1.1.0 release notes tell offline and archive caches — and any install that relied on `gc.enabled = false` to keep everything — to set `gc.suite_idle_ttl = "0s"` before upgrading: the suite reaper runs whatever `gc.enabled` says and, from an hour after the first start, deletes the suites no client asked for in `gc.suite_idle_ttl` (720 h by default, counted from the last request before the upgrade, so a long-idle suite goes at once), with their cached index files (`docs/configuration.md`, *Upgrading an existing cache* and *Suite reaping*, says so; the Task 11 review).
-- *(F)* The prose WARN names phase F adds — the seven `suite reaper: …` lines and `admin: forget failed` (documented as they are) — may become snake_case in H's documentation sweep, with phase D's and E's.
+- *(D)* The three prose WARN names `suite onboarding failed`, `suite signed-seen mark failed` and `suite cap usage read failed` (documented as they are) may become snake_case in H's documentation sweep. (Moved here from "PR #8 parity" by H1: deferred minor D-55.) *Done in H1 (Task 3, ruling R-H3).*
+- *(E)* The prose WARN names phase E adds — `freshness: read anchor rows failed`, `freshness: read current snapshot failed`, `adoption: served anchor read failed; the flip barrier runs` and `adoption: signed-anchor re-mark failed` (documented as they are) — may become snake_case in H's documentation sweep, with phase D's (above). *Done in H1 (Task 3, ruling R-H3).*
+- *(E)* `README.md` (spec §8): unsigned and SHA512-only repositories, eager first adoption and the `adopt_unsigned` threat notes, beside the flat forms H documents. *H3's.*
+- *(F)* `README.md` (spec §8, "GC and `/forget`"): suite reaping and `POST /forget` beside *Recovering a degraded repository*, and `/forget` in the sentence that names the admin interface's `POST /reconcile`; `docs/configuration.md` documents both (*Suite reaping* under `[gc]`, *Forgetting a suite* under `[admin]`). *H3's.*
+- *(F)* The admin status page and JSON show no reaping: `gc.LastRunSummary.RetiredReaped` (spec C11) is not copied into the admin GC section, which copies its fields one by one (`internal/admin/status.go`); add `retired_reaped` beside `displaced_reaped` in the JSON, the GC panel and `docs/admin-ui-spec.md` (phase F plan, design note 15). *Done in H1 (Task 4; `docs/admin-ui-spec.md` §4.6, Task 18).*
+- *(F)* The 1.1.0 release notes tell offline and archive caches — and any install that relied on `gc.enabled = false` to keep everything — to set `gc.suite_idle_ttl = "0s"` before upgrading: the suite reaper runs whatever `gc.enabled` says and, from an hour after the first start, deletes the suites no client asked for in `gc.suite_idle_ttl` (720 h by default, counted from the last request before the upgrade, so a long-idle suite goes at once), with their cached index files (`docs/configuration.md`, *Upgrading an existing cache* and *Suite reaping*, says so; the Task 11 review). *H3's.*
+- *(F)* The prose WARN names phase F adds — the seven `suite reaper: …` lines and `admin: forget failed` (documented as they are) — may become snake_case in H's documentation sweep, with phase D's and E's. *Done in H1 (Task 3, ruling R-H3).*
 
-- *(G)* `README.md` (spec §8): flat repositories — both source forms, the host-root limitation, Release-less repositories refreshed without a `Release` (`docs/configuration.md`, *Flat repositories*) — beside H's other README items.
+- *(G)* `README.md` (spec §8): flat repositories — both source forms, the host-root limitation, Release-less repositories refreshed without a `Release` (`docs/configuration.md`, *Flat repositories*) — beside H's other README items. *H3's.*
 - *(G)* `e2e/flat` gains a Release-less repository (a Gemfury-shaped `Packages.gz` with no `Release`): a republished index reaches a client within one `periodic_refresh`, and an upstream that then answers HTML keeps the old index.
-- *(G)* The status page cannot tell a flat repository refreshed without a `Release` from a dormant, never-adopted suite: after an answered anchorless check it has no snapshot and no record, so only `last_success` differs. Show the anchorless refresh in `suites[]` (a field or tag) and on the status page (phase G design review M-14, ruling PG-14).
-- *(G)* The prose WARN names phase G adds — `freshness: anchorless index over its size cap, dropped for the miss path`, `freshness: delete anchorless rows failed`, `suite reaper: list unadopted flat indexes failed` and `suite reaper: read signed evidence failed` (documented as they are) — may become snake_case in H's documentation sweep, with phase D's, E's and F's.
-- *(G)* `docs/log-fields.md` has never documented the adoption event `adoption_member_skipped`, to which phase G (E-9) added `declared_sha512`, nor `snapshot_index_target_404`, whose architecture label phase G made layout-aware (`all`, `source` for a flat suite): document both in H's observability sweep (phase G Task 16 review, Minor 8).
-- *(G-1)* The 1.1.0 release notes say that `POST /reconcile` now needs the header `X-ACU-Admin: 1`, as `POST /forget` does, so a script that calls it must add it, and that without `admin.htpasswd_file` both endpoints refuse (403) a `Host` that is not an IP address, `localhost` or `admin.listen`'s host name, a guard against DNS rebinding: an operator behind a reverse proxy that forwards the client's `Host` sets `htpasswd_file` or has the proxy send `Host: 127.0.0.1` (the maintainer's decision, 2026-10-24).
+- *(G)* The status page cannot tell a flat repository refreshed without a `Release` from a dormant, never-adopted suite: after an answered anchorless check it has no snapshot and no record, so only `last_success` differs. Show the anchorless refresh in `suites[]` (a field or tag) and on the status page (phase G design review M-14, ruling PG-14). *Done in H1 (Task 4: `suites[].anchorless`, "refreshed without a Release"; H1 design note 5).*
+- *(G)* The prose WARN names phase G adds — `freshness: anchorless index over its size cap, dropped for the miss path`, `freshness: delete anchorless rows failed`, `suite reaper: list unadopted flat indexes failed` and `suite reaper: read signed evidence failed` (documented as they are) — may become snake_case in H's documentation sweep, with phase D's, E's and F's. *Done in H1 (Task 3, ruling R-H3).*
+- *(G)* `docs/log-fields.md` has never documented the adoption event `adoption_member_skipped`, to which phase G (E-9) added `declared_sha512`, nor `snapshot_index_target_404`, whose architecture label phase G made layout-aware (`all`, `source` for a flat suite): document both in H's observability sweep (phase G Task 16 review, Minor 8). *Done in H1 (Task 18), with `admin_reconcile_requested`.*
+- *(G-1)* The 1.1.0 release notes say that `POST /reconcile` now needs the header `X-ACU-Admin: 1`, as `POST /forget` does, so a script that calls it must add it, and that without `admin.htpasswd_file` both endpoints refuse (403) a `Host` that is not an IP address, `localhost` or `admin.listen`'s host name, a guard against DNS rebinding: an operator behind a reverse proxy that forwards the client's `Host` sets `htpasswd_file` or has the proxy send `Host: 127.0.0.1` (the maintainer's decision, 2026-10-24). *H3's.*
 
-**Open decision (spec-level, for the maintainer):**
+**Decisions (spec-level; none open):**
 
-- An `anchor_corrupt` or `anchor_oversized` quarantine leaves the known-corrupt `url_path` anchor in Phase-1 serving, and the checker sees "unchanged". This is pre-existing behaviour for any bit-rotted metadata blob. Decide in a later phase whether the audit should also invalidate that `url_path` row.
+- *Decided for 1.1.0 by ruling R-H5 (phase H1), for the maintainer's confirmation: keep the behaviour; revisit if seen in the field.* An adopted unsigned flat suite whose upstream stops publishing its `Release` keeps its coherent snapshot — "refused: anchor_absent" on the status page, `adoption_refused_persistently` in the log — until `POST /forget` turns it into a Release-less suite (*Flat repositories*). It is rare in the field; the kept snapshot keeps serving cached content when an upstream dies outright, where an automatic drop would turn that into 404s; a drop would add a state transition with trust and serving consequences late in the release (deferred minor GD-5; spec §10 item 12).
+- *Decided by the maintainer, 2026-10-24, and done in H1 (Task 2, ruling R-H2):* an `anchor_corrupt` quarantine deletes, in its own transaction, the `url_path` rows that hold the corrupt `InRelease` blob, so clients fetch a fresh copy; `anchor_oversized` (not proven bad) keeps them (spec §6.1, revision 12). The entry as it stood: an `anchor_corrupt` or `anchor_oversized` quarantine leaves the known-corrupt `url_path` anchor in Phase-1 serving, and the checker sees "unchanged". This is pre-existing behaviour for any bit-rotted metadata blob. Decide in a later phase whether the audit should also invalidate that `url_path` row.
 - *Decided by the maintainer, 2026-10-24, and done before phase H:* Host-header validation of the admin listener's mutating endpoints (spec §10 G-1; phase F ruling PF-6). `POST /reconcile` takes `/forget`'s guard (`requireMutationGuard`: `X-ACU-Admin: 1` and `Sec-Fetch-Site`), and without `htpasswd_file` both refuse a `Host` that is not an IP address, `localhost` or `admin.listen`'s host name; the read-only endpoints keep no `Host` check. H's release notes carry it (above).
 
 ## PR #8 parity (gap analysis, 2026-10-17)
@@ -157,9 +162,8 @@
 
 **H:**
 
-- **Live-upstream smoke (manual, before the release that ships G; not CI).** PR #8's author verified against live pkgs.k8s.io (v1.32–v1.37, `kubectl` 1.34.11 installed through the cache); our e2e uses fixtures only. With adoption enabled and `integrity.refuse_unvouched_debs = true`, front live pkgs.k8s.io (two minors, one client per source form), NVIDIA CUDA, libnvidia-container, pkg.jenkins.io and download.sublimetext.com. For each: `adoption_success` logged; `apt-get update` output clean under the e2e grep; one package installs; zero `unvouched_deb_refused`. Record the results in the release notes.
-- **Docs (`docs/configuration.md`, strict mode):** a flat snapshot with incomplete coverage turns strict mode off for its whole host, as an incomplete `dists/` snapshot does; on a strict host, the first install from a newly added flat suite can be refused while its first adoption is queued; strict mode now refuses undeclared `.deb`s on flat-only hosts.
-- *(D)* The three prose WARN names `suite onboarding failed`, `suite signed-seen mark failed` and `suite cap usage read failed` (documented as they are) may become snake_case in H's documentation sweep.
+- **Live-upstream smoke (manual, before the release that ships G; not CI).** PR #8's author verified against live pkgs.k8s.io (v1.32–v1.37, `kubectl` 1.34.11 installed through the cache); our e2e uses fixtures only. With adoption enabled and `integrity.refuse_unvouched_debs = true`, front live pkgs.k8s.io (two minors, one client per source form), NVIDIA CUDA, libnvidia-container, pkg.jenkins.io and download.sublimetext.com. For each: `adoption_success` logged; `apt-get update` output clean under the e2e grep; one package installs; zero `unvouched_deb_refused`. Record the results in the release notes. *H3's.*
+- **Docs (`docs/configuration.md`, strict mode):** a flat snapshot with incomplete coverage turns strict mode off for its whole host, as an incomplete `dists/` snapshot does; on a strict host, the first install from a newly added flat suite can be refused while its first adoption is queued; strict mode now refuses undeclared `.deb`s on flat-only hosts. *H3's.*
 
 ## Review findings 2026-10-05 → where they are addressed
 
````

- [ ] **Step 8: Two comments that name a deferred file**

Design note 16 keeps the comments that cite an item by ID; these two point at a file this task deletes and say something it no longer holds.

`internal/handler/wallclock_stamps_test.go` (the GC's half of A-3 was closed in phase F by `TestGC_StampsFollowTheWallClock`):

old:
```go
// not. (The GC's stamps stay listed in the deferred file.)
```

new:
```go
// not. (The GC's half: TestGC_StampsFollowTheWallClock, internal/gc.)
```

`internal/freshness/anchorless.go`, the AIDEV-NOTE at the body-slot wait in `refreshAnchorlessRow` (GD-1, declined):

old:
```go
	// for a host slot; bounded, and failed checks back off. Follow-up for
	// phase H (the phase G deferred file): a short bounded wait for a body
	// slot, or a per-request timeout well under checkDeadline. The wait
```

new:
```go
	// for a host slot; bounded, and failed checks back off. A short
	// bounded wait for a body slot, or a per-request timeout well under
	// checkDeadline, was declined in phase H1 (deferred minor GD-1): it
	// would make anchorless checks fail under contention and back off — a
	// new failure mode for a rare idle slot. The wait
```

- [ ] **Step 9: Delete the seven deferred files**

```bash
git rm docs/superpowers/plans/2026-09-28-flat-repositories-A-deferred.md docs/superpowers/plans/2026-09-28-flat-repositories-B-deferred.md docs/superpowers/plans/2026-09-28-flat-repositories-C-deferred.md docs/superpowers/plans/2026-09-28-flat-repositories-D-deferred.md docs/superpowers/plans/2026-09-28-flat-repositories-E-deferred.md docs/superpowers/plans/2026-09-28-flat-repositories-F-deferred.md docs/superpowers/plans/2026-09-28-flat-repositories-G-deferred.md
```

- [ ] **Step 10: Verify**

Run: `go test -count=1 ./internal/observability/`
Expected: `ok` — the docs test and Task 3's two guards (`TestEveryEventIsDocumented` subsumes the review's per-event grep: every event the plan added has its row).

Run: `go test -count=1 ./internal/config/ ./cmd/...`
Expected: both `ok` (the conffile still parses and validates: Step 5 changed comments only).

Run: `go build ./... && go vet ./internal/freshness/ ./internal/handler/`
Expected: no output.

Run: `ls docs/superpowers/plans/ | grep -c -- '-deferred\.md'; grep -rn -e '-deferred\.md' -e 'deferred file' --include='*.go' .`
Expected: `0`, and no grep output (the phase plans keep their historical references to the deleted files; no Go file names one).

Run: `git diff --check`
Expected: no output.

Mutation checks (one at a time, each reverted):

| Mutant | Fails |
|---|---|
| `legacy_anchor_discard_failed`'s row loses its first cell's name (its mention in `legacy_snapshot_quarantined`'s row stays) | `event_docs_test.go:117: internal/freshness/legacy_audit.go:124: event legacy_anchor_discard_failed has no row in docs/log-fields.md; add one` |
| One of `main`'s listed events given a row (`admin_disabled` beside `legacy_trust_audit_deferred`) | `event_docs_test.go:119: admin_disabled has a docs/log-fields.md row now: remove it from mainEventsWithoutARow` |
| A stale entry in `mainEventsWithoutARow` (the test file: its lines shift down by one) | `event_docs_test.go:125: no_such_event is no longer logged: remove it from mainEventsWithoutARow` |

Run Task 17's Steps 2–4 again.
Expected: as there: every package `ok`, `0 issues.`, the five drift files. Replay: 21 packages `ok`, `0 issues.`, the five drift files.

- [ ] **Step 11: Commit**

```bash
gofmt -l internal/handler/wallclock_stamps_test.go internal/freshness/anchorless.go internal/observability/event_docs_test.go
git add internal/observability/event_docs_test.go docs/configuration.md docs/log-fields.md docs/admin-ui-spec.md packaging/config/config.toml.default docs/superpowers/specs/2026-09-27-flat-repositories-design.md docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md internal/handler/wallclock_stamps_test.go internal/freshness/anchorless.go
git commit -m "$(cat <<'EOF'
Document phase H1; spec revision 12; overview; delete the deferred files

log-fields, configuration, the admin UI spec and the conffile document
H1's changes: the corrupt-anchor rows and pool file the audit removes
(R-H2), the audit-queued snapshots sub-job B keeps (R-H1), the reaper's
run line and gauge, the new events and fields, the status page's new
fields and texts, /forget's 400 and 409 line, and the log-fields gaps
(adoption_member_skipped, snapshot_index_target_404,
admin_reconcile_requested, adoption_repair_declarations_unavailable). A
guard test requires a log-fields row for every snake_case event, main's
87 without one listed. Spec revision 12 records them; the overview marks
H1's carry-forwards and decisions; every deferred minor has its
disposition in the H1 plan, so the seven deferred files are deleted
(C-36, C-37, D-52, D-55, E-24; GD-5 decided for the maintainer's
confirmation: keep, R-H5).

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

The `gofmt -l` prints nothing; `git rm` in Step 9 staged the deletions.
