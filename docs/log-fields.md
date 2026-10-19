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
- `snapshot_displaced` — emitted as `true` only when a by-hash request
  was served from one of its suite's displaced snapshots (spec C8
  retained by-hash, for a client holding an earlier `InRelease`);
  `X-Cache-Snapshot` names that snapshot.
- `flip_held_ms` — emitted only when an `InRelease`/`Release` request
  waited in a flip hold (spec C8): how long it waited, in milliseconds
  (at most 20000; part of `duration_ms`). A request waits when it
  arrives, or right after it created its suite's row. Also on the
  `client_canceled` line of a held request whose client left while it
  waited.

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

### Adoption outcomes (not the request line's `outcome`)

The flat-repository work (spec C5/C7) adds values to the **adoption**
outcome — the metric label `acu_adoption_total{outcome}` (and
`acu_adoption_duration_seconds{outcome}`) and the admin JSON field
`recent_adoptions[].outcome` — never to the `request` line's `outcome`
above: `refused` (a refusal state; the `reason` says which),
`release_date_in_future`, `timed_out` (`adoption.max_duration` reached
before the downloads finished, or the commit hit its 5-minute bound; a
deadline that passes while the adoption waits in the flip barrier is not
`timed_out`) and `panicked` (the adoption goroutine panicked; recorded with
reason `panic`). An adoption ended by shutdown, in the flip barrier or
before it, is not an outcome: it is logged as
`adoption_abandoned_at_shutdown` and counted nowhere.
It also adds the gauge `acu_adoption_failing_suites` (suites whose latest
adoption failed, refusals excluded; 0 while adoption is disabled) and the
gauge `acu_freshness_waiting_for_slot` (suites waiting for an adoption
slot). The flip barrier (spec C8) adds one unlabelled counter per
residual — `acu_flip_hold_skipped_total` (a flip that waited 60 s for the
hold token and committed without a hold),
`acu_flip_quiet_cut_short_total` (a hold that committed 15 s in, before
its suite was quiet: an `InRelease`/`Release` response still being sent
then, or finished within `flip_quiet_period`, may straddle; the held
requests got the new snapshot), `acu_flip_hold_expired_total` (a hold
that reached its 20 s bound before its commit finished: its held requests
were answered from the old snapshot),
`acu_flip_waiters_over_cap_total` (an anchor request answered at once
because 256 already waited in a hold) and
`acu_flip_slow_anchor_reads_total` (an `InRelease`/`Release` response
that took 30 s or longer to send) — `acu_flip_gate_panics_total` (a
panic recovered in the barrier's hold-expiry timer: a bug, please report
it) and the unlabelled gauge `acu_flip_barrier_active` (flips in the
barrier now: waiting for quiet or for the hold token, holding, or
committing). Retained by-hash adds `acu_serve_byhash_displaced_total{host}`
(by-hash requests served from a displaced snapshot). Suite rows (spec C3)
add `acu_suite_seed_rejected_total{host}` (refusals at the
`freshness.max_suites` caps that reached the database: at most one per
suite per 10 minutes, unless the rejection cache, which holds 4,096 suites,
had to clear itself) and the unlabelled gauge `acu_suites_seed_rejected`
(suites refused within the last 10 minutes, updated within a minute). The
scheduler adds the unlabelled counter `acu_freshness_unlogged_panics_total`
(scheduled checks whose panic could not be logged because reporting it
panicked too: a bug, please report it).
`acu_adoption_duration_seconds` includes the barrier's wait (at most
140 s).

The `freshness_check` line's `result` gains `changed_deferred` (upstream
still serves content whose adoption failed or was refused; the field
`reason` says why it is held back) and `changed_queued` (no adoption slot
was free; the suite waits for one and is re-checked as soon as one is
released; a client-refused change is picked up by the next tick, see
*Capacity and fairness* in configuration.md).

## Stability guarantees

- The `path_class` enum is closed. Adding a value requires a spec
  amendment (SPEC6_5 §2.3).
- Field names never change without a spec amendment. Field-presence
  semantics (when a field appears vs is omitted) are also locked.
- New fields may be added additively — log consumers parse the keys
  they care about and ignore the rest.

## Operational events

Structured events emitted outside the per-request `request` log line:
goroutine panic recovery, freshness checks that end unknown, adoption
failure and refusal records and their retry schedule, wall-clock step
detection, the startup legacy trust audit, the flip barrier, suite rows
(cap refusals and evictions) and a slow shutdown drain (logged by
`internal/freshness`, `internal/handler` and the daemon's `main`).

| Event | Level | Fields | Meaning |
|---|---|---|---|
| `adoption_panicked` | ERROR | `where` (`adoption`, `repair`, `reconcile`, `heartbeat`, `record`), `canonical_host`, `suite_path`, `panic`, `stack` (first occurrence per `where` per process only; never for `where=heartbeat`) | A panic inside an adoption, repair, reconcile, heartbeat or failure-record goroutine was recovered; the prior snapshot keeps serving. Only an adoption panic is recorded as a failure (reason `panic`); repair and reconcile panics are logged, not recorded (plan deviation 7). `where=record` is `recordAdoptionFailure`, which writes the spec C7 failure record after an adoption ends. |
| `freshness_check_panicked` | ERROR | `where` (`check`, `scheduler`, `reschedule`, `t1`), `canonical_host`, `suite_path`, `panic`, `stack` (first occurrence per `where` per process only) | A panic during a freshness check was recovered. For `where=check` the check counts as failed (`last_check_at` bumped, so the cooldown applies) and also counts as an unknown end: `last_check_at` is bumped and `check_fail_count` extended. The `scheduler`, `reschedule` and `t1` recovers only log; `where=reschedule` guards the start-time retry spread (`rescheduleFailingAdoptions`), and `where=t1` is the handler's client-triggered check goroutine (logged by `internal/handler`). |
| `freshness check failed` | INFO | `canonical_host`, `suite_path`, `form`, `err`, `result` (always `failed`) | A check ended unknown (a transport error or error status from upstream, or the 2-minute check deadline at the host slot or the request); `last_check_at` is bumped and `check_fail_count` extended, so the scheduler backs off (2 min doubling to 1 h), and `acu_freshness_check_total{result="failed"}` is incremented. |
| `freshness: unexpected success status` | ERROR | `status`, `canonical_host`, `suite_path`, `form` (detached checks only) | The fetch client returned a success status other than 200 or 304 (a contract violation). The suite row is bookkept like an unknown end (`last_check_at` bumped, `check_fail_count` extended, `last_success_at` kept), but no `freshness check failed` line is logged and `acu_freshness_check_total` is not incremented. |
| `adoption_run_failed` | WARN; INFO when the attempt was refused (outcome `refused`) | `canonical_host`, `suite_path`, `form`, `err` | Backstop line for every adoption attempt that returned an error, so a failure with no categorised line of its own is still visible. A refusal is a state, not a failure, so its line is INFO (`adoption_refused` carries the reason and timing); operators alerting on WARN are not paged for a flapping mirror. |
| `adoption: read prior snapshot failed` | WARN | `canonical_host`, `suite_path`, `err` | The adoption could not read the suite's current (prior) snapshot before running Date and rollback checks against it; the attempt fails as `db_failed` and is retried on the normal backoff schedule rather than skipping those checks. |
| `adoption_failure_record_failed` | WARN; DEBUG only for the read that precedes the write, when it finds the suite already removed (a routine reap during adoption, not a fault) — the write-failure site below has no DEBUG case | `canonical_host`, `suite_path`, `err` | The failure/refusal record could not be read or written; the next check may retry sooner than scheduled. |
| `adoption_refused` | INFO | `canonical_host`, `suite_path`, `reason`, `refusal`, `fail_count`, `retry_in_seconds`, `next_retry_unixtime` | One attempt ended in a refusal state (`upstream_older`, `rollback_refused`), not a failure; logged for every refused attempt. |
| `adoption_deferred` | INFO | as `adoption_refused` | A suite entered the failing or refused state, or its reason changed (not logged on every attempt); the same content is retried at `next_retry_unixtime`, while new content is tried at once. |
| `adoption_failing_persistently` | WARN | `canonical_host`, `suite_path`, `reason`, `fail_count`, `failing_since_unixtime` | The suite's adoption has failed 3, 6, 12, 24, … times in a row. Refusals never reach this (spec C7); see `adoption_refused_persistently`. |
| `adoption_refused_persistently` | WARN | as `adoption_failing_persistently` (`reason` is the refusal reason; `failing_since_unixtime` is when the refusal streak began) | A refusal streak reached 3, 6, 12, 24, … attempts and began at least 24 h ago. A refusal is a state, not a failure, but one that lasts a day may be a silent freeze — a vendor that stops emitting `Date` is refused `upstream_older` forever — so it is made alertable here and on the admin page (`REFUSED 24H+`). A streak start in the future (clock step) restarts the streak, so it is never "old". |
| `adoption_retries_rescheduled` | INFO | `suites`, `spread_seconds` | At start, failing suites were given one retry spread over the refresh interval. Refusals and panicked adoptions keep their schedule. |
| `adoption_retries_reschedule_failed` | WARN | `err` | The start-time retry spread could not be written; failing suites keep their schedule. |
| `adoption_repair_list_failed` | WARN | `canonical_host`, `suite_path`, `snapshot_id`, `err` | The repair decision could not list the snapshot's repairable skipped members; no repair runs this check, the next unchanged check asks again. |
| `freshness: clamp adoption retry failed` | WARN | `canonical_host`, `suite_path`, `err` | A retry time left in the future by a backward clock step could not be clamped; the next check tries again. |
| `freshness: clear adoption failure failed` | WARN | `canonical_host`, `suite_path`, `err` | Upstream serves the current content again but the failure record could not be cleared; the next check tries again. |
| `freshness: read suite row after a panic failed` | WARN | `err`, `canonical_host`, `suite_path` | After a recovered check panic, the best-effort bump of `last_check_at`/`check_fail_count` could not even read the suite's row; the row is left as it was. |
| `freshness: persist failure bump after a panic failed` | WARN | `err`, `canonical_host`, `suite_path` | After a recovered check panic, `last_check_at` and `check_fail_count` were bumped in memory but the write failed; the row is left as it was. |
| `clock_step_detected` | WARN | `stamp`, `canonical_scheme`, `canonical_host`, `suite_path`, `stored_unixtime`, `now_unixtime` | A stored timestamp is more than 2 s in the future (the wall clock stepped backwards); the gate treated it as old. Rate-limited to one line per minute. For `stamp=adoption_next_retry_at` the stamp is not treated as old: it is clamped to now + 1 h. For `stamp=last_requested_at` (a suite's client-interest stamp, read when the suite cap looks for an idle suite to evict) the suite is not evicted: a future stamp never counts as idle. `acu_clock_steps_detected_total` counts detections, not distinct clock steps or distinct stamps: the same future stamp is detected again every time it is read — every scheduler tick for a future `last_success_at`, every check for a future `last_check_at`, and twice when a first-ever check falls back from inline to detached — so alert on its rate, not its value. |
| `legacy_snapshot_quarantined` | WARN | `reason` (`legacy_plaintext`, `malformed_envelope`, `unsigned_declarations`, `unparseable`, `anchor_missing`, `anchor_oversized`, `anchor_corrupt`, `unexpected_form`), `snapshot_id`, `canonical_host`, `suite_path`, `anchor_form`, `was_current` | The startup trust audit could not prove a pre-upgrade snapshot signed and clean; it no longer serves or vouches, and its suite is served unadopted until it re-adopts. |
| `legacy_trust_audit_deferred` | ERROR | `snapshot_id`, `canonical_host`, `suite_path`, `err`, `stack` (present only for a recovered panic) | A pre-upgrade snapshot's pool file could not be read (not "missing"), or auditing it panicked (`err` carries the panic value); the snapshot is left as it was and retried at the next start. |
| `legacy_trust_audit_progress` | INFO | `audited`, `quarantined`, `deferred` | One batch of up to 100 queued snapshots was audited and committed; the fields are running totals across this start's batches. Logged on the first start after an upgrade, and on later starts while deferred snapshots remain queued or after an interrupted audit. |
| `legacy_trust_audit_completed` | INFO | `audited`, `signed`, `quarantined`, `deferred`, `anchor_markers`, `duration` | The startup trust audit finished (every start; `audited` is 0 once the upgrade queue has drained). |
| `adoption_flip_barrier` | INFO | `canonical_host`, `suite_path`, `snapshot_id`, `outcome` (`held`, `quiet_cut_short`, `hold_expired`, `hold_skipped`, `not_needed`, `commit_failed`), `phase_a_seconds`, `token_wait_seconds`, `hold_seconds`, `held_requests` (the last four absent when the barrier was skipped for a first adoption) | An adoption committed behind the spec C8 flip barrier: `held` within its hold, once its suite was quiet; `quiet_cut_short` within its hold at 15 s, before its suite was quiet — the held requests got the new snapshot, a client still reading the old anchor then (or done with it within `flip_quiet_period`) may straddle (`acu_flip_quiet_cut_short_total`); `hold_expired` when the hold reached 20 s before the commit finished — the held requests were answered from the old snapshot (`acu_flip_hold_expired_total`); `hold_skipped` after 60 s without the hold token (`acu_flip_hold_skipped_total`); `not_needed` for a first adoption whose anchor clients were already being served; `commit_failed` when the commit ran and failed — the snapshot did not switch, the timing fields still describe the barrier, and `adoption_run_failed` gives the error. Not logged when `adoption.flip_quiet_period = 0s`, nor for a flip abandoned at shutdown (`adoption_abandoned_at_shutdown` says so). |
| `adoption_abandoned_at_shutdown` | INFO | `canonical_host`, `suite_path`, `form`, `err` | An adoption was abandoned at shutdown — waiting in the flip barrier (or about to skip it) when the daemon closed the barrier at the signal, or in any step before its commit once the daemon's lifetime context ended: nothing was committed or recorded, the change is adopted after the restart. Not a failure: no `adoption_run_failed`, not counted in `acu_adoption_total` or `acu_adoption_duration_seconds`, not shown in the admin page's recent adoptions. |
| `displaced snapshot lookup failed` | WARN | `err`, `canonical_host`, `path` | Retained by-hash could not read the suite's displaced snapshots; the request is answered 404, as before spec C8. |
| `snapshot adopted_at lookup failed` | WARN | `err`, `snapshot_id` | A snapshot `InRelease`/`Release`/`Release.gpg` could not read its snapshot's `adopted_at` (spec C8 `Last-Modified`), so this response carries the pool file's mtime instead and its `Last-Modified` may not grow with the last flip. Not logged when the request had already ended. |
| `shutdown_waiting_for_adoptions` | INFO | `waited`, `detail` | Shutdown has waited 2 s for the adoption, repair and reconcile goroutines: a commit already running is never cancelled and may take up to its 5-minute bound; other work stops at its next cancellation point (adoptions in the flip barrier were abandoned at the signal). Logged once per shutdown. |
| `suite_seed_rejected` | WARN | `canonical_host`, `suite_path`, `cap` (`global_cap`, `host_cap`), `max_suites`, `suppressed`, `detail` | A suite was refused a row by `freshness.max_suites`: it is served but will not refresh; retried after 10 minutes. At most one line a minute; `suppressed` counts the refusals since the last line. |
| `suite_evicted` | INFO | `canonical_host`, `suite_path` (the evicted suite), `for_host`, `for_suite` | At or above the global cap, a suite with no adopted snapshot that had not been requested for 24 hours was evicted for a new one. Above the cap (after `freshness.max_suites` was lowered, or after an upgrade) the new suite is still refused (`suite_seed_rejected`) until the table is back under the cap. |
| `suite_caps_exceeded` | WARN | `suites`, `max_suites`, `hosts_at_cap`, `per_host_cap`, `fullest_host`, `fullest_host_suites` | At start, the suite table holds more than `freshness.max_suites` suites (a lowered `max_suites`, or an upgraded cache), or a host holds a tenth of it or more: new suites — for a host at its cap, that host's — are refused. Above the total cap each refused attempt still evicts one suite idle for 24 hours, so the table shrinks back; a host at its cap evicts nothing, and its suites go only when idle-suite reaping (a later release) removes them. Not logged for a table at exactly the total cap: that is normal operation for a full cache, where a new suite evicts an idle one (no adopted snapshot, not requested for 24 hours) and is refused only when none qualifies (`suite_seed_rejected`). Logged once per start. |
| `suite cap usage read failed` | WARN | `err` | The startup check behind `suite_caps_exceeded` could not count the suites; the caps apply either way. |
| `handler_goroutine_panicked` | ERROR | `where` (`touch`, `seed_rejections`), `canonical_host` and `path` (`touch` only), `panic`, `stack` (first occurrence per `where` per process only) | A panic in one of the handler's background goroutines — the touch goroutine, or the cap-rejection cache's sweep timer — was recovered (a bug, please report it). |
| `suite onboarding failed` | WARN | `err`, `canonical_host`, `suite_path` | A cache hit on an untracked suite's anchor could not create its row (including the 5 s bound on that write); the anchor is served and the next hit retries. |
| `suite signed-seen mark failed` | WARN | `err`, `canonical_host`, `suite_path` | A suite's row was created, and one of its cached signed anchors changed between the handler's read and the creating write (or the handler could not read it: `suite cached anchor read failed`), so it was marked afterwards — and that write failed, or ran out of its 5 s bound. The startup pass marks it at the next start. (Every other mark is part of the write that creates the row or stores the anchor.) |
| `suite file head read failed` | WARN | `err`, `canonical_host`, `suite_path`, `path` | A suite file just fetched on a miss could not be read back from the cache's pool for its content check (a local disk fault). For a signed anchor (`InRelease`, `Release.gpg`) the miss fails — 502, no body, nothing stored — because the anchor must not be served before its suite is marked as signed; apt retries. Any other suite file (`Release`, a flat index) is stored and served, but creates no suite row and re-stamps none. |
| `suite cached anchor read failed` | WARN | `err`, `canonical_host`, `suite_path`, `path` | Creating a suite's row, the handler could not read one of the suite's cached signed anchors (its `url_path` row or its pool file; an absent row is not logged). The row is created anyway, and the anchor is read again and marked right after the write, before the response (`suite signed-seen backstop read failed` if that fails too). |
| `suite signed-seen backstop read failed` | WARN | `err`, `canonical_host`, `suite_path`, `path` | After creating a suite's row, the handler's second read of a cached signed anchor (see `suite cached anchor read failed`; also run when the anchor changed during the creation) failed — its `url_path` row or its pool file, including the 5 s bound on that step; an absent row is not logged. The response is served; the anchor stays unmarked until the startup pass marks it at the next start. |
| `snapshot_member_blob_missing` | WARN | `canonical_host`, `path`, `snapshot_id`, `blob_hash`, `snapshot_displaced` (present for a displaced snapshot's member) | A snapshot member's pool file is gone: a current member is re-fetched and checked against its declared hash; a displaced one (retained by-hash) answers 404. |
| `shutdown wait panicked` | ERROR | `panic`, `stack` | The shutdown drain's wait for the adoption, repair and reconcile goroutines panicked (a bug, please report it); shutdown carries on without waiting further for them. |
| `apt-cacher-ultra starting` | INFO | the effective configuration (see `cmd/apt-cacher-ultra/main.go`); the flat-repository work adds `adoption_flip_quiet_period` and `freshness_max_suites` | Logged once at start. `adoption_flip_quiet_period` is the configured `adoption.flip_quiet_period`; the flip barrier runs only when it is not `0s` and adoption is enabled. `freshness_max_suites` is the configured `freshness.max_suites`. |
