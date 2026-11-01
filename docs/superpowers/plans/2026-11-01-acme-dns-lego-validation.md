# Lego DNS-01 implementation and validation

Worktree: `~/projects/worktrees/apt-cacher-ultra-admin-tls-acme`, branch
`review/admin-tls-acme`. This extends the [initial server TLS / HTTP-01
implementation](2026-11-01-admin-tls-acme-validation.md). The user explicitly
selected the full Lego provider catalogue.

## Implementation decisions

- Keep `golang.org/x/crypto/acme` for the ACME protocol and existing private
  account/certificate storage, TLS publication, renewal and origin restrictions.
  Use Lego v5.6.0's provider registry for DNS APIs instead of implementing
  provider-specific integrations. The standard binary lists 227 automated
  provider codes; interactive `manual` and command-hook `exec` are unavailable.
- Add `challenge_type`, `dns_provider` and `dns_resolvers`. HTTP-01 remains the
  default. DNS-01 binds no HTTP challenge socket. Changing challenge method
  preserves a valid certificate in the same authority/name namespace, including
  offline startup without provider credentials.
- Use native provider environment variables and supported `_FILE` credentials.
  Construct a fresh provider per attempt, before CA account/order activity, and
  retain that instance through cleanup so provider-specific record IDs survive.
- Isolate SDK lifetime/output in a subprocess of the running executable, using
  `/proc/self/exe` so a package upgrade cannot replace the helper version mid-run.
  Credential/challenge material travels over private stdin, not command-line
  arguments. A bounded reply protocol uses a separate descriptor. SDK output and
  raw errors cannot become daemon logs/status. Cancellation terminates and reaps
  the worker/process group. This is not a filesystem or network sandbox: the
  worker retains the daemon's UID, environment and privileges.
- Let Lego handle zone discovery, CNAME delegation and DNS mutations. The daemon
  independently waits for the exact expected TXT value at every configured
  recursive resolver, with system resolution as the default. CA validation is
  still independent. Explicit resolvers must be IP:port addresses, at most eight.
- Respect native propagation defaults within a one-hour maximum and polling
  intervals of one second to two minutes. Initial tighter limits would have
  rejected defaults from Namecheap, Gandi and other bundled providers. Provider
  initialization is bounded to 30 seconds; creation and cleanup to five minutes
  each, accommodating SDKs such as Route 53 that wait for asynchronous changes.
  DNS issuance has a 70-minute budget, with a fresh cleanup budget afterward.
  Packaged systemd and the DNS Compose example allow six minutes for shutdown.
- Keep a conservative, durable crash reservation before activity. Once an
  attempt returns, replace that reservation with normal exponential backoff,
  preserving every observed CA `Retry-After`, whether shorter or longer than the
  original reservation. Failed startup configuration therefore does not impose
  an hour-long retry delay. A crash before the final write retains the longer
  reservation, preventing restart loops from placing repeated orders.
- Cleanup runs after partial creation failure and cancellation. A cleanup error
  stops the attempt and retains any valid existing certificate. Crash/hard-timeout
  cleanup is best effort; provider state held only in memory may be lost. Lego's
  RFC2136 adapter replaces the challenge TXT record set, so its challenge name
  or delegated target must be exclusively owned. These limits are explicit in
  [the operator guide](../../server-tls.md).

The full catalogue expands the dependency graph and the release binary (roughly
67 MiB for stripped amd64, 62 MiB for arm64). Compiler concurrency is bounded in
CI and Docker builds. The provider-list command reflects optional custom Lego
build tags, while standard releases include the full automated catalogue.

## Regressions found and addressed

Lego's module graph advances `ProtonMail/go-crypto` from v1.4.1 to v1.5.2. The
existing legacy-audit regression test found that zero-length clearsigned
plaintext now returns an empty block instead of triggering the old decode panic.
`inReleasePlaintext` explicitly rejects that block, preserving the existing
malformed-envelope verdict and avoiding an incorrect signed-seen marker. The
regression fixture and its expected verdict were retained.

The logging-contract checker mistook an error-type conversion called `Error`
for a logging call. The type is now named `Failure`; the checker was not relaxed.
A daemon renewal integration test also assumed certificate installation and
status publication happened simultaneously. It now waits for the completed
renewal before checking status and passed ten repeated race runs.

## Checks

All Go checks use the patched Go 1.27.3 toolchain and `-p 2` where relevant.

| Check | Result |
| --- | --- |
| Full repository `go test -race -p 2 -timeout 10m ./...`, with Pebble enabled | Passed across the repository, including the unchanged empty-envelope regression fixture. |
| Pebble v2.10.1 HTTP-01 and DNS-01 | Issuance, renewal, trusted certificate verification, cleanup and offline restart passed. DNS mode starts no HTTP challenge listener. |
| Real Lego RFC2136 adapter against a local TSIG-authenticated DNS fixture | Creation, CNAME delegation, propagation and cleanup passed. |
| Real Lego Cloudflare adapter against a local API fixture | Credential-file loading, record-ID retention and targeted cleanup preserving another TXT value passed. |
| Provider failure handling | Redaction, cancellation/reaping of a stalled SDK, expected-value checks at every resolver, timing bounds, partial-failure cleanup and exclusion of interactive/command providers passed. |
| Retry persistence | Both short and long CA cooldowns survive completion/restart; DNS crash reservation precedes provider initialization; normal backoff replaces a completed failure's reservation. |
| Standard/tagged lint and vet | Passed. |
| `e2ehooks` daemon tests | Passed; renewal/status timing regression additionally passed ten race runs. |
| `govulncheck@v1.9.0` | No reachable vulnerabilities, none in imported packages; one advisory in a required module outside imported packages. |
| `go mod verify` | All modules verified. |
| Production Docker smoke and authenticated HTTPS healthcheck | Passed with the final runtime implementation. |
| DNS worker tests inside the non-root container, network disabled, all capabilities dropped, no-new-privileges | Passed, including authenticated loopback DNS and local API fixtures. |
| Real apt Compose end-to-end | Passed with an observed fresh cache HIT and an isolated Compose project. |
| Static Linux amd64/arm64 binaries | Built successfully; no dynamic linking. |
| Full and custom provider catalogue | Standard command lists 227 providers; a Cloudflare-only custom build lists only `cloudflare`. Invalid CLI arguments fail instead of starting the daemon. |
| HTTP-01 and DNS-01 Compose examples; whitespace checks | Passed. |

The apt smoke ran after the dependency upgrade; the final crypto compatibility
and retry adjustments are covered by Go regressions and final container checks.
The native arm64 container job, full flat-repository distribution matrix and
Ubuntu package install/upgrade matrix remain CI checks for this extension. The
previous validation record covers those package checks before the Lego change.

No public CA or DNS-provider account was used, and no public DNS records were
modified. Tests use disposable local credentials. Live operation of every
bundled provider is not asserted; providers retain their own account, permission
and external-helper prerequisites. Wildcard/multi-name certificates, external
account binding and ARI remain outside this implementation.
