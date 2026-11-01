# Unified server TLS / ACME implementation validation

Implemented in `review/admin-tls-acme`, based on `40d40b8`, in
`~/projects/worktrees/apt-cacher-ultra-admin-tls-acme`. The original checkout's
tracked files and existing untracked work were left untouched.

## Implemented behavior

- Independent file or ACME selection for cache TLS and admin HTTPS; HTTP defaults
  and the MITM CA remain separate.
- Immutable certificate publication, TLS 1.2 minimum, complete pair/chain/name
  validation, file reload, and validity checks covering session resumption.
- Renewal and availability use the intersection of the served chain's validity
  periods, including an intermediate that expires before the leaf.
- Private descriptor-relative storage, coherent atomic certificate/key records,
  account-key recovery, cross-process locking, durable cooldown reservation before
  network activity, and persisted authority `Retry-After` deadlines.
- Dedicated bounded HTTP-01 listener, same-origin verified HTTPS transport,
  startup socket reservation, cancellation and worker cleanup.
- Admin authentication and mutation guards over HTTPS, bounded status/error
  fields, certificate metrics, configuration/deployment/recovery documentation.
- Pinned Pebble and authenticated container healthcheck CI lanes. Publishing
  depends on the independent ACME lane succeeding.

The plan's implementation stages were grouped into one code commit so that
configuration switches and their serving behavior land together. Review/plan,
deployment documentation/CI, and this validation record are separate commits.

## Checks completed

| Check | Result |
| --- | --- |
| `go test -race -timeout 10m ./...`, Go 1.27.0 | Passed across the repository. |
| Race tests for `internal/acme`, `internal/servertls`, `internal/admin`, `cmd/apt-cacher-ultra`, Go 1.27.3 | Passed, including real Pebble issuance and renewal. |
| `go vet ./...` and `go vet -tags e2ehooks ./cmd/apt-cacher-ultra` | Passed. |
| Standard and `e2ehooks` golangci-lint | Passed, zero issues. |
| `go test -tags e2ehooks -count=1 ./cmd/apt-cacher-ultra` | Passed. |
| Pinned `govulncheck@v1.9.0`, Go 1.27.3 | No reachable vulnerabilities; one advisory in a required module outside imported packages. |
| Pebble v2.10.1 | Actual HTTP-01, JWS verification, trusted local CA, renewal, offline restart passed. |
| Docker production smoke | Non-root isolation, upstream restrictions, HTTP miss/hit and constrained MITM CA persistence passed. |
| Optional HTTPS container healthcheck | Verified root/hostname, Basic authentication, both TLS listeners and restart passed. |
| Real apt Compose end-to-end | Passed; a fresh cache HIT was observed. |
| Ubuntu 24.04 and 26.04 package smoke | Install, unit validation, adoption and upgrade/maintainer-script checks passed. |
| Default and ACME Compose configuration | Both parsed successfully. |
| Pinned ShellCheck | Passed. |
| Static Linux amd64 and arm64 builds | Passed; amd64 binary has no dynamic section and runs successfully. |
| `git diff --check` | Passed. |

The initial vulnerability scan used Go 1.27.0 and correctly flagged standard
library vulnerabilities. Rechecking with the current patched
[Go 1.27.3 release](https://go.dev/dl/) cleared all reachable findings. Production
and package Docker builds used Go 1.27.3. Deployment builds should use a current
patched toolchain; there were no changes to application dependency versions.

Fault tests cover interrupted persistence, unsafe filesystem objects, invalid
certificate/key combinations, retry state across restart and timeout, rejected
account credentials without account replacement, cancellation, challenge
isolation, file rotation, TLS resumption expiry, and concurrent renewal readers.
The daemon integration test combines server TLS on both listeners with MITM
enabled and checks that both adopt the same renewed certificate.

## Deliberate limits

ACME supports one DNS name and HTTP-01, with all authority URLs on one HTTPS
origin. External account binding, wildcard/DNS-01 and ARI are not implemented.
Public Let's Encrypt issuance was not attempted; the interoperability tests use
local authorities. The full flat-repository distro matrix and native arm64
container lane remain CI checks; the local validation includes real apt, both
supported Ubuntu packages, and arm64 compilation.

The work is committed locally. No branch was pushed, PR created or source PR
merged/closed as part of this implementation.
