# Flat repositories — phase A deferred minors

Phase A's per-task and final reviews (2026-10-11, Fable max) found these minor issues and deferred them; all three final-review lenses triaged them "stay deferred". None is a correctness defect in shipped behaviour. They were recorded in a git-ignored ledger and moved here on 2026-10-17 so they survive.

**How they get done:** every later phase plan includes the items below that sit in files it already touches (cite the item ID in the task). Phase H ends with a sweep task that verifies each remaining item against the code, fixes it or records why not, and deletes this file. Items the phase A final fix wave (d57d473..fa30e7f) already resolved are left out; line numbers are as of fa30e7f and will drift.

## internal/wallclock

- **A-1** `wallclock_test.go:27`: `restore()` is called inline, not deferred, so a failing assertion leaks the −2 h offset into later tests.
- **A-2** No guard test that production code never calls `wallclock.SetOffsetForTest`; the doc should say restores run LIFO.

## internal/aptpath and internal/proxy

- **A-4** `suite.go:19-20` comment overclaims "matching the old greedy regex"; add `{"/a/dists/b/dists/c", "/a/dists/b"}` to `TestSuitePath_Dists`.
- **A-5** `suite.go:25-26` AIDEV-NOTE says "raw path"; the handler strips from `req.Path`, the same string `SuitePath` received. Reword.
- **A-6** `suite_test.go` lacks a space-byte case and a trailing-slash full-path case.
- **A-8** `TestCacheDoesNotImportProxy` checks direct imports only; comment the limitation.
- **A-9** `encode_test.go:36`: the all-bytes loop starts at 1, skipping NUL (it round-trips). Start at 0 or comment.
- **A-10** `proxy.go:172-173`: the control-byte invariant has a plain comment; promote it to an AIDEV-NOTE.
- **A-11** `encode_test.go:111-114`: the opt-in fixture test's failure message omits got/want.
- **A-12** `encode.go:60-67`: the fallback is silent (plan deviation 2; provably unreachable). Informational.
- **A-13** (pre-existing, out of scope) HTTPS-magic host (`proxy/url.go:122`) has no charset validation; now strictly safer (escaped, then `url.Parse` fails). Follow-up candidate.
- **A-14** `composers_test.go:16`: the guard regex only catches `"://" +`; it misses `"https://"+host` and `Sprintf("%s://…")`. `connect.go:591` is a legitimate exemption if widened.
- **A-15** `hot_set.go:63` has no direct unit test (`hot_prefetch_test.go` is a gofmt-drift file).

## internal/cache

- **A-19** `schema_v7_test.go:216-218` swaps `migrationGoSteps[6]`; add a "no `t.Parallel`" comment.
- **A-20** `sqlclock_test.go:16-17`: `CURRENT_TIMESTAMP` is redundant with the `CURRENT_TIME` substring.
- **A-21** The schema test asserts `idx_package_hash_flat_filename` exists, not that it is partial.
- **A-26** `cache/legacy_audit.go:235` re-filters by path suffix what the SQL `IN` already guarantees.
- **A-27** `cache/legacy_audit.go:48` doc says "missing", should say "missing or empty".

## internal/freshness


## internal/gpg


## internal/handler

- **A-55** `fetch_seam_test.go:84` comment says "Review Focus 2" but encoding is focus item 1.
- **A-56** `handler.Fetcher` (`handler.go`) vs `freshness.Fetcher` (`freshness.go`)/`AdoptionFetcher` (`adoption.go`) naming overlap. Informational naming with no behaviour: a rename would churn three packages (`handler`, `freshness`, `simtest`) for no change in what the code does, so phase E, whose tasks hand-edit these files, left it (re-filed in phase E's review fix round 1, which corrected Task 15's "in files no task changes"). (H)
