# ACME certificates for exposed interfaces

## Objective

Allow the daemon to automatically obtain and renew a server certificate via an
ACME authority, with Let's Encrypt as the default. The certificate thus
obtained can be used for the HTTPS admin interface, the proxy's HTTPS
listener, or both.

This feature does not change the operation of the MITM proxy: `[tls_mitm]`
keeps its local CA, per-host certificate generation, and CONNECT request
handling. An ACME certificate is never used as a MITM CA, and the MITM CA is
never sent to an ACME authority.

## Configuration

The ACME client has a single common configuration:

```toml
[acme]
# Disabled by default: no ACME account, challenge, or certificate is created.
enabled = false

# Let's Encrypt ACME production.
directory_url = "https://acme-v02.api.letsencrypt.org/directory"

# Single FQDN placed in the server certificate. Shared by all ACME surfaces:
# cache.listen_tls and admin.listen must be reachable under this same name
# (different ports allowed).
dns_name = "cache.example.net"

# Local address temporarily reserved for the HTTP-01 challenge.
listen = "0.0.0.0:80"

# Renewal: the certificate is renewed when its expiration is less than this
# number of days away. 7 by default (Let's Encrypt certificates: 90 days of
# validity).
renew_before_days = 7

# Certificate expiration check cadence. Must remain much lower than the
# certificate's validity period (90 days for Let's Encrypt) so as not to miss
# the renewal.
check_interval = "12h"
```

`directory_url` and `dns_name` are required when `enabled = true`;
`listen` defaults to `0.0.0.0:80` when the key is absent. The documentation
must suggest the Let's Encrypt staging URL for testing in order to avoid
production rate limits:

```toml
directory_url = "https://acme-staging-v02.api.letsencrypt.org/directory"
```

The `dns_name` is deliberately unique: the certificate covers a single FQDN,
with no additional SAN or wildcard. The surfaces that use it —
`cache.listen_tls` and `admin.listen` — must therefore be reachable under this
same name, on different ports if necessary. Distinct names per surface would
require one certificate per name; that is out of scope for this version.

Each listener decides locally whether to use the ACME certificate:

```toml
[cache]
listen_tls = "0.0.0.0:443"
use_acme = true

[admin]
enabled = true
listen = "0.0.0.0:8443"
use_acme = true
```

`cache.use_acme` selects ACME for `cache.listen_tls`. `admin.use_acme`
selects ACME for `admin.listen`. The same certificate and its renewal are
shared by both listeners when selected.

The existing `cache.tls_cert` / `cache.tls_key` and `admin.tls_cert` /
`admin.tls_key` options continue to designate a manual PEM certificate. For a
given surface, `use_acme = true` is incompatible with the manual PEM options:
there is exactly one certificate source.

## HTTP-01 challenge

The HTTP-01 protocol requires that the ACME authority be able to reach:

```text
http://<dns_name>/.well-known/acme-challenge/<token>
```

on public TCP port **80**. `acme.listen` can designate any valid local address,
including a specific IP or a non-privileged port such as `127.0.0.1:8080`,
but a local port other than 80 imperatively requires a reverse proxy, NAT or
port forwarding that publishes external port 80 to this address. Changing
only `acme.listen` to `:8080` does not allow HTTP-01 validation.

The challenge server is started just before an issuance or renewal, serves
only the `/.well-known/acme-challenge/` path, then is stopped after
validation, failure or timeout. Other paths must not expose the application's
interfaces. This strategy corresponds to Certbot's standalone mode while
avoiding a permanent HTTP listener.

Starting a request must fail explicitly if `acme.listen` is already occupied,
notably by `cache.listen`, `cache.listen_tls` or `admin.listen`. The listener
is maintained until the authority has finished the challenge, and a lock
prohibits concurrent issuances or renewals.

DNS-01 is out of initial scope. It would avoid exposing port 80, but requires
authenticated integration with a DNS provider and additional configuration.

## Configuration validation

When `[acme].enabled = true`, the daemon validates at startup:

1. `directory_url` is a valid ACME HTTPS URL, without credentials, fragment
   or ambiguous form.
2. `dns_name` is a valid FQDN, without scheme, path, port, IP address or
   wildcard.
3. `acme.listen` is a valid local `host:port` address.
4. At least one of `cache.use_acme` or `admin.use_acme` is enabled.
5. `cache.use_acme` requires `cache.listen_tls`.
6. `admin.use_acme` requires `admin.enabled`.
7. Each ACME target is free of manual `tls_cert` and `tls_key`.
8. `acme.renew_before_days` is an integer greater than or equal to 1.
9. `acme.check_interval` is a strictly positive duration.

When `[acme].enabled = false`, any `use_acme = true` is a configuration error.
The existing readability and key-matching checks for PEM files remain
applicable to listeners that do not use ACME.

When `[acme].enabled = true`, a warning is emitted at startup — without
blocking — when `acme.listen` is in address conflict with an effective daemon
listener: same port as `cache.listen`, `cache.listen_tls` or `admin.listen`,
with identical hosts or either one wildcard (`0.0.0.0`, `::`). This conflict is
not a configuration error: the challenge server's bind fails explicitly at
issuance or renewal time.

## Lifecycle and storage

The client keeps the ACME account, the certificate's private key, the full
PEM chain and the metadata needed for renewal in `<cache.dir>/acme/`. This
directory is persistent and created with restrictive permissions; private
keys, challenge tokens and signed URLs are never logged.

The stored account and certificate are reused only if they come from the
configured `acme.directory_url` directory: after a directory change (for
example from the Let's Encrypt staging environment to production), the daemon
registers a new account and obtains a new certificate. A stored certificate
that is unreadable, or whose chain does not match the private key, is treated
as absent and reissued. Before issuing, the daemon re-reads the storage once
the inter-process lock is acquired: if another instance sharing the directory
has just obtained a valid certificate, it reuses it instead of issuing a
duplicate.

At first startup, the daemon obtains a certificate before starting to serve
the surface(s) configured for ACME. The certificate's expiration is checked
every `acme.check_interval` (12h by default); the certificate is renewed when
its expiration is less than `acme.renew_before_days` days away (7 by
default). If this window covers the entire lifetime of the certificate (for
example 7 days for 6-day certificates), it is reduced to one third of this
lifetime, with a warning in the log: without this, each certificate would be
due for renewal as soon as it is issued. After a renewal failure, the delay
between two attempts doubles with each error: 1 minute after the first, then
2, 4, 8 minutes, etc., capped at 1 hour; a successful renewal resets the
delay to 1 minute. This backoff avoids any request loop towards the
authority.

A valid certificate already stored continues to be served when a renewal
temporarily fails. New TLS handshakes must use the renewed certificate
without restart; existing connections are not interrupted. If no usable
certificate is available at startup (initial issuance failure, or expired
certificate whose renewal fails), the daemon aborts its startup: explicit
error in the log, non-zero exit code, no surface is served, including the
plain HTTP proxy. A partial startup where only the ACME surfaces would be
unavailable is forbidden.

## Observability

The startup logs and the admin page expose, without secrets:

- ACME activation;
- the requested FQDN;
- the authority URL;
- the selected surfaces;
- the fingerprint and validity dates of the current certificate;
- the last issuance or renewal error.

This state appears in an ACME section distinct from the `[tls_mitm]` state.

## Validation and documentation to implement

The implementation must cover:

- configuration validations and ACME/PEM conflicts;
- issuance and the HTTP-01 challenge against a local test ACME server;
- operation behind a test proxy publishing the local challenge on its
  simulated port 80;
- explicit startup failure (non-zero exit code, no surface served) when no
  usable certificate is available at first startup;
- persistence after restart, renewal and reload without restart for the
  admin and the proxy;
- validation of `acme.renew_before_days` and `acme.check_interval`, as well
  as the address conflict warning between `acme.listen` and the daemon's
  listeners;
- renewal triggered when expiration is less than `acme.renew_before_days`
  days away;
- the backoff between retries after a renewal failure: 1 minute after the
  first error, doubling with each failure, capped at 1 hour, reset to 1 minute
  after a success;
- no regression of manual TLS;
- no regression and independence of MITM.

The user documentation will describe the DNS, firewall, NAT or reverse proxy
exposure required for public port 80, the persistent storage of the ACME
directory, the constraint of a single FQDN shared by the ACME surfaces and
the mandatory use of the Let's Encrypt staging environment before production
tests.
