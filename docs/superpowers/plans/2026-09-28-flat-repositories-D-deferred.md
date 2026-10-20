# Flat repositories — phase D deferred minors

Phase D's thirteen task reviews, their first re-reviews and its three final-review lenses (concurrency and failure modes, spec compliance, test strength; 2026-10-19) found these minor issues and triaged them "can wait". None is a correctness defect in shipped behaviour. They were recorded in a git-ignored ledger and in the final-review reports, and are moved here (deduplicated) so they survive.

**How they get done:** every later phase plan includes the items below that sit in files it already touches (cite the item ID in the task); an owning phase in parentheses marks where an item most naturally lands. Phase H ends with a sweep task that verifies each remaining item against the code, fixes it or records why not, and deletes this file.

Left out: everything the phase D final fix wave resolved — CreateSuite's failed-submit release and `deleteSuiteTx`'s scheme scoping pinned (Task 5 minors 1, 2), the onboarding path's caps, refusal, `suite_evicted` line and 5 s bound tested (Task 10 minor 1, Task 9 minor 2's eviction line), `sfGroup.Do` and the eviction hold panic-safe with one creation lifecycle (Task 10 minor 7, Task 6 minor 5), the drop-then-unlock order pinned (Task 7 minor 3), the miss path's backstop before its early returns, bounded under test and stamping unstamped rows (Task 9 minor 1 and the re-review's log-fields row), transient cap refusals (Task 5 minors 3, 4), the I-4 pre-read's cost, the by-hash allowlist isolation (Task 10 minor 3), the version-byte-first row (Task 1 minor 2), the FIFO deadline (Task 6 minor 7), phase D's own noisy tests, the `Handler.Close`/`onPanic`/mark-failure signals, the `VerifyInline` doc (Task 2 minor 1) and the overview's backstop note (Task 13 minor 5); items now carried forward in the overview, which owns them — T1-on-seed on the miss path only and waiters' late registration (E, Task 10 minor 2), unsigned eligibility beside the marker (E), `Gate.Purge` skipping live entries (F) and the two "(a later release)" texts (F, Task 13 minor 4); and items the reviews closed without action: the Unproven triggers tested only together (Task 4 minor 1: both are killed by the handler tests), a zero `RequestedAt` (Task 4 minor 2: killed by the parity and scale tests), the stale `Reconcile` doc (Task 4 minor 6: fixed by 77a2d56), Task 4's method and named-risk notes (checks that found no defect), the re-stamp's cache-level gaps (Task 6 minor 6: covered by the handler parity test), the touch purge promise (Task 6 minor 9: an F carry-forward), Reconcile's lock-map entry (Task 7 minor 1: fixed by 77a2d56), the global-cap WARN trigger (Task 8 minor 1: `TestWarnSuiteCaps_AboveTheGlobalCapOrAtAHostCap`), the sniff.go AIDEV-NOTE about reconcile (Task 2 minor 2: true since Task 3), the event-name rename (Task 9 minor 4: already an H carry-forward), and PD-12's missing definition (fixed by e58d8cf). B-27, B-36, C-12 and C-13 stay in their own files (ruling PD-1), and the two over-budget simulations stay C-38 (after the fix wave, under `-race` alone: 3.8–3.9 s and 6.5–6.6 s). Line numbers are as of the fix wave and will drift.

## internal/aptpath

- **D-3** `sniff_test.go` (`TestSniffFlatIndex`): only `.gz` and `.xz` have negative magic rows; shortening the `.zst` or `.lzma` magic (`sniff.go:123-125`; `.bz2`, `.lz4` alike) survives. Fix: one near-miss row per extension. Can wait: flat indexes are unreachable until G. (G) (Task 1 minor 4)
- **D-4** `sniff.go:192`: `ClassifySuiteFile` (the PD-9 gate and the R-D3 routing point) has no AIDEV-NOTE naming its precondition (`suite` is `SuitePath(path)`), the `""` guard its callers rely on, and that its flat branch is reachable only from tests until G; `SuitePath`'s phase-G AIDEV-NOTE does not point here. (G) (Task 1 minor 5)

## internal/gpg

- **D-6** `failclosed_test.go`: the accept leg covers only an LF body: `DecodableClearsign`'s mixed-ending output (an LF marker line, then CRLF lines) is not decoded and verified end to end, and the leg does not assert `aptpath.SniffInRelease` (the "handler agrees" direction is checked only for refusals). Coverage only. (Task 2 minor 4)

## internal/simtest

- **D-7** `clearsign.go:17-21`: the rule "no line of text may begin with `-`" is not enforced; such text silently yields an envelope whose decoded plaintext differs (a `- ` line is unescaped, a signature marker ends the message). The doc's "text, less its final newline" also omits that the decoder trims trailing blanks from every line. Fix: panic on a violating `text`. (Task 3 minor 3)

## internal/cache

- **D-9** `cache_test.go` (a gofmt drift file: edit by hand): `TestInsertCandidateSnapshot_RejectsDanglingBlobFK`, `_RejectsBothModes` and `_RejectsAllNull` go through `InsertTrackedCandidateForTest` and assert only `err != nil`, so an error from its `EnsureSuiteForTest` step would satisfy them. Fix: call `InsertCandidateSnapshot` directly. (Task 4 minor 4)
- **D-10** `adoption_failure.go:144`: B-4's `COALESCE` has no test (the only reason-less row has count 0, and `RecordAdoptionFailure` refuses an empty reason). Fix: a raw-SQL row with a count above 0 and a NULL reason. (Task 4 minor 5)

## internal/freshness

- **D-26** `reconcile_r1_test.go` (`DecodesAptsMarkerLine`) never asserts that `bytes.Replace` matched: if the encoder's first line ever ended in "\r\n", the test would pass without exercising the trailing-blank path. Fix: a `bytes.Equal(blanks, signed)` guard. (Task 3 minor 5)
- **D-27** `reconcile_test.go` (`RejectsCorruptedAnchor`) asserts only `err != nil` while its comment says only the hash check can refuse it; assert the error contains "hash mismatch". (Task 3 minor 6)

## internal/handler

- **D-28** `phase2_handler_test.go`: the local `staleBody` shadows the package helper `staleBody(path)` (`handler_test.go`); rename it, e.g. `staleAnchor`. (Task 3 minor 1)
- **D-29** `fetch_seam_test.go`: the `recFetcher` type doc says it "answers every request with body", untrue when `inRelease` is set (only the field comment says so). (Task 3 minor 2)
- **D-30** `phase2_handler_test.go` (`TestServeHTTP_AdoptedSuiteSnapshotMemberDBError_FailsClosed`): its upstream `/InRelease` body `"attacker bytes"` stays plain text with no PD-2 comment (no effect: the test asserts zero upstream calls; its sibling was wrapped). (Task 3 minor 4)
- **D-31** Untested: the recover inside the rejection sweep's `expire` itself (its `onPanic` wiring and `Handler.Close` stopping it are pinned since the fix wave), and `New` refusing a negative `MaxSuites`. (Task 9 minor 2, narrowed)
- **D-32** `suite_rows.go`: `cachedAnchors` and `markCachedSignedAnchors` repeat one loop (iterate, skip `except`, `LookupURL`, classify, `readHead`, sniff); the in-transaction mark and the backstop must agree on what counts as signed. One helper returning `(row, signed, err)` would keep them together. `runFetch` also builds `suiteSeed` twice with identical arguments. Can wait: the copies agree today. (G: if the sniff rules change) (Task 9 minor 3)

## cmd/apt-cacher-ultra

- **D-46** `suite_caps_wiring_test.go` (`startupLogs`): on its timeout path it cancels and calls `t.Fatalf` without draining `done`, so `serveListeners` can outlive the test and write to `logs` afterwards. Fix: `cancel(); <-done` first, or a draining `t.Cleanup`. Failure path only. (Task 8 minor 3)
- **D-48** `legacy_audit_wiring_test.go:162`: `cache.Open(ctx, cacheDir, nil)` prints 14 migration INFO lines under `TestServeListeners_LegacyAuditErrorHasOnePrefix`; pass `silentBuildLogger()`. (Task 12 minor 5)
- **D-49** `shutdown_drain_test.go`: its start/poll/stop loop repeats `startupLogs`' (`suite_caps_wiring_test.go`) almost line for line, and neither `http.Get` has a timeout, so a hung request is not bounded by the 15 s deadline. Fix: reuse `startupLogs`, or one shared helper with a timed `http.Client`. Test-only. (Task 12 minor 6)

## internal/admin

- **D-50** `status_template.go`: the Suites empty-state body ("Suites populate after the first adoption cycle.") differs from `docs/admin-ui-spec.md`'s copy, which the fix wave corrected ("A suite is tracked once a client requests its InRelease through the proxy (with adoption enabled, also its Release)"); the template's wording is wrong too (adoption is not needed for a row). Align the template with the spec. (H) (Final fix wave, FW-8)

## Docs and spec

- **D-52** `docs/configuration.md` (the detached-only paragraph) and spec C3 (the Phase-1 difference): "no longer checked for changes or shown on the status page" holds only for a detached-only repository first seen after the upgrade; a row an earlier release created stays tracked (`servedSuite` touches it on every `Release` serve, so neither eviction nor F's reaper removes it). Suggested: "is not tracked when first seen; one an earlier release already tracks stays tracked". (H) (Task 13 minor 3)
- **D-55** Overview: the *(D)* H item (the prose WARN names) sits under the "PR #8 parity" section's **H:** block, outside the Carry-forwards section, whose ownership rule and *(D)* legend therefore do not cover it; add an **H:** block to Carry-forwards. (H) (Task 13 minor 8)

## Test output and flakes

- **D-56** Pre-existing test-output noise outside phase D's tests (phase D's own are silent since the fix wave): `schema_migrating`/`schema migrated` INFO lines in `internal/admin` (~950), `internal/freshness` (682 non-runner lines remain under `-v`, including twelve more `cache.Open(…, nil)` in `adoption_test.go`), `internal/gc` (~420) and `cmd` (~42, D-48); about 36 WARN lines from adoption, repair and hot-prefetch tests (e.g. `TestAdopter_HappyPath` `source_parse_failed`, `TestRunHotPrefetch_HashMismatch`); metrics series-cap WARNs. (H) (Task 13 minor 9)
- **D-57** `TestProfilingRoutesOptInAndPrivate` (`internal/admin`): an intermittent `TempDir` cleanup failure (the WAL file still open at cleanup); pre-existing, not reproduced in phase D's runs. (H) (ledger; reviser's note)
- **D-58** `TestServe_HTTPSUpstream_InvalidCert_Returns502` (`cmd/apt-cacher-ultra/upstream_invalid_cert_test.go`) prints TLS handshake errors in the test output (the `internal/fetch` tests too); pre-existing. Silence them, e.g. by giving the test server a discarding `ErrorLog`. (H) (ledger)

- **D-60** `internal/cache/open_cache_test.go` (`TestOpenCache_LogsNothing`): it asserts on its buffer inside the subtest, before the `Cache.Close` cleanup `openCache` registers, so a line `Close` logs is never caught. Assert after the cleanups (e.g. close explicitly, or check in an outer `t.Cleanup`). Test-only. (Task 12 re-review r1 minor; final re-review)

## Process

