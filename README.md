# apt-cacher-ultra

A robust apt repository cache focused on availability under upstream failure.

Designed as a replacement for `apt-cacher-ng` that keeps cache hits fast and
successful even when upstream Ubuntu/Debian/PPA mirrors are slow, broken, or
under DDoS.

## Status

Core functionality complete, currently running it in my test environment,
works if I shut down upstream connectivity.  I expect to reach beta by mid May.
I expect to switch my dev/stg environment by May 8, and prod by May 12.

## Quickstart (when implemented)

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
Acquire::http::Proxy "http://[APT_CACHER_ULTRA_HOSTNAME:3142";
```

For apt repositories using https, you have these choices:

- Make no changes, but apt-cacher-ultra does not cache the associated packages
- Set up an MITM proxy (see the next section).
- Replace "https://" with "http://HTTPS///" in the sources.list entry.

### Enable the MITM HTTPS proxy (optional):

By default `CONNECT` for `https://` repos returns `405` — apt then talks
TLS straight to the upstream and the cache is bypassed for those repos.
Enabling MITM lets the cache decrypt, cache, and re-serve HTTPS sources
by signing per-host leaf certs from a local CA.

1. Add a `[tls_mitm]` block to `config.toml`:

   ```toml
   [tls_mitm]
   enabled            = true
   # allowed_host_regex narrows which upstream hosts MITM will sign for.
   # If left empty, set allow_unconstrained_ca = true (not recommended).
   allowed_host_regex = '^([a-z0-9-]+\.)*(ubuntu\.com|debian\.org)$|^download\.docker\.com$'
   # ca_cert / ca_key empty = auto-generate under <cache.dir>/ca on first start.
   ```

2. Start the daemon once so the CA is materialized, then export it:

   ```sh
   sudo systemctl restart apt-cacher-ultra
   sudo apt-cacher-ultra ca print > apt-cacher-ultra-ca.crt
   ```

3. Set up the CA key on every apt client:

  a. Install the CA and refresh the trust store:

   ```sh
   sudo cp apt-cacher-ultra-ca.crt /usr/local/share/ca-certificates/
   sudo update-ca-certificates
   ```

  b. Place the CA cert and configure apt (and only apt) to use it:

   ```sh
   sudo cp apt-cacher-ultra-ca.crt /usr/ssl/certs/ca-certificates/
   ```

  Then in an `/etc/apt/apt.conf.d` file, add:

  ```
  Acquire::https::CaInfo "/etc/ssl/certs/apt-cacher-ultra-ca.crt";
  ```

4. Generate the client apt-conf snippet (includes the CA fingerprint as
   a comment for verification):

   ```sh
   apt-cacher-ultra --print-apt-conf -config /etc/apt-cacher-ultra/config.toml \
       > /etc/apt/apt.conf.d/00aptcacher
   ```

## Build

```sh
make build           # binary at ./build/apt-cacher-ultra
make test            # unit tests
make lint            # golangci-lint (must be installed)
make deb             # .deb package (nfpm must be installed)
make clean
```
