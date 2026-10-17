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
- **Release boundary (maintainer decision, 2026-10-17):** phases A–H ship together, with other changes, in the 1.1.0 release candidate. Nothing from this branch is released earlier, so no phase needs to be independently releasable; each phase still ends green (tests, lint, gofmt) so the branch stays bisectable. The phase order is unchanged.
- Phase A's deferred review minors are listed in `2026-09-28-flat-repositories-A-deferred.md` (IDs A-1…A-57). Each phase plan includes the items in files it touches; phase H sweeps the rest and deletes that file.
- Every phase-A task must stay justified by PR #8 (its core problem or one of its four limitations), a named maintainer decision, or a confirmed defect; the review-findings table below records the mapping for the 2026-10-05 changes.
- Commit messages end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Credit: the flat-repository problem was reported and first analysed by Venkat (@venkatamutyala) in PR #8, and this work is based on that PR. They are credited in `Contributors.md` (added 2026-10-17, after phase A); every phase keeps that credit and its PR #8 reference.

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
| B. Retry discipline and capacity | `…-B-retry.md` | C7, C5 `Date` rules, C4 capacity/validators/`change_seen`, C4 scheduler fairness, C4 slot-driven admission | A | plan written 2026-10-17; executing |
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

- B: `cache.RecordAdoptionFailure`, `cache.ClearAdoptionFailure`, `freshness.contentKey`, `freshness.backoffDelay`. `adoption_next_retry_at` is a future-event stamp: B clamps it to now + 1 h and never applies the past-event ("infinitely old") rule to it — a detection is only logged through `clockStep` (phase B plan, deviation 6); B tests that a future `next_retry_at` after a backward step yields a retry within 1 h, not immediately. B also records panicked adoptions in the admin adoption ring. **Scheduler fairness (review finding 7 and the three-lens review, spec C4):** per-host queues of due suites refreshed every tick (no duplicates, persist across ticks), 4 workers, at most one check in flight per host, hosts take turns round-robin one check per turn; within a host healthy (`check_fail_count = 0`) → never-checked → failing, first in first out by queue time inside each class; backoff 2 min → 1 h computed from the persisted `check_fail_count` and `last_check_at` (every check maintains the streak in the UPDATE that already bumps `last_check_at`); goroutines and queue memory bounded. Latency bound: one tick interval + (n + 1) × ⌈H/4⌉ × 2 min, n = suites ahead of S in its host's queue (healthy ones only when S is healthy). B's DoD includes synctest simulations with **stalling** responders (not refused connections, which the fetch client's unreachable-host cooldown already collapses): (1) H = 8 hosts, several stalling, a healthy suite with n = 3 healthy suites ahead — assert the bound; (2) 500 stalling suites and 1 healthy suite on one host — the healthy suite is checked within one tick + ⌈H/4⌉ × 2 min, and again on the first tick after a simulated restart (a mutation that orders never-checked suites first, keeps the streak in memory, or keys the order on `last_check_at` must fail it); both with persistent repair and changing-candidate contention, continuing checks and a goroutine bound; the capacity simulation exercises both check and adoption admission. (An equivalent pull model — a free worker asks the cache for the next due suite — is acceptable if B's plan finds the queue bookkeeping error-prone.) **Slot-driven admission (spec C4 rev 5, phase B Task 11b):** `(*admission).freeFor(host, signedCurrent) int` (a peek that mirrors `tryAcquire`) and its `freed` wake channel; `checkOpts{bypassCooldown}`, `checkReport{result, signedCurrent}`, `(*Checker).check(ctx, ref, opts) checkReport` behind `Check`; `isQueuedResult(result)`; scheduler `park`, `finish`, `unhold`, `pop(admit)`; gauge `acu_freshness_waiting_for_slot`. A scheduler check that ends queued leaves its suite waiting in its host's queue; every slot release re-offers the freed capacity to the oldest waiting suite it admits, cooldown bypassed; a busy offer is held until the next wake or tick.
- C: `flipgate.Gate` with `AnchorStart`, `Flip`. **Hold token (review finding 6, spec C8):** at most one suite holds at a time (FIFO, wait ≤ 60 s, then commit unheld and count `acu_flip_hold_skipped_total`); phase B commits only after local quiet AND no other hold ended within the quiet period. C's DoD includes the review's overlapping-hold trace (two busy suites, holds 0.1 s apart, a shared client queue holding one suite's old anchor) in a shared-queue synctest simulation with zero straddles, a token-timeout test, and five busy suites flipping within 0.1 s (holds never overlap, at most three hold, the rest commit unheld within 60 s + 20 s and are counted, zero straddles for held suites). Open question for the maintainer: a flip waiting for the token keeps its global adoption slot (spec C8 residual, ≤ ~140 s); releasing the global slot with the host slot before the barrier would avoid that. Retained by-hash serves only non-retired snapshots.
- D: `cache.CreateSuite`, `cache.UpdateSuiteFreshness`, `cache.TouchSuite`, `cache.ErrSuiteGone` (the UPDATE-only `CommitAdoption` Step 6, spec C3), `aptpath.SniffRelease`, `aptpath.SniffFlatIndex` (the `InRelease`/`Release.gpg` sniffs and marker writes already exist from A). **Signed-seen marker on the miss path (review finding 5, its follow-up and the three-lens review, spec C3):** anchor rows (`InRelease`, `Release`) are created in the writer operation that stores the `url_path` row, before the body is written (sniff → `url_path` + `CreateSuite` + marker → write body → T1, today's seed-before-serve order); the marker for a sniff-passing `InRelease`/`Release.gpg` is written in that same operation and nowhere else in the handler — no hit-path write, no wait, no coalescing map, no failure counter; a failed operation serves no body (502, as today). `Release.gpg` creates no row. Tests: the marker row exists before the first body byte on first contact and on a refresh miss; a failed writer operation serves no body; the hit-path invariant (after misses, adoptions, `reconstructAnchor` and restarts, every sniff-passing anchor row has a marker); FIFO ordering against a later unsigned commit. D also removes reconcile's plain-text fallback in `inReleasePlaintext` once `clearsignedFixture()` replaces the plain-text test fixtures (flip `TestReparseSnapshotRelease_PlainTextInReleaseStillParses` to `_PlainTextInReleaseIsAnError`).
- E: `freshness` probe classification (`classPresent`, `classAbsent`, `classUnknown`), `Adopter.RunUnsigned`, `aptpath.AnchorPaths(form)` (spec C5). E's unsigned candidates set `Form: cache.FormUnsigned` explicitly, and its tests assert that an unsigned adoption stores `anchor_form = 'unsigned'` and vouches no `.deb`. E also resolves the two `AIDEV-TODO(phase E)` raw-SQL readers (url_path GC guard, `CommitAdoption` anchor seed: for `FormUnsigned`, Step 3c seeds `<suite>/Release`, not `/InRelease`). E's docs note the marker denial-of-service residual (spec C5 threat notes) at `adopt_unsigned`. **In-transaction never-downgrade (review finding 5, spec C5):** `CommitAdoption` re-reads the marker for `aptpath.MarkerBase(suite)` inside its writer transaction and refuses an unsigned candidate (no pointer change, no rows) when one exists or the prior current snapshot is signed; E's DoD pauses an eligible unsigned adoption before commit, serves a signed anchor and completes the marker write (also via the `/.` twin, once G exists — E adds the twin variant as a G DoD item), then resumes and asserts refusal. **Upgrade (review findings 2–3):** a phase-A-quarantined plain-text suite (fixture from A's `buildLegacyFixture` shape) adopts in the unsigned form through `Release` with no upstream change and no `unsigned_downgrade_refused`; a never-adopted suite whose signed anchor was served under a v6 binary is refused unsigned adoption on its first post-upgrade check (A's startup pass wrote its marker).
- F: `cache.ReapIdleSuite`, `cache.RetireSuiteSnapshots`, admin `POST /forget`. Non-canonical legacy suite rows (`!aptpath.IsCanonicalSuitePath`) are reaped regardless of idleness (spec C9); the plan review found none in production. The reaper's refcount release reuses `decrementSnapshotRefcountsTx`; snapshot GC sub-job C also deletes the snapshots phase A's audit retired, and sub-job B's `keep_displaced` ranking excludes retired rows so they never take retention slots. The reaper sweeps a signed-seen marker only when no suite row exists for its base or `/.` twin and it is older than 365 days. F's DoD includes a signed snapshot adopted under the phase-A binary, reaped by the F binary, then returning: never-downgrade still refuses unsigned adoption (markers outlive reaping).
- G: `aptpath.IsFlatSuitePath`, `aptpath.FlatBase`, `aptpath.DotClean`, `freshness.validateFilename`. Legacy flat onboarding also writes signed-seen markers for sniff-passing cached flat anchors (the startup pass only covers suites that already have rows), and the `/.` twin variant of E's in-transaction recheck test runs here. **PR #8 evidence in G's DoD:** `TestServeHTTP_FlatRepositoryFreshness` (a miss on `/…/deb/InRelease` and on `/…/deb/./InRelease` creates one suite row each and fires T1 under it; a later hit fires T1 again; `/…/deb/Packages_1.0_all.deb`, host-root `/InRelease` and `/…/deb//InRelease` create none), the pkgs.k8s.io shape `/core:/stable:/v1.34/deb/InRelease` → `/core:/stable:/v1.34/deb`, flat `Packages` failure fatal, and the mixed-host strict matrix. G also ports the rest of PR #8's tests (see "PR #8 parity" below).
- H: PR #8's end-to-end table in `e2e/flat`: one signed flat repository configured as `deb <uri>/ /` on one client and `deb <uri> ./` on another; both report 2.0 within one cooldown of the v2 publish, and with the upstream stopped two fresh clients install 2.0 from the snapshots. H also runs the live-upstream smoke and documents the strict-mode consequences listed under "PR #8 parity" below.

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
- Slot-driven admission (phase B Task 11b): the probe-tree check sets `out.report` on every exit; `isQueuedResult` includes `first_adoption_queued`; first and unsigned requests pass `signedCurrent = false`. The capacity simulation gains the upgrade-burst scenario: 50 never-adopted suites with interest and a trickle of signed changes → first adoptions back to back, signed suites never wait longer than one adoption, each queued suite costs at most two probes. Document `max_concurrent_adoptions = 3` as the knob that lets two first adoptions run during the burst.
- `checkLockedDetached` (still tests `ReleaseHash != nil`) and `snapshotAnchorHash`'s `Release` case need `Form` dispatch for unsigned.
- `suite_signed_seen.last_seen_at` is a past-event stamp under C4: a future stamp counts as infinitely old and is healed on the next observation. The anchor pass copies `MAX(last_requested_at, last_fetched_at)` verbatim.

**F:**

- Snapshot GC's `keep_displaced` ranking must exclude retired snapshots, which today occupy slots indefinitely. A quarantined plain-text suite's retired row is otherwise kept forever.
- `IsCanonicalSuitePath("/foo/dists/InRelease")` is true: reap such suites by idleness.
- A reaped suite's parked scheduler entry (phase B Task 11b) flushes on its next offer (`ErrNotFound` → `skipped` → done); the reaper need not touch the scheduler, and `acu_freshness_waiting_for_slot` may briefly count a reaped suite. F's tests include one such suite.

**Open decision (spec-level, for the maintainer):**

- An `anchor_corrupt` or `anchor_oversized` quarantine leaves the known-corrupt `url_path` anchor in Phase-1 serving, and the checker sees "unchanged". This is pre-existing behaviour for any bit-rotted metadata blob. Decide in a later phase whether the audit should also invalidate that `url_path` row.

## PR #8 parity (gap analysis, 2026-10-17)

A comparison of this branch at `fa30e7f` with PR #8 (head `6ec5dee`, fetched locally as `pr-8-review`) found that phase A carries none of PR #8's functional changes; they are all owned by G (strict-mode scoping is deliberately replaced by C6 and C10). The items below close the gaps the comparison found between PR #8 and the G and H definitions of done. G's and H's plans address them.

**G: port PR #8's tests, with results under our rules.**

1. **Suite grammar.** PR #8's flat `TestSuitePath` cases, verbatim: `/core:/stable:/v1.34/deb/` + `InRelease`, `Release`, `Release.gpg`, `Packages`, `Packages.gz`, `Packages.zst` → `/core:/stable:/v1.34/deb`; `/repo/flat/Sources.xz` → `/repo/flat`; `…/deb/./InRelease` and `…/deb/./Packages.gz` → `…/deb/.`; and no suite for `…/deb/amd64/kubelet_1.34.1-1.1_amd64.deb`, `/repo/flat/Packages_1.0_all.deb`, `/repo/flat/Translation-en`, `/repo/flat/by-hash/SHA256/abc`, `/InRelease`, `/Packages.gz`, `InRelease` (not absolute), `/repo//InRelease`, `/repo/./sub/InRelease`, `/evil/../pkgs/InRelease`, `/repo/%2E/InRelease` (a decoded `%`), `/./InRelease`, `/ubuntu/dists//noble/InRelease`. The one intentional difference: `/repo/flat/Contents-amd64.gz` → `/repo/flat` (C2 gives `Contents-*` a suite). Add a valid 64-hex `by-hash/SHA256/<hex>` → `/repo/flat`.
2. **Index targets (layout-aware, C5).** Each table runs with a flat suite path, and again with the same member paths under a `dists/` suite, where none of them is an IndexTarget:
   - `isIndexTarget`: `Packages`, `Packages.gz`, `Packages.xz`, `Packages.zst`, `Packages.diff/Index`, `Sources`, `Sources.gz` are targets; `main/Packages.gz` and `Packages_1.0_all.deb` are not.
   - `indexTargetGroup`: `Packages`, `Packages.gz`, `Packages.xz` → (`Packages`, `all`); `Sources.gz` → (`Sources`, `source`); `Packages.diff/Index` and `main/Packages.gz` → no group.
   - `archFromFilteredPath`: `Sources`, `Sources.xz`, `Sources.diff/Index` → `source`; `Packages`, `Packages.gz` are never filtered.
   - `missingRequestableIndexGroups`: flat `Packages` declared but not fetched is flagged even with no allowlist; served by a compressed variant only, it is not; flat `Sources` absent is not flagged when `source` is not allowlisted.
   - Handler `isIndexTargetPath` (the `snapshot_index_target_404` alert): `Packages`, `Packages.gz`, `Sources.xz` are flagged; `InRelease`, `amd64/kubelet_1.34.1-1.1_amd64.deb`, `Packages.diff/Index` are not.
3. **Adoption.** `TestAdopter_FlatSuiteAdopts` (the snapshot holds `InRelease`, `Packages`, `Packages.gz`, fetched relative to the suite path) and `TestAdopter_FlatPackagesFailureFatal`, keeping its `Translation-en` member that fetches successfully, so the all-members-4xx guard cannot be what fails the run.
4. **Strict matrix.** PR #8's nine `TestPhase3StrictMode_FlatSnapshotScopedToItsDirectory` cases, re-expressed for C6 vouching and the host-wide C10 rule (signed snapshots; "complete" = complete coverage; "declared" = the Filename is in that suite's `Packages`), plus three new cases. Expected statuses:

   | Case | PR #8 | Ours |
   |---|---|---|
   | Complete `dists/` + complete flat `/flat`: unvouched `dists/` `.deb` | 502 | 502 |
   | Same host: `/flat/amd64/x.deb`, declared | 200 (passthrough) | 200 (vouched, hash checked) |
   | Same host: a `.deb` under `/flat` that is not declared | 200 (passthrough) | 502 |
   | Suite `/flat/.`: `/flat/amd64/x.deb`, declared | 200 | 200 (exact row at `FlatBase`) |
   | Suite `/flat/.`: `/flat/./amd64/x.deb`, declared | 200 | 200 (suffix fallback after `DotClean`) |
   | `/flat/../ubuntu/pool/unknown.deb` | 502 | 502 (fallback stops at `..`) |
   | Suite `/r/sub`: `/r/amd64/x.deb`, declared (Jenkins shape) | 502 | 200 (parent base) |
   | Flat-only host, complete: an undeclared `.deb` elsewhere on the host | 200 (inactive) | 502 (strict mode now protects flat hosts) |
   | Incomplete `dists/` snapshot + flat snapshot: unvouched `.deb` | 200 | 200 (host-wide passthrough) |
   | **New:** complete `dists/` + flat snapshot with incomplete coverage (a `no_sha256` stanza): unvouched `dists/` `.deb` | 502 (flat excluded) | 200, with `unvouched_deb_passthrough_no_coverage` whose `incomplete_snapshot_id` is the flat snapshot |
   | **New:** pkgs.k8s.io shape: two complete flat minors on one host, a client adds a third and requests one of its `.deb`s while its first adoption is running | 200 (inactive for that directory) | 200 when the adoption finishes within the 15 s C10 wait |
   | **New:** the same, while that first adoption is queued, not running | 200 | 502 (documented C10 consequence) |

   The first new row is PR #8's second-commit regression, kept by design: it is the rule an incomplete `dists/` snapshot already follows. PR #8's `TestPhase3StrictMode_SnapshotCountExcludesOutOfScopeFlat` becomes: `current_snapshot_count` counts every signed, non-retired current snapshot on the host, flat ones included, and never unsigned or anchorless ones.

**H:**

- **Live-upstream smoke (manual, before the release that ships G; not CI).** PR #8's author verified against live pkgs.k8s.io (v1.32–v1.37, `kubectl` 1.34.11 installed through the cache); our e2e uses fixtures only. With adoption enabled and `integrity.refuse_unvouched_debs = true`, front live pkgs.k8s.io (two minors, one client per source form), NVIDIA CUDA, libnvidia-container, pkg.jenkins.io and download.sublimetext.com. For each: `adoption_success` logged; `apt-get update` output clean under the e2e grep; one package installs; zero `unvouched_deb_refused`. Record the results in the release notes.
- **Docs (`docs/configuration.md`, strict mode):** a flat snapshot with incomplete coverage turns strict mode off for its whole host, as an incomplete `dists/` snapshot does; on a strict host, the first install from a newly added flat suite can be refused while its first adoption is queued; strict mode now refuses undeclared `.deb`s on flat-only hosts.

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
