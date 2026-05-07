# apt-cacher-ultra — Phase 5 Scoping

Status: **scoping locked** (revision 1).
Last updated 2026-05-07. Next artifact: `SPEC5.md` modeled on
`SPEC4.md`'s structure.

This document gathers what Phase 5 is, the hooks Phases 1–4 left in
place for it, and the design decisions resolved during this scoping
pass before this becomes a locked SPEC5.md (parallel to SPEC.md /
SPEC2.md / SPEC3.md / SPEC4.md). Companion documents
[PHASE-2-SCOPING.md](PHASE-2-SCOPING.md),
[PHASE-3-SCOPING.md](PHASE-3-SCOPING.md), and
[PHASE-4-SCOPING.md](PHASE-4-SCOPING.md) record the parallel
exercises for earlier phases.

---

## 1. Goals

Phase 1 made the cache-hit path bulletproof. Phase 2 closed the
integrity and freshness loops. Phase 3 closed the service-continuity
loop. Phase 4 closed the storage-reclamation loop. Phase 5 closes
the **operator-visibility loop**:

1. **Expose a `/metrics` endpoint** in Prometheus exposition format
   so an operator running Prometheus / VictoriaMetrics / OpenObserve
   can scrape per-process counters, gauges, and histograms covering
   the request path, fetch path, freshness/adoption path, GC, and
   integrity scan. Phases 1–4 emit ~30 unique log events across 178
   call sites; Phase 5 turns each operationally-meaningful event into
   a counter-or-gauge increment alongside (not instead of) the
   existing log line.

2. **Expose a `/` status page** in HTML for human eyeballing — the
   "is the cache lagging upstream right now? which suites? for how
   long?" view that operators reach for during an incident.
   `inrelease_change_seen_at` is the canonical example (SPEC.md:492):
   the column already exists, the value is already recorded, and
   Phase 1 SPEC explicitly bookmarked it as a Phase 5 status-page
   item. The status page is the cheapest way to expose state that
   Prometheus would otherwise need a high-cardinality label set for.

3. **Expose a `/healthz` endpoint** as a simple liveness/readiness
   probe for systemd, Kubernetes, or any reverse proxy doing health
   checks. SPEC.md:577 explicitly mentions a Phase 5 health endpoint
   reporting "degraded" when the cache disk is full.

4. **Wire counters into the existing event stream.** No new
   semantics, no new chaos, no new race windows. The contract is:
   wherever today there is a `Logger.Info("adoption_success", ...)`
   call, after Phase 5 there is *also* a `metrics.Inc("adoption",
   "outcome=success")` call. Same goes for `gc_run_complete`,
   `freshness_check`, `bad_gateway`, and the rest. The handler's
   per-request log line gets paired with a counter+histogram pair
   keyed on `outcome`.

The four jobs are independent in code but share a single `[admin]`
configuration block, a single new HTTP listener, and a single new
log-event family for the admin listener's own lifecycle (bind /
unbind / scrape errors).

### 1.1 Non-goals (deferred)

Carried forward from earlier phases unchanged:

- TLS MITM listener (Phase 6).
- Source-package caching, multi-arch beyond amd64, pdiff (Phase 6+).
- Streaming-while-fetching as a singleflight optimization. Deferred
  to [FUTURE-REVIEW.md §1](FUTURE-REVIEW.md).
- Per-byte upstream read timeouts. Deferred to
  [FUTURE-REVIEW.md §1](FUTURE-REVIEW.md).
- Per-suite freshness cadence variation. Deferred to
  [FUTURE-REVIEW.md §2](FUTURE-REVIEW.md).
- Operator-triggered manual GC (admin endpoint or SIGUSR1).
  Carried forward from Phase 4 §1.1 — could be a Phase 5 add-on
  given the admin listener now exists, but is not part of the gating
  contract.

Newly deferred in Phase 5:

- **OpenTelemetry / OTLP exporters.** Prometheus exposition is the
  baseline. An OTLP push pipeline can be built on top later if any
  deployment runs an OTel collector; the metric names and label
  conventions chosen in §3.4 are designed to translate cleanly.
- **Distributed tracing.** No spans, no trace IDs propagated. The
  per-request log line already carries enough fields to correlate
  by client_addr + start time; tracing is a Phase 6+ topic.
- **Pull-based admin actions.** No `/admin/gc/run`,
  `/admin/cache/clear`, `/admin/suites/{path}/refresh` endpoints in
  Phase 5. The admin listener is **read-only**. (Phase 6 may
  introduce mutating endpoints behind explicit auth; the
  scoping-stage default of "no mutations" prevents the admin port
  from becoming an attack surface in any deployment that mistakenly
  exposes it.)
- **Per-client metrics.** No `{client_addr}` label cardinality. The
  proxy serves a small fleet of trusted apt clients; per-client
  breakdown is not load-bearing for operations.
- **Long-term storage.** The process exposes counters; whatever
  scrapes them is responsible for retention. No internal histograms
  beyond what Prometheus's own histogram type provides at scrape
  time.

### 1.2 Resolved during Phase 5 scoping

The eight design questions below were resolved with the operator
during scoping. Each resolution is normative for SPEC5.

- **Listener model.** A **separate HTTP listener** on
  `127.0.0.1:6789` by default, distinct from the proxy listener on
  `:3142`. Rationale: (a) the proxy listener accepts absolute-URL
  requests from apt clients; mixing admin paths with a request
  pipeline whose `ServeHTTP` already does scheme/host
  canonicalization is more code-surgery than benefit, (b) localhost
  binding by default keeps deployment topology and hot-package
  state unreachable from clients across the network without an
  explicit config change, (c) firewall rules become trivial
  (`127.0.0.1` is one rule), (d) systemd / Kubernetes liveness
  probes typically run on the same host or via a sidecar.
  *Alternative considered:* same listener with `/_acu/*` path
  prefix (apt-cacher-ng's choice). Rejected because operators of
  apt-cacher-ng routinely complained about unintended admin-page
  exposure on the proxy port.

- **Format.** Prometheus text exposition format
  (`text/plain; version=0.0.4`) for `/metrics`. HTML *or* JSON for
  `/` via content negotiation: `Accept: application/json` or
  `?format=json` returns JSON; everything else returns HTML. Plain
  `ok\n` 200 or `degraded\n` 503 for `/healthz`. The dual HTML/JSON
  view on `/` lets operators eyeball during an incident *and*
  consume programmatically (custom dashboards, scripts) without a
  second endpoint surface. (See §3.5 for content negotiation
  details.)

- **Counter-wiring strategy.** A thin `internal/metrics` package
  exposing `Inc(name, labels...)`, `Observe(name, value,
  labels...)`, `SetGauge(name, value)`. Each emit site adds one
  line next to the existing `Logger.Info(...)` call. The 30-event
  surface lives in ~8 helper functions (handler.logRequest,
  freshness.run, gc.runOnce, adoption.commit, etc.); most "wiring
  sites" collapse to one line per helper. *Alternative considered:*
  slog handler interception. Rejected because (a) you still
  maintain a registration table mapping event-name → metric type
  (counter / histogram / gauge), so the apparent "wire once" is
  illusory, (b) histograms need a numeric field with a known name,
  enforced only by convention, (c) renaming an event silently
  breaks the metric — Prometheus dashboards downstream go quiet
  with no compile-time signal, (d) the explicit form is reviewable
  and grep-friendly. The safety property — "every operationally
  meaningful event has a metric" — is enforced by code-review
  checklist plus a §6.3 test that scrapes /metrics after running
  each named code path.

- **Cardinality posture.** Prometheus metrics carry `{host}`
  labels for per-host metrics where the host is known at the emit
  site. The host set is bounded by `upstream.allowed_host_regex`
  (SPEC §6.6) — typical deployments allow ≤20 hosts; pathological
  ≤50. Prometheus handles this scale comfortably. `{suite_path}`
  labels are **not** added (suite_path is unbounded across
  deployments and changes over time as suites come and go;
  per-suite detail lives on the status page). The metric inventory
  in §3.4 marks each metric as `{host}`-labeled or unlabeled.

- **Healthz semantics.** `200 ok\n` when (a) cache directory is
  writable, (b) DB ping succeeds, (c) process is not in graceful
  shutdown. `503 degraded\n` on any of those failing, with the
  failing check name in an `X-Acu-Check-Failed:` header.
  *Alternative considered:* always-200 liveness + separate
  `/readyz` for readiness. Rejected as over-engineering for a
  single-process daemon — k8s-style liveness/readiness split
  applies to clusters, not single hosts.

- **Authentication.** **htpasswd-style HTTP Basic auth**, opt-in
  via `admin.htpasswd_file`. When the path is empty (default), no
  auth is enforced and the operator relies on bind-address as the
  trust boundary. When a path is configured, every admin request
  must present a valid `Authorization: Basic ...` header matching
  a `user:bcrypt-hash` line in the file. Bcrypt only — `$2a$`,
  `$2b$`, `$2y$` prefixes accepted; older Apache MD5 / SHA-1 /
  crypt formats rejected at startup with a config error.
  htpasswd-compatibility means `htpasswd -B -c file user` (the
  Apache utility's bcrypt mode) generates files this daemon
  consumes directly. The htpasswd file is re-read on mtime change
  (stat-on-each-request, parse-on-change) so operators can
  add/remove users without a restart. *Alternative considered:*
  bind-address-only with no auth. Rejected because the operator
  expects to expose the admin port behind a network in some
  deployments (multi-host monitoring, Prometheus scrape from a
  different host) and a reverse-proxy-only auth posture forces
  every operator to deploy nginx alongside this daemon. (See §3.8
  for the auth design.)

- **Default-on.** `admin.enabled = true` by default,
  `admin.listen = "127.0.0.1:6789"`. The endpoint is safe-by-default
  on loopback; the cost of a default-on admin listener is one
  extra `net.Listener`. Operators who genuinely want to disable it
  set `admin.enabled = false`. *Alternative considered:*
  default-off, opt-in. Rejected as operator-hostile — the most
  common reason to install this daemon is operations, and a
  metrics endpoint that is off-by-default makes the Day 1
  experience worse for no security gain (loopback bind is the
  security boundary, not the enabled flag).

- **Expensive-gauge refresh cadence.** A single refresher
  goroutine populates expensive gauges (`acu_blobs_db_count`,
  `acu_blobs_db_total_bytes`, `acu_pool_disk_bytes`) every 30s
  into in-memory cells; scrapes read cells. *Alternative
  considered:* recompute on every scrape. Rejected because
  Prometheus default scrape interval is 15s and some deployments
  scrape every 5s — running `du pool/` or `SELECT SUM(size) FROM
  blob` at that cadence is wasteful at multi-GiB scale.
  *Alternative considered:* expose these only on the status page,
  omit from `/metrics`. Rejected because cumulative bytes-on-disk
  is the most operationally-relevant gauge for a cache.

- **Build info source.** `runtime/debug.ReadBuildInfo()` (Go
  1.18+) for `acu_build_info{version,go_version,vcs_revision}`.
  No `-ldflags` injection, no Makefile changes — the Go toolchain
  embeds `vcs.revision`, `vcs.time`, and `vcs.modified` in the
  binary automatically when built from a git checkout. The
  Makefile's existing version handling carries forward unchanged.

---

## 2. What Phases 1–4 already left in place

Walking the existing code, prior phases deliberately seeded these
hooks that Phase 5 builds on:

| Prior-phase hook | Phase 5 use |
|---|---|
| Per-request log line via `handler.logRequest(...)` (`internal/handler/handler.go:242`+) with structured fields including `outcome`, `status`, `bytes_sent`, `duration_ms` | The single per-request counter + histogram pair derives directly from this call site. Adding metrics here wires every request type with no missed branches. |
| `Logger.Info("freshness_check", ..., "result", ...)` (`internal/freshness/*`) with `result` ∈ `{not_modified, unchanged, changed, failed}` | A 4-bucket counter `acu_freshness_check_total{result}`. |
| `adoption_success` / `adoption_run_failed` / `adoption_gpg_failed` / `adoption_member_mismatch` / `adoption_parse_failed` / `adoption_unpinned_suite` family (`internal/freshness/adoption.go` etc.) | `acu_adoption_total{outcome}` with the outcome label drawn from the existing event names. |
| `gc_run_complete` (Phase 4 SPEC4 §10.2) carrying `blobs_reaped`, `bytes_reclaimed`, `snapshots_orphan_reaped`, `snapshots_displaced_reaped`, `pool_orphans_repaired`, `pool_unlink_errors`, `duration_ms`, `phase` | Each numeric field becomes a `*_total` counter (cumulative-since-process-start) updated at run completion, plus `acu_gc_last_run_duration_seconds` and `acu_gc_last_run_unixtime` gauges. |
| `cache.GetSuiteFreshness(...)` returning `InReleaseChangeSeenAt` (`internal/cache/queries.go:193`) | Status page renders the per-suite "lagging since" view. SPEC.md:492 explicitly bookmarks this column as a Phase 5 status-page item. |
| `cache.ListSuites(ctx)` (`internal/cache/queries.go:226`) | Status page's "all tracked suites" table — one row per (host, suite_path) tuple. |
| `hostsem.HostCount()` (`internal/hostsem/sem.go:121`) | Gauge `acu_active_hosts` — point-in-time count of hosts with in-flight upstream requests. Cheap to read. |
| Single-listener architecture (`cmd/apt-cacher-ultra/main.go:105`+) with explicit listener-bind / Accept-defer separation (Phase 4 SPEC4 §4.2) | Phase 5 adds a *third* listener (alongside plain + TLS) following the same bind-early / Accept-late pattern. Same lifecycle, same graceful shutdown, no new wiring patterns. |
| `lifecycleCtx` (SPEC §9.5) | Admin listener honors graceful shutdown: a long-running scrape mid-shutdown returns 503 with `Connection: close`. |
| `runtime/debug.ReadBuildInfo()` (Go 1.18+) usable from main | Populates `acu_build_info{version,go_version,vcs_revision}` gauge=1 at startup. No `-ldflags` injection required — the Go toolchain already embeds VCS info in the binary. |
| Existing `[log]` config block conventions (`internal/config/config.go`) | Phase 5's new `[admin]` block follows the same parsing / validation conventions. Listen-address validation reuses the existing `validateListenAddr()` helper used by `[cache].listen` / `[cache].listen_tls`. |

Phase 5 is **purely additive** at the schema level (no DB changes,
no migration), at the request-path level (no changes to proxy
listener semantics, no changes to handler.go's dispatch, no
changes to fetch/cache/adoption/GC code other than one-line metric
increments alongside existing log calls), and at the operational
level (a new optional listener, off-by-default for non-loopback
exposure but on-by-default on loopback). The wire contracts (SPEC
§2), URL canonicalization (SPEC §3), per-host concurrency (SPEC
§9.3), graceful shutdown semantics (SPEC §9.5), the snapshot model
(SPEC2 §4), the hot-package model (SPEC3 §7.5), and the GC model
(SPEC4 §9.6) all carry forward unchanged.

---

## 3. Architectural sketch

### 3.1 Listener wiring

A third HTTP listener bound by `cmd/apt-cacher-ultra/main.go`
between the existing plain (`:3142`) and TLS listener wiring and
the cache.Open call. Bind order:

```
1.  net.Listen plain (cache.Listen)
2.  net.Listen TLS   (cache.ListenTLS, optional)
3.  net.Listen admin (admin.Listen, optional, default 127.0.0.1:6789)
4.  cache.Open
5.  startup-time tmp/ + staging/ sweeps
6.  GC startup pass (Phase 4)
7.  Accept() goroutines start in parallel
```

Why bind early but Accept late (parallel to SPEC4 §4.2):

- A bind failure (port in use, permission denied) should fail-fast
  before the cache directory is opened or any GC work begins.
  Surfacing the error with `cache.Open` not yet attempted is the
  cleanest exit.
- `Accept()` deferred until step 7 means an early-arriving scrape
  request sees a TCP connect with a small delay (the scrape
  client's `connect_timeout`), then a normal response — never RST
  / connection-refused. Important because Prometheus's scrape
  loop treats RST and refused as different signals than slow.

The admin listener uses a dedicated `*http.Server` with a smaller
`ReadHeaderTimeout` (5s) and `IdleTimeout` (30s) than the proxy
listener — admin requests are short-lived and frequent, not
long-streaming.

The admin handler is a `http.ServeMux` with three routes:
`GET /metrics`, `GET /`, `GET /healthz`. Any other path returns
404. Any non-GET method returns 405 with an `Allow: GET` header.
No POST, no PUT — the admin listener is read-only in Phase 5
(see §1.1).

### 3.2 The `internal/metrics` package

A small package, ~200 LoC, providing:

```go
package metrics

// Counter increments. Cumulative since process start.
func Inc(name string, labels ...string)

// Histogram observations. Bucket boundaries are picked per-metric
// at registration time (see §3.4).
func Observe(name string, value float64, labels ...string)

// Gauge sets. Point-in-time values. Setters update an in-memory
// cell read by /metrics renders.
func SetGauge(name string, value float64, labels ...string)

// Render writes the current registry to w in Prometheus text
// exposition format.
func Render(w io.Writer)
```

The package is goroutine-safe (sync.Mutex around the registry
map). Render holds the lock for the duration of the write — that's
fine because /metrics scrapes complete in low milliseconds even for
hundreds of metrics.

The package does **not** depend on any third-party Prometheus
client library. The exposition format is simple text; reimplementing
the renderer is ~50 LoC and avoids dragging in the
`prometheus/client_golang` dependency tree (which has its own
collectors and conventions that often clash with hand-rolled
metrics). Naming follows Prometheus conventions — `*_total` for
counters, `*_seconds`/`*_bytes` for typed gauges/histograms, `_count`
suffix for histogram observation count.

### 3.3 Counter-wiring sites

The 30 unique event names cataloged in §2 cover the bulk of the
metric surface. Wiring is mechanical: alongside each event emit,
a one-line `metrics.Inc(...)` or `metrics.Observe(...)` is added.
Examples:

```go
// internal/handler/handler.go:logRequest — the per-request line.
h.logRequest(r, host, path, outcome, status, bytes, false, 0, start)
metrics.Inc("acu_requests_total", "outcome="+outcome)
metrics.Observe("acu_request_duration_seconds",
    time.Since(start).Seconds(), "outcome="+outcome)
metrics.Observe("acu_response_bytes", float64(bytes), "outcome="+outcome)
```

```go
// internal/freshness — adoption_success
g.cfg.Logger.Info("adoption_success", ...)
metrics.Inc("acu_adoption_total", "outcome=success")
metrics.Observe("acu_adoption_duration_seconds", duration.Seconds(),
    "outcome=success")
```

```go
// internal/gc/gc.go — gc_run_complete
g.cfg.Logger.Info("gc_run_complete", ...,
    "blobs_reaped", res.blobsReaped, ...)
metrics.Inc("acu_gc_runs_total", "phase="+phase)
metrics.IncBy("acu_gc_blobs_reaped_total", float64(res.blobsReaped))
metrics.IncBy("acu_gc_bytes_reclaimed_total", float64(res.bytesReclaimed))
metrics.SetGauge("acu_gc_last_run_unixtime", float64(time.Now().Unix()))
metrics.Observe("acu_gc_run_duration_seconds", duration.Seconds(),
    "phase="+phase)
```

The convention is: one metric line per log emit, placed
immediately after the log call. Code-review rule (added to a
project-level guide): a new `Logger.Info("foo_xxx", ...)` call
without a corresponding `metrics.*("acu_foo_xxx_total", ...)` is
a review-failing omission unless the event is genuinely
unmeasurable (e.g. one-time startup banners).

### 3.4 Metric inventory

The full list to be enumerated in SPEC5.md §10. Preliminary
sketch of the surface area, by source:

Metrics are tagged `{host}` (the upstream `canonical_host`) when
the host is known at the emit site. The `{host}` label set is
bounded by `upstream.allowed_host_regex` (SPEC §6.6) — typical
deployments allow ≤20 distinct hosts. Pre-host-resolution emits
(method-not-allowed, bad-request before parse) carry no `{host}`
label.

#### 3.4.1 Request path (handler.go)

- `acu_requests_total{outcome,host}` — counter. Outcome ∈ {`hit`,
  `miss`, `hit_stale`, `hit_coalesced`, `method_not_allowed`,
  `bad_request`, `forbidden`, `upstream_status`, `bad_gateway`,
  `cache_write_failed`, `client_canceled`, `error`,
  `unvouched_deb_refused`, `unvouched_deb_passthrough_no_coverage`}.
  `host` is the empty string for outcomes that fail before host
  parsing (`method_not_allowed`, `bad_request`).
- `acu_request_duration_seconds{outcome,host}` — histogram.
  Buckets: 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30, 60.
- `acu_response_bytes{outcome,host}` — histogram.
  Buckets: 1024, 4096, 65536, 262144, 1048576, 10485760, 104857600,
  1073741824 (1KiB → 1GiB).
- `acu_inflight_requests` — gauge (handler.activeWG count). No
  host label (gauge is process-wide, not partitionable cleanly).

#### 3.4.2 Fetch path (internal/fetch)

- `acu_fetch_total{outcome,host}` — counter. Outcome from upstream
  status class (`2xx`, `3xx`, `4xx`, `5xx`, `timeout`,
  `connect_failed`, `dns_failed`).
- `acu_fetch_duration_seconds{outcome,host}` — histogram.
- `acu_fetch_retries_total{host}` — counter, one per retry attempt.
- `acu_active_hosts` — gauge from `hostsem.HostCount()`. No host
  label (the metric *is* the host count).
- `acu_per_host_inflight{host}` — gauge per host from `hostsem`,
  the per-host slot occupancy. Useful for diagnosing one slow
  upstream blocking others.

#### 3.4.3 Freshness / adoption (internal/freshness)

- `acu_freshness_check_total{result,host}` — counter, result ∈
  {`not_modified`, `unchanged`, `changed`, `failed`}.
- `acu_adoption_total{outcome,host}` — counter, outcome ∈
  {`success`, `parse_failed`, `gpg_failed`, `member_mismatch`,
  `unpinned_suite`, `run_failed`, `form_drift`}.
- `acu_adoption_duration_seconds{outcome,host}` — histogram.
- `acu_hot_prefetch_total{outcome,host}` — counter, outcome ∈
  {`started`, `complete`, `partial`, `deb_failed`, `hash_mismatch`}.
- `acu_adoption_heartbeat_failures_total{host}` — counter (Phase 4
  heartbeat failure).

#### 3.4.4 Integrity (internal/integrity)

- `acu_at_rest_scans_total` — counter (one per scan).
- `acu_at_rest_corruption_total` — counter (per corruption found).
- `acu_hash_validation_failure_total{phase}` — counter, phase ∈
  {`fetch`, `at_rest`}.
- `acu_pool_corruption_during_adoption_total` — counter.

#### 3.4.5 GC (internal/gc) — Phase 4 sourced

- `acu_gc_runs_total{phase}` — counter, phase ∈ {`startup`,
  `periodic`}.
- `acu_gc_blobs_reaped_total` — counter.
- `acu_gc_bytes_reclaimed_total` — counter.
- `acu_gc_snapshots_orphan_reaped_total` — counter.
- `acu_gc_snapshots_displaced_reaped_total` — counter.
- `acu_gc_pool_orphans_repaired_total` — counter.
- `acu_gc_pool_unlink_errors_total` — counter.
- `acu_gc_run_duration_seconds{phase}` — histogram.
- `acu_gc_last_run_unixtime{phase}` — gauge.

#### 3.4.6 Cache state (gauges, refreshed every 30s)

- `acu_blobs_db_count` — gauge (SELECT COUNT(*) FROM blob).
- `acu_blobs_db_total_bytes` — gauge (SELECT SUM(size) FROM blob).
- `acu_blobs_zero_refcount_backlog` — gauge (count of blobs
  awaiting GC grace).
- `acu_pool_disk_bytes` — gauge (size of pool/ on disk).
- `acu_suites_tracked` — gauge (suite_freshness rows).
- `acu_url_paths_tracked` — gauge.
- `acu_snapshots_current` — gauge (suite_freshness with
  current_snapshot_id NOT NULL).
- `acu_snapshots_displaced` — gauge.

#### 3.4.7 Build / process info

- `acu_build_info{version,go_version,vcs_revision}` — gauge=1 at
  startup. Sourced from `runtime/debug.ReadBuildInfo()`.
- `acu_process_start_unixtime` — gauge=startup time, set once.
- Standard Go process metrics (`process_cpu_seconds_total`,
  `process_resident_memory_bytes`, etc.) — emitted via the
  metrics package's process collector, modeled on
  Prometheus client conventions but reimplemented locally (~30
  LoC reading /proc).

#### 3.4.8 Admin listener self-metrics

- `acu_admin_scrape_total` — counter (`/metrics` scrapes served).
- `acu_admin_scrape_duration_seconds` — histogram.

### 3.5 Status page design (`GET /`)

#### 3.5.1 Content negotiation

The root path serves either HTML or JSON depending on the request:

1. `?format=json` query parameter → JSON, regardless of Accept.
2. Otherwise, `Accept` header: if `application/json` is acceptable
   and `text/html` is not, → JSON.
3. Otherwise → HTML (Content-Type `text/html; charset=utf-8`).

The query parameter wins because operators bookmark URLs and curl
scripts find query strings easier to compose than custom headers.
JSON Content-Type is `application/json; charset=utf-8`.

The HTML page renders a "View as JSON →" link at the top pointing
to `/?format=json`, so the JSON view is discoverable from the
browser.

#### 3.5.2 HTML rendering

A single HTML page rendered by Go's `html/template` at request
time. No JavaScript, no external assets — one self-contained
page, browser-renderable offline. **Auto-escapes via
`html/template`** (never `text/template`) — see §9 risk note.
Layout:

```
== apt-cacher-ultra status ==

Process:  apt-cacher-ultra v0.x.y, started 2026-05-07 14:32 UTC,
          uptime 12h 14m, build sha abcdef0
Cache:    /var/cache/apt-cacher-ultra, 47.2 GiB used,
          18743 blobs, 8421 url_paths
Listener: 0.0.0.0:3142 (proxy), 127.0.0.1:6789 (admin)

== Suites ==
[table of suite_freshness]
host        suite_path             last_check  last_success  current_snapshot  inrelease_change_seen_at
ubuntu...   ubuntu/dists/jammy     14:30 UTC   14:30 UTC     adopted_at:13:50  -
archive...  debian/dists/bookworm  14:31 UTC   12:15 UTC     adopted_at:08:00  10:42 UTC (lagging 4h12m)

== GC ==
last_run:        2026-05-07 14:00 UTC (10s ago, periodic)
blobs_reaped:    72
bytes_reclaimed: 1.2 GiB
zero_refcount_backlog: 3 blobs awaiting grace
displaced snapshots retained per suite: 3

== Hot packages ==
[list of hot package set, top 20 by request count, with last_requested_at]

== Recent adoptions ==
[table of last 20 adoptions across all suites]

== Active hosts ==
host                 inflight   slot_capacity
archive.ubuntu.com   2          8
```

Tables use plain `<table>`. CSS is inlined in a `<style>` block;
the page targets nice-looking type/spacing without external
fonts or JavaScript. Server-side rendering only.

The page is bounded — top-N lists capped at 20 rows each. Total
page size targets <200 KiB even for a fully-stocked cache.

#### 3.5.3 JSON shape

The JSON form is the same data, machine-readable. Schema (locked
in SPEC5; backwards-compatible additions only thereafter):

```json
{
  "process": {
    "version": "v0.x.y",
    "started_unixtime": 1746628320,
    "uptime_seconds": 44040,
    "vcs_revision": "abcdef0",
    "go_version": "go1.22.1"
  },
  "cache": {
    "dir": "/var/cache/apt-cacher-ultra",
    "bytes_used": 50678865920,
    "blob_count": 18743,
    "url_path_count": 8421,
    "zero_refcount_backlog": 3
  },
  "listeners": [
    {"role": "proxy", "addr": "0.0.0.0:3142"},
    {"role": "admin", "addr": "127.0.0.1:6789"}
  ],
  "suites": [
    {
      "host": "archive.ubuntu.com",
      "suite_path": "ubuntu/dists/jammy",
      "last_check_unixtime": 1746671400,
      "last_success_unixtime": 1746671400,
      "current_snapshot_id": 142,
      "current_snapshot_adopted_at_unixtime": 1746668400,
      "inrelease_change_seen_at_unixtime": null
    }
  ],
  "gc": {
    "last_run_unixtime": 1746671400,
    "last_run_phase": "periodic",
    "last_run_blobs_reaped": 72,
    "last_run_bytes_reclaimed": 1288490188,
    "displaced_per_suite_kept": 3
  },
  "hot_packages": [
    {"package": "linux-image-...", "request_count": 412,
     "last_requested_unixtime": 1746671280}
  ],
  "recent_adoptions": [
    {"host": "archive.ubuntu.com",
     "suite_path": "ubuntu/dists/jammy",
     "outcome": "success",
     "adopted_at_unixtime": 1746668400,
     "duration_seconds": 4.21}
  ],
  "active_hosts": [
    {"host": "archive.ubuntu.com",
     "inflight": 2,
     "slot_capacity": 8}
  ]
}
```

Top-level keys are stable; new keys may be added (consumers
should ignore unknown keys per JSON convention). Field types are
fixed.

### 3.6 Healthz design

Three stateless checks, each ≤5ms:

1. **Cache directory writable**: `os.OpenFile(cache_dir +
   "/.healthz", O_CREATE|O_WRONLY|O_TRUNC, 0644)`, write 1 byte,
   close, unlink. Exercises the actual filesystem under the cache.
2. **DB pingable**: `db.PingContext(ctx)` with 1s timeout.
3. **Process not in graceful shutdown**: read a flag set by the
   shutdown handler before `Server.Shutdown` is called.

If all three pass: `200 ok\n`. If any fails: `503 degraded\n`,
with the failing check name in a header (`X-Acu-Check-Failed:
db_ping`). No body details — operators read /metrics + status
page for diagnosis; /healthz is for binary-decision automation.

### 3.7 Refresher goroutine

Started after the admin listener binds. Runs a loop with
30s sleep + recompute of expensive gauges:

- `acu_blobs_db_count` — `SELECT COUNT(*) FROM blob`.
- `acu_blobs_db_total_bytes` — `SELECT SUM(size) FROM blob`.
- `acu_blobs_zero_refcount_backlog` — `SELECT COUNT(*) FROM blob
  WHERE refcount <= 0 AND refcount_zeroed_at IS NOT NULL`.
- `acu_pool_disk_bytes` — `du`-style filepath.Walk on pool/. The
  Phase 4 §9.6.4 startup pool scan already does this work in O(N)
  parallel; the Phase 5 refresher is single-threaded and runs
  every 30s. (For very large pools this may want to skip until
  the next interval if the prior recompute hasn't finished —
  guard with a "refresh in progress" boolean.)
- `acu_suites_tracked`, `acu_url_paths_tracked`,
  `acu_snapshots_current`, `acu_snapshots_displaced` — each a
  single COUNT query.

The 30s cadence balances scrape freshness (Prometheus default
scrape every 15s) against query cost. An operator running
many-GiB caches can dial it lower with `admin.gauge_refresh
= "60s"` if needed.

### 3.8 htpasswd auth

When `admin.htpasswd_file` is non-empty, every admin request must
present a valid `Authorization: Basic ...` header.

#### 3.8.1 File format

Apache htpasswd format, bcrypt-only. One credential per line:

```
sean:$2y$10$abcdef...
ops:$2b$12$ghijkl...
```

The hash prefix selects the algorithm. Phase 5 accepts
`$2a$`, `$2b$`, `$2y$` (all bcrypt variants — Go's
`golang.org/x/crypto/bcrypt` accepts all three with
`bcrypt.CompareHashAndPassword`). Older formats (`$apr1$` Apache
MD5, `{SHA}` SHA-1, `crypt(3)` DES) are **rejected at startup**
with a config error naming the offending line. Reasoning: those
formats are cryptographically broken or weak, and accepting them
would invite operators to use them.

Generating a file with the standard Apache utility:

```sh
htpasswd -B -c /etc/apt-cacher-ultra/htpasswd sean
htpasswd -B    /etc/apt-cacher-ultra/htpasswd ops
```

`-B` selects bcrypt; `-c` creates the file (omit for subsequent
appends). The daemon imposes no restrictions on usernames beyond
"no colons, no whitespace, no embedded newlines" (which the
Apache utility already enforces).

#### 3.8.2 File parsing and reload

At startup, the file is parsed once: each non-empty,
non-comment line is split on the first `:`, the hash prefix
validated, and `(username → hash)` stored in a map. Parse errors
fail startup with a clear error message naming the file and line
number.

At request time, the file's `os.Stat` mtime is checked against
the cached parse timestamp. If mtime has advanced, the file is
re-parsed and the map atomically swapped. Parse failures during
reload **do not** swap the map — the daemon keeps serving with
the prior credentials and emits an `htpasswd_reload_failed` Warn.
This means a temporarily-broken htpasswd file (mid-edit) does
not lock operators out.

The stat-on-each-request cost is one syscall per admin request
— negligible against the network round-trip and TLS handshake
costs of typical admin clients.

#### 3.8.3 Auth middleware

Every admin request flows through:

```go
func authMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if !authRequired() {
            next.ServeHTTP(w, r)
            return
        }
        user, pass, ok := r.BasicAuth()
        if !ok {
            w.Header().Set("WWW-Authenticate", `Basic realm="apt-cacher-ultra admin"`)
            http.Error(w, "auth required", http.StatusUnauthorized)
            return
        }
        if !checkPassword(user, pass) {
            // Constant-time response delay to blunt user-enumeration
            // timing attacks (the bcrypt cost ensures the correct-user
            // wrong-password path is slow; ensure wrong-user path is
            // also slow by hashing a sentinel).
            time.Sleep(...)
            http.Error(w, "auth failed", http.StatusUnauthorized)
            return
        }
        next.ServeHTTP(w, r)
    })
}
```

The auth middleware wraps the entire admin mux — `/metrics`,
`/`, and `/healthz` all require auth when `htpasswd_file` is
configured. Reasoning for `/healthz` requiring auth: a
publicly-readable `/healthz` exposes service-up state to
attackers probing for live caches; the operational concern about
"my k8s liveness probe needs to read /healthz" is moot because
either (a) the probe runs on the same host (loopback bind, no
auth) or (b) the operator can configure the probe to send Basic
auth.

#### 3.8.4 Non-loopback safety

When `admin.listen` resolves to a non-loopback address AND
`admin.htpasswd_file` is empty, a **`admin_unauthenticated_non_loopback`
Warn** is emitted at startup. Parallel to `gc_disabled` and
`refuse_unvouched_debs_inert` — the operator made a deliberate
choice to expose admin endpoints without auth, and the warning is
the operational signal.

---

## 4. Schema migration

**None.** Phase 5 is observation-only; no DB changes, no migration.

---

## 5. Configuration block

```toml
[admin]
enabled         = true              # master switch; default true
listen          = "127.0.0.1:6789"  # bind address; loopback by default
htpasswd_file   = ""                # bcrypt htpasswd; empty = no auth
gauge_refresh   = "30s"             # expensive-gauge refresh cadence
read_timeout    = "5s"              # HTTP ReadHeaderTimeout
idle_timeout    = "30s"             # HTTP IdleTimeout
```

Validation:

- `admin.listen` is host:port, port 1-65535, host either an IP or
  empty (means all interfaces). Reuses `validateListenAddr()`.
- `admin.htpasswd_file` if non-empty: file must exist, be readable,
  and parse cleanly (every line is `user:$2[aby]$...`). Parse
  failures at startup are config errors (process exits non-zero
  with the offending file/line in the error message).
- `admin.gauge_refresh > 0` and ≤ 1h.
- `admin.read_timeout > 0` and ≤ 1m.
- `admin.idle_timeout > 0` and ≤ 10m.

If `admin.enabled = false` the listener is not bound; `/metrics`,
`/`, `/healthz` are unreachable (the operator is implicitly opting
out of all observability).

Startup warnings:

- **`admin_disabled`** Warn when `admin.enabled = false`, parallel
  to `gc_disabled` (SPEC4 §10.2).
- **`admin_unauthenticated_non_loopback`** Warn when
  `admin.listen` resolves to anything other than 127.0.0.1, ::1,
  or "localhost" AND `admin.htpasswd_file` is empty — the
  operator has widened the trust boundary without providing auth.
- **`admin_authenticated`** Info when `admin.htpasswd_file` is
  non-empty and parses successfully, with the user count.

---

## 6. Test strategy

Three test layers, parallel to Phase 4:

### 6.1 Unit tests (internal/metrics)

- Counter Inc, Render — verify exposition format.
- Histogram Observe, bucket boundaries, _count and _sum fields.
- Gauge SetGauge — verify last-write-wins.
- Concurrent Inc/Render — verify no torn reads under -race.

### 6.2 Endpoint tests (cmd/apt-cacher-ultra integration)

- `GET /metrics` → 200, Content-Type `text/plain; version=0.0.4;
  charset=utf-8`, body parses as Prometheus exposition.
- `GET /` → 200, Content-Type `text/html`, body contains
  `<title>` and the suite table heading.
- `GET /` with `Accept: application/json` → 200, Content-Type
  `application/json; charset=utf-8`, body parses as JSON with
  the §3.5.3 schema.
- `GET /?format=json` → 200, JSON regardless of Accept.
- `GET /?format=json` with `Accept: text/html` → JSON wins
  (query param override).
- `GET /healthz` → 200, body `"ok\n"`.
- `GET /healthz` with cache_dir made unwritable → 503, body
  `"degraded\n"`, header `X-Acu-Check-Failed: cache_dir`.
- `POST /metrics` → 405, header `Allow: GET`.
- `GET /unknown` → 404.
- Admin listener with `admin.enabled = false` → no listener bound,
  port refused.

#### 6.2.1 Auth tests

- `admin.htpasswd_file = ""` (default): all requests succeed
  without `Authorization` header.
- `admin.htpasswd_file = <valid>`: `GET /metrics` without auth →
  401 with `WWW-Authenticate: Basic realm=...`.
- `GET /metrics` with valid Basic credentials → 200.
- `GET /metrics` with valid user but wrong password → 401, after a
  bcrypt-cost delay (timing parity with the no-such-user path).
- `GET /metrics` with no-such-user → 401, with the same delay.
- `GET /healthz` requires auth too (no carve-out).
- htpasswd file rewritten with new credentials → next request
  picks up the change (mtime-driven reload).
- htpasswd file rewritten with parse error → next request still
  serves with the prior credentials, `htpasswd_reload_failed`
  Warn emitted.
- Startup with htpasswd file containing `$apr1$...` (Apache MD5)
  → process exits non-zero with config error.
- Startup with htpasswd file containing one valid bcrypt and one
  malformed line → process exits non-zero with the line number.

### 6.3 Counter-wiring tests

- Issue a request that produces a known outcome (`hit`, `miss`,
  `forbidden`, `bad_gateway`), then scrape `/metrics` and verify
  the corresponding `acu_requests_total{outcome=...}` counter
  incremented by exactly 1.
- Run a synthetic GC pass with known reaped count, then scrape
  `/metrics` and verify `acu_gc_blobs_reaped_total` incremented.
- Trigger a freshness check that returns 304, scrape, verify
  `acu_freshness_check_total{result=not_modified}` incremented.

No new chaos tests. Phase 5 is observability of existing chaos,
not new chaos.

---

## 7. Definition of done

1. All Phase 1/2/3/4 tests still pass.
2. `internal/metrics` package implemented with unit tests under
   `-race`.
3. Admin listener bound, all three endpoints reachable on default
   loopback config.
4. Counter wiring at all ~30 emit sites, verified by §6.3 tests.
5. htpasswd auth implemented per §3.8; auth tests pass per §6.2.1.
6. Status page renders correctly (HTML and JSON) for an empty
   cache, a populated cache, and a cache with a lagging-upstream
   suite.
7. `/healthz` flips to 503 within one check cycle when cache_dir
   becomes unwritable; recovers within one check cycle when
   restored.
8. SPEC5.md as-built, mirroring SPEC4.md structure.
9. One-week production deployment with default `admin.*` showing
   stable scrape latency (<10ms p99), no admin-listener
   resource leaks, and the status page renders correctly under
   real traffic.

---

## 8. Risks

The primary risk in Phase 5 is **scope creep into Phase 6 territory**:

- Mutating endpoints (`/admin/gc/run`, `/admin/cache/clear`) are
  attractive but expose attack surface; deferred to Phase 6.
- TLS for the admin port is attractive once non-loopback bind is
  chosen; deferred to Phase 6.
- OpenTelemetry / OTLP push pipelines are attractive for
  cluster-scrapes; deferred.
- Distributed tracing / span propagation is attractive for
  multi-cache deployments; deferred.

A secondary risk is **counter-cardinality drift**: the §1.2
cardinality decision adds `{host}` labels (bounded by
`upstream.allowed_host_regex`) but explicitly excludes
`{suite_path}` and `{client_addr}`. An operator who later adds an
unbounded label "just for one metric" can blow up Prometheus
storage. The §3.4 metric inventory should be treated as the
exhaustive label set; new metrics added later should pass
cardinality review.

A tertiary risk is **status-page injection**: rendered values come
from cache state (suite paths, hostnames). All template rendering
must use Go's `html/template` (auto-escapes), never `text/template`
or hand-built HTML concatenation. The JSON path uses
`encoding/json` which escapes by spec.

A fourth risk is **htpasswd timing leaks**: a wrong-user 401
response that is fast and a wrong-password 401 response that is
slow lets an attacker enumerate valid usernames. §3.8.3
requires the no-such-user path to perform a sentinel bcrypt
comparison so both error paths take the same wallclock.

A fifth risk is **htpasswd file mode**: a world-readable
htpasswd file leaks bcrypt hashes to local users. The daemon
should not enforce file mode (operators may have legitimate
reasons for 0644), but SPEC5 should recommend 0600 ownership
matching the daemon user.
