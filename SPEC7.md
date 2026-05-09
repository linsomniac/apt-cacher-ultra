# apt-cacher-ultra — Phase 7 Specification

This document specifies the contract for Phase 7: the **operator
control plane** — mutating admin endpoints, CA rotation, limited
config hot-reload, and a write-role auth split. It is a delta over
[SPEC.md](SPEC.md) (Phase 1), [SPEC2.md](SPEC2.md) (Phase 2),
[SPEC3.md](SPEC3.md) (Phase 3), [SPEC4.md](SPEC4.md) (Phase 4),
[SPEC5.md](SPEC5.md) (Phase 5), and [SPEC6.md](SPEC6.md) (Phase 6).
Sections that carry forward unchanged say so explicitly and point at
the prior spec; sections that change describe only the delta. The
companion document [PHASE-7-SCOPING.md](PHASE-7-SCOPING.md) records
the design rationale and the sixteen-question scoping pass that
produced this spec.

Phase 7 is **opt-in additive** over Phase 5 / Phase 6:

- All mutating endpoints are gated by
  `admin.mutating_htpasswd_path` (and/or
  `admin.mutating_bearer_tokens`). With both unset (the default),
  every `POST /admin/*` returns `503 Service Unavailable` with
  `error: "mutating endpoints disabled"`.
- All hot-reload endpoints are gated by `[reload].allowed_keys`.
  With the list empty, `SIGHUP` is logged-and-ignored and
  `POST /admin/config/reload` returns `503`. The default config
  ships the §5.4 set populated.
- The `apt-cacher-ultra ca rotate` subcommand requires
  `tls_mitm.enabled = true` per §14.2; otherwise it exits 1 with
  `mitm_disabled`.

With these gates closed, a Phase 6 daemon upgrades to Phase 7 with
zero behavior change. Operators turn the new surface on by
populating one config field at a time.

**Audit-anchor note.** Every mutating endpoint produces an
`admin_action_started` Info on receipt (recording `caller`,
`remote_addr`, `job_id`) and a paired `admin_action_completed`
Info on terminal state (recording `outcome`, `duration_ms`,
optional `error`). The pair is the audit primitive: a control-
plane action without these two log lines is a daemon bug.

**Read/write realm separation.** Phase 5's
`admin.htpasswd_path` is the **read-only** credential. Phase 7
introduces `admin.mutating_htpasswd_path` (and the optional
bearer-token list) as the **write-only** credential. A read-role
credential cannot reach any `POST /admin/*` endpoint (`401`); a
write-role credential cannot reach `GET /metrics`, `GET /`, or
`GET /admin/events` (`401`). The two realms must be configured
with distinct credentials per §5.3.

---

## 1. Goals & non-goals

### 1.1 Phase 7 goals

1. **Mutating admin endpoints.** Five new POST endpoints on the
   Phase 5 admin listener:
   - `POST /admin/gc/run` — trigger a GC pass immediately.
   - `POST /admin/cache/clear` — invalidate cache entries selected
     by `canonical_host` or `suite`.
   - `POST /admin/suites/{path}/refresh` — force re-adoption of
     one suite immediately.
   - `POST /admin/ca/rotate` — generate a new MITM CA keypair and
     swap it in atomically.
   - `POST /admin/config/reload` — apply a hot-reloadable subset
     of the config without restart.

   All five are async — they return `202 Accepted` with a
   `Location: /admin/jobs/{id}` header pointing at a poll-able
   job-status URL. The same endpoint accepts an idempotency key
   (§2.2.0) so retries do not double-fire.

2. **CA rotation primitive.** The CA-rotate flow is reachable
   both through `POST /admin/ca/rotate` (online — daemon swaps the
   keypair while serving traffic) and through the
   `apt-cacher-ultra ca rotate` subcommand (§14.2 — operator runs
   it offline, daemon picks up the new files on next start). The
   two paths produce identical disk state. No cross-fade window
   (§9.4 — operator orders distribute-then-rotate).

3. **Limited config hot-reload.** `SIGHUP` and (optionally)
   `POST /admin/config/reload` apply the §5.4 reloadable subset.
   Listener bindings (`cache.listen`, `cache.listen_tls`,
   `admin.listen`), CA paths (`tls_mitm.ca_cert`,
   `tls_mitm.ca_key`), schema-affecting keys, and
   `tls_mitm.enabled` itself remain restart-only. The reload is
   all-or-nothing — a validation failure on any field aborts the
   whole reload and the live config is unchanged.

4. **Write-role auth split.** Mutating endpoints require a
   credential matching `admin.mutating_htpasswd_path` (or one of
   the bearer tokens in `admin.mutating_bearer_tokens`). The
   read-only Phase 5 surface continues to use
   `admin.htpasswd_path`. The two realms are mutually exclusive:
   no credential can satisfy both, and a startup-config
   validation error fires if they are configured to the same
   path (§5.3).

### 1.2 Phase 7 non-goals (deferred)

Carried forward from earlier phases unchanged:

- Source-package caching, multi-arch beyond amd64, pdiff
  (Phase 8+).
- Streaming-while-fetching as a singleflight optimization.
  Deferred to [FUTURE-REVIEW.md §1](FUTURE-REVIEW.md).
- Per-byte upstream read timeouts. Deferred to
  [FUTURE-REVIEW.md §1](FUTURE-REVIEW.md).
- Per-suite freshness cadence variation. Deferred to
  [FUTURE-REVIEW.md §2](FUTURE-REVIEW.md).
- Periodic `pool/` orphan-file rescan (SPEC4 §1.2 — periodic
  ticker + manual `/admin/gc/run` cover normal operations).

Newly deferred in Phase 7:

- **OpenTelemetry / OTLP exporters.** Same disposition as
  SPEC5 §1.2 / SPEC6 §1.2.
- **Distributed tracing.** Same disposition.
- **Admin-listener TLS.** Read-only and write-role surfaces both
  remain plain HTTP behind reverse proxy / network ACL. A daemon-
  internal TLS termination flow doubles cert-management surface
  for negligible benefit on a 127.0.0.1-by-default listener.
- **Client TLS auth (mTLS) on the admin port.** htpasswd +
  bearer + reverse-proxy-mTLS cover the operational need.
- **HSM / PKCS#11 CA keys.** SPEC6 §1.2; Phase 7 rotate writes
  file-on-disk CA. HSM glue waits for any deployment to ask.
- **Per-client CA pinning.** SPEC6 §1.2; out of scope.
- **Pre-emptive cert generation after rotate.** Cert cache flushes
  on rotate (§6.4); leaves regenerate on next CONNECT. No warm
  pre-issuance.
- **Ed25519 leaf algorithm.** SPEC6 §5.1.3 closed enum (`ecdsa-p256`,
  `rsa2048`) is unchanged.
- **Non-443 HTTPS upstreams.** SPEC6 §2.2 step 1; CONNECT port
  allowlist is unchanged.
- **Persisted job history.** Jobs are in-memory only; on daemon
  restart, all jobs are forgotten (terminal jobs gone, in-flight
  jobs are abandoned with `admin_job_orphaned` Info during
  graceful shutdown). A SQLite-backed job-history table is a
  Phase 8+ topic.
- **`/admin/cache/clear` of an individual blob by content hash.**
  Selectors are `canonical_host` or `suite`; per-blob is not.
- **Mutating endpoints on the proxy listener.** All control-plane
  mutation flows through the admin listener; the proxy listener
  continues to return `405 Method Not Allowed` for `POST` (SPEC §2.6).
- **CA rotation cross-fade.** Per §1.3 (Q4) the rotate flow is
  immediate switchover. A windowed two-CA mode (old CA continues
  to sign leaves alongside new for N hours) waits for any
  deployment to ask.
- **Job cancellation by operator.** A running job runs to
  completion (or shutdown); there is no `DELETE /admin/jobs/{id}`.
  Operators waiting on a slow job either let it finish or send
  SIGTERM to the daemon (which fails the job with
  `context canceled`).
- **Bulk endpoint** (e.g. `POST /admin/cache/clear/multi`).
  Per-call invocation only.

### 1.3 Resolved during Phase 7 scoping

The sixteen design questions raised in PHASE-7-SCOPING.md §7
were resolved with the operator. Each resolution is normative:

- **Theme (Q0).** Bucket A — operator control plane.
- **Sync vs async (Q1).** Async; `202 Accepted` + `Location:
  /admin/jobs/{id}` + JSON jobs surface.
- **Cache-clear selectors (Q2).** `canonical_host` and `suite`
  only; `path` is not in scope.
- **Cache-clear under live traffic (Q3).** In-flight requests
  complete naturally; new requests miss. No forced 503.
- **CA rotation cross-fade (Q4).** No cross-fade. Operator
  orders distribute-then-rotate.
- **Suite refresh URL (Q5).** Apt path
  (`/admin/suites/{path}/refresh`).
- **Bearer tokens (Q6).** Yes — opt-in, alongside htpasswd.
- **Caller-label cardinality (Q7).** No additional cap; existing
  Phase 5 `metric_series_cap = 1024` absorbs it.
- **Hot-reload allowlist default (Q8).** Default-populated.
- **Old-CA disk retention (Q9).** Keep all historical CAs under
  `cache_dir/ca/old/{fingerprint}/`.
- **Rotate when MITM disabled (Q10).** Refuse with exit 1 / 503.
- **SIGHUP during shutdown (Q11).** Log
  `reload_during_shutdown_ignored` Info.
- **Reload audit shape (Q12).** One event per reload listing all
  changed keys.
- **Status page caller column (Q13).** Yes — show `caller`.
- **Proxy-listener-misroute metric (Q14).** No new metric.
- **Job retention default (Q15).** `job_retention = 100`.

---

## 2. Wire contracts (deltas over SPEC §2 / SPEC2 §2 / SPEC3 §2.7 / SPEC5 §2 / SPEC6 §2)

### 2.1 Listener inventory — unchanged

Phase 7 adds no new listener and changes no port. The proxy
listener (`cache.listen`), the optional TLS-to-cache listener
(`cache.listen_tls`), and the admin listener (`admin.listen`)
all carry forward from SPEC5 §2 / SPEC6 §2 unchanged. All Phase 7
HTTP contracts live on the admin listener.

The proxy listener continues to return `405 Method Not Allowed`
for `POST`/`PUT`/`DELETE` (SPEC §2.6); a misconfigured operator
script that POSTs `/admin/something` to `:3142` is rejected at
the existing 405 path.

### 2.2 Mutating endpoints

All five mutating endpoints share a common shape:

- **Method:** `POST`.
- **Auth realm:** write-role (§5.3); without a write-role
  credential the endpoint returns `401 Unauthorized` with
  `WWW-Authenticate: Basic realm="apt-cacher-ultra (write)"`
  (htpasswd) or `Bearer realm="apt-cacher-ultra (write)"` (bearer).
- **Disabled state:** if neither
  `admin.mutating_htpasswd_path` nor `admin.mutating_bearer_tokens`
  is configured, the endpoint returns `503 Service Unavailable`
  with body `{"error":"mutating endpoints disabled"}` and
  `Retry-After: 0`. Authentication is not attempted.
- **Idempotency key (optional).** A request MAY include
  `Idempotency-Key: <opaque-string>` header (max 128 bytes,
  printable ASCII). The daemon caches the (caller, key) → job_id
  mapping for the lifetime of the resulting job plus
  `admin.idempotency_window` (default 5 minutes); duplicate
  requests within the window return the original `202` response
  with the same `Location` header. Without a key, every request
  produces a new job.
- **Success response:** `202 Accepted` with:
  - `Location: /admin/jobs/{job_id}` header.
  - `Content-Type: application/json` body
    `{"job_id":"<id>","action":"<action-name>","accepted_at":<unix>}`.
- **Error responses (pre-job):**
  - `400 Bad Request` — malformed selector, missing required
    parameter, or selector value validation failure.
  - `401 Unauthorized` — missing/invalid credential.
  - `403 Forbidden` — credential authenticates but is bound to
    the read realm.
  - `409 Conflict` — only emitted by `/admin/ca/rotate` and
    `/admin/config/reload` when another job of the same family
    is in `running` state (§9.2 / §9.3).
  - `503 Service Unavailable` — feature disabled (mutating
    realm not configured, or `tls_mitm.enabled=false` for
    rotate).

The `job_id` is a randomly-generated 16-byte URL-safe-base64
string (no padding); the daemon does not assume it carries
information beyond uniqueness.

#### 2.2.1 POST /admin/gc/run

Triggers a GC pass against `cache_dir/pool/` (Phase 4).
Synchronously enqueues a job; the GC pipeline runs on its
existing worker.

Request body: ignored. Query parameters: none.

Job result body (on `done`):

```json
{
  "scanned_blobs": <int>,
  "unlinked_blobs": <int>,
  "freed_bytes": <int>,
  "duration_ms": <int>,
  "trigger": "manual"
}
```

The `trigger` field distinguishes manual GC from periodic GC in
metrics: `acu_gc_runs_total{trigger="manual"|"periodic"}`.

#### 2.2.2 POST /admin/cache/clear

Selects cache entries by exactly one of two query parameters:

- `?canonical_host=<host>` — drops every blob, every snapshot
  row, and every Release/Packages metadata entry whose
  `canonical_host` matches (case-insensitive).
- `?suite=<suite-key>` — drops every entry whose suite-key
  matches. The suite-key is the cache-internal canonical name
  (e.g. `ubuntu/jammy`).

Exactly one of the two MUST be present; presenting both, neither,
or a third selector returns `400 Bad Request`. Multi-selector
queries (`canonical_host=…&suite=…`) are deferred per §1.2.

Job result body (on `done`):

```json
{
  "selector": {"kind": "canonical_host"|"suite", "value": "<...>"},
  "rows_deleted": <int>,
  "blobs_marked": <int>,
  "duration_ms": <int>
}
```

`rows_deleted` is the number of SQLite rows removed across all
affected tables (sum of `snapshots`, `package_hash`, and
`current_snapshot_id` rows). `blobs_marked` is the number of
pool/ files marked for sweep on the next GC tick.

In-flight requests for the cleared entries complete naturally
(§6.5 — the singleflight leader keeps the file open until its
clients finish; the next request to the same canonical
(scheme, host, path) tuple takes the cache-miss path).

#### 2.2.3 POST /admin/suites/{path}/refresh

Forces a fresh adoption pass for the suite identified by `{path}`.
The path corresponds to the `sources.list` view (e.g.
`/ubuntu/dists/jammy`); the daemon resolves it to a suite-key
internally before dispatching to the existing
`internal/freshness/adoption.go` entry point.

Request body: ignored. Query parameters: none.

Path validation:

- The path MUST start with `/` and MUST be percent-decoded by
  the HTTP server before dispatch. Path segments containing `..`
  are rejected (`400 unsafe_path`). Trailing slashes are
  normalized (one or zero); both forms are accepted.
- The path MUST resolve to a known suite per the same logic
  Phase 1 / Phase 2 use for `Release`/`InRelease` lookup. An
  unknown path returns `404 Not Found` with body
  `{"error":"unknown suite","path":"<path>"}`.

Job result body (on `done`):

```json
{
  "suite": "<suite-key>",
  "snapshot_id_before": <int>,
  "snapshot_id_after": <int>,
  "package_count": <int>,
  "duration_ms": <int>
}
```

When the upstream Release is byte-identical to the live snapshot,
`snapshot_id_after` equals `snapshot_id_before`; this is the
"no-op refresh" case and the job still reports `done`.

#### 2.2.4 POST /admin/ca/rotate

Generates a new MITM CA keypair, atomically swaps it in, flushes
the leaf-cert cache, and moves the old keypair to
`cache_dir/ca/old/{fingerprint}/` for forensic retention.

Preconditions:

- `tls_mitm.enabled` MUST be `true`. With it `false`, the endpoint
  returns `503 Service Unavailable` with body
  `{"error":"mitm disabled"}`.
- The CA path must not be operator-supplied (i.e.
  `tls_mitm.ca_cert` and `tls_mitm.ca_key` are unset, so the
  daemon manages the keypair under `cache_dir/ca/`). With
  operator-supplied paths, the endpoint returns `409 Conflict`
  with body `{"error":"operator-supplied CA — rotate via ansible"}`.
  (Online rotation of an operator-supplied CA file would race the
  ansible push; multi-cache deployments rotate via ansible across
  all instances atomically.)
- No other `/admin/ca/rotate` job may be in `running` state. A
  second concurrent request returns `409 Conflict` with body
  `{"error":"rotate already in flight","job_id":"<original>"}`.

Request body: ignored. Query parameters: none.

Job result body (on `done`):

```json
{
  "old_fingerprint_sha256": "<hex>",
  "new_fingerprint_sha256": "<hex>",
  "new_not_after_unixtime": <unix>,
  "old_path": "ca/old/<old-fingerprint>/",
  "cert_cache_evicted": <int>,
  "duration_ms": <int>
}
```

The `cert_cache_evicted` count is the number of leaf-cert cache
entries flushed at rotation (§6.4). Operators distributing the
new CA cross-cache use this number to cross-check: every cache
should report evicting roughly the same population on rotate.

#### 2.2.5 POST /admin/config/reload

Re-reads the config file from `cache.listen`'s startup-time
config path, validates the entire result against the same rules
that startup uses, and applies the §5.4 reloadable-key subset to
the live config. Non-reloadable keys whose values changed in the
file are recorded in the job result but **not** applied; their
current daemon-live values continue to govern. A validation
failure on any key aborts the entire reload (live config
unchanged).

Preconditions:

- `[reload].allowed_keys` MUST be non-empty. With it empty, the
  endpoint returns `503 Service Unavailable` with body
  `{"error":"reload disabled"}`.
- No other `/admin/config/reload` job may be in `running` state.
  Concurrent request returns `409 Conflict`.

Request body: ignored. Query parameters: none.

Job result body (on `done`):

```json
{
  "applied": [
    {"key": "<dotted.key>", "old": <json-value>, "new": <json-value>}
  ],
  "ignored_non_reloadable": [
    {"key": "<dotted.key>", "old": <json-value>, "new": <json-value>}
  ],
  "duration_ms": <int>
}
```

When no reloadable keys changed and no non-reloadable keys
changed, both arrays are empty and the job reports `done` with
`duration_ms` reflecting just the validation pass.

### 2.3 Jobs surface

Read-only endpoints on the admin listener exposing job state.
Both require the **read realm** credential
(`admin.htpasswd_path`); a write-role credential cannot read
job state (it can only `POST` mutating endpoints).

#### 2.3.1 GET /admin/jobs

Returns the most recent jobs in start-time-descending order.

Query parameters:

- `?limit=<n>` — cap on number of entries returned (default
  `min(admin.job_retention, 100)`; max equal to
  `admin.job_retention`).
- `?state=<pending|running|done|failed>` — filter to one state.
  Without the parameter, all four states are returned.

Response body:

```json
{
  "jobs": [
    {
      "job_id": "<id>",
      "action": "<action-name>",
      "state": "<state>",
      "caller": "<credential-id>",
      "remote_addr": "<ip:port>",
      "started_at_unixtime": <unix>,
      "finished_at_unixtime": <unix-or-null>,
      "error": "<message-or-null>"
    },
    ...
  ],
  "retention": <admin.job_retention>
}
```

The `caller` field is the htpasswd username or bearer-token id
(see §5.3). `error` is null on `done`, populated on `failed`.

#### 2.3.2 GET /admin/jobs/{job_id}

Returns one job's full state including the action-specific
result body. Returns `404 Not Found` for unknown / aged-out
job IDs (job retention per §6.2.4).

Response body:

```json
{
  "job_id": "<id>",
  "action": "<action-name>",
  "state": "<state>",
  "caller": "<credential-id>",
  "remote_addr": "<ip:port>",
  "started_at_unixtime": <unix>,
  "finished_at_unixtime": <unix-or-null>,
  "error": "<message-or-null>",
  "result": {<action-specific-body-from-§2.2.x>}
}
```

`result` is `null` when `state` is `pending` or `running`;
populated when `done`; populated with partial best-effort data
when `failed` (per-action; rotate `failed` may have only the
old fingerprint if the new keypair generation failed).

### 2.4 Status JSON additions — delta over SPEC5 §10.4

The `GET /?format=json` payload (SPEC5 §10.4) gains one new
top-level section `action_surface` and one extension to the
existing `tls_mitm` section:

```json
{
  ...
  "action_surface": {
    "mutating_endpoints_enabled": <bool>,
    "reload_enabled": <bool>,
    "in_flight_jobs": [
      {"job_id":"<id>","action":"<n>","state":"<s>","caller":"<c>",
       "started_at_unixtime":<u>}
    ],
    "recent_jobs": [
      {"job_id":"<id>","action":"<n>","state":"<s>","caller":"<c>",
       "started_at_unixtime":<u>,"finished_at_unixtime":<u>,
       "outcome":"done|failed"}
    ],
    "job_retention": <int>
  },
  "tls_mitm": {
    ...existing fields per SPEC6 §10.4...,
    "rotation": {
      "last_rotated_at_unixtime": <unix-or-null>,
      "last_rotated_caller": "<credential-id-or-null>",
      "old_ca_count_on_disk": <int>
    }
  }
}
```

`in_flight_jobs` is the list of jobs in `pending` or `running`
state (typically empty; bounded by the natural concurrency
ceiling of one rotate + one reload + N gc/clear/refresh).
`recent_jobs` is bounded at 10 most-recent terminal jobs (the
same surface the §10.5 status HTML shows in its "Action surface"
section).

`tls_mitm.rotation.old_ca_count_on_disk` is the number of
fingerprint-named subdirectories under `cache_dir/ca/old/`. It
serves as a forensic indicator: if this number grows over time
without operator pruning, rotation history is being retained
(per Q9 — no automatic retention policy).

### 2.5 Status HTML additions — delta over SPEC5 §10.5

The status HTML page gains a new "Action surface" section
between "Listeners" and "TLS MITM" (when MITM is on) or between
"Listeners" and "Cache" (when MITM is off). The section renders:

- Whether mutating endpoints are enabled (boolean).
- Whether reload is enabled (boolean).
- A table of in-flight jobs: `{job_id, action, state, caller,
  started_at}`.
- A table of last 10 completed jobs:
  `{job_id, action, state, caller, started_at, finished_at,
  outcome}`. (Per Q13 — `caller` shown.)

The "TLS MITM" section gains a "Last rotation" sub-row:
timestamp + caller of the last rotate (or "never" if no rotate
has happened on this daemon process).

---

## 3. URL canonicalization (Remap) — unchanged

Carry forward from SPEC §3 / SPEC2 §3 / SPEC4 §3 / SPEC5 §3 /
SPEC6 §3. Phase 7 introduces no new canonicalization rules. The
`POST /admin/cache/clear?canonical_host=…` selector takes the
canonical host as input — the operator is responsible for passing
the post-Remap value, NOT the literal sources.list hostname. The
daemon does not re-Remap the input. (Operators who want to clear
by literal hostname run `apt-cacher-ultra remap <host>` first to
discover the canonical form; that subcommand is unchanged from
SPEC6 §14.)

---

## 4. Storage layout (delta over SPEC §4 / SPEC2 §4 / SPEC4 §4 / SPEC6 §4)

### 4.1 Jobs store — in-memory only

The daemon maintains an in-memory store of recent jobs. Schema
in code (no SQLite):

```go
type Job struct {
    ID              string    // 16-byte URL-safe-base64 random
    Action          string    // gc_run | cache_clear | suite_refresh | ca_rotate | config_reload
    State           string    // pending | running | done | failed
    Caller          string    // htpasswd username or bearer-token id
    RemoteAddr      string    // %s:%d of the requesting connection
    StartedAt       time.Time
    FinishedAt      time.Time // zero value while pending/running
    Error           string    // empty on done; populated on failed
    Result          any       // action-specific body per §2.2.x
    IdempotencyKey  string    // empty if request did not include one
}
```

Lifecycle:

- A `pending` job is created synchronously when a mutating
  endpoint receives a valid request. The HTTP response (`202
  Accepted`) is sent before the job worker dequeues it.
- A `running` job has been picked up by a worker. Each action
  has its own worker / dispatch — there is no shared queue.
- A `done` or `failed` job is terminal; `Result` and/or `Error`
  is populated; `FinishedAt` is set to wall-clock time at
  termination.

Concurrency caps:

| Action | Max concurrent | Lock |
|---|---|---|
| `gc_run` | 1 | shared with periodic GC ticker |
| `cache_clear` | 4 | per-canonical_host or per-suite implicit serialization |
| `suite_refresh` | per existing `freshness.max_concurrent_adoptions` | shared with periodic refresher |
| `ca_rotate` | 1 | new global mutex |
| `config_reload` | 1 | new global mutex |

A new job request that would exceed the cap returns `409 Conflict`
(§2.2). The cap exists to guarantee bounded memory on the jobs
store and to prevent operator scripts from triggering work
faster than the daemon can dispatch it.

### 4.2 Job retention

The store retains:

- All `pending` and `running` jobs (uncapped; the §4.1
  concurrency caps bound the in-flight set).
- The most recent `admin.job_retention` (default 100, §5.1)
  jobs in `done` or `failed` state, in start-time order.

When a new terminal job arrives and the cap is full, the
oldest terminal job is dropped silently (no log line, no
metric — the SPEC5 §10.4 ring-buffer pattern).

`admin.job_retention = 0` disables history entirely; only
in-flight jobs are visible via `GET /admin/jobs`. This is a
valid configuration for operators who scrape job state via
admin-action log lines instead.

### 4.3 Idempotency-key store

The (caller, key) → job_id mapping is held alongside the jobs
store in a separate dictionary, keyed by the `caller` issuing
the request (so that two operators with distinct credentials
can use the same idempotency-key string without colliding).

Lifecycle:

- Insert on first request with a given (caller, key).
- The mapping survives until the referenced job is dropped from
  the jobs store (§4.2) OR `admin.idempotency_window` elapses
  past the job's `FinishedAt`, whichever comes first.

A duplicate request for the same (caller, key) within the window
returns the same `202` body / `Location` header as the first
request, regardless of the original job's current state. (A
duplicate after the original job has reached `failed` does NOT
retry the action; the operator who wants to retry omits the
idempotency key or uses a fresh one.)

### 4.4 CA rotation on-disk layout — delta over SPEC6 §4.2

Phase 6 wrote the auto-generated CA to `cache_dir/ca/ca.cert`
and `cache_dir/ca/ca.key`. Phase 7 keeps this convention as the
**live** path and adds a `cache_dir/ca/old/` directory for
historical CAs.

```
cache_dir/
  ca/
    ca.cert                              # current CA cert (live)
    ca.key                               # current CA key (live)
    .lock                                # flock per SPEC6 §4.2.1
    old/
      <fingerprint-prefix-hex>/
        ca.cert                          # rotated-out CA cert
        ca.key                           # rotated-out CA key
        rotated_at                       # ISO-8601 timestamp text file
      <fingerprint-prefix-hex>/
        ...
```

The fingerprint-prefix-hex is the SHA-256 of the rotated-out CA
cert in lowercase hex, **first 16 hex chars** (8 bytes / 64 bits).
This is enough to disambiguate any plausible rotation history
while keeping the directory name short. Collisions on the
prefix (statistically near-zero) are an error: the daemon
refuses to overwrite an existing `old/<prefix>/` directory and
the rotation job fails with `error: "fingerprint prefix
collision"`.

The daemon does NOT load or read files under `old/` during
normal operation. Operators rolling back run an out-of-band
recovery: copy `old/<prefix>/ca.{cert,key}` back to the live
path, restart the daemon. (A future "rotate `--from
old/<prefix>/`" subcommand is deferred per §1.2.)

No automatic retention policy: `cache_dir/ca/old/` grows
without bound. Operators who care prune manually. The
`tls_mitm.rotation.old_ca_count_on_disk` status field surfaces
the count for observability.

### 4.5 SQLite schema — unchanged

Phase 7 makes no schema changes. The Phase 4 GC tables, the
Phase 3 snapshot tables, and the Phase 2 by-hash dedup tables
are all unchanged. Cache-clear operations remove rows from
existing tables; they do not create new ones.

---

## 5. Configuration (delta over SPEC §5 / SPEC2 §5 / SPEC4 §5 / SPEC5 §5 / SPEC6 §5)

### 5.1 `[admin]` block additions

Phase 5's `[admin]` block (SPEC5 §5.1) gains four new fields:

```toml
[admin]
# --- Phase 5 fields, unchanged ---
listen                      = "127.0.0.1:9090"
htpasswd_path               = ""              # read-only realm
read_timeout                = "5s"
idle_timeout                = "30s"
gauge_refresh               = "5s"
metric_series_cap           = 1024

# --- Phase 7 additions ---
mutating_htpasswd_path      = ""              # write-role realm; empty = mutating endpoints disabled
mutating_bearer_tokens      = []              # opt-in; list of "<token-id>:<secret>" strings
job_retention               = 100             # max remembered terminal jobs
idempotency_window          = "5m"            # idempotency-key TTL after job termination
```

Field semantics:

- **`mutating_htpasswd_path`** — Filesystem path to a bcrypt
  htpasswd file (same format as `admin.htpasswd_path`). When
  non-empty AND distinct from `htpasswd_path`, mutating endpoints
  authenticate against this file. When empty AND
  `mutating_bearer_tokens` is empty, all mutating endpoints
  return `503` (§1.1).

- **`mutating_bearer_tokens`** — List of `<id>:<secret>` strings.
  `<id>` is operator-chosen, must match `^[A-Za-z0-9_-]{1,64}$`
  (used as audit-log `caller` and Prometheus label value).
  `<secret>` is the actual token bytes (length ≥ 32 chars
  required). A request with `Authorization: Bearer <secret>`
  matches if any list entry's secret matches in
  constant time (`subtle.ConstantTimeCompare`). The id is
  reported as the `caller` in audit logs and metrics; the
  secret is never logged. Empty list disables bearer auth.

- **`job_retention`** — Maximum number of terminal jobs to keep
  in memory (§4.2). `0` means "no history; only in-flight";
  values > 100000 are rejected at config validation as a
  safety bound on memory cost.

- **`idempotency_window`** — Duration the daemon retains the
  idempotency-key → job_id mapping after the referenced job
  reaches a terminal state (§4.3). Default 5 minutes;
  operators tuning to 0 disable idempotency entirely.

### 5.2 `[reload]` block (NEW)

```toml
[reload]
allowed_keys = [
  "upstream.allowed_host_regex",
  "tls_mitm.allowed_host_regex",
  "freshness.periodic_refresh",
  "gc.interval",
  "log_level",
]
```

Field semantics:

- **`allowed_keys`** — List of dotted config keys that are
  applied during a hot-reload (`SIGHUP` or
  `POST /admin/config/reload`). Empty list disables both reload
  paths. Any key in this list MUST be from the §5.4 reloadable-
  subset table; an entry for a non-reloadable key fails config
  validation at startup with
  `non_reloadable_key_in_allowed_keys` error.

The default value (above) is the maximally-populated set per
§5.4. Operators who want to narrow which keys reload (e.g. allow
log-level changes but not regex changes) edit the list.

### 5.3 Validation rules (delta over SPEC5 §5.4)

New startup config-error fail-closed cases:

- **`htpasswd_paths_collide`** — `admin.htpasswd_path` and
  `admin.mutating_htpasswd_path` are non-empty AND identical.
  Configuring the same file as both realms would silently
  collapse the realm split. Daemon refuses to start.
- **`bearer_token_invalid_id`** — Any `<id>:<secret>` entry's
  `<id>` portion fails the regex
  `^[A-Za-z0-9_-]{1,64}$`. Daemon refuses to start.
- **`bearer_token_secret_too_short`** — Any `<id>:<secret>`
  entry's `<secret>` portion is shorter than 32 chars. Daemon
  refuses to start.
- **`bearer_token_id_collision`** — Two entries in
  `mutating_bearer_tokens` share the same `<id>`. Daemon
  refuses to start.
- **`mutating_realm_unconfigured_but_used`** — At least one
  field in the §5.4 reloadable subset is non-empty (e.g.
  `[reload].allowed_keys` is non-default), AND
  `mutating_htpasswd_path` and `mutating_bearer_tokens` are both
  empty. This is an inconsistency Warn (not a startup failure):
  reload is configured but unreachable via HTTP because no
  write-role credential exists. SIGHUP still works.
  Logged as `mutating_realm_inconsistent` Warn at startup.
- **`reload_key_invalid`** — `[reload].allowed_keys` contains
  a key not in §5.4's reloadable list. Daemon refuses to
  start with the offending key named.
- **`job_retention_too_high`** — `admin.job_retention > 100000`.
  Daemon refuses to start.

### 5.4 Hot-reloadable subset

The complete set of config keys that may appear in
`[reload].allowed_keys` and that the reload pipeline (§6.3)
will apply to the live config:

| Key | Reload apply-when | Reload validation |
|---|---|---|
| `upstream.allowed_host_regex` | Next request consults the new regex | RE2 compile + IDNA-validation rules per Phase 1 §6.6 |
| `tls_mitm.allowed_host_regex` | Next CONNECT signing gate consults new regex | SPEC6 §5.1 rules: RE2 compile + §5.1.1.1 anchor symmetry; if Name Constraints derivation fails, reload aborts (live constraints unchanged) |
| `upstream.deny_target_ranges` | Next outbound TCP connect consults new list | Per Phase 1 §5.3: parse CIDRs |
| `freshness.periodic_refresh` | Refresher restarts on next natural tick | Duration parse; must be > `freshness.min_jitter` |
| `freshness.max_concurrent_adoptions` | Adoption semaphore resizes on next acquire | Integer ≥ 0 |
| `gc.interval` | GC ticker re-arms on next tick | Duration parse; must be > 0 |
| `gc.dry_run` | Next GC tick respects new value | Boolean |
| `log_level` | slog handler swaps level immediately | Enum: `debug|info|warn|error` |
| `upstream.connect_timeout` | Next request uses new timeout | Duration parse; > 0 |
| `upstream.total_timeout` | Next request uses new timeout | Duration parse; > 0; ≥ `connect_timeout` |
| `upstream.max_retries` | Next request uses new value | Integer ≥ 0 |

**Explicitly NOT reloadable** (changes to these in the file
during reload are recorded as `ignored_non_reloadable` per
§2.2.5 but not applied):

- `cache.listen`, `cache.listen_tls`, `admin.listen` —
  listener restart required.
- `cache.tls_cert`, `cache.tls_key` — TLS cert reload would
  require `tls.Config.GetCertificate` plumbing not yet in
  place (deferred to a future phase).
- `tls_mitm.enabled` — toggling on/off requires re-wiring the
  CONNECT dispatcher.
- `tls_mitm.ca_cert`, `tls_mitm.ca_key` — CA changes must use
  rotate (online) or restart (offline).
- `tls_mitm.allow_unconstrained_ca` — the SPEC6 §5.1.1.1 fail-
  closed semantic depends on this being known at CA load time.
- `tls_mitm.leaf_cert_lifetime`, `tls_mitm.ca_cert_lifetime`,
  `tls_mitm.leaf_algorithm`, `tls_mitm.cert_cache_size` — these
  affect cert generation and the cert cache; resizing the cache
  online is doable but deferred.
- `cache.cache_dir` — storage identity invariant.
- `[adoption]` schema-related keys — migration concern.
- `admin.htpasswd_path`, `admin.mutating_htpasswd_path`,
  `admin.mutating_bearer_tokens` — credential changes during
  reload would race in-flight authenticated requests.
- `admin.job_retention`, `admin.idempotency_window` —
  resizing the in-memory store mid-flight is a future
  enhancement; not required.
- `[reload].allowed_keys` itself — bootstrapping concern; a
  reload that changes its own gate is meta-confusing. To
  change the allowlist, restart.

A reload request whose file content changes a non-reloadable key
DOES NOT fail. The new value is recorded in
`ignored_non_reloadable` (§2.2.5 result body); the live value is
unchanged. Operators who intend to apply a non-reloadable key
must restart the daemon — the reload result body makes the
divergence visible.

### 5.5 Default config block additions

`packaging/config/config.toml.default` gains the four `[admin]`
fields and the new `[reload]` block, all commented with operator
guidance:

```toml
[admin]
# ... Phase 5 fields ...

# --- Phase 7 ---
# Mutating-endpoints credentials: bcrypt htpasswd file (same format
# as htpasswd_path above). Empty = mutating endpoints disabled.
# Must NOT be the same path as htpasswd_path; the read and write
# realms must use distinct credentials.
mutating_htpasswd_path = ""

# Mutating-endpoints bearer tokens (opt-in alongside or instead of
# htpasswd). Each entry is "<token-id>:<secret>"; the id is the
# audit-log caller string and the secret is the bearer token.
# Token id format: ^[A-Za-z0-9_-]{1,64}$. Secret length: ≥ 32 chars.
# Example: ["ansible-vault:Wd8s7G3Hqr...", "rotation-bot:..."]
mutating_bearer_tokens = []

# Maximum number of terminal jobs retained in memory.
# 0 = no history (only in-flight visible).
job_retention = 100

# How long the idempotency-key cache remembers a (caller, key)
# pair after the referenced job finishes. 0 = idempotency disabled.
idempotency_window = "5m"

# --- NEW Phase 7 block ---
[reload]
# Config keys that SIGHUP / POST /admin/config/reload will apply.
# Empty list disables reload entirely.
allowed_keys = [
  "upstream.allowed_host_regex",
  "tls_mitm.allowed_host_regex",
  "freshness.periodic_refresh",
  "gc.interval",
  "log_level",
]
```

---

## 6. Request handling (delta over SPEC §6 / SPEC2 §6 / SPEC3 §6 / SPEC4 §6 / SPEC5 §6 / SPEC6 §6)

### 6.1 Auth realm split

Every request to the admin listener (`admin.listen`) is
classified by HTTP method into one of two realms:

- **Read realm.** `GET`/`HEAD` requests for `/`, `/?format=json`,
  `/metrics`, `/admin/events`, `/admin/jobs`, `/admin/jobs/{id}`.
  Requires a credential matching `admin.htpasswd_path`. If
  `htpasswd_path` is empty, read realm is unauthenticated (the
  Phase 5 default).
- **Write realm.** `POST` requests for any `/admin/*` path.
  Requires a credential matching `admin.mutating_htpasswd_path`
  OR a bearer token in `admin.mutating_bearer_tokens`. If both
  are empty, the write realm is **closed** (every POST returns
  `503`).

Cross-realm credential rejection:

- A read-realm credential that authenticates against
  `htpasswd_path` MUST NOT satisfy a write-realm request. The
  middleware checks the request's HTTP method first, dispatches
  to the realm-specific authenticator, and never falls back. A
  read-only htpasswd entry on a `POST` request returns `401`
  (not `403`) — the same as no credential at all — so a curl
  user who fat-fingers the htpasswd doesn't accidentally see
  "your credential exists but is wrong realm" leakage.
- A write-realm credential on a `GET` request is the symmetric
  case: `401`. (A leaked rotate credential cannot scrape
  `/metrics`.)

WWW-Authenticate header:

- Read realm: `Basic realm="apt-cacher-ultra (read)"`.
- Write realm:
  - If `mutating_htpasswd_path` is set:
    `Basic realm="apt-cacher-ultra (write)"`.
  - If only `mutating_bearer_tokens` is set:
    `Bearer realm="apt-cacher-ultra (write)"`.
  - If both are set, both header values are emitted (HTTP/1.1
    allows comma-separated challenges; clients pick whichever
    they support):
    `Basic realm="apt-cacher-ultra (write)", Bearer realm="apt-cacher-ultra (write)"`.

Bearer token authentication:

- The header `Authorization: Bearer <secret>` is parsed; `<secret>`
  is compared against the secret portion of every entry in
  `mutating_bearer_tokens` using `subtle.ConstantTimeCompare`.
  First match wins; the matched entry's `<id>` becomes the
  `caller` field in audit logs and the `caller` Prometheus
  label value.
- A bearer secret that does not match any entry returns `401`
  with the standard headers; no log line beyond the existing
  Phase 5 access log (no audit log for failed auth — that would
  invert the audit logic, since unauthenticated requests are
  not actions).

Audit logging on auth success:

- The first event emitted after successful auth (and before the
  job worker starts) is `admin_action_started` (§10.1).
- The `caller` field on this event is the htpasswd username, or
  the bearer-token id, depending on which matched.

### 6.2 Jobs lifecycle

```
1. Mutating-endpoint handler receives valid request.
2. Authenticate against the appropriate realm (§6.1).
3. Check idempotency-key store (§4.3) for an existing
   (caller, key) → job_id mapping. If present and the referenced
   job still exists, return the original 202 response.
4. Validate request parameters. On failure return 400 with the
   error body BEFORE creating a job.
5. Check action-specific concurrency caps (§4.1). On overflow
   return 409 Conflict BEFORE creating a job.
6. Create a new Job{State: pending}. Insert into the jobs store.
   Insert into the idempotency-key store if a key was provided.
7. Emit admin_action_started Info (§10.1).
8. Send HTTP 202 with Location header to the client.
9. The action's worker (per §6.2.x) picks up the job, sets
   State: running, dispatches to the underlying entry point.
10. On terminal state (done | failed):
    a. Set FinishedAt, Result, Error.
    b. Emit admin_action_completed Info.
    c. Increment acu_admin_action_total{action,outcome,caller}.
    d. If terminal-jobs cap (admin.job_retention) is exceeded,
       drop oldest terminal job from store. (Idempotency-key
       mapping for the dropped job is invalidated at the same
       moment; subsequent requests with the same key produce a
       new job.)
```

The worker for each action is one of:

- **gc_run** — Submits to the existing Phase 4 GC pipeline via a
  new `gc.RunNow(ctx, "manual")` entry point. The GC pipeline's
  internal mutex serializes manual against periodic.
- **cache_clear** — Single-table SQLite transaction: DELETE rows
  matching the selector across `snapshots`, `package_hash`,
  `current_snapshot_id`, and the §6.4 SPEC2 `metadata` table;
  WAL fsync; commit. Pool/ blob unlinking happens on the next
  GC tick.
- **suite_refresh** — Calls
  `freshness.RefreshSuite(ctx, suite_key, "manual")` which
  takes the same `max_concurrent_adoptions` semaphore as the
  periodic refresher. If the semaphore is full, the worker
  waits up to `freshness.refresh_queue_timeout` (default 30s);
  on timeout the job fails with
  `error: "adoption semaphore full"`.
- **ca_rotate** — §6.4. Holds the global `caRotateMutex`.
- **config_reload** — §6.3. Holds the global `configReloadMutex`.

#### 6.2.1 Job worker shutdown

On graceful shutdown (`ctx.Done()` from main):

- Workers in `running` state observe ctx cancel and return
  promptly. The job transitions to `failed` with
  `error: "context canceled"`.
- Workers in `pending` state (queue waiters) drop their pending
  work; the job transitions to `failed` with
  `error: "shutdown before dispatch"`.
- An `admin_job_orphaned` Info is emitted for each pending job
  dropped at shutdown.
- The HTTP-level 202 has already been sent to the client; the
  client polling `/admin/jobs/{id}` learns of the failure
  through the job state.

#### 6.2.2 Job retention and dropping

When `admin.job_retention` is exceeded by a new terminal job:

- The oldest terminal job (by `FinishedAt`) is dropped from
  the in-memory store.
- The idempotency-key mapping for the dropped job is also
  invalidated.
- No log line; this is the SPEC5 §10.4 ring-buffer pattern.

A `GET /admin/jobs/{id}` for a dropped job returns `404 Not
Found` with body `{"error":"job aged out"}`.

### 6.3 Reload pipeline

`SIGHUP` and `POST /admin/config/reload` share one pipeline:

```
1. Acquire configReloadMutex (non-blocking; if held, request
   returns 409 / SIGHUP returns immediately with
   reload_already_in_flight Info).
2. Re-read the config file from the path the daemon was
   started with (held in process-startup state). I/O failure
   (file gone, permission denied) → reload aborts; live config
   unchanged; emit reload_failed Warn with error.
3. Parse + validate the entire file using the same rules
   startup uses. Validation failure → abort with the same
   error message startup would have produced; emit
   reload_failed Warn.
4. Compute the diff against the live config:
   - For each key in [reload].allowed_keys: record old → new
     in the applied list (omit if unchanged).
   - For each key NOT in [reload].allowed_keys: record old →
     new in the ignored_non_reloadable list (omit if unchanged).
5. For each key in applied list, atomic-swap into the live
   config behind atomic.Pointer[Config] (§6.3.1). All swaps
   happen under the same mutex; consumers Load() the pointer
   per request, so a request-in-flight either sees the old
   config or the new — never a half-state.
6. Fire reload-side-effects:
   - Refresher: signal periodic_refresh ticker to re-arm.
   - GC: signal gc.interval ticker to re-arm.
   - slog handler: swap log level if log_level changed.
   - Regex consumers: nothing — they Load() per request.
7. Emit reload_applied Info with the applied list (§10.2).
8. Release configReloadMutex.
9. For SIGHUP: nothing further. For HTTP: the job transitions
   to done with the result body.
```

#### 6.3.1 Live-config atomic pointer

The daemon holds the live config as `atomic.Pointer[Config]`
(initialized at startup, swapped only by the reload pipeline).
Every consumer that reads config does so through `cfg.Load()`,
which is wait-free.

The pointer swap is the linearization point: a request that
Loads before the swap sees the old config; a request that
Loads after sees the new. Requests in-flight at the swap moment
see whichever pointer they captured first; this is acceptable
because the reloadable subset is intentionally restricted to
keys whose mid-request change is benign (regex consulted once
per request; ticker intervals are amortized).

### 6.4 CA rotation flow

`POST /admin/ca/rotate` and `apt-cacher-ultra ca rotate`
(§14.2) call the same `tlsmitm.Rotate(ctx)` entry point:

```
1. Acquire caRotateMutex (non-blocking; if held, return 409 /
   subcommand exits 1 with rotate_in_flight).
2. Acquire the existing Phase 6 §4.2.1 flock on cache_dir/ca/.
   (Online rotate: if flock cannot be acquired within
   tls_mitm.ca_rotate_lock_timeout, default 30s, abort with
   ca_lock_timeout error. Subcommand: same timeout.)
3. Capture old CA fingerprint.
4. Generate new CA keypair per SPEC6 §5.1.1 / §5.1.2 rules
   (Name Constraints derivation included; same fail-closed
   semantics: empty regex + allow_unconstrained_ca=false →
   rotate fails with mitm_ca_unconstrained_refused).
5. Atomic-write new keypair to a temp path under
   cache_dir/ca/, then rename to cache_dir/ca/.tmp/ca.{cert,key}
   (the temp pair lives alongside the live pair during the
   atomic switchover).
6. Atomic-swap the in-memory CA pointer (atomic.Pointer[CA])
   to the new keypair. From this moment, all new CONNECTs
   sign with the new CA.
7. Flush the leaf-cert cache. Record the eviction count for
   the result body. (Existing TLS sessions on the wire are
   unaffected — they completed handshake under the old leaf
   already.)
8. Move the old live keypair to
   cache_dir/ca/old/<old-fingerprint-prefix>/. (rename, not
   copy — atomic on POSIX.)
9. Move the new keypair from cache_dir/ca/.tmp/ to the live
   path cache_dir/ca/ca.{cert,key}. (rename, atomic.)
10. Release flock.
11. Emit mitm_ca_rotated Info (§10.1) with old + new
    fingerprints, eviction count, caller (HTTP path) or
    "subcommand" (CLI path).
12. Release caRotateMutex.
13. HTTP path: job transitions to done with result body per
    §2.2.4. CLI path: subcommand exits 0 and prints the new
    fingerprint to stdout.
```

#### 6.4.1 Rotation failure paths

- New keypair generation fails (entropy, system error) →
  rollback: delete the temp path; nothing changes; live CA
  unchanged. Job fails with `error: "keypair generation
  failed: <detail>"`.
- Disk full during atomic-write of new keypair → rollback as
  above. Job fails with `error: "disk full"`.
- Name Constraints derivation fails (regex too complex) AND
  `allow_unconstrained_ca=false` → rollback. Job fails with
  `error: "mitm_ca_unconstrained_refused"`.
- Rename to old/ fails → the new keypair is live but the old
  one is still at the live path under a different name. The
  daemon emits `mitm_ca_rotation_old_archive_failed` Warn with
  the disk error; rotation succeeds (live is the new CA);
  operator must manually move the orphan file. Subsequent
  rotates skip the archive step if the target prefix already
  exists.
- Daemon crashes between swap and archive → on restart the
  daemon loads the live keypair (which is the new one); the
  un-archived old keypair is still at the live path under the
  daemon's previous filename for that path. The daemon does
  not see it (the live filename is now governed by the new
  keypair). Operator-visible artifact, no functional impact.
  Document in §11.

#### 6.4.2 Rotation under operator-supplied CA

If `tls_mitm.ca_cert` and `tls_mitm.ca_key` are non-empty (the
operator-supplied path per SPEC6 §5.1), `tlsmitm.Rotate` returns
`error: "operator-supplied CA — rotate via ansible"` immediately
and no keypair is generated.

### 6.5 Cache-clear under live traffic

`cache_clear` workers operate on SQLite rows; they do not
unlink pool/ files synchronously. Concurrent requests for
cleared entries:

- A request that arrived before the clear's transaction
  commits sees the rows; the singleflight leader keeps the
  open file descriptor; clients sharing that singleflight
  read the bytes through the open FD (POSIX semantics —
  unlink does not invalidate open FDs).
- A request that arrives after the commit sees no rows;
  takes the cache-miss path; fetches upstream; the next GC
  tick unlinks the orphaned blobs.

The job reports `done` when the SQLite transaction commits
(synchronous fsync). The blob count in the result body is the
*marked-for-sweep* count — the actual unlink happens on the
next GC tick. Operators who want the unlinks to be immediate
follow up with `POST /admin/gc/run`.

### 6.6 Suite refresh under live traffic

`suite_refresh` workers call the existing
`freshness.RefreshSuite` entry point. Concurrent client
requests for the suite during the refresh window see the old
snapshot until the adoption transaction commits the new one;
the SPEC2 atomic-snapshot-flip guarantees no torn read. After
commit, subsequent client requests see the new snapshot.

---

## 7. Freshness and adoption — minor delta over SPEC2 §7 / SPEC3 §7

### 7.1 New entry point: `RefreshSuite`

`internal/freshness/adoption.go` exports a new function:

```go
func RefreshSuite(ctx context.Context, suiteKey string, trigger string) (RefreshResult, error)
```

`trigger` is one of `"periodic"` (called by the existing
ticker) or `"manual"` (called by `cache_clear`/`suite_refresh`
workers). The function:

- Acquires the `freshness.max_concurrent_adoptions` semaphore.
- Dispatches to the existing adoption pipeline.
- Returns the same result the periodic refresher consumes
  (snapshot ID before + after, package count, error).
- Emits the existing `adoption_*` log family (no Phase 7
  changes); the trigger appears as a new field
  `trigger=manual|periodic`.

The periodic refresher is refactored to call this function
instead of inlining the adoption call. No external behavior
change for periodic refreshes.

### 7.2 Periodic refresh interval reload

When `freshness.periodic_refresh` reloads (§6.3), the existing
ticker is stopped and a fresh one is created with the new
interval. In-flight adoptions continue under the old
semaphore; new adoptions wait on the new ticker tick.

### 7.3 Everything else — unchanged

Adoption pipeline internals, snapshot promotion, GPG
verification, by-hash dedup — all carry forward from
SPEC2 / SPEC3 unchanged.

---

## 8. Stale-and-Valid-Until — unchanged

Carry forward from SPEC2 §8. Phase 7 changes nothing here.
`POST /admin/cache/clear` removes rows; it does NOT modify the
stale-validity decision logic.

---

## 9. Concurrency & deadlines (delta over SPEC §9 / SPEC2 §9 / SPEC3 §9 / SPEC4 §9 / SPEC5 §9 / SPEC6 §9)

### 9.1 Jobs concurrency

Two new mutexes, both per-process (no SQLite lock dependency):

- **`caRotateMutex`** — One CA-rotation at a time. Held from
  job creation through job completion (§6.4). HTTP requests
  observing held mutex return `409 Conflict` (§2.2.4).
- **`configReloadMutex`** — One config reload at a time. Held
  from reload start through completion (§6.3). HTTP requests
  return `409`. SIGHUP handlers return without scheduling work.

The action-specific caps (§4.1) are enforced separately by
each action's worker:

- `gc_run` cap is enforced by the existing GC pipeline mutex
  (one GC pass at a time, manual or periodic).
- `cache_clear` cap of 4 is a new bounded semaphore in the
  cache-clear worker.
- `suite_refresh` cap is the existing
  `freshness.max_concurrent_adoptions` semaphore.

### 9.2 Reload deadlock avoidance

The reload pipeline must NOT acquire any lock other than
`configReloadMutex` while holding it. Specifically:

- It does not call into the GC pipeline.
- It does not call into the freshness pipeline.
- It does not call into `tlsmitm.Rotate`.

Side effects (ticker re-arm, log-level swap, pointer-swap of
live config) are designed to be lock-free or tail-call-after-
release. A reload that wanted to mutate one of the above
subsystems would invert the lock ordering; the design forbids
this.

### 9.3 CA rotate deadlock avoidance

`tlsmitm.Rotate` takes:

1. `caRotateMutex` (this spec, §9.1).
2. The Phase 6 §4.2.1 flock on `cache_dir/ca/`.

Both are non-blocking with a timeout. A second concurrent
rotate observes step 1 held and returns `409` immediately.

The leaf-cert cache flush (§6.4 step 7) takes the cert-cache
mutex briefly. The atomic-pointer swap (§6.4 step 6) is lock-
free. No other system lock is acquired.

### 9.4 Startup ordering — delta over SPEC6 §9.5

The Phase 6 startup sequence becomes:

```
1. Validate config (SPEC6 §9.5 step 1).
2. Materialize TLS MITM CA via tlsmitm.LoadOrGenerate
   (SPEC6 §9.5 step 2).
3. Bind cache.listen (plain proxy listener).
4. Bind cache.listen_tls (TLS proxy listener), if configured.
5. Bind admin.listen, if configured.
6. Validate auth realms:
   a. Read realm: open htpasswd_path; failure → fail-closed.
   b. Write realm: if mutating_htpasswd_path is set, open it;
      failure → fail-closed. Validate
      htpasswd_paths_collide (§5.3) failure → fail-closed.
      Validate bearer-token entries → fail-closed.
   c. Emit admin_realm_summary Info with read-realm-source,
      write-realm-source ("htpasswd"|"bearer"|"both"|"closed"),
      bearer-token-count.
7. Open cache (SQLite + pool/) (SPEC6 §9.5 step 6).
8. Run Phase 4 orphan repair (SPEC6 §9.5 step 7).
9. Initialize jobs store (in-memory; empty).
10. Initialize idempotency-key store (in-memory; empty).
11. Wire SIGHUP handler (calls reload pipeline §6.3).
12. Start admin gauge refresher (Phase 5).
13. Accept on all listeners.
```

Step 6 is new in Phase 7. A failure in step 6 stops the daemon
(no `Accept`), preventing the daemon from running with broken
auth — same fail-closed pattern as Phase 6 step 2.

### 9.5 Shutdown ordering — delta over SPEC6 §9.5

Phase 6 graceful shutdown:

```
1. Receive SIGTERM / context cancel.
2. Stop accepting new connections (close listeners).
3. Drain in-flight CONNECT tunnels (SPEC6 §9.5).
4. Drain in-flight inner GETs.
5. Stop refresher / GC tickers.
6. Close SQLite.
7. Exit.
```

Phase 7 inserts step 3.5 between 3 and 4:

```
3.5. Cancel ctx for all in-flight admin jobs:
     a. running jobs observe cancel, return promptly,
        transition to failed with context canceled.
     b. pending jobs (queue waiters) drop work,
        transition to failed with shutdown before dispatch.
     c. Emit admin_job_orphaned Info per dropped job.
     d. Drain the per-action worker goroutines (max wait =
        the per-action shutdown grace, shared with §6.2.1).
```

The CA rotate worker, if running at shutdown, attempts to
finish the atomic-write step (§6.4 steps 5-9) before yielding
to ctx cancel. The rotate is either fully applied (live = new
CA, old archived) OR not started; an interrupted rotate
between steps 6 (pointer swap) and 9 (rename new to live)
leaves a recoverable state per §6.4.1.

The config reload worker, if running at shutdown, completes
the pointer swap synchronously and yields. A reload
interrupted in step 1 is treated as not-started.

---

## 10. Logging (delta over SPEC §10 / SPEC2 §10 / SPEC3 §10 / SPEC4 §10 / SPEC5 §10 / SPEC6 §10)

### 10.1 New event family: `admin_action_*`

Every mutating endpoint produces two log lines: one on receipt
(after auth, before worker dispatch) and one on terminal state.

`admin_action_started` (Info):

```json
{
  "event": "admin_action_started",
  "action": "gc_run|cache_clear|suite_refresh|ca_rotate|config_reload",
  "job_id": "<id>",
  "caller": "<htpasswd-username-or-bearer-id>",
  "remote_addr": "<ip:port>",
  "idempotency_key": "<key>" or omitted,
  "selector": {...} or omitted,    // for cache_clear / suite_refresh
  "received_at_unixtime": <unix>
}
```

`admin_action_completed` (Info on `done`, Warn on `failed`):

```json
{
  "event": "admin_action_completed",
  "action": "<as-above>",
  "job_id": "<id>",
  "caller": "<as-above>",
  "outcome": "done|failed",
  "duration_ms": <int>,
  "error": "<message>" or omitted,
  "result": {<action-specific>} or omitted
}
```

The `result` field is the same JSON body returned by
`GET /admin/jobs/{id}` (§2.3.2). It is omitted for
`outcome=failed` cases where partial result data is not
informative.

Subordinate events emitted during a job:

- `admin_job_dropped` (Info) — A pending job was dropped at
  graceful shutdown. Fields: `job_id`, `action`, `caller`.
  Synonym for §6.2.1 `admin_job_orphaned`; one of the two
  names is normative — choose at implementation time, document
  in code.

### 10.2 Reload event

`reload_applied` (Info):

```json
{
  "event": "reload_applied",
  "trigger": "sighup|http",
  "caller": "<bearer-id-or-htpasswd-user>" or "signal" for sighup,
  "applied": [
    {"key": "upstream.allowed_host_regex",
     "old": "^foo$", "new": "^foo$|^bar$"},
    ...
  ],
  "ignored_non_reloadable": [
    {"key": "cache.listen", "old": ":3142", "new": ":3143"},
    ...
  ],
  "duration_ms": <int>
}
```

The event is emitted exactly once per reload; if both `applied`
and `ignored_non_reloadable` are empty, the event is still
emitted (audit trail of "operator triggered reload, nothing
changed").

`reload_failed` (Warn):

```json
{
  "event": "reload_failed",
  "trigger": "sighup|http",
  "caller": "<as-above>",
  "stage": "io|parse|validate|atomic_swap",
  "error": "<message>"
}
```

`reload_during_shutdown_ignored` (Info):

```json
{
  "event": "reload_during_shutdown_ignored",
  "trigger": "sighup",
  "received_at_unixtime": <unix>
}
```

`reload_already_in_flight` (Info):

```json
{
  "event": "reload_already_in_flight",
  "trigger": "sighup",
  "in_flight_job_id": "<id>"
}
```

### 10.3 CA rotation events

`mitm_ca_rotated` (Info):

```json
{
  "event": "mitm_ca_rotated",
  "old_fingerprint_sha256": "<hex>",
  "new_fingerprint_sha256": "<hex>",
  "new_not_after_unixtime": <unix>,
  "cert_cache_evicted": <int>,
  "caller": "<bearer-id-or-htpasswd-user-or-subcommand>",
  "duration_ms": <int>
}
```

`mitm_ca_rotate_failed` (Warn):

```json
{
  "event": "mitm_ca_rotate_failed",
  "stage": "lock|generate|atomic_write|swap|archive",
  "old_fingerprint_sha256": "<hex>",
  "error": "<message>",
  "caller": "<as-above>"
}
```

`mitm_ca_rotation_old_archive_failed` (Warn) per §6.4.1:

```json
{
  "event": "mitm_ca_rotation_old_archive_failed",
  "old_fingerprint_sha256": "<hex>",
  "stranded_path": "<live-path>",
  "error": "<message>"
}
```

### 10.4 New metrics

```
acu_admin_action_total{action,outcome,caller}     counter
acu_admin_jobs_inflight{action}                    gauge
acu_admin_jobs_dropped_total{reason}                counter   # reason: shutdown | retention
acu_config_reloads_total{trigger,outcome}          counter   # trigger: sighup | http; outcome: applied | failed | ignored_no_change
acu_mitm_ca_rotations_total{outcome}                counter   # outcome: rotated | failed
acu_mitm_ca_rotation_last_unixtime                  gauge     # 0 if never rotated this process
```

Cardinality budget per Phase 5 §10.4:

- `action` is a closed enum (5 values).
- `outcome` is a closed enum (`done` or `failed`).
- `caller` is bounded by the htpasswd file + bearer-token list;
  expected ≤ 10 for typical deployments. Per §1.3 (Q7) no
  additional cap beyond `metric_series_cap = 1024`.
- `reason` is a closed enum (2 values).
- `trigger` is a closed enum (2 values).

### 10.5 Status page additions

Per §2.5, the §10.5 status page gains an "Action surface"
section. Layout:

```
Action surface
==============
Mutating endpoints: enabled (write realm: htpasswd | bearer | both | closed)
Reload:             enabled (5 keys allowlisted)
Job retention:      100

In-flight jobs:
  job_id            action        state    caller          started_at
  3K2k...           gc_run        running  ansible-vault   2026-05-09T14:23:01Z

Recent jobs (last 10):
  job_id            action        outcome  caller          finished_at
  abcd...           ca_rotate     done     subcommand      2026-05-09T13:50:12Z
  ...
```

Under "TLS MITM" (when enabled) a "Last rotation" sub-row:
`Last rotation: 2026-05-09T13:50:12Z by subcommand` (or
`Last rotation: never` if no rotate has run on this process).

### 10.6 Existing event families — additive only

The Phase 4 `gc_*` events, Phase 3 `adoption_*` events, and
Phase 6 `mitm_*` events all gain a new `trigger` field
distinguishing periodic from manual when relevant. Specifically:

- `gc_started` / `gc_completed` — `trigger=manual|periodic`.
- `adoption_started` / `adoption_completed` — `trigger=manual|periodic`.

Event consumers parsing the JSON tolerate the new field by
default (slog handlers ignore unknown fields). No event-family
deletions or renames.

---

## 11. Failure-mode catalog (delta over SPEC §11 / SPEC4 §11 / SPEC5 §11 / SPEC6 §11)

| ID | Failure | Behavior |
|---|---|---|
| G1 | `mutating_htpasswd_path` set to same path as `htpasswd_path` | Startup fails with `htpasswd_paths_collide` config-error |
| G2 | `mutating_bearer_tokens` entry has malformed id (e.g. spaces) | Startup fails with `bearer_token_invalid_id` |
| G3 | `mutating_bearer_tokens` entry has secret < 32 chars | Startup fails with `bearer_token_secret_too_short` |
| G4 | `mutating_bearer_tokens` has two entries with same id | Startup fails with `bearer_token_id_collision` |
| G5 | `[reload].allowed_keys` contains a non-reloadable key | Startup fails with `reload_key_invalid` naming the offending key |
| G6 | `[reload].allowed_keys` non-default but write realm closed | Startup Warn `mutating_realm_inconsistent`; daemon runs (SIGHUP still works) |
| G7 | POST /admin/* with no credential | 401 + WWW-Authenticate (write realm) |
| G8 | POST /admin/* with read-realm credential | 401 (NOT 403, per §6.1) |
| G9 | GET /metrics with write-realm credential | 401 |
| G10 | POST /admin/* when both write-realm fields empty | 503 `mutating endpoints disabled` |
| G11 | POST /admin/config/reload with `[reload].allowed_keys` empty | 503 `reload disabled` |
| G12 | SIGHUP with `[reload].allowed_keys` empty | Logged-and-ignored Info; no log spam in tight SIGHUP loops (counter logs every 10th occurrence) |
| G13 | POST /admin/ca/rotate with `tls_mitm.enabled = false` | 503 `mitm disabled` |
| G14 | POST /admin/ca/rotate with operator-supplied CA | 409 `operator-supplied CA — rotate via ansible` |
| G15 | Two concurrent POST /admin/ca/rotate | Second returns 409 `rotate already in flight` |
| G16 | Two concurrent POST /admin/config/reload | Second returns 409 `reload already in flight` |
| G17 | POST /admin/cache/clear with both selectors | 400 `multiple selectors` |
| G18 | POST /admin/cache/clear with no selectors | 400 `selector required` |
| G19 | POST /admin/cache/clear with unknown selector key | 400 `unknown selector` |
| G20 | POST /admin/suites/{path}/refresh with `..` in path | 400 `unsafe_path` |
| G21 | POST /admin/suites/{path}/refresh with unknown suite | 404 `unknown suite` |
| G22 | Idempotency-key seen but original job dropped from store | New job created (idempotency mapping was invalidated with the drop, §4.3) |
| G23 | Reload IO failure (config file missing/permission denied) | reload_failed Warn, live config unchanged, job failed |
| G24 | Reload validation failure on any field | Reload aborts; live config unchanged; job failed with the same error startup would have produced |
| G25 | Reload pointer swap fails (programmer bug — should not happen) | reload_failed Warn stage=atomic_swap, daemon continues with old config; surfaced as a regression |
| G26 | CA rotate keypair generation fails | Job failed; live CA unchanged; cert cache NOT flushed |
| G27 | CA rotate atomic-write fails (disk full) | Job failed; live CA unchanged; temp files cleaned up |
| G28 | CA rotate succeeds but old-archive rename fails | Job done; live CA is new; old keypair stranded at live-prior path; mitm_ca_rotation_old_archive_failed Warn; operator manual cleanup required |
| G29 | Daemon crashes during CA rotate between steps 6 (swap) and 9 (rename) | Restart loads new live CA; old keypair lingers under prior name; cert cache rebuilds from scratch (in-memory, lost on restart anyway). Operator-visible disk artifact, no functional impact |
| G30 | GC during CA rotate | GC runs against `pool/`; the CA pool is a different directory under `cache_dir/ca/` — no overlap. No interference |
| G31 | suite_refresh adoption semaphore full | Job waits up to `freshness.refresh_queue_timeout` (30s); on timeout fails with `adoption semaphore full` |
| G32 | cache_clear during in-flight singleflight read | In-flight read completes (POSIX FD semantics); next request misses |
| G33 | Job retention exceeded | Oldest terminal job dropped silently from store |
| G34 | Operator polls GET /admin/jobs/{id} for aged-out job | 404 `job aged out` |
| G35 | Bearer token authentication: malformed Authorization header | 401 (no log) |
| G36 | Bearer token authentication: valid format but no matching secret | 401 (no log; constant-time compare prevents timing leak) |
| G37 | Daemon SIGTERM mid-job | Job fails with `context canceled`; admin_job_orphaned Info; HTTP client polling sees the failure |

---

## 12. Test strategy (delta over SPEC §12 / SPEC2 §12 / SPEC3 §12 / SPEC4 §12 / SPEC5 §12 / SPEC6 §12)

### 12.1 Unit tests

**Auth realm split** (`internal/admin/authrole_test.go`):

- Read credential on POST returns 401.
- Write credential on GET /metrics returns 401.
- No credential on POST returns 401 with correct
  WWW-Authenticate header (Basic / Bearer / both).
- Write realm closed: POST returns 503; GET /metrics still
  works (read realm independent).
- htpasswd-vs-bearer constant-time compare verified by timing
  histogram (10000 wrong-secret attempts; max-min < 10µs).

**Config validation** (`internal/config/admin_realm_test.go`):

- `htpasswd_paths_collide` triggers fail-closed.
- `bearer_token_invalid_id` triggers fail-closed.
- `bearer_token_secret_too_short` triggers fail-closed.
- `bearer_token_id_collision` triggers fail-closed.
- `reload_key_invalid` triggers fail-closed.
- `mutating_realm_inconsistent` triggers Warn but daemon runs.

**Jobs store** (`internal/admin/jobs_test.go`):

- pending → running → done lifecycle with all fields populated.
- pending → running → failed with ctx cancel.
- Retention: insert N+1 terminal jobs, oldest dropped.
- GET /admin/jobs/{aged-out} returns 404.
- GET /admin/jobs returns most recent N with state filter
  honored.
- Idempotency: duplicate (caller, key) within window returns
  same job; outside window creates new job; different caller
  with same key creates new job.

**Reload pipeline** (`internal/admin/reload_test.go`):

- Identical config re-load: applied=[], ignored=[], outcome=done.
- Single reloadable key changed: applied=[that key], next
  request observes new value via cfg.Load().
- Single non-reloadable key changed: applied=[], ignored=[that
  key], live unchanged.
- Validation failure on a reloadable key: reload aborts;
  cfg.Load() returns old config; reload_failed Warn emitted.
- Two concurrent reloads: second returns 409.
- SIGHUP during shutdown: reload_during_shutdown_ignored Info;
  no work scheduled.

**CA rotate** (`internal/proxy/tlsmitm/rotate_test.go`):

- Generate-and-swap round trip; fingerprint changes; cert cache
  flushed; old keypair under old/<prefix>/.
- Rotate when MITM disabled returns mitm_disabled error.
- Rotate when CA is operator-supplied returns operator-supplied
  error.
- Two concurrent rotates: second blocks on caRotateMutex,
  observes 409.
- Disk-full simulation during atomic-write: rollback; live CA
  unchanged.
- Old-archive rename failure simulation: rotate succeeds; Warn
  emitted; new live CA correct.

### 12.2 Integration tests (apt-running pattern from Phase 2-6)

**`/admin/gc/run` mid-traffic**
(`cmd/apt-cacher-ultra/admin_gc_run_integ_test.go`):

- Seed cache with synthetic orphans (Phase 4 fixture).
- Send 50 concurrent client GETs.
- POST /admin/gc/run mid-traffic.
- Assert: in-flight GETs complete; orphan count drops; no
  new GET-side errors.

**`/admin/cache/clear?canonical_host=…` selectivity**
(`cmd/apt-cacher-ultra/admin_cache_clear_integ_test.go`):

- Seed cache with rows for two distinct canonical_hosts.
- POST clear for one of them.
- Assert: target rows gone; other-host rows present; client
  GET for cleared host hits cache-miss path; client GET for
  other host serves from cache.

**`/admin/suites/{path}/refresh` re-adoption**
(`cmd/apt-cacher-ultra/admin_suite_refresh_integ_test.go`):

- Seed cache with snapshot S1.
- Mutate upstream Release.
- POST refresh for the suite.
- Assert: snapshot_id_after > snapshot_id_before; subsequent
  client GETs serve the new snapshot.

**`/admin/ca/rotate` end-to-end**
(`cmd/apt-cacher-ultra/admin_ca_rotate_integ_test.go`):

- Daemon up with auto-generated CA.
- Test apt client trusts both old and new CAs (fixture).
- POST rotate.
- Assert: result body has both fingerprints; subsequent
  CONNECT presents leaf signed by new CA; old-archive
  directory created.

**`/admin/config/reload` regex change**
(`cmd/apt-cacher-ultra/admin_config_reload_integ_test.go`):

- Daemon up with `upstream.allowed_host_regex = ^foo$`.
- Edit config file: regex = `^foo$|^bar$`.
- POST reload.
- Assert: GET for `bar` host now works (was 403 before reload);
  applied list contains the regex change.

### 12.3 Chaos tests

- ctx cancel during /admin/gc/run → job fails; no partial
  commit; no orphan blobs.
- Disk full during /admin/ca/rotate → live CA unchanged; job
  failed.
- Power loss simulation (kill -9) mid-rotate → restart loads
  new live CA OR old, depending on timing; never half-state.
- Two concurrent /admin/config/reload → second 409.
- /admin/cache/clear concurrent with /admin/suites/refresh
  for the same suite → both succeed; one's transaction wins
  the SQLite serialization, the other re-runs against new
  state.

### 12.4 E2E tests

- The Phase 1-6 chaos test pass adds an "operator-action" step:
  mid-suite, POST /admin/gc/run, verify in-flight apt fetches
  complete unaffected and post-GC apt fetches still serve from
  cache.
- A fresh CA distributed to the test apt client; rotate the
  daemon's CA; verify the test apt client (which has both old
  + new in its trust store) continues to fetch successfully.

### 12.5 Production exercise (one-week soak)

Per §15 #18, a one-week production deployment exercises:

- One CA rotate.
- One /admin/gc/run.
- One /admin/cache/clear.
- One SIGHUP + /admin/config/reload pair.

Each is recorded with timing, observable metric impact, and
post-action regression check. The exercise is documented as a
runbook entry at PHASE-7-SOAK.md (created at end of soak).

---

## 13. Project layout (delta over SPEC §13 / SPEC4 §13 / SPEC5 §13 / SPEC6 §13)

New files:

```
internal/admin/
  authrole.go            # write-role auth (htpasswd + bearer); realm-aware middleware
  authrole_test.go
  jobs.go                # in-memory jobs store, idempotency-key store, retention
  jobs_test.go
  mutate.go              # POST /admin/* handlers (gc_run, cache_clear,
                         # suite_refresh, ca_rotate, config_reload)
  mutate_test.go
  reload.go              # config hot-reload pipeline
  reload_test.go

internal/freshness/
  refresh_suite.go       # exported RefreshSuite entry point (extracted
                         # from existing periodic refresher)

internal/gc/
  run_now.go             # exported RunNow entry point

internal/cache/
  clear.go               # ClearByCanonicalHost, ClearBySuite

internal/proxy/tlsmitm/
  rotate.go              # CA rotation entry point
  rotate_test.go

cmd/apt-cacher-ultra/
  admin_gc_run_integ_test.go
  admin_cache_clear_integ_test.go
  admin_suite_refresh_integ_test.go
  admin_ca_rotate_integ_test.go
  admin_config_reload_integ_test.go
  ca_rotate_subcommand.go    # `apt-cacher-ultra ca rotate` (§14.2)
  ca_rotate_subcommand_test.go
```

Modified files:

- `internal/config/config.go` — `[admin]` field additions,
  `[reload]` block, atomic-pointer-swap support for live
  config, validation rules per §5.3.
- `internal/admin/admin.go` — register POST handlers; split
  middleware into read/write realm authentication; expose
  jobs surface (`GET /admin/jobs[/{id}]`); status JSON / HTML
  additions per §2.4 / §2.5.
- `internal/admin/template/status.html` — "Action surface"
  section + "Last rotation" sub-row.
- `internal/proxy/tlsmitm/ca.go` — atomic.Pointer swap on the
  live CA (so rotate is observable lock-free by cert-issuance
  workers).
- `cmd/apt-cacher-ultra/main.go` — wire SIGHUP handler;
  startup ordering per §9.4 step 6; shutdown ordering per
  §9.5 step 3.5; jobs store init.
- `packaging/config/config.toml.default` — new fields per §5.5.
- `packaging/systemd/apt-cacher-ultra.service` — `ExecReload=`
  pointing at `kill -HUP $MAINPID` (so `systemctl reload` does
  the right thing).

---

## 14. Subcommand surface (delta over SPEC6 §14)

### 14.1 Existing subcommands — unchanged

`apt-cacher-ultra` continues to support:

- `apt-cacher-ultra` (default — runs the daemon).
- `apt-cacher-ultra --print-apt-conf` (Phase 6).
- `apt-cacher-ultra ca print` (Phase 6).
- `apt-cacher-ultra remap <host>` (Phase 5).

### 14.2 NEW: `apt-cacher-ultra ca rotate`

Synopsis:

```
apt-cacher-ultra ca rotate [--config <path>] [--force-shared-ca]
```

Semantics:

- Reads config from `--config` path (default
  `/etc/apt-cacher-ultra/config.toml`).
- Refuses to run with exit code 1 if `tls_mitm.enabled = false`,
  printing `mitm disabled` to stderr.
- Refuses to run with exit code 1 if the CA is operator-supplied
  (`tls_mitm.ca_cert` and `tls_mitm.ca_key` non-empty), printing
  `operator-supplied CA — rotate via ansible (or pass --force-shared-ca to override on a single instance)` to stderr.
- With `--force-shared-ca`, the operator-supplied check is
  skipped and the rotate proceeds. Exit code is 0 on success.
  Operators using this flag are responsible for distributing
  the new CA to peer caches.
- Calls `tlsmitm.Rotate(ctx)` (§6.4) directly; the same flock
  on `cache_dir/ca/` serializes against any running daemon. If
  the daemon is up, both compete for the flock; whichever wins
  proceeds, the loser exits 1 with `flock_timeout` after
  `tls_mitm.ca_rotate_lock_timeout` (30s).
- On success, prints to stdout (PEM-formatted-friendly, one
  field per line):
  ```
  rotated_at: 2026-05-09T13:50:12Z
  old_fingerprint_sha256: <hex>
  new_fingerprint_sha256: <hex>
  new_not_after: 2027-05-09T13:50:12Z
  old_archive: cache_dir/ca/old/<prefix>/
  ```
- Exit code 0 on success, 1 on any failure (flock timeout, disk
  full, validation failure).

Design notes:

- The subcommand is intentionally identical in disk side-effect
  to `POST /admin/ca/rotate`. An operator can use either
  interchangeably.
- Subcommand does NOT flush the cert cache (because there is no
  daemon process to flush). On daemon restart, the cert cache
  is reconstructed from scratch under the new CA — same as any
  cold-start.
- Subcommand does NOT emit log events (no slog handler is wired
  in subcommand mode). The on-disk artifacts under `ca/old/`
  + the stdout printout are the audit trail.

### 14.3 NEW: `apt-cacher-ultra ca list`

Synopsis:

```
apt-cacher-ultra ca list [--config <path>]
```

Lists the live CA and all archived CAs in `cache_dir/ca/old/`.
Output (one CA per line):

```
LIVE   <fingerprint-sha256> not_after=<rfc3339> path=<path>
old    <fingerprint-sha256> rotated_at=<rfc3339> path=<path>
old    <fingerprint-sha256> rotated_at=<rfc3339> path=<path>
...
```

Useful for operators evaluating rollback options. Exit 0 on
success.

---

## 15. Definition of done

Phase 7 is complete when all of the following hold:

1. **`go test -race ./...` passes** with all new tests under
   §12 included.

2. **Mutating endpoints functional.** Each of the five
   endpoints (gc/run, cache/clear, suites/.../refresh,
   ca/rotate, config/reload) returns 202 + Location for valid
   requests, 401/403/409/503 for the documented failure cases,
   and produces a job retrievable at GET /admin/jobs/{id}.

3. **Auth realm split enforced.** Read-role credential cannot
   reach POST /admin/*; write-role credential cannot reach
   GET /metrics. Tested by §12.1 cases.

4. **CA rotation end-to-end.** `apt-cacher-ultra ca rotate`
   subcommand and POST /admin/ca/rotate produce identical
   on-disk state. Cert cache flushes; old keypair archived under
   `cache_dir/ca/old/`. Tested by §12.2 / §12.4.

5. **Config hot-reload functional.** SIGHUP and
   POST /admin/config/reload apply the §5.4 reloadable subset.
   Tested by §12.2.

6. **Idempotency-key working.** Two requests with the same
   (caller, Idempotency-Key) within `idempotency_window`
   return the same 202 / Location.

7. **Audit logging complete.** Every successful mutation
   produces a paired `admin_action_started` /
   `admin_action_completed` log line with non-empty `caller`,
   `job_id`, `remote_addr`. Tested by §12.1 / §12.2.

8. **Status surface live.** GET /?format=json includes the
   `action_surface` section per §2.4; GET / HTML includes the
   "Action surface" section per §2.5; tls_mitm.rotation
   sub-fields populate after a rotate.

9. **Metrics complete.** `acu_admin_action_total`,
   `acu_admin_jobs_inflight`, `acu_config_reloads_total`,
   `acu_mitm_ca_rotations_total`,
   `acu_mitm_ca_rotation_last_unixtime` all expose at
   `/metrics`. Cardinality stays under
   `metric_series_cap = 1024`.

10. **Failure modes pinned.** Every G1–G37 case (§11) has at
    least one regression test. Tests assert both the user-facing
    behavior (HTTP status / log line) AND the internal invariant
    (live config unchanged on reload failure; live CA unchanged
    on rotate failure; etc.).

11. **No schema migration.** Phase 7 adds zero SQLite tables
    and modifies zero existing tables. `cache_dir` opened by a
    Phase 7 daemon is interchangeable with one opened by a
    Phase 6 daemon (forward and backward).

12. **Documentation complete.** SPEC7.md (this document) is
    locked. PHASE-7-SCOPING.md is locked at revision 2.
    `packaging/config/config.toml.default` includes the §5.5
    additions with operator-guidance comments.

13. **`apt-cacher-ultra ca rotate` and `ca list` subcommands
    work** without a running daemon. The flock contract per
    §14.2 prevents corruption when both the subcommand and a
    running daemon attempt rotate concurrently.

14. **Live exercise.** On the test environment the operator
    runs each of the five mutating endpoints once successfully,
    observes the audit log lines, the metrics increments, and
    the status page surface. (Recorded as a checklist in the
    PR or commit message.)

15. **Production soak.** A one-week production deployment
    exercises all five mutating endpoints (one CA rotate, one
    GC, one cache/clear, one suite/refresh, one config/reload
    pair via SIGHUP and HTTP). Stable
    `acu_request_total{outcome=…}` rates throughout the soak
    window; no leaked goroutines (`acu_runtime_goroutines`
    stable); no `acu_admin_action_total{outcome="failed"}`
    increments outside operator-injected failure tests.

16. **Graceful shutdown drains jobs.** SIGTERM during a job
    causes the job to fail with `context canceled`; the daemon
    exits within `shutdown_timeout`; no leaked goroutines per
    `goleak` integration test.

17. **No regression in Phase 1–6 surface.** All Phase 1–6 tests
    pass under the Phase 7 build. The Phase 6 `tls_mitm`
    behavior, in particular, is unchanged when no rotation is
    invoked.

18. **`/admin/config/reload` deadlock-free.** Stress test
    (10000 reloads with no config changes) completes within
    `10×count×reload_p99` seconds with no goroutine leak and
    no held-mutex panic.

19. **Archive directory growth observable.** A test that runs
    50 rotates verifies `cache_dir/ca/old/` contains exactly
    50 fingerprint-prefix subdirectories AND the
    `tls_mitm.rotation.old_ca_count_on_disk` status field
    reports 50.
