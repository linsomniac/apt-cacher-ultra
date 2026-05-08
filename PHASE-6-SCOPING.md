# apt-cacher-ultra — Phase 6 Scoping

Status: **scoping in progress** (revision 1).
Last updated 2026-05-07. Next artifact: `SPEC6.md` modeled on
`SPEC5.md`'s structure, once the open-question table in §7 is closed
and any further review feedback has been incorporated.

This document gathers what Phase 6 is, the hooks Phases 1–5 left in
place for it, and the design questions that need to be resolved
during this scoping pass before this becomes a locked SPEC6.md
(parallel to SPEC.md / SPEC2.md / SPEC3.md / SPEC4.md / SPEC5.md).
Companion documents [PHASE-2-SCOPING.md](PHASE-2-SCOPING.md),
[PHASE-3-SCOPING.md](PHASE-3-SCOPING.md),
[PHASE-4-SCOPING.md](PHASE-4-SCOPING.md), and
[PHASE-5-SCOPING.md](PHASE-5-SCOPING.md) record the parallel
exercises for earlier phases.

---

## 1. Goals

Phase 1 made the cache-hit path bulletproof. Phase 2 closed the
integrity and freshness loops. Phase 3 closed the service-continuity
loop. Phase 4 closed the storage-reclamation loop. Phase 5 closed
the operator-visibility loop. Phase 6 closes the **HTTPS-upstream
caching loop**:

1. **Native HTTPS upstream caching via TLS MITM.** Phases 1–5 cache
   HTTPS upstreams only via the `http://HTTPS///apt.corretto.aws/`
   URL-prefix convention preserved from apt-cacher-ng (SPEC §2.3).
   That convention works but requires every client to rewrite its
   `sources.list` — a fleet config change that pushes against the
   project's "minimize fleet ansible churn" preference. Phase 6 adds
   a TLS MITM mode where clients keep their natural
   `https://apt.corretto.aws/...` `sources.list` lines and configure
   `Acquire::https::Proxy "http://cache:3142"`. apt sends CONNECT;
   the cache generates a leaf cert for the requested hostname signed
   by an operator-trusted CA, completes TLS with the client, reads
   the inner GET as a normal absolute-URL request, fetches upstream
   over real HTTPS, and serves cached bytes on subsequent requests.

2. **Coexist with the existing `http://HTTPS///` escape hatch.** The
   pre-existing convention continues to work. Operators who don't
   want to distribute a CA to every client can keep the URL-rewrite
   path; operators who do want HTTPS in `sources.list` opt-in to
   MITM with one fleet-wide CA distribution. The two paths are
   independent — they share the same Remap canonicalization (§3.4)
   and the same downstream caching machinery, so a request that
   resolves via either path produces the same canonical
   `(scheme=https, host, path)` cache key and benefits from the same
   blob (no double-caching by path).

3. **Same SSRF / hostname-allowlist posture as Phase 1's plain GET
   path.** CONNECT targets are subject to the same
   `upstream.allowed_host_regex` and `upstream.deny_target_ranges`
   gates that protect the cache from being used as a cleartext-
   tunnel relay to internal/private IPs. CONNECT to a denied host
   returns 403 before any TLS handshake or cert generation runs.

4. **Same observability surface as Phase 1–5.** Every CONNECT
   produces the §10.1 per-request log line (with new `mitm=true`
   field and the inner-GET fields once the inner request lands);
   `acu_request_total{outcome=...}` increments for the inner GET
   the same way it does today; new metrics for cert-cache hit/miss
   and CA-pool exhaustion (§3.4 in this doc).

The goals are independent of each other in code but share a single
new `[tls_mitm]` configuration block (or whatever §1.2 settles on),
a single new long-lived in-process cert cache, and a single new
log-event family for the MITM lifecycle (CA loaded, leaf cert
issued, cert-cache evicted).

### 1.1 Non-goals (deferred)

Carried forward from earlier phases unchanged:

- Streaming-while-fetching as a singleflight optimization. Deferred
  to [FUTURE-REVIEW.md §1](FUTURE-REVIEW.md).
- Per-byte upstream read timeouts. Deferred to
  [FUTURE-REVIEW.md §1](FUTURE-REVIEW.md).
- Per-suite freshness cadence variation. Deferred to
  [FUTURE-REVIEW.md §2](FUTURE-REVIEW.md).
- Operator-triggered manual GC (admin endpoint or SIGUSR1).
  Deferred indefinitely — the periodic ticker covers normal
  operations.
- Source-package caching, multi-arch beyond amd64, pdiff. Carried
  forward from SPEC §1.2 — none of these are MITM-related and Phase
  6 should not bundle them in.

Newly deferred in Phase 6:

- **Admin-listener TLS.** The admin port (Phase 5) is plain HTTP
  by default, optionally wrapped by an operator-supplied
  reverse-proxy that terminates TLS. Phase 5 §1.2 noted Phase 6 may
  introduce TLS for the admin port; Phase 6 explicitly does **not**.
  Rationale: the admin listener defaults to `127.0.0.1` and is
  read-only; adding a second self-signed-cert flow whose only
  purpose is the admin port doubles the cert-management surface for
  no operational benefit. Operators on multi-host deployments who
  need TLS on the admin port front it with nginx / Caddy / Traefik.
- **Mutating admin endpoints.** Phase 5 §1.2 noted Phase 6 may
  introduce `/admin/gc/run`, `/admin/cache/clear`,
  `/admin/suites/{path}/refresh` behind explicit auth. Phase 6
  explicitly defers these — the MITM listener is enough surface for
  one phase. They graduate into Phase 7+ if observational data
  argues for them; absent that, the periodic GC and freshness
  refresher cover the operational need.
- **OpenTelemetry / OTLP exporters.** Same disposition as Phase 5
  §1.1 — Prometheus exposition is the baseline; OTLP can be built
  on top later.
- **Distributed tracing.** No spans, no trace IDs. Same disposition
  as Phase 5 §1.1.
- **A separate TLS-to-cache listener for the MITM port.** The MITM
  feature wraps CONNECT on the existing plain `:3142` listener (and
  the optional `cache.listen_tls` / `:3443` listener that Phase 1
  shipped — apt-over-TLS-to-the-cache is orthogonal to MITM and
  predates Phase 6). No new listener; no new listen-port config.
- **Pre-emptive cert generation for known popular hosts.** Leaf
  certs are generated on first CONNECT to a hostname and cached;
  no warm-set, no startup pre-issuance. The first CONNECT to a new
  host pays a one-time ~5–20ms cost; all subsequent requests hit
  the cert cache.
- **Per-client CA pinning.** All CONNECTs from any client see leaf
  certs signed by the same CA. Per-client CA bundles (i.e. each
  client trusts a different CA) are out of scope — the operational
  benefit doesn't justify the multi-CA bookkeeping for a fleet that
  is by definition trusted to access the cache in the first place.

### 1.2 Resolved during Phase 6 scoping

*(populated as the §7 question table closes)*

The design questions in §7 are open as of revision 1. As they
resolve through review iteration, the resolutions move here and
become normative for SPEC6.

---

## 2. What Phases 1–5 already left in place

Walking the existing code, prior phases deliberately seeded these
hooks that Phase 6 builds on:

| Prior-phase hook | Phase 6 use |
|---|---|
| `canonical_scheme` column on every cache table (Phase 1 §3.2) | `https` keys for MITM-fetched bytes are already first-class; no schema migration required |
| `HTTPS///` URL parser branch (Phase 1 §2.3) | Continues to work unchanged; MITM is additive |
| `upstream.allowed_host_regex` + `upstream.deny_target_ranges` (Phase 1) | Apply to CONNECT targets pre-handshake — the SSRF gate runs once for both the inner GET and the CONNECT host |
| `hostsem.Sem` per-host concurrency limiter (Phase 1) | Shared with the inner-GET path — a CONNECT that triggers an upstream fetch holds the same `{host}` slot the §10.4.5 metrics already count |
| `internal/handler/handler.go` request pipeline (Phase 1–5) | The inner GET dispatches into the existing pipeline; MITM only intercepts CONNECT and produces a new inner-`*http.Request` for the same handler |
| `cache.listen_tls` / `tls_cert` / `tls_key` (Phase 1 §2.1) | Phase 6 binds the MITM CONNECT handler on BOTH `:3142` (plain) and `:3443` (apt-over-TLS-to-cache) — apt's `Acquire::https::Proxy` setting can point at either, and the inner CONNECT works the same way |
| `slog`-based event stream (Phase 1) | New `mitm_*` event family slots in alongside existing events; admin listener already renders ring-buffer entries |
| `metrics.Registry` (Phase 5 §3.4) | New `acu_mitm_cert_*` counters / gauges register in the existing registry; admin `/metrics` exposes them automatically |
| Read-only admin listener (Phase 5) | Status page §10.5 has room for a "TLS MITM" section showing CA fingerprint, cert-cache hit rate, current cached-cert count |
| Graceful-shutdown sequencing (Phase 1 §9.5, Phase 5 §9.5) | Existing pattern (admin first, plain+TLS proxy concurrent) extends to draining in-flight CONNECT tunnels |
| AIDEV-NOTE convention (CLAUDE.md) | New MITM code carries anchors for the cert-generation, cert-cache, and CONNECT-handler complex spots |

No prior phase shipped a `:3443` MITM-aware variant (the existing
`cache.listen_tls` is just "apt-over-TLS-to-cache" — it terminates
TLS and dispatches absolute-URL GETs the same as plain `:3142`).
Phase 6 is additive: a CONNECT method handler that wraps the
existing pipeline.

---

## 3. Design (high level)

This section is intentionally not a full SPEC — the open questions
in §7 will move pieces around. It describes the shape of the work
so reviewers can evaluate the open questions in context.

### 3.1 Request flow

```
1. apt → cache: CONNECT apt.corretto.aws:443 HTTP/1.1
2. cache validates CONNECT target host against
   upstream.allowed_host_regex + deny_target_ranges. Reject → 403.
3. cache hijacks the connection, responds:
   HTTP/1.1 200 Connection established\r\n\r\n
4. cache looks up apt.corretto.aws in the in-process cert cache.
   Miss → generate a leaf cert (CN=apt.corretto.aws, SAN
   includes all aliases the Remap canonicalizer maps to this
   canonical host), sign with the CA, insert into cache.
5. cache performs TLS handshake on the hijacked conn using the
   leaf cert. Client validates against the operator-distributed CA.
6. cache reads the inner GET request from the now-encrypted stream.
7. cache constructs a synthetic *http.Request — method GET, URL
   built from CONNECT host + inner path + scheme=https, Host header
   from the inner request — and dispatches into the existing
   handler pipeline (proxy.ServeHTTP equivalent).
8. handler caches the response under canonical_scheme=https; cache
   replies on the encrypted inner stream; tunnel closes when the
   inner GET completes (one inner request per tunnel — apt does
   not pipeline within a CONNECT).
```

### 3.2 CA model

The CA is the operator's trust root. Two viable origin paths:

- **Auto-generated at first start.** The daemon, on first boot
  with `tls_mitm.enabled = true`, generates a CA cert+key pair
  inside `cache_dir/ca/` (mode 0700 dir, 0600 files). The cert is
  exported to a well-known location for ansible to pick up; the
  fleet's apt configuration distributes that cert into
  `/etc/apt/trusted.gpg.d/` (for repo signature trust) and/or the
  system CA store (for TLS chain trust — `update-ca-certificates`).
  Pro: zero pre-deploy cert ceremony. Con: each cache instance
  gets a different CA; multi-cache deployments need to handle
  multiple CAs.
- **Operator-supplied.** Operator generates a CA out of band
  (openssl), places `tls_mitm.ca_cert` + `tls_mitm.ca_key` paths
  in the config. Multi-cache deployments share one CA. Pro: full
  operator control; works with HSM-backed CA keys via PKCS#11
  shim if the daemon links a provider. Con: operator must handle
  the OOB ceremony.

The default-on-first-start path is much friendlier for a single-
cache deployment; the operator-supplied path is required for
multi-cache. SPEC6 should document both, with the auto-generated
path as the default for `tls_mitm.enabled = true` when no
ca_cert/ca_key are supplied.

### 3.3 Leaf cert generation + cache

Per-host leaf certs are generated on the first CONNECT to a host
and cached in process memory. Cache is keyed by **canonical** host
(after Remap), not by the literal CONNECT target — three CONNECTs
to `de.archive.ubuntu.com`, `us.archive.ubuntu.com`, and
`archive.ubuntu.com` should all produce one leaf cert with all
three names in the SAN.

Leaf cert parameters:
- Algorithm: ECDSA P-256 (default proposal — small, fast, modern).
- Lifetime: long-lived per process (default proposal — 30 days from
  issuance, regenerated on next CONNECT after expiry). Short-
  lived alternative is rotated.
- Cache: in-memory only; lost on restart, regenerated on next
  CONNECT.
- Eviction: LRU bounded at `tls_mitm.cert_cache_size` (default
  proposal — 256 entries; one entry per canonical host).

### 3.4 Configuration block (proposed)

```toml
[tls_mitm]
enabled               = false          # default OFF (no fleet CA distribution required out of the box)
ca_cert               = ""             # operator-supplied, both empty = auto-generate
ca_key                = ""
ca_storage_dir        = "/var/lib/apt-cacher-ultra/ca"  # where auto-generated CA lands
cert_cache_size       = 256
leaf_cert_lifetime    = "720h"         # 30 days
ca_cert_lifetime      = "87600h"       # 10 years (auto-generated)
allowed_host_regex    = ""             # empty = inherit from [upstream]; explicit narrows MITM-permitted hosts further than upstream-fetch-permitted
require_inner_get_only = true          # reject CONNECT tunnels that send anything other than a single GET (no POST/PUT/CONNECT-in-CONNECT/etc.)
```

`enabled = false` ships as the default; existing deployments
upgrade with no behavior change. Setting `enabled = true` without
distributing the CA produces certificate-validation errors at the
clients; a `tls_mitm_enabled_ca_undistributed` Warn at startup
names the CA fingerprint and a path the operator should
publish-via-ansible to make the feature usable.

### 3.5 Logging + metrics (proposed)

New event family `mitm_*`:
- `mitm_connect` Info — `host`, `port`, `client_addr`, `outcome`
  (`tunneled` / `denied_host` / `cert_gen_failed` / `tls_failed`)
- `mitm_cert_issued` Debug — `canonical_host`, `algorithm`,
  `lifetime_seconds`
- `mitm_cert_cache_evicted` Info — `canonical_host`, `reason`
  (`lru` / `expired`)
- `mitm_ca_loaded` Info at startup — `source` (`generated` /
  `supplied`), `fingerprint_sha256`, `not_after_unixtime`

New metrics under `acu_mitm_*`:
- `acu_mitm_connect_total{outcome}` counter
- `acu_mitm_cert_cache_size` gauge (current entries)
- `acu_mitm_cert_cache_lookups_total{outcome="hit"|"miss"}` counter
- `acu_mitm_cert_issued_total{algorithm}` counter
- `acu_mitm_ca_not_after_unixtime` gauge (operator's expiry alarm)

Status page §10.5 picks up a "TLS MITM" section with the same
fields when the feature is enabled.

---

## 4. Implementation surface (estimate)

The bulk of the change lives in **two new files** under
`internal/proxy/`:

```
internal/proxy/
  connect.go         # CONNECT method handler — host validate,
                     # hijack, leaf-cert lookup, TLS handshake,
                     # inner-GET dispatch
  tlsmitm/
    ca.go            # CA load / generate / persist
    leafcache.go     # in-memory cert cache (LRU)
    leafgen.go       # ECDSA cert generation + signing
```

Plus changes to:

- `internal/config/config.go`: new `[tls_mitm]` struct + validate.
- `cmd/apt-cacher-ultra/main.go`: load CA at startup; pass into
  proxy/admin handlers; startup config dump line.
- `internal/handler/handler.go`: route CONNECT method (currently
  returns 405 per SPEC §2.6) to the new handler.
- `internal/admin/status.go`: status page TLS MITM section.

Estimated diff: ~600–900 LOC including tests.

---

## 5. Test plan (high level)

Unit tests:
- CA generate-and-persist round-trip; fingerprint stability.
- Leaf cert issued for a host has the expected SAN set after Remap
  canonicalization (e.g. CONNECT to `de.archive.ubuntu.com` issues
  a cert valid for `de.archive.ubuntu.com`,
  `archive.ubuntu.com`, etc.).
- Cert cache LRU eviction order; expired-cert refresh.
- Allowed/denied host predicate matches `[upstream]` semantics.

Integration tests (the apt-running end-to-end style we already
have for Phase 2–4):
- apt with `Acquire::https::Proxy` against the cache, CA installed,
  fetches `apt.corretto.aws/dists/stable/Release` — first request
  miss-via-MITM, second request hit.
- Same suite, CA NOT installed → apt fails with TLS verification
  error (the cache logs a `mitm_cert_issued` line; the failure is
  on the apt side per design).
- CONNECT to a host that fails the regex → 403 before any TLS
  handshake; nothing in the cert cache.
- Concurrent CONNECTs to the same host → one cert generation, all
  requests share the cached cert (cert-gen singleflight inside the
  cache).

Chaos tests:
- CA file deleted out from under a running daemon → next CONNECT
  surfaces a Warn but does not crash; existing tunnels keep
  working (cert is in memory).
- Disk full during CA auto-generation → startup fails with a
  config-error log line, NOT a corrupted half-written CA.
- Clock skew where the leaf cert's `not_before` is in the future —
  apt rejects; the cache logs `mitm_clock_skew` Warn.

E2E tests:
- The existing chaos tests under `e2e/` need a "MITM enabled" pass
  added that re-runs the Phase 1–5 suite with HTTPS upstreams and
  CONNECT routing — verifying nothing in the cache behavior path
  broke.

---

## 6. Acceptance criteria (proposed)

Phase 6 is gated on:

1. Every test in §5 passes under `go test -race ./...`.
2. apt with `Acquire::https::Proxy "http://cache:3142"` and the
   CA installed at `/etc/ssl/certs/` successfully fetches:
   - `https://apt.corretto.aws/dists/stable/Release`
   - `https://packages.microsoft.com/repos/ms-edge/dists/stable/InRelease`
   - At least one large `.deb` from each of the above (>10MB) —
     proving the inner-GET path streams bytes correctly through
     the MITM tunnel.
3. The same `sources.list` against an apt-cacher-ng instance with
   its equivalent MITM config produces the same body bytes for
   the same URLs (regression baseline against the upstream we're
   replacing).
4. Cache-hit-rate metric for HTTPS upstreams reaches ≥95% on the
   second pull from the same `sources.list` across all three
   upstreams above (inner-GET counts as a normal cache hit; the
   only misses are first-fetch).
5. A config flag flip (`tls_mitm.enabled = false → true`) on a
   running deployment does not require a restart of the proxy
   listener — graceful reload picks up the new flag. (Open
   question §7.6 on whether reload is in Phase 6 scope or Phase
   7.)
6. SPEC6.md as-built, mirroring SPEC5.md structure.
7. One-week production deployment with `tls_mitm.enabled = true`
   showing stable CONNECT throughput, no cert-cache leaks
   (`acu_mitm_cert_cache_size` bounded), and no SPEC §10.4.1
   `outcome=cache_write_failed` regression on HTTPS-fetched
   bytes.

---

## 7. Open scoping questions

These are the design questions the operator needs to answer before
this becomes a locked SPEC6.md. Each row carries a default
proposal so the operator can answer with "yes, that default" or
"no, here's why" rather than starting from zero.

| # | Question | Default proposal | Rationale to confirm or override |
|---|----------|-----------------|----------------------------------|
| Q1 | **Phase 6 default-on or default-off?** When `tls_mitm` is configured but not explicitly enabled, does the feature run? | **Default OFF.** `tls_mitm.enabled = false` ships as default. Operator opts in. | Default-on requires fleet CA distribution before upgrade, which breaks the "minimize fleet ansible churn" preference. Default-off keeps existing deployments at zero-change-on-upgrade. |
| Q2 | **CA origin: auto-generate or operator-supplied (or both)?** | **Both.** If `ca_cert`/`ca_key` are set, use those. Otherwise auto-generate at first start under `cache_dir/ca/`. | Single-cache deployments benefit from auto-generate (zero ceremony); multi-cache deployments require operator-supplied (one shared CA). Defaulting "both supported, auto-generate when unset" covers both populations. |
| Q3 | **Leaf cert algorithm.** | **ECDSA P-256.** | Smaller cert (~250B vs ~1.1KB for RSA-2048), faster handshake (~3× CPU saving), modern client compatibility (Debian 10+ / Ubuntu 18.04+ all support). Operators who must support pre-2018 clients can override to RSA-2048 via `tls_mitm.leaf_algorithm`. |
| Q4 | **Leaf cert lifetime.** | **30 days, regenerated on next CONNECT after expiry.** | Long enough to keep the cert cache useful across reasonable uptime; short enough that an exposed leaf can't be abused for long. The cache only keeps in-memory copies — restart re-issues. |
| Q5 | **Cert cache scope.** | **In-memory only, LRU bounded at 256 entries.** | The cost of regenerating a leaf cert is ~5–20ms; the cost of persisting + reloading is more code and a synchronization concern across restarts. 256 entries covers typical fleet-of-suites distributions; bumpable via config. |
| Q6 | **CONNECT host allowlist.** | **Inherit `upstream.allowed_host_regex` from §1 for the CONNECT target by default.** Operators can narrow further with `tls_mitm.allowed_host_regex` (empty = inherit). | One regex to maintain by default; the explicit MITM-narrower exists for operators who want HTTPS-via-MITM for a smaller set than HTTPS-via-`HTTPS///`-prefix. |
| Q7 | **Hot-reload for CA / enabled flag.** | **Phase 6 ships fixed-at-startup.** Hot-reload defers to Phase 7. | A clean `tls_mitm.enabled` toggle on a running daemon is operationally nice but adds non-trivial code (atomic-swap CA + cert-cache flush + handler routing flip) for a feature whose operational cadence is "configure once". |
| Q8 | **Inner request shape.** | **GET only.** A CONNECT tunnel that sends anything other than a single GET (POST, PUT, nested CONNECT, HTTP/2, multi-request HTTP/1.1 keepalive within the same tunnel) returns 405 / drops the tunnel. | apt does not use POST/PUT for repository access (SPEC §2.6 is clear on GET+HEAD). Allowing more inside the tunnel would expand the surface for no apt benefit. |
| Q9 | **Phase 6 inner-GET observability.** Should the inner GET produce a separate `request_outcome` log line, or share the outer CONNECT line? | **Separate.** The CONNECT line says `outcome=tunneled` once; the inner GET produces the existing `request` log line with a new field `mitm=true`. | Existing parsers already key on `request` events; keeping the inner GET shape unchanged means `acu_request_total{outcome=...}` counts MITM-fetched and plain-fetched requests with the same labels (which is what an operator wants for "cache hit rate" math). |
| Q10 | **Status-page surface.** Where in the SPEC5 §10.5 status page does the TLS MITM block render? | **New top-level section "TLS MITM" between "Listeners" and "Cache".** Fields: enabled (bool), CA source (`generated`/`supplied`), CA SHA-256 fingerprint, CA `not_after`, cert-cache size + capacity, last cert issued (timestamp + canonical_host). | Discoverable next to the listener-bind summary the operator already reads at startup. |
| Q11 | **Default `[tls_mitm]` block.** Ship a complete commented default in `packaging/config/config.toml.default` so an operator who reads the default config sees the feature exists. | **Yes.** All keys present, all commented out, with the §3.4 inline notes explaining what each does. | Discoverability — operators who don't know the feature exists won't enable it. The conservative defaults make the commented-out form self-documenting. |
| Q12 | **CA distribution helper.** Ship a `apt-cacher-ultra ca print` (or similar) subcommand that emits the CA cert PEM to stdout so the ansible role can capture it and push to clients? | **Yes.** Subcommand that reads the CA cert from the configured path and writes PEM to stdout. Exit 1 if MITM is disabled. | Avoids a brittle "operator types `cat cache_dir/ca/ca.crt`" — the subcommand pins the convention and survives `ca_storage_dir` changes. |
| Q13 | **`http://HTTPS///` deprecation timeline.** Is the URL-prefix convention deprecated once MITM ships? | **Not deprecated. Coexists indefinitely.** | The convention is harmless when MITM is also available. Deprecating it would force fleet config churn — exactly what we're trying to avoid. |
| Q14 | **`apt.conf.d` snippet generation.** Should the daemon emit a recommended apt-conf snippet at startup so the operator can copy it directly? | **Yes, behind `--print-apt-conf` flag (not at every startup).** The snippet documents the proxy URL, the CA path, and any required `Acquire::https::CAInfo` lines. | A startup printout would noise the journal; a one-time `apt-cacher-ultra --print-apt-conf` invocation is what an operator would actually use during initial fleet rollout. |
| Q15 | **CA key rotation.** What's the rotation story? | **Out of scope for Phase 6.** Operators who need rotation generate a new CA out of band, distribute it to the fleet, then reconfigure the daemon. | Rotation requires fleet coordination; the daemon doesn't have a mechanism that beats "operator runs ansible". A future phase could add `apt-cacher-ultra ca rotate` once the operational pattern is clear. |
| Q16 | **Multi-cache deployments.** Does Phase 6 need to formalize "all caches share one CA" beyond the operator-supplied path? | **No, the operator-supplied path is the documented multi-cache solution.** | Phase 6 doesn't ship a clustering primitive; "all caches use the same CA" reduces to "operator distributes the same `ca_cert`/`ca_key` to every cache via ansible". |

Of these, **Q1, Q2, Q3, Q6, Q7, Q12, Q13** are the load-bearing
questions — different answers materially change the SPEC. The
others are small details that can be answered "yes default" if no
specific concern surfaces.

---

## 8. Risks

The primary risk in Phase 6 is **scope creep into Phase 7
territory**:

- Hot-reload of the `[tls_mitm]` block is operationally tempting
  but adds substantial code (Q7 — defer to Phase 7).
- Per-suite or per-host CA pinning (different CAs for different
  upstreams) is operationally tempting for hostile-multi-tenant
  scenarios but not the use case of an internal apt proxy. Defer
  indefinitely.
- A `apt-cacher-ultra ca rotate` subcommand is operationally
  tempting once `print` exists. Defer to Phase 7+.

A secondary risk is **client-side trust expansion**: distributing
the cache's CA into clients' system CA stores means any leaf cert
the cache issues will validate, and the cache becomes a trust
anchor for the whole HTTPS-using fleet. Mitigations:

- The CA is constrained by `tls_mitm.allowed_host_regex` — the
  cache will only issue leaf certs for hostnames that pass the
  regex. A compromise of the cache CA can issue certs for those
  hostnames, but cannot issue arbitrary certs (the regex is
  enforced before signing). Operators should narrow the regex
  to literal upstream hosts in production.
- The CA cert is issued with `Name Constraints` (RFC 5280 §4.2.1.10)
  limiting validity to the same hostname set as
  `tls_mitm.allowed_host_regex`. Clients that validate Name
  Constraints (BoringSSL, OpenSSL 1.1+) reject leaf certs for
  out-of-scope hostnames even if the CA is trusted.
- The CA private key sits at mode 0600 under a daemon-owned
  directory; the daemon does not need root to read it.

A tertiary risk is **CONNECT denial-of-service**: a hostile
client that opens many CONNECTs to non-existent hostnames forces
cert generation per attempt. Mitigations:

- The §1 SSRF/allowlist gate runs before cert generation, so
  CONNECT to a denied host costs nothing.
- The cert cache is bounded (Q5); over-long unique-hostname
  attacks evict cached certs (cost: amortized O(allowed_hosts) ms
  for cert regen on legitimate retry, plus eviction logging that
  surfaces the attack).
- Per-source-IP CONNECT rate-limiting is **not** in scope for
  Phase 6 — operators worried about a hostile client put the
  cache behind nftables / ipset filters, the same mechanism that
  protects the existing plain `:3142` listener.

A fourth risk is **CA private key exfiltration**: a daemon-user
compromise that reads the CA key turns the attacker into a
trusted issuer for the operator's fleet. Mitigations:

- The CA key is mode 0600 owned by the daemon user; standard
  deb postinst conventions apply.
- An operator-supplied CA path can point at an HSM-backed key
  (Q2) — Phase 6 explicitly accommodates the file-on-disk and
  PKCS#11 paths via the same `ca_key` config, leaving the HSM
  glue to Phase 7+ if any deployment asks for it.
- Operators paranoid about key compromise narrow
  `tls_mitm.allowed_host_regex` and add Name Constraints (above)
  — both reduce blast radius without depending on key secrecy
  alone.

---

## 9. Estimated effort

Modest — Phase 6 reuses far more existing machinery than it adds:

- ~2 days to scope (this document) + lock SPEC6.md.
- ~3 days to implement (CA + cert cache + CONNECT handler).
- ~2 days to write tests (unit + integration + chaos).
- ~1 day to write the apt-conf snippet helper subcommand.
- ~3 days for end-to-end verification on real upstreams (Corretto,
  Microsoft, NodeSource) and one-week production soak.

Total: ~11 working days from this scoping to a Phase 6 release
tag. About half the size of Phase 5 — Phase 5 added a whole new
HTTP listener with metrics, gauges, refresher goroutine, htpasswd
auth, status template, and self-metrics; Phase 6 adds a CONNECT
verb + cert-issuing CA, both narrower in surface.

---

## Pointers

- Phase 1 SPEC: [SPEC.md](SPEC.md)
- Phase 2 SPEC: [SPEC2.md](SPEC2.md)
- Phase 3 SPEC: [SPEC3.md](SPEC3.md)
- Phase 4 SPEC: [SPEC4.md](SPEC4.md)
- Phase 5 SPEC: [SPEC5.md](SPEC5.md)
- Future-review-only items: [FUTURE-REVIEW.md](FUTURE-REVIEW.md)
