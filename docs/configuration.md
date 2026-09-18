# Configuration reference

apt-cacher-ultra reads TOML from `/etc/apt-cacher-ultra/config.toml` by default.
Use `apt-cacher-ultra -config /path/to/config.toml` to select another file. The
[annotated example](../packaging/config/config.toml.default) is installed by the
Debian package; see the [README](../README.md) for installation and client setup.

Edit the file, then restart the daemon to apply changes:

```sh
sudo systemctl restart apt-cacher-ultra
sudo journalctl -u apt-cacher-ultra -n 50 --no-pager
```

There is no configuration reload or `SIGHUP` reload handler. Certificate and
keyring changes also require a restart. The admin password file is an exception:
its contents are checked for changes on admin requests, as described below.

## Syntax and defaults

Tables below show **loader defaults when a key is omitted**. The shipped example
selects those defaults and explicitly supplies `cache.dir`; that directory has
no loader default and must already exist and be writable by the daemon user.
The loader probes writability with a temporary file. Unknown TOML keys are
currently ignored, so check spellings carefully.

- Durations are quoted strings: `"30s"`, `"5m"`, `"24h"`, `"2d"`, or `"1d12h"`.
  Supported units are `ns`, `us`/`µs`, `ms`, `s`, `m`, `h`, and `d` (24 hours).
  Use `"0s"` for zero; zero only disables an option where documented below.
- Booleans are unquoted `true` or `false`; integers are unquoted numbers.
- Host regular expressions use Go's RE2 syntax. Single-quoted TOML strings keep
  backslashes literal: `'^deb\.debian\.org$'`. Use `^` and `$` to match the
  whole hostname; an unanchored regex can match a substring.
- Listen addresses use `host:port`, `:port`, or `[IPv6]:port`; ports must be
  numeric and in `1`–`65535`. Prefer absolute filesystem paths.
- `[[trusted_signer]]`, `[[remap]]`, and `[[mirror]]` are repeatable tables.
  Omit the entire table when no rule is wanted; an empty rule is invalid.

The current defaults enable TLS interception with an unconstrained local CA,
allow all upstream hosts and IP ranges, and disable snapshot adoption. Clients
using an HTTPS proxy must trust the cache CA. Restrict who can reach the proxy
with its bind address and network access controls; `admin.htpasswd_file` protects
only the admin listener.

## `[cache]`: storage and proxy listeners

| Key | Type | Default | Behavior and constraints |
| --- | --- | --- | --- |
| `dir` | string | **Required** | Existing writable cache root. The package example uses `/var/cache/apt-cacher-ultra`. Stores the database, blobs, temporary files, and, by default, the MITM CA. |
| `listen` | string | `"0.0.0.0:3142"` | HTTP proxy bind address. Empty string also selects the default. |
| `listen_tls` | string | `""` | Additional TLS proxy listener; empty disables it. Set together with `tls_cert` and `tls_key`. |
| `tls_cert` | string | `""` | Readable PEM server certificate for `listen_tls`; validated with its key when the daemon starts. |
| `tls_key` | string | `""` | Readable PEM private key matching `tls_cert`. |
| `advertise_host` | string | `""` | Client-facing hostname or `host:port` for `--print-apt-conf`, without a scheme or path. A missing port uses the port from `listen`. Does not change listener binding or request routing. |

`listen_tls`, `tls_cert`, and `tls_key` must be either all set or all empty.
These settings secure the client-to-proxy connection; `[tls_mitm]` separately
controls interception of upstream HTTPS repositories.

When `advertise_host` is empty, `--print-apt-conf` uses `listen`. The command
refuses unspecified bind hosts (`0.0.0.0`, `::`, or an empty host), so a typical
installation should set, for example, `advertise_host = "cache.example.net"`.
The command currently prints both HTTP and HTTPS proxy directives even when
MITM is disabled; set its `Acquire::https::Proxy` value to `"DIRECT"` in that
case. Omitting the line inherits the HTTP proxy setting, as described in
[APT's HTTPS options](https://manpages.debian.org/unstable/apt/apt-transport-https.1.en.html#OPTIONS).

The packaged systemd service runs as `apt-cacher-ultra` and permits writes under
`/var/cache/apt-cacher-ultra`. If moving `cache.dir` or `tls_mitm.ca_storage_dir`,
give the service user access and add the new writable location through a
systemd override of `ReadWritePaths`. `ProtectSystem=strict` and `ProtectHome=true`
also restrict access; placing cache or key files in a home directory requires
additional service policy changes. See the [packaged service](../packaging/systemd/apt-cacher-ultra.service).

## `[upstream]`: fetching and access policy

| Key | Type | Default | Behavior and constraints |
| --- | --- | --- | --- |
| `connect_timeout` | duration | `"30s"` | Connection establishment timeout. Nonnegative; `"0s"` selects the default. |
| `total_timeout` | duration | `"5m"` | Overall budget for a fetch, including its retries. Nonnegative; `"0s"` selects the default. |
| `idle_read_timeout` | duration | `"60s"` | Accepted and logged, but currently **does not enforce an idle-read timeout**. `total_timeout` bounds the fetch. Nonnegative; `"0s"` selects the default. |
| `max_retries` | integer | `3` | Extra attempts after the initial fetch attempt: up to four attempts with the default. Nonnegative; `0` currently selects `3`, so it does not disable retries. |
| `max_concurrent_per_host` | integer | `8` | Shared per-canonical-host concurrency limit for request fetches and background work. Nonnegative; `0` selects `8`, not unlimited. |
| `unreachable_cooldown` | duration | `"30s"` | After a failed dial, subsequent dials within this window use a short probe and stop retrying if that probe fails. `"0s"` disables this fast-failure behavior. Nonnegative. |
| `unreachable_probe_timeout` | duration | `"1s"` | Additional dial deadline while a host is in cooldown. `"0s"` removes this shorter deadline but still suppresses retries after a failed probe. Nonnegative. |
| `allowed_host_regex` | array of strings | `['^.*$']` | Allow a canonical hostname if any regex matches. Checked again for redirect destinations. Explicit `[]` denies all upstream hosts. Hostnames have no port. |
| `deny_target_ranges` | array of strings | `[]` | IPv4/IPv6 CIDR ranges blocked when dialing resolved addresses, including redirect targets. Empty means no IP-range filter. |
| `allow_https_to_http_redirect` | boolean | `true` | Follow HTTPS-to-HTTP redirects. `false` rejects downgrades. Host and IP policy still apply. |

The first failed dial can put the next retry of the same fetch into cooldown;
it is not necessary to wait for another client request. Retry counts are upper
bounds: nonretryable failures, cooldown probes, and `total_timeout` can stop a
fetch earlier. Adoption's member retries are a separate outer retry policy.

These access policies govern upstream activity. Ordinary HTTP cache hits can
still be served after a host is removed from the allowlist. HTTPS `CONNECT`
checks the allowlist before opening the intercepted connection, including for
cached content. These options are not client authentication or a cache purge.

For an installation that should fetch only public Debian mirrors, an example is:

```toml
[upstream]
allowed_host_regex = ['^deb\.debian\.org$', '^security\.debian\.org$']
deny_target_ranges = [
  "127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
  "169.254.0.0/16", "::1/128", "fc00::/7", "fe80::/10",
]
allow_https_to_http_redirect = false
```

Adjust this policy for your repositories and their CDN redirect hosts; the CIDR
example intentionally blocks private/LAN mirrors. HTTPS-to-HTTP redirects lose
transport confidentiality. Apt's signature/hash checks can protect the integrity
of signed repository content over HTTP, but do not cover arbitrary unsigned files.

## `[freshness]`: checking repository updates

| Key | Type | Default | Behavior and constraints |
| --- | --- | --- | --- |
| `cooldown` | duration | `"60s"` | Minimum interval between freshness checks for a suite, shared by request-triggered and scheduled checks. Nonnegative; `"0s"` selects the default. |
| `periodic_refresh` | duration | `"15m"` | Age after the last successful check at which a suite becomes due for background refresh. Scheduler checks for due suites every quarter of this duration, with a `5s` minimum tick. Nonnegative; `"0s"` selects the default and does not disable refresh. |
| `max_concurrent_adoptions` | integer | `2` | Global concurrent snapshot-adoption limit. `0` means unlimited; must be nonnegative. |

Freshness checks detect changed repository metadata. `[adoption]` determines
whether a changed suite is staged and published as a new coherent snapshot.

## `[adoption]`: snapshots, trust, and recovery

| Key | Type | Default | Behavior and constraints |
| --- | --- | --- | --- |
| `enabled` | boolean | `false` | Enable adoption of new snapshots. When false, freshness differences are recorded without adopting them. |
| `require_signature` | boolean | `true` | Require a signed envelope for inline metadata. `false` permits unsigned inline content and logs a startup warning; it does not disable verification of signatures that are present. See trust details below. |
| `require_pinned_signer` | boolean | `false` | Reject adoption when no `[[trusted_signer]]` matches the canonical host. Evaluated before `accept_any_signer`. |
| `accept_any_signer` | boolean | `false` | For hosts without matching signer pins, skip both signer trust checks **and cryptographic signature verification**. Signature structure is still checked when present. Matching pins keep normal verification. Logs a startup warning. |
| `allow_short_keyid` | boolean | `true` | Accept signatures containing only a legacy 64-bit issuer key ID when it uniquely identifies a loaded trusted key and cryptographic verification succeeds. `false` requires the long issuer fingerprint. |
| `keyring_dirs` | array of strings | `[]` | Extra startup keyring directories containing `.gpg` or `.asc` files. Entries must be nonempty absolute paths; nonexistent directories are skipped. Adds to built-in directories and bundled keys. |
| `architectures` | array of strings | `[]` | Architectures to adopt; empty keeps all. Filters recognized per-architecture Packages, Sources, pdiff indexes, Contents, cnf Commands, and dep11 Components. Use `"source"` for source indexes. Architecture-independent `"all"` content is always retained. |
| `required_architectures` | array of strings | `[]` | Additional guard: each declared Packages/Sources index group for these architectures must have at least one fetched compression variant before adoption commits. Empty disables this additional guard; see the built-in guard below. |
| `tolerate_optional_member_failures` | boolean | `true` | Skip optional metadata whose fetch or integrity checks fail after retries, allowing the remaining snapshot to commit. Skipped content is unavailable until repaired. Packages/Sources indexes and their pdiff indexes do not receive this tolerance. |
| `member_retry_count` | integer | `2` | Extra member-fetch attempts after availability or integrity failures. `0` means one attempt. Must be nonnegative. `404`/`410` skips are not retried in this loop. |
| `member_retry_delay` | duration | `"30s"` | Delay between member retries. Nonnegative; `"0s"` means no delay. |
| `repair_skipped_members` | boolean | `true` | After an unchanged successful freshness check, repair integrity/availability skips and reconcile missing declared requestable indexes in the live snapshot. The latter also covers required indexes previously skipped with `404`/`410`. |
| `hot_prefetch_budget` | duration | `"5m"` | Wall-clock budget for fetching hot package versions before publishing a new snapshot. `"0s"` removes this overall guard; each fetch still has upstream limits. Nonnegative. |

### Trust and signer pins

The default trust set combines bundled archive keys with `.gpg`/`.asc` files in
`/etc/apt/trusted.gpg.d`, `/etc/apt/keyrings`, `/usr/share/keyrings`, and
`keyring_dirs`. Keys are read at startup. Without a matching signer pin, any
loaded trusted key can verify a suite; the cache does not derive per-source
restrictions from a client's `Signed-By` configuration.

`accept_any_signer = true` makes client-side apt verification the trust boundary
for unpinned suites: the cache continues serving the original signed bytes, but
does not verify them cryptographically. `require_pinned_signer = true` still
rejects unpinned suites, so combining the two does not bypass pin requirements.
`require_signature = false` also allows unsigned data through the inline
verification path. The detached fallback still requires both `Release` and
`Release.gpg`; disabling this setting does not make Release-only repositories
adoptable.

### Architecture and member availability

Both architecture lists accept at most 32 entries, each matching
`^[a-z][a-z0-9]*$` (for example `amd64`, `arm64`, `i386`, `source`). When
`architectures` is nonempty, every `required_architectures` entry must also be
in it. A list such as `architectures = ["amd64", "source"]` includes
architecture-independent `all` indexes automatically. Unrecognized member paths
and architecture-independent translations/icons are not filtered.

The daemon always requires a fetched variant for each declared `binary-all`
Packages group and for declared Packages/Sources groups selected by a nonempty
`architectures` list. This applies even when `required_architectures = []`.
The explicit required list is useful, for example, when adopting all
architectures but requiring availability for only `amd64`.

`404`/`410` member responses are generally skips, independently of optional-member
tolerance, but the index-group guards can block the resulting adoption. Other
availability or integrity failures on required indexes abort adoption. A failed
adoption leaves the previous snapshot serving. Automatic repair does not retry
every optional `404`/`410` artifact; manual reconciliation is available through
[`POST /reconcile`](../README.md#recovering-a-degraded-repository).

## `[hot_packages]`, `[hold_packages]`, and `[retention]`: package working set

| Section / key | Type | Default | Behavior and constraints |
| --- | --- | --- | --- |
| `hot_packages.window` | duration | `"24h"` | Look back this far for client-requested packages when choosing versions to prefetch during adoption. `"0s"` disables hot-package prefetch. Nonnegative. |
| `hold_packages.window` | duration | `"24h"` | Grace after GC first notices a cached URL is no longer retained by recency or current-snapshot rules. `"0s"` permits deletion in that GC batch. Nonnegative. |
| `retention.max_versions_per_package` | integer | `3` | Newest distinct Debian versions retained and eligible for hot prefetch per package name and architecture, per suite. Must be at least `1`. |

Hot prefetch runs **before** the snapshot switches, using package identities
requested from the previous snapshot; it does not download every package in the
repository. The first adoption has no prior hot set.

The version limit is not a hard cap on cached versions or disk usage. Recent
requests within `gc.url_path_ttl` protect older versions too. Current snapshot
metadata and vouched artifacts without a rankable package identity remain
protected; never-requested prefetches are not protected indefinitely merely
because they were prefetched. Once all retention guards cease to apply, the hold
grace starts; bytes are later reclaimed after references disappear and
`gc.blob_grace` expires. URL expiration requires `gc.enabled = true` and a nonzero
`gc.url_path_ttl`.

## `[integrity]`: corruption checks

| Key | Type | Default | Behavior and constraints |
| --- | --- | --- | --- |
| `validate_at_rest_interval` | duration | `"24h"` | Periodically rehash blobs reachable from current snapshots. `"0s"` disables this background scan. Nonnegative. |
| `validate_at_rest_workers` | integer | `4` | Parallel scan workers. Must be at least `1` when the interval is positive; `0` is allowed only with scanning disabled. Never negative. |
| `refuse_unvouched_debs` | boolean | `false` | Reject `.deb` requests with `502` and `Retry-After` when the path lacks a current-snapshot hash and all current snapshots for that host have proven complete package coverage. Inert when adoption is disabled; incomplete coverage retains the permissive fetch behavior. |

These switches do not disable the normal download-time hash checks. A detected
at-rest mismatch logs an error and removes the corrupt pool file. Package misses
can fetch it again; affected snapshot metadata fails closed until restored.

## `[gc]`: garbage collection

| Key | Type | Default | Behavior and constraints |
| --- | --- | --- | --- |
| `enabled` | boolean | `true` | Run startup and periodic garbage collection, including the startup orphan-file scan. `false` disables those passes and logs a warning. |
| `interval` | duration | `"1h"` | Periodic GC cadence. Must be positive; disable through `enabled`. |
| `batch_size` | integer | `100` | Maximum URL-path/blob rows processed per GC batch. At least `1`. |
| `snapshot_batch_size` | integer | `10` | Maximum snapshots removed per batch. Snapshot deletion can cascade through many member rows. At least `1`. |
| `max_tick_duration` | duration | `"5m"` | Budget for startup/periodic GC passes, checked between batches; remaining work resumes later. Must be positive. |
| `blob_grace` | duration | `"5m"` | Grace after a blob's reference count reaches zero before deletion. At least `"1s"`, and greater than twice `heartbeat_interval`. |
| `keep_displaced` | integer | `3` | Recent displaced snapshots kept per suite for inspection. `0` retains none; must be nonnegative. Does not by itself retain superseded package binaries. |
| `pool_scan_workers` | integer | `4` | Workers for the startup orphan-file scan. At least `1`. |
| `heartbeat_interval` | duration | `"60s"` | Refresh in-progress adoption liveness and blob protection. Positive and subject to both bounds below. |
| `url_path_ttl` | duration | `"168h"` | Requests within this window protect cached URL rows. Older or never-requested rows are evaluated against snapshot/version retention and hold grace. `"0s"` disables URL-path expiration; other values must be at least `"1m"`. |

Both heartbeat conditions must hold:

```text
2 × gc.heartbeat_interval < gc.blob_grace
gc.heartbeat_interval < max(upstream.total_timeout × upstream.max_retries, 30m)
```

The second expression is the implementation's stale-adoption grace formula;
it is separate from the actual fetch retry budget. All `[gc]` validation still
applies when `enabled = false`.

## `[admin]`: status, metrics, and maintenance

The separate **HTTP** admin listener serves the status page, `/metrics`, and
`/healthz`, plus the mutating `POST /reconcile` maintenance endpoint. It is not a
read-only interface. Keep access restricted to trusted operators; if exposed
beyond loopback, protect the network path or place it behind a TLS reverse proxy.
HTTP Basic authentication does not encrypt credentials in transit.

| Key | Type | Default | Behavior and constraints |
| --- | --- | --- | --- |
| `enabled` | boolean | `true` | Bind the admin listener and run its background refresh work. `false` leaves proxy service running and logs a warning. |
| `listen` | string | `"127.0.0.1:6789"` | Admin bind address. A non-loopback address without authentication emits a startup warning. Explicit empty string is invalid when enabled. |
| `htpasswd_file` | string | `""` | Optional readable Apache htpasswd file with at least one user; only bcrypt (`$2a$`, `$2b$`, `$2y$`) entries are accepted. Empty disables authentication. Applies to every admin endpoint. |
| `gauge_refresh` | duration | `"30s"` | Recompute expensive metrics at this cadence; scrapes can be this stale. Positive and at most `"1h"`. |
| `read_timeout` | duration | `"5s"` | HTTP request-line/header timeout, not a response or request-body timeout. Positive and at most `"1m"`. |
| `idle_timeout` | duration | `"30s"` | Keep-alive idle timeout. Positive and at most `"10m"`. |
| `metric_series_cap` | integer | `1024` | Per-metric limit on distinct label combinations. Excess new series are dropped and a warning is emitted. At least `1`. |

Create bcrypt entries with `htpasswd -B`. The password file reloads on an admin
request when its modification time (in whole seconds) or size changes. A same-size
rewrite within the same second may be missed. Reload failures retain the previous
credentials and log a warning. Changing the configured file path needs a daemon
restart. These per-key constraints are skipped when `admin.enabled = false`.

## `[serve]`: stale responses

| Key | Type | Default | Behavior and constraints |
| --- | --- | --- | --- |
| `serve_stale_when_upstream_down` | boolean | `true` | Allow a cached-metadata fallback after an upstream-availability failure. Does not force ordinary cache hits or current snapshots to revalidate synchronously when false. |
| `log_stale_serves` | boolean | `true` | Emit the additional `stale_serve` event for that fallback; ordinary request logging continues when false. |

## `[log]`: daemon logging

| Key | Type | Default | Behavior and constraints |
| --- | --- | --- | --- |
| `level` | string | `"info"` | `"debug"`, `"info"`, `"warn"`, or `"error"`. Empty selects the default. |
| `format` | string | `"text"` | `"text"` for readable key/value lines, or `"json"` for structured single-line JSON. Empty selects the default. |

See [log fields](log-fields.md) for events and troubleshooting details.

## `[tls_mitm]`: caching HTTPS repositories

MITM interception accepts HTTPS proxy `CONNECT` requests, signs a certificate for
the requested host, and caches the decrypted repository traffic. It is enabled
by default. Every client using this route must trust the cache's CA **certificate**;
keep the CA private key on the cache server. See
[client setup](../README.md#configure-the-mitm-https-proxy-optional).

| Key | Type | Default | Behavior and constraints |
| --- | --- | --- | --- |
| `enabled` | boolean | `true` | Enable HTTPS interception. `false` makes `CONNECT` return `405`; it does not provide a pass-through tunnel or cause apt to fall back automatically. |
| `ca_cert` | string | `""` | Supplied PEM CA certificate; set together with `ca_key`, or leave both empty for automatic generation. |
| `ca_key` | string | `""` | Readable private key matching the supplied CA certificate. Certificate/key validity is checked at startup. |
| `ca_storage_dir` | string | `""` | Storage for generated `ca.crt`, `ca.key`, and `ca.ready`; empty uses `<cache.dir>/ca`. Directory must be writable, or its immediate parent must exist and be writable. Unused for a supplied CA. |
| `cert_cache_size` | integer | `256` | Maximum in-memory cached leaf certificates. At least `1`. |
| `leaf_cert_lifetime` | duration | `"720h"` | New leaf-certificate validity: at least `"5m"`, at most `"43800h"` (5 × 365 days). |
| `ca_cert_lifetime` | duration | `"87600h"` | Newly generated CA validity: at least `"24h"`, at most `"438000h"` (50 × 365 days). Does not alter an existing or supplied CA. |
| `leaf_algorithm` | string | `"ecdsa-p256"` | `"ecdsa-p256"` or `"rsa2048"` (for legacy clients). Empty is invalid when enabled. |
| `allowed_host_regex` | string | `""` | Signing gate on the literal requested hostname, before remapping. Empty imposes no MITM-side restriction. Also used to derive name constraints when generating a CA. This is one regex, unlike the upstream array. |
| `allow_unconstrained_ca` | boolean | `true` | Permit generation of a CA without name constraints when the regex is empty or cannot be translated. `false` refuses such generation. Does not validate or constrain supplied or previously generated CAs. |

When enabled, CA paths must be both supplied or both empty. The per-key MITM
constraints are skipped when `enabled = false`. The upstream hostname allowlist
and resolved-IP deny list remain separate controls; allowing a host here does
not bypass them.

### Constraining a generated CA

For a new CA, this configuration permits signing only for the listed hosts and
requires name constraints:

```toml
[tls_mitm]
enabled = true
allowed_host_regex = '^(deb\.debian\.org|security\.debian\.org)$'
allow_unconstrained_ca = false
```

Supported translation shapes are literal hosts, literal-host alternation,
single-label prefixes such as `'^[a-z0-9-]+\.debian\.org$'` or
`'^[^.]+\.debian\.org$'`, and optional two-letter regions such as
`'^([a-z]{2}\.)?archive\.ubuntu\.com$'`. Anchors are optional but must be balanced;
using both is recommended. Arbitrary regexes, `*` repetition, and alternation of
nonliteral branches are not supported for translation. A valid but unsupported
regex still acts as a signing gate when unconstrained generation is allowed.
X.509 name constraints are domain-subtree constraints and can be broader than
the runtime regex.

### CA reuse and policy changes

An existing generated CA is reused on restart. Changing the regex changes the
runtime signing gate, but does **not** rewrite that CA's name constraints;
changing its lifetime or setting `allow_unconstrained_ca = false` also does not
replace or recheck the existing CA against the new generation policy. To adopt
a new CA policy, provision a new CA (for example in a new `ca_storage_dir`) and
distribute its certificate to clients as a planned trust change.

## `[[trusted_signer]]`: restrict repository signing keys

No signer pins are configured by default. Repeat this table for additional rules.

| Key | Type | Requirement / behavior |
| --- | --- | --- |
| `match_canonical_host` | string | Required nonempty regex matched against the remapped canonical host. |
| `fingerprints` | array of strings | Required nonempty list of full 40-hex-character OpenPGP fingerprints, case-insensitive. Keys must also be present in a loaded keyring. |

```toml
[[trusted_signer]]
match_canonical_host = '^repo\.example\.net$'
# Replace this illustrative fingerprint with the repository's verified signer.
fingerprints = ['0123456789ABCDEF0123456789ABCDEF01234567']
```

All matching rules contribute the **union** of their fingerprints; rules are not
first-match-wins. With no match, the ordinary loaded-key trust set applies unless
`require_pinned_signer` rejects the suite or `accept_any_signer` bypasses
verification. Pins apply to hosts, not individual suite paths, and cannot
substitute for supplying the public keys themselves.

## `[[remap]]`: canonicalize repository hosts

No custom rules are configured by default. Custom rules run in file order before
built-in rules, and the first match wins.

| Key | Type | Requirement / behavior |
| --- | --- | --- |
| `match_host_regex` | string | Required nonempty regex. Matches the lowercased source hostname without port or trailing dot. |
| `canonical_host` | string | Required replacement hostname, without scheme, port, or path. Becomes both the cache identity and actual upstream host. |

```toml
[[remap]]
match_host_regex = '^my-mirror\.example\.net$'
canonical_host = 'archive.ubuntu.com'
```

Remapping preserves scheme and path, and discards the original explicit port on
a match. It cannot convert HTTPS to HTTP or rewrite a path. Only combine mirrors
that serve equivalent content. Built-ins canonicalize two-letter Ubuntu regional
archive/security/ports aliases and Debian country aliases such as
`ftp.us.debian.org`; Debian's main and security hostnames remain themselves.
Unmatched hosts keep their hostname and upstream port. Upstream access policy
and signer pins use the resulting canonical hostname.

## `[[mirror]]`: expose a repository under a local path

No mirror routes are configured by default. These rules support direct requests
such as `http://cache.example.net:3142/corretto/...`, alongside proxy mode.

| Key | Type | Requirement / behavior |
| --- | --- | --- |
| `prefix` | string | Required local URL prefix beginning with `/`. Matching uses path boundaries; trailing slashes are normalized. Duplicate or overlapping prefixes are rejected. |
| `upstream` | string | Required `http://` or `https://` URL with a host and optional base path. Userinfo, query strings, and fragments are rejected at daemon startup. |

```toml
[[mirror]]
prefix = '/corretto'
upstream = 'https://apt.corretto.aws/'
```

The matched prefix is replaced with the upstream base path. `/corretto` matches
`/corretto/dists/...` but not `/corretto-extra/...`. A prefix of `/` catches all
mirror-mode paths and cannot coexist with another prefix. The selected upstream
still goes through remapping and upstream access policy.
