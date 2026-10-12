# Log field reference

Closed-value sets and presence semantics for the structured fields the
daemon emits on each per-request `request` log line, and for the
operational events listed under [Operational events](#operational-events).
Use this as the schema reference when building dashboards, alerts, or
log-ingest pipelines.

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

**Security note**: the `url` field is sanitized through `urlForLog`
before emission — proxy-form requests' userinfo (any
`http://user:pass@host/...` syntax) is stripped, so a malformed or
malicious proxy request cannot smuggle credentials into the log
stream.

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
| `pdiff_patch` | regex below | `/debian/dists/bookworm/main/binary-amd64/Packages.diff/2026-05-09-1234.56.gz` |
| `metadata` | regex below | `/debian/dists/bookworm/InRelease` |
| `other` | nothing above matched | (e.g. an unfamiliar repository file) |

Exact regexes (`internal/handler/handler.go`):

```
pdiff_patch:  (?:Packages|Sources)\.diff/[0-9.-]+\.gz$
metadata:     (?:^|/)(?:
                Release(?:\.gpg)?|InRelease|
                Packages(?:\.(?:gz|xz|bz2))?|
                Sources(?:\.(?:gz|xz|bz2))?|
                (?:Packages|Sources)\.diff/Index|
                Contents-[a-z0-9]+(?:\.(?:gz|xz|bz2))?|
                Translation-[A-Za-z0-9_]+(?:\.(?:gz|xz|bz2))?
              )$
```

A path with an uppercase or hyphenated `Contents-<x>` segment (e.g.
`Contents-EXTRA`, `Contents-non-free`) does NOT match the metadata
regex and is classified as `other`. Same for any `Translation-*`
filename that contains a character outside `[A-Za-z0-9_]`.

Classification is a pure-text predicate — no DB lookup, no allocation
beyond the regex match. Order matters in the implementation:
extension-suffix checks run before regex matches for the metadata and
pdiff-patch shapes.

## `validated_hash` (SPEC6_5 §2.3)

Boolean. Present as `true` when the served bytes correspond to a
successful SHA-256 match against a `package_hash` row's declared
digest. Absent otherwise — including the trust-upstream path where no
`package_hash` row existed for the request's canonical (scheme, host,
path).

Pairs with `acu_serve_hash_validated_total{outcome=match|mismatch}`
(SPEC6_5 §10.3). One **validation event** increments the counter; one
or more **request log lines** may then carry `validated_hash=true` for
that event:

- A direct hit-path validation increments once and logs once.
- A miss-path fetch + validation increments once (at the leader) and
  may log multiple times: the leader's log carries `validated_hash`,
  AND every singleflight waiter that joined on the same key receives
  the same validated result and also logs `validated_hash=true` —
  including `outcome=hit_coalesced` lines (`internal/handler/handler.go`
  serveCacheMiss / singleflight waiter path).

Operators alarming on validation rate should read the counter, not
count log lines. Mismatch sites emit a `serve_hash_mismatch` Warn and
increment `outcome=mismatch`; the corresponding request log line does
NOT carry `validated_hash` (mismatch is a failure, not a successful
validation).

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

## Operational events

Structured events emitted outside the per-request `request` log line:
goroutine panic recovery, wall-clock step detection, and the startup
legacy trust audit (logged by `internal/freshness` and `internal/handler`).

| Event | Level | Fields | Meaning |
|---|---|---|---|
| `adoption_panicked` | ERROR | `where` (`adoption`, `repair`, `reconcile`, `heartbeat`), `canonical_host`, `suite_path`, `panic`, `stack` (first occurrence per `where` per process only; never for `where=heartbeat`) | A panic inside an adoption, repair, reconcile or heartbeat goroutine was recovered; the prior snapshot keeps serving. |
| `freshness_check_panicked` | ERROR | `where` (`check`, `scheduler`, `t1`), `canonical_host`, `suite_path`, `panic`, `stack` (first occurrence per `where` per process only) | A panic during a freshness check was recovered. For `where=check` the check counts as failed (`last_check_at` bumped, so the cooldown applies); the `scheduler` and `t1` recovers only log. |
| `clock_step_detected` | WARN | `stamp`, `canonical_scheme`, `canonical_host`, `suite_path`, `stored_unixtime`, `now_unixtime` | A stored timestamp is more than 2 s in the future (the wall clock stepped backwards); the gate treated it as old. Rate-limited to one line per minute. |
| `legacy_snapshot_quarantined` | WARN | `reason` (`legacy_plaintext`, `malformed_envelope`, `unsigned_declarations`, `unparseable`, `anchor_missing`, `anchor_oversized`, `anchor_corrupt`, `unexpected_form`), `snapshot_id`, `canonical_host`, `suite_path`, `anchor_form`, `was_current` | The startup trust audit could not prove a pre-upgrade snapshot signed and clean; it no longer serves or vouches, and its suite is served unadopted until it re-adopts. |
| `legacy_trust_audit_deferred` | ERROR | `snapshot_id`, `canonical_host`, `suite_path`, `err`, `stack` (present only for a recovered panic) | A pre-upgrade snapshot's pool file could not be read (not "missing"), or auditing it panicked (`err` carries the panic value); the snapshot is left as it was and retried at the next start. |
| `legacy_trust_audit_progress` | INFO | `audited`, `quarantined`, `deferred` | One batch of up to 100 queued snapshots was audited and committed; the fields are running totals across this start's batches. Logged on the first start after an upgrade, and on later starts while deferred snapshots remain queued or after an interrupted audit. |
| `legacy_trust_audit_completed` | INFO | `audited`, `signed`, `quarantined`, `deferred`, `anchor_markers`, `duration` | The startup trust audit finished (every start; `audited` is 0 once the upgrade queue has drained). |
