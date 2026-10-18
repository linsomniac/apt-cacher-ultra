# Flat repositories — phase C deferred minors

Phase C's eleven task reviews and its three final-review lenses (concurrency, spec compliance, test strength; 2026-10-18) found these minor issues and triaged them "can wait". None is a correctness defect in shipped behaviour. They were recorded in a git-ignored ledger and in the final-review reports, and are moved here (deduplicated) so they survive.

**How they get done:** every later phase plan includes the items below that sit in files it already touches (cite the item ID in the task); an owning phase in parentheses marks where an item most naturally lands. Phase H ends with a sweep task that verifies each remaining item against the code, fixes it or records why not, and deletes this file.

Left out: everything the phase C final fix wave resolved (the phase-A wake test, the first-adoption skip under `Close`, the Last-Modified gaps, the Guarantee premise, the `timed_out` gloss, the operator-doc and AIDEV-anchor inaccuracies, the trace premise and its comment, the handler-level cancel test, the root-level by-hash test, the site-5 heartbeat comment, the startup-field and `shutdown wait panicked` documentation, the `Close` test's barrier-event assertion, the `keep_displaced = 0` and "30 s or longer" doc wording, `packageValidation`'s doc), and items the reviews closed without action: `Key` vs `suiteKey` (pinned by `TestSuiteKey_IsTheFlipGateKey`), the expiry timer's dropped panic value (an AIDEV-NOTE on `endHoldLocked` now states its must-not-panic invariant), "Done after the write" (a Done-before-write mutant fails the busy-suite simulations' exact commit instant and the slow-reader simulations), the floor's snapshot id and the Last-Modified-after-Date lead (both now in `anchorModTime`'s doc), and the phase-2 inline chaos assertion that cannot fail on its own (its AIDEV-NOTE says so). Line numbers are as of the fix wave and will drift.

## internal/flipgate

- **C-1** `gate.go:295-310`: two promised `Close` behaviours are untested — idempotence (a second call would panic on `close(g.closedCh)`) and `Close`'s own broadcast to a flip waiting only in phase A (with a hold in progress `endHoldLocked` broadcasts instead). The nil/disabled guard is covered by the cmd daemon tests. Fix: call `Close` twice in `TestDisabledGate_IsTodaysBehaviour` and the `Close` test; add a `Close` case with only a phase-A flip waiting.
- **C-2** `gate.go:164`, `:767`: `changedLocked` is called before the break and closed checks, so an idle flip leaves a channel allocated until the next broadcast; "nil while no flip waits" and "only a flip about to wait asks for it" overstate it. Reword, or call it only on the wait path inside the same critical section.
- **C-3** `gate.go:412`, `:767-779` (performance, final review concurrency Minor 6): every `Token.Done`, for any suite, closes the global `changed` channel, so each waiting flip re-locks, re-scans its suite's in-flight map and allocates a new channel and timer on every anchor completion in the cache, for up to the barrier's 140 s. Cheap today (ruling R-C1: at most one or two waiting flips under the default pool). Per-entry wake channels would remove it; not done in the fix wave because it changes the gate's wake mechanism for no measured gain.
- **C-4** `gate_test.go` `TestFlip_PhaseAWaitsOutAnotherSuitesHold` asserts only a lower bound; tighten it to `resB.HoldStart.Equal(a.res.HoldEnd.Add(testQuiet))` (the wake itself is pinned by `TestFlip_PhaseAWakesWhenAnotherSuitesHoldEnds`).

## internal/simtest and the model simulations

- **C-5** `simtest/aptclient.go:92`, `:103-112`: two ledger properties are pinned by no test — `held: heldBefore` (an anchor's own hold must not shorten its index's window) and recording `gens` only inside the window (`AnchorGens` is the "clients crossed the flip" proof). Add an update whose anchor is held 5 s and then followed by an 11 s filler, assert its index is not checked, and assert `AnchorGens` stays empty after out-of-window updates.
- **C-6** `flipgate/sim_test.go` `clients` and `recordHeld` repeat `gate_test.go`'s `busyReaders` and are copied verbatim as the handler's `flipReaders` and `recordAnchorHeld`; export a periodic-client spawner and `RecordHeld` from simtest.
- **C-7** `flipgate/sim_test.go` `clients`: client ids collide across groups running at once (the loop index), so `Straddle.Client` is ambiguous in a failure message; add an id-base parameter.
- **C-8** `simtest/aptclient.go` docs: say that `RunUpdate` is safe for concurrent use and blocks for the whole update (in real time outside a bubble), and that `AnchorGens` sorts as strings ("10" before "9").

## internal/config

- **C-9** `config_test.go:917-920`: the `flip_quiet_period` rejection loop checks only `err != nil`, not that the error names the key, and `"-1s"` may be rejected by `Duration.UnmarshalText` rather than `Validate`. (H)

## internal/cache

- **C-10** `adoption.go:720-750`, `:809-822`: the scoping and ordering predicates of `adoptionStampTx` and `DisplacedSnapshotMembers` are untested — no test has a same-path suite on a second host or scheme, and snapshot_id order equals adoption order in every test, although `InsertCandidateSnapshot` can reuse an older candidate. Add a second host and a reused older-id candidate.
- **C-11** `adoption.go:720`: "strictly increasing per suite" holds only along the current-snapshot chain. When `current_snapshot_id` is cleared (quarantine today, the reaper next), the first-adoption rule ignores the suite's remaining displaced snapshots, whose `adopted_at` may lead the clock, so GC's `keep_displaced` ranking and retained by-hash's newest-first window could misorder. Take `MAX(adopted_at)` over the suite's adopted snapshots as a floor, or narrow the AIDEV-NOTE. (F)
- **C-12** `flip_state_test.go:158`, `:174`, `:194`: `restore()` runs after `commitCandidate` instead of being deferred, and `stubNow` (`gc_test.go:42`) registers no cleanup, so a failing stamp test leaves `nowUnix` stubbed for the rest of the package.
- **C-13** `flip_state_test.go:146-191`: the served-anchor `PutURLPath` setup is written out three times; hoist `putAnchor`.
- **C-14** `adoption.go` (`CommitAdoption` Step 3a): the stamp block separates Step 3a's comment from its loop; move `now :=` and the stamp above the comment.
- **C-15** `adoption.go` is 1,173 lines, past the ~1,150 at which `freshness.go` was split; move the read-side snapshot helpers (`GetSuiteSnapshot`, `GetSnapshotMember`, `DisplacedSnapshotMembers`, `LookupSnapshotMember`) to their own file.

## internal/freshness

- **C-16** `adoption.go:1813`: `servedAnchorUnchanged` reads under `context.WithoutCancel(ctx)` — neither shutdown nor any bound ends the read while the goroutine holds the suite mutex and a global slot — and drops errors other than not-found silently. Read under `p.barrierCtx(ctx)` and log the others (the barrier still runs on an error). (E: it touches this function for the unsigned form)
- **C-17** `adoption.go:918`: `stopErr`'s fallback for `Run`/`RunDetached` (no stop context) has no direct test; a mutant returning `nil` survives. A case with `runHooks{membersDone: cancel}` and a nil stop would pin design note 14.
- **C-18** `flip_barrier_test.go`: `TestAdopter_ShutdownInTheBarrierIsNotACommitFailure` repeats `TestAdopter_ShutdownInTheBarrierCommitsNothing` line for line, and three older tests seed the served anchor by hand instead of `barrierRig.putServedAnchor`.
- **C-19** `freshness.go:443` is a 146-column line inside `spawnAdoption`'s AIDEV-NOTE; the barrier code in `adoption.go` (2,944 lines: `flip`, `barrierOutcome`, `servedAnchorUnchanged`) would sit better in `adoption_flip.go`.
- **C-20** `freshness.go` (`spawnAdoption`): a shutdown while the member pipeline runs is still classified (usually `run_failed`), counted, put in the admin ring and logged WARN `adoption_run_failed`; since the fix wave only a flip abandoned in the barrier is treated as shutdown (final review, tests Minor 3; a pre-existing pattern). Treat an attempt that ended because the lifetime context did the same way.

## internal/handler

- **C-21** `handler.go:1189-1206` (performance, final review concurrency Minor 5): every tracked anchor request builds `context.WithCancel` plus `context.AfterFunc(h.lifecycleCtx)` — a child registered on a process-wide context under its mutex — even when no hold is pending, which is almost always; and the presence check reads the full suite row, for untracked suites' anchor requests too. A non-blocking fast path (contexts only when the request must wait) and an EXISTS query would remove both; not done in the fix wave because it needs new gate API or a new cache query for no measured gain.
- **C-22** `handler.go:1197`: every `GetSuiteFreshness` error is treated as "no row", so a transient failure followed by a successful `trySnapshotHit` read serves the anchor unregistered — an uncounted straddle window, the opposite of design note 5's safe direction. Return nil only for `cache.ErrNotFound` or an ended request context, and register otherwise.
- **C-23** `handler.go:1189`: "no registration read while the barrier is off" rests only on the order of the checks; a mutant that swaps them survives every test.
- **C-24** `handler.go:528`, `:594`, `:654`: `go h.touchAsync(req)` starts goroutines with no recover (global constraint: every goroutine recovers panics); a pre-existing pattern around one SQL UPDATE.
- **C-25** `handler.go:779` `tryDisplacedByHash`: a displaced member whose pool blob is gone is a silent 404, while the current-snapshot path logs WARN `snapshot_member_blob_missing`; add a Debug or WARN line.
- **C-26** `byhash_displaced_test.go`: missing negatives — (a) a current-snapshot hit carries no `snapshot_displaced` and `X-Cache-Snapshot` is the current id; (b) a non-by-hash member declared only by a displaced snapshot stays 404 with `KeepDisplaced > 0`; (c) the 404 negatives should assert `up.count() == 0`, to tell a snapshot 404 from a miss-path fall-through.
- **C-27** `sim_flip_test.go` `serveDetached`: the ledger stamps the anchor's arrival after `Release.gpg`, `indexTransfer` after the gate's quiet clock started at `Release`'s completion (stricter, never laxer than the plan's "the two instants coincide"); say so in its doc.

## cmd/apt-cacher-ultra

- **C-28** `main.go:796`: the gate's `Close` call and its position (right after the signal, before the listeners drain) are untested; every `serveListeners` test runs with the barrier off. Add a smoke test with adoption on and `flip_quiet_period = 10s`, and an AIDEV-NOTE marking the order load-bearing.
- **C-29** `main.go:483` / `internal/freshness/legacy_audit.go:45-59`: A-48 relies on every `RunLegacyTrustAudit` error being prefixed "legacy trust audit:"; state the rule in the callee's doc.
- **C-30** `main.go:1094`: the recovered shutdown-wait panic is logged without a stack, under a non-snake_case event name (`shutdown wait panicked`, documented as it is), and `waitLogged` then returns as if the drain had finished. Add the stack and a test; practically unreachable (`WaitGroup.Wait`).
- **C-31** `flip_wiring_test.go:41-44`: the 16 s case asserts only `err != nil`; pin `errors.Is(err, flipgate.ErrQuietOutOfRange)`.
- **C-32** `flip_wiring_test.go:51-70`: the handler-config test uses `KeepDisplaced = 3` (the `Load()` default), relies on `minimalCfg`'s `GC.Enabled = false` without saying so, and its nil-adopter comment says "adoption disabled" while `Adoption.Enabled` is true.
- **C-33** `main.go:925`, `:1086`, `:1107`: the "5-minute commit timeout" text duplicates the unexported `freshness.commitTimeout`.
- **C-34** `flip_wiring_test.go:101-141` and the B-64 test are not about flip wiring (the A-48 test belongs in `legacy_audit_wiring_test.go`); `waitLogged` has a generic name but hard-codes an adoptions event and detail.
- **C-35** `main.go:367`: the `adoption_flip_quiet_period` startup field (now documented) is pinned by no test, like every field of that line.

## Docs and packaging

- **C-36** `docs/admin-ui-spec.md`: the Recent Adoptions duration now includes the flip barrier (up to 140 s); `log-fields.md` says so only for the metric. (H)
- **C-37** `packaging/config/config.toml.default`: the `max_duration` comment does not mention the barrier, and the `keep_displaced` comment does not mention retained by-hash. (H)

## Simulation budget and flakes

- **C-38** Two phase-B simulations have little or no room under the 5 s `-race` budget: `TestSimScheduler_FiveHundredStallingOneHealthy` (6.15–7.82 s in the final tests review, 6.37 s in the fix wave's run) and `TestSim30Days_RetryDateRulesAndClockSteps` (4.4–5.1 s, then 3.85 s). Phase C does not change them. Coarsen them, never dropping assertions. (B scope; H)
- **C-39** One unexplained failure of `TestPhase2DetachedChaos_AdoptionFlipUnderConcurrency` in a loaded parallel run (final review, concurrency; it did not reproduce in four `-race` reruns or alone). The test wires no gate and runs in real time. Investigate if it recurs. (H)

## Process

- **C-40** Phase C's task commits (1f5d37e, a8eb0cd, 02d4474, 34465fa, 2df0acc, d823746) carry no mutant → failing-test lines, and that evidence lives only in session scratchpads; record a mutant table where it survives (the phase C stamp commit or the overview's phase C row; final review spec R2, task reviews 4, 6–10).
