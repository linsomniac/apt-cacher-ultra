# apt-cacher-ultra

A robust apt repository cache focused on availability under upstream failure.

Designed as a replacement for `apt-cacher-ng` that keeps cache hits
available even when upstream Ubuntu/Debian/PPA mirrors are slow, broken,
or under DDoS.  It snapshots repo updates, tracks "hot packages" across your
fleet, and pre-downloads hot packages before making snapshot updates available
for clients to request.  This provides the ability to reliably provide package
sets to clients even if upstream servers become unavailable.  The limitation is
that "cold" packages require upstream availability.  Typical usage should be
available from the cache at all times.

> **Contributing:** apt-cacher-ultra accepts AI-authored contributions only.
> See [CONTRIBUTING.md](CONTRIBUTING.md) for more details.

![Admin UI](docs/admin-ui.png)

## Status

I've released 0.10.3, which I'm thinking of as the first Release Candidate of 1.0.0.
This service has been running in my 4 environments and has served tens of thousands
of "apt update", "apt upgrade" and "apt install" sessions.

## Features

- Drop-in apt-cacher-ng replacement — same :3142 port, proxy mode, and http://HTTPS/// URL convention, so existing
client configs work unchanged.
- TLS MITM (optional) — local CA signs per-host leaf certs so HTTPS repos (e.g. download.docker.com) can be cached,
- Availability-first caching — cache hits never block on upstream; snapshot is served when upstream is down/slow
  rather than failing.
- Atomic snapshot adoption — per-suite InRelease + all referenced Packages/by-hash blobs are staged, GPG-verified, and
  flipped in a single SQLite transaction so clients always see a coherent metadata set.
- Hash validation — every metadata file is checked against InRelease, every .deb against Packages; mismatches are
rejected.
- by-hash dedup — indices stored by content hash, deduplicated across suites.
- Singleflight coalescing — N concurrent clients requesting the same uncached file produce one upstream fetch.
- Resumable upstream fetches — HTTP Range used to resume on transient failure.
gated by an allowed-host regex.
- Freshness control — periodic and on-request InRelease checks with cooldown; hot-package proactive refresh.
- Concurrency caps — per-host and global max_concurrent_adoptions semaphores keep adoption traffic from starving
request-path callers.
- Garbage collection — refcounted blobs are swept when no snapshot references them.
- Self-healing snapshots — a snapshot adopted while a required index was briefly unavailable is repaired in place,
  automatically on the next unchanged freshness check or on demand via `POST /reconcile` (see "Recovering a degraded
  repository").
- Observability — /metrics endpoint, status page, structured logs (see docs/log-fields.md).
- Packaging — ships as a .deb with systemd unit, or as standalone go executable.

## Quickstart

### As Deb Package:

```sh
make deb
dpkg -i build/apt-cacher-ultra_*.deb
#  EDIT: /etc/apt-cacher-ultra/config.toml
sudo systemctl enable --now apt-cacher-ultra
systemctl start apt-cacher-ultra
#  allow 3142 through firewall if necessary:
iptables -I INPUT -p tcp --dport 3142 -j ACCEPT
```
### Manual build:

```sh
make build
cp packaging/config/config.toml.default config.toml
#  EDIT: config.toml
./build/apt-cacher-ultra -config config.toml
```

### Configure apt clients:

Point clients at it as a proxy (matches existing apt-cacher-ng deployments)
by creating the following file with these contents:

```
# /etc/apt/apt.conf.d/00aptcacher
Acquire::http::Proxy "http://APT_CACHER_ULTRA_HOSTNAME:3142";
```

For apt repositories using **https**, pick one of these (the cache allows all
upstream hosts by default, so no allowlist editing is needed):

- **https, not cached** — leave the sources as `https://` and set **only**
  `Acquire::http::Proxy` (do *not* set `Acquire::https::Proxy`). apt connects
  directly to the upstream over TLS; those packages are simply not cached.
  (If you point `Acquire::https::Proxy` at the cache while MITM is off, the
  cache returns `405` to the `CONNECT` and apt fails — so leave it unset.)
- **https, cached, no MITM** — rewrite each source from `https://HOST/path`
  to `http://HTTPS///HOST/path` (the apt-cacher-ng convention). The
  client↔cache hop is plain http through the proxy above; the cache fetches
  the upstream over https and caches the result. No CA setup required.
- **https, cached, with MITM** — keep the sources as `https://`, enable the
  MITM proxy, and install its CA on each client (see the next section).

### Enable the MITM HTTPS proxy (optional):

By default `CONNECT` for `https://` repos returns `405` — apt then talks
TLS straight to the upstream and the cache is bypassed for those repos.
Enabling MITM lets the cache decrypt, cache, and re-serve HTTPS sources
by signing per-host leaf certs from a local CA.

1. Add a `[tls_mitm]` block to `config.toml`:

   ```toml
   [tls_mitm]
   enabled            = true
   # allowed_host_regex lists the hosts MITM may sign certs for. It is
   # translated into the CA's X.509 NameConstraints, so it accepts only a
   # restricted grammar (see the table below). This example covers the
   # Debian repos plus Docker:
   allowed_host_regex = '^(deb\.debian\.org|security\.debian\.org|download\.docker\.com)$'
   # ca_cert / ca_key empty = auto-generate under <cache.dir>/ca on first start.
   ```

   **Supported `allowed_host_regex` shapes** (anchors `^…$` optional but must
   be balanced). Anything else is refused at startup with
   `mitm_ca_unconstrained_refused` unless you set `allow_unconstrained_ca = true`:

   | Shape | Example |
   |-------|---------|
   | literal host | `^deb\.debian\.org$` |
   | single-label prefix (note `+`, not `*`) | `^[a-z0-9-]+\.debian\.org$` |
   | optional 2-letter region | `^([a-z]{2}\.)?archive\.ubuntu\.com$` |
   | alternation of **literal** hosts | `^(deb\.debian\.org\|security\.debian\.org)$` |

   `*` quantifiers and alternations with non-literal branches (e.g. the
   `^([a-z0-9-]+\.)*(ubuntu\.com|debian\.org)$` form) are **not** supported.
   To MITM-sign for **all** hosts instead (no name constraints — convenient,
   but the CA can then mint a cert for any name), use:

   ```toml
   [tls_mitm]
   enabled                = true
   allowed_host_regex     = ''
   allow_unconstrained_ca = true
   ```

2. Start the daemon once so the CA is materialized, then export it:

   ```sh
   sudo systemctl restart apt-cacher-ultra
   sudo apt-cacher-ultra ca print > apt-cacher-ultra-ca.crt
   ```

3. Set up the CA key on every apt client. Choose one of:

   a. Install the CA and refresh the system-wide trust store:

      ```sh
      sudo cp apt-cacher-ultra-ca.crt /usr/local/share/ca-certificates/
      sudo update-ca-certificates
      ```

   b. Place the CA cert and configure apt (and only apt) to use it:

      ```sh
      sudo cp apt-cacher-ultra-ca.crt /etc/ssl/certs/
      ```

      Then in an `/etc/apt/apt.conf.d` file:

      ```
      # /etc/apt/apt.conf.d/00aptcacher
      Acquire::http::Proxy "http://APT_CACHER_ULTRA_HOSTNAME:3142";
      Acquire::https::CaInfo "/etc/ssl/certs/apt-cacher-ultra-ca.crt";
      ```

4. Generate the client apt-conf snippet (includes the CA fingerprint as
   a comment for verification):

   ```sh
   apt-cacher-ultra --print-apt-conf -config /etc/apt-cacher-ultra/config.toml \
       > /etc/apt/apt.conf.d/00aptcacher
   ```

## Inspecting the cache

Two read-only management subcommands let you see what's in the blob store
and pull a specific `.deb` back out without touching the daemon. Both
run safely while the daemon is live (SQLite WAL allows concurrent
readers).

```sh
# List every cached .deb (NAME / SIZE / AGE / HOST(S))
apt-cacher-ultra packages list -config /etc/apt-cacher-ultra/config.toml

# Filter by substring against the .deb filename
apt-cacher-ultra packages list -config /etc/apt-cacher-ultra/config.toml nginx

# Alternate output formats
apt-cacher-ultra packages list -format plain   # one filename per line
apt-cacher-ultra packages list -format json    # JSON array

# Copy a specific .deb out of the pool by exact filename.
# Destination may be a directory (file is named after the .deb) or a path.
apt-cacher-ultra packages copy -config /etc/apt-cacher-ultra/config.toml \
    nginx_1.18.0-1_amd64.deb /tmp/
```

## Recovering a degraded repository

If a suite was adopted while a required index was briefly unavailable upstream,
its snapshot can end up missing that index (for example
`dists/<suite>/main/binary-all/Packages`) and serve an authoritative `404` for
it — which shows up client-side as `apt update` failing to fetch that file.
apt-cacher-ultra heals such snapshots **in place** (no re-adoption, no serving
gap), two ways:

- **Automatically** — on every freshness check where the upstream `InRelease`
  is unchanged, the daemon re-parses the snapshot's signed `Release`, fetches
  any declared-but-missing index, validates it, and inserts it into the live
  snapshot. Nothing to do; this is on by default (`[adoption].repair_skipped_members`).
- **On demand** — force an immediate reconcile of one suite through the admin
  listener (default `127.0.0.1:6789`; protect it with `admin.htpasswd_file`
  and/or the bind address). `host` and `suite` are the canonical host and suite
  path as they appear in the daemon's `adoption_success` / freshness logs;
  `scheme` is optional (default `https`):

```sh
curl -fsS -X POST http://127.0.0.1:6789/reconcile \
    --data-urlencode 'host=packages.microsoft.com' \
    --data-urlencode 'suite=/ubuntu/24.04/prod/dists/noble'
# 202 Accepted = reconcile triggered (runs asynchronously)
# 409 = busy, unknown suite, or no current snapshot
```

The metric `acu_serve_snapshot_index_target_404_total` (and the
`snapshot_index_target_404` WARN log) is the signal that a client's `apt update`
is being denied a required index; `acu_adoption_reconciled_total` rises as the
suite heals.

## Build

```sh
make build           # binary at ./build/apt-cacher-ultra
make test            # unit tests
make lint            # golangci-lint (must be installed)
make deb             # .deb package (nfpm must be installed)
make clean
```

## License

Released into the public domain under [CC0 1.0 Universal](LICENSE).

