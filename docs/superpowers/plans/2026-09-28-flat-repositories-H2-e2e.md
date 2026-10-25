# Flat repositories — Phase H2: real-apt end-to-end tests (`e2e/flat`) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `make e2e-flat` drives real apt clients — apt 2.2.4, 2.4.14, 2.8.3, 2.6.1, 3.0.3 and 3.2.0 on the six client images of spec §7 (`debian:11`, `ubuntu:22.04`, `ubuntu:24.04`, `debian:12`, `debian:13`, `ubuntu:26.04`) — against fixture copies of every repository shape spec §3 names (k8s, CUDA, libnvidia-container, Jenkins, Sublime Text, Jitsi, two unsigned variants, two Release-less ones, a `dists/` repository) through apt-cacher-ultra, and asserts spec §7's list: installs succeed, apt's exact request lines (`./`, `%253a`, `by-hash/SHA512`), clean `apt-get` output, strict mode on the signed host, publishes reaching clients within one cooldown, an older `Date` ignored, state across a restart, a 45 s commit that times out no client, the unsigned first adoption after its 5-minute confirmation gap, PR #8's end-to-end table and the phase G Release-less carry-forward; CI runs it per image. Task 12 (the controller's addition) also makes CI's `deb-test` green again: e2e/deb's phases 2–4 still assumed no adoption at first contact.

**Architecture:** e2e/deb's single-container pattern: one image per client image holds nginx (the upstream), the daemon and the apt clients. A `fixtures` build stage builds every repository in two versions, signed with a key generated there (ruling R-H2-1), and links each repository's URL path to a `current -> v1` symlink that `test.sh` flips; nginx serves them under two host names, `signed.test` (the signed repositories) and `plain.test` (the unsigned and Release-less ones), which `/etc/hosts` points at `127.0.0.1`. The daemon is built with `-tags e2ehooks`, which compiles in one test seam — `ACU_E2E_COMMIT_DELAY` sets the adopter's existing commit-delay hook — and runs with short timers (ruling R-H2-2). Each apt "client" is its own state directory selected with `APT_CONFIG`. Assertions read apt's output and its `Debug::Acquire::http` request lines, nginx's access log, the daemon's snake_case log events, the admin status JSON and `/metrics`; every wait is a bounded poll on one of them (ruling R-H2-5). `e2e/flat/run.sh` loops over the images (`E2E_FLAT_IMAGES`); CI runs one image per matrix job (ruling R-H2-3).

**Tech Stack:** Bash 5 (`set -euo pipefail`, shellcheck 0.11 clean), Docker with BuildKit, nginx-light, `apt-ftparchive`/`dpkg-deb`/`dpkg-source`/`gpg` in the fixture stage, `curl` and `jq` in the test image, Go 1.25 for the one test seam.

**Spec:** `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (revision 11) — §7's "Real-apt end-to-end" paragraph, item by item, is this plan's requirement; §3 (apt's behaviour and the real flat repositories) gives the fixture shapes; §9's H row is the definition of done; C2 (flat forms, twins, the host-root limitation), C4 (cooldown, `periodic_refresh`, Dispatch 3–4: the anchorless refresh), C5 (the `Date` rules, the unsigned confirmation, SHA512-only Releases), C6 (vouching every Filename shape), C8 (the flip barrier and its phase-B-expiry residual) and C10 (strict mode) are what the scenarios exercise. Overview: `docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md` — the H line under "Cross-phase interfaces" (PR #8's end-to-end table), the *(G)* carry-forward "`e2e/flat` gains a Release-less repository" and "PR #8 parity" (PR #8 verified live; this e2e uses fixtures only, and H3 runs the live smoke). Phase H is three plans run in order: H1 (the sweep: `…-H1-sweep.md`), **H2 (this plan)**, H3 (README, release notes, the live-upstream smoke, the final full-diff review).

## Global Constraints

Everything in the overview's Global Constraints that touches this plan applies. In particular:

- **Fixtures only.** No scenario reaches a live repository. The test container's only upstream is its own nginx; the daemon's `upstream.allowed_host_regex` admits `signed.test` and `plain.test` alone. CI's network use is the base images (Docker Hub: `golang:1.25-bookworm`, `debian:bookworm-slim` and the client image), the distribution mirrors the image build installs from (`deb.debian.org`, `archive.debian.org`, `archive.ubuntu.com`) and the Go module proxy the build stage's `go mod download` uses, as e2e/deb's job already does (design note 13).
- **Spec values kept** (ruling R-H2-2): the unsigned confirmation gap `max(cooldown, 5m)` = 5 minutes (1 hour after a 401/403 on a signature file), the 45 s commit delay, the flip hold's 20 s bound below apt's 30 s timeout, `flip_quiet_period` inside its `1s`–`15s` range, the six client images.
- **No production change.** The only Go change is the `e2ehooks` seam (Task 1), compiled only with `-tags e2ehooks`; a release build (`make build`, `make deb`, nfpm, e2e/deb) contains its no-op twin and reads no `ACU_E2E_*` variable (`TestBuildAdopter_ReleaseBinaryIgnoresTheE2ECommitDelay`).
- **Assert on stable names.** Log assertions use only snake_case events (`request`, `freshness_check`, `adoption_success`, `adoption_refused`, `adoption_flip_barrier`, `unvouched_deb_refused`, `e2e_commit_delay_enabled`) and fields, the admin status JSON (`suites[]`, a locked contract) and `/metrics`. No assertion reads a prose message, so H1's snake_case renames (its ruling R-H3) cannot break this harness, which runs after H1.
- After the Go change (Task 1): `go test -race ./...` and `golangci-lint run ./...` are clean, `golangci-lint run --build-tags e2ehooks ./cmd/...` too, and `gofmt -l .` lists only the five known drift files (`cmd/apt-cacher-ultra/ca_print.go`, `internal/cache/cache_test.go`, `internal/cache/gc_urlpath_test.go`, `internal/freshness/hot_prefetch_test.go`, `internal/proxy/connect_stats.go`). Never run `gofmt -w` on those five.
- Every shell file in `e2e/flat/` is bash with `set -euo pipefail` (`lib.sh` is sourced by `test.sh`, which sets it), and `shellcheck -x e2e/flat/*.sh` (run from the repo root) prints nothing. No bare `sleep` stands in for a condition (ruling R-H2-5); the sleeps that remain pace a poll or a client loop (`poll`'s one second, `until_candidates`' cooldown, the commit-delay loop's 3 s).
- Commit trailer: `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

Conventions in this plan:

- Shell and Dockerfile contents are written out in full: each task's files are the final ones, exactly as the planning runs executed them (design note 15). A task that extends `test.sh` gives the function bodies it adds and the one-line edit to the run list at the end of the file.
- "Run" commands are run from the repo root. Expected outputs quote the planning runs; times vary with the machine, so an expected time is a range, never an assertion.
- Each image build needs network access to Docker Hub and the distribution mirrors; a build failure that names a mirror (`Failed to fetch http://deb.debian.org/…`, `404  Not Found`) is the mirror, not the task: retry once before debugging (design note 9).

## Rulings (controller, binding)

- **R-H2-1 — test keys.** The fixtures' signing key is generated in the `fixtures` build stage (`gpg --quick-generate-key … rsa3072 sign 0`, an empty passphrase, a throwaway `GNUPGHOME`), exported to `/fixtures/keys/acu-e2e.gpg` with its fingerprint, and never leaves the image; no key is committed. Generation takes under a second, so committing a key was never needed.
- **R-H2-2 — time.** Real timers, short intervals: `freshness.cooldown = "2s"`, `periodic_refresh = "20s"` (a 5 s scheduler tick), `adoption.flip_quiet_period = "1s"`. Fixed spec values are kept (Global Constraints): the unsigned first adoption runs only on the oldest and newest images (`debian:11`, `ubuntu:26.04`, `E2E_FLAT_UNSIGNED_IMAGES`), where it overlaps the other scenarios (design note 7); the 45 s commit delay runs everywhere. Measured wall times are in Task 9.
- **R-H2-3 — CI.** One matrix job per client image, `fail-fast: false`, each running `E2E_FLAT_IMAGES=<image> bash e2e/flat/run.sh`; `timeout-minutes` 15 for the four short images and 30 for the two that wait for the unsigned confirmation (Task 10; reasons in design note 13). Controller ruling on the triggers: the same as `deb-test` — pushes to `main` and `master`, and every pull request (the workflow's own `on:`). Nightly-only would find regressions later and needs someone to watch a nightly run: more maintenance, not less.
- **R-H2-4 — what the e2e finds.** The planning runs found no daemon bug: every scenario passed on all six images against the daemon at 6786529 (Task 9). The failures met on the way were the harness's own and are fixed in the files this plan gives (Task 9 lists them); the unsigned confirmation's timing is the spec's behaviour (R-H2-6). So this plan has no daemon fix task. Task 12 (the controller's addition) fixes another harness, e2e/deb, whose replay also passes with the daemon unchanged.
- **R-H2-6 — the unsigned confirmation lands at a retry** (controller ruling). Spec C5 safeguard 2 asks for two observations "at least" `max(cooldown, 5m)` apart, and its refusal states use the same record and schedule as failures (C7), so the first adoption landing at the record's first retry 5 minutes or more after the first observation — 5 to 15 minutes in practice — is the spec's behaviour, not a defect: no code change. The 955 s deadline stands (design note 7). H3's documentation says "at least 5 minutes; with the retry schedule, usually 5 to 15". An implementer whose run fails a scenario for a daemon reason follows the ruling: a Go regression test that fails first, the fix, the scenario that caught it — never a weakened assertion.
- **R-H2-5 — flake resistance.** Every wait is `poll SECONDS WHAT COMMAND…` (a bounded poll on a log line, an HTTP answer, a status field or an apt result, with a failure message) or `until_candidates LIMIT …` (apt updates once a cooldown until the candidates match, bounded); a failure prints the scenario name, the daemon's log tail (80 lines) and nginx's access-log tail (30 lines).

## Design notes (choices the spec leaves open)

1. **Two host names, one nginx.** `signed.test` serves the seven signed repositories (six flat shapes and `dists/`), so strict mode (`integrity.refuse_unvouched_debs = true`) is on there once they adopt; `plain.test` serves the unsigned and Release-less ones, which never count for strict mode (spec C10), so it is never strict. `.test` is reserved (RFC 6761); `/etc/hosts` maps both to `127.0.0.1`, and the daemon's `deny_target_ranges = []` lets it dial loopback (as e2e/deb's config does). A third shape, the daemon's mirror mode, reaches `signed.test` through the `[[mirror]]` prefix `/m-signed`, so its clients share the proxy-mode suites.
2. **Clients are apt state directories, not containers.** `client NAME PROXY SOURCE-LINE…` writes `/clients/NAME/{sources.list,apt.conf}`; `APT_CONFIG` selects them for `apt-get`, `apt-cache` and `apt-file`. dpkg's database is shared, so every install check verifies the installed file's version and purges the packages again. This keeps the single-container pattern spec §7 names and lets two clients use the two source forms of one repository (PR #8's table) side by side.
3. **The fixture shapes** (spec §3), all versions `1.0` and `2.0`: k8s `deb <uri>/ /`, Filenames `amd64/…`, `InRelease` only, nginx answering 403 for a missing file; CUDA `./…` Filenames (apt-ftparchive's dpkg-scanpackages shape) with `InRelease`, `Release` and `Release.gpg`, plus `acu-epoch` version `1:1.0`/`1:2.0` in a file named `acu-epoch_1%3a<v>_amd64.deb`; libnvidia-container `deb <uri> /`, `InRelease` only, a SHA512-only `Release` text over a `Packages` that keeps SHA256, `Date` frozen at 2018 in both versions; Jenkins `deb <uri> binary/`, detached with an armored `Release.gpg`, Filenames `binary/…` and `direct/…`; Sublime `deb <host-root> apt/stable/`, Filenames `files/…` at the host root, `files/` keeping every build; Jitsi `deb <uri> stable/` plus `deb-src`, `Acquire-By-Hash: yes`, SHA256 and SHA512 only, `Contents-amd64`, `Sources`, a binary `Release.gpg` beside `InRelease` (deviation 1); `dists/` `deb <uri> acu main`, inline; unsigned `Release`-only at `/unsigned404` (missing files 404) and `/unsigned403` (403); Release-less `Packages`/`Packages.gz` at `/fury` (Gemfury: Filenames without `./`) and `/goreleaser` (`./` Filenames). The `sub/` and `a/b/` forms' `Release` files carry the `Suite` apt checks (`binary`, `apt/stable`, `stable`), or apt warns "Conflicting distribution" as no real repository makes it.
4. **Dates and mtimes.** v1 is dated two days before the image build and v2 one day before, both in the past and without `Valid-Until`, so a cached fixture layer stays valid; every file of a version carries its version's mtime, because otherwise v1 and v2 indexes of equal size built in the same second share nginx's ETag and `Last-Modified`, and a conditional GET would be answered 304.
5. **"Within one cooldown"** (spec §7, PR #8's table) is read as: a client that runs `apt-get update` once a cooldown sees the new version within `REACH` = cooldown + 20 s of the publish. The first update after the publish fires the check (once the cooldown has passed), the adoption and the flip barrier (phase A's 1 s quiet after the client's last anchor, a zero-length hold) follow, and a later update sees the new snapshot. Measured on the planning runs: 2–6 s. The anchorless refresh's own carry-forward bound, "within one `periodic_refresh`", is checked with no client at all: `periodic_refresh` + one tick + 10 s (measured 20–22 s).
6. **Observables.** The admin status JSON (`/?format=json`, `suites[]`: `current_snapshot_id`, `anchor_form`, `adoption_fail_reason`) for adoption state; text-log events for what happened (`adoption_refused reason=upstream_older`, `adoption_flip_barrier outcome=…`, `request … validated_hash=true`, `flip_held_ms`); `/metrics` for `acu_anchorless_sniff_rejected_total{host="plain.test"}`; nginx's access log for the request lines the cache sent upstream; apt's `Debug::Acquire::http` output for the lines apt sent (CRLF-terminated, so `apt_sent` strips CRs). No assertion reads the SQLite file.
7. **The unsigned scenario overlaps the run, and lands at a retry.** `/unsigned404`'s first contact is the first update of `first-contact`; its adoption is asserted after `commit-delay` (about 2 minutes in). The confirming observation is not the first check 5 minutes after the first one: checks before the `unsigned_unconfirmed` record's retry time see the same bytes and stay `changed_deferred` (spec C7's gate: `checkFirst`'s `deferred := gate.active && !gate.due && req.key == gate.hash`, `internal/freshness/first_adoption.go`, before `unsignedFirst` runs), and the retries come 2, 4 and 8 minutes apart, each ±25 %. So the adoption lands at the first retry at least 300 s after first contact: before 450 s when the second retry is that late, else at the third, before 900 s. The deadline is first contact + 900 s + `periodic_refresh` + one tick + 30 s (955 s). Planning runs: 310 s and 397 s (`debian:11`), 373 s (`ubuntu:26.04`); an earlier draft whose deadline assumed 5 minutes timed out at 355 s on `ubuntu:26.04`. The record survives the scenario's restarts (the confirmation stamp is persisted). Running the scenario after `upstream-down` instead would cost up to two more minutes: that scenario's refused connections put the suite in the failing class (a 2-minute backoff).
8. **Strict mode's lower bound.** "Zero `unvouched_deb_refused` on the signed host" would pass vacuously with strict mode off, so `first-contact` requests one undeclared `.deb` (`acu-rogue`, on the upstream but in no `Packages`) and requires a 502 and its refusal line; `whole-run` excludes exactly that path and also requires zero refusals on `plain.test`.
9. **`debian:11` is past its LTS** (ended 2026-08-31): its packages moved to `archive.debian.org`, and the image still names `deb.debian.org`, whose security pool now answers 404. The test stage rewrites `sources.list` for `bullseye` before installing nginx and the tools; apt itself (2.2.4, the one under test) is the image's. Images that reach end of life later need the same one-line treatment (README, "Maintenance").
10. **Two Release-less repositories.** `/goreleaser` is republished while a client updates once a cooldown (the request-triggered path, "within one cooldown"); `/fury` is republished with no client (the scheduler path, "within one `periodic_refresh`") and then put behind a captive portal (`/run/acu-e2e/captive` makes nginx answer an HTML page with 200 for every `/fury` path).
11. **The captive portal's aftermath.** A client update during the outage caches the portal's pages as `/fury`'s `InRelease` and `Release` (a miss stores what upstream answers; apt reports `Signed file isn't valid, got 'NODATA'` for the `Release`, as it would fetching directly). After the outage the first update still gets those pages, and the check it triggers deletes them (spec C4 Dispatch 4: an anchor answered 404); the harness polls, bounded by `REACH`, until an update shows no error for `/fury`. Not a daemon bug: there is no synchronous revalidation (spec §2, non-goals). Listed for H3's documentation.
12. **The commit-delay seam is a build tag.** `cmd/apt-cacher-ultra/e2ehooks.go` (`//go:build e2ehooks`) reads `ACU_E2E_COMMIT_DELAY` and calls the adopter's existing `SetCommitDelayForTest`; `e2ehooks_off.go` (`//go:build !e2ehooks`) is a no-op. The e2e image is built with the tag, every release path without it. The seam sleeps before the commit transaction, inside the barrier, as the flip simulations' slow commit does; the scenario restarts the daemon with the variable set and again without it.
13. **CI: one job per image** (ruling R-H2-3). The point of `e2e/flat` is apt-version coverage, which a subset would drop for most images; one image per job keeps each log readable and each timeout tight, and parallel jobs keep the wall time near the slowest image (the two with the unsigned wait). A short image measured about 1 minute of cold build and 2 minutes of test on the planning machine, so 15 minutes leaves room for a slower runner and its image pulls; a long-wait image can take up to 955 s for the unsigned adoption alone (design note 7) plus the rest, so it gets 30. Every input is a base image or a distribution mirror, plus the Go module proxy, which the controller ruled acceptable: `deb-test` already depends on it, and it is not a live apt upstream, which is what the "fixtures only" rule protects. A mirror outage fails one job, which is re-run alone. No automatic retry: a retry would hide a flaky scenario. A subset in CI with the full matrix only local was the alternative; it was rejected because apt-version differences are exactly what a local-only run lets drift.
14. **`apt-file` is in every test image**, so apt requests the `Contents-*` targets of every repository whose `Release` lists them (Jitsi's, by hash) and probes them, with Translations, for the Release-less ones, as on a host with apt-file installed.
15. **The planning runs** executed every file in this plan on all six images (Task 9 has the timings), in a scratch copy of the tree at 6786529, with the image and container names prefixed `planh2-` (`E2E_FLAT_NAME=planh2-e2e-flat`).
16. **`.dockerignore`** gains `/apt-cacher-ultra` (a local build output, 24 MB) and `.claude/` (Claude Code worktrees: 220 MB in the main checkout), which every `docker build` from the repo root otherwise sends as context; no Dockerfile reads either.

## Deliberate deviations from spec §7

1. **Jitsi's binary `Release.gpg` is never fetched by apt.** apt refuses a binary detached signature. The planning runs served an `InRelease`-less copy of the Jitsi fixture straight from nginx to each image's apt: 2.2.4, 2.4.14, 2.6.1 and 2.8.3 printed `W: GPG error: http://signed.test/jitsi-detached stable/ Release: Detached signature file '…_Release.gpg' is in unsupported binary format` and `E: The repository 'http://signed.test/jitsi-detached stable/ Release' is not signed.`; 3.2.0 the same after `W: OpenPGP signature verification failed:`; 3.0.3 `E: OpenPGP signature verification failed: … Signed file isn't valid, got 'NODATA' … Detached signature file '…' is in unsupported binary format`. So a detached-only binary repository cannot be exercised with real apt at all. The fixture serves `InRelease`, `Release` and the binary `Release.gpg`, and apt uses the `InRelease` (`first-contact` fails if apt fetches the `Release.gpg`). The cache's side of a binary signature — the sniff and a detached adoption — is pinned in Go (`TestSniffReleaseGPG`'s binary cases; the planning run also saw the detached-only fixture adopt, `adoption_success … suite_path=/jitsi/stable form=detached`).
2. **The 403 unsigned variant is not adopted in the run.** Its first adoption needs two observations an hour apart (spec C5 safeguard 2, a fixed constant), past any bounded run. The e2e checks what it can: its package installs through the cache, it waits `unsigned_unconfirmed` (`first-contact`), and on the long-wait images it is still unadopted after the 404 variant adopted (`unsigned-first-adoption`). The hour itself is pinned in Go (`TestCheckFirst_UnsignedAfterADeniedSignatureWaitsAnHour`).

## Review Focus

The failure modes this harness could hide, most likely first, each pinned in the owning task:

1. **A grep that matches nothing on some apt version** would pass every update vacuously. Task 3's `first-contact` runs one update against a source the cache refuses (`http://nowhere.test/`, outside the allowlist) and requires the error grep to match its output, on every image.
2. **A publish adopted by the scheduler before the client asks** (a 5 s tick races the client's cooldown) must not fail a "within one cooldown" check: `until_candidates` accepts whichever check adopted, and the bound is on what the client sees (Tasks 4–6).
3. **A strict mode that is silently off** would make "zero refusals" vacuous: the `acu-rogue` refusal (design note 8, Task 3).
4. **A request line normalised on the way** (curl drops `/./` unless given `--path-as-is`, as the planning run's first probe showed; a `%` decoded twice): the `./`, `%253a` and `by-hash/SHA512` lines are pinned from apt's own debug output, and the `./` and `%253a` ones also in nginx's access log, the line the cache sent upstream; the harness curls no `./` path (Tasks 3–4).
5. **An end-of-life image whose mirror moved** fails the image build, not a scenario: design note 9's rewrite for `bullseye`, and the README's maintenance note (Tasks 2, 11).

## File map

| Area | Files | Task |
|---|---|---|
| The commit-delay seam | `cmd/apt-cacher-ultra/e2ehooks.go`, `e2ehooks_off.go`, `e2ehooks_test.go`, `e2ehooks_off_test.go` (new); `cmd/apt-cacher-ultra/main.go` (`buildAdopter`'s one call) | 1 |
| Fixtures and the image | `e2e/flat/build-fixtures.sh`, `e2e/flat/nginx.conf`, `e2e/flat/Dockerfile` (new); `.dockerignore` | 2 |
| The harness and first contact | `e2e/flat/lib.sh`, `e2e/flat/test.sh`, `e2e/flat/run.sh` (new); `Makefile` (`e2e-flat`) | 3 |
| PR #8's table, the older `Date`, the restart | `e2e/flat/test.sh` | 4 |
| The frozen `Date` and the Release-less carry-forward | `e2e/flat/test.sh` | 5 |
| Two suites on one host, mirror mode | `e2e/flat/test.sh` | 6 |
| The 45 s commit | `e2e/flat/test.sh` | 7 |
| The unsigned first adoption, whole-run assertions | `e2e/flat/test.sh` | 8 |
| Definition of done: six images, timings | none (runs and records) | 9 |
| CI | `.github/workflows/ci.yaml` | 10 |
| Documentation | `e2e/flat/README.md` (new), `Makefile` (help comment), the overview | 11 |
| e2e/deb after eager first adoption | `e2e/deb/test.sh` (phases 2–4, two helpers) | 12 |

No migration, no new Go package, no config key, no production code path.

## Tasks

1. The commit-delay seam in an `e2ehooks` test binary
2. The fixture repositories, nginx and the test image
3. The harness, `run.sh`, `make e2e-flat` and the first-contact scenario
4. PR #8's end-to-end table, the older `Date` and the restart
5. The frozen `Date` and the Release-less repositories (the phase G carry-forward)
6. Two suites flipping on one host, and mirror mode
7. The 45 s commit
8. The unsigned first adoption and the whole-run assertions
9. Definition of done: all six images, measured
10. CI: one `e2e-flat` job per image
11. Documentation: `e2e/flat/README.md`, the Makefile help, the overview, and the list for H3
12. e2e/deb after eager first adoption (CI's `deb-test`)

---

### Task 1: The commit-delay seam in an `e2ehooks` test binary

Spec §7: "A commit-delay hook of 45 s asserts no client timeouts". The adopter already has the hook (`(*freshness.Adopter).SetCommitDelayForTest`, phase C: every commit sleeps that long inside the flip barrier before `CommitAdoption`), but only Go tests can call it. This task lets a binary built with `-tags e2ehooks` set it from `ACU_E2E_COMMIT_DELAY`, and pins that every other build ignores the variable (design note 12).

**Files:**
- Create: `cmd/apt-cacher-ultra/e2ehooks.go`, `cmd/apt-cacher-ultra/e2ehooks_off.go`
- Create (tests): `cmd/apt-cacher-ultra/e2ehooks_test.go`, `cmd/apt-cacher-ultra/e2ehooks_off_test.go`
- Modify: `cmd/apt-cacher-ultra/main.go` (`buildAdopter`, after `freshness.NewAdopter`)

**Interfaces:**
- Consumes: `(*freshness.Adopter).SetCommitDelayForTest(d time.Duration)` (`internal/freshness/adoption.go`); the test helpers `withKeyringDirs`, `newAdoptionEnabledCfg`, `newAdoptionWiringDeps` (`cmd/apt-cacher-ultra/adoption_wiring_test.go`).
- Produces: `applyE2EHooks(a *freshness.Adopter, logger *slog.Logger) error` (both build variants), the constant `e2eCommitDelayEnv = "ACU_E2E_COMMIT_DELAY"` (tagged build only) and the WARN `e2e_commit_delay_enabled` with fields `commit_delay` (a Go duration, `45s`) and `detail` — Task 7 asserts on that line.

- [ ] **Step 1: Write the tests**

`cmd/apt-cacher-ultra/e2ehooks_test.go`:

```go
//go:build e2ehooks

package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// buildAdopterLogging runs buildAdopter with adoption enabled and the
// bundled keys, and returns its log text and error.
func buildAdopterLogging(t *testing.T) (string, error) {
	t.Helper()
	withKeyringDirs(t, []string{t.TempDir()})
	cfg := newAdoptionEnabledCfg(t, true)
	c, fetcher, hosts := newAdoptionWiringDeps(t)
	var buf bytes.Buffer
	_, _, err := buildAdopter(cfg, c, fetcher, hosts, slog.New(slog.NewTextHandler(&buf, nil)))
	return buf.String(), err
}

func TestBuildAdopter_E2ECommitDelayFromTheEnvironment(t *testing.T) {
	t.Setenv(e2eCommitDelayEnv, "45s")
	logs, err := buildAdopterLogging(t)
	if err != nil {
		t.Fatalf("buildAdopter: %v", err)
	}
	if !strings.Contains(logs, "msg=e2e_commit_delay_enabled commit_delay=45s") {
		t.Fatalf("no e2e_commit_delay_enabled line with commit_delay=45s in:\n%s", logs)
	}
}

func TestBuildAdopter_E2ECommitDelayUnsetIsNoDelay(t *testing.T) {
	t.Setenv(e2eCommitDelayEnv, "")
	logs, err := buildAdopterLogging(t)
	if err != nil {
		t.Fatalf("buildAdopter: %v", err)
	}
	if strings.Contains(logs, "e2e_commit_delay_enabled") {
		t.Fatalf("an unset %s enabled the delay:\n%s", e2eCommitDelayEnv, logs)
	}
}

func TestBuildAdopter_E2ECommitDelayRejectsABadValue(t *testing.T) {
	for _, v := range []string{"-1s", "soon", "45"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv(e2eCommitDelayEnv, v)
			_, err := buildAdopterLogging(t)
			if err == nil || !strings.Contains(err.Error(), e2eCommitDelayEnv) {
				t.Fatalf("buildAdopter with %s=%q: err = %v, want one naming the variable", e2eCommitDelayEnv, v, err)
			}
		})
	}
}
```

`cmd/apt-cacher-ultra/e2ehooks_off_test.go`:

```go
//go:build !e2ehooks

package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// A release binary has no e2e seam: ACU_E2E_COMMIT_DELAY is ignored, even
// when it holds a value e2ehooks.go would refuse.
func TestBuildAdopter_ReleaseBinaryIgnoresTheE2ECommitDelay(t *testing.T) {
	for _, v := range []string{"45s", "soon"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("ACU_E2E_COMMIT_DELAY", v)
			withKeyringDirs(t, []string{t.TempDir()})
			cfg := newAdoptionEnabledCfg(t, true)
			c, fetcher, hosts := newAdoptionWiringDeps(t)
			var buf bytes.Buffer
			a, _, err := buildAdopter(cfg, c, fetcher, hosts, slog.New(slog.NewTextHandler(&buf, nil)))
			if err != nil || a == nil {
				t.Fatalf("buildAdopter: adopter %v, err %v", a, err)
			}
			if strings.Contains(buf.String(), "e2e_commit_delay") {
				t.Fatalf("a release binary read ACU_E2E_COMMIT_DELAY:\n%s", buf.String())
			}
		})
	}
}
```

- [ ] **Step 2: Run the tagged tests to verify they fail**

Run: `go test -gcflags=-e -tags e2ehooks -run 'E2ECommitDelay' ./cmd/apt-cacher-ultra/`
Expected: build failure listing `undefined: e2eCommitDelayEnv` (six lines, `e2ehooks_test.go:25` to `:52`), then `FAIL … [build failed]`.

The untagged guard (`TestBuildAdopter_ReleaseBinaryIgnoresTheE2ECommitDelay`) passes before and after this task: it pins the status quo for release builds.

- [ ] **Step 3: Implement the seam**

`cmd/apt-cacher-ultra/e2ehooks.go`:

```go
//go:build e2ehooks

package main

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
)

// e2eCommitDelayEnv names the environment variable that makes every
// adoption commit of an e2ehooks binary wait that long first (a Go duration;
// empty or unset is no delay). e2e/flat's commit-delay scenario sets it to
// 45s: a commit slower than the flip barrier's 20 s hold, which apt's 30 s
// timeout must never see (spec §7).
const e2eCommitDelayEnv = "ACU_E2E_COMMIT_DELAY"

// applyE2EHooks wires the e2e/flat test seams into a, from the environment.
//
// AIDEV-NOTE: compiled only with `go build -tags e2ehooks` (e2e/flat's
// Dockerfile); every other build gets e2ehooks_off.go's no-op, so no
// release binary reads ACU_E2E_COMMIT_DELAY. The seam is the adopter's own
// test hook (Adopter.SetCommitDelayForTest), which the flip simulations
// already use.
func applyE2EHooks(a *freshness.Adopter, logger *slog.Logger) error {
	v := os.Getenv(e2eCommitDelayEnv)
	if v == "" || a == nil {
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return fmt.Errorf("%s=%q: want a nonnegative Go duration", e2eCommitDelayEnv, v)
	}
	a.SetCommitDelayForTest(d)
	logger.Warn("e2e_commit_delay_enabled", "commit_delay", d,
		"detail", "test binary: every adoption commit waits this long first")
	return nil
}
```

`cmd/apt-cacher-ultra/e2ehooks_off.go`:

```go
//go:build !e2ehooks

package main

import (
	"log/slog"

	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
)

// applyE2EHooks is a no-op outside e2e/flat's test binary (built with
// -tags e2ehooks; see e2ehooks.go): a release binary has no test seam to
// wire and reads no ACU_E2E_* variable.
func applyE2EHooks(*freshness.Adopter, *slog.Logger) error { return nil }
```

In `cmd/apt-cacher-ultra/main.go`, `buildAdopter`:

old:
```go
	if err != nil {
		return nil, nil, fmt.Errorf("new adopter: %w", err)
	}
```
new:
```go
	if err != nil {
		return nil, nil, fmt.Errorf("new adopter: %w", err)
	}
	if err := applyE2EHooks(adopter, logger); err != nil {
		return nil, nil, fmt.Errorf("e2e hooks: %w", err)
	}
```

- [ ] **Step 4: Run the tests to verify they pass, in both builds**

Run: `go test -count=1 -tags e2ehooks -run 'E2ECommitDelay|ReleaseBinaryIgnores' -v ./cmd/apt-cacher-ultra/`
Expected: `--- PASS` for `TestBuildAdopter_E2ECommitDelayFromTheEnvironment`, `…UnsetIsNoDelay` and `…RejectsABadValue` (subtests `-1s`, `soon`, `45`); `ok`.

Run: `go test -count=1 -run 'E2ECommitDelay|ReleaseBinaryIgnores' -v ./cmd/apt-cacher-ultra/`
Expected: `--- PASS: TestBuildAdopter_ReleaseBinaryIgnoresTheE2ECommitDelay` (subtests `45s`, `soon`); `ok`.

- [ ] **Step 5: Mutation check**

Remove the `applyE2EHooks` call from `buildAdopter` (keep both new files) and run the tagged command of Step 4.
Expected: `--- FAIL: TestBuildAdopter_E2ECommitDelayFromTheEnvironment` and `--- FAIL: TestBuildAdopter_E2ECommitDelayRejectsABadValue` (the planning replay's two failing lines). Restore the call.

- [ ] **Step 6: Full verification**

Run: `gofmt -l . && go vet ./... && go vet -tags e2ehooks ./cmd/apt-cacher-ultra/ && golangci-lint run ./... && golangci-lint run --build-tags e2ehooks ./cmd/... && go test -race -count=1 -timeout 30m ./...`
Expected: `gofmt -l` lists only the five drift files; vet prints nothing; both lint runs print `0 issues.`; every package `ok`.

- [ ] **Step 7: Commit**

```bash
gofmt -l cmd/apt-cacher-ultra/e2ehooks.go cmd/apt-cacher-ultra/e2ehooks_off.go cmd/apt-cacher-ultra/e2ehooks_test.go cmd/apt-cacher-ultra/e2ehooks_off_test.go cmd/apt-cacher-ultra/main.go
git add cmd/apt-cacher-ultra/e2ehooks.go cmd/apt-cacher-ultra/e2ehooks_off.go cmd/apt-cacher-ultra/e2ehooks_test.go cmd/apt-cacher-ultra/e2ehooks_off_test.go cmd/apt-cacher-ultra/main.go
git commit -F - <<'EOF'
e2e: a commit-delay seam for the e2ehooks test binary

ACU_E2E_COMMIT_DELAY sets the adopter's commit-delay hook in a binary
built with -tags e2ehooks (e2e/flat); release builds ignore it.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```
The `gofmt -l` line prints nothing.


### Task 2: The fixture repositories, nginx and the test image

Spec §7's fixture list and §3's shapes (design notes 1, 3, 4, 9, 16; ruling R-H2-1). This task builds the image the scenarios run in; it adds no scenario yet, so its test is a smoke run of the image: nginx answers each shape's statuses and the fixtures verify against the generated key.

**Files:**
- Create: `e2e/flat/build-fixtures.sh`, `e2e/flat/nginx.conf`, `e2e/flat/Dockerfile`
- Modify: `.dockerignore`

**Interfaces:**
- Consumes: Task 1's `-tags e2ehooks` (the build stage compiles with it).
- Produces, inside the image: `/fixtures/keys/acu-e2e.gpg` and `/fixtures/keys/fingerprint`; `/fixtures/<repo>/{v1,v2}` and `/fixtures/<repo>/current -> v1` for the repos `k8s cuda nvidia jenkins sublime jitsi dists unsigned404 unsigned403 fury goreleaser` (`sublime/files` holds both builds and is not flipped); the web roots `/fixtures/www/signed.test` and `/fixtures/www/plain.test`; `/etc/acu-e2e/nginx.conf` (pid `/run/acu-e2e/nginx.pid`, access log `/var/log/acu-e2e/nginx-access.log` in the format `$host "$request" $status`, the captive-portal switch `/run/acu-e2e/captive`); the daemon at `/usr/local/bin/apt-cacher-ultra`; the harness at `/usr/local/lib/acu-e2e/{lib.sh,test.sh}` (Task 3 writes them; this task commits two-line placeholders so every commit's `Dockerfile` builds); packages `acu-<repo>` plus `acu-epoch`, `acu-jenkins-direct` and `acu-rogue`, versions `1.0`/`2.0` (`acu-epoch` `1:1.0`/`1:2.0`), each installing `/usr/share/acu-e2e/<name>` holding `<name> <version>`.

- [ ] **Step 1: Write `e2e/flat/build-fixtures.sh`**

```bash
#!/bin/bash
# build-fixtures.sh builds the e2e/flat upstream repositories under $1
# (default /fixtures), two versions of each, and the web roots nginx
# serves them from. It runs in the Dockerfile's `fixtures` stage.
#
# Layout produced:
#   keys/acu-e2e.gpg, keys/fingerprint   the throwaway signing key
#   <repo>/v1, <repo>/v2                 the two versions of a repository
#   <repo>/current -> v1                 the symlink test.sh flips
#   www/signed.test/..., www/plain.test/...
#                                        symlinks to <repo>/current at
#                                        the URL paths of spec §3's shapes
#
# Each repository reproduces one real repository's shape (spec §3, "Real
# flat repositories"); the comment on its builder says which. Packages are
# named acu-<repo> (acu-epoch, acu-jenkins-direct and acu-rogue besides),
# version 1.0 in v1 and 2.0 in v2, and install one file,
# /usr/share/acu-e2e/<name>, that holds "<name> <version>".
#
# AIDEV-NOTE: ruling R-H2-1 — the signing key is generated here, at image
# build time, and never leaves the image; no key is committed. v1 is dated
# two days before the build and v2 one day before (both in the past, so a
# cached image stays valid: no fixture carries Valid-Until), and every file
# of a version gets its version's mtime, so v1 and v2 never share an
# nginx ETag or Last-Modified even when their sizes match.
set -euo pipefail

OUT="${1:-/fixtures}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

export GNUPGHOME="$WORK/gnupg"
mkdir -m 700 "$GNUPGHOME"
gpg --batch --quiet --pinentry-mode loopback --passphrase '' \
    --quick-generate-key 'apt-cacher-ultra e2e-flat throwaway key <e2e-flat@example.invalid>' \
    rsa3072 sign 0
FPR="$(gpg --list-keys --with-colons | awk -F: '/^fpr:/ {print $10; exit}')"
mkdir -p "$OUT/keys"
gpg --batch --yes --output "$OUT/keys/acu-e2e.gpg" --export "$FPR"
printf '%s\n' "$FPR" > "$OUT/keys/fingerprint"

STAMP_V1="$(date -u -d '-2 days' '+%Y-%m-%d %H:%M:%S')"
STAMP_V2="$(date -u -d '-1 day' '+%Y-%m-%d %H:%M:%S')"
DATE_V1="$(date -u -d "$STAMP_V1 UTC" '+%a, %d %b %Y %H:%M:%S UTC')"
DATE_V2="$(date -u -d "$STAMP_V2 UTC" '+%a, %d %b %Y %H:%M:%S UTC')"
# libnvidia-container's Release Date has stayed at 2018 (spec §3).
DATE_FROZEN='Thu, 01 Mar 2018 12:00:00 UTC'

# mkdeb NAME VERSION ARCH OUT: a one-file binary package.
mkdeb() {
    local name=$1 ver=$2 arch=$3 out=$4
    local d="$WORK/deb/$name-$ver"
    mkdir -p "$d/DEBIAN" "$d/usr/share/acu-e2e" "$(dirname "$out")"
    printf '%s %s\n' "$name" "$ver" > "$d/usr/share/acu-e2e/$name"
    cat > "$d/DEBIAN/control" <<EOF
Package: $name
Version: $ver
Architecture: $arch
Maintainer: apt-cacher-ultra e2e <noreply@example.invalid>
Section: misc
Priority: optional
Description: e2e/flat fixture package
 Installs /usr/share/acu-e2e/$name, which holds its version.
EOF
    dpkg-deb --root-owner-group --build "$d" "$out" >/dev/null
}

# mksrc NAME VERSION OUTDIR: a native source package (.dsc and .tar.xz).
mksrc() {
    local name=$1 ver=$2 out=$3
    local top="$WORK/src/$name-$ver"
    local d="$top/$name-$ver"
    mkdir -p "$d/debian/source"
    echo '3.0 (native)' > "$d/debian/source/format"
    cat > "$d/debian/control" <<EOF
Source: $name
Section: misc
Priority: optional
Maintainer: apt-cacher-ultra e2e <noreply@example.invalid>
Standards-Version: 4.6.2

Package: $name
Architecture: all
Description: e2e/flat fixture source package
 The source of the $name fixture package.
EOF
    cat > "$d/debian/changelog" <<EOF
$name ($ver) unstable; urgency=low

  * e2e/flat fixture.

 -- apt-cacher-ultra e2e <noreply@example.invalid>  Thu, 01 Jan 2026 00:00:00 +0000
EOF
    printf '#!/usr/bin/make -f\n%%:\n\tdh $@\n' > "$d/debian/rules"
    (cd "$top" && dpkg-source --build "$name-$ver" >/dev/null 2>&1)
    mv "$top/${name}_${ver}.dsc" "$top/${name}_${ver}.tar.xz" "$out/"
}

# release DIR DATE [apt-ftparchive options...]: DIR/Release with Date DATE.
release() {
    local dir=$1 date=$2
    shift 2
    (cd "$dir" && apt-ftparchive \
        -o APT::FTPArchive::Release::Origin=acu-e2e \
        -o APT::FTPArchive::Release::Label=acu-e2e \
        "$@" release . > "$WORK/Release")
    sed "s/^Date: .*/Date: $date/" "$WORK/Release" > "$dir/Release"
}

sign_inline() {
    gpg --batch --quiet --yes --default-key "$FPR" --clearsign --output "$1/InRelease" "$1/Release"
}
sign_detached() { # DIR [--armor]
    local dir=$1
    shift
    gpg --batch --quiet --yes --default-key "$FPR" --detach-sign "$@" --output "$dir/Release.gpg" "$dir/Release"
}

# byhash DIR: by-hash copies of every index DIR/Release lists.
byhash() {
    local dir=$1 f
    mkdir -p "$dir/by-hash/SHA256" "$dir/by-hash/SHA512"
    while read -r f; do
        cp "$dir/$f" "$dir/by-hash/SHA256/$(sha256sum "$dir/$f" | cut -d' ' -f1)"
        cp "$dir/$f" "$dir/by-hash/SHA512/$(sha512sum "$dir/$f" | cut -d' ' -f1)"
    done < <(awk '/^SHA256:/ {on=1; next} /^[^ ]/ {on=0} on {print $3}' "$dir/Release")
}

gz() { gzip -9nk "$@"; }

# pkgs.k8s.io core:/stable:/v1.34/deb: `deb <uri>/ /`, Filenames amd64/…,
# InRelease only, no by-hash, 403 for a missing file (nginx.conf).
# acu-rogue is on the upstream but in no Packages: strict mode's refusal.
build_k8s() { # VDIR VERSION DATE
    local d="$OUT/k8s/$1"
    mkdeb acu-k8s "$2" amd64 "$d/amd64/acu-k8s_${2}_amd64.deb"
    (cd "$d" && apt-ftparchive packages amd64 > Packages)
    gz "$d/Packages"
    release "$d" "$3"
    sign_inline "$d"
    rm "$d/Release"
    mkdeb acu-rogue 1.0 amd64 "$d/amd64/acu-rogue_1.0_amd64.deb"
}

# NVIDIA CUDA ubuntu2204/x86_64: `deb <uri>/ /`, dpkg-scanpackages Filenames
# ./…, InRelease beside Release and Release.gpg, and an epoch package whose
# file name carries a literal %3a (apt requests it as %253a).
build_cuda() {
    local d="$OUT/cuda/$1"
    mkdeb acu-cuda "$2" amd64 "$d/acu-cuda_${2}_amd64.deb"
    mkdeb acu-epoch "1:$2" amd64 "$d/acu-epoch_1%3a${2}_amd64.deb"
    (cd "$d" && apt-ftparchive packages . > Packages)
    gz "$d/Packages"
    release "$d" "$3"
    sign_inline "$d"
    sign_detached "$d" --armor
}

# libnvidia-container stable/deb/amd64: `deb <uri> /`, Filenames ./…,
# InRelease only, a Release text with only a SHA512 block (Packages keeps
# its SHA256), and a Date frozen at 2018 in both versions.
build_nvidia() {
    local d="$OUT/nvidia/$1"
    mkdeb acu-nvidia "$2" amd64 "$d/acu-nvidia_${2}_amd64.deb"
    (cd "$d" && apt-ftparchive packages . > Packages)
    gz "$d/Packages"
    release "$d" "$DATE_FROZEN" \
        -o APT::FTPArchive::Release::MD5=false \
        -o APT::FTPArchive::Release::SHA1=false \
        -o APT::FTPArchive::Release::SHA256=false
    sign_inline "$d"
    rm "$d/Release"
}

# pkg.jenkins.io debian-stable: `deb <uri> binary/`, detached (Release and
# an armored Release.gpg, no InRelease), Filenames binary/… and direct/…
# relative to the parent of binary/.
build_jenkins() {
    local d="$OUT/jenkins/$1"
    mkdeb acu-jenkins "$2" all "$d/binary/acu-jenkins_${2}_all.deb"
    mkdeb acu-jenkins-direct "$2" all "$d/direct/acu-jenkins-direct_${2}_all.deb"
    (cd "$d" && { apt-ftparchive packages binary; apt-ftparchive packages direct; } > "$WORK/Packages")
    mv "$WORK/Packages" "$d/binary/Packages"
    gz "$d/binary/Packages"
    release "$d/binary" "$3" -o APT::FTPArchive::Release::Suite=binary
    sign_detached "$d/binary" --armor
}

# download.sublimetext.com apt/stable/: `deb <host-root> apt/stable/`,
# Filenames files/… relative to the host root; files/ keeps every build.
build_sublime() {
    local d="$OUT/sublime/$1"
    mkdeb acu-sublime "$2" amd64 "$OUT/sublime/files/acu-sublime_${2}_amd64.deb"
    mkdir -p "$d"
    (cd "$OUT/sublime" && apt-ftparchive packages files > "$WORK/Packages")
    mv "$WORK/Packages" "$d/Packages"
    gz "$d/Packages"
    release "$d" "$3" -o APT::FTPArchive::Release::Suite=apt/stable
    sign_inline "$d"
    sign_detached "$d" --armor
}

# download.jitsi.org stable/: `deb <uri> stable/` and a deb-src line,
# Filenames stable/…, Acquire-By-Hash with SHA256 and SHA512 only,
# Contents-amd64 for apt-file, Sources, and a binary (unarmored)
# Release.gpg beside InRelease and Release. apt (2.2 to 3.2) refuses a
# binary Release.gpg ("unsupported binary format") and uses the InRelease
# (plan deviation 1).
build_jitsi() {
    local d="$OUT/jitsi/$1"
    local s="$d/stable"
    mkdeb acu-jitsi "$2" all "$s/acu-jitsi_${2}_all.deb"
    mksrc acu-jitsi "$2" "$s"
    (cd "$d" && apt-ftparchive packages stable > "$WORK/Packages" \
        && apt-ftparchive sources stable > "$WORK/Sources" \
        && apt-ftparchive contents stable > "$WORK/Contents-amd64")
    mv "$WORK/Packages" "$WORK/Sources" "$WORK/Contents-amd64" "$s/"
    gz "$s/Packages" "$s/Sources" "$s/Contents-amd64"
    xz -k "$s/Packages"
    release "$s" "$3" \
        -o APT::FTPArchive::Release::MD5=false \
        -o APT::FTPArchive::Release::SHA1=false \
        -o APT::FTPArchive::Release::Acquire-By-Hash=yes \
        -o APT::FTPArchive::Release::Suite=stable
    byhash "$s"
    sign_inline "$s"
    sign_detached "$s"
}

# A signed dists/ repository: `deb <uri> acu main`, inline.
build_dists() {
    local d="$OUT/dists/$1"
    mkdeb acu-dists "$2" amd64 "$d/pool/main/a/acu-dists/acu-dists_${2}_amd64.deb"
    mkdir -p "$d/dists/acu/main/binary-amd64"
    (cd "$d" && apt-ftparchive packages pool/main > dists/acu/main/binary-amd64/Packages)
    gz "$d/dists/acu/main/binary-amd64/Packages"
    release "$d/dists/acu" "$3" \
        -o APT::FTPArchive::Release::Suite=acu \
        -o APT::FTPArchive::Release::Codename=acu \
        -o APT::FTPArchive::Release::Components=main \
        -o APT::FTPArchive::Release::Architectures=amd64
    sign_inline "$d/dists/acu"
}

# An unsigned repository: `deb [trusted=yes] <uri>/ /`, Release only; the
# upstream answers 404 (unsigned404) or 403 (unsigned403) for the missing
# InRelease and Release.gpg (nginx.conf).
build_unsigned() { # REPO VDIR VERSION DATE
    local d="$OUT/$1/$2"
    mkdeb "acu-$1" "$3" amd64 "$d/acu-$1_${3}_amd64.deb"
    (cd "$d" && apt-ftparchive packages . > Packages)
    gz "$d/Packages"
    release "$d" "$4"
}

# apt.fury.io (Gemfury): `deb [trusted=yes] <uri>/ /`, Packages and
# Packages.gz, no Release at all, Filenames without ./. repo.goreleaser.com
# is the same shape with ./ Filenames.
build_releaseless() { # REPO VDIR VERSION FILENAME-SED
    local d="$OUT/$1/$2"
    mkdeb "acu-$1" "$3" amd64 "$d/acu-$1_${3}_amd64.deb"
    (cd "$d" && apt-ftparchive packages . | sed "$4" > Packages)
    gz "$d/Packages"
}

for v in 1 2; do
    if [[ $v == 1 ]]; then date=$DATE_V1; else date=$DATE_V2; fi
    build_k8s "v$v" "$v.0" "$date"
    build_cuda "v$v" "$v.0" "$date"
    build_nvidia "v$v" "$v.0"
    build_jenkins "v$v" "$v.0" "$date"
    build_sublime "v$v" "$v.0" "$date"
    build_jitsi "v$v" "$v.0" "$date"
    build_dists "v$v" "$v.0" "$date"
    build_unsigned unsigned404 "v$v" "$v.0" "$date"
    build_unsigned unsigned403 "v$v" "$v.0" "$date"
    build_releaseless fury "v$v" "$v.0" 's|^Filename: \./|Filename: |'
    build_releaseless goreleaser "v$v" "$v.0" ''
done

repos=(k8s cuda nvidia jenkins sublime jitsi dists unsigned404 unsigned403 fury goreleaser)
for r in "${repos[@]}"; do
    find "$OUT/$r/v1" -exec touch -h -d "$STAMP_V1 UTC" {} +
    find "$OUT/$r/v2" -exec touch -h -d "$STAMP_V2 UTC" {} +
    ln -s v1 "$OUT/$r/current"
done
touch -d "$STAMP_V1 UTC" "$OUT/sublime/files/acu-sublime_1.0_amd64.deb"
touch -d "$STAMP_V2 UTC" "$OUT/sublime/files/acu-sublime_2.0_amd64.deb"

# link URL-PATH TARGET: the web root entry for URL-PATH.
link() {
    mkdir -p "$(dirname "$OUT/www/$1")"
    ln -s "$2" "$OUT/www/$1"
}
link 'signed.test/core:/stable:/v1.34/deb' "$OUT/k8s/current"
link signed.test/cuda/ubuntu2204/x86_64 "$OUT/cuda/current"
link signed.test/libnvidia-container/stable/deb/amd64 "$OUT/nvidia/current"
link signed.test/debian-stable "$OUT/jenkins/current"
link signed.test/apt/stable "$OUT/sublime/current"
link signed.test/files "$OUT/sublime/files"
link signed.test/jitsi "$OUT/jitsi/current"
link signed.test/debian "$OUT/dists/current"
link plain.test/unsigned404 "$OUT/unsigned404/current"
link plain.test/unsigned403 "$OUT/unsigned403/current"
link plain.test/fury "$OUT/fury/current"
link plain.test/goreleaser "$OUT/goreleaser/current"

chmod -R a+rX "$OUT"
echo "build-fixtures: key $FPR; v1 dated $DATE_V1, v2 dated $DATE_V2"
```

`chmod +x e2e/flat/build-fixtures.sh`.

- [ ] **Step 2: Write `e2e/flat/nginx.conf`**

```nginx
# nginx for e2e/flat: the two upstream host names, served from the web
# roots build-fixtures.sh links to each repository's `current` symlink.
#   signed.test  the signed repositories (flat and dists/)
#   plain.test   the unsigned and Release-less repositories
# test.sh starts it as `nginx -c /etc/acu-e2e/nginx.conf`; both names
# resolve to 127.0.0.1 through /etc/hosts.
user www-data;
worker_processes 1;
pid /run/acu-e2e/nginx.pid;
error_log /var/log/acu-e2e/nginx-error.log;

events {
    worker_connections 256;
}

http {
    include /etc/nginx/mime.types;
    default_type application/octet-stream;
    # $request is the request line the cache sent upstream, verbatim:
    # test.sh pins the ./, %253a and by-hash lines with it.
    log_format e2e '$host "$request" $status';
    access_log /var/log/acu-e2e/nginx-access.log e2e;
    sendfile on;
    # Nothing is cached between a symlink flip and the next request.
    open_file_cache off;

    server {
        listen 127.0.0.1:80 default_server;
        server_name signed.test;
        root /fixtures/www/signed.test;

        # pkgs.k8s.io's CDN answers 403 for a file it does not have.
        location ^~ "/core:/stable:/v1.34/deb/" {
            try_files $uri =403;
        }
    }

    server {
        listen 127.0.0.1:80;
        server_name plain.test;
        root /fixtures/www/plain.test;

        # The 403 variant of an unsigned repository: its missing InRelease
        # and Release.gpg answer 403, as S3 and CloudFront do.
        location ^~ /unsigned403/ {
            try_files $uri =403;
        }

        # The Release-less repository. While /run/acu-e2e/captive exists
        # every path answers an HTML page with status 200: a captive
        # portal in front of the upstream.
        location ^~ /fury/ {
            if (-f /run/acu-e2e/captive) {
                return 200 "<html><body>captive portal</body></html>\n";
            }
        }
    }
}
```

- [ ] **Step 3: Write `e2e/flat/Dockerfile`**

```dockerfile
# e2e/flat: real apt against flat (and dists/) fixture repositories through
# apt-cacher-ultra, in one container (spec §7, "Real-apt end-to-end"): nginx
# serves the upstream fixtures, the daemon runs beside it, and apt clients
# with their own state directories talk to it. e2e/flat/run.sh builds and
# runs one image per client image.
#
# Three stages:
#   build     the daemon, with -tags e2ehooks (the commit-delay seam,
#             cmd/apt-cacher-ultra/e2ehooks.go); never shipped.
#   fixtures  build-fixtures.sh: the repositories, two versions each, and
#             a throwaway signing key generated here (ruling R-H2-1).
#   test      ${CLIENT_IMAGE} with nginx, curl, jq and apt-file; its
#             docker-* apt.conf files are removed, so apt behaves as on a
#             host (no gzip-only indexes, languages, or deleted archives).
#
# Build context is the repo root:
#   docker build -f e2e/flat/Dockerfile --build-arg CLIENT_IMAGE=debian:12 \
#       -t acu-e2e-flat:debian-12 .
ARG CLIENT_IMAGE=debian:12

FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -tags e2ehooks \
    -ldflags "-s -w -X main.Version=0.0.0-e2e-flat" \
    -o /out/apt-cacher-ultra ./cmd/apt-cacher-ultra

FROM debian:bookworm-slim AS fixtures
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        apt-utils dpkg-dev gnupg gzip xz-utils && \
    rm -rf /var/lib/apt/lists/*
COPY e2e/flat/build-fixtures.sh /usr/local/bin/build-fixtures.sh
RUN bash /usr/local/bin/build-fixtures.sh /fixtures

FROM ${CLIENT_IMAGE} AS test
ENV DEBIAN_FRONTEND=noninteractive
# Debian 11's LTS ended on 2026-08-31 and its packages moved to
# archive.debian.org; the image still names deb.debian.org (design note 9).
RUN . /etc/os-release && \
    if [ "$VERSION_CODENAME" = bullseye ]; then \
        printf '%s\n' 'deb http://archive.debian.org/debian bullseye main' \
            'deb http://archive.debian.org/debian-security bullseye-security main' \
            > /etc/apt/sources.list; \
    fi && \
    rm -f /etc/apt/apt.conf.d/docker-* && \
    apt-get update && \
    apt-get install -y --no-install-recommends \
        nginx-light curl jq apt-file ca-certificates && \
    rm -rf /var/lib/apt/lists/* /etc/apt/sources.list /etc/apt/sources.list.d/* && \
    mkdir -p /run/acu-e2e /var/log/acu-e2e
COPY --from=build /out/apt-cacher-ultra /usr/local/bin/apt-cacher-ultra
COPY --from=fixtures /fixtures /fixtures
COPY e2e/flat/nginx.conf /etc/acu-e2e/nginx.conf
COPY e2e/flat/lib.sh e2e/flat/test.sh /usr/local/lib/acu-e2e/
CMD ["bash", "/usr/local/lib/acu-e2e/test.sh"]
```

- [ ] **Step 4: Keep the build context lean (design note 16)**

In `.dockerignore`, after the block `# Local scratchpad — not part of any image.` / `prompt`, add:

```
# Claude Code state (worktrees: hundreds of MB in the main checkout) and
# the binary `go build` leaves in the repo root.
.claude/
/apt-cacher-ultra
```

- [ ] **Step 5: Build the image and smoke-test it**

Write the placeholders Task 3 replaces — `e2e/flat/lib.sh`:
```bash
# shellcheck shell=bash
# lib.sh: e2e/flat's harness library (written by the next commit).
```
and `e2e/flat/test.sh`:
```bash
#!/bin/bash
echo "e2e/flat: no scenarios yet"
```

Run: `docker build -f e2e/flat/Dockerfile --build-arg CLIENT_IMAGE=debian:12 -t acu-e2e-flat:debian-12 .`
Expected: the build ends `naming to docker.io/library/acu-e2e-flat:debian-12`; the fixtures stage prints `build-fixtures: key <40 hex>; v1 dated <two days ago> UTC, v2 dated <one day ago> UTC` (cold: about 45 s on the planning machine; 20 s with the Go stage cached).

Run:
```bash
docker run --rm acu-e2e-flat:debian-12 bash -c '
echo "127.0.0.1 signed.test plain.test" >> /etc/hosts
nginx -c /etc/acu-e2e/nginx.conf
for u in http://signed.test/core:/stable:/v1.34/deb/InRelease http://signed.test/core:/stable:/v1.34/deb/README \
    http://signed.test/files/acu-sublime_2.0_amd64.deb http://plain.test/unsigned404/Release.gpg \
    http://plain.test/unsigned403/Release.gpg http://plain.test/fury/Packages.gz; do
    printf "%s %s\n" "$(curl -s -o /dev/null -w "%{http_code}" "$u")" "$u"
done
gpgv --keyring /fixtures/keys/acu-e2e.gpg /fixtures/k8s/v2/InRelease 2>&1 | tail -1
gpgv --keyring /fixtures/keys/acu-e2e.gpg /fixtures/jitsi/v1/stable/Release.gpg /fixtures/jitsi/v1/stable/Release 2>&1 | tail -1
grep -c "^ [0-9a-f]\{128\} " /fixtures/nvidia/v1/InRelease
grep -E "^(Date|Acquire-By-Hash|Suite):" /fixtures/jitsi/v2/stable/Release
readlink /fixtures/k8s/current'
```
Expected (the planning run; the dates differ):
```
200 http://signed.test/core:/stable:/v1.34/deb/InRelease
403 http://signed.test/core:/stable:/v1.34/deb/README
200 http://signed.test/files/acu-sublime_2.0_amd64.deb
404 http://plain.test/unsigned404/Release.gpg
403 http://plain.test/unsigned403/Release.gpg
200 http://plain.test/fury/Packages.gz
gpgv: Good signature from "apt-cacher-ultra e2e-flat throwaway key <e2e-flat@example.invalid>"
gpgv: Good signature from "apt-cacher-ultra e2e-flat throwaway key <e2e-flat@example.invalid>"
2
Acquire-By-Hash: yes
Date: Sat, 24 Oct 2026 02:12:01 UTC
Suite: stable
v1
```
The third-to-last count is the two SHA512 lines of libnvidia-container's SHA512-only `Release` text; the second `Good signature` is Jitsi's binary `Release.gpg`.

Run: `docker build -f e2e/flat/Dockerfile --build-arg CLIENT_IMAGE=debian:11 -t acu-e2e-flat:debian-11 .`
Expected: success; the test stage's `apt-get update` lists `http://archive.debian.org/debian bullseye` (design note 9).

Run: `shellcheck -x e2e/flat/*.sh`
Expected: no output.

- [ ] **Step 6: Commit**

```bash
git add e2e/flat/build-fixtures.sh e2e/flat/nginx.conf e2e/flat/Dockerfile e2e/flat/lib.sh e2e/flat/test.sh .dockerignore
git commit -F - <<'EOF'
e2e/flat: fixture repositories, nginx and the test image

Every flat repository shape of spec §3 (k8s, CUDA, libnvidia-container,
Jenkins, Sublime Text, Jitsi), two unsigned variants, two Release-less
repositories and a dists/ repository, two versions each behind a
`current` symlink, signed with a key generated at build time.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```
The placeholders keep this commit's image buildable; Task 3 replaces both.


### Task 3: The harness, `run.sh`, `make e2e-flat` and the first-contact scenario

Spec §7: "installs succeed; the exact request lines apt sends are pinned (`./`, `%253a`, `by-hash/SHA512`); `apt-get` output is grepped …; zero `unvouched_deb_refused` on the signed host", the Jitsi fixture's "`Contents-amd64` via apt-file, a `deb-src` line", and the signed/unsigned host split. This task writes the harness library, the driver and the first scenario, which every later scenario builds on (design notes 2, 5, 6, 8, 14; rulings R-H2-2, R-H2-5; Review Focus 1, 3, 4).

**Files:**
- Replace (Task 2's placeholders): `e2e/flat/lib.sh`, `e2e/flat/test.sh`
- Create: `e2e/flat/run.sh`
- Modify: `Makefile` (`.PHONY`, the `e2e-flat` target)

**Interfaces:**
- Consumes: Task 2's image layout (Interfaces there); Task 1's `e2e_commit_delay_enabled` (used from Task 7).
- Produces, in `lib.sh` (sourced by `test.sh`; later tasks use exactly these): settings `DAEMON_LOG NGINX_LOG CONFIG KEY PROXY ADMIN MIRROR COOLDOWN=2 PERIODIC=20 TICK=5 REACH APT_ERRORS APT_TIMEOUTS`; `say MSG`, `fail MSG`, `scenario NAME DESC` (sets `SCENARIO_MARK`, the daemon-log line count at its start), `passed`, `poll SECONDS WHAT CMD…` and `poll_quietly SECONDS CMD…` (both set `POLL_TOOK`), `log_mark`, `re TEXT`, `kv KEY VALUE`, `event NAME`, `suite_kv PATH`, `log_lines MARK REGEX…`, `log_count`, `log_has`, `harness_mark FIELDS`, `start_nginx`, `stop_nginx`, `start_daemon [NAME=VALUE…]`, `stop_daemon`, `publish REPO v1|v2`, `suite HOST PATH FIELD`, `adopted_as HOST PATH FORM`, `metric SAMPLE`, `client NAME PROXY|DIRECT SOURCE…`, `capt NAME CMD…`, `apt_update NAME [--grep=REGEX] [apt-get options…]` (sets `APT_LOG`), `candidate NAME PKG`, `candidates_are NAME PKG=VER…`, `apt_install NAME PKG=VER… [-- apt-get options…]` (sets `APT_LOG`), `until_candidates LIMIT NAME… -- PKG=VER…` (sets `POLL_TOOK`), `apt_sent REGEX`, `upstream_got HOST PATH-REGEX`. In `test.sh`: `SRC_*` source lines, the suite paths `K8S CUDA NVIDIA JENKINS SUBLIME JITSI DISTS`, `write_config`, `setup`, `first_contact` (sets `UNSIGNED_FIRST_CONTACT`, Task 8's clock) and the run list at the end of the file. In `run.sh`: `E2E_FLAT_IMAGES`, `E2E_FLAT_UNSIGNED_IMAGES`, `E2E_FLAT_NAME`; the container gets `E2E_IMAGE` and `E2E_UNSIGNED_WAIT`.

- [ ] **Step 1: Write `e2e/flat/lib.sh`**

```bash
# shellcheck shell=bash disable=SC2034  # the settings below are test.sh's
# lib.sh: helpers for e2e/flat's test.sh, which sources it. Everything runs
# inside the test container: nginx (the upstream), the daemon and the apt
# clients share it.
#
# AIDEV-NOTE: ruling R-H2-5 — every wait in this harness is poll (a bounded
# poll on an observable: a log line, an HTTP answer, an apt result) with a
# failure message; no bare sleep stands in for a condition. fail prints
# the scenario, the daemon's log tail and nginx's access-log tail.

E2E_IMAGE="${E2E_IMAGE:-unknown image}"
DAEMON_LOG=/var/log/acu-e2e/daemon.log
NGINX_LOG=/var/log/acu-e2e/nginx-access.log
CONFIG=/etc/acu-e2e/config.toml
KEY=/etc/acu-e2e/keys/acu-e2e.gpg
PROXY=http://127.0.0.1:3142
ADMIN=http://127.0.0.1:6789
MIRROR=http://127.0.0.1:3142/m-signed

# The daemon's timers (config.toml, written by write_config). The scheduler
# ticks every PERIODIC/4 seconds (5 s at least).
COOLDOWN=2
PERIODIC=20
TICK=5
# The longest a published change may take to reach a client that runs
# apt-get update once a cooldown: one cooldown for its check to fire,
# then the adoption and the flip barrier (phase A's quiet period is 1 s
# after the client's last anchor; measured 1–4 s; design note 5).
REACH=$((COOLDOWN + 20))

# apt-get update's exit status says nothing about failed items (spec §7):
# its output is grepped instead.
APT_ERRORS='Failed to fetch|Connection failed|timed out|Hash Sum mismatch|Mirror sync in progress'
# The commit-delay scenario's scoped grep: client timeouts only. A
# Hash Sum mismatch there is the documented phase-B-expiry residual.
APT_TIMEOUTS='timed out|Connection failed|Could not wait for server fd'

SCENARIO=setup
SCENARIO_START=$SECONDS
SCENARIO_MARK=0
DAEMON_PID=""
APT_RUNS=0
TIMINGS=()

say() {
    printf '[e2e-flat %s %4ds] %s\n' "$E2E_IMAGE" "$SECONDS" "$*"
}

fail() {
    say "FAIL in scenario '$SCENARIO': $*"
    echo "=== daemon log tail ($DAEMON_LOG)"
    tail -n 80 "$DAEMON_LOG" 2>/dev/null | sed -E 's/ client_addr=[^ ]+//'
    echo "=== nginx access log tail ($NGINX_LOG)"
    tail -n 30 "$NGINX_LOG" 2>/dev/null
    exit 1
}

# scenario NAME DESCRIPTION: starts a scenario; log assertions that pass
# SCENARIO_MARK see only its lines. A marker line in the daemon's log
# (appended like the daemon's own) dates the scenario for a reader.
scenario() {
    SCENARIO=$1
    SCENARIO_START=$SECONDS
    harness_mark "scenario=$1"
    SCENARIO_MARK=$(log_mark)
    say "=== scenario $1: $2"
}

# harness_mark FIELDS: a harness line in the daemon's log, with the time.
harness_mark() {
    echo "level=INFO msg=e2e_harness $* unixtime=$(date +%s)" >> "$DAEMON_LOG"
}

passed() {
    local took=$((SECONDS - SCENARIO_START))
    TIMINGS+=("$SCENARIO ${took}s")
    say "--- scenario $SCENARIO passed (${took}s)"
}

# poll SECONDS WHAT COMMAND...: runs COMMAND once a second until it
# succeeds; fails the scenario after SECONDS. POLL_TOOK is the wait.
poll() {
    local limit=$1 what=$2
    shift 2
    poll_quietly "$limit" "$@" || fail "timed out after ${limit}s waiting for $what"
}

# poll_quietly SECONDS COMMAND...: poll, returning 1 at the deadline
# instead of failing, for a caller that adds its own diagnostics.
poll_quietly() {
    local limit=$1 start=$SECONDS
    shift
    until "$@"; do
        ((SECONDS - start < limit)) || return 1
        sleep 1
    done
    POLL_TOOK=$((SECONDS - start))
}

# --- the daemon's log --------------------------------------------------

log_mark() {
    wc -l < "$DAEMON_LOG" 2>/dev/null || echo 0
}

# re QUOTED-TEXT: TEXT as an extended regex that matches it literally.
re() {
    printf '%s' "$1" | sed -e 's/[][\.*^$+?(){}|/]/\\&/g'
}

# kv KEY VALUE: the regex of one key=value field of a text log line.
kv() {
    printf '(^| )%s=%s( |$)' "$1" "$(re "$2")"
}

# log_lines MARK REGEX...: the lines after line MARK that match every REGEX.
log_lines() {
    local out
    out="$(tail -n +"$(($1 + 1))" "$DAEMON_LOG")"
    shift
    local r
    for r in "$@"; do
        out="$(grep -E -- "$r" <<< "$out" || true)"
    done
    [[ -n $out ]] && printf '%s\n' "$out"
    return 0
}

log_count() {
    log_lines "$@" | grep -c . || true
}

log_has() {
    (($(log_count "$@") > 0))
}

# event NAME: the regex of a log line's msg.
event() {
    kv msg "$1"
}

suite_kv() {
    kv suite_path "$1"
}

# --- processes -----------------------------------------------------------

start_nginx() {
    nginx -c /etc/acu-e2e/nginx.conf
    poll 10 "nginx on :80" curl -fsS -o /dev/null -H 'Host: signed.test' \
        http://127.0.0.1/debian/dists/acu/InRelease
}

stop_nginx() {
    nginx -c /etc/acu-e2e/nginx.conf -s stop
    poll 10 "nginx to stop" port_closed 80
}

port_closed() {
    ! (echo > "/dev/tcp/127.0.0.1/$1") 2> /dev/null
}

# start_daemon [NAME=VALUE...]: starts the daemon with the environment
# additions given, and waits for both listeners.
start_daemon() {
    say "starting the daemon${*:+ with $*}"
    env "$@" /usr/local/bin/apt-cacher-ultra -config "$CONFIG" >> "$DAEMON_LOG" 2>&1 &
    DAEMON_PID=$!
    poll 30 "the daemon's admin listener" curl -fsS -o /dev/null "$ADMIN/healthz"
    poll 30 "the daemon's proxy listener" bash -c '(echo > /dev/tcp/127.0.0.1/3142) 2> /dev/null'
}

stop_daemon() {
    [[ -n $DAEMON_PID ]] || return 0
    say "stopping the daemon"
    kill "$DAEMON_PID"
    poll 30 "the daemon to exit" daemon_gone
    wait "$DAEMON_PID" 2> /dev/null || true
    DAEMON_PID=""
}

daemon_gone() {
    ! kill -0 "$DAEMON_PID" 2> /dev/null
}

# publish REPO VERSION: points the upstream's REPO/current at VERSION
# (v1 or v2) with one rename, as a real publish replaces a tree.
publish() {
    ln -sfn "$2" "/fixtures/$1/current.new"
    mv -T "/fixtures/$1/current.new" "/fixtures/$1/current"
    say "published $1 $2"
}

# --- the status JSON ---------------------------------------------------

# suite HOST SUITE-PATH FIELD: one field of the suite's entry in the admin
# status JSON (suites[]); empty when the suite or the field is absent.
suite() {
    curl -fsS "$ADMIN/?format=json" |
        jq -r --arg h "$1" --arg p "$2" --arg f "$3" \
            '.suites[] | select(.host == $h and .suite_path == $p) | .[$f] // empty'
}

# adopted_as HOST SUITE-PATH FORM: the suite serves a snapshot of FORM.
adopted_as() {
    [[ -n $(suite "$1" "$2" current_snapshot_id) && $(suite "$1" "$2" anchor_form) == "$3" ]]
}

# metric NAME-AND-LABELS: the value of one sample on /metrics (empty when
# it has none yet).
metric() {
    curl -fsS "$ADMIN/metrics" | awk -v m="$1" '$1 == m {print $2}'
}

# --- apt clients ---------------------------------------------------------

# client NAME PROXY SOURCE-LINE...: a fresh apt client: its own sources,
# lists and archives under /clients/NAME (dpkg's database is shared, so
# install checks purge what they install). PROXY is the cache's proxy
# URL, or DIRECT for a mirror-mode client.
client() {
    local name=$1 proxy=$2 c=/clients/$1
    shift 2
    rm -rf "$c"
    mkdir -p "$c/lists/partial" "$c/cache/archives/partial" "$c/sources.list.d"
    printf '%s\n' "$@" > "$c/sources.list"
    cat > "$c/apt.conf" << EOF
Dir::Etc::SourceList "$c/sources.list";
Dir::Etc::SourceParts "$c/sources.list.d";
Dir::State::Lists "$c/lists";
Dir::Cache "$c/cache";
Acquire::http::Proxy "$proxy";
EOF
}

# capt NAME COMMAND...: COMMAND under client NAME's apt configuration.
capt() {
    local name=$1
    shift
    APT_CONFIG=/clients/$name/apt.conf "$@"
}

# apt_update NAME [GREP] [apt-get options...]: apt-get update on client
# NAME; fails when its output matches GREP (default APT_ERRORS). The
# output is kept in APT_LOG.
apt_update() {
    local name=$1 pattern=$APT_ERRORS
    shift
    if [[ ${1:-} == --grep=* ]]; then
        pattern=${1#--grep=}
        shift
    fi
    APT_RUNS=$((APT_RUNS + 1))
    APT_LOG=/clients/$name/update.$APT_RUNS.log
    capt "$name" apt-get update "$@" > "$APT_LOG" 2>&1 || true
    if grep -E "$pattern" "$APT_LOG" > /dev/null; then
        sed 's/^/    | /' "$APT_LOG"
        fail "apt-get update on client $name printed an error (above)"
    fi
}

# candidate NAME PACKAGE: the version client NAME would install.
candidate() {
    capt "$1" apt-cache policy "$2" | awk '/Candidate:/ {print $2}'
}

# candidates_are NAME PACKAGE=VERSION...: every candidate is as given.
candidates_are() {
    local name=$1 spec
    shift
    for spec in "$@"; do
        [[ $(candidate "$name" "${spec%%=*}") == "${spec#*=}" ]] || return 1
    done
}

# apt_install NAME PACKAGE=VERSION... [-- apt-get options...]: installs the
# packages on client NAME, checks the installed files' versions, then
# purges them. The output is kept in APT_LOG.
apt_install() {
    local name=$1 specs=() opts=() spec
    shift
    while (($#)); do
        if [[ $1 == -- ]]; then
            shift
            opts=("$@")
            break
        fi
        specs+=("$1")
        shift
    done
    APT_RUNS=$((APT_RUNS + 1))
    APT_LOG=/clients/$name/install.$APT_RUNS.log
    if ! capt "$name" apt-get install -y --no-install-recommends "${opts[@]}" "${specs[@]}" \
        > "$APT_LOG" 2>&1 || grep -E "$APT_ERRORS" "$APT_LOG" > /dev/null; then
        sed 's/^/    | /' "$APT_LOG"
        fail "apt-get install ${specs[*]} on client $name failed (above)"
    fi
    local pkgs=()
    for spec in "${specs[@]}"; do
        local pkg=${spec%%=*} ver=${spec#*=}
        pkgs+=("$pkg")
        [[ $(cat "/usr/share/acu-e2e/$pkg") == "$pkg $ver" ]] ||
            fail "client $name installed $(cat "/usr/share/acu-e2e/$pkg" 2> /dev/null), want $pkg $ver"
    done
    dpkg --purge "${pkgs[@]}" > /dev/null
}

# until_candidates LIMIT NAME... -- PACKAGE=VERSION...: runs apt-get update
# on every client NAME once a cooldown until each reports the versions;
# every update must be clean. POLL_TOOK is the wait.
until_candidates() {
    local limit=$1 names=() specs=() start=$SECONDS n
    shift
    while [[ $1 != -- ]]; do
        names+=("$1")
        shift
    done
    shift
    specs=("$@")
    while :; do
        local all=1
        for n in "${names[@]}"; do
            apt_update "$n"
            candidates_are "$n" "${specs[@]}" || all=0
        done
        ((all)) && break
        if ((SECONDS - start >= limit)); then
            fail "clients ${names[*]} did not report ${specs[*]} within ${limit}s"
        fi
        sleep "$COOLDOWN"
    done
    POLL_TOOK=$((SECONDS - start))
}

# --- request lines -------------------------------------------------------

# apt_sent REGEX: client debug output (Debug::Acquire::http) in APT_LOG
# holds a request line apt sent that matches REGEX exactly. apt prints
# the header lines as sent, CRLF included.
apt_sent() {
    tr -d '\r' < "$APT_LOG" | grep -Eq -- "^GET $1 HTTP/1\.1\$" ||
        fail "apt did not send GET $1 (Debug::Acquire::http output in $APT_LOG)"
}

# upstream_got HOST PATH-REGEX: nginx logged that request line from the
# cache.
upstream_got() {
    grep -Eq -- "^$(re "$1") \"GET $2 HTTP/1\\.1\" " "$NGINX_LOG" ||
        fail "the upstream never got GET $2 for $1"
}
```

- [ ] **Step 2: Write `e2e/flat/test.sh`** (this task's version: the first scenario and the run list; Tasks 4–8 add scenarios above the run list and extend it)

```bash
#!/bin/bash
# e2e/flat in-container test (spec §7, "Real-apt end-to-end"): real apt
# clients against flat and dists/ fixture repositories through
# apt-cacher-ultra, with nginx as the upstream, in one container.
# e2e/flat/run.sh builds the image per client image and runs this as its
# CMD. Scenarios run in order and share one cache; e2e/flat/README.md says
# what each one proves.
#
# E2E_IMAGE names the client image in messages. E2E_UNSIGNED_WAIT=1 runs
# the unsigned first-adoption scenario, which waits out the unsigned
# confirmation: 5 to 15 minutes after the first contact (run.sh sets it
# for the oldest and newest images only).
set -euo pipefail
# shellcheck source=e2e/flat/lib.sh
. /usr/local/lib/acu-e2e/lib.sh

# The repositories, as apt clients name them (spec §3's shapes; see
# build-fixtures.sh).
SRC_K8S="deb [signed-by=$KEY] http://signed.test/core:/stable:/v1.34/deb/ /"
SRC_K8S_DOT="deb [signed-by=$KEY] http://signed.test/core:/stable:/v1.34/deb ./"
SRC_CUDA="deb [signed-by=$KEY] http://signed.test/cuda/ubuntu2204/x86_64/ /"
SRC_NVIDIA="deb [signed-by=$KEY] http://signed.test/libnvidia-container/stable/deb/amd64 /"
SRC_JENKINS="deb [signed-by=$KEY] http://signed.test/debian-stable binary/"
SRC_SUBLIME="deb [signed-by=$KEY] http://signed.test/ apt/stable/"
SRC_JITSI="deb [signed-by=$KEY] http://signed.test/jitsi stable/"
SRC_JITSI_SRC="deb-src [signed-by=$KEY] http://signed.test/jitsi stable/"
SRC_DISTS="deb [signed-by=$KEY] http://signed.test/debian acu main"
SRC_UNSIGNED404="deb [trusted=yes] http://plain.test/unsigned404/ /"
SRC_UNSIGNED403="deb [trusted=yes] http://plain.test/unsigned403/ /"
SRC_FURY="deb [trusted=yes] http://plain.test/fury/ /"
SRC_GORELEASER="deb [trusted=yes] http://plain.test/goreleaser/ /"

# The suites the daemon tracks for them.
K8S=/core:/stable:/v1.34/deb
CUDA=/cuda/ubuntu2204/x86_64
NVIDIA=/libnvidia-container/stable/deb/amd64
JENKINS=/debian-stable/binary
SUBLIME=/apt/stable
JITSI=/jitsi/stable
DISTS=/debian/dists/acu

write_config() {
    mkdir -p /etc/acu-e2e/keys /var/cache/acu-e2e
    cp /fixtures/keys/acu-e2e.gpg "$KEY"
    # Short timers (ruling R-H2-2): the cooldown, the refresh interval
    # (tick 5 s) and the quietest flip barrier; the unsigned confirmation
    # stays max(cooldown, 5m) = 5 minutes, as spec C5 fixes it.
    cat > "$CONFIG" << EOF
[cache]
dir = "/var/cache/acu-e2e"
listen = "127.0.0.1:3142"

[upstream]
allowed_host_regex = ['^(signed|plain)\.test\$']
deny_target_ranges = []

[freshness]
cooldown = "${COOLDOWN}s"
periodic_refresh = "${PERIODIC}s"

[adoption]
enabled = true
require_signature = true
flip_quiet_period = "1s"
keyring_dirs = ["/etc/acu-e2e/keys"]

[integrity]
refuse_unvouched_debs = true

[[trusted_signer]]
match_canonical_host = '^signed\.test\$'
fingerprints = ["$(cat /fixtures/keys/fingerprint)"]

[[mirror]]
prefix = "/m-signed"
upstream = "http://signed.test/"

[log]
level = "info"
format = "text"
EOF
}

setup() {
    say "apt $(apt-get --version | head -n 1 | cut -d' ' -f2); fixtures key $(cat /fixtures/keys/fingerprint)"
    echo '127.0.0.1 signed.test plain.test' >> /etc/hosts
    : > "$DAEMON_LOG"
    write_config
    start_nginx
    start_daemon
}

# Every signed suite adopts in its upstream's form; the unsigned ones wait
# for confirmation; the Release-less one refreshes without a Release.
first_contact() {
    scenario first-contact "every repository through one client: adoption, installs, request lines, strict mode"
    client main "$PROXY" "$SRC_K8S" "$SRC_CUDA" "$SRC_NVIDIA" "$SRC_JENKINS" "$SRC_SUBLIME" \
        "$SRC_JITSI" "$SRC_JITSI_SRC" "$SRC_DISTS" "$SRC_UNSIGNED404" "$SRC_UNSIGNED403" "$SRC_FURY" \
        "$SRC_GORELEASER"
    # The error grep is not vacuous on this apt: an update of a source the
    # cache refuses (a host outside its allowlist) must trip it.
    client control "$PROXY" "deb [trusted=yes] http://nowhere.test/ /"
    capt control apt-get update > /clients/control/update.log 2>&1 || true
    if ! grep -Eq "$APT_ERRORS" /clients/control/update.log; then
        sed 's/^/    | /' /clients/control/update.log
        fail "the apt error grep did not match a failed update (above)"
    fi

    UNSIGNED_FIRST_CONTACT=$SECONDS
    apt_update main -o Debug::Acquire::http=true
    # by-hash uses the strongest hash the Release declares (spec §3).
    apt_sent 'http://signed\.test/jitsi/stable/by-hash/SHA512/[0-9a-f]{128}'
    # A binary Release.gpg is never fetched: apt uses the InRelease
    # (deviation 1).
    if grep -q '^GET http://signed\.test/jitsi/stable/Release\.gpg ' "$APT_LOG"; then
        fail "apt fetched jitsi's Release.gpg; the fixture's InRelease should be used"
    fi

    local s
    for s in "$K8S inline" "$CUDA inline" "$NVIDIA inline" "$JENKINS detached" \
        "$SUBLIME inline" "$JITSI inline" "$DISTS inline"; do
        poll 90 "signed.test${s% *} to adopt (${s#* })" adopted_as signed.test "${s% *}" "${s#* }"
    done
    for s in /fury /goreleaser; do
        poll 30 "the Release-less $s to refresh without a Release" log_has "$SCENARIO_MARK" \
            "$(event freshness_check)" "$(suite_kv "$s")" "$(kv form anchorless)" 'result=anchorless_(unchanged|refreshed)'
    done
    for s in /unsigned404 /unsigned403; do
        poll 30 "plain.test$s to wait for its confirmation" log_has "$SCENARIO_MARK" \
            "$(event adoption_refused)" "$(suite_kv "$s")" "$(kv reason unsigned_unconfirmed)"
    done

    # Served from the snapshots now: still clean.
    apt_update main
    apt_install main acu-k8s=1.0 acu-cuda=1.0 acu-epoch=1:1.0 acu-nvidia=1.0 acu-jenkins=1.0 \
        acu-jenkins-direct=1.0 acu-sublime=1.0 acu-jitsi=1.0 acu-dists=1.0 \
        acu-unsigned404=1.0 acu-unsigned403=1.0 acu-fury=1.0 acu-goreleaser=1.0 \
        -- -o Debug::Acquire::http=true
    # The literal ./ of a dpkg-scanpackages Filename and the epoch's %3a,
    # as apt sends them and as the cache sends them upstream (spec C1).
    apt_sent "$(re "http://signed.test$CUDA/./acu-epoch_1%253a1.0_amd64.deb")"
    upstream_got signed.test "$(re "$CUDA/./acu-epoch_1%253a1.0_amd64.deb")"
    apt_sent "$(re "http://signed.test$NVIDIA/./acu-nvidia_1.0_amd64.deb")"
    upstream_got signed.test "$(re "$NVIDIA/./acu-nvidia_1.0_amd64.deb")"
    # Every signed .deb was vouched by its snapshot (spec C6), whatever its
    # Filename's shape.
    local deb
    for deb in "$K8S/amd64/acu-k8s_1.0_amd64.deb" "$CUDA/./acu-cuda_1.0_amd64.deb" \
        "$CUDA/./acu-epoch_1%3a1.0_amd64.deb" "$NVIDIA/./acu-nvidia_1.0_amd64.deb" \
        /debian-stable/binary/acu-jenkins_1.0_all.deb /debian-stable/direct/acu-jenkins-direct_1.0_all.deb \
        /files/acu-sublime_1.0_amd64.deb /jitsi/stable/acu-jitsi_1.0_all.deb \
        /debian/pool/main/a/acu-dists/acu-dists_1.0_amd64.deb; do
        log_has "$SCENARIO_MARK" "$(event request)" "$(kv path "$deb")" "$(kv validated_hash true)" ||
            fail "$deb was not served vouched (validated_hash=true)"
    done

    # Jitsi's Contents-amd64 through apt-file, and its Sources.
    capt main apt-file search /usr/share/acu-e2e/acu-jitsi | grep -q '^acu-jitsi: /usr/share/acu-e2e/acu-jitsi$' ||
        fail "apt-file did not find acu-jitsi's file in Contents-amd64"
    capt main apt-cache showsrc acu-jitsi | grep -q '^Package: acu-jitsi$' ||
        fail "apt-cache showsrc found no acu-jitsi source package"

    # Strict mode is on for signed.test: an undeclared .deb is refused...
    local st
    st=$(curl -s -o /dev/null -w '%{http_code}' -x "$PROXY" \
        "http://signed.test$K8S/amd64/acu-rogue_1.0_amd64.deb")
    [[ $st == 502 ]] || fail "an undeclared .deb on signed.test answered $st, want 502 (strict mode)"
    log_has "$SCENARIO_MARK" "$(event unvouched_deb_refused)" "$(kv path "$K8S/amd64/acu-rogue_1.0_amd64.deb")" ||
        fail "no unvouched_deb_refused line for the undeclared .deb"
    # ...and pkgs.k8s.io's shape answers 403 for a file it lacks, which
    # the cache passes on. (Not Release: an inline snapshot answers its
    # suite's Release 404 itself, spec §10 item 9.)
    st=$(curl -s -o /dev/null -w '%{http_code}' -x "$PROXY" "http://signed.test$K8S/README")
    [[ $st == 403 ]] || fail "a missing file of the k8s shape answered $st through the cache, want 403"
    passed
}

setup
first_contact
stop_daemon
say "PASS in ${SECONDS}s: ${TIMINGS[*]}"
```

- [ ] **Step 3: Write `e2e/flat/run.sh`**

```bash
#!/usr/bin/env bash
# e2e/flat driver (spec §7, "Real-apt end-to-end"). For each client image
# in E2E_FLAT_IMAGES, builds e2e/flat/Dockerfile on it and runs the test
# container (nginx, the daemon and apt clients in one container).
#
#   E2E_FLAT_IMAGES           client images, whitespace-separated (default:
#                             the six of spec §7, oldest apt first)
#   E2E_FLAT_UNSIGNED_IMAGES  the images that also run the unsigned
#                             first-adoption scenario, which waits out its
#                             5-minute confirmation (default: the oldest
#                             and the newest)
#   E2E_FLAT_NAME             image and container name prefix
#                             (default acu-e2e-flat)
#
#   make e2e-flat
#   E2E_FLAT_IMAGES=debian:12 bash e2e/flat/run.sh
#
# Each image is independent: one failing does not stop the others. The
# exit status is non-zero iff any image failed.
set -euo pipefail
cd "$(dirname "$0")/../.."

# Whitespace-split the whole value (a CI block scalar spans lines; see
# e2e/deb/run.sh for why read -d '' is used).
IFS=$' \t\n' read -r -d '' -a images < <(
    printf '%s\0' "${E2E_FLAT_IMAGES:-debian:11 ubuntu:22.04 ubuntu:24.04 debian:12 debian:13 ubuntu:26.04}"
)
IFS=$' \t\n' read -r -d '' -a unsigned_images < <(
    printf '%s\0' "${E2E_FLAT_UNSIGNED_IMAGES:-debian:11 ubuntu:26.04}"
)
name="${E2E_FLAT_NAME:-acu-e2e-flat}"

if ((${#images[@]} == 0)); then
    echo "[e2e-flat] E2E_FLAT_IMAGES is empty; nothing to test"
    exit 2
fi
for img in "${images[@]}" "${unsigned_images[@]}"; do
    if ! [[ $img =~ ^(debian|ubuntu):[0-9]+(\.[0-9]+)?$ ]]; then
        echo "[e2e-flat] unsupported client image '$img' (want debian:N or ubuntu:NN.NN)"
        exit 2
    fi
done
if ! [[ $name =~ ^[a-z0-9][a-z0-9_.-]*$ ]]; then
    echo "[e2e-flat] invalid E2E_FLAT_NAME '$name'"
    exit 2
fi

failures=()
timings=()
for img in "${images[@]}"; do
    tag="${img/:/-}"
    unsigned_wait=0
    for u in "${unsigned_images[@]}"; do
        [[ $u == "$img" ]] && unsigned_wait=1
    done
    echo
    echo "==============================="
    echo "[e2e-flat] $img (unsigned first adoption: $unsigned_wait)"
    echo "==============================="
    start=$SECONDS
    if ! docker build -f e2e/flat/Dockerfile --build-arg CLIENT_IMAGE="$img" \
        -t "$name:$tag" .; then
        echo "[e2e-flat] $img FAIL (build)"
        failures+=("$img:build")
        continue
    fi
    built=$((SECONDS - start))
    if ! docker run --rm --name "$name-$tag" \
        -e E2E_IMAGE="$img" -e E2E_UNSIGNED_WAIT="$unsigned_wait" "$name:$tag"; then
        echo "[e2e-flat] $img FAIL (test)"
        failures+=("$img:test")
        continue
    fi
    timings+=("$img: build ${built}s, test $((SECONDS - start - built))s")
    echo "[e2e-flat] $img PASS"
done

echo
for t in "${timings[@]}"; do
    echo "[e2e-flat] $t"
done
if ((${#failures[@]} > 0)); then
    echo "[e2e-flat] OVERALL FAIL: ${failures[*]}"
    exit 1
fi
echo "[e2e-flat] OVERALL PASS (${images[*]})"
```

`chmod +x e2e/flat/run.sh e2e/flat/test.sh`.

- [ ] **Step 4: Add `make e2e-flat`**

In `Makefile`:

old:
```make
.PHONY: build test test-race lint fmt deb e2e deb-test apt-repo-test clean
```
new:
```make
.PHONY: build test test-race lint fmt deb e2e e2e-flat deb-test apt-repo-test clean
```

and after the `e2e:` target's recipe (`bash e2e/run.sh`), add (the recipe line starts with a tab):

```make

# Spec §7 real-apt end-to-end test of flat repositories: nginx, the daemon
# and apt clients in one container per client image (e2e/flat/README.md).
# E2E_FLAT_IMAGES selects the images (default: the six of spec §7).
e2e-flat:
	bash e2e/flat/run.sh
```
(Task 11 completes this comment.)

- [ ] **Step 5: Run the first scenario**

Run: `E2E_FLAT_IMAGES=debian:12 make e2e-flat`
Expected (the planning run; times vary):
```
[e2e-flat debian:12    0s] apt 2.6.1; fixtures key <40 hex>
[e2e-flat debian:12    0s] starting the daemon
[e2e-flat debian:12    1s] === scenario first-contact: every repository through one client: adoption, installs, request lines, strict mode
[e2e-flat debian:12   10s] --- scenario first-contact passed (9s)
[e2e-flat debian:12   10s] stopping the daemon
[e2e-flat debian:12   11s] PASS in 11s: first-contact 9s
[e2e-flat] debian:12 PASS

[e2e-flat] debian:12: build 20s, test 11s
[e2e-flat] OVERALL PASS (debian:12)
```

- [ ] **Step 6: Mutation checks (the harness has teeth)**

Run each, one at a time, reverting before the next:
1. In `write_config`, `refuse_unvouched_debs = true` → `false`; `E2E_FLAT_IMAGES=debian:12 make e2e-flat`.
   Expected: `FAIL in scenario 'first-contact': an undeclared .deb on signed.test answered 200, want 502 (strict mode)`.
2. In `first_contact`'s control, `grep -Eq "$APT_ERRORS" /clients/control/update.log` → `grep -Eq "no-such-apt-message" /clients/control/update.log`; rerun.
   Expected: the control update's output, ending `E: Failed to fetch http://nowhere.test/Packages  403  Forbidden [IP: 127.0.0.1 3142]` and `E: Some index files failed to download. …`, then `FAIL in scenario 'first-contact': the apt error grep did not match a failed update (above)`.

- [ ] **Step 7: Lint and commit**

Run: `shellcheck -x e2e/flat/*.sh`
Expected: no output.

```bash
git add e2e/flat/lib.sh e2e/flat/test.sh e2e/flat/run.sh Makefile
git commit -F - <<'EOF'
e2e/flat: the harness, make e2e-flat and the first-contact scenario

Every repository through one apt client: each signed suite adopts in its
upstream's form, the Release-less ones refresh without a Release, the
unsigned ones wait for confirmation; installs of every Filename shape are
vouched; apt's ./, %253a and by-hash/SHA512 request lines are pinned on
both sides; strict mode refuses an undeclared .deb; apt-file reads
Jitsi's Contents and deb-src its Sources.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

### Task 4: PR #8's end-to-end table, the older `Date` and the restart

Spec §9's H row and the overview's H line: "one signed flat repository configured as `deb <uri>/ /` on one client and `deb <uri> ./` on another; both report 2.0 within one cooldown of the v2 publish, and with the upstream stopped two fresh clients install 2.0 from the snapshots"; spec §7: "an older-Date publish is ignored; state survives a restart" (design note 5; Review Focus 2).

**Files:**
- Modify: `e2e/flat/test.sh`

**Interfaces:**
- Consumes: Task 3's `lib.sh` and `test.sh` names.
- Produces: the clients `k8s-slash` (`SRC_K8S`), `k8s-dot` (`SRC_K8S_DOT`), `fresh-slash`, `fresh-dot`; the twin suite `$K8S/.` (adopted inline); `k8s` published at v2 when this task's scenarios end (the older-`Date` scenario publishes v1 and restores v2); `snapshot_ids` and `main_candidates` (the restart's comparisons).

- [ ] **Step 1: Add the PR #8 and older-`Date` scenarios after `first_contact`**

```bash
# PR #8's end-to-end table (plan overview, "Cross-phase interfaces", H): one
# signed flat repository as `deb <uri>/ /` on one client and `deb <uri> ./`
# on another; both report 2.0 within one cooldown of the v2 publish. The
# upstream-stopped half runs in upstream_down.
pr8_table() {
    scenario pr8-table "one flat repository in both source forms; both see 2.0 after the v2 publish"
    client k8s-slash "$PROXY" "$SRC_K8S"
    client k8s-dot "$PROXY" "$SRC_K8S_DOT"
    apt_update k8s-slash
    apt_update k8s-dot -o Debug::Acquire::http=true
    apt_sent "$(re "http://signed.test$K8S/./InRelease")"
    apt_sent "$(re "http://signed.test$K8S/./Packages.gz")"
    poll 60 "the ./ twin suite to adopt" adopted_as signed.test "$K8S/." inline
    apt_install k8s-slash acu-k8s=1.0
    apt_install k8s-dot acu-k8s=1.0

    publish k8s v2
    until_candidates "$REACH" k8s-slash k8s-dot -- acu-k8s=2.0
    say "both source forms report acu-k8s 2.0 ${POLL_TOOK}s after the publish (cooldown ${COOLDOWN}s)"
    apt_install k8s-slash acu-k8s=2.0
    apt_install k8s-dot acu-k8s=2.0
    passed
}

# apt ignores an older Date; so does the cache (spec C5, upstream_older).
older_date() {
    scenario older-date "a publish with an older Date is refused; clients keep 2.0"
    publish k8s v1
    local s
    for s in "$K8S" "$K8S/."; do
        poll "$REACH" "signed.test$s to refuse the older Date" refused_older "$s"
    done
    apt_update k8s-slash
    apt_update k8s-dot
    candidates_are k8s-slash acu-k8s=2.0 || fail "k8s-slash went back to $(candidate k8s-slash acu-k8s)"
    candidates_are k8s-dot acu-k8s=2.0 || fail "k8s-dot went back to $(candidate k8s-dot acu-k8s)"
    publish k8s v2
    passed
}

# refused_older SUITE: the suite refused an older Date in this scenario;
# a client update triggers its check.
refused_older() {
    apt_update k8s-slash
    apt_update k8s-dot
    log_has "$SCENARIO_MARK" "$(event adoption_refused)" "$(suite_kv "$1")" "$(kv reason upstream_older)"
}
```

- [ ] **Step 2: Add the restart and upstream-down scenarios after them, above the run list**

```bash
# The snapshots survive a restart: the same snapshot ids, a clean update
# with the same candidates, and anchors served from the snapshots.
restart() {
    scenario restart "state survives a restart"
    local before after cands_before cands_after
    apt_update main
    before=$(snapshot_ids)
    cands_before=$(main_candidates)
    stop_daemon
    start_daemon
    after=$(snapshot_ids)
    [[ $before == "$after" ]] || fail "snapshots changed across the restart: $before -> $after"
    apt_update main
    cands_after=$(main_candidates)
    [[ $cands_before == "$cands_after" ]] || fail "candidates changed across the restart: $cands_before -> $cands_after"
    curl -fsS -D - -o /dev/null -x "$PROXY" "http://signed.test$K8S/InRelease" | grep -qi '^x-cache-snapshot:' ||
        fail "the k8s InRelease is not served from its snapshot after the restart"
    passed
}

# snapshot_ids: every signed suite's current snapshot id, one line.
snapshot_ids() {
    local s
    for s in "$K8S" "$K8S/." "$CUDA" "$NVIDIA" "$JENKINS" "$SUBLIME" "$JITSI" "$DISTS"; do
        printf '%s=%s ' "$s" "$(suite signed.test "$s" current_snapshot_id)"
    done
}

# main_candidates: the main client's candidate of every fixture package.
main_candidates() {
    local p
    for p in acu-k8s acu-cuda acu-epoch acu-nvidia acu-jenkins acu-jenkins-direct acu-sublime \
        acu-jitsi acu-dists acu-unsigned404 acu-unsigned403 acu-fury acu-goreleaser; do
        printf '%s=%s ' "$p" "$(candidate main "$p")"
    done
}

# PR #8's table, second half: with the upstream stopped, two fresh clients
# (one per source form) install 2.0 from the snapshots.
upstream_down() {
    scenario upstream-down "upstream stopped: fresh clients of both forms install 2.0 from the snapshots"
    stop_nginx
    client fresh-slash "$PROXY" "$SRC_K8S"
    client fresh-dot "$PROXY" "$SRC_K8S_DOT"
    apt_update fresh-slash
    apt_update fresh-dot
    apt_install fresh-slash acu-k8s=2.0
    apt_install fresh-dot acu-k8s=2.0
    start_nginx
    passed
}
```

- [ ] **Step 3: Extend the run list**

old:
```bash
first_contact
stop_daemon
```
new:
```bash
first_contact
pr8_table
older_date
restart
upstream_down
stop_daemon
```

- [ ] **Step 4: Run it**

Run: `E2E_FLAT_IMAGES=debian:12 make e2e-flat`
Expected, among the lines (the planning run):
```
[e2e-flat debian:12   11s] published k8s v2
[e2e-flat debian:12   16s] both source forms report acu-k8s 2.0 5s after the publish (cooldown 2s)
[e2e-flat debian:12   16s] --- scenario pr8-table passed (6s)
[e2e-flat debian:12   16s] published k8s v1
[e2e-flat debian:12   19s] published k8s v2
[e2e-flat debian:12   19s] --- scenario older-date passed (3s)
[e2e-flat debian:12   21s] --- scenario restart passed (2s)
[e2e-flat debian:12   22s] --- scenario upstream-down passed (1s)
[e2e-flat debian:12   23s] PASS in 23s: first-contact 9s pr8-table 6s older-date 3s restart 2s upstream-down 1s
```

- [ ] **Step 5: Mutation check**

Delete `pr8_table`'s `publish k8s v2` line; rerun.
Expected: `FAIL in scenario 'pr8-table': clients k8s-slash k8s-dot did not report acu-k8s=2.0 within 22s`. Restore it.

- [ ] **Step 6: Lint and commit**

Run: `shellcheck -x e2e/flat/*.sh` — no output.

```bash
git add e2e/flat/test.sh
git commit -F - <<'EOF'
e2e/flat: PR #8's end-to-end table, the older Date and the restart

Both source forms of one signed flat repository report 2.0 within one
cooldown of its v2 publish and, with the upstream stopped, fresh clients
of both forms install 2.0 from the snapshots; an older-Date publish is
refused; snapshots and candidates survive a restart.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

### Task 5: The frozen `Date` and the Release-less repositories (the phase G carry-forward)

Spec §7: "a v2 publish of a signed or anchorless repo reaches clients within one cooldown", the libnvidia-container shape (InRelease only, SHA512-only, frozen `Date`); the overview's *(G)* carry-forward: "a Release-less repository (a Gemfury-shaped `Packages.gz` with no `Release`): a republished index reaches a client within one `periodic_refresh`, and an upstream that then answers HTML keeps the old index" (design notes 5, 10, 11).

**Files:**
- Modify: `e2e/flat/test.sh`

**Interfaces:**
- Consumes: Task 3's names; Task 2's captive switch `/run/acu-e2e/captive`.
- Produces: `nvidia`, `goreleaser` and `fury` published at v2; `update_until_rejected`, `fury_clean`.

- [ ] **Step 1: Add the scenarios after `refused_older`**

```bash
# libnvidia-container's Date never changes: a new publish with the same
# Date adopts (spec C5: only an older Date is refused).
frozen_date() {
    scenario frozen-date "an InRelease-only, SHA512-only repository with a frozen Date publishes v2"
    publish nvidia v2
    until_candidates "$REACH" main -- acu-nvidia=2.0
    say "acu-nvidia 2.0 reached the client ${POLL_TOOK}s after the publish"
    apt_install main acu-nvidia=2.0
    log_has "$SCENARIO_MARK" "$(event request)" "$(kv path "$NVIDIA/./acu-nvidia_2.0_amd64.deb")" "$(kv validated_hash true)" ||
        fail "acu-nvidia 2.0 was not served vouched"
    passed
}

# Release-less repositories (spec C4 Dispatch 4) and the phase G
# carry-forward: a republished index reaches a client within one cooldown
# when clients ask, and within one periodic_refresh when none does; an
# upstream that then answers HTML keeps the old index.
anchorless() {
    scenario anchorless "Release-less repositories: republished indexes, then a captive portal"
    publish goreleaser v2
    until_candidates "$REACH" main -- acu-goreleaser=2.0
    say "acu-goreleaser 2.0 reached the client ${POLL_TOOK}s after the publish (cooldown ${COOLDOWN}s)"
    apt_install main acu-goreleaser=2.0

    publish fury v2
    # No client asks: the scheduler alone refreshes it.
    poll $((PERIODIC + TICK + 10)) "the scheduler's anchorless refresh of /fury" log_has "$SCENARIO_MARK" \
        "$(event freshness_check)" "$(suite_kv /fury)" "$(kv result anchorless_refreshed)" 'rows_replaced=[1-9]'
    say "the scheduler refreshed /fury ${POLL_TOOK}s after the publish (periodic_refresh ${PERIODIC}s)"
    apt_update main
    candidates_are main acu-fury=2.0 || fail "the client sees acu-fury $(candidate main acu-fury), want 2.0"
    apt_install main acu-fury=2.0

    say "the upstream now answers an HTML page for every /fury path"
    touch /run/acu-e2e/captive
    # A client during the outage keeps its index: the cache answers the
    # index from its copy, and the check an update triggers (once a
    # cooldown) rejects the portal's page. apt also reports the page it got
    # for the missing Release, as it would fetching directly.
    poll "$REACH" "an anchorless check to reject the HTML page" update_until_rejected
    candidates_are main acu-fury=2.0 || fail "behind the portal the client sees acu-fury $(candidate main acu-fury)"
    curl -fsS -x "$PROXY" http://plain.test/fury/Packages.gz | zcat | grep -q '^Version: 2.0$' ||
        fail "the cache no longer serves /fury's 2.0 Packages.gz behind the captive portal"
    rm /run/acu-e2e/captive
    # The portal's pages the outage's misses cached as InRelease and
    # Release are deleted by the next anchorless check, which the next
    # client update triggers (spec C4 Dispatch 4: an anchor answered 404).
    poll "$REACH" "/fury's updates to be clean after the portal" fury_clean
    passed
}

update_until_rejected() {
    apt_update main
    [[ -n $(metric 'acu_anchorless_sniff_rejected_total{host="plain.test"}') ]]
}

fury_clean() {
    apt_update main
    ! grep -E '^(E|Err):.*plain\.test/fury' "$APT_LOG" > /dev/null
}
```

- [ ] **Step 2: Extend the run list**

old:
```bash
older_date
restart
```
new:
```bash
older_date
frozen_date
anchorless
restart
```

- [ ] **Step 3: Run it**

Run: `E2E_FLAT_IMAGES=debian:12 make e2e-flat`
Expected, among the lines (the planning run):
```
[e2e-flat debian:12   ...] acu-nvidia 2.0 reached the client 2s after the publish
[e2e-flat debian:12   ...] acu-goreleaser 2.0 reached the client 5s after the publish (cooldown 2s)
[e2e-flat debian:12   45s] the scheduler refreshed /fury 21s after the publish (periodic_refresh 20s)
[e2e-flat debian:12   45s] the upstream now answers an HTML page for every /fury path
[e2e-flat debian:12   50s] --- scenario anchorless passed (32s)
[e2e-flat debian:12   54s] PASS in 54s: first-contact 7s pr8-table 6s older-date 2s frozen-date 2s anchorless 32s restart 2s upstream-down 1s
```
The scheduler's refresh takes up to `periodic_refresh` + one tick after the suite's last check (20–22 s in the planning runs); the 35 s bound is `PERIODIC + TICK + 10`.

- [ ] **Step 4: Mutation check**

Delete the `touch /run/acu-e2e/captive` line; rerun.
Expected: `FAIL in scenario 'anchorless': timed out after 22s waiting for an anchorless check to reject the HTML page`. Restore it.

- [ ] **Step 5: Lint and commit**

Run: `shellcheck -x e2e/flat/*.sh` — no output.

```bash
git add e2e/flat/test.sh
git commit -F - <<'EOF'
e2e/flat: the frozen Date and the Release-less repositories

A SHA512-only, InRelease-only repository whose Date never changes
publishes v2; a Release-less repository's republished index reaches a
client within one cooldown when clients ask and within one
periodic_refresh when none does, and an upstream that then answers HTML
keeps the old index (the phase G carry-forward).

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

### Task 6: Two suites flipping on one host, and mirror mode

Spec §7: "a mirror-mode run and a two-suites-one-host flip are included" (spec C8's shared client queue; design note 1). Both flips find their quiet window between the client's updates (`adoption_flip_barrier … outcome=held … held_requests=0` in the planning runs): the scenario proves a client sharing one connection across two suites that flip together sees no mismatch; a hold that holds requests is Task 7's.

**Files:**
- Modify: `e2e/flat/test.sh`

**Interfaces:**
- Consumes: Task 3's names.
- Produces: the clients `pair` and `mirror` (`DIRECT`: no proxy, every source on `$MIRROR`); `cuda`, `dists` and `sublime` published at v2.

- [ ] **Step 1: Add the scenarios after `fury_clean`**

```bash
# Two suites on one host flip at once while a client keeps updating both
# over its one connection to the host (spec C8).
two_suites() {
    scenario two-suites "two suites of one host flip together under a client's updates"
    client pair "$PROXY" "$SRC_CUDA" "$SRC_DISTS"
    apt_update pair
    publish cuda v2
    publish dists v2
    until_candidates 60 pair -- acu-cuda=2.0 acu-epoch=1:2.0 acu-dists=2.0
    say "both suites flipped; the client saw them ${POLL_TOOK}s after the publish"
    apt_update pair
    local s
    for s in "$CUDA" "$DISTS"; do
        log_has "$SCENARIO_MARK" "$(event adoption_flip_barrier)" "$(suite_kv "$s")" 'outcome=(held|quiet_cut_short)' ||
            fail "no held flip for $s: $(log_lines "$SCENARIO_MARK" "$(event adoption_flip_barrier)" "$(suite_kv "$s")")"
    done
    apt_install pair acu-cuda=2.0 acu-epoch=1:2.0 acu-dists=2.0 -- -o Debug::Acquire::http=true
    apt_sent "$(re "http://signed.test$CUDA/./acu-epoch_1%253a2.0_amd64.deb")"
    passed
}

# Mirror mode: every source on the cache's own address, so apt queues them
# all on one connection (spec C8, §3).
mirror_mode() {
    scenario mirror-mode "signed flat repositories through a [[mirror]] prefix, with a flip"
    client mirror DIRECT "deb [signed-by=$KEY] $MIRROR/ apt/stable/" \
        "deb [signed-by=$KEY] $MIRROR/jitsi stable/" \
        "deb [signed-by=$KEY] $MIRROR/debian-stable binary/" \
        "deb [signed-by=$KEY] $MIRROR/core:/stable:/v1.34/deb/ /"
    apt_update mirror -o Debug::Acquire::http=true
    # Without a proxy apt sends the origin form of the request line.
    apt_sent "$(re /m-signed/apt/stable/InRelease)"
    apt_install mirror acu-sublime=1.0 acu-jitsi=1.0 acu-jenkins=1.0 acu-k8s=2.0
    log_has "$SCENARIO_MARK" "$(event request)" "$(kv url /m-signed/files/acu-sublime_1.0_amd64.deb)" "$(kv validated_hash true)" ||
        fail "the mirror-mode Sublime .deb was not served vouched"
    publish sublime v2
    until_candidates "$REACH" mirror -- acu-sublime=2.0
    say "the mirror-mode client saw acu-sublime 2.0 ${POLL_TOOK}s after the publish"
    apt_install mirror acu-sublime=2.0
    passed
}
```

- [ ] **Step 2: Extend the run list**

old:
```bash
anchorless
restart
```
new:
```bash
anchorless
two_suites
mirror_mode
restart
```

- [ ] **Step 3: Run it**

Run: `E2E_FLAT_IMAGES=debian:12 make e2e-flat`
Expected, among the lines (the planning run):
```
[e2e-flat debian:12   ...] both suites flipped; the client saw them 5s after the publish
[e2e-flat debian:12   ...] --- scenario two-suites passed (8s)
[e2e-flat debian:12   ...] the mirror-mode client saw acu-sublime 2.0 2s after the publish
[e2e-flat debian:12   ...] --- scenario mirror-mode passed (6s)
[e2e-flat debian:12   70s] PASS in 70s: first-contact 8s pr8-table 6s older-date 1s frozen-date 3s anchorless 31s two-suites 8s mirror-mode 6s restart 4s upstream-down 1s
```

- [ ] **Step 4: Lint and commit**

Run: `shellcheck -x e2e/flat/*.sh` — no output.

```bash
git add e2e/flat/test.sh
git commit -F - <<'EOF'
e2e/flat: two suites flipping on one host, and mirror mode

Two suites of one host publish together while a client keeps updating
both over its one connection; a mirror-mode client (every source on the
cache's address) installs and sees a flip; no update shows an error.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

### Task 7: The 45 s commit

Spec §7: "A commit-delay hook of 45 s asserts no client timeouts, with its own scoped grep (the phase-B-expiry straddle is a documented residual)" (spec C8; design note 12). The daemon restarts with `ACU_E2E_COMMIT_DELAY=45s` (Task 1); a client updates every 3 s while the Jenkins suite flips; its updates are grepped for timeouts only (`APT_TIMEOUTS`), mismatches are counted and reported; the flip must end `hold_expired` and at least one held anchor request must have waited, none over 20 s. The daemon then restarts without the delay and a full-grep update must be clean.

**Files:**
- Modify: `e2e/flat/test.sh`

**Interfaces:**
- Consumes: Task 1's `e2e_commit_delay_enabled` line; Task 3's names; the request line's `flip_held_ms` and `adoption_flip_barrier`'s `outcome` (`docs/log-fields.md`).
- Produces: the client `delay`; `jenkins` published at v2.

- [ ] **Step 1: Add the scenario after `mirror_mode`**

```bash
# A commit slower than the flip barrier's 20 s hold (spec §7): no client
# times out; the straddle a phase-B expiry allows is a documented residual
# (spec C8), so this scenario's grep is scoped to timeouts.
commit_delay() {
    scenario commit-delay "a 45 s commit: held anchor requests never reach apt's timeout"
    stop_daemon
    start_daemon ACU_E2E_COMMIT_DELAY=45s
    log_has "$SCENARIO_MARK" "$(event e2e_commit_delay_enabled)" "$(kv commit_delay 45s)" ||
        fail "the daemon did not enable the 45 s commit delay (built without -tags e2ehooks?)"
    client delay "$PROXY" "$SRC_JENKINS" "$SRC_K8S" "$SRC_DISTS"
    apt_update delay
    publish jenkins v2
    local start=$SECONDS mismatches=0
    until log_has "$SCENARIO_MARK" "$(event adoption_success)" "$(suite_kv "$JENKINS")"; do
        ((SECONDS - start < 150)) || fail "the delayed jenkins adoption did not commit within 150s"
        apt_update delay --grep="$APT_TIMEOUTS"
        if grep -q 'Hash Sum mismatch' "$APT_LOG"; then
            mismatches=$((mismatches + 1))
        fi
        sleep 3
    done
    say "the delayed commit landed $((SECONDS - start))s after the publish; updates with the residual mismatch: $mismatches"
    log_has "$SCENARIO_MARK" "$(event adoption_flip_barrier)" "$(suite_kv "$JENKINS")" "$(kv outcome hold_expired)" ||
        fail "the jenkins flip did not end hold_expired: $(log_lines "$SCENARIO_MARK" "$(event adoption_flip_barrier)")"
    # apt asks a detached suite's InRelease first; the hold covers both
    # anchors (spec C8).
    local held max=0 ms
    held=$(log_lines "$SCENARIO_MARK" "$(event request)" "(^| )path=$(re "$JENKINS")/(InRelease|Release)( |\$)" \
        'flip_held_ms=' | grep -oE 'flip_held_ms=[0-9]+' | cut -d= -f2 || true)
    [[ -n $held ]] || fail "no jenkins InRelease or Release request waited in the hold"
    for ms in $held; do ((ms > max)) && max=$ms; done
    ((max <= 20000)) || fail "a request waited ${max} ms in a hold, over 20 s"
    say "held requests: $(wc -w <<< "$held"), longest wait ${max} ms"
    stop_daemon
    start_daemon
    # The residual clears with the next update: no error of any kind.
    apt_update delay
    candidates_are delay acu-jenkins=2.0 || fail "the client sees acu-jenkins $(candidate delay acu-jenkins)"
    apt_install delay acu-jenkins=2.0 acu-jenkins-direct=2.0
    passed
}
```

- [ ] **Step 2: Extend the run list**

old:
```bash
mirror_mode
restart
```
new:
```bash
mirror_mode
commit_delay
restart
```

- [ ] **Step 3: Run it**

Run: `E2E_FLAT_IMAGES=debian:12 make e2e-flat`
Expected, among the lines (the planning runs: the commit landed 49–50 s after the publish; one held request each time, 17.8–17.9 s; no mismatch):
```
[e2e-flat debian:12   ...] starting the daemon with ACU_E2E_COMMIT_DELAY=45s
[e2e-flat debian:12   ...] published jenkins v2
[e2e-flat debian:12   ...] the delayed commit landed 50s after the publish; updates with the residual mismatch: 0
[e2e-flat debian:12   ...] held requests: 1, longest wait 17853 ms
[e2e-flat debian:12   ...] --- scenario commit-delay passed (53s)
[e2e-flat debian:12  122s] PASS in 122s: first-contact 8s pr8-table 7s older-date 1s frozen-date 3s anchorless 30s two-suites 8s mirror-mode 6s commit-delay 53s restart 2s upstream-down 2s
```
The held request is the detached suite's `InRelease` (apt asks it first; the hold covers both anchors): `msg=request … path=/debian-stable/binary/InRelease outcome=hit status=404 … flip_held_ms=17906`, then `adoption_flip_barrier … suite_path=/debian-stable/binary … outcome=hold_expired … hold_seconds=20.0… held_requests=1`.

- [ ] **Step 4: Mutation check**

Replace `start_daemon ACU_E2E_COMMIT_DELAY=45s` with `start_daemon`; rerun.
Expected: `FAIL in scenario 'commit-delay': the daemon did not enable the 45 s commit delay (built without -tags e2ehooks?)`. Restore it.

- [ ] **Step 5: Lint and commit**

Run: `shellcheck -x e2e/flat/*.sh` — no output.

```bash
git add e2e/flat/test.sh
git commit -F - <<'EOF'
e2e/flat: a 45 s commit times out no client

With the e2ehooks commit delay at 45 s, a flip's hold expires at 20 s;
no apt update times out (a scoped grep: the phase-B-expiry straddle is a
documented residual), the held requests waited under 20 s, and the next
update after the commit is clean.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

### Task 8: The unsigned first adoption and the whole-run assertions

Spec §7: "The unsigned first-adoption scenario (5-minute confirmation) runs on the oldest and newest images only", and "zero `unvouched_deb_refused` on the signed host" over the whole run (design notes 7, 8; deviation 2). The confirming observation is the record's first retry at least 5 minutes after the first one: the scheduler's probes in between see the same bytes before the retry time and stay `changed_deferred` (spec C7's gate, `checkFirst`'s `deferred` in `internal/freshness/first_adoption.go`), and the retries come 2, 4 and 8 minutes apart, each ±25 %. So the adoption lands 300–900 s after first contact (the planning runs' times are in Task 9), and the deadline is 900 s + `periodic_refresh` + one tick + 30 s. Spec §7's "5-minute confirmation" is the floor, not the time: the spec's behaviour by controller ruling R-H2-6.

**Files:**
- Modify: `e2e/flat/test.sh`

**Interfaces:**
- Consumes: `UNSIGNED_FIRST_CONTACT` (Task 3), `poll_quietly`, `suite`, `until_candidates`; the status JSON's `adoption_fail_*` fields.
- Produces: `unsigned404` published at v2 on the long-wait images; `unsigned_record`.

- [ ] **Step 1: Add the unsigned scenario after `commit_delay`**

```bash
# The unsigned first adoption waits for a second observation of the same
# Release at least 5 minutes after the first (spec C5): run on the oldest
# and newest images only (spec §7).
#
# AIDEV-NOTE: the confirming observation is the first retry of the
# unsigned_unconfirmed record that falls 5 minutes or more after the first
# observation: the checks in between see the same bytes before the
# record's retry time and stay changed_deferred (spec C7's gate). The
# retries come 2, then 4, then 8 minutes apart, each ±25 %, so the
# adoption lands between 300 s and 900 s after first contact (design
# note 7); the deadline adds a refresh interval, a tick and 30 s.
UNSIGNED_DEADLINE=$((900 + PERIODIC + TICK + 30))
unsigned_first_adoption() {
    if [[ ${E2E_UNSIGNED_WAIT:-0} != 1 ]]; then
        say "=== scenario unsigned-first-adoption: skipped on this image (E2E_UNSIGNED_WAIT unset)"
        return 0
    fi
    scenario unsigned-first-adoption "an unsigned repository adopts after the 5-minute confirmation"
    local left=$((UNSIGNED_FIRST_CONTACT + UNSIGNED_DEADLINE - SECONDS))
    ((left > 0)) || left=1
    if ! poll_quietly "$left" adopted_as plain.test /unsigned404 unsigned; then
        fail "plain.test/unsigned404 did not adopt within ${UNSIGNED_DEADLINE}s of its first contact: $(unsigned_record)"
    fi
    say "plain.test/unsigned404 adopted $((SECONDS - UNSIGNED_FIRST_CONTACT))s after its first contact"
    # Updates of an established unsigned repository adopt at once.
    publish unsigned404 v2
    until_candidates "$REACH" main -- acu-unsigned404=2.0
    apt_install main acu-unsigned404=2.0
    # The 403 variant needs an hour between its observations.
    [[ -z $(suite plain.test /unsigned403 current_snapshot_id) ]] ||
        fail "plain.test/unsigned403 adopted before its one-hour confirmation"
    [[ $(suite plain.test /unsigned403 adoption_fail_reason) == unsigned_unconfirmed ]] ||
        fail "plain.test/unsigned403 is not waiting for its confirmation"
    passed
}

# unsigned_record: /unsigned404's failure record, from the status JSON.
unsigned_record() {
    curl -fsS "$ADMIN/?format=json" | jq -c '.suites[] | select(.suite_path == "/unsigned404") |
        {adoption_fail_reason, adoption_fail_count, adoption_fail_first_unixtime,
         adoption_next_retry_unixtime, last_check_unixtime, now: now | floor}'
}
```

- [ ] **Step 2: Add the whole-run assertions after `upstream_down`**

```bash
# Whole-run assertions over the daemon's log.
whole_run() {
    scenario whole-run "log-wide assertions"
    local refused
    refused=$(log_lines 0 "$(event unvouched_deb_refused)" "$(kv canonical_host signed.test)" |
        grep -Ev -- "$(kv path "$K8S/amd64/acu-rogue_1.0_amd64.deb")" || true)
    [[ -z $refused ]] || fail "unvouched_deb_refused on signed.test: $refused"
    # plain.test has no signed snapshot, so strict mode never applies there
    # (spec C10: unsigned and anchorless suites never count).
    refused=$(log_lines 0 "$(event unvouched_deb_refused)" "$(kv canonical_host plain.test)")
    [[ -z $refused ]] || fail "unvouched_deb_refused on plain.test: $refused"
    local bad
    bad=$(log_lines 0 '(^| )msg=[a-z_]*panicked( |$)|(^| )msg=package_hash_conflict( |$)')
    [[ -z $bad ]] || fail "panics or conflicts in the daemon log: $bad"
    kill -0 "$DAEMON_PID" || fail "the daemon is not running"
    passed
}
```

- [ ] **Step 3: Extend the run list**

old:
```bash
commit_delay
restart
```
new:
```bash
commit_delay
unsigned_first_adoption
restart
```

old:
```bash
upstream_down
stop_daemon
```
new:
```bash
upstream_down
whole_run
stop_daemon
```

The file now equals the planning run's `test.sh`; its run list reads `setup`, `first_contact`, `pr8_table`, `older_date`, `frozen_date`, `anchorless`, `two_suites`, `mirror_mode`, `commit_delay`, `unsigned_first_adoption`, `restart`, `upstream_down`, `whole_run`, `stop_daemon`, `say "PASS …"`.

- [ ] **Step 4: Run it on a short image and on a long-wait image**

Run: `E2E_FLAT_IMAGES=debian:12 make e2e-flat`
Expected: `=== scenario unsigned-first-adoption: skipped on this image (E2E_UNSIGNED_WAIT unset)`, `--- scenario whole-run passed (0s)`, `PASS in 1xxs` (119–122 s in the planning runs).

Run: `E2E_FLAT_IMAGES=debian:11 make e2e-flat`
Expected: `plain.test/unsigned404 adopted <300–900>s after its first contact`, `--- scenario unsigned-first-adoption passed`, `PASS in …s` (Task 9's table).

- [ ] **Step 5: Mutation check**

In `whole_run`, delete the `grep -Ev -- "$(kv path "$K8S/amd64/acu-rogue_1.0_amd64.deb")" || true` filter (keep `|| true`); rerun on `debian:12`.
Expected: `FAIL in scenario 'whole-run': unvouched_deb_refused on signed.test: level=INFO msg=unvouched_deb_refused canonical_host=signed.test path=/core:/stable:/v1.34/deb/amd64/acu-rogue_1.0_amd64.deb current_snapshot_count=7` (the planning run met exactly this line when the filter was a basic-regex `grep -v`). Restore it.

- [ ] **Step 6: Lint and commit**

Run: `shellcheck -x e2e/flat/*.sh` — no output.

```bash
git add e2e/flat/test.sh
git commit -F - <<'EOF'
e2e/flat: the unsigned first adoption and the whole-run assertions

On the oldest and newest images an unsigned repository adopts at its
first retry 5 minutes after first contact and then updates at once; its
403 variant still waits. Over the whole run no .deb was refused on the
signed host but the deliberate one, none on the unsigned host, and the
daemon logged no panic or hash conflict.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```


### Task 9: Definition of done: all six images, measured

Spec §9's H row: "`make e2e-flat` green on all images with the grep assertions, including PR #8's end-to-end table"; the brief's DoD adds the Release-less carry-forward and every §7 fixture and assertion, or a recorded deviation. This task changes no file: it runs the whole matrix and the Go gates, and checks the map below.

**Files:** none.

**Interfaces:**
- Consumes: everything Tasks 1–8 produce.
- Produces: the measured times (for Task 10's timeouts and Task 11's README).

- [ ] **Step 1: Run every image**

Run: `make e2e-flat`
Expected: each image ends `[e2e-flat] <image> PASS`, and the run ends:
```
[e2e-flat] debian:11: build …s, test …s
[e2e-flat] ubuntu:22.04: build …s, test …s
[e2e-flat] ubuntu:24.04: build …s, test …s
[e2e-flat] debian:12: build …s, test …s
[e2e-flat] debian:13: build …s, test …s
[e2e-flat] ubuntu:26.04: build …s, test …s
[e2e-flat] OVERALL PASS (debian:11 ubuntu:22.04 ubuntu:24.04 debian:12 debian:13 ubuntu:26.04)
```

The planning runs (2026-10-24, the scratch tree at 6786529 with this plan's files; one runner, Docker 29.8.2):

| Image | apt | Test | Unsigned adoption after first contact | Test-stage build, cold |
|---|---|---|---|---|
| `debian:11` | 2.2.4 | 320 s, 410 s, 397 s | 310 s, 397 s, 385 s | 29 s |
| `ubuntu:22.04` | 2.4.14 | 125 s, 127 s | — | 51–58 s |
| `ubuntu:24.04` | 2.8.3 | 123 s, 124 s | — | 43 s |
| `debian:12` | 2.6.1 | 119–135 s (four full runs) | — | 20 s (43 s with the Go stage cold) |
| `debian:13` | 3.0.3 | 122 s, 123 s | — | 30 s |
| `ubuntu:26.04` | 3.2.0 | 385 s | 373 s | 33 s |

"Test" is the harness's own `PASS in` time; `run.sh`'s `test …s` adds the container's start and stop (2–3 s).

Per scenario (all images): `first-contact` 7–9 s, `pr8-table` 5–7 s, `older-date` 1–3 s, `frozen-date` 2–4 s, `anchorless` 29–35 s (mostly the scheduler's 20 s), `two-suites` 7–9 s, `mirror-mode` 5–6 s, `commit-delay` 53–58 s, `restart` 2–4 s, `upstream-down` 0–2 s, `whole-run` 0 s; `unsigned-first-adoption` waits until the adoption (above). The full sequential `make e2e-flat` took 1,291 s of tests plus 91 s of mostly cached builds (about 23 minutes). Worst case: the two long-wait images at their 955 s deadline, about 17 minutes each, and the four others about 2 minutes plus a cold build each — about 45 minutes sequentially.

- [ ] **Step 2: The Go and lint gates**

Run: `gofmt -l . && go vet ./... && golangci-lint run ./... && golangci-lint run --build-tags e2ehooks ./cmd/... && go test -race -count=1 -timeout 30m ./... && shellcheck -x e2e/flat/*.sh`
Expected: `gofmt -l` lists only the five drift files; `0 issues.` twice; every package `ok`; shellcheck prints nothing.

- [ ] **Step 3: Check the map — every item of spec §7 and §9's H row, and where it is asserted**

| Requirement | Where |
|---|---|
| `e2e/flat/`, `make e2e-flat`, the single-container pattern, in-container restart, `current -> v1\|v2` flips | Tasks 2–3 (`Dockerfile`, `run.sh`, `start_daemon`/`stop_daemon`, `publish`) |
| The six client images, `docker-*` apt.conf removed, images chosen by environment | `Dockerfile` (`rm -f /etc/apt/apt.conf.d/docker-*`), `run.sh` (`E2E_FLAT_IMAGES`); Step 1 |
| k8s (`amd64/…`, 403 for missing) | `first-contact` (vouched install; `README` answers 403) |
| CUDA (`./…`, an epoch `%3a` package) | `first-contact` (pins, vouched installs), `two-suites` (the 2.0 `%253a` pin) |
| libnvidia-container (InRelease only, SHA512-only, frozen `Date`) | `first-contact` (inline adoption, vouched), `frozen-date` |
| Jenkins (`sub/`, detached, `direct/`) | `first-contact` (detached adoption, both `.deb`s vouched), `commit-delay` |
| Sublime (`files/`, host-root base) | `first-contact` (vouched), `mirror-mode` |
| Jitsi (by-hash SHA256+SHA512, binary `Release.gpg`, `Contents-amd64` via apt-file, `deb-src`) | `first-contact` (`by-hash/SHA512` pin, `apt-file search`, `apt-cache showsrc`); deviation 1 |
| Unsigned (404 and 403 variants) | `first-contact` (`unsigned_unconfirmed`, installs), `unsigned-first-adoption`; deviation 2 |
| Release-less (Gemfury shape) | `first-contact`, `anchorless` |
| A `dists/` repository | `first-contact`, `two-suites` |
| Signed repositories on one host, unsigned and Release-less on a second | design note 1; `first-contact` (`acu-rogue` 502), `whole-run` |
| A mirror-mode run; a two-suites-one-host flip | `mirror-mode`; `two-suites` |
| Installs succeed | every `apt_install` |
| Request lines pinned (`./`, `%253a`, `by-hash/SHA512`) | `first-contact`, `pr8-table`, `two-suites` |
| `apt-get` output grepped (exit status useless) | `apt_update`; the control update in `first-contact` |
| Zero `unvouched_deb_refused` on the signed host | `whole-run` (with `first-contact`'s lower bound) |
| A v2 publish of a signed repository reaches clients within one cooldown | `pr8-table`, `frozen-date`, `two-suites`, `mirror-mode` (design note 5) |
| A v2 publish of an anchorless repository reaches clients within one cooldown | `anchorless` (`goreleaser`) |
| An older-`Date` publish is ignored | `older-date` |
| State survives a restart | `restart` (and the two restarts of `commit-delay`) |
| A 45 s commit delay, no client timeouts, a scoped grep | `commit-delay` (Task 1's seam) |
| The unsigned first adoption on the oldest and newest images only | `unsigned-first-adoption` (`E2E_FLAT_UNSIGNED_IMAGES`; design note 7) |
| PR #8's end-to-end table | `pr8-table`, `upstream-down` |
| The *(G)* carry-forward (a Release-less repository: within one `periodic_refresh`; HTML keeps the old index) | `anchorless` (`fury`) |
| The CI change | Task 10 |
| The docs | Task 11 |
| CI's `deb-test` green for the RC (controller's addition) | Task 12 |

- [ ] **Step 4: Know what the planning runs fixed** (so a reviewer can tell a harness regression from a daemon one; none was a daemon bug, ruling R-H2-4)

1. apt's `Debug::Acquire::http` lines end in CRLF: `apt_sent` strips CRs before matching.
2. Without a proxy (mirror mode) apt sends the origin form, `GET /m-signed/apt/stable/InRelease`, not an absolute URL.
3. apt asks a detached suite's `InRelease` first, and the hold covers it: the held request in `commit-delay` is `…/binary/InRelease` (404 from the snapshot), so the scenario reads both anchors' `flip_held_ms`.
4. An inline snapshot answers its suite's `Release` 404 itself (spec §10 item 9): the 403 check asks `README`, a path no snapshot owns.
5. `grep -v` is a basic regex: `whole-run`'s exclusion of `acu-rogue` uses `grep -Ev` (the miss showed exactly the line Task 8's mutation check expects).
6. The sniff-rejection poll first waited for the scheduler (20 s, a REACH of 22 s): it now runs an update per poll, so the check comes from the client within a cooldown.
7. The unsigned deadline first assumed 5 minutes (design note 7).
8. The `sub/` and `a/b/` fixtures' `Release` files need a `Suite`, or apt warns "Conflicting distribution".
9. Jitsi's fixture first had no `InRelease` (deviation 1).
10. `debian:11`'s packages left `deb.debian.org` (design note 9); `nginx` became `nginx-light` (fewer mirror fetches, as e2e/deb uses).
11. v1 and v2 got distinct mtimes (design note 4).

- [ ] **Step 5: No commit** (nothing changed). If a run fails, ruling R-H2-4 applies: a harness bug is fixed in the task that owns the file; a daemon bug gets its own task (a failing Go test first, the fix, the scenario that caught it), never a weakened assertion.

### Task 10: CI: one `e2e-flat` job per image

Ruling R-H2-3 and design note 13.

**Files:**
- Modify: `.github/workflows/ci.yaml`

**Interfaces:**
- Consumes: `e2e/flat/run.sh`'s `E2E_FLAT_IMAGES` (its default `E2E_FLAT_UNSIGNED_IMAGES`, `debian:11 ubuntu:26.04`, picks the two long-wait jobs).
- Produces: the job `e2e-flat` (six matrix jobs, `fail-fast: false`).

- [ ] **Step 1: Add the job**

In `.github/workflows/ci.yaml`, before the comment `  # Fast unit tests for the apt-repo publishing scripts`, add:

```yaml
  # Spec §7 real-apt end-to-end test of flat repositories
  # (e2e/flat/README.md): nginx, the daemon and apt clients in one
  # container, one job per client image so every apt version (2.2 to 3.2)
  # runs on every change. debian:11 and ubuntu:26.04 also wait out the
  # unsigned confirmation (5 to 15 minutes after first contact); the others
  # take about 2 minutes plus the image build. No retry: a flaky scenario
  # must show. A failure that names a distribution mirror is re-run alone.
  e2e-flat:
    runs-on: ubuntu-latest
    strategy:
      fail-fast: false
      matrix:
        include:
          - { image: 'debian:11', timeout: 30 }
          - { image: 'ubuntu:22.04', timeout: 15 }
          - { image: 'ubuntu:24.04', timeout: 15 }
          - { image: 'debian:12', timeout: 15 }
          - { image: 'debian:13', timeout: 15 }
          - { image: 'ubuntu:26.04', timeout: 30 }
    timeout-minutes: ${{ matrix.timeout }}
    steps:
      - uses: actions/checkout@v5
      - run: bash e2e/flat/run.sh
        env:
          E2E_FLAT_IMAGES: ${{ matrix.image }}
```

- [ ] **Step 2: Validate the workflow**

Run: `go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.7 .github/workflows/ci.yaml && echo clean`
Expected: `clean` (the planning run; actionlint checks `timeout-minutes`' matrix expression and runs shellcheck on the `run:` lines).

Run: `python3 -c "import yaml; j = yaml.safe_load(open('.github/workflows/ci.yaml'))['jobs']['e2e-flat']; print(j['timeout-minutes'], [m['image'] for m in j['strategy']['matrix']['include']])"`
Expected: `${{ matrix.timeout }} ['debian:11', 'ubuntu:22.04', 'ubuntu:24.04', 'debian:12', 'debian:13', 'ubuntu:26.04']`

The job itself runs when the branch is next pushed; nothing here pushes. Expect each short job to take about 4–6 minutes on a GitHub runner (image pulls, a cold build, about 2 minutes of test) and each long-wait job 10–20.

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/ci.yaml
git commit -F - <<'EOF'
ci: e2e-flat, one job per client image

Runs e2e/flat/run.sh for each of spec §7's six client images, so every
apt version from 2.2 to 3.2 is exercised on every change; the oldest and
newest images also wait out the unsigned confirmation.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

### Task 11: Documentation: `e2e/flat/README.md`, the Makefile help, the overview, and the list for H3

The brief's docs DoD: how to run it, image selection, what each scenario proves; the Makefile help; a "for H3" list (below, in this plan: H3 writes the README and the release notes). This task also marks the two overview items this phase closes.

**Files:**
- Create: `e2e/flat/README.md`
- Modify: `Makefile` (the `e2e-flat` comment), `docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md`

**Interfaces:**
- Consumes: the scenario names, variables and measured times of Tasks 3–9.
- Produces: no code.

- [ ] **Step 1: Write `e2e/flat/README.md`**

````markdown
# e2e/flat: real apt against flat repositories

This end-to-end test runs real apt clients against fixture copies of the
repository shapes apt-cacher-ultra must handle — flat repositories in every
`sources.list` form, unsigned and Release-less ones, and a `dists/` one —
through the cache, on six client images whose apt versions span 2.2 to 3.2
(spec §7, "Real-apt end-to-end"; the design is
`docs/superpowers/specs/2026-09-27-flat-repositories-design.md`). It uses
fixtures only: nothing reaches a live repository.

## Running it

```sh
make e2e-flat                              # all six images, one after another
E2E_FLAT_IMAGES=debian:12 make e2e-flat    # one image
E2E_FLAT_IMAGES='debian:11 ubuntu:26.04' bash e2e/flat/run.sh
```

| Variable | Default | Meaning |
|---|---|---|
| `E2E_FLAT_IMAGES` | `debian:11 ubuntu:22.04 ubuntu:24.04 debian:12 debian:13 ubuntu:26.04` | client images to test, oldest apt first |
| `E2E_FLAT_UNSIGNED_IMAGES` | `debian:11 ubuntu:26.04` | images that also run the unsigned first adoption (5 to 15 minutes more) |
| `E2E_FLAT_NAME` | `acu-e2e-flat` | prefix of the image tags and container names |

It needs Docker with BuildKit and network access to Docker Hub
(`golang:1.25-bookworm`, `debian:bookworm-slim`, the client images), the
distribution mirrors the image build installs from and the Go module proxy.
Each image is built and run on its own; one failing does not stop the
others, and the exit status is non-zero if any failed.

Time: about 2 minutes per image plus its build (about a minute cold); the
two images that wait for the unsigned confirmation take 5 to 15 minutes
more (`apt` 2.2.4 on `debian:11`, 3.2.0 on `ubuntu:26.04`). CI runs one job
per image (`.github/workflows/ci.yaml`, job `e2e-flat`).

## How it works

One container per client image (`Dockerfile`):

- `build` compiles the daemon with `-tags e2ehooks`, which adds one test
  seam: `ACU_E2E_COMMIT_DELAY` makes every adoption commit wait that long
  (`cmd/apt-cacher-ultra/e2ehooks.go`). Release builds never contain it.
- `fixtures` runs `build-fixtures.sh`: every repository in two versions
  (`/fixtures/<repo>/v1`, `v2`; packages `acu-<repo>` at `1.0` and `2.0`),
  signed with a throwaway key generated there, and the web roots that link
  each repository's URL path to `/fixtures/<repo>/current -> v1`. A
  scenario publishes a version by flipping that symlink.
- `test` is the client image with nginx, curl, jq and apt-file; its
  `docker-*` apt.conf files are removed, so apt behaves as on a host.

`test.sh` (with `lib.sh`) starts nginx (`nginx.conf`) as the upstream for
two host names — `signed.test` for the signed repositories, `plain.test`
for the unsigned and Release-less ones — and the daemon with short timers
(`cooldown` 2 s, `periodic_refresh` 20 s, `flip_quiet_period` 1 s, strict
mode on). Each apt "client" is a state directory under `/clients`,
selected with `APT_CONFIG`. Assertions read apt's output (`apt-get
update`'s exit status says nothing, so it is grepped for `Failed to
fetch|Connection failed|timed out|Hash Sum mismatch|Mirror sync in
progress`) and its `Debug::Acquire::http` request lines, nginx's access
log, the daemon's log events, the admin status JSON and `/metrics`. Every
wait is a bounded poll; a failure prints the scenario, the daemon's log
tail and nginx's.

## The fixtures

| Path | Reproduces | Shape |
|---|---|---|
| `signed.test/core:/stable:/v1.34/deb` | pkgs.k8s.io | `deb <uri>/ /`, Filenames `amd64/…`, `InRelease` only, 403 for a missing file; plus `acu-rogue`, on the upstream but in no `Packages` |
| `signed.test/cuda/ubuntu2204/x86_64` | NVIDIA CUDA | `deb <uri>/ /`, Filenames `./…`, an epoch package in a file named `…_1%3a<v>_amd64.deb` |
| `signed.test/libnvidia-container/stable/deb/amd64` | libnvidia-container | `deb <uri> /`, `InRelease` only, a SHA512-only `Release`, `Date` frozen at 2018 |
| `signed.test/debian-stable` | pkg.jenkins.io | `deb <uri> binary/`, detached, Filenames `binary/…` and `direct/…` |
| `signed.test/apt/stable` + `signed.test/files` | download.sublimetext.com | `deb <host-root> apt/stable/`, Filenames `files/…` at the host root |
| `signed.test/jitsi` | download.jitsi.org | `deb <uri> stable/` and `deb-src`, `Acquire-By-Hash`, SHA256 and SHA512, `Contents-amd64`, a binary `Release.gpg` beside `InRelease` |
| `signed.test/debian` | a `dists/` archive | `deb <uri> acu main`, inline |
| `plain.test/unsigned404`, `plain.test/unsigned403` | an unsigned repository | `deb [trusted=yes] <uri>/ /`, `Release` only; missing files 404, or 403 |
| `plain.test/fury`, `plain.test/goreleaser` | apt.fury.io, repo.goreleaser.com | `deb [trusted=yes] <uri>/ /`, `Packages` only, no `Release` |

## The scenarios

They run in this order and share one cache.

| Scenario | Proves |
|---|---|
| `first-contact` | One client with every repository: each signed suite adopts in its upstream's form (inline, or detached for Jenkins), the Release-less ones refresh without a `Release`, the unsigned ones wait for confirmation. Installs of every Filename shape succeed and are vouched (`validated_hash=true`). apt's `./`, `%253a` and `by-hash/SHA512` request lines are pinned, apt's and the cache's upstream ones. apt-file finds Jitsi's `Contents-amd64`; `deb-src` reads its `Sources`. Strict mode refuses `acu-rogue` (502), so "no refusal" elsewhere means something; a refused host's update must trip the error grep, so the grep means something on this apt. |
| `pr8-table` | PR #8's end-to-end table: one signed flat repository as `deb <uri>/ /` on one client and `deb <uri> ./` (its own suite, the twin) on another; after the v2 publish both report 2.0 within one cooldown (bounded at cooldown + 20 s; 2–6 s measured). |
| `older-date` | A publish with an older `Date` is refused (`upstream_older`) for both twins; clients keep 2.0. |
| `frozen-date` | libnvidia-container's v2, with the same frozen `Date`, adopts and installs vouched. |
| `anchorless` | A Release-less repository's republished index reaches a client within one cooldown when it asks (`goreleaser`) and within one `periodic_refresh` when no client does (`fury`, the scheduler alone); behind a captive portal (every path an HTML page with 200) the old index stays — the cache counts the rejected page and still serves the 2.0 `Packages.gz` — and after the portal the pages its misses cached are deleted by the next check. |
| `two-suites` | Two suites of one host publish together while a client updates both over one connection: no update shows a mismatch; both flips are held. |
| `mirror-mode` | A client with every source behind the cache's `[[mirror]]` prefix (one apt queue for all) installs and sees a flip cleanly. |
| `commit-delay` | With a 45 s commit (`ACU_E2E_COMMIT_DELAY`), the flip's hold expires at 20 s: anchor requests waited under 20 s and no update timed out; a mismatch here is the documented phase-B-expiry residual, so this scenario's grep is scoped to timeouts; the first update after the commit is clean. |
| `unsigned-first-adoption` | (`E2E_FLAT_UNSIGNED_IMAGES` only) The unsigned repository adopts at the first retry of its record at least 5 minutes after first contact (5 to 15 minutes), then updates at once; the 403 variant still waits (its gap is an hour). |
| `restart` | After a restart: the same snapshots, the same candidates, a clean update, anchors served from the snapshots. |
| `upstream-down` | PR #8's table, second half: with nginx stopped, fresh clients of both source forms install 2.0 from the snapshots. |
| `whole-run` | No `unvouched_deb_refused` on `signed.test` but `acu-rogue`, none on `plain.test`, no panic and no `package_hash_conflict` in the daemon's log. |

Two shapes of spec §7 are exercised only in part: apt (2.2 to 3.2) refuses
a binary detached `Release.gpg` ("unsupported binary format"), so Jitsi's
binary signature sits beside an `InRelease` that apt uses; and the 403
unsigned variant's first adoption needs an hour, past any bounded run. Go
tests cover both (`TestSniffReleaseGPG`,
`TestCheckFirst_UnsignedAfterADeniedSignatureWaitsAnHour`).

## Debugging a failure

The failure message names the scenario and prints the last lines of the
daemon's log and of nginx's access log. To look around inside, keep the
container:

```sh
docker build -f e2e/flat/Dockerfile --build-arg CLIENT_IMAGE=debian:12 -t acu-e2e-flat:debian-12 .
docker run -d --name acu-e2e-flat-debug acu-e2e-flat:debian-12 sleep infinity
docker exec -e E2E_IMAGE=debian:12 acu-e2e-flat-debug bash /usr/local/lib/acu-e2e/test.sh
docker exec -it acu-e2e-flat-debug bash   # /var/log/acu-e2e, /clients/<name>/*.log
docker rm -f acu-e2e-flat-debug
```

The daemon's log carries an `msg=e2e_harness scenario=<name> unixtime=…`
line at the start of each scenario. `APT_CONFIG=/clients/<name>/apt.conf
apt-get update` repeats a client's update.

## Maintenance

- **An image past its end of life.** When a release's packages move to an
  archive mirror, its image build fails installing nginx (`404  Not Found`
  from the mirror). `debian:11` is already there: the `Dockerfile` points
  its `sources.list` at `archive.debian.org` for `bullseye`. Do the same
  for the next one (`old-releases.ubuntu.com` for Ubuntu).
- **A transient mirror failure** fails one image's build; re-run that
  image.
- **Fixtures are dated relative to the build** (v1 two days before, v2 one
  day before, no `Valid-Until`), so a cached image stays valid.
- **Adding a scenario**: a function in `test.sh` that starts with
  `scenario NAME DESCRIPTION`, ends with `passed`, waits only through
  `poll` or `until_candidates`, and is added to the run list at the end of
  the file. Assert on snake_case log events, the status JSON or
  `/metrics`, not on prose log messages.
````

- [ ] **Step 2: Complete the Makefile help**

old:
```make
# Spec §7 real-apt end-to-end test of flat repositories: nginx, the daemon
# and apt clients in one container per client image (e2e/flat/README.md).
# E2E_FLAT_IMAGES selects the images (default: the six of spec §7).
e2e-flat:
```
new:
```make
# Spec §7 real-apt end-to-end test of flat repositories: nginx, the daemon
# and apt clients in one container per client image (e2e/flat/README.md).
# Default images: debian:11 ubuntu:22.04 ubuntu:24.04 debian:12 debian:13
# ubuntu:26.04, about 2 minutes each plus the build; debian:11 and
# ubuntu:26.04 also wait 5-15 minutes for the unsigned confirmation.
# Override with E2E_FLAT_IMAGES (and E2E_FLAT_UNSIGNED_IMAGES).
e2e-flat:
```

- [ ] **Step 3: Mark the overview items this phase closes**

In `docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md` (as it stands after H1):

old:
```
H also runs the live-upstream smoke and documents the strict-mode consequences listed under "PR #8 parity" below.
```
new:
```
H also runs the live-upstream smoke and documents the strict-mode consequences listed under "PR #8 parity" below. The table is phase H2's `pr8-table` and `upstream-down` scenarios (`…-H2-e2e.md`, Task 4); the smoke and the documentation are H3's.
```

old:
```
a republished index reaches a client within one `periodic_refresh`, and an upstream that then answers HTML keeps the old index.
```
new:
```
a republished index reaches a client within one `periodic_refresh`, and an upstream that then answers HTML keeps the old index. *Done in phase H2 (`…-H2-e2e.md`, Task 5: the `anchorless` scenario).*
```

- [ ] **Step 4: Verify**

Run: `grep -c 'e2e-flat' Makefile && grep -n 'Done in phase H2' docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md && git diff --check`
Expected: `2` (the `.PHONY` line and the target); one overview line; `git diff --check` prints nothing.

Read `e2e/flat/README.md` against `test.sh`: every scenario in the run list has a row, in order, and every variable `run.sh` reads is in the table.

- [ ] **Step 5: Commit**

```bash
git add e2e/flat/README.md Makefile docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md
git commit -F - <<'EOF'
e2e/flat: README, the Makefile help, and the overview's H2 items

How to run e2e/flat, choose images and read a failure; what each
scenario proves; the fixtures' real-repository shapes. The overview
marks PR #8's end-to-end table and the Release-less carry-forward done.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

### Task 12: e2e/deb after eager first adoption (CI's `deb-test`)

The controller's addition: CI's `deb-test` must be green for the 1.1.0 RC, and the fix belongs in `e2e/deb` itself. At 6786529 `make deb-test` fails on `ubuntu:24.04` (the planning run): phases 1–3 pass, then `FAIL: phase-4 hot_count=0, want >= 1 (Stage 1 should mine the V2 hot pair)`. The script assumes "No adoption fires here" at phase 4's cold prime, but eager first adoption (spec C4 Dispatch 3, phase E) adopts V1 there (`freshness_check … result=first_adoption`, snapshot 1). So what the script calls "V2 adopted snapshot id=1" is V1; the `.deb` it then requests (1.1) is in no prior snapshot; the real V2 adoption, delayed about 10 s by the flip barrier's default quiet period, is what it reads as "V1 (2nd)"; and the V2 → V1 swap it was built on would now be refused anyway (spec C5: `rollback_refused`, or `upstream_older`). A harness bug, not a daemon bug: the replay below passes with the daemon unchanged (ruling R-H2-4 does not apply).

The same assumption makes phases 2 and 3 pass for the wrong snapshot: each polls for "a current snapshot", which the eager V1 adoption satisfies within 2 s, and the daemon is stopped before the change-driven V2 adoption (about 11 s later) commits — so neither proves "adopts a snapshot when upstream's InRelease changes" any more. This task restores all three:

- `wait_snapshot COLUMN FILE LIMIT` (new helper) waits until the suite's current snapshot records a given anchor's exact SHA-256 (`inrelease_hash` or `release_hash`); `dump_adoption_state` prints the rows and the log tail on a failed wait.
- Phases 2 and 3 first wait for the eager first adoption of V1 (by its hash), then swap to V2, trigger the check, and wait for V2's hash (60 s, room for the barrier's 10 s quiet period).
- Phase 4 runs V1 → V2: the cold prime's eager adoption of V1 is the first adoption; the client requests V1's `.deb`; publishing V2 drives the second adoption, whose prior snapshot is V1's. It proves what it proved before — a second-cycle adoption in which Stage 1 mines the hot pair `(hello-acu, amd64)` from the prior snapshot and hot prefetch warms the new snapshot's version (`hot_count >= 1`, `fetched >= 1`, `failed=0`, `mismatched=0`; the warmed `.deb`'s url_path row with `blob_hash` and `last_fetched_at`) — and one thing more: the warmed 1.1 row has `request_count = 0`, so it came from the prefetch, not from a client (`CommitAdoption` inserts prefetched rows with `last_requested_at NULL, request_count 0`, `internal/cache/adoption.go`).

**Files:**
- Modify: `e2e/deb/test.sh` (phases 2–4 and two helpers; `e2e/deb/Dockerfile`, `run.sh` and `e2e/upstream/build-repo.sh` are unchanged)

**Interfaces:**
- Consumes: the image layout of `e2e/deb/Dockerfile` (`/fixture-v1`, `/fixture-v2`, `/fixture-v1-detached`, `/fixture-v2-detached`, `sqlite3`), the cache's `suite_freshness.current_snapshot_id`, `suite_snapshot.{inrelease_hash,release_hash}` and `url_path.{last_requested_at,request_count,blob_hash,last_fetched_at}`, and the event `adoption_hot_prefetch_complete`.
- Produces: `wait_snapshot` (sets `SNAPSHOT_ID`, `SNAPSHOT_AFTER`), `dump_adoption_state`.

- [ ] **Step 1: Watch it fail**

Run: `UBUNTU_VERSIONS=24.04 make deb-test`
Expected: `[deb-test] phase 1 … PASS`, `phase 2 … PASS`, `phase 3 … PASS`, then `FAIL: phase-4 hot_count=0, want >= 1 (Stage 1 should mine the V2 hot pair)` and `[deb-test] OVERALL FAIL: 24.04:test`.

- [ ] **Step 2: Apply the change to `e2e/deb/test.sh`**

The complete change, as a patch against 6786529 (save it and apply it with `git apply`, or edit the hunks in by hand):

```diff
--- a/e2e/deb/test.sh
+++ b/e2e/deb/test.sh
@@ -129,6 +129,46 @@
     echo "WARN: :3142 still reachable 5s after stop_daemon"
 }
 
+# wait_snapshot COLUMN FILE LIMIT: polls once a second, LIMIT times,
+# until /dists/noble's current snapshot records FILE's SHA-256 in
+# COLUMN (inrelease_hash or release_hash). Sets SNAPSHOT_ID and
+# SNAPSHOT_AFTER (seconds) on success; returns 1 on timeout.
+#
+# AIDEV-NOTE: eager first adoption (flat-repositories spec C4 Dispatch
+# 3) adopts a suite at its first contact, so "some snapshot is current"
+# no longer says which upstream version was adopted: every wait names
+# the exact anchor bytes it expects.
+wait_snapshot() {
+    local column=$1 file=$2 limit=$3 want got i
+    want="$(sha256sum "$file" | cut -d' ' -f1)"
+    SNAPSHOT_ID=""
+    for i in $(seq 1 "$limit"); do
+        got=$(sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
+            "SELECT f.current_snapshot_id || '|' || IFNULL(s.${column}, '') FROM suite_freshness f JOIN suite_snapshot s ON s.snapshot_id = f.current_snapshot_id WHERE f.suite_path='/dists/noble' LIMIT 1;" \
+            2>/dev/null || echo "")
+        if [[ -n "$got" && "${got#*|}" == "$want" ]]; then
+            SNAPSHOT_ID="${got%%|*}"
+            SNAPSHOT_AFTER=$i
+            return 0
+        fi
+        sleep 1
+    done
+    return 1
+}
+
+# dump_adoption_state: the suite and snapshot rows and the daemon log
+# tail, for a failed wait.
+dump_adoption_state() {
+    echo "=== suite_freshness ==="
+    sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
+        "SELECT canonical_scheme, canonical_host, suite_path, current_snapshot_id, inrelease_change_seen_at FROM suite_freshness;" || true
+    echo "=== suite_snapshot ==="
+    sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
+        "SELECT snapshot_id, suite_path, inrelease_hash, release_hash, release_gpg_hash, adopted_at FROM suite_snapshot;" || true
+    echo "=== daemon log tail ==="
+    tail -100 "$daemon_log" || true
+}
+
 echo "[deb-test] launching daemon (same ExecStart as systemd unit)"
 runuser -u apt-cacher-ultra -- \
     /usr/sbin/apt-cacher-ultra --config /etc/apt-cacher-ultra/config.toml \
@@ -185,9 +225,11 @@
 #
 # Restart the daemon under an adoption-mode config and verify the
 # cache adopts a snapshot when upstream's signed InRelease changes.
-# Adoption only fires on changed InRelease bytes (freshness.go:455);
-# the fixture provides V1 + V2 trees signed with the same key, and
-# we drive a swap between two metadata GETs.
+# The fixture provides V1 + V2 trees signed with the same key. The
+# first GET of V1 adopts it at once (eager first adoption, flat-
+# repositories spec C4 Dispatch 3); swapping to V2 and GETting the
+# InRelease again drives a second, change-driven adoption, which this
+# phase waits for by V2's exact InRelease bytes.
 # ============================================================
 
 echo
@@ -305,6 +347,18 @@
 fi
 echo "[deb-test] V1 InRelease: $(wc -c < /tmp/inrelease.v1) bytes"
 
+# 2a': that miss created the suite and started its first check, which
+# adopts V1 at once (eager first adoption). Wait for it, so the V2
+# change below is checked against an adopted suite, not raced by the
+# first adoption still holding the suite.
+echo "[deb-test] phase 2: wait for the eager first adoption of V1"
+if ! wait_snapshot inrelease_hash /fixture-v1/dists/noble/InRelease 30; then
+    echo "FAIL: V1 was not adopted at first contact within 30s"
+    dump_adoption_state
+    exit 1
+fi
+echo "[deb-test] V1 adopted at first contact: snapshot id=$SNAPSHOT_ID (after ${SNAPSHOT_AFTER}s)"
+
 # 2b: swap upstream to V2 (different .deb version → different
 # Packages SHA256 → different Release content → different InRelease
 # bytes). Sleep > cooldown (1s) so the next request's freshness
@@ -315,10 +369,10 @@
 cp -r /fixture-v2/dists /var/www/html/
 cp -r /fixture-v2/pool /var/www/html/
 
-# 2c: drive a second InRelease GET. The cache hits its url_path row
-# from 2a, fires maybeFireFreshness, the freshness check sees changed
+# 2c: drive a second InRelease GET. The cache serves it from the V1
+# snapshot, fires maybeFireFreshness, the freshness check sees changed
 # bytes, and spawns the adoption goroutine.
-echo "[deb-test] phase 2: GET InRelease via proxy (cache hit → fires freshness check → adoption)"
+echo "[deb-test] phase 2: GET InRelease via proxy (snapshot hit → fires freshness check → adoption)"
 status=$(curl -sS -x http://127.0.0.1:3142 \
     -o /tmp/inrelease.trigger \
     -w '%{http_code}' \
@@ -329,36 +383,18 @@
     exit 1
 fi
 
-# 2d: poll suite_freshness for current_snapshot_id. Adoption is
-# async; the response in 2c was served from the V1 cache (it doesn't
-# wait for adoption to commit).
-echo "[deb-test] phase 2: poll suite_freshness for current_snapshot_id"
-adopted=0
-adopted_id=""
-for i in $(seq 1 30); do
-    val=$(sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
-        "SELECT IFNULL(current_snapshot_id, '') FROM suite_freshness WHERE suite_path='/dists/noble' LIMIT 1;" \
-        2>/dev/null || echo "")
-    if [[ -n "$val" ]]; then
-        adopted=1
-        adopted_id="$val"
-        echo "[deb-test] adopted snapshot id=$val (after ${i}s)"
-        break
-    fi
-    sleep 1
-done
-if [[ $adopted -eq 0 ]]; then
-    echo "FAIL: no snapshot adopted within 60s"
-    echo "=== suite_freshness ==="
-    sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
-        "SELECT canonical_scheme, canonical_host, suite_path, current_snapshot_id, inrelease_change_seen_at FROM suite_freshness;" || true
-    echo "=== suite_snapshot ==="
-    sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
-        "SELECT snapshot_id, canonical_host, suite_path, adopted_at FROM suite_snapshot;" || true
-    echo "=== daemon log tail ==="
-    tail -100 "$daemon_log" || true
+# 2d: poll for the V2 snapshot. Adoption is async (the response in 2c
+# came from the V1 snapshot), and the flip barrier first waits for the
+# suite's InRelease serves to go quiet (adoption.flip_quiet_period,
+# 10 s by default).
+echo "[deb-test] phase 2: poll for the V2 snapshot"
+if ! wait_snapshot inrelease_hash /fixture-v2/dists/noble/InRelease 60; then
+    echo "FAIL: V2 was not adopted within 60s"
+    dump_adoption_state
     exit 1
 fi
+adopted_id="$SNAPSHOT_ID"
+echo "[deb-test] adopted V2 snapshot id=$adopted_id (after ${SNAPSHOT_AFTER}s)"
 
 # 2e: a follow-up GET should now serve via snapshot_member resolution
 # and set the X-Cache-Snapshot response header (SPEC2 §6.1 / §10.1).
@@ -390,21 +426,15 @@
 # SPEC2 §7.6.3 phase 3: detached-mode adoption smoke
 #
 # Re-runs the phase-2 flow with detached fixtures (Release +
-# Release.gpg, no InRelease). Validates that the freshness checker
-# falls back from the inline path to detached when no InRelease
-# url_path row exists, and that adoption produces a snapshot whose
-# release_hash is set and inrelease_hash is nil.
+# Release.gpg, no InRelease). Validates that a suite whose upstream
+# publishes no InRelease is adopted in the detached form — at first
+# contact, by the probe tree (InRelease absent → Release →
+# Release.gpg: flat-repositories spec C4 Dispatch 3), then again when
+# its Release changes — and that the adopted snapshot's release_hash
+# is set and inrelease_hash is nil.
 #
 # A clean cache directory is required: phase 2 already adopted an
-# inline snapshot. After the wipe there is no InRelease url_path
-# row alongside the seeded Release rows, so the inline checker
-# takes the missing-InRelease-url_path branch (freshness.go
-# checkLockedInline → fallback=true) and the freshness goroutine
-# runs RunDetached. The companion 404 fallback branch (cached
-# InRelease url_path row + 404 from upstream + nil current
-# snapshot) is covered by unit tests in freshness_test.go; the
-# e2e harness only needs to confirm one fallback flavor end-to-end
-# because both branches converge on the same RunDetached call.
+# inline snapshot.
 # ============================================================
 
 echo
@@ -476,6 +506,16 @@
 done
 echo "[deb-test] V1 Release: $(wc -c < /tmp/Release.v1d) bytes; Release.gpg: $(wc -c < /tmp/Release.gpg.v1d) bytes"
 
+# 3a': the Release miss created the suite; its first check adopts
+# V1-detached at once (eager first adoption, detached form).
+echo "[deb-test] phase 3: wait for the eager first adoption of V1-detached"
+if ! wait_snapshot release_hash /fixture-v1-detached/dists/noble/Release 30; then
+    echo "FAIL: V1-detached was not adopted at first contact within 30s"
+    dump_adoption_state
+    exit 1
+fi
+echo "[deb-test] V1-detached adopted at first contact: snapshot id=$SNAPSHOT_ID (after ${SNAPSHOT_AFTER}s)"
+
 # 3b: swap to V2-detached.
 echo "[deb-test] phase 3: swap nginx webroot to V2-detached"
 sleep 2
@@ -483,16 +523,13 @@
 cp -r /fixture-v2-detached/dists /var/www/html/
 cp -r /fixture-v2-detached/pool /var/www/html/
 
-# 3c: drive a Release GET. Cache hits its url_path row, fires the
-# freshness check. detectForm sees no current_snapshot_id, defaults
-# to inline. checkLockedInline tries to look up InRelease url_path,
-# finds none, sees Release rows exist → returns (nil, true) /
-# fallback. checkLockedDetached fetches Release (200, changed),
-# fetches Release.gpg, builds an adoptionRequest with
-# form=detached. The Check goroutine routes to RunDetached, which
-# verifies the detached signature and commits a snapshot with
-# release_hash + release_gpg_hash set.
-echo "[deb-test] phase 3: GET Release via proxy (cache hit → fires freshness → detached fallback → adoption)"
+# 3c: drive a Release GET. The cache serves it from the V1-detached
+# snapshot and fires the freshness check: the current snapshot is
+# detached, so the check probes InRelease (absent), then GETs Release
+# (200, changed) and Release.gpg, and the adoption goroutine runs
+# RunDetached, which verifies the detached signature and commits a
+# snapshot with release_hash + release_gpg_hash set.
+echo "[deb-test] phase 3: GET Release via proxy (snapshot hit → fires freshness → detached adoption)"
 status=$(curl -sS -x http://127.0.0.1:3142 \
     -o /tmp/Release.trigger \
     -w '%{http_code}' \
@@ -503,34 +540,15 @@
     exit 1
 fi
 
-# 3d: poll for adoption. Same poll window as phase 2.
-echo "[deb-test] phase 3: poll suite_freshness for current_snapshot_id"
-adopted=0
-adopted_id=""
-for i in $(seq 1 30); do
-    val=$(sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
-        "SELECT IFNULL(current_snapshot_id, '') FROM suite_freshness WHERE suite_path='/dists/noble' LIMIT 1;" \
-        2>/dev/null || echo "")
-    if [[ -n "$val" ]]; then
-        adopted=1
-        adopted_id="$val"
-        echo "[deb-test] adopted detached snapshot id=$val (after ${i}s)"
-        break
-    fi
-    sleep 1
-done
-if [[ $adopted -eq 0 ]]; then
-    echo "FAIL: no detached snapshot adopted within 30s"
-    echo "=== suite_freshness ==="
-    sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
-        "SELECT canonical_scheme, canonical_host, suite_path, current_snapshot_id, inrelease_change_seen_at FROM suite_freshness;" || true
-    echo "=== suite_snapshot ==="
-    sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
-        "SELECT snapshot_id, suite_path, inrelease_hash, release_hash, release_gpg_hash, adopted_at FROM suite_snapshot;" || true
-    echo "=== daemon log tail ==="
-    tail -100 "$daemon_log" || true
+# 3d: poll for the V2-detached snapshot. Same window as phase 2.
+echo "[deb-test] phase 3: poll for the V2-detached snapshot"
+if ! wait_snapshot release_hash /fixture-v2-detached/dists/noble/Release 60; then
+    echo "FAIL: V2-detached was not adopted within 60s"
+    dump_adoption_state
     exit 1
 fi
+adopted_id="$SNAPSHOT_ID"
+echo "[deb-test] adopted detached snapshot id=$adopted_id (after ${SNAPSHOT_AFTER}s)"
 
 # 3e: assert the snapshot is in detached form — release_hash is set,
 # release_gpg_hash is set, inrelease_hash is NULL. This is the
@@ -590,34 +608,36 @@
 # wiring; this section proves the same wiring works from the
 # packaged binary running under the packaged config schema.
 #
-# Flow (V1 → V2 → V1 chain so two adoptions actually fire):
+# Flow (V1 → V2: the first adoption happens at first contact):
 #   1. Stop, wipe cache, stage V1 fixture, restart with Phase 3
 #      config (hot_packages.window, adoption.hot_prefetch_budget).
-#   2. Prime cache by GETting V1 InRelease (cold-cache miss). No
-#      adoption fires here — there's no current_snapshot_id yet
-#      and no upstream divergence to drive freshness.
-#   3. Swap nginx webroot to V2. Drive a second InRelease GET. The
-#      cached V1 bytes diverge from upstream V2 → freshness fires
-#      → V2 adoption commits. This is the FIRST adoption
-#      (prior_snapshot_id was NULL → hot_count=0).
-#   4. GET the V2 .deb so url_path.last_requested_at on
-#      hello-acu_1.1_amd64.deb is recorded (V1=1.0, V2=1.1 per the
-#      Dockerfile's repo-build VER args) — that's the (Package,
-#      Arch) signal SPEC3 §7.5.3 Stage 1 mines on the next
-#      adoption.
-#   5. Swap nginx webroot back to V1. Drive a third InRelease GET.
-#      Cached V2 bytes diverge from upstream V1 → V1 adoption
-#      fires. prior_snapshot_id = V2's id → hot pair (hello-acu,
-#      amd64) is hot → Stage 2 maps it to V1's
-#      hello-acu_1.0_amd64.deb → hot prefetch warms it.
-#   6. Assert:
-#        - adoption_hot_prefetch_complete fires for the V1 snapshot
+#   2. Prime the cache by GETting V1 InRelease (cold-cache miss). The
+#      miss creates the suite and its first check adopts V1 at once
+#      (eager first adoption, flat-repositories spec C4 Dispatch 3).
+#      This is the FIRST adoption (no prior snapshot → hot_count=0).
+#   3. GET the V1 .deb so url_path.last_requested_at on
+#      hello-acu_1.0_amd64.deb is recorded — the (Package, Arch)
+#      signal SPEC3 §7.5.3 Stage 1 mines on the next adoption.
+#   4. Swap nginx webroot to V2 (hello-acu 1.1) and GET InRelease.
+#      The changed InRelease drives the SECOND adoption, whose prior
+#      snapshot is V1's: Stage 1 finds the hot pair (hello-acu,
+#      amd64) requested from it, Stage 2 maps it to V2's
+#      hello-acu_1.1_amd64.deb, and hot prefetch warms it before the
+#      flip.
+#   5. Assert:
+#        - adoption_hot_prefetch_complete fires for the V2 snapshot
 #          with hot_count >= 1, fetched >= 1, failed=0,
 #          mismatched=0;
-#        - url_path row for hello-acu_1.0_amd64.deb exists with a
-#          non-null blob_hash AND last_fetched_at (proves
-#          CommitAdoption wrote the prefetched row in the flip
-#          transaction per SPEC3 §7.5.1).
+#        - url_path row for hello-acu_1.1_amd64.deb exists with a
+#          non-null blob_hash AND last_fetched_at, and no client has
+#          requested it (request_count = 0): CommitAdoption wrote
+#          the prefetched row in the flip transaction per SPEC3
+#          §7.5.1.
+#
+# AIDEV-NOTE: until eager first adoption this phase ran V1 → V2 → V1
+# because the cold prime adopted nothing; now the cold prime is the
+# first adoption, and going back to V1 would be refused (spec C5:
+# rollback_refused / upstream_older), so the second cycle is V1 → V2.
 # ============================================================
 
 echo
@@ -683,9 +703,8 @@
     exit 1
 fi
 
-# 4a: cold-cache prime with V1 InRelease. The freshness check fires
-# only on subsequent hits, so this MISS just establishes a url_path
-# row pointing at the V1 bytes; no adoption fires yet.
+# 4a: cold-cache prime with V1 InRelease. The miss establishes the
+# url_path row and creates the suite; its first check adopts V1.
 echo "[deb-test] phase 4: cold prime — GET V1 InRelease via proxy"
 status=$(curl -sS -x http://127.0.0.1:3142 \
     -o /tmp/p4.inrelease.v1 \
@@ -697,121 +716,75 @@
     exit 1
 fi
 
-# 4b: swap upstream to V2 so the next freshness check sees changed
-# bytes (cached=V1, upstream=V2). Sleep > cooldown.
-echo "[deb-test] phase 4: swap nginx webroot to V2 (drives V2 adoption)"
-sleep 2
-rm -rf /var/www/html/*
-cp -r /fixture-v2/dists /var/www/html/
-cp -r /fixture-v2/pool /var/www/html/
-
-echo "[deb-test] phase 4: GET InRelease to trigger V2 adoption (1st adoption)"
-status=$(curl -sS -x http://127.0.0.1:3142 \
-    -o /tmp/p4.inrelease.trigger.v2 \
-    -w '%{http_code}' \
-    --max-time 10 \
-    http://127.0.0.1/dists/noble/InRelease || echo "000")
-if [[ "$status" != "200" ]]; then
-    echo "FAIL: phase-4 V2 trigger InRelease returned $status"
-    exit 1
-fi
-
-echo "[deb-test] phase 4: poll for V2 adoption (1st)"
-v2_id=""
-for i in $(seq 1 30); do
-    val=$(sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
-        "SELECT IFNULL(current_snapshot_id, '') FROM suite_freshness WHERE suite_path='/dists/noble' LIMIT 1;" \
-        2>/dev/null || echo "")
-    if [[ -n "$val" ]]; then
-        v2_id="$val"
-        echo "[deb-test] phase 4 V2 adopted snapshot id=$v2_id (after ${i}s)"
-        break
-    fi
-    sleep 1
-done
-if [[ -z "$v2_id" ]]; then
-    echo "FAIL: phase-4 V2 adoption did not flip current_snapshot_id within 30s"
-    tail -100 "$daemon_log" || true
+echo "[deb-test] phase 4: wait for V1's first adoption (eager, at first contact)"
+if ! wait_snapshot inrelease_hash /fixture-v1/dists/noble/InRelease 30; then
+    echo "FAIL: phase-4 V1 was not adopted at first contact within 30s"
+    dump_adoption_state
     exit 1
 fi
-
-# 4c: hit the V2 .deb so SPEC3 §7.5.3 Stage 1 has a hot pair to
-# mine on the next adoption. The (Package=hello-acu, Arch=amd64)
-# tuple is recorded with last_requested_at via the Phase 1 fetch
-# path (no snapshot prefetch happened in 4b — this was the first
-# adoption with prior_snapshot_id=NULL, hot_count=0).
-# V2 fixture is version 1.1 (V1 is 1.0); see e2e/deb/Dockerfile
-# repo-build args (VER=1.1).
-V2_DEB_PATH="/pool/main/h/hello-acu/hello-acu_1.1_amd64.deb"
-echo "[deb-test] phase 4: GET V2 .deb to record url_path.last_requested_at"
+v1_id="$SNAPSHOT_ID"
+echo "[deb-test] phase 4 V1 adopted snapshot id=$v1_id (after ${SNAPSHOT_AFTER}s)"
+
+# 4b: hit the V1 .deb so SPEC3 §7.5.3 Stage 1 has a hot pair to mine
+# from the V1 snapshot on the next adoption. The (Package=hello-acu,
+# Arch=amd64) tuple is recorded with last_requested_at.
+V1_DEB_PATH="/pool/main/h/hello-acu/hello-acu_1.0_amd64.deb"
+echo "[deb-test] phase 4: GET V1 .deb to record url_path.last_requested_at"
 status=$(curl -sS -x http://127.0.0.1:3142 \
-    -o /tmp/p4.deb.v2 \
+    -o /tmp/p4.deb.v1 \
     -w '%{http_code}' \
     --max-time 10 \
-    "http://127.0.0.1${V2_DEB_PATH}" || echo "000")
+    "http://127.0.0.1${V1_DEB_PATH}" || echo "000")
 if [[ "$status" != "200" ]]; then
-    echo "FAIL: phase-4 V2 deb GET returned $status"
+    echo "FAIL: phase-4 V1 deb GET returned $status"
     exit 1
 fi
-v2_lr=$(sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
-    "SELECT IFNULL(last_requested_at, '') FROM url_path WHERE path='${V2_DEB_PATH}';" \
+v1_lr=$(sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
+    "SELECT IFNULL(last_requested_at, '') FROM url_path WHERE path='${V1_DEB_PATH}';" \
     2>/dev/null || echo "")
-if [[ -z "$v2_lr" ]]; then
-    echo "FAIL: phase-4 V2 .deb url_path.last_requested_at not recorded"
+if [[ -z "$v1_lr" ]]; then
+    echo "FAIL: phase-4 V1 .deb url_path.last_requested_at not recorded"
     sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
         "SELECT path, last_requested_at, request_count FROM url_path;" || true
     exit 1
 fi
 
-# 4d: swap upstream BACK to V1 so the next freshness check fires
-# again (cached InRelease bytes are V2's after 4b adoption; upstream
-# is V1's again). This drives the SECOND adoption — the one whose
-# hot-set Stage 1 query finds the (hello-acu, amd64) hot pair from
-# the V2 snapshot.
-echo "[deb-test] phase 4: swap nginx webroot back to V1 (drives V1 adoption + hot prefetch)"
+# 4c: swap upstream to V2 so the next freshness check sees changed
+# bytes (current snapshot V1, upstream V2). Sleep > cooldown.
+echo "[deb-test] phase 4: swap nginx webroot to V2 (drives V2 adoption + hot prefetch)"
 sleep 2
 rm -rf /var/www/html/*
-cp -r /fixture-v1/dists /var/www/html/
-cp -r /fixture-v1/pool /var/www/html/
+cp -r /fixture-v2/dists /var/www/html/
+cp -r /fixture-v2/pool /var/www/html/
 
-echo "[deb-test] phase 4: GET InRelease to trigger V1 adoption (2nd adoption + hot prefetch)"
+echo "[deb-test] phase 4: GET InRelease to trigger V2 adoption (2nd adoption + hot prefetch)"
 status=$(curl -sS -x http://127.0.0.1:3142 \
-    -o /tmp/p4.inrelease.trigger.v1 \
+    -o /tmp/p4.inrelease.trigger.v2 \
     -w '%{http_code}' \
     --max-time 10 \
     http://127.0.0.1/dists/noble/InRelease || echo "000")
 if [[ "$status" != "200" ]]; then
-    echo "FAIL: phase-4 V1 trigger InRelease returned $status"
+    echo "FAIL: phase-4 V2 trigger InRelease returned $status"
     exit 1
 fi
 
-echo "[deb-test] phase 4: poll for V1 adoption (2nd)"
-v1_id=""
-for i in $(seq 1 30); do
-    val=$(sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
-        "SELECT IFNULL(current_snapshot_id, '') FROM suite_freshness WHERE suite_path='/dists/noble' LIMIT 1;" \
-        2>/dev/null || echo "")
-    if [[ -n "$val" && "$val" != "$v2_id" ]]; then
-        v1_id="$val"
-        echo "[deb-test] phase 4 V1 adopted snapshot id=$v1_id (after ${i}s; prior was $v2_id)"
-        break
-    fi
-    sleep 1
-done
-if [[ -z "$v1_id" ]]; then
-    echo "FAIL: phase-4 V1 adoption did not flip current_snapshot_id within 30s (still v2_id=$v2_id)"
-    tail -100 "$daemon_log" || true
+echo "[deb-test] phase 4: poll for V2 adoption (2nd)"
+if ! wait_snapshot inrelease_hash /fixture-v2/dists/noble/InRelease 60; then
+    echo "FAIL: phase-4 V2 adoption did not flip the suite within 60s (still v1_id=$v1_id)"
+    dump_adoption_state
     exit 1
 fi
+v2_id="$SNAPSHOT_ID"
+echo "[deb-test] phase 4 V2 adopted snapshot id=$v2_id (after ${SNAPSHOT_AFTER}s; prior was $v1_id)"
 
-# 4e: assert SPEC3 §10.2 adoption_hot_prefetch_complete event fired
-# for the V1 snapshot with hot_count >= 1, fetched >= 1, failed=0,
+# 4d: assert SPEC3 §10.2 adoption_hot_prefetch_complete event fired
+# for the V2 snapshot with hot_count >= 1, fetched >= 1, failed=0,
 # mismatched=0. This is the "hot pair was warmed" oracle.
-echo "[deb-test] phase 4: assert adoption_hot_prefetch_complete fired for snapshot $v1_id"
+echo "[deb-test] phase 4: assert adoption_hot_prefetch_complete fired for snapshot $v2_id"
 hot_line=$(grep "msg=adoption_hot_prefetch_complete" "$daemon_log" \
-    | grep "snapshot_id=$v1_id" | tail -1)
+    | grep "snapshot_id=$v2_id" | tail -1)
 if [[ -z "$hot_line" ]]; then
-    echo "FAIL: no adoption_hot_prefetch_complete log line for snapshot $v1_id"
+    echo "FAIL: no adoption_hot_prefetch_complete log line for snapshot $v2_id"
     echo "=== relevant log tail ==="
     grep -E "hot_prefetch|adoption_" "$daemon_log" | tail -30 || true
     exit 1
@@ -825,11 +798,11 @@
 failed=$(printf '%s\n' "$hot_line" | grep -oE '(^| )failed=[0-9]+' | tr -dc '0-9')
 mismatched=$(printf '%s\n' "$hot_line" | grep -oE '(^| )mismatched=[0-9]+' | tr -dc '0-9')
 if [[ "${hot_count:-0}" -lt 1 ]]; then
-    echo "FAIL: phase-4 hot_count=$hot_count, want >= 1 (Stage 1 should mine the V2 hot pair)"
+    echo "FAIL: phase-4 hot_count=$hot_count, want >= 1 (Stage 1 should mine the V1 hot pair)"
     exit 1
 fi
 if [[ "${fetched:-0}" -lt 1 ]]; then
-    echo "FAIL: phase-4 fetched=$fetched, want >= 1 (the V1 path should warm)"
+    echo "FAIL: phase-4 fetched=$fetched, want >= 1 (the V2 path should warm)"
     exit 1
 fi
 if [[ "${failed:-0}" -ne 0 ]]; then
@@ -841,31 +814,34 @@
     exit 1
 fi
 
-# 4f: assert the V1 .deb's url_path row exists with a non-null
-# blob_hash and last_fetched_at — proof CommitAdoption inserted the
-# prefetched row. SPEC3 §7.5.1: the row is inserted in the same
-# transaction as the snapshot flip.
-V1_DEB_PATH="/pool/main/h/hello-acu/hello-acu_1.0_amd64.deb"
-v1_row=$(sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
-    "SELECT IFNULL(blob_hash, '') || '|' || IFNULL(last_fetched_at, '') FROM url_path WHERE path='${V1_DEB_PATH}';" \
+# 4e: assert the V2 .deb's url_path row exists with a non-null
+# blob_hash and last_fetched_at, and that no client requested it —
+# proof CommitAdoption inserted the prefetched row. SPEC3 §7.5.1: the
+# row is inserted in the same transaction as the snapshot flip.
+V2_DEB_PATH="/pool/main/h/hello-acu/hello-acu_1.1_amd64.deb"
+v2_row=$(sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
+    "SELECT IFNULL(blob_hash, '') || '|' || IFNULL(last_fetched_at, '') || '|' || request_count FROM url_path WHERE path='${V2_DEB_PATH}';" \
     2>/dev/null || echo "")
-if [[ -z "$v1_row" || "$v1_row" == "|" ]]; then
-    echo "FAIL: phase-4 V1 .deb url_path row missing or has null blob_hash"
+if [[ -z "$v2_row" ]]; then
+    echo "FAIL: phase-4 V2 .deb url_path row missing"
     sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
-        "SELECT path, blob_hash, last_fetched_at, last_requested_at FROM url_path;" || true
+        "SELECT path, blob_hash, last_fetched_at, last_requested_at, request_count FROM url_path;" || true
+    exit 1
+fi
+IFS='|' read -r v2_blob v2_lf v2_rc <<< "$v2_row"
+if [[ -z "$v2_blob" ]]; then
+    echo "FAIL: phase-4 V2 .deb url_path.blob_hash is null"
     exit 1
 fi
-v1_blob="${v1_row%%|*}"
-v1_lf="${v1_row#*|}"
-if [[ -z "$v1_blob" ]]; then
-    echo "FAIL: phase-4 V1 .deb url_path.blob_hash is null"
+if [[ -z "$v2_lf" ]]; then
+    echo "FAIL: phase-4 V2 .deb url_path.last_fetched_at is null (prefetch should set this)"
     exit 1
 fi
-if [[ -z "$v1_lf" ]]; then
-    echo "FAIL: phase-4 V1 .deb url_path.last_fetched_at is null (prefetch should set this)"
+if [[ "$v2_rc" != "0" ]]; then
+    echo "FAIL: phase-4 V2 .deb url_path.request_count=$v2_rc, want 0 (the row should come from the prefetch, not a client)"
     exit 1
 fi
-echo "[deb-test] phase 4 V1 .deb url_path: blob_hash=${v1_blob:0:12}.. last_fetched_at=$v1_lf"
+echo "[deb-test] phase 4 V2 .deb url_path: blob_hash=${v2_blob:0:12}.. last_fetched_at=$v2_lf request_count=$v2_rc"
 
 echo "[deb-test] phase 4 (Phase 3 hot-prefetch) PASS"
 echo "[deb-test] OVERALL PASS"
```

- [ ] **Step 3: Run it on every image `run.sh` covers**

Run: `make deb-test` (`UBUNTU_VERSIONS` defaults to `24.04 26.04`)
Expected, for each version (the planning replay on `ubuntu:24.04`; `ubuntu:26.04` printed the same lines):
```
[deb-test] phase 1 (package contract + listener) PASS
[deb-test] V1 adopted at first contact: snapshot id=1 (after 2s)
[deb-test] adopted V2 snapshot id=2 (after 11s)
[deb-test] phase 2 (inline adoption smoke) PASS
[deb-test] V1-detached adopted at first contact: snapshot id=1 (after 2s)
[deb-test] adopted detached snapshot id=2 (after 11s)
[deb-test] phase 3 (detached adoption smoke) PASS
[deb-test] phase 4 V1 adopted snapshot id=1 (after 2s)
[deb-test] phase 4 V2 adopted snapshot id=2 (after 11s; prior was 1)
[deb-test] phase 4 hot-prefetch line: level=INFO msg=adoption_hot_prefetch_complete canonical_host=127.0.0.1 suite_path=/dists/noble snapshot_id=2 hot_count=1 fetched=1 failed=0 mismatched=0 unattempted=0
[deb-test] phase 4 V2 .deb url_path: blob_hash=d6f278405fb7.. last_fetched_at=<unix time> request_count=0
[deb-test] phase 4 (Phase 3 hot-prefetch) PASS
[deb-test] OVERALL PASS
```
and finally `[deb-test] OVERALL PASS (24.04 26.04)`. Planning replay times (images `planh2-deb:<v>`, the Go and fixture stages cached): `ubuntu:24.04` build 36 s, test 53 s; `ubuntu:26.04` build 23 s, test 52 s. The CI job's `timeout-minutes: 15` stays ample.

- [ ] **Step 4: Mutation check**

In phase 4, delete step 4b (from `V1_DEB_PATH="/pool/main/h/hello-acu/hello-acu_1.0_amd64.deb"` through the `v1_lr` check's closing `fi`), so no client asks for a package of the prior snapshot; run `UBUNTU_VERSIONS=24.04 make deb-test`.
Expected: `[deb-test] phase 4 hot-prefetch line: … snapshot_id=2 hot_count=0 fetched=0 …` then `FAIL: phase-4 hot_count=0, want >= 1 (Stage 1 should mine the V1 hot pair)` (the planning replay). Restore it.

- [ ] **Step 5: Lint and commit**

Run: `shellcheck e2e/deb/test.sh`
Expected: no output (as at 6786529).

```bash
git add e2e/deb/test.sh
git commit -F - <<'EOF'
e2e/deb: phases 2-4 after eager first adoption

Eager first adoption adopts V1 at the cold prime, so phase 4's V1 -> V2
-> V1 chain no longer drove a hot-prefetch second cycle (hot_count=0),
and phases 2 and 3 passed on the first adoption before the change-driven
one committed. Each wait now names the exact anchor it expects; phase 4
runs V1 -> V2, still proving that Stage 1 mines the prior snapshot's hot
pair and prefetches the new version (now also: no client fetched it).

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

## For H3 (the README, the release notes and the spec)

What this phase leaves for H3 to write, each with its source:

1. **`README.md`, testing:** `make e2e-flat` runs real apt (2.2 to 3.2, six images) against fixtures of every supported repository layout through the cache, about 25 minutes sequentially, one CI job per image; a binary built with `-tags e2ehooks` is a test binary and must never ship (`cmd/apt-cacher-ultra/e2ehooks.go`).
2. **The unsigned confirmation's real duration** (`docs/configuration.md`, *Anchor forms and unsigned repositories*, safeguard 2; the README's unsigned paragraph): "at least 5 minutes; with the retry schedule, usually 5 to 15" — a never-adopted unsigned repository adopts at the first retry of its `unsigned_unconfirmed` record at least 5 minutes after the first observation (ruling R-H2-6, design note 7).
3. **After a captive portal** (`docs/configuration.md`, *Flat repositories*, the limits of the refresh without a `Release`): the first `apt update` through the cache after the outage can still show the portal's page for a Release-less repository's `InRelease` or `Release`, which a client's miss cached during it; the check that update triggers deletes them, so the next update is clean (design note 11).
4. **A binary detached `Release.gpg`** (optional, *Flat repositories*): the cache adopts a repository that publishes only `Release` and a binary `Release.gpg`, but apt 2.2–3.2 refuses it ("unsupported binary format"), so such a repository is unusable through any path; Jitsi publishes an `InRelease` beside it (deviation 1).
5. **Spec §7**, in H3's revision (this phase leaves the spec alone so its revision number cannot collide with H1's): the two deviations above, design note 5's reading of "within one cooldown" and design note 7's timing of the unsigned confirmation.
6. **Release notes, 1.1.0:** the e2e harness is new (fixtures only); the live-upstream smoke (overview, "PR #8 parity") stays the manual step before the release.
7. **The overview's phase table:** H2's commit once it lands.

## Found outside this plan's scope (for the controller)

e2e/deb's phase 4 failure on this branch (CI's `deb-test`), found while planning, is now Task 12 by the controller's ruling: a harness fix in `e2e/deb/test.sh`, with phases 2 and 3 tightened for the same cause. Nothing else outside `e2e/flat` was found.

## Self-review against spec §7 (done while writing)

Every clause of §7's "Real-apt end-to-end" paragraph and of §9's H row maps to a scenario in Task 9's table or to deviation 1 or 2. Every scenario ran green on all six images with the files this plan gives. The staged `test.sh` of Tasks 3–7 each ran green on `debian:12` (those staged copies lacked some comment lines and nothing else), and applying Tasks 4–8 as worded to Task 3's file gives the final file byte for byte. Task 1's red step and the mutation checks of Tasks 1, 3, 4, 5, 7 and 12 were replayed and failed as stated; Task 8's is the planning run's own earlier failure, with the same line. Task 2's smoke command ran as written. Task 12's patch applies to 6786529 (`patch --dry-run`), its failing run (Step 1) and its green runs on `ubuntu:24.04` and `ubuntu:26.04` (Step 3) were replayed, and its `test.sh` is shellcheck-clean.
