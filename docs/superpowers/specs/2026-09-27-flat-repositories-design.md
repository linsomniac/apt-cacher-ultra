# Flat repositories, eager adoption, and suite lifecycle — design

- **Date:** 2026-09-27 (revision 4, 2026-10-05/11: legacy trust audit, signed-seen ordering, fail-closed inline verification, flip hold token, fair scheduler — after the 2026-10-05 plan review, a follow-up review, and a three-lens review of grounding, alternatives and residual risk)
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
| C3 Suite rows and signals | One creation choke point with caps; client-interest stamp; signed-seen marker (written at adoption, at startup from cached anchors, and in the miss path's writer operation before a signed anchor is served). |
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
  documented Phase 1 contract (`TestCheck_NoAdopter_ChangeOnlyLogged`). Every
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
contains none of `#`, `?`, `%`, `\`.

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
empty): unsupported, logged by a globally rate-limited
`flat_host_root_unsupported`.

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
| `Release.gpg` | armor header `-----BEGIN PGP SIGNATURE-----`, or a binary OpenPGP signature packet (old-format tag bytes 0x88–0x8B, new-format 0xC2) |
| flat `Packages*`/`Sources*` | the compression magic for the extension, or plain text starting `Package:`/`Package: ` (`Sources`: `Package:` too) |

**Single creation choke point.** `cache.CreateSuite` is the **only**
`INSERT INTO suite_freshness`: create-only (`ON CONFLICT DO NOTHING`), sets
`last_requested_at = now`, and enforces two caps: `freshness.max_suites`
(default 5000) globally and `max_suites / 10` per canonical host. At the
global cap it first tries to evict (via the reaper path, under its mutex) the
least-recently-requested suite that has no current snapshot and has been
idle ≥ 24 h; otherwise it creates nothing and emits a rate-limited WARN
`suite_seed_rejected` ("suite will not refresh"), a counter and a gauge.
Rejections are cached in memory per suite key for 10 minutes (bounded map),
so an over-cap suite does not cost a writer operation per request.

Only the handler calls it, for a response whose bytes pass the sniff for
the path, on a miss **or** a `url_path` hit (so anchors cached before this
release are onboarded). For the anchor paths (`InRelease`, `Release`) the
row — and, for a signed anchor, the signed-seen marker — is created in the
**writer operation that stores the `url_path` row, before the body is
written to the client**: the miss path holds the whole body before serving,
so the order is sniff → `url_path` + `CreateSuite` + marker → write body →
T1. Today's handler already seeds its row inside `runFetch` before serving;
this keeps that order. A hit onboards a cached anchor (creates the row) the
same way. Flat index rows may be created after serving. Paths that create a
row:

- with an adopter wired: `<suite>/InRelease`, `<suite>/Release`, and flat
  `<D>/Packages*`/`Sources*` (a sniff-passing `<suite>/Release.gpg` writes the
  marker for a suite that already has a row but creates none: apt fetches it
  only after `Release`). The new row leaves `last_check_at` NULL and the
  handler fires T1 once the response is written, so eager adoption starts
  within seconds of first contact (subject to slots);
- with no adopter (Phase 1): `<suite>/InRelease` only, stamping
  `last_check_at = last_success_at = now` and the miss validators — today's
  seed, byte for byte, and no T1.

Everything else writes with UPDATE-only statements: `PutSuiteFreshness`
becomes `UpdateSuiteFreshness` (0 rows ⇒ no-op); `CommitAdoption` Step 6 is
UPDATE-only and rolls back with `ErrSuiteGone` if the row vanished;
`RecordAdoptionFailure`, `ClearAdoptionFailure` and `TouchSuite` are
UPDATE-only. `Check` and `Reconcile` require an **existing row** before
`LoadOrStore` (I-4) and re-validate after `TryLock` that the lock map still
holds the same mutex and the row still exists. The handler's by-hash
content-addressed fallback requires a known suite (I-4, handler side). A test
proves exactly one INSERT site.

**Client interest.** `suite_freshness.last_requested_at` (distinct from
`url_path.last_requested_at`; an `AIDEV-NOTE` says so) is written only by
`TouchSuite` from client-serving paths:

- anchor serves (`InRelease`, `Release`, `Release.gpg`; status < 400,
  including 304, HEAD and stale serves);
- for flat suites, root `Packages*`/`Sources*` serves (Release-less clients'
  anchor probes 404 and are never served);
- vouched `.deb` serves (`DeclaredHashesForPath` returns the suite path), so
  install-only clients keep their suite alive.

Touches are coalesced in memory (at most one write per suite per hour; the
map holds only tracked suites and is purged on reap).

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
- the handler (phase D), on the **miss path only**, inside the writer
  operation that stores the `url_path` row (and, on first contact, creates
  the suite row): for a sniff-passing `InRelease` or `Release.gpg` the marker
  is part of that operation, which completes before the body is served. If
  the operation fails the miss fails as it does today (502, no body), so a
  signed anchor body is never written to a client before its base has a
  marker — no timeout path, no residual window. The hit path writes no
  marker: every cached sniff-passing anchor row was created by a miss under
  this rule, seeded by `CommitAdoption` (which marks in its transaction),
  rebuilt by `reconstructAnchor` from a snapshot marked at commit or by the
  audit, or created by an earlier binary and marked by the startup pass
  before this binary serves; the blob behind a row is content-addressed, so a
  row that failed the sniff cannot pass it later. The writer is a FIFO queue
  drained by one goroutine (`cache.submitWrite`), so an unsigned
  `CommitAdoption` enqueued after a marker write runs after it, and its
  in-transaction re-read (C5) sees the marker.

A plain-text "InRelease" fails the sniff and never creates a marker. The
marker survives suite reaping: the reaper sweeps a marker only once no
suite row exists for its base or its `/.` twin and its `last_seen_at` is
older than 365 days (a live suite's marker is never swept, so no renewal
write is needed). `POST /forget` deletes it only if no other suite with the
same base has a row.

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
`freshness.max_suites` entries in all), served by a fixed pool of **4 check
workers** (constant):

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

**Response classification** (defined once, used everywhere):

| Class | Responses |
|---|---|
| *present* | 200 whose body passes the file's sniff (C3) |
| *absent* | 401, 403, 404, 410, or 200 failing the sniff |
| *unknown* | transport error, any other status (408, 429, 5xx, other 4xx, unfollowed 3xx), deadline — no form decision; the check fails (bump `last_check_at`) |

Counting 401/403/410 as *absent* is more lenient than apt's signed fallback
(404 only); for signed forms it can only cause a lateral move.

**Dispatch** (adopter wired; with no adopter the Phase 1 algorithm runs
unchanged, observation only):

1. **Current snapshot inline** — conditional GET `InRelease` as today;
   changed → C7 gate → adoption request.
   **Current snapshot detached** — also probe `InRelease` each check;
   *present* → inline adoption request (lateral). Then the conditional GET of
   `Release` as today.
   **Current anchor absent** (inline: `InRelease`; detached: `Release`) on two
   checks at least `max(cooldown, 5m)` apart → run the probe tree without
   validators and adopt the form found: inline ⇄ detached is a lateral move;
   never unsigned from signed. If no signed form is *present*, record the
   refusal state `anchor_absent` (visible; `last_success_at` not bumped;
   the snapshot keeps serving).
2. **Current snapshot unsigned** — probe `InRelease` and `Release.gpg`; if
   either is *present* → signed adoption request (upgrade). Otherwise
   conditional GET `Release`; changed → unsigned adoption request (no
   re-confirmation). If a signature is present but the signed adoption
   cannot succeed (key not on the cache host), the suite keeps its last
   unsigned snapshot and the admin page shows the reason; the fix is
   installing the key (or `adoption.accept_any_signer`).
3. **No current snapshot (eager first adoption)** — only if
   `last_requested_at` is within a constant **7-day interest window**
   (independent of GC settings) and the suite has a client-created anchor or
   flat index `url_path` row (`request_count > 0`; never reconstructed).
   Probe tree without validators:
   - `InRelease` *present* → inline request;
   - `InRelease` *absent* → `Release`: *present* → `Release.gpg`: *present* →
     detached request; *absent* → unsigned candidate (C5);
   - `Release` *absent* → for flat suites that have no signed-seen marker and
     no sniff-passing cached anchor row, the **anchorless refresh**; otherwise
     a failed check;
   - any *unknown* → failed check.
4. **Anchorless refresh** (flat, Release-less suites only): conditional GET
   (stored `upstream_etag`/`upstream_lastmod`) of each cached `url_path` row
   whose `SuitePath` equals the suite and whose member is a
   `Packages*`/`Sources*`/`Contents-*` file. A 200 replaces the row's blob
   only if the body passes the flat index sniff for its extension (else the
   old row stays; counter `anchorless_sniff_rejected`); 404/410 deletes the
   row; 401/403/5xx keep it. A dedicated cache method preserves
   `last_requested_at` and `request_count` and sets `last_fetched_at` only
   when the blob changes. Cached anchor rows under the suite are deleted when
   upstream answers them 404/410. No snapshot, no vouching, no strict-mode
   effect. If a `Release`/`InRelease` later becomes *present*, the suite moves
   to the normal forms. `url_path`-served metadata uses
   `max(pool mtime, last_fetched_at)` as `Last-Modified`, so apt's
   `If-Modified-Since` on `Packages*` sees a replacement.

**Adoption capacity.** Adoption, repair and reconcile obey one discipline.
Before spawning the goroutine the check `TryAcquire`s the global slot
(`max_concurrent_adoptions`) and a per-canonical-host slot (one per host). If
either is busy it spawns nothing, records `result = changed_queued` (or
`first_adoption_queued`; repair/reconcile simply skip until the next
unchanged tick) and does **not** advance `last_success_at`, so the scheduler
retries it in its rotation turn (above): admission rotates through queued
suites instead of favouring scan order. When `max_concurrent_adoptions ≥ 2`, the last free
global slot is reserved for suites that have a signed current snapshot
(security updates are never starved by first or unsigned adoptions). The
host slot is released when the member pipeline ends (before the C8 barrier);
the global slot is held through the commit. Every adoption, repair and
reconcile runs under a context deadline `adoption.max_duration` (default
`30m`); `CommitAdoption` itself runs under `context.WithoutCancel` with its
own bounded timeout, so the deadline cannot abort a commit already in
progress. `POST /reconcile` answers 409 when no slot is free.

**Validators.** `suite_freshness` validators always describe the current
snapshot's anchor (set in the flip transaction), or the `url_path` anchor
when unadopted. They are never written from a changed or deferred response
(C7 keeps its own).

**`inrelease_change_seen_at`** is set only when NULL (first sight of a
pending change), cleared in the flip transaction and on
`not_modified`/`unchanged`.

**Clock-step rule** (fixes I-7 in all touched gates). A *past-event* stamp
(`last_check_at`, `last_success_at`, `last_requested_at`,
`adoption_fail_last_at`, `adoption_fail_first_at`, `adopted_at`) found in the
future is treated as infinitely old by gates that *delay* work (cooldown, T2
threshold) and as "now" by gates that *retire* things (reap cutoff).
`next_retry_at` is only clamped to `now + 1h` (C7). Each detection emits a
rate-limited WARN `clock_step_detected`. Touch coalescing and all in-memory
timing use monotonic time.

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

**Filenames.** `ParsePackages` uses `validateFilename` (Packages/Sources
Filenames only; `ParseRelease` keeps `validateMemberPath`):

- allow `.` segments (kept literally: apt requests `<base>/./x.deb`) and `%`
  (literal bytes; C1 re-encodes it upstream; requests are compared after one
  decode);
- reject, fatal as today: empty, absolute, NUL, control bytes, backslash,
  `?`, `#`, `..` segment, empty segment, length > 4096.

**Hash blocks.** `ParseRelease` merges hash blocks per member path, as apt
does: `ReleaseMember` carries `SHA256` and `SHA512`; the size comes from the
SHA512 entry if present, else SHA256; entries with only MD5/SHA1 are ignored
(as today). One shared helper, used by `runShared`, reconcile and repair,
verifies the fetched bytes against **every** declared SHA256/SHA512 hash,
returns the SHA256 of the verified bytes (a *derived* anchor for SHA512-only
members; controlling it needs a SHA512 preimage) and the by-hash alias paths
for **every** declared algorithm. A Packages stanza with a Filename but no
SHA256 makes coverage incomplete (reason `no_sha256`), never a silent drop.
Limitation (documented): integrity-skipped SHA512-only members are not
recorded for skip-repair; reconcile still heals requestable IndexTargets.

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
says. Unsigned metadata is adopted only through the explicit unsigned form
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

1. `adoption.adopt_unsigned` (default `true`).
2. Eligible only when `InRelease` and `Release.gpg` are both *absent* and
   `Release` is *present*. The **transition into** unsigned (a suite with no
   current snapshot) needs two observations of the same `Release` bytes at
   least `max(cooldown, 5m)` apart for 404/410, or at least 1 h apart when
   either signature file answered 401/403 (WAF and S3-permission incidents);
   the first observation records reason `unsigned_unconfirmed` and the
   comparison uses `adoption_fail_first_at` (restart-safe). Updates of an
   established unsigned suite adopt at once.
3. **Never downgrade:** refused (`unsigned_downgrade_refused`, a refusal
   state) when a `suite_signed_seen` marker exists for the suite's base. The
   marker outlives reaping, covers the `./` twin, and is committed in the
   writer operation that stores a signed anchor, before any client is served
   it (C3). The check runs twice:
   at eligibility, and again **inside the `CommitAdoption` writer
   transaction**, which re-reads the marker for the candidate's base and
   refuses an unsigned candidate (no pointer change, no rows) when one exists
   or when the prior current snapshot is signed. The suite mutex alone cannot
   close this race: the handler and the `/.` twin establish signed history
   without it.
4. At most 1,000 declared members (`unsigned_too_large`).
5. No `package_hash` rows on **any** path (adoption, repair, reconcile:
   `buildPackageHashes` takes the form); `DeclaredHashesForPath` and
   `HostCurrentSnapshotsCoverage` filter `anchor_form <> 'unsigned'`.

Threat notes (documented at the knob): the signed-seen marker cuts both
ways — anyone who can make the cache serve one sniff-passing `InRelease`
for a base (a MITM, a compromised upstream, or a LAN client pointing the
cache at a host it controls) blocks unsigned adoption of that base until
`POST /forget`; this denial of service is the accepted price of never
downgrading. A MITM on the cache→upstream path can
make the cache adopt forged unsigned content only for a suite never seen
signed, after two consistent observations; verifying clients reject it,
`[trusted=yes]` clients accept it as they would fetching directly. Under the
default allow-all upstream policy with adoption enabled, a LAN client can make
the cache adopt unsigned directories it controls; this is bounded by the
per-host slot, the reserved slot, `max_duration`, the member cap and the suite
caps. Operators who need more can set `adopt_unsigned = false` or narrow
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
   contains `..` or an empty segment or has more than 32 segments. For each
   split `P' = B + "/" + F` (`B` may be `""`, the host root), query rows with
   `flat_filename <> '' AND flat_filename IN (F…)` on `(scheme, host)` (the
   `<> ''` makes SQLite use the partial index; an `EXPLAIN QUERY PLAN`
   assertion pins it), join `suite_snapshot`, and keep a row iff `B` is an
   ancestor-or-self of that suite's `FlatBase`
   (`strings.HasPrefix(base+"/", B+"/")`). Among kept rows keep those with
   the longest `B` (the most specific base).
3. The existing dispatch applies: 0 distinct hashes → strict-mode predicate;
   1 → validate; ≥ 2 → `package_hash_conflict` 502.

This is apt's rule (`.deb` URL = `<uri>/<Filename>`, `<uri>` an
ancestor-or-self of the Release directory) with one row per stanza. It covers
k8s and CUDA (exact), CUDA's dot-cleaned form, Jenkins `binary/` and
`direct/` (base = parent) and Sublime `files/` (base = host root). Rows are
constraints, not grants: a match only lets bytes through if they hash to a
signed declaration. Parent-base repos pay the fallback query on every `.deb`
hit.

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
mutex is released; a shutdown context cancellation is not recorded. `count`
and `first_at` describe the failure streak since the last success.

**Content key.** inline: sha256(InRelease); detached: sha256(sha256(Release)
‖ sha256(Release.gpg)); unsigned: sha256(Release). Any byte change is new
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
`unsigned_unconfirmed` (confirmation timing from `adoption_fail_first_at`),
`unsigned_too_large`, `anchor_absent`. They are excluded from the
failing-suites gauge and from `adoption_failing_persistently`, and shown as
"refused: …" in the admin page.

**Gate** (in every branch that would spawn an adoption). While
`now < next_retry_at` the check sends the *failing content's* validators: a
304 or a 200 with the same key ⇒ `changed_deferred` (no clear, no repair);
new content ⇒ attempt. At or after `next_retry_at` the check GETs without
validators and attempts. The record is cleared in the flip transaction and
when upstream again serves the current snapshot's content.

**Restart.** At checker start every failing row (not refusal states, not
reason `panic`) gets `next_retry_at = now + rand(0, periodic_refresh)`: one
retry per failing suite, the recovery path after an operator installs a key
or changes pins.

**Visibility.** Admin `suites[]` gains `anchor_form`, `flat`,
`last_requested_unixtime`, and the record ("adoption failing 3d 2h ·
untrusted_signer · retry in 41m", or "refused: upstream_older"). "Lagging" is
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
  at most `15s`), and (b) **no hold is in progress anywhere** and none ended
  within the same period (a hold delays other suites on the client's shared
  queue). Bounded by 60 s; on timeout proceed to phase B.
- **Hold token.** Before phase B the flip acquires the process-wide hold
  token (FIFO; at most one suite holds at a time). Waiting for it is bounded
  by 60 s; on timeout the flip commits without a hold (counter
  `acu_flip_hold_skipped_total`). Concurrent holds are what let one suite's
  hold push a shared client queue past another suite's quiet window (two
  busy suites holding at once, each committing on its own local quiet).
- **Phase B — hold.** Mark the flip pending (new anchor requests wait). Wait
  until (a) no anchor of this suite is in flight and none completed within
  `flip_quiet_period` (reachable, because new anchors are held), and (b) no
  *other* suite's hold ended within `flip_quiet_period` (clients released
  from that hold may still have this suite's index requests queued behind
  it). Then run `commit`, clear pending, wake waiters onto the new snapshot
  and release the token. The whole of phase B, including both waits, is
  bounded by 20 s from its start: at expiry pending is cleared, waiters are
  answered from the current snapshot, a commit not yet started starts
  immediately (counter `acu_flip_hold_expired_total`), and the token is
  released. A flip therefore takes at most 60 s + 60 s + 20 s plus its
  commit.

Skipped when `flip_quiet_period = 0s` (today's behaviour) and for a first
adoption whose anchor bytes equal the `url_path` anchor clients have been
served (the flip does not change what they hold). A first adoption stamps
`adopted_at = max(now, url_path anchor last_fetched_at + 1)` so clients'
`If-Modified-Since` still sees a change.

**Guarantee.** A client that received the old anchor finished receiving it at
least `flip_quiet_period` before the flip, or was held and answered from the
new snapshot, so its index requests within that period come from the snapshot
matching its anchor. Because holds never overlap and every commit waits a
quiet period after the last hold ended, the proxy's own holds cannot push a
client's queued requests for another suite past that suite's flip.
Residuals (documented, counted): phase-B expiry when the commit or an
in-flight reader exceeds 20 s (F-2 on very large suites), hold-token timeout
(more than three suites flipping at once), waiters beyond the cap, and anchor
reads slower than apt's own timeout. A flip waiting for the token keeps its
global adoption slot, so with several suites flipping at once other
adoptions can queue for up to about 140 s (bounded).
by-hash clients remain covered by retained by-hash. Answering held requests
with `503 Retry-After` instead was rejected: apt before 2.3.2 treats a 5xx as
an item failure without retrying, while answering from the current snapshot is
today's behaviour. Map entries exist only for suites with rows, expire when
idle for twice the quiet period, and are purged on reap.

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
snapshot-served anchors (`InRelease`, `Release`, `Release.gpg`) use
`max(pool mtime, adopted_at)` as the `ServeContent` modtime, so a client's
`If-Modified-Since` from the previous anchor always gets a 200 after a flip,
even across backward clock steps. `adopted_at`'s other readers (GC ranking per
suite, admin display) are unaffected.

The README's "clients always see a coherent metadata set" claim is replaced
with this guarantee and its residuals.

### C9. Suite lifecycle (limitation #3)

**Snapshot retirement.** `suite_snapshot.retired_at` marks every snapshot of a
suite incarnation that was reaped or forgotten, in the same transaction that
removes the suite row. Retired snapshots are invisible to everything that
reasons about a suite's history: the natural-key uniqueness (partial index
`WHERE retired_at IS NULL`), `InsertCandidateSnapshot`'s collision lookup,
`rollback_refused`, retained by-hash, `DeclaredHashesForPath`,
`HostCurrentSnapshotsCoverage` and snapshot-GC sub-job B. A returning or
forgotten suite therefore re-adopts unchanged upstream content at once, and a
deliberate rollback adopts after `/forget`.

**Reaper.** Its own hourly ticker under the freshness checker's lifecycle
(independent of `gc.interval` and of the T2 loop), after a monotonic uptime
floor of 1 h, at most 25 suites per run (constant), oldest first. Candidates:
`last_requested_at < now − gc.suite_idle_ttl` (default `720h`; `0s` disables),
plus rows whose `suite_path` is non-canonical under the tightened grammar
(unreachable legacy rows, reaped regardless of idleness). For each: `TryLock`
the suite mutex (busy → skip), then:

1. delete the suite's metadata `url_path` rows — rows where
   `aptpath.SuitePath(path) == suite` (never a twin's or nested suite's) —
   plain metadata first, anchors last, in batches of 1,000 rows per writer
   transaction without touching the suite row;
2. in one final writer transaction `cache.ReapIdleSuite`: re-read the row and
   bail if the stamp is newer than the cutoff or `current_snapshot_id` changed
   since the candidate was listed (compare-and-delete); run the Step-8 refcount
   decrement for the current snapshot; set `retired_at` on all of the suite's
   snapshots; delete the `suite_freshness` row.

Then it purges in-memory state (lock-map `CompareAndDelete`, flipgate entry,
touch-coalescing entry). It also sweeps signed-seen markers whose base and
`/.` twin have no `suite_freshness` row and whose `last_seen_at` is older
than 365 days, and the reconcile memo (against current snapshot ids).
The gpg once-logger maps reset when they exceed 10,000 entries. Snapshot GC
gains sub-job C: retired snapshots are deleted with their members and
`package_hash` rows in bounded chunks (10,000 rows per writer transaction),
then the header row.

**Migration backfill** of `last_requested_at`: the newest client
`last_requested_at` of the suite's anchor `url_path` rows, else migration time
minus the 7-day interest window. Dormant never-adopted suites are therefore
neither eagerly adopted after upgrade nor kept alive; they are reaped about 23
days later.

**`POST /forget` (D9).** Admin endpoint (`host`, `suite`). It requires a
non-simple request — header `X-ACU-Admin: 1` — and rejects any
`Sec-Fetch-Site` other than `same-origin` or `none`, so a web page cannot
trigger it (security G-1 class). Under the suite mutex it runs the reaper's
steps to completion (all batches, then the final transaction without the idle
checks) and deletes the signed-seen marker for the suite's base if no other
suite with that base has a row. It answers 202 on success and 409 for both
busy and unknown suites (no existence oracle). It is the documented escape
hatch for refusal states: a vendor that stopped signing, a deliberate upstream
rollback, a stuck unsigned suite. (`POST /reconcile` keeps its existing
protection; hardening it is part of G-1, §10.)

### C10. Strict mode

The host-wide rule is kept: refuse an unvouched `.deb` iff every current
signed non-retired snapshot on the host has complete coverage (PR #8's
directory scoping is not reimplemented). Unsigned snapshots, anchorless
suites and seeded-but-unadopted suites never count and never vouch. Flat
snapshots now normally have complete coverage, so strict mode protects flat
hosts.

**In-flight adoptions (D10).** The checker tracks in-flight adoptions per
canonical host. On the hit path, before answering `unvouched_deb_refused`,
the handler waits up to 15 s for in-flight adoptions on the request's host,
then re-runs the lookup once. On the miss path, where the strict decision sits
inside singleflight after host-slot acquisition by design, `runFetch` returns
`errUnvouchedRetryable` (releasing its slot) when an adoption is in flight on
the host; `serveCacheMiss`, outside singleflight, waits up to 15 s and retries
the miss once. At most 64 waiters per host; beyond that the refusal is
immediate.

Documented consequences: an adoption that takes longer than 15 s (large
`dists/` suites), a queued (not yet running) first adoption, and an unsigned
or anchorless repository on a host that also serves complete signed suites
all lead to refusals; strict posture changes as eager adoptions land after
upgrade.

### C11. Observability and configuration

**Events** (documented in `docs/log-fields.md`): `suite_seed_rejected`,
`suite_reaped`, `suite_forgotten`, `adoption_deferred`,
`adoption_failing_persistently`, `adoption_refused` (reason),
`adoption_flip_barrier`, `adoption_panicked`, `clock_step_detected`,
`flat_host_root_unsupported`, `legacy_snapshot_quarantined`,
`legacy_trust_audit_progress`, `legacy_trust_audit_deferred`,
`legacy_trust_audit_completed`; new fields `anchor_form`, `flat`,
`snapshot_displaced`. New metrics are host-labelled or unlabelled (no suite
labels) and are added to the metrics inventory comments in
`internal/freshness/metrics.go` and `internal/handler/metrics.go`.
`gc_run_complete` and `LastRunSummary` gain `retired_reaped`.

| Key | Default | Rule |
|---|---|---|
| `adoption.adopt_unsigned` | `true` | bool |
| `adoption.flip_quiet_period` | `"10s"` | `0s` disables the barrier; ≤ `15s` |
| `adoption.max_duration` | `"30m"` | ≥ `1m` |
| `freshness.max_suites` | `5000` | ≥ 10 (per-host cap = `max_suites / 10`, documented for large Artifactory/aptly/Nexus hosts) |
| `gc.suite_idle_ttl` | `"720h"` | `0s` disables reaping; else ≥ `24h` |

Constants, not knobs: backoff 2 m → 1 h (adoption failures and repeated
*unknown* checks); hold 20 s; phase A cap 60 s; hold-token wait 60 s; waiter
caps 256 (barrier) and 64 (strict wait); strict wait 15 s; reaps 25 per run;
unsigned member cap 1,000; check deadline 2 min; check workers 4; interest
window 7 d; orphan-marker TTL 365 d; cap-rejection cache 10 min;
legacy-audit anchor read cap 8 MiB and batch 100.

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
directory must be readable, and when two or more inline anchors are queued
at least one of them must exist; an unmounted, empty or foreign pool would
otherwise make every anchor look missing, so startup refuses with a message
naming the path and the two ways out (restore the pool, or wipe the cache)
and changes nothing. A single queued inline anchor that is missing is
quarantined (one lost pool file cannot be told from a lost mount, and
refusing would leave no recovery short of wiping the cache); detached-only
queues read no pool files. Each batch of up to 100 is decided in memory and
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
  set `retired_at`; dequeue. WARN `legacy_snapshot_quarantined` (reason,
  snapshot, host, suite, `was_current`); counter
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
is kept) until it re-adopts. Re-adoption happens on the first check that
observes anchor bytes different from the cached ones (phases A–D) or by
eager first adoption (phase E). For an R-1 quarantine that is the first
post-upgrade check: the cached anchor holds the tampered envelope, which a
clean upstream does not serve, so the check sees *changed* and adopts the
clean content with the fixed parser. An `anchor_missing`, `anchor_corrupt`
or `anchor_oversized` quarantine of a suite whose upstream is unchanged, and
a legacy plain-text suite, stay unadopted until upstream changes or phase E
(a vendor that never republishes stays unadopted until then); with
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
(`gc.interval = 24h`), with restarts.

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
  adoptions, `reconstructAnchor` and restarts, every `url_path` anchor row
  whose blob passes the sniff has a marker for its base); FIFO ordering (a
  marker write enqueued before an unsigned commit is visible to the commit's
  transaction).
- C4: classification table (every status class per anchor); form transitions
  both ways incl. detached → inline and the two-observation rule;
  `anchor_absent` visible and not silent; anchorless refresh (IMS, sniff
  rejection, 404 deletes, request counts preserved); capacity: 2 slow-drip
  suites + 20 normal suites + 1 degraded suite → every normal suite adopts
  within `max_duration + cooldown`, the degraded suite's checks continue,
  goroutines ≤ cap + constant, the reserved slot serves a signed suite;
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
  anchor is served and its marker committed (also via the `/.` twin) is
  refused with no pointer change; `require_signature = false` with
  marker-present malformed or frameless bodies creates no inline snapshot,
  `package_hash` row or strict participant (`malformed_clearsign` distinct
  from `missing_signature`); `TestCheck_UnsignedRecoversWhenReleaseAnchorMissing`;
  guard (d) keeps an unsigned suite's `/Release`.
- C6: every real Filename shape vouches; nested-base tiebreak; `..`/`//`/
  sibling `/flat2` negatives; `EXPLAIN QUERY PLAN` uses the partial index.
- C7: lim1 reproduction (≤ ~30 attempts/day vs ~1,440 on main); a T1 storm
  right after a failure causes zero extra attempts; 30-day simulation
  (untrusted body, new content, rollback, clock step); flapping mirror → zero
  backward flips; restart spread; panic excluded from reset; deferred-then-
  retry liveness with 304s to the failing validators.
- C8: the overlapping-hold trace (two busy suites entering holds 0.1 s
  apart, a shared client queue holding one suite's old anchor) → zero
  straddles; hold-token timeout commits without a hold and counts it;
  plain-path straddle reproduced on the phase-B tip and fixed; busy suite
  (an anchor every 500 ms for 120 s across a flip) → zero straddles;
  mirror-mode two suites with a hold in progress → zero straddles; slow reader
  → phase B ends within 20 s and the flip commits; hold never exceeds 20 s;
  idle zero-wait; client cancel; shutdown; commit failure releases waiters;
  the chaos test's straddle count asserted zero; retained by-hash (SHA256 in
  C, SHA512 in E) with stray-blob and retired-snapshot negatives;
  `Last-Modified` across same-second flips, first adoption and a backward
  step.
- C9: 90-day simulation at `gc.interval = 24h` with 12-hourly restarts
  (touched / idle / near-TTL / busy / clock jump); a reaped suite that returns
  before any GC tick with unchanged upstream re-adopts within one check;
  `/forget` then an immediate request re-adopts, and after an upstream
  rollback adopts the older content; a +30 d forward step's false reaps all
  self-heal; `gc.enabled = false` still re-adopts; never-downgrade after a
  reap; batched metadata deletion never repeats the refcount decrement;
  reap-vs-adoption chaos; blobs reclaimed within two GC ticks; twin and
  nested suites untouched; `/forget` rejects a cross-origin form POST.
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
- C10: strict matrix per `sources.list` form × Filename shape; mixed host;
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
`dists/` repo. Signed repos live on one host name, unsigned and Release-less
repos on a second, so strict mode's expectations are explicit; a mirror-mode
run and a two-suites-one-host flip are included. Assertions: installs
succeed; the exact request lines apt sends are pinned (`./`, `%253a`,
`by-hash/SHA512`); `apt-get` output is grepped for `Failed to fetch|Connection
failed|timed out|Hash Sum mismatch|Mirror sync in progress` (exit status is
useless); zero `unvouched_deb_refused` on the signed host; a v2 publish of a
signed or anchorless repo reaches clients within one cooldown; an older-Date
publish is ignored; state survives a restart. A commit-delay hook of 45 s
asserts no client timeouts, with its own scoped grep (the phase-B-expiry
straddle is a documented residual). The unsigned first-adoption scenario
(5-minute confirmation) runs on the oldest and newest images only.
`go test -race ./...`, `golangci-lint run ./...` and `gofmt` are clean at
every phase.

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
- `AIDEV-NOTE`s at every changed invariant; `Contributors.md` credit.

## 9. Implementation phases

Each phase leaves shippable state: `go test -race ./...`, lint and `gofmt`
clean; its own config keys, docs entries and `AIDEV-NOTE`s; a max-effort
review of the phase diff; fixes; a commit. The DoD lists named tests; no DoD
item depends on a later phase.

| Phase | Items | Definition of Done (highlights) |
|---|---|---|
| **A. Foundations** | v7 migration (final schema, Go backfill step, trigger) and Go types incl. the `SingleAnchorHash`/`Form`/`ReleaseDate`/`RetiredAt` fields; `aptpath` and `wallclock` packages; handler fetch seam; C1 complete (changes `+`/`~` request lines to apt's form); C2 `dists/` tightening (no flat grammar yet); `%w` wraps and sentinels; R-1; I-1 recover (logging) in adoption, repair, reconcile, T1 and scheduler goroutines; clock-step rule in existing gates; synctest harness + smoke simulation; inline verification fails closed; signed-seen markers written at adoption and by the startup marker pass (with the `InRelease`/`Release.gpg` sniffs and `MarkSignedSeenBatch`); §6.1 legacy trust audit with quarantine. | v6-fixture migration test; no-SQLite-clock test; C1 tables and production-fixture round trip; existing suite green except enumerated `SuitePath` tightening cases and the `require_signature = false` passthrough tests; R-1 and I-1 fixtures fail on main, pass here; polluted-v6 audit test (current and displaced, plain text, interruption); 30-day smoke simulation < 5 s under `-race`. |
| **B. Retry discipline and capacity** | C7 record, gate, schedule, refusal states, restart; `Date` rules; validators at commit; `change_seen` semantics; capacity (TryAcquire global + host slots for adoption, repair and reconcile; reserved slot; `max_duration`; `WithoutCancel` commit; queued results); panic recorded; fair concurrent scheduler (C4: per-host queues, 4 workers, one check per host turn, healthy → never-checked → failing, FIFO, persisted failure streak and backoff); admin, gauge, logs. | lim1 reproduction bound; T1-storm test; 30-day simulation incl. flapping mirror and clock step (liveness + bounds); slow-drip + degraded-suite capacity simulation exercising both check and adoption admission; scheduler latency-bound simulations with stalling (not refusing) responders: H = 8 with n = 3 healthy suites ahead; 500 failing + 1 healthy suite on one host, repeated after a simulated restart; "lagging" visible. |
| **C. Flip consistency** | C8 flipgate with the hold token, handler anchor hook, retained by-hash (SHA256, non-retired only), monotonic `Last-Modified`. | Straddle test fails on the B tip, passes here; overlapping-hold trace in a shared-queue simulation; five busy suites flipping within 0.1 s (holds never overlap, at most three hold, the rest commit unheld within 60 s + 20 s and are counted, zero straddles for held suites); busy-suite, cross-suite and slow-reader tests; hold ≤ 20 s; chaos straddles = 0; `flip_quiet_period = 0s` keeps existing tests unchanged. |
| **D. Creation bounds and signals** | C3 in full: sniffs, `CreateSuite` with caps (anchor rows and markers before the body, including first contact), eviction and rejection cache, legacy onboarding, adoption-disabled seed parity, UPDATE-only writers, I-4 (both sides), touch (anchors, vouched `.deb`s), signed-seen marker in the miss path's writer operation; `Release` and flat-index sniffs; `clearsignedFixture()` migration. | Single-INSERT-site test; minted-suite scale test; `dists/` onboarding test on a v6 fixture; adoption-disabled parity test. |
| **E. Eager adoption and anchor forms** | C4 probe tree, classification, form transitions, `anchor_absent`, eager first adoption with interest window, T1-on-seed; C5 unsigned form with safeguards; SHA512 merge, shared verify helper, aliases, retained by-hash SHA512; C10 in-flight wait. | Production-shaped fixture: active never-adopted suites adopt once, dormant ones make zero requests; classification table; never-downgrade at both points, including the in-transaction recheck race; a §6.1-quarantined plain-text suite adopts unsigned without a downgrade refusal; unsigned never changes posture; nvidia-shaped fixture adopts; SHA512 by-hash served; in-flight wait on hit and miss paths. |
| **F. Suite reaping** | C9 retirement, reaper (own ticker), batched metadata deletion, sub-job C, memory purge, marker TTL, `POST /forget`. | 90-day simulation at `gc.interval = 24h` with restarts; return-before-GC and `/forget` re-adoption tests; forward-step self-heal; never-downgrade after reap; `/forget` CSRF test; twins untouched. |
| **G. Flat repositories** | C2 flat grammar; flat IndexTargets (layout-aware, adoption and handler sides); `validateFilename`; C6 materialisation, fallback, coverage; anchorless refresh; flat root-index touch; flat pdiff arch labels. | PR #8's evidence: a miss on `/…/deb/InRelease` and on `/…/deb/./InRelease` creates one suite row each and fires T1 under it, a hit fires T1 again, and `/…/deb/Packages_1.0_all.deb`, host-root `/InRelease` and `/…/deb//InRelease` create none (no check fires for either path on `main`); `/core:/stable:/v1.34/deb/InRelease` → suite `/core:/stable:/v1.34/deb`; every real Filename shape adopts and vouches; flat `Packages` failure fatal; strict matrix; anchorless refresh tests; flat legacy onboarding on a v6 fixture (an untrusted flat suite makes ≤ 1 attempt per backoff step); reaping `/repo` leaves `/repo/.` intact. |
| **H. End to end and docs** | `e2e/flat/`; documentation; config file; final full-diff review. | `make e2e-flat` green on all images with the grep assertions, including PR #8's end-to-end table (one signed flat repository configured as `deb <uri>/ /` on one client and `deb <uri> ./` on another: both report 2.0 within one cooldown of the v2 publish, and with the upstream stopped two fresh clients install 2.0 from the snapshots); docs complete. |

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
10. G-1: CSRF protection for `POST /reconcile` (the new `/forget` has it).
11. First contact costs two upstream GETs of the anchor (the miss and the
    eager probe).

## 11. Risks

| Risk | Mitigation |
|---|---|
| Eager first adoption after upgrade (burst). | Interest window + dormant backfill; TryAcquire slots, per-host slot, reserved slot, `max_duration`; failures back off. |
| Tightened `dists/` grammar drops suite identity for odd client paths. | Only shapes apt never sends; Phase 1 serving continues; enumerated tests. |
| Upstream request lines change for `+`/`~` paths. | They become exactly what apt sends directly; production-fixture round trip. |
| Unsigned adoption abused by a MITM or a LAN client. | Two observations (1 h for 401/403), signed-seen marker outliving reaps and written before signed bytes are served, never-downgrade in check and inside the commit transaction, member cap, capacity bounds, no vouching, no strict-mode effect; documented hardening knobs. |
| Trust derived before this release (R-1 pollution, plain-text inline snapshots) survives the upgrade. | §6.1 startup audit quarantines them before serving; resumable; tests on a polluted v6 cache. |
| The audit misclassifies clean snapshots and quarantines them. | Clean = decodable envelope whose raw and signed declarations agree (true of every real InRelease); tests use realistic multi-section envelopes; a quarantine only degrades the suite to Phase 1 serving until it re-adopts. |
| One host's failing checks starve other suites. | Fair worker pool with per-host round-robin, healthy-first FIFO classes and a failure streak and backoff persisted across restarts; a stated latency bound pinned by simulation. |
| The startup audit makes a whole fleet's proxy unavailable. | Only a database error or a missing/empty pool refuses to start; a single unreadable pool file defers that snapshot; the audit is one-time and batched. |
| Barrier stalls or times out clients. | Holds only in phase B, ≤ 20 s (< apt's 30 s), one hold at a time, bounded in-flight wait, waiter cap, cross-suite quiet in both phases; `0s` disables; counters. |
| Suite reaping removes a live suite, or a returning suite cannot re-adopt. | 30-day TTL, touches from anchors/indexes/vouched `.deb`s, compare-and-delete, hourly cap, uptime floor, snapshot retirement; a false reap self-heals on the next request; tests pin it. |
| New code paths leave a suite silently stuck (freeze-trap class). | An explicit, visible state for every refusal, `POST /forget`, liveness assertions in every simulation. |
| Suffix fallback cost. | One partial-index query, only when no exact row vouches the path; plan pinned by test. |
| v7 migration time. | Partial index build logged; seconds for ~1 M rows. |
