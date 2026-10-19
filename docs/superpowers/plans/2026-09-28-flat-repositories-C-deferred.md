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

- **C-11** `adoption.go:720`: "strictly increasing per suite" holds only along the current-snapshot chain. When `current_snapshot_id` is cleared (quarantine today, the reaper next), the first-adoption rule ignores the suite's remaining displaced snapshots, whose `adopted_at` may lead the clock, so GC's `keep_displaced` ranking and retained by-hash's newest-first window could misorder. Take `MAX(adopted_at)` over the suite's adopted snapshots as a floor, or narrow the AIDEV-NOTE. (F)
- **C-12** `flip_state_test.go:158`, `:174`, `:194`: `restore()` runs after `commitCandidate` instead of being deferred, and `stubNow` (`gc_test.go:42`) registers no cleanup, so a failing stamp test leaves `nowUnix` stubbed for the rest of the package.
- **C-13** `flip_state_test.go:146-191`: the served-anchor `PutURLPath` setup is written out three times; hoist `putAnchor`.

## internal/freshness


## internal/handler

- **C-21** (narrowed by phase D) only its context fast path remains (it needs new `flipgate` API); the `EXISTS` read landed in phase D, Task 10. Phase E leaves it (no measured gain, and it would touch the barrier's registration race; ruling PE-7). (H)

## cmd/apt-cacher-ultra


## Docs and packaging

- **C-36** `docs/admin-ui-spec.md`: the Recent Adoptions duration now includes the flip barrier (up to 140 s); `log-fields.md` says so only for the metric. (H)
- **C-37** `packaging/config/config.toml.default`: the `max_duration` comment does not mention the barrier, and the `keep_displaced` comment does not mention retained by-hash. (H)

## Simulation budget and flakes

- **C-38** (narrowed by phase E: `TestSim30Days_RetryDateRulesAndClockSteps` was coarsened in phase E, Task 14, to about 3.6 s, every assertion kept at its per-day rate) `TestSimScheduler_FiveHundredStallingOneHealthy` has no room under the 5 s `-race` budget (6.15–7.82 s in phase B's final tests review, 6.37 s in its fix wave's run, 7.9 s in phase E). Coarsen it, never dropping assertions. (B scope; H)
- **C-39** One unexplained failure of `TestPhase2DetachedChaos_AdoptionFlipUnderConcurrency` in a loaded parallel run (final review, concurrency; it did not reproduce in four `-race` reruns or alone). The test wires no gate and runs in real time. Investigate if it recurs. (H)

## Process

