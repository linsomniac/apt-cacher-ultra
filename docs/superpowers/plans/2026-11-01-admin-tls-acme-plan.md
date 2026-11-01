# Unified admin TLS and ACME implementation plan

Implement admin HTTPS, reloadable file certificates, and optional ACME certificate management as one feature on current main. Share certificate validation and listener integration; keep ACME protocol work in a small manager around the existing Go ACME library. Deliver the work in the seven commits below, with the security and recovery tests passing before the unified change is ready to merge.

The [critical review](2026-11-01-admin-tls-acme-review.md) records the source revisions, findings and reproductions. PR #21 already contains #20, so merging the two old branches independently would duplicate review and preserve avoidable lifecycle differences. Start from `40d40b815479e788216b5c4246401b442c8ff621` or its current-main successor, preserving subsequent hardening.

## Proposed behavior

Existing configurations keep their behavior: admin defaults to loopback HTTP; the plain proxy remains available; manual cache TLS still uses `cache.listen_tls`, `tls_cert` and `tls_key`. ACME remains off by default and performs no network calls or state creation when disabled.

| Listener | Plain mode | File certificate | Managed certificate |
| --- | --- | --- | --- |
| Admin | Existing default | New `admin.tls_cert` and `admin.tls_key` | `admin.use_acme = true` |
| Cache TLS | Listener absent | Existing certificate settings | `cache.use_acme = true`, with `cache.listen_tls` |
| Plain cache proxy | Existing separate listener | Unchanged | Unchanged |

Every enabled TLS listener has exactly one certificate source. Partial pairs, conflicting file/ACME settings, missing sources, or source/configuration mismatches fail closed. Admin disabled-state semantics remain explicit; selecting ACME for a disabled admin listener is an error. TLS never substitutes for admin authentication, mutation protection or network restrictions.

One configured DNS name and one managed certificate may serve both TLS listeners. The administrator controls that name; ClientHello SNI and HTTP Host never initiate issuance. DNS-01, wildcard/IP issuance, multiple certificate profiles, external account binding and multi-node shared-state deployment are outside the first version. The challenge port is the only interface that must be publicly reachable; admin and cache listeners may remain private. The MITM CA, CONNECT-generated certificates and ACME keys remain separate in code, storage and status.

## Configuration decisions

Keep the useful PR field names for manual TLS and per-listener `use_acme`. For `[acme]`, implement:

| Key | Proposed default and validation |
| --- | --- |
| `enabled` | `false`; enabling requires at least one selecting listener |
| `directory_url` | Empty; require an explicit absolute HTTPS directory URL when enabled, with a valid host/port and no userinfo, query or fragment |
| `dns_name` | Empty; require one normalized ASCII DNS name; reject wildcard, IP, port, path, empty label and oversized input; document punycode for IDNs |
| `listen` | `127.0.0.1:8080`; a dedicated challenge listener, with explicit forwarding from public port 80; container examples use `0.0.0.0:8080` inside the container |
| `accept_terms` | `false`; require deliberate opt-in for registration and persist the directory/account terms context; report later user-action-required responses without silently replacing the account |

Do not introduce the PR's unrestricted `renew_before_days` and `check_interval` knobs in the first release. Derive renewal and recheck deadlines from certificate lifetime, with a bounded internal housekeeping interval. These settings have not shipped; there is no released compatibility contract to preserve. This removes duration-overflow and missed-window configurations.

Keep CA choice explicit rather than silently targeting production. Examples start with staging. Use the existing `cache.dir` volume for persistent managed state, under a distinct `acme` subtree; document that deleting that subtree loses certificate/account recovery state. File-based certificates remain operator-managed paths, including legitimate Certbot symlinks, and are never rewritten by the daemon.

These are proposed implementation defaults, not changes already applied to configuration.

## Commit 1 — Certificate source and validation

Create `internal/servertls` for immutable certificate snapshots, candidate validation and file loading. Keep this package independent of admin, proxy and ACME. Supply a small source contract to listener wiring: get the current certificate/snapshot, report availability, and stop any owned background work. Give both static-file and ACME sources the same validation path.

Validation before publication must check:

- Bounded, parseable PEM/DER input and a matching private/public key using the standard library.
- A currently valid interval, a non-CA server leaf, compatible key usage and server-auth EKU when restricted. Parse the full supplied chain and verify its internal links and constraints; do not mistake a parseable leaf for a usable pair.
- For ACME, the exact requested identifier set and the expected directory/name identity attached to the stored record. Manual certificates are not required to match a wildcard bind address; client hostname verification still applies.
- A sensible remaining lifetime before accepting a new managed certificate, preventing immediate reissue loops on unusably short responses.

Do not require every manual or staging certificate to chain to the host's public roots: private PKI and staging are intentional use cases. Verify actual client trust in integration tests with explicit test roots, and document the trust distinction. The HTTPS connection to a configured ACME authority must always verify its server certificate.

Configure TLS 1.2 as the minimum for both file and ACME modes, leaving cipher selection and TLS 1.3 to maintained Go defaults. Prepare TLS settings before serving and leave published certificates immutable. Go's certificate callback contract explicitly requires returned certificates not to be modified. [Go TLS configuration documentation](https://pkg.go.dev/crypto/tls#Config).

Use a lightweight availability gate before each new TLS connection, including resumptions, so a `GetCertificate`-only check cannot be skipped by session tickets. Preserve normal Go HTTP/2 setup and existing cache CONNECT behavior. Renewed certificates appear on new full handshakes; existing connections remain intact.

**Acceptance:** malformed and mismatched pairs, wrong DNS name, future/expired certificates, CA leaves and inappropriate EKU are rejected without replacing a valid snapshot. TLS 1.0/1.1 are rejected. Concurrent readers see a complete old or new snapshot; resumed connections obey the expiry policy.

## Commit 2 — Admin HTTPS and safe file reload

Add the admin file settings in `internal/config`, and wire the certificate source through current `cmd/apt-cacher-ultra/main.go` and `internal/admin/server.go`. Load and retain startup certificates before starting application serving or refresh work. Configuration checks may validate syntax and paths, but the serving source must retain the actual validated material; do not discard it and later ask `ServeTLS` to reread filenames.

Implement a cancellable file refresh loop, initially once per minute. Read bounded file contents and validate the whole candidate pair on each pass, including changes with unchanged mtime/size and symlink-target rotations. Deduplicate identical configured pairs where useful. A partial external rotation or read/parse error keeps the last currently valid snapshot and emits a bounded warning. Once that snapshot expires, new TLS connections fail until valid material is available. Do no filesystem reads during TLS handshakes, and do not add arbitrary post-renewal shell hooks.

Reuse current main's test-server setup and shutdown helpers. Preserve its authenticator, profiling cancellation, mutation guard, refresher shutdown ordering and proxy drain behavior.

**Acceptance:** real verified HTTPS requests to status, metrics and health; authentication failures/successes and mutation protections over HTTPS; plain HTTP cannot reach TLS handlers; file A-to-B rotation reaches both configured TLS surfaces without restart; broken/intermediate rotation retains A until a valid replacement; clean cancellation and no goroutine leak. Include existing manual cache TLS and default HTTP regressions.

## Commit 3 — Durable ACME state and account recovery

Create `internal/acme` with storage scoped by normalized directory identity, then certificate DNS name. Hash directory identifiers for directory names; never interpolate CA-provided URLs or request strings into filenames. Separate the per-directory account record from per-name certificate records and retry records. This prevents staging/production and name changes from destructively overwriting one another.

Use versioned single-file bundles: each certificate record contains the private key, chain and identifying metadata together; each account record contains its private key, account URL and registration state together. Write a uniquely named file with mode `0600`, fsync, rename within the same private directory, then fsync that directory. Parent directories use `0700`. Retain the last committed record on pre-commit failures and publish memory only after durable success. Define recovery for an ambiguous post-rename/fsync failure and validate the complete bundle on restart.

Persist a pending account key before registration. On an interrupted registration, recover the account by that key rather than generating a new account on every retry. Do not treat every authorization error as proof that an account should be replaced; distinguish confirmed deactivation, missing account, transient errors, and operator-action-required states.

Use a cancellable single-writer lock covering read/modify/write and issuance decisions. Re-read after acquiring it to reuse a certificate issued by a peer process. Locking protects overlapping local lifecycles; it does not promise coordinated load-balanced challenge serving or safe multi-host cache sharing.

Harden only the daemon-owned state paths against symlinks, non-regular files, unsafe ownership/permissions and unbounded reads. Use directory-relative operations and appropriate no-follow checks so validation is not separated from the operation it is meant to protect. Do not apply that prohibition to operator-provided Certbot certificate paths.

**Acceptance:** interruption and injected I/O failure at every write/fsync/rename boundary; coherent old-or-new records after restart; offline reuse of an intact valid record; pending-registration recovery with the same key; directory/name changes; lock contention/cancellation; malicious path types and restrictive modes. Convert the review's torn-write reproduction into a regression asserting preservation of the previous usable generation.

## Commit 4 — ACME protocol and bounded challenge service

Use `golang.org/x/crypto/acme` from current main's dependency set. Reuse the sensible order/authorization flow from #21, but keep validation, persistence and retry policy outside the library calls. Do not recreate nonce handling, JWS signing or ACME protocol encodings. Check compatibility of the pinned library's finalization API with the test CA; evaluate a narrowly scoped dependency update if needed, preserving main's existing security updates.

Use a dedicated outbound HTTP client with verified HTTPS, bounded dial/header/request/attempt deadlines and response sizes. Apply URL policy to every outbound ACME resource and redirect, not only the configured directory. Reject insecure downgrade, credentials and malformed resource URLs. Define permitted authority origins explicitly if a CA needs multiple hosts; allow intentionally configured private authorities without reusing the package proxy's fetch/remap client. Test redirects and authority-supplied URLs as untrusted inputs.

Own a dedicated HTTP-01 listener for the enabled process lifetime. Serve only exact active challenge tokens for the configured host; unknown tokens and unrelated paths return 404. Permit GET/HEAD, reject unnecessary request bodies, set modest header/read/write/idle limits, bound concurrent connections and avoid access logging token paths. Remove tokens after the authorization/attempt is finished. Monitor `Serve` failures and propagate them to lifecycle handling; an idle empty challenge map exposes no application routes.

HTTP-01 requires public port 80 even if the local listener is 8080. Supply port-forwarding and reverse-proxy examples that route only the challenge path, with A/AAAA reachability and host-header handling covered. [Let's Encrypt HTTP-01 documentation](https://letsencrypt.org/docs/challenge-types/#http-01-challenge).

**Acceptance:** normal issuance; asynchronous order readiness; reused authorization; rejected/expired authorization; bad nonce and malformed CA responses; mismatched issuance key; wrong identifiers/dates/EKU; redirect downgrade; challenge routing through a local proxy; unsupported methods/bodies, slow clients and connection saturation; prompt cancellation with no tokens left behind. Use fake trust roots rather than `InsecureSkipVerify` in success-path tests.

## Commit 5 — Bootstrap, renewal and shutdown lifecycle

Resolve all source choices and validate known address overlaps first. Reserve required application and challenge sockets before external account/issuance work. Application sockets are not served until certificates and the remaining daemon components are ready. On any failure, close all reserved resources. Test wildcard and dual-stack conflicts at the socket layer as well as validating obvious configuration conflicts.

Reuse a valid stored certificate without waiting for a CA call. If no usable certificate exists, perform a bounded bootstrap attempt only when the persisted retry deadline permits it. On failure, persist the next eligible time and return a clear nonzero startup result; a service restart before that deadline must not issue another order. This keeps startup atomic while avoiding the PR's restart-driven order loop. Keep sufficient diagnostics in logs because admin may not yet be available.

Use one scheduler for bootstrap and renewal. Derive a renewal target from actual `NotBefore`/`NotAfter`: conservatively renew with about one-third of lifetime remaining, and halfway through lifetimes shorter than ten days, with earlier bounded jitter. Wake at the earliest relevant renewal/check deadline rather than sleeping a user-selected interval past it. Use an injectable clock/timer for deterministic tests. These fallback choices follow Let's Encrypt's integration guidance. [Renewal guidance](https://letsencrypt.org/docs/integration-guide/#when-to-renew).

Persist retry counts and `next_attempt_at` per certificate/directory. Honor the authority's valid `Retry-After` at both the request and lifecycle layers, including across attempt timeout and process restart; add capped exponential backoff and jitter for transient failures. Do not retry earlier merely because the certificate is expiring. If retry persistence fails, stop external retry activity and surface the storage error instead of creating an untracked order loop. Keep confirmed account deactivation and terms-required responses distinguishable from ordinary network failures.

ARI is a worthwhile follow-up: the inspected pinned ACME library has no public ARI API. Keep a scheduling seam for it, document the conservative lifetime fallback, and do not claim ARI rate-limit exemptions. Avoid implementing a new protocol extension as an incidental part of the initial TLS change.

Runtime renewal failure retains the currently valid certificate and marks status degraded. At expiry, the TLS availability gate rejects new connections; it never falls back to HTTP, another directory's certificate, or the MITM CA. Existing connections and the separate plain proxy can continue. Recovery installs a newly validated certificate without restarting.

On shutdown or fatal listener failure, cancel issuance, retries and reload loops, stop challenge serving, then join workers within the existing shutdown budget. Preserve current main's flip barrier, admin drain and hijacked-CONNECT cleanup ordering.

**Acceptance:** startup failure serves no application traffic; occupied ports produce no CA side effects; repeated failed restarts respect persisted cooldown; CA outage with a valid stored certificate still starts; simulated short lifetimes renew before expiry; Retry-After survives restart and timeout; concurrent handshake/renewal/shutdown passes the race detector; both TLS listeners present the same renewed certificate; listener failure during an order cannot hang shutdown.

## Commit 6 — Observability and deployment

Keep `listeners[].addr` as a bind address. Add separate scheme and certificate-source fields, and show the configured ACME DNS name separately from wildcard bind addresses. Extend JSON contract tests and make HTML show disabled, usable, renewal-failing and expired states distinctly. Include certificate dates/fingerprint, last success, next eligible attempt and a bounded error category. Use low-cardinality metrics for expiry, attempts, renewal outcomes and reload failures; never use tokens, arbitrary URLs or raw errors as labels.

Centralize error translation before logging or creating snapshots. Allowlist stable codes and application-owned messages. Test markers in CA problem detail, nested errors, URLs, response headers and challenge values against every log/status path. Preserve HTML escaping. Certificate possession is not authentication: keep non-loopback unauthenticated-admin warnings even with TLS, and document both `/reconcile` and current main's `/forget` protections.

Leave the base systemd service's capabilities unchanged. Document an optional drop-in granting only `CAP_NET_BIND_SERVICE` for direct port-80 binding. Prefer an unprivileged local challenge port with forwarding. For Compose, retain `cap_drop: [ALL]` and `no-new-privileges`, publish public host port 80 to container port 8080 only in an explicit ACME example, persist state, and keep admin/cache publication private by default.

Update `docs/configuration.md`, packaged defaults, Docker guidance, healthcheck examples and the changelog together. HTTPS admin healthchecks must use the appropriate hostname, trust root and credentials where authentication is enabled; never recommend disabling certificate verification. Account for the existing image's HTTP-only healthcheck. Keep staging/production state separate and describe reset, backup, expiry recovery and existing Certbot migration. Manual file reload also supports operators who choose external certificate automation instead of built-in ACME.

**Acceptance:** default packaged and Docker configurations still start without certificates or extra capabilities; HTTPS/ACME variants have working healthchecks; restart preserves issued material; public challenge routes cannot reach admin handlers; no synthetic sensitive markers appear in logs/JSON/HTML; documentation examples parse and agree with runtime defaults.

## Commit 7 — Interoperability and release checks

Use the local fake authority for deterministic fault injection, then add an isolated Pebble integration lane with real JWS verification, a trusted test CA, challenge validation, renewal and restart. Pin the test dependency/image. Include both TLS surfaces, private listeners with public challenge forwarding, ACME disabled, manual-only TLS, and MITM enabled alongside server TLS.

Before the unified implementation is ready, run the repository's current checks: `go vet ./...`, `go test -race -timeout 10m ./...`, tagged `e2ehooks` vet/tests, lint and applicable Docker/package smoke tests on the CI Go toolchain. Re-run the relevant proxy/CONNECT scenarios because TLS listener changes can affect HTTP/2 and shutdown. Preserve the existing reachable-dependency vulnerability check if dependencies change. These are future implementation gates, not checks claimed by the present review.

Public Let's Encrypt staging issuance is an optional deployment acceptance exercise using an operator-controlled test domain, explicit terms acceptance, correct DNS and deliberate port exposure. It is not part of ordinary CI and is not needed to complete the local implementation. Production issuance must never be a test side effect.

## Ready-to-merge criteria

All eight review findings have a corresponding regression or deployment check. The implementation provides verified HTTPS, safe reload, durable recovery and restart-safe renewal for both listeners without changing default HTTP behavior or mixing the MITM CA into server certificates. It preserves current main's hardening and passes the release gates above. Submit as one unified PR with the original contributors credited and the repository-required AI attribution and prompts; do not merge or close the source PRs as an incidental implementation step.
