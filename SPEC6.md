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

3. **Same hostname-allowlist posture as Phase 1, split into a
   cert-signing gate and a fetch gate.** The CONNECT pre-handshake
   gate is the conjunction of two predicates (§5.1.2):
   - `tls_mitm.allowed_host_regex` (when set) is the **signing
     predicate**, evaluated against the **literal CONNECT host**
     (lower-cased + IDNA-normalized). Operators use this to bound
     which literal hostnames the cache is willing to MITM at all.
   - `upstream.allowed_host_regex` is the **fetch predicate**,
     evaluated against the **canonical host** (the same post-Remap
     hostname Phase 1 §6.6 / `internal/handler/handler.go:274`
     uses against `req.CanonicalHost`). The CONNECT step computes
     the canonical host by running the literal CONNECT host
     through Remap (`internal/proxy/proxy.go canonicalize`) — pure
     regex, no DNS, no I/O.

   Both predicates are checked pre-handshake; if either fails →
   403, no cert generation, no TLS handshake. This guarantees
   "no cert is issued unless the literal host passes the MITM
   signing predicate AND the canonical host passes the upstream
   fetch allowlist". `upstream.deny_target_ranges` does NOT run
   at CONNECT time (that would force a DNS resolution at CONNECT,
   adding latency and a DNS-poisoning surface); it runs at the
   inner GET's TCP connect time exactly as it does for plain
   proxy GETs. The net effect is identical to Phase 1: an inbound
   request that would be rejected as a plain GET to an internal
   IP is also rejected here, just at the TCP connect attempt
   rather than at CONNECT receipt.
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
   allowlist. The two predicates are evaluated in order; the
   first failure short-circuits with the appropriate
   `denied_gate` field on the `mitm_connect` Warn:
   1. **Signing predicate** — when `tls_mitm.allowed_host_regex`
      is non-empty, RE2 `MatchString` against the literal
      lower-cased host. Failure → `403 Forbidden` +
      `mitm_connect` Warn (`outcome=denied_host`,
      `denied_gate=signing`). No Remap is performed; no cert
      generation; no TLS handshake. (Empty regex →
      predicate vacuously true; fall through to the fetch
      predicate.)
   2. **Fetch predicate** — RE2 `MatchString` of
      `upstream.allowed_host_regex` against the **canonical
      host**, where canonical = result of running the literal
      CONNECT host through Remap (`internal/proxy/proxy.go`
      `canonicalize`). Remap is regex-only, so this introduces
      no DNS or I/O at CONNECT time. Failure → `403 Forbidden`
      + `mitm_connect` Warn (`outcome=denied_host`,
      `denied_gate=fetch`); no cert generation, no TLS
      handshake.

   Both must hold for the CONNECT to proceed.
   Match on a regex is unanchored RE2 substring match (Phase 1
   §5.2 semantics — the regex author adds `^…$` if they want
   exact match). The §5 SSRF gate `upstream.deny_target_ranges`
   is NOT enforced at this point — it fires at the inner GET's
   TCP connect time as it does for plain GETs. See §1.1.3 for
   the rationale.
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
7. Constructs a synthetic `*http.Request` and dispatches into
   the existing handler pipeline (`Handler.ServeHTTP` —
   `internal/handler/handler.go:230`). The handler entry point
   parses `r.RequestURI` via `parser.Parse` (handler.go:252),
   ignoring `r.URL`; the synthetic request must therefore set
   the absolute-URI form on `RequestURI`:
   - `RequestURI`: `"https://<literal_host><inner_path>"` —
     scheme=https, host = the literal lower-cased
     IDNA-normalized CONNECT host, path = the inner request's
     request-URI path. The query string and fragment from the
     inner request are dropped; apt repository URLs never
     carry them, and the parser
     (`internal/proxy/url.go parseRequestURI`) rejects both
     classes by design. An inner GET that does carry a query
     string returns 400 on the inner stream (handler-side
     `bad_request` outcome) — same shape as a malformed plain
     GET would produce.
   - `Method`: from the inner request line (already
     constrained to GET or HEAD by step 6).
   - `Host`: same literal host as in `RequestURI`. The parser
     accepts but ignores `r.Host` (handler.go uses it only as
     a logging breadcrumb), so this is symmetry-only.
   - `RemoteAddr`: the outer CONNECT's client address.
   - `Body`: `http.NoBody` for both GET and HEAD.
   - `Context`: derived from the outer CONNECT context, so
     listener Shutdown cancels the inner upstream fetch.
   - `Header`: a copy of the inner request's headers minus
     hop-by-hop entries (`Connection`, `Keep-Alive`, etc.),
     with `Host` set to the literal host (defensive — apt
     normally sends a `Host` matching the CONNECT target, but
     a divergent value should not affect cache identity).

   **The synthetic request enters the same code path as a
   plain proxy GET** — Parse → Remap → SSRF gate → cache
   lookup → upstream fetch — and the inner response is
   written back through the encrypted stream by the same
   ResponseWriter wrapping (§6.3).

   No new handler entry point is introduced. The minimal
   contract Phase 6 needs from the existing handler is:
   "given a `*http.Request` whose `RequestURI` is an
   absolute-URI of scheme `https`, treat it as a proxy-mode
   request"; the existing parser already does this
   (proxy.go:124-148, ModeProxy branch).
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
the `HTTPS///` prefix in `internal/proxy/proxy.go`
(`isHTTPSMagic` / `splitHTTPSMagic`) and treats the request
as `canonical_scheme = "https"`. Bytes fetched via this path
and via MITM share the same canonical key; a request via
either path benefits from a hit populated by the other.

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

See SPEC §3. Phase 6 reuses the unchanged Remap pipeline in
two places, both pure regex / no I/O:

1. The CONNECT step computes `canonical_host = canonicalize(literal_host)`
   for the §5.1.2 fetch gate, before issuing a cert and before
   handshake.
2. The inner GET enters the existing Phase 1 pipeline, where
   the parser canonicalizes the synthetic request-URI
   identically to a plain proxy GET — same Remap rules apply,
   same canonical key.

The leaf cert SAN list is **NOT** built from Remap output.
SAN = the literal CONNECT host only (§5.1.3), since Remap
rules are arbitrary RE2 regex with unbounded preimage and a
"every alias that maps to this canonical" SAN list is
undecidable. CONNECTs to two literal aliases of the same
canonical host produce two separate cert cache entries; the
inner-GET cache key (§3.2 unchanged) keys on canonical host
so blob bytes are not double-cached.

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
    ca.ready                               # 0600, commit marker (§4.2.1)
```

When `tls_mitm.ca_cert` and `tls_mitm.ca_key` are set
(operator-supplied path), Phase 6 reads from those paths
directly and does not write to `cache_dir/ca/`. The
`tls_mitm.ca_storage_dir` config tunable overrides the
default `cache_dir/ca/` location for the auto-generated path.

### 4.2 Startup CA materialization

When `tls_mitm.enabled = true` and no operator-supplied CA
path is set, daemon startup checks `cache_dir/ca/`. The
**`ca.ready` marker file is the all-or-nothing commit
primitive** — it contains the hex-encoded SHA-256 fingerprint
of `ca.crt`, written last during atomic generation (§4.2.1)
under interprocess lock (§4.2.2). The daemon recognizes a
generated CA as committed only when `ca.ready` exists AND
matches `sha256(ca.crt)`. Cases:

1. **`cache_dir/ca/` is empty** (first start, or operator
   wiped the directory): generate a new CA pair (§5.1.1),
   write to disk atomically (§4.2.1), then load. Emit
   `mitm_ca_generated` Info plus `mitm_ca_loaded` Info.
2. **`ca.crt`, `ca.key`, and `ca.ready` all exist; both
   files parse; `ca.ready`'s fingerprint matches
   `sha256(ca.crt)`; the cert/key match check passes**:
   load and use. Emit `mitm_ca_loaded` Info
   (`source=generated`, `fingerprint_sha256=…`,
   `not_after_unixtime=…`).
3. **Any other state of `cache_dir/ca/`** — `ca.ready`
   missing while any of `ca.crt` / `ca.key` / `ca.crt.tmp` /
   `ca.key.tmp` / `ca.ready.tmp` is present; `ca.ready`'s
   fingerprint doesn't match `sha256(ca.crt)`; either file
   fails to parse; cert/key mismatch — startup fails with
   `mitm_ca_load_failed` Error naming the offending state.
   The daemon does NOT silently regenerate in this state.
   Operator recovery is **explicit removal of the entire
   `cache_dir/ca/` directory** (or just the daemon-managed
   files within it), forcing case 1 on the next start. The
   marker scheme guarantees this branch never fires
   spuriously from a §4.2.1 mid-write crash, since
   `ca.ready` is written last and atomically: a kill -9
   anywhere during write leaves `ca.ready` absent and the
   on-disk state observably uninitialized to case 3.
   Silent regeneration would change the trust anchor under
   every client without warning; trust-root replacement is
   an operator-explicit action.
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

Auto-generation must produce a CA on disk atomically w.r.t.
crashes mid-write — meaning the §4.2 case-2 "load" branch
fires only when **all three of `ca.crt`, `ca.key`, `ca.ready`
are committed and self-consistent**. Any other on-disk state
is observably "uninitialized" to startup and produces a
case-3 `mitm_ca_load_failed` Error. The marker file
`ca.ready` is the commit primitive: it is written last, with
the SHA-256 fingerprint of the just-renamed `ca.crt` as its
content. The §12.3 chaos test "kill mid-write" is satisfied
by construction: ca.crt + ca.key alone (no ca.ready) is the
documented uninitialized state, not the trusted state.

The write sequence:

1. Acquire the §4.2.2 interprocess lock on
   `<ca_storage_dir>/.ca.lock`. Block (with timeout) until
   the lock is held; release on completion or failure.
2. Re-check the directory state under lock. If `ca.ready`
   now exists with a valid fingerprint matching `ca.crt`,
   abort the generate and load the existing CA (another
   process — typically `ca print` — completed the write
   while we were waiting on the lock). This is the
   compare-and-adopt path.
3. Best-effort cleanup of any stale `*.tmp` files left from
   prior crashed runs.
4. Create `<ca_storage_dir>` with mode `0700` if absent.
   `umask` is irrelevant — the daemon explicitly chmods.
5. Write `ca.crt.tmp` with mode `0600`. Call `Sync()` on
   the file before close.
6. Write `ca.key.tmp` with mode `0600`. Call `Sync()` on
   the file before close.
7. Rename `ca.crt.tmp → ca.crt`. Rename `ca.key.tmp →
   ca.key`. Both renames are atomic on POSIX.
8. fsync the parent directory so the renames hit stable
   storage before the marker is written.
9. Compute `sha256(ca.crt)` (re-read from disk to avoid
   trusting an in-memory copy that disk corruption could
   diverge from). Write `ca.ready.tmp` with mode `0600`
   containing the lower-cased hex fingerprint plus a
   trailing newline; Sync; close.
10. Rename `ca.ready.tmp → ca.ready`. fsync the parent
    directory.
11. Release the lock.

On any failure (mkdir, Write, Sync, rename, lock), best-
effort clean up the `*.tmp` files, release the lock, and
return the error. Startup fails; the operator sees
`mitm_ca_generation_failed` Error.

**Possible on-disk states after a kill -9 mid-write:**

| State | Meaning | §4.2 disposition |
|-------|---------|------------------|
| Empty directory (or no daemon-managed files) | First boot, or pre-step-5 crash | Case 1 — generate |
| `*.tmp` files only | Crashed between steps 5–6 | Case 3 — operator clears, regen on next start |
| `ca.crt` only, no key, no marker | Crashed between rename of crt and key | Case 3 — operator clears |
| `ca.crt` + `ca.key`, no marker | Crashed between rename of key and marker write | Case 3 — operator clears (the trust anchor is NOT yet committed; loading would change it on next start) |
| `ca.crt` + `ca.key` + `ca.ready` (matching fp) | Clean state | Case 2 — load |
| `ca.crt` + `ca.key` + `ca.ready` (mismatching fp) | Manual tampering or partial restoration | Case 3 — operator investigates |

The marker scheme converts what was the worst case (silent
half-renamed trust-anchor adoption) into a deterministic
load-or-refuse decision.

#### 4.2.2 Interprocess lock

The `ca print` subcommand (§14.1) and the daemon both
materialize the auto-generated CA via §4.2.1. To prevent
two processes from generating different CAs (each clobbering
the other's `*.tmp` files or racing the renames), Phase 6
uses an interprocess advisory lock on
`<ca_storage_dir>/.ca.lock`:

- Acquired with `flock(LOCK_EX)` (blocking with a 30s
  deadline) before §4.2.1 begins.
- Released after §4.2.1 step 11 (success path) or on any
  error path before return.
- The lock file is created with mode `0600` if absent;
  contents are unused.
- Daemon and `ca print` use the same lockfile path so they
  serialize against each other.
- Lock acquisition timeout (30s) is loud — a `mitm_ca_lock_timeout`
  Error fires and startup / `ca print` exits with the
  `mitm_ca_generation_failed` outcome. Operators
  encountering this clear stale lockfiles by removing the
  daemon and re-running.

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
  - **Name Constraints (RFC 5280 §4.2.1.10)** populated
    from the **§5.1.2 signing predicate** (i.e. the literal-
    host regex `tls_mitm.allowed_host_regex`, NOT the
    canonical-host upstream regex; the Name Constraints
    carry the cert-issuance bound, which is the literal-host
    bound). See §5.1.1.1 for the exact translation contract.
    The conversion is best-effort: an input regex outside
    the supported grammar yields a Warn at startup
    (`mitm_ca_name_constraints_skipped`) and the CA is
    issued without Name Constraints — the regex still gates
    leaf signing at runtime, but the CA is unconstrained
    cryptographically.

##### 5.1.1.1 Regex → NameConstraints translation contract

X.509 Name Constraints are not regexes; they are an
unordered set of `permittedSubtrees` and `excludedSubtrees`
each holding `GeneralName` entries (Phase 6 uses dNSName
only — IP-literal CONNECTs are rejected at §2.2 step 1, so
the iPAddress excluded-subtree subset is moot). The
translator accepts a small fragment of RE2 and rejects
everything else loudly:

**Supported input shapes** (any of, anchored with `^…$`
literal anchors only — the anchors are optional but
recommended for operator clarity):

1. **Literal hostname**: `^foo\.example\.com$` →
   `permitted = ["foo.example.com"]`. (Trailing `$`
   anchor is honored; without anchors the input still
   produces a literal subtree match because RFC 5280 dNSName
   constraint matching is suffix-based and a literal
   matches itself only.)
2. **Single-label wildcard prefix**: `^[a-z0-9-]+\.foo\.com$`
   or `^[^.]+\.foo\.com$` →
   `permitted = ["foo.com"]` (the constraint matches the
   literal suffix; any subdomain depth is allowed by RFC
   5280 dNSName subtree semantics, which the translator
   relies on rather than enumerating one-label preimage).
3. **Two-letter region prefix alternation**:
   `^([a-z]{2}\.)?archive\.ubuntu\.com$` →
   `permitted = ["archive.ubuntu.com"]` (the optional
   prefix collapses into the dNSName subtree of the
   suffix; the suffix-match contract subsumes the
   alternation).
4. **Multiple anchored alternation of literals**:
   `^(foo\.example\.com|bar\.example\.com)$` →
   `permitted = ["foo.example.com", "bar.example.com"]`.
5. **Literal-list syntactic sugar** (Phase 6 may pre-process
   `^(a|b|c)$` style alternations of literals into the
   set of literals before invoking the translator):
   produces one permitted subtree per literal.

**Rejected input shapes** (yield
`mitm_ca_name_constraints_skipped` Warn; CA issued without
Name Constraints):

- Character classes that are not single-label
  approximations (e.g. `[a-z0-9.-]+`, which can span dots
  and would break suffix-subtree semantics).
- Negated character classes (`[^x]`) anywhere other than
  `[^.]` for single-label prefixes.
- Quantifiers `*`, `+`, `{n,m}` outside the single-label
  prefix shape above.
- Backreferences, lookahead, or any RE2 feature beyond
  literals and the listed shapes.
- Alternation spanning multiple TLDs in a way that the
  pre-processor cannot resolve to a finite literal set.
- Empty regex, unanchored regex matching anything (an
  unanchored `.` matches any host substring).

**Excluded subtrees** are NOT populated. Earlier drafts
proposed adding `localhost`, RFC 1918 IPs, etc. as
`excludedSubtrees`. This is dropped because:
1. `excludedSubtrees: dNSName=localhost` is satisfied by
   any leaf whose SAN ≠ `localhost` exactly — i.e. it adds
   nothing in practice since the runtime regex won't admit
   `localhost` either.
2. iPAddress excluded subtrees are moot: §2.2 step 1
   rejects IP-literal CONNECTs before the cert path runs,
   so no IP-typed leaf SAN can ever exist.

**Output format**:

```
NameConstraints {
    PermittedDNSDomains: [...]   // from the supported shapes above
    ExcludedDNSDomains:  []      // empty by design
    PermittedIPRanges:   []      // empty
    ExcludedIPRanges:    []      // empty
    PermittedEmailAddresses: nil
    ExcludedEmailAddresses:  nil
    PermittedURIDomains:     nil
    ExcludedURIDomains:      nil
    NameConstraintsCritical: true
}
```

`Critical: true` ensures legacy clients that don't
understand the extension reject the CA cert outright
(rather than silently ignoring the constraint and trusting
arbitrary leaves). Operators whose fleet has clients that
don't honor critical Name Constraints supply a CA out of
band with whatever constraints they want.

**Client compatibility**: Modern apt + GnuTLS / OpenSSL
+ Go's stdlib + Java JCA all honor RFC 5280 §4.2.1.10
critical Name Constraints with dNSName subtrees. Phase 6
does not undertake a compatibility matrix — the
auto-generated CA path is opt-in, and operators who hit a
client that rejects the CA can either widen their regex
(triggering `mitm_ca_name_constraints_skipped` and an
unconstrained CA) or supply a CA out of band.

#### 5.1.2 Effective MITM allowlist

The CONNECT pre-handshake predicate is a conjunction of a
**signing gate** and a **fetch gate**:

```
mitm_eligible(literal_host) =
    signing_gate(literal_host)
        AND fetch_gate(canonicalize(literal_host))

signing_gate(literal_host) =
    tls_mitm.allowed_host_regex == ""
        OR re2(tls_mitm.allowed_host_regex).MatchString(literal_host)

fetch_gate(canonical_host) =
    re2(upstream.allowed_host_regex).MatchString(canonical_host)
```

- `literal_host` is the lower-cased, IDNA-normalized CONNECT
  request-target host (no port, no port suffix, no `[…]` IPv6
  brackets — IPv6/IP-literal CONNECTs are already rejected at
  step 1 of §2.2).
- `canonicalize(literal_host)` is the Remap pipeline from
  `internal/proxy/proxy.go canonicalize` — a pure regex match
  loop with no DNS or I/O. It returns `literal_host` unchanged
  when no rule matches.
- `fetch_gate` is the Phase 1 §6.6 / handler.go:274
  `Client.HostAllowed` predicate, used verbatim against the
  canonical host. Empty `upstream.allowed_host_regex` means
  "deny everything" (Phase 1 §6.6 contract).

**Signing gate vs fetch gate.** The two gates have different
inputs intentionally. The signing gate operates on the literal
CONNECT host because the resulting leaf cert's SAN is the
literal host (§5.1.3); operators bound which literal hostnames
the cache is willing to issue certs for. The fetch gate
operates on the canonical host because the cache will be
contacting the post-Remap upstream; the existing Phase 1
allowlist contract is verbatim. A literal host that Remap
canonicalizes to a denied upstream is rejected at CONNECT
without ever issuing a cert.

**No "broader than upstream" startup check.** A general
regex-subset relation between two RE2 regexes is undecidable;
a partial check that worked only on simple regex shapes would
mislead operators into thinking it caught all misconfigurations.
Instead, when `tls_mitm.allowed_host_regex` is non-empty AND
upstream is also non-empty, the daemon emits a one-shot Info
at startup naming both regexes and flagging that the operator
is responsible for the MITM regex being a subset of the
upstream regex. A regex misconfiguration (literal host passing
MITM gate but canonical host failing fetch gate) surfaces as
`outcome=denied_host` `mitm_connect` Warn, NOT as an
established tunnel followed by inner-GET 502 — the §2.2 step 2
fetch-gate check rejects the CONNECT pre-handshake before any
cert is issued. (Deny-range deny — `upstream.deny_target_ranges`
matching the inner-GET's resolved IP at TCP-connect time —
still produces `outcome=upstream_denied` on the inner request,
since that gate is intentionally deferred per §1.1.3.)

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
  Remap-canonical-host literals that match the effective
  allowlist. The set tested is the closed list of
  `canonicalHost` literals registered in the Remap rule
  table (`internal/proxy/proxy.go remapRule.canonicalHost`,
  built-in + user-supplied) — a finite enumerable set, NOT
  the unbounded preimage of each rule's regex. Reports as
  `match_count=<int>` and `total_canonical_hosts=<int>` so
  operators can see "regex permits 12 of 24 known
  canonical hosts". This is a sanity-check, not a
  correctness primitive: a regex that rejects all canonical
  hosts but matches the operator's intended hostname (which
  may not be among the listed canonicals) is still
  legitimate.
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
  `upstream.allowed_host_regex` and the MITM regex, reminds
  the operator that the §5.1.2 predicate is the conjunction
  of a literal-host signing gate and a canonical-host fetch
  gate (not "broader-of-the-two") and that the operator is
  responsible for verifying the relationship — regex subset
  is undecidable, so the daemon cannot check it. A
  misconfigured broader-than-upstream MITM regex surfaces at
  runtime as `outcome=denied_host` `mitm_connect` Warn (the
  CONNECT itself rejects the host pre-handshake; no cert is
  ever issued for a host that fails the fetch gate).

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
  existing Phase 1 fetcher rejects all upstream redirects
  (`internal/fetch/fetch.go:215-220 CheckRedirect → ErrRedirectBlocked`),
  and the handler converts that error to **502 bad_gateway**
  (handler.go:1547-1556) with the upstream redirect status
  preserved on the request log line as `upstream_status`.
  Phase 6 inherits this verbatim. The cache does NOT silently
  follow a redirect or downgrade the inner request to HTTP;
  operators whose upstream uses redirects configure a Remap
  rule pointing at the redirect target instead.

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

See §2.2 step 7 for the full synthetic-request contract. In
short: `r.RequestURI = "https://<literal_host><path>"`,
`r.Method = inner method`, `r.Host = literal_host`,
`r.Body = http.NoBody`, `r.Context()` derived from the outer
CONNECT, `r.RemoteAddr` = the outer CONNECT's client address
(the cache does not NAT or hide the originating address).
Query strings and fragments are dropped (the parser rejects
both — `internal/proxy/url.go:46-51, 77-82`); a request
whose inner GET path carries a query returns 400 on the
inner stream as a parser-side `bad_request` outcome.

The synthetic request enters `Handler.ServeHTTP` — Phase 6
adds no new handler entry point. The CONNECT-side `RequestURI`
construction is the integration seam.

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
  Fields: `host` (literal CONNECT host, lower-cased +
  IDNA-normalized), `port`, `client_addr`, `outcome`
  (`tunneled` / `denied_host` / `bad_port` /
  `ip_literal_host` / `tls_handshake_timeout` / `tls_failed`
  / `cert_gen_failed` / `inner_method_rejected`),
  `denied_gate` (`signing` / `fetch`; empty when
  `outcome != denied_host`; identifies which §5.1.2 gate
  rejected the host), `canonical_host` (post-Remap form,
  empty when outcome=`denied_host` and `denied_gate=signing`
  since canonicalization runs after the signing-gate check),
  `duration_ms`, `cert_cache` (`hit` / `miss` — empty when
  the cert path was not reached). Emitted at level Info on
  `tunneled`; Warn on every other outcome.

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

- **`mitm_ca_load_failed`** — at startup, when the §4.2 CA
  materialization case 3 fires (any inconsistent state of
  `<ca_storage_dir>` per §4.2 — `ca.ready` missing or
  fingerprint-mismatched, `ca.crt` or `ca.key` parse
  failure, cert/key mismatch). Fields: `path`, `err`. Level
  Error. The daemon does not bind after this event.

- **`mitm_ca_generation_failed`** — at startup, when atomic
  CA write fails (mkdir, Write, Sync, rename, fsync, lock).
  Fields: `path`, `err`. Level Error. The daemon does not
  bind after this event.

- **`mitm_ca_lock_timeout`** — at startup or in
  `apt-cacher-ultra ca print`, when the §4.2.2 interprocess
  flock on `<ca_storage_dir>/.ca.lock` could not be
  acquired within 30s. Fields: `path`. Level Error.
  Followed immediately by a `mitm_ca_generation_failed`
  Error so the operator sees both the cause and the effect.
  Caused by a concurrent `ca print` or daemon instance
  racing on the same storage dir. Recovery: identify and
  stop the racing process, then retry.

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
| F2 | Auto-generated CA directory in any non-clean state (any of: `ca.ready` missing while other ca-managed files present; `ca.ready`'s fingerprint disagrees with `sha256(ca.crt)`; either `ca.crt` or `ca.key` fails to parse; cert/key pair mismatch) | Startup logs `mitm_ca_load_failed` Error naming the offending state; daemon does not bind. Operator recovers by removing the entire `<ca_storage_dir>` (or just the daemon-managed files within it) to force regeneration on next start. Silent regeneration would change the trust anchor under every client; trust-root replacement is operator-explicit (§4.2 case 3) |
| F3 | `cache_dir/ca/` cannot be created (perms, disk full) OR §4.2.1 mid-write failure | Startup fails with `mitm_ca_generation_failed` Error; daemon does not bind. The §4.2.1 marker-file scheme guarantees that mid-write crashes leave the directory in §4.2 case 3 (uninitialized) — `ca.ready` is the commit primitive and is written last; absence of `ca.ready` means the trust anchor is NOT yet adopted regardless of which `*.tmp` or `ca.{crt,key}` files happen to be on disk |
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
| F14 | CONNECT host fails the §5.1.2 effective allowlist (literal host fails signing predicate, OR canonical host fails fetch predicate) | 403 on the CONNECT response; tunnel closes; `mitm_connect` Warn (`outcome=denied_host`, `denied_gate=signing` or `fetch`). No cert is issued, no TLS handshake |
| F15 | Inner GET upstream fetch fails the SSRF deny-range gate at TCP-connect time | The inner GET response is whatever the existing Phase 1 fetcher returns (typically 502 with `outcome=upstream_denied` on the request log line); tunnel closes after inner response. The CONNECT itself succeeded (as designed; §1.1.3) |
| F16 | Daemon shuts down during an in-flight CONNECT tunnel | The §9.4 sync.WaitGroup primitive holds Shutdown until the tunnel closes; on shutdown-context expiry, the conn is closed forcibly and the inner request's upstream fetch is cancelled |
| F17 | Clock skew: leaf cert `not_before` is in the future | Apt rejects with a `not yet valid` TLS error; cache's clock is the source of truth — operators should run NTP. No Phase 6 mitigation beyond logging on the cache side |
| F18 | CA expires mid-lifetime | All client TLS handshakes fail; `mitm_connect` Warn `outcome=tls_failed` rate spikes; operator's `acu_mitm_ca_not_after_unixtime` alert (set to fire 30 days before expiry) catches this before the spike |
| F19 | CA private key file ownership / mode wrong on disk (e.g. world-readable) | Phase 6 does NOT enforce mode/ownership at startup beyond the §4.2.1 atomic-write guarantees on the auto-generated path. Operator-supplied CA paths are read with whatever mode the operator chose; `apt-cacher-ultra ca print` is the audit primitive (it warns on `S_IROTH` set on `ca.key`). A future phase can add mandatory mode enforcement if any deployment asks |
| F20 | Leaf cert algorithm config invalid at startup | Startup fails with config-error log naming the rejected value |
| F21 | Upstream HTTPS server presents an invalid cert (chain failure, expired, hostname mismatch) | Inner GET fetch fails with the existing Phase 1 fetcher behavior — `outcome=bad_gateway` on the inner request log line; the cache does NOT relax verification (§5.4) |
| F22 | Upstream sends a redirect from `https://` to `http://` (or any other 3xx) | Inner GET fails with `outcome=bad_gateway` (handler.go:1547-1556 maps `fetch.ErrRedirectBlocked` → 502). The upstream's 3xx status code is preserved on the request log line as `upstream_status`. The cache does NOT silently follow the redirect or downgrade the inner request to HTTP; apt sees a 502 from the cache rather than the 3xx from upstream. Operators whose archive uses redirects configure a Remap rule pointing at the redirect target |
| F23 | §4.2.2 interprocess lock contention exceeds 30s | Startup logs `mitm_ca_lock_timeout` Error and `mitm_ca_generation_failed` Error; daemon does not bind. `apt-cacher-ultra ca print` exits with code 4. Caused by a concurrent `ca print` or another daemon instance racing on the same `<ca_storage_dir>`. Operator inspects the lockfile owner and clears stale state |

---

## 12. Test strategy (delta over SPEC §12 / SPEC2 §12 / SPEC3 §12 / SPEC4 §12 / SPEC5 §12)

### 12.1 Unit tests

In `internal/proxy/tlsmitm/`:

- CA generate-and-persist round trip: generate, write,
  reload, verify cert/key match, fingerprint stable
  (covers F1 success path).
- CA atomic-write semantics (§4.2.1 marker-file scheme):
  walk the §4.2.1 state table by killing the writer at
  each step (simulate disk-full / fault-injection at every
  numbered substep). For each kill point, verify the
  next §4.2 startup loads exactly when `ca.ready` exists
  AND `ca.ready`'s fingerprint matches `sha256(ca.crt)`,
  and refuses with `mitm_ca_load_failed` for every other
  state (including ca.crt + ca.key but no ca.ready —
  the previously-described "half-trusted" hazard). Also
  verifies `*.tmp` cleanup on §4.2.1 step-3 re-entry
  (covers F3).
- CA interprocess lock (§4.2.2): two concurrent `ca print`
  invocations against the same `<ca_storage_dir>` produce
  one CA on disk, both processes print the same
  fingerprint to stdout. The second invocation takes the
  §4.2.1 step-2 compare-and-adopt path. With `flock` held
  externally past 30s, both daemon startup and `ca print`
  fail loudly with the lock_timeout error (covers F23).
- CA marker file integrity: starting with a clean
  `<ca_storage_dir>` containing `ca.crt` + `ca.key` but
  no `ca.ready` (operator manually placed files), the
  daemon refuses to load (case 3) and emits
  `mitm_ca_load_failed`; the operator-supplied path
  (`tls_mitm.ca_cert` / `tls_mitm.ca_key` non-empty) does
  NOT consult `ca.ready` and loads on the same files.
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
- CONNECT handler: denied host → 403 with
  `denied_gate=signing` when literal host fails the
  signing predicate; with `denied_gate=fetch` when literal
  passes signing but Remap-canonical fails the fetch
  predicate. Both branches asserted (F14).
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
  (`httptest.NewTLSServer`). The system trust store does NOT
  trust httptest's auto-generated cert, so the test installs
  the test server's `Certificate()` into a custom
  `*x509.CertPool` and supplies it via the
  **`fetch.Options.rootCAs` test-only seam** (a private
  field analogous to the existing `dialContext` and `now`
  test seams in `internal/fetch/fetch.go:108-114`).
  Production code never sets `rootCAs` — `nil` keeps the
  Go default of system trust roots, preserving the §5.4
  invariant that production upstream TLS uses ONLY the
  system trust store. The test seam is package-private and
  named in `internal/fetch/fetch.go` alongside the
  existing seams; it is NOT exposed via config.

  First request misses, second hits, both observe
  `X-Acu-Mitm: 1` on the response.
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
- HTTPS-to-HTTP redirect from upstream: 502 bad_gateway on
  the inner response (Phase 1 fetcher rejects all redirects
  via `CheckRedirect` → `ErrRedirectBlocked`); upstream
  redirect status preserved on the `request` log line as
  `upstream_status`; not auto-followed (F22).
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
  old CA until restart. The next startup hits §4.2 case 3
  (any of `ca.crt`, `ca.key`, `ca.ready` missing while the
  others are present is an inconsistent state) and the
  operator intervention path is exercised. Operator clears
  `<cache_dir>/ca/` and the next start regenerates a new
  CA — visibly different fingerprint via
  `acu_mitm_ca_not_after_unixtime` and `mitm_ca_loaded`
  log line, so the trust-anchor change is loud and
  observable to monitoring (F2 end-to-end).
- **CA generation crash mid-write.** Kill -9 the daemon
  at each numbered step of §4.2.1. Restart and verify the
  §4.2 case-2 "load and use" branch fires only when
  `ca.ready` is committed AND its fingerprint matches
  `sha256(ca.crt)` AND both files parse AND cert/key match.
  Every other on-disk state (`*.tmp` only; `ca.crt` only;
  `ca.crt + ca.key` without `ca.ready`; `ca.ready` without
  one of the others; mismatching fingerprint) must produce
  `mitm_ca_load_failed` Error and the daemon must NOT bind.
  NEVER silent regeneration of a new trust anchor under a
  fleet that already trusts an old one — and NEVER silent
  adoption of `ca.crt + ca.key` whose marker file was not
  written (the trust anchor is uncommitted in that state).
  (F3 end-to-end.)
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
| F23 | 12.1 unit (interprocess flock contention: spawn a goroutine that holds the lock past 30s, verify daemon-side §4.2.1 returns the lock_timeout error and `apt-cacher-ultra ca print` exits with code 4) |

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

internal/proxy/
  proxy.go                      # unchanged (HTTPS/// magic stays in isHTTPSMagic / splitHTTPSMagic)
  url.go                        # unchanged (request-URI parsing unchanged)

internal/admin/
  status.go                     # +TLS MITM section in HTML + JSON

packaging/config/
  config.toml.default           # +commented [tls_mitm] block

packaging/scripts/
  postinstall.sh                # +chmod 0700 cache_dir/ca/
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
daemon is running. Concurrent `ca print` + daemon CA
generation is serialized by the §4.2.2 interprocess flock
on `<ca_storage_dir>/.ca.lock`: the second process to
acquire the lock takes the §4.2.1 step-2 compare-and-adopt
branch (sees `ca.ready` matching `ca.crt`, loads instead of
regenerating). Atomic rename alone does NOT prevent this
race — two processes could each write their own
`ca.crt.tmp` to a non-colliding path and rename, producing
clobbered `ca.crt`/`ca.key` from different generations. The
flock prevents that.

If `ca print` cannot acquire the lock within the 30s
timeout it exits with code 4 ("`ca print` lock contention,
likely concurrent daemon generation; retry shortly") rather
than racing.

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
- `4`: lock contention on `<ca_storage_dir>/.ca.lock`
  (concurrent `ca print` or daemon-side generation in
  progress); retry recommended.

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
   externally-observable behavior is **behaviorally identical**
   to a Phase 5 daemon, with two intentional and documented
   advertisement-only deltas. Behavior preserved:
   - GET / HEAD request handling on the proxy listener:
     unchanged.
   - Cache hit / miss / freshness / GC / admin paths:
     unchanged.
   - No `mitm_*` log lines are emitted at any point.
   - No `acu_mitm_*` metrics ever increment from a request
     path.
   - `acu_requests_total` outcome enum: unchanged (Phase 5
     set).

   Documented deltas (advertisement only — no behavior
   difference unless the client explicitly probes them):
   - **CONNECT response**: returns 405 with
     `Allow: GET, HEAD` (Phase 5 has no `Allow` because the
     listener never expected CONNECT); the response body
     and status code are unchanged from Phase 5.
   - **Status JSON shape**: gains a top-level `tls_mitm`
     key with payload `{"enabled": false}`. Operators
     parsing the JSON who add a `?` for the new key are
     compatible with both Phase 5 and disabled-mode Phase 6;
     operators expecting an exact key set must update.
   - **`acu_mitm_*` metric registration**: the metrics ARE
     registered in disabled mode (so `/metrics` scrapes are
     stable across enabled/disabled), but ALL counters and
     histograms remain at zero — no observation happens
     until `tls_mitm.enabled = true` AND a CONNECT arrives.
     Gauges (`cert_cache_size`, `cert_cache_capacity`,
     `ca_not_after_unixtime`) report zero.

   These three deltas are intentional and documented in
   `CHANGELOG.md`; operators upgrading from Phase 5 see them
   on first deploy regardless of whether they enable MITM.

**Config validation**

5. Every §5.2 validation rule fires on the documented
   invalid-input class and the daemon refuses to start with
   the named log line. Tested as part of §12.1.

**CA paths**

6. The auto-generated CA path produces a valid CA file
   triple (`ca.crt`, `ca.key`, `ca.ready` per §4.2.1) under
   `<cache_dir>/ca/`, all mode 0600, atomic on disk-full or
   kill-mid-write — the §12.3 chaos test walks every kill
   point in §4.2.1 and verifies the next start either loads
   (case 2) or refuses with `mitm_ca_load_failed` (case 3),
   never silently regenerates and never adopts a partially-
   committed trust anchor.
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
