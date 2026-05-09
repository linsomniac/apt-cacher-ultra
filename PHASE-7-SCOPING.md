# apt-cacher-ultra — Phase 7 Scoping

Status: **scoping locked** (revision 2). Last updated 2026-05-09.
Next artifact: `SPEC7.md` modeled on `SPEC6.md`'s structure.

Revision 2 closes the §7 open-question table. The load-bearing
question §7.0 (theme) resolved to **Bucket A — operator control
plane**. The fifteen remaining detail questions (§7.1–§7.15) all
accepted the proposed defaults. All sixteen resolutions are recorded
in §1.3 below.

This document gathers what Phase 7 is, the hooks Phases 1–6 left in
place for it, and the locked design decisions that will become
SPEC7.md. Companion documents [PHASE-2-SCOPING.md](PHASE-2-SCOPING.md),
[PHASE-3-SCOPING.md](PHASE-3-SCOPING.md),
[PHASE-4-SCOPING.md](PHASE-4-SCOPING.md),
[PHASE-5-SCOPING.md](PHASE-5-SCOPING.md), and
[PHASE-6-SCOPING.md](PHASE-6-SCOPING.md) record the parallel
exercises for earlier phases.

---

## 1. Goals — operator control plane

Phase 1 made the cache-hit path bulletproof. Phase 2 closed the
integrity and freshness loops. Phase 3 closed the service-continuity
loop. Phase 4 closed the storage-reclamation loop. Phase 5 closed
the operator-visibility loop. Phase 6 closed the HTTPS-upstream
caching loop.

Phase 7 closes the **operator action surface**: the runtime
controls an operator reaches for when something needs intervention.
Phase 5 gave operators *eyes* (`/metrics`, status page, ring
buffer); Phase 7 gives them *hands* — mutate, rotate, reload —
without a daemon restart and without per-host ssh.

Four sub-goals, all graduations of items SPEC4 / SPEC5 / SPEC6 §1.2
explicitly named as Phase 7+ candidates:

1. **Mutating admin endpoints.** `POST /admin/gc/run`,
   `POST /admin/cache/clear`, `POST /admin/suites/{path}/refresh`.
   SPEC5 §1.2 deferred them; SPEC6 §1.2 confirmed Phase 7+
   graduation "if observational data argues for them." Production
   experience and ad-hoc operator requests are now the data.

2. **CA rotation primitive.** `apt-cacher-ultra ca rotate`
   subcommand + optional `POST /admin/ca/rotate` endpoint. SPEC6
   Q15 resolved Phase 6 ships rotation only via out-of-band CA +
   ansible reconfigure; Phase 7 graduates the in-daemon flow.
   Cross-fade behavior (old CA continues to serve in-flight
   leaves until expiry) is the open question §7.4.

3. **Limited config hot-reload.** `SIGHUP` and (optional)
   `POST /admin/config/reload` trigger a reload of a defined
   subset: regex lists, freshness cadence, log level. Listener
   bindings, CA paths, schema-affecting items, and
   `tls_mitm.enabled` remain restart-only. The SPEC6 §1.2 carry-
   over of "hot-reload of `[tls_mitm]` block" lives entirely in
   the restart-only column — the regex *inside* the block is
   reloadable, the `enabled` flip is not.

4. **Auth for mutating endpoints.** Phase 5 ships read-only
   htpasswd; Phase 7 adds a distinct "write" role (separate
   credential file, separate audit log entry). Mutating endpoints
   can never be reached under read-only credentials, and the
   write role can never reach `/metrics` (its credentials are
   scoped to the action surface).

The four sub-goals share infrastructure:

- The existing Phase 5 admin listener — no new listener.
- The existing slog event stream — new `admin_action_*` family.
- The existing metrics registry — new `acu_admin_action_*`
  counters and `acu_admin_jobs_*` gauges.
- The existing `cache_dir/ca/` storage layout — rotation writes
  alongside; no schema migration.

### 1.1 Bounded scope

Phase 7 deliberately does NOT add:

- A new listener, port, or dispatch surface (the admin listener
  carries everything).
- A new persistence schema (jobs are in-memory only; on restart,
  unfinished jobs are abandoned with a `admin_job_orphaned` Info
  on shutdown). Persisted job history is a Phase 8+ topic.
- An RPC framing or non-HTTP transport. JSON over HTTP/1.1
  remains the contract.
- A "supervisor" / multi-instance coordination layer. Phase 7
  endpoints act on the local daemon instance only.

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
  ticker covers normal operations; manual rescan available via
  Goal 1's `/admin/gc/run`).

Newly deferred in Phase 7:

- **OpenTelemetry / OTLP exporters.** Same disposition as
  SPEC5 §1.2 / SPEC6 §1.2. Prometheus exposition is the baseline.
- **Distributed tracing.** Same disposition as SPEC5 §1.2.
- **Admin-listener TLS.** SPEC6 §1.2 deferred this; Phase 7
  doubles down on read-only-and-write being plain HTTP behind
  reverse proxy / network ACL. A daemon-internal TLS termination
  flow doubles the cert-management surface for negligible
  operational benefit on a 127.0.0.1-by-default listener.
- **Client TLS auth on admin port.** SPEC6 §10.4 noted this is a
  Phase 7+ topic; htpasswd + bearer token (or reverse-proxy mTLS)
  cover the operational need.
- **HSM / PKCS#11 CA keys.** SPEC6 §1.2; rotate writes file-on-disk
  CA. HSM glue waits for any deployment to ask.
- **Per-client CA pinning.** SPEC6 §1.2; out of scope.
- **Pre-emptive cert generation.** SPEC6 §1.2; certs continue to
  be generated on first CONNECT. Rotate may pre-warm a future
  Phase 8.
- **Ed25519 leaf algorithm.** SPEC6 §5.1.3; the closed enum
  remains `ecdsa-p256` and `rsa2048`.
- **Non-443 HTTPS upstreams.** SPEC6 §2.2 step 1; the "future
  override knob" remains future. Phase 7 endpoints do not change
  the CONNECT port allowlist.
- **Persisted job history.** A `/admin/jobs` listing of past
  job runs (status, start time, who triggered) requires a small
  SQLite table; Phase 7 keeps jobs in memory. If operators ask
  for retention beyond a daemon restart, Phase 8.
- **`/admin/cache/clear` of an individual blob by content hash.**
  Selective invalidation by `(canonical_host, suite)` is in
  scope; per-blob is not.
- **Mutating endpoints on the proxy listener.** All mutation
  flows through the admin listener; the proxy listener stays
  pure.

### 1.3 Resolved during Phase 7 scoping

The sixteen design questions raised in §7 of revision 1 were
resolved with the operator during this scoping pass. Each
resolution is normative for SPEC7.

- **Phase 7 theme (Q0).** **Bucket A — operator control plane.**
  Phase 5 gave operators eyes; Phase 7 gives them hands. The
  three alternatives (B repo-feature parity, C observability
  deepening, D advanced TLS) are not the most operationally
  valuable next phase given the post-Phase-6 production cutover.
- **Endpoint sync vs async (Q1).** **Async (202 + jobs store).**
  GC and CA rotate can take seconds; blocking the request is
  hostile to HTTP timeouts. `202 Accepted` + `Location:
  /admin/jobs/{id}` + a JSON jobs surface is the standard shape.
- **Cache-clear selectors (Q2).** **`canonical_host` and
  `suite` only.** `path` is the most surgical selector but
  the hardest to spec (path-vs-canonical-path, percent-encoding);
  the niche use case does not justify the surface in Phase 7.
- **Cache-clear under live traffic (Q3).** **In-flight requests
  complete naturally; new requests miss.** No forced 503. The
  database commits the row deletes synchronously; pool/ files
  unlink on the next GC tick (which the operator can also force
  via `/admin/gc/run`). An "emergency purge" mode that 503s
  in-flight requests is deferred unless any deployment asks.
- **CA rotation cross-fade (Q4).** **No cross-fade.** Operator
  orders distribute-then-rotate. Two-CA cert-cache partitioning
  + dual-fingerprint reporting + end-of-window cleanup is
  substantial code for a goal achievable by ordering the
  ansible step before the rotate call.
- **Suite refresh URL parameter (Q5).** **Apt path** — the URL is
  `POST /admin/suites/{path}/refresh` where `{path}` matches the
  apt sources.list view (e.g. `/ubuntu/dists/jammy`). What an
  operator types from sources.list, no internal-key translation.
- **Bearer token auth (Q6).** **Yes — accept Bearer in addition
  to htpasswd.** `Authorization: Bearer <token>` matches a
  configured `id:secret` pair. Bearer is opt-in
  (`mutating_bearer_tokens = []` by default). ansible vaults
  distribute tokens cleanly without shelling out to `htpasswd -B`.
- **Caller-label cardinality cap (Q7).** **No additional cap.**
  The existing Phase 5 `metric_series_cap = 1024` is the upper
  bound; operators with more than 1024 admin users have bigger
  problems.
- **Hot-reload allowlist default (Q8).** **Default-populated.**
  `[reload].allowed_keys` ships with the §3.4 set out of the
  box. Operators who want SIGHUP to be a no-op set
  `allowed_keys = []`. Less ceremony to benefit; the principle-
  of-least-surprise concern is mitigated by SIGHUP audit logging.
- **Old-CA disk retention (Q9).** **Keep all historical CAs
  under `ca/old/{fingerprint}/`.** Disk cost is ~5 KiB per
  rotation; operators who care prune manually. No automatic
  retention policy in Phase 7.
- **Rotate when MITM disabled (Q10).** **Refuse with exit 1.**
  Match the existing SPEC6 Q12 contract for `ca print`. Writing
  a keypair the daemon will not load is operator confusion.
- **SIGHUP during shutdown (Q11).** **Log
  `reload_during_shutdown_ignored` Info.** Match the existing
  SIGTERM-during-shutdown observability pattern.
- **Reload audit-log shape (Q12).** **One event per reload listing
  all changed keys.** `reload_applied changed=[upstream.allowed_host_regex,log_level]`.
  Per-key events would explode the event log on a wholesale
  config update.
- **Status page caller column (Q13).** **Show `caller` in the
  status page job table.** Operators looking at the page need
  to see who triggered recent actions without grepping logs.
- **Proxy-listener-misroute metric (Q14).** **No new metric.**
  The existing 405 path on the proxy listener is sufficient. A
  POST to `/admin/something` on `:3142` is an operator
  configuration error, not a class of regression worth a counter.
- **Job retention default (Q15).** **`job_retention = 100`.**
  Operators with high admin churn raise it to 1000. Memory cost
  is ~1KB per remembered job.

---

## 2. What Phases 1–6 already left in place

Walking the existing code, prior phases deliberately seeded these
hooks that Phase 7 builds on:

| Prior-phase hook | Phase 7 use |
|---|---|
| Phase 5 admin listener (`internal/admin/`) | Mutating endpoints register on the same mux; one auth realm becomes two realms |
| Phase 5 htpasswd auth helper | Reused for the new write-role htpasswd file (`mutating_htpasswd_path`) |
| Phase 5 slog event family pattern | New `admin_action_started` / `admin_action_completed` / `admin_action_failed` event family slots in alongside |
| Phase 5 metrics registry | New `acu_admin_action_total{action,outcome}` counter + `acu_admin_jobs_inflight` gauge |
| Phase 5 status-page sections | New "Action surface" section (current jobs, last 10 completed) under existing template |
| Phase 4 GC pipeline (`internal/gc/`) | `/admin/gc/run` calls into the existing GC entry point with an explicit trigger label |
| Phase 4 invariants on `pool/` and `temp/` finalize | Cache-clear flow respects the same atomic-finalize contract — clear under live traffic must not race the singleflight leader |
| Phase 3 adoption pipeline (`internal/freshness/adoption.go`) | `/admin/suites/{path}/refresh` calls into the existing adoption entry point |
| Phase 6 `apt-cacher-ultra ca print` subcommand | Pattern reused for `apt-cacher-ultra ca rotate` (same flag conventions, same exit-code contract) |
| Phase 6 `tlsmitm.LoadOrGenerate` (with flock) | Rotation reuses the flock pattern for the new keypair write; the old keypair stays loadable until the operator removes it |
| Phase 6 cert cache (LRU) | Rotation flushes the cert cache (the simpler default per §7.4); cross-fade variant retains old-CA-signed leaves until eviction |
| `slog`-based shutdown sequencing (SPEC1 §9.5, SPEC6 §9.5) | In-flight admin jobs receive ctx cancel on shutdown; the lifecycle pattern from Phase 6 generalizes |
| Phase 5 §10.4 status template | New `action_surface` block — current jobs (id, action, started_at, state) and last 10 completed |
| AIDEV-NOTE convention | New action-surface code carries anchors for the auth realm split, the job state machine, and the rotation cross-fade window |

No prior phase shipped a mutating endpoint. Phase 5 explicitly kept
the admin listener read-only (SPEC5 §1.2). Phase 6 added one
write-side subcommand (`ca print` is read-only; Phase 7 adds
`ca rotate` which writes). Phase 7 is additive on the admin
listener: same TCP socket, same status page, two new auth realms
and one new endpoint family.

---

## 3. Design (high level)

This section is intentionally not a full SPEC — the open questions
in §7 will move pieces around. It describes the shape of the work
so reviewers can evaluate the open questions in context.

### 3.1 Endpoint family (proposed)

All mutating endpoints follow one shape:

```
POST /admin/gc/run                          → 202 Accepted
POST /admin/cache/clear?canonical_host=…    → 202 Accepted
POST /admin/cache/clear?suite=…              → 202 Accepted
POST /admin/suites/{path}/refresh           → 202 Accepted
POST /admin/ca/rotate                       → 202 Accepted
                                              [?cross_fade_hours=N]

GET  /admin/jobs/{id}                       → 200 (job status JSON)
GET  /admin/jobs                            → 200 (last 10 jobs JSON)
```

`202 Accepted` is paired with `Location: /admin/jobs/{id}` and a
JSON body containing the same `id`. The operator polls
`/admin/jobs/{id}` until `state` is `done` or `failed`. This shape
matches the SPEC5 §10.4 JSON-first / Prometheus-second contract:
admin scripts speak JSON, humans browse the status page.

Job state machine:

```
pending → running → done
                  ↘ failed
```

`pending` is the brief window between `202` and the worker
picking up the job. `running` is the steady state. `done` /
`failed` are terminal. A `cancel_requested` field flips during
shutdown (ctx cancel), but the state machine does not have a
distinct `cancelled` terminal — cancel manifests as `failed` with
`error: "context canceled"`.

### 3.2 Cache-clear scope

`POST /admin/cache/clear` accepts exactly one selector:

- `?canonical_host=foo.example` — drops every blob whose
  `canonical_host` matches (case-insensitive); also drops cached
  Release/Packages metadata for that host. Cert cache is NOT
  flushed by this — that's `ca rotate`'s job.
- `?suite=jammy` — drops every blob whose suite-key matches.
  Note that "suite" here means the apt-suite name, not the path.
  `?path=/ubuntu/dists/jammy` is the path-based variant; §7.5
  decides whether to ship one or both.

Multi-selector queries (`?canonical_host=…&suite=…` AND-joined)
are deferred. The selector is exclusive to keep the audit log
line unambiguous.

The clear is **not** synchronous: the worker walks SQLite, marks
matching rows for sweep, and the next GC tick picks up the unlinked
files. The job completes (`done`) when the database updates have
committed; pool/ files persist until the next GC tick (which the
operator can also force via `/admin/gc/run`).

### 3.3 CA rotation flow (proposed default — no cross-fade)

```
1. Operator distributes NEW CA cert to all clients via ansible.
   (Pre-rotation step; daemon does not coordinate this.)

2. Operator runs:
   - apt-cacher-ultra ca rotate (subcommand, off-line)
     OR
   - POST /admin/ca/rotate (online, daemon writes the new keypair)

3. Daemon:
   a. Generates a new CA keypair under cache_dir/ca/.
      Files written atomically per Phase 6 §4.2.1; the new files
      live alongside the old until step (e).
   b. Updates the in-memory CA pointer to the new keypair.
   c. Flushes the leaf-cert cache (`acu_mitm_cert_cache_size` → 0).
      All future CONNECTs generate fresh leaves under the new CA.
   d. Emits `mitm_ca_rotated` Info with old + new fingerprints.
   e. Renames old files to `cache_dir/ca/old/{fingerprint}/{cert,key}`
      (kept for forensic rollback; not loaded). The daemon will not
      open files under `old/` automatically; manual rollback is
      "rotate again with `--from old/{fingerprint}/`".

4. Clients that have the new CA installed continue to validate
   leaves issued from this point. Clients that don't — their next
   CONNECT fails with TLS validation error (the operator's pre-rotation
   ansible step is what prevents this).
```

The §7.4 open question is whether to ship a "cross-fade" variant
where old CA stays loaded for `cross_fade_hours` and continues to
sign leaves (so the cache can dual-emit). The proposed default is
NO cross-fade — operationally cleaner, less code, fewer edge
cases. The operator's correct sequence is "distribute then rotate."

### 3.4 Hot-reload subset (proposed)

| Setting | Reloadable? | Apply when |
|---|---|---|
| `upstream.allowed_host_regex` | Yes | Next request consults the new value |
| `tls_mitm.allowed_host_regex` | Yes | Next CONNECT signing gate consults the new value |
| `freshness.periodic_refresh` | Yes | Refresher restarts on the next natural tick |
| `gc.interval` | Yes | GC re-arms its ticker |
| `log_level` (proposed addition) | Yes | slog handler swaps level |
| `cache.listen` / `listen_tls` / `admin.listen` | **No** | Listener restart needed |
| `cache.tls_cert` / `tls_mitm.ca_cert` / `ca_key` | **No** | Use rotate / restart |
| `tls_mitm.enabled` | **No** | Dispatcher wiring change |
| `cache.cache_dir` | **No** | Storage identity invariant |
| Schema-related (`adoption.*` schema bits) | **No** | Migration concern |

Reload is **all-or-nothing per setting**: a SIGHUP either commits
the new value across the whole reloadable subset or rolls back
the entire reload (validation failure on any field aborts the
whole reload). This matches the Phase 1 §9.5 startup contract:
the daemon is either fully validated or doesn't run. A partial
reload would create a state the startup contract never sees.

`/admin/config/reload` shape:

```
POST /admin/config/reload          → 202 + Location /admin/jobs/{id}
                                     OR
                                     409 Conflict if a reload is
                                     already in flight.
```

The job's `result` field on success is the diff (which fields
changed, old → new). On failure the `error` field names the
rejected field and the validation message — same wording as
startup config-load errors.

### 3.5 Auth realm split (proposed)

Phase 5 ships one htpasswd file (`admin.htpasswd_path`) protecting
the read-only surface. Phase 7 introduces:

```toml
[admin]
htpasswd_path           = "/etc/apt-cacher-ultra/admin.htpasswd"           # read-only (existing)
mutating_htpasswd_path  = "/etc/apt-cacher-ultra/admin-mutate.htpasswd"     # NEW — write role
```

Routing:

- `GET /metrics`, `GET /`, `GET /admin/jobs[/{id}]`, `GET /admin/events`
  → require credential matching `htpasswd_path` (read-role).
- `POST /admin/*` (any mutating endpoint)
  → require credential matching `mutating_htpasswd_path`
    (write-role).

A read-role credential cannot reach a `POST` endpoint (401). A
write-role credential cannot reach `GET /metrics` (401). This is
deliberate: a leaked metrics-scraper credential cannot trigger a
GC, and a leaked rotate credential cannot scrape metrics off the
machine.

If `mutating_htpasswd_path` is unset, **all mutating endpoints
return 503** with `error: "mutating endpoints disabled"`. There
is no "no auth required" mode for the write surface — the only
way to disable it is to disable the whole feature.

The §7.6 open question is whether to also accept Bearer tokens
(for ansible vault-distributed secrets) alongside htpasswd.

### 3.6 Audit logging

Every mutating-endpoint invocation emits two log lines:

```
admin_action_started   action=gc_run         job_id=… caller=write-role-username remote_addr=…
admin_action_completed action=gc_run         job_id=… outcome=done duration_ms=… (or outcome=failed error=…)
```

The `caller` field is the htpasswd username (or token id) that
authenticated the request; this is the audit primitive. Bearer-
token tokens have an opaque `id:` prefix that the operator
configures, distinct from the secret bytes.

`acu_admin_action_total{action,outcome,caller}` increments at
completion. `caller` is a label because the cardinality is
bounded by the htpasswd file (typically 1–5 entries). If §7.7
decides bearer tokens, the same label captures the token id.

### 3.7 Configuration block (proposed)

```toml
[admin]
# Existing Phase 5 fields unchanged.
listen                    = "127.0.0.1:9090"
htpasswd_path             = "/etc/apt-cacher-ultra/admin.htpasswd"
read_timeout              = "5s"
idle_timeout              = "30s"
gauge_refresh             = "5s"
metric_series_cap         = 1024

# NEW — write-role gate.
mutating_htpasswd_path    = ""           # empty = mutating endpoints disabled
mutating_bearer_tokens    = []           # optional, list of "id:secret" pairs (§7.6/§7.7)

# NEW — job retention in memory.
job_retention             = 100          # last N completed jobs visible via GET /admin/jobs

[reload]
# NEW — the SIGHUP / reload subset opt-in. Empty list disables
# reload entirely (SIGHUP is ignored; /admin/config/reload returns 503).
allowed_keys              = ["upstream.allowed_host_regex",
                             "tls_mitm.allowed_host_regex",
                             "freshness.periodic_refresh",
                             "gc.interval",
                             "log_level"]
```

§7.8 open question: do reloadable keys default to "everything in
the §3.4 table" or to "nothing"? The example above takes the
explicit-allowlist path (default empty); the alternative is
"reload is on by default for the whole §3.4 set."

---

## 4. Implementation surface (estimate)

The bulk of the change lives in **two new files** under
`internal/admin/`:

```
internal/admin/
  jobs.go             # job state machine, in-memory store, status JSON
  mutate.go           # mutating-endpoint handlers (gc/run, cache/clear,
                      # suites/.../refresh, ca/rotate, config/reload)
  reload.go           # config hot-reload pipeline (validate → swap
                      # atomic pointer → fire reload-applied events)
  authrole.go         # write-role htpasswd + bearer-token authn,
                      # role-aware middleware
```

Plus changes to:

- `internal/config/config.go`: new `[admin].mutating_htpasswd_path`
  + `[reload].allowed_keys` + atomic-pointer-swappable Config
  struct (the live config sits behind an `atomic.Pointer[Config]`;
  request handlers `Load()` it).
- `internal/admin/admin.go`: register the new POST handlers; split
  the auth middleware into read/write realms.
- `internal/freshness/`: `RefreshSuite(suiteKey)` entry point exported
  for the mutating endpoint to call.
- `internal/gc/`: `RunNow(reason)` entry point.
- `internal/cache/`: `ClearByCanonicalHost(host)` and
  `ClearBySuite(suite)` entry points; both transactional, both
  emit a `cache_clear_request` Info.
- `internal/proxy/tlsmitm/`: `Rotate(ctx)` entry point — generates
  new keypair, atomically swaps the in-memory CA, flushes the
  cert cache.
- `cmd/apt-cacher-ultra/main.go`: SIGHUP wiring; new CA-rotate
  subcommand modeled on `ca print`.
- `internal/admin/template/status.html`: new "Action surface"
  section.

Estimated diff: ~900–1300 LOC including tests. Larger than
Phase 6 (~600–900) primarily because the four sub-goals are
independently nontrivial; smaller than Phase 5 (~2k+) because no
new listener / no new metrics registry / no new template engine.

---

## 5. Test plan (high level)

Unit tests:

- Job state machine: pending→running→done, pending→running→failed,
  ctx-cancel during running → failed with cancel error.
- Auth realm split: read credential on POST → 401, write credential
  on `/metrics` → 401, no credential → 401, missing
  `mutating_htpasswd_path` → 503 on POST.
- Hot-reload validation: invalid regex in
  `upstream.allowed_host_regex` reload → reload aborts, old config
  remains live, error JSON names the field.
- Cache-clear by `canonical_host`: rows for that host removed;
  rows for other hosts untouched; the GC tick after the clear
  unlinks the corresponding pool/ files.
- CA rotation: new keypair generated, in-memory CA pointer
  swapped, cert cache flushed (`acu_mitm_cert_cache_size = 0`),
  next CONNECT issues a leaf signed by the new CA, old keypair
  moved to `ca/old/{fingerprint}/`.
- SIGHUP: identical-config reload is idempotent; reload during
  graceful-shutdown returns 503.

Integration tests (existing apt-running pattern from Phase 2–6):

- `POST /admin/gc/run` triggers GC mid-traffic; in-flight downloads
  are not interrupted; the GC report counts unlinked blobs.
- `POST /admin/suites/{path}/refresh` triggers re-adoption; new
  snapshot becomes current; subsequent client requests serve
  the new snapshot.
- `POST /admin/cache/clear?canonical_host=…` removes that host's
  blobs; subsequent client requests for that host hit the
  cache-miss path; subsequent client requests for OTHER hosts
  hit the cache.
- `POST /admin/ca/rotate` followed by a CONNECT — the new leaf
  validates against the new CA cert (test fixture trusts both).
- `POST /admin/config/reload` with a regex change — next request
  is governed by the new regex.

Chaos tests:

- ctx cancel during `/admin/gc/run` → job fails with `context
  canceled`, no partial GC commit, no orphan blobs.
- `POST /admin/ca/rotate` while a CONNECT is mid-handshake — the
  in-flight handshake completes against the OLD leaf (already
  generated); the next CONNECT picks up the new CA.
- Disk full during CA rotation — new keypair write fails atomically,
  old keypair remains live, job emits `failed` with disk-full
  error; daemon continues serving under the old CA.
- Two concurrent `/admin/config/reload` requests — second returns
  409 Conflict; first runs to completion.
- Two concurrent `/admin/ca/rotate` requests — second returns
  409 Conflict; same lock as reload.

E2E tests:

- The Phase 1–6 chaos test pass adds an "operator-action" step:
  mid-suite, force `/admin/gc/run`, verify in-flight apt fetches
  complete unaffected and post-GC apt fetches still serve from
  cache.
- A fresh CA distributed to the test apt client; rotate the
  daemon's CA; verify the test apt client (which has both old +
  new in its trust store) continues to fetch successfully.

---

## 6. Acceptance criteria (proposed)

Phase 7 is gated on:

1. Every test in §5 passes under `go test -race ./...`.
2. `POST /admin/gc/run` triggers a GC that unlinks expected
   orphans on a synthetic-orphan-seeded cache; metrics
   (`acu_gc_unlinked_total`) increment by the same amount as the
   periodic ticker would have.
3. `POST /admin/cache/clear?canonical_host=apt.corretto.aws`
   removes corretto rows; subsequent apt fetch for corretto
   re-fetches upstream; subsequent apt fetch for archive.ubuntu.com
   continues to hit the cache.
4. `POST /admin/suites/jammy/refresh` (or path-equivalent)
   triggers an adoption pass for that suite only; other suites'
   adoption schedule is unaffected.
5. `apt-cacher-ultra ca rotate` produces a new CA whose
   fingerprint matches what `apt-cacher-ultra ca print` reports
   immediately after; `POST /admin/ca/rotate` does the same with
   the daemon running.
6. SIGHUP applies a new `upstream.allowed_host_regex`; a previously
   denied host is now permitted on next request without restart.
7. Read-role credential cannot reach any POST endpoint;
   write-role credential cannot reach `/metrics` or `/`.
8. Audit log: every admin-action invocation produces matching
   `admin_action_started` / `admin_action_completed` lines with
   non-empty `caller` and `job_id` fields.
9. SPEC7.md as-built, mirroring SPEC6.md structure (§1 goals,
   §2–§5 wire/storage, §6 request flow, §7 auth, §8 jobs, §9
   startup/shutdown, §10 admin endpoints, §13 layout, §15 DoD).
10. One-week production deployment with mutating endpoints exercised
    against the live daemon (one rotate, one gc/run, one cache/clear)
    showing no regression in `acu_request_total{outcome=…}` rates
    and no leaked goroutines.

---

## 7. Questions

All sixteen questions raised at scoping kickoff are resolved
(§1.3). No open questions remain. If implementation surfaces new
tensions the answers should be added here as resolutions, then
promoted to SPEC7.md.

---

## 8. Risks

The primary risk in Phase 7 is **scope creep into a control
plane that doesn't fit the daemon's footprint**. The endpoints
described here are deliberately a thin layer over existing
internal entry points (GC, adoption, cache, tlsmitm); they are
not a full RPC surface. Pressure to add OpenAPI generation,
server-side rendering of complex job results, multi-instance
coordination, etc. is the slope to resist.

A secondary risk is **auth-realm confusion**. An operator who
sets `mutating_htpasswd_path` to the SAME file as `htpasswd_path`
breaks the realm split silently. Mitigations:

- Startup config validation rejects the case `htpasswd_path ==
  mutating_htpasswd_path` with `same_htpasswd_path_for_both_realms`
  config-error log line.
- The two endpoints' WWW-Authenticate realm strings differ
  (`apt-cacher-ultra (read)` vs `apt-cacher-ultra (write)`) so
  curl prompts surface the realm.

A tertiary risk is **CA rotation breaking a cluster mid-rotation**.
Multi-cache deployments share a CA (SPEC6 Q16); a single instance's
rotate produces a CA divergent from peer caches. Phase 7 does not
solve this — multi-cache rotate is "rotate one, ansible the new CA
to peers, ansible reload, rotate again on each."

- Mitigation: `apt-cacher-ultra ca rotate` exit code is non-zero
  on first run if the CA file path is operator-supplied (i.e.
  shared with peers); the operator must `--force` the local
  rotate or rotate via ansible across the whole cluster. Default
  protects from the foot-gun.

A fourth risk is **hot-reload of `allowed_host_regex` widening
the SSRF surface during a transition window**. An operator who
intends to narrow the allowlist via SIGHUP could fat-finger and
widen it. Mitigations:

- `admin_action_completed action=config_reload` records the diff;
  a fat-finger is auditable post-hoc.
- The reload subset deliberately excludes `tls_mitm.enabled`,
  `cache.listen`, and the deny_target_ranges so the worst case
  is "more hosts allowed than intended" — caught by the next
  CONNECT log line that names the new permitted host.

A fifth risk is **mutating-endpoint denial-of-service** — a
malicious actor with the write credential triggers GC every
second. Phase 5's `acu_admin_action_total` rate is the
observability primitive; the per-source rate-limit is **not** in
Phase 7 (operators run the admin port behind nftables / network
ACL just like Phase 5).

---

## 9. Estimated effort

Modest — Phase 7 reuses far more existing machinery than it adds:

- ~2 days to scope (this document) + lock SPEC7.md.
- ~4 days to implement (jobs store, mutating endpoints, reload
  pipeline, write-role auth, ca rotate).
- ~3 days to write tests (unit + integration + chaos).
- ~1 day to update the status page template + the apt-cacher-ultra
  CA-rotate subcommand documentation.
- ~3 days for end-to-end verification on the test environment
  (gc/run during live traffic, ca rotate during live traffic,
  config reload during live traffic) and one-week production
  soak.

Total: ~13 working days from this scoping to a Phase 7 release
tag. About 20% larger than Phase 6 — Phase 6 added a CONNECT
verb + cert-issuing CA; Phase 7 adds four sub-features (three of
which are admin-listener handlers and one of which is a CLI
subcommand). Each sub-feature is small; the sum is moderate.

---

## Pointers

- Phase 1 SPEC: [SPEC.md](SPEC.md)
- Phase 2 SPEC: [SPEC2.md](SPEC2.md)
- Phase 3 SPEC: [SPEC3.md](SPEC3.md)
- Phase 4 SPEC: [SPEC4.md](SPEC4.md)
- Phase 5 SPEC: [SPEC5.md](SPEC5.md)
- Phase 6 SPEC: [SPEC6.md](SPEC6.md)
- Future-review-only items: [FUTURE-REVIEW.md](FUTURE-REVIEW.md)
