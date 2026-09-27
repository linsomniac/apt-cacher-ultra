# Flat repositories, eager adoption, and suite lifecycle — design

- **Date:** 2026-09-27
- **Status:** draft for maintainer review
- **Branch:** `feature/flat-repos` (from `main` @ 536a1ab)
- **Origin:** reimplementation of the problem reported in PR #8 ("Refresh flat
  repositories: give them a suite identity") by Venkat (@venkatamutyala). The
  PR is used as a reference only; its analysis was reviewed skeptically and
  several of its claims were found false (see §3).

---

## 1. Summary

A *flat* apt repository has no `dists/` tree: `InRelease`/`Release`,
`Packages*` and often the `.deb`s sit in one directory
(`deb https://pkgs.k8s.io/core:/stable:/v1.34/deb/ /`). Today the cache never
gives such a directory a *suite*, so its metadata is served from the Phase 1
`url_path` cache forever and no freshness check or adoption ever runs.

This design makes flat repositories first-class, vouches their `.deb`s for
strict mode, and fixes four robustness problems that flat support multiplies:

| Area | Change |
|---|---|
| Suite identity (C1, C2) | One canonical grammar for `dists/` and flat suites; upstream URLs built with proper escaping. |
| Anchor forms (C3, C4) | Adopt inline (`InRelease`), detached (`Release`+`Release.gpg`) **and unsigned** (`Release` only) suites; adopt a suite **on first sight**, not only when upstream changes. |
| Adoption input (C5) | Flat IndexTargets; `./` Filenames; **SHA512-only** Release files; the R-1 reconcile re-parse fix. |
| Package coverage (C6) | Flat `.deb`s vouched by exact path **plus a Filename-suffix fallback** that covers every `sources.list` form. |
| Limitation #1 (C7) | Content-keyed adoption-failure record with capped, jittered backoff; rollback re-activates a retained snapshot. |
| Limitation #2 (C8) | Flip barrier, by-hash served from retained snapshots, monotonic `Last-Modified` for anchors. |
| Limitation #3 (C9) | Suites that no client has requested for `gc.suite_idle_ttl` are reaped; suite creation is bounded. |

One additive schema migration (v6 → v7) carries all persistent state (§6).

## 2. Goals and non-goals

### Goals

1. Flat repositories in every `sources.list` form apt supports refresh exactly
   as `dists/` suites do, with the same snapshot contract.
2. With `integrity.refuse_unvouched_debs = true`, flat `.deb`s are vouched
   against their signed `Packages` hashes with **no false refusals** for any
   observed real repository layout.
3. Long-term operation is bounded: no suite, retry loop, in-memory map or DB
   table grows or churns without bound; restarts and upgrades are safe.
4. Every behaviour is proven by tests, including multi-day virtual-clock
   simulations and a real-`apt` end-to-end suite.

### Non-goals (see §10 for the follow-up list)

- Changing the shipped default `adoption.enabled = false`. **With adoption
  disabled no repository (flat or `dists/`) ever refreshes its metadata** —
  that is the documented Phase 1 contract (`TestCheck_NoAdopter_ChangeOnlyLogged`).
  This design is inert without adoption; the docs will say so prominently.
- Host-root flat repositories (`deb https://h/ /`).
- Merging the `/repo` and `/repo/.` twin suites (accepted as two suites).
- Synchronous revalidation of anchors on the request path.

## 3. Background and review findings

Eight fable/max review agents examined PR #8 and main. The findings this
design depends on (all verified against code, apt source, or live metadata)
are summarised below. Finding IDs such as S-3, I-1, I-4, R-1, F-2, B-3 and C-1
refer to the 2026-09 security review (`docs/security-review-findings.md`,
maintained outside this branch).

**apt URL construction** (`apt-pkg/deb/debmetaindex.cc`
`constructMetaIndexURI`, `ArchiveURI`; `methods/http.cc` sends the path
verbatim, no dot-segment normalisation):

| `sources.list` | Metadata requested | `.deb` requested |
|---|---|---|
| `deb <uri>/ /` | `<uri>/InRelease`, `<uri>/Packages.gz` | `<uri>/<Filename>` |
| `deb <uri> ./` | `<uri>/./InRelease`, `<uri>/./Packages.gz` (literal `./`) | `<uri>/<Filename>` |
| `deb <uri> sub/` | `<uri>/sub/InRelease` | `<uri>/<Filename>` — base is the **parent** |
| `deb <uri> a/b/` | `<uri>/a/b/InRelease` | `<uri>/<Filename>` — base two levels up |

The `.deb` base is the `sources.list` URI, which the proxy never sees.

**Real flat repositories** (fetched 2026-09-26/27):

| Repo | Form | Filename shape | Notes |
|---|---|---|---|
| pkgs.k8s.io `core:/stable:/v1.34/deb` | `/ /` | `amd64/kubectl_…deb` | InRelease; no by-hash |
| NVIDIA CUDA `…/ubuntu2204/x86_64` | `/ /` | `./cuda_…deb` (11,533 stanzas) | `dpkg-scanpackages` shape |
| nvidia `libnvidia-container/stable/deb/amd64` | `/` | `./libnvidia-…deb` | **Release has only a SHA512 block**; Packages has SHA256 |
| pkg.jenkins.io `debian-stable binary/` | `sub/` | `binary/…` **and** `direct/…` | no InRelease (detached) |
| download.sublimetext.com `apt/stable/` | `a/b/` | `files/…` → `/files/…` | base is the host root |

**PR #8 claims found false:** apt does not fall back from `/ /` to
`./InRelease`; `.deb`s do not always live under the suite directory (Jenkins,
Sublime); `TestAdopter_FlatSuiteAdopts` passes on main; the `%2E` test never
exercises the real (already-decoded) request path.

**PR #8 defects (for the PR comment, not reproduced here):** partial suite
identity (flat `by-hash/`, pdiff, `Contents-*` stay stale forever); strict-mode
scoping refuses Jenkins-style `.deb`s and is escaped by an ancestor flat suite;
false-positive `snapshot_index_target_404` on `dists/` suites; `./` Filename
repos cannot be vouched; suite creation widened with no GC. No malicious code
in the PR or its tests.

**Pre-existing problems this design fixes:**

- `ParsePackages` rejects `./` Filenames (`validateMemberPath`) → CUDA-class
  repos can never adopt once parsed.
- Adoption happens only when upstream *changes* after first contact
  (`freshness.go` "changed" branch is the only `Adopter.Run` caller).
- A deterministic adoption failure retries every cooldown forever and
  re-downloads/re-hashes every member; validators are never persisted on
  "changed", so no 304 is possible while upstream is ahead; the admin
  "lagging" signal can never fire.
- Upstream rollback to a previously adopted InRelease fails forever with
  `ErrSnapshotNaturalKeyAdopted`.
- The flip is atomic per request, not per client: every request re-reads
  `current_snapshot_id`, so a client whose `apt update` straddles
  `CommitAdoption` gets the old InRelease and new indexes. The triggering
  client is always exposed (the T1 check runs after its InRelease is
  served), as is any concurrent updater, the detached `Release`/`Release.gpg`
  pair, and every client during a suite's *first* adoption. By-hash requests
  for the displaced snapshot 404 although its blobs are retained (apt then
  falls back to the plain path and fails the hash check). Anchor
  `Last-Modified` comes from pool mtime, so a flip onto an older-mtime blob
  yields a silent 304.
- Nothing deletes `suite_freshness` rows; any 200 for `<dir>/InRelease`
  creates a permanent suite, lock-map entry and periodic poll.
- Security finding S-3: percent-encoded `#`/`?` desynchronise the cache key
  from the upstream URL (string concatenation of a decoded path) and let a
  client mint unbounded `dists/` suites. R-1: reconcile re-parses the raw
  InRelease envelope.

## 4. Decisions (maintainer, 2026-09-26/27)

| # | Decision |
|---|---|
| D1 | Reimplement from scratch on `feature/flat-repos`; credit the PR author; nothing posted to GitHub. |
| D2 | Vouch flat `.deb`s: exact path + Filename-suffix fallback (C6). |
| D3 | Eager first adoption for **all** suites, `dists/` included (C4). |
| D4 | Limitation #2: flip barrier + retained by-hash + monotonic `Last-Modified`; no synchronous revalidation (C8). |
| D5 | Also in scope: SHA512-only Release files, unsigned Release-only repos, canonical `dists/` suite paths. |
| D6 | Unsigned adoption on by default, safeguarded (C3/C5), knob `adoption.adopt_unsigned`. |
| D7 | `/repo` and `/repo/.` remain two suites. |

## 5. Design

### C1. Request canonicalisation and upstream URLs

1. `proxy.Parse` rejects (`ErrInvalidPath` → 400) a decoded path containing a
   NUL or ASCII control byte (0x00–0x1F, 0x7F). apt never sends them.
2. `UpstreamURL` is built with `(&url.URL{Scheme, Host: authority, Path: path}).String()`
   instead of string concatenation, so a decoded `#`, `?`, `%` or space is
   re-escaped and reaches upstream as the same file the client named. This
   closes S-3's key/URL desync and the double-decoding of literal `%`
   (epoch-in-filename `.deb`s). A single helper `proxy.BuildUpstreamURL` is
   used by every site that composes an upstream URL from a canonical path
   (handler miss path, `buildMemberURL`, `reconstructAnchor`,
   `CommitAdoption` anchor seed, hot prefetch).
3. Unit tests pin that for every path without special characters the new
   URL is byte-identical to the old concatenation (no behaviour change for
   ordinary traffic), and that `#`, `?`, `%`, space, `+`, `~`, `:`, `@` and
   UTF-8 round-trip correctly through `url.Parse` in the fetcher.

**Out of scope (noted, pre-existing):** member URLs use the port-less
canonical host (`AIDEV-TODO(port-correctness)`); non-default-port upstreams
still cannot adopt. New code that derives sibling URLs (C4) derives them
from the anchor's stored `upstream_url` and so is port-correct.

### C2. Suite identity (`proxy.SuitePath`)

A single canonicality predicate applies to both grammars. A path has a suite
only if every segment of its suite directory and member part is non-empty,
is not `.` or `..` (except the one flat exception below), and the whole path
contains none of `#`, `?`, `%`, `\` (after decoding).

**`dists/` grammar** (tightened, D5): `^/(repo/)?dists/<codename>/<member>`
where `<member>` is non-empty. The suite is `/<repo>/dists/<codename>`, exactly
as today. New rejections: `..`/`.`/empty segments anywhere, forbidden
characters, and a path that ends at the codename (`/foo/dists/InRelease` is no
longer the nonsense suite `/foo/dists/InRelease`). Real apt clients never send
these shapes; such requests keep working through the Phase 1 `url_path` path.

**Flat grammar** (only when the `dists/` grammar does not match): the path is
`D/<member>` where `<member>` is one of

- a single segment: `InRelease`, `Release`, `Release.gpg`,
  `Packages[.<ext>]`, `Sources[.<ext>]`, `Contents-<name>` — where `<ext>` is
  `[a-z0-9]+` but not `deb`, `udeb`, `ddeb` or `dsc`;
- `by-hash/SHA256/<64 hex>` or `by-hash/SHA512/<128 hex>`;
- `Packages.diff/Index` or `Sources.diff/Index`.

The suite is the **literal** directory `D`. `D` must satisfy the canonicality
predicate except that it may end in exactly one `/.` (the `deb <uri> ./`
form, so `/repo/./Packages.gz` maps to suite `/repo/.` and strips to member
`Packages.gz`). `D` must not be the host root (`/`, `/.`, or empty): host-root
flat repos are unsupported (documented; the handler logs
`flat_host_root_unsupported` once per host).

Rationale for *whole-directory* identity: every metadata shape apt requests
for a flat source (anchors, indexes, pdiff index, by-hash, apt-file
`Contents-*`) must share the suite, or it bypasses the snapshot contract and
goes stale while the anchor flips (PR #8's split-brain). An unknown `D` is
harmless: `trySnapshotHit` finds no `suite_freshness` row and falls through.

`proxy.IsFlatSuitePath(suite)` reports the grammar; `proxy.FlatBase(suite)`
returns `D` with one trailing `/.` removed (the `.deb` base candidate, C6).

The handler's `indexTargetPathRE` alert becomes layout-aware: root-level
`Packages*`/`Sources*` count as IndexTargets only when the suite is flat, so a
`dists/` request for `/dists/noble/Packages` no longer raises
`snapshot_index_target_404` (PR #8 regression F4).

### C3. Anchor forms and snapshot storage

| Form | Anchor file(s) | Verification | Vouches `.deb`s | Strict-mode participant |
|---|---|---|---|---|
| inline | `InRelease` | GPG (clearsigned) | yes | yes |
| detached | `Release` + `Release.gpg` | GPG (detached) | yes | yes |
| **unsigned** (new) | `Release` | none | **never** | **never** |

`suite_snapshot` gains `anchor_form` (`inline`/`detached`/`unsigned`,
backfilled from the hash columns). The existing CHECK constraint allows only
"inline" (`inrelease_hash` set) or "detached" (`release_hash` and
`release_gpg_hash` set) shapes and cannot be altered without rebuilding a
table that four others reference. An unsigned snapshot therefore stores its
`Release` hash in `inrelease_hash` (the "single anchor blob" slot) with
`anchor_form = 'unsigned'`. `AIDEV-NOTE` on the column and every reader:
**`inrelease_hash` is the single-anchor blob for both `inline` and `unsigned`
forms; `anchor_form` says which file it is.** Readers that must change:
`detectForm`/`priorAdoptionForm`, `CommitAdoption` Step 3c (seed `/Release`
for unsigned), url_path GC identity guard (d) (form-aware anchor path),
`reparseSnapshotRelease`, `HostCurrentSnapshotsCoverage`, admin status.

The natural key gains the form: v7 replaces `idx_suite_snapshot_natural`
with a unique index on `(scheme, host, suite_path, COALESCE(inrelease_hash,
release_hash), anchor_form)`, so a repository that starts signing a Release
whose bytes were previously adopted unsigned is a new snapshot, not a
collision. `InsertCandidateSnapshot`'s collision lookup includes the form.

**Unsigned safeguards (D6):**

1. `adoption.adopt_unsigned` (default `true`) gates the form.
2. Eligible only when the check observed `InRelease` **and** `Release.gpg`
   both absent (404/410, or a 200 whose body fails the signature sniff —
   see C4) and a `Release` that looks like a Release file (has a
   `SHA256:`/`SHA512:` block).
3. **Never downgrade:** a suite whose current snapshot is `inline` or
   `detached` never adopts an unsigned candidate. Enforced in the check *and*
   inside `CommitAdoption` (reads the prior form in the flip transaction;
   returns `ErrUnsignedDowngrade`). A refused downgrade is a content-class
   failure (C7): the signed snapshot keeps serving and the admin page shows
   the reason.
4. **Always re-probe:** every check of an unsigned suite probes `InRelease`
   and `Release.gpg`; the moment either appears the suite adopts the signed
   form (upgrade is allowed, downgrade is not).
5. Unsigned snapshots create **no** `package_hash` rows, have
   `package_coverage_complete = 0`, and are excluded from
   `HostCurrentSnapshotsCoverage`. They never vouch a `.deb` and never change
   a host's strict-mode posture, so a forged unsigned Release (a MITM on the
   cache→upstream path, no key needed) cannot weaken strict mode.

Threat note (documented): such a MITM can make the cache adopt forged
unsigned content for a suite that has never been signed. Clients that verify
signatures reject it (availability loss while the attack lasts; it heals on
the first check after, because the genuine Release differs or its signature
reappears). `[trusted=yes]` clients accept it — exactly as they would
fetching directly.

### C4. Freshness check and eager first adoption

`checkLocked` dispatches on the suite's current snapshot form:

- **inline / detached** — unchanged algorithms (conditional GET of
  `InRelease`, or `Release` then `Release.gpg` on change), plus the C7
  deferral gate and C7 validator persistence.
- **unsigned** — (1) GET `InRelease`; a 200 that passes the clearsign sniff
  (`-----BEGIN PGP SIGNED MESSAGE-----` after optional whitespace) →
  inline adoption request (upgrade). (2) Conditional GET `Release`. (3) GET
  `Release.gpg`; a 200 that passes the signature sniff (armored header, or a
  first byte with the OpenPGP packet bit 0x80 set) → detached adoption
  request (upgrade), even if `Release` is unchanged. (4) Otherwise, if
  `Release` changed → unsigned adoption request.
- **no current snapshot, adopter wired (eager first adoption, D3)** — probe
  in apt's order without validators: `InRelease` (clearsign sniff) → inline
  request; else `Release` (Release sniff) then `Release.gpg` (signature
  sniff) → detached request, else unsigned request if `adopt_unsigned`, else
  a content-class "not adoptable" failure record (C7) so the suite stops
  probing every cooldown. The adoption request is produced even when the
  bytes equal the cached `url_path` copy — that is the point of eager
  adoption.
- **no current snapshot, no adopter (adoption disabled)** — today's Phase 1
  behaviour, byte for byte (observation only). Pinned by the existing tests.

Sibling anchor URLs are derived from any anchor `url_path` row's
`upstream_url` (replace the last path segment), falling back to
`scheme://host + path` (today's `reconstructAnchor` policy).

**Seeding and prompt first adoption.** The handler seeds `suite_freshness`
after a successful miss of `<suite>/InRelease` (body passes the clearsign
sniff) **or** `<suite>/Release` (body passes the Release sniff) — the latter
is new and gives detached/unsigned suites (Jenkins) a row on first contact.
The seed leaves `last_check_at` NULL and the handler then fires the T1 check,
so eager adoption starts within seconds of first contact instead of after the
next cooldown. The C8 barrier holds the flip until the triggering client's
update has quiesced.

**Validators.** `CommitAdoption` (and reactivation, C7) copy the adopted
snapshot's `inrelease_etag`/`inrelease_lastmod` into `suite_freshness`, so the
check after an adoption gets a 304 instead of re-downloading the body.

New `freshness_check` result values: `first_adoption`, `changed_deferred`
(C7). `inrelease_change_seen_at` is set only when it is NULL (first sight of
the pending change is preserved).

### C5. Adoption input changes

**Flat IndexTargets.** Root-level `Packages*`/`Sources*` (and their
`.diff/Index`) are IndexTargets (fatal on fetch failure, never tolerated as
optional). Group keys: root `Packages` → pseudo-arch `all` (always required
by `missingRequestableIndexGroups`); root `Sources` → `source`.
`archFromFilteredPath` scopes root `Sources` to `source` and never filters
root `Packages` (it carries every architecture). The three lockstep regexes
(`indexTargetRE`, `indexTargetGroupRE`/flat group, handler
`indexTargetPathRE`) keep their shared `AIDEV-NOTE`, extended to name the
flat alternative and the layout-awareness rule.

**Filenames.** `ParsePackages` uses a new `validateFilename` (Packages/Sources
Filenames only; `ParseRelease` keeps the strict `validateMemberPath`, whose
`.` rejection protects `isMetadataSelfPath` from aliasing):

- allow `.` segments (`./x.deb`), kept **literally** in the vouched path
  because apt requests `<base>/./x.deb` verbatim;
- reject (fatal, as today): empty, absolute, NUL, backslash, `%`, `?`, `#`,
  `..` segment, empty segment, length > 4096.

**SHA512-only Release files.** `ParseRelease` reads the `SHA256:` block as
today; if absent it reads the `SHA512:` block (128-hex digests). If both are
absent it fails as today. `ReleaseMember` gains `SHA512`. For a member with
no SHA256 declaration, `adoptMember` verifies size + SHA512 and returns the
computed SHA256 of the verified bytes; `runShared` stores that as the
member's `declared_sha256` (a *derived* anchor: bytes whose SHA512 matched a
signed declaration). By-hash: alias rows are written for every declared
algorithm among SHA256 and SHA512 (`by-hash/SHA512/<hex>` is what apt
requests when the Release advertises SHA512 with `Acquire-By-Hash: yes`); the
adoption's own by-hash fetch uses the algorithm it verifies with.
Limitation (documented): integrity-skipped SHA512-only members are not
recorded for skip-repair (the table requires a SHA256); reconcile still heals
missing requestable IndexTargets.

**Unsigned form.** `Adopter.RunUnsigned(ctx, suite, releaseBytes, etag,
lastmod)`: Release sniff → `ParseRelease` → members fetched and verified
against the Release → snapshot members `Release` + members + by-hash aliases →
no `package_hash` rows, coverage false → `CommitAdoption` with
`anchor_form = 'unsigned'`.

**R-1 fix (opportunistic, required by the form changes).**
`reparseSnapshotRelease` parses only the *verified plaintext*: for `inline`
it decodes the clearsigned message (no crypto; the bytes were verified at
adoption and are content-addressed) and parses the plaintext; for `detached`
and `unsigned` it parses the `Release` blob. It never parses the raw
envelope, so declarations hidden inside the signature armor cannot reach the
snapshot or `package_hash`. The decode is wrapped in `recover()` (defense
against the I-1 class; a stored verified blob cannot trigger it, but the
reconcile path must not be able to crash the daemon).

### C6. Package coverage for flat suites (D2)

**Materialisation.** For a flat suite with signed form, let `C =
proxy.FlatBase(suite)`. Each Packages stanza with literal Filename `F` yields
one `package_hash` row at path `C + "/" + F` (literal, `./` kept) and
`flat_filename = dotClean(F)` (the Filename with `.` segments removed;
`dists/` rows keep `''`). Dedup, conflict and identity rules are unchanged
from `dists/` suites. `package_coverage_complete` uses the `dists/`
definition (every `Packages*` directory — for flat, the root — has a
parseable variant and at least one vouching row survives). Sources and pdiff
hashing are not built for flat suites (source hashing is best-effort today;
documented).

**Lookup (`DeclaredHashesForPath`).**

1. Exact: current-snapshot rows where `path = P` (unchanged; covers
   `deb <uri>/ /` and `deb <uri> ./`, including CUDA's literal `./x.deb`).
2. Only if (1) returns nothing — the **suffix fallback**: let `P'` =
   `dotClean(P)`; if `P'` contains `..` or an empty segment, stop. For each
   split of `P'` into `B + "/" + F` (at most 32 splits), look up
   current-snapshot rows with `flat_filename IN (F…)` on the same
   `(scheme, host)` via the partial index, join `suite_snapshot` for the
   suite path, and keep a row iff `B` is an ancestor-or-self of that suite's
   `FlatBase` (`strings.HasPrefix(base+"/", B+"/")`).
3. The existing dispatch then applies unchanged: 0 distinct hashes →
   strict-mode predicate; 1 → validate; ≥ 2 → `package_hash_conflict` 502.

This is exactly apt's rule (`.deb` URL = `<uri>/<Filename>` with `<uri>` an
ancestor-or-self of the Release directory) with no guessing, one row per
stanza, and no phantom paths. It covers k8s (exact), CUDA (exact, and the
dot-cleaned form via fallback), Jenkins `binary/` and `direct/` (fallback,
base = parent), and Sublime `files/` (fallback, base = host root).

Exact rows take precedence, so a flat suite nested under another cannot
manufacture a conflict for the parent's own `.deb`s. The fallback only runs
for paths that no row vouches exactly, so `dists/` traffic that is vouched
pays nothing; an unvouched `.deb` pays one indexed query.

**Consumers.** Hit-path validation, miss-path validation and strict mode all
go through `DeclaredHashesForPath` and inherit the fallback. Hot prefetch and
the retention mirror rule join on the exact path, so they apply to flat repos
whose `.deb`s sit under the suite directory (k8s, CUDA, OBS) and not to
parent-base repos (Jenkins, Sublime), whose cached `.deb`s are retained by
recency and hold-grace like any unvouched file (documented).

**Strict mode** keeps the host-wide rule (PR #8's directory scoping is not
reimplemented): refuse an unvouched `.deb` iff every current *signed*
snapshot on the host has complete coverage. Flat snapshots now normally have
complete coverage, so strict mode protects flat hosts; seeded-but-unadopted
suites and unsigned snapshots never count.

### C7. Adoption failure record, backoff and rollback (limitation #1)

**Record** (v7 columns on `suite_freshness`): `adoption_fail_hash`,
`adoption_fail_class`, `adoption_fail_reason`, `adoption_fail_count`,
`adoption_fail_first_at`, `adoption_fail_last_at`, `adoption_next_retry_at`.
Written by dedicated statements (`RecordAdoptionFailure`,
`ClearAdoptionFailure`); `PutSuiteFreshness` and the handler seed never touch
them. `CommitAdoption` and reactivation clear them in the flip transaction.

**Content key.** `inline`: sha256(InRelease); `detached`:
sha256(sha256(Release) ‖ sha256(Release.gpg)); `unsigned`: sha256(Release).
A re-signature or any byte change is new content and is attempted at once.

**Classification** (only selects the backoff schedule; nothing is suppressed
forever):

- *content* — GPG failures other than a signature created in the future /
  expired (clock skew); Release/Packages parse failures (not local I/O);
  commit constraint violations (B-3 duplicate member path); unsigned
  downgrade refused; "not adoptable" (unsigned with `adopt_unsigned =
  false`).
- *environment* — everything else, including unknown errors (fail towards
  retrying): transport, 5xx, member 404-driven guard failures, size/hash
  mismatch, local I/O, writer errors, clock-skew GPG errors.

Prerequisites: `%w` instead of `%v` at the wrap sites that hide the cause
(natural-key collision, commit failure, `readPackagesBlob` I/O), and distinct
sentinels for the guard failures, so classification uses `errors.Is/As`.

**Backoff.** environment: `min(2m · 2^(n−1), 1h)`; content:
`min(1h · 2^(n−1), adoption.failure_retry_max)` (default `24h`; `0s` restores
today's retry-every-cooldown behaviour). ±25 % jitter (injectable seam).
`next_retry_at` is clamped on read to `now + cap`, so a backward clock step
cannot defer an attempt beyond the cap.

**Gate.** In the "changed" branch (and the eager-first-adoption branch), if
the observed content key equals `adoption_fail_hash` and `now <
next_retry_at`, the check records `result = changed_deferred` and spawns no
adoption. The check cadence itself is unchanged, so new upstream content is
still adopted within one cooldown.

**Restart.** At checker start every failing row gets `next_retry_at = now +
rand(0, periodic_refresh)` (one retry per failing suite after a restart —
the recovery path after an operator installs a key or changes pins, which
already requires a restart), keeping `count` and `first_at`.

**Rollback re-activation.** `InsertCandidateSnapshot` returns a typed
`*SnapshotAdoptedError{ID}` on a natural-key collision with an adopted row.
If that snapshot still exists (retained as displaced) and belongs to the same
suite and form, the adopter calls `cache.ReactivateSnapshot(id)` through the
C8 barrier: one transaction that bumps the target's member refcounts, flips
the pointer, re-syncs the anchor `url_path` row (Step 3c), sets
`current_since`, copies validators, clears the failure record, and decrements
the prior snapshot's refcounts. A flapping mirror becomes a cheap pointer
flip; a reaped target falls back to a normal failure record.

**Visibility.** Admin `suites[]` gains `anchor_form`, `flat`,
`last_requested_unixtime`, and the failure record ("adoption failing 3d 2h ·
untrusted_signer · retry in 41m"). The "lagging" annotation is computed from
`inrelease_change_seen_at` (now first-seen) against `now`, fixing the signal
that could never fire. Gauge `acu_adoption_failing_suites{class}`; INFO
`adoption_deferred` only on state transitions; WARN
`adoption_failing_persistently` at counts 3, 6, 12, …; the per-attempt WARN
`adoption_run_failed` stays (attempts are now rare).

### C8. Flip consistency (limitation #2)

**Layer 1 — flip barrier** (new package `internal/flipgate`, keyed by the
freshness suite key):

- `AnchorServed(key)` — the handler records the time it finished serving an
  anchor (`<suite>/InRelease` or `<suite>/Release`, status 200/304) from any
  path: snapshot hit, url_path hit, miss, stale serve.
- `AwaitFlip(ctx, key)` — before serving an anchor, the handler waits while a
  flip is pending for the suite, bounded by `quiet + 15s` (≤ 60s), the
  request context and handler shutdown; it then re-reads the snapshot pointer.
- `BeginFlip(ctx, key) (release func())` — the adopter calls it immediately
  before `CommitAdoption`/`ReactivateSnapshot`: marks the flip pending (new
  anchor serves now block and cannot refresh recency) and waits until no
  anchor was served for `adoption.flip_quiet_period` (default `15s`; `0s`
  disables the barrier), bounded by a 60s safety cap and the adoption
  context. `release` (deferred on every path) closes the pending channel
  exactly once.

Guarantee: any client that received the old anchor did so at least `quiet`
before the flip, so its index requests within `quiet` are served from the old
snapshot; anchor requests arriving during the barrier are answered from the
new snapshot. Bounded costs: at most `quiet` of extra adoption latency, at
most `quiet + 15s` of added latency for an anchor request that arrives
mid-flip. No goroutines are created (waiters are request goroutines); state
is in memory; entries expire once idle for `quiet`, so the map is bounded by
recently served suites. No lock ordering cycle: the adopter waits on the
clock while holding the freshness per-suite mutex; the handler never takes
that mutex; the DB writer is not held during the wait. Residual (documented):
a commit slower than the hold bound (F-2 on very large suites) releases
waiters to the old anchor; by-hash clients are still covered by layer 2.

**Layer 2 — by-hash from retained snapshots.** When `trySnapshotHit` misses
the current snapshot for a `by-hash/SHA256|SHA512/<hex>` member, it looks the
alias up in the suite's other *adopted* snapshots (newest first; at most
`gc.keep_displaced + 1`) and serves it with that snapshot's
`X-Cache-Snapshot`. For SHA256 aliases the served blob hash must equal the
URL's hex. No recovery fetch for displaced snapshots; missing → 404 as today.
A stray pool blob under an adopted suite still 404s
(`byhash_test.go:127` stays valid). Counter
`acu_serve_byhash_displaced_total{host}` measures averted failures.

**Layer 3 — monotonic `Last-Modified` for anchors.** Snapshot-served anchors
use `max(pool mtime, suite_freshness.current_since)` as the `ServeContent`
modtime; `current_since` is set on every flip and reactivation. A client's
`If-Modified-Since` from the previous anchor therefore always gets a 200
after a flip.

The README's "clients always see a coherent metadata set" claim is replaced
with the precise guarantee and residual.

### C9. Suite lifecycle (limitation #3)

**Client-interest signal.** `suite_freshness.last_requested_at` (v7,
backfilled to migration time). Sole writer: `cache.TouchSuite`, called
asynchronously from every client-serving path for anchor requests
(`InRelease`, `Release`, `Release.gpg`; status < 400; including 304s, HEAD
and stale serves), coalesced by `WHERE last_requested_at < now − 3600`. apt
always fetches an anchor first (verified in apt source), so one touch per
`apt update` per suite is complete. Freshness, adoption and GC never write
it; every INSERT path sets it to `now`.

**Reap.** A new first GC pass calls a `SuiteReaper` interface implemented by
`freshness.Checker`: suites with `last_requested_at < now − gc.suite_idle_ttl`
(default `720h`; `0s` disables), oldest first, at most
`gc.suite_reaps_per_tick` (default 25), skipped in the startup pass and in
the first hour of uptime. For each: `TryLock` the per-suite mutex (busy →
skip), then `cache.ReapIdleSuite` in one writer transaction:

1. re-read the row; bail if the stamp is newer than the cutoff
   (compare-and-delete — a concurrent seed or touch wins);
2. run the Step-8 refcount decrement for the current snapshot (without it
   every member blob of the suite is immortal);
3. delete the suite's metadata `url_path` rows (anchors and `is_metadata`
   rows under `suite + "/"`, capped at 1,000; the rest age out) through
   `deleteURLPathRowTx`;
4. delete the `suite_freshness` row.

Then purge in-memory state: lock-map entry (`CompareAndDelete`), flipgate
entry, gpg once-logger entries (optional `Forget` interface). The reconcile
memo is swept against current snapshot ids each scheduler tick.
Snapshot GC gains sub-job C ("suiteless"): adopted snapshots whose suite row
no longer exists are reaped regardless of `keep_displaced`. The mutex is
load-bearing: `CommitAdoption` Step 6 is an upsert and would resurrect a row
reaped mid-adoption; `Check` also re-validates after `TryLock` that the map
still holds the same mutex.

**Bounded creation.** Seeding requires the anchor sniff (C4). A check never
creates a row on failure (split `PutSuiteFreshness` into create/update).
`freshness.max_suites` (default 5000) caps tracked suites: beyond it seeding
is refused (serving is unaffected) with a rate-limited WARN
`suite_seed_rejected` and a counter. `Check` validates before `LoadOrStore`
that the suite has a row or an anchor `url_path` row (security finding I-4).

With eager first adoption (D3) a reaped suite that returns is re-adopted
within seconds of its next anchor miss, so reaping does not leave returning
repositories unvouched.

### C10. Observability, admin and logs

New events (documented in `docs/log-fields.md`): `suite_reaped`,
`suite_seed_rejected`, `adoption_deferred`, `adoption_failing_persistently`,
`adoption_snapshot_reactivated`, `adoption_flip_barrier`,
`adoption_unsigned_downgrade_refused`, `flat_host_root_unsupported`; new
fields `anchor_form`, `flat`, `snapshot_displaced` on existing lines. New
metrics are host-labelled or unlabelled (no suite labels; cardinality
unchanged) and are added to the SPEC5 inventory comments. `gc_run_complete`
and `LastRunSummary` gain `suites_reaped`, `suites_busy`, `suiteless_reaped`.

### C11. Configuration

| Key | Default | Rule |
|---|---|---|
| `adoption.adopt_unsigned` | `true` | bool |
| `adoption.failure_retry_max` | `"24h"` | `0s` = legacy retry-every-check; else ≥ `1h` |
| `adoption.flip_quiet_period` | `"15s"` | `0s` disables; ≤ `45s` |
| `freshness.max_suites` | `5000` | ≥ 1 |
| `gc.suite_idle_ttl` | `"720h"` | `0s` disables; else ≥ `24h`; WARN if < `gc.url_path_ttl` |
| `gc.suite_reaps_per_tick` | `25` | ≥ 1 |

All added to `packaging/config/config.toml.default`, `docs/configuration.md`,
and config validation tests. Bool defaults are pre-populated in
`defaultConfig()` (the existing presence-sensitive pattern).

## 6. Migration v6 → v7

Additive DDL plus backfills, forward-only, in a new `migrations[6]` entry
(no existing migration is edited). `CurrentSchemaVersion = 7`.

```sql
ALTER TABLE suite_freshness ADD COLUMN last_requested_at      INTEGER;
ALTER TABLE suite_freshness ADD COLUMN current_since          INTEGER;
ALTER TABLE suite_freshness ADD COLUMN adoption_fail_hash     TEXT;
ALTER TABLE suite_freshness ADD COLUMN adoption_fail_class    TEXT;
ALTER TABLE suite_freshness ADD COLUMN adoption_fail_reason   TEXT;
ALTER TABLE suite_freshness ADD COLUMN adoption_fail_count    INTEGER NOT NULL DEFAULT 0;
ALTER TABLE suite_freshness ADD COLUMN adoption_fail_first_at INTEGER;
ALTER TABLE suite_freshness ADD COLUMN adoption_fail_last_at  INTEGER;
ALTER TABLE suite_freshness ADD COLUMN adoption_next_retry_at INTEGER;

UPDATE suite_freshness SET last_requested_at = CAST(strftime('%s','now') AS INTEGER);
UPDATE suite_freshness
   SET current_since = (SELECT adopted_at FROM suite_snapshot
                         WHERE snapshot_id = suite_freshness.current_snapshot_id)
 WHERE current_snapshot_id IS NOT NULL;

ALTER TABLE suite_snapshot ADD COLUMN anchor_form TEXT NOT NULL DEFAULT ''
  CHECK (anchor_form IN ('', 'inline', 'detached', 'unsigned'));
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
```

Notes: the partial index scans `package_hash` once at migration (seconds on a
million-row cache; logged by the existing `schema_migrating` line) and stays
empty until flat suites adopt. Backfilling `last_requested_at` with the
migration time gives every existing suite a full idle window after upgrade.
A v7 database is refused by older binaries (existing forward-only policy;
release notes). Tests: a v6 fixture migrates, columns and indexes exist,
backfills are correct, and the new unique index accepts every pre-existing
row.

## 7. Testing strategy

Each component is built test-first. Beyond unit tests:

- **Real-cache integration** (`newAdoptionTestEnv` pattern): flat adoption for
  each real Filename shape; SHA512-only; unsigned incl. never-downgrade and
  upgrade; reactivation refcount bookkeeping; `ReapIdleSuite` refcounts and
  sub-job C; `DeclaredHashesForPath` exact/fallback/conflict/negative
  (`..`, `//`, sibling prefix `/flat2`); migration from a v6 fixture.
- **Handler, strict mode on:** every `sources.list` form × Filename shape is
  served and validated; unknown `.deb` refused; mixed `dists/` + flat host;
  unsigned suite never changes posture; seeded-unadopted suite never changes
  posture; hash mismatch → 502, nothing cached.
- **Flip barrier:** deterministic tests with a fake clock and adoption/commit
  hooks — plain-path straddle reproduced on main and fixed; anchor held and
  answered from the new snapshot; zero wait when idle; `Release.gpg` not
  blocked; waiter timeout; client cancel and shutdown; commit failure
  releases waiters; first adoption. The existing chaos test's straddle log
  becomes an assertion.
- **Virtual-clock simulations** (fake clock, no sleeps): 30 days of T1 every
  5s + T2 ticks against an upstream that serves an untrusted body, then new
  content, then a rollback, with a 2h backward clock step — adopter
  invocations stay within the backoff bound; 90 days × 15-minute ticks for
  suite GC (touched / idle / near-TTL / busy / restart mid-way / clock jump).
- **Chaos:** reap vs concurrent seed/adoption; reactivation vs GC.
- **Real-apt end-to-end** (new `e2e/flat/` compose suite, run by
  `make e2e-flat`): an nginx upstream publishing flat repos in the k8s,
  CUDA (`./`), Jenkins (`sub/`, detached, `direct/`), Sublime (`a/b/`,
  `files/`), SHA512-only and unsigned (`[trusted=yes]`) shapes plus a
  `dists/` repo on the same host; cache with adoption and strict mode on.
  Assertions: install succeeds from every repo; the exact request lines apt
  sends (pins the `./` wire shape for the tested apt version); zero
  `unvouched_deb_refused` and zero `snapshot_index_target_404`; publishing
  v2 upstream reaches clients within one cooldown; 20 `apt update` runs
  during a flip all succeed; cache restart mid-way preserves state.
- `go test -race ./...`, `golangci-lint run ./...`, `gofmt` clean at every
  phase.

## 8. Documentation

- `README.md`: supported repository layouts (flat forms, `/` vs `./` twins,
  host-root limitation, unsigned and SHA512-only support, the
  adoption-enabled requirement); corrected coherence claim; GC bullet.
- `docs/configuration.md`: new keys (C11); `[adoption]` trust text for
  unsigned repos and the never-downgrade rule; `[integrity]` strict-mode text
  for flat and unsigned suites; `[gc]` suite reaping; `[freshness]`
  eager first adoption and `max_suites`.
- `docs/log-fields.md`: new events and fields (C10).
- `docs/admin-ui-spec.md`: suites table fields.
- `packaging/config/config.toml.default`.
- `AIDEV-NOTE`s at every changed invariant (listed per component above).
- `Contributors.md`: credit for the flat-repository report.

## 9. Implementation phases

Each phase ends with the full race suite and lint green, a fable/max review
of the phase diff, fixes, and a commit.

| Phase | Content |
|---|---|
| P0 | v7 migration + types; C1 canonicalisation and `BuildUpstreamURL`; C2 suite grammar; layout-aware alert. |
| P1 | C5 adoption input: `validateFilename`, flat IndexTargets, SHA512-only, R-1 reparse; C3 `anchor_form` plumbing and unsigned form; C4 check dispatch, eager first adoption, seeding, validator persistence. |
| P2 | C6 flat package coverage and the `DeclaredHashesForPath` fallback; strict-mode exclusion of unsigned snapshots. |
| P3 | C7 failure record, classification, backoff, restart policy, rollback reactivation, admin visibility. |
| P4 | C8 flipgate, retained by-hash, monotonic `Last-Modified`. |
| P5 | C9 touch, reap pass, sub-job C, creation bounds, in-memory purge, I-4. |
| P6 | `e2e/flat/` real-apt suite, virtual-clock simulations, documentation, config file, final full review. |

## 10. Out of scope / follow-ups

1. Default `adoption.enabled = false` freezes every repository's metadata
   (Phase 1 contract). Candidate: enable by default, or add Phase 1
   invalidate-on-change refresh.
2. I-1: clearsign panic in the adoption goroutine (needs `recover()` in the
   adoption/repair goroutines and a crash marker).
3. Port-correct member URLs for non-default-port upstreams.
4. Host-root flat repositories.
5. Synchronous anchor revalidation; stage-then-flip `CommitAdoption` (F-2).
6. Per-suite identity pinning (`Origin`/`Label`/`Codename`) and `Valid-Until`
   rollback checks (C-1).
7. Flat Sources/pdiff hashing; flat `Translation` files.
8. Skip-repair records for SHA512-only members.
9. An operator endpoint to force a recheck or forget a suite (a suite that
   was once signed and whose vendor stops signing keeps its last signed
   snapshot until reaped).
10. `+` in upstream paths behind S3-style servers.

## 11. Risks

| Risk | Mitigation |
|---|---|
| Eager first adoption adopts every never-adopted suite after upgrade (burst). | Bounded by `max_concurrent_adoptions`; failures back off (C7). |
| Tightened `dists/` grammar drops suite identity for odd client paths. | Only non-canonical shapes apt never sends; they keep Phase 1 serving; covered by tests. |
| Unsigned adoption abused by a MITM. | Never-downgrade, re-probe, no vouching, no strict-mode effect (C3). |
| Barrier adds anchor latency during flips. | ≤ `quiet + 15s`, only mid-flip; `0s` disables; metrics. |
| Suite reaping removes a live suite. | 30-day default, per-tick cap, compare-and-delete, uptime floor; a false reap self-heals on the next anchor miss. |
| Suffix fallback cost on unvouched `.deb`s. | One indexed query, only when exact lookup finds nothing. |
| v7 migration time on large caches. | Partial index build logged; seconds for ~1M rows. |
