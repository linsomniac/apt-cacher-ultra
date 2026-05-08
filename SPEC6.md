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
a load-bearing trust anchor for the fleet. The cache enforces
the §5.1.2 effective allowlist at signing time, so a cache
running in steady state never issues certs for hostnames outside
the regex. **Key compromise is a separate concern**: a stolen CA
private key is unconstrained by the runtime regex unless the CA
*itself* carries cryptographic Name Constraints (RFC 5280
§4.2.1.10). The auto-generated CA path makes a best-effort
attempt to derive Name Constraints from the regex (§5.1.1); a
regex too complex to safely translate produces a CA without
constraints and a `mitm_ca_name_constraints_skipped` Warn at
startup. Operator-supplied CAs carry whatever constraints the
operator put on them — Phase 6 does not add or modify
constraints on operator-supplied CAs. Operators in adversarial
or multi-tenant environments who want cryptographic constraint
on the CA itself (i.e. limiting blast radius of a key
compromise) should either (a) narrow `tls_mitm.allowed_host_regex`
to literal hostnames the auto-generator can translate, or
(b) supply a CA out of band with the desired Name Constraints
already set. See §11 for the failure-mode catalog.

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

3. **Same hostname-allowlist posture as Phase 1, with deny-range
   enforcement deferred to inner-fetch TCP-connect time.** The
   §5.1.2 effective allowlist (`upstream.allowed_host_regex`
   intersected with the optional `tls_mitm.allowed_host_regex`)
   gates CONNECT targets pre-handshake — reject → 403, no cert
   generation, no TLS handshake. `upstream.deny_target_ranges`
   does NOT run at CONNECT time (that would force a DNS
   resolution at CONNECT, adding latency and a DNS-poisoning
   surface); it runs at the inner GET's TCP connect time exactly
   as it does for plain proxy GETs. The net effect is identical
   to Phase 1: an inbound request that would be rejected as a
   plain GET to an internal IP is also rejected here, just at
   the TCP connect attempt rather than at CONNECT receipt.
   **Implementers must NOT add a DNS lookup at CONNECT time** —
   the deny-range gate is correctly placed downstream.

4. **Same observability surface as Phase 1–5.** Every CONNECT
   produces a `mitm_connect` log line + `acu_mitm_connect_total`
   counter increment; the inner GET produces the existing
   `request` log line with a new `mitm=true` field and the
   normal `acu_requests_total{outcome=...}` increment. Operators
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

1. Parses the request-target into `(host, port)`. Lower-cases
   the host and applies IDNA "to-ASCII" normalization. Reject:
   - port other than `443` → `400 Bad Request` +
     `mitm_connect` Warn (`outcome=bad_port`).
   - host that parses as an IP literal (IPv4 dotted-quad,
     IPv4-mapped, or IPv6 in `[…]` form) → `400 Bad Request` +
     `mitm_connect` Warn (`outcome=ip_literal_host`). The
     allowlist is hostname-based, and the auto-generated CA's
     Name Constraints are dNSName-only; allowing an IP-literal
     CONNECT would let it pass the regex by coincidence and
     issue a leaf cert with no SAN that the client would
     accept. Operators with IP-literal HTTPS upstreams configure
     a hostname mapping in their DNS instead.
   The simpler default of "443 only" exists because non-443
   HTTPS upstreams are rare; a future override knob is deferred.
2. Validates the normalized `host` against the §5.1.2 effective
   allowlist (`upstream.allowed_host_regex` AND, when set, also
   `tls_mitm.allowed_host_regex`). Match on a regex is RE2
   `MatchString` against the lower-cased host (the regex
   author is responsible for adding `^…$` anchors if they
   want exact-match semantics — this matches Phase 1 §5.2
   `allowed_host_regex` semantics, which is unanchored
   substring match by default). Reject denied hosts with
   `403 Forbidden` + `mitm_connect` Warn
   (`outcome=denied_host`). The §5 SSRF gate
   `upstream.deny_target_ranges` is NOT enforced at this point
   — it fires at the inner GET's TCP connect time as it does
   for plain GETs. See §1.1.3 for the rationale.
3. Hijacks the underlying TCP connection (Go's
   `http.Hijacker`). Writes
   `HTTP/1.1 200 Connection established\r\n\r\n` to the raw
   conn before any TLS handshake.
4. Looks up the normalized literal `host` in the leaf-cert
   cache (cache key = the literal CONNECT host, NOT the
   Remap-canonical host — Remap rules are arbitrary RE2 regex
   and not enumerable, so a SAN list "every host that maps to
   this canonical" is undecidable). Miss → generates a leaf
   cert (§5.1.3) with a single SAN equal to the literal host.
   Cert generation is singleflighted per literal host:
   concurrent CONNECTs to the SAME host (case-insensitive)
   share one generation. CONNECTs to two different aliases
   of the same canonical host (e.g. `de.archive.ubuntu.com`
   and `archive.ubuntu.com`) get TWO cert cache entries —
   that is intentional and correct, the inner-GET cache
   still keys on canonical_host so the bytes are not
   double-cached.
5. Performs TLS handshake on the hijacked conn using the leaf
   cert. Handshake failure (client rejects cert, TLS version
   mismatch, etc.) closes the conn and emits `mitm_connect`
   Warn (`outcome=tls_failed`). The handshake is governed by
   the §5.4 client-facing TLS policy (TLS 1.2+, no fallback,
   no SSLv3/TLS 1.0/TLS 1.1).
6. Reads exactly one HTTP/1.1 request from the now-encrypted
   stream. The request method must be `GET` or `HEAD` (matching
   the proxy-listener method allowlist of SPEC §2.6); any
   other method returns `405 Method Not Allowed` on the inner
   stream (with `Allow: GET, HEAD`) and closes the tunnel
   with `mitm_connect` Warn (`outcome=inner_method_rejected`).
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

1. **Both `ca.crt` and `ca.key` are absent** (first start, or
   operator wiped the directory): generate a new CA pair
   (§5.1.1), write to disk atomically (§4.2.1), then load.
   Emit `mitm_ca_generated` Info plus `mitm_ca_loaded` Info.
2. **Both `ca.crt` and `ca.key` exist and parse**: load and use.
   Emit `mitm_ca_loaded` Info (`source=generated`,
   `fingerprint_sha256=…`, `not_after_unixtime=…`).
3. **Exactly one of the two files is present, OR either file
   exists but fails to parse / fails the cert-key match
   check**: startup fails with `mitm_ca_load_failed` Error
   naming the offending file. The daemon does NOT
   silently regenerate in this state — a half-present CA
   pair indicates either a partial-write crash (operator
   recovers by deleting both files to force regeneration)
   or operator error (an `apt-cacher-ultra ca print` run
   would have printed the now-missing cert; silent
   regeneration would change the trust anchor under every
   client without warning). Trust-root replacement is an
   operator-explicit action.
4. `cache_dir/ca/` cannot be created or written to: startup
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

#### 4.2.1 Atomic CA write semantics

Auto-generation must be atomic w.r.t. crashes mid-write —
a partially-written CA pair becomes the §4.2 step 3
half-present case and refuses to start, which is recoverable
but costs one operator intervention. The write sequence:

1. Create `<ca_storage_dir>` with mode `0700` if absent.
   `umask` is irrelevant — the daemon explicitly chmods.
2. Write `ca.crt.tmp` with mode `0600`. Call `Sync()` on
   the file before close.
3. Write `ca.key.tmp` with mode `0600`. Call `Sync()` on
   the file before close.
4. Rename `ca.crt.tmp → ca.crt`. Rename `ca.key.tmp →
   ca.key`. Both renames are atomic on POSIX.
5. Open the parent directory and call `Sync()` on it so the
   rename hits stable storage.

On any failure (mkdir, Write, Sync, rename), best-effort
clean up the tmp files and return the error. Startup fails;
the operator sees `mitm_ca_generation_failed` Error.

A clean run produces exactly two files, both fully written.
A crashed run produces zero or two .tmp files (which the
next startup ignores — only `ca.crt` and `ca.key` are
considered).

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
```

The inner-request method allowlist (`GET`, `HEAD`) is fixed by
SPEC §2.6 and not configurable. There is no
`require_inner_get_only` tunable.

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

The effective predicate evaluated on each CONNECT host is
the conjunction of two RE2 `MatchString` calls (Go regexp
semantics, lower-cased / IDNA-normalized host):

```
mitm_eligible(host) =
    upstream_allowed(host)
        AND (tls_mitm.allowed_host_regex == ""
             OR re2(tls_mitm.allowed_host_regex).MatchString(host))
```

`upstream_allowed(host)` is whatever predicate Phase 1 §5.2
defines for `upstream.allowed_host_regex` (verbatim — no
re-implementation in Phase 6). Empty
`tls_mitm.allowed_host_regex` means "inherit upstream" (the
common case); a non-empty value narrows further.

**No "broader than upstream" startup check.** A general
regex-subset relation is undecidable; a partial check that
worked only on simple regex shapes would mislead operators
into thinking it caught all misconfigurations. Instead, when
`tls_mitm.allowed_host_regex` is non-empty AND upstream is
also non-empty, the daemon emits a one-shot Info at startup
naming both regexes and flagging that the conjunction-not-
broader semantics is the operator's responsibility to verify.
A host that passes the MITM regex but fails the upstream
regex causes the CONNECT to succeed but the inner GET to
502 with `outcome=upstream_denied` — surfaceable via
`acu_requests_total{outcome="upstream_denied"}` alerting.

#### 5.1.3 Leaf cert parameters

Per-literal-host leaf certs are generated on first CONNECT
to a host:

- Algorithm: `tls_mitm.leaf_algorithm` (default ECDSA P-256;
  alternative `rsa2048` for pre-2018 client compatibility).
- Subject: `CN = <literal_host>` (the lower-cased,
  IDNA-normalized CONNECT target).
- Subject Alternative Names: a single dNSName entry equal
  to the literal host. The cert does NOT enumerate Remap
  aliases — Remap rules are arbitrary RE2 regex with
  unbounded preimage; an "every alias" SAN is undecidable
  in general and would mismatch any host the regex would
  match in principle but that has no Remap pre-image
  example. CONNECTs to two different aliases of the same
  canonical underlying host (e.g. `de.archive.ubuntu.com`
  and `archive.ubuntu.com`) produce two cache entries.
  This is intentional: cert validation runs against the
  client-presented SNI, which is the literal CONNECT
  target; the inner GET cache key is the canonical host
  per §3, so blob bytes are still not double-cached.
- Validity: `not_before = now - 5m`,
  `not_after = now + tls_mitm.leaf_cert_lifetime` (default 30
  days).
- Extensions:
  - `KeyUsage: digital_signature, key_encipherment`
  - `ExtendedKeyUsage: serverAuth`
- Signing: ECDSA-SHA256 against the CA's key, regardless of
  the leaf algorithm (the signing operation runs on the CA
  key; CA is ECDSA P-256).

The leaf cert cache is keyed on the literal lower-cased,
IDNA-normalized CONNECT host. Cache size = `cert_cache_size`
(default 256); LRU eviction.

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
- `tls_mitm_narrowing_regex_set` Info at startup when
  `tls_mitm.allowed_host_regex` is non-empty. Names both
  `upstream.allowed_host_regex` and the MITM regex,
  reminds the operator that the predicate is the conjunction
  (not "broader-of-the-two") and that the operator is
  responsible for the MITM regex being a subset of the
  upstream regex (regex subset is undecidable; the daemon
  cannot check it). A misconfigured broader-than-upstream
  MITM regex surfaces at runtime as an inner-GET 502
  with `outcome=upstream_denied`.

### 5.4 Client-facing and upstream TLS policy

**Client-facing TLS** (the cache acting as TLS server on the
hijacked CONNECT conn):

- Minimum protocol: TLS 1.2.
- Preferred protocol: TLS 1.3 when the client offers it.
- No TLS 1.1, TLS 1.0, or SSLv3 — clients still on those
  protocols get a `mitm_connect` Warn
  (`outcome=tls_failed`) and a closed conn.
- Cipher suite preference: Go's default
  `tls.CipherSuites()` returning suites recommended for new
  applications. No suite override knob in Phase 6.
- ALPN: HTTP/1.1 only. No HTTP/2 ALPN advertisement
  (Phase 6 does not implement HTTP/2 inside the tunnel —
  see §2.2 step 8).
- No client-cert auth (the daemon authenticates clients via
  the configured proxy listener's existing posture, e.g.
  network ACL or admin-port-only htpasswd; client TLS auth
  is a Phase 7+ topic).

**Upstream TLS** (the cache acting as TLS client to the
real HTTPS upstream during the inner GET's fetch path):

- Full certificate chain verification: REQUIRED.
  `tls.Config.InsecureSkipVerify` MUST be `false`. This is
  the existing Phase 1 fetcher posture and is unchanged in
  Phase 6.
- The system trust store is the source of trust roots.
  Phase 6 does NOT introduce per-upstream pinning, custom
  CA bundles, or `Acquire::https::CAInfo`-equivalent
  overrides.
- Hostname verification: REQUIRED, against the canonical
  upstream host (after Remap).
- Minimum protocol: TLS 1.2.
- HTTP-to-HTTPS or HTTPS-to-HTTP redirect handling: the
  existing Phase 1 fetcher does NOT follow redirects by
  default (each redirect is surfaced as a response and the
  caller decides) — Phase 6 inherits this. A redirect from
  `https://upstream/path` to `http://other/path` is treated
  as a normal upstream response; the cache does NOT
  silently downgrade the inner request to HTTP.

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

Concurrent CONNECTs to the same literal host issue one leaf
cert. The singleflight key is the literal lower-cased
IDNA-normalized host (matching the §5.1.3 cert cache key);
the value is `*tls.Certificate`. Subsequent CONNECTs that
miss the cache during a leader's generation block on the
leader, then read the cached entry. Generation latency is
dominated by ECDSA key generation (~5–10ms for P-256) +
signing (~1–2ms) — well under any reasonable handshake
timeout.

The singleflight does NOT collapse across literal-host
aliases (e.g. `de.archive.ubuntu.com` and
`archive.ubuntu.com` are separate keys). This is intentional
per §5.1.3: each literal host gets its own cert with the
literal host in the SAN.

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
  `ip_literal_host` / `tls_handshake_timeout` / `tls_failed`
  / `cert_gen_failed` / `inner_method_rejected`),
  `duration_ms`, `cert_cache` (`hit` / `miss`).
  Emitted at level Info on `tunneled`; Warn on every other
  outcome.

- **`mitm_cert_issued`** — once per cert generation, before
  insertion into the cache. Fields: `host` (the literal
  CONNECT host — matches the cert cache key), `algorithm`,
  `lifetime_seconds`, `gen_duration_ms`. Level Debug
  (cert generation is routine; only operators debugging
  cert issuance care). The cert SAN is always exactly the
  literal host (§5.1.3), so no `san_count` field is
  needed — implied to be 1.

- **`mitm_cert_cache_evicted`** — once per eviction.
  Fields: `host`, `reason` (`lru` / `expired`),
  `age_seconds`. Level Info.

- **`mitm_ca_loaded`** — once at startup. Fields: `source`
  (`generated` / `supplied`), `fingerprint_sha256`,
  `not_after_unixtime`, `name_constraints` (bool — true if
  the CA carries Name Constraints). Level Info.

- **`mitm_ca_generated`** — once when auto-generation runs.
  Fields: `path` (the `cache_dir/ca/` location written),
  `algorithm`, `lifetime_seconds`. Level Info. Operators
  scanning the journal for first-boot cues see this exactly
  once per `cache_dir/ca/` lifecycle.

- **`mitm_ca_load_failed`** — at startup, when the §4.2
  CA materialization step 3 fires (half-present pair, parse
  failure, cert/key mismatch). Fields: `path`, `err`. Level
  Error. The daemon does not bind after this event.

- **`mitm_ca_generation_failed`** — at startup, when atomic
  CA write fails (mkdir, Write, Sync, rename, fsync).
  Fields: `path`, `err`. Level Error. The daemon does not
  bind after this event.

- **`mitm_ca_name_constraints_skipped`** — at startup, when
  the regex-to-NameConstraints translation cannot safely
  produce constraints. Fields: `regex`, `reason`. Level
  Warn. The CA is generated without Name Constraints; runtime
  signing is still gated by §5.1.2 but a stolen CA key is
  cryptographically unconstrained (§ trust-anchor expansion
  note).

- **`mitm_clock_skew`** — when a freshly-generated leaf
  cert's `not_before` is in the future relative to the
  cache's wall clock at the moment of issuance. (Should be
  impossible given the §5.1.3 5m tolerance applied at
  generation time, but the check exists as belt-and-
  suspenders for a system-clock jump mid-process.) Fields:
  `host`, `not_before`, `now`. Level Warn.

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

The §10.4.1 request outcome enum on `acu_requests_total` is
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
| Last cert issued | `<host> @ <UTC timestamp>` (literal CONNECT host; empty on no issuance yet) |
| Cert hit rate (60s window) | `<percentage>` (cheap rolling counter, recomputed by the §9.7.6 refresher) |

When `tls_mitm.enabled = false`, the section is omitted.

The JSON form of the status page (§10.5 schema) carries a
top-level `tls_mitm` key — always present, abbreviated to
`{"enabled": false}` when MITM is disabled, full payload
otherwise. This mirrors the Phase 5 SPEC §10.5 invariant
that top-level keys are stable.

---

## 11. Failure-mode catalog (delta over SPEC §11 / SPEC4 §11 / SPEC5 §11)

| # | Failure | Behavior |
|---|---------|----------|
| F1 | `tls_mitm.enabled = true` but operator-supplied CA file unreadable at startup | Startup fails with config-error log; daemon does not bind |
| F2 | Auto-generated CA exists with one of `ca.crt` / `ca.key` missing OR either fails to parse OR the cert/key pair fails the match check | Startup logs `mitm_ca_load_failed` Error naming the offending file; daemon does not bind. Operator recovers by deleting BOTH files to force regeneration (silent regeneration would change the trust anchor under every client; §4.2 step 3) |
| F3 | `cache_dir/ca/` cannot be created (perms, disk full) | Startup fails with `mitm_ca_generation_failed` Error; daemon does not bind. The §4.2.1 atomic write semantics guarantee no partial files are left on a generation failure mid-run |
| F4 | CA cert / key mismatch (operator-supplied) | Startup fails with config-error log naming the mismatch. The match check is `x509.Certificate.PublicKey == PrivateKey.Public()` |
| F5 | Operator-supplied CA cert `not_after` already in the past, OR not in the future at all | Startup fails with config-error log naming the expiry timestamp |
| F6 | Operator-supplied CA cert lacks `BasicConstraints: CA:TRUE` | Startup fails with config-error log |
| F7 | Leaf cert generation fails (entropy exhaustion, key gen panics, etc.) | CONNECT closes with `mitm_connect` Warn (`outcome=cert_gen_failed`); the singleflight returns the error to all blocked waiters; the next CONNECT on the same host retries (the singleflight does NOT cache failures) |
| F8 | Cert cache full + new host requested | LRU eviction; `mitm_cert_cache_evicted` Info on the evicted entry; new cert inserted; `acu_mitm_cert_evicted_total{reason="lru"}` increments |
| F9 | TLS handshake on hijacked conn fails (client distrusts CA, TLS-version mismatch, cipher mismatch) | Tunnel closes with `mitm_connect` Warn (`outcome=tls_failed`); apt logs a TLS verification error; `tls_mitm_enabled_ca_undistributed` Warn fires from the refresher when this is the steady state |
| F10 | TLS handshake exceeds the §9.2 budget | Conn closed with `mitm_connect` Warn (`outcome=tls_handshake_timeout`) |
| F11 | Inner request method is not GET/HEAD | 405 written on the inner stream with `Allow: GET, HEAD`; tunnel closes; `mitm_connect` Warn (`outcome=inner_method_rejected`) |
| F12 | CONNECT to a port other than 443 | 400 on the CONNECT response; tunnel closes; `mitm_connect` Warn (`outcome=bad_port`) |
| F13 | CONNECT to an IP-literal host (IPv4 or IPv6) | 400 on the CONNECT response; tunnel closes; `mitm_connect` Warn (`outcome=ip_literal_host`) |
| F14 | CONNECT host fails the §5.1.2 effective allowlist | 403 on the CONNECT response; tunnel closes; `mitm_connect` Warn (`outcome=denied_host`) |
| F15 | Inner GET upstream fetch fails the SSRF deny-range gate at TCP-connect time | The inner GET response is whatever the existing Phase 1 fetcher returns (typically 502 with `outcome=upstream_denied` on the request log line); tunnel closes after inner response. The CONNECT itself succeeded (as designed; §1.1.3) |
| F16 | Daemon shuts down during an in-flight CONNECT tunnel | The §9.4 sync.WaitGroup primitive holds Shutdown until the tunnel closes; on shutdown-context expiry, the conn is closed forcibly and the inner request's upstream fetch is cancelled |
| F17 | Clock skew: leaf cert `not_before` is in the future | Apt rejects with a `not yet valid` TLS error; cache's clock is the source of truth — operators should run NTP. No Phase 6 mitigation beyond logging on the cache side |
| F18 | CA expires mid-lifetime | All client TLS handshakes fail; `mitm_connect` Warn `outcome=tls_failed` rate spikes; operator's `acu_mitm_ca_not_after_unixtime` alert (set to fire 30 days before expiry) catches this before the spike |
| F19 | CA private key file ownership / mode wrong on disk (e.g. world-readable) | Phase 6 does NOT enforce mode/ownership at startup beyond the §4.2.1 atomic-write guarantees on the auto-generated path. Operator-supplied CA paths are read with whatever mode the operator chose; `apt-cacher-ultra ca print` is the audit primitive (it warns on `S_IROTH` set on `ca.key`). A future phase can add mandatory mode enforcement if any deployment asks |
| F20 | Leaf cert algorithm config invalid at startup | Startup fails with config-error log naming the rejected value |
| F21 | Upstream HTTPS server presents an invalid cert (chain failure, expired, hostname mismatch) | Inner GET fetch fails with the existing Phase 1 fetcher behavior — `outcome=bad_gateway` on the inner request log line; the cache does NOT relax verification (§5.4) |
| F22 | Upstream sends a redirect from `https://` to `http://` | The redirect is surfaced as a normal upstream response (Phase 1 fetcher does not auto-follow redirects); the cache does NOT silently downgrade. Apt sees the 30x and decides what to do |

---

## 12. Test strategy (delta over SPEC §12 / SPEC2 §12 / SPEC3 §12 / SPEC4 §12 / SPEC5 §12)

### 12.1 Unit tests

In `internal/proxy/tlsmitm/`:

- CA generate-and-persist round trip: generate, write,
  reload, verify cert/key match, fingerprint stable
  (covers F1 success path).
- CA atomic-write semantics: simulate disk-full mid-write,
  verify zero or two .tmp files, NEVER half-renamed
  ca.crt / ca.key (covers F3).
- CA name-constraints translation: a regex with a known
  shape (`^([a-z]{2}\.)?archive\.ubuntu\.com$`) translates
  to the expected `permitted` dNSName list. A regex with a
  shape that cannot be safely translated (alternation
  spanning multiple TLDs, character-class with negated
  ranges) yields no constraints + the
  `mitm_ca_name_constraints_skipped` Warn.
- Leaf cert: literal-host single-SAN construction (no
  alias enumeration). For
  `de.archive.ubuntu.com`, the issued cert has SAN =
  `de.archive.ubuntu.com` only. For a re-CONNECT to
  `archive.ubuntu.com`, a SECOND cert is issued with SAN =
  `archive.ubuntu.com` only.
- Cert cache LRU eviction order.
- Cert cache expired-entry refresh on lookup.
- Cert generation singleflight: 100 concurrent
  `Get(literal_host)` calls produce one cert. The
  singleflight does NOT collapse across literal-host
  aliases.
- Cert generation singleflight failure path: a fake
  generator that returns an error returns the error to
  ALL waiters; the next call retries (no failure caching)
  (covers F7).
- Effective allowlist predicate: each combination of
  upstream-empty / upstream-set, mitm-empty / mitm-set
  produces the documented conjunction (§5.1.2).
- Operator-supplied CA validation: cert-key mismatch (F4),
  past not_after (F5), missing CA:TRUE (F6), unreadable
  file (F1) each fail at config validation with the right
  error.
- Leaf algorithm config: `"ecdsa-p256"` and `"rsa2048"`
  accepted; everything else rejected (F20).

In `internal/proxy/`:

- CONNECT handler: bad port → 400 (F12).
- CONNECT handler: IP-literal host → 400 (F13).
- CONNECT handler: denied host → 403 (F14).
- CONNECT handler: non-GET / non-HEAD inner request → 405
  on inner stream (F11).
- CONNECT handler: HEAD inner request → reaches the inner
  pipeline (HEAD is allowed, matching SPEC §2.6).
- CONNECT handler: TLS handshake timeout (F10).
- CONNECT handler: hijack-conn lifecycle (sync.WaitGroup
  increments on hijack, decrements on close).

### 12.2 Integration tests

Under `internal/proxy/` with a real net.Listen + apt-style
TCP client:

- End-to-end CONNECT + inner GET with a synthetic upstream
  (`httptest.NewTLSServer` configured with a cert the
  apt-side trusts via the test's CA pool). First request
  misses, second hits, both observe `X-Acu-Mitm: 1` on
  the response.
- Concurrent CONNECTs to the same host: one cert generation,
  shared cert.
- CONNECT to a host outside the allowlist: 403 (F14
  end-to-end).
- CONNECT during shutdown: the conn drains within the
  shutdown deadline (F16 end-to-end).
- CONNECT with `cache.listen_tls` (apt-over-TLS-to-cache):
  the outer TLS terminates at the proxy listener, the
  inner CONNECT then proceeds normally — verifies the
  CONNECT handler works on both the plain and TLS-to-cache
  bind paths.
- Disabled mode: `tls_mitm.enabled = false` → CONNECT
  returns 405 with `Allow: GET, HEAD` exactly as
  pre-Phase-6.
- Status JSON shape: `tls_mitm.enabled = false` produces
  `"tls_mitm": {"enabled": false}` exactly; `enabled =
  true` produces the full §10.4 payload with all fields
  present.
- TLS policy: handshake with TLS 1.0 / TLS 1.1 client
  fails (F9 specialization); upstream fetcher refuses an
  invalid upstream cert (F21 specialization).
- HTTPS-to-HTTP redirect from upstream: surfaces as a 30x
  on the inner response, NOT auto-followed (F22).
- `acu_requests_total{outcome=upstream_denied}`
  increments when a denied-target-range deny fires inside
  the inner GET (F15 end-to-end).

Under `e2e/`:

- A new `mitm_e2e_test.go` runs apt-get update against
  three HTTPS upstreams (apt.corretto.aws,
  packages.microsoft.com, one mirror behind real HTTPS).
  First pull warms the cache; second pull is verified to
  hit the cache (`X-Cache: HIT` on the inner response).

### 12.3 Chaos tests

- **CONNECT-during-CA-rotation.** Operator-supplied CA
  swapped on disk while a CONNECT is in mid-handshake.
  The daemon does NOT hot-reload the CA (Phase 6 §1.2 /
  Q7), so the in-flight handshake completes against the
  cached CA; any subsequent restart picks up the new CA.
  Verify no panic, no in-flight tunnels hang.
- **Cert-cache-full + thundering herd.** 1000 distinct
  hostnames concurrent CONNECT, cache size 256. Verify
  256 evictions land, 1000 certs issued total, no panic
  in the LRU primitive (F8 end-to-end).
- **Inner GET cancelled mid-stream.** Client closes the
  TLS tunnel mid-response. Verify the upstream fetch
  context cancels, the cache write finalizes (or rolls
  back per the SPEC2 atomic-finalize contract), no orphan
  blob in `pool/`.
- **CA file deleted out from under a running daemon.**
  Running daemon's CA stays in memory; cert cache stays
  valid; new CONNECTs continue to issue leafs against the
  old CA until restart. The next startup hits §4.2 step 3
  (half-present recovery / regeneration as appropriate
  given which file was deleted) and the operator
  intervention path is exercised (F2 end-to-end).
- **CA generation crash mid-write.** Kill -9 the daemon
  during the write phase (e.g. between the two `.tmp`
  writes). Restart. Verify either zero `.tmp` files
  (clean state) or two `.tmp` files (next gen ignores
  them); NEVER a half-renamed `ca.crt` / `ca.key` pair
  that would trigger §4.2 step 3 (F3 end-to-end).
- **CA expiry mid-runtime.** Set the CA `not_after` to
  60 seconds out, run a CONNECT every 10 seconds, verify
  successful handshakes until the moment of expiry, then
  every CONNECT after fails with `outcome=tls_failed`.
  Operator's `acu_mitm_ca_not_after_unixtime` alert
  fires on the gauge crossing the threshold (F18
  end-to-end).

### 12.4 §11 failure-mode coverage matrix

Every row in the §11 failure-mode catalog must be exercised
by at least one test in §12.1, §12.2, or §12.3:

| F# | §12 location |
|----|--------------|
| F1 | 12.1 unit (operator-supplied CA validation, unreadable file) |
| F2 | 12.1 unit (CA validation suite) + 12.3 chaos (CA file deletion) |
| F3 | 12.1 unit (atomic-write disk-full sim) + 12.3 chaos (kill mid-write) |
| F4 | 12.1 unit (cert-key mismatch) |
| F5 | 12.1 unit (past not_after) |
| F6 | 12.1 unit (missing CA:TRUE) |
| F7 | 12.1 unit (singleflight failure path) |
| F8 | 12.3 chaos (cert-cache thundering herd) |
| F9 | 12.2 integration (TLS policy / version mismatch) |
| F10 | 12.1 unit (CONNECT handler handshake timeout) |
| F11 | 12.1 unit (CONNECT handler non-GET/HEAD inner) |
| F12 | 12.1 unit (CONNECT handler bad port) |
| F13 | 12.1 unit (CONNECT handler IP-literal) |
| F14 | 12.1 unit + 12.2 integration (denied host) |
| F15 | 12.2 integration (deny-range fires on inner GET) |
| F16 | 12.2 integration (CONNECT during shutdown) |
| F17 | 12.1 unit (clock-skew leaf cert; verifies the cache emits `mitm_clock_skew` Warn — apt-side rejection is observable but not unit-testable here) |
| F18 | 12.3 chaos (CA expiry mid-runtime) |
| F19 | 12.1 unit (`apt-cacher-ultra ca print` audit warning on 0644 ca.key) |
| F20 | 12.1 unit (leaf algorithm rejection) |
| F21 | 12.2 integration (upstream invalid cert) |
| F22 | 12.2 integration (HTTPS→HTTP redirect not auto-followed) |

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

Writes the PEM-encoded CA cert to stdout. Loads the same
config file the daemon would (defaults to
`/etc/apt-cacher-ultra/config.toml`, overridable with
`-config <path>`). Behavior:

1. **Operator-supplied CA** (`tls_mitm.ca_cert` non-empty):
   reads from that path. Cert must exist and parse.
2. **Auto-generated CA, daemon already started before**
   (`<cache_dir>/ca/ca.crt` exists and parses): reads from
   that path.
3. **Auto-generated CA, daemon not yet started** (no
   `<cache_dir>/ca/ca.crt`): runs the §4.2.1 atomic
   generate+persist path EXACTLY as the daemon would,
   then prints. After this run, the daemon's first start
   will load the now-existing CA via §4.2 step 2 (not
   step 1) — there is one CA, generated by `ca print`,
   used by both subsequent ansible distribution and the
   eventual daemon process. This makes `ca print` safe to
   call as the FIRST step of fleet rollout, before the
   daemon has ever started.

Does NOT open the SQLite database; safe to run while the
daemon is running. Will skip the §4.2.1 generate path if
the daemon is concurrently running and has already
materialized the CA — race-free because the §4.2.1
atomic-rename guarantees the cert appears as a complete
file or not at all.

Audit warning: if `<ca_key>` exists with mode bits other
than `0600` (e.g. `0644` world-readable), prints a stderr
Warn naming the mode and recommending `chmod 0600` —
non-fatal (exit code unchanged), since operators may have
legitimate ownership-and-mode strategies.

Exit codes:

- `0`: cert printed.
- `1`: MITM disabled in config (`tls_mitm.enabled = false`).
- `2`: cert path unreadable / cert fails to parse / atomic
  generation fails.
- `3`: config file unreadable.

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

Phase 6 is complete when EACH of the following holds:

**Test coverage**

1. Every test in §12.1 / §12.2 / §12.3 passes under
   `go test -race ./...`.
2. Every row in the §11 failure-mode catalog has at least
   one passing test, per the §12.4 coverage matrix.
3. Every Phase 1–5 chaos test still passes (no regression
   on plain GET, freshness, GC, admin paths).

**Disabled-mode parity**

4. With `tls_mitm.enabled = false` (default), the daemon's
   externally-observable behavior is byte-identical to a
   Phase 5 daemon: CONNECT returns 405 with
   `Allow: GET, HEAD`; status JSON has
   `"tls_mitm": {"enabled": false}`; no `mitm_*` log lines
   emitted; no `acu_mitm_*` metrics increment.

**Config validation**

5. Every §5.2 validation rule fires on the documented
   invalid-input class and the daemon refuses to start with
   the named log line. Tested as part of §12.1.

**CA paths**

6. The auto-generated CA path produces a valid CA file
   pair under `<cache_dir>/ca/`, mode 0600, atomic on
   disk-full or kill-mid-write (§12.3 chaos).
7. The operator-supplied CA path validates per §5.2 and
   loads on startup; mismatched / expired / non-CA inputs
   fail validation (§12.1).
8. `apt-cacher-ultra ca print` produces a PEM that, when
   installed via `update-ca-certificates`, allows step 11
   below to succeed without `Acquire::https::Verify-Peer
   "false"` or any other TLS bypass.
9. `apt-cacher-ultra ca print` is idempotent: running twice
   produces the same fingerprint when the CA already exists,
   and runs the §4.2.1 generate path exactly once when no
   CA exists yet.

**Observability**

10. Every `mitm_*` log event in §10.2 is reachable by an
    integration test — the test asserts the log line was
    emitted with the documented field set (no extra fields,
    no missing fields).
11. Every `acu_mitm_*` metric in §10.3 increments at least
    once during the §12.2 integration suite.
12. Status page (HTML + JSON) renders the §10.4 TLS MITM
    section when MITM is enabled, exact field set asserted
    by §12.2.

**Live exercise**

13. apt with `Acquire::https::Proxy "http://cache:3142"`
    and the CA installed system-wide successfully fetches
    the following on a cold cache:
    - `https://apt.corretto.aws/dists/stable/Release`
    - `https://packages.microsoft.com/repos/ms-edge/dists/stable/InRelease`
    - One large `.deb` (>10MB) from each upstream above.
14. The same `sources.list` against an apt-cacher-ng
    instance with its equivalent MITM config returns
    identical bytes for the same URLs (regression baseline
    against the upstream we're replacing).
15. Cache-hit-rate metric for HTTPS upstreams reaches ≥95%
    on the second pull from the same `sources.list`.

**Shutdown**

16. Graceful shutdown drains in-flight CONNECT tunnels; no
    leaked goroutines, no orphan `pool/` blobs from
    cancelled inner GETs.

**Documentation**

17. SPEC6.md as-built sweep, mirroring SPEC5.md's
    §13-style revision pass — every §11 failure-mode row
    matches the implemented behavior, every config field
    matches the implemented validation, every metric name
    matches the registered metric.

**Production soak**

18. One-week production deployment with
    `tls_mitm.enabled = true` showing:
    - Stable CONNECT throughput, no cert-cache leaks
      (`acu_mitm_cert_cache_size` bounded by configured
      cap; observed in metrics).
    - No `outcome=cache_write_failed` regression on
      HTTPS-fetched bytes vs. the prior `http://HTTPS///`
      baseline.
    - The fleet's apt clients fetch HTTPS upstreams
      without manual `sources.list` rewrites.
    - `acu_mitm_ca_not_after_unixtime` Prometheus alert
      configured (fires 30 days before expiry); observed
      at expected value, not yet firing.
