# Flat repositories, eager adoption, and suite lifecycle — design

- **Date:** 2026-09-27 (revision 2, after a five-lens design review)
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
| C2 Suite identity | Whole-directory flat suites; tightened `dists/` grammar. |
| C3 Suite rows and signals | One creation choke point with caps; client-interest stamp; signed-seen marker. |
| C4 Freshness check | Probe tree in apt's order; eager first adoption; form transitions; anchorless refresh; clock-step rules. |
| C5 Adoption input | Flat IndexTargets; `./` and `%` Filenames; SHA512 hash blocks; `Date` rules; R-1 fix; I-1 panic recovery; unsigned form. |
| C6 Flat package coverage | Exact path + Filename-suffix fallback. |
| C7 Retry discipline (limitation #1) | Content-keyed failure record, one backoff schedule, refusal states, adoption capacity bounds. |
| C8 Flip consistency (limitation #2) | Two-phase flip barrier bounded below apt's timeout; by-hash from retained snapshots; monotonic `Last-Modified`. |
| C9 Suite lifecycle (limitation #3) | Hourly reaper; suiteless snapshot GC; `POST /forget`. |
| C10 Strict mode | Host-wide rule kept; unsigned/anchorless excluded; bounded wait for in-flight adoptions. |

One additive schema migration (v6 → v7) carries all persistent state (§6).
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
   in-memory map or table grows or churns without bound; restarts, upgrades
   and clock steps are safe.
4. Every behaviour is proven by tests that assert **both** upper bounds
   (cost, growth) and lower bounds (liveness), including multi-day virtual-clock
   simulations and a real-apt end-to-end suite across apt versions.

### Non-goals (§10 lists follow-ups)

- Changing the shipped default `adoption.enabled = false`. **With adoption
  disabled no repository (flat or `dists/`) refreshes its metadata** — the
  documented Phase 1 contract (`TestCheck_NoAdopter_ChangeOnlyLogged`). Every
  refresh mechanism in this design, including the anchorless refresh, requires
  adoption to be enabled; the docs say so prominently.
- Host-root flat repositories (`deb https://h/ /`).
- Merging the `/repo` and `/repo/.` twin suites (accepted as two suites).
- Synchronous revalidation of anchors on the request path.
- Re-activating a previously adopted snapshot when upstream rolls back
  (rejected: apt itself refuses an older `Date`, §C5).

## 3. Background and verified facts

Two review rounds (eight analysis agents on PR #8 and main; five design
reviewers on revision 1, several running real apt 2.8.3 / 3.0.3 / 3.2.0 /
3.3.2 against a logging proxy) established the facts below. Finding IDs such
as S-3, I-1, I-4, I-5, I-7, R-1, F-2, B-3 and C-1 refer to the 2026-09
security review (`docs/security-review-findings.md`, maintained outside this
branch).

### apt behaviour (apt source and probes)

| `sources.list` | Metadata requested | `.deb` requested |
|---|---|---|
| `deb <uri>/ /` | `<uri>/InRelease`, `<uri>/Packages.gz` | `<uri>/<Filename>` |
| `deb <uri> ./` | `<uri>/./InRelease`, `<uri>/./Packages.gz` (literal `./`) | `<uri>/<Filename>` |
| `deb <uri> sub/` | `<uri>/sub/InRelease` | `<uri>/<Filename>` — base is the **parent** |
| `deb <uri> a/b/` | `<uri>/a/b/InRelease` | `<uri>/<Filename>` — base two levels up |

- The request path is sent verbatim: no dot-segment normalisation. Filenames
  are encoded with `QuoteString(part, "+~ ")` (also `%`, controls, non-ASCII),
  so `pkg_1%3a2.deb` is requested as `pkg_1%253a2.deb`.
- apt requests `InRelease`, falls back to `Release` + `Release.gpg` **only on
  404** for signed sources; with `[trusted=yes]` any non-transient failure
  (403, soft-404 HTML) falls back. `Release.gpg` is requested even with
  `[trusted=yes]`. With no Release at all, apt fetches `Packages*` directly
  and sends `If-Modified-Since` for them.
- An InRelease whose first line is not exactly
  `-----BEGIN PGP SIGNED MESSAGE-----` is rejected (no fallback).
- by-hash uses the **strongest declared hash**: `by-hash/SHA512/<hex>`
  whenever the Release declares SHA512, even alongside SHA256.
- apt ignores an InRelease whose `Date:` is older than the one it holds, and
  rejects one dated more than 10 s in the future ("not valid yet").
- HTTP inactivity timeout (`Acquire::http::Timeout`) is **30 s**; a header
  timeout is silently re-sent once, a second one fails the item. `apt-get
  update` then exits 0 with a warning. `Acquire::Retries` defaults to 3 only
  since apt 2.3.2 (Debian 11 / Ubuntu 20.04: 0).
- apt uses one connection/queue per host; in mirror mode every source shares
  the cache's host, so a held request delays other suites' requests.

### Real flat repositories (fetched 2026-09-26/27)

| Repo | Form | Filename shape | Notes |
|---|---|---|---|
| pkgs.k8s.io `core:/stable:/v1.34/deb` | `/ /` | `amd64/kubectl_…deb` | InRelease; no by-hash; CDN answers 403 for missing files |
| NVIDIA CUDA `…/ubuntu2204/x86_64` | `/ /` | `./cuda_…deb` (11,533 stanzas) | `dpkg-scanpackages` shape |
| nvidia `libnvidia-container/stable/deb/amd64` | `/` | `./libnvidia-…deb` | **InRelease only**, whose Release text has only a SHA512 block; Packages has SHA256 |
| pkg.jenkins.io `debian-stable binary/` | `sub/` | `binary/…` and `direct/…` | no InRelease (detached) |
| download.sublimetext.com `apt/stable/` | `a/b/` | `files/…` → `/files/…` | base is the host root |
| download.jitsi.org `stable/` | `sub/` | `stable/…` | `Acquire-By-Hash: yes`, SHA256+SHA512, binary `Release.gpg` |
| apt.fury.io, repo.goreleaser.com | `[trusted=yes] … /` | `…` | **no Release at all**; Packages only |

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
  panics on a zero-plaintext frame with no `recover` in the adoption
  goroutine. I-7: a backward clock step freezes cooldown and scheduler gates.
- Adoption capacity: a global cap of 2 with no deadline, no per-host
  fairness, and goroutines queued on the slot while holding suite mutexes.
- A current-form anchor disappearing (vendor drops InRelease, or
  Release/Release.gpg) freezes the suite while apt itself falls back.
- by-hash `SHA512` requests 404 on adopted dual-hash `dists/` repos
  (e.g. packages.microsoft.com).

## 4. Decisions (maintainer, 2026-09-26/27)

| # | Decision |
|---|---|
| D1 | Reimplement from scratch on `feature/flat-repos`; credit the PR author; nothing posted to GitHub. |
| D2 | Vouch flat `.deb`s by exact path + Filename-suffix fallback (C6). |
| D3 | Eager first adoption for **all** suites (C4), limited to suites with recent client interest (review finding). |
| D4 | Limitation #2: flip barrier + retained by-hash + monotonic `Last-Modified`; no synchronous revalidation (C8). |
| D5 | Also in scope: SHA512 Release files, unsigned Release-only repos, canonical `dists/` suite paths. |
| D6 | Unsigned adoption on by default, safeguarded (C5), knob `adoption.adopt_unsigned`. |
| D7 | `/repo` and `/repo/.` remain two suites. |
| D8 | Release-less ("anchorless") flat repos refresh by revalidating `Packages*` (C4). |
| D9 | `POST /forget` admin escape hatch (C9). |
| D10 | Strict mode waits (bounded) for in-flight adoptions on the host before refusing (C10). |

## 5. Design

### C1. Request canonicalisation and upstream URLs

1. `proxy.Parse` rejects (`ErrInvalidPath` → 400) a decoded path containing
   NUL or an ASCII control byte (0x00–0x1F, 0x7F).
2. `proxy.BuildUpstreamURL(scheme, authority, path)` builds every upstream URL
   from a decoded canonical path by setting `url.URL.RawPath` to **apt's own
   encoding** of the path: each segment is encoded like apt's
   `QuoteString(part, "+~ ")` (percent-encode `+`, `~`, space, `%`, bytes
   ≤ 0x20 and ≥ 0x7F), and additionally `#` and `?`. The upstream therefore
   receives byte-for-byte what apt would send it directly. This closes S-3's
   key/URL desync, the double-decoding of `%` (`pkg_1%3a2.deb`), and the
   `+`-as-space problem on S3-style origins.
3. Call sites converted (the only composers of upstream URLs): the handler
   miss path (`proxy.Parse`), `buildMemberURL`, the `urlReconstruct` seam's
   production default (`reconstructAnchor`), `CommitAdoption`'s anchor seed,
   hot prefetch (`hot_set.go`), and the new C4 probes.
4. Tests: an apt-encoding table (`+ ~ space % # ? : @ ./` UTF-8); every path in
   a fixture extracted from the production `url_path` snapshot decodes, after
   `url.Parse` in the fetcher, to the same canonical path; plain paths without
   those characters are byte-identical to the old concatenation.

**Out of scope (pre-existing):** member URLs use the port-less canonical host
(`AIDEV-TODO(port-correctness)`). New probe code derives sibling URLs from the
anchor's stored `upstream_url` where one exists.

### C2. Suite identity (`proxy.SuitePath`)

**Canonicality predicate** (both grammars): every segment of the path is
non-empty and not `.` or `..` (one flat exception below), and the decoded path
contains none of `#`, `?`, `%`, `\`.

**`dists/` grammar** (tightened, D5): `^/(repo/)?dists/<codename>/<member>`
with a non-empty `<member>`; the suite is `/<repo>/dists/<codename>` as
today. Paths failing the predicate, or ending at the codename, get no suite
and keep Phase 1 serving. Multi-segment codenames (`dists/buster/updates`,
legacy Debian security) remain unsupported (documented).

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

Not covered, documented as served through Phase 1 only: flat Translation files
(apt's flat key is `$(LANGUAGE)`, files named `en`, `en.xz`),
`Contents-*.diff/Index`, `<index>.diff/by-hash/…`, pdiff patch files (unique
names, immutable), DEP-11 and command-not-found (never requested for flat
sources).

`proxy.IsFlatSuitePath(suite)` reports the grammar; `proxy.FlatBase(suite)`
returns `D` with one trailing `/.` removed (the `.deb` base candidate, C6).

### C3. Suite rows, client interest, and the signed-seen marker

**Single creation choke point.** `cache.CreateSuite` is the **only**
`INSERT INTO suite_freshness`: create-only (`ON CONFLICT DO NOTHING`), sets
`last_requested_at = now`, leaves `last_check_at` NULL, and enforces two caps:
`freshness.max_suites` (default 5000) globally and `max_suites / 10` per
canonical host. At a cap it creates nothing and emits a rate-limited WARN
`suite_seed_rejected` ("suite will not refresh"), a counter and a gauge.

Only the handler calls it, after serving (miss **or** `url_path` hit, so
legacy anchors cached before this release are onboarded) a response whose
bytes pass the sniff for the path:

| Path | Sniff |
|---|---|
| `<suite>/InRelease` | first line is exactly `-----BEGIN PGP SIGNED MESSAGE-----` (apt's rule; trailing `\r`/space/tab stripped) |
| `<suite>/Release` | a `SHA256:`, `SHA512:`, `SHA1:` or `MD5Sum:` header line in the first 64 KiB |
| flat `<D>/Packages[.ext]` (anchorless onboarding) | compression magic for the extension, or plain text starting `Package:` |

Everything else writes with UPDATE-only statements: `PutSuiteFreshness`
becomes `UpdateSuiteFreshness` (0 rows ⇒ no-op); `CommitAdoption` Step 6 is
UPDATE-only and rolls back with `ErrSuiteGone` if the row vanished;
`RecordAdoptionFailure`, `ClearAdoptionFailure` and `TouchSuite` are
UPDATE-only. `Check` and `Reconcile` require an **existing row** before
`LoadOrStore` (I-4), and re-validate after `TryLock` that the lock map still
holds the same mutex and the row still exists. The handler's by-hash
content-addressed fallback requires a known suite (I-4, handler side). A test
proves exactly one INSERT site.

**Client interest.** `suite_freshness.last_requested_at`, written only by
`TouchSuite` from client-serving paths:

- anchor serves (`InRelease`, `Release`, `Release.gpg`; status < 400,
  including 304, HEAD and stale serves);
- for flat suites, root `Packages*`/`Sources*` serves (anchorless clients
  never request an anchor);
- vouched `.deb` serves (the snapshot id from `DeclaredHashesForPath` maps to
  its suite), so install-only clients keep their suite alive.

Touches are coalesced in memory (per-suite last-enqueued time; at most one
write per suite per hour; the map is bounded by tracked suites and purged on
reap).

**Signed-seen marker.** Table `suite_signed_seen(scheme, host, base,
last_seen_at)` where `base` is the suite path with one trailing `/.` removed
(so twins share it). Written (coalesced daily) when the handler serves a
sniff-passing `InRelease` or signature-sniff-passing `Release.gpg`, and when a
signed snapshot is adopted. It survives suite reaping; it is deleted by
`POST /forget` and after 365 days without renewal. It is the never-downgrade
authority for unsigned adoption (C5).

### C4. Freshness check

Every check runs under the per-suite mutex with a whole-check deadline (2 min)
and short-circuits the probe chain on transport errors (bounds I-5 stalls in
the sequential scheduler).

**Response classification** (defined once, used everywhere):

| Class | Responses |
|---|---|
| *present* | 200 whose body passes the file's sniff (C3; `Release.gpg`: armor header `-----BEGIN PGP SIGNATURE-----`, or a binary OpenPGP signature packet — old-format tag bytes 0x88–0x8B, new-format 0xC2) |
| *absent* | 401, 403, 404, 410, or 200 failing the sniff |
| *unknown* | transport error, 408, 429, 5xx, deadline — no form decision; the check fails (bump `last_check_at`) |

**Dispatch** (adopter wired; with no adopter the Phase 1 algorithm runs
unchanged, observation only):

1. **Current snapshot inline/detached** — conditional GET of the current
   anchor as today. *Absent* → run the probe tree (form transition; inline ⇄
   detached is lateral, never to unsigned from signed). Changed → C7 gate →
   adoption request.
2. **Current snapshot unsigned** — probe `InRelease` and `Release.gpg`; if
   either is *present* → signed adoption request (upgrade). Otherwise
   conditional GET `Release`; changed → unsigned adoption request. If a
   signature is present but the signed adoption cannot succeed (key not on the
   cache host), the suite keeps its last unsigned snapshot and the admin page
   shows the reason — the secure outcome, documented; the fix is to install
   the key (or `POST /forget` plus `adopt_unsigned`-aware handling).
3. **No current snapshot (eager first adoption)** — only if the suite has
   recent client interest (`last_requested_at` within `gc.url_path_ttl`) and a
   client-created anchor or flat index `url_path` row (no reconstruction for
   never-adopted suites). Probe tree without validators:
   - `InRelease` *present* → inline request;
   - `InRelease` *absent* → `Release`: *present* → `Release.gpg`: *present* →
     detached request; *absent* → unsigned candidate (C5);
   - `Release` *absent* → for flat suites the **anchorless refresh** below;
     for `dists/` suites a failed check;
   - any *unknown* → failed check.
4. **Anchorless refresh** (flat suite, `InRelease` and `Release` *absent*):
   conditional GET (stored `upstream_etag`/`upstream_lastmod`) of every cached
   `url_path` metadata row whose `SuitePath` equals the suite; a 200 with new
   content replaces the row's blob exactly as the miss path would. No
   snapshot, no vouching, no strict-mode effect. If a `Release`/`InRelease`
   later becomes *present*, the suite moves to the normal forms. Metadata
   served from `url_path` uses `max(pool mtime, last_fetched_at)` as
   `Last-Modified`, so apt's `If-Modified-Since` on `Packages*` sees the
   replacement.

**Adoption capacity.** Before spawning an adoption goroutine the check
`TryAcquire`s the global adoption slot (`max_concurrent_adoptions`) and a
per-canonical-host slot (one adoption per host at a time). If either is busy
the check records `result = changed_queued` (or `first_adoption_queued`),
spawns nothing and releases the mutex; the next check retries with bytes at
most one cooldown old. Every adoption runs under a context deadline
`adoption.max_duration` (default `30m`); a timeout is a failure (C7).

**Validators.** `suite_freshness` validators always describe the current
snapshot's anchor (set in the flip transaction), or the `url_path` anchor when
unadopted. They are never written from a changed or deferred response (C7 has
its own).

**`inrelease_change_seen_at`** is set only when NULL (first sight of a pending
change), cleared in the flip transaction and on `not_modified`/`unchanged`.

**Clock-step rule** (fixes I-7 in all touched gates). A stored stamp in the
future is treated as infinitely old by gates that *delay* work (cooldown, T2
threshold, `next_retry_at`) and as "now" by gates that *retire* things (reap
cutoff, touch coalescing). Each detection emits a rate-limited WARN
`clock_step_detected`. In-memory timing (flip barrier, holds) uses monotonic
time only.

**Seeding and prompt first adoption.** After `CreateSuite` succeeds and an
adopter is wired, the handler fires T1 once the response is written, so
eager adoption starts within seconds of first contact (subject to slots).

New `freshness_check` result values: `first_adoption`,
`first_adoption_queued`, `changed_deferred`, `changed_queued`,
`form_changed`, `anchorless_refreshed`, `anchorless_unchanged`,
`upstream_older`.

### C5. Adoption input

**Panic recovery (I-1).** The adoption, repair/reconcile and on-demand
reconcile goroutines `recover()` a panic into `ErrAdoptionPanicked`. The
recovery runs before the mutex, WaitGroup and flip-barrier releases (defer
order), records a failure with reason `panic` for the content key, and logs
the stack once. `panic` records are excluded from the restart reset (C7).
The inline anchor must also pass apt's first-line rule before verification.

**Flat IndexTargets.** Root-level `Packages*`/`Sources*` (and their
`.diff/Index`) are IndexTargets — fatal on fetch failure — **only when the
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

**Hash blocks.** `ParseRelease` merges all hash blocks per member path (as apt
does): `ReleaseMember` carries `SHA256` and `SHA512` (either may be empty; at
least one required; sizes must agree). Verification prefers SHA256. A member
declared only in SHA512 is verified by size + SHA512, and `runShared`
normalises its `SHA256` to the verified bytes' computed hash at one point, so
all downstream code is unchanged (a *derived* anchor; controlling it needs a
SHA512 preimage). by-hash alias rows are written for **every** declared
algorithm (SHA256 and SHA512), fixing SHA512 by-hash 404s on dual-hash
`dists/` repos after their next adoption. A Packages stanza with a Filename
but no SHA256 makes coverage incomplete (reason `no_sha256`), never a silent
drop. Limitation (documented): integrity-skipped SHA512-only members are not
recorded for skip-repair; reconcile still heals requestable IndexTargets.

**`Date` rules** (apt's own). The verified Release text's `Date:` is parsed
and stored as `suite_snapshot.release_date`. A candidate whose `Date` is
strictly older than the current snapshot's is refused (`upstream_older`, a
refusal state, C7). A `Date` more than 10 s in the future is a failure
(`release_date_in_future`). A missing `Date` proceeds. Equal-`Date` content
that collides with a previously adopted, non-current snapshot is refused
(`rollback_refused`). The newest adopted content therefore stays current and
a flapping mirror cannot flip the cache backwards.

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

`suite_snapshot.anchor_form` (backfilled). The existing CHECK permits only the
inline shape (`inrelease_hash`) or the detached shape (`release_hash` +
`release_gpg_hash`) and cannot be altered without rebuilding a table four
others reference, so an unsigned snapshot stores its `Release` hash in
`inrelease_hash`. To make every reader explicit, the Go field is renamed
`SuiteSnapshot.SingleAnchorHash` and a `Form` field is added; the compiler
then lists every reader. A single helper `AnchorPaths(form)` / SQL expression
gives the anchor file(s) per form. Readers that change: `detectForm`,
`priorAdoptionForm`, both `cachedHash` derivations, `snapshotAnchorHash`,
`reconstructAnchor`, `CommitAdoption` Step 3c, url_path GC identity guard (d),
`reparseSnapshotRelease`, `HostCurrentSnapshotsCoverage`,
`DeclaredHashesForPath`, admin status. `InsertCandidateSnapshot` sets
`anchor_form` (non-empty, validated) and its collision lookup includes it; the
natural key becomes `(scheme, host, suite_path, COALESCE(inrelease_hash,
release_hash), anchor_form)`.

**Unsigned safeguards (D6).**

1. `adoption.adopt_unsigned` (default `true`).
2. Eligible only when `InRelease` and `Release.gpg` are both *absent* and
   `Release` is *present*, confirmed on two checks at least
   `max(cooldown, 5m)` apart with the same `Release` bytes (the first
   observation records reason `unsigned_unconfirmed`, C7).
3. **Never downgrade:** refused (`unsigned_downgrade_refused`, a refusal
   state) when a `suite_signed_seen` marker exists for the suite's base. The
   marker outlives reaping, covers the `./` twin, and is set before first
   adoption by any signed anchor a client was served. `CommitAdoption` also
   refuses an unsigned candidate when the prior current snapshot is signed.
4. At most 1,000 declared members (`unsigned_too_large`).
5. No `package_hash` rows on **any** path (adoption, repair, reconcile:
   `buildPackageHashes` takes the form); `DeclaredHashesForPath` and
   `HostCurrentSnapshotsCoverage` filter `anchor_form <> 'unsigned'`.

Threat note (documented at the knob): a MITM on the cache→upstream path can
make the cache adopt forged unsigned content only for a suite never seen
signed, after two consistent observations. Verifying clients reject it;
`[trusted=yes]` clients accept it, as they would fetching directly. Operators
serving only signature-verifying clients can set `adopt_unsigned = false`.

### C6. Package coverage for flat suites (D2)

**Materialisation.** For a signed flat suite, `C = proxy.FlatBase(suite)`.
Each stanza with literal Filename `F` yields one `package_hash` row at path
`C + "/" + F` (literal) with `flat_filename = dotClean(F)` (`.` segments
removed; `dists/` rows keep `''`). Coverage uses the `dists/` definition with
the root as the Packages directory (`buildPackageHashes`' `/dists/` gate is
replaced by a flat-aware base). Sources and pdiff hashing are not built for
flat suites (documented). The exact row is a real path for `/ /` and `./`
sources and a harmless phantom for `sub/` and `a/b/` sources.

**Lookup (`DeclaredHashesForPath`)**, current signed snapshots only:

1. Exact: rows where `path = P` (unchanged).
2. Only if (1) is empty — suffix fallback. `P' = dotClean(P)`; stop if `P'`
   contains `..` or an empty segment or has more than 32 segments. For each
   split `P' = B + "/" + F` (`B` may be `""`, the host root), query rows with
   `flat_filename <> '' AND flat_filename IN (F…)` on `(scheme, host)` (the
   `<> ''` makes SQLite use the partial index; an `EXPLAIN QUERY PLAN`
   assertion pins it), join `suite_snapshot`, and keep a row iff `B` is an
   ancestor-or-self of that suite's `FlatBase`
   (`strings.HasPrefix(base+"/", B+"/")`). Among kept rows, keep those with
   the longest `B` (the most specific base).
3. The existing dispatch applies: 0 distinct hashes → strict-mode predicate;
   1 → validate; ≥ 2 → `package_hash_conflict` 502.

This is apt's rule (`.deb` URL = `<uri>/<Filename>`, `<uri>` an
ancestor-or-self of the Release directory) with one row per stanza. It covers
k8s and CUDA (exact), CUDA's dot-cleaned form, Jenkins `binary/` and
`direct/` (base = parent) and Sublime `files/` (base = host root). Rows are
constraints, not grants: a match only lets bytes through if they hash to a
signed declaration.

**Consumers.** Hit validation, miss validation and strict mode go through
`DeclaredHashesForPath`. Hot prefetch and the retention mirror rule join on
the exact path, so they cover flat repos whose `.deb`s sit under the suite
directory (k8s, CUDA, OBS); parent-base repos (Jenkins, Sublime) are retained
by recency and hold-grace like unvouched files (documented).

### C7. Retry discipline (limitation #1)

**Failure record** (v7 columns on `suite_freshness`): `adoption_fail_hash`,
`adoption_fail_reason`, `adoption_fail_count`, `adoption_fail_first_at`,
`adoption_fail_last_at`, `adoption_next_retry_at`, `adoption_fail_etag`,
`adoption_fail_lastmod`. UPDATE-only statements; written **before** the suite
mutex is released; a shutdown context cancellation is not recorded. `count`
and `first_at` describe the failure streak since the last success.

**Content key.** inline: sha256(InRelease); detached: sha256(sha256(Release)
‖ sha256(Release.gpg)); unsigned: sha256(Release). Any byte change is new
content and is attempted at once (subject to slots).

**One schedule.** `next_retry_at = now + min(2m · 2^(count−1), 1h)` with
±25 % jitter (seeded seam), clamped on read to `now + 1h`. The `%w`
prerequisites (natural-key collision, commit failure, `readPackagesBlob` I/O)
and distinct sentinels make reasons precise for operators; the schedule does
not depend on classification.

**Refusal states** use the same record and schedule but are not failures:
`upstream_older`, `rollback_refused`, `unsigned_downgrade_refused`,
`unsigned_unconfirmed` (fixed `max(cooldown, 5m)` delay), `unsigned_too_large`.
They are excluded from the failing-suites gauge and shown as "refused: …" in
the admin page.

**Gate** (in every branch that would spawn an adoption). While
`now < next_retry_at` the check sends the *failing content's* validators: a
304 or a 200 with the same key ⇒ `changed_deferred` (no clear, no repair);
new content ⇒ attempt. At or after `next_retry_at` the check GETs without
validators and attempts. The record is cleared in the flip transaction and
when upstream again serves the current snapshot's content.

**Restart.** At checker start every failing row (except reason `panic`) gets
`next_retry_at = now + rand(0, periodic_refresh)`: one retry per failing
suite, the recovery path after an operator installs a key or changes pins.

**Visibility.** Admin `suites[]` gains `anchor_form`, `flat`,
`last_requested_unixtime`, and the record ("adoption failing 3d 2h ·
untrusted_signer · retry in 41m", or "refused: upstream_older"). "Lagging" is
computed from the first-seen `inrelease_change_seen_at` against `now`. Gauge
`acu_adoption_failing_suites`; INFO `adoption_deferred` on state transitions;
WARN `adoption_failing_persistently` at streak counts 3, 6, 12, …; per-attempt
WARN `adoption_run_failed` remains. Reason strings stay on the `html/template`
path.

### C8. Flip consistency (limitation #2)

New package `internal/flipgate` (channels, timers, monotonic time; never waits
while holding its mutex), keyed by the freshness suite key.

**Handler side.** Before serving an anchor (`<suite>/InRelease` or
`<suite>/Release`, for suites with a row) the handler calls
`t := gate.AnchorStart(ctx, key)`, which returns immediately unless a flip is
pending; then it waits at most **20 s** (a constant below apt's 30 s timeout),
honouring the request context, handler shutdown and a per-suite waiter cap
(256; beyond it, no wait). It runs **before** host-slot acquisition and
singleflight. After the (re-read) response is written, `t.Done()` records the
completion time and decrements the suite's in-flight count.

**Adopter side.** `gate.Flip(ctx, key, commit)` runs the commit in two phases:

- **Phase A — natural quiet, no client holds.** Wait until the suite has no
  anchor in flight and none completed within `flip_quiet_period` (default
  `10s`, at most `15s`), and no hold anywhere ended within the same period
  (holds delay other suites on the client's shared queue). Bounded by 60 s;
  on timeout proceed to phase B.
- **Phase B — hold.** Mark the flip pending (new anchor requests wait), wait
  for in-flight anchors to finish, run `commit`, clear pending and wake
  waiters onto the new snapshot. Phase B auto-expires 20 s after it starts:
  if the commit is still running (F-2 on very large suites), pending is
  cleared and waiters are answered from the still-current snapshot
  (documented residual; counter `acu_flip_hold_expired_total`).

Skipped entirely when `flip_quiet_period = 0s` (today's behaviour) and for a
first adoption whose anchor bytes equal the `url_path` anchor clients have
been served (the flip does not change what they hold). Phase A runs while the
adoption holds its slot (bounded, documented). Guarantee: a client that
received the old anchor either finished receiving it at least
`flip_quiet_period` before the flip, or was held and answered from the new
snapshot; its index requests within that period come from the snapshot
matching its anchor. Map entries exist only for suites with rows, expire when
idle for twice the quiet period, and are purged on reap.

**Retained by-hash.** When `trySnapshotHit` misses the current snapshot for a
`by-hash/SHA256|SHA512/<hex>` member, it looks the alias up in the suite's
other adopted snapshots (newest first; at most `gc.keep_displaced + 1`) and
serves it with that snapshot's `X-Cache-Snapshot`. SHA256 aliases must match
the served blob hash; SHA512 aliases rely on the verified `snapshot_member`
binding. No recovery fetch for displaced snapshots. Counter
`acu_serve_byhash_displaced_total{host}`. `gc.keep_displaced = 0` disables it
(documented).

**Monotonic `Last-Modified`.** `CommitAdoption` stamps `adopted_at =
max(now, prior.adopted_at + 1)`, and snapshot-served anchors (`InRelease`,
`Release`, `Release.gpg`) use `max(pool mtime, adopted_at)` as the
`ServeContent` modtime, so a client's `If-Modified-Since` from the previous
anchor always gets a 200 after a flip, even across backward clock steps.

The README's "clients always see a coherent metadata set" claim is replaced
with this guarantee and its residual.

### C9. Suite lifecycle (limitation #3)

**Reaper.** Runs from the freshness scheduler at most hourly (independent of
`gc.interval`, so daily-restarted caches still reap), after a monotonic
uptime floor of 1 h, at most 25 suites per run (constant), oldest first.
Candidates: `last_requested_at < now − gc.suite_idle_ttl` (default `720h`;
`0s` disables). For each: `TryLock` the suite mutex (busy → skip), then one
writer transaction `cache.ReapIdleSuite` that:

1. re-reads the row and bails if the stamp is newer than the cutoff or
   `current_snapshot_id` changed since the candidate was listed
   (compare-and-delete);
2. runs the Step-8 refcount decrement for the current snapshot (otherwise its
   member blobs are immortal);
3. deletes the suite's metadata `url_path` rows — rows where
   `proxy.SuitePath(path) == suite` (never a twin's or nested suite's), plain
   metadata first and anchors last, at most 1,000; if more remain, the row
   is kept and the next run continues;
4. deletes the `suite_freshness` row.

Then it purges in-memory state (lock-map `CompareAndDelete`, flipgate entry,
touch-coalescing entry). The signed-seen marker is kept. The gpg once-logger
maps reset when they exceed 10,000 entries (no cross-package interface). The
reconcile memo is swept against current snapshot ids each scheduler tick.
Snapshot GC gains sub-job C: adopted snapshots whose suite row no longer
exists are reaped regardless of `keep_displaced`, one snapshot per writer
transaction.

With eager first adoption a returning suite re-adopts within seconds of its
next anchor request (subject to slots); its by-hash history restarts.

**Migration backfill** of `last_requested_at`: the newest client
`last_requested_at` of the suite's anchor `url_path` rows, else migration
time minus 7 days. Dormant never-adopted suites are therefore neither
eagerly adopted after upgrade nor kept alive; they are reaped about 23 days
later.

**`POST /forget` (D9).** Admin endpoint with the same protection as
`POST /reconcile` (`host`, `suite` parameters). Under the suite mutex
(busy → 409) it runs the reap transaction without the idle checks and deletes
the signed-seen marker for the suite's base; unknown suite → 404; success →
202. It is the documented escape hatch for refusal states (vendor stopped
signing; deliberate upstream rollback; stuck unsigned suite).

### C10. Strict mode

The host-wide rule is kept: refuse an unvouched `.deb` iff every current
**signed** snapshot on the host has complete coverage (PR #8's directory
scoping is not reimplemented). Unsigned snapshots, anchorless suites and
seeded-but-unadopted suites never count and never vouch. Flat snapshots now
normally have complete coverage, so strict mode protects flat hosts.

**In-flight adoptions (D10).** The checker tracks in-flight adoptions per
canonical host. Before answering `unvouched_deb_refused`, the handler waits
up to 15 s for in-flight adoptions on the request's host to finish, then
re-runs the lookup once. The wait happens before host-slot acquisition and
singleflight (the adoption's own member fetches need host slots). This covers
the first `apt update && apt install` from a new repository.

Documented consequences: a queued (not yet running) first adoption and an
unsigned or anchorless repository on a host that also serves complete signed
suites both lead to refusals; strict posture changes as eager adoptions land
after upgrade.

### C11. Observability and configuration

**Events** (documented in `docs/log-fields.md`): `suite_seed_rejected`,
`suite_reaped`, `suite_forgotten`, `adoption_deferred`,
`adoption_failing_persistently`, `adoption_refused` (reason),
`adoption_flip_barrier`, `adoption_panicked`, `clock_step_detected`,
`flat_host_root_unsupported`; new fields `anchor_form`, `flat`,
`snapshot_displaced`. New metrics are host-labelled or unlabelled (no suite
labels) and are added to the metrics inventory comments in
`internal/freshness/metrics.go` and `internal/handler/metrics.go`.
`gc_run_complete`/`LastRunSummary` gain `suiteless_reaped`.

| Key | Default | Rule |
|---|---|---|
| `adoption.adopt_unsigned` | `true` | bool |
| `adoption.flip_quiet_period` | `"10s"` | `0s` disables the barrier; ≤ `15s` |
| `adoption.max_duration` | `"30m"` | ≥ `1m` |
| `freshness.max_suites` | `5000` | ≥ 10 (per-host cap = `max_suites / 10`) |
| `gc.suite_idle_ttl` | `"720h"` | `0s` disables reaping; else ≥ `24h` |

Constants, not knobs: backoff 2 m → 1 h; hold 20 s; phase A cap 60 s; waiter
cap 256; strict wait 15 s; reaps 25/run; unsigned member cap 1,000; check
deadline 2 min; marker TTL 365 d.

## 6. Migration v6 → v7

A new `migrations[6]` entry (no existing migration is edited);
`CurrentSchemaVersion = 7`. Probe-verified to run on a populated v6 database
in one transaction (SQLite 3.53, modernc).

```sql
ALTER TABLE suite_freshness ADD COLUMN last_requested_at      INTEGER;
ALTER TABLE suite_freshness ADD COLUMN adoption_fail_hash     TEXT;
ALTER TABLE suite_freshness ADD COLUMN adoption_fail_reason   TEXT;
ALTER TABLE suite_freshness ADD COLUMN adoption_fail_count    INTEGER NOT NULL DEFAULT 0;
ALTER TABLE suite_freshness ADD COLUMN adoption_fail_first_at INTEGER;
ALTER TABLE suite_freshness ADD COLUMN adoption_fail_last_at  INTEGER;
ALTER TABLE suite_freshness ADD COLUMN adoption_next_retry_at INTEGER;
ALTER TABLE suite_freshness ADD COLUMN adoption_fail_etag     TEXT;
ALTER TABLE suite_freshness ADD COLUMN adoption_fail_lastmod  TEXT;

UPDATE suite_freshness SET last_requested_at = COALESCE(
  (SELECT MAX(u.last_requested_at) FROM url_path u
    WHERE u.canonical_scheme = suite_freshness.canonical_scheme
      AND u.canonical_host   = suite_freshness.canonical_host
      AND u.path IN (suite_freshness.suite_path || '/InRelease',
                     suite_freshness.suite_path || '/Release')),
  CAST(strftime('%s','now') AS INTEGER) - 604800);

ALTER TABLE suite_snapshot ADD COLUMN anchor_form TEXT NOT NULL DEFAULT ''
  CHECK (anchor_form IN ('', 'inline', 'detached', 'unsigned'));
ALTER TABLE suite_snapshot ADD COLUMN release_date INTEGER;
UPDATE suite_snapshot
   SET anchor_form = CASE WHEN inrelease_hash IS NOT NULL THEN 'inline' ELSE 'detached' END;

DROP INDEX idx_suite_snapshot_natural;
CREATE UNIQUE INDEX idx_suite_snapshot_natural_v7
  ON suite_snapshot(canonical_scheme, canonical_host, suite_path,
                    COALESCE(inrelease_hash, release_hash), anchor_form);

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
INSERT INTO suite_signed_seen (canonical_scheme, canonical_host, base, last_seen_at)
SELECT DISTINCT ss.canonical_scheme, ss.canonical_host, ss.suite_path,
       CAST(strftime('%s','now') AS INTEGER)
  FROM suite_snapshot ss WHERE ss.adopted_at IS NOT NULL;
```

Notes: existing snapshots keep `release_date` NULL (the `Date` rule applies
from their successors on). The partial index scans `package_hash` once
(seconds on ~1 M rows; covered by the existing `schema_migrating` log line)
and stays empty until flat suites adopt. `last_seen_at`'s `strftime` backfill
uses SQLite's clock, so migration tests allow a tolerance. A v7 database is
refused by older binaries; rollback means restoring a backup or wiping the
cache (release notes).

## 7. Testing strategy

**Harness.** Virtual-clock simulations use Go's `testing/synctest` with the
**real** cache (verified: 30 virtual days in 0.02 s with modernc SQLite and
the writer goroutine) and in-memory `Fetcher`/`AdoptionFetcher` fakes. New
code blocks only on channels/timers (never a `sync.Mutex` across virtual
sleeps). Jitter uses fixed seeds. Every long-run simulation asserts **lower
bounds** (liveness: checks resume, changes adopt, touched suites survive) as
well as upper bounds (attempts, rows, goroutines, upstream requests). Where
cadence matters the simulations run the production scheduler and GC loops at
production intervals (`gc.interval = 24h`), with restarts.

**Per-component tests** (each phase's DoD names its own; highlights):

- C1: apt-encoding table; production-fixture round-trip; S-3 minted-suite
  probe fails on main.
- C2: grammar tables with decoded inputs; `dists` segment never flat;
  Parse-level test that `%2E` and `.` land on the same suite.
- C3: single-INSERT-site test; minted anchors/by-hash names at scale create
  no rows beyond caps and no lock-map growth; legacy anchor onboarding on a
  `url_path` hit; touch coalescing; signed-seen written before first
  adoption.
- C4: classification table (401/403/404/410/200-sniff-fail/408/429/5xx/timeout
  per anchor); form transitions both ways; anchorless refresh with IMS;
  capacity: 2 slow-drip suites + 20 normal suites → every normal suite adopts
  within `max_duration + cooldown`, goroutines ≤ cap + constant; clock steps
  ±2 h/±30 d with liveness asserts; `TestCheck_NoAdopter_ChangeOnlyLogged`
  and `disabled_mode_parity_test.go` pass (setup seeds rows via `CreateSuite`).
- C5: zero-plaintext InRelease fixture (I-1) crashes main and is recorded as a
  failure here; R-1 armor-injection fixture; `./` and `%` Filenames; SHA512
  merge and aliases; `Date` older/future/equal; unsigned confirmation,
  never-downgrade at both points (including after a reap), member cap, no
  `package_hash` from reconcile.
- C6: every real Filename shape vouches; nested-base tiebreak; `..`/`//`/
  sibling `/flat2` negatives; `EXPLAIN QUERY PLAN` uses the partial index.
- C7: lim1 reproduction (≤ ~30 attempts/day vs ~1,440 on main); 30-day
  simulation (untrusted body, new content, rollback, clock step); flapping
  mirror → zero backward flips; restart spread; panic excluded from reset;
  deferred-then-retry liveness with 304s to the failing validators.
- C8: plain-path straddle reproduced on the phase-B tip and fixed; hold never
  exceeds 20 s; phase-B expiry; cross-suite hold; idle zero-wait; client
  cancel; shutdown; commit failure releases waiters; the chaos test's straddle
  count asserted zero; retained by-hash (SHA256/SHA512) with stray-blob and
  reaped-snapshot negatives; `Last-Modified` across same-second flips and a
  backward step.
- C9: 90-day simulation at `gc.interval = 24h` with 12-hourly restarts
  (touched / idle / near-TTL / busy / clock jump); reap-vs-adoption chaos;
  blobs reclaimed within two GC ticks; twin and nested suites untouched;
  `/forget` round trip.
- C10: strict matrix per `sources.list` form × Filename shape; mixed host;
  in-flight wait turns a first-install refusal into a success; unsigned and
  anchorless suites never change posture.

**Real-apt end-to-end** (`e2e/flat/`, `make e2e-flat`), built on the
single-container `e2e/deb` pattern (nginx + daemon + clients in one image,
in-container restart, upstream `current -> v1|v2` symlink flips), run per
client image: `debian:11` (apt 2.2, 0 retries), `ubuntu:22.04`,
`ubuntu:24.04`, `debian:12`, `debian:13`, `ubuntu:26.04`, with the images'
`docker-*` apt.conf quirks removed. Fixtures reproduce k8s (`amd64/…`, 403 for
missing), CUDA (`./…` plus an epoch `%3a` package), libnvidia-container
(InRelease only, SHA512-only), Jenkins (`sub/`, detached, `direct/`), Sublime
(`files/`, host-root base), Jitsi (by-hash SHA256+SHA512, binary
`Release.gpg`, `Contents-amd64` via apt-file), unsigned (404 and 403
variants), Release-less (Gemfury shape), plus a `dists/` repo. Signed repos
live on one host name, unsigned and Release-less repos on a second, so strict
mode's expectations are explicit. Assertions: installs succeed; the exact
request lines apt sends are pinned (`./`, `%253a`, `by-hash/SHA512`);
`apt-get` output is grepped for `Failed to fetch|Connection failed|timed
out|Hash Sum mismatch|Mirror sync in progress` (exit status is useless); zero
`unvouched_deb_refused` on the signed host; a v2 publish reaches clients within
one cooldown for every shape; an older-`Date` publish is ignored; a
commit-delay hook of 45 s never produces client timeouts; state survives a
restart. `go test -race ./...`, `golangci-lint run ./...` and `gofmt` are clean
at every phase.

## 8. Documentation

- `README.md`: supported repository layouts (flat forms, `/` vs `./` twins,
  host-root limitation, unsigned/Release-less/SHA512 support), the
  adoption-enabled requirement, corrected coherence claim, GC and `/forget`.
- `docs/configuration.md`: new keys (C11); unsigned trust and never-downgrade;
  strict-mode behaviour for flat, unsigned, anchorless and first-use; eager
  first adoption; suite reaping; the apt timeout constraint on the barrier.
- `docs/log-fields.md`, `docs/admin-ui-spec.md`,
  `packaging/config/config.toml.default`.
- `AIDEV-NOTE`s at every changed invariant; `Contributors.md` credit.

## 9. Implementation phases

Each phase leaves shippable state: `go test -race ./...`, lint and `gofmt`
clean; its own config keys, docs entries and `AIDEV-NOTE`s; a max-effort
review of the phase diff; fixes; a commit. The phase's DoD lists named tests.

| Phase | Items | Definition of Done (highlights) |
|---|---|---|
| **A. Foundations** (no behaviour change for ordinary traffic) | v7 migration (final schema) and Go types incl. the `SingleAnchorHash`/`Form`/`ReleaseDate` rename; C1 complete; C2 `dists/` tightening (no flat grammar yet); `%w` wraps and sentinels; R-1; I-1 recover (logging); clock-step rule in existing gates; synctest harness + smoke simulation. | v6-fixture migration test; C1 tables and production-fixture round trip; existing suite green except enumerated `SuitePath` tightening cases; R-1 and I-1 fixtures fail on main, pass here; 30-day smoke simulation < 5 s under `-race`. |
| **B. Retry discipline** | C7 record, gate, schedule, refusal states, restart; `Date` rules; validators at commit; `change_seen` semantics; adoption capacity (TryAcquire, per-host slot, `max_duration`); panic recorded; admin/gauge/logs. | lim1 reproduction bound; 30-day simulation incl. flapping mirror and clock step (liveness + bounds); slow-drip capacity simulation; "lagging" visible. |
| **C. Flip consistency** | C8 flipgate, handler anchor hook, retained by-hash (SHA256), monotonic `Last-Modified`. | Straddle test fails on the B tip, passes here; hold ≤ 20 s; chaos straddles = 0; `flip_quiet_period = 0s` keeps existing tests unchanged. |
| **D. Creation bounds and signals** | C3 in full: `CreateSuite` with sniffs and caps, legacy onboarding, UPDATE-only writers, I-4 (both sides), touch (anchors, flat indexes, vouched `.deb`s), signed-seen marker. | Single-INSERT-site test; minted-suite scale test; onboarding test on a v6 fixture with legacy anchors. |
| **E. Eager adoption and anchor forms** | C4 probe tree, classification, form transitions, eager first adoption with interest window, T1-on-seed; C5 unsigned form with safeguards; SHA512 merge and aliases; C10 in-flight wait. | Production-shaped fixture: active never-adopted suites adopt once, dormant ones make zero requests; classification table; never-downgrade incl. after reap; unsigned never changes posture; nvidia-shaped fixture adopts; SHA512 by-hash served. |
| **F. Suite reaping** | C9 reaper, sub-job C, memory purge, `POST /forget`, marker TTL. | 90-day simulation at `gc.interval = 24h` with restarts; reap-vs-adoption chaos; blob reclaim; twins untouched. |
| **G. Flat repositories** | C2 flat grammar; flat IndexTargets (layout-aware, adoption and handler sides); `validateFilename`; C6 materialisation, fallback, coverage; anchorless refresh; flat pdiff arch labels. | Every real Filename shape adopts and vouches; flat `Packages` failure fatal; strict matrix; anchorless IMS refresh. |
| **H. End to end and docs** | `e2e/flat/`; documentation; config file; final full-diff review. | `make e2e-flat` green on all images with the grep assertions; docs complete. |

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
   a replaying MITM choose the first snapshot's content (bounded by the new
   `Date` rules only after the first adoption).
6. Flat Sources/pdiff hashing; flat Translation files; skip-repair records for
   SHA512-only members; backfilling SHA512 by-hash aliases for snapshots
   adopted before this release.
7. apt `--snapshot` IDs each become a `dists/` suite that eager adoption
   downloads in full (bounded by caps and reaping); a snapshot-server
   exemption.
8. Negative caching of metadata 404s (Release-less clients probe
   `.xz/.bz2/.lzma` and Translations on every update).
9. Downstream mirror tools (debmirror, aptly mirror) requesting
   `Release`+`Release.gpg` from inline suites get 404 (pre-existing).

## 11. Risks

| Risk | Mitigation |
|---|---|
| Eager first adoption after upgrade (burst). | Interest window + dormant backfill; TryAcquire slots, per-host slot, `max_duration`; failures back off. |
| Tightened `dists/` grammar drops suite identity for odd client paths. | Only shapes apt never sends; Phase 1 serving continues; enumerated tests. |
| Unsigned adoption abused by a MITM. | Two observations, signed-seen marker outliving reaps, never-downgrade in check and commit, member cap, no vouching, no strict-mode effect. |
| Barrier stalls or times out clients. | Holds only in phase B, ≤ 20 s (< apt's 30 s), auto-expiry, waiter cap, cross-suite quiet; `0s` disables; counters. |
| Suite reaping removes a live suite. | 30-day TTL, touches from anchors/indexes/vouched `.deb`s, compare-and-delete, hourly cap, uptime floor; a false reap self-heals on the next request. |
| New code paths leave a suite silently stuck (freeze-trap class). | Explicit states for every refusal (admin, logs), `POST /forget`, liveness assertions in every simulation. |
| Suffix fallback cost. | One partial-index query, only when no exact row vouches the path; plan pinned by test. |
| v7 migration time. | Partial index build logged; seconds for ~1 M rows. |
