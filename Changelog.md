# Changelog

A brief list of the changes in each release. Add an entry here with each
change; the release notes for a release (in `docs/release-notes/`) give
the detail an operator needs before upgrading.

## Unreleased: 1.1.0

Collected until 1.1.0-rc1. The detail, and what to do before upgrading
from 1.0.x, is in [docs/release-notes/1.1.0.md](docs/release-notes/1.1.0.md).

### Added

- Proxy-port `GET`/`HEAD /healthz` for load balancers, also available with
  admin disabled. Both proxy and admin health checks now require completed
  startup, serving listeners and usable proxy TLS/MITM certificates, alongside
  the existing local cache and database checks. Suggested by Andy Smith;
  see [readiness and HAProxy setup](docs/configuration.md#readiness-and-load-balancers).

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
- New configuration keys: `adoption.adopt_unsigned`,
  `adoption.flip_quiet_period`, `adoption.max_duration`,
  `freshness.max_suites` (with a per-host cap) and `gc.suite_idle_ttl`.
- New status-page and JSON fields, log events and metrics.
- Tests: `make e2e-flat` (real apt clients on six Debian and Ubuntu images,
  in CI) and `make e2e-live` (a manual smoke against live vendors).

### Changed

- HTTP, HTTPS and admin ports bind after startup preparation, including
  certificate setup, database migration, the trust audit and startup GC.
  A lifetime cache-directory lock prevents concurrent daemon maintenance;
  bind failures now surface after preparation. ACME HTTP-01 still opens its
  dedicated challenge port early. Suggested by Andy Smith.

- The systemd service allows six minutes for shutdown and signals the daemon
  before its DNS worker, allowing bounded record cleanup after listeners stop.
- The package upgrade restarts a running daemon.
- The database migrates one way (schema 6 to 7), and a trust audit checks
  the snapshots 1.0.x adopted at the first start.
- `POST /reconcile` requires the header `X-ACU-Admin: 1` and a local `Host`
  (or `admin.htpasswd_file`), and answers 400 for bad input.
- `adoption.require_signature = false` no longer adopts unsigned
  repositories; `adoption.adopt_unsigned` does.
- A database error creating a suite's row now fails the request (502).
- Six 1.0.x log messages are replaced by snake_case events.
- Built with Go 1.27; `golang.org/x/net`, `x/text`, `x/crypto`, `x/sys` and
  `github.com/cloudflare/circl` updated (`govulncheck`: no known
  vulnerability the code reaches).

### Fixed

- Empty repository indexes served as HTTP 204 now adopt successfully.
  A 204 during a resumed download discards the partial bytes before caching;
  declared member sizes and hashes are still checked. Based on Laurent Corbes's
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
