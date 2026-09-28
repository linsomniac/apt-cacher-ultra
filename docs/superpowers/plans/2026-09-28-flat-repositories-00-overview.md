# Flat repositories and suite lifecycle — Implementation Plan (overview)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make flat apt repositories first-class (refresh, adoption, strict-mode vouching), support unsigned, Release-less and SHA512-only repositories, and fix limitations #1–#3 (adoption retry loops, flip race, immortal suites) with bounded long-term behaviour.

**Architecture:** Eight phases (A–H), each leaving shippable, green state. Safety nets land before the features that need them: foundations → retry discipline and capacity → flip barrier → creation bounds → eager adoption and anchor forms → reaping → flat repositories → real-apt e2e and docs. Each phase has its own detailed plan, written after the previous phase lands (later phases edit code the earlier ones produce, so their exact code cannot be written reliably up front).

**Tech Stack:** Go 1.25 (toolchain 1.26), modernc SQLite, `github.com/ProtonMail/go-crypto`, `testing/synctest`, Docker for e2e.

**Spec:** `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (revision 3). Every task implements a named spec section; when this plan and the spec disagree, the spec wins unless the plan records a deliberate, explained deviation (listed below).

## Global Constraints

- Never edit an existing entry of `migrations` in `internal/cache/schema.go`; all schema changes go in the single new v7 entry (spec §6).
- Production SQL never uses SQLite clock functions (`strftime`, `CURRENT_TIMESTAMP`, `julianday`, `unixepoch`); persisted stamps come from `wallclock.Now()`.
- Every persisted Unix stamp comes from `internal/wallclock`; durations, deadlines and in-memory timing use monotonic `time.Now`/`time.Since`.
- `internal/cache` must not import `internal/proxy`; shared path logic lives in the leaf package `internal/aptpath` (standard library only).
- Adoption-disabled (Phase 1) behaviour stays as today: with no adopter, nothing refreshes and the seed is today's seed.
- New code that can block does so on channels/timers, never a `sync.Mutex` held across a wait.
- Constants from spec §C11: backoff 2 m → 1 h; hold 20 s; phase-A cap 60 s; waiter caps 256 (barrier) and 64 (strict wait); strict wait 15 s; reaps 25 per run; unsigned member cap 1,000; check deadline 2 min; interest window 7 d; marker TTL 365 d; cap-rejection cache 10 min.
- Config keys and defaults: `adoption.adopt_unsigned = true`; `adoption.flip_quiet_period = "10s"` (0s disables, ≤ 15s); `adoption.max_duration = "30m"` (≥ 1m); `freshness.max_suites = 5000` (≥ 10; per-host = max_suites/10); `gc.suite_idle_ttl = "720h"` (0s disables; else ≥ 24h).
- Every changed invariant gets an `AIDEV-NOTE`; existing `AIDEV-*` comments are kept and updated, never removed.
- `go test -race ./...` and `golangci-lint run ./...` are clean, and `gofmt -l .` lists no files beyond the five already listed on `main` (formatter drift we do not touch), at the end of every task that touches Go code.
- Commit messages end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Credit: the flat-repository problem was reported and first analysed by Venkat (@venkatamutyala) in PR #8; the final phase adds him to `Contributors.md`.

## Deliberate deviations from the spec

1. **`anchor_form` trigger fills instead of rejecting** (spec §6 says "a trigger rejects inserts with an empty form"). About 30 existing test fixtures insert `suite_snapshot` rows with raw SQL and no `anchor_form`. The v7 trigger is `AFTER INSERT … WHEN NEW.anchor_form = ''` and sets the form from the hash columns (inline if `inrelease_hash` is set, else detached). Production inserts always set the form explicitly (`InsertCandidateSnapshot`), unsigned snapshots can only be created with an explicit form, and a duplicate of existing content still fails on the unique index when the trigger's UPDATE runs. Phase A updates the spec text to match.
2. **Production-path fixture for C1** (spec §C1.4 says "a fixture extracted from the production `url_path` snapshot"). Committing real cache paths could leak internal hostnames. Phase A commits a synthetic table of real-world public path shapes, plus an opt-in test that walks a local database when `ACU_URLPATH_FIXTURE_DB` points at one.

## Review Focus

The input classes the spec implies that are most likely to bite users, each pinned by a test in the owning phase:

1. **Upgrading a live v6 cache** (never-adopted suites, legacy flat anchors in `url_path`, suites that are dormant): upgrade must not burst-adopt dormant suites or leave legacy flat suites row-less. Tests: phase A (v6-fixture migration), D (legacy `dists/` onboarding), E (production-shaped fixture), G (legacy flat onboarding).
2. **Paths with unusual bytes** (`+`, `~`, `%`, space, UTF-8, `|{}`) must reach upstream as apt would send them and map back to the same cache key. Test: phase A (encoding table and round trip).
3. **Upstreams answering 200 HTML for missing files** (soft-404, captive portals, maintenance pages) must never become anchors, suites, or refreshed indexes. Tests: phase D (sniffs), E (classification), G (anchorless sniff rejection).
4. **Clients sharing one apt queue** (mirror mode, many sources on one host) during flips must not straddle. Test: phase C (cross-suite hold test).
5. **Very large suites** (Ubuntu `-updates`, commits of many seconds) must degrade to today's behaviour, never to client timeouts. Test: phase C (slow commit → phase-B expiry within 20 s).

## Phases

Each phase: write its detailed plan (`2026-09-28-flat-repositories-<letter>-<name>.md`) → max-effort plan review → implement task by task (TDD) → full race suite + lint → max-effort review of the phase diff → fixes → commit → update memory.

| Phase | Plan file | Spec sections | Depends on | Status |
|---|---|---|---|---|
| A. Foundations | `…-A-foundations.md` | Shared infrastructure, C1, C2 (`dists/` only), §6, C5 (R-1, I-1 logging), C4 clock rule | — | planned |
| B. Retry discipline and capacity | `…-B-retry.md` | C7, C5 `Date` rules, C4 capacity/validators/`change_seen` | A | to write after A |
| C. Flip consistency | `…-C-flip.md` | C8 | B | to write after B |
| D. Creation bounds and signals | `…-D-creation.md` | C3 | A | to write after C |
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
- `cache.SnapshotCandidate.Form cache.AnchorForm` (empty ⇒ derived from hash columns)
- v7 columns and table exactly as spec §6 (`suite_freshness.last_requested_at`, `adoption_fail_*`; `suite_snapshot.anchor_form`, `release_date`, `retired_at`; `package_hash.flat_filename`; `suite_signed_seen`)
- `handler.Fetcher` interface (`HostAllowed`, `Fetch`)
- `freshness.ErrAdoptionPanicked`, `freshness.ErrAdoptionGuardFailed`
- Test harness `internal/simtest` (synctest helpers)

Later phases add (named here so phase plans stay consistent):

- B: `cache.RecordAdoptionFailure`, `cache.ClearAdoptionFailure`, `freshness.contentKey`, `freshness.backoffDelay`, `cache.ErrSuiteGone`
- C: `flipgate.Gate` with `AnchorStart`, `Flip`
- D: `cache.CreateSuite`, `cache.UpdateSuiteFreshness`, `cache.TouchSuite`, `cache.MarkSignedSeen`, `aptpath` sniffs (`SniffInRelease`, `SniffRelease`, `SniffReleaseGPG`, `SniffFlatIndex`)
- E: `freshness` probe classification (`classPresent`, `classAbsent`, `classUnknown`), `Adopter.RunUnsigned`
- F: `cache.ReapIdleSuite`, `cache.RetireSuiteSnapshots`, admin `POST /forget`
- G: `aptpath.IsFlatSuitePath`, `aptpath.FlatBase`, `aptpath.DotClean`, `freshness.validateFilename`

## Execution handoff

After each phase lands, the executor writes the next phase's detailed plan against the code as it then exists, has it reviewed, and continues. The overview's phase table is the progress tracker; mark a phase "done (commit <sha>)" when its DoD is met.
