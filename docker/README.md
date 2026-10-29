# Docker and Compose

Build and start HTTP caching from the repository root:

```sh
docker compose up -d --build
docker compose logs -f
curl -fsS http://127.0.0.1:6789/healthz
```

The image runs as UID/GID 10001, includes upstream TLS roots, and persists cache
and generated MITM CA in the `apt-cache` named volume. Bind mounts used instead
of that volume must be writable by UID 10001. The admin port binds to host
loopback; the proxy also defaults to loopback. For LAN clients, set
`APT_CACHER_ULTRA_BIND` to the server's LAN IP in `.env` before starting.
Edit `cache.advertise_host` to its client-facing hostname and port.
Only expose the proxy to your trusted LAN. The admin API has mutating endpoints.

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

## HTTPS MITM

Use the same `compose.yml` and `docker/config.toml`. In `[tls_mitm]`, change
`enabled` to `true` and uncomment `allowed_host_regex` and
`allow_unconstrained_ca`. Adapt the literal host allowlist to your repositories
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
builds Linux amd64/arm64 images after the test, lint, end-to-end, Debian package,
and apt-repository jobs pass. The Docker job also validates Compose and smoke
tests the production image before publishing. Pull requests build without publishing.
Pushes to the default branch publish `edge`, a branch tag and `sha-...`;
version tags publish the full version and major.minor. Stable version tags also
publish `latest`; prereleases do not overwrite `latest` or stable major.minor.
The registry name follows the fork automatically (lowercase `github.repository`).

Enable Actions in the fork. The workflow uses `GITHUB_TOKEN` with
`packages: write`; no separate registry password is needed. After the first
publish, make the GHCR package public for unauthenticated pulls, or log Docker
into GHCR on deployment hosts. No image exists until the workflow succeeds.
Pin a release version or digest for deployments. To use the published image:

```sh
APT_CACHER_ULTRA_IMAGE=ghcr.io/linsomniac/apt-cacher-ultra:edge docker compose pull
APT_CACHER_ULTRA_IMAGE=ghcr.io/linsomniac/apt-cacher-ultra:edge docker compose up -d --no-build
```
