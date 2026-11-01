# Server HTTPS and ACME

The cache TLS listener and the admin listener can use PEM files or one shared
ACME certificate. HTTP remains the default. These certificates identify this
daemon; the independent `[tls_mitm]` CA still signs intercepted repository
connections. Turning on server HTTPS does not grant access or authenticate a
client. Keep the proxy private and configure admin authentication.

## Existing certificates and Certbot

```toml
[admin]
listen = "127.0.0.1:6789"
tls_cert = "/etc/apt-cacher-ultra/server/fullchain.pem"
tls_key = "/etc/apt-cacher-ultra/server/privkey.pem"
htpasswd_file = "/etc/apt-cacher-ultra/admin.htpasswd"
```

The same `tls_cert` and `tls_key` options apply to `[cache]`, together with
`listen_tls`. Both listeners require TLS 1.2 or newer. The admin port becomes
HTTPS only, without an HTTP redirect. Update scrapers, bookmarks and healthchecks.
The configured files must be readable by the daemon. Keep private keys accessible
only to their owner and the necessary service group. Certbot symlinks work, but
the service must be able to traverse and read their targets under systemd's
filesystem restrictions. A deploy hook can instead copy a pair to service-owned
files. Publish each file by rename; the daemon tolerates the interval between
the two replacements.

Every minute the daemon validates a complete candidate pair before publishing
it to new handshakes. A malformed, mismatched, future or expired replacement
leaves the previous valid pair active and marks status degraded. At expiry, new
TLS handshakes fail, including resumptions; existing connections can finish.
Changes to paths or other TOML settings still require a restart.

## Built-in ACME

```toml
[cache]
dir = "/var/cache/apt-cacher-ultra"
listen = "127.0.0.1:3142"
listen_tls = "127.0.0.1:3143"
use_acme = true

[admin]
listen = "127.0.0.1:6789"
use_acme = true
htpasswd_file = "/etc/apt-cacher-ultra/admin.htpasswd"

[acme]
enabled = true
directory_url = "https://acme-staging-v02.api.letsencrypt.org/directory"
dns_name = "cache.example.net"
listen = "127.0.0.1:8080"
# Set true only after reviewing and accepting this authority's terms.
accept_terms = false
```

Replace the example DNS name and explicitly accept the chosen authority's terms
before startup. Start with staging to validate routing. Staging certificates are
not trusted by normal browsers; use a separately trusted test root for testing.
For production, change the directory to
`https://acme-v02.api.letsencrypt.org/directory`. Changing directories creates a
separate account/certificate namespace and requires successful issuance before
application traffic starts. Staging material is never reused for production.

Select `use_acme` independently for cache and admin; leave that listener's file
options empty. One ASCII DNS name is supported, without a wildcard, trailing dot
or IP address. International names must use their ASCII representation. The
authority must support HTTP-01, ordinary account registration without external
account binding, and HTTPS resource URLs on the directory's origin. Cross-origin
resources, redirects to another origin, credentials, query strings and fragments
are rejected. Outbound requests use system trust and direct connections; neither
`upstream.proxy` nor proxy environment variables apply. There is no insecure
certificate-verification switch. Install a private authority's root in the
service's system trust store when needed.

HTTP-01 validation reaches **public TCP port 80**, even when the local listener
uses 8080. Point the name's A/AAAA records at the forwarding host and make every
advertised address reachable. Forward only the challenge path, preserving the
configured hostname. For example, in an existing nginx virtual host:

```nginx
location ^~ /.well-known/acme-challenge/ {
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header Host cache.example.net;
}
```

Use a server block for `cache.example.net` and a separate default block rejecting
unknown hosts. Application ports can remain private. The dedicated challenge
listener serves only active GET/HEAD tokens, has bounded connections and timeouts,
and never exposes status, metrics or maintenance endpoints. It stays bound while
the daemon runs, including between renewals. A challenge listener failure shuts
the daemon down. Another service already occupying that socket prevents startup
before registration or issuance begins.

For direct binding to `:80`, an optional systemd drop-in is shown in
[`acme-port80.conf.example`](../packaging/systemd/acme-port80.conf.example).
Install it only for that deployment; the packaged service has no new capabilities.
Unprivileged forwarding to 8080 avoids needing this capability.

## Persistence, renewal and recovery

State lives under `cache.dir/acme/`, with directories restricted to their owner
and private records mode 0600. Back up the whole directory securely with the
daemon stopped, preserve ownership/permissions, and restore it with the cache.
It contains account and certificate private keys. Do not mount only individual
JSON files or edit them while the daemon runs. Each certificate and key are one
atomic record, and an advisory lock serializes account and issuance changes.
Shared network filesystems with unreliable locking or fsync are unsupported.

A valid saved certificate allows startup without contacting the CA. Otherwise,
startup needs successful bounded issuance and fails without serving application
traffic if that cannot complete. Retry deadlines are durable before contacting
the authority, so service restart loops do not place repeated orders. Renewal
uses the served chain's effective lifetime: normally at one-third remaining, halfway
for lifetimes below ten days, with earlier jitter. A newly issued chain must have
at least one hour remaining and a renewal target still in the future. Failed attempts back off to
about an hour; an authority's longer `Retry-After` takes precedence, including
across timeout and restart. There is no ARI support or ARI rate-limit exemption.

A renewal failure keeps the previous certificate while it remains valid. Expired
certificates cause TLS handshakes to fail; the independent HTTP proxy remains
available. Repair routing, clock, trust or permissions and allow the recorded
retry deadline to pass. Storage failures stop automatic issuance until restart
and successful storage validation. Confirmed account deactivation or changed
terms require operator attention; accounts are not silently replaced.

Do not delete retry state to bypass a rate limit. If a deliberate account reset
is required, stop the service and securely archive the entire authority namespace
before replacing it; this creates new account/order activity and does not reset
the authority's limits. Certbot users can retain their existing automation and
use the file mode above, without migrating account keys.

## Status and healthchecks

The status HTML and JSON report certificate source, state, SHA-256 fingerprint,
effective chain validity dates, and a bounded error category. ACME adds the name, directory, last
success, next eligible attempt, and process-local outcome counts. Raw authority
errors, challenge tokens and keys are excluded. `listeners[].addr` stays a bind
address; `scheme` describes HTTP or HTTPS. TLS sections are absent when disabled.

Prometheus exposes `acu_server_tls_not_after_unixtime`,
`acu_server_tls_available` and `acu_server_tls_degraded` with fixed `listener`
labels (`cache` or `admin`). ACME attempt, success and failure gauges are
`acu_acme_attempts`, `acu_acme_successes`, `acu_acme_failures`; counts reset on
process restart. Allow up to 30 seconds for metric refresh.

An HTTPS healthcheck must verify the certificate against the right hostname and
roots, and supply credentials if `htpasswd_file` is enabled. For curl, put the
URL, address mapping and credentials in a file readable only by the service:

```text
url = "https://cache.example.net:6789/healthz"
resolve = "cache.example.net:6789:127.0.0.1"
user = "healthcheck:replace-with-a-strong-password"
# For a private or staging CA only, add its trusted root:
# cacert = "/run/secrets/authority-root.pem"
```

Run `curl --fail --silent --show-error --max-time 4 --config /path/to/file`.
The standard Docker image's HTTP healthcheck must be overridden for HTTPS or
authenticated admin. See the explicit [ACME Compose example](../compose.acme.yml),
which adds curl for this purpose, keeps application ports on host loopback, drops
all capabilities, persists state, and publishes only the challenge on public 80.
The example requires configuration and secret files before it can start.
Compose mounts these secret files from `docker/secrets/`: make the credential
files readable by container UID 10001 (for example, owner 10001, mode 0400), and
keep their host directory private. Use a trusted staging/private root in
`authority-root.pem`; for production with system trust, remove that optional
secret and omit `cacert` from the healthcheck config. Never commit credentials.

The container smoke test uses freshly generated test PKI and credentials:

```sh
docker build -t apt-cacher-ultra:tls-test .
docker build -f docker/Dockerfile.acme --build-arg ACU_BASE_IMAGE=apt-cacher-ultra:tls-test -t apt-cacher-ultra:tls-healthcheck .
ACU_TLS_SMOKE_IMAGE=apt-cacher-ultra:tls-healthcheck go test ./internal/servertls -run TestContainerHTTPSHealthcheck -count=1
```

## Independent interoperability test

CI installs pinned Pebble binaries and runs `TestPebbleInteroperability` with
`ACU_PEBBLE_BIN_DIR` pointing to them. To run locally:

```sh
mkdir -p /tmp/acu-pebble-bin
GOBIN=/tmp/acu-pebble-bin go install github.com/letsencrypt/pebble/v2/cmd/pebble@v2.10.1
GOBIN=/tmp/acu-pebble-bin go install github.com/letsencrypt/pebble/v2/cmd/pebble-challtestsrv@v2.10.1
ACU_PEBBLE_BIN_DIR=/tmp/acu-pebble-bin go test -race ./internal/acme -run TestPebbleInteroperability -count=1
```

This uses local processes, real HTTP-01 validation and trusted test roots. It
tests issuance, renewal and offline restart without contacting Let's Encrypt.
