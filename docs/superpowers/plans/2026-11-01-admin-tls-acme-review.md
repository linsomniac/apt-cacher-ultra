# Admin TLS and ACME pull request review

Combine PRs #20 and #21 into one implementation on current main, with separate reviewable commits. PR #20 provides a useful starting point for admin HTTPS. PR #21 already builds on it, but its certificate lifecycle needs substantial revision before deployment. The accompanying [implementation plan](2026-11-01-admin-tls-acme-plan.md) replaces the unsafe parts while preserving the useful configuration and listener concepts.

## Scope and provenance

Reviewed on 2026-11-01:

| Input | Pinned revision | Scope |
| --- | --- | --- |
| [PR #20](https://github.com/linsomniac/apt-cacher-ultra/pull/20) | `d6e5eabd9eb15830a53180a9bddc18f35442e12f` | Description, discussion, commit message, all 12 changed files |
| [PR #21](https://github.com/linsomniac/apt-cacher-ultra/pull/21) | `e2099c2fe408a6f80119f2c79418ee26c2a87eab` | Description, discussion timeline, four additional commits, 43 changed files relative to #20 |
| Shared PR base | `536a1ab29b0659dadfeb08935f237b1deb555647` | Used to separate proposed changes from existing behavior |
| Current main | `40d40b815479e788216b5c4246401b442c8ff621` | Implementation base, including newer shutdown, admin, dependency and Docker changes |

Planning worktree: `/home/sean/projects/worktrees/apt-cacher-ultra-admin-tls-acme`, branch `review/admin-tls-acme`.

Isolated PR checkout: `/home/sean/projects/worktrees/apt-cacher-ultra-pr21-audit`, detached at the reviewed #21 head. The six additional reproductions are in `internal/acme/review_security_test.go` in that checkout. They deliberately pass when a reviewed defect is present; they are evidence, not regression tests for the replacement implementation.

## Prompt injection and meta security

No overt prompt injection was identified in either PR's reviewed text or changes. Neither changes agent instructions, skills, CI workflows, dependency manifests, build scripts, Git attributes, submodules, or executable file modes. The added text contains no detected hidden formatting/control characters, instruction overrides, shell execution, or encoded executable payloads. The attribution and quoted initial prompt in #20 are contribution provenance, not instructions to the reviewer. #21's merge-order language and new specification are contributor proposals, not authority to merge or adopt their requirements.

The meaningful concerns are unsupported assurance claims and expanded operational authority:

- #20 says `ServeTLS` cannot fail on a bad pair after configuration validation, but it reads the files again later. Validation does not establish that guarantee.
- #21 repeatedly says storage is atomic and errors contain no secrets. Separate file renames and raw `err.Error()` propagation contradict those claims; see findings 1 and 4.
- #21's commit message claims to fix missed renewals when the check interval exceeds the renewal window. The final loop still sleeps for the unrestricted interval; see finding 5.
- The test authority explicitly skips JWS signature verification. Several daemon tests use `InsecureSkipVerify`. These are test limitations, not evidence of a production verification bypass, but passing those tests does not establish protocol interoperability or client trust.
- #21 grants the service `CAP_NET_BIND_SERVICE` even when ACME is disabled, and registers accounts using unconditional `acme.AcceptTOS`. These are visible design choices requiring deliberate treatment in the replacement, not evidence of malicious intent.

The review fetched Git objects with hooks disabled, inspected changes before executing tests, and used only synthetic certificates and a local fake authority for test execution. No PR code was merged, no GitHub comments were posted, and no public CA registration or issuance was attempted. This is a source and focused runtime review, not a guarantee that every possible malicious behavior or defect is absent.

## Findings

### 1. High — Interrupted persistence destroys a usable certificate generation

[PR #21 storage.go:249–275](https://github.com/enix/apt-cacher-ultra/blob/e2099c2fe408a6f80119f2c79418ee26c2a87eab/internal/acme/storage.go#L249-L275) overwrites the private key, then the chain, then the directory/name metadata. Each rename is individually atomic; the record is not. A crash or write failure after the first rename leaves the old certificate paired with a new key. The old usable pair is no longer available on disk. `loadCert` treats this as absent/invalid and attempts issuance, so an otherwise recoverable restart now depends on the CA and challenge reachability. Parent directories are not fsynced after renames either.

The account key and account URL have the same split-record problem in `saveAccount`. A successful registration followed by a persistence failure can also leave the account key unavailable for recovery. Directory changes make independently replaced metadata especially problematic.

**Confirmed:** `TestReviewInterruptedSaveLosesUsableStoredCertificate` stops at the first rename boundary and observes `errStoredCertInvalid` on reload. This reproduces the torn state; it is not a physical power-loss test.

**Required:** store each coherent account/certificate record as one private, versioned atomic bundle, with file and directory fsync, identity-bound metadata, and crash-recovery tests. Persist an account key before registering it so a restart can recover the registration with the same key.

### 2. High — Issued certificates are published without matching the private key

[PR #21 client.go:230–248](https://github.com/enix/apt-cacher-ultra/blob/e2099c2fe408a6f80119f2c79418ee26c2a87eab/internal/acme/client.go#L230-L248) parses the returned leaf and checks its hostname, then attaches the freshly generated private key without comparing public keys. `setCert` persists and publishes it. A malformed or misconfigured authority response can therefore replace the working certificate with one that cannot complete a normal TLS handshake. The ACME library explicitly leaves certificate validation to its caller. [Go ACME client documentation](https://pkg.go.dev/golang.org/x/crypto/acme#Client.CreateOrderCert).

**Confirmed:** `TestReviewIssuedCertificateKeyMismatchPublished` substitutes a certificate with the requested hostname but another public key in the fake authority's response. `EnsureCertificate` reports success, the provider returns the mismatched pair, and reloading the persisted pair fails. This is an authority-response fault injection, not an assertion that an unauthenticated Internet attacker can replace an HTTPS-protected CA response.

**Required:** validate the key match, requested identifiers, validity interval, server usage and chain structure before writing or publishing a candidate. Apply the same invariant to disk recovery and manual file reloads.

### 3. Medium — Validity checks are inconsistent at startup and handshake time

[PR #21 acme.go:223–230](https://github.com/enix/apt-cacher-ultra/blob/e2099c2fe408a6f80119f2c79418ee26c2a87eab/internal/acme/acme.go#L223-L230) omits `NotBefore`; [GetCertificate:273–278](https://github.com/enix/apt-cacher-ultra/blob/e2099c2fe408a6f80119f2c79418ee26c2a87eab/internal/acme/acme.go#L273-L278) checks only for a non-nil certificate. Newly issued material is not checked for validity dates before publication either. A future-dated stored certificate passes startup, and failed renewal can leave an expired certificate returned indefinitely while status still says a certificate exists.

**Confirmed:** `TestReviewNotYetValidStoredCertificateAccepted` starts successfully offline with a future-dated certificate; `TestReviewExpiredCertificateReturnedToHandshake` gets an expired certificate without an error. Ordinary verifying clients still reject invalid certificates; these findings do not establish a bypass of client verification.

**Required:** use one validator and explicit usable/degraded/expired states. Reject new TLS connections when no currently valid certificate is available, including session-resumption paths; keep existing connections and unrelated HTTP service behavior explicitly defined.

### 4. Medium — Raw authority errors flow into logs and admin output

[PR #21 acme.go:304–311,391–397](https://github.com/enix/apt-cacher-ultra/blob/e2099c2fe408a6f80119f2c79418ee26c2a87eab/internal/acme/acme.go#L304-L311) logs and stores unrestricted error strings. ACME problem details and HTTP errors can include URLs, challenge values, account identifiers, or recovery links. `Snapshot.LastError` is copied into both the admin JSON and HTML model.

**Confirmed:** `TestReviewACMEProblemDetailLeaksIntoSnapshot` puts a synthetic token-bearing URL in an ACME problem detail and observes it unchanged in the snapshot. This is a direct propagation test; it does not claim a real secret was exposed. The HTML template escapes text, so this finding is information disclosure and untrusted diagnostic content, not demonstrated XSS.

**Required:** publish bounded, application-owned error categories and summaries. Treat CA response text as untrusted data; do not copy arbitrary detail, URL, header or body fields into status or routine logs.

### 5. High — Restart behavior bypasses backoff; renewal scheduling can miss expiry

[PR #21 main.go:241–248](https://github.com/enix/apt-cacher-ultra/blob/e2099c2fe408a6f80119f2c79418ee26c2a87eab/cmd/apt-cacher-ultra/main.go#L241-L248) calls `EnsureCertificate` once and exits on failure. Backoff exists only in `Run`, which starts after successful boot. The packaged service uses `Restart=always` and `RestartSec=5`. A fast challenge failure can consequently trigger repeated orders across service restarts without the advertised exponential backoff.

**Confirmed:** `TestReviewInitialFailureRestartsHaveNoBackoff` creates three manager lifecycles sharing storage, each with failed validation, and observes three new orders in about 5 ms. The five-second systemd behavior follows from the unit; systemd itself was not exercised.

The [renewal loop:290–317](https://github.com/enix/apt-cacher-ultra/blob/e2099c2fe408a6f80119f2c79418ee26c2a87eab/internal/acme/acme.go#L290-L317) also sleeps for `check_interval` whenever renewal is not due. Configuration only requires a positive interval, so a value longer than the remaining certificate lifetime is accepted and can sleep past expiry. Integer-to-duration conversion of `renew_before_days` is not bounded against overflow. Retry state is lost on restart; the outer lifecycle does not preserve a CA cooldown after a per-attempt context expires. The underlying ACME library does honor `Retry-After` within its own request retries, which does not solve persistence across attempts/processes.

**Required:** one persistent retry policy for bootstrap and renewal, lifetime-aware next deadlines, bounded configuration, jitter, and durable `Retry-After` handling. Let's Encrypt documents rate-limit retry headers and recommends lifetime-aware renewal. [Rate limits](https://letsencrypt.org/docs/rate-limits/#retry-after-header), [integration guide](https://letsencrypt.org/docs/integration-guide/#when-to-renew).

### 6. Medium — Listener conflicts can let initial issuance work and renewal fail

[PR #21 main.go:493–505](https://github.com/enix/apt-cacher-ultra/blob/e2099c2fe408a6f80119f2c79418ee26c2a87eab/cmd/apt-cacher-ultra/main.go#L493-L505) only warns about overlap between the challenge address and application listeners. Initial issuance happens before those listeners bind, so a conflicting setup can obtain a certificate and appear operational. Later, the application owns the port and renewal cannot bind it. Issuance also precedes checking whether unrelated application ports can bind, creating unnecessary external side effects on a doomed startup.

**Source-confirmed:** this follows from startup ordering and the temporary challenge listener; no long-running renewal deployment was simulated.

**Required:** reject known overlaps and reserve required listeners before registration/issuance, without serving application traffic until startup succeeds. Prefer owning the narrowly scoped challenge listener for the process lifetime so another process cannot take it between renewals.

### 7. Medium — Public challenge serving and service privileges need tighter scope

[PR #21 challenge.go:37–55](https://github.com/enix/apt-cacher-ultra/blob/e2099c2fe408a6f80119f2c79418ee26c2a87eab/internal/acme/challenge.go#L37-L55) sets only `ReadHeaderTimeout` and discards terminal `Serve` errors. It has no explicit read/write/idle deadlines or connection bound. Requests with slow bodies and persistent connections can consume resources for the issuance window, up to the ten-minute attempt timeout. The handler correctly limits successful responses to registered challenge tokens, but public exposure needs resource controls too.

The [systemd change](https://github.com/enix/apt-cacher-ultra/blob/e2099c2fe408a6f80119f2c79418ee26c2a87eab/packaging/systemd/apt-cacher-ultra.service#L20-L23) grants low-port binding capability to every installation, including ACME-disabled ones. Current main's Compose configuration drops all capabilities, and its healthcheck assumes HTTP admin, so the old PR does not cover the present deployment environment.

**Required:** bounded challenge HTTP service with supervised errors; high local port plus explicit public port-80 forwarding by default; an optional systemd drop-in for direct port-80 binding; documented HTTPS-aware authenticated healthchecks. HTTP-01 still requires externally reachable port 80. [Let's Encrypt challenge documentation](https://letsencrypt.org/docs/challenge-types/#http-01-challenge).

### 8. Medium — PR #20 validates one read but serves another, and omits certificate reload

[PR #20 config.go:1137–1157](https://github.com/enix/apt-cacher-ultra/blob/d6e5eabd9eb15830a53180a9bddc18f35442e12f/internal/config/config.go#L1137-L1157) loads and discards the pair during validation. [Server.Serve:426–435](https://github.com/enix/apt-cacher-ultra/blob/d6e5eabd9eb15830a53180a9bddc18f35442e12f/internal/admin/server.go#L426-L435) reloads filenames after starting refresh work and concurrently with other listener goroutines. File rotation, deletion or a programmatically constructed config can still produce a late failure. There is no silent HTTP fallback in this code; the issue is the claimed startup invariant and an avoidable second read.

Static pairs require a restart after Certbot renewal, as #20 documents and its discussion acknowledges. #21's dynamic provider fixes that only for its own managed certificate. [PR #20 status.go:540–554](https://github.com/enix/apt-cacher-ultra/blob/d6e5eabd9eb15830a53180a9bddc18f35442e12f/internal/admin/status.go#L540-L554) also changes `listeners[].addr` from a bind address to a URL only for HTTPS admin; a wildcard address is still not a useful certificate-valid client URL.

**Required:** load and retain the validated pair before serving, share a reloadable certificate source across manual and ACME modes, preserve the bind-address field, and add explicit transport/source metadata. Exercise authentication and mutation protection over HTTPS, not only `/healthz`.

## What to retain

Keep HTTP defaults, paired manual certificate settings, explicit ACME selection per listener, the separation from the MITM CA, and the shared certificate for the configured single DNS name. Keep `golang.org/x/crypto/acme` for protocol/signature handling; #21 already uses it and does not need replacement with handwritten ACME protocol code. Preserve the useful fake-authority scenarios, cancellation behavior and checks against silent plaintext fallback, while strengthening their assertions.

Avoid wholesale cherry-picks over current main: the PR base predates important admin cleanup, mutation protection, dependency and container changes. Adapt the listener additions to the current lifecycle and test helpers.

## Validation performed

The following focused selection passed for all four packages at #21's pinned head, using Go 1.26.8 and cached dependencies. The initial sandboxed attempt could not bind local sockets; the rerun with loopback access completed successfully.

```sh
GOCACHE=/tmp/acu-tls-review-gocache GOPROXY=off GOTOOLCHAIN=local \
go test ./internal/acme ./internal/config ./internal/admin ./cmd/apt-cacher-ultra \
  -run 'Test(Review|Manager_|ChallengeServer_|BackoffDelay_|AcquireLock_|.*ACME|.*Acme|Load_AdminTLS|AdminConfig_TLSEnabled|Serve_AdminTLS|New_AdminTLS|Serve_ManualTLS)' \
  -count=1 -v
```

Output is retained locally at `/tmp/acu-tls-pr21-tests.log`. All six `TestReview*` reproductions confirmed their stated behaviors. Diff whitespace checks passed for both PR increments. The standalone #20 head, full repository suite, race suite, Pebble interoperability, Docker/systemd deployment, and public-CA staging were not run. No production code was changed as part of this review and plan.
