# Flat repositories, eager adoption, and suite lifecycle — design

- **Date:** 2026-09-27 (revision 13, 2026-10-26: phase H3, documentation and release — §7 records phase H2's end-to-end suite as built: apt never fetches Jitsi's binary `Release.gpg` (apt 2.2–3.2 refuse a binary detached signature, so the fixture serves an `InRelease` beside it and the cache's side is pinned in Go; H2 deviation 1); the 403 unsigned variant is not adopted within a run (its confirmation takes an hour), and the run checks it is still unadopted after a refusal written 305 s or more after its first observation (H2 deviation 2, as built); "within one cooldown" means carried by the client's own check, before the scheduler could have checked the suite (H2 design note 5); the unsigned first adoption lands at its record's first retry at least `max(cooldown, 5 min)` after the first observation, 5 to 15 minutes in practice (H2 design note 7, ruling R-H2-6); the 45 s commit runs every update under the full error grep, stricter than the scoped grep first planned (H2's third deviation); the error grep also counts signature errors and `E:`/`Error:` lines that name the signed host; and §7 gains the manual live-upstream smoke (`e2e/live`), run green during phase H3's planning against pkgs.k8s.io, NVIDIA CUDA, libnvidia-container, pkg.jenkins.io and download.sublimetext.com; C10's flat consequences are documented in `docs/configuration.md`; C11's metric list is not exhaustive (the 1.1.0 release notes list all 21 new metrics, computed from the code); `flip_held_ms` can exceed 20000 by the hold timer's lateness (ruling R-H3-2); §8 adds the 1.1.0 release notes and the maintainer's open decisions; §9's H row names H3's work; §10 item 12: the maintainer confirmed R-H5 (2026-10-26); after the final review (2026-10-27): §6.1's pre-flight judges pool files, not snapshots — queued anchors deduplicated by hash, and once every one is missing a sample of up to 16 pinned files outside the queue decides; it refuses only when two or more files are checked and none exists, and its message says the database is already migrated (R2 I-1, R5 #1, R5 #2, R7 #2); a quarantined suite re-adopts through C4's Dispatch 3 as built, whatever its reason, an `anchor_corrupt` one at a client's request (R5 #4, R7 #3); C4 states the bound of a refused client check under a not-due record (R1 #3); C5's threat notes say unsigned adoption is not bounded in bytes (R6 #3); C9's sub-job C also runs alone an hour after each start when `gc.interval` is longer (`gc_retired_run_complete`, R3 #2); C10 re-runs the lookup once before refusing with nothing in flight (R1 #2), and a failed pre-download `package_hash` read moves a miss's strict decision after the download (`package_hash_prefetch_lookup_failed`, R4 #2); C11 lists `gc_retired_run_complete`; revision 12, 2026-10-24: phase H1, the sweep — snapshot GC sub-job B never deletes, nor ranks, a snapshot still queued for §6.1's audit (R-H1); an `anchor_corrupt` quarantine deletes, in its own transaction, the `url_path` rows that hold the corrupt `InRelease` blob (`anchor_rows_deleted`), while `anchor_oversized` and `anchor_missing` keep theirs (R-H2, the maintainer's decision of 2026-10-24); the 43 prose log messages this branch added are snake_case events, and only main's released prose messages may stay prose (R-H3, R-H6); after the plan's design reviews, the audit also removes the corrupt pool file after the batch commits (review C-1, re-review N-1: the miss path keeps an existing pool file of the same hash, so an upstream still serving the same `InRelease` was refetched onto the rotted file; `legacy_anchor_discard_failed`), GC's startup pass returns a panic as an error and a panicking pass moves the rotation past itself, and `source_paths_skipped` is INFO; C3's detached-only suite is untracked only when first seen (deferred minor D-52); C5's skipped `Sources` paths are logged (`source_paths_skipped`, GD-2); C7's status page gains `anchorless`, failing checks with the end of their backoff, *Last requested*, `gc.retired_reaped` and the adoption ring's scheme; C9's reaper logs each completed run (`suite_reaper_run_complete`) and sets `acu_suite_reaper_last_run_unixtime`, `suite_reaped` carries `last_requested_unixtime` and `evidence`, the forward-step report counts in `acu_clock_steps_detected_total`, `/forget`'s 409 is logged at DEBUG (`suite_not_forgotten`) and a body that is not a form is 400, and GC recovers its goroutines' panics (`gc_panicked`); C10's strict wait re-runs at once when nothing is in flight, before the waiter cap; C11 lists the new events, fields and gauge; §9's H row names H1–H3; §10 records GD-5, decided for 1.1.0 by ruling R-H5 for the maintainer's confirmation: keep; revision 11, 2026-10-24: the maintainer's G-1 decision — `POST /reconcile` takes `/forget`'s guard, and without `htpasswd_file` both refuse a `Host` that is not an IP address, `localhost` or `admin.listen`'s host name; the read-only endpoints keep no `Host` check; revision 10, 2026-10-20/21/23: phase G plan rulings — a `Packages` file with no SHA256 stanza parses and adopts with incomplete coverage (`no_sha256`), and a body with no stanza naming a `Filename` still fails its parse (R-G1); every writer of a `url_path` row's `last_fetched_at` stamps it after the pool file is final, so the first adoption's `adopted_at` floor stays above the `max(pool mtime, last_fetched_at)` `Last-Modified` of url_path-served metadata, and blob mtimes are not folded into that floor (R-G3); the anchorless gate is signed evidence — the cached signed-anchor re-mark, then never-downgrade's rule — not every sniff-passing anchor row (phase G deviation 1); a plain `Contents-*` that changed is deleted, not kept, in the anchorless refresh (phase G deviation 2); the anchorless body cap is 64 MiB; a replaced row's `last_fetched_at` is `MAX(now, previous + 1)`; `flat_host_root_unsupported` is a WARN, one line a minute for the process with a `suppressed` count; a Filename made only of `.` segments is refused; `suites[].flat`; after the phase G plan reviews (rulings PG-1–PG-18): with `adopt_unsigned` off, a suite the anchorless gate admits is refused as an unsigned candidate is — the record `unsigned_disabled`, a successful check logged `changed_deferred` with form `anchorless`, paced by its record, no index request and no delete (its probes still run) — so a Release-less flat repository is served as first cached; the gate runs before the knob (PG-1, PG-10, PG-17); a Debian pool artifact (`.deb`, `.udeb`, `.ddeb`, `.dsc`, `.buildinfo`, `.changes`, `.diff.gz`, or a name with `_` and a `.tar.` component) is never metadata, so strict mode vouches and refuses an `icons-foo_1.0_all.deb` like any `.deb` (PG-2, PG-16); the sniff counter is `acu_anchorless_sniff_rejected_total{host}`, renamed from C4's first `anchorless_sniff_rejected` (PG-5); a flat suite whose base has no signed evidence is judged junk on its cached anchor rows and root `Packages*`/`Sources*` rows together — junk only when it has at least one such row and every one is junk evidence — while a flat suite with signed evidence, like every `dists/` suite, is judged by its anchors alone; the signed-evidence read is lazy, per candidate, a read error gives no verdict for the run, and the index rows are read only in a run that has an unadopted flat suite (R-G2 as amended by PG-7, R-G5); at most two anchorless bodies are in memory at once for the process (PG-8); an anchorless check is answered — a success that clears the record and the pending change — when an index request got a content answer (a 304, a 200 stored or matching the cached bytes, a 404 or 410) or when the suite has no index row to ask for, and is a failed check otherwise: a sniff-rejected 200, a changed plain `Contents-*`, a 401 or a 403 is no content answer (design review M-7, PG-12, PG-15); the refresh asks `Packages*`, then `Sources*`, then `Contents-*` (M-3), stores only the changed validators of a 200 with the cached bytes, a plain `Contents-*`'s too (M-1), deletes a row only while it holds the blob the refresh read (M-4), and logs its over-cap WARN at most once a minute for the process with a `suppressed` count (M-6); a replaced row's `last_fetched_at` can lead the clock as `adopted_at` can (M-8); the package-hash lookups join the suite's `suite_freshness` row by its key (M-2); C11's constants name the body cap, the two body slots and the two one-a-minute WARNs (M-10); the residuals of C3, C4 and C6 — constraints across a host's tenants, the twins' conflict window, an upstream that answers 403 for a missing key, an empty plain `Packages` — are documented, with no twin tiebreak (PG-13); after the phase G task reviews (rulings PG-19–PG-24): a flat suite active only through a client-created root index row, with no cached anchor row to model its probes on, probes its `InRelease` and `Release` at sibling URLs of that row's stored `upstream_url`, so a non-default port survives (PG-20); the anchorless refresh deletes a cached `InRelease` or `Release` only while it holds the blob read before the probe tree ran, so a client's fetch during the probe is kept (PG-24); a delete that fails is no answer — its 404s and 410s stop counting, so a check they alone answered fails (PG-24); a read error of the refresh's index rows is a local fault, logged with nothing written and no backoff (PG-24); an unchanged plain `Contents-*` answers the check without a sniff, so a captive portal's page cached as one answers it during the outage — a documented residual (PG-24); `flat` is the admin `suites[]` field, not a new log field (PG-23); `flat_host_root_unsupported` is logged only for a host the allowlist admits; a `Packages` stanza's Filename without a SHA256 is counted for `no_sha256`, never validated, and becomes no row, path or log value; after the phase G final review (rulings PG-28–PG-40): the anchorless refresh also deletes a cached `InRelease` or `Release` whose probe answered 401 or 403 when an index request of the same check got a content answer — S3's and CloudFront's answer for a missing key — while 404 and 410 delete it whatever the indexes answered (PG-28); a 200 over the 64 MiB cap drops its row for the miss path, no content answer, and its WARN names the file, replacing C4's first rule, which kept the row, and the fetcher fails a body whose `Content-Length` exceeds the cap without reading it (PG-29), and that WARN is logged only once the delete commits (PG-39); the suffix fallback stops at a dot-cleaned path longer than 5,121 bytes and skips a split whose base is longer than a suite path or whose name is longer than a Filename, which no row can match (PG-30); it runs only while the cache holds a flat `package_hash` row — a flag primed when the cache opens, whose failed read fails the start, set before any transaction that inserts such a row commits and never cleared while it runs (PG-31, PG-36); a never-adopted flat suite's metadata rows are read once, with its anchor rows, before the host slot and the probe tree, for Dispatch 3 and the refresh, so a read fault is a local fault before any request, and a shutdown during that read is no fault (PG-32, amending PG-24; PG-35); `package_hash_conflict` names each row's suite path (PG-33); the anchorless check line counts the rows it replaced and deleted (`rows_replaced`, `rows_deleted`), a failed check's error leads with the decisive reason and names the other after `; also `, and phase G's new WARNs carry `canonical_scheme` (PG-33, PG-37; the pre-G emitter of `freshness: read anchor rows failed` gains it in phase H: PG-34); the PG-1 clause above says no index request (its probes still run); C5 records that `ParseSources` keeps `validateMemberPath` (PG-21); the limits C2 and C6 call documented, and an adopted unsigned flat suite that stops publishing its `Release`, are documented in *Flat repositories* (PG-33, PG-38); revision 9, 2026-10-20: phase F plan rulings — the reaper and `POST /forget` delete a suite row only through phase D's compare-and-delete, holding the suite through the `SuiteGuard`, so one delete path remains (R-F1); `Gate.Purge` keeps a live entry (R-F2); the reaper also reaps legacy junk rows (R-F3); the orphan-marker sweep reads a future `last_seen_at` as now, and a marker is an orphan only when neither its base nor its `/.` twin has a row under either scheme, the rule `/forget`'s marker deletion follows too (R-F5); after the phase F plan reviews (rulings PF-1–PF-9), task reviews (rulings PF-10, PF-11) and the final review (rulings PF-12–PF-15): the sweep never rewrites a stamp (PF-1, phase F deviation 3); `/forget` deletes the cached anchors of the base and its twin under both schemes with the markers (PF-2); junk is a cached anchor that fails its sniff, an `InRelease` passing the `Release` sniff (a plain-text Release) excepted, whatever the suite's record, fetched at least 24 h ago (PF-3); the junk scan runs every run and the candidates reaped regardless of idleness go first (PF-4); with `adopt_unsigned` off an established unsigned suite keeps its coherent snapshot — R-F4's retirement into the ordinary cache is dropped (PF-5); the `/forget` guard stops a cross-site form or fetch, not DNS rebinding, and Host-header validation joins G-1 (PF-6); `/forget`'s answers say what happened and the status page shows each suite's scheme (PF-7); a forward step of the wall clock defers reaping and the sweep by the uptime floor, a suite's held work is bounded at 5 minutes, a reap stamps its base's markers, reaps are counted by reason (PF-8); every suite-row delete — cap eviction, a reap, a forget — moves the base's markers up to `MAX(last_seen_at, now)` in its transaction, so an orphan's year counts from the end of its path's tracking (PF-10, widening PF-8's reap stamp); a hand edit that adds one field to one log line touches that function, not the whole file, for the deferred-minor rule (PF-11); the marker sweep runs once a day; `0s` disables reaping, not the reaper's other duties; the idle cutoff is strict and a NULL or future stamp is never idle; `/forget` takes an optional `scheme`, runs to completion on a context a client cannot cancel, and answers 400, 403 and 500 besides 202 and 409; sub-job C deletes one retired snapshot per transaction without a grace; a re-adoption's `adopted_at` stays above every snapshot of its suite, retired ones included; the 90-day simulation runs the reaper every 6 h and restarts every fifth day, and a four-day simulation runs the production cadence (phase F deviation 2); after the phase F task reviews: a non-canonical suite key deletes no metadata; the sweep judges a future marker against a fresh clock read while its cutoff stays the run's, and the reaper's two clock-step reports are logged directly, outside the one-line-a-minute limit; a panic under a suite's hold is recovered for that suite alone, and a delete whose writer call failed after its commit is a reap; `/forget` refuses a `host` holding a scheme (400), and a shutdown of the admin listener cuts it short (503); after the phase F final review: before it retires a suite's snapshots, every suite-row delete turns never-downgrade's snapshot clause into a marker — when the suite has an adopted, non-retired inline or detached snapshot it upserts its base's marker under the suite's scheme, the one marker a delete creates (PF-12); a forward step the reaper detects never ages a marker (the sweep's cutoff takes off every step detected since the reaper started), and the first sweep is due a day after the reaper starts, so a start with the clock ahead has a day to be corrected (PF-13); the classes reaped regardless of idleness have their own budget of 250 a run, tried first, beside the 25 idle suites (PF-14); the startup GC pass runs no sub-job C (PF-15); a reaped suite's packages lose the version retention its snapshot gave them; a `/forget` panic is logged with its suite (`where=forget`), and a forget whose writer call failed after its commit is logged (`suite_forgotten`, `settled=true`) when a fresh read finds the row gone; revision 8, 2026-10-19: phase E plan rulings — before an unsigned commit the adopter re-marks the suite's cached signed anchors and refuses when it cannot; a refusal inside the commit is a refusal state with its own sentinel and the barrier outcome `refused`; one check tail, and an unexpected success status is a failed check; absent answers reset `check_fail_count`; `anchor_absent` is recorded at the first absent observation, and a future `adoption_fail_first_at` never confirms an unsigned transition; the probe tree's content-key gate, the confirmation details, the interest and client-created-row rules, a 304 only to a conditional request, SHA512 verification before the pool; strict mode waits for signed adoptions only; the scheduler bound's admission order; after the phase E plan reviews (rulings PE-1–PE-8): a probe that finds a signature marks the suite's base, and the re-mark also reads the `/.` twin's rows; never-downgrade reads the markers of both schemes and also refuses while an adopted, non-retired signed snapshot of the base exists; a deferred never-adopted suite and an adopted `anchor_absent` suite are paced by their record; a never-adopted suite whose tree finds no anchor also records `anchor_absent`, and so does a detached suite whose changed `Release` has no signature, unless a failed lateral attempt's record holds, which keeps its reason and paces the suite the same way; a cross-form request records no validators; the interest window is symmetric; the unsigned content key is domain-separated; the unsigned member cap is applied before a slot is taken; strict mode's wait, and the miss path's backstop, end at most 25 s after the request started; the probe bound counts client checks and lost offers; after the phase E task reviews (rulings PE-9–PE-11): the long-run retry simulation names no day count; a suite whose anchor answered absent behind a failed attempt's record is paced by that record; a detached suite's lateral `InRelease` is held only by its own last failure (an in-memory memo) or by the record of its own content, so the detached form's failures never postpone it and a new `InRelease` is attempted at the first check that sees it; a `Packages` stanza without a SHA256 makes coverage incomplete (`no_sha256`) and counts toward the stanza cap; a hit-path client that left during the strict wait is answered `client_canceled`; revision 7, 2026-10-18: phase D plan rulings — adoption accepts exactly apt's first `InRelease` line, and a marker line with trailing blanks decodes; a binary `Release.gpg` passes its sniff only with a signature version (3–6) where its packet header puts the body; suite paths over 1024 bytes are not tracked; cap eviction holds the evictee's mutex through the checker, tries at most eight candidates, evicts at most one suite per creation attempt at or above the global cap (an attempt above it is still refused) and deletes through the compare-and-delete the reaper reuses, and a startup WARN names a table over its caps; the rejection cache holds at most 4,096 suites; `suite_seed_rejected` is rate-limited to one line a minute; the C3 metric names; the hit-path invariant covers tracked suites, and every row creation marks the suite's cached signed anchors in its own transaction; with no adopter a refresh miss re-stamps its row as before; the T1 on a creating miss lands with phase E; an attempt a shutdown ends before the barrier is abandoned like one in it; revision 6, 2026-10-18: phase C plan rulings — the C8 barrier keeps the adoption's global slot and does not honour `adoption.max_duration` but ends on shutdown (the daemon closes it at the signal), so the admission bound's *A* includes it; a hold that held no request starts no cross-suite quiet period; `flip_quiet_period` is `0s` or `1s` to `15s`; every C8 residual has its own counter; the hold-token residual and the five-suite DoD corrected (four hold, the fifth commits unheld); retained by-hash serves SHA512 aliases from phase C; exact C2 predicate and C3 `Release.gpg` sniff wording; after phase C's final review: once cross-suite quiet holds, a hold commits at 15 s (20 s less a 5 s commit margin) instead of answering its waiters from the old snapshot at 20 s — its late readers are a counted residual — and snapshot anchors carry `adopted_at` alone as `Last-Modified`; revision 5, 2026-10-17: slot-driven admission in C4 — a suite that finds no slot waits and is re-checked when one is released; the admission bound — after the phase B plan review; revision 4, 2026-10-05/11: legacy trust audit, signed-seen ordering, fail-closed inline verification, flip hold token, fair scheduler — after the 2026-10-05 plan review, a follow-up review, and a three-lens review of grounding, alternatives and residual risk; 2026-10-17: PR #8 parity notes in C10, §7 and §9)
- **Status:** draft for maintainer review
- **Branch:** `feature/flat-repos` (from `main` @ 536a1ab)
- **Origin:** reimplementation of the problem reported in PR #8 ("Refresh flat
  repositories: give them a suite identity") by Venkat (@venkatamutyala). The
  PR is a reference only; its analysis was reviewed skeptically and several of
  its claims were found false (§3).

---

## 1. Summary

A *flat* apt repository has no `dists/` tree: `InRelease`/`Release`,
`Packages*` and often the `.deb`s sit in one directory
(`deb https://pkgs.k8s.io/core:/stable:/v1.34/deb/ /`). Today the cache never
gives such a directory a *suite*, so its metadata is served from the Phase 1
`url_path` cache forever and no freshness check or adoption ever runs.

This design makes flat repositories first-class, vouches their `.deb`s for
strict mode, supports unsigned, Release-less and SHA512-only repositories, and
fixes the robustness problems flat support multiplies:

| Component | Change |
|---|---|
| C1 Request canonicalisation | One path-canonicality rule; upstream URLs encoded exactly as apt encodes them. |
| C2 Suite identity | Whole-directory flat suites; tightened `dists/` grammar; a leaf path package. |
| C3 Suite rows and signals | One creation choke point with caps; client-interest stamp; signed-seen marker (written at adoption, at startup from cached anchors, in the miss path's writer operation before a signed anchor is served, and by the checker when a probe finds a signature). |
| C4 Freshness check | Probe tree in apt's order; eager first adoption; form transitions; anchorless refresh; adoption capacity; fair concurrent scheduler with a latency bound; clock rules. |
| C5 Adoption input | Flat IndexTargets; `./` and `%` Filenames; SHA512 hash blocks; apt's `Date` rules; R-1 fix; I-1 panic recovery; inline verification accepts only decodable clearsigned envelopes; unsigned form. |
| C6 Flat package coverage | Exact path + Filename-suffix fallback. |
| C7 Retry discipline (limitation #1) | Content-keyed failure record, one backoff schedule, refusal states. |
| C8 Flip consistency (limitation #2) | Two-phase flip barrier bounded below apt's timeout, one hold at a time process-wide; by-hash from retained snapshots; monotonic `Last-Modified`. |
| C9 Suite lifecycle (limitation #3) | Hourly reaper; snapshot retirement; `POST /forget`. |
| C10 Strict mode | Host-wide rule kept; unsigned/anchorless excluded; bounded wait for in-flight adoptions. |

One additive schema migration (v6 → v7) carries all persistent state (§6); a resumable startup audit removes trust from snapshots derived before the R-1 and plain-text-InRelease fixes (§6.1).
Implementation is phased safety-nets-first (§9).

## 2. Goals and non-goals

### Goals

1. Flat repositories in every `sources.list` form apt supports refresh with the
   same snapshot contract as `dists/` suites. Unsigned (`[trusted=yes]`),
   Release-less and SHA512-only repositories refresh too.
2. With `integrity.refuse_unvouched_debs = true`, signed flat `.deb`s are
   vouched against their signed `Packages` hashes with no false refusals for
   any observed real repository layout.
3. Long-term operation is bounded: no suite, retry loop, goroutine queue,
   in-memory map or table grows or churns without bound; restarts, upgrades,
   reaps and clock steps are safe.
4. Every behaviour is proven by tests that assert **both** upper bounds
   (cost, growth) and lower bounds (liveness), including multi-day
   virtual-clock simulations and a real-apt end-to-end suite across apt
   versions.

### Non-goals (§10 lists follow-ups)

- Changing the shipped default `adoption.enabled = false`. **With adoption
  disabled no repository (flat or `dists/`) refreshes its metadata** — the
  documented Phase 1 contract (`TestCheck_NoAdopter_ChangeOnlyLogged`); the
  suite reaper (C9), which runs whatever adoption says, deletes an idle
  suite's cached metadata — as `url_path` GC does, with `gc.enabled = true`
  and a nonzero `gc.url_path_ttl`, for metadata no client asked for within
  `url_path_ttl` (after `hold_packages.window`) — so clients that return
  to a suite after either fetch upstream's files of that time through the
  cache, which then stay as fetched. Every
  refresh mechanism in this design, including the anchorless refresh,
  requires adoption to be enabled; the docs say so prominently.
- Host-root flat repositories (`deb https://h/ /`); multi-segment `dists/`
  codenames.
- Merging the `/repo` and `/repo/.` twin suites (accepted as two suites).
- Synchronous revalidation of anchors on the request path.
- Re-activating a previously adopted snapshot when upstream rolls back
  (rejected: apt itself refuses an older `Date`, C5).

## 3. Background and verified facts

Two review rounds established the facts below: eight analysis agents on PR
#8 and main, then seven design reviews, several running real apt 2.8.3,
3.0.3, 3.2.0 and 3.3.2 against a logging proxy, and probes against a
populated v6 database. Finding IDs I-1, I-4, I-5, I-7, R-1, F-2, B-3, C-1 and
G-1 refer to the 2026-09 security review (`docs/security-review-findings.md`,
maintained outside this branch); S-3 refers to the PR #8 security review.

### apt behaviour (apt source and probes)

| `sources.list` | Metadata requested | `.deb` requested |
|---|---|---|
| `deb <uri>/ /` | `<uri>/InRelease`, `<uri>/Packages.gz` | `<uri>/<Filename>` |
| `deb <uri> ./` | `<uri>/./InRelease`, `<uri>/./Packages.gz` (literal `./`) | `<uri>/<Filename>` |
| `deb <uri> sub/` | `<uri>/sub/InRelease` | `<uri>/<Filename>` — base is the **parent** |
| `deb <uri> a/b/` | `<uri>/a/b/InRelease` | `<uri>/<Filename>` — base two levels up |

- The request path is sent verbatim: no dot-segment normalisation. Filenames
  are encoded with `QuoteString(part, "+~ ")` (also `%`, controls,
  non-ASCII), in lowercase hex, so `pkg_1%3a2.deb` is requested as
  `pkg_1%253a2.deb`.
- apt requests `InRelease`, falls back to `Release` + `Release.gpg` only on
  404 for signed sources; with `[trusted=yes]` any non-transient failure
  (403, soft-404 HTML) falls back. `Release.gpg` is requested even with
  `[trusted=yes]`. With no Release at all, apt probes `InRelease` and
  `Release` (404), then fetches `Packages*` directly and sends
  `If-Modified-Since` for them.
- An InRelease whose first line is not exactly
  `-----BEGIN PGP SIGNED MESSAGE-----` is rejected (no fallback).
- by-hash uses the **strongest declared hash**: `by-hash/SHA512/<hex>`
  whenever the Release declares SHA512, even alongside SHA256.
- `Date:` parsing accepts only GMT/UTC/Z or a zero numeric offset (plus the
  asctime and RFC-850 forms); a missing or invalid Date is 0. apt ignores an
  InRelease whose Date is older than the one it holds and rejects one dated
  more than 10 s in the future. apt requires every hash type common to the
  Release and the file to match.
- HTTP inactivity timeout (`Acquire::http::Timeout`) is **30 s**; a header
  timeout is silently re-sent once and a second one fails the item.
  `apt-get update` then exits 0 with a warning. `Acquire::Retries` defaults
  to 3 only since apt 2.3.2 (Debian 11 / Ubuntu 20.04: 0). Older apt treats
  5xx as an item failure.
- apt uses one connection and queue per host; beyond 10 hosts, and in mirror
  mode for every source, queues are shared, so a held request delays other
  suites' requests.

### Real flat repositories (fetched 2026-09-26/27)

| Repo | Form | Filename shape | Notes |
|---|---|---|---|
| pkgs.k8s.io `core:/stable:/v1.34/deb` | `/ /` | `amd64/kubectl_…deb` | InRelease; no by-hash; CDN answers 403 for missing files |
| NVIDIA CUDA `…/ubuntu2204/x86_64` | `/ /` | `./cuda_…deb` (11,533 stanzas) | `dpkg-scanpackages` shape |
| nvidia `libnvidia-container/stable/deb/amd64` | `/` | `./libnvidia-…deb` | InRelease only; its Release text has only a SHA512 block; `Date` frozen at 2018; Packages has SHA256 |
| pkg.jenkins.io `debian-stable binary/` | `sub/` | `binary/…` and `direct/…` | no InRelease (detached) |
| download.sublimetext.com `apt/stable/` | `a/b/` | `files/…` → `/files/…` | base is the host root |
| download.jitsi.org `stable/` | `sub/` | `stable/…` | `Acquire-By-Hash: yes`, SHA256+SHA512, binary `Release.gpg` |
| apt.fury.io, repo.goreleaser.com | `[trusted=yes] … /` | `…` | no Release at all; Packages only |

### Pre-existing problems this design fixes

- `ParsePackages` rejects `./` and `%` Filenames (`validateMemberPath`).
- Adoption happens only when upstream *changes* after first contact.
- A deterministic adoption failure retries every cooldown forever,
  re-downloading and re-hashing members; validators are never persisted on
  "changed"; the admin "lagging" signal can never fire.
- The flip is atomic per request, not per client; the triggering client is
  always exposed; by-hash requests for the displaced snapshot 404; anchor
  `Last-Modified` from pool mtime can yield a silent 304.
- Nothing deletes `suite_freshness` rows; any 200 for `<dir>/InRelease`
  creates a permanent suite, lock-map entry and periodic poll (I-4).
- S-3: decoded `#`/`?` desynchronise the cache key from the upstream URL and
  mint unbounded `dists/` suites; `%` is double-decoded upstream. R-1:
  reconcile re-parses the raw InRelease envelope. I-1: `clearsign.Decode`
  panics on a zero-plaintext frame with no `recover`. I-7: a backward clock
  step freezes cooldown and scheduler gates.
- Adoption, repair and reconcile block on a global cap of 2 with no deadline
  and no per-host fairness, holding suite mutexes while queued.
- A current-form anchor disappearing freezes the suite while apt falls back.
- by-hash `SHA512` requests 404 on adopted dual-hash `dists/` repos (e.g.
  packages.microsoft.com).

## 4. Decisions (maintainer, 2026-09-26/27)

| # | Decision |
|---|---|
| D1 | Reimplement from scratch on `feature/flat-repos`; credit the PR author; nothing posted to GitHub. |
| D2 | Vouch flat `.deb`s by exact path + Filename-suffix fallback (C6). |
| D3 | Eager first adoption for **all** suites (C4), limited to suites with recent client interest. |
| D4 | Limitation #2: flip barrier + retained by-hash + monotonic `Last-Modified`; no synchronous revalidation (C8). |
| D5 | Also in scope: SHA512 Release files, unsigned Release-only repos, canonical `dists/` suite paths. |
| D6 | Unsigned adoption on by default, safeguarded (C5), knob `adoption.adopt_unsigned`. |
| D7 | `/repo` and `/repo/.` remain two suites. |
| D8 | Release-less ("anchorless") flat repos refresh by revalidating `Packages*` (C4). |
| D9 | `POST /forget` admin escape hatch (C9). |
| D10 | Strict mode waits (bounded) for in-flight adoptions on the host before refusing (C10). |

## 5. Design

### Shared infrastructure

- **Leaf package `internal/aptpath`** (no dependencies beyond the standard
  library) holds `SuitePath`, `IsFlatSuitePath`, `FlatBase`, `DotClean`,
  `AnchorPaths(form)`, the sniffs (C3), apt's encoding and
  `BuildUpstreamURL` (C1). `proxy`, `cache`, `freshness` and `handler` import
  it; `cache` never imports `proxy` (which pulls in config, tlsmitm and
  upstreamproxy).
- **Wall-clock seam `internal/wallclock`.** Every persisted Unix stamp
  (`cache.nowUnix`, freshness `Config.now`, handler and GC stamps) comes from
  `wallclock.Now()`, which is `time.Now()` plus a test-only offset. Durations,
  deadlines and in-memory timing use monotonic `time.Now()`/`time.Since`.
  Simulations step the wall clock backwards and forwards through the offset.
  Production SQL never uses SQLite clock functions (a test forbids
  `strftime`, `CURRENT_TIMESTAMP`, `julianday` and `unixepoch`).
- **Handler fetch seam.** The handler depends on a `handler.Fetcher`
  interface that `*fetch.Client` satisfies, so simulations can drive the
  handler with an in-memory upstream (the real client does not advance
  `testing/synctest` time).

### C1. Request canonicalisation and upstream URLs

1. `proxy.Parse` rejects (`ErrInvalidPath` → 400) a decoded path containing
   NUL or an ASCII control byte (0x00–0x1F, 0x7F).
2. `aptpath.BuildUpstreamURL(scheme, authority, path)` sets `url.URL.RawPath`
   to apt's encoding of the path, extended to every byte Go refuses in a
   `RawPath`: percent-encode, in **lowercase hex**, `+`, `~`, space, `%`,
   bytes ≤ 0x20 and ≥ 0x7F, and `" < > \ ^ `` ` `` { | } # ?`. It asserts
   `u.EscapedPath() == u.RawPath` (a test enforces it for all inputs; in
   production a mismatch increments a counter and falls back to Go's
   escaping). The upstream therefore receives what apt would send it
   directly. This closes S-3's key/URL desync, the double-decoding of `%`
   (`pkg_1%3a2.deb`) and the `+`-as-space problem on S3-style origins. It
   changes the request line for paths containing `+` or `~` (863 of 1,467
   rows in the production sample) to the form apt itself sends.
3. Call sites converted (the only five composers): `proxy.Parse`,
   `buildMemberURL`, the `urlReconstruct` seam's production default,
   `CommitAdoption`'s anchor seed, and hot prefetch; plus the new C4 probes.
4. Tests: an apt-encoding table (`+ ~ space % # ? : @ ./ | { }` UTF-8); every
   path in a fixture extracted from the production `url_path` snapshot
   decodes, after `url.Parse` in the fetcher, to the same canonical path.

**Out of scope (pre-existing):** member URLs use the port-less canonical host
(`AIDEV-TODO(port-correctness)`). New probe code derives sibling URLs from the
anchor's stored `upstream_url` where one exists (it holds the pre-redirect
URL, so this is redirect-safe).

### C2. Suite identity (`aptpath.SuitePath`)

**Canonicality predicate** (both grammars): every segment of the path is
non-empty and not `.` or `..` (one flat exception below), and the decoded path
contains none of `#`, `?`, `%`, `\` and no control byte (0x00–0x1F, 0x7F).

**`dists/` grammar** (tightened, D5): `^/(repo/)?dists/<codename>/<member>`
with a non-empty `<member>`; the suite is `/<repo>/dists/<codename>` as
today. Paths failing the predicate, or ending at the codename, get no suite
and keep Phase 1 serving.

**Flat grammar.** A path containing a `dists` segment is **never** flat. For
other paths `D/<member>` where `<member>` matches exactly one of

```
^(InRelease|Release|Release\.gpg
 |(Packages|Sources)(\.(gz|xz|bz2|lzma|lz4|zst))?
 |Contents-[a-z0-9-]+(\.(gz|xz|bz2|lzma|lz4|zst))?)$
^by-hash/SHA256/[0-9a-f]{64}$
^by-hash/SHA512/[0-9a-f]{128}$
^(Packages|Sources)\.diff/Index$
```

the suite is the **literal** directory `D`. `D` must satisfy the predicate
except that it may end in exactly one `/.` (the `deb <uri> ./` form: suite
`/repo/.`, member `Packages.gz`). `D` must not be the host root (`/`, `/.`,
empty): unsupported, served through Phase 1, and logged by
`flat_host_root_unsupported` — a WARN, at most one line a minute for the
process, whose `suppressed` counts the requests since the last line
(phase G, design note 2), and only for a host the allowlist admits (a
forbidden host's miss is refused 403).

Not covered, documented as served through Phase 1 only: flat Translation
files (apt's flat key is `$(LANGUAGE)`: files named `en`, `en.xz`),
`Contents-*.diff/Index`, `<index>.diff/by-hash/…`, pdiff patch files (unique
names, immutable), DEP-11 and command-not-found (never requested for flat
sources).

### C3. Suite rows, client interest, and the signed-seen marker

**Sniffs** (`aptpath`):

| File | Passes when |
|---|---|
| `InRelease` | the first line is exactly `-----BEGIN PGP SIGNED MESSAGE-----` (trailing `\r`/space/tab stripped) — apt's rule |
| `Release` | a `SHA256:`, `SHA512:`, `SHA1:` or `MD5Sum:` header line appears in the first 64 KiB |
| `Release.gpg` | armor header `-----BEGIN PGP SIGNATURE-----` as the first non-whitespace text (leading whitespace is accepted; leading non-header lines, which gpg tolerates, are rejected), or a binary OpenPGP signature packet: old-format tag byte 0x88–0x8B or new-format 0xC2, whose body — at the offset its length header gives (old format: one, two or four length octets, none for the indeterminate type; new format: one octet below 192, two for 192–223, five for 255, one for a partial length) — starts with a signature version 3–6 (phase D, ruling PD-10: text starting with a UTF-8 lead byte, such as "©" or U+00A0, does not pass) |
| flat `Packages*`/`Sources*` | the compression magic for the extension, or plain text starting `Package:`/`Package: ` (`Sources`: `Package:` too) |

An empty plain `Packages` fails the flat index sniff, so a Release-less
repository that serves only a plain `Packages` and is empty at first
contact gets no suite (Phase 1, never refreshed), and an index that
becomes empty is never stored by the anchorless refresh (phase G,
residual 4).

**Single creation choke point.** `createSuiteTx` (`internal/cache`),
reached through `cache.CreateSuite` (hits) and `cache.StoreFetched`
(misses), is the **only** `INSERT INTO suite_freshness`: create-only (an
existence check in the creating writer transaction leaves an existing row
alone), sets `last_requested_at = now`, and enforces two caps: `freshness.max_suites`
(default 5000) globally and `max_suites / 10` per canonical host (a host's
`http` and `https` suites counted together). At or above the global cap it
first tries to evict the least-recently-requested suite that has no current
snapshot and has been idle ≥ 24 h, through the compare-and-delete helper
phase F's reaper reuses, holding the evictee's suite mutex: the checker is
the cache's guard and takes the mutex with `TryLock`, a busy suite is
skipped, at most eight candidates are tried, and the writer runs no foreign
code. A candidate is held once the table is within eight rows of the cap,
so concurrent creations at the boundary do not refuse while an idle suite
exists; a held candidate the writer finds gone or no longer idle (a client
asked for it in between) makes the creation try once more with a fresh
candidate list (at most two attempts), and a refusal because every listed
candidate was busy is transient (below). An attempt evicts at most one
suite: above the cap (after `max_suites` was lowered, or after an upgrade)
it evicts one and is still
refused while the count stays at or above the cap, so the table converges
under the 24 h rule (phase D, ruling PD-8). The per-host cap never evicts:
a host at its cap shrinks through the reaper (C9, `gc.suite_idle_ttl`, 30
days by default), or when global-cap eviction — whose candidates come from
every host — takes one of its idle suites (with `gc.suite_idle_ttl = 0s`,
only then). The daemon logs WARN `suite_caps_exceeded` at start
when the table is above the global cap or a host is at its cap — at
exactly the global cap a creation evicts, so a full table is normal
operation and logs nothing (phase D, ruling PD-12). An
eviction leaves the evictee's `url_path` rows to `url_path` GC, drops its
lock-map and touch entries and its flip-gate entry unless that entry is
live (a live entry expires on its own; phase F, ruling R-F2), and logs
INFO `suite_evicted`.
Otherwise the creation creates nothing and emits a rate-limited WARN
`suite_seed_rejected` ("suite will not refresh"; one line a minute, field
`suppressed`), the counter `acu_suite_seed_rejected_total{host}` (refusals
that reached the writer) and the unlabelled gauge `acu_suites_seed_rejected`
(the rejection cache's size, swept within a minute). Rejections are cached
in memory per suite key — whichever of the suite's files was refused — for
10 minutes, so an over-cap suite does not cost a writer operation per
request; the map holds at most 4,096 suites (when it is full, expired
entries are dropped and, if it is still full, it is cleared, so a suite can
then be counted again within its 10 minutes). A refusal at the global cap
while every listed eviction candidate was busy (its mutex held by a check,
an adoption or another creation) is transient: it is cached for 1 minute,
and its `suite_seed_rejected` line, under the same once-a-minute limit,
has `detail` "eviction candidates busy; retried after 1 minute" (phase D
final review). A suite path longer than
1024 bytes is not a suite for tracking: classification never treats its
files as suite files, so it gets no row, marker, touch or rejection-cache
entry (phase D, ruling PD-9).

Only the handler creates rows (`StoreFetched` on a miss, `CreateSuite` on a
hit), for a response whose bytes pass the sniff for
the path, on a miss **or** a `url_path` hit (so anchors cached before this
release are onboarded). For the anchor paths (`InRelease`, `Release`) the
row — and, for a signed anchor, the signed-seen marker — is created in the
**writer operation that stores the `url_path` row, before the body is
written to the client**: the miss path holds the whole body before serving,
so the order is sniff → `url_path` + row creation + marker (one
`StoreFetched` transaction) → write body →
T1. Today's handler already seeds its row inside `runFetch` before serving;
this keeps that order. A hit onboards a cached anchor (creates the row) the
same way. Flat index rows may be created after serving. Paths that create a
row:

- with an adopter wired: `<suite>/InRelease`, `<suite>/Release`, and flat
  `<D>/Packages*`/`Sources*` (a sniff-passing `<suite>/Release.gpg` writes the
  marker for a suite that already has a row but creates none: apt fetches it
  only after `Release`). The new row leaves `last_check_at` NULL and the
  handler fires T1 once the response is written (from phase E, §9
  "T1-on-seed"; until then the suite's first check is the next hit's T1 or
  the scheduler's never-checked class), so eager adoption starts within
  seconds of first contact when a slot is free, else as soon as one is
  released (C4 admission). A refresh miss leaves an existing row as it is;
- with no adopter (Phase 1): `<suite>/InRelease` only, stamping
  `last_check_at = last_success_at = now` and the miss validators — today's
  seed, byte for byte, and no T1. A refresh miss of a tracked suite's
  `InRelease` (its cached copy was garbage-collected) re-stamps the row the
  same way — the stamps, the validators, `inrelease_change_seen_at` cleared,
  `check_fail_count = 0` — with an UPDATE in the same writer operation,
  unless the suite has a current snapshot (phase D, ruling PD-6). A
  detached-only suite (`Release` and `Release.gpg`, no `InRelease`) is not
  tracked when first seen: before phase D its `Release` hit's T1 created a
  row through the old upsert, so a newly seen one loses its Phase-1 change
  observations and its admin row (documented); one an earlier release
  already tracks stays tracked — its `Release` serves touch it, so neither
  eviction nor the reaper removes it (phase H1, deferred minor D-52).

A creation by a hit (legacy onboarding) leaves the check stamps and
validators NULL in both modes, so its first check runs at once. A plain-text
"InRelease" fails the sniff, so it seeds no row in either mode. A `url_path`
hit fires T1 only for a tracked suite (after onboarding; phase D, ruling
PD-11).

Everything else writes with UPDATE-only statements: `PutSuiteFreshness`
becomes `UpdateSuiteFreshness` (0 rows ⇒ no-op); `CommitAdoption` Step 6 is
UPDATE-only and rolls back with `ErrSuiteGone` if the row vanished;
`RecordAdoptionFailure`, `ClearAdoptionFailure` and `TouchSuite` are
UPDATE-only. `Check` and `Reconcile` require an **existing row** before
`LoadOrStore` (I-4) and re-validate after `TryLock` that the lock map still
holds the same mutex and the row still exists. `Check` first peeks at the
lock map without storing: a suite whose mutex is held answers busy with no
database read, and its row gate is an existence read (`HasSuiteRow`), so a
burst of T1s on one suite costs about one read (phase D final review). The
handler's by-hash content-addressed fallback requires a known suite (I-4,
handler side). A test proves exactly one INSERT site.

**Client interest.** `suite_freshness.last_requested_at` (distinct from
`url_path.last_requested_at`; an `AIDEV-NOTE` says so) is set by the
creating request (`createSuiteTx`) and otherwise written only by `TouchSuite`
from client-serving paths:

- anchor serves (`InRelease`, `Release`, `Release.gpg`; status < 400,
  including 304, HEAD and stale serves);
- for flat suites, root `Packages*`/`Sources*` serves (Release-less clients'
  anchor probes 404 and are never served);
- vouched `.deb` serves (`DeclaredHashesForPath` returns the suite path), so
  install-only clients keep their suite alive.

Touches are coalesced in memory (at most one write per suite per hour; the
map holds only tracked suites and is purged when a suite row is deleted: a
reap, an eviction or a forget). The
coalescing is monotonic: after a forward wall-clock step of 23 h or more, an
active suite without a snapshot keeps its pre-step stamp for up to an hour
and is evictable meanwhile — bounded and self-healing. A stamp in the future
(a backward step) never makes a suite evictable; the eviction scan reports
it through the rate-limited `clock_step_detected` (C4; phase D, ruling
PD-11).

**Signed-seen marker.** Table `suite_signed_seen(scheme, host, base,
last_seen_at)` where `base` is the suite path with one trailing `/.` removed
(`aptpath.MarkerBase`; twins share it). It records that signed metadata for
the base was seen, and it is the never-downgrade authority for unsigned
adoption (C5). "Signed-seen" is not "adopted": a suite whose signed anchor was
served but never adopted (unchanged anchors start no adoption) still has one.
Every writer upserts keeping the newest `last_seen_at`, and only for suites
that have a `suite_freshness` row. Writers:

- `CommitAdoption`, inside its transaction, for every inline or detached
  snapshot — from phase A, so snapshots adopted by any later binary carry
  their history even before the handler writer exists;
- the startup legacy pass (§6.1): every adopted pre-v7 snapshot the audit classifies
  as signed; and, at **every** start, each tracked suite whose cached
  `InRelease` or `Release.gpg` `url_path` row passes the sniff
  (`last_seen_at` = the newer of that row's `last_requested_at` and
  `last_fetched_at`; anchors `CommitAdoption` seeded have no request stamp).
  This catches signed metadata served by any earlier binary (including
  phases A–C, which have no handler writer) before any check or unsigned
  adoption runs. A never-adopted suite whose anchor row was already deleted
  by `url_path` GC before the upgrade leaves no evidence and gets no marker
  (documented);
- the handler (phase D). On a miss, inside the writer operation that
  stores the `url_path` row (and, on first contact, creates the suite row):
  for a sniff-passing `InRelease` or `Release.gpg` the marker is part of
  that operation, which completes before the body is served. If the
  operation fails the miss fails as it does today (502, no body), so a
  signed anchor body is never written to a client before its base has a
  marker — no timeout path, no residual window. A row creation — by a miss,
  or by a hit (legacy onboarding, or a suite once refused at a cap) — also
  marks, in its own transaction, the base for every sniff-passing
  `InRelease`/`Release.gpg` the suite has cached: an anchor cached while its
  suite had no row earned no marker, since markers are written only for
  suites with rows. The handler reads and sniffs those rows before it
  submits, outside the writer; the transaction re-reads them and marks the
  ones whose blob is unchanged, and a row that changed or appeared in
  between makes the creation unproven: only then does a follow-up writer
  operation sniff and mark it after the commit (a failure is logged and
  healed by the startup pass at the next start; phase D plan, deviation 2,
  ruling PD-5). Serving a tracked suite's anchor writes no marker: every
  cached sniff-passing anchor row of a tracked suite was created by a miss
  under this rule, marked by its suite's creation, seeded by
  `CommitAdoption` (which marks in its transaction), rebuilt by
  `reconstructAnchor` from a snapshot marked at commit or by the audit, or
  created by an earlier binary and marked by the startup pass before this
  binary serves; the blob behind a row is content-addressed, so a row that
  failed the sniff cannot pass it later. The writer is a FIFO queue drained
  by one goroutine (`cache.submitWrite`), so an unsigned `CommitAdoption`
  enqueued after a marker write runs after it, and its in-transaction
  re-read (C5) sees the marker;
- the freshness checker (phase E, ruling PE-5): when a probe finds a
  sniff-passing `InRelease` or `Release.gpg` for a suite without a signed
  current snapshot — a first adoption's probe tree, an unsigned suite's
  upgrade probe, an absent anchor's tree — it marks the base
  (`MarkSignedSeenBatch`, `last_seen_at` = the check's time) before the
  check admits any request; a failed write fails the check. An unsigned
  suite's clients never fetch a signature, so without this writer one only
  the checker saw could be stripped later and the suite adopted unsigned.
  The upgrade probe marks a *present* `Release.gpg` before it probes
  `Release` (phase E final fix wave, FW-1);
- the adopter, before an unsigned commit (C5 safeguard 3, ruling R-E1):
  the suite's and its `/.` twin's cached anchors under both schemes, each
  marked under its own row's scheme. Residual: an anchor cached under the
  other scheme while its suite has no row (refused at the cap) after the
  re-mark ran — served signed during the adoption's flip barrier — earns
  no marker before that commit (C5);
- a suite-row delete (phase F, ruling PF-12), in its transaction, before
  it retires the suite's snapshots: the marker of a base whose suite has
  an adopted, non-retired inline or detached snapshot (below).

A plain-text "InRelease" fails the sniff and never creates a marker. The
marker survives suite reaping: the reaper sweeps a marker only once no
suite row exists for its base or its `/.` twin, under either scheme, and
its `last_seen_at` is older than 365 days (a live suite's marker is never
swept, so no renewal write is needed; a future stamp reads as now and is
never rewritten, C9). Every suite-row delete — cap eviction, a reap, a
forget — moves the base's markers, under both schemes, up to its time
(`MAX(last_seen_at, now)`, in the deleting transaction; phase F, ruling
PF-10), so the year counts from the end of the suite's tracking.
Before it retires the suite's snapshots, the same transaction turns
never-downgrade's snapshot clause (C5 safeguard 3) into a marker, since
a retired snapshot is no history: when the suite has an adopted,
non-retired inline or detached snapshot — a pre-v7 one the trust audit
left queued and unmarked after a pool read fault (§6.1, M-8), for
instance — it upserts the marker of its base under the suite's scheme
(`MAX(last_seen_at, now)`; phase F, ruling PF-12). That is the one
marker a delete creates: a suite without such a snapshot of its own
gets none, whatever its `/.` twin or the other scheme's suite of its
path holds (their snapshots are not retired, and still count in C5
safeguard 3's snapshot clause).
Accepted residual of PF-12: a marker written from a snapshot the audit
had left queued stays if a later audit quarantines that snapshot — a
quarantine deletes no marker — so the base keeps
refusing unsigned candidates (it fails closed) until a suite of the
base, tracked again at a client's request, is forgotten (`POST
/forget`), or, with no suite of the base tracked, the orphan sweep
deletes the marker after its year.
`POST /forget` deletes the base's markers, under both schemes — and the
cached `InRelease`, `Release` and `Release.gpg` of the base and its `/.`
twin under both schemes, which the unsigned commit's re-mark would
otherwise read and mark again (phase F, ruling PF-2) — only if no other
suite row exists for the base or its `/.` twin under either scheme
(phase F, design note 13: never-downgrade reads both schemes' markers).

### C4. Freshness check

Every check runs under the per-suite mutex with a whole-check deadline
(2 min) and short-circuits the probe chain on transport errors. The T1
goroutine and the scheduler `recover()` panics (logged; the check counts as
failed: `last_check_at` is bumped, so the cooldown applies).

**Scheduler fairness (T2, phase B).** A per-check deadline does not bound how
long one suite waits behind others (a sequential loop with 500 two-minute
failing checks ahead of a healthy suite delays it by 16 h 40 min — a host
that accepts connections and stalls or answers late with 5xx; a host that
refuses or ignores connections is already collapsed by the fetch client's
`upstream.unreachable_cooldown` fast-fail, which the probes share). The
scheduler therefore keeps a **per-host queue of due suites**, refreshed every
tick (newly due suites appended, queued ones not duplicated; at most
`freshness.max_suites` entries in all — when that bound cuts a tick's due
list, the least recently checked suites are admitted first, never-checked
ones before any and a future `last_check_at` counting as never, still
queued in key order: phase D, ruling PD-11), served by a fixed pool of
**4 check workers** (constant):

- **across hosts:** at most one check in flight per canonical host; hosts
  take turns round-robin, **one check per turn**;
- **within a host:** three classes, all from persisted columns so they
  survive restarts — *healthy* (`check_fail_count = 0`, checked before), then
  *never checked* (`last_check_at IS NULL`; a new suite is reached by T1 at
  first contact anyway), then *failing* (`check_fail_count` = *k* ≥ 1).
  Inside a class, first in first out by the time the suite was queued (a
  suite re-queued after its check goes to the back), so a suite that becomes
  due later never moves ahead of one already queued and *n* below is fixed at
  queue time. `suite_freshness.check_fail_count` (v7) is the streak of
  consecutive checks that ended *unknown*: the UPDATE that bumps
  `last_check_at` after a failed check increments it, any other result resets
  it to 0 (no extra writer operation); every check, T1 or T2, maintains it;
  only the scheduler reads it;
- **failure backoff:** a failing suite is not due before
  `last_check_at + min(1 h, 2 min × 2^(k−1))`, computed from the persisted
  columns (the clock-step rule applies to `last_check_at`), so a restart
  neither reorders the queues nor makes every failing suite due at once; T1
  checks keep only the cooldown;
- queues persist across ticks, so a long-running batch never delays the
  detection of newly due suites; memory is bounded by the queues and the
  pool.

Adoption, repair and reconcile goroutines are spawned by checks and do not
hold a worker; a suite whose mutex is held (T1, adoption) costs its turn
nothing but a `TryLock`.

**Latency bound** (documented, simulation-tested). Let *H* be the number of
hosts with queued suites, *D* = 2 min the check deadline, and *n* the number
of suites ahead of *S* in its host's queue — only healthy ones when *S* is
healthy. A due suite *S* is checked within

  one tick interval (until it is queued) + (*n* + 1) × ⌈*H*/4⌉ × *D*.

Examples: 500 stalling suites on host A delay a healthy suite on host B by
at most one tick + 2 min (*H* = 2). A healthy suite on host A itself waits at
most one tick + ⌈*H*/4⌉ × 2 min once each of the 500 has failed once —
including after a restart, because the class and the backoff are persisted
and healthy suites go before never-checked and failing ones. A suite's first
failure is paid in the healthy class, once: if *n* healthy suites on a host
start stalling together, its other healthy suites wait up to *n* × 2 min
that one time; afterwards the stalled ones sit in the failing class.
Failing suites share what is left of their host's turns, slowed by the
backoff; they cannot starve healthy ones anywhere.

**Admission bound** (documented, simulation-tested). Admission is
work-conserving: while any suite waits for a slot it could take, that slot
stays idle for at most one check (*D*) plus one host turn (⌈*H*/4⌉ × *D*).
Queued suites therefore pass through the pool at its throughput: *N* suites
queued behind *k* slots they may use are all admitted within one tick +
⌈*N*/*k*⌉ × (*A* + *D*), where *A* — `adoption.max_duration` plus the C8
barrier (≤ 140 s) plus the commit bound — is the longest a slot is held.
Two writer-backlog residuals lie outside *A*: an unsigned adoption holds
its slot through its R-E1 re-mark (C5 safeguard 3, a writer operation
bounded by the commit bound), and a failed attempt through its failure
record (C7, bounded at the commit bound + 30 s); each costs microseconds
unless the single writer is backed up behind another suite's commit, so
the worst case with a wedged writer is *A* + 5 min + 5.5 min (phase E
final fix wave, FW-7; concurrency review M-2).
With the default pool of two and a
first adoption holding the general slot, signed suites go through the
reserved slot back to back. First adoptions (and unsigned ones) run only
when the pool is otherwise empty, so a burst of them is serial, and under
sustained signed load they can wait a long time; `max_concurrent_adoptions
= 3` lets two run at once. A queued suite is probed twice by the scheduler
(the check that queued it and the offered re-check that admits it), plus
once per client check while it waits (at most one per cooldown) and once
per offer another admission takes first (phase E, ruling PE-4). An offered re-check
uses its host's turn like any check, so *n* in the latency bound also counts
the host's waiting suites offered before *S* (at most one per adoption on
that host).

**Response classification** (defined once, used everywhere):

| Class | Responses |
|---|---|
| *present* | 200 whose body passes the file's sniff (C3) |
| *absent* | 401, 403, 404, 410, or 200 failing the sniff |
| *unknown* | transport error, any other status (408, 429, 5xx, other 4xx, unfollowed 3xx), deadline — no form decision; the check fails (bump `last_check_at`) |

Counting 401/403/410 as *absent* is more lenient than apt's signed fallback
(404 only); for signed forms it can only cause a lateral move. An *absent*
answer is not an *unknown* end: it resets `check_fail_count` (phase E,
ruling R-E4). A 304 is *present* only as the answer to a conditional
request (the current anchor, unchanged); to a request without validators
it is *unknown* (phase E, design note 21) — for the probe tree and for
the current form's conditional GET alike, which fails the check rather
than taking it as `not_modified` (phase E final fix wave, FW-3).

**Dispatch** (adopter wired; with no adopter the Phase 1 algorithm runs
unchanged, observation only):

1. **Current snapshot inline** — conditional GET `InRelease` as today;
   changed → C7 gate → adoption request.
   **Current snapshot detached** — also probe `InRelease` each check;
   *present* → inline adoption request (lateral; lateral content whose
   adoption failed keeps its record, which the detached anchor's
   `unchanged` does not clear). The lateral's hold is its own (phase E,
   rulings PE-10 and PE-11): a failed lateral attempt is remembered per
   suite, in memory (its content key and the retry time its failure
   wrote), and only that content is held — until that time, or while it is
   the active, not-due record's content; the detached form's failures
   never extend the hold. So with both forms failing the lateral is
   attempted at most once per lateral retry window and the detached
   content at most twice per lateral window (after each lateral attempt,
   and at its own retry time, which the jitter can put inside the lateral's
   window); a failing lateral `InRelease` is retried within its own window
   (the C7 delay at its failure, up to 75 minutes with jitter), except
   that a detached anchor answering absent behind the detached content's
   record delays it by at most one detached window (that record paces the
   suite, ruling PE-10); a new `InRelease` is attempted at the first check
   that sees it; a restart costs at most one extra attempt per form (phase
   E final fix wave, FW-6: the bounds stated exactly, jitter-tested). Then
   the conditional GET of `Release` as today; a changed
   `Release` whose `Release.gpg` is *absent* records `anchor_absent` and is
   never adopted (phase E, design review M-14), unless a failing lateral
   record holds: that record keeps its reason and schedule.
   **Current anchor absent** (inline: `InRelease`; detached and unsigned:
   `Release`) → the first absent observation records the refusal state
   `anchor_absent` (visible at once; `last_success_at` not bumped;
   `check_fail_count` reset; the snapshot keeps serving; content key
   sha256("anchor_absent"), which no content matches). The record paces the
   suite: while it is active and not due, a check makes no request and the
   scheduler does not queue the suite — the anchor is asked for again at
   the record's retry time, and client checks in between ask nothing
   (phase E, ruling PE-2). Once the record is due and the absence has
   lasted `max(cooldown, 5m)` — two observations, measured from
   `adoption_fail_first_at`, which never confirms when it lies in the
   future (deviation 2) — run the probe tree without validators and adopt
   the signed form found: inline ⇄ detached is a lateral move, and an
   unsigned suite may find a signed form; a signature found for a suite
   without a signed current snapshot is marked first (C3). The current form
   and content found again is `unchanged` and clears the record; an
   unsigned suite's own form with new bytes is its update; for a signed
   suite an unsigned candidate, or nothing, is recorded again — never
   unsigned from signed. A record from an adoption attempt (a failed
   lateral move) keeps its own C7 schedule (phase E, deviation 1). Once a
   check has found the current anchor (or a changed `Release`'s signature)
   absent behind such a record, the record paces the suite as an
   `anchor_absent` record does — no request and no scheduler check before
   its retry time — while the status page shows the attempt's reason (for
   example `gpg_failed`), not `anchor_absent` (phase E, ruling PE-10). The
   check after each attempt asks once more, so the current form's own new
   content waits at most one retry window behind the record (content that
   appears after that check is seen at the check after the next attempt),
   never indefinitely.
2. **Current snapshot unsigned** — probe `InRelease` and `Release.gpg`; if
   either is *present* → mark the base (C3) and make a signed adoption
   request (upgrade): from then on the suite is never adopted unsigned
   (C5 safeguard 3), also after the signature goes away. A *present*
   `Release.gpg` is marked before `Release` is probed for the detached
   request, so a `Release` probe that ends *unknown* (a failed check) still
   leaves the marker, and a `Release` *absent* behind it is the current
   anchor absent (Dispatch 1), not a failed check (phase E final fix wave,
   FW-1). Otherwise
   conditional GET `Release`; changed → unsigned adoption request (no
   re-confirmation; with `adopt_unsigned = false` refused as
   `unsigned_disabled`, whose record paces the suite as an `anchor_absent`
   record does, C5 safeguard 1, ruling PE-12). If a signature is present but the signed adoption
   cannot succeed (key not on the cache host), the suite keeps its last
   unsigned snapshot and the admin page shows the reason; the fix is
   installing the key (or `adoption.accept_any_signer`). A signature probe
   that ends *unknown* fails the check, and a *present* signature whose
   signed adoption is deferred (C7) is `changed_deferred`: an unsigned
   suite is never updated past a signature (phase E, design note 24).
3. **No current snapshot (eager first adoption)** — only if
   `last_requested_at` is within a constant **7-day interest window**
   (independent of GC settings) and the suite has a client-created anchor or
   flat index `url_path` row (`request_count > 0`; never reconstructed). The
   window is symmetric: a `last_requested_at` up to 7 d in the future counts
   as interest, one further ahead is dormant until a client touches the
   suite (design review M-3). Of the anchor rows only `InRelease` and
   `Release` count (apt fetches `Release.gpg` only after `Release`; phase
   E, design notes 13 and 14). A flat suite active only through a
   client-created root index row, with no cached anchor row to model its probe
   URLs on, has its `InRelease` and `Release` probed at sibling URLs of that
   index row's stored `upstream_url` (C1), so a non-default port survives
   (phase G ruling PG-20). A returning client's first request may find
   its suite still dormant (its touch and its T1 run concurrently); the
   next tick checks it, within one tick of that request (M-12). A suite
   whose record is active and not due is paced: while its last check is
   younger than `periodic_refresh`, no probe runs, so client checks ask
   nothing and new content is attempted within one refresh interval (phase
   E, ruling PE-3). Otherwise the probe tree runs without validators (the
   C7 gate compares content keys; phase E, design note 15), and a
   *present* signature marks the base before any request is admitted
   (C3):
   - `InRelease` *present* → inline request;
   - `InRelease` *absent* → `Release`: *present* → `Release.gpg`: *present* →
     detached request; *absent* → unsigned candidate (C5);
   - `Release` *absent* → for flat suites with no signed evidence, the
     **anchorless refresh**: the cached signed-anchor re-mark (C5: each
     sniff-passing `InRelease` or `Release.gpg` row of the base or its `/.`
     twin, under either scheme, marks the base) runs first, then
     never-downgrade's rule (C5 safeguard 3) must find nothing; a cached
     `Release` that passes its sniff is no signed evidence (phase G
     deviation 1), and a read or mark error fails the check. While
     `adoption.adopt_unsigned` is off such a suite is refused instead, as
     an unsigned candidate is (C5 safeguard 1; phase G ruling PG-1): the
     record `unsigned_disabled` (keyed as an `anchor_absent` record is), a
     successful check logged `changed_deferred` with form `anchorless`,
     paced by its record; no index file is requested and nothing is deleted —
     the knob is read after the evidence gate, so a suite with signed evidence
     is the failed check below whatever the knob. Every other suite: a failed
     check, which extends `check_fail_count`: a suite with nothing
     to serve backs off like an *unknown* end (phase E, design note 19), and
     records the refusal state `anchor_absent`, so the status page shows it
     (ruling PE-2);
   - any *unknown* → failed check.
4. **Anchorless refresh** (flat, Release-less suites only), under the check's
   host slot and deadline: conditional GET (stored
   `upstream_etag`/`upstream_lastmod`) of each cached `url_path` row whose
   `SuitePath` equals the suite and whose member is a
   `Packages*`/`Sources*`/`Contents-*` file, `Packages*` first, then
   `Sources*`, then `Contents-*` (a slow `Contents-*` cannot take the
   deadline from the indexes apt needs). Each request holds one of two
   process-wide body slots, taken within the check's deadline and released
   once its body is in the pool or discarded: the fetcher reads a body into
   memory, and per-host slots cannot bound the number of hosts (phase G
   ruling PG-8). A 304 keeps the row. A 200 replaces the row's blob only if
   the body passes the flat index sniff for its extension (a compressed
   `Contents-*`: its extension's magic); else the old row stays (counter
   `acu_anchorless_sniff_rejected_total{host}`, the counter this section
   first named `anchorless_sniff_rejected`: phase G ruling PG-5); a 200 with
   the cached bytes (a plain `Contents-*` too) stores only its changed
   validators. A plain `Contents-*` has no sniff: one whose bytes changed is
   deleted, and the next client miss fetches it (phase G deviation 2).
   404/410 deletes the row; a 200 over 64 MiB deletes it too, for the miss
   path — no content answer, and a WARN naming its path, logged once the
   delete commits (phase G ruling PG-39), at most once a minute
   for the process; the fetcher fails such a body without reading it when its
   `Content-Length` says so (phase G ruling PG-29, which replaces this
   section's first rule: kept, the row's validators never earned a 304, so the
   file froze while every check downloaded the cap again); 401/403, 5xx, a
   transport error and the deadline keep it. A dedicated cache method
   preserves `last_requested_at` and `request_count` and sets
   `last_fetched_at`, only when the blob changes, to `MAX(now, previous + 1)`,
   after the blob is in the pool. Cached `InRelease`/`Release` rows under the
   suite whose probe answered 404/410 are deleted, and so are those whose
   probe answered 401/403 when an index request of the same check got a
   content answer — S3's and CloudFront's answer for a missing key: a stale
   unsigned `Release` kept beside a refreshed `Packages.gz` would fail
   `[trusted=yes]` clients' hash checks for good (phase G ruling PG-28) — the
   probes' own answers, no other request. Every replace and delete applies
   only while the row still holds the blob read before the probe tree ran, so
   a client's newer fetch, one during the probe's window included, is kept.
   Only a content answer counts (phase G ruling PG-12): a 304 to a request
   with validators, a 200 stored or matching the cached bytes, a 404/410 — a
   401, a 403, a sniff-rejected 200 (a captive portal, even one whose page is
   the cached bytes), a changed plain `Contents-*` and a 200 over the cap are
   none. A delete that fails is no answer: its 404s and 410s stop counting, so
   a check they alone answered — no index request, or every content answer a
   delete — fails. Its error leads with the decisive reason — the failed
   delete, or the index requests' own when they got no content answer anyway —
   and names the other one, when there is one. The check is answered —
   `anchorless_refreshed` when a row was replaced or deleted,
   `anchorless_unchanged` otherwise, form `anchorless`, with the counts
   `rows_replaced` and `rows_deleted` — when at least one index request got a
   content answer, or when the suite has no cached index row to ask for (its
   anchors' 404s still apply); when its requests got none it is a failed
   check, which backs off; an answered check is a success that clears the
   suite's failure or refusal record and its pending change. The suite's
   cached index rows are read once, with its anchor rows, before the host slot
   and the probe tree — the same read gives Dispatch 3 its client-created
   index row; a read error is a local fault: logged at WARN, no request at
   all, nothing written and no backoff; the next check tries again (phase G
   rulings PG-24, PG-32). An upstream that answers 403 for a missing key (S3,
   CloudFront) keeps an index the repository removed cached and served (phase
   G, residual 3; a cached `InRelease` or `Release` it answers 403 is deleted
   once an index answers: PG-28); an unchanged plain `Contents-*` counts as
   a content answer without a sniff, so a captive portal's page that a
   client's miss cached as a plain `Contents-<arch>`, and that the portal
   still serves, answers the check (`anchorless_unchanged`) during the
   outage; flat repositories rarely publish a plain `Contents-*` (phase G,
   residual 5). No snapshot, no vouching, no strict-mode
   effect. If a `Release`/`InRelease` later becomes *present*, the suite
   moves to the normal forms. `url_path`-served metadata (a hit, a miss, a
   stale serve) uses `max(pool mtime, last_fetched_at)` as `Last-Modified`,
   so apt's `If-Modified-Since` on `Packages*` sees a replacement; every
   writer of `last_fetched_at` stamps it after the pool file is final, so
   C8's first-adoption `adopted_at` floor stays above it (phase G ruling
   R-G3).

**Adoption capacity.** Adoption, repair and reconcile obey one discipline.
Before spawning the goroutine the check `TryAcquire`s the global slot
(`max_concurrent_adoptions`) and a per-canonical-host slot (one per host). If
either is busy it spawns nothing, records `result = changed_queued` (or
`first_adoption_queued`; repair/reconcile simply skip until the next
unchanged tick) and does **not** advance `last_success_at`. A scheduler
check that ends queued leaves its suite **waiting for a slot** in its host's
queue (still deduplicated: no tick probes it again). Whenever an adoption
releases its global or host slot, the scheduler offers the freed capacity to
waiting suites — hosts in round-robin order, oldest first within a host,
only suites the freed capacity admits under the reserved-slot rule below —
by re-checking the offered suite at once, bypassing its cooldown (one anchor
GET per offer). A re-check that again finds no slot, or finds the suite
busy (its mutex held by a client check or a committing adoption), keeps its
place at the head and waits for the next release or tick; any other result
ends the wait (an *unknown* end sends the suite through the
failing class and its backoff). Waiting state is in memory: after a restart
the first tick rebuilds it from the stale `last_success_at` (one GET per
queued suite). A client-triggered check that is refused leaves the suite
due; the next tick parks it — unless the suite holds a failure record whose
retry is not due (an earlier content's `gpg_failed`, say): persisted state
cannot tell the record's own content from new content a client check found
queued, so such a suite is parked only once it goes stale
(`periodic_refresh`), at the record's retry time (up to an hour, ±25 %), or
by a later client check that wins a slot (the final review's R1 #3; an
in-memory mark that would park it at once is an open decision). Client-triggered checks may take a freed slot
before the scheduler's offer; they are the same work. When `max_concurrent_adoptions ≥ 2`, the last free
global slot is reserved for suites that have a signed current snapshot
(security updates are never starved by first or unsigned adoptions). The
host slot is released when the member pipeline ends (before the C8 barrier);
the global slot is held through the C8 barrier and the commit. Every
adoption, repair and reconcile runs under a context deadline
`adoption.max_duration` (default `30m`); `CommitAdoption` itself runs under
`context.WithoutCancel` with its own bounded timeout, so the deadline cannot
abort a commit already in progress. The C8 barrier's waits do not honour the
deadline either (they are bounded by themselves, ≤ 140 s, and giving up would
discard finished member work), but they end at once on shutdown, and then
nothing is committed. `POST /reconcile` answers 409 when no slot is free.

**Validators.** `suite_freshness` validators always describe the current
snapshot's anchor (set in the flip transaction), or the `url_path` anchor
when unadopted. They are never written from a changed or deferred response
(C7 keeps its own). A suite waiting for a slot is not probed until a slot is
offered, so this rule costs one GET per offer, not one per tick.

**`inrelease_change_seen_at`** is set only when NULL (first sight of a
pending change), cleared in the flip transaction and on
`not_modified`/`unchanged`.

**Clock-step rule** (fixes I-7 in all touched gates). A *past-event* stamp
(`last_check_at`, `last_success_at`, `last_requested_at`,
`adoption_fail_last_at`, `adoption_fail_first_at`, `adopted_at`, and the
`url_path.last_fetched_at` and `suite_signed_seen.last_seen_at` that C9
reads) found in the
future is treated as infinitely old by gates that *delay* work (cooldown, T2
threshold) and as "now" by gates that *retire* things (the reap cutoff, the
junk grace, and the marker sweep, which never rewrites the stamp: lowering
it to a stepped-back now would delete signed history once the clock is
corrected, phase F ruling PF-1). The
exception is the confirmation gaps — the unsigned transition's and the
absent anchor's — where a future `adoption_fail_first_at` never confirms
and re-recording restarts it: reading it as infinitely old would skip a
confirmation (phase E, deviation 2 and ruling PE-1; C4 Dispatch 1, C5
safeguard 2).
`next_retry_at` is only clamped to `now + 1h` (C7). Each detection emits a
rate-limited WARN `clock_step_detected`, except the suite reaper's
reports — the furthest future `last_requested_at` and `last_seen_at`,
each once per change, and a forward step of the wall clock — which it
bounds itself and logs directly (C9). Touch coalescing and all in-memory
timing use monotonic time. Since C8's monotonic stamp, `adopted_at` can
legitimately lead the clock (by 1 s per same-second flip, by up to a
backward step's length after one); no gate reads it as a past event today,
and one that starts to must allow for that. So can a `url_path` row's
`last_fetched_at` after an anchorless replacement (Dispatch 4:
`MAX(now, previous + 1)`): C9's junk grace reads such a stamp as recent —
a future stamp is no evidence (ruling PF-3) — which can only delay a junk
reap.

New `freshness_check` result values: `first_adoption`,
`first_adoption_queued`, `changed_deferred`, `changed_queued`,
`form_changed`, `anchor_absent`, `anchorless_refreshed`,
`anchorless_unchanged`, `upstream_older`.

### C5. Adoption input

**Panic recovery (I-1).** The adoption, repair/reconcile and on-demand
reconcile goroutines `recover()` a panic into `ErrAdoptionPanicked`, record a
failure with reason `panic` for the content key before the suite mutex is
released, and log the stack once. The barrier's `Flip` releases its pending
state through its own `defer`. `panic` records are excluded from the restart
reset (C7).

**Order of checks in `runShared`:** first-line rule (inline) → signature
verification (or unsigned eligibility) → `ParseRelease` → `Date` rules →
candidate insert → member fetch. Refusals therefore cost one verification,
never a member download.

**Flat IndexTargets.** Root-level `Packages*`/`Sources*` (and their
`.diff/Index`) are IndexTargets, fatal on fetch failure, **only when the
suite is flat**: `isIndexTarget`, `indexTargetGroup` and
`archFromFilteredPath` take the suite path. Root `Packages` groups under
pseudo-arch `all` (always required); root `Sources` under `source`. The
handler's `snapshot_index_target_404` alert uses the same layout-aware rule.
The three lockstep regexes keep their shared `AIDEV-NOTE`.

**Filenames.** `ParsePackages` uses `validateFilename` (Packages Filenames
only; `ParseRelease` and `ParseSources` keep `validateMemberPath`: phase G
ruling PG-21):

- allow `.` segments (kept literally: apt requests `<base>/./x.deb`) and `%`
  (literal bytes; C1 re-encodes it upstream; requests are compared after one
  decode);
- reject, fatal as today: empty, absolute, NUL, control bytes, backslash,
  `?`, `#`, `..` segment, empty segment, length > 4096, and a Filename made
  only of `.` segments (it names no file; phase G design note 5).

A `Sources` path `validateMemberPath` refuses — every file of a stanza
whose `Directory` fails it, and each file whose own path does — gets no
row, as before; it is no longer silent: `ParseSources` counts such paths
and keeps the first, and the adoption logs INFO `source_paths_skipped`
once per `Sources` file that skipped any (phase H1, deferred minor GD-2),
also one whose every path was refused, which still fails its parse
(`source_parse_failed`), now naming that cause.

A stanza's Filename without a SHA256 is counted for `no_sha256`, not
validated: it becomes no row, no path and no log value (phase G, the Task 3
review).

**Hash blocks.** `ParseRelease` merges hash blocks per member path, as apt
does: `ReleaseMember` carries `SHA256` and `SHA512`; the size comes from the
SHA512 entry if present, else SHA256; entries with only MD5/SHA1 are ignored
(as today). One shared helper, used by `runShared`, reconcile and repair,
verifies the fetched bytes against **every** declared SHA256/SHA512 hash,
returns the SHA256 of the verified bytes (a *derived* anchor for SHA512-only
members; controlling it needs a SHA512 preimage) and the by-hash alias paths
for **every** declared algorithm. A Packages stanza with a Filename but no
SHA256 makes coverage incomplete (reason `no_sha256`), never a silent drop —
also when no stanza has one: such a file parses and its snapshot adopts
with incomplete coverage, while a body with no stanza naming a `Filename`
(an HTML page) still fails its parse (phase G ruling R-G1).
Limitation (documented): integrity-skipped SHA512-only members are not
recorded for skip-repair; reconcile still heals requestable IndexTargets.
SHA512 is verified before the bytes reach the pool; a SHA512-only member
is never reused from the pool without a fetch (the pool is keyed by SHA256),
and once its fetched bytes verified, an existing pool file under their
SHA256 is re-hashed before it is kept and evicted when corrupt
(`pool_corruption_during_adoption`), as for any reused member; a pooled blob
whose SHA256 matches but whose SHA512 does not fails the member; a
malformed or conflicting SHA512 entry fails the parse; repair recovers
the declared hashes from the snapshot's Release; the by-hash probe uses
the strongest declared hash (phase E, design notes 3–7).

**`Date` rules** (apt's). The verified Release text's `Date:` is parsed with a
port of apt's `RFC1123StrToTime` (GMT/UTC/Z or zero numeric offset; asctime;
RFC-850); missing or invalid ⇒ 0. It is stored as
`suite_snapshot.release_date`. A candidate whose Date is strictly older than
the current snapshot's is refused (`upstream_older`, a refusal state, C7). A
parsed Date more than 10 s in the future is a failure
(`release_date_in_future`). Equal-Date content whose natural key collides
with an adopted, non-current, non-retired snapshot of the suite is refused
(`rollback_refused`). The newest adopted content therefore stays current and
a flapping mirror cannot flip the cache backwards; each flap costs one
signature verification. For repos with an equal or missing Date, rollback
protection reaches back only `gc.keep_displaced` snapshots (documented).

**R-1 fix.** `reparseSnapshotRelease` parses only verified plaintext: the
clearsigned message's plaintext for inline (decoded without crypto; the bytes
were verified at adoption and are content-addressed; decode inside
`recover()`), the `Release` blob for detached and unsigned.

**Anchor forms and storage.**

| Form | Anchor file(s) | Verification | Vouches `.deb`s | Strict-mode participant |
|---|---|---|---|---|
| inline | `InRelease` | GPG | yes | yes |
| detached | `Release` + `Release.gpg` | GPG | yes | yes |
| unsigned | `Release` | none | **never** | **never** |

`suite_snapshot.anchor_form` (backfilled; a trigger rejects inserts with an
empty form). The existing CHECK permits only the inline shape
(`inrelease_hash`) or the detached shape (`release_hash` + `release_gpg_hash`)
and cannot be altered without rebuilding a table four others reference, so
an unsigned snapshot stores its `Release` hash in `inrelease_hash`. The Go
field is renamed `SuiteSnapshot.SingleAnchorHash` and a `Form` field is added,
so the compiler lists every reader; `aptpath.AnchorPaths(form)` gives the
anchor file(s) per form. Readers that change: `detectForm`,
`priorAdoptionForm`, both `cachedHash` derivations, `snapshotAnchorHash`,
`reconstructAnchor`, `CommitAdoption` Step 3c, url_path GC identity guard (d),
`reparseSnapshotRelease`, `HostCurrentSnapshotsCoverage`,
`DeclaredHashesForPath`, admin status. `InsertCandidateSnapshot` sets the
form and its collision lookup includes it and ignores retired snapshots; the
natural key becomes `(scheme, host, suite_path, COALESCE(inrelease_hash,
release_hash), anchor_form) WHERE retired_at IS NULL`.

**Inline verification accepts only clearsigned envelopes** (from phase A).
`VerifyInline` returns `ErrMissingSignature` (`missing_signature`) for a body
without a clearsign marker and `ErrMalformedClearsign`
(`malformed_clearsign`) for a body with a marker that does not decode
(including a missing signature frame), whatever `adoption.require_signature`
says. A body that carries the marker but whose first line is not exactly
the marker — apt's rule: trailing `\r`, space and tab ignored, leading
whitespace or a leading blank line refused — is `malformed_clearsign` as
well, and a marker line with trailing blanks, which apt accepts and
go-crypto's decoder alone does not, is decoded (phase D, ruling R-D2).
Unsigned metadata is adopted only through the explicit unsigned form
below (phase E), never as an inline snapshot. `adoption.require_signature =
false` therefore changes meaning (documented): it only permits starting with
an empty keyring (useful with `accept_any_signer` or for unsigned
repositories). A plain-text "InRelease" fails the first-line sniff, is
*absent*, and the suite is handled as unsigned (or anchorless), so it never
vouches. Real apt rejects such files anyway. This costs no apt-usable
repository anything: apt itself rejects an `InRelease` whose first line is
not the clearsign marker (§3), so the only metadata the old fallback ever
adopted was metadata no apt client could consume; it lands in phase A
because keeping the fallback until phase E protects nothing. `accept_any_signer` is
unchanged: a decodable envelope with a structurally valid signature is still
accepted without cryptographic verification (delegating trust to the
clients), which is distinct from accepting an undecodable envelope. Snapshots
adopted before this change through the old fallback are quarantined at
upgrade (§6.1).

**Unsigned safeguards (D6).**

1. `adoption.adopt_unsigned` (default `true`). `false` refuses every
   unsigned candidate before a slot is taken as the refusal state
   `unsigned_disabled` (phase E, ruling PE-12) — visible, paced by its
   record, never a failed check: a never-adopted suite stays unadopted,
   served from the ordinary cache (its record paces it as C4 Dispatch 3
   does, ruling PE-3); an established unsigned suite keeps serving its
   last snapshot, refused and never updated unsigned, and its record paces
   it as an `anchor_absent` record does (no request and no scheduler check
   before its retry time). Turning the knob back on updates it at its next
   retry; bytes refused this way were never confirmed, so a never-adopted
   suite's transition is confirmed afresh. While the knob is off the cache
   does not keep unsigned repositories fresh: an established unsigned
   suite keeps serving its last snapshot, coherent and frozen, until the
   knob is turned back on or the suite is forgotten (or reaped as idle) —
   forgotten or reaped with the knob still off, it is then a never-adopted
   one — and a never-adopted one is served as the cache first stored its
   files (phase F, ruling PF-5, which drops R-F4's retirement into the ordinary
   cache: there each index file would be frozen at its own first fetch,
   so every upstream publish would bring Hash Sum mismatches that never
   clear). The knob also stops the anchorless refresh (C4 Dispatch 3–4):
   with it off, a Release-less flat repository — the most unsigned there
   is — is refused `unsigned_disabled` and served as first cached, and
   turning the knob on resumes its refresh (phase G ruling PG-1). The knob
   is for caches that serve no unsigned repositories.
2. Eligible only when `InRelease` and `Release.gpg` are both *absent* and
   `Release` is *present*. The **transition into** unsigned (a suite with no
   current snapshot) needs two observations of the same `Release` bytes at
   least `max(cooldown, 5m)` apart for 404/410, or at least 1 h apart when
   either signature file answered 401/403 (WAF and S3-permission incidents);
   the first observation records reason `unsigned_unconfirmed` and the
   comparison uses `adoption_fail_first_at` (restart-safe; the
   confirmation stamp restarts whenever the bytes or the reason change, so
   it is the first observation of these bytes; the retry schedule keeps
   growing while the reason stays the same, so bytes that change at every
   observation are paced like any deferred suite and never confirm —
   phase E final fix wave, FW-3). The hour applies when a
   signature probe of the confirming observation answered 401/403; bytes
   that already passed confirmation (a record for them from an adoption
   attempt) are not confirmed again; a future `adoption_fail_first_at`
   never confirms (phase E, design notes 16–18 and deviation 2). Updates
   of an established unsigned suite adopt at once.
3. **Never downgrade:** refused (`unsigned_downgrade_refused`, a refusal
   state) when a `suite_signed_seen` marker exists for the suite's host and
   base under either scheme (`http` or `https`: a base seen signed over
   `https` gives the same base over plain `http` the same protection; phase
   E, design review M-7), or when an adopted, non-retired inline or
   detached snapshot of the base or its `/.` twin exists under either
   scheme (M-8: also a signed displaced snapshot the audit left queued and
   unmarked while it quarantined the current one). The marker outlives
   reaping, and a suite-row delete writes one for a base whose only
   history is an adopted signed snapshot, before it retires that snapshot
   (phase F, ruling PF-12; the marker stays if the audit later
   quarantines that snapshot, C3); it covers the `/.` twin, and is committed in
   the writer operation that stores a signed anchor, before any client is
   served it (C3). One
   rule, one function (`refuseUnsignedDowngrade`: a marker read and a
   snapshot read) at every point it runs: at eligibility, and again
   **inside the `CommitAdoption` writer transaction**, which refuses an
   unsigned candidate (no pointer change, no rows) when it holds. The suite mutex alone cannot
   close this race: the handler and the `/.` twin establish signed history
   without it. Before an unsigned commit the adopter also re-runs the
   cached signed-anchor mark under the suite mutex: it reads the
   `<suite>/InRelease` and `<suite>/Release.gpg` `url_path` rows of the
   suite and of its `/.` twin (design review I-3) under both schemes (the
   commit counts a marker under either; phase E final fix wave, FW-1),
   sniffs their pool files outside the writer and marks the base, under
   each row's own scheme, for each that passes, before the
   commit is enqueued (FIFO: the commit's re-read sees it). Residual: a
   signed anchor cached under the other scheme after the re-mark ran —
   that scheme's suite refused at the cap (no row, so no marker, C3) and
   served signed during this adoption's flip barrier — earns no marker
   before this commit; the next unsigned attempt's re-mark finds it. A failed mark,
   or a pool read error other than a missing file, refuses the adoption
   (phase E, ruling R-E1) and is logged (`adoption: signed-anchor re-mark
   failed`, WARN; ERROR for a pool read error; M-1) — that refusal means
   "could not rule out a signature". A refusal inside the
   commit is the refusal state, not a failure: its own sentinel, no
   `adoption: commit failed` WARN, no `db_failed`, `adoption_flip_barrier`
   outcome `refused`, also after a barrier that outlived
   `adoption.max_duration` (ruling R-E2). Eligibility is also checked by the
   check, before a slot is taken.
4. At most 1,000 declared members, counted as the distinct paths of the
   merged SHA256 and SHA512 lists (`unsigned_too_large`); the check applies
   it before a slot is taken, and the adoption again before any download
   (phase E, design review M-5).
5. No `package_hash` rows on **any** path (adoption, repair, reconcile:
   `buildPackageHashes` takes the form); `DeclaredHashesForPath` and
   `HostCurrentSnapshotsCoverage` filter `anchor_form <> 'unsigned'`.

Threat notes (documented at the knob): the signed-seen marker cuts both
ways — anyone who can make the cache serve, or a check find (C3), one
sniff-passing `InRelease` or `Release.gpg` for a base (a MITM, a
compromised upstream, or a LAN client pointing the cache at a host it
controls) blocks unsigned adoption of that base until `POST /forget`; this
denial of service is the accepted price of never downgrading. A suite
whose pre-v7 snapshot §6.1 quarantined for `anchor_missing`,
`anchor_corrupt` or `anchor_oversized` may have no marker until a check
sees its signature upstream; if its signatures are stripped before then,
it adopts unsigned (phase E, design review M-9). A MITM on the cache→upstream path can
make the cache adopt forged unsigned content only for a suite never seen
signed — for a never-adopted suite after two consistent observations, for
an established unsigned suite at the first check that sees it (its updates
adopt at once, safeguard 2); verifying clients reject it,
`[trusted=yes]` clients accept it as they would fetching directly. Under the
default allow-all upstream policy with adoption enabled, a LAN client can make
the cache adopt unsigned directories it controls; this is bounded by the
per-host slot, the reserved slot, `max_duration`, the member cap and the suite
caps, but not in bytes: no response-body ceiling and transparent gzip (the
2026-09 security review's A-2 and VA-3, open) leave each member as large as
its server sends within the fetch timeout, and an adopted member stays while
its snapshot is kept, until a reap or a forget (the final review's R6 #3).
Operators who need more can set `adopt_unsigned = false` or narrow
`upstream.allowed_host_regex`.

### C6. Package coverage for flat suites (D2)

**Materialisation.** For a signed flat suite, `C = aptpath.FlatBase(suite)`.
Each stanza with literal Filename `F` yields one `package_hash` row at path
`C + "/" + F` (literal) with `flat_filename = DotClean(F)` (`.` segments
removed; `dists/` rows keep `''`). Coverage uses the `dists/` definition with
the root as the Packages directory (`buildPackageHashes`' `/dists/` gate is
replaced by a flat-aware base). Sources and pdiff hashing are not built for
flat suites (documented). The exact row is a real path for `/ /` and `./`
sources and a harmless phantom for `sub/` and `a/b/` sources.

**Lookup (`DeclaredHashesForPath`)**, current signed non-retired snapshots
only; it returns the suite path with each row:

1. Exact: rows where `path = P` (unchanged).
2. Only if (1) is empty — suffix fallback. `P' = DotClean(P)`; stop if `P'`
   contains `..` or an empty segment, has more than 32 segments, or is longer
   than 5,121 bytes (`aptpath.MaxSuitePathBytes` + 1 +
   `aptpath.MaxFilenameBytes`: no row's base and name are longer; phase G
   ruling PG-30). For each split `P' = B + "/" + F` (`B` may be `""`, the host
   root) — skipping one whose `B` is longer than a suite path (1,024 bytes) or
   whose `F` is longer than a Filename (4,096 bytes), which no row can match —
   query rows with `flat_filename <> '' AND flat_filename IN (F…)` on
   `(scheme, host)` (the `<> ''` makes SQLite use the partial index; an
   `EXPLAIN QUERY PLAN` assertion pins it), join `suite_snapshot` and the
   suite's `suite_freshness` row by its key (never on `current_snapshot_id`
   alone, which has no index; the exact lookup joins the same way: phase G,
   design review M-2), and keep a row iff `B` is an
   ancestor-or-self of that suite's `FlatBase`
   (`strings.HasPrefix(base+"/", B+"/")`). Among kept rows keep those with
   the longest `B` (the most specific base).
3. The existing dispatch applies: 0 distinct hashes → strict-mode predicate;
   1 → validate; ≥ 2 → `package_hash_conflict` 502 (each `declared` entry
   names its suite path; phase G ruling PG-33).

This is apt's rule (`.deb` URL = `<uri>/<Filename>`, `<uri>` an
ancestor-or-self of the Release directory) with one row per stanza. It covers
k8s and CUDA (exact), CUDA's dot-cleaned form, Jenkins `binary/` and
`direct/` (base = parent) and Sublime `files/` (base = host root). Rows are
constraints, not grants: a match only lets bytes through if they hash to a
signed declaration. Parent-base repos pay the fallback query on every `.deb`
hit. The fallback runs only while the cache holds a flat `package_hash` row
at all — a flag primed when the cache opens and set before any transaction
that inserts one commits, never cleared while it runs (phase G ruling
PG-31) — so a cache with adoption off or without flat suites pays the exact
lookup alone.

Two residuals of the rule (phase G ruling PG-13). A signed flat suite
constrains every `.deb` path that has no exact row and that its Filenames
name under any ancestor of its base — a sibling directory too (Jenkins'
`direct/` is one) — so on a multi-tenant host (OBS projects, a host behind
`accept_any_signer`) one tenant's signed `Packages` can make another
tenant's `.deb`, which no signed snapshot of its own vouches, fail
validation (502), even without strict mode; no bytes can be injected. And
the twins' exact rows share every path: a Filename upstream overwrites in
place with new bytes carries two distinct hashes between the two twins'
adoptions, so both source forms get `package_hash_conflict` (502) until
the second twin adopts — at its next check, at most one refresh interval
later unless that adoption fails (no tiebreak: a documented, bounded
window).

**Consumers.** Hit validation, miss validation and strict mode go through
`DeclaredHashesForPath`. Hot prefetch and the retention mirror rule join on
the exact path, so they cover flat repos whose `.deb`s sit under the suite
directory (k8s, CUDA, OBS); parent-base repos (Jenkins, Sublime) are
retained by recency and hold-grace like unvouched files (documented).

### C7. Retry discipline (limitation #1)

**Failure record** (v7 columns on `suite_freshness`): `adoption_fail_hash`,
`adoption_fail_reason`, `adoption_fail_count`, `adoption_fail_first_at`,
`adoption_fail_last_at`, `adoption_next_retry_at`, `adoption_fail_etag`,
`adoption_fail_lastmod`. UPDATE-only statements; written **before** the suite
mutex is released; an attempt a shutdown ends — in the flip barrier or
before it — is not recorded, not counted as an adoption outcome and logged
at INFO only (`adoption_abandoned_at_shutdown`; phase D, deferred minor
C-20). `count` and `first_at` describe the failure streak since the last
success.

**Content key.** inline: sha256(InRelease); detached: sha256(sha256(Release)
‖ sha256(Release.gpg)); unsigned: sha256("unsigned\n" ‖ Release), domain-separated
from an inline key over the same bytes (phase E, design review M-2); an `anchor_absent` record:
sha256("anchor_absent"), a constant no content matches (phase E). Any byte
change is new
content and is attempted at once (subject to slots). A re-signature alone of
a detached suite is noticed at the next retry (≤ 1 h), because a 304 on
`Release` does not fetch `Release.gpg`.

**One schedule.** `next_retry_at = now + min(2m · 2^(count−1), 1h)` with
±25 % jitter (seeded seam), clamped on read to `now + 1h`. The `%w`
prerequisites (natural-key collision, commit failure, `readPackagesBlob` I/O)
and distinct sentinels make reasons precise for operators; the schedule does
not depend on classification. Acknowledged cost: a transient member failure
(or a MITM withholding a member) can defer an available update of the *same*
content by up to 1 h, where today it retries every cooldown while
re-downloading everything.

**Refusal states** use the same record and schedule but are not failures:
`upstream_older`, `rollback_refused`, `unsigned_downgrade_refused`,
`unsigned_unconfirmed` (confirmation timing from `adoption_fail_first_at`;
its confirmation stamp, like `anchor_absent`'s, restarts on new content or
a new reason, while the retry schedule keeps growing until the reason
changes — phase E final fix wave, FW-3),
`unsigned_too_large`, `anchor_absent`, `unsigned_disabled` (C5 safeguard
1, ruling PE-12). They are excluded from the
failing-suites gauge and from `adoption_failing_persistently`, and shown as
"refused: …" in the admin page.

**Gate** (in every branch that would spawn an adoption). While
`now < next_retry_at` the check sends the *failing content's* validators: a
304 or a 200 with the same key ⇒ `changed_deferred` (no clear, no repair);
new content ⇒ attempt. At or after `next_retry_at` the check GETs without
validators and attempts. A record's validators are those of the anchor
file its request fetched: a cross-form request (a detached suite's lateral
`InRelease`, an unsigned suite's upgrade probe, an absent anchor's tree)
records none, and while a record without validators is active and not due
the check sends the current anchor's own; a 304 is `changed_deferred` only
when the record's validators were sent, otherwise `not_modified` (phase E,
design review I-1). The C4 probe tree sends no validators: there the gate
compares content keys only (phase E, design note 15). Some records pace
their suite instead of gating a request: an adopted suite's
`anchor_absent` or `unsigned_disabled` (no request before its retry time;
rulings PE-2 and PE-12); an adopted suite's record from a failed attempt
once a check found the current anchor, or a changed `Release`'s
signature, absent behind it (the same pacing while the record keeps its
own reason; ruling PE-10); and a never-adopted suite's active record (no
probe before its retry time while its last check is younger than
`periodic_refresh`; ruling PE-3) — C4 Dispatch 1, 2 and 3. The record is
cleared in the flip transaction and when upstream again serves the
current snapshot's content.

**Restart.** At checker start every failing row (not refusal states, not
reason `panic`) gets `next_retry_at = now + rand(0, periodic_refresh)`: one
retry per failing suite, the recovery path after an operator installs a key
or changes pins.

**Visibility.** Admin `suites[]` gains `anchor_form`, `flat`,
`last_requested_unixtime`, and the record ("adoption failing 3d 2h ·
untrusted_signer · retry in 41m", or "refused: upstream_older"). Phase H1
adds `anchorless` — a flat suite refreshed without a `Release`, derived
from persisted state alone: a flat suite, adoption enabled, no current snapshot, a last
success, no record, no pending change (C4 Dispatch 4) — shown as
"refreshed without a Release"; "checks failing ×k · backoff ends in …"
while `check_fail_count` > 0 (the scheduler's backoff from `last_check_at`:
the earliest it may check; the count alone for a dormant never-adopted
suite and one its record paces, which the backoff does not schedule); a
*Last requested* column; `gc.retired_reaped`; and the adoption ring's
`scheme`, shown as `scheme://host`. "Lagging" is
computed from the first-seen `inrelease_change_seen_at` against `now`. Gauge
`acu_adoption_failing_suites`; INFO `adoption_deferred` on state transitions;
WARN `adoption_failing_persistently` at streak counts 3, 6, 12, …; the
per-attempt WARN `adoption_run_failed` remains. Reason strings stay on the
`html/template` path.

### C8. Flip consistency (limitation #2)

New package `internal/flipgate` (channels, timers, monotonic time; never waits
while holding its mutex), keyed by the freshness suite key. Tokens reference
the entry *instance*, so reap purges and re-creations cannot confuse them;
all releases are deferred.

**Handler side.** Before serving an anchor (`<suite>/InRelease` or
`<suite>/Release`, for suites with a row) the handler calls
`t := gate.AnchorStart(ctx, key)`, which returns immediately unless a flip is
pending; then it waits at most **20 s** (a constant below apt's 30 s
timeout), honouring the request context, handler shutdown and a per-suite
waiter cap (256; beyond it, no wait). It runs **before** host-slot
acquisition and singleflight. After the (re-read) response is written,
`t.Done()` records the completion time and decrements the suite's in-flight
count. `Release.gpg` is never held.

**Adopter side.** `gate.Flip(ctx, key, commit)`:

- **Phase A — natural quiet, no holds.** Wait until (a) the suite has no
  anchor in flight (anchors in flight longer than 30 s, apt's own timeout,
  are ignored) and none completed within `flip_quiet_period` (default `10s`,
  `1s` to `15s`), and (b) **no hold is in progress anywhere** and none
  that held at least one request ended within the same period (such a hold
  delayed other suites on the client's shared queue; one that held nothing
  delayed no one). Bounded by 60 s; on timeout proceed to phase B. A
  phase A that finds quiet still takes the hold token and holds (a
  zero-length hold in the common case): the commit always runs inside a
  hold, so no anchor serve can read the old pointer between the quiet check
  and the commit. Never optimise the quiet path into a direct commit.
- **Hold token.** Before phase B the flip acquires the process-wide hold
  token (FIFO; at most one suite holds at a time). Waiting for it is bounded
  by 60 s; on timeout the flip commits without a hold (counter
  `acu_flip_hold_skipped_total`). Concurrent holds are what let one suite's
  hold push a shared client queue past another suite's quiet window (two
  busy suites holding at once, each committing on its own local quiet).
- **Phase B — hold.** Mark the flip pending (new anchor requests wait). Wait
  until (a) no anchor of this suite is in flight and none completed within
  `flip_quiet_period` (reachable, because new anchors are held), and (b) no
  *other* suite's hold that held at least one request ended within
  `flip_quiet_period` (clients released from that hold may still have this
  suite's index requests queued behind it). Then run `commit`, clear
  pending, wake waiters onto the new snapshot and release the token. Once
  (b) holds, the wait for (a) is cut short at 15 s from phase B's start
  (20 s less a 5 s commit margin, the time a commit normally needs): the
  commit starts then, inside the hold, so the waiters still get the new
  snapshot; anchors still being sent, or finished within
  `flip_quiet_period`, at that moment — the late readers — are a residual
  (counter `acu_flip_quiet_cut_short_total`). Because `flip_quiet_period`
  ≤ 15 s = 20 s − 5 s and holds never overlap, (b) always holds by then.
  The whole of phase B, including both waits, is bounded by 20 s from its
  start: at expiry — the backstop for a commit that has not started or not
  finished by then — pending is cleared, waiters are answered from the
  current snapshot, a commit not yet started starts immediately (counter
  `acu_flip_hold_expired_total`), and the token is released. A flip
  therefore takes at most 60 s + 60 s + 20 s plus its commit. (Answering
  the waiters from the old snapshot at 20 s and committing right after —
  this rule before phase C's final review — made nearly every held client
  fetch the new indexes for its old anchor: one slow reader made a flip
  about 20 times worse than no barrier.)

Skipped when `flip_quiet_period = 0s` (today's behaviour) and for a first
adoption whose anchor bytes equal the `url_path` anchor clients have been
served (the flip does not change what they hold; the skip still honours the
shutdown signal, so it commits nothing once the daemon is stopping). A
first adoption stamps
`adopted_at = max(now, url_path anchor last_fetched_at + 1)` so clients'
`If-Modified-Since` still sees a change.

**Guarantee.** A client that received the old anchor finished receiving it at
least `flip_quiet_period` before the flip, or was held and answered from the
new snapshot, so its index requests within that period come from the snapshot
matching its anchor. Because holds never overlap and every held commit
waits a quiet period after the last hold that held a request ended (only a
commit that timed out waiting for the hold token, which holds nothing, skips
that wait; it is a residual below), the
proxy's own holds cannot push a client's queued requests for another suite
past that suite's flip. Residuals (documented, each with its own unlabelled
counter): the quiet period cut short at 15 s into a hold, when an anchor
response of the suite is still being sent then or finished less than
`flip_quiet_period` before — a read still running `20 s − 5 s −
flip_quiet_period` (5 s by default) into the hold, such as a slow or
stalled client — whose client then fetches its indexes from the new
snapshot (`acu_flip_quiet_cut_short_total`; the held clients are answered
from the new snapshot); phase-B expiry when the commit has not finished by
20 s (F-2 on very large suites): the waiters are answered from the old
snapshot and may fetch new indexes for it (`acu_flip_hold_expired_total`);
hold-token timeout (a flip times out only behind 60 s of holds: at least
three holds at their 20 s bound, which needs commits slower than the 5 s
margin, in practice four or more, since a queued flip's suite is usually
busy and its hold then lasts at least the quiet period and up to 15 s plus
its commit — the five busy suites of §9 give four holds and one skip — so it
takes at least four suites flipping at once, which needs
`max_concurrent_adoptions` of 4 or more, or 0 for unlimited, because a flip
keeps its global slot through the barrier; `acu_flip_hold_skipped_total`),
waiters beyond the cap (`acu_flip_waiters_over_cap_total`), and anchor
reads slower than apt's own timeout (`acu_flip_slow_anchor_reads_total`).
The gauge `acu_flip_barrier_active` counts the flips in the barrier now,
and a held request's `request` line carries `flip_held_ms` (about 20000
at most: the hold's timer can fire a few milliseconds late, ruling
R-H3-2). A flip waiting
for the token keeps its global adoption slot, so while two or more suites
flip other adoptions can queue for up to about 140 s (bounded).
by-hash clients remain covered by retained by-hash. Answering held requests
with `503 Retry-After` instead was rejected: apt before 2.3.2 treats a 5xx as
an item failure without retrying, while answering from the current snapshot is
today's behaviour. Map entries are created only for suites with rows and
expire when idle for twice the quiet period; a suite-row delete (a reap,
an eviction, a forget) purges its suite's entry unless it is live — a
flip, a hold, an anchor serve in flight, or any use within the quiet
period — which then expires on its own (phase F, ruling R-F2).

**Retained by-hash.** When `trySnapshotHit` misses the current snapshot for a
`by-hash/SHA256|SHA512/<hex>` member, it looks the alias up in the suite's
other adopted, non-retired snapshots (newest first; at most
`gc.keep_displaced + 1`) and serves it with that snapshot's
`X-Cache-Snapshot`. SHA256 aliases must match the served blob hash; SHA512
aliases rely on the verified `snapshot_member` binding. No recovery fetch for
displaced snapshots. Counter `acu_serve_byhash_displaced_total{host}`.
`gc.keep_displaced = 0` disables it (documented).

**Monotonic `Last-Modified`.** `CommitAdoption` stamps `adopted_at =
max(now, prior.adopted_at + 1)` (first adoption: see above), and
snapshot-served anchors (`InRelease`, `Release`, `Release.gpg`, also on the
member-recovery path) use the serving snapshot's `adopted_at` alone as the
`ServeContent` modtime — the pool file's mtime only when `adopted_at` cannot
be read (logged) — so a client's `If-Modified-Since` from the previous
anchor always gets a 200 after a flip, whatever the clock or a recovery
rewrite of a pool file does — within one residual of suite reaping (C9):
once sub-job C has deleted a reaped or forgotten suite's retired
snapshots, a backward clock step longer than the time from the reap to
the re-adoption can stamp the re-adoption below the reaped snapshot, and
a client still holding that anchor gets a 304 until a later flip is
stamped above it (a durable per-suite floor needs a schema change).
(`max(pool mtime, adopted_at)`, this rule
before phase C's final review, let a pool file written before a backward
clock step, or rewritten by a recovery, carry a `Last-Modified` the next
flip's stamp did not exceed: 304s and stale lists until a later flip.)
`Last-Modified` can lead the response's `Date` (by 1 s after two flips in
one second, by up to a backward step's length after one); apt only echoes
it. `adopted_at`'s other readers (GC ranking per suite, admin display) need
only its order; see C4's clock-step rule.

The README's "clients always see a coherent metadata set" claim is replaced
with this guarantee and its residuals.

### C9. Suite lifecycle (limitation #3)

**Snapshot retirement.** `suite_snapshot.retired_at` marks every snapshot of a
suite incarnation that was reaped, evicted at the global cap or forgotten,
in the same transaction that removes the suite row (`deleteSuiteTx`).
Retired snapshots are invisible to everything that
reasons about a suite's history: the natural-key uniqueness (partial index
`WHERE retired_at IS NULL`), `InsertCandidateSnapshot`'s collision lookup,
`rollback_refused`, retained by-hash, `DeclaredHashesForPath`,
`HostCurrentSnapshotsCoverage` and snapshot-GC sub-job B (whose
`keep_displaced` ranking they leave). A returning or
forgotten suite therefore re-adopts unchanged upstream content at once, and a
deliberate rollback adopts after `/forget`. Only the adoption stamp still
sees them: a re-adoption's `adopted_at` stays above every snapshot of the
suite, retired ones included, so C8's `Last-Modified` never goes back
while they remain (phase F, deferred minor C-11; once sub-job C has
deleted them, C8's residual applies).

**Reaper.** Its own hourly ticker under the freshness checker's lifecycle
(independent of `gc.interval`, of `gc.enabled` and of the T2 loop), after a
monotonic uptime floor of 1 h; each run starts an interval after the
previous one finished. A run tries at most 250 of the suites reaped
regardless of idleness (non-canonical and junk rows, below) first, then,
in the same run, at most 25 idle suites, the oldest `last_requested_at`
first (constants; a busy suite counts against its own class's budget;
phase F, rulings PF-4, PF-14: the regardless classes are cheap reaps —
no snapshot, at most three anchor rows for a junk suite, no metadata for
a non-canonical row — and an upgraded table can hold thousands of them,
which one shared budget of 25 would drain over days with no idle reaping
meanwhile). Candidates:
`last_requested_at < now − gc.suite_idle_ttl` (default `720h`; `0s` disables
reaping; a NULL or future stamp is never idle, and the furthest future one
is reported through `clock_step_detected` at the first run after a start
and whenever it changes — logged directly, outside the checker's
one-line-a-minute limit, and counted),
plus rows whose `suite_path` is non-canonical under the tightened grammar
(unreachable legacy rows, reaped regardless of idleness), plus legacy junk
rows, also reaped regardless of idleness (phase F, rulings R-F3, PF-3): a
suite without a current snapshot whose cached anchor rows (`InRelease`,
`Release`, `Release.gpg`) — at least one — all hold a blob fetched at
least 24 h ago (a NULL or future stamp is not old) that is junk evidence:
its head fails its own sniff and, for an `InRelease`, the `Release` sniff
too. A plain-text Release cached as `InRelease` is the §6.1-quarantined
plain-text suite that adopts unsigned through `Release` (C5), not junk;
any other sniff-failing anchor is evidence whatever the suite's adoption
record — a failed adoption's record would otherwise keep an HTML anchor
served for good, since an unadopted suite's metadata is never
revalidated. A flat suite whose base has no signed evidence
(never-downgrade's rule, C5 safeguard 3, finds no signed-seen marker for
its base and no adopted signed snapshot of it or its twin) is judged on
its cached anchor rows and its cached root `Packages*`/`Sources*` rows
together, by the same rules
(the flat index sniff for an index row's name, fetched at least 24 h ago,
whatever the record): junk only when it has at least one such row and
every one is junk evidence. A flat suite with signed evidence, and every
`dists/` suite, is judged by its anchors alone — where that rule heals
something, a signed suite with a junk anchor; under it a healthy
Release-less suite on a host that answers its anchors with a 200 page
would be reaped every day, losing its good index during an outage. The
signed-evidence read runs only for a suite the two rules judge
differently, and a read error gives that suite no verdict for the run; the
index rows are read only in a run that has an unadopted flat suite, and a
failed read gives every such suite no verdict for the run (phase G rulings
R-G2 as amended by PG-7, R-G5). A missing or unreadable pool file is no
evidence; the sniffs
read at most 64 KiB per anchor on the reaper's goroutine, and each verdict
is kept per anchor blob until the next scan. For each candidate: hold the
suite through the checker's `SuiteGuard` (`HoldSuite` `TryLock`s the
suite mutex; busy → skip, retried next run) and re-read its row (I-4: a
row gone since the listing has its lock-map entry dropped; a suite asked
for, or adopted, since the listing is left whole — up to this re-read: a
client that asks for it between the re-read and step 2 keeps the row,
but not the metadata step 1 deleted, so a client of a suite without a
snapshot can see one Hash Sum mismatch in mid-update (the M-7 residual
`/forget` documents) and an adopted suite's next check rebuilds its
anchor row (WARN `freshness: metadata anchor missing for adopted suite;
reconstructing`); junk evidence is not
read again), then, within 5 minutes (a step cut short is retried next
run; a panic under the hold is recovered for that suite alone — logged,
the hold released as the cache then answers — and the run goes on):

1. delete the suite's metadata `url_path` rows — rows where
   `aptpath.SuitePath(path) == suite` (never a twin's or nested suite's) —
   plain metadata first, anchors last, in batches of 1,000 rows per writer
   transaction without touching the suite row;
2. in one final writer transaction `cache.ReapIdleSuite`, through phase D's
   compare-and-delete (`deleteSuiteTx`): re-read the row and
   bail if the stamp is newer than the cutoff or `current_snapshot_id` changed
   since the candidate was listed; upsert the base's marker when the suite
   has an adopted, non-retired inline or detached snapshot (PF-12, C3);
   run the Step-8 refcount
   decrement for the current snapshot; set `retired_at` on all of the suite's
   snapshots; delete the `suite_freshness` row; move the base's markers
   up (PF-10). A failed writer call is settled by a fresh read of the
   row: gone is a reap (`suite_reaped` with `settled=true`).
   `deleteSuiteTx` holds the only production `DELETE FROM
   suite_freshness`, and only cap eviction, `ReapIdleSuite` and
   `ForgetSuite` call it (phase F, ruling R-F1).

The reaper deletes no package row, but a reaped suite's cached packages
lose the retention its current snapshot gave them — `url_path` GC's
mirror rule ranks the newest `retention.max_versions_per_package`
versions within current snapshots only (a package another current
snapshot lists keeps that one's) — so with `gc.enabled = true` and a
nonzero `gc.url_path_ttl` GC deletes those no client requested within
`gc.url_path_ttl`, once `hold_packages.window` has passed, and their pool
files after `gc.blob_grace`: the intended disk reclamation (phase F
final review, spec I1).

Then `release(true)` purges in-memory state: the lock-map entry
(`CompareAndDelete`), the checker's per-suite maps and the flip-gate
entry; the cache itself drops the touch-coalescing entry once the row is
gone. `Gate.Purge` skips a live entry — a flip in progress, a hold, an
anchor serve in flight, or any use of the entry within the quiet period
(a serve completed, a hold or a flip ended) — which expires on its own,
so a release racing a re-created suite's serve never shortens that
serve's quiet period (ruling R-F2). Each
run also scans for junk rows (every run, ruling PF-4: one indexed read
that probes `url_path` by its key, pool reads only for blobs not yet
sniffed) and sweeps the reconcile memo (against the current snapshot ids
it listed). Once a day — at the first run a day or more after the reaper
started, then a day apart (monotonic; phase F, ruling PF-13: a start
with the wall clock ahead, which the forward-step guard cannot see, has
a day to be corrected) — it sweeps the signed-seen markers whose base
and `/.` twin have no `suite_freshness` row under either scheme and
whose `last_seen_at` is older than the cutoff `now − stepAhead − 365 d`,
`now` being the run's clock and `stepAhead` the sum of the forward steps
the reaper has detected since it started (PF-13: a detected step never
ages a marker; after a genuine suspend markers are only kept longer),
each delete re-checking both in its writer transaction. The sweep is a
gate that retires things (C4 clock-step rule): a future stamp reads as now, so its marker is never
swept; the sweep never rewrites it and reports the furthest through
`clock_step_detected` at the first sweep after a start and when it
changes — judged against a fresh clock read, since the run's own deletes
move markers up while it runs (the cutoff stays the run's), and logged
directly, outside the one-line-a-minute limit (rulings R-F5, PF-1). A
forward step of the wall clock that no restart re-arms — the wall clock more than 1 h ahead of the monotonic
clock between two runs, as when a VM or laptop resumes — is reported
(`clock_step_detected`, `stamp=wall_clock`), defers reaping and the
sweep by the uptime floor (ruling PF-8), and is added to `stepAhead`
for the rest of the process (PF-13): the hour gives clients time to
touch their suites, but no client touches an orphan marker, and a swept
marker cannot come back. Residuals of PF-13: a wrong step that outlives
a restart, uncorrected for a day, still ages markers by it, and a daemon
restarted at least once a day (before its first sweep comes due) never
sweeps (orphan markers are small rows: the safe side). Reaps are
counted by reason (`acu_suites_reaped_total{reason}`); `suite_reaped`
carries the judged stamp of an idle reap (`last_requested_unixtime`) and
the judged files of a junk one (`evidence`), and a completed run — its
listing read, its reaps tried, its sweeps done, or held back by the
forward-step guard — logs INFO `suite_reaper_run_complete` and sets the
gauge `acu_suite_reaper_last_run_unixtime`, which has no series until the
first completed run, after the uptime floor: an operator alerts on its
absence held 3 h, or on its age beyond 3 h — the floor plus two
intervals, so neither a restart nor one failed run pages, and a run that
panicked or failed its listing sets no gauge (phase H1). The guard's forward-step report counts in
`acu_clock_steps_detected_total` like every report. The gpg
once-logger sets start over when a new key finds 10,000 entries.
Snapshot GC gains sub-job C,
in the periodic ticks and, when `gc.interval` is longer than an hour, in
a run of C alone an hour after each start (INFO `gc_retired_run_complete`,
within `max_tick_duration`; the final review's R3 #2: a daemon restarted
more often than `gc.interval` never reached a periodic tick, so its
retired snapshots stayed for good; not an earlier first tick, which would
repeat the startup pass's url_path and blob passes) — the startup pass,
which runs before the
listeners serve, skips it, since retired snapshots never serve and the
backlog after a burst of reaps (some 70–100k `package_hash` rows per
snapshot of a large suite) could keep the listeners closed up to
`max_tick_duration` (phase F, ruling PF-15; after a start the run of C
alone above runs it, or, with a `gc.interval` of an hour or less, the
periodic ticks, normally the first one — a later one when an earlier pass,
the startup pass's blob pass say, reached its deadline, since the passes
resume in rotation): after sub-jobs A and B, within the snapshot pass's
deadline, one retired
snapshot per writer transaction is deleted with its members, skipped
members and `package_hash` rows in bounded chunks (10,000 rows per writer
transaction), then the header row; it needs no grace, since a retired
snapshot is never a candidate again (the collision lookup ignores it and
`CommitAdoption` refuses it). Sub-job B never deletes a snapshot still
queued for §6.1's audit (one the audit deferred after a read fault), nor
counts it in its `keep_displaced` ranking: with `keep_displaced = 0` such a
displaced snapshot can be its base's only signed history, which
never-downgrade reads until the audit decides it (ruling R-H1, phase H1;
the predicate sits before the ranking, so a queued snapshot takes no
place). GC recovers a panic per periodic tick and per pool-scan job —
ERROR `gc_panicked` — and goes on; a tick's panic moves the pass rotation
past the pass that panicked, and the startup pass returns its panic as an
error, which the daemon logs before it serves, and moves the rotation too
(phase H1).

**Migration backfill** of `last_requested_at`: the newest client
`last_requested_at` of the suite's anchor `url_path` rows, else migration time
minus the 7-day interest window. Dormant never-adopted suites are therefore
neither eagerly adopted after upgrade nor kept alive; they are reaped about 23
days later.

**`POST /forget` (D9).** Admin endpoint (`host`, `suite`, and an optional
`scheme`: `http` or `https`, default `https`, as for `/reconcile`). It
requires a non-simple request — header `X-ACU-Admin: 1` — and rejects any
`Sec-Fetch-Site` other than `same-origin` or `none`, so a cross-site form
or `fetch` cannot trigger it (security G-1 class; a cross-origin `fetch`
that sets the header needs a CORS preflight, never granted). Without
`htpasswd_file` it also refuses a `Host` that is not an IP address,
`localhost` or `admin.listen`'s host name, which stops DNS rebinding
(G-1, §10: the maintainer's decision of 2026-10-24, which phase F's
ruling PF-6 had left open). Each refusal is 403. It reads the suite's
row (I-4), holds the suite through the `SuiteGuard` and runs the
reaper's steps to completion, within 5 minutes
— all batches, then `cache.ForgetSuite` through `deleteSuiteTx` without
the idle checks — on a context a client disconnect cannot cancel but the
admin listener's shutdown does; the final transaction also deletes the
base's signed-seen markers, under both schemes, and the cached anchors
of the base and its `/.` twin under both schemes, if no row is left for
the base or the twin under either scheme (ruling PF-2). It answers 202
once the suite is forgotten, its body naming the suite, the markers
deleted and the rows that kept them, and 409 for both busy and unknown
suites (no existence oracle; the body says what to check: the status
page, which now shows each suite's scheme, shows `scheme://host` in its
Host cell — the scheme goes in `scheme`, default `https`, the rest in
`host` — and the suite in its Suite path column; ruling PF-7; the log
says which, at DEBUG: `suite_not_forgotten` with `reason` `no_suite`,
`busy` or `changed`, the last at WARN since it signals a bug, phase H1),
400 for a
missing `host` or `suite`, another scheme, a `host` holding `://`
(the Host cell pasted whole), or a body that is not a form (413 only for
one over 8 KiB; phase H1), 500 for a database error (logged), and 503
when the admin listener's shutdown cut it short (each metadata batch and
the final delete are their own transactions, so a forget cut short is
safe to repeat). A final delete that committed although its writer call
failed is logged `suite_forgotten` with `settled=true` before the 500 or
503 when a fresh read then finds the row gone, as a settled reap is (a
read that fails, or lands before a commit still in flight, finds it
present, and nothing is logged); a panic under the hold is logged with
the suite (`freshness_check_panicked`, `where=forget`), the hold released as
the cache then answers, and re-panicked to net/http, whose line the admin
listener logs through slog at ERROR (phase F final review, spec m1, m2).
It is the documented escape hatch for refusal states: a
vendor that stopped signing, a deliberate upstream rollback, a stuck
unsigned suite. (`POST /reconcile` takes the same guard: G-1, §10.)

### C10. Strict mode

The host-wide rule is kept: refuse an unvouched `.deb` iff every current
signed non-retired snapshot on the host has complete coverage (PR #8's
directory scoping is not reimplemented). Unsigned snapshots, anchorless
suites and seeded-but-unadopted suites never count and never vouch. Flat
snapshots now normally have complete coverage, so strict mode protects flat
hosts. A `.deb` whose name begins like a metadata file (`icons-…`,
`Packages…`) is vouched and refused like any `.deb`: `proxy.IsMetadata`
never classifies a Debian pool artifact (`.deb`, `.udeb`, `.ddeb`, `.dsc`,
`.buildinfo`, `.changes`, `.diff.gz`, or a name with `_` and a `.tar.`
component) as metadata (phase G ruling PG-2). A `url_path` row an earlier
release stored for such a `.deb` keeps `is_metadata = 1` until it is
refetched; serving and strict mode read the request's classification, so
only the admin page's hot paths show it (their Kind column and
`hot_url_paths[].is_metadata`).

**In-flight adoptions (D10).** The checker tracks in-flight signed adoptions
per canonical host (an unsigned adoption never vouches, so it is not waited
for; a count ends when the adoption goroutine returns, after its commit:
phase E, design note 27). On the hit path, before answering
`unvouched_deb_refused`,
the handler waits for in-flight adoptions on the request's host — at most
min(15 s, 25 s − the request's age), and not at all when less than 1 s of
that is left (phase E, design review M-10: the miss path still downloads
the whole `.deb` before its first byte, within apt's 30 s) — then re-runs
the lookup once; a request whose client left during the wait is answered
503 (`client_canceled`) and never served (phase E fix round 1: a re-run
lookup with the dead context failed open). On the miss path, where the
strict decision sits inside singleflight after host-slot acquisition by
design, `runFetch` returns
`errUnvouchedRetryable` (releasing its slot) when an adoption is in flight on
the host; `serveCacheMiss`, outside singleflight, waits under the same
bound and retries the miss once: requests that waited together coalesce
again, a client that left during the wait gets no retry, and the refusal
is logged once per fetch, by the singleflight leader (design review O1).
At most 64 waiters per host, each waiting request counting (coalesced
requests for one `.deb` included); beyond that the refusal is immediate —
unless nothing is in flight any more, which is checked first: a request
that arrives as the host's last adoption ends, while its waiters still
unwind, re-runs its lookup (phase H1, deferred minor E-58). With nothing
in flight, both paths still re-run the lookup once, at once and without
waiting, before refusing: an adoption that committed the vouching rows and
ended between the lookup and the in-flight check is seen by then, since
its count ends after its commit (final review R1 #2).

Documented consequences: a client fetching N unvouched packages one
after another during a long adoption waits up to 15 s for each (M-11); an
adoption's count ends after its flip barrier (up to 140 s for a busy
suite), so a package whose snapshot is still in its barrier is likely to
wait the full 15 s and be refused; an adoption that takes longer than 15 s (large
`dists/` suites), a queued (not yet running) first adoption, and an unsigned
or anchorless repository on a host that also serves complete signed suites
all lead to refusals; strict posture changes as eager adoptions land after
upgrade. Conversely, a signed snapshot with incomplete coverage turns strict
mode off for its whole host, flat or `dists/` (for a flat suite, a
`no_sha256` stanza): PR #8 avoided this for flat snapshots by scoping them to
their directory, which this design does not do. Strict mode fails open on a
database error of its lookups (the `package_hash` read on a hit or after a
download, the host's coverage read), each logged WARN, so a transient SQLite
fault does not refuse every `.deb`; a lookup that failed because the
request's context ended is no database error and is answered 503
(`client_canceled`) on the hit path, never served (phase E final fix wave,
FW-5: a MITM tunnel's Drain cancels the inner request while its writer
stays live). On the miss path a failed `package_hash` read before the
download (WARN `package_hash_prefetch_lookup_failed`) does not skip strict
mode: the read after the download decides, as the first would have —
refused, or waited for while a signed adoption is in flight, without the
second lookup — and only a failure of both reads fails open (final review
R4 #2).

### C11. Observability and configuration

**Events** (documented in `docs/log-fields.md`): `suite_seed_rejected`,
`suite_reaped`, `suite_forgotten`, `admin_forget_requested`,
`signed_seen_markers_swept`, `adoption_deferred`,
`adoption_failing_persistently`, `adoption_refused` (reason),
`adoption_flip_barrier`, `adoption_abandoned_at_shutdown` (an adoption
ended by shutdown, in the flip barrier or before it: INFO, never counted as
an adoption outcome), `suite_evicted`, `suite_caps_exceeded`,
`handler_goroutine_panicked`, `adoption_panicked`, `clock_step_detected`,
`flat_host_root_unsupported`, `legacy_snapshot_quarantined`,
`legacy_trust_audit_progress`, `legacy_trust_audit_deferred`,
`legacy_trust_audit_completed`; phase H1's `suite_reaper_run_complete`,
`suite_not_forgotten` (DEBUG; WARN for `reason=changed`), `gc_panicked`, `legacy_anchor_discard_failed`,
`reconcile_snapshot_read_failed` and `source_paths_skipped`; the final
review's `gc_retired_run_complete` (R3 #2, C9 above); and the 43
lines this branch added with prose messages, renamed to snake_case event
names (`suite_onboarding_failed`, `suite_reaper_list_suites_failed`,
`admin_forget_failed`, `freshness_anchorless_index_over_cap`,
`freshness_check_cancelled`, `suite_touch_failed` and their siblings;
rulings R-H3 and R-H6: no release logged the prose; a guard test fails any
new prose message, main's released ones excepted), each keeping its level
and fields except five that phase H1 changed besides: `canonical_scheme`
added to `freshness_check_ended_by_shutdown` and `freshness_check_cancelled`;
`canonical_scheme`, and DEBUG at shutdown, on
`freshness_read_anchor_rows_failed` (its absent-anchor site, GD-6); DEBUG at
shutdown on `adoption_served_anchor_read_failed`; and
`admin_forget_failed`'s `scheme` renamed `canonical_scheme` (below); new
fields `anchor_form`,
`snapshot_displaced`, `flip_held_ms`, phase G's `rows_replaced` and
`rows_deleted` (the anchorless `freshness_check` line, C4 Dispatch 4) and
`suite_path` in `package_hash_conflict`'s `declared` entries (C6), and phase
H1's `anchor_rows_deleted` (`legacy_snapshot_quarantined`, §6.1),
`last_requested_unixtime` and `evidence` (`suite_reaped`), `paced_stale`
(the stale-suites WARN), `canonical_scheme` on `admin_forget_requested`,
`admin_forget_failed` and beside `admin_reconcile_requested`'s `scheme` (`flat` is
the admin `suites[]` field, C7, not a log field: every suite line's
`suite_path` shows the layout; phase G ruling PG-23). New metrics include
the unlabelled
gauges `acu_adoption_failing_suites` (C7), `acu_freshness_waiting_for_slot`
(suites waiting for an adoption slot, C4) and `acu_flip_barrier_active`
(flips in the C8 barrier now), the C3 pair
`acu_suite_seed_rejected_total{host}` (counter) and
`acu_suites_seed_rejected` (gauge), `acu_freshness_unlogged_panics_total`,
phase E's `acu_repair_deferred_total{host}` and
`acu_strict_adoption_waits_total{host}`, and phase F's unlabelled counter
`acu_gc_retired_reaped_total` and `acu_suites_reaped_total{reason}` (three
values: `idle`, `non_canonical`, `junk`), and phase G's
`acu_anchorless_sniff_rejected_total{host}` (C4 Dispatch 4), and phase H1's
unlabelled gauge `acu_suite_reaper_last_run_unixtime` (C9).
New metrics carry no suite labels: they are unlabelled, host-labelled or,
for `acu_suites_reaped_total`, labelled by its three reasons. The list
above is not exhaustive: the flip barrier's six counters (C8),
`acu_serve_byhash_displaced_total{host}`, `acu_clock_steps_detected_total`
and `acu_legacy_snapshots_quarantined_total` are new too; the 1.1.0 release
notes list all 21, computed from the code. They are
listed in the metrics inventory comments of `internal/freshness/metrics.go`
and `internal/handler/metrics.go`, except `acu_gc_retired_reaped_total`,
declared with the other `gc_run_complete` counters in
`internal/gc/metrics.go`.
`gc_run_complete` and `LastRunSummary` gain `retired_reaped`, which the
admin status JSON's `gc` block carries from phase H1.

| Key | Default | Rule |
|---|---|---|
| `adoption.adopt_unsigned` | `true` | bool |
| `adoption.flip_quiet_period` | `"10s"` | `0s` disables the barrier; otherwise `1s` to `15s` |
| `adoption.max_duration` | `"30m"` | ≥ `1m` |
| `freshness.max_suites` | `5000` | ≥ 10 (per-host cap = `max_suites / 10`, documented for large Artifactory/aptly/Nexus hosts) |
| `gc.suite_idle_ttl` | `"720h"` | `0s` disables reaping; else ≥ `24h` |

Constants, not knobs: backoff 2 m → 1 h (adoption failures and repeated
*unknown* checks); hold 20 s, of which the last 5 s are kept for the commit
(the C8 quiet cut at 15 s); phase A cap 60 s; hold-token wait 60 s; waiter
caps 256 (barrier) and 64 (strict wait); strict wait 15 s, within a 25 s
request budget, none below 1 s; the miss path's backstop deadline at most
25 s after the request started; reaper interval and uptime floor 1 h, the
marker sweep 24 h apart, the first 24 h after the reaper starts, per run
250 reaps regardless of idleness and 25 idle ones, the forward-step
threshold 1 h,
a suite's held work (a reap, `/forget`) 5 min, junk grace 24 h, metadata
batches 1,000 rows, sub-job C chunks 10,000 rows, gpg once-logger sets
10,000 keys;
unsigned member cap 1,000; anchor-absent and unsigned confirmation gap
`max(cooldown, 5 min)`, 1 h after a 401/403 on a signature file; check
deadline 2 min; check workers 4; interest window 7 d; orphan-marker TTL
365 d; cap-rejection cache 10 min (1 min for
a refusal while every eviction candidate is busy; at most 4,096 suites);
tracked suite path at most 1024 bytes; legacy-audit anchor
read cap 8 MiB and batch 100; the suffix fallback's 32 segments and 5,121
bytes (a 1,024-byte base, a 4,096-byte Filename: phase G ruling PG-30); the
anchorless refresh's body cap 64 MiB, at most two bodies in memory at once
for the process (phase G ruling PG-8), and its over-cap WARN at most one
line a minute; `flat_host_root_unsupported` at most one line a minute.

## 6. Migration v6 → v7

A new `migrations[6]` entry (no existing migration is edited);
`CurrentSchemaVersion = 7`. The migration framework gains an optional Go step
run in the same transaction, with `now = nowUnix()` bound as a parameter, so
no production SQL uses SQLite's clock. The DDL was probe-verified on a
populated v6 database (SQLite 3.53, modernc).

```sql
-- DDL
ALTER TABLE suite_freshness ADD COLUMN last_requested_at      INTEGER;
ALTER TABLE suite_freshness ADD COLUMN adoption_fail_hash     TEXT;
ALTER TABLE suite_freshness ADD COLUMN adoption_fail_reason   TEXT;
ALTER TABLE suite_freshness ADD COLUMN adoption_fail_count    INTEGER NOT NULL DEFAULT 0;
ALTER TABLE suite_freshness ADD COLUMN adoption_fail_first_at INTEGER;
ALTER TABLE suite_freshness ADD COLUMN adoption_fail_last_at  INTEGER;
ALTER TABLE suite_freshness ADD COLUMN adoption_next_retry_at INTEGER;
ALTER TABLE suite_freshness ADD COLUMN adoption_fail_etag     TEXT;
ALTER TABLE suite_freshness ADD COLUMN adoption_fail_lastmod  TEXT;
ALTER TABLE suite_freshness ADD COLUMN check_fail_count       INTEGER NOT NULL DEFAULT 0;

ALTER TABLE suite_snapshot ADD COLUMN anchor_form TEXT NOT NULL DEFAULT ''
  CHECK (anchor_form IN ('', 'inline', 'detached', 'unsigned'));
ALTER TABLE suite_snapshot ADD COLUMN release_date INTEGER;
ALTER TABLE suite_snapshot ADD COLUMN retired_at   INTEGER;
UPDATE suite_snapshot
   SET anchor_form = CASE WHEN inrelease_hash IS NOT NULL THEN 'inline' ELSE 'detached' END;
CREATE TRIGGER suite_snapshot_anchor_form_required
  BEFORE INSERT ON suite_snapshot WHEN NEW.anchor_form = ''
  BEGIN SELECT RAISE(ABORT, 'suite_snapshot.anchor_form required'); END;

DROP INDEX idx_suite_snapshot_natural;
CREATE UNIQUE INDEX idx_suite_snapshot_natural_v7
  ON suite_snapshot(canonical_scheme, canonical_host, suite_path,
                    COALESCE(inrelease_hash, release_hash), anchor_form)
  WHERE retired_at IS NULL;

ALTER TABLE package_hash ADD COLUMN flat_filename TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_package_hash_flat_filename
  ON package_hash(canonical_scheme, canonical_host, flat_filename)
  WHERE flat_filename <> '';

CREATE TABLE suite_signed_seen (
  canonical_scheme TEXT NOT NULL,
  canonical_host   TEXT NOT NULL,
  base             TEXT NOT NULL,
  last_seen_at     INTEGER NOT NULL,
  PRIMARY KEY (canonical_scheme, canonical_host, base)
);

-- Every adopted pre-v7 snapshot awaits the legacy trust audit (§6.1).
-- Never-adopted candidates are inert (a later adoption that reuses one
-- verifies its own bytes) and their anchors are unpinned, so not queued.
CREATE TABLE legacy_trust_audit (
  snapshot_id INTEGER PRIMARY KEY
);
INSERT INTO legacy_trust_audit (snapshot_id)
  SELECT snapshot_id FROM suite_snapshot WHERE adopted_at IS NOT NULL;

-- Go step, same transaction, :now = nowUnix()
UPDATE suite_freshness SET last_requested_at = COALESCE(
  (SELECT MAX(u.last_requested_at) FROM url_path u
    WHERE u.canonical_scheme = suite_freshness.canonical_scheme
      AND u.canonical_host   = suite_freshness.canonical_host
      AND u.path IN (suite_freshness.suite_path || '/InRelease',
                     suite_freshness.suite_path || '/Release')),
  :now - 604800);
```

The migration writes no signed-seen markers: "adopted" is not "signed" (a
pre-v7 inline snapshot may hold a plain-text body), and the evidence for
markers lives in pool files SQL cannot read. The startup audit writes them
(§6.1).

Notes: existing snapshots keep `release_date` NULL (the `Date` rule applies
from their successors on). The partial index scans `package_hash` once
(seconds on ~1 M rows; covered by the existing `schema_migrating` log line)
and stays empty until flat suites adopt. The backfill subquery uses the
`url_path` primary key. A v7 database is refused by older binaries; rollback
means restoring a backup or wiping the cache (release notes).

### 6.1 Legacy trust audit

Rows written before this release were derived under two pre-existing
weaknesses that the schema cannot show:

- **R-1:** reconcile parsed the whole stored `InRelease` envelope, so
  declarations placed inside the signature armor (after a forged `=CRC` line)
  could add members, by-hash aliases and `package_hash` rows to a current
  snapshot. Fixing the parser (phase A) stops new pollution but removes
  nothing already stored, and reconcile never deletes extra members.
- **Plain-text fallback:** with `require_signature = false`, a plain-text or
  undecodable "InRelease" was adopted as an inline snapshot, with
  `package_hash` rows and strict-mode participation. The migration labels it
  `inline` like every single-anchor row.

The v7 migration queues every adopted snapshot in `legacy_trust_audit`. At
every start, after the migration and before the GC loops, the freshness
checker and the HTTP listeners serve, `freshness.RunLegacyTrustAudit` drains
the queue. **Pre-flight:** when the queue holds inline snapshots, the pool
directory must be readable and non-empty, and when every queued inline
anchor file is missing (snapshots that share an `InRelease` share its one
file), at least one of a sample of up to 16 pinned files outside the queue
(blobs with a positive refcount that no queued snapshot names as its
`InRelease`: blob GC never deletes them) must exist, whenever the anchor
files and the sample are two or more files; an unmounted, empty or foreign
pool would otherwise make every anchor look missing, so startup refuses
with a message naming the path and the two ways out (restore the pool, or
wipe the cache), and the audit changes nothing — though the migration,
which runs first, has: the message says the database is already at schema
7, which 1.0.x refuses, so going back means restoring the `cache.db`
copied before the upgrade (the final review's R5 #2, R7 #2). The sample
is evidence the queue cannot exhaust: after the first start the queue
holds only the deferred snapshots, whose unreadable files an operator may
remove; with the pool mounted they are then quarantined `anchor_missing`
(the final review's R2 I-1, R5 #1). A single file checked — one queued
anchor file, missing from a non-empty pool, and no other pinned file — is
quarantined (one lost pool file cannot be told from a lost mount, and
refusing would leave no recovery short of wiping the cache);
detached-only queues read no pool files. Each batch of up to 100 is decided in memory and
applied in **one writer transaction** (an INFO `legacy_trust_audit_progress`
line per batch):

- **Detached** snapshots are signed (adoption always verified `Release.gpg`,
  or under `accept_any_signer` checked its structure): marker for the base
  (`last_seen_at` = `adopted_at`, else `created_at`), dequeue. An
  **unsigned** snapshot (possible only if one is re-queued after phase E) is
  never trust-bearing: dequeue, no marker.
- **Inline** snapshots: read the stored `InRelease` blob (at most 8 MiB) from
  the pool and check its SHA256 against the pinned hash. A missing file
  (`anchor_missing`), an oversized one (`anchor_oversized`) or a hash
  mismatch (`anchor_corrupt`) is quarantined without a marker. **Any other
  read error is not evidence:** the snapshot stays queued and untouched (it
  keeps serving as before the upgrade), an ERROR
  `legacy_trust_audit_deferred` names it, and the next start retries it;
  startup continues. It is **signed and clean** when it decodes as a clearsigned
  message, its plaintext parses, and parsing the raw envelope the way the old
  reconcile did (with line endings and trailing spaces canonicalised exactly
  as the clearsign decoder canonicalises the plaintext: CRLF → LF, trailing
  spaces and tabs removed) yields no (path, SHA256, size) declaration that the
  plaintext lacks and no parse error: marker, dequeue. Anything else — no
  clearsign marker (legacy plain text), a marker that does not decode, extra
  or unparseable declarations in the raw envelope (R-1: after a forged
  `=CRC` line, or as colon-bearing armor headers), unparseable plaintext — is
  **quarantined**, like the missing, oversized and corrupt
  blobs above. An envelope that decodes (R-1 tampering, unparseable
  plaintext) still proves the upstream signs, so it is quarantined **and**
  marked; plain-text, undecodable, missing, oversized and corrupt anchors get
  no marker here. (The startup anchor pass, like the
  handler, uses only the first-line sniff, so a malformed envelope that
  still begins with the marker line does create a marker there: refusing
  unsigned adoption for a repository that presents itself as signed is the
  safe direction, and `POST /forget` is the escape hatch.)
- **Quarantine** (`cache.ApplyLegacyAuditDecisions`, in its batch's writer transaction):
  if the snapshot is a suite's current snapshot, clear `current_snapshot_id`
  and that suite's validators and run the Step-8 refcount decrement for it;
  set `retired_at`; dequeue. For `anchor_corrupt` the same transaction also
  deletes every `url_path` row whose `blob_hash` is the snapshot's stored
  `InRelease` hash — under any scheme or host, through the writer — so
  the next client request fetches a fresh copy from upstream instead of
  being served bytes apt rejects (the maintainer's decision of 2026-10-24,
  ruling R-H2, phase H1). In the transaction, not after it: the dequeue
  commits with the delete, so a crash between them cannot dequeue the
  snapshot with its corrupt rows still served. The audit also removes the
  corrupt pool file (`DiscardFinalizedBlob`, the integrity scanner's
  remedy), on its own goroutine after the batch commits, never on the
  writer: the miss path keeps an existing pool file of the same hash, so
  an upstream still serving the same `InRelease` would otherwise be
  fetched onto, and served from, the rotted file (phase H1, review C-1).
  Never before the commit: a lost commit would leave the snapshots queued
  with their anchors gone, judged `anchor_missing` at the next start, or a
  refused start when the pool holds none of the other files the pre-flight
  checks either (re-review N-1; the final review's R2 I-1). A failed
  removal is logged (ERROR `legacy_anchor_discard_failed`) and the
  quarantine stands. Residual: a crash between the commit and the removal
  leaves the corrupt file, with its rows deleted and the snapshot
  quarantined; the audit does not revisit it, so a refetch from an
  unchanged upstream serves the rotted bytes again until an eager
  re-adoption's rehash evicts the file (adoption enabled, a client asks)
  — a failed removal's outcome without its ERROR line.
  `anchor_oversized` (over the 8 MiB read, not proven bad) and
  `anchor_missing` (nothing to serve) keep their rows, and a read error is
  still no evidence. WARN `legacy_snapshot_quarantined` (reason,
  snapshot, host, suite, `was_current`, `anchor_rows_deleted`); counter
  `acu_legacy_snapshots_quarantined_total`. A quarantined snapshot never
  serves, never vouches or counts for strict mode (both read current
  snapshots only), is excluded from retained by-hash (non-retired only), can
  never be committed (`CommitAdoption` refuses retired candidates with
  `ErrSnapshotRetired`), and is deleted by snapshot GC sub-job C (phase F);
  until then it is a retired displaced row, which sub-job B's
  `keep_displaced` ranking skips from phase F on. Its blobs' refcounts are
  released exactly once.

Each batch commits with its outcomes, so an interrupted audit resumes at the
next start without repeating effects. Only a database error or a failed
pre-flight refuses to start (those would misjudge every snapshot); a single
unreadable anchor defers that snapshot. The quarantine WARN is logged after
its batch commits, so a crash in between can lose the line but never the
effect. Retired snapshots still count in the displaced-snapshot gauge until
snapshot GC deletes them. The listeners are bound before the cache opens, so
during a long first audit connections are accepted and wait; apt clients may
time out once (documented). After the queue drains, the **marker pass**
(C3) runs at every start. `INFO legacy_trust_audit_completed` reports counts
and duration.

A quarantined suite falls back to Phase 1 serving (its `url_path` anchor row
is kept — after `anchor_corrupt`, deleted: R-H2 above) until it re-adopts.
A quarantine clears the suite's current snapshot, so, as built (phase E),
every quarantined suite re-adopts as a suite without a snapshot does:
through C4's Dispatch 3 (`checkFirst`), at its first check when a client
asked for it within the interest window (after the upgrade: fetched one of
its anchor files in the 7 days before it) and a client-created anchor row
remains, otherwise once a client asks again — whatever the reason, and
without comparing the cached anchor with upstream. An `anchor_corrupt`
quarantine deleted its anchor rows, so that suite waits for a client's
request (the final review's R5 #4 and R7 #3: phases A–D's rule, re-adoption
on the first check that observes anchor bytes different from the cached
ones, made an R-1 quarantine adopt at the first post-upgrade check and the
others wait for an upstream change; it no longer applies). With
`refuse_unvouched_debs = true` their `.deb`s are refused meanwhile whenever
another current snapshot on the host has complete coverage (C10 treats them
as unadopted) — documented at upgrade. A legacy plain-text repository then
adopts in the unsigned form through `Release` (phase E), with no false
downgrade refusal, because no marker was written for it. Rebuilding derived
rows in place was rejected: `package_hash` rows do not record which index
produced them, so a rebuild is a full re-derivation, and quarantine is
simpler to prove correct; both conditions require an attack or a
non-default setting.

## 7. Testing strategy

**Harness.** Virtual-clock simulations use Go's `testing/synctest` with the
**real** cache (verified: 30 virtual days in 0.02 s with modernc SQLite and
the writer goroutine), in-memory `Fetcher`, `AdoptionFetcher` and
`handler.Fetcher` fakes, and the `wallclock` offset for backward and forward
steps. New code blocks only on channels and timers (never a `sync.Mutex`
across virtual sleeps). Jitter uses fixed seeds. Every long-run simulation
asserts **lower bounds** (liveness: checks resume, changes adopt, touched
suites survive, returning suites re-adopt) as well as upper bounds (attempts,
rows, goroutines, upstream requests). Where cadence matters the simulations
run the production scheduler, reaper and GC loops at production intervals
(`gc.interval = 24h`), with restarts — except that the 90-day reaping
simulation runs its reaper every 6 h, the hourly cadence being pinned by
the reaper's own tests and by a four-day simulation at the production
cadence (phase F, deviation 2).

**Fixtures.** A shared `clearsignedFixture()` helper replaces the
non-clearsigned `InRelease` bodies in existing handler and cmd tests (about 18
files), in phase D.

**Per-component tests** (each phase's DoD names its own; highlights):

- C1: apt-encoding table incl. the `RawPath` assertion; production-fixture
  round trip; the S-3 minted-suite probe fails on main.
- C2: grammar tables with decoded inputs; a `dists` segment is never flat;
  `%2E` and `.` land on the same suite.
- C3: single-INSERT-site test; minted anchors, markers and by-hash names at
  scale create no rows beyond caps and no map growth; cap eviction and the
  rejection cache; legacy anchor onboarding on a `url_path` hit (`dists/` in
  D, flat in G); a named adoption-disabled parity test (seed values, no T1,
  no new seeds); touch coalescing; marker written only for tracked suites;
  marker written at adoption (signed forms only) and by the startup pass for
  a never-adopted suite whose signed anchor was served; miss-path ordering
  (the marker is in the writer operation that stores the anchor row; a failed
  operation serves no body); the hit-path invariant (after a mix of misses,
  adoptions, `reconstructAnchor` and restarts, every `url_path` `InRelease`
  or `Release.gpg` row of a tracked suite whose blob passes the sniff has a
  marker for its base, at every committed state (markers exist only for
  tracked suites; phase D plan, deviation 1)); FIFO ordering (a marker write
  enqueued before an unsigned commit is visible to the commit's
  transaction).
- C4: classification table (every status class per anchor); form transitions
  both ways incl. detached → inline and the two-observation rule;
  `anchor_absent` visible and not silent; anchorless refresh (IMS, sniff
  rejection, 404 deletes, request counts preserved); capacity: 2 slow-drip
  first adoptions + 20 normal signed suites changing at once (one-minute
  adoptions) + 1 degraded suite, pool of 2 → at most one first adoption at a
  time (the reserved slot serves the signed suites); the signed suites adopt
  back to back — consecutive admissions ≤ 1 min + *D* apart, all adopted
  within one tick + 20 min + *D* — and each costs at most two anchor GETs
  before it adopts; both drips end `timed_out`; the degraded suite's checks
  continue; goroutines ≤ workers + cap + constant; at unit level a queued
  suite is not probed by ticks while it waits, is re-checked within *D* of
  its slot's release with the cooldown bypassed, and a busy offer neither
  spins nor loses its place; the upgrade burst — 50 never-adopted suites
  with interest and a trickle of signed changes, pool of 2: first
  adoptions back to back, a signed change waits for at most one adoption,
  each first adoption costs two probes by the scheduler, plus one per client
  check while it waits and one per offer another admission takes first
  (phase E, ruling PE-4: the deterministic burst, and a racing one with
  every suite on one host);
  clock steps ±2 h/±30 d with liveness asserts;
  `TestCheck_NoAdopter_ChangeOnlyLogged` (setup seeds the row); scheduler
  fairness: timing-out anchor probes on one host ahead of healthy suites on
  another, plus persistent repair and changing-candidate contention, meet the
  stated latency bound with bounded goroutines and continuing checks.
- C5: zero-plaintext InRelease fixture (I-1) crashes main and is recorded as a
  failure here; R-1 armor-injection fixture; `./` and `%` Filenames; SHA512
  merge, every-hash verification, aliases, and reconcile of a SHA512-only
  member; `Date` parsing table (apt's accepted and rejected forms) and
  older/future/equal/missing rules; unsigned confirmation (404 and 403
  timings), never-downgrade at both points, member cap, no `package_hash`
  from reconcile; an unsigned adoption paused before commit while a signed
  anchor is served and its marker committed (also via the `/.` twin, in
  phase G) is refused with no pointer change, as a refusal state; a
  missing marker with a sniff-passing cached signed anchor is re-marked
  before the commit, which then refuses (ruling R-E1);
  `require_signature = false` with
  marker-present malformed or frameless bodies creates no inline snapshot,
  `package_hash` row or strict participant (`malformed_clearsign` distinct
  from `missing_signature`); `TestCheck_UnsignedRecoversWhenReleaseAnchorMissing`;
  guard (d) keeps an unsigned suite's `/Release`.
- C6: every real Filename shape vouches; nested-base tiebreak; `..`/`//`/
  sibling `/flat2` negatives; `EXPLAIN QUERY PLAN` uses the partial index.
- C7: lim1 reproduction (≤ ~30 attempts/day vs ~1,440 on main); a T1 storm
  right after a failure causes zero extra attempts; a multi-week simulation
  (about 20 virtual days, per-day bounds: untrusted body, new content,
  rollback, clock step); flapping mirror → zero
  backward flips; restart spread; panic excluded from reset; deferred-then-
  retry liveness with 304s to the failing validators.
- C8: the overlapping-hold trace (two busy suites entering holds 0.1 s
  apart, a shared client queue holding one suite's old anchor) → zero
  straddles; hold-token timeout commits without a hold and counts it;
  plain-path straddle reproduced on the phase-B tip and fixed; busy suite
  (an anchor every 500 ms for 120 s across a flip) → zero straddles;
  mirror-mode two suites with a hold in progress → zero straddles; slow reader
  (12 s and 25 s, a commit with real latency) → the hold commits at 15 s,
  the held clients get the new snapshot, only the slow reader may straddle,
  and the cut is counted; a commit that has not started or finished by 20 s
  → the hold ends at 20 s, counted; hold never exceeds 20 s;
  idle zero-wait; client cancel; shutdown; commit failure releases waiters;
  the chaos test's straddle count asserted zero; retained by-hash (SHA256 and
  SHA512 in C; E's SHA512 adoption writes the SHA512 alias rows) with
  stray-blob and retired-snapshot negatives;
  `Last-Modified` across same-second flips, first adoption, a backward
  step, a pool file dated after its stamp and a recovery rewrite.
- C9: 90-day simulation at `gc.interval = 24h` with restarts (touched /
  idle / near-TTL / busy / clock jump; phase F, deviation 2: the reaper
  every 6 h after its 1 h floor, restarts every fifth day and an hour
  before three suites' deadlines, no url_path TTL pass, since production
  cadence with 12-hourly restarts takes about 12.5 s under `-race`); a
  reaped suite that returns
  before any GC tick with unchanged upstream re-adopts within one check;
  `/forget` then an immediate request re-adopts, and after an upstream
  rollback adopts the older content; `/forget` lifts
  `unsigned_downgrade_refused` unless the `/.` twin keeps a row, a signed
  anchor cached under the other scheme included; a +30 d forward step's
  false reaps all self-heal, and a step while running defers reaping by
  the floor and ages no orphan marker, and a clock ahead at a start has a
  day to be corrected before the first sweep; each class's per-run budget
  (250 reaped regardless of idleness, 25 idle); a reap or a cap eviction
  turns an audit-deferred signed snapshot into a marker, and a suite with
  no signed history gets none; four days at the production cadence (hourly reaper,
  `gc.url_path_ttl = 168h`, 12-hourly restarts, goroutines bounded);
  `gc.enabled = false` still re-adopts; never-downgrade after a reap; the
  marker sweep never rewrites a future stamp; batched metadata deletion
  never repeats the refcount decrement; reap-vs-adoption chaos under the
  suite cap; blobs reclaimed within two periodic GC ticks when no pass
  reaches its deadline, as in the test (the startup pass runs no sub-job
  C); twin and nested suites
  untouched; legacy junk reaped, a transient soft-404 and a plain-text
  Release kept; an established unsigned suite keeps its coherent snapshot
  while `adopt_unsigned` is off; `/forget` rejects a cross-origin form
  POST.
- §6.1: a polluted v6 cache (R-1 member, alias and `package_hash` rows on
  current and displaced snapshots; a plain-text inline snapshot with
  `package_hash` rows) upgraded: the forged index is not served, its hashes
  do not vouch, clean suites keep serving and get markers, refcounts balance,
  interruption and restart resume without double effects; a detached
  snapshot is kept; a missing or empty pool stops startup without changing
  anything; an unreadable anchor defers only that snapshot; a missing or
  corrupt anchor blob is quarantined; CRLF envelopes and whitespace-only
  lines inside a hash block do not cause a false quarantine, and an
  injection inside a CRLF envelope or the armor headers is still caught; an
  R-1-quarantined suite re-adopts on its first check against a clean
  upstream; the audit runs before GC and serving with adoption disabled; the
  quarantined plain-text suite later adopts unsigned without a downgrade
  refusal (E).
- C10: strict matrix per `sources.list` form × Filename shape; mixed host,
  including a flat snapshot with incomplete coverage; PR #8's strict cases
  re-expressed for this rule (plan overview, "PR #8 parity");
  the in-flight wait turns a first-install refusal into a success on both the
  hit and miss paths; unsigned and anchorless suites never change posture.

**Real-apt end-to-end** (`e2e/flat/`, `make e2e-flat`), built on the
single-container `e2e/deb` pattern (nginx + daemon + clients in one image,
in-container restart, upstream `current -> v1|v2` symlink flips), run per
client image: `debian:11` (apt 2.2, 0 retries), `ubuntu:22.04`,
`ubuntu:24.04`, `debian:12`, `debian:13`, `ubuntu:26.04`, with the images'
`docker-*` apt.conf quirks removed. Fixtures reproduce k8s (`amd64/…`, 403 for
missing), CUDA (`./…` plus an epoch `%3a` package), libnvidia-container
(InRelease only, SHA512-only, frozen Date), Jenkins (`sub/`, detached,
`direct/`), Sublime (`files/`, host-root base), Jitsi (by-hash SHA256+SHA512,
binary `Release.gpg`, `Contents-amd64` via apt-file, a `deb-src` line),
unsigned (404 and 403 variants), Release-less (Gemfury shape), plus a
`dists/` repo. Jitsi's binary `Release.gpg` is never fetched: apt 2.2–3.2
refuse a binary detached signature, so the fixture serves an `InRelease`
beside it, and the cache's side (the sniff, a detached adoption) is pinned
in Go. The 403 unsigned variant needs an hour between its observations,
past any run: the run checks it is still unadopted after a refusal written
305 s or more after its first observation, where a 5-minute gap would have
confirmed it. Signed repos live on one host name, unsigned and Release-less
repos on a second, so strict mode's expectations are explicit; a mirror-mode
run and a two-suites-one-host flip are included. Assertions: installs
succeed; the exact request lines apt sends are pinned (`./`, `%253a`,
`by-hash/SHA512`); `apt-get` output is grepped for `Failed to fetch|Connection
failed|timed out|Hash Sum mismatch|Mirror sync in progress` (exit status is
useless), and, on lines naming the signed host, for signature errors and any
`E:`/`Error:` line; zero `unvouched_deb_refused` on the signed host; a v2
publish of a signed or anchorless repo reaches clients within one cooldown,
carried by the client's own check before the scheduler could have checked
the suite; an older-Date publish is ignored; state survives a restart. A
commit-delay hook of 45 s asserts no client timeouts under the full grep:
its client holds the old lists and gets 304s, and its by-hash requests are
served from the displaced snapshot, so the phase-B-expiry straddle, a
documented residual, does not reach it. The unsigned first-adoption
scenario runs on the oldest and newest images only: it adopts at its
record's first retry at least 5 minutes after first contact, 5 to 15
minutes in practice (C5's two observations at least `max(cooldown, 5 min)`
apart, on C7's retry schedule). `go test -race ./...`,
`golangci-lint run ./...` and `gofmt` are clean at every phase.

**Live-upstream smoke** (`e2e/live/`, `make e2e-live`; manual, before 1.1.0
and before later releases that change the request path, adoption or
vouching, never in CI): a release-built daemon, with adoption and strict mode
on, and apt 2.6 clients against pkgs.k8s.io (two minors, both source forms),
NVIDIA CUDA, libnvidia-container, pkg.jenkins.io and
download.sublimetext.com — every suite adopts in its upstream's form, every
`apt-get update` is clean under the e2e grep, one package per repository
installs vouched, and strict mode refuses only the controls' undeclared
`.deb`s.

## 8. Documentation

- `README.md`: supported repository layouts (flat forms, `/` vs `./` twins,
  host-root limitation, unsigned/Release-less/SHA512 support), the
  adoption-enabled requirement, corrected coherence claim, GC and `/forget`.
- `docs/configuration.md`: new keys (C11); unsigned trust, never-downgrade and
  the LAN-client note; the changed meaning of `require_signature = false`
  (phase A; release note: it affects only deployments whose `InRelease` URL
  serves a non-clearsigned body, which apt rejects on its first line); the
  legacy trust audit and quarantine at upgrade;
  strict-mode behaviour for flat, unsigned, anchorless and first use; eager
  first adoption; suite reaping; the apt timeout constraint on the barrier;
  the per-host suite cap.
- `docs/log-fields.md`, `docs/admin-ui-spec.md`,
  `packaging/config/config.toml.default`.
- `docs/release-notes/1.1.0.md`: the 1.1.0 release notes, for the GitHub
  release; their inventories of new events and metrics are computed from
  the code at 1.0.2 and at the release.
- `docs/open-decisions.md`: the decisions that outlive the work, each the
  maintainer's, with its evidence and a recommendation.
- `AIDEV-NOTE`s at every changed invariant; `Contributors.md` credit.

## 9. Implementation phases

Each phase leaves shippable state: `go test -race ./...`, lint and `gofmt`
clean; its own config keys, docs entries and `AIDEV-NOTE`s; a max-effort
review of the phase diff; fixes; a commit. The DoD lists named tests; no DoD
item depends on a later phase.

| Phase | Items | Definition of Done (highlights) |
|---|---|---|
| **A. Foundations** | v7 migration (final schema, Go backfill step, trigger) and Go types incl. the `SingleAnchorHash`/`Form`/`ReleaseDate`/`RetiredAt` fields; `aptpath` and `wallclock` packages; handler fetch seam; C1 complete (changes `+`/`~` request lines to apt's form); C2 `dists/` tightening (no flat grammar yet); `%w` wraps and sentinels; R-1; I-1 recover (logging) in adoption, repair, reconcile, T1 and scheduler goroutines; clock-step rule in existing gates; synctest harness + smoke simulation; inline verification fails closed; signed-seen markers written at adoption and by the startup marker pass (with the `InRelease`/`Release.gpg` sniffs and `MarkSignedSeenBatch`); §6.1 legacy trust audit with quarantine. | v6-fixture migration test; no-SQLite-clock test; C1 tables and production-fixture round trip; existing suite green except enumerated `SuitePath` tightening cases and the `require_signature = false` passthrough tests; R-1 and I-1 fixtures fail on main, pass here; polluted-v6 audit test (current and displaced, plain text, interruption); 30-day smoke simulation < 5 s under `-race`. |
| **B. Retry discipline and capacity** | C7 record, gate, schedule, refusal states, restart; `Date` rules; validators at commit; `change_seen` semantics; capacity (TryAcquire global + host slots for adoption, repair and reconcile; reserved slot; `max_duration`; `WithoutCancel` commit; queued results with slot-driven re-offer); panic recorded; fair concurrent scheduler (C4: per-host queues, 4 workers, one check per host turn, healthy → never-checked → failing, FIFO, persisted failure streak and backoff); admin, gauge, logs. | lim1 reproduction bound; T1-storm test; long-run simulation (about 20 virtual days, per-day bounds; PE-9) incl. flapping mirror and clock step (liveness + bounds); slow-drip + degraded-suite capacity simulation exercising both check and adoption admission; scheduler latency-bound simulations with stalling (not refusing) responders: H = 8 with n = 3 healthy suites ahead; 500 failing + 1 healthy suite on one host, repeated after a simulated restart; "lagging" visible. |
| **C. Flip consistency** | C8 flipgate with the hold token, handler anchor hook, retained by-hash (SHA256 and SHA512, non-retired only), monotonic `Last-Modified`. | Straddle test fails on the B tip (the busy-suite simulation at `flip_quiet_period = 0s`), passes here; overlapping-hold trace in a shared-queue simulation; five busy suites flipping within 0.1 s (holds never overlap, four hold, the fifth commits unheld within 60 s + 20 s of the first hold and is counted, zero straddles for held suites); busy-suite, cross-suite and slow-reader tests; hold ≤ 20 s; chaos straddles = 0; `flip_quiet_period = 0s` keeps existing tests unchanged. |
| **D. Creation bounds and signals** | C3 in full: sniffs, `CreateSuite` with caps (anchor rows and markers before the body, including first contact), eviction and rejection cache, legacy onboarding, adoption-disabled seed parity, UPDATE-only writers, I-4 (both sides), touch (anchors, vouched `.deb`s), signed-seen marker in the miss path's writer operation; `Release` and flat-index sniffs; `clearsignedFixture()` migration. | Single-INSERT-site test; minted-suite scale test; `dists/` onboarding test on a v6 fixture; adoption-disabled parity test. |
| **E. Eager adoption and anchor forms** | C4 probe tree, classification, form transitions, `anchor_absent`, eager first adoption with interest window, T1-on-seed; C5 unsigned form with safeguards; SHA512 merge, shared verify helper, aliases (served from displaced snapshots by phase C's retained by-hash); C10 in-flight wait. | Production-shaped fixture: active never-adopted suites adopt once, dormant ones make zero requests; classification table; never-downgrade at both points, including the in-transaction recheck race; a §6.1-quarantined plain-text suite adopts unsigned without a downgrade refusal; unsigned never changes posture; nvidia-shaped fixture adopts; SHA512 by-hash served; in-flight wait on hit and miss paths. |
| **F. Suite reaping** | C9 retirement, reaper (own ticker), batched metadata deletion, sub-job C, memory purge, marker TTL, `POST /forget`. | 90-day simulation at `gc.interval = 24h` with restarts; return-before-GC and `/forget` re-adoption tests; forward-step self-heal; never-downgrade after reap; `/forget` CSRF test; twins untouched. |
| **G. Flat repositories** | C2 flat grammar; flat IndexTargets (layout-aware, adoption and handler sides); `validateFilename`; C6 materialisation, fallback, coverage; anchorless refresh; flat root-index touch; flat pdiff arch labels. | PR #8's evidence: a miss on `/…/deb/InRelease` and on `/…/deb/./InRelease` creates one suite row each and fires T1 under it, a hit fires T1 again, and `/…/deb/Packages_1.0_all.deb`, host-root `/InRelease` and `/…/deb//InRelease` create none (no check fires for either path on `main`); `/core:/stable:/v1.34/deb/InRelease` → suite `/core:/stable:/v1.34/deb`; every real Filename shape adopts and vouches; flat `Packages` failure fatal; strict matrix; anchorless refresh tests; flat legacy onboarding on a v6 fixture (an untrusted flat suite makes ≤ 1 attempt per backoff step); reaping `/repo` leaves `/repo/.` intact; PR #8's remaining tests ported with the expected results in the plan overview's "PR #8 parity" section. |
| **H. End to end and docs** | H1: the sweep — every deferred review minor of phases A–G, the trust audit's corrupt anchors (R-H2), snapshot GC's audit-queued snapshots (R-H1), snake_case event names (R-H3, R-H6), the docs for its own changes; H2: `e2e/flat/`; H3: documentation (README, the strict-mode, unsigned and captive-portal docs; the config file was complete, `TestDefaultConfigFileDocumentsEveryKey`), the 1.1.0 release notes, the live-upstream smoke (`e2e/live`), the H1 test carries, the package upgrade's restart of a running daemon (the security review's H-2) and Go 1.27 (the maintainer's decisions), `docs/open-decisions.md`, final full-diff review. | `make e2e-flat` green on all images with the grep assertions, including PR #8's end-to-end table (one signed flat repository configured as `deb <uri>/ /` on one client and `deb <uri> ./` on another: both report 2.0 within one cooldown of the v2 publish, and with the upstream stopped two fresh clients install 2.0 from the snapshots); a manual live-upstream smoke (plan overview, "PR #8 parity"; `e2e/live`, green in phase H3's planning on 2026-10-26) before the release that ships G; docs complete. |

## 10. Out of scope / follow-ups

1. Default `adoption.enabled = false` freezes every repository's metadata
   (Phase 1 contract). Candidate: enable by default, or a Phase 1
   invalidate-on-change refresh.
2. Port-correct member URLs for non-default-port upstreams.
3. Host-root flat repositories; multi-segment `dists/` codenames.
4. Synchronous anchor revalidation; stage-then-flip `CommitAdoption` (F-2),
   which would also remove the barrier's slow-commit residual.
5. `Valid-Until` enforcement and per-suite identity pinning
   (`Origin`/`Label`/`Codename`) for the rest of C-1. Eager first adoption lets
   a replaying MITM choose the first snapshot's content (the `Date` rules
   protect only after the first adoption).
6. Flat Sources/pdiff hashing; flat Translation files; skip-repair records for
   SHA512-only members; backfilling SHA512 by-hash aliases for snapshots
   adopted before this release.
7. apt `--snapshot` IDs each become a `dists/` suite that eager adoption
   downloads in full (bounded by caps and reaping); a snapshot-server
   exemption.
8. Negative caching of metadata 404s (Release-less clients probe
   `.xz/.bz2/.lzma` and Translations on every update).
9. Downstream mirror tools requesting `Release`+`Release.gpg` from inline
   suites get 404 (pre-existing).
10. G-1, decided by the maintainer on 2026-10-24 and done: `POST /reconcile`
    takes `/forget`'s guard (`requireMutationGuard`), and without
    `htpasswd_file` both refuse a `Host` that is not an IP address,
    `localhost` or `admin.listen`'s host name (DNS rebinding); the
    read-only endpoints keep no `Host` check, so a Prometheus server may
    scrape by name. An operator behind a reverse proxy that forwards the
    client's `Host` sets `htpasswd_file` or has the proxy send
    `Host: 127.0.0.1` (phase F, ruling PF-6, had left it open).
11. First contact costs two upstream GETs of the anchor (the miss and the
    eager probe).
12. An adopted unsigned flat suite whose upstream stops publishing its
    `Release` keeps its last snapshot, visibly `anchor_absent`, whose old
    `Packages` may name `.deb`s upstream has since deleted (404), until
    `POST /forget` turns it into a Release-less suite the anchorless
    refresh serves (C4 Dispatch 1–2, documented in *Flat repositories*).
    Whether such a suite should drop its snapshot and refresh anchorless
    once its `Release` has stayed absent was decided for 1.1.0 by a
    controller ruling, confirmed by the maintainer on 2026-10-26: keep;
    revisit if seen in the field (phase H1, ruling R-H5, deferred minor GD-5): it is
    rare, the kept snapshot keeps serving cached content when an upstream
    dies outright, where a drop would turn that into 404s, and
    a drop is a new state transition with trust and serving consequences.

## 11. Risks

| Risk | Mitigation |
|---|---|
| Eager first adoption after upgrade (burst). | Interest window + dormant backfill; TryAcquire slots, per-host slot, reserved slot, `max_duration`; slot-driven admission (no idle slot, no re-probing while queued); failures back off. |
| Tightened `dists/` grammar drops suite identity for odd client paths. | Only shapes apt never sends; Phase 1 serving continues; enumerated tests. |
| Upstream request lines change for `+`/`~` paths. | They become exactly what apt sends directly; production-fixture round trip. |
| Unsigned adoption abused by a MITM or a LAN client. | Two observations (1 h for 401/403), signed-seen marker outliving reaps and written before signed bytes are served, never-downgrade in check and inside the commit transaction, member cap, capacity bounds, no vouching, no strict-mode effect; documented hardening knobs. |
| Trust derived before this release (R-1 pollution, plain-text inline snapshots) survives the upgrade. | §6.1 startup audit quarantines them before serving; resumable; tests on a polluted v6 cache. |
| The audit misclassifies clean snapshots and quarantines them. | Clean = decodable envelope whose raw and signed declarations agree (true of every real InRelease); tests use realistic multi-section envelopes; a quarantine only degrades the suite to Phase 1 serving until it re-adopts. |
| One host's failing checks starve other suites. | Fair worker pool with per-host round-robin, healthy-first FIFO classes and a failure streak and backoff persisted across restarts; a stated latency bound pinned by simulation. |
| The startup audit makes a whole fleet's proxy unavailable. | Only a database error or a missing, empty or foreign pool refuses to start; a single unreadable pool file defers that snapshot; the audit is one-time and batched. |
| Barrier stalls or times out clients. | Holds only in phase B, ≤ 20 s (< apt's 30 s), one hold at a time, bounded in-flight wait, waiter cap, cross-suite quiet in both phases; `0s` disables; counters. |
| Suite reaping removes a live suite, or a returning suite cannot re-adopt. | 30-day TTL, touches from anchors/indexes/vouched `.deb`s, compare-and-delete, hourly cap, uptime floor, snapshot retirement; a false reap self-heals on the next request; tests pin it. |
| New code paths leave a suite silently stuck (freeze-trap class). | An explicit, visible state for every refusal, `POST /forget`, liveness assertions in every simulation. |
| Suffix fallback cost. | One partial-index query, only when no exact row vouches the path; plan pinned by test. |
| v7 migration time. | Partial index build logged; seconds for ~1 M rows. |
