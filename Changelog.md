# Changelog

A brief list of the changes in each release. Add an entry here with each
change; the release notes for a release (in `docs/release-notes/`) give
the detail an operator needs before upgrading.

## 1.1.0-rc2

Second opt-in candidate for 1.1.0; stable users remain on 1.0.5. See the
[RC2 release and upgrade notes](docs/release-notes/1.1.0-rc2.md).

- Faster release detection and publication: maintenance yields to updates,
  downloads run concurrently, transient mirror failures retry sooner, and busy
  suites no longer wait a full minute before the publication barrier holds.
- Background refresh defaults to one minute and prepublication warming to
  15 seconds. Existing explicit settings retain their values; edit them to
  receive the shorter delays. Metadata requests can wait up to five seconds
  for revalidation.
- INFO logs identify delayed checks/adoptions, active phases, owners and waits,
  and distinguish the last upstream check from pending unpublished metadata.
- Hot-package history and verified-content reuse improve prefetch behavior.
  **The database migrates to schema 10; rollback to RC1 or 1.0.x requires a
  pre-upgrade backup.**
- Recover misserved plain indexes from verified compressed siblings (#32),
  ship optional firewalld definitions, and keep prerelease download names
  consistent with checksums.
- Record the post-merge adoption benchmarks and the observed slow-commit hold
  expiry, including the successful repeat; see the manual freshness lab.

## Unreleased: 1.1.0

Collects changes throughout the release candidates for the final 1.1.0 release.
The detail, and what to do before upgrading
from 1.0.x, is in [docs/release-notes/1.1.0.md](docs/release-notes/1.1.0.md).
The freshness and publication changes marked "After 1.1.0-rc1" below are
included in 1.1.0-rc2 and planned for the final 1.1.0 release; they are
not included in the published 1.1.0-rc1.

### Added

- After 1.1.0-rc2: logs say what a database write waited behind. Every write
  in the daemon runs on one goroutine, in arrival order, so an adoption's
  commit queues with GC batches and client requests. `adoption_finished` and
  `background_work_finished` now carry `writer_wait_seconds` and
  `writer_ahead` (what the writer ran while the work's writes were queued, by
  kind of write); the progress lines name what the writer is running while a
  write is still queued; and `database_write_waited` reports any write, a
  client request's included, that waited two seconds or more. See
  [waiting for the database writer](docs/log-fields.md#waiting-for-the-database-writer).
- After 1.1.0-rc1: independent background-work diagnostics identify busy
  suite locks, active phases, elapsed time and database/upstream/host/content-lock
  waits at INFO, including before candidate creation and during failure recording.
  Progress logs continue when database heartbeats stall. Pending-change age/count
  and skipped-check counters distinguish "upstream checked" from "new metadata
  published". Busy-check messages and stale warnings are rate limited.
- After 1.1.0-rc1: the manual freshness lab measures multi-suite, multi-host
  bursts and background-only refresh using a signed repository and real APT
  clients. It remains outside the normal test suite. See the
  [measurements and limitations](e2e/freshness-lab/README.md#implementation-and-follow-up-measurements-2026-11-15).

- After 1.1.0-rc1: `freshness.revalidate_wait` (default `5s`): a request for
  a suite's `InRelease` or `Release` asks upstream before it is answered, and
  waits up to that long for the answer and for a new release's adoption, so
  the first `apt update` after an upstream publication can already show it.
  What is not done in time continues in the background and the current
  snapshot is served; `0s` restores serve-first. See
  [checking upstream when a client asks](docs/configuration.md#checking-upstream-when-a-client-asks).
- After 1.1.0-rc1: logs that answer "the update was upstream but not in the
  cache". Each `InRelease`/`Release` request line records the snapshot served,
  how long ago upstream was asked, whether the cache already knew of newer
  content (`behind_s`), and what the request-time check did (`reval`).
  `adoption_finished` records how long the cache had known of the release
  (`first_seen_seconds`) and how old the release was when published
  (`release_age_seconds`). An adoption still running after a minute logs
  `adoption_in_progress` with its phase, every minute.
- Correlated adoption start/finish logs show trigger and retry reasons,
  per-phase and total duration, snapshots, member/package reuse and downloads,
  and actual upstream byte totals, including failed and abandoned attempts.
- Upstream response logs record actual body bytes, purpose, destination, status,
  and completion, including retries and redirects, for traffic accounting.
- Schema 10 remembers client-requested package identities across version changes
  and snapshot/URL reclamation. Prefetch never extends their hot-window timestamp.

- Optional firewalld service definitions shipped in the `.deb`: separate
  `apt-cacher-ultra` (TCP 3142) and `apt-cacher-ultra-admin` (TCP 6789)
  services, with explicit zone activation and manual installation instructions.
  Installation does not enable either service or require firewalld. Adapted
  from @charlescurley's contribution in
  [discussion #31](https://github.com/linsomniac/apt-cacher-ultra/discussions/31);
  see [firewalld setup](docs/configuration.md#firewalld).
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
- The counter `acu_gc_package_list_rows_deleted_total` and the
  `package_list_rows_deleted` field of `gc_run_complete` and
  `gc_retired_run_complete`: the package rows GC deleted, of package lists
  no suite names any more (issue #14; in the release candidates they were
  `acu_gc_displaced_package_rows_deleted_total` and
  `displaced_package_rows_deleted`, and counted displaced snapshots' rows).
- GC failures and work are visible (issue #14): the counters
  `acu_gc_pass_failures_total{pass}` and
  `acu_gc_representation_rows_pruned_total`, `gc_run_complete`'s
  `representation_rows_pruned` and `representation_prune_ms`, and the status
  JSON's `gc.last_run_failed_passes`, `gc.last_run_package_list_rows_deleted`
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

- After 1.1.0-rc2: an adoption writes the package rows a publication
  changed, not the suite's whole package list. **The database migrates to
  schema 11; rollback to RC2, RC1 or 1.0.x requires a pre-upgrade backup.**
  A suite's package rows (the hashes its packages are vouched by) are one
  list per suite, updated in place, instead of a copy per snapshot.
  Committing a typical update of a 67,000-package suite took 0.05 s and
  2 MiB of write-ahead log where it took 2.0 to 2.5 s and 64 MiB (a copy
  of a production database, 241 rows changed), so the one database writer,
  which every other write waits behind, is held for a fraction of the
  time, and garbage collection has no package rows to delete afterwards. A suite's first
  adoption, and a publication that changes more than half of its
  packages, still write the whole list (about 2 s). In the freshness lab
  the commits of six such suites updating at once took 0.2 to 0.4 s
  together where they took 15 s. How much sooner all six are visible is
  not established: 136 to 160 s over four runs against 160 to 180 s over
  three, a difference no larger than the variation between runs. A single
  suite's update is unchanged, since its minute is detection and the
  publication barrier. The migration runs at
  the first start, before the ports open: it copies the current package
  rows into the new tables and drops the old one, with the package lists
  earlier versions kept for displaced snapshots; 5 to 10 s for a database
  of 1.16 million package rows in our measurement, 68 s on a production
  host with suites of the same size. On that host the commit of its
  largest suite took 12 to 24 s before the change (31 adoptions in six
  days) and 0.6 s in the first adoption after it. `adoption_finished`
  says how each commit wrote the list (`package_list_write`, and the rows
  added, changed and removed), `acu_adoption_package_list_writes_total`
  counts them, and two new events report a list that could not be read
  beforehand (WARN `package_list_diff_failed`: the whole list is written
  instead) and a difference that did not produce the publication's rows
  (ERROR `package_list_mismatch`: a defect to report; the list is written
  whole and is exact). `adoption.write_package_list_differences = false`
  (new; default `true`) makes every adoption write the whole list, as
  before: a fallback should that ERROR ever appear. The migration needs
  free disk, about 1.3 KiB per current package row (400 MiB for the
  measured database); see the release notes. See
  [the result](e2e/freshness-lab/README.md#one-package-list-per-suite-the-result-2026-11-19).
- After 1.1.0-rc1: a suite that clients keep asking for no longer waits a
  minute before it switches to a new snapshot. The flip barrier waited up to
  60 s for a pause of `adoption.flip_quiet_period` (10 s) in the suite's
  `InRelease`/`Release` requests; a suite asked for more often than that never
  pauses, so every adoption of it sat the whole 60 s, then held its requests
  anyway, and kept its adoption slot throughout — six such suites queued for
  first adoption took 6m34s to appear in the freshness lab, against 1m04s on
  1.0.5, which has no barrier. The barrier now goes on to hold once one of
  the suite's `InRelease`/`Release` responses has finished during the wait,
  a new request for it has arrived since, and the suite is still not quiet a
  quiet period after that response. A suite nobody asks for meanwhile (slow
  downloads already in progress included), the wait for other suites' holds,
  and the conditions for the switch itself are unchanged. See [Flip
  consistency](docs/configuration.md#flip-consistency).
- After 1.1.0-rc1: `freshness.periodic_refresh` defaults to `"1m"` instead
  of `"15m"`, and `adoption.hot_prefetch_budget` to `"15s"` instead of `"5m"`.
  The warming budget includes hot-set computation. Warming remains best effort
  before publication; unfinished packages are fetched on demand, and operators
  can explicitly choose a longer budget. Existing explicit settings retain their
  values: change them to receive the shorter delays. Startup logs warn about
  settings that retain long delays. These defaults do not promise five-minute
  availability; metadata work, queueing, database writes and the client-coherence
  barrier can add time.
- After 1.1.0-rc1: adoptions publish sooner. Release members and hot packages
  are fetched four at a time instead of one; a file the `Release` declares but
  upstream never serves is asked for once per adoption instead of twice; and
  members caught mid-mirror-sync are retried together, so they cost one
  `member_retry_delay` per round rather than one each.
- After 1.1.0-rc1: a failed adoption whose cause is a member that did not
  arrive or did not match (`member_fetch_failed`, `member_mismatch`: a mirror
  mid-sync) is retried after 1 minute doubling to 10, not 2 minutes doubling
  to an hour, and a new upstream publication starts that schedule again
  (content that merely differs at every attempt does not); a suite that
  still fails after two hours returns to hourly retries. The failure count,
  and the persistent-failure warning that reads it, carry across content as
  before. Other failures and all refusals keep their schedule.
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
- The database migrates one way, from 1.0.x's schema 6 to schema 11, and a
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
- Displaced snapshots keep no package lists: `gc.keep_displaced` keeps
  their headers and index files. A suite has one package list (the first
  entry of this section); when an adoption replaces it whole, or the suite
  is removed, GC deletes the old list, 10,000 rows a transaction, at its
  next run (a start, the periodic run every `gc.interval`, or the hourly
  retired run). The database file keeps its size (SQLite reuses the
  pages); an offline `VACUUM` is optional. A failing GC sub-job no longer
  stops the ones after it (issue #14).
- The startup GC pass no longer holds the listeners closed: the daemon
  serves as soon as the pool orphan scan (and, at the first 1.1.0 start,
  the migration and trust audit) is done, and runs the GC pass in the
  background. A start with a GC backlog no longer waits up to
  `gc.max_tick_duration` before serving (a 102-second wait on a test cache
  whose first start deleted the package lists earlier versions kept for
  displaced snapshots; the schema-11 migration now drops those with the
  old table).
- When `gc.interval` is longer than an hour, the retired run (which
  deletes retired snapshots and the package lists no suite names any more)
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

- After 1.1.0-rc2: an adoption no longer reads a `Packages.xz` or
  `Sources.xz` with one system call per byte. The xz decoder was handed
  the cached file without a buffer: 1.47 million reads for a 1.47 MB
  `Packages.xz`. On a production host this was most of the
  `parse_indexes` phase, 38 to 40 s of a 55 to 78 s adoption for a suite
  of 66,000 packages. The file is read through a buffer now (0.20 s where
  it took 0.72 s for that file on a development machine; not yet measured
  on that host). Rebuilding a plain `Packages` or `Sources` from its `.xz`
  sibling had the same fault and is fixed with it.
- After 1.1.0-rc2, ACU can adopt the Caddy repository on Cloudsmith.
  Empty components previously caused adoption to report `parse_failed`. ACU
  accepts a zero-byte `Packages.gz` only when it also verifies a zero-byte
  `Packages` in the same directory of the same release.
- After 1.1.0-rc1: release checks can run beside automatic repair or manual
  reconciliation instead of waiting for maintenance to finish. Unchanged
  metadata lets a slow repair continue across refresh intervals; changed or
  inconclusive metadata yields maintenance to the ordinary check. Adoptions
  can also reclaim its admission slots (`repair_yielded`). One additional
  metadata-probe slot per host prevents bulk downloads from starving detection.
  This also avoids repeatedly cancelling an unchanged repair at the new
  one-minute refresh cadence.
- After 1.1.0-rc1: `upstream.idle_read_timeout` is enforced. It was accepted
  but ignored, so an upstream that went silent mid-response held a fetch — and
  an adoption or repair behind it — until `total_timeout` (5 minutes). A
  fetch that meets silence is retried once, whatever `max_retries` allows; a
  second silence ends it. What a release can be published without gets no
  retry after a silence: a hot-package prefetch ends, and an optional file is
  skipped and left to repair, so either holds a release back one idle timeout
  at most.
- Hot prefetch verifies and reuses cached package content instead of downloading
  unchanged packages on every snapshot update. Missing or corrupt content is
  fetched and verified; concurrent prefetches of one hash avoid duplicate downloads.

- After 1.1.0-rc1: recover uncompressed `Packages`/`Sources` indexes when a
  repository serves the wrong representation (Raspberry Pi's Apache content
  negotiation in #32). Reconstruct from a declared gzip/xz sibling, verifying
  both representations' sizes and hashes with bounded decompression. Adoption,
  reconciliation and missing-blob recovery retain the selected snapshot's
  contents; compressed bytes are never served under the plain index's hash.
- Serve snapshot-consistent `Release` and `Release.gpg` files for repositories
  adopted from `InRelease`. Debian installer mirror checks now work with existing
  inline snapshots, including offline, without clearing the cache or re-adopting.
  The original signatures and metadata are extracted together; newer upstream
  sidecars cannot be mixed with cached indexes. Thanks to @charlescurley for #30.
- Require declared source indexes when `adoption.architectures` is empty,
  and reconcile missing source indexes in older snapshots. A transient upstream
  `404`/`410` can no longer publish a snapshot missing every full Sources variant
  under the default configuration. Explicit binary-only lists remain supported;
  source-index requests they exclude now log a configuration hint. Added regression
  coverage for interrupted same-size metadata replacements and retry after restart.
  Thanks to @charlescurley for issue #29.
- Adopt gzip repository indexes with trailing NUL padding, as served by
  ClickHouse, while retaining concatenated-member support, CRC/size checks,
  the cumulative decompression cap and verification of the complete stored
  bytes. Thanks to Scott Hyndman (@shyndman) for PR #26 and report #25.
- Preserve upstream Last-Modified dates per URL and content, and use content
  ETags for conditional responses to prevent stale date-only responses (#17).
- Refresh mutable repository metadata when snapshot adoption is disabled,
  including Release-only repositories; fence downloads racing a refresh and
  automatically forget previously adopted suites at startup before serving
  in this mode (#18). Their metadata is fetched afresh and their snapshots
  are retired for normal GC. Added daemon restart coverage for the transition,
  previously excluded indexes, subsequent refreshes and cleanup failures.
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
