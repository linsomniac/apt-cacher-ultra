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
suite per 10 minutes — per minute for a refusal while every eviction
candidate was busy — unless the rejection cache, which holds 4,096 suites,
had to clear itself) and the unlabelled gauge `acu_suites_seed_rejected`
(suites refused within the last 10 minutes, or the last minute for a busy
refusal; updated within a minute). The
scheduler adds the unlabelled counter `acu_freshness_unlogged_panics_total`
(recovered panics — of a scheduled check, an adoption, its failure record
or the adoption heartbeat — that could not be logged because reporting
them panicked too: a bug, please report it).
`acu_adoption_duration_seconds` includes the barrier's wait (at most
140 s). Eager adoption (spec C4, C10) adds `acu_repair_deferred_total{host}`
(repairs and reconciles of an unchanged suite deferred because no adoption
slot was free; they run on a later unchanged check) and
`acu_strict_adoption_waits_total{host}` (unvouched `.deb` requests that
waited — at most 15 s, and never past 25 s after the request arrived — for
a signed adoption running on their host before strict mode decided; the
wait is part of the request line's `duration_ms`). Suite reaping (spec C9)
adds the counter `acu_suites_reaped_total{reason}` (suites the suite
reaper deleted, by `reason`: `idle`, `non_canonical`, `junk`, settled
reaps included; see `suite_reaped`) and the counter `acu_gc_retired_reaped_total` (retired
snapshots that snapshot GC deleted: those of reaped, forgotten and
evicted suites, and those the upgrade's trust audit quarantined), whose
per-pass count the `gc_run_complete` line carries as `retired_reaped`
(always 0 for the startup pass, which runs before the listeners serve and
deletes no retired snapshot: the periodic passes do, normally the first
one after the start).

The `freshness_check` line's `result` gains `changed_deferred` (upstream
still serves content whose adoption failed or was refused; the field
`reason` says why it is held back) and `changed_queued` (no adoption slot
was free; the suite waits for one and is re-checked as soon as one is
released; a client-refused change is picked up by the next tick, see
*Capacity and fairness* in configuration.md).

Eager adoption (spec C4) adds `first_adoption` (a suite without a snapshot
found its anchor and starts its first adoption; `form` is the form found,
and `upstream_status` is 200, the answer of the anchor the form was found
by — the absent probes before it are not logged),
`first_adoption_queued` (the same, waiting for an adoption slot),
`form_changed` (an adopted suite starts adopting another anchor form: a
detached suite whose upstream now serves `InRelease`, a suite whose
current anchor stayed absent and whose upstream serves the other signed
form, or an unsigned suite that gained a signature; `changed_queued` when
no slot is free) and `anchor_absent` (the current snapshot's anchor is
absent upstream: `upstream_status` is the absent answer — 401, 403, 404,
410, or 200 for a body that is not that file — and `reason` is
`anchor_absent`; the snapshot keeps serving; also a detached suite whose
changed `Release` comes without its `Release.gpg`, `upstream_status` then
being the `Release.gpg`'s answer). `form` also takes the value `unsigned`.
A suite without a snapshot that no client asked for in the last 7 days, or
whose anchor files only the cache itself fetched, is dormant: its checks
make no request and log no `freshness_check` line. Neither do the checks
a failure or refusal record paces: those of a suite without a snapshot
whose record is not due yet, within `periodic_refresh` of its last check,
and those of an adopted suite in `anchor_absent` before its retry time.

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
(cap refusals, evictions, reaping and `POST /forget`), a slow shutdown
drain and the HTTP servers' own diagnostics (logged by
`internal/freshness`, `internal/handler`, `internal/admin` and the
daemon's `main`).

| Event | Level | Fields | Meaning |
|---|---|---|---|
| `adoption_panicked` | ERROR | `where` (`adoption`, `repair`, `reconcile`, `heartbeat`, `record`), `canonical_scheme`, `canonical_host`, `suite_path` (the suite's scheme, host and path; the scheme tells its `http` copy from its `https` one), `panic`, `stack` (first occurrence per `where` per process only) | A panic inside an adoption, repair, reconcile, heartbeat or failure-record goroutine was recovered; the prior snapshot keeps serving. Only an adoption panic is recorded as a failure (reason `panic`); repair and reconcile panics are logged, not recorded (plan deviation 7). `where=record` is `recordAdoptionFailure`, which writes the spec C7 failure record after an adoption ends. |
| `freshness_check_panicked` | ERROR | `where` (`check`, `scheduler`, `reschedule`, `t1`, `reaper`, `forget`), `canonical_scheme`, `canonical_host`, `suite_path` (the suite the panic struck — the scheme tells its `http` copy from its `https` one; all three empty when the panic names no suite), `panic`, `stack` (first occurrence per `where` per process only) | A panic during a freshness check was recovered. For `where=check` the check counts as failed (`last_check_at` bumped, so the cooldown applies) and also counts as an unknown end: `last_check_at` is bumped and `check_fail_count` extended. The `scheduler`, `reschedule` and `t1` recovers only log; `where=reschedule` guards the start-time retry spread (`rescheduleFailingAdoptions`), and `where=t1` is the handler's client-triggered check goroutine (logged by `internal/handler`). `where=reaper` is the suite reaper: a panic while it held a suite names that suite, the hold is released as the cache then answers (the row gone or not), and the run goes on to its next candidate and its sweeps; a panic elsewhere in a run (the listing, the junk scan, the sweeps; empty `canonical_scheme`, `canonical_host` and `suite_path`) ends the run, and the next run comes as scheduled. `where=forget` is `POST /forget`: a panic while it held the suite names that suite; the hold is released as the cache then answers (the row gone or not), and the panic goes on to net/http, which ends the request without an answer and logs `http: panic serving …` at ERROR (see net/http's own lines below). |
| `freshness check failed` | INFO | `canonical_host`, `suite_path`, `form` (`inline`, `detached`, `unsigned`, or `probe` for the probe tree of a suite without a snapshot), `err`, `result` (always `failed`) | A check ended unknown — a transport error, an error status that is not an absent answer, a success status other than 200 or 304 (a fetch contract violation: `err` says `unexpected success status`), a 304 to a request that carried no validators (adoption enabled; `err` says `304 Not Modified to a request without validators`), or the 2-minute check deadline at the host slot or the request — or found nothing it may adopt: a suite without a snapshot whose upstream has neither `InRelease` nor `Release` (which also records the refusal state `anchor_absent`, shown on the status page); an unsigned `Release` while `adoption.adopt_unsigned` is off is the refusal `unsigned_disabled` instead. A check whose probe found a signature but could not record the suite as seen signed fails too (`err` starts `mark the signature seen:`). A shutdown that ends a check — its request, or a cache read or marker write after it — is not a failure: nothing is written or counted (DEBUG only). `last_check_at` is bumped and `check_fail_count` extended, so the scheduler backs off (2 min doubling to 1 h), and `acu_freshness_check_total{result="failed"}` is incremented. With adoption enabled, 401, 403, 404, 410 and a 200 whose body is not the file asked for are answers, not failures (the anchor is absent): they never extend the streak. |
| `adoption_run_failed` | WARN; INFO when the attempt was refused (outcome `refused`) | `canonical_host`, `suite_path`, `form`, `err` | Backstop line for every adoption attempt that returned an error, so a failure with no categorised line of its own is still visible. A refusal is a state, not a failure, so its line is INFO (`adoption_refused` carries the reason and timing); operators alerting on WARN are not paged for a flapping mirror. |
| `adoption: read prior snapshot failed` | WARN | `canonical_host`, `suite_path`, `err` | The adoption could not read the suite's current (prior) snapshot before running Date and rollback checks against it; the attempt fails as `db_failed` and is retried on the normal backoff schedule rather than skipping those checks. |
| `adoption: signed-anchor re-mark failed` | WARN; ERROR when a cached anchor's pool file could not be read (a missing file is not an error) | `canonical_host`, `suite_path`, `err` | Before an unsigned adoption's commit, the adopter re-reads the suite's cached `InRelease` and `Release.gpg` (and those of its `/.` twin) to record any signed one (spec C5 safeguard 3), and could not read a `url_path` row or a pool file, or write the record. The adoption is refused `unsigned_downgrade_refused` (fail closed): here that refusal means the cache could not rule out a signature, not that it saw one. It is retried on the refusal's schedule. |
| `adoption_failure_record_failed` | WARN — also when the read that precedes the write finds the suite's row gone, which cannot happen while the check or adoption that writes the record holds the suite (every deleter — cap eviction, the suite reaper, `POST /forget` — holds it too) and so is a bug; DEBUG for a write that a shutdown cut short (nothing is recorded at shutdown) | `canonical_host`, `suite_path`, `err` | The failure/refusal record could not be read or written; the next check may retry sooner than scheduled. |
| `adoption_refused` | INFO | `canonical_host`, `suite_path`, `reason`, `refusal`, `fail_count`, `retry_in_seconds`, `next_retry_unixtime` | One attempt ended in a refusal state, not a failure; logged for every refused attempt. The states: `upstream_older`, `rollback_refused`, `unsigned_downgrade_refused` (the suite was seen signed: decided by the check before a slot is taken, again before any download, and inside the commit), `unsigned_too_large` (decided by the check before a slot is taken, and again before any download), `unsigned_unconfirmed` (an unsigned repository waiting for its second observation), `unsigned_disabled` (`adoption.adopt_unsigned = false`: an unsigned `Release` is not adopted; an unsigned suite already adopted keeps its last snapshot; decided by the check before a slot is taken) and `anchor_absent` (the current anchor is absent upstream, a detached suite's changed `Release` has no `Release.gpg`, or a suite without a snapshot has neither anchor; while a failed adoption attempt's record is active — a lateral move that failed — that record keeps its own reason and retry time instead, and paces the suite the same way). |
| `adoption_deferred` | INFO | as `adoption_refused` | A suite entered the failing or refused state, or its reason changed (not logged on every attempt); the same content is retried at `next_retry_unixtime`, while new content is tried at once. |
| `adoption_failing_persistently` | WARN | `canonical_host`, `suite_path`, `reason`, `fail_count`, `failing_since_unixtime` | The suite's adoption has failed 3, 6, 12, 24, … times in a row. Refusals never reach this (spec C7); see `adoption_refused_persistently`. |
| `adoption_refused_persistently` | WARN | as `adoption_failing_persistently` (`reason` is the refusal reason; `failing_since_unixtime` is when the refusal streak began) | A refusal streak reached 3, 6, 12, 24, … attempts and began at least 24 h ago. A refusal is a state, not a failure, but one that lasts a day may be a silent freeze — a vendor that stops emitting `Date` is refused `upstream_older` forever — so it is made alertable here and on the admin page (`REFUSED 24H+`). A streak start in the future (clock step) restarts the streak, so it is never "old". |
| `adoption_retries_rescheduled` | INFO | `suites`, `spread_seconds` | At start, failing suites were given one retry spread over the refresh interval. Refusals and panicked adoptions keep their schedule. |
| `adoption_retries_reschedule_failed` | WARN | `err` | The start-time retry spread could not be written; failing suites keep their schedule. |
| `adoption_repair_list_failed` | WARN | `canonical_host`, `suite_path`, `snapshot_id`, `err` | The repair decision could not list the snapshot's repairable skipped members; no repair runs this check, the next unchanged check asks again. |
| `freshness: clamp adoption retry failed` | WARN | `canonical_host`, `suite_path`, `err` | A retry time left in the future by a backward clock step could not be clamped; the next check tries again. |
| `freshness: clear adoption failure failed` | WARN | `canonical_host`, `suite_path`, `err` | Upstream serves the current content again but the failure record could not be cleared; the next check tries again. |
| `freshness: adopted suites stale beyond threshold` | WARN | `adopted_stale`, `stale_factor`, `refresh` | Logged at each scheduler tick while adopted suites have had no successful check for `stale_factor` × `periodic_refresh` (4 × 15 min by default), or carry a success stamp in the future: the signature of a freeze. Suites in a refusal state that paces them by their record (`anchor_absent`, `unsigned_disabled`) are left out of `adopted_stale`: such a suite is checked only at its record's retries (up to 75 min apart), so its last success ages on purpose; the status page and `adoption_refused` show them. A suite in any other refusal state counts, since its last success ages only when its checks fail. The gauge `acu_freshness_stale_suites{adopted}` counts every stale suite, those included. With adoption disabled, records are inert and every stale adopted suite counts. |
| `freshness: read suite row failed` | WARN | `err`, `canonical_host`, `suite_path` | A freshness check could not read the suite's row: either the existence read a check makes before it takes the suite's lock (a suite is checked only while it has a row), or the full read after it. The check is skipped and writes nothing; the next trigger (a client hit's check or the scheduler) tries again. |
| `freshness: read anchor rows failed` | WARN | `err`, `canonical_host`, `suite_path` | A check could not read the suite's cached anchor rows (`url_path`). A suite without a snapshot is not probed this time — no request, nothing written; the next check tries again. A suite whose current anchor is absent probes the other forms at their canonical URLs instead. |
| `freshness: read current snapshot failed` | WARN | `err`, `snapshot_id`, `canonical_host`, `suite_path` | A check whose absent anchor's probe tree found a form it may adopt could not read the suite's current snapshot, so it cannot tell an anchor that merely came back from new content: the check fails (`freshness check failed`) rather than re-adopting the current content. A shutdown that ends the read is not logged here. |
| `adoption_member_mismatch` | WARN | `canonical_host`, `suite_path`, `path`, and `declared_sha256`/`actual_sha256` — or `declared_sha512`/`actual_sha512` when the member's SHA512 is the hash that failed | A member's bytes did not match the hash its `Release` declares (every declared SHA256 and SHA512 is checked), and the adoption fails (outcome `member_mismatch`). Fetched bytes never reach the pool. A pool file reused under its declared SHA256 whose SHA512 differs from the declared one — the `Release`'s two declarations disagree, so a fetch would bring the same bytes — is logged with `declared_sha512`/`actual_sha512`, is not fetched again, and is kept. |
| `adoption_reconcile_failed` | WARN | `canonical_host`, `suite_path`, `snapshot_id`, `stage` (`decision`: deciding whether an unchanged suite needs a reconcile; `heal`: the reconcile run after an unchanged check; `on_demand`: `POST /reconcile`), `err` | Reconciling a snapshot's declared index files failed — for example a stored anchor that no longer decodes — so nothing was healed this time; it is tried again on a later unchanged check. |
| `freshness: read suite row after a panic failed` | WARN | `err`, `canonical_host`, `suite_path` | After a recovered check panic, the best-effort bump of `last_check_at`/`check_fail_count` could not even read the suite's row; the row is left as it was. |
| `freshness: persist failure bump after a panic failed` | WARN | `err`, `canonical_host`, `suite_path` | After a recovered check panic, `last_check_at` and `check_fail_count` were bumped in memory but the write failed; the row is left as it was. |
| `clock_step_detected` | WARN | `stamp`, `canonical_scheme`, `canonical_host`, `suite_path`, `stored_unixtime`, `now_unixtime` | A stored timestamp is more than 2 s in the future (the wall clock stepped backwards); the gate treated it as old. Rate-limited to one line per minute, except the suite reaper's reports (`stamp=last_requested_at` and `stamp=last_seen_at` from the reaper, and `stamp=wall_clock`), which it bounds itself and logs directly, so a check's report in the same minute cannot hide them. For `stamp=adoption_next_retry_at` the stamp is not treated as old: it is clamped to now + 1 h. For `stamp=last_requested_at` (a suite's client-interest stamp, read when the suite cap looks for an idle suite to evict, by the suite reaper, and by the check of a suite without a snapshot) the suite is not evicted, nor reaped as idle — a future stamp never counts as idle, though a non-canonical or junk row is reaped regardless (see `suite_reaped`); the reaper reports the furthest such stamp of a canonical suite at the first run that reaps after a start and again only when it changes — and the check counts a stamp up to 7 days ahead as recent interest; further ahead, the suite is dormant until a client asks for it again. For `stamp=last_seen_at` (a signed-seen record's stamp, read once a day by the suite reaper's sweep of records no suite uses; `suite_path` is the record's repository path, the furthest — of records with the same stamp, the first by scheme, host and path — reported at the first sweep after a start — a day after it — and when it changes; "in the future" is measured against the time of the sweep itself, so the records the run's own reaps moved up minutes earlier are no step) the record counts as seen now — it is never swept — and its stamp is left as it is: it ages out only once the clock has passed it (rewriting it to the stepped-back now would lose signed history once the clock is corrected). For `stamp=wall_clock` (the suite reaper; no suite fields) the wall clock moved more than an hour further than the monotonic clock between two of the reaper's runs — a forward step without a restart, as when a virtual machine or a laptop resumes; `stored_unixtime` is where the wall clock should be, `now_unixtime` where it is — and the reaper deletes nothing, no suite and no record, for an hour, and leaves the step out of the records' age for as long as it runs, so the record sweep never deletes a record early because of it; this one is not rate-limited and not counted in `acu_clock_steps_detected_total`. `acu_clock_steps_detected_total` counts detections, not distinct clock steps or distinct stamps: the same future stamp is detected again every time it is read — every check for a future `last_check_at` or `last_success_at` (the scheduler reads a future `last_success_at` without reporting it), every check of a suite without a snapshot for a future `last_requested_at`, and every creation attempt within eight rows of the global cap for the future `last_requested_at` its eviction scan re-reads (the line stays rate-limited) — so alert on its rate, not its value. |
| `legacy_snapshot_quarantined` | WARN | `reason` (`legacy_plaintext`, `malformed_envelope`, `unsigned_declarations`, `unparseable`, `anchor_missing`, `anchor_oversized`, `anchor_corrupt`, `unexpected_form`), `snapshot_id`, `canonical_host`, `suite_path`, `anchor_form`, `was_current` | The startup trust audit could not prove a pre-upgrade snapshot signed and clean; it no longer serves or vouches, and its suite is served unadopted until it re-adopts. |
| `legacy_trust_audit_deferred` | ERROR | `snapshot_id`, `canonical_host`, `suite_path`, `err`, `stack` (present only for a recovered panic) | A pre-upgrade snapshot's pool file could not be read (not "missing"), or auditing it panicked (`err` carries the panic value); the snapshot is left as it was and retried at the next start. |
| `legacy_trust_audit_progress` | INFO | `audited`, `quarantined`, `deferred` | One batch of up to 100 queued snapshots was audited and committed; the fields are running totals across this start's batches. Logged on the first start after an upgrade, and on later starts while deferred snapshots remain queued or after an interrupted audit. |
| `legacy_trust_audit_completed` | INFO | `audited`, `signed`, `quarantined`, `deferred`, `anchor_markers`, `duration` | The startup trust audit finished (every start; `audited` is 0 once the upgrade queue has drained). |
| `adoption_flip_barrier` | INFO | `canonical_host`, `suite_path`, `snapshot_id`, `outcome` (`held`, `quiet_cut_short`, `hold_expired`, `hold_skipped`, `not_needed`, `commit_failed`, `refused`), `phase_a_seconds`, `token_wait_seconds`, `hold_seconds`, `held_requests` (the last four absent when the barrier was skipped for a first adoption) | An adoption committed behind the spec C8 flip barrier: `held` within its hold, once its suite was quiet; `quiet_cut_short` within its hold at 15 s, before its suite was quiet — the held requests got the new snapshot, a client still reading the old anchor then (or done with it within `flip_quiet_period`) may straddle (`acu_flip_quiet_cut_short_total`); `hold_expired` when the hold reached 20 s before the commit finished — the held requests were answered from the old snapshot (`acu_flip_hold_expired_total`); `hold_skipped` after 60 s without the hold token (`acu_flip_hold_skipped_total`); `not_needed` for a first adoption whose anchor clients were already being served; `commit_failed` when the commit ran and failed — the snapshot did not switch, the timing fields still describe the barrier, and `adoption_run_failed` gives the error; `refused` when the commit refused an unsigned candidate because the suite has signed history (spec C5's recheck inside the commit) — the snapshot did not switch, and `adoption_refused` gives the reason, `unsigned_downgrade_refused`. Not logged when `adoption.flip_quiet_period = 0s`, nor for a flip abandoned at shutdown (`adoption_abandoned_at_shutdown` says so). |
| `adoption: served anchor read failed; the flip barrier runs` | WARN | `canonical_host`, `suite_path`, `path`, `err` | Before a first adoption's commit, the adopter could not read the anchor the cache serves for the suite (when clients already get the anchor being adopted, the barrier is not needed); the barrier runs — the safe direction. A missing row is not logged. |
| `adoption_abandoned_at_shutdown` | INFO | `canonical_host`, `suite_path`, `form`, `err` | An adoption was abandoned at shutdown — waiting in the flip barrier (or about to skip it) when the daemon closed the barrier at the signal, or in any step before its commit once the daemon's lifetime context ended: nothing was committed or recorded, the change is adopted after the restart. Not a failure: no `adoption_run_failed`, not counted in `acu_adoption_total` or `acu_adoption_duration_seconds`, not shown in the admin page's recent adoptions. |
| `displaced snapshot lookup failed` | WARN | `err`, `canonical_host`, `path` | Retained by-hash could not read the suite's displaced snapshots; the request is answered 404, as before spec C8. |
| `snapshot adopted_at lookup failed` | WARN | `err`, `snapshot_id` | A snapshot `InRelease`/`Release`/`Release.gpg` could not read its snapshot's `adopted_at` (spec C8 `Last-Modified`), so this response carries the pool file's mtime instead and its `Last-Modified` may not grow with the last flip. Not logged when the request had already ended. |
| `shutdown_waiting_for_adoptions` | INFO | `waited`, `detail` | Shutdown has waited 2 s for the adoption, repair and reconcile goroutines: a commit already running is never cancelled and may take up to its 5-minute bound; other work stops at its next cancellation point (adoptions in the flip barrier were abandoned at the signal). Logged once per shutdown. |
| `suite_seed_rejected` | WARN | `canonical_host`, `suite_path`, `cap` (`global_cap`, `host_cap`), `max_suites`, `suppressed`, `detail` | A suite was refused a row by `freshness.max_suites`: it is served but will not refresh; retried after 10 minutes (`detail` = `suite will not refresh`). At the total cap while every suite that could have been evicted was busy — being checked or adopted, or evicted for another new suite — the refusal is transient: `detail` = `eviction candidates busy; retried after 1 minute`, and the suite is retried after a minute. At most one line a minute; `suppressed` counts the refusals since the last line. |
| `suite_evicted` | INFO | `canonical_host`, `suite_path` (the evicted suite), `for_host`, `for_suite` | At or above the global cap, a suite with no adopted snapshot that had not been requested for 24 hours was evicted for a new one; its signed-seen records survive, moved up to the time of the eviction (as for `suite_reaped`). Above the cap (after `freshness.max_suites` was lowered, or after an upgrade) the new suite is still refused (`suite_seed_rejected`) until the table is back under the cap. |
| `suite_caps_exceeded` | WARN | `suites`, `max_suites`, `hosts_at_cap`, `per_host_cap`, `fullest_host`, `fullest_host_suites` | At start, the suite table holds more than `freshness.max_suites` suites (a lowered `max_suites`, or an upgraded cache), or a host holds a tenth of it or more: new suites — for a host at its cap, that host's — are refused. Above the total cap each refused attempt still evicts one suite idle for 24 hours, so the table shrinks back; a host at its cap evicts nothing for its own new suites; its suites go when the suite reaper removes those not requested for `gc.suite_idle_ttl`, or when total-cap eviction for any host's new suite picks one of its idle suites. Not logged for a table at exactly the total cap: that is normal operation for a full cache, where a new suite evicts an idle one (no adopted snapshot, not requested for 24 hours) and is refused only when none qualifies (`suite_seed_rejected`). Logged once per start. |
| `suite cap usage read failed` | WARN | `err` | The startup check behind `suite_caps_exceeded` could not count the suites; the caps apply either way. |
| `suite_reaped` | INFO | `canonical_scheme`, `canonical_host`, `suite_path`, `reason` (`idle`, `non_canonical`, `junk`), `metadata_rows`, `settled` (present, `true`, only for a settled reap) | The suite reaper (spec C9; hourly, from an hour after the start; a run tries at most 250 `non_canonical` and `junk` suites, first, and at most 25 `idle` ones) deleted a suite: its metadata `url_path` rows (`metadata_rows`, in batches of 1,000, the anchors last), then, in one transaction that re-checks that the suite was not adopted — nor, for `idle`, asked for — since the run listed it, its snapshots (retired: snapshot GC deletes them) and its row. `idle`: no client asked for it in `gc.suite_idle_ttl`; `non_canonical`: a row an earlier release created for a path this version no longer tracks (`metadata_rows` is 0: such a path's cached files are left to `gc.url_path_ttl`); `junk`: a suite without a snapshot whose cached anchor files (`InRelease`, `Release`, `Release.gpg`) all fail their content check — an HTML page, say; a plain `Release` cached as `InRelease` passes — each fetched at least a day ago, whatever its adoption record says. Its signed-seen records survive, moved up to the time of the reap (a later stamp is kept): every suite-row delete — a reap, a cap eviction, a forget that leaves the records — does so, so a record's year (`signed_seen_markers_swept`) counts from the end of its path's tracking; and a suite with an adopted signed snapshot (`inline` or `detached`, not retired) gets its path's record under its own scheme before its snapshots are retired — created if missing, as for a snapshot the upgrade's trust audit could not read yet — so that history survives too. `settled=true`: the final transaction committed but the writer call reported an error, and a fresh read found the row gone — it is a reap all the same. Counted in `acu_suites_reaped_total{reason}`, settled reaps included. |
| `admin_forget_requested` | INFO | `scheme`, `canonical_host`, `suite_path` | `POST /forget` accepted a request (logged by `internal/admin`): it passed the header checks (`X-ACU-Admin: 1`, `Sec-Fetch-Site`), the 8 KiB body limit and the parameter checks, and the forget starts. A request refused with 400, 403 or 413 is not logged. `scheme` is the suite's canonical scheme (the other lines about a suite name it `canonical_scheme`). The outcome follows: `suite_forgotten`, `admin: forget failed`, no line for a busy or unknown suite (409), or, after a panic, `freshness_check_panicked` with `where=forget`. |
| `suite_forgotten` | INFO | `canonical_scheme`, `canonical_host`, `suite_path`, `metadata_rows`, `markers_deleted`, `anchor_rows`; `settled` (present, `true`, only for a settled forget, whose line carries `metadata_rows` but not `markers_deleted` or `anchor_rows`) | `POST /forget` deleted a suite as the reaper would, regardless of idleness, and — only when no other suite of the repository path, under either scheme or as its `/.` twin, has a row — `markers_deleted` signed-seen records of the path and `anchor_rows` cached `InRelease`, `Release` and `Release.gpg` of the path and its twin under either scheme. Its request is logged first as `admin_forget_requested`. `settled=true`: the final transaction committed but the writer call reported an error, and a fresh read found the row gone — a forget all the same (the records and cached anchors deleted as above when no other suite of the path has a row; how many is not known); the request is then answered 500 with WARN `admin: forget failed`, or 503 with that line at DEBUG when the admin listener's shutdown cut it short. |
| `admin: forget failed` | WARN | `err`, `scheme`, `canonical_host`, `suite_path` | `POST /forget` failed on a database error, or its work took longer than 5 minutes, and answered 500. Metadata batches deleted before the error stay deleted (the anchors go last); the request can be repeated (409 then means the suite is gone after all, or busy). A forget whose final delete committed before the error is logged `suite_forgotten` with `settled=true` just before this line. Logged at DEBUG instead, and answered 503, when the admin listener began shutting down while the forget ran: shutdown cuts it short. |
| `signed_seen_markers_swept` | INFO | `count` | The suite reaper's daily sweep (the first a day after the start, then a day apart) deleted `count` signed-seen records whose repository path has no suite row (for it or its `/.` twin, under either scheme) and that were last seen — or a suite of their path last deleted: reaped, evicted at the cap, or forgotten while another suite of the path kept the record — more than 365 days ago, by the clock less the forward steps the reaper detected while running (`clock_step_detected` with `stamp=wall_clock`), so such a step never ages a record. Not logged when it deleted none. |
| `suite reaper: list suites failed`, `suite reaper: read suite row failed`, `suite reaper: delete metadata failed`, `suite reaper: reap failed`, `suite reaper: list unadopted anchors failed`, `suite reaper: junk anchor reads failed`, `suite reaper: marker sweep failed` | WARN | `err`; `canonical_scheme`, `canonical_host` and `suite_path` for a suite's step; `metadata_rows` (for `delete metadata failed`: rows deleted before the error); `count`, `canonical_scheme`, `canonical_host` and `path` for the junk anchor reads (how many anchor reads failed in the run's junk scan — a failed read is not remembered, so one unreadable pool file that N suites share counts once for each of them whose scan reaches it, up to N times — and the first: its suite's scheme and host and the anchor's path); `swept` (records deleted before the error) | A step of a suite reaper run failed. A failed listing ends the run: nothing is reaped and nothing swept (a due record sweep runs at the next run); any other failed step leaves the rest of the run to go on: a suite whose step failed — a database error, or its work cut short after 5 minutes — keeps its row and is retried at the next run (the metadata batches deleted before the error stay deleted, the anchors last); a pool file that exists but does not read keeps its suite (a missing one is not logged); the junk scan runs again at the next run, the record sweep the next day. A step that fails because the daemon is stopping is logged at DEBUG instead. |
| `suite reaper: suite busy, retried next run` | DEBUG | `canonical_scheme`, `canonical_host`, `suite_path` | A suite the reaper would reap was held — by a check, an adoption, a repair or reconcile, or another deleter (cap eviction, `POST /forget`) — or deleted meanwhile; it is tried again at the next run if it still qualifies, and counts toward the run's budget for its kind (25 `idle` suites; 250 `non_canonical` and `junk` ones). |
| `handler_goroutine_panicked` | ERROR | `where` (`touch`, `seed_rejections`), `canonical_scheme`, `canonical_host` and `path` (`touch` only), `panic`, `stack` (first occurrence per `where` per process only) | A panic in one of the handler's background goroutines — the touch goroutine, or the cap-rejection cache's sweep timer — was recovered (a bug, please report it). |
| net/http's own lines: `http: panic serving …`, `http: TLS handshake error from …` and the like | ERROR on the admin listener; WARN on the proxy listeners | none: `msg` is net/http's text — for a panic, `http: panic serving <client address>: <panic>` followed by the stack | The Go HTTP server's own diagnostics, logged through the daemon's structured log at the level each listener sets (left to itself, net/http would write them through Go's standard log package, which the daemon also sends to its log, but at INFO). On the admin listener every such line is ERROR, above all a handler panic net/http recovered: after a `POST /forget` panic, which is logged first as `freshness_check_panicked` with `where=forget`, the request ends without an answer and gets no `admin_request` line. On the proxy listeners (plain and TLS) they are WARN, a TLS handshake error included. |
| `suite onboarding failed` | WARN | `err`, `canonical_host`, `suite_path` | A cache hit on an untracked suite's anchor could not create its row (including the 5 s bound it shares with the backstop that may follow, and a shutdown that cut it short); the anchor is served and the next hit retries. |
| `suite signed-seen mark failed` | WARN | `err`, `canonical_host`, `suite_path` | A suite's row was created, and one of its cached signed anchors changed between the handler's read and the creating write (or the handler could not read it: `suite cached anchor read failed`), so it was marked afterwards — and that write failed, or ran out of its 5 s bound. The startup pass marks it at the next start. (Every other mark is part of the write that creates the row or stores the anchor.) |
| `suite file head read failed` | WARN | `err`, `canonical_host`, `suite_path`, `path` | A suite file just fetched on a miss could not be read back from the cache's pool for its content check (a local disk fault). For a signed anchor (`InRelease`, `Release.gpg`) the miss fails — 502 with `Retry-After`, request outcome `cache_write_failed` (and ERROR `cache write failed`), no body, nothing stored — because the anchor must not be served before its suite is marked as signed; apt retries. Any other suite file (`Release`, a flat index) is stored and served, but creates no suite row and re-stamps none. |
| `suite cached anchor read failed` | WARN | `err`, `canonical_host`, `suite_path`, `path` | Creating a suite's row, the handler could not read one of the suite's cached signed anchors (its `url_path` row or its pool file; an absent row is not logged). The row is created anyway, and the anchor is read again and marked right after the write, before the response — also when the client left, or the request is answered from a new snapshot, while it waited for a flip (`suite signed-seen backstop read failed` if that fails too). Between the write and that mark the suite is tracked with the anchor unmarked; if the mark fails, it stays so until the startup pass at the next start. |
| `suite signed-seen backstop read failed` | WARN | `err`, `canonical_host`, `suite_path`, `path` | After creating a suite's row, the handler's second read of a cached signed anchor (see `suite cached anchor read failed`; also run when the anchor changed during the creation) failed — its `url_path` row (within the backstop's 5 s bound, which covers its database steps; after onboarding the bound is shared with the creation, and on a miss it ends at most 25 s after the request arrived) or its pool file (a local read of a few KiB, not bounded); an absent row is not logged. The response is served; the anchor stays unmarked until the startup pass marks it at the next start. |
| `snapshot_member_blob_missing` | WARN | `canonical_host`, `path`, `snapshot_id`, `blob_hash`, `snapshot_displaced` (present for a displaced snapshot's member) | A snapshot member's pool file is gone: a current member is re-fetched and checked against its declared hash; a displaced one (retained by-hash) answers 404. |
| `pool_corruption_during_adoption` | WARN | a member that declares a SHA256: `canonical_host`, `suite_path`, `path`, `declared_sha256`, `actual_sha256`; a member the Release declares by SHA512 only: `canonical_host`, `suite_path`, `path`, `declared_sha512`, `derived_sha256` (the SHA-256 of the fetched bytes, verified against `declared_sha512`), `actual_sha256`; an anchor or index the adoption stores from memory: `hash`, `actual_sha256`, `context` (`writeBlobBytes`) | An adoption was about to reuse a pool file whose bytes no longer hash to its name (corrupted at rest). The file is evicted and replaced by freshly verified bytes; it is never bound to the snapshot, and the `.deb` hashes it declares never vouch. A SHA512-only member is fetched at every adoption, and an existing pool file under its derived SHA-256 is re-hashed before it is kept. Counted by `acu_pool_corruption_during_adoption_total`. |
| `package_coverage_incomplete` | INFO | `canonical_host`, `suite_path`, `snapshot_id`, `reason`; `directories` (`unsupported_variants`), `no_sha256_stanzas` (`no_sha256`), `skipped_rows` (`all_binary_stanzas_malformed`) | A signed snapshot's package coverage could not be proven complete (SPEC3 §7.5.4), so strict mode (`integrity.refuse_unvouched_debs`) stays off for its host. `reason`: `non_dists_layout` (the suite path is not `dists/`-shaped), `no_packages_members` (its Release lists no `Packages` index), `unsupported_variants` (a directory with a `Packages` index has no variant that was fetched and parsed; `directories` names them), `no_sha256` (spec C5: a `Packages` stanza with a `Filename` but no `SHA256`, for example one that declares only `SHA512` or `MD5sum`; it vouches nothing, and `no_sha256_stanzas` counts the distinct paths), `all_binary_stanzas_malformed` (every binary stanza lacks `Package`, `Architecture` or `Version`). A reconcile that heals `Packages` indexes recomputes the rows for them and may log it too; a reconcile never changes a snapshot's coverage. |
| `package_hash lookup failed` | WARN | `err`, `canonical_host`, `path` | On a `.deb` cache hit the vouching hashes (`package_hash`) could not be read: the cached file is served without the hash check, and strict mode (`integrity.refuse_unvouched_debs`) fails open for it, so a database fault does not refuse every package. A request whose client left is answered 503 (`client_canceled`) instead, never served, and logs nothing here. |
| `strict-mode coverage lookup failed` | WARN | `err`, `canonical_host` | Strict mode could not read its host's current snapshots and their coverage: it fails open for the request (the `.deb` is served or fetched as if strict mode were off). A request whose client left is answered 503 (`client_canceled`) on the hit path and logs nothing here. |
| `package_hash validate lookup failed` | WARN | `err`, `canonical_host`, `path` | After a `.deb` download the vouching hashes could not be read: the body is kept and served without validation (fail open, as on the hit path). |
| `shutdown wait panicked` | ERROR | `panic`, `stack` | The shutdown drain's wait for the adoption, repair and reconcile goroutines panicked (a bug, please report it); shutdown carries on without waiting further for them. |
| `apt-cacher-ultra starting` | INFO | the effective configuration (see `cmd/apt-cacher-ultra/main.go`); the flat-repository work adds `adoption_flip_quiet_period`, `freshness_max_suites`, `gc_suite_idle_ttl` and `adoption_adopt_unsigned` | Logged once at start. `adoption_flip_quiet_period` is the configured `adoption.flip_quiet_period`; the flip barrier runs only when it is not `0s` and adoption is enabled. `freshness_max_suites` is the configured `freshness.max_suites`, `gc_suite_idle_ttl` the configured `gc.suite_idle_ttl` (in nanoseconds in JSON; `0s`: suite reaping is off) and `adoption_adopt_unsigned` the configured `adoption.adopt_unsigned`. With adoption enabled, the `phase2 adoption enabled` line that follows lists the adoption settings, `adopt_unsigned` among them. |
