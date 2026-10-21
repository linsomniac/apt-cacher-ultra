# Flat repositories — Phase G: Flat repositories — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make flat apt repositories first-class (spec §9's G row): a flat directory is a suite (C2's flat grammar, its `/.` twin a suite of its own), its root `Packages*`/`Sources*` are IndexTargets on the adoption and handler sides (C5), its Packages Filenames — `./`, `%`, parent-base and host-root shapes — adopt and vouch (`validateFilename`, C6 materialisation, the suffix fallback, coverage), a Release-less flat repository refreshes by revalidating its cached indexes (C4's anchorless refresh), and strict mode protects flat hosts under the host-wide rule (C10). PR #8's evidence and its remaining tests are ported with our results (plan overview, "PR #8 parity" items 1–4).

**Architecture:** `internal/aptpath` gains the flat grammar inside `SuitePath` and the helpers the other packages share — `IsFlatSuitePath`, `FlatBase`, `DotClean`, `IsFlatHostRootPath` — so every consumer that already keys on `SuitePath` (the handler's suite files, touches and onboarding, `DeleteSuiteMetadata`, `IsCanonicalSuitePath` and with it the reaper's non-canonical class) starts covering flat suites with no change of its own. In `internal/freshness` the IndexTarget predicates take the suite path and add the root shapes only for a flat suite; `ParsePackages` validates Filenames with `validateFilename` and parses a body whose stanzas all lack a SHA256 (ruling R-G1); `buildPackageHashes` materialises a flat suite's rows at `FlatBase(suite) + "/" + F` with `package_hash.flat_filename = DotClean(F)`. `cache.DeclaredHashesForPath` keeps its exact lookup and, only when it finds nothing, runs C6's suffix fallback over the v7 partial index. The checker's Dispatch 3 counts a flat suite's client-created index rows, and a flat suite with no signed evidence whose probe tree finds neither anchor runs the anchorless refresh (`internal/freshness/anchorless.go`) through two new cache operations; url_path-served metadata answers `Last-Modified = max(pool mtime, last_fetched_at)`, with `runFetch`'s finalize-before-stamp order pinned (ruling R-G3). The reaper treats an anchorless flat suite's cached indexes as junk evidence (ruling R-G2). The admin status JSON and page gain `suites[].flat`. Twins are two suites everywhere (ruling R-G4), each pinned by a test with a real flat suite and a real `./` twin.

**Tech Stack:** Go 1.25 (toolchain 1.26), modernc SQLite, `testing/synctest`.

**Spec:** `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (revision 9; Task 16 records this plan's rulings, design notes and deviations as revision 10) — §9's G row: the C2 flat grammar, flat IndexTargets (layout-aware, adoption and handler sides), `validateFilename`, C6 materialisation, fallback and coverage, the C4 anchorless refresh, the flat root-index touch, flat pdiff arch labels. Read in full: C2, C3 (the flat rows, onboarding and touches), C4 (dispatch, the anchorless refresh), C5 (flat IndexTargets and Filenames, `no_sha256`), C6, C9 (twins, reaping flat suites), C10 (strict mode, the host-wide rule), C11, §3, §7 (G's tests), §11. Overview: `docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md` — Global Constraints, Deliberate deviations, Review Focus items 1 and 3, the "G:" line under "Later phases add", the whole "G:" block under "Carry-forwards" (from B, C, D, E and F) and "PR #8 parity" items 1–4. PR #8 itself is the local branch `pr-8-review` (head 6ec5dee): its tests are ported with our results; its code is evidence of real repository shapes, not a design (its directory-scoped strict mode is deliberately not reimplemented: spec C10). Deferred minors: `…-A-deferred.md` through `…-F-deferred.md` (IDs cited per task; Task 16 lists the ones taken and the ones checked and left, and deletes the taken ones).

## Global Constraints

Everything in the overview's Global Constraints applies. In particular:

- No schema change: v7 already holds `package_hash.flat_filename` and its partial index `idx_package_hash_flat_filename` (spec §6). Never edit an entry of `migrations`.
- Production SQL never uses SQLite clock functions; persisted stamps come from `wallclock` (the cache's `nowUnix`, the checker's `now`). In-memory timing is monotonic.
- `internal/cache` never imports `internal/freshness` or `internal/proxy`; shared path logic lives in `internal/aptpath` (standard library only: the flat grammar uses `regexp`, which `TestAptpathImportsOnlyTheStandardLibrary` allows). The writer goroutine never runs foreign code or pool I/O: the anchorless refresh writes its new blob into the pool before it submits, and the reaper's flat-index sniffs read pool files on the reaper's goroutine (ruling PD-5).
- Every phase F rule about suite-row deletes holds: one delete path (`deleteSuiteTx`, ruling R-F1), deleters hold the suite through the `SuiteGuard`, every delete moves the base's markers up (PF-10) and turns signed snapshot history into a marker (PF-12). This phase adds no suite-row delete; the anchorless refresh deletes url_path rows only (through `deleteCachedRowTx`, which releases each row's pin once).
- Constants (spec C2, C6, C11 and this plan's design notes), not knobs: the suffix fallback's 32-segment bound; the anchorless refresh's body cap 64 MiB (design note 12); `flat_host_root_unsupported` at most one line a minute (design note 2). No new config key.
- Tests that call `wallclock.SetOffsetForTest` never use `t.Parallel()`. Simulations run inside `testing/synctest` bubbles, finish in under 4.5 s under `-race` on a quiet machine (ruling PE-8), sample goroutines only after `synctest.Wait()`, and silence loggers (`simtest.DiscardLogger()`, `silentLogger()`, `discardLogger()`), or capture them.
- Every goroutine started under `internal/freshness` or `internal/gc`, and every new handler or admin goroutine, recovers panics. This phase starts none: the anchorless refresh runs inside the check (under its recover), the flat-root log line is synchronous.
- `go test -race ./...` and `golangci-lint run ./...` are clean; `gofmt -l .` lists only the five known drift files (`cmd/apt-cacher-ultra/ca_print.go`, `internal/cache/cache_test.go`, `internal/cache/gc_urlpath_test.go`, `internal/freshness/hot_prefetch_test.go`, `internal/proxy/connect_stats.go`) at the end of every task. Never run `gofmt -w` on those five; edit them by hand.
- Commit trailer: the implementer's own harness attribution line (phase A ruling); otherwise `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

Conventions in this plan:

- Each task's **Interfaces** block lists what it uses from other tasks and existing test files. Never redefine a helper: a second definition in the same package does not compile.
- Each commit step starts with `gofmt -l <files>`; it must print nothing. Run `gofmt -w` on the non-drift files a step edits before that check; never on the five drift files.
- A "verify failure" step whose expected result is a compile failure is complete when the compiler names the undefined identifiers listed; such a step runs `go test -gcflags=-e …` so every error is listed.
- Mutation checks are run one at a time, each reverted before the next; record each failing test line in the commit message. Every mutation table in this plan was replayed on a scratch copy of the tree (the red steps and the green runs too); each row names the test line that failed there.
- Edits to existing files are given as exact `old`/`new` blocks (or a whole function that replaces the one of the same name); each `old` block occurs once in the file as it stands after the previous tasks.
- **Existing tests that change.** The flat grammar makes the key `/flat/.` canonical, so `TestDeleteSuiteMetadata_ANonCanonicalKeyDeletesNothing` (Task 1) uses a twin of a twin (`/flat/./.`), which no grammar produces, for its non-canonical flat key. The IndexTarget predicates gain a suite-path argument (Task 2): their table tests (`TestIsIndexTarget`, `TestIndexTargetGroup`, `TestArchFromFilteredPath`, `TestMissingRequestableIndexGroups`, `TestMissingRequestableMembers`, the handler's `TestIsIndexTargetPath`) pass a `dists/` suite for their existing rows and gain flat rows. `ParsePackages` accepts a body whose stanzas all lack a SHA256 (ruling R-G1): `TestParsePackages_NonEmptyButNoUsableStanzas` keeps its no-Filename body, and its comment no longer calls an empty component an error. url_path metadata answers `max(pool mtime, last_fetched_at)` as `Last-Modified` (Task 10): no existing test pinned the pool mtime alone. The admin JSON gains `suites[].flat` (Task 14): `TestJSONContractPreserved`'s golden is regenerated, an additive change the contract allows. Existing tests the deferred minors and carry-forwards strengthen, each named in its task: Task 8's onboarding backstop test (`suite_rows_test.go`), Task 10's `rigUpstream.before` and `rigStart` (E-47, `retry_rig_test.go`), Task 11's `TestServeHTTP_UnsignedNeverChangesStrictPosture` (E-70), Task 12's `TestReaper_ReapsIdleAndNonCanonicalSuitesAndPurgesThem` (flat rows), Task 13's `TestCheck_UnsignedRefusedInsideTheCommitIsAState` (the twin variant), `TestRunUnsigned_RemarkMarksTheTwinsBase` (E-16) and the re-mark fault assertions (E-17), and Task 14's `TestSuitesTable_RendersRecordsAndCounts` and the JSON fixture (B-59). Any other existing test that fails means the change is wrong.

## Rulings and design notes

Controller rulings (binding):

- **R-G1 — a `Packages` file with no SHA256 stanza (the E carry-forward).** A body with at least one stanza that has a `Filename`, where no stanza has a `SHA256`, parses — no rows, every such Filename reported — and its snapshot adopts with incomplete coverage (`package_coverage_incomplete`, reason `no_sha256`), as spec C5 expects. A body with no stanza that has a `Filename` (an HTML error page, a foreign file) keeps failing its parse (`adoption_parse_failed`). Why: a well-formed SHA512-only repository must not fail at every check. Cost if wrong: such a suite vouches no `.deb`, so strict mode passes its host through (spec C10's host-wide rule, already documented for an incomplete `dists/` snapshot). Both sides and the stanza cap are pinned (Task 3). A whitespace-only body was never an error (an empty component's `Packages` is a real, empty index: `TestParsePackages_EmptyInputIsZeroResult`, `TestParsePackages_AllWhitespaceIsZeroResult`) and stays one that parses to nothing: the ruling's "empty" is read as a non-blank body with no Filename stanza (design note 5).
- **R-G2 — junk evidence for anchorless flat suites (the F carry-forward).** For a flat suite with no cached anchor row, its cached flat index rows (`Packages*`, `Sources*`) are junk evidence under the same rules as anchors (ruling PF-3: the row fails its sniff, `aptpath.SniffFlatIndex`; fetched at least 24 h ago; whatever the suite's record). A row the sniff cannot judge — a missing or unreadable pool file, a suite path over `MaxSuitePathBytes` — is no evidence of junk. A flat suite that has anchor rows is judged by its anchors alone, as today. Pinned both ways (Task 12): a junk anchorless flat suite is reaped, and a good one, or one fetched under 24 h ago, is kept. Safe as built: the evidence is one more indexed read in a run that has an anchorless flat suite (14 key probes per unadopted flat suite, design note 15; ruling R-G5), and a false positive costs the suite one fetch at its next request.
- **R-G3 — the first-adoption `Last-Modified` floor (the C carry-forward).** url_path-served metadata answers `Last-Modified = max(pool mtime, last_fetched_at)` (spec C4, the anchorless refresh; Task 10), so `adoptionStampTx`'s first-adoption floor (`MAX(last_fetched_at) + 1`) stays above it only while a pool file is never newer than its row's `last_fetched_at`. The order is kept: `runFetch` finalizes the pool file before it stamps the row, and so does the anchorless refresh; blob mtimes are not folded into the floor. Pinned by `TestServeHTTP_AMissStampsItsRowAfterItsPoolFile` (a slow upstream body: the row's stamp is never older than its pool file) and AIDEV-NOTEs at `runFetch`, at the anchorless refresh's store (`cache.RefreshURLPathBlob`), at `urlPathModTime` and at `adoptionStampTx` (Task 10).
- **R-G4 — twins.** A flat suite `/repo` and its `/.` twin `/repo/.` are two suites, each with its own row. Every carry-forward and survivor about twins becomes a test with a real flat suite and a real `./` twin: E's in-transaction recheck through the twin (Task 13), E-16 and E-19 (Task 13), F's `/forget` escape-hatch twin subtest (Task 12), "reaping `/repo` leaves `/repo/.` intact" (Tasks 1 and 12), the reaper's flat cases (Task 12), and the handler's evidence (Task 7: a miss on each creates its own row and fires T1 under it).
- **R-G5 — the simulation budget (F-12).** `TestDoD_NinetyDaysOfReapingWithRestarts` and `TestServeHTTP_MintedSuitesAtScale` (3.75–3.96 s under `-race` on phase F's machine) get no flat cases: the minted-suites DoD for flat suites is a sibling, `TestServeHTTP_MintedFlatSuitesAtScale` (Task 15; 1.5–1.8 s under `-race` on the plan's machine, where phase D's took 2.5 s). The 90-day simulation also pays for whatever the production reaper does per run, so it was measured after Task 12: an unconditional read of the flat index rows took it from 3.0 s to 3.65 s under `-race` here (about 20 %, past the budget on phase F's machine), so the read runs only in a run that holds an unadopted flat suite with no anchor row (`hasAnchorlessFlatSuite`), and the simulation stays at 3.0 s.

Rulings of earlier phases this plan applies: PD-1 (a file changed only by a mechanical rename does not count as touched for the deferred-minor rule), PD-5 (no pool I/O on the writer), PD-9 (a suite path over 1,024 bytes is no suite), PE-8 (the 4.5 s simulation budget), PF-3 (the junk content rule, extended by R-G2), PF-11 (one field added to one log line touches that function, not the file), R-D3 (the handler already routes flat indexes: phase G adds no handler code for them, only their tests).

Design notes (choices the spec leaves open — the boring, bounded option, with the reason):

1. **The flat grammar's shape** (spec C2). A path with a `dists` segment anywhere runs the `dists/` grammar alone and is never flat. Otherwise the member is the path's last one, two or three segments — one flat member name (`InRelease`, `Release`, `Release.gpg`, `Packages*`/`Sources*` with no or one of the six compression extensions, `Contents-[a-z0-9-]+` likewise), a pdiff Index (`(Packages|Sources).diff/Index`) or a by-hash name (`by-hash/SHA256/<64 hex>`, `by-hash/SHA512/<128 hex>`) — and the suite is the literal directory before it, which may end in exactly one `/.` and is never the host root. `IsFlatSuitePath(s)` is `IsCanonicalSuitePath(s)` without a `dists` segment; `IsCanonicalSuitePath` itself is unchanged (`SuitePath(s + "/InRelease") == s`) and so covers both grammars. `FlatBase` strips one trailing `/.` (it equals `MarkerBase`; two names for C6's package base and C3's marker key). `DotClean` removes `.` segments and nothing else: the callers refuse `..` and empty segments themselves.
2. **`flat_host_root_unsupported`** (spec C2) is a WARN, at most one line a minute process-wide with the count of the lines it suppressed (`suppressed`), as `suite_seed_rejected` is: fields `canonical_host`, `path`, `suppressed`, `detail` ("a flat repository at the host root is not tracked: it is served from the ordinary cache and never refreshed"). It is emitted for a metadata request whose path is a flat member at the host root (`aptpath.IsFlatHostRootPath`: `/InRelease`, `/./Packages.gz`, `/by-hash/SHA256/<hex>`), before the cache lookup; the request is served as before (Phase 1). WARN, not INFO: the repository will never refresh, and the operator can only learn it here. Task 6.
3. **Layout-aware IndexTargets** (spec C5). `isIndexTarget`, `indexTargetGroup`, `archFromFilteredPath`, `missingRequestableIndexGroups` and `missingRequestableMembers` take the suite path. A flat suite's IndexTargets are its root `Packages*`/`Sources*` and their `.diff/Index` only — a nested `binary-amd64/Packages` declared by a flat Release is no IndexTarget (apt never requests it for a flat source) — and a `dists/` suite's are today's shapes only, so a root `Packages` under a `dists/` suite is none (parity item 2's second run). Root `Packages` groups under the pseudo-arch `all` (always required), root `Sources` under `source`. The handler's `isIndexTargetPath` (the `snapshot_index_target_404` alert) takes the suite path the same way. Task 2, Task 6.
4. **"Flat pdiff arch labels"** (spec §9's G row; the spec says no more). Read as the architecture labels of a flat suite's index and pdiff Index members: the adoption filter's (`archFromFilteredPath`: root `Sources*` and `Sources.diff/Index` are `source`, filtered when `source` is not allowlisted; root `Packages*` and `Packages.diff/Index` carry every architecture and are never filtered) and the handler alert's (`indexTargetArch`: a flat `Packages*` is `all`, a flat `Sources*` is `source`, where `archFromPath` gave `""` and the metric bucketed it as `other`). `archFromPdiffIndexPath` stays `dists/`-only: spec C6 builds no pdiff hashes for flat suites (`buildPdiffHashes` returns before it for a non-`dists/` layout). Tasks 2 and 6; flagged for the maintainer.
5. **`validateFilename`** (spec C5) allows `.` segments and `%`, and refuses, fatal as today: empty, absolute, NUL or another control byte (0x00–0x1F, 0x7F), backslash, `?`, `#`, a `..` segment, an empty segment, a length over 4,096 bytes, and — not in the spec's list — a Filename made only of `.` segments (`.`, `./.`): it names no file, and its `DotClean` is empty, which would make a `flat_filename` of `''` (the `dists/` rows' value). `ParseRelease` keeps `validateMemberPath`. R-G1's whitespace-only body is unchanged (parses to nothing). Task 3.
6. **Flat materialisation reads root `Packages*` members only** (spec C6: "the root as the Packages directory"). A flat Release may declare other files; only a member with no `/` in its path is a flat Packages index (the IndexTargets of design note 3). Rows go at `FlatBase(suite) + "/" + F` with `flat_filename = DotClean(F)`; `dists/` rows keep `flat_filename = ''`. Sources and pdiff hashing are not built for flat suites (spec C6; `buildSourceHashes` and `buildPdiffHashes` already return nothing for a non-`dists/` layout, pinned in Task 4). Task 4.
7. **The suffix fallback** (spec C6). `DeclaredHashesForPath` runs its exact query first and the fallback only when that finds no row. `P' = DotClean(P)` must be absolute, with at most 32 segments, none empty or `..`; every split `B + "/" + F` (`B` from the host root `""` to the parent of the last segment) goes into one query's `flat_filename IN (…)` (at most 32 values) with `flat_filename <> ''`, joined to the current, signed (`anchor_form <> 'unsigned'`) snapshots through `suite_freshness.current_snapshot_id`; a row is kept iff its `B` is an ancestor-or-self of its suite's `FlatBase`, and of the kept rows those with the longest `B` are returned. The row's suite path comes back with it (spec C3: a vouched `.deb` touches its suite). The query plan is pinned (`USING INDEX idx_package_hash_flat_filename`). Task 5.
8. **Dispatch 3's client-created rows** (spec C4; the E carry-forward). A flat suite's client-created rows are its `InRelease` and `Release` rows (as for every suite) and its root `Packages*`/`Sources*` rows (`aptpath.FileFlatIndex`), each with `request_count > 0`. The index rows are read only when no anchor row is client-created, from one range read of the suite's url_path rows (`cache.ListSuiteMetadataRows`, Task 9), which the anchorless refresh reuses. Task 9.
9. **The anchorless gate** (spec C4 Dispatch 3; deviation 1). A never-adopted flat suite whose probe tree finds neither `InRelease` nor `Release` runs the anchorless refresh when it has no signed evidence: the adopter's cached signed-anchor re-mark (`remarkSignedAnchors`: the `InRelease` and `Release.gpg` rows of the suite and of its `/.` twin under both schemes, sniffed outside the writer, each sniff-passing one marked) runs first, then never-downgrade's rule (`cache.CheckUnsignedDowngrade`: a marker for the base under either scheme, or an adopted, non-retired signed snapshot of the base or its twin) must find nothing. Otherwise the check is the failed check and `anchor_absent` record it is today. A read or mark error fails the check (as the unsigned commit's re-mark refuses). The gate does not read `adoption.adopt_unsigned` (spec C5 safeguard 1 refuses unsigned candidates; an anchorless suite has none) — flagged for the maintainer. Task 10.
10. **The anchorless refresh** (spec C4 Dispatch 4). Under the check's host slot and deadline, every cached url_path row of the suite whose member is `Packages*`, `Sources*` or `Contents-*` (`aptpath.SuitePath(path) == suite`) gets one conditional GET with its stored `upstream_etag`/`upstream_lastmod`: a 304 keeps it; a 200 whose body passes `aptpath.SniffAnchorlessIndex` (the flat index sniff for its extension, design note 11) replaces its blob (`cache.RefreshURLPathBlob`: blob, validators and `last_fetched_at` — only when the blob changes — and never `last_requested_at` or `request_count`); a 200 that fails it keeps the row and counts `acu_anchorless_sniff_rejected_total{host}`; 404 and 410 delete it (`cache.DeleteURLPathRows`); 401, 403, 5xx, a transport error, a body over the cap or the deadline keep it. Then the suite's cached `InRelease`, `Release` and `Release.gpg` rows whose probe answered 404 or 410 are deleted (the tree's own answers: no extra request). The check is answered — `anchorless_refreshed` when a row was replaced or deleted, `anchorless_unchanged` otherwise — when at least one index row got an answer; it is a failed check when every index request ended unknown (the streak backs off); a 304 to a request without validators is unknown, as `classify` treats it. An answered anchorless check (form `anchorless`, `upstream_status` the `Release` probe's absent answer) is a success: it clears the suite's failure or refusal record — it described an anchor that is now absent upstream — and its pending change (`inrelease_change_seen_at`: the anchorless form adopts nothing, so a stamp left behind would queue the suite every tick, `pendingChange`). Its cache operations go through the adopter's `*cache.Cache`, so the checker's `Cache` interface keeps its fakes. Task 10.
11. **The anchorless sniff** (spec C3 has none for `Contents-*`; deviation 2). `aptpath.SniffAnchorlessIndex(name, head)`: `Packages*`/`Sources*` take `SniffFlatIndex`; a compressed `Contents-*` takes its extension's magic; a plain `Contents-*` has no sniff that tells it from a soft-404, so its 200 is never stored: the row is deleted instead (counted as refreshed), and the next client request fetches it upstream through the miss path, as apt would directly. Task 10.
12. **The anchorless body cap is 64 MiB** (a constant, `maxAnchorlessIndexBytes`). The checker's fetcher reads a body into memory (`fetch.Conditional`); Release-less repositories (Gemfury, goreleaser) publish indexes of kilobytes to a few MiB. A body over the cap keeps the row (unknown) and is logged WARN once per check (`freshness: anchorless index over its size cap`). Task 10.
13. **`last_fetched_at` of a replaced row is monotonic**: `MAX(now, previous + 1)`, so a replacement within the second of the previous fetch still moves `Last-Modified` past a client's `If-Modified-Since` (C8's monotonic stamp, for url_path rows; a lead of one second is harmless to the gates that read it). Task 10.
14. **`Last-Modified` of url_path-served metadata** (spec C4) is `max(pool mtime, last_fetched_at)` at every url_path metadata serve — a hit, a miss and a stale serve — through one helper (`urlPathModTime`); a `.deb` and every snapshot serve are unchanged (C8's `adopted_at` for snapshot anchors). Task 10.
15. **R-G2's read** is one query on the pool per reaper run (`cache.ListUnadoptedFlatIndexRows`): unadopted suites without a `/dists/` substring (`instr`, not `LIKE`, which ignores ASCII case), joined to url_path on its key for the 14 root index names, the same plan shape as `ListUnadoptedSuiteAnchors` (pinned), and run only when the run holds an unadopted flat suite with no anchor row (ruling R-G5). A flat suite with at least one cached anchor row is judged by its anchors (the existing scan); one with none, by its index rows; the junk memo's key gains the member name for a flat index (its extension picks the sniff). Task 12.
16. **`suites[].flat`** (spec C7, C11; the B carry-forward) is `aptpath.IsFlatSuitePath(suite_path)`: JSON `flat` (boolean, additive), and the HTML Suite path cell shows a `flat` tag. Task 14.
17. **The strict matrix** (parity item 4) runs through the real handler with snapshots committed through the cache (`commitInlineSnapshot`, `package_hash` rows with `FlatFilename`), one subtest per row; the two pkgs.k8s.io rows use the handler's `AdoptionWaiter` seam (`fakeAdoptions`): "running" commits the third minor's snapshot during the wait, "queued" reports nothing in flight. Task 11.

## Deliberate deviations from the spec

1. **The anchorless gate reads signed evidence, not every sniff-passing anchor row** (spec C4 Dispatch 3: "flat suites that have no signed-seen marker and no sniff-passing cached anchor row"). A cached `Release` that passes its sniff — an unsigned repository that has since dropped its `Release` — would keep the suite out of the anchorless refresh for good: clients would keep getting the stale `Release` and stale indexes from the ordinary cache, and Dispatch 4's own rule ("cached anchor rows under the suite are deleted when upstream answers them 404/410") could then only ever delete junk anchors. Signed evidence is what the condition protects (a signed repository whose anchors an attacker strips must not fall back to unverified refreshes), so the gate is never-downgrade's rule plus the cached signed-anchor re-mark (design note 9): a sniff-passing `InRelease` or `Release.gpg`, a marker, or a signed snapshot keeps the suite out; an unsigned `Release` does not, and the refresh deletes it when upstream answers 404. Task 10.
2. **A plain `Contents-*` is deleted, not kept, when its 200 cannot be sniffed** (spec C4 Dispatch 4: "a 200 replaces the row's blob only if the body passes the flat index sniff for its extension (else the old row stays)"). Spec C3's flat index sniff covers `Packages*`/`Sources*` only; a plain `Contents-*` has no sniff, so the literal rule would keep its first copy forever. Deleting it on a changed 200 hands it to the miss path, which serves whatever upstream serves as any first fetch does — never worse than first contact. Design note 11; Task 10.

## Review Focus (phase G share)

1. **Upgrading a live v6 cache with flat anchors** (overview Review Focus 1): a flat `InRelease` and `Packages.gz` cached by a v6 binary have no suite row; the startup pass marks nothing (untracked); the anchor's first hit onboards the suite with its marker before the first body byte; and an untrusted flat suite then makes at most one adoption attempt per backoff step — Tasks 7, 15 (`TestServeHTTP_OnboardsAFlatSuiteFromAV6Cache`, `TestDoD_UntrustedFlatSuiteFromAV6CacheKeepsTheBackoff`).
2. **Upstreams answering 200 HTML** (overview Review Focus 3) never become a flat suite, a refreshed flat index or a kept anchorless suite: the miss sniff (no row), the anchorless sniff (old row kept, counted), and the reaper's junk evidence (R-G2) — Tasks 7, 10, 12 (`TestCheck_AnchorlessSniffRejectionKeepsTheRow`, `TestReaper_ReapsAJunkAnchorlessFlatSuite`).
3. **Every real Filename shape vouches, and nothing else does**: k8s (`amd64/…`), CUDA (`./…` and its dot-cleaned form), nvidia (`./…`, SHA512-only Release), Jenkins (`binary/`, `direct/`: parent base), Sublime (`files/`: host-root base), an epoch `%3a` — and `..`, `//`, a sibling `/flat2`, a 33-segment path and an unsigned snapshot vouch nothing — Tasks 4, 5, 15 (`TestDeclaredHashesForPath_FlatShapes`, `TestDoD_EveryRealFilenameShapeAdoptsAndVouches`).
4. **Strict mode on flat hosts** (spec C10's host-wide rule, parity item 4): the twelve rows — Task 11 (`TestServeHTTP_FlatStrictMatrix`).
5. **Twins** (ruling R-G4): two rows, two snapshots, one marker base; reaping, forgetting or refusing one never touches the other's rows — Tasks 7, 12, 13.

## File map

| Area | Files | Task |
|---|---|---|
| The flat grammar | `internal/aptpath/suite.go`, `suite_test.go`, `sniff.go` (D-4), `sniff_test.go` (D-3); `internal/proxy/proxy_test.go`; `internal/cache/suite_reap_test.go` | 1 |
| Layout-aware IndexTargets | `internal/freshness/adoption.go` (the predicates, E-9), `reconcile.go`, `adoption_test.go`, `phase6_7_skip_repair_test.go`, `reconcile_test.go` (D-27), `flat_adoption_test.go` (new) | 2 |
| Filenames and R-G1 | `internal/freshness/packages_parse.go`, `packages_parse_test.go`, `flat_adoption_test.go` | 3 |
| C6 materialisation | `internal/cache/types.go`, `adoption.go` (Step 3's insert), `reconcile.go` (`insertPackageHashTx`), `flat_package_hash_test.go` (new); `internal/freshness/adoption.go` (`buildPackageHashes`), `flat_coverage_test.go` (new) | 4 |
| C6 lookup | `internal/cache/adoption.go` (`DeclaredHashesForPath`), `flat_lookup_test.go` (new) | 5 |
| Handler: alert, host root, arch labels | `internal/handler/handler.go` (`isIndexTargetPath`, `indexTargetArch`, the host-root call), `flat_root.go` (new), `path_class_test.go`, `flat_root_test.go` (new) | 6 |
| Handler evidence | `internal/handler/handler_test.go` (`TestServeHTTP_FlatRepositoryFreshness`), `flat_handler_test.go` (new) | 7 |
| Handler deferred minors | `internal/handler/suite_rows.go` (D-32, D-31, E-67, E-68), `handler.go` (E-57, E-60, E-64, E-71's seam, D-31), `suite_rows_test.go`, `seed_t1_test.go` (E-65), `strict_wait_test.go` (E-71) | 8 |
| Dispatch 3: client-created index rows | `internal/cache/anchorless.go` (new: `ListSuiteMetadataRows`), `anchorless_test.go` (new); `internal/freshness/anchorless.go` (new: `flatIndexClientCreated`), `first_adoption.go`, `flat_rig_test.go` (new), `flat_first_adoption_test.go` (new) | 9 |
| The anchorless gate and refresh | `internal/aptpath/sniff.go` (`SniffAnchorlessIndex`), `sniff_test.go`; `internal/cache/anchorless.go` (`RefreshURLPathBlob`, `DeleteURLPathRows`), `anchorless_test.go`, `adoption.go` (R-G3 note); `internal/freshness/anchorless.go` (the gate and the refresh), `first_adoption.go`, `metrics.go`, `anchorless_test.go` (new), `retry_rig_test.go` (`rigUpstream.before`, E-47), `flat_rig_test.go` (E-47); `internal/handler/handler.go` (`urlPathModTime`, R-G3 note), `singleflight.go`, `urlpath_lastmod_test.go` (new) | 10 |
| Strict mode | `internal/cache/queries.go` (AIDEV-NOTE); `internal/handler/flat_strict_test.go` (new), `unsigned_posture_test.go` (E-70) | 11 |
| Reaper and twins | `internal/cache/flat_reap.go` (new: `ListUnadoptedFlatIndexRows`), `flat_reap_test.go` (new); `internal/freshness/reaper.go`, `reaper_test.go`, `flat_reap_test.go` (new) | 12 |
| Twins in never-downgrade | `internal/freshness/unsigned_test.go` (E-16, E-17, the twin recheck), `flat_twin_test.go` (new: E-19) | 13 |
| Admin | `internal/admin/status.go` (`flat`), `status_template.go`, `suites_status_test.go` (B-59, B-62), `json_contract_test.go` (B-59), `testdata/json_contract_golden.json` | 14 |
| DoD | `internal/freshness/dod_g_test.go` (new); `internal/handler/flat_dod_test.go` (new) | 15 |
| Documentation | `docs/configuration.md`, `docs/log-fields.md`, `docs/admin-ui-spec.md`, `packaging/config/config.toml.default`, the spec, the overview, the A–F deferred files | 16 |

No migration, no new package, no new dependency, no new config key.

## Tasks

1. The flat grammar (`aptpath`), its helpers, and the deferred minors of `suite.go` and `sniff.go`
2. Layout-aware IndexTargets on the adoption side; PR #8's adoption tests
3. `validateFilename` and ruling R-G1
4. C6 materialisation and coverage
5. C6 lookup: the suffix fallback in `DeclaredHashesForPath`
6. The handler's layout-aware alert, flat arch labels and `flat_host_root_unsupported`
7. PR #8's handler evidence: rows, T1, touches and onboarding for flat suites and their twins
8. Deferred minors in the handler's suite rows and miss path
9. Dispatch 3 for flat suites: client-created index rows
10. The anchorless gate and refresh (spec C4 Dispatch 3–4) and url_path `Last-Modified` (ruling R-G3)
11. The flat strict matrix and `current_snapshot_count` (PR #8 parity item 4)
12. The reaper and `/forget` on flat suites and their twins (rulings R-G2, R-G4, R-G5)
13. Twins in never-downgrade (the E carry-forward, E-16, E-17, E-19)
14. `suites[].flat` on the admin status page (the B carry-forward; B-59, B-62)
15. Phase G's definition of done — the nvidia flat layout, every Filename shape, the v6 backoff, minted flat suites, and the DoD map
16. Documentation, spec revision 10, overview, deferred files, final verification

---
### Task 1: The flat grammar (`aptpath`), its helpers, and the deferred minors of `suite.go` and `sniff.go`

Spec C2's flat grammar lands inside `aptpath.SuitePath`, with the helpers the other packages share (overview, "Cross-phase interfaces", G line): `IsFlatSuitePath`, `FlatBase`, `DotClean`, and `IsFlatHostRootPath` (design note 2's predicate). `IsCanonicalSuitePath` keeps its one-line definition and so covers both grammars: from this task on a flat suite row is canonical, its files map to it, and `DeleteSuiteMetadata` and the reaper's metadata deletion cover them with no code change of their own (the F carry-forward) — pinned here on a real flat suite with a real `./` twin (ruling R-G4: "reaping `/repo` leaves `/repo/.` intact", its cache half; the reaper's own is Task 12). PR #8's `TestSuitePath` flat cases are ported verbatim with our results (parity item 1: the intentional `Contents-*` difference and the added 64-hex by-hash row), and §7's C2 items: a `dists` segment is never flat, and `%2E` and `.` land on the same suite (through `proxy.Parse`, which decodes the request URI). Deferred minors: A-4 (`SuitePath`'s comment overclaimed "matching the old greedy regex": reworded, and the `/a/dists/b/dists/c` row added), A-5 (the AIDEV-NOTE said "raw path": the handler strips from `req.Path`, the string `SuitePath` received — reworded), A-6 (a space-byte row and a trailing-slash row), D-3 (a near-miss magic row per compression extension in `TestSniffFlatIndex`), D-4 (`ClassifySuiteFile`'s AIDEV-NOTE: its precondition, the `""` guard its callers rely on, the flat branch now reachable; `SuitePath`'s note points at it).

**Files:**
- Modify: `internal/aptpath/suite.go` (the whole file), `internal/aptpath/suite_test.go` (the whole file), `internal/aptpath/sniff.go` (`FileFlatIndex`'s comment, `ClassifySuiteFile`'s AIDEV-NOTE), `internal/aptpath/sniff_test.go` (`TestSniffFlatIndex`), `internal/proxy/proxy_test.go` (`TestSuitePath`, `TestParse_FlatSuites`), `internal/cache/suite_reap_test.go` (`TestDeleteSuiteMetadata_ANonCanonicalKeyDeletesNothing`, `TestDeleteSuiteMetadata_AFlatSuiteLeavesItsTwin`)

**Interfaces:**
- Consumes: `hasDistsSegment` (`sniff.go`), `cleanBytes`, `MarkerBase`; `newTestParser` (proxy tests); `openCache`, `seedBlob`, `putURLPathRow`, `urlRowExists`, `sk` (cache tests).
- Produces: `aptpath.SuitePath` with the flat grammar (same signature); `aptpath.IsFlatSuitePath(s string) bool`, `aptpath.FlatBase(suite string) string`, `aptpath.DotClean(p string) string`, `aptpath.IsFlatHostRootPath(p string) bool`; unexported `distsSuitePath`, `flatSuitePath`, `flatMemberSegments`.

- [ ] **Step 1: Write the failing tests**

Replace `internal/aptpath/suite_test.go` with:

```go
package aptpath

import (
	"strings"
	"testing"
)

func TestSuitePath_Dists(t *testing.T) {
	cases := []struct{ path, want string }{
		// unchanged behaviour
		{"/ubuntu/dists/noble/InRelease", "/ubuntu/dists/noble"},
		{"/ubuntu/dists/noble/main/binary-amd64/Packages.gz", "/ubuntu/dists/noble"},
		{"/ubuntu/dists/noble/main/binary-amd64/by-hash/SHA256/" + hex64, "/ubuntu/dists/noble"},
		{"/dists/stable/Release", "/dists/stable"},
		{"/debian/dists/bookworm-updates/InRelease", "/debian/dists/bookworm-updates"},
		{"/some/deep/repo/dists/sid/InRelease", "/some/deep/repo/dists/sid"},
		{"/a/dists/b/dists/c/InRelease", "/a/dists/b/dists/c"},
		// deferred minor A-4: the last "dists" followed by a codename AND a
		// member wins; "/a/dists/b/dists/c" has no member after "c".
		{"/a/dists/b/dists/c", "/a/dists/b"},
		{"/dépôt/dists/x/InRelease", "/dépôt/dists/x"},     // UTF-8 is not a forbidden byte
		{"/my repo/dists/x/InRelease", "/my repo/dists/x"}, // deferred minor A-6: a space is not either
		{"/ubuntu/pool/main/h/hello/hello.deb", ""},
		{"/", ""},
		{"", ""},
		{"relative/dists/noble/InRelease", ""},
		// tightened (spec C2): a suite must have a member
		{"/ubuntu/dists/noble", ""},
		{"/dists/stable", ""},
		{"/foo/dists/InRelease", ""},
		// tightened: non-canonical segments
		{"/ubuntu//dists/noble/InRelease", ""},
		{"/ubuntu/./dists/noble/InRelease", ""},
		{"/ubuntu/pool/../dists/noble/InRelease", ""},
		{"/ubuntu/dists/noble/main/../InRelease", ""},
		{"/ubuntu/dists/noble//InRelease", ""},
		{"/ubuntu/dists/./InRelease", ""},
		{"/ubuntu/dists/noble/InRelease/", ""}, // deferred minor A-6: a trailing slash is an empty segment
		// tightened: forbidden decoded bytes (S-3)
		{"/ubuntu#x/dists/noble/InRelease", ""},
		{"/ubuntu/dists/noble?x/InRelease", ""},
		{"/ubuntu/dists/noble/In%52elease", ""},
		{"/ubuntu\\x/dists/noble/InRelease", ""},
		{"/ubuntu/dists/noble/In\x00Release", ""},
		{"/ubuntu/dists/noble/In\x7fRelease", ""},
	}
	for _, c := range cases {
		if got := SuitePath(c.path); got != c.want {
			t.Errorf("SuitePath(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

// Spec C2's flat grammar. The first block is PR #8's TestSuitePath flat
// cases, verbatim (plan overview, "PR #8 parity" item 1), with our results:
// the one intentional difference is Contents-* (C2 gives it a suite), and
// a valid 64-hex by-hash name is added.
func TestSuitePath_Flat(t *testing.T) {
	const k8s = "/core:/stable:/v1.34/deb"
	cases := []struct{ path, want string }{
		// PR #8, verbatim
		{k8s + "/InRelease", k8s},
		{k8s + "/Release", k8s},
		{k8s + "/Release.gpg", k8s},
		{k8s + "/Packages", k8s},
		{k8s + "/Packages.gz", k8s},
		{k8s + "/Packages.zst", k8s},
		{"/repo/flat/Sources.xz", "/repo/flat"},
		{k8s + "/amd64/kubelet_1.34.1-1.1_amd64.deb", ""},
		{"/repo/flat/Packages_1.0_all.deb", ""},
		{"/repo/flat/Translation-en", ""},
		{"/repo/flat/Contents-amd64.gz", "/repo/flat"}, // PR #8: "" — spec C2 gives Contents-* a suite
		{"/repo/flat/by-hash/SHA256/abc", ""},
		{"/InRelease", ""},
		{"/Packages.gz", ""},
		{"InRelease", ""}, // not absolute
		{k8s + "/./InRelease", k8s + "/."},
		{k8s + "/./Packages.gz", k8s + "/."},
		{"/repo//InRelease", ""},
		{"/repo/./sub/InRelease", ""},
		{"/evil/../pkgs/InRelease", ""},
		{"/repo/%2E/InRelease", ""}, // a decoded %
		{"/./InRelease", ""},
		{"/ubuntu/dists//noble/InRelease", ""},
		// added: a valid by-hash name
		{"/repo/flat/by-hash/SHA256/" + hex64, "/repo/flat"},
		// the rest of spec C2's member grammar
		{"/repo/flat/by-hash/SHA512/" + hex64 + hex64, "/repo/flat"},
		{"/repo/flat/by-hash/SHA512/" + hex64, ""}, // a SHA512 name is 128 hex digits
		{"/repo/flat/by-hash/SHA256/" + strings.ToUpper(hex64), ""},
		{"/repo/flat/by-hash/MD5Sum/0123456789abcdef0123456789abcdef", ""},
		{"/repo/flat/Packages.diff/Index", "/repo/flat"},
		{"/repo/flat/Sources.diff/Index", "/repo/flat"},
		{"/repo/flat/Packages.diff/2026-10-20-0000.00.gz", ""}, // a pdiff patch: Phase 1 only
		{"/repo/flat/Contents-amd64.diff/Index", ""},
		{"/repo/flat/Contents-udeb-arm64", "/repo/flat"},
		{"/repo/flat/Sources.bz2", "/repo/flat"},
		{"/repo/flat/Packages.lzma", "/repo/flat"},
		{"/repo/flat/Packages.lz4", "/repo/flat"},
		{"/repo/flat/Packages.Z", ""},
		{"/repo/flat/en", ""}, // a flat Translation file (apt's $(LANGUAGE)): Phase 1 only
		{"/repo/flat/Contents-AMD64.gz", ""},
		{"/repo/InRelease/", ""},
		{"/repo/./InRelease/", ""},
		{"/repo/./.", ""},
		{"/repo/./x/./InRelease", ""},
		{"/repo/././InRelease", ""}, // exactly one trailing "/."
		{"/repo/../InRelease", ""},
		{"/repo/x\\y/InRelease", ""},
		{"/repo/x#y/InRelease", ""},
		{"/r/InRelease", "/r"},
		{"/./Packages.gz", ""},
		{"/by-hash/SHA256/" + hex64, ""},
		// a dists segment anywhere: never flat (spec C2)
		{"/repo/dists/InRelease", ""},
		{"/dists/Packages.gz", ""},
		{"/repo/dists/x/y/Packages.gz", "/repo/dists/x"},
		{"/dists/x/Packages.gz", "/dists/x"},
		{"/repo/distsy/Packages.gz", "/repo/distsy"},
		{"/repo/dists.d/InRelease", "/repo/dists.d"},
	}
	for _, c := range cases {
		if got := SuitePath(c.path); got != c.want {
			t.Errorf("SuitePath(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestIsCanonicalSuitePath(t *testing.T) {
	cases := []struct {
		suite string
		want  bool
	}{
		{"/ubuntu/dists/noble", true},
		{"/dists/stable", true},
		// "InRelease" is a legal codename under the spec C2 grammar
		// (SuitePath("/foo/dists/InRelease/x") = "/foo/dists/InRelease").
		// apt never requests such a suite, so phase F reaps any legacy row
		// by idleness, not as non-canonical.
		{"/foo/dists/InRelease", true},
		{"/ubuntu/dists/noble/", false},
		{"/ubuntu//dists/noble", false},
		{"/ubuntu/./dists/noble", false},
		{"/ubuntu#x/dists/noble", false},
		{"/foo/dists", false},               // no codename
		{"/ubuntu/dists/noble/main", false}, // deeper than a suite
		{"", false},
		// flat suites (phase G)
		{"/repo", true},
		{"/repo/.", true},
		{"/core:/stable:/v1.34/deb", true},
		{"/repo/./.", false},
		{"/repo/./x", false},
		{"/repo//x", false},
		{"/repo/", false},
		{"/", false},
		{"/.", false},
		{"/repo/%2E", false},
		{"/repo/dists", false},
	}
	for _, c := range cases {
		if got := IsCanonicalSuitePath(c.suite); got != c.want {
			t.Errorf("IsCanonicalSuitePath(%q) = %v, want %v", c.suite, got, c.want)
		}
	}
}

// IsFlatSuitePath: a canonical suite path of the flat grammar.
func TestIsFlatSuitePath(t *testing.T) {
	for _, c := range []struct {
		suite string
		want  bool
	}{
		{"/repo", true},
		{"/repo/.", true},
		{"/core:/stable:/v1.34/deb", true},
		{"/debian-stable/binary", true},
		{"/repo/distsy", true},
		{"/ubuntu/dists/noble", false},
		{"/dists/stable", false},
		{"/repo/dists", false},
		{"/repo/./.", false},
		{"/repo//x", false},
		{"/", false},
		{"/.", false},
		{"", false},
	} {
		if got := IsFlatSuitePath(c.suite); got != c.want {
			t.Errorf("IsFlatSuitePath(%q) = %v, want %v", c.suite, got, c.want)
		}
	}
}

// Spec C6: a flat suite's package base, shared by the twins.
func TestFlatBase(t *testing.T) {
	for in, want := range map[string]string{
		"/repo":                    "/repo",
		"/repo/.":                  "/repo",
		"/core:/stable:/v1.34/deb": "/core:/stable:/v1.34/deb",
		"/repo/./.":                "/repo/.", // only one trailing "/." is removed
	} {
		if got := FlatBase(in); got != want {
			t.Errorf("FlatBase(%q) = %q, want %q", in, got, want)
		}
		if got := MarkerBase(in); got != FlatBase(in) {
			t.Errorf("MarkerBase(%q) = %q, FlatBase %q: the two must agree", in, got, FlatBase(in))
		}
	}
}

// Spec C6: "." segments removed, nothing else.
func TestDotClean(t *testing.T) {
	for in, want := range map[string]string{
		"./cuda_12.6_amd64.deb": "cuda_12.6_amd64.deb",
		"amd64/kubectl.deb":     "amd64/kubectl.deb",
		"/flat/./amd64/x.deb":   "/flat/amd64/x.deb",
		"/flat/././x.deb":       "/flat/x.deb",
		"a/./b/./c.deb":         "a/b/c.deb",
		"/flat/../ubuntu/x.deb": "/flat/../ubuntu/x.deb",
		"/flat//x.deb":          "/flat//x.deb",
		"pkg_1%3a2.deb":         "pkg_1%3a2.deb",
		"./.hidden.deb":         ".hidden.deb",
		"..deb":                 "..deb",
		"x.deb/.":               "x.deb",
		".":                     "",
		"":                      "",
	} {
		if got := DotClean(in); got != want {
			t.Errorf("DotClean(%q) = %q, want %q", in, got, want)
		}
	}
}

// Spec C2: a flat member at the host root (`deb https://h/ /` and
// `deb https://h ./`) gets no suite; the handler logs it.
func TestIsFlatHostRootPath(t *testing.T) {
	for _, c := range []struct {
		path string
		want bool
	}{
		{"/InRelease", true},
		{"/Release.gpg", true},
		{"/Packages.gz", true},
		{"/./InRelease", true},
		{"/./Packages.xz", true},
		{"/by-hash/SHA256/" + hex64, true},
		{"/Packages.diff/Index", true},
		{"/repo/InRelease", false},
		{"/repo/./InRelease", false},
		{"/././InRelease", false},
		{"/pool/x.deb", false},
		{"/Translation-en", false},
		{"/dists/InRelease", false},
		{"InRelease", false},
		{"/In%52elease", false},
		{"", false},
	} {
		if got := IsFlatHostRootPath(c.path); got != c.want {
			t.Errorf("IsFlatHostRootPath(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

const hex64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
```

In `internal/aptpath/sniff_test.go`, `TestSniffFlatIndex`, replace

```go
		{"Packages.Z", "\x1f\x9d", false},
```

with

```go
		// Deferred minor D-3: a near miss per extension (the magic with its
		// last byte wrong), so a shortened magic fails here.
		{"Packages.gz", "\x1f\x8c\x08\x00", false},
		{"Packages.xz", "\xfd7zXZ\x01", false},
		{"Packages.bz2", "BZi91AY&SY", false},
		{"Packages.lzma", "\x5d\x00\x01\x80\x00", false},
		{"Packages.lz4", "\x04\x22\x4d\x19\x64", false},
		{"Sources.zst", "\x28\xb5\x2f\xfe\x04", false},
		{"Packages.Z", "\x1f\x9d", false},
```

In `internal/proxy/proxy_test.go`, `TestSuitePath`, replace

```go
		// nested repo paths
		{"/some/deep/repo/dists/sid/InRelease", "/some/deep/repo/dists/sid"},
	}
```

with

```go
		// nested repo paths
		{"/some/deep/repo/dists/sid/InRelease", "/some/deep/repo/dists/sid"},
		// flat repositories (spec C2; aptpath.TestSuitePath_Flat owns the table)
		{"/core:/stable:/v1.34/deb/InRelease", "/core:/stable:/v1.34/deb"},
		{"/core:/stable:/v1.34/deb/./Packages.gz", "/core:/stable:/v1.34/deb/."},
		{"/core:/stable:/v1.34/deb/amd64/kubelet_1.34.1-1.1_amd64.deb", ""},
		{"/InRelease", ""},
	}
```

and add before `func TestStripPort(t *testing.T) {`:

```go
// Spec C2 and §7: Parse decodes the request URI before the grammar runs,
// so "%2E" and "." land on the same suite (the `deb <uri> ./` twin), and
// the pkgs.k8s.io shape maps to its literal directory (PR #8's evidence).
// A flat .deb and a host-root anchor get no suite.
func TestParse_FlatSuites(t *testing.T) {
	p := newTestParser(t)
	for _, c := range []struct {
		uri, path, suite string
	}{
		{"https://pkgs.k8s.io/core:/stable:/v1.34/deb/InRelease", "/core:/stable:/v1.34/deb/InRelease", "/core:/stable:/v1.34/deb"},
		{"http://h.example/repo/InRelease", "/repo/InRelease", "/repo"},
		{"http://h.example/repo/./InRelease", "/repo/./InRelease", "/repo/."},
		{"http://h.example/repo/%2E/InRelease", "/repo/./InRelease", "/repo/."},
		{"http://h.example/repo/%2e/Packages.gz", "/repo/./Packages.gz", "/repo/."},
		{"http://h.example/repo/amd64/x_1_amd64.deb", "/repo/amd64/x_1_amd64.deb", ""},
		{"http://h.example/InRelease", "/InRelease", ""},
		{"http://h.example/repo/%252E/InRelease", "/repo/%2E/InRelease", ""}, // a decoded '%': no suite
	} {
		r, err := p.Parse(c.uri, "")
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.uri, err)
		}
		if r.Path != c.path || r.SuitePath != c.suite {
			t.Errorf("Parse(%q) = path %q, suite %q; want %q, %q", c.uri, r.Path, r.SuitePath, c.path, c.suite)
		}
	}
}
```

In `internal/cache/suite_reap_test.go`, replace `TestDeleteSuiteMetadata_ANonCanonicalKeyDeletesNothing`'s doc comment and its first eight body lines — from `// Design note 7 (the Task 3 review's Important 1): a key no suite path` through `for _, suite := range []string{"", "/a/dists/b/c", "/flat/."} {` — with (the rest of the function is unchanged):

```go
// Design note 7 (the Task 3 review's Important 1): a key no suite path
// matches — the empty path, a non-canonical one (a dists suite with a
// member segment, a flat directory with two trailing "/.", which phase
// G's grammar never produces) — has no metadata rows. DeleteSuiteMetadata
// deletes nothing for it and leaves the host's pool .deb, its flat-repo
// files, a canonical suite's anchor and a row under the key's own prefix
// in place (a non-canonical row's cached files are url_path GC's:
// metadata_rows 0). Before the fix the empty path's range spanned the
// host and deleted the .deb and the flat-repo files, whose suite path was
// "" too before phase G. The key "/flat/." is canonical since phase G (a
// `deb <uri> ./` twin): TestDeleteSuiteMetadata_AFlatSuiteLeavesItsTwin
// deletes through it.
func TestDeleteSuiteMetadata_ANonCanonicalKeyDeletesNothing(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	blob := seedBlob(t, c, "cached bytes")
	paths := []string{"/debian/pool/main/f/foo/foo_1_amd64.deb", "/flat/Packages", "/debian/dists/x/InRelease",
		"/a/dists/b/c/InRelease", "/flat/./Packages", "/flat/././Packages"}
	for _, p := range paths {
		putURLPathRow(t, c, "http", "h", p, blob, sql.NullInt64{Int64: 1, Valid: true}, false)
	}
	for _, suite := range []string{"", "/a/dists/b/c", "/flat/./."} {
```

and add after that function:

```go
// Spec §9's G row, "reaping /repo leaves /repo/. intact" (the F
// carry-forward): with phase G's flat grammar the files of a real flat
// suite map to it, so DeleteSuiteMetadata deletes them — its anchors, root
// indexes, Contents, by-hash name and pdiff Index — and nothing of its
// `deb <uri> ./` twin, of a nested flat suite, of a prefix sibling, or its
// .debs (no suite's metadata). The twin's own call deletes only the twin's.
func TestDeleteSuiteMetadata_AFlatSuiteLeavesItsTwin(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	blob := seedBlob(t, c, "flat metadata")
	hex := strings.Repeat("ab", 32)
	flat := []string{"/flat/InRelease", "/flat/Release", "/flat/Release.gpg", "/flat/Packages", "/flat/Packages.gz",
		"/flat/Sources.xz", "/flat/Contents-amd64.gz", "/flat/by-hash/SHA256/" + hex, "/flat/Packages.diff/Index"}
	twin := []string{"/flat/./InRelease", "/flat/./Packages.gz", "/flat/./by-hash/SHA256/" + hex}
	others := []string{
		"/flat/amd64/kubectl_1.34_amd64.deb", // a package: no suite's metadata
		"/flat/./cuda_12_amd64.deb",
		"/flat/sub/InRelease", // the nested flat suite /flat/sub
		"/flat2/InRelease",    // a sibling sharing the prefix
		// no suite (Phase 1 only)
		"/flat/Translation-en", "/flat/en", "/flat/Packages.diff/2026-10-20-0000.00.gz",
	}
	for _, p := range append(append(append([]string{}, flat...), twin...), others...) {
		putURLPathRow(t, c, "http", "h", p, blob, sql.NullInt64{Int64: 1, Valid: true}, true)
	}
	if n, err := c.DeleteSuiteMetadata(ctx, sk("h", "/flat")); err != nil || n != len(flat) {
		t.Fatalf("DeleteSuiteMetadata(/flat) = %d, %v; want the suite's %d rows", n, err, len(flat))
	}
	for _, p := range flat {
		if urlRowExists(t, c, "http", "h", p) {
			t.Errorf("%s was not deleted", p)
		}
	}
	for _, p := range append(append([]string{}, twin...), others...) {
		if !urlRowExists(t, c, "http", "h", p) {
			t.Errorf("%s was deleted by /flat's metadata deletion, want it kept", p)
		}
	}
	if n, err := c.DeleteSuiteMetadata(ctx, sk("h", "/flat/.")); err != nil || n != len(twin) {
		t.Fatalf("DeleteSuiteMetadata(/flat/.) = %d, %v; want the twin's %d rows", n, err, len(twin))
	}
	for _, p := range others {
		if !urlRowExists(t, c, "http", "h", p) {
			t.Errorf("%s was deleted by the twin's metadata deletion, want it kept", p)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -gcflags=-e ./internal/aptpath/`
Expected: FAIL — the build lists `undefined: IsFlatSuitePath`, `undefined: FlatBase` (three times), `undefined: DotClean` and `undefined: IsFlatHostRootPath`.

Run: `go test ./internal/proxy/ ./internal/cache/ -run 'TestSuitePath|TestParse_FlatSuites|TestDeleteSuiteMetadata'`
Expected: FAIL — `TestSuitePath` (`proxy_test.go: SuitePath("/core:/stable:/v1.34/deb/InRelease") = "", want "/core:/stable:/v1.34/deb"`), `TestParse_FlatSuites` (`Parse("https://pkgs.k8s.io/core:/stable:/v1.34/deb/InRelease") = path "/core:/stable:/v1.34/deb/InRelease", suite ""; want …`, and the same for every flat row) and `TestDeleteSuiteMetadata_AFlatSuiteLeavesItsTwin` (`DeleteSuiteMetadata(/flat) = 0, <nil>; want the suite's 9 rows`). `TestDeleteSuiteMetadata_ANonCanonicalKeyDeletesNothing` passes (it pins behaviour the old grammar has too).

- [ ] **Step 3: Implement**

Replace `internal/aptpath/suite.go` with:

```go
// Package aptpath holds the path grammar shared by the proxy, cache,
// freshness and handler packages: suite identity, canonicality, and the
// apt-compatible upstream URL encoding. It depends only on the standard
// library so the storage layer can use it without importing internal/proxy.
package aptpath

import (
	"regexp"
	"strings"
)

// ForbiddenPathBytes are decoded bytes that disqualify a path from suite
// identity. A decoded '#' or '?' means the client sent %23/%3F, '%' means
// it sent %25, and '\' is never part of an apt path (spec C2, S-3).
const ForbiddenPathBytes = "#?%\\"

// SuitePath returns the canonical suite path for decoded request path p,
// or "" when p belongs to no suite (spec C2). Two grammars:
//
// dists/ grammar: /<repo>/dists/<codename>/<member...> with a non-empty
// member; the suite is /<repo>/dists/<codename>. The LAST "dists" segment
// that is followed by a codename and at least one member segment wins
// ("/a/dists/b/dists/c/InRelease" is the suite "/a/dists/b/dists/c").
//
// Flat grammar: a path with no "dists" segment whose tail is one flat
// member (flatMemberSegments: an anchor, a root Packages*/Sources*/Contents-*
// index, a by-hash name or a pdiff Index) belongs to the LITERAL directory
// D before that member. D may end in exactly one "/." (the `deb <uri> ./`
// form: "/repo/./InRelease" is the suite "/repo/."), and is never the
// host root ("/", "/.").
//
// AIDEV-NOTE: canonicality is strict on purpose. Every segment must be
// non-empty and not "." or ".." — the flat grammar's one exception is a
// single final "." segment of D — and ForbiddenPathBytes / control bytes
// are refused. A path that fails gets NO suite (Phase 1 url_path serving
// continues); it never gets a "cleaned" suite, because the handler maps a
// request onto its snapshot by stripping suite+"/" from req.Path, the
// same string SuitePath received (deferred minor A-5): "/repo/./x" must
// stay under "/repo/.", never "/repo". A "dists" segment anywhere makes a
// path dists-only: such a path is never flat (spec C2). ClassifySuiteFile
// (sniff.go) relies on this function as its precondition; widen or narrow
// the grammars together with it and with IsFlatSuitePath.
func SuitePath(p string) string {
	if !strings.HasPrefix(p, "/") || !cleanBytes(p) {
		return ""
	}
	segs := strings.Split(p[1:], "/")
	if hasDistsSegment(p) {
		return distsSuitePath(segs)
	}
	return flatSuitePath(segs)
}

// distsSuitePath is the dists/ grammar over p's segments.
func distsSuitePath(segs []string) string {
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return ""
		}
	}
	for i := len(segs) - 3; i >= 0; i-- {
		if segs[i] == "dists" {
			return "/" + strings.Join(segs[:i+2], "/")
		}
	}
	return ""
}

// flatSuitePath is the flat grammar over the segments of a path with no
// "dists" segment: the directory before the path's flat member, or "".
func flatSuitePath(segs []string) string {
	n := flatMemberSegments(segs)
	if n == 0 || n >= len(segs) {
		return "" // no flat member, or the member alone: the host root
	}
	dir := segs[:len(segs)-n]
	twin := dir[len(dir)-1] == "."
	if twin {
		dir = dir[:len(dir)-1]
	}
	if len(dir) == 0 {
		return "" // "/./<member>": the host root again
	}
	for _, s := range dir {
		if s == "" || s == "." || s == ".." {
			return ""
		}
	}
	d := "/" + strings.Join(dir, "/")
	if twin {
		d += "/."
	}
	return d
}

// Spec C2's flat member grammar, one regexp per member shape.
var (
	flatMemberRE   = regexp.MustCompile(`^(?:InRelease|Release|Release\.gpg|(?:Packages|Sources)(?:\.(?:gz|xz|bz2|lzma|lz4|zst))?|Contents-[a-z0-9-]+(?:\.(?:gz|xz|bz2|lzma|lz4|zst))?)$`)
	flatPdiffDirRE = regexp.MustCompile(`^(?:Packages|Sources)\.diff$`)
	hex64RE        = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hex128RE       = regexp.MustCompile(`^[0-9a-f]{128}$`)
)

// flatMemberSegments returns how many trailing segments of segs form a
// spec C2 flat member — 1 (an anchor or root index), 2 (a pdiff Index) or
// 3 (a by-hash name) — or 0 when the tail is none of them.
func flatMemberSegments(segs []string) int {
	n := len(segs)
	switch {
	case n >= 1 && flatMemberRE.MatchString(segs[n-1]):
		return 1
	case n >= 2 && segs[n-1] == "Index" && flatPdiffDirRE.MatchString(segs[n-2]):
		return 2
	case n >= 3 && segs[n-3] == "by-hash" &&
		((segs[n-2] == "SHA256" && hex64RE.MatchString(segs[n-1])) ||
			(segs[n-2] == "SHA512" && hex128RE.MatchString(segs[n-1]))):
		return 3
	}
	return 0
}

// IsFlatHostRootPath reports whether p is a flat member at the host root
// ("/InRelease", "/./Packages.gz"): spec C2 gives such a path no suite —
// a host-root flat repository (`deb https://h/ /`) is unsupported, and
// the handler logs flat_host_root_unsupported for it.
func IsFlatHostRootPath(p string) bool {
	if !strings.HasPrefix(p, "/") || !cleanBytes(p) || hasDistsSegment(p) {
		return false
	}
	segs := strings.Split(p[1:], "/")
	n := flatMemberSegments(segs)
	return n > 0 && (n == len(segs) || (n == len(segs)-1 && segs[0] == "."))
}

// IsCanonicalSuitePath reports whether s is a suite path SuitePath could
// have produced, in either grammar ("/<repo>/dists/<codename>", a flat
// directory, or its "/." twin). Used to recognise legacy suite rows that
// the grammar can never reach again (phase F reaps them). A codename equal
// to an anchor name ("/foo/dists/InRelease") is canonical: the grammar
// allows it, and idle reaping handles such rows.
func IsCanonicalSuitePath(s string) bool {
	return s != "" && SuitePath(s+"/InRelease") == s
}

// IsFlatSuitePath reports whether s is a canonical flat suite path: one
// SuitePath could have produced from the flat grammar ("/repo",
// "/core:/stable:/v1.34/deb", the twin "/repo/."). A dists suite, a
// non-canonical path and "" are not.
func IsFlatSuitePath(s string) bool {
	return IsCanonicalSuitePath(s) && !hasDistsSegment(s)
}

// FlatBase returns the directory a flat suite's Packages Filenames are
// relative to for spec C6: the suite path with ONE trailing "/." removed,
// so the twins "/repo" and "/repo/." share it (apt requests
// <uri>/<Filename> for both `deb <uri>/ /` and `deb <uri> ./`). It equals
// MarkerBase for every suite; the two names keep their two meanings
// apart (C6's package base, C3's marker key).
func FlatBase(suite string) string { return strings.TrimSuffix(suite, "/.") }

// DotClean returns p with every "." segment removed ("./cuda.deb" →
// "cuda.deb", "/flat/./amd64/x.deb" → "/flat/amd64/x.deb"); nothing else
// changes — a ".." or an empty segment stays for the caller to refuse
// (spec C6: package_hash.flat_filename and the suffix fallback).
func DotClean(p string) string {
	segs := strings.Split(p, "/")
	out := segs[:0]
	for _, s := range segs {
		if s != "." {
			out = append(out, s)
		}
	}
	return strings.Join(out, "/")
}

func cleanBytes(p string) bool {
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c < 0x20 || c == 0x7f || strings.IndexByte(ForbiddenPathBytes, c) >= 0 {
			return false
		}
	}
	return true
}
```

In `internal/aptpath/sniff.go`, replace

```go
	// FileFlatIndex is a flat suite's root Packages*/Sources* file. SuitePath
	// gives no flat suite before phase G, so the handler meets it only once
	// G adds the flat grammar there.
	FileFlatIndex
```

with

```go
	// FileFlatIndex is a flat suite's root Packages*/Sources* file (spec
	// C2's flat grammar gives it its directory's suite).
	FileFlatIndex
```

and replace `ClassifySuiteFile`'s doc comment with (deferred minor D-4):

```go
// ClassifySuiteFile reports what path is to suite, which must be
// SuitePath(path) ("" gives NotSuiteFile). A dists suite's root
// Packages*/Sources* is not a flat index, and a suite path longer than
// MaxSuitePathBytes gives NotSuiteFile: its files are served untracked.
//
// AIDEV-NOTE: deferred minor D-4. The precondition is suite ==
// SuitePath(path): the classifier trusts the grammar (suite.go) for
// canonicality and only names the file under it, so a caller passing any
// other suite gets a meaningless answer. Its callers rely on the "" guard:
// a path without a suite is never a suite file (no row, marker or touch).
// The MaxSuitePathBytes guard is ruling PD-9's one gate for every
// per-suite key. The flat branch (FileFlatIndex) is spec C3's routing
// point for flat indexes (ruling R-D3), reachable from requests since
// phase G's flat grammar; hasDistsSegment keeps a dists suite's root
// Packages*/Sources* out of it, as the grammar keeps such a path from
// ever being flat.
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/aptpath/ ./internal/proxy/ ./internal/cache/`
Expected: PASS.

Run: `go test ./...`
Expected: PASS. The flat grammar changes no other existing test: the handler, freshness and admin suites already route a flat path's files through `SuitePath` and `ClassifySuiteFile` (rulings R-D3, design note 1), and the one cache test that used `/flat/.` as a non-canonical key was changed in Step 1.

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| `flatSuitePath` drops the twin's `/.` (`if twin { d += "/." }` removed) | `TestSuitePath_Flat` (`SuitePath("/core:/stable:/v1.34/deb/./InRelease") = "/core:/stable:/v1.34/deb", want "/core:/stable:/v1.34/deb/."`), `TestIsCanonicalSuitePath`, `TestIsFlatSuitePath`, `TestSuitePath` and `TestParse_FlatSuites` (proxy), `TestDeleteSuiteMetadata_AFlatSuiteLeavesItsTwin` (`DeleteSuiteMetadata(/flat) = 12, <nil>; want the suite's 9 rows`) |
| `flatSuitePath` strips every trailing `.` segment (a loop for the one `if`) | `TestSuitePath_Flat` (`SuitePath("/repo/././InRelease") = "/repo/.", want ""`) |
| `SuitePath` tries the `dists/` grammar and falls back to the flat one (no `hasDistsSegment` gate) | `TestSuitePath_Dists` (`SuitePath("/foo/dists/InRelease") = "/foo/dists", want ""`), `TestSuitePath_Flat` (`SuitePath("/repo/dists/InRelease") = "/repo/dists", want ""`), `TestIsCanonicalSuitePath` (`IsCanonicalSuitePath("/foo/dists") = true, want false`) |
| `flatSuitePath` returns `"/"` for an empty directory | `TestSuitePath_Flat` (`SuitePath("/./InRelease") = "/", want ""`) |
| `hex64RE` accepts any hex length | `TestSuitePath_Flat` (`SuitePath("/repo/flat/by-hash/SHA256/abc") = "/repo/flat", want ""`) |
| `flatMemberRE` without its `Contents-*` alternative | `TestSuitePath_Flat` (`SuitePath("/repo/flat/Contents-amd64.gz") = "", want "/repo/flat"`), `TestDeleteSuiteMetadata_AFlatSuiteLeavesItsTwin` (`= 8, <nil>; want the suite's 9 rows`) |
| `flatMemberSegments` without its pdiff Index case | `TestSuitePath_Flat` (`SuitePath("/repo/flat/Packages.diff/Index") = "", want "/repo/flat"`), `TestIsFlatHostRootPath`, `TestDeleteSuiteMetadata_AFlatSuiteLeavesItsTwin` |
| a SHA512 by-hash name matched by `hex64RE` | `TestSuitePath_Flat` (`SuitePath("/repo/flat/by-hash/SHA512/0123…") = "", want "/repo/flat"`) |
| `DotClean` drops `..` segments instead of `.` ones | `TestDotClean` (`DotClean(".") = ".", want ""`) |
| `IsFlatSuitePath` without `!hasDistsSegment(s)` | `TestIsFlatSuitePath` (`IsFlatSuitePath("/ubuntu/dists/noble") = true, want false`) |
| `IsFlatHostRootPath` accepts any leading `.` segment (`segs[0] == "."` alone) | `TestIsFlatHostRootPath` (`IsFlatHostRootPath("/././InRelease") = true, want false`) |
| `FlatBase` strips every trailing `/.` | `TestFlatBase` (`FlatBase("/repo/./.") = "/repo", want "/repo/."`) |
| the `.zst` magic shortened to three bytes (deferred minor D-3) | `TestSniffFlatIndex` (`SniffFlatIndex("Sources.zst", "(\xb5/\xfe\x04") = true, want false`) |
| the `.lzma` magic shortened to two bytes | `TestSniffFlatIndex` (`SniffFlatIndex("Packages.lzma", "]\x00\x01\x80\x00") = true, want false`) |
| the `.bz2` magic shortened to two bytes | `TestSniffFlatIndex` (`SniffFlatIndex("Packages.bz2", "BZi91AY&SY") = true, want false`) |
| the `.lz4` magic shortened to three bytes | `TestSniffFlatIndex` (`SniffFlatIndex("Packages.lz4", "\x04\"M\x19d") = true, want false`) |

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/aptpath/ internal/proxy/proxy_test.go internal/cache/suite_reap_test.go
git add internal/aptpath/ internal/proxy/proxy_test.go internal/cache/suite_reap_test.go
git commit -m "Give flat repositories a suite: spec C2's flat grammar; deferred minors A-4, A-5, A-6, D-3, D-4"
```

(Record the mutation table in the commit message.)

### Task 2: Layout-aware IndexTargets on the adoption side; PR #8's adoption tests

Spec C5 "Flat IndexTargets": root-level `Packages*`/`Sources*` (and their `.diff/Index`) are IndexTargets, fatal on fetch failure, only when the suite is flat; `isIndexTarget`, `indexTargetGroup` and `archFromFilteredPath` take the suite path; root `Packages` groups under the pseudo-arch `all` (always required), root `Sources` under `source` (design note 3). `missingRequestableIndexGroups` and reconcile's `missingRequestableMembers` and `reconcilePending` take the suite path too, so the serve-contract guard and the reconcile heal see a flat suite's groups. The flat pdiff arch labels of the adoption filter land here (design note 4). PR #8's tables are ported with every row run under a flat suite, its `/.` twin and a `dists/` suite, where none of the root paths is an IndexTarget (parity item 2), and its two adoption tests with our results (parity item 3): a flat `Packages` 404 is skipped and then refused by the serve-contract guard (`ErrAdoptionGuardFailed`, which wraps PR #8's `ErrAdoptionMemberFetchFailed`), a 503 is the fatal IndexTarget failure itself. Deferred minors: E-9 (G: `adoption_member_skipped` names a SHA512-only member's `declared_sha512`, at all three skip sites), D-27 (`TestReconcileSnapshot_RejectsCorruptedAnchor` asserts the hash check refused it). This task changes `adoption.go`, `reconcile.go` and `freshness.go` (`planRepair`'s one call) by hand; their other open items are checked in Task 16 (E-31 and E-45 stay H's by ruling PF-11).

**Files:**
- Create: `internal/freshness/flat_adoption_test.go`
- Modify: `internal/freshness/adoption.go` (`flatArchFilterSourceRE`, `archFromFilteredPath`, `flatIndexTargetRE`, `isIndexTarget`, `flatIndexTargetGroupRE`, `indexTargetGroup`, `missingRequestableIndexGroups`, their callers in `runShared` and `adoptMember`, the three `adoption_member_skipped` lines), `internal/freshness/reconcile.go` (`reconcileSnapshot`, `reconcilePending`, `missingRequestableMembers`), `internal/freshness/freshness.go` (`planRepair`'s call), `internal/freshness/adoption_test.go`, `internal/freshness/phase6_7_skip_repair_test.go`, `internal/freshness/reconcile_test.go`

**Interfaces:**
- Consumes: `aptpath.IsFlatSuitePath` (Task 1); `newAdoptionTestEnv`, `newTolerantAdopter`, `newReconcileAdopter`, `trackSuite`, `fakePackagesStanzas`, `gzipBytes`, `makeRelease`, `hashRelease`, `sha512Of`, `snapshotPaths`, `currentID`, `lockedBuffer`, `testHex64`, `(*fakeFetcher).fail404`/`fail503`/`put`/`requested`, `simtest.ClearsignedFixture`, `(*cache.Cache).DeleteSnapshotMembersForTest`.
- Produces: `isIndexTarget(suitePath, p string) bool`, `indexTargetGroup(suitePath, p string) (group, arch string, ok bool)`, `archFromFilteredPath(suitePath, p string) (string, bool)`, `missingRequestableIndexGroups(suitePath string, declared, fetched []ReleaseMember, allowlist map[string]struct{}) []string`, `missingRequestableMembers(suitePath string, declared []ReleaseMember, present []cache.SnapshotMember, allowlist map[string]struct{}) []ReleaseMember`, `(*Adopter).reconcilePending(ctx, suite SuiteRef, snapshotID int64)`; test fixtures `testFlatSuite`, `testDistsSuite`, `layoutSuites`, `skipReason` (in `flat_adoption_test.go`, used by later tasks).

- [ ] **Step 1: Write the failing tests**

Create `internal/freshness/flat_adoption_test.go`:

```go
package freshness

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
)

// Fixture suites for the layout-aware IndexTarget predicates (spec C5):
// the pkgs.k8s.io flat shape and a dists suite.
const (
	testFlatSuite  = "/core:/stable:/v1.34/deb"
	testDistsSuite = "/ubuntu/dists/noble"
)

// layoutSuites are the suites each layout-aware table runs under: a flat
// suite, its `deb <uri> ./` twin, and a dists suite (plan overview, "PR #8
// parity" item 2: the same member paths under a dists suite are no
// IndexTargets).
var layoutSuites = []struct {
	suite string
	flat  bool
}{{testFlatSuite, true}, {testFlatSuite + "/.", true}, {testDistsSuite, false}}

// PR #8 parity item 2, isIndexTarget: a flat suite's root Packages* and
// Sources* (and their pdiff Index) are IndexTargets; under a dists suite
// none of them is. A flat suite's nested binary-<arch>/Packages is none
// either (plan design note 3: apt never requests it for a flat source).
func TestIsIndexTarget_LayoutAware(t *testing.T) {
	targets := []string{"Packages", "Packages.gz", "Packages.xz", "Packages.zst", "Packages.diff/Index", "Sources", "Sources.gz"}
	never := []string{"main/Packages.gz", "Packages_1.0_all.deb", "InRelease", "Release", "Contents-amd64.gz", "Translation-en"}
	for _, l := range layoutSuites {
		for _, p := range targets {
			if got := isIndexTarget(l.suite, p); got != l.flat {
				t.Errorf("isIndexTarget(%q, %q) = %v, want %v", l.suite, p, got, l.flat)
			}
		}
		for _, p := range never {
			if isIndexTarget(l.suite, p) {
				t.Errorf("isIndexTarget(%q, %q) = true, want false", l.suite, p)
			}
		}
		if got := isIndexTarget(l.suite, "main/binary-amd64/Packages"); got == l.flat {
			t.Errorf("isIndexTarget(%q, main/binary-amd64/Packages) = %v, want %v", l.suite, got, !l.flat)
		}
	}
}

// PR #8 parity item 2, indexTargetGroup: a flat suite's Packages variants
// group under "Packages" with the pseudo-arch "all", its Sources under
// "source"; pdiff Index and nested shapes have no group. Under a dists
// suite none of these paths groups.
func TestIndexTargetGroup_LayoutAware(t *testing.T) {
	cases := []struct {
		path, group, arch string
		ok                bool
	}{
		{"Packages", "Packages", "all", true},
		{"Packages.gz", "Packages", "all", true},
		{"Packages.xz", "Packages", "all", true},
		{"Sources.gz", "Sources", "source", true},
		{"Packages.diff/Index", "", "", false},
		{"main/Packages.gz", "", "", false},
	}
	for _, l := range layoutSuites {
		for _, c := range cases {
			want := c
			if !l.flat {
				want.group, want.arch, want.ok = "", "", false
			}
			group, arch, ok := indexTargetGroup(l.suite, c.path)
			if group != want.group || arch != want.arch || ok != want.ok {
				t.Errorf("indexTargetGroup(%q, %q) = (%q, %q, %v), want (%q, %q, %v)",
					l.suite, c.path, group, arch, ok, want.group, want.arch, want.ok)
			}
		}
	}
}

// PR #8 parity item 2, archFromFilteredPath (the flat pdiff arch labels,
// plan design note 4): a flat suite's root Sources, Sources.xz and
// Sources.diff/Index are "source"; its root Packages and Packages.gz (and
// Packages.diff/Index) carry every architecture and are never filtered.
// Under a dists suite none of these root paths is filtered. A root
// Contents-<arch> is filtered under either layout (the optional shapes).
func TestArchFromFilteredPath_LayoutAware(t *testing.T) {
	cases := []struct {
		path     string
		arch     string
		filtered bool
	}{
		{"Sources", "source", true},
		{"Sources.xz", "source", true},
		{"Sources.diff/Index", "source", true},
		{"Packages", "", false},
		{"Packages.gz", "", false},
		{"Packages.diff/Index", "", false},
	}
	for _, l := range layoutSuites {
		for _, c := range cases {
			want := c
			if !l.flat {
				want.arch, want.filtered = "", false
			}
			arch, filtered := archFromFilteredPath(l.suite, c.path)
			if arch != want.arch || filtered != want.filtered {
				t.Errorf("archFromFilteredPath(%q, %q) = (%q, %v), want (%q, %v)",
					l.suite, c.path, arch, filtered, want.arch, want.filtered)
			}
		}
		if arch, filtered := archFromFilteredPath(l.suite, "Contents-amd64.gz"); arch != "amd64" || !filtered {
			t.Errorf("archFromFilteredPath(%q, Contents-amd64.gz) = (%q, %v), want (amd64, true)", l.suite, arch, filtered)
		}
	}
}

// PR #8 parity item 2, missingRequestableIndexGroups: a flat suite's root
// Packages declared but not fetched is flagged even with no allowlist (it
// is the "all" group); served by a compressed variant only, it is not; a
// flat root Sources absent is not flagged unless "source" is allowlisted.
// Under a dists suite none of these root members is required.
func TestMissingRequestableIndexGroups_LayoutAware(t *testing.T) {
	rm := func(p string) ReleaseMember { return ReleaseMember{Path: p} }
	amd64 := map[string]struct{}{"amd64": {}}
	amd64src := map[string]struct{}{"amd64": {}, "source": {}}
	cases := []struct {
		name              string
		declared, fetched []ReleaseMember
		allowlist         map[string]struct{}
		flatWant          []string
	}{
		{"flat Packages declared but not fetched is flagged", []ReleaseMember{rm("Packages"), rm("Packages.gz")}, nil, nil, []string{"Packages"}},
		{"flat Packages served by a compressed variant is fine", []ReleaseMember{rm("Packages"), rm("Packages.gz")}, []ReleaseMember{rm("Packages.gz")}, amd64, nil},
		{"flat Sources absent is not flagged when source is not allowlisted", []ReleaseMember{rm("Packages.gz"), rm("Sources.gz")}, []ReleaseMember{rm("Packages.gz")}, amd64, nil},
		{"flat Sources absent is flagged when source is allowlisted", []ReleaseMember{rm("Packages.gz"), rm("Sources.gz")}, []ReleaseMember{rm("Packages.gz")}, amd64src, []string{"Sources"}},
	}
	for _, l := range layoutSuites {
		for _, c := range cases {
			want := c.flatWant
			if !l.flat {
				want = nil
			}
			if got := missingRequestableIndexGroups(l.suite, c.declared, c.fetched, c.allowlist); !reflect.DeepEqual(got, want) {
				t.Errorf("%s, suite %q: missingRequestableIndexGroups = %v, want %v", c.name, l.suite, got, want)
			}
		}
	}
}

// PR #8 parity item 3: a flat suite adopts, its root Packages and
// Packages.gz fetched relative to the suite path, and the snapshot holds
// InRelease, Packages and Packages.gz. The `deb <uri> ./` twin adopts the
// same way under its own path (ruling R-G4: two suites, two snapshots).
func TestAdopter_FlatSuiteAdopts(t *testing.T) {
	for _, suitePath := range []string{testFlatSuite, testFlatSuite + "/."} {
		t.Run(suitePath, func(t *testing.T) {
			ctx := context.Background()
			env := newAdoptionTestEnv(t)
			suite := SuiteRef{CanonicalScheme: "https", CanonicalHost: "pkgs.k8s.io", SuitePath: suitePath}
			trackSuite(t, env.cache, suite)
			pkgs := fakePackagesStanzas(map[string]string{"amd64/kubelet_1.deb": strings.Repeat("a", 64)})
			gz := gzipBytes(pkgs)
			releaseText, _ := makeRelease(map[string][]byte{"Packages": pkgs, "Packages.gz": gz})
			base := "https://pkgs.k8s.io" + suitePath + "/"
			env.fetcher.put(base+"Packages", pkgs)
			env.fetcher.put(base+"Packages.gz", gz)
			if err := env.adopter.Run(ctx, suite, releaseText, "", ""); err != nil {
				t.Fatalf("Run: %v", err)
			}
			paths := snapshotPaths(t, env.cache, currentID(t, env.cache, suite))
			for _, want := range []string{"InRelease", "Packages", "Packages.gz"} {
				if _, ok := paths[want]; !ok {
					t.Errorf("snapshot missing member %q (have %v)", want, paths)
				}
			}
			for _, u := range env.fetcher.requested() {
				if !strings.HasPrefix(u, base) {
					t.Errorf("fetched %s, want every member under %s", u, base)
				}
			}
		})
	}
}

// PR #8 parity item 3: a flat suite's root Packages is an IndexTarget, so
// its failure fails the run even when optional failures are tolerated,
// and no snapshot without it is published. Translation-en fetches, so the
// all-members-4xx guard cannot be what fails the run. PR #8's assertion
// (ErrAdoptionMemberFetchFailed) is kept, and the cause is pinned: a 404
// is skipped and then refused by the serve-contract guard (the flat "all"
// group; ErrAdoptionGuardFailed, which wraps member_fetch_failed); a 503
// is the fatal IndexTarget fetch failure itself, never tolerated. The
// 404's skip line says what was skipped (reason 4xx_index_target). Under a
// dists suite the same root Packages.gz is an optional member: its 503 is
// tolerated and the suite adopts.
func TestAdopter_FlatPackagesFailureFatal(t *testing.T) {
	for _, c := range []struct {
		name  string
		suite string
		fail  func(f *fakeFetcher, url string)
		fatal bool
		guard bool
	}{
		{"flat, 404", testFlatSuite, (*fakeFetcher).fail404, true, true},
		{"flat, 503", testFlatSuite, (*fakeFetcher).fail503, true, false},
		{"twin, 404", testFlatSuite + "/.", (*fakeFetcher).fail404, true, true},
		{"twin, 503", testFlatSuite + "/.", (*fakeFetcher).fail503, true, false},
		{"dists, 503", testDistsSuite, (*fakeFetcher).fail503, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			env := newAdoptionTestEnv(t)
			ad := newTolerantAdopter(t, env)
			var logs lockedBuffer
			ad.logger = slog.New(slog.NewJSONHandler(&logs, nil))
			suite := SuiteRef{CanonicalScheme: "https", CanonicalHost: "pkgs.k8s.io", SuitePath: c.suite}
			trackSuite(t, env.cache, suite)
			pkgs := fakePackagesStanzas(map[string]string{"amd64/kubelet_1.deb": strings.Repeat("a", 64)})
			tr := []byte("Package: kubelet\nDescription-en: kubelet\n")
			releaseText, _ := makeRelease(map[string][]byte{"Packages.gz": gzipBytes(pkgs), "Translation-en": tr})
			base := "https://pkgs.k8s.io" + c.suite + "/"
			env.fetcher.put(base+"Translation-en", tr)
			c.fail(env.fetcher, base+"Packages.gz")
			err := ad.Run(ctx, suite, releaseText, "", "")
			sf, gerr := env.cache.GetSuiteFreshness(ctx, suite.CanonicalScheme, suite.CanonicalHost, suite.SuitePath)
			if gerr != nil {
				t.Fatal(gerr)
			}
			if !c.fatal {
				if err != nil || sf.CurrentSnapshotID == nil {
					t.Fatalf("Run = %v, current %v; want a dists suite's optional root member tolerated and the suite adopted", err, sf.CurrentSnapshotID)
				}
				return
			}
			var me *AdoptionMemberError
			if !errors.Is(err, ErrAdoptionMemberFetchFailed) || errors.Is(err, ErrAdoptionGuardFailed) != c.guard ||
				!errors.As(err, &me) || !strings.HasPrefix(me.Path, "Packages") {
				t.Fatalf("Run = %v; want member_fetch_failed naming the flat Packages (from the serve-contract guard: %v)", err, c.guard)
			}
			if sf.CurrentSnapshotID != nil {
				t.Fatalf("a snapshot without Packages was published (%d)", *sf.CurrentSnapshotID)
			}
			if c.guard && skipReason(logs.String(), "Packages.gz") != "4xx_index_target" {
				t.Fatalf("Packages.gz skipped as %q, want 4xx_index_target", skipReason(logs.String(), "Packages.gz"))
			}
		})
	}
}

// skipReason returns the reason of the adoption_member_skipped line for
// path in the JSON log text, "" without one.
func skipReason(text, path string) string {
	for _, line := range strings.Split(text, "\n") {
		var m map[string]any
		if line != "" && json.Unmarshal([]byte(line), &m) == nil && m["msg"] == "adoption_member_skipped" && m["path"] == path {
			r, _ := m["reason"].(string)
			return r
		}
	}
	return ""
}

// Deferred minor E-9: adoption_member_skipped names a SHA512-only member's
// declared hash (declared_sha512) — its declared_sha256 is empty — at all
// three skip sites: a 404 (Contents-amd64), the architecture filter
// (Contents-arm64, amd64 allowlisted) and a tolerated optional member's
// integrity failure (Contents-all served with other bytes). A flat
// repository's Release may declare only SHA512 (libnvidia-container).
func TestAdopter_SkippedSHA512OnlyMemberNamesItsHash(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	ad := newReconcileAdopter(t, env, []string{"amd64"}) // tolerant, amd64 allowlisted
	var logs lockedBuffer
	ad.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	suite := SuiteRef{CanonicalScheme: "https", CanonicalHost: "nvidia.github.io", SuitePath: "/libnvidia-container/stable/deb/amd64"}
	trackSuite(t, env.cache, suite)
	pkgs := fakePackagesStanzas(map[string]string{"libnvidia-container1_1.17.0-1_amd64.deb": testHex64})
	members := map[string][]byte{
		"Packages":       pkgs,
		"Contents-amd64": []byte("usr/bin/nvidia-container-cli utils/libnvidia-container-tools\n"),
		"Contents-arm64": []byte("usr/bin/nvidia-container-cli utils/libnvidia-container-tools-arm64\n"),
		"Contents-all":   []byte("usr/share/doc/libnvidia-container/copyright doc/libnvidia-container\n"),
	}
	base := "https://nvidia.github.io" + suite.SuitePath + "/"
	env.fetcher.put(base+"Packages", pkgs)
	env.fetcher.fail404(base + "Contents-amd64")
	env.fetcher.put(base+"Contents-all", []byte("other bytes of the same length as the declared content!!!!!!!!\n"))
	rel := hashRelease("Origin: nvidia\n", false, true, members, nil)
	if err := ad.Run(ctx, suite, rel, "", ""); err != nil {
		t.Fatal(err)
	}
	reasons := map[string]string{"Contents-amd64": "4xx", "Contents-arm64": "arch_not_in_allowlist", "Contents-all": "optional_member_integrity"}
	seen := map[string]bool{}
	for _, line := range strings.Split(logs.String(), "\n") {
		var m map[string]any
		if line == "" || json.Unmarshal([]byte(line), &m) != nil || m["msg"] != "adoption_member_skipped" {
			continue
		}
		path, _ := m["path"].(string)
		want, ok := reasons[path]
		if !ok || m["reason"] != want || m["declared_sha256"] != "" || m["declared_sha512"] != sha512Of(members[path]) {
			t.Fatalf("adoption_member_skipped = %v, want %s skipped (%s) with its declared_sha512", m, path, want)
		}
		seen[path] = true
	}
	if len(seen) != len(reasons) {
		t.Fatalf("skip lines for %v, want one for each of %v", seen, reasons)
	}
}
```

The existing tables keep their rows under a `dists/` suite, and reconcile gains a flat case and the D-27 assertion:

In `internal/freshness/adoption_test.go`, replace

```go
	for _, p := range indexTargets {
		if !isIndexTarget(p) {
			t.Errorf("isIndexTarget(%q) = false, want true", p)
		}
	}
	for _, p := range optional {
		if isIndexTarget(p) {
			t.Errorf("isIndexTarget(%q) = true, want false", p)
		}
	}
```

with

```go
	for _, p := range indexTargets {
		if !isIndexTarget(testDistsSuite, p) {
			t.Errorf("isIndexTarget(%q) = false, want true", p)
		}
	}
	for _, p := range optional {
		if isIndexTarget(testDistsSuite, p) {
			t.Errorf("isIndexTarget(%q) = true, want false", p)
		}
	}
```

In `internal/freshness/adoption_test.go`, replace

```go
			gotArch, gotFiltered := archFromFilteredPath(tc.path)
```

with

```go
			gotArch, gotFiltered := archFromFilteredPath(testDistsSuite, tc.path)
```

In `internal/freshness/adoption_test.go`, replace

```go
			got := missingRequestableIndexGroups(tc.declared, tc.fetched, tc.allowlist)
```

with

```go
			got := missingRequestableIndexGroups(testDistsSuite, tc.declared, tc.fetched, tc.allowlist)
```

In `internal/freshness/phase6_7_skip_repair_test.go`, replace

```go
		group, arch, ok := indexTargetGroup(tc.path)
```

with

```go
		group, arch, ok := indexTargetGroup(testDistsSuite, tc.path)
```

In `internal/freshness/reconcile_test.go`, replace

```go
	got := missingRequestableMembers(declared, present, allow)
	want := []ReleaseMember{
		rm("main/binary-all/Packages", "b"),
		rm("main/binary-all/Packages.gz", "c"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("missingRequestableMembers() = %v, want %v", got, want)
	}
}
```

with

```go
	got := missingRequestableMembers(testDistsSuite, declared, present, allow)
	want := []ReleaseMember{
		rm("main/binary-all/Packages", "b"),
		rm("main/binary-all/Packages.gz", "c"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("missingRequestableMembers() = %v, want %v", got, want)
	}
}

// Spec C5 (layout-aware IndexTargets): reconcile heals a flat suite's root
// Packages — every declared variant of its "all" group — even with no
// allowlist; under a dists suite the same root members are no group, and
// nothing is missing.
func TestMissingRequestableMembers_Flat(t *testing.T) {
	rm := func(path, sha string) ReleaseMember { return ReleaseMember{Path: path, SHA256: sha} }
	declared := []ReleaseMember{rm("Packages", "a"), rm("Packages.gz", "b"), rm("Sources.gz", "c")}
	present := []cache.SnapshotMember{{Path: "Sources.gz"}}
	want := []ReleaseMember{rm("Packages", "a"), rm("Packages.gz", "b")}
	if got := missingRequestableMembers(testFlatSuite, declared, present, nil); !reflect.DeepEqual(got, want) {
		t.Errorf("flat: missingRequestableMembers() = %v, want %v", got, want)
	}
	if got := missingRequestableMembers(testDistsSuite, declared, present, nil); got != nil {
		t.Errorf("dists: missingRequestableMembers() = %v, want nil", got)
	}
}
```

In `internal/freshness/reconcile_test.go`, replace

```go
	if _, err := ad.ReconcileSnapshot(ctx, env.suite, snapID, false); err == nil {
		t.Error("ReconcileSnapshot accepted a corrupted (hash-mismatched) anchor blob — must reject")
	}
```

with

```go
	// Deferred minor D-27: refused by the hash check, not by anything else.
	if _, err := ad.ReconcileSnapshot(ctx, env.suite, snapID, false); err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Errorf("ReconcileSnapshot = %v; want the corrupted (hash-mismatched) anchor blob refused by its hash check", err)
	}
```

and add before `// newReconcileAdopter constructs an Adopter over env.cache with the given` in `internal/freshness/reconcile_test.go`:

```go
// Spec C5 (layout-aware IndexTargets) on the reconcile side: a flat
// snapshot that lost its root Packages members is pending a reconcile —
// its "all" group is required with no allowlist — and the heal fetches
// both variants back into the same snapshot.
func TestReconcileSnapshot_HealsAFlatSuitesPackages(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	ad := newReconcileAdopter(t, env, nil)
	suite := SuiteRef{CanonicalScheme: "https", CanonicalHost: "pkgs.k8s.io", SuitePath: testFlatSuite}
	trackSuite(t, env.cache, suite)
	pkgs := fakePackagesStanzas(map[string]string{"amd64/kubelet_1.deb": strings.Repeat("a", 64)})
	gz := gzipBytes(pkgs)
	rel, hashes := makeRelease(map[string][]byte{"Packages": pkgs, "Packages.gz": gz})
	base := "https://pkgs.k8s.io" + testFlatSuite + "/"
	env.fetcher.put(base+"Packages", pkgs)
	env.fetcher.put(base+"Packages.gz", gz)
	if err := ad.Run(ctx, suite, simtest.ClearsignedFixture(string(rel)), "", ""); err != nil {
		t.Fatal(err)
	}
	snapID := currentID(t, env.cache, suite)
	if err := env.cache.DeleteSnapshotMembersForTest(ctx, snapID, "Packages", "Packages.gz"); err != nil {
		t.Fatal(err)
	}
	if _, pending, err := ad.reconcilePending(ctx, suite, snapID); err != nil || !pending {
		t.Fatalf("reconcilePending = %v, %v; want the flat suite's missing Packages pending", pending, err)
	}
	if n, err := ad.ReconcileSnapshot(ctx, suite, snapID, false); err != nil || n != 2 {
		t.Fatalf("ReconcileSnapshot = %d, %v; want both Packages variants healed", n, err)
	}
	paths := snapshotPaths(t, env.cache, snapID)
	for _, m := range []string{"Packages", "Packages.gz"} {
		if paths[m] != hashes[m] {
			t.Errorf("healed %s = %q, want %q", m, paths[m], hashes[m])
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -gcflags=-e ./internal/freshness/`
Expected: FAIL — the build lists `too many arguments in call to isIndexTarget` (`have (string, string)`, `want (string)`), and the same for `archFromFilteredPath`, `indexTargetGroup`, `missingRequestableIndexGroups`, `missingRequestableMembers` and `reconcilePending`, at every call in `adoption_test.go`, `flat_adoption_test.go`, `phase6_7_skip_repair_test.go` and `reconcile_test.go`.

- [ ] **Step 3: Implement**

In `internal/freshness/adoption.go`, replace

```go
// archFilterOptionalRES are the SPEC6_7 §7 filter extension: per-arch
```

with

```go
// flatArchFilterSourceRE matches a flat suite's root Sources index and its
// pdiff Index (spec C5 flat IndexTargets): the filter scopes them to the
// pseudo-arch "source" as it does a component's source/Sources. A flat
// suite's root Packages carries every architecture and is never filtered.
var flatArchFilterSourceRE = regexp.MustCompile(`^(?:Sources(?:\.(?:gz|xz|bz2))?|Sources\.diff/Index)$`)

// archFilterOptionalRES are the SPEC6_7 §7 filter extension: per-arch
```

In `internal/freshness/adoption.go`, replace

```go
// archFromFilteredPath inspects a Release member's suite-relative path
// and reports the architecture tag the SPEC6_5 §7.2 / SPEC6_7 §7
// filter would key on. Returns ("amd64", true) for
// "main/binary-amd64/Packages.gz" or "Contents-amd64.gz", ("source",
// true) for "main/source/Sources.xz", and ("", false) for any path the
// filter does not scope to (Release.gpg, i18n translations, dep11
// icons, per-component-arch Release files, etc.).
//
// The pseudo-arch "all" is exempt by construction: arch:all content
// (e.g. Debian's Contents-all) serves clients of EVERY architecture,
// so no allowlist may filter it — it reports ("", false) like an
// unfiltered path.
func archFromFilteredPath(p string) (arch string, filtered bool) {
	if m := archFilterBinaryRE.FindStringSubmatch(p); m != nil {
```

with

```go
// archFromFilteredPath inspects a Release member's suite-relative path
// and reports the architecture tag the SPEC6_5 §7.2 / SPEC6_7 §7
// filter would key on. Returns ("amd64", true) for
// "main/binary-amd64/Packages.gz" or "Contents-amd64.gz", ("source",
// true) for "main/source/Sources.xz", and ("", false) for any path the
// filter does not scope to (Release.gpg, i18n translations, dep11
// icons, per-component-arch Release files, etc.).
//
// The pseudo-arch "all" is exempt by construction: arch:all content
// (e.g. Debian's Contents-all) serves clients of EVERY architecture,
// so no allowlist may filter it — it reports ("", false) like an
// unfiltered path.
//
// suitePath makes it layout-aware (spec C5 flat IndexTargets; the flat
// pdiff arch labels of plan design note 4): a flat suite's root Sources,
// Sources.* and Sources.diff/Index are "source", and its root Packages*
// and Packages.diff/Index — every architecture in one file, the
// pseudo-arch "all" — are never filtered. Under a dists suite a root
// Sources is no shape the filter knows.
func archFromFilteredPath(suitePath, p string) (arch string, filtered bool) {
	if aptpath.IsFlatSuitePath(suitePath) && flatArchFilterSourceRE.MatchString(p) {
		return "source", true
	}
	if m := archFilterBinaryRE.FindStringSubmatch(p); m != nil {
```

In `internal/freshness/adoption.go`, replace

```go
var indexTargetRE = regexp.MustCompile(
	`(?:^|/)(?:binary-[a-z][a-z0-9]*/Packages|source/Sources)(?:\.diff/Index|\.[a-z0-9]+)?$`)

// isIndexTarget reports whether a Release member's suite-relative path
// is an apt IndexTarget — a per-arch Packages* / per-component Sources*
// (any codec) or their pdiff Index, i.e. the files apt actually fetches
// for `apt update` + `apt install`. Everything else (Contents-*, dep11
// Components/icons, i18n Translations, …) is an optional member apt does
// not require.
//
// REC 1 keys on this: when [adoption].tolerate_optional_member_failures
// is set, an integrity/availability failure on a non-IndexTarget member
// is skipped instead of aborting the whole suite; IndexTargets stay
// fatal regardless.
func isIndexTarget(p string) bool {
	return indexTargetRE.MatchString(p)
}
```

with

```go
var indexTargetRE = regexp.MustCompile(
	`(?:^|/)(?:binary-[a-z][a-z0-9]*/Packages|source/Sources)(?:\.diff/Index|\.[a-z0-9]+)?$`)

// flatIndexTargetRE is indexTargetRE for a flat suite (spec C5): its root
// Packages* / Sources* (any codec) and their pdiff Index, which a flat
// Release declares at the suite root — the files apt requests for a
// `deb <uri>/ /` or `deb <uri> ./` source.
var flatIndexTargetRE = regexp.MustCompile(`^(?:Packages|Sources)(?:\.diff/Index|\.[a-z0-9]+)?$`)

// isIndexTarget reports whether a Release member's suite-relative path
// is an apt IndexTarget — a per-arch Packages* / per-component Sources*
// (any codec) or their pdiff Index, i.e. the files apt actually fetches
// for `apt update` + `apt install`. Everything else (Contents-*, dep11
// Components/icons, i18n Translations, …) is an optional member apt does
// not require.
//
// REC 1 keys on this: when [adoption].tolerate_optional_member_failures
// is set, an integrity/availability failure on a non-IndexTarget member
// is skipped instead of aborting the whole suite; IndexTargets stay
// fatal regardless.
//
// It is layout-aware (spec C5): a flat suite's IndexTargets are its root
// shapes (flatIndexTargetRE) and nothing else — apt never requests a
// nested binary-<arch>/Packages for a flat source — and a dists suite's
// are indexTargetRE's, so a root Packages under a dists suite is none.
func isIndexTarget(suitePath, p string) bool {
	if aptpath.IsFlatSuitePath(suitePath) {
		return flatIndexTargetRE.MatchString(p)
	}
	return indexTargetRE.MatchString(p)
}
```

In `internal/freshness/adoption.go`, replace

```go
var indexTargetGroupRE = regexp.MustCompile(
	`(?:^|/)(binary-([a-z][a-z0-9]*)/Packages|source/Sources)(?:\.[a-z0-9]+)?$`)
```

with

```go
var indexTargetGroupRE = regexp.MustCompile(
	`(?:^|/)(binary-([a-z][a-z0-9]*)/Packages|source/Sources)(?:\.[a-z0-9]+)?$`)

// flatIndexTargetGroupRE is indexTargetGroupRE for a flat suite: a root
// Packages or Sources with any (or no) codec; capture 1 is the group key.
//
// AIDEV-NOTE: indexTargetRE, flatIndexTargetRE, indexTargetGroupRE, this
// one and internal/handler's indexTargetPathRE / flatIndexTargetPathRE
// describe ONE surface — the files apt installs from — in two layouts
// (spec C5: the root shapes count only for a flat suite). They live in
// two packages on purpose (no import edge); widen or narrow them
// together, and keep the layout rule (aptpath.IsFlatSuitePath) the same
// on both sides.
var flatIndexTargetGroupRE = regexp.MustCompile(`^(Packages|Sources)(?:\.[a-z0-9]+)?$`)
```

In `internal/freshness/adoption.go`, replace

```go
// Returns (group key, arch, true) for base IndexTarget shapes — arch
// is the pseudo-arch "source" for Sources groups — and ("", "", false)
// for everything else.
func indexTargetGroup(p string) (group, arch string, ok bool) {
	m := indexTargetGroupRE.FindStringSubmatchIndex(p)
```

with

```go
// Returns (group key, arch, true) for base IndexTarget shapes — arch
// is the pseudo-arch "source" for Sources groups — and ("", "", false)
// for everything else.
//
// It is layout-aware (spec C5): a flat suite's root Packages holds every
// architecture in one file, so it groups under the pseudo-arch "all",
// which missingRequestableIndexGroups always requires, and its root
// Sources under "source"; a dists suite's groups are today's.
func indexTargetGroup(suitePath, p string) (group, arch string, ok bool) {
	if aptpath.IsFlatSuitePath(suitePath) {
		fm := flatIndexTargetGroupRE.FindStringSubmatch(p)
		switch {
		case fm == nil:
			return "", "", false
		case fm[1] == "Sources":
			return fm[1], "source", true
		}
		return fm[1], "all", true
	}
	m := indexTargetGroupRE.FindStringSubmatchIndex(p)
```

In `internal/freshness/adoption.go`, replace

```go
func missingRequestableIndexGroups(declared, fetched []ReleaseMember, allowlist map[string]struct{}) []string {
	// Declared IndexTarget groups whose arch a configured client requests.
	required := make(map[string]struct{})
	for _, m := range declared {
		group, arch, ok := indexTargetGroup(m.Path)
```

with

```go
//
// suitePath makes the groups layout-aware (indexTargetGroup): a flat
// suite's root Packages is its "all" group, required like binary-all.
func missingRequestableIndexGroups(suitePath string, declared, fetched []ReleaseMember, allowlist map[string]struct{}) []string {
	// Declared IndexTarget groups whose arch a configured client requests.
	required := make(map[string]struct{})
	for _, m := range declared {
		group, arch, ok := indexTargetGroup(suitePath, m.Path)
```

In `internal/freshness/adoption.go`, replace

```go
	for _, m := range fetched {
		if group, _, ok := indexTargetGroup(m.Path); ok {
			delete(required, group)
		}
	}
```

with

```go
	for _, m := range fetched {
		if group, _, ok := indexTargetGroup(suitePath, m.Path); ok {
			delete(required, group)
		}
	}
```

In `internal/freshness/adoption.go`, replace

```go
			if arch, filtered := archFromFilteredPath(m.Path); filtered {
				if _, ok := a.architectureAllowlist[arch]; !ok {
					a.logger.Warn("adoption_member_skipped",
						"canonical_host", suite.CanonicalHost,
						"suite_path", suite.SuitePath,
						"path", m.Path,
						"declared_sha256", m.SHA256,
						"reason", "arch_not_in_allowlist",
```

with

```go
			if arch, filtered := archFromFilteredPath(suite.SuitePath, m.Path); filtered {
				if _, ok := a.architectureAllowlist[arch]; !ok {
					a.logger.Warn("adoption_member_skipped",
						"canonical_host", suite.CanonicalHost,
						"suite_path", suite.SuitePath,
						"path", m.Path,
						"declared_sha256", m.SHA256,
						"declared_sha512", m.SHA512,
						"reason", "arch_not_in_allowlist",
```

In `internal/freshness/adoption.go`, replace

```go
			if group, arch, ok := indexTargetGroup(m.Path); ok {
				if _, want := a.requiredArchitectures[arch]; want {
```

with

```go
			if group, arch, ok := indexTargetGroup(suite.SuitePath, m.Path); ok {
				if _, want := a.requiredArchitectures[arch]; want {
```

In `internal/freshness/adoption.go`, replace

```go
			if a.tolerateOptionalMembers && !isIndexTarget(m.Path) &&
```

with

```go
			if a.tolerateOptionalMembers && !isIndexTarget(suite.SuitePath, m.Path) &&
```

In `internal/freshness/adoption.go`, replace

```go
					"path", m.Path,
					"declared_sha256", m.SHA256,
					"reason", "optional_member_integrity",
```

with

```go
					"path", m.Path,
					"declared_sha256", m.SHA256,
					"declared_sha512", m.SHA512,
					"reason", "optional_member_integrity",
```

In `internal/freshness/adoption.go`, replace

```go
	if missing := missingRequestableIndexGroups(members, fetchedMembers, a.architectureAllowlist); len(missing) > 0 {
		for _, g := range missing {
			arch := "all"
			if _, a2, ok := indexTargetGroup(g); ok {
```

with

```go
	if missing := missingRequestableIndexGroups(suite.SuitePath, members, fetchedMembers, a.architectureAllowlist); len(missing) > 0 {
		for _, g := range missing {
			arch := "all"
			if _, a2, ok := indexTargetGroup(suite.SuitePath, g); ok {
```

In `internal/freshness/adoption.go`, replace

```go
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
```

with

```go
			reason := "4xx"
			if isIndexTarget(suite.SuitePath, m.Path) {
				reason = "4xx_index_target"
			}
			a.logger.Warn("adoption_member_skipped",
				"canonical_host", suite.CanonicalHost,
				"suite_path", suite.SuitePath,
				"path", m.Path,
				"declared_sha256", m.SHA256,
				"declared_sha512", m.SHA512,
				"upstream_status", se.Code,
```

In `internal/freshness/reconcile.go`, replace

```go
	missing := missingRequestableMembers(declared, present, a.architectureAllowlist)
	if len(missing) == 0 {
```

with

```go
	missing := missingRequestableMembers(suite.SuitePath, declared, present, a.architectureAllowlist)
	if len(missing) == 0 {
```

In `internal/freshness/reconcile.go`, replace

```go
		if _, arch, ok := indexTargetGroup(m.Path); ok {
			adoptionReconciledTotal.Inc(arch)
		}
```

with

```go
		if _, arch, ok := indexTargetGroup(suite.SuitePath, m.Path); ok {
			adoptionReconciledTotal.Inc(arch)
		}
```

In `internal/freshness/reconcile.go`, replace

```go
// admission slot, inside the check's recover.
func (a *Adopter) reconcilePending(ctx context.Context, snapshotID int64) ([]ReleaseMember, bool, error) {
```

with

```go
// admission slot, inside the check's recover. suite is the snapshot's
// suite: it makes the requestable groups layout-aware (a flat suite's
// root Packages is required, spec C5).
func (a *Adopter) reconcilePending(ctx context.Context, suite SuiteRef, snapshotID int64) ([]ReleaseMember, bool, error) {
```

In `internal/freshness/reconcile.go`, replace

```go
	if len(missingRequestableMembers(declared, present, a.architectureAllowlist)) == 0 {
```

with

```go
	if len(missingRequestableMembers(suite.SuitePath, declared, present, a.architectureAllowlist)) == 0 {
```

In `internal/freshness/reconcile.go`, replace

```go
// is needed because missingRequestableIndexGroups only inspects m.Path, so
// only Path is populated in the synthetic presentMembers slice. This is
// safe because missingRequestableIndexGroups only calls indexTargetGroup(m.Path).
func missingRequestableMembers(declared []ReleaseMember, present []cache.SnapshotMember, allowlist map[string]struct{}) []ReleaseMember {
	presentMembers := make([]ReleaseMember, len(present))
	for i, p := range present {
		presentMembers[i] = ReleaseMember{Path: p.Path}
	}
	missingGroups := missingRequestableIndexGroups(declared, presentMembers, allowlist)
```

with

```go
// is needed because missingRequestableIndexGroups only inspects m.Path, so
// only Path is populated in the synthetic presentMembers slice. This is
// safe because missingRequestableIndexGroups only calls
// indexTargetGroup(suitePath, m.Path). suitePath makes the groups
// layout-aware (spec C5: a flat suite's root Packages is required).
func missingRequestableMembers(suitePath string, declared []ReleaseMember, present []cache.SnapshotMember, allowlist map[string]struct{}) []ReleaseMember {
	presentMembers := make([]ReleaseMember, len(present))
	for i, p := range present {
		presentMembers[i] = ReleaseMember{Path: p.Path}
	}
	missingGroups := missingRequestableIndexGroups(suitePath, declared, presentMembers, allowlist)
```

In `internal/freshness/reconcile.go`, replace

```go
	for _, m := range declared {
		if group, _, ok := indexTargetGroup(m.Path); ok {
			if _, missing := want[group]; missing {
```

with

```go
	for _, m := range declared {
		if group, _, ok := indexTargetGroup(suitePath, m.Path); ok {
			if _, missing := want[group]; missing {
```

In `internal/freshness/freshness.go`, replace

```go
	declared, needReconcile, err := c.adopter.reconcilePending(ctx, rp.snapshotID)
```

with

```go
	declared, needReconcile, err := c.adopter.reconcilePending(ctx, rp.suite, rp.snapshotID)
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/freshness/`
Expected: PASS (about 17–19 s).

Run: `go vet ./... && go test ./internal/handler/ ./cmd/...`
Expected: PASS (no other package calls these unexported predicates; the handler's alert is Task 6's).

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| `isIndexTarget` ignores the layout (`return indexTargetRE.MatchString(p)` alone) | `TestIsIndexTarget_LayoutAware` (`isIndexTarget("/core:/stable:/v1.34/deb", "Packages") = false, want true`) |
| `isIndexTarget` for a flat suite adds the root shapes to the `dists/` ones | `TestIsIndexTarget_LayoutAware` (`isIndexTarget("/core:/stable:/v1.34/deb", main/binary-amd64/Packages) = true, want false`) |
| `isIndexTarget` matches the root shapes under any layout | `TestIsIndexTarget_LayoutAware`, `TestAdopter_FlatPackagesFailureFatal/dists,_503` (`Run = adoption_member_fetch_failed: Packages.gz: …; want a dists suite's optional root member tolerated and the suite adopted`) |
| a flat `Sources` groups under `all` | `TestIndexTargetGroup_LayoutAware` (`indexTargetGroup("/core:/stable:/v1.34/deb", "Sources.gz") = ("Sources", "all", true), want ("Sources", "source", true)`), `TestMissingRequestableIndexGroups_LayoutAware` |
| `indexTargetGroup` without its flat branch | `TestIndexTargetGroup_LayoutAware`, `TestMissingRequestableIndexGroups_LayoutAware` (`flat Packages declared but not fetched is flagged … = [], want [Packages]`), `TestAdopter_FlatPackagesFailureFatal/flat,_404` (`Run = <nil>; want member_fetch_failed …`), `TestMissingRequestableMembers_Flat`, `TestReconcileSnapshot_HealsAFlatSuitesPackages` |
| `archFromFilteredPath` without its flat branch | `TestArchFromFilteredPath_LayoutAware` (`archFromFilteredPath("/core:/stable:/v1.34/deb", "Sources") = ("", false), want ("source", true)`) |
| `archFromFilteredPath`'s root `Sources` under any layout | `TestArchFromFilteredPath_LayoutAware` (`archFromFilteredPath("/ubuntu/dists/noble", "Sources") = ("source", true), want ("", false)`) |
| `runShared`'s tolerance check passes `""` for the suite path | `TestAdopter_FlatPackagesFailureFatal/flat,_503` (`Run = adoption_guard_failed: …: Packages: requestable index target absent from snapshot …; want member_fetch_failed naming the flat Packages (from the serve-contract guard: false)`) |
| `runShared`'s serve-contract guard passes `""` | `TestAdopter_FlatPackagesFailureFatal/flat,_404` (`Run = <nil>; …`) |
| `adoptMember`'s 4xx reason passes `""` | `TestAdopter_FlatPackagesFailureFatal/flat,_404` (`Packages.gz skipped as "4xx", want 4xx_index_target`) |
| `reconcilePending` passes `""` | `TestReconcileSnapshot_HealsAFlatSuitesPackages` (`reconcilePending = false, <nil>; want the flat suite's missing Packages pending`) |
| `reconcileSnapshot` passes `""` | `TestReconcileSnapshot_HealsAFlatSuitesPackages` (`ReconcileSnapshot = 0, <nil>; want both Packages variants healed`) |
| `missingRequestableMembers` passes `""` on | `TestMissingRequestableMembers_Flat` (`flat: missingRequestableMembers() = [], want [{Packages a  0} {Packages.gz b  0}]`), `TestReconcileSnapshot_HealsAFlatSuitesPackages` |
| E-9: the 4xx skip line without `declared_sha512` | `TestAdopter_SkippedSHA512OnlyMemberNamesItsHash` (`adoption_member_skipped = map[… path:Contents-amd64 reason:4xx …], want Contents-amd64 skipped (4xx) with its declared_sha512`) |
| E-9: the filter's skip line without it | `TestAdopter_SkippedSHA512OnlyMemberNamesItsHash` (`… path:Contents-arm64 reason:arch_not_in_allowlist …`) |
| E-9: the tolerated failure's skip line without it | `TestAdopter_SkippedSHA512OnlyMemberNamesItsHash` (`… path:Contents-all reason:optional_member_integrity …`) |
| D-27: the anchor check's error says something other than `hash mismatch` (another refusal) | `TestReconcileSnapshot_RejectsCorruptedAnchor` (`ReconcileSnapshot = reconcile: InRelease anchor blob hash differs …; want the corrupted (hash-mismatched) anchor blob refused by its hash check`) |

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/freshness/adoption.go internal/freshness/reconcile.go internal/freshness/freshness.go internal/freshness/flat_adoption_test.go internal/freshness/adoption_test.go internal/freshness/phase6_7_skip_repair_test.go internal/freshness/reconcile_test.go
git add internal/freshness/
git commit -m "Make IndexTargets layout-aware for flat suites (spec C5); PR #8's adoption tests; deferred minors E-9, D-27"
```

(Record the mutation table in the commit message.)

### Task 3: `validateFilename` and ruling R-G1

Spec C5 "Filenames": `ParsePackages` validates Packages Filenames with `validateFilename` — `.` segments (`./cuda.deb`) and `%` allowed and kept literally, the rest of `validateMemberPath`'s refusals kept, plus control bytes, `?`/`#` and a 4,096-byte bound, and a name made only of `.` segments (design note 5); `ParseRelease` keeps `validateMemberPath`. Ruling R-G1 (the E carry-forward): a body whose stanzas all name a Filename and none a SHA256 parses — no rows, every Filename reported — so its snapshot adopts with incomplete coverage (`no_sha256`); a body with no Filename stanza at all (an HTML page, a foreign file) keeps failing, a whitespace-only body still parses to nothing, and the stanza cap counts a body of gaps alone. Pinned at the parser and end to end through the adopter (a `dists/` suite here: a flat suite's coverage is Task 4's).

**Files:**
- Modify: `internal/freshness/packages_parse.go` (`parsePackages`' tail and its Filename check, `maxFilenameBytes`, `validateFilename`), `internal/freshness/packages_parse_test.go`, `internal/freshness/flat_adoption_test.go`

**Interfaces:**
- Consumes: `sha` (`release_parse_test.go`), `strconvI`, `newAdoptionTestEnv`, `hashRelease`, `noble`, `currentID`, `logRecords`, `lockedBuffer` (freshness tests); Task 2's `flat_adoption_test.go`.
- Produces: unexported `validateFilename(f string) error`, `maxFilenameBytes`; `parsePackages` accepts a body of `no_sha256` stanzas (same signature).

- [ ] **Step 1: Write the failing tests**

In `internal/freshness/packages_parse_test.go`, replace

```go
func TestParsePackages_BadFilename(t *testing.T) {
	cases := []struct{ name, fn string }{
		{"absolute", "/etc/shadow"},
		{"dotdot", "pool/../etc/shadow"},
		{"contains NUL", "pool/foo\x00.deb"},
	}
```

with

```go
func TestParsePackages_BadFilename(t *testing.T) {
	cases := []struct{ name, fn string }{
		{"absolute", "/etc/shadow"},
		{"dotdot", "pool/../etc/shadow"},
		{"contains NUL", "pool/foo\x00.deb"},
		// spec C5 validateFilename
		{"control byte", "pool/foo\x1b.deb"},
		{"backslash", "pool\\foo.deb"},
		{"question mark", "pool/foo.deb?x"},
		{"hash", "pool/foo.deb#x"},
		{"empty segment", "pool//foo.deb"},
		{"leading dotdot", "../foo.deb"},
		{"only dots", "./."},
		{"too long", strings.Repeat("a", 4097)},
	}
```

In `internal/freshness/packages_parse_test.go`, replace

```go
func TestParsePackages_NonEmptyButNoUsableStanzas(t *testing.T) {
	// A well-formed Packages-shaped file in which no stanza declares
	// both Filename and SHA256. This is structurally legal apt input
	// (every field is recognized) but contributes zero rows to
	// package_hash. SPEC2 §7.5 step 8 expects "for each pkg with
	// Filename and SHA256" — zero pkgs means zero rows. Treat this
	// as adoption_parse_failed so a downloaded-but-broken Packages
	// file (apt error page rendered through gzip's gibberish, a
	// truncated transfer, or a legitimate but empty component) does
	// not silently produce an adoption with no .deb hash coverage.
	body := "Package: foo\n" +
```

with

```go
func TestParsePackages_NonEmptyButNoUsableStanzas(t *testing.T) {
	// A Packages-shaped file in which no stanza names a Filename. It
	// declares no package file at all, so it is no Packages index:
	// adoption_parse_failed, so a downloaded-but-broken Packages file (an
	// error page, a truncated transfer) does not silently produce an
	// adoption with no .deb hash coverage. A body whose stanzas name
	// Filenames without a SHA256 parses (ruling R-G1:
	// TestParsePackages_OnlyStanzasWithoutSHA256Parse), and a blank body
	// (an empty component's index) parses to nothing
	// (TestParsePackages_AllWhitespaceIsZeroResult).
	body := "Package: foo\n" +
```

In `internal/freshness/packages_parse_test.go`, replace

```go
		{"a row and gaps up to the cap", good + gaps(MaxPackageStanzas-1), 1, MaxPackageStanzas - 1, false},
		{"one gap past the cap", good + gaps(MaxPackageStanzas), 0, 0, true},
		{"one row past the cap", gaps(MaxPackageStanzas) + good, 0, 0, true},
```

with

```go
		{"a row and gaps up to the cap", good + gaps(MaxPackageStanzas-1), 1, MaxPackageStanzas - 1, false},
		{"one gap past the cap", good + gaps(MaxPackageStanzas), 0, 0, true},
		{"one row past the cap", gaps(MaxPackageStanzas) + good, 0, 0, true},
		// Ruling R-G1: a body of gaps alone parses, up to the same cap.
		{"gaps alone up to the cap", gaps(MaxPackageStanzas), 0, MaxPackageStanzas, false},
		{"gaps alone past the cap", gaps(MaxPackageStanzas + 1), 0, 0, true},
```

In `internal/freshness/packages_parse_test.go`, add before `// MaxPackageStanzas bounds everything parsePackages keeps — rows and`:

```go
// Ruling R-G1 (the E carry-forward): a body whose stanzas all name a
// Filename but none a SHA256 — an index that declares only SHA512, SHA1 or
// MD5sum — parses with no rows and every Filename reported, so its
// snapshot adopts with incomplete coverage (no_sha256) instead of failing
// its parse at every check. ParsePackages returns no rows and no error.
func TestParsePackages_OnlyStanzasWithoutSHA256Parse(t *testing.T) {
	body := "Package: a\nFilename: ./a_1_amd64.deb\nSHA512: " + strings.Repeat("a", 128) + "\n\n" +
		"Package: b\nFilename: ./b_1_amd64.deb\nMD5sum: 0123456789abcdef0123456789abcdef\n"
	refs, noSHA256, err := parsePackages([]byte(body))
	if err != nil || len(refs) != 0 || strings.Join(noSHA256, ",") != "./a_1_amd64.deb,./b_1_amd64.deb" {
		t.Fatalf("parsePackages = %v, %q, %v; want no rows, both Filenames reported, no error", refs, noSHA256, err)
	}
	if refs, err := ParsePackages([]byte(body)); err != nil || len(refs) != 0 {
		t.Fatalf("ParsePackages = %v, %v; want no rows and no error", refs, err)
	}
}

// Ruling R-G1's other side: a body with no Filename stanza at all — an
// HTML error page, a foreign file — still fails its parse.
func TestParsePackages_NoFilenameStanzaStillFails(t *testing.T) {
	for name, body := range map[string]string{
		"HTML error page":          "<html><head><title>404 Not Found</title></head><body>Not Found</body></html>\n",
		"foreign file":             "Origin: Example\nLabel: Example\nSuite: stable\n",
		"SHA512 only, no Filename": "Package: a\nSHA512: " + strings.Repeat("a", 128) + "\n",
	} {
		if _, _, err := parsePackages([]byte(body)); err == nil {
			t.Errorf("%s: parsePackages succeeded, want a parse error", name)
		}
	}
}

// Spec C5 validateFilename: "." segments and "%" are allowed and kept
// literally — apt requests <base>/./x.deb, and a Filename's "%" is a
// literal byte that spec C1 re-encodes upstream.
func TestParsePackages_KeepsDotAndPercentFilenamesLiterally(t *testing.T) {
	body := "Package: cuda\nArchitecture: amd64\nVersion: 1\nFilename: ./cuda_12.6_amd64.deb\nSHA256: " + sha(1) + "\n\n" +
		"Package: pkg\nArchitecture: amd64\nVersion: 1:2\nFilename: pool/p/pkg_1%3a2_amd64.deb\nSHA256: " + sha(2) + "\n\n" +
		"Package: nested\nArchitecture: all\nVersion: 1\nFilename: a/./b/nested_1_all.deb\nSHA256: " + sha(3) + "\n"
	refs, err := ParsePackages([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range refs {
		got = append(got, r.Filename)
	}
	if want := "./cuda_12.6_amd64.deb,pool/p/pkg_1%3a2_amd64.deb,a/./b/nested_1_all.deb"; strings.Join(got, ",") != want {
		t.Fatalf("Filenames = %q, want %s", got, want)
	}
}

// Spec C5 validateFilename, the table: the shapes real flat repositories
// publish are accepted, and the spec's refusals stay refused.
func TestValidateFilename(t *testing.T) {
	for _, c := range []struct {
		f  string
		ok bool
	}{
		{"pool/main/f/foo/foo_1.0_amd64.deb", true},
		{"amd64/kubectl_1.34.1-1.1_amd64.deb", true}, // pkgs.k8s.io
		{"./cuda-toolkit_12.6.0-1_amd64.deb", true},  // NVIDIA CUDA, libnvidia-container
		{"binary/jenkins_2.479.3_all.deb", true},     // pkg.jenkins.io
		{"direct/jenkins_2.479.3_all.deb", true},
		{"files/sublime-text_build-4180_amd64.deb", true}, // download.sublimetext.com
		{"pkg_1%3a2.0_amd64.deb", true},                   // an epoch, %-encoded by the publisher
		{"a/./b/./c.deb", true},
		{".hidden.deb", true},
		{"..deb", true},
		{strings.Repeat("a", 4096), true},
		{"", false},
		{"/abs.deb", false},
		{"a\x00b.deb", false},
		{"a\x1fb.deb", false},
		{"a\x7fb.deb", false},
		{"a\\b.deb", false},
		{"a?b.deb", false},
		{"a#b.deb", false},
		{"a/../b.deb", false},
		{"..", false},
		{"a//b.deb", false},
		{"a/", false},
		{"./", false},
		{".", false},
		{"./.", false},
		{strings.Repeat("a", 4097), false},
	} {
		if err := validateFilename(c.f); (err == nil) != c.ok {
			t.Errorf("validateFilename(%q) = %v, want ok=%v", c.f, err, c.ok)
		}
	}
}
```

Append to `internal/freshness/flat_adoption_test.go`:

```go
// Ruling R-G1 end to end: a signed suite whose Packages declares only
// SHA512 in every stanza adopts — its snapshot's coverage incomplete
// (package_coverage_incomplete, reason no_sha256), no .deb vouched — where
// it failed adoption_parse_failed at every check before. An HTML page
// declared as the Packages member (a broken publisher) still fails the
// adoption, and nothing is adopted.
func TestAdopter_PackagesWithoutAnySHA256AdoptsIncomplete(t *testing.T) {
	ctx := context.Background()
	deb := "/ubuntu/pool/main/a/a_1_amd64.deb"
	t.Run("no stanza has a SHA256", func(t *testing.T) {
		env := newAdoptionTestEnv(t)
		var logs lockedBuffer
		env.adopter.logger = slog.New(slog.NewJSONHandler(&logs, nil))
		pkgs := []byte("Package: a\nArchitecture: amd64\nVersion: 1\nFilename: pool/main/a/a_1_amd64.deb\nSHA512: " +
			strings.Repeat("a", 128) + "\n")
		env.fetcher.put(noble+"main/binary-amd64/Packages", pkgs)
		rel := hashRelease("Origin: x\n", false, true, map[string][]byte{"main/binary-amd64/Packages": pkgs}, nil)
		if err := env.adopter.Run(ctx, env.suite, rel, "", ""); err != nil {
			t.Fatalf("Run = %v; want the SHA512-only index adopted (ruling R-G1)", err)
		}
		snap, err := env.cache.GetSuiteSnapshot(ctx, currentID(t, env.cache, env.suite))
		if err != nil || snap.PackageCoverageComplete {
			t.Fatalf("snapshot %+v, %v; want package_coverage_complete = false", snap, err)
		}
		if rows, err := env.cache.DeclaredHashesForPath(ctx, "http", "archive.ubuntu.com", deb); err != nil || len(rows) != 0 {
			t.Fatalf("%s vouched by %v (%v), want nothing", deb, rows, err)
		}
		recs := logRecords(t, &logs, "package_coverage_incomplete")
		if len(recs) != 1 || recs[0]["reason"] != "no_sha256" || recs[0]["no_sha256_stanzas"] != float64(1) {
			t.Fatalf("package_coverage_incomplete = %v, want one line, reason no_sha256, 1 stanza", recs)
		}
	})
	t.Run("an HTML page", func(t *testing.T) {
		env := newAdoptionTestEnv(t)
		html := []byte("<html><body>Not Found</body></html>\n")
		env.fetcher.put(noble+"main/binary-amd64/Packages", html)
		rel := hashRelease("Origin: x\n", true, false, map[string][]byte{"main/binary-amd64/Packages": html}, nil)
		if err := env.adopter.Run(ctx, env.suite, rel, "", ""); !errors.Is(err, ErrAdoptionParseFailed) {
			t.Fatalf("Run = %v, want ErrAdoptionParseFailed", err)
		}
		sf, err := env.cache.GetSuiteFreshness(ctx, env.suite.CanonicalScheme, env.suite.CanonicalHost, env.suite.SuitePath)
		if err != nil || sf.CurrentSnapshotID != nil {
			t.Fatalf("row %+v, %v; want nothing adopted", sf, err)
		}
	})
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -gcflags=-e ./internal/freshness/`
Expected: FAIL — `undefined: validateFilename` (`packages_parse_test.go`).

- [ ] **Step 3: Implement**

In `internal/freshness/packages_parse.go`, replace

```go
		if !validHexSHA256(sha256) {
			return fmt.Errorf("invalid sha256 %q (filename %q)", sha256, filename)
		}
		if err := validateMemberPath(filename); err != nil {
			return fmt.Errorf("invalid filename %q: %w", filename, err)
		}
```

with

```go
		if !validHexSHA256(sha256) {
			return fmt.Errorf("invalid sha256 %q (filename %q)", sha256, filename)
		}
		if err := validateFilename(filename); err != nil {
			return fmt.Errorf("invalid filename %q: %w", filename, err)
		}
```

In `internal/freshness/packages_parse.go`, replace

```go
	if len(out) == 0 {
		// A non-empty input that produced zero usable stanzas is
		// suspicious enough to flag — likely a downloaded HTML error
		// page or a truncated file. The freshness layer logs this as
		// adoption_parse_failed and the next periodic tick retries.
		if len(bytes.TrimSpace(text)) > 0 {
			return nil, nil, errors.New("packages: no stanzas with both Filename and SHA256")
		}
	}
	return out, noSHA256, nil
}
```

with

```go
	if len(out) == 0 && len(noSHA256) == 0 && len(bytes.TrimSpace(text)) > 0 {
		// A non-blank input in which no stanza names a Filename is no
		// Packages index — a downloaded HTML error page, a foreign or
		// truncated file. The freshness layer logs it as
		// adoption_parse_failed and the next periodic tick retries.
		//
		// AIDEV-NOTE: ruling R-G1 (the E carry-forward). A body whose
		// stanzas all name a Filename but none a SHA256 (an index that
		// declares only SHA512, SHA1 or MD5sum) parses: no rows, every
		// Filename reported, so its snapshot adopts with incomplete
		// coverage (no_sha256, buildPackageHashes) instead of failing at
		// every check. A whitespace-only body (an empty component's real,
		// empty index) was never an error and still parses to nothing.
		return nil, nil, errors.New("packages: no stanzas with a Filename")
	}
	return out, noSHA256, nil
}

// maxFilenameBytes bounds a Packages Filename (spec C5 validateFilename).
const maxFilenameBytes = 4096

// validateFilename is spec C5's check of a Packages Filename, in place of
// the Release members' validateMemberPath (which ParseRelease keeps): it
// allows "." segments — apt requests <base>/./x.deb literally, and a
// dpkg-scanpackages index (CUDA, libnvidia-container) writes ./x.deb — and
// "%" (literal bytes, re-encoded upstream by spec C1). It refuses, fatal
// as before: an empty name, an absolute one, NUL or another control byte,
// a backslash, "?" or "#", a ".." segment, an empty segment, a name longer
// than maxFilenameBytes, and a name made only of "." segments (it names no
// file, and its DotClean is empty: plan design note 5).
//
// AIDEV-NOTE: the literal Filename is the package_hash path's tail (spec
// C6: FlatBase(suite) + "/" + F, kept literally because apt requests
// exactly that), so anything that could step outside the base — "..", an
// absolute name, an empty segment that would collapse a path — stays
// refused here.
func validateFilename(f string) error {
	switch {
	case f == "":
		return errors.New("empty")
	case len(f) > maxFilenameBytes:
		return fmt.Errorf("longer than %d bytes", maxFilenameBytes)
	case strings.HasPrefix(f, "/"):
		return errors.New("absolute path not allowed")
	case strings.ContainsRune(f, '\\'):
		return errors.New("contains backslash")
	case strings.ContainsAny(f, "?#"):
		return errors.New("contains URL delimiter")
	}
	for i := 0; i < len(f); i++ {
		if c := f[i]; c < 0x20 || c == 0x7f {
			return fmt.Errorf("contains control byte 0x%02x", c)
		}
	}
	named := false
	for _, seg := range strings.Split(f, "/") {
		switch seg {
		case "":
			return errors.New("contains empty segment")
		case "..":
			return errors.New("contains .. segment")
		case ".":
		default:
			named = true
		}
	}
	if !named {
		return errors.New("names no file")
	}
	return nil
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/freshness/ -run 'TestParsePackages|TestValidateFilename|TestAdopter_PackagesWithoutAnySHA256AdoptsIncomplete' -v`
Expected: PASS (`TestParsePackages_CapCountsStanzasWithoutSHA256` about 0.8 s: five bodies of a million stanzas).

Run: `go test ./internal/freshness/`
Expected: PASS.

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| `validateFilename` without its length bound | `TestParsePackages_BadFilename/too_long` (`want invalid-filename error, got <nil>`), `TestValidateFilename` (`validateFilename("aaaa…") = <nil>, want ok=false`) |
| the control-byte loop refuses NUL only | `TestParsePackages_BadFilename/control_byte`, `TestValidateFilename` (`validateFilename("a\x1fb.deb") = <nil>, want ok=false`) |
| `parsePackages` validates Filenames with `validateMemberPath` again | `TestParsePackages_KeepsDotAndPercentFilenamesLiterally` (`invalid filename "./cuda_12.6_amd64.deb": contains . segment`), `TestParsePackages_BadFilename/control_byte` and `/too_long` |
| a name of only `.` segments accepted (`_ = named`) | `TestParsePackages_BadFilename/only_dots`, `TestValidateFilename` (`validateFilename(".") = <nil>, want ok=false`) |
| `%` refused (`"?#%"`) | `TestParsePackages_KeepsDotAndPercentFilenamesLiterally` (`invalid filename "pool/p/pkg_1%3a2_amd64.deb": contains URL delimiter`), `TestValidateFilename` |
| R-G1 undone: the error fires whenever no row was kept (`len(out) == 0`) | `TestAdopter_PackagesWithoutAnySHA256AdoptsIncomplete` (`Run = adoption_parse_failed: …: packages: no stanzas with a Filename; want the SHA512-only index adopted (ruling R-G1)`), `TestParsePackages_OnlyStanzasWithoutSHA256Parse`, `TestParsePackages_CapCountsStanzasWithoutSHA256/gaps_alone_up_to_the_cap` |
| R-G1 overdone: a body with no Filename stanza parses (`if false`) | `TestParsePackages_NonEmptyButNoUsableStanzas` (`want no-stanzas error, got <nil>`), `TestParsePackages_NoFilenameStanzaStillFails` (`foreign file: parsePackages succeeded, want a parse error`) |
| an empty segment accepted | `TestParsePackages_BadFilename/empty_segment`, `TestValidateFilename` (`validateFilename("a//b.deb") = <nil>, want ok=false`) |
| a `..` segment accepted | `TestParsePackages_BadFilename/dotdot` and `/leading_dotdot`, `TestValidateFilename` (`validateFilename("a/../b.deb") = <nil>, want ok=false`) |
| a backslash accepted | `TestParsePackages_BadFilename/backslash`, `TestValidateFilename` (`validateFilename("a\\b.deb") = <nil>, want ok=false`) |

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/freshness/packages_parse.go internal/freshness/packages_parse_test.go internal/freshness/flat_adoption_test.go
git add internal/freshness/packages_parse.go internal/freshness/packages_parse_test.go internal/freshness/flat_adoption_test.go
git commit -m "Validate Packages Filenames with validateFilename (spec C5); adopt a SHA512-only Packages with incomplete coverage (ruling R-G1)"
```

(Record the mutation table in the commit message.)

### Task 4: C6 materialisation and coverage

Spec C6 "Materialisation": for a signed flat suite, `C = aptpath.FlatBase(suite)`, and each stanza with literal Filename `F` yields one `package_hash` row at `C + "/" + F` with `flat_filename = DotClean(F)`; `dists/` rows keep `''`. Coverage uses the `dists/` definition with the root as the Packages directory: `buildPackageHashes`' `/dists/` gate gives way to a flat-aware base, and a flat suite's Packages directory is its root (design note 6: a nested Packages a flat Release declares is neither rows nor coverage). Sources and pdiff hashing are not built for flat suites (their builders already return nothing without a `dists/` root; pinned). `cache.PackageHash` gains `FlatFilename`, which both writers — `CommitAdoption`'s Step 3 and reconcile's `insertPackageHashTx` — persist. Every real Filename shape of spec §3's table is materialised and pinned (k8s, CUDA through `/ /` and `./`, libnvidia-container, Jenkins `binary/` and `direct/`, Sublime `files/`, an epoch `%3a`).

**Files:**
- Create: `internal/cache/flat_package_hash_test.go`, `internal/freshness/flat_coverage_test.go`
- Modify: `internal/cache/types.go` (`PackageHash.FlatFilename`), `internal/cache/adoption.go` (`CommitAdoption`'s Step 3 insert), `internal/cache/reconcile.go` (`insertPackageHashTx`'s insert), `internal/freshness/adoption.go` (`buildPackageHashes`)

**Interfaces:**
- Consumes: `aptpath.IsFlatSuitePath`, `aptpath.FlatBase`, `aptpath.DotClean` (Task 1); `validateFilename` (Task 3: `./` Filenames parse); `openCache`, `seedRow`, `seedBlob`, `sk` (cache tests); `newAdoptionTestEnv`, `trackSuite`, `fakePackagesStanzas`, `gzipBytes`, `makeRelease`, `currentID`, `logRecords`, `lockedBuffer`, `testHex64` (freshness tests).
- Produces: `cache.PackageHash.FlatFilename string`; test helpers `adoptPackages`, `flatFilenameOf`, `pkgRow` (cache) and `packageHashRows`, `adoptFlat` (freshness), used by Tasks 5, 11 and 15.

- [ ] **Step 1: Write the failing tests**

Create `internal/cache/flat_package_hash_test.go`:

```go
package cache

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// adoptPackages commits an inline snapshot of key (which needs a row) whose
// anchor is a blob naming the suite and its first row (so a second call
// with other rows adopts new content), with phs as its package_hash rows
// (their snapshot ids filled in here) and coverage complete as given, and
// returns its id.
func adoptPackages(t *testing.T, c *Cache, key SuiteKey, phs []PackageHash, complete bool) int64 {
	t.Helper()
	ctx := context.Background()
	label := "InRelease of " + key.CanonicalScheme + "://" + key.CanonicalHost + key.SuitePath
	if len(phs) > 0 {
		label += " for " + phs[0].Path
	}
	anchor := seedBlob(t, c, label)
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{CanonicalScheme: key.CanonicalScheme,
		CanonicalHost: key.CanonicalHost, SuitePath: key.SuitePath, InReleaseHash: &anchor})
	if err != nil {
		t.Fatal(err)
	}
	for i := range phs {
		phs[i].SnapshotID = id
	}
	members := []SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: anchor, DeclaredSHA256: anchor}}
	if err := c.CommitAdoption(ctx, id, members, nil, phs, nil, complete); err != nil {
		t.Fatal(err)
	}
	return id
}

// flatFilenameOf reads the flat_filename of path's package_hash row in
// snapshot id.
func flatFilenameOf(t *testing.T, c *Cache, path string, id int64) string {
	t.Helper()
	var f string
	err := c.db.QueryRow(`SELECT flat_filename FROM package_hash WHERE path = ? AND snapshot_id = ?`, path, id).Scan(&f)
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("no package_hash row for %s in snapshot %d", path, id)
	}
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// pkgRow is a package_hash row of host h: path, declared hash, flat name.
func pkgRow(path, sha, flat string) PackageHash {
	return PackageHash{CanonicalScheme: "http", CanonicalHost: "h", Path: path, DeclaredSHA256: sha,
		PackageName: "p", Architecture: "amd64", Version: "1", FlatFilename: flat}
}

// Spec C6: both package_hash writers — CommitAdoption's Step 3 and
// reconcile's insertPackageHashTx — persist a flat row's flat_filename,
// and a dists row keeps "".
func TestPackageHash_FlatFilenamePersists(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	flat, dists := sk("h", "/repo"), sk("h", "/r/dists/x")
	seedRow(t, c, flat, 1)
	seedRow(t, c, dists, 1)
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	flatID := adoptPackages(t, c, flat, []PackageHash{pkgRow("/repo/./x_1_amd64.deb", a, "x_1_amd64.deb")}, true)
	distsID := adoptPackages(t, c, dists, []PackageHash{pkgRow("/r/pool/y_1_amd64.deb", b, "")}, true)
	if got := flatFilenameOf(t, c, "/repo/./x_1_amd64.deb", flatID); got != "x_1_amd64.deb" {
		t.Fatalf("CommitAdoption's flat row: flat_filename %q, want x_1_amd64.deb", got)
	}
	if got := flatFilenameOf(t, c, "/r/pool/y_1_amd64.deb", distsID); got != "" {
		t.Fatalf("CommitAdoption's dists row: flat_filename %q, want \"\"", got)
	}
	healed := pkgRow("/repo/amd64/z_1_amd64.deb", b, "amd64/z_1_amd64.deb")
	healed.SnapshotID = flatID
	if err := c.InsertReconciledMembers(ctx, flatID, nil, []PackageHash{healed}); err != nil {
		t.Fatal(err)
	}
	if got := flatFilenameOf(t, c, "/repo/amd64/z_1_amd64.deb", flatID); got != "amd64/z_1_amd64.deb" {
		t.Fatalf("insertPackageHashTx's flat row: flat_filename %q, want amd64/z_1_amd64.deb", got)
	}
}
```

Create `internal/freshness/flat_coverage_test.go`:

```go
package freshness

import (
	"context"
	"database/sql"
	"log/slog"
	"maps"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// packageHashRows reads snapshot id's package_hash rows: path →
// flat_filename.
func packageHashRows(t *testing.T, c *cache.Cache, id int64) map[string]string {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(c.Dir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT path, flat_filename FROM package_hash WHERE snapshot_id = ?`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var p, f string
		if err := rows.Scan(&p, &f); err != nil {
			t.Fatal(err)
		}
		out[p] = f
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// adoptFlat adopts suite (given a row here) from a Release declaring
// members (path → body), each served at the suite's URL, through env's
// adopter; it returns the current snapshot.
func adoptFlat(t *testing.T, env *adoptionTestEnv, suite SuiteRef, members map[string][]byte) *cache.SuiteSnapshot {
	t.Helper()
	ctx := context.Background()
	trackSuite(t, env.cache, suite)
	base := suite.CanonicalScheme + "://" + suite.CanonicalHost + suite.SuitePath + "/"
	for p, body := range members {
		env.fetcher.put(base+p, body)
	}
	rel, _ := makeRelease(members)
	if err := env.adopter.Run(ctx, suite, rel, "", ""); err != nil {
		t.Fatalf("Run(%s): %v", suite.SuitePath, err)
	}
	snap, err := env.cache.GetSuiteSnapshot(ctx, currentID(t, env.cache, suite))
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// Spec C6 materialisation, every real Filename shape of spec §3's table: a
// flat suite's stanza with literal Filename F gives the row
// FlatBase(suite) + "/" + F with flat_filename DotClean(F), and coverage
// is complete. The `deb <uri> ./` twin materialises at the same base as
// its suite (FlatBase strips the "/."). A dists suite's rows keep "".
func TestAdopter_MaterialisesFlatPackageRows(t *testing.T) {
	for _, c := range []struct {
		name, host, suite, filename, path, flat string
	}{
		{"pkgs.k8s.io", "pkgs.k8s.io", "/core:/stable:/v1.34/deb", "amd64/kubectl_1.34.1-1.1_amd64.deb",
			"/core:/stable:/v1.34/deb/amd64/kubectl_1.34.1-1.1_amd64.deb", "amd64/kubectl_1.34.1-1.1_amd64.deb"},
		{"NVIDIA CUDA", "developer.download.nvidia.com", "/compute/cuda/repos/ubuntu2204/x86_64", "./cuda-toolkit_12.6.0-1_amd64.deb",
			"/compute/cuda/repos/ubuntu2204/x86_64/./cuda-toolkit_12.6.0-1_amd64.deb", "cuda-toolkit_12.6.0-1_amd64.deb"},
		{"NVIDIA CUDA through ./", "developer.download.nvidia.com", "/compute/cuda/repos/ubuntu2204/x86_64/.", "./cuda-toolkit_12.6.0-1_amd64.deb",
			"/compute/cuda/repos/ubuntu2204/x86_64/./cuda-toolkit_12.6.0-1_amd64.deb", "cuda-toolkit_12.6.0-1_amd64.deb"},
		{"libnvidia-container", "nvidia.github.io", "/libnvidia-container/stable/deb/amd64", "./libnvidia-container1_1.17.0-1_amd64.deb",
			"/libnvidia-container/stable/deb/amd64/./libnvidia-container1_1.17.0-1_amd64.deb", "libnvidia-container1_1.17.0-1_amd64.deb"},
		{"Jenkins binary/", "pkg.jenkins.io", "/debian-stable/binary", "binary/jenkins_2.479.3_all.deb",
			"/debian-stable/binary/binary/jenkins_2.479.3_all.deb", "binary/jenkins_2.479.3_all.deb"},
		{"Jenkins direct/", "pkg.jenkins.io", "/debian-stable/binary", "direct/jenkins_2.479.3_all.deb",
			"/debian-stable/binary/direct/jenkins_2.479.3_all.deb", "direct/jenkins_2.479.3_all.deb"},
		{"Sublime files/", "download.sublimetext.com", "/apt/stable", "files/sublime-text_build-4180_amd64.deb",
			"/apt/stable/files/sublime-text_build-4180_amd64.deb", "files/sublime-text_build-4180_amd64.deb"},
		{"an epoch, %-encoded by the publisher", "h", "/repo", "pool/p/pkg_1%3a2.0_amd64.deb",
			"/repo/pool/p/pkg_1%3a2.0_amd64.deb", "pool/p/pkg_1%3a2.0_amd64.deb"},
		{"a dists suite", "archive.ubuntu.com", "/ubuntu/dists/noble", "pool/main/f/foo_1_amd64.deb",
			"/ubuntu/pool/main/f/foo_1_amd64.deb", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := newAdoptionTestEnv(t)
			suite := SuiteRef{CanonicalScheme: "https", CanonicalHost: c.host, SuitePath: c.suite}
			index := "Packages"
			if c.flat == "" {
				index = "main/binary-amd64/Packages"
			}
			pkgs := fakePackagesStanzas(map[string]string{c.filename: testHex64})
			snap := adoptFlat(t, env, suite, map[string][]byte{index: pkgs, index + ".gz": gzipBytes(pkgs)})
			if got, want := packageHashRows(t, env.cache, snap.SnapshotID), map[string]string{c.path: c.flat}; !maps.Equal(got, want) {
				t.Fatalf("package_hash rows = %v, want %v", got, want)
			}
			if !snap.PackageCoverageComplete {
				t.Fatal("package_coverage_complete = false, want true")
			}
		})
	}
}

// Spec C6 for a flat suite's other members (plan design note 6): only its
// root Packages is its Packages directory — a nested Packages a flat
// Release declares neither gives rows nor counts for coverage — and its
// Sources and pdiff Index build no rows (Sources and pdiff hashing stay
// dists-only). A flat Release with no root Packages at all has incomplete
// coverage (no_packages_members), and a root Packages whose stanzas all
// lack a SHA256 (ruling R-G1) adopts with incomplete coverage (no_sha256).
func TestAdopter_FlatCoverageReadsTheRootPackagesOnly(t *testing.T) {
	suite := SuiteRef{CanonicalScheme: "https", CanonicalHost: "h", SuitePath: "/repo"}
	pkgs := fakePackagesStanzas(map[string]string{"./a_1_amd64.deb": testHex64})
	nested := fakePackagesStanzas(map[string]string{"./nested_1_amd64.deb": strings.Repeat("b", 64)})
	sources := []byte("Package: a\nBinary: a\nVersion: 1\nDirectory: .\nChecksums-Sha256:\n " + testHex64 + " 10 a_1.dsc\n")
	pdiff := []byte("SHA256-Current: " + testHex64 + " 10\nSHA256-Patches:\n " + testHex64 + " 10 2026-10-20-0000.00\n")
	t.Run("root Packages only", func(t *testing.T) {
		env := newAdoptionTestEnv(t)
		snap := adoptFlat(t, env, suite, map[string][]byte{"Packages": pkgs, "sub/Packages": nested, "Sources": sources,
			"Packages.diff/Index": pdiff})
		if got, want := packageHashRows(t, env.cache, snap.SnapshotID), map[string]string{"/repo/./a_1_amd64.deb": "a_1_amd64.deb"}; !maps.Equal(got, want) {
			t.Fatalf("package_hash rows = %v, want only the root Packages' %v", got, want)
		}
		if !snap.PackageCoverageComplete {
			t.Fatal("package_coverage_complete = false, want true (a nested Packages is not the flat suite's)")
		}
	})
	t.Run("no root Packages", func(t *testing.T) {
		env := newAdoptionTestEnv(t)
		var logs lockedBuffer
		env.adopter.logger = slog.New(slog.NewJSONHandler(&logs, nil))
		snap := adoptFlat(t, env, suite, map[string][]byte{"sub/Packages": nested, "Sources": sources})
		if rows := packageHashRows(t, env.cache, snap.SnapshotID); len(rows) != 0 || snap.PackageCoverageComplete {
			t.Fatalf("rows %v, coverage %v; want none and incomplete", rows, snap.PackageCoverageComplete)
		}
		if recs := logRecords(t, &logs, "package_coverage_incomplete"); len(recs) != 1 || recs[0]["reason"] != "no_packages_members" {
			t.Fatalf("package_coverage_incomplete = %v, want reason no_packages_members", recs)
		}
	})
	t.Run("no stanza has a SHA256", func(t *testing.T) {
		env := newAdoptionTestEnv(t)
		var logs lockedBuffer
		env.adopter.logger = slog.New(slog.NewJSONHandler(&logs, nil))
		gaps := []byte("Package: a\nArchitecture: amd64\nVersion: 1\nFilename: ./a_1_amd64.deb\nSHA512: " + strings.Repeat("a", 128) + "\n")
		snap := adoptFlat(t, env, suite, map[string][]byte{"Packages": gaps})
		if rows := packageHashRows(t, env.cache, snap.SnapshotID); len(rows) != 0 || snap.PackageCoverageComplete {
			t.Fatalf("rows %v, coverage %v; want none and incomplete", rows, snap.PackageCoverageComplete)
		}
		if recs := logRecords(t, &logs, "package_coverage_incomplete"); len(recs) != 1 || recs[0]["reason"] != "no_sha256" {
			t.Fatalf("package_coverage_incomplete = %v, want reason no_sha256", recs)
		}
	})
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -gcflags=-e ./internal/cache/`
Expected: FAIL — `unknown field FlatFilename in struct literal of type PackageHash` (`flat_package_hash_test.go`).

Run: `go test ./internal/freshness/ -run 'TestAdopter_MaterialisesFlatPackageRows|TestAdopter_FlatCoverageReadsTheRootPackagesOnly'`
Expected: FAIL — every flat subtest (`package_hash rows = map[], want map[/core:/stable:/v1.34/deb/amd64/kubectl_1.34.1-1.1_amd64.deb:amd64/kubectl_1.34.1-1.1_amd64.deb]`, and so on: a non-`dists/` layout built no rows), and `no root Packages` and `no stanza has a SHA256` (`package_coverage_incomplete = [… reason:non_dists_layout …], want reason no_packages_members` / `no_sha256`). The `a dists suite` row passes.

- [ ] **Step 3: Implement**

In `internal/cache/types.go`, replace

```go
	// Version is the Debian Version: string for binary packages, used by
	// the version-aware retention mirror rule to rank distinct versions per
	// (PackageName, Architecture). Empty for source/pdiff/Contents rows and
	// for pre-v6 rows; the mirror rule only applies to non-empty versions.
	Version string
}
```

with

```go
	// Version is the Debian Version: string for binary packages, used by
	// the version-aware retention mirror rule to rank distinct versions per
	// (PackageName, Architecture). Empty for source/pdiff/Contents rows and
	// for pre-v6 rows; the mirror rule only applies to non-empty versions.
	Version string
	// FlatFilename is package_hash.flat_filename (spec C6): for a flat
	// suite's row, its stanza's Filename with every "." segment removed
	// (aptpath.DotClean), the key DeclaredHashesForPath's suffix fallback
	// looks a request up by; "" for a dists row, which the fallback never
	// reads (the partial index idx_package_hash_flat_filename leaves it
	// out).
	FlatFilename string
}
```

In `internal/cache/adoption.go`, replace

```go
			if _, err := tx.ExecContext(ctx, `
INSERT INTO package_hash (canonical_scheme, canonical_host, path,
                          declared_sha256, snapshot_id,
                          package_name, architecture, version)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				p.CanonicalScheme, p.CanonicalHost, p.Path,
				p.DeclaredSHA256, snapshotID,
				p.PackageName, p.Architecture, p.Version); err != nil {
```

with

```go
			if _, err := tx.ExecContext(ctx, `
INSERT INTO package_hash (canonical_scheme, canonical_host, path,
                          declared_sha256, snapshot_id,
                          package_name, architecture, version, flat_filename)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				p.CanonicalScheme, p.CanonicalHost, p.Path,
				p.DeclaredSHA256, snapshotID,
				p.PackageName, p.Architecture, p.Version, p.FlatFilename); err != nil {
```

In `internal/cache/reconcile.go`, replace

```go
	if _, err := tx.ExecContext(ctx, `
INSERT INTO package_hash (canonical_scheme, canonical_host, path,
                          declared_sha256, snapshot_id,
                          package_name, architecture, version)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		ph.CanonicalScheme, ph.CanonicalHost, ph.Path,
		ph.DeclaredSHA256, ph.SnapshotID,
		ph.PackageName, ph.Architecture, ph.Version); err != nil {
```

with

```go
	if _, err := tx.ExecContext(ctx, `
INSERT INTO package_hash (canonical_scheme, canonical_host, path,
                          declared_sha256, snapshot_id,
                          package_name, architecture, version, flat_filename)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ph.CanonicalScheme, ph.CanonicalHost, ph.Path,
		ph.DeclaredSHA256, ph.SnapshotID,
		ph.PackageName, ph.Architecture, ph.Version, ph.FlatFilename); err != nil {
```

In `internal/freshness/adoption.go`, replace

```go
// form is the snapshot's anchor form: the unsigned form gets no row and no
// coverage (spec C5 safeguard 5 — no package_hash row on any path:
// adoption here, reconcile through the same call), so it vouches no .deb.
func (a *Adopter) buildPackageHashes(suite SuiteRef, snapshotID int64, form cache.AnchorForm, allMembers, fetchedMembers []ReleaseMember) (packageHashBuildResult, error) {
	if form == cache.FormUnsigned {
		return packageHashBuildResult{}, nil
	}
	repoRoot, ok := repoRootFromSuitePath(suite.SuitePath)
	if !ok {
```

with

```go
// form is the snapshot's anchor form: the unsigned form gets no row and no
// coverage (spec C5 safeguard 5 — no package_hash row on any path:
// adoption here, reconcile through the same call), so it vouches no .deb.
//
// A flat suite is materialised by spec C6: its root Packages* members (a
// member path with no "/") are its Packages directory, and each stanza's
// literal Filename F gives the row FlatBase(suite) + "/" + F with
// flat_filename DotClean(F) — the exact row is a real path for `/ /` and
// `./` sources and a harmless phantom for `sub/` and `a/b/` ones, whose
// .debs DeclaredHashesForPath's suffix fallback finds by flat_filename.
//
// AIDEV-NOTE: the literal F is kept in the path (apt requests
// <uri>/./x.deb verbatim; validateFilename refused anything that could
// leave the base), and only a flat row carries a flat_filename: a dists
// row's "" keeps it out of the fallback's partial index. A flat Release's
// nested Packages (not an IndexTarget for a flat source, design note 3)
// counts neither as rows nor in coverage. Sources and pdiff hashing stay
// dists-only (spec C6): their builders return nothing without a dists
// repository root.
func (a *Adopter) buildPackageHashes(suite SuiteRef, snapshotID int64, form cache.AnchorForm, allMembers, fetchedMembers []ReleaseMember) (packageHashBuildResult, error) {
	if form == cache.FormUnsigned {
		return packageHashBuildResult{}, nil
	}
	flat := aptpath.IsFlatSuitePath(suite.SuitePath)
	repoRoot, ok := repoRootFromSuitePath(suite.SuitePath)
	if flat {
		repoRoot, ok = aptpath.FlatBase(suite.SuitePath)+"/", true
	}
	// packagesIndex reports whether member path p is one of the suite's
	// Packages indexes: any Packages* for a dists suite, a root one for a
	// flat suite.
	packagesIndex := func(p string, shape func(string) bool) bool {
		return shape(p) && (!flat || !strings.Contains(p, "/"))
	}
	if !ok {
```

In `internal/freshness/adoption.go`, replace

```go
	for _, m := range allMembers {
		if !isPackagesBasename(path.Base(m.Path)) {
			continue
		}
```

with

```go
	for _, m := range allMembers {
		if !packagesIndex(m.Path, func(p string) bool { return isPackagesBasename(path.Base(p)) }) {
			continue
		}
```

In `internal/freshness/adoption.go`, replace

```go
	for _, m := range fetchedMembers {
		if !isPackagesMember(m.Path) {
			continue
		}
		body, err := a.readPackagesBlob(m.Path, m.SHA256)
```

with

```go
	for _, m := range fetchedMembers {
		if !packagesIndex(m.Path, isPackagesMember) {
			continue
		}
		body, err := a.readPackagesBlob(m.Path, m.SHA256)
```

In `internal/freshness/adoption.go`, replace

```go
		rows = append(rows, cache.PackageHash{
			CanonicalScheme: suite.CanonicalScheme,
			CanonicalHost:   suite.CanonicalHost,
			Path:            debPath,
			DeclaredSHA256:  decl.sha256,
			SnapshotID:      snapshotID,
			PackageName:     decl.packageName,
			Architecture:    decl.architecture,
			Version:         decl.version,
		})
	}
```

with

```go
		row := cache.PackageHash{
			CanonicalScheme: suite.CanonicalScheme,
			CanonicalHost:   suite.CanonicalHost,
			Path:            debPath,
			DeclaredSHA256:  decl.sha256,
			SnapshotID:      snapshotID,
			PackageName:     decl.packageName,
			Architecture:    decl.architecture,
			Version:         decl.version,
		}
		if flat {
			row.FlatFilename = aptpath.DotClean(strings.TrimPrefix(debPath, repoRoot))
		}
		rows = append(rows, row)
	}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/cache/ ./internal/freshness/`
Expected: PASS.

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| `flat_filename` is the literal Filename (no `DotClean`) | `TestAdopter_MaterialisesFlatPackageRows/NVIDIA_CUDA` (`package_hash rows = map[…/x86_64/./cuda-toolkit_12.6.0-1_amd64.deb:./cuda-toolkit_12.6.0-1_amd64.deb], want map[…:cuda-toolkit_12.6.0-1_amd64.deb]`), `/libnvidia-container`, `TestAdopter_FlatCoverageReadsTheRootPackagesOnly/root_Packages_only` |
| a `dists/` row gets a `flat_filename` (`if true`) | `TestAdopter_MaterialisesFlatPackageRows/a_dists_suite` (`package_hash rows = map[/ubuntu/pool/main/f/foo_1_amd64.deb:pool/main/f/foo_1_amd64.deb], want map[/ubuntu/pool/main/f/foo_1_amd64.deb:]`) |
| the base is the suite path, not `FlatBase` | `TestAdopter_MaterialisesFlatPackageRows/NVIDIA_CUDA_through_./` (`package_hash rows = map[…/x86_64/././cuda-toolkit_12.6.0-1_amd64.deb:…], want map[…/x86_64/./cuda-toolkit_12.6.0-1_amd64.deb:…]`) |
| a flat Release's nested Packages counts (`return shape(p)`) | `TestAdopter_FlatCoverageReadsTheRootPackagesOnly/root_Packages_only` (`package_hash rows = map[/repo/./a_1_amd64.deb:a_1_amd64.deb /repo/./nested_1_amd64.deb:nested_1_amd64.deb], want only the root Packages' …`), `/no_root_Packages` (`rows map[/repo/./nested_1_amd64.deb:…], coverage true; want none and incomplete`) |
| `CommitAdoption`'s insert passes `""` for `flat_filename` | `TestPackageHash_FlatFilenamePersists` (`CommitAdoption's flat row: flat_filename "", want x_1_amd64.deb`), every flat subtest of `TestAdopter_MaterialisesFlatPackageRows` |
| `insertPackageHashTx` passes `""` | `TestPackageHash_FlatFilenamePersists` (`insertPackageHashTx's flat row: flat_filename "", want amd64/z_1_amd64.deb`) |
| a flat row's path is dot-cleaned | `TestAdopter_MaterialisesFlatPackageRows/NVIDIA_CUDA` (`package_hash rows = map[…/x86_64/cuda-toolkit_12.6.0-1_amd64.deb:…], want map[…/x86_64/./cuda-toolkit_12.6.0-1_amd64.deb:…]`), `/libnvidia-container`, `TestAdopter_FlatCoverageReadsTheRootPackagesOnly/root_Packages_only` |

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/cache/types.go internal/cache/adoption.go internal/cache/reconcile.go internal/cache/flat_package_hash_test.go internal/freshness/adoption.go internal/freshness/flat_coverage_test.go
git add internal/cache/types.go internal/cache/adoption.go internal/cache/reconcile.go internal/cache/flat_package_hash_test.go internal/freshness/adoption.go internal/freshness/flat_coverage_test.go
git commit -m "Materialise flat suites' package_hash rows at FlatBase with flat_filename (spec C6)"
```

(Record the mutation table in the commit message.)

### Task 5: C6 lookup: the suffix fallback in `DeclaredHashesForPath`

Spec C6 "Lookup": `DeclaredHashesForPath` keeps its exact lookup (step 1) and, only when that finds no row, runs the suffix fallback (step 2, design note 7): `P' = DotClean(P)`; stop on `..`, an empty segment or more than 32 segments; for every split `B + "/" + F` query the current signed snapshots' rows with `flat_filename <> '' AND flat_filename IN (F…)` on `(scheme, host)` — the `<> ''` term is what makes SQLite use the partial index, and an `EXPLAIN QUERY PLAN` assertion pins it — keep a row iff `B` is an ancestor-or-self of its suite's `FlatBase`, and return those with the longest `B`. The dispatch (0 → strict mode, 1 → validate, ≥ 2 → `package_hash_conflict`) is unchanged and the row still names its suite, so a vouched flat `.deb` touches its suite (spec C3) with no handler change. Pinned with every real Filename shape, the nested-base tiebreak and spec §7's negatives (`..`, `//`, a sibling `/flat2`, a prefix `/fla`, 33 segments), and unsigned and displaced snapshots.

**Files:**
- Create: `internal/cache/flat_lookup_test.go`
- Modify: `internal/cache/adoption.go` (`DeclaredHashesForPath` split into `declaredHashesExact` and `declaredHashesBySuffix`; `maxFallbackSegments`, `flatSplits`, `declaredBySuffixSQL`; the `strings` import)

**Interfaces:**
- Consumes: `aptpath.DotClean`, `aptpath.FlatBase` (Task 1); `adoptPackages`, `pkgRow` (Task 4); `openCache`, `seedRow`, `seedBlob`, `sk` (cache tests).
- Produces: `(*cache.Cache).DeclaredHashesForPath` with the fallback (same signature); unexported `declaredHashesExact`, `declaredHashesBySuffix`, `flatSplits(path string) (bases, names []string)`, `declaredBySuffixSQL(n int) string`, `maxFallbackSegments`.

- [ ] **Step 1: Write the failing tests**

Create `internal/cache/flat_lookup_test.go`:

```go
package cache

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// hashOf is a 64-hex declared hash made of one repeated digit.
func hashOf(d byte) string { return strings.Repeat(string(d), 64) }

// declared runs DeclaredHashesForPath on host h and returns the rows'
// "hash@suite" pairs, sorted.
func declared(t *testing.T, c *Cache, path string) []string {
	t.Helper()
	rows, err := c.DeclaredHashesForPath(context.Background(), "http", "h", path)
	if err != nil {
		t.Fatalf("DeclaredHashesForPath(%s): %v", path, err)
	}
	var out []string
	for _, r := range rows {
		out = append(out, r.DeclaredSHA256[:1]+"@"+r.SuitePath)
	}
	slices.Sort(out)
	return out
}

// Spec C6's lookup over every real Filename shape (spec §3's table): the
// exact row (k8s, CUDA's "./", a `deb <uri> ./` twin's row at its
// FlatBase), and, only when no exact row exists, the suffix fallback —
// CUDA's dot-cleaned form, Jenkins' binary/ and direct/ (the parent base),
// Sublime's files/ (the host-root base), a twin's "./" path after
// DotClean. The fallback stops at "..", an empty segment and more than 32
// segments, and never crosses to a sibling directory or an unrelated base.
func TestDeclaredHashesForPath_FlatShapes(t *testing.T) {
	c := openCache(t)
	adopt := func(suite string, rows ...PackageHash) {
		seedRow(t, c, sk("h", suite), 1)
		adoptPackages(t, c, sk("h", suite), rows, true)
	}
	adopt("/core:/stable:/v1.34/deb", pkgRow("/core:/stable:/v1.34/deb/amd64/kubectl_1.34_amd64.deb", hashOf('1'), "amd64/kubectl_1.34_amd64.deb"))
	adopt("/cuda/x86_64", pkgRow("/cuda/x86_64/./cuda_12.6_amd64.deb", hashOf('2'), "cuda_12.6_amd64.deb"))
	adopt("/debian-stable/binary",
		pkgRow("/debian-stable/binary/binary/jenkins_2.4_all.deb", hashOf('3'), "binary/jenkins_2.4_all.deb"),
		pkgRow("/debian-stable/binary/direct/jenkins_2.5_all.deb", hashOf('4'), "direct/jenkins_2.5_all.deb"))
	adopt("/apt/stable", pkgRow("/apt/stable/files/sublime_4180_amd64.deb", hashOf('5'), "files/sublime_4180_amd64.deb"))
	adopt("/flat/.", pkgRow("/flat/amd64/x_1_amd64.deb", hashOf('6'), "amd64/x_1_amd64.deb"))
	deep := "/" + strings.TrimSuffix(strings.Repeat("d/", 30), "/") // a 30-segment flat suite
	adopt(deep, pkgRow(deep+"/x/y_1_amd64.deb", hashOf('7'), "x/y_1_amd64.deb"))
	deeper := deep + "/e" // 31 segments
	adopt(deeper, pkgRow(deeper+"/x/z_1_amd64.deb", hashOf('8'), "x/z_1_amd64.deb"))
	for _, tc := range []struct {
		name, path string
		want       []string
	}{
		{"k8s, exact", "/core:/stable:/v1.34/deb/amd64/kubectl_1.34_amd64.deb", []string{"1@/core:/stable:/v1.34/deb"}},
		{"CUDA ./, exact", "/cuda/x86_64/./cuda_12.6_amd64.deb", []string{"2@/cuda/x86_64"}},
		{"CUDA dot-cleaned, fallback", "/cuda/x86_64/cuda_12.6_amd64.deb", []string{"2@/cuda/x86_64"}},
		{"Jenkins binary/, parent base", "/debian-stable/binary/jenkins_2.4_all.deb", []string{"3@/debian-stable/binary"}},
		{"Jenkins direct/, parent base", "/debian-stable/direct/jenkins_2.5_all.deb", []string{"4@/debian-stable/binary"}},
		{"Sublime files/, host-root base", "/files/sublime_4180_amd64.deb", []string{"5@/apt/stable"}},
		{"twin, exact row at FlatBase", "/flat/amd64/x_1_amd64.deb", []string{"6@/flat/."}},
		{"twin, ./ after DotClean", "/flat/./amd64/x_1_amd64.deb", []string{"6@/flat/."}},
		{"32 segments after DotClean", deep + "/./x/y_1_amd64.deb", []string{"7@" + deep}},
		{"33 segments after DotClean", deeper + "/./x/z_1_amd64.deb", nil},
		{"the fallback stops at ..", "/flat/../flat/amd64/x_1_amd64.deb", nil},
		{"the fallback stops at an empty segment", "/flat//amd64/x_1_amd64.deb", nil},
		{"a sibling directory", "/flat2/amd64/x_1_amd64.deb", nil},
		{"a prefix of the base's last segment", "/fla/amd64/x_1_amd64.deb", nil},
		{"an unrelated base", "/other/files/sublime_4180_amd64.deb", nil},
		{"a dists suite's pool", "/ubuntu/pool/main/f/foo_1_amd64.deb", nil},
	} {
		if got := declared(t, c, tc.path); !slices.Equal(got, tc.want) {
			t.Errorf("%s: DeclaredHashesForPath(%s) = %v, want %v", tc.name, tc.path, got, tc.want)
		}
	}
}

// Spec C6 step 2's tiebreak: among the rows the fallback keeps, those with
// the longest B — the most specific base — win. "/a/./b/c/x.deb" is
// <uri>/<Filename> both for suite /a (Filename b/c/x.deb, B /a) and for
// suite /a/b/c/sub (a `sub/` source of <uri> /a/b/c: Filename x.deb, B
// /a/b/c); the nested base wins, so the two declarations never conflict.
// The exact row still wins over any fallback.
func TestDeclaredHashesForPath_TheMostSpecificBaseWins(t *testing.T) {
	c := openCache(t)
	for suite, row := range map[string]PackageHash{
		"/a":         pkgRow("/a/b/c/x_1_amd64.deb", hashOf('a'), "b/c/x_1_amd64.deb"),
		"/a/b/c/sub": pkgRow("/a/b/c/sub/x_1_amd64.deb", hashOf('b'), "x_1_amd64.deb"),
	} {
		seedRow(t, c, sk("h", suite), 1)
		adoptPackages(t, c, sk("h", suite), []PackageHash{row}, true)
	}
	if got, want := declared(t, c, "/a/./b/c/x_1_amd64.deb"), []string{"b@/a/b/c/sub"}; !slices.Equal(got, want) {
		t.Fatalf("fallback = %v, want the nested base's row %v", got, want)
	}
	if got, want := declared(t, c, "/a/b/c/x_1_amd64.deb"), []string{"a@/a"}; !slices.Equal(got, want) {
		t.Fatalf("exact = %v, want /a's exact row %v (no fallback)", got, want)
	}
}

// Spec C6 "current signed non-retired snapshots only": the fallback sees
// neither an unsigned snapshot's rows (spec C5 safeguard 5) nor a
// displaced snapshot's.
func TestDeclaredHashesForPath_TheFallbackReadsCurrentSignedSnapshotsOnly(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	unsigned, displaced := sk("h", "/unsigned"), sk("h", "/displaced")
	seedRow(t, c, unsigned, 1)
	seedRow(t, c, displaced, 1)
	rel := seedBlob(t, c, "an unsigned Release")
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h",
		SuitePath: unsigned.SuitePath, InReleaseHash: &rel, Form: FormUnsigned})
	if err != nil {
		t.Fatal(err)
	}
	row := pkgRow("/unsigned/./u_1_amd64.deb", hashOf('c'), "u_1_amd64.deb")
	row.SnapshotID = id
	if err := c.CommitAdoption(ctx, id, []SnapshotMember{{SnapshotID: id, Path: "Release", BlobHash: rel, DeclaredSHA256: rel}},
		nil, []PackageHash{row}, nil, true); err != nil {
		t.Fatal(err)
	}
	adoptPackages(t, c, displaced, []PackageHash{pkgRow("/displaced/./d_1_amd64.deb", hashOf('d'), "d_1_amd64.deb")}, true)
	adoptPackages(t, c, displaced, nil, true) // a newer snapshot displaces it
	for _, p := range []string{"/unsigned/u_1_amd64.deb", "/displaced/d_1_amd64.deb"} {
		if got := declared(t, c, p); got != nil {
			t.Errorf("DeclaredHashesForPath(%s) = %v, want nothing", p, got)
		}
	}
}

// AIDEV-NOTE pin: the fallback's query uses the partial index over
// flat_filename (spec C6: "an EXPLAIN QUERY PLAN assertion pins it").
func TestDeclaredHashesForPath_TheFallbackUsesThePartialIndex(t *testing.T) {
	c := openCache(t)
	rows, err := c.db.Query(`EXPLAIN QUERY PLAN `+declaredBySuffixSQL(3), "http", "h", "a/b.deb", "b.deb", "x/a/b.deb")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plan, "\n"), "USING INDEX idx_package_hash_flat_filename") {
		t.Fatalf("query plan does not use idx_package_hash_flat_filename:\n%s", strings.Join(plan, "\n"))
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -gcflags=-e ./internal/cache/`
Expected: FAIL — `undefined: declaredBySuffixSQL` (`flat_lookup_test.go`).

- [ ] **Step 3: Implement**

In `internal/cache/adoption.go`, replace

```go
import (
	"context"
	"database/sql"
	"errors"
	"fmt"
```

with

```go
import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
```

In `internal/cache/adoption.go`, replace

```go
// AIDEV-NOTE: this is the shared helper between §6.1 (hit-path validation)
// and §6.2 (miss-path validation). Both call sites need declared+snapshot
// pairs (the snapshot id is required for fail-closed conflict logging).
// The DISTINCT clause is over (declared_sha256, snapshot_id), so two
// snapshots that *agree* on a hash still produce two rows — one per
// snapshot. The handler-side helper distinctDeclared collapses those
// duplicate hashes when classifying "0 / 1 / 2+ distinct hashes" for
// the §6.1 hit-path dispatch; the conflict log surface shows every row
// (snapshot id included) so an operator can trace which adoptions
// disagreed.
func (c *Cache) DeclaredHashesForPath(ctx context.Context, scheme, host, path string) ([]DeclaredHash, error) {
	// AIDEV-NOTE: spec C5 safeguard 5 — an unsigned snapshot never vouches
	// a .deb. Adoption builds no package_hash rows for one; the form
	// filter keeps any that exist out (defence in depth).
	const q = `
```

with

```go
// AIDEV-NOTE: this is the shared helper between §6.1 (hit-path validation)
// and §6.2 (miss-path validation). Both call sites need declared+snapshot
// pairs (the snapshot id is required for fail-closed conflict logging).
// The DISTINCT clause is over (declared_sha256, snapshot_id), so two
// snapshots that *agree* on a hash still produce two rows — one per
// snapshot. The handler-side helper distinctDeclared collapses those
// duplicate hashes when classifying "0 / 1 / 2+ distinct hashes" for
// the §6.1 hit-path dispatch; the conflict log surface shows every row
// (snapshot id included) so an operator can trace which adoptions
// disagreed.
//
// Spec C6: the exact lookup runs first, and only when it finds no row the
// suffix fallback (declaredHashesBySuffix) looks the path up by a flat
// suite's flat_filename — apt requests a flat repository's .deb at
// <uri>/<Filename>, <uri> any ancestor-or-self of the Release directory.
//
// AIDEV-NOTE: the fallback is a constraint lookup, not a grant: a row it
// finds only lets bytes through that hash to a signed declaration, and it
// never runs when an exact row exists (so a dists suite's vouching is
// unchanged). Its cost — one partial-index query — is paid by every
// .deb no exact row vouches, which includes every .deb of a host with no
// flat suite (an empty index range per name).
func (c *Cache) DeclaredHashesForPath(ctx context.Context, scheme, host, path string) ([]DeclaredHash, error) {
	out, err := c.declaredHashesExact(ctx, scheme, host, path)
	if err != nil || len(out) > 0 {
		return out, err
	}
	return c.declaredHashesBySuffix(ctx, scheme, host, path)
}

// declaredHashesExact is DeclaredHashesForPath's exact lookup (spec C6
// step 1): the rows whose path is path.
func (c *Cache) declaredHashesExact(ctx context.Context, scheme, host, path string) ([]DeclaredHash, error) {
	// AIDEV-NOTE: spec C5 safeguard 5 — an unsigned snapshot never vouches
	// a .deb. Adoption builds no package_hash rows for one; the form
	// filter keeps any that exist out (defence in depth).
	const q = `
```

In `internal/cache/adoption.go`, replace

```go
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("DeclaredHashesForPath iter: %w", err)
	}
	return out, nil
}
```

with

```go
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("DeclaredHashesForPath iter: %w", err)
	}
	return out, nil
}

// maxFallbackSegments bounds spec C6's suffix fallback: a dot-cleaned
// request path of more segments is never looked up by suffix (a constant;
// it also bounds the fallback query's IN list).
const maxFallbackSegments = 32

// flatSplits returns spec C6's splits of a request path for the suffix
// fallback: P' = aptpath.DotClean(path) cut as B + "/" + F at every
// segment boundary, from the host root (B = "") to the parent of the
// last segment, as parallel slices. It returns nothing when P' is not
// absolute, has an empty or ".." segment, or has more than
// maxFallbackSegments segments (a ".." could step a request outside the
// base its row is kept for).
func flatSplits(path string) (bases, names []string) {
	p := aptpath.DotClean(path)
	if !strings.HasPrefix(p, "/") {
		return nil, nil
	}
	segs := strings.Split(p[1:], "/")
	if len(segs) > maxFallbackSegments {
		return nil, nil
	}
	for _, s := range segs {
		if s == "" || s == ".." {
			return nil, nil
		}
	}
	for i := range segs {
		b := ""
		if i > 0 {
			b = "/" + strings.Join(segs[:i], "/")
		}
		bases = append(bases, b)
		names = append(names, strings.Join(segs[i:], "/"))
	}
	return bases, names
}

// declaredBySuffixSQL is the suffix fallback's query for n names: the
// current signed snapshots' flat rows of (scheme, host) whose
// flat_filename is one of them, with the row's suite path.
//
// AIDEV-NOTE: the term requiring a non-empty flat_filename is what lets
// SQLite use the partial index idx_package_hash_flat_filename (whose WHERE
// is the same term); without it every lookup scans the host's rows.
// TestDeclaredHashesForPath_TheFallbackUsesThePartialIndex pins the plan.
func declaredBySuffixSQL(n int) string {
	return `
SELECT DISTINCT p.declared_sha256, p.snapshot_id, p.package_name, sf.suite_path, p.flat_filename
  FROM package_hash p
  JOIN suite_freshness sf ON sf.current_snapshot_id = p.snapshot_id
  JOIN suite_snapshot ss  ON ss.snapshot_id = p.snapshot_id
 WHERE p.canonical_scheme = ?
   AND p.canonical_host   = ?
   AND p.flat_filename   <> ''
   AND p.flat_filename IN (` + strings.TrimSuffix(strings.Repeat("?, ", n), ", ") + `)
   AND ss.anchor_form    <> 'unsigned'`
}

// declaredHashesBySuffix is spec C6's suffix fallback (step 2): for every
// split B + "/" + F of the dot-cleaned path (flatSplits), the current
// signed snapshots' rows whose flat_filename is F, kept iff B is an
// ancestor-or-self of the row's suite's FlatBase; of the kept rows, those
// with the longest B — the most specific base — are returned. A path
// flatSplits refuses has no fallback.
func (c *Cache) declaredHashesBySuffix(ctx context.Context, scheme, host, path string) ([]DeclaredHash, error) {
	bases, names := flatSplits(path)
	if len(names) == 0 {
		return nil, nil
	}
	baseOf := make(map[string]string, len(names))
	args := make([]any, 0, len(names)+2)
	args = append(args, scheme, host)
	for i, n := range names {
		baseOf[n] = bases[i]
		args = append(args, n)
	}
	rows, err := c.db.QueryContext(ctx, declaredBySuffixSQL(len(names)), args...)
	if err != nil {
		return nil, fmt.Errorf("DeclaredHashesForPath fallback: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var (
		out  []DeclaredHash
		best = -1
	)
	for rows.Next() {
		var (
			d    DeclaredHash
			name string
		)
		if err := rows.Scan(&d.DeclaredSHA256, &d.SnapshotID, &d.PackageName, &d.SuitePath, &name); err != nil {
			return nil, fmt.Errorf("DeclaredHashesForPath fallback scan: %w", err)
		}
		b, ok := baseOf[name]
		if !ok || !strings.HasPrefix(aptpath.FlatBase(d.SuitePath)+"/", b+"/") {
			continue
		}
		switch {
		case len(b) > best:
			best, out = len(b), []DeclaredHash{d}
		case len(b) == best:
			out = append(out, d)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("DeclaredHashesForPath fallback iter: %w", err)
	}
	return out, nil
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/cache/ -run 'TestDeclaredHashesForPath' -v`
Expected: PASS (the three existing `TestDeclaredHashesForPath_*` tests too: their paths have exact rows or none at all).

Run: `go test ./internal/cache/ ./internal/handler/ ./internal/freshness/`
Expected: PASS.

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| the fallback also runs beside an exact row (its rows appended) | `TestDeclaredHashesForPath_FlatShapes` (`k8s, exact: … = [1@/core:/stable:/v1.34/deb 1@/core:/stable:/v1.34/deb], want [1@/core:/stable:/v1.34/deb]`), `TestDeclaredHashesForPath_TheMostSpecificBaseWins` (`exact = [a@/a b@/a/b/c/sub], want /a's exact row [a@/a] (no fallback)`) |
| no ancestor test | `TestDeclaredHashesForPath_FlatShapes` (`a sibling directory: DeclaredHashesForPath(/flat2/amd64/x_1_amd64.deb) = [6@/flat/.], want []`) |
| the ancestor test without its `/` separators | `TestDeclaredHashesForPath_FlatShapes` (`a prefix of the base's last segment: DeclaredHashesForPath(/fla/amd64/x_1_amd64.deb) = [6@/flat/.], want []`) |
| every kept row returned (no longest-`B` rule) | `TestDeclaredHashesForPath_TheMostSpecificBaseWins` (`fallback = [a@/a b@/a/b/c/sub], want the nested base's row [b@/a/b/c/sub]`) |
| the shortest `B` wins | `TestDeclaredHashesForPath_TheMostSpecificBaseWins` (`fallback = [a@/a], want …`) |
| the query without its non-empty `flat_filename` term | `TestDeclaredHashesForPath_TheFallbackUsesThePartialIndex` (`query plan does not use idx_package_hash_flat_filename`) |
| `maxFallbackSegments = 33` | `TestDeclaredHashesForPath_FlatShapes` (`33 segments after DotClean: … = [8@/d/…/e], want []`) |
| the query without its `anchor_form` filter | `TestDeclaredHashesForPath_TheFallbackReadsCurrentSignedSnapshotsOnly` (`DeclaredHashesForPath(/unsigned/u_1_amd64.deb) = [c@/unsigned], want nothing`) |
| the query joins every snapshot of a tracked suite, not the current one | `TestDeclaredHashesForPath_TheFallbackReadsCurrentSignedSnapshotsOnly` (`DeclaredHashesForPath(/displaced/d_1_amd64.deb) = [d@/displaced], want nothing`) |
| no `DotClean` before the splits | `TestDeclaredHashesForPath_FlatShapes` (`twin, ./ after DotClean: … = [], want [6@/flat/.]`), `TestDeclaredHashesForPath_TheMostSpecificBaseWins` |

Equivalent mutants (recorded, not failures): `flatSplits` without its `..` stop, or without its empty-segment stop — a `B` holding `..` or an empty segment is never an ancestor of a canonical `FlatBase`, and an `F` holding one never equals a `flat_filename` `validateFilename` let through, so both stops are defence in depth that spec C6 names; the ancestor test against the suite path instead of `FlatBase` — `B` never holds a `.` segment (`DotClean`), so it is an ancestor of `/flat` exactly when it is one of `/flat/.`.

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/cache/adoption.go internal/cache/flat_lookup_test.go
git add internal/cache/adoption.go internal/cache/flat_lookup_test.go
git commit -m "Look flat .debs up by Filename suffix when no exact row vouches them (spec C6)"
```

(Record the mutation table in the commit message.)

### Task 6: The handler's layout-aware alert, flat arch labels and `flat_host_root_unsupported`

Spec C5: "the handler's `snapshot_index_target_404` alert uses the same layout-aware rule" — `isIndexTargetPath` takes the suite path, a flat suite's root `Packages*`/`Sources*` are its IndexTargets (never its pdiff Index), and a root `Packages` under a `dists/` suite is none (parity item 2's handler rows). The alert's architecture label for a flat index is `all` (`Packages*`) or `source` (`Sources*`) through `indexTargetArch`, where `archFromPath` gave `""` and the metric bucketed it as `other` (the flat arch labels, design note 4). Spec C2: a flat repository at the host root gets no suite and is logged by a globally rate-limited WARN `flat_host_root_unsupported` (design note 2), and is otherwise served as before. The three regexes' shared AIDEV-NOTE names the flat alternatives and the layout rule. No handler code is added for flat suite rows, touches or onboarding (ruling R-D3): Task 7 tests them. This task changes `handler.go` by hand; its open deferred minors are Task 8's.

**Files:**
- Create: `internal/handler/flat_root.go`, `internal/handler/flat_root_test.go`
- Modify: `internal/handler/handler.go` (the `Handler.flatRoot` field, `ServeHTTP`'s host-root check, `trySnapshotHit`'s alert, `flatIndexTargetPathRE`, `isIndexTargetPath`, `indexTargetArch`), `internal/handler/path_class_test.go`

**Interfaces:**
- Consumes: `aptpath.IsFlatSuitePath`, `aptpath.IsFlatHostRootPath` (Task 1); `newTestHandlerWithServe`, `commitInlineSnapshot`, `writeBlob`, `splitURL`, `newProxyReqHostPort`, `strictLogBuf`, `recFetcher`, `rowInRelease`, `simtest.OpenCache`, `simtest.ClearsignedFixture` (handler tests).
- Produces: `isIndexTargetPath(suitePath, p string) bool`, `indexTargetArch(suitePath, p string) string`, `(*Handler).noteFlatHostRoot(req *proxy.Request)`, `flatRootWarn`, `flatRootWarnEvery`; test helper `metricValue(t, series string) float64` (used by Tasks 10, 11).

- [ ] **Step 1: Write the failing tests**

In `internal/handler/path_class_test.go`, replace

```go
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isIndexTargetPath(tc.path); got != tc.want {
				t.Errorf("isIndexTargetPath(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}
```

with

```go
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isIndexTargetPath("/ubuntu/dists/noble", tc.path); got != tc.want {
				t.Errorf("isIndexTargetPath(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// PR #8 parity item 2, the handler's isIndexTargetPath (the
// snapshot_index_target_404 alert): a flat suite's suite-relative root
// Packages, Packages.gz and Sources.xz are IndexTargets; its InRelease, a
// .deb under it and its pdiff Index are not. Under a dists suite none of
// the root paths is (spec C5's layout rule, as on the adoption side).
func TestIsIndexTargetPath_LayoutAware(t *testing.T) {
	targets := []string{"Packages", "Packages.gz", "Sources.xz"}
	never := []string{"InRelease", "amd64/kubelet_1.34.1-1.1_amd64.deb", "Packages.diff/Index", "main/Packages.gz"}
	for _, l := range []struct {
		suite string
		flat  bool
	}{{"/core:/stable:/v1.34/deb", true}, {"/core:/stable:/v1.34/deb/.", true}, {"/ubuntu/dists/noble", false}} {
		for _, p := range targets {
			if got := isIndexTargetPath(l.suite, p); got != l.flat {
				t.Errorf("isIndexTargetPath(%q, %q) = %v, want %v", l.suite, p, got, l.flat)
			}
		}
		for _, p := range never {
			if isIndexTargetPath(l.suite, p) {
				t.Errorf("isIndexTargetPath(%q, %q) = true, want false", l.suite, p)
			}
		}
	}
}

// The flat arch labels of plan design note 4 on the handler side: a flat
// suite's root Packages* is "all" and its root Sources* "source"; under a
// dists suite the label is archFromPath's.
func TestIndexTargetArch(t *testing.T) {
	for _, tc := range []struct {
		suite, path, want string
	}{
		{"/flat", "Packages", "all"},
		{"/flat", "Packages.zst", "all"},
		{"/flat/.", "Packages.gz", "all"},
		{"/flat", "Sources.xz", "source"},
		{"/ubuntu/dists/noble", "main/binary-arm64/Packages.gz", "arm64"},
		{"/ubuntu/dists/noble", "main/source/Sources.gz", "source"},
		{"/ubuntu/dists/noble", "Packages.gz", ""},
	} {
		if got := indexTargetArch(tc.suite, tc.path); got != tc.want {
			t.Errorf("indexTargetArch(%q, %q) = %q, want %q", tc.suite, tc.path, got, tc.want)
		}
	}
}
```

Create `internal/handler/flat_root_test.go`:

```go
package handler

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// metricValue returns the value of the exposition series line, the
// metric name with its labels exactly as metrics.Default renders it
// (e.g. `acu_serve_snapshot_index_target_404_total{architecture="all"}`);
// 0 when the series does not exist yet. The registry is package-global, so
// callers compare before and after.
func metricValue(t *testing.T, series string) float64 {
	t.Helper()
	var buf bytes.Buffer
	metrics.Default.Render(&buf)
	for _, line := range strings.Split(buf.String(), "\n") {
		if v, ok := strings.CutPrefix(line, series+" "); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil {
				t.Fatalf("parse %q: %v", line, err)
			}
			return f
		}
	}
	return 0
}

// Spec C2 (plan design note 2): a flat repository at the host root —
// `deb https://h/ /` and `deb https://h ./` — gets no suite: its files are
// served as before (Phase 1) and the handler logs WARN
// flat_host_root_unsupported at most once a minute process-wide, each line
// counting the requests its minute suppressed. No row is created.
func TestServeHTTP_FlatHostRootIsUnsupportedAndSaysSo(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parser, err := proxy.New(nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		c := simtest.OpenCache(t)
		logs := &strictLogBuf{}
		h, err := New(Config{Parser: parser, Cache: c, Fetch: &recFetcher{body: rowInRelease},
			Logger: slog.New(slog.NewJSONHandler(logs, nil)), HostLimiter: hostsem.New(4), AdoptionEnabled: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(h.Close)
		get := func(path string) {
			t.Helper()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://root.example"+path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s = %d, want 200 (served as before)", path, rec.Code)
			}
		}
		get("/InRelease")
		get("/./InRelease")
		get("/Packages.gz")
		get("/repo/InRelease") // a flat suite: no line
		recs := logs.find("flat_host_root_unsupported")
		if len(recs) != 1 || recs[0]["level"] != "WARN" || recs[0]["path"] != "/InRelease" ||
			recs[0]["canonical_host"] != "root.example" || recs[0]["suppressed"] != float64(0) {
			t.Fatalf("flat_host_root_unsupported = %v, want one WARN for /InRelease, suppressed 0", recs)
		}
		time.Sleep(time.Minute)
		get("/./Packages.xz")
		recs = logs.find("flat_host_root_unsupported")
		if len(recs) != 2 || recs[1]["path"] != "/./Packages.xz" || recs[1]["suppressed"] != float64(2) {
			t.Fatalf("flat_host_root_unsupported = %v, want a second line a minute later, suppressed 2", recs)
		}
		get("/Release")
		time.Sleep(time.Minute)
		get("/InRelease")
		if recs = logs.find("flat_host_root_unsupported"); len(recs) != 3 || recs[2]["suppressed"] != float64(1) {
			t.Fatalf("flat_host_root_unsupported = %v, want a third line counting only its own minute (suppressed 1)", recs)
		}
		all, err := c.ListSuites(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 1 || all[0].SuitePath != "/repo" {
			t.Fatalf("suite rows = %+v, want only /repo's", all)
		}
	})
}

// Spec C5 on the handler side: the snapshot_index_target_404 alert is
// layout-aware. A flat suite's root Packages* or Sources* missing from its
// snapshot raises it, labelled "all" or "source" (the flat arch labels,
// plan design note 4), as does its `./` twin's; under a dists suite a root
// Packages.gz is no IndexTarget and raises nothing.
func TestServeHTTP_FlatSnapshotMissIndexTargetRaisesAlert(t *testing.T) {
	logs := &strictLogBuf{}
	h := newTestHandlerWithServe(t, config.ServeConfig{}, slog.New(slog.NewJSONHandler(logs, nil)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("upstream")) }))
	defer srv.Close()
	scheme, host, port := splitURL(t, srv.URL)
	for _, suite := range []string{"/flat", "/flat/.", "/dists/noble"} {
		blob := writeBlob(t, h, simtest.ClearsignedFixture("InRelease of "+suite))
		commitInlineSnapshot(t, h, scheme, host, suite, blob, []cache.SnapshotMember{
			{Path: "InRelease", BlobHash: blob, DeclaredSHA256: blob},
		}, nil)
	}
	for _, tc := range []struct {
		path, arch string
	}{
		{"/flat/Packages.xz", "all"},
		{"/flat/Sources.gz", "source"},
		{"/flat/./Packages", "all"},
		{"/dists/noble/Packages.gz", ""},
		{"/flat/Packages.diff/Index", ""},
	} {
		series := `acu_serve_snapshot_index_target_404_total{architecture="` + tc.arch + `"}`
		before := metricValue(t, series)
		logs.buf.Reset()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newProxyReqHostPort("GET", scheme, host+port, tc.path))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s = %d, want 404 (not in snapshot)", tc.path, rec.Code)
		}
		recs := logs.find("snapshot_index_target_404")
		if tc.arch == "" {
			if len(recs) != 0 {
				t.Errorf("%s raised %v, want no alert", tc.path, recs)
			}
			continue
		}
		if len(recs) != 1 || recs[0]["architecture"] != tc.arch {
			t.Errorf("%s: snapshot_index_target_404 = %v, want one, architecture %q", tc.path, recs, tc.arch)
		}
		if got := metricValue(t, series) - before; got != 1 {
			t.Errorf("%s: %s grew by %v, want 1", tc.path, series, got)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -gcflags=-e ./internal/handler/`
Expected: FAIL — `too many arguments in call to isIndexTargetPath` (`have (string, string)`, `want (string)`; three calls in `path_class_test.go`) and `undefined: indexTargetArch`.

- [ ] **Step 3: Implement**

Create `internal/handler/flat_root.go`:

```go
package handler

import (
	"sync"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
)

// flatRootWarnEvery rate-limits flat_host_root_unsupported: at most one
// line per minute process-wide; suppressed counts the requests in between
// (spec C2, plan design note 2).
const flatRootWarnEvery = time.Minute

// flatRootWarn is flat_host_root_unsupported's rate limiter.
type flatRootWarn struct {
	mu         sync.Mutex
	last       time.Time
	suppressed int
}

// noteFlatHostRoot logs, at most once a minute process-wide, that a client
// asked for a flat repository's file at the host root (spec C2:
// `deb https://h/ /` and `deb https://h ./` are unsupported — such a
// directory is no suite, so it is never refreshed, adopted or vouched).
// The request is served as before (Phase 1). Monotonic time.
//
// AIDEV-NOTE: WARN, not INFO (design note 2): the repository will never
// refresh, and this line is the only place an operator learns it. The
// limit is global, not per host, so a client population cannot turn it
// into a log flood; the suppressed count says how many requests the last
// minute hid.
func (h *Handler) noteFlatHostRoot(req *proxy.Request) {
	h.flatRoot.mu.Lock()
	now := time.Now()
	if !h.flatRoot.last.IsZero() && now.Sub(h.flatRoot.last) < flatRootWarnEvery {
		h.flatRoot.suppressed++
		h.flatRoot.mu.Unlock()
		return
	}
	suppressed := h.flatRoot.suppressed
	h.flatRoot.last, h.flatRoot.suppressed = now, 0
	h.flatRoot.mu.Unlock()
	h.logger.Warn("flat_host_root_unsupported",
		"canonical_host", req.CanonicalHost,
		"path", req.Path,
		"suppressed", suppressed,
		"detail", "a flat repository at the host root is not tracked: it is served from the ordinary cache and never refreshed",
	)
}
```

In `internal/handler/handler.go`, replace

```go
	maxSuites  int
	suiteGuard cache.SuiteGuard
	rejections seedRejections
	seedWarn   seedWarn
```

with

```go
	maxSuites  int
	suiteGuard cache.SuiteGuard
	rejections seedRejections
	seedWarn   seedWarn

	// flatRoot is flat_host_root_unsupported's rate limiter (spec C2).
	flatRoot flatRootWarn
```

In `internal/handler/handler.go`, replace

```go
	req, err := h.parser.Parse(r.RequestURI, r.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		h.logRequest(r, "", "", "bad_request", http.StatusBadRequest, 0, false, 0, start)
		return
	}
```

with

```go
	req, err := h.parser.Parse(r.RequestURI, r.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		h.logRequest(r, "", "", "bad_request", http.StatusBadRequest, 0, false, 0, start)
		return
	}
	// Spec C2: a flat repository at the host root gets no suite; say so
	// (rate-limited) and serve the request as before.
	if req.IsMetadata && req.SuitePath == "" && aptpath.IsFlatHostRootPath(req.Path) {
		h.noteFlatHostRoot(req)
	}
```

In `internal/handler/handler.go`, replace

```go
		if isIndexTargetPath(memberPath) {
			arch := archFromPath(memberPath)
```

with

```go
		if isIndexTargetPath(req.SuitePath, memberPath) {
			arch := indexTargetArch(req.SuitePath, memberPath)
```

In `internal/handler/handler.go`, replace

```go
// AIDEV-NOTE: keep in lockstep with internal/freshness indexTargetGroupRE
// (the adoption-side group key). The two live in different packages on
// purpose (no import edge), but they describe the SAME surface — the
// files apt installs from. Widen/narrow them together.
var indexTargetPathRE = regexp.MustCompile(
	`(?:^|/)(?:binary-[a-z][a-z0-9]*/Packages|source/Sources)(?:\.[a-z0-9]+)?$`)

// isIndexTargetPath reports whether a request path is an apt IndexTarget
// (see indexTargetPathRE). The handler increments a dedicated counter and
// emits an alertable WARN when it serves an authoritative "not in
// snapshot" 404 for one of these — the direct, cause-agnostic signal that
// a client's `apt update` is broken against the current snapshot (the
// 2026-06-14 binary-all incident's symptom).
func isIndexTargetPath(p string) bool {
	return indexTargetPathRE.MatchString(p)
}
```

with

```go
// AIDEV-NOTE: keep in lockstep with internal/freshness indexTargetGroupRE
// and flatIndexTargetGroupRE (the adoption-side group keys). They live in
// different packages on purpose (no import edge), but they describe the
// SAME surface — the files apt installs from — in two layouts, and the
// layout rule (spec C5: the root shapes count only for a flat suite,
// aptpath.IsFlatSuitePath) is the same on both sides. Widen/narrow them
// together.
var indexTargetPathRE = regexp.MustCompile(
	`(?:^|/)(?:binary-[a-z][a-z0-9]*/Packages|source/Sources)(?:\.[a-z0-9]+)?$`)

// flatIndexTargetPathRE is indexTargetPathRE for a flat suite: its root
// Packages* / Sources* (spec C5), which the handler sees as bare
// suite-relative member paths ("Packages.gz"). No pdiff Index, as above.
var flatIndexTargetPathRE = regexp.MustCompile(`^(?:Packages|Sources)(?:\.[a-z0-9]+)?$`)

// isIndexTargetPath reports whether a suite-relative member path of
// suitePath is an apt IndexTarget (see indexTargetPathRE; layout-aware,
// spec C5: a flat suite's are its root Packages*/Sources* only, and a
// root Packages under a dists suite is none). The handler increments a
// dedicated counter and emits an alertable WARN when it serves an
// authoritative "not in snapshot" 404 for one of these — the direct,
// cause-agnostic signal that a client's `apt update` is broken against
// the current snapshot (the 2026-06-14 binary-all incident's symptom).
func isIndexTargetPath(suitePath, p string) bool {
	if aptpath.IsFlatSuitePath(suitePath) {
		return flatIndexTargetPathRE.MatchString(p)
	}
	return indexTargetPathRE.MatchString(p)
}

// indexTargetArch is the architecture label of an IndexTarget member path
// of suitePath for the snapshot_index_target_404 alert (the flat arch
// labels of plan design note 4): a flat suite's root Packages carries
// every architecture (the pseudo-arch "all"), its root Sources is
// "source"; any other path takes archFromPath's label.
func indexTargetArch(suitePath, p string) string {
	if aptpath.IsFlatSuitePath(suitePath) {
		if strings.HasPrefix(p, "Sources") {
			return "source"
		}
		return "all"
	}
	return archFromPath(p)
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/handler/ -run 'TestIsIndexTargetPath|TestIndexTargetArch|TestServeHTTP_FlatHostRoot|TestServeHTTP_FlatSnapshotMiss|TestServeHTTP_SnapshotMissIndexTarget' -v`
Expected: PASS.

Run: `go test ./internal/handler/`
Expected: PASS (about 22 s).

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| `isIndexTargetPath` ignores the layout | `TestIsIndexTargetPath_LayoutAware` (`isIndexTargetPath("/core:/stable:/v1.34/deb", "Packages") = false, want true`), `TestServeHTTP_FlatSnapshotMissIndexTargetRaisesAlert` (`/flat/Packages.xz: snapshot_index_target_404 = [], want one, architecture "all"`) |
| the root shapes under any layout | `TestIsIndexTargetPath_LayoutAware` (`isIndexTargetPath("/ubuntu/dists/noble", "Packages") = true, want false`), `TestServeHTTP_FlatSnapshotMissIndexTargetRaisesAlert` (`/dists/noble/Packages.gz raised [map[architecture: …]], want no alert`) |
| `flatIndexTargetPathRE` admits the pdiff Index | `TestIsIndexTargetPath_LayoutAware` (`isIndexTargetPath("/core:/stable:/v1.34/deb", "Packages.diff/Index") = true, want false`), `TestServeHTTP_FlatSnapshotMissIndexTargetRaisesAlert` |
| a flat `Sources` labelled `all` | `TestIndexTargetArch` (`indexTargetArch("/flat", "Sources.xz") = "all", want "source"`), `TestServeHTTP_FlatSnapshotMissIndexTargetRaisesAlert` |
| the alert keeps `archFromPath` | `TestServeHTTP_FlatSnapshotMissIndexTargetRaisesAlert` (`/flat/Packages.xz: snapshot_index_target_404 = [map[architecture: …]], want one, architecture "all"`) |
| no host-root check in `ServeHTTP` | `TestServeHTTP_FlatHostRootIsUnsupportedAndSaysSo` (`flat_host_root_unsupported = [], want one WARN for /InRelease, suppressed 0`) |
| the line unlimited (`if false`) | `TestServeHTTP_FlatHostRootIsUnsupportedAndSaysSo` (`flat_host_root_unsupported = [map[… path:/InRelease …] map[… path:/./InRelease …] …], want one WARN …`) |
| the suppressed count not reset after a line | `TestServeHTTP_FlatHostRootIsUnsupportedAndSaysSo` (`… want a third line counting only its own minute (suppressed 1)`) |
| the line at INFO | `TestServeHTTP_FlatHostRootIsUnsupportedAndSaysSo` (`… level:INFO …], want one WARN …`) |

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/handler/handler.go internal/handler/flat_root.go internal/handler/flat_root_test.go internal/handler/path_class_test.go
git add internal/handler/handler.go internal/handler/flat_root.go internal/handler/flat_root_test.go internal/handler/path_class_test.go
git commit -m "Layout-aware snapshot_index_target_404 and flat arch labels (spec C5); log flat_host_root_unsupported (spec C2)"
```

(Record the mutation table in the commit message.)

### Task 7: PR #8's handler evidence: rows, T1, touches and onboarding for flat suites and their twins

PR #8's evidence (spec §9's G row; the overview's G line): a miss on `/…/deb/InRelease` and on `/…/deb/./InRelease` creates one suite row each and fires T1 under it, a later hit fires T1 again, and `/…/deb/Packages_1.0_all.deb`, host-root `/InRelease` and `/…/deb//InRelease` create none and fire nothing. The D carry-forward: the handler already sniffs, creates rows for and touches a flat index (ruling R-D3), so this task adds no handler code — its tests add the flat cases to the miss-path, onboarding and touch tests: a Release-less repository's root index creates its suite's row (adopter wired), a cached flat anchor or index onboards its suite on a hit, a detached flat suite's onboarding marks its cached `Release.gpg`, root `Packages*`/`Sources*` serves touch their suite (spec §9's "flat root-index touch") and a flat `Contents-*` does not, and a `.deb` vouched through the suffix fallback touches its flat suite. Review Focus 1: a v6 cache's flat `InRelease`, cached under its directory and its `./` twin, onboards two suites with the base's marker before the first body byte (legacy flat onboarding writes the marker the startup pass cannot: the E carry-forward). Ruling R-G4: each twin has its own row and its own T1. The cap-rejection cache is keyed by the suite alone (the D carry-forward), so a flat suite's files share one entry: nothing to add.

These tests pin what Tasks 1, 5 and 6 built, so they pass when written; Step 2 shows them failing against the old grammar instead.

**Files:**
- Create: `internal/handler/flat_handler_test.go`
- Modify: `internal/handler/handler_test.go` (`TestServeHTTP_FlatRepositoryFreshness`, before `TestServeHTTP_FreshnessNotTriggeredOnBlobHit`)

**Interfaces:**
- Consumes: Task 1's grammar, Task 5's fallback; `newSuiteRig`, `(*suiteRig).serve`/`get`/`suite`/`marked`/`trackOld`/`requested`/`cacheAnchor`/`secondConn`, `upFile`, `waitFor`, `rowInRelease`, `rowRelease`, `rowGPG`, `rowHTML`, `commitInlineSnapshot`, `writeBlob`, `firstByteProbe`, `sqliteFile`, `noUpstream`, `newProxyReqHostPort`, `silentLogger` (handler tests); `cache.MigrateToVersionForTest`, `freshness.RunLegacyTrustAudit`.
- Produces: test fixtures `flatPackagesGz`, `flatPackages` (used by Tasks 10, 12 and 15).

- [ ] **Step 1: Write the tests**

In `internal/handler/handler_test.go`, add before `// TestServeHTTP_FreshnessNotTriggeredOnBlobHit asserts that a hit on a`:

```go
// TestServeHTTP_FlatRepositoryFreshness is PR #8's evidence (spec §9's G
// row), ported with our results: a flat repository's InRelease — `deb
// <uri>/ /` requests <dir>/InRelease, `deb <uri> ./` <dir>/./InRelease —
// creates one suite row each on its miss, the literal directory, and fires
// a freshness check (T1) under it once the response is written (phase E's
// T1-on-seed); a later hit fires T1 again. A package whose name starts
// "Packages", the host root and an empty segment create no row and fire
// nothing. Before phase G no flat path had a suite, so neither happened
// and the first InRelease cached was served forever. PR #8's plain-text
// body would fail spec C3's InRelease sniff: the port serves a clearsigned
// one.
func TestServeHTTP_FlatRepositoryFreshness(t *testing.T) {
	const k8s = "/core:/stable:/v1.34/deb"
	r := newSuiteRig(t, true, 0)
	calls := func(suite string) int {
		n := 0
		for _, c := range r.fresh.snapshot() {
			if c.suitePath == suite {
				n++
			}
		}
		return n
	}
	for _, tc := range []struct{ path, suite string }{
		{k8s + "/InRelease", k8s},
		{k8s + "/./InRelease", k8s + "/."},
	} {
		r.serve(tc.path, upFile{body: rowInRelease})
		if miss := r.get(tc.path); miss.Code != http.StatusOK || miss.Header().Get(hdrXCache) != cacheMiss {
			t.Fatalf("%s: miss = %d %q", tc.path, miss.Code, miss.Header().Get(hdrXCache))
		}
		if r.suite(tc.suite) == nil {
			t.Fatalf("%s: the miss created no row for %s", tc.path, tc.suite)
		}
		waitFor(t, "the miss's T1 under "+tc.suite, func() bool { return calls(tc.suite) == 1 })
		if hit := r.get(tc.path); hit.Code != http.StatusOK || hit.Header().Get(hdrXCache) != cacheHit {
			t.Fatalf("%s: hit = %d %q", tc.path, hit.Code, hit.Header().Get(hdrXCache))
		}
		waitFor(t, "the hit's T1 under "+tc.suite, func() bool { return calls(tc.suite) == 2 })
	}
	for _, p := range []string{k8s + "/Packages_1.0_all.deb", "/InRelease", k8s + "//InRelease"} {
		r.serve(p, upFile{body: rowInRelease})
		for range 2 { // a miss, then a hit
			if rec := r.get(p); rec.Code != http.StatusOK {
				t.Fatalf("%s = %d", p, rec.Code)
			}
		}
	}
	r.h.Close() // drains the T1 goroutines
	all, err := r.c.ListSuites(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("suite rows = %+v, want only %s and %s/.", all, k8s, k8s)
	}
	if got := len(r.fresh.snapshot()); got != 4 {
		t.Fatalf("T1 calls = %+v, want 4 (a miss and a hit per flat suite, none for the other paths)", r.fresh.snapshot())
	}
}
```

Create `internal/handler/flat_handler_test.go`:

```go
package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
)

// Flat index bodies that pass, and one that fails, the flat index sniff
// (spec C3: the compression magic for the extension, or "Package:").
var (
	flatPackagesGz = []byte{0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03}
	flatPackages   = []byte("Package: hello\nVersion: 1\nArchitecture: amd64\nFilename: ./hello_1_amd64.deb\n")
)

// Spec C3 (the D carry-forward: the handler already sniffs, creates rows
// for and touches a flat index; phase G's grammar makes the paths
// reachable): with an adopter wired, a sniff-passing root Packages* or
// Sources* miss of a flat directory creates its suite's row — the suite of
// a Release-less repository — and a `./` twin's its own; so does any other
// directory's (the grammar's suite is the literal directory, so a
// Packages under /nested/sub/main is that directory's root index). An HTML
// page, a dists suite's root Packages.gz, a flat Contents-* and a flat
// Release.gpg create none. Without an adopter a flat index creates none
// (spec C3: InRelease only).
func TestServeHTTP_FlatIndexMissCreatesItsSuiteRow(t *testing.T) {
	for _, adoption := range []bool{true, false} {
		r := newSuiteRig(t, adoption, 0)
		files := map[string][]byte{
			"/fury/Packages.gz":          flatPackagesGz,
			"/fury/./Packages":           flatPackages,
			"/goreleaser/Sources.xz":     {0xfd, '7', 'z', 'X', 'Z', 0x00, 0x00},
			"/soft404/Packages.gz":       rowHTML,
			"/u/dists/s/Packages.gz":     flatPackagesGz,
			"/contents/Contents-amd64":   []byte("usr/bin/hello utils/hello\n"),
			"/gpg/Release.gpg":           rowGPG,
			"/nested/sub/main/Packages":  flatPackages,
			"/core:/stable:/v1.34/deb/x": []byte("not a suite file"),
		}
		for p, b := range files {
			r.serve(p, upFile{body: b})
			if rec := r.get(p); rec.Code != http.StatusOK {
				t.Fatalf("adoption=%v: %s = %d", adoption, p, rec.Code)
			}
		}
		for suite, want := range map[string]bool{
			"/fury": adoption, "/fury/.": adoption, "/goreleaser": adoption,
			"/nested/sub/main": adoption,
			"/soft404":         false, "/u/dists/s": false, "/contents": false, "/gpg": false,
		} {
			if got := r.suite(suite) != nil; got != want {
				t.Errorf("adoption=%v: %s has a row = %v, want %v", adoption, suite, got, want)
			}
		}
		if adoption && r.marked("/fury") {
			t.Error("a flat index earned a signed-seen marker")
		}
	}
}

// Spec C3 legacy onboarding for flat suites (Review Focus 1): a url_path
// hit on a cached sniff-passing flat anchor or flat index of a suite
// without a row creates the row before the body — a flat InRelease with
// its base's marker in the creating transaction, its `./` twin's InRelease
// its own row under the same base, a detached flat suite's Release
// marking its cached Release.gpg, and (adopter wired) a Release-less
// suite's cached Packages.gz with no marker — and the hit's T1 checks it.
// A cached flat Contents-* onboards nothing.
func TestServeHTTP_HitOnboardsAFlatSuite(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	const k8s = "/core:/stable:/v1.34/deb"
	r.cacheAnchor(k8s+"/InRelease", rowInRelease, 1_700_000_000)
	r.cacheAnchor(k8s+"/./InRelease", rowInRelease, 1_700_000_000)
	r.cacheAnchor("/jenkins/binary/Release", rowRelease, 1_700_000_000)
	r.cacheAnchor("/jenkins/binary/Release.gpg", rowGPG, 1_700_000_100)
	r.cacheAnchor("/fury/Packages.gz", flatPackagesGz, 1_700_000_000)
	r.cacheAnchor("/contents/Contents-amd64.gz", flatPackagesGz, 1_700_000_000)
	for _, p := range []string{k8s + "/InRelease", k8s + "/./InRelease", "/jenkins/binary/Release", "/fury/Packages.gz",
		"/contents/Contents-amd64.gz"} {
		if rec := r.get(p); rec.Code != http.StatusOK || rec.Header().Get(hdrXCache) != cacheHit {
			t.Fatalf("%s = %d %q, want a hit", p, rec.Code, rec.Header().Get(hdrXCache))
		}
	}
	for _, suite := range []string{k8s, k8s + "/.", "/jenkins/binary", "/fury"} {
		if s := r.suite(suite); s == nil || s.LastCheckAt != nil {
			t.Fatalf("%s: row %+v, want onboarded with NULL check stamps", suite, s)
		}
	}
	if r.suite("/contents") != nil {
		t.Fatal("a cached flat Contents-* onboarded its suite")
	}
	if !r.marked(k8s) {
		t.Fatal("the onboarded flat InRelease earned its base no marker")
	}
	if at, ok, err := r.c.SignedSeen(context.Background(), "http", "127.0.0.1", "/jenkins/binary"); err != nil || !ok || at != 1_700_000_100 {
		t.Fatalf("detached flat suite's marker = %d %v %v, want its cached Release.gpg's 1700000100", at, ok, err)
	}
	if r.marked("/fury") {
		t.Fatal("a Release-less flat suite's onboarding wrote a marker")
	}
	r.h.Close() // drains the T1 goroutines
	checked := map[string]bool{}
	for _, c := range r.fresh.snapshot() {
		checked[c.suitePath] = true
	}
	for _, suite := range []string{k8s, k8s + "/.", "/jenkins/binary", "/fury"} {
		if !checked[suite] {
			t.Errorf("no T1 for the onboarded %s (T1 for %v)", suite, checked)
		}
	}
	if checked["/contents"] {
		t.Error("T1 fired for the untracked /contents")
	}
}

// Spec C3 client interest, the flat root-index touch: a tracked flat
// suite's root Packages* and Sources* serves — hit or miss, its `./`
// twin's as well — record interest (TouchSuite); a flat Contents-* is no
// suite file, so its serve records none.
func TestServeHTTP_FlatRootIndexServesTouchTheirSuite(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	r.trackOld("/hit")
	r.cacheAnchor("/hit/Packages.gz", flatPackagesGz, 1_700_000_000)
	r.get("/hit/Packages.gz")
	waitFor(t, "the root Packages.gz hit's touch", func() bool { return r.requested("/hit") > 1000 })

	r.trackOld("/miss")
	r.serve("/miss/Sources.xz", upFile{body: []byte{0xfd, '7', 'z', 'X', 'Z', 0x00, 0x00}})
	r.get("/miss/Sources.xz")
	waitFor(t, "the root Sources.xz miss's touch", func() bool { return r.requested("/miss") > 1000 })

	r.trackOld("/twin/.")
	r.cacheAnchor("/twin/./Packages", flatPackages, 1_700_000_000)
	r.get("/twin/./Packages")
	waitFor(t, "the twin's root Packages hit's touch", func() bool { return r.requested("/twin/.") > 1000 })

	if f := aptpath.ClassifySuiteFile("/hit", "/hit/Contents-amd64.gz"); f.Touches() {
		t.Fatalf("a flat Contents-* is suite file %d, want none (no touch)", f)
	}
}

// Spec C3 and C6: a .deb a flat snapshot vouches through the suffix
// fallback — the Jenkins shape, whose Filenames are relative to the
// suite's parent — is served validated and touches the flat suite, so an
// install-only client keeps it alive.
func TestServeHTTP_VouchedFlatDebTouchesItsSuite(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	deb := []byte("jenkins 2.479.3, a vouched flat .deb")
	sum := sha256.Sum256(deb)
	inrel := writeBlob(t, r.h, rowInRelease)
	commitInlineSnapshot(t, r.h, "http", "127.0.0.1", "/jenkins/binary", inrel,
		[]cache.SnapshotMember{{Path: "InRelease", BlobHash: inrel, DeclaredSHA256: inrel}},
		[]cache.PackageHash{{CanonicalScheme: "http", CanonicalHost: "127.0.0.1", Path: "/jenkins/binary/binary/jenkins_2.479.3_all.deb",
			DeclaredSHA256: hex.EncodeToString(sum[:]), PackageName: "jenkins", Architecture: "all", Version: "2.479.3",
			FlatFilename: "binary/jenkins_2.479.3_all.deb"}})
	if _, err := r.secondConn().Exec(`UPDATE suite_freshness SET last_requested_at = 1000`); err != nil {
		t.Fatal(err)
	}
	r.serve("/jenkins/binary/jenkins_2.479.3_all.deb", upFile{body: deb})
	rec := r.get("/jenkins/binary/jenkins_2.479.3_all.deb")
	if rec.Code != http.StatusOK || rec.Body.String() != string(deb) {
		t.Fatalf("vouched flat .deb = %d %q", rec.Code, rec.Body.String())
	}
	waitFor(t, "the vouched flat .deb's touch", func() bool { return r.requested("/jenkins/binary") > 1000 })
}

// Spec §9's G row and Review Focus 1: a cache a v6 binary wrote — a flat
// repository's InRelease cached under its directory and under its `./`
// twin, no suite rows — upgraded in place. The startup pass marks nothing
// (both suites are untracked); each anchor's first hit onboards its own
// suite, the base's marker committed before the first body byte (legacy
// flat onboarding writes the marker the startup pass cannot: the E
// carry-forward).
func TestServeHTTP_OnboardsAFlatSuiteFromAV6Cache(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	if err := cache.MigrateToVersionForTest(ctx, dir, 6); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(rowInRelease)
	hash := hex.EncodeToString(sum[:])
	db := sqliteFile(t, filepath.Join(dir, "cache.db"))
	if _, err := db.Exec(`INSERT INTO blob(hash,size,created_at,refcount) VALUES(?,?,1,0)`, hash, len(rowInRelease)); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/repo/InRelease", "/repo/./InRelease"} {
		if _, err := db.Exec(`INSERT INTO url_path(canonical_scheme,canonical_host,path,blob_hash,upstream_url,is_metadata,last_requested_at,request_count)
		 VALUES('http','127.0.0.1',?,?,?,1,1700000000,3)`, p, hash, "http://127.0.0.1"+p); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	c, err := cache.Open(ctx, dir, silentLogger()) // migrates v6 → v7
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	w, err := c.NewTempBlob() // the pool file the v6 binary wrote
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(rowInRelease); err != nil {
		t.Fatal(err)
	}
	if got, err := w.Finalize(int64(len(rowInRelease))); err != nil || got != hash {
		t.Fatalf("pool file = %s, %v; want %s", got, err, hash)
	}
	if res, err := freshness.RunLegacyTrustAudit(ctx, c, silentLogger()); err != nil || res.AnchorMarkers != 0 {
		t.Fatalf("startup pass = %+v, %v; want no marker for the untracked flat suites", res, err)
	}
	parser, err := proxy.New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{Parser: parser, Cache: c, Fetch: noUpstream{}, Logger: silentLogger(), HostLimiter: hostsem.New(4),
		AdoptionEnabled: true, MaxSuites: 5000})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	for _, tc := range []struct{ path, suite string }{{"/repo/InRelease", "/repo"}, {"/repo/./InRelease", "/repo/."}} {
		p := &firstByteProbe{ResponseRecorder: httptest.NewRecorder(), c: c, suite: tc.suite}
		h.ServeHTTP(p, newProxyReqHostPort("GET", "http", "127.0.0.1", tc.path))
		if p.Code != http.StatusOK || p.Header().Get(hdrXCache) != cacheHit || !p.seen {
			t.Fatalf("%s: hit = %d %q, body written %v", tc.path, p.Code, p.Header().Get(hdrXCache), p.seen)
		}
		if p.err != nil || !p.row || !p.mark {
			t.Fatalf("%s: at the first body byte row=%v marker=%v (read error %v), want both", tc.path, p.row, p.mark, p.err)
		}
	}
	all, err := c.ListSuites(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("suite rows = %+v, %v; want /repo and /repo/.", all, err)
	}
	if _, ok, err := c.SignedSeen(ctx, "http", "127.0.0.1", "/repo"); err != nil || !ok {
		t.Fatalf("the twins' shared base marker = %v, %v", ok, err)
	}
}
```

- [ ] **Step 2: Run them, and against the old grammar**

Run: `go test ./internal/handler/ -run 'TestServeHTTP_FlatRepositoryFreshness|TestServeHTTP_FlatIndexMiss|TestServeHTTP_HitOnboardsAFlat|TestServeHTTP_FlatRootIndex|TestServeHTTP_VouchedFlatDeb|TestServeHTTP_OnboardsAFlatSuite' -v`
Expected: PASS.

Then, in `internal/aptpath/suite.go`'s `SuitePath`, replace the last three lines (`if hasDistsSegment(p) { return distsSuitePath(segs) }` and `return flatSuitePath(segs)`) with `return distsSuitePath(segs)` — the grammar before this phase — and run the same command.
Expected: FAIL — `TestServeHTTP_FlatRepositoryFreshness` (`/core:/stable:/v1.34/deb/InRelease: the miss created no row for /core:/stable:/v1.34/deb`), `TestServeHTTP_FlatIndexMissCreatesItsSuiteRow`, `TestServeHTTP_HitOnboardsAFlatSuite` (`/core:/stable:/v1.34/deb: row <nil>, want onboarded with NULL check stamps`), `TestServeHTTP_FlatRootIndexServesTouchTheirSuite` (`timed out waiting for the root Packages.gz hit's touch`), `TestServeHTTP_OnboardsAFlatSuiteFromAV6Cache` (`/repo/InRelease: at the first body byte row=false marker=false …`). Restore `SuitePath`.

- [ ] **Step 3: Run the package**

Run: `go test ./internal/handler/`
Expected: PASS.

- [ ] **Step 4: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| `SuitePath` without its flat grammar (Step 2) | the five tests above, and Task 6's `TestServeHTTP_FlatHostRootIsUnsupportedAndSaysSo` (`suite rows = [], want only /repo's`) and `TestServeHTTP_FlatSnapshotMissIndexTargetRaisesAlert` |
| `ClassifySuiteFile` without its `FileFlatIndex` branch | `TestServeHTTP_FlatIndexMissCreatesItsSuiteRow` (`adoption=true: /fury has a row = false, want true`), `TestServeHTTP_HitOnboardsAFlatSuite` (`/fury: row <nil>, …`), `TestServeHTTP_FlatRootIndexServesTouchTheirSuite` |
| a flat index creates a row without an adopter (`CreatesRow`) | `TestServeHTTP_FlatIndexMissCreatesItsSuiteRow` (`adoption=false: /fury/. has a row = true, want false`) |
| a flat index records no interest (`Touches` excludes `FileFlatIndex`) | `TestServeHTTP_FlatRootIndexServesTouchTheirSuite` (`timed out waiting for the root Packages.gz hit's touch`) |
| `DeclaredHashesForPath` without its fallback | `TestServeHTTP_VouchedFlatDebTouchesItsSuite` (`timed out waiting for the vouched flat .deb's touch`) |

- [ ] **Step 5: Commit**

```bash
gofmt -l internal/handler/handler_test.go internal/handler/flat_handler_test.go
git add internal/handler/handler_test.go internal/handler/flat_handler_test.go
git commit -m "PR #8's handler evidence for flat suites and twins: rows, T1, touches, onboarding, the v6 upgrade"
```

(Record the mutation table in the commit message.)

### Task 8: Deferred minors in the handler's suite rows and miss path

Tasks 6 and 10 change `internal/handler/handler.go` by hand and this task changes `internal/handler/suite_rows.go`, so their open deferred minors land here, each in its own step-sized change:
- D-32 (G): `cachedAnchors` and `markCachedSignedAnchors` repeated one loop (look up, classify, read the head, sniff); one helper, `readCachedAnchor`, now gives both the same answer, and `runFetch` builds its suite seed once.
- D-31: the rejection sweep's timer body recovers its own panic (a test seam, `sweepHook`, makes the sweep panic), and `New` refuses a negative `MaxSuites` — both pinned.
- E-57 and E-60: the strict-wait retry's two accepted residuals (re-joining the call it retries; the retry's call replacing `shared` and `waiters`) said in `serveCacheMiss`'s AIDEV-NOTE, the documentation fix each item offers.
- E-64: the T1-on-seed comment says that a creating miss re-dispatched to a current snapshot fires T1 twice, the second a cooldown skip.
- E-65: a test with two first-contact misses of one suite on different files (only the creating one fires the seed T1); `missBackstopDeadline` returns a past deadline for a request older than its budget (no floor); onboarding's backstop is cut short by a shutdown.
- E-67: the comments of `backstopTimeout`, `missBodyBudget` and `onboardTimeout` say which bound each is; `TestServeHTTP_BackstopIsBoundedBeforeTheBody`'s onboarding leg asserts `onboardTimeout`.
- E-68: `runOnboard`'s unused `*http.Request` is gone.
- E-71: a seam between the hit path's two strict-mode lookups (`strictLookupHook`) pins `case strictCanceled`: a request whose context ends there is answered `client_canceled`, never the `.deb`.

Checked and left in these files: C-21 and A-56 (H).

**Files:**
- Create: `internal/handler/handler_minors_test.go`
- Modify: `internal/handler/suite_rows.go` (`seedRejections.sweepHook`, `expire`, `signedAnchorNames`, `readCachedAnchor`, `cachedAnchors`, `markCachedSignedAnchors`, the three timeouts' comments, `onboard`, `runOnboard`), `internal/handler/handler.go` (`Handler.strictLookupHook`, `checkPackageHashWait`, `runFetch`'s seed, `serveCacheMiss`'s AIDEV-NOTE and T1-on-seed comment), `internal/handler/suite_rows_test.go` (`TestServeHTTP_BackstopIsBoundedBeforeTheBody`)

**Interfaces:**
- Consumes: `newSuiteRig`, `upFile`, `rowInRelease`, `rowRelease`, `rowGPG`, `newFlipStack`, `(*flipStack).cacheRow`/`signsAfterTheSniff`, `flipSimHost`, `newPhase3StrictHandlerCapturing`, `adoptCoverageComplete`, `splitURL`, `hostKey`, `writeBlob`, `proxyReq`, `noUpstream`, `silentLogger`.
- Produces: `(*Handler).readCachedAnchor(ctx, key cache.SuiteKey, p string) (*cache.URLPath, bool, error)`, `signedAnchorNames`; test seams `seedRejections.sweepHook`, `Handler.strictLookupHook`; `runOnboard(req *proxy.Request, sr *suiteReq, deadline time.Time)`; test helper `openTestCache`.

- [ ] **Step 1: Write the failing tests**

Create `internal/handler/handler_minors_test.go`:

```go
package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// Deferred minor D-32: readCachedAnchor is the one reading of a cached
// signed anchor that the creating transaction's marks and the backstop
// share. No row: nothing, no error; a row without a blob: unsigned; a
// clearsigned InRelease and an armored Release.gpg: signed; a plain-text
// InRelease: unsigned; a pool file that is gone: the row and an error.
func TestReadCachedAnchor(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	ctx := context.Background()
	key := cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "127.0.0.1", SuitePath: "/r/dists/s"}
	if row, signed, err := r.h.readCachedAnchor(ctx, key, "/r/dists/s/InRelease"); row != nil || signed || err != nil {
		t.Fatalf("no row: %+v %v %v, want nil, false, nil", row, signed, err)
	}
	if err := r.c.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
		Path: "/r/dists/s/InRelease", UpstreamURL: "http://127.0.0.1/r/dists/s/InRelease", IsMetadata: true}); err != nil {
		t.Fatal(err)
	}
	if row, signed, err := r.h.readCachedAnchor(ctx, key, "/r/dists/s/InRelease"); row == nil || signed || err != nil {
		t.Fatalf("a row without a blob: %+v %v %v, want the row, unsigned, nil", row, signed, err)
	}
	for _, tc := range []struct {
		path string
		body []byte
		want bool
	}{
		{"/r/dists/s/InRelease", rowInRelease, true},
		{"/r/dists/s/Release.gpg", rowGPG, true},
		{"/r/dists/p/InRelease", []byte("Origin: plain\n"), false},
	} {
		r.cacheAnchor(tc.path, tc.body, 1_700_000_000)
		k := key
		k.SuitePath = tc.path[:len(tc.path)-len("/InRelease")]
		if tc.path == "/r/dists/s/Release.gpg" {
			k.SuitePath = "/r/dists/s"
		}
		if row, signed, err := r.h.readCachedAnchor(ctx, k, tc.path); row == nil || signed != tc.want || err != nil {
			t.Fatalf("%s: %+v %v %v, want signed=%v", tc.path, row, signed, err, tc.want)
		}
	}
	row, _, _ := r.h.readCachedAnchor(ctx, key, "/r/dists/s/InRelease")
	if err := os.Remove(r.c.BlobPath(*row.BlobHash)); err != nil {
		t.Fatal(err)
	}
	if got, signed, err := r.h.readCachedAnchor(ctx, key, "/r/dists/s/InRelease"); got == nil || signed || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missing pool file: %+v %v %v, want the row, unsigned, ErrNotExist", got, signed, err)
	}
}

// Deferred minor D-31: the rejection sweep's timer body recovers its own
// panic — reported through onPanic, the lock released (the deferred
// unlock runs first) — and New refuses a negative MaxSuites.
func TestSeedRejections_ExpireRecoversItsPanic(t *testing.T) {
	var got any
	s := &seedRejections{onPanic: func(v any) { got = v }, sweepHook: func() { panic("sweep boom") }}
	s.expire()
	if got != "sweep boom" {
		t.Fatalf("onPanic got %v, want the sweep's panic", got)
	}
	done := make(chan int, 1)
	go func() { done <- s.len() }() // the lock was released
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep's panic left the rejection cache locked")
	}
	parser, err := proxy.New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(Config{Parser: parser, Cache: openTestCache(t), Fetch: noUpstream{}, HostLimiter: hostsem.New(1),
		Logger: silentLogger(), MaxSuites: -1})
	if err == nil || err.Error() != "handler: negative MaxSuites" {
		t.Fatalf("New(MaxSuites: -1) = %v, want handler: negative MaxSuites", err)
	}
}

// openTestCache opens a cache in a test directory.
func openTestCache(t *testing.T) *cache.Cache {
	t.Helper()
	c, err := cache.Open(context.Background(), t.TempDir(), silentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// Deferred minor E-65 (a): only the miss that created its suite's row
// fires the seed T1. Two first-contact misses of one suite on different
// files: the Release miss reads the suite untracked, then — while it
// prepares its creation — the InRelease miss creates the row and fires
// its seed T1; the Release miss's creation finds the row (SuiteExists)
// and fires none.
func TestServeHTTP_AMissThatFindsTheRowCreatedFiresNoSeedT1(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	r.serve("/r/dists/s/InRelease", upFile{body: rowInRelease})
	r.serve("/r/dists/s/Release", upFile{body: rowRelease})
	r.h.cachedAnchorsHook = func() { // the Release miss's creation, before its writer operation
		r.h.cachedAnchorsHook = nil
		if rec := r.get("/r/dists/s/InRelease"); rec.Code != http.StatusOK || r.suite("/r/dists/s") == nil {
			t.Errorf("InRelease miss = %d; want 200 and the row created", rec.Code)
		}
	}
	if rec := r.get("/r/dists/s/Release"); rec.Code != http.StatusOK {
		t.Fatalf("Release miss = %d", rec.Code)
	}
	r.h.Close() // drains the T1 goroutines
	if calls := r.fresh.snapshot(); len(calls) != 1 {
		t.Fatalf("T1 calls %v, want exactly one (the creating InRelease miss's)", calls)
	}
}

// Deferred minor E-65 (b), design note 32: a miss whose request started
// longer ago than missBodyBudget gets a backstop deadline already past —
// the backstop fails at once — never a floor in the future.
func TestMissBackstopDeadline_APastBudgetIsPast(t *testing.T) {
	start := time.Now().Add(-time.Minute)
	if d := missBackstopDeadline(start); !d.Equal(start.Add(missBodyBudget)) {
		t.Fatalf("deadline %v after start, want %v (the budget, already past)", d.Sub(start), missBodyBudget)
	}
	if d := missBackstopDeadline(time.Now()); time.Until(d) > backstopTimeout {
		t.Fatalf("a fresh miss's deadline is %v away, want at most %v", time.Until(d), backstopTimeout)
	}
}

// Deferred minor E-65 (c), D-34: onboarding's backstop runs under the
// handler's lifecycle, so a shutdown that begins while it runs cuts it
// short at once: it never waits out the deadline it shares with its
// creation, and the hit is served.
func TestServeHTTP_OnboardingBackstopStopsAtShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newFlipStack(t, 0)
		const suite = "/n/dists/s"
		s.h.adoptionEnabled = true
		s.cacheRow(suite+"/Release", rowRelease, wallclock.Unix())
		s.signsAfterTheSniff(suite, wallclock.Unix()) // the creation is unproven: the backstop runs
		ran := false
		s.h.backstopHook = func(ctx context.Context) {
			ran = true
			s.h.lifecycleCancel() // shutdown begins during the backstop
			select {
			case <-ctx.Done():
			case <-time.After(time.Hour):
			}
		}
		start := time.Now()
		rec := httptest.NewRecorder()
		s.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+flipSimHost+suite+"/Release", nil))
		if rec.Code != http.StatusOK || !ran || time.Since(start) != 0 {
			t.Fatalf("status %d after %v, backstop ran %v; want 200 at once, the backstop cut short by the shutdown",
				rec.Code, time.Since(start), ran)
		}
	})
}

// Deferred minor E-71: the hit path's strict-mode decision when the
// request's context ends between its two lookups — DeclaredHashesForPath
// found no row, then the host's coverage read fails on the dead context —
// is client_canceled (503), never the .deb (FW-5 (a)).
func TestServeHTTP_StrictCanceledBetweenTheLookups(t *testing.T) {
	var upstreamHits atomic.Int32
	h, srv, logs := newPhase3StrictHandlerCapturing(t, true, true, &upstreamHits)
	scheme, host, port := splitURL(t, srv.URL)
	canonHost := hostKey(host, port)
	adoptCoverageComplete(t, h, scheme, canonHost, "/dists/noble", true)
	const debPath = "/pool/main/u/unvouched/unvouched_1_amd64.deb"
	blob := writeBlob(t, h, []byte("an unvouched .deb, cached"))
	if err := h.cache.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: scheme, CanonicalHost: canonHost,
		Path: debPath, BlobHash: &blob, UpstreamURL: srv.URL + debPath}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.strictLookupHook = cancel
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, debPath).WithContext(ctx))
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "unvouched .deb, cached") {
		t.Fatalf("status %d body %q, want 503 and no .deb", rec.Code, rec.Body.String())
	}
	recs := logs.find("request")
	if len(recs) == 0 || recs[len(recs)-1]["outcome"] != "client_canceled" {
		t.Fatalf("request lines %v, want the last one client_canceled", recs)
	}
}
```

In `internal/handler/suite_rows_test.go`, `TestServeHTTP_BackstopIsBoundedBeforeTheBody`, replace

```go
				if elapsed != backstopTimeout {
					t.Fatalf("hit=%v: served after %v, want after the backstop's %v bound (apt gives up at 30 s)", hit, elapsed, backstopTimeout)
				}
```

with

```go
				// Deferred minor E-67: the miss's backstop is bounded by
				// backstopTimeout, onboarding's by the deadline it shares
				// with its creation (onboardTimeout).
				want := backstopTimeout
				if hit {
					want = onboardTimeout
				}
				if elapsed != want {
					t.Fatalf("hit=%v: served after %v, want after the backstop's %v bound (apt gives up at 30 s)", hit, elapsed, want)
				}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -gcflags=-e ./internal/handler/`
Expected: FAIL — `r.h.readCachedAnchor undefined (type *Handler has no field or method readCachedAnchor)` (five times), `unknown field sweepHook in struct literal of type seedRejections`, `h.strictLookupHook undefined`.

- [ ] **Step 3: Implement**

In `internal/handler/suite_rows.go`, replace

```go
	// onPanic reports a panic recovered in the timer's goroutine.
	onPanic func(v any)
}
```

with

```go
	// onPanic reports a panic recovered in the timer's goroutine.
	onPanic func(v any)
	// sweepHook, when set (tests only), runs in expire under the lock:
	// a hook that panics stands in for a sweep that does
	// (TestSeedRejections_ExpireRecoversItsPanic, deferred minor D-31).
	sweepHook func()
}
```

In `internal/handler/suite_rows.go`, replace

```go
	s.mu.Lock()
	defer s.mu.Unlock()
	s.timer = nil
	if s.closed {
		return
	}
	s.sweepLocked(true)
```

with

```go
	s.mu.Lock()
	defer s.mu.Unlock()
	s.timer = nil
	if s.closed {
		return
	}
	if s.sweepHook != nil {
		s.sweepHook()
	}
	s.sweepLocked(true)
```

In `internal/handler/suite_rows.go`, replace

```go
// cachedAnchors reads the suite's signed-anchor url_path rows —
// <suite>/InRelease and <suite>/Release.gpg, but not except, the path this
// request stores itself — and sniffs each blob here, outside the writer,
// for a creation to mark in its transaction (cache.SuiteSeed.Cached,
// ruling PD-5). A missing row is listed without a blob, so one that
// appears before the transaction makes the creation unproven. A row whose
// url_path read or pool file read fails is logged WARN and listed without
// a blob too: if the transaction's re-read finds it holding one, the
// creation is unproven and the backstop retries it after the commit —
// never "not signed, hash unchanged", which would leave a tracked suite's
// signed anchor unmarked (Task 9 review, ruling b).
//
// AIDEV-NOTE: a row creation marks the suite's cached signed anchors in
// the creating transaction, so a tracked suite never serves an unmarked
// signed anchor (spec C3, the hit-path invariant at every committed
// state): markers are written only for suites with rows, and serving an
// existing tracked suite's anchor writes none.
func (h *Handler) cachedAnchors(ctx context.Context, key cache.SuiteKey, except string) []cache.CachedAnchor {
	var out []cache.CachedAnchor
	for _, name := range []string{"/InRelease", "/Release.gpg"} {
		p := key.SuitePath + name
		if p == except {
			continue
		}
		a := cache.CachedAnchor{Path: p}
		row, err := h.cache.LookupURL(ctx, key.CanonicalScheme, key.CanonicalHost, p)
		switch {
		case errors.Is(err, cache.ErrNotFound):
		case err != nil:
			h.logger.Warn("suite cached anchor read failed",
				"err", err, "canonical_host", key.CanonicalHost, "suite_path", key.SuitePath, "path", p)
		case row.BlobHash != nil:
			file := aptpath.ClassifySuiteFile(key.SuitePath, p)
			head, err := readHead(h.cache.BlobPath(*row.BlobHash), file.HeadBytes())
			if err != nil {
				h.logger.Warn("suite cached anchor read failed",
					"err", err, "canonical_host", key.CanonicalHost, "suite_path", key.SuitePath, "path", p)
				break
			}
			a.BlobHash, a.SeenAt, a.Signed = *row.BlobHash, row.LastUse(), file.Sniff(p, head)
		}
		out = append(out, a)
	}
```

with

```go
// signedAnchorNames are the cached anchors whose sniff earns a suite's
// base a signed-seen marker (spec C3), relative to the suite.
var signedAnchorNames = []string{"/InRelease", "/Release.gpg"}

// readCachedAnchor reads the url_path row of the suite's cached anchor p
// and sniffs its pool file here, outside the writer (ruling PD-5). row is
// nil when there is no row (err nil) or the url_path read failed; a row
// without a blob is returned unsigned; err is the url_path or pool read's
// error (row is then the row, or nil).
//
// AIDEV-NOTE: deferred minor D-32. This is the one reading of "is this
// cached anchor signed" that cachedAnchors (the creating transaction's
// marks) and markCachedSignedAnchors (the backstop) share: the two must
// agree, or a creation would prove an anchor the backstop then refuses to
// mark, or the reverse.
func (h *Handler) readCachedAnchor(ctx context.Context, key cache.SuiteKey, p string) (row *cache.URLPath, signed bool, err error) {
	row, err = h.cache.LookupURL(ctx, key.CanonicalScheme, key.CanonicalHost, p)
	switch {
	case errors.Is(err, cache.ErrNotFound):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	case row.BlobHash == nil:
		return row, false, nil
	}
	file := aptpath.ClassifySuiteFile(key.SuitePath, p)
	head, err := readHead(h.cache.BlobPath(*row.BlobHash), file.HeadBytes())
	if err != nil {
		return row, false, err
	}
	return row, file.Sniff(p, head), nil
}

// cachedAnchors reads the suite's signed-anchor url_path rows —
// <suite>/InRelease and <suite>/Release.gpg, but not except, the path this
// request stores itself — and sniffs each blob here, outside the writer,
// for a creation to mark in its transaction (cache.SuiteSeed.Cached,
// ruling PD-5). A missing row is listed without a blob, so one that
// appears before the transaction makes the creation unproven. A row whose
// url_path read or pool file read fails is logged WARN and listed without
// a blob too: if the transaction's re-read finds it holding one, the
// creation is unproven and the backstop retries it after the commit —
// never "not signed, hash unchanged", which would leave a tracked suite's
// signed anchor unmarked (Task 9 review, ruling b).
//
// AIDEV-NOTE: a row creation marks the suite's cached signed anchors in
// the creating transaction, so a tracked suite never serves an unmarked
// signed anchor (spec C3, the hit-path invariant at every committed
// state): markers are written only for suites with rows, and serving an
// existing tracked suite's anchor writes none.
func (h *Handler) cachedAnchors(ctx context.Context, key cache.SuiteKey, except string) []cache.CachedAnchor {
	var out []cache.CachedAnchor
	for _, name := range signedAnchorNames {
		p := key.SuitePath + name
		if p == except {
			continue
		}
		a := cache.CachedAnchor{Path: p}
		row, signed, err := h.readCachedAnchor(ctx, key, p)
		switch {
		case err != nil:
			h.logger.Warn("suite cached anchor read failed",
				"err", err, "canonical_host", key.CanonicalHost, "suite_path", key.SuitePath, "path", p)
		case row != nil && row.BlobHash != nil:
			a.BlobHash, a.SeenAt, a.Signed = *row.BlobHash, row.LastUse(), signed
		}
		out = append(out, a)
	}
```

In `internal/handler/suite_rows.go`, replace

```go
	var marks []cache.SignedSeenMark
	for _, name := range []string{"/InRelease", "/Release.gpg"} {
		p := key.SuitePath + name
		if p == except {
			continue
		}
		row, err := h.cache.LookupURL(ctx, key.CanonicalScheme, key.CanonicalHost, p)
		switch {
		case errors.Is(err, cache.ErrNotFound):
			continue
		case err != nil:
			h.logger.Warn("suite signed-seen backstop read failed",
				"err", err, "canonical_host", key.CanonicalHost, "suite_path", key.SuitePath, "path", p)
			continue
		case row.BlobHash == nil:
			continue
		}
		file := aptpath.ClassifySuiteFile(key.SuitePath, p)
		head, err := readHead(h.cache.BlobPath(*row.BlobHash), file.HeadBytes())
		if err != nil {
			h.logger.Warn("suite signed-seen backstop read failed",
				"err", err, "canonical_host", key.CanonicalHost, "suite_path", key.SuitePath, "path", p)
			continue
		}
		if !file.Sniff(p, head) {
			continue
		}
```

with

```go
	var marks []cache.SignedSeenMark
	for _, name := range signedAnchorNames {
		p := key.SuitePath + name
		if p == except {
			continue
		}
		row, signed, err := h.readCachedAnchor(ctx, key, p)
		if err != nil {
			h.logger.Warn("suite signed-seen backstop read failed",
				"err", err, "canonical_host", key.CanonicalHost, "suite_path", key.SuitePath, "path", p)
			continue
		}
		if !signed {
			continue
		}
```

In `internal/handler/suite_rows.go`, replace

```go
// backstopTimeout bounds markCachedSignedAnchors where it runs before the
// body — after an unproven creation on the miss path and after onboarding
// (which shares one deadline with its creation, onboardTimeout) — under
// apt's 30 s inactivity timeout (Task 9 review, controller addition d). It
// bounds the database steps; the pool reads (readHead, at most HeadBytes
// of a local file) take no context (deferred minor D-37). On a timeout the
// backstop's reads or its write fail and are logged WARN, and the body is
// served.
const backstopTimeout = 5 * time.Second

// missBodyBudget bounds, from a miss's start, how long it may wait before
// its body (design review M-15): the fetch, the store and a flip's hold of
// the just-created suite (at most flipgate.HoldMax) come first, then the
// backstop of an unproven creation, and apt gives up after 30 s without a
// byte.
const missBodyBudget = 25 * time.Second
```

with

```go
// backstopTimeout bounds markCachedSignedAnchors after an unproven creation
// on the miss path (through missBackstopDeadline), under apt's 30 s
// inactivity timeout (Task 9 review, controller addition d). Onboarding's
// backstop does not use it: it runs to the deadline it shares with its
// creation (onboardTimeout; deferred minor E-67). It bounds the database
// steps; the pool reads (readHead, at most HeadBytes of a local file) take
// no context (deferred minor D-37). On a timeout the backstop's reads or
// its write fail and are logged WARN, and the body is served.
const backstopTimeout = 5 * time.Second

// missBodyBudget is the latest, counted from a miss's start, its backstop
// may run (design review M-15; missBackstopDeadline): the fetch, the store
// and a flip's hold of the just-created suite (at most flipgate.HoldMax)
// come first, bounded by their own timeouts, and apt gives up after 30 s
// without a byte. Only the backstop honours it (deferred minor E-67).
const missBodyBudget = 25 * time.Second
```

In `internal/handler/suite_rows.go`, replace

```go
// onboardTimeout bounds onboarding's writer operation and its backstop
// together (design review M8; deferred minor D-35): the body waits on
// both, and between them on a flip's hold of the just-created suite (at
// most flipgate.HoldMax, 20 s, reachable since eager first adoption), so
// the worst case stays at 25 s, under apt's 30 s inactivity timeout. On a
// timeout the hit serves untracked, as on any onboarding error.
const onboardTimeout = 5 * time.Second
```

with

```go
// onboardTimeout bounds onboarding's writer operation and its backstop
// together (design review M8; deferred minor D-35): the body waits on
// both, and between them on a flip's hold of the just-created suite (at
// most flipgate.HoldMax, 20 s, reachable since eager first adoption), so
// the worst case stays at 25 s, under apt's 30 s inactivity timeout. A
// creation the deadline cuts short serves the hit untracked, as on any
// onboarding error; a backstop it cuts short logs WARN and serves the hit
// tracked (deferred minor E-67).
const onboardTimeout = 5 * time.Second
```

In `internal/handler/suite_rows.go`, replace

```go
	res, shared, _ := h.sf.Do(key, func() sfResult { return h.runOnboard(r, req, sr, deadline) })
```

with

```go
	res, shared, _ := h.sf.Do(key, func() sfResult { return h.runOnboard(req, sr, deadline) })
```

In `internal/handler/suite_rows.go`, replace

```go
func (h *Handler) runOnboard(r *http.Request, req *proxy.Request, sr *suiteReq, deadline time.Time) sfResult {
```

with

```go
func (h *Handler) runOnboard(req *proxy.Request, sr *suiteReq, deadline time.Time) sfResult {
```

In `internal/handler/handler.go`, replace

```go
			if !trackedAtStart && file.CreatesRow(h.adoptionEnabled) && !h.rejections.rejected(sr.key) {
				fetched.Create = h.suiteSeed(sr, true, fres.ETag, fres.LastModified, now)
				fetched.Create.Cached = h.cachedAnchors(dbCtx, sr.key, req.Path)
			}
			if file == aptpath.FileInRelease && !h.adoptionEnabled {
				fetched.Restamp = h.suiteSeed(sr, true, fres.ETag, fres.LastModified, now)
			}
```

with

```go
			seed := h.suiteSeed(sr, true, fres.ETag, fres.LastModified, now) // deferred minor D-32: built once
			if !trackedAtStart && file.CreatesRow(h.adoptionEnabled) && !h.rejections.rejected(sr.key) {
				create := *seed
				create.Cached = h.cachedAnchors(dbCtx, sr.key, req.Path)
				fetched.Create = &create
			}
			if file == aptpath.FileInRelease && !h.adoptionEnabled {
				fetched.Restamp = seed
			}
```

In `internal/handler/handler.go`, replace

```go
	// backstopHook, when set (tests only), runs at the top of
	// markCachedSignedAnchors with the backstop's bounded context: a hook
	// that waits on it stands in for a pool or writer that never answers
	// (TestServeHTTP_BackstopIsBoundedBeforeTheBody).
	backstopHook func(ctx context.Context)
```

with

```go
	// backstopHook, when set (tests only), runs at the top of
	// markCachedSignedAnchors with the backstop's bounded context: a hook
	// that waits on it stands in for a pool or writer that never answers
	// (TestServeHTTP_BackstopIsBoundedBeforeTheBody).
	backstopHook func(ctx context.Context)

	// strictLookupHook, when set (tests only), runs on the hit path between
	// its two strict-mode lookups — after DeclaredHashesForPath found no row
	// for a .deb, before the host's coverage read — the one window in which
	// the request's context can end with the first lookup done
	// (TestServeHTTP_StrictCanceledBetweenTheLookups, deferred minor E-71).
	strictLookupHook func()
```

In `internal/handler/handler.go`, replace

```go
		if !isDebPath(req.Path) {
			return false, "", 0, 0, false, packageValidation{}
		}
		switch sm, snapID, count := h.classifyStrictMode(r.Context(),
			req.CanonicalScheme, req.CanonicalHost); sm {
		case strictCanceled:
```

with

```go
		if !isDebPath(req.Path) {
			return false, "", 0, 0, false, packageValidation{}
		}
		if h.strictLookupHook != nil {
			h.strictLookupHook()
		}
		switch sm, snapID, count := h.classifyStrictMode(r.Context(),
			req.CanonicalScheme, req.CanonicalHost); sm {
		case strictCanceled:
```

In `internal/handler/handler.go`, replace

```go
		// AIDEV-NOTE: the retry is one more singleflight call: requests
		// that waited together coalesce again, and a waiter over the
		// 64-per-host cap, or with less than 1 s of its 25 s budget left,
		// is refused at once. A client that left while it waited gets no
		// retry (it ends client_canceled). The refusal is logged once per
		// fetch, by the singleflight leader (design review O1), as
		// runFetch logged it before phase E.
```

with

```go
		// AIDEV-NOTE: the retry is one more singleflight call: requests
		// that waited together coalesce again, and a waiter over the
		// 64-per-host cap, or with less than 1 s of its 25 s budget left,
		// is refused at once. A client that left while it waited gets no
		// retry (it ends client_canceled). The refusal is logged once per
		// fetch, by the singleflight leader (design review O1), as
		// runFetch logged it before phase E. Two accepted residuals: the
		// retry can re-join the call it retries — its leader signals Done
		// before it deletes the key, deliberately — and read that call's
		// stale errUnvouchedRetryable, at worst one spurious refusal, the
		// outcome before phase E (deferred minor E-57); and the retry's
		// call replaces shared and waiters, so the first call's
		// "singleflight coalesced" line is not logged and a joiner
		// refused off the first call logs no unvouched_deb_refused of its
		// own (log attribution only; deferred minor E-60).
```

In `internal/handler/handler.go`, replace

```go
	// contact. Only the creating request fires (a coalesced waiter shares
	// its result), and only with adoption enabled: without it a created
	// row is the Phase 1 observation seed (TestServeHTTP_AdoptionDisabledSeedParity).
	// The hit path fires T1 after onboarding already (ruling PD-11).
```

with

```go
	// contact. Only the creating request fires (a coalesced waiter shares
	// its result), and only with adoption enabled: without it a created
	// row is the Phase 1 observation seed (TestServeHTTP_AdoptionDisabledSeedParity).
	// The hit path fires T1 after onboarding already (ruling PD-11). A
	// creating miss whose late registration finds a snapshot current (it
	// waited in a hold, or a flip committed just before it) is served
	// through trySnapshotHit, which fires T1 too: the seed T1 then is a
	// cooldown skip, one mutex peek (deferred minor E-64).
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/handler/ -run 'TestReadCachedAnchor|TestSeedRejections_Expire|TestServeHTTP_AMissThatFindsTheRow|TestMissBackstopDeadline_|TestServeHTTP_OnboardingBackstopStops|TestServeHTTP_StrictCanceledBetween|TestServeHTTP_BackstopIsBounded' -v`
Expected: PASS.

Run: `go test ./internal/handler/`
Expected: PASS (the refactor changes no behaviour: `TestServeHTTP_CreatingMissMarksCachedSignedAnchors`, `TestServeHTTP_OnboardingBackstopMarksAChangedAnchor`, `TestServeHTTP_AdoptionDisabledSeedParity` and the hit-path invariant test pin it).

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| `readCachedAnchor` calls every blob signed | `TestReadCachedAnchor` (`/r/dists/p/InRelease: …, want signed=false`), `TestServeHTTP_HitOnboardsALegacyAnchor` (`q: want onboarded through Release, with no marker for its plain-text InRelease`), `TestServeHTTP_CreatingMissMarksCachedSignedAnchors` |
| `readCachedAnchor` swallows a pool read error | `TestReadCachedAnchor` (`a missing pool file: …, want the row, unsigned, ErrNotExist`) |
| `expire` without its recover | `TestSeedRejections_ExpireRecoversItsPanic` (`panic: sweep boom [recovered, repanicked]`) |
| `New` accepts a negative `MaxSuites` | `TestSeedRejections_ExpireRecoversItsPanic` (`New(MaxSuites: -1) = <nil>, want handler: negative MaxSuites`) |
| `created` widened to a row found (`\|\| … == cache.SuiteExists`) | `TestServeHTTP_AMissThatFindsTheRowCreatedFiresNoSeedT1` (`T1 calls [{http 127.0.0.1 /r/dists/s} {http 127.0.0.1 /r/dists/s}], want exactly one …`) |
| a floor at now in `missBackstopDeadline` | `TestMissBackstopDeadline_APastBudgetIsPast` (`deadline 1m0.0…s after start, want 25s (the budget, already past)`) |
| onboarding's backstop on `context.Background()` | `TestServeHTTP_OnboardingBackstopStopsAtShutdown` (`status 200 after 5s, backstop ran true; want 200 at once, …`) |
| `case strictCanceled` falls through to the serve | `TestServeHTTP_StrictCanceledBetweenTheLookups` (`status 200 body "an unvouched .deb, cached", want 503 and no .deb`) |
| `runFetch`'s creation without its cached anchors | `TestServeHTTP_CreatingMissMarksCachedSignedAnchors` (`Release miss = 200; want the row created and its cached Release.gpg marked`) |
| `runFetch` drops the re-stamp | `TestServeHTTP_AdoptionDisabledSeedParity` (`after the refresh miss check=… success=…, want the second miss's second …`) |

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/handler/suite_rows.go internal/handler/handler.go internal/handler/handler_minors_test.go internal/handler/suite_rows_test.go
git add internal/handler/suite_rows.go internal/handler/handler.go internal/handler/handler_minors_test.go internal/handler/suite_rows_test.go
git commit -m "Handler deferred minors: D-31, D-32, E-57, E-60, E-64, E-65, E-67, E-68, E-71"
```

(Record the mutation table in the commit message.)

### Task 9: Dispatch 3 for flat suites: client-created index rows

Spec C4 Dispatch 3 (the E carry-forward; design note 8): eager first adoption runs only for a suite with a client-created `url_path` row (`request_count > 0`, never reconstructed), and phase E counted only `InRelease` and `Release` rows (`cachedAnchors`, its design note 14). A Release-less flat repository's suite is created by its index's serve (spec C3), so a flat suite's root `Packages*`/`Sources*` rows (`aptpath.FileFlatIndex`) now count too — read only when no anchor row is client-created, from one range read of the suite's url_path rows (`cache.ListSuiteMetadataRows`, the rows `DeleteSuiteMetadata` deletes), which Task 10's anchorless refresh reuses. A row no client fetched (hot prefetch, a reconstruction), a flat `Contents-*` and a `dists/` suite's root `Packages` count for nothing. A signed flat suite's first check adopts its flat layout end to end (C6's rows at its `FlatBase`), for the suite and its `./` twin. `first_adoption.go`'s open deferred minors: E-32 (its predicate's helper and its other five sites are in `check.go`, `current_form.go`) and E-24 (H) are checked and left; F-8 sits in `first_adoption_test.go`, which this phase does not change.

**Files:**
- Create: `internal/cache/anchorless.go` (`ListSuiteMetadataRows`), `internal/cache/anchorless_test.go`, `internal/freshness/anchorless.go` (`suiteKeyOf`, `flatIndexClientCreated`), `internal/freshness/flat_rig_test.go`, `internal/freshness/flat_first_adoption_test.go`
- Modify: `internal/freshness/first_adoption.go` (`checkFirst`'s client-created rule and its doc; the `aptpath` import)

**Interfaces:**
- Consumes: `aptpath.IsCanonicalSuitePath`, `aptpath.IsFlatSuitePath`, `aptpath.ClassifySuiteFile` (Task 1); `packageHashRows` (Task 4); `openCache`, `seedBlob`, `putURLPathRow`, `sk` (cache tests); `rigUpstream`, `rigVerifier`, `rigClock`, `lockedBuffer`, `fakeFetcher`, `fakePackagesStanzas`, `makeRelease`, `testHex64`, `discardLogger`, `simtest.ClearsignedFixture` (freshness tests).
- Produces: `(*cache.Cache).ListSuiteMetadataRows(ctx, key cache.SuiteKey) ([]cache.URLPath, error)`; `(*Adopter).flatIndexClientCreated(ctx, suite SuiteRef) (bool, error)`, `suiteKeyOf(SuiteRef) cache.SuiteKey`; the freshness test rig `flatRig` (`newFlatRig`, `url`, `cacheRow`, `check`, `row`, `requests`, `lastResult`), `poolBlob`, `flatRigHost`, `flatGz` — used by Tasks 10, 12, 13 and 15.

- [ ] **Step 1: Write the failing tests**

Create `internal/cache/anchorless_test.go`:

```go
package cache

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
)

// listedPaths returns the paths of key's suite metadata rows.
func listedPaths(t *testing.T, c *Cache, key SuiteKey) []string {
	t.Helper()
	rows, err := c.ListSuiteMetadataRows(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		out = append(out, r.Path)
	}
	return out
}

// Spec C4 (Dispatch 3, the anchorless refresh): ListSuiteMetadataRows
// lists exactly the rows DeleteSuiteMetadata would delete — the suite's
// own anchors, indexes, Contents, by-hash names and pdiff Index, in path
// order, with their columns — and nothing of its "./" twin, a nested or
// sibling suite, its .debs, or a file no grammar maps to it. A key no
// grammar produces lists nothing.
func TestListSuiteMetadataRows_TheSuitesRowsOnly(t *testing.T) {
	c := openCache(t)
	blob := seedBlob(t, c, "flat metadata")
	hex := strings.Repeat("ab", 32)
	mine := []string{"/flat/Contents-amd64.gz", "/flat/InRelease", "/flat/Packages.diff/Index", "/flat/Packages.gz",
		"/flat/by-hash/SHA256/" + hex}
	others := []string{"/flat/./Packages.gz", "/flat/amd64/x_1_amd64.deb", "/flat/sub/Packages", "/flat2/Packages",
		"/flat/Translation-en"}
	for _, p := range append(append([]string{}, mine...), others...) {
		putURLPathRow(t, c, "http", "h", p, blob, sql.NullInt64{Int64: 1_700_000_000, Valid: true}, true)
	}
	putURLPathRow(t, c, "https", "h", "/flat/Packages", blob, sql.NullInt64{}, true) // the other scheme's
	if got := listedPaths(t, c, sk("h", "/flat")); !slices.Equal(got, mine) {
		t.Fatalf("ListSuiteMetadataRows(/flat) = %v, want %v", got, mine)
	}
	rows, err := c.ListSuiteMetadataRows(context.Background(), sk("h", "/flat"))
	if err != nil {
		t.Fatal(err)
	}
	if r := rows[3]; r.Path != "/flat/Packages.gz" || r.BlobHash == nil || *r.BlobHash != blob || r.RequestCount != 1 ||
		r.LastRequestedAt == nil || *r.LastRequestedAt != 1_700_000_000 || !r.IsMetadata || r.UpstreamURL != "http://h/flat/Packages.gz" {
		t.Fatalf("row = %+v, want its columns", r)
	}
	if got, want := listedPaths(t, c, sk("h", "/flat/.")), []string{"/flat/./Packages.gz"}; !slices.Equal(got, want) {
		t.Fatalf("ListSuiteMetadataRows(/flat/.) = %v, want %v", got, want)
	}
	for _, key := range []string{"/flat/./.", ""} { // the empty key's range is the whole host
		if got := listedPaths(t, c, sk("h", key)); got != nil {
			t.Fatalf("ListSuiteMetadataRows(%q), a key no grammar produces, = %v, want nothing", key, got)
		}
	}
}
```

Create `internal/freshness/flat_rig_test.go`:

```go
package freshness

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/observability"
)

// flatRigHost is the flat rig's upstream host.
const flatRigHost = "fh"

// flatRig is retryRig for a flat suite: a real cache, Adopter and Checker
// over suite on host fh, the checker's probes and conditional GETs
// answered by a rigUpstream (200 with an ETag, 304 to it, 404 for a URL
// without a body), the adopter's members by a fakeFetcher. The suite has a
// row whose interest stamp is the rig clock's now; no url_path row is
// seeded: each test caches what its clients fetched (cacheRow). The clock
// starts at 1_800_000_000 and moves only when the test advances it.
type flatRig struct {
	t       *testing.T
	cache   *cache.Cache
	adopter *Adopter
	checker *Checker
	members *fakeFetcher
	up      *rigUpstream
	ver     *rigVerifier
	clock   *rigClock
	logs    *lockedBuffer
	suite   SuiteRef
}

func newFlatRig(t *testing.T, suitePath string) *flatRig {
	t.Helper()
	ctx := context.Background()
	c, err := cache.Open(ctx, t.TempDir(), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	r := &flatRig{
		t: t, cache: c, members: newFakeFetcher(),
		up:    &rigUpstream{bodies: map[string][]byte{}, sent: map[string][]string{}},
		ver:   &rigVerifier{rejected: map[string]bool{}, panics: map[string]bool{}, entered: make(chan struct{}, 1)},
		clock: &rigClock{t: time.Unix(1_800_000_000, 0)},
		logs:  &lockedBuffer{},
		suite: SuiteRef{CanonicalScheme: "http", CanonicalHost: flatRigHost, SuitePath: suitePath},
	}
	interest := int64(1_800_000_000)
	if err := c.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: flatRigHost,
		SuitePath: suitePath, LastRequestedAt: &interest}); err != nil {
		t.Fatal(err)
	}
	r.adopter, err = NewAdopter(AdoptionConfig{Cache: c, Fetcher: r.members, Verifier: r.ver,
		HostLimiter: hostsem.New(8), MaxConcurrent: 2, Logger: discardLogger(), now: r.clock.now})
	if err != nil {
		t.Fatal(err)
	}
	lifeCtx, cancel := context.WithCancel(ctx)
	r.checker, err = New(Config{Cache: c, Fetcher: r.up, HostLimiter: hostsem.New(8),
		Refresh: 15 * time.Minute, Adopter: r.adopter, LifetimeCtx: lifeCtx, AdoptionRing: observability.NewRing(50),
		Logger: slog.New(slog.NewJSONHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		now:    r.clock.now, rand: func() float64 { return 0.5 }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); r.checker.WaitForAdoptions() })
	return r
}

// url is the upstream URL of the suite's file name.
func (r *flatRig) url(name string) string {
	return "http://" + flatRigHost + r.suite.SuitePath + "/" + name
}

// poolBlob writes body into c's pool (with its blob row) and returns its
// hash.
func poolBlob(t *testing.T, c *cache.Cache, body []byte) string {
	t.Helper()
	w, err := c.NewTempBlob()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	h, err := w.Finalize(int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.PutBlob(context.Background(), h, int64(len(body))); err != nil {
		t.Fatal(err)
	}
	return h
}

// cacheRow stores body as the url_path row of the suite's file name, as a
// client's miss stored it: requests is its request_count (0: written by
// no client), fetched its last_fetched_at, etag its upstream_etag ("" for
// none). It returns the blob's hash.
func (r *flatRig) cacheRow(name string, body []byte, requests, fetched int64, etag string) string {
	r.t.Helper()
	h := poolBlob(r.t, r.cache, body)
	u := cache.URLPath{CanonicalScheme: "http", CanonicalHost: flatRigHost, Path: r.suite.SuitePath + "/" + name,
		BlobHash: &h, UpstreamURL: r.url(name), IsMetadata: true, RequestCount: requests, LastFetchedAt: &fetched}
	if requests > 0 {
		u.LastRequestedAt = &fetched
	}
	if etag != "" {
		u.UpstreamETag = &etag
	}
	if err := r.cache.PutURLPath(context.Background(), u); err != nil {
		r.t.Fatal(err)
	}
	return h
}

// check runs one freshness check of the suite and waits for any adoption
// it started.
func (r *flatRig) check() {
	r.checker.Check(context.Background(), "http", flatRigHost, r.suite.SuitePath)
	r.checker.WaitForAdoptions()
}

func (r *flatRig) row() cache.SuiteFreshness {
	r.t.Helper()
	s, err := r.cache.GetSuiteFreshness(context.Background(), "http", flatRigHost, r.suite.SuitePath)
	if err != nil {
		r.t.Fatal(err)
	}
	return *s
}

// requests is how many GETs the checker sent for the suite's file name.
func (r *flatRig) requests(name string) int { return len(r.up.etagsSent(r.url(name))) }

// lastResult is the result of the rig's last check line: a
// freshness_check line's result, "failed" for a failed check's line, ""
// for none.
func (r *flatRig) lastResult() string {
	var res string
	for _, line := range strings.Split(r.logs.String(), "\n") {
		var m map[string]any
		if line == "" || json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		switch m["msg"] {
		case "freshness_check":
			res, _ = m["result"].(string)
		case "freshness check failed":
			res = "failed"
		}
	}
	return res
}
```

Create `internal/freshness/flat_first_adoption_test.go`:

```go
package freshness

import (
	"context"
	"maps"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// flatGz is a body that passes the flat index sniff for a .gz name: the
// gzip magic and a header.
var flatGz = []byte{0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03}

// Spec C4 Dispatch 3 (the E carry-forward; plan design note 8): a flat
// suite's client-created root Packages* or Sources* row is evidence of
// interest beside the InRelease and Release rows, so a Release-less
// repository's suite — created by its index's serve — runs the probe tree
// on its first check. One whose only row is an index no client fetched (a
// prefetch or a reconstruction: request_count 0), a flat Contents-*, or —
// under a dists suite — a root Packages, stays dormant: no request.
func TestCheck_FlatIndexRowsCountAsClientCreated(t *testing.T) {
	for _, tc := range []struct {
		name, suite, file string
		requests          int64
		probes            bool
	}{
		{"a client-fetched Packages.gz", "/fury", "Packages.gz", 1, true},
		{"a client-fetched Sources.gz", "/fury", "Sources.gz", 3, true},
		{"its ./ twin's Packages.gz", "/fury/.", "Packages.gz", 1, true},
		{"an index no client fetched", "/fury", "Packages.gz", 0, false},
		{"a flat Contents-*", "/fury", "Contents-amd64.gz", 1, false},
		{"a dists suite's root Packages.gz", "/r/dists/x", "Packages.gz", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newFlatRig(t, tc.suite)
			r.cacheRow(tc.file, flatGz, tc.requests, 1_799_999_000, "")
			r.check()
			if got := r.requests("InRelease") > 0; got != tc.probes {
				t.Fatalf("InRelease probed = %v, want %v", got, tc.probes)
			}
		})
	}
}

// Spec C4 Dispatch 3 for a signed flat suite: its client-created InRelease
// makes it interested, and its first check adopts (eager first adoption)
// with the flat layout's members — the root Packages fetched under the
// suite — and C6's rows at its FlatBase. The `./` twin adopts its own
// snapshot the same way (ruling R-G4).
func TestCheck_AFlatSuiteAdoptsOnItsFirstCheck(t *testing.T) {
	for _, suitePath := range []string{"/core:/stable:/v1.34/deb", "/core:/stable:/v1.34/deb/."} {
		t.Run(suitePath, func(t *testing.T) {
			r := newFlatRig(t, suitePath)
			pkgs := fakePackagesStanzas(map[string]string{"amd64/kubectl_1.34.1-1.1_amd64.deb": testHex64})
			rel, _ := makeRelease(map[string][]byte{"Packages": pkgs})
			in := simtest.ClearsignedFixture(string(rel))
			r.cacheRow("InRelease", in, 1, 1_799_999_000, "")
			r.up.setAt(r.url("InRelease"), in)
			r.members.put(r.url("Packages"), pkgs)
			r.check()
			row := r.row()
			if r.lastResult() != "first_adoption" || row.CurrentSnapshotID == nil {
				t.Fatalf("result %q, current %v; want a first adoption", r.lastResult(), row.CurrentSnapshotID)
			}
			got := packageHashRows(t, r.cache, *row.CurrentSnapshotID)
			want := map[string]string{"/core:/stable:/v1.34/deb/amd64/kubectl_1.34.1-1.1_amd64.deb": "amd64/kubectl_1.34.1-1.1_amd64.deb"}
			if !maps.Equal(got, want) {
				t.Fatalf("package_hash rows = %v, want %v", got, want)
			}
			if rows, err := r.cache.DeclaredHashesForPath(context.Background(), "http", flatRigHost,
				"/core:/stable:/v1.34/deb/amd64/kubectl_1.34.1-1.1_amd64.deb"); err != nil || len(rows) != 1 || rows[0].SuitePath != suitePath {
				t.Fatalf("the .deb is vouched by %v (%v), want one row naming %s", rows, err, suitePath)
			}
		})
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -gcflags=-e ./internal/cache/`
Expected: FAIL — `c.ListSuiteMetadataRows undefined (type *Cache has no field or method ListSuiteMetadataRows)` (twice).

Run: `go test ./internal/freshness/ -run 'TestCheck_FlatIndexRowsCountAsClientCreated|TestCheck_AFlatSuiteAdoptsOnItsFirstCheck'`
Expected: FAIL — `TestCheck_FlatIndexRowsCountAsClientCreated` (`InRelease probed = false, want true`, for the client-fetched `Packages.gz`, `Sources.gz` and the twin's `Packages.gz`: their suites stayed dormant). `TestCheck_AFlatSuiteAdoptsOnItsFirstCheck` passes (Tasks 1–4 built it; it pins the end-to-end shape).

- [ ] **Step 3: Implement**

Create `internal/cache/anchorless.go`:

```go
package cache

import (
	"context"
	"fmt"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
)

// ListSuiteMetadataRows returns key's suite metadata url_path rows, in
// path order: the rows of key's scheme and host whose aptpath.SuitePath is
// the suite — never a nested suite's, a prefix sibling's or the "/." twin's,
// as suiteMetadataPaths reads them for DeleteSuiteMetadata. A key no
// grammar produces (aptpath.IsCanonicalSuitePath) has none. Spec C4:
// Dispatch 3 reads a flat suite's client-created index rows from it, and
// the anchorless refresh revalidates them.
//
// AIDEV-NOTE: one range read of the url_path primary key over the suite's
// directory; for a flat suite that range also holds its cached .debs,
// which the SuitePath filter drops. Dispatch 3 reads it only for a flat
// suite with no client-created anchor row (a Release-less repository), and
// the anchorless refresh once per check.
func (c *Cache) ListSuiteMetadataRows(ctx context.Context, key SuiteKey) ([]URLPath, error) {
	if !aptpath.IsCanonicalSuitePath(key.SuitePath) {
		return nil, nil
	}
	rows, err := c.db.QueryContext(ctx, `
SELECT canonical_scheme, canonical_host, path, blob_hash, upstream_url,
       is_metadata, last_requested_at, request_count, last_fetched_at,
       upstream_etag, upstream_lastmod
  FROM url_path
 WHERE canonical_scheme = ? AND canonical_host = ? AND path >= ? AND path < ?
 ORDER BY path`, key.CanonicalScheme, key.CanonicalHost, key.SuitePath+"/", key.SuitePath+"0")
	if err != nil {
		return nil, fmt.Errorf("ListSuiteMetadataRows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []URLPath
	for rows.Next() {
		var (
			u    URLPath
			isMD int64
		)
		if err := rows.Scan(&u.CanonicalScheme, &u.CanonicalHost, &u.Path, &u.BlobHash, &u.UpstreamURL,
			&isMD, &u.LastRequestedAt, &u.RequestCount, &u.LastFetchedAt, &u.UpstreamETag, &u.UpstreamLastMod); err != nil {
			return nil, fmt.Errorf("ListSuiteMetadataRows: scan: %w", err)
		}
		if aptpath.SuitePath(u.Path) != key.SuitePath {
			continue
		}
		u.IsMetadata = isMD != 0
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListSuiteMetadataRows: %w", err)
	}
	return out, nil
}
```

Create `internal/freshness/anchorless.go`:

```go
package freshness

// Spec C4 for flat suites: Dispatch 3's client-created index rows, and
// (Dispatch 4) the anchorless refresh of a flat, Release-less suite.

import (
	"context"
	"fmt"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// suiteKeyOf is the cache key of suite.
func suiteKeyOf(suite SuiteRef) cache.SuiteKey {
	return cache.SuiteKey{CanonicalScheme: suite.CanonicalScheme, CanonicalHost: suite.CanonicalHost, SuitePath: suite.SuitePath}
}

// flatIndexClientCreated reports whether flat suite has a client-created
// root Packages* or Sources* url_path row (aptpath.FileFlatIndex,
// request_count > 0): spec C4 Dispatch 3 counts it beside the InRelease
// and Release rows, so a Release-less repository's suite — created by its
// index's serve — is not dormant (the E carry-forward; plan design note 8).
// A row hot prefetch or a reconstruction wrote has no request count.
func (a *Adopter) flatIndexClientCreated(ctx context.Context, suite SuiteRef) (bool, error) {
	rows, err := a.cache.ListSuiteMetadataRows(ctx, suiteKeyOf(suite))
	if err != nil {
		return false, fmt.Errorf("read flat index rows: %w", err)
	}
	for _, r := range rows {
		if r.RequestCount > 0 && aptpath.ClassifySuiteFile(suite.SuitePath, r.Path) == aptpath.FileFlatIndex {
			return true, nil
		}
	}
	return false, nil
}
```

In `internal/freshness/first_adoption.go`, replace

```go
// checkFirst is spec C4 Dispatch 3 (adopter wired, no current snapshot).
// A suite without client interest in the last 7 days, or without a
// client-created InRelease or Release url_path row, is dormant: no request
// and no write (the scheduler does not queue it either, see suiteDue). A
```

with

```go
// checkFirst is spec C4 Dispatch 3 (adopter wired, no current snapshot).
// A suite without client interest in the last 7 days, or without a
// client-created InRelease or Release url_path row — or, for a flat suite,
// root Packages*/Sources* row (Adopter.flatIndexClientCreated) — is
// dormant: no request and no write (the scheduler does not queue it
// either, see suiteDue). A
```

In `internal/freshness/first_adoption.go`, replace

```go
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
```

with

```go
	rows, clientCreated, err := c.cachedAnchors(ctx, suite)
	if err == nil && !clientCreated && aptpath.IsFlatSuitePath(suitePath) {
		clientCreated, err = c.adopter.flatIndexClientCreated(ctx, suite)
	}
	if err != nil {
		c.logger.Warn("freshness: read anchor rows failed",
			"err", err, "canonical_host", host, "suite_path", suitePath)
		return nil
	}
	if !clientCreated {
		c.logger.Debug("freshness: never-adopted suite without a client-created anchor or flat index row, dormant",
			"canonical_host", host, "suite_path", suitePath)
		return nil
	}
```

In `internal/freshness/first_adoption.go`, replace

```go
	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)
```

with

```go
	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/cache/ ./internal/freshness/`
Expected: PASS.

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| an index no client fetched counts (no `RequestCount > 0`) | `TestCheck_FlatIndexRowsCountAsClientCreated/an_index_no_client_fetched` (`InRelease probed = true, want false`) |
| any metadata row with requests counts (no `FileFlatIndex` test) | `TestCheck_FlatIndexRowsCountAsClientCreated/a_flat_Contents-*` (`InRelease probed = true, want false`) |
| `checkFirst` without the flat index rows | `TestCheck_FlatIndexRowsCountAsClientCreated/a_client-fetched_Packages.gz`, `/a_client-fetched_Sources.gz`, `/its_./_twin's_Packages.gz` (`InRelease probed = false, want true`) |
| `ListSuiteMetadataRows` without its `SuitePath` filter | `TestListSuiteMetadataRows_TheSuitesRowsOnly` (`ListSuiteMetadataRows(/flat) = [/flat/./Packages.gz /flat/Contents-amd64.gz … /flat/amd64/x_1_amd64.deb …], want …`) |
| `ListSuiteMetadataRows` without its canonical-key guard | `TestListSuiteMetadataRows_TheSuitesRowsOnly` (`ListSuiteMetadataRows(""), a key no grammar produces, = [/flat/Translation-en /flat/amd64/x_1_amd64.deb], want nothing`) |

Equivalent mutant (recorded): reading the flat index rows for any suite, not only a flat one — `ClassifySuiteFile` never names a `dists/` suite's root index `FileFlatIndex`, so the `IsFlatSuitePath` test only saves a `dists/` suite the range read.

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/cache/anchorless.go internal/cache/anchorless_test.go internal/freshness/anchorless.go internal/freshness/first_adoption.go internal/freshness/flat_rig_test.go internal/freshness/flat_first_adoption_test.go
git add internal/cache/anchorless.go internal/cache/anchorless_test.go internal/freshness/anchorless.go internal/freshness/first_adoption.go internal/freshness/flat_rig_test.go internal/freshness/flat_first_adoption_test.go
git commit -m "Count a flat suite's client-created index rows for eager first adoption (spec C4 Dispatch 3)"
```

(Record the mutation table in the commit message.)

### Task 10: The anchorless gate and refresh (spec C4 Dispatch 3–4) and url_path `Last-Modified` (ruling R-G3)

Spec C4 Dispatch 3 sends a flat suite whose probe tree finds neither `InRelease` nor `Release`, and that has no signed evidence, to the anchorless refresh (Dispatch 4); every other suite keeps today's failed check and `anchor_absent` record. The gate (design note 9, deviation 1) is the cached signed-anchor re-mark (`remarkSignedAnchors`) followed by never-downgrade's rule (`cache.CheckUnsignedDowngrade`); an error fails the check closed. The refresh (design notes 10–13, deviation 2) revalidates each cached `Packages*`/`Sources*`/`Contents-*` row of the suite with one conditional GET carrying its stored validators, under the check's host slot and deadline; writes go through two new cache methods — `RefreshURLPathBlob` (spec C4's "dedicated cache method": blob and validators, `last_fetched_at` only when the blob changes, `MAX(now, previous + 1)`, never the interest columns) and `DeleteURLPathRows` — and through the adopter's `*cache.Cache` (the checker's `Cache` interface stays the subset its fakes implement). An answered refresh is a successful check (`anchorless_refreshed` or `anchorless_unchanged`, form `anchorless`) that clears the failure record and the pending change; with no answer at all it is a failed check. A 304 to a request without validators is unknown, as `classify` treats it. The handler's url_path metadata serves (hit, miss, stale) answer `Last-Modified = max(pool mtime, last_fetched_at)` (design note 14, `urlPathModTime`); ruling R-G3 keeps `runFetch`'s finalize-before-stamp order, pinned by `TestServeHTTP_AMissStampsItsRowAfterItsPoolFile` and AIDEV-NOTEs at `runFetch`, `adoptionStampTx`, `RefreshURLPathBlob` and `urlPathModTime`; blob mtimes are not folded into `adoptionStampTx`'s floor. Deferred minor E-47 (`retry_rig_test.go`, which this task changes) is taken: `rigStart` and `rigVerifier.resetCalls`.

**Files:**
- Create: `internal/freshness/anchorless_test.go`, `internal/handler/urlpath_lastmod_test.go`
- Modify: `internal/aptpath/sniff.go` (`IsAnchorlessIndex`, `SniffAnchorlessIndex`), `internal/aptpath/sniff_test.go`, `internal/cache/anchorless.go` (`RefreshURLPathBlob`, `DeleteURLPathRows`), `internal/cache/anchorless_test.go`, `internal/cache/adoption.go` (R-G3 AIDEV-NOTE at `adoptionStampTx`), `internal/freshness/anchorless.go` (the gate and the refresh), `internal/freshness/first_adoption.go` (`checkFirst`'s `!t.found` branch and doc), `internal/freshness/metrics.go` (`acu_anchorless_sniff_rejected_total`), `internal/freshness/retry_rig_test.go` (`rigUpstream.before`; E-47), `internal/freshness/flat_rig_test.go` (E-47's `rigStart`), `internal/handler/handler.go` (`urlPathModTime` at the three serves; R-G3 AIDEV-NOTE in `runFetch`), `internal/handler/singleflight.go` (`sfResult.fetchedAt`)

**Interfaces:**
- Consumes: `cache.ListSuiteMetadataRows`, `suiteKeyOf`, the flat rig (Task 9); `Adopter.remarkSignedAnchors`, `Adopter.writeBlobBytes`, `cache.CheckUnsignedDowngrade`, `(*cache.Cache).deleteURLPathRows`, `probeTree`, `finishCheck`, `failCheck`, `endedByShutdown`, `nonEmpty`, `errUnexpectedStatus`, `anchorAbsentKey`, `armoredSig`, `aptpath.SniffFlatIndex`; handler tests' `lastModifiedGet`, `httpTime`, `commitAnchor`, `adoptedAt`, `lmSuite`, `writeBlob`, `staticUpstream`, `slowUpstream`, `flatPackagesGz` (Task 7), `newTestHandlerWithServe`, `newProxyReqHostPort`.
- Produces: `aptpath.IsAnchorlessIndex(name string) bool`, `aptpath.SniffAnchorlessIndex(name string, head []byte) (pass, sniffable bool)`; `(*cache.Cache).RefreshURLPathBlob(ctx, scheme, host, path, old, newHash string, etag, lastmod *string) (bool, error)`, `(*cache.Cache).DeleteURLPathRows(ctx, scheme, host string, paths []string) (int, error)`; `(*Adopter).anchorlessEligible`, `(*Checker).anchorlessRefresh`, `(*Checker).refreshAnchorlessRow`, `maxAnchorlessIndexBytes`, the metric `acu_anchorless_sniff_rejected_total{host}`, the `freshness_check` results `anchorless_refreshed` and `anchorless_unchanged` (form `anchorless`), the failed check's form `anchorless`, WARN `freshness: anchorless index over its size cap` (`max_bytes`) and WARN `freshness: delete anchorless rows failed`; handler `urlPathModTime`, `sfResult.fetchedAt`; test helpers `gzBody`, `sniffRejected`, `flatRig.lookup`, `rigUpstream.before`, `rigStart`, `rigVerifier.resetCalls`, `cacheURLPath` — Tasks 11, 12, 15 and 16 use them.

- [ ] **Step 1: Write the failing tests**

Append to `internal/aptpath/sniff_test.go`:

```go
// Spec C4 Dispatch 4 (plan design note 11, deviation 2): the anchorless
// refresh revalidates a flat suite's root Packages*, Sources* and
// Contents-* members (index). Packages* and Sources* take the flat index
// sniff, a compressed Contents-* its extension's magic; a plain
// Contents-* has no sniff (sniffable false), and nothing else is an
// anchorless index.
func TestSniffAnchorlessIndex(t *testing.T) {
	gz := "\x1f\x8b\x08\x00"
	for _, c := range []struct {
		name, head             string
		index, pass, sniffable bool
	}{
		{"Packages", "Package: a\n", true, true, true},
		{"Packages", "<html>", true, false, true},
		{"Packages.gz", gz, true, true, true},
		{"Packages.gz", "<html>", true, false, true},
		{"Sources.xz", "\xfd7zXZ\x00", true, true, true},
		{"Sources", "Package: a\n", true, true, true},
		{"Contents-amd64.gz", gz, true, true, true},
		{"Contents-amd64.gz", "<html>", true, false, true},
		{"Contents-udeb-amd64.zst", "\x28\xb5\x2f\xfd", true, true, true},
		{"Contents-amd64", "bin/ls utils/coreutils\n", true, false, false},
		{"Contents-amd64", "<html>", true, false, false},
		{"Contents-AMD64.gz", gz, false, false, false},
		{"Contents-amd64.zip", gz, false, false, false},
		{"Packages.diff/Index", "SHA256-Current: x\n", false, false, false},
		{"InRelease", "-----BEGIN PGP SIGNED MESSAGE-----", false, false, false},
		{"Release", "Origin: x\n", false, false, false},
		{"Translation-en", "Package: a\n", false, false, false},
		{"Packages.bz3", "BZh", false, false, false},
	} {
		pass, sniffable := SniffAnchorlessIndex(c.name, []byte(c.head))
		if pass != c.pass || sniffable != c.sniffable {
			t.Errorf("SniffAnchorlessIndex(%q, %q) = %v, %v; want %v, %v", c.name, c.head, pass, sniffable, c.pass, c.sniffable)
		}
		if got := IsAnchorlessIndex(c.name); got != c.index {
			t.Errorf("IsAnchorlessIndex(%q) = %v, want %v", c.name, got, c.index)
		}
	}
}
```

In `internal/cache/anchorless_test.go`, add `"errors"` to the imports (after `"database/sql"`) and append:

```go
// putFetchedRow stores the url_path row of host h at path with blob, as a
// client's miss stored it: request_count 3, last_requested_at
// 1_700_000_000, last_fetched_at fetched, upstream_etag "e1".
func putFetchedRow(t *testing.T, c *Cache, path, blob string, fetched int64) {
	t.Helper()
	requested, etag := int64(1_700_000_000), "e1"
	if err := c.PutURLPath(context.Background(), URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: path,
		BlobHash: &blob, UpstreamURL: "http://h" + path, IsMetadata: true, LastRequestedAt: &requested,
		RequestCount: 3, LastFetchedAt: &fetched, UpstreamETag: &etag}); err != nil {
		t.Fatal(err)
	}
}

func lookupRow(t *testing.T, c *Cache, path string) *URLPath {
	t.Helper()
	u, err := c.LookupURL(context.Background(), "http", "h", path)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func strOf(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// Spec C4 Dispatch 4's dedicated cache method: RefreshURLPathBlob points
// a row at its new blob with the response's validators, keeps
// last_requested_at and request_count, and moves last_fetched_at to
// MAX(now, previous + 1) — only when the blob changes (plan design note
// 13). A row whose blob is no longer the one the caller read is left
// alone, and a malformed hash is refused.
func TestRefreshURLPathBlob_ReplacesTheBlobAndKeepsTheInterest(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	old, newer := seedBlob(t, c, "Package: a\n"), seedBlob(t, c, "Package: b\n")
	defer stubNow(t, 1_800_000_000)()
	etag, lastmod := "e2", "Tue, 15 Nov 1994 08:12:31 GMT"

	putFetchedRow(t, c, "/flat/Packages", old, 1_799_999_000)
	if ok, err := c.RefreshURLPathBlob(ctx, "http", "h", "/flat/Packages", old, newer, &etag, &lastmod); err != nil || !ok {
		t.Fatalf("RefreshURLPathBlob = %v, %v; want true", ok, err)
	}
	u := lookupRow(t, c, "/flat/Packages")
	if *u.BlobHash != newer || strOf(u.UpstreamETag) != etag || strOf(u.UpstreamLastMod) != lastmod ||
		u.RequestCount != 3 || *u.LastRequestedAt != 1_700_000_000 || *u.LastFetchedAt != 1_800_000_000 {
		t.Fatalf("row = %+v; want the new blob and validators, the interest kept, last_fetched_at now", u)
	}

	// Within the second of the previous fetch: previous + 1.
	putFetchedRow(t, c, "/flat/Sources", old, 1_800_000_000)
	if ok, err := c.RefreshURLPathBlob(ctx, "http", "h", "/flat/Sources", old, newer, nil, nil); err != nil || !ok {
		t.Fatalf("RefreshURLPathBlob = %v, %v; want true", ok, err)
	}
	if u := lookupRow(t, c, "/flat/Sources"); *u.LastFetchedAt != 1_800_000_001 || u.UpstreamETag != nil || u.UpstreamLastMod != nil {
		t.Fatalf("row = %+v; want last_fetched_at 1800000001 and no validators", u)
	}

	// The same blob: the validators only.
	putFetchedRow(t, c, "/flat/Packages.gz", old, 1_799_999_000)
	if ok, err := c.RefreshURLPathBlob(ctx, "http", "h", "/flat/Packages.gz", old, old, &etag, nil); err != nil || !ok {
		t.Fatalf("RefreshURLPathBlob = %v, %v; want true", ok, err)
	}
	if u := lookupRow(t, c, "/flat/Packages.gz"); *u.LastFetchedAt != 1_799_999_000 || strOf(u.UpstreamETag) != etag {
		t.Fatalf("row = %+v; want last_fetched_at kept and the new ETag", u)
	}

	// A row that changed since it was read is left alone.
	putFetchedRow(t, c, "/flat/Packages.xz", newer, 1_799_999_000)
	if ok, err := c.RefreshURLPathBlob(ctx, "http", "h", "/flat/Packages.xz", old, newer, &etag, nil); err != nil || ok {
		t.Fatalf("RefreshURLPathBlob of a changed row = %v, %v; want false", ok, err)
	}
	if u := lookupRow(t, c, "/flat/Packages.xz"); strOf(u.UpstreamETag) != "e1" || *u.LastFetchedAt != 1_799_999_000 {
		t.Fatalf("a changed row was written: %+v", u)
	}
	if ok, err := c.RefreshURLPathBlob(ctx, "http", "h", "/flat/Missing", old, newer, nil, nil); err != nil || ok {
		t.Fatalf("RefreshURLPathBlob of a missing row = %v, %v; want false", ok, err)
	}
	if _, err := c.RefreshURLPathBlob(ctx, "http", "h", "/flat/Packages", newer, "../../etc/passwd", nil, nil); !errors.Is(err, ErrInvalidHash) {
		t.Fatalf("a malformed hash: err %v, want ErrInvalidHash", err)
	}
}

// DeleteURLPathRows deletes the rows named, in one transaction, and
// counts them; a path without a row counts nothing, and other rows stay.
func TestDeleteURLPathRows_DeletesTheRowsNamed(t *testing.T) {
	c := openCache(t)
	blob := seedBlob(t, c, "Package: a\n")
	for _, p := range []string{"/flat/Packages", "/flat/Release", "/flat/Sources"} {
		putFetchedRow(t, c, p, blob, 1_799_999_000)
	}
	n, err := c.DeleteURLPathRows(context.Background(), "http", "h", []string{"/flat/Packages", "/flat/Release", "/flat/Gone"})
	if err != nil || n != 2 {
		t.Fatalf("DeleteURLPathRows = %d, %v; want 2", n, err)
	}
	if got := listedPaths(t, c, sk("h", "/flat")); !slices.Equal(got, []string{"/flat/Sources"}) {
		t.Fatalf("rows left = %v, want [/flat/Sources]", got)
	}
}
```

In `internal/freshness/retry_rig_test.go`, replace

```go
// body answers 404. While stalled, every request blocks until its context
// ends.
type rigUpstream struct {
	mu               sync.Mutex
	bodies           map[string][]byte
	ignoreValidators bool
	stall            bool
	fail             map[string]error    // per URL: the error Conditional returns (setFail)
	modified         map[string]string   // per URL: Last-Modified, sortable (setLastModified)
	sent             map[string][]string // If-None-Match per URL, in order
}
```

with

```go
// body answers 404. While stalled, every request blocks until its context
// ends. before, when set, runs at the start of every request, with its URL.
type rigUpstream struct {
	mu               sync.Mutex
	bodies           map[string][]byte
	ignoreValidators bool
	stall            bool
	fail             map[string]error    // per URL: the error Conditional returns (setFail)
	modified         map[string]string   // per URL: Last-Modified, sortable (setLastModified)
	sent             map[string][]string // If-None-Match per URL, in order
	before           func(url string)
}
```

In `internal/freshness/retry_rig_test.go`, replace

```go
	body, ok := u.bodies[target.URL]
	stall, ignore, failErr, stamp := u.stall, u.ignoreValidators, u.fail[target.URL], u.modified[target.URL]
	u.mu.Unlock()
	if stall {
```

with

```go
	body, ok := u.bodies[target.URL]
	stall, ignore, failErr, stamp := u.stall, u.ignoreValidators, u.fail[target.URL], u.modified[target.URL]
	before := u.before
	u.mu.Unlock()
	if before != nil {
		before(target.URL)
	}
	if stall {
```

Create `internal/freshness/anchorless_test.go`:

```go
package freshness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// gzBody is a body that passes the .gz sniff, different per s.
func gzBody(s string) []byte { return append(append([]byte{}, flatGz...), s...) }

// sniffRejected returns acu_anchorless_sniff_rejected_total for the flat
// rig's host as metrics.Default renders it.
func sniffRejected(t *testing.T) float64 {
	t.Helper()
	var buf bytes.Buffer
	metrics.Default.Render(&buf)
	for _, line := range strings.Split(buf.String(), "\n") {
		if v, ok := strings.CutPrefix(line, `acu_anchorless_sniff_rejected_total{host="`+flatRigHost+`"} `); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				t.Fatal(err)
			}
			return f
		}
	}
	return 0
}

// lookup returns the suite's url_path row of name, nil when it has none.
func (r *flatRig) lookup(name string) *cache.URLPath {
	r.t.Helper()
	u, err := r.cache.LookupURL(context.Background(), "http", flatRigHost, r.suite.SuitePath+"/"+name)
	switch {
	case errors.Is(err, cache.ErrNotFound):
		return nil
	case err != nil:
		r.t.Fatal(err)
	}
	return u
}

// Spec C4 Dispatch 3 → 4: a flat suite whose probe tree finds neither
// InRelease nor Release, with no signed evidence, refreshes its cached
// indexes anchorless — one conditional GET each, with its stored
// validators. A changed Packages.gz that passes its sniff replaces its
// blob (request_count and last_requested_at kept, last_fetched_at moved);
// a 304 keeps Sources.gz as it was; a 404 deletes Contents-amd64.gz; a
// plain Packages answered with an HTML page keeps its row and is counted.
// A plain Contents-* has no sniff (deviation 2): changed bytes delete
// Contents-i386, the same bytes keep Contents-all. The check is a
// success, anchorless_refreshed, and adopts nothing.
func TestCheck_AnchorlessRefreshRevalidatesTheIndexes(t *testing.T) {
	r := newFlatRig(t, "/fury")
	fetched := wallclock.Unix() - 3600
	oldPk, newPk, src, plain := gzBody("v1"), gzBody("v2"), gzBody("src"), []byte("Package: a\n")
	oldHash := r.cacheRow("Packages.gz", oldPk, 2, fetched, rigETag(oldPk))
	srcHash := r.cacheRow("Sources.gz", src, 1, fetched, rigETag(src))
	r.cacheRow("Contents-amd64.gz", gzBody("contents"), 1, fetched, "")
	plainHash := r.cacheRow("Packages", plain, 1, fetched, rigETag(plain))
	r.cacheRow("Contents-i386", []byte("usr/bin/a admin/a\n"), 1, fetched, "")
	allHash := r.cacheRow("Contents-all", []byte("usr/bin/b admin/b\n"), 1, fetched, "")
	r.up.setAt(r.url("Contents-i386"), []byte("usr/bin/a2 admin/a\n"))
	r.up.setAt(r.url("Contents-all"), []byte("usr/bin/b admin/b\n"))
	r.up.setAt(r.url("Packages.gz"), newPk)
	r.up.setAt(r.url("Sources.gz"), src)
	r.up.setAt(r.url("Packages"), []byte("<html>soft 404</html>"))
	rejected := sniffRejected(t)

	r.check()
	if got := r.lastResult(); got != "anchorless_refreshed" {
		t.Fatalf("result %q, want anchorless_refreshed", got)
	}
	for name, want := range map[string][]string{"Packages.gz": {rigETag(oldPk)}, "Sources.gz": {rigETag(src)},
		"Contents-amd64.gz": {""}, "Packages": {rigETag(plain)}, "InRelease": {""}, "Release": {""}} {
		if got := r.up.etagsSent(r.url(name)); !slices.Equal(got, want) {
			t.Errorf("%s: ETags sent %q, want %q", name, got, want)
		}
	}
	if n := r.requests("Release.gpg"); n != 0 {
		t.Errorf("Release.gpg probed %d times, want 0", n)
	}
	if u := r.lookup("Packages.gz"); u == nil || *u.BlobHash != hashOf(newPk) || *u.BlobHash == oldHash ||
		u.RequestCount != 2 || *u.LastRequestedAt != fetched || *u.LastFetchedAt <= fetched || *u.UpstreamETag != rigETag(newPk) {
		t.Errorf("Packages.gz row = %+v; want the new blob and ETag, its interest kept, last_fetched_at moved", u)
	}
	if u := r.lookup("Sources.gz"); u == nil || *u.BlobHash != srcHash || *u.LastFetchedAt != fetched {
		t.Errorf("Sources.gz row = %+v; want it untouched", u)
	}
	if u := r.lookup("Contents-amd64.gz"); u != nil {
		t.Errorf("Contents-amd64.gz row = %+v; want it deleted (404)", u)
	}
	if u := r.lookup("Packages"); u == nil || *u.BlobHash != plainHash || *u.LastFetchedAt != fetched {
		t.Errorf("Packages row = %+v; want it kept (the HTML failed its sniff)", u)
	}
	if u := r.lookup("Contents-i386"); u != nil {
		t.Errorf("Contents-i386 row = %+v; want it deleted (changed, and no sniff)", u)
	}
	if u := r.lookup("Contents-all"); u == nil || *u.BlobHash != allHash {
		t.Errorf("Contents-all row = %+v; want it kept (the same bytes)", u)
	}
	if got := sniffRejected(t) - rejected; got != 1 {
		t.Errorf("acu_anchorless_sniff_rejected_total rose by %v, want 1", got)
	}
	row := r.row()
	if row.CurrentSnapshotID != nil || row.LastSuccessAt == nil || *row.LastSuccessAt != 1_800_000_000 || row.CheckFailCount != 0 {
		t.Errorf("suite row = %+v; want a success and no snapshot", row)
	}
}

// Review Focus 2: an upstream that answers every request with an HTML
// page never replaces a flat index — the row stays, counted, its
// validators unchanged so the next check asks the same — and the check
// is answered: anchorless_unchanged.
func TestCheck_AnchorlessSniffRejectionKeepsTheRow(t *testing.T) {
	r := newFlatRig(t, "/fury")
	fetched := wallclock.Unix() - 3600
	pk := gzBody("v1")
	h := r.cacheRow("Packages.gz", pk, 1, fetched, rigETag(pk))
	r.up.setAt(r.url("Packages.gz"), []byte("<!DOCTYPE html><html>Not Found</html>"))
	rejected := sniffRejected(t)
	for range 2 {
		r.check()
		r.clock.advance(20 * time.Minute)
	}
	if got := r.lastResult(); got != "anchorless_unchanged" {
		t.Fatalf("result %q, want anchorless_unchanged", got)
	}
	if u := r.lookup("Packages.gz"); u == nil || *u.BlobHash != h || *u.LastFetchedAt != fetched || *u.UpstreamETag != rigETag(pk) {
		t.Fatalf("Packages.gz row = %+v; want it as it was", u)
	}
	if got, want := r.up.etagsSent(r.url("Packages.gz")), []string{rigETag(pk), rigETag(pk)}; !slices.Equal(got, want) {
		t.Fatalf("ETags sent %q, want %q", got, want)
	}
	if got := sniffRejected(t) - rejected; got != 2 {
		t.Fatalf("acu_anchorless_sniff_rejected_total rose by %v, want 2", got)
	}
}

// Spec C4 Dispatch 4's answers, one index row each: new bytes replace
// its blob; 404 and 410 delete it; 401 and 403 keep it; a 5xx, a transport error and a body over the
// cap keep it unknown — and with no answer at all the check fails (the
// streak backs off), logging the cap once. One answer is enough.
func TestCheck_AnchorlessAnswersByStatus(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		kept    bool
		result  string
		failLog bool
	}{
		{"200 with new bytes", nil, true, "anchorless_refreshed", false},
		{"404", &fetch.StatusError{Code: 404}, false, "anchorless_refreshed", false},
		{"410", &fetch.StatusError{Code: 410}, false, "anchorless_refreshed", false},
		{"401", &fetch.StatusError{Code: 401}, true, "anchorless_unchanged", false},
		{"403", &fetch.StatusError{Code: 403}, true, "anchorless_unchanged", false},
		{"503", &fetch.StatusError{Code: 503}, true, "failed", false},
		{"a transport error", errors.New("connection reset by peer"), true, "failed", false},
		{"a body over the cap", fmt.Errorf("conditional: %w", fetch.ErrConditionalBodyTooLarge), true, "failed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newFlatRig(t, "/fury")
			pk := gzBody("v1")
			r.cacheRow("Packages.gz", pk, 1, wallclock.Unix()-3600, rigETag(pk))
			r.up.setAt(r.url("Packages.gz"), gzBody("v2"))
			r.up.setFail(r.url("Packages.gz"), tc.err)
			r.check()
			if got := r.lastResult(); got != tc.result {
				t.Fatalf("result %q, want %q", got, tc.result)
			}
			if kept := r.lookup("Packages.gz") != nil; kept != tc.kept {
				t.Fatalf("row kept = %v, want %v", kept, tc.kept)
			}
			row := r.row()
			if failed := tc.result == "failed"; failed != (row.CheckFailCount == 1) || failed != (row.LastSuccessAt == nil) {
				t.Fatalf("suite row = %+v; want failed = %v", row, failed)
			}
			if got := strings.Count(r.logs.String(), "anchorless index over its size cap"); got != map[bool]int{true: 1}[tc.failLog] {
				t.Fatalf("%d size-cap WARN lines, want %d", got, map[bool]int{true: 1}[tc.failLog])
			}
		})
	}
	t.Run("one answer is enough", func(t *testing.T) {
		r := newFlatRig(t, "/fury")
		pk, src := gzBody("v1"), gzBody("src")
		r.cacheRow("Packages.gz", pk, 1, wallclock.Unix()-3600, rigETag(pk))
		r.cacheRow("Sources.gz", src, 1, wallclock.Unix()-3600, rigETag(src))
		r.up.setFail(r.url("Packages.gz"), &fetch.StatusError{Code: 503})
		r.up.setAt(r.url("Sources.gz"), src)
		r.check()
		if got := r.lastResult(); got != "anchorless_unchanged" {
			t.Fatalf("result %q, want anchorless_unchanged", got)
		}
	})
}

// Spec C4 Dispatch 4: the suite's cached anchor rows that upstream now
// answers 404 or 410 are deleted — the probe tree's own answers, no other
// request — a junk InRelease and an unsigned Release among them
// (deviation 1: neither is signed evidence); a junk Release.gpg, never
// probed when Release is absent, stays, and so does a Release answered
// 401. (A sniff-passing Release.gpg is signed evidence: the gate keeps
// the suite out, TestCheck_AnchorlessGateKeepsSignedSuitesOut.)
func TestCheck_AnchorlessDeletesTheAnchorsUpstreamDropped(t *testing.T) {
	rel, _ := makeRelease(map[string][]byte{"Packages.gz": gzBody("v1")})
	for _, tc := range []struct {
		name       string
		releaseErr error
		release    bool
		result     string
	}{
		{"Release answered 404", nil, false, "anchorless_refreshed"},
		{"Release answered 401", &fetch.StatusError{Code: 401}, true, "anchorless_refreshed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newFlatRig(t, "/fury")
			fetched := wallclock.Unix() - 3600
			pk := gzBody("v1")
			r.cacheRow("Packages.gz", pk, 1, fetched, rigETag(pk))
			r.cacheRow("InRelease", []byte("<html>junk</html>"), 0, fetched, "")
			r.cacheRow("Release", rel, 1, fetched, "")
			r.cacheRow("Release.gpg", []byte("<html>junk</html>"), 0, fetched, "")
			r.up.setAt(r.url("Packages.gz"), pk)
			r.up.setFail(r.url("InRelease"), &fetch.StatusError{Code: 410})
			if tc.releaseErr != nil {
				r.up.setFail(r.url("Release"), tc.releaseErr)
			}
			r.check()
			if got := r.lastResult(); got != tc.result {
				t.Fatalf("result %q, want %q", got, tc.result)
			}
			if r.lookup("InRelease") != nil || (r.lookup("Release") != nil) != tc.release ||
				r.lookup("Release.gpg") == nil || r.lookup("Packages.gz") == nil {
				t.Fatalf("rows: InRelease %v, Release %v, Release.gpg %v, Packages.gz %v; want InRelease gone, Release kept = %v",
					r.lookup("InRelease"), r.lookup("Release"), r.lookup("Release.gpg"), r.lookup("Packages.gz"), tc.release)
			}
			if n := r.requests("Release.gpg"); n != 0 {
				t.Fatalf("Release.gpg requested %d times, want 0", n)
			}
		})
	}
}

// Spec C4 Dispatch 3's gate (plan design note 9, deviation 1): signed
// evidence keeps a flat suite out of the anchorless refresh — a
// signed-seen marker for its base, or a sniff-passing InRelease or
// Release.gpg cached under the suite or its /. twin, under either scheme
// (re-marked first) — and so does a dists/ suite's layout: the check is
// the failed check and anchor_absent record it was, and no index is
// requested. A gate that cannot read a cached anchor's pool file fails
// the check too (fail closed), with no record.
func TestCheck_AnchorlessGateKeepsSignedSuitesOut(t *testing.T) {
	signed := simtest.ClearsignedFixture("Origin: fury\n")
	cacheAt := func(t *testing.T, r *flatRig, scheme, path string, body []byte) string {
		t.Helper()
		h := poolBlob(t, r.cache, body)
		fetched := wallclock.Unix() - 3600
		if err := r.cache.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: scheme, CanonicalHost: flatRigHost,
			Path: path, BlobHash: &h, UpstreamURL: scheme + "://" + flatRigHost + path, IsMetadata: true, LastFetchedAt: &fetched}); err != nil {
			t.Fatal(err)
		}
		return h
	}
	for _, tc := range []struct {
		name   string
		suite  string
		setup  func(t *testing.T, r *flatRig)
		record bool
	}{
		{"a signed-seen marker", "/fury", func(t *testing.T, r *flatRig) {
			if err := r.cache.MarkSignedSeenBatch(context.Background(), []cache.SignedSeenMark{{CanonicalScheme: "https",
				CanonicalHost: flatRigHost, Base: "/fury", SeenAt: 1_700_000_000}}); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"a signed InRelease cached under the twin over https", "/fury", func(t *testing.T, r *flatRig) {
			cacheAt(t, r, "https", "/fury/./InRelease", signed)
		}, true},
		{"a Release.gpg cached", "/fury/.", func(t *testing.T, r *flatRig) {
			cacheAt(t, r, "http", "/fury/Release.gpg", armoredSig("fury"))
		}, true},
		{"a dists suite", "/r/dists/x", func(t *testing.T, r *flatRig) {
			r.cacheRow("InRelease", []byte("<html>junk</html>"), 1, wallclock.Unix()-3600, "")
		}, true},
		{"an unreadable pool file", "/fury", func(t *testing.T, r *flatRig) {
			h := cacheAt(t, r, "https", "/fury/InRelease", signed)
			if err := os.Remove(r.cache.BlobPath(h)); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(r.cache.BlobPath(h), 0o700); err != nil {
				t.Fatal(err)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newFlatRig(t, tc.suite)
			if tc.suite != "/r/dists/x" {
				r.cacheRow("Packages.gz", gzBody("v1"), 1, wallclock.Unix()-3600, "")
			}
			tc.setup(t, r)
			r.check()
			if got := r.lastResult(); got != "failed" {
				t.Fatalf("result %q, want failed", got)
			}
			if n := r.requests("Packages.gz"); n != 0 {
				t.Fatalf("Packages.gz requested %d times, want 0", n)
			}
			row := r.row()
			if got := row.AdoptionFailReason != nil && *row.AdoptionFailReason == cache.ReasonAnchorAbsent; got != tc.record {
				t.Fatalf("anchor_absent recorded = %v, want %v (row %+v)", got, tc.record, row)
			}
		})
	}
}

// An answered anchorless check clears what the absent anchors left: the
// anchor_absent record and a pending change (a stamp the anchorless form
// never clears by adopting would queue the suite every tick). The suite
// is next due one refresh interval later.
func TestCheck_AnchorlessClearsTheRecordAndThePendingChange(t *testing.T) {
	r := newFlatRig(t, "/fury")
	ctx := context.Background()
	pk := gzBody("v1")
	r.cacheRow("Packages.gz", pk, 1, wallclock.Unix()-3600, rigETag(pk))
	r.up.setAt(r.url("Packages.gz"), pk)
	if err := r.cache.RecordAdoptionFailure(ctx, "http", flatRigHost, "/fury", cache.AdoptionFailure{Hash: anchorAbsentKey,
		Reason: cache.ReasonAnchorAbsent, Count: 1, FirstAt: 1_799_990_000, LastAt: 1_799_990_000, NextRetryAt: 1_799_999_000}); err != nil {
		t.Fatal(err)
	}
	row := r.row()
	seen := int64(1_799_990_000)
	row.InReleaseChangeSeenAt = &seen
	if err := r.cache.UpdateSuiteFreshness(ctx, row); err != nil {
		t.Fatal(err)
	}
	r.check()
	if got := r.lastResult(); got != "anchorless_unchanged" {
		t.Fatalf("result %q, want anchorless_unchanged", got)
	}
	row = r.row()
	if row.AdoptionFailCount != 0 || row.AdoptionFailReason != nil || row.InReleaseChangeSeenAt != nil {
		t.Fatalf("suite row = %+v; want no record and no pending change", row)
	}
	now := r.clock.now()
	if r.checker.suiteDue(row, now.Add(time.Minute)) || !r.checker.suiteDue(row, now.Add(15*time.Minute)) {
		t.Fatalf("due a minute later %v, a refresh interval later %v; want false, true",
			r.checker.suiteDue(row, now.Add(time.Minute)), r.checker.suiteDue(row, now.Add(15*time.Minute)))
	}
}

// Ruling R-G4: a twin refreshes its own rows only — /fury/.'s refresh
// never requests, replaces or deletes /fury's Packages.gz.
func TestCheck_AnchorlessRefreshesItsOwnRowsOnly(t *testing.T) {
	r := newFlatRig(t, "/fury/.")
	fetched := wallclock.Unix() - 3600
	r.cacheRow("Packages.gz", gzBody("twin"), 1, fetched, "")
	other := poolBlob(t, r.cache, gzBody("suite"))
	if err := r.cache.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: "http", CanonicalHost: flatRigHost,
		Path: "/fury/Packages.gz", BlobHash: &other, UpstreamURL: "http://" + flatRigHost + "/fury/Packages.gz",
		IsMetadata: true, RequestCount: 1, LastFetchedAt: &fetched}); err != nil {
		t.Fatal(err)
	}
	r.check()
	if got := r.lastResult(); got != "anchorless_refreshed" {
		t.Fatalf("result %q, want anchorless_refreshed", got)
	}
	if r.lookup("Packages.gz") != nil {
		t.Fatal("the twin's Packages.gz survived its 404")
	}
	u, err := r.cache.LookupURL(context.Background(), "http", flatRigHost, "/fury/Packages.gz")
	if err != nil || *u.BlobHash != other || *u.LastFetchedAt != fetched {
		t.Fatalf("/fury/Packages.gz = %+v (%v); want it untouched", u, err)
	}
	if n := len(r.up.etagsSent("http://" + flatRigHost + "/fury/Packages.gz")); n != 0 {
		t.Fatalf("/fury/Packages.gz requested %d times, want 0", n)
	}
}

// A shutdown during the refresh ends the check with no write and no
// failed-check line (as a cancelled probe does); the rows after the cut
// are not requested.
func TestCheck_AnchorlessShutdownWritesNothing(t *testing.T) {
	r := newFlatRig(t, "/fury")
	pk, src := gzBody("v1"), gzBody("src")
	r.cacheRow("Packages.gz", pk, 1, wallclock.Unix()-3600, rigETag(pk))
	r.cacheRow("Sources.gz", src, 1, wallclock.Unix()-3600, rigETag(src))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.up.before = func(url string) {
		if url == r.url("Packages.gz") {
			cancel()
		}
	}
	r.up.setFail(r.url("Packages.gz"), context.Canceled)
	r.checker.Check(ctx, "http", flatRigHost, "/fury")
	r.checker.WaitForAdoptions()
	if got := r.lastResult(); got != "" {
		t.Fatalf("result %q, want no check line", got)
	}
	if row := r.row(); row.LastCheckAt != nil || row.CheckFailCount != 0 {
		t.Fatalf("suite row = %+v; want nothing written", row)
	}
	if n := r.requests("Sources.gz"); n != 0 {
		t.Fatalf("Sources.gz requested %d times, want 0", n)
	}
}

// Spec C4 Dispatch 4: "If a Release/InRelease later becomes present, the
// suite moves to the normal forms" — the next check adopts it.
func TestCheck_AnAnchorlessSuiteThatPublishesAnInReleaseAdopts(t *testing.T) {
	r := newFlatRig(t, "/fury")
	pk := gzBody("v1")
	r.cacheRow("Packages.gz", pk, 1, wallclock.Unix()-3600, rigETag(pk))
	r.up.setAt(r.url("Packages.gz"), pk)
	r.check()
	if got := r.lastResult(); got != "anchorless_unchanged" {
		t.Fatalf("first check: %q, want anchorless_unchanged", got)
	}
	pkgs := fakePackagesStanzas(map[string]string{"x_1_amd64.deb": testHex64})
	rel, _ := makeRelease(map[string][]byte{"Packages": pkgs})
	r.up.setAt(r.url("InRelease"), simtest.ClearsignedFixture(string(rel)))
	r.members.put(r.url("Packages"), pkgs)
	r.clock.advance(20 * time.Minute)
	r.check()
	if got, row := r.lastResult(), r.row(); got != "first_adoption" || row.CurrentSnapshotID == nil {
		t.Fatalf("second check: %q, current %v; want a first adoption", got, row.CurrentSnapshotID)
	}
}
```

Create `internal/handler/urlpath_lastmod_test.go`:

```go
package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// cacheURLPath stores body as host 127.0.0.1's url_path row at path,
// last fetched at fetched, its pool file's mtime set to mtime.
func cacheURLPath(t *testing.T, h *Handler, path string, body []byte, fetched int64, mtime time.Time, isMetadata bool) {
	t.Helper()
	hash := writeBlob(t, h, body)
	if err := os.Chtimes(h.cache.BlobPath(hash), mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if err := h.cache.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
		Path: path, BlobHash: &hash, UpstreamURL: "http://127.0.0.1" + path, IsMetadata: isMetadata,
		RequestCount: 1, LastRequestedAt: &fetched, LastFetchedAt: &fetched}); err != nil {
		t.Fatal(err)
	}
}

// Spec C4 (plan design note 14): url_path-served metadata answers
// Last-Modified = max(pool mtime, last_fetched_at) — at a hit, a miss and
// a stale serve — so apt's If-Modified-Since sees a replacement whose
// bytes an earlier fetch already pooled; a .deb keeps its pool file's
// mtime.
func TestServeHTTP_URLPathMetadataLastModifiedIsTheLaterStamp(t *testing.T) {
	mtime := time.Unix(wallclock.Unix()-60, 0)
	ahead, behind := wallclock.Unix()+3600, wallclock.Unix()-3600

	t.Run("hits", func(t *testing.T) {
		h := newTestHandler(t, nil, nil)
		cacheURLPath(t, h, "/flat/Packages.gz", flatPackagesGz, ahead, mtime, true)
		cacheURLPath(t, h, "/flat/Sources", []byte("Package: s\n"), behind, mtime, true)
		cacheURLPath(t, h, "/flat/x_1_amd64.deb", []byte("deb bytes"), ahead, mtime, false)
		for path, want := range map[string]int64{"/flat/Packages.gz": ahead, "/flat/Sources": mtime.Unix(), "/flat/x_1_amd64.deb": mtime.Unix()} {
			if code, lm := lastModifiedGet(t, h, path, ""); code != http.StatusOK || lm != httpTime(want) {
				t.Errorf("%s: %d, Last-Modified %q; want 200 and %q", path, code, lm, httpTime(want))
			}
		}
	})

	t.Run("misses", func(t *testing.T) {
		restore := wallclock.SetOffsetForTest(time.Hour) // the row's stamp leads the pool file's mtime
		defer restore()
		h := newTestHandler(t, nil, nil)
		h.fetch = staticUpstream{"http://127.0.0.1/flat/Packages": []byte("Package: a\n"),
			"http://127.0.0.1/flat/y_1_amd64.deb": []byte("deb bytes")}
		for path, metadata := range map[string]bool{"/flat/Packages": true, "/flat/y_1_amd64.deb": false} {
			code, lm := lastModifiedGet(t, h, path, "")
			row, err := h.cache.LookupURL(context.Background(), "http", "127.0.0.1", path)
			if err != nil || code != http.StatusOK {
				t.Fatalf("%s: %d, row %v (%v)", path, code, row, err)
			}
			st, err := os.Stat(h.cache.BlobPath(*row.BlobHash))
			if err != nil {
				t.Fatal(err)
			}
			want := st.ModTime().Unix()
			if metadata {
				want = *row.LastFetchedAt
			}
			if lm != httpTime(want) || *row.LastFetchedAt <= st.ModTime().Unix() {
				t.Errorf("%s: Last-Modified %q; want %q (row stamp %d, pool mtime %d)", path, lm, httpTime(want),
					*row.LastFetchedAt, st.ModTime().Unix())
			}
		}
	})

	t.Run("a stale serve", func(t *testing.T) {
		h := newTestHandlerWithServe(t, config.ServeConfig{ServeStaleWhenUpstreamDown: true}, nil)
		cacheURLPath(t, h, "/flat/Packages.xz", []byte("\xfd7zXZ\x00stale"), ahead, mtime, true)
		req, err := h.parser.Parse("http://127.0.0.1/flat/Packages.xz", "127.0.0.1")
		if err != nil || !req.IsMetadata {
			t.Fatalf("Parse: %+v, %v", req, err)
		}
		rec := httptest.NewRecorder()
		if !h.tryServeStale(rec, newProxyReqHostPort("GET", "http", "127.0.0.1", "/flat/Packages.xz"), req, 0, time.Now()) {
			t.Fatal("tryServeStale = false, want a stale serve")
		}
		if lm := rec.Header().Get("Last-Modified"); lm != httpTime(ahead) {
			t.Fatalf("stale Last-Modified %q, want %q", lm, httpTime(ahead))
		}
	})
}

// Ruling R-G3: a miss reads its row's last_fetched_at after the pool file
// is final, so no url_path anchor's pool file is newer than its row's
// stamp — the stamp adoptionStampTx's first-adoption floor passes — even
// when the body arrives over a second after the request. The served
// Last-Modified is the row's stamp, and the first adoption's anchor is
// stamped after it.
func TestServeHTTP_AMissStampsItsRowAfterItsPoolFile(t *testing.T) {
	h := newTestHandler(t, nil, nil)
	body := []byte("-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\nOrigin: slow\n-----BEGIN PGP SIGNATURE-----\n\nx\n-----END PGP SIGNATURE-----\n")
	var fetches atomic.Int32
	h.fetch = slowUpstream{staticUpstream: staticUpstream{"http://127.0.0.1" + lmSuite + "/InRelease": body},
		delay: 1100 * time.Millisecond, fetches: &fetches}
	code, lm0 := lastModifiedGet(t, h, lmSuite+"/InRelease", "")
	row, err := h.cache.LookupURL(context.Background(), "http", "127.0.0.1", lmSuite+"/InRelease")
	if err != nil || code != http.StatusOK {
		t.Fatalf("miss: %d, row %v (%v)", code, row, err)
	}
	st, err := os.Stat(h.cache.BlobPath(*row.BlobHash))
	if err != nil {
		t.Fatal(err)
	}
	if *row.LastFetchedAt < st.ModTime().Unix() || lm0 != httpTime(*row.LastFetchedAt) {
		t.Fatalf("row stamp %d, pool mtime %d, Last-Modified %q; want the stamp at or after the pool file, served",
			*row.LastFetchedAt, st.ModTime().Unix(), lm0)
	}
	id := commitAnchor(t, h, "anchor adopted first")
	if code, lm1 := lastModifiedGet(t, h, lmSuite+"/InRelease", lm0); code != http.StatusOK || lm1 != httpTime(adoptedAt(t, h, id)) {
		t.Fatalf("after the first adoption, If-Modified-Since %q got %d (Last-Modified %q); want 200", lm0, code, lm1)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -gcflags=-e ./internal/aptpath/ ./internal/cache/`
Expected: FAIL — `undefined: SniffAnchorlessIndex`, `undefined: IsAnchorlessIndex`; `c.RefreshURLPathBlob undefined (type *Cache has no field or method RefreshURLPathBlob)` (six times), `c.DeleteURLPathRows undefined (type *Cache has no field or method DeleteURLPathRows, but does have method deleteURLPathRows)`.

Run: `go test ./internal/freshness/ -run Anchorless`
Expected: FAIL — every test except `TestCheck_AnchorlessAnswersByStatus/503` and `/a_transport_error` and `TestCheck_AnchorlessGateKeepsSignedSuitesOut`'s first four subtests (today every suite without an anchor gets the failed check those six expect): `result "failed", want anchorless_refreshed` and the like; `TestCheck_AnchorlessAnswersByStatus/a_body_over_the_cap` `0 size-cap WARN lines, want 1`; `TestCheck_AnchorlessGateKeepsSignedSuitesOut/an_unreadable_pool_file` `anchor_absent recorded = true, want false`; `TestCheck_AnchorlessShutdownWritesNothing` `result "failed", want no check line`.

Run: `go test ./internal/handler/ -run 'TestServeHTTP_URLPathMetadataLastModifiedIsTheLaterStamp|TestServeHTTP_AMissStampsItsRowAfterItsPoolFile'`
Expected: FAIL — `/hits`: `/flat/Packages.gz: 200, Last-Modified "<pool mtime>"; want 200 and "<an hour later>"`; `/misses`: `/flat/Packages: Last-Modified "<pool mtime>"; want "<row stamp>"`; `/a_stale_serve`: `stale Last-Modified …`; `TestServeHTTP_AMissStampsItsRowAfterItsPoolFile`: `row stamp N, pool mtime N-1, Last-Modified "<pool mtime>"; want the stamp at or after the pool file, served` (the miss serves its pool file's mtime).

- [ ] **Step 3: Implement**

Replace `internal/cache/anchorless.go` (Task 9's file) with:

```go
package cache

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
)

// ListSuiteMetadataRows returns key's suite metadata url_path rows, in
// path order: the rows of key's scheme and host whose aptpath.SuitePath is
// the suite — never a nested suite's, a prefix sibling's or the "/." twin's,
// as suiteMetadataPaths reads them for DeleteSuiteMetadata. A key no
// grammar produces (aptpath.IsCanonicalSuitePath) has none. Spec C4:
// Dispatch 3 reads a flat suite's client-created index rows from it, and
// the anchorless refresh revalidates them.
//
// AIDEV-NOTE: one range read of the url_path primary key over the suite's
// directory; for a flat suite that range also holds its cached .debs,
// which the SuitePath filter drops. Dispatch 3 reads it only for a flat
// suite with no client-created anchor row (a Release-less repository), and
// the anchorless refresh once per check.
func (c *Cache) ListSuiteMetadataRows(ctx context.Context, key SuiteKey) ([]URLPath, error) {
	if !aptpath.IsCanonicalSuitePath(key.SuitePath) {
		return nil, nil
	}
	rows, err := c.db.QueryContext(ctx, `
SELECT canonical_scheme, canonical_host, path, blob_hash, upstream_url,
       is_metadata, last_requested_at, request_count, last_fetched_at,
       upstream_etag, upstream_lastmod
  FROM url_path
 WHERE canonical_scheme = ? AND canonical_host = ? AND path >= ? AND path < ?
 ORDER BY path`, key.CanonicalScheme, key.CanonicalHost, key.SuitePath+"/", key.SuitePath+"0")
	if err != nil {
		return nil, fmt.Errorf("ListSuiteMetadataRows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []URLPath
	for rows.Next() {
		var (
			u    URLPath
			isMD int64
		)
		if err := rows.Scan(&u.CanonicalScheme, &u.CanonicalHost, &u.Path, &u.BlobHash, &u.UpstreamURL,
			&isMD, &u.LastRequestedAt, &u.RequestCount, &u.LastFetchedAt, &u.UpstreamETag, &u.UpstreamLastMod); err != nil {
			return nil, fmt.Errorf("ListSuiteMetadataRows: scan: %w", err)
		}
		if aptpath.SuitePath(u.Path) != key.SuitePath {
			continue
		}
		u.IsMetadata = isMD != 0
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListSuiteMetadataRows: %w", err)
	}
	return out, nil
}

// RefreshURLPathBlob is the anchorless refresh's write (spec C4 Dispatch
// 4: "a dedicated cache method"). It points the url_path row of scheme,
// host and path from blob old to blob newHash — already in the pool with
// its blob row: the caller wrote it outside the writer (ruling PD-5) —
// with the response's validators (nil stores NULL), and reports whether
// it did. It never writes last_requested_at or request_count, and it sets
// last_fetched_at only when the blob changes, to MAX(now, previous + 1)
// (plan design note 13: a replacement within the second of the previous
// fetch still moves Last-Modified forward). A row that changed since the
// caller read it — a miss stored newer bytes, url_path GC deleted it — is
// left alone (false): the refresh never overwrites what a client's own
// fetch just stored. Like the miss path's upsert it changes no blob
// refcount: blob GC finds a url_path row's blob through its NOT EXISTS
// clause.
//
// AIDEV-NOTE: ruling R-G3. url_path-served metadata answers Last-Modified
// = max(pool mtime, last_fetched_at) (the handler's urlPathModTime), and
// adoptionStampTx's first-adoption floor stays above it only while no
// pool file is newer than its row's last_fetched_at: the caller finalizes
// the pool file before this stamps the row, as runFetch does. Keep that
// order.
func (c *Cache) RefreshURLPathBlob(ctx context.Context, scheme, host, path, old, newHash string, etag, lastmod *string) (bool, error) {
	if !validBlobHash(old) || !validBlobHash(newHash) {
		return false, fmt.Errorf("RefreshURLPathBlob: %w", ErrInvalidHash)
	}
	now := nowUnix()
	updated := false
	err := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `
UPDATE url_path
   SET blob_hash        = ?,
       upstream_etag    = ?,
       upstream_lastmod = ?,
       last_fetched_at  = CASE WHEN blob_hash = ? THEN last_fetched_at
                               ELSE MAX(?, COALESCE(last_fetched_at, 0) + 1) END
 WHERE canonical_scheme = ? AND canonical_host = ? AND path = ? AND blob_hash = ?`,
			newHash, etag, lastmod, newHash, now, scheme, host, path, old)
		if err != nil {
			return fmt.Errorf("RefreshURLPathBlob: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("RefreshURLPathBlob: rows: %w", err)
		}
		updated = n > 0
		return nil
	})
	if err != nil {
		return false, err
	}
	return updated, nil
}

// DeleteURLPathRows deletes, in one writer transaction, the url_path rows
// of scheme and host at paths, as DeleteSuiteMetadata deletes a suite's
// (deleteCachedRowTx), and reports how many it deleted: the anchorless
// refresh's deletes (spec C4 Dispatch 4: an index or a cached anchor
// upstream answers 404 or 410, a changed plain Contents-*). No suite row
// is touched.
func (c *Cache) DeleteURLPathRows(ctx context.Context, scheme, host string, paths []string) (int, error) {
	return c.deleteURLPathRows(ctx, scheme, host, paths)
}
```

Replace `internal/freshness/anchorless.go` (Task 9's file) with:

```go
package freshness

// Spec C4 for flat suites: Dispatch 3's client-created index rows, and
// (Dispatch 4) the anchorless refresh of a flat, Release-less suite.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
)

// suiteKeyOf is the cache key of suite.
func suiteKeyOf(suite SuiteRef) cache.SuiteKey {
	return cache.SuiteKey{CanonicalScheme: suite.CanonicalScheme, CanonicalHost: suite.CanonicalHost, SuitePath: suite.SuitePath}
}

// flatIndexClientCreated reports whether flat suite has a client-created
// root Packages* or Sources* url_path row (aptpath.FileFlatIndex,
// request_count > 0): spec C4 Dispatch 3 counts it beside the InRelease
// and Release rows, so a Release-less repository's suite — created by its
// index's serve — is not dormant (the E carry-forward; plan design note 8).
// A row hot prefetch or a reconstruction wrote has no request count.
func (a *Adopter) flatIndexClientCreated(ctx context.Context, suite SuiteRef) (bool, error) {
	rows, err := a.cache.ListSuiteMetadataRows(ctx, suiteKeyOf(suite))
	if err != nil {
		return false, fmt.Errorf("read flat index rows: %w", err)
	}
	for _, r := range rows {
		if r.RequestCount > 0 && aptpath.ClassifySuiteFile(suite.SuitePath, r.Path) == aptpath.FileFlatIndex {
			return true, nil
		}
	}
	return false, nil
}

// maxAnchorlessIndexBytes is the anchorless refresh's body cap (plan
// design note 12): fetch.Conditional reads a body into memory, and the
// Release-less repositories publish indexes of kilobytes to a few MiB. A
// body over it keeps the row (an unknown answer).
const maxAnchorlessIndexBytes = 64 << 20

// errAnchorlessUnknown fails an anchorless check whose every index
// request ended unknown.
var errAnchorlessUnknown = errors.New("every anchorless index request ended unknown")

// anchorlessEligible is spec C4 Dispatch 3's gate in front of the
// anchorless refresh (plan design note 9, deviation 1): a flat suite whose
// probe tree found neither InRelease nor Release refreshes anchorless only
// with no signed evidence. The cached signed-anchor re-mark runs first
// (remarkSignedAnchors: each sniff-passing InRelease or Release.gpg row of
// the base or its /. twin, under either scheme, marks the base), then
// never-downgrade's rule (cache.CheckUnsignedDowngrade: a marker for the
// base, or an adopted signed snapshot of the base or its twin) must find
// nothing. An error — a read, a pool file, the mark — is returned: the
// caller fails the check (fail closed, as the unsigned commit's re-mark
// does).
//
// AIDEV-NOTE: the anchorless refresh stores index bytes nothing verified,
// so it must never run for a repository the cache has seen signed:
// stripping a signed flat repository's InRelease and Release would
// otherwise turn it into an unverified one. A cached Release that passes
// its sniff is no signed evidence (deviation 1): the refresh deletes it
// once upstream answers it 404. The gate does not read
// adoption.adopt_unsigned (an anchorless suite has no unsigned candidate
// to refuse; flagged for the maintainer).
func (a *Adopter) anchorlessEligible(ctx context.Context, suite SuiteRef) (bool, error) {
	if err := a.remarkSignedAnchors(ctx, suite); err != nil {
		return false, fmt.Errorf("re-mark the cached signed anchors: %w", err)
	}
	err := a.cache.CheckUnsignedDowngrade(ctx, suite.CanonicalHost, suite.SuitePath)
	switch {
	case errors.Is(err, cache.ErrUnsignedDowngradeRefused):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// anchorlessAnswer is what one anchorless index request did to its row.
type anchorlessAnswer int

const (
	// anchorlessUnknown: no answer (a transport error, the deadline, a
	// status not listed, a body over the cap, a local write error); the
	// row stays.
	anchorlessUnknown anchorlessAnswer = iota
	// anchorlessKept: answered, and the row stays (304, 401, 403, an
	// unchanged 200, a 200 failing its sniff).
	anchorlessKept
	// anchorlessReplaced: a 200 that passed its sniff replaced the blob.
	anchorlessReplaced
	// anchorlessDeleted: the row is to be deleted (404, 410, a changed
	// plain Contents-*).
	anchorlessDeleted
)

// anchorlessRefresh is spec C4 Dispatch 4 (plan design note 10) for a flat
// suite whose probe tree t found neither InRelease nor Release and whose
// gate admitted it (anchorlessEligible), under the check's host slot and
// deadline (fetchCtx). Each cached url_path row of the suite whose member
// is an anchorless index (aptpath.IsAnchorlessIndex) gets one conditional
// GET (refreshAnchorlessRow); then the rows to delete — those answered
// 404 or 410, and the suite's cached InRelease and Release rows whose
// probe in t answered 404 or 410 — go in one writer transaction. The check
// is answered — anchorless_refreshed when a row was replaced or deleted,
// anchorless_unchanged otherwise — unless every index request ended
// unknown: a failed check (the streak backs off). An answered check is a
// success (last_success_at, the streak reset), clears the suite's failure
// or refusal record (it described an anchor upstream no longer serves)
// and its pending change (the anchorless form adopts nothing: a stamp
// left behind would queue the suite every tick, pendingChange). A shutdown
// mid-refresh ends it silently; what it stored stays.
//
// AIDEV-NOTE: no snapshot, no vouching, no strict-mode effect (spec C4):
// the refresh only revalidates url_path rows a client's miss created, and
// a row it cannot answer for stays as it is. Its cache operations go
// through the adopter's *cache.Cache: the checker's Cache interface stays
// the subset its fakes implement.
func (c *Checker) anchorlessRefresh(ctx, fetchCtx context.Context, out *checkOutcome, cur *cache.SuiteFreshness, suite SuiteRef, nowUnix int64, t probeTree) {
	host, suitePath := suite.CanonicalHost, suite.SuitePath
	rows, err := c.adopter.cache.ListSuiteMetadataRows(ctx, suiteKeyOf(suite))
	if err != nil {
		if !c.endedByShutdown(ctx, suite, "read the anchorless index rows", err) {
			c.failCheck(ctx, out, cur, suite, nowUnix, "anchorless", err)
		}
		return
	}
	var (
		deletes                      []string
		requests, answered, replaced int
		lastErr                      error
		overCap                      bool
	)
	cached := make(map[string]bool, len(rows))
	for _, row := range rows {
		cached[row.Path] = true
		name := strings.TrimPrefix(row.Path, suitePath+"/")
		if !aptpath.IsAnchorlessIndex(name) || row.BlobHash == nil {
			continue
		}
		requests++
		a, err := c.refreshAnchorlessRow(ctx, fetchCtx, suite, name, row)
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			c.logger.Debug("freshness: check cancelled", "canonical_host", host, "suite_path", suitePath)
			return
		}
		switch a {
		case anchorlessUnknown:
			lastErr = err
			overCap = overCap || errors.Is(err, fetch.ErrConditionalBodyTooLarge)
		case anchorlessReplaced:
			replaced++
		case anchorlessDeleted:
			deletes = append(deletes, row.Path)
		}
		if a != anchorlessUnknown {
			answered++
		}
	}
	for _, p := range []struct {
		name string
		o    probeOutcome
	}{{"InRelease", t.inRelease}, {"Release", t.release}} {
		if path := suitePath + "/" + p.name; (p.o.status == 404 || p.o.status == 410) && cached[path] {
			deletes = append(deletes, path)
		}
	}
	deleted := 0
	if len(deletes) > 0 {
		n, err := c.adopter.cache.DeleteURLPathRows(ctx, suite.CanonicalScheme, host, deletes)
		if err != nil {
			if c.endedByShutdown(ctx, suite, "delete anchorless rows", err) {
				return
			}
			c.logger.Warn("freshness: delete anchorless rows failed",
				"err", err, "canonical_host", host, "suite_path", suitePath)
		}
		deleted = n
	}
	if overCap {
		c.logger.Warn("freshness: anchorless index over its size cap",
			"canonical_host", host, "suite_path", suitePath, "max_bytes", maxAnchorlessIndexBytes)
	}
	if requests > 0 && answered == 0 {
		c.failCheck(ctx, out, cur, suite, nowUnix, "anchorless", fmt.Errorf("%w: %w", errAnchorlessUnknown, lastErr))
		return
	}
	cur.LastCheckAt, cur.LastSuccessAt, cur.CheckFailCount = &nowUnix, &nowUnix, 0
	cur.InReleaseChangeSeenAt = nil
	e := checkEnd{form: "anchorless", result: "anchorless_unchanged", status: t.release.status, clearRecord: true}
	if replaced+deleted > 0 {
		e.result = "anchorless_refreshed"
	}
	c.finishCheck(ctx, out, cur, suite, e)
}

// refreshAnchorlessRow sends the anchorless index row's conditional GET
// (its stored upstream_url, upstream_etag and upstream_lastmod) under
// fetchCtx and applies the answer: a 304 keeps the row; a 200 whose body
// passes aptpath.SniffAnchorlessIndex replaces its blob
// (cache.RefreshURLPathBlob, after the body is in the pool) — or, with the
// same bytes, stores only changed validators; a 200 failing it keeps the
// row and counts acu_anchorless_sniff_rejected_total; a plain Contents-*
// (no sniff, plan deviation 2) is deleted when its bytes changed; 404 and
// 410 delete it, 401 and 403 keep it. Anything else is unknown, with err
// saying why.
func (c *Checker) refreshAnchorlessRow(ctx, fetchCtx context.Context, suite SuiteRef, name string, row cache.URLPath) (anchorlessAnswer, error) {
	var etag, lastmod string
	if row.UpstreamETag != nil {
		etag = *row.UpstreamETag
	}
	if row.UpstreamLastMod != nil {
		lastmod = *row.UpstreamLastMod
	}
	target := &fetch.Target{CanonicalHost: suite.CanonicalHost, URL: row.UpstreamURL}
	res, err := c.fetcher.Conditional(fetchCtx, target, etag, lastmod, maxAnchorlessIndexBytes)
	if err != nil {
		var se *fetch.StatusError
		if errors.As(err, &se) {
			switch se.Code {
			case 404, 410:
				return anchorlessDeleted, nil
			case 401, 403:
				return anchorlessKept, nil
			}
		}
		return anchorlessUnknown, fmt.Errorf("%s: %w", name, err)
	}
	switch {
	case res.Status == 304 && (etag != "" || lastmod != ""):
		return anchorlessKept, nil
	case res.Status != 200:
		return anchorlessUnknown, fmt.Errorf("%s: status %d: %w", name, res.Status, errUnexpectedStatus)
	}
	sum := sha256.Sum256(res.Body)
	hash, old := hex.EncodeToString(sum[:]), *row.BlobHash
	pass, sniffable := aptpath.SniffAnchorlessIndex(name, res.Body)
	switch {
	case !sniffable && hash == old:
		return anchorlessKept, nil
	case !sniffable:
		return anchorlessDeleted, nil
	case !pass:
		anchorlessSniffRejectedTotal.Inc(suite.CanonicalHost)
		c.logger.Debug("freshness: anchorless index failed its sniff, kept",
			"canonical_host", suite.CanonicalHost, "path", row.Path)
		return anchorlessKept, nil
	case hash == old && etag == res.ETag && lastmod == res.LastModified:
		return anchorlessKept, nil
	}
	stored, err := c.adopter.writeBlobBytes(ctx, res.Body)
	if err != nil {
		return anchorlessUnknown, fmt.Errorf("%s: store: %w", name, err)
	}
	updated, err := c.adopter.cache.RefreshURLPathBlob(ctx, suite.CanonicalScheme, suite.CanonicalHost, row.Path, old, stored,
		nonEmpty(res.ETag), nonEmpty(res.LastModified))
	if err != nil {
		return anchorlessUnknown, fmt.Errorf("%s: store: %w", name, err)
	}
	if updated && stored != old {
		return anchorlessReplaced, nil
	}
	return anchorlessKept, nil
}
```

In `internal/aptpath/sniff.go`, replace

```go
// flatIndexExt returns the compression extension of a flat index file name
// (Packages or Sources, optionally with one of the flat grammar's
// extensions; "" for none).
```

with

```go
// IsAnchorlessIndex reports whether name, a flat suite's root member name,
// is a file the anchorless refresh revalidates (spec C4 Dispatch 4): a
// Packages*, Sources* or Contents-* name of spec C2's flat member grammar.
func IsAnchorlessIndex(name string) bool {
	_, ok := anchorlessIndexExt(name)
	return ok
}

// SniffAnchorlessIndex reports whether head, the start of a 200 body for
// the anchorless index name, may replace its cached copy (spec C4
// Dispatch 4: "the flat index sniff for its extension"): a Packages* or
// Sources* name takes SniffFlatIndex, a compressed Contents-* its
// extension's magic. sniffable is false for a plain Contents-* — no sniff
// tells it from a soft-404, so its body is never stored (plan deviation
// 2) — and for a name that is no anchorless index.
func SniffAnchorlessIndex(name string, head []byte) (pass, sniffable bool) {
	ext, ok := anchorlessIndexExt(name)
	switch {
	case !ok:
		return false, false
	case !strings.HasPrefix(name, "Contents-"):
		return SniffFlatIndex(name, head), true
	case ext == "":
		return false, false
	}
	return bytes.HasPrefix(head, flatIndexMagic[ext]), true
}

// anchorlessIndexExt returns the compression extension of an anchorless
// index name ("" for none), and whether name is one.
func anchorlessIndexExt(name string) (string, bool) {
	if ext, ok := flatIndexExt(name); ok {
		return ext, true
	}
	if !strings.HasPrefix(name, "Contents-") || !flatMemberRE.MatchString(name) {
		return "", false
	}
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		return name[i:], true
	}
	return "", true
}

// flatIndexExt returns the compression extension of a flat index file name
// (Packages or Sources, optionally with one of the flat grammar's
// extensions; "" for none).
```

In `internal/cache/adoption.go`, replace

```go
// stamped above it. C-11's own readers do not care then (the row is
// gone); a durable per-suite floor needs a schema change, which phase F
// forbids.
func adoptionStampTx(
```

with

```go
// stamped above it. C-11's own readers do not care then (the row is
// gone); a durable per-suite floor needs a schema change, which phase F
// forbids.
//
// AIDEV-NOTE: ruling R-G3 (phase G). url_path-served metadata answers
// Last-Modified = max(pool mtime, last_fetched_at) (the handler's
// urlPathModTime), so the first-adoption floor below (the anchors'
// MAX(last_fetched_at) + 1) passes every url_path anchor's Last-Modified
// only while no pool file is newer than its row's last_fetched_at:
// runFetch and the anchorless refresh (RefreshURLPathBlob) finalize the
// pool file before they stamp the row. Keep that order; blob mtimes are
// not folded into this floor
// (TestServeHTTP_AMissStampsItsRowAfterItsPoolFile).
func adoptionStampTx(
```

In `internal/freshness/metrics.go`, replace

```go
// counter by host); acu_suites_reaped_total (spec C9, design review M-16,
// counter by reason: idle, non_canonical, junk). The flip barrier's
```

with

```go
// counter by host); acu_suites_reaped_total (spec C9, design review M-16,
// counter by reason: idle, non_canonical, junk);
// acu_anchorless_sniff_rejected_total (spec C4 Dispatch 4, counter by
// host). The flip barrier's
```

In `internal/freshness/metrics.go`, replace

```go
	// freshnessStaleSuites is the per-tick gauge
```

with

```go
	// anchorlessSniffRejectedTotal counts the anchorless refresh's 200
	// answers whose body failed the flat index sniff (spec C4 Dispatch 4,
	// "counter anchorless_sniff_rejected"): the cached row stays. By host,
	// so an upstream that answers HTML shows where it is.
	anchorlessSniffRejectedTotal = metrics.NewCounterWithCap(
		"acu_anchorless_sniff_rejected_total",
		"Anchorless refresh 200 answers whose body failed the flat index sniff; the cached row stays (spec C4).",
		metrics.DefaultMaxSeries,
		"host",
	)

	// freshnessStaleSuites is the per-tick gauge
```

In `internal/freshness/first_adoption.go`, replace

```go
// check). A probe that ends unknown and a tree that finds no anchor (which
// also records anchor_absent, so the status page shows it: ruling PE-2)
// are failed checks; an unsigned candidate while adoption.adopt_unsigned
```

with

```go
// check). A probe that ends unknown and a tree that finds no anchor (which
// also records anchor_absent, so the status page shows it: ruling PE-2)
// are failed checks — except that a flat suite whose tree finds no anchor
// and that has no signed evidence (Adopter.anchorlessEligible) runs the
// anchorless refresh instead (anchorlessRefresh, spec C4 Dispatch 4); an
// unsigned candidate while adoption.adopt_unsigned
```

In `internal/freshness/first_adoption.go`, replace

```go
	case !t.found:
		// Ruling PE-2: still a failed check (spec C4; the streak backs
		// off), and the refusal state anchor_absent shows it on the status
		// page; the record also paces the suite (pacedFirst).
		c.recordAdoptionFailure(
```

with

```go
	case !t.found:
		// Spec C4 Dispatch 3: a flat suite without signed evidence is a
		// Release-less repository, refreshed anchorless (Dispatch 4).
		if aptpath.IsFlatSuitePath(suitePath) {
			eligible, err := c.adopter.anchorlessEligible(ctx, suite)
			if err != nil {
				if !c.endedByShutdown(ctx, suite, "the anchorless gate", err) {
					c.failCheck(ctx, out, cur, suite, nowUnix, "anchorless", err)
				}
				return nil
			}
			if eligible {
				c.anchorlessRefresh(ctx, fetchCtx, out, cur, suite, nowUnix, t)
				return nil
			}
		}
		// Ruling PE-2: still a failed check (spec C4; the streak backs
		// off), and the refusal state anchor_absent shows it on the status
		// page; the record also paces the suite (pacedFirst).
		c.recordAdoptionFailure(
```

In `internal/handler/handler.go`, replace

```go
// header. lastFetchedAt is the source for X-Cache-Age and may be nil.
// modTime, when set, is the response's Last-Modified instead of the pool
// file's mtime (spec C8: a snapshot anchor's adopted_at).
```

with

```go
// header. lastFetchedAt is the source for X-Cache-Age and may be nil; a
// url_path serve's, it also floors a metadata file's Last-Modified
// (urlPathModTime). modTime, when set, is the response's Last-Modified
// instead of the pool file's mtime (spec C8: a snapshot anchor's
// adopted_at).
```

In `internal/handler/handler.go`, replace

```go
	cw := &countingWriter{ResponseWriter: w}
	http.ServeContent(cw, r, req.Path, modTimeOr(meta.modTime, st.ModTime()), f)
	return true, cw.statusCode(), cw.bytes
}
```

with

```go
	cw := &countingWriter{ResponseWriter: w}
	http.ServeContent(cw, r, req.Path, urlPathModTime(req, modTimeOr(meta.modTime, st.ModTime()), meta.lastFetchedAt), f)
	return true, cw.statusCode(), cw.bytes
}
```

In `internal/handler/handler.go`, replace

```go
// modTimeOr returns modTime, or the pool file's mtime when modTime is
// zero.
```

with

```go
// urlPathModTime is the Last-Modified of a url_path serve of req (spec
// C4, plan design note 14): for a metadata file, the later of mtime (its
// pool file's) and lastFetchedAt (its row's last_fetched_at), so apt's
// If-Modified-Since on an index sees a replacement whose bytes were
// already in the pool (the anchorless refresh points the row at an
// existing pool file and moves last_fetched_at past the previous stamp);
// a .deb, and a serve with no stamp (lastFetchedAt nil: a snapshot or
// by-hash serve), keep mtime.
//
// AIDEV-NOTE: ruling R-G3. adoptionStampTx's first-adoption floor is the
// anchors' MAX(last_fetched_at) + 1, which passes this only while no pool
// file is newer than its row's last_fetched_at: every writer of a row's
// stamp (runFetch, cache.RefreshURLPathBlob) reads it after the pool file
// is final. Blob mtimes are not folded into that floor.
func urlPathModTime(req *proxy.Request, mtime time.Time, lastFetchedAt *int64) time.Time {
	if !req.IsMetadata || lastFetchedAt == nil {
		return mtime
	}
	if fetched := time.Unix(*lastFetchedAt, 0); fetched.After(mtime) {
		return fetched
	}
	return mtime
}

// modTimeOr returns modTime, or the pool file's mtime when modTime is
// zero.
```

In `internal/handler/handler.go`, replace

```go
	cw := &countingWriter{ResponseWriter: w}
	http.ServeContent(cw, r, req.Path, st.ModTime(), f)
	v := packageValidation{Validated: res.validatedHash, PackageName: res.packageName, Suites: res.vouchingSuites}
```

with

```go
	cw := &countingWriter{ResponseWriter: w}
	http.ServeContent(cw, r, req.Path, urlPathModTime(req, st.ModTime(), &res.fetchedAt), f)
	v := packageValidation{Validated: res.validatedHash, PackageName: res.packageName, Suites: res.vouchingSuites}
```

In `internal/handler/handler.go`, replace

```go
	if err := h.cache.PutBlob(dbCtx, hash, fres.ContentLength); err != nil {
		return sfResult{err: fmt.Errorf("handler: put blob row: %w", err), status: fres.Status}
	}

	now := wallclock.Unix()
```

with

```go
	if err := h.cache.PutBlob(dbCtx, hash, fres.ContentLength); err != nil {
		return sfResult{err: fmt.Errorf("handler: put blob row: %w", err), status: fres.Status}
	}

	// AIDEV-NOTE: ruling R-G3. The row's stamp is read here, after the pool
	// file is final: a metadata serve's Last-Modified is max(pool mtime,
	// last_fetched_at) (urlPathModTime), and adoptionStampTx's
	// first-adoption floor (the anchors' MAX(last_fetched_at) + 1) passes it
	// only while no pool file is newer than its row's stamp. Never read it
	// before the body is in the pool
	// (TestServeHTTP_AMissStampsItsRowAfterItsPoolFile).
	now := wallclock.Unix()
```

In `internal/handler/handler.go`, replace

```go
		created:        stored.Create.Outcome == cache.SuiteCreated,
		unproven:       stored.Create.Outcome == cache.SuiteCreated && stored.Create.Unproven,
	}
}
```

with

```go
		created:        stored.Create.Outcome == cache.SuiteCreated,
		unproven:       stored.Create.Outcome == cache.SuiteCreated && stored.Create.Unproven,
		fetchedAt:      now,
	}
}
```

In `internal/handler/handler.go`, replace

```go
	cw := &countingWriter{ResponseWriter: w}
	http.ServeContent(cw, r, req.Path, st.ModTime(), f)
	// Spec C3: a stale anchor serve records client interest too.
```

with

```go
	cw := &countingWriter{ResponseWriter: w}
	http.ServeContent(cw, r, req.Path, urlPathModTime(req, st.ModTime(), row.LastFetchedAt), f)
	// Spec C3: a stale anchor serve records client interest too.
```

In `internal/handler/singleflight.go`, replace

```go
	// snapshotCount: the strict-mode current_snapshot_count of an
	// errUnvouchedRetryable result, for the refusal's log line.
	snapshotCount int
}
```

with

```go
	// snapshotCount: the strict-mode current_snapshot_count of an
	// errUnvouchedRetryable result, for the refusal's log line.
	snapshotCount int
	// fetchedAt: the last_fetched_at runFetch stored with the row, for the
	// miss serve's Last-Modified (urlPathModTime).
	fetchedAt int64
}
```

Deferred minor E-47 (test-only):

In `internal/freshness/retry_rig_test.go`, replace

```go
func (v *rigVerifier) calls() int         { v.mu.Lock(); defer v.mu.Unlock(); return v.n }
```

with

```go
func (v *rigVerifier) calls() int         { v.mu.Lock(); defer v.mu.Unlock(); return v.n }

// resetCalls restarts the verifier's call count at 0 (deferred minor E-47).
func (v *rigVerifier) resetCalls() {
	v.mu.Lock()
	v.n = 0
	v.mu.Unlock()
}
```

In `internal/freshness/retry_rig_test.go`, replace

```go
// retryRig wires a real cache, Adopter and Checker for suite /r/dists/x
// on host h, whose cached InRelease anchor is stale (testHex64), so any
// body the upstream serves is "changed". The clock starts at
// 1_800_000_000 and moves only when the test advances it; jitter is off
// (rand = 0.5).
type retryRig struct {
```

with

```go
// rigStart is the rigs' clock start, and their suite's interest stamp
// (deferred minor E-47).
const rigStart = 1_800_000_000

// retryRig wires a real cache, Adopter and Checker for suite /r/dists/x
// on host h, whose cached InRelease anchor is stale (testHex64), so any
// body the upstream serves is "changed". The clock starts at rigStart and
// moves only when the test advances it; jitter is off (rand = 0.5).
type retryRig struct {
```

In `internal/freshness/retry_rig_test.go`, replace

```go
	interest := int64(1_800_000_000)
	if err := c.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite,
```

with

```go
	interest := int64(rigStart)
	if err := c.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite,
```

In `internal/freshness/retry_rig_test.go`, replace

```go
		clock: &rigClock{t: time.Unix(1_800_000_000, 0)},
		ring:  observability.NewRing(50), logs: &lockedBuffer{},
```

with

```go
		clock: &rigClock{t: time.Unix(rigStart, 0)},
		ring:  observability.NewRing(50), logs: &lockedBuffer{},
```

In `internal/freshness/retry_rig_test.go`, replace

```go
		r.t.Fatal("rig: base content not adopted")
	}
	r.ver.mu.Lock()
	r.ver.n = 0
	r.ver.mu.Unlock()
}
```

with

```go
		r.t.Fatal("rig: base content not adopted")
	}
	r.ver.resetCalls()
}
```

In `internal/freshness/retry_rig_test.go`, replace

```go
		r.t.Fatal("rig: base pair not adopted")
	}
	r.ver.mu.Lock()
	r.ver.n = 0
	r.ver.mu.Unlock()
}
```

with

```go
		r.t.Fatal("rig: base pair not adopted")
	}
	r.ver.resetCalls()
}
```

In `internal/freshness/flat_rig_test.go`, replace

```go
// seeded: each test caches what its clients fetched (cacheRow). The clock
// starts at 1_800_000_000 and moves only when the test advances it.
```

with

```go
// seeded: each test caches what its clients fetched (cacheRow). The clock
// starts at rigStart and moves only when the test advances it.
```

In `internal/freshness/flat_rig_test.go`, replace

```go
		clock: &rigClock{t: time.Unix(1_800_000_000, 0)},
```

with

```go
		clock: &rigClock{t: time.Unix(rigStart, 0)},
```

In `internal/freshness/flat_rig_test.go`, replace

```go
	interest := int64(1_800_000_000)
```

with

```go
	interest := int64(rigStart)
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/aptpath/ ./internal/cache/ ./internal/freshness/ ./internal/handler/`
Expected: PASS (`TestServeHTTP_AMissStampsItsRowAfterItsPoolFile` takes about 1.1 s: its upstream answers after 1.1 s of wall time, so the body reaches the pool in a later second than the request).

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| no gate (`if eligible \|\| true`) | `TestCheck_AnchorlessGateKeepsSignedSuitesOut/a_signed-seen_marker`, `/a_signed_InRelease_cached_under_the_twin_over_https`, `/a_Release.gpg_cached` (`result "anchorless_refreshed", want failed`) |
| the gate without the re-mark | `…/a_signed_InRelease_cached_under_the_twin_over_https`, `/a_Release.gpg_cached`, `/an_unreadable_pool_file` |
| the gate without never-downgrade's rule | `…/a_signed-seen_marker`, `/a_signed_InRelease_cached_under_the_twin_over_https`, `/a_Release.gpg_cached` |
| the gate for `dists/` suites too | `…/a_dists_suite` (`result "anchorless_refreshed", want failed`) |
| a re-mark error is "not eligible" (records `anchor_absent`) | `…/an_unreadable_pool_file` (`anchor_absent recorded = true, want false`) |
| a 304 is unknown | `TestCheck_AnchorlessAnswersByStatus/one_answer_is_enough`, `TestCheck_AnchorlessDeletesTheAnchorsUpstreamDropped`, `TestCheck_AnchorlessClearsTheRecordAndThePendingChange`, `TestCheck_AnAnchorlessSuiteThatPublishesAnInReleaseAdopts` (`result "failed", …`) |
| a sniff-failing 200 replaces | `TestCheck_AnchorlessRefreshRevalidatesTheIndexes` (`Packages row = …; want it kept`), `TestCheck_AnchorlessSniffRejectionKeepsTheRow` |
| no sniff-rejected count | `TestCheck_AnchorlessRefreshRevalidatesTheIndexes` (`rose by 0, want 1`), `TestCheck_AnchorlessSniffRejectionKeepsTheRow` (`rose by 0, want 2`) |
| 404/410 keep the row | `TestCheck_AnchorlessRefreshRevalidatesTheIndexes` (`Contents-amd64.gz row = …; want it deleted`), `TestCheck_AnchorlessAnswersByStatus/404`, `/410`, `TestCheck_AnchorlessRefreshesItsOwnRowsOnly` |
| 401/403 delete the row | `TestCheck_AnchorlessAnswersByStatus/401`, `/403` (`result "anchorless_refreshed", want "anchorless_unchanged"`) |
| a 5xx, a transport error or a body over the cap is an answer | `TestCheck_AnchorlessAnswersByStatus/503`, `/a_transport_error`, `/a_body_over_the_cap` (`result "anchorless_unchanged", want "failed"`) |
| a changed plain `Contents-*` is kept | `TestCheck_AnchorlessRefreshRevalidatesTheIndexes` (`Contents-i386 row = …; want it deleted`) |
| an unchanged plain `Contents-*` is deleted | `TestCheck_AnchorlessRefreshRevalidatesTheIndexes` (`Contents-all row = <nil>; want it kept`) |
| no anchor deletes | `TestCheck_AnchorlessDeletesTheAnchorsUpstreamDropped/Release_answered_404`, `/Release_answered_401` |
| anchors deleted on any absent answer (401 too) | `TestCheck_AnchorlessDeletesTheAnchorsUpstreamDropped/Release_answered_401` |
| every request unknown is no failure | `TestCheck_AnchorlessAnswersByStatus/503`, `/a_transport_error`, `/a_body_over_the_cap` |
| the record kept (`clearRecord` false) | `TestCheck_AnchorlessClearsTheRecordAndThePendingChange` |
| the pending change kept | `TestCheck_AnchorlessClearsTheRecordAndThePendingChange` |
| a replacement alone is `anchorless_unchanged` | `TestCheck_AnchorlessAnswersByStatus/200_with_new_bytes` |
| a shutdown goes on (no `context.Canceled` exit) | `TestCheck_AnchorlessShutdownWritesNothing` (`Sources.gz requested 1 times, want 0`) |
| every cached row refreshed, anchors included | `TestCheck_AnchorlessDeletesTheAnchorsUpstreamDropped` (`Release.gpg <nil>`) |
| no size-cap WARN | `TestCheck_AnchorlessAnswersByStatus/a_body_over_the_cap` (`0 size-cap WARN lines, want 1`) |
| no validators sent | `TestCheck_AnchorlessRefreshRevalidatesTheIndexes`, `TestCheck_AnchorlessSniffRejectionKeepsTheRow` (`ETags sent [""], want [...]`) |
| `RefreshURLPathBlob` overwrites a row whose blob changed | `TestRefreshURLPathBlob_ReplacesTheBlobAndKeepsTheInterest` (`… of a changed row = true, <nil>; want false`) |
| `RefreshURLPathBlob` stamps an unchanged blob | `TestRefreshURLPathBlob_ReplacesTheBlobAndKeepsTheInterest` (`/flat/Packages.gz` row) |
| `RefreshURLPathBlob` not monotonic (`MAX(?, 0)`) | `TestRefreshURLPathBlob_ReplacesTheBlobAndKeepsTheInterest` (`/flat/Sources` row) |
| `RefreshURLPathBlob` without the new hash's check | `TestRefreshURLPathBlob_ReplacesTheBlobAndKeepsTheInterest` (`FOREIGN KEY constraint failed …, want ErrInvalidHash`) |
| a plain `Contents-*` sniffable | `TestSniffAnchorlessIndex` (`("Contents-amd64", …) = true, true; want false, false`) |
| `Contents-*` outside the flat member grammar | `TestSniffAnchorlessIndex` (`("Contents-AMD64.gz", …) = true, true; want false, false`) |
| `urlPathModTime` ignores the stamp | `TestServeHTTP_URLPathMetadataLastModifiedIsTheLaterStamp/hits`, `/misses`, `/a_stale_serve` |
| `urlPathModTime` for a `.deb` too | `…/hits` (`/flat/x_1_amd64.deb`), `/misses` (`/flat/y_1_amd64.deb`) |
| the miss serves its pool file's mtime | `…/misses` |
| the stale serve serves its pool file's mtime | `…/a_stale_serve` |
| R-G3: `runFetch` reads its stamp before the fetch | `TestServeHTTP_AMissStampsItsRowAfterItsPoolFile` (`row stamp N, pool mtime N+1, …`) |

Equivalent mutant (recorded): dropping the "same bytes and same validators" short cut in `refreshAnchorlessRow` only adds a write (`RefreshURLPathBlob` of the same blob and validators changes nothing). Not pinned: a 304 to a request without validators (the rig upstream never sends one; `classify` has the same rule) and a refresh whose `RefreshURLPathBlob` finds the row changed under it (counted `anchorless_unchanged`; `TestRefreshURLPathBlob_ReplacesTheBlobAndKeepsTheInterest` pins the cache side).

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/aptpath internal/cache internal/freshness internal/handler
git add internal/aptpath/sniff.go internal/aptpath/sniff_test.go internal/cache/anchorless.go internal/cache/anchorless_test.go internal/cache/adoption.go internal/freshness/anchorless.go internal/freshness/anchorless_test.go internal/freshness/first_adoption.go internal/freshness/metrics.go internal/freshness/retry_rig_test.go internal/freshness/flat_rig_test.go internal/handler/handler.go internal/handler/singleflight.go internal/handler/urlpath_lastmod_test.go
git commit -m "Refresh Release-less flat suites anchorless; url_path Last-Modified is max(pool mtime, last_fetched_at) (spec C4 Dispatch 3-4, ruling R-G3)"
```

(`gofmt -l` may list only the five drift files of the Global Constraints. Record the mutation table in the commit message.)

### Task 11: The flat strict matrix and `current_snapshot_count` (PR #8 parity item 4)

PR #8 parity item 4 and design note 17: PR #8's nine `TestPhase3StrictMode_FlatSnapshotScopedToItsDirectory` cases and three new ones, re-expressed for C6's vouching and C10's host-wide rule, run through the real handler with snapshots committed through the cache (`package_hash` rows at each suite's `FlatBase` with their `flat_filename`); the two pkgs.k8s.io rows use the handler's `AdoptionWaiter` seam (`fakeAdoptions`). PR #8's `TestPhase3StrictMode_SnapshotCountExcludesOutOfScopeFlat` becomes `TestServeHTTP_CurrentSnapshotCountCountsFlatSnapshots`: every signed current snapshot counts, flat ones and twins included, and never an unsigned snapshot or an anchorless suite. Tasks 4, 5 and 10 built the behaviour (`cache.HostCurrentSnapshotsCoverage` was already host-wide and signed-only), so the code change is one AIDEV-NOTE on that query; the tests are the deliverable, and the mutation step shows each row is load-bearing. Deferred minor E-70 (`unsigned_posture_test.go`, the other `current_snapshot_count` test) is taken: it requires exactly one refusal record, so the count is always read.

**Files:**
- Create: `internal/handler/flat_strict_test.go`
- Modify: `internal/cache/queries.go` (AIDEV-NOTE in `HostCurrentSnapshotsCoverage`), `internal/handler/unsigned_posture_test.go` (E-70)

**Interfaces:**
- Consumes: `newPhase3StrictHandlerCapturing`, `strictLogBuf.find`, `splitURL`, `hostKey`, `proxyReq`, `writeBlob`, `sha256Hex`, `commitUnsigned`, `fakeAdoptions` (handler tests); `cache.PackageHash.FlatFilename` (Task 4); the suffix fallback (Task 5).
- Produces: `strictSnap`, `commitStrictSnap` (handler tests) — Task 15 uses them.

- [ ] **Step 1: Write the tests**

Create `internal/handler/flat_strict_test.go`:

```go
package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// strictSnap is one current snapshot of a strict matrix row: its suite,
// its coverage, and its package_hash rows — each .deb's path (at the
// suite's FlatBase, as spec C6 materialises it) and flat_filename ("" for
// a dists/ row) — declaring the strict rig upstream's bytes.
type strictSnap struct {
	suite    string
	complete bool
	debs     map[string]string
}

// commitStrictSnap adopts s on host through the cache (an inline
// snapshot) and returns its id.
func commitStrictSnap(t *testing.T, h *Handler, scheme, host string, s strictSnap) int64 {
	t.Helper()
	ctx := context.Background()
	inrel := writeBlob(t, h, simtest.ClearsignedFixture("InRelease for "+s.suite))
	if err := h.cache.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: scheme, CanonicalHost: host,
		SuitePath: s.suite}); err != nil {
		t.Fatal(err)
	}
	id, _, err := h.cache.InsertTrackedCandidateForTest(ctx, cache.SnapshotCandidate{CanonicalScheme: scheme,
		CanonicalHost: host, SuitePath: s.suite, InReleaseHash: &inrel})
	if err != nil {
		t.Fatal(err)
	}
	var rows []cache.PackageHash
	for path, flat := range s.debs {
		rows = append(rows, cache.PackageHash{SnapshotID: id, CanonicalScheme: scheme, CanonicalHost: host, Path: path,
			FlatFilename: flat, DeclaredSHA256: sha256Hex([]byte("upstream bytes"))})
	}
	if err := h.cache.CommitAdoption(ctx, id, []cache.SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: inrel,
		DeclaredSHA256: inrel}}, nil, rows, nil, s.complete); err != nil {
		t.Fatal(err)
	}
	return id
}

// PR #8 parity item 4 (plan design note 17): PR #8's nine
// TestPhase3StrictMode_FlatSnapshotScopedToItsDirectory cases under C6
// vouching and C10's host-wide rule, and three new ones, through the real
// handler. A vouched .deb is served after its hash check; an unvouched
// one on a host whose every signed current snapshot is complete is
// refused (502, Retry-After 60, unvouched_deb_refused), whatever its
// layout; an incomplete snapshot of either layout turns refusal off for
// its host (unvouched_deb_passthrough_no_coverage names it); and a .deb
// of a pkgs.k8s.io minor whose first adoption is running is served once
// that adoption commits within C10's wait, but refused while it is only
// queued.
func TestServeHTTP_FlatStrictMatrix(t *testing.T) {
	const unknownDists = "/ubuntu/pool/main/u/unknown_1_amd64.deb"
	k8s := func(minor string) strictSnap {
		suite := "/core:/stable:/v1." + minor + "/deb"
		return strictSnap{suite: suite, complete: true,
			debs: map[string]string{suite + "/amd64/kubectl_1." + minor + ".0-1.1_amd64.deb": "amd64/kubectl_1." + minor + ".0-1.1_amd64.deb"}}
	}
	var (
		dists      = strictSnap{suite: "/ubuntu/dists/noble", complete: true, debs: map[string]string{"/ubuntu/pool/main/h/hello_1_amd64.deb": ""}}
		incDists   = strictSnap{suite: "/ubuntu/dists/noble", complete: false}
		flat       = strictSnap{suite: "/flat", complete: true, debs: map[string]string{"/flat/amd64/x.deb": "amd64/x.deb"}}
		incFlat    = strictSnap{suite: "/flat", complete: false, debs: map[string]string{"/flat/amd64/x.deb": "amd64/x.deb"}}
		twin       = strictSnap{suite: "/flat/.", complete: true, debs: map[string]string{"/flat/amd64/x.deb": "amd64/x.deb"}}
		jenkins    = strictSnap{suite: "/r/sub", complete: true, debs: map[string]string{"/r/sub/amd64/x.deb": "amd64/x.deb"}}
		thirdMinor = k8s("35")
	)
	for _, tc := range []struct {
		name  string
		snaps []strictSnap
		path  string
		want  int
		// adoption: "running" commits thirdMinor during C10's wait,
		// "queued" reports no adoption in flight, "" wires none.
		adoption string
		// passthrough: the index in snaps of the snapshot
		// unvouched_deb_passthrough_no_coverage names; -1 for none.
		passthrough int
	}{
		{"complete dists and flat: an unvouched dists .deb", []strictSnap{dists, flat}, unknownDists, 502, "", -1},
		{"the same host: a declared flat .deb", []strictSnap{dists, flat}, "/flat/amd64/x.deb", 200, "", -1},
		{"the same host: an undeclared .deb under the flat suite", []strictSnap{dists, flat}, "/flat/amd64/other.deb", 502, "", -1},
		{"suite /flat/.: a declared .deb at its FlatBase", []strictSnap{dists, twin}, "/flat/amd64/x.deb", 200, "", -1},
		{"suite /flat/.: the declared .deb by its ./ path", []strictSnap{dists, twin}, "/flat/./amd64/x.deb", 200, "", -1},
		{"a .. path", []strictSnap{dists, flat}, "/flat/../ubuntu/pool/unknown.deb", 502, "", -1},
		{"the Jenkins shape: a parent base", []strictSnap{dists, jenkins}, "/r/amd64/x.deb", 200, "", -1},
		{"a flat-only host: an undeclared .deb elsewhere", []strictSnap{flat}, "/elsewhere/y_1_amd64.deb", 502, "", -1},
		{"an incomplete dists snapshot and a flat one: an unvouched .deb", []strictSnap{incDists, flat}, unknownDists, 200, "", 0},
		{"complete dists and an incomplete flat snapshot: an unvouched dists .deb", []strictSnap{dists, incFlat}, unknownDists, 200, "", 1},
		{"pkgs.k8s.io: a third minor's first adoption is running", []strictSnap{k8s("33"), k8s("34")},
			"/core:/stable:/v1.35/deb/amd64/kubectl_1.35.0-1.1_amd64.deb", 200, "running", -1},
		{"pkgs.k8s.io: a third minor's first adoption is queued", []strictSnap{k8s("33"), k8s("34")},
			"/core:/stable:/v1.35/deb/amd64/kubectl_1.35.0-1.1_amd64.deb", 502, "queued", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, srv, logs := newPhase3StrictHandlerCapturing(t, true, true, nil)
			scheme, host, port := splitURL(t, srv.URL)
			canon := hostKey(host, port)
			var ids []int64
			for _, s := range tc.snaps {
				ids = append(ids, commitStrictSnap(t, h, scheme, canon, s))
			}
			a := &fakeAdoptions{}
			switch tc.adoption {
			case "running":
				a.inFlight, a.ok = true, true
				a.during = func() {
					commitStrictSnap(t, h, scheme, canon, thirdMinor)
					a.inFlight = false
				}
				h.adoptions = a
			case "queued":
				h.adoptions = a
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, proxyReq("GET", srv.URL, tc.path))
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d", rec.Code, tc.want)
			}
			refused := logs.find("unvouched_deb_refused")
			switch {
			case tc.want == http.StatusOK && rec.Body.String() != "upstream bytes":
				t.Fatalf("body %q, want the upstream's", rec.Body.String())
			case tc.want == http.StatusOK && len(refused) != 0:
				t.Fatalf("served, but logged %v", refused)
			case tc.want == http.StatusBadGateway && (len(refused) != 1 || rec.Header().Get("Retry-After") != "60"):
				t.Fatalf("refused with Retry-After %q and %d unvouched_deb_refused lines, want 60 and 1",
					rec.Header().Get("Retry-After"), len(refused))
			}
			pass := logs.find("unvouched_deb_passthrough_no_coverage")
			if tc.passthrough < 0 {
				if len(pass) != 0 {
					t.Fatalf("unexpected passthrough line %v", pass)
				}
			} else if len(pass) != 1 || pass[0]["incomplete_snapshot_id"] != float64(ids[tc.passthrough]) {
				t.Fatalf("passthrough lines %v, want one naming snapshot %d", pass, ids[tc.passthrough])
			}
			if waits := a.waits.Load(); waits != map[string]int32{"running": 1}[tc.adoption] {
				t.Fatalf("waited %d times for adoptions in flight", waits)
			}
		})
	}
}

// PR #8's TestPhase3StrictMode_SnapshotCountExcludesOutOfScopeFlat, under
// C10: current_snapshot_count counts every signed current snapshot on the
// host — a flat suite's and its /. twin's included — and never an
// unsigned snapshot or an anchorless suite (a row without a snapshot).
func TestServeHTTP_CurrentSnapshotCountCountsFlatSnapshots(t *testing.T) {
	h, srv, logs := newPhase3StrictHandlerCapturing(t, true, true, nil)
	scheme, host, port := splitURL(t, srv.URL)
	canon := hostKey(host, port)
	for _, s := range []strictSnap{{suite: "/ubuntu/dists/noble", complete: true}, {suite: "/flat", complete: true},
		{suite: "/flat/.", complete: true}} {
		commitStrictSnap(t, h, scheme, canon, s)
	}
	commitUnsigned(t, h, scheme, canon, "/unsigned", "/unsigned/amd64/u_1_amd64.deb")
	if err := h.cache.SeedSuiteForTest(context.Background(), cache.SuiteFreshness{CanonicalScheme: scheme,
		CanonicalHost: canon, SuitePath: "/anchorless"}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/pool/main/u/unknown_1_amd64.deb"))
	refused := logs.find("unvouched_deb_refused")
	if rec.Code != http.StatusBadGateway || len(refused) != 1 || refused[0]["current_snapshot_count"] != float64(3) {
		t.Fatalf("status %d, refusals %v; want 502 with current_snapshot_count 3", rec.Code, refused)
	}
}
```

In `internal/handler/unsigned_posture_test.go`, replace

```go
		for _, r := range logs.records() {
			if r["msg"] == "unvouched_deb_refused" && r["current_snapshot_count"] != float64(1) {
				t.Fatalf("current_snapshot_count %v, want 1 (the unsigned snapshot does not count)", r["current_snapshot_count"])
			}
		}
```

with

```go
		// Deferred minor E-70: exactly one refusal, so the count is read.
		refused := logs.find("unvouched_deb_refused")
		if len(refused) != 1 || refused[0]["current_snapshot_count"] != float64(1) {
			t.Fatalf("refusals %v, want one with current_snapshot_count 1 (the unsigned snapshot does not count)", refused)
		}
```

- [ ] **Step 2: Run them**

Run: `go test ./internal/handler/ -run 'TestServeHTTP_FlatStrictMatrix|TestServeHTTP_CurrentSnapshotCountCountsFlatSnapshots|TestServeHTTP_UnsignedNeverChangesStrictPosture' -v`
Expected: PASS, twelve matrix subtests. (No red step of their own: Tasks 4, 5 and 10 made them pass. Before Task 5, `suite_/flat/.:_the_declared_.deb_by_its_./_path` and `the_Jenkins_shape:_a_parent_base` fail with `status 502, want 200`; the mutations below show the rest.)

- [ ] **Step 3: The AIDEV-NOTE**

In `internal/cache/queries.go`, replace

```go
	// AIDEV-NOTE: spec C5/C10 — unsigned snapshots never count for strict
	// mode (they vouch nothing), so they are not "snapshots in force" here.
	const q = `
```

with

```go
	// AIDEV-NOTE: spec C5/C10 — unsigned snapshots never count for strict
	// mode (they vouch nothing), so they are not "snapshots in force" here.
	// Flat snapshots (and /. twins) count like dists/ ones: C10's rule is
	// host-wide, and C6's vouching replaces PR #8's per-directory scoping;
	// an anchorless suite has no snapshot (TestServeHTTP_FlatStrictMatrix,
	// TestServeHTTP_CurrentSnapshotCountCountsFlatSnapshots).
	const q = `
```

- [ ] **Step 4: Run the package**

Run: `go test ./internal/cache/ ./internal/handler/`
Expected: PASS.

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| strict mode counts `dists/` snapshots only (`AND ss.suite_path LIKE '%/dists/%'` in `HostCurrentSnapshotsCoverage`) | `TestServeHTTP_FlatStrictMatrix/a_flat-only_host:_an_undeclared_.deb_elsewhere` (`status 200, want 502`), `/complete_dists_and_an_incomplete_flat_snapshot:_an_unvouched_dists_.deb` (`status 502, want 200`), `/pkgs.k8s.io:_a_third_minor's_first_adoption_is_running` (`waited 0 times`), `/…_is_queued` (`status 200, want 502`), `TestServeHTTP_CurrentSnapshotCountCountsFlatSnapshots` (`current_snapshot_count:1`) |
| strict mode counts unsigned snapshots (no `anchor_form <> 'unsigned'`) | `TestServeHTTP_CurrentSnapshotCountCountsFlatSnapshots` (`current_snapshot_count:4`), `TestServeHTTP_UnsignedNeverChangesStrictPosture/a_protected_host_stays_protected` (E-70: `current_snapshot_count:2`), `/an_unsigned-only_host_stays_unprotected` |
| no suffix fallback (`DeclaredHashesForPath` exact only) | `TestServeHTTP_FlatStrictMatrix/suite_/flat/.:_the_declared_.deb_by_its_./_path`, `/the_Jenkins_shape:_a_parent_base` (`status 502, want 200`) |
| the miss path never waits for an adoption in flight | `TestServeHTTP_FlatStrictMatrix/pkgs.k8s.io:_a_third_minor's_first_adoption_is_running` (`status 502, want 200`) |

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/cache/queries.go internal/handler/flat_strict_test.go internal/handler/unsigned_posture_test.go
git add internal/cache/queries.go internal/handler/flat_strict_test.go internal/handler/unsigned_posture_test.go
git commit -m "Pin the flat strict matrix and current_snapshot_count under spec C10 (PR #8 parity item 4)"
```

(Record the mutation table in the commit message.)

### Task 12: The reaper and `/forget` on flat suites and their twins (rulings R-G2, R-G4)

Ruling R-G2 (the F carry-forward; design note 15): an anchorless flat suite — one with no cached anchor row — is judged junk by its cached root `Packages*`/`Sources*` rows, through the anchors' own rules (`aptpath.SniffFlatIndex` for the row's name, fetched at least `junkGrace` ago, whatever the suite's record), read by one more indexed query per run (`cache.ListUnadoptedFlatIndexRows`, the plan shape of `ListUnadoptedSuiteAnchors`, pinned); a suite with any anchor row is judged by its anchors alone; a row without a blob, a stamp or a pool file is no evidence. The junk memo's key gains the member name for a flat index (one blob is plain-text `Packages` and junk `Packages.gz`). Ruling R-G5 (deferred minor F-12, taken): the index read runs only in a run that holds an unadopted flat suite with no anchor row (`hasAnchorlessFlatSuite`) — measured, an unconditional read took `TestDoD_NinetyDaysOfReapingWithRestarts` from about 3.0 s to 3.65 s under `-race` (about 20 %, which on phase F's reference machine, 3.75–3.96 s, would pass the 4.5 s budget); gated, it stays at 3.0 s. Ruling R-G4 and the F carry-forwards: flat suites are canonical, so idleness reaps them like any suite — `TestReaper_ReapsIdleAndNonCanonicalSuitesAndPurgesThem` gains a used flat suite (kept), an idle one (reaped) and its used `./` twin (kept, rows and all); `TestReap_AFlatSuiteLeavesItsTwin` is "reaping `/repo` leaves `/repo/.` intact" in the cache on real flat paths; and `TestDoD_ForgetIsTheEscapeHatchForAFlatSuitesDowngradeRefusal` repeats phase F's escape-hatch DoD on a real flat suite whose `./` twin keeps a row. `reaper.go`'s open deferred minors (F-18, F-22, F-26) are H's: checked and left.

**Files:**
- Create: `internal/cache/flat_reap.go` (`ListUnadoptedFlatIndexRows`), `internal/cache/flat_reap_test.go`, `internal/freshness/flat_reap_test.go`
- Modify: `internal/freshness/reaper.go` (`sniffKey.name`, `junkCandidates`, `allJunk`, `anchorEvidence` docs), `internal/freshness/reaper_test.go` (`TestReaper_ReapsIdleAndNonCanonicalSuitesAndPurgesThem`: the flat suites)

**Interfaces:**
- Consumes: `cache.SuiteAnchorRow`, `aptpath.IsFlatSuitePath`, `aptpath.FileFlatIndex`'s sniff (Task 1); the reaper rig (`newReaperRig`, `junkSuite`, `cacheAnchor`, `cacheAnchorBlob`, `record`, `reapedTotal`, `softPage`), the flat rig (Task 9), `NewReaper`, `Reaper.Forget`, `reapAge`, `fakePackagesStanzas`, `makeRelease`; cache tests' `seedRow`, `adoptWith`, `putURLPathRow`, `urlRowExists`, `stubNow`, `evictNow`, `evictDay`.
- Produces: `(*cache.Cache).ListUnadoptedFlatIndexRows(ctx) ([]cache.SuiteAnchorRow, error)`, `unadoptedFlatIndexRowsSQL`, `hasAnchorlessFlatSuite`; WARN `suite reaper: list unadopted flat indexes failed` (`err`) — Task 16 documents it.

- [ ] **Step 1: Write the failing tests**

Create `internal/cache/flat_reap_test.go`:

```go
package cache

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
)

// Ruling R-G2 (plan design note 15): ListUnadoptedFlatIndexRows lists the
// cached root Packages* and Sources* rows of every unadopted flat suite —
// a /. twin's under the twin, a suite whose path holds "DISTS" (no dists
// segment: flat) too — and nothing else: no anchor, Contents-*, pdiff
// Index or nested suite's index, no adopted suite's, no dists/ suite's
// root Packages, no non-canonical row's (an empty segment: no flat
// suite).
func TestListUnadoptedFlatIndexRows_ListsTheRootIndexesOfUnadoptedFlatSuites(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	for _, k := range []SuiteKey{sk("h", "/flat"), sk("h", "/flat/."), sk("h", "/x/DISTS/y"), sk("h", "/adopted"), sk("h", "/r/dists/x"),
		sk("h", "/a//b")} {
		seedRow(t, c, k, 1)
	}
	adoptWith(t, c, sk("h", "/adopted"), map[string]string{"InRelease": "adopted anchor"})
	blob := seedBlob(t, c, "an index")
	fetched := int64(1_700_000_000)
	for _, p := range []string{"/flat/Packages", "/flat/Packages.gz", "/flat/Sources.zst", "/flat/InRelease",
		"/flat/Contents-amd64.gz", "/flat/Packages.diff/Index", "/flat/amd64/Packages", "/flat/Translation-en",
		"/flat/./Packages.xz", "/x/DISTS/y/Packages", "/adopted/Packages", "/r/dists/x/Packages", "/a//b/Packages"} {
		if err := c.PutURLPath(ctx, URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: p, BlobHash: &blob,
			UpstreamURL: "http://h" + p, IsMetadata: true, RequestCount: 1, LastFetchedAt: &fetched}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := c.ListUnadoptedFlatIndexRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, a := range got {
		if a.SuitePath+a.Path[strings.LastIndexByte(a.Path, '/'):] != a.Path || a.BlobHash == nil || *a.BlobHash != blob ||
			a.LastFetchedAt == nil || *a.LastFetchedAt != fetched {
			t.Errorf("row %+v: want a root index of its suite with blob %s fetched at %d", a, blob, fetched)
		}
		paths = append(paths, a.Path)
	}
	want := []string{"/flat/Packages", "/flat/Packages.gz", "/flat/Sources.zst", "/flat/./Packages.xz", "/x/DISTS/y/Packages"}
	if !slices.Equal(paths, want) {
		t.Fatalf("paths = %q, want %q", paths, want)
	}
}

// Design review M-14's plan rule, for R-G2's read: it drives from
// suite_freshness and probes url_path by its unique key, never a scan.
func TestListUnadoptedFlatIndexRows_ProbesURLPathByItsKey(t *testing.T) {
	c := openCache(t)
	rows, err := c.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+unadoptedFlatIndexRowsSQL)
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

// Spec §9 G row and the F carry-forward ("reaping /repo leaves /repo/.
// intact"), on a real flat suite and its ./ twin: the reap of /repo
// deletes its metadata rows — anchors, indexes, by-hash — its row and its
// snapshot's currency; the twin keeps its row, current snapshot, rows and
// the marker of the base they share.
func TestReap_AFlatSuiteLeavesItsTwin(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, evictNow)()
	repo, twin := sk("h", "/repo"), sk("h", "/repo/.")
	ids := map[SuiteKey]int64{}
	hex := strings.Repeat("cd", 32)
	for _, k := range []SuiteKey{repo, twin} {
		seedRow(t, c, k, evictNow-40*evictDay)
		ids[k], _ = adoptWith(t, c, k, map[string]string{"InRelease": "anchor of " + k.SuitePath})
		for _, name := range []string{"Packages.gz", "by-hash/SHA256/" + hex} { // the adoption cached InRelease
			putURLPathRow(t, c, "http", "h", k.SuitePath+"/"+name, seedBlob(t, c, name+" of "+k.SuitePath), sql.NullInt64{Int64: evictNow, Valid: true}, true)
		}
	}
	if err := c.MarkSignedSeenBatch(ctx, []SignedSeenMark{{CanonicalScheme: "http", CanonicalHost: "h", Base: "/repo", SeenAt: 1}}); err != nil {
		t.Fatal(err)
	}
	if n, err := c.DeleteSuiteMetadata(ctx, repo); err != nil || n != 3 {
		t.Fatalf("DeleteSuiteMetadata(/repo) = %d, %v; want its 3 rows", n, err)
	}
	cur := ids[repo]
	if del, _, err := c.ReapIdleSuite(ctx, repo, &cur, evictNow-30*evictDay); err != nil || !del {
		t.Fatalf("reap /repo = %v, %v", del, err)
	}
	if ok, err := c.HasSuiteRow(ctx, repo); err != nil || ok {
		t.Fatalf("/repo's row: %v, %v; want it gone", ok, err)
	}
	sf, err := c.GetSuiteFreshness(ctx, "http", "h", "/repo/.")
	if err != nil || sf.CurrentSnapshotID == nil || *sf.CurrentSnapshotID != ids[twin] {
		t.Fatalf("/repo/.: row %+v, %v; want it and its current snapshot kept", sf, err)
	}
	for _, name := range []string{"InRelease", "Packages.gz", "by-hash/SHA256/" + hex} {
		if urlRowExists(t, c, "http", "h", "/repo/"+name) {
			t.Errorf("/repo/%s was not deleted", name)
		}
		if !urlRowExists(t, c, "http", "h", "/repo/./"+name) {
			t.Errorf("/repo/./%s was deleted", name)
		}
	}
	if _, ok, err := c.SignedSeen(ctx, "http", "h", "/repo"); err != nil || !ok {
		t.Fatalf("the base's marker = %v, %v; want kept", ok, err)
	}
}
```

Create `internal/freshness/flat_reap_test.go`:

```go
package freshness

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// Ruling R-G2 (the F carry-forward; Review Focus 2): an anchorless flat
// suite — no cached anchor row — is judged junk by its cached root
// Packages*/Sources* rows, by the anchors' rules: every one fails the flat
// index sniff for its name and was fetched at least 24 h ago, whatever the
// suite's record says. Reaped: HTML indexes (with or without an
// anchor_absent record); a suite whose junk anchors decide alone though its
// Packages.gz is good; a Packages.gz whose bytes are plain text (no gzip
// magic). Kept: an HTML index younger than the grace; a good index; one
// good index beside a junk one; a suite whose sniff-passing InRelease
// decides alone though its Packages.gz is HTML; a missing pool file (no
// evidence); a dists/ suite's root Packages (no flat index); the same
// plain-text bytes cached as Packages, where they pass (a verdict is
// remembered per blob and name); and /junk's ./ twin, judged by its own
// good index (ruling R-G4).
func TestReaper_ReapsAJunkAnchorlessFlatSuite(t *testing.T) {
	r := newReaperRig(t)
	old, young := reapNow-2*reapDay, reapNow-3600
	gz := "\x1f\x8b\x08\x00 an index"
	signed := string(simtest.ClearsignedFixture("Origin: x\nSHA256:\n"))
	reaped := []string{"/junk", "/absent", "/junk-anchors", "/gz-name"}
	kept := []string{"/junk/.", "/young", "/good", "/mixed", "/signed", "/missing", "/r/dists/x", "/plain-name"}
	for _, s := range append(append([]string{}, reaped...), kept...) {
		r.junkSuite(s)
	}
	r.cacheAnchor("/junk", "Packages.gz", softPage, old)
	r.cacheAnchor("/junk", "Sources", softPage, old)
	r.cacheAnchor("/junk/.", "Packages.gz", gz, old)
	r.cacheAnchor("/absent", "Packages.xz", softPage, old)
	r.record("/absent", cache.ReasonAnchorAbsent)
	r.cacheAnchor("/junk-anchors", "InRelease", softPage, old)
	r.cacheAnchor("/junk-anchors", "Packages.gz", gz, old)
	r.cacheAnchor("/gz-name", "Packages.gz", "Package: a\n", old)
	r.cacheAnchor("/plain-name", "Packages", "Package: a\n", old)
	r.cacheAnchor("/young", "Packages.gz", softPage, young)
	r.cacheAnchor("/good", "Packages.gz", gz, old)
	r.cacheAnchor("/mixed", "Packages.gz", softPage, old)
	r.cacheAnchor("/mixed", "Sources.gz", gz, old)
	r.cacheAnchor("/signed", "InRelease", signed, old)
	r.cacheAnchor("/signed", "Packages.gz", softPage, old)
	r.cacheAnchorBlob("/missing", "Packages.gz", testHex64, old)
	r.cacheAnchor("/r/dists/x", "Packages", softPage, old)
	junk0 := reapedTotal(t, "junk")
	r.reaper.runOnce(context.Background())
	for _, s := range reaped {
		if r.exists(s) {
			t.Errorf("%s was not reaped as junk", s)
		}
	}
	for _, s := range kept {
		if !r.exists(s) {
			t.Errorf("%s was reaped", s)
		}
	}
	if d := reapedTotal(t, "junk") - junk0; d != float64(len(reaped)) {
		t.Fatalf(`acu_suites_reaped_total{reason="junk"} rose by %v, want %d`, d, len(reaped))
	}
	if _, err := r.cache.LookupURL(context.Background(), "http", "h", "/junk/./Packages.gz"); err != nil {
		t.Fatalf("/junk/./Packages.gz: %v; want the twin's index kept", err)
	}
	if ev := r.events("suite reaper: junk anchor reads failed"); len(ev) != 0 {
		t.Fatalf("read failure lines = %v, want none", ev)
	}
}

// The F carry-forward (TestDoD_ForgetIsTheEscapeHatchForADowngradeRefusal's
// twin subtest, on a real flat suite): /repo, adopted signed (its probe
// marked the base), is reaped, comes back unsigned and is refused as
// unsigned_downgrade_refused. POST /forget is the escape hatch: with no
// other row for the base the markers go and the suite adopts unsigned
// after the transition's two observations; with its ./ twin keeping a row
// the markers stay and the suite stays refused.
func TestDoD_ForgetIsTheEscapeHatchForAFlatSuitesDowngradeRefusal(t *testing.T) {
	for _, twin := range []bool{false, true} {
		t.Run(map[bool]string{false: "no other row", true: "the ./ twin keeps a row"}[twin], func(t *testing.T) {
			ctx := context.Background()
			r := newFlatRig(t, "/repo")
			pkgs := fakePackagesStanzas(map[string]string{"x_1_amd64.deb": testHex64})
			signedRel, _ := makeRelease(map[string][]byte{"Packages": pkgs})
			in := simtest.ClearsignedFixture(string(signedRel))
			r.cacheRow("InRelease", in, 1, r.clock.now().Unix(), "")
			r.up.setAt(r.url("InRelease"), in)
			r.members.put(r.url("Packages"), pkgs)
			r.check()
			if r.row().CurrentSnapshotID == nil {
				t.Fatalf("premise: the signed first adoption failed (%q)", r.lastResult())
			}
			rp, err := NewReaper(ReaperConfig{Cache: r.cache, Checker: r.checker, IdleTTL: 30 * 24 * time.Hour,
				Logger: slog.New(slog.NewJSONHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
			if err != nil {
				t.Fatal(err)
			}
			r.clock.advance(reapAge)
			rp.lastMono = time.Time{}
			rp.runOnce(ctx)
			if _, err := r.cache.GetSuiteFreshness(ctx, "http", flatRigHost, "/repo"); !errors.Is(err, cache.ErrNotFound) {
				t.Fatalf("premise: /repo was not reaped (%v)", err)
			}
			if _, ok, err := r.cache.SignedSeen(ctx, "http", flatRigHost, "/repo"); err != nil || !ok {
				t.Fatalf("premise: the reap took the marker (%v, %v)", ok, err)
			}
			r.up.mu.Lock()
			delete(r.up.bodies, r.url("InRelease")) // the vendor stops signing
			r.up.mu.Unlock()
			rel, _ := makeRelease(map[string][]byte{"Packages": pkgs})
			r.up.setAt(r.url("Release"), rel)
			r.adopter.adoptUnsigned = true
			comeBack := func() {
				t.Helper()
				if res, err := r.cache.CreateSuite(ctx, cache.SuiteSeed{SuiteKey: suiteKeyOf(r.suite),
					RequestedAt: r.clock.now().Unix()}, cache.SuiteCaps{}, nil); err != nil || res.Outcome != cache.SuiteCreated {
					t.Fatalf("re-creation = %+v, %v", res, err)
				}
				r.cacheRow("Release", rel, 1, r.clock.now().Unix(), "")
			}
			comeBack()
			r.check()
			if row := r.row(); row.CurrentSnapshotID != nil || strOf(row.AdoptionFailReason) != cache.ReasonUnsignedDowngradeRefused {
				t.Fatalf("back unsigned: adopted %v, record %q; want unsigned_downgrade_refused", row.CurrentSnapshotID != nil,
					strOf(row.AdoptionFailReason))
			}
			if twin {
				if err := r.cache.EnsureSuiteForTest(ctx, "http", flatRigHost, "/repo/."); err != nil {
					t.Fatal(err)
				}
			}
			if res, err := rp.Forget(ctx, "http", flatRigHost, "/repo"); err != nil || !res.Deleted {
				t.Fatalf("forget = %+v, %v", res, err)
			}
			if _, marked, err := r.cache.SignedSeen(ctx, "http", flatRigHost, "/repo"); err != nil || marked != twin {
				t.Fatalf("after /forget: marker %v (%v); want %v", marked, err, twin)
			}
			comeBack()
			r.clock.advance(time.Minute)
			r.check()
			r.clock.advance(16 * time.Minute)
			r.check()
			row := r.row()
			switch {
			case twin && (row.CurrentSnapshotID != nil || strOf(row.AdoptionFailReason) != cache.ReasonUnsignedDowngradeRefused):
				t.Fatalf("twin kept: adopted %v, record %q; want still refused", row.CurrentSnapshotID != nil, strOf(row.AdoptionFailReason))
			case !twin && row.CurrentSnapshotID == nil:
				t.Fatalf("no twin: record %q (%q); want the unsigned Release adopted", strOf(row.AdoptionFailReason), r.lastResult())
			case !twin:
				if snap, err := r.cache.GetSuiteSnapshot(ctx, *row.CurrentSnapshotID); err != nil || snap.Form != cache.FormUnsigned {
					t.Fatalf("no twin: snapshot %+v, %v; want the unsigned form", snap, err)
				}
			}
		})
	}
}

// Ruling R-G5 (deferred minor F-12): the reaper reads flat index rows
// only when a run holds an unadopted flat suite with no cached anchor row
// — the suites ruling R-G2 judges by their indexes.
func TestHasAnchorlessFlatSuite(t *testing.T) {
	id := int64(7)
	row := func(suite string, current *int64) cache.SuiteWithAdoption {
		return cache.SuiteWithAdoption{SuiteFreshness: cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h",
			SuitePath: suite, CurrentSnapshotID: current}}
	}
	anchors := map[cache.SuiteKey][]cache.SuiteAnchorRow{reapKey("/anchored"): {{Path: "/anchored/InRelease"}}}
	for _, tc := range []struct {
		name string
		rows []cache.SuiteWithAdoption
		want bool
	}{
		{"an unadopted flat suite without anchors", []cache.SuiteWithAdoption{row("/r/dists/x", nil), row("/flat", nil)}, true},
		{"an adopted flat suite", []cache.SuiteWithAdoption{row("/flat", &id)}, false},
		{"a flat suite with an anchor row", []cache.SuiteWithAdoption{row("/anchored", nil)}, false},
		{"dists suites only", []cache.SuiteWithAdoption{row("/r/dists/x", nil), row("/r/dists/y", nil)}, false},
		{"none", nil, false},
	} {
		if got := hasAnchorlessFlatSuite(tc.rows, anchors); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}
```

In `internal/freshness/reaper_test.go`, replace

```go
// now — never idle — and is reported. A reaped suite's snapshots are
// retired, its metadata url_path rows deleted, and its lock-map, flip-gate
// and touch entries purged (ruling R-F1); each reap logs suite_reaped and
// counts in acu_suites_reaped_total by reason (design review M-16).
func TestReaper_ReapsIdleAndNonCanonicalSuitesAndPurgesThem(t *testing.T) {
```

with

```go
// now — never idle — and is reported. A reaped suite's snapshots are
// retired, its metadata url_path rows deleted, and its lock-map, flip-gate
// and touch entries purged (ruling R-F1); each reap logs suite_reaped and
// counts in acu_suites_reaped_total by reason (design review M-16). Flat
// suites (phase G, the F carry-forward) are canonical and reaped by
// idleness like any: /flat, used a day ago, stays; the idle /repo goes
// and its ./ twin, used, stays with its own rows (ruling R-G4).
func TestReaper_ReapsIdleAndNonCanonicalSuitesAndPurgesThem(t *testing.T) {
```

In `internal/freshness/reaper_test.go`, replace

```go
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
```

with

```go
		reaped := map[string]int64{
			"/r/dists/idle":        reapNow - 31*reapDay,
			"/legacy//dists/x":     reapNow, // non-canonical: an empty segment
			"/foo/dists/InRelease": reapNow - 31*reapDay,
			"/repo":                reapNow - 31*reapDay,
		}
		kept := map[string]int64{
			"/r/dists/fresh":       reapNow - 29*reapDay,
			"/r/dists/future":      reapNow + 2*reapDay,
			"/bar/dists/InRelease": reapNow - reapDay,
			"/flat":                reapNow - reapDay,
			"/repo/.":              reapNow - 29*reapDay,
		}
```

In `internal/freshness/reaper_test.go`, replace

```go
		for s := range kept {
			if !r.exists(s) {
				t.Errorf("%s was reaped", s)
			}
		}
```

with

```go
		for s := range kept {
			if !r.exists(s) {
				t.Errorf("%s was reaped", s)
			}
			if _, err := r.cache.LookupURL(context.Background(), "http", "h", s+"/InRelease"); err != nil {
				t.Errorf("%s/InRelease: %v; want it kept with its suite", s, err)
			}
		}
```

In `internal/freshness/reaper_test.go`, replace

```go
		want := map[string]string{"/r/dists/idle": "idle", "/legacy//dists/x": "non_canonical", "/foo/dists/InRelease": "idle"}
```

with

```go
		want := map[string]string{"/r/dists/idle": "idle", "/legacy//dists/x": "non_canonical", "/foo/dists/InRelease": "idle",
			"/repo": "idle"}
```

In `internal/freshness/reaper_test.go`, replace

```go
		if di, dn := reapedTotal(t, "idle")-idle0, reapedTotal(t, "non_canonical")-nonCanonical0; di != 2 || dn != 1 {
			t.Fatalf("acu_suites_reaped_total rose by idle %v, non_canonical %v; want 2 and 1", di, dn)
		}
```

with

```go
		if di, dn := reapedTotal(t, "idle")-idle0, reapedTotal(t, "non_canonical")-nonCanonical0; di != 3 || dn != 1 {
			t.Fatalf("acu_suites_reaped_total rose by idle %v, non_canonical %v; want 3 and 1", di, dn)
		}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -gcflags=-e ./internal/cache/`
Expected: FAIL — `c.ListUnadoptedFlatIndexRows undefined (type *Cache has no field or method ListUnadoptedFlatIndexRows)`, `undefined: unadoptedFlatIndexRowsSQL`.

Run: `go test -gcflags=-e ./internal/freshness/`
Expected: FAIL — `undefined: hasAnchorlessFlatSuite`. (With only that function in place, `TestReaper_ReapsAJunkAnchorlessFlatSuite` fails `/junk was not reaped as junk`, `/absent was not reaped as junk`, `/gz-name was not reaped as junk`, `acu_suites_reaped_total{reason="junk"} rose by 1, want 4`: only `/junk-anchors`, judged by its anchors, goes. `TestReaper_ReapsIdleAndNonCanonicalSuitesAndPurgesThem`'s flat rows and `TestDoD_ForgetIsTheEscapeHatchForAFlatSuitesDowngradeRefusal` pass already: Task 1's grammar made flat suites canonical — before it `/flat` and `/repo/.` were reaped as `non_canonical` — and phase F's forget and reap leave a twin alone; `TestReap_AFlatSuiteLeavesItsTwin` passes once the cache package compiles.)

- [ ] **Step 3: Implement**

Create `internal/cache/flat_reap.go`:

```go
package cache

import (
	"context"
	"fmt"
	"strings"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
)

// unadoptedFlatIndexRowsSQL is ListUnadoptedFlatIndexRows' query: the
// shape of unadoptedSuiteAnchorsSQL (design review M-14), probing
// url_path's key once per root index name of spec C2's flat member
// grammar — Packages and Sources, plain or with one of the six
// compression extensions — for every unadopted suite whose path has no
// "/dists/" (instr, not LIKE: LIKE ignores ASCII case).
//
// AIDEV-NOTE: keep the join's url_path terms bare columns, as in
// unadoptedSuiteAnchorsSQL: an expression on them turns each probe into a
// scan of the host's rows (TestListUnadoptedFlatIndexRows_ProbesURLPathByItsKey
// pins the plan).
var unadoptedFlatIndexRowsSQL = func() string {
	var names []string
	for _, stem := range []string{"Packages", "Sources"} {
		for _, ext := range []string{"", ".gz", ".xz", ".bz2", ".lzma", ".lz4", ".zst"} {
			names = append(names, "sf.suite_path || '/"+stem+ext+"'")
		}
	}
	return `
SELECT sf.canonical_scheme, sf.canonical_host, sf.suite_path, u.path, u.blob_hash, u.last_fetched_at
  FROM suite_freshness sf
  JOIN url_path u
    ON u.canonical_scheme = sf.canonical_scheme
   AND u.canonical_host   = sf.canonical_host
   AND u.path IN (` + strings.Join(names, ", ") + `)
 WHERE sf.current_snapshot_id IS NULL
   AND instr(sf.suite_path, '/dists/') = 0
 ORDER BY sf.canonical_scheme, sf.canonical_host, sf.suite_path, u.path`
}()

// ListUnadoptedFlatIndexRows returns the cached root Packages* and
// Sources* rows of every flat suite (aptpath.IsFlatSuitePath) that has no
// current snapshot, in suite and path order: ruling R-G2's junk evidence
// for an anchorless flat suite. One read on the pool per reaper run.
func (c *Cache) ListUnadoptedFlatIndexRows(ctx context.Context) ([]SuiteAnchorRow, error) {
	rows, err := c.db.QueryContext(ctx, unadoptedFlatIndexRowsSQL)
	if err != nil {
		return nil, fmt.Errorf("ListUnadoptedFlatIndexRows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SuiteAnchorRow
	for rows.Next() {
		var a SuiteAnchorRow
		if err := rows.Scan(&a.CanonicalScheme, &a.CanonicalHost, &a.SuitePath, &a.Path, &a.BlobHash, &a.LastFetchedAt); err != nil {
			return nil, fmt.Errorf("ListUnadoptedFlatIndexRows: scan: %w", err)
		}
		if aptpath.IsFlatSuitePath(a.SuitePath) {
			out = append(out, a)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListUnadoptedFlatIndexRows: %w", err)
	}
	return out, nil
}
```

In `internal/freshness/reaper.go`, replace

```go
	"slices"
	"time"
```

with

```go
	"slices"
	"strings"
	"time"
```

In `internal/freshness/reaper.go`, replace

```go
// sniffKey keys a junk verdict: what a blob's head is to one kind of
// anchor never changes, since the pool is content-addressed.
type sniffKey struct {
	file aptpath.SuiteFile
	hash string
}
```

with

```go
// sniffKey keys a junk verdict: what a blob's head is to one kind of
// anchor never changes, since the pool is content-addressed. A flat
// index's verdict also depends on its name — its extension picks the
// flat index sniff (aptpath.SniffFlatIndex) — so name is the member's
// name for aptpath.FileFlatIndex and "" for an anchor (ruling R-G2).
type sniffKey struct {
	file aptpath.SuiteFile
	hash string
	name string
}
```

In `internal/freshness/reaper.go`, replace

```go
and allJunk stops at a suite's first row that is no
// evidence, so an older verdict that scan did not reach is gone — and an
// anchor no sniff applies to (aptpath.NotSuiteFile: a suite path over
// MaxSuitePathBytes) is never evidence and never read.
```

with

```go
and allJunk stops at a suite's first row that is no
// evidence, so an older verdict that scan did not reach is gone — and an
// anchor no sniff applies to (aptpath.NotSuiteFile: a suite path over
// MaxSuitePathBytes) is never evidence and never read. Ruling R-G2: a flat
// suite with no cached anchor row — an anchorless one — is judged by its
// cached root Packages*/Sources* rows instead
// (cache.ListUnadoptedFlatIndexRows, one more indexed read per run), by
// the same rules (aptpath.SniffFlatIndex for the name; fetched at least
// junkGrace ago; whatever its record); a suite with any anchor row is
// judged by its anchors alone. A failed read of the index rows is logged
// and leaves anchorless suites unjudged for the run.
```

In `internal/freshness/reaper.go`, replace

```go
	bySuite := map[cache.SuiteKey][]cache.SuiteAnchorRow{}
	for _, a := range anchors {
		bySuite[a.SuiteKey] = append(bySuite[a.SuiteKey], a)
	}
```

with

```go
	bySuite := map[cache.SuiteKey][]cache.SuiteAnchorRow{}
	for _, a := range anchors {
		bySuite[a.SuiteKey] = append(bySuite[a.SuiteKey], a)
	}
	// Ruling R-G2's evidence for anchorless flat suites, read only when
	// there is one to judge.
	//
	// AIDEV-NOTE: ruling R-G5 (deferred minor F-12): a run with no
	// unadopted flat suite lacking anchor rows reads no index rows, so a
	// cache of dists/ suites pays nothing for R-G2 — the 90-day reaping
	// simulation (TestDoD_NinetyDaysOfReapingWithRestarts) keeps its
	// 4.5 s budget under -race (an unconditional read cost it about 20 %).
	byIndexes := map[cache.SuiteKey][]cache.SuiteAnchorRow{}
	if hasAnchorlessFlatSuite(rows, bySuite) {
		indexes, err := r.cache.ListUnadoptedFlatIndexRows(ctx)
		if err != nil {
			r.warn(ctx, "suite reaper: list unadopted flat indexes failed", "err", err)
		}
		for _, a := range indexes {
			byIndexes[a.SuiteKey] = append(byIndexes[a.SuiteKey], a)
		}
	}
```

In `internal/freshness/reaper.go`, replace

```go
		as := bySuite[key]
		if listed[key] || len(as) == 0 || !r.allJunk(key, as, cutoff, next, &fails) {
```

with

```go
		as := bySuite[key]
		if len(as) == 0 {
			as = byIndexes[key] // ruling R-G2: an anchorless flat suite's evidence
		}
		if listed[key] || len(as) == 0 || !r.allJunk(key, as, cutoff, next, &fails) {
```

In `internal/freshness/reaper.go`, replace

```go
// junkReadFailures counts one junk scan's failed pool reads, keeping the
```

with

```go
// hasAnchorlessFlatSuite reports whether rows hold an unadopted flat
// suite with no cached anchor row in bySuite: a suite ruling R-G2 judges
// by its root index rows.
func hasAnchorlessFlatSuite(rows []cache.SuiteWithAdoption, bySuite map[cache.SuiteKey][]cache.SuiteAnchorRow) bool {
	for i := range rows {
		s := &rows[i].SuiteFreshness
		key := cache.SuiteKey{CanonicalScheme: s.CanonicalScheme, CanonicalHost: s.CanonicalHost, SuitePath: s.SuitePath}
		if s.CurrentSnapshotID == nil && len(bySuite[key]) == 0 && aptpath.IsFlatSuitePath(s.SuitePath) {
			return true
		}
	}
	return false
}

// junkReadFailures counts one junk scan's failed pool reads, keeping the
```

In `internal/freshness/reaper.go`, replace

```go
// allJunk reports whether every one of key's cached anchor rows holds a
// blob fetched at or before cutoff that is junk evidence (anchorEvidence).
```

with

```go
// allJunk reports whether every one of key's evidence rows — its cached
// anchor rows, or an anchorless flat suite's root index rows (ruling
// R-G2) — holds a blob fetched at or before cutoff that is junk evidence
// (anchorEvidence).
```

In `internal/freshness/reaper.go`, replace

```go
		k := sniffKey{file: file, hash: *a.BlobHash}
		junk, ok := next[k]
```

with

```go
		k := sniffKey{file: file, hash: *a.BlobHash}
		if file == aptpath.FileFlatIndex {
			k.name = a.Path[strings.LastIndexByte(a.Path, '/')+1:] // its extension picks the sniff
		}
		junk, ok := next[k]
```

In `internal/freshness/reaper.go`, replace

```go
// anchorEvidence reports whether head, the start of a cached anchor of
// kind file at path, is junk evidence (ruling PF-3): it fails its own
// sniff and, for an InRelease, the Release sniff too — a plain-text
// Release cached as InRelease is a §6.1-quarantined plain-text suite on
// its way to an unsigned adoption through Release, not junk.
```

with

```go
// anchorEvidence reports whether head, the start of a cached anchor (or,
// ruling R-G2, flat index) of kind file at path, is junk evidence (ruling
// PF-3): it fails its own sniff and, for an InRelease, the Release sniff
// too — a plain-text Release cached as InRelease is a §6.1-quarantined
// plain-text suite on its way to an unsigned adoption through Release,
// not junk.
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/cache/ ./internal/freshness/`
Expected: PASS.

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| no flat index evidence (`as = byIndexes[key]` dropped) | `TestReaper_ReapsAJunkAnchorlessFlatSuite` (`/junk was not reaped as junk`) |
| flat index evidence beside the anchors (`as = append(as, byIndexes[key]...)` for every suite) | `TestReaper_ReapsAJunkAnchorlessFlatSuite` (`/junk-anchors was not reaped as junk`) |
| a flat index verdict keyed without its name | `TestReaper_ReapsAJunkAnchorlessFlatSuite` (`/plain-name was reaped`) |
| no `IsFlatSuitePath` filter in `ListUnadoptedFlatIndexRows` | `TestListUnadoptedFlatIndexRows_ListsTheRootIndexesOfUnadoptedFlatSuites` (`"/a//b/Packages"` listed) |
| `NOT LIKE '%/dists/%'` for the pre-filter | `TestListUnadoptedFlatIndexRows_ListsTheRootIndexesOfUnadoptedFlatSuites` (`/x/DISTS/y/Packages` missing) |
| five compression extensions (no `.zst`) | `TestListUnadoptedFlatIndexRows_ListsTheRootIndexesOfUnadoptedFlatSuites` (`/flat/Sources.zst` missing) |
| adopted suites listed too | `TestListUnadoptedFlatIndexRows_ListsTheRootIndexesOfUnadoptedFlatSuites` (`/adopted/Packages` listed) |
| an expression on the probed column (`u.path \|\| ''`) | `TestListUnadoptedFlatIndexRows_ProbesURLPathByItsKey` (`SEARCH u USING INDEX … (canonical_scheme=? AND canonical_host=?)`) |
| the index read gated on suites with anchors (`len(bySuite[key]) != 0`) | `TestHasAnchorlessFlatSuite` (`an unadopted flat suite without anchors: false, want true`) |
| the index read for adopted suites too | `TestHasAnchorlessFlatSuite` (`an adopted flat suite: true, want false`) |
| the index read for `dists/` suites too | `TestHasAnchorlessFlatSuite` (`dists suites only: true, want false`) |

Equivalent mutants (recorded): dropping the SQL `/dists/` pre-filter only costs 14 key probes per unadopted `dists/` suite (the `IsFlatSuitePath` filter drops their rows); reading the index rows every run (`|| true` on the gate) changes no verdict — its cost is the budget measured above. Measure the budget after this task: `go test -race -count=3 ./internal/freshness/ -run 'TestDoD_NinetyDaysOfReapingWithRestarts' -v` should report times within 0.1 s of the base commit's.

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/cache/flat_reap.go internal/cache/flat_reap_test.go internal/freshness/reaper.go internal/freshness/reaper_test.go internal/freshness/flat_reap_test.go
git add internal/cache/flat_reap.go internal/cache/flat_reap_test.go internal/freshness/reaper.go internal/freshness/reaper_test.go internal/freshness/flat_reap_test.go
git commit -m "Judge anchorless flat suites junk by their indexes; reap and forget flat suites beside their twins (rulings R-G2, R-G4)"
```

(Record the mutation table in the commit message.)

### Task 13: Twins in never-downgrade (the E carry-forward, E-16, E-17, E-19)

Ruling R-G4's never-downgrade share: with G's grammar a `./` twin is a real suite, so the twin cases phase E could not write become tests. The E carry-forward runs `TestCheck_UnsignedRefusedInsideTheCommitIsAState` a second time with the signed anchor and its marker arriving through the `./` twin of the flat suite `/repo` while `/repo`'s unsigned adoption is paused before its commit (the commit's in-transaction recheck refuses it as a state; the body moves into `unsignedRefusedInsideTheCommit`, unchanged but for the suite, the served path and a flat Release declaring its root `Packages`). Deferred minors taken: E-16 (the re-mark's twin direction: suite `/flat/.`, evidence cached at `/flat/InRelease`), E-17 (the re-mark fault's line must carry a non-empty `err`), E-19 (the probe's marker goes to the base, `aptpath.MarkerBase`, never the twin's own path). No production code changes: phases D and E built the rules; Task 1's grammar makes the twin reachable.

**Files:**
- Create: `internal/freshness/flat_twin_test.go`
- Modify: `internal/freshness/unsigned_test.go` (`TestCheck_UnsignedRefusedInsideTheCommitIsAState` and its helper, `TestRunUnsigned_RemarkMarksTheTwinsBase`, `TestRunUnsigned_FailedRemarkRefuses`, `TestRunUnsigned_RemarkReadErrorRefuses`, `nonEmptyString`)

**Interfaces:**
- Consumes: the unsigned rig (`newUnsignedRig`, `cacheAnchorAt`, `records`, `marked`, `current`), `trackSuite`, `hashRelease`, `observability.NewRing`, the flat rig (Task 9).
- Produces: `unsignedRefusedInsideTheCommit`, `nonEmptyString` (tests).

- [ ] **Step 1: Write the tests**

In `internal/freshness/unsigned_test.go`, replace

```go
// barrier event says refused, and nothing is logged at WARN or above.
func TestCheck_UnsignedRefusedInsideTheCommitIsAState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		r := newUnsignedRig(t, 10*time.Second, 20*time.Second)
		ring := observability.NewRing(10)
```

with

```go
// barrier event says refused, and nothing is logged at WARN or above.
// The E carry-forward (phase G): the same when the signed anchor and its
// marker arrive through the ./ twin of the flat suite /repo, whose base
// the twin shares.
func TestCheck_UnsignedRefusedInsideTheCommitIsAState(t *testing.T) {
	t.Run("the suite's own anchor", func(t *testing.T) { unsignedRefusedInsideTheCommit(t, "", "") })
	t.Run("the ./ twin's anchor", func(t *testing.T) { unsignedRefusedInsideTheCommit(t, "/repo", "/repo/.") })
}

// unsignedRefusedInsideTheCommit runs TestCheck_UnsignedRefusedInsideTheCommitIsAState
// for the flat suite suite (the rig's dists/ suite when "") with the
// signed anchor served under served (the suite itself when "").
func unsignedRefusedInsideTheCommit(t *testing.T, suite, served string) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		r := newUnsignedRig(t, 10*time.Second, 20*time.Second)
		rel := r.release("unsigned")
		if suite != "" {
			r.suite.SuitePath = suite
			trackSuite(t, r.c, r.suite)
			r.members.put("http://h"+suite+"/Packages", r.pkgs)
			rel = hashRelease("Origin: unsigned\n", true, false, map[string][]byte{"Packages": r.pkgs}, nil)
		}
		if served == "" {
			served = r.suite.SuitePath
		} else {
			trackSuite(t, r.c, SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: served})
		}
		ring := observability.NewRing(10)
```

In `internal/freshness/unsigned_test.go`, replace

```go
				now := time.Now().Unix()
				path := r.suite.SuitePath + "/InRelease"
				if _, err := r.c.StoreFetched(ctx, cache.FetchedRow{Row: cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h",
					Path: path, BlobHash: &h, UpstreamURL: "http://h" + path, IsMetadata: true, LastRequestedAt: &now,
					RequestCount: 1, LastFetchedAt: &now}, Suite: cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h",
					SuitePath: r.suite.SuitePath}, MarkSeenAt: now}, cache.SuiteCaps{}, nil); err != nil {
```

with

```go
				now := time.Now().Unix()
				path := served + "/InRelease"
				if _, err := r.c.StoreFetched(ctx, cache.FetchedRow{Row: cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h",
					Path: path, BlobHash: &h, UpstreamURL: "http://h" + path, IsMetadata: true, LastRequestedAt: &now,
					RequestCount: 1, LastFetchedAt: &now}, Suite: cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h",
					SuitePath: served}, MarkSeenAt: now}, cache.SuiteCaps{}, nil); err != nil {
```

In `internal/freshness/unsigned_test.go`, replace

```go
		rel := r.release("unsigned")
		req := &adoptionRequest{suite: r.suite, form: adoptionFormUnsigned, releaseBytes: rel,
```

with

```go
		req := &adoptionRequest{suite: r.suite, form: adoptionFormUnsigned, releaseBytes: rel,
```

In `internal/freshness/unsigned_test.go`, replace

```go
			if recs := r.records("adoption: signed-anchor re-mark failed"); len(recs) != 1 || recs[0]["level"] != level || recs[0]["err"] == "" {
				t.Fatalf("re-mark fault records %v, want one at %s with the error", recs, level)
			}
```

with

```go
			if recs := r.records("adoption: signed-anchor re-mark failed"); len(recs) != 1 || recs[0]["level"] != level || !nonEmptyString(recs[0]["err"]) {
				t.Fatalf("re-mark fault records %v, want one at %s with the error", recs, level) // deferred minor E-17
			}
```

In `internal/freshness/unsigned_test.go`, replace

```go
	if recs := r.records("adoption: signed-anchor re-mark failed"); len(recs) != 1 || recs[0]["level"] != "WARN" {
		t.Fatalf("re-mark fault records %v, want one at WARN", recs)
	}
}
```

with

```go
	if recs := r.records("adoption: signed-anchor re-mark failed"); len(recs) != 1 || recs[0]["level"] != "WARN" || !nonEmptyString(recs[0]["err"]) {
		t.Fatalf("re-mark fault records %v, want one at WARN with the error", recs) // deferred minor E-17
	}
}

// nonEmptyString reports whether v, a decoded JSON log field, is a
// non-empty string (deferred minor E-17: an absent key is no error text).
func nonEmptyString(v any) bool {
	s, ok := v.(string)
	return ok && s != ""
}
```

In `internal/freshness/unsigned_test.go`, replace

```go
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
```

with

```go
// Ruling R-E1 for the /. twin: the re-mark marks the suite's base
// (aptpath.MarkerBase), which the commit's in-transaction recheck reads —
// not the twin's own path. (Proposed by the executability review; kills
// X10.) Deferred minor E-16 (phase G): it reads the base's own anchors
// too — the twin /flat/. finds the evidence cached at /flat/InRelease.
// The flat suite's Release declares its root Packages (spec C5).
func TestRunUnsigned_RemarkMarksTheTwinsBase(t *testing.T) {
	for _, evidence := range []string{"/flat/.", "/flat"} {
		t.Run("evidence at "+evidence+"/InRelease", func(t *testing.T) {
			r := newUnsignedRig(t, 0, 0)
			r.suite.SuitePath = "/flat/."
			trackSuite(t, r.c, r.suite)
			r.members.put("http://h/flat/./Packages", r.pkgs)
			r.cacheAnchorAt(evidence, "InRelease", simtest.ClearsignedFixture("Origin: signed\n"))
			rel := hashRelease("Origin: unsigned\n", true, false, map[string][]byte{"Packages": r.pkgs}, nil)
			err := r.ad.RunUnsigned(context.Background(), r.suite, rel, "", "")
			if !errors.Is(err, cache.ErrUnsignedDowngradeRefused) || r.current() != 0 {
				t.Fatalf("RunUnsigned = %v, current %d: want the commit's refusal and no snapshot", err, r.current())
			}
			if _, ok, err := r.c.SignedSeen(context.Background(), "http", "h", "/flat"); err != nil || !ok {
				t.Fatalf("no marker for the base /flat (ok %v, err %v)", ok, err)
			}
		})
	}
}
```

Create `internal/freshness/flat_twin_test.go`:

```go
package freshness

import (
	"context"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// Deferred minor E-19 (spec C3, ruling PE-5): a probe that finds a
// signature marks the suite's base (aptpath.MarkerBase), which a /. twin
// shares — the probe of /repo/. marks /repo, never /repo/. itself. The
// adoption that follows fails (no Packages upstream), so the probe's
// marker is the only one written.
func TestCheck_ATwinsProbeMarksItsBase(t *testing.T) {
	r := newFlatRig(t, "/repo/.")
	in := simtest.ClearsignedFixture("Origin: repo\n")
	r.cacheRow("InRelease", in, 1, r.clock.now().Unix(), "")
	r.up.setAt(r.url("InRelease"), in)
	r.check()
	ctx := context.Background()
	if _, ok, err := r.cache.SignedSeen(ctx, "http", flatRigHost, "/repo"); err != nil || !ok {
		t.Fatalf("the base /repo has no marker (%v, %v)", ok, err)
	}
	if _, ok, err := r.cache.SignedSeen(ctx, "http", flatRigHost, "/repo/."); err != nil || ok {
		t.Fatalf("a marker for /repo/. itself (%v, %v); want the base's only", ok, err)
	}
	if row := r.row(); row.CurrentSnapshotID != nil {
		t.Fatal("premise: the adoption committed (and could have written a marker of its own)")
	}
}
```

- [ ] **Step 2: Run them**

Run: `go test ./internal/freshness/ -run 'TestCheck_UnsignedRefusedInsideTheCommitIsAState|TestRunUnsigned_RemarkMarksTheTwinsBase|TestRunUnsigned_FailedRemarkRefuses|TestRunUnsigned_RemarkReadErrorRefuses|TestCheck_ATwinsProbeMarksItsBase' -v`
Expected: PASS (`TestCheck_UnsignedRefusedInsideTheCommitIsAState/the_./_twin's_anchor`, `TestRunUnsigned_RemarkMarksTheTwinsBase/evidence_at_/flat/InRelease` among them). They pin rules phases D and E built; the mutations below are their red runs.

- [ ] **Step 3: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| E-19: `markSignatureSeen` marks `suite.SuitePath` | `TestCheck_ATwinsProbeMarksItsBase` (`the base /repo has no marker`) |
| E-16: the re-mark reads `suite.SuitePath` and its `/.` only | `TestRunUnsigned_RemarkMarksTheTwinsBase/evidence_at_/flat/InRelease` (`RunUnsigned = <nil>, current 1`) |
| E-17: the re-mark fault's line without `"err", err` | `TestRunUnsigned_FailedRemarkRefuses/unreadable_pool_file`, `/failed_mark_write`, `TestRunUnsigned_RemarkReadErrorRefuses` (`want one at … with the error`) |
| the carry-forward: `StoreFetched` marks the served suite's own path, not its base | `TestCheck_UnsignedRefusedInsideTheCommitIsAState/the_./_twin's_anchor` (`marked false, current 1`) |

- [ ] **Step 4: Commit**

```bash
gofmt -l internal/freshness/unsigned_test.go internal/freshness/flat_twin_test.go
git add internal/freshness/unsigned_test.go internal/freshness/flat_twin_test.go
git commit -m "Pin never-downgrade through a flat suite's ./ twin (E carry-forward, deferred minors E-16, E-17, E-19)"
```

(Record the mutation table in the commit message.)

### Task 14: `suites[].flat` on the admin status page (the B carry-forward; B-59, B-62)

Design note 16 and the B carry-forward (spec C7, C11; phase B plan, deviation 3): every `suites[]` row gains `flat` — `aptpath.IsFlatSuitePath(suite_path)`, true for a flat suite and its `./` twin — additive to the locked JSON contract as `scheme` was (ruling PF-7), placed after `suite_path`; the HTML Suites table tags a flat suite's path with a neutral `flat` badge (the page's existing `.b` class). The JSON golden is regenerated; `docs/admin-ui-spec.md` §4.5 gains the field in Task 16. Deferred minors taken: B-59 (a lag under an hour shows without warning; the JSON golden pins the record's fields with values, not only zeros, and pins a flat row) and B-62 (`htmlRowStart` returns `""` for an offset `strings.Index` did not find instead of panicking). Checked and left: B-60 (`status_test.go`, which this task does not change) and B-63 (the Adoption cell's "checks failing ×k · next check in …": kept deferred by ruling — final review T M9 — and its "next check in" needs the scheduler's backoff, `freshness.backoffDelay`, which `internal/admin` reaches only through injected interfaces; H's operability sweep, flagged).

**Files:**
- Modify: `internal/admin/status.go` (`suiteEntry.Flat`, `buildSuiteEntries`, the `aptpath` import), `internal/admin/status_template.go` (the Suites table's Suite path cell), `internal/admin/suites_status_test.go`, `internal/admin/json_contract_test.go` (the populated fixture), `internal/admin/testdata/json_contract_golden.json` (regenerated)

**Interfaces:**
- Consumes: `aptpath.IsFlatSuitePath` (Task 1); admin tests' `newHealthyModel`, `renderHTMLForGolden`, `mustContain`, `suiteRows`, `suiteRow`, `p64`.
- Produces: the JSON field `suites[].flat` (boolean) and the HTML `flat` tag — Task 16 documents them.

- [ ] **Step 1: Write the failing tests**

In `internal/admin/json_contract_test.go`, replace

```go
		Suites: []suiteEntry{
			{
				Scheme:                           "http",
				Host:                             "archive.ubuntu.com",
				SuitePath:                        "dists/noble",
				LastCheckUnixTime:                &suiteCheck,
				LastSuccessUnixTime:              &suiteSuccess,
				CurrentSnapshotID:                &snapID,
				CurrentSnapshotAdoptedAtUnixTime: &snapAdopted,
				InReleaseChangeSeenAtUnixTime:    &releaseSeen,
				Lagging:                          "(lagging 30m)",
			},
		},
```

with

```go
		Suites: []suiteEntry{
			{
				Scheme:                           "http",
				Host:                             "archive.ubuntu.com",
				SuitePath:                        "dists/noble",
				LastCheckUnixTime:                &suiteCheck,
				LastSuccessUnixTime:              &suiteSuccess,
				CurrentSnapshotID:                &snapID,
				CurrentSnapshotAdoptedAtUnixTime: &snapAdopted,
				InReleaseChangeSeenAtUnixTime:    &releaseSeen,
				Lagging:                          "(lagging 30m)",
				// Deferred minor B-59: the record's fields pinned with
				// values, not only zeros.
				AnchorForm:                "inline",
				LastRequestedUnixTime:     &suiteCheck,
				CheckFailCount:            2,
				AdoptionFailReason:        &failReason,
				AdoptionFailCount:         3,
				AdoptionFailFirstUnixTime: &suiteSuccess,
				AdoptionNextRetryUnixTime: &releaseSeen,
			},
			// Spec C7/C11: suites[].flat (phase G), a flat suite's row.
			{
				Scheme:            "https",
				Host:              "pkgs.k8s.io",
				SuitePath:         "/core:/stable:/v1.34/deb",
				Flat:              true,
				CurrentSnapshotID: &flatSnapID,
				AnchorForm:        "inline",
			},
		},
```

In `internal/admin/json_contract_test.go`, replace

```go
	releaseSeen := int64(1_700_005_500)
```

with

```go
	releaseSeen := int64(1_700_005_500)
	failReason, flatSnapID := "untrusted_signer", int64(43)
```

In `internal/admin/suites_status_test.go`, replace

```go
// htmlRowStart returns the substring starting at the <tr preceding the
// given offset, up to the next >, for row-scoped assertions.
func htmlRowStart(html string, offset int) string {
	trStart := strings.LastIndex(html[:offset], "<tr")
```

with

```go
// htmlRowStart returns the substring starting at the <tr preceding the
// given offset, up to the next >, for row-scoped assertions; "" for an
// offset outside html (deferred minor B-62: a strings.Index miss, -1,
// fails the caller's assertion instead of panicking).
func htmlRowStart(html string, offset int) string {
	if offset < 0 || offset > len(html) {
		return ""
	}
	trStart := strings.LastIndex(html[:offset], "<tr")
```

In `internal/admin/suites_status_test.go`, replace

```go
		{SuiteFreshness: cache.SuiteFreshness{CanonicalHost: "lag.example", SuitePath: "/d",
			InReleaseChangeSeenAt: p64(now - 2*86400)}},
	}
}
```

with

```go
		{SuiteFreshness: cache.SuiteFreshness{CanonicalHost: "lag.example", SuitePath: "/d",
			InReleaseChangeSeenAt: p64(now - 2*86400)}},
		// Deferred minor B-59: a lag under an hour shows, but never warns.
		{SuiteFreshness: cache.SuiteFreshness{CanonicalHost: "young.example", SuitePath: "/e",
			InReleaseChangeSeenAt: p64(now - 30*60)}},
	}
}
```

In `internal/admin/suites_status_test.go`, replace

```go
		// ADOPTIONS vital sub-line: the refusal is counted apart from failures.
		`<b class="accent-crit">1 FAILED</b> &middot; <b class="accent-warn">1 REFUSED</b>`,
	)
	for host, warn := range map[string]bool{"fail.example": true, "refused.example": false, "stuck.example": true, "lag.example": true} {
```

with

```go
		// ADOPTIONS vital sub-line: the refusal is counted apart from failures.
		`<b class="accent-crit">1 FAILED</b> &middot; <b class="accent-warn">1 REFUSED</b>`,
		`<span class="lagging">(lagging 30m)</span>`,
	)
	for host, warn := range map[string]bool{"fail.example": true, "refused.example": false, "stuck.example": true, "lag.example": true,
		"young.example": false} {
```

Append to `internal/admin/suites_status_test.go`:

```go
// Spec C7, C11 (the B carry-forward): suites[].flat is
// aptpath.IsFlatSuitePath of the row's suite path — a flat suite and its
// ./ twin are flat; a dists/ suite and a path no grammar produces are not.
func TestBuildSuiteEntries_Flat(t *testing.T) {
	const now = int64(1_800_000_000)
	want := map[string]bool{"/core:/stable:/v1.34/deb": true, "/repo/.": true, "/ubuntu/dists/noble": false,
		"/legacy//dists/x": false, "/repo/./.": false}
	var rows []cache.SuiteWithAdoption
	for p := range want {
		rows = append(rows, cache.SuiteWithAdoption{SuiteFreshness: cache.SuiteFreshness{CanonicalScheme: "http",
			CanonicalHost: "h", SuitePath: p}})
	}
	for _, e := range buildSuiteEntries(rows, now) {
		if e.Flat != want[e.SuitePath] {
			t.Errorf("%s: flat = %v, want %v", e.SuitePath, e.Flat, want[e.SuitePath])
		}
	}
}

// The status page tags a flat suite's path "flat" in the Suites table;
// a dists/ suite's cell is the path alone.
func TestSuitesTable_ShowsTheFlatTag(t *testing.T) {
	const now = int64(1_800_000_000)
	m := newHealthyModel()
	m.Suites = buildSuiteEntries([]cache.SuiteWithAdoption{
		{SuiteFreshness: cache.SuiteFreshness{CanonicalScheme: "https", CanonicalHost: "pkgs.k8s.io", SuitePath: "/core:/stable:/v1.34/deb"}},
		{SuiteFreshness: cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "archive.ubuntu.com", SuitePath: "/ubuntu/dists/noble"}},
	}, now)
	html := renderHTMLForGolden(t, m)
	mustContain(t, html,
		`<td data-label="Suite path" class="mono">/core:/stable:/v1.34/deb <span class="b b--neutral">flat</span></td>`,
		`<td data-label="Suite path" class="mono">/ubuntu/dists/noble</td>`,
	)
}

// Deferred minor B-62: an offset strings.Index did not find is no row.
func TestHTMLRowStart_AMissIsNoRow(t *testing.T) {
	for _, off := range []int{-1, 100} {
		if got := htmlRowStart("<tr a><td>x</td></tr>", off); got != "" {
			t.Errorf("htmlRowStart(…, %d) = %q, want \"\"", off, got)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -gcflags=-e ./internal/admin/`
Expected: FAIL — `unknown field Flat in struct literal of type suiteEntry`, `e.Flat undefined (type suiteEntry has no field or method Flat)` (twice).

- [ ] **Step 3: Implement**

In `internal/admin/status.go`, replace

```go
	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
```

with

```go
	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
```

In `internal/admin/status.go`, replace

```go
	Scheme                           string `json:"scheme"`
	Host                             string `json:"host"`
	SuitePath                        string `json:"suite_path"`
	LastCheckUnixTime                *int64 `json:"last_check_unixtime"`
```

with

```go
	Scheme    string `json:"scheme"`
	Host      string `json:"host"`
	SuitePath string `json:"suite_path"`
	// Flat marks a flat repository's suite (aptpath.IsFlatSuitePath: spec
	// C2's flat grammar, a ./ twin included; spec C7, C11): additive to
	// the locked JSON contract, like scheme. The HTML shows it as a tag
	// beside the suite path.
	Flat                             bool   `json:"flat"`
	LastCheckUnixTime                *int64 `json:"last_check_unixtime"`
```

In `internal/admin/status.go`, replace

```go
			Host:                             r.CanonicalHost,
			SuitePath:                        r.SuitePath,
			LastCheckUnixTime:                r.LastCheckAt,
```

with

```go
			Host:                             r.CanonicalHost,
			SuitePath:                        r.SuitePath,
			Flat:                             aptpath.IsFlatSuitePath(r.SuitePath),
			LastCheckUnixTime:                r.LastCheckAt,
```

In `internal/admin/status_template.go`, replace

```go
                <td data-label="Host" class="host">{{if .Scheme}}{{.Scheme}}://{{end}}{{.Host}}</td>
                <td data-label="Suite path" class="mono">{{.SuitePath}}</td>
                <td data-label="Last check" class="time">{{unixTimePtr .LastCheckUnixTime}}</td>
```

with

```go
                <td data-label="Host" class="host">{{if .Scheme}}{{.Scheme}}://{{end}}{{.Host}}</td>
                <td data-label="Suite path" class="mono">{{.SuitePath}}{{if .Flat}} <span class="b b--neutral">flat</span>{{end}}</td>
                <td data-label="Last check" class="time">{{unixTimePtr .LastCheckUnixTime}}</td>
```

- [ ] **Step 4: Regenerate the JSON golden and run the package**

Run: `go test ./internal/admin -run TestJSONContractPreserved -update.jsoncontract`
Expected: `ok`. Then `git diff --no-ext-diff internal/admin/testdata/json_contract_golden.json` shows exactly this hunk (the minimal golden, whose `suites` is `[]`, is unchanged):

```diff
@@ -81,12 +81,32 @@
       "scheme": "http",
       "host": "archive.ubuntu.com",
       "suite_path": "dists/noble",
+      "flat": false,
       "last_check_unixtime": 1700005000,
       "last_success_unixtime": 1700004000,
       "current_snapshot_id": 42,
       "current_snapshot_adopted_at_unixtime": 1700003500,
       "inrelease_change_seen_at_unixtime": 1700005500,
-      "anchor_form": "",
+      "anchor_form": "inline",
+      "last_requested_unixtime": 1700005000,
+      "check_fail_count": 2,
+      "adoption_fail_reason": "untrusted_signer",
+      "adoption_fail_count": 3,
+      "adoption_fail_first_unixtime": 1700004000,
+      "adoption_next_retry_unixtime": 1700005500,
+      "adoption_refused": false
+    },
+    {
+      "scheme": "https",
+      "host": "pkgs.k8s.io",
+      "suite_path": "/core:/stable:/v1.34/deb",
+      "flat": true,
+      "last_check_unixtime": null,
+      "last_success_unixtime": null,
+      "current_snapshot_id": 43,
+      "current_snapshot_adopted_at_unixtime": null,
+      "inrelease_change_seen_at_unixtime": null,
+      "anchor_form": "inline",
       "last_requested_unixtime": null,
       "check_fail_count": 0,
       "adoption_fail_reason": null,
```

Run: `go test ./internal/admin/`
Expected: PASS.

- [ ] **Step 5: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| `Flat` never set | `TestBuildSuiteEntries_Flat` (`/core:/stable:/v1.34/deb: flat = false, want true`), `TestSuitesTable_ShowsTheFlatTag` |
| `Flat` is `IsCanonicalSuitePath` (a `dists/` suite too) | `TestBuildSuiteEntries_Flat` (`/ubuntu/dists/noble: flat = true, want false`), `TestSuitesTable_ShowsTheFlatTag` |
| no `flat` tag in the template | `TestSuitesTable_ShowsTheFlatTag` |
| the JSON field omitted (`json:"-"`) | `TestJSONContractPreserved/populated` (`JSON contract drifted`) |
| B-59: any lag warns (`LagWarn = LagSeconds > 0`) | `TestSuitesTable_RendersRecordsAndCounts` (`missing "<b class=\"accent-warn\">1 LAGGING</b> …"`) |
| B-62: `htmlRowStart` without its guard | `TestHTMLRowStart_AMissIsNoRow` (`panic: runtime error: slice bounds out of range [:-1]`) |

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/admin
git add internal/admin/status.go internal/admin/status_template.go internal/admin/suites_status_test.go internal/admin/json_contract_test.go internal/admin/testdata/json_contract_golden.json
git commit -m "Show suites[].flat on the admin status page (spec C7, C11; deferred minors B-59, B-62)"
```

(Record the mutation table in the commit message.)

### Task 15: Phase G's definition of done

The DoD tests that span tasks, and the map from every DoD item to its tests. `TestDoD_NvidiaFlatSuiteAdopts` is the E carry-forward: the real libnvidia-container layout (`deb …/stable/deb/amd64 /`, an InRelease-only suite, a SHA512-only Release dated 2018, `./` Filenames). `TestDoD_EveryRealFilenameShapeAdoptsAndVouches` is Review Focus 3 end to end: each real Filename shape adopts through its suite's `Packages` and vouches the path apt requests, and `..`, `//`, a sibling directory, an undeclared version, a decoded epoch, a 33-segment path and an unsigned snapshot vouch nothing. `TestDoD_UntrustedFlatSuiteFromAV6CacheKeepsTheBackoff` is Review Focus 1 and spec §9's "an untrusted flat suite makes ≤ 1 attempt per backoff step", under the real scheduler for three simulated hours after a v6 upgrade. `TestServeHTTP_MintedFlatSuitesAtScale` is phase D's minted-suites DoD for flat suites and their twins, a sibling simulation (ruling R-G5: `TestServeHTTP_MintedSuitesAtScale` and `TestDoD_NinetyDaysOfReapingWithRestarts` get no new work; Task 12 gates its read so the 90-day simulation keeps its budget). No production code changes.

**Files:**
- Create: `internal/freshness/dod_g_test.go`, `internal/handler/flat_dod_test.go`

**Interfaces:**
- Consumes: the flat rig (Task 9), `hashRelease`, `fakePackagesStanzas`, `sha512Of`, `testHex64`, `errRigUntrusted`, `startSched`, `simtest.NewUpstream`, `simtest.Serve`, `poolBlob`, `RunLegacyTrustAudit`, `backoffDelay`, `cache.MigrateToVersionForTest`, `cache.CachedAnchor`; handler tests' `mintedUpstream`, `memberStore`, `plainVerifier`.
- Produces: `flatRig.adoptFlatRig`, `flatRig.vouches`, `timedVerifier`, `mintedFlatUpstream` (tests).

- [ ] **Step 1: Write the DoD tests**

Create `internal/freshness/dod_g_test.go`:

```go
package freshness

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// adoptFlatRig adopts the flat rig's suite inline from a Release whose
// SHA256 block declares the root Packages pkgs, and fails the test unless
// the check was a first adoption.
func (r *flatRig) adoptFlatRig(pkgs []byte) {
	r.t.Helper()
	in := simtest.ClearsignedFixture(string(hashRelease("Origin: flat\n", true, false, map[string][]byte{"Packages": pkgs}, nil)))
	r.cacheRow("InRelease", in, 1, r.clock.now().Unix(), "")
	r.up.setAt(r.url("InRelease"), in)
	r.members.put(r.url("Packages"), pkgs)
	r.check()
	if r.lastResult() != "first_adoption" || r.row().CurrentSnapshotID == nil {
		r.t.Fatalf("%s: result %q; want a first adoption", r.suite.SuitePath, r.lastResult())
	}
}

// vouches reports whether a request for path on the flat rig's host is
// vouched (spec C6: DeclaredHashesForPath finds testHex64).
func (r *flatRig) vouches(path string) bool {
	r.t.Helper()
	rows, err := r.cache.DeclaredHashesForPath(context.Background(), "http", flatRigHost, path)
	if err != nil {
		r.t.Fatal(err)
	}
	return len(rows) == 1 && rows[0].DeclaredSHA256 == testHex64
}

// The E carry-forward (spec §3, libnvidia-container; phase E's DoD was
// dists/-shaped): the real flat layout — "deb …/stable/deb/amd64 /",
// an InRelease-only suite whose Release has only a SHA512 block and a
// Date frozen in 2018, and a SHA256 Packages whose Filenames start "./" —
// adopts on its first check, and vouches its .deb by the path apt
// requests, with or without the "./".
func TestDoD_NvidiaFlatSuiteAdopts(t *testing.T) {
	const suite = "/libnvidia-container/stable/deb/amd64"
	r := newFlatRig(t, suite)
	pkgs := fakePackagesStanzas(map[string]string{"./libnvidia-container1_1.17.8-1_amd64.deb": testHex64})
	text := fmt.Sprintf("Origin: https://nvidia.github.io/libnvidia-container/stable/deb/amd64\n"+
		"Label: https://nvidia.github.io/libnvidia-container/stable/deb/amd64\n"+
		"Date: Wed, 09 May 2018 22:33:58 GMT\n"+
		"SHA512:\n %s %d Packages\n", sha512Of(pkgs), len(pkgs))
	in := simtest.ClearsignedFixture(text)
	r.cacheRow("InRelease", in, 1, r.clock.now().Unix(), "")
	r.up.setAt(r.url("InRelease"), in)
	r.members.put(r.url("Packages"), pkgs)
	r.check()
	if r.lastResult() != "first_adoption" || r.row().CurrentSnapshotID == nil {
		t.Fatalf("result %q; want a first adoption", r.lastResult())
	}
	for _, p := range []string{suite + "/./libnvidia-container1_1.17.8-1_amd64.deb", suite + "/libnvidia-container1_1.17.8-1_amd64.deb"} {
		if !r.vouches(p) {
			t.Errorf("%s is not vouched", p)
		}
	}
}

// Review Focus 3 end to end (spec C5, C6; Tasks 4 and 5 pin the parts):
// each real Filename shape adopts through its suite's Packages and
// vouches the path apt requests for it — and nothing else vouches.
func TestDoD_EveryRealFilenameShapeAdoptsAndVouches(t *testing.T) {
	deep := "/" + strings.TrimSuffix(strings.Repeat("d/", 31), "/") // 31 segments
	for _, tc := range []struct {
		name, suite, filename string
		vouched, refused      []string
	}{
		{"pkgs.k8s.io (amd64/)", "/core:/stable:/v1.34/deb", "amd64/kubectl_1.34.1-1.1_amd64.deb",
			[]string{"/core:/stable:/v1.34/deb/amd64/kubectl_1.34.1-1.1_amd64.deb"},
			[]string{
				"/core:/stable:/v1.34/deb/../deb/amd64/kubectl_1.34.1-1.1_amd64.deb", // a .. segment
				"/core:/stable:/v1.34/deb//amd64/kubectl_1.34.1-1.1_amd64.deb",       // an empty segment
				"/core:/stable:/v1.34/deb2/amd64/kubectl_1.34.1-1.1_amd64.deb",       // a sibling directory
				"/core:/stable:/v1.34/deb/amd64/kubectl_1.34.2-1.1_amd64.deb",        // not declared
			}},
		{"CUDA (./)", "/compute/cuda/repos/ubuntu2404/x86_64", "./cuda-toolkit-12-6_12.6.3-1_amd64.deb",
			[]string{"/compute/cuda/repos/ubuntu2404/x86_64/./cuda-toolkit-12-6_12.6.3-1_amd64.deb",
				"/compute/cuda/repos/ubuntu2404/x86_64/cuda-toolkit-12-6_12.6.3-1_amd64.deb"}, nil},
		{"Jenkins (binary/: the parent base)", "/debian-stable/binary", "binary/jenkins_2.462.3_all.deb",
			[]string{"/debian-stable/binary/jenkins_2.462.3_all.deb"},
			[]string{"/other/binary/jenkins_2.462.3_all.deb"}},
		{"Jenkins (direct/: the parent base)", "/debian-stable/binary", "direct/jenkins_2.400_all.deb",
			[]string{"/debian-stable/direct/jenkins_2.400_all.deb"}, nil},
		{"Sublime Text (files/: the host-root base)", "/apt/stable", "files/sublime-text_build-4180_amd64.deb",
			[]string{"/files/sublime-text_build-4180_amd64.deb"}, nil},
		{"an epoch (%3a)", "/repo", "amd64/foo_1%3a2.0-1_amd64.deb",
			[]string{"/repo/amd64/foo_1%3a2.0-1_amd64.deb"}, []string{"/repo/amd64/foo_1:2.0-1_amd64.deb"}},
		{"a 31-segment suite: 33 segments are past the fallback", deep, "./amd64/x_1_amd64.deb",
			[]string{deep + "/./amd64/x_1_amd64.deb"}, []string{deep + "/amd64/x_1_amd64.deb"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newFlatRig(t, tc.suite)
			r.adoptFlatRig(fakePackagesStanzas(map[string]string{tc.filename: testHex64}))
			for _, p := range tc.vouched {
				if !r.vouches(p) {
					t.Errorf("%s is not vouched", p)
				}
			}
			for _, p := range tc.refused {
				if r.vouches(p) {
					t.Errorf("%s is vouched, want nothing", p)
				}
			}
		})
	}
	t.Run("an unsigned snapshot vouches nothing", func(t *testing.T) {
		r := newFlatRig(t, "/unsigned")
		r.adopter.adoptUnsigned = true
		pkgs := fakePackagesStanzas(map[string]string{"amd64/u_1_amd64.deb": testHex64})
		r.members.put(r.url("Packages"), pkgs)
		rel := hashRelease("Origin: unsigned\n", true, false, map[string][]byte{"Packages": pkgs}, nil)
		if err := r.adopter.RunUnsigned(context.Background(), r.suite, rel, "", ""); err != nil || r.row().CurrentSnapshotID == nil {
			t.Fatalf("RunUnsigned = %v; want an unsigned snapshot", err)
		}
		if r.vouches("/unsigned/amd64/u_1_amd64.deb") {
			t.Fatal("an unsigned snapshot vouched its .deb")
		}
	})
}

// timedVerifier refuses every signature as an untrusted signer and
// records when each verification ran (bubble time).
type timedVerifier struct {
	mu sync.Mutex
	at []time.Time
}

func (v *timedVerifier) record() ([]byte, error) {
	v.mu.Lock()
	v.at = append(v.at, time.Now())
	v.mu.Unlock()
	return nil, errRigUntrusted
}

func (v *timedVerifier) VerifyInline(context.Context, SuiteRef, []byte) ([]byte, error) {
	return v.record()
}

func (v *timedVerifier) VerifyDetached(context.Context, SuiteRef, []byte, []byte) ([]byte, error) {
	return v.record()
}

func (v *timedVerifier) times() []time.Time {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]time.Time(nil), v.at...)
}

// Review Focus 1, spec §9's G row ("flat legacy onboarding on a v6
// fixture: an untrusted flat suite makes ≤ 1 attempt per backoff step"):
// a flat InRelease a v6 binary cached, with no suite row, is upgraded in
// place; the startup pass marks nothing (the suite is untracked); its
// first serve onboards the suite with its base's marker (the handler's
// cache call, CreateSuite with the cached signed anchor:
// TestServeHTTP_OnboardsAFlatSuiteFromAV6Cache pins the handler's side).
// Its signer is not trusted: under the real scheduler for three hours each
// adoption attempt comes at least one backoff step after the last — 2 min
// doubling to 1 h — the suite keeps retrying, and the marker stays.
func TestDoD_UntrustedFlatSuiteFromAV6CacheKeepsTheBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		dir := t.TempDir()
		if err := cache.MigrateToVersionForTest(ctx, dir, 6); err != nil {
			t.Fatal(err)
		}
		pkgs := fakePackagesStanzas(map[string]string{"amd64/x_1_amd64.deb": testHex64})
		in := simtest.ClearsignedFixture(string(hashRelease("Origin: flat\n", true, false, map[string][]byte{"Packages": pkgs}, nil)))
		hash := hashOf(in)
		db, err := sql.Open("sqlite", filepath.Join(dir, "cache.db"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO blob(hash,size,created_at,refcount) VALUES(?,?,1,0)`, hash, len(in)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO url_path(canonical_scheme,canonical_host,path,blob_hash,upstream_url,is_metadata,last_requested_at,request_count)
		 VALUES('http',?,'/repo/InRelease',?,?,1,?,3)`, flatRigHost, hash, "http://"+flatRigHost+"/repo/InRelease", time.Now().Unix()); err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
		c, err := cache.Open(ctx, dir, discardLogger()) // migrates v6 → v7
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		if got := poolBlob(t, c, in); got != hash { // the pool file the v6 binary wrote
			t.Fatalf("pool file %s, want %s", got, hash)
		}
		if res, err := RunLegacyTrustAudit(ctx, c, discardAuditLogger()); err != nil || res.AnchorMarkers != 0 {
			t.Fatalf("startup pass = %+v, %v; want no marker for the untracked flat suite", res, err)
		}
		key := cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: flatRigHost, SuitePath: "/repo"}
		if res, err := c.CreateSuite(ctx, cache.SuiteSeed{SuiteKey: key, RequestedAt: time.Now().Unix(),
			Cached: []cache.CachedAnchor{{Path: "/repo/InRelease", BlobHash: hash, Signed: true}}}, cache.SuiteCaps{}, nil); err != nil ||
			res.Outcome != cache.SuiteCreated {
			t.Fatalf("onboarding = %+v, %v", res, err)
		}
		if _, ok, err := c.SignedSeen(ctx, "http", flatRigHost, "/repo"); err != nil || !ok {
			t.Fatalf("onboarding wrote no marker (%v, %v)", ok, err)
		}
		ver := &timedVerifier{}
		members := newFakeFetcher()
		members.put("http://"+flatRigHost+"/repo/Packages", pkgs)
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: ver, HostLimiter: hostsem.New(8),
			MaxConcurrent: 2, Logger: simtest.DiscardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		up := simtest.NewUpstream()
		up.Set("http://"+flatRigHost+"/repo/InRelease", simtest.Serve(in))
		_, stop := startSched(t, Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute,
			Cooldown: time.Minute, Adopter: ad, Logger: simtest.DiscardLogger(), rand: func() float64 { return 0.5 }})
		time.Sleep(3 * time.Hour)
		stop()
		at := ver.times()
		if len(at) < 6 {
			t.Fatalf("%d adoption attempts in 3 h, want at least 6: the suite must keep retrying", len(at))
		}
		for i := 1; i < len(at); i++ {
			if gap, step := at[i].Sub(at[i-1]), backoffDelay(int64(i)); gap < step {
				t.Fatalf("attempt %d came %v after attempt %d, inside its backoff step %v", i+1, gap, i, step)
			}
		}
		if _, ok, err := c.SignedSeen(ctx, "http", flatRigHost, "/repo"); err != nil || !ok {
			t.Fatalf("the marker went (%v, %v)", ok, err)
		}
		if row, err := c.GetSuiteFreshness(ctx, "http", flatRigHost, "/repo"); err != nil || row.CurrentSnapshotID != nil {
			t.Fatalf("row %+v, %v; want nothing adopted", row, err)
		}
	})
}
```

Create `internal/handler/flat_dod_test.go`:

```go
package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"
	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// mintedFlatUpstream is mintedUpstream for flat repositories: a root
// Packages.gz answers gzip magic too, so every flat suite file passes its
// sniff.
type mintedFlatUpstream struct{ mintedUpstream }

func (u mintedFlatUpstream) Fetch(ctx context.Context, target *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error) {
	if !strings.HasSuffix(target.URL, "/Packages.gz") {
		return u.mintedUpstream.Fetch(ctx, target, dst)
	}
	body := append([]byte{0x1f, 0x8b, 0x08, 0x00}, target.URL...)
	if _, err := dst.Write(body); err != nil {
		return nil, err
	}
	return &fetch.FetchResult{Status: 200, ContentLength: int64(len(body))}, nil
}

// Spec §9's G row with phase D's minted-suites DoD (Review Focus 1) for
// flat repositories — a sibling simulation, so TestServeHTTP_MintedSuitesAtScale
// keeps its 4.5 s budget (ruling R-G5, deferred minor F-12): minted flat
// directories on many hosts — each a suite and its ./ twin, with their
// InRelease, Release.gpg, root Packages.gz and by-hash names, the cap
// splitting each pair — from an
// upstream that answers every one with sniff-passing bytes create rows
// only up to the caps (2 per host, 20 in all — freshness.max_suites 20,
// a tenth of it per host: ten hosts fill theirs, the eleventh is refused
// at the global cap), a marker exactly for the bases
// of tracked suites (a suite and its twin share one), and no lock-map,
// flip-gate or touch-map entry beyond the tracked suites; the rejection
// cache holds the refused suites only.
func TestServeHTTP_MintedFlatSuitesAtScale(t *testing.T) {
	synctest.Test(t, testMintedFlatSuitesAtScale)
}

func testMintedFlatSuitesAtScale(t *testing.T) {
	c := simtest.OpenCache(t)
	ctx := context.Background()
	gate, err := flipgate.New(flipgate.Config{Quiet: flipgate.MaxQuiet})
	if err != nil {
		t.Fatal(err)
	}
	up := &simtest.StaticConditional{Status: http.StatusNotModified}
	ad, err := freshness.NewAdopter(freshness.AdoptionConfig{Cache: c, Fetcher: &memberStore{bodies: map[string][]byte{}},
		Verifier: plainVerifier{}, HostLimiter: hostsem.New(64), Logger: simtest.DiscardLogger(), FlipGate: gate})
	if err != nil {
		t.Fatal(err)
	}
	const hosts, dirs, perHostCap, maxSuites = 11, 2, 2, 20
	ck, err := freshness.New(freshness.Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(64),
		Logger: simtest.DiscardLogger(), MaxSuites: maxSuites, Adopter: ad})
	if err != nil {
		t.Fatal(err)
	}
	parser, err := proxy.New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{Parser: parser, Cache: c, Fetch: mintedFlatUpstream{}, Logger: simtest.DiscardLogger(),
		HostLimiter: hostsem.New(64), Freshness: ck, AdoptionEnabled: true, FlipGate: ad.FlipGate(),
		MaxSuites: maxSuites, SuiteGuard: ck})
	if err != nil {
		t.Fatal(err)
	}
	closeH := sync.OnceFunc(h.Close)
	t.Cleanup(closeH)
	byHash := "/by-hash/SHA256/" + strings.Repeat("ab", 32)
	// The first directory's twin comes before its suite, the second's
	// suite before its twin, so the per-host cap tracks one of each pair:
	// the twin's marker must land on the base it shares.
	order := []string{"/repo000/.", "/repo001", "/repo000", "/repo001/."}
	for i := range hosts {
		for _, suite := range order {
			base := fmt.Sprintf("http://f%02d.example%s", i, suite)
			for _, p := range []string{"/InRelease", "/Release.gpg", "/Packages.gz", byHash, "/InRelease"} {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, base+p, nil))
				if rec.Code != http.StatusOK {
					t.Fatalf("%s%s = %d", base, p, rec.Code)
				}
			}
		}
	}
	closeH() // drains the T1 checks
	all, err := c.ListSuites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tracked := map[string]bool{}
	rowsOf := map[string]int{}
	for _, s := range all {
		if !aptpath.IsFlatSuitePath(s.SuitePath) {
			t.Fatalf("row %s%s is no flat suite", s.CanonicalHost, s.SuitePath)
		}
		tracked[s.CanonicalHost+s.SuitePath] = true
		rowsOf[s.CanonicalHost]++
	}
	if len(all) != maxSuites {
		t.Fatalf("rows = %d, want %d (the global cap)", len(all), maxSuites)
	}
	if !tracked["f00.example/repo000/."] || tracked["f00.example/repo000"] {
		t.Fatalf("f00: tracked %v; want the first directory's twin, not its suite", tracked)
	}
	for i := range hosts - 1 {
		if n := rowsOf[fmt.Sprintf("f%02d.example", i)]; n != perHostCap {
			t.Fatalf("f%02d: %d rows, want the per-host cap %d", i, n, perHostCap)
		}
	}
	if n := rowsOf[fmt.Sprintf("f%02d.example", hosts-1)]; n != 0 {
		t.Fatalf("f%02d: %d rows, want 0 (refused at the global cap)", hosts-1, n)
	}
	for i := range hosts {
		for j := range dirs {
			host, suite := fmt.Sprintf("f%02d.example", i), fmt.Sprintf("/repo%03d", j)
			_, ok, err := c.SignedSeen(ctx, "http", host, aptpath.MarkerBase(suite))
			if err != nil {
				t.Fatal(err)
			}
			if want := tracked[host+suite] || tracked[host+suite+"/."]; ok != want {
				t.Fatalf("%s%s: marker %v, want %v (a marker exactly for a tracked suite's base)", host, suite, ok, want)
			}
		}
	}
	if n := ck.LockCount(); n != len(all) {
		t.Errorf("lock map = %d entries, want one per tracked suite (%d)", n, len(all))
	}
	if n := gate.Len(); n != len(all) {
		t.Errorf("flip gate = %d entries, want one per tracked suite (%d)", n, len(all))
	}
	if n := c.TouchedSuites(); n != len(all) {
		t.Errorf("touch map = %d entries, want one per tracked suite (%d)", n, len(all))
	}
	if n, want := h.rejections.len(), (hosts-1)*(2*dirs-perHostCap)+2*dirs; n != want {
		t.Errorf("rejection cache = %d entries, want the %d refused suites", n, want)
	}
}
```

- [ ] **Step 2: Run them**

Run: `go test ./internal/freshness/ -run 'TestDoD_NvidiaFlatSuiteAdopts|TestDoD_EveryRealFilenameShapeAdoptsAndVouches|TestDoD_UntrustedFlatSuiteFromAV6CacheKeepsTheBackoff' -v`
Expected: PASS, eight `TestDoD_EveryRealFilenameShapeAdoptsAndVouches` subtests. (No red step of their own: Tasks 1–14 built what they pin; the mutations below are their red runs.)

Run: `go test -race -count=3 ./internal/handler/ -run 'TestServeHTTP_MintedFlatSuitesAtScale|TestServeHTTP_MintedSuitesAtScale$' -v`
Expected: PASS; each run well under the 4.5 s simulation budget (measured on the plan's machine: 1.5–1.8 s for the flat simulation, 2.5 s for phase D's, which phase F measured at 3.76–3.94 s on its own machine).

Run: `go test -race -count=3 ./internal/freshness/ -run 'TestDoD_NinetyDaysOfReapingWithRestarts|TestDoD_UntrustedFlatSuiteFromAV6CacheKeepsTheBackoff' -v`
Expected: PASS; the 90-day simulation within 0.1 s of the base commit's time (the plan's machine: 3.0 s at the base and after Task 12), the backoff simulation under 0.5 s.

- [ ] **Step 3: Mutation checks** (one at a time, each reverted)

| Mutation | Fails |
|---|---|
| `flat_filename` without `DotClean` (Task 4) | `TestDoD_NvidiaFlatSuiteAdopts` (`…/libnvidia-container1_1.17.8-1_amd64.deb is not vouched`), `TestDoD_EveryRealFilenameShapeAdoptsAndVouches/CUDA_(./)` |
| the fallback's base must equal the suite's `FlatBase` (Task 5) | `TestDoD_EveryRealFilenameShapeAdoptsAndVouches/Jenkins_(binary/:_the_parent_base)`, `/Jenkins_(direct/:_the_parent_base)`, `/Sublime_Text_(files/:_the_host-root_base)` |
| the adoption backoff an eighth as long | `TestDoD_UntrustedFlatSuiteFromAV6CacheKeepsTheBackoff` (`attempt 3 came 3m45s after attempt 2, inside its backoff step 4m0s`) |
| onboarding marks no cached signed anchor (`markCreated`) | `TestDoD_UntrustedFlatSuiteFromAV6CacheKeepsTheBackoff` (`onboarding wrote no marker`) |
| a per-host cap of a fifth of `max_suites` | `TestServeHTTP_MintedFlatSuitesAtScale` (`f00: tracked …; want the first directory's twin, not its suite`) |
| `MarkerBase` keeps a twin's `/.` | `TestServeHTTP_MintedFlatSuitesAtScale` (`f00.example/repo000: marker false, want true`) |

- [ ] **Step 4: The whole tree**

Run: `go vet ./... && go test ./... && go test -race -timeout 20m ./...`
Expected: PASS.

Run: `golangci-lint run ./...`
Expected: no new findings (the CI lint job runs it).

Run: `gofmt -l .`
Expected: only the five drift files of the Global Constraints.

- [ ] **Step 5: The DoD map**

Every item of spec §9's G row, the overview's G line, the G carry-forwards, PR #8 parity items 1–4 and rulings R-G1..R-G5, with its tests:

| DoD item | Tests (task) |
|---|---|
| §9: C2 flat grammar; the pkgs.k8s.io shape `/core:/stable:/v1.34/deb/InRelease` → `/core:/stable:/v1.34/deb` | `TestSuitePath_Flat`, `TestIsCanonicalSuitePath`, `TestIsFlatSuitePath`, `TestFlatBase`, `TestDotClean`, `TestIsFlatHostRootPath`, `TestParse_FlatSuites` (1) |
| §9: flat IndexTargets, layout-aware, adoption and handler sides | `TestIsIndexTarget_LayoutAware`, `TestIndexTargetGroup_LayoutAware`, `TestArchFromFilteredPath_LayoutAware`, `TestMissingRequestableIndexGroups_LayoutAware`, `TestMissingRequestableMembers_Flat`, `TestReconcileSnapshot_HealsAFlatSuitesPackages` (2); `TestIsIndexTargetPath_LayoutAware`, `TestServeHTTP_FlatSnapshotMissIndexTargetRaisesAlert` (6) |
| §9: `validateFilename` | `TestValidateFilename`, `TestParsePackages_KeepsDotAndPercentFilenamesLiterally` (3) |
| §9: C6 materialisation, fallback, coverage | `TestPackageHash_FlatFilenamePersists`, `TestAdopter_MaterialisesFlatPackageRows`, `TestAdopter_FlatCoverageReadsTheRootPackagesOnly` (4); `TestDeclaredHashesForPath_FlatShapes`, `…_TheMostSpecificBaseWins`, `…_TheFallbackReadsCurrentSignedSnapshotsOnly`, `…_TheFallbackUsesThePartialIndex` (5) |
| §9: anchorless refresh (and its tests) | `TestCheck_FlatIndexRowsCountAsClientCreated` (9); `TestCheck_AnchorlessRefreshRevalidatesTheIndexes`, `…SniffRejectionKeepsTheRow`, `…AnswersByStatus`, `…DeletesTheAnchorsUpstreamDropped`, `…GateKeepsSignedSuitesOut`, `…ClearsTheRecordAndThePendingChange`, `…RefreshesItsOwnRowsOnly`, `…ShutdownWritesNothing`, `TestCheck_AnAnchorlessSuiteThatPublishesAnInReleaseAdopts`, `TestSniffAnchorlessIndex`, `TestRefreshURLPathBlob_ReplacesTheBlobAndKeepsTheInterest`, `TestDeleteURLPathRows_DeletesTheRowsNamed` (10) |
| §9: flat root-index touch | `TestServeHTTP_FlatRootIndexServesTouchTheirSuite` (7) |
| §9: flat pdiff arch labels (design note 4, flagged) | `TestArchFromFilteredPath_LayoutAware` (2), `TestIndexTargetArch` (6) |
| §9 / overview: PR #8's evidence — a miss on `…/deb/InRelease` and `…/deb/./InRelease` creates one row each and fires T1 under it; a hit fires T1 again; `…/deb/Packages_1.0_all.deb`, host-root `/InRelease` and `…/deb//InRelease` create none | `TestServeHTTP_FlatRepositoryFreshness` (7); `TestServeHTTP_FlatHostRootIsUnsupportedAndSaysSo` (6) |
| §9: every real Filename shape adopts and vouches | `TestDoD_EveryRealFilenameShapeAdoptsAndVouches`, `TestDoD_NvidiaFlatSuiteAdopts` (15); `TestDeclaredHashesForPath_FlatShapes` (5) |
| §9 / parity 3: flat `Packages` failure fatal | `TestAdopter_FlatPackagesFailureFatal` (2) |
| §9 / parity 4: the strict matrix | `TestServeHTTP_FlatStrictMatrix`, `TestServeHTTP_CurrentSnapshotCountCountsFlatSnapshots` (11) |
| §9: flat legacy onboarding on a v6 fixture; an untrusted flat suite makes ≤ 1 attempt per backoff step | `TestServeHTTP_OnboardsAFlatSuiteFromAV6Cache` (7); `TestDoD_UntrustedFlatSuiteFromAV6CacheKeepsTheBackoff` (15) |
| §9: reaping `/repo` leaves `/repo/.` intact | `TestReap_AFlatSuiteLeavesItsTwin`, `TestReaper_ReapsIdleAndNonCanonicalSuitesAndPurgesThem` (12); `TestDeleteSuiteMetadata_AFlatSuiteLeavesItsTwin` (1) |
| Parity 1: PR #8's flat `TestSuitePath` cases | `TestSuitePath_Flat` (1) |
| Parity 2: index targets, flat and `dists/` runs | the five `…_LayoutAware` tests (2, 6) |
| Parity 3: `TestAdopter_FlatSuiteAdopts`, `TestAdopter_FlatPackagesFailureFatal` | same names (2) |
| Overview: `IsFlatSuitePath`, `FlatBase`, `DotClean`, `validateFilename` | Task 1 and 3 rows above |
| Overview / (B) carry-forward: admin `suites[].flat`, `docs/admin-ui-spec.md` §4.5 | `TestBuildSuiteEntries_Flat`, `TestSuitesTable_ShowsTheFlatTag`, `TestJSONContractPreserved` (14); the doc (16) |
| Overview: legacy flat onboarding writes markers for sniff-passing cached flat anchors | `TestServeHTTP_HitOnboardsAFlatSuite`, `TestServeHTTP_OnboardsAFlatSuiteFromAV6Cache` (7) |
| Overview / (E): the `/.` twin variant of E's in-transaction recheck | `TestCheck_UnsignedRefusedInsideTheCommitIsAState/the_./_twin's_anchor` (13) |
| (C): `Last-Modified = max(pool mtime, last_fetched_at)`, finalize-before-stamp kept (ruling R-G3) | `TestServeHTTP_URLPathMetadataLastModifiedIsTheLaterStamp`, `TestServeHTTP_AMissStampsItsRowAfterItsPoolFile` (10) |
| (D): flat cases in the miss-path, onboarding and touch tests; the cap-rejection cache keyed by suite | `TestServeHTTP_FlatIndexMissCreatesItsSuiteRow`, `TestServeHTTP_HitOnboardsAFlatSuite`, `TestServeHTTP_FlatRootIndexServesTouchTheirSuite`, `TestServeHTTP_VouchedFlatDebTouchesItsSuite` (7); `TestServeHTTP_MintedFlatSuitesAtScale`'s rejection count (15) |
| (E): Dispatch 3's flat index rows; the anchorless refresh where the tree finds neither anchor | `TestCheck_FlatIndexRowsCountAsClientCreated`, `TestCheck_AFlatSuiteAdoptsOnItsFirstCheck` (9); Task 10's tests |
| (E): the nvidia DoD on the real flat layout | `TestDoD_NvidiaFlatSuiteAdopts` (15) |
| (E) / ruling R-G1: a `Packages` with no SHA256 stanza adopts incomplete; a body with no Filename keeps failing | `TestParsePackages_OnlyStanzasWithoutSHA256Parse`, `TestParsePackages_NoFilenameStanzaStillFails`, `TestAdopter_PackagesWithoutAnySHA256AdoptsIncomplete` (3) |
| (F): flat suites canonical, reaped by idleness; a flat suite kept while used | `TestReaper_ReapsIdleAndNonCanonicalSuitesAndPurgesThem` (12) |
| (F): `/forget`'s twin subtest on a real flat suite | `TestDoD_ForgetIsTheEscapeHatchForAFlatSuitesDowngradeRefusal` (12) |
| (F) / ruling R-G2: flat index rows as junk evidence, both ways | `TestReaper_ReapsAJunkAnchorlessFlatSuite`, `TestListUnadoptedFlatIndexRows_*`, `TestHasAnchorlessFlatSuite` (12) |
| Ruling R-G4: twins | `TestServeHTTP_FlatRepositoryFreshness` (7), `TestCheck_AnchorlessRefreshesItsOwnRowsOnly` (10), Task 12's and 13's tests, `TestServeHTTP_MintedFlatSuitesAtScale` (15) |
| Ruling R-G5 / F-12: the simulation budget | `TestHasAnchorlessFlatSuite` (12); the timings in Step 2 (15) |

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/freshness/dod_g_test.go internal/handler/flat_dod_test.go
git add internal/freshness/dod_g_test.go internal/handler/flat_dod_test.go
git commit -m "Phase G definition of done: real Filename shapes, the nvidia flat layout, the v6 backoff and minted flat suites"
```

(Record the mutation table and the Step 2 timings in the commit message.)

### Task 16: Documentation, spec revision 10, overview, deferred files, final verification

Spec §8 for this phase — the operator documentation of flat repositories (*Flat repositories* under `[freshness]`: the two source forms and their twins, the host-root limitation, flat adoption and vouching, the anchorless refresh and its `Last-Modified`), of the anchorless results, the new WARNs and the metric (`docs/log-fields.md`), of the flat junk evidence (*Suite reaping*), of `suites[].flat` (`docs/admin-ui-spec.md` §4.5, the B carry-forward) and of strict mode's flat snapshots in the conffile — then this plan's rulings R-G1–R-G5, deviations 1 and 2 and the design notes the spec should state, written into the spec as revision 10; the overview's phase G bookkeeping and its carry-forwards and open decisions for H; and the deferred files. The rulings' docs, each exact against the code at the end of Task 15:
- R-G1: spec C5 (a `Packages` file with no SHA256 stanza adopts incomplete; a body with no `Filename` stanza still fails); `package_coverage_incomplete`'s `no_sha256` text is unchanged (it already covers it);
- R-G2, R-G5: spec C9, *Suite reaping*, `suite_reaped`'s `junk`, the reaper WARN row's new `list unadopted flat indexes failed`;
- R-G3: spec C4 Dispatch 4's last sentence (the stamp-after-pool rule); *Flat repositories*' `Last-Modified` sentence;
- R-G4: *Flat repositories*' twin paragraph and *Suite reaping*'s "never its `./` twin's";
- deviation 1: spec C4 Dispatch 3's bullet, *Flat repositories*' last sentence, log-fields' anchorless paragraph; deviation 2: spec C4 Dispatch 4, *Flat repositories*, log-fields;
- design notes 2 (the WARN and its rate limit: spec C2, log-fields), 5 (the only-dots Filename: spec C5), 9–14 (spec C4 Dispatch 3–4, log-fields, *Flat repositories*), 15 (spec C9), 16 (admin-ui-spec §4.5).

Strict mode's other flat consequences — a flat snapshot with incomplete coverage turns strict mode off for its host, the first install from a newly added flat suite can be refused while its first adoption is queued, and flat-only hosts are now protected — are H's documentation (overview, "PR #8 parity", H); this task changes only the conffile's one-line statement of the rule. `README.md` is H's (a new H carry-forward).

Deferred minors in the files this task changes: none (C-36, D-50 are H's).

**Every deferred minor, as this plan leaves it.** Taken: A-4, A-5, A-6, D-3, D-4 (Task 1); D-27, E-9 (Task 2); D-31, D-32, E-57, E-60, E-64, E-65, E-67, E-68, E-71 (Task 8); E-47 (Task 10); E-70 (Task 11); F-12 (Task 12's gated read, with Task 15's sibling simulation and timings); E-16, E-17, E-19 (Task 13); B-59, B-62 (Task 14). Checked and left:
- tagged for H, in files this phase changes: A-56 (`handler.go`, `freshness.go`, `adoption.go`), C-21, C-36 (`docs/admin-ui-spec.md`), D-50 (`status_template.go`), D-56 (`adoption_test.go`), E-24 (`first_adoption.go`), F-18, F-22, F-26 (`reaper.go`), F-19 (`status_template.go`); and F-4, F-5, F-6 (G-1, the admin endpoints, or H);
- in a file this phase changes only by one argument at one call site (ruling PF-11's rule: that touches the function, not the file): E-31 and E-45 (`freshness.go`, whose `planRepair` passes `rp.suite` to `reconcilePending`, Task 2);
- with a site in a file this phase changes by hand but a fix that belongs to files it does not change: D-28 (`phase2_handler_test.go`; `handler_test.go` only holds the shadowed helper), E-3 (`unsigned_form_test.go`), E-13 (`flip_barrier.go`), E-32 (`check.go`, `current_form.go`), E-40 (`freshness_test.go`, `scheduler.go`);
- B-63 (`status.go`, which Task 14 changes): kept deferred by ruling (final review T M9), and its "next check in" needs the scheduler's backoff, which `internal/admin` reaches only through injected interfaces — its entry gains that note (Step 6);
- B-60 (`status_test.go`), F-8 (`first_adoption_test.go`), F-20 (the admin test servers) and every other entry: in files no task changes.

**Files:**
- Modify: `docs/configuration.md`, `docs/log-fields.md`, `docs/admin-ui-spec.md`, `packaging/config/config.toml.default`, `docs/superpowers/specs/2026-09-27-flat-repositories-design.md`, `docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md`, `…-A-deferred.md`, `…-B-deferred.md`, `…-D-deferred.md`, `…-E-deferred.md`, `…-F-deferred.md`

**Interfaces:**
- Consumes: the names, events, metrics and constants Tasks 1–15 produce, exactly as they produce them.
- Produces: no code.

- [ ] **Step 1: Operator documentation (`docs/configuration.md`)**

*Eager first adoption* counts a flat repository's root `Packages`/`Sources` as a client-created row and points a Release-less flat repository at the new *Flat repositories* subsection; *Suite reaping* names a flat repository's files and its twin, and the flat junk evidence:

```diff
diff --git a/docs/configuration.md b/docs/configuration.md
--- a/docs/configuration.md
+++ b/docs/configuration.md
@@ -274,7 +274,8 @@ a tracked suite; otherwise it is fetched from upstream.
 With adoption enabled, a tracked suite that has no snapshot yet is
 adopted on its first check, in whichever form its upstream publishes (see
 *Anchor forms and unsigned repositories*), when a client asked for it in
-the last 7 days and a client fetched its `InRelease` or `Release` through
+the last 7 days and a client fetched its `InRelease` or `Release` — or,
+for a flat repository, its root `Packages` or `Sources` — through
 the cache (a copy the cache rebuilt for itself does not count). The
 request that creates a suite starts its first check once its response is
 sent, so a new repository is adopted within seconds of its first
@@ -294,7 +295,9 @@ doubling to an hour), or once `periodic_refresh` has passed since its last
 check if that comes first; checks that clients trigger in between ask
 upstream nothing. A suite whose upstream serves neither `InRelease` nor
 `Release` fails its check (backing off from 2 minutes to an hour) and
-shows "refused: anchor_absent" on the status page.
+shows "refused: anchor_absent" on the status page — unless it is a flat
+repository the cache has never seen signed, which is refreshed without a
+`Release` (see *Flat repositories*).
 
 First adoptions never take the last free adoption slot, which is kept for
 suites that already serve a signed snapshot (see *Capacity and
@@ -313,6 +316,56 @@ only, and a miss that creates a suite starts no check (a cache hit on a
 suite's anchor still triggers its usual check, under the `cooldown`, also
 the hit that creates the suite's row).
 
+### Flat repositories
+
+A flat repository — one without a `dists/` tree, configured as
+`deb <uri> <dir>/` or `deb <uri> ./` (pkgs.k8s.io, NVIDIA CUDA,
+pkg.jenkins.io, Gemfury) — is tracked as the suite of the directory that
+holds its `InRelease` or `Release`. The `./` form keeps its literal `/.`
+and is a suite of its own, the directory's *twin*: `deb https://h/repo ./`
+and `deb https://h/repo/ /` are two suites, each with its own row and
+snapshot, that share the record of having been seen signed. With adoption
+enabled a flat repository's root `Packages` or `Sources` (plain or
+compressed) tracks its suite too, when its bytes are what they claim. A
+flat repository at the host root (`deb https://h/ ./`) is not supported:
+it is served from the ordinary cache and never refreshed, and the daemon
+logs WARN `flat_host_root_unsupported` (at most one line a minute).
+
+A signed or unsigned flat repository is adopted like any other (see
+*Anchor forms and unsigned repositories*). Its `Release` must list its
+root `Packages`, whose fetch failing fails the adoption, and the `.deb`
+files that `Packages` names are vouched by the paths apt requests for
+them: `<uri>/<Filename>`, with or without a leading `./`, for whichever
+directory at or above the `Release` the repository's `<uri>` is
+(pkg.jenkins.io's `binary/…` one level up, Sublime Text's `files/…` at the
+host root). A path of more than 32 segments is vouched only as the
+`Packages` file spells it.
+
+A flat repository that publishes no `Release` at all (Gemfury,
+goreleaser) is refreshed without one, as long as the cache has never seen
+it signed: each check sends one conditional request for every cached
+`Packages`, `Sources` and `Contents-*` file of the suite, with the
+validators upstream gave it. A changed file replaces the cached one when
+its bytes pass the same content check as on first contact; an HTML page
+or anything else that fails it keeps the old file (counted in
+`acu_anchorless_sniff_rejected_total{host}`), and a plain `Contents-*`,
+which has no such check, is deleted when it changed, for the next client
+request to fetch. A file upstream deletes (404, 410) is deleted, and so
+is a cached `InRelease` or `Release` that upstream now answers so;
+anything else — 401, 403, a 5xx, a timeout, a body over 64 MiB — keeps
+the file. Such a repository has no snapshot: it vouches nothing and
+leaves strict mode alone. Its index files are served with `Last-Modified`
+the later of the file's time and its last refresh, so apt's
+`If-Modified-Since` sees a replacement. The check logs `freshness_check`
+with `result=anchorless_refreshed` or `anchorless_unchanged`, and the
+repository is adopted at the first check after it starts publishing an
+`InRelease` or `Release`. A flat repository the cache has seen signed (a
+record of a signature for its path, a cached `InRelease` or `Release.gpg`
+that passes its check, or an adopted signed snapshot) is never refreshed
+this way: when its `InRelease` and `Release` disappear it fails its checks
+and shows "refused: anchor_absent", so stripping a signed repository's
+signatures cannot turn it into an unverified one.
+
 ## `[adoption]`: snapshots, trust, and recovery
 
 | Key | Type | Default | Behavior and constraints |
@@ -731,7 +784,8 @@ asked for least recently first; a suite being checked or adopted at
 that moment is skipped — it counts toward its kind's number — and tried
 again at the next run. Reaping a suite deletes its cached index files
 (its `InRelease`, `Release`, `Packages` and the rest of its `dists/`
-tree, in batches of 1,000 files), retires its snapshots and deletes its
+tree — a flat repository's root files, never its `./` twin's — in
+batches of 1,000 files), retires its snapshots and deletes its
 row (INFO `suite_reaped`; `acu_suites_reaped_total{reason}` counts the
 reaps). The reaper deletes no package, but a reaped suite's packages
 lose the version retention its snapshot gave them
@@ -794,7 +848,13 @@ every such file was fetched at least a day earlier, so a repository that
 answered with an error page for a few hours is kept, and so is one whose
 cached `InRelease` is a plain unsigned `Release` (the cache adopts such a
 repository through its `Release`); a file that cannot be read is never
-taken for an error page.
+taken for an error page. A flat repository with no cached `InRelease`,
+`Release` or `Release.gpg` (one without a `Release`: see *Flat
+repositories* under `[freshness]`) is judged by its cached root
+`Packages` and `Sources` files instead, by the same rules: it is reaped
+as junk when each of them is something else than its name says (an HTML
+page cached as `Packages.gz`, say) and was fetched at least a day
+earlier.
 
 A suite whose last request lies in the future (the clock was stepped
 back) is never idle — the two kinds above are reaped all the same — and
```

- [ ] **Step 2: Events and metrics (`docs/log-fields.md`)**

The metric, the anchorless results, the failed check's `anchorless` form, the three WARN rows (two anchorless, `flat_host_root_unsupported`), the reaper's new WARN, `suite_reaped`'s flat junk, and `package_coverage_incomplete`'s `non_dists_layout` (now no tracked path) and `no_packages_members` (a flat suite's root):

```diff
diff --git a/docs/log-fields.md b/docs/log-fields.md
--- a/docs/log-fields.md
+++ b/docs/log-fields.md
@@ -214,7 +214,11 @@ per-pass count the `gc_run_complete` line carries as `retired_reaped`
 leaves the retired snapshots to the periodic passes, normally the first
 one after the start; it deletes only a retired one never adopted whose
 heartbeat went stale, as it does any abandoned adoption candidate,
-counted in `orphan_candidates_reaped`).
+counted in `orphan_candidates_reaped`). Flat repositories (spec C4) add
+the counter `acu_anchorless_sniff_rejected_total{host}`: an anchorless
+refresh's 200 whose body failed the flat index check for the file's name
+(an HTML page, say), so the cached file stays (see the `anchorless_*`
+results below).
 
 The `freshness_check` line's `result` gains `changed_deferred` (upstream
 still serves content whose adoption failed or was refused; the field
@@ -245,6 +249,32 @@ a failure or refusal record paces: those of a suite without a snapshot
 whose record is not due yet, within `periodic_refresh` of its last check,
 and those of an adopted suite in `anchor_absent` before its retry time.
 
+Flat repositories (spec C4 Dispatch 3–4) add `anchorless_refreshed` and
+`anchorless_unchanged`, with `form` `anchorless`: a flat suite without a
+snapshot whose upstream serves neither `InRelease` nor `Release`, and that
+the cache has never seen signed (no signed-seen record for its path, no
+cached `InRelease` or `Release.gpg` of it or its `./` twin that passes its
+check, under either scheme, and no adopted signed snapshot), revalidated
+its cached `Packages*`, `Sources*` and `Contents-*` files — one
+conditional GET each, with the validators stored with the file. A 304
+keeps a file; a 200 replaces it when its body passes the flat index check
+for the file's name and keeps it otherwise (counted in
+`acu_anchorless_sniff_rejected_total{host}`), except that a plain
+`Contents-*`, which has no such check, is deleted when its bytes changed
+(the next client request fetches it); 404 and 410 delete the file; 401
+and 403 keep it; any other end — a 5xx, a transport error, a body over
+64 MiB, the check's deadline — keeps it, unanswered. The suite's cached
+`InRelease` and `Release` that the check's probes found answering 404 or
+410 are deleted too. `anchorless_refreshed`: at least one file was
+replaced or deleted; `anchorless_unchanged`: none. `upstream_status` is
+the `Release` probe's absent answer. Such a check is a success: it clears
+the suite's failure or refusal record (an `anchor_absent` an earlier check
+left) and its pending change, and the suite is next due a
+`periodic_refresh` later. When every file's request ended unanswered the
+check fails instead (`freshness check failed`, `form` `anchorless`). A
+flat suite the cache has seen signed is the failed check and
+`anchor_absent` record it was.
+
 ## Stability guarantees
 
 - The `path_class` enum is closed. Adding a value requires a spec
@@ -269,7 +299,7 @@ daemon's `main`).
 |---|---|---|---|
 | `adoption_panicked` | ERROR | `where` (`adoption`, `repair`, `reconcile`, `heartbeat`, `record`), `canonical_scheme`, `canonical_host`, `suite_path` (the suite's scheme, host and path; the scheme tells its `http` copy from its `https` one), `panic`, `stack` (first occurrence per `where` per process only) | A panic inside an adoption, repair, reconcile, heartbeat or failure-record goroutine was recovered; the prior snapshot keeps serving. Only an adoption panic is recorded as a failure (reason `panic`); repair and reconcile panics are logged, not recorded (plan deviation 7). `where=record` is `recordAdoptionFailure`, which writes the spec C7 failure record after an adoption ends. |
 | `freshness_check_panicked` | ERROR | `where` (`check`, `scheduler`, `reschedule`, `t1`, `reaper`, `forget`), `canonical_scheme`, `canonical_host`, `suite_path` (the suite the panic struck — the scheme tells its `http` copy from its `https` one; all three empty when the panic names no suite), `panic`, `stack` (first occurrence per `where` per process only) | A panic during a freshness check was recovered. For `where=check` the check counts as failed (`last_check_at` bumped, so the cooldown applies) and also counts as an unknown end: `last_check_at` is bumped and `check_fail_count` extended. The `scheduler`, `reschedule` and `t1` recovers only log; `where=reschedule` guards the start-time retry spread (`rescheduleFailingAdoptions`), and `where=t1` is the handler's client-triggered check goroutine (logged by `internal/handler`). `where=reaper` is the suite reaper: a panic while it held a suite names that suite, the hold is released as the cache then answers (the row gone or not), and the run goes on to its next candidate and its sweeps; a panic elsewhere in a run (the listing, the junk scan, the sweeps; empty `canonical_scheme`, `canonical_host` and `suite_path`) ends the run, and the next run comes as scheduled. `where=forget` is `POST /forget`: a panic while it held the suite names that suite; the hold is released as the cache then answers (the row gone or not), and the panic goes on to net/http, which ends the request without an answer and logs `http: panic serving …` at ERROR (see net/http's own lines below). |
-| `freshness check failed` | INFO | `canonical_host`, `suite_path`, `form` (`inline`, `detached`, `unsigned`, or `probe` for the probe tree of a suite without a snapshot), `err`, `result` (always `failed`) | A check ended unknown — a transport error, an error status that is not an absent answer, a success status other than 200 or 304 (a fetch contract violation: `err` says `unexpected success status`), a 304 to a request that carried no validators (adoption enabled; `err` says `304 Not Modified to a request without validators`), or the 2-minute check deadline at the host slot or the request — or found nothing it may adopt: a suite without a snapshot whose upstream has neither `InRelease` nor `Release` (which also records the refusal state `anchor_absent`, shown on the status page); an unsigned `Release` while `adoption.adopt_unsigned` is off is the refusal `unsigned_disabled` instead. A check whose probe found a signature but could not record the suite as seen signed fails too (`err` starts `mark the signature seen:`). A shutdown that ends a check — its request, or a cache read or marker write after it — is not a failure: nothing is written or counted (DEBUG only). `last_check_at` is bumped and `check_fail_count` extended, so the scheduler backs off (2 min doubling to 1 h), and `acu_freshness_check_total{result="failed"}` is incremented. With adoption enabled, 401, 403, 404, 410 and a 200 whose body is not the file asked for are answers, not failures (the anchor is absent): they never extend the streak. |
+| `freshness check failed` | INFO | `canonical_host`, `suite_path`, `form` (`inline`, `detached`, `unsigned`, `probe` for the probe tree of a suite without a snapshot, or `anchorless` for a flat suite's anchorless refresh), `err`, `result` (always `failed`) | A check ended unknown — a transport error, an error status that is not an absent answer, a success status other than 200 or 304 (a fetch contract violation: `err` says `unexpected success status`), a 304 to a request that carried no validators (adoption enabled; `err` says `304 Not Modified to a request without validators`), or the 2-minute check deadline at the host slot or the request — or found nothing it may adopt: a suite without a snapshot whose upstream has neither `InRelease` nor `Release` (which also records the refusal state `anchor_absent`, shown on the status page) — a flat suite the cache has never seen signed refreshes its cached indexes instead (the `anchorless_*` results); an unsigned `Release` while `adoption.adopt_unsigned` is off is the refusal `unsigned_disabled` instead. An anchorless refresh fails when every request it sent ended unanswered (`err` starts `every anchorless index request ended unknown:`) or when its check for signed evidence could not read the cache (`err` starts `re-mark the cached signed anchors:`, or names the read). A check whose probe found a signature but could not record the suite as seen signed fails too (`err` starts `mark the signature seen:`). A shutdown that ends a check — its request, or a cache read or marker write after it — is not a failure: nothing is written or counted (DEBUG only). `last_check_at` is bumped and `check_fail_count` extended, so the scheduler backs off (2 min doubling to 1 h), and `acu_freshness_check_total{result="failed"}` is incremented. With adoption enabled, 401, 403, 404, 410 and a 200 whose body is not the file asked for are answers, not failures (the anchor is absent): they never extend the streak. |
 | `adoption_run_failed` | WARN; INFO when the attempt was refused (outcome `refused`) | `canonical_host`, `suite_path`, `form`, `err` | Backstop line for every adoption attempt that returned an error, so a failure with no categorised line of its own is still visible. A refusal is a state, not a failure, so its line is INFO (`adoption_refused` carries the reason and timing); operators alerting on WARN are not paged for a flapping mirror. |
 | `adoption: read prior snapshot failed` | WARN | `canonical_host`, `suite_path`, `err` | The adoption could not read the suite's current (prior) snapshot before running Date and rollback checks against it; the attempt fails as `db_failed` and is retried on the normal backoff schedule rather than skipping those checks. |
 | `adoption: signed-anchor re-mark failed` | WARN; ERROR when a cached anchor's pool file could not be read (a missing file is not an error) | `canonical_host`, `suite_path`, `err` | Before an unsigned adoption's commit, the adopter re-reads the suite's cached `InRelease` and `Release.gpg` (and those of its `/.` twin) to record any signed one (spec C5 safeguard 3), and could not read a `url_path` row or a pool file, or write the record. The adoption is refused `unsigned_downgrade_refused` (fail closed): here that refusal means the cache could not rule out a signature, not that it saw one. It is retried on the refusal's schedule. |
@@ -302,16 +332,19 @@ daemon's `main`).
 | `displaced snapshot lookup failed` | WARN | `err`, `canonical_host`, `path` | Retained by-hash could not read the suite's displaced snapshots; the request is answered 404, as before spec C8. |
 | `snapshot adopted_at lookup failed` | WARN | `err`, `snapshot_id` | A snapshot `InRelease`/`Release`/`Release.gpg` could not read its snapshot's `adopted_at` (spec C8 `Last-Modified`), so this response carries the pool file's mtime instead and its `Last-Modified` may not grow with the last flip. Not logged when the request had already ended. |
 | `shutdown_waiting_for_adoptions` | INFO | `waited`, `detail` | Shutdown has waited 2 s for the adoption, repair and reconcile goroutines: a commit already running is never cancelled and may take up to its 5-minute bound; other work stops at its next cancellation point (adoptions in the flip barrier were abandoned at the signal). Logged once per shutdown. |
+| `freshness: anchorless index over its size cap` | WARN | `canonical_host`, `suite_path`, `max_bytes` | An anchorless refresh (spec C4 Dispatch 4) got a 200 over 64 MiB (`max_bytes`) for at least one of the suite's cached flat indexes: each such file stays as it was, unanswered. At most one line per check. |
+| `freshness: delete anchorless rows failed` | WARN | `err`, `canonical_host`, `suite_path` | An anchorless refresh could not delete the cached files upstream answered 404 or 410 (or a plain `Contents-*` whose bytes changed); they stay until a later check deletes them. The check is answered all the same. |
+| `flat_host_root_unsupported` | WARN | `canonical_host`, `path`, `suppressed`, `detail` | A client asked for a flat repository's file at the host root (`deb https://host/ ./` or `deb https://host/ /`: `/InRelease`, `/./Packages.gz`, `/by-hash/SHA256/…`; spec C2). Such a repository is no suite: it is served from the ordinary cache (`detail` says so) and never refreshed, adopted or vouched. At most one line a minute for the whole process; `suppressed` counts the requests since the last line. |
 | `suite_seed_rejected` | WARN | `canonical_host`, `suite_path`, `cap` (`global_cap`, `host_cap`), `max_suites`, `suppressed`, `detail` | A suite was refused a row by `freshness.max_suites`: it is served but will not refresh; retried after 10 minutes (`detail` = `suite will not refresh`). At the total cap while every suite that could have been evicted was busy — being checked or adopted, or evicted for another new suite — the refusal is transient: `detail` = `eviction candidates busy; retried after 1 minute`, and the suite is retried after a minute. At most one line a minute; `suppressed` counts the refusals since the last line. |
 | `suite_evicted` | INFO | `canonical_host`, `suite_path` (the evicted suite), `for_host`, `for_suite` | At or above the global cap, a suite with no adopted snapshot that had not been requested for 24 hours was evicted for a new one; its signed-seen records survive, moved up to the time of the eviction (as for `suite_reaped`). Above the cap (after `freshness.max_suites` was lowered, or after an upgrade) the new suite is still refused (`suite_seed_rejected`) until the table is back under the cap. |
 | `suite_caps_exceeded` | WARN | `suites`, `max_suites`, `hosts_at_cap`, `per_host_cap`, `fullest_host`, `fullest_host_suites` | At start, the suite table holds more than `freshness.max_suites` suites (a lowered `max_suites`, or an upgraded cache), or a host holds a tenth of it or more: new suites — for a host at its cap, that host's — are refused. Above the total cap each refused attempt still evicts one suite idle for 24 hours, so the table shrinks back; a host at its cap evicts nothing for its own new suites; its suites go when the suite reaper removes those not requested for `gc.suite_idle_ttl`, or when total-cap eviction for any host's new suite picks one of its idle suites. Not logged for a table at exactly the total cap: that is normal operation for a full cache, where a new suite evicts an idle one (no adopted snapshot, not requested for 24 hours) and is refused only when none qualifies (`suite_seed_rejected`). Logged once per start. |
 | `suite cap usage read failed` | WARN | `err` | The startup check behind `suite_caps_exceeded` could not count the suites; the caps apply either way. |
-| `suite_reaped` | INFO | `canonical_scheme`, `canonical_host`, `suite_path`, `reason` (`idle`, `non_canonical`, `junk`), `metadata_rows`, `settled` (present, `true`, only for a settled reap) | The suite reaper (spec C9; hourly, from an hour after the start; a run tries at most 250 `non_canonical` and `junk` suites, first, and at most 25 `idle` ones) deleted a suite: its metadata `url_path` rows (`metadata_rows`, in batches of 1,000, the anchors last), then, in one transaction that re-checks that the suite was not adopted — nor, for `idle`, asked for — since the run listed it, its snapshots (retired: snapshot GC deletes them) and its row. `idle`: no client asked for it in `gc.suite_idle_ttl`; `non_canonical`: a row an earlier release created for a path this version no longer tracks (`metadata_rows` is 0: such a path's cached files are left to `gc.url_path_ttl`); `junk`: a suite without a snapshot whose cached anchor files (`InRelease`, `Release`, `Release.gpg`) all fail their content check — an HTML page, say; a plain `Release` cached as `InRelease` passes — each fetched at least a day ago, whatever its adoption record says. Its signed-seen records survive, moved up to the time of the reap (a later stamp is kept): every suite-row delete — a reap, a cap eviction, a forget that leaves the records — does so, so a record's year (`signed_seen_markers_swept`) counts from the end of its path's tracking; and a suite with an adopted signed snapshot (`inline` or `detached`, not retired) gets its path's record under its own scheme before its snapshots are retired — created if missing, as for a snapshot the upgrade's trust audit could not read yet — so that history survives too. `settled=true`: the final transaction committed but the writer call reported an error, and a fresh read found the row gone — it is a reap all the same. Counted in `acu_suites_reaped_total{reason}`, settled reaps included. |
+| `suite_reaped` | INFO | `canonical_scheme`, `canonical_host`, `suite_path`, `reason` (`idle`, `non_canonical`, `junk`), `metadata_rows`, `settled` (present, `true`, only for a settled reap) | The suite reaper (spec C9; hourly, from an hour after the start; a run tries at most 250 `non_canonical` and `junk` suites, first, and at most 25 `idle` ones) deleted a suite: its metadata `url_path` rows (`metadata_rows`, in batches of 1,000, the anchors last), then, in one transaction that re-checks that the suite was not adopted — nor, for `idle`, asked for — since the run listed it, its snapshots (retired: snapshot GC deletes them) and its row. `idle`: no client asked for it in `gc.suite_idle_ttl`; `non_canonical`: a row an earlier release created for a path this version no longer tracks (`metadata_rows` is 0: such a path's cached files are left to `gc.url_path_ttl`); `junk`: a suite without a snapshot whose cached anchor files (`InRelease`, `Release`, `Release.gpg`) all fail their content check — an HTML page, say; a plain `Release` cached as `InRelease` passes — each fetched at least a day ago, whatever its adoption record says — for a flat suite with no cached anchor file, its cached root `Packages*` and `Sources*` files are judged so instead (an HTML page cached as `Packages.gz`, say). Its signed-seen records survive, moved up to the time of the reap (a later stamp is kept): every suite-row delete — a reap, a cap eviction, a forget that leaves the records — does so, so a record's year (`signed_seen_markers_swept`) counts from the end of its path's tracking; and a suite with an adopted signed snapshot (`inline` or `detached`, not retired) gets its path's record under its own scheme before its snapshots are retired — created if missing, as for a snapshot the upgrade's trust audit could not read yet — so that history survives too. `settled=true`: the final transaction committed but the writer call reported an error, and a fresh read found the row gone — it is a reap all the same. Counted in `acu_suites_reaped_total{reason}`, settled reaps included. |
 | `admin_forget_requested` | INFO | `scheme`, `canonical_host`, `suite_path` | `POST /forget` accepted a request (logged by `internal/admin`): it passed the header checks (`X-ACU-Admin: 1`, `Sec-Fetch-Site`), the 8 KiB body limit and the parameter checks, and the forget starts. A request refused with 400, 403 or 413 is not logged. `scheme` is the suite's canonical scheme (the other lines about a suite name it `canonical_scheme`). The outcome follows: `suite_forgotten`, `admin: forget failed`, no line for a busy or unknown suite (409), or, after a panic while the suite is held, `freshness_check_panicked` with `where=forget` (a panic before the hold leaves only net/http's ERROR line). |
 | `suite_forgotten` | INFO | `canonical_scheme`, `canonical_host`, `suite_path`, `metadata_rows`, `markers_deleted`, `anchor_rows`; `settled` (present, `true`, only for a settled forget, whose line carries `metadata_rows` but not `markers_deleted` or `anchor_rows`) | `POST /forget` deleted a suite as the reaper would, regardless of idleness, and — only when no other suite of the repository path, under either scheme or as its `/.` twin, has a row — `markers_deleted` signed-seen records of the path and `anchor_rows` cached `InRelease`, `Release` and `Release.gpg` of the path and its twin under either scheme. Its request is logged first as `admin_forget_requested`. `settled=true`: the final transaction committed but the writer call reported an error, and a fresh read found the row gone — a forget all the same (the records and cached anchors deleted as above when no other suite of the path has a row; how many is not known); the request is then answered 500 with WARN `admin: forget failed`, or 503 with that line at DEBUG when the admin listener's shutdown cut it short. |
 | `admin: forget failed` | WARN | `err`, `scheme`, `canonical_host`, `suite_path` | `POST /forget` failed on a database error, or its work took longer than 5 minutes, and answered 500. Metadata batches deleted before the error stay deleted (the anchors go last); the request can be repeated (409 then means the suite is gone after all, or busy). A forget whose final delete committed before the error is logged `suite_forgotten` with `settled=true` just before this line when a fresh read then finds the row gone (a read that fails, or lands before a commit still in flight, finds it present: then no such line). Logged at DEBUG instead, and answered 503, when the admin listener began shutting down while the forget ran: shutdown cuts it short. |
 | `signed_seen_markers_swept` | INFO | `count` | The suite reaper's daily sweep (the first a day after the start, then a day apart) deleted `count` signed-seen records whose repository path has no suite row (for it or its `/.` twin, under either scheme) and that were last seen — or a suite of their path last deleted: reaped, evicted at the cap, or forgotten while another suite of the path kept the record — more than 365 days ago, by the clock less the forward steps the reaper detected while running (`clock_step_detected` with `stamp=wall_clock`), so such a step never ages a record. Not logged when it deleted none. |
-| `suite reaper: list suites failed`, `suite reaper: read suite row failed`, `suite reaper: delete metadata failed`, `suite reaper: reap failed`, `suite reaper: list unadopted anchors failed`, `suite reaper: junk anchor reads failed`, `suite reaper: marker sweep failed` | WARN | `err`; `canonical_scheme`, `canonical_host` and `suite_path` for a suite's step; `metadata_rows` (for `delete metadata failed`: rows deleted before the error); `count`, `canonical_scheme`, `canonical_host` and `path` for the junk anchor reads (how many anchor reads failed in the run's junk scan — a failed read is not remembered, so one unreadable pool file that N suites share counts once for each of them whose scan reaches it, up to N times — and the first: its suite's scheme and host and the anchor's path); `swept` (records deleted before the error) | A step of a suite reaper run failed. A failed listing ends the run: nothing is reaped and nothing swept (a due record sweep runs at the next run); any other failed step leaves the rest of the run to go on: a suite whose step failed — a database error, or its work cut short after 5 minutes — keeps its row and is retried at the next run (the metadata batches deleted before the error stay deleted, the anchors last); a pool file that exists but does not read keeps its suite (a missing one is not logged); the junk scan runs again at the next run, the record sweep the next day. A step that fails because the daemon is stopping is logged at DEBUG instead. |
+| `suite reaper: list suites failed`, `suite reaper: read suite row failed`, `suite reaper: delete metadata failed`, `suite reaper: reap failed`, `suite reaper: list unadopted anchors failed`, `suite reaper: list unadopted flat indexes failed`, `suite reaper: junk anchor reads failed`, `suite reaper: marker sweep failed` | WARN | `err`; `canonical_scheme`, `canonical_host` and `suite_path` for a suite's step; `metadata_rows` (for `delete metadata failed`: rows deleted before the error); `count`, `canonical_scheme`, `canonical_host` and `path` for the junk anchor reads (how many anchor reads failed in the run's junk scan — a failed read is not remembered, so one unreadable pool file that N suites share counts once for each of them whose scan reaches it, up to N times — and the first: its suite's scheme and host and the anchor's path); `swept` (records deleted before the error) | A step of a suite reaper run failed. A failed listing ends the run: nothing is reaped and nothing swept (a due record sweep runs at the next run) — except the flat indexes' (read only in a run that holds a flat suite without a snapshot or a cached anchor file), which leaves such suites unjudged as junk for the run; any other failed step leaves the rest of the run to go on: a suite whose step failed — a database error, or its work cut short after 5 minutes — keeps its row and is retried at the next run (the metadata batches deleted before the error stay deleted, the anchors last); a pool file that exists but does not read keeps its suite (a missing one is not logged); the junk scan runs again at the next run, the record sweep the next day. A step that fails because the daemon is stopping is logged at DEBUG instead. |
 | `suite reaper: suite busy, retried next run` | DEBUG | `canonical_scheme`, `canonical_host`, `suite_path` | A suite the reaper would reap was held — by a check, an adoption, a repair or reconcile, or another deleter (cap eviction, `POST /forget`) — or deleted meanwhile; it is tried again at the next run if it still qualifies, and counts toward the run's budget for its kind (25 `idle` suites; 250 `non_canonical` and `junk` ones). |
 | `handler_goroutine_panicked` | ERROR | `where` (`touch`, `seed_rejections`), `canonical_scheme`, `canonical_host` and `path` (`touch` only), `panic`, `stack` (first occurrence per `where` per process only) | A panic in one of the handler's background goroutines — the touch goroutine, or the cap-rejection cache's sweep timer — was recovered (a bug, please report it). |
 | net/http's own lines: `http: panic serving …`, `http: TLS handshake error from …` and the like | ERROR on the admin listener; WARN on the proxy listeners | none: `msg` is net/http's text — for a panic, `http: panic serving <client address>: <panic>` followed by the stack | The Go HTTP server's own diagnostics, logged through the daemon's structured log at the level each listener sets (left to itself, net/http would write them through Go's standard log package, which the daemon also sends to its log, but at INFO). On the admin listener every such line is ERROR, above all a handler panic net/http recovered: after a `POST /forget` panic, which is logged first as `freshness_check_panicked` with `where=forget`, the request ends without an answer and gets no `admin_request` line. On the proxy listeners (plain and TLS) they are WARN, a TLS handshake error included. |
@@ -322,7 +355,7 @@ daemon's `main`).
 | `suite signed-seen backstop read failed` | WARN | `err`, `canonical_host`, `suite_path`, `path` | After creating a suite's row, the handler's second read of a cached signed anchor (see `suite cached anchor read failed`; also run when the anchor changed during the creation) failed — its `url_path` row (within the backstop's 5 s bound, which covers its database steps; after onboarding the bound is shared with the creation, and on a miss it ends at most 25 s after the request arrived) or its pool file (a local read of a few KiB, not bounded); an absent row is not logged. The response is served; the anchor stays unmarked until the startup pass marks it at the next start. |
 | `snapshot_member_blob_missing` | WARN | `canonical_host`, `path`, `snapshot_id`, `blob_hash`, `snapshot_displaced` (present for a displaced snapshot's member) | A snapshot member's pool file is gone: a current member is re-fetched and checked against its declared hash; a displaced one (retained by-hash) answers 404. |
 | `pool_corruption_during_adoption` | WARN | a member that declares a SHA256: `canonical_host`, `suite_path`, `path`, `declared_sha256`, `actual_sha256`; a member the Release declares by SHA512 only: `canonical_host`, `suite_path`, `path`, `declared_sha512`, `derived_sha256` (the SHA-256 of the fetched bytes, verified against `declared_sha512`), `actual_sha256`; an anchor or index the adoption stores from memory: `hash`, `actual_sha256`, `context` (`writeBlobBytes`) | An adoption was about to reuse a pool file whose bytes no longer hash to its name (corrupted at rest). The file is evicted and replaced by freshly verified bytes; it is never bound to the snapshot, and the `.deb` hashes it declares never vouch. A SHA512-only member is fetched at every adoption, and an existing pool file under its derived SHA-256 is re-hashed before it is kept. Counted by `acu_pool_corruption_during_adoption_total`. |
-| `package_coverage_incomplete` | INFO | `canonical_host`, `suite_path`, `snapshot_id`, `reason`; `directories` (`unsupported_variants`), `no_sha256_stanzas` (`no_sha256`), `skipped_rows` (`all_binary_stanzas_malformed`) | A signed snapshot's package coverage could not be proven complete (SPEC3 §7.5.4), so strict mode (`integrity.refuse_unvouched_debs`) stays off for its host. `reason`: `non_dists_layout` (the suite path is not `dists/`-shaped), `no_packages_members` (its Release lists no `Packages` index), `unsupported_variants` (a directory with a `Packages` index has no variant that was fetched and parsed; `directories` names them), `no_sha256` (spec C5: a `Packages` stanza with a `Filename` but no `SHA256`, for example one that declares only `SHA512` or `MD5sum`; it vouches nothing, and `no_sha256_stanzas` counts the distinct paths), `all_binary_stanzas_malformed` (every binary stanza lacks `Package`, `Architecture` or `Version`). A reconcile that heals `Packages` indexes recomputes the rows for them and may log it too; a reconcile never changes a snapshot's coverage. |
+| `package_coverage_incomplete` | INFO | `canonical_host`, `suite_path`, `snapshot_id`, `reason`; `directories` (`unsupported_variants`), `no_sha256_stanzas` (`no_sha256`), `skipped_rows` (`all_binary_stanzas_malformed`) | A signed snapshot's package coverage could not be proven complete (SPEC3 §7.5.4), so strict mode (`integrity.refuse_unvouched_debs`) stays off for its host. `reason`: `non_dists_layout` (the suite path is neither `dists/`-shaped nor a flat repository's: no path the cache tracks since flat repositories are adopted), `no_packages_members` (its Release lists no `Packages` index; for a flat suite, no root one), `unsupported_variants` (a directory with a `Packages` index has no variant that was fetched and parsed; `directories` names them), `no_sha256` (spec C5: a `Packages` stanza with a `Filename` but no `SHA256`, for example one that declares only `SHA512` or `MD5sum`; it vouches nothing, and `no_sha256_stanzas` counts the distinct paths), `all_binary_stanzas_malformed` (every binary stanza lacks `Package`, `Architecture` or `Version`). A reconcile that heals `Packages` indexes recomputes the rows for them and may log it too; a reconcile never changes a snapshot's coverage. |
 | `package_hash lookup failed` | WARN | `err`, `canonical_host`, `path` | On a `.deb` cache hit the vouching hashes (`package_hash`) could not be read: the cached file is served without the hash check, and strict mode (`integrity.refuse_unvouched_debs`) fails open for it, so a database fault does not refuse every package. A request whose client left is answered 503 (`client_canceled`) instead, never served, and logs nothing here. |
 | `strict-mode coverage lookup failed` | WARN | `err`, `canonical_host` | Strict mode could not read its host's current snapshots and their coverage: it fails open for the request (the `.deb` is served or fetched as if strict mode were off). A request whose client left is answered 503 (`client_canceled`) on the hit path and logs nothing here. |
 | `package_hash validate lookup failed` | WARN | `err`, `canonical_host`, `path` | After a `.deb` download the vouching hashes could not be read: the body is kept and served without validation (fail open, as on the hit path). |
```

- [ ] **Step 3: `suites[].flat` (`docs/admin-ui-spec.md` §4.5) and the conffile**

```diff
diff --git a/docs/admin-ui-spec.md b/docs/admin-ui-spec.md
--- a/docs/admin-ui-spec.md
+++ b/docs/admin-ui-spec.md
@@ -348,6 +348,7 @@ next to the Suites table contract in §5.3:
 | Field | Type | Meaning |
 |---|---|---|
 | `scheme` | string | The suite's canonical scheme, `http` or `https` (phase F, ruling PF-7): a repository can be tracked under both, as two rows with the same `host` and `suite_path`, and `POST /forget` (`docs/configuration.md`, *Forgetting a suite*) takes it, defaulting to `https`. The Suites table's Host column shows it as `scheme://host`. |
+| `flat` | boolean | `true` for a flat repository's suite — a suite path with no `dists` segment, its `./` twin included (spec C2) — and `false` for a `dists/` suite (phase G). The Suites table's Suite path cell shows a neutral `flat` badge after the path. |
 | `anchor_form` | string | The current snapshot's anchor form — `inline`, `detached` or `unsigned` (a `Release` no signature covers: it vouches no `.deb`); empty when the suite has no current snapshot. |
 | `last_requested_unixtime` | int64 or `null` | When a client last showed interest in the suite (spec C3): written when the suite's row is created, then by coalesced touches — at most one write per suite per hour — from serves of its anchors (`InRelease`, `Release`, `Release.gpg`; status below 400, 304 and HEAD included), of its flat indexes, and of `.deb`s its current snapshot vouched for. Index requests such as `Packages.gz` never move it. It decides cap eviction: at the `freshness.max_suites` cap, a suite with no current snapshot whose stamp is 24 hours old or more can be evicted for a new one (`suite_evicted`). |
 | `check_fail_count` | int64 | Consecutive failed checks since the last answered check: ended unknown (transport error, deadline, unexpected status), or found nothing they may adopt (a suite without a snapshot whose upstream has neither `InRelease` nor `Release`, which also records `anchor_absent`; a signature the check found but could not record as seen). An unsigned `Release` while `adoption.adopt_unsigned` is off is the refusal `unsigned_disabled`, not a failed check. Drives the scheduler's healthy → never-checked → failing ordering and its 2 min → 1 h backoff. Resets to 0 on every answered check, an absent current anchor (`anchor_absent`) included. |
@@ -360,7 +361,8 @@ next to the Suites table contract in §5.3:
 These are additive to the locked JSON contract (§0.1), the same way
 `keyring[]` was: no existing key changes shape or meaning (`scheme` too:
 the HTML Host cell gains the `scheme://` prefix, the JSON `host` is
-unchanged).
+unchanged; `flat` too: the HTML Suite path cell gains its badge, the JSON
+`suite_path` is unchanged).
 
 **Adoption column.** The Suites table's "Adoption" column renders the
 record above as one line, via the server-side `adoptionStatus` helper:
```

```diff
diff --git a/packaging/config/config.toml.default b/packaging/config/config.toml.default
--- a/packaging/config/config.toml.default
+++ b/packaging/config/config.toml.default
@@ -131,7 +131,8 @@ adopt_unsigned        = true
 validate_at_rest_interval = "24h"
 validate_at_rest_workers  = 4     # >= 1 with scanning on; >= 0 otherwise
 # Refuse unvouched .debs with 502 only when adoption is enabled and every
-# current snapshot on the host has complete package coverage.
+# signed current snapshot on the host (a flat repository's included) has
+# complete package coverage.
 refuse_unvouched_debs     = false
 
 [hot_packages]
```

- [ ] **Step 4: Spec revision 10**

The revision line records the rulings and choices; C2 states the WARN's rate limit; C4 Dispatch 3 states the gate (deviation 1) and Dispatch 4 the refresh as built (deviation 2, the cap, the monotonic stamp, the results, the record, R-G3); C5 the only-dots Filename and R-G1; C9 R-G2 and R-G5; C11 the metric:

```diff
diff --git a/docs/superpowers/specs/2026-09-27-flat-repositories-design.md b/docs/superpowers/specs/2026-09-27-flat-repositories-design.md
--- a/docs/superpowers/specs/2026-09-27-flat-repositories-design.md
+++ b/docs/superpowers/specs/2026-09-27-flat-repositories-design.md
@@ -1,6 +1,6 @@
 # Flat repositories, eager adoption, and suite lifecycle — design
 
-- **Date:** 2026-09-27 (revision 9, 2026-10-20: phase F plan rulings — the reaper and `POST /forget` delete a suite row only through phase D's compare-and-delete, holding the suite through the `SuiteGuard`, so one delete path remains (R-F1); `Gate.Purge` keeps a live entry (R-F2); the reaper also reaps legacy junk rows (R-F3); the orphan-marker sweep reads a future `last_seen_at` as now, and a marker is an orphan only when neither its base nor its `/.` twin has a row under either scheme, the rule `/forget`'s marker deletion follows too (R-F5); after the phase F plan reviews (rulings PF-1–PF-9), task reviews (rulings PF-10, PF-11) and the final review (rulings PF-12–PF-15): the sweep never rewrites a stamp (PF-1, phase F deviation 3); `/forget` deletes the cached anchors of the base and its twin under both schemes with the markers (PF-2); junk is a cached anchor that fails its sniff, an `InRelease` passing the `Release` sniff (a plain-text Release) excepted, whatever the suite's record, fetched at least 24 h ago (PF-3); the junk scan runs every run and the candidates reaped regardless of idleness go first (PF-4); with `adopt_unsigned` off an established unsigned suite keeps its coherent snapshot — R-F4's retirement into the ordinary cache is dropped (PF-5); the `/forget` guard stops a cross-site form or fetch, not DNS rebinding, and Host-header validation joins G-1 (PF-6); `/forget`'s answers say what happened and the status page shows each suite's scheme (PF-7); a forward step of the wall clock defers reaping and the sweep by the uptime floor, a suite's held work is bounded at 5 minutes, a reap stamps its base's markers, reaps are counted by reason (PF-8); every suite-row delete — cap eviction, a reap, a forget — moves the base's markers up to `MAX(last_seen_at, now)` in its transaction, so an orphan's year counts from the end of its path's tracking (PF-10, widening PF-8's reap stamp); a hand edit that adds one field to one log line touches that function, not the whole file, for the deferred-minor rule (PF-11); the marker sweep runs once a day; `0s` disables reaping, not the reaper's other duties; the idle cutoff is strict and a NULL or future stamp is never idle; `/forget` takes an optional `scheme`, runs to completion on a context a client cannot cancel, and answers 400, 403 and 500 besides 202 and 409; sub-job C deletes one retired snapshot per transaction without a grace; a re-adoption's `adopted_at` stays above every snapshot of its suite, retired ones included; the 90-day simulation runs the reaper every 6 h and restarts every fifth day, and a four-day simulation runs the production cadence (phase F deviation 2); after the phase F task reviews: a non-canonical suite key deletes no metadata; the sweep judges a future marker against a fresh clock read while its cutoff stays the run's, and the reaper's two clock-step reports are logged directly, outside the one-line-a-minute limit; a panic under a suite's hold is recovered for that suite alone, and a delete whose writer call failed after its commit is a reap; `/forget` refuses a `host` holding a scheme (400), and a shutdown of the admin listener cuts it short (503); after the phase F final review: before it retires a suite's snapshots, every suite-row delete turns never-downgrade's snapshot clause into a marker — when the suite has an adopted, non-retired inline or detached snapshot it upserts its base's marker under the suite's scheme, the one marker a delete creates (PF-12); a forward step the reaper detects never ages a marker (the sweep's cutoff takes off every step detected since the reaper started), and the first sweep is due a day after the reaper starts, so a start with the clock ahead has a day to be corrected (PF-13); the classes reaped regardless of idleness have their own budget of 250 a run, tried first, beside the 25 idle suites (PF-14); the startup GC pass runs no sub-job C (PF-15); a reaped suite's packages lose the version retention its snapshot gave them; a `/forget` panic is logged with its suite (`where=forget`), and a forget whose writer call failed after its commit is logged (`suite_forgotten`, `settled=true`) when a fresh read finds the row gone; revision 8, 2026-10-19: phase E plan rulings — before an unsigned commit the adopter re-marks the suite's cached signed anchors and refuses when it cannot; a refusal inside the commit is a refusal state with its own sentinel and the barrier outcome `refused`; one check tail, and an unexpected success status is a failed check; absent answers reset `check_fail_count`; `anchor_absent` is recorded at the first absent observation, and a future `adoption_fail_first_at` never confirms an unsigned transition; the probe tree's content-key gate, the confirmation details, the interest and client-created-row rules, a 304 only to a conditional request, SHA512 verification before the pool; strict mode waits for signed adoptions only; the scheduler bound's admission order; after the phase E plan reviews (rulings PE-1–PE-8): a probe that finds a signature marks the suite's base, and the re-mark also reads the `/.` twin's rows; never-downgrade reads the markers of both schemes and also refuses while an adopted, non-retired signed snapshot of the base exists; a deferred never-adopted suite and an adopted `anchor_absent` suite are paced by their record; a never-adopted suite whose tree finds no anchor also records `anchor_absent`, and so does a detached suite whose changed `Release` has no signature, unless a failed lateral attempt's record holds, which keeps its reason and paces the suite the same way; a cross-form request records no validators; the interest window is symmetric; the unsigned content key is domain-separated; the unsigned member cap is applied before a slot is taken; strict mode's wait, and the miss path's backstop, end at most 25 s after the request started; the probe bound counts client checks and lost offers; after the phase E task reviews (rulings PE-9–PE-11): the long-run retry simulation names no day count; a suite whose anchor answered absent behind a failed attempt's record is paced by that record; a detached suite's lateral `InRelease` is held only by its own last failure (an in-memory memo) or by the record of its own content, so the detached form's failures never postpone it and a new `InRelease` is attempted at the first check that sees it; a `Packages` stanza without a SHA256 makes coverage incomplete (`no_sha256`) and counts toward the stanza cap; a hit-path client that left during the strict wait is answered `client_canceled`; revision 7, 2026-10-18: phase D plan rulings — adoption accepts exactly apt's first `InRelease` line, and a marker line with trailing blanks decodes; a binary `Release.gpg` passes its sniff only with a signature version (3–6) where its packet header puts the body; suite paths over 1024 bytes are not tracked; cap eviction holds the evictee's mutex through the checker, tries at most eight candidates, evicts at most one suite per creation attempt at or above the global cap (an attempt above it is still refused) and deletes through the compare-and-delete the reaper reuses, and a startup WARN names a table over its caps; the rejection cache holds at most 4,096 suites; `suite_seed_rejected` is rate-limited to one line a minute; the C3 metric names; the hit-path invariant covers tracked suites, and every row creation marks the suite's cached signed anchors in its own transaction; with no adopter a refresh miss re-stamps its row as before; the T1 on a creating miss lands with phase E; an attempt a shutdown ends before the barrier is abandoned like one in it; revision 6, 2026-10-18: phase C plan rulings — the C8 barrier keeps the adoption's global slot and does not honour `adoption.max_duration` but ends on shutdown (the daemon closes it at the signal), so the admission bound's *A* includes it; a hold that held no request starts no cross-suite quiet period; `flip_quiet_period` is `0s` or `1s` to `15s`; every C8 residual has its own counter; the hold-token residual and the five-suite DoD corrected (four hold, the fifth commits unheld); retained by-hash serves SHA512 aliases from phase C; exact C2 predicate and C3 `Release.gpg` sniff wording; after phase C's final review: once cross-suite quiet holds, a hold commits at 15 s (20 s less a 5 s commit margin) instead of answering its waiters from the old snapshot at 20 s — its late readers are a counted residual — and snapshot anchors carry `adopted_at` alone as `Last-Modified`; revision 5, 2026-10-17: slot-driven admission in C4 — a suite that finds no slot waits and is re-checked when one is released; the admission bound — after the phase B plan review; revision 4, 2026-10-05/11: legacy trust audit, signed-seen ordering, fail-closed inline verification, flip hold token, fair scheduler — after the 2026-10-05 plan review, a follow-up review, and a three-lens review of grounding, alternatives and residual risk; 2026-10-17: PR #8 parity notes in C10, §7 and §9)
+- **Date:** 2026-09-27 (revision 10, 2026-10-20: phase G plan rulings — a `Packages` file with no SHA256 stanza parses and adopts with incomplete coverage (`no_sha256`), and a body with no stanza naming a `Filename` still fails its parse (R-G1); a flat suite with no cached anchor row is judged junk by its cached root `Packages*`/`Sources*` rows under the anchors' rules, a suite with an anchor row by its anchors alone (R-G2); every writer of a `url_path` row's `last_fetched_at` stamps it after the pool file is final, so the first adoption's `adopted_at` floor stays above the `max(pool mtime, last_fetched_at)` `Last-Modified` of url_path-served metadata, and blob mtimes are not folded into that floor (R-G3); the anchorless gate is signed evidence — the cached signed-anchor re-mark, then never-downgrade's rule — not every sniff-passing anchor row (phase G deviation 1); a plain `Contents-*` that changed is deleted, not kept, in the anchorless refresh (phase G deviation 2); the anchorless body cap is 64 MiB; a replaced row's `last_fetched_at` is `MAX(now, previous + 1)`; an answered anchorless check (form `anchorless`) is a success that clears the record and the pending change, and one whose every index request ended unknown is a failed check; `flat_host_root_unsupported` is a WARN, one line a minute for the process with a `suppressed` count; a Filename made only of `.` segments is refused; the reaper reads flat index rows only in a run that has an anchorless flat suite (R-G5); `suites[].flat`; revision 9, 2026-10-20: phase F plan rulings — the reaper and `POST /forget` delete a suite row only through phase D's compare-and-delete, holding the suite through the `SuiteGuard`, so one delete path remains (R-F1); `Gate.Purge` keeps a live entry (R-F2); the reaper also reaps legacy junk rows (R-F3); the orphan-marker sweep reads a future `last_seen_at` as now, and a marker is an orphan only when neither its base nor its `/.` twin has a row under either scheme, the rule `/forget`'s marker deletion follows too (R-F5); after the phase F plan reviews (rulings PF-1–PF-9), task reviews (rulings PF-10, PF-11) and the final review (rulings PF-12–PF-15): the sweep never rewrites a stamp (PF-1, phase F deviation 3); `/forget` deletes the cached anchors of the base and its twin under both schemes with the markers (PF-2); junk is a cached anchor that fails its sniff, an `InRelease` passing the `Release` sniff (a plain-text Release) excepted, whatever the suite's record, fetched at least 24 h ago (PF-3); the junk scan runs every run and the candidates reaped regardless of idleness go first (PF-4); with `adopt_unsigned` off an established unsigned suite keeps its coherent snapshot — R-F4's retirement into the ordinary cache is dropped (PF-5); the `/forget` guard stops a cross-site form or fetch, not DNS rebinding, and Host-header validation joins G-1 (PF-6); `/forget`'s answers say what happened and the status page shows each suite's scheme (PF-7); a forward step of the wall clock defers reaping and the sweep by the uptime floor, a suite's held work is bounded at 5 minutes, a reap stamps its base's markers, reaps are counted by reason (PF-8); every suite-row delete — cap eviction, a reap, a forget — moves the base's markers up to `MAX(last_seen_at, now)` in its transaction, so an orphan's year counts from the end of its path's tracking (PF-10, widening PF-8's reap stamp); a hand edit that adds one field to one log line touches that function, not the whole file, for the deferred-minor rule (PF-11); the marker sweep runs once a day; `0s` disables reaping, not the reaper's other duties; the idle cutoff is strict and a NULL or future stamp is never idle; `/forget` takes an optional `scheme`, runs to completion on a context a client cannot cancel, and answers 400, 403 and 500 besides 202 and 409; sub-job C deletes one retired snapshot per transaction without a grace; a re-adoption's `adopted_at` stays above every snapshot of its suite, retired ones included; the 90-day simulation runs the reaper every 6 h and restarts every fifth day, and a four-day simulation runs the production cadence (phase F deviation 2); after the phase F task reviews: a non-canonical suite key deletes no metadata; the sweep judges a future marker against a fresh clock read while its cutoff stays the run's, and the reaper's two clock-step reports are logged directly, outside the one-line-a-minute limit; a panic under a suite's hold is recovered for that suite alone, and a delete whose writer call failed after its commit is a reap; `/forget` refuses a `host` holding a scheme (400), and a shutdown of the admin listener cuts it short (503); after the phase F final review: before it retires a suite's snapshots, every suite-row delete turns never-downgrade's snapshot clause into a marker — when the suite has an adopted, non-retired inline or detached snapshot it upserts its base's marker under the suite's scheme, the one marker a delete creates (PF-12); a forward step the reaper detects never ages a marker (the sweep's cutoff takes off every step detected since the reaper started), and the first sweep is due a day after the reaper starts, so a start with the clock ahead has a day to be corrected (PF-13); the classes reaped regardless of idleness have their own budget of 250 a run, tried first, beside the 25 idle suites (PF-14); the startup GC pass runs no sub-job C (PF-15); a reaped suite's packages lose the version retention its snapshot gave them; a `/forget` panic is logged with its suite (`where=forget`), and a forget whose writer call failed after its commit is logged (`suite_forgotten`, `settled=true`) when a fresh read finds the row gone; revision 8, 2026-10-19: phase E plan rulings — before an unsigned commit the adopter re-marks the suite's cached signed anchors and refuses when it cannot; a refusal inside the commit is a refusal state with its own sentinel and the barrier outcome `refused`; one check tail, and an unexpected success status is a failed check; absent answers reset `check_fail_count`; `anchor_absent` is recorded at the first absent observation, and a future `adoption_fail_first_at` never confirms an unsigned transition; the probe tree's content-key gate, the confirmation details, the interest and client-created-row rules, a 304 only to a conditional request, SHA512 verification before the pool; strict mode waits for signed adoptions only; the scheduler bound's admission order; after the phase E plan reviews (rulings PE-1–PE-8): a probe that finds a signature marks the suite's base, and the re-mark also reads the `/.` twin's rows; never-downgrade reads the markers of both schemes and also refuses while an adopted, non-retired signed snapshot of the base exists; a deferred never-adopted suite and an adopted `anchor_absent` suite are paced by their record; a never-adopted suite whose tree finds no anchor also records `anchor_absent`, and so does a detached suite whose changed `Release` has no signature, unless a failed lateral attempt's record holds, which keeps its reason and paces the suite the same way; a cross-form request records no validators; the interest window is symmetric; the unsigned content key is domain-separated; the unsigned member cap is applied before a slot is taken; strict mode's wait, and the miss path's backstop, end at most 25 s after the request started; the probe bound counts client checks and lost offers; after the phase E task reviews (rulings PE-9–PE-11): the long-run retry simulation names no day count; a suite whose anchor answered absent behind a failed attempt's record is paced by that record; a detached suite's lateral `InRelease` is held only by its own last failure (an in-memory memo) or by the record of its own content, so the detached form's failures never postpone it and a new `InRelease` is attempted at the first check that sees it; a `Packages` stanza without a SHA256 makes coverage incomplete (`no_sha256`) and counts toward the stanza cap; a hit-path client that left during the strict wait is answered `client_canceled`; revision 7, 2026-10-18: phase D plan rulings — adoption accepts exactly apt's first `InRelease` line, and a marker line with trailing blanks decodes; a binary `Release.gpg` passes its sniff only with a signature version (3–6) where its packet header puts the body; suite paths over 1024 bytes are not tracked; cap eviction holds the evictee's mutex through the checker, tries at most eight candidates, evicts at most one suite per creation attempt at or above the global cap (an attempt above it is still refused) and deletes through the compare-and-delete the reaper reuses, and a startup WARN names a table over its caps; the rejection cache holds at most 4,096 suites; `suite_seed_rejected` is rate-limited to one line a minute; the C3 metric names; the hit-path invariant covers tracked suites, and every row creation marks the suite's cached signed anchors in its own transaction; with no adopter a refresh miss re-stamps its row as before; the T1 on a creating miss lands with phase E; an attempt a shutdown ends before the barrier is abandoned like one in it; revision 6, 2026-10-18: phase C plan rulings — the C8 barrier keeps the adoption's global slot and does not honour `adoption.max_duration` but ends on shutdown (the daemon closes it at the signal), so the admission bound's *A* includes it; a hold that held no request starts no cross-suite quiet period; `flip_quiet_period` is `0s` or `1s` to `15s`; every C8 residual has its own counter; the hold-token residual and the five-suite DoD corrected (four hold, the fifth commits unheld); retained by-hash serves SHA512 aliases from phase C; exact C2 predicate and C3 `Release.gpg` sniff wording; after phase C's final review: once cross-suite quiet holds, a hold commits at 15 s (20 s less a 5 s commit margin) instead of answering its waiters from the old snapshot at 20 s — its late readers are a counted residual — and snapshot anchors carry `adopted_at` alone as `Last-Modified`; revision 5, 2026-10-17: slot-driven admission in C4 — a suite that finds no slot waits and is re-checked when one is released; the admission bound — after the phase B plan review; revision 4, 2026-10-05/11: legacy trust audit, signed-seen ordering, fail-closed inline verification, flip hold token, fair scheduler — after the 2026-10-05 plan review, a follow-up review, and a three-lens review of grounding, alternatives and residual risk; 2026-10-17: PR #8 parity notes in C10, §7 and §9)
 - **Status:** draft for maintainer review
 - **Branch:** `feature/flat-repos` (from `main` @ 536a1ab)
 - **Origin:** reimplementation of the problem reported in PR #8 ("Refresh flat
@@ -247,8 +247,10 @@ other paths `D/<member>` where `<member>` matches exactly one of
 the suite is the **literal** directory `D`. `D` must satisfy the predicate
 except that it may end in exactly one `/.` (the `deb <uri> ./` form: suite
 `/repo/.`, member `Packages.gz`). `D` must not be the host root (`/`, `/.`,
-empty): unsupported, logged by a globally rate-limited
-`flat_host_root_unsupported`.
+empty): unsupported, served through Phase 1, and logged by
+`flat_host_root_unsupported` — a WARN, at most one line a minute for the
+process, whose `suppressed` counts the requests since the last line
+(phase G, design note 2).
 
 Not covered, documented as served through Phase 1 only: flat Translation
 files (apt's flat key is `$(LANGUAGE)`: files named `en`, `en.xz`),
@@ -706,27 +708,46 @@ unchanged, observation only):
    - `InRelease` *present* → inline request;
    - `InRelease` *absent* → `Release`: *present* → `Release.gpg`: *present* →
      detached request; *absent* → unsigned candidate (C5);
-   - `Release` *absent* → for flat suites that have no signed-seen marker and
-     no sniff-passing cached anchor row, the **anchorless refresh**; otherwise
+   - `Release` *absent* → for flat suites with no signed evidence, the
+     **anchorless refresh**: the cached signed-anchor re-mark (C5: each
+     sniff-passing `InRelease` or `Release.gpg` row of the base or its `/.`
+     twin, under either scheme, marks the base) runs first, then
+     never-downgrade's rule (C5 safeguard 3) must find nothing; a cached
+     `Release` that passes its sniff is no signed evidence (phase G
+     deviation 1), and a read or mark error fails the check; otherwise
      a failed check, which extends `check_fail_count`: a suite with nothing
      to serve backs off like an *unknown* end (phase E, design note 19), and
      records the refusal state `anchor_absent`, so the status page shows it
      (ruling PE-2);
    - any *unknown* → failed check.
-4. **Anchorless refresh** (flat, Release-less suites only): conditional GET
-   (stored `upstream_etag`/`upstream_lastmod`) of each cached `url_path` row
-   whose `SuitePath` equals the suite and whose member is a
-   `Packages*`/`Sources*`/`Contents-*` file. A 200 replaces the row's blob
-   only if the body passes the flat index sniff for its extension (else the
-   old row stays; counter `anchorless_sniff_rejected`); 404/410 deletes the
-   row; 401/403/5xx keep it. A dedicated cache method preserves
-   `last_requested_at` and `request_count` and sets `last_fetched_at` only
-   when the blob changes. Cached anchor rows under the suite are deleted when
-   upstream answers them 404/410. No snapshot, no vouching, no strict-mode
-   effect. If a `Release`/`InRelease` later becomes *present*, the suite moves
-   to the normal forms. `url_path`-served metadata uses
-   `max(pool mtime, last_fetched_at)` as `Last-Modified`, so apt's
-   `If-Modified-Since` on `Packages*` sees a replacement.
+4. **Anchorless refresh** (flat, Release-less suites only), under the
+   check's host slot and deadline: conditional GET (stored
+   `upstream_etag`/`upstream_lastmod`) of each cached `url_path` row whose
+   `SuitePath` equals the suite and whose member is a
+   `Packages*`/`Sources*`/`Contents-*` file. A 304 keeps the row. A 200
+   replaces the row's blob only if the body passes the flat index sniff for
+   its extension (a compressed `Contents-*`: its extension's magic); else
+   the old row stays (counter `acu_anchorless_sniff_rejected_total{host}`).
+   A plain `Contents-*` has no sniff: one whose bytes changed is deleted,
+   and the next client miss fetches it (phase G deviation 2). 404/410
+   deletes the row; 401/403 keep it; 5xx, a transport error, a body over
+   64 MiB (WARN once per check) and the deadline keep it, unanswered. A
+   dedicated cache method preserves `last_requested_at` and
+   `request_count` and sets `last_fetched_at`, only when the blob changes,
+   to `MAX(now, previous + 1)`, after the blob is in the pool. Cached
+   `InRelease`/`Release` rows under the suite whose probe answered 404/410
+   are deleted (the probes' own answers, no other request). The check is
+   answered — `anchorless_refreshed` when a row was replaced or deleted,
+   `anchorless_unchanged` otherwise, form `anchorless` — unless every index
+   request went unanswered, a failed check; an answered check is a success
+   that clears the suite's failure or refusal record and its pending
+   change. No snapshot, no vouching, no strict-mode effect. If a
+   `Release`/`InRelease` later becomes *present*, the suite moves to the
+   normal forms. `url_path`-served metadata (a hit, a miss, a stale serve)
+   uses `max(pool mtime, last_fetched_at)` as `Last-Modified`, so apt's
+   `If-Modified-Since` on `Packages*` sees a replacement; every writer of
+   `last_fetched_at` stamps it after the pool file is final, so C8's
+   first-adoption `adopted_at` floor stays above it (phase G ruling R-G3).
 
 **Adoption capacity.** Adoption, repair and reconcile obey one discipline.
 Before spawning the goroutine the check `TryAcquire`s the global slot
@@ -830,7 +851,8 @@ Filenames only; `ParseRelease` keeps `validateMemberPath`):
   (literal bytes; C1 re-encodes it upstream; requests are compared after one
   decode);
 - reject, fatal as today: empty, absolute, NUL, control bytes, backslash,
-  `?`, `#`, `..` segment, empty segment, length > 4096.
+  `?`, `#`, `..` segment, empty segment, length > 4096, and a Filename made
+  only of `.` segments (it names no file; phase G design note 5).
 
 **Hash blocks.** `ParseRelease` merges hash blocks per member path, as apt
 does: `ReleaseMember` carries `SHA256` and `SHA512`; the size comes from the
@@ -840,7 +862,10 @@ verifies the fetched bytes against **every** declared SHA256/SHA512 hash,
 returns the SHA256 of the verified bytes (a *derived* anchor for SHA512-only
 members; controlling it needs a SHA512 preimage) and the by-hash alias paths
 for **every** declared algorithm. A Packages stanza with a Filename but no
-SHA256 makes coverage incomplete (reason `no_sha256`), never a silent drop.
+SHA256 makes coverage incomplete (reason `no_sha256`), never a silent drop —
+also when no stanza has one: such a file parses and its snapshot adopts
+with incomplete coverage, while a body with no stanza naming a `Filename`
+(an HTML page) still fails its parse (phase G ruling R-G1).
 Limitation (documented): integrity-skipped SHA512-only members are not
 recorded for skip-repair; reconcile still heals requestable IndexTargets.
 SHA512 is verified before the bytes reach the pool; a SHA512-only member
@@ -1340,7 +1365,12 @@ plain-text suite that adopts unsigned through `Release` (C5), not junk;
 any other sniff-failing anchor is evidence whatever the suite's adoption
 record — a failed adoption's record would otherwise keep an HTML anchor
 served for good, since an unadopted suite's metadata is never
-revalidated. A missing or unreadable pool file is no evidence; the sniffs
+revalidated. A flat suite with no cached anchor row is judged by its
+cached root `Packages*`/`Sources*` rows instead, by the same rules (the
+flat index sniff for the row's name, fetched at least 24 h ago, whatever
+the record); a suite with any anchor row is judged by its anchors alone;
+the index rows are read only in a run that has such a suite (phase G
+rulings R-G2, R-G5). A missing or unreadable pool file is no evidence; the sniffs
 read at most 64 KiB per anchor on the reaper's goroutine, and each verdict
 is kept per anchor blob until the next scan. For each candidate: hold the
 suite through the checker's `SuiteGuard` (`HoldSuite` `TryLock`s the
@@ -1558,7 +1588,8 @@ gauges `acu_adoption_failing_suites` (C7), `acu_freshness_waiting_for_slot`
 phase E's `acu_repair_deferred_total{host}` and
 `acu_strict_adoption_waits_total{host}`, and phase F's unlabelled counter
 `acu_gc_retired_reaped_total` and `acu_suites_reaped_total{reason}` (three
-values: `idle`, `non_canonical`, `junk`).
+values: `idle`, `non_canonical`, `junk`), and phase G's
+`acu_anchorless_sniff_rejected_total{host}` (C4 Dispatch 4).
 New metrics carry no suite labels: they are unlabelled, host-labelled or,
 for `acu_suites_reaped_total`, labelled by its three reasons. They are
 listed in the metrics inventory comments of `internal/freshness/metrics.go`
```

- [ ] **Step 5: Overview**

The spec reference becomes revision 10; phase G's row reads "in progress (plan written 2026-10-20)", as phase F's docs task set its own, and becomes "done (commit …)" after the final review; the G line gains the names as built; the "**G:**" carry-forward block is deleted, as phases C–F deleted theirs — every item is done (Task 15's DoD map names the tests); H gains phase G's items (the README, a Release-less repository in `e2e/flat`, the prose WARN names); the open decisions gain the anchorless refresh's independence from `adopt_unsigned` and `proxy.IsMetadata`'s prefix rule:

```diff
diff --git a/docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md b/docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md
--- a/docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md
+++ b/docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md
@@ -8,7 +8,7 @@
 
 **Tech Stack:** Go 1.25 (toolchain 1.26), modernc SQLite, `github.com/ProtonMail/go-crypto`, `testing/synctest`, Docker for e2e.
 
-**Spec:** `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (revision 9). Plan reviews: 2026-09-28 (phase A executability and tests) and `2026-10-05-flat-repositories-plan-review.md` (upgrade trust and later-phase contracts; its findings map to the "Review findings 2026-10-05" table below). Every task implements a named spec section; when this plan and the spec disagree, the spec wins unless the plan records a deliberate, explained deviation (listed below).
+**Spec:** `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (revision 10). Plan reviews: 2026-09-28 (phase A executability and tests) and `2026-10-05-flat-repositories-plan-review.md` (upgrade trust and later-phase contracts; its findings map to the "Review findings 2026-10-05" table below). Every task implements a named spec section; when this plan and the spec disagree, the spec wins unless the plan records a deliberate, explained deviation (listed below).
 
 ## Global Constraints
 
@@ -64,7 +64,7 @@ Each phase: write its detailed plan (`2026-09-28-flat-repositories-<letter>-<nam
 | D. Creation bounds and signals | `…-D-creation.md` | C3 (rest: caps, touch, miss-path signed-seen marker, `Release`/flat sniffs) | A | done (commit 3ed0b2b) |
 | E. Eager adoption and anchor forms | `…-E-forms.md` | C4 probe tree, C5 unsigned/SHA512, C10 wait | B, D | done (commit 9ab48c4) |
 | F. Suite reaping | `…-F-reaping.md` | C9 | D, E | done (commit dfdbb90) |
-| G. Flat repositories | `…-G-flat.md` | C2 flat, C5 flat IndexTargets/Filenames, C6, C4 anchorless | A–F | to write after F |
+| G. Flat repositories | `…-G-flat.md` | C2 flat, C5 flat IndexTargets/Filenames, C6, C4 anchorless | A–F | in progress (plan written 2026-10-20) |
 | H. End to end and docs | `…-H-e2e-docs.md` | §7 e2e, §8 | all | to write after G |
 
 ## Cross-phase interfaces (names later phases rely on)
@@ -94,26 +94,13 @@ Later phases add (named here so phase plans stay consistent):
 - D: `cache.CreateSuite`, `cache.UpdateSuiteFreshness`, `cache.TouchSuite`, `cache.ErrSuiteGone` (the UPDATE-only `CommitAdoption` Step 6, spec C3), `aptpath.SniffRelease`, `aptpath.SniffFlatIndex` (the `InRelease`/`Release.gpg` sniffs and marker writes already exist from A). As built: `cache.CreateSuite(ctx, SuiteSeed, SuiteCaps, SuiteGuard) (CreateResult, error)` (`SuiteSeed.Cached` carries the suite's pre-read cached anchors, `CreateResult.Unproven` asks for the backstop mark, `CreateResult.CandidatesBusy` flags a transient all-busy refusal), `cache.StoreFetched(ctx, FetchedRow, SuiteCaps, SuiteGuard) (FetchedResult, error)` (`FetchedRow.Restamp`: the adoption-disabled refresh miss), `cache.SuiteExists` (renamed `HasSuiteRow` in phase F), `cache.SuiteCapUsage`, `cache.TouchSuite` / `TouchedSuites`, `cache.SuiteGuard` (implemented by `(*freshness.Checker).HoldSuite`) and the optional `cache.ClockStepReporter` (the checker reports eviction's future stamps), unexported `deleteSuiteTx(ctx, tx, key, wantCurrent *int64, cutoff, now) (deleteOutcome, error)` (F's `ReapIdleSuite` reuses it), `freshness.Config.MaxSuites`, `config.FreshnessConfig.MaxSuites`, `aptpath.ClassifySuiteFile` / `SuiteFile`, `aptpath.MaxSuitePathBytes`, `aptpath.DecodableClearsign`, `simtest.ClearsignedFixture`, `(*freshness.Checker).LockCount`. **Signed-seen marker on the miss path (review finding 5, its follow-up and the three-lens review, spec C3):** anchor rows (`InRelease`, `Release`) are created in the writer operation that stores the `url_path` row, before the body is written (sniff → `url_path` + `CreateSuite` + marker → write body → T1, today's seed-before-serve order (the T1 on a creating miss: phase D plan, design note 2; phase E plan, Task 10)); the marker for a sniff-passing `InRelease`/`Release.gpg` is written in that same operation and nowhere else in the handler — no hit-path write (except that every row creation, on a miss or on a hit, marks the suite's cached signed anchors in its own transaction: phase D plan, deviation 2, ruling PD-5), no wait, no coalescing map, no failure counter; a failed operation serves no body (502, as today). `Release.gpg` creates no row. **The backstop and its residual:** the one marker write outside a creating or storing transaction is the handler's `markCachedSignedAnchors`, which runs when a creation is unproven (`CreateResult.Unproven`: one of the suite's cached `InRelease`/`Release.gpg` rows changed, appeared or could not be read between the handler's pre-read and the creating transaction). It reads, sniffs and marks that anchor right after the commit and the late registration, before the response, bounded at 5 s. So an unproven creation's suite is tracked with that anchor unmarked from the commit until the backstop runs, and — if the backstop fails (WARN `suite signed-seen mark failed` or `suite signed-seen backstop read failed`) — until the startup marker pass at the next start (spec C3 accepts it; E carry-forward below). Tests: the marker row exists before the first body byte on first contact and on a refresh miss; a failed writer operation serves no body; the hit-path invariant (after misses, adoptions, `reconstructAnchor` and restarts, every sniff-passing anchor row of a tracked suite has a marker (deviation 1)); FIFO ordering against a later unsigned commit. D also removes reconcile's plain-text fallback in `inReleasePlaintext` once `clearsignedFixture()` replaces the plain-text test fixtures (flip `TestReparseSnapshotRelease_PlainTextInReleaseStillParses` to `_PlainTextInReleaseIsAnError`).
 - E: `freshness` probe classification (`classPresent`, `classAbsent`, `classUnknown`), `Adopter.RunUnsigned`, `aptpath.AnchorPaths(form)` (spec C5). E's unsigned candidates set `Form: cache.FormUnsigned` explicitly, and its tests assert that an unsigned adoption stores `anchor_form = 'unsigned'` and vouches no `.deb`. E also resolves the two `AIDEV-TODO(phase E)` raw-SQL readers (url_path GC guard, `CommitAdoption` anchor seed: for `FormUnsigned`, Step 3c seeds `<suite>/Release`, not `/InRelease`). E's docs note the marker denial-of-service residual (spec C5 threat notes) at `adopt_unsigned`. **In-transaction never-downgrade (review finding 5, spec C5):** `CommitAdoption` re-reads the marker for `aptpath.MarkerBase(suite)` inside its writer transaction and refuses an unsigned candidate (no pointer change, no rows) when one exists or the prior current snapshot is signed; E's DoD pauses an eligible unsigned adoption before commit, serves a signed anchor and completes the marker write (also via the `/.` twin, once G exists — E adds the twin variant as a G DoD item), then resumes and asserts refusal. **Upgrade (review findings 2–3):** a phase-A-quarantined plain-text suite (fixture from A's `buildLegacyFixture` shape) adopts in the unsigned form through `Release` with no upstream change and no `unsigned_downgrade_refused`; a never-adopted suite whose signed anchor was served under a v6 binary is refused unsigned adoption on its first post-upgrade check (A's startup pass wrote its marker). As built: `freshness.classify(res, err, sniff, conditional) probeOutcome` (classes `classPresent`, `classAbsent`, `classUnknown`) and the probe tree `runProbeTree`; `(*Adopter).RunUnsigned(ctx, suite, release, etag, lastmod) error` and `(*Adopter).AdoptsUnsigned() bool`; `aptpath.AnchorPaths(form string) []string` and `(*cache.SuiteSnapshot).AnchorHash(name string) *string`; `cache.ErrUnsignedDowngradeRefused` (the refusal inside the commit) and `freshness.ErrAdoptionUnsignedDowngradeRefused`, `ErrAdoptionUnsignedTooLarge`; `(*freshness.Checker).AdoptionsInFlight(host) bool` and `WaitHostAdoptions(ctx, host, start) bool` (the `handler.AdoptionWaiter` the handler gets as `handler.Config.Adoptions`); `config.AdoptionConfig.AdoptUnsigned` and `freshness.AdoptionConfig.AdoptUnsigned`; the `freshness.Cache` method `ListSuitesWithAdoption` (in place of `ListSuites`); the flip barrier's code in `internal/freshness/flip_barrier.go`; the metrics `acu_repair_deferred_total{host}` and `acu_strict_adoption_waits_total{host}`; spec C5's `no_sha256` coverage rule (E fix round 1): the unexported `freshness.parsePackages` reports the stanzas with a `Filename` but no `SHA256` (`ParsePackages` keeps its signature), and `buildPackageHashes` makes such a snapshot's coverage incomplete (`package_coverage_incomplete`, reason `no_sha256`); the stanzas it reports count toward `MaxPackageStanzas`. G's flat parity row that cites it needs no new code for a file that mixes such stanzas with complete ones; a file in which no stanza has a SHA256 is G's open question (below). The `/.` twin variant of the in-commit race test is G's (below).
 - F: `cache.ReapIdleSuite`, admin `POST /forget`. Non-canonical legacy suite rows (`!aptpath.IsCanonicalSuitePath`) are reaped regardless of idleness (spec C9); the plan review found none in production. The reaper's refcount release reuses `decrementSnapshotRefcountsTx`; snapshot GC sub-job C also deletes the snapshots phase A's audit retired, and sub-job B's `keep_displaced` ranking excludes retired rows so they never take retention slots. The reaper sweeps a signed-seen marker only when no suite row exists for its base or `/.` twin and it is older than 365 days. F's DoD includes a signed snapshot adopted under the phase-A binary, reaped by the F binary, then returning: never-downgrade still refuses unsigned adoption (markers outlive reaping). As built: `cache.ReapIdleSuite(ctx, SuiteKey, wantCurrent *int64, cutoff int64) (deleted, gone bool, err error)` (a failed submit settled by a fresh read, and a settled reap logged `suite_reaped` with `settled=true` and counted; every suite-row delete — cap eviction, a reap, a forget — moves the base's markers up to `MAX(last_seen_at, now)`, ruling PF-10, and, before it retires the suite's snapshots, upserts its base's marker under the suite's scheme when the suite has an adopted, non-retired inline or detached snapshot: unexported `markSnapshotHistoryTx`, with never-downgrade's snapshot clause shared as `signedHistorySnapshotSQL`, the one marker a delete creates, ruling PF-12), `cache.ForgetSuite(ctx, SuiteKey, wantCurrent *int64) (ForgetResult{Deleted, Gone, MarkersDeleted, AnchorRowsDeleted, KeptBy}, error)` (with the markers it deletes the cached anchors of the base and its `/.` twin under both schemes, ruling PF-2), `cache.DeleteSuiteMetadata(ctx, SuiteKey) (int, error)` (spec C9 step 1; a key `aptpath.IsCanonicalSuitePath` refuses deletes nothing), `cache.ReapRegardless` (`deleteSuiteTx`'s cutoff for a delete regardless of idleness), `cache.ListUnadoptedSuiteAnchors` (`SuiteAnchorRow`), `cache.SweepOrphanMarkers(ctx, now, cutoff int64) (MarkerSweep{Swept, Furthest}, error)` (it never rewrites a stamp, ruling PF-1; the reaper passes a fresh `now` for `Furthest` and, for the cutoff, the run's clock less the forward steps it detected and 365 days, ruling PF-13), `(*cache.Cache).RunRetiredSnapshotGCChunk(ctx, maxRows int) (reaped int, more bool, err error)` (sub-job C), `(*cache.URLPath).LastUse() int64` (the stamp's one Go definition), `(*cache.Cache).HasSuiteRow` (renamed from `SuiteExists`); cap eviction moved to `internal/cache/suite_evict.go` and `deleteSuiteTx`, the one production `DELETE FROM suite_freshness`, to `suite_reap.go` beside two of its three callers, `ReapIdleSuite` and `ForgetSuite` (cap eviction's `createSuiteTx` stays in `suite_rows.go`; ruling R-F1). `cache.RetireSuiteSnapshots` was dropped with R-F4's knob-off retirement (ruling PF-5): phase E's PE-12 carry-forward is resolved by PF-5 — with `adoption.adopt_unsigned = false` an established unsigned suite keeps its coherent snapshot, refused `unsigned_disabled`, until the knob is back on or the suite is forgotten or reaped (forgotten or reaped with the knob still off, it is then a never-adopted suite), and the docs say plainly that the cache then does not keep unsigned repositories fresh (the maintainer confirmed it, 2026-10-20). `freshness.Reaper`, `NewReaper(ReaperConfig{Cache, Checker, IdleTTL, Logger})`, `(*Reaper).Run(ctx)` (started by `main` beside `Checker.Run`; at most 250 candidates reaped regardless of idleness first, then at most 25 idle ones in the same run — `regardlessReapsPerRun`, `reapsPerRun`, `runBudgets`, ruling PF-14 —, the junk scan every run, the marker sweep daily, the first a day after `Run` starts, a forward step of the wall clock deferring reaping and the sweep by the uptime floor and taken off the sweep's cutoff for the life of the process — `stepAhead`, ruling PF-13 —, a suite's held work bounded at 5 minutes, a panic under a suite's hold recovered for that suite alone; started through `main`'s `runReaper` seam) and `(*Reaper).Forget(ctx, scheme, host, suitePath) (cache.ForgetResult, error)` (a panic under its hold logged `freshness_check_panicked` with `where=forget`, released as the cache then answers, then re-panicked to net/http, whose `http: panic serving` line the admin server's `ErrorLog` routes through slog at ERROR; a forget whose writer call failed after its commit logged `suite_forgotten` with `settled=true`); `admin.Config.Forgetter` (`admin.Forgetter`; a forget in flight is cut short by the admin listener's `Shutdown` and answered 503, and a `host` holding `://` is refused with 400), `requireMutationGuard`, and the status JSON's additive `suites[].scheme` (the HTML Host cell shows `scheme://host`; ruling PF-7); `config.GCConfig.SuiteIdleTTL` (`gc.suite_idle_ttl`); `gc.LastRunSummary.RetiredReaped`, `acu_gc_retired_reaped_total` and `acu_suites_reaped_total{reason}`; the startup GC pass runs no sub-job C, the periodic ticks do (`gc.phaseStartup`, `runSnapshotPass`; ruling PF-15); `flipgate.(*Gate).Purge` keeps a live entry (ruling R-F2). Events `suite_reaped`, `suite_forgotten` (`anchor_rows`, `settled`), `admin_forget_requested`, `signed_seen_markers_swept`; `freshness_check_panicked` gains `where=reaper` (naming the suite when it struck under a suite's hold) and `where=forget`, and `clock_step_detected` the stamps `last_seen_at` and `wall_clock`; the reaper's clock-step reports are logged outside the checker's one-line-a-minute limit; the reaper's busy line, held-step WARNs and junk-read WARN, and every `freshness_check_panicked` and `adoption_panicked` line, and the handler's `handler_goroutine_panicked` `where=touch` line, carry `canonical_scheme`; the startup line gains `gc_suite_idle_ttl` and `adoption_adopt_unsigned`. Rulings R-F1–R-F5 and PF-1–PF-15 and deviations 1 (withdrawn: it concerned the dropped retirement), 2 (the coarser 90-day simulation, a four-day one at the production cadence beside it) and 3 (the sweep never rewrites a future stamp) are in the phase F plan and spec revision 9.
-- G: `aptpath.IsFlatSuitePath`, `aptpath.FlatBase`, `aptpath.DotClean`, `freshness.validateFilename`, and the admin `suites[].flat` field (phase B plan, deviation 3). Legacy flat onboarding also writes signed-seen markers for sniff-passing cached flat anchors (the startup pass only covers suites that already have rows), and the `/.` twin variant of E's in-transaction recheck test runs here. **PR #8 evidence in G's DoD:** `TestServeHTTP_FlatRepositoryFreshness` (a miss on `/…/deb/InRelease` and on `/…/deb/./InRelease` creates one suite row each and fires T1 under it; a later hit fires T1 again; `/…/deb/Packages_1.0_all.deb`, host-root `/InRelease` and `/…/deb//InRelease` create none), the pkgs.k8s.io shape `/core:/stable:/v1.34/deb/InRelease` → `/core:/stable:/v1.34/deb`, flat `Packages` failure fatal, and the mixed-host strict matrix. G also ports the rest of PR #8's tests (see "PR #8 parity" below).
+- G: `aptpath.IsFlatSuitePath`, `aptpath.FlatBase`, `aptpath.DotClean`, `freshness.validateFilename`, and the admin `suites[].flat` field (phase B plan, deviation 3). Legacy flat onboarding also writes signed-seen markers for sniff-passing cached flat anchors (the startup pass only covers suites that already have rows), and the `/.` twin variant of E's in-transaction recheck test runs here. **PR #8 evidence in G's DoD:** `TestServeHTTP_FlatRepositoryFreshness` (a miss on `/…/deb/InRelease` and on `/…/deb/./InRelease` creates one suite row each and fires T1 under it; a later hit fires T1 again; `/…/deb/Packages_1.0_all.deb`, host-root `/InRelease` and `/…/deb//InRelease` create none), the pkgs.k8s.io shape `/core:/stable:/v1.34/deb/InRelease` → `/core:/stable:/v1.34/deb`, flat `Packages` failure fatal, and the mixed-host strict matrix. G also ports the rest of PR #8's tests (see "PR #8 parity" below). As built: `aptpath.IsFlatSuitePath`, `FlatBase`, `DotClean` and `IsFlatHostRootPath` (the flat grammar in `aptpath.SuitePath`), `aptpath.IsAnchorlessIndex` and `SniffAnchorlessIndex`; `cache.PackageHash.FlatFilename` (written by `CommitAdoption` and reconcile), `DeclaredHashesForPath`'s suffix fallback over `idx_package_hash_flat_filename` (at most 32 segments, the longest ancestor-or-self base), `cache.ListSuiteMetadataRows`, `RefreshURLPathBlob`, `DeleteURLPathRows` and `ListUnadoptedFlatIndexRows`; `freshness.validateFilename`, the layout-aware `isIndexTarget`, `indexTargetGroup`, `archFromFilteredPath`, `missingRequestableIndexGroups` and `missingRequestableMembers` (each takes the suite path), `(*Adopter).flatIndexClientCreated` (Dispatch 3's flat index rows), `(*Adopter).anchorlessEligible` and `(*Checker).anchorlessRefresh` (Dispatch 3–4; results `anchorless_refreshed`/`anchorless_unchanged`, form `anchorless`; `acu_anchorless_sniff_rejected_total{host}`), the reaper's flat index junk evidence, read only when a run has an anchorless flat suite (`hasAnchorlessFlatSuite`); the handler's layout-aware `isIndexTargetPath` and `indexTargetArch`, `flat_host_root_unsupported` (one WARN a minute) and `urlPathModTime` (`Last-Modified = max(pool mtime, last_fetched_at)` at url_path metadata serves); the admin `suites[].flat`. Rulings R-G1–R-G5 and deviations 1 (the anchorless gate reads signed evidence) and 2 (a changed plain `Contents-*` is deleted) are in the phase G plan and spec revision 10.
 - H: PR #8's end-to-end table in `e2e/flat`: one signed flat repository configured as `deb <uri>/ /` on one client and `deb <uri> ./` on another; both report 2.0 within one cooldown of the v2 publish, and with the upstream stopped two fresh clients install 2.0 from the snapshots. H also runs the live-upstream smoke and documents the strict-mode consequences listed under "PR #8 parity" below.
 
 ## Carry-forwards (from phase A and B reviews)
 
 Each item is owned by the phase it is listed under; that phase's plan addresses it. Items came from the phase A reviews (2026-10-11) unless marked *(B)* (the phase B reviews and its final review, 2026-10-18) or *(C)* (the phase C plan, 2026-10-18) or *(D)* (the phase D plan, 2026-10-18) or *(E)* (the phase E plan, 2026-10-19) or *(F)* (the phase F plan, 2026-10-20).
 
-**G:**
-
-- *(B)* The admin `suites[].flat` field (spec C7 and C11; phase B plan, deviation 3) arrives with `aptpath.IsFlatSuitePath`; add it to the `suites[]` row-field table in `docs/admin-ui-spec.md` §4.5.
-- *(C)* If G serves url_path metadata with `Last-Modified = max(pool mtime, last_fetched_at)` (spec C4, anchorless refresh), `adoptionStampTx`'s first-adoption floor (`MAX(last_fetched_at) + 1`, `internal/cache/adoption.go`) stays above it only while the pool file's mtime is not later than `last_fetched_at` — true today because `runFetch` finalizes the pool file before it stamps the row (`internal/handler/handler.go`). Keep that order, or fold the anchor blobs' mtimes into the floor.
-- *(D)* `ClassifySuiteFile` already returns `FileFlatIndex` for a root `Packages*`/`Sources*` of a suite without a `dists` segment, and the handler already sniffs, creates rows for and touches it; G's grammar makes those paths reachable (spec C3; phase D plan, ruling R-D3), so G adds no handler code for them — its tests add the flat cases to the miss-path, onboarding and touch tests. The cap-rejection cache is keyed by the suite alone, whichever file was refused, so a flat suite's indexes share one entry: G adds no per-file key.
-- *(E)* The `/.` twin variant of `TestCheck_UnsignedRefusedInsideTheCommitIsAState` (phase E plan, Task 3): the signed anchor and its marker arrive through the twin while the unsigned adoption of `/repo` is paused before its commit; the commit refuses it as a state.
-- *(E)* Spec C4 Dispatch 3: `cachedAnchors` (`internal/freshness/first_adoption.go`) counts only `InRelease` and `Release` rows as client-created (phase E plan, design note 14); G adds flat index rows (`Packages*`/`Sources*`, `request_count > 0`), and the anchorless refresh where the tree finds neither anchor (`errNoAnchor`, a failed check until then) for flat suites with no signed-seen marker and no sniff-passing cached anchor row.
-- *(E)* The nvidia-shaped DoD fixture is `dists/`-shaped (phase E plan, design note 29); G's DoD adopts the real flat layout with its `./` Filenames.
-- *(E)* A `Packages` file in which no stanza has a `SHA256` (only `SHA512`, `SHA1` or `MD5sum`) still fails its parse (`packages: no stanzas with both Filename and SHA256`, `internal/freshness/packages_parse.go`), so its suite's adoption fails as `adoption_parse_failed` at every check, where spec C5 expects the snapshot to adopt with incomplete coverage (`no_sha256`). Phase E made the rule fire only for files that also have complete stanzas (review fix round 1, Task 2). G, which brings the SHA512-only flat repositories most likely to publish such files, decides it: adopt with incomplete coverage (keeping the parse error for a body that is no Packages file at all, e.g. an HTML error page), or keep failing and document it.
-- *(F)* Until G's grammar a flat-shaped suite row is non-canonical (`aptpath.IsCanonicalSuitePath`), so the reaper would reap it at its first run regardless of idleness; none exists, since no release created one. With G's grammar flat suites are canonical and reaped by idleness like any suite, and the reaper's metadata deletion (`DeleteSuiteMetadata`: rows whose `aptpath.SuitePath` is the suite) and junk scan (`aptpath.ClassifySuiteFile` on the anchor rows) start covering them with no code change. G's DoD item "reaping `/repo` leaves `/repo/.` intact" (spec §9) runs `DeleteSuiteMetadata` and the reap on a real flat suite with a `./` twin — `TestDeleteSuiteMetadata_DeletesOnlyTheSuitesRows` and `TestReap_LeavesTwinsAndNestedSuitesAlone` use `dists/` paths and raw `/flat`, `/flat/.` keys whose files no grammar maps to a suite before G — and adds a flat suite, kept while used, to `TestReaper_ReapsIdleAndNonCanonicalSuitesAndPurgesThem`.
-- *(F)* `TestDoD_ForgetIsTheEscapeHatchForADowngradeRefusal`'s twin subtest seeds the row `rigSuite + "/."`, a `dists/` path with a twin suffix no grammar produces before G; G repeats it with a real flat suite whose `./` twin keeps a row.
-- *(F)* The reaper's junk rule (ruling R-F3 with PF-3) considers only `InRelease`, `Release` and `Release.gpg` rows, so an anchorless flat suite whose cached flat `Packages` is an HTML page is never junk, and the hot path keeps serving it (phase F design review, M-17). G decides whether flat index rows count as junk evidence (`aptpath.SniffFlatIndex`), with a test either way.
-
 **H:**
 
 - *(E)* The prose WARN names phase E adds — `freshness: read anchor rows failed`, `freshness: read current snapshot failed`, `adoption: served anchor read failed; the flip barrier runs` and `adoption: signed-anchor re-mark failed` (documented as they are) — may become snake_case in H's documentation sweep, with phase D's (listed under "PR #8 parity").
@@ -123,10 +110,16 @@ Each item is owned by the phase it is listed under; that phase's plan addresses
 - *(F)* The 1.1.0 release notes tell offline and archive caches — and any install that relied on `gc.enabled = false` to keep everything — to set `gc.suite_idle_ttl = "0s"` before upgrading: the suite reaper runs whatever `gc.enabled` says and, from an hour after the first start, deletes the suites no client asked for in `gc.suite_idle_ttl` (720 h by default, counted from the last request before the upgrade, so a long-idle suite goes at once), with their cached index files (`docs/configuration.md`, *Upgrading an existing cache* and *Suite reaping*, says so; the Task 11 review).
 - *(F)* The prose WARN names phase F adds — the seven `suite reaper: …` lines and `admin: forget failed` (documented as they are) — may become snake_case in H's documentation sweep, with phase D's and E's.
 
+- *(G)* `README.md` (spec §8): flat repositories — both source forms, the host-root limitation, Release-less repositories refreshed without a `Release` (`docs/configuration.md`, *Flat repositories*) — beside H's other README items.
+- *(G)* `e2e/flat` gains a Release-less repository (a Gemfury-shaped `Packages.gz` with no `Release`): a republished index reaches a client within one `periodic_refresh`, and an upstream that then answers HTML keeps the old index.
+- *(G)* The prose WARN names phase G adds — `freshness: anchorless index over its size cap`, `freshness: delete anchorless rows failed` and `suite reaper: list unadopted flat indexes failed` (documented as they are) — may become snake_case in H's documentation sweep, with phase D's, E's and F's.
+
 **Open decision (spec-level, for the maintainer):**
 
 - An `anchor_corrupt` or `anchor_oversized` quarantine leaves the known-corrupt `url_path` anchor in Phase-1 serving, and the checker sees "unchanged". This is pre-existing behaviour for any bit-rotted metadata blob. Decide in a later phase whether the audit should also invalidate that `url_path` row.
 - Host-header validation of the admin listener's mutating endpoints, `POST /forget` and `POST /reconcile` (spec §10 G-1; phase F ruling PF-6): their guard stops a cross-site form or `fetch`, but not DNS rebinding against a listener without `htpasswd_file`. Refusing a non-IP, non-`localhost` `Host` when no htpasswd is configured would close it, but affects operators behind reverse proxies; the guard is one function (`requireMutationGuard`), so the change is small once decided.
+- The anchorless refresh does not read `adoption.adopt_unsigned`: with the knob off a Release-less flat repository is still refreshed (it has no unsigned candidate the knob could refuse; phase G design note 9). Decide whether the knob should also stop the anchorless refresh.
+- `proxy.IsMetadata` classifies any path whose last segment starts with `Packages` or `Sources` as metadata, so a `.deb` named like `…/Packages_1.0_all.deb` (PR #8's grammar fixture) is served as metadata: no `package_hash` check and no strict-mode refusal (pre-existing; phase G's grammar gives it no suite). Decide whether `IsMetadata` should match the member names of spec C2's grammars instead of a prefix.
 
 ## PR #8 parity (gap analysis, 2026-10-17)
 
```

- [ ] **Step 6: Deferred files**

Each file's diff deletes the entries this plan resolved (each is one `- **ID** …` line; the section headings stay): A-4, A-5, A-6 (`…-A-deferred.md`); B-59, B-62 (`…-B-deferred.md`, where B-63 gains phase G's note); D-3, D-4, D-27, D-31, D-32 (`…-D-deferred.md`); E-9, E-16, E-17, E-19, E-47, E-57, E-60, E-64, E-65, E-67, E-68, E-70, E-71 (`…-E-deferred.md`); F-12 (`…-F-deferred.md`). No deferred file becomes empty; none is deleted (phase H's sweep deletes them).

```diff
diff --git a/docs/superpowers/plans/2026-09-28-flat-repositories-A-deferred.md b/docs/superpowers/plans/2026-09-28-flat-repositories-A-deferred.md
--- a/docs/superpowers/plans/2026-09-28-flat-repositories-A-deferred.md
+++ b/docs/superpowers/plans/2026-09-28-flat-repositories-A-deferred.md
@@ -11,9 +11,6 @@ Phase A's per-task and final reviews (2026-10-11, Fable max) found these minor i
 
 ## internal/aptpath and internal/proxy
 
-- **A-4** `suite.go:19-20` comment overclaims "matching the old greedy regex"; add `{"/a/dists/b/dists/c", "/a/dists/b"}` to `TestSuitePath_Dists`.
-- **A-5** `suite.go:25-26` AIDEV-NOTE says "raw path"; the handler strips from `req.Path`, the same string `SuitePath` received. Reword.
-- **A-6** `suite_test.go` lacks a space-byte case and a trailing-slash full-path case.
 - **A-8** `TestCacheDoesNotImportProxy` checks direct imports only; comment the limitation.
 - **A-9** `encode_test.go:36`: the all-bytes loop starts at 1, skipping NUL (it round-trips). Start at 0 or comment.
 - **A-10** `proxy.go:172-173`: the control-byte invariant has a plain comment; promote it to an AIDEV-NOTE.
```

```diff
diff --git a/docs/superpowers/plans/2026-09-28-flat-repositories-B-deferred.md b/docs/superpowers/plans/2026-09-28-flat-repositories-B-deferred.md
--- a/docs/superpowers/plans/2026-09-28-flat-repositories-B-deferred.md
+++ b/docs/superpowers/plans/2026-09-28-flat-repositories-B-deferred.md
@@ -33,10 +33,8 @@ Phase B's per-task reviews and its three final-review lenses (concurrency, spec
 
 ## internal/admin
 
-- **B-59** `suites_status_test.go`: no row with 0 < lag < 3600 (a `LagWarn = lag > 0` regression would pass), and the JSON golden pins only zero values (`anchor_form ""` beside `current_snapshot_id 42`).
 - **B-60** `status_test.go:261` `suites_healthy_no_lag` fixture still sets `InReleaseChangeSeenAt`.
-- **B-62** `suites_status_test.go` `htmlRowStart` slices `html[:offset]` and panics on -1 (still a test failure).
-- **B-63** The status page cannot say why a suite with failing checks (stalling upstream, backoff up to 1 h) or one waiting for a slot is not updating: no row state, an empty Adoption cell, `check_fail_count` only in the JSON (final review T M9; kept deferred by ruling). Show "checks failing ×k · next check in …" in the Adoption cell.
+- **B-63** (checked by phase G, which changes `status.go`: its "next check in" needs the scheduler's backoff, `freshness.backoffDelay`, which `internal/admin` reaches only through injected interfaces — a design choice for H) The status page cannot say why a suite with failing checks (stalling upstream, backoff up to 1 h) or one waiting for a slot is not updating: no row state, an empty Adoption cell, `check_fail_count` only in the JSON (final review T M9; kept deferred by ruling). Show "checks failing ×k · next check in …" in the Adoption cell.
 
 ## Final fix-wave residuals (scoped re-review of 9a2b22b..e361704)
 
```

```diff
diff --git a/docs/superpowers/plans/2026-09-28-flat-repositories-D-deferred.md b/docs/superpowers/plans/2026-09-28-flat-repositories-D-deferred.md
--- a/docs/superpowers/plans/2026-09-28-flat-repositories-D-deferred.md
+++ b/docs/superpowers/plans/2026-09-28-flat-repositories-D-deferred.md
@@ -8,8 +8,6 @@ Left out: everything the phase D final fix wave resolved — CreateSuite's faile
 
 ## internal/aptpath
 
-- **D-3** `sniff_test.go` (`TestSniffFlatIndex`): only `.gz` and `.xz` have negative magic rows; shortening the `.zst` or `.lzma` magic (`sniff.go:123-125`; `.bz2`, `.lz4` alike) survives. Fix: one near-miss row per extension. Can wait: flat indexes are unreachable until G. (G) (Task 1 minor 4)
-- **D-4** `sniff.go:192`: `ClassifySuiteFile` (the PD-9 gate and the R-D3 routing point) has no AIDEV-NOTE naming its precondition (`suite` is `SuitePath(path)`), the `""` guard its callers rely on, and that its flat branch is reachable only from tests until G; `SuitePath`'s phase-G AIDEV-NOTE does not point here. (G) (Task 1 minor 5)
 
 ## internal/gpg
 
@@ -27,15 +25,12 @@ Left out: everything the phase D final fix wave resolved — CreateSuite's faile
 ## internal/freshness
 
 - **D-26** `reconcile_r1_test.go` (`DecodesAptsMarkerLine`) never asserts that `bytes.Replace` matched: if the encoder's first line ever ended in "\r\n", the test would pass without exercising the trailing-blank path. Fix: a `bytes.Equal(blanks, signed)` guard. (Task 3 minor 5)
-- **D-27** `reconcile_test.go` (`RejectsCorruptedAnchor`) asserts only `err != nil` while its comment says only the hash check can refuse it; assert the error contains "hash mismatch". (Task 3 minor 6)
 
 ## internal/handler
 
 - **D-28** `phase2_handler_test.go`: the local `staleBody` shadows the package helper `staleBody(path)` (`handler_test.go`); rename it, e.g. `staleAnchor`. (Task 3 minor 1)
 - **D-29** `fetch_seam_test.go`: the `recFetcher` type doc says it "answers every request with body", untrue when `inRelease` is set (only the field comment says so). (Task 3 minor 2)
 - **D-30** `phase2_handler_test.go` (`TestServeHTTP_AdoptedSuiteSnapshotMemberDBError_FailsClosed`): its upstream `/InRelease` body `"attacker bytes"` stays plain text with no PD-2 comment (no effect: the test asserts zero upstream calls; its sibling was wrapped). (Task 3 minor 4)
-- **D-31** Untested: the recover inside the rejection sweep's `expire` itself (its `onPanic` wiring and `Handler.Close` stopping it are pinned since the fix wave), and `New` refusing a negative `MaxSuites`. (Task 9 minor 2, narrowed)
-- **D-32** `suite_rows.go`: `cachedAnchors` and `markCachedSignedAnchors` repeat one loop (iterate, skip `except`, `LookupURL`, classify, `readHead`, sniff); the in-transaction mark and the backstop must agree on what counts as signed. One helper returning `(row, signed, err)` would keep them together. `runFetch` also builds `suiteSeed` twice with identical arguments. Can wait: the copies agree today. (G: if the sniff rules change) (Task 9 minor 3)
 
 ## cmd/apt-cacher-ultra
 
```

```diff
diff --git a/docs/superpowers/plans/2026-09-28-flat-repositories-E-deferred.md b/docs/superpowers/plans/2026-09-28-flat-repositories-E-deferred.md
--- a/docs/superpowers/plans/2026-09-28-flat-repositories-E-deferred.md
+++ b/docs/superpowers/plans/2026-09-28-flat-repositories-E-deferred.md
@@ -13,7 +13,6 @@ Left out: everything the phase E final fix wave resolved — the two never-downg
 
 ## internal/freshness — adoption members and SHA512
 
-- **E-9** `adoption.go`: `adoption_member_skipped` logs `declared_sha256=""` for a SHA512-only member; add `declared_sha512`. (G: SHA512-only flat repositories) (Task 2 minor 4)
 - **E-10** `member_verify.go`: `memberWriter` embeds `*cache.BlobWriter`, exposing `Finalize`/`FinalizeExpectingHash`; only an AIDEV-NOTE keeps a caller from skipping `verifyMember`. An unexported field with forwarded `Write`/`Truncate`/`Written`/`Abort` would enforce it. Plan-mandated shape; optional. (Task 2 minor 5)
 - **E-11** Coverage: no test exercises a size mismatch for a SHA512-only member in `verifyMember` (the `Finalize(size)` path); the "no SHA256 or SHA512 block" and "empty SHA256 and SHA512 blocks" parse errors are pinned by prefix only. (Task 2 minor 6)
 - **E-13** `flip_barrier.go` (since C-19's file split; this entry said `adoption.go`): the C-16 WARN `adoption: served anchor read failed; the flip barrier runs` also fires on a routine restart (a stop after the stop check or the re-mark, before the read); skip it on `context.Canceled` or when `p.stopErr(ctx) != nil`, as the final fix wave did for the check's local steps (FW-3 (d)). Shutdown noise. (Task 3 minor 7; concurrency can-wait)
@@ -21,9 +20,6 @@ Left out: everything the phase E final fix wave resolved — the two never-downg
 
 ## internal/freshness — unsigned form and markers
 
-- **E-16** `unsigned.go` re-mark: the twin direction "suite `/flat/.` with the evidence at `/flat/InRelease`" is untested (`range []string{suite.SuitePath, suite.SuitePath + "/."}` survives). No twin row exists before G. (G) (Task 3 minor 3)
-- **E-17** `unsigned_test.go` (`TestRunUnsigned_FailedRemarkRefuses`): `recs[0]["err"] == ""` is false when the key is absent, so dropping `"err", err` from the re-mark's log survives; `TestRunUnsigned_RemarkReadErrorRefuses` checks no `err` at all. Assert a non-empty string. (Task 3 minor 4)
-- **E-19** Survivor W5: `markSignatureSeen`'s base (`aptpath.MarkerBase`) is unpinned — marking `suite.SuitePath` survives, as no twin suite exists before G. (G) (tests lens W5)
 
 ## internal/freshness — the check
 
@@ -55,7 +51,6 @@ Left out: everything the phase E final fix wave resolved — the two never-downg
 
 ## internal/freshness — tests and simulations
 
-- **E-47** `retry_rig_test.go`: the rig start `1_800_000_000` is a literal in two places (hoist a `rigStart` const); `rigVerifier.n` is reset by the same three lines in two helpers (a `resetCalls()`). (Task 5)
 - **E-48** `internal/handler/phase2_detached_chaos_test.go` hand-builds the armored-signature shape `armoredSig` builds in freshness; a `simtest.ArmoredSignatureFixture(label)` beside `ClearsignedFixture` would give one definition of a sniff-passing `Release.gpg`. (Task 5)
 - **E-50** `sim_retry_test.go` (`TestSimRetry_LongRunDateRulesAndClockSteps`): the liveness floor is carried by the failing phases (276 of 317 checks); each 2-day clock-step stretch gives 18, below the 10-a-day floor. Give each stretch its own floor (≥ 16). (Task 14 minor 2)
 - **E-51** `sim_sched_test.go` `renderedMetricLabels` is a fifth render-and-scan metrics helper (`renderedMetric`, `checkTotal`, `hostCounter`, `adoptionTotalFor`); its call site drops the `ParseFloat` error, so a missing series reads 0 with a misleading message. (Task 14 minor 3)
@@ -67,20 +62,12 @@ Left out: everything the phase E final fix wave resolved — the two never-downg
 
 ## internal/handler
 
-- **E-57** `singleflight.go` with `serveCacheMiss`: the strict-wait retry can re-join the call it retries (the leader signals `Done` before deleting the key, deliberately), reading the stale `errUnvouchedRetryable` — at worst one spurious refusal, the pre-phase-E outcome. Observed under CPU load in the final fix wave's load runs (`TestServeHTTP_StrictWaitRefusals`' retry did not coalesce); the test now starts each retry after the finished call left the map. Give the retry a fresh call (mark calls done under `g.mu`, skip a done entry, delete only one's own entry), or say so in serveCacheMiss's AIDEV-NOTE. (Task 9 minor 1; concurrency can-wait)
 - **E-58** `internal/freshness/inflight.go`: the 64-waiter cap is checked before "nothing in flight", so a request arriving as the last adoption ends with 64 waiters still unwinding is refused instead of re-running its lookup. Reorder. (Task 9 minor 3)
 - **E-59** Test gaps: dropping `snapshotCount` from the retryable result survives (no test reads the O1 line's `current_snapshot_count`); removing the hit path's `AdoptionsInFlight` pre-check survives (no hit-path leg has nothing in flight). (Task 9 minor 4)
-- **E-60** `serveCacheMiss` overwrites `shared` and `waiters` with the retry's call: the first call's `singleflight coalesced` line is dropped, and a joiner refused off the first call gets no `unvouched_deb_refused` event. Log attribution only; add the AIDEV-NOTE sentence. (Task 9 minor 6)
 - **E-61** `internal/freshness/inflight.go` `wait` wakes only when a host's count reaches 0, so with overlapping signed adoptions a request whose snapshot committed early sleeps the full bound (documented in configuration.md). Waking on every `end` would respond sooner. (Task 9 minor 7)
 - **E-62** `internal/freshness/inflight.go`: a parameter named `max` shadows the builtin in a file that uses `min`; `maxWait`. (Task 9 minor 8)
-- **E-64** A creating miss whose late registration finds a snapshot current (it waited in a hold, or a flip committed just before it — re-dispatched since the final fix wave, FW-4 (c)) fires T1 twice: `trySnapshotHit`'s and the deferred seed T1. The second is a cooldown skip. Say so in the T1-on-seed comment, or skip the seed T1 when the re-dispatch served. (Task 10 minor 1; concurrency can-wait)
-- **E-65** Unpinned: widening `created` to `|| stored.Create.Outcome == cache.SuiteExists` survives every T1/seed/onboarding test (no test runs two concurrent first-contact misses on different files of one suite); design note 32's "a deadline already past fails the backstop at once" (a floor in `missBackstopDeadline` survives); D-34's "the shutdown cuts both short" for onboarding's backstop. (Task 10 minor 2)
 - **E-66** `seed_t1_test.go` X27 (coalesced creating miss) depends on two misses coalescing within a real 300 ms upstream delay; a flipStack in a bubble with a recording freshness double is deterministic. Passed 20 of 20 under load; same class as the strict-wait window FW-4 (d) closed. (Task 10 minor 3)
-- **E-67** `suite_rows.go`: comments still treat `backstopTimeout` as onboarding's bound (onboarding's backstop runs to its own deadline); `suite_rows_test.go` asserts `backstopTimeout` for the hit leg, passing only because `onboardTimeout == backstopTimeout`; `missBodyBudget`'s doc says it bounds the wait before the body, but only the backstop honours it; "On a timeout the hit serves untracked" holds only for the creation. (Task 10 minor 4)
-- **E-68** `suite_rows.go` `runOnboard`'s `r *http.Request` is unused since D-34 (plan-fixed signature). (Task 10 minor 5)
 - **E-69** `seed_t1_test.go`: `commitSnapshotWhenTracked` repeats the setup `TestServeHTTP_LateRegistrationThatWaitedServesTheNewSnapshot` inlines, and five of the file's tests are not about T1. (Task 10 minor 6)
-- **E-70** `unsigned_posture_test.go`: the `current_snapshot_count` check runs only if an `unvouched_deb_refused` record exists, and nothing requires one — the only thing in leg 1 that catches a coverage read counting unsigned snapshots. Require exactly one record. (Task 12)
-- **E-71** Survivor of the final fix wave (FW-5 (a)): the hit path's `case strictCanceled` is unpinned — reachable only when the request's context ends between its two strict-mode lookups (a dead context fails the first, which returns first). Both sides of that window are pinned. Needs a lookup seam to test. (final fix wave)
 
 ## internal/config and cmd
 
```

```diff
diff --git a/docs/superpowers/plans/2026-09-28-flat-repositories-F-deferred.md b/docs/superpowers/plans/2026-09-28-flat-repositories-F-deferred.md
--- a/docs/superpowers/plans/2026-09-28-flat-repositories-F-deferred.md
+++ b/docs/superpowers/plans/2026-09-28-flat-repositories-F-deferred.md
@@ -25,7 +25,6 @@ Left out: everything fix rounds 1 and 2 and the final fix wave resolved — the
 - **F-9** `suite_lock_test.go:443-445`: the D-24 sentinels depend on slog TextHandler's attribute order and search the whole buffer, not the `clock_step_detected` record. They fail closed today; a JSON handler with `logRecords` (as the E-26 test uses) would check each field. (Task 14 minor 5)
 - **F-10** The DoD simulations sample goroutine bounds only right after a fresh boot or a full stop (`dod_f_test.go`, `dod_f_chaos_test.go`); goroutines that pile up during a boot and exit at its cancel are invisible. Phase F adds no goroutine that could pile up mid-boot. Now that `sampleGoroutines` stops the world before its re-read (final review tests I-1), more samples no longer mean more spurious failures. (Task 15 minor 3)
 - **F-11** `TestDoD_ProductionCadenceAcrossTwelveHourlyRestarts`: gcd's deadline (54 h) is reaped at 55 h by 1-, 2-, 3- and 6-hourly reapers alike, so the test cannot tell hourly runs from 6-hourly ones; `TestReaper_RunsAfterTheUptimeFloorThenHourly` pins the hourly cadence. Move the deadline off the 6 h grid, or credit that test in the plan's DoD map row. (Task 15 minor 5)
-- **F-12** Simulation budget headroom: `TestDoD_NinetyDaysOfReapingWithRestarts` takes 3.75-3.96 s under `-race` on a quiet machine and `TestServeHTTP_MintedSuitesAtScale` (put in a bubble by phase F Task 1) 3.76-3.94 s, against 4.5 s (ruling PE-8; the final review's and the final fix wave's timings). Watch both when phase G adds work to the same simulations. (G) (final review tests m-9)
 
 ## Operability (H: the observability sweep)
 
```

- [ ] **Step 7: Final verification**

Run: `go vet ./... && go test ./... && go test -race -timeout 20m ./...`
Expected: PASS (the plan's run: every package `ok`).

Run: `golangci-lint run ./...`
Expected: `0 issues.`

Run: `gofmt -l .`
Expected: only the five drift files of the Global Constraints.

Run: `go test ./internal/config/ ./cmd/...`
Expected: PASS (the conffile still parses: Step 3 changed a comment only).

Run: `git diff --check HEAD~1`
Expected: no output.

- [ ] **Step 8: Commit**

```bash
git add docs/configuration.md docs/log-fields.md docs/admin-ui-spec.md packaging/config/config.toml.default docs/superpowers/specs/2026-09-27-flat-repositories-design.md docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md docs/superpowers/plans/2026-09-28-flat-repositories-A-deferred.md docs/superpowers/plans/2026-09-28-flat-repositories-B-deferred.md docs/superpowers/plans/2026-09-28-flat-repositories-D-deferred.md docs/superpowers/plans/2026-09-28-flat-repositories-E-deferred.md docs/superpowers/plans/2026-09-28-flat-repositories-F-deferred.md
git commit -m "Document flat repositories; spec revision 10; overview and deferred files for phase G"
```
