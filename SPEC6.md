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
startup.

**RFC 5280 dNSName Name Constraints are suffix-based, not
exact** — see §5.1.1.1. A permitted subtree of `foo.example.com`
also admits any subdomain such as `bar.foo.example.com`. The
constraint is therefore an over-approximation of the runtime
regex: it narrows the blast radius of CA-key compromise to the
suffix tree spanned by the regex literals, NOT to the exact set
of hostnames the regex matches. Operators evaluating residual
risk must reason about subdomain expansion, not just literal
match. There is no shape of dNSName constraint that bounds a CA
to a single hostname.

Operator-supplied CAs carry whatever constraints the operator
put on them — Phase 6 does not add or modify constraints on
operator-supplied CAs. Operators in adversarial or multi-tenant
environments who want tighter cryptographic constraint on the
CA itself (i.e. limiting blast radius of a key compromise)
should either (a) narrow `tls_mitm.allowed_host_regex` to
literal hostnames whose dNSName suffix tree they accept as the
blast-radius bound, or (b) supply a CA out of band with the
desired Name Constraints already set. See §11 for the
failure-mode catalog.

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

1. Parses the request-target into `(host, port)`. The
   request-target wire form is `host ":" port` per RFC 7231
   §4.3.6. Apply the parsing rules below in order; the first
   failure short-circuits with the documented outcome.
   Failure outcomes ALL emit `400 Bad Request` to the client
   and a `mitm_connect` Warn at conn close.

   **Structural / syntactic failures** — `outcome=bad_target`:
   - **Empty request-target** (no `host:port` at all).
   - **Missing port** (no `:` in the target, e.g.
     `apt.corretto.aws`).
   - **Empty host** (`:443` with nothing before the colon).
   - **Empty port** (`apt.corretto.aws:`).
   - **Non-numeric port** (`apt.corretto.aws:https`,
     `apt.corretto.aws:abc`).
   - **Port out of range** (port < 1 or > 65535 after
     numeric parse).
   - **Multiple colons in the non-IPv6 form** (e.g.
     `host:443:extra`).
   - **Unbracketed IPv6 literal** (e.g. `::1:443` — IPv6
     literals MUST be bracketed per RFC 7230 §2.7.1, e.g.
     `[::1]:443`). The handler distinguishes "looks like
     IPv6 but missing brackets" from a malformed two-colon
     hostname by attempting IPv6 parse on the
     pre-final-colon prefix; ambiguous shapes default to
     `bad_target`.
   - **`splitHostPort`-style parse failure** for any other
     reason that prevents extracting a `(host, port)` pair.

   **Host validation failures** (after structural parse
   succeeds and host is extracted):
   - **IDNA-to-ASCII failure** (e.g. invalid Unicode,
     malformed punycode, label > 63 octets, total length >
     253 octets, or any other condition `x/net/idna`
     `Lookup.ToASCII` rejects) → `outcome=bad_host`.
   - **Invalid DNS label** after IDNA normalization
     (label contains characters not in
     `[A-Za-z0-9-]` after ToASCII; label starts or ends
     with `-`; empty label other than the optional final
     root label) → `outcome=bad_host`.
   - **IP literal** (the IDNA-normalized host parses as a
     dotted-quad IPv4, IPv4-mapped IPv6, or, after
     bracket-stripping, an IPv6 literal) →
     `outcome=ip_literal_host`. Phase 6 rejects IP-literal
     CONNECTs because the allowlist is hostname-based and
     the auto-generated CA's Name Constraints are
     dNSName-only; allowing an IP-literal CONNECT would
     let it pass the regex by coincidence and issue a
     leaf cert that does not match SNI. Operators with
     IP-literal HTTPS upstreams configure a hostname
     mapping in their DNS instead.

   **Port validation** (after structural parse and host
   validation succeed):
   - Port other than `443` → `outcome=bad_port`. The
     simpler default of "443 only" exists because non-443
     HTTPS upstreams are rare; a future override knob is
     deferred.

   **Trailing-dot normalization.** A trailing `.` on the
   host (e.g. `apt.corretto.aws.:443`) is a fully-qualified
   absolute DNS name and is canonicalized by stripping the
   trailing dot before allowlist evaluation and cert cache
   lookup. This matches Go `net.LookupHost` behavior and
   prevents `host` and `host.` from issuing two distinct
   leaf certs. The trailing dot is stripped silently — no
   Warn — since it is a legitimate FQDN form.
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
   2. **Fetch predicate** — `fetch.Client.HostAllowed` against
      the **canonical host**, where canonical = result of
      running the literal CONNECT host through Remap
      (`internal/proxy/proxy.go canonicalize`). Remap is
      regex-only, so this introduces no DNS or I/O at CONNECT
      time. The fetch predicate iterates the
      `upstream.allowed_host_regex` LIST (Phase 1 schema:
      `[]string`; see `internal/fetch/allow.go:25`
      `Client.checkAllowed`) and succeeds on any-match; empty
      list denies every host (Phase 1 §6.6 contract). Failure →
      `403 Forbidden` + `mitm_connect` Warn
      (`outcome=denied_host`, `denied_gate=fetch`); no cert
      generation, no TLS handshake.

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
   - `RequestURI`: `"https://<literal_host><inner_request_uri>"` —
     scheme=https, host = the literal lower-cased
     IDNA-normalized CONNECT host, request-URI = the inner
     request's wire-form request-target verbatim (path plus any
     query string or fragment, exactly as the client sent it).
     The MITM path does NOT strip query strings or fragments
     before dispatch; the parser
     (`internal/proxy/url.go parseRequestURI` lines 46–51 and
     77–82) is the single point that rejects them, returning
     `ErrInvalidURI`. The handler then surfaces that as 400
     `bad_request` on the inner stream — the same outcome a
     plain proxy GET with a query string would produce. This
     is intentional: stripping the query at MITM dispatch time
     would silently alias `/Packages?x=1` to `/Packages` in
     the cache, defeating the parser's `RawQuery != ""`
     guard. Apt repository URLs never carry queries or
     fragments in practice; a request that does is anomalous
     and surfaces loudly.
   - `Method`: from the inner request line (already
     constrained to GET or HEAD by step 6).
   - `Host`: same literal host as in `RequestURI`. The parser
     accepts but ignores `r.Host` (handler.go uses it only as
     a logging breadcrumb), so this is symmetry-only.
   - `RemoteAddr`: the outer CONNECT's client address.
   - `Body`: `http.NoBody` for both GET and HEAD.
   - `Context`: derived from the outer CONNECT context (so
     listener Shutdown cancels the synthetic request via the
     same path that cancels any in-flight HTTP request), with
     an unexported sentinel value attached via
     `context.WithValue` to signal that this request was
     dispatched by the MITM CONNECT path. The sentinel is the
     **logger integration contract** for the §10.1 `mitm` log
     field — see "MITM context marker" below.

     **The synthetic request's context is the
     ResponseWriter / handler-side cancellation context
     ONLY.** It is NOT the upstream fetch context. The
     existing `Handler.serveCacheMiss`
     (`internal/handler/handler.go:843`) deliberately
     fetches under `h.lifecycleCtx`, not `r.Context()`,
     so that a leader's client disconnect does NOT kill
     the fetch for waiters that are still connected
     (handler.go:849-857 documents this invariant). Phase 6
     does not change that invariant: a CONNECT client that
     hangs up mid-cache-miss leaves the leader's upstream
     fetch running until the body completes (or
     `lifecycleCtx` cancels at Shutdown). The
     `lifecycleCtx` is also what propagates Shutdown to
     in-flight upstream fetches — Phase 6 reuses that
     mechanism.

     This means: the synthetic request's context cancellation
     affects the inner ResponseWriter (a closed CONNECT
     short-circuits cache-read writes back to the client) and
     the in-handler timeout / freshness checks that already
     read `r.Context()`, but does NOT propagate into the
     leader fetch. The chaos test §12.3 asserts this directly.
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

The case-detection rule looks at the **daemon-managed
"real" files** (`ca.crt`, `ca.key`, `ca.ready`); presence
of `*.tmp` files alone (no real files committed) is not a
case-3 trigger — it is generation residue and the
generation path cleans it up under lock (§4.2.1 step 3).
The case-3 rule fires when a real file is present without
its committed siblings, or when committed siblings
disagree.

1. **No real files committed** (none of `ca.crt`,
   `ca.key`, `ca.ready` exist; any number of `*.tmp` files
   may or may not be present): treat as first-start.
   Generate a new CA pair (§5.1.1), write to disk
   atomically (§4.2.1) — generation step 3 cleans any
   stale `*.tmp` residue under the lock — then load. Emit
   `mitm_ca_generated` Info plus `mitm_ca_loaded` Info.
   Justification: a `*.tmp`-only state is the residue of a
   prior crashed first-start where no trust anchor was
   ever committed; auto-recovering it does not change any
   already-trusted CA (because there isn't one).
2. **`ca.crt`, `ca.key`, and `ca.ready` all exist; both
   files parse; `ca.ready`'s fingerprint matches
   `sha256(ca.crt)`; the cert/key match check passes**:
   load and use. Best-effort cleanup of any leftover
   `*.tmp` residue (under lock). Emit `mitm_ca_loaded`
   Info (`source=generated`, `fingerprint_sha256=…`,
   `not_after_unixtime=…`).
3. **Inconsistent committed state** — at least one of
   `ca.crt` / `ca.key` / `ca.ready` is present but the
   trio is not self-consistent. Specifically:
   - `ca.ready` missing while either `ca.crt` or `ca.key`
     exists (the trust anchor was not committed but the
     key material is on disk — this is the "half-trusted"
     hazard the marker scheme exists to detect).
   - `ca.ready` present but `ca.crt` or `ca.key` missing.
   - `ca.ready`'s fingerprint disagrees with
     `sha256(ca.crt)`.
   - Either real file fails to parse.
   - Cert/key mismatch.

   Startup fails with `mitm_ca_load_failed` Error naming
   the offending state. The daemon does NOT silently
   regenerate in this state. Operator recovery is
   **explicit removal of the entire `cache_dir/ca/`
   directory** (or just the daemon-managed real files —
   `ca.crt`, `ca.key`, `ca.ready` — within it), forcing
   case 1 on the next start. The marker scheme
   guarantees this branch never fires spuriously from a
   §4.2.1 mid-write crash where no real file was committed
   yet (that state is case 1 above); it fires when at
   least one real file landed without its committed
   siblings. Silent regeneration in that state would
   change the trust anchor under every client without
   warning; trust-root replacement is an operator-explicit
   action.
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
| Empty directory | First boot | Case 1 — generate |
| `*.tmp` files only (no real files) | Crashed between steps 5–6 | Case 1 — generation step 3 cleans tmp residue under lock and proceeds; no committed trust anchor existed |
| `ca.crt` only, no key, no marker | Crashed between rename of crt and key | Case 3 — operator clears (real file present without committed siblings) |
| `ca.crt` + `ca.key`, no marker | Crashed between rename of key and marker write | Case 3 — operator clears (key material on disk but trust anchor NOT yet committed; loading would adopt an uncommitted CA) |
| `ca.crt` + `ca.key` + `ca.ready` (matching fp) | Clean state | Case 2 — load (best-effort `*.tmp` cleanup under lock) |
| `ca.crt` + `ca.key` + `ca.ready` (mismatching fp) | Manual tampering or partial restoration | Case 3 — operator investigates |
| `ca.ready` only (no `ca.crt`/`ca.key`) | Marker present but no trust anchor — manual tampering | Case 3 — operator clears |
| Any of the above + extra `*.tmp` residue | A prior write attempt left tmp files; current trust anchor (if committed) is unaffected | Same as the row matching the real-file pattern; tmp files are cleaned in the next §4.2.1 invocation under lock |

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
  `mitm_ca_generation_failed` outcome.

  **Recovery advice — do NOT delete the lockfile.** flock
  locks are tied to the file descriptor that holds them, not
  to the file's pathname; removing `<ca_storage_dir>/.ca.lock`
  does NOT release the lock if a process still has it open,
  and it can create a split-lock condition where a new
  process opens a fresh inode at the same path and acquires
  a "lock" on a file the old holder no longer references.
  Operator recovery is:
  1. Identify the holding process: `lsof <ca_storage_dir>/.ca.lock`
     or `fuser <ca_storage_dir>/.ca.lock` will name the PID
     and command line. Typical culprits: a stuck
     `apt-cacher-ultra ca print` invocation, a previous
     daemon instance that did not exit cleanly, or a parallel
     fleet-rollout script running `ca print` on the same
     storage dir.
  2. Stop that process (graceful preferred: SIGTERM and
     wait; SIGKILL only if it is wedged). The kernel
     releases the flock when the process exits regardless
     of how it exits.
  3. Retry the original startup or `ca print`. The lock
     will be acquirable immediately.
  Do NOT `rm` the lockfile: it does not help, it can hurt.

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
allowed_host_regex = ""                       # empty = no MITM narrowing (fetch gate alone applies)
allow_unconstrained_ca = false                # opt-in: auto-generate a CA without RFC 5280 Name Constraints
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
    bound). See §5.1.1.1 for the exact translation contract
    and the supported / rejected RE2 grammar.

    **Fail-closed by default.** When the configured regex
    cannot be safely translated (empty regex, untranslatable
    grammar, etc. — see §5.1.1.1's rejected-input list),
    auto-generation FAILS at startup unless the operator
    has explicitly opted in via
    `tls_mitm.allow_unconstrained_ca = true`. Rationale: the
    auto-generated CA is distributed to every client in the
    fleet; an unconstrained CA confers fleet-wide trust on
    arbitrary leaf certs the cache can issue, and the
    runtime regex gate alone does NOT bound a stolen CA
    key's blast radius (the runtime gate runs in the cache
    process; a stolen key is signed elsewhere). Surfacing
    this as a startup failure forces the operator to
    consciously accept the risk, OR to narrow the regex to
    a translatable shape, OR to supply a CA out of band
    with whatever constraints they want.

    Behavior:

    | `allowed_host_regex` | `allow_unconstrained_ca` | Result |
    |----------------------|--------------------------|--------|
    | translatable shape (§5.1.1.1) | (any) | CA generated WITH Name Constraints; `mitm_ca_loaded` Info names the constraints |
    | empty | `false` (default) | Startup FAILS with `mitm_ca_unconstrained_refused` Error; daemon does not bind. Operator action: set the regex, OR opt in, OR supply a CA |
    | empty | `true` | CA generated WITHOUT Name Constraints; `mitm_ca_name_constraints_skipped` Warn fires (`reason="empty_regex"`); operator accepted the blast-radius posture |
    | non-empty but untranslatable | `false` (default) | Startup FAILS with `mitm_ca_unconstrained_refused` Error naming the regex and the rejection reason |
    | non-empty but untranslatable | `true` | CA generated WITHOUT Name Constraints; `mitm_ca_name_constraints_skipped` Warn fires (`reason=<translation rejection reason>`) |

    The `allow_unconstrained_ca` knob applies only to the
    **auto-generated CA path**. Operator-supplied CAs carry
    whatever constraints the operator put on them; Phase 6
    does not add or modify constraints on operator-supplied
    CAs (and does not require any opt-in for them).

##### 5.1.1.1 Regex → NameConstraints translation contract

X.509 Name Constraints are not regexes; they are an
unordered set of `permittedSubtrees` and `excludedSubtrees`
each holding `GeneralName` entries (Phase 6 uses dNSName
only — IP-literal CONNECTs are rejected at §2.2 step 1, so
the iPAddress excluded-subtree subset is moot).

**RFC 5280 dNSName matching is suffix-based, not exact.**
Per RFC 5280 §4.2.1.10: "DNS name restrictions are expressed
as host.example.com. Any DNS name that can be constructed by
simply adding zero or more labels to the left-hand side of
the name satisfies the name constraint." So a permitted
subtree of `foo.example.com` admits leaf certs whose SAN is
`foo.example.com` itself AND any deeper subdomain such as
`bar.foo.example.com` or `a.b.foo.example.com`. The
translator therefore produces an **over-approximation** of
the regex's preimage: every literal hostname the regex
admits is permitted by the constraint, but the constraint
also admits subdomains the regex would not admit. This is
inherent to the dNSName GeneralName shape — there is no
per-leaf-label "anchor" in X.509 Name Constraints. Any
"exact match" promise from earlier drafts is incorrect.

The translator accepts a small fragment of RE2 and rejects
everything else loudly. Each accepted shape is documented
with its exact `permittedSubtrees` output AND the
over-approximation it introduces:

**Supported input shapes** (any of, anchored with `^…$`
literal anchors only — the anchors are optional but
recommended for operator clarity):

1. **Literal hostname**: `^foo\.example\.com$` →
   `permitted = ["foo.example.com"]`. *Over-approximation:*
   the constraint also admits `*.foo.example.com` (any
   depth) since dNSName matching is suffix-based. Operators
   wanting exact-match cryptographic narrowing must accept
   that X.509 Name Constraints cannot express it; the
   runtime signing gate (§5.1.2) is the only exact-match
   bound and it depends on key custody.
2. **Single-label wildcard prefix**: `^[a-z0-9-]+\.foo\.com$`
   or `^[^.]+\.foo\.com$` →
   `permitted = ["foo.com"]` (the constraint matches the
   literal suffix; any subdomain depth is allowed by RFC
   5280 dNSName subtree semantics, which the translator
   relies on rather than enumerating one-label preimage).
   *Over-approximation:* the regex admits exactly one label
   of prefix; the constraint admits zero or more.
3. **Two-letter region prefix alternation**:
   `^([a-z]{2}\.)?archive\.ubuntu\.com$` →
   `permitted = ["archive.ubuntu.com"]` (the optional
   prefix collapses into the dNSName subtree of the
   suffix; the suffix-match contract subsumes the
   alternation). *Over-approximation:* same as shape 2 —
   the constraint allows arbitrary subdomain depth.
4. **Multiple anchored alternation of literals**:
   `^(foo\.example\.com|bar\.example\.com)$` →
   `permitted = ["foo.example.com", "bar.example.com"]`.
   *Over-approximation:* each entry permits its own
   subdomain tree per shape 1.
5. **Literal-list syntactic sugar** (Phase 6 may pre-process
   `^(a|b|c)$` style alternations of literals into the
   set of literals before invoking the translator):
   produces one permitted subtree per literal — same
   over-approximation as shape 4.

**The translator never produces a constraint narrower than
the regex's literal preimage; it produces a coarser-or-equal
constraint by design.** Operators who need exact-leaf
narrowing must rely on the runtime signing gate and accept
that key compromise expands blast radius to the suffix tree
of every regex literal.

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
**signing gate** and a **fetch gate**. Each gate has
different inputs and different semantics for "empty"; do not
collapse them into one predicate:

```
mitm_eligible(literal_host) =
    signing_gate(literal_host)
        AND fetch_gate(canonicalize(literal_host))

signing_gate(literal_host) =
    // tls_mitm.allowed_host_regex is at most one RE2 pattern
    // (config-validated). It is the OPTIONAL narrowing gate.
    tls_mitm.allowed_host_regex == ""
        OR re2(tls_mitm.allowed_host_regex).MatchString(literal_host)

fetch_gate(canonical_host) =
    // upstream.allowed_host_regex is a LIST of RE2 patterns
    // (config schema: []string; see internal/config/config.go:76,
    // internal/fetch/allow.go:25 Client.checkAllowed). The list
    // is "any-match": the gate succeeds iff at least one
    // compiled regex in the list matches. An EMPTY list (Phase 1
    // §6.6 contract) DENIES EVERYTHING.
    Client.HostAllowed(canonical_host)   // any-match across c.allow
```

- `literal_host` is the lower-cased, IDNA-normalized CONNECT
  request-target host (no port, no port suffix, no `[…]` IPv6
  brackets — IPv6/IP-literal CONNECTs are already rejected at
  step 1 of §2.2).
- `canonicalize(literal_host)` is the Remap pipeline from
  `internal/proxy/proxy.go canonicalize` — a pure regex match
  loop with no DNS or I/O. It returns `literal_host` unchanged
  when no rule matches.
- `fetch_gate` calls the Phase 1 §6.6 / handler.go:274
  `Client.HostAllowed` predicate verbatim against the
  canonical host. Phase 6 does NOT add a different code path
  for the fetch gate; it reuses the existing list-match
  predicate so the literal-vs-canonical asymmetry is the only
  Phase 6 delta.

**Asymmetry: signing gate is a single optional regex; fetch
gate is the existing upstream LIST of regexes.** This matches
the existing config schema (Phase 1 had a list; we did not
extend that). An empty signing regex means "no MITM
narrowing" — the fetch gate alone applies. An empty upstream
list means "deny everything" — the fetch gate fails for every
host. The two emptiness semantics are intentionally different
and tied to their config schemas:

| Gate | Type | Empty meaning |
|------|------|---------------|
| signing (`tls_mitm.allowed_host_regex`) | single RE2 string | predicate vacuously true (no narrowing applied) |
| fetch (`upstream.allowed_host_regex`) | list of RE2 strings | predicate denies every host (Phase 1 §6.6) |

Earlier drafts of this spec included an "inherit upstream"
shorthand for the signing gate when empty. That shorthand is
NOT implemented: the signing-gate config is a separate single
regex with its own validation, and "inherit" would silently
re-narrow the literal-host predicate to whatever the canonical-
host predicate is (which would defeat the Phase 6 design intent
of two independent gates with different inputs). Operators who
want signing-gate parity with the upstream list must spell out
their literal-host narrowing in `tls_mitm.allowed_host_regex`
explicitly.

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
regex-subset relation between an RE2 regex and a list of
RE2 regexes is undecidable in the general case; a partial
check that worked only on simple regex shapes would mislead
operators into thinking it caught all misconfigurations.
Instead, when `tls_mitm.allowed_host_regex` is non-empty AND
the `upstream.allowed_host_regex` list is non-empty, the
daemon emits a one-shot Info at startup naming the MITM
regex plus the full upstream list, and flagging that the
operator is responsible for the relationship between the
literal-host gate and the canonical-host gate (subset of
*which* upstream pattern? — the operator decides). A regex
misconfiguration (literal host passing MITM gate but
canonical host failing every entry in the upstream list)
surfaces as `outcome=denied_host` `mitm_connect` Warn, NOT
as an established tunnel followed by inner-GET 403 — the
§2.2 step 2 fetch-gate check rejects the CONNECT
pre-handshake before any cert is issued. (Deny-range deny —
`upstream.deny_target_ranges` matching the inner-GET's
resolved IP at TCP-connect time — still produces
`outcome=forbidden` on the inner request log line with
`upstream_status` present (`fetchAttempted=true`
distinguishes it from the pre-flight host rejection which
sets `fetchAttempted=false`), since that gate is
intentionally deferred per §1.1.3.)

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
- Signing: the signature algorithm is **derived from the CA
  key type, not the leaf algorithm**. The CA key is the
  signing key; the leaf algorithm config governs only the
  leaf's *own* key pair.
  - **ECDSA P-256 CA** (auto-generated default; also accepted
    operator-supplied) → `ECDSAWithSHA256`
    (`x509.ECDSAWithSHA256`).
  - **ECDSA P-384 CA** (operator-supplied only) →
    `ECDSAWithSHA384`.
  - **RSA-2048 / 3072 / 4096 CA** (operator-supplied only) →
    `SHA256WithRSA` (`x509.SHA256WithRSA`).
  - **Any other key type** (Ed25519, ECDSA P-521, RSA <2048,
    DSA, etc.) → startup fails with the §5.2 validation rule
    below; the daemon does not bind. Phase 6 implements this
    closed enum; future phases can add Ed25519 if any
    deployment asks.

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
- `tls_mitm.allow_unconstrained_ca`: bool; default `false`.
  When `true`, auto-generation produces a CA without RFC
  5280 Name Constraints if the regex cannot be translated
  (or is empty). When `false` (default), auto-generation
  refuses such configurations at startup with
  `mitm_ca_unconstrained_refused` Error per §5.1.1. Has
  no effect on the operator-supplied-CA path.
- `tls_mitm.ca_cert` / `tls_mitm.ca_key`: must both be set
  or both empty. Set: both files must exist, parse, and the
  cert/key must match (`x509.Certificate.PublicKey ==
  PrivateKey.Public()`); the cert must have
  `BasicConstraints: CA:TRUE`; the cert's `not_after` must be
  in the future; **the key type must be one of the supported
  signing algorithms** (ECDSA P-256, ECDSA P-384, RSA-2048,
  RSA-3072, RSA-4096) per §5.1.3 — startup rejects
  unsupported key types (e.g. Ed25519, ECDSA P-521, RSA
  smaller than 2048, DSA) with a config-error log line
  naming the rejected algorithm. Empty: `tls_mitm.ca_storage_dir`
  (or its default `<cache_dir>/ca`) must be creatable; the
  auto-generator produces ECDSA P-256, which §5.1.3 always
  accepts.

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
  `enabled = true` AND, in the last 30 minutes of process
  uptime, **at least one CONNECT attempt was observed AND
  zero of them resulted in a successful TLS handshake**.
  Both conditions must hold; "no CONNECTs at all" does NOT
  fire the warning. The signal is "clients are trying to
  use MITM but failing", not "no clients have shown up
  yet" — a quiet deployment (e.g. a freshly-deployed cache
  before any client `apt-get update` cycle has run, or a
  weekend with no fleet activity) must NOT false-alarm.
  Fired once per uptime hour from the §9.7.6 refresher
  goroutine.

  Implementation: counters of `mitm_connect` outcomes
  observed in a rolling 30-minute window, partitioned into
  "successful TLS handshake reached" (`tunneled`,
  `inner_method_rejected`, `inner_header_timeout`,
  `inner_header_too_large`, plus `inner_stream_failed` only
  when emitted post-handshake — TLS itself worked) and
  "TLS-failure" (`tls_failed`, `tls_handshake_timeout`,
  `cert_gen_failed`). Pre-TLS rejections (`bad_target` /
  `bad_host` / `bad_port` / `ip_literal_host` /
  `denied_host`, plus pre-handshake `inner_stream_failed`
  from hijack / write-200 / flush / set-deadline failures)
  do NOT count toward either bucket — they are configuration
  / client / I/O errors that arrive before the
  CA-distribution question. The pre-vs-post split for
  `inner_stream_failed` rides on the `tlsReached` flag
  passed at each call site; see `classifyOutcome` in
  `internal/proxy/connect_stats.go`. Warn fires when
  `tls_failure_count >= 1 AND tls_success_count == 0` over
  the window.

  Surfaces "operator turned on MITM but the CA is not yet
  trusted by any client" as an operationally-visible
  signal rather than apt failures buried in client logs.
- `tls_mitm_narrowing_regex_set` Info at startup when
  `tls_mitm.allowed_host_regex` is non-empty. Names the MITM
  regex and the full `upstream.allowed_host_regex` list (one
  item per entry — the upstream allowlist is a list, not a
  single regex; see §5.1.2). Reminds the operator that the
  §5.1.2 predicate is the conjunction of a literal-host
  signing gate (single regex) and a canonical-host fetch
  gate (any-match across the upstream list) and that the
  operator is responsible for verifying the relationship —
  regex-subset is undecidable, so the daemon cannot check
  it. A misconfigured broader-than-upstream MITM regex
  surfaces at
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
short: `r.RequestURI = "https://<literal_host><inner_request_uri>"`,
`r.Method = inner method`, `r.Host = literal_host`,
`r.Body = http.NoBody`, `r.Context()` derived from the outer
CONNECT, `r.RemoteAddr` = the outer CONNECT's client address
(the cache does not NAT or hide the originating address).
The inner request's wire-form request-target is preserved
verbatim — query strings and fragments are NOT stripped at
dispatch. The parser (`internal/proxy/url.go:46-51, 77-82`)
rejects both classes; an inner GET whose path carries a
query string returns 400 on the inner stream with the
handler's `bad_request` outcome — identical shape to a
malformed plain GET. Stripping at dispatch would silently
alias `/Packages?x=1` to `/Packages` in the cache; the
single rejection point in the parser is load-bearing.

The synthetic request enters `Handler.ServeHTTP` — Phase 6
adds no new handler entry point. The CONNECT-side `RequestURI`
construction is the integration seam.

### 6.2.1 MITM context marker (logger integration)

`Handler.logRequest` (`internal/handler/handler.go:1689`) is
the single point that emits the per-request `request` log
line and the per-request metric observations. Phase 5 had
no concept of MITM, so neither `logRequest` nor any of its
~20 call sites carry an MITM signal. Phase 6 adds the §10.1
`mitm` log field; the integration contract is:

1. **Sentinel.** A package-private context-key
   `mitmCtxKey` is declared in
   `internal/proxy/connect.go:720`. Its concrete type is
   an unexported empty struct (`type mitmCtxKey struct{}`)
   so the key cannot collide with any external context
   value. Ownership lives in the proxy package — the
   handler imports proxy (one-way), avoiding the circular
   import that would arise if the marker API lived in
   `internal/handler`.
2. **Setter.** The CONNECT handler attaches the marker on
   the synthetic inner request's context before dispatch
   (see `internal/proxy/connect.go:620`):
   `synth = synth.WithContext(WithMITMContext(synthParent))`.
   `proxy.WithMITMContext` (defined at
   `internal/proxy/connect.go:727-729`) is the exported
   helper that wraps a parent context; the key itself
   stays unexported.
3. **Reader.** `logRequest` calls
   `proxy.IsMITMContext` (defined at
   `internal/proxy/connect.go:735-740`) once near the end
   of the function and CONDITIONALLY appends
   `"mitm", true` to the slog attrs only when the context
   carries the marker (see
   `internal/handler/handler.go:1780-1782`). The 20+
   existing call sites do NOT change — `logRequest`'s
   signature is unchanged, and the marker is read off the
   request context the same way `r.RemoteAddr` is.
4. **Plain requests** (no CONNECT origin) carry no marker
   value, so `proxy.IsMITMContext` returns false and the
   log line OMITS the `mitm` key entirely (no
   `"mitm", false` is appended). The absence of the key
   is the negative signal — operators
   `grep '"mitm":true'` to select MITM-tunneled requests
   without needing to filter false cases. See §10.1 for
   the operator-facing description.

Why a context value rather than a `logRequest` parameter:
the parameter approach would require touching every call
site (≥20 lines per the grep at `handler.go`), expanding
the Phase 6 surface area into the §6.1 / §6.4 / §6.5
handler-internal paths Phase 6 otherwise does NOT modify.
A context marker localizes the change to one read site
(`logRequest`) and one write site
(`internal/proxy/connect.go:620`).

This contract is implementation-binding: the §12.2
integration test
(`TestLogRequest_MITMField_PresentForMITMContext` in
`internal/handler/connect_integration_test.go:97`) asserts
that an inner GET dispatched from a CONNECT emits a
`request` log line with `mitm: true`, AND that a plain
proxy GET on the same listener emits a line that OMITS
the `mitm` key entirely (the test fails on
`"mitm":false` as much as on `"mitm":true` for plain
requests). Other handler-internal call paths are not
allowed to wrap the request in a context that strips this
value (the existing handler does not — it uses
`r.Context()` only as a read source for cancellation,
never re-wraps it).

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

### 9.2.1 Inner-request header read budget and byte cap

After the TLS handshake completes, the §2.2 step 6 inner
request is read from the encrypted stream. Two independent
bounds apply:

**Time bound.** Full HTTP request line and headers must be
delivered within `10s` (counted from `time.Now()` after
handshake completion, NOT from the original CONNECT
receipt). Beyond budget, the conn is closed with a
`mitm_connect` Warn (`outcome=inner_header_timeout`).
Implementation: `tls.Conn.SetReadDeadline(now + 10s)`
before the inner request is read; clear or reset the
deadline once the headers are parsed and dispatch begins.

This budget protects against a client that completes the
TLS handshake and then slow-loris on the inner request
(byte-trickle the request line, never deliver the final
`\r\n\r\n`). 10s is generous for a healthy client whose
inner request is one line of method/path plus a handful of
short headers, but tight enough to prevent a hostile
client from indefinitely tying up a hijacked goroutine plus
TLS state.

**Byte bound — request line + headers must fit in
`64 KiB`.** Hijacked conns bypass `http.Server`'s
`MaxHeaderBytes` (default 1 MiB) protection because the
hijack moves the conn out from under the server framework
entirely. Without an explicit cap, a hostile client that
*does* feed bytes within the 10s budget — but feeds many
megabytes of header padding — can exhaust the goroutine's
read buffer and the heap allocations that back it. Phase 6
imposes a 64 KiB cap on the combined request-line + header
section (matching the spirit of apt-cacher-ng's traditional
limits and well above any legitimate apt request, which is
typically < 4 KiB total). Implementation:
`io.LimitReader(tlsConn, 64*1024)` wrapping the
`bufio.Reader` used by `http.ReadRequest`; if the limit is
reached before the `\r\n\r\n` separator is parsed, the
conn is closed with a `mitm_connect` Warn
(`outcome=inner_header_too_large`).

The cap applies only to the request preamble. The request
**body** (always `http.NoBody` per §2.2 step 7 because
inner method is constrained to GET / HEAD) carries no
bytes the cache reads from the encrypted stream — the
upstream fetch consumes the response, not the request body.

Both bounds together: a client has at most 10 seconds to
deliver at most 64 KiB of well-formed HTTP request
preamble, otherwise the conn closes with the appropriate
outcome.

The body of the inner request response (the upstream
fetch) is bounded by the existing Phase 1
`upstream.connect_timeout` and friends (§9.3); the
header budget and byte cap are the new bounds Phase 6
introduces because Phase 5 had no path that read an HTTP
request from a hijacked conn.

### 9.3 Inner request budget

The inner GET inherits the existing
`upstream.connect_timeout`, `upstream.total_timeout`, and
`upstream.idle_read_timeout` — Phase 6 adds no new
inner-request budget beyond what Phase 1 set.

### 9.4 In-flight CONNECT tunnels at shutdown

**Hijacked-conn fact.** Per Go's `net/http`
documentation, `http.Server.Shutdown` does **NOT** close
or wait for hijacked connections — once a handler hijacks
a conn (via `http.Hijacker`), that conn becomes the
caller's responsibility for the rest of its lifetime.
Phase 6 must therefore introduce an **explicit tunnel
manager** to bridge that gap; otherwise CONNECT tunnels
could outlive Shutdown and race the daemon's cache /
handler teardown. Phase 5 had no hijacked conns and so
needed nothing equivalent.

**Tunnel manager (NEW, Phase 6).** A package-private
struct living alongside the CONNECT handler in
`internal/proxy/connect.go`. It owns:

- A `sync.WaitGroup` whose counter is the number of
  in-flight tunnels (incremented on successful hijack,
  decremented from a `defer` in the tunnel goroutine).
- A `context.Context` derived from the daemon's lifecycle
  context (cancelled by the daemon's Shutdown step), and
  passed into each tunnel goroutine as the parent context
  for the synthetic inner request and any handshake
  deadlines.
- A registry (`map[net.Conn]struct{}` guarded by a
  `sync.Mutex`) so that on shutdown-deadline expiry the
  manager can forcibly `conn.Close()` every still-tracked
  conn, unblocking any goroutine wedged in a Read / Write
  / TLS handshake.

**Graceful-shutdown sequence** (extends SPEC4 §9.5 /
SPEC5 §9.5):

1. SIGINT / SIGTERM received. The daemon constructs
   `shutdownCtx, shutdownCancel := context.WithTimeout(
   ctx, drainBudget)` (existing Phase 5 mechanism).
2. Admin listener Shutdown (Phase 5 §9.5).
3. Plain + TLS proxy listeners concurrent Shutdown.
   `http.Server.Shutdown` stops accepting new connections
   and waits for active **non-hijacked** handlers to
   return. It does NOT wait for hijacked CONNECT tunnels.
4. **Tunnel manager drain.**
   1. The manager's parent context is cancelled. This
      propagates to every tunnel's inner request context
      (since each tunnel derives its synthetic request
      ctx from this parent), short-circuiting the inner
      ResponseWriter side and any handler-internal
      `r.Context()` reads.
   2. `h.lifecycleCtx` (the leader-fetch context;
      `internal/handler/handler.go:121, 145`) is
      cancelled synchronously by the daemon's Shutdown
      step — this propagates to in-flight upstream
      fetches in `serveCacheMiss` (handler.go:849-857).
   3. The daemon waits on the manager's `WaitGroup`,
      bounded by `shutdownCtx`. Either every tunnel
      finishes (graceful) or the deadline fires.
   4. On deadline expiry, the manager iterates its
      registry under the mutex and calls `conn.Close()`
      on every still-tracked conn. This unblocks any
      goroutine that was wedged in `tls.Handshake()`,
      `bufio.Read`, etc., causing them to error and
      release the WaitGroup. The daemon then waits a
      bounded grace period (≤ 1s) for the WG to drain to
      zero before proceeding regardless.
5. Refresher goroutine + GC + cache.Close — same as
   Phase 5.

The two-context design (manager parent ctx vs
`lifecycleCtx`) preserves the Phase 1 invariants:

- **Client disconnect during a leader's cache miss does
  NOT kill the leader fetch.** The synthetic request's
  context cancels (because the underlying conn detected
  client close), but `lifecycleCtx` does not. The leader
  finishes the fetch and writes the cache; later requests
  hit the now-populated entry. (See §12.3 chaos test.)
- **Shutdown DOES kill leader fetches.** `lifecycleCtx`
  cancels independently when the daemon's shutdown step
  runs, and `serveCacheMiss` honors that.

**Why two contexts, not one.** A single ctx that "cancels
on client disconnect AND on shutdown" would conflate the
two cases and break the leader-fetch-survives invariant.
The tunnel manager's parent ctx is the cancellation
fanout for shutdown only; client-disconnect propagation
arrives via the standard `http.Conn`-detected path the
synthetic request inherits, which does NOT cancel
`lifecycleCtx`.

This contract is implementation-binding: the §12.2
integration suite asserts the WG drains within the
shutdown deadline; the §12.3 chaos suite asserts a wedged
TLS handshake is force-closed at deadline expiry; the
§12.3 leader-fetch-survives test asserts client
disconnect does NOT cancel the leader fetch even on a
heavily-shut-down-adjacent path.

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
`mitm` (bool, value `true`) ONLY on inner-GETs dispatched
from a CONNECT tunnel. Plain (non-MITM) requests do NOT
emit the field — the absence of the key is the negative
signal, so operators can `grep '"mitm":true'` without
filtering `false` cases. The signal source is the §6.2.1
context-marker contract: `logRequest` (see
`internal/handler/handler.go:1780-1782`) calls
`proxy.IsMITMContext(r.Context())` once and conditionally
appends the `mitm` attr only when the context carries the
marker. No `logRequest` call site is modified. All other
fields unchanged.

### 10.2 New `mitm_*` event family

Emitted from the §9 CONNECT handler:

- **`mitm_connect`** — once per CONNECT, at conn close.
  Fields: `host` (lower-cased + IDNA-normalized via
  `ParseConnectTarget`; empty string when parsing failed
  before the host step, e.g. `bad_target`), `port`
  (numeric when parsing reached the port step; otherwise
  `0`), `client_addr`, `outcome` (`tunneled` /
  `denied_host` / `bad_target` / `bad_host` / `bad_port` /
  `ip_literal_host` / `tls_handshake_timeout` /
  `tls_failed` / `cert_gen_failed` /
  `inner_method_rejected` / `inner_header_timeout` /
  `inner_header_too_large` / `inner_stream_failed`),
  `denied_gate` (`signing` / `fetch`; field is OMITTED
  unless `outcome=denied_host` — `warnConnect` only adds
  the key when its `deniedGate` argument is non-empty,
  see `internal/proxy/connect.go:844-846`; identifies
  which §5.1.2 gate rejected the host), `duration_ms`,
  `reason` (free-form diagnostic — the underlying error
  text or rejection cause; field is OMITTED on `tunneled`
  and present on every other outcome, see
  `internal/proxy/connect.go:841-843` and the
  `warnConnect`/`infoConnect` call sites at lines
  446-633). Emitted at level Info on `tunneled`; Warn on
  every other outcome.

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

- **`mitm_ca_load_failed`** — at startup, when §4.2 case 3
  fires. Case 3 is precisely "any state where at least one
  daemon-managed real file (`ca.crt`, `ca.key`, `ca.ready`)
  is present but the trio is not self-consistent" — i.e.
  one or more real files exist without their committed
  siblings, or the marker fingerprint disagrees with
  `sha256(ca.crt)`, or a real file fails to parse, or the
  cert/key pair mismatches. A directory containing only
  `*.tmp` residue (no real files committed) is NOT case 3 —
  that is case 1 with cleanup under the lock (§4.2 case 1,
  §4.2.1 step 3). Fields: `path`, `err`. Level Error. The
  daemon does not bind after this event.

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
  racing on the same storage dir. Recovery: identify the
  holding process via `lsof <path>` or `fuser <path>`,
  stop that process (graceful preferred), then retry. Do
  NOT `rm` the lockfile — see §4.2.2 for why.

- **`mitm_ca_name_constraints_skipped`** — at startup, when
  the regex-to-NameConstraints translation cannot safely
  produce constraints AND the operator has opted in via
  `tls_mitm.allow_unconstrained_ca = true`. Fields:
  `regex`, `reason`. Level Warn. The CA is generated without
  Name Constraints; runtime signing is still gated by §5.1.2
  but a stolen CA key is cryptographically unconstrained
  (§ trust-anchor expansion note).

- **`mitm_ca_unconstrained_refused`** — at startup, when the
  regex-to-NameConstraints translation cannot safely produce
  constraints AND the operator has NOT opted in via
  `tls_mitm.allow_unconstrained_ca`. Fields: `regex`,
  `reason`. Level Error. Followed immediately by a
  `mitm_ca_generation_failed` Error so the operator sees
  both the cause and the effect. The daemon does not bind.
  Recovery: narrow the regex to a translatable shape (§5.1.1.1),
  set `allow_unconstrained_ca = true` to consciously accept
  the unconstrained-CA posture, or supply a CA out of band.

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

JSON shape (see `internal/admin/status.go:46-69`), full
payload when `tls_mitm.enabled = true`:

```json
{
  "enabled": true,
  "ca_source": "generated|supplied",
  "ca_fingerprint_sha256": "<hex, 64 chars>",
  "ca_not_after_unixtime": <int64 Unix seconds>,
  "effective_allowlist": "<regex string>",
  "cert_cache": {"size": <int>, "capacity": <int>},
  "last_cert_issued": {"host": "<literal CONNECT host>", "at_unixtime": <int64>} | null,
  "cert_hit_rate_60s_percent": <float 0-100> | null,
  "cert_hit_rate_60s_observed": <int>
}
```

`last_cert_issued` is JSON `null` until the first cert
is issued. `cert_hit_rate_60s_percent` is `null` when no
cert-cache lookups occurred in the 60s window; the
companion `cert_hit_rate_60s_observed` field is the
(hits + misses) sample size — surfaced in the JSON so
consumers can distinguish "0% of 800 lookups" from "no
data". `ca_not_after_unixtime` is an integer Unix-seconds
value; the HTML form of the page renders this as a UTC
timestamp.

The JSON form of the status page (§10.5 schema) carries a
top-level `tls_mitm` key — always present, abbreviated to
`{"enabled": false}` when MITM is disabled, full payload
otherwise (see the `MarshalJSON` short-circuit at
`internal/admin/status.go:76-79`). This mirrors the
Phase 5 SPEC §10.5 invariant that top-level keys are
stable.

---

## 11. Failure-mode catalog (delta over SPEC §11 / SPEC4 §11 / SPEC5 §11)

| # | Failure | Behavior |
|---|---------|----------|
| F1 | `tls_mitm.enabled = true` but operator-supplied CA file unreadable at startup | Startup fails with config-error log; daemon does not bind |
| F2 | Auto-generated CA directory in any non-clean state (any of: `ca.ready` missing while either `ca.crt` or `ca.key` is present; `ca.ready` present without `ca.crt`/`ca.key`; `ca.ready`'s fingerprint disagrees with `sha256(ca.crt)`; either `ca.crt` or `ca.key` fails to parse; cert/key pair mismatch). NOTE: a `*.tmp`-only state (no real files committed) is NOT case 3; it is case 1 with tmp cleanup under the §4.2.2 lock. | Startup logs `mitm_ca_load_failed` Error naming the offending state; daemon does not bind. Operator recovers by removing the daemon-managed real files (`ca.crt`, `ca.key`, `ca.ready`) within `<ca_storage_dir>` (or the entire dir) to force case 1 regeneration on next start. Silent regeneration would change the trust anchor under every client; trust-root replacement is operator-explicit (§4.2 case 3) |
| F3 | `cache_dir/ca/` cannot be created (perms, disk full) OR §4.2.1 mid-write failure | Startup fails with `mitm_ca_generation_failed` Error; daemon does not bind. The §4.2.1 marker-file scheme guarantees that mid-write crashes leave the directory in §4.2 case 3 (uninitialized) — `ca.ready` is the commit primitive and is written last; absence of `ca.ready` means the trust anchor is NOT yet adopted regardless of which `*.tmp` or `ca.{crt,key}` files happen to be on disk |
| F4 | CA cert / key mismatch (operator-supplied) | Startup fails with config-error log naming the mismatch. The match check is `x509.Certificate.PublicKey == PrivateKey.Public()` |
| F5 | Operator-supplied CA cert `not_after` already in the past, OR not in the future at all | Startup fails with config-error log naming the expiry timestamp |
| F6 | Operator-supplied CA cert lacks `BasicConstraints: CA:TRUE` | Startup fails with config-error log |
| F6a | Operator-supplied CA key type is not in §5.1.3's accepted set (ECDSA P-256/P-384, RSA 2048/3072/4096) | Startup fails with config-error log naming the rejected algorithm; daemon does not bind |
| F7 | Leaf cert generation fails (entropy exhaustion, key gen panics, etc.) | CONNECT closes with `mitm_connect` Warn (`outcome=cert_gen_failed`); the singleflight returns the error to all blocked waiters; the next CONNECT on the same host retries (the singleflight does NOT cache failures) |
| F8 | Cert cache full + new host requested | LRU eviction; `mitm_cert_cache_evicted` Info on the evicted entry; new cert inserted; `acu_mitm_cert_evicted_total{reason="lru"}` increments |
| F9 | TLS handshake on hijacked conn fails (client distrusts CA, TLS-version mismatch, cipher mismatch) | Tunnel closes with `mitm_connect` Warn (`outcome=tls_failed`); apt logs a TLS verification error; `tls_mitm_enabled_ca_undistributed` Warn fires from the refresher when this is the steady state |
| F10 | TLS handshake exceeds the §9.2 budget | Conn closed with `mitm_connect` Warn (`outcome=tls_handshake_timeout`) |
| F11 | Inner request method is not GET/HEAD | 405 written on the inner stream with `Allow: GET, HEAD`; tunnel closes; `mitm_connect` Warn (`outcome=inner_method_rejected`) |
| F11a | Inner request headers not fully received within §9.2.1's 10s budget after TLS handshake (slowloris on the inner request) | Conn closed; `mitm_connect` Warn (`outcome=inner_header_timeout`). No 408 / 4xx is written on the inner stream — partial-header state means there is no committed protocol context to write into |
| F11b | Inner request preamble (request line + headers) exceeds §9.2.1's 64 KiB cap before the `\r\n\r\n` separator is parsed (megabyte-headers DoS via fast bytes) | Conn closed; `mitm_connect` Warn (`outcome=inner_header_too_large`). No 4xx is written; the cap is enforced in the byte-source via `io.LimitReader`, which surfaces as a read error before any HTTP framing is parsed |
| F11c | CONNECT pipeline plumbing failure — ResponseWriter does not implement Hijacker, hijack returns an error, CONNECT 200 response write/flush error, or `conn.SetDeadline` fails before the TLS handshake or before the inner-request read | Conn closed; `mitm_connect` Warn (`outcome=inner_stream_failed`). The `reason` field carries the underlying error (e.g., `"hijack: ..."`, `"write 200: ..."`, `"flush 200: ..."`, `"set deadline: ..."`, `"set inner deadline: ..."`). Six emission sites in connect.go cover the lifecycle stages: pre-hijack (line 470 ResponseWriter-not-Hijacker, line 475 hijack error), CONNECT 200 response (line 492 write, line 496 flush), pre-TLS deadline (line 532), post-TLS / pre-inner-read deadline (line 564). The `tlsReached` argument passed to `warnConnect` (NOT emitted as a log field; see connect.go:826-831 docstring) drives the §9.7.6 rolling counter's pre/post-TLS sub-classification |
| F12 | CONNECT to a port other than 443 | 400 on the CONNECT response; tunnel closes; `mitm_connect` Warn (`outcome=bad_port`) |
| F13 | CONNECT to an IP-literal host (IPv4 or IPv6) | 400 on the CONNECT response; tunnel closes; `mitm_connect` Warn (`outcome=ip_literal_host`) |
| F13a | CONNECT request-target structurally malformed (missing port, empty host, non-numeric port, port out of range, multi-colon, unbracketed IPv6, etc. — see §2.2 step 1 enumeration) | 400 on the CONNECT response; tunnel closes; `mitm_connect` Warn (`outcome=bad_target`) |
| F13b | CONNECT host fails IDNA normalization or contains invalid DNS labels | 400 on the CONNECT response; tunnel closes; `mitm_connect` Warn (`outcome=bad_host`) |
| F14 | CONNECT host fails the §5.1.2 effective allowlist (literal host fails signing predicate, OR canonical host fails fetch predicate) | 403 on the CONNECT response; tunnel closes; `mitm_connect` Warn (`outcome=denied_host`, `denied_gate=signing` or `fetch`). No cert is issued, no TLS handshake |
| F15 | Inner GET upstream fetch fails the SSRF deny-range gate at TCP-connect time | Inner GET fails with 403 + `outcome=forbidden` (handler.go:1535-1542 maps `fetch.ErrTargetDenied` → 403); the request log line carries `fetchAttempted=true` and `upstream_status=0`, distinguishing this from the pre-flight host rejection (handler.go:1525-1534, `fetchAttempted=false` so `upstream_status` is omitted). Tunnel closes after the inner response (CONNECT handler reads exactly one inner request; no Keep-Alive loop, see `http.ReadRequest` at connect.go:568 and the single `Dispatch` at connect.go:629, with the explicit "Phase 6 does NOT support multi-request keepalive" comment at connect.go:631-632). The CONNECT itself succeeded (as designed; §1.1.3) |
| F16 | Daemon shuts down during an in-flight CONNECT tunnel | The §9.4 tunnel manager (sync.WaitGroup + conn registry + parent context) drains tunnels on shutdown. `http.Server.Shutdown` does NOT wait for hijacked conns (Go's stdlib contract); the manager fills that gap. Two cancellation paths fan out: the manager's parent ctx (cancels each tunnel's synthetic request ctx) and `h.lifecycleCtx` (cancels in-flight leader fetches in `serveCacheMiss`). On shutdown-deadline expiry, the manager iterates its conn registry and force-closes every still-tracked conn, unblocking any goroutine wedged in TLS handshake / Read / Write. Note: a CLIENT close mid-response cancels only the inner request's context — the leader fetch survives until lifecycleCtx cancels (Phase 1 invariant; see §12.3 chaos test) |
| F17 | Clock skew: leaf cert `not_before` is in the future | Apt rejects with a `not yet valid` TLS error; cache's clock is the source of truth — operators should run NTP. No Phase 6 mitigation beyond logging on the cache side |
| F18 | CA expires mid-lifetime | All client TLS handshakes fail; `mitm_connect` Warn `outcome=tls_failed` rate spikes; operator's `acu_mitm_ca_not_after_unixtime` alert (set to fire 30 days before expiry) catches this before the spike |
| F19 | CA private key file ownership / mode wrong on disk (e.g. world-readable) | Phase 6 does NOT enforce mode/ownership at startup beyond the §4.2.1 atomic-write guarantees on the auto-generated path. Operator-supplied CA paths are read with whatever mode the operator chose; `apt-cacher-ultra ca print` is the audit primitive (it warns on `S_IROTH` set on `ca.key`). A future phase can add mandatory mode enforcement if any deployment asks |
| F20 | Leaf cert algorithm config invalid at startup | Startup fails with config-error log naming the rejected value |
| F21 | Upstream HTTPS server presents an invalid cert (chain failure, expired, hostname mismatch) | Inner GET fetch fails with the existing Phase 1 fetcher behavior — `outcome=bad_gateway` on the inner request log line; the cache does NOT relax verification (§5.4) |
| F22 | Upstream sends a redirect from `https://` to `http://` (or any other 3xx) | Inner GET fails with `outcome=bad_gateway` (handler.go:1618-1627 maps `fetch.ErrRedirectBlocked` → 502). The upstream's 3xx status code is preserved on the request log line as `upstream_status` (see line 1626's logRequest with `fetchAttempted=true`, `res.status` carrying the redirect code). The cache does NOT silently follow the redirect or downgrade the inner request to HTTP; apt sees a 502 from the cache rather than the 3xx from upstream. Operators whose archive uses redirects configure a Remap rule pointing at the redirect target |
| F23 | §4.2.2 interprocess lock contention exceeds 30s | Startup logs `mitm_ca_lock_timeout` Error and `mitm_ca_generation_failed` Error; daemon does not bind. `apt-cacher-ultra ca print` exits with code 4. Caused by a concurrent `ca print` or another daemon instance racing on the same `<ca_storage_dir>`. Recovery: identify the holding process (`lsof <ca_storage_dir>/.ca.lock` or `fuser`), stop it, retry. **Do NOT delete the lockfile** — flock is FD-tied, removing the file does not release the lock and can cause split-lock if a new process opens a fresh inode at the same path. See §4.2.2 |
| F24 | Auto-generated CA path: `tls_mitm.allowed_host_regex` is empty OR untranslatable AND `tls_mitm.allow_unconstrained_ca = false` (default) | Startup fails with `mitm_ca_unconstrained_refused` Error and `mitm_ca_generation_failed` Error; daemon does not bind. The fail-closed default exists because an unconstrained auto-generated CA confers fleet-wide trust on arbitrary leaf certs the cache can issue, and the runtime regex gate alone does not bound key-compromise blast radius. Operator either narrows the regex (§5.1.1.1), sets `allow_unconstrained_ca = true`, or supplies a CA out of band |

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
  ranges):
  - With `allow_unconstrained_ca = false` (default):
    startup FAILS with `mitm_ca_unconstrained_refused`
    Error; daemon does not bind (F24).
  - With `allow_unconstrained_ca = true`: CA generated
    without constraints + `mitm_ca_name_constraints_skipped`
    Warn fires with the reason field set.
  Empty regex behaves the same way: refused at default,
  permitted only with the explicit opt-in.
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
  past not_after (F5), missing CA:TRUE (F6), unsupported
  key type (Ed25519, ECDSA P-521, RSA-1024) (F6a),
  unreadable file (F1) each fail at config validation with
  the right error. Accepting branches: ECDSA P-256, ECDSA
  P-384, RSA-2048, RSA-3072, RSA-4096 each load and the
  daemon's signing algorithm matches the §5.1.3 mapping
  (`ECDSAWithSHA256` for P-256, `ECDSAWithSHA384` for P-384,
  `SHA256WithRSA` for any RSA size).
- Leaf algorithm config: `"ecdsa-p256"` and `"rsa2048"`
  accepted; everything else rejected (F20).

In `internal/proxy/`:

- CONNECT handler: bad port → 400 (F12).
- CONNECT handler: IP-literal host → 400 (F13).
- CONNECT handler: bad target — table-driven test
  enumerating empty target, missing port, empty host
  (`:443`), empty port (`host:`), non-numeric port,
  port out of range, multi-colon (`host:443:extra`),
  unbracketed IPv6 (`::1:443`); each produces 400 with
  `outcome=bad_target` (F13a).
- CONNECT handler: bad host — IDNA `Lookup.ToASCII`
  failure (oversized label, oversized total name,
  invalid Unicode), invalid DNS label (illegal
  characters after IDNA, leading/trailing hyphen);
  each produces 400 with `outcome=bad_host` (F13b).
- CONNECT handler: trailing-dot canonicalization —
  CONNECT to `apt.example.com.:443` and
  `apt.example.com:443` produce a single cert cache
  entry under the un-dotted form.
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
- CONNECT handler: inner-request header slowloris —
  client completes handshake then trickles the inner
  request bytes; the §9.2.1 deadline fires and the conn
  closes with `outcome=inner_header_timeout` (F11a).
- CONNECT handler: inner-request header oversize —
  client completes handshake then sends 128 KiB of
  header padding within the time budget; the §9.2.1
  byte cap fires (`io.LimitReader` exhaustion before
  `\r\n\r\n`) and the conn closes with
  `outcome=inner_header_too_large` (F11b).
- CONNECT handler: hijack-conn lifecycle (sync.WaitGroup
  increments on hijack, decrements on close).

### 12.2 Integration tests

Under `internal/proxy/` with a real net.Listen + apt-style
TCP client:

- End-to-end CONNECT + inner GET with a synthetic upstream
  (`httptest.NewTLSServer`). The system trust store does NOT
  trust httptest's auto-generated cert, so the test installs
  the test server's `Certificate()` into a custom
  `*x509.CertPool` and supplies it to the fetch client via
  an **exported, test-only helper in the `fetch` package**:

  ```go
  // fetch.SetRootCAsForTest sets the upstream-TLS trust pool
  // on Options for integration tests. Calling this from
  // non-test code is a programming error (the function panics
  // if called outside a test context, detected via testing.Testing()).
  func SetRootCAsForTest(opts *Options, pool *x509.CertPool)
  ```

  Why an exported helper rather than a package-private field:
  the existing `dialContext` and `now` seams
  (`internal/fetch/fetch.go:107-114`) are package-private and
  ONLY usable from `internal/fetch`'s own `*_test.go` files
  (e.g. `unreachable_test.go:120`). A Phase 6 integration test
  for the CONNECT handler lives in `internal/proxy/` and
  cannot legally set a private field on a different package's
  struct. The test-only setter lives in `fetch` package, takes
  a pointer to `Options`, and is called from any test that
  needs to bypass the system trust store. Naming convention
  `*ForTest` suffix signals intent; the runtime
  `testing.Testing()` panic guard prevents accidental
  production use.

  Production code never calls `SetRootCAsForTest` — the
  unset field keeps the Go default of system trust roots,
  preserving the §5.4 invariant that production upstream TLS
  uses ONLY the system trust store. The setter is NOT
  exposed via config.

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
- `acu_requests_total{outcome=forbidden}` increments when a
  denied-target-range deny fires inside the inner GET (F15
  end-to-end). The same outcome label fires on host-allowlist
  rejection; the deny-range path is distinguished by
  `upstream_status` field presence on the request log line
  (set when `fetchAttempted=true`).

Under `e2e/`:

- A Docker-Compose rig (`e2e/run.sh` + `e2e/docker-compose.yml`)
  exercises the full request path end-to-end. Three services on a
  private bridge network: an `upstream` nginx serving a baked-in
  apt repo (`hello-acu_1.0`) built by `e2e/upstream/build-repo.sh`,
  a `cache` apt-cacher-ultra container built from the current source
  tree, and a `client` ubuntu:noble container that runs `apt-get
  update && apt-get install hello-acu` through the cache (HTTP proxy
  mode via `/etc/apt/apt.conf.d/00proxy`). The rig is HTTP-only —
  it does NOT exercise the MITM CONNECT path; that surface is covered
  by §12.1 unit tests + §12.2 in-process integration tests on
  `httptest.NewTLSServer`. First `apt-get update` warms metadata,
  the install drives a `.deb` MISS, the second `apt-get update`
  re-fetches metadata against the now-warm cache. The driver script
  fails unless `docker compose logs cache` contains at least one
  `outcome=hit` log line on a slog key=value boundary (anchored
  regex rejects `outcome=hit_stale` / `outcome=hit_coalesced`,
  which are legal §10 outcomes that do not prove a fresh cache hit).
  Run via `bash e2e/run.sh`; on failure the cleanup trap dumps
  upstream/cache/client logs before tearing down volumes.

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
- **Shutdown during wedged TLS handshake.** Open a
  CONNECT, send the bytes for `CONNECT host:443 HTTP/1.1`,
  then deliberately stall (do NOT send a TLS ClientHello).
  Trigger SIGTERM with a 2s shutdown drain budget. Verify
  the §9.4 tunnel manager (a) cancels the tunnel's parent
  context, (b) waits up to the budget for the WG to
  drain, (c) on budget expiry iterates its conn registry
  and force-closes the wedged conn, (d) the tunnel
  goroutine exits cleanly within ≤ 1s of the force-close,
  (e) `cache.Close()` runs only after the WG is at zero,
  (f) no leaked goroutines per `runtime.NumGoroutine`
  delta. Then repeat with a tunnel that completed TLS
  but is hung mid inner-request body — same expected
  outcome (force-close at deadline, clean drain).
  (F16 end-to-end.)
- **Inner GET cancelled mid-stream — leader fetch survives.**
  Client closes the TLS tunnel mid-response while a leader
  cache-miss fetch is in progress. Verify the existing
  Phase 1 leader-fetch-survives invariant
  (`internal/handler/handler.go:849-857` —
  `h.serveCacheMiss` runs under `h.lifecycleCtx`, NOT
  `r.Context()`): the upstream fetch continues, the cache
  blob finalizes, and a SECOND CONNECT to the same host
  immediately hits the now-populated cache. The cancelled
  request's ResponseWriter side detects the client close
  (Go's `http.Server` already propagates this through
  `r.Context()`), but the leader fetch is unaffected. No
  orphan blob in `pool/`. Asserts that Phase 6 does NOT
  introduce a new "cancel upstream on client close"
  semantic — the synthetic request's context governs only
  the inner ResponseWriter side.

  Separately verify: a graceful-shutdown SIGTERM during
  the same in-flight fetch DOES cancel the leader (because
  `h.lifecycleCtx` cancels at Shutdown), and the cache
  rolls back per the SPEC2 atomic-finalize contract — no
  orphan blob.
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
  at each numbered step of §4.2.1. Restart and verify:
  - §4.2 case-2 "load and use" fires only when `ca.ready`
    is committed AND its fingerprint matches `sha256(ca.crt)`
    AND both files parse AND cert/key match.
  - §4.2 case-1 "generate" fires for the no-real-files
    state — including the `*.tmp`-only sub-state where a
    prior crash left residue but committed no trust
    anchor; the residue is cleaned by §4.2.1 step 3 under
    the lock, then a fresh CA is generated. Verify NO
    silent adoption of any tmp file as a trust anchor.
  - §4.2 case-3 `mitm_ca_load_failed` fires for every
    inconsistent committed state: `ca.crt` only;
    `ca.crt + ca.key` without `ca.ready`; `ca.ready`
    only; `ca.ready` with mismatching fingerprint;
    either real file failing to parse; cert/key
    mismatch. The daemon must NOT bind; operator must
    explicitly clear.

  Two invariants must hold across every kill point:
  1. NEVER silent regeneration of a new trust anchor
     under a fleet that already trusts an old one.
  2. NEVER silent adoption of `ca.crt + ca.key` whose
     marker file was not written — that state is
     uncommitted and operator-explicit recovery is the
     only path forward.
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
| F6a | 12.1 unit (operator-supplied CA key type validation: rejects Ed25519, ECDSA P-521, RSA-1024, accepts ECDSA P-256/P-384 and RSA 2048/3072/4096; signing alg derived correctly per §5.1.3) |
| F7 | 12.1 unit (singleflight failure path) |
| F8 | 12.3 chaos (cert-cache thundering herd) |
| F9 | 12.2 integration (TLS policy / version mismatch) |
| F10 | 12.1 unit (CONNECT handler handshake timeout) |
| F11 | 12.1 unit (CONNECT handler non-GET/HEAD inner) |
| F11a | 12.1 unit (CONNECT handler inner-header slowloris: completes handshake then trickles the inner request byte-by-byte; 10s after handshake the conn closes with `outcome=inner_header_timeout`) |
| F11b | 12.1 unit (CONNECT handler inner-header oversize: completes handshake then sends 128 KiB of header padding within the 10s budget; conn closes with `outcome=inner_header_too_large` after the 64 KiB byte cap fires) |
| F11c | 12.1 unit (`internal/proxy/connect_stats_test.go` `TestConnectStats_InnerStreamFailedTLSReached` and `TestConnectStats_InnerStreamFailedNoTLS` cover the §9.7.6 pre/post-TLS classification; the per-call-site emission paths are exercised by neighboring CONNECT-handler tests) |
| F12 | 12.1 unit (CONNECT handler bad port) |
| F13 | 12.1 unit (CONNECT handler IP-literal) |
| F13a | 12.1 unit (CONNECT handler bad-target table-driven test: missing port, empty host, non-numeric port, port out of range, multi-colon, unbracketed IPv6) |
| F13b | 12.1 unit (CONNECT handler bad-host: IDNA failure, oversized label, illegal label characters) |
| F14 | 12.1 unit + 12.2 integration (denied host) |
| F15 | 12.2 integration (deny-range fires on inner GET) |
| F16 | 12.2 integration (CONNECT during shutdown — graceful drain) + 12.3 chaos (wedged TLS handshake force-closed at deadline; tunnel manager registry exercises) |
| F17 | 12.1 unit (clock-skew leaf cert; verifies the cache emits `mitm_clock_skew` Warn — apt-side rejection is observable but not unit-testable here) |
| F18 | 12.3 chaos (CA expiry mid-runtime) |
| F19 | 12.1 unit (`apt-cacher-ultra ca print` audit warning on 0644 ca.key) |
| F20 | 12.1 unit (leaf algorithm rejection) |
| F21 | 12.2 integration (upstream invalid cert) |
| F22 | 12.2 integration (HTTPS→HTTP redirect not auto-followed) |
| F23 | 12.1 unit (interprocess flock contention: spawn a goroutine that holds the lock past 30s, verify daemon-side §4.2.1 returns the lock_timeout error and `apt-cacher-ultra ca print` exits with code 4) |
| F24 | 12.1 unit (fail-closed CA: empty regex + default `allow_unconstrained_ca`, untranslatable regex + default, untranslatable + opt-in (proceeds with Warn), translatable + default (proceeds with constraints)) |

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
Acquire::http::Proxy "http://<advertised-host>:<port>";
Acquire::https::Proxy "http://<advertised-host>:<port>";
# When MITM is enabled, the following CA must be installed
# system-wide (e.g. via update-ca-certificates) for HTTPS
# repositories in sources.list to validate:
# CA fingerprint (SHA-256): <hex>
# CA path on the cache host: <path>
```

**Advertised host resolution.** The host and port emitted
in the snippet are NOT a verbatim copy of `cache.listen`
because typical configs bind to `0.0.0.0:3142` or `[::]:3142`
to accept on every interface, and unspecified addresses are
not usable as a client-facing target. Resolution order:

1. If `cache.advertise_host` (new Phase 6 config field, see
   below) is non-empty, use it verbatim. The operator sets
   this to the DNS name or unicast IP that fleet clients
   should target — `cache.example.com`,
   `apt.internal:3142`, etc. May or may not include a
   port; if no port is included, `cache.listen`'s port is
   appended.
2. Otherwise, parse `cache.listen` host:port. If the host
   is a unicast literal (anything except `0.0.0.0`,
   `::`, or empty), use it verbatim — operator's `listen`
   binds to a single addressable interface and the snippet
   is unambiguous.
3. Otherwise (`0.0.0.0`, `::`, empty host), the unspecified
   address is unusable in the snippet. Print a stderr
   diagnostic naming the listen address and `exit 5`. The
   operator must either (a) set `cache.advertise_host` in
   the config, or (b) pin `cache.listen` to a unicast
   address.

**New config field `cache.advertise_host` (Phase 6).** A
string under the `[cache]` block, default empty. Affects
ONLY the `--print-apt-conf` snippet content — it is NOT
read by the daemon's listener bind path, the request
handler, or any URL canonicalization. Validation: if
non-empty, must parse as a host or `host:port` (no scheme,
no path); reject otherwise at startup with a config-error
log line. Empty (default): the snippet falls back to the
`cache.listen` resolution above.

Exit codes:

- `0`: snippet printed.
- `1`: config file unreadable.
- `5`: `cache.listen` is bound to an unspecified address
  (`0.0.0.0` / `::`) and `cache.advertise_host` is unset;
  the snippet would emit a non-routable target. Operator
  sets `cache.advertise_host` (or pins `cache.listen` to a
  unicast address) and retries.

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
