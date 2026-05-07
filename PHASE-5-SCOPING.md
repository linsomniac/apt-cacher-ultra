# apt-cacher-ultra — Phase 5 Scoping

Status: **scoping locked** (revision 3).
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
  site. The host set is **bounded by both** (a)
  `upstream.allowed_host_regex` (SPEC §6.6) and (b) a metrics-
  package per-metric series cap. Important caveat on (a): the
  default regex (config.go:25) admits arbitrary subdomains under
  `ubuntu.com`, `debian.org`, etc. (`^([a-z0-9-]+\.)*ubuntu\.com$`),
  so a malicious or misconfigured client can in principle generate
  unbounded distinct host strings (`random1.ubuntu.com`,
  `random2.ubuntu.com`, …) — each request would 502 (no such mirror)
  but each creates a permanent series. Defense: the metrics package
  enforces a per-metric series cap (default 1024); when the cap is
  reached, the new label tuple is dropped from the increment with a
  one-shot `metrics_series_cap_reached` Warn. Operators running in
  multi-tenant or hostile-LAN environments should additionally
  tighten `upstream.allowed_host_regex` to literal hosts (e.g.
  `^archive\.ubuntu\.com$`). `{suite_path}` labels are **not** added
  (suite_path is unbounded across deployments and changes over time
  as suites come and go; per-suite detail lives on the status
  page). The metric inventory in §3.4 marks each metric as
  `{host}`-labeled or unlabeled. The cap design lives in §3.2; the
  metric inventory clarifies §3.4 prelude.

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

- **Build info source.** Hybrid: `version` comes from
  `main.Version` (the existing `Makefile` already injects this via
  `-ldflags '-X main.Version=$(VERSION)'` — see Makefile:6).
  `vcs_revision` and `go_version` come from
  `runtime/debug.ReadBuildInfo()` (Go 1.18+), which the toolchain
  populates automatically. **Architectural constraint:** the
  `internal/admin` and `internal/metrics` packages cannot import
  `main` (Go's `internal/` rule prohibits it). Instead,
  `cmd/apt-cacher-ultra/main.go` reads its own package-level
  `Version` string and `debug.ReadBuildInfo()` once at startup,
  then passes the resulting `BuildInfo` struct (a tiny three-field
  value type defined in `internal/admin` or `internal/metrics`)
  into the admin listener constructor and the metrics-registration
  helper. The constructor sets
  `acu_build_info{version,go_version,vcs_revision} = 1`. Tests in
  `internal/admin` build a synthetic `BuildInfo` directly. The
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
| `gc_run_complete` (Phase 4 SPEC4 §10.2) carrying `blobs_reaped`, `bytes_reclaimed`, `orphan_candidates_reaped`, `displaced_reaped`, `pool_orphans_repaired`, `pool_unlink_errors`, `duration_ms`, `phase` | Each numeric field becomes a `*_total` counter (cumulative-since-process-start) updated at run completion. Counter names mirror the source field names: `acu_gc_orphan_candidates_reaped_total` and `acu_gc_displaced_reaped_total`. Plus `acu_gc_last_run_duration_seconds` and `acu_gc_last_run_unixtime` gauges. |
| `cache.GetSuiteFreshness(...)` returning `InReleaseChangeSeenAt` (`internal/cache/queries.go:193`) | Status page renders the per-suite "lagging since" view. SPEC.md:492 explicitly bookmarks this column as a Phase 5 status-page item. |
| `cache.ListSuites(ctx)` (`internal/cache/queries.go:226`) | Status page's "all tracked suites" table — one row per (host, suite_path) tuple. |
| Single-listener architecture (`cmd/apt-cacher-ultra/main.go:105`+) with explicit listener-bind / Accept-defer separation (Phase 4 SPEC4 §4.2) | Phase 5 adds a *third* listener (alongside plain + TLS) following the same bind-early / Accept-late pattern. Same lifecycle, same graceful shutdown, no new wiring patterns. |
| `lifecycleCtx` (SPEC §9.5) | Admin listener honors graceful shutdown: a long-running scrape mid-shutdown returns 503 with `Connection: close`. |
| `main.Version` injected by Makefile `-ldflags '-X main.Version=$(VERSION)'` (Makefile:6) plus `runtime/debug.ReadBuildInfo()` for VCS revision and Go version | Populates `acu_build_info{version,go_version,vcs_revision}` gauge=1 at startup. The Makefile already drives `main.Version`. `cmd/apt-cacher-ultra/main.go` reads it (and `debug.ReadBuildInfo()`) once at startup and passes the resulting `BuildInfo` struct into the admin listener constructor — `internal/admin` and `internal/metrics` cannot import `main` directly. |
| `hostsem.HostCount()` (`internal/hostsem/sem.go:121`) — host-set size only, no occupancy detail | A new `Snapshot()` method on `*Sem` is required for per-host inflight gauges + status-page table. Returns `map[string]struct{Inflight, Capacity int}`. Phase 5 deliverable: extend `internal/hostsem` with `Snapshot()`. |
| Existing `[log]` config block conventions (`internal/config/config.go`) | Phase 5's new `[admin]` block follows the same parsing / validation conventions. Listen-address validation reuses the existing `validateListenAddr()` helper used by `[cache].listen` / `[cache].listen_tls`. |

Phase 5 is **additive** at the schema level (no DB changes, no
migration) and at the operational level (a new optional listener
on loopback by default). The request path is unchanged: proxy
listener semantics, handler dispatch, fetch / cache / adoption /
GC code paths all carry forward identically; the only changes are
metric-emit lines added alongside (not in place of) existing log
emits, plus explicit instrumentation around fetch outcomes (the
fetch package today logs only retries — outcome
classification is added per §3.3.1). The single non-additive
internal change is `internal/hostsem`: a new `Snapshot()` method
is added to expose per-host inflight + capacity for the
`acu_per_host_inflight{host}` gauge and the status-page table.
The wire contracts (SPEC §2), URL canonicalization (SPEC §3),
graceful shutdown semantics (SPEC §9.5), the snapshot model
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
5.  cache.SweepTmp (startup-time tmp/ sweep; SPEC §4.2)
6.  GC startup pass (Phase 4 §9.6)
7.  Accept() goroutines start in parallel
```

(Note: there is no startup `staging/` sweep — SPEC4 §4.2 documents
that staging directories from cancelled adoptions are reaped by
the §9.6 orphan-candidate-snapshot GC pass, not by a startup
sweep.)

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

A small package, ~400 LoC, exposing **typed metric handles**
(Counter, Histogram, Gauge) declared once at package init and
called by short, label-positional methods at the emit site. This
is what `internal/metrics/metrics.go` already implements; SPEC5
contracts the existing API.

```go
package metrics

// Registry holds all registered metrics. The package-level Default
// is what production code uses; tests construct private registries
// via NewRegistry() to isolate state.
type Registry struct { ... }

func NewRegistry() *Registry
var Default *Registry

// Typed handles. Each is declared once at package init time on a
// registry; subsequent .Inc / .Observe / .Set calls are
// label-positional.
type Counter struct { ... }
type Histogram struct { ... }
type Gauge struct { ... }

func NewCounter(name, help string, labelNames ...string) *Counter
func NewHistogram(name, help string, buckets []float64, labelNames ...string) *Histogram
func NewGauge(name, help string, labelNames ...string) *Gauge

// Hot-path methods. labelValues must match the declared labelNames
// arity (mismatch panics — caught by tests, not at runtime).
func (c *Counter) Inc(labelValues ...string)
func (c *Counter) Add(delta float64, labelValues ...string)

func (h *Histogram) Observe(value float64, labelValues ...string)

func (g *Gauge) Set(value float64, labelValues ...string)
func (g *Gauge) Inc(labelValues ...string)
func (g *Gauge) Dec(labelValues ...string)
func (g *Gauge) Add(delta float64, labelValues ...string)
func (g *Gauge) Reset()  // refresher-driven gauges only

// Render writes the registry to w in Prometheus text exposition
// format. Stable series order across renders.
func Render(w io.Writer)
func (r *Registry) Render(w io.Writer)
```

Why typed handles instead of string-keyed `metrics.Inc(name,
labels...)`: a typo in the metric name silently creates a new
metric that the test suite cannot easily catch. Typed handles
fail at compile time. Label-arity mismatches panic at the call
site, which a unit test exercises once per metric.

The package is goroutine-safe (per-metric sync.Mutex; one acquire
per Inc/Observe/Set/Render call). Hot path: one lock + one map
lookup. The package does **not** depend on any third-party
Prometheus client library. The exposition-format renderer is ~50
LoC and avoids dragging in the `prometheus/client_golang`
dependency tree.

**Render hold-time.** The renderer builds the per-metric output
into a `strings.Builder` *under* the metric's lock and then writes
the buffer to `io.Writer` *outside* the lock. Hot-path
Inc/Observe/Set blocking is bounded by the per-metric series count
(memory operations) regardless of the writer's speed — important
because `/metrics` is served to a remote scraper over HTTP, and a
slow consumer must not stall request-path counters. (Already
implemented in `internal/metrics/metrics.go` as of Phase 5
revision 3.)

**Per-metric series cap.** Each labeled metric carries a
`maxSeries` cap (default 1024). When a new label-value tuple
would push the metric's series count past the cap, the
Inc/Observe/Set call **silently drops** the increment and a
one-shot `metrics_series_cap_reached` Warn fires for that metric
(one log line per metric per process lifetime, so a sustained
flood does not log-storm). Existing series continue to update
normally. The cap is the back-stop against the §1.2 cardinality
risk: even when `upstream.allowed_host_regex` is wide and a
malicious client floods distinct host strings, the metric grows
to at most 1024 series (per metric). The cap is set at
construction:

```go
func NewCounter(name, help string, labelNames ...string) *Counter
// Default cap: 1024.

func NewCounterWithCap(name, help string, maxSeries int, labelNames ...string) *Counter
// Explicit cap. 0 = unbounded (use only for unlabeled or
// known-tiny-cardinality metrics).
```

Operators who legitimately need >1024 distinct values per metric
should narrow the regex first; raising the cap without that
narrowing trades one alarm for another.

Naming follows Prometheus conventions — `*_total` for counters,
`*_seconds`/`*_bytes` for typed gauges/histograms, `_count`
suffix for histogram observation count.

### 3.3 Counter-wiring sites

Each emit site adds one line next to the existing log call,
calling a typed handle declared at package init. Examples:

```go
// internal/handler/handler.go top-level package vars
var (
    requestsTotal = metrics.NewCounter("acu_requests_total",
        "Per-request outcomes",
        "outcome", "host")
    requestDuration = metrics.NewHistogram("acu_request_duration_seconds",
        "Per-request wallclock",
        []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30, 60},
        "outcome", "host")
    responseBytes = metrics.NewHistogram("acu_response_bytes",
        "Response body bytes",
        []float64{1024, 4096, 65536, 262144, 1048576, 10485760, 104857600, 1073741824},
        "outcome", "host")
)

// In logRequest:
h.logRequest(r, host, path, outcome, status, bytes, false, 0, start)
requestsTotal.Inc(outcome, host)
requestDuration.Observe(time.Since(start).Seconds(), outcome, host)
responseBytes.Observe(float64(bytes), outcome, host)
```

```go
// internal/freshness — adoption_success / adoption_run_failed
adoptionTotal.Inc("success", host)
adoptionDuration.Observe(duration.Seconds(), "success", host)
```

```go
// internal/gc/gc.go — gc_run_complete
g.cfg.Logger.Info("gc_run_complete", ...)
gcRunsTotal.Inc(phase)
gcBlobsReaped.Add(float64(tick.blobsReaped))
gcBytesReclaimed.Add(float64(tick.bytesReclaimed))
gcOrphanCandidatesReaped.Add(float64(tick.orphanCandidatesReaped))
gcDisplacedReaped.Add(float64(tick.displacedReaped))
gcLastRunUnixtime.Set(float64(time.Now().Unix()), phase)
gcRunDuration.Observe(duration.Seconds(), phase)
```

The convention is: one metric line per log emit, placed
immediately after the log call. Code-review rule (added to a
project-level guide): a new `Logger.Info("foo_xxx", ...)` call
without a corresponding `acuFooXxx.Inc(...)` is
a review-failing omission unless the event is genuinely
unmeasurable (e.g. one-time startup banners).

#### 3.3.1 Sites that need explicit instrumentation (not log-mirroring)

Most events have a 1:1 log emit + metric pair. **Two emit sites
need fresh instrumentation** because the source package does not
emit the events Phase 5 wants to count:

1. **Fetch outcomes (`internal/fetch`).** The fetch package today
   emits a single log line — `fetch retry` — when a transient
   failure provokes a retry (`fetch.go:352`). It does **not** log
   the terminal outcome of a fetch (success, status error,
   timeout, redirect blocked, etc.). Phase 5 adds explicit
   instrumentation around `Fetch()` and `Conditional()`: on
   terminal return, the classifier in §3.4.2 maps the returned
   error (or `nil`) through `errors.As(*StatusError)` /
   `errors.Is(...)` to a single outcome label and calls
   `fetchTotal.Inc(outcome, host)` +
   `fetchDuration.Observe(...)`. The classifier is a **total
   function** — every fetch return must yield exactly one outcome.
   §3.4.2 enumerates every fetch sentinel (12+ variants across
   `fetch.go` and `conditional.go`); the classifier covers all of
   them plus the `error` fallback for any future error not yet
   anticipated. This is the only Phase 5 surface that adds
   non-cosmetic logic to a non-handler package.

2. **Adoption duration.** `adoption_success` / `adoption_run_failed`
   logs do not currently carry a `duration_ms` field; the freshness
   loop measures and logs only the start/end markers. Phase 5
   captures `time.Since(start)` at the same emit site as the log
   line and feeds it to `adoptionDuration.Observe(...)`. No new
   log fields, just a metric observation alongside.

Both changes are localized — three call sites total — and do not
alter any existing return shape, retry policy, or transaction
ordering.

### 3.4 Metric inventory

The full list to be enumerated in SPEC5.md §10. Preliminary
sketch of the surface area, by source:

Metrics are tagged `{host}` (the upstream `canonical_host`) when
the host is known at the emit site. The `{host}` label set is
bounded by **two** mechanisms in concert: (a)
`upstream.allowed_host_regex` (SPEC §6.6) caps which hosts can
ever reach the fetch layer; (b) the metrics-package per-metric
series cap (§3.2 — default 1024) caps how many distinct host
values can ever materialize as series. (a) alone is insufficient
for the default broad regex (which admits subdomain wildcards
under `ubuntu.com`, `debian.org`, etc.); (b) is the back-stop.

**Stable label sets.** Prometheus expects every series of a given
metric to carry the same label keys. Outcomes that fire before
host resolution (`method_not_allowed`, `bad_request`) emit with
`host=""` (empty string), **not** with the `{host}` label
omitted. The label set is invariant for the metric; only label
values vary. Operators querying `acu_requests_total` see
`host=""` series alongside `host="archive.ubuntu.com"` series and
can filter / aggregate accordingly.

#### 3.4.1 Request path (handler.go)

- `acu_requests_total{outcome,host}` — counter. Outcome ∈ the
  exhaustive set below (every `outcome` string the handler passes
  to `logRequest` corresponds to one label value):

  - **Pre-host outcomes** (host=""): `method_not_allowed`,
    `bad_request`.
  - **Hit-path outcomes** (host populated): `hit`, `hit_stale`,
    `hit_coalesced`.
  - **Miss-path success** (host populated): `miss` (note: the
    handler logs `hit_coalesced` on the singleflight follower
    branch and `miss` on the leader; both share the same
    request-path counter pair).
  - **Pre-fetch refusals**: `forbidden` (host may be empty if
    rejected before parse, populated if rejected by allowlist
    after parse).
  - **Upstream-driven failures**: `upstream_status` (4xx
    passthrough), `bad_gateway` (5xx-after-retries, redirect
    blocked, invalid URL, default-arm).
  - **Local-fault failures**: `cache_write_failed` (disk full /
    I/O error), `client_canceled` (client disconnected mid-fetch).
  - **SPEC2 §6.2 .deb validation failures**:
    `package_hash_mismatch`, `package_hash_conflict`.
  - **SPEC2 §6.2 metadata-recovery failures**:
    `snapshot_member_refetch_mismatch`, `snapshot_recovery_failed`,
    `snapshot_recovery_upstream_status`,
    `snapshot_recovery_upstream_unreachable`,
    `snapshot_recovery_cache_write_failed`,
    `snapshot_recovery_target_denied`.
  - **SPEC3 §6.1 strict-mode**: `unvouched_deb_refused`,
    `unvouched_deb_passthrough_no_coverage`.

  This list is **exhaustive** for Phase 5 — counter wiring tests
  in §6.3 verify each outcome increments. New `logRequest`
  outcomes added later must update both this list and the test
  table; SPEC5.md §10 will codify this as part of the §3.3
  code-review rule.

- `acu_request_duration_seconds{outcome,host}` — histogram.
  Buckets: 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30, 60.
- `acu_response_bytes{outcome,host}` — histogram.
  Buckets: 1024, 4096, 65536, 262144, 1048576, 10485760, 104857600,
  1073741824 (1KiB → 1GiB).
- `acu_inflight_requests` — gauge (handler.activeWG count). No
  host label (gauge is process-wide, not partitionable cleanly).

#### 3.4.2 Fetch path (internal/fetch)

- `acu_fetch_total{outcome,host}` — counter. The classifier
  below is a **total function** mapping every fetch terminal
  return to exactly one outcome label. Each row corresponds to
  exactly one fetch sentinel (or status-code class), so an
  operator inspecting the metric sees a complete picture of
  upstream behavior. Outcome label values:

  | Source | Outcome label |
  |---|---|
  | `Fetch` returned `nil` | `success` |
  | `Conditional` returned `nil` Status 200 | `cond_changed` |
  | `Conditional` returned `nil` Status 304 | `cond_unchanged` |
  | `*StatusError` with code 4xx (matches `ErrUpstreamStatus`) | `4xx` |
  | `*StatusError` with code 5xx (matches `ErrUpstreamServerError`) | `5xx` |
  | `ErrUpstreamUnavailable` (retries exhausted on transient) | `unavailable` |
  | `ErrRedirectBlocked` (3xx CheckRedirect rejection) | `redirect_blocked` |
  | `ErrHostNotAllowed` | `host_not_allowed` |
  | `ErrTargetDenied` (post-DNS deny CIDR) | `target_denied` |
  | `ErrHostUnreachable` (cooldown-probe fast-fail; SPEC §1) | `host_unreachable` |
  | `ErrInvalidURL` | `invalid_url` |
  | `ErrSizeMismatch` | `size_mismatch` |
  | `ErrInvalidContentRange` | `invalid_content_range` |
  | `ErrTotalSizeMismatch` | `total_size_mismatch` |
  | `ErrCacheWriteFailed` | `cache_write_failed` |
  | `ErrConditionalBodyTooLarge` (Conditional only) | `body_too_large` |
  | `context.DeadlineExceeded` | `timeout` |
  | `context.Canceled` | `canceled` |
  | net dial error (`*net.OpError`, EAI_NONAME, etc.) | `dns_failed` / `connect_failed` |
  | any other | `error` (catch-all) |

  The classifier is implemented as a small, branchful helper in
  `internal/fetch` (e.g. `ClassifyOutcome(err error) string`) so
  the handler and the freshness checker share one source of
  truth. Phase 5 instrumentation calls this helper at every fetch
  return site (§3.3.1).

- `acu_fetch_duration_seconds{outcome,host}` — histogram.
  Same outcome label set as `acu_fetch_total`.
- `acu_fetch_retries_total{host}` — counter, one per retry attempt.
- `acu_active_hosts` — gauge from `hostsem.HostCount()`. No host
  label (the metric *is* the host count).
- `acu_per_host_inflight{host}` — gauge per host from
  `hostsem.Snapshot()` (new method — see §3.9), the per-host
  slot occupancy. Useful for diagnosing one slow upstream
  blocking others. Refreshed by the §3.7 refresher goroutine
  rather than per-Acquire/Release to avoid hot-path overhead.
- `acu_per_host_capacity{host}` — gauge per host from
  `hostsem.Snapshot()`. The configured slot capacity
  (`upstream.max_concurrent_per_host`) per host, exposed for
  saturation alerting (`acu_per_host_inflight / acu_per_host_capacity`).

#### 3.4.3 Freshness / adoption (internal/freshness)

- `acu_freshness_check_total{result,host}` — counter, result ∈
  {`not_modified`, `unchanged`, `changed`, `failed`}.
- `acu_adoption_total{outcome,host}` — counter, outcome ∈
  {`success`, `parse_failed`, `gpg_failed`, `member_mismatch`,
  `unpinned_suite`, `run_failed`}. Each adoption attempt emits
  exactly one outcome — the total of `acu_adoption_total` across
  all outcome values equals the number of adoption attempts. (See
  the next item for `form_drift`, which is a side-event not an
  outcome.)
- `acu_adoption_form_drift_total{prior_form,new_form,host}` —
  counter (separate metric). `adoption_form_drift` is a Warn that
  fires *during a successful adoption* when the suite's signature
  form has switched between the prior current snapshot and the
  one being adopted (inline → detached or vice versa; see
  `internal/freshness/adoption.go:756`). It is **not** an
  alternative outcome to `success`; both fire on the same code
  path. Counting it under `acu_adoption_total{outcome=form_drift}`
  would double-count successful adoptions, so it lives on its own
  counter with `prior_form` and `new_form` labels (each ∈
  {`inline`, `detached`, `unknown`}). Operators monitor for
  unexpected form changes by alerting on rate of this counter.
- `acu_adoption_duration_seconds{outcome,host}` — histogram.
  Outcome label values match `acu_adoption_total` (no `form_drift`
  here either).
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

Metric names mirror the source `gc_run_complete` log field names
(SPEC4 §10.2: `orphan_candidates_reaped`, `displaced_reaped`,
etc.) so an operator grepping logs and Prometheus together
recognizes the same identifier. The full set of `gc_run_complete`
log fields (gc.go:163-174 / gc.go:202-213) is mirrored — every
numeric field becomes a counter or gauge below.

- `acu_gc_runs_total{phase}` — counter, phase ∈ {`startup`,
  `periodic`}.
- `acu_gc_blobs_reaped_total` — counter.
- `acu_gc_bytes_reclaimed_total` — counter.
- `acu_gc_orphan_candidates_reaped_total` — counter.
- `acu_gc_displaced_reaped_total` — counter.
- `acu_gc_pool_orphans_repaired_total` — counter (mirrors log
  field `pool_orphans_repaired`; populated only on the `startup`
  phase — periodic ticks emit 0 for this field per gc.go:208).
- `acu_gc_pool_orphan_bytes_repaired_total` — counter (mirrors
  log field `pool_orphan_bytes_repaired`; same startup-only
  contribution as above).
- `acu_gc_pool_unlink_errors_total` — counter.
- `acu_gc_deadline_reached_total{phase}` — counter, incremented
  by 1 each tick whose `tick.deadlineReached` is true (mirrors
  log field `deadline_reached`). Operators alert on rate to
  detect a GC backlog where ticks are exiting early due to
  `gc.max_tick_duration`.
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
  startup. `version` is `main.Version` (Makefile-injected via
  `-ldflags`); `go_version` and `vcs_revision` come from
  `runtime/debug.ReadBuildInfo()`. Read once in
  `cmd/apt-cacher-ultra/main.go` (which is the only package that
  can name `main.Version`) and passed into the admin / metrics
  setup as a `BuildInfo` value type — see §1.2 build info source
  for the architectural constraint.
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
[table of suite_freshness, sourced from cache.ListSuitesWithAdoption() — see §3.11]
host        suite_path             last_check  last_success  current_snapshot  inrelease_change_seen_at
ubuntu...   ubuntu/dists/jammy     14:30 UTC   14:30 UTC     adopted_at:13:50  -
archive...  debian/dists/bookworm  14:31 UTC   12:15 UTC     adopted_at:08:00  10:42 UTC (lagging 4h12m)

== GC ==
[sourced from gc.GC.LastRunSummary() — see §3.11]
last_run:        2026-05-07 14:00 UTC (10s ago, periodic)
blobs_reaped:    72
bytes_reclaimed: 1.2 GiB
deadline_reached: false
zero_refcount_backlog: 3 blobs awaiting grace
displaced snapshots retained per suite: 3

== Hot packages ==
[list of hot package set, top 20 by request count, with last_requested_at]

== Recent adoptions ==
[table of last N adoptions across all suites — successes and
failures, sourced from an in-memory ring buffer (see §3.10);
the ring is empty after a process restart]

== Active hosts ==
host                 inflight   slot_capacity
archive.ubuntu.com   2          8

[sourced from hostsem.Snapshot() — see §3.9]
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
    "last_run_deadline_reached": false,
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
     "completed_unixtime": 1746668400,
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

`suites[].current_snapshot_adopted_at_unixtime` is the
`suite_snapshot.adopted_at` of the row whose `snapshot_id` equals
`current_snapshot_id`. SPEC §6.1 stores `adopted_at` as a unix
timestamp in `suite_snapshot`; the existing
`cache.ListSuites(ctx)` helper returns only `suite_freshness`
columns, so Phase 5 adds a new query helper —
`cache.ListSuitesWithAdoption(ctx)` — that returns enriched rows
including `adopted_at`. See §3.11. The field is `null` for suites
with no current snapshot (`current_snapshot_id IS NULL`).

The `gc.*` block is sourced from
`gc.GC.LastRunSummary()`-style accessor (also new in Phase 5 —
§3.11) that captures the most recent `gc_run_complete` payload in
memory. `gc.last_run_unixtime` is `null` when no GC run has
completed since process start.

`recent_adoptions[].completed_unixtime` is the timestamp the
adoption *finished* (success or failure), not `adopted_at`. For a
successful adoption this is approximately equal to the DB's
`adopted_at`; for a failure there is no DB record (see §3.10).
`recent_adoptions` is sourced from the §3.10 in-memory ring and
returns an empty array after a process restart.

### 3.6 Healthz design

Three stateless checks, typical wallclock <5ms with a hard 1.5s
ceiling (the DB-ping timeout dominates the worst-case path):

1. **Cache directory writable**: `os.CreateTemp(cache_dir,
   ".acu-healthz-*")`, write 1 byte, close, `os.Remove`.
   `CreateTemp` returns a unique-suffix filename so concurrent
   probes do not race on a fixed filename. Failure to create or
   write is the failure signal. Typical wallclock <2ms on a
   healthy local filesystem.
2. **DB pingable**: `db.PingContext(ctx)` with a 1s deadline.
   This is the single check whose worst case can dominate the
   request — a hung sqlite writer pushes it to ~1s. Typical
   wallclock <1ms.
3. **Process not in graceful shutdown**: read a flag set by the
   shutdown handler before `Server.Shutdown` is called. Microsecond
   cost (one atomic load).

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
- `acu_per_host_inflight{host}`, `acu_per_host_capacity{host}` —
  populated from `hostsem.Snapshot()` (§3.9). Before populating,
  the per-host gauges call `.Reset()` so a host that no longer
  has in-flight requests stops reporting stale values.

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

### 3.9 hostsem.Snapshot — new API

`internal/hostsem.Sem` today exposes only `HostCount() int`. The
per-host inflight gauge (§3.4.2) and the status-page active-hosts
table (§3.5.2) need per-host occupancy and capacity. Phase 5 adds
one method:

```go
// Snapshot returns a point-in-time copy of every active host's
// (inflight, capacity) tuple. inflight is the count of currently
// held tokens (waiting acquirers do not count); capacity is the
// configured per-host slot count (the same value passed to New()).
//
// AIDEV-NOTE: callers should treat the returned map as read-only
// and not modify it. The map allocation is the cost of one Lock /
// for-range / Unlock; expected to be called by the §3.7 refresher
// at 30s cadence and by the status-page handler at request time.
func (s *Sem) Snapshot() map[string]HostStat

type HostStat struct {
    Inflight int
    Capacity int
}
```

Implementation: hold the existing `sem.mu` Lock, walk
`sem.slots`, and for each `*hostSlot` record `Inflight = limit -
len(slot.ch)` and `Capacity = limit`. (The buffered channel's
length is the count of *available* tokens; in-flight is the
complement.)

The new method does not change any existing semaphore behavior —
it is purely a read view onto the existing fields under the
existing lock.

### 3.10 In-memory adoption ring (`internal/observability`)

The status page's `recent_adoptions` section (§3.5.2 / §3.5.3)
shows a tail of recent adoption attempts including outcome and
duration. The DB has `suite_snapshot.adopted_at` for *successful*
adoptions only — failed adoption attempts (parse_failed,
gpg_failed, member_mismatch, etc.) leave no DB record. Without
schema changes (§1 commits to none), the only place to keep
failure history is process memory.

Phase 5 introduces a small `internal/observability` package with
a thread-safe ring buffer:

```go
package observability

type AdoptionEvent struct {
    Host             string
    SuitePath        string
    Outcome          string  // success, parse_failed, gpg_failed, ...
    CompletedUnixSec int64
    DurationSeconds  float64
}

type Ring struct {
    mu     sync.Mutex
    buf    []AdoptionEvent
    head   int
    full   bool
    cap    int
}

func NewRing(capacity int) *Ring
func (r *Ring) Record(e AdoptionEvent)        // O(1) under lock
func (r *Ring) Snapshot() []AdoptionEvent     // newest-first copy
```

Capacity: 50 events (locked default; not tunable in Phase 5 since
the value affects neither correctness nor operator workflow).

Producers: every adoption-completion site in `internal/freshness`
calls `Ring.Record(...)` alongside the `adoption_*` log emit and
the `adoptionTotal.Inc(...)` counter increment.

Consumer: the status-page handler calls `Ring.Snapshot()` at
request time. The Snapshot copy is owned by the caller and not
mutated by the ring.

The ring is process-local: empty after every restart. The HTML
page renders an explanatory "(empty since last process start)"
note when the snapshot is empty *and* the process has been up
for less than 5 minutes — a usability cue for operators who
restarted recently.

This is the only piece of mutable in-memory state Phase 5 adds
beyond the metrics registry and the htpasswd cache. It does not
participate in graceful-shutdown ordering: dropping the ring on
shutdown is correct (the data is non-authoritative).

### 3.11 Data-source helpers (cache + GC)

Two new accessors are needed so the §3.5 status page can render
data that today is either un-queryable or only present in logs.

#### 3.11.1 `cache.ListSuitesWithAdoption(ctx)`

Today, `cache.ListSuites(ctx)` returns `[]SuiteFreshness` from a
plain `SELECT * FROM suite_freshness` (queries.go:226). The row
shape includes `current_snapshot_id` but not the snapshot's
`adopted_at` — that column lives in `suite_snapshot`, keyed by
`snapshot_id`. The status page needs both, and a per-row
`GetSuiteSnapshot` lookup is N×1 round trips that the periodic
scheduler doesn't do because it doesn't need adoption time.

Phase 5 adds a new query helper:

```go
// SuiteWithAdoption embeds SuiteFreshness and adds the adopted_at
// of the current_snapshot_id (nil when current_snapshot_id is
// NULL or the join finds no matching suite_snapshot row — both
// indicate "never adopted").
type SuiteWithAdoption struct {
    SuiteFreshness
    CurrentAdoptedAt *int64  // unix seconds; nil when not adopted
}

// ListSuitesWithAdoption returns every suite_freshness row LEFT
// JOIN suite_snapshot ON suite_freshness.current_snapshot_id =
// suite_snapshot.snapshot_id. Used by the §3.5 status page.
// Existing ListSuites() (used by the freshness scheduler) is
// untouched — the scheduler does not need adopted_at.
func (c *Cache) ListSuitesWithAdoption(ctx context.Context) ([]SuiteWithAdoption, error)
```

A single LEFT JOIN query keeps it O(1) round trips. The status
page handler may also tolerate a small TTL cache (e.g. 5s) on
this result if profiling shows the query is hot under load —
deferred to implementation time.

#### 3.11.2 `gc.GC.LastRunSummary()`

Today, `gc.GC` retains no in-memory state across runs — every
`gc_run_complete` is logged and forgotten (gc.go:202). The status
page needs the most recent run's summary fields.

Phase 5 extends `*gc.GC` with a small struct field that captures
the last completed run, populated atomically (under a mutex) at
the same emit site as the log line:

```go
type LastRunSummary struct {
    Phase               string  // "startup" | "periodic"
    AtUnixTime          int64
    DurationSeconds     float64
    BlobsReaped         int
    BytesReclaimed      int64
    OrphanCandidatesReaped int
    DisplacedReaped        int
    PoolOrphansRepaired    int
    PoolOrphanBytesRepaired int64
    PoolUnlinkErrors    int
    DeadlineReached     bool
}

// LastRunSummary returns a copy of the most recently completed
// GC run's summary, or (zero, false) when no run has completed
// since process start.
func (g *GC) LastRunSummary() (LastRunSummary, bool)
```

The summary is set under `gc.mu` after the log line emits and
before the next tick begins. The status-page handler calls
`LastRunSummary()` at request time and treats `(_, false)` as
"GC has not run yet" — render an empty cell rather than a stale
number.

This extends `*GC` non-additively: existing callers (cmd/main.go's
`StartupPass()` / `Run()` invocations) are unaffected. SPEC4
§10.2 GC contracts are unchanged — `LastRunSummary()` is a pure
accessor, not a behavioral change.

---

## 4. Schema migration

**None.** Phase 5 is observation-only; no DB changes, no migration.

---

## 5. Configuration block

```toml
[admin]
enabled            = true              # master switch; default true
listen             = "127.0.0.1:6789"  # bind address; loopback by default
htpasswd_file      = ""                # bcrypt htpasswd; empty = no auth
gauge_refresh      = "30s"             # expensive-gauge refresh cadence
read_timeout       = "5s"              # HTTP ReadHeaderTimeout
idle_timeout       = "30s"             # HTTP IdleTimeout
metric_series_cap  = 1024              # per-metric series cap (§3.2)
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
- `admin.metric_series_cap >= 1` (0 would disable all increments
  on labeled metrics — that is a footgun, not a feature; use the
  unlabeled metric form if cap-bypass is genuinely wanted).
  Recommended ≤ 100000 (per-process Prometheus cardinality
  budget — operators who hit this are encouraged to narrow
  `upstream.allowed_host_regex` instead).

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
- **Series cap** — declare a counter with cap=3, Inc 10 distinct
  label tuples, verify exactly 3 series exist + a one-shot Warn
  fires once. Existing 3 series continue to update; subsequent
  Inc on a 4th tuple is dropped.
- **Render hold-time** — render to a deliberately-slow Writer (a
  pipe with a delayed reader); verify Inc calls during the slow
  Write don't observe extended blocking. Indirect: time the Inc
  hot path under a slow concurrent Render and confirm wallclock
  is bounded by buffer-build cost, not by Write cost.

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
  the corresponding `acu_requests_total{outcome=...,host=...}`
  counter incremented by exactly 1.
- A request that fails before host parsing (`method_not_allowed`)
  produces `acu_requests_total{outcome="method_not_allowed",host=""}`
  — verifying the empty-host stable-label-set posture from §3.4.
- Run a synthetic GC pass with known reaped count, then scrape
  `/metrics` and verify `acu_gc_blobs_reaped_total`,
  `acu_gc_orphan_candidates_reaped_total`, and
  `acu_gc_displaced_reaped_total` incremented.
- Trigger a freshness check that returns 304, scrape, verify
  `acu_freshness_check_total{result=not_modified,host=...}`
  incremented.
- Issue a fetch that returns 200, then a fetch that returns 502,
  then a fetch that times out — verify
  `acu_fetch_total{outcome="2xx",host=...}`,
  `acu_fetch_total{outcome="5xx",host=...}`,
  `acu_fetch_total{outcome="timeout",host=...}` each incremented
  exactly once (validates the §3.3.1 explicit fetch
  instrumentation).

### 6.4 hostsem.Snapshot tests

- `Snapshot()` on an empty Sem returns an empty map.
- After two Acquires on host A and one on host B, `Snapshot()`
  returns `{"A": {Inflight:2, Capacity:N}, "B": {Inflight:1, Capacity:N}}`.
- A waiter blocked on Acquire (capacity exhausted) does **not**
  count toward Inflight.
- Concurrent Snapshot + Acquire/Release under `-race` — no
  data race, returned values are internally consistent.

### 6.5 Adoption ring tests

- `NewRing(50)`, record 50 events, `Snapshot()` returns all 50
  newest-first.
- Record 51 events, `Snapshot()` returns 50 (oldest dropped).
- Concurrent Record + Snapshot under `-race` — no data race; the
  Snapshot copy is independent of subsequent Record calls.
- Empty ring, `Snapshot()` returns `[]AdoptionEvent{}` (not nil).

### 6.6 Fetch outcome classifier tests (internal/fetch)

For each entry in the §3.4.2 classifier table, construct a
returned-error or `nil` value and verify
`ClassifyOutcome(err)` returns the expected outcome label. Each
sentinel + status code pair is one test case (~20 cases). The
test asserts the classifier is **total**: the catch-all `error`
arm fires on a bare `errors.New("synthetic")` and on a `nil`
ContentResult that doesn't match any specific success path.

### 6.7 Data-source helper tests (cache + GC)

- `cache.ListSuitesWithAdoption(ctx)` returns adopted_at populated
  for suites whose current_snapshot_id has a matching
  suite_snapshot row, and `nil` for suites with NULL
  current_snapshot_id.
- A suite_freshness row whose current_snapshot_id points at a
  snapshot_id that does not exist (data-corruption case) returns
  `nil` adopted_at without erroring.
- `gc.GC.LastRunSummary()` returns `(_, false)` before any run
  completes, `(_, true)` after `StartupPass()` finishes, and is
  updated atomically after each periodic tick.
- Concurrent reader of `LastRunSummary()` while a tick is updating
  it — no torn read under `-race`.

No new chaos tests. Phase 5 is observability of existing chaos,
not new chaos.

---

## 7. Definition of done

1. All Phase 1/2/3/4 tests still pass.
2. `internal/metrics` package implemented with unit tests under
   `-race`, including the per-metric series cap (§3.2) and the
   buffered-render hold-time bound (§3.2 / §6.1).
3. `internal/hostsem.Sem.Snapshot()` added per §3.9, with unit
   tests under `-race`.
4. `internal/observability` ring buffer implemented per §3.10,
   with unit tests under `-race`.
5. `cache.ListSuitesWithAdoption(ctx)` and `gc.GC.LastRunSummary()`
   data-source helpers added per §3.11, with unit tests under
   `-race`.
6. `internal/fetch.ClassifyOutcome(err)` total classifier added
   per §3.4.2, with unit tests for every sentinel.
7. Admin listener bound, all three endpoints reachable on default
   loopback config.
8. Counter wiring at all emit sites, verified by §6.3 tests
   against the **exhaustive** §3.4.1 outcome list (every
   `logRequest` outcome string appears in the test table) plus
   the §3.3.1 explicit fetch + adoption duration instrumentation.
   `acu_adoption_form_drift_total` is a separate counter from
   `acu_adoption_total` (§3.4.3). New GC counters
   `acu_gc_pool_orphan_bytes_repaired_total` and
   `acu_gc_deadline_reached_total{phase}` are wired (§3.4.5).
9. htpasswd auth implemented per §3.8; auth tests pass per §6.2.1.
10. Status page renders correctly (HTML and JSON) for an empty
    cache, a populated cache, and a cache with a lagging-upstream
    suite. `acu_build_info` populated from `main.Version` passed
    in via `BuildInfo` value type (§1.2).
11. `/healthz` uses `os.CreateTemp` (not a fixed filename), flips
    to 503 within one check cycle when cache_dir becomes
    unwritable, and recovers within one check cycle when restored.
12. SPEC5.md as-built, mirroring SPEC4.md structure.
13. One-week production deployment with default `admin.*` showing
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
cardinality decision adds `{host}` labels but explicitly excludes
`{suite_path}` and `{client_addr}`. The default
`upstream.allowed_host_regex` admits subdomain wildcards under
`ubuntu.com` / `debian.org` / etc., so a hostile or noisy client
can in principle generate unbounded distinct host strings. Phase
5 mitigates with a per-metric series cap (§3.2 — default 1024,
configurable via `admin.metric_series_cap`); overflow drops the
increment with a one-shot Warn. The cap is the back-stop, not a
substitute for a tight regex: operators in multi-tenant or
hostile-LAN environments should narrow the regex to literal hosts
before exposing the admin port. An operator who later adds an
unbounded label "just for one metric" can still consume the cap
budget and silently drop legitimate increments — the §3.4 metric
inventory should be treated as the exhaustive label set, and new
metrics added later should pass cardinality review.

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
