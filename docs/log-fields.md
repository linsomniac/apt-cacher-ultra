# Log field reference

Closed-value sets and presence semantics for the structured fields the
daemon emits on each per-request `request` log line. Use this as the
schema reference when building dashboards, alerts, or log-ingest
pipelines.

Source of truth: `internal/handler/handler.go` (`logRequestWithValidation`).
This file documents the locked surface — adding values requires a spec
amendment.

## Field-presence-as-signal pattern

Some fields are emitted **only when meaningful**. Absence is a signal,
not an error. This keeps log lines compact and lets operators grep
positively for the case they care about rather than filtering out
zero/false noise.

Fields that follow this pattern:

- `validated_hash` — emitted as `true` only when the served bytes
  matched a `package_hash` row. Never logged as `false`.
- `package_name` — emitted only when non-empty.
- `architecture` — emitted only when non-empty.
- `mitm` — emitted as `true` only when the request flowed through a
  CONNECT/MITM tunnel (SPEC6 §6.2.1).
- `upstream_status` — emitted only when a fetch was attempted; value 0
  means "fetch attempted, no response arrived" (timeout, dial denied).

Always-present fields (`method`, `url`, `canonical_host`, `path`,
`outcome`, `status`, `bytes_sent`, `duration_ms`, `client_addr`) are
documented in SPEC §10.

## `path_class` (SPEC6_5 §2.3)

Closed enum, 8 values. Emitted on every `request` log line where
`path != ""` (i.e. once URL parsing has succeeded). Drives the
`acu_serve_hash_validated_total{path_class=...}` Prometheus label
(SPEC6_5 §10.3).

| Value | Match rule | Example path |
|---|---|---|
| `binary_deb` | suffix `.deb` | `/ubuntu/pool/main/b/bash/bash_5.1_amd64.deb` |
| `binary_udeb` | suffix `.udeb` | `/debian/pool/main/d/d-i/foo_1.0_amd64.udeb` |
| `source_dsc` | suffix `.dsc` | `/debian/pool/main/b/bash/bash_5.1-2.dsc` |
| `source_tarball` | suffix `.tar.gz` / `.tar.xz` / `.tar.bz2` / `.diff.gz` | `/debian/pool/main/b/bash/bash_5.1.orig.tar.xz` |
| `pdiff_index` | suffix `/Packages.diff/Index` or `/Sources.diff/Index` | `/debian/dists/bookworm/main/binary-amd64/Packages.diff/Index` |
| `pdiff_patch` | path matches `(?:Packages\|Sources)\.diff/[0-9.-]+\.gz$` | `/debian/dists/bookworm/main/binary-amd64/Packages.diff/2026-05-09-1234.56.gz` |
| `metadata` | basename is one of `Release`, `Release.gpg`, `InRelease`, `Packages(.gz/xz/bz2)`, `Sources(.gz/xz/bz2)`, `Contents-<arch>(.gz/xz/bz2)`, `Translation-<locale>(.gz/xz/bz2)` | `/debian/dists/bookworm/InRelease` |
| `other` | nothing above matched | (e.g. an unfamiliar repository file) |

Classification is a pure-text predicate — no DB lookup, no allocation
beyond the regex match. Order matters in the implementation:
extension-suffix checks run before regex matches for the metadata and
pdiff-patch shapes.

## `validated_hash` (SPEC6_5 §2.3)

Boolean. Present as `true` iff the served bytes were SHA-256-matched
against a `package_hash` row's declared digest in this request. Absent
otherwise — including the trust-upstream path where no `package_hash`
row existed for the request's canonical (scheme, host, path).

Pairs with `acu_serve_hash_validated_total{outcome=match|mismatch}`
(SPEC6_5 §10.3): every Info log line carrying `validated_hash=true`
corresponds to a `outcome=match` counter increment; mismatch sites
emit a `serve_hash_mismatch` Warn and increment `outcome=mismatch`
without setting `validated_hash` on the per-request log.

## `architecture` (SPEC6_5 §2.3)

String. Present when extractable from the path:

- `/binary-<arch>/` segment in the path → captures `<arch>` (e.g.
  `amd64`, `arm64`, `armhf`, `i386`, `ppc64el`).
- `path_class` is `source_dsc` or `source_tarball` → literal `source`.
- Path contains `/source/` → literal `source`.
- Otherwise absent.

Drives no Prometheus label by design — operators correlate `path_class`
+ `validated_hash` against per-arch population through the
`acu_package_hash_rows_by_kind{kind}` gauge and the `repo_coverage`
status surface (SPEC6_5 §2.4).

## `package_name` (SPEC6_5 §2.3)

String. Present when:

1. `validated_hash` is `true` for this request, AND
2. The matching `package_hash` row carries a non-empty `package_name`
   (Phase 3 v3 schema — empty for pdiff patches and legacy pre-v3
   rows).

Absent otherwise. Best-effort: a `.dsc` validated against a Sources
stanza carries its `Package:` field; a pdiff patch validates against
a row with empty `package_name` and so omits the field.

## Outcome values

The `outcome` field's closed value set is locked by SPEC §10 and not
re-enumerated here. The values are listed at the SPEC anchor.

## Stability guarantees

- The `path_class` enum is closed. Adding a value requires a spec
  amendment (SPEC6_5 §2.3).
- Field names never change without a spec amendment. Field-presence
  semantics (when a field appears vs is omitted) are also locked.
- New fields may be added additively — log consumers parse the keys
  they care about and ignore the rest.
