# Flat repositories — phase B deferred minors

Phase B's per-task reviews and its three final-review lenses (concurrency, spec compliance, test strength; 2026-10-18) found these minor issues and triaged them "can wait". None is a correctness defect in shipped behaviour. They were recorded in a git-ignored ledger and in the final-review reports, and are moved here (deduplicated) so they survive.

**How they get done:** every later phase plan includes the items below that sit in files it already touches (cite the item ID in the task); an owning phase in parentheses marks where an item most naturally lands. Phase H ends with a sweep task that verifies each remaining item against the code, fixes it or records why not, and deletes this file. Items the phase B final fix wave resolved are left out, and so are items carried forward by name in the overview's "Carry-forwards (from phase A and B reviews)" (the `membersDone` wiring test, `signedCurrent` for repair/reconcile, the stale `signed=false` waiter, the duplicated check blocks and default arms, `contentKey` for unsigned, `classifyAdoptionReason`'s refusal default). Line numbers are as of the fix wave and will drift.

## internal/cache


## internal/freshness — Date parser (`release_date.go`)

- **B-9** `release_date.go:69`/`:27`: `strings.Fields`/`TrimSpace` split on Unicode whitespace where apt's `isspace` is ASCII-only (an NBSP-separated date parses differently; unrealistic).
- **B-10** `release_date.go:27`: `TrimSpace(key)` accepts `Date :`, which apt does not (same convention as `parseAcquireByHash`).
- **B-11** `release_date.go`'s `parseAptDate` default clause shadows `ok`.
- **B-12** `release_date.go:221`: `aptStoi` is a pure alias of `aptSignedInt`.
- **B-13** `releaseDate` reads `Date` on any top-level line; apt reads only the first paragraph (Release files are one paragraph in practice).
- **B-14** `release_date.go:57-62` AIDEV-NOTE attributes the RFC 850 day > 65535 divergence to `aptDigits`; it is the `aptStoi`/unsigned-short path.

## internal/freshness — outcomes, keys, records

- **B-27** `restart_reset_test.go`: the restart test exercises only `rand = 0.5`; a `rand = 0` leg would pin the lower bound.

## internal/freshness — check path, repair, reconcile

- **B-36** `check_streak_test.go:23`, `:43`, `:203`, `:311`: four overlapping checker constructors.

## internal/freshness — scheduler and admission


## internal/freshness — simulations

- **B-52** The phase B plan's Task 12 Step 1 text is stale against the strengthened simulations (reference only).

## internal/admin

- **B-59** `suites_status_test.go`: no row with 0 < lag < 3600 (a `LagWarn = lag > 0` regression would pass), and the JSON golden pins only zero values (`anchor_form ""` beside `current_snapshot_id 42`).
- **B-60** `status_test.go:261` `suites_healthy_no_lag` fixture still sets `InReleaseChangeSeenAt`.
- **B-61** `status.go` `adoptionStatus` dereferences `AdoptionFailReason` without a nil check (its caller guards).
- **B-62** `suites_status_test.go` `htmlRowStart` slices `html[:offset]` and panics on -1 (still a test failure).
- **B-63** The status page cannot say why a suite with failing checks (stalling upstream, backoff up to 1 h) or one waiting for a slot is not updating: no row state, an empty Adoption cell, `check_fail_count` only in the JSON (final review T M9; kept deferred by ruling). Show "checks failing ×k · next check in …" in the Adoption cell.

## Final fix-wave residuals (scoped re-review of 9a2b22b..e361704)

