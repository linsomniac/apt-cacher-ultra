# apt-cacher-ultra — Phase 6 Specification

This document specifies the contract for Phase 6: the
HTTPS-upstream caching loop via TLS MITM CONNECT handling. It is a
delta over [SPEC.md](SPEC.md) (Phase 1), [SPEC2.md](SPEC2.md)
(Phase 2), [SPEC3.md](SPEC3.md) (Phase 3), [SPEC4.md](SPEC4.md)
(Phase 4), and [SPEC5.md](SPEC5.md) (Phase 5). Sections that carry
forward unchanged say so explicitly and point at the prior spec;
sections that change describe only the delta. The companion
document [PHASE-6-SCOPING.md](PHASE-6-SCOPING.md) records the
design rationale and the sixteen-question scoping pass that
produced this spec.

Phase 6 is **opt-in additive** over Phase 5. With
`tls_mitm.enabled = false` (the default), no behavior changes:
CONNECT to the proxy listener returns 405 (the existing SPEC §2.6
behavior), the cache continues to serve HTTPS upstreams via the
`http://HTTPS///` URL-prefix convention. With
`tls_mitm.enabled = true`, the proxy listener accepts CONNECT,
generates a leaf cert for the requested host signed by an
operator-trusted CA, completes TLS with the client, dispatches
the inner GET into the existing handler pipeline (§6) with
`canonical_scheme = "https"`. The two paths share Remap (§3),
SSRF gates (§5), the host concurrency limiter (Phase 1
`hostsem.Sem`), and the cache (canonical_scheme=https keys).

**Trust-anchor expansion note.** Enabling Phase 6 distributes the
cache's CA cert to every client machine. Clients that trust the
CA will accept any leaf cert the cache issues — making the cache
a load-bearing trust anchor for the fleet. The CA is constrained
by `tls_mitm.allowed_host_regex` (or its `upstream.*` parent) at
signing time; the auto-generated CA also carries RFC 5280 Name
Constraints (§5.1.2) limiting validity to the same hostname set.
A compromise of the CA key cannot issue certs for hostnames
outside the regex. Operators in adversarial multi-tenant
environments should narrow the regex to a literal upstream list
before enabling. See §11 for the failure-mode catalog.

---

## 1. Goals & non-goals

### 1.1 Phase 6 goals

1. **HTTPS upstream caching via CONNECT-method MITM.** apt
   clients keep their natural `https://upstream/...`
   `sources.list` lines; configure
   `Acquire::https::Proxy "http://cache:3142"`; the cache MITMs
   the resulting CONNECT and caches the inner GET response
   under `canonical_scheme = "https"`. First request to a host
   pays a one-time cert-generation cost (~5–20ms ECDSA); all
   subsequent requests hit the cert cache.

2. **Coexist with the existing `http://HTTPS///` URL convention.**
   The pre-existing path (SPEC §2.3) continues to work unchanged.
   Operators who don't want to distribute a CA keep using it;
   operators who do want HTTPS in `sources.list` opt in to MITM
   with one fleet-wide CA distribution. Both paths produce the
   same canonical `(scheme=https, host, path)` cache key —
   bytes are not double-cached.

3. **Same SSRF / hostname-allowlist posture as Phase 1.**
   `upstream.allowed_host_regex` and `upstream.deny_target_ranges`
   gate CONNECT targets pre-handshake. Reject → 403, no cert
   generation, no TLS handshake. The CONNECT host gate runs the
   same predicate as the inner-GET host gate; an inbound CONNECT
   that would be rejected as a plain GET is also rejected as a
   CONNECT.

4. **Same observability surface as Phase 1–5.** Every CONNECT
   produces a `mitm_connect` log line + `acu_mitm_connect_total`
   counter increment; the inner GET produces the existing
   `request` log line with a new `mitm=true` field and the
   normal `acu_request_total{outcome=...}` increment. Operators
   computing cache-hit rate get one number across MITM and
   plain paths.

5. **No schema migration.** `canonical_scheme` is already
   first-class on every cache table since Phase 1 §3.2; Phase 6
   adds no SQLite migration. The CA storage lives outside the
   database (file in `cache_dir/ca/` or operator-supplied path).

### 1.2 Phase 6 non-goals (deferred)

Carried forward from earlier phases unchanged:

- Source-package caching, multi-arch beyond amd64, pdiff
  (Phase 7+).
- Streaming-while-fetching as a singleflight optimization.
  Deferred to [FUTURE-REVIEW.md §1](FUTURE-REVIEW.md).
- Per-byte upstream read timeouts. Deferred to
  [FUTURE-REVIEW.md §1](FUTURE-REVIEW.md).
- Per-suite freshness cadence variation. Deferred to
  [FUTURE-REVIEW.md §2](FUTURE-REVIEW.md).
- Operator-triggered manual GC (admin endpoint or SIGUSR1).

Newly deferred in Phase 6:

- **Admin-listener TLS.** The Phase 5 admin port stays plain
  HTTP. Operators who need TLS on the admin port front it with
  nginx / Caddy / Traefik. Adding a self-signed-cert flow whose
  only purpose is the admin port doubles the cert-management
  surface for negligible operational benefit.
- **Mutating admin endpoints.** No `/admin/gc/run`,
  `/admin/cache/clear`, `/admin/suites/{path}/refresh`. The MITM
  listener is enough surface for one phase. They graduate into
  Phase 7+ if observational data argues for them.
- **OpenTelemetry / OTLP exporters.** Same disposition as
  Phase 5 §1.2.
- **Distributed tracing.** Same disposition as Phase 5 §1.2.
- **Hot-reload of the `[tls_mitm]` block.** A flip of
  `tls_mitm.enabled` requires a daemon restart. Hot-reload
  defers to a future phase if any deployment asks for it.
- **Per-client CA pinning.** All CONNECTs from any client see
  leaf certs signed by the same CA. Per-client CA bundles
  (each client trusts a different CA) are out of scope.
- **Pre-emptive cert generation.** Leaf certs are generated on
  first CONNECT to a host; no warm-set, no startup
  pre-issuance.
- **CA key rotation as a daemon feature.** Operators who need
  to rotate generate a new CA out of band, push to clients via
  ansible, reconfigure the daemon. A future phase can add a
  rotation subcommand once the operational pattern is clear.
- **HSM / PKCS#11 CA keys.** The `ca_key` config accepts a
  filesystem path only. HSM-backed keys can be plumbed via
  PKCS#11 in a future phase if any deployment asks; the file
  path stays the contract for Phase 6.

---

## 2. Wire contracts (deltas over SPEC §2 / SPEC2 §2 / SPEC3 §2.7)

### 2.1 Proxy listener — unchanged

The proxy listener (`cache.listen`, default `:3142`) and the
optional TLS-to-cache listener (`cache.listen_tls`, default
unset) are unchanged. Phase 6 does NOT add a new listener;
CONNECT is dispatched on the existing plain and TLS listeners
when `tls_mitm.enabled = true`.

### 2.2 CONNECT method handler (NEW)

When `tls_mitm.enabled = true`, the proxy listener accepts the
HTTP `CONNECT` method on **any** path. Per RFC 7231 §4.3.6, the
request-target is `host:port` (e.g. `apt.corretto.aws:443`). The
handler:

1. Parses the request-target into `(host, port)`. Reject any
   port other than `443` with `400 Bad Request` and a
   `mitm_connect` Warn (`outcome=bad_port`). HTTP-on-non-443
   should not arrive on a CONNECT path; non-443 HTTPS is rare
   enough that the simpler default is "443 only" with an
   override knob deferred.
2. Validates `host` against the §5.1 hostname allowlist (the
   union of `upstream.allowed_host_regex` and the optional
   `tls_mitm.allowed_host_regex` narrowing). Reject denied
   hosts with `403 Forbidden` + `mitm_connect`
   Warn (`outcome=denied_host`). The §5 SSRF gate
   (`upstream.deny_target_ranges`) does not apply here — that
   gate runs at TCP connect time inside the inner-GET fetch
   path; rejecting at connect-target IP would force a DNS
   resolution at CONNECT time which the cache otherwise avoids
   doing.
3. Hijacks the underlying TCP connection (Go's
   `http.Hijacker`). Writes
   `HTTP/1.1 200 Connection established\r\n\r\n` to the raw
   conn before any TLS handshake.
4. Looks up `host` (after Remap canonicalization, §3) in the
   leaf-cert cache. Miss → generates a leaf cert (§5.1.3),
   inserts under the canonical-host key. Cert generation is
   singleflighted per canonical host: concurrent CONNECTs to
   `de.archive.ubuntu.com` and `archive.ubuntu.com` (which
   Remap collapses to one canonical host) issue one cert.
5. Performs TLS handshake on the hijacked conn using the leaf
   cert. Handshake failure (client rejects cert, TLS version
   mismatch, etc.) closes the conn and emits `mitm_connect`
   Warn (`outcome=tls_failed`).
6. Reads exactly one HTTP/1.1 request from the now-encrypted
   stream. The request method must be `GET` or `HEAD`; any
   other method returns `405 Method Not Allowed` on the inner
   stream (with `Allow: GET, HEAD`) and closes the tunnel.
7. Constructs a synthetic `*http.Request` (method, path from
   the inner request, scheme=https, host from the CONNECT
   target) and dispatches into the existing handler pipeline
   (the same pipeline a plain proxy GET enters). The inner
   request's response is written back through the encrypted
   stream.
8. Closes the tunnel after the inner request completes.
   Phase 6 does **not** support multiple inner requests per
   tunnel (HTTP/1.1 keepalive within a CONNECT). apt does not
   need this; supporting it would expand attack surface.

### 2.3 Response headers on the inner GET

`X-Cache`, `X-Cache-Age`, `X-Upstream-Status` (SPEC §2.7) are
written on the inner response exactly as they would be on a
plain GET. The CONNECT itself carries no `X-*` cache headers —
the `200 Connection established` line is the only tunnel-level
response.

A new `X-Acu-Mitm: 1` header is added to the inner response so
operators tailing apt's verbose log can confirm the request
path went through MITM. This is diagnostic; never affects apt
correctness. Plain (non-MITM) responses do not carry the
header.

### 2.4 The `http://HTTPS///` convention — unchanged

The Phase 1 URL-prefix convention (SPEC §2.3) continues to
work. apt clients with `sources.list` lines using
`http://HTTPS///apt.corretto.aws/...` and a plain
`Acquire::http::Proxy` setting reach the cache, which detects
the `HTTPS///` prefix in `internal/handler/parse.go` and
treats the request as `canonical_scheme = "https"`. Bytes
fetched via this path and via MITM share the same
canonical key; a request via either path benefits from a hit
populated by the other.

### 2.5 HTTP methods (delta over SPEC §2.6)

The proxy listener accepts:
- `GET`, `HEAD` — unchanged.
- `CONNECT` — when `tls_mitm.enabled = true`. Otherwise the
  pre-Phase-6 behavior (`405 Method Not Allowed`) is preserved.
- All other methods — `405 Method Not Allowed` (unchanged).

The `Allow` header on a 405 response is `GET, HEAD` when
MITM is disabled and `GET, HEAD, CONNECT` when MITM is
enabled.

---

## 3. URL canonicalization (Remap) — unchanged

See SPEC §3. Phase 6 reuses the unchanged Remap pipeline for
both the CONNECT-target-to-canonical-host mapping (§5.1.3) and
the inner GET. The CONNECT target IS subject to Remap before
the leaf cert SAN list is built.

---

## 4. Storage layout (delta over SPEC §4 / SPEC2 §4 / SPEC4 §4)

### 4.1 Disk

Phase 6 adds a `ca/` subtree under `cache_dir/` for the
auto-generated CA when no operator-supplied CA path is set:

```
<cache_dir>/                               # configurable, default /var/cache/apt-cacher-ultra
  cache.db                                 # unchanged through Phase 5
  cache.db-wal
  cache.db-shm
  pool/
  tmp/
  staging/
  ca/                                      # NEW in Phase 6
    ca.crt                                 # 0600, owner = daemon user
    ca.key                                 # 0600
```

When `tls_mitm.ca_cert` and `tls_mitm.ca_key` are set
(operator-supplied path), Phase 6 reads from those paths
directly and does not write to `cache_dir/ca/`. The
`tls_mitm.ca_storage_dir` config tunable overrides the
default `cache_dir/ca/` location for the auto-generated path.

### 4.2 Startup CA materialization

When `tls_mitm.enabled = true` and no operator-supplied CA
path is set, daemon startup checks `cache_dir/ca/`:

1. Both `ca.crt` and `ca.key` exist and parse: load and use.
   Emit `mitm_ca_loaded` Info (`source=generated`,
   `fingerprint_sha256=…`, `not_after_unixtime=…`).
2. Either file missing or fails to parse: generate a new CA
   pair (§5.1.1), write to disk at mode 0600, then load.
   Emit `mitm_ca_generated` Info plus `mitm_ca_loaded` Info.
3. `cache_dir/ca/` cannot be created or written to: startup
   fails with a config-error log line. The daemon does NOT
   downgrade to MITM-disabled silently — an operator who
   asked for MITM gets the loud error.

When `tls_mitm.ca_cert` and `tls_mitm.ca_key` are set:

1. Both files exist and parse: load and use. Emit
   `mitm_ca_loaded` Info (`source=supplied`).
2. Either file missing, fails to parse, or fails the
   sanity check (cert and key match, cert has `BasicConstraints
   CA:TRUE`, cert `not_after` in the future): startup fails
   with a config-error log line.

The CA cert and key are read once at startup and held in
memory for the daemon's lifetime. Hot-reload of the CA is
out of scope (§1.2 / scoping Q7).

---

## 5. Configuration (delta over SPEC §5 / SPEC2 §5 / SPEC4 §5 / SPEC5 §5)

### 5.1 New `[tls_mitm]` block

```toml
[tls_mitm]
enabled            = false                    # default OFF
ca_cert            = ""                       # operator-supplied path; empty = auto-generate
ca_key             = ""                       # operator-supplied path; empty = auto-generate
ca_storage_dir     = ""                       # auto-gen storage dir; empty = <cache_dir>/ca
cert_cache_size    = 256                      # in-memory LRU bound, entries
leaf_cert_lifetime = "720h"                   # 30 days
ca_cert_lifetime   = "87600h"                 # 10 years (auto-generated only)
leaf_algorithm     = "ecdsa-p256"             # also "rsa2048" for legacy clients
allowed_host_regex = ""                       # empty = inherit upstream.allowed_host_regex
require_inner_get_only = true                 # 405 on POST/PUT/etc. inside the tunnel
```

#### 5.1.1 CA auto-generation parameters

When the CA is auto-generated:
- Algorithm: ECDSA P-256 (matches the leaf default).
- Subject: `CN = apt-cacher-ultra CA`,
  `O = apt-cacher-ultra`. Operators can override by supplying
  their own CA.
- Validity: `not_before = now - 5m` (clock-skew tolerance),
  `not_after = now + tls_mitm.ca_cert_lifetime` (default
  10 years).
- Extensions:
  - `BasicConstraints: CA:TRUE, pathlen:0`
  - `KeyUsage: digital_signature, key_cert_sign, crl_sign`
  - `ExtendedKeyUsage: serverAuth`
  - **Name Constraints (RFC 5280 §4.2.1.10)** populated from
    the effective MITM allowlist (§5.1.2). The name
    constraints carry both `permitted` (the regex translated
    to wildcards / suffixes where possible) and `excluded`
    (`localhost`, `127.0.0.0/8`, `::1/128`, RFC 1918 ranges).
    The conversion is best-effort: a regex too complex to
    safely translate yields a Warn at startup
    (`mitm_ca_name_constraints_skipped`) and the CA is
    issued without Name Constraints — the regex still gates
    leaf signing at runtime, but the CA is unconstrained
    cryptographically.

#### 5.1.2 Effective MITM allowlist

The effective allowlist for MITM is:

```
(upstream.allowed_host_regex)
   ∩ (tls_mitm.allowed_host_regex if non-empty, else everything)
```

i.e. a host is MITM-eligible iff it passes BOTH the
upstream-fetch regex (Phase 1) AND the optional MITM-narrower
(Phase 6). Empty `tls_mitm.allowed_host_regex` means
"inherit upstream" (the common case); a non-empty value
narrows further. Setting `tls_mitm.allowed_host_regex`
broader than `upstream.allowed_host_regex` is a configuration
error caught at startup (a host MITM-eligible but not
upstream-eligible would CONNECT successfully then 502 on the
inner GET).

#### 5.1.3 Leaf cert parameters

Per-host leaf certs are generated on first CONNECT to a
canonical host:

- Algorithm: `tls_mitm.leaf_algorithm` (default ECDSA P-256;
  alternative `rsa2048` for pre-2018 client compatibility).
- Subject: `CN = <canonical_host>`.
- Subject Alternative Names: every DNS name that Remap maps to
  the canonical host. For `archive.ubuntu.com` the SAN list
  includes `archive.ubuntu.com`, `de.archive.ubuntu.com`, etc.,
  computed by walking the §3 Remap rules and the configured
  built-in defaults.
- Validity: `not_before = now - 5m`,
  `not_after = now + tls_mitm.leaf_cert_lifetime` (default 30
  days).
- Extensions:
  - `KeyUsage: digital_signature, key_encipherment`
  - `ExtendedKeyUsage: serverAuth`
- Signing: ECDSA-SHA256 against the CA's key, regardless of
  the leaf algorithm (the signing operation runs on the CA
  key; CA is ECDSA P-256).

The leaf cert cache is keyed on `canonical_host` (after
Remap), not on the literal CONNECT target. This guarantees
one cert covers every alias of a host.

### 5.2 Validation

At startup, when `tls_mitm.enabled = true`:

- `tls_mitm.cert_cache_size`: integer ≥ 1; reject 0 and
  negative. Default 256.
- `tls_mitm.leaf_cert_lifetime`: ≥ 5m, ≤ 5y. Reject longer-
  than-5y because a 5-year leaf can't be revoked except by
  flushing the in-memory cache via daemon restart.
- `tls_mitm.ca_cert_lifetime`: ≥ 1d, ≤ 50y.
- `tls_mitm.leaf_algorithm`: `"ecdsa-p256"` or `"rsa2048"`;
  any other value rejected.
- `tls_mitm.allowed_host_regex`: must compile as a Go RE2
  regex.
- `tls_mitm.ca_cert` / `tls_mitm.ca_key`: must both be set
  or both empty. Set: both files must exist, parse, and the
  cert/key must match (`x509.Certificate.PublicKey ==
  PrivateKey.Public()`); the cert must have
  `BasicConstraints: CA:TRUE`; the cert's `not_after` must be
  in the future. Empty: `tls_mitm.ca_storage_dir` (or its
  default `<cache_dir>/ca`) must be creatable.

When `tls_mitm.enabled = false`, all `tls_mitm.*` fields are
ignored. A future config that sets `enabled = false` while
specifying other tunables is accepted silently — the
operator may be staging an upgrade.

### 5.3 Loud configurations (delta over SPEC5 §5.2)

New startup loud-config events:

- `tls_mitm_enabled` Info on every successful boot when
  `enabled = true` — names the CA fingerprint, source
  (`generated` / `supplied`), `not_after`, and the count of
  hosts that match the effective allowlist (computed by
  testing the regex against every host in the built-in
  Remap rules; helps operators sanity-check that the regex
  actually permits the upstreams they care about).
- `tls_mitm_enabled_ca_undistributed` Warn when
  `enabled = true` AND no `mitm_connect`-related successful
  TLS handshakes have been observed in the last
  `30 minutes` of process uptime. Fired once per uptime
  hour from the §9.7.6 refresher goroutine. Surfaces
  "operator turned on MITM but the CA is not yet trusted by
  any client" as an operationally-visible signal rather
  than apt failures buried in client logs.
- `tls_mitm_allowed_host_regex_broader` Error at startup
  when `tls_mitm.allowed_host_regex` is broader than
  `upstream.allowed_host_regex` (config-time check).
  Refuses to start.

---

## 6. Request handling (delta over SPEC §6 / SPEC2 §6 / SPEC3 §6 / SPEC4 §6 / SPEC5 §6)

### 6.1 Method dispatch (delta over SPEC §2.6)

The proxy listener's method dispatch table:

| Method | When `tls_mitm.enabled = false` | When `tls_mitm.enabled = true` |
|--------|------------------------------|------------------------------|
| GET, HEAD | existing handler pipeline | existing handler pipeline |
| CONNECT | 405 (`Allow: GET, HEAD`) | §2.2 CONNECT handler |
| OPTIONS | 405 | 405 |
| All others | 405 | 405 |

The CONNECT branch wraps the existing handler — the inner
GET dispatched in §2.2 step 7 enters the same pipeline (same
Remap, same SSRF gate, same cache lookup, same upstream
fetcher) as a plain absolute-URL GET on the same listener.

### 6.2 Inner GET dispatch

The inner GET is a synthetic `*http.Request` constructed
from:
- `Method`: from the inner request line.
- `URL.Scheme`: `https` (always).
- `URL.Host`: from the CONNECT request-target host (NOT the
  inner request's `Host` header — apt may send a different
  `Host` for SNI vs. the CONNECT target, but the CONNECT
  target is the source of truth for the canonical
  identity).
- `URL.Path`, `URL.RawQuery`: from the inner request line.
- `Host`: same as `URL.Host`.
- `Header`: copied from the inner request, with `Host`
  rewritten if necessary.
- Body: `nil` for GET/HEAD; if a future Phase 6+ allows
  non-GET methods, the body would be the inner-request body
  with a length cap.
- `Context`: a context derived from the CONNECT context, so
  shutdown of the listener cancels in-flight inner GETs the
  same way it cancels plain GETs.

The inner request's `RemoteAddr` is the same as the outer
CONNECT's (the apt client's address) — the cache does not
NAT or hide the originating address.

### 6.3 Response writing

The inner response is written to the encrypted stream
through the same `http.ResponseWriter` interface the plain
GET uses, with a wrapper that:
- Inserts `X-Acu-Mitm: 1` header before headers are flushed.
- Counts bytes written for the §10.3 metric and the
  `request` log line's `bytes` field.

After the inner response completes, the tunnel closes:
- Connection: close on the inner response (apt should not
  expect keepalive within a CONNECT).
- Underlying TCP closes after the TLS close_notify completes.

---

## 7. Freshness and adoption — unchanged

See SPEC2 §7 / SPEC3 §7. The inner GET enters the existing
pipeline; freshness checks, adoption, and singleflight all
behave identically regardless of whether the request arrived
via plain GET or via CONNECT-MITM.

---

## 8. Stale-and-Valid-Until — unchanged

See SPEC §8. The serve-stale-when-upstream-down path applies
to MITM-fetched bytes the same as plain-fetched.

---

## 9. Concurrency & deadlines (delta over SPEC §9 / SPEC2 §9 / SPEC3 §9 / SPEC4 §9 / SPEC5 §9)

### 9.1 Cert generation singleflight

Concurrent CONNECTs to the same canonical host (after Remap)
issue one leaf cert. The singleflight key is the canonical
host; the value is `*tls.Certificate`. Subsequent CONNECTs
that miss the cache during a leader's generation block on
the leader, then read the cached entry. Generation latency
is dominated by ECDSA key generation (~5–10ms for P-256) +
signing (~1–2ms) — well under any reasonable handshake
timeout.

### 9.2 TLS handshake budget

The TLS handshake budget on the hijacked CONNECT conn is
`min(20s, ctx.Deadline)`. Beyond budget, the conn is
closed with a `mitm_connect` Warn
(`outcome=tls_handshake_timeout`). 20s is a generous bound;
healthy clients complete a handshake in single-digit
milliseconds. The budget protects against a client that
opens many CONNECTs and never finishes the handshake.

### 9.3 Inner request budget

The inner GET inherits the existing
`upstream.connect_timeout`, `upstream.total_timeout`, and
`upstream.idle_read_timeout` — Phase 6 adds no new
inner-request budget beyond what Phase 1 set.

### 9.4 In-flight CONNECT tunnels at shutdown

The graceful-shutdown sequence (SPEC4 §9.5, SPEC5 §9.5)
extends to CONNECT tunnels:

1. SIGINT / SIGTERM received.
2. Admin listener Shutdown (Phase 5 §9.5).
3. Plain + TLS proxy listeners concurrent Shutdown.
   `http.Server.Shutdown` will not return until all
   connections (including hijacked CONNECT tunnels) close
   or the shutdown context expires.
4. CONNECT tunnels in TLS handshake or in mid inner-GET
   abort when the shutdown context cancels (the inner
   request's context inherits, so the upstream fetcher
   sees the cancellation and unwinds).
5. Refresher goroutine + GC + cache.Close — same as Phase 5.

**No new shutdown ordering** — Phase 6 reuses the existing
HTTP server's connection-tracking. The hijacked-conn case
is documented Go behavior: `http.Server.Shutdown` notes
that hijacked conns are the caller's responsibility, but
the cache wraps each hijacked conn in a tracking primitive
(a `sync.WaitGroup` increment on hijack, decrement on
close) so Shutdown waits on all of them.

### 9.5 Listener startup ordering (delta over SPEC5 §9.7.1)

The ordering between bind and Accept is unchanged from
Phase 5. The new step is **CA materialization before the
proxy bind**:

1. Validate config.
2. **CA load / generate (Phase 6, NEW)** — if
   `tls_mitm.enabled = true`. Failure here is fatal before
   any listener binds. Rationale: a daemon that bound
   `:3142` then fails on CA load would have the proxy port
   accepting (and 503-ing CONNECT) for the duration of the
   bind-then-fail window; failing at CA load runs the bind
   conditionally on the prerequisite working.
3. net.Listen plain (`cache.listen`).
4. net.Listen TLS (`cache.listen_tls`, optional).
5. net.Listen admin (`admin.listen`, optional).
6. Open cache (cache.Open).
7. Run startup pool/ orphan repair (Phase 4 §9.7.6).
8. Start admin refresher goroutine (Phase 5 §9.7.6) and run
   first synchronous gauge refresh.
9. Begin Accept on plain, TLS, admin listeners.

---

## 10. Logging (delta over SPEC §10 / SPEC2 §10 / SPEC3 §10 / SPEC4 §10 / SPEC5 §10)

### 10.1 Per-request log line — delta

The Phase 1 `request` log line carries an additional field
`mitm` (bool, `false` for plain requests, `true` for
inner-GETs dispatched from a CONNECT tunnel). All other
fields unchanged.

### 10.2 New `mitm_*` event family

Emitted from the §9 CONNECT handler:

- **`mitm_connect`** — once per CONNECT, at conn close.
  Fields: `host`, `port`, `client_addr`, `outcome`
  (`tunneled` / `denied_host` / `bad_port` /
  `tls_handshake_timeout` / `tls_failed` / `cert_gen_failed`
  / `inner_method_rejected`), `duration_ms`,
  `cert_cache` (`hit` / `miss`).
  Emitted at level Info on `tunneled`; Warn on every other
  outcome.

- **`mitm_cert_issued`** — once per cert generation, before
  insertion into the cache. Fields: `canonical_host`,
  `algorithm`, `lifetime_seconds`, `san_count`,
  `gen_duration_ms`. Level Debug (cert generation is
  routine; only operators debugging cert issuance care).

- **`mitm_cert_cache_evicted`** — once per eviction.
  Fields: `canonical_host`, `reason` (`lru` / `expired` /
  `manual` — the last is for a future flush primitive that
  doesn't exist in Phase 6), `age_seconds`. Level Info.

- **`mitm_ca_loaded`** — once at startup. Fields: `source`
  (`generated` / `supplied`), `fingerprint_sha256`,
  `not_after_unixtime`, `name_constraints` (bool — true if
  the CA carries Name Constraints). Level Info.

- **`mitm_ca_generated`** — once when auto-generation runs.
  Fields: `path` (the `cache_dir/ca/` location written),
  `algorithm`, `lifetime_seconds`. Level Info. Operators
  scanning the journal for first-boot cues see this exactly
  once per `cache_dir/ca/` lifecycle.

### 10.3 New `acu_mitm_*` metric family

Registered in the §3 metrics registry alongside the
existing families:

| Metric | Type | Labels | Semantics |
|--------|------|--------|-----------|
| `acu_mitm_connect_total` | counter | `outcome` (matches `mitm_connect.outcome`) | Total CONNECTs by outcome |
| `acu_mitm_connect_duration_seconds` | histogram | (none) | CONNECT lifecycle duration including inner GET |
| `acu_mitm_cert_cache_size` | gauge | (none) | Current entries in the leaf cert cache |
| `acu_mitm_cert_cache_capacity` | gauge | (none) | Configured `cert_cache_size` |
| `acu_mitm_cert_cache_lookups_total` | counter | `outcome` (`hit` / `miss`) | Cert cache hit-rate signal |
| `acu_mitm_cert_issued_total` | counter | `algorithm` | Leaf certs issued lifetime |
| `acu_mitm_cert_evicted_total` | counter | `reason` | Cert evictions lifetime |
| `acu_mitm_ca_not_after_unixtime` | gauge | (none) | Operator's expiry alarm — drives a Prometheus alert when `< now + 30d` |
| `acu_mitm_handshake_duration_seconds` | histogram | (none) | TLS handshake duration (excluding inner GET) |

The §10.4.1 request outcome enum on `acu_request_total` is
unchanged — MITM-fetched requests still emit the same
outcomes (`hit` / `miss` / `bad_gateway` / etc.). The
`mitm` log field is the only request-level indicator that
the request came via CONNECT.

### 10.4 Status page TLS MITM section

A new top-level section "TLS MITM" renders between the
"Listeners" and "Cache" sections of the SPEC5 §10.5 status
page when `tls_mitm.enabled = true`. Fields:

| Field | Source |
|-------|--------|
| Enabled | `tls_mitm.enabled` |
| CA source | `generated` / `supplied` |
| CA SHA-256 fingerprint | hex of the cert's SHA-256 |
| CA `not_after` | UTC timestamp |
| Effective allowlist | string form of the regex |
| Cert cache | `<size> / <capacity>` |
| Last cert issued | `<canonical_host> @ <UTC timestamp>` (empty on no issuance yet) |
| Cert hit rate (60s window) | `<percentage>` (cheap rolling counter, recomputed by the §9.7.6 refresher) |

When `tls_mitm.enabled = false`, the section is omitted.

The JSON form of the status page (§10.5 schema) carries a
top-level `tls_mitm` key — always present, abbreviated to
`{"enabled": false}` when MITM is disabled, full payload
otherwise. This mirrors the Phase 5 SPEC §10.5 invariant
that top-level keys are stable.

---

## 11. Failure-mode catalog (delta over SPEC §11 / SPEC4 §11 / SPEC5 §11)

| Failure | Behavior |
|---------|----------|
| `tls_mitm.enabled = true` but CA file unreadable at startup | Startup fails with config-error log; daemon does not bind |
| Auto-generated CA exists but fails to parse (e.g. corrupted by a partial write across crashes) | Startup logs `mitm_ca_load_failed` Error; daemon does not bind. Operator deletes `cache_dir/ca/*` to force regeneration |
| `cache_dir/ca/` cannot be created (perms, disk full) | Startup fails with config-error log; daemon does not bind |
| CA cert / key mismatch (operator-supplied) | Startup fails with config-error log naming the mismatch |
| CA cert `not_after` already in the past | Startup fails with config-error log naming the expiry |
| Leaf cert generation fails (entropy exhaustion, key gen panics, etc.) | CONNECT closes with `mitm_connect` Warn (`outcome=cert_gen_failed`); the singleflight retries on the next CONNECT |
| Cert cache full + new host requested | LRU eviction; `mitm_cert_cache_evicted` Info on the evicted entry; new cert inserted |
| TLS handshake on hijacked conn fails (client distrusts CA, TLS-version mismatch) | Tunnel closes with `mitm_connect` Warn (`outcome=tls_failed`); apt logs a TLS verification error; `tls_mitm_enabled_ca_undistributed` Warn fires from the refresher when this is the steady state |
| Inner request method is not GET/HEAD | 405 written on the inner stream; tunnel closes; `mitm_connect` Warn (`outcome=inner_method_rejected`) |
| CONNECT to a port other than 443 | 400 on the CONNECT response; tunnel closes; `mitm_connect` Warn (`outcome=bad_port`) |
| CONNECT host fails the §5.1.2 effective allowlist | 403 on the CONNECT response; tunnel closes; `mitm_connect` Warn (`outcome=denied_host`) |
| Daemon shuts down during an in-flight CONNECT tunnel | The §9.4 sync.WaitGroup primitive holds Shutdown until the tunnel closes; on shutdown-context expiry, the conn is closed forcibly and the inner request's upstream fetch is cancelled |
| Clock skew: leaf cert `not_before` is in the future | Apt rejects with a `not yet valid` TLS error; cache's clock is the source of truth — operators should run NTP. No Phase 6 mitigation beyond logging on the cache side |
| CA expires mid-lifetime | All client TLS handshakes fail; `mitm_connect` Warn `outcome=tls_failed` rate spikes; Operator's `acu_mitm_ca_not_after_unixtime` alert (set to fire 30 days before expiry) catches this before the spike |

---

## 12. Test strategy (delta over SPEC §12 / SPEC2 §12 / SPEC3 §12 / SPEC4 §12 / SPEC5 §12)

### 12.1 Unit tests

In `internal/proxy/tlsmitm/`:

- CA generate-and-persist round trip: generate, write, reload,
  verify cert/key match, fingerprint stable.
- CA name-constraints translation: a regex with a known shape
  (`^([a-z]{2}\.)?archive\.ubuntu\.com$`) translates to the
  expected `permitted` list and excludes RFC 1918 / loopback.
- Leaf cert SAN list construction for a host with multiple
  Remap aliases.
- Cert cache LRU eviction order.
- Cert cache expired-entry refresh on lookup.
- Cert generation singleflight: 100 concurrent
  `Get(canonical_host)` calls produce one cert.
- Allowed-host regex: union with `upstream.allowed_host_regex`
  produces the expected predicate.

In `internal/proxy/`:

- CONNECT handler: bad port → 400.
- CONNECT handler: denied host → 403.
- CONNECT handler: non-GET inner request → 405 on inner stream.
- CONNECT handler: hijack-conn lifecycle (sync.WaitGroup
  increments on hijack, decrements on close).

### 12.2 Integration tests

Under `internal/proxy/` with a real net.Listen + apt-style
TCP client:

- End-to-end CONNECT + inner GET with a synthetic upstream
  (`httptest.NewTLSServer` configured with a cert the apt-
  side trusts via the test's CA pool). First request misses,
  second hits, both observe `X-Acu-Mitm: 1` on the response.
- Concurrent CONNECTs to the same host: one cert generation,
  shared cert.
- CONNECT to a host outside the allowlist: 403.
- CONNECT during shutdown: the conn drains within the
  shutdown deadline.

Under `e2e/`:

- A new `mitm_e2e_test.go` runs apt-get update against three
  HTTPS upstreams (apt.corretto.aws, packages.microsoft.com,
  one mirror behind real HTTPS). First pull warms the cache;
  second pull is verified to hit the cache (X-Cache: HIT on
  the inner response).

### 12.3 Chaos tests

- **CONNECT-during-CA-rotation.** Operator-supplied CA
  swapped on disk while a CONNECT is in mid-handshake. The
  daemon does NOT hot-reload the CA (Phase 6 §1.2 / Q7), so
  the in-flight handshake completes against the cached CA;
  any subsequent restart picks up the new CA. Verify no
  panic, no in-flight tunnels hang.
- **Cert-cache-full + thundering herd.** 1000 distinct
  hostnames concurrent CONNECT, cache size 256. Verify
  256 evictions land, 1000 certs issued total, no panic
  in the LRU primitive.
- **Inner GET cancelled mid-stream.** Client closes the TLS
  tunnel mid-response. Verify the upstream fetch context
  cancels, the cache write finalizes (or rolls back per
  the SPEC2 atomic-finalize contract), no orphan blob in
  `pool/`.
- **CA file deleted out from under a running daemon.** Next
  startup regenerates; running daemon's CA stays in memory;
  cert cache stays valid; new CONNECTs continue to issue
  leafs against the old CA until restart. Verify the
  in-memory CA is decoupled from disk after load.

---

## 13. Project layout (delta over SPEC §13 / SPEC4 §13 / SPEC5 §13)

```
cmd/apt-cacher-ultra/
  main.go                       # +CA load + tls_mitm config dump + ca/print-apt-conf subcommands
  ca_print.go                   # NEW: `ca print` subcommand
  print_apt_conf.go             # NEW: `--print-apt-conf` flag handler

internal/config/
  config.go                     # +TlsMitmConfig struct + validation

internal/proxy/
  connect.go                    # NEW: CONNECT method handler, inner-GET dispatch
  tlsmitm/
    ca.go                       # NEW: CA load / generate / persist
    leafcache.go                # NEW: cert cache (LRU) + singleflight
    leafgen.go                  # NEW: ECDSA / RSA cert generation
    nameconstraints.go          # NEW: regex → x509 NameConstraints translation

internal/handler/
  handler.go                    # +CONNECT method routing when MITM enabled
  parse.go                      # unchanged (HTTPS/// path remains)

internal/admin/
  status.go                     # +TLS MITM section in HTML + JSON

packaging/config/
  config.toml.default           # +commented [tls_mitm] block

packaging/
  postinst.sh                   # +chmod 0700 cache_dir/ca/
```

---

## 14. Subcommand surface (NEW)

### 14.1 `apt-cacher-ultra ca print`

Reads the CA cert (auto-generated or operator-supplied) and
writes the PEM-encoded cert to stdout. Exit codes:

- `0`: cert found and printed.
- `1`: MITM disabled in config (`tls_mitm.enabled = false`).
- `2`: cert path unreadable / cert fails to parse.
- `3`: config file unreadable.

Reads the same config file that the daemon would (defaults
to `/etc/apt-cacher-ultra/config.toml`, overridable with
`-config <path>`). Does NOT touch the database; safe to run
while the daemon is up.

### 14.2 `apt-cacher-ultra --print-apt-conf`

Prints a recommended apt-conf snippet to stdout for the
operator to drop into `/etc/apt/apt.conf.d/00aptcacher`.
Output:

```
# Generated by apt-cacher-ultra --print-apt-conf
Acquire::http::Proxy "http://<configured-listen-host>:<port>";
Acquire::https::Proxy "http://<configured-listen-host>:<port>";
# When MITM is enabled, the following CA must be installed
# system-wide (e.g. via update-ca-certificates) for HTTPS
# repositories in sources.list to validate:
# CA fingerprint (SHA-256): <hex>
# CA path on the cache host: <path>
```

The exact host/port reflect the loaded config. Exit codes:

- `0`: snippet printed.
- `1`: config file unreadable.

### 14.3 Subcommand routing

The daemon executable's argument parsing routes:
- No subcommand: run as daemon (existing behavior).
- `ca print`: §14.1.
- `--print-apt-conf`: §14.2.
- Anything else: existing flag handling.

---

## 15. Definition of done

Phase 6 is complete when:

1. Every test in §12.1 / §12.2 / §12.3 passes under
   `go test -race ./...`.
2. Every Phase 1–5 chaos test still passes (no regression on
   plain GET, freshness, GC, admin).
3. apt with `Acquire::https::Proxy "http://cache:3142"` and
   the CA installed system-wide successfully fetches the
   following on a cold cache:
   - `https://apt.corretto.aws/dists/stable/Release`
   - `https://packages.microsoft.com/repos/ms-edge/dists/stable/InRelease`
   - One large `.deb` (>10MB) from each upstream above.
4. The same `sources.list` against an apt-cacher-ng instance
   with its equivalent MITM config returns identical bytes
   for the same URLs (regression baseline).
5. Cache-hit-rate metric for HTTPS upstreams reaches ≥95% on
   the second pull from the same `sources.list`.
6. `apt-cacher-ultra ca print` produces a PEM that, when
   installed via `update-ca-certificates`, allows step 3 to
   succeed without `Acquire::https::Verify-Peer "false"` or
   any other TLS bypass.
7. Graceful shutdown drains in-flight CONNECT tunnels; no
   leaked goroutines, no orphan `pool/` blobs from
   cancelled inner GETs.
8. SPEC6.md as-built sweep, mirroring SPEC5.md's
   §13-style revision pass.
9. One-week production deployment with `tls_mitm.enabled =
   true` showing:
   - Stable CONNECT throughput, no cert-cache leaks
     (`acu_mitm_cert_cache_size` bounded).
   - No SPEC §10.4.1 `outcome=cache_write_failed` regression
     on HTTPS-fetched bytes.
   - The fleet's apt clients fetch HTTPS upstreams without
     manual `http://HTTPS///` rewrites.
