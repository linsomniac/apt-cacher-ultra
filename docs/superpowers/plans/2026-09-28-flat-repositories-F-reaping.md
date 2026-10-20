# Flat repositories — Phase F: Suite reaping — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give suites a lifecycle (spec C9, limitation #3): an hourly reaper deletes the suites no client asked for in `gc.suite_idle_ttl`, unreachable legacy rows and legacy junk rows, through the one suite-row delete phase D built; retired snapshots leave every history lookup and are deleted by snapshot GC; markers, maps and memos stay bounded; and `POST /forget` lets an operator forget a suite on purpose. An unsigned suite with `adoption.adopt_unsigned = false` keeps the coherent snapshot phase E froze for it (ruling PF-5): this phase retires nothing for the knob.

**Architecture:** `internal/cache` keeps one suite-row delete, `deleteSuiteTx` (phase D's compare-and-delete, which runs the reaper's retirement path `retireSuiteSnapshotsTx` — release the current snapshot's pins once, retire every snapshot of the suite — before its `DELETE`), now beside its three callers in `suite_reap.go`: cap eviction, `ReapIdleSuite` (the reaper's final step, which also moves the base's signed-seen markers up to the reap: design review M-9) and `ForgetSuite` (POST /forget's, which also deletes the base's signed-seen markers and the cached anchors of the base and its `/.` twin, under both schemes, when no suite row of the host is left for the base or the twin: ruling PF-2). Both settle a failed submit by a fresh read (design review M-2). `DeleteSuiteMetadata` deletes a suite's metadata url_path rows in batches of 1,000 per writer transaction, anchors last. Snapshot GC ranks retired rows out of `keep_displaced` and gains sub-job C, which deletes them in chunks of 10,000 rows. In `internal/freshness` a `Reaper`, run by `main` under the checker's lifecycle with its own hourly timer after a one-hour uptime floor, lists candidates — rows the grammar can no longer produce and legacy junk (a suite without a snapshot whose cached anchors, fetched a day ago or earlier, are all junk by their content: rulings R-F3, PF-3), reaped regardless of idleness and first (ruling PF-4), then the idle suites, oldest first — holds each through the checker's `HoldSuite` before any writer operation, with the work under the hold bounded at 5 minutes, deletes, and purges the suite's in-memory state through `release(true)`; each run also sweeps the reconcile memo, and once a day (its daily duty) sweeps the orphan signed-seen markers (a year old, no suite row for the base or its twin; a future stamp reads as now and is reported, never rewritten: ruling PF-1). A forward step of the wall clock with no restart defers reaping and the sweep by the uptime floor (design review M-8). `Gate.Purge` keeps a live entry. The admin listener gains `POST /forget` behind `requireMutationGuard` (a cross-site form or `fetch` cannot trigger it: ruling PF-6), whose answers say what happened (ruling PF-7), and the status page shows each suite's scheme; the gpg once-logger sets reset at 10,000 keys.

**Tech Stack:** Go 1.25 (toolchain 1.26), modernc SQLite, `testing/synctest`.

**Spec:** `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (revision 8; Task 16 records this plan's rulings, design notes and deviations as revision 9) — C9 in full (§9's F row: snapshot retirement, the reaper with its own ticker, batched metadata deletion, snapshot-GC sub-job C, the memory purge, the marker TTL, `POST /forget`). Interactions read: C3 (rows, caps, eviction through `deleteSuiteTx`, markers that survive reaping, touches), C4 and C5 (never-downgrade after a reap, unsigned refusals, the clock-step rule for a gate that retires things), C7 (records, `unsigned_disabled`), C8 (`Gate.Purge`), C11 (`gc.suite_idle_ttl`, events, metrics), §6.1 (audit-retired snapshots), §7 (F's tests), §10 (G-1), §11 (risks). Overview: `docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md` — Global Constraints, Deliberate deviations, Review Focus item 6, the "F:" line under "Later phases add" and every item of the "F:" carry-forwards block (from A, B, C, D and E). Deferred minors: `…-A-deferred.md` through `…-E-deferred.md` (IDs cited per task; Task 16's intro lists the ones taken and the ones checked and left, and Task 16 deletes the taken ones). The plan's two reviews (execution: D1–D8 and its mutants; design: I-1–I-7, M-1–M-17) are adjudicated as rulings PF-1–PF-9 (2026-10-20), which this revision applies throughout; the maintainer confirmed PF-5 the same day.

## Global Constraints

Everything in the overview's Global Constraints applies. In particular:

- No schema change: v7 holds every column and table this phase writes (`suite_snapshot.retired_at`, `suite_freshness.last_requested_at`, `suite_signed_seen`). Never edit an entry of `migrations`.
- Production SQL never uses SQLite clock functions; persisted stamps come from `wallclock` (the cache's `nowUnix`, the checker's `now`). The reaper's cadence and uptime floor, and all in-memory timing, use monotonic timers; its forward-step guard compares the wall clock with the monotonic one (design note 22).
- `internal/cache` never imports `internal/freshness` or `internal/proxy`; the reaper holds suites through the cache-side `cache.SuiteGuard` the checker implements. The writer goroutine never runs foreign code or pool I/O: the junk rule's sniffs read pool files on the reaper's goroutine, outside the writer (ruling PD-5).
- One suite-row delete (ruling R-F1): `deleteSuiteTx` holds the only `DELETE FROM suite_freshness` in production code, and only cap eviction (`createSuiteTx`), `ReapIdleSuite` and `ForgetSuite` call it (`TestSuiteFreshnessHasOneDeleteSite`, `TestSuiteRowWritersHaveKnownCallers`); outside `internal/cache` only the reaper and `POST /forget` call the cache's suite deleters (`TestSuiteDeletersHaveKnownCallers`), and only `ForgetSuite` and the orphan sweep delete signed-seen markers (`TestSignedSeenHasKnownDeleteSites`; design review M-5). Every deleter holds the suite through the `SuiteGuard` before its writer operation and, once the row is gone, purges with `release(true)` — gone as the cache answers it, a failed submit settled by a fresh read (design review M-2).
- An I/O fault is never evidence (overview): the junk rule reaps only on blobs it read and sniffed; a missing or unreadable pool file keeps the suite.
- Constants (spec C9, C11), not knobs: reaper interval 1 h; uptime floor 1 h; the orphan-marker sweep every 24 h (the reaper's daily duty, design note 20); at most 25 suites per run; metadata batches of 1,000 url_path rows per writer transaction; sub-job C chunks of 10,000 rows per writer transaction; orphan-marker TTL 365 d; gpg once-logger sets reset at 10,000 keys; junk grace 24 h (design note 8); the work under one suite's hold bounded at 5 min (design note 23); a forward step is the wall clock running more than 1 h ahead of the monotonic clock between two runs (design note 22). Knob: `gc.suite_idle_ttl` (default `"720h"`; `0s` disables suite reaping; otherwise at least `24h`).
- Tests that call `wallclock.SetOffsetForTest` never use `t.Parallel()`. Simulations run inside `testing/synctest` bubbles, finish in under 5 s under `-race` on a quiet machine (4.5 s: phase E ruling PE-8's margin), sample goroutines only after `synctest.Wait()`, and silence loggers with `simtest.DiscardLogger()` (or capture them).
- Every goroutine started under `internal/freshness` or `internal/gc` recovers panics (`grep -n 'go func' internal/freshness/*.go internal/gc/*.go | grep -v '_test.go:'`): the reaper's runs recover theirs (logged `freshness_check_panicked`, `where=reaper`) and release a held suite on the way out. The admin `/forget` handler runs on net/http's goroutine, which recovers.
- `go test -race ./...` and `golangci-lint run ./...` are clean; `gofmt -l .` lists only the five known drift files (`cmd/apt-cacher-ultra/ca_print.go`, `internal/cache/cache_test.go`, `internal/cache/gc_urlpath_test.go`, `internal/freshness/hot_prefetch_test.go`, `internal/proxy/connect_stats.go`) at the end of every task. Never run `gofmt -w` on those five; edit them by hand.
- Commit trailer: the implementer's own harness attribution line (phase A ruling); otherwise `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

Conventions in this plan:

- Each task's **Interfaces** block lists what it uses from other tasks and existing test files. Never redefine a helper: a second definition in the same package does not compile.
- Each commit step starts with `gofmt -l <files>`; it must print nothing. Run `gofmt -w` on the non-drift files a step edits before that check; never on the five drift files.
- A "verify failure" step whose expected result is a compile failure is complete when the compiler names the undefined identifiers listed; such a step runs `go test -gcflags=-e …` so every error is listed.
- Mutation checks are run one at a time, each reverted before the next; record each failing test line in the commit message.
- **Existing tests that change.** Ruling R-F2 changes what four existing tests pin (an evictee's flip-gate entry whose serve completed moments before the purge is now kept live): Task 1 moves them into `synctest` bubbles and lets the serve age a quiet period first, so they still prove the purge. Ruling PF-7 adds the status JSON's `scheme`: Task 10 adds it to `TestJSONContractPreserved`'s fixture and regenerates its golden, an additive change the admin page's JSON contract allows. Any other existing test that fails means the change is wrong.

## Rulings and design notes

Controller rulings (binding), as the plan's reviews left them:

- **R-F1 — one suite-row delete path.** The reaper (`cache.ReapIdleSuite`) and `POST /forget` (`cache.ForgetSuite`) delete through `deleteSuiteTx`, phase D's compare-and-delete, holding the suite through the `SuiteGuard` (`(*freshness.Checker).HoldSuite`) before the writer operation. After the commit the in-memory state is purged exactly as cap eviction does: `release(true)` drops the lock-map entry, the flip-gate entry (unless live, R-F2) and the checker's per-suite maps (`absentAnswered`, `lateralHolds`), and the cache forgets the touch entry. A busy suite is skipped by the reaper (retried next run) and refused by `/forget` (409, "busy or unknown"). No other code path deletes a `suite_freshness` row: Task 2 adds the single-DELETE guard (none existed) and a callers allowlist, and Task 3 extends it across packages (design review M-5). Tasks 2, 3, 6, 10.
- **R-F2 — `Gate.Purge` keeps live entries.** `Purge` skips an entry with a flip in progress, a hold, an anchor serve in flight, or a serve completed within the quiet period; such an entry expires on its own (twice the quiet period idle). Pinned by a `release(true)` racing a re-creation's registered serve (`TestHoldSuite_ReleaseKeepsARecreatedSuitesLiveGateEntry`). Task 1.
- **R-F3 — legacy junk rows are reaped by the reaper, not on the request path.** A tracked suite with no current snapshot whose cached anchor rows (`InRelease`, `Release`, `Release.gpg`) are all junk, all fetched at least the junk grace ago (24 h, design note 8), is reaped regardless of idleness. The anchor touch on the hot path stays as it is. What counts as junk is ruling PF-3's content rule (design note 9), which replaces the draft's record condition. Task 7.
- **R-F4 — PE-12's retirement. Withdrawn by ruling PF-5.** The draft retired an established unsigned suite's snapshots while `adoption.adopt_unsigned = false`, serving it from the ordinary cache (`cache.RetireSuiteSnapshots`, an hourly retirement in the reaper, INFO `unsigned_snapshot_retired`). None of it is built (design note 11). Task 8.
- **R-F5 — the marker sweep's clock-step rule.** The 365-day orphan-marker sweep reads `last_seen_at` as a gate that retires things: a future stamp reads as now — never as old, so never swept — and is reported through `clock_step_detected`. Ruling PF-1 overrides the ruling's "and heals it": the sweep never rewrites a stamp (design note 12, deviation 3). Markers of a reaped or forgotten suite survive reaping; only an orphan (no suite row for its base or its `/.` twin, under either scheme) a year old is swept, and `/forget` deletes a base's markers only when no other suite of the base is left (the E carry-forward's base and twin rule; design note 13). Tasks 3, 9.

The plan reviews' adjudication (binding; 2026-10-20):

- **PF-1** (design review I-1; overrides R-F5's "heals"). The marker sweep never rewrites a stamp: a future stamp reads as now, is never swept, and is reported. Lowering a stamp deletes signed history after a corrected backward step. Deviation 3. Task 9.
- **PF-2** (I-2). When `/forget` deletes the base's markers it also deletes, in the same transaction, the cached `InRelease`, `Release` and `Release.gpg` url_path rows of the base and its `/.` twin under both schemes: otherwise the unsigned commit's re-mark (phase E ruling R-E1) marks the base again and the escape hatch stays shut with no remedy. At most 12 cached anchors are fetched again. Tasks 3, 15.
- **PF-3** (I-3; replaces the draft's design note 9). The junk exemption is content-based: an `InRelease` that fails the InRelease sniff but passes the `Release` sniff is a plain-text `Release` (a §6.1 suite) and no evidence; every other sniff-failing anchor is evidence whatever the suite's adoption record. Task 7.
- **PF-4** (I-4). The junk scan runs every run, and the candidates reaped regardless of idleness (non-canonical, junk) sort before the idle ones; the junk query's plan is pinned (M-14). Tasks 6, 7, 9.
- **PF-5** (I-5; reverses the F half of phase E ruling PE-12; the maintainer confirmed it on 2026-10-20). No knob-off retirement: with `adoption.adopt_unsigned = false` an established unsigned suite keeps phase E's coherent frozen snapshot (refused `unsigned_disabled`, visible, paced). The docs say plainly what the knob means: unsigned repositories are not kept fresh by this cache; an adopted one stays at its last snapshot until the knob is turned back on (or the suite is forgotten); a never-adopted one is served as first cached; set it only if you don't serve unsigned repositories through the cache. Task 8 is that documentation and a coherence test; the overview's PE-12 carry-forward closes with it (Task 16).
- **PF-6** (I-6). Honest CSRF wording everywhere: "a cross-site form or `fetch` cannot trigger it; it does not stop DNS rebinding against a listener without `htpasswd_file`". The guard is one function, `requireMutationGuard`; Host-header validation for `/forget` and `/reconcile` goes to spec §10 G-1, a maintainer call (it affects operators behind reverse proxies). Tasks 10, 16.
- **PF-7** (I-7). `/forget`'s 202 body names what was forgotten, the markers deleted and the rows that kept them; its 409 text says what to check; the status page gains each suite's scheme (JSON `scheme`, additive; the HTML Host cell). Tasks 3, 10, 16.
- **PF-8**. The design review's minors, each where it is cited: M-1 (= D1; Task 11), M-2 (design notes 5, 14, 23; Tasks 3, 6, 10), M-3 and M-4 (design note 19; Tasks 6, 7), M-5 (Global Constraints; Task 3), M-6 (design note 21; Task 15), M-7 (design note 14; documented only), M-8 (design note 22; Tasks 6, 9, 15), M-9 (design note 12; Task 3), M-10 (design note 24; Tasks 6, 9), M-11 and M-13 (Task 16), M-12 (moot: its code went with PF-5), M-14 (design note 8; Task 7), M-15 (design note 7; Tasks 3, 4), M-16 (design note 25; Task 6), M-17 (a G carry-forward; Task 16); a DoD simulation at the production cadence (deviation 2; Task 15); `--color-moved` for Task 2's review.
- **PF-9** (the execution review). D1–D8 are applied in the tasks they name, and its proposed killer tests are added, except X12's (it tested the withdrawn retirement); X29 (sub-job C's chunk size) is pinned by a constant assertion (`TestRunRetiredSnapshotGCChunk_TheChunkIsTenThousandRows`) instead of a 4.6 s test; X3 and X19 need no test.

Rulings of earlier phases this plan applies: PD-1 (a file changed only by a mechanical rename does not count as touched for the deferred-minor rule: Task 2's `SuiteExists` → `HasSuiteRow` rename outside `suite_rows.go`, Task 9's `lastUse(row)` → `row.LastUse()` in `handler/suite_rows.go`), PD-5 (no pool I/O on the writer), PD-11 (gates that meet a future past-event stamp report it through the checker's rate-limited `clock_step_detected`), PE-3 (an `unsigned_disabled` suite's checks are paced), PE-8 (the 4.5 s simulation budget), PE-12 (its knob-off refusal state stands as phase E built it; its F half is reversed by PF-5).

Design notes (choices the spec leaves open — the boring, bounded option, with the reason):

1. **The reaper is its own type in `internal/freshness`** (`Reaper`, `NewReaper`, `ReaperConfig`), started by `main` beside `Checker.Run` under the same lifetime context and wait group, and wired to the admin listener as its `Forgetter`. It holds the checker (for `HoldSuite`, its clock, the clock-step and panic reporters and the adopter's memo) and the concrete `*cache.Cache` (the batched deletes need the real cache; the checker's `Cache` interface and its test fake stay as they are). `Checker.Run` returning at once for `periodic_refresh = 0` does not stop it, and it runs whatever `gc.enabled` says (spec §7: "`gc.enabled = false` still re-adopts" a reaped suite; `docs/configuration.md` says that reaping runs with GC off: design review M-11).
2. **`gc.suite_idle_ttl = 0s` disables suite reaping** — idle, non-canonical and junk alike: no suite row is deleted by the reaper, and no junk scan runs. The run's other duties (the marker and memo sweeps) still run. The key is validated (`0s` or at least `24h`) whatever `gc.enabled` says.
3. **Runs:** the first at the uptime floor, then one an interval after the previous run finished (as `gc.Run` does). At most 25 candidates are tried per run — a busy one counts, and is retried next run. The candidates reaped regardless of idleness (non-canonical and junk rows) go first (ruling PF-4: a junk suite fails its clients' updates, and those clients' touches would sort it after every idle suite — days behind an upgrade's idle backlog; the classes reaped regardless are legacy rows, few and finite, so they cannot starve idle reaping), then the oldest `last_requested_at` (no stamp first), then key order.
4. **The idle cutoff is strict** (spec C9: `last_requested_at < now − gc.suite_idle_ttl`): `ReapIdleSuite` gets `now − ttl − 1`, since `deleteSuiteTx` keeps a stamp after its cutoff. A NULL or future stamp is never idle (the clock-step rule for a gate that retires things); the furthest future one is reported (design note 24).
5. **The re-read under the hold** is I-4's re-validation: a row that vanished since the listing (cap eviction) has the lock-map entry `HoldSuite` stored dropped (`release(true)`); a suite asked for, or adopted, since the listing is left whole — its metadata rows are not deleted for a final step that would keep it. After the final step, whether the row is gone is the cache's answer (design note 23).
6. **`cache.ReapRegardless`** (`math.MaxInt64`) is `deleteSuiteTx`'s cutoff for a delete regardless of idleness — non-canonical and junk rows, `/forget` — and lets every row through, NULL and future stamps included (phase D's helper kept NULL rows forever).
7. **Metadata deletion** (`DeleteSuiteMetadata`): the url_path rows of the suite's scheme and host whose `aptpath.SuitePath` is the suite — never a nested suite's, a prefix sibling's or the `/.` twin's — in path order, plain metadata first and the anchors (`InRelease`, `Release`, `Release.gpg`) last, in writer transactions of at most 1,000 rows; each row releases its blob's pin with it (`deleteURLPathRowTx`), once. A failure leaves the earlier batches deleted and the anchors last, so a cut-short run never strands a suite without its anchors. Measured (design review M-15): a 1,000-row batch holds the writer about 110 ms over a 215,000-row table, and a 10,000-row sub-job C chunk about 80 ms; both constants say so beside them. Before phase G no path is a flat suite's, so a flat-shaped legacy row's metadata is left to url_path GC — as are a non-canonical row's cached files, which no suite path matches (`metadata_rows` 0; the docs say so).
8. **The junk grace is 24 h, measured on the cached anchor rows' `last_fetched_at`**: every row must have been fetched at least 24 h ago (a NULL or future stamp is not old). A transient soft-404 that replaced a good anchor is younger, so the suite is kept while its next check (with adoption on, the probe tree adopts and re-seeds the row) or url_path GC heals it; 24 h is cap eviction's idle rule and covers a day-long outage. Legacy junk — a v6 row whose `InRelease` was an HTML page — is reaped at the first run after the upgrade. The scan runs every run (ruling PF-4): one indexed read on the pool (`ListUnadoptedSuiteAnchors`, whose query plan `TestListUnadoptedSuiteAnchors_ProbesURLPathByItsKey` pins: design review M-14; about 73 ms for 5,000 suites), then pool reads of at most 64 KiB per anchor on the reaper's goroutine, each verdict kept by (file kind, blob hash) until the next run: the pool is content-addressed, so a run reads only anchors it has not sniffed (without the memo, 5,000 never-adopted suites — `max_suites`'s default, no adopter wired — would cost about 330 MB of reads an hour).
9. **The junk rule is content-based** (ruling PF-3, which replaces the draft's record condition). An anchor is evidence when it fails its own sniff — except an `InRelease` that passes the `Release` sniff: a plain-text `Release` cached as `InRelease` is a §6.1-quarantined plain-text suite that phase E adopts unsigned through `Release`, and reaped before it adopts it could never be tracked again (a plain-text `InRelease` creates no row, spec C3). Adoption records are ignored: the draft's condition (no record, or an `anchor_absent` one) kept a suite whose adoption failed for another reason (`unsigned_disabled`, `unsigned_too_large`, a failed adoption's record) serving its HTML anchor forever, since a suite without a snapshot has its url_path metadata served as cached, never revalidated (design review I-3). The pool file must exist and read: a missing file is no evidence here, and the scan's failed reads are one WARN with their count (design note 19).
10. **Busy and gone** are distinct for the reaper (skip, or drop the entry) but one answer for `/forget`: 409 for busy or unknown, no existence oracle (spec C9), with a text that says what to check (ruling PF-7). A database error is 500, logged.
11. **No knob-off retirement** (ruling PF-5, which withdraws R-F4 and this note's draft). Retiring an unsigned suite into the ordinary cache would freeze each index file at its first fetch, so every upstream publish would bring Hash Sum mismatches that never clear (design review I-5). With the knob off, the suite keeps phase E's coherent snapshot, refused `unsigned_disabled` (visible on the status page, its checks paced by ruling PE-3), until the knob is turned back on — it then adopts again through the normal path — or the suite is forgotten. `TestServeHTTP_KnobOffKeepsServingTheUnsignedSnapshot` pins that it keeps serving the snapshot coherently (Task 8); `docs/configuration.md` says the cache then does not keep unsigned repositories fresh.
12. **The sweep never rewrites a stamp** (ruling PF-1, deviation 3). A marker stamped more than 2 s past now reads as now: it is never an orphan candidate (`last_seen_at < cutoff` excludes it), and the furthest is reported through `clock_step_detected` (`stamp=last_seen_at`; design note 24). The sweep cannot tell a stamp written while the clock ran ahead from a clock now behind, and lowering the stamp in the second case deletes signed history once the clock is corrected (`TestSweepOrphanMarkers_BackwardStepThenCorrectionSweepsNothing`, design review I-1), so a future-stamped orphan lingers until the clock passes its stamp. Markers keep the newest stamp (`markSignedSeen`), and a reap moves its base's markers up to the reap, `MAX(last_seen_at, now)` under both schemes (design review M-9): the orphan's year counts from the end of its suite's tracking, so after a long downtime a reaped suite's marker is not swept a day after it became an orphan. MAX never lowers a stamp, and the UPDATE never creates a marker.
13. **Cross-scheme marker rules.** Never-downgrade reads a base's markers under either scheme (spec C5, M-7), so a marker is an orphan only when no suite row of its host exists for its base or its `/.` twin under either scheme, and `/forget` deletes the base's markers under both schemes or neither — only when no suite row of the host is left for the base or the twin. Deleting one scheme's marker would leave the other refusing; deleting a marker the other scheme's suite relies on would strip its protection. With the markers, `/forget` deletes the cached anchors of the base and its twin under both schemes (ruling PF-2, at most 12 rows): no suite owns them then, and a signed one left cached would mark the base again at the next unsigned commit (ruling R-E1); with a suite row left, both stay, and the 202 body names the rows that kept them.
14. **`POST /forget`** takes `host`, `suite` and an optional `scheme` (`http` or `https`, default `https`, as `/reconcile`; anything else is 400); it runs synchronously under the suite's hold — the row read first (I-4), all metadata batches, then `ForgetSuite` — on a context a client disconnect cannot cancel, within the hold's 5 minutes (design note 23), so the work is done when 202 comes back (spec C9's "202 on success" kept for a synchronous call). The 202 body names the suite forgotten, the markers deleted and the rows that kept them; the 409 body says what to check — busy (try again), or no such suite (the scheme defaults to `https`; host and suite as the status page, which now shows each suite's scheme, shows them) (ruling PF-7). The guard, `requireMutationGuard` (ruling PF-6): a request must carry `X-ACU-Admin: 1` and a `Sec-Fetch-Site` of `same-origin` or `none` if it has one, else 403 — a cross-site form or `fetch` cannot trigger it (a form cannot set the header; a cross-origin `fetch` that sets it is refused on the `Sec-Fetch-Site` the browser adds, and its preflight gets no CORS grant). It does not stop DNS rebinding against a listener without `htpasswd_file`; Host-header validation of `/forget` and `/reconcile` is spec §10 G-1's. `OPTIONS /forget` answers `Allow: POST, OPTIONS`. Forgetting an active suite bypasses the flip barrier (spec C8): a client in the middle of an update can see one Hash Sum mismatch, a residual the docs state (design review M-7). INFO `suite_forgotten`.
15. **Sub-job C** works one retired snapshot per writer transaction: up to 10,000 of its `snapshot_member`, `snapshot_skipped_member` and `package_hash` rows, and its header once none is left; it runs after sub-jobs A and B have drained, inside the snapshot pass and its deadline, and needs no grace (every retirer holds the suite's mutex, so no adoption of a retired snapshot runs). `gc_run_complete` and `LastRunSummary` gain `retired_reaped`, with the counter `acu_gc_retired_reaped_total`; the admin status page's GC section is unchanged (H carry-forward).
16. **The adoption stamp's floor is every snapshot of the suite** (deferred minor C-11), retired ones included, in both of `adoptionStampTx`'s branches.
17. **The gpg once-logger sets** each hold at most 10,000 keys: a new key when the set is full starts it over (the spec's "reset when they exceed 10,000 entries"); a suite may then log its once-line again.
18. **The reconcile memo** is swept each run against the current snapshot ids the run listed; a snapshot adopted during the run may lose a fresh entry (one extra Release parse).
19. **Events:** INFO `suite_reaped` (`reason`: `idle`, `non_canonical`, `junk`; `metadata_rows`), INFO `suite_forgotten` (`metadata_rows`, `markers_deleted`, `anchor_rows`), INFO `admin_forget_requested` (as `/reconcile`'s), INFO `signed_seen_markers_swept` (`count`, only when non-zero); `freshness_check_panicked` gains `where=reaper`; `clock_step_detected` gains the stamps `last_seen_at` and `wall_clock` (design note 22). The reaper's failure WARNs carry prose names — seven `suite reaper: …` lines and `admin: forget failed` — as phases D and E's do (H may rename them); a step that fails because the run's context ended (a shutdown) logs at DEBUG, not WARN (design review M-3), and a junk scan's failed pool reads are one WARN with their count, not one each (`suite reaper: junk anchor reads failed`; design review M-4).
20. **The reaper's daily duty.** Only the orphan-marker sweep (Task 9) runs at the first run after a start and then a day apart (monotonic): it works in years (its 365-day TTL) and costs a writer transaction per batch. The junk scan runs every run (ruling PF-4, design note 8). A run the forward-step guard defers neither reaps nor sweeps, and leaves the sweep due for the next run (a step makes markers look older too). The hourly part of a run is one suite listing, the junk scan, the reaps and the memo sweep.
21. **The DoD simulations' harness.** A boot is the cache opened on a directory, the adopter, the checker (its scheduler not running: the simulated clients adopt through the adopter, as a T1 check would) and the reaper and GC loops under one lifetime; a restart stops and reopens it all over the same directory, and the goroutine count returns to its base after every restart (design review M-6). The 90-day simulation needs deliberate deviation 2 to fit the 5 s `-race` budget (measured: 3.35 s; at production cadence with 12-hourly restarts, 12.5–12.8 s); a four-day simulation at the production cadence runs beside it (0.95 s). The chaos run races reaps, adoptions and clients under a global suite cap of 3 (M-6).
22. **The forward-step guard** (design review M-8). The monotonic clock does not advance while a machine is suspended or a VM paused, and the wall clock does; the uptime floor covers only a fresh start. Each run compares how far the checker's wall clock and the monotonic clock advanced since the previous run (or `Run`'s start), the wall clock in whole seconds; when the wall clock ran more than 1 h ahead, the run logs `clock_step_detected` (`stamp=wall_clock`) and defers reaping and the marker sweep by the uptime floor, the hour a restart would give clients to touch their suites. A backward step needs no guard: a future interest stamp is never idle. Runs far apart on both clocks are no step.
23. **The work under one suite's hold is bounded, and "gone" is settled** (design review M-2). The re-read, the metadata batches and the final delete run within 5 minutes (`suiteHoldTimeout`), as `commitTimeout` bounds `CommitAdoption`: a wedged writer gives the suite back instead of holding it until shutdown; a step it cuts short is a failure (WARN). `submitWrite` can fail after its transaction committed, so `ReapIdleSuite` and `ForgetSuite` settle a failed submit by a fresh read of the row (as `afterCreate` settles a creation) and report `gone`; the caller releases with `release(gone)`, so a deleted suite's lock-map entry never leaks and a kept one's is never dropped.
24. **Clock-step reports once per change** (design review M-10). The reaper reports the furthest future `last_requested_at`, and the sweep the furthest future marker, at the first run (sweep) after a start and whenever that suite (marker) or its stamp changes — not every hour until the clock passes a dead suite's stamp.
25. **The reaping metric** (design review M-16): `acu_suites_reaped_total{reason}` (`idle`, `non_canonical`, `junk`) counts the reaper's deletes; `/forget` and cap eviction have their own events.

## Deliberate deviations from the spec

1. **Withdrawn (ruling PF-5).** The draft's knob-off retirement retired every snapshot of the suite, where spec C5 safeguard 1 and the overview's E carry-forward said "the current snapshot"; that retirement is not built (design note 11). Spec revision 9 states safeguard 1 as built: the suite keeps its coherent snapshot while the knob is off.
2. **The 90-day simulation is coarser than spec §7 asks** (Task 15): the reaper runs every 6 h (its one-hour uptime floor kept), the daemon restarts every fifth day plus an hour before three suites' deadlines (not every 12 h), and GC runs at `gc.interval = 24h` without the url_path TTL pass (`gc.url_path_ttl = 0s`, a valid setting; the reaper deletes reaped suites' rows itself). Spec §7 asks for production intervals and 12-hourly restarts; that run takes 12.5–12.8 s under `-race` against the 5 s budget (Global Constraints). The coarsening keeps every assertion: Task 6 pins the hourly cadence and the floor, and the pre-deadline restarts put the floor in front of the reaps that matter. The production cadence itself — the reaper hourly, `gc.url_path_ttl = 168h`, restarts every 12 h — runs over four days in `TestDoD_ProductionCadenceAcrossTwelveHourlyRestarts` (ruling PF-8), with a suite whose deadline falls half an hour before a restart, so a reaper without its floor would reap it there.
3. **The marker sweep never rewrites a future stamp** (ruling PF-1, Task 9), where ruling R-F5 had the sweep heal it (rewrite it to now): the stamp reads as now, is never swept, and is reported; a future-stamped orphan is swept only once the clock has passed its stamp by a year. Spec revision 9 states the rule as built (C3, C4's clock-step rule, C9).

## Review Focus (phase F share)

1. **A cache restarted after a long stop, or across a wall-clock step** (days offline; NTP stepping the clock ±30 d; a VM resumed or a laptop woken after a long pause): every suite looks idle. The uptime floor gives clients an hour to touch their suites after a start, and the forward-step guard gives them the same hour after a step without a restart; a suite reaped anyway re-adopts at its next request; a future stamp is never idle, never swept, never rewritten, and is reported once per change — Tasks 6, 9, 15 (`TestReaper_AForwardStepDefersReapingByTheFloor`, `TestReaper_AForwardStepDefersTheMarkerSweep`, `TestDoD_ForwardStepFalseReapsSelfHeal`, the 90-day simulation's clock jump and restarts, `TestDoD_ProductionCadenceAcrossTwelveHourlyRestarts`).
2. **Upstreams answering 200 HTML for anchors** (soft-404s, captive portals) on legacy rows or for a moment: a legacy junk row is reaped after the 24 h grace even while clients keep touching it, at the next run and ahead of any idle backlog; a transient soft-404 younger than the grace is kept; a plain-text `Release` cached as `InRelease` is never junk, whatever the suite's record — Task 7 (`TestReaper_ReapsLegacyJunkKeepsTransientSoft404`, `TestReaper_JunkIsNotStarvedByAnIdleBacklog`).
3. **An operator, or a web page, reaching `POST /forget`**: a cross-site form or `fetch` is refused (403), DNS rebinding against a listener without `htpasswd_file` is not (documented; spec §10 G-1); a busy or unknown suite answers 409 without telling which, with a text that says what to check; forgetting a suite whose `/.` twin or other-scheme suite still has a row keeps the base's markers and cached anchors and names the rows that kept them; with none left, a signed anchor cached under the other scheme no longer marks the base again — Task 10 (`TestEndpoint_Forget`, `TestEndpoint_Forget_RefusesCrossOriginRequests`), Task 3 (`TestForgetSuite_DeletesTheBaseMarkersOnlyWithoutAnotherSuite`), Task 15 (the escape-hatch tests).
4. **Very large suites reaped** (Ubuntu `main`: thousands of metadata rows, tens of thousands of `package_hash` rows): no writer transaction deletes more than 1,000 url_path rows or 10,000 snapshot rows (about 110 ms and 80 ms of writer time, measured), and a cut-short run resumes without repeating a decrement — Tasks 3, 4 (`TestDeleteSuiteMetadata_BatchesOfAThousandAnchorsLast`, `TestRunRetiredSnapshotGCChunk_DeletesRetiredSnapshotsInChunks`, `TestDeleteSuiteMetadata_NeverRepeatsADecrement`).
5. **A suite reaped while it is in use** (an adoption or check in flight, a client returning mid-run, a re-creation's anchor serve racing the purge, a wedged writer): the reaper skips a held suite, keeps one asked for since the listing, never purges a live flip-gate entry, and gives a hold back after 5 minutes — Tasks 1, 6, 15 (`TestDoD_ReapRacesAdoptionsAndClients`, `TestReaper_BoundsTheWorkUnderAHoldAtFiveMinutes`).
6. **An unsigned suite with `adoption.adopt_unsigned = false`** keeps serving its coherent snapshot, refused `unsigned_disabled`, and the docs say the cache then does not keep it fresh — Task 8 (`TestServeHTTP_KnobOffKeepsServingTheUnsignedSnapshot`; ruling PF-5).

## File map

| Area | Files | Task |
|---|---|---|
| Flip gate | `internal/flipgate/gate.go` (`Purge`, `liveLocked`, C-2/C-3 notes), `gate_test.go`; `internal/freshness/suite_lock_test.go`, `internal/handler/suite_hit_test.go`, `suite_dod_test.go` (tests R-F2 changes) | 1 |
| One delete path | `internal/cache/suite_rows.go` (split), `suite_evict.go` (new: cap eviction), `suite_reap.go` (new: `deleteSuiteTx`), `cache.go` (`committedHook`), `single_insert_test.go`, `suite_evict_test.go`; the `HasSuiteRow` rename in `internal/freshness/freshness.go`, `freshness_test.go`, `suite_lock_test.go`, `internal/handler/handler.go` and tests | 2 |
| Reap and forget | `internal/cache/suite_reap.go`, `cache.go` (`committedHook`'s doc), `signed_seen.go` (a note), `single_insert_test.go` (the cross-package guards), `suite_evict_test.go`, `suite_reap_test.go` (new) | 3 |
| Snapshot GC | `internal/cache/gc.go` (sub-job B, `RunRetiredSnapshotGCChunk`), `snapshot_gc_retired_test.go` (new); `internal/gc/snapshot.go`, `gc.go`, `metrics.go`, `retired_test.go` (new) | 4 |
| Adoption stamp and the rule's home | `internal/cache/adoption.go`, `signed_seen.go`, `adoption_stamp_test.go` (new) | 5 |
| The reaper | `internal/freshness/reaper.go` (new), `reaper_test.go` (new), `reconcile.go` (`sweepReconcileMemo`), `adoption.go` (the memo's note), `metrics.go` (`acu_suites_reaped_total`) | 6 |
| Junk rows | `internal/cache/suite_reap.go` (`ListUnadoptedSuiteAnchors`), `unadopted_anchors_test.go` (new); `internal/freshness/reaper.go`, `reaper_junk_test.go` (new) | 7 |
| Unsigned suites with the knob off | `internal/freshness/first_adoption.go` (a note), `docs/configuration.md` (`adopt_unsigned`); `internal/handler/unsigned_knob_off_test.go` (new) | 8 |
| Marker sweep, daily duty | `internal/cache/marker_sweep.go` (new), `types.go` (`URLPath.LastUse`), `marker_sweep_test.go` (new), `single_insert_test.go`; `internal/freshness/reaper.go`, `unsigned.go`, `reaper_markers_test.go` (new); `internal/handler/suite_rows.go` (mechanical) | 9 |
| POST /forget | `internal/freshness/forget.go` (new), `forget_test.go` (new); `internal/admin/server.go`, `handlers.go`, `status.go`, `status_template.go`, `json_contract_test.go`, `testdata/json_contract_golden.json`, `forget_test.go` (new) | 10 |
| Knob and wiring | `internal/config/config.go`, `suite_idle_ttl_test.go` (new); `cmd/apt-cacher-ultra/main.go`, `reaper_wiring_test.go` (new); `packaging/config/config.toml.default` | 11 |
| gpg once-loggers | `internal/gpg/oncelog.go` (new), `verifier.go`, `oncelog_test.go` (new) | 12 |
| Adoption deferred minors | `internal/freshness/adoption.go`, `adoption_minors_test.go` (new) | 13 |
| Other deferred minors | `internal/freshness/suite_lock.go`, `clockstep.go`, `first_adoption.go`, `unsigned.go`, `suite_lock_test.go`, `scheduler_test.go`, `first_adoption_test.go`; `internal/handler/suite_hit_test.go`, `suite_dod_test.go` | 14 |
| DoD | `internal/freshness/dod_f_test.go`, `dod_f_rig_test.go`, `dod_f_chaos_test.go` (new) | 15 |
| Documentation | `docs/configuration.md`, `docs/log-fields.md`, `docs/admin-ui-spec.md`, the spec, the overview, the A, C, D and E deferred files | 16 |

No migration, no new package, no new dependency.

## Tasks

1. `Gate.Purge` keeps live entries (R-F2), and the gate's deferred minors
2. One suite-row delete path: the cache's eviction split, the single-DELETE guard and `suite_rows.go`'s deferred minors
3. The cache's reap, forget and retire operations
4. Snapshot GC: retired rows leave `keep_displaced`; sub-job C
5. The adoption stamp's floor and `adoption.go`'s deferred minors
6. The reaper: idle and non-canonical suites, the hourly run, the memory purge
7. Legacy junk rows (ruling R-F3, ruling PF-3's content rule)
8. Unsigned suites with the knob off keep their coherent snapshot (ruling PF-5)
9. The orphan-marker sweep (rulings R-F5, PF-1), the reaper's daily duty, and the marker stamp's one reader
10. `POST /forget` (spec C9, D9)
11. `gc.suite_idle_ttl` and the daemon wiring
12. The gpg once-logger sets are bounded
13. Deferred minors in `freshness/adoption.go`
14. Deferred minors in the suite lock, `first_adoption.go` and the handler tests
15. Definition of done — the 90-day simulation, production cadence, return and re-adoption, the escape hatch, self-heal, chaos, and the DoD map
16. Documentation, spec revision 9, overview, deferred files, final verification

---
### Task 1: `Gate.Purge` keeps live entries (R-F2), and the gate's deferred minors

Ruling R-F2 (phase D carry-forward, concurrency M4): a deleter's `release(true)` runs after its commit, so a re-creation of the suite can register an anchor serve before it, and `Purge` dropped that live entry — the re-created suite's next flip then skipped the serve's quiet period. `Purge` now keeps any live entry: a flip in progress or holding, a serve in flight, or one completed within the quiet period; the sweep drops it once idle for twice the quiet period. The reaper (Task 6) and `/forget` (Task 10) rely on it. Four existing tests purge an entry whose serve completed moments before; they move into `synctest` bubbles and let that serve age a quiet period first, so they still prove the purge. Deferred minors in `gate.go` and `gate_test.go`: C-1 (`Close`'s idempotence and its own broadcast to a flip waiting only in phase A, untested), C-2 (`changedLocked`'s doc overstates when the channel exists: reworded), C-3 (one global wake channel: closed with an AIDEV-NOTE stating the bound and why it stays), C-4 (`TestFlip_PhaseAWaitsOutAnotherSuitesHold` tightened to the exact quiet period).

**Files:**
- Modify: `internal/flipgate/gate.go` (`Purge`, `liveLocked`, `changed`'s field comment, `changedLocked`'s doc, `broadcastLocked`'s AIDEV-NOTE), `internal/flipgate/gate_test.go`
- Modify (tests R-F2 changes): `internal/freshness/suite_lock_test.go`, `internal/handler/suite_hit_test.go`, `internal/handler/suite_dod_test.go`

**Interfaces:**
- Consumes: `gatedAdopter`, `lockChecker`, `lockKey`, `newFakeCache` (freshness tests), `simtest.StaticConditional`; `newTestGate`, `testQuiet`, `flipAsync`, `noCommit`, `bg` (flipgate tests).
- Produces: `(*flipgate.Gate).Purge(key string) bool` keeps a live entry (same signature); the unexported `(*entry).liveLocked(now time.Time, quiet time.Duration) bool`.

- [ ] **Step 1: Write the failing tests**

In `internal/flipgate/gate_test.go`, replace `TestGate_PurgeKeepsOldTokensHarmless` — its first purge dropped an entry whose serve was in flight — with:

```go
// Ruling R-F2: Purge drops an idle entry (a deleter's release(true): the
// reaper, cap eviction, POST /forget) but never a live one — a serve in
// flight, a serve that completed within the quiet period (a flip of a
// re-created suite must still wait it out), or a flip in progress. A live
// entry expires on its own; a token of a dropped entry stays harmless.
func TestGate_PurgeSkipsLiveEntries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		old := g.AnchorStart(bg, "k")
		if g.Purge("k") || g.Len() != 1 {
			t.Fatalf("purged an entry with a serve in flight (%d left)", g.Len())
		}
		old.Done()
		time.Sleep(testQuiet - time.Second)
		if g.Purge("k") || g.Len() != 1 {
			t.Fatalf("purged an entry whose serve completed %v ago, within the quiet period (%d left)",
				testQuiet-time.Second, g.Len())
		}
		time.Sleep(time.Second)
		if !g.Purge("k") || g.Len() != 0 {
			t.Fatalf("Purge kept an idle entry (%d left)", g.Len())
		}
		old.Done()
		if g.Len() != 0 {
			t.Fatal("a purged entry's token re-created it")
		}
		start := time.Now()
		if _, err := g.Flip(bg, "k", noCommit); err != nil || time.Since(start) != 0 {
			t.Fatalf("a fresh entry was delayed %v by a purged entry's serve (err %v)", time.Since(start), err)
		}
		release := make(chan struct{})
		out := flipAsync(bg, g, "k2", func() error { <-release; return nil })
		synctest.Wait()
		if g.Purge("k2") {
			t.Fatal("purged an entry whose flip is in progress")
		}
		close(release)
		if o := <-out; o.err != nil {
			t.Fatal(o.err)
		}
	})
}
```

and after it (exec review X18: the flip-count clause of `liveLocked` was unpinned, since that test's flip already holds):

```go
// Ruling R-F2: a flip still in phase A — waiting out another suite's hold,
// with nothing of its own in flight, no completion and no hold yet — keeps
// its entry on Purge: only its flip count says it is live
// (TestGate_PurgeSkipsLiveEntries's flip already holds, which the hold
// clause alone keeps).
func TestGate_PurgeKeepsTheEntryOfAFlipInPhaseA(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		release := make(chan struct{})
		outA := flipAsync(bg, g, "a", func() error { <-release; return nil }) // idle: holds at once
		synctest.Wait()
		outB := flipAsync(bg, g, "b", noCommit) // phase A (b): waits for a's hold to end
		synctest.Wait()
		if g.Purge("b") {
			t.Error("purged the entry of a flip waiting in phase A") // not Fatal: the flips must still drain
		}
		close(release)
		for _, out := range []<-chan flipOut{outA, outB} {
			if o := <-out; o.err != nil {
				t.Fatal(o.err)
			}
		}
	})
}
```

Add, before `TestGate_BarrierActiveGauge`:

```go
// Deferred minor C-1: Close wakes a flip that waits only in phase A — with
// no hold in progress endHoldLocked does not broadcast, so Close's own
// broadcast must — and the flip returns ErrAborted wrapping ErrClosed at
// once, not when its wait would have ended (30 s for the serve in flight).
func TestGate_CloseWakesAFlipWaitingOnlyInPhaseA(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		busy := g.AnchorStart(bg, "a") // in flight: phase A waits for it
		defer busy.Done()
		out := flipAsync(bg, g, "a", func() error { t.Error("committed after Close"); return nil })
		time.Sleep(time.Second)
		closedAt := time.Now()
		g.Close()
		o := <-out
		if !errors.Is(o.err, ErrAborted) || !errors.Is(o.err, ErrClosed) || o.res.Committed || !o.at.Equal(closedAt) {
			t.Fatalf("flip: %+v, %v, %v after Close; want ErrAborted wrapping ErrClosed at once", o.res, o.err, o.at.Sub(closedAt))
		}
	})
}
```

C-1's idempotence: in `TestDisabledGate_IsTodaysBehaviour`, after the `g.Len() != 0` check inside the loop, add

```go
			g.Close() // ignored, and idempotent (deferred minor C-1)
			g.Close()
			if g.Closed() {
				t.Fatalf("%s: Closed after Close, want a disabled gate never closed", name)
			}
```

and in `TestGate_CloseAbortsEveryFlipNotYetCommitting`, right after its `g.Close()`:

```go
		g.Close() // idempotent (deferred minor C-1): a second close(closedCh) would panic
```

C-4: in `TestFlip_PhaseAWaitsOutAnotherSuitesHold`, replace

```go
		if resB.HoldStart.Before(a.res.HoldEnd.Add(testQuiet)) {
			t.Errorf("b's hold started %v after a's ended, want at least the quiet period",
				resB.HoldStart.Sub(a.res.HoldEnd))
		}
```

with

```go
		if !resB.HoldStart.Equal(a.res.HoldEnd.Add(testQuiet)) {
			t.Errorf("b's hold started %v after a's ended, want exactly the quiet period (deferred minor C-4)",
				resB.HoldStart.Sub(a.res.HoldEnd))
		}
```

In `internal/freshness/suite_lock_test.go`, add `"testing/synctest"` and `"github.com/linsomniac/apt-cacher-ultra/internal/simtest"` to the imports, and add after `TestHoldSuite_TakesOnlyAFreeSuiteAndPurgesOnDelete`:

```go
// Ruling R-F2: a deleter's release(true) runs after its commit, so a
// re-creation of the suite can register an anchor serve first. That
// serve's flip-gate entry is live and survives the purge: the re-created
// suite's next flip still waits out the serve's quiet period. The
// deleter's lock-map entry goes.
func TestHoldSuite_ReleaseKeepsARecreatedSuitesLiveGateEntry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ad, _, gate := gatedAdopter(t)
		c := lockChecker(t, newFakeCache(), &simtest.StaticConditional{Status: 304}, ad)
		release, ok := c.HoldSuite(lockKey) // the deleter holds the suite
		if !ok {
			t.Fatal("a free suite was not held")
		}
		gk := flipgate.Key(lockKey.CanonicalScheme, lockKey.CanonicalHost, lockKey.SuitePath)
		tok := gate.AnchorStart(context.Background(), gk) // the re-created suite's serve
		release(true)                                     // the deleter's purge, after its commit
		if gate.Len() != 1 || c.LockCount() != 0 {
			t.Fatalf("after release(true): %d gate entries, %d lock entries; want the live gate entry kept, the lock entry dropped",
				gate.Len(), c.LockCount())
		}
		tok.Done()
		start := time.Now()
		if _, err := gate.Flip(context.Background(), gk, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		if waited := time.Since(start); waited != gate.Quiet() {
			t.Fatalf("the re-created suite's flip waited %v, want the serve's quiet period (%v)", waited, gate.Quiet())
		}
	})
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/flipgate/ ./internal/freshness/ -run 'TestGate_PurgeSkipsLiveEntries|TestGate_PurgeKeepsTheEntryOfAFlipInPhaseA|TestGate_CloseWakesAFlipWaitingOnlyInPhaseA|TestDisabledGate_IsTodaysBehaviour|TestGate_CloseAbortsEveryFlipNotYetCommitting|TestFlip_PhaseAWaitsOutAnotherSuitesHold|TestHoldSuite_ReleaseKeepsARecreatedSuitesLiveGateEntry'`
Expected: FAIL — `TestGate_PurgeSkipsLiveEntries` (`gate_test.go: purged an entry with a serve in flight (0 left)`) and `TestHoldSuite_ReleaseKeepsARecreatedSuitesLiveGateEntry` (`after release(true): 0 gate entries, 0 lock entries; …`). The C-1 and C-4 tests pass: they pin behaviour the code already has; so does `TestGate_PurgeKeepsTheEntryOfAFlipInPhaseA` (the old `Purge` kept an entry whose flip runs, and the new one must still keep it).

- [ ] **Step 3: Implement**

In `internal/flipgate/gate.go`, replace `Purge` with:

```go
// Purge drops key's entry when it is idle, and reports whether it did.
// Spec C8: an entry is purged when its suite's row is deleted — reaped,
// evicted or forgotten (the checker's HoldSuite release(true)). A live
// entry is kept: a flip of it in progress, an anchor serve of it in
// flight, or one that completed within the quiet period. It expires on
// its own once idle for twice the quiet period (sweepLocked).
//
// AIDEV-NOTE: ruling R-F2 (phase D final review, concurrency M4). The
// deleter's release(true) runs after its commit, so a re-creation of the
// suite can register a serve before it; dropping that entry would let the
// re-created suite's next flip skip the serve's quiet period — its client
// could fetch the new snapshot's indexes for the old anchor. Purging a
// live entry was never needed: a dropped entry's tokens touch only it, and
// an idle one goes with the sweep (TestGate_PurgeSkipsLiveEntries,
// TestHoldSuite_ReleaseKeepsARecreatedSuitesLiveGateEntry).
func (g *Gate) Purge(key string) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.entries[key]
	if e == nil || e.liveLocked(time.Now(), g.quiet) {
		return false
	}
	delete(g.entries, key)
	return true
}

// liveLocked reports whether a flip of e is in progress or holds, an
// anchor serve of it is in flight, or one completed within quiet.
func (e *entry) liveLocked(now time.Time, quiet time.Duration) bool {
	return e.flips > 0 || e.hold != nil || len(e.inflight) > 0 ||
		(!e.lastDone.IsZero() && now.Sub(e.lastDone) < quiet)
}
```

C-2: the `changed` field's comment becomes `// closed on every change a flip may wait for; nil until a flip asks for it after a broadcast`, and `changedLocked`'s doc becomes:

```go
// changedLocked returns the channel the next change closes, creating it on
// demand. Only a flip asks for it, at each pass of its wait loop, before it
// knows whether it will wait (deferred minor C-2: a flip that then commits
// at once leaves the channel allocated until the next broadcast — one
// channel, harmless); a change while no flip has asked since the last
// broadcast allocates nothing.
```

C-3: above `func (g *Gate) broadcastLocked() {`, add:

```go
// broadcastLocked wakes every waiting flip: each re-locks the gate and
// re-checks its own conditions.
//
// AIDEV-NOTE: one global channel for every suite (deferred minor C-3,
// closed in phase F): every anchor completion in the cache, of any suite,
// wakes every waiting flip, which re-scans its own suite's in-flight map
// and asks for a new channel and timer. The cost is bounded by the flips
// waiting at once — one or two under the default pool, since a flip keeps
// its global adoption slot through the barrier (ruling R-C1) — for at most
// the barrier's 140 s. Per-entry wake channels would remove it but change
// the wake mechanism every barrier guarantee rests on, for no measured
// gain; revisit if max_concurrent_adoptions grows large.
```

The four tests R-F2 changes move into bubbles and let the evictee's last serve age a quiet period before the purge (a sweep needs twice that, so the entry is still there to purge):

- `internal/freshness/suite_lock_test.go`, `TestHoldSuite_TakesOnlyAFreeSuiteAndPurgesOnDelete`: its body becomes `func testHoldSuiteTakesOnlyAFreeSuiteAndPurgesOnDelete(t *testing.T)`, called as `synctest.Test(t, testHoldSuiteTakesOnlyAFreeSuiteAndPurgesOnDelete)`; its fetcher `newTestFetcher(t)` becomes `&simtest.StaticConditional{Status: 304}` (no client in a bubble); its doc gains "— the gate entry once its last serve is a quiet period old (ruling R-F2), hence the bubble"; and right before the first of its two closing `release(true)` calls (the one followed by `release(true) // idempotent`; `release` runs once, so a sleep before the second comes too late) add `time.Sleep(gate.Quiet()) // the serve's quiet period is over: the entry is idle`.
- `internal/freshness/suite_lock_test.go`, `TestCreateSuite_EvictionUnderTheChecker`: the same move (`testCreateSuiteEvictionUnderTheChecker`, the same fetcher swap, the doc gains "(the gate entry once its last serve is a quiet period old, ruling R-F2; hence the bubble)"), and after `gate.AnchorStart(ctx, flipgate.Key("http", "h", "/b")).Done()` add `time.Sleep(gate.Quiet()) // /b's serve is a quiet period old: its entry is idle`.
- `internal/handler/suite_hit_test.go`, `TestServeHTTP_OnboardingAtTheGlobalCapEvictsUnderTheGuard`: its body becomes `func testOnboardingAtTheGlobalCapEvictsUnderTheGuard(t *testing.T)`, called through `synctest.Test`; its doc gains "A bubble, so the evictee's serves can be a quiet period old: the purge keeps a live entry (ruling R-F2)."; after `first.Close() // drains their T1 checks` add:

```go
	// Ruling R-F2: the evictee's entry is purged only once its last serve
	// is a quiet period old (a live entry expires on its own); twice that
	// would let the sweep drop it first.
	time.Sleep(flipgate.MaxQuiet)
```

- `internal/handler/suite_dod_test.go` (add `"testing/synctest"` to its imports), `TestServeHTTP_MintedSuitesAtScale`: the same move (`testMintedSuitesAtScale`; doc: "A bubble, so the evictees' serves can be a quiet period old: the purge keeps a live entry (ruling R-F2)."), its gate comment becomes (deferred minor D-39's wrong "30 s" goes with it):

```go
	// The gate sweeps an entry idle for twice its quiet period (30 s at
	// the longest quiet period, 15 s; deferred minor D-39); no entry is
	// that idle when the counts are read, so they are exact.
```

  and after `idle.Close() // drains their T1 checks` add:

```go
	// Ruling R-F2: an evictee's gate entry is purged only once its last
	// serve is a quiet period old (a live entry expires on its own); twice
	// that would let the sweep drop it first.
	time.Sleep(flipgate.MaxQuiet)
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/flipgate/ ./internal/freshness/ ./internal/handler/`
Expected: PASS (the new tests and every existing one; the two handler tests run in about 0.5 s and 3 s under `-race`).

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| `Purge`'s condition back to `e == nil \|\| e.flips > 0` | `TestGate_PurgeSkipsLiveEntries` (`purged an entry with a serve in flight (0 left)`), `TestHoldSuite_ReleaseKeepsARecreatedSuitesLiveGateEntry` (`after release(true): 0 gate entries, …`) |
| `liveLocked` without `len(e.inflight) > 0` | `TestGate_PurgeSkipsLiveEntries` (`purged an entry with a serve in flight`), `TestHoldSuite_ReleaseKeepsARecreatedSuitesLiveGateEntry` |
| `liveLocked` without the `lastDone` clause | `TestGate_PurgeSkipsLiveEntries` (`purged an entry whose serve completed 9s ago, within the quiet period`) |
| `now.Sub(e.lastDone) < quiet` → `<= quiet` | `TestGate_PurgeSkipsLiveEntries` (`Purge kept an idle entry (1 left)`) |
| `Close` without its final `g.broadcastLocked()` | `TestGate_CloseWakesAFlipWaitingOnlyInPhaseA` (`… 29s after Close; want … at once`) |
| `Close` without `if g.closed { return }` | `TestGate_CloseAbortsEveryFlipNotYetCommitting` (`panic: close of closed channel`) |
| `holdsQuietLocked` waits `g.quiet + time.Second` (both places) | `TestFlip_PhaseAWaitsOutAnotherSuitesHold` (`b's hold started 11s after a's ended, want exactly the quiet period`) |
| `liveLocked` without `e.flips > 0` | `TestGate_PurgeKeepsTheEntryOfAFlipInPhaseA` (`purged the entry of a flip waiting in phase A`) |

`liveLocked` without `e.hold != nil` is an equivalent mutant (exec review X19): a hold exists only while its flip runs, so the flip count already keeps the entry.

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/flipgate/ internal/freshness/suite_lock_test.go internal/handler/suite_hit_test.go internal/handler/suite_dod_test.go
git add internal/flipgate/ internal/freshness/suite_lock_test.go internal/handler/suite_hit_test.go internal/handler/suite_dod_test.go
git commit -m "Keep a live flip-gate entry on purge (ruling R-F2); deferred minors C-1 to C-4"
```

(Record the mutation table in the commit message.)

### Task 2: One suite-row delete path: the cache's eviction split, the single-DELETE guard and `suite_rows.go`'s deferred minors

Ruling R-F1's guard and the refactor the reaper builds on, with no behaviour change. `deleteSuiteTx` moves to a new `suite_reap.go`, where Task 3 adds its two other callers; cap eviction's code moves to `suite_evict.go` beside `suite_evict_test.go` (deferred minor D-14 — `deleteSuiteTx` goes with the reaper rather than with eviction, since three callers now share it). No guard existed for the one delete: `TestSuiteFreshnessHasOneDeleteSite` pins the only `DELETE FROM suite_freshness`, and `TestSuiteRowWritersHaveKnownCallers` pins the in-package callers of `createSuiteTx` (deferred minor D-8) and of `deleteSuiteTx`. Deferred minors in `suite_rows.go`: D-11 (`reportFutureInterest`'s "furthest" unpinned: a test with three future stamps), D-12 (three inaccurate comments the reaper builds on; F-tagged), D-13 (`(*Cache).SuiteExists` shares its name with the `SuiteExists` outcome: renamed `HasSuiteRow`), D-15 (creation errors say "CreateSuite:" on the miss path: they now say "create suite row" or carry the caller's name), D-20 (a failed submit whose eviction committed: its purge gets an end-to-end test, through a test seam that runs after the commit).

**Files:**
- Create: `internal/cache/suite_evict.go`, `internal/cache/suite_reap.go`
- Modify: `internal/cache/suite_rows.go`, `internal/cache/cache.go` (`committedHook`), `internal/cache/single_insert_test.go`, `internal/cache/suite_evict_test.go`
- Modify (mechanical rename, ruling PD-1): `internal/cache/suite_rows_test.go`, `internal/freshness/freshness.go`, `internal/freshness/freshness_test.go`, `internal/freshness/suite_lock_test.go`, `internal/handler/handler.go`, `internal/handler/suite_hit_test.go`, `internal/handler/suite_dod_test.go`, `internal/handler/suite_rows_test.go`

**Interfaces:**
- Consumes: cache test helpers `openCache` (`cache_test.go`), `stubNow` (`gc_test.go`), `productionGoFiles`, `stringLiterals` (`single_insert_test.go`), `fourRows`, `seedRow`, `sk`, `rowExists`, `newFakeGuard`, `clockGuard`, `evictNow`, `evictDay` (`suite_evict_test.go`).
- Produces: `(*cache.Cache).HasSuiteRow(ctx context.Context, key cache.SuiteKey) (bool, error)` (the method formerly `SuiteExists`; `freshness.Cache` names it too); the test seam `Cache.committedHook func()`; the test helper `callSites(t *testing.T, name string) []string` (`single_insert_test.go`); `deleteSuiteTx` and `deleteOutcome` unchanged, now in `suite_reap.go`.

- [ ] **Step 1: Write the failing tests**

In `internal/cache/single_insert_test.go`, add `"slices"` to the imports, add after `insertSuiteFreshnessRE`:

```go
// deleteSuiteFreshnessRE matches a statement that deletes from
// suite_freshness, with the same quoting and schema forms.
var deleteSuiteFreshnessRE = regexp.MustCompile("(?is)\\bDELETE\\s+FROM\\s+" +
	"(?:[\"`\\[]?\\w+[\"`\\]]?\\s*\\.\\s*)?[\"`\\[]?suite_freshness\\b")
```

and, before `TestOnlyTheHandlerCreatesSuiteRows`:

```go
// Ruling R-F1 (spec C3, C9): deleteSuiteTx — the compare-and-delete cap
// eviction, the reaper and POST /forget share — holds the only DELETE FROM
// suite_freshness in production code.
func TestSuiteFreshnessHasOneDeleteSite(t *testing.T) {
	var sites []string
	productionGoFiles(t, func(p string, src []byte) {
		stringLiterals(t, p, src, func(funcName, lit string) {
			for range deleteSuiteFreshnessRE.FindAllStringIndex(lit, -1) {
				sites = append(sites, p+":"+funcName)
			}
		})
	})
	const want = "../../internal/cache/suite_reap.go:deleteSuiteTx"
	if len(sites) != 1 || sites[0] != want {
		t.Fatalf("DELETE FROM suite_freshness sites = %v, want exactly [%s]", sites, want)
	}
}

// callSites returns "file:function" for every call of the package
// function name in internal/cache's production code, sorted.
func callSites(t *testing.T, name string) []string {
	t.Helper()
	var out []string
	productionGoFiles(t, func(p string, src []byte) {
		if !strings.HasPrefix(p, "../../internal/cache/") {
			return
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fd, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if id, ok := call.Fun.(*ast.Ident); ok && id.Name == name {
						out = append(out, filepath.Base(p)+":"+fd.Name.Name)
					}
				}
				return true
			})
		}
	})
	slices.Sort(out)
	return out
}

// Deferred minor D-8 and ruling R-F1: the one INSERT and the one DELETE
// of suite_freshness are reached only from known callers, so a new writer
// that "re-creates on 0 rows", or a second delete path, fails here.
func TestSuiteRowWritersHaveKnownCallers(t *testing.T) {
	for _, c := range []struct {
		fn   string
		want []string
	}{
		{"createSuiteTx", []string{"fetched_row.go:storeFetchedTx", "suite_rows.go:CreateSuite", "testing_helpers.go:SeedSuiteForTest"}},
		{"deleteSuiteTx", []string{"suite_rows.go:createSuiteTx"}},
	} {
		if got := callSites(t, c.fn); !slices.Equal(got, c.want) {
			t.Errorf("%s callers = %v, want %v", c.fn, got, c.want)
		}
	}
}
```

In `internal/cache/suite_evict_test.go`, add `"strings"` to the imports, and before `TestAfterCreate_ReadsTheCandidatesRowUnlessTheResultSays`:

```go
// Deferred minor D-11: of several suites whose last_requested_at lies in
// the future, cap eviction reports the furthest one.
func TestCreateSuite_ReportsTheFurthestFutureStamp(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, evictNow)()
	seedRow(t, c, sk("a", "/near"), evictNow+evictDay)
	seedRow(t, c, sk("a", "/far"), evictNow+3*evictDay)
	seedRow(t, c, sk("a", "/mid"), evictNow+2*evictDay)
	g := &clockGuard{fakeGuard: newFakeGuard()}
	if _, err := c.CreateSuite(ctx, SuiteSeed{SuiteKey: sk("e", "/new"), RequestedAt: evictNow}, SuiteCaps{Global: 3}, g); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("last_requested_at a/far %d %d", 3*evictDay, evictNow); !slices.Equal(g.reports, []string{want}) {
		t.Fatalf("reports = %q, want [%q]", g.reports, want)
	}
}

// Deferred minor D-20 (design review M3): a creation whose submit fails
// although its transaction committed — its context ends right after the
// commit, before the result is handed back — purges the evictee as a
// successful one does: afterCreate reads the row, finds it gone, forgets
// the evictee's touch entry and releases it as deleted.
func TestCreateSuite_AFailedSubmitWhoseEvictionCommittedPurges(t *testing.T) {
	defer stubNow(t, evictNow)()
	c := openCache(t)
	fourRows(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	resume := make(chan struct{})
	var once sync.Once
	c.committedHook = func() { once.Do(func() { cancel(); <-resume }) }
	g := newFakeGuard()
	_, err := c.CreateSuite(ctx, SuiteSeed{SuiteKey: sk("e", "/new"), RequestedAt: evictNow}, SuiteCaps{Global: 4}, g)
	close(resume)
	if err == nil {
		t.Fatal("premise: the submit fails on the context that ended after the commit")
	}
	if rowExists(t, c, sk("b", "/idle")) {
		t.Fatal("premise: the eviction committed")
	}
	if got := g.released[sk("b", "/idle")]; !slices.Equal(got, []bool{true}) {
		t.Fatalf("released %v; want b/idle released once, as deleted", g.released)
	}
	c.touches.mu.Lock()
	_, touched := c.touches.last[sk("b", "/idle")]
	c.touches.mu.Unlock()
	if touched {
		t.Fatal("the evictee's touch entry stayed")
	}
}

// Deferred minor D-15: a failed suite-row creation names no caller it was
// not: the miss path (StoreFetched) and onboarding (CreateSuite) both say
// "create suite row", never "CreateSuite:".
func TestCreateSuite_ErrorsNameTheOperation(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	if _, err := c.db.Exec(`CREATE TRIGGER inject_insert_failure BEFORE INSERT ON suite_freshness
BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatal(err)
	}
	seed := SuiteSeed{SuiteKey: sk("h", "/r/dists/x"), RequestedAt: 1}
	_, cerr := c.CreateSuite(ctx, seed, SuiteCaps{}, nil)
	_, serr := c.StoreFetched(ctx, FetchedRow{Row: URLPath{CanonicalScheme: "http", CanonicalHost: "h",
		Path: "/r/dists/x/InRelease", UpstreamURL: "http://h/r/dists/x/InRelease", IsMetadata: true},
		Suite: seed.SuiteKey, Create: &seed}, SuiteCaps{}, nil)
	for name, err := range map[string]error{"CreateSuite": cerr, "StoreFetched": serr} {
		if err == nil || !strings.Contains(err.Error(), "create suite row: insert") || strings.Contains(err.Error(), "CreateSuite:") {
			t.Errorf("%s: err = %v, want it to say \"create suite row: insert\" and never \"CreateSuite:\"", name, err)
		}
	}
}
```

In `internal/cache/suite_rows_test.go`, rename `TestSuiteExists` to `TestHasSuiteRow`, its doc's first word to `HasSuiteRow`, and its three `c.SuiteExists(` calls to `c.HasSuiteRow(`.

- [ ] **Step 2: Run to verify failure**

Run: `go test -gcflags=-e ./internal/cache/ -run 'TestSuiteFreshnessHasOneDeleteSite|TestSuiteRowWritersHaveKnownCallers|TestCreateSuite_ReportsTheFurthestFutureStamp|TestCreateSuite_AFailedSubmitWhoseEvictionCommittedPurges|TestCreateSuite_ErrorsNameTheOperation|TestHasSuiteRow'`
Expected: compile failures naming `c.committedHook undefined` and `c.HasSuiteRow undefined` (three times).

- [ ] **Step 3: Implement**

Create `internal/cache/suite_evict.go` (`package cache`; imports `context`, `fmt`, `time`) with this header, then the following moved **verbatim** from `suite_rows.go`: the constants `suiteEvictIdle`, `suiteEvictCandidates` and `futureStampSlack` (one `const (…)` block, keeping their comments and the "Cap eviction (spec C3): …" comment above it), `ClockStepReporter`, `heldSuite` with `settle`, `holdEvictionCandidate` and `reportFutureInterest`:

```go
// AIDEV-NOTE: cap eviction (spec C3), split out of suite_rows.go
// (deferred minor D-14). The delete itself, deleteSuiteTx, lives in
// suite_reap.go beside the reaper's and POST /forget's use of it.
```

with two changes to `holdEvictionCandidate`: its five error messages' prefix `CreateSuite: ` becomes `hold eviction candidate: ` (`count suites`; `list eviction candidates`, three times: the query, `rows.Close` and `rows.Err`; `scan eviction candidate`; deferred minor D-15), and its doc's first paragraph (through "lies in the future.", up to the blank `//` line before the AIDEV-NOTE) becomes (D-12 (a), D-15):

```go
// holdEvictionCandidate returns a held eviction candidate when the table is
// at, above or within suiteEvictCandidates of the global cap (a read
// outside the writer; createSuiteTx re-checks everything, and afterCreate
// releases an unused candidate as not deleted — or as deleted when its row
// is gone, a concurrent creation having evicted it; deferred minor D-12),
// or nil: the least recently requested of at most suiteEvictCandidates
// evictable suites whose mutex guard takes at once (a nil guard takes the
// first, with a no-op release). busy reports that it found candidates but
// every one was busy. A guard that is a ClockStepReporter is told of a
// suite whose last_requested_at lies in the future. Its errors name no
// operation: creationAttempt prefixes the caller's (deferred minor D-15).
```

Create `internal/cache/suite_reap.go` (`package cache`; imports `context`, `database/sql`, `errors`, `fmt`) holding `deleteOutcome`, its constants and `deleteSuiteTx`, moved verbatim from `suite_rows.go`, with `deleteSuiteTx`'s AIDEV-NOTE ending:

```go
// (TestCreateSuite_EvictionTouchesOnlyTheEvictee,
// TestCreateSuite_EvictionLeavesTheOtherSchemesTwin). Keep it the single
// DELETE FROM suite_freshness (TestSuiteFreshnessHasOneDeleteSite, ruling
// R-F1; moved here from suite_rows.go, deferred minor D-14).
```

In `internal/cache/suite_rows.go`:
- Delete the moved code. The `const` block keeps only `suiteRowGoneTimeout`, as

```go
// suiteRowGoneTimeout bounds afterCreate's read of a held candidate's row
// after a failed submit. (Cap eviction's constants are in suite_evict.go.)
const suiteRowGoneTimeout = 5 * time.Second
```

- `creationAttempt`: `holdEvictionCandidate`'s error is returned as `fmt.Errorf("%s: %w", op, err)`; and right after its `tx.Commit()` succeeds (before `got = r`):

```go
		if c.committedHook != nil {
			c.committedHook()
		}
```

- `afterCreate`'s doc and AIDEV-NOTE become (D-12 (b), (c)):

```go
// afterCreate settles a creation attempt after its writer operation. A
// created row counts as touched (its insert stamped last_requested_at),
// and the held eviction candidate is released, telling the guard whether
// its row is gone — deleted by this creation's eviction, or found absent —
// so that release(true), and the dropped touch-coalescing entry, purge the
// suite's in-memory state when this creation saw its row gone. A
// re-creation of the evictee that lands between that commit or read and
// the release keeps its live flip-gate entry (ruling R-F2); its lock-map
// and touch entries come back with its next check and touch (deferred
// minor D-12). Unless the result says so, it reads the row: after a failed
// submit the outcome is unknown, and an unused candidate's row may have
// been evicted by a concurrent creation before this one held it (the hold
// then made a fresh lock-map entry for a suite without a row).
//
// AIDEV-NOTE: while this creation holds the candidate's mutex no other
// deleter can remove its row (every deleter holds the guard). A failed
// submit can still commit: when the commit raced the context's end in
// submitWrite (an operation run on an ended context rolls back), and when
// submitWrite returned ErrClosed while the writer ran the operation with a
// live context (at shutdown; the read then fails and answers "present")
// (deferred minor D-12). So the read decides (design review M3, M4;
// TestCreateSuite_AFailedSubmitWhoseEvictionCommittedPurges). A read
// error, or a read that lands before a commit still in flight, answers
// "present": the evictee's lock-map, flip-gate and touch entries then stay
// (one each, bounded), and a check that starts meanwhile meets a vanished
// row, which I-4's re-validation and the UPDATE-only writers make harmless
// (its candidate, if any, was retired: CommitAdoption refuses it).
```

- `createSuiteTx`'s doc gains, after "(spec C3).": `Its errors say "create suite row", which both callers do — onboarding (CreateSuite) and a miss (StoreFetched) — never one caller's name (deferred minor D-15).`; in `createSuiteTx` and `markCreated` every error prefix `CreateSuite:` becomes `create suite row:` (`read row`, `count host suites`, `count suites`, `insert`, `read cached anchor %s`, and markCreated's `fmt.Errorf("create suite row: %w", err)`).
- `SuiteExists` becomes `HasSuiteRow` (its error prefix too), with the doc:

```go
// HasSuiteRow reports whether the suite has a suite_freshness row: the
// handler's cheap tracked-suite test (one indexed read; the flip barrier
// registers only tracked suites' anchor serves), the checker's I-4 row
// gate and afterCreate's decider. (Renamed from SuiteExists, which is also
// a CreateOutcome; deferred minor D-13.)
```

  and `suiteRowGone` calls `c.HasSuiteRow(ctx, key)`.

In `internal/cache/cache.go`, add to `Cache` after `heldHook`:

```go
	// committedHook, when set (tests only), runs on the writer goroutine
	// right after a creation attempt's transaction commits, before its
	// result is handed back: it lets a test end the creator's context
	// there, so a submit fails although its eviction committed
	// (TestCreateSuite_AFailedSubmitWhoseEvictionCommittedPurges, deferred
	// minor D-20).
	committedHook func()
```

The rename's other sites (mechanical, PD-1): every `.SuiteExists(` method call becomes `.HasSuiteRow(` in `internal/freshness/freshness.go`, `internal/handler/handler.go` and the handler tests; the `freshness.Cache` interface method and its doc comment (`// HasSuiteRow is I-4's row gate …`), `fakeCache`'s method and its two comments (`freshness_test.go`), the two mentions in `suite_lock_test.go` (`existence read (HasSuiteRow)`, `of them HasSuiteRow;`), `handler.go`'s `EXISTS read (HasSuiteRow; deferred minor C-21)` and `suite_dod_test.go`'s `HasSuiteRow: %v` message. The `SuiteExists` `CreateOutcome` constant is unchanged:

```bash
sed -i 's/\.SuiteExists(/.HasSuiteRow(/g' internal/handler/suite_hit_test.go internal/handler/suite_dod_test.go internal/handler/suite_rows_test.go internal/handler/handler.go internal/freshness/freshness.go
sed -i 's/\tSuiteExists(ctx context.Context, key cache.SuiteKey) (bool, error)/\tHasSuiteRow(ctx context.Context, key cache.SuiteKey) (bool, error)/; s|// SuiteExists is I-4.s row gate|// HasSuiteRow is I-4'"'"'s row gate|' internal/freshness/freshness.go
sed -i 's/^func (f \*fakeCache) SuiteExists(/func (f *fakeCache) HasSuiteRow(/; s|// existsCalls counts SuiteExists reads|// existsCalls counts HasSuiteRow reads|; s|^// SuiteExists is the I-4 row gate.s existence read|// HasSuiteRow is the I-4 row gate'"'"'s existence read|' internal/freshness/freshness_test.go
sed -i 's|existence read (SuiteExists)|existence read (HasSuiteRow)|; s|of them SuiteExists;|of them HasSuiteRow;|' internal/freshness/suite_lock_test.go
sed -i 's|EXISTS read (SuiteExists; deferred|EXISTS read (HasSuiteRow; deferred|' internal/handler/handler.go
sed -i 's|%s%s: SuiteExists: %v|%s%s: HasSuiteRow: %v|' internal/handler/suite_dod_test.go
grep -rn 'SuiteExists' --include=*.go . | grep -v 'SuiteExists\b *[,})]\|Outcome[ !=:]*SuiteExists\|case SuiteExists\|cache\.SuiteExists'
```

The last command must print three lines, all in `suite_rows.go`: the `SuiteExists` outcome constant, its comment and `CreateSuite`'s doc mention of it — no method call or method name is left.

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/cache/ ./internal/freshness/ ./internal/handler/`
Expected: PASS.

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| a second `DELETE FROM suite_freshness` (e.g. `_ = "DELETE FROM suite_freshness"` in `afterCreate`) | `TestSuiteFreshnessHasOneDeleteSite` |
| a new in-package caller: `if s.SuitePath == "\x00" { _, _ = deleteSuiteTx(ctx, tx, s.SuiteKey, nil, 0, 0) }` at the top of `restampSuiteTx` | `TestSuiteRowWritersHaveKnownCallers` (`deleteSuiteTx callers = [fetched_row.go:restampSuiteTx …]`) |
| `reportFutureInterest`'s `ORDER BY last_requested_at DESC` → `ASC` | `TestCreateSuite_ReportsTheFurthestFutureStamp` (`reports = ["… a/near …"]`) |
| `creationAttempt` calls `afterCreate` only when the submit succeeded (`if create != nil && err == nil`) | `TestCreateSuite_AFailedSubmitWhoseEvictionCommittedPurges` (`released map[…:[false]]; want b/idle released once, as deleted`) |
| `afterCreate`'s `(!res.heldKept && c.suiteRowGone(held.key))` → `false` | `TestCreateSuite_AFailedSubmitWhoseEvictionCommittedPurges` |
| `createSuiteTx`'s `create suite row: insert` back to `CreateSuite: insert` | `TestCreateSuite_ErrorsNameTheOperation` (both legs) |

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/cache/suite_rows.go internal/cache/suite_evict.go internal/cache/suite_reap.go internal/cache/cache.go internal/cache/single_insert_test.go internal/cache/suite_evict_test.go internal/cache/suite_rows_test.go internal/freshness/freshness.go internal/freshness/freshness_test.go internal/freshness/suite_lock_test.go internal/handler/
git add internal/cache/ internal/freshness/freshness.go internal/freshness/freshness_test.go internal/freshness/suite_lock_test.go internal/handler/
git commit -m "One suite-row delete path: split cap eviction, guard the single DELETE; deferred minors D-8, D-11 to D-15, D-20"
```

(Record the mutation table in the commit message; `gofmt -l` prints nothing — none of these is a drift file.) Review the commit with `git show --color-moved=zebra HEAD` (design review, ruling PF-8): the code moved verbatim to `suite_evict.go` and `suite_reap.go` shows as moved, so only the changes this step names stand out.

### Task 3: The cache's reap, forget and retire operations

Spec C9's cache side: the batched metadata deletion (step 1), `ReapIdleSuite` (step 2) and `POST /forget`'s `ForgetSuite` — each one writer transaction (the deletion one per batch), both through `deleteSuiteTx` and the reaper's retirement path it runs (`retireSuiteSnapshotsTx`; ruling R-F1). `ReapRegardless` lets `deleteSuiteTx` delete a row whatever its interest stamp (design note 6). `ForgetSuite` deletes the base's signed-seen markers under both schemes, and the cached `InRelease`, `Release` and `Release.gpg` rows of the base and its `/.` twin under both schemes (ruling PF-2), only when no suite row of the host is left for the base or the twin under either scheme; otherwise it reports the rows that kept them (`KeptBy`, ruling PF-7; design note 13; ruling R-F5). A reap moves the base's markers up to the reap time, `MAX(last_seen_at, now)`, so the orphan sweep's year counts from the end of tracking (design review M-9, ruling PF-8). Both report whether the suite is gone, settling a failed submit — which can follow a committed transaction — by a fresh read of the row, as `afterCreate` does (design review M-2); the reaper releases its hold with that answer. Two guards extend R-F1's across packages (design review M-5): outside `internal/cache` only `internal/freshness/reaper.go` and `forget.go` call the deleters, and only `ForgetSuite`, the orphan sweep (Task 9) and the test-only `ClearSignedSeenForTest` delete markers. There is no `RetireSuiteSnapshots`: ruling PF-5 dropped the knob-off retirement that was its only caller. The DoD's "twin and nested suites untouched" and "batched metadata deletion never repeats the refcount decrement" are pinned here at the cache level.

**Files:**
- Modify: `internal/cache/suite_reap.go`, `internal/cache/cache.go` (`committedHook`'s doc), `internal/cache/signed_seen.go` (`markSignedSeen`'s writers note), `internal/cache/single_insert_test.go` (the callers list, two guards, the test-only call guard), `internal/cache/suite_evict_test.go` (two comments)
- Create: `internal/cache/suite_reap_test.go`

**Interfaces:**
- Consumes: `deleteSuiteTx`, `deleteOutcome`, the test seam `committedHook` (Task 2); `suiteRowGone` (`suite_rows.go`); `decrementSnapshotRefcountsTx` (`adoption.go`); `deleteURLPathRowTx` (`gc_urlpath.go`); `forgetTouch`, `noteTouch` (`suite_touch.go`); `aptpath.SuitePath`, `aptpath.MarkerBase`; cache test helpers `openCache`, `seedBlob`, `blobRefcount` (`cache_test.go`), `stubNow` (`gc_test.go`), `putURLPathRow` (`gc_urlpath_test.go`), `seedRow`, `sk`, `rowExists`, `evictNow`, `evictDay` (`suite_evict_test.go`), `productionGoFiles`, `stringLiterals` (`single_insert_test.go`).
- Produces: `cache.ReapRegardless int64`; `(*cache.Cache).DeleteSuiteMetadata(ctx context.Context, key SuiteKey) (int, error)`; `(*cache.Cache).ReapIdleSuite(ctx context.Context, key SuiteKey, wantCurrent *int64, cutoff int64) (deleted, gone bool, err error)`; `cache.ForgetResult{Deleted, Gone bool; MarkersDeleted, AnchorRowsDeleted int; KeptBy []SuiteKey}` and `(*cache.Cache).ForgetSuite(ctx context.Context, key SuiteKey, wantCurrent *int64) (ForgetResult, error)`; the unexported `retireSuiteSnapshotsTx`, `suiteMetadataPaths`, `deleteCachedRowTx`, `deleteURLPathRows`, `baseSuiteRowsTx`, `deleteBaseAnchorsTx`, `reapMetadataBatch` (1000); test helpers `adoptWith(t, c, key, members map[string]string) (int64, map[string]string)`, `urlRowExists(t, c, scheme, host, path) bool`, `isTouched(c, key) bool` (`suite_reap_test.go`), the regexp `deleteSignedSeenRE` (`single_insert_test.go`).

- [ ] **Step 1: Write the failing tests**

Create `internal/cache/suite_reap_test.go`:

```go
package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
)

// adoptWith commits a snapshot of key's suite (its row must exist) whose
// members are path → content, with members["InRelease"] as its anchor,
// and returns the snapshot's id and each member's blob hash.
func adoptWith(t *testing.T, c *Cache, key SuiteKey, members map[string]string) (int64, map[string]string) {
	t.Helper()
	ctx := context.Background()
	hashes := map[string]string{}
	for p, content := range members {
		hashes[p] = seedBlob(t, c, content)
	}
	anchor := hashes["InRelease"]
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{CanonicalScheme: key.CanonicalScheme,
		CanonicalHost: key.CanonicalHost, SuitePath: key.SuitePath, InReleaseHash: &anchor})
	if err != nil {
		t.Fatal(err)
	}
	var rows []SnapshotMember
	for p, h := range hashes {
		rows = append(rows, SnapshotMember{SnapshotID: id, Path: p, BlobHash: h, DeclaredSHA256: h})
	}
	if err := c.CommitAdoption(ctx, id, rows, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	return id, hashes
}

// urlRowExists reports whether (scheme, host, path) has a url_path row.
func urlRowExists(t *testing.T, c *Cache, scheme, host, path string) bool {
	t.Helper()
	_, err := c.LookupURL(context.Background(), scheme, host, path)
	if err != nil && !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

// isTouched reports whether key has a touch-coalescing entry.
func isTouched(c *Cache, key SuiteKey) bool {
	c.touches.mu.Lock()
	defer c.touches.mu.Unlock()
	_, ok := c.touches.last[key]
	return ok
}

// Spec C9 step 2 (ruling R-F1): ReapIdleSuite is deleteSuiteTx's
// compare-and-delete — kept (neither deleted nor gone) when the suite's
// current snapshot is not the one listed or a client asked for it after
// the cutoff — and on delete it releases the current snapshot's pins once,
// retires every snapshot of the suite (displaced and never-adopted ones
// too) and drops the touch entry. ReapRegardless deletes whatever the
// interest stamp says, NULL or future.
func TestReapIdleSuite_RetiresEverySnapshotAndDeletesTheRow(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, evictNow)()
	key := sk("h", "/r/dists/x")
	seedRow(t, c, key, evictNow-40*evictDay)
	old, _ := adoptWith(t, c, key, map[string]string{"InRelease": "v1", "main/Packages": "pkgs v1"})
	cur, hashes := adoptWith(t, c, key, map[string]string{"InRelease": "v2", "main/Packages": "pkgs v2"})
	next := seedBlob(t, c, "v3")
	cand, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h",
		SuitePath: key.SuitePath, InReleaseHash: &next})
	if err != nil {
		t.Fatal(err)
	}
	cutoff := evictNow - 30*evictDay
	if del, gone, err := c.ReapIdleSuite(ctx, key, &old, cutoff); err != nil || del || gone {
		t.Fatalf("another current snapshot listed: deleted %v, gone %v, err %v; want kept", del, gone, err)
	}
	if del, gone, err := c.ReapIdleSuite(ctx, key, &cur, evictNow-41*evictDay); err != nil || del || gone {
		t.Fatalf("requested after the cutoff: deleted %v, gone %v, err %v; want kept", del, gone, err)
	}
	if n := blobRefcount(t, c, hashes["main/Packages"]); n != 1 {
		t.Fatalf("a kept suite's current member has refcount %d, want its pin (1)", n)
	}
	c.noteTouch(key)
	if del, gone, err := c.ReapIdleSuite(ctx, key, &cur, cutoff); err != nil || !del || !gone {
		t.Fatalf("ReapIdleSuite = %v, %v, %v; want deleted and gone", del, gone, err)
	}
	if rowExists(t, c, key) || isTouched(c, key) {
		t.Fatalf("after the reap: row %v, touch entry %v; want neither", rowExists(t, c, key), isTouched(c, key))
	}
	for _, id := range []int64{old, cur, cand} {
		if snap, err := c.GetSuiteSnapshot(ctx, id); err != nil || snap.RetiredAt == nil {
			t.Fatalf("snapshot %d = %+v, %v; want retired", id, snap, err)
		}
	}
	if n := blobRefcount(t, c, hashes["main/Packages"]); n != 0 {
		t.Fatalf("the current member's refcount = %d, want its pin released once (0)", n)
	}
	for name, stamp := range map[string]sql.NullInt64{"NULL stamp": {}, "future stamp": {Int64: evictNow + evictDay, Valid: true}} {
		k := sk("h", "/r/dists/"+strings.Fields(name)[0])
		seedRow(t, c, k, evictNow)
		if _, err := c.db.Exec(`UPDATE suite_freshness SET last_requested_at = ? WHERE suite_path = ?`, stamp, k.SuitePath); err != nil {
			t.Fatal(err)
		}
		if del, gone, err := c.ReapIdleSuite(ctx, k, nil, evictNow); err != nil || del || gone {
			t.Fatalf("%s, idle cutoff: deleted %v, gone %v, err %v; want kept", name, del, gone, err)
		}
		if del, gone, err := c.ReapIdleSuite(ctx, k, nil, ReapRegardless); err != nil || !del || !gone {
			t.Fatalf("%s, ReapRegardless: deleted %v, gone %v, err %v; want deleted", name, del, gone, err)
		}
	}
}

// Spec C9 step 1: DeleteSuiteMetadata deletes exactly the suite's metadata
// url_path rows — the paths whose suite is this one — and never a nested
// suite's, a sibling sharing its prefix, a .deb, the /. twin's path, or the
// same path under the other scheme or another host. A second call deletes
// nothing.
func TestDeleteSuiteMetadata_DeletesOnlyTheSuitesRows(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/r/dists/x")
	blob := seedBlob(t, c, "metadata")
	put := func(scheme, host, path string) {
		putURLPathRow(t, c, scheme, host, path, blob, sql.NullInt64{Int64: 1, Valid: true}, true)
	}
	mine := []string{"/r/dists/x/InRelease", "/r/dists/x/Release", "/r/dists/x/Release.gpg",
		"/r/dists/x/main/binary-amd64/Packages.gz", "/r/dists/x/main/binary-amd64/by-hash/SHA256/" + strings.Repeat("ab", 32)}
	others := []string{
		"/r/dists/x/dists/y/InRelease",       // a nested suite's: SuitePath takes the last dists
		"/r/dists/xy/InRelease",              // a sibling sharing the prefix
		"/r/pool/main/f/foo/foo_1_amd64.deb", // no suite's metadata
		"/r/dists/x/./InRelease",             // the /. twin's path, not this suite's
	}
	for _, p := range append(append([]string{}, mine...), others...) {
		put("http", "h", p)
	}
	put("https", "h", "/r/dists/x/InRelease")
	put("http", "other", "/r/dists/x/InRelease")
	if n, err := c.DeleteSuiteMetadata(ctx, key); err != nil || n != len(mine) {
		t.Fatalf("DeleteSuiteMetadata = %d, %v; want the suite's %d rows", n, err, len(mine))
	}
	for _, p := range mine {
		if urlRowExists(t, c, "http", "h", p) {
			t.Errorf("%s was not deleted", p)
		}
	}
	for _, p := range others {
		if !urlRowExists(t, c, "http", "h", p) {
			t.Errorf("%s was deleted, want it kept", p)
		}
	}
	if !urlRowExists(t, c, "https", "h", "/r/dists/x/InRelease") || !urlRowExists(t, c, "http", "other", "/r/dists/x/InRelease") {
		t.Error("the other scheme's or another host's row was deleted")
	}
	if n, err := c.DeleteSuiteMetadata(ctx, key); err != nil || n != 0 {
		t.Fatalf("a second call = %d, %v; want nothing left to delete", n, err)
	}
}

// Spec C9 step 1: the rows go in writer transactions of at most 1,000,
// plain metadata first and the anchors last. A failure in the second batch
// leaves the first batch deleted and the anchors in place; the next call
// deletes the rest.
func TestDeleteSuiteMetadata_BatchesOfAThousandAnchorsLast(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/r/dists/x")
	blob := seedBlob(t, c, "index")
	tx, err := c.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	insert := func(p string) {
		if _, err := tx.Exec(`INSERT INTO url_path (canonical_scheme, canonical_host, path, blob_hash, upstream_url, is_metadata)
VALUES ('http', 'h', ?, ?, ?, 1)`, p, blob, "http://h"+p); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 1200 {
		insert(fmt.Sprintf("/r/dists/x/main/i%04d", i))
	}
	anchors := []string{"/r/dists/x/InRelease", "/r/dists/x/Release", "/r/dists/x/Release.gpg"}
	for _, p := range anchors {
		insert(p)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`CREATE TRIGGER fail_one BEFORE DELETE ON url_path WHEN OLD.path = '/r/dists/x/main/i1100'
BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if n, err := c.DeleteSuiteMetadata(ctx, key); err == nil || n != 1000 {
		t.Fatalf("with the second batch failing: %d deleted, err %v; want the first batch's 1,000 and an error", n, err)
	}
	for _, p := range anchors {
		if !urlRowExists(t, c, "http", "h", p) {
			t.Fatalf("%s was deleted before the plain metadata was gone", p)
		}
	}
	if _, err := c.db.Exec(`DROP TRIGGER fail_one`); err != nil {
		t.Fatal(err)
	}
	if n, err := c.DeleteSuiteMetadata(ctx, key); err != nil || n != 203 {
		t.Fatalf("the next call = %d, %v; want the 200 plain rows and 3 anchors left", n, err)
	}
}

// Spec §7 C9: batched metadata deletion never repeats a refcount
// decrement. A url_path row releases its blob's pin once, when it is
// deleted; the current snapshot's pins are released once, by the final
// step that deletes the row — not by a final step that finds the suite
// requested again, and not again by the next run.
func TestDeleteSuiteMetadata_NeverRepeatsADecrement(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, evictNow)()
	key := sk("h", "/r/dists/x")
	seedRow(t, c, key, evictNow-40*evictDay)
	cur, hashes := adoptWith(t, c, key, map[string]string{"InRelease": "anchor", "main/Packages": "pkgs", "main/Sources": "srcs"})
	// main/Packages is also cached in url_path (a client's miss); main/Sources only in the snapshot.
	putURLPathRow(t, c, "http", "h", "/r/dists/x/main/Packages", hashes["main/Packages"], sql.NullInt64{}, true)
	pkgs, srcs := hashes["main/Packages"], hashes["main/Sources"]
	cutoff := evictNow - 30*evictDay
	if _, err := c.DeleteSuiteMetadata(ctx, key); err != nil {
		t.Fatal(err)
	}
	if del, _, err := c.ReapIdleSuite(ctx, key, &cur, evictNow-41*evictDay); err != nil || del {
		t.Fatalf("a final step that finds the suite requested again: deleted %v, err %v; want kept", del, err)
	}
	if p, s := blobRefcount(t, c, pkgs), blobRefcount(t, c, srcs); p != 0 || s != 1 {
		t.Fatalf("after a kept reap: Packages %d, Sources %d; want 0 (its url_path row's pin released) and 1 (untouched)", p, s)
	}
	if n, err := c.DeleteSuiteMetadata(ctx, key); err != nil || n != 0 {
		t.Fatalf("the next run's batches = %d, %v; want nothing deleted twice", n, err)
	}
	if del, _, err := c.ReapIdleSuite(ctx, key, &cur, cutoff); err != nil || !del {
		t.Fatalf("the final step = %v, %v; want deleted", del, err)
	}
	if p, s := blobRefcount(t, c, pkgs), blobRefcount(t, c, srcs); p != -1 || s != 0 {
		t.Fatalf("after the reap: Packages %d, Sources %d; want each pin released once (-1, 0)", p, s)
	}
}

// Spec C9 (D9) and the phase E carry-forward: ForgetSuite deletes the
// suite whatever its interest stamp and, only when no other suite of the
// host has a row for its base or its /. twin under either scheme, the
// base's signed-seen markers under both schemes and the cached anchor rows
// of the base and the twin under both schemes (ruling PF-2); otherwise it
// names the rows that kept them (ruling PF-7). Another host's suite of the
// same base does not keep them, and another host's anchor row stays.
func TestForgetSuite_DeletesTheBaseMarkersOnlyWithoutAnotherSuite(t *testing.T) {
	for _, tc := range []struct {
		name    string
		other   *SuiteKey // another suite row, besides the forgotten /flat on http://h
		markers int       // markers deleted
	}{
		{name: "no other suite", markers: 2},
		{name: "the /. twin keeps them", other: &SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/flat/."}},
		{name: "the other scheme's suite keeps them", other: &SuiteKey{CanonicalScheme: "https", CanonicalHost: "h", SuitePath: "/flat"}},
		{name: "the other scheme's twin keeps them", other: &SuiteKey{CanonicalScheme: "https", CanonicalHost: "h", SuitePath: "/flat/."}},
		{name: "another host's suite does not", other: &SuiteKey{CanonicalScheme: "http", CanonicalHost: "g", SuitePath: "/flat"}, markers: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := openCache(t)
			ctx := context.Background()
			key := sk("h", "/flat")
			seedRow(t, c, key, evictNow+evictDay) // requested in the future: forgotten anyway
			if tc.other != nil {
				seedRow(t, c, *tc.other, evictNow)
			}
			marks := []SignedSeenMark{{CanonicalScheme: "http", CanonicalHost: "h", Base: "/flat", SeenAt: 1},
				{CanonicalScheme: "https", CanonicalHost: "h", Base: "/flat", SeenAt: 1},
				{CanonicalScheme: "http", CanonicalHost: "g", Base: "/flat", SeenAt: 1}}
			if err := c.MarkSignedSeenBatch(ctx, marks); err != nil {
				t.Fatal(err)
			}
			// Signed anchors cached under the other scheme and at the twin
			// path, with no row of their own, and on another host.
			blob := seedBlob(t, c, "signed anchor")
			anchors := [][2]string{{"https", "/flat/InRelease"}, {"http", "/flat/./Release.gpg"}}
			for _, a := range anchors {
				putURLPathRow(t, c, a[0], "h", a[1], blob, sql.NullInt64{Int64: 1, Valid: true}, true)
			}
			putURLPathRow(t, c, "http", "g", "/flat/InRelease", blob, sql.NullInt64{Int64: 1, Valid: true}, true)
			c.noteTouch(key)
			res, err := c.ForgetSuite(ctx, key, nil)
			wantAnchors, wantKept := 0, []SuiteKey(nil)
			if tc.markers > 0 {
				wantAnchors = len(anchors)
			} else {
				wantKept = []SuiteKey{*tc.other}
			}
			if err != nil || !res.Deleted || !res.Gone || res.MarkersDeleted != tc.markers || res.AnchorRowsDeleted != wantAnchors ||
				!slices.Equal(res.KeptBy, wantKept) {
				t.Fatalf("ForgetSuite = %+v, %v; want deleted with %d markers, %d anchor rows, kept by %v", res, err, tc.markers,
					wantAnchors, wantKept)
			}
			for _, a := range anchors {
				if urlRowExists(t, c, a[0], "h", a[1]) == (tc.markers > 0) {
					t.Errorf("anchor row %s://h%s present %v, want %v", a[0], a[1], tc.markers == 0, tc.markers == 0)
				}
			}
			if !urlRowExists(t, c, "http", "g", "/flat/InRelease") {
				t.Error("another host's anchor row was deleted")
			}
			if rowExists(t, c, key) || isTouched(c, key) {
				t.Fatal("the forgotten suite's row or touch entry stayed")
			}
			for _, m := range marks {
				_, ok, err := c.SignedSeen(ctx, m.CanonicalScheme, m.CanonicalHost, m.Base)
				if err != nil {
					t.Fatal(err)
				}
				gone := m.CanonicalHost == "h" && tc.markers > 0
				if ok == gone {
					t.Errorf("marker %s://%s%s present %v, want %v", m.CanonicalScheme, m.CanonicalHost, m.Base, ok, !gone)
				}
			}
			if tc.other != nil && !rowExists(t, c, *tc.other) {
				t.Fatal("the other suite's row was deleted")
			}
		})
	}
	t.Run("an unknown suite", func(t *testing.T) {
		c := openCache(t)
		ctx := context.Background()
		if err := c.MarkSignedSeenBatch(ctx, []SignedSeenMark{{CanonicalScheme: "http", CanonicalHost: "h", Base: "/flat", SeenAt: 1}}); err != nil {
			t.Fatal(err)
		}
		if res, err := c.ForgetSuite(ctx, sk("h", "/flat"), nil); err != nil || res.Deleted || !res.Gone || res.MarkersDeleted != 0 {
			t.Fatalf("ForgetSuite = %+v, %v; want nothing deleted, and the suite gone (it has no row)", res, err)
		}
		if _, ok, err := c.SignedSeen(ctx, "http", "h", "/flat"); err != nil || !ok {
			t.Fatalf("the marker of a suite with no row was deleted (%v, %v)", ok, err)
		}
	})
}

// Spec §7 C9 "twin and nested suites untouched": reaping /r/dists/x and
// forgetting /flat leave the nested suite /r/dists/x/dists/y and the /.
// twin /flat/. as they were — rows, current snapshots, url_path rows and
// the shared base's markers.
func TestReap_LeavesTwinsAndNestedSuitesAlone(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, evictNow)()
	reaped, nested := sk("h", "/r/dists/x"), sk("h", "/r/dists/x/dists/y")
	forgotten, twin := sk("h", "/flat"), sk("h", "/flat/.")
	ids := map[SuiteKey]int64{}
	for _, k := range []SuiteKey{reaped, nested, forgotten, twin} {
		seedRow(t, c, k, evictNow-40*evictDay)
		ids[k], _ = adoptWith(t, c, k, map[string]string{"InRelease": "anchor of " + k.SuitePath})
		putURLPathRow(t, c, "http", "h", k.SuitePath+"/main/Packages", seedBlob(t, c, "pkgs of "+k.SuitePath), sql.NullInt64{}, true)
	}
	if err := c.MarkSignedSeenBatch(ctx, []SignedSeenMark{{CanonicalScheme: "http", CanonicalHost: "h", Base: "/flat", SeenAt: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteSuiteMetadata(ctx, reaped); err != nil {
		t.Fatal(err)
	}
	cur := ids[reaped]
	if del, _, err := c.ReapIdleSuite(ctx, reaped, &cur, evictNow-30*evictDay); err != nil || !del {
		t.Fatalf("reap %v = %v, %v", reaped, del, err)
	}
	if _, err := c.DeleteSuiteMetadata(ctx, forgotten); err != nil {
		t.Fatal(err)
	}
	cur = ids[forgotten]
	if res, err := c.ForgetSuite(ctx, forgotten, &cur); err != nil || !res.Deleted || res.MarkersDeleted != 0 ||
		!slices.Equal(res.KeptBy, []SuiteKey{twin}) {
		t.Fatalf("forget %v = %+v, %v; want deleted, the twin keeping the base's marker", forgotten, res, err)
	}
	for _, k := range []SuiteKey{nested, twin} {
		sf, err := c.GetSuiteFreshness(ctx, k.CanonicalScheme, k.CanonicalHost, k.SuitePath)
		if err != nil || sf.CurrentSnapshotID == nil || *sf.CurrentSnapshotID != ids[k] {
			t.Fatalf("%v: row %+v, %v; want it and its current snapshot kept", k, sf, err)
		}
		if snap, err := c.GetSuiteSnapshot(ctx, ids[k]); err != nil || snap.RetiredAt != nil {
			t.Fatalf("%v: snapshot %+v, %v; want it not retired", k, snap, err)
		}
		for _, p := range []string{k.SuitePath + "/InRelease", k.SuitePath + "/main/Packages"} {
			if !urlRowExists(t, c, "http", "h", p) {
				t.Fatalf("%s was deleted", p)
			}
		}
	}
	if _, ok, err := c.SignedSeen(ctx, "http", "h", "/flat"); err != nil || !ok {
		t.Fatalf("the twin's base marker = %v, %v; want kept", ok, err)
	}
}

// Spec C9 step 2 (the Step-8 decrement, once): a reap releases the pins of
// the suite's current snapshot only. A displaced snapshot's pins went when
// it was displaced; releasing them again would drive its blobs' refcounts
// below the references they still have (exec review X14).
func TestReapIdleSuite_ReleasesOnlyTheCurrentSnapshotsPins(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, evictNow)()
	key := sk("h", "/r/dists/x")
	seedRow(t, c, key, evictNow-40*evictDay)
	_, old := adoptWith(t, c, key, map[string]string{"InRelease": "v1", "main/Packages": "pkgs v1"})
	cur, hashes := adoptWith(t, c, key, map[string]string{"InRelease": "v2", "main/Packages": "pkgs v2"})
	if n := blobRefcount(t, c, old["main/Packages"]); n != 0 {
		t.Fatalf("premise: the displaced snapshot's member has refcount %d, want 0 (released at displacement)", n)
	}
	if del, _, err := c.ReapIdleSuite(ctx, key, &cur, evictNow-30*evictDay); err != nil || !del {
		t.Fatalf("ReapIdleSuite = %v, %v", del, err)
	}
	if o, n := blobRefcount(t, c, old["main/Packages"]), blobRefcount(t, c, hashes["main/Packages"]); o != 0 || n != 0 {
		t.Fatalf("refcounts after the reap: displaced member %d, current member %d; want 0 and 0", o, n)
	}
}

// Design note 13 (the E carry-forward's base and twin rule): forgetting a
// "/." twin deletes the markers of its base — the key every suite of the
// base shares (aptpath.MarkerBase) — under both schemes, when no other row
// of the base or the twin is left (exec review X15).
func TestForgetSuite_ATwinPathDeletesItsBasesMarkers(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	twin := sk("h", "/flat/.")
	seedRow(t, c, twin, evictNow)
	marks := []SignedSeenMark{{CanonicalScheme: "http", CanonicalHost: "h", Base: "/flat", SeenAt: 1},
		{CanonicalScheme: "https", CanonicalHost: "h", Base: "/flat", SeenAt: 1}}
	if err := c.MarkSignedSeenBatch(ctx, marks); err != nil {
		t.Fatal(err)
	}
	res, err := c.ForgetSuite(ctx, twin, nil)
	if err != nil || !res.Deleted || res.MarkersDeleted != 2 {
		t.Fatalf("ForgetSuite(/flat/.) = %+v, %v; want deleted with the base's 2 markers", res, err)
	}
	for _, m := range marks {
		if _, ok, err := c.SignedSeen(ctx, m.CanonicalScheme, m.CanonicalHost, m.Base); err != nil || ok {
			t.Fatalf("marker %s://h/flat survived (%v, %v)", m.CanonicalScheme, ok, err)
		}
	}
}

// Design review M-9: a reap moves the base's signed-seen markers, under
// both schemes, up to the reap (MAX(last_seen_at, now)), so a reaped
// suite's marker ages from the end of its tracking, not from its last
// sighting. A stamp ahead of now is never lowered (ruling PF-1), another
// host's marker is left alone, a twin's reap stamps its base's markers,
// and a reap that keeps the suite stamps nothing.
func TestReapIdleSuite_MovesTheBaseMarkersUpToTheReap(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, evictNow)()
	key, twin := sk("h", "/r/dists/x"), sk("h", "/flat/.")
	seedRow(t, c, key, evictNow-40*evictDay)
	seedRow(t, c, twin, evictNow-40*evictDay)
	old, ahead := evictNow-100*evictDay, evictNow+evictDay
	marks := []SignedSeenMark{{CanonicalScheme: "http", CanonicalHost: "h", Base: "/r/dists/x", SeenAt: old},
		{CanonicalScheme: "https", CanonicalHost: "h", Base: "/r/dists/x", SeenAt: ahead},
		{CanonicalScheme: "http", CanonicalHost: "g", Base: "/r/dists/x", SeenAt: old},
		{CanonicalScheme: "https", CanonicalHost: "h", Base: "/flat", SeenAt: old}}
	if err := c.MarkSignedSeenBatch(ctx, marks); err != nil {
		t.Fatal(err)
	}
	stamp := func(m SignedSeenMark) int64 {
		t.Helper()
		at, ok, err := c.SignedSeen(ctx, m.CanonicalScheme, m.CanonicalHost, m.Base)
		if err != nil || !ok {
			t.Fatalf("marker %+v: %v, %v", m, ok, err)
		}
		return at
	}
	if del, _, err := c.ReapIdleSuite(ctx, key, nil, evictNow-41*evictDay); err != nil || del {
		t.Fatalf("a kept reap = %v, %v", del, err)
	}
	if got := stamp(marks[0]); got != old {
		t.Fatalf("a kept reap stamped the marker %d, want it unchanged (%d)", got, old)
	}
	for _, k := range []SuiteKey{key, twin} {
		if del, _, err := c.ReapIdleSuite(ctx, k, nil, evictNow-30*evictDay); err != nil || !del {
			t.Fatalf("reap %v = %v, %v", k, del, err)
		}
	}
	for i, want := range []int64{evictNow, ahead, old, evictNow} {
		if got := stamp(marks[i]); got != want {
			t.Errorf("marker %+v: stamp %d after the reaps, want %d", marks[i], got, want)
		}
	}
}

// Design review M-2: a reap or a forget whose submit fails although its
// transaction committed — the caller's context ends right after the commit
// — is settled by a fresh read of the row, as afterCreate settles a
// creation: the suite is reported gone (its holder releases it with
// release(true)) and its touch entry is dropped. One whose transaction
// rolled back is not gone.
func TestReapAndForget_AFailedSubmitWhoseDeleteCommittedIsGone(t *testing.T) {
	for _, op := range []string{"reap", "forget"} {
		t.Run(op, func(t *testing.T) {
			defer stubNow(t, evictNow)()
			c := openCache(t)
			key := sk("h", "/r/dists/x")
			seedRow(t, c, key, evictNow-40*evictDay)
			c.noteTouch(key)
			ctx, cancel := context.WithCancel(context.Background())
			resume := make(chan struct{})
			var once sync.Once
			c.committedHook = func() { once.Do(func() { cancel(); <-resume }) }
			var gone bool
			var err error
			if op == "reap" {
				_, gone, err = c.ReapIdleSuite(ctx, key, nil, evictNow-30*evictDay)
			} else {
				var res ForgetResult
				res, err = c.ForgetSuite(ctx, key, nil)
				gone = res.Gone
			}
			close(resume)
			if err == nil || rowExists(t, c, key) {
				t.Fatalf("premise: the submit fails (%v) on a context that ended after the commit, which deleted the row", err)
			}
			if !gone || isTouched(c, key) {
				t.Fatalf("gone %v, touch entry %v; want the suite settled gone and its touch entry dropped", gone, isTouched(c, key))
			}
		})
	}
	t.Run("rolled back", func(t *testing.T) {
		defer stubNow(t, evictNow)()
		c := openCache(t)
		key := sk("h", "/r/dists/x")
		seedRow(t, c, key, evictNow-40*evictDay)
		if _, err := c.db.Exec(`CREATE TRIGGER fail_delete BEFORE DELETE ON suite_freshness
BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
			t.Fatal(err)
		}
		if del, gone, err := c.ReapIdleSuite(context.Background(), key, nil, evictNow-30*evictDay); err == nil || del || gone {
			t.Fatalf("ReapIdleSuite = %v, %v, %v; want an error, the suite neither deleted nor gone", del, gone, err)
		}
	})
}
```

In `internal/cache/single_insert_test.go`, `TestSuiteRowWritersHaveKnownCallers`'s `deleteSuiteTx` line becomes:

```go
		{"deleteSuiteTx", []string{"suite_reap.go:ForgetSuite", "suite_reap.go:ReapIdleSuite", "suite_rows.go:createSuiteTx"}},
```

after `deleteSuiteFreshnessRE` add:

```go
// deleteSignedSeenRE matches a statement that deletes from
// suite_signed_seen, with the same quoting and schema forms.
var deleteSignedSeenRE = regexp.MustCompile("(?is)\\bDELETE\\s+FROM\\s+" +
	"(?:[\"`\\[]?\\w+[\"`\\]]?\\s*\\.\\s*)?[\"`\\[]?suite_signed_seen\\b")
```

`TestOnlyTheHandlerCreatesSuiteRows`'s doc and its test-only list gain `ClearSignedSeenForTest` (design review M-5: it deletes every marker):

```go
// Spec C3: only the handler creates suite rows. Outside internal/cache no
// production code but internal/handler calls CreateSuite or StoreFetched,
// and no production code calls a test-only seeding helper, nor
// ClearSignedSeenForTest, which deletes every signed-seen marker (design
// review M-5).
func TestOnlyTheHandlerCreatesSuiteRows(t *testing.T) {
	creators := map[string]bool{"CreateSuite": true, "StoreFetched": true}
	seeders := map[string]bool{"SeedSuiteForTest": true, "EnsureSuiteForTest": true, "InsertTrackedCandidateForTest": true,
		"ClearSignedSeenForTest": true}
```

and the file ends with R-F1's guards across packages (design review M-5):

```go
// Ruling R-F1 across packages (design review M-5): outside internal/cache
// only the suite reaper (internal/freshness/reaper.go) and POST /forget
// (internal/freshness/forget.go) call the cache's suite deleters — each
// holds the suite through the SuiteGuard first — so a new caller that
// skips the guard fails here.
func TestSuiteDeletersHaveKnownCallers(t *testing.T) {
	deleters := map[string]bool{"ReapIdleSuite": true, "ForgetSuite": true, "DeleteSuiteMetadata": true}
	allowed := map[string]bool{"../../internal/freshness/reaper.go": true, "../../internal/freshness/forget.go": true}
	productionGoFiles(t, func(p string, src []byte) {
		if strings.HasPrefix(p, "../../internal/cache/") || allowed[p] {
			return
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, p, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && deleters[sel.Sel.Name] {
					t.Errorf("%s calls %s: only the reaper and POST /forget delete suites", fset.Position(call.Pos()), sel.Sel.Name)
				}
			}
			return true
		})
	})
}

// Design review M-5: the never-downgrade authority (spec C5) is deleted
// only by POST /forget (ForgetSuite) and the test-only
// ClearSignedSeenForTest, so a new path that deletes signed-seen markers
// fails here.
func TestSignedSeenHasKnownDeleteSites(t *testing.T) {
	var sites []string
	productionGoFiles(t, func(p string, src []byte) {
		stringLiterals(t, p, src, func(funcName, lit string) {
			for range deleteSignedSeenRE.FindAllStringIndex(lit, -1) {
				sites = append(sites, p+":"+funcName)
			}
		})
	})
	slices.Sort(sites)
	want := []string{"../../internal/cache/suite_reap.go:ForgetSuite", "../../internal/cache/testing_helpers.go:ClearSignedSeenForTest"}
	if !slices.Equal(sites, want) {
		t.Fatalf("DELETE FROM suite_signed_seen sites = %v, want %v", sites, want)
	}
}
```

In `internal/cache/suite_evict_test.go` (`TestDeleteSuiteTx_IsACompareAndDelete`), the two comments that `ReapRegardless` makes inaccurate become `// A stamp in the future never qualifies for an idle cutoff` + `// (ReapRegardless lets every row through: TestReapIdleSuite_*).` and `// The cutoff is inclusive (spec C3: idle >= 24 h), and a row without an` + `// interest stamp never qualifies for an idle cutoff.`

- [ ] **Step 2: Run to verify failure**

Run: `go test -gcflags=-e ./internal/cache/ -run 'TestReapIdleSuite|TestDeleteSuiteMetadata|TestForgetSuite|TestReap_|TestReapAndForget|TestSuiteRowWritersHaveKnownCallers|TestSuiteDeletersHaveKnownCallers|TestSignedSeenHasKnownDeleteSites'`
Expected: compile failures naming `c.ReapIdleSuite undefined`, `undefined: ReapRegardless`, `c.DeleteSuiteMetadata undefined`, `c.ForgetSuite undefined` and `undefined: ForgetResult`.

- [ ] **Step 3: Implement**

In `internal/cache/suite_reap.go`, the imports gain `math`, `strings` and `github.com/linsomniac/apt-cacher-ultra/internal/aptpath`, and after them:

```go
// ReapRegardless is ReapIdleSuite's cutoff for a reap regardless of
// idleness (spec C9: non-canonical rows, ruling R-F3's junk rows): every
// row qualifies, a NULL or future interest stamp included. ForgetSuite
// deletes with it too.
const ReapRegardless int64 = math.MaxInt64

// reapMetadataBatch is spec C9's batch: metadata url_path rows deleted per
// writer transaction.
//
// AIDEV-NOTE: measured (phase F design review, M-15): one 1,000-row batch
// held the writer about 110 ms over a 215,000-row url_path and blob table,
// so a reap is a few such slices, taken in turn with the request path's
// writes. Re-measure before raising it, or if a slower disk shows request
// latency during reaps.
const reapMetadataBatch = 1000
```

`deleteSuiteTx`'s doc becomes:

```go
// deleteSuiteTx is the single suite-row delete — cap eviction (spec C3),
// the reaper's ReapIdleSuite and POST /forget's ForgetSuite (spec C9) —
// run inside the caller's writer transaction as a compare-and-delete: it
// re-reads the row and deletes it only if the row still qualifies —
// last_requested_at at or before cutoff (a stamp in the future, after a
// backward clock step, never qualifies; nor does a NULL one), unless
// cutoff is ReapRegardless, which every row passes — and
// current_snapshot_id equal to wantCurrent (nil: no current snapshot). It
// releases the current snapshot's pins and retires every snapshot of the
// suite (retireSuiteSnapshotsTx), and deletes the row.
```

and its body, from the stamp check to the `DELETE`, becomes:

```go
	if cutoff != ReapRegardless && (!requested.Valid || requested.Int64 > cutoff) {
		return suiteKept, nil
	}
	if current.Valid != (wantCurrent != nil) || (current.Valid && current.Int64 != *wantCurrent) {
		return suiteKept, nil
	}
	if err := retireSuiteSnapshotsTx(ctx, tx, key, current, now); err != nil {
		return suiteKept, fmt.Errorf("delete suite %s%s: %w", key.CanonicalHost, key.SuitePath, err)
	}
```

(the `DELETE FROM suite_freshness` statement after it is unchanged). Append:

```go
// retireSuiteSnapshotsTx is the reaper's retirement path (spec C9): it
// releases the pins of the suite's current snapshot, when it has one (the
// Step-8 decrement, decrementSnapshotRefcountsTx: once, since the snapshot
// stops being current here), and stamps retired_at on every snapshot of
// the suite not yet retired. Its caller, deleteSuiteTx, deletes the row.
//
// AIDEV-NOTE: retired snapshots are invisible to the natural key, the
// candidate lookup, rollback_refused, retained by-hash, strict mode and
// snapshot-GC sub-job B (spec C9), so the suite re-adopts unchanged or
// older content at once; sub-job C deletes them. Keyed by the full
// (scheme, host, suite_path): a /. twin or the other scheme's suite keeps
// its own.
func retireSuiteSnapshotsTx(ctx context.Context, tx *sql.Tx, key SuiteKey, current sql.NullInt64, now int64) error {
	if current.Valid {
		if err := decrementSnapshotRefcountsTx(ctx, tx, current.Int64, now); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE suite_snapshot SET retired_at = ?
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ? AND retired_at IS NULL`,
		now, key.CanonicalScheme, key.CanonicalHost, key.SuitePath); err != nil {
		return fmt.Errorf("retire snapshots: %w", err)
	}
	return nil
}

// suiteMetadataPaths returns the url_path rows of key's suite — paths of
// its scheme and host under the suite whose suite (aptpath.SuitePath) is
// exactly key's, never a /. twin's or a nested suite's (spec C9) — split
// into plain metadata and the suite's anchors (InRelease, Release,
// Release.gpg), each in path order. A read on the pool, outside the
// writer.
func (c *Cache) suiteMetadataPaths(ctx context.Context, key SuiteKey) (plain, anchors []string, err error) {
	prefix := key.SuitePath + "/"
	// The primary key's range [prefix, prefix with its '/' raised to '0').
	rows, err := c.db.QueryContext(ctx, `
SELECT path FROM url_path
 WHERE canonical_scheme = ? AND canonical_host = ? AND path >= ? AND path < ?
 ORDER BY path`, key.CanonicalScheme, key.CanonicalHost, prefix, key.SuitePath+"0")
	if err != nil {
		return nil, nil, fmt.Errorf("suite metadata paths: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, nil, fmt.Errorf("suite metadata paths: scan: %w", err)
		}
		if aptpath.SuitePath(p) != key.SuitePath {
			continue
		}
		switch strings.TrimPrefix(p, prefix) {
		case "InRelease", "Release", "Release.gpg":
			anchors = append(anchors, p)
		default:
			plain = append(plain, p)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("suite metadata paths: %w", err)
	}
	return plain, anchors, nil
}

// deleteCachedRowTx deletes the url_path row of scheme, host and path, if
// there is one, releasing its blob's pin as url_path GC does
// (deleteURLPathRowTx), and reports whether it deleted one: a path without
// a row is skipped, so a row is deleted — and its blob decremented — once.
func deleteCachedRowTx(ctx context.Context, tx *sql.Tx, scheme, host, path string, now int64) (bool, error) {
	var blob sql.NullString
	err := tx.QueryRowContext(ctx, `
SELECT blob_hash FROM url_path WHERE canonical_scheme = ? AND canonical_host = ? AND path = ?`,
		scheme, host, path).Scan(&blob)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read %s://%s%s: %w", scheme, host, path, err)
	}
	if err := deleteURLPathRowTx(ctx, tx, scheme, host, path, blob, now); err != nil {
		return false, err
	}
	return true, nil
}

// deleteURLPathRows deletes, in one writer transaction, the url_path rows
// of scheme and host at paths (deleteCachedRowTx) and reports the rows
// deleted.
func (c *Cache) deleteURLPathRows(ctx context.Context, scheme, host string, paths []string) (int, error) {
	n := 0
	err := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("delete url_path rows: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		now, deleted := nowUnix(), 0
		for _, p := range paths {
			ok, err := deleteCachedRowTx(ctx, tx, scheme, host, p, now)
			if err != nil {
				return fmt.Errorf("delete url_path rows: %w", err)
			}
			if ok {
				deleted++
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("delete url_path rows: commit: %w", err)
		}
		n = deleted
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// DeleteSuiteMetadata is spec C9's step 1: it deletes key's suite metadata
// url_path rows (suiteMetadataPaths) in writer transactions of at most
// reapMetadataBatch rows, plain metadata first and the anchors last, never
// touching the suite row, and reports how many rows it deleted. After an
// error the batches before it stay deleted; a later call deletes the rest.
//
// AIDEV-NOTE: the anchors go last so that a run cut short (an error, a
// shutdown) leaves the suite its anchors, the rows that onboard it and
// serve it unadopted. Each row's pin is released with its row, once; the
// current snapshot's pins are ReapIdleSuite's (deleteSuiteTx), so a reap
// whose final step finds the suite requested again repeats no decrement
// (TestDeleteSuiteMetadata_NeverRepeatsADecrement).
func (c *Cache) DeleteSuiteMetadata(ctx context.Context, key SuiteKey) (int, error) {
	plain, anchors, err := c.suiteMetadataPaths(ctx, key)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, paths := range [][]string{plain, anchors} {
		for len(paths) > 0 {
			batch := paths[:min(len(paths), reapMetadataBatch)]
			paths = paths[len(batch):]
			n, err := c.deleteURLPathRows(ctx, key.CanonicalScheme, key.CanonicalHost, batch)
			total += n
			if err != nil {
				return total, err
			}
		}
	}
	return total, nil
}

// ReapIdleSuite is the reaper's final step (spec C9 step 2): one writer
// transaction running deleteSuiteTx's compare-and-delete with the current
// snapshot the reaper listed (wantCurrent) and its cutoff — an idle
// cutoff, or ReapRegardless for a non-canonical or junk row — which, when
// it deletes the row, also moves the base's signed-seen markers up to now.
// It reports deleted (this call deleted the row) and gone (the suite has
// no row: deleted, none was found, or — after a failed submit — none is
// found by a fresh read). The caller releases its SuiteGuard hold with
// release(gone), which drops the lock-map and flip-gate entries; a gone
// suite's touch-coalescing entry is dropped here.
//
// AIDEV-NOTE: design review M-2: submitWrite can fail after the
// transaction committed (its context ended between the commit and the
// hand-back), so a failed submit is settled by a fresh read of the row, as
// afterCreate settles a creation; under the caller's hold no other deleter
// runs, so a row found gone then was deleted by this call. Design review
// M-9: the base's markers (aptpath.MarkerBase, under both schemes, as
// never-downgrade reads them) are stamped MAX(last_seen_at, now), so the
// orphan sweep's year counts from the end of the suite's tracking — after
// a long downtime a reaped suite's marker would otherwise be swept a day
// after it became an orphan. MAX never lowers a stamp (ruling PF-1), and
// an UPDATE never creates a marker.
func (c *Cache) ReapIdleSuite(ctx context.Context, key SuiteKey, wantCurrent *int64, cutoff int64) (deleted, gone bool, err error) {
	var out deleteOutcome
	err = c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("ReapIdleSuite: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		now := nowUnix()
		o, err := deleteSuiteTx(ctx, tx, key, wantCurrent, cutoff, now)
		if err != nil {
			return fmt.Errorf("ReapIdleSuite: %w", err)
		}
		if o == suiteDeleted {
			if _, err := tx.ExecContext(ctx, `
UPDATE suite_signed_seen SET last_seen_at = MAX(last_seen_at, ?)
 WHERE canonical_scheme IN ('http', 'https') AND canonical_host = ? AND base = ?`,
				now, key.CanonicalHost, aptpath.MarkerBase(key.SuitePath)); err != nil {
				return fmt.Errorf("ReapIdleSuite: stamp markers: %w", err)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("ReapIdleSuite: commit: %w", err)
		}
		if c.committedHook != nil {
			c.committedHook()
		}
		out = o
		return nil
	})
	if err != nil {
		gone = c.suiteRowGone(key) // never read out after an error: the operation may still run
	} else {
		deleted, gone = out == suiteDeleted, out != suiteKept
	}
	if gone {
		c.forgetTouch(key)
	}
	return deleted, gone, err
}

// ForgetResult reports ForgetSuite's effects.
type ForgetResult struct {
	// Deleted: this call deleted the suite row.
	Deleted bool
	// Gone: the suite has no row — Deleted, none was found, or, after a
	// failed submit, none is found by a fresh read. The caller releases its
	// SuiteGuard hold with release(Gone).
	Gone bool
	// MarkersDeleted counts the base's signed-seen markers deleted with it.
	MarkersDeleted int
	// AnchorRowsDeleted counts the cached anchor url_path rows of the base
	// and its /. twin, under both schemes, deleted with the markers.
	AnchorRowsDeleted int
	// KeptBy lists the suite rows that kept the base's markers and anchor
	// rows — the host's rows at the base or its /. twin, under either
	// scheme — in key order; empty when nothing kept them.
	KeptBy []SuiteKey
}

// ForgetSuite is POST /forget's final step (spec C9, D9): one writer
// transaction running deleteSuiteTx with ReapRegardless (no idle check)
// and wantCurrent, then — only once no suite row of the host is left for
// the suite's base (aptpath.MarkerBase) or its /. twin, under either
// scheme — deleting the base's signed-seen markers under both schemes and
// the cached InRelease, Release and Release.gpg url_path rows of the base
// and its twin under both schemes; otherwise it reports the rows that kept
// them. A failed submit is settled as ReapIdleSuite settles one.
//
// AIDEV-NOTE: never-downgrade reads a base's markers under either scheme
// (spec C5 safeguard 3, design review M-7), so /forget, the escape hatch
// for unsigned_downgrade_refused, deletes both or neither: a marker left
// under the other scheme would keep refusing, and one deleted while a
// twin or the other scheme's suite still has a row would strip that
// suite's protection (spec C3: "only if no other suite with the same base
// has a row"). The anchor rows go with the markers (ruling PF-2): no suite
// owns them then, and they are what the unsigned commit's re-mark (ruling
// R-E1) reads — left in place, a signed one cached under the other scheme
// or at the twin path marks the base again, and the escape hatch stays
// shut with no row left to forget. At most 12 rows, each releasing its
// blob's pin.
func (c *Cache) ForgetSuite(ctx context.Context, key SuiteKey, wantCurrent *int64) (ForgetResult, error) {
	var got ForgetResult
	err := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("ForgetSuite: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		now := nowUnix()
		out, err := deleteSuiteTx(ctx, tx, key, wantCurrent, ReapRegardless, now)
		if err != nil {
			return fmt.Errorf("ForgetSuite: %w", err)
		}
		r := ForgetResult{Deleted: out == suiteDeleted, Gone: out != suiteKept}
		if r.Deleted {
			base := aptpath.MarkerBase(key.SuitePath)
			if r.KeptBy, err = baseSuiteRowsTx(ctx, tx, key.CanonicalHost, base); err != nil {
				return fmt.Errorf("ForgetSuite: %w", err)
			}
			if len(r.KeptBy) == 0 {
				if r.AnchorRowsDeleted, err = deleteBaseAnchorsTx(ctx, tx, key.CanonicalHost, base, now); err != nil {
					return fmt.Errorf("ForgetSuite: delete anchors: %w", err)
				}
				dr, err := tx.ExecContext(ctx, `
DELETE FROM suite_signed_seen
 WHERE canonical_scheme IN ('http', 'https') AND canonical_host = ? AND base = ?`, key.CanonicalHost, base)
				if err != nil {
					return fmt.Errorf("ForgetSuite: delete markers: %w", err)
				}
				n, err := dr.RowsAffected()
				if err != nil {
					return fmt.Errorf("ForgetSuite: delete markers: %w", err)
				}
				r.MarkersDeleted = int(n)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("ForgetSuite: commit: %w", err)
		}
		if c.committedHook != nil {
			c.committedHook()
		}
		got = r
		return nil
	})
	var res ForgetResult
	if err != nil {
		res.Gone = c.suiteRowGone(key) // never read got after an error: the operation may still run
	} else {
		res = got
	}
	if res.Gone {
		c.forgetTouch(key)
	}
	return res, err
}

// baseSuiteRowsTx returns the host's suite rows at base or at its /. twin,
// under either scheme, in key order: the rows whose suites share base's
// signed-seen markers.
func baseSuiteRowsTx(ctx context.Context, tx *sql.Tx, host, base string) ([]SuiteKey, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT canonical_scheme, suite_path FROM suite_freshness
 WHERE canonical_scheme IN ('http', 'https') AND canonical_host = ? AND suite_path IN (?, ?)
 ORDER BY canonical_scheme, suite_path`, host, base, base+"/.")
	if err != nil {
		return nil, fmt.Errorf("read the base's suites: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SuiteKey
	for rows.Next() {
		k := SuiteKey{CanonicalHost: host}
		if err := rows.Scan(&k.CanonicalScheme, &k.SuitePath); err != nil {
			return nil, fmt.Errorf("read the base's suites: %w", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the base's suites: %w", err)
	}
	return out, nil
}

// deleteBaseAnchorsTx deletes the cached InRelease, Release and
// Release.gpg url_path rows of base and of its /. twin under both schemes
// (deleteCachedRowTx) and reports how many it deleted.
func deleteBaseAnchorsTx(ctx context.Context, tx *sql.Tx, host, base string, now int64) (int, error) {
	n := 0
	for _, scheme := range []string{"http", "https"} {
		for _, suite := range []string{base, base + "/."} {
			for _, name := range []string{"InRelease", "Release", "Release.gpg"} {
				ok, err := deleteCachedRowTx(ctx, tx, scheme, host, suite+"/"+name, now)
				if err != nil {
					return n, err
				}
				if ok {
					n++
				}
			}
		}
	}
	return n, nil
}
```

In `internal/cache/cache.go`, `committedHook`'s comment names the two new users of the seam (design review M-2):

```go
	// committedHook, when set (tests only), runs on the writer goroutine
	// right after the transaction of a creation attempt, of ReapIdleSuite
	// or of ForgetSuite commits, before its result is handed back: it lets
	// a test end the caller's context there, so a submit fails although its
	// eviction or delete committed
	// (TestCreateSuite_AFailedSubmitWhoseEvictionCommittedPurges, deferred
	// minor D-20; TestReapAndForget_AFailedSubmitWhoseDeleteCommittedIsGone,
	// design review M-2).
	committedHook func()
```

In `internal/cache/signed_seen.go`, `markSignedSeen`'s AIDEV-NOTE ends:

```go
// adopter's re-mark before an unsigned commit (ruling R-E1). It is written
// only for suites with a suite_freshness row, and never for an unsigned
// snapshot or a plain-text anchor. Phase F's reap (ReapIdleSuite) moves an
// existing marker's stamp up to the reap, in the transaction that deletes
// the row, and never creates one (design review M-9); POST /forget
// (ForgetSuite) and the orphan sweep delete markers.
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/cache/`
Expected: PASS (`TestDeleteSuiteMetadata_BatchesOfAThousandAnchorsLast` takes about 3 s under `-race`: 1,203 rows deleted one by one).

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| `deleteSuiteTx` without `cutoff != ReapRegardless &&` | `TestReapIdleSuite_RetiresEverySnapshotAndDeletesTheRow` (`NULL stamp, ReapRegardless: deleted false`; a future stamp passes `math.MaxInt64` anyway) |
| `retireSuiteSnapshotsTx` without its decrement | `TestReapIdleSuite_…` (`the current member's refcount = 1`), `TestDeleteSuiteMetadata_NeverRepeatsADecrement` (`after the reap: Packages 0, Sources 1`), `TestReapIdleSuite_ReleasesOnlyTheCurrentSnapshotsPins`, `TestDeleteSuiteTx_IsACompareAndDelete` |
| `retireSuiteSnapshotsTx` decrements every adopted, non-retired snapshot of the suite (exec review X14) | `TestReapIdleSuite_ReleasesOnlyTheCurrentSnapshotsPins` (`displaced member -1, current member 0; want 0 and 0`) |
| `suiteMetadataPaths` without its `aptpath.SuitePath(p) != key.SuitePath` filter | `TestDeleteSuiteMetadata_DeletesOnlyTheSuitesRows` (`DeleteSuiteMetadata = 7`), `TestReap_LeavesTwinsAndNestedSuitesAlone` (`/r/dists/x/dists/y/InRelease was deleted`) |
| `DeleteSuiteMetadata` deletes `anchors` before `plain` | `TestDeleteSuiteMetadata_BatchesOfAThousandAnchorsLast` (`1003 deleted`) |
| `reapMetadataBatch = 2000` | `TestDeleteSuiteMetadata_BatchesOfAThousandAnchorsLast` (`0 deleted`) |
| `ReapIdleSuite` without its `forgetTouch` | `TestReapIdleSuite_…` (`touch entry true`), `TestReapAndForget_AFailedSubmitWhoseDeleteCommittedIsGone/reap` |
| `ReapIdleSuite` without the stamp move (`if o == suiteDeleted` → `if false`) | `TestReapIdleSuite_MovesTheBaseMarkersUpToTheReap` (`marker {… Base:/r/dists/x …}: stamp 1791360000 after the reaps, want 1800000000`) |
| the stamp move `SET last_seen_at = MAX(last_seen_at, ?)` → `SET last_seen_at = ?` | `TestReapIdleSuite_MovesTheBaseMarkersUpToTheReap` (`… stamp 1800000000 after the reaps, want 1800086400`: the stamp ahead of now lowered) |
| the stamp move under `http` only | `TestReapIdleSuite_MovesTheBaseMarkersUpToTheReap` (`marker {CanonicalScheme:https … Base:/flat …}: stamp 1791360000 after the reaps, want 1800000000`) |
| the stamp move keyed by the raw suite path, not `aptpath.MarkerBase` | `TestReapIdleSuite_MovesTheBaseMarkersUpToTheReap` (the same `Base:/flat` line) |
| `ReapIdleSuite` without the settle (`gone = false` after an error) | `TestReapAndForget_AFailedSubmitWhoseDeleteCommittedIsGone/reap` (`gone false, touch entry true`) |
| `ForgetSuite` without the settle (`res.Gone = false` after an error) | `TestReapAndForget_AFailedSubmitWhoseDeleteCommittedIsGone/forget` |
| `ForgetSuite`'s suite-row check `canonical_scheme IN ('http', 'https')` → `= 'http'` | `TestForgetSuite_…/the_other_scheme's_suite_keeps_them`, `…/the_other_scheme's_twin_keeps_them` (`MarkersDeleted:2 … want … 0`) |
| `ForgetSuite`'s suite-row check without the twin (`host, base, base+"/.")` → `host, base, base)`) | `TestForgetSuite_…/the_/._twin_keeps_them`, `…/the_other_scheme's_twin_keeps_them`, `TestReap_LeavesTwinsAndNestedSuitesAlone` |
| `ForgetSuite`'s marker delete `canonical_scheme IN ('http', 'https')` → `= 'http'` | `TestForgetSuite_…/no_other_suite` (`MarkersDeleted:1 … want … 2`), `TestForgetSuite_ATwinPathDeletesItsBasesMarkers` |
| `ForgetSuite`'s base `aptpath.MarkerBase(key.SuitePath)` → `key.SuitePath` (exec review X15) | `TestForgetSuite_ATwinPathDeletesItsBasesMarkers` (`ForgetSuite(/flat/.) = {… MarkersDeleted:0 …}`) |
| `ForgetSuite` deletes no anchor rows (`deleteBaseAnchorsTx` → `0, error(nil)`) | `TestForgetSuite_…/no_other_suite`, `…/another_host's_suite_does_not` (`{… AnchorRowsDeleted:0 …}; want deleted with 2 markers, 2 anchor rows`) |
| `deleteBaseAnchorsTx` under `http` only | `TestForgetSuite_…/no_other_suite` (`AnchorRowsDeleted:1`) |
| `deleteBaseAnchorsTx` without the twin path | `TestForgetSuite_…/no_other_suite` (`AnchorRowsDeleted:1`) |
| `ForgetSuite` reports no `KeptBy` | `TestForgetSuite_…/the_/._twin_keeps_them` and the two other keeping cases (`… kept by [{http h /flat/.}]`), `TestReap_LeavesTwinsAndNestedSuitesAlone` |
| a call of `DeleteSuiteMetadata` in `cmd/apt-cacher-ultra/main.go` (after `cache.Open`) | `TestSuiteDeletersHaveKnownCallers` (`… calls DeleteSuiteMetadata: only the reaper and POST /forget delete suites`) |
| a second `DELETE FROM suite_signed_seen` literal (in `afterCreate`) | `TestSignedSeenHasKnownDeleteSites` |
| a call of `ClearSignedSeenForTest` in `cmd/apt-cacher-ultra/main.go` | `TestOnlyTheHandlerCreatesSuiteRows` (`… calls the test-only ClearSignedSeenForTest`) |

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/cache/suite_reap.go internal/cache/suite_reap_test.go internal/cache/single_insert_test.go internal/cache/suite_evict_test.go internal/cache/cache.go internal/cache/signed_seen.go
git add internal/cache/suite_reap.go internal/cache/suite_reap_test.go internal/cache/single_insert_test.go internal/cache/suite_evict_test.go internal/cache/cache.go internal/cache/signed_seen.go
git commit -m "Reap and forget suites through the one delete path (spec C9; rulings R-F1, R-F5, PF-2, PF-7; design review M-2, M-5, M-9)"
```

(Record the mutation table in the commit message.)

### Task 4: Snapshot GC: retired rows leave `keep_displaced`; sub-job C

Spec C9's snapshot GC: retired snapshots (reaped, forgotten, evicted at a cap, or quarantined by the §6.1 audit) never take one of a suite's `keep_displaced` slots in sub-job B's ranking (phase A carry-forward), and sub-job C deletes them — members, skipped members and `package_hash` rows in chunks of at most 10,000 rows per writer transaction, then the header (design note 15). The blob pass that follows in the same tick reclaims their blobs once `blob_grace` has passed, so a reaped suite's blobs go within two GC ticks. `gc_run_complete` and `LastRunSummary` gain `retired_reaped` (spec C11), with `acu_gc_retired_reaped_total`. Deferred minor A-3 (the GC's run stamps were not told apart from `time.Now` by any test) lands here, in the file it names. The chunk constant is pinned by a cheap assertion (exec review X29, ruling PF-9: a test that fills a 10,000-row chunk takes 4.6 s under `-race`), and its AIDEV-NOTE records the measured writer hold (design review M-15).

**Files:**
- Modify: `internal/cache/gc.go` (sub-job B's ranking, `retiredChunkRows`, `RunRetiredSnapshotGCChunk`), `internal/gc/snapshot.go` (`snapshotPassResult`, sub-job C in `runSnapshotPass`), `internal/gc/gc.go` (`LastRunSummary.RetiredReaped`, `tickResult.retiredReaped`, `gc_run_complete`'s `retired_reaped`), `internal/gc/metrics.go` (`acu_gc_retired_reaped_total`)
- Create: `internal/cache/snapshot_gc_retired_test.go`, `internal/gc/retired_test.go`

**Interfaces:**
- Consumes: `DeleteSuiteMetadata`, `ForgetSuite` (Task 3; the gc test reaps through them); `adoptWith` (Task 3, `suite_reap_test.go`); cache test helpers `adoptCandidate`, `setCurrentSnapshot`, `snapshotExists`, `stubNow` (`gc_test.go`), `seedPackageHash` (`gc_urlpath_test.go`), `seedBlob` (`cache_test.go`), `seedRow`, `sk` (`suite_evict_test.go`); gc test helpers `openTestCache`, `captureLogger`, `dbOf`, `fileExists` (`gc_test.go`), `putPoolBlob`, `adoptSnapshot`, `fastForwardZeroedAt`, `blobRowExists`, `snapshotRowExists` (`integration_test.go`); `wallclock.SetOffsetForTest`.
- Produces: `(*cache.Cache).RunRetiredSnapshotGCChunk(ctx context.Context, maxRows int) (reaped int, more bool, err error)` (maxRows ≤ 0: 10,000); `gc.LastRunSummary.RetiredReaped int`; the `gc_run_complete` field `retired_reaped`; the counter `acu_gc_retired_reaped_total`; the test helpers `retire(t, c, id)` and `childRows(t, c, id) int` (`snapshot_gc_retired_test.go`).

- [ ] **Step 1: Write the failing tests**

Create `internal/cache/snapshot_gc_retired_test.go`:

```go
package cache

import (
	"context"
	"fmt"
	"testing"
)

// retire stamps retired_at on snapshot id (what a reap, a /forget or a
// legacy-audit quarantine does).
func retire(t *testing.T, c *Cache, id int64) {
	t.Helper()
	if _, err := c.db.Exec(`UPDATE suite_snapshot SET retired_at = 1 WHERE snapshot_id = ?`, id); err != nil {
		t.Fatal(err)
	}
}

// childRows counts snapshot id's snapshot_member, snapshot_skipped_member
// and package_hash rows.
func childRows(t *testing.T, c *Cache, id int64) int {
	t.Helper()
	var n int
	if err := c.db.QueryRow(`SELECT
  (SELECT COUNT(*) FROM snapshot_member WHERE snapshot_id = ?) +
  (SELECT COUNT(*) FROM snapshot_skipped_member WHERE snapshot_id = ?) +
  (SELECT COUNT(*) FROM package_hash WHERE snapshot_id = ?)`, id, id, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Spec C9 (phase A carry-forward): retired snapshots take no
// keep_displaced slot. With the current snapshot id5 and two retired
// displaced ones (id3, id4), keep_displaced = 2 keeps the two newest
// non-retired displaced snapshots (id2, id1) — before phase F the retired
// pair took both slots and sub-job B reaped id1 and id2.
func TestRunSnapshotGCBatch_RetiredSnapshotsTakeNoKeepDisplacedSlot(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	var ids []int64
	for i := 1; i <= 5; i++ {
		ids = append(ids, adoptCandidate(t, c, "http", "kd.example", "/p", seedBlob(t, c, fmt.Sprintf("kd r%d", i)), int64(i)*1000))
	}
	setCurrentSnapshot(t, c, "http", "kd.example", "/p", ids[4])
	retire(t, c, ids[2])
	retire(t, c, ids[3])
	defer stubNow(t, 1_000_000)()
	res, err := c.RunSnapshotGCBatch(ctx, 100, 1800, 2)
	if err != nil {
		t.Fatal(err)
	}
	if res.DisplacedReaped != 0 || res.OrphanReaped != 0 {
		t.Fatalf("sub-job B reaped %+v; want nothing (the retired snapshots are not ranked)", res)
	}
	for _, id := range ids {
		if !snapshotExists(t, c, id) {
			t.Fatalf("snapshot %d was reaped by sub-job B", id)
		}
	}
}

// Spec C9 sub-job C: retired snapshots are deleted with their members,
// skipped members and package_hash rows in bounded chunks, then the header
// row; a non-retired snapshot and the current one are untouched, and the
// sub-job says when nothing retired is left.
func TestRunRetiredSnapshotGCChunk_DeletesRetiredSnapshotsInChunks(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/r/dists/x")
	seedRow(t, c, key, 1)
	members := map[string]string{"InRelease": "old anchor"}
	for i := range 12 {
		members[fmt.Sprintf("main/i%02d", i)] = fmt.Sprintf("index %d", i)
	}
	old, hashes := adoptWith(t, c, key, members)
	for i := range 9 {
		seedPackageHash(t, c, "http", "h", fmt.Sprintf("/r/pool/p%d.deb", i), hashes["main/i00"], old, "p", "amd64", "1")
	}
	if _, err := c.db.Exec(`INSERT INTO snapshot_skipped_member (snapshot_id, path, declared_sha256, size, reason, skipped_at)
VALUES (?, 'main/skipped', ?, 1, '4xx', 1)`, old, hashes["main/i01"]); err != nil {
		t.Fatal(err)
	}
	cur, _ := adoptWith(t, c, key, map[string]string{"InRelease": "new anchor"})
	retire(t, c, old)
	if n := childRows(t, c, old); n != 13+9+1 {
		t.Fatalf("premise: %d child rows, want 23", n)
	}
	// Chunks of 10: two full chunks (20 rows), then the last 3 and the header.
	for i, want := range []struct {
		reaped, left int
	}{{0, 13}, {0, 3}, {1, 0}} {
		reaped, more, err := c.RunRetiredSnapshotGCChunk(ctx, 10)
		if err != nil || !more || reaped != want.reaped || childRows(t, c, old) != want.left || snapshotExists(t, c, old) != (want.reaped == 0) {
			t.Fatalf("chunk %d: reaped %d, more %v, err %v, %d rows left, header %v; want reaped %d and %d rows left",
				i+1, reaped, more, err, childRows(t, c, old), snapshotExists(t, c, old), want.reaped, want.left)
		}
	}
	if reaped, more, err := c.RunRetiredSnapshotGCChunk(ctx, 10); err != nil || more || reaped != 0 {
		t.Fatalf("with nothing retired left: reaped %d, more %v, err %v; want 0, false", reaped, more, err)
	}
	if !snapshotExists(t, c, cur) || childRows(t, c, cur) != 1 {
		t.Fatal("the current snapshot or its member was deleted")
	}
}

// Spec C9 / §6.1: sub-job C deletes the snapshots the legacy trust audit
// quarantined (retired), members and package_hash rows included.
func TestRunRetiredSnapshotGCChunk_DeletesAuditRetiredSnapshots(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/r/dists/x")
	seedRow(t, c, key, 1)
	id, hashes := adoptWith(t, c, key, map[string]string{"InRelease": "polluted", "main/Packages": "pkgs"})
	seedPackageHash(t, c, "http", "h", "/r/pool/p.deb", hashes["main/Packages"], id, "p", "amd64", "1")
	if _, err := c.ApplyLegacyAuditDecisions(ctx, []LegacyAuditDecision{{SnapshotID: id, Outcome: LegacyAuditQuarantine}}); err != nil {
		t.Fatal(err)
	}
	for {
		_, more, err := c.RunRetiredSnapshotGCChunk(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			break
		}
	}
	if snapshotExists(t, c, id) || childRows(t, c, id) != 0 {
		t.Fatalf("the quarantined snapshot: header %v, %d child rows; want both gone", snapshotExists(t, c, id), childRows(t, c, id))
	}
}

// Spec C9: sub-job C's chunk is 10,000 rows per writer transaction, a
// constant (exec review X29; a test that fills a chunk takes seconds under
// -race, so the constant itself is pinned).
func TestRunRetiredSnapshotGCChunk_TheChunkIsTenThousandRows(t *testing.T) {
	if retiredChunkRows != 10_000 {
		t.Fatalf("retiredChunkRows = %d, want spec C9's 10,000", retiredChunkRows)
	}
}
```

Create `internal/gc/retired_test.go`:

```go
package gc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// Spec C9 sub-job C and §7 "blobs reclaimed within two GC ticks": a suite
// reaped (or forgotten) leaves retired snapshots; the next tick's snapshot
// pass deletes them (retired_reaped in gc_run_complete and LastRunSummary)
// and the same tick's blob pass reclaims their blobs once blob_grace has
// passed.
func TestRunTick_SubJobCReapsRetiredSnapshotsAndTheirBlobs(t *testing.T) {
	c := openTestCache(t)
	logger, buf := captureLogger()
	db := dbOf(t, c)
	ctx := context.Background()
	anchor := putPoolBlob(t, c, "anchor")
	b1, b2 := putPoolBlob(t, c, "member one"), putPoolBlob(t, c, "member two")
	id := adoptSnapshot(t, c, "http", "h", "/r/dists/x", anchor, []string{b1, b2})
	key := cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}
	if _, err := c.DeleteSuiteMetadata(ctx, key); err != nil { // the reaper's step 1: the seeded anchor row
		t.Fatal(err)
	}
	if res, err := c.ForgetSuite(ctx, key, &id); err != nil || !res.Deleted {
		t.Fatalf("ForgetSuite = %+v, %v", res, err)
	}
	for _, h := range []string{anchor, b1, b2} {
		fastForwardZeroedAt(t, db, h)
	}
	g, err := New(Config{Cache: c, Logger: logger, Enabled: true, Interval: time.Hour, BatchSize: 100,
		SnapshotBatchSize: 10, MaxTickDuration: time.Minute, BlobGrace: time.Minute, KeepDisplaced: 3,
		PoolScanWorkers: 2, HeartbeatStaleGrace: 30 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.StartupPass(ctx); err != nil {
		t.Fatal(err)
	}
	if snapshotRowExists(t, db, id) {
		t.Fatal("the retired snapshot survived the tick")
	}
	for _, h := range []string{anchor, b1, b2} {
		if blobRowExists(t, db, h) || fileExists(t, c.BlobPath(h)) {
			t.Fatalf("blob %s survived the tick that deleted its retired snapshot", h)
		}
	}
	if s, ok := g.LastRunSummary(); !ok || s.RetiredReaped != 1 {
		t.Fatalf("LastRunSummary = %+v, %v; want retired_reaped 1", s, ok)
	}
	var line map[string]any
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, `"msg":"gc_run_complete"`) && json.Unmarshal([]byte(l), &line) == nil {
			break
		}
	}
	if line["retired_reaped"] != float64(1) {
		t.Fatalf("gc_run_complete = %v; want retired_reaped 1", line)
	}
}

// Deferred minor A-3: the GC's run stamps (gc_run_complete's
// acu_gc_last_run_unixtime and LastRunSummary.AtUnixTime, at startup and
// on a periodic tick) come from the shared wall clock, which simulations
// step, not from time.Now.
func TestGC_StampsFollowTheWallClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		restore := wallclock.SetOffsetForTest(48 * time.Hour)
		defer restore()
		c := openTestCache(t)
		logger, _ := captureLogger()
		g, err := New(Config{Cache: c, Logger: logger, Enabled: true, Interval: time.Hour, BatchSize: 100,
			SnapshotBatchSize: 10, MaxTickDuration: time.Minute, BlobGrace: time.Minute, KeepDisplaced: 3,
			PoolScanWorkers: 2, HeartbeatStaleGrace: 30 * time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		if err := g.StartupPass(context.Background()); err != nil {
			t.Fatal(err)
		}
		if s, _ := g.LastRunSummary(); s.AtUnixTime != time.Now().Add(48*time.Hour).Unix() {
			t.Fatalf("startup stamp = %d, want the wall clock's %d", s.AtUnixTime, time.Now().Add(48*time.Hour).Unix())
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { g.Run(ctx); close(done) }()
		time.Sleep(time.Hour + time.Second)
		cancel()
		<-done
		if s, _ := g.LastRunSummary(); s.Phase != "periodic" || s.AtUnixTime != time.Now().Add(48*time.Hour-time.Second).Unix() {
			t.Fatalf("periodic summary = %+v; want the wall clock's stamp at the tick, %d", s, time.Now().Add(48*time.Hour-time.Second).Unix())
		}
	})
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -gcflags=-e ./internal/cache/ ./internal/gc/ -run 'TestRunSnapshotGCBatch_RetiredSnapshotsTakeNoKeepDisplacedSlot|TestRunRetiredSnapshotGCChunk|TestRunTick_SubJobCReapsRetiredSnapshotsAndTheirBlobs|TestGC_StampsFollowTheWallClock'`
Expected: compile failures naming `c.RunRetiredSnapshotGCChunk undefined` and `undefined: retiredChunkRows` (cache), and `s.RetiredReaped undefined` (gc).

- [ ] **Step 3: Implement**

In `internal/cache/gc.go` (`RunSnapshotGCBatch`), the AIDEV-NOTE above `selectSQL` gains

```go
	// Retired snapshots (reaped, forgotten or quarantined) are ranked out
	// the same way, before the window function (spec C9): they never take
	// one of a suite's keep_displaced slots, and sub-job C
	// (RunRetiredSnapshotGCChunk) deletes them.
```

and sub-job B's inner `WHERE adopted_at IS NOT NULL` gains `AND retired_at IS NULL` on the next line (before the `NOT IN` clause, so before the window function). Then, before `HashKnown`, add:

```go
// retiredChunkRows is spec C9's chunk for snapshot-GC sub-job C: the rows
// one writer transaction deletes.
//
// AIDEV-NOTE: measured (phase F design review, M-15): one 10,000-row
// package_hash chunk held the writer about 80 ms over a 600,000-row table
// with its four indexes. Re-measure before raising it.
const retiredChunkRows = 10_000

// RunRetiredSnapshotGCChunk is one writer transaction of snapshot-GC
// sub-job C (spec C9): it takes the oldest retired snapshot, deletes up to
// maxRows of its rows — snapshot_member, then snapshot_skipped_member,
// then package_hash — and, when none is left, its suite_snapshot header.
// It reports how many headers it deleted (0 or 1) and more = false only
// when no retired snapshot was left to work on; the snapshot pass calls it
// until then (or its deadline). maxRows <= 0 means retiredChunkRows.
//
// AIDEV-NOTE: a retired snapshot never serves, vouches or becomes current
// again (CommitAdoption refuses it; every reader filters retired_at), and
// its suite holds no adoption of it — every retirer holds the suite's
// mutex (spec C9) — so it is deleted with no grace. Its pins were released
// when it stopped being current (displacement, the reap's or quarantine's
// Step-8 decrement); deleting its rows and header only drops the
// references the blob pass's reachability predicate reads, so its blobs go
// in the same tick's blob pass once blob_grace has passed — within two GC
// ticks of the retirement. Chunking bounds the writer's hold on a large
// suite (tens of thousands of package_hash rows).
func (c *Cache) RunRetiredSnapshotGCChunk(ctx context.Context, maxRows int) (reaped int, more bool, err error) {
	if maxRows <= 0 {
		maxRows = retiredChunkRows
	}
	// The op writes gotReaped/gotMore, never the named results: on a
	// ctx/close path submitWrite returns before the op ends (see
	// InsertCandidateSnapshot).
	var gotReaped int
	var gotMore bool
	werr := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("RunRetiredSnapshotGCChunk: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		var id int64
		err = tx.QueryRowContext(ctx, `
SELECT snapshot_id FROM suite_snapshot
 WHERE retired_at IS NOT NULL
   AND snapshot_id NOT IN (SELECT current_snapshot_id FROM suite_freshness WHERE current_snapshot_id IS NOT NULL)
 ORDER BY snapshot_id
 LIMIT 1`).Scan(&id)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return tx.Commit() // nothing retired is left: gotMore stays false
		case err != nil:
			return fmt.Errorf("RunRetiredSnapshotGCChunk: select: %w", err)
		}
		left := int64(maxRows)
		for _, table := range []string{"snapshot_member", "snapshot_skipped_member", "package_hash"} {
			res, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE rowid IN (SELECT rowid FROM `+table+
				` WHERE snapshot_id = ? LIMIT ?)`, id, left)
			if err != nil {
				return fmt.Errorf("RunRetiredSnapshotGCChunk: delete %s: %w", table, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("RunRetiredSnapshotGCChunk: delete %s: %w", table, err)
			}
			if left -= n; left == 0 {
				break // the chunk is full: the header waits for the next one
			}
		}
		done := 0
		if left > 0 { // every child table came up short: no row of id is left
			if _, err := tx.ExecContext(ctx, `DELETE FROM suite_snapshot WHERE snapshot_id = ? AND retired_at IS NOT NULL`, id); err != nil {
				return fmt.Errorf("RunRetiredSnapshotGCChunk: delete snapshot %d: %w", id, err)
			}
			done = 1
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("RunRetiredSnapshotGCChunk: commit: %w", err)
		}
		gotReaped, gotMore = done, true
		return nil
	})
	if werr != nil {
		return 0, false, werr
	}
	return gotReaped, gotMore, nil
}
```

In `internal/gc/snapshot.go`, replace `runSnapshotPass` with:

```go
// snapshotPassResult counts one snapshot pass's reaps by class.
type snapshotPassResult struct {
	cache.SnapshotGCBatchResult
	// retired counts the retired snapshots sub-job C deleted (spec C9).
	retired int
}

// runSnapshotPass implements SPEC4 §9.6.3: a per-tick batched reap
// loop over orphan candidate snapshots (sub-job A) ∪ displaced
// snapshots beyond keep-N (sub-job B). Each batch is a single writer-
// tx that runs SELECT-inside-tx + cascade DELETE; the SELECT-DELETE
// liveness race is closed by single-writer ordering (heartbeat /
// CommitAdoption / InsertCandidateSnapshot writes serialize through
// the same writer goroutine — they cannot interleave between SELECT
// and DELETE inside this tx). Once A ∪ B is drained, sub-job C (spec C9)
// deletes the retired snapshots — reaped, forgotten or quarantined — in
// chunks of at most 10,000 rows per writer transaction.
//
// Returns:
//   - the cumulative counts (orphan, displaced, retired),
//   - whether the deadline tripped (true means the pass exited
//     between batches with backlog remaining; the next tick picks
//     up the rest, AND the same tick's blob pass receives an
//     already-expired deadline and exits immediately),
//   - error iff a DB-level failure occurred.
func (g *GC) runSnapshotPass(ctx context.Context, deadline time.Time, phase string) (snapshotPassResult, bool, error) {
	staleGraceSeconds := int64(g.cfg.HeartbeatStaleGrace.Seconds())

	var (
		acc        snapshotPassResult
		batchesRun int
	)
	expired := func() bool {
		if time.Now().Before(deadline) {
			return false
		}
		g.cfg.Logger.Info("gc_tick_deadline_reached",
			"phase", phase,
			"which", "snapshot",
			"batches_completed", batchesRun,
			"bytes_reclaimed_this_tick", int64(0),
		)
		return true
	}
	for {
		if err := ctx.Err(); err != nil {
			return acc, false, nil
		}
		if expired() {
			return acc, true, nil
		}

		batch, err := g.cfg.Cache.RunSnapshotGCBatch(ctx,
			g.cfg.SnapshotBatchSize,
			staleGraceSeconds,
			g.cfg.KeepDisplaced,
		)
		if err != nil {
			return acc, false, fmt.Errorf("snapshot gc batch: %w", err)
		}
		batchesRun++
		acc.OrphanReaped += batch.OrphanReaped
		acc.DisplacedReaped += batch.DisplacedReaped

		if batch.Total() == 0 {
			break // A ∪ B drained
		}
	}
	// Sub-job C (spec C9).
	for {
		if err := ctx.Err(); err != nil {
			return acc, false, nil
		}
		if expired() {
			return acc, true, nil
		}
		reaped, more, err := g.cfg.Cache.RunRetiredSnapshotGCChunk(ctx, 0)
		if err != nil {
			return acc, false, fmt.Errorf("retired snapshot gc chunk: %w", err)
		}
		batchesRun++
		acc.retired += reaped
		if !more {
			return acc, false, nil
		}
	}
}
```

In `internal/gc/gc.go`:
- `LastRunSummary` gains `RetiredReaped           int // snapshot-GC sub-job C (spec C9)` after `DisplacedReaped`; `tickResult` gains `retiredReaped          int` after `displacedReaped`; `runTick`'s snapshot block adds `res.retiredReaped += snap.retired` after its `displacedReaped` line.
- Both `gc_run_complete` lines (`StartupPass` and `Run`) gain `"retired_reaped", tick.retiredReaped,` after `"displaced_reaped", tick.displacedReaped,`; both `emitGCMetrics` calls pass `tick.retiredReaped` after `tick.displacedReaped`; both `recordLastRun` literals set `RetiredReaped: tick.retiredReaped,` after `DisplacedReaped`.

In `internal/gc/metrics.go`, before `gcURLPathRowsReapedTotal`:

```go
	gcRetiredReapedTotal = metrics.NewCounterWithCap(
		"acu_gc_retired_reaped_total",
		"Total retired suite_snapshot rows reaped by snapshot-GC sub-job C: snapshots of reaped, forgotten or quarantined suites (spec C9).",
		0,
	)
```

`emitGCMetrics` gains the parameter `retiredReaped int` after `displacedReaped int`, and after its `displacedReaped` block:

```go
	if retiredReaped > 0 {
		gcRetiredReapedTotal.Add(float64(retiredReaped))
	}
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/cache/ ./internal/gc/ ./internal/admin/`
Expected: PASS (the admin tests read `LastRunSummary`; its JSON is unchanged).

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| sub-job B without `AND retired_at IS NULL` | `TestRunSnapshotGCBatch_RetiredSnapshotsTakeNoKeepDisplacedSlot` (`sub-job B reaped {OrphanReaped:0 DisplacedReaped:2}`) |
| `RunRetiredSnapshotGCChunk`'s `LIMIT ?` bound `left` → `left+1000` | `TestRunRetiredSnapshotGCChunk_DeletesRetiredSnapshotsInChunks` (`chunk 1: … 0 rows left, header true; want … 13 rows left`) |
| the header delete runs whatever `left` is (`if left > 0` → `if true`) | `TestRunRetiredSnapshotGCChunk_DeletesRetiredSnapshotsInChunks` (`FOREIGN KEY constraint failed`) |
| `runSnapshotPass` returns before sub-job C (`if staleGraceSeconds >= 0 { return acc, false, nil }` above its loop) | `TestRunTick_SubJobCReapsRetiredSnapshotsAndTheirBlobs` (`the retired snapshot survived the tick`) |
| `StartupPass`'s `endUnix := wallclock.Unix()` → `time.Now().Unix()` (and, separately, `Run`'s) | `TestGC_StampsFollowTheWallClock` (`startup stamp = 946684800, want the wall clock's 946857600`; the periodic leg for `Run`) |
| `retiredChunkRows = 20_000` (exec review X29) | `TestRunRetiredSnapshotGCChunk_TheChunkIsTenThousandRows` (`retiredChunkRows = 20000, want spec C9's 10,000`) |

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/cache/gc.go internal/cache/snapshot_gc_retired_test.go internal/gc/
git add internal/cache/gc.go internal/cache/snapshot_gc_retired_test.go internal/gc/
git commit -m "Snapshot GC: retired rows leave keep_displaced; sub-job C deletes them in chunks (spec C9); deferred minor A-3"
```

(Record the mutation table in the commit message.)

### Task 5: The adoption stamp's floor and `adoption.go`'s deferred minors

Deferred minor C-11 (F-tagged): `adoptionStampTx` kept `adopted_at` strictly increasing only along the current-snapshot chain. Once the pointer is cleared — a quarantine — or the row deleted — a reap, an eviction or a `/forget`, whose re-created suite starts without one — the next first adoption ignored the suite's remaining snapshots, whose stamps may lead the clock, so GC's `keep_displaced` ranking and retained by-hash's newest-first window could misorder, and a client's `If-Modified-Since` from a retired snapshot's anchor could be answered 304. The floor is now every adopted snapshot of the suite, retired ones included, in both branches (design note 16). The same file's other deferred minors: E-2 (narrowed to this file: `DeclaredHashesForPath`'s doc says "any current snapshot" where only signed ones count; the handler half stays in `E-deferred.md`), E-4 (the in-transaction recheck's errors lack the `CommitAdoption:` prefix), E-5 (the never-downgrade rule moves to `signed_seen.go`, beside the markers it reads). `decrementSnapshotRefcountsTx`'s doc and `ErrSnapshotRetired`'s name the reaper's retirement path as the caller it now has.

**Files:**
- Modify: `internal/cache/adoption.go`, `internal/cache/signed_seen.go`
- Create: `internal/cache/adoption_stamp_test.go`

**Interfaces:**
- Consumes: `ReapIdleSuite`, `adoptWith` (Task 3); `openCache`, `seedBlob` (`cache_test.go`), `stubNow` (`gc_test.go`), `seedRow`, `sk` (`suite_evict_test.go`), `derefI64` (`suite_rows_test.go`).
- Produces: no new API. `rowQuerier`, `CheckUnsignedDowngrade` and `refuseUnsignedDowngrade` now live in `signed_seen.go`; `CommitAdoption`'s Step 1a errors start `CommitAdoption: `.

- [ ] **Step 1: Write the failing tests**

Create `internal/cache/adoption_stamp_test.go`:

```go
package cache

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Deferred minor C-11 (spec C8): the adoption stamp is above every
// snapshot of the suite, not only its current one. A suite reaped while
// its current snapshot's stamp led the clock — it was adopted before a
// backward step — and then re-created stamps its first adoption above the
// retired snapshot's stamp, never below, so a client's If-Modified-Since
// from that snapshot's anchor never gets a 304 for the new one (a
// quarantine clears the pointer with the same effect).
func TestAdoptionStamp_FloorsOnEverySnapshotOfTheSuite(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	const t0 = int64(1_800_000_000)
	restore := stubNow(t, t0)
	key := sk("h", "/r/dists/x")
	seedRow(t, c, key, t0)
	s1, _ := adoptWith(t, c, key, map[string]string{"InRelease": "v1"})
	if _, err := c.db.Exec(`UPDATE suite_snapshot SET adopted_at = ? WHERE snapshot_id = ?`, t0+1000, s1); err != nil {
		t.Fatal(err)
	}
	if del, _, err := c.ReapIdleSuite(ctx, key, &s1, t0); err != nil || !del {
		t.Fatalf("ReapIdleSuite = %v, %v", del, err)
	}
	restore()
	defer stubNow(t, t0+10)()
	seedRow(t, c, key, t0+10) // the suite's next request re-creates it
	s2, _ := adoptWith(t, c, key, map[string]string{"InRelease": "v2"})
	snap, err := c.GetSuiteSnapshot(ctx, s2)
	if err != nil || derefI64(snap.AdoptedAt) != t0+1001 {
		t.Fatalf("the next adoption stamped %d (%v), want %d: above the retired snapshot's stamp", derefI64(snap.AdoptedAt), err, t0+1001)
	}
}

// Deferred minor E-4: an error of CommitAdoption's in-transaction
// never-downgrade recheck says CommitAdoption, so a log line tells it from
// the early exits' reads of the same rule; the refusal still matches its
// sentinel.
func TestCommitAdoption_RecheckErrorsSayCommitAdoption(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/u/dists/s")
	seedRow(t, c, key, 1)
	rel := seedBlob(t, c, "unsigned Release")
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h",
		SuitePath: key.SuitePath, InReleaseHash: &rel, Form: FormUnsigned})
	if err != nil {
		t.Fatal(err)
	}
	commit := func() error {
		return c.CommitAdoption(ctx, id, []SnapshotMember{{SnapshotID: id, Path: "Release", BlobHash: rel, DeclaredSHA256: rel}},
			nil, nil, nil, false)
	}
	if err := c.MarkSignedSeenBatch(ctx, []SignedSeenMark{{CanonicalScheme: "http", CanonicalHost: "h", Base: key.SuitePath, SeenAt: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := commit(); !errors.Is(err, ErrUnsignedDowngradeRefused) || !strings.HasPrefix(err.Error(), "CommitAdoption: ") {
		t.Fatalf("refusal = %v; want ErrUnsignedDowngradeRefused prefixed \"CommitAdoption: \"", err)
	}
	if _, err := c.db.Exec(`DROP TABLE suite_signed_seen`); err != nil { // the recheck's read fails
		t.Fatal(err)
	}
	if err := commit(); err == nil || errors.Is(err, ErrUnsignedDowngradeRefused) ||
		!strings.HasPrefix(err.Error(), "CommitAdoption: unsigned downgrade check: ") {
		t.Fatalf("read failure = %v; want an error prefixed \"CommitAdoption: unsigned downgrade check: \"", err)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/cache/ -run 'TestAdoptionStamp_FloorsOnEverySnapshotOfTheSuite|TestCommitAdoption_RecheckErrorsSayCommitAdoption'`
Expected: FAIL — `the next adoption stamped 1800000010 (<nil>), want 1800001001: above the retired snapshot's stamp` and `refusal = cache: unsigned adoption refused: … want ErrUnsignedDowngradeRefused prefixed "CommitAdoption: "`.

- [ ] **Step 3: Implement**

In `internal/cache/adoption.go`:
- `ErrSnapshotRetired`'s doc becomes:

```go
// ErrSnapshotRetired: the candidate was retired — by the legacy trust
// audit's quarantine, or the reaper's retirement path (a reap, POST
// /forget, cap eviction) — and must never become current.
```

- Step 1a's `return err` becomes:

```go
				// The prefix tells this authoritative read from the early
				// exits' (deferred minor E-4); errors.Is still finds
				// ErrUnsignedDowngradeRefused.
				return fmt.Errorf("CommitAdoption: %w", err)
```

- Move `rowQuerier`, `CheckUnsignedDowngrade` and `refuseUnsignedDowngrade` verbatim to the end of `internal/cache/signed_seen.go` (its imports gain `github.com/linsomniac/apt-cacher-ultra/internal/aptpath`; `adoption.go` keeps its `aptpath` import for Step 3c and Step 7a), the paragraph of `refuseUnsignedDowngrade`'s doc that ends "A read error is returned as it is." ending instead `A read error is returned as it is. (Moved here from adoption.go beside the markers it reads, deferred minor E-5.)`.
- `decrementSnapshotRefcountsTx`'s doc becomes:

```go
// decrementSnapshotRefcountsTx releases one pin per distinct blob the
// snapshot's members reference (SPEC4 §7.5.1 Rule 3). It must run exactly
// once per snapshot, when it stops being current: CommitAdoption Step 8
// (displacement), the legacy trust audit's quarantine (spec §6.1) and the
// reaper's retirement path (retireSuiteSnapshotsTx: cap eviction, the
// reaper and POST /forget, spec C9) are its callers.

```

- Replace `adoptionStampTx` (its doc and body) with:

```go
// adoptionStampTx returns the adopted_at a commit at now stamps (spec C8,
// monotonic Last-Modified): max(now, the newest adopted_at of any snapshot
// of the suite + 1) — current, displaced or retired — and, for a suite
// with no current snapshot (a first adoption, or one after a quarantine
// or a reap), also above the newest
// last_fetched_at of the suite's InRelease/Release/Release.gpg url_path
// rows + 1 — those rows are what clients were served, with that
// Last-Modified or older. The handler serves snapshot anchors with
// adopted_at as their Last-Modified (never the pool file's mtime), so a
// client's If-Modified-Since from the previous anchor always gets a 200
// after a flip, whatever the clock does.
//
// AIDEV-NOTE: call it before CommitAdoption Step 3c, which re-seeds the
// anchor rows' last_fetched_at with now. adopted_at can now lead the wall
// clock (by 1 s per same-second flip, by up to a backward step's length
// after one), so the floor is every snapshot of the suite, not only the
// current one (deferred minor C-11): once current_snapshot_id is cleared
// (a quarantine) or the row deleted (a reap, an eviction, POST /forget —
// the re-created suite starts without one), the suite's remaining
// snapshots may lead the clock, and a stamp below theirs would misorder
// GC's keep_displaced ranking and retained by-hash's newest-first window,
// and could answer a client's If-Modified-Since from a retired snapshot
// 304. Stamps are strictly increasing per suite, so those readers need
// only their order; the legacy audit's markSnapshotBaseTx
// (legacy_audit.go) reads one as a wall-clock stamp, and the clock-step
// rule (internal/freshness/clockstep.go, spec C4) lists adopted_at as a
// past-event stamp — no gate reads it that way, so a stamp in the future
// is harmless there.
func adoptionStampTx(ctx context.Context, tx *sql.Tx, scheme, host, suite string, now int64) (int64, error) {
	var newest, current sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
SELECT (SELECT MAX(adopted_at) FROM suite_snapshot
         WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?),
       (SELECT current_snapshot_id FROM suite_freshness
         WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?)`,
		scheme, host, suite, scheme, host, suite).Scan(&newest, &current); err != nil {
		return 0, fmt.Errorf("read the suite's newest adopted_at: %w", err)
	}
	stamp := now
	if newest.Valid {
		stamp = max(stamp, newest.Int64+1)
	}
	if current.Valid {
		return stamp, nil
	}
	var fetched sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
SELECT MAX(last_fetched_at) FROM url_path
 WHERE canonical_scheme = ? AND canonical_host = ? AND path IN (?, ?, ?)`,
		scheme, host, suite+"/InRelease", suite+"/Release", suite+"/Release.gpg").Scan(&fetched); err != nil {
		return 0, fmt.Errorf("read anchor last_fetched_at: %w", err)
	}
	if fetched.Valid {
		stamp = max(stamp, fetched.Int64+1)
	}
	return stamp, nil
}
```

- `DeclaredHashesForPath`'s doc's first sentence becomes `// DeclaredHashesForPath returns every package_hash row covering this .deb` / `// path under any signed (inline or detached) current snapshot — an` / `// unsigned one never vouches (deferred minor E-2). Joins package_hash → suite_freshness` (the rest unchanged).

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/cache/ ./internal/freshness/ ./internal/handler/`
Expected: PASS (the handler's `Last-Modified` tests and the flip tests keep their stamps: with a current snapshot the newest stamp is the current one's).

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| the floor's `MAX(adopted_at)` limited to `retired_at IS NULL` | `TestAdoptionStamp_FloorsOnEverySnapshotOfTheSuite` (`stamped 1800000010 … want 1800001001`) |
| the floor dropped (`stamp := now` only, before the url_path floor) | `TestAdoptionStamp_FloorsOnEverySnapshotOfTheSuite` |
| Step 1a's `fmt.Errorf("CommitAdoption: %w", err)` → `err` | `TestCommitAdoption_RecheckErrorsSayCommitAdoption` (`refusal = cache: unsigned adoption refused: …`; the refusal leg's `t.Fatalf` ends the test before the read-failure leg runs) |

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/cache/adoption.go internal/cache/signed_seen.go internal/cache/adoption_stamp_test.go
git add internal/cache/adoption.go internal/cache/signed_seen.go internal/cache/adoption_stamp_test.go
git commit -m "Floor the adoption stamp on every snapshot of the suite (deferred minor C-11); E-2, E-4, E-5"
```

(Record the mutation table in the commit message.)

### Task 6: The reaper: idle and non-canonical suites, the hourly run, the memory purge

Spec C9's reaper, in `internal/freshness` (design note 1): its own hourly timer after a one-hour monotonic uptime floor (design note 3), at most 25 candidates per run — those reaped regardless of idleness first (ruling PF-4), then the oldest `last_requested_at`. Candidates are the rows whose suite path the grammar can no longer produce (`!aptpath.IsCanonicalSuitePath`: reaped regardless of idleness, phase A carry-forward) and the suites no client asked for in `gc.suite_idle_ttl` — strictly older than the TTL (design note 4); a canonical path with an odd codename such as `/foo/dists/InRelease` is reaped by idleness only (phase A carry-forward). The reap cutoff is a gate that retires things: a NULL or future stamp is never idle, and the furthest future one is reported through the checker's rate-limited `clock_step_detected` at the first run after a start and whenever the furthest changes (design review M-10), not every hour until the clock passes it. Each candidate is held through `HoldSuite` before any writer operation (ruling R-F1), re-read under the hold (design note 5), its metadata deleted in batches (`DeleteSuiteMetadata`), then deleted by `ReapIdleSuite`'s compare-and-delete with the current snapshot listed — all within `suiteHoldTimeout`, 5 minutes (design review M-2: a wedged writer releases the suite instead of holding it until shutdown). The hold is released with `release(gone)`, `gone` being `ReapIdleSuite`'s answer — settled by a fresh read after a failed submit (Task 3) — or the re-read's finding no row: that purges the lock-map entry and per-suite state and, unless live (Task 1), the flip-gate entry, and `ReapIdleSuite` the touch entry. A busy suite is skipped and retried next run. A forward step of the wall clock that no restart re-arms — the checker's wall clock running more than an hour ahead of the monotonic clock between two runs, as after a resumed VM or laptop — is reported (`clock_step_detected`, `stamp=wall_clock`) and defers reaping by the uptime floor (design review M-8). A step that fails because the daemon is stopping logs at DEBUG, not WARN (design review M-3). Each reap counts in `acu_suites_reaped_total{reason}` (design review M-16). Each run also sweeps the adopter's reconcile memo against the current snapshots (spec C9; design note 18), whose AIDEV-NOTE said its entries were never reaped. A run recovers its own panic and releases a held suite on the way out. With `IdleTTL = 0` no suite row is deleted (design note 2); the memo sweep still runs. The phase B carry-forward's parked scheduler entry of a reaped suite is pinned here: its next offer flushes it. The exec review's surviving mutants X2, X5r, X6, X8, X13 and X25 get their killer tests here (ruling PF-9); X3 (the idle cutoff off by one second) needs none, since a candidate's stamp is already before the cutoff and a touch moves it to now.

**Files:**
- Create: `internal/freshness/reaper.go`, `internal/freshness/reaper_test.go`
- Modify: `internal/freshness/reconcile.go` (`sweepReconcileMemo`), `internal/freshness/adoption.go` (the memo field's AIDEV-NOTE), `internal/freshness/metrics.go` (`acu_suites_reaped_total`)

**Interfaces:**
- Consumes: `cache.ReapRegardless`, `(*cache.Cache).DeleteSuiteMetadata`, `ReapIdleSuite` (`(deleted, gone bool, err error)`), `ForgetSuite` (a test's other deleter) (Task 3), `HasSuiteRow` (Task 2), `ListSuitesWithAdoption`, `GetSuiteFreshness`, `Dir`; `(*Checker).HoldSuite`, `clockStep`, `reportPanic`, `now`, `tick`, `checkRecovered`, the fields `absentAnswered`, `refresh`; `unloggedPanic`, `clockStepTolerance`, `suiteKey`; `aptpath.IsCanonicalSuitePath`; `metrics.NewCounterWithCap`; freshness test helpers `gatedAdopter` (`suite_lock_test.go`), `lockedBuffer` (`panic_recovery_test.go`), `admitAll`, `admitNone`, `renderedMetric` (`scheduler_test.go`), `simtest.StaticConditional`.
- Produces: `freshness.ReaperConfig{Cache *cache.Cache; Checker *Checker; IdleTTL time.Duration; Logger *slog.Logger}`, `freshness.NewReaper(cfg ReaperConfig) (*Reaper, error)`, `(*Reaper).Run(ctx context.Context)`; unexported `(*Reaper).runOnce(ctx)`, `deferredByForwardStep(now, mono time.Time) bool`, `candidates(rows, now) []reapCandidate`, `reapSuite(ctx, reapCandidate) bool`, `warn(ctx, msg, args...)`, `sweepReconcileMemo(rows)`, the fields `interval`, `floor`, `reapHook func(context.Context, cache.SuiteKey)`, `idleTTL`, `lastWall`, `lastMono`, `deferUntil`, `reported`; `reapCandidate{key, current, requested, reason, cutoff}`; `futureStamp{key, stamp}`; `sortCandidates`; `sameSnapshot(a, b *int64) bool`; the constants `reapInterval`, `reapUptimeFloor`, `reapsPerRun`, `forwardStepSlack`, `suiteHoldTimeout`; the counter `suitesReapedTotal` (`acu_suites_reaped_total{reason}`); `(*Adopter).sweepReconcileMemo(current map[int64]bool) int`; test helpers `newReaperRig(t) *reaperRig` (fields `cache`, `adopter`, `gate`, `checker`, `reaper`, `logs`; methods `seed(suite, requested)`, `adopt(suite, anchor) int64`, `exists(suite) bool`, `events(msg) []map[string]any`), `reapKey(suite) cache.SuiteKey`, `reapedTotal(t, reason) float64`, `reapNow`, `reapDay` (`reaper_test.go`).

- [ ] **Step 1: Write the failing tests**

Create `internal/freshness/reaper_test.go`:

```go
package freshness

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// reapNow is the reaper rig's wall clock (the checker's now).
const (
	reapNow = int64(1_800_000_000)
	reapDay = int64(86400)
)

// reaperRig is a real cache, an adopter whose flip gate is enabled (10 s
// quiet), a checker at the fixed wall clock reapNow, and a reaper with the
// default 30-day idle TTL, all logging JSON to logs.
type reaperRig struct {
	t       *testing.T
	cache   *cache.Cache
	adopter *Adopter
	gate    *flipgate.Gate
	checker *Checker
	reaper  *Reaper
	logs    *lockedBuffer
}

func newReaperRig(t *testing.T) *reaperRig {
	t.Helper()
	ad, rc, gate := gatedAdopter(t)
	logs := &lockedBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	chk, err := New(Config{Cache: rc, Fetcher: &simtest.StaticConditional{Status: 304}, HostLimiter: hostsem.New(8),
		Adopter: ad, Logger: logger, now: func() time.Time { return time.Unix(reapNow, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	rp, err := NewReaper(ReaperConfig{Cache: rc, Checker: chk, IdleTTL: 30 * 24 * time.Hour, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	return &reaperRig{t: t, cache: rc, adopter: ad, gate: gate, checker: chk, reaper: rp, logs: logs}
}

func reapKey(suite string) cache.SuiteKey {
	return cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: suite}
}

// seed gives suite a row (through CreateSuite, so it has a touch entry),
// its client-interest stamp, a lock-map entry and a flip-gate entry whose
// serve the caller lets age, and a cached InRelease url_path row.
func (r *reaperRig) seed(suite string, requested int64) {
	r.t.Helper()
	ctx := context.Background()
	if err := r.cache.EnsureSuiteForTest(ctx, "http", "h", suite); err != nil {
		r.t.Fatal(err)
	}
	if err := r.cache.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h",
		SuitePath: suite, LastRequestedAt: &requested}); err != nil {
		r.t.Fatal(err)
	}
	release, ok := r.checker.HoldSuite(reapKey(suite))
	if !ok {
		r.t.Fatalf("%s: busy", suite)
	}
	release(false)
	r.gate.AnchorStart(ctx, flipgate.Key("http", "h", suite)).Done()
	h, err := r.adopter.writeBlobBytes(ctx, []byte("cached anchor of "+suite))
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.cache.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: suite + "/InRelease",
		BlobHash: &h, UpstreamURL: "http://h" + suite + "/InRelease", IsMetadata: true, RequestCount: 1}); err != nil {
		r.t.Fatal(err)
	}
}

// adopt gives suite a current snapshot whose one member is its anchor.
func (r *reaperRig) adopt(suite, anchor string) int64 {
	r.t.Helper()
	ctx := context.Background()
	h, err := r.adopter.writeBlobBytes(ctx, []byte(anchor))
	if err != nil {
		r.t.Fatal(err)
	}
	id, _, err := r.cache.InsertCandidateSnapshot(ctx, cache.SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h",
		SuitePath: suite, InReleaseHash: &h})
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.cache.CommitAdoption(ctx, id, []cache.SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: h,
		DeclaredSHA256: h}}, nil, nil, nil, false); err != nil {
		r.t.Fatal(err)
	}
	return id
}

func (r *reaperRig) exists(suite string) bool {
	r.t.Helper()
	ok, err := r.cache.HasSuiteRow(context.Background(), reapKey(suite))
	if err != nil {
		r.t.Fatal(err)
	}
	return ok
}

// events returns the rig's JSON log records whose msg is msg.
func (r *reaperRig) events(msg string) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(r.logs.String(), "\n") {
		var m map[string]any
		if line != "" && json.Unmarshal([]byte(line), &m) == nil && m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}

// reapedTotal returns acu_suites_reaped_total for reason (design review
// M-16) as metrics.Default renders it.
func reapedTotal(t *testing.T, reason string) float64 {
	t.Helper()
	var buf bytes.Buffer
	metrics.Default.Render(&buf)
	for _, line := range strings.Split(buf.String(), "\n") {
		if v, ok := strings.CutPrefix(line, `acu_suites_reaped_total{reason="`+reason+`"} `); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				t.Fatal(err)
			}
			return f
		}
	}
	return 0
}

// Spec C9: one run reaps the suites no client asked for in
// gc.suite_idle_ttl, and rows whose path the grammar cannot produce
// regardless of idleness; a canonical path with an odd codename is reaped
// by idleness only. A stamp in the future (a backward clock step) reads as
// now — never idle — and is reported. A reaped suite's snapshots are
// retired, its metadata url_path rows deleted, and its lock-map, flip-gate
// and touch entries purged (ruling R-F1); each reap logs suite_reaped and
// counts in acu_suites_reaped_total by reason (design review M-16).
func TestReaper_ReapsIdleAndNonCanonicalSuitesAndPurgesThem(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newReaperRig(t)
		reaped := map[string]int64{
			"/r/dists/idle":        reapNow - 31*reapDay,
			"/legacy//dists/x":     reapNow, // non-canonical: an empty segment
			"/foo/dists/InRelease": reapNow - 31*reapDay,
		}
		kept := map[string]int64{
			"/r/dists/fresh":       reapNow - 29*reapDay,
			"/r/dists/future":      reapNow + 2*reapDay,
			"/bar/dists/InRelease": reapNow - reapDay,
		}
		for s, at := range reaped {
			r.seed(s, at)
		}
		for s, at := range kept {
			r.seed(s, at)
		}
		snap := r.adopt("/r/dists/idle", "idle anchor")
		idle0, nonCanonical0 := reapedTotal(t, "idle"), reapedTotal(t, "non_canonical")
		time.Sleep(r.gate.Quiet()) // the seeded serves are a quiet period old: their gate entries are idle
		r.reaper.runOnce(context.Background())
		for s := range reaped {
			if r.exists(s) {
				t.Errorf("%s was not reaped", s)
			}
			if _, err := r.cache.LookupURL(context.Background(), "http", "h", s+"/InRelease"); err == nil && s != "/legacy//dists/x" {
				t.Errorf("%s/InRelease was not deleted with its suite", s)
			}
		}
		for s := range kept {
			if !r.exists(s) {
				t.Errorf("%s was reaped", s)
			}
		}
		if got, err := r.cache.GetSuiteSnapshot(context.Background(), snap); err != nil || got.RetiredAt == nil {
			t.Fatalf("the reaped suite's snapshot = %+v, %v; want retired", got, err)
		}
		if l, g, tm := r.checker.LockCount(), r.gate.Len(), r.cache.TouchedSuites(); l != len(kept) || g != len(kept) || tm != len(kept) {
			t.Fatalf("lock map %d, flip gate %d, touch map %d entries; want %d each (the kept suites')", l, g, tm, len(kept))
		}
		reasons := map[string]string{}
		for _, ev := range r.events("suite_reaped") {
			reasons[ev["suite_path"].(string)] = ev["reason"].(string)
		}
		want := map[string]string{"/r/dists/idle": "idle", "/legacy//dists/x": "non_canonical", "/foo/dists/InRelease": "idle"}
		if fmt.Sprint(reasons) != fmt.Sprint(want) {
			t.Fatalf("suite_reaped reasons = %v, want %v", reasons, want)
		}
		if ev := r.events("clock_step_detected"); len(ev) != 1 || ev[0]["suite_path"] != "/r/dists/future" || ev[0]["stamp"] != "last_requested_at" {
			t.Fatalf("clock_step_detected = %v, want one for /r/dists/future's last_requested_at", ev)
		}
		if di, dn := reapedTotal(t, "idle")-idle0, reapedTotal(t, "non_canonical")-nonCanonical0; di != 2 || dn != 1 {
			t.Fatalf("acu_suites_reaped_total rose by idle %v, non_canonical %v; want 2 and 1", di, dn)
		}
	})
}

// Spec C9 and the C4 clock-step rule: candidates reads a NULL interest
// stamp as never idle, a future one as now (never idle), and orders the
// candidates oldest first — a non-canonical row by its own stamp.
func TestReaperCandidates_NullAndFutureStampsAreNeverIdle(t *testing.T) {
	r := newReaperRig(t)
	at := func(v int64) *int64 { return &v }
	row := func(suite string, requested *int64) cache.SuiteWithAdoption {
		return cache.SuiteWithAdoption{SuiteFreshness: cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h",
			SuitePath: suite, LastRequestedAt: requested}}
	}
	cands := r.reaper.candidates([]cache.SuiteWithAdoption{
		row("/r/dists/null", nil),
		row("/r/dists/future", at(reapNow+reapDay)),
		row("/r/dists/old", at(reapNow-40*reapDay)),
		row("/legacy//dists/x", at(reapNow-50*reapDay)),
		row("/r/dists/edge", at(reapNow-30*reapDay)), // exactly the TTL: not older than it
	}, time.Unix(reapNow, 0))
	var got []string
	for _, c := range cands {
		got = append(got, c.reason+" "+c.key.SuitePath)
	}
	if want := []string{"non_canonical /legacy//dists/x", "idle /r/dists/old"}; !slices.Equal(got, want) {
		t.Fatalf("candidates = %q, want %q", got, want)
	}
}

// Ruling PF-4 (design review I-4): the candidates reaped regardless of
// idleness — non-canonical rows here, legacy junk too (Task 7) — sort
// before the idle ones whatever their stamps, so an idle backlog of
// reapsPerRun or more never holds them back; idle ones go oldest first.
func TestReaperCandidates_ReapedRegardlessGoFirst(t *testing.T) {
	r := newReaperRig(t)
	at := func(v int64) *int64 { return &v }
	row := func(suite string, requested *int64) cache.SuiteWithAdoption {
		return cache.SuiteWithAdoption{SuiteFreshness: cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h",
			SuitePath: suite, LastRequestedAt: requested}}
	}
	cands := r.reaper.candidates([]cache.SuiteWithAdoption{
		row("/r/dists/old", at(reapNow-40*reapDay)),
		row("/legacy//dists/x", at(reapNow)), // its clients still ask for it
		row("/r/dists/older", at(reapNow-50*reapDay)),
	}, time.Unix(reapNow, 0))
	var got []string
	for _, c := range cands {
		got = append(got, c.reason+" "+c.key.SuitePath)
	}
	if want := []string{"non_canonical /legacy//dists/x", "idle /r/dists/older", "idle /r/dists/old"}; !slices.Equal(got, want) {
		t.Fatalf("candidates = %q, want %q", got, want)
	}
}

// Design note 4: of several future interest stamps a run reports the
// furthest (clock_step_detected is rate-limited, so one line).
func TestReaperCandidates_ReportsTheFurthestFutureStamp(t *testing.T) {
	r := newReaperRig(t)
	at := func(v int64) *int64 { return &v }
	row := func(suite string, requested *int64) cache.SuiteWithAdoption {
		return cache.SuiteWithAdoption{SuiteFreshness: cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h",
			SuitePath: suite, LastRequestedAt: requested}}
	}
	r.reaper.candidates([]cache.SuiteWithAdoption{
		row("/r/dists/near", at(reapNow+reapDay)),
		row("/r/dists/far", at(reapNow+3*reapDay)),
		row("/r/dists/mid", at(reapNow+2*reapDay)),
	}, time.Unix(reapNow, 0))
	if ev := r.events("clock_step_detected"); len(ev) != 1 || ev[0]["suite_path"] != "/r/dists/far" {
		t.Fatalf("clock_step_detected = %v, want one for /r/dists/far", ev)
	}
}

// Design review M-10: a dead suite's future stamp is reported at the
// first run after a start and again only when the furthest future stamp
// changes (or comes back after a run found none), not every hour until
// the clock passes it. Every report counts in
// acu_clock_steps_detected_total, so the count shows the reports.
func TestReaperCandidates_ReportsAFutureStampOnlyWhenItChanges(t *testing.T) {
	r := newReaperRig(t)
	rows := func(stamp int64) []cache.SuiteWithAdoption {
		return []cache.SuiteWithAdoption{{SuiteFreshness: cache.SuiteFreshness{CanonicalScheme: "http",
			CanonicalHost: "h", SuitePath: "/r/dists/dead", LastRequestedAt: &stamp}}}
	}
	reports := func() float64 {
		v, _ := strconv.ParseFloat(renderedMetric("acu_clock_steps_detected_total"), 64)
		return v
	}
	before := reports()
	for range 3 {
		r.reaper.candidates(rows(reapNow+5*365*reapDay), time.Unix(reapNow, 0))
	}
	if got := reports() - before; got != 1 {
		t.Fatalf("three runs over one future stamp: %v reports, want 1", got)
	}
	r.reaper.candidates(rows(reapNow+6*365*reapDay), time.Unix(reapNow, 0))
	if got := reports() - before; got != 2 {
		t.Fatalf("after the furthest stamp changed: %v reports, want 2", got)
	}
	r.reaper.candidates(nil, time.Unix(reapNow, 0))
	r.reaper.candidates(rows(reapNow+6*365*reapDay), time.Unix(reapNow, 0))
	if got := reports() - before; got != 3 {
		t.Fatalf("after a run without it: %v reports, want 3", got)
	}
}

// Spec C9: a run tries at most 25 suites, oldest last_requested_at first;
// the next run takes the rest.
func TestReaper_AtMost25PerRunOldestFirst(t *testing.T) {
	r := newReaperRig(t)
	for i := range 30 {
		r.seed(fmt.Sprintf("/r/dists/s%02d", i), reapNow-31*reapDay-int64(30-i)*3600) // s00 oldest
	}
	r.reaper.runOnce(context.Background())
	for i := range 30 {
		if got, want := r.exists(fmt.Sprintf("/r/dists/s%02d", i)), i >= 25; got != want {
			t.Fatalf("after one run: s%02d exists %v, want %v (the 25 oldest reaped)", i, got, want)
		}
	}
	r.reaper.runOnce(context.Background())
	if n := len(r.events("suite_reaped")); n != 30 {
		t.Fatalf("after two runs: %d reaped, want all 30", n)
	}
}

// Spec C9: a suite whose mutex is held (a check, an adoption) is skipped
// and reaped by the next run once free; a suite asked for, or adopted,
// between the listing and the hold keeps its metadata rows.
func TestReaper_SkipsABusySuiteAndRetriesNextRun(t *testing.T) {
	r := newReaperRig(t)
	r.seed("/r/dists/busy", reapNow-31*reapDay)
	release, ok := r.checker.HoldSuite(reapKey("/r/dists/busy"))
	if !ok {
		t.Fatal("hold")
	}
	r.reaper.runOnce(context.Background())
	if !r.exists("/r/dists/busy") {
		t.Fatal("a busy suite was reaped")
	}
	release(false)
	r.reaper.runOnce(context.Background())
	if r.exists("/r/dists/busy") {
		t.Fatal("the suite was not reaped once free")
	}
	// Asked for after the listing: the re-read under the hold keeps it whole.
	r.seed("/r/dists/back", reapNow-31*reapDay)
	r.reaper.reapHook = func(context.Context, cache.SuiteKey) {
		now := reapNow
		if err := r.cache.SeedSuiteForTest(context.Background(), cache.SuiteFreshness{CanonicalScheme: "http",
			CanonicalHost: "h", SuitePath: "/r/dists/back", LastRequestedAt: &now}); err != nil {
			t.Error(err)
		}
	}
	r.reaper.runOnce(context.Background())
	if !r.exists("/r/dists/back") {
		t.Fatal("a suite asked for during the run was reaped")
	}
	if _, err := r.cache.LookupURL(context.Background(), "http", "h", "/r/dists/back/InRelease"); err != nil {
		t.Fatalf("a suite asked for during the run lost its anchor row: %v", err)
	}
}

// Design note 5 (I-4's re-validation under the hold): a suite adopted
// between the run's listing and its hold is left whole — its row, its new
// current snapshot and its metadata url_path rows. Without the re-read's
// snapshot comparison the reaper deletes the anchors of a suite that the
// final compare-and-delete then keeps.
func TestReaper_KeepsTheMetadataOfASuiteAdoptedSinceTheListing(t *testing.T) {
	ctx := context.Background()
	r := newReaperRig(t)
	r.seed("/r/dists/x", reapNow-31*reapDay)
	var adopted int64
	r.reaper.reapHook = func(context.Context, cache.SuiteKey) {
		r.reaper.reapHook = nil
		adopted = r.adopt("/r/dists/x", "adopted since the listing")
	}
	r.reaper.runOnce(ctx)
	sf, err := r.cache.GetSuiteFreshness(ctx, "http", "h", "/r/dists/x")
	if err != nil || sf.CurrentSnapshotID == nil || *sf.CurrentSnapshotID != adopted {
		t.Fatalf("row %+v, %v; want it kept with the snapshot adopted since the listing (%d)", sf, err, adopted)
	}
	if _, err := r.cache.LookupURL(ctx, "http", "h", "/r/dists/x/InRelease"); err != nil {
		t.Fatalf("the suite adopted since the listing lost its anchor row: %v", err)
	}
}

// Spec C9 step 2: the final step re-checks the interest stamp in its own
// transaction — a client that asks for the suite while its metadata rows
// are being deleted keeps it (the rows it lost come back on its misses). A
// trigger stands in for the client's touch, landing in the metadata batch
// that deletes the suite's InRelease row.
func TestReaper_AClientAskingDuringTheMetadataBatchesKeepsTheSuite(t *testing.T) {
	ctx := context.Background()
	r := newReaperRig(t)
	r.seed("/r/dists/x", reapNow-31*reapDay)
	db, err := sql.Open("sqlite", filepath.Join(r.cache.Dir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(fmt.Sprintf(`CREATE TRIGGER touch_during_reap AFTER DELETE ON url_path
WHEN OLD.path = '/r/dists/x/InRelease'
BEGIN UPDATE suite_freshness SET last_requested_at = %d WHERE suite_path = '/r/dists/x'; END`, reapNow)); err != nil {
		t.Fatal(err)
	}
	r.reaper.runOnce(ctx)
	if !r.exists("/r/dists/x") || len(r.events("suite_reaped")) != 0 {
		t.Fatalf("row %v, %d suite_reaped lines; want the suite asked for during its metadata deletion kept",
			r.exists("/r/dists/x"), len(r.events("suite_reaped")))
	}
}

// Design note 5: a suite whose row vanished between the run's listing and
// its hold (cap eviction, POST /forget) has the lock-map entry the hold
// stored dropped (release(true)), so the map stays bounded by the table.
func TestReaper_DropsTheLockEntryOfASuiteGoneSinceTheListing(t *testing.T) {
	ctx := context.Background()
	r := newReaperRig(t)
	r.seed("/r/dists/x", reapNow-31*reapDay) // leaves a lock-map entry
	r.reaper.reapHook = func(_ context.Context, k cache.SuiteKey) {
		r.reaper.reapHook = nil
		if res, err := r.cache.ForgetSuite(ctx, k, nil); err != nil || !res.Deleted { // gone under another deleter
			t.Errorf("ForgetSuite = %+v, %v", res, err)
		}
	}
	r.reaper.runOnce(ctx)
	if r.exists("/r/dists/x") || r.checker.LockCount() != 0 {
		t.Fatalf("row %v, %d lock entries; want the vanished suite's entry dropped", r.exists("/r/dists/x"), r.checker.LockCount())
	}
	if n := len(r.events("suite_reaped")); n != 0 {
		t.Fatalf("%d suite_reaped lines for a suite the reaper did not delete", n)
	}
}

// Design note 5: a suite the run keeps — asked for since the listing — is
// released as kept (release(false)): its lock-map entry, and with it the
// checker's per-suite state, stays.
func TestReaper_AKeptSuiteKeepsItsLockEntry(t *testing.T) {
	ctx := context.Background()
	r := newReaperRig(t)
	r.seed("/r/dists/back", reapNow-31*reapDay) // leaves a lock-map entry
	key := suiteKey("http", "h", "/r/dists/back")
	r.checker.absentAnswered.Store(key, struct{}{})
	r.reaper.reapHook = func(context.Context, cache.SuiteKey) {
		r.reaper.reapHook = nil
		now := reapNow
		if err := r.cache.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "http",
			CanonicalHost: "h", SuitePath: "/r/dists/back", LastRequestedAt: &now}); err != nil {
			t.Error(err)
		}
	}
	r.reaper.runOnce(ctx)
	if !r.exists("/r/dists/back") || r.checker.LockCount() != 1 {
		t.Fatalf("row %v, %d lock entries; want the kept suite and its entry", r.exists("/r/dists/back"), r.checker.LockCount())
	}
	if _, ok := r.checker.absentAnswered.Load(key); !ok {
		t.Fatal("the kept suite's per-suite state was dropped")
	}
}

// Design review M-2: the work under one suite's hold runs within
// suiteHoldTimeout — 5 min, CommitAdoption's bound — so a wedged writer
// releases the suite instead of holding it until shutdown.
func TestReaper_BoundsTheWorkUnderAHoldAtFiveMinutes(t *testing.T) {
	r := newReaperRig(t)
	r.seed("/r/dists/a", reapNow-31*reapDay)
	var deadline, at time.Time
	var ok bool
	r.reaper.reapHook = func(ctx context.Context, _ cache.SuiteKey) { deadline, ok = ctx.Deadline(); at = time.Now() }
	start := time.Now()
	r.reaper.runOnce(context.Background())
	if !ok || deadline.Before(start.Add(5*time.Minute)) || deadline.After(at.Add(5*time.Minute)) {
		t.Fatalf("the held steps' deadline = %v (set %v), want 5 min after the hold", deadline, ok)
	}
}

// Design review M-3: a run cut short by shutdown — the daemon's context
// ends while a suite is held — logs the failed step at DEBUG, not WARN,
// and keeps the suite.
func TestReaper_AShutdownMidRunIsNoFailure(t *testing.T) {
	r := newReaperRig(t)
	r.seed("/r/dists/a", reapNow-31*reapDay)
	ctx, cancel := context.WithCancel(context.Background())
	r.reaper.reapHook = func(context.Context, cache.SuiteKey) { cancel() }
	r.reaper.runOnce(ctx)
	if !r.exists("/r/dists/a") {
		t.Fatal("the cancelled run reaped the suite")
	}
	if ev := r.events("suite reaper: read suite row failed"); len(ev) != 1 || ev[0]["level"] != "DEBUG" {
		t.Fatalf("read failures = %v, want one at DEBUG", ev)
	}
}

// Spec C9: the first run waits out the monotonic uptime floor (1 h), then
// one runs every hour; gc.suite_idle_ttl = 0s reaps nothing.
func TestReaper_RunsAfterTheUptimeFloorThenHourly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newReaperRig(t)
		r.seed("/r/dists/a", reapNow-31*reapDay)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { r.reaper.Run(ctx); close(done) }()
		defer func() { cancel(); <-done }()
		time.Sleep(time.Hour - time.Second)
		if !r.exists("/r/dists/a") {
			t.Fatal("reaped before the uptime floor")
		}
		time.Sleep(2 * time.Second)
		if r.exists("/r/dists/a") {
			t.Fatal("not reaped by the first run, at the floor")
		}
		r.seed("/r/dists/b", reapNow-31*reapDay)
		time.Sleep(time.Hour - 2*time.Second)
		if !r.exists("/r/dists/b") {
			t.Fatal("reaped before the next hourly run")
		}
		time.Sleep(2 * time.Second)
		if r.exists("/r/dists/b") {
			t.Fatal("not reaped by the next hourly run")
		}
	})
	r := newReaperRig(t)
	r.reaper.idleTTL = 0
	r.seed("/r/dists/a", reapNow-365*reapDay)
	r.seed("/legacy//dists/x", reapNow)
	r.reaper.runOnce(context.Background())
	if !r.exists("/r/dists/a") || !r.exists("/legacy//dists/x") {
		t.Fatal("gc.suite_idle_ttl = 0s reaped a suite")
	}
}

// Design review M-8: a forward step of the wall clock that no restart
// re-arms (a VM resumed after a long pause, a laptop after a long
// suspend) makes suites look idle at once. The reaper sees the wall clock
// run ahead of the monotonic clock between two runs, reports it
// (clock_step_detected, stamp=wall_clock) and reaps nothing until an
// uptime floor later, giving clients the hour a restart gives them.
func TestReaper_AForwardStepDefersReapingByTheFloor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newReaperRig(t)
		var step atomic.Int64
		start := time.Now()
		r.checker.now = func() time.Time { return time.Unix(reapNow+step.Load(), 0).Add(time.Since(start)) }
		r.seed("/r/dists/a", reapNow-reapDay) // asked for yesterday
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { r.reaper.Run(ctx); close(done) }()
		defer func() { cancel(); <-done }()
		time.Sleep(time.Hour + time.Second) // the first run, at the floor: nothing idle
		if !r.exists("/r/dists/a") {
			t.Fatal("reaped before the step")
		}
		step.Store(40 * reapDay) // the wall clock jumps 40 days while the process runs
		time.Sleep(time.Hour)
		if !r.exists("/r/dists/a") {
			t.Fatal("reaped by the run that saw the forward step")
		}
		if ev := r.events("clock_step_detected"); len(ev) != 1 || ev[0]["stamp"] != "wall_clock" {
			t.Fatalf("clock_step_detected = %v, want one with stamp=wall_clock", ev)
		}
		time.Sleep(time.Hour)
		if r.exists("/r/dists/a") {
			t.Fatal("not reaped an uptime floor after the step")
		}
	})
}

// Design review M-8: only the wall clock running ahead of the monotonic
// clock is a step — two runs further apart than forwardStepSlack (a slow
// run, a long interval) are not.
func TestReaper_RunsFarApartAreNoForwardStep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newReaperRig(t)
		start := time.Now()
		r.checker.now = func() time.Time { return time.Unix(reapNow, 0).Add(time.Since(start)) }
		r.reaper.interval = 3 * time.Hour
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { r.reaper.Run(ctx); close(done) }()
		defer func() { cancel(); <-done }()
		time.Sleep(time.Hour + time.Second) // the first run
		r.seed("/r/dists/a", reapNow-31*reapDay)
		time.Sleep(3 * time.Hour) // the next run, three hours later
		if r.exists("/r/dists/a") {
			t.Fatal("a run three hours after the previous one took the interval for a forward step")
		}
		if ev := r.events("clock_step_detected"); len(ev) != 0 {
			t.Fatalf("clock_step_detected = %v, want none", ev)
		}
	})
}

// Spec C9: each run drops the reconcile memo's entries of snapshots that
// are no suite's current one.
func TestReaper_SweepsTheReconcileMemo(t *testing.T) {
	r := newReaperRig(t)
	r.seed("/r/dists/a", reapNow)
	cur := r.adopt("/r/dists/a", "a anchor")
	r.adopter.reconciledSnapshots.Store(cur, struct{}{})
	r.adopter.reconciledSnapshots.Store(cur+100, struct{}{}) // a displaced (or reaped) snapshot
	r.reaper.runOnce(context.Background())
	var left []int64
	r.adopter.reconciledSnapshots.Range(func(k, _ any) bool { left = append(left, k.(int64)); return true })
	if !slices.Equal(left, []int64{cur}) {
		t.Fatalf("memo = %v, want only the current snapshot %d", left, cur)
	}
}

// Design note 2: gc.suite_idle_ttl = 0s keeps the reconcile memo sweep.
func TestReaper_SweepsTheReconcileMemoWithReapingDisabled(t *testing.T) {
	r := newReaperRig(t)
	r.reaper.idleTTL = 0
	r.seed("/r/dists/a", reapNow)
	cur := r.adopt("/r/dists/a", "a anchor")
	r.adopter.reconciledSnapshots.Store(cur, struct{}{})
	r.adopter.reconciledSnapshots.Store(cur+100, struct{}{})
	r.reaper.runOnce(context.Background())
	if _, ok := r.adopter.reconciledSnapshots.Load(cur + 100); ok {
		t.Fatal("gc.suite_idle_ttl = 0s stopped the reconcile memo sweep")
	}
	if _, ok := r.adopter.reconciledSnapshots.Load(cur); !ok {
		t.Fatal("the current snapshot's memo entry was dropped")
	}
}

// The reaper's goroutine recovers panics (I-1 for internal/freshness): a
// panic while a suite is held releases the hold, is logged
// (freshness_check_panicked, where=reaper), and the next run still comes.
func TestReaper_RecoversAPanicAndReleasesTheHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newReaperRig(t)
		r.reaper.floor, r.reaper.interval = time.Minute, time.Minute
		r.seed("/r/dists/a", reapNow-31*reapDay)
		panics := 1
		r.reaper.reapHook = func(context.Context, cache.SuiteKey) {
			if panics > 0 {
				panics--
				panic("reaper bug")
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { r.reaper.Run(ctx); close(done) }()
		defer func() { cancel(); <-done }()
		time.Sleep(time.Minute + time.Second)
		if ev := r.events("freshness_check_panicked"); len(ev) != 1 || ev[0]["where"] != "reaper" {
			t.Fatalf("panic lines = %v, want one with where=reaper", ev)
		}
		if !r.exists("/r/dists/a") {
			t.Fatal("the panicking run reaped the suite")
		}
		time.Sleep(time.Minute)
		if r.exists("/r/dists/a") {
			t.Fatal("the next run did not reap the suite: the panic left it held")
		}
	})
}

// Spec C9 / phase B carry-forward: a suite reaped while it waits in the
// scheduler for an adoption slot is flushed by its next offer — no row,
// so the check is skipped and the entry done — and leaves no lock-map
// entry; acu_freshness_waiting_for_slot counts it only until then.
func TestReaper_AReapedWaiterFlushesOnItsNextOffer(t *testing.T) {
	r := newReaperRig(t)
	r.checker.refresh = 15 * time.Minute
	r.seed("/r/dists/w", reapNow-31*reapDay)
	r.adopt("/r/dists/w", "w anchor") // adopted, never checked: due at once
	ctx := context.Background()
	q := newSchedQueues()
	r.checker.tick(ctx, q)
	it, ok := q.pop(admitNone)
	if !ok {
		t.Fatal("nothing queued")
	}
	q.finish(checkDone{item: it, report: checkReport{result: "changed_queued", signedCurrent: true}}, 0) // waits for a slot
	r.reaper.runOnce(ctx)
	if r.exists("/r/dists/w") || q.waiting() != 1 {
		t.Fatalf("premise: reaped %v, waiting %d; want the reaped suite still parked", !r.exists("/r/dists/w"), q.waiting())
	}
	it, ok = q.pop(admitAll)
	if !ok || !it.waiting {
		t.Fatalf("offer = %+v, %v; want the waiter", it, ok)
	}
	rep := r.checker.checkRecovered(ctx, it)
	q.finish(checkDone{item: it, report: rep}, 0)
	if rep.result != resultSkipped || q.len() != 0 || q.waiting() != 0 || r.checker.LockCount() != 0 {
		t.Fatalf("after the offer: result %q, queued %d, waiting %d, locks %d; want skipped, 0, 0, 0",
			rep.result, q.len(), q.waiting(), r.checker.LockCount())
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -gcflags=-e ./internal/freshness/ -run 'TestReaper'`
Expected: compile failures naming `undefined: Reaper`, `undefined: NewReaper` and `undefined: ReaperConfig` (the compiler stops there; `sweepReconcileMemo` is reached through `runOnce`, and `acu_suites_reaped_total` through the rendered metrics).

- [ ] **Step 3: Implement**

Create `internal/freshness/reaper.go`:

```go
package freshness

// Spec C9 (limitation #3): the suite reaper. Under the freshness checker's
// lifecycle, with its own hourly ticker, it deletes suites no client asked
// for in gc.suite_idle_ttl — and unreachable legacy rows regardless of
// idleness — through the one suite-row delete (cache.ReapIdleSuite,
// deleteSuiteTx), and keeps the in-memory state bounded by the suite table.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// Spec C9 and C11 constants, not knobs.
const (
	// reapInterval is the reaper's own cadence, independent of gc.interval
	// and of the scheduler's tick.
	reapInterval = time.Hour
	// reapUptimeFloor is how long after the start the first run waits
	// (monotonic): clients returning after a downtime touch their suites
	// first, so a long stop never reaps an active suite. A forward step of
	// the wall clock defers reaping by as long (deferredByForwardStep).
	reapUptimeFloor = time.Hour
	// reapsPerRun bounds the suites one run tries to reap (a busy one
	// counts; it is retried next run).
	reapsPerRun = 25
	// forwardStepSlack is how far the wall clock may run ahead of the
	// monotonic clock between two runs before the reaper takes it for a
	// forward step (design review M-8).
	forwardStepSlack = time.Hour
	// suiteHoldTimeout bounds the work done under one suite's hold — the
	// re-read, the metadata batches and the final delete — as commitTimeout
	// bounds CommitAdoption (design review M-2): a wedged writer releases
	// the suite after it instead of holding it until shutdown.
	suiteHoldTimeout = 5 * time.Minute
)

// ReaperConfig configures a Reaper.
type ReaperConfig struct {
	Cache   *cache.Cache
	Checker *Checker
	// IdleTTL is gc.suite_idle_ttl: a suite no client asked for in this
	// long is reaped. Zero disables suite reaping (the run's other sweeps
	// still run).
	IdleTTL time.Duration
	Logger  *slog.Logger
}

// Reaper is spec C9's suite reaper and the work behind POST /forget (D9).
type Reaper struct {
	cache   *cache.Cache
	checker *Checker
	idleTTL time.Duration
	logger  *slog.Logger
	// interval and floor are reapInterval and reapUptimeFloor; tests
	// shorten them.
	interval, floor time.Duration
	// reapHook, when set (tests only), runs in reapSuite while the suite
	// is held, with the context its steps run on: a panic there stands in
	// for a bug between the hold and its release.
	reapHook func(context.Context, cache.SuiteKey)
	// lastWall and lastMono are the checker's wall clock and the monotonic
	// clock at the previous run, or at Run's start; deferUntil is the
	// monotonic time before which a run does not reap, after a forward
	// step of the wall clock (design review M-8). reported is the furthest
	// future interest stamp the previous run found (design review M-10).
	// Only the run's goroutine touches them.
	lastWall, lastMono, deferUntil time.Time
	reported                       futureStamp
}

// futureStamp is a suite's stamp found in the future.
type futureStamp struct {
	key   cache.SuiteKey
	stamp int64
}

// NewReaper returns a reaper over cfg.Cache whose suite holds and
// in-memory purges go through cfg.Checker (its HoldSuite, the
// cache.SuiteGuard cap eviction uses).
func NewReaper(cfg ReaperConfig) (*Reaper, error) {
	if cfg.Cache == nil {
		return nil, errors.New("freshness: reaper: nil Cache")
	}
	if cfg.Checker == nil {
		return nil, errors.New("freshness: reaper: nil Checker")
	}
	if cfg.IdleTTL < 0 {
		return nil, fmt.Errorf("freshness: reaper: idle TTL must not be negative, got %v", cfg.IdleTTL)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Reaper{cache: cfg.Cache, checker: cfg.Checker, idleTTL: cfg.IdleTTL, logger: logger,
		interval: reapInterval, floor: reapUptimeFloor}, nil
}

// Run runs the reaper until ctx ends: the first run once the uptime floor
// has passed, then one every interval after the previous run finished.
// Each run recovers its own panic (logged, freshness_check_panicked with
// where=reaper), so the next run still comes.
func (r *Reaper) Run(ctx context.Context) {
	r.lastWall, r.lastMono = r.checker.now(), time.Now()
	t := time.NewTimer(r.floor)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		r.safeRun(ctx)
		t.Reset(r.interval)
	}
}

// safeRun is one run under a recover.
func (r *Reaper) safeRun(ctx context.Context) {
	defer func() {
		if v := recover(); v != nil {
			unloggedPanic(func() { r.checker.reportPanic("freshness_check_panicked", "reaper", SuiteRef{}, v) })
		}
	}()
	r.runOnce(ctx)
}

// reapCandidate is a suite one run may reap.
type reapCandidate struct {
	key       cache.SuiteKey
	current   *int64 // the current snapshot the run listed (the compare-and-delete's)
	requested *int64 // last_requested_at as listed; oldest first
	reason    string // suite_reaped's reason: idle, non_canonical or junk
	cutoff    int64  // ReapIdleSuite's cutoff
}

// runOnce is one reaper run (spec C9): unless a forward step of the wall
// clock defers it, reap at most reapsPerRun of the candidates, those
// reaped regardless of idleness first, then the oldest; then sweep the
// reconcile memo against the current snapshots.
func (r *Reaper) runOnce(ctx context.Context) {
	rows, err := r.cache.ListSuitesWithAdoption(ctx)
	if err != nil {
		r.warn(ctx, "suite reaper: list suites failed", "err", err)
		return
	}
	now := r.checker.now()
	deferred := r.deferredByForwardStep(now, time.Now())
	if !deferred && r.idleTTL > 0 {
		cands := r.candidates(rows, now)
		for _, c := range cands[:min(len(cands), reapsPerRun)] {
			if ctx.Err() != nil {
				return
			}
			r.reapSuite(ctx, c)
		}
	}
	r.sweepReconcileMemo(rows)
}

// deferredByForwardStep is the reaper's guard against a forward step of
// the wall clock that no restart re-arms (design review M-8): a VM resumed
// after a long pause, a laptop after a long suspend, NTP stepping the
// clock ahead. It compares how far the wall clock (the checker's) and the
// monotonic clock advanced since the previous run, or Run's start; when
// the wall clock ran more than forwardStepSlack ahead it logs
// clock_step_detected (stamp=wall_clock) and defers reaping by the uptime
// floor, giving clients the hour a restart gives them to touch their
// suites. It reports whether this run is deferred.
//
// AIDEV-NOTE: the monotonic clock does not advance while a machine is
// suspended or a VM paused, and the wall clock does; the uptime floor
// covers only a fresh start. The wall clock is compared in whole seconds
// (Unix), never through time.Time.Sub, which uses the monotonic readings
// when both values carry one. A backward step needs no guard here: a
// future interest stamp is never idle (candidates).
func (r *Reaper) deferredByForwardStep(now, mono time.Time) bool {
	if !r.lastMono.IsZero() {
		elapsed := mono.Sub(r.lastMono)
		ahead := time.Duration(now.Unix()-r.lastWall.Unix())*time.Second - elapsed
		if ahead > forwardStepSlack {
			r.logger.Warn("clock_step_detected", "stamp", "wall_clock",
				"stored_unixtime", r.lastWall.Unix()+int64(elapsed/time.Second), "now_unixtime", now.Unix())
			r.deferUntil = mono.Add(r.floor)
		}
	}
	r.lastWall, r.lastMono = now, mono
	return mono.Before(r.deferUntil)
}

// candidates lists the suites a run may reap, in sortCandidates' order:
// rows whose suite path the grammar can no longer produce
// (aptpath.IsCanonicalSuitePath: unreachable legacy rows), regardless of
// idleness, and suites no client asked for in idleTTL.
//
// AIDEV-NOTE: the reap cutoff is a gate that retires things (spec C4
// clock-step rule): a last_requested_at in the future (a backward clock
// step) reads as now — never idle — and the furthest is reported through
// the checker's rate-limited clock_step_detected at the first run after a
// start and whenever the furthest changes (design review M-10: a dead
// suite's future stamp would otherwise be reported every hour until the
// clock passed it); a NULL stamp is never idle either (the v7 backfill and
// every creation set one). A canonical path with an odd codename such as
// /foo/dists/InRelease is reaped by idleness like any suite (phase A
// carry-forward).
func (r *Reaper) candidates(rows []cache.SuiteWithAdoption, now time.Time) []reapCandidate {
	idleCutoff := now.Add(-r.idleTTL).Unix()
	futureAfter := now.Unix() + int64(clockStepTolerance/time.Second)
	var out []reapCandidate
	var furthest futureStamp
	for i := range rows {
		s := &rows[i].SuiteFreshness
		c := reapCandidate{key: cache.SuiteKey{CanonicalScheme: s.CanonicalScheme, CanonicalHost: s.CanonicalHost,
			SuitePath: s.SuitePath}, current: s.CurrentSnapshotID, requested: s.LastRequestedAt}
		switch {
		case !aptpath.IsCanonicalSuitePath(s.SuitePath):
			c.reason, c.cutoff = "non_canonical", cache.ReapRegardless
		case s.LastRequestedAt != nil && *s.LastRequestedAt > futureAfter:
			if *s.LastRequestedAt > furthest.stamp {
				furthest = futureStamp{key: c.key, stamp: *s.LastRequestedAt}
			}
			continue
		case s.LastRequestedAt != nil && *s.LastRequestedAt < idleCutoff:
			// spec C9: last_requested_at < now − gc.suite_idle_ttl;
			// deleteSuiteTx keeps a stamp after its cutoff.
			c.reason, c.cutoff = "idle", idleCutoff-1
		default:
			continue
		}
		out = append(out, c)
	}
	if furthest.stamp != 0 && furthest != r.reported {
		r.checker.clockStep("last_requested_at", furthest.key.CanonicalScheme, furthest.key.CanonicalHost,
			furthest.key.SuitePath, furthest.stamp, now)
	}
	r.reported = furthest
	sortCandidates(out)
	return out
}

// sortCandidates orders the candidates reaped regardless of idleness
// (cutoff ReapRegardless: non-canonical and junk rows) first, then by the
// oldest last_requested_at (no stamp first), then by key, so a run's
// order is fixed.
//
// AIDEV-NOTE: ruling PF-4 (design review I-4). A junk suite's clients
// touch it at every update, so by its stamp it would sort after every idle
// suite and wait for an idle backlog to drain below reapsPerRun — days
// after an upgrade — while it fails those clients' updates. The classes
// reaped regardless are legacy rows, few and finite, so they cannot starve
// idle reaping.
func sortCandidates(cands []reapCandidate) {
	stamp := func(c reapCandidate) int64 {
		if c.requested == nil {
			return -1 << 63
		}
		return *c.requested
	}
	slices.SortStableFunc(cands, func(a, b reapCandidate) int {
		if ra, rb := a.cutoff == cache.ReapRegardless, b.cutoff == cache.ReapRegardless; ra != rb {
			if ra {
				return -1
			}
			return 1
		}
		if sa, sb := stamp(a), stamp(b); sa != sb {
			if sa < sb {
				return -1
			}
			return 1
		}
		ka := a.key.CanonicalScheme + "|" + a.key.CanonicalHost + "|" + a.key.SuitePath
		kb := b.key.CanonicalScheme + "|" + b.key.CanonicalHost + "|" + b.key.SuitePath
		switch {
		case ka < kb:
			return -1
		case ka > kb:
			return 1
		}
		return 0
	})
}

// reapSuite runs spec C9's steps for one candidate under the suite's
// mutex, within suiteHoldTimeout: the metadata url_path rows in batches
// (cache.DeleteSuiteMetadata), then the final compare-and-delete
// (cache.ReapIdleSuite). A busy suite is skipped (retried next run). It
// reports whether the suite was reaped.
//
// AIDEV-NOTE: ruling R-F1. The suite is held through the checker's
// HoldSuite — the SuiteGuard cap eviction uses — before any writer
// operation, and released with release(true) only once its row is gone:
// that drops the lock-map entry and the checker's per-suite state, and the
// flip-gate entry unless a re-creation's serve keeps it live (ruling
// R-F2); ReapIdleSuite drops the touch entry. Whether the row is gone is
// ReapIdleSuite's answer, which settles a failed submit by a fresh read
// (design review M-2). The re-read under the hold is I-4's re-validation:
// a suite a client asked for (or that adopted) since the listing keeps its
// metadata rows, and one that vanished meanwhile (cap eviction) has the
// entry HoldSuite stored for it dropped. A step that fails because the
// run's context ended (shutdown) is logged at DEBUG, not WARN (design
// review M-3); one cut short by suiteHoldTimeout is a failure (WARN).
func (r *Reaper) reapSuite(ctx context.Context, c reapCandidate) bool {
	release, ok := r.checker.HoldSuite(c.key)
	if !ok {
		r.logger.Debug("suite reaper: suite busy, retried next run",
			"canonical_host", c.key.CanonicalHost, "suite_path", c.key.SuitePath)
		return false
	}
	gone := false
	defer func() { release(gone) }()
	hctx, cancel := context.WithTimeout(ctx, suiteHoldTimeout)
	defer cancel()
	if r.reapHook != nil {
		r.reapHook(hctx, c.key)
	}
	cur, err := r.cache.GetSuiteFreshness(hctx, c.key.CanonicalScheme, c.key.CanonicalHost, c.key.SuitePath)
	switch {
	case errors.Is(err, cache.ErrNotFound):
		gone = true
		return false
	case err != nil:
		r.warn(ctx, "suite reaper: read suite row failed", "err", err,
			"canonical_host", c.key.CanonicalHost, "suite_path", c.key.SuitePath)
		return false
	case !sameSnapshot(cur.CurrentSnapshotID, c.current) ||
		(c.cutoff != cache.ReapRegardless && (cur.LastRequestedAt == nil || *cur.LastRequestedAt > c.cutoff)):
		return false // asked for, or adopted, since the listing
	}
	rows, err := r.cache.DeleteSuiteMetadata(hctx, c.key)
	if err != nil {
		r.warn(ctx, "suite reaper: delete metadata failed", "err", err, "metadata_rows", rows,
			"canonical_host", c.key.CanonicalHost, "suite_path", c.key.SuitePath)
		return false
	}
	var deleted bool
	deleted, gone, err = r.cache.ReapIdleSuite(hctx, c.key, c.current, c.cutoff)
	if err != nil {
		r.warn(ctx, "suite reaper: reap failed", "err", err,
			"canonical_host", c.key.CanonicalHost, "suite_path", c.key.SuitePath)
		return false
	}
	if !deleted {
		return false
	}
	suitesReapedTotal.Inc(c.reason)
	r.logger.Info("suite_reaped",
		"canonical_scheme", c.key.CanonicalScheme,
		"canonical_host", c.key.CanonicalHost,
		"suite_path", c.key.SuitePath,
		"reason", c.reason,
		"metadata_rows", rows,
	)
	return true
}

// warn logs a failed step of a run at WARN — or at DEBUG once ctx, the
// run's context, has ended: a shutdown in the middle of a run is no
// failure (design review M-3; the E-13 and FW-3 (d) class).
func (r *Reaper) warn(ctx context.Context, msg string, args ...any) {
	if ctx.Err() != nil {
		r.logger.Debug(msg, args...)
		return
	}
	r.logger.Warn(msg, args...)
}

// sameSnapshot reports whether two optional snapshot ids are equal.
func sameSnapshot(a, b *int64) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

// sweepReconcileMemo drops the adopter's reconcile memo entries
// (Adopter.reconciledSnapshots) of snapshots no suite has as its current
// one in rows (spec C9), so the memo stays bounded by the suite table.
// Without an adopter there is no memo.
func (r *Reaper) sweepReconcileMemo(rows []cache.SuiteWithAdoption) {
	if r.checker.adopter == nil {
		return
	}
	current := make(map[int64]bool, len(rows))
	for _, s := range rows {
		if s.CurrentSnapshotID != nil {
			current[*s.CurrentSnapshotID] = true
		}
	}
	r.checker.adopter.sweepReconcileMemo(current)
}
```

In `internal/freshness/reconcile.go`, after `ReconcileSnapshot`:

```go
// sweepReconcileMemo drops every reconciledSnapshots entry whose snapshot
// is not in current — the suites' current snapshots, as the reaper's run
// listed them (spec C9) — and reports how many it dropped. A displaced
// snapshot never becomes current again, so its entry is dead; one adopted
// after the listing may lose a fresh entry, which costs it one extra parse
// of its Release on its next unchanged check.
func (a *Adopter) sweepReconcileMemo(current map[int64]bool) int {
	n := 0
	a.reconciledSnapshots.Range(func(k, _ any) bool {
		if id, ok := k.(int64); !ok || !current[id] {
			a.reconciledSnapshots.Delete(k)
			n++
		}
		return true
	})
	return n
}
```

In `internal/freshness/adoption.go`, the `reconciledSnapshots` field's AIDEV-NOTE replaces its last three lines ("Entries are never reaped; growth is bounded by distinct AUTOINCREMENT snapshot IDs over the process lifetime — the same long-tail profile the per-suite `locks` map (freshness.go) already accepts.") with:

```go
	// Each reaper run drops the entries of snapshots that are no suite's
	// current one (sweepReconcileMemo, spec C9), so the memo stays bounded
	// by the suite table rather than by every snapshot id the process saw.
```

In `internal/freshness/metrics.go`, the flat-repository metrics AIDEV-NOTE names the new counter; its lines from `B-31` through `acu_flip_slow_anchor_reads_total and` become:

```go
// B-31, counter by host); acu_strict_adoption_waits_total (spec C10,
// counter by host); acu_suites_reaped_total (spec C9, design review M-16,
// counter by reason: idle, non_canonical, junk). The flip barrier's
// acu_flip_hold_skipped_total, acu_flip_hold_expired_total,
// acu_flip_quiet_cut_short_total, acu_flip_waiters_over_cap_total,
// acu_flip_slow_anchor_reads_total and
```

and the `var` block gains, before `freshnessStaleSuites`:

```go
	// suitesReapedTotal counts the suites the suite reaper deleted, by
	// reason (spec C9, design review M-16): a closed three-value enum —
	// idle, non_canonical, junk — so dashboards see reaping without
	// parsing suite_reaped lines.
	suitesReapedTotal = metrics.NewCounterWithCap(
		"acu_suites_reaped_total",
		"Suites the suite reaper deleted, by reason: idle, non_canonical or junk (spec C9).",
		metrics.DefaultMaxSeries,
		"reason",
	)
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/freshness/ -run 'TestReaper|TestHoldSuite|TestScheduler'` then `go test -race ./internal/freshness/`
Expected: PASS (the reaper tests take about 5 s in all under `-race`, each well under a second).

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| `candidates` without its non-canonical case (`… && false`) | `TestReaper_ReapsIdleAndNonCanonicalSuitesAndPurgesThem` (`/legacy//dists/x was not reaped`), `TestReaperCandidates_NullAndFutureStampsAreNeverIdle`, `TestReaperCandidates_ReapedRegardlessGoFirst` (`"idle /legacy//dists/x"`) |
| the idle comparison `<` → `<=` | `TestReaperCandidates_NullAndFutureStampsAreNeverIdle` (`… "idle /r/dists/edge"]`) |
| `reapsPerRun = 30` | `TestReaper_AtMost25PerRunOldestFirst` (`after one run: s25 exists false`) |
| `reapSuite` releases with `release(false)` always (`release(gone && false)`) | `TestReaper_ReapsIdleAndNonCanonicalSuitesAndPurgesThem` (`lock map 6, flip gate 6, touch map 3 entries`), `TestReaper_DropsTheLockEntryOfASuiteGoneSinceTheListing`, `TestReaper_AReapedWaiterFlushesOnItsNextOffer` (`locks 1`) |
| the re-read under the hold checks only the current snapshot (the stamp clause dropped) | `TestReaper_SkipsABusySuiteAndRetriesNextRun` (`a suite asked for during the run lost its anchor row`) |
| `Run`'s first timer `r.floor` → `0` | `TestReaper_RunsAfterTheUptimeFloorThenHourly` (`reaped before the uptime floor`), `TestReaper_RecoversAPanicAndReleasesTheHold` |
| `safeRun` without its recover | `TestReaper_RecoversAPanicAndReleasesTheHold` (`panic: reaper bug`) |
| `sweepReconcileMemo` sweeps nothing | `TestReaper_SweepsTheReconcileMemo` (`memo = […], want only the current snapshot`), `TestReaper_SweepsTheReconcileMemoWithReapingDisabled` |
| `runOnce`'s `r.idleTTL > 0` → `>= 0` | `TestReaper_RunsAfterTheUptimeFloorThenHourly` (`gc.suite_idle_ttl = 0s reaped a suite`) |
| X2: `candidates` reports the nearest future stamp | `TestReaperCandidates_ReportsTheFurthestFutureStamp` (`want one for /r/dists/far`) |
| X5r: the re-read without the snapshot comparison | `TestReaper_KeepsTheMetadataOfASuiteAdoptedSinceTheListing` (`the suite adopted since the listing lost its anchor row`) |
| X6: a row gone since the listing leaves `gone` false | `TestReaper_DropsTheLockEntryOfASuiteGoneSinceTheListing` (`row false, 1 lock entries`) |
| X8: `reapSuite` releases with `release(true)` always (`release(gone \|\| true)`) | `TestReaper_AKeptSuiteKeepsItsLockEntry` (`row true, 0 lock entries`) |
| X13: the memo sweep runs only while `idleTTL > 0` | `TestReaper_SweepsTheReconcileMemoWithReapingDisabled` (`gc.suite_idle_ttl = 0s stopped the reconcile memo sweep`) |
| X25: the final compare-and-delete with `cache.ReapRegardless` | `TestReaper_AClientAskingDuringTheMetadataBatchesKeepsTheSuite` (`row false, 1 suite_reaped lines`) |
| PF-4: `sortCandidates` by stamp only (the regardless clause dropped) | `TestReaperCandidates_ReapedRegardlessGoFirst` (`["idle /r/dists/older" "idle /r/dists/old" "non_canonical /legacy//dists/x"]`) |
| M-10: the furthest future stamp reported every run (`furthest != r.reported` dropped) | `TestReaperCandidates_ReportsAFutureStampOnlyWhenItChanges` (`three runs over one future stamp: 3 reports, want 1`) |
| M-10: `r.reported` kept when a run finds none | `TestReaperCandidates_ReportsAFutureStampOnlyWhenItChanges` (`after a run without it: 2 reports, want 3`) |
| M-8: `deferredByForwardStep` returns `false` | `TestReaper_AForwardStepDefersReapingByTheFloor` (`reaped by the run that saw the forward step`) |
| M-8: the wall clock's advance not set against the monotonic interval (`- 0*elapsed`) | `TestReaper_RunsFarApartAreNoForwardStep` (`a run three hours after the previous one took the interval for a forward step`) |
| M-8: deferred by two floors | `TestReaper_AForwardStepDefersReapingByTheFloor` (`not reaped an uptime floor after the step`) |
| M-3: `warn` logs WARN after the run's context ended | `TestReaper_AShutdownMidRunIsNoFailure` (`level:WARN`) |
| M-2: the held work without a deadline (`context.WithCancel`) | `TestReaper_BoundsTheWorkUnderAHoldAtFiveMinutes` (`(set false)`) |
| M-2: `suiteHoldTimeout = 10 * time.Minute` | `TestReaper_BoundsTheWorkUnderAHoldAtFiveMinutes` (`want 5 min after the hold`) |
| M-2: `ReapIdleSuite`'s `gone` ignored (`deleted, _, err = …`) | `TestReaper_ReapsIdleAndNonCanonicalSuitesAndPurgesThem` (`lock map 6, flip gate 6, touch map 3 entries`), `TestReaper_AReapedWaiterFlushesOnItsNextOffer` (`locks 1`) |
| M-16: `suitesReapedTotal.Inc` dropped | `TestReaper_ReapsIdleAndNonCanonicalSuitesAndPurgesThem` (`acu_suites_reaped_total rose by idle 0, non_canonical 0; want 2 and 1`) |
| M-16: every reap counted as `idle` | `TestReaper_ReapsIdleAndNonCanonicalSuitesAndPurgesThem` (`rose by idle 3, non_canonical 0`) |

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/freshness/reaper.go internal/freshness/reaper_test.go internal/freshness/reconcile.go internal/freshness/adoption.go internal/freshness/metrics.go
git add internal/freshness/reaper.go internal/freshness/reaper_test.go internal/freshness/reconcile.go internal/freshness/adoption.go internal/freshness/metrics.go
git commit -m "Reap idle and non-canonical suites hourly under the suite's hold (spec C9, ruling R-F1)"
```

(Record the mutation table in the commit message.)

### Task 7: Legacy junk rows (ruling R-F3, ruling PF-3's content rule)

A v6 binary created a suite row for any anchor served with a status below 400, an HTML soft-404 included; the hot path touches the row on every such serve, so idleness never reaps it, and the hot path does not sniff. The reaper does (ruling R-F3), in every run (ruling PF-4): a suite with no current snapshot and at least one cached anchor row (`InRelease`, `Release`, `Release.gpg`), every one of them fetched at least 24 h ago (design note 8) and junk evidence, is reaped regardless of idleness (`reason` `junk`), sorted before the idle candidates (Task 6's `sortCandidates`), so an idle backlog never holds it back. Junk evidence is content, not records (ruling PF-3, design note 9): an anchor is evidence when it fails its own sniff and, for an `InRelease`, the `Release` sniff too — a plain-text Release cached as `InRelease` is the §6.1-quarantined plain-text suite phase E adopts unsigned through `Release`, so it is kept, record or none; every other sniff-failing anchor is evidence whatever the suite's record (none, `anchor_absent`, a failed adoption's, `unsigned_disabled`, `unsigned_too_large`), since an unadopted suite's cached anchor is served as cached, never revalidated. An `InRelease`'s head is read to the `Release` sniff's 64 KiB. A transient soft-404 younger than the grace survives; a missing or unreadable pool file is no evidence, and the reads that fail for another reason than a missing file are logged once per scan with their count (design review M-4). Verdicts are kept by blob between runs (design note 8), so steady-state runs read nothing. The junk query's plan is pinned: it drives from `suite_freshness` and probes `url_path` by its unique key (design review M-14). The junk evidence is not re-read under the hold, only the current snapshot (the design review's underspecified item; `junkCandidates`' AIDEV-NOTE says why that is enough). Junk reaps count in `acu_suites_reaped_total{reason="junk"}` (Task 6). The exec review's surviving X30 (the grace not pinned at its boundary) gets its killer here (ruling PF-9).

**Files:**
- Modify: `internal/cache/suite_reap.go` (`SuiteAnchorRow`, `unadoptedSuiteAnchorsSQL`, `ListUnadoptedSuiteAnchors`), `internal/freshness/reaper.go` (`junkGrace`, `sniffs`, `sniffKey`, `junkCandidates`, `junkReadFailures`, `allJunk`, `anchorEvidence`; `runOnce` merges the junk candidates)
- Create: `internal/cache/unadopted_anchors_test.go`, `internal/freshness/reaper_junk_test.go`

**Interfaces:**
- Consumes: Task 6's `Reaper`, `reapCandidate`, `sortCandidates`, `warn`, `reapsPerRun`, `reaperRig` (`seed`, `adopt`, `exists`, `events`), `reapedTotal`, `reapNow`, `reapDay`; `readCapped`, `errAnchorTooLarge` (`legacy_audit.go`); `aptpath.ClassifySuiteFile`, `SuiteFile.Sniff`, `SuiteFile.HeadBytes`, `aptpath.FileInRelease`, `aptpath.FileRelease`; `(*cache.Cache).BlobPath`, `PutBlob`, `PutURLPath`, `RecordAdoptionFailure`, `EnsureSuiteForTest`, `SeedSuiteForTest`; `(*Adopter).writeBlobBytes`; `cache.ReasonAnchorAbsent`, `cache.ReasonUnsignedDisabled`; `simtest.ClearsignedFixture`; `testHex64`; cache test helpers `openCache`, `seedBlob`, `seedRow`, `sk`, `adoptWith`.
- Produces: `cache.SuiteAnchorRow{SuiteKey; Path string; BlobHash *string; LastFetchedAt *int64}`, `(*cache.Cache).ListUnadoptedSuiteAnchors(ctx) ([]SuiteAnchorRow, error)`, the unexported `unadoptedSuiteAnchorsSQL`; the `suite_reaped` reason `junk`; `junkGrace`; `anchorEvidence(file, path, head) bool`; test helpers `(*reaperRig).junkSuite(suite)`, `cacheAnchor(suite, name, body string, fetched int64)`, `cacheAnchorBlob(suite, name, h string, fetched int64)`, `record(suite, reason string)`, the constant `softPage` (`reaper_junk_test.go`).

- [ ] **Step 1: Write the failing tests**

Create `internal/cache/unadopted_anchors_test.go`:

```go
package cache

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// Ruling R-F3's evidence: ListUnadoptedSuiteAnchors lists the cached
// InRelease, Release and Release.gpg rows of the suites without a current
// snapshot, with their blob and fetch stamp — not an adopted suite's, not
// another file of the suite, not a nested suite's or another host's.
func TestListUnadoptedSuiteAnchors_ListsOnlyUnadoptedSuitesAnchors(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	unadopted, adopted := sk("h", "/r/dists/x"), sk("h", "/r/dists/y")
	seedRow(t, c, unadopted, 1)
	seedRow(t, c, adopted, 1)
	adoptWith(t, c, adopted, map[string]string{"InRelease": "y anchor"})
	blob := seedBlob(t, c, "anchor")
	fetched := int64(1_700_000_000)
	put := func(host, path string) {
		t.Helper()
		if err := c.PutURLPath(ctx, URLPath{CanonicalScheme: "http", CanonicalHost: host, Path: path, BlobHash: &blob,
			UpstreamURL: "http://" + host + path, IsMetadata: true, RequestCount: 1, LastFetchedAt: &fetched}); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"/r/dists/x/InRelease", "/r/dists/x/Release", "/r/dists/x/Release.gpg",
		"/r/dists/x/main/binary-amd64/Packages", "/r/dists/x/dists/z/InRelease", "/r/dists/y/Release"} {
		put("h", p)
	}
	put("other", "/r/dists/x/InRelease")
	got, err := c.ListUnadoptedSuiteAnchors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, a := range got {
		if a.SuiteKey != unadopted || a.BlobHash == nil || *a.BlobHash != blob || a.LastFetchedAt == nil || *a.LastFetchedAt != fetched {
			t.Errorf("row %+v, want %v's with blob %s fetched at %d", a, unadopted, blob, fetched)
		}
		paths = append(paths, a.Path)
	}
	want := []string{"/r/dists/x/InRelease", "/r/dists/x/Release", "/r/dists/x/Release.gpg"}
	if !slices.Equal(paths, want) {
		t.Errorf("paths = %q, want %q", paths, want)
	}
}

// Design review M-14: the junk scan's query drives from suite_freshness
// and probes url_path by its unique key — (canonical_scheme,
// canonical_host, path) for each anchor path — never a scan of url_path,
// which would make the hourly read grow with the cache rather than with
// the suites.
func TestListUnadoptedSuiteAnchors_ProbesURLPathByItsKey(t *testing.T) {
	c := openCache(t)
	rows, err := c.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+unadoptedSuiteAnchorsSQL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	probed := false
	for _, d := range plan {
		if strings.HasPrefix(d, "SCAN u") {
			t.Fatalf("plan %q scans url_path", plan)
		}
		if strings.HasPrefix(d, "SEARCH u USING") && strings.Contains(d, "(canonical_scheme=? AND canonical_host=? AND path=?)") {
			probed = true
		}
	}
	if !probed {
		t.Fatalf("plan %q: want url_path probed by (canonical_scheme, canonical_host, path)", plan)
	}
}
```

Create `internal/freshness/reaper_junk_test.go`:

```go
package freshness

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// junkSuite gives suite a row a client asked for just now — so idleness
// never reaps it — and no cached anchor row.
func (r *reaperRig) junkSuite(suite string) {
	r.t.Helper()
	ctx := context.Background()
	if err := r.cache.EnsureSuiteForTest(ctx, "http", "h", suite); err != nil {
		r.t.Fatal(err)
	}
	now := reapNow
	if err := r.cache.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h",
		SuitePath: suite, LastRequestedAt: &now}); err != nil {
		r.t.Fatal(err)
	}
}

// cacheAnchor caches body as suite's anchor name (InRelease, Release or
// Release.gpg), fetched at the unix time fetched.
func (r *reaperRig) cacheAnchor(suite, name, body string, fetched int64) {
	r.t.Helper()
	ctx := context.Background()
	h, err := r.adopter.writeBlobBytes(ctx, []byte(body))
	if err != nil {
		r.t.Fatal(err)
	}
	r.cacheAnchorBlob(suite, name, h, fetched)
}

// cacheAnchorBlob points suite's anchor name at blob h — a blob row whose
// pool file need not exist — fetched at the unix time fetched.
func (r *reaperRig) cacheAnchorBlob(suite, name, h string, fetched int64) {
	r.t.Helper()
	if err := r.cache.PutBlob(context.Background(), h, 1); err != nil {
		r.t.Fatal(err)
	}
	if err := r.cache.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h",
		Path: suite + "/" + name, BlobHash: &h, UpstreamURL: "http://h" + suite + "/" + name, IsMetadata: true,
		RequestCount: 1, LastFetchedAt: &fetched}); err != nil {
		r.t.Fatal(err)
	}
}

// record gives suite an adoption record with reason.
func (r *reaperRig) record(suite, reason string) {
	r.t.Helper()
	if err := r.cache.RecordAdoptionFailure(context.Background(), "http", "h", suite, cache.AdoptionFailure{Hash: "k",
		Reason: reason, Count: 1, FirstAt: reapNow, LastAt: reapNow, NextRetryAt: reapNow + 60}); err != nil {
		r.t.Fatal(err)
	}
}

const softPage = "<!DOCTYPE html><html><body>Not Found</body></html>\n"

// Ruling R-F3 with ruling PF-3's content rule: a tracked suite with no
// current snapshot whose cached anchor rows are all junk evidence, fetched
// at least 24 h ago, is reaped as legacy junk although a client asked for
// it just now — whatever its adoption record says: none, anchor_absent, a
// failed adoption's (untrusted_signer) or unsigned_disabled. Kept: a
// transient soft-404 younger than the grace; a plain-text Release cached
// as InRelease (a §6.1-quarantined plain-text suite, adopted unsigned
// through Release), with no record at all and its hash block past the
// first 512 bytes (the Release sniff reads 64 KiB); a suite with any
// anchor that passes its sniff; one with no anchor row; one whose pool
// file is missing (no evidence); an adopted suite. Each junk reap counts
// in acu_suites_reaped_total{reason="junk"}.
func TestReaper_ReapsLegacyJunkKeepsTransientSoft404(t *testing.T) {
	r := newReaperRig(t)
	old, young := reapNow-2*reapDay, reapNow-3600
	signed := string(simtest.ClearsignedFixture("Origin: x\nSHA256:\n"))
	for _, s := range []string{"/r/dists/junk", "/r/dists/absent", "/r/dists/detached", "/r/dists/failing",
		"/r/dists/disabled", "/r/dists/soft404", "/r/dists/plain", "/r/dists/good", "/r/dists/mixed",
		"/r/dists/norows", "/r/dists/missing", "/r/dists/adopted"} {
		r.junkSuite(s)
	}
	r.cacheAnchor("/r/dists/junk", "InRelease", softPage, old)
	r.cacheAnchor("/r/dists/absent", "InRelease", softPage, old)
	r.record("/r/dists/absent", cache.ReasonAnchorAbsent)
	r.cacheAnchor("/r/dists/detached", "Release", softPage, old)
	r.cacheAnchor("/r/dists/detached", "Release.gpg", softPage+"sig", old)
	r.cacheAnchor("/r/dists/failing", "InRelease", softPage, old)
	r.record("/r/dists/failing", "untrusted_signer") // upstream signs now; its keys are not installed
	r.cacheAnchor("/r/dists/disabled", "InRelease", softPage, old)
	r.record("/r/dists/disabled", cache.ReasonUnsignedDisabled)
	r.cacheAnchor("/r/dists/soft404", "InRelease", softPage, young)
	// A long Description puts the hash block past the InRelease sniff's
	// 512 bytes, as in many real Release files.
	r.cacheAnchor("/r/dists/plain", "InRelease", "Origin: x\nDescription: "+strings.Repeat("x", 600)+"\nSHA256:\n", old)
	r.cacheAnchor("/r/dists/good", "InRelease", signed, old)
	r.cacheAnchor("/r/dists/mixed", "InRelease", softPage, old)
	r.cacheAnchor("/r/dists/mixed", "Release", "Origin: x\nSHA256:\n "+testHex64+" 1 main/Packages\n", old)
	r.cacheAnchorBlob("/r/dists/missing", "InRelease", testHex64, old)
	r.cacheAnchor("/r/dists/adopted", "InRelease", softPage, old)
	r.adopt("/r/dists/adopted", "adopted anchor")
	junk0 := reapedTotal(t, "junk")
	r.reaper.runOnce(context.Background())
	for _, s := range []string{"/r/dists/junk", "/r/dists/absent", "/r/dists/detached", "/r/dists/failing", "/r/dists/disabled"} {
		if r.exists(s) {
			t.Errorf("%s was not reaped as junk", s)
		}
	}
	for _, s := range []string{"/r/dists/soft404", "/r/dists/plain", "/r/dists/good", "/r/dists/mixed", "/r/dists/norows",
		"/r/dists/missing", "/r/dists/adopted"} {
		if !r.exists(s) {
			t.Errorf("%s was reaped", s)
		}
	}
	for _, ev := range r.events("suite_reaped") {
		if ev["reason"] != "junk" {
			t.Errorf("suite_reaped %v, want reason junk", ev)
		}
	}
	if d := reapedTotal(t, "junk") - junk0; d != 5 {
		t.Fatalf(`acu_suites_reaped_total{reason="junk"} rose by %v, want 5`, d)
	}
}

// A junk sniff's verdict is kept by blob from one run to the next (the
// pool is content-addressed), so a run reads only the anchors it has not
// sniffed: here the second run reaps on the first run's verdict for an
// InRelease whose pool file has gone since.
func TestReaper_RemembersJunkVerdictsByBlob(t *testing.T) {
	r := newReaperRig(t)
	ctx := context.Background()
	old := reapNow - 2*reapDay
	r.junkSuite("/r/dists/x")
	r.cacheAnchor("/r/dists/x", "InRelease", softPage, old)
	r.cacheAnchor("/r/dists/x", "Release", softPage+"release", reapNow-3600)
	r.reaper.runOnce(ctx)
	if !r.exists("/r/dists/x") {
		t.Fatal("reaped while one anchor was younger than the grace")
	}
	row, err := r.cache.LookupURL(ctx, "http", "h", "/r/dists/x/InRelease")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(r.cache.BlobPath(*row.BlobHash)); err != nil {
		t.Fatal(err)
	}
	r.cacheAnchor("/r/dists/x", "Release", softPage+"release", old)
	r.reaper.runOnce(ctx)
	if r.exists("/r/dists/x") {
		t.Fatal("not reaped: the second run read the InRelease again instead of reusing its verdict")
	}
}

// Ruling R-F3, design note 8: the junk grace is 24 h — an anchor fetched
// 23 h ago may still be a transient soft-404, one fetched 25 h ago is junk
// evidence.
func TestReaper_TheJunkGraceIsADay(t *testing.T) {
	r := newReaperRig(t)
	r.junkSuite("/r/dists/young")
	r.junkSuite("/r/dists/old")
	r.cacheAnchor("/r/dists/young", "InRelease", softPage, reapNow-23*3600)
	r.cacheAnchor("/r/dists/old", "InRelease", softPage, reapNow-25*3600)
	r.reaper.runOnce(context.Background())
	if !r.exists("/r/dists/young") || r.exists("/r/dists/old") {
		t.Fatalf("fetched 23 h ago: exists %v; 25 h ago: exists %v; want kept and reaped",
			r.exists("/r/dists/young"), r.exists("/r/dists/old"))
	}
}

// Ruling PF-4 (design review I-4): a junk suite fails its clients'
// updates, so it is not held back by an idle backlog: with 30 idle suites
// waiting — more than a run reaps — the junk suite, asked for just now,
// goes in the first run.
func TestReaper_JunkIsNotStarvedByAnIdleBacklog(t *testing.T) {
	r := newReaperRig(t)
	for i := range 30 {
		r.seed(fmt.Sprintf("/r/dists/idle%02d", i), reapNow-31*reapDay-int64(i))
	}
	r.junkSuite("/r/dists/junk")
	r.cacheAnchor("/r/dists/junk", "InRelease", softPage, reapNow-2*reapDay)
	r.reaper.runOnce(context.Background())
	if r.exists("/r/dists/junk") {
		t.Fatal("the junk suite waited behind the idle backlog")
	}
	if n := len(r.events("suite_reaped")); n != reapsPerRun {
		t.Fatalf("%d reaped, want %d", n, reapsPerRun)
	}
}

// Design review M-4: pool reads that fail — here a directory where the
// blob should be — keep their suites (an I/O fault is never evidence) and
// are logged once per scan with their count, not once per anchor.
func TestReaper_JunkReadFailuresLogOncePerScan(t *testing.T) {
	r := newReaperRig(t)
	for _, s := range []string{"/r/dists/a", "/r/dists/b"} {
		h := strings.Repeat(s[len(s)-1:], 64)
		r.junkSuite(s)
		r.cacheAnchorBlob(s, "InRelease", h, reapNow-2*reapDay)
		if err := os.MkdirAll(r.cache.BlobPath(h), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r.reaper.runOnce(context.Background())
	if !r.exists("/r/dists/a") || !r.exists("/r/dists/b") {
		t.Fatal("an unreadable anchor was taken for junk evidence")
	}
	if ev := r.events("suite reaper: junk anchor reads failed"); len(ev) != 1 || ev[0]["count"] != float64(2) {
		t.Fatalf("read failure lines = %v, want one with count 2", ev)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -gcflags=-e ./internal/cache/ ./internal/freshness/ -run 'TestListUnadoptedSuiteAnchors|TestReaper_ReapsLegacyJunk|TestReaper_RemembersJunkVerdicts|TestReaper_TheJunkGrace|TestReaper_JunkIs|TestReaper_JunkRead'`
Expected: cache: compile failures `c.ListUnadoptedSuiteAnchors undefined` and `undefined: unadoptedSuiteAnchorsSQL`; freshness: FAIL — `/r/dists/junk was not reaped as junk` (and `absent`, `detached`, `failing`, `disabled`), `acu_suites_reaped_total{reason="junk"} rose by 0, want 5`, `not reaped: the second run read the InRelease again instead of reusing its verdict`, `fetched 23 h ago: exists true; 25 h ago: exists true`, `the junk suite waited behind the idle backlog`, `read failure lines = [], want one with count 2`.

- [ ] **Step 3: Implement**

At the end of `internal/cache/suite_reap.go`:

```go
// SuiteAnchorRow is a cached anchor url_path row — <suite>/InRelease,
// <suite>/Release or <suite>/Release.gpg — of a suite without a current
// snapshot: the evidence of the reaper's legacy junk rule (ruling R-F3).
type SuiteAnchorRow struct {
	SuiteKey
	Path          string
	BlobHash      *string
	LastFetchedAt *int64
}

// unadoptedSuiteAnchorsSQL is ListUnadoptedSuiteAnchors' query.
//
// AIDEV-NOTE: design review M-14: SQLite drives from suite_freshness and
// probes url_path's unique key (canonical_scheme, canonical_host, path)
// once per anchor path, so the cost grows with the suites, not with
// url_path (measured: 73 ms for 5,000 suites over 215,000 url_path rows,
// on the read pool). Keep the join's url_path terms bare columns: an
// expression on them turns each probe into a scan of the host's rows
// (TestListUnadoptedSuiteAnchors_ProbesURLPathByItsKey pins the plan).
const unadoptedSuiteAnchorsSQL = `
SELECT sf.canonical_scheme, sf.canonical_host, sf.suite_path, u.path, u.blob_hash, u.last_fetched_at
  FROM suite_freshness sf
  JOIN url_path u
    ON u.canonical_scheme = sf.canonical_scheme
   AND u.canonical_host   = sf.canonical_host
   AND u.path IN (sf.suite_path || '/InRelease', sf.suite_path || '/Release', sf.suite_path || '/Release.gpg')
 WHERE sf.current_snapshot_id IS NULL
 ORDER BY sf.canonical_scheme, sf.canonical_host, sf.suite_path, u.path`

// ListUnadoptedSuiteAnchors returns the cached anchor rows of every suite
// that has no current snapshot, in suite and path order. One read on the
// pool per reaper run.
func (c *Cache) ListUnadoptedSuiteAnchors(ctx context.Context) ([]SuiteAnchorRow, error) {
	rows, err := c.db.QueryContext(ctx, unadoptedSuiteAnchorsSQL)
	if err != nil {
		return nil, fmt.Errorf("ListUnadoptedSuiteAnchors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SuiteAnchorRow
	for rows.Next() {
		var a SuiteAnchorRow
		if err := rows.Scan(&a.CanonicalScheme, &a.CanonicalHost, &a.SuitePath, &a.Path, &a.BlobHash, &a.LastFetchedAt); err != nil {
			return nil, fmt.Errorf("ListUnadoptedSuiteAnchors: scan: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListUnadoptedSuiteAnchors: %w", err)
	}
	return out, nil
}
```

In `internal/freshness/reaper.go`:
- the import block gains `"io/fs"`; the constant block gains, after `suiteHoldTimeout`:

```go
	// junkGrace is ruling R-F3's grace (design note 8): a legacy junk
	// suite's cached anchor rows must all have been fetched at least this
	// long ago, so a transient soft-404 is never taken for junk.
	junkGrace = 24 * time.Hour
```

- `Reaper` gains a last field, after `reported`:

```go
	// sniffs holds the previous run's junk verdicts by blob (ruling R-F3);
	// only the run's goroutine touches it.
	sniffs map[sniffKey]bool
```

  and the key type follows `futureStamp`:

```go
// sniffKey keys a junk verdict: what a blob's head is to one kind of
// anchor never changes, since the pool is content-addressed.
type sniffKey struct {
	file aptpath.SuiteFile
	hash string
}
```

- `runOnce` merges the junk candidates (its doc says so):

```go
// runOnce is one reaper run (spec C9): unless a forward step of the wall
// clock defers it, reap at most reapsPerRun of the candidates — idle,
// non-canonical and legacy junk (junkCandidates, every run: ruling PF-4)
// — those reaped regardless of idleness first, then the oldest; then
// sweep the reconcile memo against the current snapshots.
func (r *Reaper) runOnce(ctx context.Context) {
	rows, err := r.cache.ListSuitesWithAdoption(ctx)
	if err != nil {
		r.warn(ctx, "suite reaper: list suites failed", "err", err)
		return
	}
	now := r.checker.now()
	deferred := r.deferredByForwardStep(now, time.Now())
	if !deferred && r.idleTTL > 0 {
		cands := r.candidates(rows, now)
		cands = append(cands, r.junkCandidates(ctx, rows, cands, now)...)
		sortCandidates(cands)
		for _, c := range cands[:min(len(cands), reapsPerRun)] {
			if ctx.Err() != nil {
				return
			}
			r.reapSuite(ctx, c)
		}
	}
	r.sweepReconcileMemo(rows)
}
```

- before `sweepReconcileMemo`:

```go
// junkCandidates returns ruling R-F3's legacy junk suites that are not
// already in have, as candidates reaped regardless of idleness: a suite
// without a current snapshot whose cached anchor rows (InRelease, Release,
// Release.gpg) — at least one — were all fetched at least junkGrace ago
// and are all junk evidence (anchorEvidence). A read failure of the anchor
// rows skips the rule for this run; pool reads that fail are logged once,
// with their count (design review M-4).
//
// AIDEV-NOTE: legacy junk is a suite a v6 binary created for an HTML page
// served as InRelease: every anchor serve with a status below 400 touches
// it, so idleness never reaps it, and the hot path does not sniff (ruling
// R-F3). The scan runs every run (ruling PF-4): such a suite fails its
// clients' updates, and the scan is one indexed read on the pool
// (ListUnadoptedSuiteAnchors) plus pool reads only for the blobs no
// earlier run sniffed. Two things keep a real suite: the grace (design
// note 8: a transient soft-404 that replaced a good anchor is younger; its
// next check or a later miss heals the row) and the content rule (ruling
// PF-3, design note 9: a plain-text Release cached as InRelease is a
// §6.1-quarantined plain-text suite phase E adopts unsigned, not junk).
// Adoption records are ignored: a failed adoption's record,
// unsigned_disabled or unsigned_too_large would otherwise keep an HTML
// anchor served forever (a suite without a snapshot has its url_path
// metadata served as cached, never revalidated). Only blobs read and
// sniffed are evidence: a missing or unreadable pool file keeps the suite
// (overview: an I/O fault is never evidence). The evidence is not read
// again under the hold, which re-reads only the current snapshot
// (reapSuite): between the scan and the hold a junk suite's anchor rows
// change only through an adoption, which that comparison catches, or
// through url_path GC or a miss that re-fetched a row whose pool file had
// gone, after which the reap costs the suite one fetch at its next
// request. The sniffs run here, on the reaper's goroutine, never on the
// writer (ruling PD-5).
func (r *Reaper) junkCandidates(ctx context.Context, rows []cache.SuiteWithAdoption, have []reapCandidate, now time.Time) []reapCandidate {
	anchors, err := r.cache.ListUnadoptedSuiteAnchors(ctx)
	if err != nil {
		r.warn(ctx, "suite reaper: list unadopted anchors failed", "err", err)
		return nil
	}
	bySuite := map[cache.SuiteKey][]cache.SuiteAnchorRow{}
	for _, a := range anchors {
		bySuite[a.SuiteKey] = append(bySuite[a.SuiteKey], a)
	}
	listed := map[cache.SuiteKey]bool{}
	for _, c := range have {
		listed[c.key] = true
	}
	cutoff := now.Add(-junkGrace).Unix()
	next := map[sniffKey]bool{}
	var fails junkReadFailures
	var out []reapCandidate
	for i := range rows {
		s := &rows[i].SuiteFreshness
		key := cache.SuiteKey{CanonicalScheme: s.CanonicalScheme, CanonicalHost: s.CanonicalHost, SuitePath: s.SuitePath}
		as := bySuite[key]
		if listed[key] || len(as) == 0 || !r.allJunk(key, as, cutoff, next, &fails) {
			continue
		}
		out = append(out, reapCandidate{key: key, requested: s.LastRequestedAt, reason: "junk", cutoff: cache.ReapRegardless})
	}
	r.sniffs = next
	if fails.n > 0 {
		r.warn(ctx, "suite reaper: junk anchor reads failed", "count", fails.n, "err", fails.first,
			"canonical_host", fails.host, "path", fails.path)
	}
	return out
}

// junkReadFailures counts one junk scan's failed pool reads, keeping the
// first, so a scan logs one line however many fail (design review M-4).
type junkReadFailures struct {
	n          int
	first      error
	host, path string
}

// allJunk reports whether every one of key's cached anchor rows holds a
// blob fetched at or before cutoff that is junk evidence (anchorEvidence).
// A row without a blob or a fetch stamp, a fetch stamp after cutoff (a
// future one included: the clock-step rule), or a pool file that is
// missing or does not read, is no evidence; a read that fails for another
// reason than a missing file is counted in fails. Each verdict goes into
// next; one the previous run reached (r.sniffs) is reused without reading
// the pool file again, so a run reads only the anchors it has not sniffed
// (at most 64 KiB each).
func (r *Reaper) allJunk(key cache.SuiteKey, as []cache.SuiteAnchorRow, cutoff int64, next map[sniffKey]bool, fails *junkReadFailures) bool {
	for _, a := range as {
		if a.BlobHash == nil || a.LastFetchedAt == nil || *a.LastFetchedAt > cutoff {
			return false
		}
		file := aptpath.ClassifySuiteFile(key.SuitePath, a.Path)
		k := sniffKey{file: file, hash: *a.BlobHash}
		junk, ok := r.sniffs[k]
		if !ok {
			n := file.HeadBytes()
			if file == aptpath.FileInRelease {
				n = aptpath.FileRelease.HeadBytes() // the Release sniff reads up to 64 KiB
			}
			head, err := readCapped(r.cache.BlobPath(*a.BlobHash), int64(n))
			if err != nil && !errors.Is(err, errAnchorTooLarge) {
				if !errors.Is(err, fs.ErrNotExist) {
					if fails.n == 0 {
						fails.first, fails.host, fails.path = err, key.CanonicalHost, a.Path
					}
					fails.n++
				}
				return false
			}
			junk = anchorEvidence(file, a.Path, head)
		}
		next[k] = junk
		if !junk {
			return false
		}
	}
	return true
}

// anchorEvidence reports whether head, the start of a cached anchor of
// kind file at path, is junk evidence (ruling PF-3): it fails its own
// sniff and, for an InRelease, the Release sniff too — a plain-text
// Release cached as InRelease is a §6.1-quarantined plain-text suite on
// its way to an unsigned adoption through Release, not junk.
func anchorEvidence(file aptpath.SuiteFile, path string, head []byte) bool {
	if file.Sniff(path, head) {
		return false
	}
	return file != aptpath.FileInRelease || !aptpath.FileRelease.Sniff(path, head)
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/cache/ ./internal/freshness/`
Expected: PASS.

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| `allJunk` without the grace (`*a.LastFetchedAt > cutoff` dropped) | `TestReaper_ReapsLegacyJunkKeepsTransientSoft404` (`/r/dists/soft404 was reaped`), `TestReaper_RemembersJunkVerdictsByBlob`, `TestReaper_TheJunkGraceIsADay` |
| a read failure skips the row (`return false` → `continue`) | `TestReaper_ReapsLegacyJunkKeepsTransientSoft404` (`/r/dists/missing was reaped`), `TestReaper_JunkReadFailuresLogOncePerScan` (`an unreadable anchor was taken for junk evidence`) |
| any junk anchor makes the suite junk (`if junk { return true }`) | `TestReaper_ReapsLegacyJunkKeepsTransientSoft404` (`/r/dists/good was reaped`, `/r/dists/mixed was reaped`), `TestReaper_RemembersJunkVerdictsByBlob` |
| `runOnce` without the junk candidates | `TestReaper_ReapsLegacyJunkKeepsTransientSoft404` (`junk`, `absent`, `detached`, `failing`, `disabled` not reaped), the other three junk-reaping tests |
| `r.sniffs = next` dropped | `TestReaper_RemembersJunkVerdictsByBlob` |
| the query without `WHERE sf.current_snapshot_id IS NULL` | `TestListUnadoptedSuiteAnchors_ListsOnlyUnadoptedSuitesAnchors` (`/r/dists/y/…` rows) |
| the query without `Release.gpg` | `TestListUnadoptedSuiteAnchors_ListsOnlyUnadoptedSuitesAnchors` |
| PF-3: `anchorEvidence` ignores the `Release` sniff on an `InRelease` row (its last line `return true`) | `TestReaper_ReapsLegacyJunkKeepsTransientSoft404` (`/r/dists/plain was reaped`) |
| PF-3: an `InRelease` head read at its own 512 bytes (the `FileRelease.HeadBytes()` line dropped) | `TestReaper_ReapsLegacyJunkKeepsTransientSoft404` (`/r/dists/plain was reaped`) |
| PF-4: `sortCandidates` by stamp only | `TestReaper_JunkIsNotStarvedByAnIdleBacklog` (`the junk suite waited behind the idle backlog`), `TestReaperCandidates_ReapedRegardlessGoFirst` |
| X30: `junkGrace = 2 * time.Hour` | `TestReaper_TheJunkGraceIsADay` (`fetched 23 h ago: exists false`) |
| X30: `junkGrace = 48 * time.Hour` | `TestReaper_TheJunkGraceIsADay` (`25 h ago: exists true`) |
| M-4: a WARN per failed read | `TestReaper_JunkReadFailuresLogOncePerScan` (`read failure lines = [… three …], want one with count 2`) |
| M-4: the count not kept (`fails.n = 1`) | `TestReaper_JunkReadFailuresLogOncePerScan` (`count:1`) |
| M-14: the url_path probe on an expression (`u.path \|\| '' IN (…)`) | `TestListUnadoptedSuiteAnchors_ProbesURLPathByItsKey` (`plan ["SCAN sf USING INDEX sqlite_autoindex_suite_freshness_1" "SEARCH u USING INDEX sqlite_autoindex_url_path_1 (canonical_scheme=? AND canonical_host=?)"]`) |
| M-16: junk candidates reaped with `reason: "idle"` | `TestReaper_ReapsLegacyJunkKeepsTransientSoft404` (`want reason junk`, `rose by 0, want 5`) |

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/cache/suite_reap.go internal/cache/unadopted_anchors_test.go internal/freshness/reaper.go internal/freshness/reaper_junk_test.go
git add internal/cache/suite_reap.go internal/cache/unadopted_anchors_test.go internal/freshness/reaper.go internal/freshness/reaper_junk_test.go
git commit -m "Reap legacy junk suites after a 24 h grace, keeping plain-text Release anchors (rulings R-F3, PF-3, PF-4)"
```

(Record the mutation table in the commit message.)

### Task 8: Unsigned suites with the knob off keep their coherent snapshot (ruling PF-5)

Ruling PF-5 (design review I-5) drops R-F4's retirement and reverses the F half of phase E ruling PE-12: with `adoption.adopt_unsigned = false`, an established unsigned suite keeps phase E's coherent frozen snapshot — refused `unsigned_disabled`, visible and paced (phase E's `TestCheck_UnsignedSuiteWithTheKnobOffIsAPacedRefusal`) — and the reaper does not touch it. Retired into the ordinary cache, each index file would be frozen at its own first fetch (an unadopted suite's url_path metadata is served as cached, never revalidated), and every upstream publish would bring Hash Sum mismatches that never clear; a coherent stale snapshot fails softer, needs no code and is reversible. The maintainer confirmed it (2026-10-20). So this task has no production code: it pins the behaviour with a handler test (after upstream moves on, a check refuses the new `Release` and the reaper runs, the suite still serves its snapshot's `Release` and `Packages` as one coherent pair), corrects `unsignedRefusal`'s AIDEV-NOTE (which said the retirement was phase F's), and writes the honest `adopt_unsigned = false` semantics into `docs/configuration.md` — the knob's row, the `unsigned_disabled` refusal and safeguard 1: the cache does not keep unsigned repositories fresh while the knob is off; an adopted one stays at its last snapshot until the knob is turned back on (or the suite is forgotten, or reaped as idle); a never-adopted one is served as first cached; set it only if you do not serve unsigned repositories through the cache. Spec C5 and the overview's PE-12 carry-forward are Task 16's. The task keeps its number (later task numbers stay stable); R-F4's retirement code, its tests (`reaper_unsigned_test.go`, `unsigned_retire_test.go`), `RetireSuiteSnapshots` (Task 3) and the INFO `unsigned_snapshot_retired` are gone from every task.

**Files:**
- Modify: `internal/freshness/first_adoption.go` (`unsignedRefusal`'s AIDEV-NOTE), `docs/configuration.md` (`adopt_unsigned`)
- Create: `internal/handler/unsigned_knob_off_test.go`

**Interfaces:**
- Consumes: Task 6's `freshness.NewReaper`, `ReaperConfig`, `(*Reaper).Run`; `freshness.New`, `(*Checker).Check`, `freshness.NewAdopter` (`AdoptUnsigned`), `(*Adopter).RunUnsigned`; `cache.ReasonUnsignedDisabled`, `(*cache.Cache).GetSuiteFreshness`; the handler's `flipStack` (`newFlipStack`, `publish`, `track`, `current`, `members`, `ad`, `h`), `staticUpstream`, `plainVerifier`, `hdrXCacheSnapshot`, `flipSimHost`; `simtest.NewUpstream`, `simtest.Serve`, `simtest.DiscardLogger`; `hostsem.New`.
- Produces: nothing new.

- [ ] **Step 1: Write the test**

Create `internal/handler/unsigned_knob_off_test.go`:

```go
package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// Ruling PF-5 (it reverses the F half of phase E ruling PE-12; spec C5
// safeguard 1): with adoption.adopt_unsigned off, an established unsigned
// suite keeps serving its last snapshot — its Release and Packages as one
// coherent pair, with X-Cache-Snapshot — after upstream has moved on, a
// check has refused the new Release (unsigned_disabled, visible) and the
// suite reaper has run. Nothing returns it to the ordinary cache, where
// each index file would be frozen at its own first fetch and every
// upstream publish would bring Hash Sum mismatches that never clear.
func TestServeHTTP_KnobOffKeepsServingTheUnsignedSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		s := newFlipStack(t, 0) // its adopter's adopt_unsigned is off
		const suite = "/u/dists/s"
		releaseURL := "http://" + flipSimHost + suite + "/Release"
		pkgsURL := "http://" + flipSimHost + suite + "/main/binary-amd64/Packages"
		ref, rel1 := s.publish(suite, 1)
		s.members.mu.Lock()
		pkgs1 := s.members.bodies[pkgsURL]
		s.members.mu.Unlock()
		s.track(suite)
		on, err := freshness.NewAdopter(freshness.AdoptionConfig{Cache: s.c, Fetcher: s.members, Verifier: plainVerifier{},
			HostLimiter: hostsem.New(8), Logger: simtest.DiscardLogger(), AdoptUnsigned: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := on.RunUnsigned(ctx, ref, rel1, "", ""); err != nil { // adopted while the knob was on
			t.Fatal(err)
		}
		snap := s.current(suite)
		get := func(path string) (snapshot, body string, code int) {
			rec := httptest.NewRecorder()
			s.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+flipSimHost+path, nil))
			return rec.Header().Get(hdrXCacheSnapshot), rec.Body.String(), rec.Code
		}
		_, rel2 := s.publish(suite, 2) // upstream moves on
		s.members.mu.Lock()
		pkgs2 := s.members.bodies[pkgsURL]
		s.members.mu.Unlock()
		s.h.fetch = staticUpstream{releaseURL: rel2, pkgsURL: pkgs2}
		up := simtest.NewUpstream()
		up.Set(releaseURL, simtest.Serve(rel2))
		chk, err := freshness.New(freshness.Config{Cache: s.c, Fetcher: up, HostLimiter: hostsem.New(8),
			Adopter: s.ad, Logger: simtest.DiscardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		chk.Check(ctx, "http", flipSimHost, suite)
		if sf, err := s.c.GetSuiteFreshness(ctx, "http", flipSimHost, suite); err != nil || sf.AdoptionFailReason == nil ||
			*sf.AdoptionFailReason != cache.ReasonUnsignedDisabled {
			t.Fatalf("after the check: %+v, %v; want the new Release refused, unsigned_disabled", sf, err)
		}
		rp, err := freshness.NewReaper(freshness.ReaperConfig{Cache: s.c, Checker: chk, IdleTTL: 30 * 24 * time.Hour,
			Logger: simtest.DiscardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { rp.Run(runCtx); close(done) }()
		time.Sleep(time.Hour + time.Second) // the uptime floor: the first run
		synctest.Wait()
		cancel()
		<-done
		for path, want := range map[string][]byte{suite + "/Release": rel1, suite + "/main/binary-amd64/Packages": pkgs1} {
			if got, body, code := get(path); got != snap || body != string(want) || code != http.StatusOK {
				t.Fatalf("%s after the run: snapshot %q, status %d, the snapshot's bytes %v; want snapshot %s's, a coherent pair",
					path, got, code, body == string(want), snap)
			}
		}
		if got := s.current(suite); got != snap {
			t.Fatalf("current snapshot %q after the run, want %s kept", got, snap)
		}
	})
}
```

- [ ] **Step 2: Run it**

Run: `go test -race ./internal/handler/ -run 'TestServeHTTP_KnobOffKeepsServingTheUnsignedSnapshot'`
Expected: PASS at once — Task 6's reaper has no knob-off code and this task adds none; the test pins that (it fails on the draft's retirement, Step 5).

- [ ] **Step 3: The note and the docs**

In `internal/freshness/first_adoption.go`, `unsignedRefusal`'s AIDEV-NOTE replaces "The established suite keeps serving its last snapshot and is never updated unsigned; returning it to the ordinary cache, as configuration.md's "served from the ordinary cache, unadopted" promises, is phase F's (retire the snapshot through the reaper's retirement path; overview F carry-forward)." with (its first line is the note's unchanged context; Task 14 moves the function, note and all, to `unsigned.go`):

```go
// suiteDue — so checks inside its window ask upstream nothing. The
// established suite keeps serving its last snapshot, coherent and never
// updated unsigned, until the knob is turned back on, the suite is
// forgotten (POST /forget) or it is reaped as idle: ruling PF-5 dropped
// the retirement PE-12 left to phase F — returned to the ordinary cache,
// each index file would be frozen at its own first fetch, and every
// upstream publish would bring Hash Sum mismatches that never clear
// (TestServeHTTP_KnobOffKeepsServingTheUnsignedSnapshot). Before PE-12 the
// knob off failed every check, with no record: an established suite froze
// unpaced and invisible (final reviews E1, E2).
```

In `docs/configuration.md`, the `[adoption]` table's `adopt_unsigned` row becomes:

```markdown
| `adopt_unsigned` | boolean | `true` | Adopt a repository that publishes a `Release` with no signature (neither `InRelease` nor `Release.gpg`) as an unsigned snapshot: served coherently, but never trusted — it vouches no `.deb` and never counts for `integrity.refuse_unvouched_debs`. A suite the cache has seen signed is never adopted unsigned. `false` refuses every unsigned `Release` ("refused: unsigned_disabled", a state, not a failed check), so the cache no longer keeps unsigned repositories fresh: an unsigned suite already adopted keeps serving its last snapshot, coherent but frozen — never updated — until the knob is turned back on (it then updates at its next retry, at most 75 minutes later: a restart keeps a refusal's retry time), the suite is forgotten (`POST /forget`), or it is reaped after `gc.suite_idle_ttl` without a client; a repository not yet adopted stays unadopted and is served as the cache first stored each of its files, never refreshed, so an upstream publish can leave its clients with Hash Sum mismatches. Set it to `false` only if you do not serve unsigned repositories through the cache. Either way the suite is asked upstream again only at its retry times (2 minutes, doubling to an hour; for a suite not yet adopted, also once `periodic_refresh` has passed), never at the checks clients trigger in between. See *Anchor forms and unsigned repositories* for the safeguards and the threat notes. |
```

the `unsigned_disabled` entry of the refusal list becomes:

```markdown
- `unsigned_disabled`: `adopt_unsigned = false`, so an unsigned `Release` is not adopted; an unsigned suite already adopted keeps serving its last snapshot, not updated, until the knob is turned back on (or the suite is forgotten).
```

and safeguard 1 under *Anchor forms and unsigned repositories* becomes:

```markdown
1. `adopt_unsigned` (default `true`). With `false` an unsigned `Release`
   is refused ("refused: unsigned_disabled"), and the cache no longer
   keeps unsigned repositories fresh: an unsigned suite already adopted
   keeps serving its last snapshot, coherent but frozen, until the knob
   is turned back on or the suite is forgotten (`POST /forget`); a suite
   not yet adopted stays unadopted and is served as the cache first
   stored its files. Set it only if you do not serve unsigned
   repositories through the cache. The suite is asked upstream only at
   its retry times; a signature appearing upstream still moves it to a
   signed form, at the next retry.
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/handler/ ./internal/freshness/`
Expected: PASS.

- [ ] **Step 5: Mutation check** (reverted after)

| Mutation | Fails |
|---|---|
| the reaper removes an established unsigned suite while the knob is off (a first `candidates` case: `CurrentAnchorForm == unsigned && !AdoptsUnsigned()` → reaped regardless — the draft's retirement in effect) | `TestServeHTTP_KnobOffKeepsServingTheUnsignedSnapshot` (`/u/dists/s/Release after the run: snapshot "", status 200, the snapshot's bytes false`) |

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/handler/unsigned_knob_off_test.go internal/freshness/first_adoption.go
git add internal/handler/unsigned_knob_off_test.go internal/freshness/first_adoption.go docs/configuration.md
git commit -m "Keep the coherent snapshot of an unsigned suite while adopt_unsigned is off (ruling PF-5)"
```

(Record the mutation in the commit message.)

### Task 9: The orphan-marker sweep (rulings R-F5, PF-1), the reaper's daily duty, and the marker stamp's one reader

Spec C9 and C3: the reaper sweeps the signed-seen markers whose base has no suite row of its host — neither the base nor its `/.` twin, under either scheme (design note 13) — and whose `last_seen_at` is older than 365 days, in writer transactions of at most 1,000, whatever `gc.suite_idle_ttl` says; each delete re-checks, in its transaction, that the marker is still an orphan and still a year old (the exec review's X16/X17 killer pins both). A marker survives its suite's reap until then (`/forget`'s rule is Task 3's; a reap also moves the base's markers up to the reap, design review M-9, Task 3). The clock-step rule for a gate that retires things (ruling R-F5) with ruling PF-1 (design review I-1, which overrides R-F5's "healed"; deliberate deviation 3): the sweep never rewrites a stamp. A marker stamped more than 2 s in the future reads as now — never an orphan candidate — and the furthest is reported through the rate-limited `clock_step_detected` (`stamp=last_seen_at`) at the first sweep after a start and whenever it changes (design review M-10); it ages out only once the clock has passed it. Rewriting it to now, as the draft did, deleted signed history after a backward step was corrected (`TestSweepOrphanMarkers_BackwardStepThenCorrectionSweepsNothing`). The sweep is the reaper's daily duty (design note 20 with ruling PF-4: the junk scan stays in every run, Task 7, so `TestReaper_RemembersJunkVerdictsByBlob` needs no day between its runs): it runs at the first run after a start and then a day apart (monotonic); a run the forward-step guard defers (Task 6) does not sweep and leaves the sweep due (design review M-8). The never-downgrade authority's delete sites stay pinned (design review M-5): `TestSignedSeenHasKnownDeleteSites` gains the sweep. The sweep is the stamp's reader, so deferred minor E-18 lands: `(*cache.URLPath).LastUse()` replaces the freshness and handler copies (the handler's is a mechanical replacement, ruling PD-1); and E-20: a test that every sighting advances the stamp. E-40's `types.go` part (the `SuiteWithAdoption` doc) rides along. The exec review's surviving X31 (the TTL not pinned at its boundary) gets its killer here (ruling PF-9).

**Files:**
- Create: `internal/cache/marker_sweep.go`, `internal/cache/marker_sweep_test.go`, `internal/freshness/reaper_markers_test.go`
- Modify: `internal/cache/types.go` (`URLPath.LastUse`, `SuiteWithAdoption`'s doc), `internal/cache/single_insert_test.go` (`TestSignedSeenHasKnownDeleteSites`), `internal/freshness/unsigned.go` (`anchorLastUse` removed), `internal/handler/suite_rows.go` (`lastUse` removed; mechanical), `internal/freshness/reaper.go` (`markerTTL`, `dailyEvery`, `dailyAt`, `reportedMarker`, `sweepMarkers`, `runOnce`)

**Interfaces:**
- Consumes: `cache.SignedSeenMark`, `MarkSignedSeenBatch`, `SignedSeen`, `futureStampSlack` (Task 2), `submitWrite`; `(*Checker).clockStep`, the field `now`; Task 6's `Reaper`, `runOnce`, `deferredByForwardStep`, `warn`, `reaperRig` (`seed`, `exists`, `events`), `reapNow`, `reapDay`; `renderedMetric` (`scheduler_test.go`); the retry rig (`newRetryRig`, `release`, `up`, `ver.reject`, `check`, `clock`), `contentKey`, `adoptionFormInline`; cache test helpers `openCache`, `seedRow`, `sk`, `productionGoFiles`, `stringLiterals`, `deleteSignedSeenRE`.
- Produces: `(*cache.URLPath).LastUse() int64`; `cache.MarkerSweep{Swept int; Furthest *SignedSeenMark}`, `(*cache.Cache).SweepOrphanMarkers(ctx, now, cutoff int64) (MarkerSweep, error)`, the unexported `orphanMarkers`, `deleteOrphanMarkers`, `orphanSuiteRowSQL`, `markerSweepBatch`; the INFO event `signed_seen_markers_swept` (`count`); `clock_step_detected` with `stamp=last_seen_at`; unexported `markerTTL`, `dailyEvery`, the `Reaper` fields `dailyAt time.Time` (tests zero it to force the sweep) and `reportedMarker cache.SignedSeenMark`, `(*Reaper).sweepMarkers(ctx, now time.Time)`; test helpers `putMarks`, `markerStamp`, `sweepNow`, `sweepYear` (`marker_sweep_test.go`), `(*reaperRig).markerAt(base) int64`, `(*reaperRig).mark(base string, at int64)`.

- [ ] **Step 1: Write the failing tests**

Create `internal/cache/marker_sweep_test.go`:

```go
package cache

import (
	"context"
	"fmt"
	"testing"
)

const (
	sweepNow  = int64(1_800_000_000)
	sweepYear = int64(365 * 86400)
)

// putMarks writes markers, each with its own stamp.
func putMarks(t *testing.T, c *Cache, marks []SignedSeenMark) {
	t.Helper()
	if err := c.MarkSignedSeenBatch(context.Background(), marks); err != nil {
		t.Fatal(err)
	}
}

// markerStamp returns m's marker stamp, or -1 when it has none.
func markerStamp(t *testing.T, c *Cache, m SignedSeenMark) int64 {
	t.Helper()
	at, ok, err := c.SignedSeen(context.Background(), m.CanonicalScheme, m.CanonicalHost, m.Base)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return -1
	}
	return at
}

// Deferred minor E-18: URLPath.LastUse is the one Go copy of a marker
// stamp's source — the newer of last_requested_at and last_fetched_at, 0
// when neither is set.
func TestURLPath_LastUse(t *testing.T) {
	at := func(v int64) *int64 { return &v }
	for _, tc := range []struct {
		requested, fetched *int64
		want               int64
	}{
		{nil, nil, 0}, {at(5), nil, 5}, {nil, at(7), 7}, {at(5), at(7), 7}, {at(9), at(7), 9},
	} {
		if got := (&URLPath{LastRequestedAt: tc.requested, LastFetchedAt: tc.fetched}).LastUse(); got != tc.want {
			t.Errorf("LastUse(requested %v, fetched %v) = %d, want %d", tc.requested != nil, tc.fetched != nil, got, tc.want)
		}
	}
}

// Spec C9 (ruling R-F5): the sweep deletes a marker last seen before the
// cutoff whose base no suite row of its host has — not the base, not its
// "/." twin, under neither scheme — and keeps every other: a younger
// orphan, one at the cutoff, and the markers a suite row of the same host
// keeps under either scheme, through the base or its twin.
func TestSweepOrphanMarkers_SweepsOnlyYearOldOrphans(t *testing.T) {
	c := openCache(t)
	cutoff := sweepNow - sweepYear
	seedRow(t, c, sk("h", "/kept/dists/s"), sweepNow)
	seedRow(t, c, SuiteKey{CanonicalScheme: "https", CanonicalHost: "h", SuitePath: "/other/dists/s"}, sweepNow)
	seedRow(t, c, sk("h", "/flat/."), sweepNow)
	swept := []SignedSeenMark{
		{"http", "h", "/gone/dists/s", cutoff - 1},
		{"https", "h", "/gone/dists/s", cutoff - 1},
		{"http", "h2", "/kept/dists/s", cutoff - 1}, // another host's suite does not keep it
	}
	kept := []SignedSeenMark{
		{"http", "h", "/young/dists/s", cutoff + 1},
		{"http", "h", "/edge/dists/s", cutoff},
		{"http", "h", "/kept/dists/s", cutoff - 1},
		{"http", "h", "/other/dists/s", cutoff - 1}, // its suite's row is under https
		{"http", "h", "/flat", cutoff - 1},          // its /. twin has a row
	}
	putMarks(t, c, append(append([]SignedSeenMark{}, swept...), kept...))
	res, err := c.SweepOrphanMarkers(context.Background(), sweepNow, cutoff)
	if err != nil || res.Swept != len(swept) || res.Furthest != nil {
		t.Fatalf("SweepOrphanMarkers = %+v, %v; want %d swept, no future stamp", res, err, len(swept))
	}
	for _, m := range swept {
		if markerStamp(t, c, m) != -1 {
			t.Errorf("%+v was not swept", m)
		}
	}
	for _, m := range kept {
		if markerStamp(t, c, m) != m.SeenAt {
			t.Errorf("%+v was swept or changed", m)
		}
	}
}

// Ruling PF-1 (it overrides R-F5's "healed"): the sweep never rewrites a
// stamp. A marker stamped more than 2 s past now reads as now — never an
// orphan, its stamp left as it is — and the furthest is reported; one
// within the 2 s slack is no clock step. A future stamp ages out only
// once the clock has passed it by the TTL.
func TestSweepOrphanMarkers_NeverRewritesAFutureStamp(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	slack := SignedSeenMark{"http", "h", "/slack/dists/s", sweepNow + 2}
	putMarks(t, c, []SignedSeenMark{slack})
	if res, err := c.SweepOrphanMarkers(ctx, sweepNow, sweepNow-sweepYear); err != nil || res.Swept != 0 || res.Furthest != nil {
		t.Fatalf("a stamp within the slack: %+v, %v; want nothing swept, no future stamp", res, err)
	}
	far := SignedSeenMark{"http", "h", "/far/dists/s", sweepNow + 5*sweepYear}
	near := SignedSeenMark{"http", "h", "/near/dists/s", sweepNow + 10}
	putMarks(t, c, []SignedSeenMark{far, near})
	res, err := c.SweepOrphanMarkers(ctx, sweepNow, sweepNow-sweepYear)
	if err != nil || res.Swept != 0 || res.Furthest == nil || *res.Furthest != far {
		t.Fatalf("SweepOrphanMarkers = %+v (furthest %+v), %v; want nothing swept, the furthest %+v", res, res.Furthest, err, far)
	}
	for _, m := range []SignedSeenMark{far, near, slack} {
		if got := markerStamp(t, c, m); got != m.SeenAt {
			t.Fatalf("%s's stamp is %d, want %d unchanged", m.Base, got, m.SeenAt)
		}
	}
	later := near.SeenAt + sweepYear + 1 // a year after near: near and slack are year-old orphans
	if res, err := c.SweepOrphanMarkers(ctx, later, later-sweepYear); err != nil || res.Swept != 2 || res.Furthest == nil ||
		*res.Furthest != far {
		t.Fatalf("a year later: %+v, %v; want near and slack swept, far kept and reported", res, err)
	}
	last := far.SeenAt + sweepYear + 1
	if res, err := c.SweepOrphanMarkers(ctx, last, last-sweepYear); err != nil || res.Swept != 1 || res.Furthest != nil {
		t.Fatalf("a year after its stamp: %+v, %v; want far swept", res, err)
	}
}

// Design review I-1, the hazard ruling PF-1 closes: a reaped suite's base
// marker, an orphan last seen a month ago, survives a two-year backward
// clock step and its correction. Read as now during the step (never
// rewritten), it is a month old again once the clock is right.
func TestSweepOrphanMarkers_BackwardStepThenCorrectionSweepsNothing(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	orphan := SignedSeenMark{"http", "h", "/reaped/dists/s", sweepNow - 31*86400}
	putMarks(t, c, []SignedSeenMark{orphan})
	stepped := sweepNow - 2*sweepYear // the clock set back two years
	if res, err := c.SweepOrphanMarkers(ctx, stepped, stepped-sweepYear); err != nil || res.Swept != 0 ||
		res.Furthest == nil || *res.Furthest != orphan {
		t.Fatalf("during the step: %+v, %v; want nothing swept, the marker reported", res, err)
	}
	corrected := sweepNow + 86400
	if res, err := c.SweepOrphanMarkers(ctx, corrected, corrected-sweepYear); err != nil || res.Swept != 0 {
		t.Fatalf("after the correction: %+v, %v; want nothing swept", res, err)
	}
	if got := markerStamp(t, c, orphan); got != orphan.SeenAt {
		t.Fatalf("the marker's stamp is %d, want %d: never-downgrade protection lost", got, orphan.SeenAt)
	}
}

// Ruling R-F5: the delete re-checks in its own writer transaction both
// that a marker is still an orphan and that it is still a year old. A
// marker whose suite came back, or that was sighted again, between the
// sweep's listing and its delete is kept — the never-downgrade authority
// is not stripped from a suite that is tracked or seen signed again.
func TestDeleteOrphanMarkers_RechecksInTheWriter(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	cutoff := sweepNow - sweepYear
	back := SignedSeenMark{"http", "h", "/back/dists/s", cutoff - 1}
	seen := SignedSeenMark{"http", "h", "/seen/dists/s", cutoff - 1}
	putMarks(t, c, []SignedSeenMark{back, seen})
	keys, err := c.orphanMarkers(ctx, cutoff)
	if err != nil || len(keys) != 2 {
		t.Fatalf("orphanMarkers = %v, %v; want both", keys, err)
	}
	seedRow(t, c, sk("h", "/back/dists/s"), sweepNow)                          // the suite came back
	putMarks(t, c, []SignedSeenMark{{"http", "h", "/seen/dists/s", sweepNow}}) // sighted again
	if n, err := c.deleteOrphanMarkers(ctx, keys, cutoff); err != nil || n != 0 {
		t.Fatalf("deleteOrphanMarkers = %d, %v; want nothing deleted", n, err)
	}
	if markerStamp(t, c, back) != back.SeenAt || markerStamp(t, c, seen) != sweepNow {
		t.Fatal("a re-checked marker was deleted or changed")
	}
}

// Spec C9's bounded writer transactions: the sweep deletes at most 1,000
// markers per writer transaction. A failure in the second batch leaves the
// first deleted and reports it; the next sweep deletes the rest.
func TestSweepOrphanMarkers_DeletesInBatchesOfAThousand(t *testing.T) {
	c := openCache(t)
	var marks []SignedSeenMark
	for i := range 1200 {
		marks = append(marks, SignedSeenMark{"http", "h", fmt.Sprintf("/m%04d/dists/s", i), 1})
	}
	putMarks(t, c, marks)
	if _, err := c.db.Exec(`CREATE TRIGGER fail_m1100 BEFORE DELETE ON suite_signed_seen
WHEN OLD.base = '/m1100/dists/s' BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	res, err := c.SweepOrphanMarkers(context.Background(), sweepNow, sweepNow-sweepYear)
	if err == nil || res.Swept != 1000 {
		t.Fatalf("first sweep = %+v, %v; want 1000 swept, then the second batch's error", res, err)
	}
	if markerStamp(t, c, marks[999]) != -1 || markerStamp(t, c, marks[1000]) != 1 {
		t.Fatal("the first batch is not the first 1,000 markers in key order")
	}
	if _, err := c.db.Exec(`DROP TRIGGER fail_m1100`); err != nil {
		t.Fatal(err)
	}
	if res, err := c.SweepOrphanMarkers(context.Background(), sweepNow, sweepNow-sweepYear); err != nil || res.Swept != 200 {
		t.Fatalf("second sweep = %+v, %v; want the remaining 200", res, err)
	}
}
```

Create `internal/freshness/reaper_markers_test.go`:

```go
package freshness

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// markerAt returns base's marker stamp on host h over http, or -1.
func (r *reaperRig) markerAt(base string) int64 {
	r.t.Helper()
	at, ok, err := r.cache.SignedSeen(context.Background(), "http", "h", base)
	if err != nil {
		r.t.Fatal(err)
	}
	if !ok {
		return -1
	}
	return at
}

// mark writes base's marker on host h over http, stamped at.
func (r *reaperRig) mark(base string, at int64) {
	r.t.Helper()
	if err := r.cache.MarkSignedSeenBatch(context.Background(), []cache.SignedSeenMark{{CanonicalScheme: "http",
		CanonicalHost: "h", Base: base, SeenAt: at}}); err != nil {
		r.t.Fatal(err)
	}
}

// Spec C9 (ruling R-F5): a run sweeps the orphan markers last seen a year
// ago, while a reaped suite's marker survives the reap. A marker stamped
// in the future reads as now: it is kept as it is — never rewritten
// (ruling PF-1) — and reported as clock_step_detected (stamp
// last_seen_at). The sweep runs with gc.suite_idle_ttl = 0s too.
func TestReaper_SweepsOrphanMarkersAndKeepsFutureStamps(t *testing.T) {
	ctx := context.Background()
	r := newReaperRig(t)
	r.seed("/idle/dists/s", reapNow-31*reapDay)
	r.mark("/idle/dists/s", reapNow-31*reapDay)
	r.mark("/gone/dists/s", reapNow-366*reapDay)
	r.mark("/ahead/dists/s", reapNow+30*reapDay)
	r.reaper.runOnce(ctx)
	// The reap stamps the base's markers MAX(last_seen_at, the cache's
	// clock) (design review M-9): kept, and never lowered.
	if r.exists("/idle/dists/s") || r.markerAt("/idle/dists/s") < reapNow-31*reapDay {
		t.Fatalf("idle suite exists %v, its marker %d; want the suite reaped and its marker kept (at least %d)",
			r.exists("/idle/dists/s"), r.markerAt("/idle/dists/s"), reapNow-31*reapDay)
	}
	if r.markerAt("/gone/dists/s") != -1 || r.markerAt("/ahead/dists/s") != reapNow+30*reapDay {
		t.Fatalf("year-old orphan %d, future marker %d; want the orphan swept and the future stamp kept as it was (%d)",
			r.markerAt("/gone/dists/s"), r.markerAt("/ahead/dists/s"), reapNow+30*reapDay)
	}
	if ev := r.events("signed_seen_markers_swept"); len(ev) != 1 || ev[0]["count"] != float64(1) {
		t.Fatalf("signed_seen_markers_swept lines %v, want one with count 1", ev)
	}
	steps := r.events("clock_step_detected")
	if len(steps) != 1 || steps[0]["stamp"] != "last_seen_at" || steps[0]["suite_path"] != "/ahead/dists/s" ||
		steps[0]["stored_unixtime"] != float64(reapNow+30*reapDay) {
		t.Fatalf("clock_step_detected lines %v, want one for the future marker", steps)
	}
	r.reaper.idleTTL = 0
	r.mark("/old/dists/s", reapNow-400*reapDay)
	r.reaper.dailyAt = time.Time{} // the next day's run: the sweep is daily
	r.reaper.runOnce(ctx)
	if r.markerAt("/old/dists/s") != -1 {
		t.Fatal("gc.suite_idle_ttl = 0s stopped the marker sweep")
	}
}

// Spec C9: the orphan-marker TTL is 365 days — an orphan seen 364 days
// ago is kept, one seen 366 days ago is swept.
func TestReaper_TheMarkerTTLIsAYear(t *testing.T) {
	r := newReaperRig(t)
	r.mark("/kept/dists/s", reapNow-364*reapDay)
	r.mark("/gone/dists/s", reapNow-366*reapDay)
	r.reaper.runOnce(context.Background())
	if r.markerAt("/kept/dists/s") == -1 || r.markerAt("/gone/dists/s") != -1 {
		t.Fatalf("seen 364 days ago: %d; 366 days ago: %d; want kept and swept",
			r.markerAt("/kept/dists/s"), r.markerAt("/gone/dists/s"))
	}
}

// Design note 20 (ruling PF-4): the marker sweep is the run's daily duty
// — at the first run after a start, then a day apart — while the hourly
// runs reap: an orphan a year old is swept at the first run; another
// found an hour later waits for the run a day after the first.
func TestReaper_DailyDutiesRunAtStartAndADayApart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		r := newReaperRig(t)
		r.mark("/first/dists/s", reapNow-366*reapDay)
		r.reaper.runOnce(ctx)
		if r.markerAt("/first/dists/s") != -1 {
			t.Fatal("the first run after a start did not sweep")
		}
		r.mark("/later/dists/s", reapNow-366*reapDay)
		time.Sleep(time.Hour)
		r.reaper.runOnce(ctx)
		if r.markerAt("/later/dists/s") == -1 {
			t.Fatal("an hourly run swept: the sweep is a daily duty")
		}
		time.Sleep(23 * time.Hour)
		r.reaper.runOnce(ctx)
		if r.markerAt("/later/dists/s") != -1 {
			t.Fatal("the run a day after the first did not sweep")
		}
	})
}

// Design review M-10: a marker stamped in the future is reported at the
// first sweep after a start and again only when the furthest such marker
// changes (here a later sighting moves its stamp on), not at every daily
// sweep until the clock passes it. Every report counts in
// acu_clock_steps_detected_total.
func TestReaper_ReportsAFutureMarkerOnlyWhenItChanges(t *testing.T) {
	ctx := context.Background()
	r := newReaperRig(t)
	reports := func() float64 {
		v, _ := strconv.ParseFloat(renderedMetric("acu_clock_steps_detected_total"), 64)
		return v
	}
	r.mark("/ahead/dists/s", reapNow+30*reapDay)
	before := reports()
	for range 3 {
		r.reaper.dailyAt = time.Time{} // a day apart
		r.reaper.runOnce(ctx)
	}
	if got := reports() - before; got != 1 {
		t.Fatalf("three sweeps over one future marker: %v reports, want 1", got)
	}
	r.mark("/ahead/dists/s", reapNow+31*reapDay)
	r.reaper.dailyAt = time.Time{}
	r.reaper.runOnce(ctx)
	if got := reports() - before; got != 2 {
		t.Fatalf("after its stamp moved on: %v reports, want 2", got)
	}
}

// Design review M-8: the forward-step guard defers the marker sweep too.
// A daily sweep whose run sees the wall clock jump ten days — enough to
// make an orphan five days short of the TTL look a year old — does not
// sweep; the sweep stays due and runs an uptime floor later.
func TestReaper_AForwardStepDefersTheMarkerSweep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newReaperRig(t)
		var step atomic.Int64
		start := time.Now()
		r.checker.now = func() time.Time { return time.Unix(reapNow+step.Load(), 0).Add(time.Since(start)) }
		r.mark("/orphan/dists/s", reapNow-360*reapDay)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { r.reaper.Run(ctx); close(done) }()
		defer func() { cancel(); <-done }()
		time.Sleep(time.Hour + time.Second) // the first run, at the floor, sweeps: 360 days is not a year
		if r.markerAt("/orphan/dists/s") == -1 {
			t.Fatal("swept before the step")
		}
		time.Sleep(23 * time.Hour) // through the hourly runs to just before the next daily one
		step.Store(10 * reapDay)   // the wall clock jumps ten days
		time.Sleep(time.Hour)
		if r.markerAt("/orphan/dists/s") == -1 {
			t.Fatal("swept by the run that saw the forward step")
		}
		time.Sleep(time.Hour)
		if r.markerAt("/orphan/dists/s") != -1 {
			t.Fatal("not swept an uptime floor after the step")
		}
	})
}

// Deferred minor E-20 (phase E design note 30): the checker's marker
// writer writes at every probe that sights a signature, so the marker's
// last_seen_at — what the orphan sweep ages — is the last sighting.
func TestCheck_EverySightingAdvancesTheMarkerStamp(t *testing.T) {
	ctx := context.Background()
	r := newRetryRig(t, 0)
	in := r.release("v1", 0)
	r.up.set(in)
	r.ver.reject(contentKey(adoptionFormInline, in, nil, nil)) // stays unadopted: each probe sights it
	t0 := r.clock.now().Unix()
	r.check()
	if at, ok, err := r.cache.SignedSeen(ctx, "http", rigHost, rigSuite); err != nil || !ok || at != t0 {
		t.Fatalf("first sighting: last_seen_at %d (%v, %v), want %d", at, ok, err, t0)
	}
	r.clock.advance(time.Hour)
	r.check()
	if at, _, err := r.cache.SignedSeen(ctx, "http", rigHost, rigSuite); err != nil || at != t0+3600 {
		t.Fatalf("second sighting: last_seen_at %d (%v), want %d", at, err, t0+3600)
	}
}
```

In `internal/cache/single_insert_test.go`, `TestSignedSeenHasKnownDeleteSites`' doc becomes

```go
// Design review M-5: the never-downgrade authority (spec C5) is deleted
// only by POST /forget (ForgetSuite), the year-old orphan sweep
// (deleteOrphanMarkers, ruling R-F5) and the test-only
// ClearSignedSeenForTest, so a new path that deletes signed-seen markers
// fails here.
```

and its `want` list gains the sweep:

```go
	want := []string{"../../internal/cache/marker_sweep.go:deleteOrphanMarkers", "../../internal/cache/suite_reap.go:ForgetSuite",
		"../../internal/cache/testing_helpers.go:ClearSignedSeenForTest"}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -gcflags=-e ./internal/cache/ ./internal/freshness/ -run 'TestSweepOrphanMarkers|TestDeleteOrphanMarkers|TestURLPath_LastUse|TestSignedSeenHasKnownDeleteSites|TestReaper_SweepsOrphanMarkers|TestReaper_TheMarkerTTL|TestReaper_DailyDuties|TestReaper_ReportsAFutureMarker|TestReaper_AForwardStepDefersTheMarkerSweep|TestCheck_EverySightingAdvancesTheMarkerStamp'`
Expected: cache: compile failures `(&URLPath{…}).LastUse undefined`, `c.SweepOrphanMarkers undefined`, `c.orphanMarkers undefined`, `c.deleteOrphanMarkers undefined`; freshness: compile failures `r.reaper.dailyAt undefined`. (Once `dailyAt` exists, `TestCheck_EverySightingAdvancesTheMarkerStamp` passes alone: it pins phase E's writer, deferred minor E-20.)

- [ ] **Step 3: Implement**

In `internal/cache/types.go`, after `URLPath`:

```go
// LastUse is the row's last use: the newer of its last_requested_at and
// last_fetched_at (0 when neither is set) — the stamp a signed-seen marker
// from the row carries (the handler's markers and backstop, the adopter's
// re-mark; ListTrackedSuiteAnchors computes the same in SQL for the
// startup marker pass). The orphan-marker sweep reads it back.
func (u *URLPath) LastUse() int64 {
	var seen int64
	if u.LastRequestedAt != nil {
		seen = *u.LastRequestedAt
	}
	if u.LastFetchedAt != nil && *u.LastFetchedAt > seen {
		seen = *u.LastFetchedAt
	}
	return seen
}
```

and `SuiteWithAdoption`'s doc ends "Returned by ListSuitesWithAdoption (SPEC5 §9.7.8) for the status-page render, the scheduler's tick and the suite reaper (deferred minor E-40)." Delete `anchorLastUse` (`internal/freshness/unsigned.go`) and `lastUse` (`internal/handler/suite_rows.go`); their three calls become `row.LastUse()`.

Create `internal/cache/marker_sweep.go`:

```go
package cache

// Spec C9: the orphan signed-seen marker sweep (rulings R-F5, PF-1).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// markerSweepBatch bounds the markers one writer transaction of the sweep
// deletes.
const markerSweepBatch = 1000

// MarkerSweep is one SweepOrphanMarkers call's result.
type MarkerSweep struct {
	// Swept is how many orphan markers were deleted.
	Swept int
	// Furthest is the marker stamped furthest past now (more than
	// futureStampSlack: a clock step), with that stamp in SeenAt; nil when
	// none is. The sweep leaves its stamp as it is.
	Furthest *SignedSeenMark
}

// SweepOrphanMarkers is spec C9's orphan-marker sweep (ruling R-F5). It
// reads the marker stamped furthest past now + futureStampSlack, if any
// (Furthest; one read on the pool), then deletes, in writer transactions
// of at most markerSweepBatch, the markers last seen before cutoff whose
// base no suite row of their host has any more — not the base itself, not
// its "/." twin, under neither scheme — each delete re-checking that, and
// the stamp, in its transaction. On an error, Swept counts the batches
// already committed.
//
// AIDEV-NOTE: ruling PF-1 (design review I-1; it overrides R-F5's
// "healed"): the sweep never rewrites a stamp. A future stamp reads as
// now: it is never an orphan candidate (last_seen_at < cutoff excludes
// it) and it is reported (Furthest). The sweep cannot tell a stamp written
// while the clock was ahead from a clock that is behind now, and lowering
// the stamp in the second case deletes signed history once the clock is
// corrected (TestSweepOrphanMarkers_BackwardStepThenCorrectionSweepsNothing),
// so a future stamp ages out only once the clock passes it; markSignedSeen's
// MAX never lowers one either. Never-downgrade reads a base's markers under
// either scheme (spec C5, design review M-7), so a marker is an orphan only
// while no suite of its host, under either scheme, is its base or the
// base's twin: a live suite's marker is never swept, so no renewal write is
// needed (spec C3).
func (c *Cache) SweepOrphanMarkers(ctx context.Context, now, cutoff int64) (MarkerSweep, error) {
	var res MarkerSweep
	var m SignedSeenMark
	err := c.db.QueryRowContext(ctx, `
SELECT canonical_scheme, canonical_host, base, last_seen_at FROM suite_signed_seen
 WHERE last_seen_at > ?
 ORDER BY last_seen_at DESC LIMIT 1`, now+futureStampSlack).Scan(&m.CanonicalScheme, &m.CanonicalHost, &m.Base, &m.SeenAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return res, fmt.Errorf("SweepOrphanMarkers: read future stamps: %w", err)
	default:
		res.Furthest = &m
	}
	for {
		keys, err := c.orphanMarkers(ctx, cutoff)
		if err != nil {
			return res, err
		}
		if len(keys) == 0 {
			return res, nil
		}
		n, err := c.deleteOrphanMarkers(ctx, keys, cutoff)
		res.Swept += n
		if err != nil {
			return res, err
		}
		if n == 0 || len(keys) < markerSweepBatch {
			return res, nil
		}
	}
}

// orphanSuiteRowSQL is the orphan test, given a marker's host and base
// (twice: the base and its twin): a suite row of that host, under either
// scheme, at the base or at the base's "/." twin.
const orphanSuiteRowSQL = `
SELECT 1 FROM suite_freshness sf
 WHERE sf.canonical_scheme IN ('http', 'https') AND sf.canonical_host = %s
   AND sf.suite_path IN (%s, %s || '/.')`

// orphanMarkers lists up to markerSweepBatch orphan markers last seen
// before cutoff, in key order (one read on the pool).
func (c *Cache) orphanMarkers(ctx context.Context, cutoff int64) ([]SignedSeenMark, error) {
	rows, err := c.db.QueryContext(ctx, `
SELECT m.canonical_scheme, m.canonical_host, m.base FROM suite_signed_seen m
 WHERE m.last_seen_at < ?
   AND NOT EXISTS (`+fmt.Sprintf(orphanSuiteRowSQL, "m.canonical_host", "m.base", "m.base")+`)
 ORDER BY m.canonical_scheme, m.canonical_host, m.base
 LIMIT ?`, cutoff, markerSweepBatch)
	if err != nil {
		return nil, fmt.Errorf("SweepOrphanMarkers: list orphans: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SignedSeenMark
	for rows.Next() {
		var m SignedSeenMark
		if err := rows.Scan(&m.CanonicalScheme, &m.CanonicalHost, &m.Base); err != nil {
			return nil, fmt.Errorf("SweepOrphanMarkers: scan: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("SweepOrphanMarkers: list orphans: %w", err)
	}
	return out, nil
}

// deleteOrphanMarkers deletes keys in one writer transaction, each only if
// it is still an orphan last seen before cutoff, and returns how many it
// deleted.
func (c *Cache) deleteOrphanMarkers(ctx context.Context, keys []SignedSeenMark, cutoff int64) (int, error) {
	n := 0
	err := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("SweepOrphanMarkers: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		deleted := 0
		for _, k := range keys {
			r, err := tx.ExecContext(ctx, `
DELETE FROM suite_signed_seen
 WHERE canonical_scheme = ? AND canonical_host = ? AND base = ? AND last_seen_at < ?
   AND NOT EXISTS (`+fmt.Sprintf(orphanSuiteRowSQL, "?", "?", "?")+`)`,
				k.CanonicalScheme, k.CanonicalHost, k.Base, cutoff, k.CanonicalHost, k.Base, k.Base)
			if err != nil {
				return fmt.Errorf("SweepOrphanMarkers: delete %s://%s%s: %w", k.CanonicalScheme, k.CanonicalHost, k.Base, err)
			}
			m, err := r.RowsAffected()
			if err != nil {
				return fmt.Errorf("SweepOrphanMarkers: delete: %w", err)
			}
			deleted += int(m)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("SweepOrphanMarkers: commit: %w", err)
		}
		n = deleted
		return nil
	})
	if err != nil {
		return 0, err // n is the writer's until the op returns
	}
	return n, nil
}
```

In `internal/freshness/reaper.go`, the constant block gains, after `junkGrace`:

```go
	// markerTTL is spec C9's orphan-marker TTL: a signed-seen marker no
	// suite row of its host has for its base or the base's twin is swept
	// once it was last seen this long ago.
	markerTTL = 365 * 24 * time.Hour
	// dailyEvery is how often a run also sweeps the orphan markers (design
	// note 20, ruling PF-4): at the first run after a start, then a day
	// apart (monotonic).
	dailyEvery = 24 * time.Hour
```

`Reaper` gains, after `sniffs`:

```go
	// dailyAt is when the marker sweep last ran (monotonic; zero: due);
	// reportedMarker is the marker stamped furthest in the future that the
	// previous sweep found (design review M-10). Only the run's goroutine
	// touches them; tests zero dailyAt to force a sweep.
	dailyAt        time.Time
	reportedMarker cache.SignedSeenMark
```

`runOnce` becomes:

```go
// runOnce is one reaper run (spec C9): unless a forward step of the wall
// clock defers it, reap at most reapsPerRun of the candidates — idle,
// non-canonical and legacy junk (junkCandidates, every run: ruling PF-4)
// — those reaped regardless of idleness first, then the oldest, and,
// when a day has passed since it last ran, sweep the orphan signed-seen
// markers (sweepMarkers); then sweep the reconcile memo against the
// current snapshots.
//
// AIDEV-NOTE: design note 20, ruling PF-4. Only the marker sweep is
// daily: it works in years (its 365-day TTL) and costs a writer
// transaction per batch. A run the forward-step guard defers neither
// reaps nor sweeps, and leaves the sweep due for the next run (design
// review M-8): a step makes markers look older too.
func (r *Reaper) runOnce(ctx context.Context) {
	rows, err := r.cache.ListSuitesWithAdoption(ctx)
	if err != nil {
		r.warn(ctx, "suite reaper: list suites failed", "err", err)
		return
	}
	now := r.checker.now()
	deferred := r.deferredByForwardStep(now, time.Now())
	if !deferred && r.idleTTL > 0 {
		cands := r.candidates(rows, now)
		cands = append(cands, r.junkCandidates(ctx, rows, cands, now)...)
		sortCandidates(cands)
		for _, c := range cands[:min(len(cands), reapsPerRun)] {
			if ctx.Err() != nil {
				return
			}
			r.reapSuite(ctx, c)
		}
	}
	if !deferred && (r.dailyAt.IsZero() || time.Since(r.dailyAt) >= dailyEvery) {
		r.dailyAt = time.Now()
		r.sweepMarkers(ctx, now)
	}
	r.sweepReconcileMemo(rows)
}
```

and after it:

```go
// sweepMarkers runs spec C9's orphan-marker sweep (ruling R-F5,
// cache.SweepOrphanMarkers) with the cutoff now − markerTTL: it deletes
// the orphans last seen before the cutoff, logging
// signed_seen_markers_swept when it deleted any, and reports the marker
// stamped furthest in the future through the rate-limited
// clock_step_detected (stamp=last_seen_at) at the first sweep after a
// start and whenever that marker or its stamp changes (design review
// M-10). It never rewrites a stamp (ruling PF-1).
func (r *Reaper) sweepMarkers(ctx context.Context, now time.Time) {
	res, err := r.cache.SweepOrphanMarkers(ctx, now.Unix(), now.Add(-markerTTL).Unix())
	var furthest cache.SignedSeenMark
	if res.Furthest != nil {
		furthest = *res.Furthest
		if furthest != r.reportedMarker {
			r.checker.clockStep("last_seen_at", furthest.CanonicalScheme, furthest.CanonicalHost, furthest.Base,
				furthest.SeenAt, now)
		}
	}
	r.reportedMarker = furthest
	if res.Swept > 0 {
		r.logger.Info("signed_seen_markers_swept", "count", res.Swept)
	}
	if err != nil {
		r.warn(ctx, "suite reaper: marker sweep failed", "err", err, "swept", res.Swept)
	}
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/cache/ ./internal/freshness/ ./internal/handler/`
Expected: PASS (`TestSweepOrphanMarkers_DeletesInBatchesOfAThousand` takes about 2.5 s under `-race`).

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| the orphan test without the twin (`IN (%s, %s)`) | `TestSweepOrphanMarkers_SweepsOnlyYearOldOrphans` (`{Swept:4 …}`) |
| the orphan test under one scheme (`IN ('http')`) | `TestSweepOrphanMarkers_SweepsOnlyYearOldOrphans` |
| the cutoff inclusive (both `last_seen_at < ?` → `<=`) | `TestSweepOrphanMarkers_SweepsOnlyYearOldOrphans` (`{Swept:4 …}`: the edge marker) |
| PF-1: the sweep sets future stamps to now (the draft's heal `UPDATE` put back) | `TestSweepOrphanMarkers_NeverRewritesAFutureStamp` (`/far/dists/s's stamp is 1800000000, want 1957680000 unchanged`), `TestSweepOrphanMarkers_BackwardStepThenCorrectionSweepsNothing` (`after the correction: {Swept:1 …}`), `TestReaper_SweepsOrphanMarkersAndKeepsFutureStamps` |
| `Furthest` without the slack (`now+futureStampSlack` → `now`) | `TestSweepOrphanMarkers_NeverRewritesAFutureStamp` (`a stamp within the slack: {Swept:0 Furthest:0x…}`) |
| `Furthest` reports the nearest (`ORDER BY last_seen_at ASC`) | `TestSweepOrphanMarkers_NeverRewritesAFutureStamp` (`… Base:/near/dists/s …`) |
| `markerSweepBatch = 1101` | `TestSweepOrphanMarkers_DeletesInBatchesOfAThousand` (`first sweep = {Swept:0 …}`) |
| X16: the delete without its in-transaction orphan re-check | `TestDeleteOrphanMarkers_RechecksInTheWriter` (`deleteOrphanMarkers = 1, <nil>; want nothing deleted`) |
| X17: the delete without its in-transaction stamp re-check (`last_seen_at < ?` → `? > 0`) | `TestDeleteOrphanMarkers_RechecksInTheWriter` (`deleteOrphanMarkers = 1`) |
| `LastUse` ignores `last_fetched_at` | `TestURLPath_LastUse` |
| `sweepMarkers` reports no clock step | `TestReaper_SweepsOrphanMarkersAndKeepsFutureStamps` (`clock_step_detected lines []`), `TestReaper_ReportsAFutureMarkerOnlyWhenItChanges` |
| `markerTTL = 30 * 24 * time.Hour` | `TestReaper_SweepsOrphanMarkersAndKeepsFutureStamps` (`idle suite exists false, its marker -1`), `TestReaper_TheMarkerTTLIsAYear`, `TestReaper_AForwardStepDefersTheMarkerSweep` |
| X31: `markerTTL = 300 * 24 * time.Hour` | `TestReaper_TheMarkerTTLIsAYear` (`seen 364 days ago: -1`), `TestReaper_AForwardStepDefersTheMarkerSweep` |
| X31: `markerTTL = 400 * 24 * time.Hour` | `TestReaper_TheMarkerTTLIsAYear` (`366 days ago: 1768377600`), and three more |
| `runOnce` without `sweepMarkers` | `TestReaper_SweepsOrphanMarkersAndKeepsFutureStamps` and the four other sweep tests |
| the daily gate always open (`if !deferred {`) | `TestReaper_DailyDutiesRunAtStartAndADayApart` (`an hourly run swept`) |
| `dailyEvery = 48 * time.Hour` | `TestReaper_DailyDutiesRunAtStartAndADayApart` (`the run a day after the first did not sweep`), `TestReaper_AForwardStepDefersTheMarkerSweep` |
| `markSignatureSeen` returns early when `SignedSeen` finds a marker (write-once) | `TestCheck_EverySightingAdvancesTheMarkerStamp` (`second sighting: last_seen_at 1800000000 …, want 1800003600`) |
| M-10: a future marker reported at every sweep | `TestReaper_ReportsAFutureMarkerOnlyWhenItChanges` (`three sweeps over one future marker: 3 reports, want 1`) |
| M-10: a future marker reported once ever (`r.reportedMarker == (cache.SignedSeenMark{})`) | `TestReaper_ReportsAFutureMarkerOnlyWhenItChanges` (`after its stamp moved on: 1 reports, want 2`) |
| M-8: the sweep not deferred (`!deferred &&` dropped) | `TestReaper_AForwardStepDefersTheMarkerSweep` (`swept by the run that saw the forward step`) |
| M-8: a deferred sweep uses up the day (`dailyAt` set before the `!deferred` check) | `TestReaper_AForwardStepDefersTheMarkerSweep` (`not swept an uptime floor after the step`) |

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/cache/marker_sweep.go internal/cache/marker_sweep_test.go internal/cache/types.go internal/cache/single_insert_test.go internal/freshness/reaper.go internal/freshness/reaper_markers_test.go internal/freshness/unsigned.go internal/handler/suite_rows.go
git add internal/cache/marker_sweep.go internal/cache/marker_sweep_test.go internal/cache/types.go internal/cache/single_insert_test.go internal/freshness/reaper.go internal/freshness/reaper_markers_test.go internal/freshness/unsigned.go internal/handler/suite_rows.go
git commit -m "Sweep orphan signed-seen markers a year old, never rewriting a stamp (rulings R-F5, PF-1); the daily sweep; deferred minors E-18, E-20, E-40 (types.go)"
```

(Record the mutation table in the commit message.)

### Task 10: `POST /forget` (spec C9, D9)

The admin escape hatch for refusal states (a vendor that stopped signing, a deliberate upstream rollback, a stuck unsigned suite): the reaper's steps for one suite, run to completion regardless of idleness, plus — when no other suite with the base or its twin has a row, under either scheme — the base's markers and its cached anchor rows (`ForgetSuite`, Task 3; design notes 10, 13, 14, ruling PF-2). The freshness side, `(*Reaper).Forget`, follows ruling R-F1's one delete path, bounded by `suiteHoldTimeout` (5 min) and releasing with `ForgetSuite`'s settled `Gone` (design review M-2); `suite_forgotten` logs `anchor_rows` (PF-2). The admin side adds the route, the guard factored into `requireMutationGuard` (ruling PF-6: `X-ACU-Admin: 1`; a `Sec-Fetch-Site` other than `same-origin` or `none` is 403; its AIDEV-NOTE says honestly that it stops a cross-site form or fetch but not DNS rebinding against a listener without `htpasswd_file`, Host-header validation being spec §10 G-1's for `/forget` and `/reconcile` together — `/reconcile` itself is unchanged in F) and the answers (ruling PF-7): 202 with a body that names what was forgotten, the markers deleted and the rows that kept them (`Forgetter.Forget` now returns `cache.ForgetResult`), 409 for busy or unknown (no existence oracle) with a body that says what to check, 400, 403, 405, 413, 500 (logged), 501. `OPTIONS /forget` answers `Allow: POST, OPTIONS`. INFO `admin_forget_requested` (as `/reconcile`'s) and `suite_forgotten`. The status page gains each suite's scheme (PF-7, design review I-7): JSON `scheme` — additive to the locked contract; the JSON-contract golden gains the one key — and the HTML Host cell (`http://host`), since `/forget`'s scheme defaults to `https` and a suite's two schemes rendered as two identical rows. The exec review's surviving X9 gets its killer here (PF-9). The escape-hatch scenarios themselves (re-adoption, the `unsigned_downgrade_refused` case with and without a `/.` twin row, and with a signed anchor cached under the other scheme) are Task 15's.

**Files:**
- Create: `internal/freshness/forget.go`, `internal/freshness/forget_test.go`, `internal/admin/forget_test.go`
- Modify: `internal/admin/server.go` (`Config.Forgetter`, `Forgetter`, the dispatcher), `internal/admin/handlers.go` (`handleForget`, `forgottenText`, `requireMutationGuard`), `internal/admin/status.go` (`suiteEntry.Scheme`), `internal/admin/status_template.go` (the Suites table), `internal/admin/json_contract_test.go` and `testdata/json_contract_golden.json` (the fixture's scheme), `internal/freshness/reaper.go` (`reapHook`'s doc)

**Interfaces:**
- Consumes: `(*cache.Cache).GetSuiteFreshness`, `DeleteSuiteMetadata`, `ForgetSuite`, `cache.ForgetResult` (`Deleted`, `Gone`, `MarkersDeleted`, `AnchorRowsDeleted`, `KeptBy`; Task 3), `TouchedSuites`, `MarkSignedSeenBatch`, `PutURLPath`, `SeedSuiteForTest`; `(*Checker).HoldSuite`, `LockCount`, `lockLoadedHook`; Task 6's `reaperRig`, `reapKey`, `reapNow`, `suiteHoldTimeout`, `reapHook`; Task 9's `(*reaperRig).markerAt`; the admin test helpers `startAdminServer`, `adminOpt`, `mustGet`.
- Produces: `(*freshness.Reaper).Forget(ctx context.Context, scheme, host, suitePath string) (cache.ForgetResult, error)`; `admin.Forgetter` (`Forget(ctx, scheme, host, suitePath string) (cache.ForgetResult, error)`), `admin.Config.Forgetter`; unexported `forgottenText`, `requireMutationGuard(w, r) bool`; the route `POST /forget`; the status JSON key `suites[].scheme`; the events `admin_forget_requested` (`scheme`, `canonical_host`, `suite_path`) and `suite_forgotten` (`canonical_scheme`, `canonical_host`, `suite_path`, `metadata_rows`, `markers_deleted`, `anchor_rows`); WARN `admin: forget failed`; test helpers `stubForgetter`, `forgotten(markers, keptBy...)`, `withForgetter`, `forget(t, base, body, headers...) (status, allow, body)` (`internal/admin/forget_test.go`), `(*reaperRig).cacheOtherSchemeAnchor(suite)` (`internal/freshness/forget_test.go`).

- [ ] **Step 1: Write the failing tests**

Create `internal/freshness/forget_test.go`:

```go
package freshness

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// Spec C9 (D9): Forget deletes a suite regardless of idleness — one a
// client asked for just now, with a current snapshot — through the
// reaper's steps (ruling R-F1): its metadata rows, its snapshots retired,
// its row, its lock-map, flip-gate and touch entries; and, with no other
// suite of the base left, the base's markers under both schemes and its
// cached anchors under the other scheme too (ruling PF-2). It logs
// suite_forgotten.
func TestReaper_ForgetDeletesASuiteRegardlessOfIdleness(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		r := newReaperRig(t)
		r.seed("/r/dists/x", reapNow)
		snap := r.adopt("/r/dists/x", "anchor x")
		if err := r.cache.MarkSignedSeenBatch(ctx, []cache.SignedSeenMark{
			{CanonicalScheme: "http", CanonicalHost: "h", Base: "/r/dists/x", SeenAt: reapNow},
			{CanonicalScheme: "https", CanonicalHost: "h", Base: "/r/dists/x", SeenAt: reapNow},
		}); err != nil {
			t.Fatal(err)
		}
		r.cacheOtherSchemeAnchor("/r/dists/x")
		time.Sleep(r.gate.Quiet()) // the seeded serve is a quiet period old
		res, err := r.reaper.Forget(ctx, "http", "h", "/r/dists/x")
		if err != nil || !res.Deleted || res.MarkersDeleted != 2 || res.AnchorRowsDeleted != 1 || len(res.KeptBy) != 0 {
			t.Fatalf("Forget = %+v, %v; want deleted, 2 markers and 1 anchor row, kept by nothing", res, err)
		}
		if r.exists("/r/dists/x") {
			t.Fatal("the suite row survived")
		}
		if got, err := r.cache.GetSuiteSnapshot(ctx, snap); err != nil || got.RetiredAt == nil {
			t.Fatalf("snapshot %+v, %v; want retired", got, err)
		}
		for _, scheme := range []string{"http", "https"} {
			if _, err := r.cache.LookupURL(ctx, scheme, "h", "/r/dists/x/InRelease"); !errors.Is(err, cache.ErrNotFound) {
				t.Fatalf("%s://h/r/dists/x/InRelease: %v; want the anchor row deleted", scheme, err)
			}
		}
		if r.markerAt("/r/dists/x") != -1 {
			t.Fatal("the base's marker survived")
		}
		if l, g, tm := r.checker.LockCount(), r.gate.Len(), r.cache.TouchedSuites(); l != 0 || g != 0 || tm != 0 {
			t.Fatalf("lock map %d, flip gate %d, touch map %d entries; want none", l, g, tm)
		}
		ev := r.events("suite_forgotten")
		if len(ev) != 1 || ev[0]["metadata_rows"] != float64(1) || ev[0]["markers_deleted"] != float64(2) ||
			ev[0]["anchor_rows"] != float64(1) {
			t.Fatalf("suite_forgotten lines %v, want one with metadata_rows 1, markers_deleted 2, anchor_rows 1", ev)
		}
	})
}

// cacheOtherSchemeAnchor caches suite's InRelease under https: an anchor
// no https suite row owns (ruling PF-2's case).
func (r *reaperRig) cacheOtherSchemeAnchor(suite string) {
	r.t.Helper()
	ctx := context.Background()
	h, err := r.adopter.writeBlobBytes(ctx, []byte("https anchor of "+suite))
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.cache.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "https", CanonicalHost: "h", Path: suite + "/InRelease",
		BlobHash: &h, UpstreamURL: "https://h" + suite + "/InRelease", IsMetadata: true, RequestCount: 1}); err != nil {
		r.t.Fatal(err)
	}
}

// Spec C9: busy and unknown are one answer — not deleted, no error — and
// an unknown suite gets no lock-map entry (I-4). A busy suite keeps its
// row. Markers another suite with the base keeps (here the other scheme's
// suite) survive the forget, and the result names that suite (ruling
// PF-7).
func TestReaper_ForgetAnswersFalseForBusyAndUnknown(t *testing.T) {
	ctx := context.Background()
	r := newReaperRig(t)
	stored := 0
	r.checker.lockLoadedHook = func(string) { stored++ }
	if res, err := r.reaper.Forget(ctx, "http", "h", "/r/dists/none"); res.Deleted || err != nil || stored != 0 {
		t.Fatalf("unknown: %+v, %v, %d lock-map loads; want not deleted, nil, none (I-4: no entry for a suite without a row)",
			res, err, stored)
	}
	r.checker.lockLoadedHook = nil
	r.seed("/r/dists/busy", reapNow)
	release, held := r.checker.HoldSuite(reapKey("/r/dists/busy"))
	if !held {
		t.Fatal("busy")
	}
	res, err := r.reaper.Forget(ctx, "http", "h", "/r/dists/busy")
	release(false)
	if res.Deleted || err != nil || !r.exists("/r/dists/busy") {
		t.Fatalf("busy: %+v, %v, row kept %v; want not deleted, nil, kept", res, err, r.exists("/r/dists/busy"))
	}
	r.seed("/r/dists/both", reapNow)
	if err := r.cache.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "https", CanonicalHost: "h",
		SuitePath: "/r/dists/both"}); err != nil {
		t.Fatal(err)
	}
	if err := r.cache.MarkSignedSeenBatch(ctx, []cache.SignedSeenMark{{CanonicalScheme: "http", CanonicalHost: "h",
		Base: "/r/dists/both", SeenAt: reapNow}}); err != nil {
		t.Fatal(err)
	}
	res, err = r.reaper.Forget(ctx, "http", "h", "/r/dists/both")
	want := cache.SuiteKey{CanonicalScheme: "https", CanonicalHost: "h", SuitePath: "/r/dists/both"}
	if !res.Deleted || err != nil || r.markerAt("/r/dists/both") != reapNow || len(res.KeptBy) != 1 || res.KeptBy[0] != want {
		t.Fatalf("other scheme's suite: %+v, %v, marker %d; want forgotten, the marker kept by %v", res, err,
			r.markerAt("/r/dists/both"), want)
	}
}

// The same as the reaper's design note 5, for POST /forget: the row read
// before the hold finds the suite, the re-read under it finds it gone; the
// entry the hold stored goes (release(true)).
func TestReaper_ForgetDropsTheLockEntryOfASuiteGoneUnderTheHold(t *testing.T) {
	ctx := context.Background()
	r := newReaperRig(t)
	r.seed("/r/dists/x", reapNow) // leaves a lock-map entry
	r.checker.lockLoadedHook = func(string) {
		r.checker.lockLoadedHook = nil
		if res, err := r.cache.ForgetSuite(ctx, reapKey("/r/dists/x"), nil); err != nil || !res.Deleted {
			t.Errorf("ForgetSuite = %+v, %v", res, err)
		}
	}
	if res, err := r.reaper.Forget(ctx, "http", "h", "/r/dists/x"); res.Deleted || err != nil {
		t.Fatalf("Forget = %+v, %v; want not deleted (gone), nil", res, err)
	}
	if n := r.checker.LockCount(); n != 0 {
		t.Fatalf("%d lock entries; want the vanished suite's entry dropped", n)
	}
}

// Design review M-2: Forget's work runs within suiteHoldTimeout (5 min) —
// the admin endpoint hands it a context a disconnect cannot cancel, so a
// wedged writer would otherwise hold the suite and the request for good.
func TestReaper_ForgetBoundsItsWorkAtFiveMinutes(t *testing.T) {
	r := newReaperRig(t)
	r.seed("/r/dists/x", reapNow)
	var deadline, at time.Time
	var ok bool
	r.reaper.reapHook = func(ctx context.Context, _ cache.SuiteKey) { deadline, ok = ctx.Deadline(); at = time.Now() }
	start := time.Now()
	if _, err := r.reaper.Forget(context.WithoutCancel(context.Background()), "http", "h", "/r/dists/x"); err != nil {
		t.Fatal(err)
	}
	if !ok || deadline.Before(start.Add(5*time.Minute)) || deadline.After(at.Add(5*time.Minute)) {
		t.Fatalf("Forget's deadline = %v (set %v), want 5 min after its start", deadline, ok)
	}
}

// A database failure is an error (the endpoint's 500), not a 409.
func TestReaper_ForgetReportsADatabaseError(t *testing.T) {
	r := newReaperRig(t)
	r.seed("/r/dists/x", reapNow)
	if err := r.cache.Close(); err != nil {
		t.Fatal(err)
	}
	if res, err := r.reaper.Forget(context.Background(), "http", "h", "/r/dists/x"); res.Deleted || err == nil {
		t.Fatalf("Forget on a closed cache = %+v, %v; want an error", res, err)
	}
}
```

Create `internal/admin/forget_test.go`:

```go
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// stubForgetter is a test double for the Forgetter interface: it records
// every call and returns a fixed result.
type stubForgetter struct {
	mu                  sync.Mutex
	res                 cache.ForgetResult
	err                 error
	calls               int
	scheme, host, suite string
	detached            bool // the context it got can never be canceled
}

func (s *stubForgetter) Forget(ctx context.Context, scheme, host, suitePath string) (cache.ForgetResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.scheme, s.host, s.suite = scheme, host, suitePath
	s.detached = ctx.Done() == nil
	return s.res, s.err
}

func (s *stubForgetter) snapshot() (calls int, scheme, host, suite string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.scheme, s.host, s.suite
}

// forgotten is a stub result: the suite was forgotten, with markers
// deleted and the rows in keptBy keeping none.
func forgotten(markers int, keptBy ...cache.SuiteKey) cache.ForgetResult {
	return cache.ForgetResult{Deleted: true, Gone: true, MarkersDeleted: markers, KeptBy: keptBy}
}

func withForgetter(f Forgetter) adminOpt { return func(cfg *Config) { cfg.Forgetter = f } }

// forget sends POST /forget with body and the given headers (name, value
// pairs) and returns the status, the Allow header and the response body.
func forget(t *testing.T, base, body string, headers ...string) (int, string, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/forget", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	text, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header.Get("Allow"), string(text)
}

// Spec C9 (D9) and ruling PF-7: POST /forget answers 202 once the suite
// is forgotten, its body naming the suite, the markers deleted and the
// rows that kept them; 409 for busy or unknown, its body saying what to
// check; 500 for a database failure; 400 for a missing parameter or
// another scheme (no call); 501 without a Forgetter; scheme defaults to
// https. GET is 405 and OPTIONS lists POST.
func TestEndpoint_Forget(t *testing.T) {
	admin := []string{"X-ACU-Admin", "1"}
	cases := []struct {
		name   string
		stub   *stubForgetter
		body   string
		status int
		calls  int
		text   string
	}{
		{"forgotten 202", &stubForgetter{res: forgotten(2)}, "host=deb.example&suite=/debian/dists/stable", http.StatusAccepted, 1,
			"forgotten https://deb.example/debian/dists/stable; signed-seen markers deleted: 2"},
		{"forgotten, markers kept 202", &stubForgetter{res: forgotten(0,
			cache.SuiteKey{CanonicalScheme: "https", CanonicalHost: "h", SuitePath: "/flat"},
			cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/flat/."})},
			"scheme=http&host=h&suite=/flat", http.StatusAccepted, 1,
			"forgotten http://h/flat; signed-seen markers deleted: 0; kept by https://h/flat http://h/flat/."},
		{"busy or unknown 409", &stubForgetter{}, "host=h&suite=/s", http.StatusConflict, 1,
			"not forgotten: busy (try again) or no such suite (scheme defaults to https; host and suite as the status page shows them)"},
		{"database error 500", &stubForgetter{err: errors.New("disk I/O error")}, "host=h&suite=/s", http.StatusInternalServerError, 1,
			"forget failed"},
		{"missing suite 400", &stubForgetter{res: forgotten(0)}, "host=h", http.StatusBadRequest, 0,
			"host and suite are required; scheme is http or https"},
		{"another scheme 400", &stubForgetter{res: forgotten(0)}, "scheme=ftp&host=h&suite=/s", http.StatusBadRequest, 0,
			"host and suite are required; scheme is http or https"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, base, cleanup := startAdminServer(t, withForgetter(tc.stub))
			defer cleanup()
			if status, _, text := forget(t, base, tc.body, admin...); status != tc.status || strings.TrimRight(text, "\n") != tc.text {
				t.Errorf("status = %d, body %q; want %d, %q", status, text, tc.status, tc.text)
			}
			if calls, _, _, _ := tc.stub.snapshot(); calls != tc.calls {
				t.Errorf("calls = %d, want %d", calls, tc.calls)
			}
		})
	}
	t.Run("scheme defaults to https, http is kept", func(t *testing.T) {
		stub := &stubForgetter{res: forgotten(0)}
		_, base, cleanup := startAdminServer(t, withForgetter(stub))
		defer cleanup()
		forget(t, base, "host=deb.example&suite=/debian/dists/stable", admin...)
		if _, scheme, host, suite := stub.snapshot(); scheme != "https" || host != "deb.example" || suite != "/debian/dists/stable" {
			t.Errorf("params scheme=%q host=%q suite=%q", scheme, host, suite)
		}
		forget(t, base, "scheme=http&host=h&suite=/s", admin...)
		if _, scheme, _, _ := stub.snapshot(); scheme != "http" {
			t.Errorf("scheme = %q, want http", scheme)
		}
	})
	t.Run("a client that disconnects does not cut it short", func(t *testing.T) {
		stub := &stubForgetter{res: forgotten(0)}
		_, base, cleanup := startAdminServer(t, withForgetter(stub))
		defer cleanup()
		forget(t, base, "host=h&suite=/s", admin...)
		stub.mu.Lock()
		defer stub.mu.Unlock()
		if !stub.detached {
			t.Error("Forget got the request's context; want one a disconnect cannot cancel")
		}
	})
	t.Run("no forgetter 501", func(t *testing.T) {
		_, base, cleanup := startAdminServer(t)
		defer cleanup()
		if status, _, _ := forget(t, base, "host=h&suite=/s", admin...); status != http.StatusNotImplemented {
			t.Errorf("status = %d, want 501", status)
		}
	})
	t.Run("GET 405, OPTIONS lists POST", func(t *testing.T) {
		_, base, cleanup := startAdminServer(t, withForgetter(&stubForgetter{res: forgotten(0)}))
		defer cleanup()
		for _, method := range []string{http.MethodGet, http.MethodOptions} {
			req, _ := http.NewRequest(method, base+"/forget", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			want := map[string]int{http.MethodGet: http.StatusMethodNotAllowed, http.MethodOptions: http.StatusNoContent}[method]
			if resp.StatusCode != want || resp.Header.Get("Allow") != "POST, OPTIONS" {
				t.Errorf("%s: status %d, Allow %q; want %d, \"POST, OPTIONS\"", method, resp.StatusCode, resp.Header.Get("Allow"), want)
			}
		}
	})
}

// Spec C9 and ruling PF-6: a cross-site form or fetch cannot trigger
// /forget (requireMutationGuard). A form POST (no X-ACU-Admin: 1) and a
// cross-site or same-site fetch (Sec-Fetch-Site) are refused with 403
// before the Forgetter runs; a same-origin request, a browser's direct
// navigation (none) and curl (no Sec-Fetch-Site) pass. (DNS rebinding is
// not covered: spec §10 G-1.)
func TestEndpoint_Forget_RefusesCrossOriginRequests(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers []string
		status  int
	}{
		{"form POST without the header", nil, http.StatusForbidden},
		{"header not 1", []string{"X-ACU-Admin", "true"}, http.StatusForbidden},
		{"cross-site fetch", []string{"X-ACU-Admin", "1", "Sec-Fetch-Site", "cross-site"}, http.StatusForbidden},
		{"same-site fetch", []string{"X-ACU-Admin", "1", "Sec-Fetch-Site", "same-site"}, http.StatusForbidden},
		{"same-origin", []string{"X-ACU-Admin", "1", "Sec-Fetch-Site", "same-origin"}, http.StatusAccepted},
		{"direct (none)", []string{"X-ACU-Admin", "1", "Sec-Fetch-Site", "none"}, http.StatusAccepted},
		{"curl", []string{"X-ACU-Admin", "1"}, http.StatusAccepted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubForgetter{res: forgotten(0)}
			_, base, cleanup := startAdminServer(t, withForgetter(stub))
			defer cleanup()
			if status, _, _ := forget(t, base, "host=h&suite=/s", tc.headers...); status != tc.status {
				t.Errorf("status = %d, want %d", status, tc.status)
			}
			calls, _, _, _ := stub.snapshot()
			if want := map[bool]int{true: 1, false: 0}[tc.status == http.StatusAccepted]; calls != want {
				t.Errorf("calls = %d, want %d", calls, want)
			}
		})
	}
}

// Ruling PF-7 (design review I-7): the status page shows each suite's
// scheme — JSON scheme (additive to the locked contract) and the HTML Host
// cell — so an operator can tell a suite's http and https rows apart and
// give POST /forget the scheme it needs (it defaults to https).
func TestStatus_SuitesShowTheirScheme(t *testing.T) {
	s, base, cleanup := startAdminServer(t)
	defer cleanup()
	for _, scheme := range []string{"http", "https"} {
		if err := s.cfg.Cache.SeedSuiteForTest(context.Background(), cache.SuiteFreshness{CanonicalScheme: scheme,
			CanonicalHost: "deb.example", SuitePath: "/debian/dists/stable"}); err != nil {
			t.Fatal(err)
		}
	}
	resp := mustGet(t, base+"/?format=json")
	var doc struct {
		Suites []struct {
			Scheme string `json:"scheme"`
			Host   string `json:"host"`
		} `json:"suites"`
	}
	err := json.NewDecoder(resp.Body).Decode(&doc)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	schemes := map[string]bool{}
	for _, e := range doc.Suites {
		schemes[e.Scheme+" "+e.Host] = true
	}
	if !schemes["http deb.example"] || !schemes["https deb.example"] {
		t.Fatalf("JSON suites %+v, want an http and an https entry", doc.Suites)
	}
	resp = mustGet(t, base+"/")
	html, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	for _, cell := range []string{`class="host">http://deb.example</td>`, `class="host">https://deb.example</td>`} {
		if !strings.Contains(string(html), cell) {
			t.Errorf("HTML lacks the Host cell %s", cell)
		}
	}
}
```

In `internal/admin/json_contract_test.go`, the populated fixture's suite entry gains, before `Host`:

```go
				Scheme:                           "http",
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -gcflags=-e ./internal/freshness/ ./internal/admin/ -run 'TestReaper_Forget|TestEndpoint_Forget|TestStatus_SuitesShowTheirScheme|TestJSONContract'`
Expected: compile failures `r.reaper.Forget undefined`, `undefined: Forgetter`, `cfg.Forgetter undefined`, `unknown field Scheme in struct literal of type suiteEntry`.

- [ ] **Step 3: Implement**

Create `internal/freshness/forget.go`:

```go
package freshness

// Spec C9 (D9): POST /forget, the reaper's steps for one suite on demand.

import (
	"context"
	"errors"
	"fmt"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// Forget is POST /forget (spec C9, D9; admin.Forgetter): the reaper's
// steps for one suite, run to completion regardless of idleness within
// suiteHoldTimeout — under the suite's mutex every metadata batch, then
// the final compare-and-delete with no idle check (cache.ForgetSuite),
// which also deletes the signed-seen markers of the suite's base, and the
// cached anchor rows of the base and its "/." twin under both schemes,
// when no suite row of its host is left for the base or the twin, under
// either scheme (design note 13, ruling PF-2). Its result's Deleted is
// false, with a nil error, when the suite has no row or is busy (the
// endpoint answers 409 for both: no existence oracle); KeptBy names the
// rows that kept the markers (ruling PF-7). An error is a database
// failure.
//
// AIDEV-NOTE: ruling R-F1 — the one suite-row delete path: the row read
// first (I-4: a lock-map entry is stored only for a suite with a row),
// HoldSuite, the re-read under the hold, deleteSuiteTx through
// cache.ForgetSuite, release(gone) — gone being ForgetSuite's answer,
// settled by a fresh read after a failed submit, or the re-read's finding
// no row (design review M-2). The suite's next request re-creates it
// never adopted, so unchanged upstream content re-adopts at once (signed)
// or after the transition's confirmation (unsigned), and a deliberate
// rollback adopts: the escape hatch for refusal states. Forgetting an
// active suite bypasses the flip barrier (spec C8): a client in the
// middle of an update then can see one Hash Sum mismatch (design review
// M-7, a documented residual).
func (r *Reaper) Forget(ctx context.Context, scheme, host, suitePath string) (cache.ForgetResult, error) {
	ctx, cancel := context.WithTimeout(ctx, suiteHoldTimeout)
	defer cancel()
	key := cache.SuiteKey{CanonicalScheme: scheme, CanonicalHost: host, SuitePath: suitePath}
	switch _, err := r.cache.GetSuiteFreshness(ctx, scheme, host, suitePath); {
	case errors.Is(err, cache.ErrNotFound):
		return cache.ForgetResult{}, nil
	case err != nil:
		return cache.ForgetResult{}, fmt.Errorf("forget: read suite row: %w", err)
	}
	release, ok := r.checker.HoldSuite(key)
	if !ok {
		return cache.ForgetResult{}, nil
	}
	gone := false
	defer func() { release(gone) }()
	if r.reapHook != nil {
		r.reapHook(ctx, key)
	}
	cur, err := r.cache.GetSuiteFreshness(ctx, scheme, host, suitePath)
	switch {
	case errors.Is(err, cache.ErrNotFound):
		gone = true
		return cache.ForgetResult{}, nil
	case err != nil:
		return cache.ForgetResult{}, fmt.Errorf("forget: read suite row: %w", err)
	}
	rows, err := r.cache.DeleteSuiteMetadata(ctx, key)
	if err != nil {
		return cache.ForgetResult{}, fmt.Errorf("forget: %w", err)
	}
	res, err := r.cache.ForgetSuite(ctx, key, cur.CurrentSnapshotID)
	gone = res.Gone
	if err != nil {
		return cache.ForgetResult{}, fmt.Errorf("forget: %w", err)
	}
	if !res.Deleted {
		return res, nil
	}
	r.logger.Info("suite_forgotten",
		"canonical_scheme", scheme,
		"canonical_host", host,
		"suite_path", suitePath,
		"metadata_rows", rows,
		"markers_deleted", res.MarkersDeleted,
		"anchor_rows", res.AnchorRowsDeleted,
	)
	return res, nil
}
```

In `internal/freshness/reaper.go`, `reapHook`'s doc now says it runs "in reapSuite and Forget while the suite is held".

In `internal/admin/server.go`, `Config` gains after `Reconciler`:

```go
	// Forgetter is OPTIONAL. When non-nil, POST /forget is enabled and
	// delegates to Forgetter.Forget (spec C9, D9). When nil, the endpoint
	// returns 501 Not Implemented. cmd wires the freshness Reaper; New
	// does NOT reject a nil Forgetter.
	Forgetter Forgetter
```

and after the `Reconciler` interface:

```go
// Forgetter deletes one suite as the reaper would, regardless of
// idleness, for POST /forget (spec C9, D9). nil → POST /forget returns
// 501. cmd wires the freshness Reaper; the interface keeps admin from
// importing internal/freshness. Forget's result has Deleted when the
// suite was forgotten, with the base's markers deleted and the rows that
// kept them (KeptBy, ruling PF-7); Deleted is false with a nil error when
// the suite is busy or unknown (the endpoint answers 409 for both: no
// existence oracle); an error is a database failure (500).
type Forgetter interface {
	Forget(ctx context.Context, scheme, host, suitePath string) (cache.ForgetResult, error)
}
```

The dispatcher's `OPTIONS` branch gains `else if r.URL.Path == "/forget" { w.Header().Set("Allow", "POST, OPTIONS") }` between the profile case and the default; after the `/reconcile` branch:

```go
		// POST /forget, likewise (spec C9, D9).
		if r.URL.Path == "/forget" {
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST, OPTIONS")
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			s.handleForget(w, r)
			return
		}
```

In `internal/admin/handlers.go` (imports gain `"fmt"` and `internal/cache`), after `handleReconcile`:

```go
// handleForget serves POST /forget (spec C9, D9), the documented escape
// hatch for refusal states — a vendor that stopped signing, a deliberate
// upstream rollback, a stuck unsigned suite: it deletes one suite as the
// reaper would, regardless of idleness, and the signed-seen markers of its
// base (with the base's cached anchor files) when no other suite with that
// base has a row. The work is done when the answer comes; a client that
// disconnects does not cut it short.
//
// Headers: X-ACU-Admin: 1 (required); Sec-Fetch-Site, when present, must
// be same-origin or none (requireMutationGuard). Params (form body): host
// and suite (required), scheme (optional: http or https, default https).
// Returns:
//   - 202 Accepted: the suite is forgotten; the body names it, the markers
//     deleted and the rows that kept them (ruling PF-7).
//   - 400 Bad Request: host or suite missing, or another scheme.
//   - 403 Forbidden: the header is missing, or the request is cross-site.
//   - 409 Conflict: busy or unknown (no existence oracle); the body says
//     what to check.
//   - 413 Request Entity Too Large: the body exceeds 8 KiB.
//   - 500 Internal Server Error: a database failure (logged).
//   - 501 Not Implemented: no Forgetter wired.
func (s *Server) handleForget(w http.ResponseWriter, r *http.Request) {
	if !requireMutationGuard(w, r) {
		return
	}
	if s.cfg.Forgetter == nil {
		http.Error(w, "forget not available\n", http.StatusNotImplemented)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "request body too large\n", http.StatusRequestEntityTooLarge)
		return
	}
	host := strings.TrimSpace(r.FormValue("host"))
	suite := strings.TrimSpace(r.FormValue("suite"))
	scheme := strings.TrimSpace(r.FormValue("scheme"))
	if scheme == "" {
		scheme = "https"
	}
	if host == "" || suite == "" || (scheme != "http" && scheme != "https") {
		http.Error(w, "host and suite are required; scheme is http or https\n", http.StatusBadRequest)
		return
	}
	s.logger.Info("admin_forget_requested", "scheme", scheme, "canonical_host", host, "suite_path", suite)
	res, err := s.cfg.Forgetter.Forget(context.WithoutCancel(r.Context()), scheme, host, suite)
	switch {
	case err != nil:
		s.logger.Warn("admin: forget failed", "err", err, "scheme", scheme, "canonical_host", host, "suite_path", suite)
		http.Error(w, "forget failed\n", http.StatusInternalServerError)
		return
	case !res.Deleted:
		http.Error(w, "not forgotten: busy (try again) or no such suite (scheme defaults to https; "+
			"host and suite as the status page shows them)\n", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	_, _ = io.WriteString(w, forgottenText(scheme, host, suite, res))
}

// forgottenText is POST /forget's 202 body (ruling PF-7): what was
// forgotten, how many signed-seen markers of its base went with it, and
// the suite rows that kept them, if any.
func forgottenText(scheme, host, suite string, res cache.ForgetResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "forgotten %s://%s%s; signed-seen markers deleted: %d", scheme, host, suite, res.MarkersDeleted)
	if len(res.KeptBy) > 0 {
		b.WriteString("; kept by")
		for _, k := range res.KeptBy {
			fmt.Fprintf(&b, " %s://%s%s", k.CanonicalScheme, k.CanonicalHost, k.SuitePath)
		}
	}
	b.WriteString("\n")
	return b.String()
}

// requireMutationGuard is the guard of the admin listener's mutating
// endpoints (POST /forget; spec §10 G-1 brings POST /reconcile under it):
// it answers 403 and reports false unless the request carries
// X-ACU-Admin: 1 and, when it has one, a Sec-Fetch-Site of same-origin or
// none. curl and scripts send no Sec-Fetch-Site.
//
// AIDEV-NOTE: ruling PF-6 (design review I-6); keep the claim honest. A
// cross-site form or fetch cannot trigger an endpoint behind this guard:
// a form POST cannot carry a custom header, a cross-origin fetch that sets
// one is refused on the Sec-Fetch-Site the browser adds (a page cannot
// forge it), and its CORS preflight is never granted. It does not stop
// DNS rebinding against a listener without htpasswd_file: a page whose
// own name resolves to the listener's address makes same-origin requests.
// Host-header validation of the mutating endpoints is spec §10 G-1's, for
// /forget and /reconcile together: it affects operators behind reverse
// proxies, a maintainer call.
func requireMutationGuard(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-ACU-Admin") != "1" {
		http.Error(w, "this endpoint requires the header X-ACU-Admin: 1\n", http.StatusForbidden)
		return false
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		http.Error(w, "cross-site requests are refused\n", http.StatusForbidden)
		return false
	}
	return true
}
```

In `internal/admin/status.go`, `suiteEntry` gains a first field:

```go
	// Scheme is the suite's canonical scheme, http or https: a suite of
	// either scheme may share host and path, and POST /forget takes it
	// (ruling PF-7; additive to the locked JSON contract).
	Scheme                           string `json:"scheme"`
```

and `buildSuiteEntries` sets `Scheme: r.CanonicalScheme,` before `Host`. In `internal/admin/status_template.go`, the Suites panel's description begins "One row per (scheme, host, suite)." and its Host cell becomes:

```html
                <td data-label="Host" class="host">{{if .Scheme}}{{.Scheme}}://{{end}}{{.Host}}</td>
```

Regenerate the JSON-contract golden (one key added, `"scheme": "http"`): `go test ./internal/admin/ -run TestJSONContractPreserved -update.jsoncontract`.

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/freshness/ ./internal/admin/`
Expected: PASS.

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| `requireMutationGuard` without the `X-ACU-Admin` check | `TestEndpoint_Forget_RefusesCrossOriginRequests` (`status = 202, want 403`, `calls = 1, want 0`) |
| `same-site` let through | `TestEndpoint_Forget_RefusesCrossOriginRequests/same-site_fetch` |
| `Forget` gets `r.Context()` | `TestEndpoint_Forget` (`Forget got the request's context; …`) |
| no scheme check | `TestEndpoint_Forget/another_scheme_400` (`status = 202, body "forgotten ftp://h/s; …"`) |
| the dispatcher's `OPTIONS` without the `/forget` case | `TestEndpoint_Forget` (`OPTIONS: status 204, Allow "GET, HEAD, OPTIONS"`) |
| `Forget` without its first read (`HoldSuite` first) | `TestReaper_ForgetAnswersFalseForBusyAndUnknown` (`1 lock-map loads; … I-4`) |
| `Forget` drops `gone = res.Gone` (`release(false)` after the delete) | `TestReaper_ForgetDeletesASuiteRegardlessOfIdleness` (`lock map 1, flip gate 1, touch map 0 entries`) |
| `Forget` without `DeleteSuiteMetadata` | `TestReaper_ForgetDeletesASuiteRegardlessOfIdleness` (`AnchorRowsDeleted:2`: the http anchor went with the markers instead) |
| X9: a row gone under the hold leaves `gone` false | `TestReaper_ForgetDropsTheLockEntryOfASuiteGoneUnderTheHold` (`1 lock entries`) |
| PF-7: the 202 body without the keeping rows | `TestEndpoint_Forget/forgotten,_markers_kept_202` |
| PF-7: the draft's 409 text (`not forgotten (busy or unknown)`) | `TestEndpoint_Forget/busy_or_unknown_409` |
| PF-2: `anchor_rows` not logged | `TestReaper_ForgetDeletesASuiteRegardlessOfIdleness` (`suite_forgotten lines […]`) |
| M-2: `Forget` without its bound (`context.WithCancel`) | `TestReaper_ForgetBoundsItsWorkAtFiveMinutes` (`(set false)`) |
| PF-7: the status JSON without the scheme | `TestStatus_SuitesShowTheirScheme` (`JSON suites [{Scheme: Host:deb.example} …]`) |
| PF-7: the HTML Host cell without the scheme | `TestStatus_SuitesShowTheirScheme` (`HTML lacks the Host cell class="host">http://deb.example</td>`) |

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/freshness/forget.go internal/freshness/forget_test.go internal/freshness/reaper.go internal/admin/forget_test.go internal/admin/server.go internal/admin/handlers.go internal/admin/status.go internal/admin/status_template.go internal/admin/json_contract_test.go
git add internal/freshness/forget.go internal/freshness/forget_test.go internal/freshness/reaper.go internal/admin/forget_test.go internal/admin/server.go internal/admin/handlers.go internal/admin/status.go internal/admin/status_template.go internal/admin/json_contract_test.go internal/admin/testdata/json_contract_golden.json
git commit -m "Add POST /forget: the reaper's steps for one suite, guarded, with replies that say what happened; show each suite's scheme (spec C9, D9; rulings PF-2, PF-6, PF-7)"
```

(Record the mutation table in the commit message.)

### Task 11: `gc.suite_idle_ttl` and the daemon wiring

Spec C11's knob: `gc.suite_idle_ttl`, default `720h`, `0s` disables suite reaping (design note 2), otherwise at least `24h`; checked whatever `gc.enabled` says, since the reaper runs with the freshness checker (design note 1). `main` builds the reaper beside the checker, starts it under the same lifetime context and wait group, and wires it as the admin listener's `Forgetter`. The packaged `config.toml.default` documents the key (its tests require every key, at its default). The packaged comment says what reaping costs an offline or archive cache — a reaped suite comes back from upstream — and that `0s` is its setting (design review M-11). The comments that still promised phase F (`warnSuiteCaps`) or the unsigned form "in a later release" (`config.go`) are rewritten, and deferred minor E-72 lands. (The draft's `Validate` block stopped at its `else if` line; it is complete below — exec review D1, design review M-1.)

**Files:**
- Modify: `internal/config/config.go` (`GCConfig.SuiteIdleTTL`, its `Load` default and validation; E-72; two stale comments), `packaging/config/config.toml.default` (`[gc]` `suite_idle_ttl`; the `[admin]` comment), `cmd/apt-cacher-ultra/main.go` (`reaperConfig`, the reaper's construction and goroutine, `Forgetter`, `warnSuiteCaps`'s comments)
- Create: `internal/config/suite_idle_ttl_test.go`, `cmd/apt-cacher-ultra/reaper_wiring_test.go`

**Interfaces:**
- Consumes: `freshness.NewReaper`, `ReaperConfig`, `(*Reaper).Run` (Task 6), `(*Reaper).Forget`, `admin.Config.Forgetter` (Task 10); config test helper `writeTOML`; cmd test helpers `minimalCfg`, `silentBuildLogger`, `waitForDaemonReady`, `shutdownTimeout`, `metrics.Default.SnapshotNamesForTest`, `UnregisterAddedSinceForTest`.
- Produces: `config.GCConfig.SuiteIdleTTL Duration` (TOML `gc.suite_idle_ttl`); `reaperConfig(cfg *config.Config, c *cache.Cache, checker *freshness.Checker, logger *slog.Logger) freshness.ReaperConfig` (`cmd/apt-cacher-ultra/main.go`).

- [ ] **Step 1: Write the failing tests**

Create `internal/config/suite_idle_ttl_test.go`:

```go
package config

import (
	"strings"
	"testing"
	"time"
)

// Spec C11: gc.suite_idle_ttl defaults to 720h; 0s (disabled) and values
// of at least 24h load; a shorter or a negative one is refused, naming the
// key, whatever gc.enabled says (the reaper runs with the checker).
func TestLoad_SuiteIdleTTL(t *testing.T) {
	dir := t.TempDir()
	load := func(name, extra string) (*Config, error) {
		t.Helper()
		return Load(writeTOML(t, dir, name, "[cache]\ndir = \""+dir+"\"\n"+extra))
	}
	cfg, err := load("default.toml", "")
	if err != nil || cfg.GC.SuiteIdleTTL.Duration != 720*time.Hour {
		t.Fatalf("default: %v, %v; want 720h", cfg.GC.SuiteIdleTTL.Duration, err)
	}
	for i, ok := range []time.Duration{0, 24 * time.Hour, 8760 * time.Hour} {
		cfg, err := load("ok.toml", "[gc]\nsuite_idle_ttl = \""+ok.String()+"\"\n")
		if err != nil || cfg.GC.SuiteIdleTTL.Duration != ok {
			t.Errorf("case %d: suite_idle_ttl %s loaded as %v, %v", i, ok, cfg.GC.SuiteIdleTTL.Duration, err)
		}
	}
	for _, bad := range []string{"23h59m", "-1h"} {
		for _, enabled := range []string{"true", "false"} {
			_, err := load("bad.toml", "[gc]\nenabled = "+enabled+"\nsuite_idle_ttl = \""+bad+"\"\n")
			if err == nil || !strings.Contains(err.Error(), "gc.suite_idle_ttl") {
				t.Errorf("suite_idle_ttl %s with gc.enabled = %s: %v; want a refusal naming the key", bad, enabled, err)
			}
		}
	}
}
```

Create `cmd/apt-cacher-ultra/reaper_wiring_test.go`:

```go
package main

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
)

// Spec C11: gc.suite_idle_ttl reaches the reaper.
func TestReaperConfig_CarriesSuiteIdleTTL(t *testing.T) {
	cfg := minimalCfg(t.TempDir(), nil)
	cfg.GC.SuiteIdleTTL.Duration = 48 * time.Hour
	if rc := reaperConfig(cfg, nil, nil, silentBuildLogger()); rc.IdleTTL != 48*time.Hour {
		t.Fatalf("IdleTTL = %v, want gc.suite_idle_ttl (48h)", rc.IdleTTL)
	}
}

// Spec C9 (D9): the daemon wires the reaper as the admin listener's
// Forgetter — POST /forget for a suite the cache does not track answers
// 409 (busy or unknown), not 501. Mutates shutdownTimeout, so NOT
// t.Parallel.
func TestServe_ForgetIsWired(t *testing.T) {
	oldTimeout := shutdownTimeout
	shutdownTimeout = 500 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = oldTimeout })
	preMetrics := metrics.Default.SnapshotNamesForTest()

	cfg := minimalCfg(t.TempDir(), nil)
	cfg.Admin = config.AdminConfig{
		Enabled:         true,
		GaugeRefresh:    config.Duration{Duration: 50 * time.Millisecond},
		ReadTimeout:     config.Duration{Duration: 5 * time.Second},
		IdleTimeout:     config.Duration{Duration: 30 * time.Second},
		MetricSeriesCap: 1024,
	}
	cacheLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	adminLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = cacheLn.Close()
		t.Fatal(err)
	}
	cfg.Admin.Listen = adminLn.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- serveListeners(ctx, cfg, silentBuildLogger(), cacheLn, nil, adminLn, nil) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-serveDone:
		case <-time.After(15 * time.Second):
			t.Errorf("serveListeners did not return on cleanup")
		}
		metrics.Default.UnregisterAddedSinceForTest(preMetrics)
	})
	if err := waitForDaemonReady(t, cacheLn.Addr().String(), 10*time.Second); err != nil {
		t.Fatalf("daemon never became ready: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+cfg.Admin.Listen+"/forget",
		strings.NewReader("host=deb.example&suite=/debian/dists/stable"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-ACU-Admin", "1")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("POST /forget = %d, want 409 from the wired reaper", resp.StatusCode)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -gcflags=-e ./internal/config/ ./cmd/apt-cacher-ultra/ -run 'TestLoad_SuiteIdleTTL|TestReaperConfig|TestServe_ForgetIsWired'`
Expected: compile failures `cfg.GC.SuiteIdleTTL undefined` and `undefined: reaperConfig`.

- [ ] **Step 3: Implement**

In `internal/config/config.go`, `GCConfig` gains after `URLPathTTL`:

```go
	// SuiteIdleTTL is gc.suite_idle_ttl (spec C9, C11): the suite reaper
	// deletes a suite no client asked for in this long — its metadata
	// url_path rows and its row, its snapshots retired for snapshot GC —
	// so a returning client's suite re-adopts as new. Default 720h (30
	// days); 0s disables suite reaping (the reaper's other duties still
	// run); otherwise at least 24h. Checked whatever gc.enabled says: the
	// reaper runs with the freshness checker, not with GC.
	SuiteIdleTTL Duration `toml:"suite_idle_ttl"`
```

`Load`'s presence-sensitive defaults gain, after `url_path_ttl`'s (`Defaults()` leaves it 0, as it does `url_path_ttl`):

```go
	if !md.IsDefined("gc", "suite_idle_ttl") {
		cfg.GC.SuiteIdleTTL.Duration = 720 * time.Hour
	}
```

`Validate` gains, after `url_path_ttl`'s check (outside the `gc.enabled` block):

```go
	if c.GC.SuiteIdleTTL.Duration < 0 {
		errs = append(errs, errors.New("gc.suite_idle_ttl must not be negative (0s disables suite reaping)"))
	} else if c.GC.SuiteIdleTTL.Duration > 0 && c.GC.SuiteIdleTTL.Duration < 24*time.Hour {
		errs = append(errs, fmt.Errorf(
			"gc.suite_idle_ttl (%s) must be 0s (disabled) or >= 24h", c.GC.SuiteIdleTTL.Duration))
	}
```

Three comments change: in `defaultConfig`, `// Spec C5 D6: unsigned adoption on by default, safeguarded.` becomes `// Spec C5 safeguard 1, C11: unsigned adoption on by default,` / `// safeguarded (deferred minor E-72).`; `RequireSignature`'s doc ends "Unsigned repositories are handled by the unsigned form (adopt_unsigned, spec C5)." and `AcceptAnySigner`'s "…does not make unsigned upstreams adoptable (adopt_unsigned governs the unsigned form, spec C5)." (phase E shipped the form those comments promised "in a later release").

In `packaging/config/config.toml.default`, `[gc]` ends:

```toml
url_path_ttl        = "168h"
# A suite no client asked for in this long is reaped: its metadata rows and
# its row; its snapshots go with the next GC runs. Its next request makes it
# new again, from upstream. 0s disables suite reaping (an offline or archive
# cache); otherwise >= 24h.
suite_idle_ttl      = "720h"
# GC numeric constraints still apply when enabled=false; suite reaping runs
# with the freshness checker whatever enabled says.
```

and `[admin]`'s first comment becomes `# HTTP status page, /metrics, /healthz, and mutating POST /reconcile and` / `# POST /forget (the latter needs the header X-ACU-Admin: 1).`

In `cmd/apt-cacher-ultra/main.go`, after `freshChecker` is built:

```go
	// Spec C9: the suite reaper runs beside the checker, under its
	// lifetime, whatever gc.enabled says, and serves POST /forget.
	reaper, err := freshness.NewReaper(reaperConfig(cfg, c, freshChecker, logger))
	if err != nil {
		return fmt.Errorf("build suite reaper: %w", err)
	}
```

after the `freshChecker.Run` goroutine:

```go
	freshWG.Add(1)
	go func() {
		defer freshWG.Done()
		reaper.Run(freshCtx)
	}()
```

`admin.Config` gains `Forgetter: reaper,` after `Reconciler`; before `checkerConfig`:

```go
// reaperConfig is the suite reaper's configuration (spec C9, C11:
// gc.suite_idle_ttl). Factored out, like checkerConfig, for the wiring
// tests.
func reaperConfig(cfg *config.Config, c *cache.Cache, checker *freshness.Checker, logger *slog.Logger) freshness.ReaperConfig {
	return freshness.ReaperConfig{Cache: c, Checker: checker, IdleTTL: cfg.GC.SuiteIdleTTL.Duration, Logger: logger}
}
```

`warnSuiteCaps`'s call-site comment and doc name "the suite reaper" where they said "phase F's reaper" (the doc: "waits for the suite reaper (gc.suite_idle_ttl, 30 days by default; spec C9)").

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/config/ ./cmd/apt-cacher-ultra/ ./internal/admin/`
Expected: PASS (`TestDefaultConfigFileDocumentsEveryKey` and `TestDefaultConfigFilePreservesProfile` hold the packaged file to the new key and its default).

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| no `suite_idle_ttl` default | `TestLoad_SuiteIdleTTL` (`default: 0s`), `TestDefaultConfigFilePreservesProfile` |
| the lower bound `24*time.Hour` → `time.Hour` | `TestLoad_SuiteIdleTTL` (`23h59m … want a refusal`) |
| the checks only with `gc.enabled` (`c.GC.Enabled &&` in both) | `TestLoad_SuiteIdleTTL` (`with gc.enabled = false`) |
| `reaperConfig` drops the TTL | `TestReaperConfig_CarriesSuiteIdleTTL` |
| `Forgetter: reaper` dropped | `TestServe_ForgetIsWired` (`POST /forget = 501`) |

The reaper goroutine's start mirrors the checker's two lines above it; no test waits out its hour-long uptime floor in the daemon (Task 6 pins `Run`'s schedule).

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/config/ cmd/apt-cacher-ultra/main.go cmd/apt-cacher-ultra/reaper_wiring_test.go
git add internal/config/config.go internal/config/suite_idle_ttl_test.go packaging/config/config.toml.default cmd/apt-cacher-ultra/main.go cmd/apt-cacher-ultra/reaper_wiring_test.go
git commit -m "Add gc.suite_idle_ttl and wire the suite reaper and POST /forget into the daemon (spec C9, C11); deferred minor E-72"
```

(Record the mutation table in the commit message.)

### Task 12: The gpg once-logger sets are bounded

Spec C9: "The gpg once-logger maps reset when they exceed 10,000 entries." The verifier's three per-suite once-loggers (`adoption_untrusted_signer`, `adoption_short_keyid_fallback`, `adoption_unverified_signer`) kept a `sync.Map` for the process lifetime while suites come and go; each becomes an `onceSet` holding at most 10,000 keys, where a new key when the set is full starts it over (design note 17) — the cost is a once-line logged again for a suite seen before.

**Files:**
- Create: `internal/gpg/oncelog.go`, `internal/gpg/oncelog_test.go`
- Modify: `internal/gpg/verifier.go` (the three fields and their three `LoadOrStore` sites; the `sync` import goes)

**Interfaces:**
- Consumes: `(*Verifier).logUntrustedSignerOnce`, `freshness.SuiteRef`.
- Produces: unexported `onceSetMax = 10_000`, `onceSet` (zero value ready), `(*onceSet).first(key string) bool`; the test handler `countingHandler` (`oncelog_test.go`).

- [ ] **Step 1: Write the failing tests**

Create `internal/gpg/oncelog_test.go`:

```go
package gpg

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
)

// Spec C9: each once-logger set holds at most 10,000 keys; a new key when
// it is full starts the set over, so an earlier suite logs its once-line
// again instead of the set growing for the process lifetime.
func TestOnceSet_StartsOverPastTenThousandKeys(t *testing.T) {
	var s onceSet
	if !s.first("a") || s.first("a") {
		t.Fatal("a key is not new exactly once")
	}
	for i := 1; i < onceSetMax; i++ {
		if !s.first(fmt.Sprintf("k%d", i)) {
			t.Fatalf("key %d was not new", i)
		}
	}
	if s.first("a") {
		t.Fatal("a full set forgot a key before overflowing")
	}
	if !s.first("overflow") {
		t.Fatal("the 10,001st key was not new")
	}
	if !s.first("a") {
		t.Fatal("after starting over, the set still remembered an old key")
	}
	if n := len(s.keys); n != 2 {
		t.Fatalf("the set holds %d keys after starting over, want 2", n)
	}
}

// countingHandler counts log records by message.
type countingHandler struct {
	mu sync.Mutex
	n  map[string]int
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.n[r.Message]++
	return nil
}

func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

// The verifier's once-loggers use the bounded set: the untrusted-signer
// line is logged once per suite, and again for a suite seen before once
// 10,000 others have filled the set.
func TestVerifier_OnceLoggersAreBounded(t *testing.T) {
	h := &countingHandler{n: map[string]int{}}
	v := &Verifier{logger: slog.New(h)}
	suite := func(path string) freshness.SuiteRef {
		return freshness.SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: path}
	}
	v.logUntrustedSignerOnce(suite("/first"), nil)
	v.logUntrustedSignerOnce(suite("/first"), nil)
	for i := 1; i < onceSetMax; i++ {
		v.logUntrustedSignerOnce(suite(fmt.Sprintf("/s%d", i)), nil)
	}
	v.logUntrustedSignerOnce(suite("/overflow"), nil)
	v.logUntrustedSignerOnce(suite("/first"), nil)
	if n := h.n["adoption_untrusted_signer"]; n != onceSetMax+2 {
		t.Fatalf("adoption_untrusted_signer logged %d times, want %d", n, onceSetMax+2)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -gcflags=-e ./internal/gpg/ -run 'TestOnceSet|TestVerifier_OnceLoggers'`
Expected: compile failures `undefined: onceSet`, `undefined: onceSetMax`.

- [ ] **Step 3: Implement**

Create `internal/gpg/oncelog.go`:

```go
package gpg

import "sync"

// onceSetMax is spec C9's bound on each once-logger set (a constant, not
// a knob).
const onceSetMax = 10_000

// onceSet is a once-logger's key set ("canonical_host|suite_path"),
// bounded by onceSetMax: a new key when the set is full starts it over.
// The zero value is ready to use.
//
// AIDEV-NOTE: spec C9 — suites come and go (the reaper deletes them), so
// an unbounded set would grow for the process lifetime. The cost of the
// reset is a once-line logged again for a suite seen before it.
type onceSet struct {
	mu   sync.Mutex
	keys map[string]struct{}
}

// first reports whether key is new to the set, and records it.
func (s *onceSet) first(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.keys[key]; ok {
		return false
	}
	if s.keys == nil || len(s.keys) >= onceSetMax {
		s.keys = make(map[string]struct{})
	}
	s.keys[key] = struct{}{}
	return true
}
```

In `internal/gpg/verifier.go`, `shortKeyIDLogged`, `untrustedSignerLogged` and `unverifiedSignerLogged` become `onceSet` (the first's comment: "Keyed by "canonical_host|suite_path"; at most onceSetMax keys, then it starts over (spec C9)."), each `if _, loaded := v.<set>.LoadOrStore(key, struct{}{}); loaded {` becomes `if !v.<set>.first(key) {`, and the `sync` import goes.

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/gpg/`
Expected: PASS.

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| no bound (`if s.keys == nil {`) | `TestOnceSet_StartsOverPastTenThousandKeys` (`after starting over, the set still remembered an old key`), `TestVerifier_OnceLoggersAreBounded` (`logged 10001 times, want 10002`) |
| the bound one late (`>` for `>=`) | both, as above |
| `logUntrustedSignerOnce` never deduplicates (`… && false`) | `TestVerifier_OnceLoggersAreBounded` (`logged 10003 times`) |

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/gpg/
git add internal/gpg/oncelog.go internal/gpg/oncelog_test.go internal/gpg/verifier.go
git commit -m "Bound the gpg once-logger sets at 10,000 keys (spec C9)"
```

(Record the mutation table in the commit message.)

### Task 13: Deferred minors in `freshness/adoption.go`

Task 6 changes `internal/freshness/adoption.go` by hand (the reconcile memo's AIDEV-NOTE), so its open deferred minors land, each in its own step-sized change: E-6 (a reused blob's SHA-512 mismatch logs `adoption_member_mismatch` with both full digests), E-7 (the reuse rehash computes SHA-512 only for a member that declares one), E-8 (`hashFile`'s comment sits above `hashFile`), E-12 (the by-hash probe's local faults are `ErrAdoptionDBFailed`, as on the plain path), E-15 (a test that an unsigned Release declaring `Sources` and a `Packages.diff/Index` writes no `package_hash` row). Checked and left in this file: E-9 (G: SHA512-only flat repositories). E-13 names `adoption.go`, but its WARN lives in `flip_barrier.go` since C-19's split, which no task changes; Task 16 corrects its file name in `E-deferred.md`.

**Files:**
- Modify: `internal/freshness/adoption.go`
- Create: `internal/freshness/adoption_minors_test.go`

**Interfaces:**
- Consumes: `(*Adopter).adoptMember`, `tryByHashFetch`, `writeBlobBytes`, `RunUnsigned`, `Run`; `byHashProbePath`, `ReleaseMember`, `ErrAdoptionMemberMismatch`, `ErrAdoptionDBFailed`; `(*cache.Cache).Dir`, `Close`; freshness test helpers `newUnsignedRig` (`records`, `current`, `members`, `pkgs`), `hashRelease`, `shaOf`, `sha512Of`, `packageHashCount`, `passThroughVerifier`, `testHex64`; `simtest.OpenCache`, `simtest.ClearsignedFixture`.
- Produces: no API; the WARN `adoption_member_mismatch` on the reuse path (`declared_sha512`, `actual_sha512`); the test fetcher `closingFetcher` (`adoption_minors_test.go`).

- [ ] **Step 1: Write the failing tests**

Create `internal/freshness/adoption_minors_test.go`:

```go
package freshness

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// Deferred minor E-6: a pooled blob whose SHA-256 matches but whose
// SHA-512 does not fails the member and logs adoption_member_mismatch
// with both full SHA-512 digests (the reason chip carries 12 characters).
func TestAdoptMember_ReusedBlobSHA512MismatchLogsTheFullDigests(t *testing.T) {
	ctx := context.Background()
	r := newUnsignedRig(t, 0, 0)
	body := []byte("pooled member bytes")
	if _, err := r.ad.writeBlobBytes(ctx, body); err != nil {
		t.Fatal(err)
	}
	declared := strings.Repeat("ab", 64)
	_, err := r.ad.adoptMember(ctx, r.suite, ReleaseMember{Path: "main/binary-amd64/Packages", SHA256: shaOf(body),
		SHA512: declared, Size: int64(len(body))}, false)
	if !errors.Is(err, ErrAdoptionMemberMismatch) {
		t.Fatalf("adoptMember = %v, want ErrAdoptionMemberMismatch", err)
	}
	recs := r.records("adoption_member_mismatch")
	if len(recs) != 1 || recs[0]["declared_sha512"] != declared || recs[0]["actual_sha512"] != sha512Of(body) {
		t.Fatalf("adoption_member_mismatch records %v; want one with both full SHA-512 digests", recs)
	}
}

// closingFetcher serves body and then closes the cache: the fetch
// succeeds and the PutBlob after it fails, a local fault.
type closingFetcher struct {
	c    *cache.Cache
	body []byte
}

func (f closingFetcher) Fetch(_ context.Context, _ *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error) {
	if _, err := dst.Write(f.body); err != nil {
		return nil, err
	}
	_ = f.c.Close()
	return &fetch.FetchResult{Status: 200, ContentLength: int64(len(f.body))}, nil
}

// Deferred minor E-12: the by-hash probe's local faults — the temp blob,
// the blob row — are ErrAdoptionDBFailed, as on the plain path, so the
// member fails at once instead of being downloaded again.
func TestTryByHashFetch_LocalFaultsAreDBFailures(t *testing.T) {
	ctx := context.Background()
	body := []byte("by-hash member bytes")
	m := ReleaseMember{Path: "main/binary-amd64/Packages", SHA256: shaOf(body), Size: int64(len(body))}
	probe := byHashProbePath(m)
	t.Run("temp blob", func(t *testing.T) {
		r := newUnsignedRig(t, 0, 0)
		if err := os.RemoveAll(filepath.Join(r.c.Dir(), "tmp")); err != nil {
			t.Fatal(err)
		}
		if _, err := r.ad.tryByHashFetch(ctx, r.suite, m, probe); !errors.Is(err, ErrAdoptionDBFailed) {
			t.Fatalf("tryByHashFetch = %v, want ErrAdoptionDBFailed", err)
		}
	})
	t.Run("blob row", func(t *testing.T) {
		c := simtest.OpenCache(t)
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: closingFetcher{c: c, body: body}, Verifier: passThroughVerifier{},
			HostLimiter: hostsem.New(8), Logger: slog.New(slog.DiscardHandler)})
		if err != nil {
			t.Fatal(err)
		}
		suite := SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/u/dists/s"}
		if _, err := ad.tryByHashFetch(ctx, suite, m, probe); !errors.Is(err, ErrAdoptionDBFailed) {
			t.Fatalf("tryByHashFetch = %v, want ErrAdoptionDBFailed", err)
		}
	})
}

// Deferred minor E-15 (spec C5 safeguard 5): an unsigned Release that
// declares a Sources file and a Packages.diff/Index writes no package_hash
// row — the Sources and pdiff builders do not run for the unsigned form —
// while the same Release adopted inline writes one row per builder (the
// fixture parses).
func TestRunUnsigned_SourcesAndPdiffGetNoPackageHash(t *testing.T) {
	ctx := context.Background()
	sources := []byte("Package: bash\nVersion: 5.1-2\nChecksums-Sha256:\n " + testHex64 +
		" 4080 bash_5.1-2.dsc\nDirectory: pool/main/b/bash\n\n")
	index := []byte("SHA256-Download:\n " + testHex64 + " 1500 2026-05-09-1234.56.gz\n")
	for _, unsigned := range []bool{true, false} {
		r := newUnsignedRig(t, 0, 0)
		members := map[string][]byte{"main/binary-amd64/Packages": r.pkgs, "main/source/Sources": sources,
			"main/binary-amd64/Packages.diff/Index": index}
		for p, b := range members {
			r.members.put("http://h/u/dists/s/"+p, b)
		}
		rel := hashRelease("Origin: e15\n", true, false, members, nil)
		var err error
		if unsigned {
			err = r.ad.RunUnsigned(ctx, r.suite, rel, "", "")
		} else {
			err = r.ad.Run(ctx, r.suite, simtest.ClearsignedFixture(string(rel)), "", "")
		}
		if err != nil {
			t.Fatalf("unsigned %v: %v", unsigned, err)
		}
		want := 3 // the .deb, the .dsc and the patch
		if unsigned {
			want = 0
		}
		if n := packageHashCount(t, r.c, r.current()); n != want {
			t.Errorf("unsigned %v: %d package_hash rows, want %d", unsigned, n, want)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/freshness/ -run 'TestAdoptMember_ReusedBlobSHA512|TestTryByHashFetch_LocalFaults|TestRunUnsigned_SourcesAndPdiff'`
Expected: FAIL — `adoption_member_mismatch records []`, `tryByHashFetch = cache: open tmp blob: … no such file or directory, want ErrAdoptionDBFailed`, `tryByHashFetch = cache: closed, want ErrAdoptionDBFailed`. `TestRunUnsigned_SourcesAndPdiffGetNoPackageHash` passes: it pins phase E's guards (E-15).

- [ ] **Step 3: Implement**

In `adoptMember`'s rehash on reuse, replace `actual, actual512, err := hashFileDigests(a.cache.BlobPath(m.SHA256))` with:

```go
		// A SHA-512 is computed only for a member that declares one
		// (deferred minor E-7): Debian's and Ubuntu's Releases declare
		// SHA-256 alone.
		var actual, actual512 string
		var err error
		if m.SHA512 == "" {
			actual, err = hashFile(a.cache.BlobPath(m.SHA256))
		} else {
			actual, actual512, err = hashFileDigests(a.cache.BlobPath(m.SHA256))
		}
```

and, in the mismatch branch, before its `return`:

```go
			// The WARN carries the full digests (deferred minor E-6).
			a.logger.Warn("adoption_member_mismatch",
				"canonical_host", suite.CanonicalHost,
				"suite_path", suite.SuitePath,
				"path", m.Path,
				"declared_sha512", m.SHA512,
				"actual_sha512", actual512,
			)
```

In `tryByHashFetch`, `newMemberWriter`'s error returns `fmt.Errorf("%w: NewTempBlob %s: %v", ErrAdoptionDBFailed, byHashRelPath, err)` and `PutBlob`'s `fmt.Errorf("%w: PutBlob %s: %v", ErrAdoptionDBFailed, hashHex, err)`, under the comment:

```go
	// A local fault (the temp blob, the blob row) is ErrAdoptionDBFailed,
	// as on the plain path, so the caller fails the member instead of
	// downloading it again (deferred minor E-12).
```

Move `hashFile`'s comment from above `shortHash` to above `hashFile`, as:

```go
// hashFile streams a file through sha256 and returns the lowercase hex
// digest. Used by writeBlobBytes, and by adoptMember's "rehash on reuse"
// defense for a member that declares no SHA-512 (one that does goes
// through hashFileDigests) — content sitting in pool/ from a prior fetch
// is re-verified before being trusted as a snapshot member.
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/freshness/`
Expected: PASS.

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| the reuse mismatch WARN dropped | `TestAdoptMember_ReusedBlobSHA512MismatchLogsTheFullDigests` |
| `tryByHashFetch`'s `newMemberWriter` error unwrapped | `TestTryByHashFetch_LocalFaultsAreDBFailures/temp_blob` |
| `tryByHashFetch`'s `PutBlob` error unwrapped | `TestTryByHashFetch_LocalFaultsAreDBFailures/blob_row` (`cache: closed`) |
| the `!unsigned` guard on `buildSourceHashes` → `true` | `TestRunUnsigned_SourcesAndPdiffGetNoPackageHash` (`unsigned true: 1 package_hash rows`) |
| the `!unsigned` guard on `buildPdiffHashes` → `true` | `TestRunUnsigned_SourcesAndPdiffGetNoPackageHash` |

E-7 changes CPU only: hashing a SHA-256-only member with `hashFileDigests` is an equivalent mutant no test can see (the reuse tests pin both branches' results).

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/freshness/adoption.go internal/freshness/adoption_minors_test.go
git add internal/freshness/adoption.go internal/freshness/adoption_minors_test.go
git commit -m "Close adoption.go's deferred minors E-6, E-7, E-8, E-12, E-15"
```

(Record the mutation table in the commit message.)

### Task 14: Deferred minors in the suite lock, `first_adoption.go` and the handler tests

Earlier tasks change these files by hand — `suite_lock_test.go`, `suite_hit_test.go` and `suite_dod_test.go` (Task 1), `first_adoption.go` (Task 8) — and this task moves code into `suite_lock.go`'s neighbours (`clockstep.go`, `scheduler_test.go`, `unsigned.go`), so their open deferred minors land: D-24, D-25, E-49, E-33 (its `first_adoption.go` half), E-34, E-26 (a test of `first_adoption.go`'s anchor-row read error), D-36, D-40, D-41, D-42, D-43, D-44. Nothing here changes behaviour; the steps are moves, comments and stronger tests, so no step fails first. The `clockStep` AIDEV-NOTE also learns phase F's retiring gates (the anchor rule: notes follow the code they describe): a future marker stamp is read as now and never rewritten (ruling PF-1), and the reaper and the sweep report such a stamp when they first see it and when it changes (design review M-10). `unsignedRefusal` moves with the note Task 8 gave it (ruling PF-5).

**Files:**
- Modify: `internal/freshness/suite_lock.go` (D-25: `ReportClockStep` moves out; `HoldSuite`'s doc; two notes; the `time` import goes), `internal/freshness/clockstep.go` (`ReportClockStep`, the `cache` import, `clockStep`'s AIDEV-NOTE), `internal/freshness/suite_lock_test.go` (D-24; three tests move out; `fmt` and `slices` imports go), `internal/freshness/scheduler_test.go` (the three tests, verbatim; `slices` import; E-49), `internal/freshness/first_adoption.go` (E-33: `unsignedRefusal` moves out; E-34), `internal/freshness/first_adoption_test.go` (E-26; the imports gain `log/slog`, `strings` and `sync/atomic`), `internal/freshness/unsigned.go` (`unsignedRefusal`, verbatim with Task 8's note; the file comment), `internal/handler/suite_hit_test.go` (D-36), `internal/handler/suite_dod_test.go` (D-40 to D-44; the `sync` import)

**Interfaces:**
- Consumes: nothing new.
- Produces: no API change: `(*Checker).ReportClockStep` and `(*Checker).unsignedRefusal` keep their signatures in their new files.

- [ ] **Step 1: Freshness moves and docs**

1. Move `ReportClockStep` (doc and body) from `suite_lock.go` to the end of `clockstep.go`, its doc gaining "Moved here from suite_lock.go beside clockStep (deferred minor D-25)."; `clockstep.go`'s import becomes `("time"; "github.com/linsomniac/apt-cacher-ultra/internal/cache")` and `suite_lock.go` drops `time`.
2. `HoldSuite`'s doc becomes:

```go
// HoldSuite takes the suite's mutex without waiting (cache.SuiteGuard): ok
// is false while anything else holds it — a check (or its busy peek), an
// adoption, repair or reconcile, or another HoldSuite (a racing cap
// eviction, the reaper, POST /forget) — and when lockSuite's re-validation
// finds the loaded mutex stale (another holder deleted the suite in the
// window; deferred minor D-25). release(true) — the suite's row was
// deleted while held — purges its flip-gate entry, then drops its lock-map
// entry and unlocks (unlockDropped); release(false) only unlocks. Spec C3
// cap eviction, the reaper and POST /forget use it (ruling R-F1).
```

and the two notes that named "phase F's reaper" name "the reaper, POST /forget" (`lockSuite`'s "(cap eviction through HoldSuite's release(true), a check or Reconcile that finds its row gone, the reaper, POST /forget)", its paragraph re-wrapped; `unlockDropped`'s "The reaper and POST /forget drop through this same helper.").
3. `clockStep`'s AIDEV-NOTE becomes:

```go
// AIDEV-NOTE: only PAST-EVENT stamps (last_check_at, last_success_at,
// last_requested_at, adoption_fail_first_at/last_at, adopted_at) are
// "infinitely old" when in the future — for a gate that starts work. A
// gate that retires things reads a future stamp as now instead, never as
// old: cap eviction and the reaper (last_requested_at), the orphan-marker
// sweep (suite_signed_seen.last_seen_at, read as now, never rewritten;
// rulings R-F5, PF-1) and the legacy junk grace (url_path.last_fetched_at,
// no evidence while in the future; not reported). The reaper and the
// sweep report the furthest such stamp at their first run after a start
// and when it changes (design review M-10), not every run. adopted_at
// may legitimately lead the clock since spec C8's monotonic stamp
// (max(now, prior + 1)); no gate reads it as a past event today, so a
// check that starts to must allow for that. adoption_next_retry_at is a
// FUTURE-event stamp: a detection is logged through this function, but the
// stamp is clamped to now+1h (retryGateFor), never treated as infinitely
// old.
```

4. Move `TestTick_QueuesAtMostMaxSuites`, `TestScheduler_FlushesAnEvictedWaiter` and `TestTick_BoundAdmitsTheLeastRecentlyChecked` (with their comments) verbatim from `suite_lock_test.go` to the end of `scheduler_test.go` (D-25). `scheduler_test.go` imports `slices`; `suite_lock_test.go` drops `fmt` and `slices`.
5. D-24: `TestCreateSuite_ReportsAFutureStampThroughTheChecker` ends:

```go
	// Deferred minor D-24: ReportClockStep passes three strings by
	// position; each must reach its own field.
	out := logs.String()
	if !strings.Contains(out, "clock_step_detected") || !strings.Contains(out, "stamp=last_requested_at") ||
		!strings.Contains(out, "canonical_scheme=http ") || !strings.Contains(out, "canonical_host=h ") ||
		!strings.Contains(out, "suite_path=/r/dists/x ") {
		t.Fatalf("log = %q; want clock_step_detected for http://h/r/dists/x's last_requested_at", out)
	}
}
```

6. E-49: `seedFakeSuite`'s doc becomes:

```go
// seedFakeSuite gives fc a suite with a cached InRelease anchor at
// http://<host><suite>/InRelease. Its client interest (spec C4) is
// lastSuccess, or — nil — the real wall clock's now: a checker whose now
// is more than the 7-day interest window from it sees the suite dormant,
// so such a test passes lastSuccess (deferred minor E-49).
```

7. E-33: move `unsignedRefusal` — doc, AIDEV-NOTE (as Task 8 left it) and body — verbatim from `first_adoption.go` to `unsigned.go`, before `markSignatureSeen`; `unsigned.go`'s file comment becomes:

```go
// Spec C5 "Unsigned safeguards" (D6), the adopter's side — and the
// checker's refusals before a slot is taken (unsignedRefusal, here since
// deferred minor E-33): the unsigned form adopts a Release no signature
// covers, so it never vouches a .deb, never counts for strict mode, never
// writes a signed-seen marker, and is refused whenever the suite has
// signed history.
```

8. E-34: `checkFirst`'s doc sentence becomes:

```go
// pending-change item; the latter has no record when it is queued, so it
// is not paced: at most one probe more, when a client check's failed
// adoption recorded one between the enqueue and the dequeue; deferred
// minor E-34). Otherwise the probe tree
```

9. E-26 (phase E design note 35 was unpinned: a mutant that fails the check on the read error survived): append to `first_adoption_test.go` (the imports gain `log/slog`, `strings` and `sync/atomic`):

```go
// lookupFailCache fails LookupURL while fail is set.
type lookupFailCache struct {
	Cache
	fail atomic.Bool
}

func (c *lookupFailCache) LookupURL(ctx context.Context, scheme, host, path string) (*cache.URLPath, error) {
	if c.fail.Load() {
		return nil, fmt.Errorf("injected url_path read failure (%s)", path)
	}
	return c.Cache.LookupURL(ctx, scheme, host, path)
}

// Deferred minor E-26 (phase E design note 35): a never-adopted suite whose
// anchor rows cannot be read is logged at WARN and left alone — no
// request, no write, no backoff (a local fault is no failed check) — and
// the next check, the fault gone, adopts it.
func TestCheckFirst_AnchorRowReadErrorWritesNothing(t *testing.T) {
	r := newRetryRig(t, 0)
	r.up.set(r.release("v1", 0))
	fc := &lookupFailCache{Cache: r.cache}
	fc.fail.Store(true)
	var logs lockedBuffer
	chk, err := New(Config{Cache: fc, Fetcher: r.up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute,
		Adopter: r.adopter, Logger: slog.New(slog.NewJSONHandler(&logs, nil)), now: r.clock.now, rand: func() float64 { return 0.5 }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	chk.Check(ctx, "http", rigHost, rigSuite)
	chk.WaitForAdoptions()
	warns := logRecords(t, &logs, "freshness: read anchor rows failed")
	if len(warns) != 1 || warns[0]["level"] != "WARN" || warns[0]["suite_path"] != rigSuite ||
		!strings.Contains(fmt.Sprint(warns[0]["err"]), "injected url_path read failure") {
		t.Fatalf("WARN records %v, want one naming the suite and the read error", warns)
	}
	if n := len(r.up.etagsSent(rigAnchorURL)); n != 0 {
		t.Fatalf("%d upstream requests, want none", n)
	}
	if row := r.row(); row.LastCheckAt != nil || row.CheckFailCount != 0 || row.AdoptionFailCount != 0 {
		t.Fatalf("the read error wrote the row: %+v", row)
	}
	fc.fail.Store(false)
	chk.Check(ctx, "http", rigHost, rigSuite)
	chk.WaitForAdoptions()
	if r.currentSnapshot() == nil {
		t.Fatal("the next check, the fault gone, did not adopt")
	}
}
```

- [ ] **Step 2: Handler test fixes**

1. D-36 (`TestTouchAsync_RecoversItsPanic`):

```go
	var stack string
	if len(recs) == 1 {
		stack, _ = recs[0]["stack"].(string) // deferred minor D-36: a missing stack fails, not panics
	}
	if len(recs) != 1 || recs[0]["where"] != "touch" || recs[0]["panic"] == nil || !strings.Contains(stack, "touchAsync") {
```

2. In `testMintedSuitesAtScale`: the `get` closure's parameter `url` becomes `target` (D-44: it shadowed `net/url`); after it, D-43's once-guarded closes:

```go
	// closeOnce closes h once, now or at cleanup, so a failing get never
	// leaves its T1 goroutines and sweep timers running while the cache
	// closes (deferred minor D-43).
	closeOnce := func(h *Handler) func() {
		once := sync.OnceFunc(h.Close)
		t.Cleanup(once)
		return once
	}
```

with `closeIdle := closeOnce(idle)` after `idle := newHandler()`, `closeH := closeOnce(h)` after `h := newHandler()`, and `closeIdle()` / `closeH()` where `idle.Close()` / `h.Close()` stood (the file imports `sync`). The marker loop's `c.SignedSeen(ctx, "http", host, suite)` passes `aptpath.MarkerBase(suite)` (D-44).
3. D-44, `firstByteProbe` keeps its read errors:

```go
type firstByteProbe struct {
	*httptest.ResponseRecorder
	c         *cache.Cache
	suite     string
	seen      bool
	row, mark bool
	err       error // the first read error (deferred minor D-44)
}

func (p *firstByteProbe) Write(b []byte) (int, error) {
	if !p.seen {
		p.seen = true
		var markErr error
		p.row, p.err = p.c.HasSuiteRow(context.Background(), cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "127.0.0.1", SuitePath: p.suite})
		_, p.mark, markErr = p.c.SignedSeen(context.Background(), "http", "127.0.0.1", aptpath.MarkerBase(p.suite))
		if p.err == nil {
			p.err = markErr
		}
	}
	return p.ResponseRecorder.Write(b)
}
```

and `TestServeHTTP_OnboardsADistsSuiteFromAV6Cache` checks `if p.err != nil || !p.row || !p.mark { t.Fatalf("at the first body byte row=%v marker=%v (read error %v), want both", p.row, p.mark, p.err) }`.
4. In `TestHitPathInvariant_EveryTrackedSignedAnchorHasAMarker`: `h2`'s config gains `MaxSuites: 5000` (D-42); `assertHitPathInvariant(t, r.c, db, "after c's Release.gpg hit")` follows c's `Release.gpg` check and `assertHitPathInvariant(t, r.c, db, "after d's adoption")` follows `commitInlineSnapshot` (D-41); the check labelled "after serving" is labelled "after d's reconstruction" (D-40).

- [ ] **Step 3: Run the tests**

Run: `go test -race ./internal/freshness/ ./internal/handler/`
Expected: PASS (`go vet` first: an unused or missing import is the usual slip of the moves).

- [ ] **Step 4: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| `ReportClockStep` passes the host and scheme swapped | `TestCreateSuite_ReportsAFutureStampThroughTheChecker` (`canonical_scheme=h canonical_host=http …`) |
| `checkFirst` fails the check on the anchor-row read error (`c.failCheck(ctx, out, cur, suite, nowUnix, "probe", err)` before its `return nil`) | `TestCheckFirst_AnchorRowReadErrorWritesNothing` (`the read error wrote the row: {… LastCheckAt:0x… CheckFailCount:1 …}`) |

The other items are moves, comments and failure-path hygiene (D-36, D-43, D-44's probe error) that no mutation of production code reaches.

- [ ] **Step 5: Commit**

```bash
gofmt -l internal/freshness/suite_lock.go internal/freshness/clockstep.go internal/freshness/suite_lock_test.go internal/freshness/scheduler_test.go internal/freshness/first_adoption.go internal/freshness/first_adoption_test.go internal/freshness/unsigned.go internal/handler/suite_hit_test.go internal/handler/suite_dod_test.go
git add internal/freshness/suite_lock.go internal/freshness/clockstep.go internal/freshness/suite_lock_test.go internal/freshness/scheduler_test.go internal/freshness/first_adoption.go internal/freshness/first_adoption_test.go internal/freshness/unsigned.go internal/handler/suite_hit_test.go internal/handler/suite_dod_test.go
git commit -m "Close deferred minors D-24, D-25, D-36, D-40-D-44, E-26, E-33 (first_adoption.go), E-34, E-49"
```

### Task 15: Definition of done — the 90-day simulation, production cadence, return and re-adoption, the escape hatch, self-heal, chaos, and the DoD map

The acceptance tests of spec §9's F row, spec §7's C9 list, the overview's "F:" line and its F carry-forwards, and the plan review's additions, over the code of Tasks 1–14. They are written after the code, so no step fails first: a DoD test that fails names a defect in an earlier task, fixed there. Three files:
- the simulation harness (design note 21), now with a cadence — the coarse one of deliberate deviation 2, or the production one — and the tests on it: the 90-day simulation (deviation 2), with a goroutine bound after every restart (design review M-6); the four-day production-cadence simulation the design review asked for (the reaper hourly, `gc.url_path_ttl = 168h`, a restart every 12 h: a suite whose deadline falls between a boot's last run and the restart is reaped at the next boot's floor, never at its start; a suite whose anchor rows url_path GC deleted days earlier is still reaped, its marker kept; goroutines back at the first boot's count after every restart); and the forward-step self-heal, now in two parts (design review M-8): a step across a restart reaps at the first run, as before; a step while the reaper runs is reported (`clock_step_detected`, `stamp=wall_clock`) and defers reaping by an uptime floor, after which the suites no client asked for go;
- the retry-rig tests (return before GC, `/forget` and rollback, `gc.enabled = false`, never-downgrade after a reap from a phase-A-binary marker, and the `unsigned_downgrade_refused` escape hatch with no other row, with the `/.` twin keeping a row, and — ruling PF-2 — with a signed `InRelease` cached under the other scheme, which `/forget` now deletes with the markers). `newRigReaper` and `clientFetchedRelease` live here now (Task 8's retirement tests, which defined them, are gone: ruling PF-5). The rig's clock jumps 31 days at once, so `reapRig` clears the reaper's monotonic reference first: for a daemon that lived through those days the monotonic clock would have moved as far, and the forward-step guard (Task 6) must not take the jump for a step;
- the reap-vs-adoption chaos test (Review Focus 5), now under a suite cap of three for four suites (design review M-6: creations refused at the cap race the reaps, forgets and adoptions; a refused client's suite stays untracked) and with a goroutine bound once everything has stopped.

The DoD map in Step 4 ties every requirement — the spec's, the overview's, and rulings PF-1..PF-9's — to its tests.

**Files:**
- Create: `internal/freshness/dod_f_test.go`, `internal/freshness/dod_f_rig_test.go`, `internal/freshness/dod_f_chaos_test.go`

**Interfaces:**
- Consumes: `NewReaper`, `ReaperConfig`, `(*Reaper).Run`, `runOnce`, `Forget` (`cache.ForgetResult`), the fields `interval`, `lastMono`, the constant `reapInterval` (Tasks 6, 10); `gc.New`, `gc.Config` (`URLPathTTL`, `HoldWindow`), `(*gc.GC).StartupPass`, `Run`; `RunLegacyTrustAudit`; `(*cache.Cache).CreateSuite` (`cache.SuiteCaps{Global}`, `cache.SuiteRejectedGlobal`), `TouchSuite`, `HasSuiteRow`, `GetSuiteFreshness`, `GetSuiteSnapshot`, `GetSnapshotMember`, `ListSnapshotMembers`, `ListSuitesWithAdoption`, `BlobExists`, `SignedSeen`, `MarkSignedSeenBatch`, `LookupURL`, `PutURLPath`, `ClearSignedSeenForTest`, `EnqueueLegacyTrustAudit`, `EnsureSuiteForTest`, `SeedSuiteForTest`, `TouchedSuites`, `Dir`; `(*Checker).HoldSuite`, `Check`, `WaitForAdoptions`, `LockCount`; `(*Adopter).writeBlobBytes`; `flipgate.New`, `(*flipgate.Gate).Len`; `wallclock.SetOffsetForTest`, `wallclock.Unix`; `simtest.ClearsignedFixture`, `simtest.OpenCache`, `simtest.StaticConditional`; freshness test helpers `newRetryRig` (`adopt`, `release`, `up`, `unsignedRelease`, `check`, `row`, `currentSnapshot`, `clock`, `adopter`, `logs`), `seedStaleAnchor`, `seedStaleAnchorAt`, `rigUpstream`, `newFakeFetcher`, `passThroughVerifier`, `fakePackagesStanzas`, `hashRelease`, `shaOf`, `hashOf`, `strOf`, `lockedBuffer`, `discardLogger`, `baseGoroutines`, `sampleGoroutines` (`sim_sched_test.go`), constants `rigHost`, `rigSuite`, `rigAnchorURL`, `rigReleaseURL`, `testHex64`.
- Produces: test helpers `simReapEvery`, `simCadence`, `coarseCadence`, `productionCadence`, `simBoot`, `startSimBoot(t, dir, members, logs) *simBoot`, `startSimBootAt(t, dir, members, logs, cad simCadence) *simBoot` (`stop`, `exists`, `use`, `count`), `simKey`, `simSuites`, `reapedLines` (`dod_f_test.go`); `reapAge`, `newRigReaper(r) *Reaper`, `(*retryRig).clientFetchedRelease(body)`, `reapRig(rp)`, `clientReturns()`, `retiredCount()`, `reCreateForRelease(rel)`, `downgradeRefusedAfterReap(t)` (`dod_f_rig_test.go`); `chaosCount`, `countMsgs` (`dod_f_chaos_test.go`).

- [ ] **Step 1: Write the DoD tests**

Create `internal/freshness/dod_f_test.go`:

```go
package freshness

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/gc"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// simReapEvery is the ninety-day simulation's reaper interval (deliberate
// deviation 2): six hours, not the hourly reapInterval, so ninety days fit
// the 5 s -race budget; the uptime floor stays one hour. Task 6 and
// TestDoD_ProductionCadenceAcrossTwelveHourlyRestarts pin the hourly
// cadence.
const simReapEvery = 6 * time.Hour

// simCadence is a simulation boot's reaper interval and gc.url_path_ttl.
type simCadence struct {
	reapEvery  time.Duration
	urlPathTTL time.Duration
}

var (
	// coarseCadence is deliberate deviation 2's: the reaper every six
	// hours, no url_path TTL pass.
	coarseCadence = simCadence{reapEvery: simReapEvery}
	// productionCadence is the shipped one: the reaper hourly
	// (reapInterval), gc.url_path_ttl at its default, 168h.
	productionCadence = simCadence{reapEvery: reapInterval, urlPathTTL: 168 * time.Hour}
)

// simBoot is one start of the suite lifecycle over the cache in dir: the
// adopter, the checker (whose scheduler does not run: the simulation's
// clients adopt through the adopter, as a T1 check would), the reaper (its
// one-hour floor, then every reapEvery of its cadence) and GC
// (gc.interval = 24h after its startup pass; the url_path TTL pass only at
// the production cadence, deliberate deviation 2) under one lifetime.
type simBoot struct {
	c       *cache.Cache
	ad      *Adopter
	checker *Checker
	reaper  *Reaper
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// startSimBoot starts a boot at the coarse cadence.
func startSimBoot(t *testing.T, dir string, members *fakeFetcher, logs *lockedBuffer) *simBoot {
	t.Helper()
	return startSimBootAt(t, dir, members, logs, coarseCadence)
}

// startSimBootAt starts a boot at cad.
func startSimBootAt(t *testing.T, dir string, members *fakeFetcher, logs *lockedBuffer, cad simCadence) *simBoot {
	t.Helper()
	ctx := context.Background()
	c, err := cache.Open(ctx, dir, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	b := &simBoot{c: c}
	if b.ad, err = NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
		HostLimiter: hostsem.New(8), Logger: discardLogger()}); err != nil {
		t.Fatal(err)
	}
	if b.checker, err = New(Config{Cache: c, Fetcher: &simtest.StaticConditional{Status: 304}, HostLimiter: hostsem.New(8),
		Adopter: b.ad, Logger: logger}); err != nil {
		t.Fatal(err)
	}
	if b.reaper, err = NewReaper(ReaperConfig{Cache: c, Checker: b.checker, IdleTTL: 30 * 24 * time.Hour, Logger: logger}); err != nil {
		t.Fatal(err)
	}
	b.reaper.interval = cad.reapEvery
	g, err := gc.New(gc.Config{Cache: c, Logger: discardLogger(), Enabled: true, Interval: 24 * time.Hour, BatchSize: 100,
		SnapshotBatchSize: 10, MaxTickDuration: 5 * time.Minute, BlobGrace: 5 * time.Minute, KeepDisplaced: 3,
		PoolScanWorkers: 1, HeartbeatStaleGrace: 30 * time.Minute, URLPathTTL: cad.urlPathTTL, HoldWindow: 24 * time.Hour,
		MaxVersionsPerPackage: 3})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.StartupPass(ctx); err != nil {
		t.Fatal(err)
	}
	life, cancel := context.WithCancel(ctx)
	b.cancel = cancel
	b.wg.Add(2)
	go func() { defer b.wg.Done(); b.reaper.Run(life) }()
	go func() { defer b.wg.Done(); g.Run(life) }()
	return b
}

// stop ends the boot: its loops, then the cache.
func (b *simBoot) stop(t *testing.T) {
	t.Helper()
	b.cancel()
	b.wg.Wait()
	if err := b.c.Close(); err != nil {
		t.Fatal(err)
	}
}

func simKey(suite string) cache.SuiteKey {
	return cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: suite}
}

// exists reports whether suite has a row.
func (b *simBoot) exists(t *testing.T, suite string) bool {
	t.Helper()
	ok, err := b.c.HasSuiteRow(context.Background(), simKey(suite))
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// use is a client's apt update of suite: a hit touches its row; a miss —
// the suite has no row — creates it, and its T1 check adopts the upstream
// InRelease at once. It reports whether the suite was (re)created.
func (b *simBoot) use(t *testing.T, suite string, inRelease []byte) bool {
	t.Helper()
	ctx := context.Background()
	key := simKey(suite)
	if b.exists(t, suite) {
		if err := b.c.TouchSuite(ctx, key); err != nil {
			t.Fatal(err)
		}
		return false
	}
	res, err := b.c.CreateSuite(ctx, cache.SuiteSeed{SuiteKey: key, RequestedAt: wallclock.Unix()}, cache.SuiteCaps{}, b.checker)
	if err != nil || res.Outcome != cache.SuiteCreated {
		t.Fatalf("%s: CreateSuite = %+v, %v", suite, res, err)
	}
	if err := b.ad.Run(ctx, SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: suite}, inRelease, "", ""); err != nil {
		t.Fatalf("%s: adoption: %v", suite, err)
	}
	return true
}

// count runs a COUNT query on its own connection to the boot's cache.
func (b *simBoot) count(t *testing.T, query string) int {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(b.c.Dir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// simSuites builds the suites' upstream: each a clearsigned InRelease of
// its own (an anchor blob no other suite shares) over one shared Packages.
func simSuites(members *fakeFetcher, suites ...string) map[string][]byte {
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo_1.deb": testHex64})
	out := map[string][]byte{}
	for _, s := range suites {
		members.put("http://h"+s+"/main/binary-amd64/Packages", pkgs)
		out[s] = simtest.ClearsignedFixture(string(hashRelease("Origin: "+s+"\n", true, false,
			map[string][]byte{"main/binary-amd64/Packages": pkgs}, nil)))
	}
	return out
}

// reapedLines counts the suite_reaped lines in logs by suite path.
func reapedLines(logs *lockedBuffer) map[string]int {
	out := map[string]int{}
	for _, line := range strings.Split(logs.String(), "\n") {
		var m map[string]any
		if line != "" && json.Unmarshal([]byte(line), &m) == nil && m["msg"] == "suite_reaped" {
			out[m["suite_path"].(string)]++
		}
	}
	return out
}

// Spec §7 C9, Review Focus 1: a 30-day forward step of the wall clock
// makes every suite look idle. Taken across a restart (the cache was off,
// or its clock was set while it was), the reaper's first run, an uptime
// floor after the start, reaps them all — false reaps. Taken while the
// reaper runs (a resumed VM or laptop), the run that sees it reports
// clock_step_detected (stamp=wall_clock) and reaps nothing: reaping waits
// an uptime floor more (design review M-8), and the suites no client
// asked for meanwhile go at the next run. Each false reap self-heals at
// its client's next request: the miss re-creates the row and the
// unchanged content re-adopts at once, and the markers, which outlive
// reaping, keep the suites' signed history.
func TestDoD_ForwardStepFalseReapsSelfHeal(t *testing.T) {
	for _, atRestart := range []bool{true, false} {
		name := "while running"
		if atRestart {
			name = "at a restart"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := context.Background()
				dir := t.TempDir()
				members := newFakeFetcher()
				suites := []string{"/r/dists/s0", "/r/dists/s1", "/r/dists/s2"}
				inRelease := simSuites(members, suites...)
				logs := &lockedBuffer{}
				b := startSimBoot(t, dir, members, logs)
				defer func() { b.stop(t) }()
				for _, s := range suites {
					b.use(t, s, inRelease[s])
				}
				if atRestart {
					b.stop(t)
				}
				restore := wallclock.SetOffsetForTest(30*24*time.Hour + time.Minute)
				defer restore()
				if atRestart {
					b = startSimBoot(t, dir, members, logs)
				}
				time.Sleep(time.Hour + time.Minute) // past the uptime floor: the first run
				if !atRestart {
					for _, s := range suites {
						if !b.exists(t, s) {
							t.Fatalf("%s reaped by the run that saw the forward step", s)
						}
					}
					if !strings.Contains(logs.String(), `"msg":"clock_step_detected","stamp":"wall_clock"`) {
						t.Fatal("no clock_step_detected line for the wall clock's step")
					}
					time.Sleep(simReapEvery) // the next run, past the deferral
				}
				for _, s := range suites {
					if b.exists(t, s) {
						t.Fatalf("%s survived the forward step", s)
					}
					if _, ok, err := b.c.SignedSeen(ctx, "http", "h", s); err != nil || !ok {
						t.Fatalf("%s's marker did not outlive the false reap (%v, %v)", s, ok, err)
					}
				}
				for _, s := range suites {
					if !b.use(t, s, inRelease[s]) {
						t.Fatalf("%s: the client's return found a row", s)
					}
					sf, err := b.c.GetSuiteFreshness(ctx, "http", "h", s)
					if err != nil || sf.CurrentSnapshotID == nil {
						t.Fatalf("%s: not re-adopted (%+v, %v)", s, sf, err)
					}
					snap, err := b.c.GetSuiteSnapshot(ctx, *sf.CurrentSnapshotID)
					if err != nil || strOf(snap.SingleAnchorHash) != shaOf(inRelease[s]) {
						t.Fatalf("%s: re-adopted %+v, %v; want its unchanged anchor", s, snap, err)
					}
				}
				if got := reapedLines(logs); len(got) != len(suites) {
					t.Fatalf("suite_reaped %v, want the three false reaps", got)
				}
			})
		})
	}
}

// Spec §7 C9, §9 F (deliberate deviation 2): ninety virtual days of the
// reaper and of GC at gc.interval = 24h, restarted every fifth day and an
// hour before three suites' deadlines (the uptime floor then delays their
// reap). Suites used daily are never reaped; one used again on day 29
// (near the TTL) lasts until day 59; a suite whose last use was day L is
// there on day L+30 and gone on day L+31 — a busy one too, whose check
// holds its mutex across the run that would reap it; its anchor blob is
// gone within two GC ticks; a reaped suite whose client returns re-adopts
// at once; a five-day backward clock step reaps nothing; at the end the
// suite tables and the in-memory maps hold the live suites only, and
// after every restart the goroutines are back at the first boot's count
// (design review M-6).
func TestDoD_NinetyDaysOfReapingWithRestarts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		members := newFakeFetcher()
		active := []string{"/r/dists/a0", "/r/dists/a1"}
		uses := map[string][]int{"/r/dists/b0": {0}, "/r/dists/b1": {0, 10}, "/r/dists/b2": {0, 20}, "/r/dists/n0": {0, 29}}
		last := map[string]int{"/r/dists/b0": 0, "/r/dists/b1": 10, "/r/dists/b2": 20, "/r/dists/n0": 29}
		const busy, returning, back = "/r/dists/b1", "/r/dists/c0", 45 // c0: used on day 0, back on day 45
		inRelease := simSuites(members, "/r/dists/a0", "/r/dists/a1", "/r/dists/b0", "/r/dists/b1", "/r/dists/b2",
			"/r/dists/n0", returning)
		logs := &lockedBuffer{}
		b := startSimBoot(t, dir, members, logs)
		defer func() { b.stop(t) }()
		base := baseGoroutines()
		restart := func() {
			b.stop(t)
			b = startSimBoot(t, dir, members, logs)
			if n := sampleGoroutines(base); n > base {
				t.Fatalf("after a restart: %d goroutines, the first boot had %d", n, base)
			}
		}
		for day := 0; day < 90; day++ {
			if day > 0 && day%5 == 0 { // 00:00, every fifth day
				restart()
			}
			time.Sleep(8 * time.Hour)
			if day == 30 || day == 40 || day == 50 { // 08:00, an hour before b0's, b1's and b2's deadlines
				restart()
			}
			time.Sleep(time.Hour)
			// 09:00: the clients.
			for _, s := range active {
				if created := b.use(t, s, inRelease[s]); created != (day == 0) {
					t.Fatalf("day %d: active suite %s created %v: reaped while in daily use", day, s, created)
				}
			}
			for s, days := range uses {
				for _, d := range days {
					if d == day {
						b.use(t, s, inRelease[s])
					}
				}
			}
			if day == 0 || day >= back {
				if created := b.use(t, returning, inRelease[returning]); created != (day == 0 || day == back) {
					t.Fatalf("day %d: %s created %v, want a row only on its first use and on its return", day, returning, created)
				}
			}
			if day == 31 && b.exists(t, returning) {
				t.Fatalf("day %d: %s not reaped", day, returning)
			}
			for s, l := range last {
				switch day {
				case l + 30:
					if !b.exists(t, s) {
						t.Fatalf("day %d: %s reaped 30 days after its last use, before the TTL", day, s)
					}
				case l + 31:
					if b.exists(t, s) {
						t.Fatalf("day %d: %s not reaped 31 days after its last use", day, s)
					}
				case l + 33:
					if ok, err := b.c.BlobExists(shaOf(inRelease[s])); err != nil || ok {
						t.Fatalf("day %d: %s's anchor blob outlived two GC ticks (%v, %v)", day, s, ok, err)
					}
				}
			}
			switch day {
			case last[busy] + 30:
				// The 08:00 restart puts the runs at 09:00 (the deadline:
				// not yet idle), 15:00 and 21:00. The busy suite's check
				// holds its mutex from 14:30 to 16:00, across the 15:00
				// run; the 21:00 run reaps it.
				time.Sleep(5*time.Hour + 30*time.Minute)
				release, ok := b.checker.HoldSuite(simKey(busy))
				if !ok {
					t.Fatalf("day %d: %s busy", day, busy)
				}
				time.Sleep(90 * time.Minute)
				if !b.exists(t, busy) {
					t.Fatalf("day %d: a held suite was reaped", day)
				}
				release(false)
				time.Sleep(8 * time.Hour)
			case 61:
				// 12:00: the wall clock steps five days back for twelve hours.
				time.Sleep(3 * time.Hour)
				restore := wallclock.SetOffsetForTest(-5 * 24 * time.Hour)
				time.Sleep(12 * time.Hour)
				restore()
			default:
				time.Sleep(15 * time.Hour)
			}
		}
		live := len(active) + 1
		if n := b.count(t, `SELECT COUNT(*) FROM suite_freshness`); n != live {
			t.Errorf("%d suite rows, want the %d live suites'", n, live)
		}
		if n := b.count(t, `SELECT COUNT(*) FROM suite_snapshot`); n != live {
			t.Errorf("%d suite_snapshot rows, want the %d live suites' current ones", n, live)
		}
		if b.checker.LockCount() > live || b.c.TouchedSuites() > live {
			t.Errorf("lock map %d, touch map %d entries; want at most %d", b.checker.LockCount(), b.c.TouchedSuites(), live)
		}
		want := map[string]int{"/r/dists/b0": 1, "/r/dists/b1": 1, "/r/dists/b2": 1, "/r/dists/n0": 1, returning: 1}
		got := reapedLines(logs)
		if len(got) != len(want) {
			t.Fatalf("suite_reaped %v, want once each for %v", got, want)
		}
		for s := range want {
			if got[s] != 1 {
				t.Fatalf("suite_reaped %v, want once each for %v", got, want)
			}
		}
	})
}

// Design review (deliberate deviation 2's other half): four virtual days
// at the production cadence — the reaper hourly after its one-hour floor,
// gc.url_path_ttl = 168h, a restart every twelve hours. A suite whose
// deadline falls 30 minutes before a restart, after the last run of the
// boot before, is reaped at the next boot's floor, never at its start; a
// suite whose anchor rows url_path GC deleted days
// before its deadline is still reaped, its marker kept; a suite in daily
// use is never reaped; after every restart the goroutines are back at the
// first boot's count.
func TestDoD_ProductionCadenceAcrossTwelveHourlyRestarts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		dir := t.TempDir()
		members := newFakeFetcher()
		const active, edge, gcd = "/r/dists/active", "/r/dists/edge", "/r/dists/gcd"
		inRelease := simSuites(members, active)
		logs := &lockedBuffer{}
		start := time.Now()
		ttl := 30 * 24 * time.Hour
		b := startSimBootAt(t, dir, members, logs, productionCadence)
		defer func() { b.stop(t) }()
		base := baseGoroutines()
		seed := func(suite string, requested time.Time) {
			t.Helper()
			if err := b.c.EnsureSuiteForTest(ctx, "http", "h", suite); err != nil {
				t.Fatal(err)
			}
			at := requested.Unix()
			if err := b.c.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h",
				SuitePath: suite, LastRequestedAt: &at}); err != nil {
				t.Fatal(err)
			}
		}
		// edge's deadline: 35 h 30 m, between the last run before the
		// restart at 36 h (35 h) and that restart.
		seed(edge, start.Add(35*time.Hour+30*time.Minute-ttl))
		// gcd's deadline: 54 h; its cached InRelease, unused for 8 days,
		// is past url_path_ttl, and its base was last seen signed then.
		seed(gcd, start.Add(54*time.Hour-ttl))
		old := start.Add(-8 * 24 * time.Hour).Unix()
		h, err := b.ad.writeBlobBytes(ctx, simtest.ClearsignedFixture("Origin: gcd\nSHA256:\n"))
		if err != nil {
			t.Fatal(err)
		}
		if err := b.c.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: gcd + "/InRelease",
			BlobHash: &h, UpstreamURL: "http://h" + gcd + "/InRelease", IsMetadata: true, RequestCount: 1,
			LastRequestedAt: &old, LastFetchedAt: &old}); err != nil {
			t.Fatal(err)
		}
		if err := b.c.MarkSignedSeenBatch(ctx, []cache.SignedSeenMark{{CanonicalScheme: "http", CanonicalHost: "h",
			Base: gcd, SeenAt: old}}); err != nil {
			t.Fatal(err)
		}
		gcdAnchor := func() bool {
			t.Helper()
			_, err := b.c.LookupURL(ctx, "http", "h", gcd+"/InRelease")
			if err != nil && !errors.Is(err, cache.ErrNotFound) {
				t.Fatal(err)
			}
			return err == nil
		}
		for half := 0; half < 8; half++ { // twelve hours each
			if half > 0 {
				b.stop(t)
				b = startSimBootAt(t, dir, members, logs, productionCadence)
				if n := sampleGoroutines(base); n > base {
					t.Fatalf("after the restart at %d h: %d goroutines, the first boot had %d", 12*half, n, base)
				}
			}
			for step := 0; step < 2; step++ { // the active suite's client, every six hours
				b.use(t, active, inRelease[active])
				switch {
				case half == 2 && step == 1: // 30 h; then up to 35 h 59 m: the boot's last run, at 35 h
					time.Sleep(5*time.Hour + 59*time.Minute)
					if !b.exists(t, edge) {
						t.Fatalf("%s reaped before its deadline", edge)
					}
					time.Sleep(time.Minute)
				case half == 3 && step == 0: // 36 h: the restart; the floor's run at 37 h
					time.Sleep(time.Hour - time.Minute)
					if !b.exists(t, edge) {
						t.Fatalf("%s reaped before the boot's uptime floor", edge)
					}
					time.Sleep(2 * time.Minute)
					if b.exists(t, edge) {
						t.Fatalf("%s not reaped at the boot's floor, 90 minutes past its deadline", edge)
					}
					time.Sleep(5*time.Hour - time.Minute)
				case half == 4 && step == 0: // 48 h: url_path GC has deleted gcd's anchor; gcd is still tracked
					if gcdAnchor() || !b.exists(t, gcd) {
						t.Fatalf("at 48 h: %s's anchor row %v, its row %v; want the anchor deleted by url_path GC, the row kept",
							gcd, gcdAnchor(), b.exists(t, gcd))
					}
					time.Sleep(6 * time.Hour)
				case half == 4 && step == 1: // 54 h: the deadline; the run at 55 h reaps it
					time.Sleep(30 * time.Minute)
					if !b.exists(t, gcd) {
						t.Fatalf("%s reaped before its deadline", gcd)
					}
					time.Sleep(time.Hour)
					if b.exists(t, gcd) {
						t.Fatalf("%s not reaped although url_path GC took its anchor", gcd)
					}
					if at, ok, err := b.c.SignedSeen(ctx, "http", "h", gcd); err != nil || !ok || at < old {
						t.Fatalf("%s's marker after the reap: %d, %v, %v; want it kept", gcd, at, ok, err)
					}
					time.Sleep(4*time.Hour + 30*time.Minute)
				default:
					time.Sleep(6 * time.Hour)
				}
			}
		}
		if !b.exists(t, active) {
			t.Fatalf("%s, in use every six hours, was reaped", active)
		}
		if got := reapedLines(logs); len(got) != 2 || got[edge] != 1 || got[gcd] != 1 {
			t.Fatalf("suite_reaped %v, want %s and %s once each", got, edge, gcd)
		}
	})
}
```

Create `internal/freshness/dod_f_rig_test.go`:

```go
package freshness

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/gc"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

const reapAge = 31 * 24 * time.Hour // past gc.suite_idle_ttl's 30 days

// newRigReaper is a reaper over the retry rig's cache and checker (whose
// adopter holds the adopt_unsigned knob), logging to the rig's logs.
func newRigReaper(r *retryRig) *Reaper {
	r.t.Helper()
	rp, err := NewReaper(ReaperConfig{Cache: r.cache, Checker: r.checker, IdleTTL: 30 * 24 * time.Hour,
		Logger: slog.New(slog.NewJSONHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	if err != nil {
		r.t.Fatal(err)
	}
	return rp
}

// clientFetchedRelease stands in for a client fetching the rig suite's
// Release from upstream: the url_path row the handler's miss stores.
func (r *retryRig) clientFetchedRelease(body []byte) {
	r.t.Helper()
	ctx := context.Background()
	h, err := r.adopter.writeBlobBytes(ctx, body)
	if err != nil {
		r.t.Fatal(err)
	}
	now := r.clock.now().Unix()
	if err := r.cache.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: rigHost,
		Path: rigSuite + "/Release", BlobHash: &h, UpstreamURL: rigReleaseURL, IsMetadata: true,
		RequestCount: 1, LastRequestedAt: &now, LastFetchedAt: &now}); err != nil {
		r.t.Fatal(err)
	}
}

// reapRig ages the rig suite past the TTL and runs the reaper once; the
// suite must be gone after. The rig's clock jumps the 31 days at once;
// for a daemon that lived through them the monotonic clock would have
// moved as far, so the forward-step guard (design review M-8) is told
// so: its last monotonic reading is cleared.
func (r *retryRig) reapRig(rp *Reaper) {
	r.t.Helper()
	r.clock.advance(reapAge)
	rp.lastMono = time.Time{}
	rp.runOnce(context.Background())
	if _, err := r.cache.GetSuiteFreshness(context.Background(), "http", rigHost, rigSuite); err == nil {
		r.t.Fatal("the idle rig suite was not reaped")
	}
}

// clientReturns stands in for a client's next request after a reap or a
// forget: the anchor miss re-creates the suite's row, and the anchor's
// url_path row — InRelease, with a stale blob, so the check sees a change
// — is the client-created anchor the first adoption needs.
func (r *retryRig) clientReturns() {
	r.t.Helper()
	res, err := r.cache.CreateSuite(context.Background(), cache.SuiteSeed{SuiteKey: cache.SuiteKey{CanonicalScheme: "http",
		CanonicalHost: rigHost, SuitePath: rigSuite}, RequestedAt: r.clock.now().Unix()}, cache.SuiteCaps{}, nil)
	if err != nil || res.Outcome != cache.SuiteCreated {
		r.t.Fatalf("re-creation = %+v, %v", res, err)
	}
	seedStaleAnchor(r.t, r.cache)
}

// retiredCount is the number of retired snapshots of the rig suite.
func (r *retryRig) retiredCount() int {
	r.t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(r.cache.Dir(), "cache.db"))
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM suite_snapshot WHERE suite_path = ? AND retired_at IS NOT NULL`,
		rigSuite).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

// Spec §7 C9: a reaped suite whose client returns before any GC tick
// re-adopts its unchanged upstream content within one check — the retired
// snapshot with the same natural key refuses nothing — and the GC pass
// that then deletes the retired snapshot leaves the new one whole.
func TestDoD_ReturnBeforeGCReadoptsWithinOneCheck(t *testing.T) {
	ctx := context.Background()
	r := newRetryRig(t, time.Minute)
	rp := newRigReaper(r)
	r.adopt()
	old := r.currentSnapshot()
	r.reapRig(rp)
	r.clientReturns()
	r.check()
	snap := r.currentSnapshot()
	if snap == nil || snap.SnapshotID == old.SnapshotID || strOf(snap.SingleAnchorHash) != strOf(old.SingleAnchorHash) {
		t.Fatalf("after one check: snapshot %+v; want the unchanged anchor adopted as a new snapshot (old %d)", snap, old.SnapshotID)
	}
	g, err := gc.New(gc.Config{Cache: r.cache, Logger: discardLogger(), Enabled: true, Interval: 24 * time.Hour, BatchSize: 100,
		SnapshotBatchSize: 10, MaxTickDuration: 5 * time.Minute, BlobGrace: time.Nanosecond, KeepDisplaced: 3,
		PoolScanWorkers: 1, HeartbeatStaleGrace: 30 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.StartupPass(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.cache.GetSuiteSnapshot(ctx, old.SnapshotID); err == nil {
		t.Fatal("the retired snapshot survived the GC pass")
	}
	m, err := r.cache.GetSnapshotMember(ctx, snap.SnapshotID, "main/binary-amd64/Packages")
	if err != nil {
		t.Fatalf("the new snapshot lost its Packages member: %v", err)
	}
	for _, h := range []string{m.BlobHash, strOf(snap.SingleAnchorHash)} {
		if ok, err := r.cache.BlobExists(h); err != nil || !ok {
			t.Fatalf("blob %s of the new snapshot: exists %v, %v", h, ok, err)
		}
	}
}

// Spec §7 C9 (D9): POST /forget then the client's next request re-adopts
// — after an upstream rollback that the adopted history refused, the
// older content; then, forgotten again, the unchanged content at once.
func TestDoD_ForgetThenRequestReadoptsAndRollbackAdopts(t *testing.T) {
	ctx := context.Background()
	r := newRetryRig(t, time.Minute)
	rp := newRigReaper(r)
	d1, d2 := r.clock.now().Add(-2*time.Hour).Unix(), r.clock.now().Add(-time.Hour).Unix()
	v1, v2 := r.release("v1", d1), r.release("v2", d2)
	r.up.set(v1)
	r.check()
	r.up.set(v2)
	r.clock.advance(2 * time.Minute)
	r.check()
	if strOf(r.currentSnapshot().SingleAnchorHash) != hashOf(v2) {
		t.Fatal("premise: v2 is not current")
	}
	r.up.set(v1) // the upstream rolls back
	r.clock.advance(2 * time.Minute)
	r.check()
	if strOf(r.currentSnapshot().SingleAnchorHash) != hashOf(v2) {
		t.Fatal("the rollback was adopted before /forget")
	}
	for i := range 2 {
		if res, err := rp.Forget(ctx, "http", rigHost, rigSuite); err != nil || !res.Deleted {
			t.Fatalf("forget %d = %+v, %v", i, res, err)
		}
		r.clientReturns()
		r.clock.advance(2 * time.Minute)
		r.check()
		if snap := r.currentSnapshot(); snap == nil || strOf(snap.SingleAnchorHash) != hashOf(v1) {
			t.Fatalf("after forget %d and one check: snapshot %+v; want v1 adopted", i, snap)
		}
	}
}

// Spec §7 C9: with gc.enabled = false nothing deletes retired snapshots,
// yet a suite reaped twice re-adopts its unchanged content each time: the
// retired snapshots that share its natural key refuse nothing.
func TestDoD_GCDisabledStillReadopts(t *testing.T) {
	r := newRetryRig(t, time.Minute)
	rp := newRigReaper(r)
	r.adopt()
	anchor := strOf(r.currentSnapshot().SingleAnchorHash)
	for i := range 2 {
		r.reapRig(rp)
		r.clientReturns()
		r.check()
		if snap := r.currentSnapshot(); snap == nil || strOf(snap.SingleAnchorHash) != anchor {
			t.Fatalf("return %d: snapshot %+v; want the unchanged anchor re-adopted", i, snap)
		}
	}
	if n := r.retiredCount(); n != 2 {
		t.Fatalf("%d retired snapshots, want the 2 reaped ones, kept (no GC)", n)
	}
}

// downgradeRefusedAfterReap leaves the rig suite as the overview's F line
// describes: adopted signed under the phase-A binary (no marker of its own
// commit; the startup pass marks it), reaped by the F binary, back with an
// unsigned upstream and refused as unsigned_downgrade_refused. It returns
// the reaper and the unsigned Release.
func downgradeRefusedAfterReap(t *testing.T) (*retryRig, *Reaper, []byte) {
	t.Helper()
	ctx := context.Background()
	r := newRetryRig(t, time.Minute)
	rp := newRigReaper(r)
	r.adopt()
	id := *r.row().CurrentSnapshotID
	if err := r.cache.ClearSignedSeenForTest(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.cache.EnqueueLegacyTrustAudit(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := RunLegacyTrustAudit(ctx, r.cache, discardLogger()); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := r.cache.SignedSeen(ctx, "http", rigHost, rigSuite); err != nil || !ok {
		t.Fatalf("premise: the startup pass wrote no marker (%v, %v)", ok, err)
	}
	r.reapRig(rp)
	if _, ok, err := r.cache.SignedSeen(ctx, "http", rigHost, rigSuite); err != nil || !ok {
		t.Fatalf("the reap took the marker (%v, %v): markers outlive reaping", ok, err)
	}
	r.up.mu.Lock()
	delete(r.up.bodies, rigAnchorURL) // the vendor stops signing: InRelease 404
	r.up.mu.Unlock()
	rel := r.unsignedRelease("u1", nil)
	r.adopter.adoptUnsigned = true
	r.reCreateForRelease(rel)
	r.check()
	if row := r.row(); row.CurrentSnapshotID != nil || strOf(row.AdoptionFailReason) != cache.ReasonUnsignedDowngradeRefused {
		t.Fatalf("back unsigned: adopted %v, record %q; want unsigned_downgrade_refused", row.CurrentSnapshotID != nil,
			strOf(row.AdoptionFailReason))
	}
	return r, rp, rel
}

// reCreateForRelease stands in for a client of an unsigned upstream after
// a reap or a forget: InRelease answers 404, so the Release miss re-creates
// the row (an adopter is wired) and is the client-created anchor.
func (r *retryRig) reCreateForRelease(rel []byte) {
	r.t.Helper()
	res, err := r.cache.CreateSuite(context.Background(), cache.SuiteSeed{SuiteKey: cache.SuiteKey{CanonicalScheme: "http",
		CanonicalHost: rigHost, SuitePath: rigSuite}, RequestedAt: r.clock.now().Unix()}, cache.SuiteCaps{}, nil)
	if err != nil || res.Outcome != cache.SuiteCreated {
		r.t.Fatalf("re-creation = %+v, %v", res, err)
	}
	r.clientFetchedRelease(rel)
}

// Overview F line, spec §7 C9: a signed snapshot adopted under the
// phase-A binary, reaped by the F binary, returns with an unsigned
// upstream: never-downgrade still refuses it — markers outlive reaping —
// at every later check.
func TestDoD_NeverDowngradeAfterReap(t *testing.T) {
	r, _, _ := downgradeRefusedAfterReap(t)
	for range 3 {
		r.clock.advance(time.Hour)
		r.check()
		if row := r.row(); row.CurrentSnapshotID != nil {
			t.Fatal("an unsigned snapshot was adopted for a suite with signed history")
		}
	}
}

// Phase E carry-forward (spec C5 threat notes): POST /forget is the
// escape hatch for unsigned_downgrade_refused. With no other suite row for
// the base, /forget deletes its markers and the suite adopts unsigned
// after the transition's two observations; with the /. twin keeping a
// row, the markers stay and the suite stays refused. Ruling PF-2: a signed
// InRelease still cached under the other scheme, with no suite row there,
// goes with the markers — left in place, the unsigned commit's re-mark
// (ruling R-E1) would read it and mark the base again, keeping the hatch
// shut with no row left to forget.
func TestDoD_ForgetIsTheEscapeHatchForADowngradeRefusal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(context.Context, *retryRig)
		twin  bool // the markers stay and the suite stays refused
	}{
		{name: "no other row"},
		{name: "the twin keeps a row", twin: true, setup: func(ctx context.Context, r *retryRig) {
			if err := r.cache.EnsureSuiteForTest(ctx, "http", rigHost, rigSuite+"/."); err != nil {
				r.t.Fatal(err)
			}
		}},
		{name: "the other scheme cached a signed anchor", setup: func(ctx context.Context, r *retryRig) {
			h, err := r.adopter.writeBlobBytes(ctx, simtest.ClearsignedFixture("Origin: signed\nSHA256:\n"))
			if err != nil {
				r.t.Fatal(err)
			}
			if err := r.cache.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "https", CanonicalHost: rigHost,
				Path: rigSuite + "/InRelease", BlobHash: &h, UpstreamURL: "https://" + rigHost + rigSuite + "/InRelease",
				IsMetadata: true, RequestCount: 1}); err != nil {
				r.t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r, rp, rel := downgradeRefusedAfterReap(t)
			if tc.setup != nil {
				tc.setup(ctx, r)
			}
			if res, err := rp.Forget(ctx, "http", rigHost, rigSuite); err != nil || !res.Deleted {
				t.Fatalf("forget = %+v, %v", res, err)
			}
			if _, marked, err := r.cache.SignedSeen(ctx, "http", rigHost, rigSuite); err != nil || marked != tc.twin {
				t.Fatalf("after /forget: marker %v (%v); want %v", marked, err, tc.twin)
			}
			r.reCreateForRelease(rel)
			r.clock.advance(time.Minute)
			r.check()
			r.clock.advance(16 * time.Minute)
			r.check()
			row := r.row()
			switch {
			case tc.twin && (row.CurrentSnapshotID != nil || strOf(row.AdoptionFailReason) != cache.ReasonUnsignedDowngradeRefused):
				t.Fatalf("twin kept: adopted %v, record %q; want still refused", row.CurrentSnapshotID != nil, strOf(row.AdoptionFailReason))
			case !tc.twin:
				if snap := r.currentSnapshot(); snap == nil || snap.Form != cache.FormUnsigned || strOf(snap.SingleAnchorHash) != hashOf(rel) {
					t.Fatalf("snapshot %+v, record %q; want the unsigned Release adopted", snap, strOf(row.AdoptionFailReason))
				}
			}
		})
	}
}
```

Create `internal/freshness/dod_f_chaos_test.go`:

```go
package freshness

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// chaosCount runs a COUNT query on its own connection to c.
func chaosCount(t *testing.T, c *cache.Cache, query string) int {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(c.Dir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// countMsgs counts logs' JSON records by msg.
func countMsgs(logs *lockedBuffer) map[string]int {
	out := map[string]int{}
	for _, line := range strings.Split(logs.String(), "\n") {
		var m map[string]any
		if line != "" && json.Unmarshal([]byte(line), &m) == nil {
			if msg, ok := m["msg"].(string); ok {
				out[msg]++
			}
		}
	}
	return out
}

// Review Focus 5, spec §7 C9's reap-vs-adoption chaos: for two virtual
// days, clients come and go on four suites (creating, touching and
// checking them) under a suite cap of three (design review M-6: a fourth
// suite's creation is refused, or evicts an idle one, while a suite is
// reaped or forgotten), the upstream publishes new content that their
// checks adopt behind a flip barrier, an operator forgets suites, and the
// reaper runs hourly with a three-hour TTL, all at once. Nothing
// deadlocks or panics; afterwards every current snapshot is adopted and
// not retired, every snapshot of a suite without a row is retired, every
// current snapshot's member blobs are there, the table holds at most
// three rows, and the lock map, the flip gate and the touch map hold no
// more entries than there are suite rows; once everything has stopped the
// goroutines are back at the count before the run. The run reaped, forgot,
// refused at the cap and re-adopted suites (lower bounds).
func TestDoD_ReapRacesAdoptionsAndClients(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		c := simtest.OpenCache(t)
		gate, err := flipgate.New(flipgate.Config{Quiet: 10 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		logs := &lockedBuffer{}
		logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
		members := newFakeFetcher()
		up := &rigUpstream{bodies: map[string][]byte{}, sent: map[string][]string{}}
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
			HostLimiter: hostsem.New(8), MaxConcurrent: 2, Logger: logger, FlipGate: gate})
		if err != nil {
			t.Fatal(err)
		}
		life, stop := context.WithCancel(ctx)
		ck, err := New(Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8), Cooldown: time.Minute,
			Refresh: 15 * time.Minute, Adopter: ad, LifetimeCtx: life, Logger: logger})
		if err != nil {
			t.Fatal(err)
		}
		rp, err := NewReaper(ReaperConfig{Cache: c, Checker: ck, IdleTTL: 3 * time.Hour, Logger: logger})
		if err != nil {
			t.Fatal(err)
		}
		suites := []string{"/r/dists/s0", "/r/dists/s1", "/r/dists/s2", "/r/dists/s3"}
		caps := cache.SuiteCaps{Global: 3}
		var refused atomic.Int64
		var pubMu sync.Mutex
		publish := func(s string, gen int) {
			pubMu.Lock()
			defer pubMu.Unlock()
			pkgs := fakePackagesStanzas(map[string]string{fmt.Sprintf("pool/main/f/foo_%d.deb", gen): testHex64})
			members.put("http://h"+s+"/main/binary-amd64/Packages", pkgs)
			up.setAt("http://h"+s+"/InRelease", simtest.ClearsignedFixture(string(hashRelease(
				fmt.Sprintf("Origin: %s generation %d\n", s, gen), true, false,
				map[string][]byte{"main/binary-amd64/Packages": pkgs}, nil))))
		}
		for _, s := range suites {
			publish(s, 0)
		}
		var wg sync.WaitGroup
		spawn := func(f func()) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				f()
			}()
		}
		sleep := func(d time.Duration) bool {
			select {
			case <-life.Done():
				return false
			case <-time.After(d):
				return true
			}
		}
		base := baseGoroutines()
		spawn(func() { rp.Run(life) })
		for i, s := range suites { // the clients: a suite's client returns every 20 minutes to 5 hours
			rng := rand.New(rand.NewPCG(uint64(i), 1))
			spawn(func() {
				key := cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: s}
				for sleep(time.Duration(20+rng.IntN(280)) * time.Minute) {
					ok, err := c.HasSuiteRow(ctx, key)
					if err != nil {
						t.Error(err)
						return
					}
					if ok {
						_ = c.TouchSuite(ctx, key)
					} else {
						res, err := c.CreateSuite(ctx, cache.SuiteSeed{SuiteKey: key, RequestedAt: time.Now().Unix()}, caps, ck)
						if err != nil {
							t.Error(err)
							return
						}
						if res.Outcome == cache.SuiteRejectedGlobal {
							refused.Add(1) // served untracked: no row, so no anchor row to seed or suite to check
							continue
						}
						seedStaleAnchorAt(t, c, "h", s)
					}
					ck.Check(ctx, "http", "h", s) // its T1 check
				}
			})
		}
		spawn(func() { // the upstream publishes a new generation of a suite every five hours
			rng := rand.New(rand.NewPCG(7, 7))
			for gen := 1; sleep(5 * time.Hour); gen++ {
				publish(suites[rng.IntN(len(suites))], gen)
			}
		})
		spawn(func() { // an operator forgets a suite every seven hours
			rng := rand.New(rand.NewPCG(9, 9))
			for sleep(7 * time.Hour) {
				if _, err := rp.Forget(ctx, "http", "h", suites[rng.IntN(len(suites))]); err != nil {
					t.Error(err)
				}
			}
		})
		time.Sleep(48 * time.Hour)
		stop()
		wg.Wait()
		ck.WaitForAdoptions()
		time.Sleep(time.Minute) // the barrier's quiet periods end; idle gate entries are sweepable
		for _, q := range []struct{ what, sql string }{
			{"current snapshots retired or never adopted", `SELECT COUNT(*) FROM suite_freshness sf JOIN suite_snapshot ss
  ON ss.snapshot_id = sf.current_snapshot_id WHERE ss.retired_at IS NOT NULL OR ss.adopted_at IS NULL`},
			{"current pointers to no snapshot", `SELECT COUNT(*) FROM suite_freshness WHERE current_snapshot_id IS NOT NULL
  AND current_snapshot_id NOT IN (SELECT snapshot_id FROM suite_snapshot)`},
			{"live snapshots of suites without a row", `SELECT COUNT(*) FROM suite_snapshot ss WHERE ss.retired_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM suite_freshness sf WHERE sf.canonical_scheme = ss.canonical_scheme
  AND sf.canonical_host = ss.canonical_host AND sf.suite_path = ss.suite_path)`},
		} {
			if n := chaosCount(t, c, q.sql); n != 0 {
				t.Errorf("%d %s", n, q.what)
			}
		}
		rows, err := c.ListSuitesWithAdoption(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range rows {
			if s.CurrentSnapshotID == nil {
				continue
			}
			ms, err := c.ListSnapshotMembers(ctx, *s.CurrentSnapshotID)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range ms {
				if ok, err := c.BlobExists(m.BlobHash); err != nil || !ok {
					t.Errorf("%s: member %s's blob is gone (%v, %v)", s.SuitePath, m.Path, ok, err)
				}
			}
		}
		if n := len(rows); n > caps.Global || ck.LockCount() > n || gate.Len() > n || c.TouchedSuites() > n {
			t.Errorf("%d rows (cap %d); lock map %d, flip gate %d, touch map %d entries", n, caps.Global, ck.LockCount(),
				gate.Len(), c.TouchedSuites())
		}
		if n := sampleGoroutines(base); n > base {
			t.Errorf("%d goroutines after the run, %d before it", n, base)
		}
		got := countMsgs(logs)
		if got["suite_reaped"] == 0 || got["suite_forgotten"] == 0 || refused.Load() == 0 || got["adoption_success"] <= len(suites) {
			t.Errorf("reaps %d, forgets %d, refusals at the cap %d, adoptions %d; want reaps, forgets, refusals and re-adoptions",
				got["suite_reaped"], got["suite_forgotten"], refused.Load(), got["adoption_success"])
		}
	})
}
```

- [ ] **Step 2: Run them, and their budget**

Run: `go test -race ./internal/freshness/ -run 'TestDoD_(NinetyDays|ProductionCadence|ForwardStep|ReturnBeforeGC|ForgetThen|GCDisabled|NeverDowngrade|ForgetIsTheEscapeHatch|ReapRaces)' -count=3 -v`
Expected: PASS, every run under 4.5 s (PE-8). Measured under `-race` at load average 1.2–1.5: `TestDoD_NinetyDaysOfReapingWithRestarts` 3.27–3.38 s (at the production cadence for all ninety days it took 12.5–12.8 s: deliberate deviation 2); `TestDoD_ReapRacesAdoptionsAndClients` 1.67–1.97 s; `TestDoD_ProductionCadenceAcrossTwelveHourlyRestarts` 0.83–0.98 s; `TestDoD_ForwardStepFalseReapsSelfHeal` (both parts) 0.79–0.85 s; `TestDoD_ForgetIsTheEscapeHatchForADowngradeRefusal` (three cases) 0.95–1.03 s; the other retry-rig tests 0.3–0.4 s each.

- [ ] **Step 3: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| `retireSuiteSnapshotsTx` retires only the current snapshot (`AND snapshot_id = ?`) | `TestDoD_ForgetThenRequestReadoptsAndRollbackAdopts` (`after forget 0 and one check: snapshot <nil>; want v1 adopted`: the displaced v1, left live, refuses the rollback) |
| `ReapIdleSuite` deletes the base's markers (its stamping `UPDATE` made a `DELETE`) | `TestDoD_NeverDowngradeAfterReap`, `TestDoD_ForgetIsTheEscapeHatchForADowngradeRefusal` (`the reap took the marker`) |
| `ForgetSuite`'s other-row check ignores the twin (`base, base`) | `TestDoD_ForgetIsTheEscapeHatchForADowngradeRefusal/the_twin_keeps_a_row` (`after /forget: marker false; want true`) |
| snapshot-GC sub-job C does nothing (`RunRetiredSnapshotGCChunk` not called, `internal/gc/snapshot.go`) | `TestDoD_ReturnBeforeGCReadoptsWithinOneCheck` (`the retired snapshot survived the GC pass`), `TestDoD_NinetyDaysOfReapingWithRestarts` (`day 33: /r/dists/b0's anchor blob outlived two GC ticks`) |
| PF-2: `ForgetSuite` deletes no anchor rows | `TestDoD_ForgetIsTheEscapeHatchForADowngradeRefusal/the_other_scheme_cached_a_signed_anchor` (`snapshot <nil>, record "unsigned_downgrade_refused"`) |
| M-8: the forward-step guard never defers | `TestDoD_ForwardStepFalseReapsSelfHeal/while_running` (`/r/dists/s0 reaped by the run that saw the forward step`) |
| `reapUptimeFloor = 2 * time.Hour` | `TestDoD_ProductionCadenceAcrossTwelveHourlyRestarts` (`/r/dists/edge not reaped at the boot's floor`) |
| no uptime floor (`Run`'s first timer `0`) | `TestDoD_ProductionCadenceAcrossTwelveHourlyRestarts` (`/r/dists/edge reaped before the boot's uptime floor`) |
| M-6: a goroutine leaked per boot (in `startSimBootAt`, the harness's own check) | `TestDoD_NinetyDaysOfReapingWithRestarts` (`after a restart: 10 goroutines, the first boot had 9`) |
| M-6: a goroutine leaked per reaper run (`Run`) | `TestDoD_ReapRacesAdoptionsAndClients` (`54 goroutines after the run, 6 before it`) |

- [ ] **Step 4: The DoD map**

| Requirement | Tests | Task |
|---|---|---|
| §9 F: 90-day simulation at `gc.interval = 24h` with restarts (touched, idle, near-TTL, busy, clock jump; §7), goroutines bounded per boot (design review M-6) | `TestDoD_NinetyDaysOfReapingWithRestarts` (deviation 2) | 15 |
| §7 at the production cadence (the design review's addition to deviation 2): hourly runs, the floor across restarts, `gc.url_path_ttl = 168h` taking a suite's anchors before its reap | `TestDoD_ProductionCadenceAcrossTwelveHourlyRestarts` | 15 |
| §9 F, §7: return before GC re-adopts within one check | `TestDoD_ReturnBeforeGCReadoptsWithinOneCheck` | 15 |
| §9 F, §7: `/forget` then a request re-adopts; after an upstream rollback, the older content | `TestDoD_ForgetThenRequestReadoptsAndRollbackAdopts`; `TestReaper_ForgetDeletesASuiteRegardlessOfIdleness` | 15, 10 |
| §9 F, §7: a forward step's false reaps self-heal; a step while running defers reaping by the floor (design review M-8) | `TestDoD_ForwardStepFalseReapsSelfHeal` (both parts), `TestReaper_AForwardStepDefersReapingByTheFloor`, `TestReaper_RunsFarApartAreNoForwardStep`, `TestReaper_AForwardStepDefersTheMarkerSweep` | 15, 6, 9 |
| §9 F, §7, overview F line: never-downgrade after a reap (a phase-A-binary signed snapshot, its marker from the startup pass); a reap moves the base's markers up (design review M-9) | `TestDoD_NeverDowngradeAfterReap`, `TestReapIdleSuite_MovesTheBaseMarkersUpToTheReap` | 15, 3 |
| §9 F, §7: `/forget` CSRF — a cross-site form or fetch cannot trigger it (ruling PF-6; DNS rebinding is spec §10 G-1's) | `TestEndpoint_Forget_RefusesCrossOriginRequests` | 10 |
| §9 F, §7: twins and nested suites untouched | `TestReap_LeavesTwinsAndNestedSuitesAlone`, `TestDeleteSuiteMetadata_DeletesOnlyTheSuitesRows`, `TestSweepOrphanMarkers_SweepsOnlyYearOldOrphans`, `TestForgetSuite_ATwinPathDeletesItsBasesMarkers` | 3, 9 |
| §7: `gc.enabled = false` still re-adopts | `TestDoD_GCDisabledStillReadopts` | 15 |
| §7: batched metadata deletion never repeats the decrement | `TestDeleteSuiteMetadata_NeverRepeatsADecrement`, `TestDeleteSuiteMetadata_BatchesOfAThousandAnchorsLast`, `TestReapIdleSuite_ReleasesOnlyTheCurrentSnapshotsPins` | 3 |
| §7: reap-vs-adoption chaos, under the suite cap, goroutines bounded (design review M-6) | `TestDoD_ReapRacesAdoptionsAndClients` | 15 |
| §7: blobs reclaimed within two GC ticks | `TestDoD_NinetyDaysOfReapingWithRestarts` (day L+33), `TestRunTick_SubJobCReapsRetiredSnapshotsAndTheirBlobs` | 15, 4 |
| Overview F line: `ReapIdleSuite`, `POST /forget`; the refcount release reuses `decrementSnapshotRefcountsTx` (`RetireSuiteSnapshots` dropped with R-F4's retirement, ruling PF-5) | `TestReapIdleSuite_RetiresEverySnapshotAndDeletesTheRow`, `TestEndpoint_Forget` | 3, 10 |
| Overview F line, A carry-forward: non-canonical rows reaped regardless of idleness, first (ruling PF-4); `/foo/dists/InRelease` by idleness | `TestReaper_ReapsIdleAndNonCanonicalSuitesAndPurgesThem`, `TestReaperCandidates_NullAndFutureStampsAreNeverIdle`, `TestReaperCandidates_ReapedRegardlessGoFirst` | 6 |
| Overview F line, A carry-forward: retired snapshots leave `keep_displaced`; sub-job C deletes audit-retired ones | `TestRunSnapshotGCBatch_RetiredSnapshotsTakeNoKeepDisplacedSlot`, `TestRunRetiredSnapshotGCChunk_DeletesAuditRetiredSnapshots`, `…_DeletesRetiredSnapshotsInChunks`, `…_TheChunkIsTenThousandRows` | 4 |
| Overview F line, R-F5 with ruling PF-1, E carry-forward: the marker sweep (a year, no row for the base or twin, either scheme, re-checked in the writer; a future stamp read as now, never rewritten, reported when it changes) | `TestSweepOrphanMarkers_SweepsOnlyYearOldOrphans`, `…_NeverRewritesAFutureStamp`, `…_BackwardStepThenCorrectionSweepsNothing`, `TestDeleteOrphanMarkers_RechecksInTheWriter`, `TestReaper_SweepsOrphanMarkersAndKeepsFutureStamps`, `TestReaper_TheMarkerTTLIsAYear`, `TestReaper_ReportsAFutureMarkerOnlyWhenItChanges`, `TestCheck_EverySightingAdvancesTheMarkerStamp` | 9 |
| B carry-forward: a reaped suite's parked scheduler entry flushes | `TestReaper_AReapedWaiterFlushesOnItsNextOffer` | 6 |
| C, D carry-forwards, R-F1: the purge (lock map, flip gate, touch) through the one delete path; only the reaper and `/forget` call the deleters (design review M-5) | `TestReaper_ReapsIdleAndNonCanonicalSuitesAndPurgesThem`, `TestReaper_ForgetDeletesASuiteRegardlessOfIdleness`, `TestReaper_DropsTheLockEntryOfASuiteGoneSinceTheListing`, `TestReaper_AKeptSuiteKeepsItsLockEntry`, `TestSuiteFreshnessHasOneDeleteSite`, `TestSuiteRowWritersHaveKnownCallers`, `TestSuiteDeletersHaveKnownCallers`, `TestSignedSeenHasKnownDeleteSites` | 2, 3, 6, 9, 10 |
| D carry-forward, R-F2: `Gate.Purge` skips live entries | `TestGate_PurgeSkipsLiveEntries`, `TestGate_PurgeKeepsTheEntryOfAFlipInPhaseA`, `TestHoldSuite_ReleaseKeepsARecreatedSuitesLiveGateEntry` | 1 |
| D carry-forward, R-F3 with rulings PF-3, PF-4: legacy junk reaped every run, first, whatever its record; a plain-text Release kept by content; a transient soft-404 kept | `TestReaper_ReapsLegacyJunkKeepsTransientSoft404`, `TestReaper_RemembersJunkVerdictsByBlob`, `TestReaper_TheJunkGraceIsADay`, `TestReaper_JunkIsNotStarvedByAnIdleBacklog`, `TestReaper_JunkReadFailuresLogOncePerScan`, `TestListUnadoptedSuiteAnchors_ProbesURLPathByItsKey` | 7 |
| E carry-forward PE-12, ruling PF-5: with `adopt_unsigned = false` an established unsigned suite keeps its coherent snapshot (refused, visible, paced) | `TestServeHTTP_KnobOffKeepsServingTheUnsignedSnapshot`, `TestCheck_UnsignedSuiteWithTheKnobOffIsAPacedRefusal` (phase E) | 8 |
| E carry-forward, ruling PF-2: the `unsigned_downgrade_refused` escape hatch with no other row, with a `/.` twin row, and with a signed anchor cached under the other scheme | `TestDoD_ForgetIsTheEscapeHatchForADowngradeRefusal`, `TestForgetSuite_DeletesTheBaseMarkersOnlyWithoutAnotherSuite` | 15, 3 |
| Ruling PF-7: `/forget`'s replies say what happened; the status page shows the scheme | `TestEndpoint_Forget`, `TestReaper_ForgetAnswersFalseForBusyAndUnknown`, `TestStatus_SuitesShowTheirScheme` | 10 |
| Design review M-2: bounded held work, a failed submit settled by a fresh read | `TestReaper_BoundsTheWorkUnderAHoldAtFiveMinutes`, `TestReaper_ForgetBoundsItsWorkAtFiveMinutes`, `TestReapAndForget_AFailedSubmitWhoseDeleteCommittedIsGone`, `TestReaper_ForgetDropsTheLockEntryOfASuiteGoneUnderTheHold` | 3, 6, 10 |
| Design review M-3, M-4, M-10, M-16: shutdown at DEBUG; one line per failed scan; future stamps reported when they change; `acu_suites_reaped_total{reason}` | `TestReaper_AShutdownMidRunIsNoFailure`, `TestReaper_JunkReadFailuresLogOncePerScan`, `TestReaperCandidates_ReportsAFutureStampOnlyWhenItChanges`, `TestReaper_ReapsIdleAndNonCanonicalSuitesAndPurgesThem`, `TestReaper_ReapsLegacyJunkKeepsTransientSoft404` | 6, 7, 9 |
| D, E carry-forwards: the "(a later release)" texts | Task 16's grep | 16 |

- [ ] **Step 5: Commit**

```bash
gofmt -l internal/freshness/dod_f_test.go internal/freshness/dod_f_rig_test.go internal/freshness/dod_f_chaos_test.go
git add internal/freshness/dod_f_test.go internal/freshness/dod_f_rig_test.go internal/freshness/dod_f_chaos_test.go
git commit -m "Phase F definition of done: the 90-day and production-cadence simulations, return and re-adoption, the escape hatch, self-heal and chaos"
```

(Record the mutation table and the measured simulation times in the commit message.)

### Task 16: Documentation, spec revision 9, overview, deferred files, final verification

Spec §8 for this phase — the operator documentation of suite reaping (`gc.suite_idle_ttl` and *Suite reaping* under `[gc]`), of `POST /forget` (*Forgetting a suite* under `[admin]`), of what an upgrade reaps, and of the events and the metrics C11 lists — then the rulings R-F1–R-F5 and PF-1–PF-9, deviations 2 and 3 and the design notes the spec should state, written into the spec as revision 9; the overview's phase F bookkeeping and its carry-forwards for G and H; and the deferred files. The `adopt_unsigned` row and safeguard 1 are Task 8's (ruling PF-5's honest semantics); `packaging/config/config.toml.default` was Task 11's. The rulings' docs, each exact against the code:
- PF-1: no marker stamp is rewritten — spec C3, C4's clock-step rule and C9, and `clock_step_detected`'s `last_seen_at` text in `docs/log-fields.md` (deviation 3);
- PF-2: `/forget` also deletes the cached anchors of the base and its `/.` twin under both schemes with the markers — *Forgetting a suite*, `suite_forgotten`'s `anchor_rows`, spec C3 and C9;
- PF-3, PF-4: the content-based junk rule, the junk scan every run and the candidates reaped regardless of idleness first — *Suite reaping*, `suite_reaped`, spec C9; the exec review's observation that a non-canonical row's cached files are left to `gc.url_path_ttl` (`metadata_rows` 0) is stated in both;
- PF-5: spec C5 safeguard 1; the overview marks phase E's PE-12 carry-forward resolved by PF-5; `unsigned_snapshot_retired` and every other trace of the retirement are gone;
- PF-6: honest CSRF wording ("a cross-site form or `fetch` cannot trigger it; it does not stop DNS rebinding against a listener without `htpasswd_file`") in *Forgetting a suite* and spec C9; Host-header validation for `/forget` and `/reconcile` added to spec §10's G-1 item and to the overview's open decisions;
- PF-7: the `/forget` answers (the `curl` example now uses `--fail-with-body` so a 409's text shows) and the status page's scheme, with `docs/admin-ui-spec.md`'s additive `scheme` row;
- PF-8: the forward-step guard (*Suite reaping*, `clock_step_detected`'s `stamp=wall_clock`, spec C9), a reap stamping its base's markers (spec C3, `signed_seen_markers_swept`, *Suite reaping*), `acu_suites_reaped_total{reason}` (log-fields' metrics paragraph, spec C11), and M-11 — reaping with `gc.enabled = false` (the `enabled` row, *Upgrading an existing cache*), what a reaped suite needs (upstream), the offline cache's `"0s"`, and a reap's effect on strict mode; M-7's one-update residual of forgetting an active suite;
- M-13: C-6 joins E-32's category (a site in a file F changes, its fix elsewhere) and its entry says so; M-17: a new G carry-forward (flat index rows as junk evidence).

Every "(a later release)" text the D and E carry-forwards name is rewritten (`docs/configuration.md`: the caps paragraph, the threat notes; `docs/log-fields.md`: `suite_caps_exceeded`; `docs/admin-ui-spec.md`: the unsigned refusals). `README.md` is H's (spec §8; a new H carry-forward), as phase E left its README items to H.

Deferred minors in the files this task changes: E-63 (its spec half: the 91-column line in spec C10). Left in these files: C-36, C-37, D-50, D-52, D-55, E-24, E-75 (H); E-36 and E-61 (their fixes are code in `adoption_failure.go` and `inflight.go`, which no task changes; the docs only describe that code); E-76 (the phase E plan).

**Every deferred minor, as this plan leaves it.** Taken: C-1, C-2, C-3, C-4, D-39 (Task 1); D-8, D-11, D-12, D-13, D-14, D-15, D-20 (Task 2); A-3 (Task 4); C-11, E-2 (its `internal/cache/adoption.go` half), E-4, E-5 (Task 5); E-18, E-20, E-40 (its `types.go` part) (Task 9); E-72 (Task 11); E-6, E-7, E-8, E-12, E-15 (Task 13); D-24, D-25, D-36, D-40, D-41, D-42, D-43, D-44, E-26, E-33 (its `first_adoption.go` half), E-34, E-49 (Task 14); E-63 (its spec half; this task). Checked and left:
- tagged for another phase: D-3, D-4, D-32, E-9, E-16, E-19 (G); A-56, C-9, C-21, C-36, C-37, C-38, C-39, D-50, D-52, D-55, D-56, D-57, D-58, E-24, E-46, E-75 (H);
- in files phase F changes only by a mechanical rename (ruling PD-1): E-2's and E-63's `internal/handler/handler.go` halves; E-31 (`Reconcile` in `freshness.go`, `baseline` in `check.go`), E-44 (`freshness.go`; its entry gains a note: phase F adds the `reaper` kind), E-45 (`freshness.go`, `scheduler.go`), E-40's `freshness_test.go` part; E-67, E-68 (`internal/handler/suite_rows.go`);
- with a site in a file phase F changes by hand but a fix that belongs to files it does not change: C-6 (`busyReaders` is in `gate_test.go`, which Task 1 changes; the fix belongs to `flipgate/sim_test.go`, simtest and the handler tests — its entry gains that note; design review M-13), E-32 (one of the hold predicate's six sites is in `first_adoption.go`; the helper and the other sites are `freshness.go`, `check.go` and `current_form.go` — its entry gains that note), E-33's `hostCounter` half (`current_form_test.go`), E-40's `scheduler.go` part, E-13 (its WARN lives in `flip_barrier.go`, not in `adoption.go`, which Task 13 changes: the entry's file name is corrected here);
- in files no task changes: A-1, A-2, A-4, A-5, A-6, A-8 to A-15, A-19, A-20, A-21, A-26, A-27, A-55; B-9 to B-14, B-27, B-36, B-52, B-59 to B-63; C-5, C-7, C-8, C-12, C-13; D-6, D-7, D-9, D-10, D-16 to D-19, D-26 to D-31, D-46, D-48, D-49, D-60; E-1, E-3, E-10, E-11, E-14, E-17, E-21, E-22, E-23, E-25, E-27 to E-30, E-35, E-36, E-37, E-38, E-39, E-41, E-42, E-43, E-47, E-48, E-50 to E-62, E-64 to E-66, E-69, E-70, E-71, E-73, E-74, E-76, E-77.

**Files:**
- Modify: `docs/configuration.md`, `docs/log-fields.md`, `docs/admin-ui-spec.md`, `docs/superpowers/specs/2026-09-27-flat-repositories-design.md`, `docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md`, `…-A-deferred.md`, `…-C-deferred.md`, `…-D-deferred.md`, `…-E-deferred.md`

**Interfaces:**
- Consumes: the names, events, metrics and constants Tasks 1–15 produce, exactly as they produce them.
- Produces: no code.

- [ ] **Step 1: Operator documentation (`docs/configuration.md`)**

The caps paragraph and *Tracked suites* point at the reaper; the threat notes point at *Forgetting a suite*; *Upgrading an existing cache* says what the reaper deletes after an upgrade (spec C9's migration backfill: about 23 days for a suite with no client anchor request), with GC off too, and what an offline cache sets first; `[gc]` gains the `suite_idle_ttl` row, its `enabled` row says the reaper runs regardless, and *Suite reaping* is added; `[admin]` names `POST /forget` and gains *Forgetting a suite*, with the guard's honest scope and a `curl` example in `/reconcile`'s README style:

```diff
diff --git a/docs/configuration.md b/docs/configuration.md
--- a/docs/configuration.md
+++ b/docs/configuration.md
@@ -251,8 +251,9 @@ host can also get there by normal growth). Above the total cap new suites
 are refused, but each refused attempt still evicts one suite that qualifies
 (no adopted snapshot, not requested for 24 hours), so the table shrinks
 back under the cap as suites fall idle. A host at its own cap refuses that
-host's new suites and evicts nothing for them; its suites go when idle-suite
-reaping (a later release) removes those not requested for 30 days, or when
+host's new suites and evicts nothing for them; its suites go when the suite
+reaper removes those not requested for `gc.suite_idle_ttl` (30 days by
+default; see *Suite reaping* under `[gc]`), or when
 a new suite of any host at the total cap evicts one of its idle suites (no
 adopted snapshot, not requested for 24 hours). A table
 at exactly the total cap logs no warning: that is a full cache working
@@ -261,7 +262,9 @@ none qualifies (WARN `suite_seed_rejected`).
 
 Serving a tracked suite's `InRelease`, `Release` or `Release.gpg`, or a
 `.deb` its current snapshot vouches for, records that the suite is in use
-(at most one write per suite per hour). A by-hash request with no cached
+(at most one write per suite per hour); a suite not in use for
+`gc.suite_idle_ttl` (30 days by default) is no longer tracked (see *Suite
+reaping* under `[gc]`). A by-hash request with no cached
 copy under its own path is answered from an identical cached file only for
 a tracked suite; otherwise it is fetched from upstream.
 
@@ -491,8 +494,8 @@ anyone who can make the cache see one signed-looking `InRelease` or
 `Release.gpg` for a repository path — served to a client, or found by a
 check; a man in the middle, a compromised upstream, or a LAN client
 pointing the cache at a host it controls — blocks unsigned adoption of
-that path until its suite is forgotten (`POST /forget`, added with suite
-reaping in a later release). That denial of service is the accepted price
+that path until its suite is forgotten (`POST /forget`, see *Forgetting a
+suite* under `[admin]`). That denial of service is the accepted price
 of never downgrading. A man in the middle between the cache and upstream
 can make the cache adopt forged unsigned content only for a suite never
 seen signed — for a suite not yet adopted only after two consistent
@@ -585,7 +588,16 @@ cached anchor was already garbage-collected leaves no record. Suites whose
 their signed-seen record,
 on their first cache hit. A cache with more than `freshness.max_suites`
 suites, or a tenth of it or more on one host, logs WARN
-`suite_caps_exceeded` at start; see *Tracked suites*.
+`suite_caps_exceeded` at start; see *Tracked suites*. From an hour after
+the start the suite reaper (see *Suite reaping* under `[gc]`) deletes the
+suites no client has asked for in `gc.suite_idle_ttl`, counted from the
+last request for one of their anchor files before the upgrade (from 7 days
+before the upgrade for a suite with none: such a suite goes about 23 days
+after the upgrade unless a client asks for it), the rows an earlier
+release created for paths this version no longer tracks, and the suites
+whose cached anchor files are all error pages — with `gc.enabled = false`
+too. An offline or archive cache that must keep serving such suites sets
+`gc.suite_idle_ttl = "0s"` before upgrading.
 
 After the upgrade, suites without a snapshot adopt eagerly only if a client
 fetched one of their anchor files in the 7 days before the upgrade, or asks
@@ -667,7 +679,7 @@ served an unvouched package.
 
 | Key | Type | Default | Behavior and constraints |
 | --- | --- | --- | --- |
-| `enabled` | boolean | `true` | Run startup and periodic garbage collection, including the startup orphan-file scan. `false` disables those passes and logs a warning. |
+| `enabled` | boolean | `true` | Run startup and periodic garbage collection, including the startup orphan-file scan. `false` disables those passes and logs a warning. It does not stop the suite reaper, which deletes idle suites' cached index files whatever this says (see *Suite reaping*; `suite_idle_ttl = "0s"` turns it off). |
 | `interval` | duration | `"24h"` | Delay after a GC tick completes before the next periodic tick. Must be positive; disable through `enabled`. |
 | `batch_size` | integer | `100` | Maximum URL-path/blob rows processed per GC batch. At least `1`. |
 | `snapshot_batch_size` | integer | `10` | Maximum snapshots removed per batch. Snapshot deletion can cascade through many member rows. At least `1`. |
@@ -677,6 +689,7 @@ served an unvouched package.
 | `pool_scan_workers` | integer | `4` | Workers for the startup orphan-file scan. At least `1`. |
 | `heartbeat_interval` | duration | `"60s"` | Refresh in-progress adoption liveness and blob protection. Positive and subject to both bounds below. |
 | `url_path_ttl` | duration | `"168h"` | Requests within this window protect cached URL rows. Older or never-requested rows are evaluated against snapshot/version retention and hold grace. `"0s"` disables URL-path expiration; other values must be at least `"1m"`. |
+| `suite_idle_ttl` | duration | `"720h"` | A tracked suite no client has asked for in this long is reaped: no longer tracked, its cached index files and snapshots deleted (see *Suite reaping*). `"0s"` disables suite reaping; other values must be at least `"24h"`. |
 
 Startup still runs the orphan-file scan and a GC tick. Periodic ticks wait a full
 `interval` after the previous tick completes, so a long run cannot trigger an
@@ -701,12 +714,71 @@ The second expression is the implementation's stale-adoption grace formula;
 it is separate from the actual fetch retry budget. All `[gc]` validation still
 applies when `enabled = false`.
 
+### Suite reaping
+
+The suite reaper deletes the tracked suites (see *Tracked suites*) that no
+client has asked for in `suite_idle_ttl`: a client asks for a suite when it
+fetches the suite's `InRelease`, `Release` or `Release.gpg`, or a `.deb`
+its snapshot vouches for, through the cache. The reaper runs an hour after
+the daemon starts and then every hour, whatever `interval` and `enabled`
+say. A run deletes at most 25 suites — first those reaped however
+recently they were asked for (below), then those asked for least
+recently; a suite being checked or adopted at that moment is skipped and
+tried again at the next run. Reaping a suite deletes its cached index
+files (its `InRelease`, `Release`, `Packages` and the rest of its `dists/`
+tree, in batches of 1,000 files; packages are not touched), retires its
+snapshots and deletes its row (INFO `suite_reaped`;
+`acu_suites_reaped_total{reason}` counts the reaps). The next GC passes
+delete the retired snapshots, and then the pool files nothing else uses,
+once `blob_grace` has passed; with `enabled = false` suites are still
+reaped, but their snapshots and pool files stay until GC runs again.
+
+A reaped suite whose clients come back is tracked again at their first
+request and, with adoption enabled, adopted again at its first check
+(unchanged upstream content included; an unsigned repository after the
+two observations of its first adoption). Its files then come from
+upstream again: a cache that must keep serving suites nobody asks for
+for a while — an offline or archive cache — sets `suite_idle_ttl = "0s"`.
+The record that a repository path was seen signed survives the reap, so
+the returning suite is never adopted unsigned (see *Anchor forms and
+unsigned repositories*); the reaper deletes that record only when no
+suite of the path has a row and the path was last seen signed, or its
+last suite reaped, more than a year ago. With
+`integrity.refuse_unvouched_debs = true`, a reaped suite's packages are
+no longer vouched for, and reaping a suite whose signed snapshot was the
+host's only one with incomplete package coverage turns strict mode on for
+that host, as long as the host keeps another signed snapshot (see the
+strict-mode notes under `[integrity]`).
+
+Two kinds of suites are reaped however recently they were asked for, at
+every run: rows an earlier release created for paths this version no
+longer tracks (`reason=non_canonical`; such a path's cached files are
+left to URL-row expiry, `url_path_ttl`), and suites without a snapshot
+whose cached `InRelease`, `Release` and `Release.gpg` are all something
+else, such as an HTML error page (`reason=junk`), typically rows an
+earlier release created. A suite of the second kind is reaped only when
+every such file was fetched at least a day earlier, so a repository that
+answered with an error page for a few hours is kept, and so is one whose
+cached `InRelease` is a plain unsigned `Release` (the cache adopts such a
+repository through its `Release`); a file that cannot be read is never
+taken for an error page.
+
+A suite whose last request lies in the future (the clock was stepped
+back) is never idle. A forward step can make every suite look idle: the
+hour before the first run after a start gives clients time to ask again;
+a step while the daemon runs (a resumed virtual machine or laptop: the
+wall clock moved more than an hour further than the time that passed) is
+logged as WARN `clock_step_detected` with `stamp=wall_clock` and holds
+the reaper's deletions back for an hour; a suite reaped anyway comes back
+at its next request, as above. `"0s"` turns suite reaping off; the reaper
+then only deletes the year-old records of signed paths no suite tracks.
+
 ## `[admin]`: status, metrics, and maintenance
 
 The separate **HTTP** admin listener serves the status page, `/metrics`, and
-`/healthz`, plus the mutating `POST /reconcile` maintenance endpoint. It is not a
-read-only interface. Keep access restricted to trusted operators; if exposed
-beyond loopback, protect the network path or place it behind a TLS reverse proxy.
+`/healthz`, plus the mutating `POST /reconcile` and `POST /forget` maintenance
+endpoints. It is not a read-only interface. Keep access restricted to trusted
+operators; if exposed beyond loopback, protect the network path or place it behind a TLS reverse proxy.
 HTTP Basic authentication does not encrypt credentials in transit.
 
 | Key | Type | Default | Behavior and constraints |
@@ -736,6 +808,50 @@ command-line, trace, or other debug routes are exposed.
 See the [idle CPU investigation](idle-cpu-review.md) for findings, maintenance
 metrics, tuning tradeoffs, and profile-capture commands.
 
+### Forgetting a suite (`POST /forget`)
+
+`POST /forget` reaps one tracked suite at once, however recently it was
+used (see *Suite reaping* under `[gc]`): its cached index files, its
+snapshots and its row are deleted. The next request for it tracks it again
+as a new repository and, with adoption enabled, adopts what upstream
+serves then. That is the way out of two refusal states: a deliberate
+upstream rollback (refused `upstream_older` or `rollback_refused`), and a
+vendor that stopped signing (`unsigned_downgrade_refused`). For the latter
+it also deletes the record that the repository path was seen signed, and
+the cached `InRelease`, `Release` and `Release.gpg` of the path and of its
+`./` twin under either scheme (a signed copy left there would mark the
+path signed again), but only when no other suite of the path has a row —
+under `http` or `https`, or as its `./` twin; while one does, the answer
+names it: forget it too, or the forgotten suite is refused again. A
+client in the middle of an update when its suite is forgotten can see one
+Hash Sum mismatch: forgetting does not wait for the update to end, as an
+adoption's flip does (see *Flip consistency*).
+
+`scheme`, `host` and `suite` are the suite's canonical scheme, host and
+suite path as the status page shows them (its Host column reads
+`scheme://host`); `scheme` is optional (`http` or `https`, default
+`https`). The request must carry the header `X-ACU-Admin: 1`, which a
+cross-site form cannot send, and a browser request from another site is
+refused (by its `Sec-Fetch-Site` header), so a cross-site form or `fetch`
+cannot trigger it. It does not stop DNS rebinding — a web page whose own
+name is made to resolve to the admin listener's address — against a
+listener without `htpasswd_file`: configure one if a browser runs on the
+cache's host. `curl` and scripts are not affected:
+
+```sh
+curl -sS --fail-with-body -X POST http://127.0.0.1:6789/forget -H 'X-ACU-Admin: 1' \
+    --data-urlencode 'host=packages.microsoft.com' \
+    --data-urlencode 'suite=/ubuntu/24.04/prod/dists/noble'
+# 202 Accepted = forgotten (the work is done when the answer comes); the body
+#   names the suite, the signed-path records deleted and the suites that kept them
+# 409 = busy (being checked or adopted: try again) or no such suite (check the
+#   scheme, which defaults to https, and the host and suite the status page shows)
+# 403 = the header is missing, or a cross-site browser request
+```
+
+Each request is logged as INFO `admin_forget_requested`, and each suite
+forgotten as INFO `suite_forgotten`.
+
 ## `[serve]`: stale responses
 
 | Key | Type | Default | Behavior and constraints |
```

- [ ] **Step 2: Events and the metrics (`docs/log-fields.md`)**

```diff
diff --git a/docs/log-fields.md b/docs/log-fields.md
--- a/docs/log-fields.md
+++ b/docs/log-fields.md
@@ -203,7 +203,13 @@ slot was free; they run on a later unchanged check) and
 `acu_strict_adoption_waits_total{host}` (unvouched `.deb` requests that
 waited — at most 15 s, and never past 25 s after the request arrived — for
 a signed adoption running on their host before strict mode decided; the
-wait is part of the request line's `duration_ms`).
+wait is part of the request line's `duration_ms`). Suite reaping (spec C9)
+adds the counter `acu_suites_reaped_total{reason}` (suites the suite
+reaper deleted, by `reason`: `idle`, `non_canonical`, `junk`; see
+`suite_reaped`) and the counter `acu_gc_retired_reaped_total` (retired
+snapshots that snapshot GC deleted: those of reaped, forgotten and
+evicted suites, and those the upgrade's trust audit quarantined), whose
+per-pass count the `gc_run_complete` line carries as `retired_reaped`.
 
 The `freshness_check` line's `result` gains `changed_deferred` (upstream
 still serves content whose adoption failed or was refused; the field
@@ -249,13 +255,14 @@ Structured events emitted outside the per-request `request` log line:
 goroutine panic recovery, freshness checks that end unknown, adoption
 failure and refusal records and their retry schedule, wall-clock step
 detection, the startup legacy trust audit, the flip barrier, suite rows
-(cap refusals and evictions) and a slow shutdown drain (logged by
-`internal/freshness`, `internal/handler` and the daemon's `main`).
+(cap refusals, evictions, reaping and `POST /forget`) and a slow shutdown
+drain (logged by `internal/freshness`, `internal/handler`, `internal/admin`
+and the daemon's `main`).
 
 | Event | Level | Fields | Meaning |
 |---|---|---|---|
 | `adoption_panicked` | ERROR | `where` (`adoption`, `repair`, `reconcile`, `heartbeat`, `record`), `canonical_host`, `suite_path`, `panic`, `stack` (first occurrence per `where` per process only) | A panic inside an adoption, repair, reconcile, heartbeat or failure-record goroutine was recovered; the prior snapshot keeps serving. Only an adoption panic is recorded as a failure (reason `panic`); repair and reconcile panics are logged, not recorded (plan deviation 7). `where=record` is `recordAdoptionFailure`, which writes the spec C7 failure record after an adoption ends. |
-| `freshness_check_panicked` | ERROR | `where` (`check`, `scheduler`, `reschedule`, `t1`), `canonical_host`, `suite_path`, `panic`, `stack` (first occurrence per `where` per process only) | A panic during a freshness check was recovered. For `where=check` the check counts as failed (`last_check_at` bumped, so the cooldown applies) and also counts as an unknown end: `last_check_at` is bumped and `check_fail_count` extended. The `scheduler`, `reschedule` and `t1` recovers only log; `where=reschedule` guards the start-time retry spread (`rescheduleFailingAdoptions`), and `where=t1` is the handler's client-triggered check goroutine (logged by `internal/handler`). |
+| `freshness_check_panicked` | ERROR | `where` (`check`, `scheduler`, `reschedule`, `t1`, `reaper`), `canonical_host`, `suite_path`, `panic`, `stack` (first occurrence per `where` per process only) | A panic during a freshness check was recovered. For `where=check` the check counts as failed (`last_check_at` bumped, so the cooldown applies) and also counts as an unknown end: `last_check_at` is bumped and `check_fail_count` extended. The `scheduler`, `reschedule` and `t1` recovers only log; `where=reschedule` guards the start-time retry spread (`rescheduleFailingAdoptions`), and `where=t1` is the handler's client-triggered check goroutine (logged by `internal/handler`). `where=reaper` is a suite reaper run (empty `canonical_host` and `suite_path`): the run ends, a suite it held is released, and the next run comes as scheduled. |
 | `freshness check failed` | INFO | `canonical_host`, `suite_path`, `form` (`inline`, `detached`, `unsigned`, or `probe` for the probe tree of a suite without a snapshot), `err`, `result` (always `failed`) | A check ended unknown — a transport error, an error status that is not an absent answer, a success status other than 200 or 304 (a fetch contract violation: `err` says `unexpected success status`), a 304 to a request that carried no validators (adoption enabled; `err` says `304 Not Modified to a request without validators`), or the 2-minute check deadline at the host slot or the request — or found nothing it may adopt: a suite without a snapshot whose upstream has neither `InRelease` nor `Release` (which also records the refusal state `anchor_absent`, shown on the status page); an unsigned `Release` while `adoption.adopt_unsigned` is off is the refusal `unsigned_disabled` instead. A check whose probe found a signature but could not record the suite as seen signed fails too (`err` starts `mark the signature seen:`). A shutdown that ends a check — its request, or a cache read or marker write after it — is not a failure: nothing is written or counted (DEBUG only). `last_check_at` is bumped and `check_fail_count` extended, so the scheduler backs off (2 min doubling to 1 h), and `acu_freshness_check_total{result="failed"}` is incremented. With adoption enabled, 401, 403, 404, 410 and a 200 whose body is not the file asked for are answers, not failures (the anchor is absent): they never extend the streak. |
 | `adoption_run_failed` | WARN; INFO when the attempt was refused (outcome `refused`) | `canonical_host`, `suite_path`, `form`, `err` | Backstop line for every adoption attempt that returned an error, so a failure with no categorised line of its own is still visible. A refusal is a state, not a failure, so its line is INFO (`adoption_refused` carries the reason and timing); operators alerting on WARN are not paged for a flapping mirror. |
 | `adoption: read prior snapshot failed` | WARN | `canonical_host`, `suite_path`, `err` | The adoption could not read the suite's current (prior) snapshot before running Date and rollback checks against it; the attempt fails as `db_failed` and is retried on the normal backoff schedule rather than skipping those checks. |
@@ -278,7 +285,7 @@ detection, the startup legacy trust audit, the flip barrier, suite rows
 | `adoption_reconcile_failed` | WARN | `canonical_host`, `suite_path`, `snapshot_id`, `stage` (`decision`: deciding whether an unchanged suite needs a reconcile; `heal`: the reconcile run after an unchanged check; `on_demand`: `POST /reconcile`), `err` | Reconciling a snapshot's declared index files failed — for example a stored anchor that no longer decodes — so nothing was healed this time; it is tried again on a later unchanged check. |
 | `freshness: read suite row after a panic failed` | WARN | `err`, `canonical_host`, `suite_path` | After a recovered check panic, the best-effort bump of `last_check_at`/`check_fail_count` could not even read the suite's row; the row is left as it was. |
 | `freshness: persist failure bump after a panic failed` | WARN | `err`, `canonical_host`, `suite_path` | After a recovered check panic, `last_check_at` and `check_fail_count` were bumped in memory but the write failed; the row is left as it was. |
-| `clock_step_detected` | WARN | `stamp`, `canonical_scheme`, `canonical_host`, `suite_path`, `stored_unixtime`, `now_unixtime` | A stored timestamp is more than 2 s in the future (the wall clock stepped backwards); the gate treated it as old. Rate-limited to one line per minute. For `stamp=adoption_next_retry_at` the stamp is not treated as old: it is clamped to now + 1 h. For `stamp=last_requested_at` (a suite's client-interest stamp, read when the suite cap looks for an idle suite to evict, and by the check of a suite without a snapshot) the suite is not evicted — a future stamp never counts as idle — and the check counts a stamp up to 7 days ahead as recent interest; further ahead, the suite is dormant until a client asks for it again. `acu_clock_steps_detected_total` counts detections, not distinct clock steps or distinct stamps: the same future stamp is detected again every time it is read — every check for a future `last_check_at` or `last_success_at` (the scheduler reads a future `last_success_at` without reporting it), every check of a suite without a snapshot for a future `last_requested_at`, and every creation attempt within eight rows of the global cap for the future `last_requested_at` its eviction scan re-reads (the line stays rate-limited) — so alert on its rate, not its value. |
+| `clock_step_detected` | WARN | `stamp`, `canonical_scheme`, `canonical_host`, `suite_path`, `stored_unixtime`, `now_unixtime` | A stored timestamp is more than 2 s in the future (the wall clock stepped backwards); the gate treated it as old. Rate-limited to one line per minute. For `stamp=adoption_next_retry_at` the stamp is not treated as old: it is clamped to now + 1 h. For `stamp=last_requested_at` (a suite's client-interest stamp, read when the suite cap looks for an idle suite to evict, by the suite reaper, and by the check of a suite without a snapshot) the suite is not evicted or reaped — a future stamp never counts as idle; the reaper reports the furthest such stamp at its first run after a start and again only when it changes — and the check counts a stamp up to 7 days ahead as recent interest; further ahead, the suite is dormant until a client asks for it again. For `stamp=last_seen_at` (a signed-seen record's stamp, read once a day by the suite reaper's sweep of records no suite uses; `suite_path` is the record's repository path, the furthest, reported at the first sweep after a start and when it changes) the record counts as seen now — it is never swept — and its stamp is left as it is: it ages out only once the clock has passed it (rewriting it to the stepped-back now would lose signed history once the clock is corrected). For `stamp=wall_clock` (the suite reaper; no suite fields) the wall clock moved more than an hour further than the monotonic clock between two of the reaper's runs — a forward step without a restart, as when a virtual machine or a laptop resumes; `stored_unixtime` is where the wall clock should be, `now_unixtime` where it is — and the reaper deletes nothing, no suite and no record, for an hour; this one is not rate-limited and not counted in `acu_clock_steps_detected_total`. `acu_clock_steps_detected_total` counts detections, not distinct clock steps or distinct stamps: the same future stamp is detected again every time it is read — every check for a future `last_check_at` or `last_success_at` (the scheduler reads a future `last_success_at` without reporting it), every check of a suite without a snapshot for a future `last_requested_at`, and every creation attempt within eight rows of the global cap for the future `last_requested_at` its eviction scan re-reads (the line stays rate-limited) — so alert on its rate, not its value. |
 | `legacy_snapshot_quarantined` | WARN | `reason` (`legacy_plaintext`, `malformed_envelope`, `unsigned_declarations`, `unparseable`, `anchor_missing`, `anchor_oversized`, `anchor_corrupt`, `unexpected_form`), `snapshot_id`, `canonical_host`, `suite_path`, `anchor_form`, `was_current` | The startup trust audit could not prove a pre-upgrade snapshot signed and clean; it no longer serves or vouches, and its suite is served unadopted until it re-adopts. |
 | `legacy_trust_audit_deferred` | ERROR | `snapshot_id`, `canonical_host`, `suite_path`, `err`, `stack` (present only for a recovered panic) | A pre-upgrade snapshot's pool file could not be read (not "missing"), or auditing it panicked (`err` carries the panic value); the snapshot is left as it was and retried at the next start. |
 | `legacy_trust_audit_progress` | INFO | `audited`, `quarantined`, `deferred` | One batch of up to 100 queued snapshots was audited and committed; the fields are running totals across this start's batches. Logged on the first start after an upgrade, and on later starts while deferred snapshots remain queued or after an interrupted audit. |
@@ -291,8 +298,13 @@ detection, the startup legacy trust audit, the flip barrier, suite rows
 | `shutdown_waiting_for_adoptions` | INFO | `waited`, `detail` | Shutdown has waited 2 s for the adoption, repair and reconcile goroutines: a commit already running is never cancelled and may take up to its 5-minute bound; other work stops at its next cancellation point (adoptions in the flip barrier were abandoned at the signal). Logged once per shutdown. |
 | `suite_seed_rejected` | WARN | `canonical_host`, `suite_path`, `cap` (`global_cap`, `host_cap`), `max_suites`, `suppressed`, `detail` | A suite was refused a row by `freshness.max_suites`: it is served but will not refresh; retried after 10 minutes (`detail` = `suite will not refresh`). At the total cap while every suite that could have been evicted was busy — being checked or adopted, or evicted for another new suite — the refusal is transient: `detail` = `eviction candidates busy; retried after 1 minute`, and the suite is retried after a minute. At most one line a minute; `suppressed` counts the refusals since the last line. |
 | `suite_evicted` | INFO | `canonical_host`, `suite_path` (the evicted suite), `for_host`, `for_suite` | At or above the global cap, a suite with no adopted snapshot that had not been requested for 24 hours was evicted for a new one. Above the cap (after `freshness.max_suites` was lowered, or after an upgrade) the new suite is still refused (`suite_seed_rejected`) until the table is back under the cap. |
-| `suite_caps_exceeded` | WARN | `suites`, `max_suites`, `hosts_at_cap`, `per_host_cap`, `fullest_host`, `fullest_host_suites` | At start, the suite table holds more than `freshness.max_suites` suites (a lowered `max_suites`, or an upgraded cache), or a host holds a tenth of it or more: new suites — for a host at its cap, that host's — are refused. Above the total cap each refused attempt still evicts one suite idle for 24 hours, so the table shrinks back; a host at its cap evicts nothing for its own new suites; its suites go when idle-suite reaping (a later release) removes them, or when total-cap eviction for any host's new suite picks one of its idle suites. Not logged for a table at exactly the total cap: that is normal operation for a full cache, where a new suite evicts an idle one (no adopted snapshot, not requested for 24 hours) and is refused only when none qualifies (`suite_seed_rejected`). Logged once per start. |
+| `suite_caps_exceeded` | WARN | `suites`, `max_suites`, `hosts_at_cap`, `per_host_cap`, `fullest_host`, `fullest_host_suites` | At start, the suite table holds more than `freshness.max_suites` suites (a lowered `max_suites`, or an upgraded cache), or a host holds a tenth of it or more: new suites — for a host at its cap, that host's — are refused. Above the total cap each refused attempt still evicts one suite idle for 24 hours, so the table shrinks back; a host at its cap evicts nothing for its own new suites; its suites go when the suite reaper removes those not requested for `gc.suite_idle_ttl`, or when total-cap eviction for any host's new suite picks one of its idle suites. Not logged for a table at exactly the total cap: that is normal operation for a full cache, where a new suite evicts an idle one (no adopted snapshot, not requested for 24 hours) and is refused only when none qualifies (`suite_seed_rejected`). Logged once per start. |
 | `suite cap usage read failed` | WARN | `err` | The startup check behind `suite_caps_exceeded` could not count the suites; the caps apply either way. |
+| `suite_reaped` | INFO | `canonical_scheme`, `canonical_host`, `suite_path`, `reason` (`idle`, `non_canonical`, `junk`), `metadata_rows` | The suite reaper (spec C9; hourly, from an hour after the start, at most 25 suites a run, `non_canonical` and `junk` ones first) deleted a suite: its metadata `url_path` rows (`metadata_rows`, in batches of 1,000, the anchors last), then, in one transaction that re-checks that the suite was not adopted — nor, for `idle`, asked for — since the run listed it, its snapshots (retired: snapshot GC deletes them) and its row. `idle`: no client asked for it in `gc.suite_idle_ttl`; `non_canonical`: a row an earlier release created for a path this version no longer tracks (`metadata_rows` is 0: such a path's cached files are left to `gc.url_path_ttl`); `junk`: a suite without a snapshot whose cached anchor files (`InRelease`, `Release`, `Release.gpg`) all fail their content check — an HTML page, say; a plain `Release` cached as `InRelease` passes — each fetched at least a day ago, whatever its adoption record says. Its signed-seen records survive, stamped with the time of the reap at the latest. Counted in `acu_suites_reaped_total{reason}`. |
+| `suite_forgotten` | INFO | `canonical_scheme`, `canonical_host`, `suite_path`, `metadata_rows`, `markers_deleted`, `anchor_rows` | `POST /forget` deleted a suite as the reaper would, regardless of idleness, and — only when no other suite of the repository path, under either scheme or as its `/.` twin, has a row — `markers_deleted` signed-seen records of the path and `anchor_rows` cached `InRelease`, `Release` and `Release.gpg` of the path and its twin under either scheme. Each request is logged first as INFO `admin_forget_requested` (`scheme`, `canonical_host`, `suite_path`; by `internal/admin`). |
+| `admin: forget failed` | WARN | `err`, `scheme`, `canonical_host`, `suite_path` | `POST /forget` failed on a database error, or its work took longer than 5 minutes, and answered 500. Metadata batches deleted before the error stay deleted (the anchors go last); the request can be repeated (409 then means the suite is gone after all, or busy). |
+| `signed_seen_markers_swept` | INFO | `count` | The suite reaper's daily sweep deleted `count` signed-seen records whose repository path has no suite row (for it or its `/.` twin, under either scheme) and that were last seen — or their path's suite last reaped — more than 365 days ago. Not logged when it deleted none. |
+| `suite reaper: list suites failed`, `suite reaper: read suite row failed`, `suite reaper: delete metadata failed`, `suite reaper: reap failed`, `suite reaper: list unadopted anchors failed`, `suite reaper: junk anchor reads failed`, `suite reaper: marker sweep failed` | WARN | `err`; `canonical_host` and `suite_path` for a suite's step; `metadata_rows` (rows deleted before the error); `count`, `canonical_host` and `path` for the junk anchor reads (how many pool files failed to read in the run, and the first); `swept` (records deleted before the error) | A step of a suite reaper run failed; nothing else in the run depends on it. A failed listing reaps nothing this run; a suite whose step failed — a database error, or its work cut short after 5 minutes — keeps its row and is retried at the next run (the metadata batches deleted before the error stay deleted, the anchors last); a pool file that exists but does not read keeps its suite (a missing one is not logged); the junk scan runs again at the next run, the record sweep the next day. A step that fails because the daemon is stopping is logged at DEBUG instead. |
 | `handler_goroutine_panicked` | ERROR | `where` (`touch`, `seed_rejections`), `canonical_host` and `path` (`touch` only), `panic`, `stack` (first occurrence per `where` per process only) | A panic in one of the handler's background goroutines — the touch goroutine, or the cap-rejection cache's sweep timer — was recovered (a bug, please report it). |
 | `suite onboarding failed` | WARN | `err`, `canonical_host`, `suite_path` | A cache hit on an untracked suite's anchor could not create its row (including the 5 s bound it shares with the backstop that may follow, and a shutdown that cut it short); the anchor is served and the next hit retries. |
 | `suite signed-seen mark failed` | WARN | `err`, `canonical_host`, `suite_path` | A suite's row was created, and one of its cached signed anchors changed between the handler's read and the creating write (or the handler could not read it: `suite cached anchor read failed`), so it was marked afterwards — and that write failed, or ran out of its 5 s bound. The startup pass marks it at the next start. (Every other mark is part of the write that creates the row or stores the anchor.) |
```

- [ ] **Step 3: Admin page (`docs/admin-ui-spec.md`)**

The status page gains each suite's scheme (Task 10: JSON `scheme`, additive, and the Host cell's `scheme://` prefix); the GC section's `retired_reaped` stays an H carry-forward (design note 15); the refusal texts change:

```diff
diff --git a/docs/admin-ui-spec.md b/docs/admin-ui-spec.md
--- a/docs/admin-ui-spec.md
+++ b/docs/admin-ui-spec.md
@@ -347,6 +347,7 @@ next to the Suites table contract in §5.3:
 
 | Field | Type | Meaning |
 |---|---|---|
+| `scheme` | string | The suite's canonical scheme, `http` or `https` (phase F, ruling PF-7): a repository can be tracked under both, as two rows with the same `host` and `suite_path`, and `POST /forget` (`docs/configuration.md`, *Forgetting a suite*) takes it, defaulting to `https`. The Suites table's Host column shows it as `scheme://host`. |
 | `anchor_form` | string | The current snapshot's anchor form — `inline`, `detached` or `unsigned` (a `Release` no signature covers: it vouches no `.deb`); empty when the suite has no current snapshot. |
 | `last_requested_unixtime` | int64 or `null` | When a client last showed interest in the suite (spec C3): written when the suite's row is created, then by coalesced touches — at most one write per suite per hour — from serves of its anchors (`InRelease`, `Release`, `Release.gpg`; status below 400, 304 and HEAD included), of its flat indexes, and of `.deb`s its current snapshot vouched for. Index requests such as `Packages.gz` never move it. It decides cap eviction: at the `freshness.max_suites` cap, a suite with no current snapshot whose stamp is 24 hours old or more can be evicted for a new one (`suite_evicted`). |
 | `check_fail_count` | int64 | Consecutive failed checks since the last answered check: ended unknown (transport error, deadline, unexpected status), or found nothing they may adopt (a suite without a snapshot whose upstream has neither `InRelease` nor `Release`, which also records `anchor_absent`; a signature the check found but could not record as seen). An unsigned `Release` while `adoption.adopt_unsigned` is off is the refusal `unsigned_disabled`, not a failed check. Drives the scheduler's healthy → never-checked → failing ordering and its 2 min → 1 h backoff. Resets to 0 on every answered check, an absent current anchor (`anchor_absent`) included. |
@@ -357,7 +358,9 @@ next to the Suites table contract in §5.3:
 | `adoption_refused` | boolean | `true` when the record above is a refusal state (`upstream_older`, `rollback_refused`, `unsigned_downgrade_refused`, `unsigned_unconfirmed`, `unsigned_too_large`, `anchor_absent`, `unsigned_disabled`), not a failure. |
 
 These are additive to the locked JSON contract (§0.1), the same way
-`keyring[]` was: no existing key changes shape or meaning.
+`keyring[]` was: no existing key changes shape or meaning (`scheme` too:
+the HTML Host cell gains the `scheme://` prefix, the JSON `host` is
+unchanged).
 
 **Adoption column.** The Suites table's "Adoption" column renders the
 record above as one line, via the server-side `adoptionStatus` helper:
@@ -375,7 +378,8 @@ is an unsigned repository waiting for its second observation
 sees a 401/403 on a signature file) and clears itself when it adopts;
 `unsigned_downgrade_refused` (the suite was seen signed) lasts until
 upstream serves a signature the cache adopts, or the suite is forgotten
-(`POST /forget`, added with suite reaping in a later release);
+(`POST /forget`, an admin endpoint outside this page: see
+`docs/configuration.md`, *Forgetting a suite*);
 `unsigned_too_large` (more than 1,000 declared files) lasts until
 upstream's `Release` declares fewer; `anchor_absent` means the current
 snapshot's anchor is gone upstream (or a detached suite's changed
@@ -384,7 +388,7 @@ a suite without a snapshot, that upstream serves neither `InRelease` nor
 `Release`; `unsigned_disabled` means `adoption.adopt_unsigned` is off:
 an unsigned `Release` is not adopted, and an unsigned suite already
 adopted keeps serving its last snapshot without updates until the knob is
-turned back on. None of them hides lag (only `upstream_older` and
+turned back on or the suite is forgotten. None of them hides lag (only `upstream_older` and
 `rollback_refused` do, §9.1), and all five follow the `REFUSED 24H+` rule
 below. A suite without a snapshot shows lag from the first check that
 found its anchor, so a first adoption refused for good
```

- [ ] **Step 4: Spec revision 9**

The **Date** line's `(revision 8, 2026-10-19:` becomes `(revision 9, 2026-10-20: …; revision 8, 2026-10-19:`, the inserted text being:

```text
revision 9, 2026-10-20: phase F plan rulings — the reaper and `POST /forget` delete a suite row only through phase D's compare-and-delete, holding the suite through the `SuiteGuard`, so one delete path remains (R-F1); `Gate.Purge` keeps a live entry (R-F2); the reaper also reaps legacy junk rows (R-F3); the orphan-marker sweep reads a future `last_seen_at` as now, and a marker is an orphan only when neither its base nor its `/.` twin has a row under either scheme, the rule `/forget`'s marker deletion follows too (R-F5); after the phase F plan reviews (rulings PF-1–PF-9): the sweep never rewrites a stamp (PF-1, phase F deviation 3); `/forget` deletes the cached anchors of the base and its twin under both schemes with the markers (PF-2); junk is a cached anchor that fails its sniff, an `InRelease` passing the `Release` sniff (a plain-text Release) excepted, whatever the suite's record, fetched at least 24 h ago (PF-3); the junk scan runs every run and the candidates reaped regardless of idleness go first (PF-4); with `adopt_unsigned` off an established unsigned suite keeps its coherent snapshot — R-F4's retirement into the ordinary cache is dropped (PF-5); the `/forget` guard stops a cross-site form or fetch, not DNS rebinding, and Host-header validation joins G-1 (PF-6); `/forget`'s answers say what happened and the status page shows each suite's scheme (PF-7); a forward step of the wall clock defers reaping and the sweep by the uptime floor, a suite's held work is bounded at 5 minutes, a reap stamps its base's markers, reaps are counted by reason (PF-8); the marker sweep runs once a day; `0s` disables reaping, not the reaper's other duties; the idle cutoff is strict and a NULL or future stamp is never idle; `/forget` takes an optional `scheme`, runs to completion on a context a client cannot cancel, and answers 400, 403 and 500 besides 202 and 409; sub-job C deletes one retired snapshot per transaction without a grace; a re-adoption's `adopted_at` stays above every snapshot of its suite, retired ones included; the 90-day simulation runs the reaper every 6 h and restarts every fifth day, and a four-day simulation runs the production cadence (phase F deviation 2)
```

Then C3's marker paragraph (design note 13, rulings PF-1, PF-2, M-9), C4's clock-step rule (R-F5, PF-1), C5 safeguard 1 (PF-5), C9 (R-F1–R-F3, R-F5, PF-1–PF-4, PF-6–PF-8, design notes 2–8, 14, 15, deferred minor C-11), C10's rewrapped line (E-63), C11's events, metrics and constants, §7 (deviation 2, the production-cadence simulation, the new DoD items) and §10's G-1 item (PF-6):

```diff
diff --git a/docs/superpowers/specs/2026-09-27-flat-repositories-design.md b/docs/superpowers/specs/2026-09-27-flat-repositories-design.md
--- a/docs/superpowers/specs/2026-09-27-flat-repositories-design.md
+++ b/docs/superpowers/specs/2026-09-27-flat-repositories-design.md
@@ -454,10 +454,18 @@ that have a `suite_freshness` row. Writers:
 
 A plain-text "InRelease" fails the sniff and never creates a marker. The
 marker survives suite reaping: the reaper sweeps a marker only once no
-suite row exists for its base or its `/.` twin and its `last_seen_at` is
-older than 365 days (a live suite's marker is never swept, so no renewal
-write is needed). `POST /forget` deletes it only if no other suite with the
-same base has a row.
+suite row exists for its base or its `/.` twin, under either scheme, and
+its `last_seen_at` is older than 365 days (a live suite's marker is never
+swept, so no renewal write is needed; a future stamp reads as now and is
+never rewritten, C9). A reap moves the base's markers up to the reap
+(`MAX(last_seen_at, now)`), so the year counts from the end of the
+suite's tracking. `POST /forget` deletes the base's markers, under both
+schemes — and the cached `InRelease`, `Release` and `Release.gpg` of the
+base and its `/.` twin under both schemes, which the unsigned commit's
+re-mark would otherwise read and mark again (phase F, ruling PF-2) — only
+if no other suite row exists for the base or its `/.` twin under either
+scheme (phase F, design note 13: never-downgrade reads both schemes'
+markers).
 
 ### C4. Freshness check
 
@@ -733,9 +741,14 @@ pending change), cleared in the flip transaction and on
 
 **Clock-step rule** (fixes I-7 in all touched gates). A *past-event* stamp
 (`last_check_at`, `last_success_at`, `last_requested_at`,
-`adoption_fail_last_at`, `adoption_fail_first_at`, `adopted_at`) found in the
+`adoption_fail_last_at`, `adoption_fail_first_at`, `adopted_at`, and the
+`url_path.last_fetched_at` and `suite_signed_seen.last_seen_at` that C9
+reads) found in the
 future is treated as infinitely old by gates that *delay* work (cooldown, T2
-threshold) and as "now" by gates that *retire* things (reap cutoff). The
+threshold) and as "now" by gates that *retire* things (the reap cutoff, the
+junk grace, and the marker sweep, which never rewrites the stamp: lowering
+it to a stepped-back now would delete signed history once the clock is
+corrected, phase F ruling PF-1). The
 exception is the confirmation gaps — the unsigned transition's and the
 absent anchor's — where a future `adoption_fail_first_at` never confirms
 and re-recording restarts it: reading it as infinitely old would skip a
@@ -886,9 +899,15 @@ upgrade (§6.1).
    it as an `anchor_absent` record does (no request and no scheduler check
    before its retry time). Turning the knob back on updates it at its next
    retry; bytes refused this way were never confirmed, so a never-adopted
-   suite's transition is confirmed afresh. Phase F retires an established
-   unsigned suite's current snapshot through the reaper's retirement path
-   while the knob is off, so it returns to the ordinary cache.
+   suite's transition is confirmed afresh. While the knob is off the cache
+   does not keep unsigned repositories fresh: an established unsigned
+   suite keeps serving its last snapshot, coherent and frozen, until the
+   knob is turned back on or the suite is forgotten (or reaped as idle),
+   and a never-adopted one is served as the cache first stored its files
+   (phase F, ruling PF-5, which drops R-F4's retirement into the ordinary
+   cache: there each index file would be frozen at its own first fetch,
+   so every upstream publish would bring Hash Sum mismatches that never
+   clear). The knob is for caches that serve no unsigned repositories.
 2. Eligible only when `InRelease` and `Release.gpg` are both *absent* and
    `Release` is *present*. The **transition into** unsigned (a suite with no
    current snapshot) needs two observations of the same `Release` bytes at
@@ -1233,36 +1252,89 @@ removes the suite row. Retired snapshots are invisible to everything that
 reasons about a suite's history: the natural-key uniqueness (partial index
 `WHERE retired_at IS NULL`), `InsertCandidateSnapshot`'s collision lookup,
 `rollback_refused`, retained by-hash, `DeclaredHashesForPath`,
-`HostCurrentSnapshotsCoverage` and snapshot-GC sub-job B. A returning or
+`HostCurrentSnapshotsCoverage` and snapshot-GC sub-job B (whose
+`keep_displaced` ranking they leave). A returning or
 forgotten suite therefore re-adopts unchanged upstream content at once, and a
-deliberate rollback adopts after `/forget`.
+deliberate rollback adopts after `/forget`. Only the adoption stamp still
+sees them: a re-adoption's `adopted_at` stays above every snapshot of the
+suite, retired ones included, so C8's `Last-Modified` never goes back
+(phase F, deferred minor C-11).
 
 **Reaper.** Its own hourly ticker under the freshness checker's lifecycle
-(independent of `gc.interval` and of the T2 loop), after a monotonic uptime
-floor of 1 h, at most 25 suites per run (constant), oldest first. Candidates:
-`last_requested_at < now − gc.suite_idle_ttl` (default `720h`; `0s` disables),
+(independent of `gc.interval`, of `gc.enabled` and of the T2 loop), after a
+monotonic uptime floor of 1 h; each run starts an interval after the
+previous one finished. At most 25 suites per run (constant; a busy one
+counts): those reaped regardless of idleness first (phase F, ruling
+PF-4), then the oldest `last_requested_at`. Candidates:
+`last_requested_at < now − gc.suite_idle_ttl` (default `720h`; `0s` disables
+reaping; a NULL or future stamp is never idle, and the furthest future one
+is reported through `clock_step_detected` at the first run after a start
+and whenever it changes),
 plus rows whose `suite_path` is non-canonical under the tightened grammar
-(unreachable legacy rows, reaped regardless of idleness). For each: `TryLock`
-the suite mutex (busy → skip), then:
+(unreachable legacy rows, reaped regardless of idleness), plus legacy junk
+rows, also reaped regardless of idleness (phase F, rulings R-F3, PF-3): a
+suite without a current snapshot whose cached anchor rows (`InRelease`,
+`Release`, `Release.gpg`) — at least one — all hold a blob fetched at
+least 24 h ago (a NULL or future stamp is not old) that is junk evidence:
+its head fails its own sniff and, for an `InRelease`, the `Release` sniff
+too. A plain-text Release cached as `InRelease` is the §6.1-quarantined
+plain-text suite that adopts unsigned through `Release` (C5), not junk;
+any other sniff-failing anchor is evidence whatever the suite's adoption
+record — a failed adoption's record would otherwise keep an HTML anchor
+served for good, since an unadopted suite's metadata is never
+revalidated. A missing or unreadable pool file is no evidence; the sniffs
+read at most 64 KiB per anchor on the reaper's goroutine, and each verdict
+is kept per anchor blob until the next scan. For each candidate: hold the
+suite through the checker's `SuiteGuard` (`HoldSuite` `TryLock`s the
+suite mutex; busy → skip, retried next run) and re-read its row (I-4: a
+row gone since the listing has its lock-map entry dropped; a suite asked
+for, or adopted, since the listing is left whole; junk evidence is not
+read again), then, within 5 minutes (a step cut short is retried next
+run):
 
 1. delete the suite's metadata `url_path` rows — rows where
    `aptpath.SuitePath(path) == suite` (never a twin's or nested suite's) —
    plain metadata first, anchors last, in batches of 1,000 rows per writer
    transaction without touching the suite row;
-2. in one final writer transaction `cache.ReapIdleSuite`: re-read the row and
+2. in one final writer transaction `cache.ReapIdleSuite`, through phase D's
+   compare-and-delete (`deleteSuiteTx`): re-read the row and
    bail if the stamp is newer than the cutoff or `current_snapshot_id` changed
-   since the candidate was listed (compare-and-delete); run the Step-8 refcount
+   since the candidate was listed; run the Step-8 refcount
    decrement for the current snapshot; set `retired_at` on all of the suite's
-   snapshots; delete the `suite_freshness` row.
-
-Then it purges in-memory state (lock-map `CompareAndDelete`, flipgate entry,
-touch-coalescing entry). It also sweeps signed-seen markers whose base and
-`/.` twin have no `suite_freshness` row and whose `last_seen_at` is older
-than 365 days, and the reconcile memo (against current snapshot ids).
-The gpg once-logger maps reset when they exceed 10,000 entries. Snapshot GC
-gains sub-job C: retired snapshots are deleted with their members and
-`package_hash` rows in bounded chunks (10,000 rows per writer transaction),
-then the header row.
+   snapshots; delete the `suite_freshness` row. `deleteSuiteTx` holds the
+   only production `DELETE FROM suite_freshness`, and only cap eviction,
+   `ReapIdleSuite` and `ForgetSuite` call it (phase F, ruling R-F1).
+
+Then `release(true)` purges in-memory state: the lock-map entry
+(`CompareAndDelete`), the checker's per-suite maps, the flip-gate entry and
+the touch-coalescing entry. `Gate.Purge` skips a live entry — a flip in
+progress, a hold, an anchor serve in flight or one completed within the
+quiet period — which expires on its own, so a release racing a re-created
+suite's serve never shortens that serve's quiet period (ruling R-F2). Each
+run also scans for junk rows (every run, ruling PF-4: one indexed read
+that probes `url_path` by its key, pool reads only for blobs not yet
+sniffed) and sweeps the reconcile memo (against the current snapshot ids
+it listed). Once a day — at the first run after a start, then a day apart
+(monotonic) — it sweeps the signed-seen markers whose base and `/.` twin
+have no `suite_freshness` row under either scheme and whose
+`last_seen_at` is older than 365 days, each delete re-checking both in
+its writer transaction. The sweep is a gate that retires things (C4
+clock-step rule): a future stamp reads as now, so its marker is never
+swept; the sweep never rewrites it and reports the furthest through
+`clock_step_detected` at the first sweep after a start and when it
+changes (rulings R-F5, PF-1). A forward step of the wall clock that no
+restart re-arms — the wall clock more than 1 h ahead of the monotonic
+clock between two runs, as when a VM or laptop resumes — is reported
+(`clock_step_detected`, `stamp=wall_clock`) and defers reaping and the
+sweep by the uptime floor (ruling PF-8). Reaps are counted by reason
+(`acu_suites_reaped_total{reason}`). The gpg once-logger sets start
+over when a new key finds 10,000 entries. Snapshot GC gains sub-job C:
+after sub-jobs A and B, within the snapshot pass's deadline, one retired
+snapshot per writer transaction is deleted with its members, skipped
+members and `package_hash` rows in bounded chunks (10,000 rows per writer
+transaction), then the header row; it needs no grace, since a retired
+snapshot is never a candidate again (the collision lookup ignores it and
+`CommitAdoption` refuses it).
 
 **Migration backfill** of `last_requested_at`: the newest client
 `last_requested_at` of the suite's anchor `url_path` rows, else migration time
@@ -1270,14 +1342,28 @@ minus the 7-day interest window. Dormant never-adopted suites are therefore
 neither eagerly adopted after upgrade nor kept alive; they are reaped about 23
 days later.
 
-**`POST /forget` (D9).** Admin endpoint (`host`, `suite`). It requires a
-non-simple request — header `X-ACU-Admin: 1` — and rejects any
-`Sec-Fetch-Site` other than `same-origin` or `none`, so a web page cannot
-trigger it (security G-1 class). Under the suite mutex it runs the reaper's
-steps to completion (all batches, then the final transaction without the idle
-checks) and deletes the signed-seen marker for the suite's base if no other
-suite with that base has a row. It answers 202 on success and 409 for both
-busy and unknown suites (no existence oracle). It is the documented escape
+**`POST /forget` (D9).** Admin endpoint (`host`, `suite`, and an optional
+`scheme`: `http` or `https`, default `https`, as for `/reconcile`). It
+requires a non-simple request — header `X-ACU-Admin: 1` — and rejects any
+`Sec-Fetch-Site` other than `same-origin` or `none`, so a cross-site form
+or `fetch` cannot trigger it (security G-1 class; a cross-origin `fetch`
+that sets the header needs a CORS preflight, never granted); both refusals
+are 403. It does not stop DNS rebinding against a listener without
+`htpasswd_file`: Host-header validation is part of G-1 (§10; phase F,
+ruling PF-6). It reads the suite's row (I-4), holds the suite through the
+`SuiteGuard` and runs the reaper's steps to completion, within 5 minutes
+— all batches, then `cache.ForgetSuite` through `deleteSuiteTx` without
+the idle checks — on a context a client disconnect cannot cancel; the
+final transaction also deletes the base's signed-seen markers, under both
+schemes, and the cached anchors of the base and its `/.` twin under both
+schemes, if no row is left for the base or the twin under either scheme
+(ruling PF-2). It answers 202 once the suite is forgotten, its body naming
+the suite, the markers deleted and the rows that kept them, and 409 for
+both busy and unknown suites (no existence oracle; the body says what to
+check: the scheme, which defaults to `https`, and the host and suite as
+the status page shows them, which now shows each suite's scheme; ruling
+PF-7), 400 for a missing `host` or `suite` or another scheme, and 500 for
+a database error (logged). It is the documented escape
 hatch for refusal states: a vendor that stopped signing, a deliberate upstream
 rollback, a stuck unsigned suite. (`POST /reconcile` keeps its existing
 protection; hardening it is part of G-1, §10.)
@@ -1302,8 +1388,9 @@ that is left (phase E, design review M-10: the miss path still downloads
 the whole `.deb` before its first byte, within apt's 30 s) — then re-runs
 the lookup once; a request whose client left during the wait is answered
 503 (`client_canceled`) and never served (phase E fix round 1: a re-run
-lookup with the dead context failed open). On the miss path, where the strict decision sits
-inside singleflight after host-slot acquisition by design, `runFetch` returns
+lookup with the dead context failed open). On the miss path, where the
+strict decision sits inside singleflight after host-slot acquisition by
+design, `runFetch` returns
 `errUnvouchedRetryable` (releasing its slot) when an adoption is in flight on
 the host; `serveCacheMiss`, outside singleflight, waits under the same
 bound and retries the miss once: requests that waited together coalesce
@@ -1335,7 +1422,8 @@ stays live).
 ### C11. Observability and configuration
 
 **Events** (documented in `docs/log-fields.md`): `suite_seed_rejected`,
-`suite_reaped`, `suite_forgotten`, `adoption_deferred`,
+`suite_reaped`, `suite_forgotten`, `admin_forget_requested`,
+`signed_seen_markers_swept`, `adoption_deferred`,
 `adoption_failing_persistently`, `adoption_refused` (reason),
 `adoption_flip_barrier`, `adoption_abandoned_at_shutdown` (an adoption
 ended by shutdown, in the flip barrier or before it: INFO, never counted as
@@ -1350,8 +1438,10 @@ gauges `acu_adoption_failing_suites` (C7), `acu_freshness_waiting_for_slot`
 (flips in the C8 barrier now), the C3 pair
 `acu_suite_seed_rejected_total{host}` (counter) and
 `acu_suites_seed_rejected` (gauge), `acu_freshness_unlogged_panics_total`,
-and phase E's `acu_repair_deferred_total{host}` and
-`acu_strict_adoption_waits_total{host}`.
+phase E's `acu_repair_deferred_total{host}` and
+`acu_strict_adoption_waits_total{host}`, and phase F's unlabelled counter
+`acu_gc_retired_reaped_total` and `acu_suites_reaped_total{reason}` (three
+values: `idle`, `non_canonical`, `junk`).
 New metrics are host-labelled or
 unlabelled (no suite labels) and are added to the metrics inventory comments in
 `internal/freshness/metrics.go` and `internal/handler/metrics.go`.
@@ -1370,7 +1460,11 @@ Constants, not knobs: backoff 2 m → 1 h (adoption failures and repeated
 (the C8 quiet cut at 15 s); phase A cap 60 s; hold-token wait 60 s; waiter
 caps 256 (barrier) and 64 (strict wait); strict wait 15 s, within a 25 s
 request budget, none below 1 s; the miss path's backstop deadline at most
-25 s after the request started; reaps 25 per run;
+25 s after the request started; reaper interval and uptime floor 1 h, the
+marker sweep 24 h apart, reaps 25 per run, the forward-step threshold 1 h,
+a suite's held work (a reap, `/forget`) 5 min, junk grace 24 h, metadata
+batches 1,000 rows, sub-job C chunks 10,000 rows, gpg once-logger sets
+10,000 keys;
 unsigned member cap 1,000; anchor-absent and unsigned confirmation gap
 `max(cooldown, 5 min)`, 1 h after a 401/403 on a signature file; check
 deadline 2 min; check workers 4; interest window 7 d; orphan-marker TTL
@@ -1578,7 +1672,10 @@ asserts **lower bounds** (liveness: checks resume, changes adopt, touched
 suites survive, returning suites re-adopt) as well as upper bounds (attempts,
 rows, goroutines, upstream requests). Where cadence matters the simulations
 run the production scheduler, reaper and GC loops at production intervals
-(`gc.interval = 24h`), with restarts.
+(`gc.interval = 24h`), with restarts — except that the 90-day reaping
+simulation runs its reaper every 6 h, the hourly cadence being pinned by
+the reaper's own tests and by a four-day simulation at the production
+cadence (phase F, deviation 2).
 
 **Fixtures.** A shared `clearsignedFixture()` helper replaces the
 non-clearsigned `InRelease` bodies in existing handler and cmd tests (about 18
@@ -1670,15 +1767,28 @@ files), in phase D.
   stray-blob and retired-snapshot negatives;
   `Last-Modified` across same-second flips, first adoption, a backward
   step, a pool file dated after its stamp and a recovery rewrite.
-- C9: 90-day simulation at `gc.interval = 24h` with 12-hourly restarts
-  (touched / idle / near-TTL / busy / clock jump); a reaped suite that returns
+- C9: 90-day simulation at `gc.interval = 24h` with restarts (touched /
+  idle / near-TTL / busy / clock jump; phase F, deviation 2: the reaper
+  every 6 h after its 1 h floor, restarts every fifth day and an hour
+  before three suites' deadlines, no url_path TTL pass, since production
+  cadence with 12-hourly restarts takes about 12.5 s under `-race`); a
+  reaped suite that returns
   before any GC tick with unchanged upstream re-adopts within one check;
   `/forget` then an immediate request re-adopts, and after an upstream
-  rollback adopts the older content; a +30 d forward step's false reaps all
-  self-heal; `gc.enabled = false` still re-adopts; never-downgrade after a
-  reap; batched metadata deletion never repeats the refcount decrement;
-  reap-vs-adoption chaos; blobs reclaimed within two GC ticks; twin and
-  nested suites untouched; `/forget` rejects a cross-origin form POST.
+  rollback adopts the older content; `/forget` lifts
+  `unsigned_downgrade_refused` unless the `/.` twin keeps a row, a signed
+  anchor cached under the other scheme included; a +30 d forward step's
+  false reaps all self-heal, and a step while running defers reaping by
+  the floor; four days at the production cadence (hourly reaper,
+  `gc.url_path_ttl = 168h`, 12-hourly restarts, goroutines bounded);
+  `gc.enabled = false` still re-adopts; never-downgrade after a reap; the
+  marker sweep never rewrites a future stamp; batched metadata deletion
+  never repeats the refcount decrement; reap-vs-adoption chaos under the
+  suite cap; blobs reclaimed within two GC ticks; twin and nested suites
+  untouched; legacy junk reaped, a transient soft-404 and a plain-text
+  Release kept; an established unsigned suite keeps its coherent snapshot
+  while `adopt_unsigned` is off; `/forget` rejects a cross-origin form
+  POST.
 - §6.1: a polluted v6 cache (R-1 member, alias and `package_hash` rows on
   current and displaced snapshots; a plain-text inline snapshot with
   `package_hash` rows) upgraded: the forged index is not served, its hashes
@@ -1783,7 +1893,11 @@ item depends on a later phase.
    `.xz/.bz2/.lzma` and Translations on every update).
 9. Downstream mirror tools requesting `Release`+`Release.gpg` from inline
    suites get 404 (pre-existing).
-10. G-1: CSRF protection for `POST /reconcile` (the new `/forget` has it).
+10. G-1: CSRF protection for `POST /reconcile` (the new `/forget` has it, in
+    `requireMutationGuard`), and Host-header validation of the admin
+    listener's mutating endpoints, `/forget` and `/reconcile` (DNS
+    rebinding against a listener without `htpasswd_file`; operators behind
+    reverse proxies are affected, a maintainer call; phase F, ruling PF-6).
 11. First contact costs two upstream GETs of the anchor (the miss and the
     eager probe).
 
```

- [ ] **Step 5: Overview**

The spec reference becomes revision 9; phase F's row reads "in progress (plan written 2026-10-20)", as phase E's docs task set its own, and becomes "done (commit …)" after the final review; the "Later phases add" F line gains the names as built and records that phase E's PE-12 carry-forward is resolved by ruling PF-5; the "**F:**" carry-forward block is deleted, as phases C, D and E deleted theirs — every item is done (Task 15's DoD map names the tests: the `keep_displaced` ranking (Task 4), `/foo/dists/InRelease` by idleness and the parked waiter (Task 6), the purge and the one delete path (Tasks 1, 2, 3, 6, 10), `Gate.Purge` (Task 1), the docs (this task), `/forget` through `deleteSuiteTx` (Tasks 3, 10), the junk rows (Task 7), the escape hatch (Task 15), PE-12 (Task 8, ruling PF-5), `last_seen_at` (Task 9)); G and H gain phase F's items, and the open decisions gain PF-6's Host-header validation:

```diff
diff --git a/docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md b/docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md
--- a/docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md
+++ b/docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md
@@ -8,7 +8,7 @@
 
 **Tech Stack:** Go 1.25 (toolchain 1.26), modernc SQLite, `github.com/ProtonMail/go-crypto`, `testing/synctest`, Docker for e2e.
 
-**Spec:** `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (revision 8). Plan reviews: 2026-09-28 (phase A executability and tests) and `2026-10-05-flat-repositories-plan-review.md` (upgrade trust and later-phase contracts; its findings map to the "Review findings 2026-10-05" table below). Every task implements a named spec section; when this plan and the spec disagree, the spec wins unless the plan records a deliberate, explained deviation (listed below).
+**Spec:** `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (revision 9). Plan reviews: 2026-09-28 (phase A executability and tests) and `2026-10-05-flat-repositories-plan-review.md` (upgrade trust and later-phase contracts; its findings map to the "Review findings 2026-10-05" table below). Every task implements a named spec section; when this plan and the spec disagree, the spec wins unless the plan records a deliberate, explained deviation (listed below).
 
 ## Global Constraints
 
@@ -63,7 +63,7 @@ Each phase: write its detailed plan (`2026-09-28-flat-repositories-<letter>-<nam
 | C. Flip consistency | `…-C-flip.md` | C8 incl. the hold token | B | done (commit 1f736a6) |
 | D. Creation bounds and signals | `…-D-creation.md` | C3 (rest: caps, touch, miss-path signed-seen marker, `Release`/flat sniffs) | A | done (commit 3ed0b2b) |
 | E. Eager adoption and anchor forms | `…-E-forms.md` | C4 probe tree, C5 unsigned/SHA512, C10 wait | B, D | done (commit 9ab48c4) |
-| F. Suite reaping | `…-F-reaping.md` | C9 | D, E | to write after E |
+| F. Suite reaping | `…-F-reaping.md` | C9 | D, E | in progress (plan written 2026-10-20) |
 | G. Flat repositories | `…-G-flat.md` | C2 flat, C5 flat IndexTargets/Filenames, C6, C4 anchorless | A–F | to write after F |
 | H. End to end and docs | `…-H-e2e-docs.md` | §7 e2e, §8 | all | to write after G |
 
@@ -93,29 +93,13 @@ Later phases add (named here so phase plans stay consistent):
 - C: package `flipgate` — `Gate` (`New(Config{Quiet})`, `Enabled`, `Quiet`, `Stats`, `Len`, `Purge(key)`, `Close()` (shutdown; `main` calls it at the signal), `Closed()`, `AnchorStart(ctx, key) *Token`, `Flip(ctx, key, commit) (Result, error)`), `Key(scheme, host, suitePath)` (the freshness suite key), `Token.Done`, `Token.Held`, outcomes `held`/`quiet_cut_short`/`hold_expired`/`hold_skipped`/`aborted`/`disabled`, `ErrAborted`, `ErrClosed`, the spec C11 constants (`PhaseAMax`, `TokenWaitMax`, `HoldMax`, `CommitMargin`, `AnchorTimeout`, `MaxWaiters`, `MinQuiet`, `MaxQuiet`; `MaxQuiet ≤ HoldMax − CommitMargin` is pinned), the counters `acu_flip_hold_skipped_total`, `acu_flip_quiet_cut_short_total`, `acu_flip_hold_expired_total`, `acu_flip_waiters_over_cap_total`, `acu_flip_slow_anchor_reads_total`, `acu_flip_gate_panics_total` and the gauge `acu_flip_barrier_active`. `freshness.AdoptionConfig.FlipGate` and the nil-safe `(*Adopter).FlipGate()` (the handler shares the adopter's gate: `handler.Config.FlipGate`); `(*Adopter).SetCommitDelayForTest` (test seam: a commit's latency in simulations); `handler.Config.KeepDisplaced`; `cache.DisplacedSnapshotMembers`; `config.AdoptionConfig.FlipQuietPeriod`; the events `adoption_flip_barrier` (outcome `commit_failed` when the commit fails) and `adoption_abandoned_at_shutdown` (a flip abandoned at the signal: INFO, never an adoption outcome); the request fields `snapshot_displaced` and `flip_held_ms`; `acu_serve_byhash_displaced_total{host}`. Snapshot anchors' `Last-Modified` is `adopted_at` alone (never the pool mtime). Rulings: an adoption keeps its global slot through the barrier and frees its host slot before it (R-C1); the barrier's waits ignore `adoption.max_duration` and end on shutdown, committing nothing (R-C2); the admission bound's *A* includes the barrier's ≤ 140 s (R-C3). Five busy suites flipping at once: four hold, the fifth commits unheld (phase C plan, deviation 2).
 - D: `cache.CreateSuite`, `cache.UpdateSuiteFreshness`, `cache.TouchSuite`, `cache.ErrSuiteGone` (the UPDATE-only `CommitAdoption` Step 6, spec C3), `aptpath.SniffRelease`, `aptpath.SniffFlatIndex` (the `InRelease`/`Release.gpg` sniffs and marker writes already exist from A). As built: `cache.CreateSuite(ctx, SuiteSeed, SuiteCaps, SuiteGuard) (CreateResult, error)` (`SuiteSeed.Cached` carries the suite's pre-read cached anchors, `CreateResult.Unproven` asks for the backstop mark, `CreateResult.CandidatesBusy` flags a transient all-busy refusal), `cache.StoreFetched(ctx, FetchedRow, SuiteCaps, SuiteGuard) (FetchedResult, error)` (`FetchedRow.Restamp`: the adoption-disabled refresh miss), `cache.SuiteExists`, `cache.SuiteCapUsage`, `cache.TouchSuite` / `TouchedSuites`, `cache.SuiteGuard` (implemented by `(*freshness.Checker).HoldSuite`) and the optional `cache.ClockStepReporter` (the checker reports eviction's future stamps), unexported `deleteSuiteTx(ctx, tx, key, wantCurrent *int64, cutoff, now) (deleteOutcome, error)` (F's `ReapIdleSuite` reuses it), `freshness.Config.MaxSuites`, `config.FreshnessConfig.MaxSuites`, `aptpath.ClassifySuiteFile` / `SuiteFile`, `aptpath.MaxSuitePathBytes`, `aptpath.DecodableClearsign`, `simtest.ClearsignedFixture`, `(*freshness.Checker).LockCount`. **Signed-seen marker on the miss path (review finding 5, its follow-up and the three-lens review, spec C3):** anchor rows (`InRelease`, `Release`) are created in the writer operation that stores the `url_path` row, before the body is written (sniff → `url_path` + `CreateSuite` + marker → write body → T1, today's seed-before-serve order (the T1 on a creating miss: phase D plan, design note 2; phase E plan, Task 10)); the marker for a sniff-passing `InRelease`/`Release.gpg` is written in that same operation and nowhere else in the handler — no hit-path write (except that every row creation, on a miss or on a hit, marks the suite's cached signed anchors in its own transaction: phase D plan, deviation 2, ruling PD-5), no wait, no coalescing map, no failure counter; a failed operation serves no body (502, as today). `Release.gpg` creates no row. **The backstop and its residual:** the one marker write outside a creating or storing transaction is the handler's `markCachedSignedAnchors`, which runs when a creation is unproven (`CreateResult.Unproven`: one of the suite's cached `InRelease`/`Release.gpg` rows changed, appeared or could not be read between the handler's pre-read and the creating transaction). It reads, sniffs and marks that anchor right after the commit and the late registration, before the response, bounded at 5 s. So an unproven creation's suite is tracked with that anchor unmarked from the commit until the backstop runs, and — if the backstop fails (WARN `suite signed-seen mark failed` or `suite signed-seen backstop read failed`) — until the startup marker pass at the next start (spec C3 accepts it; E carry-forward below). Tests: the marker row exists before the first body byte on first contact and on a refresh miss; a failed writer operation serves no body; the hit-path invariant (after misses, adoptions, `reconstructAnchor` and restarts, every sniff-passing anchor row of a tracked suite has a marker (deviation 1)); FIFO ordering against a later unsigned commit. D also removes reconcile's plain-text fallback in `inReleasePlaintext` once `clearsignedFixture()` replaces the plain-text test fixtures (flip `TestReparseSnapshotRelease_PlainTextInReleaseStillParses` to `_PlainTextInReleaseIsAnError`).
 - E: `freshness` probe classification (`classPresent`, `classAbsent`, `classUnknown`), `Adopter.RunUnsigned`, `aptpath.AnchorPaths(form)` (spec C5). E's unsigned candidates set `Form: cache.FormUnsigned` explicitly, and its tests assert that an unsigned adoption stores `anchor_form = 'unsigned'` and vouches no `.deb`. E also resolves the two `AIDEV-TODO(phase E)` raw-SQL readers (url_path GC guard, `CommitAdoption` anchor seed: for `FormUnsigned`, Step 3c seeds `<suite>/Release`, not `/InRelease`). E's docs note the marker denial-of-service residual (spec C5 threat notes) at `adopt_unsigned`. **In-transaction never-downgrade (review finding 5, spec C5):** `CommitAdoption` re-reads the marker for `aptpath.MarkerBase(suite)` inside its writer transaction and refuses an unsigned candidate (no pointer change, no rows) when one exists or the prior current snapshot is signed; E's DoD pauses an eligible unsigned adoption before commit, serves a signed anchor and completes the marker write (also via the `/.` twin, once G exists — E adds the twin variant as a G DoD item), then resumes and asserts refusal. **Upgrade (review findings 2–3):** a phase-A-quarantined plain-text suite (fixture from A's `buildLegacyFixture` shape) adopts in the unsigned form through `Release` with no upstream change and no `unsigned_downgrade_refused`; a never-adopted suite whose signed anchor was served under a v6 binary is refused unsigned adoption on its first post-upgrade check (A's startup pass wrote its marker). As built: `freshness.classify(res, err, sniff, conditional) probeOutcome` (classes `classPresent`, `classAbsent`, `classUnknown`) and the probe tree `runProbeTree`; `(*Adopter).RunUnsigned(ctx, suite, release, etag, lastmod) error` and `(*Adopter).AdoptsUnsigned() bool`; `aptpath.AnchorPaths(form string) []string` and `(*cache.SuiteSnapshot).AnchorHash(name string) *string`; `cache.ErrUnsignedDowngradeRefused` (the refusal inside the commit) and `freshness.ErrAdoptionUnsignedDowngradeRefused`, `ErrAdoptionUnsignedTooLarge`; `(*freshness.Checker).AdoptionsInFlight(host) bool` and `WaitHostAdoptions(ctx, host, start) bool` (the `handler.AdoptionWaiter` the handler gets as `handler.Config.Adoptions`); `config.AdoptionConfig.AdoptUnsigned` and `freshness.AdoptionConfig.AdoptUnsigned`; the `freshness.Cache` method `ListSuitesWithAdoption` (in place of `ListSuites`); the flip barrier's code in `internal/freshness/flip_barrier.go`; the metrics `acu_repair_deferred_total{host}` and `acu_strict_adoption_waits_total{host}`; spec C5's `no_sha256` coverage rule (E fix round 1): the unexported `freshness.parsePackages` reports the stanzas with a `Filename` but no `SHA256` (`ParsePackages` keeps its signature), and `buildPackageHashes` makes such a snapshot's coverage incomplete (`package_coverage_incomplete`, reason `no_sha256`); the stanzas it reports count toward `MaxPackageStanzas`. G's flat parity row that cites it needs no new code for a file that mixes such stanzas with complete ones; a file in which no stanza has a SHA256 is G's open question (below). The `/.` twin variant of the in-commit race test is G's (below).
-- F: `cache.ReapIdleSuite`, `cache.RetireSuiteSnapshots`, admin `POST /forget`. Non-canonical legacy suite rows (`!aptpath.IsCanonicalSuitePath`) are reaped regardless of idleness (spec C9); the plan review found none in production. The reaper's refcount release reuses `decrementSnapshotRefcountsTx`; snapshot GC sub-job C also deletes the snapshots phase A's audit retired, and sub-job B's `keep_displaced` ranking excludes retired rows so they never take retention slots. The reaper sweeps a signed-seen marker only when no suite row exists for its base or `/.` twin and it is older than 365 days. F's DoD includes a signed snapshot adopted under the phase-A binary, reaped by the F binary, then returning: never-downgrade still refuses unsigned adoption (markers outlive reaping).
+- F: `cache.ReapIdleSuite`, `cache.RetireSuiteSnapshots`, admin `POST /forget`. Non-canonical legacy suite rows (`!aptpath.IsCanonicalSuitePath`) are reaped regardless of idleness (spec C9); the plan review found none in production. The reaper's refcount release reuses `decrementSnapshotRefcountsTx`; snapshot GC sub-job C also deletes the snapshots phase A's audit retired, and sub-job B's `keep_displaced` ranking excludes retired rows so they never take retention slots. The reaper sweeps a signed-seen marker only when no suite row exists for its base or `/.` twin and it is older than 365 days. F's DoD includes a signed snapshot adopted under the phase-A binary, reaped by the F binary, then returning: never-downgrade still refuses unsigned adoption (markers outlive reaping). As built: `cache.ReapIdleSuite(ctx, SuiteKey, wantCurrent *int64, cutoff int64) (deleted, gone bool, err error)` (a failed submit settled by a fresh read; a deleting reap moves the base's markers up to the reap), `cache.ForgetSuite(ctx, SuiteKey, wantCurrent *int64) (ForgetResult{Deleted, Gone, MarkersDeleted, AnchorRowsDeleted, KeptBy}, error)` (with the markers it deletes the cached anchors of the base and its `/.` twin under both schemes, ruling PF-2), `cache.DeleteSuiteMetadata(ctx, SuiteKey) (int, error)` (spec C9 step 1), `cache.ReapRegardless` (`deleteSuiteTx`'s cutoff for a delete regardless of idleness), `cache.ListUnadoptedSuiteAnchors` (`SuiteAnchorRow`), `cache.SweepOrphanMarkers(ctx, now, cutoff int64) (MarkerSweep{Swept, Furthest}, error)` (it never rewrites a stamp, ruling PF-1), `(*cache.Cache).RunRetiredSnapshotGCChunk(ctx, maxRows int) (reaped int, more bool, err error)` (sub-job C), `(*cache.URLPath).LastUse() int64` (the stamp's one Go definition), `(*cache.Cache).HasSuiteRow` (renamed from `SuiteExists`); cap eviction moved to `internal/cache/suite_evict.go` and `deleteSuiteTx`, the one production `DELETE FROM suite_freshness`, to `suite_reap.go` beside its three callers (ruling R-F1). `cache.RetireSuiteSnapshots` was dropped with R-F4's knob-off retirement (ruling PF-5): phase E's PE-12 carry-forward is resolved by PF-5 — with `adoption.adopt_unsigned = false` an established unsigned suite keeps its coherent snapshot, refused `unsigned_disabled`, until the knob is back on or the suite is forgotten, and the docs say plainly that the cache then does not keep unsigned repositories fresh (the maintainer confirmed it, 2026-10-20). `freshness.Reaper`, `NewReaper(ReaperConfig{Cache, Checker, IdleTTL, Logger})`, `(*Reaper).Run(ctx)` (started by `main` beside `Checker.Run`; candidates reaped regardless of idleness first, the junk scan every run, the marker sweep daily, a forward step of the wall clock deferring both by the uptime floor, a suite's held work bounded at 5 minutes) and `(*Reaper).Forget(ctx, scheme, host, suitePath) (cache.ForgetResult, error)`; `admin.Config.Forgetter` (`admin.Forgetter`), `requireMutationGuard`, and the status JSON's additive `suites[].scheme` (the HTML Host cell shows `scheme://host`; ruling PF-7); `config.GCConfig.SuiteIdleTTL` (`gc.suite_idle_ttl`); `gc.LastRunSummary.RetiredReaped`, `acu_gc_retired_reaped_total` and `acu_suites_reaped_total{reason}`; `flipgate.(*Gate).Purge` keeps a live entry (ruling R-F2). Events `suite_reaped`, `suite_forgotten` (`anchor_rows`), `admin_forget_requested`, `signed_seen_markers_swept`; `freshness_check_panicked` gains `where=reaper` and `clock_step_detected` the stamps `last_seen_at` and `wall_clock`. Rulings R-F1–R-F5 and PF-1–PF-9 and deviations 1 (withdrawn: it concerned the dropped retirement), 2 (the coarser 90-day simulation, a four-day one at the production cadence beside it) and 3 (the sweep never rewrites a future stamp) are in the phase F plan and spec revision 9.
 - G: `aptpath.IsFlatSuitePath`, `aptpath.FlatBase`, `aptpath.DotClean`, `freshness.validateFilename`, and the admin `suites[].flat` field (phase B plan, deviation 3). Legacy flat onboarding also writes signed-seen markers for sniff-passing cached flat anchors (the startup pass only covers suites that already have rows), and the `/.` twin variant of E's in-transaction recheck test runs here. **PR #8 evidence in G's DoD:** `TestServeHTTP_FlatRepositoryFreshness` (a miss on `/…/deb/InRelease` and on `/…/deb/./InRelease` creates one suite row each and fires T1 under it; a later hit fires T1 again; `/…/deb/Packages_1.0_all.deb`, host-root `/InRelease` and `/…/deb//InRelease` create none), the pkgs.k8s.io shape `/core:/stable:/v1.34/deb/InRelease` → `/core:/stable:/v1.34/deb`, flat `Packages` failure fatal, and the mixed-host strict matrix. G also ports the rest of PR #8's tests (see "PR #8 parity" below).
 - H: PR #8's end-to-end table in `e2e/flat`: one signed flat repository configured as `deb <uri>/ /` on one client and `deb <uri> ./` on another; both report 2.0 within one cooldown of the v2 publish, and with the upstream stopped two fresh clients install 2.0 from the snapshots. H also runs the live-upstream smoke and documents the strict-mode consequences listed under "PR #8 parity" below.
 
 ## Carry-forwards (from phase A and B reviews)
 
-Each item is owned by the phase it is listed under; that phase's plan addresses it. Items came from the phase A reviews (2026-10-11) unless marked *(B)* (the phase B reviews and its final review, 2026-10-18) or *(C)* (the phase C plan, 2026-10-18) or *(D)* (the phase D plan, 2026-10-18) or *(E)* (the phase E plan, 2026-10-19).
-
-**F:**
-
-- Snapshot GC's `keep_displaced` ranking must exclude retired snapshots, which today occupy slots indefinitely. A quarantined plain-text suite's retired row is otherwise kept forever.
-- `IsCanonicalSuitePath("/foo/dists/InRelease")` is true: reap such suites by idleness.
-- A reaped suite's parked scheduler entry (phase B Task 11b) flushes on its next offer (`ErrNotFound` → `skipped` → done); the reaper need not touch the scheduler, and `acu_freshness_waiting_for_slot` may briefly count a reaped suite. F's tests include one such suite.
-- *(C)* Spec C8: flip-gate entries are purged on reap. After deleting a suite row the reaper calls `Adopter.FlipGate().Purge(flipgate.Key(scheme, host, suitePath))` (nil-safe; a no-op while a flip of the suite runs; idle entries also expire on their own after twice the quiet period).
-- *(D)* `ReapIdleSuite` deletes through `deleteSuiteTx` (`internal/cache/suite_rows.go`), passing the current snapshot it read as `wantCurrent`, and purges in-memory state as cap eviction does: the checker's lock-map entry and the flip-gate entry (hold the suite through the `SuiteGuard`; its `release(true)` after the commit drops both) and the cache's touch entry (`forgetTouch`). Above the global cap, cap eviction removes one suite idle for 24 h without a current snapshot per refused creation (ruling PD-8), so a lowered `max_suites` converges by itself; a host at its per-host cap never evicts for its own creations; it shrinks through the reaper, or when global-cap eviction (whose candidates come from every host) takes one of its idle suites (phase D final review, spec Minor 3).
-- *(D)* `Gate.Purge` (`internal/flipgate/gate.go`) drops an entry unless a flip of it is running, even while registered serves are in flight. Before the reaper relies on it, make `Purge` skip an entry with in-flight tokens or a completion within the quiet period (such an entry expires on its own): today a `release(true)` that runs after a concurrent re-creation of the evictee registered a serve purges that live entry, so a later flip need not wait out that serve's quiet period (phase D final review, concurrency M4; narrow and bounded in D).
-- *(D)* `docs/configuration.md` (the caps paragraph) and `docs/log-fields.md` (the `suite_caps_exceeded` row) say "idle-suite reaping (a later release)": rewrite both when F lands (phase D Task 13 review, minor 4).
-- *(D)* `POST /forget` deletes through `deleteSuiteTx` too (cutoff `math.MaxInt64`, `wantCurrent` = the current snapshot it read), holding the suite through the `SuiteGuard`: otherwise R-D1's "one suite-row delete path" breaks, and so does `ErrSuiteGone`'s "a backstop, not an expected path" (phase D plan, design note 12).
-- *(D)* Legacy junk rows — suites a v6 binary created whose cached `InRelease` is an HTML page — stay alive because every anchor serve with a status below 400 touches its suite, sniff-failing bytes included. Reap them regardless of idleness (like non-canonical rows), or make the anchor touch require that the suite's anchor passed its sniff.
-- *(E)* `docs/configuration.md` (*Anchor forms and unsigned repositories*, the threat notes) and `docs/admin-ui-spec.md` (§4.5, the unsigned and absent-anchor refusals) name `POST /forget` "added with suite reaping in a later release": rewrite both when F lands.
-- *(E)* `POST /forget` is the documented escape hatch for `unsigned_downgrade_refused` (spec C5 threat notes): F's `/forget` re-adoption tests include a suite refused that way whose marker `/forget` deletes (no other suite row with its base), which then adopts unsigned after the two observations, and one whose `/.` twin keeps a row, which stays refused.
-- *(E)* With `adopt_unsigned = false`, F retires each established unsigned suite's current snapshot through the reaper's retirement path, so it is served from the ordinary cache as configuration.md says (phase E ruling PE-12). Until F, such a suite keeps serving its last snapshot, refused as `unsigned_disabled` and paced by its record (`pacedByRefusal`); F also rewrites the "arrives with suite reaping in a later release" texts at the knob in `docs/configuration.md` (the `adopt_unsigned` row, safeguard 1), and its test turns the knob off for an adopted unsigned suite and asserts the retirement, Phase-1 serving and no unsigned update.
-- *(E)* `suite_signed_seen.last_seen_at` is a past-event stamp (spec C4): phase E reads markers only for their existence (unsigned eligibility, `CommitAdoption`'s recheck), and its re-mark (ruling R-E1) stamps the anchor row's last use verbatim, as the startup anchor pass does. F's 365-day orphan-marker sweep is the stamp's first reader: apply the clock-step rule there as for a gate that retires things — a future stamp reads as now, never as old — and heal it on the next observation. This corrects the earlier E carry-forward, which said a future stamp "counts as infinitely old": the sweep retires markers, and reading a future stamp as old would delete signed history after a backward clock step (phase E plan review, M-20).
+Each item is owned by the phase it is listed under; that phase's plan addresses it. Items came from the phase A reviews (2026-10-11) unless marked *(B)* (the phase B reviews and its final review, 2026-10-18) or *(C)* (the phase C plan, 2026-10-18) or *(D)* (the phase D plan, 2026-10-18) or *(E)* (the phase E plan, 2026-10-19) or *(F)* (the phase F plan, 2026-10-20).
 
 **G:**
 
@@ -126,15 +110,22 @@ Each item is owned by the phase it is listed under; that phase's plan addresses
 - *(E)* Spec C4 Dispatch 3: `cachedAnchors` (`internal/freshness/first_adoption.go`) counts only `InRelease` and `Release` rows as client-created (phase E plan, design note 14); G adds flat index rows (`Packages*`/`Sources*`, `request_count > 0`), and the anchorless refresh where the tree finds neither anchor (`errNoAnchor`, a failed check until then) for flat suites with no signed-seen marker and no sniff-passing cached anchor row.
 - *(E)* The nvidia-shaped DoD fixture is `dists/`-shaped (phase E plan, design note 29); G's DoD adopts the real flat layout with its `./` Filenames.
 - *(E)* A `Packages` file in which no stanza has a `SHA256` (only `SHA512`, `SHA1` or `MD5sum`) still fails its parse (`packages: no stanzas with both Filename and SHA256`, `internal/freshness/packages_parse.go`), so its suite's adoption fails as `adoption_parse_failed` at every check, where spec C5 expects the snapshot to adopt with incomplete coverage (`no_sha256`). Phase E made the rule fire only for files that also have complete stanzas (review fix round 1, Task 2). G, which brings the SHA512-only flat repositories most likely to publish such files, decides it: adopt with incomplete coverage (keeping the parse error for a body that is no Packages file at all, e.g. an HTML error page), or keep failing and document it.
+- *(F)* Until G's grammar a flat-shaped suite row is non-canonical (`aptpath.IsCanonicalSuitePath`), so the reaper would reap it at its first run regardless of idleness; none exists, since no release created one. With G's grammar flat suites are canonical and reaped by idleness like any suite, and the reaper's metadata deletion (`DeleteSuiteMetadata`: rows whose `aptpath.SuitePath` is the suite) and junk scan (`aptpath.ClassifySuiteFile` on the anchor rows) start covering them with no code change. G's DoD item "reaping `/repo` leaves `/repo/.` intact" (spec §9) runs `DeleteSuiteMetadata` and the reap on a real flat suite with a `./` twin — `TestDeleteSuiteMetadata_DeletesOnlyTheSuitesRows` and `TestReap_LeavesTwinsAndNestedSuitesAlone` use `dists/` paths and raw `/flat`, `/flat/.` keys whose files no grammar maps to a suite before G — and adds a flat suite, kept while used, to `TestReaper_ReapsIdleAndNonCanonicalSuitesAndPurgesThem`.
+- *(F)* `TestDoD_ForgetIsTheEscapeHatchForADowngradeRefusal`'s twin subtest seeds the row `rigSuite + "/."`, a `dists/` path with a twin suffix no grammar produces before G; G repeats it with a real flat suite whose `./` twin keeps a row.
+- *(F)* The reaper's junk rule (ruling R-F3 with PF-3) considers only `InRelease`, `Release` and `Release.gpg` rows, so an anchorless flat suite whose cached flat `Packages` is an HTML page is never junk, and the hot path keeps serving it (phase F design review, M-17). G decides whether flat index rows count as junk evidence (`aptpath.SniffFlatIndex`), with a test either way.
 
 **H:**
 
 - *(E)* The prose WARN names phase E adds — `freshness: read anchor rows failed`, `freshness: read current snapshot failed`, `adoption: served anchor read failed; the flip barrier runs` and `adoption: signed-anchor re-mark failed` (documented as they are) — may become snake_case in H's documentation sweep, with phase D's (listed under "PR #8 parity").
 - *(E)* `README.md` (spec §8): unsigned and SHA512-only repositories, eager first adoption and the `adopt_unsigned` threat notes, beside the flat forms H documents.
+- *(F)* `README.md` (spec §8, "GC and `/forget`"): suite reaping and `POST /forget` beside *Recovering a degraded repository*, and `/forget` in the sentence that names the admin interface's `POST /reconcile`; `docs/configuration.md` documents both (*Suite reaping* under `[gc]`, *Forgetting a suite* under `[admin]`).
+- *(F)* The admin status page and JSON show no reaping: `gc.LastRunSummary.RetiredReaped` (spec C11) is not copied into the admin GC section, which copies its fields one by one (`internal/admin/status.go`); add `retired_reaped` beside `displaced_reaped` in the JSON, the GC panel and `docs/admin-ui-spec.md` (phase F plan, design note 15).
+- *(F)* The prose WARN names phase F adds — the seven `suite reaper: …` lines and `admin: forget failed` (documented as they are) — may become snake_case in H's documentation sweep, with phase D's and E's.
 
 **Open decision (spec-level, for the maintainer):**
 
 - An `anchor_corrupt` or `anchor_oversized` quarantine leaves the known-corrupt `url_path` anchor in Phase-1 serving, and the checker sees "unchanged". This is pre-existing behaviour for any bit-rotted metadata blob. Decide in a later phase whether the audit should also invalidate that `url_path` row.
+- Host-header validation of the admin listener's mutating endpoints, `POST /forget` and `POST /reconcile` (spec §10 G-1; phase F ruling PF-6): their guard stops a cross-site form or `fetch`, but not DNS rebinding against a listener without `htpasswd_file`. Refusing a non-IP, non-`localhost` `Host` when no htpasswd is configured would close it, but affects operators behind reverse proxies; the guard is one function (`requireMutationGuard`), so the change is small once decided.
 
 ## PR #8 parity (gap analysis, 2026-10-17)
 
```

- [ ] **Step 6: Deferred files**

Each file's diff below deletes the entries this plan resolved (each is one `- **ID** …` line; the section headings stay): A-3 (`…-A-deferred.md`); C-1, C-2, C-3, C-4, C-11 (`…-C-deferred.md`); D-8, D-11, D-12, D-13, D-14, D-15, D-20, D-24, D-25, D-36, D-39, D-40, D-41, D-42, D-43, D-44 (`…-D-deferred.md`); E-4, E-5, E-6, E-7, E-8, E-12, E-15, E-18, E-20, E-26, E-34, E-49, E-72 (`…-E-deferred.md`). The C diff also annotates C-6 (M-13), and the E diff narrows E-2, E-33, E-40 and E-63, corrects E-13's file name and annotates E-32 and E-44. Each file's change is one diff against the file as it was before this step, so no order matters (the draft's separate deletion step made its E narrowing diff fail to apply after the deletions: exec review D2):

```diff
diff --git a/docs/superpowers/plans/2026-09-28-flat-repositories-A-deferred.md b/docs/superpowers/plans/2026-09-28-flat-repositories-A-deferred.md
--- a/docs/superpowers/plans/2026-09-28-flat-repositories-A-deferred.md
+++ b/docs/superpowers/plans/2026-09-28-flat-repositories-A-deferred.md
@@ -8,7 +8,6 @@ Phase A's per-task and final reviews (2026-10-11, Fable max) found these minor i
 
 - **A-1** `wallclock_test.go:27`: `restore()` is called inline, not deferred, so a failing assertion leaks the −2 h offset into later tests.
 - **A-2** No guard test that production code never calls `wallclock.SetOffsetForTest`; the doc should say restores run LIFO.
-- **A-3** GC wallclock stamps (`gc.go:276,362`) are not distinguished from `time.Now` by any test (the handler's are, since phase C: `TestHandler_StampsFollowTheWallClock`).
 
 ## internal/aptpath and internal/proxy
 
```

```diff
diff --git a/docs/superpowers/plans/2026-09-28-flat-repositories-C-deferred.md b/docs/superpowers/plans/2026-09-28-flat-repositories-C-deferred.md
--- a/docs/superpowers/plans/2026-09-28-flat-repositories-C-deferred.md
+++ b/docs/superpowers/plans/2026-09-28-flat-repositories-C-deferred.md
@@ -8,15 +8,11 @@ Left out: everything the phase C final fix wave resolved (the phase-A wake test,
 
 ## internal/flipgate
 
-- **C-1** `gate.go:295-310`: two promised `Close` behaviours are untested — idempotence (a second call would panic on `close(g.closedCh)`) and `Close`'s own broadcast to a flip waiting only in phase A (with a hold in progress `endHoldLocked` broadcasts instead). The nil/disabled guard is covered by the cmd daemon tests. Fix: call `Close` twice in `TestDisabledGate_IsTodaysBehaviour` and the `Close` test; add a `Close` case with only a phase-A flip waiting.
-- **C-2** `gate.go:164`, `:767`: `changedLocked` is called before the break and closed checks, so an idle flip leaves a channel allocated until the next broadcast; "nil while no flip waits" and "only a flip about to wait asks for it" overstate it. Reword, or call it only on the wait path inside the same critical section.
-- **C-3** `gate.go:412`, `:767-779` (performance, final review concurrency Minor 6): every `Token.Done`, for any suite, closes the global `changed` channel, so each waiting flip re-locks, re-scans its suite's in-flight map and allocates a new channel and timer on every anchor completion in the cache, for up to the barrier's 140 s. Cheap today (ruling R-C1: at most one or two waiting flips under the default pool). Per-entry wake channels would remove it; not done in the fix wave because it changes the gate's wake mechanism for no measured gain.
-- **C-4** `gate_test.go` `TestFlip_PhaseAWaitsOutAnotherSuitesHold` asserts only a lower bound; tighten it to `resB.HoldStart.Equal(a.res.HoldEnd.Add(testQuiet))` (the wake itself is pinned by `TestFlip_PhaseAWakesWhenAnotherSuitesHoldEnds`).
 
 ## internal/simtest and the model simulations
 
 - **C-5** `simtest/aptclient.go:92`, `:103-112`: two ledger properties are pinned by no test — `held: heldBefore` (an anchor's own hold must not shorten its index's window) and recording `gens` only inside the window (`AnchorGens` is the "clients crossed the flip" proof). Add an update whose anchor is held 5 s and then followed by an 11 s filler, assert its index is not checked, and assert `AnchorGens` stays empty after out-of-window updates.
-- **C-6** `flipgate/sim_test.go` `clients` and `recordHeld` repeat `gate_test.go`'s `busyReaders` and are copied verbatim as the handler's `flipReaders` and `recordAnchorHeld`; export a periodic-client spawner and `RecordHeld` from simtest.
+- **C-6** (checked by phase F, which changes `gate_test.go`, where `busyReaders` lives, but none of the files the fix belongs to: `flipgate/sim_test.go`, simtest and the handler tests) `flipgate/sim_test.go` `clients` and `recordHeld` repeat `gate_test.go`'s `busyReaders` and are copied verbatim as the handler's `flipReaders` and `recordAnchorHeld`; export a periodic-client spawner and `RecordHeld` from simtest.
 - **C-7** `flipgate/sim_test.go` `clients`: client ids collide across groups running at once (the loop index), so `Straddle.Client` is ambiguous in a failure message; add an id-base parameter.
 - **C-8** `simtest/aptclient.go` docs: say that `RunUpdate` is safe for concurrent use and blocks for the whole update (in real time outside a bubble), and that `AnchorGens` sorts as strings ("10" before "9").
 
@@ -26,7 +22,6 @@ Left out: everything the phase C final fix wave resolved (the phase-A wake test,
 
 ## internal/cache
 
-- **C-11** `adoption.go:720`: "strictly increasing per suite" holds only along the current-snapshot chain. When `current_snapshot_id` is cleared (quarantine today, the reaper next), the first-adoption rule ignores the suite's remaining displaced snapshots, whose `adopted_at` may lead the clock, so GC's `keep_displaced` ranking and retained by-hash's newest-first window could misorder. Take `MAX(adopted_at)` over the suite's adopted snapshots as a floor, or narrow the AIDEV-NOTE. (F)
 - **C-12** `flip_state_test.go:158`, `:174`, `:194`: `restore()` runs after `commitCandidate` instead of being deferred, and `stubNow` (`gc_test.go:42`) registers no cleanup, so a failing stamp test leaves `nowUnix` stubbed for the rest of the package.
 - **C-13** `flip_state_test.go:146-191`: the served-anchor `PutURLPath` setup is written out three times; hoist `putAnchor`.
 
```

```diff
diff --git a/docs/superpowers/plans/2026-09-28-flat-repositories-D-deferred.md b/docs/superpowers/plans/2026-09-28-flat-repositories-D-deferred.md
--- a/docs/superpowers/plans/2026-09-28-flat-repositories-D-deferred.md
+++ b/docs/superpowers/plans/2026-09-28-flat-repositories-D-deferred.md
@@ -21,24 +21,15 @@ Left out: everything the phase D final fix wave resolved — CreateSuite's faile
 
 ## internal/cache
 
-- **D-8** `single_insert_test.go`: the guards check where the INSERT lives and who outside `internal/cache` calls the creators, not in-package callers of `createSuiteTx`; a future writer that "re-creates on 0 rows" through it would fail neither. Fix: an allowlist (`CreateSuite`, `storeFetchedTx`, `SeedSuiteForTest`). Can wait: every current writer has a behavioural UPDATE-only test. (Task 4 minor 3)
 - **D-9** `cache_test.go` (a gofmt drift file: edit by hand): `TestInsertCandidateSnapshot_RejectsDanglingBlobFK`, `_RejectsBothModes` and `_RejectsAllNull` go through `InsertTrackedCandidateForTest` and assert only `err != nil`, so an error from its `EnsureSuiteForTest` step would satisfy them. Fix: call `InsertCandidateSnapshot` directly. (Task 4 minor 4)
 - **D-10** `adoption_failure.go:144`: B-4's `COALESCE` has no test (the only reason-less row has count 0, and `RecordAdoptionFailure` refuses an empty reason). Fix: a raw-SQL row with a count above 0 and a NULL reason. (Task 4 minor 5)
-- **D-11** `suite_rows.go:438`: `reportFutureInterest`'s "furthest in the future" (`ORDER BY last_requested_at DESC`) is unpinned: its test has a single future row. (Task 5 minor 4, the rest of which the fix wave pinned)
-- **D-12** Comments later code builds on are inaccurate: (a) `holdEvictionCandidate`'s doc (`suite_rows.go:360`) says afterCreate "releases an unused candidate as not deleted", but an unused candidate whose row vanished is released as deleted; (b) afterCreate's doc (`:315`) says the purge happens "exactly when the suite has no row", which overclaims — a re-creation of the evictee can land between the commit or the read and the release (final review, concurrency M4); (c) its AIDEV-NOTE (`:322`) calls a commit racing the context's end "the one way a failed submit still commits", but `submitWrite` also returns `ErrClosed` while the writer runs the op with a live context (harmless at shutdown). (F: fix before the reaper reuses the code) (Task 5 minor 5)
-- **D-13** `suite_rows.go`: the method `(*Cache).SuiteExists` shares its name with the `SuiteExists` CreateOutcome constant (the handler uses both); ambiguous to read and grep. A rename (e.g. `HasSuiteRow`) is free before the 1.1.0 RC; since the fix wave it is also in `freshness.Cache`. (Task 5 minor 6)
-- **D-14** `suite_rows.go` holds creation, eviction, deletion, existence, cap usage and freshness updates (about 750 lines). The eviction block (its constants, `ClockStepReporter`, `heldSuite`, `holdEvictionCandidate`, `reportFutureInterest`, `deleteSuiteTx`) would sit naturally in a `suite_evict.go` beside `suite_evict_test.go`. (Task 5 minor 7)
-- **D-15** Errors name the wrong operation: `holdEvictionCandidate`, `createSuiteTx` and `markCreated` errors carry "CreateSuite:" on the miss path too, so a failed miss logs "handler: put url row: CreateSuite: insert: …" (`creationAttempt` names the operation only for begin and commit). Fix: wrap with the operation's name. (Task 6 minor 1)
 - **D-16** `fetched_row.go:44-51`: "a failure stores nothing" holds per attempt only. After a retry (a vanished or, since the fix wave, a kept candidate), the first attempt has committed the url_path row with the suite untracked and no marker; if the second fails, `StoreFetched` returns an error (502) and that row stays. Safe under deviation 1 (the next hit's onboarding marks it through `Cached`); state the per-attempt guarantee in the doc and the AIDEV-NOTE. (Task 6 minor 2)
 - **D-17** `fetched_row.go:19-21`: undocumented precondition on `Create.Cached`: the url_path upsert runs first in the same transaction, so a `Cached` entry for `Row.Path` itself always reads as changed — a spurious `Unproven` and an extra backstop. The only caller excludes that path (`cachedAnchors`' `except`). Document it on `FetchedRow.Create` with an AIDEV-NOTE, or filter it in `storeFetchedTx`. (Task 6 minor 3)
 - **D-18** `fetched_row.go:17-31`: "Create.SuiteKey == Suite" (and Restamp's key) is documented, not checked; a mismatched caller would create one suite, re-stamp another and mark a third, silently. Fix: a guard in `StoreFetched` that returns an error. (Task 6 minor 4)
 - **D-19** Doc nits: `fetched_row.go:12` and `:44` say "a metadata miss", but every miss goes through `StoreFetched`, `.deb` rows included; `FetchedResult.Marked` has no production reader and does not cover `markCreated`'s marks (`fetched_row_test.go` asserts it false while a marker was written) — say "MarkSeenAt's marker was written". (Task 6 minor 8)
-- **D-20** `suite_rows.go` (`creationAttempt`): the exact purge after a failed submit whose eviction did commit (design review M3: afterCreate's re-read) has no end-to-end test. Since the fix wave, dropping the failure-path `afterCreate` still releases the hold (the deferred settle) and survives `TestCreateSuite_AFailedSubmitReleasesItsCandidate`; what it loses is that purge — one lock-map, gate and touch entry left behind, bounded, as after afterCreate's read error. A test needs a commit that races its context's end in `submitWrite`. (Final fix wave, FW-2)
 
 ## internal/freshness
 
-- **D-24** `suite_lock_test.go` (`TestCreateSuite_ReportsAFutureStampThroughTheChecker`): asserts `stamp` and `suite_path` but not `canonical_host`/`canonical_scheme`; `ReportClockStep` passes three same-typed strings by position, so swapping two compiles and passes. (Task 7 minor 4)
-- **D-25** Placement and doc nits: `ReportClockStep` (`suite_lock.go:113`) belongs in `clockstep.go` beside `clockStep`; the three scheduler tests in `suite_lock_test.go` belong in `scheduler_test.go`; `HoldSuite`'s doc names "a check, adoption, repair or reconcile" and the stale entry as the reasons `ok` is false, but not a concurrent `HoldSuite` (a racing eviction, F's reaper) or, since the fix wave, a check's lock-map peek. (Task 7 minor 5 — its other two bullets were fixed in the first fix round; re-review r1)
 - **D-26** `reconcile_r1_test.go` (`DecodesAptsMarkerLine`) never asserts that `bytes.Replace` matched: if the encoder's first line ever ended in "\r\n", the test would pass without exercising the trailing-blank path. Fix: a `bytes.Equal(blanks, signed)` guard. (Task 3 minor 5)
 - **D-27** `reconcile_test.go` (`RejectsCorruptedAnchor`) asserts only `err != nil` while its comment says only the hash check can refuse it; assert the error contains "hash mismatch". (Task 3 minor 6)
 
@@ -49,13 +40,6 @@ Left out: everything the phase D final fix wave resolved — CreateSuite's faile
 - **D-30** `phase2_handler_test.go` (`TestServeHTTP_AdoptedSuiteSnapshotMemberDBError_FailsClosed`): its upstream `/InRelease` body `"attacker bytes"` stays plain text with no PD-2 comment (no effect: the test asserts zero upstream calls; its sibling was wrapped). (Task 3 minor 4)
 - **D-31** Untested: the recover inside the rejection sweep's `expire` itself (its `onPanic` wiring and `Handler.Close` stopping it are pinned since the fix wave), and `New` refusing a negative `MaxSuites`. (Task 9 minor 2, narrowed)
 - **D-32** `suite_rows.go`: `cachedAnchors` and `markCachedSignedAnchors` repeat one loop (iterate, skip `except`, `LookupURL`, classify, `readHead`, sniff); the in-transaction mark and the backstop must agree on what counts as signed. One helper returning `(row, signed, err)` would keep them together. `runFetch` also builds `suiteSeed` twice with identical arguments. Can wait: the copies agree today. (G: if the sniff rules change) (Task 9 minor 3)
-- **D-36** `suite_hit_test.go:699`: `recs[0]["stack"].(string)` panics, instead of failing with the test's message, when the stack is missing; use the comma-ok form. (Task 10 minor 8)
-- **D-39** `suite_dod_test.go:66-67`: "the longest one (30 s)" is wrong: `flipgate.MaxQuiet` is 15 s, and 30 s is the gate's sweep horizon (twice the quiet period). (Task 11 minor 1)
-- **D-40** `suite_dod_test.go` (`TestHitPathInvariant_…`): the check after d's reconstruction is labelled "after serving"; rename it "after d's reconstruction". (Task 11 minor 2)
-- **D-41** `suite_dod_test.go`: no invariant assertion after c's `Release.gpg` hit or after d's adoption (before the delete), though the brief and the test's comment say "after every step". (Task 11 minor 3)
-- **D-42** `suite_dod_test.go`: `h2` omits `MaxSuites` (unbounded in the handler; production validates it to at least 10) while its comment says "a restart with a larger max_suites"; set `MaxSuites: 5000`. (Task 11 minor 4)
-- **D-43** `suite_dod_test.go` (`TestServeHTTP_MintedSuitesAtScale`): `idle` and `h` are closed only on the success path; a failing `get` leaves their T1 goroutines and sweep timers running while the cache closes. A once-guarded close in `t.Cleanup` fixes it. Failure path only. (Task 11 minor 5)
-- **D-44** `suite_dod_test.go`: one `SignedSeen` call passes the raw suite path while the others go through `aptpath.MarkerBase` (equivalent for `dists/` suites); the closure parameter `url` shadows `net/url`; `firstByteProbe` discards `SignedSeen`'s error (an error reads as "marker=false"; the test still fails closed). (Task 11 minors 6–8)
 
 ## cmd/apt-cacher-ultra
 
```

```diff
diff --git a/docs/superpowers/plans/2026-09-28-flat-repositories-E-deferred.md b/docs/superpowers/plans/2026-09-28-flat-repositories-E-deferred.md
--- a/docs/superpowers/plans/2026-09-28-flat-repositories-E-deferred.md
+++ b/docs/superpowers/plans/2026-09-28-flat-repositories-E-deferred.md
@@ -9,31 +9,22 @@ Left out: everything the phase E final fix wave resolved — the two never-downg
 ## internal/cache
 
 - **E-1** `gc_urlpath.go` (url_path GC guard (d)): the rewritten detached arm (`ss.anchor_form = 'detached'`) is not pinned — a typo mutant passes every GC test, because `TestRunURLPathGCBatch_DetachedReleaseAndGPGProtected` stores the snapshot's own blobs and guard (c) keeps them. Fix: a `FormDetached` row in the table test (Release and Release.gpg kept, InRelease reaped), ideally deriving the kept set from `aptpath.AnchorPaths(form)`. Can wait: guards (b) and (c) and anchor reconstruction backstop it. (Task 1 minor 1)
-- **E-2** Stale doc comments after the unsigned form: `adoption.go` (`DeclaredHashesForPath`'s doc says "under any current snapshot"; only signed ones count) and `internal/handler/handler.go` (`classifyStrictMode`: "Host has no adopted suites", which now includes a host whose current snapshots are all unsigned). Say "signed (inline or detached) current snapshot". Comment only. (Task 1 minor 2)
+- **E-2** (narrowed by phase F: `internal/cache/adoption.go`'s `DeclaredHashesForPath` doc was fixed in its Task 5) Stale doc comment after the unsigned form: `internal/handler/handler.go` (`classifyStrictMode`: "Host has no adopted suites", which now includes a host whose current snapshots are all unsigned). Say "signed (inline or detached) current snapshot". Comment only. (Task 1 minor 2)
 - **E-3** `unsigned_form_test.go`: `derefS` is a word-for-word copy of `derefStr` (`suite_rows_test.go`) in the same package. Test-only. (Task 1 minor 3)
-- **E-4** `adoption.go` `CommitAdoption` Step 1a returns `refuseUnsignedDowngrade`'s read errors without the `CommitAdoption:` prefix, so a log line cannot tell the authoritative read from the early exit's. Wrap with `fmt.Errorf("CommitAdoption: %w", err)`. Cosmetic. (Task 1 minor 4)
-- **E-5** `adoption.go` (about 1,100 lines): `rowQuerier`, `CheckUnsignedDowngrade` and `refuseUnsignedDowngrade` are a self-contained unit that fits `signed_seen.go`. Maintenance only. (Task 1 minor 5)
 
 ## internal/freshness — adoption members and SHA512
 
-- **E-6** `adoption.go` (rehash on reuse): a pooled blob whose SHA-512 mismatches returns without the `adoption_member_mismatch` WARN, so the full digests are logged nowhere (the detail carries 12-character `shortHash` values, whose doc promises the WARN has them). Add the WARN with `declared_sha512`/`actual_sha512`. Diagnostics. (Task 2 minor 1)
-- **E-7** `adoption.go`: every reused blob is also hashed with SHA-512, including SHA256-only Releases (all of Debian and Ubuntu); `hashFileDigests`' doc limits it to members that declare both. Keep `hashFile` when `m.SHA512 == ""`. CPU cost only; bundle with the next change to `adoptMember`. (Task 2 minor 2; concurrency can-wait)
-- **E-8** `adoption.go`: `hashFile`'s comment, sitting above `shortHash`, still says `adoptMember` uses it. (Task 2 minor 3)
 - **E-9** `adoption.go`: `adoption_member_skipped` logs `declared_sha256=""` for a SHA512-only member; add `declared_sha512`. (G: SHA512-only flat repositories) (Task 2 minor 4)
 - **E-10** `member_verify.go`: `memberWriter` embeds `*cache.BlobWriter`, exposing `Finalize`/`FinalizeExpectingHash`; only an AIDEV-NOTE keeps a caller from skipping `verifyMember`. An unexported field with forwarded `Write`/`Truncate`/`Written`/`Abort` would enforce it. Plan-mandated shape; optional. (Task 2 minor 5)
 - **E-11** Coverage: no test exercises a size mismatch for a SHA512-only member in `verifyMember` (the `Finalize(size)` path); the "no SHA256 or SHA512 block" and "empty SHA256 and SHA512 blocks" parse errors are pinned by prefix only. (Task 2 minor 6)
-- **E-12** `adoption.go` `tryByHashFetch` returns a `PutBlob` or `newMemberWriter` failure unwrapped; the plain path then wraps the same kind of fault as `ErrAdoptionDBFailed`. Costs at most one extra download, no double WARN. (Task 2 round-2 re-review)
-- **E-13** `adoption.go`: the C-16 WARN `adoption: served anchor read failed; the flip barrier runs` also fires on a routine restart (a stop after the stop check or the re-mark, before the read); skip it on `context.Canceled` or when `p.stopErr(ctx) != nil`, as the final fix wave did for the check's local steps (FW-3 (d)). Shutdown noise. (Task 3 minor 7; concurrency can-wait)
+- **E-13** `flip_barrier.go` (since C-19's file split; this entry said `adoption.go`): the C-16 WARN `adoption: served anchor read failed; the flip barrier runs` also fires on a routine restart (a stop after the stop check or the re-mark, before the read); skip it on `context.Canceled` or when `p.stopErr(ctx) != nil`, as the final fix wave did for the check's local steps (FW-3 (d)). Shutdown noise. (Task 3 minor 7; concurrency can-wait)
 - **E-14** The C-16 WARN and the A-49 DEBUG line (`legacy_audit.go`, the startup marker pass's skipped anchor) are not asserted by any test. (Task 3 minor 6)
 
 ## internal/freshness — unsigned form and markers
 
-- **E-15** Survivor U4: the `!unsigned` guards on `buildSourceHashes` and `buildPdiffHashes` (`adoption.go`) are untested — every unsigned test Release declares only Packages. The read-side filter still keeps those rows from vouching, but hot prefetch sees them. Add a leg whose unsigned Release declares a Sources file and a `Packages.diff/Index` and assert no rows. (Task 3 minor 2; tests lens U4)
 - **E-16** `unsigned.go` re-mark: the twin direction "suite `/flat/.` with the evidence at `/flat/InRelease`" is untested (`range []string{suite.SuitePath, suite.SuitePath + "/."}` survives). No twin row exists before G. (G) (Task 3 minor 3)
 - **E-17** `unsigned_test.go` (`TestRunUnsigned_FailedRemarkRefuses`): `recs[0]["err"] == ""` is false when the key is absent, so dropping `"err", err` from the re-mark's log survives; `TestRunUnsigned_RemarkReadErrorRefuses` checks no `err` at all. Assert a non-empty string. (Task 3 minor 4)
-- **E-18** `unsigned.go` `anchorLastUse` is a verbatim copy of the handler's `lastUse`, and the SQL in `ListTrackedSuiteAnchors` defines the stamp a third time. A `(*cache.URLPath).LastUse()` would leave one Go copy. (F: consolidate when the orphan-marker sweep becomes the stamp's reader) (Task 3 minor 9)
 - **E-19** Survivor W5: `markSignatureSeen`'s base (`aptpath.MarkerBase`) is unpinned — marking `suite.SuitePath` survives, as no twin suite exists before G. (G) (tests lens W5)
-- **E-20** Survivor W7: design note 30's "the checker's marker writer writes at every probe" is unpinned — a write-once mutant (return when `SignedSeen` finds a marker) survives. Sight the signature twice and assert `last_seen_at` advanced. (F: the orphan sweep reads the newest stamp) (Task 6 minor 4; tests lens W7)
 
 ## internal/freshness — the check
 
@@ -42,15 +33,13 @@ Left out: everything the phase E final fix wave resolved — the two never-downg
 - **E-23** `check.go` nits: `result, clear, ok := steadyResult(…)` shadows the builtin `clear`; `checkInline`'s "deferred exactly like the 304 above" (the 304 now lives in `steadyResult`) and the detached steady-result comment are stale; `checkInline`'s doc lists 2-tuples for a 3-value return. (Task 4 minor 3)
 - **E-24** A never-adopted check that ends unknown (for example `Release.gpg` answering 5xx after a 200 `Release`) leaves no record, so it is not paced: every client check past the cooldown re-runs the probe tree, a full `Release` download included. Bounded by the cooldown, and spec C4 keeps T1 on the cooldown only; the permanent case (the knob off) is a paced refusal since ruling PE-12. Document the cost, or gate never-adopted client checks on the failure backoff. (H) (Task 6 minor 1, remainder; spec lens can-wait)
 - **E-25** `scheduler.go` `suiteDue` queues an interested never-adopted suite that has no client-created anchor row every tick; its check reads the row and three url_path rows and ends `skipped`. The Task 6 brief's prose says such a suite is not queued. Cheap; correct the prose or accept it in a design note. (Task 6 minor 7)
-- **E-26** Survivor O2: design note 35 (a `cachedAnchors` read error is a WARN, no write, no backoff) is untested; a mutant calling `failCheck` there survives. (Task 6 minor 8; tests lens O2)
 - **E-27** Survivor C3: `probe` passes `conditional = false` to `classify`; a mutant claiming a conditional request survives. Only a misbehaving upstream that answers 304 without validators reaches it. (tests lens C3)
 - **E-28** Survivors X2/X3: the `crossForm` marking in `anchorFoundAgain` and `upgradeProbe` is unpinned (I-1 is pinned only through `lateralInline`). Low impact: those requests' validators belong to an absent anchor, or are the same file. (Task 7 minor 1; tests lens X2, X3)
 - **E-29** Held cross-form content is fetched in full at every check: an unsigned suite whose upgrade fails (a key the cache does not trust) GETs `InRelease` without validators and rewrites the marker at every client check past the cooldown (concurrency E10: 30 of 30), and `lateralInline` likewise re-downloads a held `InRelease`. Bounded per cooldown and visible. Follow-up: pace the unsigned-upgrade case as PE-2 does — no freshness lost, since `upgradeProbe` returns done while a signature is present. (Task 7 minor 3; concurrency and spec can-wait)
 - **E-30** `scheduler.go` tick: the stale WARN's `adopted_stale` no longer equals the gauge's `adopted="true"` value (it leaves out `anchor_absent`); consider an `absent_stale` field. (Task 7 minor 2, remainder)
 - **E-31** Two "signed current" predicates: `Reconcile` uses `snap.Form != cache.FormUnsigned` and drops the read error unlogged (fail-closed), `baseline` uses inline-or-detached. One helper. (Task 7 minor 6)
-- **E-32** Repetition: the hold predicate `gate.active && !gate.due && key == gate.hash` is spelled out four times (a `retryGate.holds(key)` method); the answered-request block (prevSuccess, bump, setChangeSeen, admit, finishCheck) three times. (Task 7 minor 7)
-- **E-33** Placement: `hostCounter` (a shared helper used by `capacity_test.go`) lives in `current_form_test.go`; `unsignedRefusal` has callers outside first adoption but lives in `first_adoption.go`. (Task 7 minor 8)
-- **E-34** `first_adoption.go` `checkFirst` doc: "the latter has no record, so it is never paced anyway" — `pendingChange` is computed at enqueue time, so a client check's failed adoption between the enqueue and the dequeue makes the pending-change check run the tree where it was paced before. At most one probe per race. Say "has no record when queued". (Task 6 round-1 re-review)
+- **E-32** (checked by phase F, which changes `first_adoption.go`, one of the predicate's six sites, but neither `check.go` nor `current_form.go`, where the helper and the other sites are) Repetition: the hold predicate `gate.active && !gate.due && key == gate.hash` is spelled out four times (a `retryGate.holds(key)` method); the answered-request block (prevSuccess, bump, setChangeSeen, admit, finishCheck) three times. (Task 7 minor 7)
+- **E-33** (narrowed by phase F: `unsignedRefusal` moved to `unsigned.go` in its Task 14) Placement: `hostCounter` (a shared helper used by `capacity_test.go`) lives in `current_form_test.go`. (Task 7 minor 8)
 - **E-35** `internal/handler/phase2_migration_test.go` (`waitForMigrationFlip`): "any non-NULL value indicates the first adoption has flipped" sits directly above the newer wait-for-the-snapshot text it contradicts. (Task 6 minor 6, remainder)
 
 ## internal/freshness — retry records, scheduler and panics
@@ -59,11 +48,11 @@ Left out: everything the phase E final fix wave resolved — the two never-downg
 - **E-37** B-38 remainder (spec M3): the future `last_success_at` is reported (`clockStep`) before the cooldown gate in `checkLocked`, and several answers put the future stamp back (a queued result, `anchor_absent`, a failed check), so every non-busy T1 increments `acu_clock_steps_detected_total` until an answer heals it — on a busy suite more often than the per-tick count B-38 removed. Move the block below the cooldown gate and reword the comments ("each check that runs reports it until a successful answer rewrites the stamp"). Metric noise only. Re-filed. (Task 8 minor 3; spec M3)
 - **E-38** `scheduler.go` `Run`: B-45's wiring is untested — dropping the generation stamp at dispatch, or passing `d.item.gen` to `finish`, passes every scheduler test; only `finish` is tested with a generation passed by hand. A Run-level test whose lost offer's done arrives after the release's wake was consumed. (Task 8 minor 1)
 - **E-39** `scheduler.go` `refreshSigned` demotion is untested (a never-demote mutant survives; design note 25 claims both directions). (Task 8 minor 2)
-- **E-40** Small stale docs: `freshness_test.go` (`listHook`'s comment names `ListSuites`), `internal/cache/types.go` (`SuiteWithAdoption` "for the status-page render"), `scheduler.go` (`held`: "not offered until unhold"; `pending`: "class-queued suites", though `enqueue` also marks waiting and in-flight duplicates, and "(pop and done drop it)" overstates `pop`; `finish`'s doc omits B-45's unheld case, which the inline comment states). (Task 8 minor 4; Task 8 round-1 re-review)
+- **E-40** (narrowed by phase F: `internal/cache/types.go`'s `SuiteWithAdoption` doc was fixed in its Task 9) Small stale docs: `freshness_test.go` (`listHook`'s comment names `ListSuites`), `scheduler.go` (`held`: "not offered until unhold"; `pending`: "class-queued suites", though `enqueue` also marks waiting and in-flight duplicates, and "(pop and done drop it)" overstates `pop`; `finish`'s doc omits B-45's unheld case, which the inline comment states). (Task 8 minor 4; Task 8 round-1 re-review)
 - **E-41** `sim_burst_test.go` (`TestSimCapacity_UpgradeBurstSameHost`, docstring item 4): "each lost offer coincides with another admission" — `hx`'s coincides with the test's synthetic lease, which the admission count does not include. Wording only. (Task 8 round-1 re-review)
 - **E-42** `adoption_failure.go` `recoverAdoption`: `reportPanic` runs first inside the single `unloggedPanic` closure, so a panicking log handler also skips `acu_adoption_total{panicked}`, the duration histogram and the ring event. Put only `reportPanic` under the boundary. (Task 13 minor 2; concurrency can-wait)
 - **E-43** Tests: B-18's second site (the record's recover) is untested (a bare `reportPanic` survives); the B-20 test asserts only the absence of a WARN, so it passes if the line disappears or the demotion keys on the write's context. Assert exactly one DEBUG line. (Task 13 minor 3)
-- **E-44** `freshness.go`: `panicStackLogged`'s kinds omit heartbeat, record and reschedule; "One helper for every recover in the package" overclaims (`legacy_audit.go` and `reconcile.go` turn panics into errors). (Task 13 minor 4, remainder)
+- **E-44** `freshness.go`: `panicStackLogged`'s kinds omit heartbeat, record and reschedule; "One helper for every recover in the package" overclaims (`legacy_audit.go` and `reconcile.go` turn panics into errors). Phase F adds the kind `reaper` (`Reaper.safeRun`, `reaper.go`); `freshness.go` changed there only by a rename (ruling PD-1). (Task 13 minor 4, remainder)
 - **E-45** A sibling of B-18: `recoverGoroutine` (repair and reconcile goroutines), `safeReschedule` and `safeTick` have no second recover boundary, so a panicking log handler escapes them (into `Run`, which has none); `checkWorker` open-codes `unloggedPanic`. Wrap them uniformly. Needs a panicking log handler. (Task 13 minor 5; concurrency can-wait)
 - **E-46** `deferred_e_test.go` groups tests by provenance, not subject (jitter, legacy audit, reconcile, heartbeat, `runRequest`); move them beside their subjects when phase H deletes the deferred files. (H) (Task 13 minor 6)
 
@@ -71,7 +60,6 @@ Left out: everything the phase E final fix wave resolved — the two never-downg
 
 - **E-47** `retry_rig_test.go`: the rig start `1_800_000_000` is a literal in two places (hoist a `rigStart` const); `rigVerifier.n` is reset by the same three lines in two helpers (a `resetCalls()`). (Task 5)
 - **E-48** `internal/handler/phase2_detached_chaos_test.go` hand-builds the armored-signature shape `armoredSig` builds in freshness; a `simtest.ArmoredSignatureFixture(label)` beside `ClearsignedFixture` would give one definition of a sniff-passing `Release.gpg`. (Task 5)
-- **E-49** `scheduler_test.go` `seedFakeSuite` with a nil `lastSuccess` stamps interest at the real `time.Now()`; a future caller with a fixed `now` more than 7 days away would be dormant. Nothing is affected today. (Task 5)
 - **E-50** `sim_retry_test.go` (`TestSimRetry_LongRunDateRulesAndClockSteps`): the liveness floor is carried by the failing phases (276 of 317 checks); each 2-day clock-step stretch gives 18, below the 10-a-day floor. Give each stretch its own floor (≥ 16). (Task 14 minor 2)
 - **E-51** `sim_sched_test.go` `renderedMetricLabels` is a fifth render-and-scan metrics helper (`renderedMetric`, `checkTotal`, `hostCounter`, `adoptionTotalFor`); its call site drops the `ParseFloat` error, so a missing series reads 0 with a misleading message. (Task 14 minor 3)
 - **E-52** `sim_retry_test.go` (B-56): the doc says both legs check that content dated more than 10 s ahead is `release_date_in_future`; only the backward leg does. Add the `ahead` case to the forward leg or reword. (Task 14 minor 4)
@@ -88,7 +76,7 @@ Left out: everything the phase E final fix wave resolved — the two never-downg
 - **E-60** `serveCacheMiss` overwrites `shared` and `waiters` with the retry's call: the first call's `singleflight coalesced` line is dropped, and a joiner refused off the first call gets no `unvouched_deb_refused` event. Log attribution only; add the AIDEV-NOTE sentence. (Task 9 minor 6)
 - **E-61** `internal/freshness/inflight.go` `wait` wakes only when a host's count reaches 0, so with overlapping signed adoptions a request whose snapshot committed early sleeps the full bound (documented in configuration.md). Waking on every `end` would respond sooner. (Task 9 minor 7)
 - **E-62** `internal/freshness/inflight.go`: a parameter named `max` shadows the builtin in a file that uses `min`; `maxWait`. (Task 9 minor 8)
-- **E-63** `handler.go` `AdoptionWaiter.WaitHostAdoptions`'s doc lists two false cases (the cap, under 1 s left) but not the third (the context ended during the wait), documented on `Checker.WaitHostAdoptions`. Behaviour is unaffected (`clientLeft` runs whatever the wait reports). Also one 91-column line in spec C10. (Task 9 round-1 re-review)
+- **E-63** `handler.go` `AdoptionWaiter.WaitHostAdoptions`'s doc lists two false cases (the cap, under 1 s left) but not the third (the context ended during the wait), documented on `Checker.WaitHostAdoptions`. Behaviour is unaffected (`clientLeft` runs whatever the wait reports). (Narrowed by phase F: the 91-column line in spec C10 was rewrapped in revision 9.) (Task 9 round-1 re-review)
 - **E-64** A creating miss whose late registration finds a snapshot current (it waited in a hold, or a flip committed just before it — re-dispatched since the final fix wave, FW-4 (c)) fires T1 twice: `trySnapshotHit`'s and the deferred seed T1. The second is a cooldown skip. Say so in the T1-on-seed comment, or skip the seed T1 when the re-dispatch served. (Task 10 minor 1; concurrency can-wait)
 - **E-65** Unpinned: widening `created` to `|| stored.Create.Outcome == cache.SuiteExists` survives every T1/seed/onboarding test (no test runs two concurrent first-contact misses on different files of one suite); design note 32's "a deadline already past fails the backstop at once" (a floor in `missBackstopDeadline` survives); D-34's "the shutdown cuts both short" for onboarding's backstop. (Task 10 minor 2)
 - **E-66** `seed_t1_test.go` X27 (coalesced creating miss) depends on two misses coalescing within a real 300 ms upstream delay; a flipStack in a bubble with a recording freshness double is deterministic. Passed 20 of 20 under load; same class as the strict-wait window FW-4 (d) closed. (Task 10 minor 3)
@@ -100,7 +88,6 @@ Left out: everything the phase E final fix wave resolved — the two never-downg
 
 ## internal/config and cmd
 
-- **E-72** `internal/config/config.go`: the validation cites "Spec C5 D6" where the field comment cites "spec C5 safeguard 1, C11". Pick one form. (Task 11)
 - **E-73** `cmd/apt-cacher-ultra/adopt_unsigned_wiring_test.go` matches the JSON substring `"level":"WARN","msg":"suite cap usage read failed"`, which depends on slog's key order; decode the line. (Task 11)
 - **E-74** Note: cmd tests built on `minimalCfg` (a struct literal, not `defaultConfig`) run with `AdoptUnsigned = false`, while production defaults to true — for anyone writing cmd-level unsigned tests that expect the default. (Task 11)
 
```

Check: `grep -c -E '^- \*\*[A-E]-[0-9]+\*\*' docs/superpowers/plans/2026-09-28-flat-repositories-[ABCDE]-deferred.md` printed 21, 14, 17, 43 and 77 at `07cf306`, and prints 20, 14, 12, 27 and 64 after this step.

- [ ] **Step 7: Final verification**

Run:

```bash
go vet ./...
golangci-lint run ./...
gofmt -l .
go test -race -count=1 ./...
grep -n 'later release' docs/configuration.md docs/log-fields.md docs/admin-ui-spec.md
grep -n 'go func' internal/freshness/*.go internal/gc/*.go | grep -v '_test.go:'
go test -race -count=1 -run 'TestSim|TestDoD_|Scale|AtTheGlobalCap' -v ./internal/freshness/ ./internal/handler/ ./internal/flipgate/ 2>&1 | grep -E '^--- (PASS|FAIL)'
```

Expected: PASS everywhere; vet clean; lint `0 issues`; `gofmt -l` lists exactly the five drift files (`cmd/apt-cacher-ultra/ca_print.go`, `internal/cache/cache_test.go`, `internal/cache/gc_urlpath_test.go`, `internal/freshness/hot_prefetch_test.go`, `internal/proxy/connect_stats.go`); the `later release` grep prints nothing; the `go func` grep (test files filtered out: exec review D7) lists the six production sites it listed at `07cf306` (`freshness.go` three, `scheduler.go`, `adoption.go`, `gc/pool_scan.go`), each recovering: the reaper starts no goroutine (its runs are `safeRun` calls on the goroutine `main` starts beside `Checker.Run`); every simulation, DoD and scale test but `TestSimScheduler_FiveHundredStallingOneHealthy` (C-38, phase H) under 4.5 s (phase E ruling PE-8: the 5 s budget with about 0.5 s of margin). Measured in the revision's run under `-race` (load average 2.3, not quiet): `TestSimScheduler_FiveHundredStallingOneHealthy` 7.00 s (C-38); `TestSimRetry_LongRunDateRulesAndClockSteps` 3.63 s; `TestSimScheduler_StallingHostWithAdoptionContention` 3.54 s; `TestSimFlip_DetachedBusySuiteStraddlesOnlyWithoutTheBarrier` 3.40 s; `TestDoD_NinetyDaysOfReapingWithRestarts` 3.35 s; `TestServeHTTP_MintedSuitesAtScale` 2.91 s; `TestDoD_ReapRacesAdoptionsAndClients` 1.77 s; `TestDoD_ProductionCadenceAcrossTwelveHourlyRestarts` 0.95 s; `TestDoD_ForwardStepFalseReapsSelfHeal` 0.94 s; `TestServeHTTP_OnboardingAtTheGlobalCapEvictsUnderTheGuard` 0.46 s; every other one at most 3.13 s.

Then every test phase F adds or changes runs five times: for each package, `go test -race -count=5 -run '<its new and changed tests>' <package>` (the revision's `count5.sh` derives the lists from `git diff 6a58100..HEAD`: a changed test function, or every test of a file whose helper changed). Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add docs/
git commit -m "Document suite reaping and POST /forget; record phase F's rulings in the spec (revision 9), update the overview and deferred files"
```
