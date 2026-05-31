# apt-cacher-ultra — Phase 5 Specification

This document specifies the contract for Phase 5: the operator-
visibility loop (Prometheus `/metrics`, HTML/JSON status page,
`/healthz` liveness probe). It is a delta over [SPEC.md](SPEC.md)
(Phase 1), [SPEC2.md](SPEC2.md) (Phase 2), [SPEC3.md](SPEC3.md)
(Phase 3), and [SPEC4.md](SPEC4.md) (Phase 4). Sections that
carry forward unchanged say so explicitly and point at the prior
spec; sections that change describe only the delta. The companion
document [PHASE-5-SCOPING.md](PHASE-5-SCOPING.md) records the
design rationale and the eight-question scoping pass — plus three
review-finding rounds — that produced this spec.

Phase 5 is **semantically** additive over Phase 4: no existing
request path, adoption path, freshness path, GC path, or wire
contract changes. A third HTTP listener (default
`127.0.0.1:6789`) bound alongside the proxy and TLS listeners
exposes three read-only endpoints. Counter / histogram / gauge
increments are added *alongside* (not in place of) existing log
emits; the metric inventory is enumerated in §10.4 and is the
contract surface for operator monitoring.

**Startup compatibility note.** Phase 5 is *not* purely
operationally additive: the `[admin]` block defaults to
`enabled = true` with `listen = "127.0.0.1:6789"` (§5.1). On a
host where port 6789 is already bound (extremely unlikely —
the port is assigned to no IANA-registered service and is far
from the well-known range, but conceivable on a host running
some unrelated service on it), the daemon's startup will fail
with a bind error from step 3 of §9.7.1. Operators in that
narrow case can either move their other service or set
`admin.enabled = false` (or `admin.listen = "127.0.0.1:0"` /
some other port). The default-on choice is the operator-
friendly trade-off; see PHASE-5-SCOPING.md §1.2 "Default-on"
for the rationale. Operators upgrading from Phase 4 should
verify the chosen port is free as part of the rollout.

---

## 1. Goals & non-goals

### 1.1 Phase 5 goals

1. **Expose a `/metrics` endpoint** in Prometheus text exposition
   format (`text/plain; version=0.0.4; charset=utf-8`) so an
   operator running Prometheus / VictoriaMetrics / OpenObserve can
   scrape per-process counters, gauges, and histograms covering
   the request path, fetch path, freshness/adoption path, GC, and
   integrity scan. Phases 1–4 emit ~30 distinct log events across
   178 call sites; Phase 5 turns each operationally-meaningful
   event into a counter-or-gauge increment alongside (not instead
   of) the existing log line. The full metric inventory is §10.4.

2. **Expose a `/` status page** in HTML for human eyeballing —
   the "is the cache lagging upstream right now? which suites?
   for how long?" view operators reach for during an incident.
   `inrelease_change_seen_at` (SPEC.md:492) is the canonical
   per-suite "lagging" signal. The same endpoint serves a
   machine-readable JSON form via content negotiation (§9.7.3 /
   §10.5). The status page is the cheapest way to expose state
   that Prometheus would otherwise need a high-cardinality label
   set for (per-suite, per-package).

3. **Expose a `/healthz` endpoint** as a simple liveness/readiness
   probe for systemd, Kubernetes, or any reverse proxy doing
   health checks. SPEC.md:577 explicitly bookmarks a Phase 5
   health endpoint reporting "degraded" when the cache disk is
   full.

4. **Wire counters into the existing event stream.** No new
   semantics, no new chaos, no new race windows. The contract is:
   wherever today there is a `Logger.Info("foo_event", ...)` call
   on a request-path / adoption-path / GC-path / freshness-path
   site, after Phase 5 there is also exactly one
   `acuFooEvent.Inc(...)` call (or `Observe(...)`, or `Set(...)`).
   The handler's per-request log line gains a counter+histogram
   pair keyed on `outcome`. Two emit sites — fetch outcomes and
   adoption duration — require *explicit instrumentation* (not
   log-mirroring) because the source package today does not log
   the terminal event. See §9.7.6 and §10.4.2.

The four jobs share one `[admin]` configuration block (§5.1), one
new HTTP listener (§9.7), and one new log-event family for the
admin listener's own lifecycle (§10.2).

### 1.2 Phase 5 non-goals (deferred)

Carried forward from earlier phases unchanged:

- TLS MITM listener (Phase 6).
- Source-package caching, multi-arch beyond amd64, pdiff (Phase
  6+).
- Streaming-while-fetching as a singleflight optimization.
  Deferred to [FUTURE-REVIEW.md §1](FUTURE-REVIEW.md).
- Per-byte upstream read timeouts. Deferred to
  [FUTURE-REVIEW.md §1](FUTURE-REVIEW.md).
- Per-suite freshness cadence variation. Deferred to
  [FUTURE-REVIEW.md §2](FUTURE-REVIEW.md).
- Operator-triggered manual GC (admin endpoint or SIGUSR1).
  Carried forward from Phase 4 §1.1; could be a Phase 5 add-on
  given the admin listener now exists, but not part of the
  Phase 5 gating contract.

Newly deferred in Phase 5:

- **OpenTelemetry / OTLP exporters.** Prometheus text exposition
  is the baseline. An OTLP push pipeline can be built on top
  later if any deployment runs an OTel collector; the metric
  names and label conventions chosen in §10.4 are designed to
  translate cleanly.
- **Distributed tracing.** No spans, no trace IDs propagated.
  The per-request log line already carries enough fields to
  correlate by `client_addr` + start time; tracing is a Phase 6+
  topic.
- **Pull-based admin actions.** No `/admin/gc/run`,
  `/admin/cache/clear`, `/admin/suites/{path}/refresh` endpoints
  in Phase 5. The admin listener is **read-only**. Phase 6 may
  introduce mutating endpoints behind explicit auth; the
  scoping-stage default of "no mutations" prevents the admin
  port from becoming an attack surface in any deployment that
  mistakenly exposes it.
- **Per-client metrics.** No `{client_addr}` label cardinality.
  The proxy serves a small fleet of trusted apt clients;
  per-client breakdown is not load-bearing for operations.
- **Long-term storage.** The process exposes counters; whatever
  scrapes them is responsible for retention. No internal
  histograms beyond what Prometheus's own histogram type
  provides at scrape time.

---

## 2. Wire contracts

### 2.1 Proxy listener

Unchanged — see SPEC.md §2 / SPEC2 §2 / SPEC3 §2.7. Phase 5 adds
no new response headers and changes no request-path behavior. The
proxy listener (`:3142` / configurable via `cache.listen`) and
the optional TLS listener (`cache.listen_tls`) are entirely
unchanged.

### 2.2 Admin listener (NEW)

A third HTTP listener (default `127.0.0.1:6789`, configurable via
`admin.listen`) serves three read-only endpoints. The admin
listener is **plain HTTP only** — TLS for the admin port is
deferred to Phase 6. Operators who need TLS for the admin
endpoint front it with a reverse proxy.

Wire contracts on the admin listener:

| Method | Path        | Response                                        |
|--------|-------------|-------------------------------------------------|
| GET    | `/metrics` (exact) | 200; `text/plain; version=0.0.4; charset=utf-8` |
| GET    | `/` (exact, NOT subtree — §9.7.1 uses Go 1.22+ `{$}` pattern) | 200; `text/html; charset=utf-8` or `application/json; charset=utf-8` (content negotiation per §9.7.3) |
| GET    | `/healthz` (exact) | 200 `ok\n` or 503 `degraded\n` with `X-Acu-Check-Failed:` header |
| HEAD   | (any of above) | Same status code, empty body, all headers as for GET |
| OPTIONS | (any path) | 204; `Allow: GET, HEAD, OPTIONS` |
| GET    | any other path (e.g. `/unknown`, `/metrics2`) | 404 |
| Any non-GET method | `/metrics`, `/`, `/healthz` | 405 with `Allow: GET, HEAD, OPTIONS` |

No POST, no PUT, no DELETE — the admin listener is read-only.

When `admin.htpasswd_file` is configured, every admin request
must present a valid `Authorization: Basic <user>:<password>`
header. See §9.7.5 for the auth contract.

---

## 3. URL canonicalization (Remap)

Unchanged — see SPEC.md §3.

---

## 4. Storage layout

### 4.1 Disk

Unchanged — see SPEC.md §4.1 / SPEC2 §4.1 / SPEC3 §4.1 / SPEC4
§4.1. The on-disk layout is exactly Phase 4's. Phase 5 introduces
no new directories or files. The `/healthz` writability check
uses `os.CreateTemp(cache_dir, ".acu-healthz-*")` which leaves
no debris (the temp file is removed before the check returns).

### 4.2 Startup cleanup

Unchanged — see SPEC4 §4.2. Phase 5 binds the admin listener
between the existing TLS-listener bind and the `cache.Open` call;
the SPEC4 startup ordering (bind plain → bind TLS → bind admin →
cache.Open → SweepTmp → GC startup pass → Accept goroutines) is
specified in §9.7.1 below.

### 4.3 SQLite schema

Unchanged. **Phase 5 introduces no DB schema changes.** No new
tables, no new columns, no migration. The status page's
`recent_adoptions` view is sourced from a process-local in-memory
ring buffer (§9.7.7), not from a new DB table — failed adoptions
have no DB record, and adding one for status-page visibility was
considered and rejected (DB schema changes are reserved for
correctness work, not observability).

### 4.4 Suite identification

Unchanged — see SPEC.md §4.4.

### 4.5 Classifying metadata vs. blob

Unchanged — see SPEC.md §4.5.

---

## 5. Configuration (TOML)

### 5.1 Example (deltas)

Existing sections (Phase 1 + Phase 2 + Phase 3 + Phase 4 keys)
carry forward unchanged. Phase 5 adds one new top-level block:

```toml
[admin]
# Master switch. When false, the listener is not bound; /metrics,
# /, and /healthz are unreachable. The cache continues to serve
# proxy traffic normally — the operator has implicitly opted out
# of all observability. A startup admin_disabled Warn fires when
# false, parallel to gc_disabled (SPEC4 §10.2). Default true: the
# loopback bind is the security boundary, not the enabled flag,
# and a metrics endpoint that is off-by-default makes the Day 1
# operator experience worse for no security gain.
enabled            = true

# Bind address for the admin listener. Default 127.0.0.1:6789 —
# loopback by default, port chosen to avoid colliding with the
# proxy (3142) or any common dev port. Operators who need
# off-host scrapes (e.g. Prometheus on a separate node) widen
# this to 0.0.0.0:6789 AND configure htpasswd_file. An exposure
# of admin.listen to a non-loopback address with htpasswd_file
# empty triggers admin_unauthenticated_non_loopback Warn at
# startup (§5.2 / §10.2). Reuses validateListenAddr().
listen             = "127.0.0.1:6789"

# Optional Apache htpasswd file for HTTP Basic auth on every
# admin request. Empty (default) means "no auth — operator
# relies on bind-address as the trust boundary." Non-empty path
# must exist, be readable, and parse as bcrypt-only htpasswd
# (every line is `user:$2[aby]$...`). Apache MD5 ($apr1$),
# SHA-1 ({SHA}), and crypt(3) are rejected at startup with a
# config error naming the offending line. The file is re-read
# on mtime change (stat-on-each-request, parse-on-change) so
# operators add/remove users without a restart. Recommended file
# mode 0600 owned by the daemon user; the daemon does not enforce
# the mode (operators may have legitimate 0644 setups).
htpasswd_file      = ""

# Period of the in-process refresher goroutine that recomputes
# expensive gauges (acu_blobs_db_count, acu_blobs_db_total_bytes,
# acu_pool_disk_bytes, acu_per_host_inflight, etc.). Scrapes
# read in-memory cells; the cells are refreshed on this cadence.
# A scrape can read a cell up to gauge_refresh seconds stale
# (between two refreshes); operators tolerant of fresher data
# can lower this. Default 30s — slow enough that du-style pool/
# scans don't dominate CPU on multi-GiB caches, fast enough
# that gauge values are never older than 30s on a healthy
# refresher loop. The refresher runs an immediate first refresh
# at startup (before serving the first scrape) so the first
# /metrics response contains populated gauges, not zeros (§9.7.6).
# Must be > 0 and ≤ 1h.
gauge_refresh      = "30s"

# HTTP server timeouts for the admin listener. ReadHeaderTimeout
# bounds how long the server waits for a request line + headers
# (5s is generous for any reasonable scrape client). IdleTimeout
# bounds keep-alive idle wait (30s matches typical
# Prometheus scrape gaps). Both must be > 0; read_timeout ≤ 1m,
# idle_timeout ≤ 10m.
read_timeout       = "5s"
idle_timeout       = "30s"

# Per-metric series cap. When a labeled metric (Counter /
# Histogram / Gauge) sees a new label-value tuple that would
# push its series count past this cap, the Inc / Observe / Set
# call is silently dropped and a one-shot
# metrics_series_cap_reached Warn fires for that metric.
# Existing series continue to update normally. Default 1024 —
# bounded enough that a malicious client cannot blow up
# Prometheus storage even with the broad default
# upstream.allowed_host_regex (subdomain-wildcard regex admits
# unbounded distinct host strings). Operators with hundreds of
# legitimate hosts can raise the cap; operators concerned with
# cardinality should narrow the regex to literal hosts FIRST,
# then optionally raise the cap. Must be ≥ 1.
metric_series_cap  = 1024
```

### 5.2 Config validation (deltas)

Phase 1 + Phase 2 + Phase 3 + Phase 4 validation carries forward.
Phase 5 adds:

- `admin.enabled` is bool, default `true` (pre-populated in
  `defaultConfig` so the zero-value-vs-absent distinction works
  the same way `gc.enabled` does — see SPEC4 §5.2).
- `admin.listen` parses as `host:port` via `validateListenAddr()`,
  the same helper that validates `cache.listen` and
  `cache.listen_tls` (port 1–65535, host either an IP or empty).
- `admin.htpasswd_file` if non-empty: file must exist, be a
  regular file, be readable. Each non-empty, non-comment line
  must split on the first `:` into `(username, hash)` with the
  hash starting with `$2a$`, `$2b$`, or `$2y$`. Any other prefix
  (`$apr1$`, `{SHA}`, bare crypt(3) DES) is a config error
  naming the file and the offending line number. Empty
  username, embedded whitespace, or embedded colon within the
  username also fail. Parse failures at startup are config
  errors (process exits non-zero with the offending file/line
  in the error message).
- `admin.gauge_refresh` parses as duration, > 0 and ≤ 1h.
- `admin.read_timeout` parses as duration, > 0 and ≤ 1m.
- `admin.idle_timeout` parses as duration, > 0 and ≤ 10m.
- `admin.metric_series_cap` parses as int, ≥ 1. (0 would disable
  every increment on labeled metrics — that is a footgun, not a
  feature; use the unlabeled metric form if cap-bypass is
  genuinely needed.)

Cross-key startup warnings:

- **`admin_disabled`** Warn when `admin.enabled = false`,
  parallel to `gc_disabled` (SPEC4 §10.2).
- **`admin_unauthenticated_non_loopback`** Warn when
  `admin.listen` resolves to anything other than `127.0.0.1`,
  `::1`, or `localhost` AND `admin.htpasswd_file` is empty —
  the operator has widened the trust boundary without
  providing auth. Parallel to
  `refuse_unvouched_debs_inert` (SPEC3 §10.2).
- **`admin_authenticated`** Info when `admin.htpasswd_file` is
  non-empty and parses successfully, with the user count.

---

## 6. Request handling

Unchanged — see SPEC.md §6 / SPEC2 §6 / SPEC3 §6 / SPEC4 §6. The
request handler's behavior on the proxy listener is untouched.

The handler's per-request `logRequest(...)` call site (SPEC §10.1
per-request line) gains three companion calls — one counter
increment, one duration histogram observation, one response-bytes
histogram observation — placed immediately after the log emit.
These additions do not alter request handling; they are pure
side-effects observed by the metrics package. See §10.4.1 for the
metric definitions and §10.4 prelude for the wiring rule.

---

## 7. Freshness and adoption

Unchanged — see SPEC.md §7 / SPEC2 §7 / SPEC3 §7 / SPEC4 §7. The
freshness loop and adoption flow are untouched.

The freshness package gains:

1. **One counter+histogram pair per existing log emit.** Each
   `freshness_check`, `adoption_success`, `adoption_run_failed`,
   `adoption_gpg_failed`, `adoption_member_mismatch`,
   `adoption_parse_failed`, `adoption_unpinned_suite` emit gains a
   neighboring `acuFreshnessCheckTotal.Inc(...)` /
   `acuAdoptionTotal.Inc(...)` call.

2. **Adoption duration measurement.** The freshness package's
   adoption goroutine captures `time.Since(start)` at the same
   site as the success / failure log emit and feeds it to
   `acuAdoptionDuration.Observe(...)`. No new log fields are
   added; the histogram observation is the only new
   instrumentation. See §10.4.3.

3. **Form drift counter (separate from outcome).**
   `adoption_form_drift` Warn (adoption.go:756) fires *during a
   successful adoption* when the suite's signature form has
   switched (inline ↔ detached). It is **not** an alternative
   outcome to `success`; both fire on the same code path.
   `acu_adoption_form_drift_total{prior_form,new_form,host}`
   is a separate counter from `acu_adoption_total{outcome,host}`,
   so the sum of `acu_adoption_total` across outcomes equals the
   adoption-attempt count without double-counting form drifts.
   See §10.4.3.

4. **Adoption ring-buffer record.** Every adoption-completion site
   in the freshness package calls `Ring.Record(...)` on the
   process-local adoption-event ring (§9.7.7) alongside the
   `adoption_*` log emit and the `acu_adoption_total.Inc(...)`
   counter increment. The ring populates the status page's
   `recent_adoptions` section.

---

## 8. Stale-and-Valid-Until

Unchanged — see SPEC.md §8.

---

## 9. Concurrency & deadlines

### 9.1 Per-request

Unchanged — see SPEC.md §9.1.

### 9.2 Singleflight

Unchanged — see SPEC.md §9.2.

### 9.3 Per-host concurrency on upstream

Unchanged — see SPEC.md §9.3 / SPEC2 §9.3.1 / SPEC3 §9.3 / SPEC4
§9.3. Phase 5 adds a new read-only accessor on `*hostsem.Sem`:

```go
// Snapshot returns a point-in-time copy of every active host's
// (inflight, capacity) tuple. inflight is the count of currently
// held tokens (waiting acquirers do not count); capacity is the
// configured per-host slot count (the same value passed to New).
func (s *Sem) Snapshot() map[string]HostStat

type HostStat struct {
    Inflight int
    Capacity int
}
```

Implementation: the existing `sem.mu` Lock is held while walking
`sem.slots`; for each `*hostSlot`, `Inflight = len(slot.ch)` and
`Capacity = cap(slot.ch)`. **The buffered channel's length is
the count of currently-held tokens** — `Acquire` *sends* into
the channel when a token is acquired (sem.go:77) and *receives*
on Release (sem.go:96), so the channel fills as concurrency
rises. `cap(slot.ch)` is the per-host limit configured via
`upstream.max_concurrent_per_host`; `s.limit` is identical and
equally usable. The new method does not change any existing
semaphore behavior — it is purely a read view onto the existing
fields under the existing lock.

`Snapshot()` is called by the §9.7.6 refresher goroutine at the
configured `admin.gauge_refresh` cadence (default 30s) and by the
status-page handler at request time. The map allocation cost is
one Lock / for-range / Unlock under the existing per-Sem mutex.

### 9.4 SQLite concurrency

Unchanged — see SPEC4 §9.4.

### 9.5 Graceful shutdown

The SPEC4 §9.5 graceful-shutdown sequence carries forward and is
extended to cover the admin listener. The full sequence becomes:

1. SIGINT/SIGTERM → cancel `lifecycleCtx`.
2. `admin.Server.Shutdown(ctx)` — admin listener stops accepting
   new connections; any in-flight scrape gets a short window to
   complete (default 5s).
3. `cache.Server.Shutdown(ctx)` (proxy) and (if enabled)
   `cache.ServerTLS.Shutdown(ctx)` — Phase 1 + Phase 4 behavior.
4. `handler.Close()` — drains in-flight singleflight fetches.
5. `gc.Cancel()` — cancels the GC goroutine; any in-flight
   batch completes; no new batch starts.
6. `freshness.Close()` — drains in-flight adoption goroutines.
7. `cache.Close()` — closes the SQLite DB.

The admin listener shuts down **first** because its endpoints
read DB state (status page, refresher gauges) and a partial
shutdown should not surface inconsistent observability data to a
scraper. A scrape mid-shutdown that is past the listener-shutdown
deadline receives `Connection: close` on its half-served
response.

The refresher goroutine (§9.7.6) is rooted at `lifecycleCtx` and
exits on the SIGINT/SIGTERM. The htpasswd reload mechanism is
stat-on-each-request, so there is no separate reload goroutine to
shut down.

The §9.7.7 in-memory adoption ring is not part of graceful
shutdown — its data is non-authoritative and dropping it on
restart is correct.

### 9.6 Garbage collection

Unchanged — see SPEC4 §9.6.

The `*gc.GC` struct gains one read-only accessor for the status
page (§9.7.4 / §10.5):

```go
type LastRunSummary struct {
    Phase                   string  // "startup" | "periodic"
    AtUnixTime              int64
    DurationSeconds         float64
    BlobsReaped             int
    BytesReclaimed          int64
    OrphanCandidatesReaped  int
    DisplacedReaped         int
    PoolOrphansRepaired     int
    PoolOrphanBytesRepaired int64
    PoolUnlinkErrors        int
    DeadlineReached         bool
}

// LastRunSummary returns a copy of the most recently completed
// GC run's summary, or (zero, false) when no run has completed
// since process start.
func (g *GC) LastRunSummary() (LastRunSummary, bool)
```

The summary is captured under `gc.mu` *after* the
`gc_run_complete` log line is emitted and *before* the next tick
begins. It is a pure accessor — GC behavior is unchanged. The
status-page handler treats `(_, false)` as "GC has not run yet"
and renders an empty cell rather than a stale number.

### 9.7 Admin listener (NEW)

#### 9.7.1 Listener wiring

A dedicated `*http.Server` bound by
`cmd/apt-cacher-ultra/main.go` between the existing TLS-listener
wiring and the `cache.Open` call. Bind order:

```
1. net.Listen plain  (cache.Listen)
2. net.Listen TLS    (cache.ListenTLS, optional)
3. net.Listen admin  (admin.Listen, optional, default 127.0.0.1:6789)
4. cache.Open
5. cache.SweepTmp    (startup-time tmp/ sweep; SPEC §4.2)
6. GC startup pass   (Phase 4 §9.6)
7. Refresher goroutine started (§9.7.6)
8. Accept goroutines start in parallel
```

(There is no startup `staging/` sweep — SPEC4 §4.2 documents that
staging directories from cancelled adoptions are reaped by the
§9.6 orphan-candidate-snapshot GC pass.)

Why bind early but Accept late (parallel to SPEC4 §4.2):

- A bind failure (port in use, permission denied) fails-fast
  before the cache directory is opened or any GC work begins.
- `Accept()` deferred to step 8 means an early-arriving scrape
  request sees a TCP connect with a small delay (the scrape
  client's `connect_timeout`), then a normal response — never RST
  / connection-refused. Important because Prometheus's scrape
  loop treats RST and refused as different signals from slow.

The admin listener uses a smaller `ReadHeaderTimeout` (default
5s) and `IdleTimeout` (default 30s) than the proxy listener —
admin requests are short-lived and frequent, not long-streaming.

The admin handler is a `http.ServeMux` with three explicit
routes registered using Go 1.22+ enhanced patterns:

- `GET /metrics` — exact-path match.
- `GET /healthz` — exact-path match.
- `GET /{$}` — exact-path match for the status page (the `{$}`
  terminator restricts the pattern to literal `/`, otherwise
  `/` is a subtree pattern that catches every unmatched path).
- `GET /` (subtree fallback registered last) — handler
  responds 404 unconditionally. This is the catch-all for
  `/anything-not-above`. (Without this fallback, a request like
  `GET /metrics2` would hit the standard `http.ServeMux` 404,
  which is identical in body but unreachable here because the
  pattern set already covers /metrics, /healthz, and `/{$}`.)

Any non-GET method on `/metrics`, `/healthz`, or `/` returns 405
with `Allow: GET, HEAD, OPTIONS`. `OPTIONS` returns 204. The
405-vs-404 distinction is enforced inside each route's handler
(after the mux dispatches) by checking the request method,
because Go 1.22+ patterns reject the wrong method with 405
automatically only when *another* method pattern is registered
for the same path; for these read-only routes, only GET is
declared, so the handler must handle method mismatch itself.

Go version: the project targets Go 1.25 (`go.mod`); enhanced
ServeMux patterns landed in Go 1.22.

#### 9.7.2 Endpoint: `GET /metrics`

Renders the metrics-package registry to the response body in
Prometheus text exposition format
(`text/plain; version=0.0.4; charset=utf-8`). The renderer holds
each metric's mutex *only* long enough to build the per-metric
output into a `strings.Builder`; the builder is then written to
the response body outside the lock. Hot-path Inc/Observe/Set
blocking is bounded by the per-metric series count, regardless
of how slow the scraper consumes the response. See §10.4 for
the metric inventory.

The handler emits `acu_admin_scrape_total` (counter) and
`acu_admin_scrape_duration_seconds` (histogram) for self-
observability. A scrape that errors mid-write logs
`admin_scrape_error` Warn with the error.

#### 9.7.3 Endpoint: `GET /` (status page)

Renders the cache's operational state in either HTML or JSON
depending on content negotiation:

1. `?format=json` query parameter → JSON, regardless of `Accept`
   header. Operators bookmark URLs and curl scripts find query
   strings easier to compose than custom headers.
2. Otherwise, `Accept` header: if `application/json` is
   acceptable AND `text/html` is not, → JSON.
3. Otherwise → HTML (`Content-Type: text/html; charset=utf-8`).

The HTML page renders a "View as JSON →" link at the top
pointing to `/?format=json` so the JSON view is discoverable
from the browser.

JSON Content-Type is `application/json; charset=utf-8`.

The HTML form uses Go's `html/template` (auto-escapes); never
`text/template` or hand-built HTML concatenation. All cache
state values (suite paths, hostnames) flow through template
expressions and are escaped by spec. The JSON path uses
`encoding/json` which escapes by spec.

The page is bounded — top-N lists capped at 20 rows each. Total
page size targets <200 KiB even for a fully-stocked cache.

**Per-query timeout.** Each DB query the status-page handler
issues (`cache.ListSuitesWithAdoption(ctx)`, the `hot_url_paths`
top-20 query) runs under `context.WithTimeout(r.Context(), 5s)`
— a tighter bound than the refresher's 10s because the status
page is interactive and a user looking at the browser tab
expects a response within seconds. On any query timeout or
error, the handler responds 503 with body `service unavailable`
and emits `admin_status_render_failed` Warn with `err`,
`format` (html/json), and `query` (which DB call timed out).
`hostsem.Snapshot()` and `gc.GC.LastRunSummary()` do not need
deadlines — they are pure in-memory reads bounded by mutex hold
time. The §9.7.7 ring snapshot is also lock-bounded.

The HTML page renders an empty-ring explanatory note —
"(empty since last process start)" — when the §9.7.7 ring
returns no events AND the process has been up <5 minutes. This
is a usability cue for operators who restarted recently.

The JSON shape is locked at SPEC5 (backwards-compatible
additions only thereafter); see §10.5.

#### 9.7.4 Endpoint: `GET /healthz`

Three stateless checks, typical wallclock <5ms with a hard 1.5s
ceiling (the DB-ping timeout dominates the worst-case path):

1. **Cache directory writable.** `os.CreateTemp(cache_dir,
   ".acu-healthz-*")`, write 1 byte, close, `os.Remove`.
   `CreateTemp` returns a unique-suffix filename so concurrent
   probes do not race on a fixed filename. Failure to create or
   write is the failure signal. Typical wallclock <2ms on a
   healthy local filesystem.
2. **DB pingable.** `db.PingContext(ctx)` with a 1s deadline.
   This is the single check whose worst case can dominate the
   response — a hung sqlite writer pushes it to ~1s. Typical
   wallclock <1ms.
3. **Process not in graceful shutdown.** Reads an atomic flag
   set by the SIGINT/SIGTERM handler before
   `admin.Server.Shutdown` is called. Microsecond cost.

If all three pass: `200`, body `"ok\n"`. If any fails: `503`,
body `"degraded\n"`, with the failing check name in
`X-Acu-Check-Failed:` header (one of `cache_dir`, `db_ping`,
`shutdown`). No body details — operators read `/metrics` and
the status page for diagnosis; `/healthz` is for binary-decision
automation.

When `admin.htpasswd_file` is configured, `/healthz` requires
auth too — there is no carve-out. Either the probe runs on the
same host (loopback bind, no auth) or the operator configures
the probe to send Basic auth.

#### 9.7.5 htpasswd Basic auth

When `admin.htpasswd_file` is non-empty, every admin request
must present a valid `Authorization: Basic <base64-user:pass>`
header. The middleware:

```go
1. Stat the htpasswd file. Compare the (mtime, size) tuple
   against the cached parse's tuple — if EITHER differs, re-read
   and re-parse. On parse failure, KEEP the prior credential
   map; emit htpasswd_reload_failed Warn.
2. r.BasicAuth() to extract user, pass.
3. If !ok → 401 with `WWW-Authenticate: Basic realm="apt-cacher-ultra admin"`.
4. lookup user in map; if absent, perform a sentinel
   bcrypt.CompareHashAndPassword against a fixed hash so the
   wallclock matches the wrong-password path; return 401.
5. bcrypt.CompareHashAndPassword(map[user], pass). On mismatch
   → 401. On match → next handler.
```

The `(mtime, size)` reload key catches the same-second-rewrite
case that an mtime-only check would miss: a same-second rewrite
that changes credentials almost always changes file size
(adding/removing a user, or replacing a hash, alters the byte
count). It also catches mtime moving *backward* (clock change,
ansible apply on a different host, file restored from backup) —
any change in the tuple triggers reload, regardless of
direction. The known limitation: an editor that saves the file
with the *exact same* size *and* mtime *and* different content
(e.g. swapping two hashes of identical length within the same
second) is not detected; operators in that pathological case
can `touch` the file or restart the daemon. Two cheap syscalls
remain on the request path (one Stat, one BasicAuth).

The htpasswd file format is Apache's:

```
sean:$2y$10$abcdef...
ops:$2b$12$ghijkl...
```

Bcrypt only — `$2a$`, `$2b$`, `$2y$` prefixes accepted; older
Apache MD5 (`$apr1$`), SHA-1 (`{SHA}`), and crypt(3) DES are
rejected at startup with a config error.

Generating a file with the standard Apache utility:

```sh
htpasswd -B -c /etc/apt-cacher-ultra/htpasswd sean
htpasswd -B    /etc/apt-cacher-ultra/htpasswd ops
```

`-B` selects bcrypt; `-c` creates the file (omit for subsequent
appends).

Reload semantics: at startup, the file is parsed once into a
`map[string]string` (user → bcrypt hash) plus a cached
`(mtime, size)` tuple. At every admin request, `os.Stat`
returns the current `(mtime, size)`; if EITHER differs from
the cached tuple, the file is re-parsed and the map atomically
swapped. A reload that fails parsing keeps the prior map in
place and emits `htpasswd_reload_failed` Warn — a mid-edit
broken htpasswd does not lock operators out. The
stat-on-each-request cost is one syscall per admin request,
negligible against the bcrypt comparison and TLS-handshake
costs of typical scrape clients.

Timing-attack mitigation: the no-such-user path performs a
sentinel `bcrypt.CompareHashAndPassword` against a fixed
known-good hash so its wallclock matches the wrong-password
path. Without the sentinel, a wrong-user 401 returns in
microseconds while a wrong-password 401 takes ~100ms (bcrypt
cost), which lets an attacker enumerate valid usernames.

#### 9.7.6 Refresher goroutine

Started after the admin listener binds (step 7 of §9.7.1). The
goroutine performs an **immediate first recompute** before
sleeping, so the first `/metrics` scrape (which can race the
listener-Accept transition by milliseconds) sees populated
gauges rather than zeros. The loop is:

```
1. recompute all gauges (each query under a per-query timeout
   — see "Per-query timeout" below)
2. sleep admin.gauge_refresh
3. goto 1
```

Recomputed gauges:

- `acu_blobs_db_count` — `SELECT COUNT(*) FROM blob`.
- `acu_blobs_db_total_bytes` — `SELECT SUM(size) FROM blob`.
- `acu_blobs_zero_refcount_backlog` — `SELECT COUNT(*) FROM blob
  WHERE refcount <= 0 AND refcount_zeroed_at IS NOT NULL`.
- `acu_pool_disk_bytes` — `filepath.Walk` of `pool/` summing
  `info.Size()`. Single-threaded (the Phase 4 §9.6.4 startup
  scan is the parallel path). On a recompute that overruns the
  next interval, the goroutine guards with a "refresh in
  progress" boolean and skips the next interval rather than
  starting a parallel walk.
- `acu_suites_tracked` — `SELECT COUNT(*) FROM suite_freshness`.
- `acu_url_paths_tracked` — `SELECT COUNT(*) FROM url_path`.
- `acu_snapshots_current` — `SELECT COUNT(*) FROM
  suite_freshness WHERE current_snapshot_id IS NOT NULL`.
- `acu_snapshots_displaced` — `SELECT COUNT(*) FROM
  suite_snapshot WHERE adopted_at IS NOT NULL` minus
  `acu_snapshots_current`.
- `acu_per_host_inflight{host}` and
  `acu_per_host_capacity{host}` — populated from
  `hostsem.Snapshot()` (§9.3). Before populating, the
  per-host gauges call `.Reset()` so a host that no longer has
  in-flight requests stops reporting stale values.

The refresher goroutine is rooted at `lifecycleCtx`; on
shutdown it exits cleanly.

**Per-query timeout.** Each gauge recompute runs under
`context.WithTimeout(lifecycleCtx, 10s)`. The 10s ceiling is
generous enough for a `du`-style `pool/` walk on a
multi-GiB cache and a `SELECT SUM(size) FROM blob` on a
tens-of-thousands-of-rows table, while keeping the refresher
loop responsive — a hung query cannot block subsequent gauges
from refreshing. On timeout (or any other error), the gauge
keeps its prior value (or remains empty if never populated)
and `refresher_query_failed` Warn fires with `metric_name`,
`err`, and `duration_ms`. The next loop iteration retries.

**No mid-refresh contention.** A "refresh in progress" boolean
guards the `du pool/` walk specifically — if the prior walk has
not finished by the time the next interval fires, the next
interval **skips** the walk (other queries proceed normally).
This prevents two concurrent walks of a slow filesystem.

#### 9.7.7 In-memory adoption ring

A small process-local ring buffer in `internal/observability`:

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
    mu   sync.Mutex
    buf  []AdoptionEvent
    head int
    full bool
    cap  int
}

func NewRing(capacity int) *Ring
func (r *Ring) Record(e AdoptionEvent)        // O(1) under lock
func (r *Ring) Snapshot() []AdoptionEvent     // newest-first copy
```

Capacity is 50 events (locked default; not tunable in Phase 5
since the value affects neither correctness nor operator
workflow).

Producers: every adoption-completion site in
`internal/freshness` calls `Ring.Record(...)` alongside the
`adoption_*` log emit and the `acu_adoption_total.Inc(...)`
counter increment. Both successful and failed adoptions are
recorded.

Consumer: the status-page handler calls `Ring.Snapshot()` at
request time. The snapshot copy is owned by the caller and not
mutated by the ring.

The ring is process-local: empty after every restart. The HTML
status page renders an explanatory "(empty since last process
start)" cue when the snapshot is empty AND the process has been
up for less than 5 minutes.

The ring is the only piece of mutable in-memory state Phase 5
adds beyond the metrics registry and the htpasswd cache. It does
not participate in graceful-shutdown ordering: dropping the ring
on shutdown is correct (the data is non-authoritative).

#### 9.7.8 Data-source helpers

Two new accessors source data the §9.7.3 status page renders.
Both are pure additions; existing callers are untouched.

**`cache.ListSuitesWithAdoption(ctx)`.** Today
`cache.ListSuites(ctx)` (queries.go:226) returns
`[]SuiteFreshness` from `SELECT * FROM suite_freshness`. The
row shape includes `current_snapshot_id` but not the snapshot's
`adopted_at`. The status page needs both, and a per-row
`GetSuiteSnapshot` lookup is N×1 round trips that the periodic
freshness scheduler does not need.

```go
type SuiteWithAdoption struct {
    SuiteFreshness
    CurrentAdoptedAt *int64  // unix seconds; nil when not adopted
}

// ListSuitesWithAdoption returns every suite_freshness row LEFT
// JOIN suite_snapshot ON suite_freshness.current_snapshot_id =
// suite_snapshot.snapshot_id.
func (c *Cache) ListSuitesWithAdoption(ctx context.Context) ([]SuiteWithAdoption, error)
```

A single LEFT JOIN keeps it O(1) round trips. A suite_freshness
row whose current_snapshot_id points at a snapshot_id that does
not exist (data-corruption case) returns `nil` adopted_at
without erroring. Existing `ListSuites()` (used by the freshness
scheduler) is **untouched**.

**`gc.GC.LastRunSummary()`** — see §9.6 above for the type and
contract.

---

## 10. Logging (deltas)

Phase 1 + Phase 2 + Phase 3 + Phase 4 logging (SPEC §10 / SPEC2
§10 / SPEC3 §10 / SPEC4 §10) carries forward exactly. Phase 5
adds:

### 10.1 Per-request line additions

None on the proxy listener. The proxy `logRequest(...)` line is
unchanged. Phase 5 adds metric *increments* alongside the log
call (§10.4); the log line itself adds no fields.

The admin listener adds its own per-request log line —
`admin_request` Info — with `method`, `path`, `status`, `bytes`,
`duration_ms`, `auth_user` (empty when no htpasswd configured),
`scrape_id` (random uint64 for correlating with §10.4.8
self-metrics).

### 10.2 New structured events

- **`admin_disabled`** Warn at startup when `admin.enabled =
  false`. Parallel to `gc_disabled` (SPEC4 §10.2).

- **`admin_unauthenticated_non_loopback`** Warn at startup when
  `admin.listen` resolves to anything other than `127.0.0.1`,
  `::1`, or `localhost` AND `admin.htpasswd_file` is empty —
  the operator has widened the trust boundary without auth.
  Parallel to `refuse_unvouched_debs_inert` (SPEC3 §10.2).

- **`admin_authenticated`** Info at startup when
  `admin.htpasswd_file` is non-empty and parses successfully.
  Fields: `user_count`.

- **`admin_request`** Info per admin request. See §10.1 above.

- **`htpasswd_reload_failed`** Warn when the mtime-driven reload
  encounters a parse failure. Fields: `path`, `err`,
  `line_number` (the offending line, 1-indexed). The daemon
  keeps serving with the prior credential map; this Warn is
  the operator-visible signal to fix the file.

- **`admin_scrape_error`** Warn when the `/metrics` handler's
  Render-and-write to the response body returns an error
  (e.g. broken pipe). Fields: `err`, `bytes_written`.

- **`admin_status_render_failed`** Warn when the `/` status page
  handler fails to render (e.g. template-execution error,
  data-source query error). Fields: `err`, `format` (`html` /
  `json`). The handler responds 500 with body `internal error`.

- **`refresher_query_failed`** Warn when the §9.7.6 refresher
  goroutine's query for one expensive gauge fails. Fields:
  `metric_name`, `err`. The gauge keeps its prior value
  (empty if never populated).

- **`metrics_series_cap_reached`** Warn (one-shot per metric per
  process lifetime) when a labeled metric first hits its
  per-metric series cap. Fields: `metric_name`, `cap`. Existing
  series continue to update; subsequent Inc/Observe/Set on a
  new label tuple is dropped silently. This is the
  operator-visible signal to either narrow
  `upstream.allowed_host_regex` or raise `admin.metric_series_cap`.

- **`adoption_form_drift`** Warn (already emitted in Phase 2)
  carries forward unchanged. Phase 5 adds a counter
  (`acu_adoption_form_drift_total`) alongside the existing log
  emit; the log fields are unchanged.

### 10.3 Startup config dump (additions)

Phase 1's startup config dump adds the `[admin]` block (all
seven keys: `enabled`, `listen`, `htpasswd_file`, `gauge_refresh`,
`read_timeout`, `idle_timeout`, `metric_series_cap`) verbatim,
with one synthesized field:

- `admin.htpasswd_users` — count of credentials parsed from
  the htpasswd file (0 when `htpasswd_file` is empty), so the
  operator can read the user count without re-running their
  parse.

The `htpasswd_file` value itself is dumped as the path string;
neither the file content nor any bcrypt hash is logged.

### 10.4 Metric inventory

The metric inventory below is the **contract surface for
operator monitoring**. Adding a new metric without updating this
inventory is a review-failing omission; renaming, retyping, or
removing an existing metric is a backwards-incompatible change
requiring a major-version bump.

**Stable label sets.** Prometheus expects every series of a
given metric to carry the same label keys. Outcomes that fire
before host resolution (`method_not_allowed`, `bad_request`)
emit with `host=""` (empty string), **not** with the `{host}`
label omitted.

**Per-metric series cap.** Each labeled metric carries a
`maxSeries` cap (default 1024 via `admin.metric_series_cap`).
Overflow drops the increment with a one-shot
`metrics_series_cap_reached` Warn (§10.2). Existing series
continue to update normally.

**Naming conventions.** `*_total` for counters,
`*_seconds`/`*_bytes` for typed gauges/histograms, `_count`
suffix for histogram observation counts. Every **application**
metric this process exposes carries the `acu_` prefix. **Two
explicit exceptions** preserve interoperability with standard
Prometheus dashboards and alerting rules:

- `process_*` (CPU, RSS, FDs, start time) — emitted unprefixed
  to match the names the official Prometheus client libraries
  use, so dashboards keyed on `process_resident_memory_bytes`
  Just Work.
- `go_*` (goroutine count, GC stats, memstats) — same rationale.

These two prefixes are exclusively reserved for the §10.4.7
process collector; new application metrics must use `acu_`.

**Render hold-time.** The renderer holds each metric's lock
*only* long enough to build its output into a `strings.Builder`;
the buffer is then written to `io.Writer` outside the lock.
Hot-path Inc/Observe/Set blocking is bounded by the per-metric
series count regardless of writer speed.

#### 10.4.1 Request path (handler.go)

`acu_requests_total{outcome, host}` — counter. Outcomes are the
**exhaustive** set passed to `handler.logRequest(...)`:

| Group | Outcomes (host="" for pre-host outcomes; host populated otherwise) |
|---|---|
| Pre-host | `method_not_allowed`, `bad_request` |
| Hit-path | `hit`, `hit_stale`, `hit_coalesced` |
| Miss-path success | `miss` |
| Pre-fetch refusal | `forbidden` |
| Upstream-driven | `upstream_status` (4xx passthrough), `bad_gateway` (5xx, redirect blocked, invalid URL, default arm) |
| Local-fault | `cache_write_failed`, `client_canceled` |
| SPEC2 §6.2 .deb validation | `package_hash_mismatch`, `package_hash_conflict` |
| SPEC2 §6.2 metadata recovery | `snapshot_member_refetch_mismatch`, `snapshot_recovery_failed`, `snapshot_recovery_upstream_status`, `snapshot_recovery_upstream_unreachable`, `snapshot_recovery_cache_write_failed`, `snapshot_recovery_target_denied` |
| SPEC3 §6.1 strict-mode | `unvouched_deb_refused`, `unvouched_deb_passthrough_no_coverage` |

`acu_request_duration_seconds{outcome, host}` — histogram.
Buckets: `0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30, 60`.

`acu_response_bytes{outcome, host}` — histogram.
Buckets: `1024, 4096, 65536, 262144, 1048576, 10485760, 104857600,
1073741824` (1 KiB → 1 GiB).

`acu_inflight_requests` — gauge (`handler.activeWG` count). No
host label (the gauge is process-wide, not partitionable
cleanly).

#### 10.4.2 Fetch path (internal/fetch)

`acu_fetch_total{outcome, host}` — counter. The classifier below
is a **total function** mapping every fetch terminal return to
exactly one outcome label. The success-vs-error split needs the
operation context (a `nil` error from `Fetch()` means HTTP 200
streamed; a `nil` error from `Conditional()` carries either 200
or 304 in `*ConditionalResult.Status`), so the classifier is
exposed as **two helpers** rather than one:

```go
// ClassifyFetchOutcome maps a fetch.Fetch() return to an outcome
// label. err is the second return value; nil → "success".
func ClassifyFetchOutcome(err error) string

// ClassifyConditionalOutcome maps a fetch.Conditional() return
// to an outcome label. res is the first return value (may be
// nil when err != nil); err is the second. A nil err with
// res.Status == 200 → "cond_changed"; nil err with status 304
// → "cond_unchanged".
func ClassifyConditionalOutcome(res *ConditionalResult, err error) string
```

Both helpers walk the error chain in the **precedence order
listed below** (first match wins). Precedence matters because
the dialer wraps cooldown-probe failures with **both**
`ErrUpstreamUnavailable` *and* `ErrHostUnreachable`
(`internal/fetch/dialer.go:163`), so checking
`ErrUpstreamUnavailable` first would mask `host_unreachable`.
Specific causes always come before generic causes:

| Order | Match | Outcome label |
|---|---|---|
| 1 | `nil` err and `Fetch` op | `success` |
| 2 | `nil` err and `Conditional` op, `res.Status == 200` | `cond_changed` |
| 3 | `nil` err and `Conditional` op, `res.Status == 304` | `cond_unchanged` |
| 4 | `errors.Is(err, ErrHostNotAllowed)` | `host_not_allowed` |
| 5 | `errors.Is(err, ErrTargetDenied)` | `target_denied` |
| 6 | `errors.Is(err, ErrInvalidURL)` | `invalid_url` |
| 7 | `errors.Is(err, ErrRedirectBlocked)` | `redirect_blocked` |
| 8 | `errors.Is(err, ErrConditionalBodyTooLarge)` (Conditional only) | `body_too_large` |
| 9 | `errors.Is(err, ErrCacheWriteFailed)` | `cache_write_failed` |
| 10 | `errors.Is(err, ErrSizeMismatch)` | `size_mismatch` |
| 11 | `errors.Is(err, ErrInvalidContentRange)` | `invalid_content_range` |
| 12 | `errors.Is(err, ErrTotalSizeMismatch)` | `total_size_mismatch` |
| 13 | `errors.Is(err, ErrHostUnreachable)` (cooldown-probe fast-fail) | `host_unreachable` |
| 14 | `errors.Is(err, context.DeadlineExceeded)` | `timeout` |
| 15 | `errors.Is(err, context.Canceled)` | `canceled` |
| 16 | `errors.As(err, *StatusError)` and code in `[400, 500)` | `4xx` |
| 17 | `errors.As(err, *StatusError)` and code in `[500, 600)` | `5xx` |
| 18 | `errors.Is(err, ErrUpstreamServerError)` (5xx without StatusError) | `5xx` |
| 19 | `errors.Is(err, ErrUpstreamStatus)` (4xx without StatusError) | `4xx` |
| 20 | `errors.Is(err, ErrUpstreamUnavailable)` (retries exhausted; checked AFTER `host_unreachable`) | `unavailable` |
| 21 | `*net.OpError` with `*net.DNSError` in chain | `dns_failed` |
| 22 | other dial-layer `*net.OpError` (connect refused, no route, EHOSTUNREACH at the syscall layer) | `connect_failed` |
| 23 | any other non-nil err | `error` (catch-all) |

Operations 13 and 20 both fire on the dialer-cooldown-probe path
because the error wraps both sentinels (`%w: %w` at
`dialer.go:163`); the order above ensures the specific
`host_unreachable` label wins. Operations 16/17 (StatusError)
come before 18/19 (bare sentinel) so a wrapped
`ErrUpstreamServerError` carrying a real HTTP code surfaces the
code-class label, not the bare-sentinel one — Phase 1 always
wraps with StatusError, but external callers might not. Net dial
errors are matched after all explicit sentinels because
`*net.OpError` can ride alongside any of them.

`acu_fetch_duration_seconds{outcome, host}` — histogram. Same
outcome label set as `acu_fetch_total`.
Buckets: `0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30, 60, 300`.

`acu_fetch_retries_total{host}` — counter, one per `fetch retry`
log emit.

`acu_active_hosts` — gauge from `hostsem.HostCount()`. No host
label (the metric *is* the host count).

`acu_per_host_inflight{host}` — gauge populated from
`hostsem.Snapshot()` by the §9.7.6 refresher (not per
Acquire/Release — that would be hot-path overhead).

`acu_per_host_capacity{host}` — gauge populated from
`hostsem.Snapshot()`; the configured slot capacity per host.
Operators alert on saturation via `acu_per_host_inflight /
acu_per_host_capacity`.

#### 10.4.3 Freshness / adoption (internal/freshness)

`acu_freshness_check_total{result, host}` — counter, result ∈
{`not_modified`, `unchanged`, `changed`, `failed`}.

`acu_adoption_total{outcome, host}` — counter. outcome ∈
{`success`, `parse_failed`, `gpg_failed`, `member_mismatch`,
`member_fetch_failed`, `db_failed`, `unpinned_suite`,
`run_failed`}. Each adoption attempt emits exactly one outcome —
the total of `acu_adoption_total` across all outcome values
equals the number of adoption attempts. `member_fetch_failed`
(a declared member the upstream would not serve intact — size/
content-length mismatch, transport error, or non-404 status) and
`db_failed` (a local cache/DB fault) were broken out of the
`run_failed` catch-all so the dominant real-world failure is
operator-distinguishable; `run_failed` now means "no other
category matched." **`form_drift` is not an outcome here**; see
the next item.

`acu_adoption_form_drift_total{prior_form, new_form, host}` —
counter (separate metric). Each label ∈ {`inline`, `detached`,
`unknown`}. Counts adoptions whose signature form changed from
the prior current snapshot. Independent of outcome — the
adoption that emitted `form_drift` Warn is also counted under
`acu_adoption_total{outcome="success"}`.

`acu_adoption_duration_seconds{outcome, host}` — histogram. Same
outcome label set as `acu_adoption_total` (no `form_drift`
here).
Buckets: `1, 5, 10, 30, 60, 300, 600, 1800, 3600`.

`acu_hot_prefetch_total{outcome, host}` — counter, outcome ∈
{`started`, `complete`, `partial`, `deb_failed`, `hash_mismatch`}.

`acu_adoption_heartbeat_failures_total{host}` — counter (Phase 4
heartbeat failure; mirrors `adoption_heartbeat_failed` Warn from
SPEC4 §10.2).

#### 10.4.4 Integrity (internal/integrity)

`acu_at_rest_scans_total` — counter (one per scan).

`acu_at_rest_corruption_total` — counter (per corruption found).

`acu_hash_validation_failure_total{phase}` — counter, phase ∈
{`fetch`, `at_rest`}.

`acu_pool_corruption_during_adoption_total` — counter (mirrors
`pool_corruption_during_adoption` Warn from SPEC2 §10).

#### 10.4.5 Garbage collection (internal/gc) — Phase 4 sourced

Metric names mirror the source `gc_run_complete` log field names
(SPEC4 §10.2) so an operator grepping logs and Prometheus
together recognizes the same identifier. Every numeric field of
the `gc_run_complete` log line has a counter or gauge equivalent.

`acu_gc_runs_total{phase}` — counter. phase ∈ {`startup`,
`periodic`}.

`acu_gc_blobs_reaped_total` — counter.

`acu_gc_bytes_reclaimed_total` — counter.

`acu_gc_orphan_candidates_reaped_total` — counter.

`acu_gc_displaced_reaped_total` — counter.

`acu_gc_pool_orphans_repaired_total` — counter (mirrors log
field `pool_orphans_repaired`; populated only on the `startup`
phase — periodic ticks emit 0 for this field per SPEC4 §9.6.2).

`acu_gc_pool_orphan_bytes_repaired_total` — counter (mirrors log
field `pool_orphan_bytes_repaired`; same startup-only
contribution).

`acu_gc_pool_unlink_errors_total` — counter.

`acu_gc_deadline_reached_total{phase}` — counter, incremented by
1 each tick whose `deadline_reached` is true (mirrors log field
`deadline_reached`). Operators alert on rate to detect a GC
backlog where ticks exit early due to `gc.max_tick_duration`.

`acu_gc_run_duration_seconds{phase}` — histogram.
Buckets: `0.1, 0.5, 1, 5, 10, 30, 60, 300, 600`.

`acu_gc_last_run_unixtime{phase}` — gauge.

#### 10.4.6 Cache state (refresher-driven gauges)

Refreshed every `admin.gauge_refresh` (default 30s) by the
§9.7.6 refresher.

`acu_blobs_db_count` — gauge.

`acu_blobs_db_total_bytes` — gauge.

`acu_blobs_zero_refcount_backlog` — gauge.

`acu_pool_disk_bytes` — gauge.

`acu_suites_tracked` — gauge.

`acu_url_paths_tracked` — gauge.

`acu_snapshots_current` — gauge.

`acu_snapshots_displaced` — gauge.

#### 10.4.7 Build / process info

`acu_build_info{version, go_version, vcs_revision}` — gauge=1
at startup. `version` is `main.Version` (Makefile-injected via
`-ldflags`); `go_version` and `vcs_revision` come from
`runtime/debug.ReadBuildInfo()`. Read once in
`cmd/apt-cacher-ultra/main.go` (the only package that can name
`main.Version`) and passed into the admin/metrics setup as a
`BuildInfo` value type — `internal/admin` and `internal/metrics`
cannot import `main`.

```go
type BuildInfo struct {
    Version     string
    GoVersion   string
    VCSRevision string
}
```

`acu_process_start_unixtime` — gauge=startup time, set once.

Standard Prometheus process metrics (unprefixed by `acu_` per
the §10.4 naming-convention exception) — emitted via the
metrics package's process collector. Phase 5 emits:

- `process_cpu_seconds_total` (counter)
- `process_resident_memory_bytes` (gauge)
- `process_virtual_memory_bytes` (gauge)
- `process_open_fds` (gauge)
- `process_max_fds` (gauge)
- `process_start_time_seconds` (gauge; equals
  `acu_process_start_unixtime` — both are emitted because the
  former is the conventional name and the latter is the
  application-namespaced name)

Implementation reads `/proc/self/*` on Linux; Phase 5 platforms
are Linux-only. The `go_*` runtime-metrics namespace
(`go_goroutines`, `go_memstats_*`, `go_gc_duration_seconds`) is
**reserved by convention** but not emitted in Phase 5; if a
future phase adds a Go runtime collector, these names are
available unprefixed.

#### 10.4.8 Admin listener self-metrics

`acu_admin_scrape_total` — counter (`/metrics` scrapes served).

`acu_admin_scrape_duration_seconds` — histogram.
Buckets: `0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1`.

`acu_admin_status_total` — counter (`/` status page renders served).

`acu_admin_status_duration_seconds{format}` — histogram. format ∈
{`html`, `json`}.

`acu_admin_healthz_total{status}` — counter. status ∈ {`ok`,
`degraded`}.

`acu_admin_auth_failures_total{reason}` — counter. reason ∈
{`no_credentials`, `unknown_user`, `wrong_password`}. Operators
alert on rate to detect credential-stuffing attempts.

### 10.5 Status page schema

The status page (§9.7.3) renders the following data structure.
The HTML form is a human-readable table layout; the JSON form is
the schema below verbatim.

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
  "hot_url_paths": [
    {"host": "archive.ubuntu.com",
     "path": "/ubuntu/pool/main/l/linux/linux-image-foo.deb",
     "is_metadata": false,
     "request_count": 412,
     "last_requested_unixtime": 1746671280}
  ],
  "recent_adoptions": [
    {"host": "archive.ubuntu.com",
     "suite_path": "ubuntu/dists/jammy",
     "outcome": "success",
     "completed_unixtime": 1746668400,
     "duration_seconds": 4.21},
    {"host": "packages.microsoft.com",
     "suite_path": "/ubuntu/22.04/prod/dists/jammy",
     "outcome": "gpg_failed",
     "reason": "untrusted_signer",
     "completed_unixtime": 1746668412,
     "duration_seconds": 0.001},
    {"host": "packages.icinga.com",
     "suite_path": "ubuntu/dists/icinga-jammy",
     "outcome": "member_fetch_failed",
     "reason": "member_fetch_failed",
     "member_path": "Contents-amd64",
     "detail": "served 114572 vs declared 1664594",
     "completed_unixtime": 1746668420,
     "duration_seconds": 0.18}
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

Field semantics:

- `suites[].current_snapshot_adopted_at_unixtime` is the
  `suite_snapshot.adopted_at` of the row whose `snapshot_id`
  equals `current_snapshot_id`. Sourced from
  `cache.ListSuitesWithAdoption(ctx)` (§9.7.8). `null` when
  `current_snapshot_id IS NULL` or when a data-corruption
  case yields no matching `suite_snapshot` row.
- `gc.last_run_unixtime` is `null` (and the rest of the `gc.*`
  block is omitted) when no GC run has completed since process
  start. Sourced from `gc.GC.LastRunSummary()` (§9.6).
- `recent_adoptions[].completed_unixtime` is the timestamp the
  adoption *finished* (success or failure), not `adopted_at`.
  Sourced from the §9.7.7 in-memory ring; returns `[]` (not
  null) after a process restart.
- `recent_adoptions[].reason` is an additive sub-classification
  that breaks the `gpg_failed` bucket out into the specific
  verifier sentinel — one of `untrusted_signer`, `short_keyid`,
  `no_usable_signature`, `missing_signature`, `ambiguous_keyid`,
  or `crypto_verify_failed`. For non-gpg failure outcomes the
  field mirrors `outcome` (`parse_failed`, `member_mismatch`,
  `member_fetch_failed`, `db_failed`, `unpinned_suite`,
  `run_failed`). Omitted on success rows so the wire shape stays
  unchanged on the happy path. Operators use this field to
  discriminate between "key not installed" (`untrusted_signer`)
  and "signature itself bogus" (`crypto_verify_failed`) without
  grepping logs.
- `recent_adoptions[].member_path` / `.detail` are additive
  fields naming the specific Release member that caused a
  member-scoped failure (`member_fetch_failed` or
  `member_mismatch`) and a short human description, e.g.
  `Contents-amd64` and `served 114572 vs declared 1664594`.
  Sourced from the `*freshness.AdoptionMemberError` carried in
  the adoption error chain. Both are omitted when empty (success
  rows and non-member failures), so the wire shape is unchanged
  on the happy path. The admin HTML surfaces them on the
  adoption row so the operator sees *which* member failed and
  *why* without grepping the `adoption_run_failed` log line.
- `active_hosts` is sourced from `hostsem.Snapshot()` (§9.3).
  Empty when no host has held a slot since process start.
- `hot_url_paths` lists the top 20 `url_path` rows ordered by
  `request_count` DESC, then `last_requested_at` DESC. Fields
  map directly to columns: `host`=`canonical_host`,
  `path`=`path`, `is_metadata`=`is_metadata`,
  `request_count`=`request_count`,
  `last_requested_unixtime`=`last_requested_at`. The query is
  `SELECT canonical_host, path, is_metadata, request_count,
  last_requested_at FROM url_path WHERE last_requested_at IS NOT
  NULL ORDER BY request_count DESC, last_requested_at DESC LIMIT
  20`. **Package-name + architecture are not joined in:** that
  data lives in `package_hash` keyed by `(canonical_scheme,
  canonical_host, path, snapshot_id)`, and surfacing it would
  require a join with current snapshots only — deferred to a
  Phase 6 enhancement to keep the §9.7.3 query cost bounded.
  Operators read package identity from the path.

The HTML form caps each table at 20 rows. Total page size targets
<200 KiB.

---

## 11. Failure-mode catalog (deltas)

Phase 1 + Phase 2 + Phase 3 + Phase 4 catalog (SPEC §11 / SPEC2
§11 / SPEC3 §11 / SPEC4 §11) carries forward exactly. Phase 5
adds:

| Condition | Behavior |
|---|---|
| Admin listener bind fails (port in use, permission denied) | Process exits non-zero with the bind error; `cache.Open` not yet attempted (§9.7.1 step 3). Operator-visible signal: startup error log + non-zero exit. |
| Admin listener Accept errors (file-descriptor exhaustion, etc.) | Per-error Warn `admin_accept_error`. The proxy listener is unaffected. |
| `/metrics` scrape with broken pipe (scraper disconnected mid-write) | `admin_scrape_error` Warn; scrape counter still increments (the scrape was attempted). Hot-path Inc/Observe/Set unaffected — the renderer's lock-hold time is bounded by buffer-build, not by the slow writer. |
| `/healthz` cache-dir check fails (disk full, mount becomes read-only) | 503 with `X-Acu-Check-Failed: cache_dir`. The check uses `os.CreateTemp` so concurrent probes do not race on a fixed filename. Recovers within one check cycle when the cache dir becomes writable again. |
| `/healthz` DB ping fails (sqlite writer hung past 1s) | 503 with `X-Acu-Check-Failed: db_ping`. The 1s ceiling is the worst-case wallclock for `/healthz`. |
| `/healthz` during graceful shutdown | 503 with `X-Acu-Check-Failed: shutdown` — the SIGINT/SIGTERM handler sets the flag before `admin.Server.Shutdown` is called, so a reverse-proxy-driven probe sees `degraded` and steers traffic away. |
| htpasswd file deleted while daemon running | `os.Stat` in the reload check returns an error; reload is skipped, prior credential map continues to authenticate. `htpasswd_reload_failed` Warn fires. Operator restores the file or restarts the daemon. |
| htpasswd file rewritten with parse error mid-edit | Reload swap aborted; prior credential map continues. `htpasswd_reload_failed` Warn fires (one per failed reload attempt). Operators are not locked out by a transient editor save. |
| htpasswd file rewritten with new credentials | mtime-driven reload picks up the change on the next admin request. No restart required. |
| Status page renders against a partially-populated cache (zero suites, zero blobs) | Empty tables render with explanatory headers; no error. The "(empty since last process start)" cue shows in the recent_adoptions section when uptime <5min. |
| Status page renders during graceful shutdown | The §9.5 ordering shuts the admin listener first, so a status-page request mid-shutdown receives a partial response with `Connection: close` rather than mid-write race against a closing DB. |
| Refresher goroutine query timeout (long-running `du pool/`, slow DB SUM) | Each query runs under a 10s `context.WithTimeout`. On timeout: `refresher_query_failed` Warn with `metric_name`/`err`/`duration_ms`; the gauge keeps its prior value. The refresher's "refresh in progress" guard prevents two concurrent walks of `pool/`. The next loop iteration retries. |
| Status-page DB query timeout (slow `ListSuitesWithAdoption`, slow `hot_url_paths` query) | Each query runs under a 5s `context.WithTimeout` derived from the request context. On timeout: handler responds 503 `service unavailable`; `admin_status_render_failed` Warn with `err`/`format`/`query`. Status-page `/` does not hang on a slow DB. |
| Per-metric series cap reached (malicious client, broad regex) | One-shot `metrics_series_cap_reached` Warn; subsequent Inc on a new label tuple is dropped silently. Existing series continue to update normally. Operator narrows `upstream.allowed_host_regex` or raises `admin.metric_series_cap`. |
| Process-collector read of `/proc/self/*` fails (non-Linux platform, container without `/proc`) | Process metrics are zeroed; no error surfaces to the scraper. Phase 5 platforms are Linux-only; this is a defense against future portability work, not a correctness path. |
| Adoption ring snapshot called with a concurrent Record | The ring's mu protects both; the Snapshot copy is independent of subsequent Record calls. |
| Status page renders immediately after a process restart (ring empty, last-run summary `(_, false)`) | HTML page shows "(empty since last process start)" cue for recent_adoptions; JSON page returns `recent_adoptions: []` and `gc: {...}` with `last_run_unixtime: null` and the rest of the `gc.*` block omitted. |
| Build info read from `debug.ReadBuildInfo()` returns `false` (e.g. test binary built without VCS info) | `vcs_revision` and `go_version` are populated as best-effort from the BuildInfo struct (typically empty / runtime version); `acu_build_info` still emits a single series with whatever values were available. The metric gauge=1 invariant holds. |

---

## 12. Test strategy (deltas)

Phase 1 + Phase 2 + Phase 3 + Phase 4 test strategy (SPEC §12 /
SPEC2 §12 / SPEC3 §12 / SPEC4 §12) carries forward exactly. Phase
5 adds:

### 12.1 Unit tests (additions)

#### 12.1.1 internal/metrics

- **Counter Inc / Add.** Verify exposition format: `# HELP`, `#
  TYPE`, label-positional series lines, integer rendering
  (`5`), float rendering (`5.5`), `+Inf` / `-Inf` / `NaN`
  handling.
- **Counter negative-delta drop.** A negative `Add(-5)` is
  silently dropped; the counter does not move backwards.
- **Counter label-arity panic.** `Inc("too", "many")` on a
  counter declared with one label panics at the call site.
- **Histogram Observe.** Bucket counts, `_sum`, `_count`. Verify
  `+Inf` bucket. NaN observation is dropped.
- **Histogram non-ascending buckets panic.** `NewHistogram(...,
  []float64{1.0, 0.5})` panics at construction.
- **Gauge Set / Inc / Dec / Add.** Last-write-wins on Set;
  Inc/Dec/Add are signed.
- **Gauge Reset.** Drops all series; subsequent Set on a
  previously-existing label tuple creates a fresh series.
- **Registry duplicate-name panic.** Declaring two metrics with
  the same name on one registry panics.
- **Label-value escaping.** Special characters (`\`, `"`,
  newline) escape per Prometheus exposition spec.
- **Stable series order.** Sorting is alphabetical on the joined
  label-key string.
- **Concurrent Inc + Render.** No torn reads under `-race`.
- **Series cap.** Declare a counter with cap=3, Inc 10 distinct
  label tuples, verify exactly 3 series exist + a one-shot
  `metrics_series_cap_reached` Warn fires. Subsequent Inc on a
  4th tuple is dropped; Inc on an existing tuple still
  increments.
- **Render hold-time.** Render to a deliberately-slow Writer
  (a pipe with a delayed reader); verify Inc calls during the
  slow Write don't block beyond the buffer-build cost.

#### 12.1.2 internal/hostsem (Snapshot additions)

- `Snapshot()` on a fresh `*Sem` returns an empty map.
- After two `Acquire`s on host A and one on host B, `Snapshot()`
  returns `{"A": {Inflight: 2, Capacity: N}, "B": {Inflight: 1,
  Capacity: N}}`.
- A waiter blocked on `Acquire` (capacity exhausted) does **not**
  count toward Inflight.
- Concurrent `Snapshot` + `Acquire` / `Release` under `-race` —
  no data race; returned values are internally consistent.

#### 12.1.3 internal/observability (adoption ring)

- `NewRing(50)`, record 50 events, `Snapshot()` returns all 50
  newest-first.
- Record 51 events, `Snapshot()` returns 50 (oldest dropped).
- Concurrent Record + Snapshot under `-race` — no data race; the
  Snapshot copy is independent of subsequent Record calls.
- Empty ring, `Snapshot()` returns `[]AdoptionEvent{}` (not
  nil).

#### 12.1.4 internal/cache (ListSuitesWithAdoption)

- Suite with non-NULL current_snapshot_id pointing to an existing
  suite_snapshot row → `CurrentAdoptedAt` populated with the
  row's `adopted_at`.
- Suite with NULL current_snapshot_id → `CurrentAdoptedAt` is
  nil.
- Suite with current_snapshot_id pointing at a non-existent
  snapshot_id (data-corruption case) → `CurrentAdoptedAt` is
  nil; no error.
- Empty database → returns `[]SuiteWithAdoption{}` (not nil).
- Existing `ListSuites()` callers (the freshness scheduler)
  continue to work; the new helper is purely additive.

#### 12.1.5 internal/gc (LastRunSummary)

- `LastRunSummary()` returns `(_, false)` before any run
  completes.
- After `StartupPass()` finishes, `LastRunSummary()` returns
  `(_, true)` with the startup-phase fields.
- After a periodic tick, `LastRunSummary()` reflects the
  periodic-phase fields.
- Concurrent reader of `LastRunSummary()` while a tick is
  updating it — no torn read under `-race`.

#### 12.1.6 internal/fetch (ClassifyFetchOutcome / ClassifyConditionalOutcome)

For each entry in the §10.4.2 classifier precedence table,
construct an `(err)` or `(res, err)` pair and verify the
appropriate helper returns the expected outcome label. ~23 test
cases total (one per precedence row).

The test additionally asserts:

- **The classifier is total.** The catch-all `error` arm fires
  on a bare `errors.New("synthetic")` (not matching any specific
  sentinel) for both helpers.
- **Precedence is correct for the dialer-cooldown wrap.**
  Construct `fmt.Errorf("%w: %w: ...", ErrUpstreamUnavailable,
  ErrHostUnreachable, errors.New("dial timeout"))` (matching the
  `internal/fetch/dialer.go:163` shape) and verify
  `ClassifyFetchOutcome` returns `host_unreachable`, NOT
  `unavailable`. This is the load-bearing precedence test —
  inverting the order silently degrades operator visibility.
- **StatusError takes precedence over bare sentinel.** A
  `*StatusError{Code: 503}` returns `5xx`, even though it
  also matches `ErrUpstreamServerError` via `Is`.
- **Conditional success-status routing.** A nil err with
  `&ConditionalResult{Status: 200}` → `cond_changed`; nil err
  with `Status: 304` → `cond_unchanged`; nil err with
  `Status: 0` (zero value, programming error) → falls through
  to the catch-all `error`.

### 12.2 Integration tests (additions)

#### 12.2.1 Endpoint tests (cmd/apt-cacher-ultra)

- `GET /metrics` → 200, Content-Type
  `text/plain; version=0.0.4; charset=utf-8`, body parses as
  Prometheus exposition.
- `GET /` → 200, Content-Type `text/html; charset=utf-8`, body
  contains `<title>` and the suite-table heading.
- `GET /` with `Accept: application/json` → 200, Content-Type
  `application/json; charset=utf-8`, body parses as JSON
  matching the §10.5 schema.
- `GET /?format=json` → 200, JSON regardless of `Accept`.
- `GET /?format=json` with `Accept: text/html` → JSON wins
  (query param override).
- `GET /healthz` → 200, body `"ok\n"`.
- `GET /healthz` with cache_dir made unwritable → 503, body
  `"degraded\n"`, `X-Acu-Check-Failed: cache_dir`. Recovers to
  200 within one check cycle when the directory is restored.
- `POST /metrics` → 405, `Allow: GET, HEAD, OPTIONS`.
- `OPTIONS /metrics` → 204.
- `HEAD /metrics` → 200, empty body, headers as for GET.
- `GET /unknown` → 404.
- `admin.enabled = false` → no listener bound, port refused.

#### 12.2.2 Auth tests

- `admin.htpasswd_file = ""` (default): all requests succeed
  without `Authorization` header.
- `admin.htpasswd_file = <valid>`: `GET /metrics` without auth
  → 401 with `WWW-Authenticate: Basic realm=...`.
- `GET /metrics` with valid Basic credentials → 200.
- `GET /metrics` with valid user but wrong password → 401, after
  a bcrypt-cost delay.
- `GET /metrics` with no-such-user → 401, with the same
  bcrypt-cost delay (sentinel-bcrypt timing parity).
- `GET /healthz` requires auth too (no carve-out).
- htpasswd file rewritten with new credentials → next request
  picks up the change (mtime-driven reload).
- htpasswd file rewritten with parse error → next request still
  serves with the prior credentials, `htpasswd_reload_failed`
  Warn emitted.
- Startup with htpasswd file containing `$apr1$...` (Apache MD5)
  → process exits non-zero with config error.
- Startup with htpasswd file containing one valid bcrypt and
  one malformed line → process exits non-zero with the line
  number.

#### 12.2.3 Counter-wiring tests

- Issue a request that produces a known outcome (`hit`, `miss`,
  `forbidden`, `bad_gateway`), then scrape `/metrics` and verify
  the corresponding `acu_requests_total{outcome=...,host=...}`
  counter incremented by exactly 1.
- A request that fails before host parsing (`method_not_allowed`)
  produces
  `acu_requests_total{outcome="method_not_allowed",host=""}` —
  validating the empty-host stable-label-set posture from §10.4.
- **Exhaustive outcome coverage.** The test table covers every
  outcome string in the §10.4.1 inventory; a new
  `logRequest(...)` outcome added to the handler without a
  corresponding test-table entry is a CI failure.
- Run a synthetic GC pass with known reaped count, then scrape
  and verify `acu_gc_blobs_reaped_total`,
  `acu_gc_orphan_candidates_reaped_total`,
  `acu_gc_displaced_reaped_total`,
  `acu_gc_pool_orphan_bytes_repaired_total`, and
  `acu_gc_deadline_reached_total{phase="startup"}` increment as
  expected.
- Trigger a freshness check that returns 304, scrape, verify
  `acu_freshness_check_total{result="not_modified",host=...}`
  incremented.
- Issue a fetch that returns 200, then a fetch that returns
  502, then a fetch that times out — verify
  `acu_fetch_total{outcome="success",host=...}`,
  `acu_fetch_total{outcome="5xx",host=...}`,
  `acu_fetch_total{outcome="timeout",host=...}` each incremented
  exactly once (validates the §9.7 / §10.4.2 explicit fetch
  instrumentation).
- Trigger an adoption with a form change → verify
  `acu_adoption_form_drift_total{prior_form,new_form,host}`
  incremented AND `acu_adoption_total{outcome="success",host=...}`
  incremented (form_drift does not replace success).

#### 12.2.4 Status page rendering tests

- Empty cache: HTML renders without error; JSON returns the
  §10.5 schema with empty arrays for `suites`, `hot_url_paths`,
  `recent_adoptions`, `active_hosts`. `gc.last_run_unixtime` is
  null.
- Populated cache: tables show suite rows with adopted_at,
  GC last-run summary, recent adoptions.
- Lagging suite: a suite whose `inrelease_change_seen_at` is
  > `last_success_at` renders with a "(lagging Xh Ym)"
  annotation in the HTML; JSON populates
  `inrelease_change_seen_at_unixtime`.
- HTML escaping: a suite path containing `<script>alert(1)</script>`
  renders escaped; the JSON response carries the raw string with
  `encoding/json` escaping.

### 12.3 Soak (manual / nightly)

- Start the daemon with `admin.enabled = true`. Run a Prometheus
  scrape every 15s for 24 hours under realistic apt-client load.
  Verify:
  - Scrape latency p99 < 10ms throughout the window.
  - No admin-listener resource leaks (file descriptors, goroutine
    count stable).
  - Refresher goroutine continues to update gauges across the
    24h window.
  - `acu_admin_scrape_total` increments at the expected rate
    (~5760 over 24h at 15s cadence).
  - No `metrics_series_cap_reached` Warn under realistic load.

---

## 13. Project layout & tooling (deltas)

Phase 1 + Phase 2 + Phase 3 + Phase 4 layout carries forward.
Phase 5 adds three new packages and modifies a handful of
existing ones:

```
internal/
  metrics/                 # NEW package
    metrics.go             # Counter / Histogram / Gauge primitives;
                           # text exposition renderer; per-metric
                           # series cap; buffered render
    metrics_test.go        # unit tests for §12.1.1
  admin/                   # NEW package
    server.go              # *http.Server wiring, ServeMux,
                           # graceful shutdown
    auth.go                # htpasswd parser + mtime-driven reload
                           # + Basic-auth middleware + sentinel
                           # bcrypt timing parity
    metrics_handler.go     # GET /metrics
    status_handler.go      # GET / (HTML + JSON)
    healthz_handler.go     # GET /healthz
    refresher.go           # §9.7.6 refresher goroutine
    admin_test.go          # endpoint integration tests for
                           # §12.2.1 / §12.2.2 / §12.2.4
  observability/           # NEW package
    ring.go                # adoption-event ring buffer (§9.7.7)
    ring_test.go           # unit tests for §12.1.3
  hostsem/
    sem.go                 # add Snapshot() per §9.3
    sem_test.go            # unit tests for §12.1.2
  cache/
    queries.go             # add ListSuitesWithAdoption() per §9.7.8
    queries_test.go        # unit tests for §12.1.4
  gc/
    gc.go                  # add LastRunSummary() per §9.6
    gc_test.go             # unit tests for §12.1.5
  handler/
    handler.go             # counter wiring at logRequest
                           # call sites (§10.4.1)
  fetch/
    fetch.go               # ClassifyFetchOutcome(err) and
                           # ClassifyConditionalOutcome(res, err)
                           # helpers + explicit instrumentation
                           # around Fetch() and Conditional()
                           # terminal returns (§10.4.2)
    classify_test.go       # unit tests for §12.1.6
  freshness/
    adoption.go            # adoption_form_drift counter +
                           # adoption-ring Record calls;
                           # adoption duration measurement
  config/
    config.go              # [admin] block decoder + validation
                           # (7 keys); admin_disabled /
                           # admin_unauthenticated_non_loopback /
                           # admin_authenticated startup events
cmd/apt-cacher-ultra/
  main.go                  # bind admin listener (§9.7.1);
                           # construct BuildInfo from main.Version
                           # + debug.ReadBuildInfo() and pass to
                           # admin/metrics setup
```

`go.mod` additions:

- `golang.org/x/crypto/bcrypt` — already an indirect dependency
  via the ProtonMail OpenPGP library; promoted to a direct
  dependency for the htpasswd auth path. No new transitive
  dependencies.

CI jobs from earlier phases carry forward. The `go test -race
./...` job picks up the new packages automatically. The `e2e/`
job does not gain a new test (Phase 5 is observability-only; no
new chaos surface). The §12.3 24h soak is a manual / nightly
target, not a per-PR CI gate.

---

## 14. Definition of done

Phase 5 is done when:

1. Every Phase 1 chaos test (SPEC §12.3), Phase 2 chaos tests
   (SPEC2 §12.3, §12.4), Phase 3 chaos tests (SPEC3 §12.3,
   §12.4), and Phase 4 chaos tests (SPEC4 §12.3, §12.4, §12.5,
   §12.6) continue to pass — Phase 5 must not regress prior
   behavior.

2. `internal/metrics` package implemented per §13, with unit
   tests under `-race` covering §12.1.1 (including the
   per-metric series cap and the buffered-render hold-time
   bound).

3. `internal/hostsem.Sem.Snapshot()` added per §9.3, with unit
   tests under `-race` covering §12.1.2.

4. `internal/observability` ring buffer implemented per §9.7.7,
   with unit tests under `-race` covering §12.1.3.

5. `cache.ListSuitesWithAdoption(ctx)` and
   `gc.GC.LastRunSummary()` data-source helpers added per §9.7.8
   / §9.6, with unit tests under `-race` covering §12.1.4 /
   §12.1.5.

6. `internal/fetch.ClassifyFetchOutcome(err)` and
   `ClassifyConditionalOutcome(res, err)` total classifiers
   added per §10.4.2, with unit tests covering every sentinel
   in precedence order (§12.1.6) — including the
   `host_unreachable`-vs-`unavailable` precedence test for the
   dialer-cooldown wrap.

7. Admin listener bound, all three endpoints reachable on the
   default loopback config (§9.7).

8. Counter wiring at every `handler.logRequest(...)` outcome
   covered by §10.4.1; the `/metrics` exposition includes the
   exhaustive set verified by §12.2.3 counter-wiring tests.
   `acu_adoption_form_drift_total` is a separate counter from
   `acu_adoption_total`. New GC counters
   `acu_gc_pool_orphan_bytes_repaired_total` and
   `acu_gc_deadline_reached_total{phase}` are wired.

9. htpasswd auth implemented per §9.7.5; auth tests pass per
   §12.2.2.

10. Status page renders correctly (HTML and JSON) for an empty
    cache, a populated cache, and a cache with a lagging-upstream
    suite. `acu_build_info` populated from `main.Version` passed
    in via `BuildInfo` value type (§10.4.7).

11. `/healthz` uses `os.CreateTemp` (not a fixed filename), flips
    to 503 within one check cycle when cache_dir becomes
    unwritable, and recovers within one check cycle when restored
    (§9.7.4).

12. SPEC5.md reflects the as-built reality (this document is
    updated as we go, not just before).

13. The cache is deployed to one production environment with
    `admin.enabled = true` and default `admin.*` values for at
    least one week. Monitoring shows:
    - `/metrics` scrapes at expected periodic cadence with
      latency p99 < 10ms;
    - cumulative counters advancing in line with proxy-listener
      log activity;
    - status page renders correctly under real traffic;
    - no admin-listener resource leaks (file descriptors,
      goroutine count);
    - no observed `admin_disabled` Warn in the journal (i.e. the
      operator did not opt out unintentionally);
    - no observed `admin_unauthenticated_non_loopback` Warn
      unless the operator deliberately widened `admin.listen`
      and accepted the trust-boundary trade-off;
    - `htpasswd_reload_failed`, `admin_scrape_error`,
      `refresher_query_failed`, and `metrics_series_cap_reached`
      Warns, if any, name specific paths and motivate operator
      investigation.
