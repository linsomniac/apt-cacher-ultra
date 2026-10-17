# Flat repositories — phase A deferred minors

Phase A's per-task and final reviews (2026-10-11, Fable max) found these minor issues and deferred them; all three final-review lenses triaged them "stay deferred". None is a correctness defect in shipped behaviour. They were recorded in a git-ignored ledger and moved here on 2026-10-17 so they survive.

**How they get done:** every later phase plan includes the items below that sit in files it already touches (cite the item ID in the task). Phase H ends with a sweep task that verifies each remaining item against the code, fixes it or records why not, and deletes this file. Items the phase A final fix wave (d57d473..fa30e7f) already resolved are left out; line numbers are as of fa30e7f and will drift.

## internal/wallclock

- **A-1** `wallclock_test.go:27`: `restore()` is called inline, not deferred, so a failing assertion leaks the −2 h offset into later tests.
- **A-2** No guard test that production code never calls `wallclock.SetOffsetForTest`; the doc should say restores run LIFO.
- **A-3** Handler and GC wallclock stamps (`handler.go:680,868,1523,1793`; `gc.go:276,362`) are not distinguished from `time.Now` by any test.

## internal/aptpath and internal/proxy

- **A-4** `suite.go:19-20` comment overclaims "matching the old greedy regex"; add `{"/a/dists/b/dists/c", "/a/dists/b"}` to `TestSuitePath_Dists`.
- **A-5** `suite.go:25-26` AIDEV-NOTE says "raw path"; the handler strips from `req.Path`, the same string `SuitePath` received. Reword.
- **A-6** `suite_test.go` lacks a space-byte case and a trailing-slash full-path case.
- **A-7** Spec C2's canonicality predicate lists only `#`, `?`, `%` and `\`; the code also refuses control bytes. Amend the spec text.
- **A-8** `TestCacheDoesNotImportProxy` checks direct imports only; comment the limitation.
- **A-9** `encode_test.go:36`: the all-bytes loop starts at 1, skipping NUL (it round-trips). Start at 0 or comment.
- **A-10** `proxy.go:172-173`: the control-byte invariant has a plain comment; promote it to an AIDEV-NOTE.
- **A-11** `encode_test.go:111-114`: the opt-in fixture test's failure message omits got/want.
- **A-12** `encode.go:60-67`: the fallback is silent (plan deviation 2; provably unreachable). Informational.
- **A-13** (pre-existing, out of scope) HTTPS-magic host (`proxy/url.go:122`) has no charset validation; now strictly safer (escaped, then `url.Parse` fails). Follow-up candidate.
- **A-14** `composers_test.go:16`: the guard regex only catches `"://" +`; it misses `"https://"+host` and `Sprintf("%s://…")`. `connect.go:591` is a legitimate exemption if widened.
- **A-15** `hot_set.go:63` has no direct unit test (`hot_prefetch_test.go` is a gofmt-drift file).
- **A-16** `SniffReleaseGPG` accepts an indented armor header (safe direction) and rejects gpg-tolerated leading non-header lines (exotic); note in the spec C3 table wording.
- **A-17** `sniff_test` negatives should pin the `0x8C` and `0xC3` upper bounds.

## internal/cache

- **A-18** (pre-existing) Cache tests emit `schema_migrating`/`schema migrated` INFO lines; test output is not pristine.
- **A-19** `schema_v7_test.go:216-218` swaps `migrationGoSteps[6]`; add a "no `t.Parallel`" comment.
- **A-20** `sqlclock_test.go:16-17`: `CURRENT_TIMESTAMP` is redundant with the `CURRENT_TIME` substring.
- **A-21** The schema test asserts `idx_package_hash_flat_filename` exists, not that it is partial.
- **A-22** No test asserts `GetSuiteSnapshot` returns `FormInline` for an inline row.
- **A-23** `CommitAdoption` Step 7a's `MarkerBase` call is unexercised (no `/r/.` suite in tests); add one commit case.
- **A-24** `MarkSignedSeenBatch`'s multi-mark and 500-row chunk loop are untested.
- **A-25** `signed_seen.go:55-57`: per-row error unprefixed.
- **A-26** `cache/legacy_audit.go:235` re-filters by path suffix what the SQL `IN` already guarantees.
- **A-27** `cache/legacy_audit.go:48` doc says "missing", should say "missing or empty".

## internal/freshness

- **A-28** Stale doc comments on `detectForm` (`freshness.go:514-515`) and `priorAdoptionForm` (`adoption.go:1498-1503`) still say "derived from hash columns".
- **A-29** `gc_urlpath.go:274-275`: the `AIDEV-TODO(phase E)` sits as an SQL `--` comment inside the query string; move it to a Go comment. (Phase E resolves the TODO itself.)
- **A-30** (pre-existing) `TestCheck_DetachedKnownSuite_AdoptsOnReleaseChange` emits INFO/WARN lines.
- **A-31** `adoption.go:86-89`: the sentinel comment encodes a load-bearing invariant but lacks the `AIDEV-NOTE:` prefix.
- **A-32** `reconcile.go:181-191`: AIDEV-NOTE wording ("unsigned regions") and a >100-column line.
- **A-33** `clearsignForTest` generates an RSA key per call (slow under `-race`); use an EdDSA config.
- **A-34** No CRLF-envelope or `Hash:`-header injection case in the reconcile tests (covered structurally; the legacy-audit tests have both).
- **A-35** `panic_recovery_test.go:213-225`: adopters built without a logger (default-logger noise under `-v`).
- **A-36** `bumpFailedCheck` (`freshness.go:519-528`) swallows `Get`/`PutSuiteFreshness` errors silently; add a Warn like `checkLocked`'s. (Phase B rewrites this path.)
- **A-37** Three "log a panic, stack once" implementations (`Checker.reportPanic`, the handler's `atomic.Bool`, heartbeat with none); consider one helper. Heartbeat never logs a stack.
- **A-38** `panic_recovery_test.go:244-246`: a successful `TryLock` is never unlocked.
- **A-39** `TestTick_FutureLastSuccessAtIsDue` does not assert the scheduler-path `clockStep` fired (deleting `scheduler.go:118` stays green). (Phase B rewrites the scheduler.)
- **A-40** The clock-step test does not assert `LastSuccessAt` healed to now.
- **A-41** No tolerance-edge cases (now+2 fresh, now+3 stale) in `TestStaleSuiteCounts`.
- **A-42** `acu_clock_steps_detected_total` counts detections, not steps; say so in the docs.
- **A-43** `clockStep` could live in `clockstep.go` (`freshness.go` is over 1,150 lines).
- **A-44** Smoke test asserts 358..362 although exactly 360 is safe; tighten to `== 360`.
- **A-45** `simtest.StaticConditional`'s doc says Body only on 200 but returns it for any status.
- **A-46** No guard that non-test code never imports `internal/simtest` (which imports `testing`).
- **A-47** `legacy_audit.go`: `res.Signed++` counts `FormUnsigned` keeps (after phase E re-queues); rename to `Kept` or skip unsigned.
- **A-48** Doubled "legacy trust audit:" error prefix (`RunLegacyTrustAudit` and `main.go:480`).
- **A-49** The marker pass silently skips tracked anchors whose pool file is missing or unreadable; add a Debug line.

## internal/gpg

- **A-50** The begin-marker literal is duplicated (`verifier.go:288` and `:493`); hoist to a const.
- **A-51** (pre-existing) `requireBareClearsigned` errors (`verifier.go:500,508`) are plain `errors.New` and classify as `crypto_verify_failed`; wrap with `ErrMalformedClearsign`.
- **A-52** `failclosed_test.go:98`: the `GetSuiteFreshness` check passes vacuously on any error.
- **A-53** No test for `VerifyDetached`'s "keyring is empty" message (`verifier.go:420-421`).

## internal/handler

- **A-54** `handler.go:172`: the `cfg.Fetch` nil check is now an interface compare (a typed-nil `*fetch.Client` would pass); add an AIDEV-NOTE.
- **A-55** `fetch_seam_test.go:84` comment says "Review Focus 2" but encoding is focus item 1.
- **A-56** `handler.Fetcher` vs `freshness.Fetcher`/`AdoptionFetcher` naming overlap. Informational.

## Docs

- **A-57** `configuration.md:275-278` pre-flight wording: it also refuses for an unreadable pool, or a pool holding none of the queued files (≥ 2). Verify against the current text, which the final fix wave partly reworded.
