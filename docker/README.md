# Docker and Compose

Build and start HTTP caching from the repository root:

```sh
docker compose up -d --build
docker compose logs -f
docker compose exec -T apt-cacher-ultra wget -qO- http://127.0.0.1:6789/healthz
```

The image runs as UID/GID 10001, includes upstream TLS roots, and persists cache
and generated MITM CA in the `apt-cache` named volume. Bind mounts used instead
of that volume must be writable by UID 10001. Admin binds to loopback **inside
the container** and is not published. The proxy defaults to host loopback. For LAN clients, set
`APT_CACHER_ULTRA_BIND` to the server's LAN IP in `.env` before starting.
Edit `cache.advertise_host` to its client-facing hostname and port.
Only expose the proxy to your trusted LAN. The admin API has mutating endpoints.

The Docker config blocks loopback, private, link-local and shared address ranges
as upstream destinations, including names that resolve to those ranges. This
also prevents proxy clients from reading the internal admin API. HTTPS upstream
redirects to HTTP are refused. Public repositories are allowed; narrow
`upstream.allowed_host_regex` to your repository hosts where practical.
For a private mirror, narrow that hostname list and remove only the mirror's
range from `deny_target_ranges`; retain loopback and link-local protection.
An explicit upstream proxy must enforce destination restrictions itself (the
daemon requires an empty deny list when `upstream.proxy` is configured).

Use `docker compose exec` for maintenance and metrics, for example:

```sh
docker compose exec -T apt-cacher-ultra wget -qO- http://127.0.0.1:6789/metrics
```

To expose admin outside the container, configure `admin.htpasswd_file`, mount
the password file read-only, and use an authenticated health check before
changing its listen address and adding a host port. Publishing a port on host
loopback does not restrict neighboring containers' direct access. See
[admin authentication](../docs/configuration.md#admin-status-metrics-and-maintenance).

HTTP-only clients use:

```text
Acquire::http::Proxy "http://CACHE_HOST:3142";
Acquire::https::Proxy "DIRECT";
```

The `http://HTTPS///HOST/path` source convention also works without MITM.
Snapshot adoption starts disabled; enable `[adoption].enabled` in the config
for verified snapshots. Debian/Ubuntu keys are bundled. Third-party repositories
need their signing keys mounted under `/etc/apt/keyrings` or a configured
`adoption.keyring_dirs` directory before enabling adoption.

## Server HTTPS and ACME

The cache and admin listeners can serve HTTPS using existing PEM certificates or
built-in ACME. Follow [server HTTPS setup](../docs/server-tls.md) for certificate
reload, provider credentials, persistent state, and authenticated healthchecks
that verify the certificate and hostname.

For managed certificates, use the standalone [HTTP-01 example](../compose.acme.yml)
or [DNS-01 example](../compose.acme-dns.yml). Configure your domain, credentials
and explicit authority terms acceptance before starting. HTTP-01 publishes port
80 for validation; DNS-01 requires no inbound challenge port. The examples keep
application ports on host loopback.

Server certificates and the repository MITM CA are configured independently.

## HTTPS MITM

Use the same `compose.yml` and `docker/config.toml`. In `[tls_mitm]`, change
`enabled` to `true`. A constrained CA policy is already configured, including
`allow_unconstrained_ca = false`. Adapt the literal host allowlist to your repositories
(include regional Ubuntu mirror names if used), and set `cache.advertise_host`.
Start the service, or restart it if it is already running:

```sh
docker compose up -d --build
docker compose restart apt-cacher-ultra
```

No separate SSL certificate mount is needed: on first MITM startup the daemon
generates its CA certificate and private key under
`/var/cache/apt-cacher-ultra/ca/` (`ca.crt` and `ca.key`). The existing `apt-cache`
volume mounts that parent directory, preserving both files across container
recreation. The daemon generates per-host certificates from that CA as needed.
Run these commands from the directory containing `compose.yml` on the cache
host after enabling MITM. Print the public CA certificate to the terminal:

```sh
docker compose exec -T apt-cacher-ultra apt-cacher-ultra ca print -config /etc/apt-cacher-ultra/config.toml
```

Or save it to a file in the current directory on the cache host:

```sh
docker compose exec -T apt-cacher-ultra apt-cacher-ultra ca print -config /etc/apt-cacher-ultra/config.toml > apt-cacher-ultra-ca.crt
```

Copy `apt-cacher-ultra-ca.crt` from the cache host to **every APT client** before
enabling its HTTPS proxy. Copy only this public certificate; keep `ca.key` in
the server volume. On each client, install the copied certificate for APT-only
trust:

```sh
sudo install -m 0644 apt-cacher-ultra-ca.crt /etc/apt/apt-cacher-ultra-ca.crt
```

```text
Acquire::http::Proxy "http://CACHE_HOST:3142";
Acquire::https::Proxy "http://CACHE_HOST:3142";
Acquire::https::CaInfo "/etc/apt/apt-cacher-ultra-ca.crt";
```

Keep source URLs as normal `https://` URLs. Keep the private CA key on the
cache host. The volume preserves the CA across upgrades; `down -v` removes it
and the cache. Editing the allowlist does not regenerate existing CA name
constraints. See [CA reuse and policy changes](../docs/configuration.md#ca-reuse-and-policy-changes).
Restart the service after editing TOML configuration.

## GHCR images

The [CI workflow](../.github/workflows/ci.yaml) runs tests and checks once, then
builds and smoke tests Linux amd64/arm64 images on native runners after the test,
lint, end-to-end (including flat repositories), Debian package, and apt-repository
jobs pass. Smoke tests check network isolation, proxy destination restrictions,
HTTP caching, and MITM CA persistence across container replacement. Pull requests
build without publishing. Only the separate publishing job has registry write access.
Pushes to the default branch publish `edge`, a branch tag and `sha-...`;
version tags publish the full version and major.minor. Stable version tags also
publish `latest`; prereleases do not overwrite `latest` or stable major.minor.
Manual runs publish only from the default branch. The registry name follows the
fork automatically (lowercase `github.repository`).

Enable Actions in the fork. The workflow uses `GITHUB_TOKEN` with
`packages: write`; no separate registry password is needed. After the first
publish, make the GHCR package public for unauthenticated pulls, or log Docker
into GHCR on deployment hosts. No image exists until the workflow succeeds.
Pin a release version or digest for deployments. To use the published image:

```sh
APT_CACHER_ULTRA_IMAGE=ghcr.io/linsomniac/apt-cacher-ultra:edge docker compose pull
APT_CACHER_ULTRA_IMAGE=ghcr.io/linsomniac/apt-cacher-ultra:edge docker compose up -d --no-build
```

## Vulnerability scanning

The [OpenVEX assessment](../security/openpgp.vex.json) marks
[GO-2026-5932](https://pkg.go.dev/vuln/GO-2026-5932) as `not_affected` for this
application's `golang.org/x/crypto@v0.58.0` dependency. Its vulnerable legacy
OpenPGP packages are absent from the production dependency graph on Linux
AMD64 and ARM64. OpenPGP verification uses the maintained ProtonMail library;
`x/crypto` is still needed for bcrypt and other cryptographic primitives.

CI and the release workflow run `make check-deps`, rejecting direct or
transitive legacy OpenPGP imports on either architecture. A failed dependency
lookup also fails the guard. CI separately runs `govulncheck` without any
exceptions. The guard's offline regression tests run with `make check-deps-test`.

To scan a production image built from the current checkout with Trivy's
[local OpenVEX support](https://trivy.dev/docs/latest/supply-chain/vex/file/):

```sh
make check-deps
docker build -t apt-cacher-ultra:scan .
trivy image --scanners vuln --exit-code 1 --show-suppressed \
  --vex security/openpgp.vex.json apt-cacher-ultra:scan
```

The assessment matches this application's Go module and the exact `x/crypto`
version; it does not apply to other applications or other advisories. Inspect
the suppressed finding to retain its justification. When updating `x/crypto`,
rerun the guard and scan, then reassess the VEX statement and update its module
version, timestamp and document version. Also reassess when adding production
architectures or build tags. Custom builds that import legacy OpenPGP must not
use this assessment.
