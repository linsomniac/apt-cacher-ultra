# Flat repositories — phase B deferred minors

Phase B's per-task reviews and its three final-review lenses (concurrency, spec compliance, test strength; 2026-10-18) found these minor issues and triaged them "can wait". None is a correctness defect in shipped behaviour. They were recorded in a git-ignored ledger and in the final-review reports, and are moved here (deduplicated) so they survive.

**How they get done:** every later phase plan includes the items below that sit in files it already touches (cite the item ID in the task); an owning phase in parentheses marks where an item most naturally lands. Phase H ends with a sweep task that verifies each remaining item against the code, fixes it or records why not, and deletes this file. Items the phase B final fix wave resolved are left out, and so are items carried forward by name in the overview's "Carry-forwards (from phase A and B reviews)" (the `membersDone` wiring test, `signedCurrent` for repair/reconcile, the stale `signed=false` waiter, the duplicated check blocks and default arms, `contentKey` for unsigned, `classifyAdoptionReason`'s refusal default). Line numbers are as of the fix wave and will drift.

## internal/cache

- **B-1** `queries.go:207` AIDEV-NOTE rationale: a column-count mismatch fails `Scan` loudly; the silent hazard it should name is a same-position edit or reorder.
- **B-2** `adoption_failure.go:133` `RescheduleFailingAdoptions` doc: `offset` runs on the writer goroutine per row, possibly after the call returned on a cancelled context, so the caller must pass a goroutine-safe source (production's `math/rand/v2` is).
- **B-3** `adoption_failure_test.go:217`: the restart test reschedules one row, so a hoisted `offset()` shared by all rows would pass; add a second suite with a counting offset (`TestSimRestartSpread`'s buckets already catch it end to end).
- **B-4** `adoption_failure.go:141`: `adoption_fail_reason NOT IN (…)` skips NULL reasons; `COALESCE` would make the invariant explicit (unreachable: empty reasons are rejected on write).
- **B-5** `cache_test.go:2139`: `TestCommitAdoption_PreservesExistingSuiteFreshnessColumns`'s name half-contradicts its body (it now asserts the etag is replaced).
- **B-6** `flip_state_test.go:48-49` dereference `LastCheckAt`/`LastSuccessAt` without a nil check.
- **B-7** `adoption.go:30`: the `ErrSnapshotNaturalKeyAdopted` doc should say it arrives wrapped in `*NaturalKeyAdoptedError`.
- **B-8** No test asserts the validators on `CommitAdoption`'s INSERT (no-conflict) branch of Step 6. (D: moot once Step 6 is UPDATE-only.)

## internal/freshness — Date parser (`release_date.go`)

- **B-9** `release_date.go:69`/`:27`: `strings.Fields`/`TrimSpace` split on Unicode whitespace where apt's `isspace` is ASCII-only (an NBSP-separated date parses differently; unrealistic).
- **B-10** `release_date.go:27`: `TrimSpace(key)` accepts `Date :`, which apt does not (same convention as `parseAcquireByHash`).
- **B-11** `release_date.go`'s `parseAptDate` default clause shadows `ok`.
- **B-12** `release_date.go:221`: `aptStoi` is a pure alias of `aptSignedInt`.
- **B-13** `releaseDate` reads `Date` on any top-level line; apt reads only the first paragraph (Release files are one paragraph in practice).
- **B-14** `release_date.go:57-62` AIDEV-NOTE attributes the RFC 850 day > 65535 divergence to `aptDigits`; it is the `aptStoi`/unsigned-short path.

## internal/freshness — outcomes, keys, records

- **B-15** `metrics.go:242` comment: `timed_out` must stay ahead of the member sentinels and is safe only because member-fetch errors are flattened with `%v`; also note that a commit-timeout error is classified `timed_out`.
- **B-16** `retry_test.go:10`: the detached content key is not pinned to its exact formula (`sha256(sha256(rel) || sha256(sig))`); a swapped order would pass, and keys persist across restarts (one extra attempt after an upgrade).
- **B-17** `retry.go:52`: `jitterDelay` does not clamp `u` outside [0, 1) (callers pass `rand.Float64`).
- **B-18** `adoption_failure.go:127`: `recoverAdoption` runs `reportPanic`, metrics and the ring after `recover()` with no second boundary; a panicking slog handler would escape (pre-existing class; `checkLockedSafe` got nested recovers in the fix wave).
- **B-19** `adoption_failure_test.go`: no test for a reason change mid-streak (the `adoption_deferred` re-log), `AdoptionFailLastMod`, the retry attributes, or a detached-form record.
- **B-20** `recordAdoptionFailure`'s shutdown check-then-act can log `adoption_failure_record_failed` "context canceled" at WARN when shutdown races a failure (log noise only).
- **B-21** `adoption_failure_test.go` `TestAdoptionFailure_PanicIsRecorded`: `evs[len(evs)-1]` relies on a single event (`Ring.Snapshot` order).
- **B-22** `freshness_test.go` `fakeCache.RecordAdoptionFailure` succeeds for unknown suites (production is UPDATE-only, pinned by `TestRecordAdoptionFailure_IsUpdateOnly`).
- **B-23** `freshness.go:1682` `applyClamp` runs twice on the inline → detached fallback of a first-ever suite (a double `acu_clock_steps_detected_total` count and a redundant write); one-line fix: set `cur.AdoptionNextRetryAt = &g.clampTo` after `SetAdoptionNextRetry`.
- **B-24** `retry_gate_test.go` `TestGate_DueRetryHasNoValidators` cannot tell "" from the anchor's validators (the rig suite has none); add a leg on an adopted suite past `next_retry_at`.
- **B-25** One extra `Release.gpg` GET per deferred detached check when upstream ignores validators (accepted cost; recorded so E's shared tail keeps it in mind). (E)
- **B-26** `freshness.go:613` `rescheduleFailingAdoptions`: comment why the `c.adopter == nil` guard is safe.
- **B-27** `restart_reset_test.go`: the restart test exercises only `rand = 0.5`; a `rand = 0` leg would pin the lower bound.

## internal/freshness — check path, repair, reconcile

- **B-28** A degraded snapshot's Release is parsed twice per heal attempt (`reconcile.go:122` `reconcilePending`, then `ReconcileSnapshot`; perf only, bounded by the cooldown).
- **B-29** `capacity_test.go:136`, `:212`: "B did not start" is proven by a 150 ms wall-clock window; assert `changed_queued` / admission usage instead.
- **B-30** `freshness.go:533`, `:594`: `adoption_reconcile_failed` is emitted at the decision and at the heal with identical fields; add a `stage` attribute.
- **B-31** `freshness.go:544`: a repair deferred for want of a slot is logged at DEBUG only, with no counter.
- **B-32** Under steady client traffic a degraded suite re-runs its repair every cooldown, and each run may take the reserved slot for up to `max_duration` if a member stalls; weigh with E's `signedCurrent` change. (E)
- **B-33** The `Release.gpg` Conditional on `fetchCtx` is untested (reverting it to `ctx` survives); add a stall-on-gpg synctest when E moves the shared tail (D in the latency bound depends on it). (E)
- **B-34** `recordUnknownEnd`'s `freshnessCheckTotal.Inc("failed")` is asserted nowhere. (E)
- **B-35** The inline unexpected-status default arm's ERROR line lacks the `form` attribute (the detached arm has it). (E)
- **B-36** `check_streak_test.go:23`, `:43`, `:203`, `:311`: four overlapping checker constructors.
- **B-37** A deterministic repair or reconcile panic is logged, not recorded (plan deviation 7), so it re-runs on later unchanged checks with an ERROR line each; revisit if a record keyed on the snapshot (not the content) becomes available. (E)

## internal/freshness — scheduler and admission

- **B-38** `scheduler.go:326` `suiteDue` re-detects (and counts) a future `last_success_at` on every tick; only `acu_clock_steps_detected_total`'s rate is affected (`docs/log-fields.md` now says it counts detections).
- **B-39** `scheduler_test.go:299` `TestRun_FourWorkersOneCheckPerHost` does not exercise the in-flight skip (needs 3 hosts × 3 suites or a slow host); `TestSchedQueues_RoundRobinOneInFlightPerHost` covers pop's skip.
- **B-40** `scheduler.go:291` `prune` resets `q.next` to 0 when the next host is pruned (bounded one-turn skew).
- **B-41** `scheduler.go:552`: the tick's sort comparator builds `suiteKey` strings per comparison (perf nit).
- **B-42** `scheduler_test.go:634` the busy-arm re-park test asserts only `it.waiting`; also assert the previous `signed` is kept.
- **B-43** Nothing pins that a non-waiting scheduler check (no pending change) honours the cooldown (`checkRecovered` with `waiting = false` inside the cooldown → `skipped`).
- **B-44** `scheduler.go:491` `checkWorker`'s last-resort recover leaves no trace; add a counter (not via the logger).
- **B-45** `scheduler.go:424`: the wake-versus-done race can spend a wake on an in-flight item, so its re-offer waits for the next wake or tick (bounded by one tick; documented). A release-generation counter would let an offer whose slot was released after dispatch park unheld.
- **B-46** `scheduler.go:178` `park`: every re-parked waiter goes to index 0, so two lost or busy offers in a row on one host invert the waiters' order (final review C M-3; kept deferred by ruling). Give each waiter a waiting-since sequence number and re-insert sorted.

## internal/freshness — simulations

- **B-47** `sim_sched_test.go:237`: the latency simulation's goroutine slack is 16, loose for its load (the capacity simulation uses 8).
- **B-48** The latency simulation does not seed the `rand` seam (spec §7 "fixed seeds"; it records no adoption failures today). Seed it when touched.
- **B-49** The pacing simulation never measures a +2 h forward clock step (spec §7 lists ±2 h; +30 d covers "every suite due at once").
- **B-50** `sim_sched_test.go:217`: the repair assertion (≥ 2) is looser than its message.
- **B-51** `simMembers`' `delay` and `started` maps are written without the lock (always before the scheduler starts; the doc states the rule).
- **B-52** The phase B plan's Task 12 Step 1 text is stale against the strengthened simulations (reference only).
- **B-53** The 500-suite simulation has no adopter, so no persistent repair or changing-candidate contention (overview DoD); add a changing candidate on host `a`.
- **B-54** `sim_retry_test.go` Lim1 pins jitter at 0.5, so ±25 % is never exercised end to end (the low end gives about 37 attempts a day, not "about 30").
- **B-55** `TestSimRestartSpread`'s `retried` counts upstream calls, not adoption attempts (the tick buckets give it teeth).
- **B-56** The 30-day simulation's clock-step phases prove little about retry or Date handling (no active record during the step).
- **B-57** The 30-day simulation's untrusted phase has no attempt floor (183 measured; Lim1 has 18..30).
- **B-58** The flapping-mirror simulation does not pin the rollback guard on its own (the cost is now pinned by `TestGate_RefusalStormCostsNothing`), and its samples land on the tick grid (harmless at a 30-minute refresh).

## cmd/apt-cacher-ultra

- **B-64** `main.go` shutdown: `freshChecker.WaitForAdoptions()` can wait up to `commitTimeout` for a running commit (its comment now says so); log an INFO line when the drain takes long, so a slow stop is explained in the journal.

## internal/admin

- **B-59** `suites_status_test.go`: no row with 0 < lag < 3600 (a `LagWarn = lag > 0` regression would pass), and the JSON golden pins only zero values (`anchor_form ""` beside `current_snapshot_id 42`).
- **B-60** `status_test.go:261` `suites_healthy_no_lag` fixture still sets `InReleaseChangeSeenAt`.
- **B-61** `status.go` `adoptionStatus` dereferences `AdoptionFailReason` without a nil check (its caller guards).
- **B-62** `suites_status_test.go` `htmlRowStart` slices `html[:offset]` and panics on -1 (still a test failure).
- **B-63** The status page cannot say why a suite with failing checks (stalling upstream, backoff up to 1 h) or one waiting for a slot is not updating: no row state, an empty Adoption cell, `check_fail_count` only in the JSON (final review T M9; kept deferred by ruling). Show "checks failing ×k · next check in …" in the Adoption cell.
