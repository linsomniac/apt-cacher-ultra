# apt-cacher-ultra — Phase 1 Specification

Status: **locked for Phase 1 implementation**. Last updated 2026-05-05.

This document specifies the contract for Phase 1, the MVP cache. Phases 2–6 are referenced at the high level captured in the design discussion but not specified in detail here — separate specs will follow as those phases are scoped.

---

## 1. Goals & non-goals

### 1.1 Phase 1 goals

1. **Cache-hit path never blocks on upstream network.** A request for cached content returns from local disk; an unreachable upstream cannot delay a cache hit.
2. **Drop-in replacement for the existing apt-cacher-ng deployment.** Proxy mode, the `http://HTTPS///hostname/path` URL convention, and the same listen port (`:3142`) — fleet ansible config does not change.
3. **Survive upstream outages gracefully.** Stale cached metadata is served; never hang.
4. **Coalesce concurrent fetches** so N clients asking for the same uncached file produce one upstream fetch.
5. **Resumable upstream downloads** via HTTP Range on transient failure.
6. **Periodic + on-request freshness checks** for `InRelease`, with a cooldown to prevent thrash.
7. **A reliable test suite**, with a gating chaos test that fails today on apt-cacher-ng and must pass on us.

### 1.2 Phase 1 non-goals (deferred to later phases)

- Atomic metadata flip (Phase 2).
- by-hash dedup of indices (Phase 2).
- GPG signature verification of `InRelease` (Phase 2).
- Hot-package proactive refresh (Phase 3).
- Garbage collection of unreferenced blobs (Phase 4).
- Status page and `/metrics` endpoint (Phase 5).
- TLS MITM listener (Phase 6).
- Source-package caching, authenticated upstreams, pdiff, mirror-mode polish, multi-arch beyond amd64.

Phase 1 will write data shaped to support these without later schema breaks (e.g. blob refcounts exist from day one even though GC arrives in Phase 4).

---

## 2. Wire contracts

### 2.1 Listener

- **HTTP** on `:3142` by default. Required.
- **HTTPS** on `:3443` opt-in via config (`listen_tls`, `tls_cert`, `tls_key`). Self-signed cert on the trusted internal network.

### 2.2 Proxy mode (primary)

Client config (unchanged from existing apt-cacher-ng deployment):
```
Acquire::http::Proxy "http://cache:3142";
```

apt sends an absolute-URI request:
```
GET http://archive.ubuntu.com/ubuntu/dists/noble/InRelease HTTP/1.1
Host: archive.ubuntu.com
```

Cache parses the absolute URL, applies Remap canonicalization (§3), looks up cache, fetches upstream if needed.

### 2.3 The `http://HTTPS///` convention (preserved as-is)

For HTTPS-only upstreams, sources.list uses:
```
deb http://HTTPS///apt.corretto.aws/ stable main
```

apt sends:
```
GET http://HTTPS///apt.corretto.aws/dists/stable/Release HTTP/1.1
```

Cache parser recognizes the `HTTPS///` host segment as the magic prefix. The next path segment is the real upstream host, the remainder is the path, and upstream protocol is HTTPS. Behavior is bit-for-bit identical to apt-cacher-ng.

### 2.4 Mirror mode (secondary, supported)

sources.list:
```
deb http://cache:3142/ubuntu/ noble main
```

apt sends a relative-URI request to the cache:
```
GET /ubuntu/dists/noble/InRelease HTTP/1.1
Host: cache:3142
```

Each `[[mirror]]` config entry maps a `prefix` (e.g. `/ubuntu`) to an `upstream` URL. First-prefix-match wins.

### 2.5 Range requests

- **Inbound:** the cache honors `Range` requests against any cached blob. Spec: RFC 7233 single-range only (no multipart) for Phase 1.
- **Outbound:** upstream fetches use Range to resume after transient failures. See §6.3.

### 2.6 HTTP methods

`GET` and `HEAD` only. All other methods → `405 Method Not Allowed`. apt does not use `POST`/`PUT` for repository access.

### 2.7 Response headers added by the cache

- `X-Cache`: `HIT`, `MISS`, `HIT-STALE`, `HIT-COALESCED` (waited on a singleflight)
- `X-Cache-Age`: seconds since the cached blob was last fetched from upstream
- `X-Upstream-Status`: when upstream fetch was attempted, the upstream HTTP status (or `unreachable`/`timeout`)

These are diagnostic and never affect apt correctness.

---

## 3. URL canonicalization (Remap)

### 3.1 Why

Different geo mirror DNS names (`de.archive.ubuntu.com`, `us.archive.ubuntu.com`, `archive.ubuntu.com`) serve identical content under identical paths. Without canonicalization, three machines using three different mirrors cache the same bytes three times.

### 3.2 Algorithm

Input: `(scheme, host, port, path)` from the request URL.

1. Apply user `[[remap]]` rules from config in order. First match wins.
2. If no user rule matches, apply built-in Remap rules in order. First match wins.
3. If no rule matches, the canonical host is the input host.

User rules run first so a user can override a built-in (e.g. point all `*.archive.ubuntu.com` at an internal mirror without disabling the built-in set).

Output: `(canonical_scheme, canonical_host, path)`. Port is dropped from the canonical form (assumed default for the scheme). **The canonical tuple including scheme is the cache key everywhere** — SQLite primary keys, singleflight keys, and freshness keys all include `canonical_scheme`. Different schemes for the same host/path may resolve to different upstream content (notably the `HTTPS///` convention) and must not collide.

### 3.3 Built-in default rules

```
^([a-z]{2}\.)?archive\.ubuntu\.com$           -> archive.ubuntu.com
^([a-z]{2}\.)?security\.ubuntu\.com$          -> security.ubuntu.com
^([a-z]{2}\.)?ports\.ubuntu\.com$             -> ports.ubuntu.com
^(ftp\.)?[a-z]{2}\.debian\.org$               -> deb.debian.org
^deb\.debian\.org$                            -> deb.debian.org
^security\.debian\.org$                       -> security.debian.org
```

Users can override or extend via `[[remap]]` config entries (§5).

### 3.4 The HTTPS/// case

When the URL parser detects the `HTTPS///` magic, it produces a canonical tuple with `canonical_scheme = "https"` *before* Remap rules run. Remap rules then operate on the real underlying host.

---

## 4. Storage layout

### 4.1 Disk

```
<cache_dir>/                               # configurable, default /var/cache/apt-cacher-ultra
  cache.db                                 # SQLite (WAL mode)
  cache.db-wal
  cache.db-shm
  pool/
    <hash[0:2]>/<hash>                     # content-addressed blobs (sha256 hex)
  tmp/
    <uuid>                                 # in-flight downloads (temp files)
  staging/                                 # reserved for Phase 2 atomic flip
```

Blobs are written to `tmp/<uuid>`, hash-validated, then atomically renamed to `pool/<hash[0:2]>/<hash>`. The two-character prefix bucket exists so directory listings remain manageable at scale.

### 4.2 Startup cleanup

On startup, `tmp/` is swept: any file with mtime older than `5 minutes` is deleted (orphaned partial downloads from a previous crash).

### 4.3 SQLite schema

```sql
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA foreign_keys = ON;

CREATE TABLE blob (
  hash         TEXT PRIMARY KEY,             -- sha256 hex (lowercase, 64 chars)
  size         INTEGER NOT NULL,
  created_at   INTEGER NOT NULL,             -- unix epoch seconds
  refcount     INTEGER NOT NULL DEFAULT 0    -- populated for Phase 4 GC
);

CREATE TABLE url_path (
  canonical_scheme  TEXT NOT NULL,           -- "http" or "https"
  canonical_host    TEXT NOT NULL,
  path              TEXT NOT NULL,
  blob_hash         TEXT REFERENCES blob(hash),
  upstream_url      TEXT NOT NULL,           -- the real upstream URL we fetch from
  is_metadata       INTEGER NOT NULL,        -- 1 = index/Release/InRelease/etc., 0 = .deb
  last_requested_at INTEGER,
  request_count     INTEGER NOT NULL DEFAULT 0,
  last_fetched_at   INTEGER,                 -- last time we hit upstream for this path
  upstream_etag     TEXT,                    -- validator for resumable-fetch If-Range (§6.3)
  upstream_lastmod  TEXT,                    -- validator fallback when ETag absent
  PRIMARY KEY (canonical_scheme, canonical_host, path)
);

CREATE INDEX idx_url_path_metadata     ON url_path(is_metadata);
CREATE INDEX idx_url_path_last_req     ON url_path(last_requested_at);

CREATE TABLE suite_freshness (
  canonical_scheme         TEXT NOT NULL,
  canonical_host           TEXT NOT NULL,
  suite_path               TEXT NOT NULL,    -- e.g. "/ubuntu/dists/noble"
  last_check_at            INTEGER,
  last_success_at          INTEGER,
  inrelease_etag           TEXT,             -- for conditional GET
  inrelease_lastmod        TEXT,             -- for conditional GET
  inrelease_change_seen_at INTEGER,          -- diagnostic: upstream has a newer InRelease we have not adopted (Phase 1; Phase 2 atomic flip will adopt)
  PRIMARY KEY (canonical_scheme, canonical_host, suite_path)
);

CREATE TABLE schema_version (
  version INTEGER PRIMARY KEY                 -- forward-only; downgrades not supported
);
INSERT INTO schema_version VALUES (1);
```

### 4.4 Suite identification

A request path matching `^/(?:(.+)/)?dists/([^/]+)(?:/.*)?$` identifies `(repo_path = $1, suite_codename = $2)`. `repo_path` may be empty — some upstreams (`apt.corretto.aws`, `repo.charm.sh`) serve `/dists/...` directly off the host root. The cache derives `suite_path = "<repo_path>/dists/<suite_codename>"` (or `"/dists/<suite_codename>"` when `repo_path` is empty) and stores per-suite freshness state under `(canonical_scheme, canonical_host, suite_path)`.

A request that does not match this pattern (e.g. a `.deb` in `pool/`) is not associated with any specific suite for freshness purposes — it's just a blob.

### 4.5 Classifying metadata vs. blob

A path is `is_metadata = 1` if its filename matches:
- `InRelease`, `Release`, `Release.gpg`
- `Packages*`, `Sources*`, `Contents-*`
- `Translation-*`, `Components-*`, `icons-*`
- `*.diff/Index`, `by-hash/*`

Everything else is treated as an immutable blob (`.deb`, `.udeb`, `.tar.*`, etc.).

This drives policy: metadata is subject to freshness checks; blobs, once stored with the right size, are forever.

---

## 5. Configuration (TOML)

Default location: `/etc/apt-cacher-ultra/config.toml`. Override with `--config <path>`.

### 5.1 Example

```toml
[cache]
dir         = "/var/cache/apt-cacher-ultra"
listen      = "0.0.0.0:3142"
listen_tls  = ""                          # empty disables TLS listener
tls_cert    = ""
tls_key     = ""

[upstream]
connect_timeout         = "30s"
total_timeout           = "5m"
idle_read_timeout       = "60s"
max_retries             = 3               # for resumable Range retries
max_concurrent_per_host = 8

# Open-proxy hardening (§6.6). Only upstream hosts matching one of these
# regexes will be fetched. Empty list = deny-all (cache becomes read-only on
# miss). The defaults cover the apt repos we know about; users add their own.
allowed_host_regex = [
  '^([a-z0-9-]+\.)*ubuntu\.com$',
  '^([a-z0-9-]+\.)*debian\.org$',
  '^ppa\.launchpadcontent\.net$',
  '^apt\.corretto\.aws$',
  '^repo\.charm\.sh$',
  '^pkg\.haproxy\.com$',
  '^download\.docker\.com$',
]
# After DNS resolution, every resolved IP is checked against these CIDRs and
# the request is refused if any match. Defense-in-depth against DNS rebinding
# and against an attacker-registered hostname that resolves into private space.
deny_target_ranges = [
  "127.0.0.0/8", "::1/128",
  "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
  "169.254.0.0/16", "fe80::/10",
  "::ffff:127.0.0.0/104",                 # IPv4-mapped loopback
]

[freshness]
cooldown          = "60s"                 # min interval between freshness checks per suite
periodic_refresh  = "15m"                 # background refresh cadence per known suite

[serve]
serve_stale_when_upstream_down = true
log_stale_serves               = true

[log]
level  = "info"                           # debug | info | warn | error
format = "json"                           # json | text

# Custom Remap rules. Built-in rules (§3.3) are applied first; these extend them.
[[remap]]
match_host_regex = '^my-internal-mirror\.example\.com$'
canonical_host   = "archive.ubuntu.com"

# Optional secondary mirror-mode prefixes.
[[mirror]]
prefix   = "/corretto"
upstream = "https://apt.corretto.aws/"
```

### 5.2 Config validation

On startup the cache validates:
- `cache.dir` exists and is writable.
- `cache.listen` parses as a valid `host:port`.
- TLS fields are all-set or all-empty.
- Remap regex compiles.
- `upstream.allowed_host_regex` entries compile.
- `upstream.deny_target_ranges` entries parse as valid CIDRs.
- Mirror prefixes start with `/` and don't overlap.
- All durations parse.

Invalid config → exit 1 with a clear error. Never start with a partial / fallback config.

---

## 6. Request handling

### 6.1 The fast path: cache hit

```
1. Parse request URL → (scheme, host, path) + HTTPS/// detection.
2. Apply Remap → (canonical_host, path).
3. SELECT blob_hash FROM url_path WHERE canonical_host=? AND path=?
4. If blob_hash found AND blob exists on disk:
   a. Open pool/<h[0:2]>/<h>.
   b. Send response headers (Content-Length, Content-Type best-effort, X-Cache: HIT).
   c. sendfile (or io.Copy) the file (honoring Range).
   d. UPDATE url_path SET last_requested_at=now, request_count=request_count+1.
   e. If is_metadata=1, trigger async freshness check (§7).
   f. Done.
```

This path **never makes a network call** before sending response headers. Every step is local disk + SQLite.

### 6.2 Cache miss: singleflight fetch

```
key := canonical_scheme + "|" + canonical_host + "|" + path
result := singleflight.Do(key, func() (*FetchResult, error) {
    // Fetch upstream with deadline = upstream.total_timeout
    // Stream to tmp/<uuid> while computing sha256
    // On completion, atomic-rename to pool/<h[0:2]>/<h>
    // INSERT/UPDATE blob, url_path
    // Return blob hash + size
})
// All concurrent callers see the same result.
// Then each caller streams from the now-cached file (honoring its own Range).
```

Phase 1 chooses **coalesce-and-serialize**: the second-through-Nth concurrent client waits for the first fetch to finish, then reads from the cached file. They do not stream the partially-downloaded bytes. Rationale: simpler, correct, and the typical apt workload sees infrequent large concurrent first-fetches. **Streaming-while-fetching is a candidate Phase 2 optimization** if measurement shows it matters.

`X-Cache: MISS` for the first; `X-Cache: HIT-COALESCED` for the rest.

### 6.3 Resumable upstream fetch

The upstream client retries on transient failure (connection reset, EOF mid-stream, 5xx) up to `max_retries` times. On the *initial* fetch we capture the upstream's `ETag` and/or `Last-Modified` headers as validators. Each retry sends:

- `Range: bytes=<written>-` to resume from where the previous attempt left off.
- `If-Range: <validator>` (preferring `ETag`, falling back to `Last-Modified`) so the server only honors the resume if the underlying object hasn't changed.

A retry is accepted only if **all** of the following hold:

1. Response status is `206 Partial Content`.
2. `Content-Range` parses as `bytes <start>-<end>/<total>` and `<start>` equals our written cursor.
3. `<total>` matches the total length recorded on the initial fetch.

If the server returns `200 OK` instead (which happens when the `If-Range` validator no longer matches), the partial file is discarded and the fetch restarts from byte 0. Without these checks, an upstream object swap mid-fetch could splice bytes from two different versions into one cached blob.

Hash is computed incrementally over the whole stream so a complete-then-verified blob is always consistent. The temp file is renamed into `pool/` only after the full blob is hash-finalized; failed resumes leave nothing in `pool/`.

If all retries fail and we have a stale cached copy: serve stale (metadata only). If no cached copy: 502.

### 6.4 Cache miss with upstream down

For metadata: if a stale cached entry exists, serve with `X-Cache: HIT-STALE`. If no entry, 502 Bad Gateway with `Retry-After: 30`.

For `.deb`: 502 Bad Gateway with `Retry-After: 60`. apt will retry; if upstream comes back, next request succeeds.

### 6.5 Hash validation in Phase 1

In Phase 1, the cache:
- Computes sha256 of every fetched blob and stores it.
- **Does not** validate against `InRelease`/`Packages` (Phase 2 will).
- **Does** reject mismatched `Content-Length` (declared vs. received). Treats as a fetch failure.

The cache trusts upstream byte-for-byte in Phase 1. Phase 2 closes this hole.

### 6.6 Upstream allowlist (open-proxy hardening)

Listening on `0.0.0.0:3142` and willing to fetch arbitrary absolute proxy URLs is by definition an open-proxy posture. Even on a trusted internal network the blast radius is meaningful: anyone who can reach the cache port can use it to probe internal services or cloud metadata endpoints (`169.254.169.254`, `fd00:ec2::254`, …).

Phase 1 enforces two layered defenses, both configured in `[upstream]` (§5.1):

1. **Host allowlist (`allowed_host_regex`):** before any upstream connection, the canonical host must match at least one regex in the list. Default ships with the well-known apt repository hosts; users add their own. Empty list = deny-all (cache becomes read-only on miss).
2. **Target range deny-list (`deny_target_ranges`):** *after* DNS resolution, every resolved IP is checked against these CIDRs. RFC1918, loopback, link-local, IPv4-mapped loopback, and IPv6 link-local are denied by default. This is defense-in-depth against DNS rebinding and against an attacker registering a real-looking hostname that resolves into private space.

If either check fails the cache returns `403 Forbidden` to the client, logs the attempt with the offending URL and resolved IP, and makes no upstream connection.

These checks are belt-and-suspenders, not a substitute for network-level isolation: operators are expected to bind the cache to an internal interface or firewall the listener port at the OS level.

---

## 7. Freshness state machine (per suite)

For each `(canonical_host, suite_path)` discovered through requests, the cache maintains freshness state in `suite_freshness`.

### 7.1 Triggers

A freshness check on suite `S` is *attempted* when:
- (T1) Any cached metadata file under `S` is requested by a client.
- (T2) The periodic timer for `S` fires (every `freshness.periodic_refresh`).

### 7.2 Algorithm

```
attempt_freshness_check(S):
  now := time.Now()
  acquire S.in_memory_check_lock (try)
    // If another goroutine is already checking S, just return.
  if !acquired:
    return

  // Cooldown gate.
  if now - S.last_check_at < freshness.cooldown:
    release lock; return

  // Conditional GET on InRelease.
  url := upstream URL for S/InRelease
  req := GET url with If-None-Match: S.inrelease_etag
                       If-Modified-Since: S.inrelease_lastmod
  ctx := context with deadline = upstream.total_timeout
  resp, err := upstream.Do(ctx, req)

  if err != nil OR resp.StatusCode in 5xx:
    S.last_check_at = now            // bump anyway: don't hammer broken upstream
    log("freshness check failed", S, err)
    release lock; return

  if resp.StatusCode == 304:
    S.last_check_at = now
    S.last_success_at = now
    release lock; return

  if resp.StatusCode == 200:
    body := read body, hash
    if hash == hash of cached InRelease for S:
      // bytes unchanged despite no 304 (upstream didn't honor conditional GET)
      S.last_check_at = now
      S.last_success_at = now
      S.inrelease_etag = resp.ETag
      S.inrelease_lastmod = resp.LastModified
      release lock; return

    // New InRelease detected at upstream. Phase 1 deliberately does NOT
    // replace the cached InRelease here. Adopting it without also refreshing
    // every index it references would create a hash-mismatch window for any
    // client mid-update — exactly the failure mode this project exists to
    // eliminate. Until Phase 2's atomic-flip transaction lands, the cache
    // continues serving the *current* (matching) InRelease + indices set;
    // clients see consistent metadata even if it is hours older than upstream.
    S.last_check_at = now
    S.last_success_at = now
    S.inrelease_change_seen_at = now           // diagnostic; surfaces in logs
    log("InRelease changed at upstream; awaiting Phase 2 atomic flip", S)
    release lock; return
```

### 7.3 Off the request path

Triggers from T1 spawn a goroutine to run the algorithm; the request that triggered it has already been served. The lock is in-memory (a `sync.Map[suite_key]*sync.Mutex` with TryLock) and holds only for the duration of the upstream call.

### 7.4 Periodic scheduler

A single goroutine ticks every `freshness.periodic_refresh / 4` (a "fast" tick) and inspects all known suites; any suite whose `last_success_at` is older than `freshness.periodic_refresh` gets an `attempt_freshness_check`. Cooldown logic deduplicates against any T1 check that fired recently.

---

## 8. Stale-and-Valid-Until

Phase 1 does not parse `InRelease` to read `Valid-Until` itself; that's the client's job. The cache's behavior:

- If upstream is reachable: serve fresh (or recently-refreshed-from-cache) content.
- If upstream is unreachable AND we have a cached file: serve cached, mark `X-Cache: HIT-STALE`.
- If the cached file is past its `Valid-Until`, apt will reject it client-side. Document `Acquire::Check-Valid-Until=false` for clients that need to keep working through extended outages.

**Phase 1 may also serve metadata that is *known* to be older than upstream.** When the freshness check (§7.2) detects a new `InRelease` at upstream, Phase 1 records the observation but does *not* adopt it (because adopting without refreshing referenced indices would create a hash-mismatch window). The cache keeps serving the consistent older set until Phase 2's atomic-flip lands. Logs and (Phase 5) the status page expose `inrelease_change_seen_at` so operators can tell when the cache is intentionally lagging upstream.

Phase 1 logs every stale serve (gated by `serve.log_stale_serves`).

---

## 9. Concurrency & deadlines

### 9.1 Per-request

Each inbound request runs in its own goroutine. Per-request `context.Context` with deadline:
- For cache-hit responses: no deadline (limited by `http.Server` global deadlines).
- For cache-miss: derived from `upstream.total_timeout`.

### 9.2 Singleflight

Keyed on the canonical URL string. A second client arriving while a fetch is in flight blocks on the singleflight result, then opens the cached file independently.

### 9.3 Per-host concurrency limit on upstream

A semaphore per upstream canonical host (size `upstream.max_concurrent_per_host`, default 8) prevents the cache from hammering a single upstream during a refresh storm. Clients waiting on the semaphore share a per-suite singleflight, so the practical concurrent-upstream-connections-per-host stays bounded.

### 9.4 SQLite concurrency

SQLite is opened in WAL mode with a single writer goroutine fed by a buffered channel. Reads use the connection pool freely. This avoids `SQLITE_BUSY` under load and makes writes serializable without explicit locking in business logic.

### 9.5 Graceful shutdown

On SIGTERM/SIGINT:
1. Stop accepting new connections.
2. Wait up to 30s for in-flight requests to drain.
3. Cancel any in-flight upstream fetches (their writes to `tmp/` will be discarded).
4. Flush SQLite.
5. Exit.

In-progress downloads in `tmp/` are NOT preserved across restarts — they're cleaned up on next start (§4.2).

---

## 10. Logging

Phase 1 ships `log/slog` JSON output (config `log.format = "json"`). Per-request log line includes:
- `method`, `url`, `canonical_host`, `path`
- `outcome`: `hit` | `miss` | `hit_stale` | `hit_coalesced` | `502` | `error`
- `bytes_sent`
- `duration_ms`
- `upstream_status` (when a fetch was attempted)
- `client_addr`

Plus structured logs for: freshness attempts (success, 304, change-detected, failure), singleflight coalescing, retry-on-transient-failure, blob writes, schema migrations, startup config dump.

Metrics are deferred to Phase 5.

---

## 11. Failure-mode catalog

| Scenario | Phase 1 behavior |
|---|---|
| Cache hit, upstream irrelevant | Serve from disk. |
| Cache miss, upstream OK | Fetch, cache, serve. |
| Cache miss, upstream slow (under deadline) | Fetch (clients wait), serve. |
| Cache miss, upstream times out | 502 + `Retry-After`. |
| Cache miss, upstream 5xx | Retry up to `max_retries`, then 502. |
| Cache miss, upstream drops mid-stream | Resume with Range up to `max_retries`, then 502. |
| Cached metadata, upstream times out | Serve `HIT-STALE`. Trigger background freshness check (which will also fail; cooldown gate kicks in). |
| Cached `.deb`, upstream irrelevant | Serve `HIT`. |
| Two clients, same uncached file | Singleflight: one fetch, both served. |
| Two clients, two different uncached files | Independent fetches (subject to per-host semaphore). |
| Upstream host not in `allowed_host_regex` | `403 Forbidden`; no upstream connection made. Logged. |
| Upstream resolves to a `deny_target_ranges` IP | `403 Forbidden`; connection aborted before TCP. Logged. |
| Resumable Range refused (validator changed → `200`) | Discard partial; restart from byte 0 (still under `max_retries`). |
| Cache disk full | New writes fail; existing reads succeed. Loud error log. Health endpoint (Phase 5) reports degraded. |
| SQLite locked or corrupt at startup | Fail to start with clear error. |
| SQLite write error at runtime | Log; continue serving from disk; refuse to record new entries until recoverable. |
| Upstream returns wrong Content-Length | Treated as fetch failure; partial blob discarded. |
| Upstream returns wrong sha256 (P1: no validation) | Stored as-is. **Phase 2 will reject.** |

---

## 12. Test strategy

### 12.1 Unit tests

- URL parsing: proxy mode, mirror mode, `HTTPS///` magic, malformed inputs.
- Remap: built-in rules + user rules + precedence.
- Config loader: valid configs, every kind of invalid config.
- Freshness state machine: table-driven against a fake clock and fake upstream.
- SQLite schema migration + idempotent re-runs.
- Range math (request parsing, response slicing, edge cases at file boundaries).

### 12.2 Integration tests

A `testutil` package provides:
- `FakeUpstream`: an `httptest.Server` with controllable behavior per route — `OK`, `slow`, `hang`, `drop_after_n_bytes`, `5xx`, `wrong_size`, `wrong_hash`, `304`, `200_with_etag`, etc.
- `RunCache`: starts the cache binary in-process against a temp `cache_dir` and a fake upstream; returns a client.

Tests cover all entries in §11.

### 12.3 Chaos test (the gating test)

```
GIVEN
  a cache configured with upstream U
  a primed cache containing InRelease, Packages, and 5 referenced .deb files for "noble"
WHEN
  U is replaced with a hang-forever fake (no responses, no resets)
  50 concurrent clients each issue {GET InRelease, GET Packages, GET 5 .deb} via apt-style requests
THEN
  every request returns 200 within 100ms (p99)
  every response body matches the primed bytes exactly
  cache RSS stays under 256 MB throughout
  goroutine count returns to baseline within 5s of test end
```

This test fails today on apt-cacher-ng (clients hang waiting for upstream). It must pass on apt-cacher-ultra. CI runs it on every PR.

### 12.4 End-to-end test (slower CI lane)

A docker-compose with: a mock-upstream serving a small but valid Debian repository tree, the cache, and an `apt`-running test container. Asserts a clean `apt update && apt install`. This catches integration bugs the in-process tests can't.

### 12.5 Soak (manual / nightly)

24h soak with synthetic traffic: assert RSS stable, no FD leak, no goroutine leak, SQLite size growth bounded by content.

---

## 13. Project layout & tooling

```
apt-cacher-ultra/
  cmd/apt-cacher-ultra/main.go
  internal/
    cache/        # disk + SQLite cache
    config/       # TOML loader + validation
    fetch/        # upstream HTTP client (deadlines, Range, retries)
    freshness/    # per-suite freshness scheduler
    proxy/        # HTTP server, URL parsing, Remap
    server/       # wiring & lifecycle
  testutil/       # FakeUpstream, chaos harness, fixtures
  packaging/
    nfpm.yaml
    systemd/apt-cacher-ultra.service
    config/config.toml.default
  Makefile
  .golangci.yaml
  go.mod
  README.md
  SPEC.md         # this file
  NOTES.md        # Phase 0 lessons-learned (separate doc)
```

- Go module path: `github.com/linsomniac/apt-cacher-ultra`.
- SQLite: `modernc.org/sqlite` (pure Go) so the `.deb` ships a single static binary, no cgo, no system libsqlite dependency.
- Logging: `log/slog`.
- Linting: `golangci-lint` with a strict config including `errcheck`, `staticcheck`, `gosimple`, `gocritic`, `revive`.
- Packaging: `nfpm` for `.deb` build.
- CI: GitHub Actions running `go test ./...`, `golangci-lint`, the chaos test, and the docker-compose end-to-end test.

---

## 14. Definition of done

Phase 1 is done when:

1. The chaos test (§12.3) passes reliably (10 consecutive runs, no flakes).
2. The end-to-end test (§12.4) passes against real `apt`.
3. The `.deb` package installs cleanly on Ubuntu 24.04 and 26.04 and serves traffic via the systemd unit.
4. The cache is deployed to one production environment for at least one week with monitoring showing zero cache-hit failures and bounded RSS / FD count.
5. SPEC.md reflects the as-built reality (this document is updated as we go, not just before).

