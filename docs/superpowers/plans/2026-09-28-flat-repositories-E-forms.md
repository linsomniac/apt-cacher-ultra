# Flat repositories — Phase E: Eager adoption and anchor forms — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make every suite with recent client interest adopt on its first check in whichever anchor form its upstream publishes — clearsigned `InRelease`, detached `Release` + `Release.gpg`, or (safeguarded, never as a downgrade) unsigned `Release` — move between the signed forms when an anchor disappears, read SHA512-only Release files, and let strict mode wait briefly for an adoption in flight before refusing a `.deb`.

**Architecture:** The freshness check becomes one dispatch over the suite's current anchor form with one shared tail. Every anchor response is classified once (`present`, `absent`, `unknown`); a suite without a current snapshot runs spec C4's probe tree when it has recent client interest and a client-created anchor row, and a dormant one costs no upstream request. The adopter gains the unsigned form (`adoptionFormUnsigned`, `RunUnsigned`): one never-downgrade rule (signed-seen markers of the base under either scheme, or any kept signed snapshot of the base or its `/.` twin) checked before a slot is taken, before any download and inside `CommitAdoption`'s transaction, a member cap, no `package_hash` rows, a re-run of the cached signed-anchor mark before the commit, and a refusal recorded as a state. A probe that finds a signature marks the suite's base, so a signature only the checker saw is signed history too. A failure or refusal record paces its suite: a deferred never-adopted suite is probed at its retry time or once per refresh interval, an adopted `anchor_absent` suite at its retry times only, and client checks in between ask nothing. `ParseRelease` merges hash blocks per member; one verifier checks every declared SHA-256/SHA-512 hash and yields the aliases for every declared algorithm. The checker tracks signed adoptions in flight per host; the handler waits for them (at most 15 s and never past 25 s into the request, 64 waiters per host) before refusing an unvouched `.deb`, and fires T1 after a miss that created its suite's row.

**Tech Stack:** Go 1.25 (toolchain 1.26), modernc SQLite, `testing/synctest`, `github.com/ProtonMail/go-crypto` (clearsign decode only).

**Spec:** `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (revision 7; Task 15 records this plan's rulings, design notes and deviations as revision 8) — C4's probe tree, classification, form transitions, `anchor_absent`, eager first adoption with the 7-day interest window and T1-on-seed; C5's unsigned form with every D6 safeguard, never-downgrade at both points (including the in-transaction recheck), the SHA512 merge, the shared verify helper and the aliases; C10's bounded in-flight wait (D10); §9's phase E row. Interactions read: C3 (markers, interest, rows), C7 (refusal states, the retry gate), C8 (barrier; retained by-hash serves SHA512 aliases since phase C), C9 (what phase F's reaper needs), C11 (keys, events, metrics), §6.1 (quarantined plain-text suites adopt unsigned through `Release`), §7 (E's tests), §11. Overview: `docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md` — Global Constraints, Deliberate deviations, Review Focus items 1, 3 and 6, the "E:" line under "Later phases add" and every item of the "E:" carry-forwards block (from A, B, C and D). Deferred minors: `…-A-deferred.md`, `…-B-deferred.md`, `…-C-deferred.md`, `…-D-deferred.md` (IDs cited per task; Task 15's intro lists the ones taken and the ones checked and left, and Task 15 deletes the taken ones).

**Plan reviews (2026-10-19)**, revised in this version under the controller's rulings PE-1–PE-8 (below):
- An executability review applied the draft task by task. It reported defects D1–D10 and O1, killed 132 of 133 plan mutants, and ran 30 extra mutants; 14 proposed tests kill its surviving X-mutants.
- A design review reported I-1–I-6, M-1–M-20, its verdicts and eight underspecified items.

Each task says what the revision changed in it. Every task was re-verified in code: build, tests, each expected failure, each mutation step, and a literal replay of the task's blocks.

## Global Constraints

Everything in the overview's Global Constraints applies. In particular:

- No schema change: v7 holds every column and table this phase writes (`suite_snapshot.anchor_form` already permits `'unsigned'`; `suite_signed_seen`; the `adoption_fail_*` record). Never edit an entry of `migrations`.
- Production SQL never uses SQLite clock functions; persisted stamps come from `wallclock` (the cache's `nowUnix`, the checker's and adopter's `now`). Durations, deadlines, the C10 wait and in-memory timing use monotonic `time.Now`/`time.Since`.
- `internal/cache` never imports `internal/freshness` or `internal/proxy`. The writer goroutine never runs foreign code or pool I/O: R-E1's sniffs read pool files on the adoption goroutine before its mark is submitted, as phase D's handler does (ruling PD-5).
- Adoption-disabled (Phase 1) behaviour stays as today: with no adopter wired the check runs the phase D algorithm (conditional `InRelease`, detached fallback on 404 for a never-adopted suite, no sniff, no probe tree), now through the shared tail (R-E3); no T1 fires on a miss (`TestServeHTTP_AdoptionDisabledSeedParity` stays green).
- Constants (spec C11), not knobs: interest window 7 d; anchor-absent and unsigned confirmation gap `max(cooldown, 5 min)`, 1 h after a 401/403 on a signature file; unsigned member cap 1,000; strict wait 15 s within a 25 s request budget, none below 1 s; strict waiters 64 per host; the miss path's body budget 25 s; check deadline 2 min. Knob: `adoption.adopt_unsigned` (bool, default `true`).
- Trust is never inferred from "adopted": an unsigned snapshot never vouches a `.deb`, never counts for strict mode, never writes a signed-seen marker; no code path stores a plain-text body as an inline snapshot. Unsigned candidates set `Form: cache.FormUnsigned` explicitly (the cache never derives it).
- Tests that call `wallclock.SetOffsetForTest` never use `t.Parallel()`. Simulations run inside `testing/synctest` bubbles, finish in under 5 s under `-race` with about 0.5 s of margin (ruling PE-8; only the 500-suite simulation stays over it, C-38, phase H), and silence loggers with `simtest.DiscardLogger()` (or capture them).
- Every goroutine started under `internal/freshness` recovers panics (`grep -n 'go func' internal/freshness/*.go`), and so does every goroutine this phase starts in `internal/handler`.
- `go test -race ./...` and `golangci-lint run ./...` are clean; `gofmt -l .` lists only the five known drift files (`cmd/apt-cacher-ultra/ca_print.go`, `internal/cache/cache_test.go`, `internal/cache/gc_urlpath_test.go`, `internal/freshness/hot_prefetch_test.go`, `internal/proxy/connect_stats.go`) at the end of every task. Never run `gofmt -w` on those five; edit them by hand.
- Commit trailer: the implementer's own harness attribution line (phase A ruling); otherwise `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

Conventions in this plan:

- Each task's **Interfaces** block lists what it uses from other tasks and existing test files. Never redefine a helper: a second definition in the same package does not compile.
- Each commit step starts with `gofmt -l <files>`; it must print nothing. Run `gofmt -w` on the non-drift files a step edits before that check; never on the five drift files.
- A "verify failure" step whose expected result is a compile failure is complete when the compiler names the undefined identifiers listed. `go vet` stops at a package's first type error, so such a step runs `go test -gcflags=-e …` (every error) or names only `go vet`'s first error per package (executability review D10).
- Mutation checks are run one at a time, each reverted before the next; record each failing test line in the commit message.
- **Existing tests that change.** Task 5 is a fixture-only commit that stays green on the phase D code. It gives the freshness tests' never-adopted suites clearsigned `InRelease` bodies, client interest and client-created anchor rows. For rulings PE-3 and M-14 it also makes three gate tests start from an adopted suite and gives detached fixtures armored signatures. Tasks 6, 7 and 13 then change only the expectations the new dispatch and the rulings change, each listed with its reason. Any other existing test that fails means the change is wrong.

## Rulings and design notes

Controller rulings (binding):

- **R-E1 — unsigned eligibility evidence** (D carry-forward). The signed-seen marker stays the single authority `CommitAdoption` re-reads inside its writer transaction. Before any unsigned commit the adopter re-runs the cached signed-anchor mark for the suite under the suite mutex it already holds: it reads the suite's `<suite>/InRelease` and `<suite>/Release.gpg` url_path rows, and those of its `/.` twin (design review I-3, ruling PE-5), and sniffs their pool files outside the writer, marks through phase D's machinery (`MarkSignedSeenBatch`), then commits; the mark is enqueued first, so FIFO makes it visible to the commit's re-read. A failed mark refuses the unsigned adoption (recorded as a refusal and logged, design review M-1; retried at the record's retry time, spec C7), never commits it. One authority, D's machinery reused, no pool I/O on the writer, and D's backstop residual closed where it matters. Task 3 (`remarkSignedAnchors`).
- **R-E2 — refusals are states, not failures** (C carry-forwards). An in-commit refusal surfaces as its own sentinel (`ErrAdoptionUnsignedDowngradeRefused`) through `classifyAdoptionOutcome` (outcome `refused`), bypasses the `adoption: commit failed` WARN and the `db_failed` wrap, and gives `adoption_flip_barrier` an outcome that is not `commit_failed` (`refused`) — also after a barrier that outlived `adoption.max_duration`. Each E refusal sentinel has its own `classifyAdoptionReason` case and stays out of `refusalKeepsCurrent`; tests assert the log levels and the recorded reason. Task 3.
- **R-E3 — one check tail.** The probe-tree rewrite folds the duplicated inline/detached tails into one shared tail (`finishCheck`) and one 304/unchanged helper (`steadyResult`), so record-clearing and the `freshness_check` line live in one place; the unexpected-status default arms log through the failed-check line (`failCheck`, today's `recordUnknownEnd`) and count as `failed`. Task 4.
- **R-E4 — absent statuses reset the streak.** 401/403/404/410 (and a 200 failing its sniff) on an anchor probe classify as absent through `classify`, not through the failed-check path; an `anchor_absent` suite does not inherit the failure backoff (B carry-forward). Tasks 6, 7; design note 19 records where spec C4 itself makes a check that found no anchor a failed one.

Plan-review rulings (binding, 2026-10-19; the reviews' findings they accept are applied in the tasks named):

- **PE-1 — deviation 2 stays, and covers the `anchor_absent` gap** (M-19): a future `adoption_fail_first_at` confirms neither an unsigned transition nor an absence; re-recording restarts the streak. Tasks 6, 7.
- **PE-2 — R-E4 and C4 Dispatch 3 keep their split, with visibility and pacing.**
  - A never-adopted suite whose tree finds no anchor fails the check (the spec's rule; the streak backs off). It also records `anchor_absent` (`anchorAbsentKey`, moved to Task 6's `probe.go`), so the status page shows it.
  - An adopted `anchor_absent` suite is paced by its record (`pacedAbsent`; `suiteDue` does not queue it). Its anchor is asked for again only at the retry time, and client checks inside the window ask nothing.
  - Without this, an EOL release costs 400–1,800 requests a day, forever. The cost if wrong: a returning anchor is noticed at the retry time, at most an hour later.
  - Tasks 6, 7.
- **PE-3 — the scheduler paces deferred never-adopted suites** (I-4). While the record is active and not due and the last check is younger than `periodic_refresh`, no probe runs (`pacedFirst`), so client checks ask nothing.
  - Without this, a full anchor download per client check costs up to hundreds of MiB a day per suite. The cost if wrong: new content of a failing suite is attempted within one refresh interval rather than at the next client hit.
  - It replaces design note 15's "fetches its anchor in full on each check". Task 6; Task 5 moves three gate tests to adopted suites; Task 13 adapts two of its tests.
- **PE-4 — the probe bound** (I-5, design note 26): keep the deterministic burst simulation and add a same-host racing one. The bound is "twice by the scheduler, plus one per client check while waiting (at most one per cooldown) and one per offer another admission takes first". Task 8; docs in Task 15.
- **PE-5 — I-1, I-2, I-3, I-6 accepted:**
  - I-1: cross-form requests record no validators, and the current anchor's own are sent while a record without validators is active (Task 7).
  - I-2: a fourth marker writer. A probe that finds a sniff-passing signature for a suite without a signed current snapshot marks the base before admitting; a failed write fails the check (Tasks 6, 7).
  - I-3: R-E1's re-mark also reads the `/.` twin's rows (Task 3).
  - I-6: the production-shaped DoD fixture is rebuilt (Task 12).
- **PE-6 — the accepted minors.**
  - Code:
    - M-1: the re-mark's failure is logged, WARN or ERROR, and fails closed (Task 3).
    - M-2: the unsigned content key is domain-separated (Task 3).
    - M-3: the interest window is symmetric (Task 6).
    - M-5: the member cap is checked before a slot is taken (Tasks 6, 7).
    - M-7 and M-8: never-downgrade reads both schemes' markers and any kept signed snapshot (Tasks 1, 3).
    - M-10: strict wait = min(15 s, 25 s − the request's age), none below 1 s (Task 9).
    - M-13: the stale WARN leaves out refusal states (Task 7).
    - M-14: a detached suite's changed `Release` without a signature records `anchor_absent`, done in E (Task 7).
    - M-15: the miss path's backstop deadline counts from the request's start, done in E (Task 10).
    - M-16: an unsigned suite's own form found again with new bytes is its update (Task 7).
    - M-19: the deviation texts.
    - M-20: the `last_seen_at` carry-forward is "corrected" (Task 15).
  - Documentation (Task 15): M-4, M-6, M-9, M-11, M-12, M-17.
  - The underspecified items 1–8, including 5: a client that left during the strict wait gets no retry (Task 9).
- **PE-7 — C-21 re-tagged (H); C-40 closed** (M-18): the task commits are history and the tests are the durable evidence, and copying mutant tables into docs would be maintenance noise, as for D-59. Task 15.
- **PE-8 — the executability review's fixes.**
  - D1–D10 text fixes, in the tasks they name.
  - O1: `unvouched_deb_refused` is logged once per fetch (only when `!shared`), pinned with two coalesced requests (Task 9).
  - D6: the 5 s `-race` budget is firm, with about 0.5 s of margin. The burst simulations run in about 3–3.5 s (Task 8); the 30-day simulation (renamed `TestSimRetry_LongRunDateRulesAndClockSteps` by ruling PE-9, below) is coarsened to about 3.6 s without weakening an assertion (Task 14).
  - The 14 proposed tests are accepted, in the tasks the review names; X26 and X28 get no test.
- **PE-9 — the coarsened retry simulation names no day count** (review fix round 1, Task 14). `TestSim30Days_RetryDateRulesAndClockSteps` runs about 20 virtual days since D6, so it is renamed `TestSimRetry_LongRunDateRulesAndClockSteps`, its doc comment states the horizon, and spec §7's C7 line says "a multi-week simulation (about 20 virtual days, per-day bounds)". This plan's task code blocks and mutant tables keep the old name as executed.

Rulings of earlier phases this plan applies: PD-1 (a file changed only by a mechanical rename does not count as touched for the deferred-minor rule: Task 4's comment renames, Task 8's `finish(…, 0)` edit, Task 3's `sed` in the drift file `hot_prefetch_test.go`), PD-5 (no pool I/O on the writer: R-E1's sniffs run on the adoption goroutine), PD-11 (the hit path already fires T1 after onboarding).

Design notes (choices the spec leaves open — the boring, bounded option, with the reason):

1. **One anchor mapping** (Task 1). `aptpath.AnchorPaths(form string) []string` takes the form's name (`aptpath` is standard-library-only and cannot import `cache`); `(*cache.SuiteSnapshot).AnchorHash(name string) *string` maps an anchor file to its hash column (the unsigned `Release` lives in the single-anchor slot). Every reader of a snapshot's anchor goes through them — `CommitAdoption` Step 3c, url_path GC guard (d), the check's baseline (`snapshotAnchorHash`, the detached baseline), `servedAnchorUnchanged`, the probe tree's `sameAs` — so no reader infers a form from which hash column is set.
2. **The in-transaction recheck comes first** (Task 1). For an unsigned candidate, `CommitAdoption`'s Step 1a applies the never-downgrade rule inside the writer transaction, before any write: `refuseUnsignedDowngrade`, the rule eligibility shares (`Cache.CheckUnsignedDowngrade`), refuses when a marker exists for the host and `aptpath.MarkerBase(suite)` under either scheme (M-7; the `/.` twin shares the base), or an adopted, non-retired inline or detached snapshot of the base or its `/.` twin exists under either scheme (M-8: also a signed displaced snapshot the audit left queued and unmarked). Its refusal (`cache.ErrUnsignedDowngradeRefused`) changes nothing.
3. **SHA512 is checked before the bytes reach `pool/`** (Task 2): a SHA512 tee during the fetch, SHA256 through `FinalizeExpectingHash`. Mismatched bytes are never left in the pool as a stray blob (the plain fetch path used to leave them).
4. **SHA512-only members are never reused from the pool without a fetch** (Task 2): the pool is keyed by SHA256, which the Release does not declare for them. Their rehash on reuse runs after the fetch (review fix round 1): once the fetched bytes match the SHA512, an existing `pool/<derived SHA256>` is re-hashed before `Finalize`'s dedup could keep it (`verifyMember`'s hook, `rehashPooledBlob`), and a corrupt one is logged (`pool_corruption_during_adoption`), counted and evicted, so the verified bytes replace it. A pooled blob whose SHA256 matches but whose SHA512 does not fails the member (the Release disagrees with itself), and the blob is kept (another snapshot may hold it).
5. **Hash-block parsing** (Task 2): a malformed or conflicting SHA512 entry fails the parse as a SHA256 one does; an identical repeated entry is one member; a Release with neither block is an error (`no SHA256 or SHA512 block`).
6. **Repair recovers SHA512 from the snapshot's Release** (Task 2): a skip record keeps only SHA256, so repair re-parses the snapshot's Release (the trust anchor) for every declared hash; when that fails it falls back to the record's SHA256 (DEBUG `adoption_repair_declarations_unavailable`).
7. **The by-hash probe uses the strongest declared hash** (Task 2), SHA512 when declared, as apt does.
8. **R-E1's re-mark** (Task 3) runs before the flip barrier, not inside a hold, under the barrier's context and the commit's bound, over the suite's rows and its `/.` twin's (I-3). A pool file that does not exist is no evidence (as in the startup marker pass, with which it shares `sniffCachedAnchor`); any other read error, and a failed mark, refuses the attempt as `unsigned_downgrade_refused` (fail closed) and logs `adoption: signed-anchor re-mark failed` (WARN, ERROR for a pool read error; M-1): that refusal means "could not rule out a signature".
9. **`classifyAdoptionReason`'s refusal default** (Task 3): a refusal sentinel without its own case is recorded under the outcome (`refused`), which the admin page shows as a failure — never as `rollback_refused`, whose lag the page hides.
10. **Unsigned snapshots build no vouching rows at all** (Task 3): besides `package_hash` (`buildPackageHashes` takes the form), the Sources and pdiff builders do not run for them.
11. **An in-commit refusal's barrier outcome is `refused`** (Task 3), a new `adoption_flip_barrier` value (R-E2 asks only for "not `commit_failed`").
12. **Unsigned eligibility is checked three times** (Tasks 3, 6, 7): by the check before a slot is taken (`unsignedRefusal`, from `unsignedFirst`, `checkUnsigned` and `anchorFoundAgain`: a refused candidate never holds a slot), in `runShared` before any download (so `RunUnsigned` is safe for any caller), and in the commit (design note 2). The member cap is applied twice: by the check before a slot is taken (`unsignedTooLarge`, M-5) and in `runShared` before any download; both count `ParseRelease`'s merged SHA256/SHA512 paths, so MD5- or SHA1-only entries do not count.
13. **Interest** (Task 6): `last_requested_at` within 7 days of now in either direction (M-3: a symmetric window). A stamp up to 7 d ahead keeps the suites that were interested before a backward step; one further ahead is dormant until a client touches the suite (`TouchSuite` rewrites the stamp within its coalescing hour). Reading any future stamp as interest would turn every suite touched during a year-ahead boot into a first adoption. A future stamp is reported by the check through `clockStep`; `suiteDue` reads it silently. A returning client's first request may find its suite still dormant (its touch and the hit's T1 run concurrently); the next tick adopts it, within one tick of that request (M-12; `TestDoD_ActiveSuitesAdoptOnceDormantSuitesStaySilent`).
14. **A client-created anchor row** (Task 6) is an `InRelease` or `Release` url_path row with `request_count > 0`. `Release.gpg` does not count (apt fetches it only after `Release`); flat index rows count from phase G.
15. **The probe tree sends no validators and gates by content key** (Task 6; spec C4 "without validators"); ruling PE-3 bounds what that costs. A never-adopted suite whose record is active and not due is paced (`pacedFirst`): no probe while its last check is younger than `periodic_refresh`, so client checks ask nothing, and the scheduler's refresh check or the due retry fetches the anchor in full. Spec C4 and C7 compose: the record gates by key, and the pacing bounds the cost (design review I-4; the draft's "C4 and C7 disagree" was overstated). With the default 15-minute refresh, a suite failing for a missing vendor key costs at most one anchor download per refresh interval, not one per client check.
16. **The unsigned confirmation gap** (Task 6): `max(cooldown, 5 min)`, or an hour when a signature probe of the confirming observation answered 401/403 (spec C5 does not name the observation; the confirming one is the observation that would adopt, and it needs no extra state).
17. **Confirmed once** (Task 6): bytes that already passed confirmation — a record for the same content key from an adoption attempt — are not confirmed again, so a failing unsigned first adoption keeps the C7 schedule instead of restarting a short streak per attempt.
18. **Confirmation streaks restart** (Task 6, `restartsStreak`): an `unsigned_unconfirmed` or `anchor_absent` record for other content, or after another reason, starts at count 1 with `first_at` now, so `adoption_fail_first_at` is the first observation of this state and content — the time spec C5 compares.
19. **Nothing adoptable is a failed check** (Tasks 6, 7): a never-adopted suite whose tree finds neither `InRelease` nor `Release` (spec C4 Dispatch 3: "otherwise a failed check") and an unsigned candidate while `adopt_unsigned` is off extend the streak and back off (2 min → 1 h). The no-anchor case also records `anchor_absent` (ruling PE-2), so the status page shows it and the record paces the suite. R-E4's reset applies to absent probes and to an adopted suite's `anchor_absent`, where a snapshot keeps serving; a suite with nothing to serve does not spend its probes on every tick for as long as clients ask for it.
20. **Probe URLs** (Task 6): each anchor URL is a cached anchor row's `upstream_url` with its last segment replaced (keeping a non-default port or a mirror prefix), else the checker's `urlReconstruct`.
21. **A 304 is present only to a conditional request** (Task 6, `classify`); a 304 to a request without validators is a protocol violation, unknown. (Spec C4's table lists no 304.)
22. **Lateral content keeps its record** (Task 7): a detached suite's failing lateral `InRelease` is held by its record, and the detached anchor's "unchanged" does not clear it (otherwise every other check would attempt it again). Ruling PE-10 (review fix round 1): while any record of the suite is active and not due, no lateral attempt is made at all (`lateralInline` holds when the record is that content's, else leaves the check to the detached path), so a publish that fails in both forms is attempted once per form per retry window, not at every check; a good `InRelease` waits at most the record's retry time behind a failing detached record. **A record's validators belong to its request's own anchor file** (I-1). A cross-form request (`lateralInline`, `upgradeProbe`, an absent anchor's tree; `adoptionRequest.crossForm`) records none; the candidate keeps them, so a successful flip still stores them. While a record without validators is active and not due, the check sends the current anchor's own (`retryGate.validators`). A 304 is `changed_deferred` only when the record's validators were sent (`retryGate.recordValidators`), otherwise `not_modified`. Without this, a CDN sending `Last-Modified` alone answered the detached `Release` 304 to the failed lateral `InRelease`'s stamp, and the suite froze.
23. **`anchor_absent` records** (Tasks 6, 7):
    - They use one constant content key, `sha256("anchor_absent")`: the absence has no content, and the constant never equals a real key, so whatever upstream serves next is new content.
    - They start at the first absent observation (deviation 1).
    - The record paces an adopted suite (ruling PE-2, `pacedAbsent`): no request before its retry time, and `suiteDue` does not queue it.
    - A record from an adoption attempt (a failed lateral move) keeps its own schedule. Once a check found the current anchor absent behind it (the absent `InRelease` whose lateral move failed; M-14 behind a held lateral record), it paces the suite as an `anchor_absent` record does (ruling PE-10, `pacedAbsentAnswer`), while its reason (e.g. `gpg_failed`) stays the one shown; the check after each attempt asks once more.
    - The current form and content found again is "unchanged" and clears the record. An unsigned suite's own form with new bytes is its update (M-16). For a signed suite, an unsigned candidate is recorded again.
    - A detached suite whose changed `Release` comes with an absent `Release.gpg` records it too (M-14), keyed on the constant: never adopted, paced, visible.
24. **An unsigned suite never updates past a signature** (Task 7): an unknown signature probe fails the check (a signature may be there); a present signature whose signed adoption is deferred is `changed_deferred`, not an unsigned update. The upgrade probe marks the base when it finds a signature (I-2), so after it the suite is never updated unsigned, also once the signature is gone (`TestCheck_UnsignedSuiteSeenSignedNeverUpdatesUnsigned`).
25. **The waiters' signed flag** (Task 8) is refreshed from each suite's current form on every tick (`refreshSigned`, reading `ListSuitesWithAdoption`): exact within one tick, in both directions.
26. **The probe bound** (Task 8; ruling PE-4, I-5): a queued suite is probed twice by the scheduler — the check that queued it and the offered re-check that admits it — plus once per client check while it waits (at most one per cooldown) and once per offer another admission takes first (an offer that loses its slot keeps its place, spec C4). The deterministic burst simulation (signed anchors answer 1 s late) pins the scheduler's two; `TestSimCapacity_UpgradeBurstSameHost` runs the burst on shared hosts with client checks and checks the bound with the lost offers counted (`offerHook`); since an offer's GET takes no virtual time the burst's own races almost never meet one, so one suite's first offer loses its slot by construction and the test asserts it was counted (review fix round 1).
27. **Only signed adoptions count as in flight** (Task 9): an unsigned adoption never vouches, so waiting for it would only delay the refusal; a count ends when the adoption goroutine returns, after its commit, so a woken request re-reads rows the new snapshot already wrote. Repairs and reconciles that add `package_hash` rows are not counted (acceptable: they run on unchanged checks). `acu_strict_adoption_waits_total` counts the requests that waited (not those refused at the 64-waiter cap or below the 1 s floor).
28. **T1 on a creating miss fires after the response** (Task 10, a `defer`), so the creating request's own serve is a registered serve its suite's first flip waits for; the `defer` also fires when the client left, which is right (the row was created). Only the creating request fires it (`TestServeHTTP_CoalescedCreatingMissFiresT1Once`).
29. **The nvidia-shaped DoD fixture is `dists/`-shaped** (Tasks 2, 12): a SHA512-only Release dated 2018 with SHA256 Packages; the flat layout and `./` Filenames are phase G's.
30. **The checker's marker writer always writes** (Tasks 6, 7; ruling PE-5, I-2): `Adopter.markSignatureSeen` calls `MarkSignedSeenBatch` at every probe that finds a signature for a suite without a signed current snapshot, with no read first. The marker keeps the newest stamp (its last sighting, which phase F's orphan sweep reads), and the check's pacing bounds the writes. A failed write fails the check: nothing is admitted on a signature the cache could not record.
31. **The strict wait is bounded by the request's age** (Task 9; M-10, O1, underspecified 5):
    - `WaitHostAdoptions(ctx, host, start)` waits at most min(`strictWait`, `strictRequestBudget` − the request's age) and not at all below `strictMinWait` (1 s). The miss path still downloads the whole `.deb` before its first byte, within apt's 30 s.
    - A client that left during the wait gets no retry (`client_canceled`).
    - The refusal is logged once per fetch, by the singleflight leader.
    - Each waiting request takes a waiter slot, coalesced ones included (M-11, documented).
32. **The miss path's backstop deadline counts from the request's start** (Task 10; M-15, D-35's sibling): `missBackstopDeadline(start)` = min(now + `backstopTimeout`, start + `missBodyBudget`). A deadline already past fails the backstop at once (logged; the body is served), as any backstop timeout does.
33. **The stale WARN leaves out refusal states** (Task 7; M-13): `freshness: adopted suites stale beyond threshold` counts adopted stale suites minus those in a refusal state. An `anchor_absent` suite keeps its last success old on purpose, and the status page shows it. The gauge `acu_freshness_stale_suites` still counts every stale suite.
34. **The unsigned content key is domain-separated** (Task 3; M-2): sha256("unsigned\n" ‖ Release). An `InRelease` body equal to a `Release` body (the legacy plain-text shape) can no longer share a key with an inline record and skip confirmation. No stored key is unsigned before phase E.
35. **A `cachedAnchors` read error is logged, not recorded** (Task 6; underspecified 2): WARN `freshness: read anchor rows failed`, no request, no write, no backoff, retried at the next check — at most one per cooldown and per tick. A local fault is not an upstream failure.

## Deliberate deviations from the spec

1. **`anchor_absent` is recorded at the first absent observation** (Task 7). Spec C4 records the refusal state only after two observations and a probe tree that finds no signed form. Recording it at once is the mechanism the two-observation rule needs anyway — the record's `adoption_fail_first_at` is the first observation, restart-safe — and it makes the state visible (status page, `adoption_refused`) from the first check that sees it; the snapshot keeps serving either way, and the tree still runs only after the gap. Its side effects (M-19): a transient blip costs an INFO `adoption_refused` + `adoption_deferred` pair and two writer operations (the record and its clear), and when another record is active and due the tree runs at once, without the two-observation gap.
2. **A future `adoption_fail_first_at` never confirms an unsigned transition** (Task 6). Spec C4's clock-step rule reads a future past-event stamp as infinitely old in gates that delay work, which here would confirm at once and skip safeguard 2 (the overview's E carry-forward asked for exactly that rule). The gate instead treats it as unconfirmed, and the next record restarts the streak at now (`recordAdoptionFailure`'s past-event rule): a backward clock step costs one more gap, never a skipped confirmation. The same holds for the `anchor_absent` gap (ruling PE-1, M-19): a future `first_at` never confirms the absence, and re-recording restarts it.

## Review Focus (phase E share)

1. **Upgrading a live v6 cache** (hundreds of never-adopted suites, most of them dormant; §6.1-quarantined plain-text suites; suites a v6 binary served signed but never adopted): only suites a client asked for in the last 7 days adopt — dormant ones make no request, and a returning client's suite adopts within one tick — signed updates never wait behind more than one first adoption, a first adoption that keeps failing is paced (no probe per client check), a quarantined plain-text suite adopts unsigned without a downgrade refusal, and a suite once served or probed signed, under either scheme, is never adopted unsigned — Tasks 1, 6, 8, 12 (the production-shaped fixture, I-6).
2. **Upstreams answering 200 HTML, 401 or 403 for anchor files** (soft-404s, captive portals, WAFs, S3 permission errors): classified absent, never adopted and never taken for a signature; a denied signature stretches the unsigned confirmation to an hour; any other unexpected answer is a counted failed check — Tasks 4, 6, 7.
3. **An upstream that drops its current anchor or its signature** (an inline suite's `InRelease` disappears; a detached suite's `Release.gpg` 404s while its `Release` changes): the signed snapshot keeps serving, the state is visible as `anchor_absent` in both cases (M-14) — unless a failed adoption attempt's record holds (a lateral move that failed), which keeps its own reason (e.g. `gpg_failed`) and paces the suite the same way (ruling PE-10) — a lateral move happens only to the other signed form, and nothing moves a signed suite to unsigned; an absent anchor is asked for only at its record's retry times (PE-2: about 24 requests a day after the first hour, none from client checks) and never inherits the failure backoff; a cross-form failure leaves no validators that could freeze the current form (I-1) — Task 7 (`TestCheck_AbsentAnchorIsPacedByItsRecord`, `TestCheck_AbsentAnchorNeverMovesToUnsigned`, `TestCheck_DetachedSuiteThatDropsItsSignatureNeverGoesUnsigned`, `TestCheck_DetachedSuiteKeepsAdoptingWhileItsLateralInReleaseFails`).
4. **The first-adoption burst** (an upgrade, or many new suites at once) under the default pool of two: first adoptions run one at a time and back to back, each queued suite costs two probes by the scheduler plus one per client check and one per lost offer (PE-4), goroutines stay bounded, and contention on a stalling host does not starve a healthy suite — Task 8 (`TestSimCapacity_UpgradeBurst`, `TestSimCapacity_UpgradeBurstSameHost`), Task 14 (`TestSimScheduler_StallingHostWithAdoptionContention`).
5. **Strict mode while suites are being adopted** (`refuse_unvouched_debs = true` during first installs after an upgrade): a `.deb` requested while a signed adoption runs on its host waits for it (at most 15 s and never past 25 s into the request, 64 waiters per host) on both paths and is served once vouched; a client that left is not retried, and the refusal is logged once per fetch; unsigned snapshots never vouch and never turn strict mode on — Tasks 1, 9, 12.
6. **A never-downgrade race** (a signed anchor served on a client miss while an unsigned adoption of the same suite is in flight): the commit's in-transaction recheck refuses it as a state, even after a barrier past `max_duration`, and a cached signed anchor whose marker was lost is re-marked before an unsigned commit — Tasks 1, 3.

## File map

| Area | Files | Task |
|---|---|---|
| Anchor forms in the cache | `internal/aptpath/anchor.go` (new: `AnchorPaths`); `internal/cache/types.go` (`AnchorHash`), `adoption.go` (the never-downgrade rule `refuseUnsignedDowngrade` and `CheckUnsignedDowngrade`, Step 1a recheck, Step 3c by form, `DeclaredHashesForPath` skips unsigned), `gc_urlpath.go` (guard (d) by form), `queries.go` (`HostCurrentSnapshotsCoverage` skips unsigned) | 1 |
| SHA512 | `internal/freshness/release_parse.go` (merged blocks), `member_verify.go` (new: the shared verifier and aliases), `adoption.go`, `repair.go`, `reconcile.go` | 2 |
| The unsigned form in the adopter | `internal/freshness/unsigned.go` (new: eligibility, the cap, the re-mark over the twin's rows), `adoption.go` (`runShared`, the re-mark's log line, Step 11, `buildPackageHashes`), `legacy_audit.go` (`sniffCachedAnchor`), `retry.go` (`contentKey`, the unsigned key), `metrics.go` (refusal outcomes and reasons), `reconcile.go` | 3 |
| One check tail | `internal/freshness/check.go` (new: the check paths moved out of `freshness.go`), `freshness.go` | 4 |
| Fixtures | `internal/cache/testing_helpers.go`, the freshness test fixtures (Task 5's table) and `internal/handler/phase2_detached_chaos_test.go` (armored signatures) | 5 |
| Eager first adoption | `internal/freshness/probe.go` (new: `classify`, `probe`, `anchorAbsentKey`), `first_adoption.go` (new: `checkFirst`, the symmetric `interested`, `pacedFirst`, the probe tree, `unsignedFirst`), `unsigned.go` (`markSignatureSeen`, `unsignedTooLarge`), `check.go`, `scheduler.go` (dormancy), `adoption_failure.go` (`restartsStreak`), `freshness.go`; `internal/cache/signed_seen.go` (the writers' note) | 6 |
| Form transitions | `internal/freshness/current_form.go` (new: `pacedAbsent`, `anchorAbsent`, `anchorFoundAgain`, `lateralInline`, `upgradeProbe`, `checkUnsigned`), `check.go` (the paced check, `steadyResult`, M-14), `freshness.go` (repair slot by form, `crossForm`, the gate's validators), `adoption_failure.go` (cross-form records), `first_adoption.go` (`unsignedRefusal`), `scheduler.go` (the paced `suiteDue`, the stale WARN), `metrics.go` | 7 |
| Scheduler | `internal/freshness/scheduler.go` (`offerHook`), `admission.go`, `check.go`, `freshness.go` (`Cache.ListSuitesWithAdoption`); `sim_burst_test.go` (both burst simulations) | 8 |
| Strict mode's wait | `internal/freshness/inflight.go` (new: the request-age bound), `freshness.go`, `metrics.go`; `internal/handler/handler.go` (the client-left check, the leader's refusal line), `singleflight.go`; `cmd/apt-cacher-ultra/main.go` | 9 |
| First contact | `internal/handler/handler.go` (T1-on-seed, the miss backstop's deadline), `singleflight.go`, `suite_rows.go` (onboarding's deadline, `missBackstopDeadline`) | 10 |
| The knob | `internal/config/config.go`, `packaging/config/config.toml.default`, `cmd/apt-cacher-ultra/main.go` | 11 |
| DoD | `internal/freshness/dod_e_test.go`, `internal/handler/unsigned_posture_test.go` (new tests) | 12 |
| Deferred minors | `internal/freshness/flip_barrier.go` (new: the barrier code moved out of `adoption.go`), `freshness.go`, `adoption.go`, `adoption_failure.go`, `legacy_audit.go`, `reconcile.go`, `retry.go`, `internal/handler/handler.go`; tests | 13 |
| Simulations | `internal/freshness/sim_sched_test.go`, `sim_retry_test.go` (the coarsened retry simulation, about 20 virtual days; and the `simMembers` callers) | 14 |
| Documentation | `docs/configuration.md`, `docs/log-fields.md`, `docs/admin-ui-spec.md`, the spec, the overview, the four deferred files | 15 |

No migration, no new package, no new dependency.

## Tasks

1. Anchor forms in the cache — `AnchorPaths`, the unsigned form's storage rules and the in-transaction never-downgrade recheck
2. SHA512 hash blocks and the shared member verifier
3. The unsigned form in the adopter — eligibility, member cap, no vouching, the re-mark (R-E1) and the in-commit refusal as a state (R-E2)
4. One check tail (R-E3): the check paths move to `check.go`, share their tail, and an unexpected status is a failed check
5. Fixture-only commit — the existing check tests' never-adopted suites become ones phase E adopts
6. Eager first adoption — classification, the probe tree, dormancy and the unsigned transition
7. Form transitions of adopted suites — lateral moves, `anchor_absent`, the unsigned upgrade, and `signedCurrent` for repair and reconcile
8. The scheduler under eager adoption — waiters' signed flag, the upgrade-burst simulation, and the scheduler's deferred minors
9. Strict mode waits for adoptions in flight (C10, D10)
10. The handler at first contact — T1-on-seed, waiters' late registration, onboarding's deadline
11. The `adoption.adopt_unsigned` knob, its wiring, and `main`'s deferred minors
12. Definition of done — the end-to-end scenarios and the DoD map
13. Deferred minors in the files this phase changes (code)
14. Deferred minors in the simulations this phase changes
15. Documentation, spec revision 8, overview, deferred files, final verification

---
### Task 1: Anchor forms in the cache — `AnchorPaths`, the unsigned form's storage rules and the in-transaction never-downgrade recheck

Spec C5 "Anchor forms and storage" (`aptpath.AnchorPaths(form)`; the readers that must go through the form, not the hash columns: `CommitAdoption` Step 3c and url_path GC guard (d) — the two `AIDEV-TODO(phase E)` readers, overview "E:" line), safeguard 3's second point (the never-downgrade re-read inside the `CommitAdoption` writer transaction, refusing an unsigned candidate with no pointer change and no rows when the suite has signed history; overview review finding 5), and safeguard 5's strict-mode reads (`DeclaredHashesForPath` and `HostCurrentSnapshotsCoverage` filter `anchor_form <> 'unsigned'`; spec C10: unsigned snapshots never count and never vouch). Signed history is one rule, `refuseUnsignedDowngrade`, which the commit reads inside its transaction and the early exits of Tasks 3, 6 and 7 read through `CheckUnsignedDowngrade` — the same SQL, so an early exit never admits what the commit refuses. The rule is a signed-seen marker for the suite's base (the `/.` twin shares it), or an adopted, non-retired inline or detached snapshot of the suite or its twin: the current one, or a displaced one that the legacy audit left queued, and so unmarked (design review M-8). Both count **under either scheme** (M-7): a base seen signed over https protects the same base over plain http, the scheme a man-in-the-middle can strip. A retired snapshot (quarantined, reaped) is no history. Deferred minor A-29: the `AIDEV-TODO(phase E)` that sat as an SQL comment in `gc_urlpath.go` becomes a Go `AIDEV-NOTE`, and its TODO is resolved. No adoption produces an unsigned snapshot yet (Task 3 does), so the tests build unsigned rows directly.

**Files:**
- Create: `internal/aptpath/anchor.go`, `internal/aptpath/anchor_test.go`, `internal/cache/unsigned_form_test.go`
- Modify: `internal/cache/types.go` (`SuiteSnapshot.AnchorHash`, `anchorHash`), `internal/cache/adoption.go` (`ErrUnsignedDowngradeRefused`, Step 1a, Step 3c, `rowQuerier`, `CheckUnsignedDowngrade`, `refuseUnsignedDowngrade`, `DeclaredHashesForPath`), `internal/cache/queries.go` (`HostCurrentSnapshotsCoverage`), `internal/cache/gc_urlpath.go` (guard (d))

**Interfaces:**
- Consumes: cache test helpers `openCache`, `seedBlob` (`cache_test.go`), `stubNow` (`gc_test.go`), `derefI64` (`suite_rows_test.go`), `putURLPathRow`, `drainURLPathGC`, `urlPathExists`, `seedPackageHash`, `testMaxVersions` (`gc_urlpath_test.go`); `EnsureSuiteForTest`, `InsertTrackedCandidateForTest` (`testing_helpers.go`); `MarkSignedSeenBatch`, `SignedSeen` (phase A).
- Produces: `aptpath.AnchorPaths(form string) []string`; `(*cache.SuiteSnapshot).AnchorHash(name string) *string` and the unexported `anchorHash(form, name, single, release, releaseGPG *string) *string` (Tasks 2, 3, 7 read snapshot anchors through it); `cache.ErrUnsignedDowngradeRefused` (Task 3 maps it to the adopter's refusal); `(*cache.Cache).CheckUnsignedDowngrade(ctx, host, suite string) error` (the never-downgrade rule outside a transaction: Task 3's `unsignedEligible` reads it, for itself and for Tasks 6 and 7); the test helper `seedFormSnapshot(t, c, scheme, host, suite, form, single, release, gpg *string, coverage bool) int64` (`unsigned_form_test.go`).

- [ ] **Step 1: Write the failing tests**

Create `internal/aptpath/anchor_test.go`:

```go
package aptpath

import (
	"slices"
	"testing"
)

// Spec C5 "Anchor forms and storage": the anchor file(s) of each form.
func TestAnchorPaths(t *testing.T) {
	for _, c := range []struct {
		form string
		want []string
	}{
		{"inline", []string{"InRelease"}},
		{"detached", []string{"Release", "Release.gpg"}},
		{"unsigned", []string{"Release"}},
		{"", nil},
		{"Inline", nil},
	} {
		if got := AnchorPaths(c.form); !slices.Equal(got, c.want) {
			t.Errorf("AnchorPaths(%q) = %q, want %q", c.form, got, c.want)
		}
	}
	// Callers may keep the slice: a second call is not affected by edits.
	AnchorPaths("detached")[0] = "x"
	if got := AnchorPaths("detached"); got[0] != "Release" {
		t.Errorf("AnchorPaths shares its result: %q", got)
	}
}
```

Create `internal/cache/unsigned_form_test.go`:

```go
package cache

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// seedFormSnapshot inserts a suite row and an adopted snapshot of form
// (single holds the inline or unsigned anchor hash, release and gpg the
// detached pair), points the suite at it, and returns its id. Raw SQL:
// tests build forms adoption cannot reach here.
func seedFormSnapshot(t *testing.T, c *Cache, scheme, host, suite string, form AnchorForm, single, release, gpg *string, coverage bool) int64 {
	t.Helper()
	if _, err := c.db.Exec(`INSERT INTO suite_freshness (canonical_scheme, canonical_host, suite_path) VALUES (?, ?, ?)`,
		scheme, host, suite); err != nil {
		t.Fatal(err)
	}
	cov := 0
	if coverage {
		cov = 1
	}
	res, err := c.db.Exec(`INSERT INTO suite_snapshot
  (canonical_scheme, canonical_host, suite_path, inrelease_hash, release_hash, release_gpg_hash,
   anchor_form, created_at, adopted_at, heartbeat_at, package_coverage_complete)
  VALUES (?, ?, ?, ?, ?, ?, ?, 1, 1, 1, ?)`, scheme, host, suite, single, release, gpg, string(form), cov)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	if _, err := c.db.Exec(`UPDATE suite_freshness SET current_snapshot_id = ?
  WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`, id, scheme, host, suite); err != nil {
		t.Fatal(err)
	}
	return id
}

// Spec C5: an unsigned snapshot's Release hash sits in the single-anchor
// slot, so a snapshot names its anchors' hashes through its form.
func TestSuiteSnapshot_AnchorHash(t *testing.T) {
	single, rel, gpg := "single", "release", "gpg"
	for _, c := range []struct {
		form AnchorForm
		name string
		want *string
	}{
		{FormInline, "InRelease", &single},
		{FormInline, "Release", nil},
		{FormInline, "Release.gpg", nil},
		{FormDetached, "InRelease", nil},
		{FormDetached, "Release", &rel},
		{FormDetached, "Release.gpg", &gpg},
		{FormUnsigned, "InRelease", nil},
		{FormUnsigned, "Release", &single},
		{FormUnsigned, "Release.gpg", nil},
		{"", "InRelease", nil},
	} {
		s := SuiteSnapshot{Form: c.form, SingleAnchorHash: &single, ReleaseHash: &rel, ReleaseGPGHash: &gpg}
		got := s.AnchorHash(c.name)
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("%s snapshot: AnchorHash(%q) = %v, want %v", c.form, c.name, derefS(got), derefS(c.want))
		}
	}
}

func derefS(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// Spec C5 / overview E carry-forward: CommitAdoption Step 3c seeds an
// unsigned snapshot's <suite>/Release anchor — never <suite>/InRelease —
// and an unsigned commit writes no signed-seen marker.
func TestCommitAdoption_UnsignedSeedsItsReleaseAnchor(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, 1_800_000_000)()
	const suite = "/u/dists/s"
	rel := seedBlob(t, c, "Origin: u\nSHA256:\n")
	id, _, err := c.InsertTrackedCandidateForTest(ctx, SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h",
		SuitePath: suite, InReleaseHash: &rel, Form: FormUnsigned})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id, []SnapshotMember{{SnapshotID: id, Path: "Release", BlobHash: rel, DeclaredSHA256: rel}},
		nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	row, err := c.LookupURL(ctx, "http", "h", suite+"/Release")
	if err != nil || row.BlobHash == nil || *row.BlobHash != rel {
		t.Fatalf("Release anchor row = %+v, %v; want the snapshot's Release (%s)", row, err, rel)
	}
	if _, err := c.LookupURL(ctx, "http", "h", suite+"/InRelease"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unsigned snapshot seeded an InRelease anchor (err %v)", err)
	}
	if _, ok, err := c.SignedSeen(ctx, "http", "h", suite); err != nil || ok {
		t.Fatalf("an unsigned commit wrote a signed-seen marker (ok %v, err %v)", ok, err)
	}
	if snap, err := c.GetSuiteSnapshot(ctx, id); err != nil || snap.Form != FormUnsigned || snap.AdoptedAt == nil {
		t.Fatalf("snapshot = %+v, %v; want an adopted unsigned snapshot", snap, err)
	}
}

// Spec C5 safeguard 3, the second point: CommitAdoption re-reads the
// marker for the candidate's base inside its transaction (the /. twin
// shares it) and refuses an unsigned candidate — no pointer change, no
// rows — when one exists or the suite's current snapshot is signed. A
// signed candidate, and an unsigned one over an unsigned current snapshot,
// commit.
func TestCommitAdoption_RefusesAnUnsignedDowngrade(t *testing.T) {
	for _, tc := range []struct {
		name, suite, markerBase string
		prior                   AnchorForm // "" for none
		candidate               AnchorForm
		refused                 bool
	}{
		{"marker for the base", "/u/dists/s", "/u/dists/s", "", FormUnsigned, true},
		{"marker for the twin's base", "/flat/.", "/flat", "", FormUnsigned, true},
		{"signed current snapshot", "/u/dists/s", "", FormInline, FormUnsigned, true},
		{"detached current snapshot", "/u/dists/s", "", FormDetached, FormUnsigned, true},
		{"unsigned current snapshot", "/u/dists/s", "", FormUnsigned, FormUnsigned, false},
		{"no history", "/u/dists/s", "", "", FormUnsigned, false},
		{"signed candidate with a marker", "/u/dists/s", "/u/dists/s", "", FormInline, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := openCache(t)
			ctx := context.Background()
			defer stubNow(t, 1_800_000_000)()
			var priorID int64
			if tc.prior != "" {
				h := seedBlob(t, c, "prior anchor")
				g := seedBlob(t, c, "prior signature")
				switch tc.prior {
				case FormDetached:
					priorID = seedFormSnapshot(t, c, "http", "h", tc.suite, tc.prior, nil, &h, &g, true)
				default:
					priorID = seedFormSnapshot(t, c, "http", "h", tc.suite, tc.prior, &h, nil, nil, true)
				}
			} else if err := c.EnsureSuiteForTest(ctx, "http", "h", tc.suite); err != nil {
				t.Fatal(err)
			}
			if tc.markerBase != "" {
				if err := c.MarkSignedSeenBatch(ctx, []SignedSeenMark{{CanonicalScheme: "http", CanonicalHost: "h",
					Base: tc.markerBase, SeenAt: 1}}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.candidate == FormUnsigned {
				// The early exits read the same rule (CheckUnsignedDowngrade).
				if early := c.CheckUnsignedDowngrade(ctx, "h", tc.suite); errors.Is(early, ErrUnsignedDowngradeRefused) != tc.refused {
					t.Fatalf("CheckUnsignedDowngrade = %v, want refused %v", early, tc.refused)
				}
			}
			anchor := seedBlob(t, c, "candidate anchor")
			id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h",
				SuitePath: tc.suite, InReleaseHash: &anchor, Form: tc.candidate})
			if err != nil {
				t.Fatal(err)
			}
			name := "Release"
			if tc.candidate == FormInline {
				name = "InRelease"
			}
			err = c.CommitAdoption(ctx, id, []SnapshotMember{{SnapshotID: id, Path: name, BlobHash: anchor, DeclaredSHA256: anchor}},
				nil, nil, nil, false)
			sf, gerr := c.GetSuiteFreshness(ctx, "http", "h", tc.suite)
			if gerr != nil {
				t.Fatal(gerr)
			}
			if !tc.refused {
				if err != nil || sf.CurrentSnapshotID == nil || *sf.CurrentSnapshotID != id {
					t.Fatalf("commit = %v, current %v; want the candidate committed", err, sf.CurrentSnapshotID)
				}
				return
			}
			if !errors.Is(err, ErrUnsignedDowngradeRefused) {
				t.Fatalf("commit = %v, want ErrUnsignedDowngradeRefused", err)
			}
			if got := derefI64(sf.CurrentSnapshotID); (priorID == 0 && sf.CurrentSnapshotID != nil) || (priorID != 0 && got != priorID) {
				t.Fatalf("current snapshot moved to %d, want %d", got, priorID)
			}
			if members, err := c.ListSnapshotMembers(ctx, id); err != nil || len(members) != 0 {
				t.Fatalf("refused candidate has members %v (%v), want none", members, err)
			}
			if snap, err := c.GetSuiteSnapshot(ctx, id); err != nil || snap.AdoptedAt != nil {
				t.Fatalf("refused candidate = %+v (%v), want it unadopted", snap, err)
			}
			if _, err := c.LookupURL(ctx, "http", "h", tc.suite+"/Release"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("a refused commit seeded the Release anchor (err %v)", err)
			}
		})
	}
}

// Spec C5 safeguard 3 beyond the current snapshot and the suite's own
// scheme (design review M-7, M-8): a marker for the base under the other
// scheme, and an adopted, non-retired signed snapshot that is not current
// (a displaced one the legacy audit left unmarked) — of the suite, of its
// /. twin, or under the other scheme — refuse an unsigned candidate. A
// retired snapshot (quarantined) and a signed candidate never adopted are
// no history. CheckUnsignedDowngrade agrees with the commit every time.
func TestCommitAdoption_RefusesUnsignedForAnySignedHistory(t *testing.T) {
	type history struct {
		scheme, suite string
		form          AnchorForm
		current       bool
		adopted       bool
		retired       bool
	}
	for _, tc := range []struct {
		name                     string
		suite                    string // the unsigned candidate's, over http
		markerScheme, markerBase string
		snapshot                 *history
		refused                  bool
	}{
		{name: "marker under the other scheme", suite: "/u/dists/s", markerScheme: "https", markerBase: "/u/dists/s", refused: true},
		{name: "displaced signed snapshot", suite: "/u/dists/s",
			snapshot: &history{scheme: "http", suite: "/u/dists/s", form: FormInline, adopted: true}, refused: true},
		{name: "displaced detached snapshot of the twin", suite: "/flat",
			snapshot: &history{scheme: "http", suite: "/flat/.", form: FormDetached, adopted: true}, refused: true},
		{name: "signed snapshot under the other scheme", suite: "/u/dists/s",
			snapshot: &history{scheme: "https", suite: "/u/dists/s", form: FormInline, current: true, adopted: true}, refused: true},
		{name: "retired signed snapshot", suite: "/u/dists/s",
			snapshot: &history{scheme: "http", suite: "/u/dists/s", form: FormInline, adopted: true, retired: true}},
		{name: "signed candidate never adopted", suite: "/u/dists/s",
			snapshot: &history{scheme: "http", suite: "/u/dists/s", form: FormInline}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := openCache(t)
			ctx := context.Background()
			defer stubNow(t, 1_800_000_000)()
			if s := tc.snapshot; s != nil {
				h := seedBlob(t, c, "signed history")
				var id int64
				if s.form == FormDetached {
					g := seedBlob(t, c, "its signature")
					id = seedFormSnapshot(t, c, s.scheme, "h", s.suite, s.form, nil, &h, &g, true)
				} else {
					id = seedFormSnapshot(t, c, s.scheme, "h", s.suite, s.form, &h, nil, nil, true)
				}
				if !s.current {
					if _, err := c.db.Exec(`UPDATE suite_freshness SET current_snapshot_id = NULL WHERE current_snapshot_id = ?`, id); err != nil {
						t.Fatal(err)
					}
				}
				if !s.adopted {
					if _, err := c.db.Exec(`UPDATE suite_snapshot SET adopted_at = NULL WHERE snapshot_id = ?`, id); err != nil {
						t.Fatal(err)
					}
				}
				if s.retired {
					if _, err := c.db.Exec(`UPDATE suite_snapshot SET retired_at = 1 WHERE snapshot_id = ?`, id); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.markerBase != "" {
				if err := c.MarkSignedSeenBatch(ctx, []SignedSeenMark{{CanonicalScheme: tc.markerScheme, CanonicalHost: "h",
					Base: tc.markerBase, SeenAt: 1}}); err != nil {
					t.Fatal(err)
				}
			}
			if early := c.CheckUnsignedDowngrade(ctx, "h", tc.suite); errors.Is(early, ErrUnsignedDowngradeRefused) != tc.refused {
				t.Fatalf("CheckUnsignedDowngrade = %v, want refused %v", early, tc.refused)
			}
			rel := seedBlob(t, c, "unsigned Release")
			id, _, err := c.InsertTrackedCandidateForTest(ctx, SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h",
				SuitePath: tc.suite, InReleaseHash: &rel, Form: FormUnsigned})
			if err != nil {
				t.Fatal(err)
			}
			err = c.CommitAdoption(ctx, id, []SnapshotMember{{SnapshotID: id, Path: "Release", BlobHash: rel, DeclaredSHA256: rel}},
				nil, nil, nil, false)
			if tc.refused != errors.Is(err, ErrUnsignedDowngradeRefused) || (!tc.refused && err != nil) {
				t.Fatalf("commit = %v, want refused %v", err, tc.refused)
			}
			sf, err := c.GetSuiteFreshness(ctx, "http", "h", tc.suite)
			if err != nil {
				t.Fatal(err)
			}
			if committed := sf.CurrentSnapshotID != nil && *sf.CurrentSnapshotID == id; committed == tc.refused {
				t.Fatalf("current snapshot %v, candidate %d: want committed %v", sf.CurrentSnapshotID, id, !tc.refused)
			}
		})
	}
}

// Spec C5 / §7: url_path GC's identity guard (d) keeps the anchor of the
// current snapshot's form — an unsigned suite's <suite>/Release, not the
// <suite>/InRelease its single-anchor slot would suggest. Guards (b)/(c)
// cannot decide here: the rows hold other blobs than the snapshot's.
func TestRunURLPathGCBatch_IdentityGuardFollowsTheAnchorForm(t *testing.T) {
	c := openCache(t)
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()
	anchor := seedBlob(t, c, "the snapshot's anchor")
	old := sql.NullInt64{Int64: now - 30*86400, Valid: true}
	for _, s := range []struct {
		suite string
		form  AnchorForm
	}{{"/u/dists/unsigned", FormUnsigned}, {"/u/dists/inline", FormInline}} {
		seedFormSnapshot(t, c, "http", "h", s.suite, s.form, &anchor, nil, nil, false)
		for _, name := range []string{"/InRelease", "/Release"} {
			putURLPathRow(t, c, "http", "h", s.suite+name, seedBlob(t, c, "served "+s.suite+name), old, true)
		}
	}
	drainURLPathGC(t, c, 100, 7*86400, 0, testMaxVersions)
	for _, w := range []struct {
		path string
		kept bool
	}{
		{"/u/dists/unsigned/Release", true},
		{"/u/dists/unsigned/InRelease", false},
		{"/u/dists/inline/InRelease", true},
		{"/u/dists/inline/Release", false},
	} {
		if got := urlPathExists(t, c, w.path); got != w.kept {
			t.Errorf("%s kept = %v, want %v", w.path, got, w.kept)
		}
	}
}

// Spec C5 safeguard 5 / C10: an unsigned current snapshot never vouches a
// .deb and never counts for strict mode, even if a package_hash row or a
// complete-coverage bit somehow exists for it.
func TestStrictModeReads_IgnoreUnsignedSnapshots(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, 1_800_000_000)()
	rel := seedBlob(t, c, "unsigned Release")
	id := seedFormSnapshot(t, c, "http", "h", "/u/dists/s", FormUnsigned, &rel, nil, nil, true)
	deb := seedBlob(t, c, "a .deb")
	seedPackageHash(t, c, "http", "h", "/u/pool/p.deb", deb, id, "p", "amd64", "1")
	if rows, err := c.DeclaredHashesForPath(ctx, "http", "h", "/u/pool/p.deb"); err != nil || len(rows) != 0 {
		t.Fatalf("DeclaredHashesForPath = %v, %v; want no row from an unsigned snapshot", rows, err)
	}
	if cov, err := c.HostCurrentSnapshotsCoverage(ctx, "http", "h"); err != nil || len(cov) != 0 {
		t.Fatalf("HostCurrentSnapshotsCoverage = %v, %v; want no unsigned snapshot", cov, err)
	}
	// A signed snapshot on the same host is still counted and still vouches.
	in := seedBlob(t, c, "signed InRelease")
	sid := seedFormSnapshot(t, c, "http", "h", "/s/dists/s", FormInline, &in, nil, nil, true)
	seedPackageHash(t, c, "http", "h", "/s/pool/p.deb", deb, sid, "p", "amd64", "1")
	if rows, err := c.DeclaredHashesForPath(ctx, "http", "h", "/s/pool/p.deb"); err != nil || len(rows) != 1 {
		t.Fatalf("DeclaredHashesForPath(signed) = %v, %v; want one row", rows, err)
	}
	if cov, err := c.HostCurrentSnapshotsCoverage(ctx, "http", "h"); err != nil || len(cov) != 1 || cov[0].SnapshotID != sid {
		t.Fatalf("HostCurrentSnapshotsCoverage = %v, %v; want the signed snapshot only", cov, err)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/aptpath/ ./internal/cache/ -run 'TestAnchorPaths|TestSuiteSnapshot_AnchorHash|TestCommitAdoption_UnsignedSeedsItsReleaseAnchor|TestCommitAdoption_RefusesAnUnsignedDowngrade|TestCommitAdoption_RefusesUnsignedForAnySignedHistory|TestRunURLPathGCBatch_IdentityGuardFollowsTheAnchorForm|TestStrictModeReads_IgnoreUnsignedSnapshots'`
Expected: compile failures naming `undefined: AnchorPaths` (aptpath), `s.AnchorHash undefined`, `c.CheckUnsignedDowngrade undefined` and `undefined: ErrUnsignedDowngradeRefused` (cache).

- [ ] **Step 3: Implement**

Create `internal/aptpath/anchor.go`:

```go
package aptpath

// AnchorPaths returns the anchor file(s) of a snapshot of the given anchor
// form, relative to its suite (spec C5 "Anchor forms and storage"):
// "inline" has InRelease, "detached" Release and Release.gpg, "unsigned"
// Release alone. Any other form has none. form is cache.AnchorForm's
// value (aptpath imports nothing outside the standard library).
//
// AIDEV-NOTE: an unsigned snapshot stores its Release hash in the
// single-anchor slot (suite_snapshot.inrelease_hash), so no reader may
// derive an anchor file from the hash columns alone; go through the form.
func AnchorPaths(form string) []string {
	switch form {
	case "inline":
		return []string{"InRelease"}
	case "detached":
		return []string{"Release", "Release.gpg"}
	case "unsigned":
		return []string{"Release"}
	}
	return nil
}
```

In `internal/cache/types.go`, after the `SuiteSnapshot` struct, add:

```go
// AnchorHash returns the hash of the snapshot's anchor file name
// ("InRelease", "Release" or "Release.gpg"), or nil when the snapshot's
// form has no such anchor (aptpath.AnchorPaths).
func (s *SuiteSnapshot) AnchorHash(name string) *string {
	return anchorHash(s.Form, name, s.SingleAnchorHash, s.ReleaseHash, s.ReleaseGPGHash)
}

// anchorHash maps an anchor file of a snapshot of form to the hash column
// that holds it (spec C5): the single-anchor slot holds InRelease for the
// inline form and Release for the unsigned one.
//
// AIDEV-NOTE: the one place the column-to-file mapping lives; never read
// inrelease_hash as "the InRelease" (an unsigned snapshot keeps its
// Release there).
func anchorHash(form AnchorForm, name string, single, release, releaseGPG *string) *string {
	switch {
	case name == "InRelease" && form == FormInline, name == "Release" && form == FormUnsigned:
		return single
	case name == "Release" && form == FormDetached:
		return release
	case name == "Release.gpg" && form == FormDetached:
		return releaseGPG
	}
	return nil
}
```

In `internal/cache/adoption.go`, after `ErrSnapshotRetired`, add:

```go
// ErrUnsignedDowngradeRefused is the never-downgrade refusal of an unsigned
// candidate whose suite has signed history (spec C5 safeguard 3,
// refuseUnsignedDowngrade): CommitAdoption's recheck inside the writer
// transaction returns it with nothing written, and CheckUnsignedDowngrade
// returns it for the early exits. A refusal state, not a failure: the
// adopter records it as unsigned_downgrade_refused.
var ErrUnsignedDowngradeRefused = errors.New("cache: unsigned adoption refused: the suite has signed history")
```

In `CommitAdoption`, right after the `retiredAt.Valid` check (before "Step 2: insert snapshot_member rows"), add Step 1a:

```go
		// Step 1a (spec C5 safeguard 3, never-downgrade): an unsigned
		// candidate is refused, before anything is written, when the
		// suite has signed history (refuseUnsignedDowngrade: a signed-seen
		// marker for its base, or an adopted signed snapshot, under either
		// scheme).
		//
		// AIDEV-NOTE: this in-transaction re-read is the authority; the
		// check's eligibility read and the adopter's are only early exits
		// (CheckUnsignedDowngrade, the same rule). The suite mutex cannot
		// close the race on its own: the handler and the checker write
		// markers in their own writer operations, and the /. twin shares
		// the base without sharing the mutex. The writer is FIFO, so a
		// marker write enqueued before this commit is visible here
		// (TestStoreFetched_MarkerPrecedesALaterWriterOperation; the
		// adopter's re-mark before an unsigned commit, ruling R-E1).
		if AnchorForm(form) == FormUnsigned {
			if err := refuseUnsignedDowngrade(ctx, tx, host, suite); err != nil {
				return err
			}
		}
```

In Step 3c, the comment's first paragraph becomes:

```go
		// Step 3c (freshness-freeze fix, SPEC3 §7.5.1 / SPEC2 §7.4):
		// seed/sync the metadata anchor url_path row(s) — InRelease
		// (inline), Release + Release.gpg (detached) or Release
		// (unsigned) — to point at the blob this snapshot adopted.
		// Historically adoption left the anchor's blob_hash pointing at
		// whatever bytes a client cache-miss last stored, so it diverged
		// from the snapshot's anchor hash and the SPEC4 §5 GC guards
		// (b)/(c) could never vouch for it; the anchor was then reaped on
		// a low-traffic lull and the suite froze (the checker silently
		// skips when the anchor is absent, SPEC2 §7.4). Syncing it here
		// keeps the hash-based guards matching — the SPEC4 §5 identity
		// guard (d) is the belt to this suspenders.
```

Then replace

```go
		// AIDEV-TODO(phase E): an unsigned snapshot stores its Release
		// hash in inrelease_hash; guard by anchor_form.
		var anchorInRel, anchorRel, anchorRelGPG sql.NullString
		if err := tx.QueryRowContext(ctx, `
SELECT inrelease_hash, release_hash, release_gpg_hash
  FROM suite_snapshot WHERE snapshot_id = ?`, snapshotID).
			Scan(&anchorInRel, &anchorRel, &anchorRelGPG); err != nil {
			return fmt.Errorf("CommitAdoption: read anchor hashes: %w", err)
		}
		seedAnchor := func(filename string, h sql.NullString) error {
			if !h.Valid || h.String == "" {
				return nil
			}
			anchorPath := suite + filename
```

with

```go
		// AIDEV-NOTE: the files seeded are the form's anchors
		// (aptpath.AnchorPaths, anchorHash): an unsigned snapshot keeps
		// its Release hash in inrelease_hash, and its anchor is
		// <suite>/Release — seeding <suite>/InRelease with it would serve
		// a Release body as an InRelease (spec C5, phase E).
		var anchorInRel, anchorRel, anchorRelGPG *string
		if err := tx.QueryRowContext(ctx, `
SELECT inrelease_hash, release_hash, release_gpg_hash
  FROM suite_snapshot WHERE snapshot_id = ?`, snapshotID).
			Scan(&anchorInRel, &anchorRel, &anchorRelGPG); err != nil {
			return fmt.Errorf("CommitAdoption: read anchor hashes: %w", err)
		}
		seedAnchor := func(name string) error {
			h := anchorHash(AnchorForm(form), name, anchorInRel, anchorRel, anchorRelGPG)
			if h == nil || *h == "" {
				return nil
			}
			anchorPath := suite + "/" + name
```

and, at the end of the same block, replace `scheme, host, anchorPath, h.String,` with `scheme, host, anchorPath, *h,` and the three `seedAnchor("/InRelease", …)`, `seedAnchor("/Release", …)`, `seedAnchor("/Release.gpg", …)` calls with

```go
		for _, name := range aptpath.AnchorPaths(form) {
			if err := seedAnchor(name); err != nil {
				return fmt.Errorf("CommitAdoption: seed %s anchor: %w", name, err)
			}
		}
```

Before `decrementSnapshotRefcountsTx`, add the rule and its reader outside a transaction:

```go
// rowQuerier is what refuseUnsignedDowngrade reads through: CommitAdoption's
// writer transaction, or the read pool.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// CheckUnsignedDowngrade is spec C5's never-downgrade rule
// (refuseUnsignedDowngrade) read outside a transaction, for the early
// exits: the check's before an unsigned request takes a slot, the
// adopter's before anything is downloaded. CommitAdoption re-reads the same
// rule inside its writer transaction; that read is the authority.
func (c *Cache) CheckUnsignedDowngrade(ctx context.Context, host, suite string) error {
	return refuseUnsignedDowngrade(ctx, c.db, host, suite)
}

// refuseUnsignedDowngrade is the never-downgrade rule for an unsigned
// candidate of suite (spec C5 safeguard 3): ErrUnsignedDowngradeRefused when
// the suite has signed history on host under either scheme —
//   - a signed-seen marker for its base (aptpath.MarkerBase: the /. twin
//     shares it), or
//   - an adopted, non-retired inline or detached snapshot of the suite or
//     its twin: the current snapshot, or a displaced one the legacy trust
//     audit left queued, and so unmarked, after an I/O fault.
//
// A retired snapshot (quarantined by the audit, or reaped) is no history:
// its trust was removed. A read error is returned as it is.
//
// AIDEV-NOTE: one rule and one SQL at both never-downgrade points, so the
// early exits can never admit what the commit refuses. Either scheme counts
// (design review M-7): a base seen signed over https protects the same base
// over plain http, the scheme a man-in-the-middle can strip. The displaced
// snapshot clause is design review M-8.
func refuseUnsignedDowngrade(ctx context.Context, q rowQuerier, host, suite string) error {
	base := aptpath.MarkerBase(suite)
	var scheme string
	err := q.QueryRowContext(ctx, `
SELECT canonical_scheme FROM suite_signed_seen
 WHERE canonical_scheme IN ('http', 'https') AND canonical_host = ? AND base = ?
 LIMIT 1`, host, base).Scan(&scheme)
	switch {
	case err == nil:
		return fmt.Errorf("%w: %s://%s%s has a signed-seen marker", ErrUnsignedDowngradeRefused, scheme, host, base)
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("unsigned downgrade check: read signed-seen markers: %w", err)
	}
	var path, form string
	err = q.QueryRowContext(ctx, `
SELECT canonical_scheme, suite_path, anchor_form FROM suite_snapshot
 WHERE canonical_scheme IN ('http', 'https') AND canonical_host = ? AND suite_path IN (?, ?)
   AND adopted_at IS NOT NULL AND retired_at IS NULL AND anchor_form IN ('inline', 'detached')
 LIMIT 1`, host, base, base+"/.").Scan(&scheme, &path, &form)
	switch {
	case err == nil:
		return fmt.Errorf("%w: %s://%s%s has an adopted %s snapshot", ErrUnsignedDowngradeRefused, scheme, host, path, form)
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("unsigned downgrade check: read signed snapshots: %w", err)
	}
	return nil
}
```

In `DeclaredHashesForPath`, the query becomes (with the AIDEV-NOTE above it):

```go
	// AIDEV-NOTE: spec C5 safeguard 5 — an unsigned snapshot never vouches
	// a .deb. Adoption builds no package_hash rows for one; the form
	// filter keeps any that exist out (defence in depth).
	const q = `
SELECT DISTINCT p.declared_sha256, p.snapshot_id, p.package_name, sf.suite_path
  FROM package_hash p
  JOIN suite_freshness sf ON sf.current_snapshot_id = p.snapshot_id
  JOIN suite_snapshot ss  ON ss.snapshot_id = p.snapshot_id
 WHERE p.canonical_scheme = ?
   AND p.canonical_host   = ?
   AND p.path             = ?
   AND ss.anchor_form    <> 'unsigned'`
```

In `internal/cache/queries.go`, `HostCurrentSnapshotsCoverage`'s query gains `   AND ss.anchor_form     <> 'unsigned'` after `AND ss.canonical_host   = ?`, with this note above the query:

```go
	// AIDEV-NOTE: spec C5/C10 — unsigned snapshots never count for strict
	// mode (they vouch nothing), so they are not "snapshots in force" here.
```

In `internal/cache/gc_urlpath.go` (`urlPathRetainedTx`), append to the comment above `var guarded int`:

```go
	//
	// AIDEV-NOTE: guard (d) keys the anchor path on the snapshot's form
	// (aptpath.AnchorPaths), never on which hash column is set: an unsigned
	// snapshot keeps its Release hash in inrelease_hash, and its anchor is
	// <suite>/Release (spec C5; phase E resolved the AIDEV-TODO that sat in
	// the SQL, deferred minor A-29).
```

and replace guard (d)'s last lines

```go
             WHERE sf.canonical_scheme = ? AND sf.canonical_host = ?
               -- AIDEV-TODO(phase E): an unsigned snapshot stores its
               -- Release hash in inrelease_hash; guard by anchor_form.
               AND ((ss.inrelease_hash IS NOT NULL AND ? = sf.suite_path || '/InRelease')
                 OR (ss.release_hash   IS NOT NULL AND ? IN (sf.suite_path || '/Release', sf.suite_path || '/Release.gpg'))))`,
		blobHash, blobHash, blobHash, blobHash, scheme, host, path, path).Scan(&guarded)
```

with

```go
             WHERE sf.canonical_scheme = ? AND sf.canonical_host = ?
               AND ((ss.anchor_form = 'inline'   AND ? = sf.suite_path || '/InRelease')
                 OR (ss.anchor_form = 'detached' AND ? IN (sf.suite_path || '/Release', sf.suite_path || '/Release.gpg'))
                 OR (ss.anchor_form = 'unsigned' AND ? = sf.suite_path || '/Release')))`,
		blobHash, blobHash, blobHash, blobHash, scheme, host, path, path, path).Scan(&guarded)
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/aptpath/ ./internal/cache/`
Expected: PASS (the new tests and every existing one).

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| Step 1a's condition `AnchorForm(form) == FormUnsigned` → `false && …` | `TestCommitAdoption_RefusesAnUnsignedDowngrade/{marker_for_the_base, marker_for_the_twin's_base, signed_current_snapshot, detached_current_snapshot}`, `TestCommitAdoption_RefusesUnsignedForAnySignedHistory/{marker_under_the_other_scheme, displaced_signed_snapshot, displaced_detached_snapshot_of_the_twin, signed_snapshot_under_the_other_scheme}` |
| `refuseUnsignedDowngrade`: `base := aptpath.MarkerBase(suite)` → `base := suite` | `TestCommitAdoption_RefusesAnUnsignedDowngrade/marker_for_the_twin's_base` |
| the snapshot clause finds nothing (`anchor_form IN ('inline', 'detached')` → `IN ('none')`) | `TestCommitAdoption_RefusesAnUnsignedDowngrade/{signed_current_snapshot, detached_current_snapshot}`, `TestCommitAdoption_RefusesUnsignedForAnySignedHistory/{displaced_signed_snapshot, displaced_detached_snapshot_of_the_twin, signed_snapshot_under_the_other_scheme}` |
| the marker read keyed by scheme (`canonical_scheme IN ('http', 'https')` → `IN ('http')`) | `…ForAnySignedHistory/marker_under_the_other_scheme` |
| the snapshot read keyed by scheme (the same edit in its query) | `…ForAnySignedHistory/signed_snapshot_under_the_other_scheme` |
| the snapshot clause without `retired_at IS NULL` | `…ForAnySignedHistory/retired_signed_snapshot` |
| the snapshot clause without `adopted_at IS NOT NULL` | `…ForAnySignedHistory/signed_candidate_never_adopted` |
| the snapshot clause without the twin (`base, base+"/."` → `base, base`) | `…ForAnySignedHistory/displaced_detached_snapshot_of_the_twin` |
| `CheckUnsignedDowngrade` returns nil | `TestCommitAdoption_RefusesAnUnsignedDowngrade/{marker_for_the_base, marker_for_the_twin's_base, signed_current_snapshot, detached_current_snapshot}`, `…ForAnySignedHistory/{the four refused legs}` |
| Step 3c: `anchorHash(AnchorForm(form), …)` → `anchorHash(FormInline, …)` | `TestCommitAdoption_UnsignedSeedsItsReleaseAnchor` |
| guard (d): the unsigned arm's `'/Release'` → `'/InRelease'` | `TestRunURLPathGCBatch_IdentityGuardFollowsTheAnchorForm` |
| drop `AND ss.anchor_form <> 'unsigned'` from `DeclaredHashesForPath`, then (separately) from `HostCurrentSnapshotsCoverage` | `TestStrictModeReads_IgnoreUnsignedSnapshots` (each) |
| `anchorHash`: drop `name == "Release" && form == FormUnsigned` | `TestSuiteSnapshot_AnchorHash` |

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/aptpath/ internal/cache/types.go internal/cache/adoption.go internal/cache/queries.go internal/cache/gc_urlpath.go internal/cache/unsigned_form_test.go
git add internal/aptpath/anchor.go internal/aptpath/anchor_test.go internal/cache/
git commit -m "Store the unsigned anchor form correctly and refuse an unsigned downgrade inside CommitAdoption"
```

(Record the mutation table in the commit message. `gofmt -l` must print nothing: none of these files is a drift file.)

### Task 2: SHA512 hash blocks and the shared member verifier

Spec C5 "Hash blocks": `ParseRelease` merges hash blocks per member path, as apt does; `ReleaseMember` carries `SHA256` and `SHA512`; the size comes from the SHA512 entry if present, else SHA256; entries with only MD5/SHA1 are ignored. One shared helper, used by `runShared`, reconcile and repair (all three fetch through `adoptMember`), verifies the fetched bytes against **every** declared SHA256/SHA512 hash, returns the SHA256 of the verified bytes (a *derived* anchor for SHA512-only members) and the by-hash alias paths for every declared algorithm; the adopter probes by-hash with the strongest declared hash (spec §3, apt's rule). Limitation (spec, documented in Task 15): integrity-skipped SHA512-only members are not recorded for skip-repair; reconcile still heals requestable IndexTargets. Spec §9's E row and the overview's C carry-forward: retained by-hash already serves SHA512 aliases from displaced snapshots (phase C plan, deviation 3), so E only writes the alias rows — the DoD test here serves one from the current and one from a displaced snapshot of real adoptions. The nvidia-shaped DoD fixture adopts here (its flat layout and `./` Filenames are phase G's).

Design notes 3–7 record the choices: SHA512 is checked from a tee before the bytes reach pool/ and SHA256 through `FinalizeExpectingHash`, so the plain-path fetch no longer leaves mismatched bytes in the pool as a stray blob; a SHA512-only member is never reused from the pool; a pool blob whose SHA256 matches but whose SHA512 does not fails the member (the Release disagrees with itself) and is kept; a malformed or conflicting SHA512 entry fails the parse like a SHA256 one; repair reads the snapshot's Release (the trust anchor) to recover the SHA512 the skip record does not keep.

**Existing tests that change:** `TestParseRelease_LargeRealistic` repeated one path 30,000 times as a scale shortcut; identical repeated entries are now one member (the per-path merge), so it uses 30,000 distinct paths.

**Files:**
- Create: `internal/freshness/member_verify.go`, `internal/freshness/hash_blocks_test.go`, `internal/handler/byhash_sha512_test.go`
- Modify: `internal/freshness/release_parse.go`, `internal/freshness/release_parse_test.go`, `internal/freshness/adoption.go` (`adoptMember`, `tryByHashFetch`, `fetchMemberURL`, `runShared` Steps 5 and 7, `byHashAliasPath`), `internal/freshness/reconcile.go`, `internal/freshness/repair.go`

**Interfaces:**
- Consumes: Task 1 (none directly). Test helpers `newAdoptionTestEnv`, `fakePackagesStanzas`, `shaOf`, `passThroughVerifier`, `discardLogger`, `testHex64`, `sha` (freshness); `memberStore`, `plainVerifier`, `noUpstream`, `flipSimHost`, `silentLogger`, `hdrXCacheSnapshot` (handler); `simtest.ClearsignedFixture`, `simtest.OpenCache`; `cache.(*Cache).DeleteSnapshotMembersForTest`, `ListRepairableSkippedMembers`.
- Produces: `ReleaseMember.SHA512`; `memberWriter`, `newMemberWriter(*cache.Cache) (*memberWriter, error)`, `verifyMember(w *memberWriter, m ReleaseMember, size int64) (string, error)`, `*memberHashMismatch`, `hashFileDigests(path) (sha256, sha512 string, err error)`, `byHashPath(declaredPath, algo, hex string) string`, `byHashAliases(m) []string`, `byHashProbePath(m) string`, `aliasRows(snapshotID, m, blob) []cache.SnapshotMember`, `memberRows(snapshotID, m, blob) []cache.SnapshotMember`, `verifiedMember(m, blob) ReleaseMember`, `(*Adopter).repairDeclarations(ctx, suite, snapshotID) map[string]ReleaseMember` (all in package freshness); test helpers `sha512Of(b []byte) string`, `hashRelease(header string, with256, with512 bool, members map[string][]byte, forge map[string]string) []byte`, `snapshotPaths(t, c, id) map[string]string`, `currentID(t, c, suite) int64` and the constant `noble` (`hash_blocks_test.go`).

- [ ] **Step 1: Write the failing tests**

Create `internal/freshness/hash_blocks_test.go`:

```go
package freshness

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

func sha512Of(b []byte) string {
	s := sha512.Sum512(b)
	return hex.EncodeToString(s[:])
}

// hashRelease builds Release text for members (path → content) with a
// SHA256 block, a SHA512 block, or both (spec C5), in path order, after
// the given header lines. forge maps a path to a block ("SHA256" or
// "SHA512") whose entry declares another content's hash.
func hashRelease(header string, with256, with512 bool, members map[string][]byte, forge map[string]string) []byte {
	paths := make([]string, 0, len(members))
	for p := range members {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var b strings.Builder
	b.WriteString(header)
	block := func(name string, sum func([]byte) string) {
		b.WriteString(name + ":\n")
		for _, p := range paths {
			body := members[p]
			if forge[p] == name {
				body = []byte("forged " + p)
			}
			fmt.Fprintf(&b, " %s %d %s\n", sum(body), len(members[p]), p)
		}
	}
	if with256 {
		block("SHA256", shaOf)
	}
	if with512 {
		block("SHA512", sha512Of)
	}
	return []byte(b.String())
}

// Spec C5 "Hash blocks": ParseRelease merges the SHA256 and SHA512 blocks
// per member path, as apt does; the size comes from the SHA512 entry when
// there is one; members declared only in MD5Sum/SHA1 are not returned.
func TestParseRelease_MergesHashBlocks(t *testing.T) {
	h256, h512 := sha(1), strings.Repeat("a", 128)
	o512 := strings.Repeat("b", 128)
	body := "Origin: x\n" +
		"MD5Sum:\n 0123456789abcdef0123456789abcdef 9 md5/only\n" +
		"SHA512:\n" +
		" " + h512 + " 20 both/Packages\n" +
		" " + o512 + " 30 only512/Packages\n" +
		"SHA256:\n" +
		" " + h256 + " 10 both/Packages\n" +
		" " + sha(2) + " 40 only256/Packages\n"
	got, err := ParseRelease([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := []ReleaseMember{
		{Path: "both/Packages", SHA256: h256, SHA512: h512, Size: 20},
		{Path: "only512/Packages", SHA512: o512, Size: 30},
		{Path: "only256/Packages", SHA256: sha(2), Size: 40},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("members = %+v\nwant %+v", got, want)
	}
	// A SHA512 block alone is a complete Release (libnvidia-container).
	got, err = ParseRelease([]byte("SHA512:\n " + o512 + " 30 main/binary-amd64/Packages\n"))
	if err != nil || len(got) != 1 || got[0].SHA512 != o512 || got[0].SHA256 != "" {
		t.Fatalf("SHA512-only Release = %+v, %v", got, err)
	}
}

// A malformed SHA512 entry fails the parse like a malformed SHA256 one,
// and so does a member declared twice in one block with different values;
// a repeated identical entry is one member.
func TestParseRelease_SHA512BlockErrors(t *testing.T) {
	good := strings.Repeat("c", 128)
	for _, c := range []struct {
		name, body, want string
	}{
		{"short hash", "SHA512:\n " + strings.Repeat("c", 64) + " 1 p\n", "invalid sha512"},
		{"upper case", "SHA512:\n " + strings.ToUpper(good) + " 1 p\n", "invalid sha512"},
		{"conflicting duplicate", "SHA512:\n " + good + " 1 p\n " + good + " 2 p\n", "declared twice"},
		{"conflicting SHA256 duplicate", "SHA256:\n " + sha(1) + " 1 p\n " + sha(2) + " 1 p\n", "declared twice"},
	} {
		if _, err := ParseRelease([]byte(c.body)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
	got, err := ParseRelease([]byte("SHA512:\n " + good + " 1 p\n " + good + " 1 p\n"))
	if err != nil || len(got) != 1 {
		t.Fatalf("identical duplicate = %+v, %v; want one member", got, err)
	}
}

// The shared verifier (spec C5): every declared hash must match; a
// SHA512-only member yields the SHA-256 of its bytes; mismatched bytes
// never reach pool/.
func TestVerifyMember(t *testing.T) {
	body := []byte("Package: p\n")
	other := []byte("other bytes")
	for _, c := range []struct {
		name     string
		m        ReleaseMember
		mismatch string // the algorithm that fails, "" for none
		noHash   bool
	}{
		{"sha256 only", ReleaseMember{Path: "p", SHA256: shaOf(body)}, "", false},
		{"sha512 only", ReleaseMember{Path: "p", SHA512: sha512Of(body)}, "", false},
		{"both", ReleaseMember{Path: "p", SHA256: shaOf(body), SHA512: sha512Of(body)}, "", false},
		{"sha256 wrong", ReleaseMember{Path: "p", SHA256: shaOf(other), SHA512: sha512Of(body)}, "SHA256", false},
		{"sha512 wrong", ReleaseMember{Path: "p", SHA256: shaOf(body), SHA512: sha512Of(other)}, "SHA512", false},
		{"sha512-only wrong", ReleaseMember{Path: "p", SHA512: sha512Of(other)}, "SHA512", false},
		{"no hash", ReleaseMember{Path: "p"}, "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := newAdoptionTestEnv(t)
			w, err := newMemberWriter(env.cache)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write(body); err != nil {
				t.Fatal(err)
			}
			got, err := verifyMember(w, c.m, int64(len(body)))
			exists, xerr := env.cache.BlobExists(shaOf(body))
			if xerr != nil {
				t.Fatal(xerr)
			}
			var mm *memberHashMismatch
			switch {
			case c.noHash:
				if err == nil || exists {
					t.Fatalf("no declared hash: err %v, in pool %v; want an error and nothing in the pool", err, exists)
				}
			case c.mismatch != "":
				if !errors.As(err, &mm) || mm.algo != c.mismatch || exists {
					t.Fatalf("err %v (in pool %v); want a %s mismatch and nothing in the pool", err, exists, c.mismatch)
				}
			default:
				if err != nil || got != shaOf(body) || !exists {
					t.Fatalf("got %q, %v (in pool %v); want the bytes' SHA-256 and the blob in the pool", got, err, exists)
				}
			}
		})
	}
}

// Truncate restarts the SHA-512 too (a resumed fetch that starts over).
func TestMemberWriter_TruncateRestartsBothHashes(t *testing.T) {
	env := newAdoptionTestEnv(t)
	w, err := newMemberWriter(env.cache)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("the real bytes")
	if _, err := w.Write([]byte("a partial, stale ")); err != nil {
		t.Fatal(err)
	}
	if err := w.Truncate(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if got, err := verifyMember(w, ReleaseMember{Path: "p", SHA512: sha512Of(body)}, int64(len(body))); err != nil || got != shaOf(body) {
		t.Fatalf("after Truncate: %q, %v; want the real bytes verified", got, err)
	}
}

func TestByHashAliasesAndProbe(t *testing.T) {
	h256, h512 := sha(1), strings.Repeat("d", 128)
	m := ReleaseMember{Path: "main/binary-amd64/Packages.xz", SHA256: h256, SHA512: h512}
	want := []string{"main/binary-amd64/by-hash/SHA256/" + h256, "main/binary-amd64/by-hash/SHA512/" + h512}
	if got := byHashAliases(m); !slices.Equal(got, want) {
		t.Errorf("byHashAliases = %q, want %q", got, want)
	}
	if got := byHashProbePath(m); got != want[1] {
		t.Errorf("probe = %q, want the strongest hash's %q", got, want[1])
	}
	root := ReleaseMember{Path: "Contents-amd64", SHA512: h512}
	if got := byHashAliases(root); !slices.Equal(got, []string{"by-hash/SHA512/" + h512}) {
		t.Errorf("a root member's aliases = %q", got)
	}
	if got := byHashProbePath(ReleaseMember{Path: "a/b", SHA256: h256}); got != "a/by-hash/SHA256/"+h256 {
		t.Errorf("SHA256-only probe = %q", got)
	}
}

const noble = "http://archive.ubuntu.com/ubuntu/dists/noble/"

// snapshotPaths returns the snapshot's member paths mapped to their blobs.
func snapshotPaths(t *testing.T, c *cache.Cache, id int64) map[string]string {
	t.Helper()
	rows, err := c.ListSnapshotMembers(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		if r.DeclaredSHA256 != r.BlobHash {
			t.Errorf("member %s declares %s for blob %s", r.Path, r.DeclaredSHA256, r.BlobHash)
		}
		out[r.Path] = r.BlobHash
	}
	return out
}

func currentID(t *testing.T, c *cache.Cache, s SuiteRef) int64 {
	t.Helper()
	sf, err := c.GetSuiteFreshness(context.Background(), s.CanonicalScheme, s.CanonicalHost, s.SuitePath)
	if err != nil || sf.CurrentSnapshotID == nil {
		t.Fatalf("no current snapshot: %+v, %v", sf, err)
	}
	return *sf.CurrentSnapshotID
}

// Phase E DoD (spec C5, §7): an nvidia-shaped suite — InRelease only, a
// Release text with only a SHA512 block, a Date frozen in 2018, a Packages
// index with SHA256 Filenames — adopts: its Packages member is bound to
// the SHA-256 derived from the verified bytes, it is aliased by SHA512
// only, and its .debs are vouched. (Its flat layout and "./" Filenames are
// phase G's.)
func TestAdopter_SHA512OnlyReleaseAdopts(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ctx := context.Background()
	deb := shaOf([]byte("a .deb"))
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/libnvidia-container1_1.17.0-1_amd64.deb": deb})
	env.fetcher.put(noble+"main/binary-amd64/Packages", pkgs)
	rel := hashRelease("Origin: https://nvidia.github.io/libnvidia-container/stable/deb/amd64\nDate: Thu, 05 Apr 2018 16:50:03 +0000\n",
		false, true, map[string][]byte{"main/binary-amd64/Packages": pkgs}, nil)
	if err := env.adopter.Run(ctx, env.suite, simtest.ClearsignedFixture(string(rel)), "", ""); err != nil {
		t.Fatal(err)
	}
	id := currentID(t, env.cache, env.suite)
	paths := snapshotPaths(t, env.cache, id)
	derived := shaOf(pkgs)
	if paths["main/binary-amd64/Packages"] != derived {
		t.Fatalf("Packages member = %q, want the derived SHA-256 %s; members %v", paths["main/binary-amd64/Packages"], derived, paths)
	}
	if paths["main/binary-amd64/by-hash/SHA512/"+sha512Of(pkgs)] != derived {
		t.Fatalf("no SHA512 alias bound to the Packages blob; members %v", paths)
	}
	if _, ok := paths["main/binary-amd64/by-hash/SHA256/"+derived]; ok {
		t.Fatalf("a SHA256 alias for a member the Release declares only by SHA512; members %v", paths)
	}
	rows, err := env.cache.DeclaredHashesForPath(ctx, "http", "archive.ubuntu.com", "/ubuntu/pool/main/libnvidia-container1_1.17.0-1_amd64.deb")
	if err != nil || len(rows) != 1 || rows[0].DeclaredSHA256 != deb {
		t.Fatalf("the .deb is not vouched: %v, %v", rows, err)
	}
	snap, err := env.cache.GetSuiteSnapshot(ctx, id)
	if err != nil || snap.ReleaseDate == nil || *snap.ReleaseDate != 1522947003 {
		t.Fatalf("release_date = %v (%v), want the frozen 2018 Date", snap.ReleaseDate, err)
	}
}

// Spec C5: the fetched bytes are verified against every declared hash: a
// dual-hash Release whose SHA512 (or SHA256) entry is forged fails the
// member — an IndexTarget, so the adoption — and the bytes never reach
// the pool; when both match, the member is aliased under both algorithms.
func TestAdopter_VerifiesEveryDeclaredHash(t *testing.T) {
	for _, forged := range []string{"SHA512", "SHA256", ""} {
		t.Run("forged "+forged, func(t *testing.T) {
			env := newAdoptionTestEnv(t)
			pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo_1.deb": testHex64})
			env.fetcher.put(noble+"main/binary-amd64/Packages", pkgs)
			var forge map[string]string
			if forged != "" {
				forge = map[string]string{"main/binary-amd64/Packages": forged}
			}
			rel := hashRelease("Origin: x\n", true, true, map[string][]byte{"main/binary-amd64/Packages": pkgs}, forge)
			err := env.adopter.Run(context.Background(), env.suite, simtest.ClearsignedFixture(string(rel)), "", "")
			if forged == "" {
				if err != nil {
					t.Fatal(err)
				}
				paths := snapshotPaths(t, env.cache, currentID(t, env.cache, env.suite))
				for _, alias := range []string{"by-hash/SHA256/" + shaOf(pkgs), "by-hash/SHA512/" + sha512Of(pkgs)} {
					if paths["main/binary-amd64/"+alias] != shaOf(pkgs) {
						t.Errorf("alias %s missing; members %v", alias, paths)
					}
				}
				return
			}
			if !errors.Is(err, ErrAdoptionMemberMismatch) {
				t.Fatalf("Run = %v, want a member mismatch", err)
			}
			if !strings.Contains(err.Error(), "main/binary-amd64/Packages") {
				t.Fatalf("Run = %v, want the member named", err)
			}
			if ok, _ := env.cache.BlobExists(shaOf(pkgs)); ok {
				t.Fatal("bytes that failed a declared hash reached the pool")
			}
		})
	}
}

// Spec §3/C5: with Acquire-By-Hash the adopter probes by-hash with the
// strongest declared hash, as apt requests it.
func TestAdopter_ByHashProbeUsesTheStrongestHash(t *testing.T) {
	env := newAdoptionTestEnv(t)
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo_1.deb": testHex64})
	byHash := noble + "main/binary-amd64/by-hash/SHA512/" + sha512Of(pkgs)
	env.fetcher.put(byHash, pkgs)
	rel := hashRelease("Origin: x\nAcquire-By-Hash: yes\n", true, true, map[string][]byte{"main/binary-amd64/Packages": pkgs}, nil)
	if err := env.adopter.Run(context.Background(), env.suite, simtest.ClearsignedFixture(string(rel)), "", ""); err != nil {
		t.Fatal(err)
	}
	if got := env.fetcher.requested(); len(got) != 1 || got[0] != byHash {
		t.Fatalf("fetched %q, want only the SHA512 by-hash URL", got)
	}
}

// Spec C5 / §7: reconcile heals a SHA512-only IndexTarget into the
// snapshot with its SHA512 alias, through the same verifier.
func TestReconcile_HealsASHA512OnlyMember(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ctx := context.Background()
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo_1.deb": testHex64})
	env.fetcher.put(noble+"main/binary-all/Packages", pkgs)
	rel := hashRelease("Origin: x\n", false, true, map[string][]byte{"main/binary-all/Packages": pkgs}, nil)
	if err := env.adopter.Run(ctx, env.suite, simtest.ClearsignedFixture(string(rel)), "", ""); err != nil {
		t.Fatal(err)
	}
	id := currentID(t, env.cache, env.suite)
	alias := "main/binary-all/by-hash/SHA512/" + sha512Of(pkgs)
	if err := env.cache.DeleteSnapshotMembersForTest(ctx, id, "main/binary-all/Packages", alias); err != nil {
		t.Fatal(err)
	}
	healed, err := env.adopter.ReconcileSnapshot(ctx, env.suite, id, true)
	if err != nil || healed != 1 {
		t.Fatalf("ReconcileSnapshot = %d, %v; want one member healed", healed, err)
	}
	paths := snapshotPaths(t, env.cache, id)
	if paths["main/binary-all/Packages"] != shaOf(pkgs) || paths[alias] != shaOf(pkgs) {
		t.Fatalf("after reconcile: members %v; want the Packages member and its SHA512 alias", paths)
	}
}

// Spec C5: an integrity-skipped member of a dual-hash suite is repaired
// with an alias for both declared algorithms (the record holds only the
// SHA-256; the snapshot's Release adds the SHA512); a SHA512-only member
// that fails integrity is skipped without a repair record (documented
// limitation).
func TestRepair_UsesEveryDeclaredHash(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ctx := context.Background()
	ad, err := NewAdopter(AdoptionConfig{Cache: env.cache, Fetcher: env.fetcher, Verifier: passThroughVerifier{},
		HostLimiter: hostsem.New(8), TolerateOptionalMemberFailures: true, RepairSkippedMembers: true,
		Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo_1.deb": testHex64})
	contents := []byte("Contents of the archive\n")
	translation := []byte("Package: foo\nDescription-en: foo\n")
	env.fetcher.put(noble+"main/binary-amd64/Packages", pkgs)
	env.fetcher.put(noble+"main/Contents-amd64", []byte("a mirror mid-sync")) // fails both hashes
	env.fetcher.put(noble+"main/i18n/Translation-en", []byte("stale"))
	dual := hashRelease("Origin: x\n", true, true, map[string][]byte{
		"main/binary-amd64/Packages": pkgs, "main/Contents-amd64": contents}, nil)
	// The SHA512-only member is declared in a second SHA512 block.
	rel := append(dual, []byte("SHA512:\n "+sha512Of(translation)+fmt.Sprintf(" %d main/i18n/Translation-en\n", len(translation)))...)
	if err := ad.Run(ctx, env.suite, simtest.ClearsignedFixture(string(rel)), "", ""); err != nil {
		t.Fatal(err)
	}
	id := currentID(t, env.cache, env.suite)
	skips, err := env.cache.ListRepairableSkippedMembers(ctx, id)
	if err != nil || len(skips) != 1 || skips[0].Path != "main/Contents-amd64" {
		t.Fatalf("repair records = %+v, %v; want Contents-amd64 only (no record for the SHA512-only member)", skips, err)
	}
	env.fetcher.put(noble+"main/Contents-amd64", contents) // the mirror synced
	ad.RepairSkippedMembers(ctx, env.suite, id)
	paths := snapshotPaths(t, env.cache, id)
	for _, p := range []string{"main/Contents-amd64", "main/by-hash/SHA256/" + shaOf(contents), "main/by-hash/SHA512/" + sha512Of(contents)} {
		if paths[p] != shaOf(contents) {
			t.Errorf("after repair %s = %q; members %v", p, paths[p], paths)
		}
	}
}

// A pooled blob whose bytes match the declared SHA-256 but not the
// declared SHA-512 — a Release whose two declarations disagree — fails
// the member and is kept in the pool (it is the SHA-256's true content).
func TestAdopter_PoolReuseChecksTheSHA512(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ctx := context.Background()
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo_1.deb": testHex64})
	if _, err := env.adopter.writeBlobBytes(ctx, pkgs); err != nil {
		t.Fatal(err)
	}
	rel := hashRelease("Origin: x\n", true, true, map[string][]byte{"main/binary-amd64/Packages": pkgs},
		map[string]string{"main/binary-amd64/Packages": "SHA512"})
	err := env.adopter.Run(ctx, env.suite, simtest.ClearsignedFixture(string(rel)), "", "")
	if !errors.Is(err, ErrAdoptionMemberMismatch) || !strings.Contains(err.Error(), "SHA512") {
		t.Fatalf("Run = %v, want a SHA512 member mismatch", err)
	}
	if ok, _ := env.cache.BlobExists(shaOf(pkgs)); !ok {
		t.Fatal("the pooled blob was evicted; it is the declared SHA-256's true content")
	}
	if n := len(env.fetcher.requested()); n != 0 {
		t.Fatalf("fetched %d times, want none (a fetch brings the same bytes)", n)
	}
}
```

Create `internal/handler/byhash_sha512_test.go`:

```go
package handler

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// Phase E DoD (spec C5, C8): an adoption of a Release that declares SHA256
// and SHA512 writes both by-hash aliases, and the handler serves the
// SHA512 one apt requests — from the current snapshot and, once the next
// adoption has displaced it, from the displaced snapshot (retained by-hash).
func TestServeHTTP_SHA512ByHashFromAnAdoption(t *testing.T) {
	ctx := context.Background()
	c := simtest.OpenCache(t)
	members := &memberStore{bodies: map[string][]byte{}}
	ad, err := freshness.NewAdopter(freshness.AdoptionConfig{Cache: c, Fetcher: members, Verifier: plainVerifier{},
		HostLimiter: hostsem.New(8), Logger: simtest.DiscardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	parser, err := proxy.New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{Parser: parser, Cache: c, Fetch: noUpstream{}, Logger: silentLogger(),
		HostLimiter: hostsem.New(8), KeepDisplaced: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	const suite = "/r/dists/x"
	if err := c.EnsureSuiteForTest(ctx, "http", flipSimHost, suite); err != nil {
		t.Fatal(err)
	}
	ref := freshness.SuiteRef{CanonicalScheme: "http", CanonicalHost: flipSimHost, SuitePath: suite}
	// adopt publishes generation gen and adopts it; it returns the
	// Packages bytes, their SHA512 alias path and the snapshot id.
	adopt := func(gen int) ([]byte, string, string) {
		pkgs := []byte(fmt.Sprintf("Package: p\nArchitecture: amd64\nVersion: %d\nFilename: pool/p_%d_amd64.deb\nSize: 1\nSHA256: %064x\n\n",
			gen, gen, gen))
		members.put("http://"+flipSimHost+suite+"/main/binary-amd64/Packages", pkgs)
		s256, s512 := sha256.Sum256(pkgs), sha512.Sum512(pkgs)
		rel := fmt.Sprintf("Label: generation %d\nSHA256:\n %x %d main/binary-amd64/Packages\nSHA512:\n %x %d main/binary-amd64/Packages\n",
			gen, s256, len(pkgs), s512, len(pkgs))
		if err := ad.Run(ctx, ref, simtest.ClearsignedFixture(rel), "", ""); err != nil {
			t.Fatal(err)
		}
		sf, err := c.GetSuiteFreshness(ctx, "http", flipSimHost, suite)
		if err != nil || sf.CurrentSnapshotID == nil {
			t.Fatalf("generation %d not adopted: %+v, %v", gen, sf, err)
		}
		return pkgs, suite + "/main/binary-amd64/by-hash/SHA512/" + hex.EncodeToString(s512[:]),
			strconv.FormatInt(*sf.CurrentSnapshotID, 10)
	}
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+flipSimHost+path, nil))
		return rec
	}
	pkgs1, alias1, id1 := adopt(1)
	if rec := get(alias1); rec.Code != http.StatusOK || rec.Body.String() != string(pkgs1) || rec.Header().Get(hdrXCacheSnapshot) != id1 {
		t.Fatalf("current snapshot's SHA512 alias: %d %q from snapshot %q; want 200 with Packages from %s",
			rec.Code, rec.Body.String(), rec.Header().Get(hdrXCacheSnapshot), id1)
	}
	pkgs2, alias2, id2 := adopt(2)
	if rec := get(alias1); rec.Code != http.StatusOK || rec.Body.String() != string(pkgs1) || rec.Header().Get(hdrXCacheSnapshot) != id1 {
		t.Fatalf("displaced snapshot's SHA512 alias: %d %q from snapshot %q; want 200 with the old Packages from %s",
			rec.Code, rec.Body.String(), rec.Header().Get(hdrXCacheSnapshot), id1)
	}
	if rec := get(alias2); rec.Code != http.StatusOK || rec.Body.String() != string(pkgs2) || rec.Header().Get(hdrXCacheSnapshot) != id2 {
		t.Fatalf("new snapshot's SHA512 alias: %d from snapshot %q; want 200 from %s", rec.Code, rec.Header().Get(hdrXCacheSnapshot), id2)
	}
}
```

In `internal/freshness/release_parse_test.go`, add `"strconv"` to the imports and, in `TestParseRelease_LargeRealistic`, replace the loop with

```go
	for i := 0; i < n; i++ {
		// Distinct paths: since phase E an identical entry repeated in one
		// block is one member (ParseRelease merges per path).
		b.WriteString(" ")
		b.WriteString(h)
		b.WriteString(" 4533 main/binary-amd64/by-hash/SHA256/")
		b.WriteString(strconv.Itoa(i))
		b.WriteString("\n")
	}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -gcflags=-e ./internal/freshness/ -run 'TestParseRelease|TestVerifyMember|TestMemberWriter_|TestByHashAliasesAndProbe|TestAdopter_SHA512OnlyReleaseAdopts|TestAdopter_VerifiesEveryDeclaredHash|TestAdopter_ByHashProbeUsesTheStrongestHash|TestReconcile_HealsASHA512OnlyMember|TestRepair_UsesEveryDeclaredHash|TestAdopter_PoolReuseChecksTheSHA512'` (`-gcflags=-e` lifts the compiler's 10-error cap, so every identifier below is named)
Expected: compile failure naming `unknown field SHA512 in struct literal of type ReleaseMember`, `undefined: newMemberWriter`, `verifyMember`, `memberHashMismatch`, `byHashAliases`, `byHashProbePath`.

Run: `go test ./internal/handler/ -run TestServeHTTP_SHA512ByHashFromAnAdoption`
Expected: FAIL — `current snapshot's SHA512 alias: 404 "not in snapshot\n"` (no adoption writes a SHA512 alias yet).

- [ ] **Step 3: Implement**

In `internal/freshness/release_parse.go`, `ReleaseMember` becomes:

```go
// ReleaseMember is one member a Release/InRelease declares, its SHA256:
// and SHA512: entries merged (spec C5 "Hash blocks"). Path is
// suite-relative, e.g. "main/binary-amd64/Packages". At least one hash is
// set.
type ReleaseMember struct {
	Path   string
	SHA256 string // lowercase 64 hex chars; "" when only SHA512 is declared
	SHA512 string // lowercase 128 hex chars; "" when not declared
	Size   int64  // the SHA512 entry's size when there is one, else the SHA256 entry's
}
```

Replace `ParseRelease` (its doc comment too) with:

```go
// ParseRelease extracts the SHA256: and SHA512: blocks from verified
// Release-equivalent text — the cleartext payload of an InRelease, or the
// body of a detached or unsigned Release file — merging them per member
// path, as apt does (spec C5 "Hash blocks"): a member carries every hash
// declared for it, and its size comes from the SHA512 entry when there is
// one, else from the SHA256 entry. Members are returned in the order of
// their first entry.
//
// MD5Sum: and SHA1: blocks are ignored on purpose: a member declared only
// there is not returned. A Release with neither a SHA256 nor a SHA512
// block is an error, and so is a member declared twice in one block with
// a different hash or size (a malformed line in either block fails the
// parse, as before).
//
// AIDEV-NOTE: SHA512-only members (libnvidia-container's Release has only
// a SHA512 block) carry SHA256 == "": every consumer goes through the
// shared member verifier (member_verify.go), which derives the SHA-256 of
// the verified bytes, never through m.SHA256 alone.
//
// Metadata-self entries — "Release", "InRelease", "Release.gpg" — are
// silently dropped. apt-ftparchive's `release` subcommand emits the
// generated Release into its OWN SHA256 block as a directory-walk
// artifact, listing it at "stub size" (headers only, ~188 bytes) with
// the corresponding stub hash; the on-disk file we'd refetch is the
// FULL output (~1.5 KiB) with a different hash, so naively iterating
// these entries would dead-end at a content-length mismatch. apt
// itself never refetches Release/Release.gpg/InRelease via the SHA256
// block — it has those bytes already from the freshness fetch — so
// dropping them is faithful to apt semantics. Adoption inserts the
// metadata-self snapshot_member row(s) explicitly in step 6 of
// runShared — InRelease for inline mode, Release + Release.gpg for
// detached mode.
//
// AIDEV-NOTE: callers have already cryptographically verified the input
// (§7.6 verify step). This function is a pure text parser — it consults
// no trust store and performs no signature operation. Path validation
// here is defense-in-depth against a malformed-but-signed upstream.
func ParseRelease(text []byte) ([]ReleaseMember, error) {
	scanner := bufio.NewScanner(bytes.NewReader(text))
	scanner.Buffer(make([]byte, 0, scanBufCap), scanBufCap)

	var out []ReleaseMember
	at := make(map[string]int) // member path → its index in out
	type entry struct {
		hash string
		size int64
	}
	seen := map[string]map[string]entry{"SHA256": {}, "SHA512": {}} // per block, the first entry per path
	parsedRows := 0                                                 // distinct from len(out) — counts filtered entries too
	block := ""                                                     // "SHA256" or "SHA512" inside one of those blocks
	sawBlock := false
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if line == "" {
			// Blank line ends the current block. apt's spec allows
			// (but does not require) one between sections.
			block = ""
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if block == "" {
				continue
			}
			m, err := parseReleaseHashLine(line, block)
			if err != nil {
				return nil, fmt.Errorf("release: line %d: %w", lineNo, err)
			}
			// Cap on parsed rows, NOT on retained rows: an upstream
			// that pads a Release with a million metadata-self
			// entries would otherwise never trip the cap (they all
			// get filtered) yet still pay the per-line parse cost.
			// The caller-side body bound (~4 MiB for inline
			// InRelease) is the primary DoS gate; this is a
			// secondary bound that holds even if a future detached-
			// mode path streams a larger Release file.
			parsedRows++
			if parsedRows > MaxReleaseMembers {
				return nil, fmt.Errorf("release: exceeds %d members", MaxReleaseMembers)
			}
			if isMetadataSelfPath(m.Path) {
				continue
			}
			hash := m.SHA256
			if block == "SHA512" {
				hash = m.SHA512
			}
			if prev, dup := seen[block][m.Path]; dup {
				if prev.hash != hash || prev.size != m.Size {
					return nil, fmt.Errorf("release: line %d: %s declared twice in the %s block with different values", lineNo, m.Path, block)
				}
				continue
			}
			seen[block][m.Path] = entry{hash, m.Size}
			i, ok := at[m.Path]
			if !ok {
				i = len(out)
				at[m.Path] = i
				out = append(out, ReleaseMember{Path: m.Path})
			}
			switch block {
			case "SHA256":
				out[i].SHA256 = hash
				if out[i].SHA512 == "" {
					out[i].Size = m.Size
				}
			case "SHA512":
				out[i].SHA512 = hash
				out[i].Size = m.Size
			}
			continue
		}
		// Header line. Trim trailing whitespace; the header label
		// itself never legitimately contains spaces.
		switch strings.TrimRight(line, " \t") {
		case "SHA256:":
			block, sawBlock = "SHA256", true
		case "SHA512:":
			block, sawBlock = "SHA512", true
		default:
			block = ""
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("release: scan: %w", err)
	}
	if !sawBlock {
		return nil, errors.New("release: no SHA256 or SHA512 block")
	}
	if len(out) == 0 {
		return nil, errors.New("release: empty SHA256 and SHA512 blocks")
	}
	return out, nil
}
```

Replace `parseReleaseHashLine` with:

```go
// parseReleaseHashLine parses one entry of block ("SHA256" or "SHA512"):
// the hash lands in the member's field of that algorithm.
func parseReleaseHashLine(line, block string) (ReleaseMember, error) {
	// strings.Fields collapses runs of whitespace and strips the
	// leading indent. Real Release files pad the size column with
	// spaces for alignment; Fields sees through that.
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return ReleaseMember{}, fmt.Errorf("malformed line %q (want 3 fields, got %d)", line, len(fields))
	}
	hash := fields[0]
	valid := validHexSHA256
	if block == "SHA512" {
		valid = validHexSHA512
	}
	if !valid(hash) {
		return ReleaseMember{}, fmt.Errorf("invalid %s: %q", strings.ToLower(block), hash)
	}
	size, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return ReleaseMember{}, fmt.Errorf("invalid size %q: %w", fields[1], err)
	}
	if size < 0 {
		return ReleaseMember{}, fmt.Errorf("negative size: %d", size)
	}
	path := fields[2]
	if err := validateMemberPath(path); err != nil {
		return ReleaseMember{}, fmt.Errorf("invalid path %q: %w", path, err)
	}
	if block == "SHA512" {
		return ReleaseMember{Path: path, SHA512: hash, Size: size}, nil
	}
	return ReleaseMember{Path: path, SHA256: hash, Size: size}, nil
}
```

and add after `validHexSHA256`:

```go
// validHexSHA512 reports whether s is exactly 128 lowercase hex characters.
func validHexSHA512(s string) bool {
	return len(s) == 128 && validHexSHA256(s[:64]) && validHexSHA256(s[64:])
}
```

Create `internal/freshness/member_verify.go`:

```go
package freshness

// Spec C5 "Hash blocks": one verifier for every member fetch — adoption,
// reconcile and the skipped-member repair all go through adoptMember —
// checks the bytes against every SHA-256 and SHA-512 the Release declares
// for the member, returns the SHA-256 of the verified bytes, and names
// the member's by-hash aliases for every declared algorithm.

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// memberWriter is the fetch destination of a member: the cache's
// BlobWriter (which hashes SHA-256 as it writes) with a SHA-512 hasher
// beside it, so a SHA-512 declaration is checked before the bytes reach
// pool/.
type memberWriter struct {
	*cache.BlobWriter
	sha512 hash.Hash
}

func newMemberWriter(c *cache.Cache) (*memberWriter, error) {
	w, err := c.NewTempBlob()
	if err != nil {
		return nil, err
	}
	return &memberWriter{BlobWriter: w, sha512: sha512.New()}, nil
}

// Write writes p to the temp blob and feeds both hashers.
func (w *memberWriter) Write(p []byte) (int, error) {
	n, err := w.BlobWriter.Write(p)
	w.sha512.Write(p[:n])
	return n, err
}

// Truncate restarts the blob and both hashers (a resumed fetch whose
// validator changed starts over; fetch.FetchDst).
func (w *memberWriter) Truncate() error {
	if err := w.BlobWriter.Truncate(); err != nil {
		return err
	}
	w.sha512.Reset()
	return nil
}

// memberHashMismatch is verifyMember's error for bytes whose hash is not
// the declared one: algo is "SHA256" or "SHA512".
type memberHashMismatch struct {
	algo, declared, actual string
}

func (e *memberHashMismatch) Error() string {
	return fmt.Sprintf("%s mismatch: declared %s, got %s", e.algo, e.declared, e.actual)
}

// verifyMember checks the bytes a fetch wrote into w against every hash m
// declares and moves them into pool/ (spec C5): the SHA-512 first, from
// the tee, so mismatched bytes never reach pool/; then the SHA-256 through
// FinalizeExpectingHash, or — for a SHA512-only member — Finalize. size is
// the length to check (0 skips it). It returns the SHA-256 of the
// verified bytes: the declared one, or for a SHA512-only member the
// derived one (controlling it needs a SHA-512 preimage). A hash mismatch
// is a *memberHashMismatch; the temp is removed on every failure.
//
// AIDEV-NOTE: never Finalize before every declared hash matched: Finalize
// renames the temp into pool/ under its own hash, so a forged member would
// otherwise sit in the pool as a stray blob.
func verifyMember(w *memberWriter, m ReleaseMember, size int64) (string, error) {
	if m.SHA256 == "" && m.SHA512 == "" {
		_ = w.Abort()
		return "", fmt.Errorf("member %s declares no SHA256 or SHA512 hash", m.Path)
	}
	if m.SHA512 != "" {
		if got := hex.EncodeToString(w.sha512.Sum(nil)); got != m.SHA512 {
			_ = w.Abort()
			return "", &memberHashMismatch{algo: "SHA512", declared: m.SHA512, actual: got}
		}
	}
	if m.SHA256 == "" {
		return w.Finalize(size)
	}
	got, err := w.FinalizeExpectingHash(m.SHA256, size)
	if errors.Is(err, cache.ErrHashMismatch) {
		return "", &memberHashMismatch{algo: "SHA256", declared: m.SHA256, actual: got}
	}
	return got, err
}

// hashFileDigests streams a pool file through SHA-256 and SHA-512 (the
// rehash-on-reuse defence for a member that declares both).
func hashFileDigests(p string) (sum256, sum512 string, err error) {
	f, err := os.Open(p)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = f.Close() }()
	h256, h512 := sha256.New(), sha512.New()
	if _, err := io.Copy(io.MultiWriter(h256, h512), f); err != nil {
		return "", "", err
	}
	return hex.EncodeToString(h256.Sum(nil)), hex.EncodeToString(h512.Sum(nil)), nil
}

// byHashPath returns the by-hash path of a member for one algorithm
// ("SHA256" or "SHA512"): <dir>/by-hash/<algo>/<hex>, or by-hash/<algo>/<hex>
// for a member at the suite root (see byHashAliasPath's AIDEV-NOTE: apt
// requests root members by hash too).
func byHashPath(declaredPath, algo, hexHash string) string {
	dir := path.Dir(declaredPath)
	if dir == "." || dir == "" {
		return "by-hash/" + algo + "/" + hexHash
	}
	return dir + "/by-hash/" + algo + "/" + hexHash
}

// byHashAliases returns m's by-hash alias paths, one per algorithm its
// Release declares (spec C5: apt requests the strongest, and a client
// configured for another hash may ask for the other one).
func byHashAliases(m ReleaseMember) []string {
	var out []string
	if m.SHA256 != "" {
		out = append(out, byHashAliasPath(m.Path, m.SHA256))
	}
	if m.SHA512 != "" {
		out = append(out, byHashPath(m.Path, "SHA512", m.SHA512))
	}
	return out
}

// byHashProbePath is the by-hash URL path adoption probes for m: apt's
// rule, the strongest declared hash (spec §3: by-hash/SHA512 whenever the
// Release declares SHA512).
func byHashProbePath(m ReleaseMember) string {
	if m.SHA512 != "" {
		return byHashPath(m.Path, "SHA512", m.SHA512)
	}
	return byHashAliasPath(m.Path, m.SHA256)
}

// aliasRows returns m's by-hash alias rows, each bound to blob — the
// SHA-256 of m's verified bytes (verifyMember).
func aliasRows(snapshotID int64, m ReleaseMember, blob string) []cache.SnapshotMember {
	var out []cache.SnapshotMember
	for _, p := range byHashAliases(m) {
		out = append(out, cache.SnapshotMember{SnapshotID: snapshotID, Path: p, BlobHash: blob, DeclaredSHA256: blob})
	}
	return out
}

// memberRows returns a verified member's snapshot_member rows: its path
// and its alias rows (aliasRows).
func memberRows(snapshotID int64, m ReleaseMember, blob string) []cache.SnapshotMember {
	return append([]cache.SnapshotMember{{SnapshotID: snapshotID, Path: m.Path, BlobHash: blob, DeclaredSHA256: blob}},
		aliasRows(snapshotID, m, blob)...)
}

// verifiedMember is m once its bytes verified as blob: from here on its
// SHA256 is the verified bytes' hash, declared or derived — what the
// Packages/Sources/pdiff builders read the pool by.
func verifiedMember(m ReleaseMember, blob string) ReleaseMember {
	m.SHA256 = blob
	return m
}
```

In `internal/freshness/adoption.go`, replace `adoptMember`, `tryByHashFetch` and `fetchMemberURL` (with their doc comments) with:

```go
// adoptMember handles step 5 for one declared member: try pool reuse
// (with rehash defense), else fetch from upstream. Returns the blob
// hash on success: the SHA-256 of bytes that matched every hash m
// declares (spec C5) — m.SHA256 itself when it is declared, the derived
// SHA-256 for a SHA512-only member (verifyMember).
//
// AIDEV-NOTE: a SHA512-only member is never reused from the pool (its
// SHA-256 is not known before the fetch): it is fetched at every
// adoption, as small repositories that publish only SHA512 can afford.
func (a *Adopter) adoptMember(ctx context.Context, suite SuiteRef, m ReleaseMember, acquireByHash bool) (string, error) {
	exists := false
	if m.SHA256 != "" {
		var err error
		exists, err = a.cache.BlobExists(m.SHA256)
		if err != nil {
			return "", fmt.Errorf("%w: BlobExists %s: %v", ErrAdoptionDBFailed, m.SHA256, err)
		}
	}
	if exists {
		// "Rehash on reuse" — SPEC2 §7.5 step 5. Pool blobs predating
		// Phase 2 were inserted under the trust-upstream model; their
		// on-disk content was not verified against a declared hash at
		// the time. Re-hashing bounds the verified set to bytes we
		// have *just* confirmed.
		actual, actual512, err := hashFileDigests(a.cache.BlobPath(m.SHA256))
		if err != nil {
			return "", fmt.Errorf("%w: rehash %s: %v", ErrAdoptionDBFailed, m.SHA256, err)
		}
		if actual == m.SHA256 && m.SHA512 != "" && actual512 != m.SHA512 {
			// The pool bytes are exactly the declared SHA-256, so their
			// SHA-512 is fixed: the Release's two declarations disagree.
			// A fetch would bring the same bytes; fail the member (spec
			// C5: every declared hash must match), and keep the pool file.
			return "", newMemberErr(ErrAdoptionMemberMismatch, m.Path,
				fmt.Sprintf("declared SHA512 %s, got %s", shortHash(m.SHA512), shortHash(actual512)))
		}
		if actual == m.SHA256 {
			// Confirmed reuse. Make sure the row exists (may not, e.g.
			// a stray pool file from a partial migration); PutBlob is
			// idempotent.
			if err := a.cache.PutBlob(ctx, m.SHA256, m.Size); err != nil {
				return "", fmt.Errorf("%w: PutBlob reuse %s: %v", ErrAdoptionDBFailed, m.SHA256, err)
			}
			return m.SHA256, nil
		}
		// Pool blob is corrupted at rest. SPEC2 §7.5 step 5: log and
		// evict; fall through to upstream fetch.
		a.logger.Warn("pool_corruption_during_adoption",
			"canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath,
			"path", m.Path,
			"declared_sha256", m.SHA256,
			"actual_sha256", actual,
		)
		integrity.IncPoolCorruptionDuringAdoption()
		if err := os.Remove(a.cache.BlobPath(m.SHA256)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: evict corrupted %s: %v", ErrAdoptionDBFailed, m.SHA256, err)
		}
		// Don't try to delete the blob row — Phase 4 GC handles
		// refcount=0 cleanup. The next refetch will Put-Blob through.
	}

	// Acquire host slot for the member fetch (SPEC §9.3 / §9.3.1).
	// Adoption uses the same hostsem as the request path, so a
	// member fetch contends with cache-miss fetches for the same
	// per-host budget.
	release, err := a.hostSem.Acquire(ctx, suite.CanonicalHost)
	if err != nil {
		return "", fmt.Errorf("%w: hostsem acquire %s: %v",
			ErrAdoptionMemberFetchFailed, suite.CanonicalHost, err)
	}
	defer release()

	// REC 2: prefer Acquire-By-Hash when the upstream advertises it and
	// the member lives under a directory. A by-hash URL embeds the
	// declared sha256 in its path, so it is immune to the publication
	// race where the mutable path serves a newer-but-valid revision than
	// our InRelease declared (the dominant intermittent failure on the
	// Ubuntu archives). The probe is best-effort and side-effect-free:
	// tryByHashFetch never logs adoption_member_skipped / increments the
	// skip metric and never lets mismatched bytes reach pool/ (it uses
	// FinalizeExpectingHash). On ANY by-hash failure we fall back to the
	// authoritative plain-path fetch below, which keeps today's exact
	// semantics (404/410 → skip; size/hash mismatch → typed-fatal).
	// The probe uses the strongest declared hash, as apt does (spec §3).
	if acquireByHash && m.Path != "" {
		if hash, ferr := a.tryByHashFetch(ctx, suite, m, byHashProbePath(m)); ferr == nil {
			return hash, nil
		} else {
			a.logger.Debug("adoption_byhash_fallback",
				"canonical_host", suite.CanonicalHost,
				"suite_path", suite.SuitePath,
				"path", m.Path,
				"err", ferr,
			)
		}
	}
	return a.fetchMemberURL(ctx, suite, m, m.Path)
}

// tryByHashFetch attempts to fetch a member from its content-addressed
// by-hash URL (byHashRelPath). It is a SIDE-EFFECT-FREE probe: unlike
// fetchMemberURL it emits no adoption_member_skipped WARN, increments no
// skip metric, and — by verifying every declared hash before the bytes
// reach pool/ (verifyMember) — never promotes bytes whose hash differs
// from the declaration into pool/ (a by-hash URL that serves wrong bytes
// is rejected with the temp removed, leaving no orphan). On success the
// verified blob is PutBlob'd and its SHA-256 returned. Any error means
// "by-hash unavailable or wrong"; the caller falls back to the
// authoritative plain-path fetch.
func (a *Adopter) tryByHashFetch(ctx context.Context, suite SuiteRef, m ReleaseMember, byHashRelPath string) (string, error) {
	target := &fetch.Target{
		CanonicalHost: suite.CanonicalHost,
		URL:           buildMemberURL(suite, byHashRelPath),
	}
	w, err := newMemberWriter(a.cache)
	if err != nil {
		return "", err
	}
	defer func() { _ = w.Abort() }() // no-op once a Finalize* wins
	if _, err := a.fetcher.Fetch(ctx, target, w); err != nil {
		return "", err // by-hash 404 (pruned), transport, etc. — silent fallback
	}
	hashHex, err := verifyMember(w, m, m.Size)
	if err != nil {
		return "", err
	}
	if err := a.cache.PutBlob(ctx, hashHex, w.Written()); err != nil {
		return "", err
	}
	return hashHex, nil
}

// fetchMemberURL fetches one Release member from a specific suite-
// relative URL (either the plain declared path or its by-hash alias),
// verifies the bytes against the member's declared Size + SHA256, and
// PutBlobs them. The host slot is held by the caller (adoptMember).
//
// Error semantics are the authoritative member-fetch rules: a 404/410
// returns errAdoptionMemberSkipped (SPEC2 §7.5.2 "declared but does not
// serve"); a size/content-length mismatch, finalize error, transport
// error, or other status returns a typed *AdoptionMemberError wrapping
// ErrAdoptionMemberFetchFailed; a hash mismatch wraps
// ErrAdoptionMemberMismatch. Verification always uses m.Size and every
// hash m declares (the signed-Release declarations; verifyMember),
// independent of which URL served the bytes; mismatched bytes never reach
// pool/.
func (a *Adopter) fetchMemberURL(ctx context.Context, suite SuiteRef, m ReleaseMember, relPath string) (string, error) {
	upstreamURL := buildMemberURL(suite, relPath)
	target := &fetch.Target{
		CanonicalHost: suite.CanonicalHost,
		URL:           upstreamURL,
	}

	w, err := newMemberWriter(a.cache)
	if err != nil {
		return "", fmt.Errorf("%w: NewTempBlob %s: %v", ErrAdoptionDBFailed, m.Path, err)
	}
	defer func() { _ = w.Abort() }() // no-op once Finalize wins

	res, err := a.fetcher.Fetch(ctx, target, w)
	if err != nil {
		// SPEC2 §7.5.2 (Phase 2 clarification): only the explicit
		// "resource not present" 4xx codes — 404 Not Found and 410
		// Gone — are treated as "upstream declared but does not
		// serve" and skipped. The canonical case is Ubuntu's Release
		// file declaring an uncompressed Contents-amd64 the archive
		// only ships as Contents-amd64.gz (404). Other 4xx codes
		// stay fatal:
		//   - 401/403: auth or policy. The project assumes no
		//     upstream needs auth, but if one starts returning these
		//     we want loud failure, not silent partial snapshots.
		//   - 408/425/429: transient (timeout / too-early / rate
		//     limit). Adoption should retry on the next tick rather
		//     than persist a degraded snapshot.
		//   - All other 4xx: unknown semantics; fail closed.
		// 5xx and transport errors stay fatal too — those are
		// "upstream is broken right now", not "upstream never serves
		// this thing".
		var se *fetch.StatusError
		if errors.As(err, &se) && (se.Code == 404 || se.Code == 410) {
			// SPEC6_7 §6: a 404-skipped IndexTarget gets its own reason
			// value. Skipping it is unchanged (Ubuntu's main archive
			// declares foreign-arch Packages* that only ports.ubuntu.com
			// serves), but the signal is categorically worse than an
			// optional-member artifact — for an arch clients actually
			// use it means `apt update` hard-fails for the snapshot's
			// lifetime — so operators need to alert on it separately
			// (and opt into the required_architectures guard).
			reason := "4xx"
			if isIndexTarget(m.Path) {
				reason = "4xx_index_target"
			}
			a.logger.Warn("adoption_member_skipped",
				"canonical_host", suite.CanonicalHost,
				"suite_path", suite.SuitePath,
				"path", m.Path,
				"declared_sha256", m.SHA256,
				"upstream_status", se.Code,
				"reason", reason,
			)
			adoptionMembersSkippedTotal.Inc(reason)
			return "", errAdoptionMemberSkipped
		}
		return "", newMemberErr(ErrAdoptionMemberFetchFailed, m.Path,
			fmt.Sprintf("fetch: %v", err))
	}
	// Size sanity: declared Size in the Release file should match
	// what the fetch actually wrote. fetch.Client already honors
	// Content-Length internally, so the case where they diverge is
	// genuinely abnormal — surface it as a member-fetch error rather
	// than a hash mismatch (the bytes might still hash correctly,
	// but a length mismatch is its own integrity violation).
	if m.Size > 0 && res.ContentLength > 0 && res.ContentLength != m.Size {
		return "", newMemberErr(ErrAdoptionMemberFetchFailed, m.Path,
			fmt.Sprintf("served %d vs declared %d", res.ContentLength, m.Size))
	}

	hashHex, err := verifyMember(w, m, res.ContentLength)
	var mm *memberHashMismatch
	switch {
	case errors.As(err, &mm):
		// Mismatch: adoption rejects, and the bytes never reached pool/
		// (verifyMember removed the temp). Log loudly.
		suffix, prefix := "sha256", ""
		if mm.algo == "SHA512" {
			suffix, prefix = "sha512", "SHA512 "
		}
		a.logger.Warn("adoption_member_mismatch",
			"canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath,
			"path", m.Path,
			"declared_"+suffix, mm.declared,
			"actual_"+suffix, mm.actual,
		)
		return "", newMemberErr(ErrAdoptionMemberMismatch, m.Path,
			fmt.Sprintf("%sdeclared %s, got %s", prefix, shortHash(mm.declared), shortHash(mm.actual)))
	case err != nil:
		return "", newMemberErr(ErrAdoptionMemberFetchFailed, m.Path,
			fmt.Sprintf("finalize: %v", err))
	}
	if err := a.cache.PutBlob(ctx, hashHex, w.Written()); err != nil {
		return "", fmt.Errorf("%w: PutBlob %s: %v", ErrAdoptionDBFailed, hashHex, err)
	}
	return hashHex, nil
}
```

`byHashAliasPath` keeps its doc comment and AIDEV-NOTE; its last doc line becomes `// SPEC6_7 §4. Since phase E the SHA512 alias (byHashPath) follows the same` / `// rule (spec C5).` and its body:

```go
	if declaredPath == "" {
		return ""
	}
	return byHashPath(declaredPath, "SHA256", sha256hex)
```

In `runShared`, Step 5: the comment above `fetchedMembers` ends "…or a Packages-blob read against an empty pool entry. Its SHA256 is the verified bytes' hash (verifiedMember: derived for a SHA512-only member), so the Step 8 builders read the pool by it." Right after `fetchedMembers := make(...)`, add

```go
	// pendingAliases are Step 7's rows, built from each member's
	// declaration while it is at hand: one alias per declared algorithm
	// (spec C5), bound to the verified blob.
	var pendingAliases []cache.SnapshotMember
```

In the tolerated-failure branch, the `integritySkipped = append(...)` becomes conditional:

```go
				// A SHA512-only member has no SHA-256 for a repair record
				// to verify against: it is not recorded (spec C5
				// limitation; reconcile still heals requestable
				// IndexTargets).
				if m.SHA256 != "" {
					integritySkipped = append(integritySkipped, cache.SkippedMember{
						SnapshotID:     snapshotID,
						Path:           m.Path,
						DeclaredSHA256: m.SHA256,
						Size:           m.Size,
						Reason:         cache.SkipReasonOptionalMemberIntegrity,
						Detail:         detail,
					})
				}
```

and the success path's two appends become

```go
		memberRows = append(memberRows, cache.SnapshotMember{
			SnapshotID:     snapshotID,
			Path:           m.Path,
			BlobHash:       blobHash,
			DeclaredSHA256: blobHash,
		})
		pendingAliases = append(pendingAliases, aliasRows(snapshotID, m, blobHash)...)
		fetchedMembers = append(fetchedMembers, verifiedMember(m, blobHash))
```

Replace Step 7 (from its comment through the end of its loop) with

```go
	// Step 7: by-hash alias rows. apt's Acquire-By-Hash clients fetch
	// from <suite>/<component>/by-hash/<algorithm>/<declared hash> — the
	// strongest hash the Release declares, so SHA512 whenever it is
	// declared (spec C5) — and a snapshot_member row at that alias path
	// lets §6.1 resolve those requests through the same blob. Each
	// fetched member contributes one alias per declared algorithm
	// (pendingAliases, Step 5). We dedupe — multiple members with the
	// same content would produce the same alias path and trigger a
	// unique violation.
	//
	// Only fetched members have aliases: a 4xx-skipped member has no
	// blob in the pool, so an alias row pointing at it would violate the
	// snapshot_member.blob_hash → blob.hash foreign key.
	aliasSeen := make(map[string]bool, len(pendingAliases))
	for _, r := range pendingAliases {
		if aliasSeen[r.Path] {
			continue
		}
		aliasSeen[r.Path] = true
		memberRows = append(memberRows, r)
	}
```

In `internal/freshness/reconcile.go` (`ReconcileSnapshot`), replace the two `rows = append(...)` statements and `healedDecl = append(healedDecl, m)` with

```go
		// Spec C5: the member and an alias per declared algorithm, bound
		// to the bytes adoptMember verified against every declared hash.
		rows = append(rows, memberRows(snapshotID, m, blobHash)...)
		healedDecl = append(healedDecl, verifiedMember(m, blobHash))
```

In `internal/freshness/repair.go` (`RepairSkippedMembers`): before the loop add `decl := a.repairDeclarations(ctx, suite, snapshotID)`; after building `m` from the row add

```go
		// Spec C5: the record keeps only the SHA-256; the snapshot's
		// Release adds every other declared hash (a dual-hash member is
		// verified against both and gets its SHA512 alias).
		if d, ok := decl[row.Path]; ok && d.SHA256 == row.DeclaredSHA256 {
			m = d
		}
```

replace the `members := []cache.SnapshotMember{…}` literal and its alias `if` with `members := memberRows(snapshotID, m, blobHash)`; and before `repairPending` add:

```go
// repairDeclarations re-reads the members the snapshot's Release declares
// (reparseSnapshotRelease, the trust anchor), by path, so a repair checks
// every declared hash and writes an alias per declared algorithm (spec
// C5). nil when it cannot: the repair then verifies against the skip
// records' SHA-256 alone, as before phase E (logged at DEBUG; reconcile
// reports an unreadable anchor at WARN).
func (a *Adopter) repairDeclarations(ctx context.Context, suite SuiteRef, snapshotID int64) map[string]ReleaseMember {
	declared, err := a.reparseSnapshotRelease(ctx, snapshotID)
	if err != nil {
		a.logger.Debug("adoption_repair_declarations_unavailable",
			"canonical_host", suite.CanonicalHost, "suite_path", suite.SuitePath,
			"snapshot_id", snapshotID, "err", err)
		return nil
	}
	out := make(map[string]ReleaseMember, len(declared))
	for _, m := range declared {
		out[m.Path] = m
	}
	return out
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/freshness/ ./internal/handler/`
Expected: PASS.

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| `verifyMember`: the SHA512 comparison → `false && …` | `TestVerifyMember/{sha512_wrong, sha512-only_wrong}`, `TestAdopter_VerifiesEveryDeclaredHash/forged_SHA512` |
| `byHashAliases`: drop the SHA512 alias | `TestByHashAliasesAndProbe`, `TestAdopter_SHA512OnlyReleaseAdopts`, `TestAdopter_VerifiesEveryDeclaredHash/forged_`, `TestReconcile_HealsASHA512OnlyMember`, `TestRepair_UsesEveryDeclaredHash`, `TestServeHTTP_SHA512ByHashFromAnAdoption` |
| `byHashProbePath` always probes SHA256 | `TestByHashAliasesAndProbe`, `TestAdopter_ByHashProbeUsesTheStrongestHash` |
| `ParseRelease`: the SHA512 entry no longer sets the size | `TestParseRelease_MergesHashBlocks` |
| `RepairSkippedMembers` ignores `decl` | `TestRepair_UsesEveryDeclaredHash` |
| record integrity skips of SHA512-only members (`if m.SHA256 != ""` → `if true`) | `TestRepair_UsesEveryDeclaredHash` (the commit fails on the empty declared hash) |
| `memberWriter.Truncate` does not reset the SHA-512 | `TestMemberWriter_TruncateRestartsBothHashes` |
| `adoptMember`: drop the pooled blob's SHA512 check | `TestAdopter_PoolReuseChecksTheSHA512` |

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/freshness/ internal/handler/byhash_sha512_test.go | grep -v hot_prefetch_test.go
git add internal/freshness/member_verify.go internal/freshness/hash_blocks_test.go internal/freshness/release_parse.go \
  internal/freshness/release_parse_test.go internal/freshness/adoption.go internal/freshness/reconcile.go \
  internal/freshness/repair.go internal/handler/byhash_sha512_test.go
git commit -m "Merge SHA256 and SHA512 hash blocks and verify every declared hash of every member"
```

(`gofmt -l` must print nothing after the filter: `hot_prefetch_test.go` is a drift file this task does not touch.)

### Task 3: The unsigned form in the adopter — eligibility, member cap, no vouching, the re-mark (R-E1) and the in-commit refusal as a state (R-E2)

Spec C5 "Unsigned safeguards" (D6), the adopter's share: safeguard 1 (`adoption.adopt_unsigned`, here the adopter's `AdoptionConfig.AdoptUnsigned`; Task 11 wires the config key), safeguard 3's first point in `runShared`'s order of checks ("signature verification (or unsigned eligibility)" — the knob, a signed current snapshot, a marker for the base, before anything is downloaded), safeguard 4 (at most 1,000 declared members, `unsigned_too_large`), safeguard 5 (no `package_hash` rows on any path: `buildPackageHashes` takes the form, and the Sources and pdiff builders are not run), the form's storage (candidate `Form: cache.FormUnsigned` set explicitly — overview "E:" carry-forward; the Release in the single-anchor slot; one `Release` metadata-self member). Ruling R-E1: before any unsigned commit the adopter re-runs the cached signed-anchor mark under the suite mutex it already holds, outside the writer, and refuses (records a refusal) when it cannot. Ruling R-E2: a refusal raised inside `CommitAdoption` surfaces as `ErrAdoptionUnsignedDowngradeRefused` through `classifyAdoptionOutcome` (outcome `refused`), bypasses the `adoption: commit failed` WARN and the `db_failed` wrap, and gives `adoption_flip_barrier` the outcome `refused` — also after a barrier that outlived `adoption.max_duration`; each E refusal sentinel has its own `classifyAdoptionReason` case and none joins admin's `refusalKeepsCurrent`. Overview "E:" carry-forwards: `contentKey`'s unsigned case (sha256(Release), never sha256(nil)); `servedAnchorUnchanged` compares `<suite>/Release` for the unsigned form (the `AIDEV-TODO(phase E)` there is resolved). Deferred minors C-16 (`servedAnchorUnchanged` reads under `p.barrierCtx(ctx)` with a bound and logs errors other than "no row") and A-49 (the startup marker pass says at DEBUG which cached anchor it skipped) sit in code this task rewrites.

The re-mark (R-E1) shares one sniff helper with the startup marker pass, `sniffCachedAnchor` (`legacy_audit.go`), so the two agree on what counts as signed; phase D's handler applies the same `aptpath.SuiteFile` sniffs. It reads the rows of the suite's base **and of its `/.` twin** (`MarkerBase(s)` and `MarkerBase(s)+"/."`: the twins share the marker, design review I-3 — no twin row can exist before phase G, so E's behaviour is unchanged), and marks the base. A failed re-mark is logged before the refusal is returned (`adoption: signed-anchor re-mark failed`, ERROR for a pool read error, WARN otherwise; design review M-1): the refusal it records, `unsigned_downgrade_refused`, can then mean "could not rule out signed history". The eligibility read is Task 1's `CheckUnsignedDowngrade` — the same rule as the commit's recheck, so it now also refuses a marker under the other scheme and a displaced signed snapshot (M-7, M-8) — and `unsignedEligible` no longer takes the prior snapshot. The member cap's message comes from `unsignedCap(n)`, which Tasks 6 and 7 reuse to refuse a too-large unsigned Release before a slot is taken (M-5). The unsigned content key is domain-separated, `sha256("unsigned\n" ‖ Release)`, so a record for an InRelease body never matches a Release with the same bytes (design review M-2; no stored record holds an unsigned key from before phase E). The DoD's never-downgrade race is pinned here at the writer: `TestCheck_UnsignedRefusedInsideTheCommitIsAState` pauses an eligible unsigned adoption before its commit (`Adopter.beforeCommit`, a test seam), stores a signed anchor with its marker through `cache.StoreFetched` — exactly the handler's miss-path writer operation — and resumes; the barrier outlives the 20 s `max_duration`. No check produces unsigned requests yet (Task 6), so the tests drive `RunUnsigned` and `spawnAdoption` directly.

**Existing tests that change:** `TestUnsignedKeysAndReasons` expected `contentKey(unsigned)` to be sha256(Release); with M-2's domain separation it expects sha256("unsigned\n" ‖ Release), and also that the unsigned and inline keys of the same bytes differ. The four `a.buildPackageHashes(suiteRef, 1, members, members)` calls in `internal/freshness/hot_prefetch_test.go` (a gofmt drift file: edit it with the `sed` below, never `gofmt -w`) gain the form argument `cache.FormInline`.

**Files:**
- Create: `internal/freshness/unsigned.go`, `internal/freshness/unsigned_test.go`, `internal/admin/refusal_reasons_test.go` (a new file: no existing `internal/admin` file changes, so B-59–B-63 stay where they are)
- Modify: `internal/freshness/adoption.go` (`adoptionFormUnsigned`, `AdoptionConfig.AdoptUnsigned`, `Adopter.adoptUnsigned`/`beforeCommit`, `runRequest`, `runShared`, `barrierOutcome`, `servedAnchorUnchanged`, `formOfSnapshot`, `formName`, `buildPackageHashes`), `internal/freshness/reconcile.go`, `internal/freshness/retry.go` (`contentKey`), `internal/freshness/metrics.go` (`classifyAdoptionOutcome`, `classifyAdoptionReason`), `internal/freshness/legacy_audit.go` (`markFromCachedSignedAnchors`, `sniffCachedAnchor`), `internal/freshness/hot_prefetch_test.go` (drift file, by `sed`)

**Interfaces:**
- Consumes: Task 1's `cache.ErrUnsignedDowngradeRefused`, `CheckUnsignedDowngrade` and Step 3c; Task 2's `hashRelease`, `snapshotPaths`, `shaOf` (`hash_blocks_test.go`). Test helpers `fakeFetcher`/`newFakeFetcher`, `fakePackagesStanzas`, `passThroughVerifier`, `trackSuite`, `testHex64`, `lockedBuffer`, `changedUpstream`, `mustAcquire`, `adoptionTotalFor`, `strOf`; `simtest.OpenCache`, `simtest.ClearsignedFixture`; `cache.(*Cache).StoreFetched`, `MarkSignedSeenBatch`, `SignedSeen`, `ClearSignedSeenForTest`.
- Produces: `adoptionFormUnsigned`; `AdoptionConfig.AdoptUnsigned bool`; `(*Adopter).RunUnsigned(ctx, suite SuiteRef, release []byte, etag, lastmod string) error`; `(*Adopter).unsignedEligible(ctx, suite SuiteRef) error` (Tasks 6 and 7's checks call it before taking a slot); `unsignedCap(n int) error` (Tasks 6 and 7); `(*Adopter).remarkSignedAnchors(ctx, suite) error` and `errRemarkPoolRead`; `ErrAdoptionUnsignedDowngradeRefused`, `ErrAdoptionUnsignedTooLarge`, `errUnsignedDisabled`; `unsignedMemberCap = 1000`; `anchorLastUse(*cache.URLPath) int64`; `sniffCachedAnchor(c *cache.Cache, path, blobHash string) (bool, error)`; the test seam `Adopter.beforeCommit func()`; test helpers `unsignedRig`, `newUnsignedRig(t, quiet, maxDuration)`, `(*unsignedRig).release/current/marked/cacheAnchor/cacheAnchorAt/records`, `packageHashCount(t, c, id) int`, `derefStrP` (`unsigned_test.go`).

- [ ] **Step 1: Write the failing tests**

Create `internal/freshness/unsigned_test.go`:

```go
package freshness

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/observability"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// unsignedRig is a real cache and an adopter with adoption.adopt_unsigned
// on, for suite /u/dists/s on host h; quiet > 0 adds a flip barrier.
type unsignedRig struct {
	t       *testing.T
	c       *cache.Cache
	ad      *Adopter
	gate    *flipgate.Gate
	members *fakeFetcher
	logs    *lockedBuffer
	suite   SuiteRef
	pkgs    []byte
}

const unsignedDeb = "/u/pool/main/f/foo_1.deb"

func newUnsignedRig(t *testing.T, quiet time.Duration, maxDuration time.Duration) *unsignedRig {
	t.Helper()
	c := simtest.OpenCache(t)
	r := &unsignedRig{t: t, c: c, members: newFakeFetcher(), logs: &lockedBuffer{},
		suite: SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/u/dists/s"}}
	var err error
	if quiet > 0 {
		if r.gate, err = flipgate.New(flipgate.Config{Quiet: quiet}); err != nil {
			t.Fatal(err)
		}
	}
	r.ad, err = NewAdopter(AdoptionConfig{Cache: c, Fetcher: r.members, Verifier: passThroughVerifier{},
		HostLimiter: hostsem.New(8), MaxConcurrent: 2, MaxDuration: maxDuration, FlipGate: r.gate,
		AdoptUnsigned: true, Logger: slog.New(slog.NewJSONHandler(r.logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	trackSuite(t, c, r.suite)
	r.pkgs = fakePackagesStanzas(map[string]string{"pool/main/f/foo_1.deb": testHex64})
	r.members.put("http://h/u/dists/s/main/binary-amd64/Packages", r.pkgs)
	return r
}

// release is an unsigned Release text declaring the rig's Packages.
func (r *unsignedRig) release(label string) []byte {
	return hashRelease("Origin: "+label+"\n", true, false, map[string][]byte{"main/binary-amd64/Packages": r.pkgs}, nil)
}

func (r *unsignedRig) current() int64 {
	r.t.Helper()
	sf, err := r.c.GetSuiteFreshness(context.Background(), "http", "h", r.suite.SuitePath)
	if err != nil {
		r.t.Fatal(err)
	}
	if sf.CurrentSnapshotID == nil {
		return 0
	}
	return *sf.CurrentSnapshotID
}

func (r *unsignedRig) marked() bool {
	r.t.Helper()
	_, ok, err := r.c.SignedSeen(context.Background(), "http", "h", r.suite.SuitePath)
	if err != nil {
		r.t.Fatal(err)
	}
	return ok
}

// cacheAnchor stores body as the suite's url_path row for file
// (InRelease, Release.gpg or Release), as a client's miss would have.
func (r *unsignedRig) cacheAnchor(file string, body []byte) string {
	r.t.Helper()
	return r.cacheAnchorAt(r.suite.SuitePath, file, body)
}

// cacheAnchorAt is cacheAnchor for the suite path dir (the /. twin's).
func (r *unsignedRig) cacheAnchorAt(dir, file string, body []byte) string {
	r.t.Helper()
	h, err := r.ad.writeBlobBytes(context.Background(), body)
	if err != nil {
		r.t.Fatal(err)
	}
	now := int64(1_700_000_000)
	path := dir + "/" + file
	if err := r.c.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: path,
		BlobHash: &h, UpstreamURL: "http://h" + path, IsMetadata: true, LastRequestedAt: &now, RequestCount: 1, LastFetchedAt: &now}); err != nil {
		r.t.Fatal(err)
	}
	return h
}

// records returns the adopter's JSON log records whose msg is msg ("" for all).
func (r *unsignedRig) records(msg string) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(r.logs.String(), "\n") {
		var m map[string]any
		if line != "" && json.Unmarshal([]byte(line), &m) == nil && (msg == "" || m["msg"] == msg) {
			out = append(out, m)
		}
	}
	return out
}

// Spec C5 / overview "E:" carry-forward: an unsigned adoption stores the
// candidate with Form unsigned (anchor_form 'unsigned'), serves its
// Release from the snapshot, writes no InRelease or Release.gpg member, no
// package_hash row (it vouches no .deb) and no signed-seen marker, and
// CommitAdoption seeds <suite>/Release.
func TestRunUnsigned_AdoptsWithoutVouching(t *testing.T) {
	r := newUnsignedRig(t, 0, 0)
	ctx := context.Background()
	rel := r.release("unsigned v1")
	if err := r.ad.RunUnsigned(ctx, r.suite, rel, `"e1"`, ""); err != nil {
		t.Fatal(err)
	}
	id := r.current()
	snap, err := r.c.GetSuiteSnapshot(ctx, id)
	if err != nil || snap.Form != cache.FormUnsigned || derefStrP(snap.SingleAnchorHash) != shaOf(rel) || snap.ReleaseHash != nil {
		t.Fatalf("snapshot = %+v (%v); want an unsigned snapshot whose single-anchor slot holds the Release", snap, err)
	}
	paths := snapshotPaths(t, r.c, id)
	if paths["Release"] != shaOf(rel) || paths["main/binary-amd64/Packages"] != shaOf(r.pkgs) {
		t.Fatalf("members %v; want Release and Packages", paths)
	}
	for _, p := range []string{"InRelease", "Release.gpg"} {
		if _, ok := paths[p]; ok {
			t.Errorf("an unsigned snapshot has a %s member", p)
		}
	}
	if rows, err := r.c.DeclaredHashesForPath(ctx, "http", "h", unsignedDeb); err != nil || len(rows) != 0 {
		t.Fatalf("DeclaredHashesForPath = %v, %v; an unsigned snapshot vouches nothing", rows, err)
	}
	if n := packageHashCount(t, r.c, id); n != 0 {
		t.Fatalf("the unsigned snapshot has %d package_hash rows, want none", n)
	}
	if r.marked() {
		t.Fatal("an unsigned adoption wrote a signed-seen marker")
	}
	if row, err := r.c.LookupURL(ctx, "http", "h", r.suite.SuitePath+"/Release"); err != nil || derefStrP(row.BlobHash) != shaOf(rel) {
		t.Fatalf("Release anchor row = %+v, %v", row, err)
	}
	if evs := r.records("adoption_success"); len(evs) != 1 || evs[0]["form"] != "unsigned" {
		t.Fatalf("adoption_success = %v, want one with form unsigned", evs)
	}
}

func derefStrP(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// Spec C5: eligibility comes before any download — the knob, a signed
// current snapshot, a marker for the base (the /. twin shares it) under
// either scheme (design review M-7) — and the member cap before any member
// fetch; each refusal is its sentinel and writes nothing.
func TestRunUnsigned_EligibilityBeforeAnyDownload(t *testing.T) {
	big := make(map[string][]byte, unsignedMemberCap+1)
	for i := 0; i <= unsignedMemberCap; i++ {
		big[fmt.Sprintf("main/i18n/Translation-%d", i)] = []byte{byte(i)}
	}
	for _, tc := range []struct {
		name  string
		setup func(r *unsignedRig) []byte
		want  error
	}{
		{"knob off", func(r *unsignedRig) []byte { r.ad.adoptUnsigned = false; return r.release("x") }, errUnsignedDisabled},
		{"marker for the base", func(r *unsignedRig) []byte {
			if err := r.c.MarkSignedSeenBatch(context.Background(), []cache.SignedSeenMark{{CanonicalScheme: "http",
				CanonicalHost: "h", Base: r.suite.SuitePath, SeenAt: 1}}); err != nil {
				r.t.Fatal(err)
			}
			return r.release("x")
		}, ErrAdoptionUnsignedDowngradeRefused},
		{"marker under the other scheme", func(r *unsignedRig) []byte {
			if err := r.c.MarkSignedSeenBatch(context.Background(), []cache.SignedSeenMark{{CanonicalScheme: "https",
				CanonicalHost: "h", Base: r.suite.SuitePath, SeenAt: 1}}); err != nil {
				r.t.Fatal(err)
			}
			return r.release("x")
		}, ErrAdoptionUnsignedDowngradeRefused},
		{"marker for the twin's base", func(r *unsignedRig) []byte {
			r.suite.SuitePath = "/flat/."
			trackSuite(r.t, r.c, r.suite)
			if err := r.c.MarkSignedSeenBatch(context.Background(), []cache.SignedSeenMark{{CanonicalScheme: "http",
				CanonicalHost: "h", Base: "/flat", SeenAt: 1}}); err != nil {
				r.t.Fatal(err)
			}
			return r.release("x")
		}, ErrAdoptionUnsignedDowngradeRefused},
		{"signed current snapshot", func(r *unsignedRig) []byte {
			if err := r.ad.Run(context.Background(), r.suite, simtest.ClearsignedFixture(string(r.release("signed"))), "", ""); err != nil {
				r.t.Fatal(err)
			}
			if err := r.c.ClearSignedSeenForTest(context.Background()); err != nil { // the prior form alone refuses
				r.t.Fatal(err)
			}
			return r.release("x")
		}, ErrAdoptionUnsignedDowngradeRefused},
		{"too many members", func(r *unsignedRig) []byte {
			return hashRelease("Origin: big\n", true, false, big, nil)
		}, ErrAdoptionUnsignedTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newUnsignedRig(t, 0, 0)
			rel := tc.setup(r)
			before, fetched := r.current(), len(r.members.requested())
			err := r.ad.RunUnsigned(context.Background(), r.suite, rel, "", "")
			if !errors.Is(err, tc.want) {
				t.Fatalf("RunUnsigned = %v, want %v", err, tc.want)
			}
			if r.current() != before {
				t.Fatal("a refused unsigned adoption moved the current snapshot")
			}
			if n := len(r.members.requested()) - fetched; n != 0 {
				t.Fatalf("a refused unsigned adoption fetched %d members, want none", n)
			}
		})
	}
}

// Ruling R-E1 (overview, D carry-forward; DoD): before an unsigned commit
// the adopter re-runs the cached signed-anchor mark. A suite whose marker
// is missing but whose cached InRelease (or Release.gpg) passes the sniff
// — its own, or its /. twin's (design review I-3: the twins share the
// marker) — is marked and the commit is refused; a cached plain-text "InRelease"
// (the §6.1 legacy case) earns no marker and the adoption commits, and so
// does one whose pool file is gone (no evidence).
func TestRunUnsigned_RemarksCachedSignedAnchors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		file    string
		body    []byte
		gone    bool
		twin    bool // the suite is /flat; the row is its /. twin's, /flat/./<file>
		refused bool
	}{
		{"sniff-passing InRelease", "InRelease", simtest.ClearsignedFixture("Origin: signed\n"), false, false, true},
		{"sniff-passing Release.gpg", "Release.gpg", []byte("-----BEGIN PGP SIGNATURE-----\n\nwsBc\n=AAAA\n-----END PGP SIGNATURE-----\n"), false, false, true},
		{"the twin's sniff-passing InRelease", "InRelease", simtest.ClearsignedFixture("Origin: twin\n"), false, true, true},
		{"plain-text InRelease", "InRelease", []byte("Origin: plain\nSHA256:\n"), false, false, false},
		{"pool file gone", "InRelease", simtest.ClearsignedFixture("Origin: gone\n"), true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newUnsignedRig(t, 0, 0)
			dir := r.suite.SuitePath
			if tc.twin {
				r.suite.SuitePath, dir = "/flat", "/flat/."
				trackSuite(t, r.c, r.suite)
				r.members.put("http://h/flat/main/binary-amd64/Packages", r.pkgs)
			}
			h := r.cacheAnchorAt(dir, tc.file, tc.body)
			if tc.gone {
				if err := os.Remove(r.c.BlobPath(h)); err != nil {
					t.Fatal(err)
				}
			}
			if r.marked() {
				t.Fatal("setup: the suite already has a marker")
			}
			err := r.ad.RunUnsigned(context.Background(), r.suite, r.release("unsigned"), "", "")
			if !tc.refused {
				if err != nil || r.current() == 0 || r.marked() {
					t.Fatalf("RunUnsigned = %v, current %d, marked %v; want adopted, no marker", err, r.current(), r.marked())
				}
				return
			}
			if !errors.Is(err, ErrAdoptionUnsignedDowngradeRefused) || !errors.Is(err, cache.ErrUnsignedDowngradeRefused) {
				t.Fatalf("RunUnsigned = %v, want the commit's never-downgrade refusal", err)
			}
			if r.current() != 0 || !r.marked() {
				t.Fatalf("current %d, marked %v; want no snapshot and the re-mark's marker", r.current(), r.marked())
			}
		})
	}
}

// Ruling R-E1, fail closed: when signed evidence cannot be read, or the
// re-mark cannot be written, the unsigned adoption is refused, never
// committed — and the fault is logged (design review M-1: the refusal
// alone would read "the suite has signed history"), at ERROR for a pool
// read error and at WARN for a failed mark.
func TestRunUnsigned_FailedRemarkRefuses(t *testing.T) {
	for how, level := range map[string]string{"unreadable pool file": "ERROR", "failed mark write": "WARN"} {
		t.Run(how, func(t *testing.T) {
			r := newUnsignedRig(t, 0, 0)
			h := r.cacheAnchor("InRelease", simtest.ClearsignedFixture("Origin: signed\n"))
			switch how {
			case "unreadable pool file": // a directory where the pool file should be: a read error, not "does not exist"
				if err := os.Remove(r.c.BlobPath(h)); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(r.c.BlobPath(h), 0o700); err != nil {
					t.Fatal(err)
				}
			case "failed mark write":
				db, err := sql.Open("sqlite", filepath.Join(r.c.Dir(), "cache.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = db.Close() }()
				if _, err := db.Exec(`CREATE TRIGGER no_marks BEFORE INSERT ON suite_signed_seen
  BEGIN SELECT RAISE(ABORT, 'marker writes fail'); END`); err != nil {
					t.Fatal(err)
				}
			}
			err := r.ad.RunUnsigned(context.Background(), r.suite, r.release("unsigned"), "", "")
			if !errors.Is(err, ErrAdoptionUnsignedDowngradeRefused) || errors.Is(err, cache.ErrUnsignedDowngradeRefused) {
				t.Fatalf("RunUnsigned = %v, want the re-mark's refusal (before the commit)", err)
			}
			if r.current() != 0 {
				t.Fatal("committed although the re-mark failed")
			}
			if recs := r.records("adoption: signed-anchor re-mark failed"); len(recs) != 1 || recs[0]["level"] != level || recs[0]["err"] == "" {
				t.Fatalf("re-mark fault records %v, want one at %s with the error", recs, level)
			}
		})
	}
}

// Spec C5 / §7: reconcile builds no package_hash row for an unsigned
// snapshot (buildPackageHashes takes the form).
func TestReconcile_UnsignedSnapshotGetsNoPackageHash(t *testing.T) {
	r := newUnsignedRig(t, 0, 0)
	ctx := context.Background()
	all := fakePackagesStanzas(map[string]string{"pool/main/f/foo_1.deb": testHex64})
	r.members.put("http://h/u/dists/s/main/binary-all/Packages", all)
	rel := hashRelease("Origin: u\n", true, false, map[string][]byte{"main/binary-all/Packages": all}, nil)
	if err := r.ad.RunUnsigned(ctx, r.suite, rel, "", ""); err != nil {
		t.Fatal(err)
	}
	id := r.current()
	if err := r.c.DeleteSnapshotMembersForTest(ctx, id, "main/binary-all/Packages"); err != nil {
		t.Fatal(err)
	}
	if healed, err := r.ad.ReconcileSnapshot(ctx, r.suite, id, true); err != nil || healed != 1 {
		t.Fatalf("ReconcileSnapshot = %d, %v; want one member healed", healed, err)
	}
	if n := packageHashCount(t, r.c, id); n != 0 {
		t.Fatalf("after reconcile the unsigned snapshot has %d package_hash rows, want none", n)
	}
}

// packageHashCount counts the snapshot's package_hash rows directly: the
// strict-mode reads filter unsigned snapshots out (Task 1), so only a raw
// count shows that none were written.
func packageHashCount(t *testing.T, c *cache.Cache, id int64) int {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(c.Dir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM package_hash WHERE snapshot_id = ?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Spec C8 for the unsigned form: a first adoption whose Release is the
// url_path Release clients were served skips the barrier, like the
// detached form.
func TestRunUnsigned_FirstAdoptionOfTheServedReleaseSkipsTheBarrier(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newUnsignedRig(t, 10*time.Second, 0)
		rel := r.release("served")
		r.cacheAnchor("Release", rel)
		tok := r.gate.AnchorStart(context.Background(), flipgate.Key("http", "h", r.suite.SuitePath))
		defer tok.Done()
		start := time.Now()
		if err := r.ad.RunUnsigned(context.Background(), r.suite, rel, "", ""); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d != 0 || r.current() == 0 {
			t.Fatalf("waited %v, current %d; want an immediate commit", d, r.current())
		}
		if evs := r.records("adoption_flip_barrier"); len(evs) != 1 || evs[0]["outcome"] != "not_needed" {
			t.Fatalf("adoption_flip_barrier = %v, want one not_needed", evs)
		}
	})
}

// Ruling R-E2 and the phase E DoD's never-downgrade race (spec C5, §7):
// an eligible unsigned adoption is paused before its commit while a
// client's miss stores a signed anchor for the suite with its marker (the
// handler's writer operation); resumed, the commit's in-transaction
// recheck refuses it — no pointer change, no rows — even though its flip
// barrier outlived adoption.max_duration. The refusal is a state: the
// outcome is refused, the record says unsigned_downgrade_refused, the
// barrier event says refused, and nothing is logged at WARN or above.
func TestCheck_UnsignedRefusedInsideTheCommitIsAState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		r := newUnsignedRig(t, 10*time.Second, 20*time.Second)
		ring := observability.NewRing(10)
		checkLogs := &lockedBuffer{}
		chk, err := New(Config{Cache: r.c, Fetcher: &changedUpstream{}, HostLimiter: hostsem.New(8),
			Adopter: r.ad, AdoptionRing: ring, Logger: slog.New(slog.NewJSONHandler(checkLogs, nil))})
		if err != nil {
			t.Fatal(err)
		}
		key := flipgate.Key("http", "h", r.suite.SuitePath)
		tok := r.gate.AnchorStart(ctx, key) // a client reads the old (url_path) anchor: phase A waits
		var once sync.Once
		r.ad.beforeCommit = func() {
			once.Do(func() {
				// The race: a signed InRelease is served on a miss, with its
				// marker, while the adoption is paused before its commit.
				body := simtest.ClearsignedFixture("Origin: now signed\n")
				h, err := r.ad.writeBlobBytes(ctx, body)
				if err != nil {
					t.Error(err)
					return
				}
				now := time.Now().Unix()
				path := r.suite.SuitePath + "/InRelease"
				if _, err := r.c.StoreFetched(ctx, cache.FetchedRow{Row: cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h",
					Path: path, BlobHash: &h, UpstreamURL: "http://h" + path, IsMetadata: true, LastRequestedAt: &now,
					RequestCount: 1, LastFetchedAt: &now}, Suite: cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h",
					SuitePath: r.suite.SuitePath}, MarkSeenAt: now}, cache.SuiteCaps{}, nil); err != nil {
					t.Error(err)
				}
			})
		}
		rel := r.release("unsigned")
		req := &adoptionRequest{suite: r.suite, form: adoptionFormUnsigned, releaseBytes: rel,
			key: contentKey(adoptionFormUnsigned, nil, rel, nil)}
		mu := &sync.Mutex{}
		mu.Lock()
		before := adoptionTotalFor(t, "h")
		chk.spawnAdoption(mu, req, mustAcquire(t, chk.admission, "h", false))
		time.Sleep(25 * time.Second) // the adoption waits in the barrier; its 20 s deadline passes
		tok.Done()                   // the commit runs 10 s later, after the race
		chk.WaitForAdoptions()
		if !r.marked() || r.current() != 0 {
			t.Fatalf("marked %v, current %d; want the served anchor's marker and no unsigned snapshot", r.marked(), r.current())
		}
		sf, err := r.c.GetSuiteFreshness(ctx, "http", "h", r.suite.SuitePath)
		if err != nil {
			t.Fatal(err)
		}
		if strOf(sf.AdoptionFailReason) != cache.ReasonUnsignedDowngradeRefused || !cache.IsRefusalReason(strOf(sf.AdoptionFailReason)) {
			t.Fatalf("record reason %q, want unsigned_downgrade_refused (a refusal state)", strOf(sf.AdoptionFailReason))
		}
		if evs := ring.Snapshot(); len(evs) != 1 || evs[0].Outcome != "refused" || evs[0].Reason != cache.ReasonUnsignedDowngradeRefused {
			t.Fatalf("admin ring %+v, want one refused/unsigned_downgrade_refused attempt", evs)
		}
		if got := adoptionTotalFor(t, "h") - before; got != 1 {
			t.Fatalf("acu_adoption_total rose by %v, want 1", got)
		}
		if evs := r.records("adoption_flip_barrier"); len(evs) != 1 || evs[0]["outcome"] != "refused" {
			t.Fatalf("adoption_flip_barrier = %v, want one with outcome refused", evs)
		}
		for _, logs := range []*lockedBuffer{r.logs, checkLogs} {
			for _, line := range strings.Split(logs.String(), "\n") {
				var m map[string]any
				if line != "" && json.Unmarshal([]byte(line), &m) == nil && (m["level"] == "WARN" || m["level"] == "ERROR") {
					t.Errorf("a refusal logged %v at %v", m["msg"], m["level"])
				}
			}
		}
		var runFailed int
		for _, line := range strings.Split(checkLogs.String(), "\n") {
			var m map[string]any
			if line != "" && json.Unmarshal([]byte(line), &m) == nil && m["msg"] == "adoption_run_failed" && m["level"] == "INFO" {
				runFailed++
			}
		}
		if runFailed != 1 {
			t.Fatalf("%d INFO adoption_run_failed lines, want 1", runFailed)
		}
	})
}

// Overview "E:" carry-forwards: contentKey keys the unsigned Release
// itself, domain-separated from the inline key of the same bytes (design
// review M-2); classifyAdoptionReason maps every refusal sentinel to its
// own refusal reason, and an unrecognised refusal to no refusal reason.
func TestUnsignedKeysAndReasons(t *testing.T) {
	a, b := []byte("Release a"), []byte("Release b")
	if got, want := contentKey(adoptionFormUnsigned, nil, a, nil), shaOf(append([]byte("unsigned\n"), a...)); got != want {
		t.Fatalf("unsigned key = %s, want sha256(unsigned + Release) %s", got, want)
	}
	if contentKey(adoptionFormUnsigned, nil, a, nil) == contentKey(adoptionFormInline, a, nil, nil) {
		t.Fatal("an unsigned Release and an InRelease with the same bytes share a content key")
	}
	if contentKey(adoptionFormUnsigned, nil, a, nil) == contentKey(adoptionFormUnsigned, nil, b, nil) {
		t.Fatal("two unsigned Releases share a content key")
	}
	for _, c := range []struct {
		err  error
		want string
	}{
		{ErrAdoptionUpstreamOlder, cache.ReasonUpstreamOlder},
		{ErrAdoptionRollbackRefused, cache.ReasonRollbackRefused},
		{ErrAdoptionUnsignedDowngradeRefused, cache.ReasonUnsignedDowngradeRefused},
		{ErrAdoptionUnsignedTooLarge, cache.ReasonUnsignedTooLarge},
	} {
		err := fmt.Errorf("adoption: %w", c.err)
		outcome := classifyAdoptionOutcome(err)
		if got := classifyAdoptionReason(err, outcome, nil); outcome != "refused" || got != c.want || !cache.IsRefusalReason(got) {
			t.Errorf("%v: outcome %q reason %q, want refused and %q", c.err, outcome, got, c.want)
		}
	}
	if got := classifyAdoptionReason(errors.New("x"), "refused", nil); cache.IsRefusalReason(got) {
		t.Errorf("an unrecognised refusal got the refusal reason %q", got)
	}
	if formName(adoptionFormUnsigned) != "unsigned" {
		t.Errorf("formName(unsigned) = %q", formName(adoptionFormUnsigned))
	}
	if f, ok := formOfSnapshot(&cache.SuiteSnapshot{Form: cache.FormUnsigned}); !ok || f != adoptionFormUnsigned {
		t.Errorf("formOfSnapshot(unsigned) = %v, %v", f, ok)
	}
}

// Ruling R-E1, fail closed: a url_path read error during the re-mark is
// not "no evidence" — the unsigned adoption is refused before its commit,
// and the fault is logged at WARN (design review M-1). (Proposed by the
// executability review; kills X09.)
func TestRunUnsigned_RemarkReadErrorRefuses(t *testing.T) {
	r := newUnsignedRig(t, 0, 0)
	db, err := sql.Open("sqlite", filepath.Join(r.c.Dir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`ALTER TABLE url_path RENAME TO url_path_unreadable`); err != nil {
		t.Fatal(err)
	}
	err = r.ad.RunUnsigned(context.Background(), r.suite, r.release("unsigned"), "", "")
	if !errors.Is(err, ErrAdoptionUnsignedDowngradeRefused) || errors.Is(err, cache.ErrUnsignedDowngradeRefused) {
		t.Fatalf("RunUnsigned = %v, want the re-mark's refusal (before the commit)", err)
	}
	if r.current() != 0 {
		t.Fatal("committed although the re-mark could not read the cached anchors")
	}
	if recs := r.records("adoption: signed-anchor re-mark failed"); len(recs) != 1 || recs[0]["level"] != "WARN" {
		t.Fatalf("re-mark fault records %v, want one at WARN", recs)
	}
}

// Ruling R-E1 for the /. twin: the re-mark marks the suite's base
// (aptpath.MarkerBase), which the commit's in-transaction recheck reads —
// not the twin's own path. (Proposed by the executability review; kills
// X10.)
func TestRunUnsigned_RemarkMarksTheTwinsBase(t *testing.T) {
	r := newUnsignedRig(t, 0, 0)
	r.suite.SuitePath = "/flat/."
	trackSuite(t, r.c, r.suite)
	r.members.put("http://h/flat/./main/binary-amd64/Packages", r.pkgs)
	r.cacheAnchor("InRelease", simtest.ClearsignedFixture("Origin: signed\n"))
	err := r.ad.RunUnsigned(context.Background(), r.suite, r.release("unsigned"), "", "")
	if !errors.Is(err, cache.ErrUnsignedDowngradeRefused) || r.current() != 0 {
		t.Fatalf("RunUnsigned = %v, current %d: want the commit's refusal and no snapshot", err, r.current())
	}
	if _, ok, err := r.c.SignedSeen(context.Background(), "http", "h", "/flat"); err != nil || !ok {
		t.Fatalf("no marker for the base /flat (ok %v, err %v)", ok, err)
	}
}

// Ruling R-E1: the re-mark's marker carries the cached anchor's last use,
// as the startup pass and the handler's backstop stamp it (phase F's
// orphan-marker sweep reads the stamp). (Proposed by the executability
// review; kills X11.)
func TestRunUnsigned_RemarkStampsTheAnchorsLastUse(t *testing.T) {
	r := newUnsignedRig(t, 0, 0)
	r.cacheAnchor("InRelease", simtest.ClearsignedFixture("Origin: signed\n"))
	_ = r.ad.RunUnsigned(context.Background(), r.suite, r.release("unsigned"), "", "")
	seen, ok, err := r.c.SignedSeen(context.Background(), "http", "h", r.suite.SuitePath)
	if err != nil || !ok || seen != 1_700_000_000 {
		t.Fatalf("marker = %d, %v, %v; want the cached anchor's last use 1700000000", seen, ok, err)
	}
}
```

Create `internal/admin/refusal_reasons_test.go` (ruling R-E2: every phase E refusal stays out of `refusalKeepsCurrent`):

```go
package admin

import (
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// Ruling R-E2 (phase E): only the refusals whose suite already serves
// content at least as new as upstream hide its lag. Every phase E refusal
// — the suite is in fact behind — keeps it, so a stuck suite stays visible.
func TestRefusalKeepsCurrent_OnlyUpstreamOlderAndRollback(t *testing.T) {
	for reason, want := range map[string]bool{
		cache.ReasonUpstreamOlder:            true,
		cache.ReasonRollbackRefused:          true,
		cache.ReasonUnsignedDowngradeRefused: false,
		cache.ReasonUnsignedUnconfirmed:      false,
		cache.ReasonUnsignedTooLarge:         false,
		cache.ReasonAnchorAbsent:             false,
		"refused":                            false, // classifyAdoptionReason's fallback
	} {
		if got := refusalKeepsCurrent(reason); got != want {
			t.Errorf("refusalKeepsCurrent(%q) = %v, want %v", reason, got, want)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -gcflags=-e ./internal/freshness/ -run 'TestRunUnsigned_|TestReconcile_UnsignedSnapshotGetsNoPackageHash|TestCheck_UnsignedRefusedInsideTheCommitIsAState|TestUnsignedKeysAndReasons'` (`-gcflags=-e`: every identifier below is named)
(`TestRefusalKeepsCurrent_OnlyUpstreamOlderAndRollback` passes already: it pins what `internal/admin` must keep.)
Expected: compile failure naming `unknown field AdoptUnsigned in struct literal of type AdoptionConfig`, `undefined: unsignedMemberCap`, `RunUnsigned`, `errUnsignedDisabled`, `ErrAdoptionUnsignedDowngradeRefused`, `ErrAdoptionUnsignedTooLarge`, `adoptionFormUnsigned`, `r.ad.beforeCommit undefined`, `r.ad.adoptUnsigned undefined`.

- [ ] **Step 3: Implement**

Create `internal/freshness/unsigned.go`:

```go
package freshness

// Spec C5 "Unsigned safeguards" (D6), the adopter's side: the unsigned
// form adopts a Release no signature covers, so it never vouches a .deb,
// never counts for strict mode, never writes a signed-seen marker, and is
// refused whenever the suite has signed history.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// unsignedMemberCap is spec C5 safeguard 4 (a C11 constant): an unsigned
// Release declaring more members than this is refused.
const unsignedMemberCap = 1000

var (
	// ErrAdoptionUnsignedDowngradeRefused refuses unsigned metadata for a
	// suite with signed history: a signed-seen marker for its base, a
	// signed current snapshot, or signed evidence the adopter could not
	// rule out (spec C5 safeguard 3; ruling R-E1). A refusal state, not a
	// failure (spec C7): recorded as unsigned_downgrade_refused.
	ErrAdoptionUnsignedDowngradeRefused = errors.New("adoption_unsigned_downgrade_refused")

	// ErrAdoptionUnsignedTooLarge refuses an unsigned Release that declares
	// more than unsignedMemberCap members (spec C5 safeguard 4). A refusal
	// state: recorded as unsigned_too_large.
	ErrAdoptionUnsignedTooLarge = errors.New("adoption_unsigned_too_large")

	// errUnsignedDisabled: adoption.adopt_unsigned is false. The checker
	// never requests an unsigned adoption then; this guards RunUnsigned.
	errUnsignedDisabled = errors.New("unsigned adoption is disabled (adoption.adopt_unsigned = false)")
)

// RunUnsigned executes the adoption flow for an unsigned suite: release is
// the Release body no signature covers (spec C5 "unsigned" form). Like
// Run and RunDetached it is synchronous, and the caller holds the suite
// mutex. The checker calls it only for a confirmed candidate (two
// observations, spec C5 safeguard 2); runShared re-checks eligibility and
// the commit re-checks never-downgrade in its transaction.
func (a *Adopter) RunUnsigned(ctx context.Context, suite SuiteRef, release []byte, etag, lastmod string) error {
	return a.runShared(ctx, suite, &adoptionPayload{
		form:         adoptionFormUnsigned,
		releaseBytes: release,
	}, etag, lastmod)
}

// unsignedEligible is spec C5's unsigned eligibility, checked before
// anything is downloaded (the "order of checks in runShared") and by the
// check before an unsigned request takes a slot: the knob is on, and the
// suite has no signed history — cache.CheckUnsignedDowngrade, the rule
// CommitAdoption re-reads in its transaction (a signed-seen marker for the
// base, or an adopted signed snapshot of the suite or its /. twin, under
// either scheme). It is the first of the two never-downgrade points; the
// second, the authority, is the commit's in-transaction recheck.
func (a *Adopter) unsignedEligible(ctx context.Context, suite SuiteRef) error {
	if !a.adoptUnsigned {
		return errUnsignedDisabled
	}
	err := a.cache.CheckUnsignedDowngrade(ctx, suite.CanonicalHost, suite.SuitePath)
	switch {
	case errors.Is(err, cache.ErrUnsignedDowngradeRefused):
		// %v, not %w: cache.ErrUnsignedDowngradeRefused in the chain means
		// the commit itself refused (runShared's Step 11).
		return fmt.Errorf("%w: %v", ErrAdoptionUnsignedDowngradeRefused, err)
	case err != nil:
		return fmt.Errorf("%w: %w", ErrAdoptionDBFailed, err)
	}
	return nil
}

// unsignedCap is spec C5 safeguard 4: ErrAdoptionUnsignedTooLarge when an
// unsigned Release declares more than unsignedMemberCap members (n is
// ParseRelease's count of merged member paths, the members runShared
// would fetch).
func unsignedCap(n int) error {
	if n <= unsignedMemberCap {
		return nil
	}
	return fmt.Errorf("%w: %d declared members, at most %d", ErrAdoptionUnsignedTooLarge, n, unsignedMemberCap)
}

// errRemarkPoolRead marks a re-mark that failed reading a pool file: an I/O
// fault, logged at ERROR (any other re-mark failure is a WARN).
var errRemarkPoolRead = errors.New("read the pool file")

// remarkSignedAnchors re-runs, for an unsigned commit, the cached
// signed-anchor mark of the suite (ruling R-E1, phase D's backstop rule):
// it reads the InRelease and Release.gpg url_path rows of the suite's base
// and of its /. twin (aptpath.MarkerBase(s) and MarkerBase(s)+"/.", which
// share the marker), sniffs their pool files here — never on the writer —
// and marks the base for each one that passes, stamped with the row's last
// use. The mark is enqueued and committed before the commit is enqueued, so
// the commit's in-transaction re-read (FIFO writer) sees it. A pool file
// that does not exist is no evidence (as in the startup marker pass); any
// other read error (errRemarkPoolRead for a pool file), and a failed mark,
// is an error: the caller refuses the commit.
//
// AIDEV-NOTE: this closes phase D's backstop residual where it matters — a
// tracked suite whose sniff-passing signed anchor stayed unmarked after a
// failed backstop, including one cached under the /. twin's path (design
// review I-3). Fail closed: never commit unsigned while signed evidence
// could not be ruled out, so a refusal from here can mean "could not rule
// out" (an I/O or database fault), not only "signed".
func (a *Adopter) remarkSignedAnchors(ctx context.Context, suite SuiteRef) error {
	base := aptpath.MarkerBase(suite.SuitePath)
	var marks []cache.SignedSeenMark
	for _, dir := range []string{base, base + "/."} {
		for _, name := range []string{"InRelease", "Release.gpg"} {
			p := dir + "/" + name
			row, err := a.cache.LookupURL(ctx, suite.CanonicalScheme, suite.CanonicalHost, p)
			switch {
			case errors.Is(err, cache.ErrNotFound):
				continue
			case err != nil:
				return fmt.Errorf("read %s: %w", p, err)
			case row.BlobHash == nil:
				continue
			}
			signed, err := sniffCachedAnchor(a.cache, p, *row.BlobHash)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				continue
			case err != nil:
				return fmt.Errorf("%w of %s: %w", errRemarkPoolRead, p, err)
			case !signed:
				continue
			}
			seen := anchorLastUse(row)
			if seen <= 0 {
				seen = wallclock.Unix()
			}
			marks = append(marks, cache.SignedSeenMark{CanonicalScheme: suite.CanonicalScheme,
				CanonicalHost: suite.CanonicalHost, Base: base, SeenAt: seen})
		}
	}
	if len(marks) == 0 {
		return nil
	}
	if err := a.cache.MarkSignedSeenBatch(ctx, marks); err != nil {
		return fmt.Errorf("mark: %w", err)
	}
	return nil
}

// anchorLastUse is a url_path row's last use — the newer of its
// last_requested_at and last_fetched_at (0 when neither is set) — the
// stamp a marker from it carries, as in the startup marker pass and the
// handler's backstop.
func anchorLastUse(row *cache.URLPath) int64 {
	var seen int64
	if row.LastRequestedAt != nil {
		seen = *row.LastRequestedAt
	}
	if row.LastFetchedAt != nil && *row.LastFetchedAt > seen {
		seen = *row.LastFetchedAt
	}
	return seen
}
```

In `internal/freshness/legacy_audit.go`, replace `markFromCachedSignedAnchors` with the following (it takes the logger: `RunLegacyTrustAudit` calls `markFromCachedSignedAnchors(ctx, c, logger)`), and add `sniffCachedAnchor` after it:

```go
// markFromCachedSignedAnchors is the C3 startup marker pass: every
// tracked suite whose cached InRelease or Release.gpg passes the sniff
// gets a marker stamped with that row's last use. Idempotent (markers
// keep the newest stamp), so it runs at every start.
func markFromCachedSignedAnchors(ctx context.Context, c *cache.Cache, logger *slog.Logger) (int, error) {
	anchors, err := c.ListTrackedSuiteAnchors(ctx)
	if err != nil {
		return 0, fmt.Errorf("legacy trust audit: list anchors: %w", err)
	}
	var marks []cache.SignedSeenMark
	for _, a := range anchors {
		ok, err := sniffCachedAnchor(c, a.Path, a.BlobHash)
		if err != nil {
			// Pool file gone or unreadable: no evidence either way
			// (deferred minor A-49: said at DEBUG, not silently).
			logger.Debug("legacy trust audit: cached anchor skipped",
				"canonical_host", a.CanonicalHost, "path", a.Path, "err", err)
			continue
		}
		if ok {
			marks = append(marks, cache.SignedSeenMark{CanonicalScheme: a.CanonicalScheme,
				CanonicalHost: a.CanonicalHost, Base: aptpath.MarkerBase(a.SuitePath), SeenAt: a.SeenAt})
		}
	}
	if err := c.MarkSignedSeenBatch(ctx, marks); err != nil {
		return 0, fmt.Errorf("legacy trust audit: mark anchors: %w", err)
	}
	return len(marks), nil
}

// sniffCachedAnchor reports whether the pool file of a cached signed
// anchor (path is <suite>/InRelease or <suite>/Release.gpg) passes its
// sniff (spec C3: the rule the handler marks by). The startup marker pass
// and the adopter's re-mark before an unsigned commit (ruling R-E1) share
// it, so they agree on what counts as signed. A pool file that does not
// exist is an error wrapping fs.ErrNotExist; any other path is never
// signed.
func sniffCachedAnchor(c *cache.Cache, path, blobHash string) (bool, error) {
	var file aptpath.SuiteFile
	switch {
	case strings.HasSuffix(path, "/InRelease"):
		file = aptpath.FileInRelease
	case strings.HasSuffix(path, "/Release.gpg"):
		file = aptpath.FileReleaseGPG
	default:
		return false, nil
	}
	head, err := readCapped(c.BlobPath(blobHash), int64(file.HeadBytes()))
	if err != nil && !errors.Is(err, errAnchorTooLarge) {
		return false, err
	}
	return file.Sniff(path, head), nil
}
```

In `internal/freshness/adoption.go`:

- The form type and payload:

```go
// adoptionForm distinguishes the anchor forms (SPEC2 §7.6.3, spec C5).
type adoptionForm int

const (
	adoptionFormInline   adoptionForm = iota // clearsigned InRelease
	adoptionFormDetached                     // Release + Release.gpg
	adoptionFormUnsigned                     // Release alone, unverified (spec C5, phase E)
)
```

  `adoptionPayload`'s doc becomes "Exactly one of (inlineBytes), (releaseBytes + sigBytes) or — for the unsigned form — (releaseBytes) is set on entry; runShared populates the *Hash fields once step 2 has stored the blobs in pool/.", and the detached fields' comment "Detached mode (form == adoptionFormDetached); the unsigned form uses releaseBytes and releaseHash alone."

- `AdoptionConfig`, after `FlipGate`:

```go
	// AdoptUnsigned is adoption.adopt_unsigned (spec C5 safeguard 1, D6):
	// whether a Release-only upstream is adopted in the unsigned form
	// (never vouching a .deb, never a downgrade from signed). The zero
	// value is off; config defaults it on.
	AdoptUnsigned bool
```

  `Adopter`, after `flipGate` (and `adoptUnsigned: cfg.AdoptUnsigned,` in `NewAdopter`'s literal after `flipGate:`):

```go
	// adoptUnsigned is adoption.adopt_unsigned (spec C5 safeguard 1).
	adoptUnsigned bool

	// beforeCommit, when set (tests only), runs inside the commit — after
	// the flip barrier, before CommitAdoption: the window spec C5's
	// in-transaction never-downgrade recheck closes.
	beforeCommit func()
```

- `runRequest`'s switch gains `case adoptionFormUnsigned: p.releaseBytes = req.releaseBytes`.

- `runShared`'s doc comment becomes:

```go
// runShared is the SPEC2 §7.5 adoption pipeline shared across the anchor
// forms. Form-specific dispatch happens at these points:
//   - Step 1: VerifyInline vs VerifyDetached; the unsigned form's
//     eligibility (spec C5) instead of a verification.
//   - Step 2: persist 1 blob (InRelease, or the unsigned Release) vs 2
//     blobs (Release, Release.gpg).
//   - Step 3: the unsigned form's member cap.
//   - Step 6: metadata-self rows — InRelease, Release + Release.gpg, or
//     Release (unsigned).
//   - Step 8: no package_hash rows for the unsigned form.
//   - Before the barrier: the unsigned form's re-mark (ruling R-E1).
//
// The other steps are identical: parse, candidate insert, member
// prefetch, by-hash, atomic flip, success log.
```

- Step 1's switch gains:

```go
	case adoptionFormUnsigned:
		// Spec C5: unsigned eligibility takes the verification's place —
		// the first never-downgrade point, before anything is downloaded.
		// Nothing verifies the text: it never vouches a .deb (Step 8).
		if err := a.unsignedEligible(ctx, suite); err != nil {
			return err // a refusal: the checker records and logs it
		}
		releaseText = p.releaseBytes
```

- Step 2's switch gains:

```go
	case adoptionFormUnsigned:
		rh, err := a.writeBlobBytes(ctx, p.releaseBytes)
		if err != nil {
			a.logger.Warn("adoption: persist Release failed",
				"canonical_host", suite.CanonicalHost,
				"suite_path", suite.SuitePath,
				"err", err,
			)
			return fmt.Errorf("%w: persist Release: %v", ErrAdoptionDBFailed, err)
		}
		p.releaseHash = rh
```

- Right after Step 3's `ParseRelease` error check:

```go
	// Spec C5 safeguard 4: an unsigned Release declaring more than
	// unsignedMemberCap members is refused before any download.
	if p.form == adoptionFormUnsigned {
		if err := unsignedCap(len(members)); err != nil {
			return err
		}
	}
```

- Step 4's candidate switch gains:

```go
	case adoptionFormUnsigned:
		// Spec C5: the unsigned Release sits in the single-anchor slot,
		// and the form is named explicitly — the cache never derives it.
		cand.InReleaseHash = &p.releaseHash
		cand.Form = cache.FormUnsigned
```

- Step 6: its comment ends "…detached mode contributes two (Release, Release.gpg); the unsigned form one (Release)." and the switch gains, first:

```go
	case adoptionFormUnsigned:
		memberRows = append(memberRows, cache.SnapshotMember{
			SnapshotID:     snapshotID,
			Path:           "Release",
			BlobHash:       p.releaseHash,
			DeclaredSHA256: p.releaseHash,
		})
```

- Step 8: the builder call becomes `a.buildPackageHashes(suite, snapshotID, cand.Form, members, fetchedMembers)`, followed by

```go
	// Spec C5 safeguard 5: no package_hash row on any path for the
	// unsigned form (buildPackageHashes returns none; the Sources and
	// pdiff builders are not run), so it vouches no .deb and hot prefetch
	// warms nothing for it.
	unsigned := cand.Form == cache.FormUnsigned
```

  and the `buildSourceHashes` and `buildPdiffHashes` blocks are each wrapped in `if !unsigned { … }` (their `sourceRows, _, err :=` / `pdiffRows, _, err :=` then declare inside the block).

- After the `p.stopErr(ctx)` check that precedes Step 11:

```go
	// Ruling R-E1: before an unsigned commit, re-run the cached
	// signed-anchor mark of the suite (the adoption holds its mutex), so
	// the commit's in-transaction marker re-read sees any signed anchor the
	// suite has cached — even one phase D's backstop failed to mark. A
	// failed re-mark refuses the commit (fail closed). It runs before the
	// barrier, outside the hold, under the shutdown signal (not the
	// adoption deadline: it would discard finished member work) and the
	// commit's bound.
	//
	// AIDEV-NOTE: the refusal is recorded as unsigned_downgrade_refused,
	// which here can mean "could not rule out signed history" (a pool or
	// database fault), so the fault itself is logged first: ERROR for a
	// pool read error, WARN otherwise (design review M-1).
	if p.form == adoptionFormUnsigned {
		mctx, cancel := context.WithTimeout(p.barrierCtx(ctx), commitTimeout)
		err := a.remarkSignedAnchors(mctx, suite)
		cancel()
		if err != nil {
			if stop := p.stopErr(ctx); stop != nil {
				return fmt.Errorf("adoption: shutdown before the commit: %w", stop)
			}
			level := slog.LevelWarn
			if errors.Is(err, errRemarkPoolRead) {
				level = slog.LevelError
			}
			a.logger.Log(ctx, level, "adoption: signed-anchor re-mark failed",
				"canonical_host", suite.CanonicalHost, "suite_path", suite.SuitePath, "err", err)
			return fmt.Errorf("%w: the cached signed anchors could not be re-marked: %w",
				ErrAdoptionUnsignedDowngradeRefused, err)
		}
	}
```

- In the `commit` closure, after the `commitDelay` sleep:

```go
		if a.beforeCommit != nil {
			a.beforeCommit() // test seam: the window the in-transaction recheck closes
		}
```

- Step 11's error handling, after the `flipgate.ErrAborted` case:

```go
		// Ruling R-E2: a refusal raised inside the commit transaction (the
		// never-downgrade recheck) is a state, not a failure — its own
		// sentinel, no WARN, no db_failed wrap — also after a barrier that
		// outlived adoption.max_duration (spawnAdoption keeps the commit's
		// own classification then).
		if errors.Is(err, cache.ErrUnsignedDowngradeRefused) {
			return fmt.Errorf("%w: %w", ErrAdoptionUnsignedDowngradeRefused, err)
		}
```

- Replace `barrierOutcome`, `servedAnchorUnchanged`, `formOfSnapshot` and `formName` with:

```go
// barrierOutcome is adoption_flip_barrier's outcome: the barrier's own;
// refused when the commit refused the candidate (ruling R-E2: an unsigned
// downgrade found inside the commit transaction is a state, not a failed
// commit); or commit_failed when the commit ran and failed. Either way the
// snapshot did not switch, and the timing fields still describe the
// barrier.
func barrierOutcome(outcome string, commitErr error) string {
	switch {
	case errors.Is(commitErr, cache.ErrUnsignedDowngradeRefused):
		return "refused"
	case commitErr != nil:
		return "commit_failed"
	}
	return outcome
}

// servedAnchorUnchanged reports whether the anchor this adoption commits
// is byte-for-byte the url_path anchor clients have been served: the
// primary anchor of its form (InRelease inline; Release for the detached
// and unsigned forms — index consistency depends on the Release text, not
// on its signature). A read error answers false, so the barrier runs (the
// safe direction); one other than "no row" is logged (deferred minor
// C-16). The read runs under the shutdown signal, like the barrier's own
// waits (ruling R-C2), never unbounded.
func (a *Adopter) servedAnchorUnchanged(ctx context.Context, suite SuiteRef, p *adoptionPayload) bool {
	path, want := suite.SuitePath+inReleaseFilename, p.inlineHash
	if p.form == adoptionFormDetached || p.form == adoptionFormUnsigned {
		path, want = suite.SuitePath+releaseFilename, p.releaseHash
	}
	rctx, cancel := context.WithTimeout(p.barrierCtx(ctx), commitTimeout)
	defer cancel()
	row, err := a.cache.LookupURL(rctx, suite.CanonicalScheme, suite.CanonicalHost, path)
	if err != nil && !errors.Is(err, cache.ErrNotFound) {
		a.logger.Warn("adoption: served anchor read failed; the flip barrier runs",
			"canonical_host", suite.CanonicalHost, "suite_path", suite.SuitePath, "path", path, "err", err)
	}
	return err == nil && row.BlobHash != nil && *row.BlobHash == want
}

// formOfSnapshot maps a snapshot's stored anchor form to the adoption
// form; (0, false) for nil or a form adoption cannot drift from.
func formOfSnapshot(snap *cache.SuiteSnapshot) (adoptionForm, bool) {
	if snap == nil {
		return 0, false
	}
	switch snap.Form {
	case cache.FormInline:
		return adoptionFormInline, true
	case cache.FormDetached:
		return adoptionFormDetached, true
	case cache.FormUnsigned:
		return adoptionFormUnsigned, true
	}
	return 0, false
}

// formName renders an adoptionForm as a stable string suitable for
// structured logs (the cache.AnchorForm values). Operators pivot
// adoption_success on this to track per-suite form drift over time.
func formName(f adoptionForm) string {
	switch f {
	case adoptionFormInline:
		return "inline"
	case adoptionFormDetached:
		return "detached"
	case adoptionFormUnsigned:
		return "unsigned"
	default:
		return "unknown"
	}
}
```

- `buildPackageHashes` takes the form: its doc comment gains the paragraph below, its signature becomes `func (a *Adopter) buildPackageHashes(suite SuiteRef, snapshotID int64, form cache.AnchorForm, allMembers, fetchedMembers []ReleaseMember) (packageHashBuildResult, error)`, and its body starts with the unsigned return:

```go
//
// form is the snapshot's anchor form: the unsigned form gets no row and no
// coverage (spec C5 safeguard 5 — no package_hash row on any path:
// adoption here, reconcile through the same call), so it vouches no .deb.
```

```go
	if form == cache.FormUnsigned {
		return packageHashBuildResult{}, nil
	}
```

In `internal/freshness/reconcile.go` (`ReconcileSnapshot`), the builder call becomes:

```go
	// Build package_hash rows for the healed indexes (declared set = full
	// re-parsed Release so coverage math matches adoption; fetched set =
	// the indexes we just healed). The snapshot's form decides: an
	// unsigned snapshot gets none (spec C5 safeguard 5).
	snap, err := a.cache.GetSuiteSnapshot(ctx, snapshotID)
	if err != nil {
		return 0, fmt.Errorf("reconcile: read snapshot: %w", err)
	}
	phRes, err := a.buildPackageHashes(suite, snapshotID, snap.Form, declared, healedDecl)
```

In `internal/freshness/retry.go`, replace `contentKey` with:

```go
// contentKey identifies the content an adoption attempted (spec C7).
// Any byte change is new content, attempted at once. Inline:
// sha256(InRelease). Detached: sha256(sha256(Release) || sha256(Release.gpg)),
// so a re-signature alone is new content too. Unsigned:
// sha256("unsigned\n" || Release), domain-separated from the inline key, so
// a record for an InRelease body never matches a Release with the same
// bytes (design review M-2: the "confirmed once" rule trusts a record with
// an equal key).
//
// AIDEV-NOTE: each form keys its own argument — inline the InRelease
// (inline), detached and unsigned the Release (release). Keying the
// unsigned Release through inline (nil) would give all unsigned content
// one key, and new content would be deferred (overview, B carry-forward).
// No stored record holds an unsigned key from before phase E.
func contentKey(form adoptionForm, inline, release, sig []byte) string {
	switch form {
	case adoptionFormDetached:
		r, g := sha256.Sum256(release), sha256.Sum256(sig)
		h := sha256.New()
		h.Write(r[:])
		h.Write(g[:])
		return hex.EncodeToString(h.Sum(nil))
	case adoptionFormUnsigned:
		h := sha256.New()
		h.Write([]byte("unsigned\n"))
		h.Write(release)
		return hex.EncodeToString(h.Sum(nil))
	}
	s := sha256.Sum256(inline)
	return hex.EncodeToString(s[:])
}
```

In `internal/freshness/metrics.go`: `classifyAdoptionOutcome`'s first case becomes

```go
	case errors.Is(err, ErrAdoptionUpstreamOlder), errors.Is(err, ErrAdoptionRollbackRefused),
		errors.Is(err, ErrAdoptionUnsignedDowngradeRefused), errors.Is(err, ErrAdoptionUnsignedTooLarge):
		return "refused"
```

and `classifyAdoptionReason`'s `"refused"` case becomes

```go
	case "refused":
		// AIDEV-NOTE: one case per refusal sentinel (overview, B
		// carry-forward). A refusal recorded under another reason would be
		// misread — rollback_refused is in admin's refusalKeepsCurrent, which
		// hides the lag of a suite that is in fact behind — so an
		// unrecognised refusal falls back to the outcome, which the admin
		// page shows as a failure.
		switch {
		case errors.Is(err, ErrAdoptionUpstreamOlder):
			return cache.ReasonUpstreamOlder
		case errors.Is(err, ErrAdoptionRollbackRefused):
			return cache.ReasonRollbackRefused
		case errors.Is(err, ErrAdoptionUnsignedDowngradeRefused):
			return cache.ReasonUnsignedDowngradeRefused
		case errors.Is(err, ErrAdoptionUnsignedTooLarge):
			return cache.ReasonUnsignedTooLarge
		}
		return outcome
```

The drift file's call sites:

```bash
sed -i 's/a.buildPackageHashes(suiteRef, 1, members, members)/a.buildPackageHashes(suiteRef, 1, cache.FormInline, members, members)/' internal/freshness/hot_prefetch_test.go
grep -c 'cache.FormInline, members, members' internal/freshness/hot_prefetch_test.go   # 4
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/freshness/ ./internal/cache/ ./internal/handler/ ./internal/admin/`
Expected: PASS.

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| `refusalKeepsCurrent` (`internal/admin/status.go`) also lists `cache.ReasonAnchorAbsent` | `TestRefusalKeepsCurrent_OnlyUpstreamOlderAndRollback` |
| the R-E1 block's condition → `false && p.form == adoptionFormUnsigned` | `TestRunUnsigned_RemarksCachedSignedAnchors/{sniff-passing_InRelease, sniff-passing_Release.gpg, the_twin's_sniff-passing_InRelease}`, `TestRunUnsigned_FailedRemarkRefuses/{unreadable_pool_file, failed_mark_write}` |
| `remarkSignedAnchors`: a failed mark returns nil | `TestRunUnsigned_FailedRemarkRefuses/failed_mark_write` |
| `remarkSignedAnchors`: any pool read error counts as no evidence | `TestRunUnsigned_FailedRemarkRefuses/unreadable_pool_file` |
| `remarkSignedAnchors`: a url_path read error counts as no evidence (`continue`) | `TestRunUnsigned_RemarkReadErrorRefuses` |
| `remarkSignedAnchors` reads only the suite's own rows (`range []string{suite.SuitePath}`) | `TestRunUnsigned_RemarksCachedSignedAnchors/the_twin's_sniff-passing_InRelease` |
| `remarkSignedAnchors` marks `suite.SuitePath`, not the base | `TestRunUnsigned_RemarkMarksTheTwinsBase` |
| `remarkSignedAnchors`: the stamp is not the row's last use (`anchorLastUse(row) * 0`) | `TestRunUnsigned_RemarkStampsTheAnchorsLastUse` |
| the re-mark fault is not logged (its level below DEBUG) | `TestRunUnsigned_FailedRemarkRefuses/{unreadable_pool_file, failed_mark_write}`, `TestRunUnsigned_RemarkReadErrorRefuses` |
| a pool read fault logged at WARN | `TestRunUnsigned_FailedRemarkRefuses/unreadable_pool_file` |
| Step 11 drops the `cache.ErrUnsignedDowngradeRefused` mapping | `TestCheck_UnsignedRefusedInsideTheCommitIsAState`, `TestRunUnsigned_RemarksCachedSignedAnchors/{sniff-passing_*, the_twin's_*}` |
| `barrierOutcome`: the refusal case → `case false:` | `TestCheck_UnsignedRefusedInsideTheCommitIsAState` |
| `unsignedEligible` ignores the signed history (`case false && errors.Is(…)`) | `TestRunUnsigned_EligibilityBeforeAnyDownload/{marker_for_the_base, marker_under_the_other_scheme, marker_for_the_twin's_base, signed_current_snapshot}` |
| drop the member cap (`unsignedCap(0)`) | `TestRunUnsigned_EligibilityBeforeAnyDownload/too_many_members` |
| `buildPackageHashes` builds rows for the unsigned form | `TestRunUnsigned_AdoptsWithoutVouching`, `TestReconcile_UnsignedSnapshotGetsNoPackageHash` |
| `contentKey` drops the unsigned case | `TestUnsignedKeysAndReasons` |
| `contentKey`'s unsigned case without the `"unsigned\n"` prefix | `TestUnsignedKeysAndReasons` |
| `classifyAdoptionReason`'s refusal default → `cache.ReasonRollbackRefused` | `TestUnsignedKeysAndReasons` |
| the unsigned candidate's `Form` → `cache.FormInline` | `TestRunUnsigned_AdoptsWithoutVouching` and every other unsigned test |
| `servedAnchorUnchanged` compares InRelease for the unsigned form | `TestRunUnsigned_FirstAdoptionOfTheServedReleaseSkipsTheBarrier` |
| `unsignedEligible` ignores the knob | `TestRunUnsigned_EligibilityBeforeAnyDownload/knob_off` |

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/freshness/unsigned.go internal/freshness/unsigned_test.go internal/freshness/adoption.go \
  internal/freshness/reconcile.go internal/freshness/retry.go internal/freshness/metrics.go internal/freshness/legacy_audit.go \
  internal/admin/refusal_reasons_test.go
git add internal/freshness/unsigned.go internal/freshness/unsigned_test.go internal/freshness/adoption.go \
  internal/freshness/reconcile.go internal/freshness/retry.go internal/freshness/metrics.go \
  internal/freshness/legacy_audit.go internal/freshness/hot_prefetch_test.go internal/admin/refusal_reasons_test.go
git commit -m "Adopt unsigned Release-only suites, never as a downgrade, and record an in-commit refusal as a state"
```

(`gofmt -l` must print nothing; `hot_prefetch_test.go` is a drift file and is not passed to it.)

### Task 4: One check tail (R-E3): the check paths move to `check.go`, share their tail, and an unexpected status is a failed check

Ruling R-E3 (overview "E:" carry-forward from the phase B reviews): before the probe tree is added, the duplicated inline/detached blocks fold into one shared tail (`finishCheck`: persist, record-clearing, the `freshness_check` line and its counter), one failed-check record (`failCheck`, today's `recordUnknownEnd`), one 304/unchanged helper (`steadyResult`), one baseline read (`baseline`: the current snapshot's anchor hash through its form, else the url_path blob, and `signedCurrent`) and one admission step (`admit`: the twice-copied no-slot `tryAcquire` block). The unexpected-status default arms — today an ERROR `freshness: unexpected success status` with no counter — become failed checks on `failCheck`'s line, counted `acu_freshness_check_total{result="failed"}` (deferred minors B-34, B-35). Behaviour is otherwise unchanged: no existing test changes. `snapshotAnchorHash` and the detached baseline read the snapshot through `cache.SuiteSnapshot.AnchorHash` (Task 1), the overview's "both cachedHash derivations and snapshotAnchorHash's Release case need Form dispatch" carry-forward. Deferred minors B-23 (`applyClamp` keeps `cur` in step, so the inline check's detached fallback does not clamp and count a second time), B-33 (a stall-on-`Release.gpg` synctest pins that the signature GET runs under the check deadline) and B-25 (the detached path's extra `Release.gpg` GET for deferred content, kept and documented in an AIDEV-NOTE) sit in this code.

The comment-only references to the old names are renamed mechanically (`sed`, ruling PD-1: no logic change in those files).

**Files:**
- Create: `internal/freshness/check.go`, `internal/freshness/check_tail_test.go`
- Modify: `internal/freshness/freshness.go` (remove `checkLocked`, `detectForm`, `checkLockedInline`, `checkLockedDetached`, `recordUnknownEnd`; imports; `applyClamp`; `snapshotAnchorHash`; comments), and by `sed` the comments in `internal/freshness/check_streak_test.go`, `internal/freshness/freshness_test.go`, `internal/handler/phase2_detached_gpg_forgery_test.go`

**Interfaces:**
- Consumes: Task 1's `cache.SuiteSnapshot.AnchorHash`. Test helpers `newFakeCache`, `fakeCache.putURL/putSuite/putSnapshot/track/suite`, `seedFakeAnchor`, `testHex64`, `hashOf`, `i64Of`, `discardLogger`, `lockedBuffer`, `renderedMetric`, `passThroughVerifier`, `newFakeFetcher`; `simtest.NewUpstream`, `simtest.Serve`, `simtest.Stall`, `simtest.OpenCache`.
- Produces (package freshness): `checkEnd{form, result string; status int; reason string; clearRecord, signedCurrent bool}`, `(*Checker).finishCheck(ctx, out, cur, suite, e checkEnd) *repairRequest`, `(*Checker).failCheck(ctx, out, cur, suite, nowUnix int64, form string, err error)`, `steadyResult(cur, res, newHash, cachedHash string, gate retryGate, nowUnix int64) (result string, clearRecord, ok bool)`, `bodyHash(res) string`, `(*Checker).baseline(ctx, cur, suite, row *cache.URLPath, name string) (hash string, signedCurrent bool)`, `(*Checker).admit(out, cur, req, signedCurrent bool, prevSuccess *int64, result, queued string) (*adoptionRequest, string)`, `errUnexpectedStatus`, `checkInline`, `checkDetached` (renamed from `checkLocked…`); test helpers `checkTotal(t, result, host) float64`, `logRecords(t, buf, msg)`, `seedDetachedSuite(fc, host)` (`check_tail_test.go`).

- [ ] **Step 1: Write the failing tests**

Create `internal/freshness/check_tail_test.go`:

```go
package freshness

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// checkTotal returns acu_freshness_check_total for result on host.
func checkTotal(t *testing.T, result, host string) float64 {
	t.Helper()
	var buf bytes.Buffer
	metrics.Default.Render(&buf)
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(line, "acu_freshness_check_total{") &&
			strings.Contains(line, `result="`+result+`"`) && strings.Contains(line, `host="`+host+`"`) {
			v, err := strconv.ParseFloat(line[strings.LastIndexByte(line, ' ')+1:], 64)
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
	}
	return 0
}

// logRecords returns the JSON log records in buf whose msg is msg.
func logRecords(t *testing.T, buf *lockedBuffer, msg string) []map[string]any {
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

// seedDetachedSuite gives the fake cache's /r/dists/x on host a current
// detached snapshot with cached Release and Release.gpg anchors.
func seedDetachedSuite(fc *fakeCache, host string) {
	rel, sig := testHex64, hashOf([]byte("a signature"))
	id := int64(7)
	fc.putSnapshot(cache.SuiteSnapshot{SnapshotID: id, CanonicalScheme: "http", CanonicalHost: host, SuitePath: "/r/dists/x",
		Form: cache.FormDetached, ReleaseHash: &rel, ReleaseGPGHash: &sig})
	fc.putSuite(cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: host, SuitePath: "/r/dists/x", CurrentSnapshotID: &id})
	for path, h := range map[string]string{"/r/dists/x/Release": rel, "/r/dists/x/Release.gpg": sig} {
		h := h
		fc.putURL(cache.URLPath{CanonicalScheme: "http", CanonicalHost: host, Path: path, BlobHash: &h,
			UpstreamURL: "http://" + host + path, IsMetadata: true})
	}
}

// Ruling R-E3 (overview, B carry-forward; deferred minors B-34, B-35): a
// success status other than 200 and 304 — a fetch.Conditional contract
// violation — is a failed check on the one failed-check line, with its
// form, counted as failed, extending the streak and leaving
// last_success_at alone, for both forms.
func TestCheck_UnexpectedStatusIsAFailedCheck(t *testing.T) {
	for _, form := range []string{"inline", "detached"} {
		t.Run(form, func(t *testing.T) {
			host := "unexpected-" + form
			fc := newFakeCache()
			url := "http://" + host + "/r/dists/x/InRelease"
			if form == "inline" {
				stale := testHex64
				fc.putURL(cache.URLPath{CanonicalScheme: "http", CanonicalHost: host, Path: "/r/dists/x/InRelease",
					BlobHash: &stale, UpstreamURL: url, IsMetadata: true})
				fc.track("http", host, "/r/dists/x")
			} else {
				seedDetachedSuite(fc, host)
				url = "http://" + host + "/r/dists/x/Release"
			}
			success := int64(1_799_000_000)
			s, _ := fc.suite("http", host, "/r/dists/x")
			s.LastSuccessAt = &success
			fc.putSuite(s)
			up := simtest.NewUpstream()
			up.Set(url, func(context.Context, string) (*fetch.ConditionalResult, error) {
				return &fetch.ConditionalResult{Status: 206}, nil
			})
			logs := &lockedBuffer{}
			c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8),
				Logger: slog.New(slog.NewJSONHandler(logs, nil)), now: func() time.Time { return time.Unix(1_800_000_000, 0) }})
			if err != nil {
				t.Fatal(err)
			}
			before := checkTotal(t, "failed", host)
			rep := c.check(context.Background(), SuiteRef{CanonicalScheme: "http", CanonicalHost: host, SuitePath: "/r/dists/x"}, checkOpts{})
			if rep.result != "failed" {
				t.Fatalf("report = %q, want failed", rep.result)
			}
			if got := checkTotal(t, "failed", host) - before; got != 1 {
				t.Fatalf("acu_freshness_check_total{result=failed} rose by %v, want 1", got)
			}
			lines := logRecords(t, logs, "freshness check failed")
			if len(lines) != 1 || lines[0]["form"] != form || !strings.Contains(lines[0]["err"].(string), "unexpected success status 206") {
				t.Fatalf("failed-check lines %v, want one for form %s naming the status", lines, form)
			}
			if n := len(logRecords(t, logs, "freshness_check")); n != 0 {
				t.Fatalf("%d freshness_check lines for a failed check, want 0", n)
			}
			row, _ := fc.suite("http", host, "/r/dists/x")
			if row.CheckFailCount != 1 || row.LastCheckAt == nil || *row.LastCheckAt != 1_800_000_000 || i64Of(row.LastSuccessAt) != success {
				t.Fatalf("row = %+v; want the streak at 1, last_check_at now, last_success_at kept", row)
			}
		})
	}
}

// Deferred minor B-34: a transport failure counts as failed too.
func TestCheck_TransportFailureCountsAsFailed(t *testing.T) {
	fc := newFakeCache()
	seedFakeAnchor(fc)
	fc.track("http", "h", "/r/dists/x")
	up := simtest.NewUpstream()
	up.Set("http://h/r/dists/x/InRelease", func(context.Context, string) (*fetch.ConditionalResult, error) {
		return nil, fetch.ErrUpstreamServerError
	})
	c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8), Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	before := checkTotal(t, "failed", "h")
	c.Check(context.Background(), "http", "h", "/r/dists/x")
	if got := checkTotal(t, "failed", "h") - before; got != 1 {
		t.Fatalf("acu_freshness_check_total{result=failed} rose by %v, want 1", got)
	}
}

// Deferred minor B-33: the Release.gpg GET of a detached check runs under
// the check deadline: a signature that stalls ends the check at
// checkDeadline (spec C4: D in the latency bound), not later.
func TestCheck_DetachedSignatureStallEndsAtTheCheckDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCache()
		seedDetachedSuite(fc, "h")
		up := simtest.NewUpstream()
		up.Set("http://h/r/dists/x/Release", simtest.Serve([]byte("a new Release\n")))
		up.Set("http://h/r/dists/x/Release.gpg", simtest.Stall())
		c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8), Logger: discardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		rep := c.check(context.Background(), SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}, checkOpts{})
		if d := time.Since(start); d != checkDeadline {
			t.Fatalf("the check took %v, want the %v check deadline", d, checkDeadline)
		}
		if rep.result != "changed" {
			t.Fatalf("result %q, want changed (the signature fetch failed; retried later)", rep.result)
		}
	})
}

// Deferred minor B-23: a next_retry_at left far in the future by a
// backward clock step is clamped once per check, also when the inline
// check falls back to the detached path.
func TestCheck_FutureRetryIsClampedOncePerCheck(t *testing.T) {
	fc := newFakeCache()
	seedFakeAnchor(fc)
	for path, h := range map[string]string{"/r/dists/x/Release": testHex64, "/r/dists/x/Release.gpg": hashOf([]byte("sig"))} {
		h := h
		fc.putURL(cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: path, BlobHash: &h,
			UpstreamURL: "http://h" + path, IsMetadata: true})
	}
	fc.track("http", "h", "/r/dists/x")
	now := time.Unix(1_800_000_000, 0)
	if err := fc.RecordAdoptionFailure(context.Background(), "http", "h", "/r/dists/x", cache.AdoptionFailure{
		Hash: testHex64, Reason: "gpg_failed", Count: 1, FirstAt: now.Unix(), LastAt: now.Unix(),
		NextRetryAt: now.Add(10 * time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	up := simtest.NewUpstream() // InRelease 404s: the never-adopted suite falls back to detached
	up.Set("http://h/r/dists/x/Release", simtest.Serve([]byte("a Release\n")))
	up.Set("http://h/r/dists/x/Release.gpg", simtest.Serve([]byte("a signature\n")))
	ad, err := NewAdopter(AdoptionConfig{Cache: simtest.OpenCache(t), Fetcher: newFakeFetcher(), Verifier: passThroughVerifier{},
		HostLimiter: hostsem.New(8), MaxConcurrent: 2, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8), Adopter: ad, Logger: discardLogger(),
		now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	parse := func() float64 {
		v, _ := strconv.ParseFloat(renderedMetric("acu_clock_steps_detected_total"), 64)
		return v
	}
	before := parse()
	c.Check(context.Background(), "http", "h", "/r/dists/x")
	c.WaitForAdoptions()
	if got := parse() - before; got != 1 {
		t.Fatalf("clock steps detected: %v, want 1 (one clamp per check)", got)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/freshness/ -run 'TestCheck_UnexpectedStatusIsAFailedCheck|TestCheck_TransportFailureCountsAsFailed|TestCheck_DetachedSignatureStallEndsAtTheCheckDeadline|TestCheck_FutureRetryIsClampedOncePerCheck'`
Expected: FAIL — `TestCheck_UnexpectedStatusIsAFailedCheck/{inline,detached}` (`report = "failed"` comes from the old default arm, but `acu_freshness_check_total{result=failed} rose by 0, want 1` and no `freshness check failed` line) and `TestCheck_FutureRetryIsClampedOncePerCheck` (`clock steps detected: 2, want 1`); the other two pass today (they pin behaviour the move must keep).

- [ ] **Step 3: Implement**

Create `internal/freshness/check.go` — `checkLocked` and `detectForm` move here unchanged, `checkLockedInline`/`checkLockedDetached` become `checkInline`/`checkDetached` over the shared helpers, and `recordUnknownEnd` becomes `failCheck`:

```go
package freshness

// The freshness check (spec C4), run with the suite mutex held: checkLocked
// reads the row, applies the cooldown and dispatches on the suite's anchor
// form. Every answered check ends in one tail, finishCheck, and every
// failed one in failCheck (ruling R-E3: record-clearing, the
// freshness_check line and its counter live in one place).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
)

// checkLocked is the body of Check, run with the per-suite mutex held.
// It fills out: out.req is non-nil iff the observed result is "changed",
// the caller has an Adopter wired in (Phase 2 path) AND an admission slot
// was free (out.lease); the caller uses this signal to spawn the adoption
// goroutine. On a confirmed-unchanged result planRepair sets out.repair
// and out.lease when there is something to heal and a slot was free.
//
// Form dispatch (SPEC2 §7.6.3):
//   - If the suite has a current snapshot, the form is read from the
//     snapshot row's anchor_form (snap.Form): detached → detached,
//     otherwise inline. The check then issues a conditional GET against
//     InRelease (inline) or Release (detached).
//   - For first-ever checks (no current snapshot), inline is tried
//     first. If the inline conditional GET 404s AND a Release url_path
//     row exists, we fall back to the detached path. This is what
//     bootstraps detached-mode adoption for upstreams that ship only
//     Release + Release.gpg.
//
// Outcomes (all paths must update suite_freshness so cooldown applies
// to the next attempt):
//
//   - Cooldown gate fails: no DB write, return nil (reported as
//     skipped). The scheduler's slot-driven re-check of a waiting suite
//     and its check of a suite with a pending change bypass the gate
//     (opts.bypassCooldown, spec C4).
//   - Metadata url_path row absent: no DB write (we have nothing to
//     check against), return nil — first request for this suite will
//     land in url_path through the normal miss path.
//   - 304: last_check_at = last_success_at = now.
//   - 200, body hash matches cached: bump last_check_at, last_success_at,
//     and refresh validators (upstream may have rotated etag/lastmod
//     while bytes are unchanged); clear any adoption failure record.
//   - 200, body hash differs: bump last_check_at, last_success_at,
//     stamp inrelease_change_seen_at on first sighting only, return
//     *adoptionRequest.
//   - Same, but no admission slot is free (spec C4): "changed_queued",
//     no request, last_success_at left at its previous value; the
//     scheduler parks the suite until a slot it can use is released (a
//     client check's suite is parked by the next tick: its pending
//     change makes it due, see Checker.pendingChange).
//   - Spec C7 gate (retryGateFor, adopter wired only): while a failure
//     record exists and next_retry_at has not come, the GET carries the
//     failing content's validators, and a 304 or the same content key
//     is "changed_deferred" (no adoption); new content is attempted at
//     once; at next_retry_at the GET carries no validators.
//   - Error (network, 4xx, 5xx, check deadline — also one hit while
//     waiting for the host slot) or a success status other than 200 and
//     304: a failed check (failCheck) — last_check_at bumped,
//     check_fail_count extended (don't hammer a broken upstream), the
//     "freshness check failed" line and the failed counter;
//     last_success_at keeps its previous value. Every answered check
//     above resets check_fail_count to 0 (spec C4).
//   - Cancelled caller (shutdown), at the host slot or the GET: no DB
//     write; it says nothing about upstream.
//
// SPEC2 wiring: the body-hash comparison uses
// suite_snapshot.inrelease_hash (inline) or .release_hash (detached)
// when the suite has an adopted snapshot. Without this, every
// freshness check after a successful adoption would observe "changed"
// against the stale url_path.blob_hash and thrash the adoption
// candidate-uniqueness constraint forever.
func (c *Checker) checkLocked(ctx, fetchCtx context.Context, out *checkOutcome, scheme, host, suitePath string, opts checkOpts) {
	now := c.now()
	nowUnix := now.Unix()

	cur, err := c.cache.GetSuiteFreshness(ctx, scheme, host, suitePath)
	switch {
	case errors.Is(err, cache.ErrNotFound):
		// I-4: the row vanished after check's pre-read (cap eviction, later
		// the reaper). Nothing to check and nothing to write; check drops
		// the lock-map entry.
		out.vanished = true
		return
	case err != nil:
		c.logger.Warn("freshness: read suite row failed",
			"err", err,
			"canonical_host", host,
			"suite_path", suitePath,
		)
		return
	}

	// Cooldown gate. Spec C4 clock-step rule: a past-event stamp found in
	// the future (backward wall-clock step) is treated as infinitely old,
	// never as "just checked" — otherwise the suite freezes for the size
	// of the step (security finding I-7).
	if cur.LastCheckAt != nil && c.cooldown > 0 {
		last := time.Unix(*cur.LastCheckAt, 0)
		switch {
		case last.After(now.Add(clockStepTolerance)):
			c.clockStep("last_check_at", scheme, host, suitePath, *cur.LastCheckAt, now)
		case opts.bypassCooldown:
			// The scheduler's slot-driven re-check, or its check of a
			// suite with a pending change (spec C4).
		case now.Sub(last) < c.cooldown:
			return // out.report stays resultSkipped
		}
	}

	suite := SuiteRef{
		CanonicalScheme: scheme,
		CanonicalHost:   host,
		SuitePath:       suitePath,
	}

	// Form preference: if there's a current snapshot, the form is its
	// anchor_form (snap.Form). Otherwise default to inline; the inline
	// path triggers a detached fallback on 404.
	form := c.detectForm(ctx, cur)
	if form == adoptionFormDetached {
		out.req, out.repair = c.checkDetached(ctx, fetchCtx, out, cur, suite, now, nowUnix)
	} else {
		req, repair, fellBack := c.checkInline(ctx, fetchCtx, out, cur, suite, now, nowUnix)
		if fellBack {
			req, repair = c.checkDetached(ctx, fetchCtx, out, cur, suite, now, nowUnix)
		}
		out.req, out.repair = req, repair
	}
	if out.req == nil && out.repair != nil {
		c.planRepair(ctx, out)
	}
}

// detectForm reads the suite's current snapshot (if any) and returns
// its anchor_form (snap.Form): detached, or inline for anything else
// until phase E dispatches the unsigned forms. First-ever suites
// (cur.CurrentSnapshotID == nil) and snapshots that fail to load
// default to inline; the inline-path fallback handles the boot-strap
// case for detached-only upstreams.
//
// AIDEV-TODO(phase E): unsigned form dispatch.
func (c *Checker) detectForm(ctx context.Context, cur *cache.SuiteFreshness) adoptionForm {
	if cur.CurrentSnapshotID == nil {
		return adoptionFormInline
	}
	snap, err := c.cache.GetSuiteSnapshot(ctx, *cur.CurrentSnapshotID)
	if err != nil || snap == nil {
		return adoptionFormInline
	}
	if snap.Form == cache.FormDetached {
		return adoptionFormDetached
	}
	return adoptionFormInline
}

// checkEnd is how an answered check ended, for finishCheck: the
// freshness_check line's form, result, upstream status and (for
// changed_deferred and refusals) reason; whether the adoption failure
// record is cleared; and whether the current snapshot is signed (the
// scheduler's reserved-slot rule).
type checkEnd struct {
	form          string
	result        string
	status        int
	reason        string
	clearRecord   bool
	signedCurrent bool
}

// finishCheck is the one tail of every answered check (ruling R-E3): it
// persists the row, clears the failure record when upstream serves the
// current content again (spec C7), logs freshness_check, counts it,
// reports it to the scheduler, and returns the repair signal of a
// confirmed-unchanged suite with a current snapshot.
func (c *Checker) finishCheck(ctx context.Context, out *checkOutcome, cur *cache.SuiteFreshness, suite SuiteRef, e checkEnd) *repairRequest {
	host, suitePath := suite.CanonicalHost, suite.SuitePath
	if perr := c.cache.UpdateSuiteFreshness(ctx, *cur); perr != nil {
		c.logger.Warn("freshness: persist success failed",
			"err", perr,
			"canonical_host", host,
			"suite_path", suitePath,
		)
	}
	if e.clearRecord {
		if err := c.cache.ClearAdoptionFailure(ctx, suite.CanonicalScheme, host, suitePath); err != nil {
			c.logger.Warn("freshness: clear adoption failure failed",
				"err", err, "canonical_host", host, "suite_path", suitePath)
		}
	}
	logAttrs := []any{
		"canonical_host", host,
		"suite_path", suitePath,
		"form", e.form,
		"result", e.result,
		"upstream_status", e.status,
	}
	if e.reason != "" {
		logAttrs = append(logAttrs, "reason", e.reason)
	}
	out.report = checkReport{result: e.result, signedCurrent: e.signedCurrent}
	c.logger.Info("freshness_check", logAttrs...)
	freshnessCheckTotal.Inc(e.result, host)
	return repairIfUnchanged(e.result, suite, cur)
}

// failCheck is the bookkeeping of a check that ended unknown (spec C4): a
// Conditional error, the check deadline hit while waiting for the host
// slot, or a success status other than 200 and 304 (a fetch.Conditional
// contract violation; ruling R-E3). SPEC §7.2: last_check_at is bumped to
// space out the next attempt and the streak extends; last_success_at is
// left alone — it carries the "we know upstream is fine" signal that
// periodic_refresh uses. It logs "freshness check failed" and counts
// acu_freshness_check_total{result="failed"}, and reports failed.
//
// AIDEV-NOTE: ctx is the caller's context, never fetchCtx: the deadline
// that ended the check must not fail the write that records it.
// AIDEV-NOTE: in phase B every Conditional error extends the streak,
// including 401/403/404/410, which spec C4 classifies as absent, not
// unknown. Phase E's probe classification must reset the streak on
// absent, or anchor_absent suites inherit the hourly backoff.
func (c *Checker) failCheck(ctx context.Context, out *checkOutcome, cur *cache.SuiteFreshness, suite SuiteRef, nowUnix int64, form string, err error) {
	host, suitePath := suite.CanonicalHost, suite.SuitePath
	cur.LastCheckAt = &nowUnix
	cur.CheckFailCount++
	if perr := c.cache.UpdateSuiteFreshness(ctx, *cur); perr != nil {
		c.logger.Warn("freshness: persist failure-bump failed",
			"err", perr,
			"canonical_host", host,
			"suite_path", suitePath,
		)
	}
	c.logger.Info("freshness check failed",
		"err", err,
		"canonical_host", host,
		"suite_path", suitePath,
		"form", form,
		"result", "failed",
	)
	freshnessCheckTotal.Inc("failed", host)
	out.report.result = "failed"
}

// errUnexpectedStatus is a success status other than 200 and 304 from
// fetch.Conditional: a contract violation, a failed check (ruling R-E3).
var errUnexpectedStatus = errors.New("unexpected success status")

// steadyResult handles, for both signed forms (ruling R-E3: one 304/
// unchanged helper), the responses that say upstream still serves what
// the suite has: a 304 — which, while a failure record's validators were
// sent, says upstream still serves the failing content (spec C7
// changed_deferred) — and a 200 carrying the current anchor's bytes
// (newHash == cachedHash), whose validators are taken (the only response
// they are ever taken from, spec C4) and which clears a failure record
// (spec C7). ok is false for any other response.
func steadyResult(cur *cache.SuiteFreshness, res *fetch.ConditionalResult, newHash, cachedHash string, gate retryGate, nowUnix int64) (result string, clearRecord, ok bool) {
	switch {
	case res.Status == 304 && gate.active:
		setChangeSeen(cur, nowUnix)
		return "changed_deferred", false, true
	case res.Status == 304:
		// Upstream serves the cached anchor again; clear the pending
		// diagnostic (e.g. after an upstream rollback).
		cur.InReleaseChangeSeenAt = nil
		return "not_modified", false, true
	case res.Status == 200 && newHash == cachedHash:
		if res.ETag != "" {
			v := res.ETag
			cur.InReleaseETag = &v
		}
		if res.LastModified != "" {
			v := res.LastModified
			cur.InReleaseLastMod = &v
		}
		cur.InReleaseChangeSeenAt = nil
		return "unchanged", gate.active, true
	}
	return "", false, false
}

// baseline is what a check compares the current form's anchor file name
// ("InRelease" or "Release") with: the current snapshot's hash of that
// anchor — through the snapshot's form (cache.SuiteSnapshot.AnchorHash),
// since an unsigned snapshot keeps its Release in the single-anchor slot —
// else the url_path anchor's blob; and whether the current snapshot is
// signed (the reserved-slot rule, spec C4). hash is "" when there is no
// baseline: the check skips without a write (logged at DEBUG).
//
// AIDEV-NOTE: the snapshot is authoritative once a suite is adopted:
// comparing with the url_path blob instead would see "changed" after
// every adoption and thrash the candidate-uniqueness constraint.
func (c *Checker) baseline(ctx context.Context, cur *cache.SuiteFreshness, suite SuiteRef, row *cache.URLPath, name string) (hash string, signedCurrent bool) {
	if cur.CurrentSnapshotID != nil {
		snap, err := c.cache.GetSuiteSnapshot(ctx, *cur.CurrentSnapshotID)
		if err != nil {
			c.logger.Warn("freshness: snapshot lookup failed; falling back to url_path",
				"err", err,
				"snapshot_id", *cur.CurrentSnapshotID,
				"canonical_host", suite.CanonicalHost,
				"suite_path", suite.SuitePath,
			)
		} else {
			signedCurrent = snap.Form == cache.FormInline || snap.Form == cache.FormDetached
			if h := snap.AnchorHash(name); h != nil {
				hash = *h
			}
		}
	}
	if hash == "" && row.BlobHash != nil {
		hash = *row.BlobHash
	}
	if hash == "" {
		c.logger.Debug("freshness: no baseline anchor hash, skipping",
			"canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath,
			"anchor", name,
		)
	}
	return hash, signedCurrent
}

// admit takes an admission slot for req (spec C4: TryAcquire before
// spawning, never wait) and returns req with the result to log: result
// when a slot was free (out.lease holds it), or queued with no request
// when none was — the suite's last_success_at then keeps its previous
// value (prevSuccess). With no adopter wired nothing is admitted and req
// is returned as it is (Phase 1 observes and adopts nothing).
//
// AIDEV-NOTE: a queued result is what the scheduler parks a suite on
// (isQueuedResult). A client check's queued result leaves the change
// pending (change seen, no record), which makes the suite due at the next
// tick (Checker.pendingChange); that tick's check bypasses the cooldown
// and parks it.
func (c *Checker) admit(out *checkOutcome, cur *cache.SuiteFreshness, req *adoptionRequest, signedCurrent bool,
	prevSuccess *int64, result, queued string) (*adoptionRequest, string) {
	if c.adopter == nil {
		return req, result
	}
	l, ok := c.admission.tryAcquire(req.suite.CanonicalHost, signedCurrent)
	if !ok {
		cur.LastSuccessAt = prevSuccess
		return nil, queued
	}
	out.lease = l
	return req, result
}

// bodyHash is the sha256 hex of a 200 response's body ("" for a 304).
func bodyHash(res *fetch.ConditionalResult) string {
	if res.Status != 200 {
		return ""
	}
	sum := sha256.Sum256(res.Body)
	return hex.EncodeToString(sum[:])
}

// checkInline runs the conditional GET against InRelease and
// processes the result. Returns:
//   - (req, false) on success (req may be nil for unchanged/304).
//   - (nil, true) when the inline path got 404 AND the suite has no
//     current snapshot AND a Release url_path row exists — the
//     dispatcher then retries via checkDetached. We return
//     without persisting in the fallback case so the detached
//     attempt's persist is the only one that runs.
//
// The body of this function is the SPEC §7.2 inline algorithm; it has
// been factored out of checkLocked to give detached mode (SPEC2 §7.6.3)
// a parallel implementation. Mutates *cur in the non-fallback paths.
func (c *Checker) checkInline(ctx, fetchCtx context.Context, out *checkOutcome, cur *cache.SuiteFreshness, suite SuiteRef, now time.Time, nowUnix int64) (*adoptionRequest, *repairRequest, bool) {
	scheme, host, suitePath := suite.CanonicalScheme, suite.CanonicalHost, suite.SuitePath

	// Locate the InRelease url_path row to get the upstream URL plus
	// (Phase 1 fallback) the cached blob hash for byte-equality
	// comparison on 200.
	inReleasePath := suitePath + inReleaseFilename
	urlRow, err := c.cache.LookupURL(ctx, scheme, host, inReleasePath)
	switch {
	case errors.Is(err, cache.ErrNotFound):
		// Suite has freshness state but no cached InRelease url_path row.
		if cur.CurrentSnapshotID == nil {
			// First-ever / never-adopted suite. The detached fallback
			// path triggers on 404 from the upstream's /InRelease, which
			// requires us to actually issue the GET. If we don't even
			// have a url_path row to attempt the GET, try the symmetric
			// detached lookup directly.
			if c.hasReleaseURLRow(ctx, scheme, host, suitePath) {
				return nil, nil, true
			}
			c.logger.Debug("freshness: no cached InRelease url_path, skipping",
				"canonical_host", host,
				"suite_path", suitePath,
			)
			return nil, nil, false
		}
		// SPEC2 §7.4 freshness recovery. The suite HAS a current
		// snapshot but its InRelease anchor row was reaped (SPEC4 §5 GC
		// TTL during a low-traffic request lull). Without this branch
		// the check dead-ends here and the suite freezes forever: it
		// keeps serving the stale snapshot from snapshot_member and
		// never observes upstream rolling forward — the production
		// freeze trap. Reconstruct the upstream URL from the suite
		// identity, re-seed the anchor (pinned to the snapshot's adopted
		// hash so the SPEC4 §5 guards re-protect it), and fall through
		// to the conditional GET with the synthesized row.
		anchorHash := c.snapshotAnchorHash(ctx, *cur.CurrentSnapshotID, inReleaseFilename)
		urlRow = c.reconstructAnchor(ctx, scheme, host, suitePath, inReleasePath, *cur.CurrentSnapshotID, anchorHash, nowUnix)
	case err != nil:
		c.logger.Warn("freshness: lookup InRelease url_path failed",
			"err", err,
			"canonical_host", host,
			"suite_path", suitePath,
		)
		return nil, nil, false
	}

	cachedHash, signedCurrent := c.baseline(ctx, cur, suite, urlRow, "InRelease")
	if cachedHash == "" {
		return nil, nil, false
	}

	target := &fetch.Target{
		CanonicalHost: host,
		URL:           urlRow.UpstreamURL,
	}
	gate := c.retryGateFor(cur, now)
	c.applyClamp(ctx, cur, gate, now)
	etag, lastmod := gate.validators(cur)

	// SPEC §9.3: bound concurrent upstream calls to host. Without
	// this, distinct suites on the same host all run Conditional in
	// parallel, each capable of pulling MaxInReleaseBytes into
	// memory — a resource-exhaustion path under a refresh storm or
	// an adversarial allowlisted upstream. Sharing the limiter with
	// the handler's miss path means cache-miss fetches and
	// freshness checks contend for the same per-host budget.
	release, err := c.hostSem.Acquire(fetchCtx, host)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			c.logger.Debug("freshness: host limiter acquire aborted",
				"err", err,
				"canonical_host", host,
				"suite_path", suitePath,
			)
			return nil, nil, false
		}
		// Spec C4: the check deadline hit while waiting for the slot.
		c.failCheck(ctx, out, cur, suite, nowUnix, "inline", err)
		return nil, nil, false
	}
	defer release()

	res, err := c.fetcher.Conditional(fetchCtx, target, etag, lastmod, c.maxBody)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// A cancelled caller (shutdown) is not evidence about upstream;
			// the check deadline (DeadlineExceeded) is, and counts below.
			c.logger.Debug("freshness: check cancelled", "canonical_host", host, "suite_path", suitePath)
			return nil, nil, false
		}
		// 404 with no current snapshot AND a Release url_path row →
		// fall back to detached without persisting. The detached
		// attempt becomes the authoritative result for this Check.
		if cur.CurrentSnapshotID == nil && isStatusNotFound(err) &&
			c.hasReleaseURLRow(ctx, scheme, host, suitePath) {
			c.logger.Debug("freshness: InRelease 404 on first-ever check, falling back to detached",
				"canonical_host", host,
				"suite_path", suitePath,
			)
			return nil, nil, true
		}
		c.failCheck(ctx, out, cur, suite, nowUnix, "inline", err)
		return nil, nil, false
	}
	if res.Status != 200 && res.Status != 304 {
		// fetch.Conditional answers only 200 or 304 on success: anything
		// else is a contract violation, a failed check (ruling R-E3).
		c.failCheck(ctx, out, cur, suite, nowUnix, "inline", fmt.Errorf("%w %d", errUnexpectedStatus, res.Status))
		return nil, nil, false
	}

	prevSuccess := cur.LastSuccessAt
	cur.LastCheckAt = &nowUnix
	cur.LastSuccessAt = &nowUnix
	cur.CheckFailCount = 0

	// SPEC §10: every freshness attempt emits a structured log line. The
	// `result` enum is what operators pivot on — success-with-no-change
	// (the steady state) vs. success-with-change (the interesting one).
	newHash := bodyHash(res)
	e := checkEnd{form: "inline", status: res.Status, signedCurrent: signedCurrent}
	var req *adoptionRequest
	if result, clear, ok := steadyResult(cur, res, newHash, cachedHash, gate, nowUnix); ok {
		e.result, e.clearRecord = result, clear
	} else if gate.active && !gate.due && newHash == gate.hash {
		// An upstream that ignores validators re-served the failing
		// bytes: deferred exactly like the 304 above.
		setChangeSeen(cur, nowUnix)
		e.result = "changed_deferred"
	} else {
		// Upstream has a new InRelease. Record the diagnostic
		// regardless of whether an Adopter is wired — operators
		// monitoring a Phase 1 deployment still see the
		// divergence.
		setChangeSeen(cur, nowUnix)
		c.logger.Info("InRelease changed at upstream",
			"canonical_host", host,
			"suite_path", suitePath,
			"cached_hash", cachedHash,
			"upstream_hash", newHash,
		)
		e.result = "changed"
		// The body is bounded by maxBody (§7.2 cap, default 4 MiB),
		// so retaining it through the goroutine handoff is acceptable.
		req = &adoptionRequest{
			suite:   suite,
			form:    adoptionFormInline,
			key:     newHash,
			bytes:   res.Body,
			etag:    res.ETag,
			lastmod: res.LastModified,
		}
		req, e.result = c.admit(out, cur, req, signedCurrent, prevSuccess, "changed", "changed_queued")
	}
	if e.result == "changed_deferred" {
		e.reason = gate.reason
	}
	return req, c.finishCheck(ctx, out, cur, suite, e), false
}

// checkDetached runs the SPEC2 §7.6.3 detached-form freshness
// check: a conditional GET on Release for change detection, plus a
// fresh GET on Release.gpg when Release has changed. Mutates *cur.
//
// Logically symmetric to checkInline. The differences:
//   - Two url_path rows are required (Release AND Release.gpg).
//     Either missing → skip.
//   - On observed change, fetch Release.gpg in a second call so the
//     Adopter can verify the detached signature.
//   - The adoptionRequest carries form=detached + both bodies.
//
// AIDEV-NOTE: a Release that changed costs one Release.gpg GET even when
// the result is then deferred (its content key covers the signature), and
// an upstream that ignores validators pays it on every deferred check
// (deferred minor B-25: an accepted cost). The Release.gpg GET runs under
// fetchCtx, the check deadline (TestCheck_DetachedSignatureStallEndsAtTheCheckDeadline).
func (c *Checker) checkDetached(ctx, fetchCtx context.Context, out *checkOutcome, cur *cache.SuiteFreshness, suite SuiteRef, now time.Time, nowUnix int64) (*adoptionRequest, *repairRequest) {
	scheme, host, suitePath := suite.CanonicalScheme, suite.CanonicalHost, suite.SuitePath

	releasePath := suitePath + releaseFilename
	releaseGPGPath := suitePath + releaseGPGFilename

	releaseURL, err := c.cache.LookupURL(ctx, scheme, host, releasePath)
	switch {
	case errors.Is(err, cache.ErrNotFound):
		// SPEC2 §7.4 freshness recovery (detached): an adopted suite
		// whose Release anchor was reaped must recover, not freeze —
		// symmetric to the inline path. First-ever suites (no snapshot)
		// still skip.
		if cur.CurrentSnapshotID == nil {
			c.logger.Debug("freshness: no cached Release url_path, skipping detached",
				"canonical_host", host,
				"suite_path", suitePath,
			)
			return nil, nil
		}
		relHash := c.snapshotAnchorHash(ctx, *cur.CurrentSnapshotID, releaseFilename)
		releaseURL = c.reconstructAnchor(ctx, scheme, host, suitePath, releasePath, *cur.CurrentSnapshotID, relHash, nowUnix)
	case err != nil:
		c.logger.Warn("freshness: lookup Release url_path failed",
			"err", err,
			"canonical_host", host,
			"suite_path", suitePath,
		)
		return nil, nil
	}
	releaseGPGURL, err := c.cache.LookupURL(ctx, scheme, host, releaseGPGPath)
	switch {
	case errors.Is(err, cache.ErrNotFound):
		if cur.CurrentSnapshotID == nil {
			c.logger.Debug("freshness: no cached Release.gpg url_path, skipping detached",
				"canonical_host", host,
				"suite_path", suitePath,
			)
			return nil, nil
		}
		gpgHash := c.snapshotAnchorHash(ctx, *cur.CurrentSnapshotID, releaseGPGFilename)
		releaseGPGURL = c.reconstructAnchor(ctx, scheme, host, suitePath, releaseGPGPath, *cur.CurrentSnapshotID, gpgHash, nowUnix)
	case err != nil:
		c.logger.Warn("freshness: lookup Release.gpg url_path failed",
			"err", err,
			"canonical_host", host,
			"suite_path", suitePath,
		)
		return nil, nil
	}

	cachedHash, signedCurrent := c.baseline(ctx, cur, suite, releaseURL, "Release")
	if cachedHash == "" {
		return nil, nil
	}

	target := &fetch.Target{
		CanonicalHost: host,
		URL:           releaseURL.UpstreamURL,
	}
	gate := c.retryGateFor(cur, now)
	c.applyClamp(ctx, cur, gate, now)
	etag, lastmod := gate.validators(cur)

	releaseSlot, err := c.hostSem.Acquire(fetchCtx, host)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			c.logger.Debug("freshness: host limiter acquire aborted",
				"err", err,
				"canonical_host", host,
				"suite_path", suitePath,
			)
			return nil, nil
		}
		// Spec C4: the check deadline hit while waiting for the slot.
		c.failCheck(ctx, out, cur, suite, nowUnix, "detached", err)
		return nil, nil
	}
	defer releaseSlot()

	res, err := c.fetcher.Conditional(fetchCtx, target, etag, lastmod, c.maxBody)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// A cancelled caller (shutdown) is not evidence about upstream;
			// the check deadline (DeadlineExceeded) is, and counts below.
			c.logger.Debug("freshness: check cancelled", "canonical_host", host, "suite_path", suitePath)
			return nil, nil
		}
		c.failCheck(ctx, out, cur, suite, nowUnix, "detached", err)
		return nil, nil
	}
	if res.Status != 200 && res.Status != 304 {
		c.failCheck(ctx, out, cur, suite, nowUnix, "detached", fmt.Errorf("%w %d", errUnexpectedStatus, res.Status))
		return nil, nil
	}

	prevSuccess := cur.LastSuccessAt
	cur.LastCheckAt = &nowUnix
	cur.LastSuccessAt = &nowUnix
	cur.CheckFailCount = 0

	newHash := bodyHash(res)
	e := checkEnd{form: "detached", status: res.Status, signedCurrent: signedCurrent}
	var req *adoptionRequest
	if result, clear, ok := steadyResult(cur, res, newHash, cachedHash, gate, nowUnix); ok {
		// A 304 while a failure record exists: Release still matches the
		// failing content's validators, and a re-signature behind it waits
		// for the retry (spec C7).
		e.result, e.clearRecord = result, clear
	} else {
		setChangeSeen(cur, nowUnix)
		// The content key covers the signature, so Release.gpg is fetched
		// before the gate can compare (no validators: the signature is
		// bound to this Release).
		gpgTarget := &fetch.Target{
			CanonicalHost: host,
			URL:           releaseGPGURL.UpstreamURL,
		}
		gpgRes, gerr := c.fetcher.Conditional(fetchCtx, gpgTarget, "", "", defaultMaxReleaseGPGBytes)
		switch {
		case gerr != nil || gpgRes.Status != 200:
			// As before: record the change and retry on a later check;
			// last_success_at stays bumped because Release answered.
			if gerr != nil {
				c.logger.Warn("freshness: Release.gpg fetch failed; deferring adoption",
					"err", gerr, "canonical_host", host, "suite_path", suitePath)
			} else {
				c.logger.Warn("freshness: Release.gpg fetch returned non-200; deferring adoption",
					"status", gpgRes.Status, "canonical_host", host, "suite_path", suitePath)
			}
			e.result = "changed"
		default:
			key := contentKey(adoptionFormDetached, nil, res.Body, gpgRes.Body)
			if gate.active && !gate.due && key == gate.hash {
				e.result = "changed_deferred"
				break
			}
			c.logger.Info("Release changed at upstream",
				"canonical_host", host,
				"suite_path", suitePath,
				"cached_hash", cachedHash,
				"upstream_hash", newHash,
			)
			req = &adoptionRequest{
				suite:        suite,
				form:         adoptionFormDetached,
				key:          key,
				releaseBytes: res.Body,
				sigBytes:     gpgRes.Body,
				etag:         res.ETag,
				lastmod:      res.LastModified,
			}
			req, e.result = c.admit(out, cur, req, signedCurrent, prevSuccess, "changed", "changed_queued")
		}
	}
	if e.result == "changed_deferred" {
		e.reason = gate.reason
	}
	return req, c.finishCheck(ctx, out, cur, suite, e)
}
```

In `internal/freshness/freshness.go`: delete `checkLocked` (with its doc comment), `detectForm`, `checkLockedInline`, `checkLockedDetached` and `recordUnknownEnd`; drop the now unused imports `crypto/sha256` and `encoding/hex` and add `strings`; replace `applyClamp` with:

```go
// applyClamp persists a clamped next_retry_at (backward clock step) and
// keeps cur in step with it, so a later gate read in the same check (the
// inline check's detached fallback) does not clamp — and count — it again
// (deferred minor B-23).
func (c *Checker) applyClamp(ctx context.Context, cur *cache.SuiteFreshness, g retryGate, now time.Time) {
	if g.clampTo == 0 {
		return
	}
	c.clockStep("adoption_next_retry_at", cur.CanonicalScheme, cur.CanonicalHost, cur.SuitePath, *cur.AdoptionNextRetryAt, now)
	if err := c.cache.SetAdoptionNextRetry(ctx, cur.CanonicalScheme, cur.CanonicalHost, cur.SuitePath, g.clampTo); err != nil {
		c.logger.Warn("freshness: clamp adoption retry failed",
			"err", err, "canonical_host", cur.CanonicalHost, "suite_path", cur.SuitePath)
	}
	clamped := g.clampTo
	cur.AdoptionNextRetryAt = &clamped
}
```

and `snapshotAnchorHash` with:

```go
// snapshotAnchorHash returns the adopted hash for the given anchor
// filename ("/InRelease", "/Release", "/Release.gpg") from the suite's
// current snapshot, through its form (cache.SuiteSnapshot.AnchorHash: an
// unsigned snapshot keeps its Release in the single-anchor slot), or nil
// if unavailable. Used by the recovery paths to pin a reconstructed
// anchor's blob_hash to the snapshot so the SPEC4 §5 hash guards
// re-protect the re-seeded row.
func (c *Checker) snapshotAnchorHash(ctx context.Context, snapshotID int64, filename string) *string {
	snap, err := c.cache.GetSuiteSnapshot(ctx, snapshotID)
	if err != nil || snap == nil {
		return nil
	}
	h := snap.AnchorHash(strings.TrimPrefix(filename, "/"))
	if h == nil {
		return nil
	}
	v := *h
	return &v
}
```

Rename the old names in comments:

```bash
sed -i 's/checkLockedInline/checkInline/g; s/checkLockedDetached/checkDetached/g' \
  internal/freshness/freshness.go internal/freshness/check_streak_test.go internal/freshness/freshness_test.go \
  internal/handler/phase2_detached_gpg_forgery_test.go
grep -rn 'checkLocked[ID]\|recordUnknownEnd' internal/   # nothing
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/freshness/ ./internal/handler/`
Expected: PASS — the four new tests and every existing one, unchanged.

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| `checkDetached`: the `Release.gpg` GET under `ctx` instead of `fetchCtx` | `TestCheck_DetachedSignatureStallEndsAtTheCheckDeadline` (the bubble deadlocks) |
| `failCheck` without `freshnessCheckTotal.Inc("failed", host)` | `TestCheck_UnexpectedStatusIsAFailedCheck/{inline,detached}`, `TestCheck_TransportFailureCountsAsFailed` |
| the status guard `res.Status != 200 && res.Status != 304` → `false && …` (both paths) | `TestCheck_UnexpectedStatusIsAFailedCheck/{inline,detached}` |
| `applyClamp` no longer updates `cur.AdoptionNextRetryAt` | `TestCheck_FutureRetryIsClampedOncePerCheck` |

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/freshness/check.go internal/freshness/check_tail_test.go internal/freshness/freshness.go \
  internal/freshness/check_streak_test.go internal/freshness/freshness_test.go internal/handler/phase2_detached_gpg_forgery_test.go
git add internal/freshness/check.go internal/freshness/check_tail_test.go internal/freshness/freshness.go \
  internal/freshness/check_streak_test.go internal/freshness/freshness_test.go internal/handler/phase2_detached_gpg_forgery_test.go
git commit -m "Fold the inline and detached check paths into one tail; an unexpected status is a counted failed check"
```

### Task 5: Fixture-only commit — the existing check tests' never-adopted suites become ones phase E adopts

Tasks 6 and 7 change what a check of a suite with no current snapshot does (spec C4 Dispatch 3: nothing without client interest in the last 7 days and a client-created anchor row; otherwise the probe tree, which classifies each anchor by its sniff). Most existing check tests build such suites with a plain-text "InRelease", a reconstructed-style anchor row (`request_count` 0), no interest stamp and plain-text `Release.gpg` bodies; under phase E they would be dormant, or their anchors absent. This task changes **only fixtures**, so that every existing test keeps proving what it proves today, and it stays green on the Task 4 code — which ignores interest, `request_count` and the bodies' shape. Tasks 6 and 7 then change only expectations, each listed with its reason.

The fixture rules (one per helper, so the tests themselves barely change):

| Fixture | Change | Why it is neutral on the Task 4 code |
|---|---|---|
| `cache.SeedSuiteForTest` | also writes `s.LastRequestedAt` when set | no check reads the suite's interest before Task 6 |
| `seedStaleAnchor`, `seedStaleAnchorAt`, `seedSimSuite`, `seedFakeSuite`, `seedFakeAnchor`, `seedDetachedAnchors` | anchor rows get `RequestCount: 1` (a client created them) | `request_count` is read by url_path GC only |
| `newRetryRig` | the suite's interest stamp is the rig clock's now (1 800 000 000), not the real clock | as above |
| `seedFakeSuite` | interest at the suite's last success, else now | as above |
| `retryRig.release`, `barrierRig.release`, `retrySim.release` and every body a test serves as `InRelease` (`signedRelease`) | a clearsign envelope (`simtest.ClearsignedFixture`) around the same Release text | the pass-through and rig verifiers hand the envelope to `ParseRelease`, which skips the armor lines; content keys are hashes of whatever is served |
| bodies served as `Release` | `retryRig.releaseText` (the plain text) | unchanged bytes |
| bodies served as `Release.gpg` | `armoredSig(label)` (an armor header, so spec C3's sniff passes) | the verifiers never parse signatures |
| `fakeCache` | a new `interest(scheme, host, suitePath, at)` helper; the fake-cache tests whose checker has a fixed `now` stamp interest at it | — |
| three gate tests (`TestGate_DeferredSendsFailingValidators`, `TestGate_T1StormAfterFailureCostsNothing`, `TestGate_DetachedResignature`) | start from an adopted suite (`retryRig.adopt`, `adoptDetached`) | they prove the spec C7 validators of the current form's conditional GET, which only an adopted suite has after phase E; the old code applies the gate the same way to an adopted suite |
| three more gate tests (`TestGate_SameContent200IsDeferred`, `TestGate_NewContentIsAttemptedAtOnce`, `TestGate_DetachedSameContent200IsDeferred`) | start from an adopted suite (`retryRig.adopt`, `adoptDetached`) | their second check comes a minute after a failed attempt: after phase E a never-adopted suite inside its record's window is not probed at all (ruling PE-3, Task 6), while an adopted suite's conditional GET keeps the gate they prove; the old code applies the gate the same way to an adopted suite |
| `TestSim30Days_RetryDateRulesAndClockSteps` | a client touches the suite when upstream publishes v2 (`retrySim.touch`) | a never-adopted suite stays checked only while clients ask for it (10 days in, the seed's interest has lapsed) |
| `TestRunLegacyTrustAudit_QuarantinedR1SuiteReadoptsOnFirstCheck` | a client fetches the quarantined suite's anchor (`TouchURLPath`) | the anchor row was created by the adoption's seed, not by a client |
| `TestCheck_AdopterInvokedOnChange`, `TestCheck_DetachedFallback_OnInRelease404`, `TestCheck_DetachedFallback_OnMissingInReleaseURLRow` | clearsigned `InRelease`, armored `Release.gpg`, client-created rows, interest at the checker's fixed `now` | as above |
| `TestCheck_FutureRetryIsClampedOncePerCheck` (Task 4) | interest at the checker's `now` | as above |
| `TestCheck_DetachedKnownSuite_AdoptsOnReleaseChange` | its `Release.gpg` body is armored (`armoredSig`) | after phase E an adopted detached suite classifies the signature of a changed `Release` through spec C3's sniff, and one that fails it is absent (Task 7, design review M-14); the Task 4 code never reads the body |
| `makeChaos2DetachedSnapshot` (`internal/handler`; `TestPhase2DetachedChaos_AdoptionFlipUnderConcurrency`, `TestPhase2DetachedGPGForgery_RejectionLeavesSnapshotIntact`) | the label-distinguishing `Release.gpg` placeholder gets an armor header | as above: the detached adoption these tests drive needs a signature that passes the sniff after phase E; the verifiers ignore the signature bytes |
| `TestCheck_RepairPanicIsRecovered`, `TestChecker_FreshTickRepairsSkippedMembers` | the adopted and served `InRelease` is clearsigned (`asInRelease`) | an adopted suite whose upstream serves the same bytes stays "unchanged"; phase E reads a plain-text `InRelease` as absent |
| `TestCheck_HealthySuiteAsksForNoRepairSlot` | serves `r.release` itself (it is already clearsigned) instead of wrapping it twice | same bytes shape as before the rig change |

**Files:**
- Modify: `internal/cache/testing_helpers.go` (`SeedSuiteForTest`); tests in `internal/freshness/`: `adoption_date_test.go` (`signedRelease`, `asInRelease`), `capacity_test.go`, `check_tail_test.go`, `clockstep_test.go`, `flip_barrier_test.go`, `freshness_test.go` (`fakeCache.interest`), `legacy_audit_test.go`, `panic_recovery_test.go`, `phase6_7_skip_repair_test.go`, `reoffer_test.go`, `retry_gate_test.go`, `retry_rig_test.go` (`releaseText`, `armoredSig`, `adopt`, `adoptDetached`), `scheduler_test.go`, `sim_retry_test.go` (`touch`), `sim_sched_test.go`; in `internal/handler/`: `phase2_detached_chaos_test.go` (`makeChaos2DetachedSnapshot`).

**Interfaces:**
- Consumes: `simtest.ClearsignedFixture`, `cache.(*Cache).TouchSuite`, `cache.(*Cache).TouchURLPath`.
- Produces (test helpers, package freshness unless noted): `signedRelease(label string, date int64, members map[string][]byte) []byte`; `asInRelease(text []byte) []byte`; `(*retryRig).releaseText(label string, date int64) []byte` (`release` now returns its clearsigned form); `armoredSig(label string) []byte`; `(*retryRig).adopt()`, `(*retryRig).adoptDetached()` (adopt a base snapshot, then reset the verifier's call count); `(*fakeCache).interest(scheme, host, suitePath string, at int64)`; `(*retrySim).touch(t)`; `cache.(*Cache).SeedSuiteForTest` honours `LastRequestedAt`.

- [ ] **Step 1: Apply the fixture changes**

Apply this patch (`git apply`), which is the whole task:

```diff
diff --git a/internal/cache/testing_helpers.go b/internal/cache/testing_helpers.go
index 7df07bb..44305bd 100644
--- a/internal/cache/testing_helpers.go
+++ b/internal/cache/testing_helpers.go
@@ -78,6 +78,7 @@ func (c *Cache) ClearSignedSeenForTest(ctx context.Context) error {
 // spec C3 made every writer but CreateSuite UPDATE-only: it creates the
 // row if absent (createSuiteTx, uncapped, interest stamp now) and writes
-// s's six check columns with UpdateSuiteFreshness's statement, in one
-// writer transaction. Fixtures use it to build a tracked suite in a given
+// s's six check columns with UpdateSuiteFreshness's statement, and its
+// LastRequestedAt (the client-interest stamp) when set, in one writer
+// transaction. Fixtures use it to build a tracked suite in a given
 // state. This file is not a _test.go file, so the helper is compiled into
 // the production binary; never call it from non-test code
@@ -97,4 +98,11 @@ func (c *Cache) SeedSuiteForTest(ctx context.Context, s SuiteFreshness) error {
 			return fmt.Errorf("SeedSuiteForTest: update: %w", err)
 		}
+		if s.LastRequestedAt != nil {
+			if _, err := tx.ExecContext(ctx, `UPDATE suite_freshness SET last_requested_at = ?
+ WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`,
+				*s.LastRequestedAt, s.CanonicalScheme, s.CanonicalHost, s.SuitePath); err != nil {
+				return fmt.Errorf("SeedSuiteForTest: interest: %w", err)
+			}
+		}
 		return tx.Commit()
 	})
diff --git a/internal/freshness/adoption_date_test.go b/internal/freshness/adoption_date_test.go
index 1a26849..e5e5569 100644
--- a/internal/freshness/adoption_date_test.go
+++ b/internal/freshness/adoption_date_test.go
@@ -13,4 +13,5 @@ import (
 	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
 	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
+	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
 )
 
@@ -26,4 +27,15 @@ func datedRelease(label string, date int64, members map[string][]byte) []byte {
 }
 
+// signedRelease is datedRelease in a clearsign envelope: an InRelease body
+// that spec C3's sniff passes. A pass-through verifier hands the envelope
+// to ParseRelease, which skips the armor lines.
+func signedRelease(label string, date int64, members map[string][]byte) []byte {
+	return simtest.ClearsignedFixture(string(datedRelease(label, date, members)))
+}
+
+// asInRelease wraps Release text in a clearsign envelope, as signedRelease
+// does.
+func asInRelease(text []byte) []byte { return simtest.ClearsignedFixture(string(text)) }
+
 // adoptDated runs one adoption of env's suite whose Release is labelled
 // label and dated date (0 = no Date field).
diff --git a/internal/freshness/capacity_test.go b/internal/freshness/capacity_test.go
index 402bf73..aabb9a5 100644
--- a/internal/freshness/capacity_test.go
+++ b/internal/freshness/capacity_test.go
@@ -17,6 +17,7 @@ import (
 )
 
-// seedStaleAnchorAt caches a stale InRelease anchor (testHex64) and a
-// suite row for suite on host, so any body upstream serves is "changed".
+// seedStaleAnchorAt caches a stale client-created InRelease anchor
+// (testHex64, request_count 1) and a suite row for suite on host, so any
+// body upstream serves is "changed".
 func seedStaleAnchorAt(t *testing.T, c *cache.Cache, host, suite string) {
 	t.Helper()
@@ -27,5 +28,5 @@ func seedStaleAnchorAt(t *testing.T, c *cache.Cache, host, suite string) {
 	}
 	if err := c.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: host, Path: suite + "/InRelease",
-		BlobHash: &stale, UpstreamURL: "http://" + host + suite + "/InRelease", IsMetadata: true}); err != nil {
+		BlobHash: &stale, UpstreamURL: "http://" + host + suite + "/InRelease", IsMetadata: true, RequestCount: 1}); err != nil {
 		t.Fatal(err)
 	}
@@ -65,6 +66,6 @@ func TestCheck_NoSlotQueuesDetachedToo(t *testing.T) {
 	r := newRetryRig(t, 0)
 	seedDetachedAnchors(t, r.cache)
-	r.up.setAt(rigReleaseURL, r.release("det1", 0))
-	r.up.setAt(rigReleaseGPGURL, []byte("sig one"))
+	r.up.setAt(rigReleaseURL, r.releaseText("det1", 0))
+	r.up.setAt(rigReleaseGPGURL, armoredSig("sig one"))
 	r.check()
 	adopted := r.row()
@@ -74,6 +75,6 @@ func TestCheck_NoSlotQueuesDetachedToo(t *testing.T) {
 	r.clock.advance(time.Hour)
 	held := mustAcquire(t, r.checker.admission, rigHost, true)
-	r.up.setAt(rigReleaseURL, r.release("det2", 0))
-	r.up.setAt(rigReleaseGPGURL, []byte("sig two"))
+	r.up.setAt(rigReleaseURL, r.releaseText("det2", 0))
+	r.up.setAt(rigReleaseGPGURL, armoredSig("sig two"))
 	r.check()
 	if r.ver.calls() != 1 || lastResult(r) != "changed_queued" {
@@ -111,5 +112,5 @@ func TestCheck_GlobalSlotCapsConcurrentAdoptions(t *testing.T) {
 		seedStaleAnchorAt(t, c, host, rigSuite)
 		bf.put("http://"+host+rigSuite+"/main/binary-amd64/Packages", pkgs)
-		up.setAt("http://"+host+rigSuite+"/InRelease", datedRelease("cap", 0, map[string][]byte{"main/binary-amd64/Packages": pkgs}))
+		up.setAt("http://"+host+rigSuite+"/InRelease", signedRelease("cap", 0, map[string][]byte{"main/binary-amd64/Packages": pkgs}))
 	}
 	ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: bf, Verifier: passThroughVerifier{},
@@ -325,5 +326,5 @@ func TestCheck_HealthySuiteAsksForNoRepairSlot(t *testing.T) {
 	// Clearsigned: the unchanged check reconciles, which parses only
 	// clearsigned InRelease (phase D).
-	r.up.set(simtest.ClearsignedFixture(string(r.release("v1", 0))))
+	r.up.set(r.release("v1", 0))
 	r.check()
 	held := mustAcquire(t, r.checker.admission, rigHost, true)
@@ -387,5 +388,5 @@ func TestAdoption_MaxDurationBoundsAStalledAdoption(t *testing.T) {
 		seedStaleAnchorAt(t, c, rigHost, rigSuite)
 		pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": testHex64})
-		rel := datedRelease("slow", 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})
+		rel := signedRelease("slow", 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})
 		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: stallFetcher{}, Verifier: passThroughVerifier{},
 			HostLimiter: hostsem.New(8), MaxConcurrent: 2, MaxDuration: 20 * time.Minute})
diff --git a/internal/freshness/check_tail_test.go b/internal/freshness/check_tail_test.go
index f46f838..c5ca917 100644
--- a/internal/freshness/check_tail_test.go
+++ b/internal/freshness/check_tail_test.go
@@ -180,4 +180,5 @@ func TestCheck_FutureRetryIsClampedOncePerCheck(t *testing.T) {
 	fc.track("http", "h", "/r/dists/x")
 	now := time.Unix(1_800_000_000, 0)
+	fc.interest("http", "h", "/r/dists/x", now.Unix())
 	if err := fc.RecordAdoptionFailure(context.Background(), "http", "h", "/r/dists/x", cache.AdoptionFailure{
 		Hash: testHex64, Reason: "gpg_failed", Count: 1, FirstAt: now.Unix(), LastAt: now.Unix(),
diff --git a/internal/freshness/clockstep_test.go b/internal/freshness/clockstep_test.go
index d98a52d..d6bbe5c 100644
--- a/internal/freshness/clockstep_test.go
+++ b/internal/freshness/clockstep_test.go
@@ -15,5 +15,5 @@ func seedFakeAnchor(fc *fakeCache) {
 	stale := testHex64
 	fc.putURL(cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: "/r/dists/x/InRelease",
-		BlobHash: &stale, UpstreamURL: "http://h/r/dists/x/InRelease", IsMetadata: true})
+		BlobHash: &stale, UpstreamURL: "http://h/r/dists/x/InRelease", IsMetadata: true, RequestCount: 1})
 }
 
diff --git a/internal/freshness/flip_barrier_test.go b/internal/freshness/flip_barrier_test.go
index f93bf9e..8120675 100644
--- a/internal/freshness/flip_barrier_test.go
+++ b/internal/freshness/flip_barrier_test.go
@@ -62,5 +62,5 @@ func newBarrierRig(t *testing.T, quiet, heartbeat time.Duration) *barrierRig {
 
 func (r *barrierRig) release(label string) []byte {
-	return datedRelease(label, 0, map[string][]byte{"main/binary-amd64/Packages": r.pkgs})
+	return signedRelease(label, 0, map[string][]byte{"main/binary-amd64/Packages": r.pkgs})
 }
 
@@ -722,5 +722,5 @@ func TestSimCapacity_TheBarrierHoldsTheGlobalSlotWithinItsBound(t *testing.T) {
 		}
 		rel := func(label string, pkgs []byte) []byte {
-			return datedRelease(label, 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})
+			return signedRelease(label, 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})
 		}
 		pkgsURL := func(host string) string { return "http://" + host + rigSuite + "/main/binary-amd64/Packages" }
diff --git a/internal/freshness/freshness_test.go b/internal/freshness/freshness_test.go
index 5cbd4ad..cddc89f 100644
--- a/internal/freshness/freshness_test.go
+++ b/internal/freshness/freshness_test.go
@@ -119,4 +119,15 @@ func (f *fakeCache) track(scheme, host, suitePath string) {
 }
 
+// interest stamps a tracked suite's client interest (last_requested_at,
+// spec C4) at unix time at.
+func (f *fakeCache) interest(scheme, host, suitePath string, at int64) {
+	f.mu.Lock()
+	defer f.mu.Unlock()
+	k := keyOf(scheme, host, suitePath)
+	s := f.suites[k]
+	s.LastRequestedAt = &at
+	f.suites[k] = s
+}
+
 // UpdateSuiteFreshness mirrors the production writer: UPDATE-only (an
 // unknown suite is left alone) and only the check columns.
@@ -1288,4 +1299,5 @@ func TestCheck_AdopterInvokedOnChange(t *testing.T) {
 	fc := newFakeCache()
 	fc.track("http", "127.0.0.1", "/dists/noble")
+	fc.interest("http", "127.0.0.1", "/dists/noble", 11000)
 	memberBody := []byte("member content")
 	memberHash := hashOf(memberBody)
@@ -1293,5 +1305,5 @@ func TestCheck_AdopterInvokedOnChange(t *testing.T) {
 		"Origin: Test\nSuite: noble\nSHA256:\n %s %d main/Sources\n",
 		memberHash, len(memberBody))
-	body := []byte(releaseText)
+	body := simtest.ClearsignedFixture(releaseText)
 
 	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
@@ -1305,5 +1317,5 @@ func TestCheck_AdopterInvokedOnChange(t *testing.T) {
 		CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
 		Path: "/dists/noble/InRelease", BlobHash: &stale,
-		UpstreamURL: srv.URL + "/dists/noble/InRelease", IsMetadata: true,
+		UpstreamURL: srv.URL + "/dists/noble/InRelease", IsMetadata: true, RequestCount: 1,
 	})
 
@@ -1383,5 +1395,5 @@ func TestCheck_DetachedKnownSuite_AdoptsOnReleaseChange(t *testing.T) {
 		memberHash, len(memberBody),
 	))
-	sigBody := []byte("placeholder Release.gpg bytes")
+	sigBody := armoredSig("placeholder Release.gpg bytes")
 
 	staleHash := strings.Repeat("0", 64)
@@ -1520,4 +1532,5 @@ func TestCheck_DetachedFallback_OnInRelease404(t *testing.T) {
 	fc := newFakeCache()
 	fc.track("http", "127.0.0.1", "/dists/noble")
+	fc.interest("http", "127.0.0.1", "/dists/noble", 12000)
 
 	memberBody := []byte("fallback member content")
@@ -1527,5 +1540,5 @@ func TestCheck_DetachedFallback_OnInRelease404(t *testing.T) {
 		memberHash, len(memberBody),
 	))
-	sigBody := []byte("placeholder Release.gpg bytes for fallback")
+	sigBody := armoredSig("placeholder Release.gpg bytes for fallback")
 
 	var inReleaseHits, releaseHits, gpgHits atomic.Int32
@@ -1566,5 +1579,5 @@ func TestCheck_DetachedFallback_OnInRelease404(t *testing.T) {
 		CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
 		Path: "/dists/noble/Release", BlobHash: &staleRH,
-		UpstreamURL: srv.URL + "/dists/noble/Release", IsMetadata: true,
+		UpstreamURL: srv.URL + "/dists/noble/Release", IsMetadata: true, RequestCount: 1,
 	})
 	fc.putURL(cache.URLPath{
@@ -1643,4 +1656,5 @@ func TestCheck_DetachedFallback_OnMissingInReleaseURLRow(t *testing.T) {
 	fc := newFakeCache()
 	fc.track("http", "127.0.0.1", "/dists/noble")
+	fc.interest("http", "127.0.0.1", "/dists/noble", 14000)
 
 	memberBody := []byte("missing-row member content")
@@ -1650,5 +1664,5 @@ func TestCheck_DetachedFallback_OnMissingInReleaseURLRow(t *testing.T) {
 		memberHash, len(memberBody),
 	))
-	sigBody := []byte("placeholder Release.gpg")
+	sigBody := armoredSig("placeholder Release.gpg")
 
 	var inReleaseHits, releaseHits, gpgHits atomic.Int32
@@ -1679,5 +1693,5 @@ func TestCheck_DetachedFallback_OnMissingInReleaseURLRow(t *testing.T) {
 		CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
 		Path: "/dists/noble/Release", BlobHash: &staleRH,
-		UpstreamURL: srv.URL + "/dists/noble/Release", IsMetadata: true,
+		UpstreamURL: srv.URL + "/dists/noble/Release", IsMetadata: true, RequestCount: 1,
 	})
 	fc.putURL(cache.URLPath{
diff --git a/internal/freshness/legacy_audit_test.go b/internal/freshness/legacy_audit_test.go
index d6c0ad5..d37a23b 100644
--- a/internal/freshness/legacy_audit_test.go
+++ b/internal/freshness/legacy_audit_test.go
@@ -730,4 +730,9 @@ func TestRunLegacyTrustAudit_QuarantinedR1SuiteReadoptsOnFirstCheck(t *testing.T
 		t.Fatalf("audit = %+v (%v), want 1 quarantined", res, err)
 	}
+	// A client fetched the anchor (spec C4 Dispatch 3 needs a
+	// client-created anchor row).
+	if err := env.cache.TouchURLPath(ctx, "http", "h", "/r/dists/p/InRelease"); err != nil {
+		t.Fatal(err)
+	}
 	// A clean upstream: the signed InRelease, and its Packages member for
 	// the adopter's fetcher.
diff --git a/internal/freshness/panic_recovery_test.go b/internal/freshness/panic_recovery_test.go
index 9809dc2..ba249e9 100644
--- a/internal/freshness/panic_recovery_test.go
+++ b/internal/freshness/panic_recovery_test.go
@@ -67,7 +67,7 @@ func (l *lockedBuffer) String() string {
 }
 
-// seedStaleAnchor gives suite /r/dists/x on host h an InRelease anchor
-// whose hash differs from anything upstream serves, so a check sees
-// "changed".
+// seedStaleAnchor gives suite /r/dists/x on host h a client-created
+// InRelease anchor (request_count 1) whose hash differs from anything
+// upstream serves, so a check sees "changed".
 func seedStaleAnchor(t *testing.T, c *cache.Cache) {
 	t.Helper()
@@ -79,5 +79,5 @@ func seedStaleAnchor(t *testing.T, c *cache.Cache) {
 	if err := c.PutURLPath(ctx, cache.URLPath{
 		CanonicalScheme: "http", CanonicalHost: "h", Path: "/r/dists/x/InRelease",
-		BlobHash: &stale, UpstreamURL: "http://h/r/dists/x/InRelease", IsMetadata: true,
+		BlobHash: &stale, UpstreamURL: "http://h/r/dists/x/InRelease", IsMetadata: true, RequestCount: 1,
 	}); err != nil {
 		t.Fatal(err)
@@ -196,4 +196,5 @@ func TestCheck_RepairPanicIsRecovered(t *testing.T) {
 		"Contents-amd64.gz":          []byte(strings.Repeat("C", 100)),
 	})
+	releaseText = asInRelease(releaseText)
 	base := "http://127.0.0.1/dists/noble/"
 	env.fetcher.put(base+"main/binary-amd64/Packages", pkgs)
diff --git a/internal/freshness/phase6_7_skip_repair_test.go b/internal/freshness/phase6_7_skip_repair_test.go
index bdfe916..be7a05a 100644
--- a/internal/freshness/phase6_7_skip_repair_test.go
+++ b/internal/freshness/phase6_7_skip_repair_test.go
@@ -575,4 +575,5 @@ func TestChecker_FreshTickRepairsSkippedMembers(t *testing.T) {
 		"Contents-amd64.gz":          goodContents,
 	})
+	releaseText = asInRelease(releaseText)
 	base := "http://127.0.0.1/dists/noble/"
 	env.fetcher.put(base+"main/binary-amd64/Packages", pkgs)
diff --git a/internal/freshness/reoffer_test.go b/internal/freshness/reoffer_test.go
index 3890153..c949829 100644
--- a/internal/freshness/reoffer_test.go
+++ b/internal/freshness/reoffer_test.go
@@ -96,5 +96,5 @@ func TestRun_ReoffersWhenASlotFrees(t *testing.T) {
 			members.put("http://"+host+rigSuite+"/main/binary-amd64/Packages", pkgs)
 			up.Set("http://"+host+rigSuite+"/InRelease",
-				simtest.Serve(datedRelease("v1-"+host, 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})))
+				simtest.Serve(signedRelease("v1-"+host, 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})))
 		}
 		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
@@ -152,5 +152,5 @@ func TestRun_OfferThatLosesTheSlotKeepsItsPlace(t *testing.T) {
 			pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat(string(rune('a'+i)), 64)})
 			members.put("http://h"+suite+"/main/binary-amd64/Packages", pkgs)
-			rels[suite] = datedRelease("v1"+suite, 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})
+			rels[suite] = signedRelease("v1"+suite, 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})
 		}
 		var aCalls atomic.Int32
@@ -209,5 +209,5 @@ func TestRun_BusyOfferIsRetriedAfterTheNextTick(t *testing.T) {
 		members.put("http://h"+rigSuite+"/main/binary-amd64/Packages", pkgs)
 		anchor := "http://h" + rigSuite + "/InRelease"
-		up.Set(anchor, simtest.Serve(datedRelease("v1", 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})))
+		up.Set(anchor, simtest.Serve(signedRelease("v1", 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})))
 		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
 			HostLimiter: hostsem.New(8), MaxConcurrent: 1, Logger: simtest.DiscardLogger()})
@@ -251,5 +251,5 @@ func TestRun_HeldOfferIsRetriedOnTheNextWake(t *testing.T) {
 		members.put("http://h"+rigSuite+"/main/binary-amd64/Packages", pkgs)
 		anchor := "http://h" + rigSuite + "/InRelease"
-		up.Set(anchor, simtest.Serve(datedRelease("v1", 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})))
+		up.Set(anchor, simtest.Serve(signedRelease("v1", 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})))
 		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
 			HostLimiter: hostsem.New(8), MaxConcurrent: 1, Logger: simtest.DiscardLogger()})
@@ -300,5 +300,5 @@ func TestRun_ClientRefusedSuiteIsParkedByTheNextTick(t *testing.T) {
 		members.put("http://h"+suite+"/main/binary-amd64/Packages", pkgs)
 		anchor := "http://h" + suite + "/InRelease"
-		up.Set(anchor, simtest.Serve(datedRelease("v1", 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})))
+		up.Set(anchor, simtest.Serve(signedRelease("v1", 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})))
 		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
 			HostLimiter: hostsem.New(8), MaxConcurrent: 1, Logger: simtest.DiscardLogger()})
@@ -353,5 +353,5 @@ func TestRun_ClientRefusedBusySuiteIsParkedInOrder(t *testing.T) {
 			members.delay[pu] = 10 * time.Minute
 			up.Set("http://"+host+suite+"/InRelease",
-				simtest.Serve(datedRelease("v1-"+host, 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})))
+				simtest.Serve(signedRelease("v1-"+host, 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})))
 		}
 		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
diff --git a/internal/freshness/retry_gate_test.go b/internal/freshness/retry_gate_test.go
index 54337b2..b9b821a 100644
--- a/internal/freshness/retry_gate_test.go
+++ b/internal/freshness/retry_gate_test.go
@@ -49,4 +49,5 @@ func lastResult(r *retryRig) string {
 func TestGate_DeferredSendsFailingValidators(t *testing.T) {
 	r := newRetryRig(t, 0)
+	r.adopt()
 	bad := failOnce(t, r)
 	seen := r.row().InReleaseChangeSeenAt
@@ -73,4 +74,5 @@ func TestGate_DeferredSendsFailingValidators(t *testing.T) {
 func TestGate_SameContent200IsDeferred(t *testing.T) {
 	r := newRetryRig(t, 0)
+	r.adopt()
 	failOnce(t, r)
 	r.up.setIgnoreValidators(true)
@@ -85,4 +87,5 @@ func TestGate_SameContent200IsDeferred(t *testing.T) {
 func TestGate_NewContentIsAttemptedAtOnce(t *testing.T) {
 	r := newRetryRig(t, 0)
+	r.adopt()
 	failOnce(t, r)
 	r.up.set(r.release("fixed", 0))
@@ -168,4 +171,5 @@ func TestGate_FutureNextRetryClampedToOneHour(t *testing.T) {
 func TestGate_T1StormAfterFailureCostsNothing(t *testing.T) {
 	r := newRetryRig(t, 0)
+	r.adopt()
 	bad := failOnce(t, r)
 	before := len(r.up.etagsSent(rigAnchorURL))
@@ -349,7 +353,7 @@ func TestGate_DetachedResignature(t *testing.T) {
 	for _, via200 := range []bool{false, true} {
 		r := newRetryRig(t, 0)
-		seedDetachedAnchors(t, r.cache)
+		r.adoptDetached()
 		relURL, sigURL := "http://h/r/dists/x/Release", "http://h/r/dists/x/Release.gpg"
-		rel, sig1, sig2 := r.release("det", 0), []byte("sig one"), []byte("sig two")
+		rel, sig1, sig2 := r.releaseText("det", 0), armoredSig("sig one"), armoredSig("sig two")
 		r.ver.reject(contentKey(adoptionFormDetached, nil, rel, sig1))
 		r.up.setAt(relURL, rel)
@@ -388,6 +392,6 @@ func TestGate_DetachedResignature(t *testing.T) {
 func TestGate_DetachedSameContent200IsDeferred(t *testing.T) {
 	r := newRetryRig(t, 0)
-	seedDetachedAnchors(t, r.cache)
-	rel, sig := r.release("det", 0), []byte("sig one")
+	r.adoptDetached()
+	rel, sig := r.releaseText("det", 0), armoredSig("sig one")
 	r.ver.reject(contentKey(adoptionFormDetached, nil, rel, sig))
 	r.up.setAt(rigReleaseURL, rel)
@@ -407,5 +411,5 @@ func TestGate_DetachedCurrentContentClearsTheRecord(t *testing.T) {
 	r := newRetryRig(t, 0)
 	seedDetachedAnchors(t, r.cache)
-	good, goodSig := r.release("good", 0), []byte("good sig")
+	good, goodSig := r.releaseText("good", 0), armoredSig("good sig")
 	r.up.setAt(rigReleaseURL, good)
 	r.up.setAt(rigReleaseGPGURL, goodSig)
@@ -414,5 +418,5 @@ func TestGate_DetachedCurrentContentClearsTheRecord(t *testing.T) {
 		t.Fatal("setup: good pair not adopted")
 	}
-	bad, badSig := r.release("bad", 0), []byte("bad sig")
+	bad, badSig := r.releaseText("bad", 0), armoredSig("bad sig")
 	r.ver.reject(contentKey(adoptionFormDetached, nil, bad, badSig))
 	r.up.setAt(rigReleaseURL, bad)
@@ -432,5 +436,6 @@ func TestGate_DetachedCurrentContentClearsTheRecord(t *testing.T) {
 
 // seedDetachedAnchors removes the rig's InRelease anchor and caches stale
-// Release + Release.gpg anchors, so checks take the detached path.
+// Release + Release.gpg anchors (client-created), so checks take the
+// detached path.
 func seedDetachedAnchors(t *testing.T, c *cache.Cache) {
 	t.Helper()
@@ -446,5 +451,5 @@ func seedDetachedAnchors(t *testing.T, c *cache.Cache) {
 		h := h
 		if err := c.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: rigHost, Path: path,
-			BlobHash: &h, UpstreamURL: "http://h" + path, IsMetadata: true}); err != nil {
+			BlobHash: &h, UpstreamURL: "http://h" + path, IsMetadata: true, RequestCount: 1}); err != nil {
 			t.Fatal(err)
 		}
diff --git a/internal/freshness/retry_rig_test.go b/internal/freshness/retry_rig_test.go
index 1259e0c..043a27b 100644
--- a/internal/freshness/retry_rig_test.go
+++ b/internal/freshness/retry_rig_test.go
@@ -15,4 +15,5 @@ import (
 	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
 	"github.com/linsomniac/apt-cacher-ultra/internal/observability"
+	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
 )
 
@@ -160,5 +161,8 @@ func newRetryRig(t *testing.T, cooldown time.Duration) *retryRig {
 	t.Cleanup(func() { _ = c.Close() })
 	seedStaleAnchor(t, c)
-	if err := c.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite}); err != nil {
+	// Client interest at the rig clock's now (spec C4 Dispatch 3).
+	interest := int64(1_800_000_000)
+	if err := c.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite,
+		LastRequestedAt: &interest}); err != nil {
 		t.Fatal(err)
 	}
@@ -191,10 +195,55 @@ func newRetryRig(t *testing.T, cooldown time.Duration) *retryRig {
 }
 
-// release is a Release body the rig can adopt: label makes it distinct;
-// date != 0 adds a Date field.
+// release is an InRelease body the rig can adopt: the Release text of
+// releaseText in a clearsign envelope (spec C3's sniff passes it, the rig
+// verifier hands it to ParseRelease, which skips the armor lines).
 func (r *retryRig) release(label string, date int64) []byte {
+	return simtest.ClearsignedFixture(string(r.releaseText(label, date)))
+}
+
+// releaseText is a Release body the rig can adopt (detached and unsigned
+// forms): label makes it distinct; date != 0 adds a Date field.
+func (r *retryRig) releaseText(label string, date int64) []byte {
 	return datedRelease(label, date, map[string][]byte{"main/binary-amd64/Packages": r.pkgs})
 }
 
+// armoredSig is a Release.gpg body that passes spec C3's sniff (an armor
+// header); label makes it distinct. The rig verifier never checks it.
+func armoredSig(label string) []byte {
+	return []byte("-----BEGIN PGP SIGNATURE-----\n\n" + label + "\n-----END PGP SIGNATURE-----\n")
+}
+
+// adopt makes the rig suite adopted — a current snapshot of a clearsigned
+// "base" InRelease — so its checks run the current form's conditional GET
+// (spec C4 Dispatch 1), whose validators the spec C7 gate sets. The
+// verifier's call count restarts at 0.
+func (r *retryRig) adopt() {
+	r.t.Helper()
+	r.up.set(r.release("base", 0))
+	r.check()
+	if r.row().CurrentSnapshotID == nil {
+		r.t.Fatal("rig: base content not adopted")
+	}
+	r.ver.mu.Lock()
+	r.ver.n = 0
+	r.ver.mu.Unlock()
+}
+
+// adoptDetached is adopt for the detached form: the rig suite's current
+// snapshot is a "base" Release + Release.gpg pair (seedDetachedAnchors).
+func (r *retryRig) adoptDetached() {
+	r.t.Helper()
+	seedDetachedAnchors(r.t, r.cache)
+	r.up.setAt(rigReleaseURL, r.releaseText("base", 0))
+	r.up.setAt(rigReleaseGPGURL, armoredSig("base sig"))
+	r.check()
+	if r.row().CurrentSnapshotID == nil {
+		r.t.Fatal("rig: base pair not adopted")
+	}
+	r.ver.mu.Lock()
+	r.ver.n = 0
+	r.ver.mu.Unlock()
+}
+
 // check runs one freshness check and waits for any adoption it started.
 func (r *retryRig) check() {
diff --git a/internal/freshness/scheduler_test.go b/internal/freshness/scheduler_test.go
index 5a38f11..4ee3f84 100644
--- a/internal/freshness/scheduler_test.go
+++ b/internal/freshness/scheduler_test.go
@@ -39,7 +39,12 @@ func seedFakeSuite(fc *fakeCache, host, suite string, lastSuccess *int64) {
 	bh := testHex64
 	fc.putURL(cache.URLPath{CanonicalScheme: "http", CanonicalHost: host, Path: suite + "/InRelease",
-		BlobHash: &bh, UpstreamURL: "http://" + host + suite + "/InRelease", IsMetadata: true})
+		BlobHash: &bh, UpstreamURL: "http://" + host + suite + "/InRelease", IsMetadata: true, RequestCount: 1})
+	// Client interest (spec C4): at the last success, else now.
+	interest := time.Now().Unix()
+	if lastSuccess != nil {
+		interest = *lastSuccess
+	}
 	fc.putSuite(cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: host, SuitePath: suite,
-		LastSuccessAt: lastSuccess, LastCheckAt: lastSuccess})
+		LastSuccessAt: lastSuccess, LastCheckAt: lastSuccess, LastRequestedAt: &interest})
 }
 
@@ -316,4 +321,7 @@ func TestSuiteDue(t *testing.T) {
 	}
 	for _, tc := range cases {
+		if tc.s.LastRequestedAt == nil {
+			tc.s.LastRequestedAt = at(-time.Minute) // client interest (spec C4)
+		}
 		if got := c.suiteDue(tc.s, now); got != tc.want {
 			t.Errorf("%s: suiteDue = %v, want %v", tc.name, got, tc.want)
diff --git a/internal/freshness/sim_retry_test.go b/internal/freshness/sim_retry_test.go
index 0cf16ce..2c56055 100644
--- a/internal/freshness/sim_retry_test.go
+++ b/internal/freshness/sim_retry_test.go
@@ -48,9 +48,17 @@ func startRetrySim(t *testing.T, refresh time.Duration) *retrySim {
 
 func (s *retrySim) release(label string, date int64) []byte {
-	return datedRelease(label, date, map[string][]byte{"main/binary-amd64/Packages": s.pkgs})
+	return signedRelease(label, date, map[string][]byte{"main/binary-amd64/Packages": s.pkgs})
 }
 
 func (s *retrySim) serve(body []byte) { s.up.Set(rigAnchorURL, simtest.Serve(body)) }
 
+// touch records a client's request for the suite (its interest stamp).
+func (s *retrySim) touch(t *testing.T) {
+	t.Helper()
+	if err := s.c.TouchSuite(context.Background(), cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite}); err != nil {
+		t.Fatal(err)
+	}
+}
+
 func (s *retrySim) row(t *testing.T) cache.SuiteFreshness {
 	t.Helper()
@@ -130,4 +138,5 @@ func TestSim30Days_RetryDateRulesAndClockSteps(t *testing.T) {
 
 		v2 := s.release("v2", time.Now().Unix())
+		s.touch(t) // apt update: a client asks for the suite again
 		s.serve(v2)
 		time.Sleep(window)
@@ -236,5 +245,5 @@ func TestSimRestartSpread(t *testing.T) {
 			host := fmt.Sprintf("h%02d", i)
 			seedSimSuite(t, c, host, rigSuite, now, 0) // fresh: only the retry makes it due
-			bad := datedRelease("bad-"+host, 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})
+			bad := signedRelease("bad-"+host, 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})
 			ver.reject(hashOf(bad))
 			url := "http://" + host + rigSuite + "/InRelease"
diff --git a/internal/freshness/sim_sched_test.go b/internal/freshness/sim_sched_test.go
index 6067a4c..f69ec01 100644
--- a/internal/freshness/sim_sched_test.go
+++ b/internal/freshness/sim_sched_test.go
@@ -33,5 +33,5 @@ func seedSimSuite(t *testing.T, c *cache.Cache, host, suite string, lastAt, fail
 	}
 	if err := c.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: host, Path: suite + "/InRelease",
-		BlobHash: &bh, UpstreamURL: "http://" + host + suite + "/InRelease", IsMetadata: true}); err != nil {
+		BlobHash: &bh, UpstreamURL: "http://" + host + suite + "/InRelease", IsMetadata: true, RequestCount: 1}); err != nil {
 		t.Fatal(err)
 	}
@@ -102,5 +102,5 @@ func TestSimScheduler_LatencyBoundEightHosts(t *testing.T) {
 		good, bad := []byte(strings.Repeat("C", 100)), []byte(strings.Repeat("z", 90))
 		pkgsR := fakePackagesStanzas(map[string]string{"pool/main/r/r/r_1.deb": testHex64})
-		relR := datedRelease("r", 0, map[string][]byte{"main/binary-amd64/Packages": pkgsR, "Contents-amd64.gz": good})
+		relR := signedRelease("r", 0, map[string][]byte{"main/binary-amd64/Packages": pkgsR, "Contents-amd64.gz": good})
 		rBase := "http://h1/r/dists/s/"
 		rByHash := rBase + "by-hash/SHA256/" + shaOf(good)
@@ -132,5 +132,5 @@ func TestSimScheduler_LatencyBoundEightHosts(t *testing.T) {
 			}
 			rel := func(n int64) []byte {
-				return datedRelease(fmt.Sprintf("c-%s-%d", host, n), 0, map[string][]byte{"main/binary-amd64/Packages": pkgs(n)})
+				return signedRelease(fmt.Sprintf("c-%s-%d", host, n), 0, map[string][]byte{"main/binary-amd64/Packages": pkgs(n)})
 			}
 			members.put(pkgsURL, pkgs(0))
@@ -451,5 +451,5 @@ func TestSimCapacity_SlowDripAndDegraded(t *testing.T) {
 		}
 		rel := func(label string, pkgs []byte) []byte {
-			return datedRelease(label, 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})
+			return signedRelease(label, 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})
 		}
 		pkgsURL := func(host string) string { return "http://" + host + rigSuite + "/main/binary-amd64/Packages" }
diff --git a/internal/handler/phase2_detached_chaos_test.go b/internal/handler/phase2_detached_chaos_test.go
index 7d8b774..d835614 100644
--- a/internal/handler/phase2_detached_chaos_test.go
+++ b/internal/handler/phase2_detached_chaos_test.go
@@ -267,5 +267,7 @@ func makeChaos2DetachedSnapshot(label string) chaos2Snapshot {
 		chaos2PackagesGzPath: pkgsGz,
 	})
-	sig := []byte(fmt.Sprintf("--detached-sig-%s--", label))
+	// An armor header (spec C3's Release.gpg sniff passes): after phase E
+	// a signature that fails the sniff is absent, not adopted.
+	sig := []byte(fmt.Sprintf("-----BEGIN PGP SIGNATURE-----\n\ndetached-sig-%s\n-----END PGP SIGNATURE-----\n", label))
 
 	return chaos2Snapshot{
```

- [ ] **Step 2: Run the suite on the unchanged production code**

Run: `go test -race ./internal/cache/ ./internal/freshness/ ./internal/handler/`
Expected: PASS, every test (the fixtures are neutral on the Task 4 code). `git diff --stat -- '*.go' ':!*_test.go'` lists only `internal/cache/testing_helpers.go`.

- [ ] **Step 3: Commit**

```bash
gofmt -l internal/cache/testing_helpers.go internal/freshness/adoption_date_test.go internal/freshness/capacity_test.go \
  internal/freshness/check_tail_test.go internal/freshness/clockstep_test.go internal/freshness/flip_barrier_test.go \
  internal/freshness/freshness_test.go internal/freshness/legacy_audit_test.go internal/freshness/panic_recovery_test.go \
  internal/freshness/phase6_7_skip_repair_test.go internal/freshness/reoffer_test.go internal/freshness/retry_gate_test.go internal/freshness/retry_rig_test.go \
  internal/freshness/scheduler_test.go internal/freshness/sim_retry_test.go internal/freshness/sim_sched_test.go \
  internal/handler/phase2_detached_chaos_test.go
git add internal/cache/testing_helpers.go internal/freshness/*_test.go internal/handler/phase2_detached_chaos_test.go
git commit -m "Give the check tests' never-adopted suites client interest, client-created anchors and sniff-passing bodies"
```

### Task 6: Eager first adoption — classification, the probe tree, dormancy and the unsigned transition

Spec C4 "Response classification" (defined once: `classify`, the overview's `classPresent`/`classAbsent`/`classUnknown`) and Dispatch 3: a suite with no current snapshot (adopter wired) is checked only while it has client interest — `last_requested_at` within the constant 7-day window — and a client-created `InRelease` or `Release` `url_path` row (`request_count > 0`, never a reconstructed one); otherwise it makes no request and no write, and the scheduler does not queue it (`suiteDue`). An active one runs the probe tree without validators — `InRelease` present → inline; absent → `Release` present → `Release.gpg` present → detached, absent → unsigned candidate — and adopts the form found: `first_adoption`, or `first_adoption_queued` without a slot (`isQueuedResult` parks it; the request passes `signedCurrent = false`, overview "E:" carry-forward). Any unknown probe, a tree that finds neither `InRelease` nor `Release`, and an unsigned candidate while `adoption.adopt_unsigned` is off are failed checks (`failCheck`, streak extended); absent statuses never extend the streak (ruling R-E4). The unsigned candidate passes spec C5 safeguards at the check, before a slot is taken: eligibility (`Adopter.unsignedEligible`: the knob, a signed-seen marker → refusal state `unsigned_downgrade_refused`) and the transition confirmation (two observations of the same `Release` bytes at least `max(cooldown, 5 min)` apart, one hour when either signature file answered 401/403; `unsigned_unconfirmed` until then, the streak restarting whenever the bytes or the reason change — `restartsStreak`, so `adoption_fail_first_at` is the first observation of these bytes; the overview's past-event carry-forward for `adoption_fail_first_at`). The spec C7 gate applies in the tree by content key only (the probes carry no validators; design note 15). Phase 1 (no adopter) keeps the Task 4 algorithm.

The controller's rulings on the plan review add four behaviours here:
- **Pacing (ruling PE-3, design review I-4):** a never-adopted suite whose failure or refusal record is active and not due, and whose last check is younger than `periodic_refresh`, is not probed (`pacedFirst`): client checks (T1) inside that window ask nothing, and the scheduler's refresh check and the due retry still run the tree. A last check stamped in the future reads as infinitely old. Without it a deferred suite downloaded its anchor in full at every client hit (no validators). Cost: new content of a failing suite is attempted within one refresh interval, not at the next client hit.
- **No anchor at all (ruling PE-2):** still a failed check that backs off (spec C4), and it also records the refusal state `anchor_absent` (`anchorAbsentKey`, which lives in `probe.go` from this task on), so the status page shows it.
- **The checker's marker writer (ruling PE-5, design review I-2):** a probe that finds a sniff-passing signature (`InRelease` or `Release.gpg` present) marks the suite's base (`Adopter.markSignatureSeen`, stamped now) before anything is admitted; a failed write fails the check. A signature only the cache's own probe saw is signed history from then on, so a later strip never yields an unsigned candidate.
- **The member cap before a slot (design review M-5):** `unsignedFirst` refuses `unsigned_too_large` (`unsignedTooLarge`, Task 3's `unsignedCap` over `ParseRelease`) before the confirmation, so a too-large unsigned suite never takes a slot.

The interest window is symmetric (design review M-3): a stamp up to 7 days in the future counts, one further ahead is dormant until a client touches the suite again. The executability review's tests for X01, X04, X12 and X13 are part of the test file.

Design notes 13–21, 23, 30 and 35 record the choices the spec leaves open: an interest stamp up to 7 days in the future counts as interested and is reported through `clockStep`, one further ahead does not (a symmetric window); the 401/403 gap is decided by the confirming observation's probes; bytes that already passed confirmation (a record for them from an adoption attempt) are not confirmed again; a future `adoption_fail_first_at` never confirms (deviation 2: the C4 rule would read it as infinitely old and skip the safeguard).

**Files:**
- Create: `internal/freshness/probe.go`, `internal/freshness/first_adoption.go`, `internal/freshness/first_adoption_test.go`
- Modify: `internal/freshness/check.go` (`checkLocked` dispatch), `internal/freshness/freshness.go` (`isQueuedResult`), `internal/freshness/scheduler.go` (`suiteDue`), `internal/freshness/adoption_failure.go` (`recordAdoptionFailure`, `restartsStreak`), `internal/freshness/unsigned.go` (`unsignedTooLarge`, `Adopter.markSignatureSeen`), `internal/cache/signed_seen.go` (the marker writers' AIDEV-NOTE), `internal/freshness/retry_rig_test.go` (`rigUpstream.setFail`); expectations in `internal/freshness/capacity_test.go`, `internal/freshness/reoffer_test.go`, `internal/freshness/freshness_test.go`, `internal/handler/phase2_migration_test.go`

**Interfaces:**
- Consumes: Task 3's `Adopter.unsignedEligible`, `unsignedCap`, `ErrAdoptionUnsignedDowngradeRefused`, `errUnsignedDisabled`, `adoptionFormUnsigned`, `formName`, `contentKey`'s unsigned case and `Adopter.adoptUnsigned`; Task 4's `checkEnd`, `finishCheck`, `failCheck`, `admit`, `checkTotal`; Task 5's rig fixtures (`release`, `releaseText`, `armoredSig`, interest stamps); `cache.ReasonUnsignedUnconfirmed`, `cache.ReasonUnsignedDowngradeRefused`, `cache.ReasonUnsignedTooLarge`, `cache.ReasonAnchorAbsent`, `cache.SignedSeenMark`, `cache.(*Cache).SetAdoptionNextRetry`, `aptpath.SniffInRelease`/`SniffRelease`/`SniffReleaseGPG`; test helpers `newRetryRig`, `rigReleaseURL`, `rigReleaseGPGURL`, `lastResult`, `adoptionTotalFor`, `renderedMetric`, `changedUpstream`, `datedRelease`, `checkTotal`, `testHex64`.
- Produces (package freshness): `probeClass` with `classUnknown`, `classPresent`, `classAbsent`; `probeOutcome{class probeClass; res *fetch.ConditionalResult; status int; err error}` with `cancelled()`, `authDenied()`; `classify(res *fetch.ConditionalResult, err error, sniff func([]byte) bool, conditional bool) probeOutcome`; `anchorURLs{model, modelName string; reconstruct func(string) string}` with `of(name string) string`; `(*Checker).anchorURLsFor(suite SuiteRef, rows map[string]string) anchorURLs`; `(*Checker).probe(fetchCtx context.Context, suite SuiteRef, urls anchorURLs, name string) probeOutcome`; constants `interestWindow` (7 d), `minConfirmGap` (5 min), `deniedConfirmGap` (1 h); `anchorAbsentKey` (`probe.go`; Task 7 records it too); `errNoAnchor`; `(*Checker).confirmGap() time.Duration`; `interested(lastRequested *int64, now time.Time) (ok, future bool)`; `(*Checker).pacedFirst(cur, gate retryGate, now) bool`; `(*Checker).cachedAnchors(ctx, suite) (urls map[string]string, clientCreated bool, err error)`; `probeTree` with `request(suite) *adoptionRequest`, `signatureDenied() bool` and `signatureSeen() bool`; `(*Adopter).markSignatureSeen(ctx, suite, seenAt int64) error`; `unsignedTooLarge(release []byte) error`; `(*Checker).runProbeTree(fetchCtx, suite, urls) probeTree`; `(*Checker).checkFirst(ctx, fetchCtx, out, cur, suite, now, nowUnix) *adoptionRequest`; `(*Checker).unsignedFirst(ctx, cur, req, t probeTree, gate retryGate, now) (proceed bool, reason string, err error)`; `restartsStreak(cur *cache.SuiteFreshness, key, reason string) bool`. Test helpers: `(*rigUpstream).setFail(url string, err error)`; `(*retryRig).interestAt(at int64)`, `(*retryRig).currentSnapshot() *cache.SuiteSnapshot`, `(*retryRig).unsignedRelease(label string, sigErr error) []byte` (`first_adoption_test.go`). Task 7 reuses `classify`, `probe`, `anchorURLsFor`, `cachedAnchors`, `runProbeTree`, `confirmGap`, `restartsStreak`, `anchorAbsentKey`, `signatureSeen`, `markSignatureSeen` and `unsignedTooLarge`.

- [ ] **Step 1: Write the failing tests**

Add `setFail` to the rig upstream (`internal/freshness/retry_rig_test.go`) — the field, the method, and the two lines in `Conditional` (`git apply`):

```diff
diff --git a/internal/freshness/retry_rig_test.go b/internal/freshness/retry_rig_test.go
index 043a27b..6d91334 100644
--- a/internal/freshness/retry_rig_test.go
+++ b/internal/freshness/retry_rig_test.go
@@ -48,4 +48,5 @@ type rigUpstream struct {
 	ignoreValidators bool
 	stall            bool
+	fail             map[string]error    // per URL: the error Conditional returns (setFail)
 	sent             map[string][]string // If-None-Match per URL, in order
 }
@@ -53,4 +54,19 @@ type rigUpstream struct {
 func (u *rigUpstream) set(body []byte) { u.setAt(rigAnchorURL, body) }
 
+// setFail makes every request for url fail with err (nil restores the
+// body or the default 404).
+func (u *rigUpstream) setFail(url string, err error) {
+	u.mu.Lock()
+	defer u.mu.Unlock()
+	if u.fail == nil {
+		u.fail = map[string]error{}
+	}
+	if err == nil {
+		delete(u.fail, url)
+		return
+	}
+	u.fail[url] = err
+}
+
 func (u *rigUpstream) setAt(url string, body []byte) {
 	u.mu.Lock()
@@ -69,5 +85,5 @@ func (u *rigUpstream) Conditional(ctx context.Context, target *fetch.Target, eta
 	u.sent[target.URL] = append(u.sent[target.URL], etag)
 	body, ok := u.bodies[target.URL]
-	stall, ignore := u.stall, u.ignoreValidators
+	stall, ignore, failErr := u.stall, u.ignoreValidators, u.fail[target.URL]
 	u.mu.Unlock()
 	if stall {
@@ -75,4 +91,7 @@ func (u *rigUpstream) Conditional(ctx context.Context, target *fetch.Target, eta
 		return nil, ctx.Err()
 	}
+	if failErr != nil {
+		return nil, failErr
+	}
 	if !ok {
 		return nil, &fetch.StatusError{Code: 404}
```

Create `internal/freshness/first_adoption_test.go`:

```go
package freshness

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
)

// Spec C4 "Response classification", defined once: every status class.
func TestClassify_Table(t *testing.T) {
	pass := func([]byte) bool { return true }
	fail := func([]byte) bool { return false }
	status := func(code int) error { return &fetch.StatusError{Code: code} }
	cases := []struct {
		name        string
		res         *fetch.ConditionalResult
		err         error
		sniff       func([]byte) bool
		conditional bool
		want        probeClass
	}{
		{"200 passing the sniff", &fetch.ConditionalResult{Status: 200, Body: []byte("x")}, nil, pass, false, classPresent},
		{"200 failing the sniff (soft 404)", &fetch.ConditionalResult{Status: 200, Body: []byte("<html>")}, nil, fail, false, classAbsent},
		{"304 to a conditional request", &fetch.ConditionalResult{Status: 304}, nil, fail, true, classPresent},
		{"304 to an unconditional request", &fetch.ConditionalResult{Status: 304}, nil, pass, false, classUnknown},
		{"206", &fetch.ConditionalResult{Status: 206}, nil, pass, false, classUnknown},
		{"401", nil, status(401), pass, false, classAbsent},
		{"403", nil, status(403), pass, false, classAbsent},
		{"404", nil, status(404), pass, false, classAbsent},
		{"410", nil, status(410), pass, false, classAbsent},
		{"400", nil, status(400), pass, false, classUnknown},
		{"408", nil, status(408), pass, false, classUnknown},
		{"429", nil, status(429), pass, false, classUnknown},
		{"500", nil, status(500), pass, false, classUnknown},
		{"503", nil, status(503), pass, false, classUnknown},
		{"an unfollowed 3xx", nil, fmt.Errorf("%w: %w", fetch.ErrRedirectBlocked, status(302)), pass, false, classUnknown},
		{"transport error", nil, errors.New("connection reset by peer"), pass, false, classUnknown},
		{"host not allowed", nil, fetch.ErrHostNotAllowed, pass, false, classUnknown},
		{"body over its limit", nil, fetch.ErrConditionalBodyTooLarge, pass, false, classUnknown},
		{"check deadline", nil, context.DeadlineExceeded, pass, false, classUnknown},
		{"cancelled caller", nil, context.Canceled, pass, false, classUnknown},
	}
	for _, tc := range cases {
		o := classify(tc.res, tc.err, tc.sniff, tc.conditional)
		if o.class != tc.want {
			t.Errorf("%s: class %v, want %v", tc.name, o.class, tc.want)
		}
		if got, want := o.authDenied(), tc.name == "401" || tc.name == "403"; got != want {
			t.Errorf("%s: authDenied = %v, want %v", tc.name, got, want)
		}
		if got, want := o.cancelled(), tc.name == "cancelled caller"; got != want {
			t.Errorf("%s: cancelled = %v, want %v", tc.name, got, want)
		}
	}
}

// The probe tree's URLs: a sibling of a cached anchor row's upstream_url
// keeps its port and prefix; without a row, the suite identity.
func TestAnchorURLs(t *testing.T) {
	recon := func(name string) string { return "http://h/r/dists/x/" + name }
	u := anchorURLs{model: "http://h:8080/mirror/r/dists/x/Release", modelName: "Release", reconstruct: recon}
	if got := u.of("InRelease"); got != "http://h:8080/mirror/r/dists/x/InRelease" {
		t.Errorf("sibling = %q", got)
	}
	if got := (anchorURLs{reconstruct: recon}).of("Release.gpg"); got != "http://h/r/dists/x/Release.gpg" {
		t.Errorf("no model: %q", got)
	}
	if got := (anchorURLs{model: "http://h/odd", modelName: "InRelease", reconstruct: recon}).of("Release"); got != "http://h/r/dists/x/Release" {
		t.Errorf("a model not ending in its name: %q", got)
	}
}

// interestAt reseeds the rig suite's client interest at unix time at.
func (r *retryRig) interestAt(at int64) {
	r.t.Helper()
	if err := r.cache.SeedSuiteForTest(context.Background(), cache.SuiteFreshness{CanonicalScheme: "http",
		CanonicalHost: rigHost, SuitePath: rigSuite, LastRequestedAt: &at}); err != nil {
		r.t.Fatal(err)
	}
}

// Spec C4 Dispatch 3 (the phase E DoD's eager first adoption): a suite
// with client interest adopts on its first check even when upstream still
// serves what the client fetched — the probe tree sends no validators, so
// the suite's validators cannot turn it into a 304 that adopts nothing.
func TestCheckFirst_AdoptsUnchangedUpstreamOnTheFirstCheck(t *testing.T) {
	r := newRetryRig(t, 0)
	body := r.release("v1", 0)
	r.up.set(body)
	etag, interest := rigETag(body), r.clock.now().Unix()
	if err := r.cache.SeedSuiteForTest(context.Background(), cache.SuiteFreshness{CanonicalScheme: "http",
		CanonicalHost: rigHost, SuitePath: rigSuite, InReleaseETag: &etag, LastRequestedAt: &interest}); err != nil {
		t.Fatal(err)
	}
	r.check()
	if sent := r.up.etagsSent(rigAnchorURL); len(sent) != 1 || sent[0] != "" {
		t.Fatalf("validators sent = %q, want one probe without validators", sent)
	}
	ev := r.events("freshness_check")
	if len(ev) != 1 || ev[0]["result"] != "first_adoption" || ev[0]["form"] != "inline" {
		t.Fatalf("freshness_check = %v, want one first_adoption of the inline form", ev)
	}
	snap := r.currentSnapshot()
	if snap == nil || snap.Form != cache.FormInline || strOf(snap.SingleAnchorHash) != hashOf(body) {
		t.Fatalf("current snapshot %+v, want the inline v1", snap)
	}
	if r.ver.calls() != 1 {
		t.Fatalf("verifier calls = %d, want 1", r.ver.calls())
	}
}

// currentSnapshot returns the rig suite's current snapshot, or nil.
func (r *retryRig) currentSnapshot() *cache.SuiteSnapshot {
	r.t.Helper()
	row := r.row()
	if row.CurrentSnapshotID == nil {
		return nil
	}
	snap, err := r.cache.GetSuiteSnapshot(context.Background(), *row.CurrentSnapshotID)
	if err != nil {
		r.t.Fatal(err)
	}
	return snap
}

// Spec C4 Dispatch 3: each form the probe tree finds is adopted, and the
// probes stop at the first present signed anchor.
func TestCheckFirst_ProbeTreeAdoptsTheFormFound(t *testing.T) {
	t.Run("inline", func(t *testing.T) {
		r := newRetryRig(t, 0)
		r.up.set(r.release("v1", 0))
		r.up.setAt(rigReleaseURL, r.releaseText("v1", 0))
		r.check()
		if snap := r.currentSnapshot(); snap == nil || snap.Form != cache.FormInline {
			t.Fatalf("snapshot %+v, want inline", snap)
		}
		if n := len(r.up.etagsSent(rigReleaseURL)); n != 0 {
			t.Fatalf("Release probed %d times after a present InRelease, want 0", n)
		}
	})
	t.Run("detached", func(t *testing.T) {
		r := newRetryRig(t, 0)
		rel, sig := r.releaseText("v1", 0), armoredSig("v1 sig")
		r.up.setAt(rigReleaseURL, rel)
		r.up.setAt(rigReleaseGPGURL, sig)
		r.check()
		snap := r.currentSnapshot()
		if snap == nil || snap.Form != cache.FormDetached || strOf(snap.ReleaseHash) != hashOf(rel) || strOf(snap.ReleaseGPGHash) != hashOf(sig) {
			t.Fatalf("snapshot %+v, want the detached pair", snap)
		}
		if ev := r.events("freshness_check"); ev[len(ev)-1]["result"] != "first_adoption" || ev[len(ev)-1]["form"] != "detached" {
			t.Fatalf("last freshness_check = %v, want first_adoption detached", ev[len(ev)-1])
		}
	})
	t.Run("an HTML InRelease is absent", func(t *testing.T) {
		r := newRetryRig(t, 0)
		r.up.set([]byte("<html>moved</html>"))
		r.up.setAt(rigReleaseURL, r.releaseText("v1", 0))
		r.up.setAt(rigReleaseGPGURL, armoredSig("v1 sig"))
		r.check()
		if snap := r.currentSnapshot(); snap == nil || snap.Form != cache.FormDetached {
			t.Fatalf("snapshot %+v, want detached", snap)
		}
	})
}

// Spec C4 "Response classification" per anchor of the probe tree: absent
// moves on, unknown fails the check at once (no further probe, the streak
// extends), and a tree that finds no anchor fails it too — and records
// anchor_absent, so the status page shows it (ruling PE-2).
func TestCheckFirst_ClassificationPerAnchor(t *testing.T) {
	unknowns := map[string]error{
		"503":       &fetch.StatusError{Code: 503},
		"429":       &fetch.StatusError{Code: 429},
		"redirect":  fmt.Errorf("%w: %w", fetch.ErrRedirectBlocked, &fetch.StatusError{Code: 302}),
		"transport": errors.New("connection reset by peer"),
	}
	absents := map[string]error{
		"401": &fetch.StatusError{Code: 401}, "403": &fetch.StatusError{Code: 403},
		"404": &fetch.StatusError{Code: 404}, "410": &fetch.StatusError{Code: 410},
	}
	for name, err := range unknowns {
		for _, url := range []string{rigAnchorURL, rigReleaseURL, rigReleaseGPGURL} {
			t.Run("unknown "+name+" at "+url, func(t *testing.T) {
				r := newRetryRig(t, 0)
				r.up.setAt(rigReleaseURL, r.releaseText("v1", 0))
				r.up.setAt(rigReleaseGPGURL, armoredSig("v1 sig"))
				r.up.setFail(url, err)
				before := checkTotal(t, "failed", rigHost)
				r.check()
				row := r.row()
				if row.CheckFailCount != 1 || row.CurrentSnapshotID != nil || row.LastSuccessAt != nil {
					t.Fatalf("row %+v, want a failed check: streak 1, nothing adopted, no success", row)
				}
				if got := checkTotal(t, "failed", rigHost) - before; got != 1 {
					t.Fatalf("failed checks counted %v, want 1", got)
				}
				if url == rigAnchorURL && len(r.up.etagsSent(rigReleaseURL)) != 0 {
					t.Fatal("the tree went on after an unknown InRelease")
				}
			})
		}
	}
	for name, err := range absents {
		t.Run("absent "+name+" InRelease moves on", func(t *testing.T) {
			r := newRetryRig(t, 0)
			r.up.setFail(rigAnchorURL, err)
			r.up.setAt(rigReleaseURL, r.releaseText("v1", 0))
			r.up.setAt(rigReleaseGPGURL, armoredSig("v1 sig"))
			r.check()
			if snap := r.currentSnapshot(); snap == nil || snap.Form != cache.FormDetached {
				t.Fatalf("snapshot %+v, want detached", snap)
			}
			if r.row().CheckFailCount != 0 {
				t.Fatal("an absent anchor extended the streak")
			}
		})
	}
	t.Run("no anchor at all", func(t *testing.T) {
		r := newRetryRig(t, 0)
		r.up.setFail(rigReleaseURL, &fetch.StatusError{Code: 410})
		r.check()
		if row := r.row(); row.CheckFailCount != 1 || row.LastCheckAt == nil {
			t.Fatalf("row %+v, want a failed check", row)
		}
		if ev := r.events("freshness check failed"); len(ev) != 1 || ev[0]["form"] != "probe" {
			t.Fatalf("failed-check lines %v, want one with form probe", ev)
		}
		if row := r.row(); strOf(row.AdoptionFailReason) != cache.ReasonAnchorAbsent || row.AdoptionFailCount != 1 {
			t.Fatalf("record %q/%d, want anchor_absent/1", strOf(row.AdoptionFailReason), row.AdoptionFailCount)
		}
	})
}

// Spec C4 Dispatch 3: a never-adopted suite without client interest in the
// last 7 days, or without a client-created anchor row, makes no request and
// no write; an interest stamp up to 7 days in the future counts and is
// reported as a clock step, one further ahead is dormant (design review M-3:
// a symmetric window).
func TestCheckFirst_DormantSuitesMakeNoRequest(t *testing.T) {
	now := int64(1_800_000_000)
	for _, tc := range []struct {
		name    string
		prepare func(r *retryRig)
	}{
		{"interest exactly 7 days old", func(r *retryRig) { r.interestAt(now - 7*24*3600) }},
		{"interest 8 days old", func(r *retryRig) { r.interestAt(now - 8*24*3600) }},
		{"interest 8 days in the future", func(r *retryRig) { r.interestAt(now + 8*24*3600) }},
		{"a reconstructed anchor row (request_count 0)", func(r *retryRig) {
			stale := testHex64
			if err := r.cache.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: "http", CanonicalHost: rigHost,
				Path: rigSuite + "/InRelease", BlobHash: &stale, UpstreamURL: rigAnchorURL, IsMetadata: true}); err != nil {
				r.t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRetryRig(t, 0)
			r.up.set(r.release("v1", 0))
			tc.prepare(r)
			r.check()
			if n := len(r.up.etagsSent(rigAnchorURL)); n != 0 {
				t.Fatalf("%d requests for a dormant suite, want 0", n)
			}
			if row := r.row(); row.LastCheckAt != nil || len(r.events("freshness_check")) != 0 {
				t.Fatalf("a dormant suite was written or logged: %+v", row)
			}
		})
	}
	t.Run("interest in the future", func(t *testing.T) {
		r := newRetryRig(t, 0)
		r.up.set(r.release("v1", 0))
		r.interestAt(now + 3600)
		steps := func() float64 {
			v, _ := strconv.ParseFloat(renderedMetric("acu_clock_steps_detected_total"), 64)
			return v
		}
		before := steps()
		r.check()
		if r.currentSnapshot() == nil {
			t.Fatal("a suite whose interest stamp lies in the future was not adopted")
		}
		if steps()-before != 1 {
			t.Fatal("the future interest stamp was not reported as a clock step")
		}
	})
}

// Spec C4 Dispatch 3: the scheduler never queues a dormant never-adopted
// suite (zero requests), but still queues an adopted one without interest,
// and Phase 1 (no adopter) ignores interest.
func TestSuiteDue_DormantNeverAdoptedSuites(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	at := func(d time.Duration) *int64 { v := now.Add(d).Unix(); return &v }
	id := int64(1)
	stale := at(-2 * time.Hour)
	withAdopter, err := New(Config{Cache: newFakeCache(), Fetcher: &changedUpstream{}, HostLimiter: hostsem.New(1),
		Refresh: time.Hour, Adopter: &Adopter{}, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	phase1, err := New(Config{Cache: newFakeCache(), Fetcher: &changedUpstream{}, HostLimiter: hostsem.New(1),
		Refresh: time.Hour, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name             string
		s                cache.SuiteFreshness
		want, wantPhase1 bool
	}{
		{"never adopted, interested", cache.SuiteFreshness{LastSuccessAt: stale, LastRequestedAt: at(-time.Hour)}, true, true},
		{"never adopted, no interest stamp", cache.SuiteFreshness{LastSuccessAt: stale}, false, true},
		{"never adopted, interest 8 days old", cache.SuiteFreshness{LastSuccessAt: stale, LastRequestedAt: at(-8 * 24 * time.Hour)}, false, true},
		{"never adopted, retry due but dormant", cache.SuiteFreshness{LastSuccessAt: stale, LastRequestedAt: at(-8 * 24 * time.Hour),
			AdoptionFailCount: 1, AdoptionNextRetryAt: at(-time.Minute)}, false, true},
		{"never adopted, interest in the future", cache.SuiteFreshness{LastSuccessAt: stale, LastRequestedAt: at(time.Hour)}, true, true},
		{"never adopted, interest 8 days in the future", cache.SuiteFreshness{LastSuccessAt: stale, LastRequestedAt: at(8 * 24 * time.Hour)}, false, true},
		{"adopted, no interest", cache.SuiteFreshness{LastSuccessAt: stale, CurrentSnapshotID: &id}, true, true},
	}
	for _, tc := range cases {
		if got := withAdopter.suiteDue(tc.s, now); got != tc.want {
			t.Errorf("%s: suiteDue = %v, want %v", tc.name, got, tc.want)
		}
		if got := phase1.suiteDue(tc.s, now); got != tc.wantPhase1 {
			t.Errorf("%s (no adopter): suiteDue = %v, want %v", tc.name, got, tc.wantPhase1)
		}
	}
}

// Spec C7 in the probe tree: the probes carry no validators, so the gate
// compares content keys — the failing content is deferred until its retry
// time, new content is attempted at its first probe. A deferred suite is
// probed only by the scheduler's refresh check and at its retry (ruling
// PE-3), so each step comes a refresh interval after the last check, and a
// long retry time is set directly, as a long backoff would.
func TestCheckFirst_GateComparesContentKeys(t *testing.T) {
	r := newRetryRig(t, 0)
	bad := r.release("bad", 0)
	r.ver.reject(hashOf(bad))
	r.up.set(bad)
	r.check()
	if r.ver.calls() != 1 || r.row().AdoptionFailCount != 1 {
		t.Fatalf("setup: calls %d count %d", r.ver.calls(), r.row().AdoptionFailCount)
	}
	retryIn := func(d time.Duration) {
		t.Helper()
		if err := r.cache.SetAdoptionNextRetry(context.Background(), "http", rigHost, rigSuite, r.clock.now().Add(d).Unix()); err != nil {
			t.Fatal(err)
		}
	}
	retryIn(time.Hour)
	r.clock.advance(16 * time.Minute)
	r.check()
	if r.ver.calls() != 1 || lastResult(r) != "changed_deferred" {
		t.Fatalf("same content before its retry: calls %d result %q, want 1 changed_deferred", r.ver.calls(), lastResult(r))
	}
	if ev := r.events("freshness_check"); ev[len(ev)-1]["reason"] != "gpg_failed" {
		t.Fatalf("reason = %v, want gpg_failed", ev[len(ev)-1]["reason"])
	}
	for _, e := range r.up.etagsSent(rigAnchorURL) {
		if e != "" {
			t.Fatalf("a probe carried validator %q", e)
		}
	}
	r.clock.advance(45 * time.Minute) // past the retry time
	r.check()
	if r.ver.calls() != 2 {
		t.Fatalf("at the retry time: calls %d, want 2", r.ver.calls())
	}
	retryIn(time.Hour)
	r.up.set(r.release("fixed", 0))
	r.clock.advance(16 * time.Minute)
	r.check()
	if r.ver.calls() != 3 || r.currentSnapshot() == nil {
		t.Fatalf("new content: calls %d, adopted %v; want it attempted at its first probe and adopted", r.ver.calls(), r.currentSnapshot() != nil)
	}
}

// unsignedRelease serves an unsigned upstream to the rig: no InRelease
// (404), the Release text, and Release.gpg failing with sigErr (nil: 404).
func (r *retryRig) unsignedRelease(label string, sigErr error) []byte {
	rel := r.releaseText(label, 0)
	r.up.setAt(rigReleaseURL, rel)
	r.up.setFail(rigReleaseGPGURL, sigErr)
	return rel
}

// Spec C5 safeguard 2: the transition into unsigned needs two observations
// of the same Release bytes at least max(cooldown, 5 min) apart; the first
// records unsigned_unconfirmed, the streak (and adoption_fail_first_at)
// restarts when the bytes change, and the adoption is the unsigned form.
func TestCheckFirst_UnsignedTransitionIsConfirmed(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopter.adoptUnsigned = true
	t0 := r.clock.now().Unix()
	r.unsignedRelease("u1", nil)
	r.check()
	row := r.row()
	if lastResult(r) != "changed_deferred" || strOf(row.AdoptionFailReason) != cache.ReasonUnsignedUnconfirmed ||
		row.AdoptionFailCount != 1 || i64Of(row.AdoptionFailFirstAt) != t0 {
		t.Fatalf("first observation: result %q record %q/%d/%d", lastResult(r), strOf(row.AdoptionFailReason), row.AdoptionFailCount, i64Of(row.AdoptionFailFirstAt))
	}
	if ev := r.events("freshness_check"); ev[len(ev)-1]["reason"] != cache.ReasonUnsignedUnconfirmed || ev[len(ev)-1]["form"] != "unsigned" {
		t.Fatalf("freshness_check %v, want reason unsigned_unconfirmed and form unsigned", ev[len(ev)-1])
	}
	r.clock.advance(time.Minute)
	lines := len(r.events("freshness_check"))
	r.check() // before the record's retry time: paced (ruling PE-3), nothing probed or recorded
	if row := r.row(); row.AdoptionFailCount != 1 || lastResult(r) != "changed_deferred" || len(r.events("freshness_check")) != lines {
		t.Fatalf("inside the retry time: count %d result %q, %d new lines", row.AdoptionFailCount, lastResult(r),
			len(r.events("freshness_check"))-lines)
	}
	// New bytes restart the confirmation.
	r.unsignedRelease("u2", nil)
	r.clock.advance(time.Minute)
	t2 := r.clock.now().Unix()
	r.check()
	if row := r.row(); row.AdoptionFailCount != 1 || i64Of(row.AdoptionFailFirstAt) != t2 {
		t.Fatalf("new bytes: count %d first %d, want a new streak at %d", row.AdoptionFailCount, i64Of(row.AdoptionFailFirstAt), t2)
	}
	r.clock.advance(2 * time.Minute) // due, but only 2 min after the first observation
	r.check()
	if row := r.row(); row.CurrentSnapshotID != nil || row.AdoptionFailCount != 2 || i64Of(row.AdoptionFailFirstAt) != t2 {
		t.Fatalf("2 min apart: adopted %v count %d first %d; want unconfirmed, count 2, the same first", row.CurrentSnapshotID != nil,
			row.AdoptionFailCount, i64Of(row.AdoptionFailFirstAt))
	}
	r.clock.advance(4 * time.Minute) // due again, 6 min after the first observation
	r.check()
	snap := r.currentSnapshot()
	if snap == nil || snap.Form != cache.FormUnsigned || lastResult(r) != "first_adoption" {
		t.Fatalf("confirmed: snapshot %+v result %q, want an unsigned first adoption", snap, lastResult(r))
	}
	if r.ver.calls() != 0 {
		t.Fatalf("verifier calls = %d for an unsigned adoption, want 0", r.ver.calls())
	}
}

// Spec C5 safeguard 2: a 401 or 403 on a signature file stretches the gap
// to an hour (WAF and S3-permission incidents).
func TestCheckFirst_UnsignedAfterADeniedSignatureWaitsAnHour(t *testing.T) {
	for _, denied := range []string{"InRelease", "Release.gpg"} {
		t.Run(denied, func(t *testing.T) {
			r := newRetryRig(t, 0)
			r.adopter.adoptUnsigned = true
			var sigErr error
			if denied == "InRelease" {
				r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 403})
			} else {
				sigErr = &fetch.StatusError{Code: 401}
			}
			r.unsignedRelease("u1", sigErr)
			for elapsed := time.Duration(0); elapsed < 59*time.Minute; elapsed += 2 * time.Minute {
				r.check()
				if r.row().CurrentSnapshotID != nil {
					t.Fatalf("adopted %v after the first observation, want an hour", elapsed)
				}
				r.clock.advance(2 * time.Minute)
			}
			r.clock.advance(65 * time.Minute) // past the hour and the record's retry time
			r.check()
			if snap := r.currentSnapshot(); snap == nil || snap.Form != cache.FormUnsigned {
				t.Fatalf("not adopted after the hour: %+v", snap)
			}
		})
	}
}

// Spec C5 safeguard 3 at eligibility: a suite whose base has a signed-seen
// marker is refused before a slot is taken or anything downloaded, and the
// refusal is a recorded state (changed_deferred, unsigned_downgrade_refused).
func TestCheckFirst_UnsignedRefusedWithSignedHistory(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopter.adoptUnsigned = true
	if err := r.cache.MarkSignedSeenBatch(context.Background(), []cache.SignedSeenMark{{CanonicalScheme: "http",
		CanonicalHost: rigHost, Base: rigSuite, SeenAt: r.clock.now().Unix()}}); err != nil {
		t.Fatal(err)
	}
	r.unsignedRelease("u1", nil)
	before := adoptionTotalFor(t, rigHost)
	r.check()
	row := r.row()
	if strOf(row.AdoptionFailReason) != cache.ReasonUnsignedDowngradeRefused || lastResult(r) != "changed_deferred" {
		t.Fatalf("record %q result %q, want unsigned_downgrade_refused and changed_deferred", strOf(row.AdoptionFailReason), lastResult(r))
	}
	if adoptionTotalFor(t, rigHost) != before {
		t.Fatal("an adoption ran for a suite with signed history")
	}
	r.clock.advance(2 * time.Hour) // never confirms
	r.check()
	if r.row().CurrentSnapshotID != nil {
		t.Fatal("an unsigned snapshot was adopted for a suite with signed history")
	}
}

// adoption.adopt_unsigned = false: an unsigned candidate is a failed check.
func TestCheckFirst_UnsignedDisabledIsAFailedCheck(t *testing.T) {
	r := newRetryRig(t, 0)
	r.unsignedRelease("u1", nil)
	r.check()
	if row := r.row(); row.CheckFailCount != 1 || row.CurrentSnapshotID != nil || row.AdoptionFailCount != 0 {
		t.Fatalf("row %+v, want a failed check with no record", row)
	}
	if ev := r.events("freshness check failed"); len(ev) != 1 || ev[0]["form"] != "unsigned" {
		t.Fatalf("failed-check lines %v, want one with form unsigned", ev)
	}
}

// Deviation from the C4 clock-step rule (design note): an
// adoption_fail_first_at in the future never confirms an unsigned
// transition; the next observation restarts the streak.
func TestCheckFirst_FutureConfirmationStartNeverConfirms(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopter.adoptUnsigned = true
	rel := r.unsignedRelease("u1", nil)
	now := r.clock.now()
	if err := r.cache.RecordAdoptionFailure(context.Background(), "http", rigHost, rigSuite, cache.AdoptionFailure{
		Hash: contentKey(adoptionFormUnsigned, nil, rel, nil), Reason: cache.ReasonUnsignedUnconfirmed, Count: 1,
		FirstAt: now.Add(2 * time.Hour).Unix(), LastAt: now.Unix(), NextRetryAt: now.Unix()}); err != nil {
		t.Fatal(err)
	}
	r.check()
	row := r.row()
	if row.CurrentSnapshotID != nil {
		t.Fatal("a future streak start confirmed the unsigned transition")
	}
	if i64Of(row.AdoptionFailFirstAt) != now.Unix() {
		t.Fatalf("first_at = %d, want the streak restarted at %d", i64Of(row.AdoptionFailFirstAt), now.Unix())
	}
}

// isQueuedResult: the scheduler parks first adoptions that found no slot.
func TestIsQueuedResult_FirstAdoption(t *testing.T) {
	for result, want := range map[string]bool{"changed_queued": true, "first_adoption_queued": true,
		"first_adoption": false, "changed": false, "changed_deferred": false} {
		if got := isQueuedResult(result); got != want {
			t.Errorf("isQueuedResult(%q) = %v, want %v", result, got, want)
		}
	}
}

// Ruling PE-3 (design review I-4): a never-adopted suite whose record is
// active and not due is probed only by the scheduler's refresh check (its
// last check at least periodic_refresh old) and at its due retry — a
// client check (T1) inside that window asks nothing, so a deferred suite
// no longer downloads its anchor in full at every client hit. A last check
// stamped in the future reads as infinitely old (clock-step rule).
func TestCheckFirst_DeferredSuiteIgnoresClientChecksUntilItsRetry(t *testing.T) {
	r := newRetryRig(t, 0)
	bad := r.release("bad", 0)
	r.ver.reject(hashOf(bad))
	r.up.set(bad)
	r.check() // the attempt fails: a record due in 2 minutes
	probes := func() int { return len(r.up.etagsSent(rigAnchorURL)) }
	if probes() != 1 || r.row().AdoptionFailCount != 1 {
		t.Fatalf("setup: %d probes, record count %d", probes(), r.row().AdoptionFailCount)
	}
	lines := len(r.events("freshness_check"))
	for range 10 { // client checks inside the window
		r.clock.advance(10 * time.Second)
		r.check()
	}
	if probes() != 1 || len(r.events("freshness_check")) != lines || r.ver.calls() != 1 {
		t.Fatalf("client checks inside the window: %d probes, %d lines, %d attempts; want none",
			probes()-1, len(r.events("freshness_check"))-lines, r.ver.calls()-1)
	}
	r.clock.advance(time.Minute + time.Second) // the retry is due
	r.check()
	if probes() != 2 || r.ver.calls() != 2 {
		t.Fatalf("the due retry: %d probes, %d attempts; want one tree and one attempt", probes()-1, r.ver.calls()-1)
	}
	// A long retry time (as a long backoff would set), then the scheduler's
	// refresh check: the last check is periodic_refresh (15 min) old.
	if err := r.cache.SetAdoptionNextRetry(context.Background(), "http", rigHost, rigSuite, r.clock.now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	r.clock.advance(14 * time.Minute)
	r.check()
	if probes() != 2 {
		t.Fatal("a check 14 minutes after the last one probed a deferred suite")
	}
	r.clock.advance(time.Minute)
	r.check()
	if probes() != 3 || lastResult(r) != "changed_deferred" {
		t.Fatalf("the refresh check: %d probes, result %q; want one more tree, changed_deferred", probes()-2, lastResult(r))
	}
	future := r.clock.now().Add(24 * time.Hour).Unix()
	row := r.row()
	row.LastCheckAt = &future
	if err := r.cache.UpdateSuiteFreshness(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	r.clock.advance(time.Minute)
	r.check()
	if probes() != 4 {
		t.Fatal("a deferred suite whose last check lies in the future was not probed")
	}
}

// Spec C3 revision 8 (ruling PE-5, design review I-2): a probe that finds
// a signature — InRelease, or Release.gpg — for a suite without a signed
// current snapshot marks the suite's base, stamped now, before anything is
// admitted, even when the signed adoption then fails; so stripping the
// signatures later (an upstream, or a man-in-the-middle, answering 404)
// never makes the suite an unsigned candidate. A marker write that fails
// fails the check, and nothing is attempted.
func TestCheckFirst_ProbedSignatureMarksTheBase(t *testing.T) {
	for _, form := range []string{"inline", "detached"} {
		t.Run(form, func(t *testing.T) {
			r := newRetryRig(t, 0)
			r.adopter.adoptUnsigned = true
			if form == "inline" {
				signed := r.release("signed", 0)
				r.ver.reject(hashOf(signed))
				r.up.set(signed)
			} else {
				rel, sig := r.releaseText("signed", 0), armoredSig("its signature")
				r.ver.reject(contentKey(adoptionFormDetached, nil, rel, sig))
				r.up.setAt(rigReleaseURL, rel)
				r.up.setAt(rigReleaseGPGURL, sig)
			}
			r.check()
			if r.currentSnapshot() != nil || r.ver.calls() != 1 {
				t.Fatalf("setup: the signed attempt must fail (verifier calls %d)", r.ver.calls())
			}
			seen, ok, err := r.cache.SignedSeen(context.Background(), "http", rigHost, rigSuite)
			if err != nil || !ok || seen != r.clock.now().Unix() {
				t.Fatalf("marker = %d, %v, %v; want the probe's sighting at %d", seen, ok, err, r.clock.now().Unix())
			}
			// The signatures are stripped (both 404); the Release text stays.
			r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 404})
			r.unsignedRelease("stripped", &fetch.StatusError{Code: 404})
			r.clock.advance(time.Hour)
			r.check()
			if row := r.row(); strOf(row.AdoptionFailReason) != cache.ReasonUnsignedDowngradeRefused || row.CurrentSnapshotID != nil {
				t.Fatalf("record %q, current %v; want unsigned_downgrade_refused and nothing adopted",
					strOf(row.AdoptionFailReason), row.CurrentSnapshotID)
			}
		})
	}
	t.Run("failed marker write", func(t *testing.T) {
		r := newRetryRig(t, 0)
		r.up.set(r.release("signed", 0))
		db, err := sql.Open("sqlite", filepath.Join(r.cache.Dir(), "cache.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		if _, err := db.Exec(`CREATE TRIGGER no_marks BEFORE INSERT ON suite_signed_seen
  BEGIN SELECT RAISE(ABORT, 'marker writes fail'); END`); err != nil {
			t.Fatal(err)
		}
		r.check()
		if row := r.row(); row.CheckFailCount != 1 || row.CurrentSnapshotID != nil || r.ver.calls() != 0 {
			t.Fatalf("row %+v, verifier calls %d: want a failed check and no attempt", row, r.ver.calls())
		}
	})
}

// Design review M-5 (spec C5 safeguard 4, before a slot): an unsigned
// Release declaring more than unsignedMemberCap members is refused at the
// check — unsigned_too_large recorded at its first observation, no
// confirmation, no slot, nothing fetched — and stays refused at its retry.
func TestCheckFirst_UnsignedTooLargeNeverTakesASlot(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopter.adoptUnsigned = true
	big := make(map[string][]byte, unsignedMemberCap+1)
	for i := 0; i <= unsignedMemberCap; i++ {
		big[fmt.Sprintf("main/i18n/Translation-%d", i)] = []byte{byte(i)}
	}
	r.up.setAt(rigReleaseURL, datedRelease("big", 0, big))
	before := adoptionTotalFor(t, rigHost)
	r.check()
	if row := r.row(); strOf(row.AdoptionFailReason) != cache.ReasonUnsignedTooLarge || lastResult(r) != "changed_deferred" {
		t.Fatalf("first observation: record %q result %q, want unsigned_too_large, changed_deferred",
			strOf(row.AdoptionFailReason), lastResult(r))
	}
	r.clock.advance(time.Hour)
	r.check()
	if row := r.row(); row.CurrentSnapshotID != nil || strOf(row.AdoptionFailReason) != cache.ReasonUnsignedTooLarge || row.AdoptionFailCount != 2 {
		t.Fatalf("at the retry: current %v record %q/%d; want still refused, count 2",
			row.CurrentSnapshotID, strOf(row.AdoptionFailReason), row.AdoptionFailCount)
	}
	if adoptionTotalFor(t, rigHost) != before || len(r.members.requested()) != 0 {
		t.Fatal("a too-large unsigned suite ran an adoption")
	}
}

// Spec C4 "Response classification": an unknown answer on Release.gpg (a
// 503) is a failed check — never "absent", so never an unsigned candidate —
// also with adoption.adopt_unsigned on. TestCheckFirst_ClassificationPerAnchor
// runs with the knob off, which turns a wrongly unsigned candidate into a
// failed check as well and hides the mix-up. (Proposed by the executability
// review; kills X01.)
func TestCheckFirst_UnknownSignatureIsNeverAnUnsignedCandidate(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopter.adoptUnsigned = true
	r.unsignedRelease("u1", &fetch.StatusError{Code: 503})
	before := checkTotal(t, "failed", rigHost)
	r.check()
	row := r.row()
	if row.CheckFailCount != 1 || row.CurrentSnapshotID != nil || row.AdoptionFailCount != 0 {
		t.Fatalf("row %+v, want a failed check: streak 1, no record, nothing adopted", row)
	}
	if got := checkTotal(t, "failed", rigHost) - before; got != 1 {
		t.Fatalf("failed checks counted %v, want 1", got)
	}
}

// Review Focus 2 / spec C3: a soft-404 (a 200 HTML page) on Release.gpg is
// absent, never taken for a signature: with Release present the tree finds
// an unsigned candidate, not the detached form. (Proposed by the
// executability review; kills X04.)
func TestCheckFirst_HTMLReleaseGPGIsNoSignature(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopter.adoptUnsigned = true
	r.unsignedRelease("u1", nil)
	r.up.setAt(rigReleaseGPGURL, []byte("<html><body>Not Found</body></html>\n"))
	r.check()
	row := r.row()
	if row.CurrentSnapshotID != nil || strOf(row.AdoptionFailReason) != cache.ReasonUnsignedUnconfirmed || r.ver.calls() != 0 {
		t.Fatalf("current %v, record %q, verifier calls %d: want the unsigned candidate's first observation, no detached attempt",
			row.CurrentSnapshotID, strOf(row.AdoptionFailReason), r.ver.calls())
	}
}

// Design note 14: a client-created Release.gpg row alone does not make a
// never-adopted suite active (apt fetches it only after Release): with its
// InRelease row reconstructed (request_count 0) the suite stays dormant.
// (Proposed by the executability review; kills X12.)
func TestCheckFirst_ReleaseGPGRowAloneIsNotClientInterest(t *testing.T) {
	r := newRetryRig(t, 0)
	r.up.set(r.release("v1", 0))
	ctx := context.Background()
	stale := testHex64
	sig, err := r.adopter.writeBlobBytes(ctx, armoredSig("a signature"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.cache.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: rigHost,
		Path: rigSuite + "/InRelease", BlobHash: &stale, UpstreamURL: rigAnchorURL, IsMetadata: true}); err != nil {
		t.Fatal(err)
	}
	if err := r.cache.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: rigHost,
		Path: rigSuite + "/Release.gpg", BlobHash: &sig, UpstreamURL: rigReleaseGPGURL, IsMetadata: true, RequestCount: 1}); err != nil {
		t.Fatal(err)
	}
	r.check()
	if n := len(r.up.etagsSent(rigAnchorURL)); n != 0 {
		t.Fatalf("%d requests for a suite whose only client-created anchor row is Release.gpg, want 0", n)
	}
}

// Design note 17 ("confirmed once"): bytes that passed the confirmation and
// then failed their adoption attempt are not confirmed again — at the
// record's retry time the attempt runs at once. (Proposed by the
// executability review; kills X13.)
func TestCheckFirst_ConfirmedUnsignedContentIsNotReconfirmed(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopter.adoptUnsigned = true
	r.members.put(rigPkgsURL, []byte("not the declared Packages"))
	r.unsignedRelease("u1", nil)
	r.check() // first observation: unsigned_unconfirmed
	r.clock.advance(6 * time.Minute)
	r.check() // confirmed; the attempt fails on the member
	row := r.row()
	if row.CurrentSnapshotID != nil || strOf(row.AdoptionFailReason) == cache.ReasonUnsignedUnconfirmed {
		t.Fatalf("setup: current %v record %q, want a failed attempt recorded", row.CurrentSnapshotID, strOf(row.AdoptionFailReason))
	}
	r.members.put(rigPkgsURL, r.pkgs) // the mirror synced
	r.clock.advance(time.Hour)        // past the attempt's retry time
	r.check()
	if snap := r.currentSnapshot(); snap == nil || snap.Form != cache.FormUnsigned {
		t.Fatalf("at the retry time: snapshot %+v, record %q; want the unsigned adoption at once",
			snap, strOf(r.row().AdoptionFailReason))
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -gcflags=-e ./internal/freshness/ -run 'TestClassify_Table|TestAnchorURLs|TestCheckFirst_|TestSuiteDue_DormantNeverAdoptedSuites|TestIsQueuedResult_FirstAdoption'`
Expected: FAIL — compile error naming `undefined: probeClass`, `classify`, `classUnknown`, `classPresent`, `classAbsent` and `anchorURLs` (the type checker stops at these; the other new names follow once they exist).

- [ ] **Step 3: Implement**

Create `internal/freshness/probe.go`:

```go
package freshness

// Spec C4 "Response classification", defined once and used by every anchor
// probe: present, absent or unknown.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
)

// probeClass is spec C4's response class of one anchor probe.
type probeClass int

const (
	// classUnknown: a transport error, any status other than those below
	// (408, 429, 5xx, other 4xx, an unfollowed 3xx), a body over its
	// limit, or the check deadline. No form decision; the check fails.
	classUnknown probeClass = iota
	// classPresent: a 200 whose body passes the file's sniff, or a 304 to
	// a request that carried validators (the cached anchor still stands).
	classPresent
	// classAbsent: 401, 403, 404, 410, or a 200 failing the sniff.
	classAbsent
)

func (p probeClass) String() string {
	switch p {
	case classPresent:
		return "present"
	case classAbsent:
		return "absent"
	}
	return "unknown"
}

// probeOutcome is one classified anchor probe.
type probeOutcome struct {
	class probeClass
	// res is the response of a present probe (status 200 or 304), and of a
	// 200 that failed its sniff; nil for an error.
	res *fetch.ConditionalResult
	// status is the upstream status (0 for a transport error, a deadline
	// or a cancelled caller).
	status int
	// err says what ended an unknown or error-status probe.
	err error
}

// cancelled reports a probe the caller's own context ended (shutdown):
// it says nothing about upstream, so the check writes nothing.
func (o probeOutcome) cancelled() bool { return errors.Is(o.err, context.Canceled) }

// authDenied reports a 401 or 403: on a signature file it stretches the
// unsigned confirmation gap to an hour (spec C5 safeguard 2).
func (o probeOutcome) authDenied() bool { return o.status == 401 || o.status == 403 }

// classify maps one fetch.Conditional result to its spec C4 class. sniff
// is the probed file's sniff; conditional reports whether the request
// carried validators — a 304 to a request without any is a protocol
// violation, unknown.
//
// AIDEV-NOTE: the one place a status becomes a class. 401/403/410 count as
// absent (more lenient than apt's 404-only signed fallback; for a signed
// form this can only cause a lateral move), so they never extend the
// check_fail_count streak (ruling R-E4). An unfollowed 3xx arrives as
// ErrRedirectBlocked wrapping a *fetch.StatusError with the 3xx code:
// unknown, like every code not listed.
func classify(res *fetch.ConditionalResult, err error, sniff func([]byte) bool, conditional bool) probeOutcome {
	if err != nil {
		var se *fetch.StatusError
		if !errors.As(err, &se) {
			return probeOutcome{class: classUnknown, err: err}
		}
		switch se.Code {
		case 401, 403, 404, 410:
			return probeOutcome{class: classAbsent, status: se.Code, err: err}
		}
		return probeOutcome{class: classUnknown, status: se.Code, err: err}
	}
	switch {
	case res.Status == 200 && sniff(res.Body):
		return probeOutcome{class: classPresent, res: res, status: 200}
	case res.Status == 200:
		return probeOutcome{class: classAbsent, res: res, status: 200}
	case res.Status == 304 && conditional:
		return probeOutcome{class: classPresent, res: res, status: 304}
	}
	return probeOutcome{class: classUnknown, status: res.Status, err: errUnexpectedStatus}
}

// anchorAbsentKey is the content key of an anchor_absent record: the
// absence has no content, and the constant can never equal a real content
// key, so whatever upstream serves next is new content. A never-adopted
// suite whose probe tree finds no anchor records it (ruling PE-2), and so
// does an adopted suite whose current anchor is absent (anchorAbsent).
var anchorAbsentKey = func() string {
	s := sha256.Sum256([]byte("anchor_absent"))
	return hex.EncodeToString(s[:])
}()

// Each anchor file's sniff (spec C3) for classify.
var (
	sniffInRelease  = aptpath.SniffInRelease
	sniffRelease    = aptpath.SniffRelease
	sniffReleaseGPG = aptpath.SniffReleaseGPG
)

// probe GETs the anchor file name ("InRelease", "Release", "Release.gpg")
// of suite without validators, under fetchCtx (the check deadline), and
// classifies the answer. The caller holds the host slot.
func (c *Checker) probe(fetchCtx context.Context, suite SuiteRef, urls anchorURLs, name string) probeOutcome {
	limit, sniff := c.maxBody, sniffInRelease
	switch name {
	case "Release":
		sniff = sniffRelease
	case "Release.gpg":
		limit, sniff = defaultMaxReleaseGPGBytes, sniffReleaseGPG
	}
	target := &fetch.Target{CanonicalHost: suite.CanonicalHost, URL: urls.of(name)}
	res, err := c.fetcher.Conditional(fetchCtx, target, "", "", limit)
	return classify(res, err, sniff, false)
}

// anchorURLs builds the upstream URLs of a suite's anchor files. A cached
// anchor row's stored upstream_url is the model — it keeps a non-default
// port or a mirror prefix that the canonical host and path do not carry —
// and a sibling is that URL with its last segment replaced; without one the
// checker's urlReconstruct builds the URL from the suite identity.
type anchorURLs struct {
	model       string // a cached anchor row's upstream_url, or ""
	modelName   string // that row's file name
	reconstruct func(name string) string
}

func (u anchorURLs) of(name string) string {
	if u.model != "" {
		if prefix, ok := strings.CutSuffix(u.model, "/"+u.modelName); ok {
			return prefix + "/" + name
		}
	}
	return u.reconstruct(name)
}

// anchorURLsFor returns the URL builder for suite, modelled on the first
// of rows (InRelease, Release, Release.gpg order) that is cached.
func (c *Checker) anchorURLsFor(suite SuiteRef, rows map[string]string) anchorURLs {
	u := anchorURLs{reconstruct: func(name string) string {
		return c.urlReconstruct(suite.CanonicalScheme, suite.CanonicalHost, suite.SuitePath+"/"+name)
	}}
	for _, name := range []string{"InRelease", "Release", "Release.gpg"} {
		if url := rows[name]; url != "" {
			u.model, u.modelName = url, name
			break
		}
	}
	return u
}
```

Create `internal/freshness/first_adoption.go`:

```go
package freshness

// Spec C4 Dispatch 3: eager first adoption. A suite with no current
// snapshot adopts on its first check, in whichever anchor form its
// upstream publishes, when a client has asked for it recently; a dormant
// one costs no upstream request.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

const (
	// interestWindow is spec C4's eager-adoption interest window (a C11
	// constant, independent of the GC settings); the v7 backfill
	// (cache.interestWindowSeconds) uses the same 7 days.
	interestWindow = 7 * 24 * time.Hour

	// minConfirmGap is the floor of the unsigned transition's and the
	// absent anchor's confirmation gap, max(cooldown, 5 min) (spec C4, C5).
	minConfirmGap = 5 * time.Minute

	// deniedConfirmGap is the unsigned confirmation gap when a signature
	// file answered 401 or 403 (WAF and S3-permission incidents, spec C5
	// safeguard 2).
	deniedConfirmGap = time.Hour
)

// errNoAnchor ends the probe tree of a suite whose upstream has neither
// InRelease nor Release: a failed check (spec C4 Dispatch 3; the anchorless
// refresh of flat suites is phase G's).
var errNoAnchor = errors.New("no anchor: InRelease and Release are absent")

// confirmGap is max(cooldown, 5 min).
func (c *Checker) confirmGap() time.Duration { return max(c.cooldown, minConfirmGap) }

// interested reports spec C4's client interest: last_requested_at within
// the 7-day interest window of now, in either direction. future reports a
// stamp beyond the clock-step tolerance ahead of now (a backward clock
// step), which the check reports.
//
// AIDEV-NOTE: the window is symmetric (design review M-3). A stamp up to
// 7 d ahead keeps the suites that were interested before a backward step
// of at most the window; one further ahead is dormant until a client
// touches the suite again (TouchSuite rewrites the stamp with now, within
// its coalescing hour). Reading any future stamp as interested would turn
// every suite touched during a year-ahead boot into a first adoption.
func interested(lastRequested *int64, now time.Time) (ok, future bool) {
	if lastRequested == nil {
		return false, false
	}
	last := time.Unix(*lastRequested, 0)
	d := now.Sub(last)
	if d < 0 {
		d = -d
	}
	return d < interestWindow, last.After(now.Add(clockStepTolerance))
}

// pacedFirst is ruling PE-3 (design review I-4): a never-adopted suite
// whose failure or refusal record is active and not due, and whose last
// check is younger than periodic_refresh, is not probed — a client check
// (T1) inside that window asks nothing. The scheduler's stale check (the
// last check at least periodic_refresh old) and the due retry still run
// the probe tree, so new content of a failing suite is attempted within
// one refresh interval. A last_check_at in the future reads as infinitely
// old (spec C4 clock-step rule: a gate that delays work never freezes on
// it); with periodic refresh off (0) nothing is paced.
//
// AIDEV-NOTE: the probe tree sends no validators (it must adopt an
// unchanged upstream, spec C4), so without this pacing every client check
// of a deferred suite downloaded its anchor in full — up to hundreds of
// MiB a day per suite for a missing vendor key.
func (c *Checker) pacedFirst(cur *cache.SuiteFreshness, gate retryGate, now time.Time) bool {
	if !gate.active || gate.due || cur.LastCheckAt == nil {
		return false
	}
	last := time.Unix(*cur.LastCheckAt, 0)
	return !last.After(now.Add(clockStepTolerance)) && now.Sub(last) < c.refresh
}

// cachedAnchors reads suite's cached anchor url_path rows: the upstream
// URL of each (by file name) and whether a client created one of the
// InRelease or Release rows (request_count > 0; a reconstructed row has
// none, spec C4 "never reconstructed").
func (c *Checker) cachedAnchors(ctx context.Context, suite SuiteRef) (urls map[string]string, clientCreated bool, err error) {
	urls = make(map[string]string, 3)
	for _, name := range []string{"InRelease", "Release", "Release.gpg"} {
		row, err := c.cache.LookupURL(ctx, suite.CanonicalScheme, suite.CanonicalHost, suite.SuitePath+"/"+name)
		switch {
		case errors.Is(err, cache.ErrNotFound):
			continue
		case err != nil:
			return nil, false, fmt.Errorf("read %s: %w", name, err)
		}
		urls[name] = row.UpstreamURL
		if name != "Release.gpg" && row.RequestCount > 0 {
			clientCreated = true
		}
	}
	return urls, clientCreated, nil
}

// probeTree is spec C4's probe tree, every probe without validators:
// InRelease present → inline; absent → Release: present → Release.gpg:
// present → detached, absent → unsigned. found is false when InRelease and
// Release are both absent; end is set when a probe ended the tree unknown
// (or the caller was cancelled), with the name of its file.
type probeTree struct {
	form                    adoptionForm
	found                   bool
	inRelease, release, sig probeOutcome
	end                     *probeOutcome
	endName                 string
}

func (c *Checker) runProbeTree(fetchCtx context.Context, suite SuiteRef, urls anchorURLs) probeTree {
	var t probeTree
	stop := func(o *probeOutcome, name string) probeTree {
		t.end, t.endName = o, name
		return t
	}
	t.inRelease = c.probe(fetchCtx, suite, urls, "InRelease")
	switch t.inRelease.class {
	case classPresent:
		t.form, t.found = adoptionFormInline, true
		return t
	case classUnknown:
		return stop(&t.inRelease, "InRelease")
	}
	t.release = c.probe(fetchCtx, suite, urls, "Release")
	switch t.release.class {
	case classAbsent:
		return t
	case classUnknown:
		return stop(&t.release, "Release")
	}
	t.sig = c.probe(fetchCtx, suite, urls, "Release.gpg")
	switch t.sig.class {
	case classPresent:
		t.form, t.found = adoptionFormDetached, true
	case classAbsent:
		t.form, t.found = adoptionFormUnsigned, true
	default:
		return stop(&t.sig, "Release.gpg")
	}
	return t
}

// request builds the adoption request for the form the tree found. The
// validators are the anchor's own (Release's for the detached and unsigned
// forms): the flip transaction stores them as the suite's (spec C4
// "Validators").
func (t probeTree) request(suite SuiteRef) *adoptionRequest {
	switch t.form {
	case adoptionFormDetached:
		rel, sig := t.release.res, t.sig.res
		return &adoptionRequest{suite: suite, form: adoptionFormDetached,
			key:          contentKey(adoptionFormDetached, nil, rel.Body, sig.Body),
			releaseBytes: rel.Body, sigBytes: sig.Body, etag: rel.ETag, lastmod: rel.LastModified}
	case adoptionFormUnsigned:
		rel := t.release.res
		return &adoptionRequest{suite: suite, form: adoptionFormUnsigned,
			key:          contentKey(adoptionFormUnsigned, nil, rel.Body, nil),
			releaseBytes: rel.Body, etag: rel.ETag, lastmod: rel.LastModified}
	}
	b := t.inRelease.res
	return &adoptionRequest{suite: suite, form: adoptionFormInline,
		key: contentKey(adoptionFormInline, b.Body, nil, nil), bytes: b.Body, etag: b.ETag, lastmod: b.LastModified}
}

// signatureDenied reports a 401/403 on either signature file probed.
func (t probeTree) signatureDenied() bool { return t.inRelease.authDenied() || t.sig.authDenied() }

// signatureSeen reports a signature file probed present: InRelease (its
// clearsign envelope passed the sniff) or Release.gpg.
func (t probeTree) signatureSeen() bool {
	return t.inRelease.class == classPresent || t.sig.class == classPresent
}

// checkFirst is spec C4 Dispatch 3 (adopter wired, no current snapshot).
// A suite without client interest in the last 7 days, or without a
// client-created InRelease or Release url_path row, is dormant: no request
// and no write (the scheduler does not queue it either, see suiteDue). A
// suite whose record is active and not due is paced (pacedFirst): no
// request between the scheduler's refresh checks. Otherwise the probe tree
// runs without validators and the form found is adopted — result
// first_adoption, or first_adoption_queued with no slot — through the spec
// C7 gate (content keys only: the probes carry no validators) and, for the
// unsigned form, spec C5's eligibility, member cap and transition
// confirmation (unsignedFirst). A probe that finds a signature marks the
// suite's base first (Adopter.markSignatureSeen; a failed mark fails the
// check). A probe that ends unknown, a tree that finds no anchor (which
// also records anchor_absent, so the status page shows it: ruling PE-2),
// and an unsigned candidate while adoption.adopt_unsigned is off are
// failed checks. A read error of the anchor rows is logged at WARN with no
// write and no backoff: a local fault, retried at the next check (at most
// one per cooldown and per tick).
//
// AIDEV-NOTE: a first or unsigned request passes signedCurrent = false to
// admit, so it never takes the last free slot that the reserved-slot rule
// keeps for suites with a signed current snapshot (spec C4). The
// freshness_check line of a tree result carries upstream_status 200, the
// status of the anchor the form was found by; the absent probes before it
// are not logged.
func (c *Checker) checkFirst(ctx, fetchCtx context.Context, out *checkOutcome, cur *cache.SuiteFreshness, suite SuiteRef, now time.Time, nowUnix int64) *adoptionRequest {
	host, suitePath := suite.CanonicalHost, suite.SuitePath
	ok, future := interested(cur.LastRequestedAt, now)
	if future {
		c.clockStep("last_requested_at", suite.CanonicalScheme, host, suitePath, *cur.LastRequestedAt, now)
	}
	if !ok {
		c.logger.Debug("freshness: never-adopted suite without recent interest, dormant",
			"canonical_host", host, "suite_path", suitePath)
		return nil
	}
	rows, clientCreated, err := c.cachedAnchors(ctx, suite)
	if err != nil {
		c.logger.Warn("freshness: read anchor rows failed",
			"err", err, "canonical_host", host, "suite_path", suitePath)
		return nil
	}
	if !clientCreated {
		c.logger.Debug("freshness: never-adopted suite without a client-created anchor row, dormant",
			"canonical_host", host, "suite_path", suitePath)
		return nil
	}

	gate := c.retryGateFor(cur, now)
	c.applyClamp(ctx, cur, gate, now)
	if c.pacedFirst(cur, gate, now) {
		c.logger.Debug("freshness: deferred never-adopted suite paced until its retry or the next refresh check",
			"canonical_host", host, "suite_path", suitePath, "reason", gate.reason)
		return nil
	}

	release, err := c.hostSem.Acquire(fetchCtx, host)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			c.logger.Debug("freshness: host limiter acquire aborted",
				"err", err, "canonical_host", host, "suite_path", suitePath)
			return nil
		}
		c.failCheck(ctx, out, cur, suite, nowUnix, "probe", err)
		return nil
	}
	defer release()

	t := c.runProbeTree(fetchCtx, suite, c.anchorURLsFor(suite, rows))
	switch {
	case t.end != nil && t.end.cancelled():
		c.logger.Debug("freshness: check cancelled", "canonical_host", host, "suite_path", suitePath)
		return nil
	case t.end != nil:
		c.failCheck(ctx, out, cur, suite, nowUnix, "probe", fmt.Errorf("%s: %w", t.endName, t.end.err))
		return nil
	case !t.found:
		// Ruling PE-2: still a failed check (spec C4; the streak backs
		// off), and the refusal state anchor_absent shows it on the status
		// page; the record also paces the suite (pacedFirst).
		c.recordAdoptionFailure(&adoptionRequest{suite: suite, key: anchorAbsentKey}, cache.ReasonAnchorAbsent)
		c.failCheck(ctx, out, cur, suite, nowUnix, "probe", errNoAnchor)
		return nil
	}
	if t.signatureSeen() {
		if err := c.adopter.markSignatureSeen(ctx, suite, nowUnix); err != nil {
			c.failCheck(ctx, out, cur, suite, nowUnix, "probe", fmt.Errorf("mark the signature seen: %w", err))
			return nil
		}
	}
	req := t.request(suite)
	// Spec C7: the same content as the record's, before its retry time.
	deferred, reason := gate.active && !gate.due && req.key == gate.hash, gate.reason
	if !deferred && t.form == adoptionFormUnsigned {
		proceed, refusal, err := c.unsignedFirst(ctx, cur, req, t, gate, now)
		if err != nil {
			c.failCheck(ctx, out, cur, suite, nowUnix, "unsigned", err)
			return nil
		}
		deferred, reason = !proceed, refusal
	}

	prevSuccess := cur.LastSuccessAt
	cur.LastCheckAt, cur.LastSuccessAt, cur.CheckFailCount = &nowUnix, &nowUnix, 0
	setChangeSeen(cur, nowUnix)
	e := checkEnd{form: formName(t.form), status: 200}
	if deferred {
		e.result, e.reason, req = "changed_deferred", reason, nil
	} else {
		req, e.result = c.admit(out, cur, req, false, prevSuccess, "first_adoption", "first_adoption_queued")
	}
	c.finishCheck(ctx, out, cur, suite, e)
	return req
}

// unsignedFirst applies spec C5's safeguards to an unsigned candidate of
// a never-adopted suite, before a slot is taken: the knob (off: an error,
// the check fails), never-downgrade (signed history, see
// Adopter.unsignedEligible: refused, recorded as
// unsigned_downgrade_refused), the member cap (unsigned_too_large; design
// review M-5: a too-large suite never takes a slot) and the transition
// confirmation — two observations of the same Release bytes at least
// confirmGap apart, an hour when a signature file answered 401/403
// (unsigned_unconfirmed until then; the streak, and with it
// adoption_fail_first_at, restarts whenever the bytes or the reason change,
// see recordAdoptionFailure). proceed is true for a confirmed candidate;
// otherwise reason is the refusal just recorded.
//
// AIDEV-NOTE: content that already passed confirmation (a record for the
// same bytes from an adoption attempt: any reason but unsigned_unconfirmed
// and unsigned_downgrade_refused) is not confirmed again, so a failing
// unsigned first adoption keeps the C7 schedule instead of restarting a
// short streak per attempt. A future adoption_fail_first_at never
// confirms (its age is negative) and re-recording restarts the streak
// (recordAdoptionFailure's past-event rule): a deviation from the C4
// clock-step rule, which would read it as infinitely old and skip the
// confirmation.
func (c *Checker) unsignedFirst(ctx context.Context, cur *cache.SuiteFreshness, req *adoptionRequest, t probeTree, gate retryGate, now time.Time) (proceed bool, reason string, err error) {
	switch err := c.adopter.unsignedEligible(ctx, req.suite); {
	case errors.Is(err, ErrAdoptionUnsignedDowngradeRefused):
		c.recordAdoptionFailure(req, cache.ReasonUnsignedDowngradeRefused)
		return false, cache.ReasonUnsignedDowngradeRefused, nil
	case err != nil:
		return false, "", err
	}
	if err := unsignedTooLarge(req.releaseBytes); err != nil {
		c.recordAdoptionFailure(req, cache.ReasonUnsignedTooLarge)
		return false, cache.ReasonUnsignedTooLarge, nil
	}
	sameContent := gate.active && gate.hash == req.key
	if sameContent && gate.reason != cache.ReasonUnsignedUnconfirmed && gate.reason != cache.ReasonUnsignedDowngradeRefused {
		return true, "", nil
	}
	gap := c.confirmGap()
	if t.signatureDenied() {
		gap = deniedConfirmGap
	}
	if sameContent && gate.reason == cache.ReasonUnsignedUnconfirmed && cur.AdoptionFailFirstAt != nil &&
		now.Sub(time.Unix(*cur.AdoptionFailFirstAt, 0)) >= gap {
		return true, "", nil
	}
	c.recordAdoptionFailure(req, cache.ReasonUnsignedUnconfirmed)
	return false, cache.ReasonUnsignedUnconfirmed, nil
}
```

Apply the dispatch, `isQueuedResult`, `suiteDue`, `recordAdoptionFailure`, `unsignedTooLarge`, `markSignatureSeen` and marker-note changes (`git apply`; Step 1 already applied the rig change, which is not in this patch):

```diff
diff --git a/internal/cache/signed_seen.go b/internal/cache/signed_seen.go
index f897a7d..1fe9ae9 100644
--- a/internal/cache/signed_seen.go
+++ b/internal/cache/signed_seen.go
@@ -30,7 +30,11 @@ type execer interface {
 // markCreated: SuiteSeed.SignedSeenAt and every cached signed anchor in
 // SuiteSeed.Cached, ruling PD-5), and only for an unproven creation the
-// handler's markCachedSignedAnchors backstop. It is written only for
-// suites with a suite_freshness row, and never for an unsigned snapshot or
-// a plain-text anchor.
+// handler's markCachedSignedAnchors backstop; from phase E the freshness
+// checker too, through MarkSignedSeenBatch, when its probe finds a
+// sniff-passing InRelease or Release.gpg for a suite without a signed
+// current snapshot (spec C3 revision 8, design review I-2), and the
+// adopter's re-mark before an unsigned commit (ruling R-E1). It is written
+// only for suites with a suite_freshness row, and never for an unsigned
+// snapshot or a plain-text anchor.
 func markSignedSeen(ctx context.Context, ex execer, scheme, host, base string, seenAt int64) error {
 	if _, err := ex.ExecContext(ctx, `
diff --git a/internal/freshness/adoption_failure.go b/internal/freshness/adoption_failure.go
index eb4a4b0..21987ef 100644
--- a/internal/freshness/adoption_failure.go
+++ b/internal/freshness/adoption_failure.go
@@ -61,4 +61,7 @@ func (c *Checker) recordAdoptionFailure(req *adoptionRequest, reason string) {
 		first = *cur.AdoptionFailFirstAt
 	}
+	if restartsStreak(cur, req.key, reason) {
+		count, first = 1, now.Unix()
+	}
 	delay := jitterDelay(backoffDelay(count), c.rand())
 	next := now.Add(delay).Unix()
@@ -101,4 +104,21 @@ func (c *Checker) recordAdoptionFailure(req *adoptionRequest, reason string) {
 }
 
+// restartsStreak reports a confirmation record (spec C4, C5) that starts
+// a new streak: unsigned_unconfirmed or anchor_absent recorded for other
+// content or after another reason. Their adoption_fail_first_at is the
+// first observation of THIS state and content, which the confirmation gap
+// is measured from ("two observations of the same Release bytes").
+//
+// AIDEV-NOTE: count restarts with first_at, so the retry schedule of a
+// confirmation is short (2 min, 4 min, …) and the gap, not the backoff,
+// decides when it confirms.
+func restartsStreak(cur *cache.SuiteFreshness, key, reason string) bool {
+	if reason != cache.ReasonUnsignedUnconfirmed && reason != cache.ReasonAnchorAbsent {
+		return false
+	}
+	return cur.AdoptionFailCount == 0 || cur.AdoptionFailReason == nil || *cur.AdoptionFailReason != reason ||
+		cur.AdoptionFailHash == nil || *cur.AdoptionFailHash != key
+}
+
 // refusalEscalateAge is how long a refusal streak lasts before it is
 // escalated: WARN adoption_refused_persistently at the streak milestones,
diff --git a/internal/freshness/check.go b/internal/freshness/check.go
index b0d8b5c..33c5c8f 100644
--- a/internal/freshness/check.go
+++ b/internal/freshness/check.go
@@ -124,7 +124,15 @@ func (c *Checker) checkLocked(ctx, fetchCtx context.Context, out *checkOutcome,
 	}
 
+	// Spec C4 Dispatch 3: with an adopter wired, a suite with no current
+	// snapshot runs the probe tree (or stays dormant).
+	if c.adopter != nil && cur.CurrentSnapshotID == nil {
+		out.req = c.checkFirst(ctx, fetchCtx, out, cur, suite, now, nowUnix)
+		return
+	}
+
 	// Form preference: if there's a current snapshot, the form is its
-	// anchor_form (snap.Form). Otherwise default to inline; the inline
-	// path triggers a detached fallback on 404.
+	// anchor_form (snap.Form). Otherwise (no adopter: the Phase 1
+	// observation) default to inline; the inline path triggers a detached
+	// fallback on 404.
 	form := c.detectForm(ctx, cur)
 	if form == adoptionFormDetached {
diff --git a/internal/freshness/freshness.go b/internal/freshness/freshness.go
index 8a6f099..27ae44d 100644
--- a/internal/freshness/freshness.go
+++ b/internal/freshness/freshness.go
@@ -389,7 +389,10 @@ const (
 )
 
-// isQueuedResult reports whether result means "new content, no slot".
-// Phase E adds first_adoption_queued here.
-func isQueuedResult(result string) bool { return result == "changed_queued" }
+// isQueuedResult reports whether result means "new content, no slot":
+// changed_queued, or first_adoption_queued for a suite with no current
+// snapshot (spec C4).
+func isQueuedResult(result string) bool {
+	return result == "changed_queued" || result == "first_adoption_queued"
+}
 
 // Check runs one freshness check of the suite. It is the entry point for
diff --git a/internal/freshness/scheduler.go b/internal/freshness/scheduler.go
index 54e777b..9108215 100644
--- a/internal/freshness/scheduler.go
+++ b/internal/freshness/scheduler.go
@@ -330,5 +330,6 @@ func (q *schedQueues) has(key string) bool {
 // last_check_at in the future is infinitely old. Without an adopter
 // nothing is ever attempted, so only staleness counts (Phase 1 parity,
-// mirroring retryGateFor).
+// mirroring retryGateFor); with one, a never-adopted suite is due only
+// while it has client interest (spec C4 Dispatch 3).
 func (c *Checker) suiteDue(s cache.SuiteFreshness, now time.Time) bool {
 	nowU := now.Unix()
@@ -343,4 +344,14 @@ func (c *Checker) suiteDue(s cache.SuiteFreshness, now time.Time) bool {
 		return false
 	}
+	// Spec C4 Dispatch 3: with an adopter wired, a never-adopted suite
+	// without client interest in the last 7 days is dormant and never
+	// queued (its check would make no request anyway). A stamp up to 7 days
+	// in the future counts as interested (interested: a symmetric window);
+	// checkFirst reports the clock step.
+	if c.adopter != nil && s.CurrentSnapshotID == nil {
+		if ok, _ := interested(s.LastRequestedAt, now); !ok {
+			return false
+		}
+	}
 	if s.CheckFailCount > 0 && s.LastCheckAt != nil && *s.LastCheckAt <= future {
 		if nowU < *s.LastCheckAt+int64(backoffDelay(s.CheckFailCount)/time.Second) {
diff --git a/internal/freshness/unsigned.go b/internal/freshness/unsigned.go
index 589bf35..013317a 100644
--- a/internal/freshness/unsigned.go
+++ b/internal/freshness/unsigned.go
@@ -87,4 +87,36 @@ func unsignedCap(n int) error {
 }
 
+// unsignedTooLarge is spec C5 safeguard 4 on an unsigned Release before a
+// slot is taken (design review M-5): unsignedCap over ParseRelease's merged
+// members. A Release that does not parse is not refused here: the adoption
+// reports the parse failure, and runShared applies the cap again.
+func unsignedTooLarge(release []byte) error {
+	members, err := ParseRelease(release)
+	if err != nil {
+		return nil
+	}
+	return unsignedCap(len(members))
+}
+
+// markSignatureSeen is the checker's signed-seen marker writer (spec C3,
+// revision 8; ruling PE-5, design review I-2): a probe found a sniff-passing
+// signature — InRelease, or Release.gpg — for a suite without a signed
+// current snapshot, so the cache has seen the suite signed even when no
+// client fetched the signature and no signed adoption commits. It marks
+// the suite's base (aptpath.MarkerBase, shared by the /. twin), stamped
+// seenAt, before the check admits any request; the caller fails the check
+// when it cannot.
+//
+// AIDEV-NOTE: it writes at every such probe, with no read first: the
+// marker keeps the newest stamp, its last sighting, which phase F's
+// orphan-marker sweep reads; the check's pacing bounds the writes. Without
+// this writer a signature only the checker ever saw (an unsigned suite's
+// clients never fetch one) could be stripped later and the suite adopted
+// unsigned.
+func (a *Adopter) markSignatureSeen(ctx context.Context, suite SuiteRef, seenAt int64) error {
+	return a.cache.MarkSignedSeenBatch(ctx, []cache.SignedSeenMark{{CanonicalScheme: suite.CanonicalScheme,
+		CanonicalHost: suite.CanonicalHost, Base: aptpath.MarkerBase(suite.SuitePath), SeenAt: seenAt}})
+}
+
 // errRemarkPoolRead marks a re-mark that failed reading a pool file: an I/O
 // fault, logged at ERROR (any other re-mark failure is a WARN).
```

- [ ] **Step 4: Change the existing expectations the new dispatch changes**

| Test | Was | Now | Why |
|---|---|---|---|
| `TestCheck_NoSlotQueuesWithoutAdvancingSuccess`, `TestCheck_GlobalSlotCapsConcurrentAdoptions`, `TestCheck_ReservedSlotGoesToSignedCurrent` (never-adopted leg) | `changed_queued` | `first_adoption_queued` | spec C4: a suite with no current snapshot reports the first-adoption results |
| `TestCheck_ReportsResultAndBypassesCooldown` | `changed_queued`, then `changed` | `first_adoption_queued`, then `first_adoption` | as above (the adopted legs keep `changed_queued`) |
| `TestCheck_DetachedFallback_OnMissingInReleaseURLRow` | `InRelease` never requested | requested once, answers 404 | the probe tree asks for `InRelease` first, with or without a cached row (the phase D fallback remains only without an adopter) |
| `TestPhase2Migration_V1ToV2EndToEnd` | waits for any flip | waits for the flip to B's `InRelease` | the first post-migration hit already adopts A (eager first adoption); B is the next adoption |

```diff
diff --git a/internal/freshness/capacity_test.go b/internal/freshness/capacity_test.go
index aabb9a5..d3ec4d5 100644
--- a/internal/freshness/capacity_test.go
+++ b/internal/freshness/capacity_test.go
@@ -37,6 +37,6 @@ func seedStaleAnchorAt(t *testing.T, c *cache.Cache, host, suite string) {
 
 // Spec C4: with no free slot the check spawns nothing, reports
-// changed_queued and leaves last_success_at alone so the scheduler
-// retries it on its next turn.
+// first_adoption_queued (the rig suite has no current snapshot) and leaves
+// last_success_at alone so the scheduler retries it on its next turn.
 func TestCheck_NoSlotQueuesWithoutAdvancingSuccess(t *testing.T) {
 	r := newRetryRig(t, 0)
@@ -44,6 +44,6 @@ func TestCheck_NoSlotQueuesWithoutAdvancingSuccess(t *testing.T) {
 	r.up.set(r.release("v1", 0))
 	r.check()
-	if r.ver.calls() != 0 || lastResult(r) != "changed_queued" {
-		t.Fatalf("calls %d result %q, want 0 changed_queued", r.ver.calls(), lastResult(r))
+	if r.ver.calls() != 0 || lastResult(r) != "first_adoption_queued" {
+		t.Fatalf("calls %d result %q, want 0 first_adoption_queued", r.ver.calls(), lastResult(r))
 	}
 	if row := r.row(); row.LastSuccessAt != nil || row.InReleaseChangeSeenAt == nil || row.LastCheckAt == nil {
@@ -133,6 +133,6 @@ func TestCheck_GlobalSlotCapsConcurrentAdoptions(t *testing.T) {
 	// B's refusal is read from its result and the slot count, not
 	// inferred from a timing window (deferred minor B-29).
-	if rep := chk.check(ctx, SuiteRef{CanonicalScheme: "http", CanonicalHost: hosts[1], SuitePath: rigSuite}, checkOpts{}); rep.result != "changed_queued" {
-		t.Fatalf("B's check = %q, want changed_queued while the only global slot is held", rep.result)
+	if rep := chk.check(ctx, SuiteRef{CanonicalScheme: "http", CanonicalHost: hosts[1], SuitePath: rigSuite}, checkOpts{}); rep.result != "first_adoption_queued" {
+		t.Fatalf("B's check = %q, want first_adoption_queued while the only global slot is held", rep.result)
 	}
 	if g, _ := chk.admission.usage(); g != 1 {
@@ -302,6 +302,6 @@ func TestCheck_ReservedSlotGoesToSignedCurrent(t *testing.T) {
 	r.up.set(r.release("v1", 0))
 	r.check()
-	if r.ver.calls() != 0 || lastResult(r) != "changed_queued" {
-		t.Fatalf("never-adopted suite: calls %d result %q, want 0 changed_queued (the last slot is reserved)", r.ver.calls(), lastResult(r))
+	if r.ver.calls() != 0 || lastResult(r) != "first_adoption_queued" {
+		t.Fatalf("never-adopted suite: calls %d result %q, want 0 first_adoption_queued (the last slot is reserved)", r.ver.calls(), lastResult(r))
 	}
 	other.release()
diff --git a/internal/freshness/freshness_test.go b/internal/freshness/freshness_test.go
index c9211bc..3cddaf4 100644
--- a/internal/freshness/freshness_test.go
+++ b/internal/freshness/freshness_test.go
@@ -1670,6 +1670,8 @@ func TestCheck_DetachedFallback_OnMissingInReleaseURLRow(t *testing.T) {
 		switch r.URL.Path {
 		case "/dists/noble/InRelease":
+			// Spec C4 Dispatch 3: the probe tree asks for InRelease
+			// first, with or without a cached row.
 			inReleaseHits.Add(1)
-			t.Errorf("InRelease fetched despite missing url_path row — fallback should not issue a GET")
+			w.WriteHeader(404)
 		case "/dists/noble/Release":
 			releaseHits.Add(1)
@@ -1737,6 +1739,6 @@ func TestCheck_DetachedFallback_OnMissingInReleaseURLRow(t *testing.T) {
 	c.WaitForAdoptions()
 
-	if inReleaseHits.Load() != 0 {
-		t.Errorf("InRelease hits = %d, want 0", inReleaseHits.Load())
+	if inReleaseHits.Load() != 1 {
+		t.Errorf("InRelease hits = %d, want 1 (the probe tree's absent InRelease)", inReleaseHits.Load())
 	}
 	if releaseHits.Load() != 1 {
diff --git a/internal/freshness/reoffer_test.go b/internal/freshness/reoffer_test.go
index c949829..ba03681 100644
--- a/internal/freshness/reoffer_test.go
+++ b/internal/freshness/reoffer_test.go
@@ -56,6 +56,6 @@ func TestCheck_ReportsResultAndBypassesCooldown(t *testing.T) {
 	held := mustAcquire(t, r.checker.admission, rigHost, true)
 	r.up.set(r.release("v1", 0))
-	if rep := r.checker.check(ctx, ref, checkOpts{}); rep.result != "changed_queued" || rep.signedCurrent {
-		t.Fatalf("no slot: %+v, want changed_queued for a suite with no signed snapshot", rep)
+	if rep := r.checker.check(ctx, ref, checkOpts{}); rep.result != "first_adoption_queued" || rep.signedCurrent {
+		t.Fatalf("no slot: %+v, want first_adoption_queued for a suite with no current snapshot", rep)
 	}
 	if rep := r.checker.check(ctx, ref, checkOpts{}); rep.result != resultSkipped {
@@ -63,6 +63,6 @@ func TestCheck_ReportsResultAndBypassesCooldown(t *testing.T) {
 	}
 	held.release()
-	if rep := r.checker.check(ctx, ref, checkOpts{bypassCooldown: true}); rep.result != "changed" {
-		t.Fatalf("bypassed cooldown with a free slot: result %q, want changed", rep.result)
+	if rep := r.checker.check(ctx, ref, checkOpts{bypassCooldown: true}); rep.result != "first_adoption" {
+		t.Fatalf("bypassed cooldown with a free slot: result %q, want first_adoption", rep.result)
 	}
 	r.checker.WaitForAdoptions()
diff --git a/internal/handler/phase2_migration_test.go b/internal/handler/phase2_migration_test.go
index c9ff09f..016d789 100644
--- a/internal/handler/phase2_migration_test.go
+++ b/internal/handler/phase2_migration_test.go
@@ -3,5 +3,7 @@ package handler
 import (
 	"context"
+	"crypto/sha256"
 	"database/sql"
+	"encoding/hex"
 	"fmt"
 	"net/http"
@@ -172,5 +174,5 @@ func TestPhase2Migration_V1ToV2EndToEnd(t *testing.T) {
 			got)
 	}
-	if err := waitForMigrationFlip(t, stack, upstreamURL, srv.URL, 15*time.Second); err != nil {
+	if err := waitForMigrationFlip(t, stack, upstreamURL, srv.URL, snapB.inRelease, 15*time.Second); err != nil {
 		t.Fatalf("adoption flip never happened: %v", err)
 	}
@@ -459,6 +461,10 @@ func newPhase2MigrationStack(t *testing.T, c *cache.Cache, upstream *url.URL) *p
 // CurrentSnapshotID = NULL (never adopted), so any non-NULL value
 // indicates the first adoption has flipped.
-func waitForMigrationFlip(t *testing.T, stack *phase2ChaosStack, upstreamURL *url.URL, srvURL string, timeout time.Duration) error {
+// The first post-migration hit already adopts A (spec C4 eager first
+// adoption), so it waits for the snapshot whose InRelease is want.
+func waitForMigrationFlip(t *testing.T, stack *phase2ChaosStack, upstreamURL *url.URL, srvURL string, want []byte, timeout time.Duration) error {
 	t.Helper()
+	sum := sha256.Sum256(want)
+	wantHash := hex.EncodeToString(sum[:])
 	deadline := time.Now().Add(timeout)
 	for time.Now().Before(deadline) {
@@ -466,5 +472,8 @@ func waitForMigrationFlip(t *testing.T, stack *phase2ChaosStack, upstreamURL *ur
 			"http", upstreamURL.Hostname(), chaos2Suite)
 		if err == nil && suite != nil && suite.CurrentSnapshotID != nil {
-			return nil
+			snap, err := stack.handler.cache.GetSuiteSnapshot(context.Background(), *suite.CurrentSnapshotID)
+			if err == nil && snap.SingleAnchorHash != nil && *snap.SingleAnchorHash == wantHash {
+				return nil
+			}
 		}
 		rec := httptest.NewRecorder()
@@ -472,5 +481,5 @@ func waitForMigrationFlip(t *testing.T, stack *phase2ChaosStack, upstreamURL *ur
 		time.Sleep(20 * time.Millisecond)
 	}
-	return fmt.Errorf("current_snapshot_id still NULL after %v", timeout)
+	return fmt.Errorf("no current snapshot of the wanted InRelease after %v", timeout)
 }
 
```

- [ ] **Step 5: Run the tests**

Run: `go test -race ./internal/freshness/ ./internal/handler/ ./internal/cache/ ./cmd/...`
Expected: PASS.

- [ ] **Step 6: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| `classify`: 410 not absent | `TestClassify_Table`, `TestCheckFirst_ClassificationPerAnchor/absent_410…` |
| `classify`: a 304 counts as present without validators | `TestClassify_Table` |
| `interested`: `<` → `<=` (7 days is still interested) | `TestCheckFirst_DormantSuitesMakeNoRequest/interest_exactly_7_days_old` |
| `interested`: a future stamp is not interested (`d = interestWindow` for `d < 0`) | `TestCheckFirst_DormantSuitesMakeNoRequest/interest_in_the_future`, `TestSuiteDue_DormantNeverAdoptedSuites` |
| `interested`: any future stamp is interested (`d = 0` for `d < 0`; design review M-3) | `TestCheckFirst_DormantSuitesMakeNoRequest/interest_8_days_in_the_future`, `TestSuiteDue_DormantNeverAdoptedSuites` |
| `cachedAnchors`: `request_count` ignored | `TestCheckFirst_DormantSuitesMakeNoRequest/a_reconstructed_anchor_row…`, `TestCheckFirst_ReleaseGPGRowAloneIsNotClientInterest` |
| `cachedAnchors`: a `Release.gpg` row counts as client-created (X12) | `TestCheckFirst_ReleaseGPGRowAloneIsNotClientInterest` |
| `suiteDue`: the dormancy clause removed | `TestSuiteDue_DormantNeverAdoptedSuites` |
| `checkLocked`: Dispatch 3 never taken (the phase D path) | `TestCheckFirst_AdoptsUnchangedUpstreamOnTheFirstCheck`, `…ProbeTreeAdoptsTheFormFound`, `…ClassificationPerAnchor`, `…DormantSuitesMakeNoRequest` |
| `runProbeTree`: an unknown `InRelease` treated as absent | `TestCheckFirst_ClassificationPerAnchor/unknown_…_at_…InRelease` |
| `runProbeTree`: an unknown `Release.gpg` → an unsigned candidate (X01) | `TestCheckFirst_UnknownSignatureIsNeverAnUnsignedCandidate` |
| `probe`: the `Release.gpg` sniff always passes (X04) | `TestCheckFirst_HTMLReleaseGPGIsNoSignature` |
| `checkFirst`: no `anchor_absent` record when the tree finds no anchor (PE-2) | `TestCheckFirst_ClassificationPerAnchor/no_anchor_at_all` |
| `pacedFirst` always false (PE-3) | `TestCheckFirst_DeferredSuiteIgnoresClientChecksUntilItsRetry`, `TestCheckFirst_UnsignedTransitionIsConfirmed` |
| `pacedFirst` ignores the retry being due | `TestCheckFirst_DeferredSuiteIgnoresClientChecksUntilItsRetry`, `TestCheckFirst_UnsignedTransitionIsConfirmed`, `TestCheckFirst_ConfirmedUnsignedContentIsNotReconfirmed` |
| `pacedFirst` ignores the refresh age (paced whenever not due) | `TestCheckFirst_DeferredSuiteIgnoresClientChecksUntilItsRetry`, `TestCheckFirst_GateComparesContentKeys` |
| `pacedFirst` reads a future last check as recent | `TestCheckFirst_DeferredSuiteIgnoresClientChecksUntilItsRetry` |
| `checkFirst`: no marker for a probed signature (I-2) | `TestCheckFirst_ProbedSignatureMarksTheBase/{inline, detached, failed_marker_write}` |
| `signatureSeen` ignores `Release.gpg` | `TestCheckFirst_ProbedSignatureMarksTheBase/detached` |
| `checkFirst`: a failed marker write ignored | `TestCheckFirst_ProbedSignatureMarksTheBase/failed_marker_write` |
| `unsignedFirst`: the gap compared with 0 (`>= gap-gap`) | `TestCheckFirst_UnsignedTransitionIsConfirmed`, `TestCheckFirst_UnsignedAfterADeniedSignatureWaitsAnHour` |
| `unsignedFirst`: the 401/403 gap not applied | `TestCheckFirst_UnsignedAfterADeniedSignatureWaitsAnHour` |
| `unsignedFirst`: no eligibility check | `TestCheckFirst_UnsignedRefusedWithSignedHistory`, `TestCheckFirst_UnsignedDisabledIsAFailedCheck`, `TestCheckFirst_ProbedSignatureMarksTheBase/{inline, detached}` |
| `unsignedFirst`: no member cap before the slot (M-5) | `TestCheckFirst_UnsignedTooLargeNeverTakesASlot` |
| `unsignedFirst`: confirmed content confirmed again (X13) | `TestCheckFirst_ConfirmedUnsignedContentIsNotReconfirmed` |
| `recordAdoptionFailure`: `restartsStreak` never true | `TestCheckFirst_UnsignedTransitionIsConfirmed` |
| `isQueuedResult` without `first_adoption_queued` | `TestIsQueuedResult_FirstAdoption`, `TestRun_ReoffersWhenASlotFrees`, `TestRun_OfferThatLosesTheSlotKeepsItsPlace`, `TestRun_HeldOfferIsRetriedOnTheNextWake`, `TestRun_ClientRefusedSuiteIsParkedByTheNextTick` |
| `checkFirst`: `admit(…, signedCurrent = true, …)` | `TestCheck_ReservedSlotGoesToSignedCurrent` |

- [ ] **Step 7: Commit**

```bash
gofmt -l internal/freshness/probe.go internal/freshness/first_adoption.go internal/freshness/first_adoption_test.go \
  internal/freshness/check.go internal/freshness/freshness.go internal/freshness/scheduler.go internal/freshness/adoption_failure.go \
  internal/freshness/unsigned.go internal/cache/signed_seen.go \
  internal/freshness/retry_rig_test.go internal/freshness/capacity_test.go internal/freshness/reoffer_test.go \
  internal/freshness/freshness_test.go internal/handler/phase2_migration_test.go
git add internal/freshness/probe.go internal/freshness/first_adoption.go internal/freshness/first_adoption_test.go \
  internal/freshness/check.go internal/freshness/freshness.go internal/freshness/scheduler.go internal/freshness/adoption_failure.go \
  internal/freshness/unsigned.go internal/cache/signed_seen.go \
  internal/freshness/retry_rig_test.go internal/freshness/capacity_test.go internal/freshness/reoffer_test.go \
  internal/freshness/freshness_test.go internal/handler/phase2_migration_test.go
git commit -m "Adopt never-adopted suites with client interest on their first check, in the form their upstream publishes"
```

### Task 7: Form transitions of adopted suites — lateral moves, `anchor_absent`, the unsigned upgrade, and `signedCurrent` for repair and reconcile

Spec C4 Dispatch 1 and 2 with an adopter wired (Phase 1 keeps the Task 4 algorithm):

- **Detached current:** `InRelease` is probed on every check (`lateralInline`); present → an inline request, result `form_changed` (`changed_queued` without a slot). An absent or unknown `InRelease`, or the failing content before its retry time, leaves the check to the detached path, whose "unchanged" then does not clear the lateral content's record (otherwise every other check would attempt it again; spec C7's bounded attempts).
- **Cross-form requests record no validators (design review I-1):** a lateral move, an unsigned suite's upgrade and a tree's request of another form fetch another anchor file than the one the check conditional-GETs, so `adoptionRequest.crossForm` keeps their validators out of the failure record (a successful adoption still stores them). While a record without validators is active and not due, the GET carries the current anchor's own validators, and a 304 is `changed_deferred` only when the record's own were sent (`retryGate.recordValidators`), `not_modified` otherwise. Before, a detached suite behind a CDN that sends `Last-Modified` but no ETag sent its failing lateral `InRelease`'s stamp on `Release`, got 304 for a new `Release` of the same publish, and froze (`TestCheck_DetachedSuiteKeepsAdoptingWhileItsLateralInReleaseFails`, with the rig's new `setLastModified` mode).
- **Current anchor absent** (inline `InRelease`, detached `Release`, unsigned `Release`: 401/403/404/410 or a 200 failing the sniff, through `classify`): `anchorAbsent`. The first observation records the refusal state `anchor_absent` (deviation 1: recorded at once, so it is visible and survives restarts); at the record's retry time, once the absence has lasted `max(cooldown, 5 min)`, the probe tree runs without validators (`anchorFoundAgain`): the other signed form found is adopted (`form_changed`); the same form and content found again is "unchanged"; an unsigned suite's own form found again with new bytes is its update (design review M-16: result `changed`, after the unsigned refusals); an unsigned candidate of a signed suite, or nothing, is recorded again — never unsigned from signed. A record from an adoption attempt (the lateral move failed) keeps its own schedule. `last_success_at` is not bumped (spec C4) and the streak resets (ruling R-E4). A future `adoption_fail_first_at` never confirms the absence (deviation 2, design review M-19: `TestCheck_AbsentAnchorWithAFutureFirstAtWaitsAnotherGap`).
- **`anchor_absent` is paced by its record (ruling PE-2):** while the record is active and not due, `checkLocked` makes no request (`pacedAbsent`; client checks ask nothing) and `suiteDue` does not queue the suite, so an absent anchor costs one request per retry (2 min doubling to 1 h, about 24 a day after the first hour), not one per tick and per client check forever; a `next_retry_at` beyond the legitimate horizon is not paced, so the check clamps it. The stale-suite WARN leaves out adopted suites in a refusal state (they keep an old `last_success_at` on purpose and are visible on their own); the gauge still counts them (design review M-13).
- **Detached current, signature absent (design review M-14):** a changed `Release` whose `Release.gpg` is absent (404, or a 200 failing its sniff) records `anchor_absent` (unless the lateral `InRelease`'s record is held), keeps `last_success_at`, and is paced like any `anchor_absent` suite — before, it cost three requests and a WARN per tick, forever. A signature fetch that ends unknown keeps the old behaviour (WARN, result `changed`, retried at the next tick).
- **Unsigned current** (`checkUnsigned`, Dispatch 2): `InRelease` present → inline request; else `Release.gpg` present → `Release` and a detached request — an upgrade, `form_changed`; either signature found first marks the suite's base (`Adopter.markSignatureSeen`, design review I-2: the suite's clients never fetch a signature, so only this probe can record that the suite was seen signed; a failed mark fails the check); an unknown signature probe fails the check (a signature may be there). Both absent → the conditional GET of `Release`; changed → an unsigned request at once (spec C5 safeguard 2: no re-confirmation) after the unsigned refusals before a slot (`unsignedRefusal`, shared with Task 6's `unsignedFirst`: signed history → `unsigned_downgrade_refused`; more than 1,000 members → `unsigned_too_large`, design review M-5); the knob off is a failed check. A reaped `Release` anchor is reconstructed through the form (`TestCheck_UnsignedRecoversWhenReleaseAnchorMissing`, spec §7). The absent-anchor tree marks the base too when it finds a signature for an unsigned suite.
- **`detectForm`** dispatches the unsigned form; **repair and reconcile** take the reserved slot only for a signed current snapshot (`repairRequest.signed`, set by `finishCheck`; `Reconcile` reads the snapshot's form), the overview's "E:" carry-forward.

Deferred minors (`freshness.go`): B-30 (`adoption_reconcile_failed` gains `stage`: `decision`, `heal`, `on_demand`), B-31 (a repair deferred for want of a slot is counted, `acu_repair_deferred_total{host}`), B-32 (weighed: a signed suite's repair keeps the reserved-slot right; AIDEV-NOTE at `planRepair`), B-37 (kept: a deterministic repair panic is logged, not recorded — no record keyed on a snapshot exists in v7; AIDEV-NOTE at `spawnRepair`).

**Files:**
- Create: `internal/freshness/current_form.go`, `internal/freshness/current_form_test.go`
- Modify: `internal/freshness/check.go` (`checkLocked`: the dispatch and `anchor_absent` pacing; `detectForm`, `finishCheck`, `steadyResult`, `checkInline`, `checkDetached`), `internal/freshness/freshness.go` (`adoptionRequest.crossForm`, `retryGate.recordValidators`/`validators`, `repairRequest.signed`, `planRepair`, `spawnRepair`, `Reconcile`), `internal/freshness/adoption_failure.go` (no validators for a cross-form record), `internal/freshness/first_adoption.go` (`unsignedRefusal`, used by `unsignedFirst`), `internal/freshness/scheduler.go` (`suiteDue`'s `anchor_absent` pacing, `staleSuiteCounts` and the stale WARN), `internal/freshness/metrics.go` (`repairDeferredTotal`), `internal/freshness/retry_rig_test.go` (`rigUpstream.setLastModified`); expectations in `internal/freshness/freshness_test.go`, `internal/freshness/capacity_test.go`

**Interfaces:**
- Consumes: Task 6's `classify`, `classAbsent`, `probe`, `anchorURLsFor`, `cachedAnchors`, `runProbeTree`, `probeTree.request`, `probeTree.signatureSeen`, `confirmGap`, `restartsStreak` (via `recordAdoptionFailure`), `anchorAbsentKey`, `Adopter.markSignatureSeen`, `unsignedTooLarge`, `rigUpstream.setFail`, `retryRig.currentSnapshot`, `retryRig.unsignedRelease`; Task 5's `retryRig.adopt`, `adoptDetached`, `asInRelease`; Task 4's `admit`, `baseline`, `steadyResult`; Task 3's `Adopter.RunUnsigned`, `unsignedEligible`, `formOfSnapshot`; Task 1's `aptpath.AnchorPaths`, `SuiteSnapshot.AnchorHash`.
- Produces (package freshness): `pacedAbsent(g retryGate) bool`; `adoptionRequest.crossForm`; `(retryGate).recordValidators() bool`; `(*Checker).unsignedRefusal(ctx, req) (string, error)`; `(probeTree).body(name string) []byte`, `(probeTree).sameAs(snap *cache.SuiteSnapshot) bool`; `(*Checker).anchorAbsent(ctx, fetchCtx, out, cur, suite, now, nowUnix, form adoptionForm, o probeOutcome, gate retryGate, signedCurrent bool) *adoptionRequest`; `(*Checker).anchorFoundAgain(ctx, out, cur, suite, nowUnix, form adoptionForm, t probeTree, signedCurrent bool) *adoptionRequest`; `(*Checker).lateralInline(ctx, fetchCtx, out, cur, suite, urls anchorURLs, gate retryGate, signedCurrent bool, nowUnix int64) (req *adoptionRequest, done, held bool)`; `(*Checker).upgradeProbe(ctx, fetchCtx, out, cur, suite, urls, gate, nowUnix) (req *adoptionRequest, done bool)`; `(*Checker).checkUnsigned(ctx, fetchCtx, out, cur, suite, now, nowUnix) (*adoptionRequest, *repairRequest)`; `repairRequest.signed`; metric `acu_repair_deferred_total{host}` (`repairDeferredTotal`); `staleSuiteCounts` returns `(adopted, unadopted, refused int)`. Test helpers: `(*rigUpstream).setLastModified(url, stamp string)` (`retry_rig_test.go`); `(*retryRig).adoptUnsigned(label string) []byte`, `hostCounter(t, name, host string) float64` (`current_form_test.go`).

- [ ] **Step 1: Write the failing tests**

Give the rig upstream a `Last-Modified`-only mode (`internal/freshness/retry_rig_test.go`, `git apply`):

```diff
diff --git a/internal/freshness/retry_rig_test.go b/internal/freshness/retry_rig_test.go
index 6d91334..f8e16e6 100644
--- a/internal/freshness/retry_rig_test.go
+++ b/internal/freshness/retry_rig_test.go
@@ -41,6 +41,9 @@ func rigETag(body []byte) string { return "e-" + hashOf(body)[:12] }
 // rigUpstream answers the checker's conditional GETs by URL: 200 with the
 // body and its ETag, or 304 when If-None-Match equals that ETag (unless
-// validators are ignored). A URL without a body answers 404. While
-// stalled, every request blocks until its context ends.
+// validators are ignored). A URL given a Last-Modified stamp
+// (setLastModified) answers like a CDN that sends no ETag: 200 with the
+// stamp, or 304 when If-Modified-Since is at or after it. A URL without a
+// body answers 404. While stalled, every request blocks until its context
+// ends.
 type rigUpstream struct {
 	mu               sync.Mutex
@@ -49,7 +52,19 @@ type rigUpstream struct {
 	stall            bool
 	fail             map[string]error    // per URL: the error Conditional returns (setFail)
+	modified         map[string]string   // per URL: Last-Modified, sortable (setLastModified)
 	sent             map[string][]string // If-None-Match per URL, in order
 }
 
+// setLastModified makes url answer with Last-Modified stamp and no ETag
+// (stamps compare as strings: use one sortable format).
+func (u *rigUpstream) setLastModified(url, stamp string) {
+	u.mu.Lock()
+	defer u.mu.Unlock()
+	if u.modified == nil {
+		u.modified = map[string]string{}
+	}
+	u.modified[url] = stamp
+}
+
 func (u *rigUpstream) set(body []byte) { u.setAt(rigAnchorURL, body) }
 
@@ -81,9 +96,9 @@ func (u *rigUpstream) etagsSent(url string) []string {
 }
 
-func (u *rigUpstream) Conditional(ctx context.Context, target *fetch.Target, etag, _ string, _ int64) (*fetch.ConditionalResult, error) {
+func (u *rigUpstream) Conditional(ctx context.Context, target *fetch.Target, etag, lastmod string, _ int64) (*fetch.ConditionalResult, error) {
 	u.mu.Lock()
 	u.sent[target.URL] = append(u.sent[target.URL], etag)
 	body, ok := u.bodies[target.URL]
-	stall, ignore, failErr := u.stall, u.ignoreValidators, u.fail[target.URL]
+	stall, ignore, failErr, stamp := u.stall, u.ignoreValidators, u.fail[target.URL], u.modified[target.URL]
 	u.mu.Unlock()
 	if stall {
@@ -97,4 +112,10 @@ func (u *rigUpstream) Conditional(ctx context.Context, target *fetch.Target, eta
 		return nil, &fetch.StatusError{Code: 404}
 	}
+	if stamp != "" {
+		if !ignore && lastmod != "" && lastmod >= stamp {
+			return &fetch.ConditionalResult{Status: 304, LastModified: stamp}, nil
+		}
+		return &fetch.ConditionalResult{Status: 200, Body: body, LastModified: stamp}, nil
+	}
 	tag := rigETag(body)
 	if !ignore && etag != "" && etag == tag {
```

Create `internal/freshness/current_form_test.go`:

```go
package freshness

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// adoptUnsigned makes the rig suite's current snapshot an unsigned
// Release labelled label (adopted directly; the check-side confirmation
// is TestCheckFirst_UnsignedTransitionIsConfirmed's) and serves it with
// no signature files.
func (r *retryRig) adoptUnsigned(label string) []byte {
	r.t.Helper()
	r.adopter.adoptUnsigned = true
	rel := r.unsignedRelease(label, nil)
	if err := r.adopter.RunUnsigned(context.Background(), SuiteRef{CanonicalScheme: "http", CanonicalHost: rigHost,
		SuitePath: rigSuite}, rel, "", ""); err != nil {
		r.t.Fatal(err)
	}
	if snap := r.currentSnapshot(); snap == nil || snap.Form != cache.FormUnsigned {
		r.t.Fatalf("rig: unsigned %s not adopted: %+v", label, snap)
	}
	return rel
}

// Spec C4 Dispatch 1 (lateral): a detached suite probes InRelease on every
// check and moves to inline as soon as one is served.
func TestCheck_DetachedSuiteMovesToInlineWhenInReleaseAppears(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adoptDetached()
	r.check() // InRelease absent: the detached check stands
	if lastResult(r) != "not_modified" && lastResult(r) != "unchanged" {
		t.Fatalf("result %q, want the detached suite unchanged", lastResult(r))
	}
	if n := len(r.up.etagsSent(rigAnchorURL)); n != 2 {
		t.Fatalf("InRelease probed %d times over two checks, want once per check", n)
	}
	in := r.release("v2", 0)
	r.up.set(in)
	r.check()
	snap := r.currentSnapshot()
	if lastResult(r) != "form_changed" || snap.Form != cache.FormInline || strOf(snap.SingleAnchorHash) != hashOf(in) {
		t.Fatalf("result %q snapshot %+v, want form_changed to the inline v2", lastResult(r), snap)
	}
}

// Spec C7 in the lateral probe: an InRelease that failed stays deferred
// until its retry time, and the detached anchor's "unchanged" does not
// clear its record (otherwise every other check would attempt it). The
// record holds no validators (a cross-form request), so the detached GET
// carries the current Release's own, and its 304 is not_modified (design
// review I-1).
func TestCheck_LateralFailingContentKeepsItsRecord(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adoptDetached()
	bad := r.release("bad", 0)
	r.ver.reject(hashOf(bad))
	r.up.set(bad)
	r.check()
	if r.ver.calls() != 1 || r.row().AdoptionFailCount != 1 {
		t.Fatalf("setup: calls %d count %d", r.ver.calls(), r.row().AdoptionFailCount)
	}
	for i := 0; i < 3; i++ {
		r.clock.advance(20 * time.Second)
		r.check()
	}
	if r.ver.calls() != 1 || r.row().AdoptionFailCount != 1 {
		t.Fatalf("before the retry time: calls %d count %d, want 1 and the record kept", r.ver.calls(), r.row().AdoptionFailCount)
	}
	if lastResult(r) != "not_modified" {
		t.Fatalf("result %q, want not_modified: the detached anchor answered its own validators", lastResult(r))
	}
	r.clock.advance(2 * time.Minute)
	r.check()
	if r.ver.calls() != 2 {
		t.Fatalf("at the retry time: calls %d, want 2", r.ver.calls())
	}
}

// Spec C4 Dispatch 1: an absent current anchor is confirmed by two
// observations at least max(cooldown, 5 min) apart, then the probe tree
// adopts the other signed form (lateral). Each absent observation is an
// answer: the streak stays 0 (ruling R-E4), last_success_at stays old and
// the state is visible as anchor_absent.
func TestCheck_AbsentAnchorMovesLaterallyAfterTwoObservations(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopt()
	adopted := r.row().LastSuccessAt
	r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 503}) // an unknown end first: streak 1
	r.clock.advance(time.Minute)
	r.check()
	if r.row().CheckFailCount != 1 {
		t.Fatalf("setup: streak %d, want 1", r.row().CheckFailCount)
	}
	r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 404})
	r.up.setAt(rigReleaseURL, r.releaseText("v2", 0))
	r.up.setAt(rigReleaseGPGURL, armoredSig("v2 sig"))
	r.clock.advance(time.Minute)
	r.check()
	row := r.row()
	if lastResult(r) != "anchor_absent" || strOf(row.AdoptionFailReason) != cache.ReasonAnchorAbsent || row.AdoptionFailCount != 1 {
		t.Fatalf("first observation: result %q record %q/%d", lastResult(r), strOf(row.AdoptionFailReason), row.AdoptionFailCount)
	}
	if row.CheckFailCount != 0 || i64Of(row.LastSuccessAt) != i64Of(adopted) || row.LastCheckAt == nil {
		t.Fatalf("row %+v: want streak 0, last_success_at kept, last_check_at bumped", row)
	}
	if ev := r.events("freshness_check"); ev[len(ev)-1]["reason"] != cache.ReasonAnchorAbsent || ev[len(ev)-1]["upstream_status"] != float64(404) {
		t.Fatalf("freshness_check %v, want reason anchor_absent and upstream_status 404", ev[len(ev)-1])
	}
	r.clock.advance(time.Minute)
	r.check() // inside the record's retry time
	if n := len(r.up.etagsSent(rigReleaseURL)); n != 0 || r.row().AdoptionFailCount != 1 {
		t.Fatalf("before the retry time: %d Release probes, count %d; want none and 1", n, r.row().AdoptionFailCount)
	}
	r.clock.advance(time.Minute) // due, 2 min after the first observation
	r.check()
	if n := len(r.up.etagsSent(rigReleaseURL)); n != 0 || r.row().AdoptionFailCount != 2 {
		t.Fatalf("2 min apart: %d Release probes, count %d; want none and 2", n, r.row().AdoptionFailCount)
	}
	r.clock.advance(4 * time.Minute) // due, 6 min after the first observation
	r.check()
	snap := r.currentSnapshot()
	if lastResult(r) != "form_changed" || snap.Form != cache.FormDetached {
		t.Fatalf("confirmed: result %q snapshot %+v, want form_changed to detached", lastResult(r), snap)
	}
	if row := r.row(); row.AdoptionFailCount != 0 {
		t.Fatalf("record %q/%d left after the lateral move", strOf(row.AdoptionFailReason), row.AdoptionFailCount)
	}
}

// Spec C4: never unsigned from signed. A signed suite whose signatures
// all disappear keeps its snapshot, stays visibly anchor_absent and is
// re-probed on the C7 schedule — even with adoption.adopt_unsigned on.
func TestCheck_AbsentAnchorNeverMovesToUnsigned(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopt()
	r.adopter.adoptUnsigned = true
	before := r.currentSnapshot()
	r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 404})
	r.unsignedRelease("dropped", nil)
	for i := 0; i < 20; i++ {
		r.clock.advance(3 * time.Minute)
		r.check()
	}
	snap := r.currentSnapshot()
	if snap.SnapshotID != before.SnapshotID || snap.Form != cache.FormInline {
		t.Fatalf("snapshot %+v, want the signed one kept", snap)
	}
	if lastResult(r) != "anchor_absent" || strOf(r.row().AdoptionFailReason) != cache.ReasonAnchorAbsent {
		t.Fatalf("result %q record %q, want anchor_absent", lastResult(r), strOf(r.row().AdoptionFailReason))
	}
	if trees := len(r.up.etagsSent(rigReleaseGPGURL)); trees < 2 || trees > 8 {
		t.Fatalf("%d probe trees in an hour, want the C7 schedule's 2..8", trees)
	}
}

// Review Focus 3: a detached repository whose upstream publishes a new
// Release but drops Release.gpg (a vendor that stopped signing, or a
// signature lost in a mirror sync) keeps its signed snapshot and is never
// adopted unsigned — with adoption.adopt_unsigned on and long past the
// unsigned confirmation gap. An absent signature (404) is the refusal
// state anchor_absent, paced by its record (design review M-14: before,
// it cost three requests and a WARN per tick, forever); a signature fetch
// that fails unknown (503) is retried at every check, with a WARN.
func TestCheck_DetachedSuiteThatDropsItsSignatureNeverGoesUnsigned(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		r := newRetryRig(t, 0)
		r.adoptDetached()
		r.adopter.adoptUnsigned = true
		before := r.currentSnapshot()
		r.up.setAt(rigReleaseURL, r.releaseText("v2", 0))
		r.up.setFail(rigReleaseGPGURL, &fetch.StatusError{Code: 404})
		gpgBefore, success := len(r.up.etagsSent(rigReleaseGPGURL)), r.row().LastSuccessAt
		for i := 0; i < 6; i++ {
			r.clock.advance(10 * time.Minute)
			r.check()
		}
		if snap := r.currentSnapshot(); snap.SnapshotID != before.SnapshotID || snap.Form != cache.FormDetached {
			t.Fatalf("snapshot %+v, want the signed detached one kept", snap)
		}
		if lastResult(r) != "anchor_absent" || r.ver.calls() != 0 || strOf(r.row().AdoptionFailReason) != cache.ReasonAnchorAbsent {
			t.Fatalf("result %q, %d verifications, record %q; want anchor_absent, none, anchor_absent", lastResult(r),
				r.ver.calls(), strOf(r.row().AdoptionFailReason))
		}
		// Due at +2, +4, +8, +16 and +32 min after each observation: the
		// check at 50 min is inside the record's retry time.
		if n := len(r.up.etagsSent(rigReleaseGPGURL)) - gpgBefore; n != 5 {
			t.Fatalf("%d signature requests over six checks, want 5 (one check paced)", n)
		}
		if n := len(r.events("freshness: Release.gpg fetch failed; deferring adoption")); n != 0 {
			t.Fatalf("%d WARN lines for an absent signature, want none (a refusal state)", n)
		}
		if got := i64Of(r.row().LastSuccessAt); got != i64Of(success) {
			t.Fatalf("last_success_at moved to %d, want it kept at %d (an absent anchor, spec C4)", got, i64Of(success))
		}
	})
	t.Run("unknown", func(t *testing.T) {
		r := newRetryRig(t, 0)
		r.adoptDetached()
		r.adopter.adoptUnsigned = true
		before := r.currentSnapshot()
		r.up.setAt(rigReleaseURL, r.releaseText("v2", 0))
		r.up.setFail(rigReleaseGPGURL, &fetch.StatusError{Code: 503})
		for i := 0; i < 6; i++ {
			r.clock.advance(10 * time.Minute)
			r.check()
		}
		if snap := r.currentSnapshot(); snap.SnapshotID != before.SnapshotID || snap.Form != cache.FormDetached {
			t.Fatalf("snapshot %+v, want the signed detached one kept", snap)
		}
		if lastResult(r) != "changed" || r.ver.calls() != 0 || r.row().AdoptionFailCount != 0 {
			t.Fatalf("result %q, %d verifications, record %q/%d; want changed, none, none", lastResult(r), r.ver.calls(),
				strOf(r.row().AdoptionFailReason), r.row().AdoptionFailCount)
		}
		if n := len(r.events("freshness: Release.gpg fetch failed; deferring adoption")); n != 6 {
			t.Fatalf("%d WARN lines over six checks, want one per check", n)
		}
	})
}

// Spec C4 "Response classification" on the current anchor: a soft 404
// (an HTML 200) and 401/403/410 are absent; a 5xx is unknown (a failed
// check, the streak extends).
func TestCheck_CurrentAnchorClassification(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(r *retryRig)
		want    string
	}{
		{"HTML 200", func(r *retryRig) { r.up.set([]byte("<html>Not here</html>")) }, "anchor_absent"},
		{"403", func(r *retryRig) { r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 403}) }, "anchor_absent"},
		{"410", func(r *retryRig) { r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 410}) }, "anchor_absent"},
		{"503", func(r *retryRig) { r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 503}) }, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRetryRig(t, 0)
			r.adopt()
			tc.prepare(r)
			r.clock.advance(time.Minute)
			r.check()
			row := r.row()
			switch tc.want {
			case "failed":
				if row.CheckFailCount != 1 || row.AdoptionFailCount != 0 {
					t.Fatalf("row %+v, want a failed check without a record", row)
				}
			default:
				if lastResult(r) != tc.want || row.CheckFailCount != 0 {
					t.Fatalf("result %q streak %d, want %s and 0", lastResult(r), row.CheckFailCount, tc.want)
				}
			}
		})
	}
}

// The probe tree finds the current snapshot's own anchor again (it came
// back between the check's request and the tree): nothing to adopt.
func TestProbeTree_SameAsTheCurrentSnapshot(t *testing.T) {
	in := []byte("-----BEGIN PGP SIGNED MESSAGE-----\nx")
	h := hashOf(in)
	snap := &cache.SuiteSnapshot{Form: cache.FormInline, SingleAnchorHash: &h}
	tree := probeTree{form: adoptionFormInline, found: true, inRelease: probeOutcome{class: classPresent,
		res: &fetch.ConditionalResult{Status: 200, Body: in}}}
	if !tree.sameAs(snap) {
		t.Fatal("the current inline anchor was not recognised")
	}
	other := tree
	other.inRelease.res = &fetch.ConditionalResult{Status: 200, Body: []byte("-----BEGIN PGP SIGNED MESSAGE-----\ny")}
	if other.sameAs(snap) {
		t.Fatal("other content counted as the current anchor")
	}
	if (probeTree{form: adoptionFormDetached, found: true}).sameAs(snap) {
		t.Fatal("another form counted as the current anchor")
	}
}

// Spec C4 Dispatch 2: an unsigned suite upgrades as soon as either
// signature file is served (result form_changed).
func TestCheck_UnsignedSuiteUpgradesWhenSigned(t *testing.T) {
	t.Run("InRelease", func(t *testing.T) {
		r := newRetryRig(t, 0)
		r.adoptUnsigned("u1")
		in := r.release("signed", 0)
		r.up.setFail(rigAnchorURL, nil)
		r.up.set(in)
		r.check()
		if snap := r.currentSnapshot(); lastResult(r) != "form_changed" || snap.Form != cache.FormInline {
			t.Fatalf("result %q snapshot %+v, want form_changed to inline", lastResult(r), snap)
		}
	})
	t.Run("Release.gpg", func(t *testing.T) {
		r := newRetryRig(t, 0)
		rel := r.adoptUnsigned("u1")
		r.up.setFail(rigReleaseGPGURL, nil)
		r.up.setAt(rigReleaseGPGURL, armoredSig("now signed"))
		r.check()
		snap := r.currentSnapshot()
		if lastResult(r) != "form_changed" || snap.Form != cache.FormDetached || strOf(snap.ReleaseHash) != hashOf(rel) {
			t.Fatalf("result %q snapshot %+v, want form_changed to the detached pair", lastResult(r), snap)
		}
	})
	t.Run("an unknown signature probe fails the check", func(t *testing.T) {
		r := newRetryRig(t, 0)
		r.adoptUnsigned("u1")
		r.unsignedRelease("u2", nil)
		r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 503})
		before := r.currentSnapshot().SnapshotID
		r.check()
		if row := r.row(); row.CheckFailCount != 1 || r.currentSnapshot().SnapshotID != before {
			t.Fatalf("row %+v, want a failed check and no unsigned update", row)
		}
	})
}

// Spec C5 safeguard 2: updates of an established unsigned suite adopt at
// once (no confirmation), unless a signed-seen marker appeared for its base
// (never downgrade: refused, recorded as unsigned_downgrade_refused).
func TestCheck_UnsignedSuiteUpdates(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adoptUnsigned("u1")
	u2 := r.unsignedRelease("u2", nil)
	r.check()
	if snap := r.currentSnapshot(); lastResult(r) != "changed" || strOf(snap.SingleAnchorHash) != hashOf(u2) || snap.Form != cache.FormUnsigned {
		t.Fatalf("result %q snapshot %+v, want u2 adopted at once", lastResult(r), snap)
	}
	if err := r.cache.MarkSignedSeenBatch(context.Background(), []cache.SignedSeenMark{{CanonicalScheme: "http",
		CanonicalHost: rigHost, Base: rigSuite, SeenAt: r.clock.now().Unix()}}); err != nil {
		t.Fatal(err)
	}
	r.unsignedRelease("u3", nil)
	r.clock.advance(time.Minute)
	r.check()
	if snap := r.currentSnapshot(); strOf(snap.SingleAnchorHash) != hashOf(u2) ||
		strOf(r.row().AdoptionFailReason) != cache.ReasonUnsignedDowngradeRefused || lastResult(r) != "changed_deferred" {
		t.Fatalf("with a marker: snapshot %+v record %q result %q, want u2 kept and the refusal", snap,
			strOf(r.row().AdoptionFailReason), lastResult(r))
	}
}

// Spec §7 (C5): an unsigned suite whose Release anchor row was reaped
// recovers it from the snapshot (the reader goes through the form: the
// unsigned Release hash sits in the single-anchor slot) and stays fresh.
func TestCheck_UnsignedRecoversWhenReleaseAnchorMissing(t *testing.T) {
	r := newRetryRig(t, 0)
	rel := r.adoptUnsigned("u1")
	ctx := context.Background()
	if err := r.cache.EvictURLPath(ctx, "http", rigHost, rigSuite+"/Release"); err != nil {
		t.Fatal(err)
	}
	r.check()
	row, err := r.cache.LookupURL(ctx, "http", rigHost, rigSuite+"/Release")
	if err != nil || strOf(row.BlobHash) != hashOf(rel) {
		t.Fatalf("Release anchor row %+v (%v), want it re-seeded with the snapshot's hash", row, err)
	}
	if res := lastResult(r); res != "unchanged" {
		t.Fatalf("result %q, want unchanged", res)
	}
}

// The reserved-slot rule (spec C4) for repair and reconcile: only a suite
// whose current snapshot is signed may take the last free slot (overview
// "E:" carry-forward: signedCurrent = form != unsigned).
func TestReconcile_UnsignedSuiteNeverTakesTheReservedSlot(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adoptUnsigned("u1")
	held := mustAcquire(t, r.checker.admission, "other", true)
	defer held.release()
	if r.checker.Reconcile(context.Background(), "http", rigHost, rigSuite) {
		t.Fatal("an unsigned suite's reconcile took the reserved slot")
	}
	signed := newRetryRig(t, 0)
	signed.adopt()
	heldSigned := mustAcquire(t, signed.checker.admission, "other", true)
	defer heldSigned.release()
	if !signed.checker.Reconcile(context.Background(), "http", rigHost, rigSuite) {
		t.Fatal("a signed suite's reconcile was refused the reserved slot")
	}
	signed.checker.WaitForAdoptions()
}

// finishCheck hands the repair the current snapshot's signedness.
func TestFinishCheck_RepairCarriesSignedCurrent(t *testing.T) {
	fc := newFakeCache()
	fc.track("http", "h", "/r/dists/x")
	c, err := New(Config{Cache: fc, Fetcher: &changedUpstream{}, HostLimiter: hostsem.New(1), Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	id := int64(3)
	for _, signed := range []bool{true, false} {
		cur := &cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", CurrentSnapshotID: &id}
		rp := c.finishCheck(context.Background(), &checkOutcome{}, cur, SuiteRef{CanonicalScheme: "http", CanonicalHost: "h",
			SuitePath: "/r/dists/x"}, checkEnd{form: "inline", result: "unchanged", status: 200, signedCurrent: signed})
		if rp == nil || rp.signed != signed {
			t.Fatalf("signedCurrent %v: repair %+v", signed, rp)
		}
	}
}

// hostCounter returns the value of the counter name for host, as
// metrics.Default renders it (0 when absent).
func hostCounter(t *testing.T, name, host string) float64 {
	t.Helper()
	var buf bytes.Buffer
	metrics.Default.Render(&buf)
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(line, name+"{") && strings.Contains(line, `host="`+host+`"`) {
			v, err := strconv.ParseFloat(line[strings.LastIndexByte(line, ' ')+1:], 64)
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
	}
	return 0
}

// The reserved-slot rule for the tick's repair (spec C4; overview "E:"
// carry-forward): a degraded unsigned suite's repair never takes the last
// free slot, a signed suite's may.
func TestCheck_RepairOfAnUnsignedSuiteNeverTakesTheReservedSlot(t *testing.T) {
	for _, form := range []cache.AnchorForm{cache.FormUnsigned, cache.FormInline} {
		t.Run(string(form), func(t *testing.T) {
			r := newRetryRig(t, 0)
			r.adopter.tolerateOptionalMembers, r.adopter.repairSkippedMembers, r.adopter.adoptUnsigned = true, true, true
			good := []byte(strings.Repeat("C", 100))
			text := datedRelease("degraded", 0, map[string][]byte{"main/binary-amd64/Packages": r.pkgs, "Contents-amd64.gz": good})
			r.members.put("http://h/r/dists/x/Contents-amd64.gz", []byte(strings.Repeat("z", 90))) // a stale optional member
			ref := SuiteRef{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite}
			if form == cache.FormUnsigned {
				r.up.setAt(rigReleaseURL, text)
				if err := r.adopter.RunUnsigned(context.Background(), ref, text, "", ""); err != nil {
					t.Fatal(err)
				}
			} else {
				in := asInRelease(text)
				r.up.set(in)
				if err := r.adopter.Run(context.Background(), ref, in, "", ""); err != nil {
					t.Fatal(err)
				}
			}
			held := mustAcquire(t, r.checker.admission, "other", true)
			defer held.release()
			before := hostCounter(t, "acu_repair_deferred_total", rigHost)
			r.check()
			deferred := hostCounter(t, "acu_repair_deferred_total", rigHost) - before
			if want := map[cache.AnchorForm]float64{cache.FormUnsigned: 1, cache.FormInline: 0}[form]; deferred != want {
				t.Fatalf("result %q: %v repairs deferred, want %v", lastResult(r), deferred, want)
			}
		})
	}
}

// Design review I-1: a record's validators belong to the anchor file its
// request fetched. A detached suite whose lateral InRelease keeps failing
// (a key not imported), behind a CDN that sends Last-Modified but no ETag,
// keeps adopting its new Release: the lateral record holds no validators,
// so the Release GET carries the current Release's own — never the
// InRelease's Last-Modified, which answers 304 for a Release of the same
// publish and froze the suite.
func TestCheck_DetachedSuiteKeepsAdoptingWhileItsLateralInReleaseFails(t *testing.T) {
	r := newRetryRig(t, 0)
	r.up.setLastModified(rigReleaseURL, "2026-01-01T00:00:00Z")
	r.up.setLastModified(rigReleaseGPGURL, "2026-01-01T00:00:00Z")
	r.adoptDetached()
	// One publish: an InRelease the cache cannot verify, a new Release and its signature.
	in := r.release("v2", 0)
	r.ver.reject(hashOf(in))
	r.up.set(in)
	rel := r.releaseText("v2", 0)
	r.up.setAt(rigReleaseURL, rel)
	r.up.setAt(rigReleaseGPGURL, armoredSig("v2 sig"))
	for _, url := range []string{rigAnchorURL, rigReleaseURL, rigReleaseGPGURL} {
		r.up.setLastModified(url, "2026-02-01T00:00:00Z")
	}
	r.clock.advance(time.Minute)
	r.check() // the lateral attempt fails
	if row := r.row(); r.ver.calls() != 1 || row.AdoptionFailCount != 1 || row.AdoptionFailLastMod != nil || row.AdoptionFailETag != nil {
		t.Fatalf("setup: calls %d, record %d with validators %v/%v; want the lateral failure recorded without validators",
			r.ver.calls(), row.AdoptionFailCount, strOf(row.AdoptionFailETag), strOf(row.AdoptionFailLastMod))
	}
	r.clock.advance(time.Minute) // before the record's retry time
	r.check()
	if snap := r.currentSnapshot(); snap.Form != cache.FormDetached || strOf(snap.ReleaseHash) != hashOf(rel) {
		t.Fatalf("snapshot %+v, result %q; want the detached v2 adopted while the lateral InRelease is deferred",
			snap, lastResult(r))
	}
}

// Spec C3 revision 8 (ruling PE-5, design review I-2): an unsigned suite
// whose upstream starts signing is marked as seen signed by the upgrade
// probe — even when the upgrade then fails (a key not imported) — so a
// later strip of both signature files (a man-in-the-middle answering 404)
// never updates it unsigned: refused as unsigned_downgrade_refused, the
// snapshot kept. A marker write that fails fails the check.
func TestCheck_UnsignedSuiteSeenSignedNeverUpdatesUnsigned(t *testing.T) {
	t.Run("stripped after a failed upgrade", func(t *testing.T) {
		r := newRetryRig(t, 0)
		u1 := r.adoptUnsigned("u1")
		signed := r.release("now signed", 0)
		r.ver.reject(hashOf(signed))
		r.up.set(signed)
		r.check() // the upgrade probe marks the base; the upgrade fails
		if r.ver.calls() != 1 || r.currentSnapshot().Form != cache.FormUnsigned {
			t.Fatalf("setup: calls %d, snapshot %+v; want a failed upgrade", r.ver.calls(), r.currentSnapshot())
		}
		if _, ok, err := r.cache.SignedSeen(context.Background(), "http", rigHost, rigSuite); err != nil || !ok {
			t.Fatalf("no marker after the probe saw the signature (ok %v, err %v)", ok, err)
		}
		r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 404})
		r.unsignedRelease("u2, modified", &fetch.StatusError{Code: 404})
		r.clock.advance(time.Hour)
		r.check()
		if snap := r.currentSnapshot(); strOf(snap.SingleAnchorHash) != hashOf(u1) ||
			strOf(r.row().AdoptionFailReason) != cache.ReasonUnsignedDowngradeRefused {
			t.Fatalf("snapshot %+v, record %q; want u1 kept and unsigned_downgrade_refused", snap, strOf(r.row().AdoptionFailReason))
		}
	})
	t.Run("failed marker write", func(t *testing.T) {
		r := newRetryRig(t, 0)
		r.adoptUnsigned("u1")
		r.up.set(r.release("now signed", 0))
		db, err := sql.Open("sqlite", filepath.Join(r.cache.Dir(), "cache.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		if _, err := db.Exec(`CREATE TRIGGER no_marks BEFORE INSERT ON suite_signed_seen
  BEGIN SELECT RAISE(ABORT, 'marker writes fail'); END`); err != nil {
			t.Fatal(err)
		}
		r.check()
		if row := r.row(); row.CheckFailCount != 1 || r.ver.calls() != 0 || r.currentSnapshot().Form != cache.FormUnsigned {
			t.Fatalf("row %+v, verifier calls %d: want a failed check, no upgrade attempt", row, r.ver.calls())
		}
	})
}

// Ruling PE-2: an adopted suite in anchor_absent is paced by its record —
// a client check (T1) inside the record's retry time asks nothing, and the
// scheduler does not queue it (suiteDue) until that time. A next_retry_at
// beyond the legitimate horizon (a backward clock step) is not paced: once
// the suite is due (its last success is old), the check clamps it to an
// hour and asks nothing.
func TestCheck_AbsentAnchorIsPacedByItsRecord(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopt()
	r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 404})
	r.clock.advance(time.Minute)
	r.check() // the first observation: anchor_absent, due in 2 minutes
	gets := func() int { return len(r.up.etagsSent(rigAnchorURL)) }
	before, lines := gets(), len(r.events("freshness_check"))
	for range 5 {
		r.clock.advance(20 * time.Second)
		r.check()
	}
	if gets() != before || len(r.events("freshness_check")) != lines {
		t.Fatalf("client checks inside the retry time: %d requests, %d lines; want none", gets()-before,
			len(r.events("freshness_check"))-lines)
	}
	// A long refusal backoff (as a later observation sets), and the last
	// success older than periodic_refresh: stale, but paced by the record.
	if err := r.cache.SetAdoptionNextRetry(context.Background(), "http", rigHost, rigSuite,
		r.clock.now().Add(30*time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	r.clock.advance(16 * time.Minute)
	if r.checker.suiteDue(r.row(), r.clock.now()) {
		t.Fatal("the scheduler queues an anchor_absent suite inside its record's retry time")
	}
	r.check()
	if gets() != before {
		t.Fatal("a check inside the record's retry time made a request")
	}
	r.clock.advance(15 * time.Minute) // past the retry time
	if !r.checker.suiteDue(r.row(), r.clock.now()) {
		t.Fatal("an anchor_absent suite is not due at its record's retry time")
	}
	r.check() // the anchor's GET, and — the absence confirmed — the tree, whose first probe is InRelease
	if gets() != before+2 {
		t.Fatalf("the due check made %d InRelease requests, want 2", gets()-before)
	}
	if err := r.cache.SetAdoptionNextRetry(context.Background(), "http", rigHost, rigSuite,
		r.clock.now().Add(24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	r.clock.advance(16 * time.Minute) // the last success is older than periodic_refresh
	if !r.checker.suiteDue(r.row(), r.clock.now()) {
		t.Fatal("a next_retry_at beyond the horizon kept the suite from being due")
	}
	r.check()
	if gets() != before+2 || i64Of(r.row().AdoptionNextRetryAt) != r.clock.now().Add(time.Hour).Unix() {
		t.Fatalf("beyond the horizon: %d requests, next_retry_at %d; want none and now + 1 h", gets()-before-2,
			i64Of(r.row().AdoptionNextRetryAt))
	}
}

// Design review M-5: an established unsigned suite whose new Release
// declares more than unsignedMemberCap members is refused at the check —
// unsigned_too_large, no slot, no adoption — and keeps its snapshot.
func TestCheck_UnsignedSuiteTooLargeUpdateIsRefused(t *testing.T) {
	r := newRetryRig(t, 0)
	u1 := r.adoptUnsigned("u1")
	big := make(map[string][]byte, unsignedMemberCap+1)
	for i := 0; i <= unsignedMemberCap; i++ {
		big[fmt.Sprintf("main/i18n/Translation-%d", i)] = []byte{byte(i)}
	}
	r.up.setAt(rigReleaseURL, datedRelease("big", 0, big))
	before := adoptionTotalFor(t, rigHost)
	r.check()
	if snap := r.currentSnapshot(); strOf(snap.SingleAnchorHash) != hashOf(u1) ||
		strOf(r.row().AdoptionFailReason) != cache.ReasonUnsignedTooLarge || lastResult(r) != "changed_deferred" {
		t.Fatalf("snapshot %+v record %q result %q; want u1 kept, unsigned_too_large, changed_deferred", snap,
			strOf(r.row().AdoptionFailReason), lastResult(r))
	}
	if adoptionTotalFor(t, rigHost) != before {
		t.Fatal("a too-large unsigned update ran an adoption")
	}
}

// Design review M-16: an unsigned suite whose Release was absent and is
// found again by the probe tree — its own form — is "unchanged" when the
// bytes are the snapshot's (the record is cleared) and an unsigned update
// otherwise (result changed, adopted at once, after the unsigned refusals:
// signed history refuses it); it is never re-recorded as anchor_absent.
func TestCheck_AbsentUnsignedReleaseThatComesBack(t *testing.T) {
	for _, tc := range []struct {
		name           string
		update, marker bool
		result         string
	}{
		{"same bytes", false, false, "unchanged"},
		{"new bytes", true, false, "changed"},
		{"new bytes with signed history", true, true, "changed_deferred"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRetryRig(t, 0)
			u1 := r.adoptUnsigned("u1")
			back := u1
			if tc.update {
				back = r.releaseText("u2", 0)
			}
			var n atomic.Int32
			up := simtest.NewUpstream()
			up.Set(rigReleaseURL, func(ctx context.Context, etag string) (*fetch.ConditionalResult, error) {
				if n.Add(1) <= 2 { // the check's GET twice; the tree's probe finds it
					return nil, &fetch.StatusError{Code: 404}
				}
				return simtest.Serve(back)(ctx, etag)
			})
			chk, err := New(Config{Cache: r.cache, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute,
				Adopter: r.adopter, Logger: slog.New(slog.NewJSONHandler(r.logs, nil)), // lastResult reads its lines
				now: r.clock.now, rand: func() float64 { return 0.5 }})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			chk.Check(ctx, "http", rigHost, rigSuite) // absent: anchor_absent recorded
			chk.WaitForAdoptions()
			if row := r.row(); strOf(row.AdoptionFailReason) != cache.ReasonAnchorAbsent {
				t.Fatalf("setup: record %q, want anchor_absent", strOf(row.AdoptionFailReason))
			}
			if tc.marker {
				if err := r.cache.MarkSignedSeenBatch(ctx, []cache.SignedSeenMark{{CanonicalScheme: "http",
					CanonicalHost: rigHost, Base: rigSuite, SeenAt: r.clock.now().Unix()}}); err != nil {
					t.Fatal(err)
				}
			}
			r.clock.advance(10 * time.Minute) // past the confirmation gap and the record's retry time
			chk.Check(ctx, "http", rigHost, rigSuite)
			chk.WaitForAdoptions()
			row, snap := r.row(), r.currentSnapshot()
			if res := lastResult(r); res != tc.result {
				t.Fatalf("result %q, want %q", res, tc.result)
			}
			if tc.marker {
				if strOf(row.AdoptionFailReason) != cache.ReasonUnsignedDowngradeRefused || strOf(snap.SingleAnchorHash) != hashOf(u1) {
					t.Fatalf("record %q, snapshot %+v; want unsigned_downgrade_refused and u1 kept", strOf(row.AdoptionFailReason), snap)
				}
				return
			}
			if row.AdoptionFailCount != 0 || snap.Form != cache.FormUnsigned || strOf(snap.SingleAnchorHash) != hashOf(back) {
				t.Fatalf("record %q/%d, snapshot %+v; want no record and the unsigned snapshot of the Release served",
					strOf(row.AdoptionFailReason), row.AdoptionFailCount, snap)
			}
		})
	}
}

// Design review I-2 on the absent-anchor path of an unsigned suite: when
// the probe tree finds a signature (InRelease appeared between the check's
// probes and the tree), the suite's base is marked before the upgrade is
// admitted — so it stays marked when the upgrade fails.
func TestCheck_AbsentUnsignedReleaseTreeFindsASignature(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adoptUnsigned("u1")
	signed := r.release("now signed", 0)
	r.ver.reject(hashOf(signed))
	var n atomic.Int32
	up := simtest.NewUpstream() // Release and Release.gpg: 404
	up.Set(rigAnchorURL, func(ctx context.Context, etag string) (*fetch.ConditionalResult, error) {
		if n.Add(1) <= 2 { // the upgrade probes of both checks; the tree's probe finds it
			return nil, &fetch.StatusError{Code: 404}
		}
		return simtest.Serve(signed)(ctx, etag)
	})
	chk, err := New(Config{Cache: r.cache, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute,
		Adopter: r.adopter, Logger: discardLogger(), now: r.clock.now, rand: func() float64 { return 0.5 }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	chk.Check(ctx, "http", rigHost, rigSuite) // Release absent: anchor_absent recorded
	chk.WaitForAdoptions()
	r.clock.advance(10 * time.Minute)
	chk.Check(ctx, "http", rigHost, rigSuite) // the tree finds InRelease: the upgrade, which fails
	chk.WaitForAdoptions()
	if r.ver.calls() != 1 || r.currentSnapshot().Form != cache.FormUnsigned {
		t.Fatalf("setup: calls %d, snapshot %+v; want one failed upgrade", r.ver.calls(), r.currentSnapshot())
	}
	if _, ok, err := r.cache.SignedSeen(ctx, "http", rigHost, rigSuite); err != nil || !ok {
		t.Fatalf("no marker after the tree found a signature (ok %v, err %v)", ok, err)
	}
}

// Deviation 2, for anchor_absent (design review M-19): a future
// adoption_fail_first_at never confirms the absence — the tree does not
// run — and re-recording restarts it at now (recordAdoptionFailure's
// past-event rule), so a backward clock step costs one more gap, never a
// skipped observation.
func TestCheck_AbsentAnchorWithAFutureFirstAtWaitsAnotherGap(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopt()
	r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 404})
	r.up.setAt(rigReleaseURL, r.releaseText("v2", 0))
	r.up.setAt(rigReleaseGPGURL, armoredSig("v2 sig"))
	now := r.clock.now()
	if err := r.cache.RecordAdoptionFailure(context.Background(), "http", rigHost, rigSuite, cache.AdoptionFailure{
		Hash: anchorAbsentKey, Reason: cache.ReasonAnchorAbsent, Count: 1,
		FirstAt: now.Add(2 * time.Hour).Unix(), LastAt: now.Unix(), NextRetryAt: now.Unix()}); err != nil {
		t.Fatal(err)
	}
	r.check()
	if n := len(r.up.etagsSent(rigReleaseURL)); n != 0 {
		t.Fatalf("the tree ran (%d Release probes) on an absence first observed in the future", n)
	}
	if first := i64Of(r.row().AdoptionFailFirstAt); first != now.Unix() {
		t.Fatalf("first_at = %d, want it restarted at %d", first, now.Unix())
	}
	r.clock.advance(6 * time.Minute) // past the gap and the record's retry time
	r.check()
	if snap := r.currentSnapshot(); lastResult(r) != "form_changed" || snap.Form != cache.FormDetached {
		t.Fatalf("result %q snapshot %+v, want the lateral move one gap later", lastResult(r), snap)
	}
}

// Design review M-13: an adopted suite stale in a refusal state
// (anchor_absent keeps its last success old on purpose) counts in the
// stale gauge but not in the stale WARN, which keeps alerting on a suite
// stale for any other reason.
func TestStaleSuiteCounts_RefusalStates(t *testing.T) {
	id, old := int64(7), int64(500)
	absent, failing := cache.ReasonAnchorAbsent, "gpg_failed"
	suites := []cache.SuiteFreshness{
		{LastSuccessAt: &old, CurrentSnapshotID: &id, AdoptionFailCount: 3, AdoptionFailReason: &absent},
		{LastSuccessAt: &old, CurrentSnapshotID: &id, AdoptionFailCount: 3, AdoptionFailReason: &failing},
		{LastSuccessAt: &old, CurrentSnapshotID: &id},
		{LastSuccessAt: &old, AdoptionFailCount: 1, AdoptionFailReason: &absent},
	}
	adopted, unadopted, refused := staleSuiteCounts(suites, 1000, 3000)
	if adopted != 3 || unadopted != 1 || refused != 1 {
		t.Fatalf("adopted %d unadopted %d refused %d, want 3, 1, 1", adopted, unadopted, refused)
	}
}

func TestTick_StaleWarnLeavesOutRefusalStates(t *testing.T) {
	for _, absent := range []bool{true, false} {
		r := newRetryRig(t, 0)
		r.adopt()
		if absent {
			r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 404}) // anchor_absent
		} else {
			r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 503}) // failed checks
		}
		r.clock.advance(time.Minute)
		r.check()
		r.clock.advance(time.Hour) // past 4 × periodic_refresh
		r.checker.tick(context.Background(), newSchedQueues())
		warns := r.events("freshness: adopted suites stale beyond threshold")
		if absent && len(warns) != 0 {
			t.Fatalf("anchor_absent: %d stale WARNs, want none", len(warns))
		}
		if !absent && (len(warns) != 1 || warns[0]["adopted_stale"] != float64(1)) {
			t.Fatalf("failing upstream: stale WARNs %v, want one with adopted_stale 1", warns)
		}
	}
}

// Design note 23 / design review M-14: a changed Release whose signature
// is absent does not take the record a still-served lateral InRelease
// holds — that content keeps its own schedule.
func TestCheck_AbsentSignatureKeepsTheLateralRecord(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adoptDetached()
	bad := r.release("bad", 0)
	r.ver.reject(hashOf(bad))
	r.up.set(bad)
	r.clock.advance(time.Minute)
	r.check() // the lateral attempt fails: a record for the InRelease content
	r.up.setAt(rigReleaseURL, r.releaseText("v2", 0))
	r.up.setFail(rigReleaseGPGURL, &fetch.StatusError{Code: 404})
	r.clock.advance(20 * time.Second) // before the record's retry time: the lateral content is held
	r.check()
	if row := r.row(); lastResult(r) != "anchor_absent" || strOf(row.AdoptionFailReason) == cache.ReasonAnchorAbsent ||
		strOf(row.AdoptionFailHash) != hashOf(bad) {
		t.Fatalf("result %q record %q; want anchor_absent reported and the lateral content's record kept", lastResult(r),
			strOf(row.AdoptionFailReason))
	}
}

// Design note 24: an unsigned suite never updates past a signature that may
// be there — an unknown Release.gpg probe fails the check exactly as an
// unknown InRelease does (TestCheck_UnsignedSuiteUpgradesWhenSigned covers
// only the latter). (Proposed by the executability review; kills X06.)
func TestCheck_UnsignedSuiteUnknownReleaseGPGFailsTheCheck(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adoptUnsigned("u1")
	r.unsignedRelease("u2", &fetch.StatusError{Code: 503})
	before := r.currentSnapshot().SnapshotID
	r.check()
	if row := r.row(); row.CheckFailCount != 1 || r.currentSnapshot().SnapshotID != before {
		t.Fatalf("row %+v, current %d: want a failed check and no unsigned update", row, r.currentSnapshot().SnapshotID)
	}
}

// Spec C4 reserved slot: an unsigned suite (its current snapshot vouches
// nothing) never takes the last free slot — neither for an update of its
// unsigned Release nor for an upgrade to a signed form; both queue.
// (Proposed by the executability review; kills X07, X08.)
func TestCheck_UnsignedSuiteNeverTakesTheReservedSlotToAdopt(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		r := newRetryRig(t, 0) // MaxConcurrent: 2
		r.adoptUnsigned("u1")
		if upgrade {
			r.up.set(r.release("now signed", 0)) // InRelease present: the upgrade
		} else {
			r.unsignedRelease("u2", nil) // an update of the unsigned Release
		}
		held := mustAcquire(t, r.checker.admission, "other", true) // one of two slots taken
		r.check()
		held.release()
		if lastResult(r) != "changed_queued" {
			t.Fatalf("upgrade=%v: result %q, want changed_queued (the last slot is reserved for signed suites)", upgrade, lastResult(r))
		}
	}
}

// Spec C4 Dispatch 1 / design note 23: an anchor that was absent and is
// found again by the probe tree — the same form and the same bytes as the
// current snapshot — is "unchanged" and clears the anchor_absent record;
// it is not a form change that re-adopts the current content. The
// upstream answers the anchor's own GET 404 on the first two checks and
// serves it again to the tree's probe (an anchor that comes back within a
// check). (Proposed by the executability review; kills X25, which the plan
// pinned at the predicate only.)
func TestCheck_AbsentAnchorThatComesBackIsUnchanged(t *testing.T) {
	r := newRetryRig(t, 0)
	v1 := r.release("v1", 0)
	r.up.set(v1)
	r.check() // the first adoption: inline v1
	snap := r.currentSnapshot()
	if snap == nil {
		t.Fatal("setup: v1 not adopted")
	}
	var n atomic.Int32
	up := simtest.NewUpstream()
	up.Set(rigAnchorURL, func(ctx context.Context, etag string) (*fetch.ConditionalResult, error) {
		if n.Add(1) <= 2 {
			return nil, &fetch.StatusError{Code: 404}
		}
		return simtest.Serve(v1)(ctx, etag)
	})
	chk, err := New(Config{Cache: r.cache, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute,
		Adopter: r.adopter, Logger: discardLogger(), now: r.clock.now, rand: func() float64 { return 0.5 }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	chk.Check(ctx, "http", rigHost, rigSuite) // absent: anchor_absent recorded
	chk.WaitForAdoptions()
	if row := r.row(); strOf(row.AdoptionFailReason) != cache.ReasonAnchorAbsent {
		t.Fatalf("setup: record %q, want anchor_absent", strOf(row.AdoptionFailReason))
	}
	r.clock.advance(10 * time.Minute) // past the confirmation gap and the record's retry time
	chk.Check(ctx, "http", rigHost, rigSuite)
	chk.WaitForAdoptions()
	row := r.row()
	if row.AdoptionFailCount != 0 || row.CurrentSnapshotID == nil || *row.CurrentSnapshotID != snap.SnapshotID {
		t.Fatalf("row %+v: want the record cleared and snapshot %d still current", row, snap.SnapshotID)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/freshness/ -run 'TestCheck_DetachedSuiteMovesToInline|TestCheck_LateralFailingContent|TestCheck_AbsentAnchor|TestCheck_CurrentAnchorClassification|TestProbeTree_SameAs|TestCheck_UnsignedSuite|TestCheck_UnsignedRecovers|TestReconcile_UnsignedSuite|TestFinishCheck_Repair|TestCheck_RepairOfAnUnsignedSuite'`
Expected: FAIL — compile error naming `tree.sameAs undefined (type probeTree has no field or method sameAs)`, `rp.signed undefined` and `assignment mismatch: 3 variables but staleSuiteCounts returns 2 values` (with `-gcflags=-e` every one is listed).

- [ ] **Step 3: Implement**

Create `internal/freshness/current_form.go`:

```go
package freshness

// Spec C4 Dispatch 1 and 2 with an adopter wired: a suite with a current
// snapshot moves between the signed forms when its anchor disappears or
// the other signed form appears, an unsigned suite upgrades as soon as a
// signature is served, and nothing ever moves a signed suite to unsigned.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
)

// pacedAbsent is ruling PE-2 for an adopted suite: while its active record
// is anchor_absent and not due, the check makes no request — the current
// anchor is asked for again at the record's retry time (2 min doubling to
// 1 h: about 24 requests a day once the absence has lasted an hour), not at
// every tick and every client check (T1). checkLocked applies it before
// any request, and suiteDue does not queue such a suite.
//
// AIDEV-NOTE: the record's schedule is the refusal's, not the failure
// backoff: the check_fail_count streak stays 0 (ruling R-E4). Cost: a
// returning anchor is noticed at the record's retry time (at most 1 h
// later) instead of at once. An EOL release removed upstream would
// otherwise cost 400–1,800 requests a day, forever.
func pacedAbsent(g retryGate) bool {
	return g.active && !g.due && g.reason == cache.ReasonAnchorAbsent
}

// body returns the bytes of the anchor file name the tree fetched.
func (t probeTree) body(name string) []byte {
	switch name {
	case "Release":
		return t.release.res.Body
	case "Release.gpg":
		return t.sig.res.Body
	}
	return t.inRelease.res.Body
}

// sameAs reports whether the tree found exactly snap's form and anchor
// files: the anchor came back between the check's request and the tree.
func (t probeTree) sameAs(snap *cache.SuiteSnapshot) bool {
	form, ok := formOfSnapshot(snap)
	if !ok || !t.found || form != t.form {
		return false
	}
	for _, name := range aptpath.AnchorPaths(formName(t.form)) {
		h := snap.AnchorHash(name)
		sum := sha256.Sum256(t.body(name))
		if h == nil || *h != hex.EncodeToString(sum[:]) {
			return false
		}
	}
	return true
}

// anchorAbsent is spec C4 Dispatch 1's "current anchor absent" (inline:
// InRelease; detached: Release; unsigned: Release) for an adopted suite.
// The first absent observation records the refusal state anchor_absent
// (keyed anchorAbsentKey); once the record is due and the absence has
// lasted confirmGap (two observations at least max(cooldown, 5 min)
// apart), the probe tree runs without validators (anchorFoundAgain): the
// other signed form found is adopted (a lateral move or an upgrade, result
// form_changed); the current form and content found again is "unchanged"
// (the record is cleared); an unsigned suite's own form found again with
// new bytes is its update; anything else — an unsigned candidate of a
// signed suite, or nothing — is recorded again: never unsigned from signed.
// A record from an adoption attempt (the lateral move failed) keeps its own
// schedule: at its retry time the tree runs at once. Every outcome but a
// probe that ends unknown leaves last_success_at alone and resets the
// check_fail_count streak (ruling R-E4: an absent anchor is an answer, not
// an unknown end).
//
// AIDEV-NOTE: last_success_at stays old on purpose (spec C4: "visible;
// last_success_at not bumped"); the record paces the suite (pacedAbsent),
// so it costs one anchor request per retry, plus the probe tree once the
// absence is confirmed. A future adoption_fail_first_at never confirms
// (its age is negative) and re-recording restarts it — deviation 2, here
// for the absence's two observations.
func (c *Checker) anchorAbsent(ctx, fetchCtx context.Context, out *checkOutcome, cur *cache.SuiteFreshness, suite SuiteRef,
	now time.Time, nowUnix int64, form adoptionForm, o probeOutcome, gate retryGate, signedCurrent bool) *adoptionRequest {
	record := &adoptionRequest{suite: suite, key: anchorAbsentKey}
	absentRecord := gate.active && gate.reason == cache.ReasonAnchorAbsent
	runTree := false
	switch {
	case !gate.active:
		c.recordAdoptionFailure(record, cache.ReasonAnchorAbsent) // the first observation
	case !gate.due:
		// Inside the retry time of a record from an adoption attempt (an
		// anchor_absent record is paced before any request: pacedAbsent).
	case !absentRecord:
		runTree = true
	case cur.AdoptionFailFirstAt != nil && now.Sub(time.Unix(*cur.AdoptionFailFirstAt, 0)) >= c.confirmGap():
		runTree = true
	default:
		c.recordAdoptionFailure(record, cache.ReasonAnchorAbsent)
	}
	if runTree {
		rows, _, err := c.cachedAnchors(ctx, suite)
		if err != nil {
			c.logger.Warn("freshness: read anchor rows failed",
				"err", err, "canonical_host", suite.CanonicalHost, "suite_path", suite.SuitePath)
		}
		t := c.runProbeTree(fetchCtx, suite, c.anchorURLsFor(suite, rows))
		switch {
		case t.end != nil && t.end.cancelled():
			return nil
		case t.end != nil:
			c.failCheck(ctx, out, cur, suite, nowUnix, formName(form), fmt.Errorf("%s: %w", t.endName, t.end.err))
			return nil
		case t.found && (t.form != adoptionFormUnsigned || form == adoptionFormUnsigned):
			return c.anchorFoundAgain(ctx, out, cur, suite, nowUnix, form, t, signedCurrent)
		}
		c.recordAdoptionFailure(record, cache.ReasonAnchorAbsent)
	}
	cur.LastCheckAt = &nowUnix
	cur.CheckFailCount = 0
	c.finishCheck(ctx, out, cur, suite, checkEnd{form: formName(form), result: "anchor_absent", status: o.status,
		reason: cache.ReasonAnchorAbsent, signedCurrent: signedCurrent})
	return nil
}

// anchorFoundAgain handles a probe tree, run for an absent current anchor,
// that found a form the suite may adopt: a signed form, or — for an
// unsigned suite — the unsigned form again (design review M-16). The
// current snapshot's own anchors are "unchanged" (the record is cleared);
// anything else is a request: another form's (form_changed, a cross-form
// request), or new content of the current form (changed), an unsigned
// suite's update after the unsigned refusals (unsignedRefusal). A signature
// found for a suite without a signed current snapshot is marked first
// (Adopter.markSignatureSeen); a failed mark fails the check.
func (c *Checker) anchorFoundAgain(ctx context.Context, out *checkOutcome, cur *cache.SuiteFreshness, suite SuiteRef,
	nowUnix int64, form adoptionForm, t probeTree, signedCurrent bool) *adoptionRequest {
	if !signedCurrent && t.signatureSeen() {
		if err := c.adopter.markSignatureSeen(ctx, suite, nowUnix); err != nil {
			c.failCheck(ctx, out, cur, suite, nowUnix, formName(form), fmt.Errorf("mark the signature seen: %w", err))
			return nil
		}
	}
	snap, _ := c.cache.GetSuiteSnapshot(ctx, *cur.CurrentSnapshotID)
	same := t.sameAs(snap)
	req := t.request(suite)
	req.crossForm = t.form != form
	refusal := ""
	if !same && t.form == adoptionFormUnsigned {
		var err error
		if refusal, err = c.unsignedRefusal(ctx, req); err != nil {
			c.failCheck(ctx, out, cur, suite, nowUnix, formName(form), err)
			return nil
		}
	}
	prevSuccess := cur.LastSuccessAt
	cur.LastCheckAt, cur.LastSuccessAt, cur.CheckFailCount = &nowUnix, &nowUnix, 0
	e := checkEnd{form: formName(t.form), status: 200, signedCurrent: signedCurrent}
	switch {
	case same:
		cur.InReleaseChangeSeenAt = nil
		e.result, e.clearRecord, req = "unchanged", true, nil
	case refusal != "":
		setChangeSeen(cur, nowUnix)
		e.result, e.reason, req = "changed_deferred", refusal, nil
	case t.form == form:
		setChangeSeen(cur, nowUnix)
		req, e.result = c.admit(out, cur, req, signedCurrent, prevSuccess, "changed", "changed_queued")
	default:
		setChangeSeen(cur, nowUnix)
		req, e.result = c.admit(out, cur, req, signedCurrent, prevSuccess, "form_changed", "changed_queued")
	}
	c.finishCheck(ctx, out, cur, suite, e)
	return req
}

// lateralInline is spec C4 Dispatch 1 for a detached current snapshot:
// InRelease is probed on every check, and present → an inline adoption
// request (a lateral move, result form_changed; a cross-form request). An
// absent or unknown InRelease, or the failing content before its retry
// time (held), leaves the check to the detached path (done = false): the
// current form's own request decides the result. The caller holds the host
// slot.
//
// AIDEV-NOTE: held must stop the detached path's "unchanged" from
// clearing the record: it belongs to the InRelease content, which
// upstream still serves, and clearing it would let every other check
// attempt that content again (spec C7's bounded attempts). The record
// keeps no validators (crossForm), so the detached path's GET carries the
// current Release's own (design review I-1).
func (c *Checker) lateralInline(ctx, fetchCtx context.Context, out *checkOutcome, cur *cache.SuiteFreshness, suite SuiteRef,
	urls anchorURLs, gate retryGate, signedCurrent bool, nowUnix int64) (req *adoptionRequest, done, held bool) {
	o := c.probe(fetchCtx, suite, urls, "InRelease")
	if o.class != classPresent {
		return nil, false, false
	}
	req = probeTree{form: adoptionFormInline, found: true, inRelease: o}.request(suite)
	req.crossForm = true
	if gate.active && !gate.due && req.key == gate.hash {
		return nil, false, true
	}
	prevSuccess := cur.LastSuccessAt
	cur.LastCheckAt, cur.LastSuccessAt, cur.CheckFailCount = &nowUnix, &nowUnix, 0
	setChangeSeen(cur, nowUnix)
	e := checkEnd{form: "inline", status: 200, signedCurrent: signedCurrent}
	req, e.result = c.admit(out, cur, req, signedCurrent, prevSuccess, "form_changed", "changed_queued")
	c.finishCheck(ctx, out, cur, suite, e)
	return req, true, false
}

// upgradeProbe is the first half of spec C4 Dispatch 2 (current snapshot
// unsigned): InRelease present → an inline request; else Release.gpg
// present → Release (no validators) and a detached request — an upgrade,
// result form_changed (changed_deferred while the gate holds that content,
// changed_queued without a slot), a cross-form request. A signature found
// marks the suite's base before anything else (Adopter.markSignatureSeen,
// design review I-2: the suite's clients never fetch one, so only this
// probe can record that the suite was seen signed; a failed mark fails the
// check). Both absent → done = false: the unsigned check goes on. A probe
// that ends unknown is a failed check: a signature may be there, so the
// unsigned path does not run. The caller holds the host slot.
func (c *Checker) upgradeProbe(ctx, fetchCtx context.Context, out *checkOutcome, cur *cache.SuiteFreshness, suite SuiteRef,
	urls anchorURLs, gate retryGate, nowUnix int64) (req *adoptionRequest, done bool) {
	fail := func(name string, o probeOutcome) (*adoptionRequest, bool) {
		if !o.cancelled() {
			c.failCheck(ctx, out, cur, suite, nowUnix, "unsigned", fmt.Errorf("%s (%s): %w", name, o.class, o.err))
		}
		return nil, true
	}
	t := probeTree{found: true, inRelease: c.probe(fetchCtx, suite, urls, "InRelease")}
	switch t.inRelease.class {
	case classPresent:
		t.form = adoptionFormInline
	case classUnknown:
		return fail("InRelease", t.inRelease)
	default:
		if t.sig = c.probe(fetchCtx, suite, urls, "Release.gpg"); t.sig.class == classAbsent {
			return nil, false
		} else if t.sig.class == classUnknown {
			return fail("Release.gpg", t.sig)
		}
		if t.release = c.probe(fetchCtx, suite, urls, "Release"); t.release.class != classPresent {
			if t.release.err == nil {
				t.release.err = errors.New("not a Release")
			}
			return fail("Release", t.release)
		}
		t.form = adoptionFormDetached
	}
	if err := c.adopter.markSignatureSeen(ctx, suite, nowUnix); err != nil {
		c.failCheck(ctx, out, cur, suite, nowUnix, "unsigned", fmt.Errorf("mark the signature seen: %w", err))
		return nil, true
	}
	req = t.request(suite)
	req.crossForm = true
	prevSuccess := cur.LastSuccessAt
	cur.LastCheckAt, cur.LastSuccessAt, cur.CheckFailCount = &nowUnix, &nowUnix, 0
	setChangeSeen(cur, nowUnix)
	e := checkEnd{form: formName(t.form), status: 200}
	if gate.active && !gate.due && req.key == gate.hash {
		e.result, e.reason, req = "changed_deferred", gate.reason, nil
	} else {
		req, e.result = c.admit(out, cur, req, false, prevSuccess, "form_changed", "changed_queued")
	}
	c.finishCheck(ctx, out, cur, suite, e)
	return req, true
}

// checkUnsigned is spec C4 Dispatch 2 (current snapshot unsigned): the
// upgrade probes (adopter wired), then a conditional GET of Release
// against the snapshot's Release hash; changed → an unsigned adoption
// request — no re-confirmation (spec C5 safeguard 2: updates of an
// established unsigned suite adopt at once), but the unsigned refusals
// apply before a slot is taken (unsignedRefusal: signed history refuses
// it as unsigned_downgrade_refused, a member count over the cap as
// unsigned_too_large) and the knob (off: a failed check). A reaped Release
// anchor is reconstructed from the snapshot
// (TestCheck_UnsignedRecoversWhenReleaseAnchorMissing). Without an adopter
// it only observes, like the other forms.
func (c *Checker) checkUnsigned(ctx, fetchCtx context.Context, out *checkOutcome, cur *cache.SuiteFreshness, suite SuiteRef,
	now time.Time, nowUnix int64) (*adoptionRequest, *repairRequest) {
	scheme, host, suitePath := suite.CanonicalScheme, suite.CanonicalHost, suite.SuitePath
	releasePath := suitePath + releaseFilename
	row, err := c.cache.LookupURL(ctx, scheme, host, releasePath)
	switch {
	case errors.Is(err, cache.ErrNotFound):
		h := c.snapshotAnchorHash(ctx, *cur.CurrentSnapshotID, releaseFilename)
		row = c.reconstructAnchor(ctx, scheme, host, suitePath, releasePath, *cur.CurrentSnapshotID, h, nowUnix)
	case err != nil:
		c.logger.Warn("freshness: lookup Release url_path failed",
			"err", err, "canonical_host", host, "suite_path", suitePath)
		return nil, nil
	}
	cachedHash, _ := c.baseline(ctx, cur, suite, row, "Release")
	if cachedHash == "" {
		return nil, nil
	}
	gate := c.retryGateFor(cur, now)
	c.applyClamp(ctx, cur, gate, now)

	release, err := c.hostSem.Acquire(fetchCtx, host)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			c.logger.Debug("freshness: host limiter acquire aborted",
				"err", err, "canonical_host", host, "suite_path", suitePath)
			return nil, nil
		}
		c.failCheck(ctx, out, cur, suite, nowUnix, "unsigned", err)
		return nil, nil
	}
	defer release()

	if c.adopter != nil {
		urls := c.anchorURLsFor(suite, map[string]string{"Release": row.UpstreamURL})
		if req, done := c.upgradeProbe(ctx, fetchCtx, out, cur, suite, urls, gate, nowUnix); done {
			return req, nil
		}
	}

	etag, lastmod := gate.validators(cur)
	res, err := c.fetcher.Conditional(fetchCtx, &fetch.Target{CanonicalHost: host, URL: row.UpstreamURL}, etag, lastmod, c.maxBody)
	if c.adopter != nil {
		if o := classify(res, err, sniffRelease, etag != "" || lastmod != ""); o.class == classAbsent {
			return c.anchorAbsent(ctx, fetchCtx, out, cur, suite, now, nowUnix, adoptionFormUnsigned, o, gate, false), nil
		}
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			c.logger.Debug("freshness: check cancelled", "canonical_host", host, "suite_path", suitePath)
			return nil, nil
		}
		c.failCheck(ctx, out, cur, suite, nowUnix, "unsigned", err)
		return nil, nil
	}
	if res.Status != 200 && res.Status != 304 {
		c.failCheck(ctx, out, cur, suite, nowUnix, "unsigned", fmt.Errorf("%w %d", errUnexpectedStatus, res.Status))
		return nil, nil
	}

	newHash := bodyHash(res)
	var req *adoptionRequest
	e := checkEnd{form: "unsigned", status: res.Status}
	key := contentKey(adoptionFormUnsigned, nil, res.Body, nil)
	steady := res.Status == 304 || newHash == cachedHash
	deferred := !steady && gate.active && !gate.due && key == gate.hash
	if !steady && !deferred && c.adopter != nil {
		refusal, err := c.unsignedRefusal(ctx, &adoptionRequest{suite: suite, key: key, releaseBytes: res.Body,
			etag: res.ETag, lastmod: res.LastModified})
		if err != nil {
			c.failCheck(ctx, out, cur, suite, nowUnix, "unsigned", err)
			return nil, nil
		}
		if refusal != "" {
			deferred, gate.reason = true, refusal
		}
	}
	prevSuccess := cur.LastSuccessAt
	cur.LastCheckAt, cur.LastSuccessAt, cur.CheckFailCount = &nowUnix, &nowUnix, 0
	switch result, clear, ok := steadyResult(cur, res, newHash, cachedHash, gate, nowUnix); {
	case ok:
		e.result, e.clearRecord = result, clear
		if result == "changed_deferred" {
			e.reason = gate.reason
		}
	case deferred:
		setChangeSeen(cur, nowUnix)
		e.result, e.reason = "changed_deferred", gate.reason
	default:
		setChangeSeen(cur, nowUnix)
		req = &adoptionRequest{suite: suite, form: adoptionFormUnsigned, key: key, releaseBytes: res.Body,
			etag: res.ETag, lastmod: res.LastModified}
		req, e.result = c.admit(out, cur, req, false, prevSuccess, "changed", "changed_queued")
	}
	return req, c.finishCheck(ctx, out, cur, suite, e)
}
```

Apply the dispatch, pacing, validators, absence, refusal, stale-WARN, repair and reconcile changes — `failCheck`'s second AIDEV-NOTE and the metrics inventory's line for `acu_repair_deferred_total` (deferred minor B-31) included (`git apply`):

```diff
diff --git a/internal/freshness/adoption_failure.go b/internal/freshness/adoption_failure.go
index 21987ef..6a37587 100644
--- a/internal/freshness/adoption_failure.go
+++ b/internal/freshness/adoption_failure.go
@@ -70,4 +70,9 @@ func (c *Checker) recordAdoptionFailure(req *adoptionRequest, reason string) {
 		NextRetryAt: next, ETag: nonEmpty(req.etag), LastMod: nonEmpty(req.lastmod),
 	}
+	if req.crossForm {
+		// Another anchor file's validators: never sent on the current
+		// form's GET (design review I-1, retryGate.validators).
+		f.ETag, f.LastMod = nil, nil
+	}
 	if err := c.cache.RecordAdoptionFailure(ctx, s.CanonicalScheme, s.CanonicalHost, s.SuitePath, f); err != nil {
 		c.logger.Warn("adoption_failure_record_failed",
diff --git a/internal/freshness/check.go b/internal/freshness/check.go
index 33c5c8f..7597646 100644
--- a/internal/freshness/check.go
+++ b/internal/freshness/check.go
@@ -130,4 +130,14 @@ func (c *Checker) checkLocked(ctx, fetchCtx context.Context, out *checkOutcome,
 		return
 	}
+	// Ruling PE-2: an adopted suite in the refusal state anchor_absent is
+	// paced by its record: no request before the record's retry time.
+	if c.adopter != nil {
+		if gate := c.retryGateFor(cur, now); pacedAbsent(gate) {
+			c.applyClamp(ctx, cur, gate, now)
+			c.logger.Debug("freshness: anchor_absent suite paced until its retry",
+				"canonical_host", host, "suite_path", suitePath)
+			return
+		}
+	}
 
 	// Form preference: if there's a current snapshot, the form is its
@@ -136,7 +146,10 @@ func (c *Checker) checkLocked(ctx, fetchCtx context.Context, out *checkOutcome,
 	// fallback on 404.
 	form := c.detectForm(ctx, cur)
-	if form == adoptionFormDetached {
+	switch form {
+	case adoptionFormUnsigned:
+		out.req, out.repair = c.checkUnsigned(ctx, fetchCtx, out, cur, suite, now, nowUnix)
+	case adoptionFormDetached:
 		out.req, out.repair = c.checkDetached(ctx, fetchCtx, out, cur, suite, now, nowUnix)
-	} else {
+	default:
 		req, repair, fellBack := c.checkInline(ctx, fetchCtx, out, cur, suite, now, nowUnix)
 		if fellBack {
@@ -151,11 +164,9 @@ func (c *Checker) checkLocked(ctx, fetchCtx context.Context, out *checkOutcome,
 
 // detectForm reads the suite's current snapshot (if any) and returns
-// its anchor_form (snap.Form): detached, or inline for anything else
-// until phase E dispatches the unsigned forms. First-ever suites
-// (cur.CurrentSnapshotID == nil) and snapshots that fail to load
-// default to inline; the inline-path fallback handles the boot-strap
-// case for detached-only upstreams.
-//
-// AIDEV-TODO(phase E): unsigned form dispatch.
+// its anchor_form (snap.Form): inline, detached or unsigned. First-ever
+// suites (cur.CurrentSnapshotID == nil; only without an adopter, see
+// checkFirst) and snapshots that fail to load default to inline; the
+// inline-path fallback handles the boot-strap case for detached-only
+// upstreams.
 func (c *Checker) detectForm(ctx context.Context, cur *cache.SuiteFreshness) adoptionForm {
 	if cur.CurrentSnapshotID == nil {
@@ -163,9 +174,6 @@ func (c *Checker) detectForm(ctx context.Context, cur *cache.SuiteFreshness) ado
 	}
 	snap, err := c.cache.GetSuiteSnapshot(ctx, *cur.CurrentSnapshotID)
-	if err != nil || snap == nil {
-		return adoptionFormInline
-	}
-	if snap.Form == cache.FormDetached {
-		return adoptionFormDetached
+	if form, ok := formOfSnapshot(snap); err == nil && ok {
+		return form
 	}
 	return adoptionFormInline
@@ -219,5 +227,9 @@ func (c *Checker) finishCheck(ctx context.Context, out *checkOutcome, cur *cache
 	c.logger.Info("freshness_check", logAttrs...)
 	freshnessCheckTotal.Inc(e.result, host)
-	return repairIfUnchanged(e.result, suite, cur)
+	rp := repairIfUnchanged(e.result, suite, cur)
+	if rp != nil {
+		rp.signed = e.signedCurrent
+	}
+	return rp
 }
 
@@ -233,8 +245,11 @@ func (c *Checker) finishCheck(ctx context.Context, out *checkOutcome, cur *cache
 // AIDEV-NOTE: ctx is the caller's context, never fetchCtx: the deadline
 // that ended the check must not fail the write that records it.
-// AIDEV-NOTE: in phase B every Conditional error extends the streak,
-// including 401/403/404/410, which spec C4 classifies as absent, not
-// unknown. Phase E's probe classification must reset the streak on
-// absent, or anchor_absent suites inherit the hourly backoff.
+// AIDEV-NOTE: with an adopter wired, classify sends 401/403/404/410 and a
+// 200 failing its sniff to absent before this point (ruling R-E4: an
+// absent anchor resets the streak, so an anchor_absent suite never
+// inherits the hourly backoff); only unknown ends and checks that find
+// nothing they may adopt (no anchor at all; an unsigned candidate while
+// adoption.adopt_unsigned is off) fail here. Without an adopter (Phase 1)
+// every Conditional error still extends the streak, as before phase E.
 func (c *Checker) failCheck(ctx context.Context, out *checkOutcome, cur *cache.SuiteFreshness, suite SuiteRef, nowUnix int64, form string, err error) {
 	host, suitePath := suite.CanonicalHost, suite.SuitePath
@@ -263,9 +278,10 @@ func (c *Checker) failCheck(ctx context.Context, out *checkOutcome, cur *cache.S
 var errUnexpectedStatus = errors.New("unexpected success status")
 
-// steadyResult handles, for both signed forms (ruling R-E3: one 304/
-// unchanged helper), the responses that say upstream still serves what
-// the suite has: a 304 — which, while a failure record's validators were
-// sent, says upstream still serves the failing content (spec C7
-// changed_deferred) — and a 200 carrying the current anchor's bytes
+// steadyResult handles, for every form (ruling R-E3: one 304/unchanged
+// helper), the responses that say upstream still serves what the suite
+// has: a 304 — which, when the failure record's own validators were sent
+// (retryGate.recordValidators), says upstream still serves the failing
+// content (spec C7 changed_deferred), and otherwise that it still serves
+// the current anchor — and a 200 carrying the current anchor's bytes
 // (newHash == cachedHash), whose validators are taken (the only response
 // they are ever taken from, spec C4) and which clears a failure record
@@ -273,5 +289,5 @@ var errUnexpectedStatus = errors.New("unexpected success status")
 func steadyResult(cur *cache.SuiteFreshness, res *fetch.ConditionalResult, newHash, cachedHash string, gate retryGate, nowUnix int64) (result string, clearRecord, ok bool) {
 	switch {
-	case res.Status == 304 && gate.active:
+	case res.Status == 304 && gate.recordValidators():
 		setChangeSeen(cur, nowUnix)
 		return "changed_deferred", false, true
@@ -468,4 +484,9 @@ func (c *Checker) checkInline(ctx, fetchCtx context.Context, out *checkOutcome,
 
 	res, err := c.fetcher.Conditional(fetchCtx, target, etag, lastmod, c.maxBody)
+	if c.adopter != nil && cur.CurrentSnapshotID != nil {
+		if o := classify(res, err, sniffInRelease, etag != "" || lastmod != ""); o.class == classAbsent {
+			return c.anchorAbsent(ctx, fetchCtx, out, cur, suite, now, nowUnix, adoptionFormInline, o, gate, signedCurrent), nil, false
+		}
+	}
 	if err != nil {
 		if errors.Is(err, context.Canceled) {
@@ -641,5 +662,20 @@ func (c *Checker) checkDetached(ctx, fetchCtx context.Context, out *checkOutcome
 	defer releaseSlot()
 
+	lateralHeld := false
+	if c.adopter != nil && cur.CurrentSnapshotID != nil {
+		urls := c.anchorURLsFor(suite, map[string]string{"Release": releaseURL.UpstreamURL})
+		req, done, held := c.lateralInline(ctx, fetchCtx, out, cur, suite, urls, gate, signedCurrent, nowUnix)
+		if done {
+			return req, nil
+		}
+		lateralHeld = held
+	}
+
 	res, err := c.fetcher.Conditional(fetchCtx, target, etag, lastmod, c.maxBody)
+	if c.adopter != nil && cur.CurrentSnapshotID != nil {
+		if o := classify(res, err, sniffRelease, etag != "" || lastmod != ""); o.class == classAbsent {
+			return c.anchorAbsent(ctx, fetchCtx, out, cur, suite, now, nowUnix, adoptionFormDetached, o, gate, signedCurrent), nil
+		}
+	}
 	if err != nil {
 		if errors.Is(err, context.Canceled) {
@@ -668,6 +704,7 @@ func (c *Checker) checkDetached(ctx, fetchCtx context.Context, out *checkOutcome
 		// A 304 while a failure record exists: Release still matches the
 		// failing content's validators, and a re-signature behind it waits
-		// for the retry (spec C7).
-		e.result, e.clearRecord = result, clear
+		// for the retry (spec C7). A record the lateral InRelease still
+		// holds is not cleared by the detached anchor's "unchanged".
+		e.result, e.clearRecord = result, clear && !lateralHeld
 	} else {
 		setChangeSeen(cur, nowUnix)
@@ -680,5 +717,21 @@ func (c *Checker) checkDetached(ctx, fetchCtx context.Context, out *checkOutcome
 		}
 		gpgRes, gerr := c.fetcher.Conditional(fetchCtx, gpgTarget, "", "", defaultMaxReleaseGPGBytes)
+		var sig probeOutcome
+		if c.adopter != nil && cur.CurrentSnapshotID != nil {
+			sig = classify(gpgRes, gerr, sniffReleaseGPG, false)
+		}
 		switch {
+		case sig.class == classAbsent:
+			// Design review M-14: the changed Release has no signature —
+			// absent (ruling R-E4), never adopted. The refusal state
+			// anchor_absent makes it visible and paces the suite
+			// (pacedAbsent); a record the lateral InRelease still holds
+			// keeps its own schedule. last_success_at stays, as for an
+			// absent current anchor (spec C4).
+			if !lateralHeld {
+				c.recordAdoptionFailure(&adoptionRequest{suite: suite, key: anchorAbsentKey}, cache.ReasonAnchorAbsent)
+			}
+			cur.LastSuccessAt = prevSuccess
+			e.result, e.reason, e.status = "anchor_absent", cache.ReasonAnchorAbsent, sig.status
 		case gerr != nil || gpgRes.Status != 200:
 			// As before: record the change and retry on a later check;
diff --git a/internal/freshness/first_adoption.go b/internal/freshness/first_adoption.go
index 9a60a77..825b351 100644
--- a/internal/freshness/first_adoption.go
+++ b/internal/freshness/first_adoption.go
@@ -298,4 +298,27 @@ func (c *Checker) checkFirst(ctx, fetchCtx context.Context, out *checkOutcome, c
 }
 
+// unsignedRefusal applies spec C5's refusals to an unsigned request before
+// a slot is taken — the first adoption's (unsignedFirst) and an unsigned
+// suite's update (checkUnsigned, anchorFoundAgain): never-downgrade
+// (Adopter.unsignedEligible's signed history: unsigned_downgrade_refused)
+// and the member cap (unsignedTooLarge: unsigned_too_large; design review
+// M-5). It records the refusal and returns its reason, "" to go on, or an
+// error — the knob off, a read error — for which the caller fails the
+// check.
+func (c *Checker) unsignedRefusal(ctx context.Context, req *adoptionRequest) (string, error) {
+	switch err := c.adopter.unsignedEligible(ctx, req.suite); {
+	case errors.Is(err, ErrAdoptionUnsignedDowngradeRefused):
+		c.recordAdoptionFailure(req, cache.ReasonUnsignedDowngradeRefused)
+		return cache.ReasonUnsignedDowngradeRefused, nil
+	case err != nil:
+		return "", err
+	}
+	if err := unsignedTooLarge(req.releaseBytes); err != nil {
+		c.recordAdoptionFailure(req, cache.ReasonUnsignedTooLarge)
+		return cache.ReasonUnsignedTooLarge, nil
+	}
+	return "", nil
+}
+
 // unsignedFirst applies spec C5's safeguards to an unsigned candidate of
 // a never-adopted suite, before a slot is taken: the knob (off: an error,
@@ -321,14 +344,6 @@ func (c *Checker) checkFirst(ctx, fetchCtx context.Context, out *checkOutcome, c
 // confirmation.
 func (c *Checker) unsignedFirst(ctx context.Context, cur *cache.SuiteFreshness, req *adoptionRequest, t probeTree, gate retryGate, now time.Time) (proceed bool, reason string, err error) {
-	switch err := c.adopter.unsignedEligible(ctx, req.suite); {
-	case errors.Is(err, ErrAdoptionUnsignedDowngradeRefused):
-		c.recordAdoptionFailure(req, cache.ReasonUnsignedDowngradeRefused)
-		return false, cache.ReasonUnsignedDowngradeRefused, nil
-	case err != nil:
-		return false, "", err
-	}
-	if err := unsignedTooLarge(req.releaseBytes); err != nil {
-		c.recordAdoptionFailure(req, cache.ReasonUnsignedTooLarge)
-		return false, cache.ReasonUnsignedTooLarge, nil
+	if refusal, err := c.unsignedRefusal(ctx, req); err != nil || refusal != "" {
+		return false, refusal, err
 	}
 	sameContent := gate.active && gate.hash == req.key
diff --git a/internal/freshness/freshness.go b/internal/freshness/freshness.go
index 27ae44d..6ed48cb 100644
--- a/internal/freshness/freshness.go
+++ b/internal/freshness/freshness.go
@@ -335,4 +335,11 @@ type adoptionRequest struct {
 	// a failure is recorded under it.
 	key string
+
+	// crossForm marks a request whose form is not the suite's current
+	// form (a lateral move, an unsigned suite's upgrade, the probe tree of
+	// an absent anchor): its validators belong to another anchor file than
+	// the one the check conditional-GETs, so a failure record keeps none
+	// (design review I-1). A successful adoption still stores them.
+	crossForm bool
 }
 
@@ -348,4 +355,8 @@ type repairRequest struct {
 	suite      SuiteRef
 	snapshotID int64
+	// signed: the current snapshot is signed, so the repair may take the
+	// slot the reserved-slot rule keeps for signed suites (spec C4;
+	// overview "E:" carry-forward).
+	signed bool
 }
 
@@ -626,4 +637,11 @@ func (c *Checker) spawnAdoption(mu *sync.Mutex, req *adoptionRequest, l *lease)
 // suites (the steady state) never compete with adoptions for slots. The
 // slot is still taken without waiting and before spawning.
+// AIDEV-NOTE: the reserved-slot rule follows the suite (rp.signed: its
+// current snapshot is signed), so a signed suite's repair may take the
+// last free slot and hold it for up to adoption.max_duration if a member
+// stalls (deferred minor B-32, weighed in phase E: kept — spec C4 reserves
+// the slot for suites with a signed current snapshot, not for adoptions,
+// and the per-host slot keeps one such repair per host). A deferred
+// repair is counted (acu_repair_deferred_total, B-31).
 func (c *Checker) planRepair(ctx context.Context, out *checkOutcome) {
 	rp := out.repair
@@ -639,5 +657,5 @@ func (c *Checker) planRepair(ctx context.Context, out *checkOutcome) {
 		c.logger.Warn("adoption_reconcile_failed",
 			"canonical_host", rp.suite.CanonicalHost, "suite_path", rp.suite.SuitePath,
-			"snapshot_id", rp.snapshotID, "err", err)
+			"snapshot_id", rp.snapshotID, "stage", "decision", "err", err)
 	}
 	out.needReconcile = needReconcile
@@ -646,8 +664,9 @@ func (c *Checker) planRepair(ctx context.Context, out *checkOutcome) {
 		return
 	}
-	l, ok := c.admission.tryAcquire(rp.suite.CanonicalHost, true)
+	l, ok := c.admission.tryAcquire(rp.suite.CanonicalHost, rp.signed)
 	if !ok {
 		c.logger.Debug("freshness: repair deferred, no adoption slot",
 			"canonical_host", rp.suite.CanonicalHost, "suite_path", rp.suite.SuitePath)
+		repairDeferredTotal.Inc(rp.suite.CanonicalHost)
 		out.repair = nil
 		return
@@ -680,4 +699,10 @@ func (c *Checker) planRepair(ctx context.Context, out *checkOutcome) {
 // AIDEV-NOTE: defer order is load-bearing: Done (last), Unlock, lease
 // release, recover (first). See recoverGoroutine.
+// AIDEV-NOTE: a repair or reconcile that panics deterministically is
+// logged (freshness_check_panicked, where=repair), not recorded: the spec
+// C7 record is keyed on upstream content, and an unchanged suite's heal
+// has none, so the next unchanged check runs it again — one ERROR line
+// per cooldown at most (deferred minor B-37, phase B deviation 7; kept in
+// phase E: v7 has no record keyed on a snapshot).
 func (c *Checker) spawnRepair(mu *sync.Mutex, out *checkOutcome) {
 	rp, l, needRepair, needReconcile := out.repair, out.lease, out.needRepair, out.needReconcile
@@ -700,5 +725,5 @@ func (c *Checker) spawnRepair(mu *sync.Mutex, out *checkOutcome) {
 				c.logger.Warn("adoption_reconcile_failed",
 					"canonical_host", rp.suite.CanonicalHost, "suite_path", rp.suite.SuitePath,
-					"snapshot_id", rp.snapshotID, "err", err)
+					"snapshot_id", rp.snapshotID, "stage", "heal", "err", err)
 			}
 		}
@@ -1022,12 +1047,29 @@ func (c *Checker) retryGateFor(cur *cache.SuiteFreshness, now time.Time) retryGa
 }
 
-// validators returns what the conditional GET sends: the current
-// anchor's validators with no record, the failing content's before
-// next_retry_at, none at or after it.
+// recordValidators reports whether the conditional GET sends the failure
+// record's validators: a record is active, not due, and holds validators —
+// the failing content's, from a request of the current form (a cross-form
+// request records none).
+func (g retryGate) recordValidators() bool {
+	return g.active && !g.due && (g.etag != "" || g.lastmod != "")
+}
+
+// validators returns what the conditional GET sends: none at or after the
+// record's next_retry_at; the failing content's before it when the record
+// holds validators; otherwise — no record, or a record without validators —
+// the current anchor's own.
+//
+// AIDEV-NOTE: design review I-1. A record's validators are those of the
+// anchor file its request fetched; sent on another file's GET they can
+// answer 304 for content never seen (a detached suite's Release behind a
+// CDN that sends Last-Modified only, after its lateral InRelease failed),
+// which froze the suite. Cross-form requests therefore record none, and a
+// 304 is changed_deferred only when the record's own were sent
+// (steadyResult).
 func (g retryGate) validators(cur *cache.SuiteFreshness) (etag, lastmod string) {
 	switch {
 	case g.active && g.due:
 		return "", ""
-	case g.active:
+	case g.recordValidators():
 		return g.etag, g.lastmod
 	}
@@ -1122,5 +1164,12 @@ func (c *Checker) Reconcile(ctx context.Context, scheme, host, suitePath string)
 		return false
 	}
-	l, ok := c.admission.tryAcquire(host, true)
+	// The reserved-slot rule (spec C4): only a signed current snapshot may
+	// take the last free slot; a snapshot that cannot be read counts as
+	// unsigned.
+	signed := false
+	if snap, err := c.cache.GetSuiteSnapshot(ctx, snapID); err == nil {
+		signed = snap.Form != cache.FormUnsigned
+	}
+	l, ok := c.admission.tryAcquire(host, signed)
 	if !ok {
 		mu.Unlock()
@@ -1140,5 +1189,5 @@ func (c *Checker) Reconcile(ctx context.Context, scheme, host, suitePath string)
 			c.logger.Warn("adoption_reconcile_failed",
 				"canonical_host", host, "suite_path", suitePath,
-				"snapshot_id", snapID, "err", err)
+				"snapshot_id", snapID, "stage", "on_demand", "err", err)
 		}
 	}()
diff --git a/internal/freshness/metrics.go b/internal/freshness/metrics.go
index f66d9a1..c064827 100644
--- a/internal/freshness/metrics.go
+++ b/internal/freshness/metrics.go
@@ -33,5 +33,6 @@ import (
 // (spec C7, unlabelled gauge); acu_freshness_waiting_for_slot (spec C4,
 // unlabelled gauge); acu_freshness_unlogged_panics_total (deferred minor
-// B-44, unlabelled counter). The flip barrier's acu_flip_hold_skipped_total,
+// B-44, unlabelled counter); acu_repair_deferred_total (deferred minor
+// B-31, counter by host). The flip barrier's acu_flip_hold_skipped_total,
 // acu_flip_hold_expired_total, acu_flip_quiet_cut_short_total,
 // acu_flip_waiters_over_cap_total, acu_flip_slow_anchor_reads_total and
@@ -61,4 +62,15 @@ var (
 	)
 
+	// repairDeferredTotal counts repairs and reconciles of an unchanged
+	// suite that found no free admission slot and wait for its next
+	// unchanged check (deferred minor B-31: the DEBUG line alone was
+	// invisible).
+	repairDeferredTotal = metrics.NewCounterWithCap(
+		"acu_repair_deferred_total",
+		"Repairs and reconciles deferred because no adoption slot was free, by host (spec C4).",
+		metrics.DefaultMaxSeries,
+		"host",
+	)
+
 	// freshnessStaleSuites is the per-tick gauge of suites whose
 	// last_success_at has aged past staleSuiteFactor*refresh (SPEC §7.4
diff --git a/internal/freshness/scheduler.go b/internal/freshness/scheduler.go
index 9108215..706cf58 100644
--- a/internal/freshness/scheduler.go
+++ b/internal/freshness/scheduler.go
@@ -29,10 +29,12 @@ const staleSuiteFactor = 4
 // staleSuiteCounts partitions suites whose last_success_at has aged past
 // staleBefore into adopted (current_snapshot_id set — the dangerous case
-// that serves stale metadata) and unadopted. A nil last_success_at is
-// treated as "never succeeded" rather than "went stale" and is not
-// counted, so brand-new suites don't trip the gauge. A last_success_at
-// more than clockStepTolerance past now (backward clock step) counts as
-// stale (spec C4). SPEC §7.4.
-func staleSuiteCounts(suites []cache.SuiteFreshness, staleBefore, now int64) (adopted, unadopted int) {
+// that serves stale metadata) and unadopted, and counts the adopted ones
+// in a refusal state (refused, a subset of adopted: an anchor_absent suite
+// keeps its old last_success_at on purpose, spec C4). A nil
+// last_success_at is treated as "never succeeded" rather than "went
+// stale" and is not counted, so brand-new suites don't trip the gauge. A
+// last_success_at more than clockStepTolerance past now (backward clock
+// step) counts as stale (spec C4). SPEC §7.4.
+func staleSuiteCounts(suites []cache.SuiteFreshness, staleBefore, now int64) (adopted, unadopted, refused int) {
 	futureAfter := now + int64(clockStepTolerance/time.Second)
 	for _, s := range suites {
@@ -45,11 +47,15 @@ func staleSuiteCounts(suites []cache.SuiteFreshness, staleBefore, now int64) (ad
 			continue
 		}
-		if s.CurrentSnapshotID != nil {
-			adopted++
-		} else {
+		switch {
+		case s.CurrentSnapshotID == nil:
 			unadopted++
+		case s.AdoptionFailCount > 0 && s.AdoptionFailReason != nil && cache.IsRefusalReason(*s.AdoptionFailReason):
+			adopted++
+			refused++
+		default:
+			adopted++
 		}
 	}
-	return adopted, unadopted
+	return adopted, unadopted, refused
 }
 
@@ -335,4 +341,14 @@ func (c *Checker) suiteDue(s cache.SuiteFreshness, now time.Time) bool {
 	nowU := now.Unix()
 	future := nowU + int64(clockStepTolerance/time.Second)
+	// Ruling PE-2: an adopted suite in the refusal state anchor_absent is
+	// due only at its record's retry time (pacedAbsent), whatever its
+	// last_success_at. A next_retry_at beyond the legitimate horizon (a
+	// backward clock step) is not paced here: once the suite is due (its
+	// last_success_at is old), the check clamps it (retryGateFor).
+	if c.adopter != nil && s.CurrentSnapshotID != nil && s.AdoptionFailCount > 0 && s.AdoptionFailReason != nil &&
+		*s.AdoptionFailReason == cache.ReasonAnchorAbsent && s.AdoptionNextRetryAt != nil &&
+		*s.AdoptionNextRetryAt > nowU && *s.AdoptionNextRetryAt <= nowU+int64(retryHorizon/time.Second) {
+		return false
+	}
 	stale := s.LastSuccessAt == nil || *s.LastSuccessAt <= now.Add(-c.refresh).Unix()
 	if s.LastSuccessAt != nil && *s.LastSuccessAt > future {
@@ -555,10 +571,18 @@ func (c *Checker) tick(ctx context.Context, q *schedQueues) {
 	// staleSuiteFactor*refresh is the freeze signature.
 	staleBefore := nowT.Add(-staleSuiteFactor * c.refresh).Unix()
-	adoptedStale, unadoptedStale := staleSuiteCounts(suites, staleBefore, nowT.Unix())
+	adoptedStale, unadoptedStale, refusedStale := staleSuiteCounts(suites, staleBefore, nowT.Unix())
 	freshnessStaleSuites.Set(float64(adoptedStale), "true")
 	freshnessStaleSuites.Set(float64(unadoptedStale), "false")
-	if adoptedStale > 0 {
+	// Design review M-13: a suite in a refusal state (anchor_absent keeps
+	// its last_success_at old on purpose) is visible on its own — the
+	// status page, adoption_refused and, after a day,
+	// adoption_refused_persistently — so the WARN leaves it out; the gauge
+	// counts it. Without an adopter records are inert and count as stale.
+	if c.adopter == nil {
+		refusedStale = 0
+	}
+	if warnStale := adoptedStale - refusedStale; warnStale > 0 {
 		c.logger.Warn("freshness: adopted suites stale beyond threshold",
-			"adopted_stale", adoptedStale,
+			"adopted_stale", warnStale,
 			"stale_factor", staleSuiteFactor,
 			"refresh", c.refresh.String(),
```

- [ ] **Step 4: Change the existing expectations**

| Test | Was | Now | Why |
|---|---|---|---|
| `TestCheck_DetachedKnownSuite_AdoptsOnReleaseChange` | `InRelease` never requested | requested (answers 404), then the detached adoption | spec C4 Dispatch 1: a detached suite probes `InRelease` on every check |
| `TestCheck_UnchangedTick_LogsReconcileFailure` | — | also asserts `stage: decision` | deferred minor B-30 |
| `TestCheck_RepairObeysTheGlobalSlot` | — | also asserts `acu_repair_deferred_total{host=b.example}` rose by 1 | deferred minor B-31 |
| `TestStaleSuiteCounts` | two return values | three: also `refused`, 0 for its suites (none has a record) | design review M-13: the stale WARN leaves out suites in a refusal state |

```diff
diff --git a/internal/freshness/capacity_test.go b/internal/freshness/capacity_test.go
index d3ec4d5..43637dc 100644
--- a/internal/freshness/capacity_test.go
+++ b/internal/freshness/capacity_test.go
@@ -212,6 +212,11 @@ func TestCheck_RepairObeysTheGlobalSlot(t *testing.T) {
 	}
 	// B's repair is deferred for want of a slot: read from its line and
-	// the slot count, not a timing window (deferred minor B-29).
+	// the slot count, not a timing window (deferred minor B-29), and
+	// counted (B-31).
+	deferredBefore := hostCounter(t, "acu_repair_deferred_total", hosts[1])
 	chk.Check(ctx, "http", hosts[1], suitePath)
+	if got := hostCounter(t, "acu_repair_deferred_total", hosts[1]) - deferredBefore; got != 1 {
+		t.Fatalf("acu_repair_deferred_total{host=b.example} rose by %v, want 1", got)
+	}
 	deferred := false
 	for _, line := range strings.Split(logs.String(), "\n") {
diff --git a/internal/freshness/freshness_test.go b/internal/freshness/freshness_test.go
index 69512f9..cd0c3cd 100644
--- a/internal/freshness/freshness_test.go
+++ b/internal/freshness/freshness_test.go
@@ -787,5 +787,5 @@ func TestStaleSuiteCounts(t *testing.T) {
 		mk(i(3003), i(7)), // 3 s ahead -> infinitely old: count adopted
 	}
-	adopted, unadopted := staleSuiteCounts(suites, staleBefore, now)
+	adopted, unadopted, refused := staleSuiteCounts(suites, staleBefore, now)
 	if adopted != 4 {
 		t.Errorf("adopted stale = %d, want 4", adopted)
@@ -794,4 +794,7 @@ func TestStaleSuiteCounts(t *testing.T) {
 		t.Errorf("unadopted stale = %d, want 1", unadopted)
 	}
+	if refused != 0 {
+		t.Errorf("refused stale = %d, want 0 (no suite has a record)", refused)
+	}
 }
 
@@ -1412,6 +1415,6 @@ func TestCheck_DetachedKnownSuite_AdoptsOnReleaseChange(t *testing.T) {
 			_, _ = w.Write(sigBody)
 		case "/dists/noble/InRelease":
-			// Detached path must NOT hit InRelease for a known-detached
-			// suite — the form is dispatched up front.
+			// Spec C4 Dispatch 1: a known-detached suite probes InRelease
+			// on every check (a lateral move to inline); absent here.
 			gotInReleaseFetch.Store(true)
 			w.WriteHeader(http.StatusNotFound)
@@ -1496,6 +1499,6 @@ func TestCheck_DetachedKnownSuite_AdoptsOnReleaseChange(t *testing.T) {
 	c.WaitForAdoptions()
 
-	if gotInReleaseFetch.Load() {
-		t.Error("known-detached suite unexpectedly fetched InRelease")
+	if !gotInReleaseFetch.Load() {
+		t.Error("known-detached suite did not probe InRelease")
 	}
 	if releaseHits.Load() != 1 {
@@ -2122,5 +2125,5 @@ func TestCheck_UnchangedTick_LogsReconcileFailure(t *testing.T) {
 		found = true
 		errText, _ := rec["err"].(string)
-		if rec["level"] != "WARN" || rec["canonical_host"] != "127.0.0.1" ||
+		if rec["level"] != "WARN" || rec["stage"] != "decision" || rec["canonical_host"] != "127.0.0.1" ||
 			rec["suite_path"] != "/ubuntu/dists/noble" || rec["snapshot_id"] != float64(snapID) ||
 			!strings.Contains(errText, "not a valid clearsigned message") {
```

- [ ] **Step 5: Run the tests**

Run: `go test -race ./internal/freshness/ ./internal/handler/ ./internal/cache/ ./internal/admin/ ./cmd/...`
Expected: PASS.

- [ ] **Step 6: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| `lateralInline` never requests (`if true \|\| o.class != classPresent`) | `TestCheck_DetachedSuiteMovesToInlineWhenInReleaseAppears`, `TestCheck_LateralFailingContentKeepsItsRecord`, `TestCheck_DetachedSuiteKeepsAdoptingWhileItsLateralInReleaseFails`, `TestCheck_AbsentSignatureKeepsTheLateralRecord` |
| `lateralInline` never reports held content | `TestCheck_AbsentSignatureKeepsTheLateralRecord` |
| `anchorAbsent`: the gap compared with 0 | `TestCheck_AbsentAnchorMovesLaterallyAfterTwoObservations` |
| `anchorAbsent`: a future first observation confirms (deviation 2, M-19) | `TestCheck_AbsentAnchorWithAFutureFirstAtWaitsAnotherGap` |
| `anchorAbsent`: an unsigned candidate counts as a form found (`case t.found:`) | `TestCheck_AbsentAnchorNeverMovesToUnsigned` |
| `anchorAbsent`: an unsigned suite's own form found again is not handled (M-16) | `TestCheck_AbsentUnsignedReleaseThatComesBack/{same_bytes, new_bytes, new_bytes_with_signed_history}` |
| `anchorAbsent`: the streak not reset | `TestCheck_AbsentAnchorMovesLaterallyAfterTwoObservations` |
| `anchorAbsent`: `last_success_at` bumped | `TestCheck_AbsentAnchorMovesLaterallyAfterTwoObservations` |
| `anchorFoundAgain`: `same` never true (X25) | `TestCheck_AbsentAnchorThatComesBackIsUnchanged`, `TestCheck_AbsentUnsignedReleaseThatComesBack/same_bytes` |
| `anchorFoundAgain`: an unsigned update without the refusals | `TestCheck_AbsentUnsignedReleaseThatComesBack/new_bytes_with_signed_history` |
| `anchorFoundAgain`: new content of the same form reported `form_changed` | `TestCheck_AbsentUnsignedReleaseThatComesBack/new_bytes` |
| `anchorFoundAgain`: no marker for a found signature (I-2) | `TestCheck_AbsentUnsignedReleaseTreeFindsASignature` |
| `checkLocked`: no `anchor_absent` pacing (PE-2) | `TestCheck_AbsentAnchorIsPacedByItsRecord`, `TestCheck_DetachedSuiteThatDropsItsSignatureNeverGoesUnsigned/absent` |
| `suiteDue`: no `anchor_absent` pacing | `TestCheck_AbsentAnchorIsPacedByItsRecord` |
| `suiteDue`: the pacing ignores the horizon | `TestCheck_AbsentAnchorIsPacedByItsRecord` |
| `recordAdoptionFailure` keeps a cross-form request's validators (I-1) | `TestCheck_DetachedSuiteKeepsAdoptingWhileItsLateralInReleaseFails`, `TestCheck_LateralFailingContentKeepsItsRecord` |
| `steadyResult`: any 304 with an active record is `changed_deferred` (`gate.active`) | `TestCheck_LateralFailingContentKeepsItsRecord` |
| `validators` sends a record's empty validators (`case g.active:`) | `TestCheck_LateralFailingContentKeepsItsRecord` |
| `checkInline`: the absence test without the sniff | `TestCheck_CurrentAnchorClassification/HTML_200` |
| `checkDetached`: a `Release.gpg` that fails unknown reports `unchanged` (the first `e.result = "changed"`) | `TestCheck_DetachedSuiteThatDropsItsSignatureNeverGoesUnsigned/unknown` |
| `checkDetached`: the signature never classified (`sig = probeOutcome{}`; M-14) | `TestCheck_DetachedSuiteThatDropsItsSignatureNeverGoesUnsigned/absent`, `TestCheck_AbsentSignatureKeepsTheLateralRecord` |
| `checkDetached`: an absent signature takes the lateral content's record (`if true`) | `TestCheck_AbsentSignatureKeepsTheLateralRecord` |
| `checkDetached`: an absent signature bumps `last_success_at` | `TestCheck_DetachedSuiteThatDropsItsSignatureNeverGoesUnsigned/absent` |
| `upgradeProbe`: `InRelease` never present | `TestCheck_UnsignedSuiteUpgradesWhenSigned/InRelease`, `TestCheck_UnsignedSuiteSeenSignedNeverUpdatesUnsigned` |
| `upgradeProbe`: an unknown `InRelease` falls through to the unsigned path | `TestCheck_UnsignedSuiteUpgradesWhenSigned/an_unknown_signature_probe_fails_the_check` |
| `upgradeProbe`: an unknown `Release.gpg` falls through (X06) | `TestCheck_UnsignedSuiteUnknownReleaseGPGFailsTheCheck` |
| `upgradeProbe`: no marker (I-2) | `TestCheck_UnsignedSuiteSeenSignedNeverUpdatesUnsigned/{stripped_after_a_failed_upgrade, failed_marker_write}` |
| `upgradeProbe`: a failed mark ignored | `TestCheck_UnsignedSuiteSeenSignedNeverUpdatesUnsigned/failed_marker_write` |
| `upgradeProbe`: `admit(…, true, …)` (X07) | `TestCheck_UnsignedSuiteNeverTakesTheReservedSlotToAdopt` |
| `checkUnsigned`: no unsigned refusals | `TestCheck_UnsignedSuiteUpdates`, `TestCheck_UnsignedSuiteTooLargeUpdateIsRefused` |
| `unsignedRefusal`: no member cap (M-5) | `TestCheck_UnsignedSuiteTooLargeUpdateIsRefused`, `TestCheckFirst_UnsignedTooLargeNeverTakesASlot` |
| `checkUnsigned`: an unsigned update admitted as signed (X08) | `TestCheck_UnsignedSuiteNeverTakesTheReservedSlotToAdopt` |
| `detectForm` never returns unsigned | `TestCheck_UnsignedSuiteUpgradesWhenSigned`, `TestCheck_UnsignedSuiteUpdates`, `TestCheck_UnsignedRecoversWhenReleaseAnchorMissing` |
| the stale WARN counts refusal states (M-13) | `TestTick_StaleWarnLeavesOutRefusalStates` |
| `staleSuiteCounts` never counts `refused` | `TestStaleSuiteCounts_RefusalStates`, `TestTick_StaleWarnLeavesOutRefusalStates` |
| `Reconcile`: `signed` always true (`signed = snap.Form != cache.FormUnsigned \|\| true`) | `TestReconcile_UnsignedSuiteNeverTakesTheReservedSlot` |
| `planRepair`: `tryAcquire(…, true)` | `TestCheck_RepairOfAnUnsignedSuiteNeverTakesTheReservedSlot/unsigned` |
| `finishCheck`: `rp.signed = true` | `TestFinishCheck_RepairCarriesSignedCurrent`, `TestCheck_RepairOfAnUnsignedSuiteNeverTakesTheReservedSlot/unsigned` |
| `planRepair` without `repairDeferredTotal.Inc` | `TestCheck_RepairObeysTheGlobalSlot` |
| `planRepair`'s `adoption_reconcile_failed` without `stage` | `TestCheck_UnchangedTick_LogsReconcileFailure` |

(`lateralInline`'s held flag is pinned through the lateral record an absent signature must not take: with the current Release's own validators, the held window's detached GET answers 304, which never clears a record.)

- [ ] **Step 7: Commit**

```bash
gofmt -l internal/freshness/current_form.go internal/freshness/current_form_test.go internal/freshness/check.go \
  internal/freshness/freshness.go internal/freshness/metrics.go internal/freshness/adoption_failure.go internal/freshness/first_adoption.go \
  internal/freshness/scheduler.go internal/freshness/retry_rig_test.go internal/freshness/freshness_test.go internal/freshness/capacity_test.go
git add internal/freshness/current_form.go internal/freshness/current_form_test.go internal/freshness/check.go \
  internal/freshness/freshness.go internal/freshness/metrics.go internal/freshness/adoption_failure.go internal/freshness/first_adoption.go \
  internal/freshness/scheduler.go internal/freshness/retry_rig_test.go internal/freshness/freshness_test.go internal/freshness/capacity_test.go
git commit -m "Move adopted suites between anchor forms: lateral moves, anchor_absent, the unsigned upgrade"
```

### Task 8: The scheduler under eager adoption — waiters' signed flag, the upgrade-burst simulation, and the scheduler's deferred minors

The overview's "E:" carry-forwards for slot-driven admission: a parked waiter kept the `signed` flag of the check that parked it, so a waiter parked as a first adoption that a client check has since adopted (signed) waited for two free slots instead of one until its next re-check. The tick now reads each suite's current form (`ListSuitesWithAdoption`, which the admin page already uses; the checker's `Cache` interface swaps `ListSuites` for it) and sets every waiter's flag to "its current snapshot is signed" (`refreshSigned`): within one tick, and never wrong in either direction (design note 25). The capacity simulation gains the upgrade burst: 50 never-adopted suites with interest and a trickle of signed changes, pool of two — first adoptions one at a time and back to back, a signed change waits for at most one adoption, each first-adoption suite costs at most two probes, goroutines bounded. It is deterministic (its signed anchors answer 1 s late, so an offer never races a same-instant signed check) and runs 80 virtual minutes, about 3 s under `-race` (ruling PE-8: under 5 s with margin; the last adoption starts at about 68 minutes). Ruling PE-4 (design review I-5) adds the burst where production has it — `TestSimCapacity_UpgradeBurstSameHost`: 3 hosts × (8 never-adopted + 2 signed) suites, commits that take 2 s, a client checking each never-adopted suite every 5 minutes until it adopts, and no delay ordering the checks, so admissions race. It asserts first adoptions one at a time; a signed change waiting for at most one adoption (its commit included) + D + one host turn; each never-adopted suite's anchor probes before its adoption at most 2 + its client checks + its lost offers — the bound design note 26 now states; and lost offers in all at most the admissions in all (work-conserving). A test-only `Checker.offerHook` (run on the Run goroutine for each finished offer) counts the lost offers. Documenting `max_concurrent_adoptions = 3` as the knob that lets two first adoptions run at once is Task 15's.

Deferred minors in `scheduler.go` and `admission.go`: B-38 (a future `last_success_at` was counted as a clock step on every tick: `suiteDue` now reads it as infinitely old silently and the check that heals it reports it once), B-45 (an offer refused after a slot release it could not see is re-offered at once: a release generation stamps each dispatched item), B-46 (a re-parked waiter returns to its place by a waiting sequence number, so lost or busy offers in a row never invert the order), B-66 (a suite already class-queued as stale that a client check leaves with a pending change keeps the mark for its turn), D-23 (`boundDue`'s "admitted suites keep key order" is pinned).

**Files:**
- Create: `internal/freshness/sched_e_test.go`, `internal/freshness/sim_burst_test.go`
- Modify: `internal/freshness/scheduler.go` (`schedItem`, `schedQueues`, `enqueue`, `park`, `refreshSigned`, `finish`, `pop`, `done`, `Run` (and its `offerHook` call), `withGen`, `suiteDue`, `tick`), `internal/freshness/admission.go` (`releases`, `generation`), `internal/freshness/freshness.go` (the `Cache` interface, `Checker.offerHook`), `internal/freshness/check.go` (`checkLocked` reports a future `last_success_at`), `internal/freshness/freshness_test.go` (`fakeCache.ListSuitesWithAdoption`); `internal/freshness/scheduler_test.go` and `internal/freshness/suite_lock_test.go` only by the mechanical `finish` signature change below (PD-1)

**Interfaces:**
- Consumes: Task 6's first-adoption results, `seedStaleAnchorAt` (Task 5: client interest and a client-created anchor); `cache.(*Cache).ListSuitesWithAdoption`, `cache.SuiteWithAdoption.CurrentAnchorForm`; test helpers `simMembers`, `startSched`, `signedRelease`, `trackSuite`, `schedRow`, `admitAll`, `admitNone`, `mustAcquire`, `seedFakeSuite`, `fakeCache.putSnapshot`, `renderedMetric`.
- Produces: `freshness.Cache.ListSuitesWithAdoption(ctx) ([]cache.SuiteWithAdoption, error)` (replaces `ListSuites` in the interface); `(*admission).generation() uint64`; `schedItem.waitSeq`, `schedItem.gen`; `(*schedQueues).refreshSigned(signed map[string]bool)`; `(*schedQueues).finish(d checkDone, gen uint64)`; `withGen(item schedItem, gen uint64) schedItem`; the test seam `Checker.offerHook func(ref SuiteRef, rep checkReport)`. Test helpers `admitSigned` (`sched_e_test.go`), `startsByURL` (`sim_burst_test.go`).

- [ ] **Step 1: Write the failing tests**

Create `internal/freshness/sched_e_test.go`:

```go
package freshness

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
)

// admitSigned admits only signed waiters (one free slot of two).
func admitSigned(_ string, signed bool) bool { return signed }

// parkTwo parks /1 then /2 on host a as waiters (signed as given).
func parkTwo(t *testing.T, q *schedQueues, signed bool) {
	t.Helper()
	one := int64(1)
	for _, p := range []string{"/1", "/2"} {
		q.enqueue(schedRow("a", p, &one, 0), false)
		it, _ := q.pop(admitNone)
		q.finish(checkDone{item: it, report: checkReport{result: "changed_queued", signedCurrent: signed}}, 0)
	}
}

// Deferred minor B-46: lost offers in a row on one host never invert the
// waiters' order: a re-parked waiter goes back to its place (oldest first).
func TestSchedQueues_LostOffersKeepTheWaitersOrder(t *testing.T) {
	q := newSchedQueues()
	parkTwo(t, q, true)
	for _, want := range []string{"/1", "/2"} { // both offers lost, one after the other
		it, _ := q.pop(admitAll)
		if it.ref.SuitePath != want {
			t.Fatalf("offered %s, want %s", it.ref.SuitePath, want)
		}
		q.finish(checkDone{item: it, report: checkReport{result: "changed_queued", signedCurrent: true}}, 0)
	}
	q.unhold()
	if it, _ := q.pop(admitAll); it.ref.SuitePath != "/1" {
		t.Fatalf("after two lost offers the head is %s, want /1 (the oldest)", it.ref.SuitePath)
	}
}

// Deferred minor B-45: an offer whose check found no slot is held unless a
// slot was released after it was dispatched — that release's wake may be
// spent already, so it is offered again at once.
func TestSchedQueues_LostOfferAfterAReleaseIsNotHeld(t *testing.T) {
	for _, tc := range []struct {
		name      string
		gen       uint64
		wantAgain bool
	}{
		{"no release since dispatch", 0, false},
		{"a release since dispatch", 1, true},
	} {
		q := newSchedQueues()
		one := int64(1)
		q.enqueue(schedRow("a", "/1", &one, 0), false)
		it, _ := q.pop(admitNone)
		q.finish(checkDone{item: it, report: checkReport{result: "changed_queued"}}, 0)
		it, _ = q.pop(admitAll) // the offer, dispatched at generation 0
		q.finish(checkDone{item: it, report: checkReport{result: "changed_queued"}}, tc.gen)
		if _, again := q.pop(admitAll); again != tc.wantAgain {
			t.Errorf("%s: offered again at once = %v, want %v", tc.name, again, tc.wantAgain)
		}
	}
}

// The release generation counts every release that frees something.
func TestAdmission_GenerationCountsReleases(t *testing.T) {
	a := newAdmission(2)
	l := mustAcquire(t, a, "h", true)
	l.releaseHost()
	l.releaseHost() // idempotent: frees nothing
	l.release()     // the global slot
	if g := a.generation(); g != 2 {
		t.Fatalf("generation %d, want 2", g)
	}
}

// Deferred minor B-66: a suite already queued as stale that a client
// check then leaves with a pending change bypasses the cooldown at its turn.
func TestSchedQueues_PendingChangeReachesAQueuedSuite(t *testing.T) {
	q := newSchedQueues()
	one := int64(1)
	s := schedRow("a", "/1", &one, 0)
	q.enqueue(s, false)
	if q.enqueue(s, true) {
		t.Fatal("a duplicate was queued")
	}
	it, _ := q.pop(admitAll)
	if !it.pendingChange {
		t.Fatal("the pending change marked on a queued suite was lost")
	}
	q.done(it)
	q.enqueue(s, false)
	if it, _ := q.pop(admitAll); it.pendingChange {
		t.Fatal("a pending mark outlived the suite's turn")
	}
}

// Overview "E:" carry-forward: a waiter parked as a first adoption whose
// suite a client check has since adopted (signed) is admitted as signed
// after the next tick, not only when two slots are free.
func TestTick_RefreshesTheWaitersSignedFlag(t *testing.T) {
	ctx := context.Background()
	fc := newFakeCache()
	recent := time.Unix(1_800_000_000, 0).Add(-time.Hour).Unix()
	seedFakeSuite(fc, "a", "/1", &recent)
	c, err := New(Config{Cache: fc, Fetcher: &changedUpstream{}, HostLimiter: hostsem.New(1), Refresh: time.Hour,
		Adopter: &Adopter{}, Logger: discardLogger(), now: func() time.Time { return time.Unix(1_800_000_000, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	q := newSchedQueues()
	one := int64(1)
	q.enqueue(schedRow("a", "/1", &one, 0), false)
	it, _ := q.pop(admitNone)
	q.finish(checkDone{item: it, report: checkReport{result: "first_adoption_queued"}}, 0)
	if _, ok := q.pop(admitSigned); ok {
		t.Fatal("setup: an unsigned waiter was admitted to the reserved slot")
	}
	id := int64(9)
	fc.putSnapshot(cache.SuiteSnapshot{SnapshotID: id, CanonicalScheme: "http", CanonicalHost: "a", SuitePath: "/1", Form: cache.FormInline})
	s, _ := fc.suite("http", "a", "/1")
	s.CurrentSnapshotID = &id
	fc.putSuite(s)
	c.tick(ctx, q)
	if it, ok := q.pop(admitSigned); !ok || !it.signed {
		t.Fatalf("after the tick: offered %v (%+v), want the now-signed waiter", ok, it)
	}
}

// Deferred minor B-38: the scheduler reads a future last_success_at as
// infinitely old without counting it on every tick; the check reports it.
func TestSuiteDue_FutureSuccessIsNotCountedPerTick(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	future := now.Add(time.Hour).Unix()
	c, err := New(Config{Cache: newFakeCache(), Fetcher: &changedUpstream{}, HostLimiter: hostsem.New(1),
		Refresh: time.Hour, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	steps := func() float64 {
		v, _ := strconv.ParseFloat(renderedMetric("acu_clock_steps_detected_total"), 64)
		return v
	}
	before := steps()
	for i := 0; i < 3; i++ {
		if !c.suiteDue(cache.SuiteFreshness{LastSuccessAt: &future}, now) {
			t.Fatal("a future last_success_at is not due")
		}
	}
	if steps() != before {
		t.Fatal("suiteDue counted the clock step")
	}
}

// Deferred minor D-23: when the queue bound cuts a tick's due list, the
// admitted suites keep key order (C4's FIFO by queue time).
func TestBoundDue_AdmittedSuitesKeepKeyOrder(t *testing.T) {
	now := int64(1_800_000_000)
	at := func(ago int64) *int64 { v := now - ago; return &v }
	due := []dueSuite{
		{key: "http|h|/a", s: cache.SuiteFreshness{LastCheckAt: at(100)}},
		{key: "http|h|/b", s: cache.SuiteFreshness{LastCheckAt: at(1000)}},
		{key: "http|h|/c", s: cache.SuiteFreshness{LastCheckAt: at(2000)}},
	}
	got := boundDue(due, newSchedQueues(), 2, now)
	if len(got) != 2 || got[0].key != "http|h|/b" || got[1].key != "http|h|/c" {
		t.Fatalf("admitted %v, want /b then /c (the two least recently checked, in key order)", got)
	}
}

// Deferred minor B-66 (proposed by the executability review, D5): a
// pending mark that reaches a suite while it is in flight (a client check
// leaves a pending change during the scheduled check) is dropped when the
// suite's turn ends (done), so it never makes the suite's NEXT queued
// turn bypass the cooldown. Kills "done keeps the pending mark", which
// TestSchedQueues_PendingChangeReachesAQueuedSuite cannot: there pop has
// already dropped the mark before done runs.
func TestSchedQueues_PendingMarkWhileInFlightEndsWithTheTurn(t *testing.T) {
	q := newSchedQueues()
	one := int64(1)
	s := schedRow("a", "/1", &one, 0)
	q.enqueue(s, false)
	it, ok := q.pop(admitAll)
	if !ok || it.pendingChange {
		t.Fatalf("setup: pop = %+v, %v; want the suite without a pending mark", it, ok)
	}
	if q.enqueue(s, true) { // in flight: a duplicate, the mark is noted
		t.Fatal("an in-flight suite was queued twice")
	}
	q.done(it)
	q.enqueue(s, false)
	next, ok := q.pop(admitAll)
	if !ok {
		t.Fatal("the suite was not queued again after its turn")
	}
	if next.pendingChange {
		t.Fatal("a pending mark noted while the suite was in flight outlived its turn")
	}
}
```

Create `internal/freshness/sim_burst_test.go`:

```go
package freshness

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// Phase E DoD (overview "E:" carry-forward), the upgrade burst: 50
// never-adopted suites with client interest (each first adoption takes one
// virtual minute) and a trickle of changes to five signed suites, a pool of
// two. The first adoptions run one at a time and back to back (the last
// free slot is reserved for signed suites); a signed change never waits for
// more than one adoption; each first-adoption suite costs at most two
// anchor probes before its adoption starts (the tick's and the offer's);
// goroutines stay bounded.
//
// AIDEV-NOTE: the signed suites' anchors answer 1 s late, so an offer made
// at a slot release — on the tick grid, like the signed suites' checks —
// takes its slot before a signed check that runs at the same instant asks
// for one: same-instant goroutine order inside a bubble is not
// deterministic, and an offer that loses its slot to a concurrent check
// costs its suite one more probe (a lost offer, spec C4: it keeps its place
// and waits for the next release). The two-probe bound is for offers that
// meet no concurrent admission and suites no client checks (phase E plan,
// design note 26); TestSimCapacity_UpgradeBurstSameHost has both. The run
// lasts 80 virtual minutes (the last adoption starts at about 68): ruling
// PE-8's 5 s budget under -race, with margin.
func TestSimCapacity_UpgradeBurst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		c := simtest.OpenCache(t)
		members := &simMembers{fakeFetcher: newFakeFetcher(), stalled: map[string]bool{}, delay: map[string]time.Duration{}}
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
			HostLimiter: hostsem.New(8), MaxConcurrent: 2, MaxDuration: 30 * time.Minute, Logger: simtest.DiscardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		pkgsFor := func(label string) []byte {
			return fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_" + label + ".deb": testHex64})
		}
		pkgsURL := func(host string) string { return "http://" + host + rigSuite + "/main/binary-amd64/Packages" }
		anchorURL := func(host string) string { return "http://" + host + rigSuite + "/InRelease" }
		up := simtest.NewUpstream()
		var first []string
		for i := 0; i < 50; i++ {
			host := fmt.Sprintf("f%02d", i)
			seedStaleAnchorAt(t, c, host, rigSuite) // client interest now, a client-created anchor
			members.put(pkgsURL(host), pkgsFor(host))
			members.delay[pkgsURL(host)] = time.Minute
			up.Set(anchorURL(host), simtest.Serve(signedRelease("first-"+host, 0,
				map[string][]byte{"main/binary-amd64/Packages": pkgsFor(host)})))
			first = append(first, host)
		}
		t0 := time.Now()
		changeAt := map[string]time.Duration{}
		v2 := map[string][]byte{}
		var signed []string
		for i := 0; i < 5; i++ {
			host := fmt.Sprintf("s%d", i)
			ref := SuiteRef{CanonicalScheme: "http", CanonicalHost: host, SuitePath: rigSuite}
			members.put(pkgsURL(host), pkgsFor("1-"+host))
			trackSuite(t, c, ref)
			v1 := signedRelease("v1-"+host, 0, map[string][]byte{"main/binary-amd64/Packages": pkgsFor("1-" + host)})
			if err := ad.Run(ctx, ref, v1, "", ""); err != nil {
				t.Fatal(err)
			}
			members.put(pkgsURL(host), pkgsFor("2-"+host))
			members.delay[pkgsURL(host)] = time.Minute
			v2[host] = signedRelease("v2-"+host, 0, map[string][]byte{"main/binary-amd64/Packages": pkgsFor("2-" + host)})
			at := time.Duration(10+12*i)*time.Minute + 7*time.Second // a trickle, off the tick grid
			changeAt[host] = at
			v2body := v2[host]
			// The signed anchors answer 1 s late (see the AIDEV-NOTE above).
			up.Set(anchorURL(host), simtest.After(time.Second, func(ctx context.Context, etag string) (*fetch.ConditionalResult, error) {
				if time.Since(t0) < at {
					return simtest.Serve(v1)(ctx, etag)
				}
				return simtest.Serve(v2body)(ctx, etag)
			}))
			signed = append(signed, host)
		}
		members.started = map[string]time.Time{}

		runtime.Gosched()
		base := runtime.NumGoroutine()
		maxG := base
		_, stop := startSched(t, Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute,
			Cooldown: time.Minute, Adopter: ad, Logger: simtest.DiscardLogger(), rand: func() float64 { return 0.5 }})
		for time.Since(t0) < 80*time.Minute {
			time.Sleep(61 * time.Second) // off the 3 m 45 s tick grid
			maxG = max(maxG, runtime.NumGoroutine())
		}
		stop()

		members.mu.Lock()
		started := make(map[string]time.Time, len(members.started))
		for h, at := range members.started {
			started[h] = at
		}
		members.mu.Unlock()
		// (1) Every first adoption ran, one at a time, back to back: each
		// admission at least one adoption after the previous one and at
		// most two adoptions (a signed one may run in between) + D later.
		var starts []time.Time
		for _, host := range first {
			at, ok := started[host]
			if !ok {
				t.Fatalf("%s: its first adoption never started", host)
			}
			if sf, err := c.GetSuiteFreshness(ctx, "http", host, rigSuite); err != nil || sf.CurrentSnapshotID == nil {
				t.Fatalf("%s: not adopted after 3 h (err %v)", host, err)
			}
			starts = append(starts, at)
			// (3) At most two anchor probes before its adoption started.
			n := 0
			for _, call := range up.Calls(anchorURL(host)) {
				if !call.After(at) {
					n++
				}
			}
			if n > 2 {
				t.Errorf("%s: %d anchor probes before its adoption started, want <= 2", host, n)
			}
		}
		slices.SortFunc(starts, time.Time.Compare)
		for i := 1; i < len(starts); i++ {
			gap := starts[i].Sub(starts[i-1])
			if gap < time.Minute {
				t.Fatalf("first adoptions %d and %d overlapped (%v apart): the last free slot is reserved", i-1, i, gap)
			}
			if gap > 2*time.Minute+checkDeadline {
				t.Errorf("first adoptions %d and %d were %v apart, want back to back (<= %v)", i-1, i, gap, 2*time.Minute+checkDeadline)
			}
		}
		// (2) A signed change waits for at most one adoption: from the
		// first probe that saw it to its adoption's start.
		for _, host := range signed {
			at, ok := started[host]
			if !ok {
				t.Fatalf("%s: its v2 adoption never started", host)
			}
			var seen time.Time
			for _, call := range up.Calls(anchorURL(host)) {
				if call.Sub(t0) >= changeAt[host] {
					seen = call
					break
				}
			}
			if seen.IsZero() || at.Sub(seen) > time.Minute+checkDeadline {
				t.Errorf("%s: its change was seen at %v and its adoption started %v later, want <= one adoption + D",
					host, seen.Sub(t0), at.Sub(seen))
			}
		}
		// (4) Workers + Run + two adoptions, plus slack for transient
		// goroutines (the cache writer's work, timer callbacks).
		const slack = 8
		if limit := base + checkWorkers + 1 + 2 + slack; maxG > limit {
			t.Fatalf("goroutines peaked at %d, limit %d", maxG, limit)
		}
	})
}

// startsByURL wraps a member fetcher and records each URL's first fetch:
// the start of the adoption that fetches it.
type startsByURL struct {
	AdoptionFetcher
	mu sync.Mutex
	at map[string]time.Time
}

func (s *startsByURL) Fetch(ctx context.Context, target *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error) {
	s.mu.Lock()
	if _, seen := s.at[target.URL]; !seen {
		s.at[target.URL] = time.Now()
	}
	s.mu.Unlock()
	return s.AdoptionFetcher.Fetch(ctx, target, dst)
}

// Ruling PE-4 (design review I-5): the upgrade burst where production has
// it — shared hosts (an archive host carries several suites) — and with
// admissions that race: 3 hosts × (8 never-adopted + 2 signed) suites, a
// pool of two, commits that take 2 s, a client checking each never-adopted
// suite every 5 minutes until it is adopted, and no delay ordering the
// signed checks behind the offers. Asserted:
//  1. first adoptions run one at a time;
//  2. a signed change waits for at most one adoption (its commit included)
//     + D + one host turn (its host's check in flight, at most D);
//  3. each never-adopted suite's anchor probes before its adoption started
//     are at most 2 (the scheduler's) + its client checks + its lost offers
//     (offers another admission took first);
//  4. lost offers in all are at most the admissions in all — each lost
//     offer coincides with another admission (work-conserving);
//  5. goroutines stay bounded.
func TestSimCapacity_UpgradeBurstSameHost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		c := simtest.OpenCache(t)
		members := &simMembers{fakeFetcher: newFakeFetcher(), stalled: map[string]bool{}, delay: map[string]time.Duration{}}
		starts := &startsByURL{AdoptionFetcher: members, at: map[string]time.Time{}}
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: starts, Verifier: passThroughVerifier{},
			HostLimiter: hostsem.New(8), MaxConcurrent: 2, MaxDuration: 30 * time.Minute, Logger: simtest.DiscardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		const commit = 2 * time.Second
		ad.SetCommitDelayForTest(commit)
		pkgsFor := func(label string) []byte {
			return fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_" + label + ".deb": testHex64})
		}
		pkgsURL := func(s SuiteRef) string {
			return "http://" + s.CanonicalHost + s.SuitePath + "/main/binary-amd64/Packages"
		}
		anchorURL := func(s SuiteRef) string { return "http://" + s.CanonicalHost + s.SuitePath + "/InRelease" }
		up := simtest.NewUpstream()
		t0 := time.Now()
		var first, signed []SuiteRef
		changeAt := map[SuiteRef]time.Duration{}
		for h := 0; h < 3; h++ {
			host := fmt.Sprintf("h%d", h)
			for i := 0; i < 8; i++ {
				s := SuiteRef{CanonicalScheme: "http", CanonicalHost: host, SuitePath: fmt.Sprintf("/r/dists/f%d", i)}
				seedStaleAnchorAt(t, c, host, s.SuitePath) // client interest now, a client-created anchor
				label := host + s.SuitePath
				members.put(pkgsURL(s), pkgsFor(label))
				members.delay[pkgsURL(s)] = time.Minute
				up.Set(anchorURL(s), simtest.Serve(signedRelease("first "+label, 0,
					map[string][]byte{"main/binary-amd64/Packages": pkgsFor(label)})))
				first = append(first, s)
			}
			for i := 0; i < 2; i++ {
				s := SuiteRef{CanonicalScheme: "http", CanonicalHost: host, SuitePath: fmt.Sprintf("/r/dists/s%d", i)}
				label := host + s.SuitePath
				members.put(pkgsURL(s), pkgsFor("1 "+label))
				trackSuite(t, c, s)
				v1 := signedRelease("v1 "+label, 0, map[string][]byte{"main/binary-amd64/Packages": pkgsFor("1 " + label)})
				if err := ad.Run(ctx, s, v1, "", ""); err != nil {
					t.Fatal(err)
				}
				members.put(pkgsURL(s), pkgsFor("2 "+label))
				members.delay[pkgsURL(s)] = time.Minute
				v2 := signedRelease("v2 "+label, 0, map[string][]byte{"main/binary-amd64/Packages": pkgsFor("2 " + label)})
				at := time.Duration(5+6*len(signed))*time.Minute + 7*time.Second // a trickle through the burst
				changeAt[s] = at
				up.Set(anchorURL(s), func(ctx context.Context, etag string) (*fetch.ConditionalResult, error) {
					if time.Since(t0) < at {
						return simtest.Serve(v1)(ctx, etag)
					}
					return simtest.Serve(v2)(ctx, etag)
				})
				signed = append(signed, s)
			}
		}
		starts.mu.Lock()
		starts.at = map[string]time.Time{} // the signed v1 adoptions above do not count
		starts.mu.Unlock()

		var mu sync.Mutex
		lost := map[SuiteRef]int{}
		clients := map[SuiteRef][]time.Time{}
		started := func(s SuiteRef) (time.Time, bool) {
			starts.mu.Lock()
			defer starts.mu.Unlock()
			at, ok := starts.at[pkgsURL(s)]
			return at, ok
		}
		runtime.Gosched()
		base := runtime.NumGoroutine()
		maxG := base
		chk, err := New(Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute,
			Cooldown: time.Minute, Adopter: ad, Logger: simtest.DiscardLogger(), rand: func() float64 { return 0.5 }})
		if err != nil {
			t.Fatal(err)
		}
		chk.offerHook = func(ref SuiteRef, rep checkReport) {
			if isQueuedResult(rep.result) {
				mu.Lock()
				lost[ref]++
				mu.Unlock()
			}
		}
		runCtx, cancel := context.WithCancel(ctx)
		runDone := make(chan struct{})
		go func() { chk.Run(runCtx); close(runDone) }()
		clientDone := make(chan struct{})
		go func() { // the clients: every 5 minutes, apt update on each suite not adopted yet
			defer close(clientDone)
			for runCtx.Err() == nil {
				for _, s := range first {
					if _, ok := started(s); ok {
						continue
					}
					mu.Lock()
					clients[s] = append(clients[s], time.Now())
					mu.Unlock()
					chk.Check(runCtx, s.CanonicalScheme, s.CanonicalHost, s.SuitePath)
				}
				select {
				case <-runCtx.Done():
				case <-time.After(5 * time.Minute):
				}
			}
		}()
		for time.Since(t0) < 50*time.Minute {
			time.Sleep(61 * time.Second) // off the 3 m 45 s tick grid
			maxG = max(maxG, runtime.NumGoroutine())
		}
		cancel()
		<-runDone
		<-clientDone
		chk.WaitForAdoptions()

		mu.Lock()
		defer mu.Unlock()
		// (1) Every first adoption ran, one at a time.
		var firstStarts []time.Time
		totalLost := 0
		for _, s := range first {
			at, ok := started(s)
			if !ok {
				t.Fatalf("%v: its first adoption never started", s)
			}
			if sf, err := c.GetSuiteFreshness(ctx, "http", s.CanonicalHost, s.SuitePath); err != nil || sf.CurrentSnapshotID == nil {
				t.Fatalf("%v: not adopted after 50 min (err %v)", s, err)
			}
			firstStarts = append(firstStarts, at)
			// (3) Anchor probes before its adoption started.
			probes, checks := 0, 0
			for _, call := range up.Calls(anchorURL(s)) {
				if !call.After(at) {
					probes++
				}
			}
			for _, call := range clients[s] {
				if !call.After(at) {
					checks++
				}
			}
			if probes > 2+checks+lost[s] {
				t.Errorf("%v: %d anchor probes before its adoption, want <= 2 + %d client checks + %d lost offers",
					s, probes, checks, lost[s])
			}
			totalLost += lost[s]
		}
		slices.SortFunc(firstStarts, time.Time.Compare)
		for i := 1; i < len(firstStarts); i++ {
			if gap := firstStarts[i].Sub(firstStarts[i-1]); gap < time.Minute {
				t.Fatalf("first adoptions %d and %d overlapped (%v apart): the last free slot is reserved", i-1, i, gap)
			}
		}
		// (2) A signed change waits for at most one adoption + D + one host turn.
		for _, s := range signed {
			at, ok := started(s)
			if !ok {
				t.Fatalf("%v: its v2 adoption never started", s)
			}
			var seen time.Time
			for _, call := range up.Calls(anchorURL(s)) {
				if call.Sub(t0) >= changeAt[s] {
					seen = call
					break
				}
			}
			if limit := time.Minute + commit + 2*checkDeadline; seen.IsZero() || at.Sub(seen) > limit {
				t.Errorf("%v: its change was seen at %v and its adoption started %v later, want <= %v",
					s, seen.Sub(t0), at.Sub(seen), limit)
			}
			totalLost += lost[s]
		}
		// (4) Work-conserving: a lost offer needs another admission.
		if admissions := len(first) + len(signed); totalLost > admissions {
			t.Errorf("%d lost offers for %d admissions", totalLost, admissions)
		}
		// (5) Workers + Run + the clients + two adoptions, plus slack for
		// transient goroutines (the cache writer's work, timer callbacks).
		const slack = 8
		if limit := base + checkWorkers + 1 + 1 + 2 + slack; maxG > limit {
			t.Fatalf("goroutines peaked at %d, limit %d", maxG, limit)
		}
	})
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/freshness/ -run 'TestSchedQueues_LostOffersKeep|TestSchedQueues_LostOfferAfterARelease|TestAdmission_Generation|TestSchedQueues_PendingChangeReaches|TestTick_RefreshesTheWaiters|TestSuiteDue_FutureSuccess|TestBoundDue_Admitted|TestSimCapacity_UpgradeBurst'`
Expected: FAIL — compile error: `too many arguments in call to q.finish` (the new tests pass the generation: `have (checkDone, number)`, `want (checkDone)`), `a.generation undefined` and `chk.offerHook undefined` (add `-gcflags=-e` to see all three past the compiler's 10-error cap).

- [ ] **Step 3: Implement**

```diff
diff --git a/internal/freshness/admission.go b/internal/freshness/admission.go
index e18666f..10bb9b8 100644
--- a/internal/freshness/admission.go
+++ b/internal/freshness/admission.go
@@ -22,4 +22,8 @@ type admission struct {
 	// through freeFor rather than counting events. Never closed.
 	freed chan struct{}
+	// releases counts slot releases (a generation): an offer dispatched
+	// before a release that then finds no slot is re-offered at once
+	// rather than held (deferred minor B-45).
+	releases uint64
 }
 
@@ -91,4 +95,11 @@ func (a *admission) wake() {
 }
 
+// generation is the number of slot releases so far.
+func (a *admission) generation() uint64 {
+	a.mu.Lock()
+	defer a.mu.Unlock()
+	return a.releases
+}
+
 // usage reports the global slots and hosts in use.
 func (a *admission) usage() (global, hosts int) {
@@ -111,4 +122,5 @@ func (l *lease) releaseHost() {
 		delete(l.a.hosts, l.host)
 		l.hostHeld = false
+		l.a.releases++
 	}
 	l.a.mu.Unlock()
@@ -134,4 +146,7 @@ func (l *lease) release() {
 		l.globalHeld = false
 	}
+	if freed {
+		l.a.releases++
+	}
 	l.a.mu.Unlock()
 	if freed {
diff --git a/internal/freshness/check.go b/internal/freshness/check.go
index 7597646..1323602 100644
--- a/internal/freshness/check.go
+++ b/internal/freshness/check.go
@@ -101,4 +101,11 @@ func (c *Checker) checkLocked(ctx, fetchCtx context.Context, out *checkOutcome,
 	}
 
+	// A last_success_at in the future made the suite due (suiteDue reads
+	// it as infinitely old without reporting it on every tick, deferred
+	// minor B-38); the check reports it once and any answer heals it.
+	if cur.LastSuccessAt != nil && time.Unix(*cur.LastSuccessAt, 0).After(now.Add(clockStepTolerance)) {
+		c.clockStep("last_success_at", scheme, host, suitePath, *cur.LastSuccessAt, now)
+	}
+
 	// Cooldown gate. Spec C4 clock-step rule: a past-event stamp found in
 	// the future (backward wall-clock step) is treated as infinitely old,
diff --git a/internal/freshness/freshness.go b/internal/freshness/freshness.go
index 6ed48cb..684686c 100644
--- a/internal/freshness/freshness.go
+++ b/internal/freshness/freshness.go
@@ -79,5 +79,5 @@ type Cache interface {
 	// (UPDATE-only, spec C3: a vanished row is left alone).
 	UpdateSuiteFreshness(ctx context.Context, s cache.SuiteFreshness) error
-	ListSuites(ctx context.Context) ([]cache.SuiteFreshness, error)
+	ListSuitesWithAdoption(ctx context.Context) ([]cache.SuiteWithAdoption, error)
 	LookupURL(ctx context.Context, scheme, host, path string) (*cache.URLPath, error)
 	GetSuiteSnapshot(ctx context.Context, snapshotID int64) (*cache.SuiteSnapshot, error)
@@ -232,4 +232,10 @@ type Checker struct {
 	lockLoadedHook func(key string)
 
+	// offerHook, when set (tests only), runs on the Run goroutine for each
+	// finished offer — a waiting suite's slot-driven re-check — with its
+	// report; a queued result is a lost offer (another admission took the
+	// slot first). The upgrade-burst simulations count lost offers with it.
+	offerHook func(ref SuiteRef, rep checkReport)
+
 	// unlockDroppedHook, when set (tests only), runs in unlockDropped
 	// between dropping a suite's lock-map entry and unlocking its mutex:
diff --git a/internal/freshness/freshness_test.go b/internal/freshness/freshness_test.go
index cd0c3cd..742eb65 100644
--- a/internal/freshness/freshness_test.go
+++ b/internal/freshness/freshness_test.go
@@ -154,5 +154,5 @@ func (f *fakeCache) UpdateSuiteFreshness(ctx context.Context, s cache.SuiteFresh
 }
 
-func (f *fakeCache) ListSuites(ctx context.Context) ([]cache.SuiteFreshness, error) {
+func (f *fakeCache) ListSuitesWithAdoption(ctx context.Context) ([]cache.SuiteWithAdoption, error) {
 	if h := f.listHook; h != nil {
 		h()
@@ -163,7 +163,13 @@ func (f *fakeCache) ListSuites(ctx context.Context) ([]cache.SuiteFreshness, err
 		return nil, f.listErr
 	}
-	out := make([]cache.SuiteFreshness, 0, len(f.suites))
+	out := make([]cache.SuiteWithAdoption, 0, len(f.suites))
 	for _, s := range f.suites {
-		out = append(out, s)
+		row := cache.SuiteWithAdoption{SuiteFreshness: s}
+		if s.CurrentSnapshotID != nil {
+			if snap, ok := f.snapshots[*s.CurrentSnapshotID]; ok {
+				row.CurrentAnchorForm = string(snap.Form)
+			}
+		}
+		out = append(out, row)
 	}
 	return out, nil
diff --git a/internal/freshness/scheduler.go b/internal/freshness/scheduler.go
index 706cf58..09cdb3f 100644
--- a/internal/freshness/scheduler.go
+++ b/internal/freshness/scheduler.go
@@ -109,4 +109,10 @@ type schedItem struct {
 	// its check bypasses the cooldown, so the tick parks it (spec C4).
 	pendingChange bool
+	// waitSeq orders the waiting list: when the suite first became
+	// waiting (deferred minor B-46: re-parks keep that order).
+	waitSeq uint64
+	// gen is the admission generation when the item was dispatched
+	// (deferred minor B-45).
+	gen uint64
 }
 
@@ -148,8 +154,13 @@ type schedQueues struct {
 	next   int      // index in order of the host whose turn is next
 	queued map[string]struct{}
+	// pending holds the keys of class-queued suites a later tick found
+	// with a pending change (deferred minor B-66): pop sets the item's
+	// pendingChange from it.
+	pending map[string]struct{}
+	waitSeq uint64 // the last waitSeq handed out
 }
 
 func newSchedQueues() *schedQueues {
-	return &schedQueues{hosts: map[string]*hostQueue{}, queued: map[string]struct{}{}}
+	return &schedQueues{hosts: map[string]*hostQueue{}, queued: map[string]struct{}{}, pending: map[string]struct{}{}}
 }
 
@@ -161,4 +172,11 @@ func (q *schedQueues) enqueue(s cache.SuiteFreshness, pendingChange bool) bool {
 	key := suiteKey(s.CanonicalScheme, s.CanonicalHost, s.SuitePath)
 	if _, dup := q.queued[key]; dup {
+		// A suite queued earlier as merely stale that a client check
+		// has since left with a pending change keeps its place but
+		// bypasses the cooldown when its turn comes (B-66). A waiting or
+		// in-flight suite ignores the mark (pop and done drop it).
+		if pendingChange {
+			q.pending[key] = struct{}{}
+		}
 		return false
 	}
@@ -191,10 +209,27 @@ func (q *schedQueues) park(item schedItem, signed bool) {
 	}
 	h.inFlight = false
-	front := item.waiting
+	if !item.waiting {
+		q.waitSeq++
+		item.waitSeq = q.waitSeq
+	}
 	item.waiting, item.signed = true, signed
-	if front {
-		h.waiting = slices.Insert(h.waiting, 0, item)
-	} else {
-		h.waiting = append(h.waiting, item)
+	// Oldest first: a re-parked waiter goes back to its place, so lost or
+	// busy offers in a row never invert the order (B-46).
+	i, _ := slices.BinarySearchFunc(h.waiting, item.waitSeq, func(w schedItem, seq uint64) int {
+		return cmp.Compare(w.waitSeq, seq)
+	})
+	h.waiting = slices.Insert(h.waiting, i, item)
+}
+
+// refreshSigned sets every waiter's signed flag from signed (keys of
+// suites whose current snapshot is signed, read by the tick): a waiter
+// parked as a first adoption that a client check has since adopted is
+// admitted under the reserved-slot rule as the signed suite it now is
+// (overview "E:" carry-forward: the flag was the parking check's).
+func (q *schedQueues) refreshSigned(signed map[string]bool) {
+	for _, h := range q.hosts {
+		for i := range h.waiting {
+			h.waiting[i].signed = signed[h.waiting[i].key]
+		}
 	}
 }
@@ -206,5 +241,5 @@ func (q *schedQueues) park(item schedItem, signed bool) {
 // parks it held too — re-offering at once would spin while the mutex is
 // held; any other result ends the suite's turn.
-func (q *schedQueues) finish(d checkDone) {
+func (q *schedQueues) finish(d checkDone, gen uint64) {
 	switch {
 	case isQueuedResult(d.report.result):
@@ -213,6 +248,9 @@ func (q *schedQueues) finish(d checkDone) {
 		// one: re-offered on the next wake or tick, never at once — the
 		// pop loop runs right after this done, and a peek that still says
-		// yes would otherwise re-offer it in a loop.
-		d.item.held = d.item.waiting
+		// yes would otherwise re-offer it in a loop. Unless a slot was
+		// released after it was dispatched (gen moved on, B-45): that
+		// release's wake may already be spent, so it is offered at once —
+		// each such re-offer needs a new release, so it cannot loop.
+		d.item.held = d.item.waiting && gen == d.item.gen
 		q.park(d.item, d.report.signedCurrent)
 	case d.item.waiting && d.report.result == resultBusy:
@@ -274,4 +312,8 @@ func (q *schedQueues) pop(admit func(host string, signed bool) bool) (schedItem,
 			h.classes[c][0] = schedItem{}
 			h.classes[c] = h.classes[c][1:]
+			if _, ok := q.pending[item.key]; ok {
+				item.pendingChange = true
+				delete(q.pending, item.key)
+			}
 			if len(h.classes[c]) == 0 {
 				h.classes[c] = nil // let the backing array go
@@ -289,4 +331,5 @@ func (q *schedQueues) pop(admit func(host string, signed bool) bool) (schedItem,
 func (q *schedQueues) done(item schedItem) {
 	delete(q.queued, item.key)
+	delete(q.pending, item.key)
 	if h, ok := q.hosts[item.ref.CanonicalHost]; ok {
 		h.inFlight = false
@@ -351,9 +394,6 @@ func (c *Checker) suiteDue(s cache.SuiteFreshness, now time.Time) bool {
 		return false
 	}
-	stale := s.LastSuccessAt == nil || *s.LastSuccessAt <= now.Add(-c.refresh).Unix()
-	if s.LastSuccessAt != nil && *s.LastSuccessAt > future {
-		c.clockStep("last_success_at", s.CanonicalScheme, s.CanonicalHost, s.SuitePath, *s.LastSuccessAt, now)
-		stale = true
-	}
+	stale := s.LastSuccessAt == nil || *s.LastSuccessAt <= now.Add(-c.refresh).Unix() ||
+		*s.LastSuccessAt > future // infinitely old; checkLocked reports it once (B-38)
 	retryDue := c.adopter != nil && s.AdoptionFailCount > 0 && s.AdoptionNextRetryAt != nil && *s.AdoptionNextRetryAt <= nowU
 	if !stale && !retryDue && !c.pendingChange(s) {
@@ -473,5 +513,5 @@ func (c *Checker) Run(ctx context.Context) {
 			}
 			select {
-			case work <- item:
+			case work <- withGen(item, c.admission.generation()):
 				idle--
 				if item.waiting {
@@ -497,6 +537,9 @@ func (c *Checker) Run(ctx context.Context) {
 			if d.item.waiting {
 				offers--
+				if c.offerHook != nil {
+					c.offerHook(d.item.ref, d.report)
+				}
 			}
-			q.finish(d)
+			q.finish(d, c.admission.generation())
 		}
 	}
@@ -514,4 +557,10 @@ func (c *Checker) safeReschedule(ctx context.Context) {
 }
 
+// withGen stamps item with the admission generation at dispatch (B-45).
+func withGen(item schedItem, gen uint64) schedItem {
+	item.gen = gen
+	return item
+}
+
 // checkDone is a worker's report of one scheduled check.
 type checkDone struct {
@@ -560,11 +609,21 @@ func (c *Checker) safeTick(ctx context.Context, q *schedQueues) {
 }
 
-// tick refreshes the queues with every due suite and updates the gauges.
+// tick refreshes the queues with every due suite, the waiting suites'
+// signed flags (the reserved-slot rule), and the gauges.
 func (c *Checker) tick(ctx context.Context, q *schedQueues) {
-	suites, err := c.cache.ListSuites(ctx)
+	rows, err := c.cache.ListSuitesWithAdoption(ctx)
 	if err != nil {
 		c.logger.Warn("freshness: list suites failed", "err", err)
 		return
 	}
+	suites := make([]cache.SuiteFreshness, len(rows))
+	signed := make(map[string]bool)
+	for i, r := range rows {
+		suites[i] = r.SuiteFreshness
+		if r.CurrentAnchorForm == string(cache.FormInline) || r.CurrentAnchorForm == string(cache.FormDetached) {
+			signed[suiteKey(r.CanonicalScheme, r.CanonicalHost, r.SuitePath)] = true
+		}
+	}
+	q.refreshSigned(signed)
 	nowT := c.now()
 	// SPEC §7.4 observability: an adopted suite stale past
```

Then pass the generation in the existing direct `finish` calls (mechanical):

```bash
perl -pi -e 's/q\.finish\((checkDone\{.*\})\)/q.finish($1, 0)/' internal/freshness/scheduler_test.go internal/freshness/suite_lock_test.go
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/freshness/ ./internal/handler/ ./cmd/...`
Expected: PASS; under `-race` `TestSimCapacity_UpgradeBurst` takes about 3.0–3.4 s and `TestSimCapacity_UpgradeBurstSameHost` about 2.2–2.5 s (12 cores, load 3), both under ruling PE-8's 5 s budget with margin. The first is deterministic (its signed anchors answer 1 s late — design note 26); the second lets admissions race and bounds the probes by its counted client checks and lost offers.

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| `park` inserts at the front again | `TestSchedQueues_LostOffersKeepTheWaitersOrder`, `TestSchedQueues_ReparkKeepsItsPlace`, `TestSchedQueues_RoundRobinAcrossWaitingHosts`, `TestSchedQueues_BusyOfferIsHeldUntilTheNextWake`, `TestRun_OfferThatLosesTheSlotKeepsItsPlace` |
| `finish`: held whatever the generation | `TestSchedQueues_LostOfferAfterAReleaseIsNotHeld` |
| `finish`: a lost offer never held | `TestSchedQueues_LostOffersKeepTheWaitersOrder`, `TestSchedQueues_LostOfferAfterAReleaseIsNotHeld`, `TestSchedQueues_LostOfferIsHeldUntilTheNextWake` |
| `lease.releaseHost` does not count | `TestAdmission_GenerationCountsReleases` |
| `enqueue` drops the pending mark of a duplicate | `TestSchedQueues_PendingChangeReachesAQueuedSuite` |
| `done` keeps the pending mark | `TestSchedQueues_PendingMarkWhileInFlightEndsWithTheTurn` (a mark noted while the suite is in flight; `pop` drops a queued suite's mark before `done` runs) |
| `pop` ignores the pending mark | `TestSchedQueues_PendingChangeReachesAQueuedSuite` |
| `refreshSigned` never promotes a waiter | `TestTick_RefreshesTheWaitersSignedFlag` |
| `checkLocked` does not report a future `last_success_at` | `TestTick_FutureLastSuccessAtIsDue` |
| `tryAcquire` without the reserved-slot rule | `TestSimCapacity_UpgradeBurst`, `TestSimCapacity_UpgradeBurstSameHost`, `TestAdmission_ReservedSlotForSignedSuites`, `TestAdmission_FreeForAgreesWithTryAcquire` |
| `boundDue` returns `byCheck[:room]` | `TestBoundDue_AdmittedSuitesKeepKeyOrder` |
| `boundDue` admits by key alone | `TestBoundDue_AdmittedSuitesKeepKeyOrder`, `TestTick_BoundAdmitsTheLeastRecentlyChecked` |

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/freshness/sched_e_test.go internal/freshness/sim_burst_test.go internal/freshness/scheduler.go \
  internal/freshness/admission.go internal/freshness/freshness.go internal/freshness/check.go internal/freshness/freshness_test.go \
  internal/freshness/scheduler_test.go internal/freshness/suite_lock_test.go
git add internal/freshness/sched_e_test.go internal/freshness/sim_burst_test.go internal/freshness/scheduler.go \
  internal/freshness/admission.go internal/freshness/freshness.go internal/freshness/check.go internal/freshness/freshness_test.go \
  internal/freshness/scheduler_test.go internal/freshness/suite_lock_test.go
git commit -m "Keep waiters' signed flag current, simulate the upgrade burst, and close the scheduler's deferred minors"
```

### Task 9: Strict mode waits for adoptions in flight (C10, D10)

Spec C10 "In-flight adoptions": the checker tracks the signed adoptions in flight per canonical host (`hostAdoptions`: counted from `spawnAdoption` until the goroutine returns, after its commit; unsigned adoptions never vouch, so they are not counted — design note 27). On the **hit path**, before `unvouched_deb_refused`, the handler waits — while an adoption is in flight on the host — then re-runs the lookup once (`checkPackageHashWait`). On the **miss path**, where the strict decision sits inside singleflight after the host slot by design, `runFetch` returns `errUnvouchedRetryable` (giving the slot back); `serveCacheMiss`, outside singleflight, waits and retries the miss once — the waiting requests coalesce again — and a second `errUnvouchedRetryable` is the refusal. Each wait is bounded by the request's age (design review M-10): at most min(15 s, 25 s − the time since the request started), and none when less than 1 s of that is left (an immediate refusal, not counted as a wait) — the miss path still downloads the whole `.deb` after the wait, before its first byte, and apt gives up after 30 s without one; both paths pass the request's `start`. At most 64 waiters per host; beyond that the refusal is immediate. The refusal is logged once per fetch, by the singleflight leader (design review O1: `unvouched_deb_refused` with its `current_snapshot_count`, as `runFetch` logged it before phase E; pinned with two coalesced requests), and a client that left while it waited gets no retry and ends `client_canceled` (underspecified item 5). The executability review's `TestServeHTTP_StrictHitWaitsOnce` (X19) and `TestSpecC11PhaseEConstants` (X14–X18, here with the two new constants) are part of the tests. With adoption disabled `main` wires no waiter (Phase 1 parity). New counter `acu_strict_adoption_waits_total{host}` (a request that waited); `docs/log-fields.md` and `docs/configuration.md` get it in Task 15, with the spec's documented consequences (an adoption over 15 s, a queued first adoption, an unsigned or anchorless repository on a host that also serves complete signed suites all still refuse).

Deferred minor D-33 (`tryByHashContentAddressed`'s doc lists four gates, the known-suite gate second) sits in `handler.go`, which this task changes.

**Files:**
- Create: `internal/freshness/inflight.go`, `internal/freshness/inflight_test.go`, `internal/handler/strict_wait_test.go`, `cmd/apt-cacher-ultra/strict_wait_wiring_test.go`
- Modify: `internal/freshness/freshness.go` (`Checker.inflight`, `New`, `spawnAdoption`), `internal/freshness/metrics.go` (`strictWaitsTotal` and the metrics inventory), `internal/handler/handler.go` (`AdoptionWaiter`, `Config.Adoptions`, `Handler.adoptions`, `tryURLPathHit`, `checkPackageHash` (takes `start`) → `checkPackageHashWait`, `runFetch`'s strict branch, `serveCacheMiss`, `errUnvouchedRetryable`, `logUnvouchedRefused`, D-33), `internal/handler/singleflight.go` (`sfResult.snapshotCount`), `cmd/apt-cacher-ultra/main.go` (`handlerConfig`)

**Interfaces:**
- Consumes: `Adopter.beforeCommit` (Task 3's test seam), Task 7's `retryRig.adoptUnsigned` and `hostCounter`, Task 6's `retryRig.unsignedRelease`, `retryRig.currentSnapshot`; handler test helpers `newPhase3StrictHandlerCapturing`, `adoptCoverageComplete`, `strictLogBuf.records`, `writeBlob`, `splitURL`, `hostKey`, `proxyReq`, `sha256Hex`; cmd test helpers `withKeyringDirs`, `newAdoptionEnabledCfg`, `newAdoptionWiringDeps`, `buildAdopter`, `checkerConfig`, `silentBuildLogger`.
- Produces: `freshness.(*Checker).AdoptionsInFlight(host string) bool`, `freshness.(*Checker).WaitHostAdoptions(ctx context.Context, host string, start time.Time) bool` (false at the 64-waiter cap and when less than 1 s of the request's 25 s budget is left; true at once when nothing is in flight); unexported `hostAdoptions` with `begin`, `end`, `inFlight`, `wait`, constants `strictWait` (15 s), `strictWaitersPerHost` (64), `strictRequestBudget` (25 s), `strictMinWait` (1 s); `handler.AdoptionWaiter` (the two methods), `handler.Config.Adoptions`; metric `acu_strict_adoption_waits_total{host}` (requests that waited: counted in `wait` after the cap check). Test helpers `fakeAdoptions` (records each wait's request age and counts `AdoptionsInFlight` calls), `holdHostSlots`, `commitVouching`, `strictWaitRig`, `waitDebPath` (`strict_wait_test.go`).

- [ ] **Step 1: Write the failing tests**

Create `internal/freshness/inflight_test.go`:

```go
package freshness

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// Spec C10: a request waits until the host's last signed adoption in
// flight ends, at most strictWait; with nothing in flight it may re-run its
// lookup at once; a cancelled request stops waiting.
func TestHostAdoptions_Wait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHostAdoptions()
		start := time.Now()
		if !h.wait(context.Background(), "a", strictWait) || time.Since(start) != 0 {
			t.Fatal("with nothing in flight the wait must return at once, re-running the lookup")
		}
		h.begin("a")
		h.begin("a")
		go func() {
			time.Sleep(time.Second)
			h.end("a")
			time.Sleep(time.Second)
			h.end("a")
		}()
		if !h.wait(context.Background(), "a", strictWait) || time.Since(start) != 2*time.Second {
			t.Fatalf("waited %v, want until the second adoption ended (2 s)", time.Since(start))
		}
		h.begin("a")
		start = time.Now()
		h.wait(context.Background(), "a", strictWait)
		if d := time.Since(start); d != strictWait {
			t.Fatalf("waited %v, want the %v bound", d, strictWait)
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(3 * time.Second); cancel() }()
		start = time.Now()
		h.wait(ctx, "a", strictWait)
		if d := time.Since(start); d != 3*time.Second {
			t.Fatalf("a cancelled request waited %v, want 3 s", d)
		}
		h.end("a")
		if h.inFlight("a") {
			t.Fatal("still in flight after every end")
		}
	})
}

// Design review M-10: a request waits at most min(15 s, 25 s − its age),
// and not at all (an immediate refusal, not counted as a wait) when less
// than 1 s of that budget is left: the miss path still downloads the .deb
// after the wait, inside apt's 30 s.
func TestChecker_WaitIsBoundedByTheRequestsAge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := &Checker{inflight: newHostAdoptions()}
		c.inflight.begin("a")
		for _, tc := range []struct {
			age, want time.Duration
			ok        bool
		}{
			{0, strictWait, true},
			{20 * time.Second, 5 * time.Second, true},
			{24*time.Second + 500*time.Millisecond, 0, false},
		} {
			before := hostCounter(t, "acu_strict_adoption_waits_total", "a")
			start := time.Now()
			if got := c.WaitHostAdoptions(context.Background(), "a", start.Add(-tc.age)); got != tc.ok {
				t.Fatalf("age %v: WaitHostAdoptions = %v, want %v", tc.age, got, tc.ok)
			}
			if d := time.Since(start); d != tc.want {
				t.Fatalf("age %v: waited %v, want %v", tc.age, d, tc.want)
			}
			if counted := hostCounter(t, "acu_strict_adoption_waits_total", "a") - before; counted != map[bool]float64{true: 1, false: 0}[tc.ok] {
				t.Fatalf("age %v: %v waits counted", tc.age, counted)
			}
		}
		c.inflight.end("a")
	})
}

// Spec C10: at most 64 waiters per host; the 65th is refused at once.
// acu_strict_adoption_waits_total counts the requests that waited: not the
// refused one, nor one with nothing in flight.
func TestHostAdoptions_WaiterCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHostAdoptions()
		beforeA, beforeB := hostCounter(t, "acu_strict_adoption_waits_total", "a"), hostCounter(t, "acu_strict_adoption_waits_total", "b")
		h.begin("a")
		var wg sync.WaitGroup
		for i := 0; i < strictWaitersPerHost; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); h.wait(context.Background(), "a", strictWait) }()
		}
		synctest.Wait()
		start := time.Now()
		if h.wait(context.Background(), "a", strictWait) || time.Since(start) != 0 {
			t.Fatal("the 65th waiter was not refused at once")
		}
		if h.wait(context.Background(), "b", strictWait) != true {
			t.Fatal("the cap is per host")
		}
		h.end("a")
		wg.Wait()
		if a, b := hostCounter(t, "acu_strict_adoption_waits_total", "a")-beforeA, hostCounter(t, "acu_strict_adoption_waits_total", "b")-beforeB; a != strictWaitersPerHost || b != 0 {
			t.Fatalf("waits counted a=%v b=%v, want %d and 0", a, b, strictWaitersPerHost)
		}
	})
}

// Spec C10: the checker counts a signed adoption in flight from its spawn
// until its goroutine returns (after the commit); an unsigned adoption is
// not counted (it never vouches).
func TestChecker_CountsSignedAdoptionsInFlight(t *testing.T) {
	for _, unsigned := range []bool{false, true} {
		r := newRetryRig(t, 0)
		var during bool
		r.adopter.beforeCommit = func() { during = r.checker.AdoptionsInFlight(rigHost) }
		if unsigned {
			r.adoptUnsigned("u1")
			r.unsignedRelease("u2", nil)
		} else {
			r.up.set(r.release("v1", 0))
		}
		r.check()
		if snap := r.currentSnapshot(); snap == nil || (unsigned && snap.Form != cache.FormUnsigned) {
			t.Fatalf("unsigned=%v: nothing adopted: %+v", unsigned, snap)
		}
		if during == unsigned {
			t.Fatalf("unsigned=%v: in flight during the commit = %v", unsigned, during)
		}
		if r.checker.AdoptionsInFlight(rigHost) {
			t.Fatalf("unsigned=%v: still in flight after the adoption returned", unsigned)
		}
	}
}

// Spec C11: phase E's constants are not knobs, and every other test uses
// them symbolically, so only this one sees a change of value. (Proposed by
// the executability review; kills X14–X18. The request budget and the
// minimum wait are design review M-10's.)
func TestSpecC11PhaseEConstants(t *testing.T) {
	for name, c := range map[string]struct{ got, want int64 }{
		"interest window":         {int64(interestWindow), int64(7 * 24 * time.Hour)},
		"confirmation gap floor":  {int64(minConfirmGap), int64(5 * time.Minute)},
		"denied-signature gap":    {int64(deniedConfirmGap), int64(time.Hour)},
		"strict wait":             {int64(strictWait), int64(15 * time.Second)},
		"strict waiters per host": {int64(strictWaitersPerHost), 64},
		"strict request budget":   {int64(strictRequestBudget), int64(25 * time.Second)},
		"strict minimum wait":     {int64(strictMinWait), int64(time.Second)},
		"unsigned member cap":     {int64(unsignedMemberCap), 1000},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d (spec C11)", name, c.got, c.want)
		}
	}
}
```

Create `internal/handler/strict_wait_test.go`:

```go
package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// fakeAdoptions is a handler.AdoptionWaiter: inFlight answers
// AdoptionsInFlight (asks counts the calls: one per strict-mode refusal
// the miss path's runFetch considers); WaitHostAdoptions records the
// request's age, runs during (the adoption that finishes while the request
// waits) and returns ok.
type fakeAdoptions struct {
	inFlight bool
	ok       bool
	during   func()
	waits    atomic.Int32
	asks     atomic.Int32
	mu       sync.Mutex
	ages     []time.Duration // time.Since(start) at each wait
}

func (f *fakeAdoptions) AdoptionsInFlight(string) bool { f.asks.Add(1); return f.inFlight }

func (f *fakeAdoptions) WaitHostAdoptions(_ context.Context, _ string, start time.Time) bool {
	f.mu.Lock()
	f.ages = append(f.ages, time.Since(start))
	f.mu.Unlock()
	f.waits.Add(1)
	if f.during != nil {
		f.during()
	}
	return f.ok
}

// holdHostSlots takes every host-limiter slot of host for d, so a miss's
// runFetch waits at its slot and concurrent misses of one path coalesce in
// singleflight (strict mode refuses before any upstream fetch).
func holdHostSlots(t *testing.T, h *Handler, host string, d time.Duration) {
	t.Helper()
	var rels []func()
	for range 4 { // the strict rig's hostsem.New(4)
		rel, err := h.sem.Acquire(context.Background(), host)
		if err != nil {
			t.Fatal(err)
		}
		rels = append(rels, rel)
	}
	go func() {
		time.Sleep(d)
		for _, rel := range rels {
			rel()
		}
	}()
}

// commitVouching adopts a complete-coverage inline snapshot of suite whose
// package_hash vouches debPath at debHash.
func commitVouching(t *testing.T, h *Handler, scheme, host, suite, debPath, debHash string) {
	t.Helper()
	inrel := writeBlob(t, h, simtest.ClearsignedFixture("InRelease for "+suite))
	if err := h.cache.SeedSuiteForTest(context.Background(), cache.SuiteFreshness{CanonicalScheme: scheme,
		CanonicalHost: host, SuitePath: suite}); err != nil {
		t.Fatal(err)
	}
	id, _, err := h.cache.InsertTrackedCandidateForTest(context.Background(), cache.SnapshotCandidate{
		CanonicalScheme: scheme, CanonicalHost: host, SuitePath: suite, InReleaseHash: &inrel})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.cache.CommitAdoption(context.Background(), id,
		[]cache.SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: inrel, DeclaredSHA256: inrel}}, nil,
		[]cache.PackageHash{{SnapshotID: id, CanonicalScheme: scheme, CanonicalHost: host, Path: debPath, DeclaredSHA256: debHash}},
		nil, true); err != nil {
		t.Fatal(err)
	}
}

// strictWaitRig is a strict-mode handler on a host whose only current
// snapshot has complete coverage, so an unvouched .deb is refused.
func strictWaitRig(t *testing.T, a *fakeAdoptions) (*Handler, *strictLogBuf, string, string, string) {
	t.Helper()
	h, srv, logs := newPhase3StrictHandlerCapturing(t, true, true, nil)
	scheme, host, port := splitURL(t, srv.URL)
	canon := hostKey(host, port)
	adoptCoverageComplete(t, h, scheme, canon, "/dists/noble", true)
	h.adoptions = a
	return h, logs, srv.URL, scheme, canon
}

const waitDebPath = "/pool/main/h/hello_1_amd64.deb"

// Spec C10 on the miss path: strict mode would refuse an unvouched .deb
// while a signed adoption is in flight on its host; runFetch gives its
// host slot back, serveCacheMiss waits (outside singleflight) and retries
// once — the adoption that finished meanwhile vouches the .deb.
func TestServeHTTP_StrictMissWaitsForAnAdoptionInFlight(t *testing.T) {
	a := &fakeAdoptions{inFlight: true, ok: true}
	h, _, srvURL, scheme, canon := strictWaitRig(t, a)
	a.during = func() {
		commitVouching(t, h, scheme, canon, "/dists/new", waitDebPath, sha256Hex([]byte("upstream bytes")))
		a.inFlight = false
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srvURL, waitDebPath))
	if rec.Code != http.StatusOK || rec.Body.String() != "upstream bytes" {
		t.Fatalf("status %d body %q, want the .deb the new snapshot vouches", rec.Code, rec.Body.String())
	}
	if a.waits.Load() != 1 {
		t.Fatalf("waited %d times, want 1", a.waits.Load())
	}
}

// Spec C10 on the hit path: the refusal of a cached unvouched .deb first
// waits for the host's adoption in flight, then re-runs the lookup once.
func TestServeHTTP_StrictHitWaitsForAnAdoptionInFlight(t *testing.T) {
	a := &fakeAdoptions{inFlight: true, ok: true}
	h, _, srvURL, scheme, canon := strictWaitRig(t, a)
	deb := []byte("a cached .deb")
	blob := writeBlob(t, h, deb)
	if err := h.cache.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: scheme, CanonicalHost: canon,
		Path: waitDebPath, BlobHash: &blob, UpstreamURL: srvURL + waitDebPath}); err != nil {
		t.Fatal(err)
	}
	a.during = func() { commitVouching(t, h, scheme, canon, "/dists/new", waitDebPath, sha256Hex(deb)) }
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srvURL, waitDebPath))
	if rec.Code != http.StatusOK || rec.Body.String() != string(deb) {
		t.Fatalf("status %d body %q, want the cached .deb, now vouched", rec.Code, rec.Body.String())
	}
	if a.waits.Load() != 1 {
		t.Fatalf("waited %d times, want 1", a.waits.Load())
	}
}

// Spec C10: no wait without an adoption in flight; beyond 64 waiters per
// host (the waiter says no) the refusal is immediate; and a .deb still
// unvouched after the wait is refused, its refusal logged once.
func TestServeHTTP_StrictWaitRefusals(t *testing.T) {
	for _, tc := range []struct {
		name      string
		a         *fakeAdoptions
		wantWaits int32
	}{
		{"nothing in flight", &fakeAdoptions{}, 0},
		{"the waiter cap", &fakeAdoptions{inFlight: true, ok: false}, 1},
		{"still unvouched after the wait", &fakeAdoptions{inFlight: true, ok: true}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, logs, srvURL, _, _ := strictWaitRig(t, tc.a)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, proxyReq("GET", srvURL, waitDebPath))
			if rec.Code != http.StatusBadGateway || rec.Header().Get("Retry-After") != "60" {
				t.Fatalf("status %d Retry-After %q, want the strict-mode refusal", rec.Code, rec.Header().Get("Retry-After"))
			}
			if got := tc.a.waits.Load(); got != tc.wantWaits {
				t.Fatalf("waited %d times, want %d", got, tc.wantWaits)
			}
			n := 0
			for _, rec := range logs.records() {
				if rec["msg"] == "unvouched_deb_refused" {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("%d unvouched_deb_refused events, want 1", n)
			}
		})
	}
	// Design review O1: two coalesced requests share one fetch, so the
	// refusal is logged once — by the singleflight leader — also after the
	// retry, whose requests coalesce again. Each request's wait is bounded
	// by its own age (design review M-10: the miss path passes the
	// request's start, so the time spent before the wait counts).
	for _, tc := range []struct {
		name string
		a    *fakeAdoptions
	}{
		{"the waiter cap, two coalesced requests", &fakeAdoptions{inFlight: true, ok: false}},
		{"still unvouched after the wait, two coalesced requests", &fakeAdoptions{inFlight: true, ok: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, logs, srvURL, _, canon := strictWaitRig(t, tc.a)
			const delay = 300 * time.Millisecond
			holdHostSlots(t, h, canon, delay) // the first call's leader waits: the second request joins it
			var once sync.Once
			tc.a.during = func() { once.Do(func() { holdHostSlots(t, h, canon, delay) }) } // so does the retry's
			var wg sync.WaitGroup
			for range 2 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, proxyReq("GET", srvURL, waitDebPath))
					if rec.Code != http.StatusBadGateway {
						t.Errorf("status %d, want the strict-mode refusal", rec.Code)
					}
				}()
			}
			wg.Wait()
			shared := 0 // the leader's line, for the request's last singleflight call
			for _, rec := range logs.records() {
				if rec["msg"] == "singleflight coalesced" {
					shared++
				}
			}
			if shared != 1 {
				t.Fatalf("%d singleflight coalesced lines, want 1 (setup: the requests must coalesce)", shared)
			}
			if got := tc.a.waits.Load(); got != 2 {
				t.Fatalf("waited %d times, want 2 (each request waits)", got)
			}
			for _, age := range tc.a.ages {
				if age < delay {
					t.Fatalf("a wait began %v into its request, want at least the %v at the host slot: the request's start, not the wait's", age, delay)
				}
			}
			n := 0
			for _, rec := range logs.records() {
				if rec["msg"] == "unvouched_deb_refused" {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("%d unvouched_deb_refused events for one fetch, want 1", n)
			}
		})
	}
}

// Underspecified item 5 of the design review: a client that leaves while
// its miss waits gets no retry — no second attempt for nobody — and the
// request ends client_canceled, not as a logged refusal.
func TestServeHTTP_StrictMissClientLeftDuringTheWait(t *testing.T) {
	a := &fakeAdoptions{inFlight: true, ok: true}
	h, logs, srvURL, _, _ := strictWaitRig(t, a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.during = cancel // the client leaves while it waits
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srvURL, waitDebPath).WithContext(ctx))
	if a.asks.Load() != 1 || rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("%d attempts, status %d; want one attempt and the client_canceled answer", a.asks.Load(), rec.Code)
	}
	for _, rec := range logs.records() {
		if rec["msg"] == "unvouched_deb_refused" {
			t.Fatal("a client that left was logged as refused")
		}
	}
}

// Spec C10 on the hit path: one wait per request — a cached .deb still
// unvouched after the wait is refused, not waited for again (the miss
// path's "still unvouched" case is TestServeHTTP_StrictWaitRefusals).
// (Proposed by the executability review; kills X19. The fake stops
// reporting an adoption after four waits so the mutant fails cleanly
// instead of recursing forever.)
func TestServeHTTP_StrictHitWaitsOnce(t *testing.T) {
	a := &fakeAdoptions{inFlight: true, ok: true}
	a.during = func() {
		if a.waits.Load() >= 4 {
			a.inFlight = false
		}
	}
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
	if got := a.waits.Load(); got != 1 {
		t.Fatalf("waited %d times, want 1", got)
	}
}
```

Create `cmd/apt-cacher-ultra/strict_wait_wiring_test.go`:

```go
package main

import (
	"context"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
)

// Spec C10: strict mode waits on the checker's adoptions in flight when
// adoption is enabled (main builds an adopter); with no adopter, or no
// checker, there is nothing to wait for and the field stays a nil
// interface (a typed nil would be called).
func TestHandlerConfig_StrictModeWaitsOnTheCheckersAdoptions(t *testing.T) {
	withKeyringDirs(t, []string{t.TempDir()})
	cfg := newAdoptionEnabledCfg(t, false)
	c, fetcher, hosts := newAdoptionWiringDeps(t)
	a, _, err := buildAdopter(cfg, c, fetcher, hosts, silentBuildLogger())
	if err != nil {
		t.Fatal(err)
	}
	ck, err := freshness.New(checkerConfig(context.Background(), cfg, c, fetcher, hosts, silentBuildLogger(), a, nil))
	if err != nil {
		t.Fatal(err)
	}
	if w, ok := handlerConfig(cfg, nil, c, fetcher, hosts, silentBuildLogger(), ck, a).Adoptions.(*freshness.Checker); !ok || w != ck {
		t.Fatal("the handler does not wait on the checker's adoptions")
	}
	if hc := handlerConfig(cfg, nil, c, fetcher, hosts, silentBuildLogger(), ck, nil); hc.Adoptions != nil {
		t.Fatalf("no adopter, yet Adoptions = %T", hc.Adoptions)
	}
	if hc := handlerConfig(cfg, nil, c, fetcher, hosts, silentBuildLogger(), nil, a); hc.Adoptions != nil {
		t.Fatalf("no checker, yet Adoptions = %T", hc.Adoptions)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go vet ./internal/freshness/ ./internal/handler/ ./cmd/apt-cacher-ultra/`
Expected: FAIL — `undefined: newHostAdoptions`, `h.adoptions undefined`, `hc.Adoptions undefined`.

- [ ] **Step 3: Implement**

Create `internal/freshness/inflight.go`:

```go
package freshness

// Spec C10 "In-flight adoptions" (D10): the checker tracks the signed
// adoptions in flight per canonical host, and strict mode waits for them
// (at most strictWait, at most strictWaitersPerHost waiters per host)
// before it refuses an unvouched .deb.

import (
	"context"
	"sync"
	"time"
)

const (
	// strictWait bounds how long a request waits for a host's adoptions
	// in flight (a C11 constant).
	strictWait = 15 * time.Second
	// strictWaitersPerHost caps the requests waiting per host; beyond it
	// the refusal is immediate (a C11 constant).
	strictWaitersPerHost = 64
	// strictRequestBudget bounds the wait by the request's age: it waits
	// at most strictRequestBudget minus the time since the request started
	// (design review M-10). The miss path still downloads the whole .deb
	// after the wait before its first byte, and apt gives up after 30 s
	// without one (a C11 constant).
	strictRequestBudget = 25 * time.Second
	// strictMinWait: a wait shorter than this is not started; the refusal
	// is immediate (a C11 constant).
	strictMinWait = time.Second
)

// hostAdoptions counts signed adoptions in flight per host and lets
// requests wait until a host has none.
//
// AIDEV-NOTE: only signed adoptions count (unsigned snapshots never vouch,
// so waiting for one would only delay the refusal), and a count drops only
// when the adoption goroutine has returned — after its commit — so a waiter
// that wakes re-reads a package_hash table that already holds the new
// snapshot's rows.
type hostAdoptions struct {
	mu      sync.Mutex
	n       map[string]int
	waiters map[string]int
	idle    map[string]chan struct{} // closed when the host's count drops to 0
}

func newHostAdoptions() *hostAdoptions {
	return &hostAdoptions{n: map[string]int{}, waiters: map[string]int{}, idle: map[string]chan struct{}{}}
}

// begin counts an adoption on host.
func (h *hostAdoptions) begin(host string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.n[host]++
}

// end uncounts an adoption on host and wakes its waiters when it was the
// last.
func (h *hostAdoptions) end(host string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.n[host]--; h.n[host] > 0 {
		return
	}
	delete(h.n, host)
	if ch, ok := h.idle[host]; ok {
		close(ch)
		delete(h.idle, host)
	}
}

// inFlight reports whether host has a signed adoption in flight.
func (h *hostAdoptions) inFlight(host string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n[host] > 0
}

// wait blocks until host has no signed adoption in flight, max passes or
// ctx ends, and reports whether the caller may re-run its lookup: false
// when the host already has strictWaitersPerHost waiters, or max is below
// strictMinWait (the caller refuses at once). With nothing in flight it
// returns true at once. A request that waits is counted
// (acu_strict_adoption_waits_total).
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
		h.mu.Unlock()
		return false
	}
	h.waiters[host]++
	strictWaitsTotal.Inc(host)
	ch, ok := h.idle[host]
	if !ok {
		ch = make(chan struct{})
		h.idle[host] = ch
	}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		if h.waiters[host]--; h.waiters[host] == 0 {
			delete(h.waiters, host)
		}
		h.mu.Unlock()
	}()
	t := time.NewTimer(max)
	defer t.Stop()
	select {
	case <-ch:
	case <-t.C:
	case <-ctx.Done():
	}
	return true
}

// AdoptionsInFlight reports whether a signed adoption is in flight on the
// canonical host (spec C10: the miss path asks before it refuses).
func (c *Checker) AdoptionsInFlight(host string) bool { return c.inflight.inFlight(host) }

// WaitHostAdoptions waits until no signed adoption is in flight on the
// canonical host (spec C10) — at most min(15 s, 25 s − the time since the
// request started at start) — and reports whether the caller should re-run
// its lookup once: false when 64 requests already wait on the host or
// less than 1 s of that budget is left, and the caller refuses at once.
func (c *Checker) WaitHostAdoptions(ctx context.Context, host string, start time.Time) bool {
	return c.inflight.wait(ctx, host, min(strictWait, strictRequestBudget-time.Since(start)))
}
```

Then (`git apply`; the metrics inventory's line for the counter included):

```diff
diff --git a/cmd/apt-cacher-ultra/main.go b/cmd/apt-cacher-ultra/main.go
index 5f584a3..1e39b2d 100644
--- a/cmd/apt-cacher-ultra/main.go
+++ b/cmd/apt-cacher-ultra/main.go
@@ -1123,4 +1123,10 @@ func handlerConfig(
 		guard = freshChecker
 	}
+	// Spec C10: strict mode waits for the checker's adoptions in flight;
+	// with adoption disabled there are none (nil: no wait).
+	var adoptions handler.AdoptionWaiter
+	if freshChecker != nil && adopter != nil {
+		adoptions = freshChecker
+	}
 	return handler.Config{
 		Parser:              parser,
@@ -1137,4 +1143,5 @@ func handlerConfig(
 		MaxSuites:           cfg.Freshness.MaxSuites,
 		SuiteGuard:          guard,
+		Adoptions:           adoptions,
 	}
 }
diff --git a/internal/freshness/freshness.go b/internal/freshness/freshness.go
index 684686c..3f9f4c0 100644
--- a/internal/freshness/freshness.go
+++ b/internal/freshness/freshness.go
@@ -191,4 +191,5 @@ type Checker struct {
 	refresh             time.Duration
 	maxSuites           int
+	inflight            *hostAdoptions // spec C10: signed adoptions in flight per host
 	maxBody             int64
 	logger              *slog.Logger
@@ -310,4 +311,5 @@ func New(cfg Config) (*Checker, error) {
 		gpgReasonClassifier: cfg.GPGReasonClassifier,
 		admission:           newAdmission(maxConc),
+		inflight:            newHostAdoptions(),
 		maxAdoptionDuration: maxDur,
 	}, nil
@@ -539,6 +541,16 @@ func (c *Checker) suiteRowExists(ctx context.Context, ref SuiteRef) bool {
 func (c *Checker) spawnAdoption(mu *sync.Mutex, req *adoptionRequest, l *lease) {
 	c.adoptionWg.Add(1)
+	// Spec C10: a signed adoption is in flight from here (before the
+	// goroutine runs, so a request right after the check sees it) until
+	// the goroutine has returned, its commit done.
+	signed := req.form != adoptionFormUnsigned
+	if signed {
+		c.inflight.begin(req.suite.CanonicalHost)
+	}
 	go func() {
 		defer c.adoptionWg.Done()
+		if signed {
+			defer c.inflight.end(req.suite.CanonicalHost)
+		}
 		defer mu.Unlock()
 		defer l.release()
diff --git a/internal/freshness/metrics.go b/internal/freshness/metrics.go
index c064827..ead2a68 100644
--- a/internal/freshness/metrics.go
+++ b/internal/freshness/metrics.go
@@ -34,5 +34,6 @@ import (
 // unlabelled gauge); acu_freshness_unlogged_panics_total (deferred minor
 // B-44, unlabelled counter); acu_repair_deferred_total (deferred minor
-// B-31, counter by host). The flip barrier's acu_flip_hold_skipped_total,
+// B-31, counter by host); acu_strict_adoption_waits_total (spec C10,
+// counter by host). The flip barrier's acu_flip_hold_skipped_total,
 // acu_flip_hold_expired_total, acu_flip_quiet_cut_short_total,
 // acu_flip_waiters_over_cap_total, acu_flip_slow_anchor_reads_total and
@@ -62,4 +63,13 @@ var (
 	)
 
+	// strictWaitsTotal counts requests that waited for a host's signed
+	// adoptions in flight before strict mode decided (spec C10).
+	strictWaitsTotal = metrics.NewCounterWithCap(
+		"acu_strict_adoption_waits_total",
+		"Unvouched .deb requests that waited for the host's adoptions in flight before strict mode decided, by host (spec C10).",
+		metrics.DefaultMaxSeries,
+		"host",
+	)
+
 	// repairDeferredTotal counts repairs and reconciles of an unchanged
 	// suite that found no free admission slot and wait for its next
diff --git a/internal/handler/handler.go b/internal/handler/handler.go
index df04bf9..69aaa64 100644
--- a/internal/handler/handler.go
+++ b/internal/handler/handler.go
@@ -60,4 +60,18 @@ type FreshnessChecker interface {
 }
 
+// AdoptionWaiter is strict mode's view of the freshness checker's signed
+// adoptions in flight (spec C10, D10; *freshness.Checker implements it).
+// nil (adoption disabled) refuses at once, as before.
+type AdoptionWaiter interface {
+	// AdoptionsInFlight reports whether a signed adoption is in flight on
+	// the canonical host.
+	AdoptionsInFlight(host string) bool
+	// WaitHostAdoptions waits until none is — at most min(15 s, 25 s −
+	// the time since the request started at start) — and reports whether
+	// to re-run the lookup once: false when 64 requests already wait on
+	// the host or less than 1 s of the budget is left.
+	WaitHostAdoptions(ctx context.Context, host string, start time.Time) bool
+}
+
 // Config carries handler dependencies. All non-pointer fields are
 // optional and defaulted in New.
@@ -79,4 +93,9 @@ type Config struct {
 	Freshness FreshnessChecker
 
+	// Adoptions lets strict mode wait for adoptions in flight on the
+	// request's host before refusing an unvouched .deb (spec C10). nil:
+	// no wait.
+	Adoptions AdoptionWaiter
+
 	// Serve is the SPEC §6.4 / §8 stale-serve policy. Zero value is
 	// safe (both flags off): the handler will always 502 on a metadata
@@ -151,4 +170,5 @@ type Handler struct {
 	sem       *hostsem.Sem
 	freshness FreshnessChecker
+	adoptions AdoptionWaiter
 	serve     config.ServeConfig
 	logger    *slog.Logger
@@ -250,4 +270,5 @@ func New(cfg Config) (*Handler, error) {
 		sem:                     cfg.HostLimiter,
 		freshness:               cfg.Freshness,
+		adoptions:               cfg.Adoptions,
 		serve:                   cfg.Serve,
 		logger:                  logger,
@@ -370,4 +391,10 @@ var (
 	// failure) so operators can grep for the strict-mode contribution.
 	ErrUnvouchedDebRefused = errors.New("handler: unvouched .deb refused (strict mode)")
+
+	// errUnvouchedRetryable: strict mode would refuse a missed .deb while
+	// a signed adoption is in flight on its host (spec C10). runFetch
+	// returns it inside singleflight; serveCacheMiss waits and retries
+	// once, and turns a second one into ErrUnvouchedDebRefused.
+	errUnvouchedRetryable = errors.New("handler: unvouched .deb, an adoption is in flight on the host")
 )
 
@@ -690,5 +717,5 @@ func (h *Handler) tryURLPathHit(w http.ResponseWriter, r *http.Request, req *pro
 	var v packageValidation
 	if !req.IsMetadata {
-		if served, outcome, status, body, fellThrough, gotV := h.checkPackageHash(w, r, req, *row.BlobHash); fellThrough {
+		if served, outcome, status, body, fellThrough, gotV := h.checkPackageHash(w, r, req, *row.BlobHash, start); fellThrough {
 			return false, "", 0, 0, packageValidation{}
 		} else if served {
@@ -759,10 +786,12 @@ var byHashSuffixRegex = regexp.MustCompile(`/by-hash/SHA256/([0-9a-f]{64})$`)
 // non-adoption upholds it.
 //
-// Three security gates apply *before* a serve:
+// Four security gates apply *before* a serve (deferred minor D-33):
 //
 //  1. SuitePath != "" — by-hash URLs are only meaningful under
 //     /dists/<suite>/. A request without a suite path is not a legitimate
 //     apt by-hash request; reject without consulting the pool.
-//  2. HostAllowed — ServeHTTP delays the §6.6 allowlist check until after
+//  2. A known suite (sr.tracked: the suite has a row, I-4, spec C3) — a
+//     minted suite name falls through to the miss path.
+//  3. HostAllowed — ServeHTTP delays the §6.6 allowlist check until after
 //     tryCacheHit (lines 213/225). For Phase 1/2 hits that's safe because
 //     url_path rows are host-scoped — only previously-allowed hosts have
@@ -770,5 +799,5 @@ var byHashSuffixRegex = regexp.MustCompile(`/by-hash/SHA256/([0-9a-f]{64})$`)
 //     re-runs the allowlist check; otherwise a client could request a
 //     known SHA256 under a disallowed host and bypass §6.6.
-//  3. Same-host provenance (HostHasBlob) — the blob must have been
+//  4. Same-host provenance (HostHasBlob) — the blob must have been
 //     fetched under this host before. Without this, a caller who learns
 //     a SHA256 from one cached host could request it under any other
@@ -929,4 +958,13 @@ func (h *Handler) recordByHashURLPath(req *proxy.Request, hash string) {
 }
 
+// logUnvouchedRefused logs the miss path's strict-mode refusal.
+func (h *Handler) logUnvouchedRefused(req *proxy.Request, count int) {
+	h.logger.Info("unvouched_deb_refused",
+		"canonical_host", req.CanonicalHost,
+		"path", req.Path,
+		"current_snapshot_count", count,
+	)
+}
+
 // checkPackageHash runs the SPEC2 §6.1 defense-in-depth on a .deb hit.
 //
@@ -946,5 +984,14 @@ func (h *Handler) recordByHashURLPath(req *proxy.Request, hash string) {
 // validated_hash=true + package_name=<name>. The mismatch + conflict
 // branches also increment acu_serve_hash_validated_total{outcome=mismatch}.
-func (h *Handler) checkPackageHash(w http.ResponseWriter, r *http.Request, req *proxy.Request, blobHash string) (served bool, outcome string, status int, bytesWritten int64, fellThrough bool, validation packageValidation) {
+func (h *Handler) checkPackageHash(w http.ResponseWriter, r *http.Request, req *proxy.Request, blobHash string, start time.Time) (served bool, outcome string, status int, bytesWritten int64, fellThrough bool, validation packageValidation) {
+	return h.checkPackageHashWait(w, r, req, blobHash, start, true)
+}
+
+// checkPackageHashWait is checkPackageHash; mayWait lets a strict-mode
+// refusal first wait for the host's signed adoptions in flight and run
+// the lookup once more (spec C10: at most 15 s and at most 25 s since the
+// request started at start, at most 64 waiters per host, beyond which the
+// refusal is immediate).
+func (h *Handler) checkPackageHashWait(w http.ResponseWriter, r *http.Request, req *proxy.Request, blobHash string, start time.Time, mayWait bool) (served bool, outcome string, status int, bytesWritten int64, fellThrough bool, validation packageValidation) {
 	rows, err := h.cache.DeclaredHashesForPath(r.Context(),
 		req.CanonicalScheme, req.CanonicalHost, req.Path)
@@ -976,4 +1023,8 @@ func (h *Handler) checkPackageHash(w http.ResponseWriter, r *http.Request, req *
 			req.CanonicalScheme, req.CanonicalHost); sm {
 		case strictRefuse:
+			if mayWait && h.adoptions != nil && h.adoptions.AdoptionsInFlight(req.CanonicalHost) &&
+				h.adoptions.WaitHostAdoptions(r.Context(), req.CanonicalHost, start) {
+				return h.checkPackageHashWait(w, r, req, blobHash, start, false)
+			}
 			st, body := h.refuseUnvouchedDeb(w, req.CanonicalHost, req.Path, count)
 			return true, "unvouched_deb_refused", st, body, false, packageValidation{}
@@ -1349,5 +1400,5 @@ func (h *Handler) serveCacheMiss(w http.ResponseWriter, r *http.Request, req *pr
 
 	file, trackedAtStart := sr.file, sr.tracked
-	res, shared, waiters := h.sf.Do(key, func() sfResult {
+	fetchOnce := func() sfResult {
 		// Use the handler's lifecycle ctx, not the request ctx. Two
 		// goals: (1) a leader who disconnects must not kill the fetch
@@ -1359,5 +1410,33 @@ func (h *Handler) serveCacheMiss(w http.ResponseWriter, r *http.Request, req *pr
 		// drain budget elapses.
 		return h.runFetch(h.lifecycleCtx, req, file, trackedAtStart)
-	})
+	}
+	res, shared, waiters := h.sf.Do(key, fetchOnce)
+	if errors.Is(res.err, errUnvouchedRetryable) {
+		// Spec C10: strict mode would refuse this .deb while a signed
+		// adoption runs on its host. Wait for it here — outside
+		// singleflight and the host slot — at most 15 s, then retry the
+		// miss once; a second "retryable" is the refusal.
+		//
+		// AIDEV-NOTE: the retry is one more singleflight call: requests
+		// that waited together coalesce again, and a waiter over the
+		// 64-per-host cap, or with less than 1 s of its 25 s budget left,
+		// is refused at once. A client that left while it waited gets no
+		// retry (it ends client_canceled). The refusal is logged once per
+		// fetch, by the singleflight leader (design review O1), as
+		// runFetch logged it before phase E.
+		waited := h.adoptions != nil && h.adoptions.WaitHostAdoptions(r.Context(), req.CanonicalHost, start)
+		switch {
+		case r.Context().Err() != nil:
+			res.err = r.Context().Err()
+		case waited:
+			res, shared, waiters = h.sf.Do(key, fetchOnce)
+		}
+		if errors.Is(res.err, errUnvouchedRetryable) {
+			if !shared {
+				h.logUnvouchedRefused(req, res.snapshotCount)
+			}
+			res.err = ErrUnvouchedDebRefused
+		}
+	}
 
 	// SPEC §10: leader emits a structured log line whenever waiters
@@ -1779,9 +1858,11 @@ func (h *Handler) runFetch(ctx context.Context, req *proxy.Request, file aptpath
 					req.CanonicalScheme, req.CanonicalHost); sm {
 				case strictRefuse:
-					h.logger.Info("unvouched_deb_refused",
-						"canonical_host", req.CanonicalHost,
-						"path", req.Path,
-						"current_snapshot_count", count,
-					)
+					// Spec C10: with a signed adoption in flight on the
+					// host, end this attempt (releasing the host slot)
+					// and let serveCacheMiss wait outside singleflight.
+					if h.adoptions != nil && h.adoptions.AdoptionsInFlight(req.CanonicalHost) {
+						return sfResult{err: errUnvouchedRetryable, snapshotCount: count}
+					}
+					h.logUnvouchedRefused(req, count)
 					return sfResult{err: ErrUnvouchedDebRefused}
 				case strictPassthrough:
diff --git a/internal/handler/singleflight.go b/internal/handler/singleflight.go
index 14bd4b6..770d675 100644
--- a/internal/handler/singleflight.go
+++ b/internal/handler/singleflight.go
@@ -38,4 +38,7 @@ type sfResult struct {
 	// vouchingSuites: the suites whose snapshots vouched a validated .deb.
 	vouchingSuites []string
+	// snapshotCount: the strict-mode current_snapshot_count of an
+	// errUnvouchedRetryable result, for the refusal's log line.
+	snapshotCount int
 }
 
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/freshness/ ./internal/handler/ ./cmd/...`
Expected: PASS.

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| `wait`: the cap admits a 65th waiter (`>`) | `TestHostAdoptions_WaiterCap` |
| `wait`: a request that waits is not counted (the `Inc` removed) | `TestHostAdoptions_WaiterCap` |
| `wait`: the refused 65th request is counted too (the `Inc` before the cap check) | `TestHostAdoptions_WaiterCap` |
| `end`: never wakes the waiters (`>= 0`) | `TestHostAdoptions_Wait` |
| `end`: wakes waiters while another adoption runs (`> 1`) | `TestHostAdoptions_Wait` |
| `wait`: waits even with nothing in flight | `TestHostAdoptions_Wait`, `TestHostAdoptions_WaiterCap` |
| `wait`: ignores a cancelled request | `TestHostAdoptions_Wait` |
| `wait`: no minimum wait (M-10) | `TestChecker_WaitIsBoundedByTheRequestsAge` |
| `WaitHostAdoptions` ignores the request's age (`strictWait` alone) | `TestChecker_WaitIsBoundedByTheRequestsAge` |
| `spawnAdoption`: counts unsigned adoptions | `TestChecker_CountsSignedAdoptionsInFlight` |
| `spawnAdoption`: never uncounts | `TestChecker_CountsSignedAdoptionsInFlight` |
| hit path: refuses after the wait without re-running the lookup | `TestServeHTTP_StrictHitWaitsForAnAdoptionInFlight` |
| hit path: waits again after the re-run (`mayWait` ignored, X19) | `TestServeHTTP_StrictHitWaitsOnce` |
| miss path: `runFetch` refuses at once | `TestServeHTTP_StrictMissWaitsForAnAdoptionInFlight`, `TestServeHTTP_StrictWaitRefusals` |
| miss path: no retry after the wait | `TestServeHTTP_StrictMissWaitsForAnAdoptionInFlight` |
| miss path: the converted refusal is not logged | `TestServeHTTP_StrictWaitRefusals` |
| miss path: every coalesced request logs the refusal (`if true`, O1) | `TestServeHTTP_StrictWaitRefusals/{the_waiter_cap,_two_coalesced_requests, still_unvouched_after_the_wait,_two_coalesced_requests}` |
| miss path: the wait gets a fresh start (`time.Now()`) | `TestServeHTTP_StrictWaitRefusals/{… two coalesced legs}` |
| miss path: a retry for a client that left | `TestServeHTTP_StrictMissClientLeftDuringTheWait` |
| a C11 constant changed: `minConfirmGap` 4 min, `deniedConfirmGap` 62 min, `unsignedMemberCap` 2,000, `strictWait` 16 s, `strictWaitersPerHost` 65, `strictRequestBudget` 30 s, `strictMinWait` 0 (X14–X18) | `TestSpecC11PhaseEConstants` (the last two also `TestChecker_WaitIsBoundedByTheRequestsAge`) |
| `handlerConfig`: no waiter wired | `TestHandlerConfig_StrictModeWaitsOnTheCheckersAdoptions` |
| `handlerConfig`: a waiter wired without an adopter | `TestHandlerConfig_StrictModeWaitsOnTheCheckersAdoptions` |

(The hit path passing a fresh `start` instead of the request's is a near-equivalent mutant: nothing slow happens before the hit path's wait, so the two ages differ by microseconds.)

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/freshness/inflight.go internal/freshness/inflight_test.go internal/freshness/freshness.go internal/freshness/metrics.go \
  internal/handler/handler.go internal/handler/singleflight.go internal/handler/strict_wait_test.go \
  cmd/apt-cacher-ultra/main.go cmd/apt-cacher-ultra/strict_wait_wiring_test.go
git add internal/freshness/inflight.go internal/freshness/inflight_test.go internal/freshness/freshness.go internal/freshness/metrics.go \
  internal/handler/handler.go internal/handler/singleflight.go internal/handler/strict_wait_test.go \
  cmd/apt-cacher-ultra/main.go cmd/apt-cacher-ultra/strict_wait_wiring_test.go
git commit -m "Let strict mode wait for a host's signed adoptions in flight before refusing an unvouched .deb"
```

### Task 10: The handler at first contact — T1-on-seed, waiters' late registration, onboarding's deadline

Overview "E:" carry-forwards from phase D:

- **T1-on-seed** (spec C3; phase D plan, design note 2): a miss that created its suite's row fires T1 once its response is done, so the new suite's eager first adoption starts at first contact. It is keyed on the creation (`sfResult.created` from `FetchedResult.Create.Outcome == cache.SuiteCreated`; `Tracked` stays what the late flip-barrier registration uses), fired by the creating request only (a coalesced waiter shares its result), and only with adoption enabled (`TestServeHTTP_AdoptionDisabledSeedParity`). The hit path already fires T1 after onboarding (ruling PD-11); no second T1 is added there. Firing after the response (a `defer`) keeps the creating request's own serve out of its suite's first flip: the barrier waits for it like any registered serve; the `defer` also fires when the client left, which is right — the row was created (design note 28). The executability review's `TestServeHTTP_CoalescedCreatingMissFiresT1Once` (X27) pins "the creating request only".
- **Waiters' late registration**, untested in phase D because no flip could hold a just-created suite: a coalesced miss waiter (`t9_miss_waiter_noreg`) and an onboarding waiter (`t10_waiter_noreg`) each register their own serve; with eager first adoption a flip of a just-created suite is reachable, so each path gets a test that the waiter waits in that flip's hold and serves what it committed.

Deferred minors in `internal/handler/suite_rows.go`: D-35 (E; onboarding's creation and its backstop share one deadline, `onboardTimeout`, so a hold of the just-created suite between them cannot push the wait before the body past 5 s + `flipgate.HoldMax`), D-34 (onboarding runs under the handler's lifecycle context, so a graceful shutdown cuts it short), D-37 (the backstop's bound covers its database steps; its pool reads take no context — `backstopTimeout`'s doc and the miss path's AIDEV-NOTE say so; `docs/log-fields.md`'s row in Task 15). D-32 is tagged G and stays.

Design review M-15 (D-35's sibling on the miss path): the miss path's backstop got a fresh `backstopTimeout` after the fetch, the store and a possible flip hold (≤ 20 s), so the body could start 27 s or more into the request. Its deadline now counts from the request's start too: `missBackstopDeadline(start)` = min(now + 5 s, start + `missBodyBudget`, 25 s). A deadline already past fails the backstop at once (logged WARN; the body is served), as any backstop timeout does.

**Files:**
- Create: `internal/handler/seed_t1_test.go`
- Modify: `internal/handler/handler.go` (`runFetch`'s result, `serveCacheMiss`: T1-on-seed and the backstop's deadline, the miss path's AIDEV-NOTE), `internal/handler/singleflight.go` (`sfResult.created`), `internal/handler/suite_rows.go` (`backstopTimeout` and `onboardTimeout` docs, `missBodyBudget`, `missBackstopDeadline`, `onboard`, `runOnboard`)

**Interfaces:**
- Consumes: Task 6's eager first adoption (what T1 starts); handler test helpers `newSuiteRig`, `suiteRig.serve/get/suite/cacheAnchor`, `recordingFreshness.snapshot`, `upFile`, `rowInRelease`, `rowRelease`, `newFlipStack`, `flipStack.holdUntilTracked/cacheRow/signsAfterTheSniff`, `staticUpstream`, `writeBlob`, `flipSimHost`; handler test seams `cachedAnchorsHook`, `backstopHook`, `activeWG`, `lifecycleCancel`.
- Produces: `sfResult.created`; `runOnboard(r, req, sr, deadline time.Time)`; `missBodyBudget` (25 s), `missBackstopDeadline(start time.Time) time.Time`. Test helpers `slowUpstream`, `commitSnapshotWhenTracked`, `twoGets` (`seed_t1_test.go`).

- [ ] **Step 1: Write the failing tests**

Create `internal/handler/seed_t1_test.go`:

```go
package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// Spec C3 T1-on-seed (phase D plan, design note 2; overview "E:"
// carry-forward): a miss that creates its suite's row fires T1 once its
// response is done, keyed on the creation — a miss on a suite that already
// has a row fires none — and only with adoption enabled.
func TestServeHTTP_MissThatCreatesItsSuiteFiresT1(t *testing.T) {
	for _, adoption := range []bool{true, false} {
		r := newSuiteRig(t, adoption, 0)
		r.serve("/r/dists/s/InRelease", upFile{body: rowInRelease})
		r.serve("/r/dists/s/Release", upFile{body: rowRelease})
		if rec := r.get("/r/dists/s/InRelease"); rec.Code != http.StatusOK || r.suite("/r/dists/s") == nil {
			t.Fatalf("adoption=%v: the creating miss = %d", adoption, rec.Code)
		}
		if rec := r.get("/r/dists/s/Release"); rec.Code != http.StatusOK {
			t.Fatalf("adoption=%v: the second miss = %d", adoption, rec.Code)
		}
		r.h.activeWG.Wait()
		want := 0
		if adoption {
			want = 1
		}
		if calls := r.fresh.snapshot(); len(calls) != want || (want == 1 && calls[0].suitePath != "/r/dists/s") {
			t.Fatalf("adoption=%v: T1 calls %v, want %d for /r/dists/s", adoption, calls, want)
		}
	}
}

// slowUpstream answers like staticUpstream after delay, counting fetches.
type slowUpstream struct {
	staticUpstream
	delay   time.Duration
	fetches *atomic.Int32
}

func (u slowUpstream) Fetch(ctx context.Context, target *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error) {
	u.fetches.Add(1)
	time.Sleep(u.delay)
	return u.staticUpstream.Fetch(ctx, target, dst)
}

// commitSnapshotWhenTracked holds a flip of suite until the suite has a row
// and commits a snapshot whose InRelease it returns.
func commitSnapshotWhenTracked(t *testing.T, s *flipStack, suite string) ([]byte, chan struct{}) {
	t.Helper()
	ctx := context.Background()
	body := simtest.ClearsignedFixture("Origin: snapshot\n")
	hash := writeBlob(t, s.h, body)
	cand, _, err := s.c.InsertCandidateSnapshot(ctx, cache.SnapshotCandidate{CanonicalScheme: "http",
		CanonicalHost: flipSimHost, SuitePath: suite, InReleaseHash: &hash})
	if err != nil {
		t.Fatal(err)
	}
	return body, s.holdUntilTracked(suite, 0, func() error {
		return s.c.CommitAdoption(ctx, cand, []cache.SnapshotMember{{SnapshotID: cand, Path: "InRelease",
			BlobHash: hash, DeclaredSHA256: hash}}, nil, nil, nil, false)
	})
}

// twoGets runs two concurrent GETs of path and returns their bodies.
func twoGets(s *flipStack, path string) [2][]byte {
	var bodies [2][]byte
	var wg sync.WaitGroup
	for i := range bodies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			s.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+flipSimHost+path, nil))
			bodies[i] = rec.Body.Bytes()
		}()
	}
	wg.Wait()
	return bodies
}

// Phase D carry-forward (final review, tests Minor 3; t9_miss_waiter_noreg):
// a miss waiter that shares the creating leader's fetch registers its own
// serve with the flip barrier, so a flip of the just-created suite (an
// eager first adoption) holds it too, and it serves what the flip
// committed — reachable since phase E.
func TestServeHTTP_CoalescedMissWaiterRegistersItsOwnServe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newFlipStack(t, 10*time.Second)
		const suite = "/n/dists/s"
		var fetches atomic.Int32
		s.h.fetch = slowUpstream{staticUpstream: staticUpstream{"http://" + flipSimHost + suite + "/InRelease": rowInRelease},
			delay: time.Second, fetches: &fetches}
		snap, flipped := commitSnapshotWhenTracked(t, s, suite)
		bodies := twoGets(s, suite+"/InRelease")
		<-flipped
		if fetches.Load() != 1 {
			t.Fatalf("%d upstream fetches, want 1 (the waiter shares the leader's)", fetches.Load())
		}
		for i, b := range bodies {
			if !bytes.Equal(b, snap) {
				t.Fatalf("request %d served %q, want the snapshot the flip committed (its serve was not registered)", i, b)
			}
		}
	})
}

// Phase D carry-forward (t10_waiter_noreg): an onboarding hit that shares
// another hit's creation registers its own serve, as above.
func TestServeHTTP_OnboardingWaiterRegistersItsOwnServe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newFlipStack(t, 10*time.Second)
		const suite = "/n/dists/s"
		s.cacheRow(suite+"/InRelease", rowInRelease, wallclock.Unix()) // a legacy anchor: the hits onboard
		var creations atomic.Int32
		s.h.cachedAnchorsHook = func() { // the first creation lasts a second: the other hit joins it
			if creations.Add(1) == 1 {
				time.Sleep(time.Second)
			}
		}
		snap, flipped := commitSnapshotWhenTracked(t, s, suite)
		bodies := twoGets(s, suite+"/InRelease")
		<-flipped
		if creations.Load() != 1 {
			t.Fatalf("%d onboardings ran, want 1 shared", creations.Load())
		}
		for i, b := range bodies {
			if !bytes.Equal(b, snap) {
				t.Fatalf("hit %d served %q, want the snapshot the flip committed (its serve was not registered)", i, b)
			}
		}
	})
}

// Deferred minor D-35: onboarding's creation and its backstop share one
// deadline, so a hold of the just-created suite between them (reachable
// since eager first adoption) cannot stretch the wait before the body past
// onboardTimeout + flipgate.HoldMax.
func TestServeHTTP_OnboardingSharesOneDeadlineWithItsBackstop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newFlipStack(t, 10*time.Second)
		const suite = "/n/dists/s"
		s.h.adoptionEnabled = true
		s.cacheRow(suite+"/Release", rowRelease, wallclock.Unix())
		s.signsAfterTheSniff(suite, wallclock.Unix())
		s.h.backstopHook = func(ctx context.Context) { <-ctx.Done() } // a writer that never answers
		flipped := s.holdUntilTracked(suite, 3*time.Second, func() error { return nil })
		start := time.Now()
		rec := httptest.NewRecorder()
		s.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+flipSimHost+suite+"/Release", nil))
		<-flipped
		if elapsed := time.Since(start); rec.Code != http.StatusOK || elapsed != onboardTimeout {
			t.Fatalf("status %d after %v, want 200 after the shared %v (not the 3 s hold + a fresh %v)",
				rec.Code, elapsed, onboardTimeout, backstopTimeout)
		}
	})
}

// Deferred minor D-34: onboarding runs under the handler's lifecycle, so a
// graceful shutdown cuts its creation short; the hit is served untracked.
func TestServeHTTP_OnboardingStopsAtShutdown(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	r.cacheAnchor("/r/dists/s/InRelease", rowInRelease, 1_700_000_000)
	r.h.cachedAnchorsHook = func() { r.h.lifecycleCancel() } // shutdown begins during the creation
	if rec := r.get("/r/dists/s/InRelease"); rec.Code != http.StatusOK {
		t.Fatalf("hit = %d, want the cached anchor served", rec.Code)
	}
	if r.suite("/r/dists/s") != nil {
		t.Fatal("the onboarding outlived the shutdown")
	}
}

// Design review M-15: the miss path's backstop deadline counts from the
// request's start, so a slow fetch (or a flip's hold) leaves the backstop
// only what is left of the 25 s budget (missBodyBudget): the body still
// starts inside apt's 30 s, not after the fetch plus a fresh
// backstopTimeout.
func TestServeHTTP_MissBackstopSharesTheRequestsBudget(t *testing.T) {
	const budget = 25 * time.Second // missBodyBudget
	for _, fetchTime := range []time.Duration{2 * time.Second, 22 * time.Second} {
		t.Run(fetchTime.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newFlipStack(t, 0)
				const suite = "/n/dists/s"
				s.h.adoptionEnabled = true // a Release creates the row; it marks nothing itself
				var fetches atomic.Int32
				s.h.fetch = slowUpstream{staticUpstream: staticUpstream{"http://" + flipSimHost + suite + "/Release": rowRelease},
					delay: fetchTime, fetches: &fetches}
				s.signsAfterTheSniff(suite, wallclock.Unix())                 // the creation is unproven: the backstop runs
				s.h.backstopHook = func(ctx context.Context) { <-ctx.Done() } // a writer that never answers
				start := time.Now()
				rec := httptest.NewRecorder()
				s.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+flipSimHost+suite+"/Release", nil))
				want := min(fetchTime+backstopTimeout, budget)
				if elapsed := time.Since(start); rec.Code != http.StatusOK || elapsed != want {
					t.Fatalf("status %d after %v, want 200 after %v (the %v fetch, then the backstop within the %v budget)",
						rec.Code, elapsed, want, fetchTime, budget)
				}
			})
		})
	}
}

// Spec C3 T1-on-seed: only the creating request fires T1 — a coalesced
// miss waiter shares the leader's result, creation included, and fires
// nothing. (Proposed by the executability review; kills X27.)
func TestServeHTTP_CoalescedCreatingMissFiresT1Once(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	var fetches atomic.Int32
	r.h.fetch = slowUpstream{staticUpstream: staticUpstream{r.srv.URL + "/r/dists/s/InRelease": rowInRelease},
		delay: 300 * time.Millisecond, fetches: &fetches}
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rec := r.get("/r/dists/s/InRelease"); rec.Code != http.StatusOK {
				t.Errorf("miss = %d", rec.Code)
			}
		}()
	}
	wg.Wait()
	r.h.activeWG.Wait()
	if fetches.Load() != 1 {
		t.Fatalf("%d upstream fetches, want 1 (setup: the two misses must coalesce)", fetches.Load())
	}
	if calls := r.fresh.snapshot(); len(calls) != 1 {
		t.Fatalf("T1 calls %v, want exactly one (the creating leader's)", calls)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/handler/ -run 'TestServeHTTP_MissThatCreatesItsSuiteFiresT1|TestServeHTTP_CoalescedMissWaiter|TestServeHTTP_OnboardingWaiter|TestServeHTTP_OnboardingSharesOneDeadline|TestServeHTTP_OnboardingStopsAtShutdown|TestServeHTTP_MissBackstopSharesTheRequestsBudget|TestServeHTTP_CoalescedCreatingMissFiresT1Once'`
Expected: FAIL — `TestServeHTTP_MissThatCreatesItsSuiteFiresT1` (`adoption=true: T1 calls [], want 1 for /r/dists/s`), `TestServeHTTP_OnboardingSharesOneDeadlineWithItsBackstop` (`status 200 after 8.01s, want 200 after the shared 5s`), `TestServeHTTP_OnboardingStopsAtShutdown` (`the onboarding outlived the shutdown`), `TestServeHTTP_MissBackstopSharesTheRequestsBudget/22s` (`status 200 after 27s, want 200 after 25s`), `TestServeHTTP_CoalescedCreatingMissFiresT1Once` (`T1 calls [], want exactly one`). The two waiter tests and the budget test's `2s` leg pass already: they pin phase D's behaviour now that a flip can reach it.

- [ ] **Step 3: Implement**

```diff
diff --git a/internal/handler/handler.go b/internal/handler/handler.go
index 69aaa64..acfb5e7 100644
--- a/internal/handler/handler.go
+++ b/internal/handler/handler.go
@@ -1455,4 +1455,14 @@ func (h *Handler) serveCacheMiss(w http.ResponseWriter, r *http.Request, req *pr
 		return
 	}
+	// Spec C3 T1-on-seed (phase D plan, design note 2): a miss that created
+	// its suite's row fires T1 once its response is done, so the new
+	// suite's eager first adoption (spec C4 Dispatch 3) starts at first
+	// contact. Only the creating request fires (a coalesced waiter shares
+	// its result), and only with adoption enabled: without it a created
+	// row is the Phase 1 observation seed (TestServeHTTP_AdoptionDisabledSeedParity).
+	// The hit path fires T1 after onboarding already (ruling PD-11).
+	if res.created && !shared && h.adoptionEnabled {
+		defer h.maybeFireFreshness(req)
+	}
 	outcome, logOutcome := cacheMiss, "miss"
 	if shared {
@@ -1479,6 +1489,13 @@ func (h *Handler) serveCacheMiss(w http.ResponseWriter, r *http.Request, req *pr
 		//
 		// AIDEV-NOTE: the body waits on the backstop, so backstopTimeout
-		// (5 s) bounds it well under apt's 30 s inactivity timeout (Task 9
-		// review, controller addition d; TestServeHTTP_BackstopIsBoundedBeforeTheBody).
+		// (5 s) bounds its database steps (Task 9 review, controller
+		// addition d; TestServeHTTP_BackstopIsBoundedBeforeTheBody); its
+		// pool reads (readHead, at most HeadBytes of a local file) take no
+		// context (deferred minor D-37). Before it the body may also wait
+		// in a flip's hold of the just-created suite (at most
+		// flipgate.HoldMax): reachable since eager first adoption — so the
+		// deadline also counts from the request's start (design review
+		// M-15, missBackstopDeadline): at most missBodyBudget (25 s) after
+		// it, under apt's 30 s inactivity timeout.
 		// On a timeout or any other failure markCachedSignedAnchors logs
 		// WARN and the body is served anyway: the body in hand was marked in
@@ -1486,5 +1503,5 @@ func (h *Handler) serveCacheMiss(w http.ResponseWriter, r *http.Request, req *pr
 		// marker pass marks the other cached anchors at the next start —
 		// the one residual (spec C3).
-		bctx, cancel := context.WithTimeout(h.lifecycleCtx, backstopTimeout)
+		bctx, cancel := context.WithDeadline(h.lifecycleCtx, missBackstopDeadline(start))
 		h.markCachedSignedAnchors(bctx, sr.key, req.Path)
 		cancel()
@@ -2057,6 +2074,6 @@ func (h *Handler) runFetch(ctx context.Context, req *proxy.Request, file aptpath
 	//
 	// AIDEV-NOTE: the sniff reads the finalized pool file (SniffReleaseGPG
-	// requires a binary signature's version byte: ruling PD-10). Phase E's
-	// T1-on-seed keys on stored.Create.Outcome == cache.SuiteCreated;
+	// requires a binary signature's version byte: ruling PD-10). T1-on-seed
+	// keys on stored.Create.Outcome == cache.SuiteCreated (sfResult.created);
 	// stored.Tracked is what the late flip-barrier registration uses.
 	// A pool file that cannot be read back (a local disk fault) is logged
@@ -2106,4 +2123,5 @@ func (h *Handler) runFetch(ctx context.Context, req *proxy.Request, file aptpath
 		vouchingSuites: vouchers,
 		suiteTracked:   stored.Tracked,
+		created:        stored.Create.Outcome == cache.SuiteCreated,
 		unproven:       stored.Create.Outcome == cache.SuiteCreated && stored.Create.Unproven,
 	}
diff --git a/internal/handler/singleflight.go b/internal/handler/singleflight.go
index 770d675..bf57f1a 100644
--- a/internal/handler/singleflight.go
+++ b/internal/handler/singleflight.go
@@ -38,4 +38,7 @@ type sfResult struct {
 	// vouchingSuites: the suites whose snapshots vouched a validated .deb.
 	vouchingSuites []string
+	// created: this miss created its suite's row (cache.SuiteCreated): the
+	// leader fires T1 once its response is done (spec C3 T1-on-seed).
+	created bool
 	// snapshotCount: the strict-mode current_snapshot_count of an
 	// errUnvouchedRetryable result, for the refusal's log line.
diff --git a/internal/handler/suite_rows.go b/internal/handler/suite_rows.go
index 842bf83..3c29bbe 100644
--- a/internal/handler/suite_rows.go
+++ b/internal/handler/suite_rows.go
@@ -330,10 +330,32 @@ func (h *Handler) cachedAnchors(ctx context.Context, key cache.SuiteKey, except
 
 // backstopTimeout bounds markCachedSignedAnchors where it runs before the
-// body — after an unproven creation on the miss path and after onboarding —
-// well under apt's 30 s inactivity timeout, like onboardTimeout (Task 9
-// review, controller addition d). On a timeout the backstop's reads or its
-// write fail and are logged WARN, and the body is served.
+// body — after an unproven creation on the miss path and after onboarding
+// (which shares one deadline with its creation, onboardTimeout) — under
+// apt's 30 s inactivity timeout (Task 9 review, controller addition d). It
+// bounds the database steps; the pool reads (readHead, at most HeadBytes
+// of a local file) take no context (deferred minor D-37). On a timeout the
+// backstop's reads or its write fail and are logged WARN, and the body is
+// served.
 const backstopTimeout = 5 * time.Second
 
+// missBodyBudget bounds, from a miss's start, how long it may wait before
+// its body (design review M-15): the fetch, the store and a flip's hold of
+// the just-created suite (at most flipgate.HoldMax) come first, then the
+// backstop of an unproven creation, and apt gives up after 30 s without a
+// byte.
+const missBodyBudget = 25 * time.Second
+
+// missBackstopDeadline is the miss path's backstop deadline:
+// backstopTimeout from now, but never past missBodyBudget from the
+// request's start (an expired deadline fails the backstop at once; it is
+// logged WARN and the body is served).
+func missBackstopDeadline(start time.Time) time.Time {
+	d := time.Now().Add(backstopTimeout)
+	if limit := start.Add(missBodyBudget); limit.Before(d) {
+		return limit
+	}
+	return d
+}
+
 // markCachedSignedAnchors is the backstop for an unproven creation
 // (cache.CreateResult.Unproven): a cached anchor row that changed or
@@ -416,7 +438,10 @@ func (h *Handler) registerLate(r *http.Request, req *proxy.Request, sr *suiteReq
 }
 
-// onboardTimeout bounds onboarding's writer operation (design review M8):
-// the body waits on it, so it stays well under apt's 30 s inactivity
-// timeout; on timeout the hit serves untracked, as on any onboarding error.
+// onboardTimeout bounds onboarding's writer operation and its backstop
+// together (design review M8; deferred minor D-35): the body waits on
+// both, and between them on a flip's hold of the just-created suite (at
+// most flipgate.HoldMax, 20 s, reachable since eager first adoption), so
+// the worst case stays at 25 s, under apt's 30 s inactivity timeout. On a
+// timeout the hit serves untracked, as on any onboarding error.
 const onboardTimeout = 5 * time.Second
 
@@ -445,5 +470,8 @@ func (h *Handler) onboard(r *http.Request, req *proxy.Request, sr *suiteReq, blo
 	}
 	key := "onboard|" + sr.key.CanonicalScheme + "|" + sr.key.CanonicalHost + "|" + sr.key.SuitePath
-	res, shared, _ := h.sf.Do(key, func() sfResult { return h.runOnboard(r, req, sr) })
+	// One deadline for the creation and the backstop (D-35), under the
+	// handler's lifecycle so a graceful shutdown cuts both short (D-34).
+	deadline := time.Now().Add(onboardTimeout)
+	res, shared, _ := h.sf.Do(key, func() sfResult { return h.runOnboard(r, req, sr, deadline) })
 	if !res.suiteTracked {
 		return nil
@@ -452,5 +480,5 @@ func (h *Handler) onboard(r *http.Request, req *proxy.Request, sr *suiteReq, blo
 	t := h.registerLate(r, req, sr)
 	if res.unproven && !shared {
-		bctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), backstopTimeout)
+		bctx, cancel := context.WithDeadline(h.lifecycleCtx, deadline)
 		h.markCachedSignedAnchors(bctx, sr.key, req.Path)
 		cancel()
@@ -463,5 +491,5 @@ func (h *Handler) onboard(r *http.Request, req *proxy.Request, sr *suiteReq, blo
 // (created, or found), and in unproven whether the creation needs the
 // backstop.
-func (h *Handler) runOnboard(r *http.Request, req *proxy.Request, sr *suiteReq) sfResult {
+func (h *Handler) runOnboard(r *http.Request, req *proxy.Request, sr *suiteReq, deadline time.Time) sfResult {
 	now := wallclock.Unix()
 	seed := h.suiteSeed(sr, false, "", "", now)
@@ -469,5 +497,5 @@ func (h *Handler) runOnboard(r *http.Request, req *proxy.Request, sr *suiteReq)
 		seed.SignedSeenAt = now
 	}
-	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), onboardTimeout)
+	ctx, cancel := context.WithDeadline(h.lifecycleCtx, deadline)
 	defer cancel()
 	seed.Cached = h.cachedAnchors(ctx, sr.key, req.Path)
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/handler/ ./cmd/...`
Expected: PASS.

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| `serveCacheMiss` never fires T1-on-seed | `TestServeHTTP_MissThatCreatesItsSuiteFiresT1`, `TestServeHTTP_CoalescedCreatingMissFiresT1Once` |
| T1-on-seed fired by coalesced waiters too (`!shared` dropped, X27) | `TestServeHTTP_CoalescedCreatingMissFiresT1Once` |
| `created` keyed on `stored.Tracked` (fires for an existing suite) | `TestServeHTTP_MissThatCreatesItsSuiteFiresT1` |
| T1-on-seed without the adoption check | `TestServeHTTP_MissThatCreatesItsSuiteFiresT1`, `TestServeHTTP_AdoptionDisabledSeedParity` |
| `serveCacheMiss`: only the leader registers (`if !shared { t = h.registerLate(…) }`) | `TestServeHTTP_CoalescedMissWaiterRegistersItsOwnServe` |
| `onboard`: only the leader registers | `TestServeHTTP_OnboardingWaiterRegistersItsOwnServe` |
| `onboard`: the backstop gets a fresh `backstopTimeout` | `TestServeHTTP_OnboardingSharesOneDeadlineWithItsBackstop` |
| `runOnboard` under `context.WithoutCancel(r.Context())` again | `TestServeHTTP_OnboardingStopsAtShutdown` |
| the miss path's backstop gets a fresh `backstopTimeout` again (M-15) | `TestServeHTTP_MissBackstopSharesTheRequestsBudget/22s` |
| `missBackstopDeadline` is the budget alone (`true \|\| limit.Before(d)`) | `TestServeHTTP_MissBackstopSharesTheRequestsBudget/2s`, `TestServeHTTP_BackstopIsBoundedBeforeTheBody/miss` |

(T1 fired before the response — the `defer` dropped, the review's X28 — is near-equivalent and has no test: the creating serve registered before its body, so the flip waits for it either way; only the timing differs.)

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/handler/seed_t1_test.go internal/handler/handler.go internal/handler/singleflight.go internal/handler/suite_rows.go
git add internal/handler/seed_t1_test.go internal/handler/handler.go internal/handler/singleflight.go internal/handler/suite_rows.go
git commit -m "Fire T1 when a miss creates its suite, and pin waiters' registration with the flip barrier"
```

### Task 11: The `adoption.adopt_unsigned` knob, its wiring, and `main`'s deferred minors

Spec C5 safeguard 1 and C11: `adoption.adopt_unsigned` (bool, default `true`, pre-populated in `defaultConfig` like every default-true bool). `buildAdopter` passes it to `AdoptionConfig.AdoptUnsigned` (Task 3), and the startup line `phase2 adoption enabled` logs it from the adopter (`Adopter.AdoptsUnsigned`). `packaging/config/config.toml.default` documents the key in this task, because `TestDefaultConfigFileDocumentsEveryKey` requires every key there; its prose in `docs/configuration.md` (with the spec C5 threat notes: the signed-seen marker's denial-of-service residual, sited at `adopt_unsigned`, overview "E:" line) is Task 15's.

Deferred minors in `cmd/apt-cacher-ultra/main.go`: D-45 (`warnSuiteCaps` treats an unvalidated `max_suites` of 0 as no cap, not as "exceeded"), D-47 (its read-error path is tested). The tests live in a new file, so `suite_caps_wiring_test.go` (D-46) and `shutdown_drain_test.go` (D-49) are not touched.

**Files:**
- Create: `internal/config/adopt_unsigned_test.go`, `cmd/apt-cacher-ultra/adopt_unsigned_wiring_test.go`
- Modify: `internal/config/config.go` (`AdoptionConfig.AdoptUnsigned`, `defaultConfig`), `internal/freshness/unsigned.go` (`Adopter.AdoptsUnsigned`), `cmd/apt-cacher-ultra/main.go` (`buildAdopter`, `warnSuiteCaps`), `packaging/config/config.toml.default`

**Interfaces:**
- Consumes: Task 3's `AdoptionConfig.AdoptUnsigned`; config test helper `writeTOML`; cmd test helpers `withKeyringDirs`, `newAdoptionEnabledCfg`, `newAdoptionWiringDeps`, `buildAdopter`, `warnSuiteCaps`; `cache.(*Cache).EnsureSuiteForTest`.
- Produces: `config.AdoptionConfig.AdoptUnsigned bool` (`toml:"adopt_unsigned"`); `freshness.(*Adopter).AdoptsUnsigned() bool`.

- [ ] **Step 1: Write the failing tests**

Create `internal/config/adopt_unsigned_test.go`:

```go
package config

import "testing"

// Spec C5 safeguard 1, C11: adoption.adopt_unsigned defaults to true, and
// an explicit false survives Load (the bool pre-populate pattern).
func TestLoad_AdoptUnsignedDefaultTrue(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(writeTOML(t, dir, "config.toml", `
[cache]
dir = "`+dir+`"
`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Adoption.AdoptUnsigned {
		t.Error("adopt_unsigned default = false, want true")
	}
	cfg, err = Load(writeTOML(t, dir, "config2.toml", `
[cache]
dir = "`+dir+`"
[adoption]
adopt_unsigned = false
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Adoption.AdoptUnsigned {
		t.Error("explicit adopt_unsigned = false was clobbered to true")
	}
}
```

Create `cmd/apt-cacher-ultra/adopt_unsigned_wiring_test.go`:

```go
package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// Spec C5 safeguard 1: adoption.adopt_unsigned reaches the adopter, and
// the startup line says what it was built with.
func TestBuildAdopter_WiresAdoptUnsigned(t *testing.T) {
	for _, on := range []bool{true, false} {
		withKeyringDirs(t, []string{t.TempDir()})
		cfg := newAdoptionEnabledCfg(t, false)
		cfg.Adoption.AdoptUnsigned = on
		c, fetcher, hosts := newAdoptionWiringDeps(t)
		var logs bytes.Buffer
		a, _, err := buildAdopter(cfg, c, fetcher, hosts, slog.New(slog.NewJSONHandler(&logs, nil)))
		if err != nil {
			t.Fatal(err)
		}
		if a.AdoptsUnsigned() != on {
			t.Fatalf("adopt_unsigned = %v, adopter built with %v", on, a.AdoptsUnsigned())
		}
		want := `"adopt_unsigned":false`
		if on {
			want = `"adopt_unsigned":true`
		}
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("startup line lacks %s:\n%s", want, logs.String())
		}
	}
}

// Deferred minors D-45 and D-47: warnSuiteCaps says nothing for an
// unvalidated max_suites of 0 (no cap, not "exceeded"), and a failed read
// of the cap usage is a WARN.
func TestWarnSuiteCaps_ZeroCapAndReadFailure(t *testing.T) {
	c, _, _ := newAdoptionWiringDeps(t)
	if err := c.EnsureSuiteForTest(context.Background(), "http", "h", "/r/dists/x"); err != nil { // one suite: "exceeds" a cap of 0
		t.Fatal(err)
	}
	var logs bytes.Buffer
	warnSuiteCaps(context.Background(), c, 0, slog.New(slog.NewJSONHandler(&logs, nil)))
	if logs.Len() != 0 {
		t.Fatalf("max_suites 0 logged:\n%s", logs.String())
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	warnSuiteCaps(context.Background(), c, 100, slog.New(slog.NewJSONHandler(&logs, nil)))
	if !strings.Contains(logs.String(), `"level":"WARN","msg":"suite cap usage read failed"`) {
		t.Fatalf("a failed cap read was not a WARN:\n%s", logs.String())
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go vet ./internal/config/ ./cmd/apt-cacher-ultra/`
Expected: FAIL — `cfg.Adoption.AdoptUnsigned undefined` in both packages (vet stops at the first error of a package; `go test -gcflags=-e ./cmd/apt-cacher-ultra/` also names `a.AdoptsUnsigned undefined`).

- [ ] **Step 3: Implement**

```diff
diff --git a/cmd/apt-cacher-ultra/main.go b/cmd/apt-cacher-ultra/main.go
index 1e39b2d..01cec8d 100644
--- a/cmd/apt-cacher-ultra/main.go
+++ b/cmd/apt-cacher-ultra/main.go
@@ -1023,4 +1023,5 @@ func buildAdopter(
 		RepairSkippedMembers:           cfg.Adoption.RepairSkippedMembers,
 		RequiredArchitectures:          cfg.Adoption.RequiredArchitectures,
+		AdoptUnsigned:                  cfg.Adoption.AdoptUnsigned,
 		Logger:                         logger,
 	})
@@ -1033,4 +1034,5 @@ func buildAdopter(
 		"trusted_signer_blocks", len(pins),
 		"require_pinned_signer", cfg.Adoption.RequirePinnedSigner,
+		"adopt_unsigned", adopter.AdoptsUnsigned(),
 		"allow_short_keyid", cfg.Adoption.AllowShortKeyID,
 		"accept_any_signer", cfg.Adoption.AcceptAnySigner,
@@ -1064,4 +1066,10 @@ func buildAdopter(
 // max_suites needlessly (PD-12).
 func warnSuiteCaps(ctx context.Context, c *cache.Cache, maxSuites int, logger *slog.Logger) {
+	// Validate keeps max_suites >= 10; an unvalidated configuration (the
+	// tests' minimalCfg) has 0, which means no cap here, not "exceeded"
+	// (deferred minor D-45).
+	if maxSuites <= 0 {
+		return
+	}
 	perHost := maxSuites / 10
 	u, err := c.SuiteCapUsage(ctx, perHost)
diff --git a/internal/config/config.go b/internal/config/config.go
index 7321b5c..4a582be 100644
--- a/internal/config/config.go
+++ b/internal/config/config.go
@@ -191,4 +191,12 @@ type AdoptionConfig struct {
 	FlipQuietPeriod Duration `toml:"flip_quiet_period"`
 
+	// AdoptUnsigned is adoption.adopt_unsigned (spec C5 safeguard 1, C11):
+	// adopt suites whose upstream publishes only an unsigned Release, in
+	// the unsigned form — which never vouches a .deb, never counts for
+	// strict mode, and is refused whenever the suite has signed history.
+	// Default true (pre-populated in defaultConfig: no zero value can tell
+	// "absent" from "false").
+	AdoptUnsigned bool `toml:"adopt_unsigned"`
+
 	// Architectures is the SPEC6_5 §5.1 per-arch adoption allowlist.
 	// Empty (default) preserves Phase 6 behavior: every Release-listed
@@ -869,4 +877,6 @@ func defaultConfig() *Config {
 			// lifetime). Opt out with `repair_skipped_members = false`.
 			RepairSkippedMembers: true,
+			// Spec C5 D6: unsigned adoption on by default, safeguarded.
+			AdoptUnsigned: true,
 		},
 		GC: GCConfig{
diff --git a/internal/freshness/unsigned.go b/internal/freshness/unsigned.go
index 013317a..8abf007 100644
--- a/internal/freshness/unsigned.go
+++ b/internal/freshness/unsigned.go
@@ -52,4 +52,8 @@ func (a *Adopter) RunUnsigned(ctx context.Context, suite SuiteRef, release []byt
 }
 
+// AdoptsUnsigned reports adoption.adopt_unsigned as the adopter was built
+// with (the startup line logs it).
+func (a *Adopter) AdoptsUnsigned() bool { return a.adoptUnsigned }
+
 // unsignedEligible is spec C5's unsigned eligibility, checked before
 // anything is downloaded (the "order of checks in runShared") and by the
diff --git a/packaging/config/config.toml.default b/packaging/config/config.toml.default
index b133bc5..ec24d1d 100644
--- a/packaging/config/config.toml.default
+++ b/packaging/config/config.toml.default
@@ -114,4 +114,10 @@ max_duration          = "30m"
 # between 1s and 15s.
 flip_quiet_period     = "10s"
+# Adopt suites whose upstream publishes only an unsigned Release (after two
+# consistent observations). Such a snapshot never vouches a .deb or counts
+# for strict mode, and is refused once a signed anchor has been served for
+# the suite (which a party able to inject one can use to block it; see the
+# reference). false keeps such suites unadopted.
+adopt_unsigned        = true
 
 [integrity]
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/config/ ./cmd/... ./internal/freshness/`
Expected: PASS, including `TestDefaultConfigFileDocumentsEveryKey`.

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| `defaultConfig`: `AdoptUnsigned: false` | `TestLoad_AdoptUnsignedDefaultTrue` |
| `buildAdopter` drops `AdoptUnsigned` | `TestBuildAdopter_WiresAdoptUnsigned` |
| the startup line logs a constant | `TestBuildAdopter_WiresAdoptUnsigned` |
| `warnSuiteCaps`: `maxSuites < 0` | `TestWarnSuiteCaps_ZeroCapAndReadFailure` |

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/config/config.go internal/config/adopt_unsigned_test.go internal/freshness/unsigned.go \
  cmd/apt-cacher-ultra/main.go cmd/apt-cacher-ultra/adopt_unsigned_wiring_test.go
git add internal/config/config.go internal/config/adopt_unsigned_test.go internal/freshness/unsigned.go \
  cmd/apt-cacher-ultra/main.go cmd/apt-cacher-ultra/adopt_unsigned_wiring_test.go packaging/config/config.toml.default
git commit -m "Add adoption.adopt_unsigned (default true) and wire it to the adopter"
```

### Task 12: Definition of done — the end-to-end scenarios and the DoD map

Spec §9's E row, the overview's "E:" line and the brief's mandatory DoD, each tied to the test that proves it. Five scenarios are new here (the production-shaped fixture rebuilt as design review I-6 asks); the rest landed with the code they pin.

| DoD item | Test (task) |
|---|---|
| Production-shaped fixture (design review I-6: 3 hosts × 8 never-adopted suites, half dormant by the v7 backfill's stamp, a reconstructed anchor row on each host, a client returning at hour 1, a rejected first adoption): active suites adopt exactly once, dormant ones make zero requests (scheduler and client checks), the returning suite adopts within one tick of its touch (M-12), the rejected one keeps its C7 schedule | `TestDoD_ActiveSuitesAdoptOnceDormantSuitesStaySilent` (12) |
| Classification table, every status class per anchor | `TestClassify_Table`, `TestCheckFirst_ClassificationPerAnchor` (6), `TestCheck_CurrentAnchorClassification` (7) |
| Form transitions both ways (incl. detached → inline) and the two-observation rule; `anchor_absent` visible | `TestCheckFirst_ProbeTreeAdoptsTheFormFound` (6), `TestCheck_DetachedSuiteMovesToInlineWhenInReleaseAppears`, `TestCheck_AbsentAnchorMovesLaterallyAfterTwoObservations`, `TestCheck_AbsentAnchorNeverMovesToUnsigned`, `TestCheck_UnsignedSuiteUpgradesWhenSigned` (7) |
| Never-downgrade at eligibility | `TestRunUnsigned_EligibilityBeforeAnyDownload` (3), `TestCheckFirst_UnsignedRefusedWithSignedHistory` (6), `TestCheck_UnsignedSuiteUpdates`, `TestCheck_UnsignedSuiteSeenSignedNeverUpdatesUnsigned` (7) |
| One never-downgrade rule at eligibility and in the commit: a marker under either scheme (M-7), or an adopted non-retired signed snapshot of the base or its `/.` twin (M-8) | `TestCommitAdoption_RefusesUnsignedForAnySignedHistory` (1), `TestRunUnsigned_EligibilityBeforeAnyDownload` (3) |
| Never-downgrade inside the commit transaction, incl. the race (paused before commit, signed anchor served, marker committed, resumed → refused) and a barrier past `max_duration`; the refusal is a state (R-E2) | `TestCommitAdoption_RefusesAnUnsignedDowngrade` (1), `TestCheck_UnsignedRefusedInsideTheCommitIsAState`, `TestRefusalKeepsCurrent_OnlyUpstreamOlderAndRollback` (3) |
| R-E1: a missing marker with a sniff-passing cached signed anchor is re-marked and refuses, also from the `/.` twin's rows (I-3); a failed mark or pool read refuses and is logged (M-1) | `TestRunUnsigned_RemarksCachedSignedAnchors`, `TestRunUnsigned_FailedRemarkRefuses`, `TestRunUnsigned_RemarkReadErrorRefuses`, `TestRunUnsigned_RemarkMarksTheTwinsBase` (3) |
| The fourth marker writer (I-2): a probe that finds a sniff-passing signature for a suite without a signed current snapshot marks the base before admitting | `TestCheckFirst_ProbedSignatureMarksTheBase` (6), `TestCheck_AbsentUnsignedReleaseTreeFindsASignature` (7) |
| §6.1-quarantined plain-text suite adopts unsigned through `Release`, never `unsigned_downgrade_refused` | `TestDoD_QuarantinedPlainTextSuiteAdoptsUnsigned` (12) |
| A never-adopted suite whose signed anchor a v6 binary served is refused unsigned on its first post-upgrade check | `TestDoD_V6ServedSignedAnchorRefusesUnsigned` (12) |
| Unsigned never changes posture (vouches nothing, never counts, never turns strict on) | `TestStrictModeReads_IgnoreUnsignedSnapshots` (1), `TestServeHTTP_UnsignedNeverChangesStrictPosture` (12) |
| Unsigned candidates set `Form: FormUnsigned`, store `anchor_form = 'unsigned'`, vouch no `.deb` on any path | `TestRunUnsigned_AdoptsWithoutVouching`, `TestReconcile_UnsignedSnapshotGetsNoPackageHash` (3) |
| Step 3c seeds `<suite>/Release` for unsigned; GC guard (d) keeps it; a reaped one is reconstructed | `TestCommitAdoption_UnsignedSeedsItsReleaseAnchor`, `TestRunURLPathGCBatch_IdentityGuardFollowsTheAnchorForm` (1), `TestCheck_UnsignedRecoversWhenReleaseAnchorMissing` (7) |
| Unsigned confirmation (404 and 401/403 timings), member cap — refused before a slot is taken (M-5) | `TestCheckFirst_UnsignedTransitionIsConfirmed`, `TestCheckFirst_UnsignedAfterADeniedSignatureWaitsAnHour`, `TestCheckFirst_UnsignedTooLargeNeverTakesASlot` (6), `TestUnsignedKeysAndReasons` (3), `TestCheck_UnsignedSuiteTooLargeUpdateIsRefused` (7) |
| Pacing (rulings PE-2, PE-3): a deferred never-adopted suite and an `anchor_absent` suite make no request before their record's retry or the next refresh check | `TestCheckFirst_DeferredSuiteIgnoresClientChecksUntilItsRetry` (6), `TestCheck_AbsentAnchorIsPacedByItsRecord` (7) |
| A cross-form request records no validators; the current anchor's own are sent while a record is active (I-1) | `TestCheck_DetachedSuiteKeepsAdoptingWhileItsLateralInReleaseFails`, `TestCheck_LateralFailingContentKeepsItsRecord` (7) |
| The stale-suite WARN leaves out suites in a refusal state (M-13) | `TestStaleSuiteCounts_RefusalStates`, `TestTick_StaleWarnLeavesOutRefusalStates` (7) |
| nvidia-shaped fixture adopts (SHA512-only Release, Date 2018, SHA256 Packages) | `TestAdopter_SHA512OnlyReleaseAdopts` (2), `TestDoD_NvidiaShapedSuiteAdopts` (12) |
| SHA512 merge, every-hash verification, aliases; SHA512 by-hash served, also from a displaced snapshot | `TestParseRelease_MergesHashBlocks`, `TestAdopter_VerifiesEveryDeclaredHash`, `TestReconcile_HealsASHA512OnlyMember` (2), `TestServeHTTP_SHA512ByHashFromAnAdoption` (2) |
| C10 wait on the hit and the miss path, bounded by the request's age (M-10); 64-waiter cap; adoption disabled → no wait | `TestServeHTTP_StrictHitWaitsForAnAdoptionInFlight`, `TestServeHTTP_StrictMissWaitsForAnAdoptionInFlight`, `TestServeHTTP_StrictWaitRefusals`, `TestHostAdoptions_WaiterCap`, `TestHandlerConfig_StrictModeWaitsOnTheCheckersAdoptions`, `TestChecker_WaitIsBoundedByTheRequestsAge`, `TestServeHTTP_StrictMissClientLeftDuringTheWait` (9) |
| T1-on-seed on the miss path, keyed on the creation, fired by the creating request only | `TestServeHTTP_MissThatCreatesItsSuiteFiresT1`, `TestServeHTTP_CoalescedCreatingMissFiresT1Once` (10) |
| The miss path's backstop deadline counts from the request's start (M-15) | `TestServeHTTP_MissBackstopSharesTheRequestsBudget` (10) |
| Waiters' late registration, both paths | `TestServeHTTP_CoalescedMissWaiterRegistersItsOwnServe`, `TestServeHTTP_OnboardingWaiterRegistersItsOwnServe` (10) |
| Capacity upgrade burst: 50 never-adopted suites with interest, signed suites never wait more than one adoption, ≤ 2 probes per queued suite; also all on one host | `TestSimCapacity_UpgradeBurst`, `TestSimCapacity_UpgradeBurstSameHost` (8) |
| One check tail; unexpected statuses counted as failed (R-E3); absent statuses reset the streak (R-E4) | `TestCheck_UnexpectedStatusIsAFailedCheck` (4), `TestCheckFirst_ClassificationPerAnchor`, `TestCheck_AbsentAnchorMovesLaterallyAfterTwoObservations` (6, 7) |
| `signedCurrent = form != unsigned` for repair and reconcile | `TestCheck_RepairOfAnUnsignedSuiteNeverTakesTheReservedSlot`, `TestReconcile_UnsignedSuiteNeverTakesTheReservedSlot` (7) |
| Waiters' signed flag current | `TestTick_RefreshesTheWaitersSignedFlag` (8) |
| `adoption.adopt_unsigned` default true, wired | `TestLoad_AdoptUnsignedDefaultTrue`, `TestBuildAdopter_WiresAdoptUnsigned` (11) |

The `/.` twin variant of the in-commit race test belongs to phase G (Task 15 records it in the overview's G carry-forwards).

**Files:**
- Create: `internal/freshness/dod_e_test.go`, `internal/handler/unsigned_posture_test.go`

**Interfaces:**
- Consumes: everything above; test helpers `newRetryRig`, `retryRig.release/releaseText/unsignedRelease/currentSnapshot/check/row`, `signedRelease`, `seedStaleAnchorAt`, `startSched`, `errRigUntrusted`, `fakePackagesStanzas`, `newFakeFetcher`, `sha512Of`, `discardAuditLogger`, `lastResult`; `RunLegacyTrustAudit`, `Adopter.writeBlobBytes`, `contentKey`, `backoffDelay`, `interestWindow`, `checkDeadline`; cache `PutURLPath`, `SeedSuiteForTest`, `TouchSuite`; handler helpers `newPhase3StrictHandler(Capturing)`, `adoptCoverageComplete`, `writeBlob`, `splitURL`, `hostKey`, `proxyReq`, `sha256Hex`.
- Produces: test helpers `dodVerifier` (a `Verifier` recording each suite's verification times, rejecting chosen content keys) and `commitLegacyInline(t, r *retryRig, anchor []byte) int64` (`dod_e_test.go`), `commitUnsigned(t, h, scheme, host, suite, debPath string)` (`unsigned_posture_test.go`).

- [ ] **Step 1: Write the scenario tests**

Create `internal/freshness/dod_e_test.go`:

```go
package freshness

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// dodVerifier passes every InRelease through as its own plaintext and
// records each verification per suite; content keys in reject fail as an
// untrusted signer would.
type dodVerifier struct {
	mu     sync.Mutex
	reject map[string]bool
	calls  map[SuiteRef][]time.Time
}

func (v *dodVerifier) VerifyInline(_ context.Context, s SuiteRef, b []byte) ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls[s] = append(v.calls[s], time.Now())
	if v.reject[contentKey(adoptionFormInline, b, nil, nil)] {
		return nil, errRigUntrusted
	}
	return b, nil
}

func (v *dodVerifier) VerifyDetached(_ context.Context, _ SuiteRef, rel, _ []byte) ([]byte, error) {
	return rel, nil
}

func (v *dodVerifier) attempts(s SuiteRef) []time.Time {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]time.Time(nil), v.calls[s]...)
}

// Phase E DoD (spec §9 row E; design review I-6): a production-shaped
// fixture under the real scheduler at a 15-minute refresh — 3 hosts × 8
// never-adopted suites; on each host half are dormant by the v7 backfill
// rule (interest stamped migration time minus the 7-day window), three are
// used by clients today, and one has interest but only a reconstructed
// anchor row (request_count 0). Asserted: every active suite adopts exactly
// once (verifications); every dormant suite costs zero upstream requests,
// also to a client check; a dormant suite whose client returns at hour 1 —
// its hit's T1 running before the touch is visible, the handler's race
// (design review M-12) — adopts once, within one scheduler tick of the
// touch; and an active suite whose first adoption is rejected keeps the
// spec C7 schedule: never retried before its backoff, and not frozen.
func TestDoD_ActiveSuitesAdoptOnceDormantSuitesStaySilent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		c := simtest.OpenCache(t)
		members := newFakeFetcher()
		ver := &dodVerifier{reject: map[string]bool{}, calls: map[SuiteRef][]time.Time{}}
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: ver, HostLimiter: hostsem.New(8),
			MaxConcurrent: 2, Logger: simtest.DiscardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		up := simtest.NewUpstream()
		anchorURL := func(s SuiteRef) string { return "http://" + s.CanonicalHost + s.SuitePath + "/InRelease" }
		backfill := time.Now().Add(-interestWindow).Unix()
		var active, dormant []SuiteRef
		var rejected, returning SuiteRef
		for h := 0; h < 3; h++ {
			host := fmt.Sprintf("h%d", h)
			for i := 0; i < 8; i++ {
				s := SuiteRef{CanonicalScheme: "http", CanonicalHost: host, SuitePath: fmt.Sprintf("/r/dists/s%d", i)}
				pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_" + host + s.SuitePath + ".deb": testHex64})
				members.put("http://"+host+s.SuitePath+"/main/binary-amd64/Packages", pkgs)
				in := signedRelease("v1 "+host+s.SuitePath, 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})
				up.Set(anchorURL(s), simtest.Serve(in))
				switch {
				case i < 3: // clients use it today: interest now, a client-created anchor
					seedStaleAnchorAt(t, c, host, s.SuitePath)
					active = append(active, s)
					if h == 0 && i == 2 {
						rejected = s
						ver.reject[contentKey(adoptionFormInline, in, nil, nil)] = true
					}
				case i == 3: // interest now, but its anchor row was reconstructed (request_count 0)
					stale := testHex64
					if err := c.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: host,
						Path: s.SuitePath + "/InRelease", BlobHash: &stale, UpstreamURL: anchorURL(s), IsMetadata: true}); err != nil {
						t.Fatal(err)
					}
					if err := c.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: host,
						SuitePath: s.SuitePath}); err != nil {
						t.Fatal(err)
					}
					dormant = append(dormant, s)
				default: // the v7 backfill: dormant
					seedStaleAnchorAt(t, c, host, s.SuitePath)
					if err := c.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: host,
						SuitePath: s.SuitePath, LastRequestedAt: &backfill}); err != nil {
						t.Fatal(err)
					}
					if h == 0 && i == 4 {
						returning = s
					} else {
						dormant = append(dormant, s)
					}
				}
			}
		}
		chk, stop := startSched(t, Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute,
			Cooldown: time.Minute, Adopter: ad, Logger: simtest.DiscardLogger(), rand: func() float64 { return 0.5 }})
		time.Sleep(time.Hour + 7*time.Second)
		// The returning client: the hit's T1 runs before its touch is
		// visible (both are asynchronous in the handler), so it finds the
		// suite dormant; the scheduler's next tick sees the touch.
		chk.Check(ctx, "http", returning.CanonicalHost, returning.SuitePath)
		if n := len(up.Calls(anchorURL(returning))); n != 0 {
			t.Fatalf("returning %v: %d upstream requests before its touch, want 0", returning, n)
		}
		touched := time.Now()
		if err := c.TouchSuite(ctx, cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: returning.CanonicalHost,
			SuitePath: returning.SuitePath}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2*time.Hour - 7*time.Second)
		stop()
		for _, s := range dormant { // a client check (T1) of a dormant suite asks upstream nothing either
			chk.Check(ctx, "http", s.CanonicalHost, s.SuitePath)
		}

		for _, s := range append(active, returning) {
			if s == rejected {
				continue
			}
			if sf, err := c.GetSuiteFreshness(ctx, "http", s.CanonicalHost, s.SuitePath); err != nil || sf.CurrentSnapshotID == nil {
				t.Fatalf("%v: not adopted (err %v)", s, err)
			}
			if n := len(ver.attempts(s)); n != 1 {
				t.Fatalf("%v: %d verifications, want exactly one adoption", s, n)
			}
		}
		const tick = 15 * time.Minute / 4 // the scheduler's tick: periodic_refresh / 4
		if at := ver.attempts(returning); at[0].Sub(touched) > tick+checkDeadline {
			t.Fatalf("returning %v: adopted %v after its touch, want within one scheduler tick (%v) and its check",
				returning, at[0].Sub(touched), tick)
		}
		for _, s := range dormant {
			if n := len(up.Calls(anchorURL(s))); n != 0 {
				t.Fatalf("dormant %v: %d upstream requests, want 0", s, n)
			}
		}
		// The rejected suite keeps its record's schedule (spec C7): each
		// retry at least its backoff after the attempt before it, and the
		// record counts every attempt.
		at := ver.attempts(rejected)
		if len(at) < 5 {
			t.Fatalf("rejected %v: %d attempts in 3 h, want the C7 schedule's retries (not frozen)", rejected, len(at))
		}
		for i := 1; i < len(at); i++ {
			if gap := at[i].Sub(at[i-1]); gap < backoffDelay(int64(i)) {
				t.Fatalf("rejected %v: attempt %d came %v after the one before, want at least its %v backoff",
					rejected, i+1, gap, backoffDelay(int64(i)))
			}
		}
		sf, err := c.GetSuiteFreshness(ctx, "http", rejected.CanonicalHost, rejected.SuitePath)
		if err != nil || sf.CurrentSnapshotID != nil || sf.AdoptionFailCount != int64(len(at)) {
			t.Fatalf("rejected %v: row %+v (err %v), want no snapshot and a record of %d attempts", rejected, sf, err, len(at))
		}
	})
}

// commitLegacyInline commits an inline snapshot of the rig suite whose
// InRelease blob is anchor, with the rig's Packages member — what a v6
// binary adopted.
func commitLegacyInline(t *testing.T, r *retryRig, anchor []byte) int64 {
	t.Helper()
	ctx := context.Background()
	ah, err := r.adopter.writeBlobBytes(ctx, anchor)
	if err != nil {
		t.Fatal(err)
	}
	ph, err := r.adopter.writeBlobBytes(ctx, r.pkgs)
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := r.cache.InsertTrackedCandidateForTest(ctx, cache.SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite, InReleaseHash: &ah})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.cache.CommitAdoption(ctx, id, []cache.SnapshotMember{
		{SnapshotID: id, Path: "InRelease", BlobHash: ah, DeclaredSHA256: ah},
		{SnapshotID: id, Path: "main/binary-amd64/Packages", BlobHash: ph, DeclaredSHA256: ph},
	}, nil, nil, nil, true); err != nil {
		t.Fatal(err)
	}
	return id
}

// Phase E DoD (overview "E:" line, review finding 2; spec §6.1): a suite a
// v6 binary adopted from a plain-text "InRelease" (the old
// require_signature = false fallback), quarantined by phase A's audit,
// adopts in the unsigned form through Release with no upstream change and
// never meets unsigned_downgrade_refused.
func TestDoD_QuarantinedPlainTextSuiteAdoptsUnsigned(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopter.adoptUnsigned = true
	ctx := context.Background()
	text := r.releaseText("plain", 0)
	old := commitLegacyInline(t, r, text)
	// A pre-v7 cache has no markers; the v7 migration queues the audit.
	if err := r.cache.ClearSignedSeenForTest(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.cache.EnqueueLegacyTrustAudit(ctx, old); err != nil {
		t.Fatal(err)
	}
	if res, err := RunLegacyTrustAudit(ctx, r.cache, discardAuditLogger()); err != nil || res.Quarantined != 1 {
		t.Fatalf("audit = %+v (%v), want the plain-text snapshot quarantined", res, err)
	}
	if r.row().CurrentSnapshotID != nil {
		t.Fatal("setup: the quarantined snapshot is still current")
	}
	// Upstream as it was: the same plain text as "InRelease" and as
	// Release, no signature.
	r.up.set(text)
	r.unsignedRelease("plain", nil)
	for _, step := range []time.Duration{0, 2 * time.Minute, 4 * time.Minute} {
		r.clock.advance(step)
		r.check()
		if strOf(r.row().AdoptionFailReason) == cache.ReasonUnsignedDowngradeRefused {
			t.Fatal("the quarantined plain-text suite was refused as a downgrade")
		}
	}
	snap := r.currentSnapshot()
	if snap == nil || snap.Form != cache.FormUnsigned || strOf(snap.SingleAnchorHash) != hashOf(text) {
		t.Fatalf("snapshot %+v, want the unsigned adoption of the same Release", snap)
	}
}

// Phase E DoD (overview "E:" line, review finding 2): a never-adopted
// suite whose signed InRelease a v6 binary served (cached, no marker) is
// refused unsigned adoption on its first post-upgrade check: phase A's
// startup pass marks the cached signed anchor.
func TestDoD_V6ServedSignedAnchorRefusesUnsigned(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopter.adoptUnsigned = true
	ctx := context.Background()
	signed, err := r.adopter.writeBlobBytes(ctx, r.release("served by v6", 0))
	if err != nil {
		t.Fatal(err)
	}
	served := int64(1_799_000_000)
	if err := r.cache.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: rigHost,
		Path: rigSuite + "/InRelease", BlobHash: &signed, UpstreamURL: rigAnchorURL, IsMetadata: true,
		RequestCount: 1, LastRequestedAt: &served}); err != nil {
		t.Fatal(err)
	}
	if _, err := RunLegacyTrustAudit(ctx, r.cache, discardAuditLogger()); err != nil { // the startup pass
		t.Fatal(err)
	}
	// Upstream now publishes only an unsigned Release.
	r.unsignedRelease("unsigned now", nil)
	r.check()
	if row := r.row(); strOf(row.AdoptionFailReason) != cache.ReasonUnsignedDowngradeRefused || row.CurrentSnapshotID != nil {
		t.Fatalf("record %q current %v, want unsigned_downgrade_refused and nothing adopted",
			strOf(row.AdoptionFailReason), row.CurrentSnapshotID)
	}
}

// Phase E DoD (spec §3, libnvidia-container): an InRelease-only suite
// whose Release text has only a SHA512 block and a Date frozen in 2018,
// with a SHA256 Packages, adopts on its first check and vouches its .deb.
func TestDoD_NvidiaShapedSuiteAdopts(t *testing.T) {
	r := newRetryRig(t, 0)
	text := fmt.Sprintf("Origin: https://nvidia.github.io/libnvidia-container/stable/deb/amd64\n"+
		"Label: https://nvidia.github.io/libnvidia-container/stable/deb/amd64\n"+
		"Date: Wed, 09 May 2018 22:33:58 GMT\n"+
		"SHA512:\n %s %d main/binary-amd64/Packages\n", sha512Of(r.pkgs), len(r.pkgs))
	r.up.set(simtest.ClearsignedFixture(text))
	r.check()
	snap := r.currentSnapshot()
	if lastResult(r) != "first_adoption" || snap == nil || snap.Form != cache.FormInline {
		t.Fatalf("result %q snapshot %+v, want an inline first adoption", lastResult(r), snap)
	}
	rows, err := r.cache.DeclaredHashesForPath(context.Background(), "http", rigHost, "/r/pool/main/f/foo/foo_1.deb")
	if err != nil || len(rows) != 1 || rows[0].DeclaredSHA256 != testHex64 {
		t.Fatalf("package_hash rows %+v (%v), want the .deb vouched by its SHA256", rows, err)
	}
}
```

Create `internal/handler/unsigned_posture_test.go`:

```go
package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// commitUnsigned adopts an unsigned snapshot of suite with complete
// coverage and — as if a bug wrote one — a package_hash row vouching
// debPath: whatever it holds, it must neither vouch nor count.
func commitUnsigned(t *testing.T, h *Handler, scheme, host, suite, debPath string) {
	t.Helper()
	ctx := context.Background()
	rel := writeBlob(t, h, []byte("Origin: unsigned\nSHA256:\n"))
	if err := h.cache.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: scheme, CanonicalHost: host,
		SuitePath: suite}); err != nil {
		t.Fatal(err)
	}
	id, _, err := h.cache.InsertTrackedCandidateForTest(ctx, cache.SnapshotCandidate{CanonicalScheme: scheme,
		CanonicalHost: host, SuitePath: suite, InReleaseHash: &rel, Form: cache.FormUnsigned})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.cache.CommitAdoption(ctx, id, []cache.SnapshotMember{{SnapshotID: id, Path: "Release", BlobHash: rel,
		DeclaredSHA256: rel}}, nil, []cache.PackageHash{{SnapshotID: id, CanonicalScheme: scheme, CanonicalHost: host,
		Path: debPath, DeclaredSHA256: sha256Hex([]byte("upstream bytes"))}}, nil, true); err != nil {
		t.Fatal(err)
	}
}

// Phase E DoD (spec C5 safeguard 5, C10): an unsigned snapshot never
// changes a host's strict-mode posture — it vouches no .deb on a host
// strict mode protects, and on a host with no signed snapshot it does not
// turn strict mode on.
func TestServeHTTP_UnsignedNeverChangesStrictPosture(t *testing.T) {
	const deb = "/pool/main/u/unsigned_1_amd64.deb"
	t.Run("a protected host stays protected", func(t *testing.T) {
		h, srv, logs := newPhase3StrictHandlerCapturing(t, true, true, nil)
		scheme, host, port := splitURL(t, srv.URL)
		canon := hostKey(host, port)
		adoptCoverageComplete(t, h, scheme, canon, "/dists/signed", true)
		commitUnsigned(t, h, scheme, canon, "/dists/unsigned", deb)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, proxyReq("GET", srv.URL, deb))
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status %d, want the strict refusal: an unsigned snapshot vouched the .deb", rec.Code)
		}
		for _, r := range logs.records() {
			if r["msg"] == "unvouched_deb_refused" && r["current_snapshot_count"] != float64(1) {
				t.Fatalf("current_snapshot_count %v, want 1 (the unsigned snapshot does not count)", r["current_snapshot_count"])
			}
		}
	})
	t.Run("an unsigned-only host stays unprotected", func(t *testing.T) {
		h, srv := newPhase3StrictHandler(t, true, true, nil)
		scheme, host, port := splitURL(t, srv.URL)
		commitUnsigned(t, h, scheme, hostKey(host, port), "/dists/unsigned", deb)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, proxyReq("GET", srv.URL, deb))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want the passthrough: an unsigned snapshot turned strict mode on", rec.Code)
		}
	})
}
```

- [ ] **Step 2: Run them**

Run: `go test -race -run 'TestDoD_|TestServeHTTP_UnsignedNeverChangesStrictPosture' ./internal/freshness/ ./internal/handler/`
Expected: PASS (the behaviour landed in Tasks 1–11; these pin it end to end). `TestDoD_ActiveSuitesAdoptOnceDormantSuitesStaySilent` runs in about 1.5 s under `-race`.

- [ ] **Step 3: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| `DeclaredHashesForPath` without its `anchor_form <> 'unsigned'` filter | `TestServeHTTP_UnsignedNeverChangesStrictPosture/a_protected_host_stays_protected` |
| `HostCurrentSnapshotsCoverage` without its filter | `TestServeHTTP_UnsignedNeverChangesStrictPosture` (both legs) |
| `checkFirst`: every suite interested | `TestDoD_ActiveSuitesAdoptOnceDormantSuitesStaySilent` |
| `cachedAnchors`: a reconstructed row counts as client-created (`row.RequestCount > 0` dropped) | `TestDoD_ActiveSuitesAdoptOnceDormantSuitesStaySilent` |
| a failure record's retry ignores its count (`backoffDelay(1)`) | `TestDoD_ActiveSuitesAdoptOnceDormantSuitesStaySilent` |
| `TouchSuite` writes nothing (the returning client's interest is lost) | `TestDoD_ActiveSuitesAdoptOnceDormantSuitesStaySilent` |
| the startup pass marks nothing (`sniffCachedAnchor` false) | `TestDoD_V6ServedSignedAnchorRefusesUnsigned` |
| the startup pass marks a plain-text anchor (`sniffCachedAnchor` true) | `TestDoD_QuarantinedPlainTextSuiteAdoptsUnsigned` |
| `ParseRelease` ignores SHA512 blocks | `TestDoD_NvidiaShapedSuiteAdopts` |

(`suiteDue`'s dormancy clause alone cannot change a request count — the check's gate stops a dormant suite too — so `TestSuiteDue_DormantNeverAdoptedSuites` (Task 6) pins it.)

- [ ] **Step 4: Run the whole suite**

Run: `go test -race ./... && golangci-lint run ./... && gofmt -l .`
Expected: PASS; 0 issues; only the five drift files.

- [ ] **Step 5: Commit**

```bash
gofmt -l internal/freshness/dod_e_test.go internal/handler/unsigned_posture_test.go
git add internal/freshness/dod_e_test.go internal/handler/unsigned_posture_test.go
git commit -m "Pin phase E's definition of done end to end"
```

### Task 13: Deferred minors in the files this phase changes (code)

The deferred-minor rule (brief): every untagged or E-tagged item in a file a task changes by hand is taken; items tagged for another phase stay. The items below sit in files Tasks 1–12 change and are not taken there. (Tasks 4, 7, 8, 9, 10 and 11 took B-23, B-25, B-30–B-35, B-37, B-38, B-45, B-46, B-66, D-23, D-33, D-34, D-35, D-37, D-45, D-47; Task 1 A-29; Task 3 A-49 and C-16; the simulation items are Task 14's; the documentation items Task 15's.)

| ID | File | Change |
|---|---|---|
| A-37 | `freshness.go`, `adoption.go` | One helper, `logPanic`, logs every recovered panic in the package (the stack once per kind); `reportPanic` and the adopter's heartbeat use it (the heartbeat now logs its stack the first time — D-51's lost fact) |
| A-47 | `legacy_audit.go` | An unsigned snapshot the audit keeps is audited, not counted as signed |
| B-16 | `retry_test.go` | The detached content key is pinned to `sha256(sha256(Release) ‖ sha256(Release.gpg))` |
| B-17 | `retry.go` | `jitterDelay` clamps the random value to [0, 1] (and NaN to 0) |
| B-18 | `adoption_failure.go`, `freshness.go` | `recoverAdoption`'s reporting, and `recordAdoptionFailure`'s own recover, run under a second boundary (`unloggedPanic`): a panicking log handler neither escapes nor skips the record, and is counted (`acu_freshness_unlogged_panics_total`) |
| B-19 | `adoption_failure_test.go` | Tests: a reason change mid-streak re-logs `adoption_deferred` with the retry attributes; `AdoptionFailLastMod`; a detached record's key |
| B-20 | `adoption_failure.go` | A record write that a shutdown cut short is logged at DEBUG, not WARN |
| B-21 | `adoption_failure_test.go` | `TestAdoptionFailure_PanicIsRecorded` finds the panicked event by outcome |
| B-24 | `retry_gate_test.go` | `TestGate_DueRetryHasNoValidators` runs on an adopted suite, whose anchor has validators |
| B-28 | `reconcile.go`, `freshness.go` | A tick's heal reuses the declared set its decision parsed: one parse per heal (`reconcilePending` returns it; `reconcileSnapshot` takes it) |
| C-17 | `adoption.go` (now `flip_barrier.go`) | Test: without a stop context, a cancelled `ctx` before the commit is a shutdown and commits nothing |
| C-19 | `adoption.go` | The flip-barrier code moves, unchanged, to `flip_barrier.go` |
| C-29 | `legacy_audit.go` | `RunLegacyTrustAudit`'s doc states the "legacy trust audit:" prefix rule; the batch seam's error gets the prefix too |
| D-21 | `cache/adoption.go`, `cache/snapshot_reads.go` | Tests pin both scheme predicates (an `https` twin adopted later; an adopted `https` snapshot holding the alias) |
| D-22 | `cache/adoption_scope_test.go` | The stamp test reports a read error instead of panicking on a nil snapshot |
| D-38 | `handler/handler.go` | A fetched signed anchor whose pool file cannot be read back is a local fault: `fetch.ErrCacheWriteFailed`, outcome `cache_write_failed` |
| D-5 | `aptpath/sniff_test.go` | Tagged E (no other task changes the file): `TestDecodableClearsign` gains a marker line with exactly one trailing blank, as long as the `\r` case, so `DecodableClearsign`'s `b[end-1] == '\r'` test is pinned |
| D-1 | `aptpath/sniff_test.go` | Untagged, in the file D-5 changes: `TestClassifySuiteFile`'s empty-suite row becomes `{"", "/InRelease", NotSuiteFile}` (only the `suite == ""` guard refuses it) and gains `{"", "/Packages.gz", NotSuiteFile}` |
| D-2 | `aptpath/sniff_test.go` | Untagged, same file: `TestSniffRelease` gains a header whose colon is one byte past the 64 KiB window, so an overrun of the window fails |

Checked and left (tagged for another phase, or in a file no task changes by hand): see Task 15's intro, which lists every ID with its file.

Two tests here meet ruling PE-3 (a deferred never-adopted suite asks upstream nothing before its record's retry): `TestAdoptionPanicWithABrokenLoggerIsContained`'s second check runs past the record's retry, and B-19's `TestAdoptionFailure_ReasonChangeValidatorsAndDetachedKey` runs on an adopted suite, where new content is attempted at once. The record write keeps Task 7's cross-form rule next to the new `recordHook`.

**Files:**
- Create: `internal/freshness/flip_barrier.go`, `internal/freshness/deferred_e_test.go`, `internal/handler/anchor_readback_test.go`
- Modify: `internal/freshness/adoption.go`, `internal/freshness/adoption_failure.go`, `internal/freshness/freshness.go`, `internal/freshness/legacy_audit.go`, `internal/freshness/reconcile.go`, `internal/freshness/retry.go`, `internal/handler/handler.go`; tests `internal/aptpath/sniff_test.go` (D-1, D-2, D-5), `internal/freshness/adoption_failure_test.go`, `internal/freshness/retry_gate_test.go`, `internal/freshness/retry_test.go`, `internal/cache/adoption_scope_test.go`

**Interfaces:**
- Consumes: Task 5's `retryRig.adopt`, `releaseText`, `armoredSig`, `signedRelease`; Task 6's `seedDetachedAnchors` use; Task 7's `retryRig.adoptUnsigned`; test helpers `simMembers`, `logRecords`, `panicLogHandler`, `renderedMetric`, `newSuiteRig`, `suiteRig.unreadableBlob`, cache `commitFor`, `seedBlob`, `stubNow`.
- Produces: `logPanic(logger *slog.Logger, event, where string, suite SuiteRef, r any)`, `unloggedPanic(report func())`; `(*Adopter).reconcileSnapshot(ctx, suite, snapshotID int64, declared []ReleaseMember, force bool) (int, error)`; `reconcilePending` now returns `([]ReleaseMember, bool, error)`; `checkOutcome.declared`; test seams `Adopter.heartbeatHook`, `Adopter.reparseHook`, `Checker.recordHook`.

- [ ] **Step 1: Move the barrier code (C-19)**

Move these six declarations, with their doc comments and unchanged, from `internal/freshness/adoption.go` to a new file `internal/freshness/flip_barrier.go`: `(*Adopter).FlipGate`, `(*adoptionPayload).stopErr`, `(*adoptionPayload).barrierCtx`, `(*Adopter).flip`, `barrierOutcome`, `(*Adopter).servedAnchorUnchanged`. The new file starts:

```go
package freshness

// The spec C8 flip barrier, the adopter's side: the commit waits behind the
// barrier (flip), the shutdown signal and the context the barrier waits
// under (stopErr, barrierCtx), the barrier event's outcome, and the first
// adoption that skips it (servedAnchorUnchanged). Split out of adoption.go
// unchanged (deferred minor C-19).

import (
	"context"
	"errors"
	"fmt"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"
)
```

Run: `go build ./... && go test ./internal/freshness/` — PASS (a move only; it runs first, on the Task 12 tree, because Step 2's tests use the three test seams Step 4 adds — executability review D2).

- [ ] **Step 2: Write the failing tests**

Create `internal/freshness/deferred_e_test.go`:

```go
package freshness

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// unlogged returns acu_freshness_unlogged_panics_total.
func unlogged() float64 {
	v, _ := strconv.ParseFloat(renderedMetric("acu_freshness_unlogged_panics_total"), 64)
	return v
}

// Deferred minor A-37: the adopter's heartbeat logs its panic through the
// package's one helper — adoption_panicked, where=heartbeat, the stack the
// first time — and the adoption still commits.
func TestHeartbeatPanicIsLoggedWithItsStackOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		panicStackLogged.Delete("heartbeat")
		c := simtest.OpenCache(t)
		members := &simMembers{fakeFetcher: newFakeFetcher(), stalled: map[string]bool{}, delay: map[string]time.Duration{}}
		pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": testHex64})
		members.put(rigPkgsURL, pkgs)
		members.delay[rigPkgsURL] = 3 * time.Second // the heartbeat ticks while the member arrives
		logs := &lockedBuffer{}
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
			HostLimiter: hostsem.New(8), HeartbeatInterval: time.Second, Logger: slog.New(slog.NewJSONHandler(logs, nil))})
		if err != nil {
			t.Fatal(err)
		}
		ad.heartbeatHook = func() { panic("heartbeat bug") }
		ref := SuiteRef{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite}
		trackSuite(t, c, ref)
		if err := ad.Run(context.Background(), ref, signedRelease("v1", 0, map[string][]byte{"main/binary-amd64/Packages": pkgs}), "", ""); err != nil {
			t.Fatalf("the adoption failed after a heartbeat panic: %v", err)
		}
		recs := logRecords(t, logs, "adoption_panicked")
		if len(recs) != 1 || recs[0]["where"] != "heartbeat" || recs[0]["level"] != "ERROR" || recs[0]["stack"] == nil ||
			recs[0]["suite_path"] != rigSuite {
			t.Fatalf("adoption_panicked records %v, want one ERROR where=heartbeat with the suite and the stack", recs)
		}
	})
}

// Deferred minor A-47: the audit counts an unsigned snapshot it keeps as
// audited, not as signed.
func TestRunLegacyTrustAudit_UnsignedKeepIsNotSigned(t *testing.T) {
	r := newRetryRig(t, 0)
	ctx := context.Background()
	r.adoptUnsigned("u1")
	id := *r.row().CurrentSnapshotID
	if err := r.cache.EnqueueLegacyTrustAudit(ctx, id); err != nil {
		t.Fatal(err)
	}
	res, err := RunLegacyTrustAudit(ctx, r.cache, discardAuditLogger())
	if err != nil || res.Audited != 1 || res.Signed != 0 || res.Quarantined != 0 {
		t.Fatalf("audit = %+v (%v), want 1 audited, 0 signed, 0 quarantined", res, err)
	}
}

// Deferred minor C-29: every error RunLegacyTrustAudit returns starts
// "legacy trust audit:" (main's refusal line relies on it).
func TestRunLegacyTrustAudit_ErrorsCarryThePrefix(t *testing.T) {
	r := newRetryRig(t, 0)
	ctx := context.Background()
	r.adoptUnsigned("u1")
	if err := r.cache.EnqueueLegacyTrustAudit(ctx, *r.row().CurrentSnapshotID); err != nil {
		t.Fatal(err)
	}
	prev := legacyAuditAfterBatch
	t.Cleanup(func() { legacyAuditAfterBatch = prev })
	boom := errors.New("batch hook failed")
	legacyAuditAfterBatch = func(int) error { return boom }
	_, err := RunLegacyTrustAudit(ctx, r.cache, discardAuditLogger())
	if !errors.Is(err, boom) || !strings.HasPrefix(err.Error(), "legacy trust audit: ") {
		t.Fatalf("err = %v, want the batch error with the prefix", err)
	}
}

// Deferred minor B-17: a random value outside [0, 1) or NaN cannot push
// the delay past ±25 %.
func TestJitterDelay_ClampsTheRandomValue(t *testing.T) {
	d := 4 * time.Minute
	for _, tc := range []struct {
		u    float64
		want time.Duration
	}{{-1, 3 * time.Minute}, {math.NaN(), 3 * time.Minute}, {0, 3 * time.Minute}, {0.5, d}, {1, 5 * time.Minute}, {7, 5 * time.Minute}} {
		if got := jitterDelay(d, tc.u); got != tc.want {
			t.Errorf("jitterDelay(%v, %v) = %v, want %v", d, tc.u, got, tc.want)
		}
	}
}

// Deferred minor B-18: when the logger itself panics while an adoption
// panic is reported, the panic does not escape, the mutex is released, the
// failure is still recorded, and the unloggable panic is counted.
func TestAdoptionPanicWithABrokenLoggerIsContained(t *testing.T) {
	r := newRetryRig(t, 0)
	body := r.release("v1", 0)
	r.ver.panicOn(hashOf(body))
	r.up.set(body)
	r.checker.logger = slog.New(panicLogHandler{panicOn: func(rec slog.Record) bool { return rec.Message == "adoption_panicked" }})
	before := unlogged()
	r.check() // returns: WaitForAdoptions saw the goroutine end
	if got := unlogged() - before; got < 1 {
		t.Fatalf("unlogged panics rose by %v, want at least 1", got)
	}
	if strOf(r.row().AdoptionFailReason) != cache.ReasonPanic {
		t.Fatalf("record reason %q, want panic", strOf(r.row().AdoptionFailReason))
	}
	// Past the record's retry: before it, a check of the deferred
	// never-adopted suite asks upstream nothing (ruling PE-3).
	r.clock.advance(backoffDelay(1) + time.Minute)
	r.check() // the suite mutex was released: a second check runs
	if n := len(r.up.etagsSent(rigAnchorURL)); n != 2 {
		t.Fatalf("%d checks reached upstream, want 2", n)
	}
}

// Deferred minor B-20: a shutdown that cuts a failure record's write short
// is not a WARN.
func TestAdoptionFailureRecordCutByShutdownIsNotAWarning(t *testing.T) {
	r := newRetryRig(t, 0)
	bad := r.release("bad", 0)
	r.ver.reject(hashOf(bad))
	r.up.set(bad)
	r.checker.recordHook = r.cancelLifetime // shutdown begins right before the write
	r.check()
	for _, rec := range r.events("adoption_failure_record_failed") {
		if rec["level"] == "WARN" {
			t.Fatalf("a record cut short by shutdown logged WARN: %v", rec)
		}
	}
	if r.row().AdoptionFailCount != 0 {
		t.Fatal("a record was written after shutdown began")
	}
}

// Deferred minor B-28: a tick's heal parses the snapshot's pinned Release
// once — the decision's parse is reused.
func TestCheck_ReconcileOnTheTickParsesTheReleaseOnce(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopter.repairSkippedMembers = true
	ctx := context.Background()
	all := fakePackagesStanzas(map[string]string{"pool/main/c/c/c_1_all.deb": testHex64})
	const allPath = "main/binary-all/Packages" // a required group (arch all) the snapshot lacks
	r.members.put("http://h/r/dists/x/"+allPath, all)
	in := signedRelease("degraded", 0, map[string][]byte{"main/binary-amd64/Packages": r.pkgs, allPath: all})
	ah, err := r.adopter.writeBlobBytes(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	ph, err := r.adopter.writeBlobBytes(ctx, r.pkgs)
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := r.cache.InsertTrackedCandidateForTest(ctx, cache.SnapshotCandidate{CanonicalScheme: "http",
		CanonicalHost: rigHost, SuitePath: rigSuite, InReleaseHash: &ah})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.cache.CommitAdoption(ctx, id, []cache.SnapshotMember{
		{SnapshotID: id, Path: "InRelease", BlobHash: ah, DeclaredSHA256: ah},
		{SnapshotID: id, Path: "main/binary-amd64/Packages", BlobHash: ph, DeclaredSHA256: ph},
	}, nil, nil, nil, true); err != nil {
		t.Fatal(err)
	}
	var parses atomic.Int32
	r.adopter.reparseHook = func() { parses.Add(1) }
	r.up.set(in) // unchanged: the tick decides and heals
	r.check()
	if _, err := r.cache.GetSnapshotMember(ctx, id, allPath); err != nil {
		t.Fatalf("the missing member was not healed: %v", err)
	}
	if n := parses.Load(); n != 1 {
		t.Fatalf("the Release was parsed %d times for one heal, want 1", n)
	}
}

// Deferred minor C-17 (phase C design note 14): without a stop context
// (Run/RunDetached), a cancelled ctx before the commit is a shutdown — the
// adoption ends and commits nothing.
func TestRunRequest_CancelledWithoutAStopContextCommitsNothing(t *testing.T) {
	r := newRetryRig(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := r.release("v1", 0)
	req := &adoptionRequest{suite: SuiteRef{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite},
		form: adoptionFormInline, bytes: body, key: hashOf(body)}
	err := r.adopter.runRequest(ctx, req, runHooks{membersDone: cancel})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the shutdown (context.Canceled)", err)
	}
	if r.currentSnapshot() != nil {
		t.Fatal("an adoption cancelled before its commit committed")
	}
}
```

Create `internal/handler/anchor_readback_test.go`:

```go
package handler

import (
	"log/slog"
	"net/http"
	"testing"
)

// Deferred minor D-38: a fetched signed anchor whose pool file cannot be
// read back for its sniff is a local disk fault, labelled like the others
// (outcome cache_write_failed, ERROR "cache write failed"), not an
// unclassified fetch error.
func TestServeHTTP_UnreadableFetchedAnchorIsACacheFault(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	r.serve("/r/dists/s/InRelease", upFile{body: rowInRelease})
	r.unreadableBlob(rowInRelease)
	if rec := r.get("/r/dists/s/InRelease"); rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", rec.Code)
	}
	lines := logRecords(r.logs, "request", slog.LevelInfo)
	if len(lines) != 1 || lines[0]["outcome"] != "cache_write_failed" {
		t.Fatalf("request lines %v, want one with outcome cache_write_failed", lines)
	}
	if n := len(logRecords(r.logs, "unclassified fetch error", slog.LevelError)); n != 0 {
		t.Fatalf("%d unclassified fetch error lines, want 0", n)
	}
	if n := len(logRecords(r.logs, "cache write failed", slog.LevelError)); n != 1 {
		t.Fatalf("%d cache write failed lines, want 1", n)
	}
}
```

And change the existing tests (B-16, B-19, B-21, B-24, D-21, D-22):

```diff
diff --git a/internal/cache/adoption_scope_test.go b/internal/cache/adoption_scope_test.go
index 30f02e0..c5e129a 100644
--- a/internal/cache/adoption_scope_test.go
+++ b/internal/cache/adoption_scope_test.go
@@ -49,9 +49,18 @@ func TestAdoptionStamp_IsScopedToTheSuitesSchemeAndHost(t *testing.T) {
 		}
 	}
+	// Deferred minor D-21: the https twin of the same host and suite,
+	// adopted later, does not count either.
+	twin := commitFor(t, c, "https", "a", suite, "the https twin's anchor", "", "")
+	if _, err := c.db.Exec(`UPDATE suite_snapshot SET adopted_at = 1800009999 WHERE snapshot_id = ?`, twin); err != nil {
+		t.Fatal(err)
+	}
 	id := commitFor(t, c, "http", "a", suite, "a's anchor", "", "")
 	snap, err := c.GetSuiteSnapshot(ctx, id)
-	if err != nil || snap.AdoptedAt == nil || *snap.AdoptedAt != 1_800_000_000 {
-		t.Fatalf("first adoption on host a stamped %d (%v), want now (1800000000): other hosts and schemes do not count",
-			derefI64(snap.AdoptedAt), err)
+	if err != nil { // deferred minor D-22: report the error, never dereference a nil snapshot
+		t.Fatal(err)
+	}
+	if snap.AdoptedAt == nil || *snap.AdoptedAt != 1_800_000_000 {
+		t.Fatalf("first adoption on host a stamped %d, want now (1800000000): other hosts and schemes do not count",
+			derefI64(snap.AdoptedAt))
 	}
 }
@@ -92,4 +101,5 @@ func TestDisplacedSnapshotMembers_OrdersByAdoptionAndScopesToTheSuite(t *testing
 	current := commitFor(t, c, "http", "a", suite, "current anchor", "", "")
 	commitFor(t, c, "http", "b", suite, "b's anchor", alias, "b's member")
+	commitFor(t, c, "https", "a", suite, "the https twin's anchor", alias, "the twin's member") // D-21
 	rows, err := c.DisplacedSnapshotMembers(ctx, "http", "a", suite, alias, current, 5)
 	if err != nil {
@@ -97,5 +107,5 @@ func TestDisplacedSnapshotMembers_OrdersByAdoptionAndScopesToTheSuite(t *testing
 	}
 	if len(rows) != 2 || rows[0].SnapshotID != older || rows[1].SnapshotID != newer {
-		t.Fatalf("displaced members = %+v, want snapshot %d (adopted last) then %d, and none of host b", rows, older, newer)
+		t.Fatalf("displaced members = %+v, want snapshot %d (adopted last) then %d, and none of host b or of https", rows, older, newer)
 	}
 	// The limit keeps the newest by adoption too.
diff --git a/internal/freshness/adoption_failure_test.go b/internal/freshness/adoption_failure_test.go
index ee1aa5c..c174565 100644
--- a/internal/freshness/adoption_failure_test.go
+++ b/internal/freshness/adoption_failure_test.go
@@ -130,7 +130,12 @@ func TestAdoptionFailure_PanicIsRecorded(t *testing.T) {
 		t.Fatalf("record = %d %q, want 1 %q", row.AdoptionFailCount, strOf(row.AdoptionFailReason), cache.ReasonPanic)
 	}
-	evs := r.ring.Snapshot()
-	if len(evs) == 0 || evs[len(evs)-1].Outcome != "panicked" || evs[len(evs)-1].Reason != cache.ReasonPanic {
-		t.Fatalf("ring = %+v, want a panicked event", evs)
+	panicked := 0
+	for _, ev := range r.ring.Snapshot() { // deferred minor B-21: by outcome, not position
+		if ev.Outcome == "panicked" && ev.Reason == cache.ReasonPanic {
+			panicked++
+		}
+	}
+	if panicked != 1 {
+		t.Fatalf("ring = %+v, want one panicked event", r.ring.Snapshot())
 	}
 	v, _ := r.checker.locks.Load(suiteKey("http", rigHost, rigSuite))
@@ -142,4 +147,56 @@ func TestAdoptionFailure_PanicIsRecorded(t *testing.T) {
 }
 
+// Deferred minor B-19: a reason change mid-streak keeps the streak and
+// logs adoption_deferred again (a state transition), with the retry
+// attributes; the failing content's Last-Modified is recorded; a detached
+// failure is keyed sha256(sha256(Release) || sha256(Release.gpg)).
+func TestAdoptionFailure_ReasonChangeValidatorsAndDetachedKey(t *testing.T) {
+	r := newRetryRig(t, 0)
+	// An adopted suite: a never-adopted one would not attempt the new
+	// content before its record's retry (ruling PE-3).
+	r.up.set(r.release("v1", 0))
+	r.check()
+	if r.row().CurrentSnapshotID == nil {
+		t.Fatal("setup: v1 not adopted")
+	}
+	bad := r.release("bad", 0)
+	r.ver.reject(hashOf(bad))
+	r.up.set(bad)
+	r.check() // gpg_failed
+	future := r.release("from the future", r.clock.now().Add(time.Hour).Unix())
+	r.up.set(future)
+	r.clock.advance(time.Minute)
+	r.check() // new content, attempted at once: release_date_in_future
+	row := r.row()
+	if row.AdoptionFailCount != 2 || strOf(row.AdoptionFailReason) == "gpg_failed" {
+		t.Fatalf("record %d %q, want the streak at 2 with the new reason", row.AdoptionFailCount, strOf(row.AdoptionFailReason))
+	}
+	deferred := r.events("adoption_deferred")
+	if len(deferred) != 2 || deferred[1]["reason"] != strOf(row.AdoptionFailReason) || deferred[1]["fail_count"] != float64(2) {
+		t.Fatalf("adoption_deferred lines %v, want two, the second for the new reason at count 2", deferred)
+	}
+	if deferred[1]["retry_in_seconds"] != float64(4*60) || deferred[1]["next_retry_unixtime"] != float64(i64Of(row.AdoptionNextRetryAt)) {
+		t.Fatalf("retry attributes %v, want retry_in_seconds 240 and next_retry_unixtime %d", deferred[1], i64Of(row.AdoptionNextRetryAt))
+	}
+	// The failing content's Last-Modified is kept with its record.
+	r.checker.recordAdoptionFailure(&adoptionRequest{suite: SuiteRef{CanonicalScheme: "http", CanonicalHost: rigHost,
+		SuitePath: rigSuite}, key: testHex64, lastmod: "Mon, 01 Jan 2024 00:00:00 GMT"}, "gpg_failed")
+	if got := strOf(r.row().AdoptionFailLastMod); got != "Mon, 01 Jan 2024 00:00:00 GMT" {
+		t.Fatalf("AdoptionFailLastMod = %q", got)
+	}
+
+	d := newRetryRig(t, 0)
+	seedDetachedAnchors(t, d.cache)
+	rel, sig := d.releaseText("det", 0), armoredSig("det sig")
+	key := contentKey(adoptionFormDetached, nil, rel, sig)
+	d.ver.reject(key)
+	d.up.setAt(rigReleaseURL, rel)
+	d.up.setAt(rigReleaseGPGURL, sig)
+	d.check()
+	if got := strOf(d.row().AdoptionFailHash); got != key {
+		t.Fatalf("detached record keyed %s, want %s", got, key)
+	}
+}
+
 // Refusals use the record and schedule but are not failures: logged as
 // adoption_refused and never as adoption_failing_persistently.
diff --git a/internal/freshness/retry_gate_test.go b/internal/freshness/retry_gate_test.go
index b9b821a..fc05101 100644
--- a/internal/freshness/retry_gate_test.go
+++ b/internal/freshness/retry_gate_test.go
@@ -101,6 +101,13 @@ func TestGate_NewContentIsAttemptedAtOnce(t *testing.T) {
 
 // At next_retry_at the check sends no validators and retries.
+//
+// Deferred minor B-24: on an adopted suite, whose anchor has validators, so
+// "none" is told apart from the current anchor's.
 func TestGate_DueRetryHasNoValidators(t *testing.T) {
 	r := newRetryRig(t, 0)
+	r.adopt()
+	if strOf(r.row().InReleaseETag) == "" {
+		t.Fatal("setup: the adopted suite has no validators")
+	}
 	failOnce(t, r)
 	r.clock.advance(2 * time.Minute)
diff --git a/internal/freshness/retry_test.go b/internal/freshness/retry_test.go
index 47c04c9..26ed828 100644
--- a/internal/freshness/retry_test.go
+++ b/internal/freshness/retry_test.go
@@ -2,4 +2,6 @@ package freshness
 
 import (
+	"crypto/sha256"
+	"encoding/hex"
 	"testing"
 	"time"
@@ -23,4 +25,9 @@ func TestContentKey(t *testing.T) {
 		t.Fatalf("detached key %q must be a distinct sha256 hex", k1)
 	}
+	// The exact formula: keys persist across restarts (deferred minor B-16).
+	r, g := sha256.Sum256(rel), sha256.Sum256(sig1)
+	if want := sha256.Sum256(append(r[:], g[:]...)); k1 != hex.EncodeToString(want[:]) {
+		t.Fatalf("detached key = %s, want sha256(sha256(Release) || sha256(Release.gpg))", k1)
+	}
 }
 
```

In `internal/aptpath/sniff_test.go`, `TestDecodableClearsign`'s cases gain, after the `m + " \t\nHash: SHA256\n"` row (D-5):

```go
		{m + " \nHash: SHA256\n", m + "\nHash: SHA256\n"}, // one trailing blank: as long as "\r" (deferred minor D-5)
```

`TestSniffRelease` gains, after `atEdge` (D-2):

```go
	pastEdge := strings.Repeat("y", ReleaseSniffBytes-len("SHA256:")) + "\nSHA256:\n" // deferred minor D-2
```

and, after the `"colon at byte 65536"` row:

```go
		{"colon at byte 65537", pastEdge, false},
```

`TestClassifySuiteFile`'s row `{"", "/x/InRelease", NotSuiteFile},` becomes (D-1):

```go
		{"", "/InRelease", NotSuiteFile},   // deferred minor D-1: only the guard refuses it
		{"", "/Packages.gz", NotSuiteFile}, // nor a host-root flat index
```

- [ ] **Step 3: Run to verify failure**

Run: `go test -gcflags=-e -run '^$' ./internal/freshness/ ./internal/handler/ ./internal/cache/`
Expected: FAIL — `internal/freshness` does not build: `ad.heartbeatHook undefined`, `r.checker.recordHook undefined`, `r.adopter.reparseHook undefined` (`go vet` stops at a package's first error; `-gcflags=-e` lists them all). With those three fields declared (and not yet called), `TestHeartbeatPanicIsLoggedWithItsStackOnce`, `TestRunLegacyTrustAudit_UnsignedKeepIsNotSigned`, `TestRunLegacyTrustAudit_ErrorsCarryThePrefix`, `TestJitterDelay_ClampsTheRandomValue`, `TestAdoptionFailureRecordCutByShutdownIsNotAWarning`, `TestCheck_ReconcileOnTheTickParsesTheReleaseOnce` (`parsed 0 times for one heal, want 1`: the hook has no call site yet), `TestServeHTTP_UnreadableFetchedAnchorIsACacheFault` (outcome `bad_gateway`) fail; `TestAdoptionPanicWithABrokenLoggerIsContained` crashes the test binary (`panic: log handler bug: adoption_panicked`); `TestContentKey`, `TestGate_DueRetryHasNoValidators`, `TestAdoptionFailure_*`, `TestRunRequest_CancelledWithoutAStopContextCommitsNothing`, `TestAdoptionStamp_IsScopedToTheSuitesSchemeAndHost`, `TestDisplacedSnapshotMembers_OrdersByAdoptionAndScopesToTheSuite`, `TestDecodableClearsign`, `TestSniffRelease` and `TestClassifySuiteFile` pass (they pin behaviour the code already has; Step 6 shows their teeth).

- [ ] **Step 4: Implement the rest**

```diff
diff --git a/internal/freshness/adoption.go b/internal/freshness/adoption.go
index ee045de..b2033be 100644
--- a/internal/freshness/adoption.go
+++ b/internal/freshness/adoption.go
@@ -544,4 +544,11 @@ type Adopter struct {
 	beforeCommit func()
 
+	// heartbeatHook, when set (tests only), runs at each heartbeat tick.
+	heartbeatHook func()
+
+	// reparseHook, when set (tests only), runs at each parse of a
+	// snapshot's pinned Release (reparseSnapshotRelease).
+	reparseHook func()
+
 	// reconcilePendingHook, when set, runs at the start of
 	// reconcilePending. Test seam: a panic in the repair decision must not
@@ -874,4 +881,7 @@ func (a *Adopter) runHeartbeatTicker(ctx context.Context, host string, snapshotI
 			return
 		case <-t.C:
+			if a.heartbeatHook != nil {
+				a.heartbeatHook()
+			}
 			a.heartbeat(ctx, host, snapshotID, tracker)
 		}
@@ -1259,9 +1269,5 @@ func (a *Adopter) runShared(ctx context.Context, suite SuiteRef, p *adoptionPayl
 			defer func() {
 				if r := recover(); r != nil {
-					a.logger.Error("adoption_panicked",
-						"where", "heartbeat",
-						"canonical_host", suite.CanonicalHost,
-						"suite_path", suite.SuitePath,
-						"panic", fmt.Sprint(r))
+					unloggedPanic(func() { logPanic(a.logger, "adoption_panicked", "heartbeat", suite, r) })
 				}
 			}()
diff --git a/internal/freshness/adoption_failure.go b/internal/freshness/adoption_failure.go
index 6a37587..da85796 100644
--- a/internal/freshness/adoption_failure.go
+++ b/internal/freshness/adoption_failure.go
@@ -33,5 +33,5 @@ func (c *Checker) recordAdoptionFailure(req *adoptionRequest, reason string) {
 	defer func() {
 		if r := recover(); r != nil {
-			c.reportPanic("adoption_panicked", "record", s, r)
+			unloggedPanic(func() { c.reportPanic("adoption_panicked", "record", s, r) })
 		}
 	}()
@@ -75,6 +75,15 @@ func (c *Checker) recordAdoptionFailure(req *adoptionRequest, reason string) {
 		f.ETag, f.LastMod = nil, nil
 	}
+	if c.recordHook != nil {
+		c.recordHook()
+	}
 	if err := c.cache.RecordAdoptionFailure(ctx, s.CanonicalScheme, s.CanonicalHost, s.SuitePath, f); err != nil {
-		c.logger.Warn("adoption_failure_record_failed",
+		// A shutdown that raced the failure ended the write: nothing is
+		// recorded at shutdown (spec C7), so that is no fault (B-20).
+		level := slog.LevelWarn
+		if c.lifetimeCtx.Err() != nil {
+			level = slog.LevelDebug
+		}
+		c.logger.Log(ctx, level, "adoption_failure_record_failed",
 			"canonical_host", s.CanonicalHost, "suite_path", s.SuitePath, "err", err)
 		return
@@ -164,15 +173,19 @@ func (c *Checker) recoverAdoption(req *adoptionRequest, start time.Time) {
 		return
 	}
-	c.reportPanic("adoption_panicked", "adoption", req.suite, r)
-	d := time.Since(start)
-	host := req.suite.CanonicalHost
-	adoptionTotal.Inc("panicked", host)
-	adoptionDurationSeconds.Observe(d.Seconds(), "panicked", host)
-	if c.adoptionRing != nil {
-		c.adoptionRing.Record(observability.AdoptionEvent{
-			Host: host, SuitePath: req.suite.SuitePath, Outcome: "panicked", Reason: cache.ReasonPanic,
-			CompletedUnixSec: c.now().Unix(), DurationSeconds: d.Seconds(),
-		})
-	}
+	// The reporting runs under its own boundary (B-18): a panic there must
+	// neither escape nor skip the record below.
+	unloggedPanic(func() {
+		c.reportPanic("adoption_panicked", "adoption", req.suite, r)
+		d := time.Since(start)
+		host := req.suite.CanonicalHost
+		adoptionTotal.Inc("panicked", host)
+		adoptionDurationSeconds.Observe(d.Seconds(), "panicked", host)
+		if c.adoptionRing != nil {
+			c.adoptionRing.Record(observability.AdoptionEvent{
+				Host: host, SuitePath: req.suite.SuitePath, Outcome: "panicked", Reason: cache.ReasonPanic,
+				CompletedUnixSec: c.now().Unix(), DurationSeconds: d.Seconds(),
+			})
+		}
+	})
 	c.recordAdoptionFailure(req, cache.ReasonPanic)
 }
diff --git a/internal/freshness/freshness.go b/internal/freshness/freshness.go
index 3f9f4c0..6517004 100644
--- a/internal/freshness/freshness.go
+++ b/internal/freshness/freshness.go
@@ -244,4 +244,8 @@ type Checker struct {
 	unlockDroppedHook func(key string)
 
+	// recordHook, when set (tests only), runs in recordAdoptionFailure
+	// right before the record is written.
+	recordHook func()
+
 	lastClockStepLog atomic.Int64 // monotonic ns since processStart of the last clock_step_detected line
 }
@@ -377,4 +381,5 @@ type checkOutcome struct {
 	needRepair    bool
 	needReconcile bool
+	declared      []ReleaseMember // what reconcilePending parsed, for the heal (B-28)
 	lease         *lease
 	// report is what the scheduler learns from this check.
@@ -669,5 +674,5 @@ func (c *Checker) planRepair(ctx context.Context, out *checkOutcome) {
 	}
 	out.needRepair = c.adopter.repairPending(ctx, rp.suite, rp.snapshotID)
-	needReconcile, err := c.adopter.reconcilePending(ctx, rp.snapshotID)
+	declared, needReconcile, err := c.adopter.reconcilePending(ctx, rp.snapshotID)
 	if err != nil {
 		// A fail-closed anchor (R-1 decode error) must be visible, not a
@@ -677,5 +682,5 @@ func (c *Checker) planRepair(ctx context.Context, out *checkOutcome) {
 			"snapshot_id", rp.snapshotID, "stage", "decision", "err", err)
 	}
-	out.needReconcile = needReconcile
+	out.needReconcile, out.declared = needReconcile, declared
 	if !out.needRepair && !out.needReconcile {
 		out.repair = nil
@@ -724,5 +729,5 @@ func (c *Checker) planRepair(ctx context.Context, out *checkOutcome) {
 // phase E: v7 has no record keyed on a snapshot).
 func (c *Checker) spawnRepair(mu *sync.Mutex, out *checkOutcome) {
-	rp, l, needRepair, needReconcile := out.repair, out.lease, out.needRepair, out.needReconcile
+	rp, l, needRepair, needReconcile, declared := out.repair, out.lease, out.needRepair, out.needReconcile, out.declared
 	c.adoptionWg.Add(1)
 	go func() {
@@ -740,5 +745,5 @@ func (c *Checker) spawnRepair(mu *sync.Mutex, out *checkOutcome) {
 			// on-demand path (Reconcile) so a fail-closed anchor (R-1
 			// decode error) is visible, not a silent non-heal.
-			if _, err := c.adopter.ReconcileSnapshot(rctx, rp.suite, rp.snapshotID, false); err != nil {
+			if _, err := c.adopter.reconcileSnapshot(rctx, rp.suite, rp.snapshotID, declared, false); err != nil {
 				c.logger.Warn("adoption_reconcile_failed",
 					"canonical_host", rp.suite.CanonicalHost, "suite_path", rp.suite.SuitePath,
@@ -792,8 +797,17 @@ var ErrAdoptionPanicked = errors.New("adoption_panicked")
 var panicStackLogged sync.Map
 
-// reportPanic logs a recovered panic. The stack is attached to the first
-// occurrence per goroutine kind per process only (spec C5 "log the stack
-// once"): a deterministic panic on every attempt must not flood the log.
+// reportPanic logs a recovered panic (logPanic, with the checker's logger).
 func (c *Checker) reportPanic(event, where string, suite SuiteRef, r any) {
+	logPanic(c.logger, event, where, suite, r)
+}
+
+// logPanic logs a recovered panic as event at ERROR, with where, the suite
+// and the panic value. The stack is attached to the first occurrence per
+// goroutine kind (where) per process only (spec C5 "log the stack once"): a
+// deterministic panic on every attempt must not flood the log. One helper
+// for every recover in the package — the checker's goroutines and the
+// adopter's heartbeat (deferred minor A-37; the handler's counterpart is
+// logGoroutinePanic).
+func logPanic(logger *slog.Logger, event, where string, suite SuiteRef, r any) {
 	attrs := []any{
 		"where", where,
@@ -805,5 +819,17 @@ func (c *Checker) reportPanic(event, where string, suite SuiteRef, r any) {
 		attrs = append(attrs, "stack", string(debug.Stack()))
 	}
-	c.logger.Error(event, attrs...)
+	logger.Error(event, attrs...)
+}
+
+// unloggedPanic runs report under its own recover: a report that panics
+// itself (a broken log handler) is counted, never escapes (deferred minor
+// B-18).
+func unloggedPanic(report func()) {
+	defer func() {
+		if recover() != nil {
+			freshnessUnloggedPanics.Inc()
+		}
+	}()
+	report()
 }
 
diff --git a/internal/freshness/legacy_audit.go b/internal/freshness/legacy_audit.go
index aa43653..1d1b6ad 100644
--- a/internal/freshness/legacy_audit.go
+++ b/internal/freshness/legacy_audit.go
@@ -50,5 +50,7 @@ type legacyVerdict struct {
 // audit resumes on the next start. A single unreadable anchor, or a panic
 // while auditing one snapshot, is not an error: that snapshot is deferred
-// (left queued, unchanged, ERROR logged).
+// (left queued, unchanged, ERROR logged). Every error it returns starts
+// "legacy trust audit:" — main's refusal line relies on it (deferred minors
+// A-48, C-29) — so a new error path must keep the prefix.
 //
 // AIDEV-NOTE: plan review 2026-10-05 findings 1–3 and its follow-ups.
@@ -111,5 +113,9 @@ func RunLegacyTrustAudit(ctx context.Context, c *cache.Cache, logger *slog.Logge
 				switch d.Outcome {
 				case cache.LegacyAuditKeep:
-					res.Signed++
+					// An unsigned snapshot is kept but proves nothing
+					// signed (deferred minor A-47).
+					if decided[i].Form != cache.FormUnsigned {
+						res.Signed++
+					}
 				case cache.LegacyAuditQuarantine, cache.LegacyAuditQuarantineSigned:
 					res.Quarantined++
@@ -131,5 +137,5 @@ func RunLegacyTrustAudit(ctx context.Context, c *cache.Cache, logger *slog.Logge
 			"audited", res.Audited, "quarantined", res.Quarantined, "deferred", res.Deferred)
 		if err := legacyAuditAfterBatch(batch); err != nil {
-			return res, err
+			return res, fmt.Errorf("legacy trust audit: %w", err)
 		}
 	}
diff --git a/internal/freshness/reconcile.go b/internal/freshness/reconcile.go
index 26b62b6..67a670a 100644
--- a/internal/freshness/reconcile.go
+++ b/internal/freshness/reconcile.go
@@ -35,4 +35,12 @@ import (
 //     force=true (on-demand path) bypasses the memo; force=false (tick path) does not.
 func (a *Adopter) ReconcileSnapshot(ctx context.Context, suite SuiteRef, snapshotID int64, force bool) (int, error) {
+	return a.reconcileSnapshot(ctx, suite, snapshotID, nil, force)
+}
+
+// reconcileSnapshot is ReconcileSnapshot; a non-nil declared is the
+// snapshot's declared set its caller already parsed from the same pinned
+// anchor (reconcilePending on the tick), so the Release is parsed once per
+// heal (deferred minor B-28).
+func (a *Adopter) reconcileSnapshot(ctx context.Context, suite SuiteRef, snapshotID int64, declared []ReleaseMember, force bool) (int, error) {
 	if !force {
 		if _, done := a.reconciledSnapshots.Load(snapshotID); done {
@@ -46,7 +54,9 @@ func (a *Adopter) ReconcileSnapshot(ctx context.Context, suite SuiteRef, snapsho
 	// cannot be tampered with without changing the blob hash (which would
 	// invalidate the snapshot_member.declared_sha256 = blob.hash invariant).
-	declared, err := a.reparseSnapshotRelease(ctx, snapshotID)
-	if err != nil {
-		return 0, err // logged by caller; do not memoize
+	if declared == nil {
+		var err error
+		if declared, err = a.reparseSnapshotRelease(ctx, snapshotID); err != nil {
+			return 0, err // logged by caller; do not memoize
+		}
 	}
 	present, err := a.cache.ListSnapshotMembers(ctx, snapshotID)
@@ -117,28 +127,29 @@ func (a *Adopter) ReconcileSnapshot(ctx context.Context, suite SuiteRef, snapsho
 
 // reconcilePending reports whether ReconcileSnapshot(force=false) would
-// fetch anything. A memoised snapshot costs nothing; otherwise the
+// fetch anything and, when it would, the declared set it parsed (the heal
+// reuses it: B-28). A memoised snapshot costs nothing; otherwise the
 // trust-anchored Release is re-parsed once, and a complete snapshot is
 // memoised so later checks skip it. The Checker asks before it takes an
 // admission slot, inside the check's recover.
-func (a *Adopter) reconcilePending(ctx context.Context, snapshotID int64) (bool, error) {
+func (a *Adopter) reconcilePending(ctx context.Context, snapshotID int64) ([]ReleaseMember, bool, error) {
 	if a.reconcilePendingHook != nil {
 		a.reconcilePendingHook()
 	}
 	if _, done := a.reconciledSnapshots.Load(snapshotID); done {
-		return false, nil
+		return nil, false, nil
 	}
 	declared, err := a.reparseSnapshotRelease(ctx, snapshotID)
 	if err != nil {
-		return false, err
+		return nil, false, err
 	}
 	present, err := a.cache.ListSnapshotMembers(ctx, snapshotID)
 	if err != nil {
-		return false, fmt.Errorf("reconcile: list members: %w", err)
+		return nil, false, fmt.Errorf("reconcile: list members: %w", err)
 	}
 	if len(missingRequestableMembers(declared, present, a.architectureAllowlist)) == 0 {
 		a.reconciledSnapshots.Store(snapshotID, struct{}{})
-		return false, nil
+		return nil, false, nil
 	}
-	return true, nil
+	return declared, true, nil
 }
 
@@ -158,4 +169,7 @@ func (a *Adopter) reconcilePending(ctx context.Context, snapshotID int64) (bool,
 // declared set — do not accept member declarations from any other source.
 func (a *Adopter) reparseSnapshotRelease(ctx context.Context, snapshotID int64) ([]ReleaseMember, error) {
+	if a.reparseHook != nil {
+		a.reparseHook()
+	}
 	for _, name := range []string{"InRelease", "Release"} {
 		mem, err := a.cache.GetSnapshotMember(ctx, snapshotID, name)
diff --git a/internal/freshness/retry.go b/internal/freshness/retry.go
index 8ad987e..df1d0d5 100644
--- a/internal/freshness/retry.go
+++ b/internal/freshness/retry.go
@@ -64,6 +64,12 @@ func backoffDelay(count int64) time.Duration {
 }
 
-// jitterDelay spreads d by ±25 %: u is uniform in [0, 1); u = 0.5 is d.
+// jitterDelay spreads d by ±25 %: u is uniform in [0, 1); u = 0.5 is d. A
+// u outside [0, 1] (or NaN) is clamped, so the delay stays within ±25 %
+// whatever the random seam returns (deferred minor B-17).
 func jitterDelay(d time.Duration, u float64) time.Duration {
+	if !(u >= 0) { // also NaN
+		u = 0
+	}
+	u = min(u, 1)
 	return d + time.Duration((u*2-1)*retryJitterFrac*float64(d))
 }
diff --git a/internal/handler/handler.go b/internal/handler/handler.go
index acfb5e7..e7a9886 100644
--- a/internal/handler/handler.go
+++ b/internal/handler/handler.go
@@ -2091,5 +2091,7 @@ func (h *Handler) runFetch(ctx context.Context, req *proxy.Request, file aptpath
 				"err", herr, "canonical_host", req.CanonicalHost, "suite_path", req.SuitePath, "path", req.Path)
 			if file.Marks() {
-				return sfResult{err: fmt.Errorf("handler: read suite file head: %w", herr), status: fres.Status}
+				// A local disk fault, labelled like every other one
+				// (cache_write_failed; deferred minor D-38).
+				return sfResult{err: fmt.Errorf("handler: read suite file head: %w: %w", fetch.ErrCacheWriteFailed, herr), status: fres.Status}
 			}
 		}
```

- [ ] **Step 5: Run the tests**

Run: `go test -race ./...` and `golangci-lint run ./...`
Expected: PASS; 0 issues.

- [ ] **Step 6: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| the heartbeat logs on its own (no `logPanic`) | `TestHeartbeatPanicIsLoggedWithItsStackOnce` |
| the audit counts every keep as signed | `TestRunLegacyTrustAudit_UnsignedKeepIsNotSigned` |
| the batch seam's error without the prefix | `TestRunLegacyTrustAudit_ErrorsCarryThePrefix` |
| `jitterDelay`: `u < 0` (NaN passes) | `TestJitterDelay_ClampsTheRandomValue` |
| `jitterDelay`: no upper clamp | `TestJitterDelay_ClampsTheRandomValue` |
| `unloggedPanic` without its recover | `TestAdoptionPanicWithABrokenLoggerIsContained` (the test binary crashes) |
| the shutdown-cut record logged WARN | `TestAdoptionFailureRecordCutByShutdownIsNotAWarning` |
| `spawnRepair` passes no declared set | `TestCheck_ReconcileOnTheTickParsesTheReleaseOnce` |
| `stopErr` ignores a cancelled `ctx` | `TestRunRequest_CancelledWithoutAStopContextCommitsNothing` |
| `contentKey`: the two hashes swapped | `TestContentKey` |
| the gate's due retry sends the current anchor's validators | `TestGate_DueRetryHasNoValidators` |
| the stamp's prior read without its scheme predicate | `TestAdoptionStamp_IsScopedToTheSuitesSchemeAndHost` |
| `DisplacedSnapshotMembers` without its scheme predicate | `TestDisplacedSnapshotMembers_OrdersByAdoptionAndScopesToTheSuite` |
| the anchor read-back error not wrapped as `ErrCacheWriteFailed` | `TestServeHTTP_UnreadableFetchedAnchorIsACacheFault` |
| `DecodableClearsign`: `end == len(clearsignBegin)+1` without `&& b[end-1] == '\r'` | `TestDecodableClearsign` |
| `ClassifySuiteFile` without the `suite == ""` guard | `TestClassifySuiteFile` |
| `SniffRelease` reads one byte past the window (`head[:ReleaseSniffBytes+1]`) | `TestSniffRelease` |

- [ ] **Step 7: Commit**

```bash
gofmt -l internal/freshness internal/handler internal/cache internal/aptpath | grep -v -e hot_prefetch_test.go -e cache_test.go -e gc_urlpath_test.go
git add internal/freshness/flip_barrier.go internal/freshness/deferred_e_test.go internal/handler/anchor_readback_test.go \
  internal/freshness/adoption.go internal/freshness/adoption_failure.go internal/freshness/freshness.go \
  internal/freshness/legacy_audit.go internal/freshness/reconcile.go internal/freshness/retry.go internal/handler/handler.go \
  internal/freshness/adoption_failure_test.go internal/freshness/retry_gate_test.go internal/freshness/retry_test.go \
  internal/cache/adoption_scope_test.go internal/aptpath/sniff_test.go
git commit -m "Close the deferred minors in the files phase E changes"
```

### Task 14: Deferred minors in the simulations this phase changes

`sim_sched_test.go` and `sim_retry_test.go` change by hand in Task 5 (fixtures), so their untagged deferred minors are taken here. Ruling PE-8 keeps the 5 s `-race` budget firm, with about 0.5 s of margin (executability review D6). B-57's day-5 touch keeps the 30-day simulation's suite checked through the whole 10-day untrusted phase, and each check of a never-adopted suite now reads its anchor rows. That took the run to 4.6–5.6 s under `-race` (pre-E 4.0 s). So the simulation is coarsened here, every assertion kept at its per-day rate:
- The older-content phase runs 5 days (was 8): 3..150 verifications, still at most 30 a day.
- Each clock-step phase runs 2 days (was 4).
- The liveness floor is 10 checks per simulated day: 190 over the run's 19 full days, where it was 280 over "~28".
- The untrusted phase keeps its 10 days: the 7-day interest window must lapse inside it, so its floor catches a suite gone dormant.

The run spans about 20 virtual days and takes about 3.6 s under `-race`. Four extra mutants in Step 3 show the coarsened phases keep their teeth. C-38 (H) keeps only the 500-suite simulation.

| ID | Change |
|---|---|
| B-51 | `simMembers`' `delay` and `started` maps are written only under its lock, through `setDelay` and `recordStarts` (every caller is switched; the type's doc says so) |
| B-53 | A sibling of the 500-suite simulation with an adopter wired: 100 stalling suites, a healthy adopted suite S and a changing candidate C on one host — S's latency bound holds while C keeps adopting, goroutines bounded (`TestSimScheduler_StallingHostWithAdoptionContention`, 3.1 s under `-race`; the 500-suite simulation itself stays as it is, C-38) |
| B-54 | `TestSimLim1_AttemptsAcrossTheJitter`: limitation #1 at both ends of the ±25 % jitter (28..40 attempts a day at the shortest delays, 16..26 at the longest); `startRetrySimRand` parameterises the seam |
| B-55 | `TestSimRestartSpread` also counts adoption attempts (50), not only upstream requests |
| B-56 | `TestSimRetry_ClockStepsWithAnActiveRecord`: a failure record active across a backward step (retry within the clamped hour; content dated ahead of the stepped clock is `release_date_in_future`) and a forward step (the overdue retry at the next tick; new content adopted) |
| B-57 | The 30-day simulation's untrusted phase has a floor (150..300 attempts in 10 days); a client touches the suite at day 5, so it stays interesting (spec C4 Dispatch 3) |
| B-58 | The flapping-mirror samples move off the tick grid; `TestSimFlappingMirror_UndatedContentIsARefusedRollback` pins the rollback guard alone (no Date on either side) and that it is recorded as the refusal state `rollback_refused` |

**Files:**
- Modify (tests only): `internal/freshness/sim_sched_test.go`, `internal/freshness/sim_retry_test.go`, and, for B-51's setters, `internal/freshness/sim_burst_test.go`, `internal/freshness/deferred_e_test.go`, `internal/freshness/reoffer_test.go`, `internal/freshness/flip_barrier_test.go`

**Interfaces:**
- Consumes: Task 5's `retrySim.touch`, `signedRelease`; `seedSimSuite`, `startSched`, `trackSuite`, `fakePackagesStanzas`, `renderedMetric`.
- Produces (test helpers): `(*simMembers).setDelay(url string, d time.Duration)`, `(*simMembers).recordStarts()`, `startRetrySimRand(t, refresh time.Duration, rnd func() float64) *retrySim`, `renderedMetricLabels(name string, labels ...string) string`.

- [ ] **Step 1: Apply the simulation changes**

```diff
diff --git a/internal/freshness/deferred_e_test.go b/internal/freshness/deferred_e_test.go
index dab1294..2c2c921 100644
--- a/internal/freshness/deferred_e_test.go
+++ b/internal/freshness/deferred_e_test.go
@@ -34,5 +34,5 @@ func TestHeartbeatPanicIsLoggedWithItsStackOnce(t *testing.T) {
 		pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": testHex64})
 		members.put(rigPkgsURL, pkgs)
-		members.delay[rigPkgsURL] = 3 * time.Second // the heartbeat ticks while the member arrives
+		members.setDelay(rigPkgsURL, 3*time.Second) // the heartbeat ticks while the member arrives
 		logs := &lockedBuffer{}
 		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
diff --git a/internal/freshness/flip_barrier_test.go b/internal/freshness/flip_barrier_test.go
index 8120675..4617aa7 100644
--- a/internal/freshness/flip_barrier_test.go
+++ b/internal/freshness/flip_barrier_test.go
@@ -741,5 +741,5 @@ func TestSimCapacity_TheBarrierHoldsTheGlobalSlotWithinItsBound(t *testing.T) {
 			up.Set("http://"+host+rigSuite+"/InRelease", answer)
 		}
-		members.started = map[string]time.Time{}
+		members.recordStarts()
 		ring := observability.NewRing(10)
 		t0 := time.Now()
diff --git a/internal/freshness/reoffer_test.go b/internal/freshness/reoffer_test.go
index ba03681..5af3bf9 100644
--- a/internal/freshness/reoffer_test.go
+++ b/internal/freshness/reoffer_test.go
@@ -351,5 +351,5 @@ func TestRun_ClientRefusedBusySuiteIsParkedInOrder(t *testing.T) {
 			pu := "http://" + host + suite + "/main/binary-amd64/Packages"
 			members.put(pu, pkgs)
-			members.delay[pu] = 10 * time.Minute
+			members.setDelay(pu, 10*time.Minute)
 			up.Set("http://"+host+suite+"/InRelease",
 				simtest.Serve(signedRelease("v1-"+host, 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})))
diff --git a/internal/freshness/sim_burst_test.go b/internal/freshness/sim_burst_test.go
index 87ab8d8..089eb32 100644
--- a/internal/freshness/sim_burst_test.go
+++ b/internal/freshness/sim_burst_test.go
@@ -57,5 +57,5 @@ func TestSimCapacity_UpgradeBurst(t *testing.T) {
 			seedStaleAnchorAt(t, c, host, rigSuite) // client interest now, a client-created anchor
 			members.put(pkgsURL(host), pkgsFor(host))
-			members.delay[pkgsURL(host)] = time.Minute
+			members.setDelay(pkgsURL(host), time.Minute)
 			up.Set(anchorURL(host), simtest.Serve(signedRelease("first-"+host, 0,
 				map[string][]byte{"main/binary-amd64/Packages": pkgsFor(host)})))
@@ -76,5 +76,5 @@ func TestSimCapacity_UpgradeBurst(t *testing.T) {
 			}
 			members.put(pkgsURL(host), pkgsFor("2-"+host))
-			members.delay[pkgsURL(host)] = time.Minute
+			members.setDelay(pkgsURL(host), time.Minute)
 			v2[host] = signedRelease("v2-"+host, 0, map[string][]byte{"main/binary-amd64/Packages": pkgsFor("2-" + host)})
 			at := time.Duration(10+12*i)*time.Minute + 7*time.Second // a trickle, off the tick grid
@@ -90,5 +90,5 @@ func TestSimCapacity_UpgradeBurst(t *testing.T) {
 			signed = append(signed, host)
 		}
-		members.started = map[string]time.Time{}
+		members.recordStarts()
 
 		runtime.Gosched()
diff --git a/internal/freshness/sim_retry_test.go b/internal/freshness/sim_retry_test.go
index 2c56055..b20eb93 100644
--- a/internal/freshness/sim_retry_test.go
+++ b/internal/freshness/sim_retry_test.go
@@ -29,4 +29,11 @@ type retrySim struct {
 
 func startRetrySim(t *testing.T, refresh time.Duration) *retrySim {
+	t.Helper()
+	return startRetrySimRand(t, refresh, func() float64 { return 0.5 })
+}
+
+// startRetrySimRand is startRetrySim with the scheduler's random seam rnd
+// (the retry jitter: 0.5 is none).
+func startRetrySimRand(t *testing.T, refresh time.Duration, rnd func() float64) *retrySim {
 	t.Helper()
 	c := simtest.OpenCache(t)
@@ -43,5 +50,5 @@ func startRetrySim(t *testing.T, refresh time.Duration) *retrySim {
 	}
 	_, s.stop = startSched(t, Config{Cache: c, Fetcher: s.up, HostLimiter: hostsem.New(8), Refresh: refresh,
-		Cooldown: time.Minute, Adopter: ad, Logger: simtest.DiscardLogger(), rand: func() float64 { return 0.5 }})
+		Cooldown: time.Minute, Adopter: ad, Logger: simtest.DiscardLogger(), rand: rnd})
 	return s
 }
@@ -107,4 +114,32 @@ func TestSimLim1_DeterministicFailureAttemptsPerDay(t *testing.T) {
 }
 
+// Deferred minor B-54: limitation #1's attempt count at both ends of the
+// ±25 % retry jitter — more attempts with the shortest delays, fewer with
+// the longest, both far below main's 1,440 a day.
+func TestSimLim1_AttemptsAcrossTheJitter(t *testing.T) {
+	attempts := map[string]int{}
+	for _, leg := range []struct {
+		name     string
+		u        float64
+		min, max int
+	}{{"shortest", 0, 28, 40}, {"longest", 0.999, 16, 26}} {
+		synctest.Test(t, func(t *testing.T) {
+			s := startRetrySimRand(t, 15*time.Minute, func() float64 { return leg.u })
+			bad := s.release("untrusted", 0)
+			s.ver.reject(hashOf(bad))
+			s.serve(bad)
+			time.Sleep(24*time.Hour + 7*time.Minute)
+			s.stop()
+			attempts[leg.name] = s.ver.calls()
+			if a := s.ver.calls(); a < leg.min || a > leg.max {
+				t.Fatalf("%s jitter: %d attempts in a day, want %d..%d", leg.name, a, leg.min, leg.max)
+			}
+		})
+	}
+	if attempts["shortest"] <= attempts["longest"] {
+		t.Fatalf("attempts %v: the shortest delays must give more attempts", attempts)
+	}
+}
+
 // Spec C7/C5 30-day simulation: an untrusted body (bounded attempts), new
 // content (adopted within a refresh interval), an upstream rollback to
@@ -112,4 +147,12 @@ func TestSimLim1_DeterministicFailureAttemptsPerDay(t *testing.T) {
 // when upstream recovers), and backward and forward clock steps (new
 // content still adopted, checks continue).
+//
+// AIDEV-NOTE: the run spans about 20 virtual days (27 before phase E). The
+// untrusted phase keeps its 10 days: the 7-day interest window lapses
+// inside it, so its floor catches a suite gone dormant without the day-5
+// touch (B-57). The older-content phase runs 5 days and each clock step 2,
+// every bound at the same per-day rate, so the run stays well under the
+// 5 s -race budget (executability review D6, ruling PE-8: about 3.6–4 s).
+// Re-time it under -race before lengthening any phase.
 func TestSim30Days_RetryDateRulesAndClockSteps(t *testing.T) {
 	synctest.Test(t, func(t *testing.T) {
@@ -129,7 +172,9 @@ func TestSim30Days_RetryDateRulesAndClockSteps(t *testing.T) {
 		s.ver.reject(hashOf(bad))
 		s.serve(bad)
-		time.Sleep(10*day + 7*time.Minute)
-		if a := s.ver.calls(); a > 10*30 {
-			t.Fatalf("untrusted body: %d attempts in 10 days, want <= 300", a)
+		time.Sleep(5 * day)
+		s.touch(t) // a client asks again: the suite stays interesting
+		time.Sleep(5*day + 7*time.Minute)
+		if a := s.ver.calls(); a < 10*15 || a > 10*30 { // deferred minor B-57: a floor too
+			t.Fatalf("untrusted body: %d attempts in 10 days, want 150..300", a)
 		}
 		if s.current(t) != "" {
@@ -148,10 +193,10 @@ func TestSim30Days_RetryDateRulesAndClockSteps(t *testing.T) {
 		s.serve(older)
 		before := s.ver.calls()
-		time.Sleep(8*day + 11*time.Minute)
+		time.Sleep(5*day + 11*time.Minute)
 		if s.current(t) != hashOf(v2) {
 			t.Fatal("the suite moved back to older-dated content")
 		}
-		if a := s.ver.calls() - before; a < 3 || a > 8*30 {
-			t.Fatalf("older content cost %d verifications in 8 days, want 3..240", a)
+		if a := s.ver.calls() - before; a < 3 || a > 5*30 {
+			t.Fatalf("older content cost %d verifications in 5 days, want 3..150", a)
 		}
 		if r := strOf(s.row(t).AdoptionFailReason); r != cache.ReasonUpstreamOlder {
@@ -166,5 +211,5 @@ func TestSim30Days_RetryDateRulesAndClockSteps(t *testing.T) {
 
 		restore := wallclock.SetOffsetForTest(-2 * time.Hour)
-		time.Sleep(4*day + 13*time.Minute)
+		time.Sleep(2*day + 13*time.Minute)
 		v3 := s.release("v3", wallclock.Now().Unix())
 		s.serve(v3)
@@ -176,5 +221,5 @@ func TestSim30Days_RetryDateRulesAndClockSteps(t *testing.T) {
 
 		restore = wallclock.SetOffsetForTest(30 * day)
-		time.Sleep(4*day + 17*time.Minute)
+		time.Sleep(2*day + 17*time.Minute)
 		v4 := s.release("v4", wallclock.Now().Unix())
 		s.serve(v4)
@@ -185,10 +230,123 @@ func TestSim30Days_RetryDateRulesAndClockSteps(t *testing.T) {
 		restore()
 		s.stop()
-		if n := len(s.up.Calls(rigAnchorURL)); n < 28*10 {
-			t.Fatalf("only %d checks over ~28 days, want >= 280 (liveness)", n)
+		days := int(time.Since(t0) / day)
+		if n := len(s.up.Calls(rigAnchorURL)); n < days*10 {
+			t.Fatalf("only %d checks over %d days, want >= %d (liveness)", n, days, days*10)
+		}
+	})
+}
+
+// Deferred minor B-58: with no Date on either side the Date rule cannot
+// refuse the older content; the rollback guard alone keeps the suite on
+// v2, and records it as the refusal state rollback_refused, not a failure.
+func TestSimFlappingMirror_UndatedContentIsARefusedRollback(t *testing.T) {
+	synctest.Test(t, func(t *testing.T) {
+		s := startRetrySim(t, 30*time.Minute)
+		v1, v2 := s.release("v1", 0), s.release("v2", 0)
+		s.serve(v1)
+		time.Sleep(34 * time.Minute) // v1 adopted (the first tick)
+		if s.current(t) != hashOf(v1) {
+			t.Fatal("setup: v1 not adopted")
+		}
+		var n atomic.Int64
+		s.up.Set(rigAnchorURL, func(ctx context.Context, etag string) (*fetch.ConditionalResult, error) {
+			if n.Add(1)%2 == 1 {
+				return simtest.Serve(v2)(ctx, etag)
+			}
+			return simtest.Serve(v1)(ctx, etag)
+		})
+		seenV2 := false
+		reasons := map[string]bool{}
+		for i := 0; i < 24*4; i++ { // sample every 15 min for a day, off the tick grid
+			time.Sleep(15 * time.Minute)
+			switch s.current(t) {
+			case hashOf(v2):
+				seenV2 = true
+			case hashOf(v1):
+				if seenV2 {
+					t.Fatalf("flipped back to v1 after %v", time.Duration(i+1)*15*time.Minute)
+				}
+			}
+			if r := s.row(t).AdoptionFailReason; r != nil {
+				reasons[*r] = true
+			}
+		}
+		s.stop()
+		if !seenV2 || s.current(t) != hashOf(v2) {
+			t.Fatal("never settled on v2")
+		}
+		// v2 served again clears the record (spec C7), so it comes and goes;
+		// whenever it is there, it is the refusal state.
+		if len(reasons) != 1 || !reasons[cache.ReasonRollbackRefused] {
+			t.Fatalf("record reasons %v, want only rollback_refused", reasons)
 		}
 	})
 }
 
+// Deferred minor B-56: clock steps while a failure record is active. A
+// backward step leaves next_retry_at far ahead: the retry still comes
+// within the clamped hour (spec C7). A forward step makes it long overdue:
+// the retry comes at the next tick. In both, content dated by the stepped
+// clock is adopted, and content dated more than 10 s ahead of it is the
+// failure release_date_in_future, not an adoption.
+func TestSimRetry_ClockStepsWithAnActiveRecord(t *testing.T) {
+	synctest.Test(t, func(t *testing.T) {
+		t.Cleanup(wallclock.SetOffsetForTest(0))
+		const refresh = 40 * time.Minute
+		tick := refresh / 4
+		s := startRetrySim(t, refresh)
+		failFor := func(label string) {
+			t.Helper()
+			bad := s.release(label, wallclock.Now().Unix())
+			s.ver.reject(hashOf(bad))
+			s.serve(bad)
+			for i := 0; i < 5 && s.row(t).AdoptionFailCount < 4; i++ {
+				time.Sleep(time.Hour) // the streak grows: next retry 16 min, then 32
+			}
+			if s.row(t).AdoptionFailCount < 4 {
+				t.Fatalf("%s: streak %d, want >= 4", label, s.row(t).AdoptionFailCount)
+			}
+		}
+		// Backward: the record's next_retry_at lands 2 h in the future.
+		failFor("bad-before-the-backward-step")
+		restore := wallclock.SetOffsetForTest(-2 * time.Hour)
+		before := s.ver.calls()
+		time.Sleep(time.Hour + tick + time.Minute)
+		if s.ver.calls() == before {
+			t.Fatal("after a backward step, no retry within the clamped hour")
+		}
+		ahead := s.release("ahead-of-the-stepped-clock", wallclock.Now().Add(time.Hour).Unix())
+		s.serve(ahead)
+		time.Sleep(refresh + tick + time.Minute) // the suite is due again once stale
+		if s.current(t) == hashOf(ahead) || strOf(s.row(t).AdoptionFailReason) != "release_date_in_future" {
+			t.Fatalf("content dated an hour ahead: current %q reason %q, want refused as release_date_in_future",
+				s.current(t), strOf(s.row(t).AdoptionFailReason))
+		}
+		good := s.release("good-after-the-backward-step", wallclock.Now().Unix())
+		s.serve(good)
+		time.Sleep(refresh + tick + time.Minute)
+		if s.current(t) != hashOf(good) {
+			t.Fatal("after a backward step, new content was not adopted")
+		}
+		restore()
+		// Forward: the record's next_retry_at is 30 days overdue.
+		failFor("bad-before-the-forward-step")
+		restore = wallclock.SetOffsetForTest(30 * 24 * time.Hour)
+		before = s.ver.calls()
+		time.Sleep(tick + time.Minute)
+		if s.ver.calls() == before {
+			t.Fatal("after a forward step, the overdue retry did not come at the next tick")
+		}
+		good = s.release("good-after-the-forward-step", wallclock.Now().Unix())
+		s.serve(good)
+		time.Sleep(refresh + tick + time.Minute)
+		if s.current(t) != hashOf(good) {
+			t.Fatal("after a forward step, new content was not adopted")
+		}
+		restore()
+		s.stop()
+	})
+}
+
 // Spec C5: a flapping mirror alternating old and new content never moves
 // the suite backwards.
@@ -212,4 +370,5 @@ func TestSimFlappingMirror_NeverFlipsBack(t *testing.T) {
 		})
 		seenV2 := false
+		time.Sleep(4 * time.Minute)   // samples off the 7 m 30 s tick grid (deferred minor B-58)
 		for i := 0; i < 3*24*4; i++ { // sample every 15 min for 3 days
 			time.Sleep(15 * time.Minute)
@@ -283,4 +442,9 @@ func TestSimRestartSpread(t *testing.T) {
 			t.Fatalf("%d of 50 failing suites retried within the refresh interval after the restart", retried)
 		}
+		// Deferred minor B-55: each was attempted once — an adoption, not
+		// only an upstream request.
+		if n := ver.calls(); n != 50 {
+			t.Fatalf("%d adoption attempts after the restart, want 50", n)
+		}
 		for b, n := range buckets {
 			if n > 25 {
diff --git a/internal/freshness/sim_sched_test.go b/internal/freshness/sim_sched_test.go
index f69ec01..fa72d47 100644
--- a/internal/freshness/sim_sched_test.go
+++ b/internal/freshness/sim_sched_test.go
@@ -2,4 +2,5 @@ package freshness
 
 import (
+	"bytes"
 	"context"
 	"encoding/json"
@@ -9,4 +10,5 @@ import (
 	"runtime"
 	"slices"
+	"strconv"
 	"strings"
 	"sync"
@@ -19,4 +21,5 @@ import (
 	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
 	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
+	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
 	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
 	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
@@ -113,6 +116,6 @@ func TestSimScheduler_LatencyBoundEightHosts(t *testing.T) {
 		}
 		due("h1", "/r/dists/s")
-		members.delay[rBase+"Contents-amd64.gz"] = 10 * time.Minute
-		members.delay[rByHash] = 10 * time.Minute
+		members.setDelay(rBase+"Contents-amd64.gz", 10*time.Minute)
+		members.setDelay(rByHash, 10*time.Minute)
 		up.Set(rBase+"InRelease", simtest.Serve(relR))
 		// C on h2 and C3 on h3: adopted once (so the current snapshot is
@@ -140,5 +143,5 @@ func TestSimScheduler_LatencyBoundEightHosts(t *testing.T) {
 			}
 			due(host, cSuite)
-			members.delay[pkgsURL] = 10 * time.Minute
+			members.setDelay(pkgsURL, 10*time.Minute)
 			var n atomic.Int64
 			var b simtest.Behavior = func(ctx context.Context, etag string) (*fetch.ConditionalResult, error) {
@@ -370,10 +373,138 @@ func TestSimScheduler_FiveHundredStallingOneHealthy(t *testing.T) {
 }
 
+// Deferred minor B-53 (phase B DoD): the stalling-host bound with an
+// adopter wired, so adoptions contend for the slots on the same host — C
+// publishes a new InRelease on every request and each adoption fetches its
+// Packages for 10 minutes — at a fifth of the 500-suite simulation's size
+// (that one, without an adopter, is already over the 5 s -race budget:
+// deferred minor C-38, phase H). Once the 100 stalling suites have failed,
+// the healthy S is checked within refresh + one tick + D every time, C
+// keeps adopting, and goroutines stay bounded.
+func TestSimScheduler_StallingHostWithAdoptionContention(t *testing.T) {
+	synctest.Test(t, func(t *testing.T) {
+		ctx := context.Background()
+		c := simtest.OpenCache(t)
+		up := simtest.NewUpstream()
+		members := &simMembers{fakeFetcher: newFakeFetcher(), stalled: map[string]bool{}, delay: map[string]time.Duration{}}
+		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
+			HostLimiter: hostsem.New(8), MaxConcurrent: 2, MaxDuration: 30 * time.Minute, Logger: simtest.DiscardLogger()})
+		if err != nil {
+			t.Fatal(err)
+		}
+		stale := time.Now().Add(-2 * time.Hour).Unix()
+		for i := 0; i < 100; i++ {
+			suite := fmt.Sprintf("/s%03d/dists/x", i)
+			seedSimSuite(t, c, "a", suite, stale, 0)
+			up.Set("http://a"+suite+"/InRelease", simtest.Stall())
+		}
+		adopt := func(suite string, pkgs []byte) {
+			t.Helper()
+			ref := SuiteRef{CanonicalScheme: "http", CanonicalHost: "a", SuitePath: suite}
+			members.put("http://a"+suite+"/main/binary-amd64/Packages", pkgs)
+			trackSuite(t, c, ref)
+			if err := ad.Run(ctx, ref, signedRelease("v0"+suite, 0, map[string][]byte{"main/binary-amd64/Packages": pkgs}), "", ""); err != nil {
+				t.Fatal(err)
+			}
+		}
+		const sPath, cPath = "/zzz/dists/x", "/ccc/dists/x"
+		sURL := "http://a" + sPath + "/InRelease"
+		adopt(sPath, fakePackagesStanzas(map[string]string{"pool/s_1.deb": testHex64}))
+		up.Set(sURL, simtest.NotModified())
+		pkgs := func(n int64) []byte {
+			return fakePackagesStanzas(map[string]string{fmt.Sprintf("pool/c_%d.deb", n): testHex64})
+		}
+		adopt(cPath, pkgs(0))
+		cPkgs := "http://a" + cPath + "/main/binary-amd64/Packages"
+		members.setDelay(cPkgs, 10*time.Minute)
+		var n atomic.Int64
+		up.Set("http://a"+cPath+"/InRelease", func(ctx context.Context, etag string) (*fetch.ConditionalResult, error) {
+			k := n.Add(1)
+			members.put(cPkgs, pkgs(k)) // before the check can spawn the adoption
+			return simtest.Serve(signedRelease(fmt.Sprintf("c%d", k), 0, map[string][]byte{"main/binary-amd64/Packages": pkgs(k)}))(ctx, etag)
+		})
+		cAdoptions := func() float64 {
+			v, _ := strconv.ParseFloat(renderedMetricLabels("acu_adoption_total", `outcome="success"`, `host="a"`), 64)
+			return v
+		}
+		startAdoptions := cAdoptions()
+		const refresh = time.Hour
+		tick := refresh / 4
+		runtime.Gosched()
+		base := runtime.NumGoroutine()
+		maxG := base
+		_, stop := startSched(t, Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: refresh,
+			Adopter: ad, Logger: simtest.DiscardLogger(), rand: func() float64 { return 0.5 }})
+		// Phase 1: every stalling suite fails once (100 × D on one host).
+		for time.Sleep(61 * time.Second); ; time.Sleep(61 * time.Second) {
+			maxG = max(maxG, runtime.NumGoroutine())
+			all, err := c.ListSuites(ctx)
+			if err != nil {
+				t.Fatal(err)
+			}
+			failing := 0
+			for _, s := range all {
+				if s.CheckFailCount > 0 {
+					failing++
+				}
+			}
+			if failing == 100 {
+				break
+			}
+		}
+		before := len(up.Calls(sURL))
+		if before == 0 {
+			t.Fatal("S never checked in phase 1")
+		}
+		// Phase 2: three hours of contention.
+		for end := time.Now().Add(3*time.Hour + 11*time.Minute); time.Now().Before(end); time.Sleep(61 * time.Second) {
+			maxG = max(maxG, runtime.NumGoroutine())
+		}
+		stop()
+		calls := up.Calls(sURL)[before-1:]
+		if len(calls) < 3 {
+			t.Fatalf("S checked %d times in phase 2, want at least 2", len(calls)-1)
+		}
+		for i := 1; i < len(calls); i++ {
+			if gap := calls[i].Sub(calls[i-1]); gap > refresh+tick+checkDeadline {
+				t.Fatalf("S waited %v between checks, bound %v", gap, refresh+tick+checkDeadline)
+			}
+		}
+		if got := cAdoptions() - startAdoptions; got < 3 {
+			t.Fatalf("C adopted %v times, want it to keep adopting (>= 3)", got)
+		}
+		const slack = 8
+		if limit := base + checkWorkers + 1 + 2 + slack; maxG > limit {
+			t.Fatalf("goroutines peaked at %d, limit %d", maxG, limit)
+		}
+	})
+}
+
+// renderedMetricLabels returns the value of the series name whose labels
+// include every one of labels, as metrics.Default renders it ("" if none).
+func renderedMetricLabels(name string, labels ...string) string {
+	var buf bytes.Buffer
+	metrics.Default.Render(&buf)
+	for _, line := range strings.Split(buf.String(), "\n") {
+		if !strings.HasPrefix(line, name+"{") {
+			continue
+		}
+		ok := true
+		for _, l := range labels {
+			ok = ok && strings.Contains(line, l)
+		}
+		if ok {
+			return line[strings.LastIndexByte(line, ' ')+1:]
+		}
+	}
+	return ""
+}
+
 // simMembers is an AdoptionFetcher serving canned members, except that
 // every fetch for a stalled host blocks until its context ends and a URL
 // with a delay answers only after it. It tracks how many stalled fetches
-// are in flight and, once started is set, each host's first member fetch
-// (an adoption's admission). Set delays and started before the scheduler
-// starts.
+// are in flight and, once recordStarts was called, each host's first
+// member fetch (an adoption's admission). Its maps are written only
+// through stall, setDelay and recordStarts, under its lock (deferred minor
+// B-51).
 type simMembers struct {
 	*fakeFetcher
@@ -388,4 +519,14 @@ type simMembers struct {
 func (m *simMembers) stall(host string) { m.mu.Lock(); m.stalled[host] = true; m.mu.Unlock() }
 
+// setDelay makes every fetch of url answer only after d.
+func (m *simMembers) setDelay(url string, d time.Duration) {
+	m.mu.Lock()
+	m.delay[url] = d
+	m.mu.Unlock()
+}
+
+// recordStarts starts recording each host's first member fetch.
+func (m *simMembers) recordStarts() { m.mu.Lock(); m.started = map[string]time.Time{}; m.mu.Unlock() }
+
 func (m *simMembers) Fetch(ctx context.Context, target *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error) {
 	m.mu.Lock()
@@ -467,5 +608,5 @@ func TestSimCapacity_SlowDripAndDegraded(t *testing.T) {
 			}
 			members.put(pkgsURL(host), pkgsFor("2-"+host))
-			members.delay[pkgsURL(host)] = time.Minute
+			members.setDelay(pkgsURL(host), time.Minute)
 			v2[host] = rel("v2-"+host, pkgsFor("2-"+host))
 			up.Set(anchorURL(host), simtest.After(time.Second, simtest.Serve(v2[host])))
@@ -480,5 +621,5 @@ func TestSimCapacity_SlowDripAndDegraded(t *testing.T) {
 		seedStaleAnchorAt(t, c, "deg", rigSuite)
 		up.Set(anchorURL("deg"), simtest.Stall())
-		members.started = map[string]time.Time{}
+		members.recordStarts()
 
 		runtime.Gosched()
```

- [ ] **Step 2: Run them**

Run: `go test -race -run 'TestSim' -v ./internal/freshness/`
Expected: PASS; every simulation but `TestSimScheduler_FiveHundredStallingOneHealthy` (C-38, phase H) under 4.5 s — the 30-day one about 3.6 s, the burst 3.0–3.5 s. They pin behaviour the code already has; Step 3 shows their teeth.

- [ ] **Step 3: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| `classOf`: a failing suite counted healthy | `TestSimScheduler_StallingHostWithAdoptionContention` |
| `jitterDelay` ignores the random value | `TestSimLim1_AttemptsAcrossTheJitter` |
| `retryGateFor`: a record is never due | `TestSimRestartSpread` |
| `retryGateFor`: no clamp of a far `next_retry_at` | `TestSimRetry_ClockStepsWithAnActiveRecord` |
| the Date rule's future check removed | `TestSimRetry_ClockStepsWithAnActiveRecord` |
| `retryMaxDelay` 2 h | `TestSim30Days_RetryDateRulesAndClockSteps` (124 attempts in 10 days) |
| the natural-key collision not classified `rollback_refused` | `TestSimFlappingMirror_UndatedContentIsARefusedRollback` |

The coarsened 30-day simulation keeps its teeth (each mutant run against it alone):

| Mutation | Fails |
|---|---|
| `retryMaxDelay` 30 min (a retry storm) | `TestSim30Days_RetryDateRulesAndClockSteps` (360 attempts in 10 days) |
| `retryMaxDelay` 2 h | `TestSim30Days_RetryDateRulesAndClockSteps` (124 attempts in 10 days) |
| a refusal's streak restarts at every attempt (`\|\| cache.IsRefusalReason(reason)`) | `TestSim30Days_RetryDateRulesAndClockSteps` (180 verifications of the older content in 5 days) |
| the Date rule off (`if false && prior != nil && …`) | `TestSim30Days_RetryDateRulesAndClockSteps` |

- [ ] **Step 4: Commit**

```bash
gofmt -l internal/freshness/sim_sched_test.go internal/freshness/sim_retry_test.go internal/freshness/sim_burst_test.go \
  internal/freshness/deferred_e_test.go internal/freshness/reoffer_test.go internal/freshness/flip_barrier_test.go
git add internal/freshness/sim_sched_test.go internal/freshness/sim_retry_test.go internal/freshness/sim_burst_test.go \
  internal/freshness/deferred_e_test.go internal/freshness/reoffer_test.go internal/freshness/flip_barrier_test.go
git commit -m "Close the simulations' deferred minors: contention, jitter, clock steps with a record, rollback alone"
```

### Task 15: Documentation, spec revision 8, overview, deferred files, final verification

Spec §8 for this phase — the operator documentation of eager first adoption (C4), the anchor forms and the unsigned form with its safeguards and its threat notes at the knob (C5), SHA512-only repositories, strict mode's in-flight wait (C10), and the events and metrics C11 lists — then the rulings R-E1–R-E4 and PE-1–PE-8 (with the design-review items they accept), deviations 1 and 2 and the design notes the spec should state, written into the spec as revision 8; the overview's phase E bookkeeping and the carry-forwards for F, G and H; and the deferred files. `packaging/config/config.toml.default` documented `adopt_unsigned` in Task 11 (its C-37 comments are phase H's).

Deferred minors in the files this task changes: B-67 (`docs/admin-ui-spec.md`: the `adoption_refused_persistently` sentence), D-53 (`docs/log-fields.md`: `clock_step_detected` names the creation attempts' re-reads), D-54 (spec C4: the queue bound's admission order), D-51 (resolved by A-37 in Task 13: the heartbeat now logs its stack), D-59 (moot: Task 13 finishes C-19's file split, so no summary needs "C-19 (wrap only)"). Left in these files: C-36, C-37, D-50, D-52, D-55 (H; the new **H:** carry-forward block holds phase E's items only). C-40 is closed (ruling PE-7): the task commits are history and the tests are the durable evidence; copying mutant tables into the docs would be maintenance noise — the reasoning that closed D-59. C-21 is re-tagged (H) (ruling PE-7, design review M-18), so the deferred-minor rule stays mechanical, and C-38 is narrowed to the 500-suite simulation (Task 14 coarsened the 30-day one).

The docs and the spec state the design as the plan reviews' rulings leave it: the checker's marker writer (PE-5, I-2); the pacing of deferred never-adopted suites and adopted `anchor_absent` suites (PE-2, PE-3) and C7's gate wording (I-1); the symmetric interest window (M-3); never-downgrade under both schemes and with any kept signed snapshot (M-7, M-8), the re-mark's twin rows and its log line (I-3, M-1); strict mode's wait bounded by the request's age (M-10), its per-request cost and the barrier's (M-11 and the verdict table), the refusal logged once per fetch (O1); the miss path's backstop deadline (M-15); the probe bound (PE-4); and the documentation items M-4, M-5, M-6, M-9, M-12, M-13, M-14, M-17 and underspecified 3 (a tree result's `upstream_status`), 7 and 8. M-20's correction of the `last_seen_at` carry-forward is said in so many words. M-14 and M-15 are done in phase E, so no H carry-forward is needed for them, and I-3 is done too (PE-5), so it needs no G item.

**Every deferred minor, as this plan leaves it.** Taken: A-29 (Task 1); A-49, C-16 (Task 3); B-23, B-25, B-33, B-34, B-35 (Task 4); B-30, B-31, B-32, B-37 (Task 7); B-38, B-45, B-46, B-66, D-23 (Task 8); D-33 (Task 9); D-34, D-35, D-37 (Task 10); D-45, D-47 (Task 11); A-37, A-47, B-16, B-17, B-18, B-19, B-20, B-21, B-24, B-28, C-17, C-19, C-29, D-1, D-2, D-5, D-21, D-22, D-38 (Task 13); B-51, B-53, B-54, B-55, B-56, B-57, B-58 (Task 14); B-67, C-40, D-51, D-53, D-54, D-59 (this task; C-40 and D-59 closed with their reasons). Checked and left:

- tagged for another phase: C-11 (F; `cache/adoption.go`); D-3, D-4 (G; `aptpath/sniff_test.go`, `sniff.go`), D-32 (G; `handler/suite_rows.go`); C-9, C-21 (re-tagged here, ruling PE-7), C-36, C-37, C-38 (narrowed here), C-39, D-50, D-52, D-55, D-56, D-57, D-58 (H); A-56 (H; review fix round 1 re-filed it: its files, `handler.go`, `freshness.go` and `adoption.go`, are hand-edited by phase E, but it is informational naming with no behaviour, and a rename would churn three packages);
- in files phase E changes only mechanically (ruling PD-1): B-36 (`check_streak_test.go`), D-24, D-25 (`suite_lock_test.go`, `suite_lock.go`), A-15 (`hot_set.go`; `hot_prefetch_test.go` gets only Task 3's `sed`);
- in files no task changes: A-1–A-14 (`wallclock`, `aptpath` suite and encoding, `proxy`), A-19–A-21 (cache schema tests), A-26, A-27 (`cache/legacy_audit.go`), A-55; B-9–B-14 (`release_date.go`), B-27 (`restart_reset_test.go`), B-52 (the phase B plan), B-59–B-63 (`internal/admin`); C-1–C-8 (`flipgate`, `simtest`), C-12, C-13 (`flip_state_test.go`); D-6 (`gpg/failclosed_test.go`), D-7 (`simtest/clearsign.go`), D-8–D-20 (`cache` tests, `cache/adoption_failure.go`, `cache/suite_rows.go`, `cache/fetched_row.go`), D-26, D-27 (reconcile tests), D-28–D-31 (handler tests), D-36 (`suite_hit_test.go`), D-39–D-44 (`suite_dod_test.go`), D-46, D-48, D-49 (cmd tests), D-60 (`cache/open_cache_test.go`).

**Files:**
- Modify: `docs/configuration.md`, `docs/log-fields.md`, `docs/admin-ui-spec.md`, `docs/superpowers/specs/2026-09-27-flat-repositories-design.md`, `docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md`, `…-A-deferred.md`, `…-B-deferred.md`, `…-C-deferred.md`, `…-D-deferred.md`

**Interfaces:**
- Consumes: the names, events, metrics and constants Tasks 1–14 produce, exactly as they produce them.
- Produces: no code.

- [ ] **Step 1: Operator documentation (`docs/configuration.md`)**

`[freshness]` gains *Eager first adoption*; `[adoption]` gains the `adopt_unsigned` row and *Anchor forms and unsigned repositories* (the five safeguards; spec C5's threat notes, at the knob as the spec asks, with the signed-seen marker's denial-of-service residual; SHA512-only repositories and their repair limitation); the refusal list grows to six; *Capacity and fairness* names the repair counter and the unsigned reserved-slot rule; *Upgrading an existing cache* says how quarantined plain-text suites, signed-history suites, dormant suites and pre-upgrade SHA512 by-hash names behave; `[integrity]` documents strict mode's wait and its consequences (spec C10).

```diff
diff --git a/docs/configuration.md b/docs/configuration.md
index 2a00cd2..9e49236 100644
--- a/docs/configuration.md
+++ b/docs/configuration.md
@@ -266,4 +266,46 @@ copy under its own path is answered from an identical cached file only for
 a tracked suite; otherwise it is fetched from upstream.
 
+### Eager first adoption
+
+With adoption enabled, a tracked suite that has no snapshot yet is
+adopted on its first check, in whichever form its upstream publishes (see
+*Anchor forms and unsigned repositories*), when a client asked for it in
+the last 7 days and a client fetched its `InRelease` or `Release` through
+the cache (a copy the cache rebuilt for itself does not count). The
+request that creates a suite starts its first check once its response is
+sent, so a new repository is adopted within seconds of its first
+`apt update` when an adoption slot is free. A suite nobody asked for in 7
+days is dormant: it costs no upstream request until a client asks again,
+and is then checked — and adopted, when a slot is free — within one
+scheduler tick (`periodic_refresh / 4`) of that request (the request's own
+check may run before its interest is recorded). A request stamp up to 7 days in the future, left by a clock
+that was stepped back, still counts as interest; one further ahead leaves
+the suite dormant until a client asks for it again. After an upgrade, a
+suite counts as asked for when a client fetched one of its anchor files in
+the 7 days before the upgrade.
+
+A suite whose first adoption failed or was refused (see *How failed
+adoptions are retried*) is probed again at its retry time (2 minutes,
+doubling to an hour), or once `periodic_refresh` has passed since its last
+check if that comes first; checks that clients trigger in between ask
+upstream nothing. A suite whose upstream serves neither `InRelease` nor
+`Release` fails its check (backing off from 2 minutes to an hour) and
+shows "refused: anchor_absent" on the status page.
+
+First adoptions never take the last free adoption slot, which is kept for
+suites that already serve a signed snapshot (see *Capacity and
+fairness*): with the default `max_concurrent_adoptions = 2` a first
+adoption runs only while no other adoption runs, so on a cache kept busy
+by signed updates it can wait a long time. Busy caches should set `3`,
+which also lets two first adoptions run at once, for example for the
+burst after an upgrade. A suite waiting for a slot is probed twice by the
+scheduler — by the check that queued it and by the re-check that admits
+it — plus once for each client check while it waits (at most one per
+`cooldown`) and once for each slot offered to it that another admission
+takes first.
+
+With adoption disabled nothing changes: suites are checked for changes
+only, and the request that creates a suite starts no check.
+
 ## `[adoption]`: snapshots, trust, and recovery
 
@@ -271,5 +313,6 @@ a tracked suite; otherwise it is fetched from upstream.
 | --- | --- | --- | --- |
 | `enabled` | boolean | `false` | Enable adoption of new snapshots. When false, freshness differences are recorded without adopting them, so tracked suites lag upstream by design: the status page's SUITES vital then reads "ADOPTION DISABLED — METADATA NOT REFRESHED" (stale, not a fault), rows keep their lag text without warning, and failure records left from a time adoption was enabled are inert (no retries, `acu_adoption_failing_suites` reports 0). |
-| `require_signature` | boolean | `true` | Allows starting with an empty keyring. It no longer relaxes verification: only a decodable clearsigned `InRelease` is adopted as inline metadata in every mode (plain-text bodies are `missing_signature`, undecodable ones `malformed_clearsign`). Unsigned repositories are handled by the unsigned form (a later release). |
+| `require_signature` | boolean | `true` | Allows starting with an empty keyring. It no longer relaxes verification: only a decodable clearsigned `InRelease` is adopted as inline metadata in every mode (plain-text bodies are `missing_signature`, undecodable ones `malformed_clearsign`). Unsigned repositories are adopted in the unsigned form instead (`adopt_unsigned`). |
+| `adopt_unsigned` | boolean | `true` | Adopt a repository that publishes a `Release` with no signature (neither `InRelease` nor `Release.gpg`) as an unsigned snapshot: served coherently, but never trusted — it vouches no `.deb` and never counts for `integrity.refuse_unvouched_debs`. A suite the cache has seen signed is never adopted unsigned. `false` leaves such repositories served from the ordinary cache, unadopted. See *Anchor forms and unsigned repositories* for the safeguards and the threat notes. |
 | `require_pinned_signer` | boolean | `false` | Reject adoption when no `[[trusted_signer]]` matches the canonical host. Evaluated before `accept_any_signer`. |
 | `accept_any_signer` | boolean | `false` | For hosts without matching signer pins, skip both signer trust checks **and cryptographic signature verification**. A decodable clearsigned `InRelease` (or, for the detached pair, a parseable `Release.gpg` signature) is still required, as in every mode. Matching pins keep normal verification. Logs a startup warning. |
@@ -292,10 +335,14 @@ When an adoption fails (an untrusted or malformed signature, a member that will
 If the cache cannot read the current snapshot when an adoption starts, the attempt fails as `db_failed` (and is retried on the backoff schedule) rather than skipping the rollback checks below.
 
-Two outcomes are refusals, not failures, and are shown as "refused: …" on the status page:
+Six outcomes are refusals, not failures, and are shown as "refused: …" on the status page:
 
 - `upstream_older`: upstream serves a Release whose `Date` is older than the snapshot being served, or has no `Date` at all after a dated snapshot (a missing `Date` counts as 0). apt would ignore it too; the cache keeps serving the newer snapshot.
 - `rollback_refused`: upstream serves content identical to an older snapshot the cache already replaced (a flapping mirror). Only the last `gc.keep_displaced` snapshots are remembered for this.
+- `unsigned_unconfirmed`: an unsigned repository's first adoption waits for a second observation of the same `Release` (see *Anchor forms and unsigned repositories*).
+- `unsigned_downgrade_refused`: the suite has been seen signed, so its unsigned `Release` is not adopted.
+- `unsigned_too_large`: an unsigned `Release` declares more than 1,000 files.
+- `anchor_absent`: the suite's current anchor file is gone upstream, or a detached suite's changed `Release` comes without its `Release.gpg`; its snapshot keeps serving, and the suite moves to its other signed form when upstream serves one. A suite without a snapshot whose upstream serves neither `InRelease` nor `Release` shows it too.
 
-Refused content follows the same retry schedule as a failure, but a refusal is a state, not a failure: it logs INFO (`adoption_refused`, and `adoption_run_failed` at INFO), is not counted by `acu_adoption_failing_suites`, and shows no lag. A refusal that lasts more than 24 hours is escalated anyway, because it may be a silent freeze (a vendor that stops emitting `Date` is refused `upstream_older` forever): the status page warns on its row and on the SUITES vital ("REFUSED 24H+", never critical) and the log carries WARN `adoption_refused_persistently` at the streak milestones.
+Refused content follows the same retry schedule as a failure, but a refusal is a state, not a failure: it logs INFO (`adoption_refused`, and `adoption_run_failed` at INFO) and is not counted by `acu_adoption_failing_suites`. `upstream_older` and `rollback_refused` show no lag (the cache already serves content at least as new as upstream's); the other four keep their lag, because the suite is in fact behind. A refusal that lasts more than 24 hours is escalated anyway, because it may be a silent freeze (a vendor that stops emitting `Date` is refused `upstream_older` forever): the status page warns on its row and on the SUITES vital ("REFUSED 24H+", never critical) and the log carries WARN `adoption_refused_persistently` at the streak milestones.
 
 A Release dated more than 10 seconds ahead of the cache's clock is a failure (apt rejects it as "not valid yet"); check the cache host's clock if this persists.
@@ -305,5 +352,5 @@ At start, every failing suite gets one retry spread over the next `periodic_refr
 ### Capacity and fairness
 
-`freshness.max_concurrent_adoptions` slots are shared by adoptions, repairs and reconciles, plus one slot per upstream host. Nothing blocks on a slot: a suite whose scheduled check finds none waits in the scheduler's queue and is re-checked as soon as capacity it can use is released (hosts in turn, oldest first); while it waits, no request is made for it. A change found by a client-triggered check that finds no slot stays pending, and the next scheduler tick checks that suite (bypassing the cooldown) and queues it the same way, so it starts waiting within one tick. Client requests still trigger checks under the normal cooldown and may take a freed slot first. When there are at least two slots, the last free one is reserved for suites that already serve a signed snapshot, so security updates are not delayed by first adoptions. With the default pool of two, first adoptions therefore run only when no other adoption is running; `max_concurrent_adoptions = 3` lets two run at once (useful for the burst after an upgrade). A repair or reconcile that finds no slot simply runs on a later unchanged check. `POST /reconcile` answers 409 when no slot is free. The gauge `acu_freshness_waiting_for_slot` counts suites waiting for a slot. An adoption gives its host's slot back when its downloads are done, but keeps its global slot through the flip barrier and the commit, so a slot is held for at most `max_duration` + 140 s (the barrier, see *Flip consistency*; `acu_adoption_duration_seconds` includes that time) + the 5-minute commit bound: N suites waiting for k slots they may use are all admitted within one tick + ⌈N/k⌉ × (that sum + 2 minutes).
+`freshness.max_concurrent_adoptions` slots are shared by adoptions, repairs and reconciles, plus one slot per upstream host. Nothing blocks on a slot: a suite whose scheduled check finds none waits in the scheduler's queue and is re-checked as soon as capacity it can use is released (hosts in turn, oldest first); while it waits, no request is made for it. A change found by a client-triggered check that finds no slot stays pending, and the next scheduler tick checks that suite (bypassing the cooldown) and queues it the same way, so it starts waiting within one tick. Client requests still trigger checks under the normal cooldown and may take a freed slot first. When there are at least two slots, the last free one is reserved for suites that already serve a signed snapshot, so security updates are not delayed by first or unsigned adoptions. With the default pool of two, first adoptions therefore run only when no other adoption is running, and can wait a long time on a cache kept busy by signed updates; `max_concurrent_adoptions = 3` avoids that and lets two run at once (useful for the burst after an upgrade). A repair or reconcile that finds no slot simply runs on a later unchanged check (`acu_repair_deferred_total{host}` counts them); one of a suite whose current snapshot is unsigned never takes the reserved slot. `POST /reconcile` answers 409 when no slot is free. The gauge `acu_freshness_waiting_for_slot` counts suites waiting for a slot. An adoption gives its host's slot back when its downloads are done, but keeps its global slot through the flip barrier and the commit, so a slot is held for at most `max_duration` + 140 s (the barrier, see *Flip consistency*; `acu_adoption_duration_seconds` includes that time) + the 5-minute commit bound: N suites waiting for k slots they may use are all admitted within one tick + ⌈N/k⌉ × (that sum + 2 minutes).
 
 Periodic checks run on four workers, at most one check per upstream host at a time, hosts taking turns. On each host, healthy suites go first, then never-checked suites, then suites whose checks keep failing; a failing suite waits 2 minutes, doubling to an hour, between checks. The upstream part of each check (waiting for the host's fetch slot and the requests) is limited to 2 minutes; the check's own cache writes are not cut short, so the deadline is always recorded. A due suite S is therefore checked within one tick + (n + 1) × ⌈H/4⌉ × 2 minutes, where H is the number of hosts with suites waiting and n the number of suites ahead of S in its host's queue (only healthy ones when S is healthy): stalling hosts delay a healthy suite on another host by at most ⌈H/4⌉ checks per suite ahead of it (with four hosts or fewer, one check), and once a stalling host's suites are known to fail they queue behind its healthy suites — also right after a restart, because the order is persisted. The bound assumes responsive cache I/O.
@@ -334,5 +381,108 @@ does not verify them cryptographically. `require_pinned_signer = true` still
 rejects unpinned suites, so combining the two does not bypass pin requirements.
 `require_signature = false` no longer lets unsigned data through the inline
-path. The detached fallback still requires both `Release` and `Release.gpg`.
+path, and the detached form still requires both `Release` and `Release.gpg`:
+unsigned metadata is adopted only in the unsigned form below, which trusts
+nothing.
+
+### Anchor forms and unsigned repositories
+
+A suite is adopted in whichever form its upstream publishes: **inline** (a
+clearsigned `InRelease`), **detached** (`Release` with `Release.gpg`) or
+**unsigned** (`Release` alone). A suite with no snapshot yet asks for
+`InRelease`, then `Release`, then `Release.gpg`; an answer counts as present
+only when it is the file it claims to be. An HTML page served with status
+200 is absent, like 401, 403, 404 and 410; any other answer (a 5xx, a
+timeout, a redirect) fails the check without deciding anything. The admin
+JSON's `suites[].anchor_form` names a suite's form.
+
+Suites move between the signed forms by themselves. A detached suite asks
+for `InRelease` on every check and moves to the inline form as soon as
+upstream serves one. When a suite's current anchor (`InRelease` for
+inline, `Release` for detached and unsigned) is absent, the status page
+shows "refused: anchor_absent" while the snapshot keeps serving. The suite
+then asks for its anchor again only at its retry times (2 minutes,
+doubling to an hour); checks in between, including those clients trigger,
+ask upstream nothing, so a returning anchor is noticed at the next retry.
+Once the absence has lasted `max(cooldown, 5 minutes)`, each retry also
+probes the other forms, and the suite moves to the other signed form if
+upstream serves it. The snapshot keeps serving however long its anchor
+stays absent: clients of a release removed upstream (an end-of-life
+distribution) keep getting it through the cache where upstream would
+answer 404, so "refused: anchor_absent" is the sign to update their
+sources. A suite never moves from a signed form to unsigned: a signed
+suite whose upstream stops signing keeps its last signed snapshot (a
+detached suite whose `Release` changes while its `Release.gpg` is absent
+shows "refused: anchor_absent" and is paced the same way; a `Release.gpg`
+request that fails — a 5xx, a timeout — logs WARN `freshness: Release.gpg
+fetch failed; deferring adoption` and is tried again at the next check).
+An unsigned suite moves to a signed form at the first check that finds
+`InRelease` or `Release.gpg`. From that check on the cache has seen the
+suite signed, so it is never adopted unsigned again, even when the signed
+adoption fails (a key the cache does not trust) or the signature later
+disappears.
+
+Unsigned adoption has five safeguards:
+
+1. `adopt_unsigned` (default `true`).
+2. A suite's first unsigned adoption waits for two checks that see the same
+   `Release` at least `max(cooldown, 5 minutes)` apart — an hour apart when
+   `InRelease` or `Release.gpg` answers 401 or 403 at the confirming check,
+   since a firewall or an S3 permission error can hide a signature (a 403
+   that later turns into 404s confirms after the shorter gap). Until then
+   the status page shows "refused: unsigned_unconfirmed". A `Release`
+   generated anew for each request, or one that differs between
+   load-balanced backends, never shows the same bytes twice and stays
+   unconfirmed. Later updates of an unsigned suite are adopted at once.
+3. Never a downgrade: a suite the cache has seen signed is never adopted
+   unsigned ("refused: unsigned_downgrade_refused"). Seen signed means it
+   has a signed snapshot (the current one, or one of the displaced
+   snapshots the cache keeps), or the cache served its signed `InRelease`
+   or `Release.gpg`, a check found one upstream, or an earlier version had
+   cached one (recorded at upgrade). The record covers the repository path on its host under both
+   `http` and `https`, so a suite seen signed over one scheme is never
+   adopted unsigned over the other. The rule is checked before an adoption slot is taken and again
+   inside the commit, so a signed anchor served while an unsigned adoption
+   runs still stops it. Just before the commit the cache also re-reads the
+   signed anchors it has cached for the suite and records any it finds; if
+   it cannot read them, the adoption is refused as well (logged as
+   `adoption: signed-anchor re-mark failed`): that refusal means the cache
+   could not rule out a signature.
+4. At most 1,000 files declared in the `Release`'s SHA256 and SHA512
+   lists, each path counted once (files listed only under MD5Sum or SHA1
+   do not count), checked before an adoption slot is taken
+   ("refused: unsigned_too_large").
+5. An unsigned snapshot vouches no `.deb`, never counts for strict mode
+   and never records its suite as signed.
+
+Threat notes. The record that a suite was seen signed cuts both ways:
+anyone who can make the cache see one signed-looking `InRelease` or
+`Release.gpg` for a repository path — served to a client, or found by a
+check; a man in the middle, a compromised upstream, or a LAN client
+pointing the cache at a host it controls — blocks unsigned adoption of
+that path until its suite is forgotten (`POST /forget`, added with suite
+reaping in a later release). That denial of service is the accepted price
+of never downgrading. A man in the middle between the cache and upstream
+can make the cache adopt forged unsigned content only for a suite never
+seen signed, and only after two consistent observations; apt clients that
+verify signatures reject it, and `[trusted=yes]` clients accept it as they
+would when fetching directly. A suite whose pre-upgrade snapshot was
+quarantined because its stored `InRelease` was missing, corrupt or
+oversized (see *Upgrading an existing cache*) may have no record that it
+was signed until a check sees its signature upstream; if its signatures
+are gone by then (stripped by a man in the middle, for instance), it
+adopts unsigned. Under the default allow-all upstream policy
+with adoption enabled, a LAN client can make the cache adopt unsigned
+directories it controls; that is bounded by the per-host adoption slot,
+the reserved slot, `max_duration`, the 1,000-file cap and
+`freshness.max_suites`. To harden, set `adopt_unsigned = false` or narrow
+`upstream.allowed_host_regex`.
+
+`Release` files that list only SHA512 hashes (some vendor repositories)
+adopt: every declared SHA256 and SHA512 hash of a file is checked, and
+by-hash requests are answered for both algorithms, from the current
+snapshot and from the displaced ones kept for retained by-hash. An
+optional file that lists only a SHA512 hash and fails its integrity check
+is not retried by automatic repair; the reconcile on unchanged checks
+still heals the index files apt requests.
 
 ### Architecture and member availability
@@ -372,9 +522,12 @@ line names it. A suite quarantined for declarations outside its signature
 adopts again on its first freshness check, because its tampered copy never
 matches a clean upstream; one quarantined because its stored file was lost
-or corrupt waits for the next upstream change. A plain-text suite cannot
-adopt in this release (every attempt fails `missing_signature`): it is served
-from the ordinary cache without vouching until upstream publishes a
-clearsigned `InRelease`, or, from the release that adds the unsigned form,
-until it adopts as an unsigned snapshot. In strict mode, a quarantined
+or corrupt waits for the next upstream change, and may have no record that
+it was signed (see the threat notes in *Anchor forms and unsigned
+repositories*). A quarantined plain-text
+suite adopts again in the unsigned form, through its `Release` (its
+plain-text "InRelease" counts as absent), once a client has asked for it
+and two checks have seen the same `Release` (see *Anchor forms and
+unsigned repositories*), unless the cache has seen it signed; an unsigned
+snapshot vouches no package. In strict mode, a quarantined
 suite's packages are refused while other suites on the same host have
 complete coverage, as for any unadopted suite. While inline snapshots are
@@ -391,11 +544,21 @@ not stop the daemon either: that snapshot keeps serving as before, a
 The audit resumes if the start is interrupted and is a no-op afterwards. A
 suite whose signed `InRelease` was served by an earlier version is recorded
-as signed from cached anchors; one whose cached anchor was already
-garbage-collected leaves no record. Suites whose `InRelease` an earlier
-release cached but never tracked are tracked, with their signed-seen record,
+as signed from cached anchors, so it is never adopted unsigned; one whose
+cached anchor was already garbage-collected leaves no record. Suites whose
+`InRelease` an earlier release cached but never tracked are tracked, with
+their signed-seen record,
 on their first cache hit. A cache with more than `freshness.max_suites`
 suites, or a tenth of it or more on one host, logs WARN
 `suite_caps_exceeded` at start; see *Tracked suites*.
 
+After the upgrade, suites without a snapshot adopt eagerly only if a client
+fetched one of their anchor files in the 7 days before the upgrade, or asks
+again (see *Eager first adoption*); with the default
+`max_concurrent_adoptions = 2` these first adoptions run one at a time,
+while signed updates keep their own slot. Snapshots adopted before the
+upgrade carry no SHA512 by-hash names: such requests are answered as
+before (from the ordinary cache or upstream) until the suite's next
+adoption.
+
 ## `[hot_packages]`, `[hold_packages]`, and `[retention]`: package working set
 
@@ -425,5 +588,5 @@ grace starts; bytes are later reclaimed after references disappear and
 | `validate_at_rest_interval` | duration | `"24h"` | Periodically rehash blobs reachable from current snapshots. `"0s"` disables this background scan. Nonnegative. |
 | `validate_at_rest_workers` | integer | `4` | Parallel scan workers. Must be at least `1` when the interval is positive; `0` is allowed only with scanning disabled. Never negative. |
-| `refuse_unvouched_debs` | boolean | `false` | Reject `.deb` requests with `502` and `Retry-After` when the path lacks a current-snapshot hash and all current snapshots for that host have proven complete package coverage. Inert when adoption is disabled; incomplete coverage retains the permissive fetch behavior. |
+| `refuse_unvouched_debs` | boolean | `false` | Reject `.deb` requests with `502` and `Retry-After` when the path lacks a current-snapshot hash and all current signed snapshots for that host have proven complete package coverage (unsigned snapshots never count). Inert when adoption is disabled; incomplete coverage retains the permissive fetch behavior. A request first waits up to 15 seconds for a signed adoption running on its host; see below. |
 
 These switches do not disable the normal download-time hash checks. A detected
@@ -431,4 +594,28 @@ at-rest mismatch logs an error and removes the corrupt pool file. Package misses
 can fetch it again; affected snapshot metadata fails closed until restored.
 
+With `refuse_unvouched_debs = true`, a `.deb` that no current snapshot
+vouches for, requested while a signed adoption is running for a suite on
+the same upstream host, waits for the host's signed adoptions to end and
+is looked up once more, so a package from an update being adopted is
+served rather than refused (`acu_strict_adoption_waits_total{host}` counts
+the requests that waited). The wait lasts at most 15 seconds, and never
+past 25 seconds after the request arrived, so that a miss still has time
+to download the package before apt's 30-second timeout; a request with
+less than a second of that left, or arriving while 64 requests of its host
+already wait, is refused at once. Each request waits on its own: requests
+for the same package each count toward the 64, and a client fetching
+several unvouched packages one after another while a long adoption runs
+waits up to 15 seconds for each. An adoption ends after its flip barrier
+(up to 140 seconds for a busy suite; see *Flip consistency*), so a package
+whose snapshot is still in its barrier is likely to wait the full 15
+seconds and be refused. Refusals also remain when an adoption takes longer
+than that (large `dists/` suites), when a first adoption is still waiting
+for a slot, and for packages of an unsigned repository on a host
+whose signed suites all have complete coverage: unsigned snapshots never
+vouch and never count, so a host whose only snapshots are unsigned is
+never strict. Strict posture changes as eager first adoptions land after
+an upgrade. Conversely, a signed snapshot with incomplete coverage turns
+strict mode off for its whole host.
+
 ## `[gc]`: garbage collection
 
```

- [ ] **Step 2: Events and metrics (`docs/log-fields.md`)**

The metrics paragraph gains `acu_repair_deferred_total{host}` and `acu_strict_adoption_waits_total{host}` and widens `acu_freshness_unlogged_panics_total` (B-18); the `freshness_check` results gain `first_adoption`, `first_adoption_queued`, `form_changed` and `anchor_absent`, and dormant suites are described; `freshness check failed` absorbs the unexpected-status line (R-E3), whose own row goes; the refusal states, the heartbeat's stack (A-37), the shutdown-cut record write (B-20), `clock_step_detected`'s readers (B-38, B-23, D-53), the barrier's `refused` outcome (R-E2), the anchor read-back fault (D-38), the backstop's bound (D-37) and onboarding's shared deadline (D-34, D-35) are updated; two new WARN rows; the startup line names `adopt_unsigned`.

```diff
diff --git a/docs/log-fields.md b/docs/log-fields.md
index 8e2aca9..4a47a63 100644
--- a/docs/log-fields.md
+++ b/docs/log-fields.md
@@ -194,8 +194,15 @@ had to clear itself) and the unlabelled gauge `acu_suites_seed_rejected`
 refusal; updated within a minute). The
 scheduler adds the unlabelled counter `acu_freshness_unlogged_panics_total`
-(scheduled checks whose panic could not be logged because reporting it
-panicked too: a bug, please report it).
+(recovered panics — of a scheduled check, an adoption, its failure record
+or the adoption heartbeat — that could not be logged because reporting
+them panicked too: a bug, please report it).
 `acu_adoption_duration_seconds` includes the barrier's wait (at most
-140 s).
+140 s). Eager adoption (spec C4, C10) adds `acu_repair_deferred_total{host}`
+(repairs and reconciles of an unchanged suite deferred because no adoption
+slot was free; they run on a later unchanged check) and
+`acu_strict_adoption_waits_total{host}` (unvouched `.deb` requests that
+waited — at most 15 s, and never past 25 s after the request arrived — for
+a signed adoption running on their host before strict mode decided; the
+wait is part of the request line's `duration_ms`).
 
 The `freshness_check` line's `result` gains `changed_deferred` (upstream
@@ -206,4 +213,26 @@ released; a client-refused change is picked up by the next tick, see
 *Capacity and fairness* in configuration.md).
 
+Eager adoption (spec C4) adds `first_adoption` (a suite without a snapshot
+found its anchor and starts its first adoption; `form` is the form found,
+and `upstream_status` is 200, the answer of the anchor the form was found
+by — the absent probes before it are not logged),
+`first_adoption_queued` (the same, waiting for an adoption slot),
+`form_changed` (an adopted suite starts adopting another anchor form: a
+detached suite whose upstream now serves `InRelease`, a suite whose
+current anchor stayed absent and whose upstream serves the other signed
+form, or an unsigned suite that gained a signature; `changed_queued` when
+no slot is free) and `anchor_absent` (the current snapshot's anchor is
+absent upstream: `upstream_status` is the absent answer — 401, 403, 404,
+410, or 200 for a body that is not that file — and `reason` is
+`anchor_absent`; the snapshot keeps serving; also a detached suite whose
+changed `Release` comes without its `Release.gpg`, `upstream_status` then
+being the `Release.gpg`'s answer). `form` also takes the value `unsigned`.
+A suite without a snapshot that no client asked for in the last 7 days, or
+whose anchor files only the cache itself fetched, is dormant: its checks
+make no request and log no `freshness_check` line. Neither do the checks
+a failure or refusal record paces: those of a suite without a snapshot
+whose record is not due yet, within `periodic_refresh` of its last check,
+and those of an adopted suite in `anchor_absent` before its retry time.
+
 ## Stability guarantees
 
@@ -226,12 +255,12 @@ detection, the startup legacy trust audit, the flip barrier, suite rows
 | Event | Level | Fields | Meaning |
 |---|---|---|---|
-| `adoption_panicked` | ERROR | `where` (`adoption`, `repair`, `reconcile`, `heartbeat`, `record`), `canonical_host`, `suite_path`, `panic`, `stack` (first occurrence per `where` per process only; never for `where=heartbeat`) | A panic inside an adoption, repair, reconcile, heartbeat or failure-record goroutine was recovered; the prior snapshot keeps serving. Only an adoption panic is recorded as a failure (reason `panic`); repair and reconcile panics are logged, not recorded (plan deviation 7). `where=record` is `recordAdoptionFailure`, which writes the spec C7 failure record after an adoption ends. |
+| `adoption_panicked` | ERROR | `where` (`adoption`, `repair`, `reconcile`, `heartbeat`, `record`), `canonical_host`, `suite_path`, `panic`, `stack` (first occurrence per `where` per process only) | A panic inside an adoption, repair, reconcile, heartbeat or failure-record goroutine was recovered; the prior snapshot keeps serving. Only an adoption panic is recorded as a failure (reason `panic`); repair and reconcile panics are logged, not recorded (plan deviation 7). `where=record` is `recordAdoptionFailure`, which writes the spec C7 failure record after an adoption ends. |
 | `freshness_check_panicked` | ERROR | `where` (`check`, `scheduler`, `reschedule`, `t1`), `canonical_host`, `suite_path`, `panic`, `stack` (first occurrence per `where` per process only) | A panic during a freshness check was recovered. For `where=check` the check counts as failed (`last_check_at` bumped, so the cooldown applies) and also counts as an unknown end: `last_check_at` is bumped and `check_fail_count` extended. The `scheduler`, `reschedule` and `t1` recovers only log; `where=reschedule` guards the start-time retry spread (`rescheduleFailingAdoptions`), and `where=t1` is the handler's client-triggered check goroutine (logged by `internal/handler`). |
-| `freshness check failed` | INFO | `canonical_host`, `suite_path`, `form`, `err`, `result` (always `failed`) | A check ended unknown (a transport error or error status from upstream, or the 2-minute check deadline at the host slot or the request); `last_check_at` is bumped and `check_fail_count` extended, so the scheduler backs off (2 min doubling to 1 h), and `acu_freshness_check_total{result="failed"}` is incremented. |
-| `freshness: unexpected success status` | ERROR | `status`, `canonical_host`, `suite_path`, `form` (detached checks only) | The fetch client returned a success status other than 200 or 304 (a contract violation). The suite row is bookkept like an unknown end (`last_check_at` bumped, `check_fail_count` extended, `last_success_at` kept), but no `freshness check failed` line is logged and `acu_freshness_check_total` is not incremented. |
+| `freshness check failed` | INFO | `canonical_host`, `suite_path`, `form` (`inline`, `detached`, `unsigned`, or `probe` for the probe tree of a suite without a snapshot), `err`, `result` (always `failed`) | A check ended unknown — a transport error, an error status that is not an absent answer, a success status other than 200 or 304 (a fetch contract violation: `err` says `unexpected success status`), or the 2-minute check deadline at the host slot or the request — or found nothing it may adopt: a suite without a snapshot whose upstream has neither `InRelease` nor `Release` (which also records the refusal state `anchor_absent`, shown on the status page), or an unsigned `Release` while `adoption.adopt_unsigned` is off. A check whose probe found a signature but could not record the suite as seen signed fails too (`err` starts `mark the signature seen:`). `last_check_at` is bumped and `check_fail_count` extended, so the scheduler backs off (2 min doubling to 1 h), and `acu_freshness_check_total{result="failed"}` is incremented. With adoption enabled, 401, 403, 404, 410 and a 200 whose body is not the file asked for are answers, not failures (the anchor is absent): they never extend the streak. |
 | `adoption_run_failed` | WARN; INFO when the attempt was refused (outcome `refused`) | `canonical_host`, `suite_path`, `form`, `err` | Backstop line for every adoption attempt that returned an error, so a failure with no categorised line of its own is still visible. A refusal is a state, not a failure, so its line is INFO (`adoption_refused` carries the reason and timing); operators alerting on WARN are not paged for a flapping mirror. |
 | `adoption: read prior snapshot failed` | WARN | `canonical_host`, `suite_path`, `err` | The adoption could not read the suite's current (prior) snapshot before running Date and rollback checks against it; the attempt fails as `db_failed` and is retried on the normal backoff schedule rather than skipping those checks. |
-| `adoption_failure_record_failed` | WARN; DEBUG only for the read that precedes the write, when it finds the suite already removed (a routine reap during adoption, not a fault) — the write-failure site below has no DEBUG case | `canonical_host`, `suite_path`, `err` | The failure/refusal record could not be read or written; the next check may retry sooner than scheduled. |
-| `adoption_refused` | INFO | `canonical_host`, `suite_path`, `reason`, `refusal`, `fail_count`, `retry_in_seconds`, `next_retry_unixtime` | One attempt ended in a refusal state (`upstream_older`, `rollback_refused`), not a failure; logged for every refused attempt. |
+| `adoption: signed-anchor re-mark failed` | WARN; ERROR when a cached anchor's pool file could not be read (a missing file is not an error) | `canonical_host`, `suite_path`, `err` | Before an unsigned adoption's commit, the adopter re-reads the suite's cached `InRelease` and `Release.gpg` (and those of its `/.` twin) to record any signed one (spec C5 safeguard 3), and could not read a `url_path` row or a pool file, or write the record. The adoption is refused `unsigned_downgrade_refused` (fail closed): here that refusal means the cache could not rule out a signature, not that it saw one. It is retried on the refusal's schedule. |
+| `adoption_failure_record_failed` | WARN; DEBUG for the read that precedes the write when it finds the suite already removed (a routine reap during adoption, not a fault), and for a write that a shutdown cut short (nothing is recorded at shutdown) | `canonical_host`, `suite_path`, `err` | The failure/refusal record could not be read or written; the next check may retry sooner than scheduled. |
+| `adoption_refused` | INFO | `canonical_host`, `suite_path`, `reason`, `refusal`, `fail_count`, `retry_in_seconds`, `next_retry_unixtime` | One attempt ended in a refusal state, not a failure; logged for every refused attempt. The states: `upstream_older`, `rollback_refused`, `unsigned_downgrade_refused` (the suite was seen signed: decided by the check before a slot is taken, again before any download, and inside the commit), `unsigned_too_large` (decided by the check before a slot is taken, and again before any download), `unsigned_unconfirmed` (an unsigned repository waiting for its second observation) and `anchor_absent` (the current anchor is absent upstream, a detached suite's changed `Release` has no `Release.gpg`, or a suite without a snapshot has neither anchor). |
 | `adoption_deferred` | INFO | as `adoption_refused` | A suite entered the failing or refused state, or its reason changed (not logged on every attempt); the same content is retried at `next_retry_unixtime`, while new content is tried at once. |
 | `adoption_failing_persistently` | WARN | `canonical_host`, `suite_path`, `reason`, `fail_count`, `failing_since_unixtime` | The suite's adoption has failed 3, 6, 12, 24, … times in a row. Refusals never reach this (spec C7); see `adoption_refused_persistently`. |
@@ -242,13 +271,16 @@ detection, the startup legacy trust audit, the flip barrier, suite rows
 | `freshness: clamp adoption retry failed` | WARN | `canonical_host`, `suite_path`, `err` | A retry time left in the future by a backward clock step could not be clamped; the next check tries again. |
 | `freshness: clear adoption failure failed` | WARN | `canonical_host`, `suite_path`, `err` | Upstream serves the current content again but the failure record could not be cleared; the next check tries again. |
+| `freshness: adopted suites stale beyond threshold` | WARN | `adopted_stale`, `stale_factor`, `refresh` | Logged at each scheduler tick while adopted suites have had no successful check for `stale_factor` × `periodic_refresh` (4 × 15 min by default), or carry a success stamp in the future: the signature of a freeze. Suites in a refusal state are left out of `adopted_stale` (an `anchor_absent` suite keeps its last success old on purpose; the status page and `adoption_refused` show them). The gauge `acu_freshness_stale_suites{adopted}` counts every stale suite, those included. With adoption disabled, records are inert and every stale adopted suite counts. |
 | `freshness: read suite row failed` | WARN | `err`, `canonical_host`, `suite_path` | A freshness check could not read the suite's row: either the existence read a check makes before it takes the suite's lock (a suite is checked only while it has a row), or the full read after it. The check is skipped and writes nothing; the next trigger (a client hit's check or the scheduler) tries again. |
+| `freshness: read anchor rows failed` | WARN | `err`, `canonical_host`, `suite_path` | A check could not read the suite's cached anchor rows (`url_path`). A suite without a snapshot is not probed this time — no request, nothing written; the next check tries again. A suite whose current anchor is absent probes the other forms at their canonical URLs instead. |
 | `freshness: read suite row after a panic failed` | WARN | `err`, `canonical_host`, `suite_path` | After a recovered check panic, the best-effort bump of `last_check_at`/`check_fail_count` could not even read the suite's row; the row is left as it was. |
 | `freshness: persist failure bump after a panic failed` | WARN | `err`, `canonical_host`, `suite_path` | After a recovered check panic, `last_check_at` and `check_fail_count` were bumped in memory but the write failed; the row is left as it was. |
-| `clock_step_detected` | WARN | `stamp`, `canonical_scheme`, `canonical_host`, `suite_path`, `stored_unixtime`, `now_unixtime` | A stored timestamp is more than 2 s in the future (the wall clock stepped backwards); the gate treated it as old. Rate-limited to one line per minute. For `stamp=adoption_next_retry_at` the stamp is not treated as old: it is clamped to now + 1 h. For `stamp=last_requested_at` (a suite's client-interest stamp, read when the suite cap looks for an idle suite to evict) the suite is not evicted: a future stamp never counts as idle. `acu_clock_steps_detected_total` counts detections, not distinct clock steps or distinct stamps: the same future stamp is detected again every time it is read — every scheduler tick for a future `last_success_at`, every check for a future `last_check_at`, and twice when a first-ever check falls back from inline to detached — so alert on its rate, not its value. |
+| `clock_step_detected` | WARN | `stamp`, `canonical_scheme`, `canonical_host`, `suite_path`, `stored_unixtime`, `now_unixtime` | A stored timestamp is more than 2 s in the future (the wall clock stepped backwards); the gate treated it as old. Rate-limited to one line per minute. For `stamp=adoption_next_retry_at` the stamp is not treated as old: it is clamped to now + 1 h. For `stamp=last_requested_at` (a suite's client-interest stamp, read when the suite cap looks for an idle suite to evict, and by the check of a suite without a snapshot) the suite is not evicted — a future stamp never counts as idle — and the check counts a stamp up to 7 days ahead as recent interest; further ahead, the suite is dormant until a client asks for it again. `acu_clock_steps_detected_total` counts detections, not distinct clock steps or distinct stamps: the same future stamp is detected again every time it is read — every check for a future `last_check_at` or `last_success_at` (the scheduler reads a future `last_success_at` without reporting it), every check of a suite without a snapshot for a future `last_requested_at`, and every creation attempt within eight rows of the global cap for the future `last_requested_at` its eviction scan re-reads (the line stays rate-limited) — so alert on its rate, not its value. |
 | `legacy_snapshot_quarantined` | WARN | `reason` (`legacy_plaintext`, `malformed_envelope`, `unsigned_declarations`, `unparseable`, `anchor_missing`, `anchor_oversized`, `anchor_corrupt`, `unexpected_form`), `snapshot_id`, `canonical_host`, `suite_path`, `anchor_form`, `was_current` | The startup trust audit could not prove a pre-upgrade snapshot signed and clean; it no longer serves or vouches, and its suite is served unadopted until it re-adopts. |
 | `legacy_trust_audit_deferred` | ERROR | `snapshot_id`, `canonical_host`, `suite_path`, `err`, `stack` (present only for a recovered panic) | A pre-upgrade snapshot's pool file could not be read (not "missing"), or auditing it panicked (`err` carries the panic value); the snapshot is left as it was and retried at the next start. |
 | `legacy_trust_audit_progress` | INFO | `audited`, `quarantined`, `deferred` | One batch of up to 100 queued snapshots was audited and committed; the fields are running totals across this start's batches. Logged on the first start after an upgrade, and on later starts while deferred snapshots remain queued or after an interrupted audit. |
 | `legacy_trust_audit_completed` | INFO | `audited`, `signed`, `quarantined`, `deferred`, `anchor_markers`, `duration` | The startup trust audit finished (every start; `audited` is 0 once the upgrade queue has drained). |
-| `adoption_flip_barrier` | INFO | `canonical_host`, `suite_path`, `snapshot_id`, `outcome` (`held`, `quiet_cut_short`, `hold_expired`, `hold_skipped`, `not_needed`, `commit_failed`), `phase_a_seconds`, `token_wait_seconds`, `hold_seconds`, `held_requests` (the last four absent when the barrier was skipped for a first adoption) | An adoption committed behind the spec C8 flip barrier: `held` within its hold, once its suite was quiet; `quiet_cut_short` within its hold at 15 s, before its suite was quiet — the held requests got the new snapshot, a client still reading the old anchor then (or done with it within `flip_quiet_period`) may straddle (`acu_flip_quiet_cut_short_total`); `hold_expired` when the hold reached 20 s before the commit finished — the held requests were answered from the old snapshot (`acu_flip_hold_expired_total`); `hold_skipped` after 60 s without the hold token (`acu_flip_hold_skipped_total`); `not_needed` for a first adoption whose anchor clients were already being served; `commit_failed` when the commit ran and failed — the snapshot did not switch, the timing fields still describe the barrier, and `adoption_run_failed` gives the error. Not logged when `adoption.flip_quiet_period = 0s`, nor for a flip abandoned at shutdown (`adoption_abandoned_at_shutdown` says so). |
+| `adoption_flip_barrier` | INFO | `canonical_host`, `suite_path`, `snapshot_id`, `outcome` (`held`, `quiet_cut_short`, `hold_expired`, `hold_skipped`, `not_needed`, `commit_failed`, `refused`), `phase_a_seconds`, `token_wait_seconds`, `hold_seconds`, `held_requests` (the last four absent when the barrier was skipped for a first adoption) | An adoption committed behind the spec C8 flip barrier: `held` within its hold, once its suite was quiet; `quiet_cut_short` within its hold at 15 s, before its suite was quiet — the held requests got the new snapshot, a client still reading the old anchor then (or done with it within `flip_quiet_period`) may straddle (`acu_flip_quiet_cut_short_total`); `hold_expired` when the hold reached 20 s before the commit finished — the held requests were answered from the old snapshot (`acu_flip_hold_expired_total`); `hold_skipped` after 60 s without the hold token (`acu_flip_hold_skipped_total`); `not_needed` for a first adoption whose anchor clients were already being served; `commit_failed` when the commit ran and failed — the snapshot did not switch, the timing fields still describe the barrier, and `adoption_run_failed` gives the error; `refused` when the commit refused an unsigned candidate because the suite has signed history (spec C5's recheck inside the commit) — the snapshot did not switch, and `adoption_refused` gives the reason, `unsigned_downgrade_refused`. Not logged when `adoption.flip_quiet_period = 0s`, nor for a flip abandoned at shutdown (`adoption_abandoned_at_shutdown` says so). |
+| `adoption: served anchor read failed; the flip barrier runs` | WARN | `canonical_host`, `suite_path`, `path`, `err` | Before a first adoption's commit, the adopter could not read the anchor the cache serves for the suite (when clients already get the anchor being adopted, the barrier is not needed); the barrier runs — the safe direction. A missing row is not logged. |
 | `adoption_abandoned_at_shutdown` | INFO | `canonical_host`, `suite_path`, `form`, `err` | An adoption was abandoned at shutdown — waiting in the flip barrier (or about to skip it) when the daemon closed the barrier at the signal, or in any step before its commit once the daemon's lifetime context ended: nothing was committed or recorded, the change is adopted after the restart. Not a failure: no `adoption_run_failed`, not counted in `acu_adoption_total` or `acu_adoption_duration_seconds`, not shown in the admin page's recent adoptions. |
 | `displaced snapshot lookup failed` | WARN | `err`, `canonical_host`, `path` | Retained by-hash could not read the suite's displaced snapshots; the request is answered 404, as before spec C8. |
@@ -260,10 +292,10 @@ detection, the startup legacy trust audit, the flip barrier, suite rows
 | `suite cap usage read failed` | WARN | `err` | The startup check behind `suite_caps_exceeded` could not count the suites; the caps apply either way. |
 | `handler_goroutine_panicked` | ERROR | `where` (`touch`, `seed_rejections`), `canonical_host` and `path` (`touch` only), `panic`, `stack` (first occurrence per `where` per process only) | A panic in one of the handler's background goroutines — the touch goroutine, or the cap-rejection cache's sweep timer — was recovered (a bug, please report it). |
-| `suite onboarding failed` | WARN | `err`, `canonical_host`, `suite_path` | A cache hit on an untracked suite's anchor could not create its row (including the 5 s bound on that write); the anchor is served and the next hit retries. |
+| `suite onboarding failed` | WARN | `err`, `canonical_host`, `suite_path` | A cache hit on an untracked suite's anchor could not create its row (including the 5 s bound it shares with the backstop that may follow, and a shutdown that cut it short); the anchor is served and the next hit retries. |
 | `suite signed-seen mark failed` | WARN | `err`, `canonical_host`, `suite_path` | A suite's row was created, and one of its cached signed anchors changed between the handler's read and the creating write (or the handler could not read it: `suite cached anchor read failed`), so it was marked afterwards — and that write failed, or ran out of its 5 s bound. The startup pass marks it at the next start. (Every other mark is part of the write that creates the row or stores the anchor.) |
-| `suite file head read failed` | WARN | `err`, `canonical_host`, `suite_path`, `path` | A suite file just fetched on a miss could not be read back from the cache's pool for its content check (a local disk fault). For a signed anchor (`InRelease`, `Release.gpg`) the miss fails — 502, no body, nothing stored — because the anchor must not be served before its suite is marked as signed; apt retries. Any other suite file (`Release`, a flat index) is stored and served, but creates no suite row and re-stamps none. |
+| `suite file head read failed` | WARN | `err`, `canonical_host`, `suite_path`, `path` | A suite file just fetched on a miss could not be read back from the cache's pool for its content check (a local disk fault). For a signed anchor (`InRelease`, `Release.gpg`) the miss fails — 502 with `Retry-After`, request outcome `cache_write_failed` (and ERROR `cache write failed`), no body, nothing stored — because the anchor must not be served before its suite is marked as signed; apt retries. Any other suite file (`Release`, a flat index) is stored and served, but creates no suite row and re-stamps none. |
 | `suite cached anchor read failed` | WARN | `err`, `canonical_host`, `suite_path`, `path` | Creating a suite's row, the handler could not read one of the suite's cached signed anchors (its `url_path` row or its pool file; an absent row is not logged). The row is created anyway, and the anchor is read again and marked right after the write, before the response — also when the client left, or the request is answered from a new snapshot, while it waited for a flip (`suite signed-seen backstop read failed` if that fails too). Between the write and that mark the suite is tracked with the anchor unmarked; if the mark fails, it stays so until the startup pass at the next start. |
-| `suite signed-seen backstop read failed` | WARN | `err`, `canonical_host`, `suite_path`, `path` | After creating a suite's row, the handler's second read of a cached signed anchor (see `suite cached anchor read failed`; also run when the anchor changed during the creation) failed — its `url_path` row or its pool file, including the 5 s bound on that step; an absent row is not logged. The response is served; the anchor stays unmarked until the startup pass marks it at the next start. |
+| `suite signed-seen backstop read failed` | WARN | `err`, `canonical_host`, `suite_path`, `path` | After creating a suite's row, the handler's second read of a cached signed anchor (see `suite cached anchor read failed`; also run when the anchor changed during the creation) failed — its `url_path` row (within the backstop's 5 s bound, which covers its database steps; after onboarding the bound is shared with the creation, and on a miss it ends at most 25 s after the request arrived) or its pool file (a local read of a few KiB, not bounded); an absent row is not logged. The response is served; the anchor stays unmarked until the startup pass marks it at the next start. |
 | `snapshot_member_blob_missing` | WARN | `canonical_host`, `path`, `snapshot_id`, `blob_hash`, `snapshot_displaced` (present for a displaced snapshot's member) | A snapshot member's pool file is gone: a current member is re-fetched and checked against its declared hash; a displaced one (retained by-hash) answers 404. |
 | `shutdown wait panicked` | ERROR | `panic`, `stack` | The shutdown drain's wait for the adoption, repair and reconcile goroutines panicked (a bug, please report it); shutdown carries on without waiting further for them. |
-| `apt-cacher-ultra starting` | INFO | the effective configuration (see `cmd/apt-cacher-ultra/main.go`); the flat-repository work adds `adoption_flip_quiet_period` and `freshness_max_suites` | Logged once at start. `adoption_flip_quiet_period` is the configured `adoption.flip_quiet_period`; the flip barrier runs only when it is not `0s` and adoption is enabled. `freshness_max_suites` is the configured `freshness.max_suites`. |
+| `apt-cacher-ultra starting` | INFO | the effective configuration (see `cmd/apt-cacher-ultra/main.go`); the flat-repository work adds `adoption_flip_quiet_period` and `freshness_max_suites` | Logged once at start. `adoption_flip_quiet_period` is the configured `adoption.flip_quiet_period`; the flip barrier runs only when it is not `0s` and adoption is enabled. `freshness_max_suites` is the configured `freshness.max_suites`. With adoption enabled, the `phase2 adoption enabled` line that follows lists the adoption settings, `adopt_unsigned` among them. |
```

- [ ] **Step 3: Admin page (`docs/admin-ui-spec.md`)**

§4.5: `anchor_form` takes `unsigned`; `check_fail_count`'s meaning follows R-E4 and design note 19; `adoption_refused` lists all six states; a paragraph on the four phase E refusals; B-67's rewording of the `adoption_refused_persistently` sentence.

```diff
diff --git a/docs/admin-ui-spec.md b/docs/admin-ui-spec.md
index 43053b6..f23cbda 100644
--- a/docs/admin-ui-spec.md
+++ b/docs/admin-ui-spec.md
@@ -348,12 +348,12 @@ next to the Suites table contract in §5.3:
 | Field | Type | Meaning |
 |---|---|---|
-| `anchor_form` | string | The current snapshot's signing form — `inline` or `detached` today (`unsigned` joins in a later phase); empty when the suite has no current snapshot. |
+| `anchor_form` | string | The current snapshot's anchor form — `inline`, `detached` or `unsigned` (a `Release` no signature covers: it vouches no `.deb`); empty when the suite has no current snapshot. |
 | `last_requested_unixtime` | int64 or `null` | When a client last showed interest in the suite (spec C3): written when the suite's row is created, then by coalesced touches — at most one write per suite per hour — from serves of its anchors (`InRelease`, `Release`, `Release.gpg`; status below 400, 304 and HEAD included), of its flat indexes, and of `.deb`s its current snapshot vouched for. Index requests such as `Packages.gz` never move it. It decides cap eviction: at the `freshness.max_suites` cap, a suite with no current snapshot whose stamp is 24 hours old or more can be evicted for a new one (`suite_evicted`). |
-| `check_fail_count` | int64 | Consecutive checks that ended unknown (transport error, deadline, unexpected status) since the last answered check; drives the scheduler's healthy → never-checked → failing ordering and its 2 min → 1 h backoff. Resets to 0 on every answered check. |
+| `check_fail_count` | int64 | Consecutive failed checks since the last answered check: ended unknown (transport error, deadline, unexpected status), or found nothing they may adopt (a suite without a snapshot whose upstream has neither `InRelease` nor `Release`, which also records `anchor_absent`; an unsigned `Release` while `adoption.adopt_unsigned` is off; a signature the check found but could not record as seen). Drives the scheduler's healthy → never-checked → failing ordering and its 2 min → 1 h backoff. Resets to 0 on every answered check, an absent current anchor (`anchor_absent`) included. |
 | `adoption_fail_reason` | string or `null` | The current failure or refusal reason; `null` when no record exists. |
 | `adoption_fail_count` | int64 | Consecutive adoption attempts in the current failure/refusal streak; `0` when no record exists. |
 | `adoption_fail_first_unixtime` | int64 or `null` | When the current streak started. |
 | `adoption_next_retry_unixtime` | int64 or `null` | When the same content is retried next; new content is tried at once regardless of this stamp. |
-| `adoption_refused` | boolean | `true` when the record above is a refusal state (`upstream_older`, `rollback_refused`, …), not a failure. |
+| `adoption_refused` | boolean | `true` when the record above is a refusal state (`upstream_older`, `rollback_refused`, `unsigned_downgrade_refused`, `unsigned_unconfirmed`, `unsigned_too_large`, `anchor_absent`), not a failure. |
 
 These are additive to the locked JSON contract (§0.1), the same way
@@ -370,4 +370,24 @@ refusal record. While adoption is disabled, the text gains the suffix
 and is inert now.
 
+**Unsigned and absent-anchor refusals (phase E).** `unsigned_unconfirmed`
+is an unsigned repository waiting for its second observation
+(`max(cooldown, 5 min)` after the first, an hour when the confirming check
+sees a 401/403 on a signature file) and clears itself when it adopts;
+`unsigned_downgrade_refused` (the suite was seen signed) lasts until
+upstream serves a signature the cache adopts, or the suite is forgotten
+(`POST /forget`, added with suite reaping in a later release);
+`unsigned_too_large` (more than 1,000 declared files) lasts until
+upstream's `Release` declares fewer; `anchor_absent` means the current
+snapshot's anchor is gone upstream (or a detached suite's changed
+`Release` has no `Release.gpg`) while the snapshot keeps serving, or, for
+a suite without a snapshot, that upstream serves neither `InRelease` nor
+`Release`. None of them hides lag (only `upstream_older` and
+`rollback_refused` do, §9.1), and all four follow the `REFUSED 24H+` rule
+below. A suite without a snapshot shows lag from the first check that
+found its anchor, so a first adoption refused for good
+(`unsigned_downgrade_refused`, or an `unsigned_unconfirmed` that never
+confirms) shows a lag that keeps growing: the suite has in fact never been
+adopted.
+
 **FAILING count.** The Suites panel's eyebrow and the SUITES vital cell's
 sub-line count suites with a genuine failure record (`adoption_refused ==
@@ -383,6 +403,9 @@ silent freeze — a vendor that stops emitting `Date` is refused
 warns (`data-state="warn"`), the SUITES vital warns (never crit), and the
 eyebrow and the vital's sub-line add `{n} REFUSED 24H+` (only when > 0).
-Below 24 h nothing changes. The checker logs WARN
-`adoption_refused_persistently` for the same condition.
+Below 24 h nothing changes. The checker also logs WARN
+`adoption_refused_persistently`, but only at a streak milestone (the 3rd,
+6th, 12th, 24th, 48th … attempt) reached once the streak is 24 h old:
+with hourly retries the first comes at the 48th attempt, about 43 h in —
+about a day after the row starts warning.
 
 **Adoption disabled.** With adoption disabled (the shipped default) a
```

- [ ] **Step 4: Spec revision 8**

The Date line's revision entry; C4 (D-54's admission order; R-E4 and design note 21 under the classification; Dispatch 1 with deviation 1 and design note 22; Dispatch 2 with design note 24; Dispatch 3 with design notes 13–15 and 19); C5 (design notes 3–7 under *Hash blocks*; safeguard 2 with design notes 16–18 and deviation 2; safeguard 3 with R-E1, R-E2 and design note 12); C7 (the `anchor_absent` key, the probe tree's gate, the confirmation records' streaks); C10 (design note 27); C11 (the two metrics and the confirmation-gap constant); §7 (the upgrade burst with design note 26; the `/.` twin variant in G; R-E1's test).

```diff
diff --git a/docs/superpowers/specs/2026-09-27-flat-repositories-design.md b/docs/superpowers/specs/2026-09-27-flat-repositories-design.md
index 9be6f46..8911bcd 100644
--- a/docs/superpowers/specs/2026-09-27-flat-repositories-design.md
+++ b/docs/superpowers/specs/2026-09-27-flat-repositories-design.md
@@ -1,5 +1,5 @@
 # Flat repositories, eager adoption, and suite lifecycle — design
 
-- **Date:** 2026-09-27 (revision 7, 2026-10-18: phase D plan rulings — adoption accepts exactly apt's first `InRelease` line, and a marker line with trailing blanks decodes; a binary `Release.gpg` passes its sniff only with a signature version (3–6) where its packet header puts the body; suite paths over 1024 bytes are not tracked; cap eviction holds the evictee's mutex through the checker, tries at most eight candidates, evicts at most one suite per creation attempt at or above the global cap (an attempt above it is still refused) and deletes through the compare-and-delete the reaper reuses, and a startup WARN names a table over its caps; the rejection cache holds at most 4,096 suites; `suite_seed_rejected` is rate-limited to one line a minute; the C3 metric names; the hit-path invariant covers tracked suites, and every row creation marks the suite's cached signed anchors in its own transaction; with no adopter a refresh miss re-stamps its row as before; the T1 on a creating miss lands with phase E; an attempt a shutdown ends before the barrier is abandoned like one in it; revision 6, 2026-10-18: phase C plan rulings — the C8 barrier keeps the adoption's global slot and does not honour `adoption.max_duration` but ends on shutdown (the daemon closes it at the signal), so the admission bound's *A* includes it; a hold that held no request starts no cross-suite quiet period; `flip_quiet_period` is `0s` or `1s` to `15s`; every C8 residual has its own counter; the hold-token residual and the five-suite DoD corrected (four hold, the fifth commits unheld); retained by-hash serves SHA512 aliases from phase C; exact C2 predicate and C3 `Release.gpg` sniff wording; after phase C's final review: once cross-suite quiet holds, a hold commits at 15 s (20 s less a 5 s commit margin) instead of answering its waiters from the old snapshot at 20 s — its late readers are a counted residual — and snapshot anchors carry `adopted_at` alone as `Last-Modified`; revision 5, 2026-10-17: slot-driven admission in C4 — a suite that finds no slot waits and is re-checked when one is released; the admission bound — after the phase B plan review; revision 4, 2026-10-05/11: legacy trust audit, signed-seen ordering, fail-closed inline verification, flip hold token, fair scheduler — after the 2026-10-05 plan review, a follow-up review, and a three-lens review of grounding, alternatives and residual risk; 2026-10-17: PR #8 parity notes in C10, §7 and §9)
+- **Date:** 2026-09-27 (revision 8, 2026-10-19: phase E plan rulings — before an unsigned commit the adopter re-marks the suite's cached signed anchors and refuses when it cannot; a refusal inside the commit is a refusal state with its own sentinel and the barrier outcome `refused`; one check tail, and an unexpected success status is a failed check; absent answers reset `check_fail_count`; `anchor_absent` is recorded at the first absent observation, and a future `adoption_fail_first_at` never confirms an unsigned transition; the probe tree's content-key gate, the confirmation details, the interest and client-created-row rules, a 304 only to a conditional request, SHA512 verification before the pool; strict mode waits for signed adoptions only; the scheduler bound's admission order; after the phase E plan reviews (rulings PE-1–PE-8): a probe that finds a signature marks the suite's base, and the re-mark also reads the `/.` twin's rows; never-downgrade reads the markers of both schemes and also refuses while an adopted, non-retired signed snapshot of the base exists; a deferred never-adopted suite and an adopted `anchor_absent` suite are paced by their record; a never-adopted suite whose tree finds no anchor also records `anchor_absent`, and so does a detached suite whose changed `Release` has no signature; a cross-form request records no validators; the interest window is symmetric; the unsigned content key is domain-separated; the unsigned member cap is applied before a slot is taken; strict mode's wait, and the miss path's backstop, end at most 25 s after the request started; the probe bound counts client checks and lost offers; revision 7, 2026-10-18: phase D plan rulings — adoption accepts exactly apt's first `InRelease` line, and a marker line with trailing blanks decodes; a binary `Release.gpg` passes its sniff only with a signature version (3–6) where its packet header puts the body; suite paths over 1024 bytes are not tracked; cap eviction holds the evictee's mutex through the checker, tries at most eight candidates, evicts at most one suite per creation attempt at or above the global cap (an attempt above it is still refused) and deletes through the compare-and-delete the reaper reuses, and a startup WARN names a table over its caps; the rejection cache holds at most 4,096 suites; `suite_seed_rejected` is rate-limited to one line a minute; the C3 metric names; the hit-path invariant covers tracked suites, and every row creation marks the suite's cached signed anchors in its own transaction; with no adopter a refresh miss re-stamps its row as before; the T1 on a creating miss lands with phase E; an attempt a shutdown ends before the barrier is abandoned like one in it; revision 6, 2026-10-18: phase C plan rulings — the C8 barrier keeps the adoption's global slot and does not honour `adoption.max_duration` but ends on shutdown (the daemon closes it at the signal), so the admission bound's *A* includes it; a hold that held no request starts no cross-suite quiet period; `flip_quiet_period` is `0s` or `1s` to `15s`; every C8 residual has its own counter; the hold-token residual and the five-suite DoD corrected (four hold, the fifth commits unheld); retained by-hash serves SHA512 aliases from phase C; exact C2 predicate and C3 `Release.gpg` sniff wording; after phase C's final review: once cross-suite quiet holds, a hold commits at 15 s (20 s less a 5 s commit margin) instead of answering its waiters from the old snapshot at 20 s — its late readers are a counted residual — and snapshot anchors carry `adopted_at` alone as `Last-Modified`; revision 5, 2026-10-17: slot-driven admission in C4 — a suite that finds no slot waits and is re-checked when one is released; the admission bound — after the phase B plan review; revision 4, 2026-10-05/11: legacy trust audit, signed-seen ordering, fail-closed inline verification, flip hold token, fair scheduler — after the 2026-10-05 plan review, a follow-up review, and a three-lens review of grounding, alternatives and residual risk; 2026-10-17: PR #8 parity notes in C10, §7 and §9)
 - **Status:** draft for maintainer review
 - **Branch:** `feature/flat-repos` (from `main` @ 536a1ab)
@@ -27,5 +27,5 @@ fixes the robustness problems flat support multiplies:
 | C1 Request canonicalisation | One path-canonicality rule; upstream URLs encoded exactly as apt encodes them. |
 | C2 Suite identity | Whole-directory flat suites; tightened `dists/` grammar; a leaf path package. |
-| C3 Suite rows and signals | One creation choke point with caps; client-interest stamp; signed-seen marker (written at adoption, at startup from cached anchors, and in the miss path's writer operation before a signed anchor is served). |
+| C3 Suite rows and signals | One creation choke point with caps; client-interest stamp; signed-seen marker (written at adoption, at startup from cached anchors, in the miss path's writer operation before a signed anchor is served, and by the checker when a probe finds a signature). |
 | C4 Freshness check | Probe tree in apt's order; eager first adoption; form transitions; anchorless refresh; adoption capacity; fair concurrent scheduler with a latency bound; clock rules. |
 | C5 Adoption input | Flat IndexTargets; `./` and `%` Filenames; SHA512 hash blocks; apt's `Date` rules; R-1 fix; I-1 panic recovery; inline verification accepts only decodable clearsigned envelopes; unsigned form. |
@@ -435,5 +435,14 @@ that have a `suite_freshness` row. Writers:
   by one goroutine (`cache.submitWrite`), so an unsigned `CommitAdoption`
   enqueued after a marker write runs after it, and its in-transaction
-  re-read (C5) sees the marker.
+  re-read (C5) sees the marker;
+- the freshness checker (phase E, ruling PE-5): when a probe finds a
+  sniff-passing `InRelease` or `Release.gpg` for a suite without a signed
+  current snapshot — a first adoption's probe tree, an unsigned suite's
+  upgrade probe, an absent anchor's tree — it marks the base
+  (`MarkSignedSeenBatch`, `last_seen_at` = the check's time) before the
+  check admits any request; a failed write fails the check. An unsigned
+  suite's clients never fetch a signature, so without this writer one only
+  the checker saw could be stripped later and the suite adopted unsigned;
+- the adopter, before an unsigned commit (C5 safeguard 3, ruling R-E1).
 
 A plain-text "InRelease" fails the sniff and never creates a marker. The
@@ -459,6 +468,9 @@ refuses or ignores connections is already collapsed by the fetch client's
 scheduler therefore keeps a **per-host queue of due suites**, refreshed every
 tick (newly due suites appended, queued ones not duplicated; at most
-`freshness.max_suites` entries in all), served by a fixed pool of **4 check
-workers** (constant):
+`freshness.max_suites` entries in all — when that bound cuts a tick's due
+list, the least recently checked suites are admitted first, never-checked
+ones before any and a future `last_check_at` counting as never, still
+queued in key order: phase D, ruling PD-11), served by a fixed pool of
+**4 check workers** (constant):
 
 - **across hosts:** at most one check in flight per canonical host; hosts
@@ -517,6 +529,10 @@ With the default pool of two and a
 first adoption holding the general slot, signed suites go through the
 reserved slot back to back. First adoptions (and unsigned ones) run only
-when the pool is otherwise empty, so a burst of them is serial;
-`max_concurrent_adoptions = 3` lets two run at once. An offered re-check
+when the pool is otherwise empty, so a burst of them is serial, and under
+sustained signed load they can wait a long time; `max_concurrent_adoptions
+= 3` lets two run at once. A queued suite is probed twice by the scheduler
+(the check that queued it and the offered re-check that admits it), plus
+once per client check while it waits (at most one per cooldown) and once
+per offer another admission takes first (phase E, ruling PE-4). An offered re-check
 uses its host's turn like any check, so *n* in the latency bound also counts
 the host's waiting suites offered before *S* (at most one per adoption on
@@ -532,5 +548,9 @@ that host).
 
 Counting 401/403/410 as *absent* is more lenient than apt's signed fallback
-(404 only); for signed forms it can only cause a lateral move.
+(404 only); for signed forms it can only cause a lateral move. An *absent*
+answer is not an *unknown* end: it resets `check_fail_count` (phase E,
+ruling R-E4). A 304 is *present* only as the answer to a conditional
+request (the current anchor, unchanged); to a request without validators
+it is *unknown* (phase E, design note 21).
 
 **Dispatch** (adopter wired; with no adopter the Phase 1 algorithm runs
@@ -540,24 +560,59 @@ unchanged, observation only):
    changed → C7 gate → adoption request.
    **Current snapshot detached** — also probe `InRelease` each check;
-   *present* → inline adoption request (lateral). Then the conditional GET of
-   `Release` as today.
-   **Current anchor absent** (inline: `InRelease`; detached: `Release`) on two
-   checks at least `max(cooldown, 5m)` apart → run the probe tree without
-   validators and adopt the form found: inline ⇄ detached is a lateral move;
-   never unsigned from signed. If no signed form is *present*, record the
-   refusal state `anchor_absent` (visible; `last_success_at` not bumped;
-   the snapshot keeps serving).
+   *present* → inline adoption request (lateral; lateral content whose
+   adoption failed keeps its record, which the detached anchor's
+   `unchanged` does not clear). Then the conditional GET of `Release` as
+   today; a changed `Release` whose `Release.gpg` is *absent* records
+   `anchor_absent` and is never adopted (phase E, design review M-14).
+   **Current anchor absent** (inline: `InRelease`; detached and unsigned:
+   `Release`) → the first absent observation records the refusal state
+   `anchor_absent` (visible at once; `last_success_at` not bumped;
+   `check_fail_count` reset; the snapshot keeps serving; content key
+   sha256("anchor_absent"), which no content matches). The record paces the
+   suite: while it is active and not due, a check makes no request and the
+   scheduler does not queue the suite — the anchor is asked for again at
+   the record's retry time, and client checks in between ask nothing
+   (phase E, ruling PE-2). Once the record is due and the absence has
+   lasted `max(cooldown, 5m)` — two observations, measured from
+   `adoption_fail_first_at`, which never confirms when it lies in the
+   future (deviation 2) — run the probe tree without validators and adopt
+   the signed form found: inline ⇄ detached is a lateral move, and an
+   unsigned suite may find a signed form; a signature found for a suite
+   without a signed current snapshot is marked first (C3). The current form
+   and content found again is `unchanged` and clears the record; an
+   unsigned suite's own form with new bytes is its update; for a signed
+   suite an unsigned candidate, or nothing, is recorded again — never
+   unsigned from signed. A record from an adoption attempt (a failed
+   lateral move) keeps its own C7 schedule (phase E, deviation 1).
 2. **Current snapshot unsigned** — probe `InRelease` and `Release.gpg`; if
-   either is *present* → signed adoption request (upgrade). Otherwise
+   either is *present* → mark the base (C3) and make a signed adoption
+   request (upgrade): from then on the suite is never adopted unsigned
+   (C5 safeguard 3), also after the signature goes away. Otherwise
    conditional GET `Release`; changed → unsigned adoption request (no
    re-confirmation). If a signature is present but the signed adoption
    cannot succeed (key not on the cache host), the suite keeps its last
    unsigned snapshot and the admin page shows the reason; the fix is
-   installing the key (or `adoption.accept_any_signer`).
+   installing the key (or `adoption.accept_any_signer`). A signature probe
+   that ends *unknown* fails the check, and a *present* signature whose
+   signed adoption is deferred (C7) is `changed_deferred`: an unsigned
+   suite is never updated past a signature (phase E, design note 24).
 3. **No current snapshot (eager first adoption)** — only if
    `last_requested_at` is within a constant **7-day interest window**
    (independent of GC settings) and the suite has a client-created anchor or
-   flat index `url_path` row (`request_count > 0`; never reconstructed).
-   Probe tree without validators:
+   flat index `url_path` row (`request_count > 0`; never reconstructed). The
+   window is symmetric: a `last_requested_at` up to 7 d in the future counts
+   as interest, one further ahead is dormant until a client touches the
+   suite (design review M-3). Of the anchor rows only `InRelease` and
+   `Release` count (apt fetches `Release.gpg` only after `Release`; phase
+   E, design notes 13 and 14). A returning client's first request may find
+   its suite still dormant (its touch and its T1 run concurrently); the
+   next tick checks it, within one tick of that request (M-12). A suite
+   whose record is active and not due is paced: while its last check is
+   younger than `periodic_refresh`, no probe runs, so client checks ask
+   nothing and new content is attempted within one refresh interval (phase
+   E, ruling PE-3). Otherwise the probe tree runs without validators (the
+   C7 gate compares content keys; phase E, design note 15), and a
+   *present* signature marks the base before any request is admitted
+   (C3):
    - `InRelease` *present* → inline request;
    - `InRelease` *absent* → `Release`: *present* → `Release.gpg`: *present* →
@@ -565,5 +620,8 @@ unchanged, observation only):
    - `Release` *absent* → for flat suites that have no signed-seen marker and
      no sniff-passing cached anchor row, the **anchorless refresh**; otherwise
-     a failed check;
+     a failed check, which extends `check_fail_count`: a suite with nothing
+     to serve backs off like an *unknown* end (phase E, design note 19), and
+     records the refusal state `anchor_absent`, so the status page shows it
+     (ruling PE-2);
    - any *unknown* → failed check.
 4. **Anchorless refresh** (flat, Release-less suites only): conditional GET
@@ -684,4 +742,10 @@ SHA256 makes coverage incomplete (reason `no_sha256`), never a silent drop.
 Limitation (documented): integrity-skipped SHA512-only members are not
 recorded for skip-repair; reconcile still heals requestable IndexTargets.
+SHA512 is verified before the bytes reach the pool; a SHA512-only member
+is never reused from the pool (which is keyed by SHA256); a pooled blob
+whose SHA256 matches but whose SHA512 does not fails the member; a
+malformed or conflicting SHA512 entry fails the parse; repair recovers
+the declared hashes from the snapshot's Release; the by-hash probe uses
+the strongest declared hash (phase E, design notes 3–7).
 
 **`Date` rules** (apt's). The verified Release text's `Date:` is parsed with a
@@ -763,18 +827,46 @@ upgrade (§6.1).
    either signature file answered 401/403 (WAF and S3-permission incidents);
    the first observation records reason `unsigned_unconfirmed` and the
-   comparison uses `adoption_fail_first_at` (restart-safe). Updates of an
-   established unsigned suite adopt at once.
+   comparison uses `adoption_fail_first_at` (restart-safe; the record's
+   streak restarts whenever the bytes or the reason change, so the stamp is
+   the first observation of these bytes). The hour applies when a
+   signature probe of the confirming observation answered 401/403; bytes
+   that already passed confirmation (a record for them from an adoption
+   attempt) are not confirmed again; a future `adoption_fail_first_at`
+   never confirms (phase E, design notes 16–18 and deviation 2). Updates
+   of an established unsigned suite adopt at once.
 3. **Never downgrade:** refused (`unsigned_downgrade_refused`, a refusal
-   state) when a `suite_signed_seen` marker exists for the suite's base. The
-   marker outlives reaping, covers the `./` twin, and is committed in the
-   writer operation that stores a signed anchor, before any client is served
-   it (C3). The check runs twice:
-   at eligibility, and again **inside the `CommitAdoption` writer
-   transaction**, which re-reads the marker for the candidate's base and
-   refuses an unsigned candidate (no pointer change, no rows) when one exists
-   or when the prior current snapshot is signed. The suite mutex alone cannot
+   state) when a `suite_signed_seen` marker exists for the suite's host and
+   base under either scheme (`http` or `https`: a base seen signed over
+   `https` gives the same base over plain `http` the same protection; phase
+   E, design review M-7), or when an adopted, non-retired inline or
+   detached snapshot of the base or its `/.` twin exists under either
+   scheme (M-8: also a signed displaced snapshot the audit left queued and
+   unmarked while it quarantined the current one). The marker outlives
+   reaping, covers the `./` twin, and is committed in the writer operation
+   that stores a signed anchor, before any client is served it (C3). One
+   rule, one function (`refuseUnsignedDowngrade`: a marker read and a
+   snapshot read) at every point it runs: at eligibility, and again
+   **inside the `CommitAdoption` writer transaction**, which refuses an
+   unsigned candidate (no pointer change, no rows) when it holds. The suite mutex alone cannot
    close this race: the handler and the `/.` twin establish signed history
-   without it.
-4. At most 1,000 declared members (`unsigned_too_large`).
+   without it. Before an unsigned commit the adopter also re-runs the
+   cached signed-anchor mark under the suite mutex: it reads the
+   `<suite>/InRelease` and `<suite>/Release.gpg` `url_path` rows of the
+   suite and of its `/.` twin (design review I-3), sniffs their pool files
+   outside the writer and marks the base for each that passes, before the
+   commit is enqueued (FIFO: the commit's re-read sees it); a failed mark,
+   or a pool read error other than a missing file, refuses the adoption
+   (phase E, ruling R-E1) and is logged (`adoption: signed-anchor re-mark
+   failed`, WARN; ERROR for a pool read error; M-1) — that refusal means
+   "could not rule out a signature". A refusal inside the
+   commit is the refusal state, not a failure: its own sentinel, no
+   `adoption: commit failed` WARN, no `db_failed`, `adoption_flip_barrier`
+   outcome `refused`, also after a barrier that outlived
+   `adoption.max_duration` (ruling R-E2). Eligibility is also checked by the
+   check, before a slot is taken.
+4. At most 1,000 declared members, counted as the distinct paths of the
+   merged SHA256 and SHA512 lists (`unsigned_too_large`); the check applies
+   it before a slot is taken, and the adoption again before any download
+   (phase E, design review M-5).
 5. No `package_hash` rows on **any** path (adoption, repair, reconcile:
    `buildPackageHashes` takes the form); `DeclaredHashesForPath` and
@@ -782,9 +874,13 @@ upgrade (§6.1).
 
 Threat notes (documented at the knob): the signed-seen marker cuts both
-ways — anyone who can make the cache serve one sniff-passing `InRelease`
-for a base (a MITM, a compromised upstream, or a LAN client pointing the
-cache at a host it controls) blocks unsigned adoption of that base until
-`POST /forget`; this denial of service is the accepted price of never
-downgrading. A MITM on the cache→upstream path can
+ways — anyone who can make the cache serve, or a check find (C3), one
+sniff-passing `InRelease` or `Release.gpg` for a base (a MITM, a
+compromised upstream, or a LAN client pointing the cache at a host it
+controls) blocks unsigned adoption of that base until `POST /forget`; this
+denial of service is the accepted price of never downgrading. A suite
+whose pre-v7 snapshot §6.1 quarantined for `anchor_missing`,
+`anchor_corrupt` or `anchor_oversized` may have no marker until a check
+sees its signature upstream; if its signatures are stripped before then,
+it adopts unsigned (phase E, design review M-9). A MITM on the cache→upstream path can
 make the cache adopt forged unsigned content only for a suite never seen
 signed, after two consistent observations; verifying clients reject it,
@@ -850,5 +946,8 @@ success.
 
 **Content key.** inline: sha256(InRelease); detached: sha256(sha256(Release)
-‖ sha256(Release.gpg)); unsigned: sha256(Release). Any byte change is new
+‖ sha256(Release.gpg)); unsigned: sha256("unsigned\n" ‖ Release), domain-separated
+from an inline key over the same bytes (phase E, design review M-2); an `anchor_absent` record:
+sha256("anchor_absent"), a constant no content matches (phase E). Any byte
+change is new
 content and is attempted at once (subject to slots). A re-signature alone of
 a detached suite is noticed at the next retry (≤ 1 h), because a 304 on
@@ -866,5 +965,7 @@ re-downloading everything.
 **Refusal states** use the same record and schedule but are not failures:
 `upstream_older`, `rollback_refused`, `unsigned_downgrade_refused`,
-`unsigned_unconfirmed` (confirmation timing from `adoption_fail_first_at`),
+`unsigned_unconfirmed` (confirmation timing from `adoption_fail_first_at`;
+its streak, like `anchor_absent`'s, restarts on new content or a new
+reason),
 `unsigned_too_large`, `anchor_absent`. They are excluded from the
 failing-suites gauge and from `adoption_failing_persistently`, and shown as
@@ -875,6 +976,18 @@ failing-suites gauge and from `adoption_failing_persistently`, and shown as
 304 or a 200 with the same key ⇒ `changed_deferred` (no clear, no repair);
 new content ⇒ attempt. At or after `next_retry_at` the check GETs without
-validators and attempts. The record is cleared in the flip transaction and
-when upstream again serves the current snapshot's content.
+validators and attempts. A record's validators are those of the anchor
+file its request fetched: a cross-form request (a detached suite's lateral
+`InRelease`, an unsigned suite's upgrade probe, an absent anchor's tree)
+records none, and while a record without validators is active and not due
+the check sends the current anchor's own; a 304 is `changed_deferred` only
+when the record's validators were sent, otherwise `not_modified` (phase E,
+design review I-1). The C4 probe tree sends no validators: there the gate
+compares content keys only (phase E, design note 15). Two records pace
+their suite instead of gating a request: an adopted suite's
+`anchor_absent` (no request before its retry time) and a never-adopted
+suite's active record (no probe before its retry time while its last check
+is younger than `periodic_refresh`) — C4 Dispatch 1 and 3, rulings PE-2
+and PE-3. The record is cleared in the flip transaction and when upstream
+again serves the current snapshot's content.
 
 **Restart.** At checker start every failing row (not refusal states, not
@@ -1093,15 +1206,28 @@ snapshots now normally have complete coverage, so strict mode protects flat
 hosts.
 
-**In-flight adoptions (D10).** The checker tracks in-flight adoptions per
-canonical host. On the hit path, before answering `unvouched_deb_refused`,
-the handler waits up to 15 s for in-flight adoptions on the request's host,
-then re-runs the lookup once. On the miss path, where the strict decision sits
+**In-flight adoptions (D10).** The checker tracks in-flight signed adoptions
+per canonical host (an unsigned adoption never vouches, so it is not waited
+for; a count ends when the adoption goroutine returns, after its commit:
+phase E, design note 27). On the hit path, before answering
+`unvouched_deb_refused`,
+the handler waits for in-flight adoptions on the request's host — at most
+min(15 s, 25 s − the request's age), and not at all when less than 1 s of
+that is left (phase E, design review M-10: the miss path still downloads
+the whole `.deb` before its first byte, within apt's 30 s) — then re-runs
+the lookup once. On the miss path, where the strict decision sits
 inside singleflight after host-slot acquisition by design, `runFetch` returns
 `errUnvouchedRetryable` (releasing its slot) when an adoption is in flight on
-the host; `serveCacheMiss`, outside singleflight, waits up to 15 s and retries
-the miss once. At most 64 waiters per host; beyond that the refusal is
-immediate.
-
-Documented consequences: an adoption that takes longer than 15 s (large
+the host; `serveCacheMiss`, outside singleflight, waits under the same
+bound and retries the miss once: requests that waited together coalesce
+again, a client that left during the wait gets no retry, and the refusal
+is logged once per fetch, by the singleflight leader (design review O1).
+At most 64 waiters per host, each waiting request counting (coalesced
+requests for one `.deb` included); beyond that the refusal is immediate.
+
+Documented consequences: a client fetching N unvouched packages one
+after another during a long adoption waits up to 15 s for each (M-11); an
+adoption's count ends after its flip barrier (up to 140 s for a busy
+suite), so a package whose snapshot is still in its barrier is likely to
+wait the full 15 s and be refused; an adoption that takes longer than 15 s (large
 `dists/` suites), a queued (not yet running) first adoption, and an unsigned
 or anchorless repository on a host that also serves complete signed suites
@@ -1129,5 +1255,7 @@ gauges `acu_adoption_failing_suites` (C7), `acu_freshness_waiting_for_slot`
 (flips in the C8 barrier now), the C3 pair
 `acu_suite_seed_rejected_total{host}` (counter) and
-`acu_suites_seed_rejected` (gauge), and `acu_freshness_unlogged_panics_total`.
+`acu_suites_seed_rejected` (gauge), `acu_freshness_unlogged_panics_total`,
+and phase E's `acu_repair_deferred_total{host}` and
+`acu_strict_adoption_waits_total{host}`.
 New metrics are host-labelled or
 unlabelled (no suite labels) and are added to the metrics inventory comments in
@@ -1146,7 +1274,11 @@ Constants, not knobs: backoff 2 m → 1 h (adoption failures and repeated
 *unknown* checks); hold 20 s, of which the last 5 s are kept for the commit
 (the C8 quiet cut at 15 s); phase A cap 60 s; hold-token wait 60 s; waiter
-caps 256 (barrier) and 64 (strict wait); strict wait 15 s; reaps 25 per run;
-unsigned member cap 1,000; check deadline 2 min; check workers 4; interest
-window 7 d; orphan-marker TTL 365 d; cap-rejection cache 10 min (1 min for
+caps 256 (barrier) and 64 (strict wait); strict wait 15 s, within a 25 s
+request budget, none below 1 s; the miss path's backstop deadline at most
+25 s after the request started; reaps 25 per run;
+unsigned member cap 1,000; anchor-absent and unsigned confirmation gap
+`max(cooldown, 5 min)`, 1 h after a 401/403 on a signature file; check
+deadline 2 min; check workers 4; interest window 7 d; orphan-marker TTL
+365 d; cap-rejection cache 10 min (1 min for
 a refusal while every eviction candidate is busy; at most 4,096 suites);
 tracked suite path at most 1024 bytes; legacy-audit anchor
@@ -1392,5 +1524,11 @@ files), in phase D.
   suite is not probed by ticks while it waits, is re-checked within *D* of
   its slot's release with the cooldown bypassed, and a busy offer neither
-  spins nor loses its place;
+  spins nor loses its place; the upgrade burst — 50 never-adopted suites
+  with interest and a trickle of signed changes, pool of 2: first
+  adoptions back to back, a signed change waits for at most one adoption,
+  each first adoption costs two probes by the scheduler, plus one per client
+  check while it waits and one per offer another admission takes first
+  (phase E, ruling PE-4: the deterministic burst, and a racing one with
+  every suite on one host);
   clock steps ±2 h/±30 d with liveness asserts;
   `TestCheck_NoAdopter_ChangeOnlyLogged` (setup seeds the row); scheduler
@@ -1405,6 +1543,9 @@ files), in phase D.
   timings), never-downgrade at both points, member cap, no `package_hash`
   from reconcile; an unsigned adoption paused before commit while a signed
-  anchor is served and its marker committed (also via the `/.` twin) is
-  refused with no pointer change; `require_signature = false` with
+  anchor is served and its marker committed (also via the `/.` twin, in
+  phase G) is refused with no pointer change, as a refusal state; a
+  missing marker with a sniff-passing cached signed anchor is re-marked
+  before the commit, which then refuses (ruling R-E1);
+  `require_signature = false` with
   marker-present malformed or frameless bodies creates no inline snapshot,
   `package_hash` row or strict participant (`malformed_clearsign` distinct
```

- [ ] **Step 5: Overview**

The spec reference becomes revision 8; phase E's row reads "in progress (plan written 2026-10-19)", as phase D's docs task set its own, and becomes "done (commit …)" after the final review; the "Later phases add" E line gains the names as built, and the D line's "(the T1 on a creating miss: phase E, design note 2)" names the phase D plan's note it meant (this plan numbers its own notes). The "**E:**" carry-forward block is deleted, as phases C and D deleted theirs — its items are done: `Form: cache.FormUnsigned` set explicitly (Task 3); one check tail and the counted default arms (Task 4, R-E3); `contentKey`'s unsigned case and `classifyAdoptionReason`'s per-sentinel cases (Task 3); the waiters' signed flag (Task 8); `out.report` on every exit, `first_adoption_queued` in `isQueuedResult`, `signedCurrent = false` for first and unsigned requests and the upgrade burst (Tasks 6, 8), with `max_concurrent_adoptions = 3` documented beside eager first adoption (Step 1); `Form` dispatch for the detached check and `snapshotAnchorHash` (Tasks 4, 7); the unsigned confirmation's `adoption_fail_first_at` (Task 6, deviation 2); absent statuses off the failure streak (Tasks 6, 7, R-E4); `signedCurrent` for repair and reconcile and the two-slot sentence (Task 7, Step 1); `refusalKeepsCurrent` untouched and pinned (Task 3); `servedAnchorUnchanged`'s unsigned case and the displaced SHA512 by-hash test (Tasks 2, 3); the in-commit refusal recorded as a state after a barrier past `max_duration`, off the failure path (Task 3, R-E2); T1-on-seed on the miss path (Task 10); unsigned eligibility beside the marker (Task 3, R-E1); the waiters' late registration (Task 10). Its `suite_signed_seen.last_seen_at` item moves to **F:** (phase E reads markers only for their existence; the re-mark stamps a row's last use verbatim, as the anchor pass does; F's 365-day orphan-marker sweep is the stamp's first reader). The intro gains the *(E)* legend; **F:** and **G:** gain phase E's items, and a new **H:** block holds the two phase E items for H (phase D's *(D)* H item stays under "PR #8 parity": D-55 is phase H's).

```diff
diff --git a/docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md b/docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md
index 57770dc..d294426 100644
--- a/docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md
+++ b/docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md
@@ -9,5 +9,5 @@
 **Tech Stack:** Go 1.25 (toolchain 1.26), modernc SQLite, `github.com/ProtonMail/go-crypto`, `testing/synctest`, Docker for e2e.
 
-**Spec:** `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (revision 7). Plan reviews: 2026-09-28 (phase A executability and tests) and `2026-10-05-flat-repositories-plan-review.md` (upgrade trust and later-phase contracts; its findings map to the "Review findings 2026-10-05" table below). Every task implements a named spec section; when this plan and the spec disagree, the spec wins unless the plan records a deliberate, explained deviation (listed below).
+**Spec:** `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (revision 8). Plan reviews: 2026-09-28 (phase A executability and tests) and `2026-10-05-flat-repositories-plan-review.md` (upgrade trust and later-phase contracts; its findings map to the "Review findings 2026-10-05" table below). Every task implements a named spec section; when this plan and the spec disagree, the spec wins unless the plan records a deliberate, explained deviation (listed below).
 
 ## Global Constraints
@@ -63,5 +63,5 @@ Each phase: write its detailed plan (`2026-09-28-flat-repositories-<letter>-<nam
 | C. Flip consistency | `…-C-flip.md` | C8 incl. the hold token | B | done (commit 1f736a6) |
 | D. Creation bounds and signals | `…-D-creation.md` | C3 (rest: caps, touch, miss-path signed-seen marker, `Release`/flat sniffs) | A | done (commit 3ed0b2b) |
-| E. Eager adoption and anchor forms | `…-E-forms.md` | C4 probe tree, C5 unsigned/SHA512, C10 wait | B, D | to write after D |
+| E. Eager adoption and anchor forms | `…-E-forms.md` | C4 probe tree, C5 unsigned/SHA512, C10 wait | B, D | in progress (plan written 2026-10-19) |
 | F. Suite reaping | `…-F-reaping.md` | C9 | D, E | to write after E |
 | G. Flat repositories | `…-G-flat.md` | C2 flat, C5 flat IndexTargets/Filenames, C6, C4 anchorless | A–F | to write after F |
@@ -92,6 +92,6 @@ Later phases add (named here so phase plans stay consistent):
 - B: `cache.RecordAdoptionFailure`, `cache.ClearAdoptionFailure`, `freshness.contentKey`, `freshness.backoffDelay`. `adoption_next_retry_at` is a future-event stamp: B clamps it to now + 1 h and never applies the past-event ("infinitely old") rule to it — a detection is only logged through `clockStep` (phase B plan, deviation 6); B tests that a future `next_retry_at` after a backward step yields a retry within 1 h, not immediately. B also records panicked adoptions in the admin adoption ring. **Scheduler fairness (review finding 7 and the three-lens review, spec C4):** per-host queues of due suites refreshed every tick (no duplicates, persist across ticks), 4 workers, at most one check in flight per host, hosts take turns round-robin one check per turn; within a host healthy (`check_fail_count = 0`) → never-checked → failing, first in first out by queue time inside each class; backoff 2 min → 1 h computed from the persisted `check_fail_count` and `last_check_at` (every check maintains the streak in the UPDATE that already bumps `last_check_at`); goroutines and queue memory bounded. Latency bound: one tick interval + (n + 1) × ⌈H/4⌉ × 2 min, n = suites ahead of S in its host's queue (healthy ones only when S is healthy). B's DoD includes synctest simulations with **stalling** responders (not refused connections, which the fetch client's unreachable-host cooldown already collapses): (1) H = 8 hosts, several stalling, a healthy suite with n = 3 healthy suites ahead — assert the bound; (2) 500 stalling suites and 1 healthy suite on one host — the healthy suite is checked within one tick + ⌈H/4⌉ × 2 min, and again on the first tick after a simulated restart (a mutation that orders never-checked suites first, keeps the streak in memory, or keys the order on `last_check_at` must fail it); both with persistent repair and changing-candidate contention, continuing checks and a goroutine bound; the capacity simulation exercises both check and adoption admission. (An equivalent pull model — a free worker asks the cache for the next due suite — is acceptable if B's plan finds the queue bookkeeping error-prone.) **Slot-driven admission (spec C4 rev 5, phase B Task 11b):** `(*admission).freeFor(host, signedCurrent) int` (a peek that mirrors `tryAcquire`) and its `freed` wake channel; `checkOpts{bypassCooldown}`, `checkReport{result, signedCurrent}`, `(*Checker).check(ctx, ref, opts) checkReport` behind `Check`; `isQueuedResult(result)`; scheduler `park`, `finish`, `unhold`, `pop(admit)`; gauge `acu_freshness_waiting_for_slot`. A scheduler check that ends queued leaves its suite waiting in its host's queue; every slot release re-offers the freed capacity to the oldest waiting suite it admits, cooldown bypassed; a busy offer is held until the next wake or tick.
 - C: package `flipgate` — `Gate` (`New(Config{Quiet})`, `Enabled`, `Quiet`, `Stats`, `Len`, `Purge(key)`, `Close()` (shutdown; `main` calls it at the signal), `Closed()`, `AnchorStart(ctx, key) *Token`, `Flip(ctx, key, commit) (Result, error)`), `Key(scheme, host, suitePath)` (the freshness suite key), `Token.Done`, `Token.Held`, outcomes `held`/`quiet_cut_short`/`hold_expired`/`hold_skipped`/`aborted`/`disabled`, `ErrAborted`, `ErrClosed`, the spec C11 constants (`PhaseAMax`, `TokenWaitMax`, `HoldMax`, `CommitMargin`, `AnchorTimeout`, `MaxWaiters`, `MinQuiet`, `MaxQuiet`; `MaxQuiet ≤ HoldMax − CommitMargin` is pinned), the counters `acu_flip_hold_skipped_total`, `acu_flip_quiet_cut_short_total`, `acu_flip_hold_expired_total`, `acu_flip_waiters_over_cap_total`, `acu_flip_slow_anchor_reads_total`, `acu_flip_gate_panics_total` and the gauge `acu_flip_barrier_active`. `freshness.AdoptionConfig.FlipGate` and the nil-safe `(*Adopter).FlipGate()` (the handler shares the adopter's gate: `handler.Config.FlipGate`); `(*Adopter).SetCommitDelayForTest` (test seam: a commit's latency in simulations); `handler.Config.KeepDisplaced`; `cache.DisplacedSnapshotMembers`; `config.AdoptionConfig.FlipQuietPeriod`; the events `adoption_flip_barrier` (outcome `commit_failed` when the commit fails) and `adoption_abandoned_at_shutdown` (a flip abandoned at the signal: INFO, never an adoption outcome); the request fields `snapshot_displaced` and `flip_held_ms`; `acu_serve_byhash_displaced_total{host}`. Snapshot anchors' `Last-Modified` is `adopted_at` alone (never the pool mtime). Rulings: an adoption keeps its global slot through the barrier and frees its host slot before it (R-C1); the barrier's waits ignore `adoption.max_duration` and end on shutdown, committing nothing (R-C2); the admission bound's *A* includes the barrier's ≤ 140 s (R-C3). Five busy suites flipping at once: four hold, the fifth commits unheld (phase C plan, deviation 2).
-- D: `cache.CreateSuite`, `cache.UpdateSuiteFreshness`, `cache.TouchSuite`, `cache.ErrSuiteGone` (the UPDATE-only `CommitAdoption` Step 6, spec C3), `aptpath.SniffRelease`, `aptpath.SniffFlatIndex` (the `InRelease`/`Release.gpg` sniffs and marker writes already exist from A). As built: `cache.CreateSuite(ctx, SuiteSeed, SuiteCaps, SuiteGuard) (CreateResult, error)` (`SuiteSeed.Cached` carries the suite's pre-read cached anchors, `CreateResult.Unproven` asks for the backstop mark, `CreateResult.CandidatesBusy` flags a transient all-busy refusal), `cache.StoreFetched(ctx, FetchedRow, SuiteCaps, SuiteGuard) (FetchedResult, error)` (`FetchedRow.Restamp`: the adoption-disabled refresh miss), `cache.SuiteExists`, `cache.SuiteCapUsage`, `cache.TouchSuite` / `TouchedSuites`, `cache.SuiteGuard` (implemented by `(*freshness.Checker).HoldSuite`) and the optional `cache.ClockStepReporter` (the checker reports eviction's future stamps), unexported `deleteSuiteTx(ctx, tx, key, wantCurrent *int64, cutoff, now) (deleteOutcome, error)` (F's `ReapIdleSuite` reuses it), `freshness.Config.MaxSuites`, `config.FreshnessConfig.MaxSuites`, `aptpath.ClassifySuiteFile` / `SuiteFile`, `aptpath.MaxSuitePathBytes`, `aptpath.DecodableClearsign`, `simtest.ClearsignedFixture`, `(*freshness.Checker).LockCount`. **Signed-seen marker on the miss path (review finding 5, its follow-up and the three-lens review, spec C3):** anchor rows (`InRelease`, `Release`) are created in the writer operation that stores the `url_path` row, before the body is written (sniff → `url_path` + `CreateSuite` + marker → write body → T1, today's seed-before-serve order (the T1 on a creating miss: phase E, design note 2)); the marker for a sniff-passing `InRelease`/`Release.gpg` is written in that same operation and nowhere else in the handler — no hit-path write (except that every row creation, on a miss or on a hit, marks the suite's cached signed anchors in its own transaction: phase D plan, deviation 2, ruling PD-5), no wait, no coalescing map, no failure counter; a failed operation serves no body (502, as today). `Release.gpg` creates no row. **The backstop and its residual:** the one marker write outside a creating or storing transaction is the handler's `markCachedSignedAnchors`, which runs when a creation is unproven (`CreateResult.Unproven`: one of the suite's cached `InRelease`/`Release.gpg` rows changed, appeared or could not be read between the handler's pre-read and the creating transaction). It reads, sniffs and marks that anchor right after the commit and the late registration, before the response, bounded at 5 s. So an unproven creation's suite is tracked with that anchor unmarked from the commit until the backstop runs, and — if the backstop fails (WARN `suite signed-seen mark failed` or `suite signed-seen backstop read failed`) — until the startup marker pass at the next start (spec C3 accepts it; E carry-forward below). Tests: the marker row exists before the first body byte on first contact and on a refresh miss; a failed writer operation serves no body; the hit-path invariant (after misses, adoptions, `reconstructAnchor` and restarts, every sniff-passing anchor row of a tracked suite has a marker (deviation 1)); FIFO ordering against a later unsigned commit. D also removes reconcile's plain-text fallback in `inReleasePlaintext` once `clearsignedFixture()` replaces the plain-text test fixtures (flip `TestReparseSnapshotRelease_PlainTextInReleaseStillParses` to `_PlainTextInReleaseIsAnError`).
-- E: `freshness` probe classification (`classPresent`, `classAbsent`, `classUnknown`), `Adopter.RunUnsigned`, `aptpath.AnchorPaths(form)` (spec C5). E's unsigned candidates set `Form: cache.FormUnsigned` explicitly, and its tests assert that an unsigned adoption stores `anchor_form = 'unsigned'` and vouches no `.deb`. E also resolves the two `AIDEV-TODO(phase E)` raw-SQL readers (url_path GC guard, `CommitAdoption` anchor seed: for `FormUnsigned`, Step 3c seeds `<suite>/Release`, not `/InRelease`). E's docs note the marker denial-of-service residual (spec C5 threat notes) at `adopt_unsigned`. **In-transaction never-downgrade (review finding 5, spec C5):** `CommitAdoption` re-reads the marker for `aptpath.MarkerBase(suite)` inside its writer transaction and refuses an unsigned candidate (no pointer change, no rows) when one exists or the prior current snapshot is signed; E's DoD pauses an eligible unsigned adoption before commit, serves a signed anchor and completes the marker write (also via the `/.` twin, once G exists — E adds the twin variant as a G DoD item), then resumes and asserts refusal. **Upgrade (review findings 2–3):** a phase-A-quarantined plain-text suite (fixture from A's `buildLegacyFixture` shape) adopts in the unsigned form through `Release` with no upstream change and no `unsigned_downgrade_refused`; a never-adopted suite whose signed anchor was served under a v6 binary is refused unsigned adoption on its first post-upgrade check (A's startup pass wrote its marker).
+- D: `cache.CreateSuite`, `cache.UpdateSuiteFreshness`, `cache.TouchSuite`, `cache.ErrSuiteGone` (the UPDATE-only `CommitAdoption` Step 6, spec C3), `aptpath.SniffRelease`, `aptpath.SniffFlatIndex` (the `InRelease`/`Release.gpg` sniffs and marker writes already exist from A). As built: `cache.CreateSuite(ctx, SuiteSeed, SuiteCaps, SuiteGuard) (CreateResult, error)` (`SuiteSeed.Cached` carries the suite's pre-read cached anchors, `CreateResult.Unproven` asks for the backstop mark, `CreateResult.CandidatesBusy` flags a transient all-busy refusal), `cache.StoreFetched(ctx, FetchedRow, SuiteCaps, SuiteGuard) (FetchedResult, error)` (`FetchedRow.Restamp`: the adoption-disabled refresh miss), `cache.SuiteExists`, `cache.SuiteCapUsage`, `cache.TouchSuite` / `TouchedSuites`, `cache.SuiteGuard` (implemented by `(*freshness.Checker).HoldSuite`) and the optional `cache.ClockStepReporter` (the checker reports eviction's future stamps), unexported `deleteSuiteTx(ctx, tx, key, wantCurrent *int64, cutoff, now) (deleteOutcome, error)` (F's `ReapIdleSuite` reuses it), `freshness.Config.MaxSuites`, `config.FreshnessConfig.MaxSuites`, `aptpath.ClassifySuiteFile` / `SuiteFile`, `aptpath.MaxSuitePathBytes`, `aptpath.DecodableClearsign`, `simtest.ClearsignedFixture`, `(*freshness.Checker).LockCount`. **Signed-seen marker on the miss path (review finding 5, its follow-up and the three-lens review, spec C3):** anchor rows (`InRelease`, `Release`) are created in the writer operation that stores the `url_path` row, before the body is written (sniff → `url_path` + `CreateSuite` + marker → write body → T1, today's seed-before-serve order (the T1 on a creating miss: phase D plan, design note 2; phase E plan, Task 10)); the marker for a sniff-passing `InRelease`/`Release.gpg` is written in that same operation and nowhere else in the handler — no hit-path write (except that every row creation, on a miss or on a hit, marks the suite's cached signed anchors in its own transaction: phase D plan, deviation 2, ruling PD-5), no wait, no coalescing map, no failure counter; a failed operation serves no body (502, as today). `Release.gpg` creates no row. **The backstop and its residual:** the one marker write outside a creating or storing transaction is the handler's `markCachedSignedAnchors`, which runs when a creation is unproven (`CreateResult.Unproven`: one of the suite's cached `InRelease`/`Release.gpg` rows changed, appeared or could not be read between the handler's pre-read and the creating transaction). It reads, sniffs and marks that anchor right after the commit and the late registration, before the response, bounded at 5 s. So an unproven creation's suite is tracked with that anchor unmarked from the commit until the backstop runs, and — if the backstop fails (WARN `suite signed-seen mark failed` or `suite signed-seen backstop read failed`) — until the startup marker pass at the next start (spec C3 accepts it; E carry-forward below). Tests: the marker row exists before the first body byte on first contact and on a refresh miss; a failed writer operation serves no body; the hit-path invariant (after misses, adoptions, `reconstructAnchor` and restarts, every sniff-passing anchor row of a tracked suite has a marker (deviation 1)); FIFO ordering against a later unsigned commit. D also removes reconcile's plain-text fallback in `inReleasePlaintext` once `clearsignedFixture()` replaces the plain-text test fixtures (flip `TestReparseSnapshotRelease_PlainTextInReleaseStillParses` to `_PlainTextInReleaseIsAnError`).
+- E: `freshness` probe classification (`classPresent`, `classAbsent`, `classUnknown`), `Adopter.RunUnsigned`, `aptpath.AnchorPaths(form)` (spec C5). E's unsigned candidates set `Form: cache.FormUnsigned` explicitly, and its tests assert that an unsigned adoption stores `anchor_form = 'unsigned'` and vouches no `.deb`. E also resolves the two `AIDEV-TODO(phase E)` raw-SQL readers (url_path GC guard, `CommitAdoption` anchor seed: for `FormUnsigned`, Step 3c seeds `<suite>/Release`, not `/InRelease`). E's docs note the marker denial-of-service residual (spec C5 threat notes) at `adopt_unsigned`. **In-transaction never-downgrade (review finding 5, spec C5):** `CommitAdoption` re-reads the marker for `aptpath.MarkerBase(suite)` inside its writer transaction and refuses an unsigned candidate (no pointer change, no rows) when one exists or the prior current snapshot is signed; E's DoD pauses an eligible unsigned adoption before commit, serves a signed anchor and completes the marker write (also via the `/.` twin, once G exists — E adds the twin variant as a G DoD item), then resumes and asserts refusal. **Upgrade (review findings 2–3):** a phase-A-quarantined plain-text suite (fixture from A's `buildLegacyFixture` shape) adopts in the unsigned form through `Release` with no upstream change and no `unsigned_downgrade_refused`; a never-adopted suite whose signed anchor was served under a v6 binary is refused unsigned adoption on its first post-upgrade check (A's startup pass wrote its marker). As built: `freshness.classify(res, err, sniff, conditional) probeOutcome` (classes `classPresent`, `classAbsent`, `classUnknown`) and the probe tree `runProbeTree`; `(*Adopter).RunUnsigned(ctx, suite, release, etag, lastmod) error` and `(*Adopter).AdoptsUnsigned() bool`; `aptpath.AnchorPaths(form string) []string` and `(*cache.SuiteSnapshot).AnchorHash(name string) *string`; `cache.ErrUnsignedDowngradeRefused` (the refusal inside the commit) and `freshness.ErrAdoptionUnsignedDowngradeRefused`, `ErrAdoptionUnsignedTooLarge`; `(*freshness.Checker).AdoptionsInFlight(host) bool` and `WaitHostAdoptions(ctx, host) bool` (the `handler.AdoptionWaiter` the handler gets as `handler.Config.Adoptions`); `config.AdoptionConfig.AdoptUnsigned` and `freshness.AdoptionConfig.AdoptUnsigned`; the `freshness.Cache` method `ListSuitesWithAdoption` (in place of `ListSuites`); the flip barrier's code in `internal/freshness/flip_barrier.go`; the metrics `acu_repair_deferred_total{host}` and `acu_strict_adoption_waits_total{host}`. The `/.` twin variant of the in-commit race test is G's (below).
 - F: `cache.ReapIdleSuite`, `cache.RetireSuiteSnapshots`, admin `POST /forget`. Non-canonical legacy suite rows (`!aptpath.IsCanonicalSuitePath`) are reaped regardless of idleness (spec C9); the plan review found none in production. The reaper's refcount release reuses `decrementSnapshotRefcountsTx`; snapshot GC sub-job C also deletes the snapshots phase A's audit retired, and sub-job B's `keep_displaced` ranking excludes retired rows so they never take retention slots. The reaper sweeps a signed-seen marker only when no suite row exists for its base or `/.` twin and it is older than 365 days. F's DoD includes a signed snapshot adopted under the phase-A binary, reaped by the F binary, then returning: never-downgrade still refuses unsigned adoption (markers outlive reaping).
 - G: `aptpath.IsFlatSuitePath`, `aptpath.FlatBase`, `aptpath.DotClean`, `freshness.validateFilename`, and the admin `suites[].flat` field (phase B plan, deviation 3). Legacy flat onboarding also writes signed-seen markers for sniff-passing cached flat anchors (the startup pass only covers suites that already have rows), and the `/.` twin variant of E's in-transaction recheck test runs here. **PR #8 evidence in G's DoD:** `TestServeHTTP_FlatRepositoryFreshness` (a miss on `/…/deb/InRelease` and on `/…/deb/./InRelease` creates one suite row each and fires T1 under it; a later hit fires T1 again; `/…/deb/Packages_1.0_all.deb`, host-root `/InRelease` and `/…/deb//InRelease` create none), the pkgs.k8s.io shape `/core:/stable:/v1.34/deb/InRelease` → `/core:/stable:/v1.34/deb`, flat `Packages` failure fatal, and the mixed-host strict matrix. G also ports the rest of PR #8's tests (see "PR #8 parity" below).
@@ -100,26 +100,5 @@ Later phases add (named here so phase plans stay consistent):
 ## Carry-forwards (from phase A and B reviews)
 
-Each item is owned by the phase it is listed under; that phase's plan addresses it. Items came from the phase A reviews (2026-10-11) unless marked *(B)* (the phase B reviews and its final review, 2026-10-18) or *(C)* (the phase C plan, 2026-10-18) or *(D)* (the phase D plan, 2026-10-18).
-
-**E:**
-
-- Production unsigned candidates must set `SnapshotCandidate.Form` explicitly; derivation from hashes is for legacy and fixture callers only.
-- `checkLockedInline` and `checkLockedDetached` share long verbatim blocks after phase B (gated 304, unchanged + record clear, the persist/clear/log/metric tail; Task 7 review), plus *(B)* Task 9's no-slot `tryAcquire` block and `signedCurrent` block and Task 10's unexpected-status default arms. E's probe-tree rewrite of the check path folds them into one shared tail (e.g. `finishCheck`) and one 304/unchanged helper, so record-clearing and the `freshness_check` line live in one place. The default arms log only `freshness: unexpected success status` (ERROR) and increment no counter; route them through `recordUnknownEnd`'s line and `acu_freshness_check_total{result="failed"}` in the same refactor (`docs/log-fields.md` documents today's behaviour).
-- *(B)* `contentKey` (`retry.go`) hashes its `inline` argument for every form except detached. An unsigned caller must key the Release body explicitly (pass it as `inline`, or give `contentKey` an unsigned case); passing it as `release` keys all unsigned content as sha256(nil), so new content would be deferred, breaking C7's "attempted at once".
-- *(B)* `classifyAdoptionReason`'s `refused` case (`metrics.go`) returns `rollback_refused` for any refusal other than `upstream_older`. Each E refusal sentinel needs its own case: recorded as `rollback_refused`, it would be treated by `refusalKeepsCurrent` as "the cache is current" and its lag hidden — the opposite of the Task 14 ruling.
-- *(B)* A parked waiter keeps the `signed` flag of the check that parked it (Task 11b). Once T1 checks can perform first adoptions, a waiter parked as a first adoption (`signed = false`) may already serve a signed snapshot by its offer; it then waits for two free slots instead of one until its next re-check corrects the flag. Bounded, but re-check the flag (or re-read it at offer time) when E touches the queue.
-- Slot-driven admission (phase B Task 11b): the probe-tree check sets `out.report` on every exit; `isQueuedResult` includes `first_adoption_queued`; first and unsigned requests pass `signedCurrent = false`. The capacity simulation gains the upgrade-burst scenario: 50 never-adopted suites with interest and a trickle of signed changes → first adoptions back to back, signed suites never wait longer than one adoption, each queued suite costs at most two probes. Document `max_concurrent_adoptions = 3` as the knob that lets two first adoptions run during the burst.
-- `checkLockedDetached` (still tests `ReleaseHash != nil`) and `snapshotAnchorHash`'s `Release` case need `Form` dispatch for unsigned.
-- `suite_signed_seen.last_seen_at` is a past-event stamp under C4: a future stamp counts as infinitely old and is healed on the next observation. The anchor pass copies `MAX(last_requested_at, last_fetched_at)` verbatim.
-- The unsigned confirmation gate compares `adoption_fail_first_at`; apply the C4 past-event rule to it there. (Phase B resets a future streak start when it records a failure and clamps `next_retry_at`; no phase-B gate reads `first_at`/`last_at`.)
-- `recordUnknownEnd` treats every `Conditional` error as unknown, extending `check_fail_count` — including 401/403/404/410; E's absent classification must bypass it (reset the streak) for those statuses, or `anchor_absent` suites inherit the hourly scheduler backoff.
-- `planRepair` and `Reconcile` pass `signedCurrent = true` (true in phase B, where every current snapshot is signed); E must pass `snap.Form != FormUnsigned`. First adoptions take a slot only when two are free under the default `max_concurrent_adoptions = 2`; E: say so in configuration.md next to eager first adoption (phase B's *Capacity and fairness* already states the two-slot rule).
-- `internal/admin`'s `refusalKeepsCurrent` lists the refusal reasons for which the cache already serves content at least as new as upstream (phase B: `upstream_older`, `rollback_refused`); E's unsigned refusal reasons stay out of it so a stuck suite keeps showing lag.
-- *(C)* `servedAnchorUnchanged` (`internal/freshness/adoption.go`, `AIDEV-TODO(phase E)`) must compare `<suite>/Release` for the unsigned form. Retained by-hash already serves SHA512 aliases from displaced snapshots (phase C plan, deviation 3): E's SHA512 adoption only writes the alias rows, and its SHA512 by-hash test should include a displaced snapshot.
-- *(C)* An in-commit refusal after a barrier that outlived `adoption.max_duration` must still be recorded as the refusal. `spawnAdoption` reclassifies an attempt as `timed_out` only when the deadline fired before the member pipeline finished (phase C plan, design note 19), so E's never-downgrade refusal must surface from `CommitAdoption` as its refusal sentinel through `classifyAdoptionOutcome`; E's refusal test should include a barrier that outlives the deadline.
-- *(C)* Keep an in-commit refusal off the failure path. Today any error from `CommitAdoption` reaches `flip()` as a commit error: `adoption_flip_barrier` logs `commit_failed`, `runShared` logs WARN `adoption: commit failed` and wraps the error in `ErrAdoptionDBFailed` (`internal/freshness/adoption.go`, Step 11). A refusal is a state, not a failure (spec C7): E must let a refusal raised inside `CommitAdoption` bypass that WARN and the `db_failed` wrap (and give the barrier event an outcome that is not `commit_failed`), and its refusal test should assert the log levels.
-- *(D)* Fire T1 once a miss creates an adopter-wired row (spec C3; phase D plan, design note 2); until then the next hit's T1 or the never-checked class checks it. Add it to the miss path only: key it on `FetchedResult.Create.Outcome == cache.SuiteCreated` (`sfResult` needs a `created` field; the `runFetch` AIDEV-NOTE names `stored.Create.Outcome`); `FetchedResult.Tracked` is what the late flip-gate registration uses. The hit path already fires T1 after onboarding (`onboard` sets the suite tracked, then `maybeFireFreshness`: design note 3, ruling PD-11), so a T1 keyed on `CreateResult.Outcome` there would only add a second, busy or cooled-down check per onboarding hit (phase D final review, spec Minor 7).
-- *(D)* Unsigned eligibility (spec C5 safeguard 3) must not rest on the marker alone: phase D's backstop residual (above, under "Later phases add") leaves a tracked suite whose sniff-passing signed anchor is unmarked after a failed backstop until the next start, and a MITM answering 404 for `InRelease` and `Release.gpg` on two checks could then adopt it unsigned. At unsigned eligibility, treat a sniff-passing cached `InRelease`/`Release.gpg` `url_path` row of the suite (C4 Dispatch 3's "sniff-passing cached anchor row") as signed evidence alongside the marker, or re-run the cached-anchor mark under the suite mutex before any unsigned commit (phase D final review, spec Minor 1 and the concurrency review's declined note).
-- *(D)* Waiters' late registration with the flip barrier is untested on both paths (an onboarding waiter, `t10_waiter_noreg`; a coalesced miss waiter, `t9_miss_waiter_noreg`): no flip can hold a just-created suite in phase D. Once E's eager first adoption makes a flip of a just-created suite reachable, add a test per path that a waiter registers its own serve (phase D final review, tests Minor 3; Task 10 minor 2).
+Each item is owned by the phase it is listed under; that phase's plan addresses it. Items came from the phase A reviews (2026-10-11) unless marked *(B)* (the phase B reviews and its final review, 2026-10-18) or *(C)* (the phase C plan, 2026-10-18) or *(D)* (the phase D plan, 2026-10-18) or *(E)* (the phase E plan, 2026-10-19).
 
 **F:**
@@ -134,4 +113,7 @@ Each item is owned by the phase it is listed under; that phase's plan addresses
 - *(D)* `POST /forget` deletes through `deleteSuiteTx` too (cutoff `math.MaxInt64`, `wantCurrent` = the current snapshot it read), holding the suite through the `SuiteGuard`: otherwise R-D1's "one suite-row delete path" breaks, and so does `ErrSuiteGone`'s "a backstop, not an expected path" (phase D plan, design note 12).
 - *(D)* Legacy junk rows — suites a v6 binary created whose cached `InRelease` is an HTML page — stay alive because every anchor serve with a status below 400 touches its suite, sniff-failing bytes included. Reap them regardless of idleness (like non-canonical rows), or make the anchor touch require that the suite's anchor passed its sniff.
+- *(E)* `docs/configuration.md` (*Anchor forms and unsigned repositories*, the threat notes) and `docs/admin-ui-spec.md` (§4.5, the unsigned and absent-anchor refusals) name `POST /forget` "added with suite reaping in a later release": rewrite both when F lands.
+- *(E)* `POST /forget` is the documented escape hatch for `unsigned_downgrade_refused` (spec C5 threat notes): F's `/forget` re-adoption tests include a suite refused that way whose marker `/forget` deletes (no other suite row with its base), which then adopts unsigned after the two observations, and one whose `/.` twin keeps a row, which stays refused.
+- *(E)* `suite_signed_seen.last_seen_at` is a past-event stamp (spec C4): phase E reads markers only for their existence (unsigned eligibility, `CommitAdoption`'s recheck), and its re-mark (ruling R-E1) stamps the anchor row's last use verbatim, as the startup anchor pass does. F's 365-day orphan-marker sweep is the stamp's first reader: apply the clock-step rule there as for a gate that retires things — a future stamp reads as now, never as old — and heal it on the next observation. This corrects the earlier E carry-forward, which said a future stamp "counts as infinitely old": the sweep retires markers, and reading a future stamp as old would delete signed history after a backward clock step (phase E plan review, M-20).
 
 **G:**
@@ -140,4 +122,12 @@ Each item is owned by the phase it is listed under; that phase's plan addresses
 - *(C)* If G serves url_path metadata with `Last-Modified = max(pool mtime, last_fetched_at)` (spec C4, anchorless refresh), `adoptionStampTx`'s first-adoption floor (`MAX(last_fetched_at) + 1`, `internal/cache/adoption.go`) stays above it only while the pool file's mtime is not later than `last_fetched_at` — true today because `runFetch` finalizes the pool file before it stamps the row (`internal/handler/handler.go`). Keep that order, or fold the anchor blobs' mtimes into the floor.
 - *(D)* `ClassifySuiteFile` already returns `FileFlatIndex` for a root `Packages*`/`Sources*` of a suite without a `dists` segment, and the handler already sniffs, creates rows for and touches it; G's grammar makes those paths reachable (spec C3; phase D plan, ruling R-D3), so G adds no handler code for them — its tests add the flat cases to the miss-path, onboarding and touch tests. The cap-rejection cache is keyed by the suite alone, whichever file was refused, so a flat suite's indexes share one entry: G adds no per-file key.
+- *(E)* The `/.` twin variant of `TestCheck_UnsignedRefusedInsideTheCommitIsAState` (phase E plan, Task 3): the signed anchor and its marker arrive through the twin while the unsigned adoption of `/repo` is paused before its commit; the commit refuses it as a state.
+- *(E)* Spec C4 Dispatch 3: `cachedAnchors` (`internal/freshness/first_adoption.go`) counts only `InRelease` and `Release` rows as client-created (phase E plan, design note 14); G adds flat index rows (`Packages*`/`Sources*`, `request_count > 0`), and the anchorless refresh where the tree finds neither anchor (`errNoAnchor`, a failed check until then) for flat suites with no signed-seen marker and no sniff-passing cached anchor row.
+- *(E)* The nvidia-shaped DoD fixture is `dists/`-shaped (phase E plan, design note 29); G's DoD adopts the real flat layout with its `./` Filenames.
+
+**H:**
+
+- *(E)* The prose WARN names phase E adds — `freshness: read anchor rows failed`, `adoption: served anchor read failed; the flip barrier runs` and `adoption: signed-anchor re-mark failed` (documented as they are) — may become snake_case in H's documentation sweep, with phase D's (listed under "PR #8 parity").
+- *(E)* `README.md` (spec §8): unsigned and SHA512-only repositories, eager first adoption and the `adopt_unsigned` threat notes, beside the flat forms H documents.
 
 **Open decision (spec-level, for the maintainer):**
```

- [ ] **Step 6: Deferred files**

Delete the entries this plan resolved (each is one `- **ID** …` line; leave the section headings): A-29, A-37, A-47, A-49 (`…-A-deferred.md`); B-16, B-17, B-18, B-19, B-20, B-21, B-23, B-24, B-25, B-28, B-30, B-31, B-32, B-33, B-34, B-35, B-37, B-38, B-45, B-46, B-51, B-53, B-54, B-55, B-56, B-57, B-58, B-66, B-67 (`…-B-deferred.md`); C-16, C-17, C-19, C-29, C-40 (`…-C-deferred.md`); D-1, D-2, D-5, D-21, D-22, D-23, D-33, D-34, D-35, D-37, D-38, D-45, D-47, D-51, D-53, D-54, D-59 (`…-D-deferred.md`).

Then re-tag C-21 and narrow C-38 (ruling PE-7; Task 14):

```diff
diff --git a/docs/superpowers/plans/2026-09-28-flat-repositories-C-deferred.md b/docs/superpowers/plans/2026-09-28-flat-repositories-C-deferred.md
--- a/docs/superpowers/plans/2026-09-28-flat-repositories-C-deferred.md
+++ b/docs/superpowers/plans/2026-09-28-flat-repositories-C-deferred.md
@@ -36,5 +36,5 @@
 ## internal/handler
 
-- **C-21** (narrowed by phase D) only its context fast path remains (it needs new `flipgate` API); the `EXISTS` read landed in phase D, Task 10.
+- **C-21** (narrowed by phase D) only its context fast path remains (it needs new `flipgate` API); the `EXISTS` read landed in phase D, Task 10. Phase E leaves it (no measured gain, and it would touch the barrier's registration race; ruling PE-7). (H)
 
 ## cmd/apt-cacher-ultra
@@ -48,5 +48,5 @@
 ## Simulation budget and flakes
 
-- **C-38** Two phase-B simulations have little or no room under the 5 s `-race` budget: `TestSimScheduler_FiveHundredStallingOneHealthy` (6.15–7.82 s in the final tests review, 6.37 s in the fix wave's run) and `TestSim30Days_RetryDateRulesAndClockSteps` (4.4–5.1 s, then 3.85 s). Phase C does not change them. Coarsen them, never dropping assertions. (B scope; H)
+- **C-38** (narrowed by phase E: `TestSim30Days_RetryDateRulesAndClockSteps` was coarsened in phase E, Task 14, to about 3.6 s, every assertion kept at its per-day rate) `TestSimScheduler_FiveHundredStallingOneHealthy` has no room under the 5 s `-race` budget (6.15–7.82 s in phase B's final tests review, 6.37 s in its fix wave's run, 7.9 s in phase E). Coarsen it, never dropping assertions. (B scope; H)
 - **C-39** One unexplained failure of `TestPhase2DetachedChaos_AdoptionFlipUnderConcurrency` in a loaded parallel run (final review, concurrency; it did not reproduce in four `-race` reruns or alone). The test wires no gate and runs in real time. Investigate if it recurs. (H)
 
```

Check: `grep -c -E '^- \*\*[A-D]-[0-9]+\*\*' docs/superpowers/plans/2026-09-28-flat-repositories-[ABCD]-deferred.md` prints 4, 29, 5 and 17 fewer than before: 25, 43, 22 and 60 at `aecc0e9`, then 21, 14, 17 and 43.

- [ ] **Step 7: Final verification**

Run:

```bash
go vet ./...
golangci-lint run ./...
gofmt -l .
go test -race -count=1 ./...
grep -rn 'AIDEV-TODO(phase E)' --include=*.go .
grep -rn "Phase E's probe classification must reset" --include=*.go .
go test -race -count=1 -v -run 'TestDoD_|TestSimCapacity_UpgradeBurst|TestCheck_UnsignedRefusedInsideTheCommitIsAState|TestRunUnsigned_RemarksCachedSignedAnchors|TestCheck_DetachedSuiteThatDropsItsSignatureNeverGoesUnsigned|TestHostAdoptions_WaiterCap' ./internal/freshness/
go test -race -count=1 -v -run 'TestServeHTTP_Strict(Hit|Miss)WaitsForAnAdoptionInFlight|TestServeHTTP_MissThatCreatesItsSuiteFiresT1|TestServeHTTP_(CoalescedMiss|Onboarding)WaiterRegistersItsOwnServe|TestServeHTTP_AdoptionDisabledSeedParity|TestServeHTTP_UnsignedNeverChangesStrictPosture|TestServeHTTP_SHA512ByHashFromAnAdoption' ./internal/handler/
go test -race -count=1 -run 'TestSim' -v ./internal/freshness/ 2>&1 | grep -E '^--- (PASS|FAIL)'
```

Expected: PASS everywhere; vet clean; lint `0 issues`; `gofmt -l` lists exactly the five drift files (`cmd/apt-cacher-ultra/ca_print.go`, `internal/cache/cache_test.go`, `internal/cache/gc_urlpath_test.go`, `internal/freshness/hot_prefetch_test.go`, `internal/proxy/connect_stats.go`); both `grep`s print nothing; every simulation but `TestSimScheduler_FiveHundredStallingOneHealthy` (C-38, phase H) under 4.5 s (ruling PE-8: the 5 s budget with about 0.5 s of margin).

- [ ] **Step 8: Commit**

```bash
git add docs/
git commit -m "Document eager adoption, anchor forms and strict mode's wait; record phase E's rulings in the spec, update the overview and deferred files"
```

