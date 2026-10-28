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
| `proxy` | string | `""` | HTTP or HTTPS forward proxy URL for all upstream fetches. Empty means direct access. Optional Basic-auth credentials; see [upstream proxy setup](#using-an-upstream-proxy). Requires empty `deny_target_ranges`. |
| `connect_timeout` | duration | `"30s"` | Connection establishment timeout. Nonnegative; `"0s"` selects the default. |
| `total_timeout` | duration | `"5m"` | Overall budget for a fetch, including its retries. Nonnegative; `"0s"` selects the default. |
| `idle_read_timeout` | duration | `"60s"` | Accepted and logged, but currently **does not enforce an idle-read timeout**. `total_timeout` bounds the fetch. Nonnegative; `"0s"` selects the default. |
| `max_retries` | integer | `3` | Extra attempts after the initial fetch attempt: up to four attempts with the default. Nonnegative; `0` currently selects `3`, so it does not disable retries. |
| `max_concurrent_per_host` | integer | `8` | Shared per-canonical-host concurrency limit for request fetches and background work. Nonnegative; `0` selects `8`, not unlimited. |
| `unreachable_cooldown` | duration | `"30s"` | After a failed dial, subsequent dials within this window use a short probe and stop retrying if that probe fails. `"0s"` disables this fast-failure behavior. Nonnegative. |
| `unreachable_probe_timeout` | duration | `"1s"` | Additional dial deadline while a host is in cooldown. `"0s"` removes this shorter deadline but still suppresses retries after a failed probe. Nonnegative. |
| `allowed_host_regex` | array of strings | `['^.*$']` | Allow a canonical hostname if any regex matches. Checked again for redirect destinations. Explicit `[]` denies all upstream hosts. Hostnames have no port. |
| `deny_target_ranges` | array of strings | `[]` | In direct mode, IPv4/IPv6 CIDR ranges blocked when dialing resolved addresses, including redirect targets. Empty means no IP-range filter. Nonempty lists cannot be combined with `proxy`. |
| `allow_https_to_http_redirect` | boolean | `true` | Follow HTTPS-to-HTTP redirects. `false` rejects downgrades. Host policy still applies; in direct mode, IP policy does too. |

The first failed dial can put the next retry of the same fetch into cooldown;
it is not necessary to wait for another client request. Retry counts are upper
bounds: nonretryable failures, cooldown probes, and `total_timeout` can stop a
fetch earlier. Adoption's member retries are a separate outer retry policy.

Upstream request paths are encoded exactly as apt encodes them (lowercase
percent-encoding of `+`, `~`, space, `%`, control and non-ASCII bytes, plus
`#` and `?`). Paths containing `+` or `~` therefore reach upstream as
`%2b`/`%7e`, as they do when apt fetches directly. Besides apt's set, the
cache also escapes `"` `<` `>` `\` `^` `` ` `` `{` `|` `}`, because Go's URL
handling requires those bytes escaped in a path. Request paths containing a
decoded control byte are rejected with 400.

### Using an upstream proxy

To chain this cache through another forward proxy (including apt-cacher-ng),
set `proxy` in the existing `[upstream]` table, then restart the daemon:

```toml
[upstream]
proxy = "http://proxy.example.net:3128"
deny_target_ranges = []
```

The route is `apt client → apt-cacher-ultra → upstream proxy → repository`.
Cache hits remain local. All outbound repository requests use the proxy,
including cache misses, freshness checks, adoption, prefetches, and redirects.
The proxy receives absolute URLs for HTTP repositories and `CONNECT` tunnels
for HTTPS repositories; it must permit CONNECT to the repository ports.
Repository hostnames are resolved by the proxy, so they need not resolve on
the cache server. Client-side HTTPS interception and CA setup are unchanged.

Use an absolute `http://` or `https://` proxy URL with a hostname or bracketed
IPv6 address. The port is optional (defaults to 80 or 443 respectively); an
explicit port must be 1–65535. A trailing `/` is allowed, but other paths,
queries, and fragments are rejected. SOCKS, PAC, per-host proxy selection, and
automatic direct fallback are not supported. `HTTP_PROXY`, `HTTPS_PROXY`,
`ALL_PROXY`, `NO_PROXY`, and their lowercase equivalents are ignored, whether
`proxy` is set or empty. To restore direct access, set `proxy = ""` and restart.

For Basic authentication, include credentials in the URL:

```toml
proxy = "https://cache-user:secret%40value@proxy.example.net:3129"
```

Percent-encode reserved characters in credentials (`%40` represents `@`).
Usernames must be nonempty and cannot contain `:`; credentials cannot contain
control characters. Protect a config file containing credentials so only root
and the daemon's service account can read it. Credentials go to the proxy,
including on CONNECT requests, and are not used as repository authentication.
An `http://` proxy exposes Basic credentials on the cache-to-proxy network hop,
even for HTTPS repositories; use `https://` when that hop needs encryption.

Both HTTPS proxy certificates and HTTPS repository certificates are verified
against the cache server's system trust store. For a private proxy CA, install
its public certificate into that trust store and restart apt-cacher-ultra.
The CA generated by this cache for its clients is separate from that trust
store. Connections through an HTTPS proxy use HTTP/1.1 for compatibility with
Go's forwarding/CONNECT transport, even if the proxy advertises HTTP/2.

`allowed_host_regex` and the HTTPS-to-HTTP redirect policy still apply to
repository hosts. The proxy hostname itself need not match the allowlist.
**A nonempty `deny_target_ranges` with `proxy` is a startup error:** the cache
cannot check the repository IP chosen by the proxy's resolver. Configure any
destination IP restrictions on the upstream proxy before switching to proxy
mode; clearing the local list alone does not preserve those restrictions.

If the proxy fails, fetches fail under the usual retry and stale-cache rules;
the cache never bypasses it by fetching directly. TCP dial failures put the
proxy endpoint into cooldown, shared across repositories; CONNECT or TLS
failures do not trigger that dial cooldown. `connect_timeout` limits TCP and
TLS handshakes, while `total_timeout` bounds the entire fetch, including the
wait for a CONNECT response and any retries. Per-host concurrency limits still
apply to repository hosts, not to the proxy as a whole.
An upstream proxy's `407 Proxy Authentication Required` fails immediately
without retrying the same credentials. Cache clients receive stale data where
eligible or `502 Bad Gateway`; fix the proxy credentials in the cache config.

### Repository access policy

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
| `max_concurrent_adoptions` | integer | `2` | Global concurrent snapshot-adoption limit. `0` means unlimited; must be nonnegative. Shared by adoptions, repairs and reconciles; see *Capacity and fairness*. |
| `max_suites` | integer | `5000` | Most suites tracked (checked, adopted, shown on the status page) at once, and at most a tenth of it per upstream host (`http` and `https` counted together). At least `10`; an explicit `0` is an error. See *Tracked suites*. |

Freshness checks detect changed repository metadata. `[adoption]` determines
whether a changed suite is staged and published as a new coherent snapshot.

### Tracked suites

A suite is tracked once a client fetches its `InRelease` — or, with adoption
enabled, its `Release` — and the bytes are what they claim (an `InRelease`
starts with the clearsign line apt requires; a `Release` has a hash block):
an HTML error page or a stub never tracks a suite, and `Release.gpg` never
does on its own. A suite whose `InRelease` (with adoption enabled, also
`Release`) an earlier release cached is tracked on its first cache hit. A
suite path longer than 1024 bytes is never tracked; its files are served
untracked.

With adoption disabled only `InRelease` tracks a suite, and a refetched
`InRelease` (after its cached copy was garbage-collected) still counts as a
successful check, as before. Two things differ from earlier releases: a
plain-text "InRelease" (no clearsign line; apt rejects it too) no longer
tracks a suite, and neither does a repository that publishes only `Release`
and `Release.gpg` (for example pkg.jenkins.io). Earlier releases tracked
such a repository from its first `Release` hit; one first seen after the
upgrade is now served as before but not tracked — not checked for changes
or shown on the status page — while adoption is disabled, and one an
earlier release already tracks stays tracked.

At most `max_suites` suites are tracked and at most `max_suites / 10` per
host. At the total cap, a new suite makes room by evicting the least
recently requested suite that has no adopted snapshot and has not been
requested for 24 hours (at most eight are tried, a suite being checked or
adopted is skipped, INFO `suite_evicted` names it). A suite refused a row is
still served — it just does not refresh — and is not retried for 10
minutes (one minute in the busy case below): each refusal that reaches the
database is counted in `acu_suite_seed_rejected_total{host}` (at most once
per suite per retry window, unless more than 4,096 suites were refused
within it), the gauge `acu_suites_seed_rejected` shows how many suites are
waiting out their retry window (updated within a minute), and WARN `suite_seed_rejected` (at most
one a minute, with how many were suppressed) names the suite. When the
cache is full and every suite that could make room is busy at that moment
(being checked or adopted, or evicted for another new suite), the refusal
is retried after one minute instead, and the warning's `detail` says so.
Raise `max_suites` for very large Artifactory, aptly or Nexus hosts.

A cache can hold more suites than the caps allow: after `max_suites` is
lowered, or after an upgrade from a release without the caps (one that
collected more than 5,000 suites, or more than 500 on one host, for example
from an upstream that answers missing `InRelease` files with an HTML page).
The daemon logs WARN `suite_caps_exceeded` at start when the table holds
more than `max_suites` suites, or a host holds `max_suites / 10` or more (a
host can also get there by normal growth). Above the total cap new suites
are refused, but each refused attempt still evicts one suite that qualifies
(no adopted snapshot, not requested for 24 hours), so the table shrinks
back under the cap as suites fall idle. A host at its own cap refuses that
host's new suites and evicts nothing for them; its suites go when the suite
reaper removes those not requested for `gc.suite_idle_ttl` (30 days by
default; see *Suite reaping* under `[gc]`), or when
a new suite of any host at the total cap evicts one of its idle suites (no
adopted snapshot, not requested for 24 hours) — with
`gc.suite_idle_ttl = "0s"` only the latter. A table
at exactly the total cap logs no warning: that is a full cache working
normally, where a new suite evicts an idle one and is refused only when
none qualifies (WARN `suite_seed_rejected`).

Serving a tracked suite's `InRelease`, `Release` or `Release.gpg` — or,
for a flat repository, its root `Packages` or `Sources` — or a `.deb` its
current snapshot vouches for, records that the suite is in use
(at most one write per suite per hour); a suite not in use for
`gc.suite_idle_ttl` (30 days by default) is no longer tracked (see *Suite
reaping* under `[gc]`). A by-hash request with no cached
copy under its own path is answered from an identical cached file only for
a tracked suite; otherwise it is fetched from upstream.

### Eager first adoption

With adoption enabled, a tracked suite that has no snapshot yet is
adopted on its first check, in whichever form its upstream publishes (see
*Anchor forms and unsigned repositories*), when a client asked for it in
the last 7 days and a client fetched its `InRelease` or `Release` — or,
for a flat repository, its root `Packages` or `Sources` — through
the cache (a copy the cache rebuilt for itself does not count). The
request that creates a suite starts its first check once its response is
sent, so a new repository is adopted within seconds of its first
`apt update` when an adoption slot is free. A suite nobody asked for in 7
days is dormant: it costs no upstream request until a client asks again,
and is then checked — and adopted, when a slot is free — within one
scheduler tick (`periodic_refresh / 4`) of that request (the request's own
check may run before its interest is recorded). A request stamp up to 7 days in the future, left by a clock
that was stepped back, still counts as interest; one further ahead leaves
the suite dormant until a client asks for it again. After an upgrade, a
suite counts as asked for when a client fetched one of its anchor files in
the 7 days before the upgrade.

A suite whose first adoption failed or was refused (see *How failed
adoptions are retried*) is probed again at its retry time (2 minutes,
doubling to an hour), or once `periodic_refresh` has passed since its last
check if that comes first; checks that clients trigger in between ask
upstream nothing. A suite whose upstream serves neither `InRelease` nor
`Release` fails its check (backing off from 2 minutes to an hour) and
shows "refused: anchor_absent" on the status page — unless it is a flat
repository the cache has never seen signed, which is refreshed without a
`Release` (see *Flat repositories*), or, while `adoption.adopt_unsigned`
is off, shows "refused: unsigned_disabled" instead.

A first check that ends without an answer the cache can act on — a 5xx or
a timeout while it probes, `Release.gpg` failing after a 200 `Release`,
say — is a failed check: the scheduler backs off from it, but it records
no adoption failure, so a check a client triggers past the `cooldown`
probes again, a full `Release` download included. Client checks are bound
by the `cooldown` alone; the lasting case, an unsigned `Release` while
`adoption.adopt_unsigned` is off, is a refusal, which its record paces.

First adoptions never take the last free adoption slot, which is kept for
suites that already serve a signed snapshot (see *Capacity and
fairness*): with the default `max_concurrent_adoptions = 2` a first
adoption runs only while no other adoption runs, so on a cache kept busy
by signed updates it can wait a long time. Busy caches should set `3`,
which also lets two first adoptions run at once, for example for the
burst after an upgrade. A suite waiting for a slot is probed twice by the
scheduler — by the check that queued it and by the re-check that admits
it — plus once for each client check while it waits (at most one per
`cooldown`) and once for each slot offered to it that another admission
takes first.

With adoption disabled nothing changes: suites are checked for changes
only, and a miss that creates a suite starts no check (a cache hit on a
suite's anchor still triggers its usual check, under the `cooldown`, also
the hit that creates the suite's row).

### Flat repositories

A flat repository — one without a `dists/` tree, configured as
`deb <uri> <dir>/`, `deb <uri>/ /` or `deb <uri> ./` (pkgs.k8s.io, NVIDIA
CUDA, pkg.jenkins.io, Gemfury) — is tracked as the suite of the directory
that holds its `InRelease` or `Release`. The `./` form keeps its literal
`/.` and is a suite of its own, the directory's *twin*:
`deb https://h/repo ./` and `deb https://h/repo/ /` are two suites, each
with its own row and snapshot, that share the record of having been seen
signed. With adoption enabled a flat repository's root `Packages` or
`Sources` (plain or compressed) tracks its suite too, when its bytes are
what they claim. A flat repository at the host root (`deb https://h/ ./`
or `deb https://h/ /`) is not supported: it is served from the ordinary
cache and never refreshed, and the daemon logs WARN
`flat_host_root_unsupported` (at most one line a minute).

A signed or unsigned flat repository is adopted like any other (see
*Anchor forms and unsigned repositories*). Releases before 1.1.0 never
adopted a flat repository, so its signing key may be installed only on
the clients (for `signed-by=`); a signed one now needs it on the cache
host too (see *Trust and signer pins*): copy it to `/etc/apt/keyrings` or
a directory of `keyring_dirs` and restart the daemon, which reads keys
only at startup. Until then each adoption of the repository fails
(outcome `gpg_failed`, or `unpinned_suite` under `require_pinned_signer`
while no `[[trusted_signer]]` matches the host) and logs WARN
`adoption_run_failed`, on the schedule of *How failed adoptions are
retried*. When its `Release` lists its
root `Packages`, that file's fetch failing fails the adoption; a signed
`Release` that lists none adopts with incomplete coverage
(`no_packages_members`), which turns strict mode off for its host (an
unsigned snapshot never counts for strict mode). The `.deb` files that
`Packages` names are vouched by the paths apt requests for them:
`<uri>/<Filename>`, with or without a leading `./`, for whichever
directory at or above the `Release` the repository's `<uri>` is
(pkg.jenkins.io's `binary/…` one level up, Sublime Text's `files/…` at the
host root). A path of more than 32 segments is vouched only as the
`Packages` file spells it. Because a `Filename` may name a path under any
directory above its `Release`, a signed flat repository's `Packages` also
constrains such a path of another repository on the same host, while no
signed snapshot of that repository vouches the path: on a host shared by
many tenants (Open Build Service projects, say) one tenant's `Filename`
can make another tenant's `.deb` fail its hash check (502), strict mode on
or off. A declaration only ever refuses bytes that do not match it; it
never lets other bytes through. The two twins declare the same paths: when
upstream rewrites a `.deb` in place with new bytes, the twin that adopted
the new `Packages` and the twin still serving the old one disagree on its
hash, and the file is refused (502, `package_hash_conflict`, whose
`declared` entries name each row's `suite_path`) under both twins until
the second one adopts — at its next check, at most `periodic_refresh`
later unless that adoption fails.

With adoption enabled, a flat repository that publishes no `Release` at
all (Gemfury, goreleaser) is refreshed without one, as long as the cache
has never seen it signed and `adoption.adopt_unsigned` is on (with it
off, such a repository is served as the cache first stored its files and
shows "refused: unsigned_disabled"; turning the knob on resumes its
refresh): each check sends one conditional request for every cached
`Packages`, `Sources` and `Contents-*` file of the suite, in that order,
with the validators upstream gave it, and reads at most two such files at
once across the whole process. A changed file replaces the cached one
when its bytes pass the flat index check for its name (for a compressed
`Contents-*`, its compression magic); an HTML page or anything else
that fails it keeps the old file (counted in
`acu_anchorless_sniff_rejected_total{host}`), and a plain `Contents-*`,
which has no such check, is deleted when it changed, for the next client
request to fetch. A file upstream deletes (404, 410) is deleted, and so is
a cached `InRelease` or `Release` that upstream now answers so — or
answers 401 or 403 while a file the check asked for got a real answer (the
Amazon S3 and CloudFront answer for a missing file) — unless a client's
own fetch stored other bytes for it meanwhile. A file whose new body is
over 64 MiB is deleted too, so that the next client request fetches it in
full (a WARN names it once the delete succeeds, at most one line a minute;
a failed delete keeps it for the next check); anything else — 401,
403, a 5xx, a timeout — keeps the file. A check that asks for files counts
as answered only when at least one of them got a real answer (304 to a
request with validators, a 200 stored or identical to the cached file, 404
or 410): one whose files got none — an HTML page for every file (a captive
portal, a vendor's retired site; even a page the cache already holds), 401
and 403, a changed plain `Contents-*`, a body over 64 MiB — fails and
backs off: its *Last success* stops advancing on the status page,
`suites[].check_fail_count` counts the failures in the status JSON, and
`acu_freshness_check_total{result="failed"}` counts them. A delete that
fails is no answer: its 404s and 410s stop counting, so a check they alone
answered — no file asked for, or every real answer a delete — fails. Such
a repository has no snapshot: it vouches nothing and never changes its
host's strict posture; on a strict host whose signed suites are complete
its `.deb`s are refused like any unvouched package. Its index files are
served with `Last-Modified` the later of the file's time and its last
refresh, so apt's `If-Modified-Since` sees a replacement. The check logs
`freshness_check` with `result=anchorless_refreshed` or
`anchorless_unchanged` and the number of files it replaced and deleted
(`rows_replaced`, `rows_deleted`), and the repository is adopted at the
first check after it starts publishing an `InRelease` or a signed
`Release` (an unsigned one after its confirmation: safeguard 2 of
*Anchor forms and unsigned repositories*). A flat repository the cache
has seen signed (a record of a signature for its
path, a cached `InRelease` or `Release.gpg` that passes its check, or an
adopted signed snapshot) is never refreshed this way: when its `InRelease`
and `Release` disappear it fails its checks and shows "refused:
anchor_absent", so stripping a signed repository's signatures cannot turn
it into an unverified one. Nor is an unsigned flat repository the cache
has adopted: when it stops publishing its `Release` it keeps serving its
last snapshot and shows "refused: anchor_absent"; `POST /forget` turns it
into a repository refreshed without a `Release`.

Three limits of the refresh without a `Release`. Behind an upstream that
answers 403 for a missing file (Amazon S3, CloudFront), an index file the
repository removed stays cached and served (a cached `InRelease` or
`Release` does not, once another file answers: see above): if that is the
`Packages.xz` apt tries first, a client never sees the fresh
`Packages.gz`. A repository whose only index is a plain `Packages` that is
empty at first contact gets no suite (an empty file is not recognisably an
index): it is served as cached and never refreshed; likewise an index that
later becomes empty is never stored (the cached one stays). And an
unchanged plain `Contents-*` counts as a real answer without a check: a
captive portal's page that a client's miss cached as a plain
`Contents-<arch>`, and that the portal still serves, answers the check
(`anchorless_unchanged`) during the outage. Flat repositories rarely
publish a plain `Contents-*`.

After an outage that answered every file with an HTML page (a captive
portal, a vendor's maintenance page), a Release-less repository's
clients can see one more error. A client's update during the outage
caches the page as the repository's `InRelease`, `Release` and
`Release.gpg` (a miss stores what upstream answers; apt reports the page
as an invalid signed file, as it would fetching directly). The first
`apt update` after the outage can still get those pages; the check it
triggers finds `InRelease` and `Release` answered 404 again (or, behind
Amazon S3 or CloudFront, 403) and deletes them, so the next update is
clean. The page cached as `Release.gpg` stays: apt never asks for it
while the repository publishes no `Release`, but if the repository later
publishes one without an `InRelease`, apt fetches `Release.gpg` beside it
and prints a
signature error for the page (`Signed file isn't valid, got 'NODATA'`)
until the cache adopts the repository — a signed one at its first check,
an unsigned one after its confirmation (see *Anchor forms and unsigned
repositories*, safeguard 2). An adopted snapshot never serves that page:
it answers `Release.gpg` with its own signature or, for an inline or
unsigned snapshot, 404. `POST /forget` deletes it at once (*Forgetting a
suite* under `[admin]` says when).

Three limits of every flat repository, signed or not. Its Translation
files (apt names them after the language alone: `en`, `en.xz`), its
`Contents-*.diff/Index`, `<index>.diff/by-hash/…` and pdiff patch files,
and its DEP-11 and command-not-found files are served from the ordinary
cache and never refreshed, as at the host root. Its source packages
(`.dsc`, tarballs) and pdiff patches are never vouched: the cache builds
no hashes from a flat repository's `Sources` or pdiff indexes. And the
`.deb`s of a repository whose `Filename`s point above its `Release`
(pkg.jenkins.io, Sublime Text) get none of its snapshot's retention
(`retention.max_versions_per_package`, hot prefetch): they are kept by
recency (`gc.url_path_ttl`) and the hold grace, like files no snapshot
declares.

## `[adoption]`: snapshots, trust, and recovery

| Key | Type | Default | Behavior and constraints |
| --- | --- | --- | --- |
| `enabled` | boolean | `false` | Enable adoption of new snapshots. When false, freshness differences are recorded without adopting them, so tracked suites lag upstream by design: the status page's SUITES vital then reads "ADOPTION DISABLED — METADATA NOT REFRESHED" (stale, not a fault), rows keep their lag text without warning, and failure records left from a time adoption was enabled are inert (no retries, `acu_adoption_failing_suites` reports 0). The suite reaper still runs (see *Suite reaping* under `[gc]`): a suite no client asked for in `gc.suite_idle_ttl` has its cached index files deleted, so clients that come back to it fetch upstream's current files through the cache, which then stay as fetched. |
| `require_signature` | boolean | `true` | Allows starting with an empty keyring. It no longer relaxes verification: only a decodable clearsigned `InRelease` is adopted as inline metadata in every mode (plain-text bodies are `missing_signature`, undecodable ones `malformed_clearsign`). Unsigned repositories are adopted in the unsigned form instead (`adopt_unsigned`). |
| `adopt_unsigned` | boolean | `true` | Adopt a repository that publishes a `Release` with no signature (neither `InRelease` nor `Release.gpg`) as an unsigned snapshot: served coherently, but never trusted — it vouches no `.deb` and never counts for `integrity.refuse_unvouched_debs`. A suite the cache has seen signed is never adopted unsigned. `false` refuses every unsigned `Release` ("refused: unsigned_disabled", a state, not a failed check), so the cache no longer keeps unsigned repositories fresh: an unsigned suite already adopted keeps serving its last snapshot, coherent but frozen — never updated — until the knob is turned back on (it then updates at its next retry, at most 75 minutes later: a restart keeps a refusal's retry time), the suite is forgotten (`POST /forget`), or it is reaped after `gc.suite_idle_ttl` without a client. Forgotten or reaped with the knob still off, it is then a repository not yet adopted, which is worse: the forget or reap deletes its cached index files first, so its next requests fetch upstream's current files, but it stays unadopted and each file then stays as the cache first stored it, never refreshed, so an upstream publish can leave its clients with Hash Sum mismatches. Off also stops the refresh of a flat repository that publishes no `Release` at all (see *Flat repositories* under `[freshness]`): it is served as the cache first stored its files, and the status page shows "refused: unsigned_disabled"; turning the knob on resumes its refresh. Set it to `false` only if you do not serve unsigned repositories through the cache. Either way the suite is asked upstream again only at its retry times (2 minutes, doubling to an hour; for a suite not yet adopted, also once `periodic_refresh` has passed), never at the checks clients trigger in between. See *Anchor forms and unsigned repositories* for the safeguards and the threat notes. |
| `require_pinned_signer` | boolean | `false` | Reject adoption when no `[[trusted_signer]]` matches the canonical host. Evaluated before `accept_any_signer`. |
| `accept_any_signer` | boolean | `false` | For hosts without matching signer pins, skip both signer trust checks **and cryptographic signature verification**. A decodable clearsigned `InRelease` (or, for the detached pair, a parseable `Release.gpg` signature) is still required, as in every mode. Matching pins keep normal verification. Logs a startup warning. |
| `allow_short_keyid` | boolean | `true` | Accept signatures containing only a legacy 64-bit issuer key ID when it uniquely identifies a loaded trusted key and cryptographic verification succeeds. `false` requires the long issuer fingerprint. |
| `keyring_dirs` | array of strings | `[]` | Extra startup keyring directories containing `.gpg` or `.asc` files. Entries must be nonempty absolute paths; nonexistent directories are skipped. Adds to built-in directories and bundled keys. |
| `architectures` | array of strings | `[]` | Architectures to adopt; empty keeps all. Filters recognized per-architecture Packages, Sources, pdiff indexes, Contents, cnf Commands, and dep11 Components. Use `"source"` for source indexes. Architecture-independent `"all"` content is always retained. |
| `required_architectures` | array of strings | `[]` | Additional guard: each declared Packages/Sources index group for these architectures must have at least one fetched compression variant before adoption commits. Empty disables this additional guard; see the built-in guard below. |
| `tolerate_optional_member_failures` | boolean | `true` | Skip optional metadata whose fetch or integrity checks fail after retries, allowing the remaining snapshot to commit. A fetch that `max_duration` or a shutdown cuts short is not such a failure: the adoption ends there (`timed_out`, or abandoned at shutdown) and commits nothing. Skipped content is unavailable until repaired. Packages/Sources indexes and their pdiff indexes do not receive this tolerance. |
| `member_retry_count` | integer | `2` | Extra member-fetch attempts after availability or integrity failures. `0` means one attempt. Must be nonnegative. `404`/`410` skips are not retried in this loop. |
| `member_retry_delay` | duration | `"30s"` | Delay between member retries. Nonnegative; `"0s"` means no delay. |
| `repair_skipped_members` | boolean | `true` | After an unchanged successful freshness check, repair integrity/availability skips and reconcile missing declared requestable indexes in the live snapshot. The latter also covers required indexes previously skipped with `404`/`410`. |
| `hot_prefetch_budget` | duration | `"5m"` | Wall-clock budget for fetching hot package versions before publishing a new snapshot. `"0s"` removes this overall guard; each fetch still has upstream limits. Nonnegative. |
| `max_duration` | duration | `"30m"` | Longest an adoption, repair or reconcile may run. An abandoned adoption is retried on the normal schedule; a repair or reconcile runs again on the next unchanged check. The deadline never cuts the final commit short: the commit runs under its own 5-minute bound. Nor does it cut the flip barrier short (at most 140 s, see *Flip consistency*), which only a shutdown ends early. On shutdown an adoption that has not reached its commit is abandoned at once, but a commit already running is waited for, up to those 5 minutes; the packaged systemd unit's `TimeoutStopSec=35` may kill the process first, in which case SQLite rolls the commit back and nothing is lost. At least `"1m"`. |
| `flip_quiet_period` | duration | `"10s"` | How long a suite's `InRelease`/`Release` serves must have been quiet before a new snapshot replaces the current one, so a client that just received the old one fetches matching indexes (see *Flip consistency*). `"0s"` disables the barrier: adoptions commit at once, as before. Otherwise between `"1s"` and `"15s"`: a shorter period protects almost no one, and a longer one leaves less of a hold's first 15 s for the responses already being sent when it starts, so more holds commit before the suite is quiet (`acu_flip_quiet_cut_short_total`). |

### How failed adoptions are retried

When an adoption fails (an untrusted or malformed signature, a member that will not download intact, a `Date` in the future, a timeout), the cache records the failing content and backs off: the same content is retried after 2, 4, 8, 16, 32 minutes and then hourly (each ±25 %). Until then each check asks upstream with the failing content's validators, so an unchanged upstream costs one small request and no download. **Any new content is tried at once.** A deterministic failure therefore costs about 30 attempts a day instead of one per `cooldown`.

If the cache cannot read the current snapshot when an adoption starts, the attempt fails as `db_failed` (and is retried on the backoff schedule) rather than skipping the rollback checks below.

Seven outcomes are refusals, not failures, and are shown as "refused: …" on the status page:

- `upstream_older`: upstream serves a Release whose `Date` is older than the snapshot being served, or has no `Date` at all after a dated snapshot (a missing `Date` counts as 0). apt would ignore it too; the cache keeps serving the newer snapshot.
- `rollback_refused`: upstream serves content identical to an older snapshot the cache already replaced (a flapping mirror). Only the last `gc.keep_displaced` snapshots are remembered for this.
- `unsigned_unconfirmed`: an unsigned repository's first adoption waits for a second observation of the same `Release` (see *Anchor forms and unsigned repositories*).
- `unsigned_downgrade_refused`: the suite has been seen signed, so its unsigned `Release` is not adopted.
- `unsigned_too_large`: an unsigned `Release` declares more than 1,000 files.
- `unsigned_disabled`: `adopt_unsigned = false`, so an unsigned `Release` is not adopted; an unsigned suite already adopted keeps serving its last snapshot, not updated, until the knob is turned back on, the suite is forgotten (`POST /forget`), or it is reaped after `gc.suite_idle_ttl` without a client. Forgotten or reaped with the knob still off, it is then a repository not yet adopted, served as the cache first stored its files (see `adopt_unsigned`). A flat repository that publishes no `Release` at all is refused this way too while the knob is off: it is not refreshed (see *Flat repositories*).
- `anchor_absent`: the suite's current anchor file is gone upstream, or a detached suite's changed `Release` comes without its `Release.gpg`; its snapshot keeps serving, and the suite moves to its other signed form when upstream serves one. A suite without a snapshot whose upstream serves neither `InRelease` nor `Release` shows it too (unless it is a flat repository the cache has never seen signed: see *Flat repositories*). While a failed adoption of the suite is recorded (its move to the other form failed), the status page shows that failure's reason instead, and the suite is paced the same way.

Refused content follows the same retry schedule as a failure, but a refusal is a state, not a failure: it logs INFO (`adoption_refused`, and `adoption_run_failed` at INFO) and is not counted by `acu_adoption_failing_suites`. `upstream_older` and `rollback_refused` show no lag (the cache already serves content at least as new as upstream's); the other five keep their lag, because the suite is in fact behind. A refusal that lasts more than 24 hours is escalated anyway, because it may be a silent freeze (a vendor that stops emitting `Date` is refused `upstream_older` forever): the status page warns on its row and on the SUITES vital ("REFUSED 24H+", never critical) and the log carries WARN `adoption_refused_persistently` at the streak milestones.

A Release dated more than 10 seconds ahead of the cache's clock is a failure (apt rejects it as "not valid yet"); check the cache host's clock if this persists.

At start, every failing suite gets one retry spread over the next `periodic_refresh`, so installing a missing key or fixing pins takes effect without waiting an hour. Refusals and panicked adoptions keep their schedule.

### Capacity and fairness

`freshness.max_concurrent_adoptions` slots are shared by adoptions, repairs and reconciles, plus one slot per upstream host. Nothing blocks on a slot: a suite whose scheduled check finds none waits in the scheduler's queue and is re-checked as soon as capacity it can use is released (hosts in turn, oldest first); while it waits, no request is made for it. A change found by a client-triggered check that finds no slot stays pending, and the next scheduler tick checks that suite (bypassing the cooldown) and queues it the same way, so it starts waiting within one tick. Client requests still trigger checks under the normal cooldown and may take a freed slot first. When there are at least two slots, the last free one is reserved for suites that already serve a signed snapshot, so security updates are not delayed by first or unsigned adoptions. With the default pool of two, first adoptions therefore run only when no other adoption is running, and can wait a long time on a cache kept busy by signed updates; `max_concurrent_adoptions = 3` avoids that and lets two run at once (useful for the burst after an upgrade). A repair or reconcile that finds no slot simply runs on a later unchanged check (`acu_repair_deferred_total{host}` counts them); one of a suite whose current snapshot is unsigned never takes the reserved slot. `POST /reconcile` answers 409 when no slot is free. The gauge `acu_freshness_waiting_for_slot` counts suites waiting for a slot. An adoption gives its host's slot back when its downloads are done, but keeps its global slot through the flip barrier and the commit, so a slot is held for at most `max_duration` + 140 s (the barrier, see *Flip consistency*; `acu_adoption_duration_seconds` includes that time) + the 5-minute commit bound: N suites waiting for k slots they may use are all admitted within one tick + ⌈N/k⌉ × (that sum + 2 minutes). Two short database writes made while the slot is held — an unsigned adoption's re-read of the suite's signed anchors before its commit, and a failed attempt's failure record — are outside that bound: they take microseconds unless the database writer is stuck behind another suite's long commit, in which case they can add up to 5 and 5.5 minutes.

Periodic checks run on four workers, at most one check per upstream host at a time, hosts taking turns. On each host, healthy suites go first, then never-checked suites, then suites whose checks keep failing; a failing suite waits 2 minutes, doubling to an hour, between checks. The upstream part of each check (waiting for the host's fetch slot and the requests) is limited to 2 minutes; the check's own cache writes are not cut short, so the deadline is always recorded. A due suite S is therefore checked within one tick + (n + 1) × ⌈H/4⌉ × 2 minutes, where H is the number of hosts with suites waiting and n the number of suites ahead of S in its host's queue (only healthy ones when S is healthy): stalling hosts delay a healthy suite on another host by at most ⌈H/4⌉ checks per suite ahead of it (with four hosts or fewer, one check), and once a stalling host's suites are known to fail they queue behind its healthy suites — also right after a restart, because the order is persisted. The bound assumes responsive cache I/O.

### Flip consistency

apt fetches a suite's `InRelease` (or `Release`) first and its indexes after, one request at a time on one connection. If a new snapshot replaced the current one in between, the client would get indexes that do not match its `InRelease`, and `apt update` would fail with hash-sum mismatches. Before switching a suite to a new snapshot, an adoption therefore waits:

1. up to 60 s for the suite to go quiet by itself: none of its `InRelease`/`Release` responses being sent, none finished within `flip_quiet_period`, no other suite's switch holding (even one that has not held a request yet), and no hold that held requests ended within that period;
2. up to 60 s for its turn to hold (one suite holds at a time); a suite that went quiet by itself still takes its turn, for a hold of no length;
3. then it holds: new `InRelease`/`Release` requests of the suite wait (`Release.gpg` never does) until the responses already being sent are done and the suite has been quiet for `flip_quiet_period`, and no other suite's hold that held requests ended within that period. Then the new snapshot is committed and the waiting requests are answered from it. If the suite is still not quiet 15 s into the hold, the new snapshot is committed then anyway, so the waiting requests still get it; a hold lasts at most 20 s, below apt's 30 s timeout.

A client that received the old `InRelease` therefore either finished receiving it at least `flip_quiet_period` before the switch, or waited and got the new one, so the index requests it sends within that period of its `InRelease` match it — also when many suites share its connection (mirror mode, or more than ten hosts). The period runs from when the cache finished sending, so on a slow link it is slightly shorter for the client. A client that fetches many indexes over a slow link may send some later than that; for a repository whose `Release` declares `Acquire-By-Hash: yes` (most distribution archives) apt asks for those by hash, and they are still served from the snapshot they name (below); many third-party and flat repositories do not declare it, and for them the switch's wait is the only protection. A switch takes at most 60 s + 60 s + 20 s plus its commit; `adoption.max_duration` does not cut it short. A shutdown abandons it at once: at the signal the daemon answers the waiting requests and abandons every switch that has not started its commit (nothing is committed or recorded, the adoption logs INFO `adoption_abandoned_at_shutdown` and is not counted as an adoption outcome, and the change is adopted after the restart). The first adoption of a suite whose `InRelease` (for a detached suite, `Release`) is exactly the one clients were already being served skips the wait, unless the daemon is stopping. Each switch logs `adoption_flip_barrier`; `acu_flip_barrier_active` shows how many switches are waiting or holding now, and the `request` line of an `InRelease`/`Release` request that waited in a hold carries `flip_held_ms`.

Residuals, each with a counter: a hold whose suite is still not quiet 15 s in — an `InRelease`/`Release` response still being sent then, or finished less than `flip_quiet_period` before (a client reading it more than 20 s − 5 s − `flip_quiet_period`, 5 s by default, into the hold: a slow, stalled or vanished client) — commits then; the waiting requests get the new snapshot, but such a late reader may fetch indexes that do not match the `InRelease` it got (`acu_flip_quiet_cut_short_total`); a commit that has not finished 20 s into the hold (very large suites) ends the hold anyway, and the waiting requests are answered from the snapshot still current then, so they may get indexes newer than their `InRelease` (`acu_flip_hold_expired_total`); a switch that waits 60 s for its turn commits without holding (`acu_flip_hold_skipped_total`; it takes three or four long holds ahead of it, so at least four suites switching at once, which needs `freshness.max_concurrent_adoptions` of 4 or more, or `0`); beyond 256 requests waiting in one hold, the next are answered at once (`acu_flip_waiters_over_cap_total`); and `InRelease`/`Release` responses that take 30 s or longer to send (very slow links) are not waited for (`acu_flip_slow_anchor_reads_total`). An alert on `rate(acu_flip_hold_expired_total)` catches commits slower than the hold. apt clients configured with an `Acquire::http::Timeout` below 20 s can time out while held; set `flip_quiet_period = "0s"` if you run such clients. A client still holding an older `InRelease` that asks for indexes by hash is still served: a by-hash file the current snapshot lacks is served from the newest `gc.keep_displaced` + 1 displaced snapshots of its suite (none when `gc.keep_displaced = 0`; `acu_serve_byhash_displaced_total`; the request line carries `snapshot_displaced`). Snapshot `InRelease`, `Release` and `Release.gpg` responses carry the snapshot's adoption time as `Last-Modified`, which grows with every switch — also for two switches in the same second and whatever the clock does — so `If-Modified-Since` never hides a new snapshot.

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
`require_signature = false` no longer lets unsigned data through the inline
path, and the detached form still requires both `Release` and `Release.gpg`:
unsigned metadata is adopted only in the unsigned form below, which trusts
nothing.

### Anchor forms and unsigned repositories

A suite is adopted in whichever form its upstream publishes: **inline** (a
clearsigned `InRelease`), **detached** (`Release` with `Release.gpg`) or
**unsigned** (`Release` alone). A suite with no snapshot yet asks for
`InRelease`, then `Release`, then `Release.gpg`; an answer counts as present
only when it is the file it claims to be. An HTML page served with status
200 is absent, like 401, 403, 404 and 410; any other answer (a 5xx, a
timeout, a redirect) fails the check without deciding anything. The admin
JSON's `suites[].anchor_form` names a suite's form.

Suites move between the signed forms by themselves. A detached suite asks
for `InRelease` on every check and moves to the inline form as soon as
upstream serves one. An `InRelease` whose move failed (a signing key the
cache does not trust) is tried again at its own retry time (2 minutes,
doubling to an hour, up to 75 minutes with jitter), whatever the detached
form publishes meanwhile — unless the detached `Release` is absent too,
which delays it by at most one retry of the detached form — and a new
`InRelease` is tried at the first check that sees it; so a publish that
fails in both forms is attempted once per `InRelease` retry and at most
twice per `InRelease` retry in the detached form, not at every check.
When a suite's current
anchor (`InRelease` for inline, `Release` for detached and unsigned) is
absent, the status page
shows "refused: anchor_absent" while the snapshot keeps serving. The suite
then asks for its anchor again only at its retry times (2 minutes,
doubling to an hour); checks in between, including those clients trigger,
ask upstream nothing, so a returning anchor is noticed at the next retry.
Once the absence has lasted `max(cooldown, 5 minutes)`, each retry also
probes the other forms, and the suite moves to the other signed form if
upstream serves it. The snapshot keeps serving however long its anchor
stays absent: clients of a release removed upstream (an end-of-life
distribution) keep getting it through the cache where upstream would
answer 404, so "refused: anchor_absent" is the sign to update their
sources. A suite never moves from a signed form to unsigned: a signed
suite whose upstream stops signing keeps its last signed snapshot (a
detached suite whose `Release` changes while its `Release.gpg` is absent
shows "refused: anchor_absent" and is paced the same way; a `Release.gpg`
request that fails — a 5xx, a timeout — logs WARN `freshness: Release.gpg
fetch failed; deferring adoption` and is tried again at the next check).
When a failed adoption is already recorded for the suite — typically the
move to its other signed form failed — the status page keeps showing that
failure's reason (for example `gpg_failed`) instead of "refused:
anchor_absent", and the suite is paced by its retry time the same way;
the first check after each retry asks upstream once more.
An unsigned suite moves to a signed form at the first check that finds
`InRelease` or `Release.gpg`. From that check on the cache has seen the
suite signed, so it is never adopted unsigned again, even when the signed
adoption fails (a key the cache does not trust) or the signature later
disappears.

Unsigned adoption has five safeguards:

1. `adopt_unsigned` (default `true`). With `false` an unsigned `Release`
   is refused ("refused: unsigned_disabled"), and the cache no longer
   keeps unsigned repositories fresh: an unsigned suite already adopted
   keeps serving its last snapshot, coherent but frozen, until the knob
   is turned back on, the suite is forgotten (`POST /forget`), or it is
   reaped after `gc.suite_idle_ttl` without a client — forgotten or
   reaped with the knob still off, it is then a suite not yet adopted; a
   suite not yet adopted stays unadopted and is served as the cache first
   stored its files. Set it only if you do not serve unsigned
   repositories through the cache. The suite is asked upstream only at
   its retry times; a signature appearing upstream still moves it to a
   signed form, at the next retry.
2. A suite's first unsigned adoption waits for two checks that see the same
   `Release` at least `max(cooldown, 5 minutes)` apart — an hour apart when
   `InRelease` or `Release.gpg` answers 401 or 403 at the confirming check,
   since a firewall or an S3 permission error can hide a signature (a 403
   that later turns into 404s confirms after the shorter gap). The
   confirming check is the first of the suite's retries (2 minutes,
   doubling, each ±25 %) that comes that long after the first
   observation; a due retry runs at the scheduler's next tick, every
   quarter of `periodic_refresh`, or at a client's `apt update` after it.
   So with the default `cooldown` and `periodic_refresh` a new unsigned
   repository is adopted at least 5 minutes after its first `apt update`
   through the cache — usually 5 to 15. Until then the status page shows
   "refused: unsigned_unconfirmed". A `Release` generated anew for each
   request, or one that differs between
   load-balanced backends, never shows the same bytes twice and stays
   unconfirmed; it is asked for again at its retry times (2 minutes,
   doubling to an hour) or once `periodic_refresh` has passed, like any
   suite whose first adoption is deferred. Later updates of an unsigned
   suite are adopted at once.
3. Never a downgrade: a suite the cache has seen signed is never adopted
   unsigned ("refused: unsigned_downgrade_refused"). Seen signed means it
   has a signed snapshot (the current one, or one of the displaced
   snapshots the cache keeps), or the cache served its signed `InRelease`
   or `Release.gpg`, a check found one upstream, or an earlier version had
   cached one (recorded at upgrade). The record covers the repository
   path on its host under both `http` and `https`, so a suite seen signed
   over one scheme is never adopted unsigned over the other. The rule is
   checked before an adoption slot is taken and again inside the commit,
   so a signed anchor served while an unsigned adoption runs still stops
   it. Just before the commit the cache also re-reads the signed anchors
   it has cached for the suite, under both schemes, and records any it
   finds (one cached over the other scheme during the commit's flip
   barrier, for a suite refused at `freshness.max_suites`, is found only
   by the next attempt); if it cannot read them, the adoption is refused
   as well (logged as `adoption_signed_anchor_remark_failed`): that
   refusal means the cache could not rule out a signature.
4. At most 1,000 files declared in the `Release`'s SHA256 and SHA512
   lists, each path counted once (files listed only under MD5Sum or SHA1
   do not count), checked before an adoption slot is taken
   ("refused: unsigned_too_large").
5. An unsigned snapshot vouches no `.deb`, never counts for strict mode
   and never records its suite as signed.

Threat notes. The record that a suite was seen signed cuts both ways:
anyone who can make the cache see one signed-looking `InRelease` or
`Release.gpg` for a repository path — served to a client, or found by a
check; a man in the middle, a compromised upstream, or a LAN client
pointing the cache at a host it controls — blocks unsigned adoption of
that path until its suite is forgotten (`POST /forget`, see *Forgetting a
suite* under `[admin]`). That denial of service is the accepted price
of never downgrading. A man in the middle between the cache and upstream
can make the cache adopt forged unsigned content only for a suite never
seen signed — for a suite not yet adopted only after two consistent
observations, but for an unsigned suite already adopted at the first check
that sees it, since updates of an unsigned suite are adopted at once
(safeguard 2); apt clients that verify signatures reject it, and
`[trusted=yes]` clients accept it as they would when fetching directly.
A suite whose pre-upgrade snapshot was
quarantined because its stored `InRelease` was missing, corrupt or
oversized (see *Upgrading an existing cache*) may have no record that it
was signed until a check sees its signature upstream; if its signatures
are gone by then (stripped by a man in the middle, for instance), it
adopts unsigned. Under the default allow-all upstream policy
with adoption enabled, a LAN client can make the cache adopt unsigned
directories it controls; that is bounded by the per-host adoption slot,
the reserved slot, `max_duration`, the 1,000-file cap and
`freshness.max_suites`, but not in bytes: each file can be as large as
its server sends within `upstream.total_timeout` (no response-body
ceiling, and gzip is decompressed in transit: the 2026-09 security
review's A-2 and VA-3, open), and an adopted file stays while its
snapshot is current or kept displaced, until the suite is reaped or
forgotten. To harden, set `adopt_unsigned = false` (unsigned
suites already adopted then stay at their last snapshot, see safeguard 1)
or narrow `upstream.allowed_host_regex`.

`Release` files that list only SHA512 hashes (some vendor repositories)
adopt: every declared SHA256 and SHA512 hash of a file is checked, and
by-hash requests are answered for both algorithms, from the current
snapshot and from the displaced ones kept for retained by-hash. An
optional file that lists only a SHA512 hash and fails its integrity check
is not retried by automatic repair; the reconcile on unchanged checks
still heals the index files apt requests.

A `Release.gpg` in binary form (not ASCII-armored) is accepted: the cache
adopts a repository that publishes `Release` with such a signature. apt
2.2 to 3.2 refuse it, through the cache or directly ("Detached signature
file … is in unsupported binary format"), so such a repository is
unusable unless it also publishes an `InRelease`, which apt then uses
instead (Jitsi's repository does).

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

### Upgrading an existing cache

Upgrading the Debian package to 1.1.0 or later restarts a running daemon
(`try-restart`: a stopped daemon stays stopped until you start it), so its
first start after the upgrade, and what that start does below, begins as the
package is upgraded; earlier packages left the old binary running until its
next start. After the upgrade, check `systemctl status apt-cacher-ultra` (or
`journalctl -u apt-cacher-ultra`): apt does not show a new binary that exits
at startup. The unit is `Type=simple`, so systemctl reports only a restart job
that fails, not a daemon that exits once started, and `Restart=always` starts
it again every 5 seconds.

On its first start after an upgrade, the daemon audits every metadata snapshot
adopted by earlier versions before it serves anything (a large cache can take
a little while; progress is logged per 100 snapshots, and because the
listeners are already open, connections made meanwhile wait — apt clients may
time out once). A snapshot whose stored `InRelease` is not a signed, clean
clearsigned message — a plain-text file adopted with `require_signature =
false`, or one carrying declarations outside its signature — or whose pool
file is missing or corrupt, is quarantined: it stops serving and vouching,
its suite is served from the ordinary cache, and a `legacy_snapshot_quarantined`
line names it. When the stored `InRelease` is corrupt — its bytes no longer
match the hash recorded for them — the audit also deletes every cached copy
of it (each `url_path` row that holds that file, under any scheme or host;
the line's `anchor_rows_deleted` counts them) and removes the corrupt file,
so the next client update fetches a fresh one from upstream instead of
being served bytes apt rejects, also when upstream still serves the same
file (`legacy_anchor_discard_failed` says when the file could not be
removed; a crash in the instant between the quarantine and the removal
leaves the file too, with no such line, until a re-adoption replaces it);
a missing file, or one too large for the audit to read
(`anchor_oversized`, not proven bad), keeps its rows. A snapshot whose
pool file cannot be read at all is deferred, not judged: an ERROR
`legacy_trust_audit_deferred` names it at every start, and it keeps its
rows and files until the pool is fixed or the suite is reaped or
forgotten (`POST /forget`). With adoption enabled, a quarantined suite
adopts again as a suite without a snapshot does (see *Eager first
adoption*), whatever it was quarantined for and without comparing its
stored copy with upstream: at its first check when a client fetched one of
its anchor files in the 7 days before the upgrade, otherwise once a client
asks for it again. A suite quarantined `anchor_corrupt`, whose cached
copies the audit deleted, waits for that request. One quarantined because
its stored file was lost or corrupt may have no record that it was signed
(see the threat notes in *Anchor forms and unsigned repositories*). A
quarantined plain-text
suite adopts again in the unsigned form, through its `Release` (its
plain-text "InRelease" counts as absent), once a client has asked for it
and two checks have seen the same `Release` (see *Anchor forms and
unsigned repositories*), unless the cache has seen it signed; an unsigned
snapshot vouches no package. In strict mode, a quarantined
suite's packages are refused while other suites on the same host have
complete coverage, as for any unadopted suite. While inline snapshots are
queued for the audit, the cache directory's `pool/` must be present: the
daemon refuses to start when `pool/` is missing, unreadable or empty, or
when it holds none of the queued inline snapshots' files and none of a
sample of the files the current snapshots use, two or more files checked
(a foreign or unmounted pool; snapshots that share an `InRelease` share
its one file). Mount or restore it, or wipe the cache directory to start
empty. The audit then changes nothing, but the migration before it has:
the database is at schema 7, which 1.0.x refuses, so going back to 1.0.x
means restoring the `cache.db` copied before the upgrade: stop the
daemon first (a refused start runs again every 5 seconds and would
migrate the copy again, and a copy put back under a running daemon can be
undone by its checkpoint), install the 1.0.x package, put the copy back
as `cache.db` (with its `cache.db-wal` if one was copied, and no other
`-wal` or `-shm` file beside it), then start it; the refusal says to stop
the daemon and restore the copy. Otherwise a missing pool file does not
stop the daemon, even when it belongs to the only queued snapshot, as
long as the pool holds one of the files checked, or that file is the only
one checked: that snapshot is quarantined. So is a snapshot whose file
could not be read at an earlier start and was then removed, on the same
condition. A single pool file that cannot be read for another reason (permissions, I/O error) does
not stop the daemon either: that snapshot keeps serving as before, a
`legacy_trust_audit_deferred` error names it, and the next start retries it.
The audit resumes if the start is interrupted and is a no-op afterwards. A
suite whose signed `InRelease` was served by an earlier version is recorded
as signed from cached anchors, so it is never adopted unsigned; one whose
cached anchor was already garbage-collected leaves no record. Suites whose
`InRelease` an earlier release cached but never tracked are tracked, with
their signed-seen record,
on their first cache hit. A cache with more than `freshness.max_suites`
suites, or a tenth of it or more on one host, logs WARN
`suite_caps_exceeded` at start; see *Tracked suites*. From an hour after
the start the suite reaper (see *Suite reaping* under `[gc]`) deletes the
suites no client has asked for in `gc.suite_idle_ttl`, counted from the
last request for one of their anchor files before the upgrade (from 7 days
before the upgrade for a suite with none: such a suite goes about 23 days
after the upgrade unless a client asks for it), the rows an earlier
release created for paths this version no longer tracks, and the suites
whose cached anchor files are all error pages — with `gc.enabled = false`
too. An offline or archive cache that must keep serving such suites —
including one that relied on `gc.enabled = false` to keep everything —
sets `gc.suite_idle_ttl = "0s"` before upgrading.

After the upgrade, suites without a snapshot adopt eagerly only if a client
fetched one of their anchor files in the 7 days before the upgrade, or asks
again (see *Eager first adoption*); with the default
`max_concurrent_adoptions = 2` these first adoptions run one at a time,
while signed updates keep their own slot. Snapshots adopted before the
upgrade carry no SHA512 by-hash names: such requests are answered 404
from the snapshot, and apt then fetches the file by its name, until the
suite's next adoption. Releases before 1.1.0 never adopted a flat
repository, whose signing key may be installed only on the clients: load
it on the cache host before upgrading (and, under
`require_pinned_signer`, add its `[[trusted_signer]]` rule), or each of
its adoptions fails and is retried (see *Flat repositories*).

## `[hot_packages]`, `[hold_packages]`, and `[retention]`: package working set

| Section / key | Type | Default | Behavior and constraints |
| --- | --- | --- | --- |
| `hot_packages.window` | duration | `"24h"` | Look back this far for client-requested packages when choosing versions to prefetch during adoption. `"0s"` disables hot-package prefetch. Nonnegative. |
| `hold_packages.window` | duration | `"24h"` | Grace after GC first notices a cached URL is no longer retained by recency or current-snapshot rules. `"0s"` permits deletion in that GC batch. Nonnegative. |
| `retention.max_versions_per_package` | integer | `3` | Newest distinct Debian versions retained and eligible for hot prefetch per package name and architecture, per suite with a current snapshot (a reaped suite has none: see *Suite reaping* under `[gc]`). Must be at least `1`. |

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
| `refuse_unvouched_debs` | boolean | `false` | Reject `.deb` requests with `502` and `Retry-After` when the path lacks a current-snapshot hash and all current signed snapshots for that host have proven complete package coverage (unsigned snapshots never count). Inert when adoption is disabled; incomplete coverage retains the permissive fetch behavior. A request first waits up to 15 seconds for a signed adoption running on its host; see below. |

These switches do not disable the normal download-time hash checks. A detected
at-rest mismatch logs an error and removes the corrupt pool file. Package misses
can fetch it again; affected snapshot metadata fails closed until restored.

With `refuse_unvouched_debs = true`, a `.deb` that no current snapshot
vouches for, requested while a signed adoption is running for a suite on
the same upstream host, waits for the host's signed adoptions to end and
is looked up once more, so a package from an update being adopted is
served rather than refused (`acu_strict_adoption_waits_total{host}` counts
the requests that waited). The wait lasts at most 15 seconds, and never
past 25 seconds after the request arrived, so that a miss still has time
to download the package before apt's 30-second timeout; a request with
less than a second of that left, or arriving while 64 requests of its host
already wait, is refused at once. Each request waits on its own: requests
for the same package each count toward the 64, and a client fetching
several unvouched packages one after another while a long adoption runs
waits up to 15 seconds for each. An adoption ends after its flip barrier
(up to 140 seconds for a busy suite; see *Flip consistency*), so a package
whose snapshot is still in its barrier is likely to wait the full 15
seconds and be refused. Refusals also remain when an adoption takes longer
than that (large `dists/` suites), when a first adoption is still waiting
for a slot, and for packages of an unsigned repository on a host
whose signed suites all have complete coverage: unsigned snapshots never
vouch and never count, so a host whose only snapshots are unsigned is
never strict. Strict posture changes as eager first adoptions land after
an upgrade. Conversely, a signed snapshot with incomplete coverage turns
strict mode off for its whole host. A `.deb` whose name begins like an
index file (a package named `icons-foo`, a flat repository's
`Packages_1.0_all.deb`) is a package all the same: vouched, checked and
refused like any other.

Flat repositories follow the same host-wide rule. Releases before 1.1.0
never adopted a flat repository, so one never made its host strict; now:

- A host that serves only flat repositories is strict as soon as one of
  them has a signed snapshot, as long as every signed snapshot on the
  host has complete coverage: a `.deb` there that no signed snapshot's
  `Packages` declares — a file the vendor serves beside the repository
  without listing it, a package of an unsigned repository, or one of a
  repository not adopted yet (after an upgrade the host's repositories
  adopt one at a time) — is refused, where earlier releases passed it
  through.
- A signed flat snapshot with incomplete coverage turns strict mode off
  for its whole host, as an incomplete `dists/` snapshot does: a
  `Packages` stanza without a SHA256 (`no_sha256`), say, or a signed
  `Release` that lists no root `Packages` (`no_packages_members`). The
  adoption logs INFO `package_coverage_incomplete` with the snapshot and
  the reason, and a `.deb` then served unvouched on that host logs INFO
  `unvouched_deb_passthrough_no_coverage` (at most once an hour per path)
  naming the snapshot as `incomplete_snapshot_id`.
- The first install from a flat repository a client has just added to a
  strict host — a new Kubernetes minor on pkgs.k8s.io, say — waits up to
  15 seconds while that repository's first adoption runs, and is refused
  while the adoption is still waiting for a slot; an attempt after the
  adoption has committed succeeds.
- A flat repository refreshed without a `Release` has no snapshot (see
  *Flat repositories* under `[freshness]`): it never changes its host's
  posture, and on a strict host its packages are refused like any
  unvouched package.

Strict mode fails open on a database error: when the cache cannot read a
package's vouching hashes or its host's snapshot coverage, the `.deb` is
served as if strict mode were off, so a transient SQLite fault does not
turn into a refusal of every package. Each such read logs WARN
(`package_hash lookup failed` on a cache hit, `strict-mode coverage lookup
failed`, and `package_hash validate lookup failed` after a download). A
miss reads the hashes before its download and again after it: when only
the first read fails (WARN `package_hash_prefetch_lookup_failed`), the
second decides, and a package strict mode refuses is refused after its
download rather than served. A read fault never overturns a refusal
already decided: before refusing a package while no adoption runs on its
host, the cache reads its hashes once more, in case an adoption has just
vouched for it, and only hashes found there change the answer — a fault
in that read is logged as above and the package is refused. A request
whose client left (or, through the HTTPS proxy, whose tunnel is shutting
down) is not a database error: it is answered 503 and never served an
unvouched package.

## `[gc]`: garbage collection

| Key | Type | Default | Behavior and constraints |
| --- | --- | --- | --- |
| `enabled` | boolean | `true` | Run startup and periodic garbage collection, including the startup orphan-file scan. `false` disables those passes and logs WARN `gc_disabled`, whose `detail` ends by saying whether suite reaping still runs: `suite reaping still runs: gc.suite_idle_ttl = 720h0m0s …` (the value in Go's duration format), or, with `suite_idle_ttl = "0s"`, `suite reaping is off too (gc.suite_idle_ttl = "0s"), but still deleted are …` and what still deletes then: suites evicted at the `freshness.max_suites` total cap and, daily, signed-seen records of untracked paths last seen over a year ago. It does not stop the suite reaper, which deletes idle suites' cached index files whatever this says (see *Suite reaping*; `suite_idle_ttl = "0s"` turns its reaping off). |
| `interval` | duration | `"24h"` | Delay after a GC tick completes before the next periodic tick. Must be positive; disable through `enabled`. |
| `batch_size` | integer | `100` | Maximum URL-path/blob rows processed per GC batch. At least `1`. |
| `snapshot_batch_size` | integer | `10` | Maximum snapshots removed per batch. Snapshot deletion can cascade through many member rows. At least `1`. |
| `max_tick_duration` | duration | `"5m"` | Budget for startup/periodic GC passes, checked between batches; remaining work resumes later. The startup pass, which runs before the listeners serve, leaves the retired snapshots (of a reaped, evicted or forgotten suite, or one the upgrade's trust audit quarantined) to a run an hour after the start, when `interval` is longer than an hour, and to the periodic passes (see *Suite reaping*); it deletes only a retired one never adopted whose heartbeat went stale, as it does any abandoned adoption candidate. That run deletes only retired snapshots, within this budget too (INFO `gc_retired_run_complete`). Must be positive. |
| `blob_grace` | duration | `"5m"` | Grace after a blob's reference count reaches zero before deletion. At least `"1s"`, and greater than twice `heartbeat_interval`. |
| `keep_displaced` | integer | `3` | Recent displaced snapshots kept per suite for inspection. `0` retains none; must be nonnegative. A displaced snapshot the upgrade's trust audit has not judged yet (one whose pool file it could not read: see *Upgrading an existing cache*) is kept, and takes none of the `keep_displaced` places, until the audit decides it: it may be the suite's only evidence of a signature. Does not by itself retain superseded package binaries. Also bounds retained by-hash: a by-hash request the current snapshot lacks is served from the newest `keep_displaced` + 1 displaced snapshots of its suite, and `0` turns that off (see *Flip consistency*). |
| `pool_scan_workers` | integer | `4` | Workers for the startup orphan-file scan. At least `1`. |
| `heartbeat_interval` | duration | `"60s"` | Refresh in-progress adoption liveness and blob protection. Positive and subject to both bounds below. |
| `url_path_ttl` | duration | `"168h"` | Requests within this window protect cached URL rows. Older or never-requested rows are evaluated against snapshot/version retention and hold grace. `"0s"` disables URL-path expiration; other values must be at least `"1m"`. |
| `suite_idle_ttl` | duration | `"720h"` | A tracked suite no client has asked for in this long is reaped: no longer tracked, its cached index files and snapshots deleted (see *Suite reaping*). The reap deletes no package, but the suite's packages lose the version retention its snapshot gave them: with `enabled = true` and a nonzero `url_path_ttl`, GC then deletes those no client asked for within `url_path_ttl`, after `hold_packages.window`. `"0s"` disables suite reaping (a host at its per-host cap then loses suites only to eviction at the total cap, see *Tracked suites*); other values must be at least `"24h"`. |

Startup still runs the orphan-file scan and a GC tick. Periodic ticks wait a full
`interval` after the previous tick completes, so a long run cannot trigger an
immediate catch-up run. Existing configuration files that explicitly set
`interval = "1h"` keep that setting; change it to `"24h"` for daily collection.

Less frequent collection retains eligible data longer and can require more disk
space. The interval controls cleanup only: freshness checks, integrity checks,
adoption heartbeats, and offline serving remain independent. GC resumes unfinished
work across ticks in URL-path, snapshot, then blob order; a large backlog can take
several intervals to drain. `max_tick_duration` is checked between batches and
does not interrupt a running batch or bound the startup orphan-file scan.

Both heartbeat conditions must hold:

```text
2 × gc.heartbeat_interval < gc.blob_grace
gc.heartbeat_interval < max(upstream.total_timeout × upstream.max_retries, 30m)
```

The second expression is the implementation's stale-adoption grace formula;
it is separate from the actual fetch retry budget. All `[gc]` validation still
applies when `enabled = false`.

### Suite reaping

The suite reaper deletes the tracked suites (see *Tracked suites*) that no
client has asked for in `suite_idle_ttl`: a client asks for a suite when it
fetches the suite's `InRelease`, `Release` or `Release.gpg` — or, for a
flat repository, its root `Packages` or `Sources` — or a `.deb` its
snapshot vouches for, through the cache. The reaper runs an hour after
the daemon starts and then every hour, whatever `interval` and `enabled`
say. A run deletes at most 250 of the suites reaped however recently
they were asked for (below), first, and at most 25 idle ones, those
asked for least recently first; a suite being checked or adopted at
that moment is skipped — it counts toward its kind's number — and tried
again at the next run. Reaping a suite deletes its cached index files
(its `InRelease`, `Release`, `Packages` and the rest of its `dists/`
tree — a flat repository's root files, never its `./` twin's — in
batches of 1,000 files), retires its snapshots and deletes its
row (INFO `suite_reaped`; `acu_suites_reaped_total{reason}` counts the
reaps). Each run that completes logs INFO `suite_reaper_run_complete` and
sets the gauge `acu_suite_reaper_last_run_unixtime`, which has no series
until the first completed run, an hour after the start; a run whose
listing failed, that panicked or that a shutdown cut short does neither.
To catch a reaper that stopped, alert on
`absent(acu_suite_reaper_last_run_unixtime)` held by a Prometheus
`for: 3h`, or on `time() - acu_suite_reaper_last_run_unixtime > 3 * 3600`:
a restart or one failed run never pages, and a reaper that stopped
completing runs pages within about three hours (the gauge's entry in
[log fields](log-fields.md) gives the reasoning). The age leg alone never
fires for a reaper that has not completed a run since its start.
The reaper deletes no package, but a reaped suite's packages
lose the version retention its snapshot gave them
(`retention.max_versions_per_package` counts versions in current
snapshots only; a package another suite's snapshot lists keeps that
one's): with `enabled = true` and a nonzero `url_path_ttl`, GC deletes
those no client asked for within `url_path_ttl`, after
`hold_packages.window`, and their pool files after `blob_grace`. GC
deletes the retired snapshots an hour after each start (when `interval`
is longer than an hour: a run of that work alone, INFO
`gc_retired_run_complete`, so a daemon restarted more often than
`interval` still deletes them) and at each periodic pass, but not in the
pass at startup, which deletes only a never-adopted one whose heartbeat
went stale, as it does any abandoned adoption candidate, so a restart
after many reaps does not keep the listeners closed while they are
deleted. The pool files nothing else uses then go at the next blob pass
(the next start's, or the next periodic pass's), once `blob_grace` has
passed. With `enabled = false` suites are still reaped, but their
snapshots and pool files stay until GC runs again.

A reaped suite whose clients come back is tracked again at their first
request and, with adoption enabled, adopted again at its first check
(unchanged upstream content included; an unsigned repository after the
two observations of its first adoption). Its files then come from
upstream again: a cache that must keep serving suites nobody asks for
for a while — an offline or archive cache — sets `suite_idle_ttl = "0s"`.
With `adoption.adopt_unsigned = false` a returning unsigned repository
is not adopted again: it is refused `unsigned_disabled` and served as
the cache first stores each of its files, never refreshed (see
`adopt_unsigned` under `[adoption]`). A client in the middle of an update
when its suite is reaped can see one Hash Sum mismatch, as with a forget
(see *Forgetting a suite* under `[admin]`).
The record that a repository path was seen signed survives the reap, so
the returning suite is never adopted unsigned (see *Anchor forms and
unsigned repositories*). A suite whose only evidence of a signature is
its signed snapshot — one the upgrade's trust audit could not read yet,
for example (see *Upgrading an existing cache*) — gets that record when
it is reaped or evicted at the `freshness.max_suites` cap, before its
snapshot is retired. That record stays even if a later start's audit
reads the snapshot and quarantines it: the path then keeps refusing an
unsigned `Release` (`unsigned_downgrade_refused`) until a suite of it,
tracked again at a client's request, is forgotten (`POST /forget`), or,
while no suite of the path is tracked, the reaper deletes the record a
year later. The reaper deletes that record only when no
suite of the path has a row and the path was last seen signed — or a
suite of it last reaped, evicted at the `freshness.max_suites` cap or
forgotten, each of which moves the record's date up to that moment —
more than a year ago. With
`integrity.refuse_unvouched_debs = true`, a reaped suite's packages are
no longer vouched for, and reaping a suite whose signed snapshot was the
host's only one with incomplete package coverage turns strict mode on for
that host, as long as the host keeps another signed snapshot (see the
strict-mode notes under `[integrity]`).

Two kinds of suites are reaped however recently they were asked for, at
every run: rows an earlier release created for paths this version no
longer tracks (`reason=non_canonical`; such a path's cached files are
left to URL-row expiry, `url_path_ttl`), and suites without a snapshot
whose cached `InRelease`, `Release` and `Release.gpg` are all something
else, such as an HTML error page (`reason=junk`), typically rows an
earlier release created. A suite of the second kind is reaped only when
every such file was fetched at least a day earlier, so a repository that
answered with an error page for a few hours is kept, and so is one whose
cached `InRelease` is a plain unsigned `Release` (the cache adopts such a
repository through its `Release`); a file that cannot be read is never
taken for an error page. A flat repository the cache has never seen
signed (see *Flat repositories* under `[freshness]`) is judged by its
cached `InRelease`, `Release` and `Release.gpg` and its cached root
`Packages` and `Sources` files together, by the same rules: it is reaped
as junk only when it has at least one of them and each is something else
than its name says (an HTML page cached as `Packages.gz`, say) and was
fetched at least a day earlier — so a healthy repository without a
`Release` on a host that answers `InRelease` and `Release` with an HTML
page keeps its good `Packages.gz`. A flat repository seen signed, like
any `dists/` suite, is judged by its anchor files alone. A run that
cannot read those files, or whether a repository was seen signed, judges
the repositories concerned at its next run.

A suite whose last request lies in the future (the clock was stepped
back) is never idle — the two kinds above are reaped all the same — and
the furthest such date (of a path this version tracks) is logged as WARN
`clock_step_detected` (`stamp=last_requested_at`) by the first run that
reaps after a start, and again only when it changes. A forward step can
make every suite look idle: the hour before the first run after a start
gives clients time to ask again;
a step while the daemon runs (a resumed virtual machine or laptop: the
wall clock moved more than an hour further than the time that passed) is
logged as WARN `clock_step_detected` with `stamp=wall_clock` and holds
the reaper's deletions back for an hour; a suite reaped anyway comes back
at its next request, as above. A deleted record of a signed path does
not come back, so a step never makes the reaper delete one early: a step
it sees while running is left out of the records' age until the daemon
stops, and its first record sweep comes a day after the start (then one
a day), so a clock that was ahead at the start has a day to be
corrected. Two limits remain: a step the daemon was restarted across,
not corrected within a day of the start, makes the sweep delete records
up to the step early; and a daemon restarted at least once a day
(before its first sweep comes due) never sweeps those records. `"0s"`
turns suite reaping off; the reaper then only deletes the year-old
records of signed paths no suite tracks.

## `[admin]`: status, metrics, and maintenance

The separate **HTTP** admin listener serves the status page, `/metrics`, and
`/healthz`, plus the mutating `POST /reconcile` and `POST /forget` maintenance
endpoints. It is not a read-only interface. Keep access restricted to trusted
operators; if exposed beyond loopback, protect the network path or place it behind a TLS reverse proxy.
HTTP Basic authentication does not encrypt credentials in transit.

Both mutating endpoints refuse a request with 403, the body naming the
rule, unless it carries the header `X-ACU-Admin: 1`. A cross-site form
cannot send that header, and a page on another origin that sets it
needs a CORS preflight the listener never grants, so a browser request
from another origin cannot trigger them. A `Sec-Fetch-Site` header,
which browsers send to https and loopback addresses, must also be
`same-origin` or `none` when present. Without `htpasswd_file` they also
refuse a request whose `Host` is a name other than `localhost` or the
host name in `listen`. That stops DNS rebinding against them: a web
page whose own name is made to resolve to the listener's address. Call
them by IP address (`http://127.0.0.1:6789/…`), as `localhost`, or by
the host name in `listen`. Behind a reverse proxy that forwards the
client's `Host`, set `htpasswd_file`, or have the proxy send
`Host: 127.0.0.1` only where a rebinding page cannot reach it: behind
TLS at the proxy (a page under another name fails the certificate
check), or in a server block that matches the proxy's own name — never
in a default or catch-all one, which would pass a rebinding page's
requests on as if they named the address. The status page, `/metrics`, `/healthz` and the
profiling routes have no `Host` check, so a Prometheus server may scrape
by name, and without `htpasswd_file` a rebinding page can still read
them.

| Key | Type | Default | Behavior and constraints |
| --- | --- | --- | --- |
| `enabled` | boolean | `true` | Bind the admin listener and run its background refresh work. `false` leaves proxy service running and logs a warning. |
| `listen` | string | `"127.0.0.1:6789"` | Admin bind address. A non-loopback address without authentication emits a startup warning. Explicit empty string is invalid when enabled. |
| `htpasswd_file` | string | `""` | Optional readable Apache htpasswd file with at least one user; only bcrypt (`$2a$`, `$2b$`, `$2y$`) entries are accepted. Empty disables authentication. Applies to every admin endpoint. |
| `pprof_enabled` | boolean | `false` | Enable `GET /debug/pprof/profile`, `/debug/pprof/heap`, and `/debug/pprof/goroutine` on the admin listener, with the same authentication. CPU capture defaults to 30 seconds; `?seconds=N` accepts integers from 1 through 300. Profiles expose process details; enable only for trusted operators. |
| `gauge_refresh` | duration | `"30s"` | Delay after a completed refresh before recomputing gauges. Values can lag by this delay plus refresh work time. Positive and at most `"1h"`. |
| `read_timeout` | duration | `"5s"` | HTTP request-line/header timeout, not a response or request-body timeout. Positive and at most `"1m"`. |
| `idle_timeout` | duration | `"30s"` | Keep-alive idle timeout. Positive and at most `"10m"`. |
| `metric_series_cap` | integer | `1024` | Per-metric limit on distinct label combinations. Excess new series are dropped and a warning is emitted. At least `1`. |

Create bcrypt entries with `htpasswd -B`. The password file reloads on an admin
request when its modification time (in whole seconds) or size changes. A same-size
rewrite within the same second may be missed. Reload failures retain the previous
credentials and log a warning. Changing the configured file path needs a daemon
restart. These per-key constraints are skipped when `admin.enabled = false`.

Profiling is inactive until explicitly enabled and requested. Profile routes
accept GET only, use binary `go tool pprof` format, and do not force a Go garbage
collection. CPU sampling stops on client disconnect or shutdown; a capture
interrupted by shutdown returns `503`. Profile response writes have a five-second
deadline and are interrupted when the client or server stops. No profiling index,
command-line, trace, or other debug routes are exposed.

See the [idle CPU investigation](idle-cpu-review.md) for findings, maintenance
metrics, tuning tradeoffs, and profile-capture commands.

### Forgetting a suite (`POST /forget`)

`POST /forget` reaps one tracked suite at once, however recently it was
used (see *Suite reaping* under `[gc]`): its cached index files, its
snapshots and its row are deleted. The next request for it tracks it again
as a new repository and, with adoption enabled, adopts what upstream
serves then. That is the way out of two refusal states: a deliberate
upstream rollback (refused `upstream_older` or `rollback_refused`), and a
vendor that stopped signing (`unsigned_downgrade_refused`). The latter
needs `adoption.adopt_unsigned = true` (the default): with the knob off,
the forgotten vendor's unsigned repository is not adopted — it is refused
`unsigned_disabled` and served as the cache first stores each of its
files, never refreshed (see `adopt_unsigned` under `[adoption]`).

When no other suite of the repository path has a row — under `http` or
`https`, or as its `/.` twin — forgetting a suite also deletes the record
that the path was seen signed, and the cached `InRelease`, `Release` and
`Release.gpg` of the path and of its `/.` twin under either scheme (a
signed copy left there would mark the path signed again). That is what
lifts `unsigned_downgrade_refused`, and it happens whatever the suite was
refused for, or if it was refused at all: forgetting a suite forgets that
its path was signed, so until the cache sees a signature there again
(served to a client or found by a check), never-downgrade does not
protect the path — an unsigned `Release` served there meanwhile is
adopted like any unsigned repository's (see *Anchor forms and unsigned
repositories*). While another suite of the path has a row, the record
and those files stay and the answer names that suite: a
suite forgotten to lift `unsigned_downgrade_refused` is then refused
again — forget the other one too.

A client in the middle of an update when its suite is forgotten can see
one Hash Sum mismatch: forgetting does not wait for the update to end, as
an adoption's flip does (see *Flip consistency*). A forget still running
when the daemon begins shutting down is cut short and answers 503: the
index files it deleted stay deleted (the anchors go last), and it can be
repeated once the daemon is back (409 then means it had completed after
all, or the suite is busy).

`scheme`, `host` and `suite` are the suite's canonical scheme, host and
suite path as the status page shows them: its Host column reads
`scheme://host`, so pass the part before `://` as `scheme` and the rest
as `host` (a `host` holding `://` is refused with 400). `scheme` is
optional (`http` or `https`, default `https`). The request must pass
the mutating endpoints' checks described under
[`[admin]`](#admin-status-metrics-and-maintenance): the header
`X-ACU-Admin: 1`, no browser request from another origin, and, without
`htpasswd_file`, an IP address, `localhost` or `listen`'s host name as
the host. From `curl` and scripts, that means sending the header, and,
with `htpasswd_file` set, the credentials (`-u user`):

```sh
curl -sS --fail-with-body -X POST http://127.0.0.1:6789/forget -H 'X-ACU-Admin: 1' \
    --data-urlencode 'scheme=https' \
    --data-urlencode 'host=packages.microsoft.com' \
    --data-urlencode 'suite=/ubuntu/24.04/prod/dists/noble'
# 202 Accepted = forgotten (the work is done when the answer comes); the body
#   names the suite, the signed-path records deleted and the suites that kept them
# 400 = host or suite missing, a scheme other than http or https, a host
#   holding scheme:// (pass the scheme in scheme), or a body that is not a form
# 401 = htpasswd_file is set and the credentials are missing or wrong (add -u user)
# 413 = a body over 8 KiB
# 403 = the header is missing, a browser request from another origin, or,
#   without htpasswd_file, a host name other than localhost or listen's (use the address)
# 409 = busy (being checked or adopted: try again) or no such suite (check the
#   scheme, which defaults to https, and the host and suite the status page shows)
# 500 = a database error (logged as WARN admin_forget_failed); repeat it:
#   409 then means it was forgotten after all (or is busy)
# 503 = the daemon began shutting down while the forget ran; repeat it once it is back
```

`--fail-with-body` needs curl 7.76 or later; an older curl (Debian 11,
Ubuntu 20.04) refuses it: use `-sS -w '%{http_code}\n'` instead, which
prints the body and then the status.

Each accepted request is logged as INFO `admin_forget_requested` (a
request refused with 400, 403 or 413 is not), and each suite forgotten
as INFO `suite_forgotten` — with `settled=true` when its final delete
committed although the request then failed (500, or 503 at shutdown)
and a fresh read then found the row gone. A 409 is logged at DEBUG as
`suite_not_forgotten`, whose `reason` says which case it was (`no_suite`,
`busy`), or at WARN with `reason=changed`, which should never occur (a
bug: please report it).

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
