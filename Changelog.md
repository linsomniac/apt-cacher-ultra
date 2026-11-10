# Changelog

A brief list of the changes in each release. Add an entry here with each
change; the release notes for a release (in `docs/release-notes/`) give
the detail an operator needs before upgrading.

## Unreleased: 1.1.0

Collected until 1.1.0-rc1. The detail, and what to do before upgrading
from 1.0.x, is in [docs/release-notes/1.1.0.md](docs/release-notes/1.1.0.md).

### Added

- Persistent admin usage statistics: data fetched/served, cache hits, estimated
  net bytes saved and average effective transfer rates, with lifetime and
  since-start views in HTML and JSON. Includes background downloads and retries;
  checkpoints every 30 seconds and at graceful shutdown. Schema 9 adds usage
  storage after schema 8's metadata migration. Requested by @sdh91 in issue #11;
  see [usage statistics](docs/usage-statistics.md).
- Optional `cache.db_dir` to keep SQLite database files on separate storage
  from cached content. Empty or omitted preserves the existing layout.
  Suggested by @grifferz in issue #13; see
  [separate database storage](docs/configuration.md#separate-database-storage).
- Proxy-port `GET`/`HEAD /healthz` for load balancers, also available with
  admin disabled. Both proxy and admin health checks now require completed
  startup, serving listeners and usable proxy TLS/MITM certificates, alongside
  the existing local cache and database checks. Suggested by Andy Smith;
  see [readiness and HAProxy setup](docs/configuration.md#readiness-and-load-balancers).
- Optional `cache.trusted_proxies` IP/CIDR allowlist for secure
  `X-Forwarded-For` logging, with right-to-left chain validation and the
  original socket peer retained in `peer_addr`. Supports HTTP, HTTPS, and
  CONNECT. Thanks to @grifferz for requesting this in issue #15.
- Optional admin HTTPS and a shared server-certificate lifecycle for the cache
  and admin listeners, including automatic reload of PEM certificates.
- Automatic ACME issuance and renewal with HTTP-01 or DNS-01, using Lego's full
  automated DNS provider catalogue, private persistent state, durable retry
  deadlines, and certificate health reporting. Based on Laurent Corbes's
  (@lcaflc) PRs #20 and #21; see [server HTTPS setup](docs/server-tls.md).
- Flat repositories (`deb <uri>/ /`, `deb <uri> ./`, `deb <uri> binary/`)
  are tracked, refreshed and adopted like `dists/` suites, and vouch their
  packages for strict mode. Based on Venkat's (@venkatamutyala) PR #8.
- Unsigned repositories are adopted as unsigned snapshots, never trusted
  (`adoption.adopt_unsigned`); Release-less flat repositories are refreshed
  file by file; SHA512-only `Release` files adopt.
- Eager first adoption: a new repository is adopted at its first
  `apt update` through the cache.
- Retry discipline: failed adoptions back off from 2 minutes to an hour;
  refusals are told apart from failures; checks run on four workers with
  per-host fairness.
- Flip consistency: a new snapshot waits for clients that hold the old
  `InRelease` (`adoption.flip_quiet_period`).
- Suite lifecycle: an hourly reaper deletes suites no client has used for
  `gc.suite_idle_ttl` (30 days), and `POST /forget` reaps one suite at once.
- Strict mode (`integrity.refuse_unvouched_debs`) covers flat repositories
  and waits up to 15 seconds for a signed adoption in flight.
- The signer downgrade guard: adoption refuses a new `InRelease` or
  `Release.gpg` that has lost the strong signer its suite's current one
  has (a key Debian 13, Ubuntu 24.04 and 26.04 all accept), the shape of
  an on-path strip; reason `signer_downgrade`, counted by
  `acu_adoption_refused_total{reason}`, and the current snapshot keeps
  serving. If the vendor rotated to a key the cache lacks, install it;
  otherwise confirm with the vendor first (docs/configuration.md, *Trust
  and signer pins*).
- New configuration keys: `adoption.adopt_unsigned`,
  `adoption.flip_quiet_period`, `adoption.max_duration`,
  `freshness.max_suites` (with a per-host cap) and `gc.suite_idle_ttl`.
- New status-page and JSON fields, log events and metrics.
- The counter `acu_gc_displaced_package_rows_deleted_total` and the
  `displaced_package_rows_deleted` field of `gc_run_complete` and
  `gc_retired_run_complete`: displaced snapshots' package-list rows GC
  deleted (issue #14).
- GC failures and work are visible (issue #14): the counters
  `acu_gc_pass_failures_total{pass}` and
  `acu_gc_representation_rows_pruned_total`, `gc_run_complete`'s
  `representation_rows_pruned` and `representation_prune_ms`, and the status
  JSON's `gc.last_run_failed_passes`, `gc.last_run_displaced_package_rows_deleted`
  and `gc.last_run_representation_rows_pruned` (GC panel rows); a failed pass
  turns the GC vital crit (DEGRADED). Alert:
  `increase(acu_gc_pass_failures_total[1d]) > 0`.
- Tests: `make e2e-flat` (real apt clients on six Debian and Ubuntu images,
  in CI) and `make e2e-live` (a manual smoke against live vendors).
- `make e2e-apt-oracle` and `make e2e-apt-oracle-old`: check that the real
  apt of six client images, and of five older ones (Debian 10, Ubuntu
  20.04, 18.04, 16.04 and 14.04), still gives the recorded verdicts and
  error classes on the verifier's 904 signature-shape fixtures (manual;
  run before every release).

### Changed

- Snapshot adoption reuses transaction-scoped prepared INSERT statements for
  members, skipped members and package hashes, reducing commit time for large
  repositories without changing validation or transaction boundaries. Thanks
  to Scott Hyndman (@shyndman) for PR #28 (issue #27).
- HTTP, HTTPS and admin ports bind after startup preparation, including
  certificate setup, database migration, the trust audit and startup GC.
  A lifetime cache-directory lock prevents concurrent daemon maintenance;
  bind failures now surface after preparation. ACME HTTP-01 still opens its
  dedicated challenge port early. Suggested by Andy Smith.
- The systemd service allows six minutes for shutdown and signals the daemon
  before its DNS worker, allowing bounded record cleanup after listeners stop.
- The package upgrade restarts a running daemon.
- The database migrates one way, from 1.0.x's schema 6 to schema 9, and a
  trust audit checks the snapshots 1.0.x adopted at the first start.
- `POST /reconcile` requires the header `X-ACU-Admin: 1` and a local `Host`
  (or `admin.htpasswd_file`), and answers 400 for bad input.
- `adoption.require_signature = false` no longer adopts unsigned
  repositories; `adoption.adopt_unsigned` does.
- A database error creating a suite's row now fails the request (502).
- Six 1.0.x log messages are replaced by snake_case events.
- A signature from a key the cache knows that fails verification is now
  reported as `bad_signature` (it was `crypto_verify_failed`) in the status
  page's adoption reasons.
- An anchor refused for its shape is reported as `malformed_clearsign`
  (an `InRelease`) or the new `malformed_signature` (a `Release.gpg`),
  each naming the rule in its log line, where it was
  `crypto_verify_failed`, `untrusted_signer` or `no_usable_signature`:
  among real repositories, pkgs.k8s.io v1.30's (a version 3 signature,
  which Debian 13's apt refuses too) and devuan's binary `Release.gpg`.
- A reconcile decision that fails on a snapshot (a stored anchor apt's
  text layer refuses, say) warns once, not at every check;
  `acu_adoption_reconcile_failed_total{stage}` counts every failure.
- GC deletes displaced snapshots' package lists (their `package_hash`
  rows), 10,000 rows a transaction, at its next run that reaches them (a
  start, the periodic run every `gc.interval`, or the hourly retired run),
  instead of keeping `gc.keep_displaced` snapshots' worth per suite;
  within the hour a suite that adopts several times an hour still holds
  those it displaced since the last run. `gc.keep_displaced` now keeps
  displaced snapshots' headers and index files, not their package lists.
  The database file keeps its size (SQLite reuses the pages); an offline
  `VACUUM` is optional. A failing GC sub-job no longer stops the ones after
  it (issue #14).
- When `gc.interval` is longer than an hour, the retired run (which
  deletes retired snapshots and strips displaced snapshots' package lists)
  now runs every hour, not only while it has a backlog, so a cache whose
  upstreams update several times a day sheds those rows within the hour
  instead of at the next daily run. On a quiet cache a run finds nothing
  and costs two small queries; a GC sub-job that fails at every call now
  logs `gc_tick_failed` (and counts in `acu_gc_pass_failures_total`) once
  an hour, not once a day (issue #14).
- Built with Go 1.27; `golang.org/x/net`, `x/text`, `x/crypto`, `x/sys`,
  `github.com/cloudflare/circl` and `github.com/ProtonMail/go-crypto`
  updated (`govulncheck`: no known vulnerability the code reaches).

### Fixed

- Adopt gzip repository indexes with trailing NUL padding, as served by
  ClickHouse, while retaining concatenated-member support, CRC/size checks,
  the cumulative decompression cap and verification of the complete stored
  bytes. Thanks to Scott Hyndman (@shyndman) for PR #26 and report #25.
- Preserve upstream Last-Modified dates per URL and content, and use content
  ETags for conditional responses to prevent stale date-only responses (#17).
- Refresh mutable repository metadata when snapshot adoption is disabled,
  including Release-only repositories; fence downloads racing a refresh and
  warn when an existing adopted snapshot cannot refresh in this mode (#18).
  Schema 8 migrates stored dates automatically and is forward-only.
- Empty repository indexes served as HTTP 204 now adopt successfully, and
  a client's request for an index file that upstream answers 204 is
  served as an empty file; declared member sizes and hashes are still
  checked, and a 204 during a resumed download discards the partial bytes
  before caching. A 204 for an `InRelease`, `Release` or `Release.gpg`,
  or for a `.deb` or any other package file, stays an upstream error (502,
  nothing cached). A 204 logs `upstream_status` 204 and counts in
  `acu_fetch_total{outcome="no_content"}`. Based on Laurent Corbes's
  (@lcaflc) PR #22.
- Restarting a partial download without a validator no longer consumes an
  extra retry. Failed destination resets stop as cache write errors instead
  of retrying with stale partial bytes.
- An `InRelease` the signature library cannot decode no longer crashes the
  daemon.
- A backward clock step no longer freezes freshness checks.
- Panics in freshness checks, adoptions, the reaper and GC are recovered
  and logged.
- Idle SQLite read connections are kept between requests, so connection
  churn no longer starves the writer.
- A GC pass that fails at every tick no longer keeps the passes after it,
  the blob pass included, from running; each failed pass logs
  `gc_tick_failed`, at startup too, and a startup pass in which one failed
  still logs `gc_run_complete` and sets the GC metrics.
- GC no longer holds every database write for seconds while it deletes old
  snapshots: periodic runs retire the displaced snapshots beyond
  `gc.keep_displaced` and delete them 10,000 rows a transaction.
- Garbage collection frees disk again on a busy cache (issue #14,
  reported by Skibbi (@skibbipl)): a pass that runs out of
  `gc.max_tick_duration` no longer ends the run, so the pass that deletes
  unused files is no longer skipped behind the others; retired snapshots
  left over are deleted hourly, and at every start. A
  run that ends with `deadline_reached=true` is now normal while a
  backlog drains: the status page shows it as a warning (WATCHING), no
  longer DEGRADED.
- A GC pass gives up a database write it cannot start before its deadline,
  so a busy writer no longer pushes GC past `gc.max_tick_duration`
  beyond the one write already under way at the deadline (issue #14).
- The startup GC pass retires the displaced snapshots beyond
  `gc.keep_displaced` and deletes them 10,000 rows a transaction, like a
  periodic run, instead of deleting them in one transaction; a start's
  displaced snapshots now count in `displaced_reaped` and again in
  `retired_reaped` (issue #14).
- Upstream downloads are no longer decompressed in transit (a gzip-encoded
  body skipped the size check, and a resumed one could be corrupted), and
  adoption stops a member's download at the size its `Release` declares.
- With `integrity.refuse_unvouched_debs`, a `.deb` whose vouching snapshot
  is retired, or replaced by one that no longer lists it, while it
  downloads is refused like any unvouched `.deb`, instead of being served
  unchecked.
- Adoption refuses an `InRelease` or `Release.gpg` whose format a
  supported apt rejects, as measured on the 904 shapes of its corpus with
  eleven clients' apt (1.0 to 3.2): a line after the signature, a forged
  armor checksum, damaged armor, a binary `Release.gpg` (the cache
  adopted them and its clients' `apt update` then failed). An older
  client's refusal counts only for a shape no real signer writes; key
  strength is judged only by the signer downgrade guard (see
  docs/configuration.md).
- Adoption refuses a signed anchor that carries a bad signature from a key
  the cache knows beside a good one, as apt does with gpgv, whatever that
  key's usage (Ubuntu 18.04 and older check every key).
- Older clients' first-signature and critical-subpacket rules now hold:
  when an anchor's signatures use different hashes, its first must be by
  a key the cache holds (Ubuntu 16.04's and 14.04's gpgv checks only that
  one), and a critical revocation reason, issuer fingerprint or key block
  subpacket is refused (gpgv 1.4, and for the key block every gpgv before
  2.2.27, refuses it), so an on-path edit of either kind can no longer
  make the cache adopt an anchor those clients then refuse.
- An anchor whose only good signature is hashed with SHA-1 is no longer
  adopted (reason `crypto_verify_failed`): a SHA-1 signature never
  counts, as every apt from 1.6 on (Ubuntu 18.04, Debian 10 and later)
  refuses it.
- A crafted signature packet can no longer make the daemon allocate
  gigabytes and be killed: the verifier reads each packet itself before
  the signature library sees it.
- Key files are read as the apt clients read them, every block and the
  keys before junk (where clients differ, the cache reads what any of
  them reads: docs/open-decisions.md), a key's copies merged, files up to
  256 MiB (`debian-keyring.gpg`); a revocation or expiry the cache cannot
  read stops that key from counting, and `keyring_key_marked` names it at
  start; a skipped or truncated file logs `keyring_file_skipped` or
  `keyring_file_truncated`.
