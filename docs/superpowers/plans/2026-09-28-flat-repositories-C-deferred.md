# Flat repositories — phase C deferred minors

Phase C's eleven task reviews and its three final-review lenses (concurrency, spec compliance, test strength; 2026-10-18) found these minor issues and triaged them "can wait". None is a correctness defect in shipped behaviour. They were recorded in a git-ignored ledger and in the final-review reports, and are moved here (deduplicated) so they survive.

**How they get done:** every later phase plan includes the items below that sit in files it already touches (cite the item ID in the task); an owning phase in parentheses marks where an item most naturally lands. Phase H ends with a sweep task that verifies each remaining item against the code, fixes it or records why not, and deletes this file.

Left out: everything the phase C final fix wave resolved (the phase-A wake test, the first-adoption skip under `Close`, the Last-Modified gaps, the Guarantee premise, the `timed_out` gloss, the operator-doc and AIDEV-anchor inaccuracies, the trace premise and its comment, the handler-level cancel test, the root-level by-hash test, the site-5 heartbeat comment, the startup-field and `shutdown wait panicked` documentation, the `Close` test's barrier-event assertion, the `keep_displaced = 0` and "30 s or longer" doc wording, `packageValidation`'s doc), and items the reviews closed without action: `Key` vs `suiteKey` (pinned by `TestSuiteKey_IsTheFlipGateKey`), the expiry timer's dropped panic value (an AIDEV-NOTE on `endHoldLocked` now states its must-not-panic invariant), "Done after the write" (a Done-before-write mutant fails the busy-suite simulations' exact commit instant and the slow-reader simulations), the floor's snapshot id and the Last-Modified-after-Date lead (both now in `anchorModTime`'s doc), and the phase-2 inline chaos assertion that cannot fail on its own (its AIDEV-NOTE says so). Line numbers are as of the fix wave and will drift.

## internal/flipgate


## internal/simtest and the model simulations

- **C-5** `simtest/aptclient.go:92`, `:103-112`: two ledger properties are pinned by no test — `held: heldBefore` (an anchor's own hold must not shorten its index's window) and recording `gens` only inside the window (`AnchorGens` is the "clients crossed the flip" proof). Add an update whose anchor is held 5 s and then followed by an 11 s filler, assert its index is not checked, and assert `AnchorGens` stays empty after out-of-window updates.
- **C-6** (checked by phase F, which changes `gate_test.go`, where `busyReaders` lives, but none of the files the fix belongs to: `flipgate/sim_test.go`, simtest and the handler tests) `flipgate/sim_test.go` `clients` and `recordHeld` repeat `gate_test.go`'s `busyReaders` and are copied verbatim as the handler's `flipReaders` and `recordAnchorHeld`; export a periodic-client spawner and `RecordHeld` from simtest.
- **C-7** `flipgate/sim_test.go` `clients`: client ids collide across groups running at once (the loop index), so `Straddle.Client` is ambiguous in a failure message; add an id-base parameter.
- **C-8** `simtest/aptclient.go` docs: say that `RunUpdate` is safe for concurrent use and blocks for the whole update (in real time outside a bubble), and that `AnchorGens` sorts as strings ("10" before "9").

## internal/config

- **C-9** `config_test.go:917-920`: the `flip_quiet_period` rejection loop checks only `err != nil`, not that the error names the key, and `"-1s"` may be rejected by `Duration.UnmarshalText` rather than `Validate`. (H)

## internal/cache

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

- **C-38** (narrowed by phase E: the retry simulation, now `TestSimRetry_LongRunDateRulesAndClockSteps` (ruling PE-9; about 20 virtual days), was coarsened in phase E, Task 14, to about 3.6 s, every assertion kept at its per-day rate) `TestSimScheduler_FiveHundredStallingOneHealthy` has no room under the 5 s `-race` budget (6.15–7.82 s in phase B's final tests review, 6.37 s in its fix wave's run, 7.9 s in phase E). Coarsen it, never dropping assertions. (B scope; H)
- **C-39** One unexplained failure of `TestPhase2DetachedChaos_AdoptionFlipUnderConcurrency` in a loaded parallel run (final review, concurrency; it did not reproduce in four `-race` reruns or alone). The test wires no gate and runs in real time. Investigate if it recurs. (H)

## Process

