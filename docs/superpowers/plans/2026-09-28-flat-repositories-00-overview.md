# Flat repositories and suite lifecycle — Implementation Plan (overview)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make flat apt repositories first-class (refresh, adoption, strict-mode vouching), support unsigned, Release-less and SHA512-only repositories, and fix limitations #1–#3 (adoption retry loops, flip race, immortal suites) with bounded long-term behaviour.

**Architecture:** Eight phases (A–H), each leaving shippable, green state. Safety nets land before the features that need them: foundations → retry discipline and capacity → flip barrier → creation bounds → eager adoption and anchor forms → reaping → flat repositories → real-apt e2e and docs. Each phase has its own detailed plan, written after the previous phase lands (later phases edit code the earlier ones produce, so their exact code cannot be written reliably up front).

**Tech Stack:** Go 1.25 (toolchain 1.26), modernc SQLite, `github.com/ProtonMail/go-crypto`, `testing/synctest`, Docker for e2e.

**Spec:** `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (revision 4). Plan reviews: 2026-09-28 (phase A executability and tests) and `2026-10-05-flat-repositories-plan-review.md` (upgrade trust and later-phase contracts; its findings map to the "Review findings 2026-10-05" table below). Every task implements a named spec section; when this plan and the spec disagree, the spec wins unless the plan records a deliberate, explained deviation (listed below).

## Global Constraints

- Never edit an existing entry of `migrations` in `internal/cache/schema.go`. Phase A adds v7, which is the complete schema for every phase (spec §6). Once v7 is committed, a development or test cache may already be at v7, so v7 is never edited either: a later phase that needs another schema change adds v8.
- Production SQL never uses SQLite clock functions (`strftime`, `CURRENT_TIMESTAMP`, `julianday`, `unixepoch`); persisted stamps come from `wallclock.Now()`.
- Every persisted Unix stamp comes from `internal/wallclock`; durations, deadlines and in-memory timing use monotonic `time.Now`/`time.Since`. `Checker.now` and `Adopter.now` are wall clocks: never measure a duration with them.
- Tests that call `wallclock.SetOffsetForTest` never use `t.Parallel()` (the offset is process-global).
- Simulations (synctest bubbles, `internal/simtest`) cost about 3.5 ms per freshness check and 1 ms per scheduler tick under `-race`. Size each one to finish in under 5 s under `-race` by choosing coarser intervals, never by dropping assertions, and keep each package under 5 min under `-race` (CI runs `go test -race -timeout 10m ./...`).
- `internal/cache` must not import `internal/proxy`; shared path logic lives in the leaf package `internal/aptpath` (standard library only).
- Adoption-disabled (Phase 1) behaviour stays as today: with no adopter, nothing refreshes and the seed is today's seed.
- New code that can block does so on channels/timers, never a `sync.Mutex` held across a wait.
- Constants from spec §C11: backoff 2 m → 1 h (adoption failures and repeated *unknown* checks); hold 20 s; phase-A cap 60 s; hold-token wait 60 s; waiter caps 256 (barrier) and 64 (strict wait); strict wait 15 s; reaps 25 per run; unsigned member cap 1,000; check deadline 2 min; check workers 4; interest window 7 d; orphan-marker TTL 365 d; cap-rejection cache 10 min; legacy-audit anchor read cap 8 MiB and batch 100.
- Config keys and defaults: `adoption.adopt_unsigned = true`; `adoption.flip_quiet_period = "10s"` (0s disables, ≤ 15s); `adoption.max_duration = "30m"` (≥ 1m); `freshness.max_suites = 5000` (≥ 10; per-host = max_suites/10); `gc.suite_idle_ttl = "720h"` (0s disables; else ≥ 24h).
- Every changed invariant gets an `AIDEV-NOTE`; existing `AIDEV-*` comments are kept and updated, never removed.
- `go test -race ./...` and `golangci-lint run ./...` are clean, and `gofmt -l .` lists no files beyond the five already listed on `main` (formatter drift we do not touch: `cmd/apt-cacher-ultra/ca_print.go`, `internal/cache/cache_test.go`, `internal/cache/gc_urlpath_test.go`, `internal/freshness/hot_prefetch_test.go`, `internal/proxy/connect_stats.go`), at the end of every task that touches Go code. Never run `gofmt -w` on those five; edit them by hand.
- Every goroutine started under `internal/freshness` recovers panics (I-1); `grep -n 'go func' internal/freshness/*.go` lists them.
- Trust is never inferred from "adopted": only a decodable clearsigned `InRelease` or a verified detached pair is signed. No phase may reintroduce a text fallback in `VerifyInline`, write a signed-seen marker for a plain-text or undecodable anchor, or let a retired snapshot serve, vouch or become current. (Reconcile's own plain-text fallback in `inReleasePlaintext` survives phase A only for test fixtures and is removed in D; production cannot reach it.)
- An I/O fault is never evidence: code that removes trust (audit, quarantine, reaping) acts on "file does not exist" or on content it read and checked, and on any other read error defers (leaves the item queued, changes nothing, logs ERROR). Refusing to start is reserved for faults that would misjudge everything (database errors, a missing or empty pool).
- **Release boundary (recommended; the maintainer decides):** release phases A–E as one version. If A ships alone it is still safe: R-1-quarantined suites re-adopt on their first check, plain-text `InRelease` repositories (which apt itself rejects) stay on Phase 1 serving and lose vouching, signed-seen history accumulates, and nothing can adopt unsigned before E. F–H may ship separately.
- Every phase-A task must stay justified by PR #8 (its core problem or one of its four limitations), a named maintainer decision, or a confirmed defect; the review-findings table below records the mapping for the 2026-10-05 changes.
- Commit messages end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Credit: the flat-repository problem was reported and first analysed by Venkat (@venkatamutyala) in PR #8; the final phase adds him to `Contributors.md`.

## Deliberate deviations from the spec

1. **Production-path fixture for C1** (spec §C1.4 says "a fixture extracted from the production `url_path` snapshot"). Committing real cache paths could leak internal hostnames. Phase A commits a synthetic table of public path shapes (plain and special: `+`, `~`, literal `%`, `./`, `:`), plus an opt-in test that walks a local database when `ACU_URLPATH_FIXTURE_DB` points at one (the plan review ran it on the production copy: 1,467 of 1,467 paths round-trip).
2. **No counter on a `RawPath` mismatch** (spec §C1.2 says a mismatch "increments a counter and falls back"). `aptpath` is standard-library-only, and the fallback is unreachable: escaping is byte-local and a test checks every byte on every toolchain CI builds with, so a Go change to `net/url` fails that test before it ships. The fallback (Go's own escaping, still a correct URL) is kept.

Earlier drafts also filled an empty `anchor_form` from the hash columns instead of rejecting it. The plan review showed that fill can store an empty form under `INSERT OR IGNORE` and can never produce `unsigned`, so phase A follows the spec: a trigger rejects form-less rows, and the 11 test fixtures that insert raw rows name their form.

## Review Focus

The input classes the spec implies that are most likely to bite users, each pinned by a test in the owning phase:

1. **Upgrading a live v6 cache** (never-adopted suites, legacy flat anchors in `url_path`, suites that are dormant): upgrade must not burst-adopt dormant suites or leave legacy flat suites row-less. Tests: phase A (v6-fixture migration, backfill correlation, atomic rollback), D (legacy `dists/` onboarding), E (production-shaped fixture), G (legacy flat onboarding).
2. **Paths with unusual bytes** (`+`, `~`, `%`, space, UTF-8, `|{}`) must reach upstream as apt would send them and map back to the same cache key. Test: phase A (encoding table and round trip).
3. **Upstreams answering 200 HTML for missing files** (soft-404, captive portals, maintenance pages) must never become anchors, suites, or refreshed indexes. Tests: phase D (sniffs), E (classification), G (anchorless sniff rejection).
4. **Clients sharing one apt queue** (mirror mode, many sources on one host) during flips must not straddle. Test: phase C (cross-suite hold test).
5. **Very large suites** (Ubuntu `-updates`, commits of many seconds) must degrade to today's behaviour, never to client timeouts. Test: phase C (slow commit → phase-B expiry within 20 s).
6. **Trust carried across the upgrade** (R-1-polluted snapshots, plain-text inline snapshots, suites served signed metadata but never adopted) must neither keep unearned trust nor lose signed history. Tests: phase A (polluted-v6 audit, anchor marker pass), E (quarantined plain-text suite adopts unsigned without a false refusal), F (A-created signed snapshot → reap → return keeps never-downgrade).
7. **Saturation** (many failing or slow suites on one host, persistent repairs, queued adoptions) must not starve healthy suites elsewhere. Test: phase B (scheduler latency-bound simulation).

## Phases

Each phase: write its detailed plan (`2026-09-28-flat-repositories-<letter>-<name>.md`) → max-effort plan review (reviewers apply the plan in a throwaway worktree and mutation-test its tests) → implement task by task (TDD) → full race suite + lint → max-effort review of the phase diff → fixes → commit → update memory.

| Phase | Plan file | Spec sections | Depends on | Status |
|---|---|---|---|---|
| A. Foundations | `…-A-foundations.md` | Shared infrastructure, C1, C2 (`dists/` only), §6, §6.1 legacy trust audit, C3 marker writes at adoption + sniffs + startup pass, C5 (R-1, I-1 logging, fail-closed inline verification), C4 clock rule | — | done (commit ffa8256) |
| B. Retry discipline and capacity | `…-B-retry.md` | C7, C5 `Date` rules, C4 capacity/validators/`change_seen`, C4 scheduler fairness | A | to write after A |
| C. Flip consistency | `…-C-flip.md` | C8 incl. the hold token | B | to write after B |
| D. Creation bounds and signals | `…-D-creation.md` | C3 (rest: caps, touch, miss-path signed-seen marker, `Release`/flat sniffs) | A | to write after C |
| E. Eager adoption and anchor forms | `…-E-forms.md` | C4 probe tree, C5 unsigned/SHA512, C10 wait | B, D | to write after D |
| F. Suite reaping | `…-F-reaping.md` | C9 | D, E | to write after E |
| G. Flat repositories | `…-G-flat.md` | C2 flat, C5 flat IndexTargets/Filenames, C6, C4 anchorless | A–F | to write after F |
| H. End to end and docs | `…-H-e2e-docs.md` | §7 e2e, §8 | all | to write after G |

## Cross-phase interfaces (names later phases rely on)

Created in phase A (exact signatures in the phase A plan):

- `wallclock.Now() time.Time`, `wallclock.Unix() int64`, `wallclock.SetOffsetForTest(d time.Duration) (restore func())`
- `aptpath.SuitePath(p string) string` (tightened `dists/` grammar; phase G adds the flat grammar inside the same function)
- `aptpath.IsCanonicalSuitePath(s string) bool`
- `aptpath.AptEncodePath(p string) string`, `aptpath.BuildUpstreamURL(scheme, authority, path string) string`
- `cache.AnchorForm` with `cache.FormInline`, `cache.FormDetached`, `cache.FormUnsigned`
- `cache.SuiteSnapshot.SingleAnchorHash *string`, `.Form cache.AnchorForm`, `.ReleaseDate *int64`, `.RetiredAt *int64`
- `cache.SnapshotCandidate.Form cache.AnchorForm` (empty ⇒ derived for the inline and detached hash shapes only; `FormUnsigned` is never derived), `cache.ErrInvalidAnchorForm`
- v7 columns and table exactly as spec §6 (`suite_freshness.last_requested_at`, `adoption_fail_*`, `check_fail_count`; `suite_snapshot.anchor_form` with the rejecting trigger, `release_date`, `retired_at`; `package_hash.flat_filename`; `suite_signed_seen`); `migrationGoSteps`. New suite rows get `last_requested_at = now` on insert (`PutSuiteFreshness`, `CommitAdoption`), so no row is created without an interest stamp.
- `handler.Fetcher` interface (`HostAllowed`, `Fetch`)
- `freshness.ErrAdoptionPanicked`, `freshness.ErrAdoptionGuardFailed`, `(*Checker).recoverGoroutine(where, suite)` (phase B adds the failure record there, before the mutex is released), `(*Checker).clockStep`, `clockStepTolerance` (2 s)
- Test harness `internal/simtest` (synctest helpers) and the simulation starter `startSimChecker` in `internal/freshness/sim_smoke_test.go`
- `gpg.ErrMalformedClearsign` (`malformed_clearsign`); `VerifyInline` accepts only decodable clearsigned envelopes in every mode
- `aptpath.SniffInRelease`, `aptpath.SniffReleaseGPG`, `aptpath.SniffHeadBytes`, `aptpath.MarkerBase`
- `cache.SignedSeenMark`, `(*cache.Cache).MarkSignedSeenBatch`, `(*cache.Cache).SignedSeen`, unexported `markSignedSeen(ctx, execer, …)`; `CommitAdoption` marks inline/detached commits in-transaction
- `legacy_trust_audit` queue (adopted pre-v7 snapshots); `freshness.RunLegacyTrustAudit`; `cache.LegacyAuditBatch(ctx, afterID, limit)`, `ApplyLegacyAuditDecisions` (outcomes `LegacyAuditKeep`, `Drop`, `Quarantine`, `QuarantineSigned`; one transaction per batch), `EnqueueLegacyTrustAudit`, `ListTrackedSuiteAnchors`; `cache.ErrSnapshotRetired` (`CommitAdoption` refuses retired candidates); unexported `decrementSnapshotRefcountsTx` (the single Step-8 refcount release)

Later phases add (named here so phase plans stay consistent):

- B: `cache.RecordAdoptionFailure`, `cache.ClearAdoptionFailure`, `freshness.contentKey`, `freshness.backoffDelay`. `adoption_next_retry_at` is a future-event stamp: B clamps it to now + 1 h and never routes it through `clockStep`; B tests that a future `next_retry_at` after a backward step yields a retry within 1 h, not immediately. B also records panicked adoptions in the admin adoption ring. **Scheduler fairness (review finding 7 and the three-lens review, spec C4):** per-host queues of due suites refreshed every tick (no duplicates, persist across ticks), 4 workers, at most one check in flight per host, hosts take turns round-robin one check per turn; within a host healthy (`check_fail_count = 0`) → never-checked → failing, first in first out by queue time inside each class; backoff 2 min → 1 h computed from the persisted `check_fail_count` and `last_check_at` (every check maintains the streak in the UPDATE that already bumps `last_check_at`); goroutines and queue memory bounded. Latency bound: one tick interval + (n + 1) × ⌈H/4⌉ × 2 min, n = suites ahead of S in its host's queue (healthy ones only when S is healthy). B's DoD includes synctest simulations with **stalling** responders (not refused connections, which the fetch client's unreachable-host cooldown already collapses): (1) H = 8 hosts, several stalling, a healthy suite with n = 3 healthy suites ahead — assert the bound; (2) 500 stalling suites and 1 healthy suite on one host — the healthy suite is checked within one tick + ⌈H/4⌉ × 2 min, and again on the first tick after a simulated restart (a mutation that orders never-checked suites first, keeps the streak in memory, or keys the order on `last_check_at` must fail it); both with persistent repair and changing-candidate contention, continuing checks and a goroutine bound; the capacity simulation exercises both check and adoption admission. (An equivalent pull model — a free worker asks the cache for the next due suite — is acceptable if B's plan finds the queue bookkeeping error-prone.)
- C: `flipgate.Gate` with `AnchorStart`, `Flip`. **Hold token (review finding 6, spec C8):** at most one suite holds at a time (FIFO, wait ≤ 60 s, then commit unheld and count `acu_flip_hold_skipped_total`); phase B commits only after local quiet AND no other hold ended within the quiet period. C's DoD includes the review's overlapping-hold trace (two busy suites, holds 0.1 s apart, a shared client queue holding one suite's old anchor) in a shared-queue synctest simulation with zero straddles, a token-timeout test, and five busy suites flipping within 0.1 s (holds never overlap, at most three hold, the rest commit unheld within 60 s + 20 s and are counted, zero straddles for held suites). Open question for the maintainer: a flip waiting for the token keeps its global adoption slot (spec C8 residual, ≤ ~140 s); releasing the global slot with the host slot before the barrier would avoid that. Retained by-hash serves only non-retired snapshots.
- D: `cache.CreateSuite`, `cache.UpdateSuiteFreshness`, `cache.TouchSuite`, `cache.ErrSuiteGone` (the UPDATE-only `CommitAdoption` Step 6, spec C3), `aptpath.SniffRelease`, `aptpath.SniffFlatIndex` (the `InRelease`/`Release.gpg` sniffs and marker writes already exist from A). **Signed-seen marker on the miss path (review finding 5, its follow-up and the three-lens review, spec C3):** anchor rows (`InRelease`, `Release`) are created in the writer operation that stores the `url_path` row, before the body is written (sniff → `url_path` + `CreateSuite` + marker → write body → T1, today's seed-before-serve order); the marker for a sniff-passing `InRelease`/`Release.gpg` is written in that same operation and nowhere else in the handler — no hit-path write, no wait, no coalescing map, no failure counter; a failed operation serves no body (502, as today). `Release.gpg` creates no row. Tests: the marker row exists before the first body byte on first contact and on a refresh miss; a failed writer operation serves no body; the hit-path invariant (after misses, adoptions, `reconstructAnchor` and restarts, every sniff-passing anchor row has a marker); FIFO ordering against a later unsigned commit. D also removes reconcile's plain-text fallback in `inReleasePlaintext` once `clearsignedFixture()` replaces the plain-text test fixtures (flip `TestReparseSnapshotRelease_PlainTextInReleaseStillParses` to `_PlainTextInReleaseIsAnError`).
- E: `freshness` probe classification (`classPresent`, `classAbsent`, `classUnknown`), `Adopter.RunUnsigned`, `aptpath.AnchorPaths(form)` (spec C5). E's unsigned candidates set `Form: cache.FormUnsigned` explicitly, and its tests assert that an unsigned adoption stores `anchor_form = 'unsigned'` and vouches no `.deb`. E also resolves the two `AIDEV-TODO(phase E)` raw-SQL readers (url_path GC guard, `CommitAdoption` anchor seed: for `FormUnsigned`, Step 3c seeds `<suite>/Release`, not `/InRelease`). E's docs note the marker denial-of-service residual (spec C5 threat notes) at `adopt_unsigned`. **In-transaction never-downgrade (review finding 5, spec C5):** `CommitAdoption` re-reads the marker for `aptpath.MarkerBase(suite)` inside its writer transaction and refuses an unsigned candidate (no pointer change, no rows) when one exists or the prior current snapshot is signed; E's DoD pauses an eligible unsigned adoption before commit, serves a signed anchor and completes the marker write (also via the `/.` twin, once G exists — E adds the twin variant as a G DoD item), then resumes and asserts refusal. **Upgrade (review findings 2–3):** a phase-A-quarantined plain-text suite (fixture from A's `buildLegacyFixture` shape) adopts in the unsigned form through `Release` with no upstream change and no `unsigned_downgrade_refused`; a never-adopted suite whose signed anchor was served under a v6 binary is refused unsigned adoption on its first post-upgrade check (A's startup pass wrote its marker).
- F: `cache.ReapIdleSuite`, `cache.RetireSuiteSnapshots`, admin `POST /forget`. Non-canonical legacy suite rows (`!aptpath.IsCanonicalSuitePath`) are reaped regardless of idleness (spec C9); the plan review found none in production. The reaper's refcount release reuses `decrementSnapshotRefcountsTx`; snapshot GC sub-job C also deletes the snapshots phase A's audit retired, and sub-job B's `keep_displaced` ranking excludes retired rows so they never take retention slots. The reaper sweeps a signed-seen marker only when no suite row exists for its base or `/.` twin and it is older than 365 days. F's DoD includes a signed snapshot adopted under the phase-A binary, reaped by the F binary, then returning: never-downgrade still refuses unsigned adoption (markers outlive reaping).
- G: `aptpath.IsFlatSuitePath`, `aptpath.FlatBase`, `aptpath.DotClean`, `freshness.validateFilename`. Legacy flat onboarding also writes signed-seen markers for sniff-passing cached flat anchors (the startup pass only covers suites that already have rows), and the `/.` twin variant of E's in-transaction recheck test runs here. **PR #8 evidence in G's DoD:** `TestServeHTTP_FlatRepositoryFreshness` (a miss on `/…/deb/InRelease` and on `/…/deb/./InRelease` creates one suite row each and fires T1 under it; a later hit fires T1 again; `/…/deb/Packages_1.0_all.deb`, host-root `/InRelease` and `/…/deb//InRelease` create none), the pkgs.k8s.io shape `/core:/stable:/v1.34/deb/InRelease` → `/core:/stable:/v1.34/deb`, flat `Packages` failure fatal, and the mixed-host strict matrix.
- H: PR #8's end-to-end table in `e2e/flat`: one signed flat repository configured as `deb <uri>/ /` on one client and `deb <uri> ./` on another; both report 2.0 within one cooldown of the v2 publish, and with the upstream stopped two fresh clients install 2.0 from the snapshots.

## Phase A carry-forwards (from the phase A reviews, 2026-10-11)

Each item is owned by the phase it is listed under; that phase's plan addresses it.

**B:**

- The scheduler rewrite recovers panics at the worker and run-loop level. Today a panic in `tick` before the per-suite loop (`ListSuites`, gauges) is unrecovered.
- Panicked adoptions are recorded in the admin adoption ring and the duration histogram.
- Scheduler pacing covers the burst after both backward and forward clock steps.
- The backoff gate applies the C4 clock-step rule to `adoption_fail_*_at`, as the AIDEV-NOTE at the clock-step helper says.
- `simtest` phase lengths are not exact multiples of the tick interval (same-instant goroutine order is nondeterministic). `startSimChecker`-style helpers register `t.Cleanup(stop)`. `StaticConditional`-style latency fetchers honour `ctx.Done()`.

**D:**

- The one-insert-site convergence for `suite_freshness` must account for the two phase-A insert sites that stamp `last_requested_at`: `CommitAdoption` Step 6 and `PutSuiteFreshness`.
- `SniffReleaseGPG` accepts `0xC2`, which is also a UTF-8 lead byte. This is the safe direction and spec-mandated, but phase D's miss-path marker should know it.
- `requireBareClearsigned` admits leading whitespace that apt's first-line sniff rejects. Decide whether adoption should match apt.

**E:**

- Production unsigned candidates must set `SnapshotCandidate.Form` explicitly; derivation from hashes is for legacy and fixture callers only.
- `checkLockedDetached` (still tests `ReleaseHash != nil`) and `snapshotAnchorHash`'s `Release` case need `Form` dispatch for unsigned.
- `suite_signed_seen.last_seen_at` is a past-event stamp under C4: a future stamp counts as infinitely old and is healed on the next observation. The anchor pass copies `MAX(last_requested_at, last_fetched_at)` verbatim.

**F:**

- Snapshot GC's `keep_displaced` ranking must exclude retired snapshots, which today occupy slots indefinitely. A quarantined plain-text suite's retired row is otherwise kept forever.
- `IsCanonicalSuitePath("/foo/dists/InRelease")` is true: reap such suites by idleness.

**Open decision (spec-level, for the maintainer):**

- An `anchor_corrupt` or `anchor_oversized` quarantine leaves the known-corrupt `url_path` anchor in Phase-1 serving, and the checker sees "unchanged". This is pre-existing behaviour for any bit-rotted metadata blob. Decide in a later phase whether the audit should also invalidate that `url_path` row.

## Review findings 2026-10-05 → where they are addressed

| # | Finding | Spec | Plan |
|---|---|---|---|
| 1 | Already-materialised R-1 pollution survives the parser fix | §6.1 | A: Tasks 5 (queue), 15 (audit, quarantine) |
| 2 | Legacy plain-text inline snapshots keep trust; false signed-seen | §6.1, C5 | A: Tasks 5 (no migration markers), 13, 15; E upgrade test |
| 3 | Signed history missing for unadopted suites and staged upgrades | C3, §6.1 | A: Tasks 14 (adoption writes), 15 (startup anchor pass, every start); F reap-and-return test; G flat anchors |
| 4 | Malformed signed envelopes accepted when signatures are optional | C5 | A: Task 13 |
| 5 | Signed-seen not rechecked atomically at unsigned commit | C3, C5 | D (marker in the miss path's writer operation, before the body), E (in-transaction recheck and race test) |
| 6 | Concurrent holds let a shared queue straddle | C8 | C (hold token, cross-suite quiet in phase B, trace simulation) |
| 7 | Sequential scheduler starves healthy suites | C4 | B (per-host queues, worker pool, healthy → never-checked → failing FIFO, persisted streak and backoff, corrected latency bound and simulations) |

A follow-up Fable max-effort review of the revision (2026-10-05) found no blocker; its four major findings — detached path untested by the audit, I/O faults turned into quarantines, the first-contact marker hole, and an incorrect latency bound — and its minor findings are folded into spec §6.1/C3/C4/C5 and phase A Tasks 8, 13, 15 and 16.

A three-lens Fable max-effort review (2026-10-11: grounding in PR #8, best remedy per finding, residual risk) found every change grounded and no scope creep. Folded in: the failure streak persisted (`check_fail_count`) with healthy → never-checked → failing FIFO classes; the signed-seen marker written only on the miss path in the writer operation that stores the anchor (no hit-path write, no 2 s wait, no residual window) and orphan-only marker sweeps; `Release.gpg` creates no suite row; a per-anchor read error defers instead of refusing to start; only adopted snapshots queued; decoder-exact canonicalisation and armor-header and CRLF injection tests; a recovery test for R-1 quarantines; a startup-order test; precise recovery and strict-mode wording; PR #8's evidence tests named in G and H.

## Execution handoff

After each phase lands, the executor writes the next phase's detailed plan against the code as it then exists, has it reviewed, and continues. The overview's phase table is the progress tracker; mark a phase "done (commit <sha>)" when its DoD is met.
