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

The flat-repository work (spec C5/C7) adds the adoption outcomes `refused`
(a refusal state; the `reason` says which), `release_date_in_future` and
`timed_out` (`adoption.max_duration` reached), the gauge
`acu_adoption_failing_suites` (suites whose latest adoption failed, refusals
excluded) and the gauge `acu_freshness_waiting_for_slot` (suites waiting for
an adoption slot).

The `freshness_check` line's `result` gains `changed_deferred` (upstream
still serves content whose adoption failed or was refused; the field
`reason` says why it is held back) and `changed_queued` (no adoption slot
was free; the suite waits for one, and the scheduler re-checks it as soon as
one is released, see *Capacity and fairness* in configuration.md).

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
| `adoption_panicked` | ERROR | `where` (`adoption`, `repair`, `reconcile`, `heartbeat`, `record`), `canonical_host`, `suite_path`, `panic`, `stack` (first occurrence per `where` per process only; never for `where=heartbeat`) | A panic inside an adoption, repair, reconcile, heartbeat or failure-record goroutine was recovered; the prior snapshot keeps serving. `where=record` is `recordAdoptionFailure`, which writes the spec C7 failure record after an adoption, repair or reconcile ends. |
| `freshness_check_panicked` | ERROR | `where` (`check`, `scheduler`, `reschedule`), `canonical_host`, `suite_path`, `panic`, `stack` (first occurrence per `where` per process only) | A panic during a freshness check was recovered. For `where=check` the check counts as failed (`last_check_at` bumped, so the cooldown applies) and also counts as an unknown end: `last_check_at` is bumped and `check_fail_count` extended. The `scheduler` and `reschedule` recovers only log; `where=reschedule` guards the start-time retry spread (`rescheduleFailingAdoptions`). |
| `freshness check failed` | INFO | `canonical_host`, `suite_path`, `form`, `err`, `result` (always `failed`) | A check ended unknown (transport error, the 2-minute check deadline at the host slot or the request, or an unexpected status); `last_check_at` is bumped and `check_fail_count` extended, so the scheduler backs off (2 min doubling to 1 h). |
| `adoption: read prior snapshot failed` | WARN | `canonical_host`, `suite_path`, `err` | The adoption could not read the suite's current (prior) snapshot before running Date and rollback checks against it; the attempt fails as `db_failed` and is retried on the normal backoff schedule rather than skipping those checks. |
| `adoption_failure_record_failed` | WARN; DEBUG only for the read that precedes the write, when it finds the suite already removed (a routine reap during adoption, not a fault) — the write-failure site below has no DEBUG case | `canonical_host`, `suite_path`, `err` | The failure/refusal record could not be read or written; the next check may retry sooner than scheduled. |
| `adoption_refused` | INFO | `canonical_host`, `suite_path`, `reason`, `refusal`, `fail_count`, `retry_in_seconds`, `next_retry_unixtime` | One attempt ended in a refusal state (`upstream_older`, `rollback_refused`), not a failure; logged for every refused attempt. |
| `adoption_deferred` | INFO | as `adoption_refused` | A suite entered the failing or refused state, or its reason changed (not logged on every attempt); the same content is retried at `next_retry_unixtime`, while new content is tried at once. |
| `adoption_failing_persistently` | WARN | `canonical_host`, `suite_path`, `reason`, `fail_count`, `failing_since_unixtime` | The suite's adoption has failed 3, 6, 12, 24, … times in a row. Refusals never reach this (spec C7). |
| `adoption_retries_rescheduled` | INFO | `suites`, `spread_seconds` | At start, failing suites were given one retry spread over the refresh interval. |
| `adoption_retries_reschedule_failed` | WARN | `err` | The start-time retry spread could not be written; failing suites keep their schedule. |
| `adoption_repair_list_failed` | WARN | `canonical_host`, `suite_path`, `snapshot_id`, `err` | The repair decision could not list the snapshot's repairable skipped members; no repair runs this check, the next unchanged check asks again. |
| `freshness: clamp adoption retry failed` | WARN | `canonical_host`, `suite_path`, `err` | A retry time left in the future by a backward clock step could not be clamped; the next check tries again. |
| `freshness: clear adoption failure failed` | WARN | `canonical_host`, `suite_path`, `err` | Upstream serves the current content again but the failure record could not be cleared; the next check tries again. |
| `freshness: read suite row after a panic failed` | WARN | `err`, `canonical_host`, `suite_path` | After a recovered check panic, the best-effort bump of `last_check_at`/`check_fail_count` could not even read the suite's row; the row is left as it was. |
| `freshness: persist failure bump after a panic failed` | WARN | `err`, `canonical_host`, `suite_path` | After a recovered check panic, `last_check_at` and `check_fail_count` were bumped in memory but the write failed; the row is left as it was. |
| `clock_step_detected` | WARN | `stamp`, `canonical_scheme`, `canonical_host`, `suite_path`, `stored_unixtime`, `now_unixtime` | A stored timestamp is more than 2 s in the future (the wall clock stepped backwards); the gate treated it as old. Rate-limited to one line per minute. For `stamp=adoption_next_retry_at` the stamp is not treated as old: it is clamped to now + 1 h. `acu_clock_steps_detected_total` counts detections — one per stored timestamp found in the future — not distinct clock steps. |
| `legacy_snapshot_quarantined` | WARN | `reason` (`legacy_plaintext`, `malformed_envelope`, `unsigned_declarations`, `unparseable`, `anchor_missing`, `anchor_oversized`, `anchor_corrupt`, `unexpected_form`), `snapshot_id`, `canonical_host`, `suite_path`, `anchor_form`, `was_current` | The startup trust audit could not prove a pre-upgrade snapshot signed and clean; it no longer serves or vouches, and its suite is served unadopted until it re-adopts. |
| `legacy_trust_audit_deferred` | ERROR | `snapshot_id`, `canonical_host`, `suite_path`, `err`, `stack` (present only for a recovered panic) | A pre-upgrade snapshot's pool file could not be read (not "missing"), or auditing it panicked (`err` carries the panic value); the snapshot is left as it was and retried at the next start. |
| `legacy_trust_audit_progress` | INFO | `audited`, `quarantined`, `deferred` | One batch of up to 100 queued snapshots was audited and committed; the fields are running totals across this start's batches. Logged on the first start after an upgrade, and on later starts while deferred snapshots remain queued or after an interrupted audit. |
| `legacy_trust_audit_completed` | INFO | `audited`, `signed`, `quarantined`, `deferred`, `anchor_markers`, `duration` | The startup trust audit finished (every start; `audited` is 0 once the upgrade queue has drained). |
