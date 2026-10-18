# Flat repositories — Phase D: Creation bounds and signals — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give `suite_freshness` one creation choke point with caps and bounded eviction, make every other writer UPDATE-only, record client interest and signed-seen history at the points spec C3 names — the signed-seen marker inside the writer operation that stores a signed anchor, before its body is served — and close I-4 on both sides, so minted paths can grow no table or map past the caps.

**Architecture:** `aptpath` gains the `Release` and flat-index sniffs and one classifier (`ClassifySuiteFile`) that tells the handler, for any request path, whether serving it creates the suite's row, earns its base a signed-seen marker, or records client interest. `internal/cache` holds the only `INSERT INTO suite_freshness` (`createSuiteTx`, reached through `CreateSuite` for url_path-hit onboarding and `StoreFetched` for misses), enforces `freshness.max_suites` globally and a tenth of it per host, and at the global cap evicts the least-recently-requested idle suite without a current snapshot — holding that suite's in-memory mutex through a `SuiteGuard` the freshness checker implements, and deleting it with the compare-and-delete helper phase F's reaper will reuse. The handler's miss path stores the url_path row, the suite row and the marker in one writer transaction before writing the body, then registers the serve with the flip barrier; its hit path onboards anchors cached before this release, touches suites it serves (coalesced in the cache to one write per suite per hour), and serves by-hash from the pool only for known suites. The checker checks and reconciles only suites with rows and re-validates its lock after `TryLock`.

**Tech Stack:** Go 1.25 (toolchain 1.26), modernc SQLite, `testing/synctest`, `github.com/ProtonMail/go-crypto` (clearsign decode only).

**Spec:** `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (revision 6; Task 13 records this plan's rulings and deviations as revision 7) — C3 in full (sniffs table incl. `Release` and flat indexes; `cache.CreateSuite` as the single INSERT site with the global and per-host caps, cap eviction, the 10-minute rejection cache, the rate-limited WARN `suite_seed_rejected`, its counter and gauge; the handler as the only caller, on a miss or a url_path hit; anchor rows and markers in the writer operation that stores the url_path row, before the body; legacy `dists/` onboarding; the adoption-disabled seed byte for byte with no T1; UPDATE-only writers; I-4 both sides; client interest; the miss-path marker writer and the hit-path invariant; FIFO ordering), C11's `freshness.max_suites` and `suite_seed_rejected`, C4's scheduler queue bound (phase B plan, deviation 4), §7 "C3" and "Fixtures", §9 phase D row. Interactions read: C4 (validators, scheduler, the T1-on-seed that §9 gives phase E), C5 (never-downgrade consumes the marker in E), C8 (gate registration), C9 (the reaper D's eviction must not duplicate). Overview: `docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md` — Global Constraints, Review Focus items 1 and 3, the "D:" line under "Later phases add" and the "D:" carry-forwards (all four). Deferred minors: `…-A-deferred.md`, `…-B-deferred.md`, `…-C-deferred.md` (IDs cited per task; Task 12's intro lists the ones taken and the ones checked and left, with the reason; Task 13 deletes the taken ones).

## Global Constraints

Everything in the overview's Global Constraints applies. In particular:

- No schema change: v7 already holds every column and table this phase writes (`suite_freshness.last_requested_at`, `suite_snapshot.retired_at`, `suite_signed_seen`). Never edit an entry of `migrations`.
- Production SQL never uses SQLite clock functions; persisted stamps come from `wallclock` (the cache's `nowUnix`, the handler's `wallclock.Unix()`). In-memory timing — touch coalescing, the rejection cache, the WARN rate limit — uses monotonic `time.Now`/`time.Since`.
- `internal/cache` never imports `internal/freshness` or `internal/proxy`; the eviction guard is the cache-side interface `cache.SuiteGuard`, which `*freshness.Checker` implements. The writer goroutine never runs foreign code: the guard is taken before the writer operation is submitted.
- Exactly one `INSERT INTO suite_freshness` exists in production code, in `createSuiteTx` (`TestSuiteFreshnessHasOneInsertSite`). Every other writer of `suite_freshness` is UPDATE-only. Outside `internal/cache`, only `internal/handler` calls `CreateSuite` or `StoreFetched`, and no production code calls the three test-only seeding helpers `SeedSuiteForTest`, `EnsureSuiteForTest` and `InsertTrackedCandidateForTest` (`TestOnlyTheHandlerCreatesSuiteRows`; other `…ForTest` helpers, such as the `ResetForTest` that `internal/proxy` calls, are out of its scope).
- Constants (spec C3/C11), not knobs: rejection cache 10 min (at most 4,096 suites); eviction idle 24 h, at most eight candidates; touch coalescing 1 h; `suite_seed_rejected` at most once a minute. Knob: `freshness.max_suites` (default `5000`, at least `10`; per-host cap `max_suites / 10`).
- Adoption-disabled behaviour stays as today: with no adopter the only rows created are `<suite>/InRelease` seeds, stamped exactly as the phase C tip stamps them (`TestServeHTTP_AdoptionDisabledSeedParity`), and no T1 fires on a miss.
- Tests that call `wallclock.SetOffsetForTest` never use `t.Parallel()`. Simulations and the scale test run inside `testing/synctest` bubbles where they need virtual time, finish in under 5 s under `-race`, and silence loggers with `simtest.DiscardLogger()` (or capture them).
- Every goroutine started under `internal/freshness` recovers panics, and so does every goroutine this phase starts in `internal/handler` (deferred minor C-24).
- `go test -race ./...` and `golangci-lint run ./...` are clean; `gofmt -l .` lists only the five known drift files (`cmd/apt-cacher-ultra/ca_print.go`, `internal/cache/cache_test.go`, `internal/cache/gc_urlpath_test.go`, `internal/freshness/hot_prefetch_test.go`, `internal/proxy/connect_stats.go`) at the end of every task. Never run `gofmt -w` on those five; edit them by hand (Task 4's rename scripts edit three of them — `cache_test.go`, `gc_urlpath_test.go`, `hot_prefetch_test.go` — word by word and must not reformat them).
- Commit trailer: the implementer's own harness attribution line (phase A ruling); otherwise `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

Conventions in this plan:

- Each task's **Interfaces** block lists what it uses from other tasks and existing test files. Never redefine a helper: a second definition in the same package does not compile.
- Each commit step starts with `gofmt -l <files>`; it must print nothing (the drift files are named where a task touches them).
- A "verify failure" step whose expected result is a compile failure is complete when the compiler names the undefined identifiers listed.
- Mutation checks are run one at a time, each reverted before the next; record each failing test line in the commit message.
- **Existing tests that change.** Task 4 migrates every fixture that relied on an upsert (listed there, applied by a script whose counts it states); Tasks 3 and 9 replace plain-text `InRelease` fixtures with `simtest.ClearsignedFixture` (listed there); Task 10 seeds the suite row in the one by-hash test whose suite had none. Any other existing test that fails means the change is wrong.

## Rulings and design notes

Controller rulings (binding):

- **R-D1 — cap eviction before phase F's reaper.** One compare-and-delete helper, `deleteSuiteTx` (Task 5), is the only suite-row delete: inside the caller's writer transaction it re-reads the row and deletes it only if it still qualifies (no current snapshot — or, for F, the listed one — and `last_requested_at` at or before the cutoff), releasing the current snapshot's pins (F only), retiring every snapshot of the suite and deleting the row; F's `cache.ReapIdleSuite` reuses it. Count, evict and insert run in the one writer transaction `CreateSuite`/`StoreFetched` already is, so concurrent creations can never exceed a cap, and no request ever runs a batched `url_path` delete: the evictee's metadata rows are left to `url_path` GC (AIDEV-NOTE on `CreateSuite`). After a committed eviction the evictee's in-memory state is purged exactly as F's reaper will purge it: the guard's `release(true)` drops the lock-map entry (`CompareAndDelete`) and the flip-gate entry (`Purge`), the cache drops its touch-coalescing entry, and the scheduler flushes a queued entry through I-4 (no row → `skipped` → done). "Under its mutex" fits the layering: the checker's `HoldSuite` takes the candidate's mutex with `TryLock` (busy → the next candidate) before the writer operation is submitted, and releases it after the commit.
- **R-D2 — adoption accepts exactly apt's first line.** apt's `pkgAcqMetaClearSig::VerifyDone` rejects an `InRelease` unless `StartsWithGPGClearTextSignature` holds (`acquire-item.cc`, line 2040 of the copy in the controller scratchpad): the first line, with trailing `' '`, `'\t'`, `'\r'` and `'\n'` stripped by `_strrstrip` (`strutl.cc`, line 240), must equal `-----BEGIN PGP SIGNED MESSAGE-----` exactly; leading whitespace or a leading blank line fails. A snapshot anchor is served to apt clients, so `requireBareClearsigned` now requires `aptpath.SniffInRelease` (the same rule) of any body that contains the marker, and returns `ErrMalformedClearsign` otherwise (Task 2). The other direction is closed too: go-crypto v1.4.1's `clearsign.Decode` refuses a marker line with trailing blanks, which apt (and RFC 4880 §6.2) accepts, so every decode of an envelope — the verifier and reconcile's `inReleasePlaintext` — goes through `aptpath.DecodableClearsign`, which hands the decoder a copy with the bare marker line (Tasks 2, 3). Pinned by `TestVerifyInline_FollowsAptsFirstLineRule`.
- **R-D3 — the flat index sniff before phase G.** `aptpath.SniffRelease` and `aptpath.SniffFlatIndex` land with full tables; `ClassifySuiteFile(suite, path)` returns `FileFlatIndex` for a root `Packages*`/`Sources*` of a suite without a `dists` segment, so the handler already routes flat indexes through the sniff, the row creation and the touch. No flat grammar is added: `SuitePath` gives no flat suite before G, so the branch is reachable only in unit tests until then.
- **R-D4 — adoption-disabled parity.** The parity test (Task 9) pins every column of the row today's first-contact `InRelease` miss creates with no adopter, as measured on the phase C tip: `last_check_at = last_success_at = last_requested_at =` the miss's stamp, the miss's `ETag`/`Last-Modified`, `inrelease_change_seen_at`/`current_snapshot_id` NULL, `check_fail_count = 0`, an empty failure record — and that no T1 fires and no other path seeds a row.
- **R-D5 — metric names.** `acu_suite_seed_rejected_total{host}` (counter: refusals that reached the writer, so at most one per suite per rejection-cache window) and `acu_suites_seed_rejected` (unlabelled gauge: the rejection cache's size). The WARN is rate-limited to one line per minute process-wide, with `suppressed`. The rejection cache holds at most 4,096 suites: at the bound it drops expired entries and, if still full, clears itself (each suite then costs one more writer operation). All documented in `docs/log-fields.md` and the handler's metrics inventory comment.

Design notes (choices the spec leaves open — the boring, bounded option, with the reason):

1. **Seed values.** Only the adoption-disabled miss of `<suite>/InRelease` stamps the row as a successful check with the miss's validators (today's seed, R-D4). Every other creation — any adopter-wired creation and every url_path-hit onboarding — leaves `last_check_at`, `last_success_at` and the validators NULL, as spec C3 says for the adopter-wired seed: the first check runs at once (no cooldown) and unconditionally, and no row ever carries one anchor's validators for another (a `Release`-created row reused by the inline check would send `Release`'s validators for `InRelease`).
2. **T1 on the miss path stays phase E's** (spec §9: "T1-on-seed"). Until E an adopter-wired row is first checked by the next hit's T1 (hits fire T1 today) or the next scheduler tick (never-checked class), at the cost of one unconditional anchor GET (spec §10.11). `TestServeHTTP_FreshnessNotTriggeredOnMiss` keeps passing.
3. **Url_path-hit onboarding seeds the NULL row in both modes** and is followed by the T1 every hit already fires, which then checks at once — the sequence today's hit produced when its T1 created the row through the old upsert. Without onboarding, I-4 would leave a row-less legacy suite unchecked forever.
4. **Creating a row also marks the suite's other cached signed anchors** (`markCachedSignedAnchors`, Task 9). Markers are written only for tracked suites, so an `InRelease` or `Release.gpg` url_path row stored while its suite had no row (refused at a cap, or cached by a pre-v7 binary for a suite without a row) has none; once a hit or miss creates the row, the hit-path invariant needs it. The body in hand is marked in the creating transaction; the other cached anchors in one follow-up writer operation whose failure is logged and healed by the startup marker pass at the next start (deviation 2).
5. **A refresh miss no longer re-stamps an existing row.** Today's upsert overwrote the check stamps and validators of a suite whose `InRelease` url_path row had been evicted; `CreateSuite` is create-only, so the next check refreshes them (one unconditional-or-stale conditional GET). The marker is still written on a refresh miss.
6. **Eviction takes at most eight candidates** (least recently requested first; a busy one is skipped) and runs only at exactly the global cap: above it — rows created before `max_suites` was lowered — one eviction cannot make room, so nothing is evicted and phase F's reaper shrinks the table. A creation that races another at the cap boundary may be refused (cached for 10 minutes); caps are never exceeded.
7. **Touch coalescing lives in the cache** (`TouchSuite`): the cache deletes suite rows (eviction now, F's reaper next), so it purges its own map; created rows are noted at creation, so the creating request's touch writes nothing. The map holds only suites whose UPDATE found a row and is swept of entries older than an hour when it reaches 4,096. A touch claims its entry before the write, so concurrent touches of one suite write once, and drops it when the write fails or finds no row, so an untracked suite never stays.
8. **Touch call sites.** Anchor serves (`InRelease`, `Release`, `Release.gpg`; status below 400 — 200, 206, 304, HEAD) from a snapshot, the url_path cache, the miss path and the snapshot-member recovery path touch their suite when it is tracked; a stale serve touches without knowing (TouchSuite is UPDATE-only: an untracked suite costs one no-op write per stale serve, a rare path); a hash-validated `.deb` serve touches every suite whose current snapshot vouched it (`DeclaredHash.SuitePath`). Flat root-index touches come with G's grammar through `ClassifySuiteFile` (R-D3).
9. **Late gate registration re-dispatches.** The serve that creates a suite's row registers with the flip barrier right after the creation (phase C carry-forward). If that registration waited in a hold, the flip may have committed a snapshot, so the request is served through the hit path instead of with the bytes it fetched.
10. **`SniffRelease`** matches a hash-block header line the way `ParseRelease` reads one: the field name with its colon alone on a line, trailing CR, space and tab ignored, case-sensitive. **`SniffFlatIndex`** follows the spec table literally: an empty uncompressed index fails; `.lzma` is the classic `5D 00 00` header.
11. **Test fixtures reach the single INSERT site through test-only helpers** (`SeedSuiteForTest`, `EnsureSuiteForTest`, `InsertTrackedCandidateForTest` in `internal/cache/testing_helpers.go`, beside the existing `…ForTest` helpers), so the migration of ~160 fixture call sites is a mechanical rename; a guard test keeps them out of production code.
12. **`ErrSuiteGone` is classified `db_failed`** (no new outcome): every deleter of a suite row holds the suite's mutex (an adoption holds it from its check to its commit), so it is a backstop, not an expected path.
13. **The per-host cap counts rows by `canonical_host` across schemes**, like the per-host admission slot.
14. **`suite_evicted`** (INFO; spec C11 names no event for it) logs each eviction: `canonical_host`, `suite_path` (the evictee), `for_host`, `for_suite`.
15. **Scheduler queue bound** (spec C4, phase B plan deviation 4): a tick enqueues due suites only while fewer than `freshness.max_suites` are queued, waiting or in flight.
16. **`handler_goroutine_panicked`** (ERROR, `where = touch`): the touch goroutine's recover (deferred minor C-24) needs an event and spec C11 names none; the T1 goroutine keeps `freshness_check_panicked`. Both go through one helper, `logGoroutinePanic`, with the stack once per `where` per process.
17. **`freshness.max_suites` is presence-sensitive**, like `adoption.max_duration`: absent means 5000; any value below 10, an explicit 0 included, fails validation (spec C11: "≥ 10").
18. **The displaced-blob WARN** (deferred minor C-25) reuses `snapshot_member_blob_missing` with `snapshot_displaced = true` rather than a new event: same condition, same operator action.
19. **`acu_freshness_unlogged_panics_total`** (deferred minor B-44; the item asks for a counter, not a log line): unlabelled, named like `acu_flip_gate_panics_total`, incremented only by the scheduler's last-resort recover, never through the logger that panicked.
20. **A shutdown before the flip barrier** (deferred minor C-20) is decided in `spawnAdoption` alone: any error once the lifetime context has ended is an abandon at shutdown (spec C7, phase C design note 18), so `freshness/adoption.go` stays untouched; a genuine failure that coincides with the shutdown is logged at INFO and its change retried after the restart.
21. **The scale test's gate** (Task 11) is a `flipgate.New` gate wired into the handler, the object `Adopter.FlipGate()` returns in production: minting suites needs no adopter, and `Len()` is read from the gate itself.
22. **Clearsigned fixtures where an outcome depends on them** (spec §7 "Fixtures": the helper "replaces the non-clearsigned `InRelease` bodies in existing handler and cmd tests (about 18 files)"): Tasks 3 and 9 convert the fixtures whose test needs a sniff-passing `InRelease` — a miss that must create a row or marker (five handler files) and reconcile's parse (four freshness files). The other plain-text bodies (snapshot anchors, url_path rows of suites the test never tracks, the cmd tests) are served exactly as before, so converting them changes no outcome; they are left, and the full suite passing unchanged is the evidence.

## Deliberate deviations from the spec

1. **The hit-path invariant covers tracked suites.** Spec C3/§7 say every `url_path` anchor row whose blob passes the sniff has a marker. Markers are written only for suites with a `suite_freshness` row (spec C3), so a sniff-passing anchor of a suite refused at a cap has none, by the spec's own rule. The invariant tested (Task 11) is: every sniff-passing `InRelease`/`Release.gpg` row of a tracked suite has a marker for its base. Task 13 amends spec C3 and §7.
2. **Row creation on a hit writes markers.** Spec C3 says the hit path writes no marker. A url_path-hit onboarding creates the row of a suite whose anchors were cached while it had none — a legacy `dists/` suite (the startup pass marks only tracked suites) or one refused at a cap — so the creating transaction marks a sniff-passing `InRelease` in hand, and `markCachedSignedAnchors` marks the suite's other cached signed anchors (design note 4). Serving an existing tracked suite's anchor still writes no marker. Task 13 amends spec C3.

## Review Focus (phase D share)

1. **Minted paths at scale** (random `dists/` codenames, `Release.gpg` and by-hash names, many hosts) must create no rows beyond the caps, no markers for untracked suites, and no growth of the lock map, the flip gate, the touch map or the rejection cache — Tasks 5, 6, 7, 10, 11.
2. **Upgrading a live v6 cache** (a `dists/` anchor cached without a suite row) must onboard the suite on its first hit, with its marker — Tasks 10, 11.
3. **Upstreams answering 200 HTML for missing files** must never create a row or a marker: every suite file is sniffed before it counts — Tasks 1, 9, 10.
4. **A failing or full writer** must never serve a signed anchor body without its marker, and must never leave the url_path row without the marker it was stored with — Tasks 6, 9.
5. **A suite deleted while it is being checked or adopted** (eviction, later the reaper) must end that work without resurrecting the row or running two checks of one suite — Tasks 4, 5, 7.

## File map

| File | Change |
|---|---|
| `internal/aptpath/sniff.go` (+ `sniff_test.go`) | `SniffRelease`, `SniffFlatIndex`, `SuiteFile`, `ClassifySuiteFile`, `DecodableClearsign` (R-D2); A-17. |
| `internal/gpg/verifier.go` (+ `failclosed_test.go`, `verifier_test.go`) | apt's first-line rule in `requireBareClearsigned`; envelopes decoded through `DecodableClearsign` (R-D2); A-50, A-51, A-52, A-53. |
| `internal/simtest/clearsign.go` (new) (+ `clearsign_test.go`) | `ClearsignedFixture`. |
| `internal/freshness/reconcile.go` (+ `reconcile_r1_test.go`, `reconcile_test.go`, `freshness_test.go`, `phase6_7_skip_repair_test.go`) | Reconcile parses only clearsigned `InRelease`; fixtures; A-32, A-33, A-34. |
| `internal/cache/suite_rows.go` (new) (+ `suite_rows_test.go`, `single_insert_test.go`, `suite_evict_test.go`, `suite_interest_test.go`) | `SuiteKey`, `SuiteSeed`, `SuiteCaps`, `CreateOutcome`, `CreateResult`, `SuiteGuard`, `CreateSuite`, `createSuiteTx`, `deleteSuiteTx`, `holdEvictionCandidate`, `SuiteExists`, `UpdateSuiteFreshness`, `ErrSuiteGone`. |
| `internal/cache/fetched_row.go` (new) (+ `fetched_row_test.go`) | `FetchedRow`, `FetchedResult`, `StoreFetched`; the FIFO test. |
| `internal/cache/suite_touch.go` (new) (+ `suite_touch_test.go`) | `TouchSuite` with coalescing, `TouchedSuites`. |
| `internal/cache/snapshot_reads.go` (new) (+ `adoption_scope_test.go`) | The read-side snapshot helpers moved out of `adoption.go` (C-15); C-10, A-18. |
| `internal/cache/queries.go`, `adoption.go`, `adoption_failure.go`, `cache.go`, `signed_seen.go`, `types.go`, `testing_helpers.go` (+ `cache_test.go`, `adoption_failure_test.go`, `anchor_form_test.go`, `signed_seen_test.go`) | `PutSuiteFreshness` removed; `putURLPathSQL`; Step 6 UPDATE-only; `DeclaredHash.SuitePath`; `touches` field; test-only seeding helpers and `MigrateToVersionForTest`; A-18, A-22, A-23, A-24, A-25, B-1–B-5, B-8, C-14. |
| `internal/freshness/freshness.go`, `suite_lock.go` (new), `scheduler.go`, `metrics.go` (+ `suite_lock_test.go`, `scheduler_test.go`, `sim_sched_test.go`, `flip_barrier_test.go`, `panic_recovery_test.go`, `freshness_test.go`) | `UpdateSuiteFreshness`; I-4 pre-check and re-validation in `Check` and `Reconcile`; `HoldSuite`; `LockCount`; queue bound; `acu_freshness_unlogged_panics_total`; A-30, A-35, A-38, B-22, B-39–B-44, B-47–B-50, C-18, C-19 (its line), C-20. |
| `internal/config/config.go` (+ `config_test.go`), `packaging/config/config.toml.default` | `freshness.max_suites`. |
| `internal/handler/handler.go`, `suite_rows.go` (new), `metrics.go`, `singleflight.go` (+ `suite_rows_test.go`, `suite_hit_test.go`, `suite_dod_test.go`, `byhash_test.go`, `byhash_displaced_test.go`, `sim_flip_test.go`) | C3 creation, markers, touches, onboarding, rejection cache, late registration, by-hash I-4; C-21 (its query half), C-22–C-27. |
| `cmd/apt-cacher-ultra/main.go` (+ `suite_caps_wiring_test.go`, `shutdown_drain_test.go`, `flip_wiring_test.go`, `legacy_audit_wiring_test.go`) | Wiring `MaxSuites` and the guard; C-28, C-30–C-35. |
| ~40 existing test files | Fixture migration (Task 4), clearsigned fixtures (Tasks 3, 9), deferred minors in touched files (Task 12). |
| `docs/configuration.md`, `docs/log-fields.md`, the spec, the overview, the three deferred files | Documentation, amendments, stamps. |

## Tasks

1. The `Release` and flat-index sniffs and the suite-file classifier (`internal/aptpath`).
2. Adoption accepts exactly apt's first line (R-D2).
3. `simtest.ClearsignedFixture`; reconcile parses only clearsigned `InRelease`.
4. One INSERT site; UPDATE-only writers; Check needs a row; the fixture migration.
5. Caps and cap eviction under the suite's mutex.
6. The miss path's writer operation, client-interest touches and the FIFO guarantee (cache).
7. I-4 re-validation in the checker; `HoldSuite`; the scheduler's queue bound.
8. `freshness.max_suites` and its wiring.
9. The handler's miss path creates rows and markers before the body.
10. The handler's hit path: onboarding, touches, by-hash for known suites.
11. Phase D DoD: minted suites at scale, `dists/` onboarding from a v6 cache, the hit-path invariant.
12. Deferred minors in the files this phase touches.
13. Documentation, spec amendments, overview, deferred files, final verification.

---

### Task 1: The `Release` and flat-index sniffs and the suite-file classifier

Spec C3 "Sniffs" (the `Release` row and the flat `Packages*`/`Sources*` row; the `InRelease` and `Release.gpg` sniffs exist since phase A) and the rule of which paths create a row, earn a marker and record client interest; ruling R-D3 (the flat branch is tested now, reachable from G). Deferred minor A-17 (`sniff_test.go`).

**Files:**
- Modify: `internal/aptpath/sniff.go` (append), `internal/aptpath/sniff_test.go`

**Interfaces:**
- Consumes: `aptpath.SniffInRelease`, `SniffReleaseGPG`, `SniffHeadBytes` (phase A).
- Produces: `const ReleaseSniffBytes = 64 << 10`; `SniffRelease(head []byte) bool`; `SniffFlatIndex(name string, head []byte) bool`; `type SuiteFile int` with `NotSuiteFile`, `FileInRelease`, `FileRelease`, `FileReleaseGPG`, `FileFlatIndex`; `ClassifySuiteFile(suite, path string) SuiteFile`; methods `(SuiteFile).CreatesRow(adopter bool) bool`, `.Marks() bool`, `.Touches() bool`, `.HeadBytes() int`, `.Sniff(path string, head []byte) bool`.

- [ ] **Step 1: Write the failing tests**

In `internal/aptpath/sniff_test.go`, replace `import "testing"` with

```go
import (
	"strings"
	"testing"
)
```

In `TestSniffReleaseGPG`'s table (deferred minor A-17), replace

```go
		{[]byte{0x87, 0x00}, false},
		{[]byte{0xc1, 0x00}, false},
```

with

```go
		{[]byte{0x87, 0x00}, false},
		{[]byte{0x8c, 0x00}, false}, // just above the old-format range (A-17)
		{[]byte{0xc1, 0x00}, false},
		{[]byte{0xc3, 0x00}, false}, // just above the new-format tag (A-17)
```

Append to the file:

```go
// Spec C3 Release sniff: a hash-block header line in the first 64 KiB.
func TestSniffRelease(t *testing.T) {
	inside := strings.Repeat("y", ReleaseSniffBytes-len("SHA256:\n")-1) + "\nSHA256:\n"
	outside := strings.Repeat("y", ReleaseSniffBytes) + "\nSHA256:\n"
	cases := []struct {
		name, head string
		want       bool
	}{
		{"SHA256 block", "Origin: Debian\nLabel: Debian\nSHA256:\n 0123 12 main/binary-amd64/Packages\n", true},
		{"MD5Sum block", "Origin: x\nMD5Sum:\n 0123 12 main/Sources\n", true},
		{"SHA1 block", "SHA1:\n 0123 12 Packages\n", true},
		{"SHA512 block, CRLF", "Origin: x\r\nSHA512:\r\n 0123 12 Packages\r\n", true},
		{"trailing blanks", "SHA256: \t\n", true},
		{"last line, no newline", "Origin: x\nSHA256:", true},
		{"header ends at 64 KiB", inside, true},
		{"header after 64 KiB", outside, false},
		{"HTML soft-404", "<html><body>Not Found</body></html>\n", false},
		{"value on the header line", "SHA256: 0123 12 Packages\n", false},
		{"indented", " SHA256:\n", false},
		{"lower case", "sha256:\n", false},
		{"no hash block", "Origin: x\nDescription: none\n", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		if got := SniffRelease([]byte(c.head)); got != c.want {
			t.Errorf("%s: SniffRelease = %v, want %v", c.name, got, c.want)
		}
	}
}

// Spec C3 flat index sniff: the compression magic for the extension, or
// plain text starting "Package:".
func TestSniffFlatIndex(t *testing.T) {
	cases := []struct {
		name, head string
		want       bool
	}{
		{"Packages", "Package: foo\nVersion: 1\n", true},
		{"Packages", "Package:foo\n", true},
		{"Sources", "Package: foo\nBinary: foo\n", true},
		{"Packages", "", false},
		{"Packages", " Package: foo\n", false},
		{"Packages", "<html>Not Found</html>", false},
		{"Packages.gz", "\x1f\x8b\x08\x00", true},
		{"Packages.gz", "Package: foo\n", false},
		{"Packages.gz", "\x1f", false},
		{"Packages.xz", "\xfd7zXZ\x00\x00", true},
		{"Packages.xz", "\x1f\x8b\x08\x00", false},
		{"Packages.bz2", "BZh91AY&SY", true},
		{"Packages.lzma", "\x5d\x00\x00\x80\x00", true},
		{"Packages.lz4", "\x04\x22\x4d\x18\x64", true},
		{"Sources.zst", "\x28\xb5\x2f\xfd\x04", true},
		{"Sources.gz", "\x1f\x8b\x08\x00", true},
		{"Packages.Z", "\x1f\x9d", false},
		{"Packages.diff/Index", "SHA1-Current: x\n", false},
		{"InRelease", "-----BEGIN PGP SIGNED MESSAGE-----\n", false},
	}
	for _, c := range cases {
		if got := SniffFlatIndex(c.name, []byte(c.head)); got != c.want {
			t.Errorf("SniffFlatIndex(%q, %q) = %v, want %v", c.name, c.head, got, c.want)
		}
	}
}

// Spec C3: what a request path is to its suite. A dists suite's root
// Packages is no flat index (R-D3: the flat branch waits for phase G's
// grammar, but the classifier knows it now).
func TestClassifySuiteFile(t *testing.T) {
	cases := []struct {
		suite, path string
		want        SuiteFile
	}{
		{"/u/dists/noble", "/u/dists/noble/InRelease", FileInRelease},
		{"/u/dists/noble", "/u/dists/noble/Release", FileRelease},
		{"/u/dists/noble", "/u/dists/noble/Release.gpg", FileReleaseGPG},
		{"/u/dists/noble", "/u/dists/noble/main/binary-amd64/Packages.gz", NotSuiteFile},
		{"/u/dists/noble", "/u/dists/noble/Packages.gz", NotSuiteFile},
		{"/u/dists/noble", "/u/dists/noble/main/InRelease", NotSuiteFile},
		{"/u/dists/noble", "/u/dists/noble", NotSuiteFile},
		{"/u/dists/noble", "/u/dists/nobleX/InRelease", NotSuiteFile},
		{"/repo/flat", "/repo/flat/InRelease", FileInRelease},
		{"/repo/flat", "/repo/flat/Packages.gz", FileFlatIndex},
		{"/repo/flat", "/repo/flat/Sources", FileFlatIndex},
		{"/repo/flat/.", "/repo/flat/./Packages.xz", FileFlatIndex},
		{"/repo/flat", "/repo/flat/Packages.diff/Index", NotSuiteFile},
		{"/repo/flat", "/repo/flat/Packages_1.0_all.deb", NotSuiteFile},
		{"", "/x/InRelease", NotSuiteFile},
	}
	for _, c := range cases {
		if got := ClassifySuiteFile(c.suite, c.path); got != c.want {
			t.Errorf("ClassifySuiteFile(%q, %q) = %d, want %d", c.suite, c.path, got, c.want)
		}
	}
}

// Spec C3: which suite files create a row (InRelease always; Release and
// flat indexes only with an adopter), earn a marker (signed anchors), and
// record client interest (all of them).
func TestSuiteFile_Rules(t *testing.T) {
	cases := []struct {
		f                                  SuiteFile
		createsP1, createsAd, marks, touch bool
		head                               int
	}{
		{NotSuiteFile, false, false, false, false, SniffHeadBytes},
		{FileInRelease, true, true, true, true, SniffHeadBytes},
		{FileRelease, false, true, false, true, ReleaseSniffBytes},
		{FileReleaseGPG, false, false, true, true, SniffHeadBytes},
		{FileFlatIndex, false, true, false, true, SniffHeadBytes},
	}
	for _, c := range cases {
		if got := c.f.CreatesRow(false); got != c.createsP1 {
			t.Errorf("%d.CreatesRow(no adopter) = %v, want %v", c.f, got, c.createsP1)
		}
		if got := c.f.CreatesRow(true); got != c.createsAd {
			t.Errorf("%d.CreatesRow(adopter) = %v, want %v", c.f, got, c.createsAd)
		}
		if got := c.f.Marks(); got != c.marks {
			t.Errorf("%d.Marks = %v, want %v", c.f, got, c.marks)
		}
		if got := c.f.Touches(); got != c.touch {
			t.Errorf("%d.Touches = %v, want %v", c.f, got, c.touch)
		}
		if got := c.f.HeadBytes(); got != c.head {
			t.Errorf("%d.HeadBytes = %d, want %d", c.f, got, c.head)
		}
	}
	sniffs := []struct {
		f    SuiteFile
		path string
		head string
	}{
		{FileInRelease, "/u/dists/s/InRelease", "-----BEGIN PGP SIGNED MESSAGE-----\n"},
		{FileRelease, "/u/dists/s/Release", "Origin: x\nSHA256:\n"},
		{FileReleaseGPG, "/u/dists/s/Release.gpg", "-----BEGIN PGP SIGNATURE-----\n"},
		{FileFlatIndex, "/repo/flat/Packages.gz", "\x1f\x8b\x08\x00"},
	}
	for _, s := range sniffs {
		if !s.f.Sniff(s.path, []byte(s.head)) {
			t.Errorf("%d.Sniff(%q) = false, want true", s.f, s.head)
		}
		if s.f.Sniff(s.path, []byte("<html>Not Found</html>")) {
			t.Errorf("%d.Sniff passed an HTML page", s.f)
		}
		if NotSuiteFile.Sniff(s.path, []byte(s.head)) {
			t.Errorf("NotSuiteFile.Sniff(%q) = true", s.head)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/aptpath/`
Expected: compile failure naming `undefined: ReleaseSniffBytes`, `SniffRelease`, `SniffFlatIndex`, `SuiteFile`, `ClassifySuiteFile`, `NotSuiteFile`, `FileInRelease`, `FileRelease`, `FileReleaseGPG`, `FileFlatIndex`.

- [ ] **Step 3: Implement**

Append to `internal/aptpath/sniff.go` (its imports, `bytes` and `strings`, already cover it):

```go
// ReleaseSniffBytes is how much of a body SniffRelease reads (spec C3:
// the first 64 KiB).
const ReleaseSniffBytes = 64 << 10

// SniffRelease reports whether head (the first bytes of a body, at most
// ReleaseSniffBytes are read) looks like a Release file: a "SHA256:",
// "SHA512:", "SHA1:" or "MD5Sum:" hash-block header line — the field name
// alone at the start of a line, trailing CR, space and tab ignored, as
// ParseRelease reads it — in the first 64 KiB (spec C3). An HTML error
// page or a plain-text stub has none.
func SniffRelease(head []byte) bool {
	if len(head) > ReleaseSniffBytes {
		head = head[:ReleaseSniffBytes]
	}
	for len(head) > 0 {
		line := head
		if i := bytes.IndexByte(head, '\n'); i >= 0 {
			line, head = head[:i], head[i+1:]
		} else {
			head = nil
		}
		switch string(bytes.TrimRight(line, "\r \t")) {
		case "SHA256:", "SHA512:", "SHA1:", "MD5Sum:":
			return true
		}
	}
	return false
}

// flatIndexMagic maps a flat Packages*/Sources* extension to its
// compression magic (spec C3 flat index sniff).
var flatIndexMagic = map[string][]byte{
	".gz":   {0x1f, 0x8b},
	".xz":   {0xfd, '7', 'z', 'X', 'Z', 0x00},
	".bz2":  {'B', 'Z', 'h'},
	".lzma": {0x5d, 0x00, 0x00},
	".lz4":  {0x04, 0x22, 0x4d, 0x18},
	".zst":  {0x28, 0xb5, 0x2f, 0xfd},
}

// SniffFlatIndex reports whether head passes the flat index sniff for the
// file named name (its last path segment, e.g. "Packages.xz"; spec C3):
// the compression magic for its extension, or for an uncompressed
// Packages/Sources plain text starting "Package:". Any other name fails.
func SniffFlatIndex(name string, head []byte) bool {
	ext, ok := flatIndexExt(name)
	if !ok {
		return false
	}
	if ext == "" {
		return bytes.HasPrefix(head, []byte("Package:"))
	}
	return bytes.HasPrefix(head, flatIndexMagic[ext])
}

// flatIndexExt returns the compression extension of a flat index file name
// (Packages or Sources, optionally with one of the flat grammar's
// extensions; "" for none).
func flatIndexExt(name string) (string, bool) {
	for _, stem := range []string{"Packages", "Sources"} {
		rest, ok := strings.CutPrefix(name, stem)
		if !ok {
			continue
		}
		if rest == "" {
			return "", true
		}
		if _, known := flatIndexMagic[rest]; known {
			return rest, true
		}
	}
	return "", false
}

// SuiteFile is what a request path is to its suite for spec C3: whether
// serving it creates the suite's row, writes its signed-seen marker, or
// records client interest.
type SuiteFile int

const (
	// NotSuiteFile: no row, no marker, no touch.
	NotSuiteFile SuiteFile = iota
	// FileInRelease is <suite>/InRelease.
	FileInRelease
	// FileRelease is <suite>/Release.
	FileRelease
	// FileReleaseGPG is <suite>/Release.gpg.
	FileReleaseGPG
	// FileFlatIndex is a flat suite's root Packages*/Sources* file. SuitePath
	// gives no flat suite before phase G, so the handler meets it only once
	// G adds the flat grammar there.
	FileFlatIndex
)

// ClassifySuiteFile reports what path is to suite, which must be
// SuitePath(path) ("" gives NotSuiteFile). A dists suite's root
// Packages*/Sources* is not a flat index.
func ClassifySuiteFile(suite, path string) SuiteFile {
	if suite == "" {
		return NotSuiteFile
	}
	name, ok := strings.CutPrefix(path, suite+"/")
	if !ok {
		return NotSuiteFile
	}
	switch name {
	case "InRelease":
		return FileInRelease
	case "Release":
		return FileRelease
	case "Release.gpg":
		return FileReleaseGPG
	}
	if _, ok := flatIndexExt(name); ok && !hasDistsSegment(suite) {
		return FileFlatIndex
	}
	return NotSuiteFile
}

// hasDistsSegment reports whether p has a "dists" segment: such a path is
// never flat (spec C2).
func hasDistsSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == "dists" {
			return true
		}
	}
	return false
}

// CreatesRow reports whether a sniff-passing serve of f creates its
// suite's row (spec C3): InRelease always; Release and flat indexes only
// with an adopter wired. Release.gpg never does.
func (f SuiteFile) CreatesRow(adopter bool) bool {
	switch f {
	case FileInRelease:
		return true
	case FileRelease, FileFlatIndex:
		return adopter
	}
	return false
}

// Marks reports whether a sniff-passing body of f is signed metadata that
// earns its suite's base a signed-seen marker (spec C3).
func (f SuiteFile) Marks() bool { return f == FileInRelease || f == FileReleaseGPG }

// Touches reports whether serving f records client interest in its suite
// (spec C3 TouchSuite): every suite file does.
func (f SuiteFile) Touches() bool { return f != NotSuiteFile }

// HeadBytes is how many leading bytes of a body f's sniff reads.
func (f SuiteFile) HeadBytes() int {
	if f == FileRelease {
		return ReleaseSniffBytes
	}
	return SniffHeadBytes
}

// Sniff reports whether head (at least HeadBytes of the body, or all of
// it) passes f's sniff (spec C3 table); NotSuiteFile never passes.
func (f SuiteFile) Sniff(path string, head []byte) bool {
	switch f {
	case FileInRelease:
		return SniffInRelease(head)
	case FileRelease:
		return SniffRelease(head)
	case FileReleaseGPG:
		return SniffReleaseGPG(head)
	case FileFlatIndex:
		return SniffFlatIndex(path[strings.LastIndexByte(path, '/')+1:], head)
	}
	return false
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/aptpath/ && go vet ./internal/aptpath/`
Expected: PASS.

- [ ] **Step 5: Mutation check (do not commit)**

One at a time, reverting each: (a) in `SniffRelease`, `switch string(line) {` → `TestSniffRelease` FAILS (`SHA512 block, CRLF: SniffRelease = false, want true`); (b) delete the `if len(head) > ReleaseSniffBytes { … }` truncation → `TestSniffRelease` FAILS (`header after 64 KiB: SniffRelease = true, want false`); (c) in `SniffFlatIndex`, `return len(head) == 0 || bytes.HasPrefix(head, []byte("Package:"))` → `TestSniffFlatIndex` FAILS (`SniffFlatIndex("Packages", "") = true, want false`); (d) in `ClassifySuiteFile`, drop `&& !hasDistsSegment(suite)` → `TestClassifySuiteFile` FAILS (`ClassifySuiteFile("/u/dists/noble", "/u/dists/noble/Packages.gz") = 4, want 0`); (e) in `CreatesRow`, return `true` for `FileRelease, FileFlatIndex` → `TestSuiteFile_Rules` FAILS (`2.CreatesRow(no adopter) = true, want false`); (f) `Marks` returns `f == FileInRelease` → `TestSuiteFile_Rules` FAILS (`3.Marks = false, want true`); (g) `.lzma` magic `{0x5d, 0x00, 0x01}` → `TestSniffFlatIndex` FAILS.

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/aptpath/
git add internal/aptpath/sniff.go internal/aptpath/sniff_test.go
git commit -m "Add the Release and flat-index sniffs and the suite-file classifier"
```

---

### Task 2: Adoption accepts exactly apt's first line (R-D2)

Ruling R-D2 (overview carry-forward "D:" item 3): `requireBareClearsigned` admitted leading whitespace that apt's first-line rule rejects, and go-crypto's decoder refuses a marker line with trailing blanks that apt accepts. After this task adoption refuses exactly what apt refuses on the first line and accepts what it accepts. Deferred minors A-50 (one begin-marker literal), A-51 (the structural guard's errors wrap `ErrMalformedClearsign`), A-52 (`failclosed_test.go`'s vacuous row check), A-53 (the detached empty-keyring message), all in files this task touches.

**Files:**
- Modify: `internal/aptpath/sniff.go` (append), `internal/aptpath/sniff_test.go`, `internal/gpg/verifier.go`, `internal/gpg/failclosed_test.go`, `internal/gpg/verifier_test.go`

**Interfaces:**
- Consumes: `aptpath.SniffInRelease` (phase A); gpg test helpers `newTestEntity`, `newKeyring`, `clearsignWith`, `detachSignWith(t, e, message, armored)`, `newSuite`, `silentLogger`, `fakeReleasePlaintext` (package `gpg`).
- Produces: `aptpath.DecodableClearsign(b []byte) []byte` (Task 3 decodes stored envelopes through it); `gpg.clearsignBeginMarker` (unexported).

- [ ] **Step 1: Write the failing tests**

Append to `internal/aptpath/sniff_test.go`:

```go
// Ruling R-D2: a marker line with trailing blanks passes apt's rule; the
// copy go-crypto decodes has the bare marker line, and nothing else moves.
func TestDecodableClearsign(t *testing.T) {
	const m = "-----BEGIN PGP SIGNED MESSAGE-----"
	cases := []struct{ in, want string }{
		{m + "\nHash: SHA256\n", m + "\nHash: SHA256\n"},
		{m + "\r\nHash: SHA256\r\n", m + "\r\nHash: SHA256\r\n"},
		{m + " \t\nHash: SHA256\n", m + "\nHash: SHA256\n"},
		{m + " \r\nHash: SHA256\r\n", m + "\nHash: SHA256\r\n"},
		{m, m},
		{m + "  ", m},
		{" " + m + "\n", " " + m + "\n"}, // fails apt's rule: left alone, refused later
		{"Origin: x\n", "Origin: x\n"},
	}
	for _, c := range cases {
		if got := string(DecodableClearsign([]byte(c.in))); got != c.want {
			t.Errorf("DecodableClearsign(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
```

In `internal/gpg/failclosed_test.go`, add `"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"` to the imports (before the `internal/cache` import). In `TestAdopter_MalformedEnvelopeCreatesNoSnapshot` (deferred minor A-52), insert after the line `suite := freshness.SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}`:

```go
	// The suite is tracked, so the closing check reads a row (deferred
	// minor A-52: it passed vacuously on ErrNotFound).
	if err := c.PutSuiteFreshness(context.Background(), cache.SuiteFreshness{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}); err != nil {
		t.Fatal(err)
	}
```

and replace its last check

```go
	if sf, err := c.GetSuiteFreshness(context.Background(), "http", "h", "/r/dists/x"); err == nil && sf.CurrentSnapshotID != nil {
		t.Fatalf("a current snapshot was created: %+v", sf)
	}
```

with

```go
	if sf, err := c.GetSuiteFreshness(context.Background(), "http", "h", "/r/dists/x"); err != nil || sf.CurrentSnapshotID != nil {
		t.Fatalf("suite row = %+v (err %v), want the row with no current snapshot", sf, err)
	}
```

(Task 4's migration renames that `PutSuiteFreshness` to `SeedSuiteForTest`.) Append to the file:

```go
// Ruling R-D2 (phase D): adoption accepts exactly what apt's first-line
// rule accepts. apt rejects an InRelease whose first line is not the
// clearsign marker — a leading blank line or leading whitespace included
// (acquire-item.cc VerifyDone → StartsWithGPGClearTextSignature, which
// strips only trailing whitespace) — so the verifier refuses it too, as
// malformed_clearsign, and the handler's sniff agrees. Trailing blanks on
// the marker line are stripped by apt and accepted here.
func TestVerifyInline_FollowsAptsFirstLineRule(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	v, err := NewVerifier(VerifierConfig{Keyring: newKeyring(t, signer), RequireSignature: true, Logger: silentLogger()})
	if err != nil {
		t.Fatal(err)
	}
	good := clearsignWith(t, signer, []byte(fakeReleasePlaintext))
	for name, prefix := range map[string]string{
		"leading newline": "\n",
		"leading CRLF":    "\r\n",
		"leading spaces":  "  ",
		"leading tab":     "\t",
		"leading BOM":     "\xef\xbb\xbf",
	} {
		body := append([]byte(prefix), good...)
		_, err := v.VerifyInline(context.Background(), newSuite(), body)
		if !errors.Is(err, ErrMalformedClearsign) || ClassifyVerifyErr(err) != "malformed_clearsign" {
			t.Errorf("%s: err = %v, want malformed_clearsign", name, err)
		}
		if aptpath.SniffInRelease(body) {
			t.Errorf("%s: the handler's sniff accepts a body adoption refuses", name)
		}
	}
	trailing := bytes.Replace(good, []byte(clearsignBeginMarker+"\n"), []byte(clearsignBeginMarker+" \t\r\n"), 1)
	if bytes.Equal(trailing, good) {
		t.Fatal("test premise: the marker line was not rewritten")
	}
	if _, err := v.VerifyInline(context.Background(), newSuite(), trailing); err != nil {
		t.Fatalf("trailing blanks on the marker line: %v, want accepted (apt strips them)", err)
	}
}

// Deferred minor A-53: the detached path names an empty keyring too.
func TestVerifyDetached_EmptyKeyringSaysSo(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	v, err := NewVerifier(VerifierConfig{Keyring: newKeyring(t), RequireSignature: false, Logger: silentLogger()})
	if err != nil {
		t.Fatal(err)
	}
	rel := []byte(fakeReleasePlaintext)
	_, err = v.VerifyDetached(context.Background(), newSuite(), rel, detachSignWith(t, signer, rel, true))
	if !errors.Is(err, ErrNoUsableSignature) || !strings.Contains(err.Error(), "keyring is empty") {
		t.Fatalf("err = %v, want ErrNoUsableSignature naming the empty keyring", err)
	}
}
```

In `internal/gpg/verifier_test.go` (deferred minor A-51), in `TestVerifier_RejectsPrefixGarbage` insert after the `error doesn't mention BEGIN marker` check, and in `TestVerifier_RejectsSuffixGarbage` after the `error doesn't mention END marker` check:

```go
	if ClassifyVerifyErr(err) != "malformed_clearsign" { // deferred minor A-51
		t.Fatalf("reason = %q, want malformed_clearsign", ClassifyVerifyErr(err))
	}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/aptpath/ ./internal/gpg/`
Expected: compile failure naming `undefined: DecodableClearsign` (aptpath) and `undefined: clearsignBeginMarker` (gpg). (With Step 3's `clearsignBeginMarker` alone in place, the red state shows: `TestVerifyInline_FollowsAptsFirstLineRule` fails with `leading newline: err = <nil>, want malformed_clearsign` and with `trailing blanks on the marker line: InRelease clearsign envelope is malformed, want accepted`; both garbage tests fail with `reason = "crypto_verify_failed", want malformed_clearsign`.)

- [ ] **Step 3: Implement**

Append to `internal/aptpath/sniff.go`:

```go
// DecodableClearsign returns b ready for go-crypto's clearsign decoder:
// when b's first line passes SniffInRelease but carries trailing blanks —
// which apt's rule ignores and RFC 4880 §6.2 allows after an armor header
// line, but go-crypto v1.4.1 refuses — it returns a copy whose first line
// is exactly the marker; otherwise b itself. Nothing else changes, so
// neither does the signed text.
//
// AIDEV-NOTE: ruling R-D2 (phase D): adoption accepts exactly apt's first
// line. Every decode of an InRelease envelope (the verifier, reconcile's
// inReleasePlaintext) goes through this, so a body apt accepts is never
// refused, nor later found undecodable, for its marker line alone.
func DecodableClearsign(b []byte) []byte {
	if !SniffInRelease(b) {
		return b
	}
	end := bytes.IndexByte(b, '\n')
	if end < 0 {
		end = len(b)
	}
	if end == len(clearsignBegin) || (end == len(clearsignBegin)+1 && b[end-1] == '\r') {
		return b
	}
	return append([]byte(clearsignBegin), b[end:]...)
}
```

In `internal/gpg/verifier.go`, add `"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"` to the imports (before `internal/freshness`). In `decodeClearsigned`, replace its doc's first two lines

```go
// decodeClearsigned is clearsign.Decode with a panic turned into
// errClearsignPanicked.
```

with

```go
// decodeClearsigned is clearsign.Decode with a panic turned into
// errClearsignPanicked. It decodes aptpath.DecodableClearsign(b), so a
// marker line with trailing blanks — accepted by apt — decodes too
// (ruling R-D2).
```

and its `block, _ = clearsign.Decode(b)` with `block, _ = clearsign.Decode(aptpath.DecodableClearsign(b))`. In `VerifyInline`, replace `if bytes.Contains(inRelease, []byte("-----BEGIN PGP SIGNED MESSAGE-----")) {` with `if bytes.Contains(inRelease, []byte(clearsignBeginMarker)) {`. Replace the whole `requireBareClearsigned` function and its doc comment with:

```go
// clearsignBeginMarker opens a clearsigned message (deferred minor A-50:
// one literal).
const clearsignBeginMarker = "-----BEGIN PGP SIGNED MESSAGE-----"

// requireBareClearsigned enforces apt's shape on a body that carries the
// clearsign marker: its FIRST line is exactly the marker (trailing CR,
// space and tab ignored — aptpath.SniffInRelease, apt's
// StartsWithGPGClearTextSignature), and only whitespace follows the END
// line. clearsign.Decode silently strips bytes around the message; we
// reject them to keep "verified bytes == stored bytes (modulo trailing
// newlines)" — SPEC2 §7.5 step 2. Both failures are
// ErrMalformedClearsign (deferred minor A-51).
//
// AIDEV-NOTE: ruling R-D2 (phase D). A snapshot anchor is served to apt
// clients, which reject an InRelease whose first line is not the marker
// (leading whitespace or a blank line included), so adoption accepts
// exactly what apt accepts — the same rule the handler's sniff applies
// before a body earns a suite row or a signed-seen marker.
//
// A body without the marker returns nil: the decode step rejects it with
// ErrMissingSignature.
func requireBareClearsigned(b []byte) error {
	const endSig = "-----END PGP SIGNATURE-----"
	if !bytes.Contains(b, []byte(clearsignBeginMarker)) {
		return nil
	}
	if !aptpath.SniffInRelease(b) {
		return fmt.Errorf("%w: the first line is not the BEGIN marker (apt rejects it)", ErrMalformedClearsign)
	}
	endIdx := bytes.Index(b, []byte(endSig))
	if endIdx < 0 {
		// Missing END marker — let clearsign.Decode produce the real
		// error. Don't fabricate one here.
		return nil
	}
	if !isWhitespaceOnly(b[endIdx+len(endSig):]) {
		return fmt.Errorf("%w: extraneous bytes after the END marker", ErrMalformedClearsign)
	}
	return nil
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/aptpath/ ./internal/gpg/`
Expected: PASS (every existing gpg test included: `TestVerifier_AcceptsTrailingNewlines` and the fail-closed matrix are unchanged).

- [ ] **Step 5: Mutation check (do not commit)**

One at a time, reverting each: (a) in `requireBareClearsigned`, `if idx := bytes.Index(b, []byte(clearsignBeginMarker)); !isWhitespaceOnly(b[:idx]) {` in place of the `SniffInRelease` test → `TestVerifyInline_FollowsAptsFirstLineRule` FAILS (`leading newline: err = <nil>, want malformed_clearsign`); (b) its first-line error as `errors.New("the first line is not the BEGIN marker (apt rejects it)")` → `TestVerifyInline_FollowsAptsFirstLineRule` and `TestVerifier_RejectsPrefixGarbage` FAIL; (c) the END-tail error unwrapped the same way → `TestVerifier_RejectsSuffixGarbage` FAILS (`reason = "crypto_verify_failed"`); (d) `clearsign.Decode(b)` in `decodeClearsigned` → `TestVerifyInline_FollowsAptsFirstLineRule` FAILS (`trailing blanks on the marker line: … malformed`); (e) in `DecodableClearsign`, `if end == len(clearsignBegin) {` → `TestDecodableClearsign` FAILS on the CRLF case.

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/aptpath/ internal/gpg/
git add internal/aptpath/sniff.go internal/aptpath/sniff_test.go internal/gpg/verifier.go internal/gpg/failclosed_test.go internal/gpg/verifier_test.go
git commit -m "Accept exactly apt's InRelease first line in adoption"
```

---

### Task 3: `simtest.ClearsignedFixture`; reconcile parses only clearsigned `InRelease`

Spec §7 "Fixtures" (a shared `clearsignedFixture()` helper) and the overview's "D:" line and Global Constraint: reconcile's plain-text fallback in `inReleasePlaintext` is removed and `TestReparseSnapshotRelease_PlainTextInReleaseStillParses` becomes `_PlainTextInReleaseIsAnError`. The decode goes through `aptpath.DecodableClearsign` (ruling R-D2). Only the six freshness fixtures that relied on the fallback change here (measured: `TestReparseSnapshotRelease_PlainTextInReleaseStillParses`, `TestReconcileSnapshot_HealsBinaryAllInPlace`, `TestReconcileSnapshot_GateAndForce`, `TestReconcileMetric_Increments`, `TestCheck_UnchangedTick_ReconcilesDegraded`, `TestCheck_HealthySuiteAsksForNoRepairSlot`; plus `TestReconcileSnapshot_RejectsCorruptedAnchor`, whose corrupt bytes would otherwise be refused for being plain text before its hash check is reached); the handler and cmd fixtures change in Task 9, where the handler starts sniffing. Deferred minors A-32 (`reconcile.go` wording and a line over 100 columns), A-33 (`clearsignForTest` uses Ed25519) and A-34 (a CRLF-envelope and a header-block injection case), all in files this task touches.

**Files:**
- Create: `internal/simtest/clearsign.go`, `internal/simtest/clearsign_test.go`
- Modify: `internal/freshness/reconcile.go`, `internal/freshness/reconcile_r1_test.go`, `internal/freshness/reconcile_test.go`, `internal/freshness/capacity_test.go`, `internal/freshness/freshness_test.go`

**Interfaces:**
- Consumes: `aptpath.DecodableClearsign`, `aptpath.SniffInRelease` (Task 2, phase A); freshness test helpers `newAdoptionTestEnv`, `storeInlineSnapshot`, `clearsignForTest`, `makeRelease`, `newRetryRig` (package `freshness`).
- Produces: `simtest.ClearsignedFixture(text string) []byte` (Tasks 4, 9 and 11 use it).

- [ ] **Step 1: Write the failing tests**

Create `internal/simtest/clearsign_test.go`:

```go
package simtest

import (
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp/clearsign"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
)

// The fixture is what spec C3's sniff and reconcile's decoder take for a
// clearsigned InRelease, and its plaintext is the text it wraps.
func TestClearsignedFixture(t *testing.T) {
	cases := []struct{ text, plain string }{
		{"Origin: x\nSHA256:\n 0123 1 main/Packages\n", "Origin: x\nSHA256:\n 0123 1 main/Packages"},
		{"Origin: x\nSuite: y", "Origin: x\nSuite: y"},
	}
	for _, c := range cases {
		f := ClearsignedFixture(c.text)
		if !aptpath.SniffInRelease(f) {
			t.Fatalf("%q: fails the InRelease sniff", f)
		}
		b, _ := clearsign.Decode(f)
		if b == nil {
			t.Fatalf("%q: does not decode", f)
		}
		if string(b.Plaintext) != c.plain {
			t.Fatalf("plaintext = %q, want %q", b.Plaintext, c.plain)
		}
	}
}
```

In `internal/freshness/reconcile_r1_test.go`: add `"github.com/ProtonMail/go-crypto/openpgp/packet"` to the imports (after `…/openpgp/clearsign`). Replace the whole `TestReparseSnapshotRelease_PlainTextInReleaseStillParses` function with:

```go
// Phase D removed reconcile's plain-text fallback: a stored InRelease
// without a clearsign marker is an error, never parsed (the fixtures that
// needed it use simtest.ClearsignedFixture).
func TestReparseSnapshotRelease_PlainTextInReleaseIsAnError(t *testing.T) {
	env := newAdoptionTestEnv(t)
	body := "Origin: Test\nSHA256:\n " + strings.Repeat("a", 64) + " 10 main/binary-amd64/Packages\n"
	id := storeInlineSnapshot(t, env, []byte(body))
	got, err := env.adopter.reparseSnapshotRelease(context.Background(), id)
	if err == nil || len(got) != 0 || !strings.Contains(err.Error(), "not clearsigned") {
		t.Fatalf("got %+v, %v; want a not-clearsigned error and no members", got, err)
	}
}
```

Replace the doc comment of `TestReparseSnapshotRelease_IgnoresInjectionWithLeadingWhitespace`

```go
// R-1: the verifier accepts whitespace before the BEGIN marker, so the
// clearsign detection must find a marker anywhere (never HasPrefix), and
// the injection must be ignored for a whitespace-prefixed envelope too.
```

with

```go
// R-1: until phase D the verifier accepted whitespace before the BEGIN
// marker (ruling R-D2 now refuses it), so snapshots adopted earlier can
// hold such an envelope: the clearsign detection must find a marker
// anywhere (never HasPrefix), and the injection must be ignored for a
// whitespace-prefixed envelope too.
```

In `clearsignForTest` (deferred minor A-33) replace `ent, err := openpgp.NewEntity("t", "", "t@example.invalid", nil)` with

```go
	// Ed25519: an RSA key per call is slow under -race (deferred minor A-33).
	ent, err := openpgp.NewEntity("t", "", "t@example.invalid", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
```

Insert before the `// storeInlineSnapshot writes blob to the pool and records an adopted` comment:

```go
// Deferred minor A-34: an injection after the armor CRC of a CRLF envelope
// is ignored like one in an LF envelope.
func TestReparseSnapshotRelease_IgnoresInjectionInACRLFEnvelope(t *testing.T) {
	env := newAdoptionTestEnv(t)
	genuine := "Origin: Test\nSuite: noble\nSHA256:\n " + strings.Repeat("a", 64) + " 10 main/binary-amd64/Packages\n"
	signed := clearsignForTest(t, genuine)
	evil := "=AAAA\nSHA256:\n " + strings.Repeat("b", 64) + " 10 main/binary-evilarch/Packages\n"
	idx := bytes.LastIndex(signed, []byte("-----END PGP SIGNATURE-----"))
	tampered := append(append(append([]byte{}, signed[:idx]...), evil...), signed[idx:]...)
	id := storeInlineSnapshot(t, env, bytes.ReplaceAll(tampered, []byte("\n"), []byte("\r\n")))
	got, err := env.adopter.reparseSnapshotRelease(context.Background(), id)
	if err != nil || len(got) != 1 || got[0].Path != "main/binary-amd64/Packages" {
		t.Fatalf("declared = %+v, err=%v; want only the signed member", got, err)
	}
}

// Deferred minor A-34: a declaration placed in the signed message's own
// header block (beside "Hash:") makes the envelope undecodable — an error,
// never a parse of the raw bytes.
func TestReparseSnapshotRelease_HeaderBlockInjectionFailsClosed(t *testing.T) {
	env := newAdoptionTestEnv(t)
	genuine := "Origin: Test\nSuite: noble\nSHA256:\n " + strings.Repeat("a", 64) + " 10 main/binary-amd64/Packages\n"
	signed := clearsignForTest(t, genuine)
	hashLine := bytes.Index(signed, []byte("Hash: "))
	lineEnd := hashLine + bytes.IndexByte(signed[hashLine:], '\n') + 1
	tampered := append(append(append([]byte{}, signed[:lineEnd]...),
		"SHA256: "+strings.Repeat("d", 64)+" 10 main/binary-evilarch/Packages\n"...), signed[lineEnd:]...)
	id := storeInlineSnapshot(t, env, tampered)
	got, err := env.adopter.reparseSnapshotRelease(context.Background(), id)
	if err == nil || len(got) != 0 {
		t.Fatalf("got %d members, err=%v; want a decode error and no members", len(got), err)
	}
}

// Ruling R-D2: a stored envelope whose marker line carries trailing blanks
// — apt accepts it, go-crypto's decoder alone would not — still decodes.
func TestReparseSnapshotRelease_DecodesAptsMarkerLine(t *testing.T) {
	env := newAdoptionTestEnv(t)
	genuine := "Origin: Test\nSuite: noble\nSHA256:\n " + strings.Repeat("a", 64) + " 10 main/binary-amd64/Packages\n"
	signed := clearsignForTest(t, genuine)
	const marker = "-----BEGIN PGP SIGNED MESSAGE-----"
	blanks := bytes.Replace(signed, []byte(marker+"\n"), []byte(marker+" \t\n"), 1)
	id := storeInlineSnapshot(t, env, blanks)
	got, err := env.adopter.reparseSnapshotRelease(context.Background(), id)
	if err != nil || len(got) != 1 || got[0].Path != "main/binary-amd64/Packages" {
		t.Fatalf("declared = %+v, err=%v; want the signed member", got, err)
	}
}
```

In `internal/freshness/reconcile_test.go`, add `"github.com/linsomniac/apt-cacher-ultra/internal/simtest"` to the imports (after `internal/metrics`). In `adoptCompleteSnapshot`, insert after its `releaseText, _ := makeRelease(…)` statement:

```go
	releaseText = simtest.ClearsignedFixture(string(releaseText)) // reconcile parses only clearsigned InRelease
```

In `TestReconcileSnapshot_RejectsCorruptedAnchor`, insert after its `corrupt, _ := makeRelease(…)` statement:

```go
	// Clearsigned, so only the hash check can refuse it.
	corrupt = simtest.ClearsignedFixture(string(corrupt))
```

In `internal/freshness/capacity_test.go`, `TestCheck_HealthySuiteAsksForNoRepairSlot`, replace `r.up.set(r.release("v1", 0))` with

```go
	// Clearsigned: the unchanged check reconciles, which parses only
	// clearsigned InRelease (phase D).
	r.up.set(simtest.ClearsignedFixture(string(r.release("v1", 0))))
```

In `internal/freshness/freshness_test.go`, add `"github.com/linsomniac/apt-cacher-ultra/internal/simtest"` to the imports (after `internal/hostsem`), and in `TestCheck_UnchangedTick_ReconcilesDegraded` insert after its `releaseText, _ := makeRelease(…)` statement:

```go
	releaseText = simtest.ClearsignedFixture(string(releaseText)) // reconcile parses only clearsigned InRelease
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/simtest/ ./internal/freshness/ -run 'TestClearsignedFixture|TestReparseSnapshotRelease'`
Expected: compile failure naming `undefined: ClearsignedFixture` (simtest) and `undefined: simtest.ClearsignedFixture` (freshness). (With Step 3's simtest file alone, the red state shows: `TestReparseSnapshotRelease_PlainTextInReleaseIsAnError` fails with `got [{Path:main/binary-amd64/Packages …}], <nil>; want a not-clearsigned error and no members`, and `TestReparseSnapshotRelease_DecodesAptsMarkerLine` with `stored InRelease is not a valid clearsigned message`.)

- [ ] **Step 3: Implement**

Create `internal/simtest/clearsign.go`:

```go
package simtest

import "strings"

// fakeSignatureBlock is a structurally valid armored signature block whose
// body is no real signature. go-crypto's clearsign.Decode accepts it (it
// reads the armor lazily); nothing verifies it cryptographically.
const fakeSignatureBlock = "-----BEGIN PGP SIGNATURE-----\n\nwsBcBAABCAAQBQJmAAAAAAkQAAAAAAAAAAAAAAA=\n=AAAA\n-----END PGP SIGNATURE-----\n"

// ClearsignedFixture wraps text — the Release text of an InRelease — in a
// clearsign envelope (spec §7 "Fixtures"): the first line is the marker
// apt requires, so the handler's sniff (spec C3) counts the body as signed
// metadata; clearsign.Decode returns text, less its final newline, as the
// plaintext, so reconcile parses it; a pass-through verifier hands the
// whole envelope to ParseRelease, which skips the armor lines. The
// signature is a fixed fake: tests that verify signatures sign for real.
// No line of text may begin with "-" (it would need dash-escaping).
func ClearsignedFixture(text string) []byte {
	return []byte("-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\n" +
		strings.TrimSuffix(text, "\n") + "\n" + fakeSignatureBlock)
}
```

In `internal/freshness/reconcile.go`, add `"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"` to the imports (before `internal/cache`). In `reparseSnapshotRelease`'s doc (deferred minor A-32), replace

```go
// exact bytes adoption verified. Intact is not the same as signed — an
// InRelease envelope also carries unsigned regions (armor headers, lines
// after the armor CRC), so for InRelease inReleasePlaintext limits the
// parse to the signed region (R-1). A detached Release is covered whole by
// its Release.gpg and is parsed as-is.
```

with

```go
// exact bytes adoption verified. Intact is not the same as signed: an
// InRelease envelope also carries regions outside the signature (armor
// headers, lines after the armor CRC), so for InRelease
// inReleasePlaintext limits the parse to the signed plaintext (R-1). A
// detached Release is covered whole by its Release.gpg and is parsed
// as-is.
```

Replace the `AIDEV-NOTE: R-1.` paragraph of `inReleasePlaintext`'s doc and the body down to the `clearsign.Decode` call — from `// AIDEV-NOTE: R-1. Never ParseRelease the raw envelope: lines placed` through `	block, _ := clearsign.Decode(blob)` — with:

```go
// AIDEV-NOTE: R-1. Never ParseRelease the raw envelope: lines placed
// after a forged "=CRC" line inside the signature armor, or as
// colon-bearing armor headers, are part of the stored bytes but were never
// signed (both verify with go-crypto v1.4.1). A body without a clearsign
// marker is an error since phase D (the plain-text fallback served only
// test fixtures; VerifyInline never adopts plain text, and the startup
// audit quarantines legacy plain-text snapshots — one it deferred fails
// its reconcile here, closed, until a later start audits it). The marker
// test is bytes.Contains, never HasPrefix: snapshots adopted before phase
// D may hold whitespace before the marker (ruling R-D2 now refuses that at
// adoption). A decode failure is an error, never a raw parse; the decode
// sees aptpath.DecodableClearsign(blob) (R-D2) and runs under recover():
// a stored blob cannot trigger I-1, but reconcile must never crash the
// daemon.
func inReleasePlaintext(blob []byte) (text []byte, err error) {
	if !bytes.Contains(blob, []byte("-----BEGIN PGP SIGNED MESSAGE-----")) {
		return nil, errors.New("reconcile: stored InRelease is not clearsigned")
	}
	defer func() {
		if r := recover(); r != nil {
			text, err = nil, fmt.Errorf("reconcile: clearsign decode panicked: %v", r)
		}
	}()
	block, _ := clearsign.Decode(aptpath.DecodableClearsign(blob))
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/simtest/ ./internal/freshness/`
Expected: PASS (the legacy-audit tests included: `classifyLegacyInline` reaches `inReleasePlaintext` only for bodies that carry the marker).

- [ ] **Step 5: Mutation check (do not commit)**

One at a time, reverting each: (a) in `inReleasePlaintext`, `return blob, nil` for a marker-less body → `TestReparseSnapshotRelease_PlainTextInReleaseIsAnError` FAILS; (b) decode `blob` itself (`block, _ := clearsign.Decode(blob)` plus `_ = aptpath.DecodableClearsign` to keep the import) → `TestReparseSnapshotRelease_DecodesAptsMarkerLine` FAILS (`stored InRelease is not a valid clearsigned message`); (c) in `reparseSnapshotRelease`, `got != mem.BlobHash && false` → `TestReconcileSnapshot_RejectsCorruptedAnchor` FAILS (before this task's fixture change this mutant survived: the plain-text corrupt bytes were refused anyway).

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/simtest/ internal/freshness/reconcile.go internal/freshness/reconcile_r1_test.go internal/freshness/reconcile_test.go internal/freshness/capacity_test.go internal/freshness/freshness_test.go
git add internal/simtest/clearsign.go internal/simtest/clearsign_test.go internal/freshness/reconcile.go internal/freshness/reconcile_r1_test.go internal/freshness/reconcile_test.go internal/freshness/capacity_test.go internal/freshness/freshness_test.go
git commit -m "Parse only clearsigned InRelease in reconcile; add the clearsigned test fixture"
```

---

### Task 4: One INSERT site; UPDATE-only writers; Check needs a row; the fixture migration

Spec C3 "one creation choke point" (`cache.CreateSuite`, its per-host and global caps; cap eviction is Task 5), "every other writer is UPDATE-only" (`PutSuiteFreshness` becomes `UpdateSuiteFreshness`; `CommitAdoption` Step 6 becomes an UPDATE that rolls the flip back with `cache.ErrSuiteGone` when the row is gone), and I-4's checker side: `Check` reads the row before it creates a lock-map entry and never creates a row (Task 7 adds the re-validation after `TryLock`). The handler's adoption-disabled `InRelease` seed keeps its values and moves to `CreateSuite` (Task 9 moves it into the miss path's writer operation). Design notes 5 (a refresh miss no longer re-stamps the row), 11 (test-only seeding helpers), 12 (`ErrSuiteGone` is classified `db_failed`: the adopter already wraps every `CommitAdoption` error as `"%w: commit: %w", ErrAdoptionDBFailed` in `freshness/adoption.go`, so no freshness change) and 13 (the per-host cap counts across schemes). Deferred minors, all in files this task touches: B-1 (`queries.go`), B-2 and B-4 (`cache/adoption_failure.go`), B-3 (`cache/adoption_failure_test.go`), B-5 (`cache_test.go`), B-8 (moot: Step 6 has no INSERT branch left; Task 13 deletes it), A-22 (`GetSuiteSnapshot` in `cache/adoption.go`, test in `anchor_form_test.go`), A-23 (`CommitAdoption` Step 7a, test in `signed_seen_test.go`), A-30 (`freshness_test.go`), A-35 and A-38 (`panic_recovery_test.go`). B-22 (`freshness_test.go`'s fake `RecordAdoptionFailure`) was checked: since 7877898 the fake writes nothing for an unknown suite and returns nil, exactly like production, so nothing changes and Task 13 deletes it.

Intermediate state, by design: until Tasks 9 and 10 land, rows are created only by the `InRelease`-miss seed (as today, minus the re-stamp), so a suite first seen through a hit or a detached anchor is neither checked (I-4) nor adoptable (Step 6). The phase ships whole; every existing test below passes at every task boundary.

**Files:**
- Create: `internal/cache/suite_rows.go`, `internal/cache/suite_rows_test.go`, `internal/cache/single_insert_test.go`
- Modify: `internal/cache/queries.go` (delete `PutSuiteFreshness`; B-1), `internal/cache/adoption.go` (Step 6 and its doc), `internal/cache/adoption_failure.go` (a comment; B-2; B-4), `internal/cache/types.go` (two comments), `internal/cache/testing_helpers.go` (three test-only helpers), `internal/freshness/freshness.go` (`Cache.UpdateSuiteFreshness`; the I-4 pre-check; `checkLocked` and `bumpFailedCheck` never create), `internal/handler/handler.go` (the seed through `CreateSuite`)
- Replace: `internal/cache/suite_interest_test.go`
- Modify tests: `internal/cache/adoption_failure_test.go`, `internal/cache/cache_test.go` (drift file: edit by hand, never `gofmt -w`), `internal/cache/anchor_form_test.go`, `internal/cache/signed_seen_test.go`, `internal/freshness/freshness_test.go`, `internal/freshness/panic_recovery_test.go`
- Migrate (Step 5): the 25 and 30 test files the two scripts print, and the hand edits listed there (`adoption_test.go`, `freshness_test.go`, `flip_barrier_test.go`, `panic_recovery_test.go`, `capacity_test.go`, `phase6_7_skip_repair_test.go`, `sim_sched_test.go`, `chaos_snapshot_race_test.go`, `flip_stack_test.go`, `sim_flip_test.go`, `phase2_detached_chaos_test.go`, `phase2_detached_gpg_forgery_test.go`)

**Interfaces:**
- Consumes: `aptpath.MarkerBase(suite string) string`; in `internal/cache`: `markSignedSeen(ctx, tx, scheme, host, base string, at int64) error`, `submitWrite`, `nowUnix`, test helpers `openCache`, `seedBlob`, `stubNow`, `putSuite`, `getSuite`, `sampleFailure`, `assertRecord`, `assertNoRecord`, `afSuite` (`adoption_failure_test.go`), `commitCandidate` (`flip_state_test.go`); in `internal/freshness`: test helpers `newFakeCache`, `hashOf`, `newTestFetcher`, `discardLogger`, `checkOpts`, `resultSkipped`.
- Produces (later tasks use these exact names):
  - `cache.SuiteKey`, `cache.SuiteSeed`, `cache.SuiteCaps`, `cache.CreateOutcome` (`SuiteCreated`, `SuiteExists`, `SuiteRejectedGlobal`, `SuiteRejectedHost`; `String()` = `created`, `exists`, `global_cap`, `host_cap`; `Rejected() bool`), `cache.CreateResult{Outcome; Evicted *SuiteKey}`, `cache.SuiteGuard`, `cache.ErrSuiteGone` (block below).
  - `func (c *Cache) CreateSuite(ctx context.Context, seed SuiteSeed, caps SuiteCaps, guard SuiteGuard) (CreateResult, error)` — `guard` is unused until Task 5.
  - `func createSuiteTx(ctx context.Context, tx *sql.Tx, seed SuiteSeed, caps SuiteCaps) (CreateResult, error)` — Task 5 adds the eviction parameters, Task 6's `StoreFetched` calls it.
  - `func (c *Cache) UpdateSuiteFreshness(ctx context.Context, s SuiteFreshness) error`; `freshness.Cache` declares it in place of `PutSuiteFreshness`.
  - Test-only (`testing_helpers.go`): `(*Cache).SeedSuiteForTest(ctx, s SuiteFreshness) error`, `(*Cache).EnsureSuiteForTest(ctx, scheme, host, suitePath string) error`, `(*Cache).InsertTrackedCandidateForTest(ctx, sc SnapshotCandidate) (int64, bool, error)`.
  - Test helpers: `derefI64(*int64) int64`, `derefStr(*string) string` (`suite_rows_test.go`); `productionGoFiles`, `stringLiterals` (`single_insert_test.go`); `trackSuite(t testing.TB, c *cache.Cache, suite SuiteRef)` (`freshness/adoption_test.go`); `(*fakeCache).track(scheme, host, suitePath string)` (`freshness_test.go`); `(*flipStack).track(suite string)` (`handler/flip_stack_test.go`).

- [ ] **Step 1: Write the failing tests**

Create `internal/cache/suite_rows_test.go`:

```go
package cache

import (
	"context"
	"errors"
	"testing"
)

// Spec C3: CreateSuite creates an absent row with the seed's stamps and
// the creation's interest stamp, and never touches an existing one.
func TestCreateSuite_CreatesOnlyAnAbsentRow(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: afSuite}
	check, etag := int64(1_800_000_100), "e1"
	res, err := c.CreateSuite(ctx, SuiteSeed{SuiteKey: key, LastCheckAt: &check, LastSuccessAt: &check,
		InReleaseETag: &etag, RequestedAt: 1_800_000_000}, SuiteCaps{}, nil)
	if err != nil || res.Outcome != SuiteCreated || res.Evicted != nil {
		t.Fatalf("CreateSuite = %+v, %v; want a creation", res, err)
	}
	s := getSuite(t, c)
	if derefI64(s.LastCheckAt) != check || derefI64(s.LastSuccessAt) != check || derefStr(s.InReleaseETag) != etag ||
		s.InReleaseLastMod != nil || derefI64(s.LastRequestedAt) != 1_800_000_000 || s.CurrentSnapshotID != nil ||
		s.InReleaseChangeSeenAt != nil || s.CheckFailCount != 0 {
		t.Fatalf("row = %+v, want the seed's stamps", s)
	}
	assertNoRecord(t, "created row", s)
	other := int64(1)
	res, err = c.CreateSuite(ctx, SuiteSeed{SuiteKey: key, LastCheckAt: &other, RequestedAt: 1}, SuiteCaps{}, nil)
	if err != nil || res.Outcome != SuiteExists {
		t.Fatalf("second CreateSuite = %+v, %v; want exists", res, err)
	}
	if s := getSuite(t, c); derefI64(s.LastCheckAt) != check || derefI64(s.LastRequestedAt) != 1_800_000_000 {
		t.Fatalf("an existing row was overwritten: %+v", s)
	}
}

// Spec C3 caps: max_suites/10 rows per canonical host — http and https of
// a host count together — and max_suites in all. A refusal writes nothing;
// zero caps are unbounded.
func TestCreateSuite_EnforcesTheCaps(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	create := func(scheme, host, suite string, caps SuiteCaps) CreateOutcome {
		t.Helper()
		// Freshly requested rows: none is idle enough to evict (Task 5).
		res, err := c.CreateSuite(ctx, SuiteSeed{SuiteKey: SuiteKey{CanonicalScheme: scheme, CanonicalHost: host, SuitePath: suite},
			RequestedAt: nowUnix()}, caps, nil)
		if err != nil {
			t.Fatal(err)
		}
		return res.Outcome
	}
	caps := SuiteCaps{Global: 4, PerHost: 2}
	steps := []struct {
		scheme, host, suite string
		want                CreateOutcome
	}{
		{"http", "a", "/1", SuiteCreated},
		{"https", "a", "/2", SuiteCreated},
		{"http", "a", "/3", SuiteRejectedHost},
		{"http", "a", "/1", SuiteExists},
		{"http", "b", "/1", SuiteCreated},
		{"http", "c", "/1", SuiteCreated},
		{"http", "d", "/1", SuiteRejectedGlobal},
	}
	for i, s := range steps {
		if got := create(s.scheme, s.host, s.suite, caps); got != s.want {
			t.Fatalf("step %d (%s://%s%s): %v, want %v", i, s.scheme, s.host, s.suite, got, s.want)
		}
	}
	if st, err := c.GetSuiteStats(ctx); err != nil || st.Tracked != 4 {
		t.Fatalf("tracked = %+v (%v), want 4 rows", st, err)
	}
	if got := create("http", "d", "/1", SuiteCaps{}); got != SuiteCreated {
		t.Fatalf("uncapped creation = %v, want created", got)
	}
}

// Spec C3: a creation whose request served sniff-passing signed metadata
// marks the suite's base in the creating transaction ("/repo/." and
// "/repo" share one); a creation without it, an existing row and a refusal
// mark nothing.
func TestCreateSuite_MarksSignedSeenInTheCreatingTransaction(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	create := func(host, suite string, signedAt int64, caps SuiteCaps) {
		t.Helper()
		if _, err := c.CreateSuite(ctx, SuiteSeed{SuiteKey: SuiteKey{CanonicalScheme: "http", CanonicalHost: host, SuitePath: suite},
			RequestedAt: 5, SignedSeenAt: signedAt}, caps, nil); err != nil {
			t.Fatal(err)
		}
	}
	marked := func(host, base string) bool {
		t.Helper()
		_, ok, err := c.SignedSeen(ctx, "http", host, base)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	create("h", "/repo/.", 7, SuiteCaps{})
	if at, ok, err := c.SignedSeen(ctx, "http", "h", "/repo"); err != nil || !ok || at != 7 {
		t.Fatalf("marker = %d %v %v, want 7 for base /repo", at, ok, err)
	}
	create("h", "/r/dists/plain", 0, SuiteCaps{})
	if marked("h", "/r/dists/plain") {
		t.Fatal("a creation without signed metadata wrote a marker")
	}
	create("h", "/r/dists/plain", 9, SuiteCaps{})
	if marked("h", "/r/dists/plain") {
		t.Fatal("finding an existing row wrote a marker")
	}
	create("h2", "/s", 9, SuiteCaps{Global: 1})
	if marked("h2", "/s") {
		t.Fatal("a refused creation wrote a marker")
	}
}

// Spec C3: UpdateSuiteFreshness is UPDATE-only — it never creates a row —
// and writes a check's columns only: not current_snapshot_id, not
// last_requested_at, not the failure record.
func TestUpdateSuiteFreshness_IsUpdateOnly(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	check := int64(100)
	if err := c.UpdateSuiteFreshness(ctx, SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: afSuite,
		LastCheckAt: &check}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetSuiteFreshness(ctx, "http", "h", afSuite); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateSuiteFreshness created a row: %v", err)
	}
	if _, err := c.CreateSuite(ctx, SuiteSeed{SuiteKey: SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: afSuite},
		RequestedAt: 1_800_000_000}, SuiteCaps{}, nil); err != nil {
		t.Fatal(err)
	}
	id := commitCandidate(t, c, SnapshotCandidate{}, "update-only snapshot")
	if err := c.RecordAdoptionFailure(ctx, "http", "h", afSuite, sampleFailure()); err != nil {
		t.Fatal(err)
	}
	etag, seen, other := "e2", int64(300), int64(7)
	if err := c.UpdateSuiteFreshness(ctx, SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: afSuite,
		LastCheckAt: &check, LastSuccessAt: &check, InReleaseETag: &etag, InReleaseChangeSeenAt: &seen,
		CheckFailCount: 3, CurrentSnapshotID: &other, LastRequestedAt: &other}); err != nil {
		t.Fatal(err)
	}
	s := getSuite(t, c)
	if derefI64(s.LastCheckAt) != 100 || derefI64(s.LastSuccessAt) != 100 || derefStr(s.InReleaseETag) != "e2" ||
		derefI64(s.InReleaseChangeSeenAt) != 300 || s.CheckFailCount != 3 {
		t.Fatalf("check columns not written: %+v", s)
	}
	if derefI64(s.CurrentSnapshotID) != id || derefI64(s.LastRequestedAt) != 1_800_000_000 {
		t.Fatalf("UpdateSuiteFreshness wrote current_snapshot_id or last_requested_at: %+v", s)
	}
	assertRecord(t, "after UpdateSuiteFreshness", s, sampleFailure())
}

// Spec C3: CommitAdoption Step 6 is UPDATE-only. A suite without a row is
// refused with ErrSuiteGone and the flip rolls back whole: no row, no
// member, no pin, no marker, and the candidate stays a candidate.
func TestCommitAdoption_RefusesAGoneSuite(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	adopted := seedBlob(t, c, "inrelease bytes (gone suite)")
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/gone", InReleaseHash: &adopted})
	if err != nil {
		t.Fatal(err)
	}
	err = c.CommitAdoption(ctx, id, []SnapshotMember{
		{SnapshotID: id, Path: "InRelease", BlobHash: adopted, DeclaredSHA256: adopted},
	}, nil, nil, nil, false)
	if !errors.Is(err, ErrSuiteGone) {
		t.Fatalf("CommitAdoption = %v, want ErrSuiteGone", err)
	}
	if _, err := c.GetSuiteFreshness(ctx, "http", "h", "/r/dists/gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the commit created a suite row: %v", err)
	}
	if snap, err := c.GetSuiteSnapshot(ctx, id); err != nil || snap.AdoptedAt != nil {
		t.Fatalf("candidate = %+v (%v), want it unadopted", snap, err)
	}
	if ms, err := c.ListSnapshotMembers(ctx, id); err != nil || len(ms) != 0 {
		t.Fatalf("members = %v (%v), want none", ms, err)
	}
	if b, err := c.GetBlob(ctx, adopted); err != nil || b.RefCount != 0 {
		t.Fatalf("blob = %+v (%v), want refcount 0", b, err)
	}
	if _, ok, err := c.SignedSeen(ctx, "http", "h", "/r/dists/gone"); err != nil || ok {
		t.Fatalf("marker written (%v)", err)
	}
}

func derefI64(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

func derefStr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}
```

Create `internal/cache/single_insert_test.go` (it parses production sources; comments never count, only string literals):

```go
package cache

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// insertSuiteFreshnessRE matches a statement that inserts into
// suite_freshness: INSERT (OR IGNORE / OR REPLACE …) or REPLACE INTO,
// across line breaks, any case.
var insertSuiteFreshnessRE = regexp.MustCompile(`(?is)\b(?:INSERT(?:\s+OR\s+\w+)?|REPLACE)\s+INTO\s+suite_freshness\b`)

// productionGoFiles calls fn for every non-test .go file of the module
// (internal/cache is two levels below its root).
func productionGoFiles(t *testing.T, fn func(path string, src []byte)) {
	t.Helper()
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fn(filepath.ToSlash(p), b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// stringLiterals calls fn with every string literal of a Go source file
// and the name of the function declaration that holds it. Comments are
// not literals, so prose that mentions an INSERT never counts.
func stringLiterals(t *testing.T, path string, src []byte, fn func(funcName, lit string)) {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range f.Decls {
		name := "(top level)"
		if fd, ok := d.(*ast.FuncDecl); ok {
			name = fd.Name.Name
		}
		ast.Inspect(d, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				fn(name, lit.Value)
			}
			return true
		})
	}
}

// Spec C3: CreateSuite's createSuiteTx holds the only INSERT INTO
// suite_freshness in production code, so no other writer can create — or
// resurrect — a suite row.
func TestSuiteFreshnessHasOneInsertSite(t *testing.T) {
	var sites []string
	productionGoFiles(t, func(p string, src []byte) {
		stringLiterals(t, p, src, func(funcName, lit string) {
			for range insertSuiteFreshnessRE.FindAllStringIndex(lit, -1) {
				sites = append(sites, p+":"+funcName)
			}
		})
	})
	const want = "../../internal/cache/suite_rows.go:createSuiteTx"
	if len(sites) != 1 || sites[0] != want {
		t.Fatalf("INSERT INTO suite_freshness sites = %v, want exactly [%s]", sites, want)
	}
}

// Spec C3: only the handler creates suite rows. Outside internal/cache no
// production code but internal/handler calls CreateSuite or StoreFetched,
// and no production code calls a test-only seeding helper.
func TestOnlyTheHandlerCreatesSuiteRows(t *testing.T) {
	creators := map[string]bool{"CreateSuite": true, "StoreFetched": true}
	seeders := map[string]bool{"SeedSuiteForTest": true, "EnsureSuiteForTest": true, "InsertTrackedCandidateForTest": true}
	productionGoFiles(t, func(p string, src []byte) {
		inCache := strings.HasPrefix(p, "../../internal/cache/")
		inHandler := strings.HasPrefix(p, "../../internal/handler/")
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, p, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				name = fn.Sel.Name
			case *ast.Ident:
				name = fn.Name
			}
			switch {
			case creators[name] && !inCache && !inHandler:
				t.Errorf("%s calls %s: only internal/handler creates suite rows", fset.Position(call.Pos()), name)
			case seeders[name] && (!inCache || !strings.HasSuffix(p, "/testing_helpers.go")):
				t.Errorf("%s calls the test-only %s", fset.Position(call.Pos()), name)
			}
			return true
		})
	})
}
```

Replace the whole of `internal/cache/suite_interest_test.go` (its two tests pinned the upserts this task removes) with:

```go
package cache

import (
	"context"
	"testing"
)

// A suite row's client-interest stamp is set at its creation (phase F
// reaps by it) and kept by the check writer and the adoption commit: only
// the creation and TouchSuite write it (spec C3).
func TestSuiteInterestStamp_SetAtCreationKeptByWriters(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	if _, err := c.CreateSuite(ctx, SuiteSeed{SuiteKey: SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: afSuite},
		RequestedAt: 1_800_000_000}, SuiteCaps{}, nil); err != nil {
		t.Fatal(err)
	}
	defer stubNow(t, 1_800_009_999)()
	later := int64(1_800_009_999)
	if err := c.UpdateSuiteFreshness(ctx, SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: afSuite,
		LastCheckAt: &later}); err != nil {
		t.Fatal(err)
	}
	commitCandidate(t, c, SnapshotCandidate{}, "interest stamp")
	if s := getSuite(t, c); derefI64(s.LastRequestedAt) != 1_800_000_000 {
		t.Fatalf("last_requested_at = %d, want 1800000000 (set at creation, kept)", derefI64(s.LastRequestedAt))
	}
}
```

In `internal/cache/adoption_failure_test.go`, rename `TestPutSuiteFreshness_WritesStreakNeverRecord` to `TestUpdateSuiteFreshness_WritesStreakNeverRecord`, change its comment's first word to `UpdateSuiteFreshness`, replace its second `putSuite(t, c, SuiteFreshness{CheckFailCount: 5})` with

```go
	if err := c.UpdateSuiteFreshness(ctx, SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: afSuite,
		CheckFailCount: 5}); err != nil {
		t.Fatal(err)
	}
```

and its `"after PutSuiteFreshness"` with `"after UpdateSuiteFreshness"`. In `TestRescheduleFailingAdoptions_SkipsRefusalsAndPanics` (deferred minor B-3) add `"/e": "run_failed"` to the `reasons` map, replace the `RescheduleFailingAdoptions` call and its check with

```go
	// B-3: offset is drawn per row, so a counting source gives each
	// rescheduled row its own offset (a hoisted call would give both 10).
	var calls int64
	n, err := c.RescheduleFailingAdoptions(ctx, 5_000, func() int64 { calls++; return calls * 10 })
	if err != nil || n != 2 {
		t.Fatalf("RescheduleFailingAdoptions = %d, %v; want 2, nil", n, err)
	}
```

and the `/a` assertion with

```go
	a, e := next("/a"), next("/e")
	if a == nil || e == nil || min(*a, *e) != 5_010 || max(*a, *e) != 5_020 {
		t.Fatalf("/a, /e next = %d, %d; want 5010 and 5020 in either order", derefI64(a), derefI64(e))
	}
```

In `internal/cache/cache_test.go` (by hand): rename `TestCommitAdoption_PreservesExistingSuiteFreshnessColumns` to `TestCommitAdoption_KeepsCheckColumnsAdoptsCandidateValidators` (deferred minor B-5: the body asserts the validators are replaced). Rename `TestPutSuiteFreshness_PreservesCurrentSnapshotID` to `TestUpdateSuiteFreshness_PreservesCurrentSnapshotID`; in it, replace the comment's first sentence with `// Freshness checks call UpdateSuiteFreshness on every probe. The` / `// Phase 2 adoption pointer must survive those writes — otherwise` / `// every freshness tick would silently un-adopt the suite.`, the comment `// Now do a Phase-1-style freshness write on the same suite.` with `// Now do a freshness check's write on the same suite.`, the call after it `c.PutSuiteFreshness(ctx, SuiteFreshness{` with `c.UpdateSuiteFreshness(ctx, SuiteFreshness{`, and `"PutSuiteFreshness clobbered current_snapshot_id` with `"UpdateSuiteFreshness clobbered current_snapshot_id`.

In `internal/cache/anchor_form_test.go`, `TestGetSuiteSnapshot_ReturnsFormAndLifecycleFields` (deferred minor A-22), append after the detached snapshot's check:

```go
	// A-22: an InRelease candidate with no explicit form is inline.
	inl := hA
	id3, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/z", InReleaseHash: &inl})
	if err != nil {
		t.Fatal(err)
	}
	s3, err := c.GetSuiteSnapshot(ctx, id3)
	if err != nil {
		t.Fatal(err)
	}
	if s3.Form != FormInline || s3.SingleAnchorHash == nil || *s3.SingleAnchorHash != hA || s3.ReleaseHash != nil {
		t.Fatalf("inline snapshot = %+v", s3)
	}
```

In `internal/cache/signed_seen_test.go`, `TestCommitAdoption_SignedFormsRecordSignedSeen` (deferred minor A-23), replace the final loop with

```go
	// A-23: a flat suite "/flat/." is marked at its base "/flat".
	flat := seedBlob(t, c, "flat inline anchor (signed-seen)")
	commit("/flat/.", SnapshotCandidate{InReleaseHash: &flat},
		[]SnapshotMember{{Path: "InRelease", BlobHash: flat, DeclaredSHA256: flat}})
	for base, want := range map[string]bool{"/r/dists/inline": true, "/r/dists/detached": true, "/r/dists/unsigned": false,
		"/flat": true, "/flat/.": false} {
		at, ok, err := c.SignedSeen(ctx, "http", "h", base)
		if err != nil || ok != want || (ok && at != 1_800_000_000) {
			t.Errorf("%s: marker ok=%v at=%d err=%v, want ok=%v at the commit time", base, ok, at, err, want)
		}
	}
```

Append to `internal/freshness/freshness_test.go`:

```go
// I-4 (spec C3): a suite without a row is not checked — no upstream
// request, no row created, no lock-map entry — however its T1 fires.
func TestCheck_UntrackedSuiteIsNotChecked(t *testing.T) {
	fc := newFakeCache()
	bh := hashOf([]byte("x"))
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()
	fc.putURL(cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1", Path: "/dists/minted/InRelease",
		BlobHash: &bh, UpstreamURL: srv.URL + "/dists/minted/InRelease", IsMetadata: true,
	})
	c, err := New(Config{Cache: fc, Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8), Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if rep := c.check(context.Background(), SuiteRef{CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
		SuitePath: "/dists/minted"}, checkOpts{bypassCooldown: true}); rep.result != resultSkipped {
		t.Fatalf("result = %q, want %q", rep.result, resultSkipped)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("upstream requests = %d, want 0", n)
	}
	if _, ok := fc.suite("http", "127.0.0.1", "/dists/minted"); ok {
		t.Fatal("the check created a suite row")
	}
	locks := 0
	c.locks.Range(func(any, any) bool { locks++; return true })
	if locks != 0 {
		t.Fatalf("lock map holds %d entries, want 0", locks)
	}
}
```

Deferred minor A-30, also in `freshness_test.go`: the four checker tests that open a real cache (`TestCheck_AdopterInvokedOnChange`, `TestCheck_DetachedKnownSuite_AdoptsOnReleaseChange`, `TestCheck_DetachedFallback_OnInRelease404`, `TestCheck_DetachedFallback_OnMissingInReleaseURLRow`) log nothing: in each, `cache.Open(context.Background(), dir, nil)` becomes `cache.Open(context.Background(), dir, discardLogger()) // A-30`, and the `NewAdopter(AdoptionConfig{…})` literal gains `Logger: discardLogger(),` after `HostLimiter: hostsem.New(8),`.

In `internal/freshness/panic_recovery_test.go`, `TestCheck_RepairPanicIsRecovered`: both `NewAdopter` literals (`good` and `panicky`) gain `Logger: discardLogger()` (deferred minor A-35), and the final `TryLock` check unlocks what it took (A-38):

```go
	if mu := v.(*sync.Mutex); !mu.TryLock() {
		t.Fatal("suite mutex leaked by the panicking repair goroutine")
	} else {
		mu.Unlock()
	}
```

- [ ] **Step 2: Run to verify failure**

Run: `go vet ./internal/cache/; go test ./internal/freshness/ -run TestCheck_UntrackedSuiteIsNotChecked`
Expected: the cache test package does not compile (first error `c.UpdateSuiteFreshness undefined`; `CreateSuite`, `SuiteSeed`, `SuiteKey`, `SuiteCaps`, `ErrSuiteGone` and the outcomes are undefined too); `TestCheck_UntrackedSuiteIsNotChecked` FAILS with `result = "not_modified", want "skipped"`.

- [ ] **Step 3: Implement the choke point and the UPDATE-only writers**

Create `internal/cache/suite_rows.go` starting with this block (the contract later tasks build on):

```go
package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
)

// Spec C3: suite rows have one creation choke point. createSuiteTx holds
// the only INSERT INTO suite_freshness in production code (pinned by
// TestSuiteFreshnessHasOneInsertSite); every other writer is UPDATE-only.

// ErrSuiteGone is returned by CommitAdoption when the suite row no longer
// exists: the flip is rolled back and nothing is written (spec C3).
//
// AIDEV-NOTE: unreachable while every deleter of a suite row holds the
// suite's mutex (cap eviction through a SuiteGuard; phase F's reaper and
// POST /forget) — an adoption holds it from its check to its commit. It
// stays as the backstop for a guard-less deleter: the commit rolls back
// instead of resurrecting the row.
var ErrSuiteGone = errors.New("cache: suite row is gone")

// SuiteKey names one suite.
type SuiteKey struct {
	CanonicalScheme string
	CanonicalHost   string
	SuitePath       string
}

// SuiteSeed is the row CreateSuite inserts (spec C3).
type SuiteSeed struct {
	SuiteKey
	// LastCheckAt, LastSuccessAt and the validators are set only by the
	// adoption-disabled InRelease miss (today's seed: the miss counts as a
	// successful check). Every other creation leaves them NULL, so the
	// suite's first check runs at once and unconditionally.
	LastCheckAt, LastSuccessAt      *int64
	InReleaseETag, InReleaseLastMod *string
	// RequestedAt is the row's client-interest stamp (last_requested_at):
	// the creating request's wall-clock second.
	RequestedAt int64
	// SignedSeenAt, when non-zero, records the signed-seen marker for the
	// suite's base in the creating transaction: the creating request served
	// sniff-passing signed metadata.
	SignedSeenAt int64
}

// SuiteCaps bounds row creation (spec C3): Global is freshness.max_suites,
// PerHost is max_suites/10 rows per canonical host. Zero is unbounded.
type SuiteCaps struct {
	Global  int
	PerHost int
}

// CreateOutcome is what CreateSuite did.
type CreateOutcome int

const (
	// SuiteCreated: the row was inserted.
	SuiteCreated CreateOutcome = iota + 1
	// SuiteExists: the row was already there; nothing was written.
	SuiteExists
	// SuiteRejectedGlobal: freshness.max_suites rows exist and no idle
	// suite could be evicted.
	SuiteRejectedGlobal
	// SuiteRejectedHost: the canonical host already has max_suites/10 rows.
	SuiteRejectedHost
)

// String is the outcome's log value.
func (o CreateOutcome) String() string {
	switch o {
	case SuiteCreated:
		return "created"
	case SuiteExists:
		return "exists"
	case SuiteRejectedGlobal:
		return "global_cap"
	case SuiteRejectedHost:
		return "host_cap"
	}
	return "none"
}

// Rejected reports whether the outcome is a cap rejection.
func (o CreateOutcome) Rejected() bool { return o == SuiteRejectedGlobal || o == SuiteRejectedHost }

// CreateResult reports a creation and the suite evicted to make room.
type CreateResult struct {
	Outcome CreateOutcome
	Evicted *SuiteKey
}

// SuiteGuard lets cap eviction hold a candidate's in-memory suite mutex
// (spec C3: eviction runs under the suite's mutex) without internal/cache
// importing internal/freshness; *freshness.Checker implements it.
// HoldSuite must not block: ok is false when the suite is busy (a check or
// an adoption holds its mutex). release must be called exactly once;
// release(true) — the row was deleted — also drops the suite's in-memory
// state (lock-map entry, flip-gate entry).
type SuiteGuard interface {
	HoldSuite(key SuiteKey) (release func(deleted bool), ok bool)
}
```

then add, in the same file:

- `CreateSuite` with the doc comment "CreateSuite is spec C3's single creation choke point, for a suite the caller found without a row (the handler's url_path-hit onboarding). It creates the row only if absent (SuiteExists otherwise), within caps: it refuses at the per-host cap, and at the global cap until cap eviction (for which guard holds the evicted suite's mutex) is wired. Count and insert run in one writer transaction, so concurrent creations never exceed a cap. With seed.SignedSeenAt set, a created row's base is marked signed-seen in the same transaction." Body: one `submitWrite` operation that begins a transaction, calls `createSuiteTx`, commits, and stores the result in a local only after the commit; the result is read only after `submitWrite` returned nil (on a ctx/close path the operation may still run — the `InsertCandidateSnapshot` rule).
- `createSuiteTx`, with the doc and AIDEV-NOTE

```go
// createSuiteTx inserts seed's row unless it exists, enforcing caps inside
// the caller's writer transaction. It holds the only INSERT INTO
// suite_freshness in production code (spec C3).
//
// AIDEV-NOTE: keep this the single INSERT site
// (TestSuiteFreshnessHasOneInsertSite). Everything else that writes
// suite_freshness is UPDATE-only, so no writer can resurrect a row that
// eviction (or phase F's reaper) deleted.
```

  In order: the row exists (`SELECT 1 … WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`) → `SuiteExists`, nothing written; `caps.PerHost > 0` and `SELECT COUNT(*) FROM suite_freshness WHERE canonical_host = ?` ≥ `caps.PerHost` → `SuiteRejectedHost`; `caps.Global > 0` and `SELECT COUNT(*) FROM suite_freshness` ≥ `caps.Global` → `SuiteRejectedGlobal`; otherwise

```sql
INSERT INTO suite_freshness
  (canonical_scheme, canonical_host, suite_path,
   last_check_at, last_success_at, inrelease_etag, inrelease_lastmod, last_requested_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
```

  with the seed's fields (`RequestedAt` last), then, if `seed.SignedSeenAt != 0`, `markSignedSeen(ctx, tx, scheme, host, aptpath.MarkerBase(seed.SuitePath), seed.SignedSeenAt)`, and `SuiteCreated`. Errors are wrapped `CreateSuite: read row: %w`, `CreateSuite: count host suites: %w`, `CreateSuite: count suites: %w`, `CreateSuite: insert: %w`, `CreateSuite: %w`.
- `UpdateSuiteFreshness`, one `submitWrite` operation running

```sql
UPDATE suite_freshness
   SET last_check_at            = ?,
       last_success_at          = ?,
       inrelease_etag           = ?,
       inrelease_lastmod        = ?,
       inrelease_change_seen_at = ?,
       check_fail_count         = ?
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?
```

  (0 rows is not an error; errors wrapped `UpdateSuiteFreshness: %w`), with this doc, which carries `PutSuiteFreshness`'s AIDEV-NOTE over:

```go
// UpdateSuiteFreshness writes a check's result to an existing suite row:
// last_check_at, last_success_at, the validators, inrelease_change_seen_at
// and check_fail_count. UPDATE-only (spec C3): a suite without a row is
// left alone (0 rows is not an error). It never writes current_snapshot_id
// (CommitAdoption's flip owns it).
//
// AIDEV-NOTE: last_requested_at (v7) is the suite's client-interest
// stamp. This writer never writes it (createSuiteTx sets it, TouchSuite
// updates it), so a scheduler write cannot keep an idle suite alive.
// check_fail_count (spec C4) is written on every call: each check result
// sets or resets the streak in the same statement that bumps
// last_check_at. The adoption failure record (adoption_fail_*) is never
// written here (spec C7). (Carried over from PutSuiteFreshness.)
```

In `internal/cache/queries.go`, delete `PutSuiteFreshness` (its AIDEV-NOTE now lives on `UpdateSuiteFreshness`). Deferred minor B-1: replace the `suiteFreshnessColumns` AIDEV-NOTE (`// AIDEV-NOTE: keep in lockstep with scanDest. Every column is nullable or` … `// following fields silently instead of failing.`) with

```go
// AIDEV-NOTE: keep in lockstep with scanDest, position by position. A
// column added to one list only fails Scan loudly (the counts differ); the
// silent hazard is a reorder or a same-position edit in one list, which
// scans a column into another field of a compatible type (deferred minor
// B-1).
```

In `internal/cache/adoption.go`, `CommitAdoption`: in the doc's step list, item 6 becomes `//  6. UPDATE suite_freshness.current_snapshot_id to snapshotID, set the` / `//     row's validators to the candidate's, and clear` / `//     inrelease_change_seen_at and the adoption failure record (spec C4,` / `//     C7). UPDATE-only (spec C3): no row rolls everything back with` / `//     ErrSuiteGone.`; the Step 5 comment becomes `// Step 5: read prior current_snapshot_id from suite_freshness. A` / `// missing row leaves prior NULL here and fails Step 6.`; and Step 6 — from its `// Step 6: upsert the pointer …` comment through the `INSERT … ON CONFLICT` statement's error return — becomes:

```go
		// Step 6: flip the pointer and make the row describe the adopted
		// anchor (spec C4 "Validators"): its validators become the
		// candidate's, the pending-change stamp and the adoption failure
		// record are cleared (spec C4, C7). Check and success stamps,
		// check_fail_count and last_requested_at stay as they are.
		//
		// AIDEV-NOTE: UPDATE-only (spec C3). A suite whose row is gone
		// rolls the whole flip back with ErrSuiteGone: an adoption never
		// resurrects a suite that cap eviction (or phase F's reaper)
		// deleted, and createSuiteTx stays the only INSERT.
		res, err := tx.ExecContext(ctx, `
UPDATE suite_freshness
   SET current_snapshot_id      = ?,
       inrelease_etag           = ?,
       inrelease_lastmod        = ?,
       inrelease_change_seen_at = NULL,
       `+clearAdoptionFailureSet+`
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`,
			snapshotID, candETag, candLastMod, scheme, host, suite)
		if err != nil {
			return fmt.Errorf("CommitAdoption: flip pointer: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("CommitAdoption: flip pointer: %w", err)
		} else if n == 0 {
			return fmt.Errorf("%w: %s://%s%s", ErrSuiteGone, scheme, host, suite)
		}
```

In `internal/cache/adoption_failure.go`: `clearAdoptionFailureSet`'s doc says `CommitAdoption's flip` / `// appends it to its UPDATE, so a successful adoption clears the record in` / `// the same transaction that makes the snapshot current.`; deferred minor B-2 appends to `RescheduleFailingAdoptions`'s doc `// offset runs on the writer goroutine, possibly after this call returned` / `// on a cancelled context, so it must be safe to call from another` / `// goroutine (math/rand/v2's top-level functions are; deferred minor B-2).`; deferred minor B-4 changes the select's `AND adoption_fail_reason NOT IN (` to `AND COALESCE(adoption_fail_reason, '') NOT IN (`. In `internal/cache/types.go`, the two comments naming `PutSuiteFreshness` (`CheckFailCount`'s and the failure-record AIDEV-NOTE's) name `UpdateSuiteFreshness`.

Append to `internal/cache/testing_helpers.go` (beside the existing `…ForTest` helpers; each doc ends `TEST-ONLY.`, and `SeedSuiteForTest`'s says `never call from non-test code (TestOnlyTheHandlerCreatesSuiteRows)`):

- `SeedSuiteForTest(ctx context.Context, s SuiteFreshness) error` — the upsert fixtures relied on: in one writer transaction, `createSuiteTx(ctx, tx, SuiteSeed{SuiteKey: key, RequestedAt: nowUnix()}, SuiteCaps{})`, then the `UpdateSuiteFreshness` UPDATE of `s`'s six check columns.
- `EnsureSuiteForTest(ctx context.Context, scheme, host, suitePath string) error` — `c.CreateSuite(ctx, SuiteSeed{SuiteKey: …, RequestedAt: nowUnix()}, SuiteCaps{}, nil)`, result discarded.
- `InsertTrackedCandidateForTest(ctx context.Context, sc SnapshotCandidate) (int64, bool, error)` — `EnsureSuiteForTest` for `sc`'s suite, then `c.InsertCandidateSnapshot(ctx, sc)`.

In `internal/freshness/freshness.go`: in the `Cache` interface, replace `PutSuiteFreshness(ctx context.Context, s cache.SuiteFreshness) error` with

```go
	// UpdateSuiteFreshness writes a check's result to an existing row
	// (UPDATE-only, spec C3: a vanished row is left alone).
	UpdateSuiteFreshness(ctx context.Context, s cache.SuiteFreshness) error
```

and rename the six `c.cache.PutSuiteFreshness(` calls to `c.cache.UpdateSuiteFreshness(`. In `check`, directly after `key := suiteKey(scheme, host, suitePath)`:

```go
	// I-4 (spec C3): only a suite with a row is checked, and only it gets
	// a lock-map entry; a minted path's T1 costs one indexed read.
	if !c.suiteRowExists(ctx, ref) {
		return checkReport{result: resultSkipped}
	}
```

and after `check`:

```go
// suiteRowExists reports whether the suite has a suite_freshness row
// (I-4: the checker works only on tracked suites). A read error counts as
// "no", logged: the check is skipped and the next trigger tries again.
func (c *Checker) suiteRowExists(ctx context.Context, ref SuiteRef) bool {
	_, err := c.cache.GetSuiteFreshness(ctx, ref.CanonicalScheme, ref.CanonicalHost, ref.SuitePath)
	if err != nil && !errors.Is(err, cache.ErrNotFound) {
		c.logger.Warn("freshness: read suite row failed",
			"err", err, "canonical_host", ref.CanonicalHost, "suite_path", ref.SuitePath)
	}
	return err == nil
}
```

In `checkLocked`, the `errors.Is(err, cache.ErrNotFound)` case no longer builds a fresh row: it is `// The row vanished after check's pre-read (I-4): nothing to check` / `// and nothing to write.` and `return` (the outcome stays `skipped`). In `bumpFailedCheck`, the same case is `return // the row is gone: nothing to bump (UPDATE-only, spec C3)`, and its `// AIDEV-NOTE: phase D turns the Put into the UPDATE-only writer.` becomes `// AIDEV-NOTE: UPDATE-only since phase D (spec C3): a row deleted since the` / `// panic records nothing, and nothing here may create one.`

In `internal/handler/handler.go`, the `InRelease`-miss seed builds a `cache.SuiteSeed` (the same `LastCheckAt`/`LastSuccessAt` = `&now`, the miss's `ETag`/`Last-Modified`, plus `RequestedAt: now`) and calls `h.cache.CreateSuite(dbCtx, seed, cache.SuiteCaps{}, nil)` (result discarded; the WARN on error unchanged); append to the comment above it `CreateSuite` / `// is create-only (spec C3): a refresh miss leaves an existing row as` / `// it is.`

- [ ] **Step 4: Run the new cache tests**

Run: `go build ./... && go vet ./internal/cache/`
Expected: the build succeeds; vet fails in the existing test files only (first error `c.PutSuiteFreshness undefined`); Step 5 migrates them.

- [ ] **Step 5: Migrate the fixtures**

Save the two scripts outside the repository (say `/tmp/m1.py` and `/tmp/m2.py`) and run them from the repository root; neither is committed. First, every `PutSuiteFreshness` in a test file becomes `SeedSuiteForTest` (the fake's two messages about the checker's own write become `UpdateSuiteFreshness`) — `python3 -I /tmp/m1.py .`:

```python
"""Phase D fixture migration, step 1: every PutSuiteFreshness in a test file
(calls, fake methods, failure messages, comments) becomes SeedSuiteForTest,
except the two freshness_test.go messages about the checker's own write,
which is UpdateSuiteFreshness now."""
import os
import re
import sys

root = sys.argv[1]
checker_write = {
    '"PutSuiteFreshness was called; want skip without write"': '"UpdateSuiteFreshness was called; want skip without write"',
    '"PutSuiteFreshness called inside cooldown"': '"UpdateSuiteFreshness called inside cooldown"',
}
word = re.compile(r'\bPutSuiteFreshness\b')
changed = {}
for dp, dn, fn in os.walk(root):
    dn[:] = [d for d in dn if not d.startswith('.')]
    for f in fn:
        if not f.endswith('_test.go'):
            continue
        p = os.path.join(dp, f)
        s = open(p).read()
        n = 0
        for old, new in checker_write.items():
            n += s.count(old)
            s = s.replace(old, new)
        s, k = word.subn('SeedSuiteForTest', s)
        n += k
        if n:
            open(p, 'w').write(s)
            changed[os.path.relpath(p, root)] = n
for k in sorted(changed):
    print(changed[k], k)
print('total', sum(changed.values()), 'in', len(changed), 'files')
```

Expected last line: `total 65 in 25 files`. Second, every test candidate gets a suite row first, except in the two files whose tests need a row-less candidate — `python3 -I /tmp/m2.py . internal/cache/anchor_form_test.go internal/cache/suite_rows_test.go`:

```python
"""Phase D fixture migration, step 2: test candidates get a suite row first."""
import os
import sys

root = sys.argv[1]
skip = set(sys.argv[2:])
changed = {}
for dp, dn, fn in os.walk(root):
    dn[:] = [d for d in dn if not d.startswith('.')]
    for f in fn:
        if not f.endswith('_test.go'):
            continue
        p = os.path.join(dp, f)
        rel = os.path.relpath(p, root)
        if rel in skip:
            continue
        s = open(p).read()
        n = s.count('.InsertCandidateSnapshot(')
        if n:
            s = s.replace('.InsertCandidateSnapshot(', '.InsertTrackedCandidateForTest(')
            open(p, 'w').write(s)
            changed[rel] = n
for k in sorted(changed):
    print(changed[k], k)
print('total', sum(changed.values()), 'in', len(changed), 'files')
```

Expected last line: `total 116 in 30 files`.

Then by hand. In `internal/freshness/freshness_test.go`, replace the fake's (now renamed) `SeedSuiteForTest` method with these three:

```go
// SeedSuiteForTest stores s whole, creating the suite: the fixture
// upsert, like cache.(*Cache).SeedSuiteForTest.
func (f *fakeCache) SeedSuiteForTest(ctx context.Context, s cache.SuiteFreshness) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.suites[keyOf(s.CanonicalScheme, s.CanonicalHost, s.SuitePath)] = s
	return nil
}

// track gives a suite an empty row (spec C3: the checker checks only
// tracked suites).
func (f *fakeCache) track(scheme, host, suitePath string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := keyOf(scheme, host, suitePath)
	if _, ok := f.suites[k]; !ok {
		f.suites[k] = cache.SuiteFreshness{CanonicalScheme: scheme, CanonicalHost: host, SuitePath: suitePath}
	}
}

// UpdateSuiteFreshness mirrors the production writer: UPDATE-only (an
// unknown suite is left alone) and only the check columns.
func (f *fakeCache) UpdateSuiteFreshness(ctx context.Context, s cache.SuiteFreshness) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putCount.Add(1)
	if f.putErr != nil {
		return f.putErr
	}
	k := keyOf(s.CanonicalScheme, s.CanonicalHost, s.SuitePath)
	cur, ok := f.suites[k]
	if !ok {
		return nil
	}
	cur.LastCheckAt, cur.LastSuccessAt = s.LastCheckAt, s.LastSuccessAt
	cur.InReleaseETag, cur.InReleaseLastMod = s.InReleaseETag, s.InReleaseLastMod
	cur.InReleaseChangeSeenAt, cur.CheckFailCount = s.InReleaseChangeSeenAt, s.CheckFailCount
	f.suites[k] = cur
	return nil
}
```

In `internal/freshness/adoption_test.go`, `newAdoptionTestEnv` builds `env := &adoptionTestEnv{…}` (the existing literal), calls `trackSuite(t, c, env.suite)` with the comment `// Spec C3: CommitAdoption is UPDATE-only, so an adopted suite needs a` / `// row — in production the handler created it before any check ran.`, and returns `env`; below it add

```go
// trackSuite gives suite a suite_freshness row: spec C3 lets only the
// handler create rows, and CommitAdoption (UPDATE-only) and the checker
// (I-4) need one.
func trackSuite(t testing.TB, c *cache.Cache, suite SuiteRef) {
	t.Helper()
	if err := c.EnsureSuiteForTest(context.Background(), suite.CanonicalScheme, suite.CanonicalHost, suite.SuitePath); err != nil {
		t.Fatalf("EnsureSuiteForTest: %v", err)
	}
}
```

In `internal/handler/flip_stack_test.go`, above `adopt` add

```go
// track gives suite a suite_freshness row: spec C3 lets only the handler
// create rows, and CommitAdoption is UPDATE-only.
func (s *flipStack) track(suite string) {
	if err := s.c.EnsureSuiteForTest(context.Background(), "http", flipSimHost, suite); err != nil {
		s.t.Errorf("track %s: %v", suite, err)
	}
}
```

and call `s.track(suite)` right after `ref, rel := s.publish(suite, gen)` in `adopt` and in `adoptDetached` (`sim_flip_test.go`). The remaining one-line insertions (each test builds its suite outside the helpers above):

| File | Test | Insert | Where |
|---|---|---|---|
| `freshness/adoption_test.go` | `TestAdopter_FormDriftWARN_OnFormTransition`, `_MemberSkipped_404SkipsMidList`, `_MultiArch_NoFilter_AllArchsAdopted`, `_MultiArch_AllowlistFiltersOutOtherArchs`, `_Allowlist_KeepsBinaryAllIndexTarget`, `_Sources_HappyPath`, `_Sources_CrossVariantDisagreement`, `_Sources_IntraFileOrigTarballConflict`, `_PdiffIndex_HappyPath`, `_PdiffIndex_MalformedEntriesSkipped`, `_MultiArch_AllowlistKeepsSourcePseudoArch` | `trackSuite(t, c, suite)` | before the first `ad.Run(context.Background(), suite,` |
| `freshness/adoption_test.go` | `TestAdopter_PackageHash_NonStandardSuiteSkips` | `trackSuite(t, env.cache, env.suite)` | before `env.adopter.Run(` |
| `freshness/flip_barrier_test.go` | `newBarrierRig` | `trackSuite(t, c, barrierSuite)` | before its `return &barrierRig{…}` |
| `freshness/flip_barrier_test.go` | `TestSimCapacity_TheBarrierHoldsTheGlobalSlotWithinItsBound` | `trackSuite(t, c, ref)` | before `if err := ad.Run(ctx, ref, rel("v1-"+host, …` |
| `freshness/freshness_test.go` | `TestCheck_OK_ContentChanged_RecordsObservation`, `_TryLock_SecondCallSkips`, `_4xxFromUpstream_BumpsCheckOnly`, `_NoAdopter_ChangeOnlyLogged`, `_AdopterInvokedOnChange`, `_DetachedFallback_OnInRelease404`, `_DetachedFallback_OnMissingInReleaseURLRow`, `_404OnInRelease_NoFallback_WhenReleaseRowsMissing` | `fc.track("http", "127.0.0.1", "/dists/noble")` | after `fc := newFakeCache()` |
| `freshness/freshness_test.go` | `TestCheck_HostLimiterBoundsConcurrency` | `fc.track("http", "127.0.0.1", sp)` | before its `fc.putURL(cache.URLPath{` |
| `freshness/freshness_test.go` | `TestCheck_AdopterInvokedOnChange`, `_DetachedFallback_OnInRelease404`, `_DetachedFallback_OnMissingInReleaseURLRow`, `_DetachedKnownSuite_AdoptsOnReleaseChange` | `trackSuite(t, realCache, SuiteRef{CanonicalScheme: "http", CanonicalHost: "127.0.0.1", SuitePath: "/dists/noble"})` | after `t.Cleanup(func() { _ = realCache.Close() })` |
| `freshness/freshness_test.go` | `TestCheck_UnchangedTick_ReconcilesDegraded`, `TestCheck_FailedTick_DoesNotReconcile` | `trackSuite(t, env.cache, env.suite)` | before `if err := ad.Run(ctx, env.suite, releaseText, "", ""); err != nil {` |
| `freshness/panic_recovery_test.go` | `TestCheck_AdoptionPanicIsRecovered`, `TestCheck_AdoptionPanicRecoveredBeforeMutexRelease` | `trackSuite(t, env.cache, SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"})` | after `seedStaleAnchor(t, env.cache)` |
| `freshness/panic_recovery_test.go` | `TestCheck_RepairPanicIsRecovered` | `trackSuite(t, env.cache, env.suite)` | before `if err := good.Run(` |
| `freshness/panic_recovery_test.go` | `TestCheck_SynchronousPanicReleasesMutex`, `TestCheck_PanickingCheckIsCooldownGated` | `fc.track("http", "h", "/r/dists/x")` | after `fc := newFakeCache()` |
| `freshness/capacity_test.go` | `TestCheck_RepairObeysTheGlobalSlot` | `trackSuite(t, c, suite)` | before `if err := setupAd.Run(` |
| `freshness/phase6_7_skip_repair_test.go` | `TestChecker_FreshTickRepairsSkippedMembers` | `trackSuite(t, env.cache, env.suite)` | before `if err := ad.Run(ctx, env.suite,` |
| `freshness/sim_sched_test.go` | `TestSimCapacity_SlowDripAndDegraded` | `trackSuite(t, c, ref)` | before `if err := ad.Run(ctx, ref, rel("v1-"+host, …` |
| `freshness/sim_sched_test.go` | `TestSimScheduler_LatencyBoundEightHosts` | `trackSuite(t, c, SuiteRef{CanonicalScheme: "http", CanonicalHost: "h1", SuitePath: "/r/dists/s"})`; and `trackSuite(t, c, SuiteRef{CanonicalScheme: "http", CanonicalHost: host, SuitePath: cSuite})` | before the `ad.Run` of each of those two suites |
| `handler/phase2_detached_chaos_test.go`, `handler/phase2_detached_gpg_forgery_test.go` | `TestPhase2DetachedChaos_AdoptionFlipUnderConcurrency`, `TestPhase2DetachedGPGForgery_RejectionLeavesSnapshotIntact` | `if err := stack.handler.cache.EnsureSuiteForTest(context.Background(), "http", upstreamURL.Hostname(), chaos2Suite); err != nil { t.Fatal(err) }`, commented `// Spec C3: CommitAdoption is UPDATE-only; the detached suite's` / `// InRelease misses (404) created no row.` | before `stack.adopter.RunDetached(` |
| `cache/chaos_snapshot_race_test.go` | `TestChaos_SnapshotGC_HeartbeatLivenessRace_VariantTwo` | `if err := c.EnsureSuiteForTest(ctx, "http", "v2.example", "/p"); err != nil { t.Fatal(err) }`, commented `// CommitAdoption is UPDATE-only (spec C3): the suite needs its row.` | after `s := orphanCandidate(…)` (without it the "S survives" orderings fail with `ErrSuiteGone`, under `-race` mostly) |

- [ ] **Step 6: Run the suite**

Run: `go vet ./... && go test ./... && go test -race -count=3 -run 'TestChaos_' ./internal/cache/`
Expected: PASS. `grep -rn 'PutSuiteFreshness' --include=*.go .` prints only the "Carried over from PutSuiteFreshness" note.

- [ ] **Step 7: Mutation check (do not commit)**

One at a time, reverting each: (a) the per-host check `n >= caps.PerHost && false` → `TestCreateSuite_EnforcesTheCaps` FAILS (`step 2 (http://a/3): created, want host_cap`); (b) count per `canonical_scheme = ? AND canonical_host = ?` → same test FAILS at step 2; (c) `n > caps.Global` → same test FAILS (`step 6 (http://d/1): created, want global_cap`); (d) mark on `SuiteExists` too → `TestCreateSuite_MarksSignedSeenInTheCreatingTransaction` FAILS (`finding an existing row wrote a marker`); (e) ignore `n == 0` in Step 6 → `TestCommitAdoption_RefusesAGoneSuite` FAILS; (f) on `n == 0` INSERT the row instead → that test and `TestSuiteFreshnessHasOneInsertSite` FAIL (`sites = [../../internal/cache/adoption.go:CommitAdoption …]`); (g) `UpdateSuiteFreshness` also sets `last_requested_at` → `TestUpdateSuiteFreshness_IsUpdateOnly` and `TestSuiteInterestStamp_SetAtCreationKeptByWriters` FAIL; (h) drop the `suiteRowExists` pre-check → `TestCheck_UntrackedSuiteIsNotChecked` FAILS (`lock map holds 1 entries, want 0`); (i) Step 7a marks `suite` instead of `aptpath.MarkerBase(suite)` → `TestCommitAdoption_SignedFormsRecordSignedSeen` FAILS (`/flat: marker ok=false`); (j) a `c.CreateSuite(…)` call added to `freshness.go` → `TestOnlyTheHandlerCreatesSuiteRows` FAILS; (k) hoist `offset()` out of `RescheduleFailingAdoptions`' loop → `TestRescheduleFailingAdoptions_SkipsRefusalsAndPanics` FAILS; (l) `GetSuiteSnapshot` blanks `FormInline` → `TestGetSuiteSnapshot_ReturnsFormAndLifecycleFields` FAILS.

- [ ] **Step 8: Commit**

```bash
gofmt -l internal/ cmd/
golangci-lint run ./...
go test -race ./internal/cache/ ./internal/freshness/ ./internal/handler/
git add -A internal/ cmd/
git commit -m "Give suite rows one creation choke point; make every other writer UPDATE-only"
```

Expected: `gofmt -l` prints exactly the four drift files under `internal/` and `cmd/` (`cmd/apt-cacher-ultra/ca_print.go`, `internal/cache/cache_test.go`, `internal/cache/gc_urlpath_test.go`, `internal/freshness/hot_prefetch_test.go`) plus `internal/proxy/connect_stats.go` — the scripts edit three of them without reformatting; lint `0 issues`.

---

### Task 5: Caps and cap eviction under the suite's mutex

Spec C3 "At the global cap it first tries to evict (via the reaper path, under its mutex) the least-recently-requested suite that has no current snapshot and has been idle ≥ 24 h; otherwise it creates nothing"; ruling R-D1 (one compare-and-delete helper, `deleteSuiteTx`, which phase F's `ReapIdleSuite` reuses; count, evict and insert in one writer transaction; the evictee's `url_path` rows left to GC; the guard taken before the writer operation is submitted); design note 6 (at most eight candidates, and only at exactly the cap). The production guard — the checker's `HoldSuite`, whose `release(true)` drops the lock-map and flip-gate entries — is Task 7; Task 6 adds the touch-map purge to `afterCreate`; the handler passes the guard from Task 9. The marker of an evicted suite survives (spec C3: only phase F's sweep removes markers).

**Files:**
- Modify: `internal/cache/suite_rows.go`, `internal/cache/testing_helpers.go` (the `createSuiteTx` call in `SeedSuiteForTest`)
- Create: `internal/cache/suite_evict_test.go`

**Interfaces:**
- Consumes: Task 4's `SuiteKey`, `SuiteSeed`, `SuiteCaps`, `CreateResult`, `SuiteGuard`, `createSuiteTx`; `decrementSnapshotRefcountsTx(ctx, tx, snapshotID, now int64) error` (`cache/adoption.go`, `CommitAdoption`'s Step 8 helper); `MarkSignedSeenBatch`, `SignedSeen`, `LookupURL`, `PutURLPath`, `GetSnapshotMember`, `GetBlob`; test helpers `openCache`, `seedBlob`, `stubNow`.
- Produces:
  - `func deleteSuiteTx(ctx context.Context, tx *sql.Tx, key SuiteKey, wantCurrent *int64, cutoff, now int64) (bool, error)` — the single suite-row delete (F reuses it with `wantCurrent` set).
  - `func createSuiteTx(ctx context.Context, tx *sql.Tx, seed SuiteSeed, caps SuiteCaps, held *heldSuite, now int64) (CreateResult, error)` (Task 6's `StoreFetched` passes a held candidate too).
  - `type heldSuite struct { key SuiteKey; release func(deleted bool) }`; `func (c *Cache) holdEvictionCandidate(ctx context.Context, caps SuiteCaps, guard SuiteGuard) (*heldSuite, error)`; `func (c *Cache) afterCreate(res CreateResult, held *heldSuite)` (Task 6 extends it).
  - Constants `suiteEvictIdle = 24 * time.Hour`, `suiteEvictCandidates = 8`.
  - Test helpers (`suite_evict_test.go`): `fakeGuard` / `newFakeGuard()` (fields `busy`, `asked`, `released`, `onHold`), `sk(host, suite string) SuiteKey`, `seedRow`, `adoptRow`, `rowExists`, `fourRows`, constants `evictNow`, `evictDay`.

- [ ] **Step 1: Write the failing tests**

Create `internal/cache/suite_evict_test.go`:

```go
package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
)

// fakeGuard is a SuiteGuard: it holds any suite not marked busy, records
// the order it was asked in and how each hold was released, and runs
// onHold (if set) inside HoldSuite — before the writer operation.
type fakeGuard struct {
	mu       sync.Mutex
	busy     map[SuiteKey]bool
	asked    []SuiteKey
	released map[SuiteKey][]bool
	onHold   func(SuiteKey)
}

func newFakeGuard() *fakeGuard {
	return &fakeGuard{busy: map[SuiteKey]bool{}, released: map[SuiteKey][]bool{}}
}

func (g *fakeGuard) HoldSuite(key SuiteKey) (func(deleted bool), bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked = append(g.asked, key)
	if g.busy[key] {
		return nil, false
	}
	if g.onHold != nil {
		g.onHold(key)
	}
	return func(deleted bool) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.released[key] = append(g.released[key], deleted)
	}, true
}

const evictNow = int64(1_800_000_000)
const evictDay = int64(24 * 3600)

func sk(host, suite string) SuiteKey {
	return SuiteKey{CanonicalScheme: "http", CanonicalHost: host, SuitePath: suite}
}

// seedRow creates a row whose client-interest stamp is requested.
func seedRow(t *testing.T, c *Cache, key SuiteKey, requested int64) {
	t.Helper()
	res, err := c.CreateSuite(context.Background(), SuiteSeed{SuiteKey: key, RequestedAt: requested}, SuiteCaps{}, nil)
	if err != nil || res.Outcome != SuiteCreated {
		t.Fatalf("seed %v: %+v, %v", key, res, err)
	}
}

// adoptRow gives an existing row a current snapshot.
func adoptRow(t *testing.T, c *Cache, key SuiteKey) int64 {
	t.Helper()
	ctx := context.Background()
	h := seedBlob(t, c, "anchor of "+key.SuitePath)
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{CanonicalScheme: key.CanonicalScheme,
		CanonicalHost: key.CanonicalHost, SuitePath: key.SuitePath, InReleaseHash: &h})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id, []SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: h, DeclaredSHA256: h}},
		nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	return id
}

func rowExists(t *testing.T, c *Cache, key SuiteKey) bool {
	t.Helper()
	_, err := c.GetSuiteFreshness(context.Background(), key.CanonicalScheme, key.CanonicalHost, key.SuitePath)
	if err != nil && !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

// fourRows fills a cache to a global cap of 4: an adopted suite (the
// idlest, never evictable), two idle suites without a snapshot, and one
// requested an hour ago (not idle).
func fourRows(t *testing.T, c *Cache) {
	t.Helper()
	seedRow(t, c, sk("a", "/adopted"), evictNow-5*evictDay)
	adoptRow(t, c, sk("a", "/adopted"))
	seedRow(t, c, sk("b", "/idle"), evictNow-3*evictDay)
	seedRow(t, c, sk("c", "/idle2"), evictNow-2*evictDay)
	seedRow(t, c, sk("d", "/fresh"), evictNow-3600)
}

// Spec C3 / R-D1: at the global cap, CreateSuite evicts the
// least-recently-requested suite that has no current snapshot and has been
// idle for 24 h, under its mutex (the guard), in the creating transaction:
// the evictee's snapshots are retired (CommitAdoption refuses its
// candidate), its url_path rows are left to GC, and the guard is told the
// row is gone.
func TestCreateSuite_EvictsTheIdlestSuiteAtTheGlobalCap(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, evictNow)()
	fourRows(t, c)
	h := seedBlob(t, c, "candidate of /idle")
	cand, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "b",
		SuitePath: "/idle", InReleaseHash: &h})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.PutURLPath(ctx, URLPath{CanonicalScheme: "http", CanonicalHost: "b", Path: "/idle/InRelease",
		UpstreamURL: "http://b/idle/InRelease", IsMetadata: true}); err != nil {
		t.Fatal(err)
	}
	if err := c.MarkSignedSeenBatch(ctx, []SignedSeenMark{{CanonicalScheme: "http", CanonicalHost: "b", Base: "/idle",
		SeenAt: evictNow - 3*evictDay}}); err != nil {
		t.Fatal(err)
	}
	g := newFakeGuard()
	res, err := c.CreateSuite(ctx, SuiteSeed{SuiteKey: sk("e", "/new"), RequestedAt: evictNow}, SuiteCaps{Global: 4}, g)
	if err != nil || res.Outcome != SuiteCreated || res.Evicted == nil || *res.Evicted != sk("b", "/idle") {
		t.Fatalf("CreateSuite = %+v (evicted %v), %v; want created, evicting b/idle", res, res.Evicted, err)
	}
	if !slices.Equal(g.asked, []SuiteKey{sk("b", "/idle")}) || !slices.Equal(g.released[sk("b", "/idle")], []bool{true}) {
		t.Fatalf("guard asked %v, released %v; want b/idle held and released as deleted", g.asked, g.released)
	}
	if rowExists(t, c, sk("b", "/idle")) || !rowExists(t, c, sk("e", "/new")) || !rowExists(t, c, sk("a", "/adopted")) {
		t.Fatal("rows after the eviction: want b/idle gone, e/new and a/adopted present")
	}
	if snap, err := c.GetSuiteSnapshot(ctx, cand); err != nil || snap.RetiredAt == nil || *snap.RetiredAt != evictNow {
		t.Fatalf("evictee's candidate = %+v, %v; want retired at %d", snap, err, evictNow)
	}
	if err := c.CommitAdoption(ctx, cand, nil, nil, nil, nil, false); !errors.Is(err, ErrSnapshotRetired) {
		t.Fatalf("CommitAdoption of the evictee's candidate = %v, want ErrSnapshotRetired", err)
	}
	if _, err := c.LookupURL(ctx, "http", "b", "/idle/InRelease"); err != nil {
		t.Fatalf("the evictee's url_path row was deleted (%v): url_path GC owns it", err)
	}
	if _, ok, err := c.SignedSeen(ctx, "http", "b", "/idle"); err != nil || !ok {
		t.Fatalf("the evictee's signed-seen marker is gone (%v): markers survive reaping (spec C3)", err)
	}
}

// A busy candidate (a check or an adoption holds its mutex) is skipped for
// the next one; nothing busy is ever deleted.
func TestCreateSuite_EvictionSkipsBusySuites(t *testing.T) {
	c := openCache(t)
	defer stubNow(t, evictNow)()
	fourRows(t, c)
	g := newFakeGuard()
	g.busy[sk("b", "/idle")] = true
	res, err := c.CreateSuite(context.Background(), SuiteSeed{SuiteKey: sk("e", "/new"), RequestedAt: evictNow},
		SuiteCaps{Global: 4}, g)
	if err != nil || res.Outcome != SuiteCreated || res.Evicted == nil || *res.Evicted != sk("c", "/idle2") {
		t.Fatalf("CreateSuite = %+v, %v; want created, evicting c/idle2", res, err)
	}
	if !slices.Equal(g.asked, []SuiteKey{sk("b", "/idle"), sk("c", "/idle2")}) || len(g.released[sk("b", "/idle")]) != 0 {
		t.Fatalf("guard asked %v, released %v", g.asked, g.released)
	}
	if !rowExists(t, c, sk("b", "/idle")) {
		t.Fatal("the busy suite was deleted")
	}
}

// With nothing evictable (every suite adopted, recently requested or
// busy), a creation at the global cap is refused and nothing is deleted.
func TestCreateSuite_RefusesWhenNothingIsEvictable(t *testing.T) {
	c := openCache(t)
	defer stubNow(t, evictNow)()
	fourRows(t, c)
	g := newFakeGuard()
	g.busy[sk("b", "/idle")], g.busy[sk("c", "/idle2")] = true, true
	res, err := c.CreateSuite(context.Background(), SuiteSeed{SuiteKey: sk("e", "/new"), RequestedAt: evictNow},
		SuiteCaps{Global: 4}, g)
	if err != nil || res.Outcome != SuiteRejectedGlobal || res.Evicted != nil {
		t.Fatalf("CreateSuite = %+v, %v; want global_cap, nothing evicted", res, err)
	}
	if st, err := c.GetSuiteStats(context.Background()); err != nil || st.Tracked != 4 {
		t.Fatalf("tracked = %+v, %v; want 4", st, err)
	}
}

// Design note 6: above the cap (rows created before max_suites was
// lowered) one eviction cannot make room, so nothing is evicted; the held
// candidate is released as not deleted.
func TestCreateSuite_NeverEvictsAboveTheCap(t *testing.T) {
	c := openCache(t)
	defer stubNow(t, evictNow)()
	fourRows(t, c)
	seedRow(t, c, sk("f", "/idle3"), evictNow-4*evictDay)
	g := newFakeGuard()
	res, err := c.CreateSuite(context.Background(), SuiteSeed{SuiteKey: sk("e", "/new"), RequestedAt: evictNow},
		SuiteCaps{Global: 4}, g)
	if err != nil || res.Outcome != SuiteRejectedGlobal || res.Evicted != nil {
		t.Fatalf("CreateSuite = %+v, %v; want global_cap", res, err)
	}
	if !slices.Equal(g.released[sk("f", "/idle3")], []bool{false}) || !rowExists(t, c, sk("f", "/idle3")) {
		t.Fatalf("released %v; want f/idle3 kept and released as not deleted", g.released)
	}
}

// R-D1: the writer re-checks the candidate (compare-and-delete). A
// candidate requested after it was chosen — here inside HoldSuite, before
// the writer operation — is no longer idle: nothing is deleted and the
// creation is refused.
func TestCreateSuite_EvictionRechecksInTheWriter(t *testing.T) {
	c := openCache(t)
	defer stubNow(t, evictNow)()
	fourRows(t, c)
	g := newFakeGuard()
	g.onHold = func(k SuiteKey) {
		if _, err := c.db.Exec(`UPDATE suite_freshness SET last_requested_at = ? WHERE suite_path = ?`, evictNow, k.SuitePath); err != nil {
			t.Error(err)
		}
	}
	res, err := c.CreateSuite(context.Background(), SuiteSeed{SuiteKey: sk("e", "/new"), RequestedAt: evictNow},
		SuiteCaps{Global: 4}, g)
	if err != nil || res.Outcome != SuiteRejectedGlobal {
		t.Fatalf("CreateSuite = %+v, %v; want global_cap", res, err)
	}
	if !slices.Equal(g.released[sk("b", "/idle")], []bool{false}) || !rowExists(t, c, sk("b", "/idle")) {
		t.Fatalf("released %v; want b/idle kept", g.released)
	}
}

// R-D1: deleteSuiteTx is a compare-and-delete phase F's reaper reuses: it
// deletes only a row still idle at the cutoff with the expected current
// snapshot (releasing that snapshot's pins), retires the suite's
// snapshots, and treats a future stamp (backward clock step) as not idle.
func TestDeleteSuiteTx_IsACompareAndDelete(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, evictNow)()
	key := sk("h", "/r/dists/x")
	seedRow(t, c, key, evictNow-2*evictDay)
	id := adoptRow(t, c, key)
	anchor, err := c.GetSnapshotMember(ctx, id, "InRelease")
	if err != nil {
		t.Fatal(err)
	}
	del := func(k SuiteKey, want *int64, cutoff int64) bool {
		t.Helper()
		var deleted bool
		if err := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
			tx, err := conn.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if deleted, err = deleteSuiteTx(ctx, tx, k, want, cutoff, evictNow); err != nil {
				return err
			}
			return tx.Commit()
		}); err != nil {
			t.Fatal(err)
		}
		return deleted
	}
	other := id + 1
	for name, ok := range map[string]bool{
		"no current snapshot expected": del(key, nil, evictNow-evictDay),
		"another current snapshot":     del(key, &other, evictNow-evictDay),
		"requested after the cutoff":   del(key, &id, evictNow-3*evictDay),
		"no such row":                  del(sk("h", "/nope"), nil, evictNow),
	} {
		if ok {
			t.Fatalf("%s: deleted", name)
		}
	}
	if !del(key, &id, evictNow-evictDay) {
		t.Fatal("a qualifying row was not deleted")
	}
	if rowExists(t, c, key) {
		t.Fatal("row still present")
	}
	if b, err := c.GetBlob(ctx, anchor.BlobHash); err != nil || b.RefCount != 0 {
		t.Fatalf("anchor blob = %+v, %v; want its pin released (refcount 0)", b, err)
	}
	if snap, err := c.GetSuiteSnapshot(ctx, id); err != nil || snap.RetiredAt == nil {
		t.Fatalf("snapshot = %+v, %v; want retired", snap, err)
	}
	// A stamp in the future never qualifies, whatever the cutoff.
	future := sk("h", "/future")
	seedRow(t, c, future, evictNow+evictDay)
	if del(future, nil, evictNow) {
		t.Fatal("a row requested in the future was deleted")
	}
}

// Count, eviction and insert run in one writer transaction, so concurrent
// creations never exceed either cap.
func TestCreateSuite_ConcurrentCreationsNeverExceedTheCaps(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	caps := SuiteCaps{Global: 10, PerHost: 3}
	var wg sync.WaitGroup
	var mu sync.Mutex
	created := 0
	for h := range 8 {
		for s := range 5 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := c.CreateSuite(ctx, SuiteSeed{SuiteKey: sk(fmt.Sprintf("h%d", h), fmt.Sprintf("/s%d", s)),
					RequestedAt: nowUnix()}, caps, newFakeGuard())
				if err != nil {
					t.Error(err)
					return
				}
				if res.Outcome == SuiteCreated {
					mu.Lock()
					created++
					mu.Unlock()
				}
			}()
		}
	}
	wg.Wait()
	all, err := c.ListSuites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	perHost := map[string]int{}
	for _, s := range all {
		perHost[s.CanonicalHost]++
		if perHost[s.CanonicalHost] > 3 {
			t.Fatalf("host %s has %d rows, cap 3", s.CanonicalHost, perHost[s.CanonicalHost])
		}
	}
	if len(all) != 10 || created != 10 {
		t.Fatalf("rows = %d, created = %d; want 10 and 10 (the global cap)", len(all), created)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/cache/ -run 'TestCreateSuite_|TestDeleteSuiteTx_'`
Expected: build failure `undefined: deleteSuiteTx`.

- [ ] **Step 3: Implement eviction in `internal/cache/suite_rows.go`**

Add `"time"` to the imports and, after them:

```go
// Cap eviction (spec C3): a suite is evictable when it has no current
// snapshot and no client has asked for it for suiteEvictIdle; at most
// suiteEvictCandidates are tried per creation (a busy one is skipped).
const (
	suiteEvictIdle       = 24 * time.Hour
	suiteEvictCandidates = 8
)
```

After the `SuiteGuard` interface:

```go
// heldSuite is an eviction candidate whose in-memory mutex the caller
// holds (through a SuiteGuard) while the writer operation runs.
type heldSuite struct {
	key     SuiteKey
	release func(deleted bool)
}
```

Replace `CreateSuite` (doc included) with the following, and add `afterCreate` after it:

```go
// CreateSuite is spec C3's single creation choke point, for a suite the
// caller found without a row (the handler's url_path-hit onboarding). It
// creates the row only if absent (SuiteExists otherwise), within caps: it
// refuses at the per-host cap; at the global cap it first evicts the
// least-recently-requested suite that has no current snapshot, has been
// idle for suiteEvictIdle and whose mutex guard takes without waiting (a
// nil guard takes none: no in-memory state exists to protect). Count,
// eviction and insert run in one writer transaction, so concurrent
// creations never exceed a cap. With seed.SignedSeenAt set, a created
// row's base is marked signed-seen in the same transaction.
//
// AIDEV-NOTE: the evictee's metadata url_path rows are left to url_path
// GC: a request never pays for a batched delete (phase F's reaper does
// that work, off the request path). The guard is taken before the writer
// operation is submitted — the writer goroutine never runs foreign code.
func (c *Cache) CreateSuite(ctx context.Context, seed SuiteSeed, caps SuiteCaps, guard SuiteGuard) (CreateResult, error) {
	held, err := c.holdEvictionCandidate(ctx, caps, guard)
	if err != nil {
		return CreateResult{}, err
	}
	var got CreateResult
	err = c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("CreateSuite: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		r, err := createSuiteTx(ctx, tx, seed, caps, held, nowUnix())
		if err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("CreateSuite: commit: %w", err)
		}
		got = r
		return nil
	})
	if err != nil {
		// Never read got after an error: on a ctx/close path the op may
		// still be running (see InsertCandidateSnapshot).
		c.afterCreate(CreateResult{}, held)
		return CreateResult{}, err
	}
	c.afterCreate(got, held)
	return got, nil
}

// afterCreate releases the held eviction candidate, telling the guard
// whether its row was deleted.
//
// AIDEV-NOTE: after a failed submit (cancelled context, closing cache)
// the outcome is unknown and the candidate is released as not deleted. A
// queued operation may still commit its eviction later: the evictee's
// lock-map and flip-gate entries then stay (one each, bounded), and a
// check that started meanwhile meets a vanished row, which I-4's
// re-validation and the UPDATE-only writers make harmless (its candidate,
// if any, was retired: CommitAdoption refuses it).
func (c *Cache) afterCreate(res CreateResult, held *heldSuite) {
	if held != nil {
		held.release(res.Evicted != nil)
	}
}
```

Add `holdEvictionCandidate`. It counts rows outside the writer (`SELECT COUNT(*) FROM suite_freshness`; below `caps.Global`, or `caps.Global <= 0`, it returns nil), lists at most `suiteEvictCandidates` candidates with

```sql
SELECT canonical_scheme, canonical_host, suite_path
  FROM suite_freshness
 WHERE current_snapshot_id IS NULL AND last_requested_at <= ?
 ORDER BY last_requested_at, canonical_scheme, canonical_host, suite_path
 LIMIT ?
```

(cutoff `nowUnix() - int64(suiteEvictIdle/time.Second)`), and returns the first one `guard.HoldSuite` takes (a nil guard takes the first with a no-op release); none taken → nil. Errors are wrapped `CreateSuite: count suites: %w`, `CreateSuite: list eviction candidates: %w`, `CreateSuite: scan eviction candidate: %w` (check `rows.Close()` and `rows.Err()`). Doc: "holdEvictionCandidate returns a held eviction candidate when the global cap looks reached (a read outside the writer; createSuiteTx re-checks everything), or nil: the least recently requested of at most suiteEvictCandidates evictable suites whose mutex guard takes at once."

`createSuiteTx` takes `held *heldSuite, now int64` after `caps`; its doc's first sentence becomes "createSuiteTx inserts seed's row unless it exists, enforcing caps inside the caller's writer transaction; at exactly the global cap it first evicts held (deleteSuiteTx), refusing when held is nil or no longer qualifies." (the rest, with the AIDEV-NOTE, unchanged). Its global-cap block becomes

```go
	var res CreateResult
	if caps.Global > 0 {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM suite_freshness`).Scan(&n); err != nil {
			return CreateResult{}, fmt.Errorf("CreateSuite: count suites: %w", err)
		}
		if n >= caps.Global {
			// One eviction makes room only at the cap itself; above it
			// (rows created before the cap was lowered) nothing is evicted
			// and phase F's reaper shrinks the table.
			if held == nil || n > caps.Global {
				return CreateResult{Outcome: SuiteRejectedGlobal}, nil
			}
			deleted, err := deleteSuiteTx(ctx, tx, held.key, nil, now-int64(suiteEvictIdle/time.Second), now)
			if err != nil {
				return CreateResult{}, err
			}
			if !deleted {
				return CreateResult{Outcome: SuiteRejectedGlobal}, nil
			}
			evicted := held.key
			res.Evicted = &evicted
		}
	}
```

and its end `res.Outcome = SuiteCreated` / `return res, nil`. Add, after `createSuiteTx`:

```go
// deleteSuiteTx is the single suite-row delete (spec C3 cap eviction; phase
// F's ReapIdleSuite reuses it), run inside the caller's writer
// transaction as a compare-and-delete: it re-reads the row and deletes it
// only if the row still qualifies — last_requested_at at or before cutoff
// (a stamp in the future, after a backward clock step, never qualifies)
// and current_snapshot_id equal to wantCurrent (nil: no current
// snapshot). It releases the current snapshot's pins (the Step-8
// decrement), retires every snapshot of the suite, and deletes the row.
// It reports whether it deleted.
//
// AIDEV-NOTE: retiring the snapshots is load-bearing: CommitAdoption
// refuses a retired candidate, and retired rows leave the natural-key,
// rollback and retained-by-hash lookups, so a returning suite re-adopts
// unchanged content at once (spec C9).
func deleteSuiteTx(ctx context.Context, tx *sql.Tx, key SuiteKey, wantCurrent *int64, cutoff, now int64) (bool, error) {
	var current, requested sql.NullInt64
	err := tx.QueryRowContext(ctx, `
SELECT current_snapshot_id, last_requested_at FROM suite_freshness
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`,
		key.CanonicalScheme, key.CanonicalHost, key.SuitePath).Scan(&current, &requested)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("delete suite %s%s: read row: %w", key.CanonicalHost, key.SuitePath, err)
	}
	if !requested.Valid || requested.Int64 > cutoff {
		return false, nil
	}
	if current.Valid != (wantCurrent != nil) || (current.Valid && current.Int64 != *wantCurrent) {
		return false, nil
	}
	if current.Valid {
		if err := decrementSnapshotRefcountsTx(ctx, tx, current.Int64, now); err != nil {
			return false, fmt.Errorf("delete suite %s%s: %w", key.CanonicalHost, key.SuitePath, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE suite_snapshot SET retired_at = ?
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ? AND retired_at IS NULL`,
		now, key.CanonicalScheme, key.CanonicalHost, key.SuitePath); err != nil {
		return false, fmt.Errorf("delete suite %s%s: retire snapshots: %w", key.CanonicalHost, key.SuitePath, err)
	}
	if _, err := tx.ExecContext(ctx, `
DELETE FROM suite_freshness
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`,
		key.CanonicalScheme, key.CanonicalHost, key.SuitePath); err != nil {
		return false, fmt.Errorf("delete suite %s%s: %w", key.CanonicalHost, key.SuitePath, err)
	}
	return true, nil
}
```

In `internal/cache/testing_helpers.go`, `SeedSuiteForTest`'s call becomes `createSuiteTx(ctx, tx, SuiteSeed{SuiteKey: key, RequestedAt: nowUnix()}, SuiteCaps{}, nil, nowUnix())`.

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/cache/`
Expected: PASS (Task 4's `TestCreateSuite_EnforcesTheCaps` creates freshly requested rows, so nothing there is evictable).

- [ ] **Step 5: Mutation check (do not commit)**

One at a time, reverting each: (a) `ORDER BY last_requested_at DESC, …` → `TestCreateSuite_EvictsTheIdlestSuiteAtTheGlobalCap` FAILS (`evicting &{http c /idle2}`); (b) drop `current_snapshot_id IS NULL AND` from the candidate query → the same test FAILS (`global_cap`: the adopted suite is chosen, and `deleteSuiteTx` refuses it); (c) `if held == nil {` (evict above the cap) → `TestCreateSuite_NeverEvictsAboveTheCap` FAILS; (d) drop `|| requested.Int64 > cutoff` → `TestCreateSuite_EvictionRechecksInTheWriter` and `TestDeleteSuiteTx_IsACompareAndDelete` FAIL; (e) make the retire `UPDATE` match nothing (`AND 0`) → `TestCreateSuite_EvictsTheIdlestSuiteAtTheGlobalCap` FAILS (`want retired`); (f) `held.release(true)` always → `TestCreateSuite_NeverEvictsAboveTheCap` FAILS (`released map[{http f /idle3}:[true]]`); (g) take a busy candidate anyway → `TestCreateSuite_EvictionSkipsBusySuites` FAILS; (h) skip `decrementSnapshotRefcountsTx` → `TestDeleteSuiteTx_IsACompareAndDelete` FAILS (`RefCount:1`); (i) ignore `wantCurrent` → the same test FAILS (`no current snapshot expected: deleted`).

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/cache/suite_rows.go internal/cache/suite_evict_test.go internal/cache/testing_helpers.go
golangci-lint run ./internal/cache/...
git add internal/cache/suite_rows.go internal/cache/suite_evict_test.go internal/cache/testing_helpers.go
git commit -m "Evict the idlest suite without a current snapshot at the global suite cap"
```

---

### Task 6: The miss path's writer operation, client-interest touches and the FIFO guarantee (cache)

Spec C3, cache side: the anchor row and the signed-seen marker are created "in the writer operation that stores the `url_path` row, before the body is written" (`StoreFetched`: one transaction — the `url_path` upsert, `createSuiteTx` under the caps with eviction, the marker only for a suite with a row); "a failed operation serves no body" (here: it stores nothing and returns the error; Task 9 answers 502); FIFO ordering (a writer operation enqueued after the marker write sees it — phase E's unsigned `CommitAdoption` re-read); client interest (`TouchSuite`: UPDATE-only, at most one write per suite per hour, the map holds only tracked suites and is purged on eviction); vouched `.deb` serves touch "the suite path `DeclaredHashesForPath` returns" (`DeclaredHash.SuitePath`); and `SuiteExists`, the one-row `EXISTS` read the handler's flip-barrier registration uses (deferred minor C-21's query half, Task 10). Design note 7. Deferred minors A-24 and A-25 (`signed_seen.go`, which this task touches to list the new marker writers in `markSignedSeen`'s AIDEV-NOTE).

**Files:**
- Create: `internal/cache/fetched_row.go`, `internal/cache/fetched_row_test.go`, `internal/cache/suite_touch.go`, `internal/cache/suite_touch_test.go`
- Modify: `internal/cache/queries.go` (`putURLPathSQL`), `internal/cache/cache.go` (`touches` field), `internal/cache/suite_rows.go` (`afterCreate`, `SuiteExists`), `internal/cache/adoption.go` (`DeclaredHash.SuitePath`), `internal/cache/signed_seen.go` (AIDEV-NOTE; A-25), `internal/cache/suite_rows_test.go`, `internal/cache/signed_seen_test.go` (A-24, A-25)

**Interfaces:**
- Consumes: Task 5's `holdEvictionCandidate`, `createSuiteTx(…, held, now)`, `afterCreate`, and its test helpers `fakeGuard`, `fourRows`, `seedRow`, `sk`, `rowExists`, `evictNow`; Task 4's `derefI64`; `markSignedSeen`, `submitWrite`, `c.writes` (the writer's FIFO channel).
- Produces:
  - `type FetchedRow struct { Row URLPath; Suite SuiteKey; Create *SuiteSeed; MarkSeenAt int64 }`, `type FetchedResult struct { Create CreateResult; Tracked, Marked bool }`, `func (c *Cache) StoreFetched(ctx context.Context, m FetchedRow, caps SuiteCaps, guard SuiteGuard) (FetchedResult, error)` (Task 9's miss path).
  - `func (c *Cache) TouchSuite(ctx context.Context, key SuiteKey) error`, `func (c *Cache) TouchedSuites() int` (Tasks 10, 11).
  - `func (c *Cache) SuiteExists(ctx context.Context, key SuiteKey) (bool, error)` (Task 10).
  - `DeclaredHash.SuitePath string` (Task 10).
  - `afterCreate(key SuiteKey, res CreateResult, held *heldSuite)`; constants `touchEvery = time.Hour`, `touchSweepAt = 4096`.
  - Test helpers `fetchedRow(t, c, key, file) URLPath` (`fetched_row_test.go`), `requestedAt(t, c, key) int64` (`suite_touch_test.go`).

- [ ] **Step 1: Write the failing tests**

Create `internal/cache/fetched_row_test.go`:

```go
package cache

import (
	"context"
	"database/sql"
	"errors"
	"runtime"
	"testing"
)

// fetchedRow is the url_path row of a miss of key's file, over a stored
// blob (url_path.blob_hash references blob).
func fetchedRow(t *testing.T, c *Cache, key SuiteKey, file string) URLPath {
	t.Helper()
	h := seedBlob(t, c, "body of "+key.CanonicalHost+key.SuitePath+file)
	at := int64(1_800_000_000)
	return URLPath{CanonicalScheme: key.CanonicalScheme, CanonicalHost: key.CanonicalHost, Path: key.SuitePath + file,
		BlobHash: &h, UpstreamURL: "http://" + key.CanonicalHost + key.SuitePath + file, IsMetadata: true,
		LastRequestedAt: &at, RequestCount: 1, LastFetchedAt: &at}
}

// Spec C3: one writer transaction stores the url_path row, creates the
// suite row and marks the suite's base — before the handler serves.
func TestStoreFetched_StoresRowSuiteAndMarkerTogether(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/r/dists/x")
	res, err := c.StoreFetched(ctx, FetchedRow{Row: fetchedRow(t, c, key, "/InRelease"), Suite: key,
		Create: &SuiteSeed{SuiteKey: key, RequestedAt: 1_800_000_000}, MarkSeenAt: 1_800_000_000}, SuiteCaps{}, nil)
	if err != nil || res.Create.Outcome != SuiteCreated || !res.Tracked || !res.Marked {
		t.Fatalf("StoreFetched = %+v, %v; want created, tracked, marked", res, err)
	}
	if _, err := c.LookupURL(ctx, "http", "h", "/r/dists/x/InRelease"); err != nil {
		t.Fatalf("url_path row: %v", err)
	}
	if at, ok, err := c.SignedSeen(ctx, "http", "h", "/r/dists/x"); err != nil || !ok || at != 1_800_000_000 {
		t.Fatalf("marker = %d %v %v", at, ok, err)
	}
	// A refresh miss of a tracked suite: no creation, marker still written.
	res, err = c.StoreFetched(ctx, FetchedRow{Row: fetchedRow(t, c, key, "/InRelease"), Suite: key,
		Create: &SuiteSeed{SuiteKey: key, RequestedAt: 1}, MarkSeenAt: 1_800_000_500}, SuiteCaps{}, nil)
	if err != nil || res.Create.Outcome != SuiteExists || !res.Tracked || !res.Marked {
		t.Fatalf("refresh StoreFetched = %+v, %v; want exists, tracked, marked", res, err)
	}
	if at, _, _ := c.SignedSeen(ctx, "http", "h", "/r/dists/x"); at != 1_800_000_500 {
		t.Fatalf("marker after the refresh = %d, want 1800000500", at)
	}
}

// Spec C3: markers only for suites with a row. An untracked suite's
// signed anchor, or one refused at a cap, stores its url_path row with no
// marker.
func TestStoreFetched_MarksOnlyTrackedSuites(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	gpgOnly := sk("h", "/r/dists/untracked")
	res, err := c.StoreFetched(ctx, FetchedRow{Row: fetchedRow(t, c, gpgOnly, "/Release.gpg"), Suite: gpgOnly,
		MarkSeenAt: 7}, SuiteCaps{}, nil)
	if err != nil || res.Tracked || res.Marked || res.Create.Outcome != 0 {
		t.Fatalf("StoreFetched = %+v, %v; want untracked, unmarked, no creation", res, err)
	}
	refused := sk("h", "/r/dists/refused")
	seedRow(t, c, sk("h", "/r/dists/first"), nowUnix())
	res, err = c.StoreFetched(ctx, FetchedRow{Row: fetchedRow(t, c, refused, "/InRelease"), Suite: refused,
		Create: &SuiteSeed{SuiteKey: refused, RequestedAt: 7}, MarkSeenAt: 7}, SuiteCaps{Global: 10, PerHost: 1}, nil)
	if err != nil || res.Create.Outcome != SuiteRejectedHost || res.Tracked || res.Marked {
		t.Fatalf("StoreFetched at the host cap = %+v, %v; want host_cap, untracked, unmarked", res, err)
	}
	for _, p := range []string{"/r/dists/untracked/Release.gpg", "/r/dists/refused/InRelease"} {
		if _, err := c.LookupURL(ctx, "http", "h", p); err != nil {
			t.Fatalf("%s: url_path row not stored: %v", p, err)
		}
	}
	for _, k := range []SuiteKey{gpgOnly, refused} {
		if _, ok, err := c.SignedSeen(ctx, "http", "h", k.SuitePath); err != nil || ok {
			t.Fatalf("%s: marker written for an untracked suite (%v)", k.SuitePath, err)
		}
	}
}

// Spec C3: a failed writer operation stores nothing — no url_path row, no
// suite row, no marker — and returns the error (the handler then answers
// 502 with no body).
func TestStoreFetched_AFailedOperationStoresNothing(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	if _, err := c.db.Exec(`CREATE TRIGGER fail_mark BEFORE INSERT ON suite_signed_seen
BEGIN SELECT RAISE(ABORT, 'injected marker failure'); END`); err != nil {
		t.Fatal(err)
	}
	key := sk("h", "/r/dists/x")
	_, err := c.StoreFetched(ctx, FetchedRow{Row: fetchedRow(t, c, key, "/InRelease"), Suite: key,
		Create: &SuiteSeed{SuiteKey: key, RequestedAt: 1}, MarkSeenAt: 1}, SuiteCaps{}, nil)
	if err == nil {
		t.Fatal("StoreFetched succeeded with a failing marker insert")
	}
	if _, err := c.LookupURL(ctx, "http", "h", "/r/dists/x/InRelease"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("url_path row stored by a failed operation (%v)", err)
	}
	if rowExists(t, c, key) {
		t.Fatal("suite row created by a failed operation")
	}
}

// StoreFetched creates under the caps with cap eviction, like CreateSuite,
// and releases the held candidate.
func TestStoreFetched_EvictsAtTheGlobalCap(t *testing.T) {
	c := openCache(t)
	defer stubNow(t, evictNow)()
	fourRows(t, c)
	g := newFakeGuard()
	key := sk("e", "/new")
	res, err := c.StoreFetched(context.Background(), FetchedRow{Row: fetchedRow(t, c, key, "/InRelease"), Suite: key,
		Create: &SuiteSeed{SuiteKey: key, RequestedAt: evictNow}}, SuiteCaps{Global: 4}, g)
	if err != nil || res.Create.Outcome != SuiteCreated || res.Create.Evicted == nil || *res.Create.Evicted != sk("b", "/idle") {
		t.Fatalf("StoreFetched = %+v, %v; want created, evicting b/idle", res, err)
	}
	if len(g.released[sk("b", "/idle")]) != 1 || !g.released[sk("b", "/idle")][0] {
		t.Fatalf("released %v; want b/idle released as deleted", g.released)
	}
}

// Spec C3 FIFO ordering: the writer is a FIFO queue drained by one
// goroutine, so a writer operation enqueued after a StoreFetched that
// marks — phase E's unsigned CommitAdoption, with its in-transaction marker
// re-read — sees the marker.
func TestStoreFetched_MarkerPrecedesALaterWriterOperation(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/r/dists/x")
	seedRow(t, c, key, nowUnix())
	row := fetchedRow(t, c, key, "/InRelease") // its blob is written before the writer is blocked
	block, blocked := make(chan struct{}), make(chan struct{})
	go func() {
		_ = c.submitWrite(ctx, func(context.Context, *sql.Conn) error {
			close(blocked)
			<-block
			return nil
		})
	}()
	<-blocked // the writer is busy; what follows queues in order
	waitQueued := func(n int) {
		t.Helper()
		for len(c.writes) < n {
			runtime.Gosched()
		}
	}
	storeErr := make(chan error, 1)
	go func() {
		_, err := c.StoreFetched(ctx, FetchedRow{Row: row, Suite: key, MarkSeenAt: 9}, SuiteCaps{}, nil)
		storeErr <- err
	}()
	waitQueued(1)
	var sawMarker bool
	later := make(chan error, 1)
	go func() {
		later <- c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
			var n int
			err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM suite_signed_seen
 WHERE canonical_scheme = 'http' AND canonical_host = 'h' AND base = '/r/dists/x'`).Scan(&n)
			sawMarker = n == 1
			return err
		})
	}()
	waitQueued(2)
	close(block)
	if err := <-storeErr; err != nil {
		t.Fatal(err)
	}
	if err := <-later; err != nil {
		t.Fatal(err)
	}
	if !sawMarker {
		t.Fatal("a writer operation enqueued after StoreFetched did not see its marker")
	}
}
```

Create `internal/cache/suite_touch_test.go`:

```go
package cache

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func requestedAt(t *testing.T, c *Cache, key SuiteKey) int64 {
	t.Helper()
	s, err := c.GetSuiteFreshness(context.Background(), key.CanonicalScheme, key.CanonicalHost, key.SuitePath)
	if err != nil {
		t.Fatal(err)
	}
	return derefI64(s.LastRequestedAt)
}

// Spec C3: touches are coalesced to one write per suite per hour, in
// monotonic time; the row's creation counts as its first touch.
func TestTouchSuite_WritesAtMostOncePerHour(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := openCache(t)
		ctx := context.Background()
		key := sk("h", "/r/dists/x")
		t0 := nowUnix()
		seedRow(t, c, key, t0)
		touch := func(after time.Duration, want int64) {
			t.Helper()
			time.Sleep(after)
			if err := c.TouchSuite(ctx, key); err != nil {
				t.Fatal(err)
			}
			if got := requestedAt(t, c, key); got != want {
				t.Fatalf("after %v more: last_requested_at = %d, want %d", after, got-t0, want-t0)
			}
		}
		touch(10*time.Minute, t0)      // within the creation's hour: no write
		touch(50*time.Minute, t0+3600) // an hour after the creation: written
		touch(59*time.Minute, t0+3600) // within that write's hour: no write
		touch(time.Minute, t0+7200)    // an hour after it: written
	})
}

// Concurrent touches of one suite write once.
func TestTouchSuite_ConcurrentTouchesWriteOnce(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/r/dists/x")
	if err := c.SeedSuiteForTest(ctx, SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`CREATE TABLE touch_writes (n INTEGER);
CREATE TRIGGER count_touch AFTER UPDATE OF last_requested_at ON suite_freshness
BEGIN INSERT INTO touch_writes VALUES (1); END`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.TouchSuite(ctx, key); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var n int
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM touch_writes`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("20 concurrent touches wrote %d times, want 1", n)
	}
}

// Spec C3: the coalescing map holds only tracked suites — a touch of a
// suite without a row writes nothing, creates nothing and leaves no entry
// — and an eviction purges the evictee's entry.
func TestTouchSuite_MapHoldsOnlyTrackedSuites(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, evictNow)()
	for i := range 50 {
		if err := c.TouchSuite(ctx, sk("minted", "/dists/"+strings.Repeat("x", i+1))); err != nil {
			t.Fatal(err)
		}
	}
	if n := c.TouchedSuites(); n != 0 {
		t.Fatalf("map holds %d entries after touching 50 untracked suites, want 0", n)
	}
	if rowExists(t, c, sk("minted", "/dists/x")) {
		t.Fatal("a touch created a row")
	}
	fourRows(t, c) // four creations: four entries
	if _, err := c.CreateSuite(ctx, SuiteSeed{SuiteKey: sk("e", "/new"), RequestedAt: evictNow}, SuiteCaps{Global: 4},
		newFakeGuard()); err != nil {
		t.Fatal(err)
	}
	c.touches.mu.Lock()
	_, evictee := c.touches.last[sk("b", "/idle")]
	_, created := c.touches.last[sk("e", "/new")]
	n := len(c.touches.last)
	c.touches.mu.Unlock()
	if evictee || !created || n != 4 {
		t.Fatalf("map after the eviction: evictee %v, created %v, %d entries; want false, true, 4", evictee, created, n)
	}
}

// At touchSweepAt entries the map drops the ones older than touchEvery.
func TestTouchSuite_MapIsSwept(t *testing.T) {
	c := openCache(t)
	c.touches.mu.Lock()
	c.touches.last = make(map[SuiteKey]time.Time)
	old := time.Now().Add(-2 * touchEvery)
	for i := range touchSweepAt - 1 {
		c.touches.last[sk("old", "/"+strings.Repeat("o", i+1))] = old
	}
	c.touches.last[sk("fresh", "/f")] = time.Now()
	c.touches.mu.Unlock()
	c.noteTouch(sk("new", "/n"))
	if n := c.TouchedSuites(); n != 2 {
		t.Fatalf("map after the sweep = %d entries, want 2 (the fresh one and the new one)", n)
	}
}

// Spec C3: a vouched .deb serve touches the suite whose current snapshot
// declared it, so DeclaredHashesForPath names that suite.
func TestDeclaredHashesForPath_NamesTheVouchingSuites(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	const deb = "/ubuntu/pool/main/h/hello/hello_1_amd64.deb"
	debHash := strings.Repeat("a", 64)
	for _, suite := range []string{"/ubuntu/dists/noble", "/ubuntu/dists/noble-updates"} {
		h := seedBlob(t, c, "anchor of "+suite)
		id, _, err := c.InsertTrackedCandidateForTest(ctx, SnapshotCandidate{CanonicalScheme: "http",
			CanonicalHost: "archive.example", SuitePath: suite, InReleaseHash: &h})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.CommitAdoption(ctx, id, []SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: h, DeclaredSHA256: h}},
			nil, []PackageHash{{CanonicalScheme: "http", CanonicalHost: "archive.example", Path: deb,
				DeclaredSHA256: debHash, SnapshotID: id}}, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := c.DeclaredHashesForPath(ctx, "http", "archive.example", deb)
	if err != nil {
		t.Fatal(err)
	}
	var suites []string
	for _, r := range rows {
		suites = append(suites, r.SuitePath)
	}
	slices.Sort(suites)
	if !slices.Equal(suites, []string{"/ubuntu/dists/noble", "/ubuntu/dists/noble-updates"}) {
		t.Fatalf("vouching suites = %v", suites)
	}
}
```

Append to `internal/cache/suite_rows_test.go`:

```go
// SuiteExists is the handler's cheap tracked-suite test.
func TestSuiteExists(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: afSuite}
	if ok, err := c.SuiteExists(ctx, key); err != nil || ok {
		t.Fatalf("before creation: %v, %v", ok, err)
	}
	if _, err := c.CreateSuite(ctx, SuiteSeed{SuiteKey: key, RequestedAt: 1}, SuiteCaps{}, nil); err != nil {
		t.Fatal(err)
	}
	if ok, err := c.SuiteExists(ctx, key); err != nil || !ok {
		t.Fatalf("after creation: %v, %v", ok, err)
	}
	if ok, err := c.SuiteExists(ctx, SuiteKey{CanonicalScheme: "https", CanonicalHost: "h", SuitePath: afSuite}); err != nil || ok {
		t.Fatalf("another scheme: %v, %v", ok, err)
	}
}
```

Append to `internal/cache/signed_seen_test.go` (adding `"fmt"` and `"strings"` to its imports):

```go
// Deferred minor A-24: a batch writes every mark, over several 500-row
// transactions, and duplicates within a batch keep the newest stamp.
func TestMarkSignedSeenBatch_ManyMarksInChunks(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	var marks []SignedSeenMark
	for i := range 1201 {
		marks = append(marks, SignedSeenMark{CanonicalScheme: "http", CanonicalHost: "h",
			Base: fmt.Sprintf("/r/dists/s%04d", i), SeenAt: int64(1000 + i)})
	}
	marks = append(marks,
		SignedSeenMark{CanonicalScheme: "http", CanonicalHost: "h", Base: "/dup", SeenAt: 100},
		SignedSeenMark{CanonicalScheme: "http", CanonicalHost: "h", Base: "/dup", SeenAt: 50})
	if err := c.MarkSignedSeenBatch(ctx, marks); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM suite_signed_seen WHERE base LIKE '/r/dists/s%'`).Scan(&n); err != nil || n != 1201 {
		t.Fatalf("markers = %d, %v; want 1201", n, err)
	}
	if at, _, _ := c.SignedSeen(ctx, "http", "h", "/r/dists/s1200"); at != 2200 {
		t.Fatalf("last mark of the third chunk = %d, want 2200", at)
	}
	if at, _, _ := c.SignedSeen(ctx, "http", "h", "/dup"); at != 100 {
		t.Fatalf("duplicate = %d, want the newest stamp 100", at)
	}
}

// Deferred minor A-25: a failing mark names the batch writer.
func TestMarkSignedSeenBatch_ErrorsArePrefixed(t *testing.T) {
	c := openCache(t)
	if _, err := c.db.Exec(`CREATE TRIGGER fail_mark BEFORE INSERT ON suite_signed_seen
BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatal(err)
	}
	err := c.MarkSignedSeenBatch(context.Background(), []SignedSeenMark{{CanonicalScheme: "http", CanonicalHost: "h", Base: "/b", SeenAt: 1}})
	if err == nil || !strings.HasPrefix(err.Error(), "MarkSignedSeenBatch: mark signed-seen http://h/b: ") {
		t.Fatalf("err = %v; want it prefixed \"MarkSignedSeenBatch: mark signed-seen http://h/b: \"", err)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go vet ./internal/cache/`
Expected: the test package does not compile (first error `c.StoreFetched undefined`; `FetchedRow`, `TouchSuite`, `TouchedSuites`, `SuiteExists`, `c.touches`, `touchEvery`, `touchSweepAt` and `DeclaredHash.SuitePath` are undefined too).

- [ ] **Step 3: Implement**

Create `internal/cache/fetched_row.go`:

```go
package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
)

// FetchedRow is what a metadata miss stores in one writer transaction,
// before its body is served (spec C3): the url_path row and, for a suite
// file, the suite row and the signed-seen marker.
type FetchedRow struct {
	Row URLPath
	// Suite is the suite the path belongs to; zero when it has none.
	Suite SuiteKey
	// Create, when set, creates Suite's row (Create.SuiteKey == Suite) if
	// it has none, under the caps, with cap eviction.
	Create *SuiteSeed
	// MarkSeenAt, when non-zero, marks the suite's base signed-seen if the
	// suite has a row once Create has run: the body is a sniff-passing
	// InRelease or Release.gpg.
	MarkSeenAt int64
}

// FetchedResult reports what StoreFetched did for the suite.
type FetchedResult struct {
	// Create is the creation's result (zero Outcome when Create was nil).
	Create CreateResult
	// Tracked: the suite has a row after the transaction.
	Tracked bool
	// Marked: the signed-seen marker was written.
	Marked bool
}

// StoreFetched stores a metadata miss's url_path row, creates the suite row
// and writes the signed-seen marker as m asks, all in ONE writer
// transaction (spec C3): a signed anchor body is never served before its
// base has a marker, and a failure stores nothing — the caller then fails
// the miss (502, no body) as it does today. A cap rejection is not a
// failure: the url_path row is stored and the suite stays untracked (no
// marker).
//
// AIDEV-NOTE: the writer is a FIFO queue drained by one goroutine
// (submitWrite), so an unsigned CommitAdoption enqueued after this
// operation runs after it, and its in-transaction marker re-read (phase E)
// sees the marker (TestStoreFetched_MarkerPrecedesALaterWriterOperation).
func (c *Cache) StoreFetched(ctx context.Context, m FetchedRow, caps SuiteCaps, guard SuiteGuard) (FetchedResult, error) {
	var held *heldSuite
	if m.Create != nil {
		h, err := c.holdEvictionCandidate(ctx, caps, guard)
		if err != nil {
			return FetchedResult{}, err
		}
		held = h
	}
	isMD := int64(0)
	if m.Row.IsMetadata {
		isMD = 1
	}
	var got FetchedResult
	err := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("StoreFetched: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		u := m.Row
		if _, err := tx.ExecContext(ctx, putURLPathSQL,
			u.CanonicalScheme, u.CanonicalHost, u.Path, u.BlobHash, u.UpstreamURL,
			isMD, u.LastRequestedAt, u.RequestCount, u.LastFetchedAt,
			u.UpstreamETag, u.UpstreamLastMod); err != nil {
			return fmt.Errorf("StoreFetched: url_path: %w", err)
		}
		var r FetchedResult
		if m.Create != nil {
			cr, err := createSuiteTx(ctx, tx, *m.Create, caps, held, nowUnix())
			if err != nil {
				return err
			}
			r.Create = cr
		}
		if m.Suite != (SuiteKey{}) {
			var one int
			err := tx.QueryRowContext(ctx, `
SELECT 1 FROM suite_freshness
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`,
				m.Suite.CanonicalScheme, m.Suite.CanonicalHost, m.Suite.SuitePath).Scan(&one)
			switch {
			case err == nil:
				r.Tracked = true
			case !errors.Is(err, sql.ErrNoRows):
				return fmt.Errorf("StoreFetched: read suite row: %w", err)
			}
		}
		if m.MarkSeenAt != 0 && r.Tracked {
			if err := markSignedSeen(ctx, tx, m.Suite.CanonicalScheme, m.Suite.CanonicalHost,
				aptpath.MarkerBase(m.Suite.SuitePath), m.MarkSeenAt); err != nil {
				return fmt.Errorf("StoreFetched: %w", err)
			}
			r.Marked = true
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("StoreFetched: commit: %w", err)
		}
		got = r
		return nil
	})
	if err != nil {
		// Never read got after an error (see CreateSuite).
		if m.Create != nil {
			c.afterCreate(m.Create.SuiteKey, CreateResult{}, held)
		}
		return FetchedResult{}, err
	}
	if m.Create != nil {
		c.afterCreate(m.Create.SuiteKey, got.Create, held)
	}
	return got, nil
}
```

Create `internal/cache/suite_touch.go`:

```go
package cache

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"
)

// touchEvery is spec C3's touch coalescing: at most one last_requested_at
// write per suite per hour.
const touchEvery = time.Hour

// touchSweepAt is the coalescing map size at which entries older than
// touchEvery (which no longer suppress anything) are dropped.
const touchSweepAt = 4096

// suiteTouches is the in-memory coalescing state of TouchSuite: when each
// tracked suite's last_requested_at was last written, in monotonic time.
type suiteTouches struct {
	mu   sync.Mutex
	last map[SuiteKey]time.Time
}

// TouchSuite records client interest in a suite (spec C3): it sets
// last_requested_at = now on the suite's existing row (UPDATE-only), at
// most once per suite per hour. A touch within the hour of the previous
// write, or of the row's creation, writes nothing. The coalescing map
// holds only suites whose row the write found (and, for the length of one
// write, a suite being touched); it is purged when this cache deletes a
// suite row.
//
// AIDEV-NOTE: suite_freshness.last_requested_at is the SUITE's interest
// stamp (eager-adoption window, eviction, reaping), distinct from
// url_path.last_requested_at; only TouchSuite and the row's creation write
// it, from client-serving paths. Coalescing uses monotonic time, so a wall
// clock step neither suppresses nor multiplies writes.
func (c *Cache) TouchSuite(ctx context.Context, key SuiteKey) error {
	if !c.claimTouch(key) {
		return nil
	}
	now := nowUnix()
	found := false
	err := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `
UPDATE suite_freshness SET last_requested_at = ?
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`,
			now, key.CanonicalScheme, key.CanonicalHost, key.SuitePath)
		if err != nil {
			return fmt.Errorf("TouchSuite: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("TouchSuite: rows: %w", err)
		}
		found = n > 0
		return nil
	})
	// Never read found after an error: the operation may still run. A
	// failed touch, or one that found no row, leaves no entry, so the next
	// serve tries again and an untracked suite never stays in the map.
	if err != nil || !found {
		c.forgetTouch(key)
		return err
	}
	return nil
}

// TouchedSuites is the size of the touch-coalescing map (tests and
// diagnostics).
func (c *Cache) TouchedSuites() int {
	c.touches.mu.Lock()
	defer c.touches.mu.Unlock()
	return len(c.touches.last)
}

// claimTouch reports whether key's touch is due — none written within
// touchEvery — and if so records it now, so concurrent touches of one
// suite write once.
func (c *Cache) claimTouch(key SuiteKey) bool {
	c.touches.mu.Lock()
	defer c.touches.mu.Unlock()
	if t, ok := c.touches.last[key]; ok && time.Since(t) < touchEvery {
		return false
	}
	c.noteTouchLocked(key)
	return true
}

// noteTouch records that key's last_requested_at was just written (by its
// creation).
func (c *Cache) noteTouch(key SuiteKey) {
	c.touches.mu.Lock()
	defer c.touches.mu.Unlock()
	c.noteTouchLocked(key)
}

func (c *Cache) noteTouchLocked(key SuiteKey) {
	if c.touches.last == nil {
		c.touches.last = make(map[SuiteKey]time.Time)
	}
	if len(c.touches.last) >= touchSweepAt {
		for k, t := range c.touches.last {
			if time.Since(t) >= touchEvery {
				delete(c.touches.last, k)
			}
		}
	}
	c.touches.last[key] = time.Now()
}

// forgetTouch drops key's entry: its row is gone, or its touch failed.
func (c *Cache) forgetTouch(key SuiteKey) {
	c.touches.mu.Lock()
	defer c.touches.mu.Unlock()
	delete(c.touches.last, key)
}
```

In `internal/cache/cache.go`, add to the `Cache` struct after `revisionEpoch uint64`:

```go
	// touches coalesces TouchSuite writes (spec C3).
	touches suiteTouches
```

In `internal/cache/queries.go`, move `PutURLPath`'s statement out of the function into a package constant `putURLPathSQL` (unchanged text, directly after `PutURLPath`, with the doc `// putURLPathSQL is PutURLPath's upsert, shared with StoreFetched (one` / `// statement, so the miss path's url_path row matches PutURLPath's).`), and have `PutURLPath` execute `putURLPathSQL`.

In `internal/cache/suite_rows.go`, replace `afterCreate` (doc included) with

```go
// afterCreate releases the held eviction candidate, telling the guard
// whether its row was deleted, and keeps the touch-coalescing map in step
// with a committed eviction (the evictee's entry goes) or creation (its
// insert stamped last_requested_at, so it counts as a touch).
//
// AIDEV-NOTE: after a failed submit (cancelled context, closing cache)
// the outcome is unknown and the candidate is released as not deleted. A
// queued operation may still commit its eviction later: the evictee's
// lock-map, flip-gate and touch entries then stay (one each, bounded), and a
// check that started meanwhile meets a vanished row, which I-4's
// re-validation and the UPDATE-only writers make harmless (its candidate,
// if any, was retired: CommitAdoption refuses it).
func (c *Cache) afterCreate(key SuiteKey, res CreateResult, held *heldSuite) {
	if res.Evicted != nil {
		c.forgetTouch(*res.Evicted)
	}
	if res.Outcome == SuiteCreated {
		c.noteTouch(key)
	}
	if held != nil {
		held.release(res.Evicted != nil)
	}
}
```

pass `seed.SuiteKey` as its new first argument at both call sites in `CreateSuite`, and add before `UpdateSuiteFreshness`:

```go
// SuiteExists reports whether the suite has a suite_freshness row: the
// handler's cheap tracked-suite test (one indexed read; the flip barrier
// registers only tracked suites' anchor serves).
func (c *Cache) SuiteExists(ctx context.Context, key SuiteKey) (bool, error) {
	var one int
	err := c.db.QueryRowContext(ctx, `
SELECT 1 FROM suite_freshness
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`,
		key.CanonicalScheme, key.CanonicalHost, key.SuitePath).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("SuiteExists: %w", err)
	}
	return true, nil
}
```

In `internal/cache/adoption.go`: `DeclaredHash` gains, after `PackageName`,

```go
	// SuitePath is the suite whose current snapshot the row belongs to
	// (spec C3: a vouched .deb serve touches that suite).
	SuitePath string
```

and `DeclaredHashesForPath` selects `SELECT DISTINCT p.declared_sha256, p.snapshot_id, p.package_name, sf.suite_path` and scans `&d.SuitePath` last (one snapshot is the current snapshot of one suite, so the row count is unchanged).

In `internal/cache/signed_seen.go`, the `markSignedSeen` AIDEV-NOTE becomes

```go
// AIDEV-NOTE: the marker is the never-downgrade authority for unsigned
// adoption (spec C5). Writers: CommitAdoption (signed forms, in its
// transaction), the startup legacy audit and anchor pass (spec §6.1), and
// from phase D the handler: on a miss, StoreFetched (the same writer
// transaction as the anchor's url_path row, before the body is served);
// on a suite-row creation, CreateSuite's transaction (SuiteSeed.SignedSeenAt)
// and markCachedSignedAnchors for the suite's other cached anchors. It is
// written only for suites with a suite_freshness row, and never for an
// unsigned snapshot or a plain-text anchor.
```

and, deferred minor A-25, `MarkSignedSeenBatch`'s per-row error is returned as `fmt.Errorf("MarkSignedSeenBatch: %w", err) // deferred minor A-25`.

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/cache/ && go build ./...`
Expected: PASS.

- [ ] **Step 5: Mutation check (do not commit)**

One at a time, reverting each: (a) `if m.MarkSeenAt != 0 {` (mark untracked suites) → `TestStoreFetched_MarksOnlyTrackedSuites` FAILS (`Marked:true`); (b) pass `nil` for `held` in `StoreFetched` → `TestStoreFetched_EvictsAtTheGlobalCap` FAILS (`global_cap`); (c) ignore the marker's error → `TestStoreFetched_AFailedOperationStoresNothing` FAILS; (d) write the marker in a second writer operation after the first (`MarkSignedSeenBatch` after `submitWrite` returns) → `TestStoreFetched_MarkerPrecedesALaterWriterOperation` and `TestStoreFetched_AFailedOperationStoresNothing` FAIL; (e) `claimTouch` checks without recording → `TestTouchSuite_ConcurrentTouchesWriteOnce` and `TestTouchSuite_WritesAtMostOncePerHour` FAIL; (f) drop `noteTouch` from `afterCreate` → `TestTouchSuite_WritesAtMostOncePerHour` FAILS (`after 10m0s more: last_requested_at = 600, want 0`); (g) keep the entry when the UPDATE found no row → `TestTouchSuite_MapHoldsOnlyTrackedSuites` FAILS (`50 entries`); (h) drop `forgetTouch(*res.Evicted)` → the same test FAILS (`evictee true`); (i) never sweep → `TestTouchSuite_MapIsSwept` FAILS (`4097 entries`); (j) select `''` for the suite path → `TestDeclaredHashesForPath_NamesTheVouchingSuites` FAILS; (k) undo A-25 → `TestMarkSignedSeenBatch_ErrorsArePrefixed` FAILS; (l) `marks = nil` after the first chunk → `TestMarkSignedSeenBatch_ManyMarksInChunks` FAILS (`markers = 500`).

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/cache/fetched_row.go internal/cache/fetched_row_test.go internal/cache/suite_touch.go internal/cache/suite_touch_test.go internal/cache/queries.go internal/cache/cache.go internal/cache/suite_rows.go internal/cache/adoption.go internal/cache/signed_seen.go internal/cache/suite_rows_test.go internal/cache/signed_seen_test.go
golangci-lint run ./internal/cache/...
git add internal/cache/
git commit -m "Store a miss's url_path row, suite row and marker in one writer operation; coalesce suite touches"
```

---

### Task 7: I-4 re-validation in the checker; `HoldSuite`; the scheduler's queue bound

Spec C3: "`Check` and `Reconcile` require an existing row before `LoadOrStore` (I-4) and re-validate after `TryLock` that the lock map still holds the same mutex and the row still exists" (Task 4 added `Check`'s pre-read; `Reconcile` already read first); ruling R-D1's guard: `*freshness.Checker` implements `cache.SuiteGuard` — `HoldSuite` takes the candidate's mutex with `TryLock`, and `release(true)` drops the lock-map entry (`CompareAndDelete`) and the flip-gate entry (`Purge`) while still holding it; a check that finds its row gone drops its own entry; the scheduler flushes an evicted suite's queued or parked entry through I-4 (no row → `skipped` → done; overview "A reaped suite's parked scheduler entry"). Spec C4's queue bound ("at most `freshness.max_suites` entries in all", phase B plan deviation 4; design note 15): `freshness.Config.MaxSuites`, wired in Task 8. The `locks` field's AIDEV-NOTE ("entries are not actively reaped") and `Reconcile`'s ("Compare Check, which does LoadOrStore unconditionally") are no longer true and are rewritten.

**Files:**
- Create: `internal/freshness/suite_lock.go`, `internal/freshness/suite_lock_test.go`
- Modify: `internal/freshness/freshness.go` (`Config.MaxSuites`; `Checker.maxSuites`, `lockLoadedHook`; `checkOutcome.vanished`; `check`, `checkLocked`, `Reconcile`; two AIDEV-NOTEs), `internal/freshness/scheduler.go` (`tick`)

**Interfaces:**
- Consumes: Task 4's `suiteRowExists`, `resultSkipped`; Task 5's `cache.SuiteGuard`; `Adopter.FlipGate()` and `(*flipgate.Gate).Purge`/`AnchorStart`/`Len` (nil-safe); test helpers `newFakeCache`, `seedFakeSuite`, `keyOf`, `fc.track`, `fc.putSuite`, `changedUpstream` (`panic_recovery_test.go`), `newFakeFetcher`, `passThroughVerifier`, `newSchedQueues`, `admitAll`, `admitNone`, `checkDone`.
- Produces:
  - `func (c *Checker) HoldSuite(key cache.SuiteKey) (func(deleted bool), bool)` (the guard Task 9 passes to the cache); `func (c *Checker) LockCount() int` (Task 11).
  - `freshness.Config.MaxSuites int` (Task 8 wires `freshness.max_suites`).
  - `Checker.lockLoadedHook func(key string)` (test seam).
  - Test helpers `gatedAdopter(t) (*Adopter, *cache.Cache, *flipgate.Gate)`, `lockChecker(t, c Cache, f Fetcher, ad *Adopter) *Checker`.

- [ ] **Step 1: Write the failing tests**

Create `internal/freshness/suite_lock_test.go`:

```go
package freshness

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
)

var lockRef = SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}
var lockKey = cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}

// gatedAdopter is an adopter over a real cache whose flip gate is enabled.
func gatedAdopter(t *testing.T) (*Adopter, *cache.Cache, *flipgate.Gate) {
	t.Helper()
	rc, err := cache.Open(context.Background(), t.TempDir(), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	gate, err := flipgate.New(flipgate.Config{Quiet: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ad, err := NewAdopter(AdoptionConfig{Cache: rc, Fetcher: newFakeFetcher(), Verifier: passThroughVerifier{},
		HostLimiter: hostsem.New(8), FlipGate: gate, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	return ad, rc, gate
}

func lockChecker(t *testing.T, c Cache, f Fetcher, ad *Adopter) *Checker {
	t.Helper()
	ck, err := New(Config{Cache: c, Fetcher: f, HostLimiter: hostsem.New(8), Logger: discardLogger(), Adopter: ad})
	if err != nil {
		t.Fatal(err)
	}
	return ck
}

// R-D1: HoldSuite takes a free suite's mutex without waiting and refuses a
// busy one; release(false) only unlocks, release(true) also drops the
// suite's lock-map and flip-gate entries (once).
func TestHoldSuite_TakesOnlyAFreeSuiteAndPurgesOnDelete(t *testing.T) {
	ad, _, gate := gatedAdopter(t)
	fc := newFakeCache()
	fc.track("http", "h", "/r/dists/x")
	c := lockChecker(t, fc, newTestFetcher(t), ad)
	ctx := context.Background()
	gate.AnchorStart(ctx, flipgate.Key("http", "h", "/r/dists/x")).Done()
	release, ok := c.HoldSuite(lockKey)
	if !ok {
		t.Fatal("a free suite was not held")
	}
	if _, ok := c.HoldSuite(lockKey); ok {
		t.Fatal("a held suite was held twice")
	}
	if rep := c.check(ctx, lockRef, checkOpts{}); rep.result != resultBusy {
		t.Fatalf("check of a held suite = %q, want %q", rep.result, resultBusy)
	}
	release(false)
	if c.LockCount() != 1 || gate.Len() != 1 {
		t.Fatalf("after release(false): %d lock entries, %d gate entries; want 1 and 1", c.LockCount(), gate.Len())
	}
	release, ok = c.HoldSuite(lockKey)
	if !ok {
		t.Fatal("a released suite was not held again")
	}
	release(true)
	release(true) // idempotent
	if c.LockCount() != 0 || gate.Len() != 0 {
		t.Fatalf("after release(true): %d lock entries, %d gate entries; want 0 and 0", c.LockCount(), gate.Len())
	}
}

// Spec C3 I-4: a check re-validates after TryLock that the lock map still
// holds its mutex. One that loaded the mutex of a suite evicted (and
// re-created) meanwhile is skipped, so it never runs beside a check that
// takes the new entry.
func TestCheck_RevalidatesItsLockAfterTryLock(t *testing.T) {
	fc := newFakeCache()
	seedFakeSuite(fc, "h", "/r/dists/x", nil)
	up := &changedUpstream{body: []byte("x")}
	c := lockChecker(t, fc, up, nil)
	c.lockLoadedHook = func(string) {
		c.lockLoadedHook = nil
		release, ok := c.HoldSuite(lockKey) // an eviction in the window
		if !ok {
			t.Error("HoldSuite in the window failed")
			return
		}
		release(true)
	}
	ctx := context.Background()
	if rep := c.check(ctx, lockRef, checkOpts{bypassCooldown: true}); rep.result != resultSkipped || up.calls.Load() != 0 {
		t.Fatalf("check on a stale mutex = %q with %d upstream calls; want skipped, 0", rep.result, up.calls.Load())
	}
	if rep := c.check(ctx, lockRef, checkOpts{bypassCooldown: true}); rep.result == resultSkipped || up.calls.Load() != 1 {
		t.Fatalf("next check = %q with %d upstream calls; want it to run", rep.result, up.calls.Load())
	}
}

// Spec C3 I-4: a check that finds its row gone after the pre-read writes
// nothing, calls no upstream, and drops its lock-map entry.
func TestCheck_VanishedRowDropsItsLockEntry(t *testing.T) {
	fc := newFakeCache()
	seedFakeSuite(fc, "h", "/r/dists/x", nil)
	up := &changedUpstream{body: []byte("x")}
	c := lockChecker(t, fc, up, nil)
	c.lockLoadedHook = func(string) {
		c.lockLoadedHook = nil
		fc.mu.Lock()
		delete(fc.suites, keyOf("http", "h", "/r/dists/x"))
		fc.mu.Unlock()
	}
	if rep := c.check(context.Background(), lockRef, checkOpts{bypassCooldown: true}); rep.result != resultSkipped {
		t.Fatalf("result = %q, want %q", rep.result, resultSkipped)
	}
	if up.calls.Load() != 0 || c.LockCount() != 0 {
		t.Fatalf("%d upstream calls, %d lock entries; want 0 and 0", up.calls.Load(), c.LockCount())
	}
	if _, ok := fc.suite("http", "h", "/r/dists/x"); ok {
		t.Fatal("the vanished row was written back")
	}
}

// Spec C3 I-4: Reconcile re-validates under the mutex — the lock map must
// still hold it and the row must still name the snapshot read before.
func TestReconcile_RevalidatesUnderTheMutex(t *testing.T) {
	ad, _, _ := gatedAdopter(t)
	fc := newFakeCache()
	id := int64(42)
	fc.putSuite(cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", CurrentSnapshotID: &id})
	c := lockChecker(t, fc, newTestFetcher(t), ad)
	ctx := context.Background()
	c.lockLoadedHook = func(string) { // re-adopted in the window
		c.lockLoadedHook = nil
		other := int64(43)
		fc.putSuite(cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", CurrentSnapshotID: &other})
	}
	if c.Reconcile(ctx, "http", "h", "/r/dists/x") {
		t.Fatal("reconciled a snapshot that is no longer current")
	}
	c.lockLoadedHook = func(string) { // evicted in the window
		c.lockLoadedHook = nil
		if release, ok := c.HoldSuite(lockKey); ok {
			release(true)
		}
	}
	if c.Reconcile(ctx, "http", "h", "/r/dists/x") {
		t.Fatal("reconciled on a stale mutex")
	}
	if !c.Reconcile(ctx, "http", "h", "/r/dists/x") {
		t.Fatal("an undisturbed reconcile did not start")
	}
	c.WaitForAdoptions()
}

// R-D1 end to end: cap eviction with the checker as the guard skips a busy
// suite and purges an evicted suite's lock-map and flip-gate entries.
func TestCreateSuite_EvictionUnderTheChecker(t *testing.T) {
	ad, rc, gate := gatedAdopter(t)
	c := lockChecker(t, rc, newTestFetcher(t), ad)
	ctx := context.Background()
	k := func(s string) cache.SuiteKey {
		return cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: s}
	}
	old := time.Now().Unix() - 3*86400
	for _, s := range []string{"/a", "/b"} {
		if _, err := rc.CreateSuite(ctx, cache.SuiteSeed{SuiteKey: k(s), RequestedAt: old}, cache.SuiteCaps{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	release, ok := c.HoldSuite(k("/a")) // /a is busy (a check holds it)
	if !ok {
		t.Fatal("HoldSuite(/a)")
	}
	gate.AnchorStart(ctx, flipgate.Key("http", "h", "/b")).Done()
	res, err := rc.CreateSuite(ctx, cache.SuiteSeed{SuiteKey: k("/c"), RequestedAt: time.Now().Unix()}, cache.SuiteCaps{Global: 2}, c)
	if err != nil || res.Outcome != cache.SuiteCreated || res.Evicted == nil || *res.Evicted != k("/b") {
		t.Fatalf("CreateSuite = %+v, %v; want created, evicting /b (/a is busy)", res, err)
	}
	release(false)
	if c.LockCount() != 1 || gate.Len() != 0 {
		t.Fatalf("%d lock entries (want /a's only), %d gate entries (want 0)", c.LockCount(), gate.Len())
	}
}

// Spec C4: the scheduler keeps at most freshness.max_suites suites queued,
// waiting or in flight; later ticks refill to the bound.
func TestTick_QueuesAtMostMaxSuites(t *testing.T) {
	fc := newFakeCache()
	for i := range 30 {
		seedFakeSuite(fc, fmt.Sprintf("h%d", i%5), fmt.Sprintf("/s%d/dists/x", i), nil)
	}
	c, err := New(Config{Cache: fc, Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute,
		MaxSuites: 10, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	q := newSchedQueues()
	c.tick(ctx, q)
	if q.len() != 10 {
		t.Fatalf("queued after one tick = %d, want 10", q.len())
	}
	c.tick(ctx, q)
	if q.len() != 10 {
		t.Fatalf("queued after two ticks = %d, want 10", q.len())
	}
	for range 4 {
		it, ok := q.pop(admitAll)
		if !ok {
			t.Fatal("nothing to pop")
		}
		q.done(it)
	}
	c.tick(ctx, q)
	if q.len() != 10 {
		t.Fatalf("queued after four checks and a tick = %d, want 10", q.len())
	}
}

// R-D1 / I-4: a suite evicted while it waits in the scheduler for a slot
// flushes on its next offer — no row, so the check is skipped and the
// entry is done — with no lock-map entry left behind.
func TestScheduler_FlushesAnEvictedWaiter(t *testing.T) {
	fc := newFakeCache()
	seedFakeSuite(fc, "h", "/r/dists/x", nil)
	c := lockChecker(t, fc, newTestFetcher(t), nil)
	ctx := context.Background()
	q := newSchedQueues()
	c.tick(ctx, q)
	it, ok := q.pop(admitNone)
	if !ok {
		t.Fatal("nothing queued")
	}
	q.finish(checkDone{item: it, report: checkReport{result: "changed_queued"}}) // waits for a slot
	fc.mu.Lock()
	delete(fc.suites, keyOf("http", "h", "/r/dists/x")) // evicted meanwhile
	fc.mu.Unlock()
	it, ok = q.pop(admitAll)
	if !ok || !it.waiting {
		t.Fatalf("offer = %+v, %v; want the waiter", it, ok)
	}
	rep := c.checkRecovered(ctx, it)
	q.finish(checkDone{item: it, report: rep})
	if rep.result != resultSkipped || q.len() != 0 || q.waiting() != 0 || c.LockCount() != 0 {
		t.Fatalf("after the offer: result %q, queued %d, waiting %d, locks %d; want skipped, 0, 0, 0",
			rep.result, q.len(), q.waiting(), c.LockCount())
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/freshness/ -run 'TestHoldSuite_'`
Expected: build failure naming `c.HoldSuite undefined`, `c.LockCount undefined`, `c.lockLoadedHook undefined` (and `unknown field MaxSuites`).

- [ ] **Step 3: Implement**

Create `internal/freshness/suite_lock.go`:

```go
package freshness

import (
	"sync"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"
)

// The checker is the cache's SuiteGuard: spec C3 cap eviction runs under
// the evicted suite's mutex.
var _ cache.SuiteGuard = (*Checker)(nil)

// HoldSuite takes the suite's mutex without waiting (cache.SuiteGuard): ok
// is false while a check, adoption, repair or reconcile holds it.
// release(true) — the suite's row was deleted while held — drops its
// lock-map entry and its flip-gate entry, then unlocks; release(false)
// only unlocks. Spec C3 cap eviction uses it; phase F's reaper purges the
// same way.
//
// AIDEV-NOTE: the entry is dropped while the mutex is still held, so a
// check that loaded this mutex earlier fails check's post-TryLock
// re-validation (the map no longer holds it) instead of running beside a
// check of a re-created suite.
func (c *Checker) HoldSuite(key cache.SuiteKey) (func(deleted bool), bool) {
	k := suiteKey(key.CanonicalScheme, key.CanonicalHost, key.SuitePath)
	muVal, _ := c.locks.LoadOrStore(k, &sync.Mutex{})
	mu := muVal.(*sync.Mutex)
	if !mu.TryLock() {
		return nil, false
	}
	if cur, ok := c.locks.Load(k); !ok || cur != muVal {
		mu.Unlock()
		return nil, false
	}
	var once sync.Once
	return func(deleted bool) {
		once.Do(func() {
			if deleted {
				c.locks.CompareAndDelete(k, muVal)
				c.adopter.FlipGate().Purge(flipgate.Key(key.CanonicalScheme, key.CanonicalHost, key.SuitePath))
			}
			mu.Unlock()
		})
	}, true
}

// LockCount is the number of suites with a lock-map entry (tests and
// diagnostics). I-4 keeps it at most the number of suite rows, plus the
// suites a check is unwinding right now.
func (c *Checker) LockCount() int {
	n := 0
	c.locks.Range(func(any, any) bool { n++; return true })
	return n
}
```

In `internal/freshness/freshness.go`:

- `Config` gains, after `Logger`: `// MaxSuites is freshness.max_suites (spec C4, C11): the scheduler` / `// keeps at most this many suites queued, waiting for a slot or in` / `// flight. Zero is unbounded (tests).` / `MaxSuites int`; `Checker` gains `maxSuites int` (after `refresh`), set from it in `New`.
- Replace the `locks` field's comment and add the hook after it:

```go
	// locks holds *sync.Mutex per suite key. SPEC §7.3 specifies the
	// lock is in-memory; it is held for a check and for the adoption,
	// repair or reconcile the check hands it to.
	//
	// AIDEV-NOTE: I-4 (spec C3) bounds the map by the suite table: Check
	// and Reconcile read the suite's row before LoadOrStore and, after
	// TryLock, re-validate that the map still holds the same mutex; a check
	// that then finds the row gone drops its entry (CompareAndDelete), and
	// cap eviction (phase F's reaper too) drops the evictee's through
	// HoldSuite's release(true). An entry outlives its row only while a
	// check that loaded it unwinds (LockCount).
	locks sync.Map

	// lockLoadedHook, when set (tests only), runs in check and Reconcile
	// between loading a suite's mutex and TryLock: the window I-4's
	// re-validation closes.
	lockLoadedHook func(key string)
```

- `checkOutcome` gains `// vanished: the suite row was gone when the check read it (I-4).` / `vanished bool`.
- In `check`, the block from `muVal, _ := c.locks.LoadOrStore(key, &sync.Mutex{})` through the `TryLock` busy return becomes:

```go
	muVal, _ := c.locks.LoadOrStore(key, &sync.Mutex{})
	mu := muVal.(*sync.Mutex)
	if c.lockLoadedHook != nil {
		c.lockLoadedHook(key)
	}
	if !mu.TryLock() {
		// Another goroutine is on it; SPEC §7.3 says skip.
		return checkReport{result: resultBusy}
	}
	// I-4: the mutex counts only while the lock map still holds it. Cap
	// eviction (and phase F's reaper) drops the entry of a suite it deleted
	// while holding this mutex; a check that loaded it before then must not
	// run beside one that loads the new entry.
	if cur, ok := c.locks.Load(key); !ok || cur != muVal {
		mu.Unlock()
		return checkReport{result: resultSkipped}
	}
```

  and right after the `if !ok { … return checkReport{result: "failed"} }` that follows `checkLockedSafe`:

```go
	if out.vanished {
		// I-4: keep the lock map to suites with rows.
		c.locks.CompareAndDelete(key, muVal)
		mu.Unlock()
		return out.report
	}
```

- In `checkLocked`, the `errors.Is(err, cache.ErrNotFound)` case sets `out.vanished = true` before its `return`, with the comment `// I-4: the row vanished after check's pre-read (cap eviction, later` / `// the reaper). Nothing to check and nothing to write; check drops` / `// the lock-map entry.`
- In `Reconcile`'s AIDEV-NOTE, replace the last sentence (`Compare Check, which does LoadOrStore unconditionally` … `allowlist and the suite path via a cache hit.`) with `Check does the same since phase D (I-4, spec C3), and` / `both re-validate after TryLock: the map must still hold the mutex and the` / `row must still name the snapshot read before.` In its body, name the key (`key := suiteKey(scheme, host, suitePath)`), call `c.lockLoadedHook` between `LoadOrStore` and `TryLock` as in `check`, and directly after the `TryLock` failure return add:

```go
	// I-4 (spec C3): re-validate under the mutex — the lock map must still
	// hold it and the suite must still have its row, with the snapshot read
	// above still current. Otherwise the suite was deleted (cap eviction,
	// phase F's reaper) or re-adopted meanwhile.
	if cur, ok := c.locks.Load(key); !ok || cur != muVal {
		mu.Unlock()
		return false
	}
	if sf, err := c.cache.GetSuiteFreshness(ctx, scheme, host, suitePath); err != nil ||
		sf.CurrentSnapshotID == nil || *sf.CurrentSnapshotID != snapID {
		mu.Unlock()
		return false
	}
```

In `internal/freshness/scheduler.go`, `tick`'s enqueue loop becomes:

```go
	for _, s := range due {
		// Spec C4: at most freshness.max_suites suites queued, waiting or
		// in flight; the rest are queued by a later tick.
		if c.maxSuites > 0 && q.len() >= c.maxSuites {
			break
		}
		q.enqueue(s, c.pendingChange(s))
	}
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/freshness/ ./internal/cache/ ./internal/handler/`
Expected: PASS.

- [ ] **Step 5: Mutation check (do not commit)**

One at a time, reverting each: (a) drop `check`'s post-`TryLock` re-validation → `TestCheck_RevalidatesItsLockAfterTryLock` FAILS (`check on a stale mutex = "changed" with 1 upstream calls`); (b) drop the `vanished` `CompareAndDelete` → `TestCheck_VanishedRowDropsItsLockEntry` FAILS (`1 lock entries`); (c) drop `Reconcile`'s lock re-validation → `TestReconcile_RevalidatesUnderTheMutex` FAILS (`reconciled on a stale mutex`); (d) drop its snapshot comparison (`|| false` for `*sf.CurrentSnapshotID != snapID`) → the same test FAILS (`no longer current`); (e) `HoldSuite`'s `release(true)` skips `Purge` → `TestHoldSuite_TakesOnlyAFreeSuiteAndPurgesOnDelete` and `TestCreateSuite_EvictionUnderTheChecker` FAIL; (f) it skips `CompareAndDelete` → those two and the re-validation tests FAIL; (g) drop the queue bound → `TestTick_QueuesAtMostMaxSuites` FAILS (`queued after one tick = 30`).

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/freshness/suite_lock.go internal/freshness/suite_lock_test.go internal/freshness/freshness.go internal/freshness/scheduler.go
golangci-lint run ./internal/freshness/...
git add internal/freshness/suite_lock.go internal/freshness/suite_lock_test.go internal/freshness/freshness.go internal/freshness/scheduler.go
git commit -m "Re-validate suite locks after TryLock; let cap eviction hold a suite's mutex; bound the scheduler queues"
```

---

### Task 8: `freshness.max_suites` and its wiring

Spec C11's knob (`freshness.max_suites`: default `5000`, at least `10`; the per-host cap is `max_suites / 10`), following `adoption.max_duration`'s convention (phase C): the default is applied in `Load` only when the key is absent, so an explicit value below 10 — `0` included — fails `Validate` instead of being rescued. The checker gets it here (Task 7's queue bound); the handler gets it, with the checker as its eviction guard, in Task 9. The packaged config documents it (`TestDefaultConfigFileDocumentsEveryKey` and `TestDefaultConfigFilePreservesProfile` check that). The startup line reports it; the new startup-line test also pins `adoption_flip_quiet_period` (deferred minor C-35, `main.go`, a file this task touches).

**Files:**
- Modify: `internal/config/config.go`, `internal/config/config_test.go`, `packaging/config/config.toml.default`, `cmd/apt-cacher-ultra/main.go` (new `checkerConfig`; the startup line)
- Create: `cmd/apt-cacher-ultra/suite_caps_wiring_test.go`

**Interfaces:**
- Consumes: Task 7's `freshness.Config.MaxSuites`; test helpers `writeTOML` (`config_test.go`), `minimalCfg` (`main_test.go`), `silentBuildLogger` (`adoption_wiring_test.go`), `captureBuilder` (`cert_cache_evicted_test.go`).
- Produces: `config.FreshnessConfig.MaxSuites int` (`toml:"max_suites"`); `func checkerConfig(lifetimeCtx context.Context, cfg *config.Config, c *cache.Cache, fetchClient *fetch.Client, hostLimiter *hostsem.Sem, logger *slog.Logger, adopter *freshness.Adopter, ring *observability.Ring) freshness.Config` (`main.go`); the test file Task 9 extends.

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/config_test.go`:

```go
// freshness.max_suites (spec C11): default 5000, at least 10; an explicit
// value below 10 — 0 included — is refused, not rescued to the default.
func TestFreshnessMaxSuites_DefaultAndBounds(t *testing.T) {
	load := func(extra string) (*Config, error) {
		dir := t.TempDir()
		return Load(writeTOML(t, dir, "config.toml", "[cache]\ndir = \""+dir+"\"\n"+extra))
	}
	cfg, err := load("")
	if err != nil || cfg.Freshness.MaxSuites != 5000 {
		t.Fatalf("default = %d (err %v), want 5000", cfg.Freshness.MaxSuites, err)
	}
	for _, good := range []int{10, 20000} {
		if cfg, err = load(fmt.Sprintf("[freshness]\nmax_suites = %d\n", good)); err != nil || cfg.Freshness.MaxSuites != good {
			t.Fatalf("explicit %d = %d (err %v)", good, cfg.Freshness.MaxSuites, err)
		}
	}
	for _, bad := range []int{9, 0, -1} {
		_, err := load(fmt.Sprintf("[freshness]\nmax_suites = %d\n", bad))
		if err == nil || !strings.Contains(err.Error(), "freshness.max_suites must be at least 10") {
			t.Errorf("max_suites = %d: err = %v, want the max_suites bound", bad, err)
		}
	}
}
```

Create `cmd/apt-cacher-ultra/suite_caps_wiring_test.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/config"
)

// Spec C11: freshness.max_suites reaches the checker (the scheduler's
// queue bound, spec C4).
func TestCheckerConfig_CarriesMaxSuites(t *testing.T) {
	cfg := minimalCfg(t.TempDir(), nil)
	cfg.Freshness.MaxSuites = 1234
	fc := checkerConfig(context.Background(), cfg, nil, nil, nil, silentBuildLogger(), nil, nil)
	if fc.MaxSuites != 1234 {
		t.Fatalf("MaxSuites = %d, want freshness.max_suites (1234)", fc.MaxSuites)
	}
	if fc.Cooldown != cfg.Freshness.Cooldown.Duration || fc.Refresh != cfg.Freshness.PeriodicRefresh.Duration {
		t.Fatalf("cooldown/refresh = %v/%v, want the config's", fc.Cooldown, fc.Refresh)
	}
}

// The startup line reports freshness.max_suites, and (deferred minor C-35)
// adoption.flip_quiet_period, with their configured values.
func TestServeListeners_StartupLineReportsTheSuiteCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	text := "[cache]\ndir = \"" + t.TempDir() + "\"\n[freshness]\nmax_suites = 1234\n[adoption]\nflip_quiet_period = \"5s\"\n" +
		"[tls_mitm]\nenabled = false\n[admin]\nenabled = false\n"
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var logs captureBuilder
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveListeners(ctx, cfg, slog.New(slog.NewJSONHandler(&logs, nil)), ln, nil, nil, nil) }()
	// Wait until the daemon serves (the startup line precedes opening the
	// cache), then shut it down.
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Get("http://" + ln.Addr().String() + "/healthz-probe")
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("daemon not serving within 15s: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serveListeners: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		if rec["msg"] != "apt-cacher-ultra starting" {
			continue
		}
		if rec["freshness_max_suites"] != float64(1234) {
			t.Errorf("freshness_max_suites = %v, want 1234", rec["freshness_max_suites"])
		}
		if rec["adoption_flip_quiet_period"] != float64(5*time.Second) {
			t.Errorf("adoption_flip_quiet_period = %v, want 5s in nanoseconds", rec["adoption_flip_quiet_period"])
		}
		return
	}
	t.Fatal("startup line not found")
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go vet ./internal/config/ ./cmd/apt-cacher-ultra/`
Expected: both test packages fail to compile on `cfg.Freshness.MaxSuites undefined` (and `checkerConfig` is undefined).

- [ ] **Step 3: Implement**

In `internal/config/config.go`, `FreshnessConfig` gains, after `MaxConcurrentAdoptions`:

```go
	// MaxSuites is freshness.max_suites (spec C3, C4, C11): at most this
	// many suite rows (at the cap, cap eviction makes room for a new suite
	// when an idle one without a snapshot exists), at most a tenth of it
	// per canonical host, and at most this many suites in the scheduler's
	// queues. Default 5000; at least 10.
	MaxSuites int `toml:"max_suites"`
```

In `Load`, after the `max_concurrent_adoptions` presence default:

```go
	// freshness.max_suites: applied only when absent, so an explicit value
	// below 10 (0 included) fails Validate instead of being rescued.
	if !md.IsDefined("freshness", "max_suites") {
		cfg.Freshness.MaxSuites = 5000
	}
```

In `Validate`, after the `max_concurrent_adoptions` check: `if c.Freshness.MaxSuites < 10 {` → `errors.New("freshness.max_suites must be at least 10")`.

In `packaging/config/config.toml.default`, `[freshness]`, after `max_concurrent_adoptions`:

```toml
max_suites               = 5000  # tracked suites; a tenth per host; at least 10
```

In `cmd/apt-cacher-ultra/main.go`: add `"freshness_max_suites", cfg.Freshness.MaxSuites,` to the startup line after `"freshness_max_concurrent_adoptions", …`; replace the inline `freshness.New(freshness.Config{…})` literal with `freshness.New(checkerConfig(freshCtx, cfg, c, fetchClient, hostLimiter, logger, adopter, adoptionRing))`, and add before `handlerConfig`:

```go
// checkerConfig is the freshness checker's configuration (spec C4, C11:
// freshness.max_suites bounds the scheduler's queues). Factored out, like
// handlerConfig, for the wiring tests.
```

whose body returns the literal it replaced (same fields and values) plus `MaxSuites: cfg.Freshness.MaxSuites,` after `Refresh`.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/config/ && go test -race ./cmd/apt-cacher-ultra/`
Expected: PASS.

- [ ] **Step 5: Mutation check (do not commit)**

One at a time, reverting each: (a) drop the `Load` default → `TestFreshnessMaxSuites_DefaultAndBounds` FAILS; (b) `MaxSuites < 1` → the same test FAILS (`max_suites = 9: err = <nil>`); (c) drop `MaxSuites` from `checkerConfig` → `TestCheckerConfig_CarriesMaxSuites` FAILS; (d) drop the startup field → `TestServeListeners_StartupLineReportsTheSuiteCap` FAILS (`freshness_max_suites = <nil>`); (e) drop the packaged line → `TestDefaultConfigFileDocumentsEveryKey` FAILS.

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/config/ cmd/apt-cacher-ultra/main.go cmd/apt-cacher-ultra/suite_caps_wiring_test.go
golangci-lint run ./internal/config/... ./cmd/...
git add internal/config/config.go internal/config/config_test.go packaging/config/config.toml.default cmd/apt-cacher-ultra/main.go cmd/apt-cacher-ultra/suite_caps_wiring_test.go
git commit -m "Add freshness.max_suites and give it to the checker"
```

Expected: `gofmt -l` prints nothing; lint `0 issues`.

---

### Task 9: The handler's miss path creates rows and markers before the body

Spec C3, handler side, for misses: a suite file's bytes must pass their sniff before they count; the url_path row, the suite row (`<suite>/InRelease` always; `<suite>/Release` and flat indexes with an adopter wired) and the marker of a sniff-passing `InRelease`/`Release.gpg` are stored by one writer operation (`StoreFetched`, Task 6) before the body is written; a failed operation answers 502 with no body (today's url_path-write failure path, unchanged); `Release.gpg` creates no row; the caps with the checker as the eviction guard; the 10-minute rejection cache; WARN `suite_seed_rejected` (rate-limited), its counter and gauge (R-D5); the adoption-disabled seed byte for byte with no T1 (R-D4: `TestServeHTTP_AdoptionDisabledSeedParity`); the overview's phase C carry-forward "the serve that creates a suite's row registers with the flip barrier" (a test that fails without it). Design notes 1 (seed values), 2 (no T1 on the miss path), 4 and deviation 2 (a creation marks the suite's other cached signed anchors), 5 (a refresh miss no longer re-stamps), 9 (a late registration that waited serves what is current), 14 (`suite_evicted`). From here the handler sniffs `InRelease`, so the five handler fixtures that relied on a plain-text `InRelease` creating a row serve `simtest.ClearsignedFixture` (Step 1; they pass before and after the production change).

**Files:**
- Create: `internal/handler/suite_rows.go`, `internal/handler/suite_rows_test.go`
- Modify: `internal/handler/handler.go` (`Config.MaxSuites`, `Config.SuiteGuard`; handler fields; `New`; `ServeHTTP`; `tryCacheHit`, `trySnapshotHit`; `serveCacheMiss`; `runFetch`), `internal/handler/metrics.go`, `internal/handler/singleflight.go`, `cmd/apt-cacher-ultra/main.go` (`handlerConfig`), `cmd/apt-cacher-ultra/suite_caps_wiring_test.go`
- Modify tests (fixtures): `internal/handler/handler_test.go`, `phase2_handler_test.go`, `phase2_chaos_test.go`, `phase3_chaos_test.go`, `chaos_test.go`

**Interfaces:**
- Consumes: Task 1's `aptpath.ClassifySuiteFile`, `SuiteFile` (`CreatesRow`, `Marks`, `HeadBytes`, `Sniff`), `aptpath.MarkerBase`; Task 4's `cache.SuiteKey`, `SuiteSeed`, `SuiteCaps`, `CreateResult`, `SuiteGuard`; Task 5's eviction (through the guard); Task 6's `FetchedRow`, `StoreFetched`, `MarkSignedSeenBatch`; Task 7's `(*freshness.Checker).HoldSuite`; Task 8's `checkerConfig`; test helpers `syncLog`, `logRecords` (`flip_hook_test.go`), `sumMetricLines` (`disabled_mode_parity_test.go`), `newRecordingFreshness`, `silentLogger`, `proxyReq` (`handler_test.go`), `writeBlob` (`phase2_handler_test.go`), `newFlipStack` (`flip_stack_test.go`), `newAdoptionWiringDeps`, `minimalCfg`, `silentBuildLogger` (cmd).
- Produces (Task 10 builds on them):
  - `handler.Config.MaxSuites int`, `handler.Config.SuiteGuard cache.SuiteGuard`; handler fields `maxSuites`, `suiteGuard`, `rejections seedRejections`, `seedWarn seedWarn`.
  - In `suite_rows.go`: `type suiteReq struct { key cache.SuiteKey; file aptpath.SuiteFile; tracked bool; tok *flipgate.Token }`, `newSuiteReq(req *proxy.Request) *suiteReq`, `suiteKeyOf(req *proxy.Request) cache.SuiteKey`, `seedRejections` (`rejected`, `add`, `len`), `seedWarn`, `(h *Handler) suiteCaps() cache.SuiteCaps`, `(h *Handler) suiteSeed(sr *suiteReq, miss bool, etag, lastmod string, now int64) *cache.SuiteSeed`, `(h *Handler) noteCreate(key cache.SuiteKey, res cache.CreateResult)`, `readHead(path string, n int) ([]byte, error)`, `(h *Handler) markCachedSignedAnchors(ctx context.Context, key cache.SuiteKey, except string)`, `(h *Handler) registerLate(r *http.Request, req *proxy.Request, sr *suiteReq) bool`; constants `seedRejectTTL = 10 * time.Minute`, `seedRejectMax = 4096`, `seedRejectSweep = time.Minute`, `seedWarnEvery = time.Minute`.
  - `tryCacheHit(…, sr *suiteReq)`, `trySnapshotHit(…, sr *suiteReq)`, `serveCacheMiss(…, sr *suiteReq)`, `runFetch(ctx, req, file aptpath.SuiteFile, trackedAtStart bool)`; `sfResult.suiteTracked bool`.
  - Metrics `acu_suite_seed_rejected_total{host}` (`suiteSeedRejectedTotal`) and `acu_suites_seed_rejected` (`seedRejectedSuites`).
  - Test helpers (`suite_rows_test.go`): `suiteRig` / `newSuiteRig(t, adoption bool, maxSuites int)` with `serve`, `get`, `suite`, `marked`, `watch`, `secondConn`; `upFile`; `staticUpstream`; fixtures `rowInRelease`, `rowRelease`, `rowGPG`, `rowHTML`; `derefI64`, `derefStr`.

- [ ] **Step 1: Write the failing tests, and migrate the fixtures**

Create `internal/handler/suite_rows_test.go`:

```go
package handler

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// Spec C3 fixtures: bodies that pass, and one that fails, the sniffs.
var (
	rowInRelease = simtest.ClearsignedFixture("Origin: T\nSuite: s\n")
	rowRelease   = []byte("Origin: T\nSHA256:\n " + strings.Repeat("a", 64) + " 1 main/binary-amd64/Packages\n")
	rowGPG       = []byte("-----BEGIN PGP SIGNATURE-----\n\nwsBcBAABCAAQ\n=AAAA\n-----END PGP SIGNATURE-----\n")
	rowHTML      = []byte("<html><body>Not Found</body></html>\n")
)

// upFile is one file the rig's upstream serves.
type upFile struct {
	body          []byte
	etag, lastMod string
}

// suiteRig is a handler over a real cache and an httptest upstream whose
// files the test sets (spec C3 rows and markers).
type suiteRig struct {
	t     *testing.T
	h     *Handler
	c     *cache.Cache
	dir   string
	srv   *httptest.Server
	logs  *syncLog
	fresh *recordingFreshness

	mu    sync.Mutex
	files map[string]upFile
}

func newSuiteRig(t *testing.T, adoption bool, maxSuites int) *suiteRig {
	t.Helper()
	r := &suiteRig{t: t, dir: t.TempDir(), logs: &syncLog{}, fresh: newRecordingFreshness(), files: map[string]upFile{}}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		f, ok := r.files[req.URL.Path]
		r.mu.Unlock()
		if !ok {
			http.NotFound(w, req)
			return
		}
		if f.etag != "" {
			w.Header().Set("ETag", f.etag)
		}
		if f.lastMod != "" {
			w.Header().Set("Last-Modified", f.lastMod)
		}
		_, _ = w.Write(f.body)
	}))
	t.Cleanup(r.srv.Close)
	c, err := cache.Open(context.Background(), r.dir, silentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	parser, err := proxy.New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	fc, err := fetch.New(fetch.Options{ConnectTimeout: 2 * time.Second, TotalTimeout: 5 * time.Second, MaxRetries: 2,
		AllowedHostRegex: []string{`^127\.0\.0\.1$`}, Logger: silentLogger()})
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{Parser: parser, Cache: c, Fetch: fc, HostLimiter: hostsem.New(4),
		Logger: slog.New(slog.NewJSONHandler(r.logs, nil)), Freshness: r.fresh,
		AdoptionEnabled: adoption, MaxSuites: maxSuites})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close) // registered after the cache's cleanup, so it runs first
	r.h, r.c = h, c
	return r
}

func (r *suiteRig) serve(path string, f upFile) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.files[path] = f
}

func (r *suiteRig) get(path string) *httptest.ResponseRecorder {
	r.t.Helper()
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, proxyReq("GET", r.srv.URL, path))
	return rec
}

// suite returns the suite's row, or nil when it has none.
func (r *suiteRig) suite(suitePath string) *cache.SuiteFreshness {
	r.t.Helper()
	s, err := r.c.GetSuiteFreshness(context.Background(), "http", "127.0.0.1", suitePath)
	if errors.Is(err, cache.ErrNotFound) {
		return nil
	}
	if err != nil {
		r.t.Fatal(err)
	}
	return s
}

func (r *suiteRig) marked(base string) bool {
	r.t.Helper()
	_, ok, err := r.c.SignedSeen(context.Background(), "http", "127.0.0.1", base)
	if err != nil {
		r.t.Fatal(err)
	}
	return ok
}

// R-D4: with no adopter, the first-contact miss of <suite>/InRelease
// creates the row today's handler creates — every column — and no T1
// fires; no other path creates a row, and an InRelease that fails the
// sniff creates none.
func TestServeHTTP_AdoptionDisabledSeedParity(t *testing.T) {
	r := newSuiteRig(t, false, 0)
	const lm = "Mon, 01 Jan 2024 00:00:00 GMT"
	r.serve("/r/dists/s/InRelease", upFile{body: rowInRelease, etag: `"e1"`, lastMod: lm})
	before := wallclock.Unix()
	if rec := r.get("/r/dists/s/InRelease"); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), rowInRelease) {
		t.Fatalf("miss = %d %q", rec.Code, rec.Body.String())
	}
	after := wallclock.Unix()
	s := r.suite("/r/dists/s")
	if s == nil || s.LastRequestedAt == nil {
		t.Fatalf("row = %+v, want the seed", s)
	}
	stamp := *s.LastRequestedAt
	if stamp < before || stamp > after || derefI64(s.LastCheckAt) != stamp || derefI64(s.LastSuccessAt) != stamp {
		t.Fatalf("stamps check=%v success=%v requested=%d, want all the miss's second in [%d, %d]",
			s.LastCheckAt, s.LastSuccessAt, stamp, before, after)
	}
	if derefStr(s.InReleaseETag) != `"e1"` || derefStr(s.InReleaseLastMod) != lm {
		t.Fatalf("validators = %v %v, want the miss's", s.InReleaseETag, s.InReleaseLastMod)
	}
	if s.InReleaseChangeSeenAt != nil || s.CurrentSnapshotID != nil || s.CheckFailCount != 0 {
		t.Fatalf("row = %+v, want no change-seen stamp, no snapshot, no failed checks", s)
	}
	if s.AdoptionFailHash != nil || s.AdoptionFailReason != nil || s.AdoptionFailCount != 0 || s.AdoptionFailFirstAt != nil ||
		s.AdoptionFailLastAt != nil || s.AdoptionNextRetryAt != nil || s.AdoptionFailETag != nil || s.AdoptionFailLastMod != nil {
		t.Fatalf("row = %+v, want an empty failure record", s)
	}
	for path, f := range map[string]upFile{
		"/r/dists/t/Release":       {body: rowRelease},
		"/r/dists/u/Release.gpg":   {body: rowGPG},
		"/r/dists/v/InRelease":     {body: rowHTML}, // fails the sniff
		"/r/dists/w/main/Packages": {body: []byte("Package: p\n")},
	} {
		r.serve(path, f)
		if rec := r.get(path); rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", path, rec.Code)
		}
	}
	for _, suite := range []string{"/r/dists/t", "/r/dists/u", "/r/dists/v", "/r/dists/w"} {
		if r.suite(suite) != nil {
			t.Errorf("%s: a row was created", suite)
		}
	}
	r.h.Close() // drains any freshness goroutine
	if calls := r.fresh.snapshot(); len(calls) != 0 {
		t.Fatalf("misses fired T1: %+v", calls)
	}
}

// Design note 1, spec C3: with an adopter, a sniff-passing InRelease or
// Release miss creates a row whose check stamps and validators are NULL
// (its first check runs at once); Release.gpg never creates one.
func TestServeHTTP_AdopterWiredMissesCreateUnstampedRows(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	r.serve("/r/dists/a/InRelease", upFile{body: rowInRelease, etag: `"e1"`})
	r.serve("/r/dists/b/Release", upFile{body: rowRelease, etag: `"e2"`})
	r.serve("/r/dists/c/Release", upFile{body: rowHTML})
	r.serve("/r/dists/d/Release.gpg", upFile{body: rowGPG})
	for _, p := range []string{"/r/dists/a/InRelease", "/r/dists/b/Release", "/r/dists/c/Release", "/r/dists/d/Release.gpg"} {
		if rec := r.get(p); rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", p, rec.Code)
		}
	}
	for _, suite := range []string{"/r/dists/a", "/r/dists/b"} {
		s := r.suite(suite)
		if s == nil || s.LastCheckAt != nil || s.LastSuccessAt != nil || s.InReleaseETag != nil || s.LastRequestedAt == nil {
			t.Fatalf("%s: row = %+v, want created with NULL check stamps and validators", suite, s)
		}
	}
	for _, suite := range []string{"/r/dists/c", "/r/dists/d"} {
		if r.suite(suite) != nil {
			t.Errorf("%s: a row was created (HTML Release, or Release.gpg)", suite)
		}
	}
}

// markWatcher records, at the first body byte written to the client,
// whether the suite's row and its base's marker existed.
type markWatcher struct {
	*httptest.ResponseRecorder
	r          *suiteRig
	suite      string
	checked    bool
	row, mark  bool
	checkedErr error
}

func (w *markWatcher) Write(p []byte) (int, error) {
	if !w.checked {
		w.checked = true
		w.row = w.r.suite(w.suite) != nil
		_, w.mark, w.checkedErr = w.r.c.SignedSeen(context.Background(), "http", "127.0.0.1", aptpath.MarkerBase(w.suite))
	}
	return w.ResponseRecorder.Write(p)
}

func (r *suiteRig) watch(path, suite string) *markWatcher {
	r.t.Helper()
	w := &markWatcher{ResponseRecorder: httptest.NewRecorder(), r: r, suite: suite}
	r.h.ServeHTTP(w, proxyReq("GET", r.srv.URL, path))
	if w.Code != http.StatusOK || !w.checked || w.checkedErr != nil {
		r.t.Fatalf("%s: status %d, body written %v, read error %v", path, w.Code, w.checked, w.checkedErr)
	}
	return w
}

// Spec C3 miss-path ordering: the signed-seen marker (and, on first
// contact, the suite row) exists before the first body byte reaches the
// client — on first contact, on a refresh miss, and for a tracked suite's
// Release.gpg.
func TestServeHTTP_MarkerPrecedesTheFirstBodyByte(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	r.serve("/r/dists/s/InRelease", upFile{body: rowInRelease})
	if w := r.watch("/r/dists/s/InRelease", "/r/dists/s"); !w.row || !w.mark {
		t.Fatalf("first contact: at the first body byte row=%v marker=%v, want both", w.row, w.mark)
	}
	// A refresh miss: the row exists, its url_path row and marker are gone.
	db := r.secondConn()
	if _, err := db.Exec(`DELETE FROM url_path WHERE path = '/r/dists/s/InRelease'; DELETE FROM suite_signed_seen`); err != nil {
		t.Fatal(err)
	}
	if w := r.watch("/r/dists/s/InRelease", "/r/dists/s"); !w.mark {
		t.Fatal("refresh miss: no marker at the first body byte")
	}
	r.serve("/r/dists/s/Release.gpg", upFile{body: rowGPG})
	if _, err := db.Exec(`DELETE FROM suite_signed_seen`); err != nil {
		t.Fatal(err)
	}
	if w := r.watch("/r/dists/s/Release.gpg", "/r/dists/s"); !w.mark {
		t.Fatal("Release.gpg of a tracked suite: no marker at the first body byte")
	}
}

// secondConn opens cache.db on a second connection (the test injects
// failures and rewinds state the cache's API does not expose).
func (r *suiteRig) secondConn() *sql.DB {
	r.t.Helper()
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(r.dir, "cache.db"), RawQuery: q.Encode()}).String())
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = db.Close() })
	return db
}

// Spec C3: a failed writer operation serves no body — 502, as a url_path
// write failure always has — and stores nothing.
func TestServeHTTP_FailedMissWriteServesNoBody(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	r.serve("/r/dists/s/InRelease", upFile{body: rowInRelease})
	if _, err := r.secondConn().Exec(`CREATE TRIGGER fail_mark BEFORE INSERT ON suite_signed_seen
BEGIN SELECT RAISE(ABORT, 'injected marker failure'); END`); err != nil {
		t.Fatal(err)
	}
	rec := r.get("/r/dists/s/InRelease")
	if rec.Code != http.StatusBadGateway || bytes.Contains(rec.Body.Bytes(), []byte("Origin: T")) {
		t.Fatalf("status %d body %q; want 502 without the anchor", rec.Code, rec.Body.String())
	}
	if r.suite("/r/dists/s") != nil {
		t.Fatal("the failed operation created the suite row")
	}
	if _, err := r.c.LookupURL(context.Background(), "http", "127.0.0.1", "/r/dists/s/InRelease"); !errors.Is(err, cache.ErrNotFound) {
		t.Fatalf("the failed operation stored the url_path row (%v)", err)
	}
}

// Spec C3: markers only for suites with a row — an untracked suite's
// Release.gpg, and a suite refused at a cap, get none (the url_path rows
// are stored).
func TestServeHTTP_MarkersOnlyForTrackedSuites(t *testing.T) {
	r := newSuiteRig(t, true, 10) // per-host cap 1
	r.serve("/r/dists/a/InRelease", upFile{body: rowInRelease})
	r.serve("/r/dists/b/InRelease", upFile{body: rowInRelease})
	r.serve("/r/dists/c/Release.gpg", upFile{body: rowGPG})
	for _, p := range []string{"/r/dists/a/InRelease", "/r/dists/b/InRelease", "/r/dists/c/Release.gpg"} {
		if rec := r.get(p); rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", p, rec.Code)
		}
	}
	if !r.marked("/r/dists/a") || r.marked("/r/dists/b") || r.marked("/r/dists/c") {
		t.Fatalf("markers a=%v b=%v c=%v; want only the tracked suite a", r.marked("/r/dists/a"), r.marked("/r/dists/b"), r.marked("/r/dists/c"))
	}
	if r.suite("/r/dists/b") != nil || r.suite("/r/dists/c") != nil {
		t.Fatal("a row beyond the per-host cap, or for a Release.gpg")
	}
}

// Spec C3 / R-D5: a refusal at a cap is cached for 10 minutes (the next
// request costs no writer operation), counted once
// (acu_suite_seed_rejected_total), shown by the gauge
// (acu_suites_seed_rejected) and logged WARN suite_seed_rejected.
func TestServeHTTP_SuiteCapRejectionIsCachedCountedAndLogged(t *testing.T) {
	r := newSuiteRig(t, true, 10) // per-host cap 1
	r.serve("/r/dists/a/InRelease", upFile{body: rowInRelease})
	r.serve("/r/dists/b/InRelease", upFile{body: rowInRelease})
	r.get("/r/dists/a/InRelease")
	before := sumMetricLines(t, []string{"acu_suite_seed_rejected_total"})["acu_suite_seed_rejected_total"]
	r.get("/r/dists/b/InRelease")
	db := r.secondConn()
	if _, err := db.Exec(`DELETE FROM url_path WHERE path = '/r/dists/b/InRelease'`); err != nil {
		t.Fatal(err)
	}
	r.get("/r/dists/b/InRelease") // a second miss within the window: the cache answers
	got := sumMetricLines(t, []string{"acu_suite_seed_rejected_total", "acu_suites_seed_rejected"})
	if d := got["acu_suite_seed_rejected_total"] - before; d != 1 {
		t.Fatalf("acu_suite_seed_rejected_total rose by %v, want 1 (the repeat is cached)", d)
	}
	if got["acu_suites_seed_rejected"] != 1 || r.h.rejections.len() != 1 {
		t.Fatalf("gauge %v, cache %d; want 1 and 1", got["acu_suites_seed_rejected"], r.h.rejections.len())
	}
	recs := logRecords(r.logs, "suite_seed_rejected", slog.LevelWarn)
	if len(recs) != 1 || recs[0]["suite_path"] != "/r/dists/b" || recs[0]["cap"] != "host_cap" ||
		recs[0]["max_suites"] != float64(10) || recs[0]["detail"] != "suite will not refresh" {
		t.Fatalf("suite_seed_rejected records = %+v", recs)
	}
}

// The rejection cache holds a refusal for exactly 10 minutes (monotonic
// time) and stays bounded: full of live entries, it clears itself.
func TestSeedRejections_TTLAndBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var s seedRejections
		k := cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/s"}
		s.add(k)
		time.Sleep(seedRejectTTL - time.Second)
		if !s.rejected(k) {
			t.Fatal("expired before 10 minutes")
		}
		time.Sleep(time.Second)
		if s.rejected(k) || s.len() != 0 {
			t.Fatalf("still cached after 10 minutes (%d entries)", s.len())
		}
		for i := range seedRejectMax {
			s.add(cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/" + strings.Repeat("x", i+1)})
		}
		s.add(k)
		if n := s.len(); n != 1 {
			t.Fatalf("after overflowing with live entries: %d entries, want 1 (cleared, then the new one)", n)
		}
	})
}

// R-D5: suite_seed_rejected is rate-limited to one line per minute
// process-wide; the next line carries how many were suppressed.
func TestNoteCreate_WarnIsRateLimited(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs := &syncLog{}
		h := &Handler{logger: slog.New(slog.NewJSONHandler(logs, nil)), maxSuites: 10}
		refuse := func(suite string) {
			h.noteCreate(cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: suite},
				cache.CreateResult{Outcome: cache.SuiteRejectedGlobal})
		}
		refuse("/a")
		refuse("/b")
		refuse("/c")
		time.Sleep(seedWarnEvery)
		refuse("/d")
		recs := logRecords(logs, "suite_seed_rejected", slog.LevelWarn)
		if len(recs) != 2 || recs[0]["suppressed"] != float64(0) || recs[1]["suppressed"] != float64(2) ||
			recs[1]["suite_path"] != "/d" || recs[1]["cap"] != "global_cap" {
			t.Fatalf("records = %+v; want /a (suppressed 0) and /d (suppressed 2)", recs)
		}
	})
}

// staticUpstream serves fixed bodies by URL to the handler (a flip-stack
// handler's own upstream answers 404).
type staticUpstream map[string][]byte

func (u staticUpstream) HostAllowed(string) bool { return true }

func (u staticUpstream) Fetch(_ context.Context, target *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error) {
	body, ok := u[target.URL]
	if !ok {
		return nil, &fetch.StatusError{Code: 404}
	}
	if _, err := dst.Write(body); err != nil {
		return nil, err
	}
	return &fetch.FetchResult{Status: 200, ContentLength: int64(len(body))}, nil
}

// Phase C carry-forward: the serve that creates a suite's row registers
// with the flip barrier right after the creation, like any tracked
// anchor serve — a flip right after it waits out the quiet period.
func TestServeHTTP_RowCreatingServeRegistersWithTheBarrier(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newFlipStack(t, 10*time.Second)
		const suite = "/n/dists/s"
		s.h.fetch = staticUpstream{"http://" + flipSimHost + suite + "/InRelease": rowInRelease}
		if _, code := s.get(suite+"/InRelease", 0); code != http.StatusOK {
			t.Fatalf("first contact = %d", code)
		}
		if n := s.gate.Len(); n != 1 {
			t.Fatalf("gate entries after the row-creating serve = %d, want 1", n)
		}
		start := time.Now()
		if _, err := s.gate.Flip(context.Background(), flipgate.Key("http", flipSimHost, suite), func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		if got := time.Since(start); got != 10*time.Second {
			t.Fatalf("a flip right after the serve waited %v, want the 10s quiet period", got)
		}
	})
}

// Design note 9: a row-creating serve whose late registration waited in a
// flip's hold is answered from what is current after the flip — here the
// snapshot the flip committed — not with the bytes its miss fetched.
func TestServeHTTP_LateRegistrationThatWaitedServesTheNewSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newFlipStack(t, 10*time.Second)
		const suite = "/n/dists/s"
		s.h.fetch = staticUpstream{"http://" + flipSimHost + suite + "/InRelease": rowInRelease}
		snapBody := simtest.ClearsignedFixture("Origin: snapshot\n")
		hash := writeBlob(t, s.h, snapBody)
		ctx := context.Background()
		cand, _, err := s.c.InsertCandidateSnapshot(ctx, cache.SnapshotCandidate{CanonicalScheme: "http",
			CanonicalHost: flipSimHost, SuitePath: suite, InReleaseHash: &hash})
		if err != nil {
			t.Fatal(err)
		}
		flipped := make(chan struct{})
		go func() {
			defer close(flipped)
			// The suite is idle: the hold starts at once. The commit waits
			// for the row the client's miss creates.
			_, err := s.gate.Flip(ctx, flipgate.Key("http", flipSimHost, suite), func() error {
				for {
					if ok, err := s.c.SuiteExists(ctx, cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: flipSimHost, SuitePath: suite}); err != nil || ok {
						break
					}
					time.Sleep(time.Millisecond)
				}
				return s.c.CommitAdoption(ctx, cand, []cache.SnapshotMember{{SnapshotID: cand, Path: "InRelease",
					BlobHash: hash, DeclaredSHA256: hash}}, nil, nil, nil, false)
			})
			if err != nil {
				t.Error(err)
			}
		}()
		synctest.Wait() // the flip holds
		rec := httptest.NewRecorder()
		s.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+flipSimHost+suite+"/InRelease", nil))
		<-flipped
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), snapBody) {
			t.Fatalf("status %d body %q; want the committed snapshot's InRelease", rec.Code, rec.Body.String())
		}
	})
}

func derefI64(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

func derefStr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}
```

Append to `cmd/apt-cacher-ultra/suite_caps_wiring_test.go` (adding the `internal/freshness` import):

```go
// Spec C3: the handler gets freshness.max_suites and the checker as its
// cap-eviction guard; with no checker the guard is a nil interface, never
// a typed nil.
func TestHandlerConfig_CarriesTheSuiteCapsAndGuard(t *testing.T) {
	cfg := minimalCfg(t.TempDir(), nil)
	cfg.Freshness.MaxSuites = 1234
	c, fetcher, hosts := newAdoptionWiringDeps(t)
	ck, err := freshness.New(checkerConfig(context.Background(), cfg, c, fetcher, hosts, silentBuildLogger(), nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	hc := handlerConfig(cfg, nil, c, fetcher, hosts, silentBuildLogger(), ck, nil)
	if hc.MaxSuites != 1234 {
		t.Fatalf("MaxSuites = %d, want freshness.max_suites (1234)", hc.MaxSuites)
	}
	if g, ok := hc.SuiteGuard.(*freshness.Checker); !ok || g != ck {
		t.Fatalf("SuiteGuard = %T %v, want the checker", hc.SuiteGuard, hc.SuiteGuard)
	}
	if hc := handlerConfig(cfg, nil, c, fetcher, hosts, silentBuildLogger(), nil, nil); hc.SuiteGuard != nil {
		t.Fatalf("no checker, yet SuiteGuard = %T (a typed nil would be called)", hc.SuiteGuard)
	}
}
```

Fixtures (each test's `InRelease` now passes the sniff; add the `internal/simtest` import where missing):
- `handler_test.go`, `TestServeHTTP_MissOnInReleaseSeedsFreshness`: `body := simtest.ClearsignedFixture("Origin: Ubuntu\nSuite: noble\n")`; its doc says the cache "creates" (not "upserts") a row after a miss of a suite's "(clearsigned, spec C3 sniff) InRelease".
- `phase2_handler_test.go`, `TestServeHTTP_PreSnapshotSuiteUsesURLPath`: `body := simtest.ClearsignedFixture("Phase 1 InRelease bytes") // creates the row (spec C3 sniff)`.
- `phase2_chaos_test.go`, `makeChaos2Snapshot`: `inRelease: simtest.ClearsignedFixture(string(rel)), // passes the spec C3 sniff`; `chaos2BuildRelease`'s doc ends "Upstream serves it at /InRelease wrapped by simtest.ClearsignedFixture; the pass-through verifier returns that envelope verbatim and ParseRelease skips its armor lines."
- `phase3_chaos_test.go`, `makeChaos3Snapshot`: the same `inRelease:` change.
- `chaos_test.go`, `chaosBodyFor`: for `path == "/ubuntu/dists/noble/InRelease"` return `simtest.ClearsignedFixture("chaos-body|" + path)` (comment: clearsigned, so the priming miss creates the suite's row and later hits fire freshness checks).

- [ ] **Step 2: Run to verify failure**

Run: `go vet ./internal/handler/ ./cmd/apt-cacher-ultra/`
Expected: `unknown field MaxSuites in struct literal of type Config` (handler) and `hc.MaxSuites undefined` (cmd). The migrated fixture tests pass on the old handler too.

- [ ] **Step 3: Implement**

Create `internal/handler/suite_rows.go`:

```go
package handler

import (
	"context"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
)

// suiteReq is what one request knows about its suite (spec C3), threaded
// from ServeHTTP through the hit, miss and stale paths.
type suiteReq struct {
	key  cache.SuiteKey
	file aptpath.SuiteFile
	// tracked: the suite has a suite_freshness row, as this request last
	// read or wrote it.
	tracked bool
	// tok is this request's flip-barrier registration; ServeHTTP's
	// deferred Done covers a registration made after the suite's row was
	// created (phase C carry-forward).
	tok *flipgate.Token
}

func newSuiteReq(req *proxy.Request) *suiteReq {
	sr := &suiteReq{}
	if req.IsMetadata && req.SuitePath != "" {
		sr.key = suiteKeyOf(req)
		sr.file = aptpath.ClassifySuiteFile(req.SuitePath, req.Path)
	}
	return sr
}

// suiteKeyOf is the key of req's suite.
func suiteKeyOf(req *proxy.Request) cache.SuiteKey {
	return cache.SuiteKey{CanonicalScheme: req.CanonicalScheme, CanonicalHost: req.CanonicalHost, SuitePath: req.SuitePath}
}

// seedRejectTTL, seedRejectMax and seedRejectSweep shape spec C3's
// cap-rejection cache: a suite refused a row is not retried for 10
// minutes (spec C11 constant); the cache holds at most seedRejectMax
// suites and drops expired ones at most once a minute.
const (
	seedRejectTTL   = 10 * time.Minute
	seedRejectMax   = 4096
	seedRejectSweep = time.Minute
)

// seedRejections caches cap rejections per suite (spec C3), so an over-cap
// suite costs no writer operation per request. Monotonic time. At
// seedRejectMax entries, expired ones are dropped; if it is still full it
// is cleared (each suite then costs one more writer operation).
type seedRejections struct {
	mu        sync.Mutex
	until     map[cache.SuiteKey]time.Time
	lastSweep time.Time
}

// rejected reports whether key was refused a row within seedRejectTTL.
func (s *seedRejections) rejected(key cache.SuiteKey) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(false)
	t, ok := s.until[key]
	if ok && !time.Now().Before(t) {
		delete(s.until, key)
		ok = false
	}
	seedRejectedSuites.Set(float64(len(s.until)))
	return ok
}

// add caches a rejection of key.
func (s *seedRejections) add(key cache.SuiteKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.until == nil {
		s.until = make(map[cache.SuiteKey]time.Time)
	}
	if len(s.until) >= seedRejectMax {
		s.sweepLocked(true)
		if len(s.until) >= seedRejectMax {
			clear(s.until)
		}
	}
	s.until[key] = time.Now().Add(seedRejectTTL)
	seedRejectedSuites.Set(float64(len(s.until)))
}

// len is the number of suites cached as refused (tests).
func (s *seedRejections) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.until)
}

// sweepLocked drops expired entries, at most once per seedRejectSweep
// unless forced.
func (s *seedRejections) sweepLocked(force bool) {
	now := time.Now()
	if !force && now.Sub(s.lastSweep) < seedRejectSweep {
		return
	}
	s.lastSweep = now
	for k, t := range s.until {
		if !now.Before(t) {
			delete(s.until, k)
		}
	}
}

// seedWarnEvery rate-limits suite_seed_rejected: at most one line per
// minute process-wide; suppressed counts the rejections in between.
const seedWarnEvery = time.Minute

// seedWarn is suite_seed_rejected's rate limiter.
type seedWarn struct {
	mu         sync.Mutex
	last       time.Time
	suppressed int
}

// suiteCaps is CreateSuite's bounds: freshness.max_suites globally and a
// tenth of it per canonical host (spec C3); 0 is unbounded (tests).
func (h *Handler) suiteCaps() cache.SuiteCaps {
	return cache.SuiteCaps{Global: h.maxSuites, PerHost: h.maxSuites / 10}
}

// suiteSeed is the row a creation inserts (spec C3). Only an
// adoption-disabled miss of <suite>/InRelease stamps the miss as a
// successful check with its validators — today's seed, byte for byte
// (TestServeHTTP_AdoptionDisabledSeedParity); every other creation leaves
// the stamps and validators NULL, so the suite's first check runs at once
// and unconditionally.
func (h *Handler) suiteSeed(sr *suiteReq, miss bool, etag, lastmod string, now int64) *cache.SuiteSeed {
	s := &cache.SuiteSeed{SuiteKey: sr.key, RequestedAt: now}
	if miss && !h.adoptionEnabled && sr.file == aptpath.FileInRelease {
		s.LastCheckAt, s.LastSuccessAt = &now, &now
		if etag != "" {
			s.InReleaseETag = &etag
		}
		if lastmod != "" {
			s.InReleaseLastMod = &lastmod
		}
	}
	return s
}

// noteCreate handles a creation's outcome: a cap rejection is cached,
// counted and logged (rate-limited WARN suite_seed_rejected); an eviction
// is logged at INFO (suite_evicted).
func (h *Handler) noteCreate(key cache.SuiteKey, res cache.CreateResult) {
	if res.Evicted != nil {
		h.logger.Info("suite_evicted",
			"canonical_host", res.Evicted.CanonicalHost,
			"suite_path", res.Evicted.SuitePath,
			"for_host", key.CanonicalHost,
			"for_suite", key.SuitePath,
		)
	}
	if !res.Outcome.Rejected() {
		return
	}
	h.rejections.add(key)
	suiteSeedRejectedTotal.Inc(key.CanonicalHost)
	h.seedWarn.mu.Lock()
	now := time.Now()
	if !h.seedWarn.last.IsZero() && now.Sub(h.seedWarn.last) < seedWarnEvery {
		h.seedWarn.suppressed++
		h.seedWarn.mu.Unlock()
		return
	}
	suppressed := h.seedWarn.suppressed
	h.seedWarn.last, h.seedWarn.suppressed = now, 0
	h.seedWarn.mu.Unlock()
	h.logger.Warn("suite_seed_rejected",
		"canonical_host", key.CanonicalHost,
		"suite_path", key.SuitePath,
		"cap", res.Outcome.String(),
		"max_suites", h.maxSuites,
		"suppressed", suppressed,
		"detail", "suite will not refresh",
	)
}

// readHead reads at most n leading bytes of a pool file.
func readHead(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, int64(n)))
}

// markCachedSignedAnchors gives a suite this request just created the
// signed-seen marker its other cached signed anchors earn (spec C3): an
// InRelease or Release.gpg url_path row stored while the suite had no row
// (refused at a cap, or cached by an earlier binary) wrote no marker.
// SeenAt is that row's last use, as in the startup marker pass. A failure
// is logged; the startup pass heals it at the next start.
//
// AIDEV-NOTE: this keeps the hit-path invariant — every sniff-passing
// anchor row of a tracked suite has a marker for its base — across row
// creation, since markers are written only for suites with rows.
func (h *Handler) markCachedSignedAnchors(ctx context.Context, key cache.SuiteKey, except string) {
	var marks []cache.SignedSeenMark
	for _, name := range []string{"/InRelease", "/Release.gpg"} {
		p := key.SuitePath + name
		if p == except {
			continue
		}
		row, err := h.cache.LookupURL(ctx, key.CanonicalScheme, key.CanonicalHost, p)
		if err != nil || row.BlobHash == nil {
			continue
		}
		file := aptpath.ClassifySuiteFile(key.SuitePath, p)
		head, err := readHead(h.cache.BlobPath(*row.BlobHash), file.HeadBytes())
		if err != nil || !file.Sniff(p, head) {
			continue
		}
		var seen int64
		if row.LastRequestedAt != nil {
			seen = *row.LastRequestedAt
		}
		if row.LastFetchedAt != nil && *row.LastFetchedAt > seen {
			seen = *row.LastFetchedAt
		}
		if seen > 0 {
			marks = append(marks, cache.SignedSeenMark{CanonicalScheme: key.CanonicalScheme,
				CanonicalHost: key.CanonicalHost, Base: aptpath.MarkerBase(key.SuitePath), SeenAt: seen})
		}
	}
	if len(marks) == 0 {
		return
	}
	if err := h.cache.MarkSignedSeenBatch(ctx, marks); err != nil {
		h.logger.Warn("suite signed-seen mark failed",
			"err", err, "canonical_host", key.CanonicalHost, "suite_path", key.SuitePath)
	}
}

// registerLate registers, with the flip barrier, the anchor serve of a
// suite whose row this request created or found created after ServeHTTP's
// anchorStart (phase C carry-forward: the serve that creates a row is
// tracked like any other). It reports whether the registration waited in a
// hold.
func (h *Handler) registerLate(r *http.Request, req *proxy.Request, sr *suiteReq) bool {
	if sr.tok != nil || !sr.tracked {
		return false
	}
	t := h.anchorStart(r, req)
	if t == nil {
		return false
	}
	sr.tok = t
	return t.Held() > 0
}
```

In `internal/handler/metrics.go`: append to the inventory AIDEV-NOTE `; acu_suite_seed_rejected_total{host} (a suite refused` / `// a row by the spec C3 caps; counted once per refusal that reached the` / `// writer, i.e. at most once per suite per 10-minute rejection-cache` / `// window) and acu_suites_seed_rejected (unlabelled gauge: suites in that` / `// cache now).` (after "retained by-hash)"), and declare `suiteSeedRejectedTotal = metrics.NewCounterWithCap("acu_suite_seed_rejected_total", "Suites refused a suite row by freshness.max_suites (global or per-host cap): they will not refresh. By upstream host (spec C3).", metrics.DefaultMaxSeries, "host")` and `seedRejectedSuites = metrics.NewGaugeWithCap("acu_suites_seed_rejected", "Suites refused a suite row by freshness.max_suites within the last 10 minutes (spec C3 rejection cache).", 0)`, each with a short doc comment. In `internal/handler/singleflight.go`, `sfResult` gains `// suiteTracked: the request's suite has a row after the miss's writer` / `// operation (spec C3).` / `suiteTracked bool`.

In `internal/handler/handler.go`:

- Import `internal/aptpath`. `Config` gains, after `KeepDisplaced`: `MaxSuites int` (doc: "freshness.max_suites: the spec C3 suite-row caps (this many rows in all, a tenth of it per canonical host). 0 is unbounded (tests); production configs validate it to at least 10.") and `SuiteGuard cache.SuiteGuard` (doc: "holds a suite's in-memory mutex for cap eviction (spec C3); production passes the freshness checker. nil evicts without holding one (no checker, so no in-memory suite state)."). `Handler` gains, after `keepDisplaced`, `maxSuites int`, `suiteGuard cache.SuiteGuard`, `rejections seedRejections`, `seedWarn seedWarn` (comment: "Spec C3 suite rows: the caps, the eviction guard, the cap-rejection cache and suite_seed_rejected's rate limiter."). `New` refuses `cfg.MaxSuites < 0` (`errors.New("handler: negative MaxSuites")`) and copies both fields.
- `ServeHTTP`: replace the `anchorStart` block's opening (`if t := h.anchorStart(r, req); t != nil {` / `defer t.Done()`) with

```go
	// Spec C3: what this request knows about its suite's row, threaded
	// through the hit, miss and stale paths. Its barrier token's Done runs
	// once the response has been written, also for a token registered
	// after the request created the suite's row.
	sr := newSuiteReq(req)
	defer func() { sr.tok.Done() }()

	// Spec C8: register an anchor serve with the flip barrier before
	// anything reads the snapshot pointer (it may wait here while the
	// suite's flip holds anchors) and before host-slot acquisition and
	// singleflight.
	if t := h.anchorStart(r, req); t != nil {
		sr.tok = t
```

  (the rest of that block unchanged; the comment above it loses "; Done runs once the response has been written"), and pass `sr` to `tryCacheHit` and `serveCacheMiss`.
- `tryCacheHit` and `trySnapshotHit` take `sr *suiteReq` last; `tryCacheHit` passes it to `trySnapshotHit`, which sets `sr.tracked = false` in its `cache.ErrNotFound` case and `sr.tracked = true` once the suite row was read.
- `serveCacheMiss` takes `sr *suiteReq`, captures `file, trackedAtStart := sr.file, sr.tracked` before the singleflight call, calls `h.runFetch(h.lifecycleCtx, req, file, trackedAtStart)`, and right after the `res.err != nil` return adds:

```go
	if res.suiteTracked {
		sr.tracked = true
	}
	// Phase C carry-forward: the serve of an anchor whose suite row this
	// miss created (or found created by a concurrent request) registers
	// with the flip barrier right after the creation, before its body. A
	// registration that waited in a hold serves what is current now: the
	// flip may have committed a snapshot (design note 9).
	if h.registerLate(r, req, sr) {
		if served, outcome, status, body, v := h.tryCacheHit(w, r, req, start, sr); served {
			if outcome == "" {
				outcome = "hit"
			}
			h.logRequestWithValidation(r, req.CanonicalHost, req.Path, outcome, status, body, false, 0, start, v)
			return
		}
	}
```

- `runFetch` takes `file aptpath.SuiteFile, trackedAtStart bool`. Replace everything from `if err := h.cache.PutURLPath(dbCtx, row); err != nil {` through the end of the old `InRelease` seed block with:

```go
	// Spec C3: one writer operation stores the url_path row and, for a
	// suite file whose body passes its sniff, creates the suite's row
	// (InRelease always; Release and flat indexes with an adopter wired)
	// and writes the signed-seen marker of a signed anchor (InRelease,
	// Release.gpg) — before the body is served. A failed operation fails
	// the miss (502, no body) as a url_path write failure always has, so a
	// signed anchor body is never served before its base has a marker.
	// A suite row created here — the scheduler scans suite_freshness —
	// replaces the old InRelease seed (a refresh miss no longer re-stamps
	// an existing row: design note 5).
	//
	// AIDEV-NOTE: the sniff reads the finalized pool file. SniffReleaseGPG
	// accepts a leading 0xC2 byte (a new-format signature packet) that is
	// also a UTF-8 lead byte: such a body writes a marker, the safe
	// direction (it can only refuse unsigned adoption).
	sr := &suiteReq{key: suiteKeyOf(req), file: file}
	fetched := cache.FetchedRow{Row: row}
	if file != aptpath.NotSuiteFile {
		fetched.Suite = sr.key
		head, herr := readHead(h.cache.BlobPath(hash), file.HeadBytes())
		if herr == nil && file.Sniff(req.Path, head) {
			if file.Marks() {
				fetched.MarkSeenAt = now
			}
			if !trackedAtStart && file.CreatesRow(h.adoptionEnabled) && !h.rejections.rejected(sr.key) {
				fetched.Create = h.suiteSeed(sr, true, fres.ETag, fres.LastModified, now)
			}
		}
	}
	stored, err := h.cache.StoreFetched(dbCtx, fetched, h.suiteCaps(), h.suiteGuard)
	if err != nil {
		return sfResult{err: fmt.Errorf("handler: put url row: %w", err), status: fres.Status}
	}
	if fetched.Create != nil {
		h.noteCreate(sr.key, stored.Create)
		if stored.Create.Outcome == cache.SuiteCreated {
			h.markCachedSignedAnchors(dbCtx, sr.key, req.Path)
		}
	}
```

  and set `suiteTracked: stored.Tracked` in the returned `sfResult`. `inReleaseSuffix`'s doc becomes `// inReleaseSuffix is the path suffix that identifies the InRelease` / `// file under a suite path (anchorStart's anchor test).`

In `cmd/apt-cacher-ultra/main.go`, `handlerConfig` sets `MaxSuites: cfg.Freshness.MaxSuites` and `SuiteGuard: guard`, where before the literal:

```go
	// Spec C3: the checker holds an evicted suite's mutex. A nil checker
	// must stay a nil interface (a typed nil would be called).
	var guard cache.SuiteGuard
	if freshChecker != nil {
		guard = freshChecker
	}
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/handler/ ./cmd/... ./internal/cache/ && go test ./...`
Expected: PASS (the cache's `TestOnlyTheHandlerCreatesSuiteRows` included).

- [ ] **Step 5: Mutation check (do not commit)**

One at a time, reverting each: (a) `suiteSeed` stamps regardless of the adopter → `TestServeHTTP_AdopterWiredMissesCreateUnstampedRows` FAILS; (b) it never stamps → `TestServeHTTP_AdoptionDisabledSeedParity` FAILS (`stamps check=<nil>`); (c) `file.CreatesRow(true)` → the parity test FAILS (`/r/dists/t: a row was created`); (d) skip the sniff (`|| true`) → the parity and adopter-wired tests FAIL (`/r/dists/v: a row was created`); (e) mark only when `!trackedAtStart` → `TestServeHTTP_MarkerPrecedesTheFirstBodyByte` FAILS (`refresh miss: no marker`); (f) ignore `StoreFetched`'s error → `TestServeHTTP_FailedMissWriteServesNoBody` FAILS (`status 200`); (g) drop `!h.rejections.rejected(sr.key)` → `TestServeHTTP_SuiteCapRejectionIsCachedCountedAndLogged` FAILS (`rose by 2`); (h) no WARN rate limit → `TestNoteCreate_WarnIsRateLimited` FAILS; (i) cached refusals never expire → `TestSeedRejections_TTLAndBound` FAILS; (j) skip `registerLate` → `TestServeHTTP_RowCreatingServeRegistersWithTheBarrier` (`gate entries … = 0`) and `TestServeHTTP_LateRegistrationThatWaitedServesTheNewSnapshot` FAIL; (k) keep the late registration but not the re-dispatch → the latter FAILS (it serves the fetched bytes); (l) `var guard cache.SuiteGuard = freshChecker` → `TestHandlerConfig_CarriesTheSuiteCapsAndGuard` FAILS.

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/handler/ cmd/apt-cacher-ultra/main.go cmd/apt-cacher-ultra/suite_caps_wiring_test.go
golangci-lint run ./...
git add internal/handler/ cmd/apt-cacher-ultra/main.go cmd/apt-cacher-ultra/suite_caps_wiring_test.go
git commit -m "Create suite rows and signed-seen markers in the miss path's writer operation, under the suite caps"
```

---

### Task 10: The handler's hit path: onboarding, touches, by-hash for known suites

Spec C3, handler side, for hits: a url_path hit on a cached, sniff-passing anchor (or flat index) of a suite without a row creates the row before the body — legacy `dists/` onboarding, and suites once refused at a cap — with its marker (deviation 2; design notes 3, 4); client interest from anchor serves (`InRelease`, `Release`, `Release.gpg`; status below 400: 200, 206, 304, HEAD; snapshot, url_path, miss, recovery and stale serves) and from hash-validated `.deb` serves (the vouching suites, `DeclaredHash.SuitePath`), coalesced in the cache (design notes 7, 8); "the handler's by-hash content-addressed fallback requires a known suite (I-4, handler side)". Deferred minors in `handler.go` and `byhash_displaced_test.go`, files this task touches: C-21 (its query half: one `SuiteExists` read in `anchorStart`; the context fast path needs new gate API and stays), C-22 (a failed registration read registers), C-23 (a test that no read happens while the barrier is off, via `existsReads`), C-24 (the touch goroutine recovers, through one helper the T1 goroutine now shares — `logGoroutinePanic`, one stack per `where`), C-25 (a displaced member's missing blob is logged), C-26 (three missing negatives). The one by-hash test whose suite had no row (`TestServeHTTP_ByHashContentAddressed_PoolHit`) gets one.

**Files:**
- Modify: `internal/handler/suite_rows.go` (`onboard`, `touchAsync`, `servedSuite`, `vouchingSuites`, `vouchedSuites`), `internal/handler/handler.go`, `internal/handler/singleflight.go` (`vouchingSuites`), `internal/handler/byhash_test.go`, `internal/handler/byhash_displaced_test.go`
- Create: `internal/handler/suite_hit_test.go`

**Interfaces:**
- Consumes: Task 9's `suiteReq`, `suiteKeyOf`, `suiteSeed`, `suiteCaps`, `noteCreate`, `readHead`, `markCachedSignedAnchors`, `registerLate`, `seedRejections`, and its test rig (`newSuiteRig`, `serve`, `get`, `suite`, `marked`, `secondConn`, fixtures, `derefI64`); Task 6's `TouchSuite`, `SuiteExists`, `DeclaredHash.SuitePath`; Task 4's `CreateSuite`; test helpers `writeBlob`, `commitInlineSnapshot`, `splitURL`, `hostKey`, `newProxyReqHostPort` (`phase2_handler_test.go`), `newFlipStack`, `syncLog`, `logRecords`, `newDisplacedHandler`, `adoptWithMembers`, `aliasOf`, `getMember`, `itoa`.
- Produces: `(h *Handler) onboard(r *http.Request, req *proxy.Request, sr *suiteReq, blobHash string) (held bool)`; `(h *Handler) touchAsync(req *proxy.Request, touchURL bool, suites []cache.SuiteKey)`; `servedSuite(sr *suiteReq, status int) []cache.SuiteKey`; `vouchingSuites(rows []cache.DeclaredHash, hash string) []string`; `vouchedSuites(req *proxy.Request, v packageValidation, status int) []cache.SuiteKey`; `packageValidation.Suites []string`; `sfResult.vouchingSuites []string`; `(h *Handler) logGoroutinePanic(event string, stackOnce *atomic.Bool, v any, attrs ...any)`; `(h *Handler) suiteExists(ctx, key) (bool, error)`; handler fields `existsReads atomic.Int64`, `suiteExistsForTest func(cache.SuiteKey) (bool, error)`; `tryURLPathHit(…, start time.Time, sr *suiteReq)`, `tryByHashContentAddressed(…, sr *suiteReq)`; event `handler_goroutine_panicked` (Task 13 documents it); test helpers `cacheAnchor`, `waitFor`, `requested`, `trackOld` (`suite_hit_test.go`).

- [ ] **Step 1: Write the failing tests**

Create `internal/handler/suite_hit_test.go`:

```go
package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
)

// cacheAnchor stores body as the url_path row of path, as a binary before
// phase D left it: no suite row, no marker. seen is its request and fetch
// stamp.
func (r *suiteRig) cacheAnchor(path string, body []byte, seen int64) {
	r.t.Helper()
	hash := writeBlob(r.t, r.h, body)
	if err := r.c.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
		Path: path, BlobHash: &hash, UpstreamURL: r.srv.URL + path, IsMetadata: true,
		LastRequestedAt: &seen, RequestCount: 1, LastFetchedAt: &seen}); err != nil {
		r.t.Fatal(err)
	}
}

// waitFor polls cond for up to 2 s (asynchronous touches and T1).
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Spec C3 legacy onboarding (deviation 2): a url_path hit on a cached
// sniff-passing anchor of a suite without a row creates the row (NULL
// check stamps, design note 3) before the body, marks its base — the
// anchor in hand and the suite's other cached signed anchors
// (markCachedSignedAnchors) — and the hit's T1 then checks the suite. A
// plain-text InRelease onboards nothing; without an adopter a Release
// does not either.
func TestServeHTTP_HitOnboardsALegacyAnchor(t *testing.T) {
	for _, adoption := range []bool{false, true} {
		r := newSuiteRig(t, adoption, 0)
		r.cacheAnchor("/r/dists/s/InRelease", rowInRelease, 1_700_000_000)
		if rec := r.get("/r/dists/s/InRelease"); rec.Code != http.StatusOK || rec.Header().Get(hdrXCache) != cacheHit {
			t.Fatalf("adoption=%v: hit = %d %q", adoption, rec.Code, rec.Header().Get(hdrXCache))
		}
		s := r.suite("/r/dists/s")
		if s == nil || s.LastCheckAt != nil || s.LastSuccessAt != nil || s.InReleaseETag != nil || s.LastRequestedAt == nil {
			t.Fatalf("adoption=%v: row = %+v, want onboarded with NULL check stamps", adoption, s)
		}
		if !r.marked("/r/dists/s") {
			t.Fatalf("adoption=%v: the onboarded InRelease earned no marker", adoption)
		}
		select {
		case <-r.fresh.signal:
		case <-time.After(2 * time.Second):
			t.Fatalf("adoption=%v: the hit fired no T1", adoption)
		}

		r.cacheAnchor("/r/dists/p/InRelease", []byte("Origin: plain\n"), 1_700_000_000)
		r.get("/r/dists/p/InRelease")
		if r.suite("/r/dists/p") != nil {
			t.Errorf("adoption=%v: a plain-text InRelease onboarded its suite", adoption)
		}

		r.cacheAnchor("/r/dists/d/Release", rowRelease, 1_700_000_000)
		r.cacheAnchor("/r/dists/d/Release.gpg", rowGPG, 1_700_000_100)
		r.get("/r/dists/d/Release")
		if got := r.suite("/r/dists/d") != nil; got != adoption {
			t.Fatalf("adoption=%v: Release hit created a row = %v", adoption, got)
		}
		if adoption {
			// Release itself earns no marker; the cached Release.gpg does,
			// at its own stamp.
			if at, ok, err := r.c.SignedSeen(context.Background(), "http", "127.0.0.1", "/r/dists/d"); err != nil || !ok || at != 1_700_000_100 {
				t.Fatalf("marker = %d %v %v, want the cached Release.gpg's 1700000100", at, ok, err)
			}
		}
	}
}

// requested reads a suite's client-interest stamp.
func (r *suiteRig) requested(suite string) int64 {
	r.t.Helper()
	s := r.suite(suite)
	if s == nil {
		r.t.Fatalf("%s has no row", suite)
	}
	return derefI64(s.LastRequestedAt)
}

// trackOld gives suite a row whose client-interest stamp is old and whose
// touch is due.
func (r *suiteRig) trackOld(suite string) {
	r.t.Helper()
	if err := r.c.SeedSuiteForTest(context.Background(), cache.SuiteFreshness{CanonicalScheme: "http",
		CanonicalHost: "127.0.0.1", SuitePath: suite}); err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.secondConn().Exec(`UPDATE suite_freshness SET last_requested_at = 1000 WHERE suite_path = ?`, suite); err != nil {
		r.t.Fatal(err)
	}
}

// Spec C3 client interest: a tracked suite's anchor served from the
// url_path cache, from the miss path, or stale records interest (TouchSuite).
func TestServeHTTP_AnchorServesTouchTheirSuite(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	r.trackOld("/r/dists/hit")
	r.cacheAnchor("/r/dists/hit/InRelease", rowInRelease, 1_700_000_000)
	r.get("/r/dists/hit/InRelease")
	waitFor(t, "the hit's touch", func() bool { return r.requested("/r/dists/hit") > 1000 })

	r.trackOld("/r/dists/miss")
	r.serve("/r/dists/miss/Release.gpg", upFile{body: rowGPG})
	r.get("/r/dists/miss/Release.gpg")
	waitFor(t, "the miss's touch", func() bool { return r.requested("/r/dists/miss") > 1000 })

	r.trackOld("/r/dists/stale")
	r.cacheAnchor("/r/dists/stale/InRelease", rowInRelease, 1_700_000_000)
	r.h.serve = config.ServeConfig{ServeStaleWhenUpstreamDown: true}
	req, err := r.h.parser.Parse(r.srv.URL+"/r/dists/stale/InRelease", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if !r.h.tryServeStale(httptest.NewRecorder(), proxyReq("GET", r.srv.URL, "/r/dists/stale/InRelease"), req, 0, time.Now()) {
		t.Fatal("no stale serve")
	}
	waitFor(t, "the stale serve's touch", func() bool { return r.requested("/r/dists/stale") > 1000 })
}

// Spec C3: a hash-validated .deb serve touches every suite whose current
// snapshot vouched it, so install-only clients keep their suite alive.
func TestServeHTTP_VouchedDebServeTouchesItsSuite(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	deb := []byte("a vouched .deb")
	debHash := writeBlob(t, r.h, deb)
	inrel := writeBlob(t, r.h, rowInRelease)
	const debPath = "/r/pool/main/h/hello_1_amd64.deb"
	commitInlineSnapshot(t, r.h, "http", "127.0.0.1", "/r/dists/s", inrel,
		[]cache.SnapshotMember{{Path: "InRelease", BlobHash: inrel, DeclaredSHA256: inrel}},
		[]cache.PackageHash{{CanonicalScheme: "http", CanonicalHost: "127.0.0.1", Path: debPath, DeclaredSHA256: debHash}})
	if _, err := r.secondConn().Exec(`UPDATE suite_freshness SET last_requested_at = 1000`); err != nil {
		t.Fatal(err)
	}
	now := int64(1_700_000_000)
	if err := r.c.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
		Path: debPath, BlobHash: &debHash, UpstreamURL: r.srv.URL + debPath, LastRequestedAt: &now, RequestCount: 1,
		LastFetchedAt: &now}); err != nil {
		t.Fatal(err)
	}
	if rec := r.get(debPath); rec.Code != http.StatusOK || rec.Body.String() != string(deb) {
		t.Fatalf("deb hit = %d %q", rec.Code, rec.Body.String())
	}
	waitFor(t, "the vouched .deb's touch", func() bool { return r.requested("/r/dists/s") > 1000 })
}

// Spec C3: which serves record interest in which suite.
func TestServedSuite_Rules(t *testing.T) {
	key := cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/s"}
	for _, tc := range []struct {
		file    aptpath.SuiteFile
		tracked bool
		status  int
		want    bool
	}{
		{aptpath.FileInRelease, true, 200, true},
		{aptpath.FileRelease, true, 304, true},
		{aptpath.FileReleaseGPG, true, 206, true},
		{aptpath.FileFlatIndex, true, 200, true},
		{aptpath.FileInRelease, true, 404, false},
		{aptpath.FileInRelease, false, 200, false},
		{aptpath.NotSuiteFile, true, 200, false},
	} {
		got := servedSuite(&suiteReq{key: key, file: tc.file, tracked: tc.tracked}, tc.status)
		if (got != nil) != tc.want || (got != nil && got[0] != key) {
			t.Errorf("file %v tracked %v status %d: %v, want touch=%v", tc.file, tc.tracked, tc.status, got, tc.want)
		}
	}
	req := &proxy.Request{CanonicalScheme: "http", CanonicalHost: "h"}
	if got := vouchedSuites(req, packageValidation{Validated: true, Suites: []string{"/a", "/b"}}, 200); len(got) != 2 ||
		got[1] != (cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/b"}) {
		t.Errorf("vouched = %v", got)
	}
	if vouchedSuites(req, packageValidation{Suites: []string{"/a"}}, 200) != nil ||
		vouchedSuites(req, packageValidation{Validated: true, Suites: []string{"/a"}}, 404) != nil {
		t.Error("an unvalidated or failed serve touched a suite")
	}
	rows := []cache.DeclaredHash{{DeclaredSHA256: "x", SuitePath: "/a"}, {DeclaredSHA256: "x", SuitePath: "/a"},
		{DeclaredSHA256: "y", SuitePath: "/b"}, {DeclaredSHA256: "x", SuitePath: "/c"}}
	if got := vouchingSuites(rows, "x"); !slices.Equal(got, []string{"/a", "/c"}) {
		t.Errorf("vouchingSuites = %v, want [/a /c]", got)
	}
}

// Deferred minor C-23: anchorStart reads nothing while the barrier is off,
// and one EXISTS per anchor request while it is on.
func TestAnchorStart_ReadsOnlyWhileTheBarrierIsOn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		off := newFlipStack(t, 0)
		off.adopt("/u/dists/s", 1)
		off.get("/u/dists/s/InRelease", 0)
		if n := off.h.existsReads.Load(); n != 0 {
			t.Fatalf("barrier off: %d registration reads, want 0", n)
		}
		on := newFlipStack(t, 10*time.Second)
		on.adopt("/u/dists/s", 1)
		on.get("/u/dists/s/InRelease", 0)
		on.get("/u/dists/s/main/binary-amd64/Packages", 0)
		if n := on.h.existsReads.Load(); n != 1 {
			t.Fatalf("barrier on: %d registration reads for one anchor and one index, want 1", n)
		}
	})
}

// Deferred minor C-22: a failed registration read registers the anchor
// serve anyway (the safe direction: a missed registration could hand the
// client the old anchor after a flip).
func TestAnchorStart_RegistersOnAReadError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newFlipStack(t, 10*time.Second)
		s.h.suiteExistsForTest = func(cache.SuiteKey) (bool, error) { return false, errors.New("injected read failure") }
		s.get("/u/dists/s/InRelease", 0)
		if n := s.gate.Len(); n != 1 {
			t.Fatalf("gate entries after a failed read = %d, want 1 (registered)", n)
		}
	})
}

// Deferred minor C-24: the touch goroutine recovers its panic and logs it
// once with the stack.
func TestTouchAsync_RecoversItsPanic(t *testing.T) {
	logs := &syncLog{}
	h := &Handler{logger: slog.New(slog.NewJSONHandler(logs, nil))} // nil cache: the touch panics
	touchPanicStackLogged.Store(false)
	h.touchAsync(&proxy.Request{CanonicalScheme: "http", CanonicalHost: "h", Path: "/r/dists/s/InRelease"}, true, nil)
	recs := logRecords(logs, "handler_goroutine_panicked", slog.LevelError)
	if len(recs) != 1 || recs[0]["where"] != "touch" || recs[0]["panic"] == nil || !strings.Contains(recs[0]["stack"].(string), "touchAsync") {
		t.Fatalf("records = %+v; want one with where=touch, the panic and its stack", recs)
	}
}
```

In `internal/handler/byhash_test.go`, `TestServeHTTP_ByHashContentAddressed_PoolHit`: before `// No url_path row for the by-hash URL itself. No snapshot.` add

```go
	// I-4 (spec C3): only a known suite — one with a row — is served by
	// hash from the pool.
	if err := h.cache.EnsureSuiteForTest(context.Background(), scheme, hostKey(host, port), "/dists/noble"); err != nil {
		t.Fatal(err)
	}
```

and after that test add:

```go
// I-4, handler side (spec C3): a by-hash path under a suite without a row
// — a minted suite name — is not served from the pool, whatever blob its
// host holds; it goes to the miss path.
func TestServeHTTP_ByHashContentAddressed_UnknownSuiteIsNotServed(t *testing.T) {
	upstreamCalls := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		http.NotFound(w, nil)
	}))
	defer srv.Close()
	h := newTestHandler(t, nil, nil)
	scheme, host, port := splitURL(t, srv.URL)
	body := []byte("by-hash Packages bytes")
	hash := writeBlob(t, h, body)
	if err := h.cache.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: scheme, CanonicalHost: hostKey(host, port),
		Path: "/dists/noble/main/binary-amd64/Packages.gz", BlobHash: &hash, IsMetadata: true}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newProxyReqHostPort("GET", scheme, host+port, "/dists/minted/main/binary-amd64/by-hash/SHA256/"+hash))
	if rec.Body.String() == string(body) || upstreamCalls.Load() != 1 {
		t.Fatalf("status %d, %d upstream calls; want no pool serve and one miss", rec.Code, upstreamCalls.Load())
	}
}
```

In `internal/handler/byhash_displaced_test.go`:
- Deferred minor C-26 (c): `TestServeHTTP_RetainedByHashSHA256MustMatchTheBlob`, `…SearchesKeepDisplacedPlusOne`, `…IsOffWhenKeepDisplacedIsZero` and `…NeverServesRetiredSnapshots` keep `up` from `newDisplacedHandler` and, after their last 404 check, assert `up.count() == 0` (`// deferred minor C-26: a snapshot 404, not a miss`).
- Deferred minor C-25: `TestServeHTTP_RetainedByHashLostBlobIs404WithoutAFetch` passes a `logs := &syncLog{}` to `newDisplacedHandler`, keeps `id1 :=` from the first `adoptWithMembers`, and ends with

```go
	recs := logRecords(logs, "snapshot_member_blob_missing", slog.LevelWarn)
	if len(recs) != 1 || recs[0]["snapshot_id"] != float64(id1) || recs[0]["snapshot_displaced"] != true {
		t.Fatalf("snapshot_member_blob_missing records = %+v, want one for snapshot %d, displaced", recs, id1)
	}
```

- Deferred minor C-26 (a), (b), after that test:

```go
// Deferred minor C-26 (a): a by-hash member of the current snapshot is an
// ordinary snapshot serve — the current X-Cache-Snapshot, no
// snapshot_displaced field.
func TestServeHTTP_CurrentSnapshotByHashIsNotDisplaced(t *testing.T) {
	logs := &syncLog{}
	h, up := newDisplacedHandler(t, 3, logs)
	adoptWithMembers(t, h, "one", map[string]string{aliasOf("Packages one"): "Packages one"})
	id2 := adoptWithMembers(t, h, "two", map[string]string{aliasOf("Packages two"): "Packages two"})
	rec := getMember(t, h, aliasOf("Packages two"))
	if rec.Code != http.StatusOK || rec.Header().Get(hdrXCacheSnapshot) != itoa(id2) {
		t.Fatalf("got %d from snapshot %q, want 200 from the current %d", rec.Code, rec.Header().Get(hdrXCacheSnapshot), id2)
	}
	if strings.Contains(logs.String(), `"snapshot_displaced"`) || up.count() != 0 {
		t.Fatalf("a current-snapshot serve logged snapshot_displaced or asked upstream:\n%s", logs.String())
	}
}

// Deferred minor C-26 (b): retained serving is for by-hash members only;
// an index only a displaced snapshot declared stays a 404, with no miss.
func TestServeHTTP_RetainedByHashIsOnlyForByHashMembers(t *testing.T) {
	h, up := newDisplacedHandler(t, 3, nil)
	adoptWithMembers(t, h, "one", map[string]string{"main/binary-amd64/Packages": "Packages one"})
	adoptWithMembers(t, h, "two", nil)
	if rec := getMember(t, h, "main/binary-amd64/Packages"); rec.Code != http.StatusNotFound || up.count() != 0 {
		t.Fatalf("got %d with %d upstream calls, want a snapshot 404", rec.Code, up.count())
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/handler/ -run XXX`
Expected: build failure naming `undefined: servedSuite`, `undefined: vouchedSuites`, `unknown field Suites in struct literal of type packageValidation`, `undefined: vouchingSuites`, `….existsReads undefined`.

- [ ] **Step 3: Implement**

Append to `internal/handler/suite_rows.go` (imports gain `slices`, `sync/atomic` and `internal/wallclock`):

```go
// onboard creates, before the body is served, the row of an untracked
// suite whose cached anchor (or flat index) a url_path hit serves — the
// legacy onboarding of spec C3, for anchors cached before this release or
// while the suite was refused a row: sniff the blob, CreateSuite (marking
// a sniff-passing InRelease's base in the same transaction), mark the
// suite's other cached signed anchors, and register the serve with the
// flip barrier. It reports whether that registration waited in a flip's
// hold: the caller then serves through the snapshot path, which may have
// changed meanwhile.
func (h *Handler) onboard(r *http.Request, req *proxy.Request, sr *suiteReq, blobHash string) (held bool) {
	if sr.tracked || !sr.file.CreatesRow(h.adoptionEnabled) || h.rejections.rejected(sr.key) {
		return false
	}
	head, err := readHead(h.cache.BlobPath(blobHash), sr.file.HeadBytes())
	if err != nil || !sr.file.Sniff(req.Path, head) {
		return false
	}
	now := wallclock.Unix()
	seed := h.suiteSeed(sr, false, "", "", now)
	if sr.file.Marks() {
		seed.SignedSeenAt = now
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cancel()
	res, err := h.cache.CreateSuite(ctx, *seed, h.suiteCaps(), h.suiteGuard)
	if err != nil {
		// Untracked suites need no marker: serving on is safe, and the next
		// hit tries again.
		h.logger.Warn("suite onboarding failed",
			"err", err, "canonical_host", req.CanonicalHost, "suite_path", req.SuitePath)
		return false
	}
	h.noteCreate(sr.key, res)
	switch res.Outcome {
	case cache.SuiteCreated:
		h.markCachedSignedAnchors(ctx, sr.key, req.Path)
		sr.tracked = true
	case cache.SuiteExists:
		sr.tracked = true
	default:
		return false
	}
	return h.registerLate(r, req, sr)
}

// touchAsync updates the url_path row's last_requested_at and
// request_count (touchURL) and records client interest in suites (spec C3
// TouchSuite, coalesced in the cache) without blocking the response: the
// caller runs it as a goroutine. A fresh context keeps a disconnected
// client from orphaning the writes. Deferred minor C-24: it recovers its
// panics.
func (h *Handler) touchAsync(req *proxy.Request, touchURL bool, suites []cache.SuiteKey) {
	defer func() {
		if v := recover(); v != nil {
			h.logGoroutinePanic("handler_goroutine_panicked", &touchPanicStackLogged, v,
				"where", "touch", "canonical_host", req.CanonicalHost, "path", req.Path)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if touchURL {
		if err := h.cache.TouchURLPath(ctx, req.CanonicalScheme, req.CanonicalHost, req.Path); err != nil {
			h.logger.Debug("touch failed", "err", err, "canonical_host", req.CanonicalHost, "path", req.Path)
		}
	}
	for _, k := range suites {
		if err := h.cache.TouchSuite(ctx, k); err != nil {
			h.logger.Debug("suite touch failed", "err", err, "canonical_host", k.CanonicalHost, "suite_path", k.SuitePath)
		}
	}
}

// touchPanicStackLogged limits the touch goroutine's panic stack to one
// per process.
var touchPanicStackLogged atomic.Bool

// servedSuite returns the suite a served suite file records interest in
// (spec C3): the request's own suite when it is tracked and the response
// status is below 400 (304, HEAD and stale serves included).
func servedSuite(sr *suiteReq, status int) []cache.SuiteKey {
	if !sr.file.Touches() || !sr.tracked || status >= http.StatusBadRequest {
		return nil
	}
	return []cache.SuiteKey{sr.key}
}

// vouchingSuites returns the distinct suite paths of the rows that declare
// hash (the suites a validated .deb serve keeps alive).
func vouchingSuites(rows []cache.DeclaredHash, hash string) []string {
	var out []string
	for _, r := range rows {
		if r.DeclaredSHA256 == hash && !slices.Contains(out, r.SuitePath) {
			out = append(out, r.SuitePath)
		}
	}
	return out
}

// vouchedSuites returns the suites whose snapshots vouched a served .deb
// (spec C3: install-only clients keep their suite alive).
func vouchedSuites(req *proxy.Request, v packageValidation, status int) []cache.SuiteKey {
	if !v.Validated || status >= http.StatusBadRequest {
		return nil
	}
	out := make([]cache.SuiteKey, 0, len(v.Suites))
	for _, s := range v.Suites {
		out = append(out, cache.SuiteKey{CanonicalScheme: req.CanonicalScheme, CanonicalHost: req.CanonicalHost, SuitePath: s})
	}
	return out
}
```

In `internal/handler/singleflight.go`, `sfResult` gains `// vouchingSuites: the suites whose snapshots vouched a validated .deb.` / `vouchingSuites []string`.

In `internal/handler/handler.go`:

- `Handler` gains, after `seedWarn`: `existsReads atomic.Int64` and `suiteExistsForTest func(cache.SuiteKey) (bool, error)` (comment: "existsReads counts anchorStart's registration reads (tests: deferred minor C-23, no read while the barrier is off); suiteExistsForTest, when set (tests only), answers them instead of the cache."). `packageValidation` gains `Suites []string` ("the suite paths whose current snapshots vouched the served bytes (spec C3: a vouched .deb serve touches them). Not logged.").
- After `t1PanicStackLogged`, add the shared helper, and make `maybeFireFreshness`'s recover call `h.logGoroutinePanic("freshness_check_panicked", &t1PanicStackLogged, r, "where", "t1", "canonical_host", req.CanonicalHost, "suite_path", req.SuitePath)`:

```go
// logGoroutinePanic logs a recovered panic of a handler goroutine as
// event, with the stack the first time stackOnce is claimed in this
// process (one flag per where: the T1 check, the touch goroutine).
func (h *Handler) logGoroutinePanic(event string, stackOnce *atomic.Bool, v any, attrs ...any) {
	attrs = append(attrs, "panic", fmt.Sprint(v))
	if stackOnce.CompareAndSwap(false, true) {
		attrs = append(attrs, "stack", string(debug.Stack()))
	}
	h.logger.Error(event, attrs...)
}
```

- Delete the old `touchAsync` (it moved to `suite_rows.go`) and put in its place:

```go
// suiteExists is anchorStart's registration read: one indexed EXISTS
// (suiteExistsForTest replaces it in tests).
func (h *Handler) suiteExists(ctx context.Context, key cache.SuiteKey) (bool, error) {
	if h.suiteExistsForTest != nil {
		return h.suiteExistsForTest(key)
	}
	return h.cache.SuiteExists(ctx, key)
}
```

- `anchorStart`: the `GetSuiteFreshness` check becomes

```go
	h.existsReads.Add(1)
	exists, err := h.suiteExists(r.Context(), suiteKeyOf(req))
	switch {
	case err == nil && !exists:
		return nil // an untracked suite creates no gate state
	case err != nil && r.Context().Err() != nil:
		return nil // the client left during the read
	}
```

  and its AIDEV-NOTE's sentence "The row check before it — which is what decides whether a suite is tracked — therefore costs one extra indexed read per <suite>/InRelease and <suite>/Release request, tracked or not, while the barrier is on (none when it is off)." becomes "… — which is what decides whether a suite is tracked — therefore costs one indexed EXISTS read (SuiteExists; deferred minor C-21) per <suite>/InRelease and <suite>/Release request, tracked or not, while the barrier is on (none when it is off; existsReads counts them). A failed read registers anyway (deferred minor C-22): an unneeded registration only delays a flip by this serve, a missed one could hand the client the old anchor after the flip."
- `trySnapshotHit`: the displaced by-hash serve calls `go h.touchAsync(req, true, nil)`; after `serveSnapshotMemberMiss`, `if suites := servedSuite(sr, status); served && suites != nil { go h.touchAsync(req, false, suites) }`; the ordinary serve calls `go h.touchAsync(req, true, servedSuite(sr, status))`.
- `tryCacheHit` calls `h.tryURLPathHit(w, r, req, start, sr)` and `h.tryByHashContentAddressed(w, r, req, sr)`.
- `tryURLPathHit(w, r, req, start time.Time, sr *suiteReq)`: after the `.deb` validation block, the rest of the function becomes

```go
	// Spec C3 legacy onboarding: a cached anchor (or flat index) of a suite
	// without a row creates the row before its body is served.
	if req.IsMetadata && h.onboard(r, req, sr, *row.BlobHash) {
		// The registration waited in a flip's hold: what is current now
		// may be a snapshot (design note 9).
		if served, status, body, handled, displaced := h.trySnapshotHit(w, r, req, start, sr); handled {
			return served, "", status, body, packageValidation{SnapshotDisplaced: displaced}
		}
	}

	served, status, bytesWritten = h.serveBlobWithHeaders(w, r, req, *row.BlobHash, snapshotMeta{
		lastFetchedAt: row.LastFetchedAt,
	})
	if !served {
		return false, "", 0, 0, packageValidation{}
	}
	go h.touchAsync(req, true, append(servedSuite(sr, status), vouchedSuites(req, v, status)...))
	h.maybeFireFreshness(req)
	return true, "", status, bytesWritten, v
```

- `checkPackageHash`'s matching-row return adds `Suites: vouchingSuites(rows, distinct[0]),`.
- `tryByHashContentAddressed(…, sr *suiteReq)`: right after the `req.SuitePath == ""` check, `if !sr.tracked { return false, 0, 0 }` with the comment "I-4, handler side (spec C3): only a known suite (one with a row) is served from the pool by hash; a minted suite name falls through to the miss path and creates nothing here."; `byHashSuffixRegex`'s AIDEV-NOTE ends "Since phase D only known suites (with a suite_freshness row) get it (I-4, spec C3)."
- `tryDisplacedByHash`: inside the loop, after the `served` return, log the miss (C-25): `h.logger.Warn("snapshot_member_blob_missing", "canonical_host", req.CanonicalHost, "path", req.Path, "snapshot_id", row.SnapshotID, "blob_hash", row.BlobHash, "snapshot_displaced", true)` with the comment "Deferred minor C-25: no recovery fetch for a displaced member, but its missing blob is logged like a current member's."
- `serveCacheMiss`: replace the final `logRequestWithValidation(…, packageValidation{Validated: res.validatedHash, PackageName: res.packageName})` with

```go
	v := packageValidation{Validated: res.validatedHash, PackageName: res.packageName, Suites: res.vouchingSuites}
	if suites := append(servedSuite(sr, cw.statusCode()), vouchedSuites(req, v, cw.statusCode())...); len(suites) > 0 {
		go h.touchAsync(req, false, suites)
	}
	h.logRequestWithValidation(r, req.CanonicalHost, req.Path, logOutcome, cw.statusCode(), cw.bytes, true, res.status, start, v)
```

- `runFetch`: declare `vouchers []string` beside `validatedHash`/`packageName`, set `vouchers = vouchingSuites(rows, distinct[0])` where `packageName = firstPackageName(rows, distinct[0])` is set, and return `vouchingSuites: vouchers` (realign the `sfResult` literal).
- `tryServeStale`: right after its `http.ServeContent`:

```go
	// Spec C3: a stale anchor serve records client interest too. Whether
	// the suite has a row is not known here; TouchSuite is UPDATE-only, so
	// an untracked suite costs one no-op write per stale serve.
	stale := &suiteReq{key: suiteKeyOf(req), file: aptpath.ClassifySuiteFile(req.SuitePath, req.Path), tracked: true}
	if suites := servedSuite(stale, cw.statusCode()); suites != nil {
		go h.touchAsync(req, false, suites)
	}
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/handler/ && go test ./...`
Expected: PASS.

- [ ] **Step 5: Mutation check (do not commit)**

One at a time, reverting each: (a) skip `onboard` → `TestServeHTTP_HitOnboardsALegacyAnchor` FAILS (`row = <nil>`); (b) onboard without `SignedSeenAt` → the same test FAILS (`earned no marker`); (c) skip `markCachedSignedAnchors` in `onboard` → it FAILS (`want the cached Release.gpg's 1700000100`); (d) onboard without the sniff (`&& false`) → it FAILS (`a plain-text InRelease onboarded its suite`); (e) drop the by-hash `!sr.tracked` check → `TestServeHTTP_ByHashContentAddressed_UnknownSuiteIsNotServed` FAILS; (f) the url_path hit touches only vouched suites → `TestServeHTTP_AnchorServesTouchTheirSuite` FAILS (`the hit's touch`); (g) the miss touches only vouched suites → it FAILS (`the miss's touch`); (h) no stale touch → it FAILS (`the stale serve's touch`); (i) drop `Suites` from the hit's validation → `TestServeHTTP_VouchedDebServeTouchesItsSuite` FAILS; (j) `servedSuite` ignores the status → `TestServedSuite_Rules` FAILS; (k) count a read before the `Enabled` check → `TestAnchorStart_ReadsOnlyWhileTheBarrierIsOn` FAILS; (l) a read error returns nil → `TestAnchorStart_RegistersOnAReadError` FAILS; (m) `touchAsync` without its recover → `TestTouchAsync_RecoversItsPanic` FAILS (the panic escapes); (n) drop the C-25 log → `TestServeHTTP_RetainedByHashLostBlobIs404WithoutAFetch` FAILS.

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/handler/
golangci-lint run ./internal/handler/...
git add internal/handler/
git commit -m "Onboard cached anchors, record client interest and serve by-hash only for known suites"
```

---

### Task 11: Phase D DoD: minted suites at scale, `dists/` onboarding from a v6 cache, the hit-path invariant

The §9 phase D row's DoD tests that Tasks 4–10 did not already provide (the single-INSERT-site test is Task 4's; the adoption-disabled parity test Task 9's): the minted-suite scale test (rows, markers, the lock map, `FlipGate().Len()`, the touch map and the rejection cache stay bounded by the tracked suites; Review Focus 1), `dists/` onboarding on a cache a v6 binary wrote (Review Focus 2), and the hit-path invariant as deviation 1 states it, across first-contact and refresh misses, a cap refusal later lifted, legacy onboarding (also through a `Release` hit, where only `markCachedSignedAnchors` marks), an adoption whose anchor row the checker reconstructs (`reconstructAnchor`), and a restart (the startup pass in `freshness.RunLegacyTrustAudit`). The scale test runs in under 3 s under `-race` (measured 2.9 s), inside the 5 s budget.

**Files:**
- Create: `internal/handler/suite_dod_test.go`
- Modify: `internal/cache/testing_helpers.go` (`MigrateToVersionForTest`)

**Interfaces:**
- Consumes: everything from Tasks 4–10; `freshness.RunLegacyTrustAudit`; `flipgate.MinQuiet`; `simtest.OpenCache`, `simtest.StaticConditional`, `simtest.DiscardLogger`; Task 9's and 10's test helpers (`newSuiteRig`, `serve`, `get`, `suite`, `secondConn`, `cacheAnchor`, fixtures); `writeBlob`, `commitInlineSnapshot`, `newProxyReqHostPort`, `noUpstream`, `silentLogger`.
- Produces: `func MigrateToVersionForTest(ctx context.Context, dir string, version int) error` (`internal/cache/testing_helpers.go`, TEST-ONLY: creates `dir/cache.db` with the migrations before `version` applied); test helpers `mintedUpstream`, `firstByteProbe`, `sqliteFile`, `assertHitPathInvariant`.

- [ ] **Step 1: Write the tests**

Create `internal/handler/suite_dod_test.go`:

```go
package handler

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"
	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// mintedUpstream answers every suite file of any suite on any host with
// bytes that pass the sniffs — a hostile or overly permissive upstream,
// the worst case for row creation.
type mintedUpstream struct{}

func (mintedUpstream) HostAllowed(string) bool { return true }

func (mintedUpstream) Fetch(_ context.Context, target *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error) {
	var body []byte
	switch {
	case strings.HasSuffix(target.URL, "/InRelease"):
		body = simtest.ClearsignedFixture("Origin: minted\nLabel: " + target.URL + "\n")
	case strings.HasSuffix(target.URL, "/Release.gpg"):
		body = rowGPG
	default:
		body = []byte("minted " + target.URL)
	}
	if _, err := dst.Write(body); err != nil {
		return nil, err
	}
	return &fetch.FetchResult{Status: 200, ContentLength: int64(len(body))}, nil
}

// Phase D DoD (spec §9; Review Focus 1): minted dists/ codenames, their
// Release.gpg and by-hash names on many hosts, from an upstream that
// answers all of them with sniff-passing bytes, create rows only up to the
// caps (here 4 per host), markers only for tracked suites, and no growth
// of the checker's lock map, the flip gate or the touch map beyond the
// tracked suites; the rejection cache holds the refused suites only.
func TestServeHTTP_MintedSuitesAtScale(t *testing.T) {
	c := simtest.OpenCache(t)
	gate, err := flipgate.New(flipgate.Config{Quiet: flipgate.MinQuiet})
	if err != nil {
		t.Fatal(err)
	}
	ck, err := freshness.New(freshness.Config{Cache: c, Fetcher: &simtest.StaticConditional{Status: http.StatusNotModified},
		HostLimiter: hostsem.New(64), Logger: simtest.DiscardLogger(), MaxSuites: 40})
	if err != nil {
		t.Fatal(err)
	}
	parser, err := proxy.New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{Parser: parser, Cache: c, Fetch: mintedUpstream{}, Logger: simtest.DiscardLogger(),
		HostLimiter: hostsem.New(64), Freshness: ck, AdoptionEnabled: true, FlipGate: gate, MaxSuites: 40, SuiteGuard: ck})
	if err != nil {
		t.Fatal(err)
	}
	const hosts, perHost, perHostCap = 8, 15, 4
	byHash := "/main/by-hash/SHA256/" + strings.Repeat("ab", 32)
	for i := range hosts {
		for j := range perHost {
			base := fmt.Sprintf("http://h%02d.example/r/dists/minted%03d", i, j)
			for _, p := range []string{"/InRelease", "/Release.gpg", byHash, "/InRelease"} {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, base+p, nil))
				if rec.Code != http.StatusOK {
					t.Fatalf("%s%s = %d", base, p, rec.Code)
				}
			}
		}
	}
	h.Close() // drains the hits' T1 checks
	all, err := c.ListSuites(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tracked := map[string]bool{}
	for _, s := range all {
		tracked[s.CanonicalHost+s.SuitePath] = true
	}
	if len(all) != hosts*perHostCap {
		t.Fatalf("rows = %d, want %d (the per-host cap on every host)", len(all), hosts*perHostCap)
	}
	for i := range hosts {
		for j := range perHost {
			host, suite := fmt.Sprintf("h%02d.example", i), fmt.Sprintf("/r/dists/minted%03d", j)
			_, ok, err := c.SignedSeen(context.Background(), "http", host, suite)
			if err != nil {
				t.Fatal(err)
			}
			if ok != tracked[host+suite] {
				t.Fatalf("%s%s: marker %v, tracked %v; want a marker exactly for tracked suites", host, suite, ok, tracked[host+suite])
			}
		}
	}
	if n := ck.LockCount(); n > len(all) {
		t.Errorf("lock map = %d entries, more than the %d tracked suites", n, len(all))
	}
	if n := gate.Len(); n == 0 || n > len(all) {
		t.Errorf("flip gate = %d entries, want 1..%d (tracked suites only)", n, len(all))
	}
	if n := c.TouchedSuites(); n > len(all) {
		t.Errorf("touch map = %d entries, more than the %d tracked suites", n, len(all))
	}
	if n, want := h.rejections.len(), hosts*(perHost-perHostCap); n != want {
		t.Errorf("rejection cache = %d entries, want the %d refused suites", n, want)
	}
}

// firstByteProbe records, at the first body byte, whether the suite has a
// row and its base a marker.
type firstByteProbe struct {
	*httptest.ResponseRecorder
	c         *cache.Cache
	suite     string
	seen      bool
	row, mark bool
}

func (p *firstByteProbe) Write(b []byte) (int, error) {
	if !p.seen {
		p.seen = true
		ok, err := p.c.SuiteExists(context.Background(), cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "127.0.0.1", SuitePath: p.suite})
		p.row = err == nil && ok
		_, p.mark, _ = p.c.SignedSeen(context.Background(), "http", "127.0.0.1", aptpath.MarkerBase(p.suite))
	}
	return p.ResponseRecorder.Write(b)
}

// sqliteFile opens path on its own connection (fixtures the cache API does
// not write).
func sqliteFile(t *testing.T, path string) *sql.DB {
	t.Helper()
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// Phase D DoD (spec §9; Review Focus 2): a cache a v6 binary wrote — a
// dists/ InRelease cached, no suite row — upgraded in place. The startup
// pass marks nothing (the suite is untracked); the anchor's first hit
// onboards the suite, with its marker, before the first body byte.
func TestServeHTTP_OnboardsADistsSuiteFromAV6Cache(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	if err := cache.MigrateToVersionForTest(ctx, dir, 6); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(rowInRelease)
	hash := hex.EncodeToString(sum[:])
	db := sqliteFile(t, filepath.Join(dir, "cache.db"))
	for _, q := range []string{
		`INSERT INTO blob(hash,size,created_at,refcount) VALUES(?,?,1,0)`,
		`INSERT INTO url_path(canonical_scheme,canonical_host,path,blob_hash,upstream_url,is_metadata,last_requested_at,request_count)
		 VALUES('http','127.0.0.1','/u/dists/s/InRelease',?,'http://127.0.0.1/u/dists/s/InRelease',1,1700000000,3)`,
	} {
		args := []any{hash}
		if strings.Contains(q, "INTO blob") {
			args = append(args, len(rowInRelease))
		}
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	_ = db.Close()
	c, err := cache.Open(ctx, dir, silentLogger()) // migrates v6 → v7
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	w, err := c.NewTempBlob() // the pool file the v6 binary wrote
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(rowInRelease); err != nil {
		t.Fatal(err)
	}
	if got, err := w.Finalize(int64(len(rowInRelease))); err != nil || got != hash {
		t.Fatalf("pool file = %s, %v; want %s", got, err, hash)
	}
	if res, err := freshness.RunLegacyTrustAudit(ctx, c, silentLogger()); err != nil || res.AnchorMarkers != 0 {
		t.Fatalf("startup pass = %+v, %v; want no marker for the untracked suite", res, err)
	}
	parser, err := proxy.New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{Parser: parser, Cache: c, Fetch: noUpstream{}, Logger: silentLogger(), HostLimiter: hostsem.New(4),
		AdoptionEnabled: true, MaxSuites: 5000})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	p := &firstByteProbe{ResponseRecorder: httptest.NewRecorder(), c: c, suite: "/u/dists/s"}
	h.ServeHTTP(p, newProxyReqHostPort("GET", "http", "127.0.0.1", "/u/dists/s/InRelease"))
	if p.Code != http.StatusOK || p.Header().Get(hdrXCache) != cacheHit || !p.seen {
		t.Fatalf("hit = %d %q, body written %v", p.Code, p.Header().Get(hdrXCache), p.seen)
	}
	if !p.row || !p.mark {
		t.Fatalf("at the first body byte row=%v marker=%v, want both", p.row, p.mark)
	}
	s, err := c.GetSuiteFreshness(ctx, "http", "127.0.0.1", "/u/dists/s")
	if err != nil || s.LastCheckAt != nil || s.LastSuccessAt != nil {
		t.Fatalf("onboarded row = %+v, %v; want NULL check stamps (its first check runs at once)", s, err)
	}
}

// assertHitPathInvariant checks deviation 1's invariant: every
// sniff-passing InRelease or Release.gpg url_path row of a tracked suite
// has a marker for its base.
func assertHitPathInvariant(t *testing.T, c *cache.Cache, db *sql.DB, when string) {
	t.Helper()
	rows, err := db.Query(`SELECT canonical_host, path, blob_hash FROM url_path
 WHERE path LIKE '%/InRelease' OR path LIKE '%/Release.gpg'`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	checked := 0
	for rows.Next() {
		var host, path, hash string
		if err := rows.Scan(&host, &path, &hash); err != nil {
			t.Fatal(err)
		}
		suite := strings.TrimSuffix(strings.TrimSuffix(path, "/InRelease"), "/Release.gpg")
		if ok, err := c.SuiteExists(context.Background(), cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: host, SuitePath: suite}); err != nil || !ok {
			continue
		}
		body, err := os.ReadFile(c.BlobPath(hash))
		if err != nil {
			t.Fatal(err)
		}
		file := aptpath.ClassifySuiteFile(suite, path)
		if !file.Sniff(path, body) {
			continue
		}
		checked++
		if _, ok, err := c.SignedSeen(context.Background(), "http", host, aptpath.MarkerBase(suite)); err != nil || !ok {
			t.Errorf("%s: %s of a tracked suite has no marker", when, path)
		}
	}
	if checked == 0 {
		t.Fatalf("%s: no tracked signed anchor to check", when)
	}
}

// Deviation 1, spec §7: after first-contact and refresh misses, a cap
// refusal later lifted, legacy onboarding, an adoption whose anchor row
// the checker reconstructs, and a restart, every sniff-passing anchor row
// of a tracked suite has a marker for its base.
func TestHitPathInvariant_EveryTrackedSignedAnchorHasAMarker(t *testing.T) {
	r := newSuiteRig(t, true, 10) // per-host cap 1
	ctx := context.Background()
	db := r.secondConn()
	r.serve("/r/dists/a/InRelease", upFile{body: rowInRelease})
	r.serve("/r/dists/b/InRelease", upFile{body: rowInRelease})
	r.serve("/r/dists/b/Release.gpg", upFile{body: rowGPG})
	r.get("/r/dists/a/InRelease") // first contact: tracked
	r.get("/r/dists/b/InRelease") // refused at the per-host cap
	r.get("/r/dists/b/Release.gpg")
	if r.suite("/r/dists/b") != nil {
		t.Fatal("premise: b must be refused")
	}
	// The cap is lifted (a restart with a larger max_suites): a second
	// handler over the same cache.
	h2, err := New(Config{Parser: r.h.parser, Cache: r.c, Fetch: r.h.fetch, Logger: silentLogger(),
		HostLimiter: hostsem.New(4), AdoptionEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h2.Close)
	get2 := func(path string) {
		t.Helper()
		rec := httptest.NewRecorder()
		h2.ServeHTTP(rec, proxyReq("GET", r.srv.URL, path))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", path, rec.Code)
		}
	}
	get2("/r/dists/b/InRelease") // onboards b, marking its cached anchors
	r.cacheAnchor("/r/dists/c/InRelease", rowInRelease, 1_700_000_000)
	r.cacheAnchor("/r/dists/c/Release.gpg", rowGPG, 1_700_000_000)
	get2("/r/dists/c/Release.gpg") // legacy rows: Release.gpg creates no row
	get2("/r/dists/c/InRelease")   // onboards c
	r.cacheAnchor("/r/dists/e/Release", rowRelease, 1_700_000_000)
	r.cacheAnchor("/r/dists/e/Release.gpg", rowGPG, 1_700_000_000)
	get2("/r/dists/e/Release") // onboards e: Release marks nothing itself, its cached Release.gpg does
	if _, err := db.Exec(`DELETE FROM url_path WHERE path = '/r/dists/a/InRelease';
DELETE FROM suite_signed_seen WHERE base = '/r/dists/a'`); err != nil {
		t.Fatal(err)
	}
	get2("/r/dists/a/InRelease") // refresh miss
	// d: adopted (CommitAdoption marks); its anchor row is gone, and the
	// checker reconstructs it from the snapshot.
	inrel := writeBlob(t, r.h, rowInRelease)
	commitInlineSnapshot(t, r.h, "http", "127.0.0.1", "/r/dists/d", inrel,
		[]cache.SnapshotMember{{Path: "InRelease", BlobHash: inrel, DeclaredSHA256: inrel}}, nil)
	ck, err := freshness.New(freshness.Config{Cache: r.c, Fetcher: &simtest.StaticConditional{Status: http.StatusNotModified},
		HostLimiter: hostsem.New(4), Logger: silentLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ck.Check(ctx, "http", "127.0.0.1", "/r/dists/d")
	if _, err := r.c.LookupURL(ctx, "http", "127.0.0.1", "/r/dists/d/InRelease"); err != nil {
		t.Fatalf("premise: the checker reconstructs d's anchor row (%v)", err)
	}
	assertHitPathInvariant(t, r.c, db, "after serving")
	// A restart: markers lost (an earlier binary never wrote them), healed
	// by the startup pass before anything serves.
	if _, err := db.Exec(`DELETE FROM suite_signed_seen`); err != nil {
		t.Fatal(err)
	}
	if _, err := freshness.RunLegacyTrustAudit(ctx, r.c, silentLogger()); err != nil {
		t.Fatal(err)
	}
	assertHitPathInvariant(t, r.c, db, "after a restart")
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go vet ./internal/handler/`
Expected: `undefined: cache.MigrateToVersionForTest`.

- [ ] **Step 3: Implement the fixture helper**

Append to `internal/cache/testing_helpers.go` (import `path/filepath`):

```go
// MigrateToVersionForTest creates dir/cache.db at schema version (the
// migrations before it applied, no Go step after it), for upgrade
// fixtures a test then fills with an older binary's rows and opens with
// Open. TEST-ONLY.
func MigrateToVersionForTest(ctx context.Context, dir string, version int) error {
	db, err := openDB(filepath.Join(dir, "cache.db"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	for v := 0; v < version; v++ {
		if err := applyMigration(ctx, db, v); err != nil {
			return fmt.Errorf("MigrateToVersionForTest: migration %d: %w", v, err)
		}
	}
	return nil
}
```

(`TestOnlyTheHandlerCreatesSuiteRows` does not list it: it creates no suite row.)

- [ ] **Step 4: Run the tests**

Run: `go test -race -run 'TestServeHTTP_MintedSuitesAtScale|TestServeHTTP_OnboardsADistsSuiteFromAV6Cache|TestHitPathInvariant_' -v ./internal/handler/`
Expected: PASS, the scale test in under 5 s.

- [ ] **Step 5: Mutation check (do not commit)**

One at a time, reverting each: (a) `suiteCaps` returns `cache.SuiteCaps{}` → `TestServeHTTP_MintedSuitesAtScale` FAILS (`rows = 120, want 32`); (b) skip `onboard` → `TestServeHTTP_OnboardsADistsSuiteFromAV6Cache` FAILS (`row=false marker=false`); (c) `onboard` skips `markCachedSignedAnchors` → `TestHitPathInvariant_EveryTrackedSignedAnchorHasAMarker` FAILS (`/r/dists/e/Release.gpg of a tracked suite has no marker`); (d) mark only when the miss creates the row → the invariant test FAILS (`/r/dists/a/InRelease …`). (The by-hash I-4 check is pinned by Task 10's `TestServeHTTP_ByHashContentAddressed_UnknownSuiteIsNotServed`; the scale test's by-hash names hold no pool blob.)

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/handler/suite_dod_test.go internal/cache/testing_helpers.go
git add internal/handler/suite_dod_test.go internal/cache/testing_helpers.go
git commit -m "Phase D DoD: minted suites at scale, v6 onboarding, the hit-path invariant"
```

---

### Task 12: Deferred minors in the files this phase touches

The deferred-minor rule (overview Global Constraints; the three deferred files' "How they get done"): this phase includes the items that sit in files it changes, except items tagged with another phase (owned by it). Applied as: an item is taken when its fix changes a file a task of this phase edits for its own work; the files Task 4's two rename scripts change only mechanically (a call renamed, nothing else) do not count (B-27 `restart_reset_test.go`, B-36 `check_streak_test.go`, C-12 and C-13 `flip_state_test.go` live in such files, and A-15 asks for a test of the untouched `hot_set.go`, which would go in another, `hot_prefetch_test.go`). Tasks 1–11 took A-17, A-22–A-25, A-30, A-32–A-35, A-38, A-50–A-53, B-1–B-5, B-8 (moot), B-22 (already true), C-21 (query half), C-22–C-26 and C-35. This task takes the rest of the untagged items in those files:

- `internal/freshness/scheduler.go` (Task 7) and `scheduler_test.go`: B-40 (pruning the host whose turn is next resets the turn to the first host), B-41 (the tick's comparator built keys per comparison), B-44 (`checkWorker`'s last-resort recover left no trace: it now counts `acu_freshness_unlogged_panics_total`, declared in `metrics.go`), and, in the test file this task opens, B-39 (no test exercised the in-flight skip), B-42 (the busy re-park test did not check `signed`), B-43 (nothing pinned that an ordinary scheduler check honours the cooldown).
- `internal/freshness/sim_sched_test.go` (Task 4 edits it by hand): B-47 (goroutine slack 16 → 8, as in the capacity simulation), B-48 (seed the rand seam), B-49 (the pacing simulation gains spec §7's +2 h forward step). B-50 was checked: the assertion is already `repairs < 2 || repairs != unchanged`, as tight as its message; Task 13 deletes it.
- `internal/freshness/freshness.go`: C-19's long line in `spawnAdoption`'s AIDEV-NOTE (its other half, splitting the barrier code out of `freshness/adoption.go`, is in a file D does not touch); C-20 (an attempt the shutdown ends before the flip barrier — any error once the lifetime context has ended — is abandoned like a flip in the barrier: not counted, not in the admin ring, logged INFO `adoption_abandoned_at_shutdown`; `spawnAdoption` alone decides it, so `freshness/adoption.go` stays untouched).
- `internal/freshness/flip_barrier_test.go` (Task 4 edits it by hand): C-18 (a duplicated shutdown test merged; three tests seed the served anchor through `putServedAnchor`, which now takes the anchor file).
- `internal/cache/cache_test.go` (Task 4 edits it by hand): A-18 (`openCache` logs nothing).
- `internal/cache/adoption.go` (Tasks 4 and 6): C-10 (the stamp's and the displaced lookup's scoping and ordering predicates had no test), C-14 (the stamp block sat between Step 3a's comment and its loop), C-15 (the read-side snapshot helpers move to `snapshot_reads.go`; `adoption.go` 1,180 → 1,031 lines).
- `internal/handler/sim_flip_test.go` (Task 4 edits it by hand): C-27 (doc).
- `cmd/apt-cacher-ultra/main.go` (Tasks 8, 9) and the test files this task opens: C-28 (AIDEV-NOTE on the gate's close order, and a smoke test), C-30 (the shutdown-wait panic logs its stack), C-31 (pin `flipgate.ErrQuietOutOfRange`), C-32 (the handler-config test), C-33 (an AIDEV-NOTE tying the "5-minute" figures to freshness's `commitTimeout`), C-34 (`waitLogged` → `waitAdoptionDrain`; its test moves to `shutdown_drain_test.go` and the A-48 test to `legacy_audit_wiring_test.go`).

Checked and left in files this phase touches (Task 13 keeps them in their deferred files): tagged for a later phase — B-23, B-30, B-31 (`freshness.go`) and B-66 (`scheduler.go`) for E, C-11 (`cache/adoption.go`) for F, C-9 (`config_test.go`), C-37 (`config.toml.default`), C-38 (`sim_sched_test.go`) and C-39 (`phase2_detached_chaos_test.go`) for H, as phase C left its tagged items; B-46 (`scheduler.go`), kept deferred by ruling; B-28 (`reconcile.go`): skipping the second parse means handing `ReconcileSnapshot` a declared set parsed outside it, against its trust-anchor AIDEV-NOTE, to save a cooldown-bounded cost; B-38, B-45 (`scheduler.go`) and B-51 (`sim_sched_test.go`): accepted behaviour their own text calls documented, whose change would alter a metric's meaning (B-38) or the scheduler's wake protocol (B-45); B-53 (`sim_sched_test.go`): an adopter in the 500-suite simulation adds load to a test already over the 5 s `-race` budget (6.8 s measured; C-38); C-21's context fast path (`handler.go`) needs new `flipgate` API; C-29 (`main.go`) and C-6 (`sim_flip_test.go`) are fixed in files D does not touch (`freshness/legacy_audit.go`; `simtest` and `flipgate`'s tests); A-37's freshness half (`reportPanic` in `freshness.go`, the heartbeat in `freshness/adoption.go`) needs a helper shared across packages.

**Files:**
- Modify: `internal/freshness/scheduler.go`, `internal/freshness/metrics.go`, `internal/freshness/scheduler_test.go`, `internal/freshness/sim_sched_test.go`, `internal/freshness/freshness.go`, `internal/freshness/flip_barrier_test.go`, `internal/cache/cache_test.go` (drift file: by hand), `internal/cache/adoption.go`, `internal/handler/sim_flip_test.go`, `cmd/apt-cacher-ultra/main.go`, `cmd/apt-cacher-ultra/flip_wiring_test.go`, `cmd/apt-cacher-ultra/legacy_audit_wiring_test.go`
- Create: `internal/cache/snapshot_reads.go`, `internal/cache/adoption_scope_test.go`, `cmd/apt-cacher-ultra/shutdown_drain_test.go`

**Interfaces:**
- Consumes: Task 4's `InsertTrackedCandidateForTest` and `derefI64`; cache test helpers `openCache`, `seedBlob`, `stubNow`; the scheduler test helpers `schedRow`, `seedFakeSuite`, `changedUpstream`, `newFakeCache`, `discardLogger`, `renderedMetric`, `simtest.NewUpstream`; the retry rig (`newRetryRig`, `blockNext`, `entered`, `cancelLifetime`, `events`, `ring`) and `adoptionTotalFor` (`flip_barrier_test.go`); cmd helpers `withKeyringDirs`, `newAdoptionEnabledCfg`, `syncLogBuffer`, `silentBuildLogger`; `discardSlog` (`schema_v7_test.go`).
- Produces: `waitAdoptionDrain(wait func(), notice time.Duration, logger *slog.Logger)` (renamed from `waitLogged`); `(*barrierRig).putServedAnchor(t, file string, body []byte)`; `internal/cache/snapshot_reads.go` holding `GetSuiteSnapshot`, `GetSnapshotMember`, `DisplacedSnapshotMembers`, `SnapshotMemberLookup` and `LookupSnapshotMember` unchanged; test helper `commitFor` (`adoption_scope_test.go`); the unlabelled counter `acu_freshness_unlogged_panics_total` (`freshnessUnloggedPanics`, `internal/freshness/metrics.go`).

- [ ] **Step 1: Write the failing tests**

Create `internal/cache/adoption_scope_test.go` (C-10, A-18):

```go
package cache

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
)

// commitFor adopts an inline snapshot of (scheme, host, suite) whose anchor
// is content and whose extra member path holds memberContent, creating the
// suite's row first; it returns the snapshot id.
func commitFor(t *testing.T, c *Cache, scheme, host, suite, content, path, memberContent string) int64 {
	t.Helper()
	ctx := context.Background()
	h := seedBlob(t, c, content)
	id, _, err := c.InsertTrackedCandidateForTest(ctx, SnapshotCandidate{CanonicalScheme: scheme, CanonicalHost: host,
		SuitePath: suite, InReleaseHash: &h})
	if err != nil {
		t.Fatal(err)
	}
	members := []SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: h, DeclaredSHA256: h}}
	if path != "" {
		m := seedBlob(t, c, memberContent)
		members = append(members, SnapshotMember{SnapshotID: id, Path: path, BlobHash: m, DeclaredSHA256: m})
	}
	if err := c.CommitAdoption(ctx, id, members, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	return id
}

// Deferred minor C-10: the adoption stamp (spec C8) reads only its own
// suite's prior snapshot and anchor rows — the same suite path on another
// host or scheme, adopted later or fetched in the future, does not move it.
func TestAdoptionStamp_IsScopedToTheSuitesSchemeAndHost(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, 1_800_000_000)()
	const suite = "/r/dists/x"
	other := commitFor(t, c, "http", "b", suite, "b's anchor", "", "")
	if _, err := c.db.Exec(`UPDATE suite_snapshot SET adopted_at = 1800009999 WHERE snapshot_id = ?`, other); err != nil {
		t.Fatal(err)
	}
	future := int64(1_800_005_000)
	for _, k := range []struct{ scheme, host string }{{"http", "b"}, {"https", "a"}} {
		h := seedBlob(t, c, "anchor of "+k.scheme+"://"+k.host)
		if err := c.PutURLPath(ctx, URLPath{CanonicalScheme: k.scheme, CanonicalHost: k.host, Path: suite + "/InRelease",
			BlobHash: &h, UpstreamURL: k.scheme + "://" + k.host + suite + "/InRelease", IsMetadata: true, LastFetchedAt: &future}); err != nil {
			t.Fatal(err)
		}
	}
	id := commitFor(t, c, "http", "a", suite, "a's anchor", "", "")
	snap, err := c.GetSuiteSnapshot(ctx, id)
	if err != nil || snap.AdoptedAt == nil || *snap.AdoptedAt != 1_800_000_000 {
		t.Fatalf("first adoption on host a stamped %d (%v), want now (1800000000): other hosts and schemes do not count",
			derefI64(snap.AdoptedAt), err)
	}
}

// Deferred minor C-10: displaced snapshots are ordered by adoption, not by
// id — an older candidate adopted later is the newer displaced snapshot —
// and a same-path suite on another host is not one of them.
func TestDisplacedSnapshotMembers_OrdersByAdoptionAndScopesToTheSuite(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, 1_800_000_000)()
	const suite, alias = "/r/dists/x", "main/by-hash/SHA256/shared"
	key := SuiteKey{CanonicalScheme: "http", CanonicalHost: "a", SuitePath: suite}
	if _, err := c.CreateSuite(ctx, SuiteSeed{SuiteKey: key, RequestedAt: 1}, SuiteCaps{}, nil); err != nil {
		t.Fatal(err)
	}
	cand := func(content, member string) (int64, []SnapshotMember) {
		t.Helper()
		h, m := seedBlob(t, c, content), seedBlob(t, c, member)
		id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "a",
			SuitePath: suite, InReleaseHash: &h})
		if err != nil {
			t.Fatal(err)
		}
		return id, []SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: h, DeclaredSHA256: h},
			{SnapshotID: id, Path: alias, BlobHash: m, DeclaredSHA256: m}}
	}
	older, olderMembers := cand("older candidate", "member of the older candidate")
	newer, newerMembers := cand("newer candidate", "member of the newer candidate")
	for _, x := range []struct {
		id      int64
		members []SnapshotMember
	}{{newer, newerMembers}, {older, olderMembers}} { // the older id is adopted last
		if err := c.CommitAdoption(ctx, x.id, x.members, nil, nil, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	current := commitFor(t, c, "http", "a", suite, "current anchor", "", "")
	commitFor(t, c, "http", "b", suite, "b's anchor", alias, "b's member")
	rows, err := c.DisplacedSnapshotMembers(ctx, "http", "a", suite, alias, current, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].SnapshotID != older || rows[1].SnapshotID != newer {
		t.Fatalf("displaced members = %+v, want snapshot %d (adopted last) then %d, and none of host b", rows, older, newer)
	}
	// The limit keeps the newest by adoption too.
	rows, err = c.DisplacedSnapshotMembers(ctx, "http", "a", suite, alias, current, 1)
	if err != nil || len(rows) != 1 || rows[0].SnapshotID != older {
		t.Fatalf("limit 1: displaced members = %+v, %v; want only snapshot %d", rows, err, older)
	}
}

// Deferred minor A-18: the package's cache helper logs nothing (test output
// stays pristine).
func TestOpenCache_LogsNothing(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	openCache(t)
	if buf.Len() != 0 {
		t.Fatalf("openCache logged:\n%s", buf.String())
	}
}
```

In `internal/freshness/scheduler_test.go`, before `TestSuiteDue` (B-40, B-43):

```go
// Deferred minor B-40: when the host whose turn is next is pruned, the
// turn passes to the first surviving host after it, not back to the first.
func TestSchedQueues_PruneHandsTheTurnToTheNextHost(t *testing.T) {
	q := newSchedQueues()
	one := int64(1)
	for _, h := range []string{"a", "b", "c", "d"} {
		q.enqueue(schedRow(h, "/1", &one, 0), false)
	}
	c := q.hosts["c"]
	for i := range c.classes {
		c.classes[i] = nil
	}
	delete(q.queued, suiteKey("http", "c", "/1"))
	q.next = 2 // c's turn
	q.prune()
	if got := q.order[q.next]; got != "d" || len(q.order) != 3 {
		t.Fatalf("after pruning c: order %v, turn %q; want d's turn", q.order, got)
	}
	q.next = 2 // d's turn, the last host
	q.hosts["d"].classes = [numSchedClasses][]schedItem{}
	delete(q.queued, suiteKey("http", "d", "/1"))
	q.prune()
	if got := q.order[q.next]; got != "a" {
		t.Fatalf("after pruning the last host: turn %q, want a's (wrapped)", got)
	}
}

// Deferred minor B-43: an ordinary scheduler check — neither a waiter's
// re-offer nor a pending change — honours the cooldown; a waiter's does not.
func TestCheckRecovered_OrdinaryCheckHonoursTheCooldown(t *testing.T) {
	fc := newFakeCache()
	recent := time.Now().Unix()
	seedFakeSuite(fc, "h", "/r/dists/x", &recent)
	up := &changedUpstream{body: []byte("x")}
	c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(1), Cooldown: time.Hour, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ref := SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}
	item := schedItem{ref: ref, key: suiteKey("http", "h", "/r/dists/x")}
	if rep := c.checkRecovered(context.Background(), item); rep.result != resultSkipped || up.calls.Load() != 0 {
		t.Fatalf("ordinary check inside the cooldown = %q with %d upstream calls; want skipped, 0", rep.result, up.calls.Load())
	}
	item.waiting = true
	if c.checkRecovered(context.Background(), item); up.calls.Load() != 1 {
		t.Fatalf("a waiter's re-check made %d upstream calls, want 1 (it bypasses the cooldown)", up.calls.Load())
	}
}
```

and before `TestRun_SurvivesATickPanic`'s comment (B-39):

```go
// Deferred minor B-39: with fewer hosts than workers, pop skips the hosts
// whose check is in flight: at most one check per host, so three hosts
// never have four checks in flight, and every suite is still checked.
func TestRun_InFlightHostsAreSkipped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCache()
		up := simtest.NewUpstream()
		var urls []string
		for h := range 3 {
			for s := range 3 {
				host, suite := fmt.Sprintf("h%d", h), fmt.Sprintf("/s%d/dists/x", s)
				seedFakeSuite(fc, host, suite, nil)
				url := "http://" + host + suite + "/InRelease"
				up.Set(url, simtest.After(time.Minute, simtest.NotModified()))
				urls = append(urls, url)
			}
		}
		c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute, Logger: discardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { c.Run(ctx); close(done) }()
		time.Sleep(time.Hour)
		cancel()
		<-done
		if total, perHost := up.MaxInFlight(); total != 3 || perHost != 1 {
			t.Fatalf("max in flight = %d overall, %d per host; want 3 (one per host) and 1", total, perHost)
		}
		for _, u := range urls {
			if len(up.Calls(u)) == 0 {
				t.Errorf("%s never checked", u)
			}
		}
	})
}
```

In `TestSchedQueues_BusyOfferIsHeldUntilTheNextWake` (B-42) the last check becomes `it.ref.SuitePath != "/1" || !it.waiting || !it.signed` with the message `"after unhold got %+v, want /1 waiting, still signed (deferred minor B-42)"`.

In `TestCheckWorker_SendsDoneWhenTheRecoverPathPanics` (B-44) the doc gains "That panic cannot be logged, so acu_freshness_unlogged_panics_total counts it (deferred minor B-44)."; after the `c.locks.Store(…)` line:

```go
	unlogged := func() float64 {
		var f float64
		if v := renderedMetric("acu_freshness_unlogged_panics_total"); v != "" {
			if _, err := fmt.Sscan(v, &f); err != nil {
				t.Fatal(err)
			}
		}
		return f
	}
	before := unlogged()
```

and after the `select`, before `cancel()`:

```go
	if got := unlogged() - before; got != 1 {
		t.Errorf("acu_freshness_unlogged_panics_total rose by %v, want 1 (deferred minor B-44)", got)
	}
```

In `internal/freshness/sim_sched_test.go`, `TestSimScheduler_PacingAfterClockSteps` (B-49) gets a fifth phase after the −2 h one, `phase("forward +2h (back to 0)", 0)`, and its doc ends "The steps: +30 d, -30 d, and spec §7's -2 h and +2 h (deferred minor B-49 added the +2 h step)."

In `internal/freshness/flip_barrier_test.go`, after `TestCheck_ShutdownAbandonIsNotAFailure` (C-20):

```go
// Deferred minor C-20: an attempt the shutdown ends before the flip
// barrier (here in its verification, when the lifetime context ends) is
// not a failure either: not counted, not in the admin ring, not recorded,
// and logged as INFO adoption_abandoned_at_shutdown instead of WARN
// adoption_run_failed.
func TestCheck_ShutdownBeforeTheBarrierIsNotAFailure(t *testing.T) {
	r := newRetryRig(t, 0)
	r.ver.blockNext()
	r.up.set(r.release("slow", 0))
	before := adoptionTotalFor(t, rigHost)
	go r.checker.Check(context.Background(), "http", rigHost, rigSuite)
	<-r.ver.entered
	r.cancelLifetime()
	r.checker.WaitForAdoptions()
	if got := adoptionTotalFor(t, rigHost) - before; got != 0 {
		t.Errorf("acu_adoption_total for the host rose by %v, want 0", got)
	}
	if evs := r.ring.Snapshot(); len(evs) != 0 {
		t.Errorf("admin ring %+v, want no entry", evs)
	}
	if n := len(r.events("adoption_run_failed")); n != 0 {
		t.Errorf("%d adoption_run_failed lines, want 0:\n%s", n, r.logs.String())
	}
	if evs := r.events("adoption_abandoned_at_shutdown"); len(evs) != 1 || evs[0]["level"] != "INFO" {
		t.Errorf("adoption_abandoned_at_shutdown lines %v, want one at INFO", evs)
	}
	if row := r.row(); row.AdoptionFailCount != 0 {
		t.Errorf("shutdown recorded a failure: %+v", row)
	}
}
```

Create `cmd/apt-cacher-ultra/shutdown_drain_test.go` (B-64's test moved and renamed, C-30, C-28):

```go
package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// Deferred minor B-64: a shutdown that waits on a running adoption commit
// says so once the wait outlasts the notice delay, and still returns
// exactly when the drain ends. (Moved from flip_wiring_test.go: deferred
// minor C-34.)
func TestWaitAdoptionDrain_ExplainsASlowDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs := &syncLogBuffer{}
		logger := slog.New(slog.NewJSONHandler(logs, nil))
		start := time.Now()
		waitAdoptionDrain(func() { time.Sleep(time.Second) }, adoptionDrainNotice, logger)
		if strings.Contains(logs.String(), "shutdown_waiting_for_adoptions") {
			t.Fatalf("a 1s drain was logged:\n%s", logs.String())
		}
		waitAdoptionDrain(func() { time.Sleep(time.Minute) }, adoptionDrainNotice, logger)
		if n := strings.Count(logs.String(), `"msg":"shutdown_waiting_for_adoptions"`); n != 1 {
			t.Fatalf("a 1m drain logged %d notices, want 1:\n%s", n, logs.String())
		}
		if d := time.Since(start); d != time.Second+time.Minute {
			t.Fatalf("waitAdoptionDrain returned after %v in all, want when the drains ended (1m1s)", d)
		}
	})
}

// Deferred minor C-30: a panicking drain wait is logged with its stack,
// and the shutdown carries on.
func TestWaitAdoptionDrain_LogsAPanicWithItsStack(t *testing.T) {
	logs := &syncLogBuffer{}
	waitAdoptionDrain(func() { panic("drain bug") }, time.Hour, slog.New(slog.NewJSONHandler(logs, nil)))
	out := logs.String()
	if !strings.Contains(out, `"msg":"shutdown wait panicked"`) || !strings.Contains(out, `"panic":"drain bug"`) ||
		!strings.Contains(out, `"stack":"goroutine`) {
		t.Fatalf("panic log = %s; want the panic and its stack", out)
	}
}

// Deferred minor C-28: the daemon starts and stops with adoption on and
// the flip barrier enabled (the gate closes at the signal, before the
// listeners drain). No daemon test reaches a flip in the barrier.
func TestServeListeners_ShutsDownWithTheBarrierOn(t *testing.T) {
	withKeyringDirs(t, []string{t.TempDir()})
	cfg := newAdoptionEnabledCfg(t, false)
	cfg.Adoption.FlipQuietPeriod.Duration = 10 * time.Second
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveListeners(ctx, cfg, silentBuildLogger(), ln, nil, nil, nil) }()
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Get("http://" + ln.Addr().String() + "/probe")
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("daemon not serving within 15s: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveListeners: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the daemon did not stop within 15s with the barrier on")
	}
}
```

In `cmd/apt-cacher-ultra/flip_wiring_test.go`: delete `TestWaitLogged_ExplainsASlowAdoptionDrain` (moved above) and move `TestServeListeners_LegacyAuditErrorHasOnePrefix` unchanged to the end of `legacy_audit_wiring_test.go`, its doc gaining `(Moved from flip_wiring_test.go: deferred minor C-34.)`; the file's imports become `errors`, `log/slog`, `strconv`, `strings`, `testing`, `time`, `internal/flipgate`, `internal/proxy`. C-31: `TestBuildAdopter_BuildsTheFlipBarrier`'s last check becomes `!errors.Is(err, flipgate.ErrQuietOutOfRange)` with `"buildAdopter(16s) = %v, want flipgate.ErrQuietOutOfRange (deferred minor C-31)"`. C-32: `TestHandlerConfig_SharesTheFlipGateAndKeepDisplaced` sets `cfg.GC.KeepDisplaced = 7` and checks 7; its doc's last sentence becomes "With no adopter (main builds none when adoption is disabled) there is no gate. gc.keep_displaced is wired whether or not GC runs (minimalCfg leaves gc.enabled false); 7 is not its default (3), so the value seen is the configured one (deferred minor C-32)."; the nil-adopter failure says `"no adopter, yet the handler has a flip barrier"`.

- [ ] **Step 2: Run to verify failure**

Run: `go vet ./cmd/apt-cacher-ultra/ && go test ./internal/cache/ ./internal/freshness/ -run 'TestOpenCache_LogsNothing|TestAdoptionStamp_|TestDisplacedSnapshotMembers_|TestSchedQueues_|TestCheckRecovered_|TestRun_InFlightHostsAreSkipped|TestCheckWorker_|TestCheck_ShutdownBeforeTheBarrier|TestSimScheduler_PacingAfterClockSteps'`
Expected: cmd: `undefined: waitAdoptionDrain`; `TestOpenCache_LogsNothing` FAILS (`openCache logged:` the migration lines); `TestSchedQueues_PruneHandsTheTurnToTheNextHost` FAILS (`order [a b d], turn "a"; want d's turn`); `TestCheckWorker_SendsDoneWhenTheRecoverPathPanics` FAILS (`rose by 0, want 1`); `TestCheck_ShutdownBeforeTheBarrierIsNotAFailure` FAILS (`acu_adoption_total for the host rose by 1, want 0`, then the ring entry and the `adoption_run_failed` line). The C-10, B-39, B-42, B-43 and B-49 tests pass on the current code: they pin behaviour that had no test (the mutants of the first four are in Step 5; the +2 h phase checks the burst bound the other phases pin).

- [ ] **Step 3: Implement**

`internal/freshness/scheduler.go` — B-40, `prune` becomes:

```go
// prune drops hosts with nothing queued and nothing in flight. The turn
// stays with the host whose turn it was, or, when that host is dropped,
// passes to the first surviving host after it (deferred minor B-40).
func (q *schedQueues) prune() {
	if len(q.order) == 0 {
		return
	}
	nextIdx := q.next % len(q.order)
	kept := q.order[:0]
	newNext := -1
	for i, host := range q.order {
		h := q.hosts[host]
		if h.inFlight || !h.empty() {
			if newNext < 0 && i >= nextIdx {
				newNext = len(kept)
			}
			kept = append(kept, host)
			continue
		}
		delete(q.hosts, host)
	}
	q.order = kept
	q.next = max(newNext, 0) // none after it: wrap to the first
}
```

B-41: in `tick`, collect `type dueSuite struct { key string; s cache.SuiteFreshness }` values with the key built once (`suiteKey(…)`), sort by `due[i].key < due[j].key`, and enqueue `d.s` (the queue bound unchanged); the FIFO comment gains "Keys are built once per suite, not per comparison (deferred minor B-41)."

`internal/freshness/sim_sched_test.go`, `TestSimScheduler_LatencyBoundEightHosts`: B-48 adds `rand: func() float64 { return 0.5 }}) // spec §7 fixed seeds (deferred minor B-48)` to its `startSched` config; B-47 makes the goroutine limit `baseG + checkWorkers + 1 + 2 + 8` (comment: "… (8, as in the capacity simulation: deferred minor B-47)").

`internal/freshness/freshness.go`, C-19: the over-long line of `spawnAdoption`'s AIDEV-NOTE is wrapped to `// The host slot is freed earlier, when the member pipeline ends; the` / `// global slot only here, after the flip barrier and the commit (ruling` / `// R-C1).`

C-20: `spawnAdoption`'s abandon test becomes

```go
		if errors.Is(err, flipgate.ErrAborted) || (err != nil && c.lifetimeCtx.Err() != nil) {
```

— a verification or member fetch cut short by shutdown and runShared's shutdown-before-the-commit check all end with the lifetime context (`stopErr` reads it), so `freshness/adoption.go` needs no change. The comment above it gains "So does any error once the lifetime context has ended: a verification or member fetch cut short, or runShared's shutdown-before-the-commit check (deferred minor C-20).", the function's AIDEV-NOTE now ends "An attempt ended by shutdown — a flip abandoned in the barrier (design note 18), or any error once the lifetime context has ended (deferred minor C-20) — is never counted, logged or recorded as a failure (INFO adoption_abandoned_at_shutdown).", and `metrics.go`'s inventory note says "An adoption ended by shutdown is not an adoption outcome".

`internal/freshness/metrics.go`, B-44: after `freshnessWaitingForSlot`:

```go
	// freshnessUnloggedPanics counts scheduled checks whose panic could
	// not be logged: the panic report in checkRecovered's recover panicked
	// too, and checkWorker's last-resort recover caught it (deferred minor
	// B-44). Any increase is a bug.
	freshnessUnloggedPanics = metrics.NewCounter(
		"acu_freshness_unlogged_panics_total",
		"Scheduled freshness checks whose panic could not be logged because reporting it panicked too (a bug).",
	)
```

and the inventory AIDEV-NOTE lists it ("acu_freshness_unlogged_panics_total (deferred minor B-44, unlabelled counter)"). In `scheduler.go`, `checkWorker`'s inner recover counts instead of discarding — never through the logger, which is what panicked:

```go
				defer func() {
					if recover() != nil {
						freshnessUnloggedPanics.Inc()
					}
				}()
```

and its AIDEV-NOTE ends "so the item just reports failed and acu_freshness_unlogged_panics_total counts it (deferred minor B-44)."

`internal/freshness/flip_barrier_test.go`, C-18: `TestAdopter_ShutdownInTheBarrierCommitsNothing` gains the duplicate test's two checks (no `ErrAdoptionDBFailed` / `db_failed` classification; no `adoption: commit failed` in the logs; comment "merged from a duplicate test: deferred minor C-18"), and `TestAdopter_ShutdownInTheBarrierIsNotACommitFailure` is deleted; `putServedAnchor(t, file string, body []byte)` writes `rigSuite + "/" + file` with `UpstreamURL: "http://" + rigHost + path`, its two callers pass `"InRelease"`, and `TestAdopter_FirstAdoptionOfTheServedAnchorSkipsTheBarrier` (`"InRelease"`, `tc.served(rel)`), `TestAdopter_DetachedFirstAdoptionComparesTheRelease` (`"Release"`) and `TestAdopter_FirstAdoptionSkipStillCommits` (`"InRelease"` or `"Release"`; declare `var err error`) call it instead of writing the blob and the url_path row by hand.

`internal/cache/cache_test.go`, A-18: `openCache` opens with `discardSlog()` (`// deferred minor A-18: no log noise`).

`internal/cache/adoption.go`: C-14 moves `now := nowUnix()` and the `adoptionStampTx` block (with its "Spec C8 (monotonic Last-Modified)" comment) above Step 3a's comment, whose `(queries.go:45-75)` becomes `(putURLPathSQL, queries.go)` and which ends "(deferred minor C-14 moved the stamp above this comment)". C-15 moves `GetSuiteSnapshot`, `GetSnapshotMember`, `DisplacedSnapshotMembers`, the `SnapshotMemberLookup` type and `LookupSnapshotMember`, doc comments included and otherwise unchanged, to a new `internal/cache/snapshot_reads.go` (imports `context`, `database/sql`, `errors`, `fmt`; a file comment "Read-side snapshot helpers (moved from adoption.go: deferred minor C-15). Reads use the connection pool freely.").

`internal/handler/sim_flip_test.go`, C-27: `serveDetached`'s doc ends "The ledger stamps the anchor's arrival after Release.gpg, while the gate's quiet clock started at Release's completion: the window the ledger checks is stricter, never laxer, than the plan's "the two instants coincide" (deferred minor C-27)."

`cmd/apt-cacher-ultra/main.go`: C-34 renames `waitLogged` to `waitAdoptionDrain` (its call site and the gate comment's reference too) and its doc becomes "waitAdoptionDrain runs wait (the adoption drain) and, once it has run for notice, logs shutdown_waiting_for_adoptions …; a panicking wait is logged with its stack (deferred minor C-30) and the shutdown carries on." with the C-33 AIDEV-NOTE: "the 5-minute figure here, in the notice's detail, in the drain comment above and in docs/log-fields.md and docs/configuration.md is freshness's unexported commitTimeout (internal/freshness/adoption.go): change them together (deferred minor C-33)."; C-30: its recover logs `logger.Error("shutdown wait panicked", "panic", r, "stack", string(debug.Stack()))`; C-28: above `adopter.FlipGate().Close()` add "AIDEV-NOTE: the order is load-bearing (deferred minor C-28): before the listeners shut down, so held requests are answered while they still can be, and before the drain, so the drain never waits on a barrier. TestServeListeners_ShutsDownWithTheBarrierOn is a smoke test only: no daemon test reaches a flip in the barrier."

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/cache/ ./internal/freshness/ ./internal/handler/ ./cmd/... && go test -race -count=5 -v -run 'TestSimScheduler_LatencyBoundEightHosts|TestRun_InFlightHostsAreSkipped|TestSimScheduler_PacingAfterClockSteps' ./internal/freshness/`
Expected: PASS (the latency simulation within the tighter goroutine limit on every run; the pacing simulation, now five phases, about 3 s under `-race`, up from 2.4 s: under the 5 s budget).

- [ ] **Step 5: Mutation check (do not commit)**

One at a time, reverting each: (a) `q.next = 0` in `prune` → `TestSchedQueues_PruneHandsTheTurnToTheNextHost` FAILS; (b) `pop` no longer skips in-flight hosts → `TestRun_InFlightHostsAreSkipped` FAILS (`4 overall, 2 per host`); (c) the busy arm re-parks with `signed = false` → `TestSchedQueues_BusyOfferIsHeldUntilTheNextWake` FAILS; (d) `checkRecovered` always bypasses the cooldown → `TestCheckRecovered_OrdinaryCheckHonoursTheCooldown` FAILS (`"changed" with 1 upstream calls`); (e) `adoptionStampTx`'s prior read ignores the host → `TestAdoptionStamp_IsScopedToTheSuitesSchemeAndHost` FAILS (`stamped 1800010000`), (f) its anchor read ignores the scheme → the same test FAILS (`stamped 1800005001`); (g) `DisplacedSnapshotMembers` orders by `snapshot_id` in the outer query, (h) in the inner one, or (i) ignores the host → `TestDisplacedSnapshotMembers_OrdersByAdoptionAndScopesToTheSuite` FAILS; (j) `openCache` with a nil logger → `TestOpenCache_LogsNothing` FAILS; (k) the shutdown-wait log without its stack → `TestWaitAdoptionDrain_LogsAPanicWithItsStack` FAILS; (l) the last-resort recover counts nothing → `TestCheckWorker_SendsDoneWhenTheRecoverPathPanics` FAILS (`rose by 0, want 1`); (m) `spawnAdoption` abandons only on `flipgate.ErrAborted` → `TestCheck_ShutdownBeforeTheBarrierIsNotAFailure` FAILS (`rose by 1, want 0`); (n) it abandons on any error → the `TestAdoptionFailure_` record tests FAIL (`record = count 0`).

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/freshness/scheduler.go internal/freshness/metrics.go internal/freshness/scheduler_test.go internal/freshness/sim_sched_test.go internal/freshness/freshness.go internal/freshness/flip_barrier_test.go internal/cache/adoption.go internal/cache/snapshot_reads.go internal/cache/adoption_scope_test.go internal/handler/sim_flip_test.go cmd/apt-cacher-ultra/
golangci-lint run ./...
git add internal/freshness/ internal/cache/adoption.go internal/cache/snapshot_reads.go internal/cache/adoption_scope_test.go internal/cache/cache_test.go internal/handler/sim_flip_test.go cmd/apt-cacher-ultra/
git commit -m "Close the deferred minors in the files phase D touches"
```

Expected: `gofmt -l` prints only `cmd/apt-cacher-ultra/ca_print.go` (a drift file inside `cmd/apt-cacher-ultra/`).

---

### Task 13: Documentation, spec amendments, overview, deferred files, final verification

Spec §8 (operator docs for C3, C11's `freshness.max_suites` and the events and metrics this phase adds), the rulings R-D1–R-D5, deviations 1 and 2 and the design notes the spec should state, written into the spec as revision 7; the overview's phase D bookkeeping and the carry-forwards D hands to E, F, G and H; the deferred files (every item this plan took is deleted, three are narrowed); then the full verification. No Go code changes in this task.

**Files:**
- Modify: `docs/configuration.md`, `docs/log-fields.md`, `docs/superpowers/specs/2026-09-27-flat-repositories-design.md`, `docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md`, `docs/superpowers/plans/2026-09-28-flat-repositories-A-deferred.md`, `…-B-deferred.md`, `…-C-deferred.md`

- [ ] **Step 1: Operator documentation**

`docs/configuration.md`, `[freshness]` table, after `max_concurrent_adoptions`:

```markdown
| `max_suites` | integer | `5000` | Most suites tracked (checked, adopted, shown on the status page) at once, and at most a tenth of it per upstream host (`http` and `https` counted together). At least `10`; an explicit `0` is an error. See *Tracked suites*. |
```

After the table's closing paragraph, a subsection `### Tracked suites` saying: a suite is tracked once a client fetches its `InRelease` — or, with adoption enabled, its `Release` — and the bytes are what they claim (an `InRelease` starts with the clearsign line apt requires; a `Release` has a hash block): an HTML error page or a stub never tracks a suite, and `Release.gpg` never does on its own. A suite whose `InRelease` (with adoption enabled, also `Release`) an earlier release cached is tracked on its first cache hit. With adoption disabled only `InRelease` tracks, as before, except that a plain-text "InRelease" (no clearsign line; apt rejects it too) no longer does. At most `max_suites` suites are tracked and at most `max_suites / 10` per host; at the total cap, the least recently requested suite that has no adopted snapshot and has not been requested for 24 hours makes room (at most eight are tried, a suite being checked or adopted is skipped, INFO `suite_evicted` names it). A suite refused a row is still served — it just does not refresh — and is not retried for 10 minutes: each refusal that reaches the database (at most one per suite per 10 minutes) is counted in `acu_suite_seed_rejected_total{host}`, the gauge `acu_suites_seed_rejected` shows how many suites are waiting out those 10 minutes, and WARN `suite_seed_rejected` (at most one a minute, with how many were suppressed) names the suite. Raise `max_suites` for very large Artifactory, aptly or Nexus hosts; lowering it below the number of tracked suites refuses new suites until the count falls below it. Serving a tracked suite's `InRelease`, `Release` or `Release.gpg`, or a `.deb` its current snapshot vouches for, records that the suite is in use (at most one write per suite per hour). A by-hash request with no cached copy under its own path is answered from an identical cached file only for a tracked suite; otherwise it is fetched from upstream. In *Upgrading an existing cache*, add: "Suites whose `InRelease` an earlier release cached but never tracked are tracked, with their signed-seen record, on their first cache hit."

`docs/log-fields.md`:
- The metrics paragraph, after the retained by-hash sentence: "Suite rows (spec C3) add `acu_suite_seed_rejected_total{host}` (refusals at the `freshness.max_suites` caps that reached the database: at most one per suite per 10 minutes) and the unlabelled gauge `acu_suites_seed_rejected` (suites refused within the last 10 minutes). The scheduler adds the unlabelled counter `acu_freshness_unlogged_panics_total` (scheduled checks whose panic could not be logged because reporting it panicked too: a bug, please report it)."
- The adoption outcomes paragraph: "A flip abandoned at shutdown is not an outcome" becomes "An adoption ended by shutdown, in the flip barrier or before it, is not an outcome".
- The operational events intro gains "suite rows (cap refusals and evictions)".
- New operational events rows: `suite_seed_rejected` | WARN | `canonical_host`, `suite_path`, `cap` (`global_cap`, `host_cap`), `max_suites`, `suppressed`, `detail` | A suite was refused a row by `freshness.max_suites`: it is served but will not refresh; retried after 10 minutes. At most one line a minute; `suppressed` counts the refusals since the last line. — `suite_evicted` | INFO | `canonical_host`, `suite_path` (the evicted suite), `for_host`, `for_suite` | At the global cap, an idle suite with no adopted snapshot made room for a new one. — `handler_goroutine_panicked` | ERROR | `where` (`touch`), `canonical_host`, `path`, `panic`, `stack` (first occurrence per `where` per process only) | A panic in the handler's background touch goroutine was recovered (a bug, please report it). — `suite onboarding failed` | WARN | `err`, `canonical_host`, `suite_path` | A cache hit on an untracked suite's anchor could not create its row; the anchor is served and the next hit retries. — `suite signed-seen mark failed` | WARN | `err`, `canonical_host`, `suite_path` | After creating a suite's row, its other cached signed anchors could not be marked; the startup pass marks them at the next start. — `snapshot_member_blob_missing` | WARN | `canonical_host`, `path`, `snapshot_id`, `blob_hash`, `snapshot_displaced` (present for a displaced snapshot's member) | A snapshot member's pool file is gone: a current member is re-fetched and checked against its declared hash; a displaced one (retained by-hash) answers 404.
- The `adoption_abandoned_at_shutdown` row's meaning becomes: "An adoption was abandoned at shutdown — waiting in the flip barrier (or about to skip it) when the daemon closed the barrier at the signal, or in any step before its commit once the daemon's lifetime context ended: nothing was committed or recorded, the change is adopted after the restart." (the "Not a failure: …" sentence unchanged).
- `shutdown wait panicked` gains the field `stack`; the `apt-cacher-ultra starting` row adds `freshness_max_suites` ("the configured `freshness.max_suites`").

- [ ] **Step 2: Spec revision 7**

In `docs/superpowers/specs/2026-09-27-flat-repositories-design.md`:
- The Date line starts `revision 7, 2026-10-18: phase D plan rulings — adoption accepts exactly apt's first `InRelease` line, and a marker line with trailing blanks decodes; cap eviction holds the evictee's mutex through the checker, tries at most eight candidates, runs only at exactly the cap and deletes through the compare-and-delete the reaper reuses; the rejection cache holds at most 4,096 suites; `suite_seed_rejected` is rate-limited to one line a minute; the C3 metric names; the hit-path invariant covers tracked suites, and a row creation on a hit marks the suite's signed anchors; the T1 on a creating miss lands with phase E; an attempt a shutdown ends before the barrier is abandoned like one in it;` followed by the existing revision 6 text.
- C3 "Single creation choke point": "(via the reaper path, under its mutex)" becomes "(through the compare-and-delete helper phase F's reaper reuses, holding the evictee's suite mutex)"; after "(bounded map)" insert "— at most 4,096 suites; when it is full, expired entries are dropped and, if it is still full, it is cleared —"; after "per request." add: "The WARN is rate-limited to one line a minute (field `suppressed`); the counter is `acu_suite_seed_rejected_total{host}` (refusals that reached the writer) and the unlabelled gauge `acu_suites_seed_rejected` the cache's size. The per-host cap counts a host's `http` and `https` suites together. Eviction tries at most eight candidates, least recently requested first, skipping a suite whose mutex is held (the checker is the cache's guard; the writer runs no foreign code); it runs only at exactly the global cap — above it, after `max_suites` was lowered, creation is refused until the reaper (C9) shrinks the table — leaves the evictee's `url_path` rows to `url_path` GC and logs INFO `suite_evicted`."
- C3, adopter-wired creation: after "the handler fires T1 once the response is written" add "(from phase E, §9 "T1-on-seed"; until then the suite's first check is the next hit's T1 or the scheduler's never-checked class)". After the Phase 1 bullet ("— today's seed, byte for byte, and no T1.") add: "A creation by a hit (legacy onboarding) leaves the check stamps and validators NULL in both modes; a refresh miss of a tracked suite leaves its row as it is (create-only). A plain-text "InRelease" fails the sniff, so it seeds no row in either mode."
- C3 "Client interest": "is written only by `TouchSuite` from client-serving paths" becomes "is set by the creating request (`CreateSuite`) and otherwise written only by `TouchSuite` from client-serving paths".
- C3 marker writers, handler entry: "The hit path writes no marker: …" becomes "Serving a tracked suite's anchor writes no marker: …" (the rest unchanged), followed by: "A row creation on a hit — legacy onboarding, or a suite once refused at a cap — marks the anchor in hand in the creating transaction and the suite's other cached signed anchors in one follow-up writer operation (a failure is logged and healed by the startup pass at the next start): an anchor cached while its suite had no row earned no marker, since markers are written only for suites with rows (phase D plan, deviation 2)."
- C5 "Inline verification accepts only clearsigned envelopes": add "A body that carries the marker but whose first line is not exactly the marker — apt's rule: trailing `\r`, space and tab ignored, leading whitespace or a leading blank line refused — is `malformed_clearsign` as well, and a marker line with trailing blanks, which apt accepts and go-crypto's decoder alone does not, is decoded (phase D, ruling R-D2)."
- C7 "Failure record": "a shutdown context cancellation is not recorded" becomes "an attempt a shutdown ends — in the flip barrier or before it — is not recorded, not counted as an adoption outcome and logged at INFO only (`adoption_abandoned_at_shutdown`; phase D, deferred minor C-20)".
- §7, C3 tests: the hit-path invariant reads "every `url_path` `InRelease` or `Release.gpg` row of a tracked suite whose blob passes the sniff has a marker for its base (markers exist only for tracked suites; phase D plan, deviation 1)".
- §7 "Fixtures": "(about 18 files), in phase D" becomes "in phase D: the ones whose outcome depends on a sniff-passing `InRelease` (phase D plan, design note 22)".
- C11: `adoption_abandoned_at_shutdown`'s gloss becomes "(an adoption ended by shutdown, in the flip barrier or before it: INFO, never counted as an adoption outcome)"; the events gain `suite_evicted` and `handler_goroutine_panicked`; after "`acu_flip_barrier_active` (flips in the C8 barrier now)" add "and the C3 pair `acu_suite_seed_rejected_total{host}` (counter) and `acu_suites_seed_rejected` (gauge), plus `acu_freshness_unlogged_panics_total`"; the constant "cap-rejection cache 10 min" becomes "cap-rejection cache 10 min (at most 4,096 suites)".

- [ ] **Step 3: Overview**

In `docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md`:
- The header's spec reference becomes "(revision 7)"; the deferred-minor line's "(IDs C-1…)" becomes "(IDs C-1…C-40)".
- Phase table: D's status becomes "in progress (plan written 2026-10-18)", as phase C's docs task set its own; it becomes "done (commit …)" after the final review.
- "Later phases add", the D line: after its first sentence add the names as built — `cache.CreateSuite(ctx, SuiteSeed, SuiteCaps, SuiteGuard) (CreateResult, error)`, `cache.StoreFetched(ctx, FetchedRow, SuiteCaps, SuiteGuard) (FetchedResult, error)`, `cache.SuiteExists`, `cache.TouchSuite` / `TouchedSuites`, `cache.SuiteGuard` (implemented by `(*freshness.Checker).HoldSuite`), unexported `deleteSuiteTx(ctx, tx, key, wantCurrent *int64, cutoff, now)` (F's `ReapIdleSuite` reuses it), `freshness.Config.MaxSuites`, `config.FreshnessConfig.MaxSuites`, `aptpath.ClassifySuiteFile` / `SuiteFile`, `aptpath.DecodableClearsign`, `simtest.ClearsignedFixture`, `(*freshness.Checker).LockCount`; "→ write body → T1, today's seed-before-serve order" gains "(the T1 on a creating miss: phase E, design note 2)"; "nowhere else in the handler — no hit-path write" gains "(except that a row creation on a hit marks the suite's cached signed anchors: phase D plan, deviation 2)"; "every sniff-passing anchor row has a marker" becomes "every sniff-passing anchor row of a tracked suite has a marker (deviation 1)".
- Carry-forwards: delete the "**D:**" block, as phase C deleted its own — its four items are done (the one-insert-site convergence: Task 4; the `0xC2` note: Task 9's AIDEV-NOTE in `runFetch`; apt's first line: ruling R-D2, Task 2; the row-creating serve's registration: Task 9, with the scale test's gate-size check in Task 11). The section's intro gains "or *(D)* (the phase D plan, 2026-10-18)". Add:
  - under **E:** *(D)* Fire T1 once a miss creates an adopter-wired row (spec C3; phase D plan, design note 2); until then the next hit's T1 or the never-checked class checks it.
  - under **F:** *(D)* `ReapIdleSuite` deletes through `deleteSuiteTx` (`internal/cache/suite_rows.go`), passing the current snapshot it read as `wantCurrent`, and purges in-memory state as cap eviction does: the checker's lock-map entry and the flip-gate entry (hold the suite through the `SuiteGuard`; its `release(true)` after the commit drops both) and the cache's touch entry (`forgetTouch`). Cap eviction stops above the cap, so after `max_suites` is lowered only the reaper shrinks the table.
  - under **G:** *(D)* `ClassifySuiteFile` already returns `FileFlatIndex` for a root `Packages*`/`Sources*` of a suite without a `dists` segment, and the handler already sniffs, creates rows for and touches it; G's grammar makes those paths reachable (spec C3; phase D plan, ruling R-D3), so G adds no handler code for them — its tests add the flat cases to the miss-path, onboarding and touch tests.
  - under **H:** *(D)* The two prose WARN names `suite onboarding failed` and `suite signed-seen mark failed` (documented as they are) may become snake_case in H's documentation sweep.

- [ ] **Step 4: Deferred files**

Delete the entries this plan resolved: A-17, A-18, A-22, A-23, A-24, A-25, A-30, A-32, A-33, A-34, A-35, A-38, A-50, A-51, A-52, A-53 (`…-A-deferred.md`); B-1, B-2, B-3, B-4, B-5, B-8 (moot: Step 6 is UPDATE-only), B-22 (already true), B-39, B-40, B-41, B-42, B-43, B-44, B-47, B-48, B-49, B-50 (already tight) (`…-B-deferred.md`); C-10, C-14, C-15, C-18, C-20, C-22, C-23, C-24, C-25, C-26, C-27, C-28, C-30, C-31, C-32, C-33, C-34, C-35 (`…-C-deferred.md`). Narrow three: A-37 to "the freshness side — `Checker.reportPanic` (`freshness.go`) and the heartbeat's recover (`freshness/adoption.go`) — still has its own copies; phase D gave the handler's two recovers one helper, `logGoroutinePanic`"; C-19 to its file-split half; C-21 to its context fast path (the `EXISTS` read landed in phase D, Task 10). Every other item stays unchanged; Task 12's intro lists the ones in files this phase touches and why each stays.

- [ ] **Step 5: Final verification**

Run:

```bash
go test -race ./...
golangci-lint run ./...
gofmt -l .
grep -rn 'AIDEV-TODO(phase D)' --include=*.go .
grep -rn 'PutSuiteFreshness' --include=*.go .
go test -race -count=1 -v -run 'TestSuiteFreshnessHasOneInsertSite|TestOnlyTheHandlerCreatesSuiteRows|TestServeHTTP_AdoptionDisabledSeedParity|TestServeHTTP_MintedSuitesAtScale|TestServeHTTP_OnboardsADistsSuiteFromAV6Cache|TestHitPathInvariant_' ./internal/cache/ ./internal/handler/
```

Expected: PASS everywhere; lint `0 issues`; `gofmt -l` lists exactly the five drift files; no `AIDEV-TODO(phase D)`; `PutSuiteFreshness` appears only in `UpdateSuiteFreshness`'s "Carried over from PutSuiteFreshness" note; the DoD tests pass, the scale test in under 5 s under `-race`.

- [ ] **Step 6: Commit**

```bash
git add docs/
git commit -m "Document suite caps and tracking, record phase D's rulings in the spec, update the overview and deferred files"
```
