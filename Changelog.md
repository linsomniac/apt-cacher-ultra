# Changelog

A brief list of the changes in each release. Add an entry here with each
change; the release notes for a release (in `docs/release-notes/`) give
the detail an operator needs before upgrading.

## Unreleased: 1.1.0

Collected until 1.1.0-rc1. The detail, and what to do before upgrading
from 1.0.x, is in [docs/release-notes/1.1.0.md](docs/release-notes/1.1.0.md).

### Added

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

- An `InRelease` the signature library cannot decode no longer crashes the
  daemon.
- A backward clock step no longer freezes freshness checks.
- Panics in freshness checks, adoptions, the reaper and GC are recovered
  and logged.
- Idle SQLite read connections are kept between requests, so connection
  churn no longer starves the writer.
