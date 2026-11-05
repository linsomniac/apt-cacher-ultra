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
challenge_type = "http-01"
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
authority must support the selected challenge method, ordinary account registration without external
account binding, and HTTPS resource URLs on the directory's origin. Cross-origin
resources, redirects to another origin, credentials, query strings and fragments
are rejected. ACME protocol requests use system trust and direct connections; neither
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

## DNS-01 with Lego providers

DNS-01 proves control by creating a TXT record. It needs no inbound challenge
port: the cache and admin listeners can stay on a private network, and
`acme.listen` is ignored. The name must still belong to a publicly delegated DNS
zone when using Let's Encrypt. Select a provider and keep the listener settings
and explicit terms acceptance from the ACME example above:

```toml
[acme]
enabled = true
directory_url = "https://acme-staging-v02.api.letsencrypt.org/directory"
dns_name = "cache.example.net"
challenge_type = "dns-01"
dns_provider = "cloudflare"
# Optional recursive resolvers, as IP:port (bracket IPv6). Empty uses the system.
dns_resolvers = ["1.1.1.1:53", "8.8.8.8:53"]
accept_terms = false
```

The standard build includes the full [Lego provider catalogue](https://go-acme.github.io/lego/dns/),
pinned to v5.6.0. Run `apt-cacher-ultra acme dns-providers` to list the compiled
provider codes. The interactive `manual` and arbitrary command-hook `exec`
providers are excluded from the unattended daemon. `rfc2136` aliases `dnsupdate`;
`acme-dns` aliases `acmedns`. Provider-specific prerequisites still apply; bundling
an SDK does not supply external credential helpers or account setup.

Supply credentials through the selected provider's native Lego environment
variables, including its supported `_FILE` variants. No credentials belong in
the TOML file or command line. For Cloudflare, use
`CLOUDFLARE_DNS_API_TOKEN_FILE=/run/secrets/cloudflare-dns-token`. The token needs
DNS edit and zone read access for the intended zone; use a separate zone-read
token if desired, as described in [Lego's Cloudflare guide](https://go-acme.github.io/lego/dns/cloudflare/).
Restrict credential permissions and scope to the smallest zone the provider
supports. A fresh provider is initialized for each issuance attempt, so replacing
the contents of a credential file is picked up at the next attempt. Changing
environment variables requires a daemon restart.

For the packaged systemd service, create a drop-in such as:

```ini
[Service]
LoadCredential=cloudflare-dns-token:/etc/apt-cacher-ultra/secrets/cloudflare-dns-token
Environment=CLOUDFLARE_DNS_API_TOKEN_FILE=%d/cloudflare-dns-token
```

Keep the source file root-owned and mode 0600, reload systemd, and restart the
service. Systemd makes the loaded credential readable by the service; a changed
source file needs a restart to refresh that copy. For other providers, use their
documented variables in an appropriately protected `EnvironmentFile`, or use
loaded credential files where supported.

For Docker, use [compose.acme-dns.yml](../compose.acme-dns.yml) and
[docker/config.acme-dns.toml](../docker/config.acme-dns.toml). It mounts a token
file and publishes only loopback application ports. Follow the secret ownership
and verified healthcheck instructions below. The daemon and its provider worker
run as the same unprivileged UID; no low-port capability is needed.

Lego handles provider APIs and CNAME delegation. The daemon waits until the
expected TXT value is visible through **every** configured recursive resolver
before asking the CA to validate it. Use resolvers that see the public zone for
split-DNS deployments. The CA performs its own independent DNS lookup. Provider
propagation timeout/polling settings use their native environment variables:
accepted timeouts are 1 second to 1 hour and intervals 1 second to 2 minutes.
Values outside those ranges fail provider initialization. Default values come
from the selected provider (or two minutes/two seconds if it supplies none).

Give this daemon exclusive use of its challenge name or delegated target unless
the provider explicitly supports shared TXT records. For example, Lego's
`dnsupdate` adapter removes the existing TXT record set before inserting its
challenge value. Do not share that name with another concurrent certificate
manager. Delegation can isolate credentials and record ownership from the main
DNS zone. DNS-01 still supports one certificate name here; it does not enable
wildcards or multi-name certificates.

Provider initialization has a 30-second limit. Record creation and cleanup each
have a five-minute limit: some providers, including Route 53, wait for
asynchronous DNS API changes inside these operations. Cleanup gets a fresh
budget after cancellation or a partially failed creation. DNS issuance has a
70-minute budget for provider operations, propagation and ACME work, plus any
cleanup still needed. The packaged systemd service and DNS Compose example allow
six minutes for shutdown so cleanup can finish. Application listeners stop
accepting traffic while cleanup runs. The systemd unit uses `KillMode=mixed` to
send SIGTERM to the daemon first, preserving the provider worker for cleanup;
other supervisors should allow the same grace period and signal the daemon first.

Provider SDKs run in a short-lived process of the same executable so a stuck SDK
can be terminated and its raw output kept out of daemon logs. Cancellation first
interrupts the SDK operation through its context. A responsive worker retains
its provider state for cleanup; an unresponsive worker is killed after a short,
bounded grace period. This is **not a security sandbox**: providers inherit the
daemon's environment and permissions. Their networking and proxy behavior follow their own SDK settings,
independently of the ACME client's origin restrictions and `upstream.proxy`.

Cleanup is best effort. A crash, hard timeout, or remote API failure can leave a
TXT record behind, especially where the provider keeps record IDs only in memory.
A reported cleanup failure stops that issuance attempt and retains any existing
valid certificate. Check provider audit logs and remove only this attempt's
stale TXT value after confirming no active attempt uses it. Never delete an
entire shared record set as a generic recovery step.

Status exposes bounded categories such as `dns_provider_configuration_failed`,
`dns_present_failed`, `dns_propagation_timeout`, `dns_cleanup_failed`, and
`dns_worker_failed`. Provider error bodies, debug output, credentials and tokens
are suppressed. Diagnose configuration failures using the provider's documented
required variables and file permissions; check scoped API permissions and DNS
visibility for creation/propagation failures. Provider constructors are checked
before account/order activity, but a valid saved certificate still starts
offline without initializing a provider.


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
at least one hour remaining and a renewal target still in the future. Crash recovery reserves the attempt budget (five minutes for HTTP-01, 70 for
DNS-01) plus exponential backoff. A completed failure uses normal backoff,
starting at about a minute and growing to about an hour; an authority's longer `Retry-After` takes precedence, including
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
effective chain validity dates, and a bounded error category. ACME adds the name, directory, challenge type, DNS provider where applicable, last
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

This uses local processes, real HTTP-01 and DNS-01 validation, and trusted test
roots. DNS-01 uses the real Lego RFC2136 adapter against an authenticated local
DNS server. It tests issuance, renewal, cleanup and offline restart without
contacting Let's Encrypt or a public DNS-provider API. Separate tests exercise
Lego's Cloudflare adapter against a local API fixture, including credential-file
loading and preservation of the provider's record ID until cleanup.

The full provider catalogue adds many cloud SDK dependencies and increases build
size and memory use. Build/test with `GOFLAGS=-p=2` on small machines. Custom
builds can use Lego's `lego_dnslight` plus specific provider tags, for example
`go build -tags lego_dnslight,lego_dns_cloudflare ./cmd/apt-cacher-ultra`; the
provider-list command reflects what that binary includes. Standard releases
include all automated providers.
