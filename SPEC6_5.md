# apt-cacher-ultra — Phase 6.5 Specification

This document specifies the contract for Phase 6.5: **repo-feature
parity** with apt-cacher-ng across the three classes of repository
content the existing daemon either underserves or doesn't validate
— **multi-arch beyond amd64**, **source-package caching**, and
**pdiff (delta-Packages) hash validation**. It is a delta over
[SPEC.md](SPEC.md) (Phase 1), [SPEC2.md](SPEC2.md) (Phase 2),
[SPEC3.md](SPEC3.md) (Phase 3), [SPEC4.md](SPEC4.md) (Phase 4),
[SPEC5.md](SPEC5.md) (Phase 5), and [SPEC6.md](SPEC6.md) (Phase 6).
Sections that carry forward unchanged say so explicitly and point
at the prior spec; sections that change describe only the delta.

Phase 6.5 is **additive and mostly-already-working**:

- Phase 1's `url_path` cache already serves any path the proxy
  fetches, including arm64/i386 `.deb` files, source artifacts
  (`.dsc`, source tarballs), and pdiff index/diff files. Phase 6.5
  does NOT re-architect the cache-hit path.
- Phase 2's adoption pipeline already fetches every Release-listed
  member regardless of architecture or content type. Phase 6.5 adds
  parsers and validators for the file types Phase 2 was content to
  fetch-but-not-interpret.
- Phase 3's `package_hash` table accommodates non-`.deb` paths by
  schema design (no CHECK constraint on `path`). Phase 6.5 extends
  the population logic to source artifacts and pdiff diff files —
  same table, more rows, more file kinds parsed.
- Phase 3's strict mode (`refuse_unvouched_debs`) stays `.deb`-only.
  Phase 6.5 does **not** introduce strict-mode refusal of source or
  diff artifacts (see §1.2).

The net effect is that a Phase 6.5 daemon serves debian-style source
repositories (`deb-src` lines in `sources.list`), arm64 / armhf /
i386 binary repositories, and apt's PDiff client mode (`Acquire::PDiffs
"true"`) with the same correctness guarantees Phase 2 / Phase 3 give
binary `.deb` traffic — without rebuilding the request pipeline or
the singleflight machinery.

**Implementation-context note.** Several design decisions in this
spec depend on assumed behavior of the existing serve-time hash-
validation path (`internal/handler/handler.go`). Sites flagged
**"AIDEV-VERIFY"** call out specific assumptions to confirm during
implementation; if a flagged assumption proves false, the affected
section's design is revisited in a SPEC6_5 review pass before code
lands.

---

## 1. Goals & non-goals

### 1.1 Phase 6.5 goals

1. **Multi-arch parity.** The cache already adopts every binary-
   architecture's `Packages` index from a Release file. Phase 6.5
   pins this with regression tests, adds an optional
   `[adoption].architectures` allowlist for operators who want to
   filter (cache-disk savings on single-arch fleets), and surfaces
   the per-arch population in the §10.4 status payload.

2. **Source-package caching.** Adoption gains a `Sources` parser
   analogous to Phase 3's Packages parser. Source artifacts
   (`.dsc`, `*.tar.gz`, `*.tar.xz`, `*.tar.bz2`, `*.diff.gz`,
   `*.debian.tar.*`) get hash-validated on serve when they fall
   under a current snapshot's `package_hash` rows — same code path
   as Phase 3 binary validation, just population-extended.

3. **pdiff hash validation.** Adoption gains a `Packages.diff/Index`
   parser. The diff files referenced from the Index (named like
   `<unix-timestamp>.gz`) get inserted as `package_hash` rows; on
   serve, the existing hash-validation path validates them. `.diff/`
   files survive Index rotation: an Index that no longer lists a
   given diff implies the diff's `package_hash` row is dropped on
   the next snapshot adoption (existing Phase 3 snapshot lifecycle).

The three goals share infrastructure:

- The existing `internal/freshness/adoption.go` adoption loop —
  Phase 6.5 only adds member-type parsers.
- The existing `package_hash` schema — no schema migration.
- The existing `internal/handler/handler.go` serve-time validation
  hook — Phase 6.5 verifies it is path-agnostic (existing code
  already handles non-`.deb` paths in the validation step;
  AIDEV-VERIFY).
- The existing Phase 6 TLS MITM path — source-only and pdiff-aware
  HTTPS upstreams work identically to binary HTTPS upstreams.

### 1.2 Phase 6.5 non-goals (deferred)

Carried forward unchanged from earlier phases:

- Streaming-while-fetching, per-byte upstream read timeouts
  (FUTURE-REVIEW.md §1).
- Per-suite freshness cadence variation (FUTURE-REVIEW.md §2).
- Operator-triggered manual GC or adoption (Phase 7+).
- The Phase 7 control-plane work (mutating admin endpoints, CA
  rotation primitive, write-role auth) — Phase 6.5 stays read-only.

Newly deferred in Phase 6.5:

- **Strict-mode refusal of source / diff artifacts.** The existing
  `refuse_unvouched_debs` flag stays `.deb`-only. Extending it to
  `.dsc` / source tarballs / diff files would require a new flag
  (the existing one's name is `_debs`) AND a serve-time policy
  decision for each artifact class. Phase 6.5 hash-validates these
  artifacts when they have known hashes, but the "no hash known →
  refuse" branch is not extended. Operators who want strict-mode
  parity for source/diff artifacts wait for a future phase.
- **Source-only repositories.** A repo that publishes ONLY
  `Sources` files (no `binary-<arch>/Packages`) is not a normal
  Debian shape; the cache's existing adoption loop should handle
  this transparently, but Phase 6.5 does not add explicit support
  or tests for the source-only case.
- **Build-Depends-aware adoption.** The cache does not parse
  source `Build-Depends:` fields and does not pre-fetch the
  binary build dependencies of a source package. Source repos
  cache like any other repo: lazy fetch on client request.
- **pdiff generation.** The cache caches upstream-published
  pdiffs; it does NOT compute new diffs between adopted snapshots.
  apt-cacher-ng has an option to do this; Phase 6.5 does not.
  Operators who need generated pdiffs run their own diff service
  upstream.
- **`Contents-<arch>` index parsing.** `Contents-amd64.gz`,
  `Contents-source.gz`, etc. are listed in Release and adopted as
  members (they end up in `snapshot_member` for hash-validation on
  re-fetch). Phase 6.5 does NOT parse them — they are not
  referenced by apt's normal package fetch flow, and parsing them
  costs adoption time without operational benefit.
- **`Packages.xz` mid-snapshot disagreement recovery.** Phase 3
  already detects and rejects the case where `Packages.gz` and
  `Packages.xz` disagree on `Architecture:` for the same path
  (`adoption.go:1258` `ErrAdoptionParseFailed`). Phase 6.5 inherits
  this; the same disagreement check applies to Sources files
  cross-variant.
- **Per-component (main/contrib/non-free) filtering.** Operators
  who want to skip non-free entirely run a derived Release
  upstream or use a per-suite mirror that strips non-free.
  Filtering inside the cache is not a Phase 6.5 goal — it
  introduces a new adoption-skip dimension whose interaction with
  GPG signing is non-trivial.
- **`udeb` (mini-deb) hash validation.** The Debian installer
  uses `.udeb` artifacts; these are listed in
  `debian-installer/binary-<arch>/Packages` rather than the main
  Packages files. Phase 6.5 treats `.udeb` like `.deb` for hash
  validation IF the Packages file that lists them is adopted (the
  d-i Packages file is a Release member like any other). Strict-
  mode refusal continues to be `.deb`-only per `handler.go:1816`.

### 1.3 Resolved during Phase 6.5 scoping

This spec is the scoping artifact (no separate PHASE-6_5-SCOPING.md);
the design questions below are decided here. Subsequent revision
passes can re-open them.

- **Schema delta.** None. `package_hash` accommodates source and
  diff paths with no migration. `snapshot_member` already covers
  Sources / pdiff Index files as Release members.
- **Strict-mode extension.** No (per §1.2 — out of scope).
- **Architecture filter shape.** New `[adoption].architectures`
  list (TOML); empty = "adopt every arch the Release advertises"
  (preserves Phase 6 behavior). Non-empty = "skip Packages files
  whose `binary-<arch>/` segment is not in the list."
- **pdiff Index parser language.** RFC822-stanza format identical
  to apt's view (one stanza per current Index, with `SHA256-Patches`
  and `SHA256-Download` blocks). Parser modeled on `ParseRelease`.
- **pdiff diff-file hash storage.** Reuse `package_hash` keyed on
  the suite-relative diff path
  (e.g. `main/binary-amd64/Packages.diff/2026-05-09-1234.56.gz`).
  Adoption-time insert; snapshot-lifecycle DELETE per Phase 3 v3.
- **`source` as architecture value.** When inserting source-
  artifact rows into `package_hash`, the `architecture` column
  takes the value `"source"` (Debian convention). Multi-arch
  filter (`[adoption].architectures`) treats `"source"` as a
  pseudo-arch — operators can opt out of source adoption via the
  filter.
- **PDiff index members not under `binary-<arch>/`.** A repo
  that publishes pdiffs for `Sources` (path
  `main/source/Sources.diff/Index`) gets the same Phase 6.5
  treatment.

---

## 2. Wire contracts (deltas over SPEC §2 / SPEC2 §2 / SPEC3 §2.7 / SPEC5 §2 / SPEC6 §2)

### 2.1 Listener inventory — unchanged

Phase 6.5 adds no new listener and changes no port. The proxy
listener (`cache.listen`), the optional TLS-to-cache listener
(`cache.listen_tls`), and the admin listener (`admin.listen`) all
carry forward from earlier specs.

### 2.2 URL-path coverage (NEW documentation)

Phase 6.5 codifies which apt URL shapes the cache validates with
hashes (Phase 3 / Phase 6.5) versus serves trust-upstream. The
table is normative for §6 request handling.

| URL path shape | Phase | Validated via | Strict-refusable |
|---|---|---|---|
| `/dists/<suite>/(In)?Release(\.gpg)?` | 1 / 2 | `suite_snapshot` | n/a |
| `/dists/<suite>/.../Packages(\.(gz|xz|bz2))?` | 2 | `snapshot_member` (the file's own SHA256) | n/a |
| `/dists/<suite>/.../Sources(\.(gz|xz|bz2))?` | **6.5** | `snapshot_member` | n/a |
| `/dists/<suite>/.../Packages\.diff/Index` | **6.5** | `snapshot_member` | n/a |
| `/dists/<suite>/.../Sources\.diff/Index` | **6.5** | `snapshot_member` | n/a |
| `/dists/<suite>/.../Contents-<arch>(\.gz)?` | 2 (member-fetch only) | `snapshot_member` | n/a |
| `/pool/.../*.deb` | 3 | `package_hash` (declared SHA256 from Packages) | yes (`refuse_unvouched_debs`) |
| `/pool/.../*.udeb` | 3 (implicit) | `package_hash` if d-i Packages adopted | no |
| `/pool/.../*.dsc` | **6.5** | `package_hash` from Sources `Files:` block | no |
| `/pool/.../*.tar.{gz,xz,bz2}` | **6.5** | `package_hash` from Sources `Files:` block | no |
| `/pool/.../*.diff.gz` | **6.5** | `package_hash` from Sources `Files:` block | no |
| `/pool/.../*.debian.tar.{gz,xz}` | **6.5** | `package_hash` from Sources `Files:` block | no |
| `/dists/<suite>/.../Packages\.diff/<unix-timestamp>\.gz` | **6.5** | `package_hash` from Index `SHA256-Patches:` | no |
| `/dists/<suite>/.../Sources\.diff/<unix-timestamp>\.gz` | **6.5** | `package_hash` from Index `SHA256-Patches:` | no |
| Anything else | trust-upstream (Phase 1 cache) | not validated | no |

The strict-refusable column is the boolean `isDebPath(p)` test in
`internal/handler/handler.go:1821` — it carries forward unchanged
from SPEC3 §6.1 (Phase 6.5 deliberately preserves the `.deb`-only
gate per §1.2).

### 2.3 Request log line additions — delta over SPEC5 §10.1

The existing per-request log (Phase 1+) gains four new fields,
each populated when the served path falls in one of the new
Phase 6.5 categories:

- `path_class` — string. Values: `binary_deb` (existing default
  for `.deb`), `binary_udeb`, `source_dsc`, `source_tarball`,
  `pdiff_index`, `pdiff_patch`, `metadata`, `other`.
- `validated_hash` — bool. `true` iff the served bytes were
  matched against a `package_hash` (or `snapshot_member`) row's
  declared SHA256. Existing Phase 3 `.deb` validation flips this
  to `true`; Phase 6.5 expands the population to source artifacts
  and pdiff patches.
- `architecture` — string. Populated for `binary_*` paths
  (extracted from the path's `binary-<arch>/` segment), `source`
  for source artifacts, empty otherwise.
- `package_name` — string. Best-effort: populated from the
  `package_hash` row's `package_name` column (Phase 3 v3) when a
  matching row exists; empty when serving via `url_path` only.

These fields are additive — existing log consumers continue to
parse the fields they care about and ignore the new ones.

### 2.4 Status JSON additions — delta over SPEC5 §10.4

The `GET /?format=json` payload (SPEC5 §10.4) gains one new
top-level section `repo_coverage` and one extension to the
existing per-host `cache_summary` payload:

```json
{
  ...
  "repo_coverage": {
    "architectures_seen": ["amd64", "arm64", "armhf", "source"],
    "architectures_filter": ["amd64", "arm64"]   or [] if unset,
    "snapshots_with_sources":   <int>,
    "snapshots_with_pdiff":     <int>,
    "package_hash_rows": {
      "binary":    <int>,
      "source":    <int>,
      "pdiff":     <int>,
      "total":     <int>
    }
  },
  "cache_summary": {
    ...existing per-host fields...,
    "by_architecture": {
      "amd64":  {"package_hash_count": <int>, "blob_count": <int>, "blob_bytes": <int>},
      "arm64":  {"package_hash_count": <int>, "blob_count": <int>, "blob_bytes": <int>},
      "source": {"package_hash_count": <int>, "blob_count": <int>, "blob_bytes": <int>}
    }
  }
}
```

`architectures_seen` is the union of `architecture` values across
every current snapshot's `package_hash` rows. `architectures_filter`
echoes the operator's `[adoption].architectures` setting (empty list
when unset).

`snapshots_with_sources` counts current snapshots that have at
least one `package_hash` row with `architecture = "source"`.
`snapshots_with_pdiff` counts current snapshots whose
`snapshot_member` table contains at least one `*.diff/Index` path
(populated regardless of whether any individual diff file was ever
fetched).

The `by_architecture` sub-object is keyed by architecture string;
the daemon emits one entry per architecture present in the host's
current snapshots' `package_hash` rows.

### 2.5 Status HTML additions — delta over SPEC5 §10.5

The status HTML page gains a new "Repository coverage" section
between "Cache" and "Listeners". Renders:

- Architectures seen (as a comma-separated list).
- Architectures filter (or "(unfiltered)" when empty).
- Snapshots with Sources adoption (count).
- Snapshots with pdiff adoption (count).
- A small table summarizing `package_hash` row counts per kind
  (binary / source / pdiff / total).

The per-host expansion in the existing Cache section gains the
`by_architecture` breakdown.

---

## 3. URL canonicalization (Remap) — unchanged

Carry forward from SPEC §3 / SPEC2 §3 / SPEC4 §3 / SPEC5 §3 /
SPEC6 §3. Phase 6.5 introduces no new canonicalization rules.
Source-package paths and pdiff paths are routed through the same
Remap pipeline as binary paths; in practice Remap rules are
typically scheme/host-only, so all path shapes within a host
receive identical canonicalization.

---

## 4. Storage layout (delta over SPEC §4 / SPEC2 §4 / SPEC4 §4 / SPEC6 §4)

### 4.1 SQLite schema — no migration

Phase 6.5 makes **zero schema changes**. The existing tables
accommodate the new content classes:

- `package_hash` (Phase 2 + Phase 3 v3): `(canonical_scheme,
  canonical_host, path, snapshot_id) → declared_sha256,
  package_name, architecture`. Used for binary `.deb`s today;
  Phase 6.5 inserts rows for source artifacts (`.dsc`, source
  tarballs) and pdiff patch files. Path uniqueness is preserved
  (binary `/pool/.../foo_1.0_amd64.deb` and source
  `/pool/.../foo_1.0.dsc` have distinct paths). The `architecture`
  column takes `"source"` for source rows and the binary-`<arch>`
  segment value for binary and udeb rows; for pdiff patch rows it
  takes the `binary-<arch>` segment from the Index path
  (e.g. `amd64` for `main/binary-amd64/Packages.diff/<ts>.gz`),
  or `source` for `main/source/Sources.diff/<ts>.gz`.
- `snapshot_member` (Phase 2): unchanged. `Sources(.gz|.xz|.bz2)`
  and `Packages.diff/Index` are listed in Release SHA256 blocks
  and inserted as members during adoption — Phase 2 §7.5 already
  handles them; Phase 6.5 just pulls signal out of them.
- `suite_snapshot`, `suite_freshness`, `url_path`, `blob` — all
  unchanged.

### 4.2 Pool layout — unchanged

`<cache.dir>/pool/<sha256-prefix>/<sha256>` is the storage shape
(Phase 4). Source tarballs, `.dsc` files, and pdiff patch files
land here exactly like `.deb`s — content-addressed. Phase 4 GC
reaps unreferenced pool blobs the same way.

### 4.3 Refcount semantics — unchanged

Phase 4 v4's `blob.refcount` + `blob.refcount_zeroed_at` semantics
apply identically. Each `package_hash` row insert increments the
target blob's refcount by 1; each delete decrements. Source rows
and pdiff rows participate in the same accounting.

---

## 5. Configuration (delta over SPEC §5 / SPEC2 §5 / SPEC4 §5 / SPEC5 §5 / SPEC6 §5)

### 5.1 `[adoption]` block additions

Phase 3's `[adoption]` block gains one new field:

```toml
[adoption]
# ... existing Phase 3 fields ...

# Architectures the adoption pipeline keeps. Empty = adopt every
# binary-<arch>/Packages and source/Sources file the Release lists.
# Non-empty = only adopt members under binary-<arch>/ (or
# source/) where <arch> is in the list. Saves disk on caches whose
# clients only fetch a subset of the upstream's published arches.
#
# Use the literal Debian architecture name: "amd64", "arm64",
# "armhf", "i386", "ppc64el", "s390x", etc. The pseudo-arch
# "source" controls whether Sources files are adopted.
#
# Examples:
#   architectures = []                            # default — all
#   architectures = ["amd64"]                     # x86_64-only fleet
#   architectures = ["amd64", "arm64"]            # mixed binary, no source
#   architectures = ["amd64", "arm64", "source"]  # binary + source
architectures = []
```

Field semantics:

- **`architectures`** (`[]string`) — Optional allowlist. The
  default empty list preserves Phase 6 behavior (every
  Release-listed Packages / Sources file is adopted regardless
  of arch). When non-empty, the adoption loop:
  1. Inspects each Release member's path. For paths matching
     `<component>/binary-<arch>/(Packages(.(gz|xz|bz2))?|Packages.diff/Index)`
     or `<component>/source/(Sources(.(gz|xz|bz2))?|Sources.diff/Index)`,
     extracts the `<arch>` (or the literal `"source"` for
     source/...).
  2. Skips the member when the extracted arch is NOT in the
     allowlist. Skip emits the existing `adoption_member_skipped`
     Warn (`internal/freshness/adoption.go:errAdoptionMemberSkipped`)
     with a new field `reason="arch_not_in_allowlist"`.
  3. Members whose path doesn't match either pattern (e.g.
     `Release.gpg`, `Contents-<arch>`, `i18n/Translation-*`) are
     unaffected by the filter — they pass through unchanged. The
     filter scope is **only** the per-arch / per-source index
     files and their pdiffs; per-arch `Contents` files are NOT
     filtered (they're niche, GC reaps them via the existing
     refcount path if not referenced).

### 5.2 Validation rules — additions

New startup config-error fail-closed cases:

- **`architectures_invalid_value`** — Any entry in
  `architectures` contains a character outside
  `^[a-z][a-z0-9]*$` (Debian arch name shape; lowercase letters
  + digits, must start with a letter). Daemon refuses to start
  with the offending value named.
- **`architectures_too_many`** — More than 32 entries. Anti-foot-
  gun guard; real fleets care about ≤ 5 arches. Operators who
  legitimately need more raise the cap by editing the config
  validator.

No other Phase 6.5 config additions; all source-package and pdiff
behavior is wholly schema-driven (presence of Sources /
`Packages.diff/Index` in the Release file triggers parsing).

### 5.3 Default config block additions

`packaging/config/config.toml.default` gains the new field with
operator guidance:

```toml
[adoption]
# ... existing Phase 3 fields ...

# Phase 6.5: limit which Debian architectures' Packages/Sources
# indices the cache adopts. Empty (default) = adopt all the Release
# advertises. Use Debian arch names; "source" controls Sources
# adoption.
#   amd64-only fleet: ["amd64"]
#   amd64+arm64 with source repos: ["amd64", "arm64", "source"]
architectures = []
```

---

## 6. Request handling (delta over SPEC §6 / SPEC2 §6 / SPEC3 §6 / SPEC4 §6 / SPEC5 §6 / SPEC6 §6)

### 6.1 Path-class classification (NEW)

Every served request is classified by `path_class` per the §2.2
table. The classifier is a pure-text predicate over `req.Path`:

```go
func classifyPath(p string) PathClass {
    switch {
    case strings.HasSuffix(p, ".deb"):
        return ClassBinaryDeb
    case strings.HasSuffix(p, ".udeb"):
        return ClassBinaryUdeb
    case strings.HasSuffix(p, ".dsc"):
        return ClassSourceDsc
    case strings.HasSuffix(p, ".diff.gz") ||
         strings.HasSuffix(p, ".tar.gz") ||
         strings.HasSuffix(p, ".tar.xz") ||
         strings.HasSuffix(p, ".tar.bz2") ||
         strings.HasSuffix(p, ".debian.tar.gz") ||
         strings.HasSuffix(p, ".debian.tar.xz") ||
         strings.HasSuffix(p, ".debian.tar.bz2"):
        return ClassSourceTarball
    case isPdiffIndexPath(p):
        return ClassPdiffIndex     // .../Packages.diff/Index, .../Sources.diff/Index
    case isPdiffPatchPath(p):
        return ClassPdiffPatch     // .../Packages.diff/<unix-ts>.gz
    case isMetadataPath(p):
        return ClassMetadata       // Release / InRelease / Packages.* / Sources.* / Contents-*
    default:
        return ClassOther
    }
}
```

`isPdiffIndexPath` matches the literal substring
`Packages.diff/Index` or `Sources.diff/Index` at end-of-path
(Index is the only filename inside the .diff/ directory that
isn't a patch file).

`isPdiffPatchPath` matches a path containing `Packages.diff/` or
`Sources.diff/` followed by a filename matching
`^[0-9.-]+\.gz$` (apt-ftparchive's Index entries name patches by
their generation timestamp, e.g. `2026-05-09-1234.56.gz`). The
regex is intentionally loose — apt-ftparchive's actual format is
`<YYYY-MM-DD-HHMM.SS>.gz` but historical archives have used
slightly different conventions; the digit/dot/dash predicate
covers them all without admitting traversal patterns.

The classifier is consulted at request-log emit (§2.3) and at
serve-time validation (§6.2).

### 6.2 Serve-time hash validation — extension

The Phase 3 hash-validation hook (`internal/handler/handler.go`
serve path) is **already path-agnostic at the validation step**:
when a request's canonical (scheme, host, path) tuple has a row
in `package_hash` for a current snapshot, the served body's
SHA-256 is compared against the declared hash; on mismatch, the
serve fails with the existing Phase 3 error path. (AIDEV-VERIFY:
confirm during implementation.)

Phase 6.5 leaves the validation step untouched. The change is in
**which paths have rows in `package_hash`**:

- **Source artifacts** (`.dsc`, source tarballs, `.diff.gz`,
  `.debian.tar.*`) — adoption-time insertion via §7.1.
- **pdiff patch files** — adoption-time insertion via §7.3.

The strict-mode refusal predicate stays `.deb`-only via the
existing `isDebPath` gate (`handler.go:1821`); Phase 6.5 does
NOT route source / pdiff requests through `classifyStrictMode`.
A `.dsc` request whose `package_hash` row is missing serves as
trust-upstream (Phase 1 fall-through), same as today.

#### 6.2.1 Hash mismatch behavior

When `package_hash` exists for the path AND the served bytes'
SHA-256 differs from the declared hash:

- The serve fails with the existing Phase 3 hash-mismatch error
  (`502 Bad Gateway`, body `error: "hash_mismatch"`).
- A `serve_hash_mismatch` Warn is emitted with `path_class`
  populated per §2.3.
- The cached blob is NOT auto-purged (existing Phase 3 behavior
  — operator decides via Phase 7+ `/admin/cache/clear` once
  available, or by `apt-cacher-ultra cache clear` subcommand,
  also future).

The mismatch path is identical for binary and source / pdiff
artifacts; the only difference is the `path_class` field's value
in the log line.

#### 6.2.2 Multi-arch serve — already supported

A request for `main/pool/p/pkg/pkg_1.0_arm64.deb` flows through
the same handler as the amd64 case. The handler's `package_hash`
lookup keys on the literal path; the row's `architecture` column
is populated by adoption. No code path is arch-specific.

Phase 6.5 adds tests that exercise:

- arm64-only request against a multi-arch repo: cache miss →
  fetch → cache hit on next request.
- `[adoption].architectures = ["amd64"]`: arm64 Packages file
  NOT adopted; arm64 `.deb` requests fall to trust-upstream
  (Phase 1 cache only); strict mode is inert because no
  `package_hash` row exists.

### 6.3 pdiff request flow — already supported, now validated

apt's pdiff client sequence:

1. Client GETs `Packages.diff/Index` (it was an adopted member
   per §7.3; cache serves from `snapshot_member` with the
   verified Index bytes).
2. Client parses the Index, picks N most recent diffs whose
   chain produces the current Packages from the client's local
   copy.
3. Client GETs each `<unix-ts>.gz` patch file. Cache serves from
   `url_path` if cached, fetches upstream + caches if not. With
   Phase 6.5 hash validation, the served bytes are matched
   against `package_hash` (populated from the Index's
   `SHA256-Patches:` block) — mismatch → §6.2.1 error path.
4. Client applies the patches to its local Packages. (The
   apply step happens client-side; the cache is uninvolved.)

Phase 6.5 does NOT validate the *result* of patch application —
that's the apt client's responsibility (and apt does check the
final Packages SHA-256 against the Release-declared value).

### 6.4 Source-package request flow — new validation

apt's source-fetch client sequence (`apt-get source <pkg>`):

1. Client GETs `dists/<suite>/main/source/Sources(.gz|.xz)`. With
   Phase 6.5 adoption, the file is adopted as a Release member
   (it always was) AND its inner stanzas are parsed (§7.1) to
   populate `package_hash` for the listed source artifacts.
2. Client parses the Sources file, computes which artifacts to
   fetch for the named source package version (typically: one
   `.dsc`, one upstream tarball, one Debian patches/tarball).
3. Client GETs each artifact under `pool/`. Cache validates
   against `package_hash` per §6.2.

The single-host request flow is identical to the binary case.
Multi-host: a source repo at one host that mirrors binaries from
another (rare but legal) is treated independently per host —
each host's adoption populates its own `package_hash` rows.

---

## 7. Freshness and adoption (delta over SPEC2 §7 / SPEC3 §7 / SPEC6 §7)

### 7.1 Sources file parsing (NEW)

The existing adoption loop reads each member's bytes
(`internal/freshness/adoption.go` `runShared`); for members whose
path matches `(.*)/source/Sources(\.(gz|xz|bz2))?` (regex; loose),
Phase 6.5 dispatches to a new `parseSources` function modeled on
the existing `parsePackages`.

`parseSources` reads RFC822-style stanzas from the Sources body
(after decompression if applicable). For each stanza:

- Extract `Package:`, `Version:`, `Directory:` (the suite-relative
  base path), and the `Files:` block (the SHA256 variant when
  present, falling back to MD5/SHA1 only when the stanza has
  zero SHA256 — strict refusal otherwise to match Phase 2's
  trust-SHA256-only posture).
- For each file in the `Files:` block, compute the suite-relative
  path `<Directory>/<filename>` and insert a `package_hash` row:
  - `canonical_scheme`, `canonical_host`: from the suite identity.
  - `path`: the suite-relative path computed above, joined with
    the host prefix as the existing adoption helper does.
  - `declared_sha256`: from the Files block.
  - `package_name`: `<Package>` from the stanza.
  - `architecture`: literal `"source"`.
  - `snapshot_id`: the in-flight snapshot.

Implementation notes:

- Sources files are typically much smaller than Packages files
  (few thousand source packages for a typical suite vs tens of
  thousands of binaries × multiple arches), so the existing
  `maxDecompressedPackagesBytes` cap (`adoption.go:32` —
  `256 << 20`) is amply oversized. Phase 6.5 reuses the same cap
  rather than introducing a Sources-specific one.
- Stanza parsing reuses the existing `internal/parser` deb822
  helpers (Phase 3). Same memory bounds, same line caps.
- Cross-variant disagreement detection (`Sources.gz` says SHA256
  X, `Sources.xz` says SHA256 Y for the same file) reuses the
  existing Phase 3 `ErrAdoptionParseFailed` predicate.

`parseSources` is ALWAYS called when a Sources member is adopted,
even when `[adoption].architectures` excludes `"source"`. The
filter (§5.1) decides whether the Sources file member is fetched
at all; once fetched, it's parsed.

### 7.2 Architecture filter (NEW)

The Phase 6.5 arch filter is a Release-member predicate applied
at the start of `runShared`'s per-member loop. The predicate's
inputs:

- The member's suite-relative path.
- `[adoption].architectures` (empty list = filter inert).

Predicate decision:

```
if architectures is empty:
    return KEEP

m = match(path, "(?:^|/)(binary-([a-z][a-z0-9]*)|(source))(?:/|$)")
if m matches:
    arch = m.group(2) or "source" (whichever captured)
    if arch in architectures:
        return KEEP
    return SKIP, reason="arch_not_in_allowlist"

return KEEP   # path doesn't carry a per-arch / per-source segment
```

The skipped member emits `adoption_member_skipped` Warn with the
new reason field; outcome is `success` for the overall adoption
(skipping is not a failure).

The filter applies to:

- `<comp>/binary-<arch>/Packages(\.(gz|xz|bz2))?` — Packages
  index files.
- `<comp>/binary-<arch>/Packages.diff/Index` — pdiff index files.
- `<comp>/source/Sources(\.(gz|xz|bz2))?` — Sources index files.
- `<comp>/source/Sources.diff/Index` — pdiff source-index files.

The filter does NOT apply to:

- `Release`, `InRelease`, `Release.gpg` — already handled by
  Phase 2 metadata-self.
- `<comp>/binary-<arch>/Release` — per-component-arch Release
  files (some archives publish them); always kept (auxiliary
  metadata, very small, no harm in caching).
- `<comp>/Contents-<arch>(\.gz)?` — Contents files; not relevant
  to package fetch flow, kept for `apt-file` compatibility.
- `<comp>/i18n/Translation-*` — i18n files; not arch-specific.
- `<comp>/dep11/*` — AppStream metadata; not arch-specific in
  the path layout (per-arch is in the file content).

### 7.3 pdiff Index parsing (NEW)

For each adopted Release member whose path ends in
`Packages.diff/Index` or `Sources.diff/Index`, Phase 6.5
dispatches to a new `parsePdiffIndex` function modeled on
`ParseRelease`.

The Index format (apt-ftparchive output):

```
SHA256-Current:
 <sha256> <size>
SHA256-History:
 <sha256> <size> <patch-name>
 ...
SHA256-Patches:
 <sha256> <size> <patch-name>
 ...
SHA256-Download:
 <sha256> <size> <patch-name>.gz
 ...
Canonical-Path: dists/<suite>/<comp>/binary-<arch>/Packages
```

Phase 6.5 cares about the `SHA256-Download:` block — that's the
list of compressed patch files apt actually fetches over the
wire. (`SHA256-Patches:` is the uncompressed form; apt uses the
download form for HTTP transit, then uncompresses locally before
applying.)

For each entry in `SHA256-Download:`:

- Extract `<patch-name>.gz` (the filename; e.g.
  `2026-05-09-1234.56.gz`).
- Compute suite-relative path: `<dirname-of-Index>/<patch-name>.gz`.
  E.g. for Index path `main/binary-amd64/Packages.diff/Index`
  the dirname is `main/binary-amd64/Packages.diff/` and the patch
  path is `main/binary-amd64/Packages.diff/2026-05-09-1234.56.gz`.
- Insert a `package_hash` row:
  - `path`: the suite-relative path computed above, joined with
    the host prefix per `runShared` conventions.
  - `declared_sha256`: from the `SHA256-Download:` block.
  - `package_name`: empty string (pdiff patches have no
    package identity).
  - `architecture`: extracted from the Index path's
    `binary-<arch>/` segment (or `"source"` for the
    `source/Sources.diff/` shape).

The `SHA256-Patches:` block (uncompressed-form hashes) is NOT
populated into `package_hash`; the cache never serves the
uncompressed form (apt fetches `.gz` over the wire and
decompresses client-side).

If the Index has neither `SHA256-Download:` nor `SHA256-Patches:`
blocks (malformed or empty Index), parsing returns no rows; the
Index file itself is still adopted as a `snapshot_member` with
its declared SHA-256, the same as any other Release member.

#### 7.3.1 pdiff snapshot lifecycle

When a new snapshot adopts a fresh Index:

- The new Index lists a fresh window of patches; its parsed rows
  go into `package_hash` for the new snapshot.
- The old snapshot's `package_hash` rows for patch files (under
  the old snapshot_id) are reaped per the existing Phase 3 v3
  snapshot-lifecycle logic — when the old snapshot is no longer
  current, its `package_hash` rows are GC-eligible per Phase 4.
- A patch file that's still on disk but no longer in any current
  snapshot's `package_hash` falls through to Phase 1 trust-
  upstream serve (the `url_path` row still hits) until Phase 4
  refcount-reaps the underlying blob.

This matches binary `.deb` lifecycle exactly: an old version's
`.deb` lingers in the cache until refcount reaches zero across
all snapshots, then GC reclaims it.

### 7.4 Adoption metrics — additions

The Phase 5 `acu_adoption_*` metric family (SPEC5 §10.4.6) gains
new labels and counters:

```
acu_adoption_members_skipped_total{reason}     counter   # reason: 4xx | arch_not_in_allowlist | optional_member_integrity | hash_mismatch | parse_failed
acu_adoption_sources_parsed_total{outcome}      counter   # outcome: ok | parse_failed
acu_adoption_pdiff_indexes_parsed_total{outcome} counter
acu_package_hash_rows_by_kind{kind}             gauge     # kind: binary | source | pdiff
```

`reason` is the existing Phase 2 `adoption_member_skipped` Warn
field hoisted into Prometheus. The cardinality is bounded (≤ 4
values).

`acu_package_hash_rows_by_kind` is a refresher-computed gauge
(Phase 5 §3.4). Refresh cost is one COUNT() per kind per refresh
tick; small relative to the existing per-host blob count
recompute.

---

## 8. Stale-and-Valid-Until — unchanged

Carry forward from SPEC2 §8. Phase 6.5 changes nothing here.
Source-artifact and pdiff-patch hash validation does not interact
with stale-validity: a `package_hash` row's freshness is governed
by the snapshot's freshness, which is governed by the suite's
Release adoption clock, unchanged.

---

## 9. Concurrency & deadlines (delta over SPEC §9 / SPEC2 §9 / SPEC3 §9 / SPEC4 §9 / SPEC5 §9 / SPEC6 §9)

### 9.1 Adoption budget — extended

Phase 2 §9.1 set the per-adoption wall-clock cap and the
`MaxConcurrentAdoptions` semaphore. Phase 6.5 changes neither
explicitly, but Sources / pdiff parsing adds CPU work to the
parse phase. Phase 4 §7.5.2's per-adoption heartbeat ticker
(`HeartbeatInterval`) covers the parse phase's gap-bounding —
no new sites are needed.

For a typical adoption:

- Sources parse: O(stanzas) ≈ tens of thousands of rows for
  debian-main amd64; ms-scale on modern CPU.
- Each pdiff Index parse: O(SHA256-Download lines) ≈ tens to
  hundreds; sub-ms each.
- Multi-arch: parse cost scales with arch count linearly.

Total adoption time stays within the existing Phase 2 budget for
real-world repos. Operators with extreme-size archives (debian-all
across all arches: hundreds of thousands of rows) should expect
per-suite adoption to take seconds, not the original Phase 2
budget assumed milliseconds — but the heartbeat ticker prevents
this from looking like a stalled adoption.

### 9.2 Per-blob memory cap — unchanged

The existing `maxDecompressedPackagesBytes` cap
(`adoption.go:32` — `256 << 20`) applies to every member
regardless of file kind. Real Sources files are well under
this limit (debian-main Sources is ~30 MiB uncompressed); pdiff
Index files are tens of KiB. The cap is the safety bound against
hostile-but-signed upstreams.

### 9.3 SQLite write contention — minor delta

`package_hash` row inserts during adoption already happen in one
transaction per snapshot (Phase 2 §9.4). Phase 6.5 adds source
and pdiff rows to the same transaction; row count grows by
roughly 5–15 % for repos with `deb-src` and pdiffs enabled.
WAL fsync overhead is negligible relative to the parse work.

Concurrent serve-time `package_hash` lookups during adoption
read from snapshots that are not yet `current` — they hit no
contention with the in-flight adoption's INSERTs (Phase 2's
snapshot-flip happens at COMMIT and is observed atomically by
serve queries).

---

## 10. Logging (delta over SPEC §10 / SPEC2 §10 / SPEC3 §10 / SPEC4 §10 / SPEC5 §10 / SPEC6 §10)

### 10.1 Existing event family additions

The Phase 2 `adoption_*` event family gains a new
`adoption_member_skipped` reason value:

- `arch_not_in_allowlist` — the member's path was filtered by
  `[adoption].architectures` (§7.2).
- `optional_member_integrity` — a non-IndexTarget member
  (`Contents-*`, `dep11`, `i18n`, icons) failed its
  fetch/size/hash check and was skipped under
  `[adoption].tolerate_optional_member_failures` (default true)
  rather than aborting the suite (SPEC2 §7.5.2). The Warn carries
  a `detail` field. IndexTarget members are never skipped this way.

The Phase 1 per-request log line gains the four new fields per
§2.3.

### 10.2 New events

`source_parsed` (Debug):

```json
{
  "event": "source_parsed",
  "canonical_scheme": "<scheme>",
  "canonical_host":   "<host>",
  "suite_path":       "<path>",
  "snapshot_id":      <int>,
  "stanza_count":     <int>,
  "package_hash_rows":<int>,
  "duration_ms":      <int>
}
```

`pdiff_index_parsed` (Debug):

```json
{
  "event": "pdiff_index_parsed",
  "canonical_scheme": "<scheme>",
  "canonical_host":   "<host>",
  "suite_path":       "<path>",
  "index_path":       "<member-path>",
  "snapshot_id":      <int>,
  "patch_count":      <int>,
  "duration_ms":      <int>
}
```

`source_parse_failed` (Warn) — Sources file parse error during
adoption (malformed stanza, unrecognized hash variant, exceeds
member byte cap):

```json
{
  "event": "source_parse_failed",
  "suite_path": "<path>",
  "member_path":"<member-path>",
  "stage":      "decompress|parse|hash_extract",
  "error":      "<message>"
}
```

A Sources parse failure does NOT abort adoption — it surfaces as
a Warn and the Sources member's `package_hash` rows are skipped.
(Same disposition as a `Packages.gz` parse failure in Phase 3;
adoption proceeds with the remaining members.)

`pdiff_index_parse_failed` (Warn) — symmetric to the above for
Index parse errors.

### 10.3 Metric additions

```
acu_adoption_members_skipped_total{reason}              counter
acu_adoption_sources_parsed_total{outcome}              counter
acu_adoption_pdiff_indexes_parsed_total{outcome}        counter
acu_package_hash_rows_by_kind{kind}                     gauge
acu_serve_hash_validated_total{path_class,outcome}      counter
```

`acu_serve_hash_validated_total` is the serve-time validation
counter, labeled by the §6.1 path class and the
`outcome=match|mismatch`. It is the operational counterpart to
the `serve_hash_mismatch` Warn — operators alarm on
`outcome="mismatch"` rate.

Cardinality budget per Phase 5 §10.4: `path_class` is a closed
enum (8 values); `kind` is a closed enum (3 values); `outcome` is
a closed enum (2 values); `reason` is a closed enum (≤ 5 values).
All comfortably under `metric_series_cap = 1024`.

### 10.4 Status JSON / HTML — already covered

§2.4 / §2.5 specify the additions.

---

## 11. Failure-mode catalog (delta over SPEC §11 / SPEC4 §11 / SPEC5 §11 / SPEC6 §11)

| ID | Failure | Behavior |
|---|---|---|
| H1 | `[adoption].architectures` contains an invalid arch name (e.g. `"AMD64"` uppercase, `"x86_64"` non-Debian-form) | Startup fails with `architectures_invalid_value` config-error |
| H2 | `[adoption].architectures` has > 32 entries | Startup fails with `architectures_too_many` config-error |
| H3 | Source repo's `Sources.gz` exceeds `maxDecompressedPackagesBytes` (256 MiB) | `source_parse_failed` Warn `stage=decompress`; Sources member's hash rows skipped; adoption proceeds with binary-only hash coverage |
| H4 | `Sources` stanza has no SHA256 in `Files:` block (only MD5/SHA1) | `source_parse_failed` Warn `stage=hash_extract`; that stanza's rows skipped; other stanzas in the same Sources file proceed |
| H5 | `Packages.diff/Index` has no `SHA256-Download:` block | `pdiff_index_parsed` Debug with `patch_count=0`; no `package_hash` rows inserted for diffs; Index member itself adopted normally |
| H6 | `Packages.diff/Index` contains entries that don't match `<digits>.gz` filename pattern | The malformed entries are skipped; well-formed entries inserted; `pdiff_index_parsed` Debug carries the count of well-formed entries only |
| H7 | Cross-variant Sources disagreement (e.g. `Sources.gz` says SHA256 X for `pkg.dsc`, `Sources.xz` says SHA256 Y for the same path) | `ErrAdoptionParseFailed` (existing Phase 3 predicate); whole adoption fails as `adoption_parse_failed`; prior snapshot continues to serve |
| H8 | A `.dsc` request's path has a `package_hash` row but the served bytes' SHA-256 mismatches | `serve_hash_mismatch` Warn `path_class=source_dsc`; serve fails with 502; existing Phase 3 mismatch path |
| H9 | A pdiff patch request's path has a `package_hash` row but the served bytes' SHA-256 mismatches | Same as H8 with `path_class=pdiff_patch` |
| H10 | An `arm64`-only client requests `pkg_*.deb` against a cache with `architectures = ["amd64"]` (so no arm64 Packages adopted, no package_hash row exists for arm64 paths) | Falls through to trust-upstream Phase 1 path — request fetched + cached without hash validation. No regression vs Phase 6 default behavior |
| H11 | Sources file lists a file with a path containing `..` segments | `source_parse_failed` Warn `stage=parse`; that stanza's rows skipped (defense-in-depth even though signed input means upstream has already vouched for the content) |
| H12 | Race: `[adoption].architectures` config changes between two adoptions (operator restarts daemon between adoptions to apply) | First adoption uses old filter, populates rows accordingly; second uses new filter, populates differently. Snapshot lifecycle handles the diff — old rows GC-reaped when their snapshot is no longer current |
| H13 | Two clients race on same arm64 `.deb` first-fetch | Existing Phase 1 singleflight semantics apply identically to non-amd64; no Phase 6.5 regression |
| H14 | A repo publishes pdiffs for one arch but not another (e.g. only `binary-amd64/Packages.diff/Index` exists) | Adoption parses the Index it finds; arches without Index just have no `package_hash` rows for diff paths. apt clients running against those arches fall back to whole-Packages fetch |
| H15 | Source artifact path collides with binary path (theoretically impossible due to extension conventions, but defensively) | The `package_hash` PRIMARY KEY (canonical_scheme, canonical_host, path, snapshot_id) means the second insertion would either overwrite (if INSERT OR REPLACE) or conflict (if plain INSERT). Implementation MUST use plain INSERT and surface conflict as `source_parse_failed` `stage=parse` Warn — collision indicates upstream malformedness |

---

## 12. Test strategy (delta over SPEC §12 / SPEC2 §12 / SPEC3 §12 / SPEC4 §12 / SPEC5 §12 / SPEC6 §12)

### 12.1 Unit tests

**Path classification** (`internal/handler/path_class_test.go`):

- Each PathClass enum value reached by at least one fixture path.
- Edge cases: trailing slash, query string, percent-encoding.
- Symmetric check: `classifyPath(p) != ClassOther` ⇒ at least
  one well-formed pattern.

**Sources parser** (`internal/freshness/sources_parse_test.go`):

- Real debian-main Sources fixture (committed test data, gzip-
  compressed): parse succeeds; row count matches expected; per-
  stanza Files block correctly extracted.
- Stanza with multiple SHA256 entries (uncommon but legal): all
  files inserted with their distinct hashes.
- Stanza with NO SHA256 (legacy MD5-only): rows skipped with the
  documented Warn.
- Stanza with `..` in a Files entry: rejected per H11.
- Cross-variant disagreement: detected per H7.

**pdiff Index parser** (`internal/freshness/pdiff_parse_test.go`):

- Real `Packages.diff/Index` fixture: rows extracted per
  `SHA256-Download:` block.
- Index missing `SHA256-Download:`: zero rows; Debug log emitted.
- Index with malformed patch filenames: well-formed entries kept,
  malformed skipped.
- Index where `SHA256-Patches:` and `SHA256-Download:` disagree
  on file count (legitimate — Patches lists uncompressed,
  Download lists compressed): treated independently; Phase 6.5
  only consumes Download.

**Architecture filter** (`internal/freshness/arch_filter_test.go`):

- Empty allowlist: every member kept (matches Phase 6 behavior).
- Single-arch allowlist: members for other arches skipped with
  `arch_not_in_allowlist` reason; matched arch's members kept.
- `"source"` in the allowlist: Sources files kept; binary kept
  per arch list.
- Non-arch-segment members (Release, Translation-*, Contents-*):
  always kept regardless of allowlist.

**Validation rules** (`internal/config/architectures_test.go`):

- `architectures = ["AMD64"]` (uppercase): startup fails per H1.
- `architectures = ["x86_64"]` (non-Debian form): startup fails
  per H1 (must start with letter, but `x86_64` does start with
  letter — let me re-check… `x86_64` matches `^[a-z][a-z0-9]*$`?
  Hmm `x` is letter, then `8`, then `6`, then `_`. The `_` is
  not in the regex's character class. So `x86_64` is rejected.
  Good.)
- `architectures = ["amd64", "arm64", ..., 33 entries]`: startup
  fails per H2.

### 12.2 Integration tests

**Multi-arch end-to-end**
(`cmd/apt-cacher-ultra/multiarch_integ_test.go`):

- Synthetic upstream serving a Release with `binary-amd64`,
  `binary-arm64`, `binary-armhf` Packages files.
- Client GETs an arm64 `.deb`: cache miss → fetch upstream →
  cache hit on second GET; hash validated against
  `package_hash` row with `architecture="arm64"`.
- Client GETs an armhf `.deb`: same flow; `architecture="armhf"`.
- Status JSON shows all three arches in `architectures_seen`.

**Architecture filter end-to-end**
(`cmd/apt-cacher-ultra/arch_filter_integ_test.go`):

- Daemon configured `architectures = ["amd64"]`.
- Synthetic upstream with binary-amd64 + binary-arm64.
- Adoption logs show arm64 Packages member skipped with
  `reason=arch_not_in_allowlist`.
- Status JSON's `architectures_seen` contains only `amd64`.
- Client GETs an arm64 `.deb`: trust-upstream serve (no
  validation, no strict-mode).

**Source-package end-to-end**
(`cmd/apt-cacher-ultra/source_pkg_integ_test.go`):

- Synthetic upstream with `main/source/Sources.gz` + corresponding
  `.dsc` + tarballs in `pool/`.
- Client GETs the Sources.gz: served + cached.
- Client GETs the `.dsc`: cache miss → fetch → validated against
  `package_hash` row with `architecture="source"`.
- Tampered `.dsc` (test inverts a byte upstream): cache rejects
  with H8 hash-mismatch path.

**pdiff end-to-end**
(`cmd/apt-cacher-ultra/pdiff_integ_test.go`):

- Synthetic upstream serving `Packages.diff/Index` with two patch
  entries.
- Client GETs the Index: served + cached.
- Client GETs each patch file: cache miss → fetch → validated.
- Tampered patch file: H9 mismatch path.

### 12.3 Chaos tests

- Adoption mid-flight when `[adoption].architectures` changes:
  H12 — restart enforces new filter; old snapshot unchanged.
- Sources file parse failure mid-adoption: per-member skip; other
  members proceed; snapshot adopted with partial source coverage.
- Disk full during adoption with extra source/pdiff rows: same
  Phase 2 transactional guarantee — adoption rolls back; live
  snapshot unchanged.

### 12.4 Production exercise

A one-week production deployment exercises the new path coverage:

- An apt client with a `deb-src` line successfully runs
  `apt-get source` for at least three packages.
- An apt client with `Acquire::PDiffs "true"` successfully
  applies pdiffs from the cache.
- An arm64 client successfully fetches and validates `.deb`
  artifacts.

Recorded as a checklist in the PR or commit message. The
exercise IS NOT gated on having clients of all three classes;
operators with single-arch / no-source / no-pdiff fleets pass
the exercise vacuously for those features (and their cache's
status JSON shows the corresponding zero counts).

---

## 13. Project layout (delta over SPEC §13 / SPEC4 §13 / SPEC5 §13 / SPEC6 §13)

New files:

```
internal/freshness/
  sources_parse.go        # parseSources (Sources file → []package_hash row)
  sources_parse_test.go
  pdiff_parse.go          # parsePdiffIndex (Packages.diff/Index → []package_hash row)
  pdiff_parse_test.go
  arch_filter.go          # architectures-allowlist predicate
  arch_filter_test.go

internal/handler/
  path_class.go           # classifyPath + PathClass enum
  path_class_test.go

cmd/apt-cacher-ultra/
  multiarch_integ_test.go
  arch_filter_integ_test.go
  source_pkg_integ_test.go
  pdiff_integ_test.go
```

Modified files:

- `internal/config/config.go` — `[adoption].architectures` field;
  validation rules per §5.2.
- `internal/freshness/adoption.go` — wire arch filter into
  `runShared`'s member loop; dispatch to `parseSources` /
  `parsePdiffIndex` for matching member paths; emit new metrics.
- `internal/handler/handler.go` — add `path_class` to per-request
  log line; populate `architecture` and `package_name` from
  `package_hash` lookup; emit `serve_hash_validated_total`
  metric per §6.2 / §10.3.
- `internal/admin/template/status.html` — Repository coverage
  section per §2.5.
- `internal/admin/status.go` — populate `repo_coverage` JSON
  block per §2.4; new SQL queries for per-arch / per-kind
  counts (refresher-driven).
- `packaging/config/config.toml.default` — new
  `[adoption].architectures` field per §5.3.

---

## 14. Subcommand surface — unchanged

Phase 6.5 adds no new subcommands. `apt-cacher-ultra remap`,
`apt-cacher-ultra ca print`, `apt-cacher-ultra --print-apt-conf`
all continue to work unchanged. The optional inspection of
`package_hash` row counts is exposed via the §10.3 metrics and
the §2.4 status JSON, not via a new subcommand.

---

## 15. Definition of done

Phase 6.5 is complete when all of the following hold:

1. **`go test -race ./...` passes** with all new tests under
   §12 included.

2. **Multi-arch parity.** A repo publishing `binary-amd64`,
   `binary-arm64`, `binary-armhf` adopts all three. Per-arch
   `.deb` fetches validate against the corresponding
   `package_hash` rows. Status JSON's `architectures_seen`
   reflects the population.

3. **Architecture filter.** Setting
   `[adoption].architectures = ["amd64"]` against a multi-arch
   upstream skips the non-amd64 Packages members with
   `reason=arch_not_in_allowlist` Warn; per-arch counts on the
   status page reflect the filter.

4. **Source-package adoption.** A repo with `main/source/Sources`
   in its Release adopts the file, parses stanzas, and
   populates `package_hash` rows with `architecture="source"`.
   `.dsc` and source-tarball fetches validate against those rows.

5. **pdiff adoption.** A repo publishing
   `Packages.diff/Index` files (one per arch) adopts the
   Index, parses `SHA256-Download:` entries, and populates
   `package_hash` rows for the listed patch files. Per-patch
   fetches validate against those rows.

6. **No schema migration.** Phase 6.5 makes ZERO SQLite schema
   changes. A `cache.dir` opened by a Phase 6.5 daemon is
   interchangeable with one opened by a Phase 6 daemon (forward
   AND backward compatible).

7. **`isDebPath` strict-mode gate preserved.** A `.dsc` or
   pdiff patch request whose `package_hash` row is missing
   serves trust-upstream (Phase 1 fall-through) — strict-mode
   refusal is NOT extended in Phase 6.5. Verified by the
   existing `internal/handler/phase3_strict_test.go` continuing
   to pass.

8. **Hash-validation path is path-agnostic.** AIDEV-VERIFY
   resolved during implementation: the existing serve-time
   validation hook already validates any path with a
   `package_hash` row, regardless of extension. If verification
   surfaces a `.deb`-only gate, this DoD is amended with the
   refactor required to lift the gate.

9. **Status surface live.** `GET /?format=json` includes the
   `repo_coverage` section per §2.4; `GET /` HTML includes the
   "Repository coverage" section per §2.5; `by_architecture`
   sub-objects populate after a multi-arch adoption.

10. **Metrics complete.** `acu_adoption_members_skipped_total`
    (with new `reason="arch_not_in_allowlist"`),
    `acu_adoption_sources_parsed_total`,
    `acu_adoption_pdiff_indexes_parsed_total`,
    `acu_package_hash_rows_by_kind`, and
    `acu_serve_hash_validated_total` all expose at `/metrics`.
    Cardinality stays under `metric_series_cap = 1024`.

11. **Failure modes pinned.** Every H1–H15 case (§11) has at
    least one regression test or a clear no-op-by-design
    annotation in the test suite.

12. **Documentation.** SPEC6_5.md (this document) is locked.
    `packaging/config/config.toml.default` includes the §5.3
    additions with operator-guidance comments.

13. **Live exercise.** On the test environment the operator
    runs:
    a. An apt-get source against a real source repo (e.g.
       `apt-get source bash` against debian).
    b. An apt update with `Acquire::PDiffs "true"` configured
       against a stable suite.
    c. An apt-get install against an arm64 chroot (or equivalent
       arm64 client).
    Each is recorded with timing and observable metric impact.

14. **Graceful shutdown unaffected.** SIGTERM during an
    adoption with Sources / pdiff parsing in flight respects
    the existing Phase 2 ctx-cancel contract; no leaked
    goroutines per goleak.

15. **No regression in Phase 1–6 surface.** All Phase 1–6 tests
    pass under the Phase 6.5 build. The Phase 3 strict-mode
    `.deb`-only gate (`phase3_strict_test.go`) is unchanged.

16. **One-week production soak.** Stable
    `acu_request_total{outcome=…}` rates throughout.
    `acu_serve_hash_validated_total{outcome="mismatch"}` stays
    at zero (any mismatch is operator-investigated; a single
    real mismatch is acceptable as a discovered upstream
    irregularity, but a sustained mismatch rate signals a
    daemon bug).

17. **Path-class log field stable.** The new `path_class` field
    in the per-request log line is enumerated in
    `docs/log-fields.md` (a new file added in this phase) so
    log consumers know the closed value set.

18. **Multi-arch adoption time bounded.** A real-world
    multi-arch debian-main adoption (amd64 + arm64 + armhf +
    i386 + source) completes within `2× the Phase 6 single-arch
    adoption baseline`. Captured during the live exercise; if
    the actual ratio exceeds this, the adoption budget per §9.1
    is documented and the §9 disposition revisited.
