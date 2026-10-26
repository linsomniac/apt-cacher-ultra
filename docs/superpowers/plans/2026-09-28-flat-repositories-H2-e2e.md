# Flat repositories — Phase H2: real-apt end-to-end tests (`e2e/flat`) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `make e2e-flat` drives real apt clients — apt 2.2.4, 2.4.14, 2.8.3, 2.6.1, 3.0.3 and 3.2.0 on the six client images of spec §7 (`debian:11`, `ubuntu:22.04`, `ubuntu:24.04`, `debian:12`, `debian:13`, `ubuntu:26.04`) — against fixture copies of every repository shape spec §3 names (k8s, CUDA, libnvidia-container, Jenkins, Sublime Text, Jitsi, two unsigned variants, two Release-less ones, a `dists/` repository) through apt-cacher-ultra, and asserts spec §7's list: installs succeed, apt's exact request lines (`./`, `%253a`, `by-hash/SHA512`), clean `apt-get` output (signature errors included), strict mode on the signed host, publishes reaching clients within one cooldown through the client's own check, an older `Date` ignored, state across a real restart, a 45 s commit that times out no client, the unsigned first adoption no sooner than its 5-minute confirmation gap, PR #8's end-to-end table with the upstream really down, and the phase G Release-less carry-forward; CI runs it per image. Task 12 (the controller's addition) also makes CI's `deb-test` green again: e2e/deb's phases 2–4 still assumed no adoption at first contact.

**Architecture:** e2e/deb's single-container pattern: one image per client image holds nginx (the upstream), the daemon and the apt clients. A `fixtures` build stage builds every repository in two versions, signed with a key generated there (ruling R-H2-1), checks the shapes the scenarios rely on, and links each repository's URL path to a `current -> v1` symlink that `test.sh` flips; nginx serves them under two host names, `signed.test` (the signed repositories) and `plain.test` (the unsigned and Release-less ones), which `/etc/hosts` points at `127.0.0.1`. The daemon is built with `-tags e2ehooks`, which compiles in one test seam — `ACU_E2E_COMMIT_DELAY` sets the adopter's existing commit-delay hook — and runs with short timers (ruling R-H2-2). Each apt "client" is its own state directory selected with `APT_CONFIG`. Assertions read apt's output and its `Debug::Acquire::http` request lines, nginx's access log, the daemon's snake_case log events, the admin status JSON and `/metrics`; every wait is a bounded poll on one of them (ruling R-H2-5), and each check that could pass vacuously has a control that shows it can fail. `e2e/flat/run.sh` loops over the images (`E2E_FLAT_IMAGES`); CI runs one image per matrix job (ruling R-H2-3).

**Tech Stack:** Bash 5 (`set -euo pipefail`, shellcheck 0.11), Docker with BuildKit, nginx-light, `apt-ftparchive`/`dpkg-deb`/`dpkg-source`/`gpg` in the fixture stage, `curl` and `jq` in the test image, Go 1.25 for the one test seam.

**Spec:** `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (revision 12) — §7's "Real-apt end-to-end" paragraph, item by item, is this plan's requirement; §3 (apt's behaviour and the real flat repositories) gives the fixture shapes; §9's H row is the definition of done; C2 (flat forms, twins, the host-root limitation), C4 (cooldown, `periodic_refresh`, Dispatch 3–4: the anchorless refresh), C5 (the `Date` rules, the unsigned confirmation, SHA512-only Releases), C6 (vouching every Filename shape), C8 (the flip barrier and its phase-B-expiry residual) and C10 (strict mode) are what the scenarios exercise. Overview: `docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md` — the H line under "Cross-phase interfaces" (PR #8's end-to-end table), the *(G)* carry-forward "`e2e/flat` gains a Release-less repository" and "PR #8 parity" (PR #8 verified live; this e2e uses fixtures only, and H3 runs the live smoke). Phase H is three plans run in order: H1 (the sweep: `…-H1-sweep.md`), **H2 (this plan)**, H3 (README, release notes, the live-upstream smoke, the final full-diff review).

## Revision 3 (2026-10-26): the post-H1 pre-flight

Phase H1 landed (stamped 4a3a1e0). A pre-flight compared this plan with that tree (`.superpowers/sdd/2026-09-28-flat-repositories-H2-e2e/preflight-h1-delta.md`): no event, field, level, status-JSON path, metric, config key or admin call the plan relies on changed; every `old` block still occurs once; Task 12's patch applies; Task 1 replays. Changes, by the controller's rulings:

- **B-1, the overview's H1 carry (parked item 12: `main.go`'s end of the `CheckBackoff` wiring is untested; *H2's*):** Task 4's `upstream-down` now polls, bounded by `PERIODIC + TICK + 10` = 48 s, until the k8s suite's row on the status page shows `backoff ends in` (`backoff_shown`) while nginx is down, with a daemon mutant that drops the wiring; Task 11 marks the carry done; Task 9's map and the README say so. This assertion was not run in revision 3 (no Docker): the implementer runs Task 4's steps and records the measured time.
- **M-1** spec revision 12; **M-2** the guard's line is `event_docs_test.go:116` (Task 1); **M-3** design note 7 quotes `gate.holds(req.key)`; **M-4** a re-check of H1 as executed beside the plan-based one (twelve renamed DEBUG events); **M-5** H1's rename in the past tense (Global Constraints); **M-6** For H3 item 5's reason.
- **Info, wording only:** I-1 (the H1 re-check is done), I-3 (Task 11's done markers read `*Done in H2 (…)*`). Not taken: I-2 (a step to mark H2's phase row done: H3 keeps it), I-4 (optional extra assertions) and I-5 (a cross-phase `- H2:` line), which add content rather than wording.

Replayed without Docker: Task 1's Go steps on a scratch copy of 4a3a1e0 (red, green tagged and untagged, both mutations, vet, `golangci-lint --build-tags e2ehooks`), `go test -count=1 ./internal/observability/` with Task 1's row, every docs `old` block of Tasks 1, 3, 10 and 11 at 4a3a1e0 (each occurs once), Task 11's overview edits applied, `bash -n` and the per-task `shellcheck` on the changed `test.sh` (its findings are unchanged: the new helper follows `restart`), Tasks 4–8 applied to Task 3's file (byte for byte), and `backoff_shown`'s row match with gawk and mawk 1.3.4 (the images' awk) against rows shaped as the status template renders them.

## Revision 2 (2026-10-25): the design and exec reviews of draft c8e6fe4

Both reviews (`planH2-review/design.md`, `exec.md`) found no Critical issue and no daemon bug; the exec reviewer applied Tasks 1–12 as worded (branch `review/phaseH2-exec`, ae1ce48..dd759a2), whose files equal this plan's draft byte for byte outside `docs/`, and ran them green on all six images. By the controller's rulings every Important finding is adopted, and every Minor and Nit unless it conflicts with spec §7, a ruling or another finding, or a run showed it hurts. Every change below was replayed (Task 9), and each new or changed check's mutation step failed as its task states.

| Finding | Outcome | Where |
|---|---|---|
| design I-1: the 5-minute floor never asserted | adopted: `took >= 300` on the long-wait images (a `minConfirmGap` of 1 minute fails it: "adopted 112s after its first contact"); `whole_run` checks `/unsigned404` is unadopted on the short ones (partial: only when its first retry came before `whole_run`) | Task 8 |
| design I-2: the scheduler can carry "within one cooldown" | adopted: `baseline` reads the suites' `last_success_unixtime` after a client update, `by_client` requires the client to see the publish before one `periodic_refresh` after it; in `pr8-table`, `frozen-date`, `anchorless` (goreleaser), `two-suites` (now bounded by `REACH`) and `mirror-mode`. `periodic_refresh` raised to 30 s (tick 7.5 s, `TICK=8`), so the scheduler bound is 48 s and the unsigned deadline 968 s. `baseline` waits for the update's own checks, so a read before they land cannot make the check flaky (design note 5) | Tasks 3–6 |
| design I-3: signature errors match none of the grep's words; no detached flip under the full grep | adopted: `APT_SIGNED_ERRORS` counts on lines naming `signed.test` or `/m-signed/`; a wrong-key control (`/fixtures/keys/other.gpg`, a second throwaway key: one file the fixtures control, the same on every image, rather than an image's archive keyring) must fail `apt_update`; Jenkins flips in `mirror-mode`; the 45 s commit moves to Jitsi | Tasks 2, 3, 6, 7 |
| exec I-1: `restart` passes without restarting | adopted: `process.started_unixtime` after the restart is no older than the stop | Task 4 |
| exec I-2: `upstream-down` passes with nginx up | adopted: nginx's access log does not grow while it is stopped | Task 4 |
| exec I-3: `apt_update` could skip its grep | adopted: the `nowhere.test` control goes through `apt_update` and must fail it; Task 3's mutation 3 (`if false` for the grep) fails the run | Task 3 |
| design M-1: `re` escapes `/` | adopted (`/` is not special in an ERE); the "stray \" warning did not reproduce on the images' grep 3.8–3.12 (as the exec reviewer found) | Task 3 |
| design M-2, exec m-2: `grep -q` at the end of a pipe under `pipefail` | adopted the robust way: `apt_sent_line` greps the file itself (a CR-tolerant pattern, no pipe); `apt-file`, `showsrc`, `zcat` and the restart's headers go to files first; `apt_errors` greps never use `-q` | Tasks 3–5 |
| design M-3: `older-date`'s client check is apt's own | adopted: the snapshot ids are unchanged and a fresh client sees 2.0 | Task 4 |
| design M-4: the 403 variant's check is a coin flip | adopted: it waits until that record's second retry is past 5 minutes (`adoption_fail_count >= 3`), then asserts it unadopted; deviation 2 reworded | Task 8 |
| design M-5: the commit-delay grep catches only timeouts | adopted: the scope is now subtractive — every error but the residual's words (`APT_RESIDUAL`, `apt_update --allow=`) | Tasks 3, 7 |
| design M-6: `fury_clean` relies on apt's prefixes | adopted: `Error:` too, and the cache answers `/fury/InRelease` 404 | Task 5 |
| design M-7: the sniff counter may predate the portal | adopted: it must grow past its value before the portal | Task 5 |
| design M-8: an unexpected exit names no scenario | adopted: `fail` sets `FAILED`; an `EXIT` trap prints the scenario and the log tail otherwise | Task 3 |
| design M-9: Ctrl-C leaves the container running | adopted `docker run --init` and a stale-container `docker rm -f`; the replay showed `--init` alone is not enough (tini forwards SIGINT to bash only, which waits for its child), so `test.sh` also traps INT and TERM: with the trap the container exits on `docker kill --signal=INT`, without it, it keeps running | Task 3 |
| design M-10, exec m-7: the tagged tests never run in CI; no guard on the shipped binary | adopted: CI's `test` job vets and tests `-tags e2ehooks ./cmd/apt-cacher-ultra/`; the `test` job and `release.yaml` fail a build that contains `ACU_E2E_` | Task 10 |
| design M-11: CI's long-wait selection is implicit | adopted: each matrix row names `unsigned`; `run.sh` reads `${E2E_FLAT_UNSIGNED_IMAGES-…}` (set but empty: none) | Tasks 3, 10 |
| design M-12: image-build robustness | partly: `-o Acquire::Retries=3` adopted; dropping `bullseye-security` is not — the replay's `debian:11` build then failed (`perl : Depends: perl-base (= 5.32.1-4+deb11u3) but 5.32.1-4+deb11u5 is to be installed`: the image's own packages come from it) | Task 2 |
| design M-13: the panic regex misses prose lines | adopted | Task 8 |
| design: `plain.test` never strict, asserted by absence | adopted: after the unsigned adoption an unvouched `.deb` there answers 200 | Task 8 |
| design: curl Jitsi's binary `Release.gpg` through the cache | not adopted: the replay answered 404, by design (spec §10 item 9: an inline snapshot answers its suite's `Release` and `Release.gpg` 404), and before the adoption a 200 proves passthrough only, not the sniff, which `TestSniffReleaseGPG` pins | — |
| design: `gpgconf --kill gpg-agent` before the key's removal | adopted | Task 2 |
| design: a workflow-level `concurrency` group | not adopted: it changes every CI job's behaviour, outside this phase's scope; left to the maintainer | — |
| design: underspecified items | adopted: the first CI run's job times are H3's to compare with the timeouts (For H3); the README says to run `test.sh` once per container; the For-H3 time is the measured one; the H1 re-checks are done, against H1's plan and against H1 as executed (below) | Tasks 11, For H3 |
| design: Task 9's map lists `two-suites` under "within one cooldown" at 60 s | adopted: `two-suites` now waits `REACH` and checks `by_client` | Tasks 6, 9 |
| exec m-1: Tasks 3–7's shellcheck expectations | adopted: each lint step states what shellcheck prints at that task, measured on the staged files | Tasks 3–8 |
| exec m-3: `apt_sent` can be vacuous | adopted: `apt_sent_line` and a negative control | Task 3 |
| exec m-4: `poll` can be vacuous | adopted: `setup`'s self-test | Task 3 |
| exec m-5: fixture shapes unchecked | adopted: `build-fixtures.sh` fails the build when libnvidia-container's Dates differ or its `Release` is not SHA512-only | Task 2 |
| exec m-6: the Go tests do not pin that the delay takes effect | adopted (text): `e2ehooks.go`'s AIDEV-NOTE names `commit-delay` as the pin | Task 1 |
| exec m-8, controller: the docs overstate the unsigned wait | adopted: README and Makefile help give "about 3 to 14 minutes more" (measured 4 to 5) and "5 to 15 minutes after first contact" | Task 11 |
| exec n-1: noisy readiness poll | adopted: `curl -fs`; nginx's `[notice] signal process started` on stop stays (stderr, harmless) | Task 3 |
| exec n-2: "(E2E_UNSIGNED_WAIT unset)" | adopted: "(E2E_UNSIGNED_WAIT is not 1)" | Task 8 |
| exec n-3: Task 8's mutation step leaves a dangling pipe | adopted: reworded | Task 8 |
| exec n-4: the debug recipe works once per container | adopted | Task 11 |
| controller: actionlint by `go run` was blocked | adopted: a pinned image, `rhysd/actionlint:1.7.12`, run with `--network none` | Task 10 |

### Re-check against H1 revision 2 (30c9638), 2026-10-25

No H2 assertion, config key, status-JSON path or metric depends on anything H1 renames, removes or changes: H1's 43 renames (twelve of them DEBUG) touch none of the events this harness reads (`request`, `freshness_check` — matched as a whole `msg=` value, so H1's `freshness_check_cancelled` and the like never match it — `adoption_success`, `adoption_refused`, `adoption_flip_barrier`, `unvouched_deb_refused`, `package_hash_conflict`) or their fields; H1 only adds status fields and changes config comments; `shutdown wait panicked` becomes `shutdown_wait_panicked`, which `whole_run`'s `[a-z_]*panicked` alternative matches. One addition: H1's `TestEveryEventIsDocumented` parses every non-test Go file under `cmd/` and `internal/` whatever its build tag, so `e2ehooks.go`'s `e2e_commit_delay_enabled` needs a `docs/log-fields.md` row, which Task 1 now adds (without it H1's guard prints `cmd/apt-cacher-ultra/e2ehooks.go:41: event e2e_commit_delay_enabled has no row in docs/log-fields.md; add one`; checked with H1's two guard files on this plan's tree, and silent with the row); it is snake_case, so `TestLogMessagesAreSnakeCase` needs no list entry. H2's two overview edits (Task 11) still apply after H1's Task 18 overview diff (applied in a scratch copy: each old text occurs once).

### Re-check against H1 as executed (4a3a1e0), 2026-10-26

The conclusion stands. The events the harness reads keep their names, levels and fields: `request` (`internal/handler/handler.go:2623`), `freshness_check` (`internal/freshness/check.go:272`), `adoption_refused` (`adoption_failure.go:121`), `adoption_success` (`adoption.go:1862`), `adoption_flip_barrier` (`flip_barrier.go:78,83`), `unvouched_deb_refused` (`handler.go:989`) and `package_hash_conflict` (`handler.go:1140`). Every production panic event ends in `_panicked` (`adoption_panicked`, `freshness_check_panicked`, `gc_panicked`, `handler_goroutine_panicked`, `shutdown_wait_panicked`). The status JSON and `/metrics` changes are additive. Config, e2e/, CI and the Makefile are unchanged since 6786529. Twelve of the 43 renames are DEBUG. The one H1 output for H2 is the overview's `CheckBackoff` carry (overview line 138), which Task 4 now covers.

## Global Constraints

Everything in the overview's Global Constraints that touches this plan applies. In particular:

- **Fixtures only.** No scenario reaches a live repository. The test container's only upstream is its own nginx; the daemon's `upstream.allowed_host_regex` admits `signed.test` and `plain.test` alone. CI's network use is the base images (Docker Hub: `golang:1.25-bookworm`, `debian:bookworm-slim`, the client image; locally also `rhysd/actionlint` for the workflow check), the distribution mirrors the image build installs from (`deb.debian.org`, `archive.debian.org`, `archive.ubuntu.com`) and the Go module proxy the build stage's `go mod download` uses, as e2e/deb's job already does (design note 13).
- **Spec values kept** (ruling R-H2-2): the unsigned confirmation gap `max(cooldown, 5m)` = 5 minutes (1 hour after a 401/403 on a signature file), the 45 s commit delay, the flip hold's 20 s bound below apt's 30 s timeout, `flip_quiet_period` inside its `1s`–`15s` range, the six client images.
- **No production change.** The only Go change is the `e2ehooks` seam (Task 1), compiled only with `-tags e2ehooks`; a release build (`make build`, `make deb`, nfpm, e2e/deb) contains its no-op twin and reads no `ACU_E2E_*` variable (`TestBuildAdopter_ReleaseBinaryIgnoresTheE2ECommitDelay`, and CI's grep of the release binary, Task 10).
- **Assert on stable names.** Log assertions use only snake_case events (`request`, `freshness_check`, `adoption_success`, `adoption_refused`, `adoption_flip_barrier`, `unvouched_deb_refused`, `e2e_commit_delay_enabled`, `*_panicked`, `package_hash_conflict`) and fields, the admin status JSON (`suites[]`, `process.started_unixtime`: a locked contract), `/metrics`, and one status-page text, a failing suite's `backoff ends in` (Task 4: the overview's H1 carry asks for exactly that text). No assertion reads a prose message except `whole_run`'s panic catch-all (net/http's `http: panic serving …`; `shutdown wait panicked`, which H1 renamed `shutdown_wait_panicked`, matched by the regex's `[a-z_]*panicked` alternative as `gc_panicked` is), so H1's snake_case renames (its rulings R-H3 and R-H6) cannot break this harness, which runs after H1. The event H2's Go code adds has a `docs/log-fields.md` row (Task 1), as H1's `TestEveryEventIsDocumented` requires.
- **No assertion is weakened to pass** (the controller's rule): a check that flakes gets a wider bound with its reason, never removed.
- After the Go change (Task 1): `go test -race ./...` and `golangci-lint run ./...` are clean, `golangci-lint run --build-tags e2ehooks ./cmd/...` too, and `gofmt -l .` lists only the five known drift files (`cmd/apt-cacher-ultra/ca_print.go`, `internal/cache/cache_test.go`, `internal/cache/gc_urlpath_test.go`, `internal/freshness/hot_prefetch_test.go`, `internal/proxy/connect_stats.go`). Never run `gofmt -w` on those five.
- Every shell file in `e2e/flat/` is bash with `set -euo pipefail` (`lib.sh` is sourced by `test.sh`, which sets it), and `shellcheck -x e2e/flat/*.sh` (run from the repo root) prints nothing once Task 8 lands; Tasks 3–7's lint steps state exactly what it prints until then (functions and variables a later task uses). No `grep -q` ends a pipe (design note 18). No bare `sleep` stands in for a condition (ruling R-H2-5); the sleeps that remain pace a poll or a client loop (`poll`'s one second, `until_candidates`' cooldown, the commit-delay loop's 3 s).
- Commit trailer: `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

Conventions in this plan:

- Shell and Dockerfile contents are written out in full: each task's files are the final ones, exactly as the planning runs executed them (design note 15), except revision 3's `backoff_shown` poll in Task 4's `upstream_down`, which only `bash -n`, `shellcheck` and a row-match test have checked (no Docker in revision 3). A task that extends `test.sh` gives the function bodies it adds and the one-line edit to the run list at the end of the file.
- "Run" commands are run from the repo root. Expected outputs quote the planning runs; times vary with the machine, so an expected time is a range, never an assertion.
- Each image build needs network access to Docker Hub and the distribution mirrors; a build failure that names a mirror (`Failed to fetch http://deb.debian.org/…`, `404  Not Found`) is the mirror, not the task: retry once before debugging (design note 9).
- Daemon mutants (Task 8's checks) are scratch builds only: change the named constant in a scratch copy of the tree, build and run `e2e/flat` from it, and never commit the change.

## Rulings (controller, binding)

- **R-H2-1 — test keys.** The fixtures' signing key is generated in the `fixtures` build stage (`gpg --quick-generate-key … rsa3072 sign 0`, an empty passphrase, a throwaway `GNUPGHOME`), exported to `/fixtures/keys/acu-e2e.gpg` with its fingerprint, and never leaves the image; no key is committed. Generation takes under a second, so committing a key was never needed. A second key, generated the same way, signs nothing: `/fixtures/keys/other.gpg` is the wrong-key control's (design note 17).
- **R-H2-2 — time.** Real timers, short intervals: `freshness.cooldown = "2s"`, `periodic_refresh = "30s"` (a 7.5 s scheduler tick; 20 s in the draft, raised for `by_client`'s margin: design note 5), `adoption.flip_quiet_period = "1s"`. Fixed spec values are kept (Global Constraints): the unsigned first adoption runs only on the oldest and newest images (`debian:11`, `ubuntu:26.04`, `E2E_FLAT_UNSIGNED_IMAGES`), where it overlaps the other scenarios (design note 7); the 45 s commit delay runs everywhere. Measured wall times are in Task 9.
- **R-H2-3 — CI.** One matrix job per client image, `fail-fast: false`, each running `E2E_FLAT_IMAGES=<image> E2E_FLAT_UNSIGNED_IMAGES=<image or empty> bash e2e/flat/run.sh`; `timeout-minutes` 15 for the four short images and 30 for the two that wait for the unsigned confirmation (Task 10; reasons in design note 13). Controller ruling on the triggers: the same as `deb-test` — pushes to `main` and `master`, and every pull request (the workflow's own `on:`). Nightly-only would find regressions later and needs someone to watch a nightly run: more maintenance, not less.
- **R-H2-4 — what the e2e finds.** The planning runs found no daemon bug: every scenario passed on all six images against the daemon at 6786529 (Task 9), and so did the reviews' 28 runs. The failures met on the way were the harness's own and are fixed in the files this plan gives (Task 9 lists them); the unsigned confirmation's timing is the spec's behaviour (R-H2-6). So this plan has no daemon fix task. Task 12 (the controller's addition) fixes another harness, e2e/deb, whose replay also passes with the daemon unchanged. An implementer whose run fails a scenario for a daemon reason follows the ruling: a Go regression test that fails first, the fix, the scenario that caught it — never a weakened assertion.
- **R-H2-5 — flake resistance.** Every wait is `poll SECONDS WHAT COMMAND…` (a bounded poll on a log line, an HTTP answer, a status field or an apt result, with a failure message) or `until_candidates LIMIT …` (apt updates once a cooldown until the candidates match, bounded); a failure prints the scenario name, the daemon's log tail (80 lines) and nginx's access-log tail (30 lines), and so does an unexpected exit (design note 19).
- **R-H2-6 — the unsigned confirmation lands at a retry** (controller ruling). Spec C5 safeguard 2 asks for two observations "at least" `max(cooldown, 5m)` apart, and its refusal states use the same record and schedule as failures (C7), so the first adoption landing at the record's first retry 5 minutes or more after the first observation — 5 to 15 minutes in practice — is the spec's behaviour, not a defect: no code change. The deadline (968 s with the 30 s `periodic_refresh`) follows from it (design note 7). H3's documentation says "at least 5 minutes; with the retry schedule, usually 5 to 15".
- **R-H2-7 — the reviews** (controller ruling, 2026-10-25). Every Important finding of both reviews is adopted, and every Minor and Nit unless it conflicts with spec §7, a ruling or another finding, or a run shows it hurts; the revision table above records each outcome. `periodic_refresh` is 30 s; the bounds that depend on it are recomputed from their formulas and remeasured.

## Design notes (choices the spec leaves open)

1. **Two host names, one nginx.** `signed.test` serves the seven signed repositories (six flat shapes and `dists/`), so strict mode (`integrity.refuse_unvouched_debs = true`) is on there once they adopt; `plain.test` serves the unsigned and Release-less ones, which never count for strict mode (spec C10), so it is never strict. `.test` is reserved (RFC 6761); `/etc/hosts` maps both to `127.0.0.1`, and the daemon's `deny_target_ranges = []` lets it dial loopback (as e2e/deb's config does). A third shape, the daemon's mirror mode, reaches `signed.test` through the `[[mirror]]` prefix `/m-signed`, so its clients share the proxy-mode suites.
2. **Clients are apt state directories, not containers.** `client NAME PROXY SOURCE-LINE…` writes `/clients/NAME/{sources.list,apt.conf}`; `APT_CONFIG` selects them for `apt-get`, `apt-cache` and `apt-file`. dpkg's database is shared, so every install check verifies the installed file's version and purges the packages again. This keeps the single-container pattern spec §7 names and lets two clients use the two source forms of one repository (PR #8's table) side by side.
3. **The fixture shapes** (spec §3), all versions `1.0` and `2.0`: k8s `deb <uri>/ /`, Filenames `amd64/…`, `InRelease` only, nginx answering 403 for a missing file; CUDA `./…` Filenames (apt-ftparchive's dpkg-scanpackages shape) with `InRelease`, `Release` and `Release.gpg`, plus `acu-epoch` version `1:1.0`/`1:2.0` in a file named `acu-epoch_1%3a<v>_amd64.deb`; libnvidia-container `deb <uri> /`, `InRelease` only, a SHA512-only `Release` text over a `Packages` that keeps SHA256, `Date` frozen at 2018 in both versions (both checked by the build: design note 20); Jenkins `deb <uri> binary/`, detached with an armored `Release.gpg`, Filenames `binary/…` and `direct/…`; Sublime `deb <host-root> apt/stable/`, Filenames `files/…` at the host root, `files/` keeping every build; Jitsi `deb <uri> stable/` plus `deb-src`, `Acquire-By-Hash: yes`, SHA256 and SHA512 only, `Contents-amd64`, `Sources`, a binary `Release.gpg` beside `InRelease` (deviation 1); `dists/` `deb <uri> acu main`, inline; unsigned `Release`-only at `/unsigned404` (missing files 404) and `/unsigned403` (403); Release-less `Packages`/`Packages.gz` at `/fury` (Gemfury: Filenames without `./`) and `/goreleaser` (`./` Filenames). The `sub/` and `a/b/` forms' `Release` files carry the `Suite` apt checks (`binary`, `apt/stable`, `stable`), or apt warns "Conflicting distribution" as no real repository makes it.
4. **Dates and mtimes.** v1 is dated two days before the image build and v2 one day before, both in the past and without `Valid-Until`, so a cached fixture layer stays valid; every file of a version carries its version's mtime, because otherwise v1 and v2 indexes of equal size built in the same second share nginx's ETag and `Last-Modified`, and a conditional GET would be answered 304.
5. **"Within one cooldown", and by whom** (spec §7, PR #8's table). A client that runs `apt-get update` once a cooldown must see the new version within `REACH` = cooldown + 20 s of the publish: the first update after the publish fires the check (once the cooldown has passed), the adoption and the flip barrier (phase A's 1 s quiet after the client's last anchor, a zero-length hold) follow, and a later update sees the new snapshot. The time alone could be met by the scheduler too, so each such scenario also proves the path: `baseline START HOST SUITE…` waits until the suites' `last_success_unixtime` is no older than the client update that started at `START`, less a cooldown (the checks that update fired, or the one whose cooldown made it skip them), and `by_client` then requires the client to see the publish before `BASELINE + periodic_refresh` — the earliest the scheduler could check those suites again. Waiting for the update's own checks matters: a status read before they land would take an older stamp and could fail a correct daemon. With `periodic_refresh` at 30 s the margin is wide: the client's check carried every publish 4–8 s after the baseline in the planning runs. The anchorless refresh's own carry-forward bound, "within one `periodic_refresh`", is checked with no client at all: `periodic_refresh` + one tick + 10 s = 48 s (measured: 29–37 s after the publish).
6. **Observables.** The admin status JSON (`/?format=json`, `suites[]`: `current_snapshot_id`, `anchor_form`, `last_success_unixtime`, `adoption_fail_*`; `process.started_unixtime`) for state; text-log events for what happened (`adoption_refused reason=upstream_older`, `adoption_flip_barrier outcome=…`, `request … validated_hash=true`, `flip_held_ms`); `/metrics` for `acu_anchorless_sniff_rejected_total{host="plain.test"}`; the status page's HTML for one text, a failing suite's `backoff ends in` (Task 4); nginx's access log for the request lines the cache sent upstream, and for "the upstream answered nothing" while it is stopped; apt's `Debug::Acquire::http` output for the lines apt sent (CRLF-terminated, which `apt_sent_line`'s pattern allows). No assertion reads the SQLite file.
7. **The unsigned scenario overlaps the run, and lands at a retry.** `/unsigned404`'s first contact is the first update of `first-contact`; its adoption is asserted after `commit-delay` (about 2 minutes in). The confirming observation is not the first check 5 minutes after the first one: checks before the `unsigned_unconfirmed` record's retry time see the same bytes and stay `changed_deferred` (spec C7's gate: `checkFirst`'s `deferred, reason := gate.holds(req.key), gate.reason` (`retryGate.holds`: `g.active && !g.due && key == g.hash`, `internal/freshness/freshness.go`), `internal/freshness/first_adoption.go`, before `unsignedFirst` runs), and the retries come 2, 4 and 8 minutes apart, each ±25 %. So the adoption lands at the first retry at least 300 s after first contact: before 450 s when the second retry is that late, else at the third, before 900 s. The deadline is first contact + 900 s + `periodic_refresh` + one tick + 30 s (968 s; a due retry is picked up within a tick, so this is generous). The floor is asserted too: an adoption sooner than 300 s after first contact fails (`took >= 300`; `UNSIGNED_FIRST_CONTACT` is read before the first update reaches the cache, so this cannot fail a correct daemon), and on the four short images `whole_run` requires `/unsigned404` to be still unadopted at about 2 minutes, which catches a broken floor whenever that suite's first retry (90–150 s) came first. Planning runs: draft 310, 397 and 385 s (`debian:11`), 373 s (`ubuntu:26.04`); revision 2 373 and 382 s (`debian:11`), 326 and 356 s (`ubuntu:26.04`); an earlier draft whose deadline assumed 5 minutes timed out at 355 s on `ubuntu:26.04`. The record survives the scenario's restarts (the confirmation stamp is persisted). Running the scenario after `upstream-down` instead would cost up to two more minutes: that scenario's refused connections put the suite in the failing class (a 2-minute backoff).
8. **Lower bounds for the absence checks.** "Zero `unvouched_deb_refused` on the signed host" would pass vacuously with strict mode off, so `first-contact` requests one undeclared `.deb` (`acu-rogue`, on the upstream but in no `Packages`) and requires a 502 and its refusal line; `whole-run` excludes exactly that path and also requires zero refusals on `plain.test`. Likewise every update check has a control on every apt version: `apt_update` must fail an update of a source the cache refuses (`nowhere.test`: `E: Failed to fetch …  403  Forbidden`) and one of a signed source checked against the wrong key (design note 17); `apt_sent_line` must not match a line apt never sent; `poll_quietly` must time out on `false`.
9. **`debian:11` is past its LTS** (ended 2026-08-31): its packages moved to `archive.debian.org`, and the image still names `deb.debian.org`, whose security pool now answers 404. The test stage rewrites `sources.list` for `bullseye` (main and security: the image's own packages, `perl-base` among them, come from security, so `apt-file`'s `perl` needs it) before installing nginx and the tools; apt itself (2.2.4, the one under test) is the image's. Every `apt-get` of the image build retries three times (apt 2.2 makes none by default). Images that reach end of life later need the same treatment (README, "Maintenance").
10. **Two Release-less repositories.** `/goreleaser` is republished while a client updates once a cooldown (the request-triggered path, "within one cooldown", `by_client`); `/fury` is republished with no client (the scheduler path, "within one `periodic_refresh`") and then put behind a captive portal (`/run/acu-e2e/captive` makes nginx answer an HTML page with 200 for every `/fury` path).
11. **The captive portal's aftermath.** A client update during the outage caches the portal's pages as `/fury`'s `InRelease` and `Release` (a miss stores what upstream answers; apt reports `Signed file isn't valid, got 'NODATA'` for the `Release`, as it would fetching directly — on `plain.test`, so the signature words do not count there: design note 17). After the outage the first update still gets those pages, and the check it triggers deletes them (spec C4 Dispatch 4: an anchor answered 404); the harness polls, bounded by `REACH`, until an update shows no error for `/fury` and the cache answers `/fury/InRelease` 404 again. Not a daemon bug: there is no synchronous revalidation (spec §2, non-goals). Listed for H3's documentation.
12. **The commit-delay seam is a build tag.** `cmd/apt-cacher-ultra/e2ehooks.go` (`//go:build e2ehooks`) reads `ACU_E2E_COMMIT_DELAY` and calls the adopter's existing `SetCommitDelayForTest`; `e2ehooks_off.go` (`//go:build !e2ehooks`) is a no-op. The e2e image is built with the tag, every release path without it, and CI checks the release binary for the string. The seam sleeps before the commit transaction, inside the barrier, as the flip simulations' slow commit does; the scenario restarts the daemon with the variable set and again without it. The Go tests pin the parse and the log line; `commit-delay` pins that the delay takes effect (`outcome=hold_expired`).
13. **CI: one job per image** (ruling R-H2-3). The point of `e2e/flat` is apt-version coverage, which a subset would drop for most images; one image per job keeps each log readable and each timeout tight, and parallel jobs keep the wall time near the slowest image (the two with the unsigned wait). Each matrix row names its `unsigned` image (or none), so a long timeout and a long wait go together by construction. A short image measured about 1 minute of cold build and 2.3 minutes of test on the planning machine, so 15 minutes leaves room for a slower runner and its image pulls; a long-wait image can take up to 968 s for the unsigned adoption alone (design note 7) plus the rest and a cold build, about 20 minutes, so it gets 30. Every input is a base image or a distribution mirror, plus the Go module proxy, which the controller ruled acceptable: `deb-test` already depends on it, and it is not a live apt upstream, which is what the "fixtures only" rule protects. A mirror outage fails one job, which is re-run alone. No automatic retry: a retry would hide a flaky scenario. A subset in CI with the full matrix only local was the alternative; it was rejected because apt-version differences are exactly what a local-only run lets drift. The `e2ehooks` unit tests run in the `test` job, and the `test` job and the release workflow fail a binary that contains `ACU_E2E_`. Who compares the first CI run's job times with the timeouts: H3 (For H3).
14. **`apt-file` is in every test image**, so apt requests the `Contents-*` targets of every repository whose `Release` lists them (Jitsi's, by hash) and probes them, with Translations, for the Release-less ones, as on a host with apt-file installed.
15. **The planning runs** executed every file in this plan (revision 2: in a fresh scratch copy at c8e6fe4, on all six images, with each new or changed check's mutation step; the draft: on all six images, as did the exec review), with the image and container names prefixed `planh2-` (`E2E_FLAT_NAME=planh2-e2e-flat`).
16. **`.dockerignore`** gains `/apt-cacher-ultra` (a local build output, 24 MB) and `.claude/` (Claude Code worktrees: 220 MB in the main checkout), which every `docker build` from the repo root otherwise sends as context; no Dockerfile reads either.
17. **Signature errors count on the signed host only.** apt reports a signature failure in none of the update grep's words, keeps the old lists and exits 0 — "GPG error" (apt 2.x), "OpenPGP signature verification failed" (apt 3.x), "is not signed", "error occurred during the signature verification", "Clearsigned file isn't valid", and "Conflicting distribution" for a wrong `Suite` — and the next update hides it. `APT_SIGNED_ERRORS` counts those words on lines that name `//signed.test/` or `/m-signed/`, because the captive portal's aftermath prints the same words for `plain.test` (design note 11). A detached suite whose `Release` and `Release.gpg` came from different snapshots shows exactly this, so the detached Jenkins suite flips in `mirror-mode`, under the normal barrier and the full grep; the 45 s commit moves to Jitsi, which nothing else flips (losing only the draft's observation that a detached hold also covers `InRelease`, which Go pins). The wrong-key control checks the signed `dists/` source against `/fixtures/keys/other.gpg`, a key generated with the fixtures that signs nothing, rather than borrowing an image's archive keyring: one file the fixtures control, the same on every image.
18. **No `grep -q` ends a pipe.** Under `pipefail`, `grep -q`'s early exit can fail the pipeline through the writer's SIGPIPE once the input outgrows the pipe buffer (both reviews reproduced it, with 137 KB and 300 KB logs; the exec review measured today's largest debug log at 36.9 KB). `apt_sent_line` greps the file itself with a CR-tolerant pattern; command outputs go to files before a `grep -q`; `apt_errors` and the other pipelines use greps that read all their input.
19. **Exits and signals.** `fail` sets `FAILED`; an `EXIT` trap prints the scenario and the daemon's log tail for any other non-zero exit (a `set -e` exit: nginx not binding, `dpkg`, a publish's `mv`). `test.sh` traps INT and TERM into `fail`, since bash as PID 1 ignores signals it has no handler for, and `--init`'s tini forwards them to bash alone, which waits for its child; `run.sh` runs the container with `--init` and first removes a stale container of the same name.
20. **The build checks its shapes.** `build-fixtures.sh` fails the image build when libnvidia-container's v1 and v2 `Date` lines differ or its `Release` has an MD5, SHA1 or SHA256 block, the two shapes `frozen-date` and `first-contact` would otherwise pass without.

## Deliberate deviations from spec §7

1. **Jitsi's binary `Release.gpg` is never fetched by apt.** apt refuses a binary detached signature. The planning runs served an `InRelease`-less copy of the Jitsi fixture straight from nginx to each image's apt: 2.2.4, 2.4.14, 2.6.1 and 2.8.3 printed `W: GPG error: http://signed.test/jitsi-detached stable/ Release: Detached signature file '…_Release.gpg' is in unsupported binary format` and `E: The repository 'http://signed.test/jitsi-detached stable/ Release' is not signed.`; 3.2.0 the same after `W: OpenPGP signature verification failed:`; 3.0.3 `E: OpenPGP signature verification failed: … Signed file isn't valid, got 'NODATA' … Detached signature file '…' is in unsupported binary format`. So a detached-only binary repository cannot be exercised with real apt at all. The fixture serves `InRelease`, `Release` and the binary `Release.gpg`, and apt uses the `InRelease` (`first-contact` fails if apt fetches the `Release.gpg`). Nor does the cache serve it once the suite is adopted: an inline snapshot answers its suite's `Release.gpg` 404 (spec §10 item 9; the revision-2 replay). The cache's side of a binary signature — the sniff and a detached adoption — is pinned in Go (`TestSniffReleaseGPG`'s binary cases; the planning run also saw the detached-only fixture adopt, `adoption_success … suite_path=/jitsi/stable form=detached`).
2. **The 403 unsigned variant is not adopted in the run.** Its first adoption needs two observations an hour apart (spec C5 safeguard 2, a fixed constant), past any bounded run. The e2e checks what it can: its package installs through the cache, it waits `unsigned_unconfirmed` (`first-contact`), and on the long-wait images it is still unadopted after its record's second retry has come 5 minutes or more after its first observation, where a 5-minute gap would have confirmed it (`unsigned-first-adoption`; a second retry before 300 s, about 6 % of runs, leaves that run's check vacuous). The hour itself is pinned in Go (`TestCheckFirst_UnsignedAfterADeniedSignatureWaitsAnHour`).

## Review Focus

The failure modes this harness could hide, most likely first, each pinned in the owning task:

1. **An error check that matches nothing on some apt version, or is not applied** would pass every update vacuously. Task 3's `first-contact` requires `apt_update` itself to fail an update of a source the cache refuses (`http://nowhere.test/`) and one of a signed source checked against the wrong key, on every image.
2. **A publish carried by the scheduler, not the client** would pass a "within one cooldown" check on time alone: `by_client` requires the client to see it before the scheduler could have checked the suite (Tasks 4–6).
3. **A strict mode that is silently off** would make "zero refusals" vacuous: the `acu-rogue` refusal (design note 8, Task 3).
4. **A request line normalised on the way** (curl drops `/./` unless given `--path-as-is`, as the planning run's first probe showed; a `%` decoded twice): the `./`, `%253a` and `by-hash/SHA512` lines are pinned from apt's own debug output, and the `./` and `%253a` ones also in nginx's access log, the line the cache sent upstream; the harness curls no `./` path (Tasks 3–4).
5. **A scenario that never did what it names** — a restart without a restart, an upstream that never went down, a floor never tested: `restart` reads `process.started_unixtime`, `upstream-down` requires nginx's log not to grow, the unsigned scenario asserts the 5-minute floor (Tasks 4, 8).
6. **An end-of-life image whose mirror moved** fails the image build, not a scenario: design note 9's rewrite for `bullseye`, and the README's maintenance note (Tasks 2, 11).

## File map

| Area | Files | Task |
|---|---|---|
| The commit-delay seam | `cmd/apt-cacher-ultra/e2ehooks.go`, `e2ehooks_off.go`, `e2ehooks_test.go`, `e2ehooks_off_test.go` (new); `cmd/apt-cacher-ultra/main.go` (`buildAdopter`'s one call); `docs/log-fields.md` (the `e2e_commit_delay_enabled` row) | 1 |
| Fixtures and the image | `e2e/flat/build-fixtures.sh`, `e2e/flat/nginx.conf`, `e2e/flat/Dockerfile` (new); `.dockerignore` | 2 |
| The harness and first contact | `e2e/flat/lib.sh`, `e2e/flat/test.sh`, `e2e/flat/run.sh` (new); `Makefile` (`e2e-flat`) | 3 |
| PR #8's table, the older `Date`, the restart, the status page's backoff | `e2e/flat/test.sh` | 4 |
| The frozen `Date` and the Release-less carry-forward | `e2e/flat/test.sh` | 5 |
| Two suites on one host, mirror mode | `e2e/flat/test.sh` | 6 |
| The 45 s commit | `e2e/flat/test.sh` | 7 |
| The unsigned first adoption, whole-run assertions | `e2e/flat/test.sh` | 8 |
| Definition of done: six images, timings | none (runs and records) | 9 |
| CI | `.github/workflows/ci.yaml`, `.github/workflows/release.yaml` | 10 |
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
10. CI: one `e2e-flat` job per image; the seam's tests and guard
11. Documentation: `e2e/flat/README.md`, the Makefile help, the overview, and the list for H3
12. e2e/deb after eager first adoption (CI's `deb-test`)

---

### Task 1: The commit-delay seam in an `e2ehooks` test binary

Spec §7: "A commit-delay hook of 45 s asserts no client timeouts". The adopter already has the hook (`(*freshness.Adopter).SetCommitDelayForTest`, phase C: every commit sleeps that long inside the flip barrier before `CommitAdoption`), but only Go tests can call it. This task lets a binary built with `-tags e2ehooks` set it from `ACU_E2E_COMMIT_DELAY`, and pins that every other build ignores the variable (design note 12); CI runs these tests and greps the release binary (Task 10). The unit tests pin the parse and the log line; that the delay takes effect is `commit-delay`'s (Task 7: `outcome=hold_expired`), as the AIDEV-NOTE says (exec review m-6).

**Files:**
- Create: `cmd/apt-cacher-ultra/e2ehooks.go`, `cmd/apt-cacher-ultra/e2ehooks_off.go`
- Create (tests): `cmd/apt-cacher-ultra/e2ehooks_test.go`, `cmd/apt-cacher-ultra/e2ehooks_off_test.go`
- Modify: `cmd/apt-cacher-ultra/main.go` (`buildAdopter`, after `freshness.NewAdopter`), `docs/log-fields.md` (the event's row: H1's `TestEveryEventIsDocumented` reads every non-test Go file, build tag or not)

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
// release binary reads ACU_E2E_COMMIT_DELAY (CI greps the release build for
// the string). The seam is the adopter's own test hook
// (Adopter.SetCommitDelayForTest), which the flip simulations already use.
// e2ehooks_test.go pins the parse and the log line; that the delay reaches
// the adopter is pinned by e2e/flat's commit-delay scenario
// (outcome=hold_expired).
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

In `docs/log-fields.md`'s *Operational events* table, immediately before the row that begins `` | `apt-cacher-ultra starting` | INFO | ``, add:

```
| `e2e_commit_delay_enabled` | WARN | `commit_delay` (a Go duration, `45s`), `detail` | Logged once at start, and only by a test binary built with `-tags e2ehooks` (e2e/flat's image, `cmd/apt-cacher-ultra/e2ehooks.go`) when `ACU_E2E_COMMIT_DELAY` is set: every adoption commit then waits `commit_delay` first, inside the flip barrier. A release binary has no such seam, never reads the variable and never logs this line. |
```

- [ ] **Step 4: Run the tests to verify they pass, in both builds**

Run: `go test -count=1 -tags e2ehooks -run 'E2ECommitDelay|ReleaseBinaryIgnores' -v ./cmd/apt-cacher-ultra/`
Expected: `--- PASS` for `TestBuildAdopter_E2ECommitDelayFromTheEnvironment`, `…UnsetIsNoDelay` and `…RejectsABadValue` (subtests `-1s`, `soon`, `45`); `ok`.

Run: `go test -count=1 -run 'E2ECommitDelay|ReleaseBinaryIgnores' -v ./cmd/apt-cacher-ultra/`
Expected: `--- PASS: TestBuildAdopter_ReleaseBinaryIgnoresTheE2ECommitDelay` (subtests `45s`, `soon`); `ok`.

- [ ] **Step 5: Mutation check**

Remove the `applyE2EHooks` call from `buildAdopter` (keep both new files) and run the tagged command of Step 4.
Expected: `--- FAIL: TestBuildAdopter_E2ECommitDelayFromTheEnvironment` and `--- FAIL: TestBuildAdopter_E2ECommitDelayRejectsABadValue` (the planning replay's two failing lines). Restore the call.

Then delete the `e2e_commit_delay_enabled` row from `docs/log-fields.md` and run `go test -count=1 -run TestEveryEventIsDocumented ./internal/observability/` (H1's guard).
Expected: `event_docs_test.go:116: cmd/apt-cacher-ultra/e2ehooks.go:41: event e2e_commit_delay_enabled has no row in docs/log-fields.md; add one`. Restore the row.

- [ ] **Step 6: Full verification**

Run: `gofmt -l . && go vet ./... && go vet -tags e2ehooks ./cmd/apt-cacher-ultra/ && golangci-lint run ./... && golangci-lint run --build-tags e2ehooks ./cmd/... && go test -race -count=1 -timeout 30m ./...`
Expected: `gofmt -l` lists only the five drift files; vet prints nothing; both lint runs print `0 issues.`; every package `ok` (`internal/observability` included: H1's `TestEveryEventIsDocumented` and `TestLogMessagesAreSnakeCase` pass with the new row).

- [ ] **Step 7: Commit**

```bash
gofmt -l cmd/apt-cacher-ultra/e2ehooks.go cmd/apt-cacher-ultra/e2ehooks_off.go cmd/apt-cacher-ultra/e2ehooks_test.go cmd/apt-cacher-ultra/e2ehooks_off_test.go cmd/apt-cacher-ultra/main.go
git add cmd/apt-cacher-ultra/e2ehooks.go cmd/apt-cacher-ultra/e2ehooks_off.go cmd/apt-cacher-ultra/e2ehooks_test.go cmd/apt-cacher-ultra/e2ehooks_off_test.go cmd/apt-cacher-ultra/main.go docs/log-fields.md
git commit -F - <<'EOF'
e2e: a commit-delay seam for the e2ehooks test binary

ACU_E2E_COMMIT_DELAY sets the adopter's commit-delay hook in a binary
built with -tags e2ehooks (e2e/flat); release builds ignore it.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```
The `gofmt -l` line prints nothing.

### Task 2: The fixture repositories, nginx and the test image

Spec §7's fixture list and §3's shapes (design notes 1, 3, 4, 9, 16, 17, 20; ruling R-H2-1). This task builds the image the scenarios run in; it adds no scenario yet, so its test is a smoke run of the image (nginx answers each shape's statuses and the fixtures verify against the generated key) and two mutants the build must refuse.

**Files:**
- Create: `e2e/flat/build-fixtures.sh`, `e2e/flat/nginx.conf`, `e2e/flat/Dockerfile`, and placeholders `e2e/flat/lib.sh`, `e2e/flat/test.sh` (Task 3 replaces them)
- Modify: `.dockerignore`

**Interfaces:**
- Consumes: Task 1's `-tags e2ehooks` (the build stage compiles with it).
- Produces, inside the image: `/fixtures/keys/acu-e2e.gpg` and `/fixtures/keys/fingerprint`; `/fixtures/keys/other.gpg` (a second key that signs nothing: Task 3's wrong-key control); `/fixtures/<repo>/{v1,v2}` and `/fixtures/<repo>/current -> v1` for the repos `k8s cuda nvidia jenkins sublime jitsi dists unsigned404 unsigned403 fury goreleaser` (`sublime/files` holds both builds and is not flipped); the web roots `/fixtures/www/signed.test` and `/fixtures/www/plain.test`; `/etc/acu-e2e/nginx.conf` (pid `/run/acu-e2e/nginx.pid`, access log `/var/log/acu-e2e/nginx-access.log` in the format `$host "$request" $status`, the captive-portal switch `/run/acu-e2e/captive`); the daemon at `/usr/local/bin/apt-cacher-ultra`; the harness at `/usr/local/lib/acu-e2e/{lib.sh,test.sh}`; packages `acu-<repo>` plus `acu-epoch`, `acu-jenkins-direct` and `acu-rogue`, versions `1.0`/`2.0` (`acu-epoch` `1:1.0`/`1:2.0`), each installing `/usr/share/acu-e2e/<name>` holding `<name> <version>`.

- [ ] **Step 1: Write `e2e/flat/build-fixtures.sh`**

```bash
#!/bin/bash
# build-fixtures.sh builds the e2e/flat upstream repositories under $1
# (default /fixtures), two versions of each, and the web roots nginx
# serves them from. It runs in the Dockerfile's `fixtures` stage.
#
# Layout produced:
#   keys/acu-e2e.gpg, keys/fingerprint   the throwaway signing key
#   keys/other.gpg                       a second key that signs nothing
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
cleanup() {
    local h
    for h in "$WORK"/gnupg*; do
        GNUPGHOME=$h gpgconf --kill gpg-agent 2> /dev/null || true
    done
    rm -rf "$WORK"
}
trap cleanup EXIT

export GNUPGHOME="$WORK/gnupg"
mkdir -m 700 "$GNUPGHOME"
gpg --batch --quiet --pinentry-mode loopback --passphrase '' \
    --quick-generate-key 'apt-cacher-ultra e2e-flat throwaway key <e2e-flat@example.invalid>' \
    rsa3072 sign 0
FPR="$(gpg --list-keys --with-colons | awk -F: '/^fpr:/ {print $10; exit}')"
mkdir -p "$OUT/keys"
gpg --batch --yes --output "$OUT/keys/acu-e2e.gpg" --export "$FPR"
printf '%s\n' "$FPR" > "$OUT/keys/fingerprint"

# A second throwaway key that signs nothing: first-contact checks a signed
# source against it and expects apt's signature error.
mkdir -m 700 "$WORK/gnupg-other"
GNUPGHOME="$WORK/gnupg-other" gpg --batch --quiet --pinentry-mode loopback --passphrase '' \
    --quick-generate-key 'apt-cacher-ultra e2e-flat unrelated key <e2e-flat-other@example.invalid>' \
    rsa3072 sign 0
GNUPGHOME="$WORK/gnupg-other" gpg --batch --yes --output "$OUT/keys/other.gpg" --export

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

# Shape self-checks: a scenario relying on one of these shapes would
# pass vacuously without it (frozen-date, first-contact's inline
# adoption of a SHA512-only Release).
if [[ $(grep '^Date:' "$OUT/nvidia/v1/InRelease") != "$(grep '^Date:' "$OUT/nvidia/v2/InRelease")" ]]; then
    echo "build-fixtures: libnvidia-container's v1 and v2 Dates differ (want one frozen Date)" >&2
    exit 1
fi
if grep -qE '^(MD5Sum|SHA1|SHA256):' "$OUT/nvidia/v1/InRelease" "$OUT/nvidia/v2/InRelease"; then
    echo "build-fixtures: libnvidia-container's Release is not SHA512-only" >&2
    exit 1
fi

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
RUN apt-get -o Acquire::Retries=3 update && \
    apt-get -o Acquire::Retries=3 install -y --no-install-recommends \
        apt-utils dpkg-dev gnupg gzip xz-utils && \
    rm -rf /var/lib/apt/lists/*
COPY e2e/flat/build-fixtures.sh /usr/local/bin/build-fixtures.sh
RUN bash /usr/local/bin/build-fixtures.sh /fixtures

FROM ${CLIENT_IMAGE} AS test
ENV DEBIAN_FRONTEND=noninteractive
# Debian 11's LTS ended on 2026-08-31 and its packages moved to
# archive.debian.org; the image still names deb.debian.org (design note 9).
# bullseye-security stays: the image's own packages (perl-base) come from
# it, so apt-file's perl cannot install without it. Retries: apt 2.2
# (Debian 11) makes none by default.
RUN . /etc/os-release && \
    if [ "$VERSION_CODENAME" = bullseye ]; then \
        printf '%s\n' 'deb http://archive.debian.org/debian bullseye main' \
            'deb http://archive.debian.org/debian-security bullseye-security main' \
            > /etc/apt/sources.list; \
    fi && \
    rm -f /etc/apt/apt.conf.d/docker-* && \
    apt-get -o Acquire::Retries=3 update && \
    apt-get -o Acquire::Retries=3 install -y --no-install-recommends \
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
gpgv --keyring /fixtures/keys/other.gpg /fixtures/k8s/v2/InRelease 2>&1 | tail -1
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
gpgv: Can't check signature: No public key
2
Acquire-By-Hash: yes
Date: Sat, 24 Oct 2026 05:46:26 UTC
Suite: stable
v1
```
The second `Good signature` is Jitsi's binary `Release.gpg`; the third `gpgv` shows the second key verifies nothing; the count is the two SHA512 lines of libnvidia-container's SHA512-only `Release` text.

Run: `docker build -f e2e/flat/Dockerfile --build-arg CLIENT_IMAGE=debian:11 -t acu-e2e-flat:debian-11 .`
Expected: success; the test stage's `apt-get update` lists `http://archive.debian.org/debian bullseye` and `bullseye-security` (design note 9).

- [ ] **Step 6: Mutation checks (the shape self-checks)**

Run each, one at a time, reverting before the next, with `docker build --target fixtures -f e2e/flat/Dockerfile .`:
1. In `build_nvidia`, replace `release "$d" "$DATE_FROZEN" \` with `release "$d" "$(date -u -d "-$((3 - ${2%%.*})) days" '+%a, %d %b %Y %H:%M:%S UTC')" \` (a moving Date).
   Expected: `build-fixtures: libnvidia-container's v1 and v2 Dates differ (want one frozen Date)`, and the build fails (`exit code: 1`).
2. In `build_nvidia`, delete the line `-o APT::FTPArchive::Release::SHA256=false` (and the `\` before it).
   Expected: `build-fixtures: libnvidia-container's Release is not SHA512-only`, and the build fails.

- [ ] **Step 7: Lint and commit**

Run: `shellcheck -x e2e/flat/*.sh`
Expected: no output.

```bash
git add e2e/flat/build-fixtures.sh e2e/flat/nginx.conf e2e/flat/Dockerfile e2e/flat/lib.sh e2e/flat/test.sh .dockerignore
git commit -F - <<'EOF'
e2e/flat: fixture repositories, nginx and the test image

Every flat repository shape of spec §3 (k8s, CUDA, libnvidia-container,
Jenkins, Sublime Text, Jitsi), two unsigned variants, two Release-less
repositories and a dists/ repository, two versions each behind a
`current` symlink, signed with a key generated at build time; the build
checks the shapes the scenarios rely on.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```
The placeholders keep this commit's image buildable; Task 3 replaces both.

### Task 3: The harness, `run.sh`, `make e2e-flat` and the first-contact scenario

Spec §7: "installs succeed; the exact request lines apt sends are pinned (`./`, `%253a`, `by-hash/SHA512`); `apt-get` output is grepped …; zero `unvouched_deb_refused` on the signed host", the Jitsi fixture's "`Contents-amd64` via apt-file, a `deb-src` line", and the signed/unsigned host split. This task writes the harness library, the driver and the first scenario, which every later scenario builds on (design notes 2, 5, 6, 8, 14, 17, 18, 19; rulings R-H2-2, R-H2-5; Review Focus 1, 3, 4). Every check here that could pass without its behaviour has a control in `setup` or `first_contact` that shows it can fail.

**Files:**
- Replace (Task 2's placeholders): `e2e/flat/lib.sh`, `e2e/flat/test.sh`
- Create: `e2e/flat/run.sh`
- Modify: `Makefile` (`.PHONY`, the `e2e-flat` target)

**Interfaces:**
- Consumes: Task 2's image layout (Interfaces there), including `/fixtures/keys/other.gpg`; Task 1's `e2e_commit_delay_enabled` (used from Task 7).
- Produces, in `lib.sh` (sourced by `test.sh`; later tasks use exactly these): settings `DAEMON_LOG NGINX_LOG CONFIG KEY PROXY ADMIN MIRROR COOLDOWN=2 PERIODIC=30 TICK=8 REACH APT_ERRORS APT_SIGNED_HOSTS APT_SIGNED_ERRORS APT_RESIDUAL`, state `FAILED` (set by `fail`) and `BASELINE` (set by `baseline`); `say MSG`, `fail MSG`, `scenario NAME DESC` (sets `SCENARIO_MARK`, the daemon-log line count at its start), `passed`, `poll SECONDS WHAT CMD…` and `poll_quietly SECONDS CMD…` (both set `POLL_TOOK`), `log_mark`, `re TEXT`, `kv KEY VALUE`, `event NAME`, `suite_kv PATH`, `log_lines MARK REGEX…`, `log_count`, `log_has`, `harness_mark FIELDS`, `start_nginx`, `stop_nginx`, `start_daemon [NAME=VALUE…]`, `stop_daemon`, `publish REPO v1|v2`, `suite HOST PATH FIELD`, `adopted_as HOST PATH FORM`, `metric SAMPLE` (0 when absent), `baseline START HOST SUITE…`, `by_client`, `client NAME PROXY|DIRECT SOURCE…`, `capt NAME CMD…`, `apt_update NAME [--allow=REGEX] [apt-get options…]` (sets `APT_LOG`), `apt_errors FILE [ALLOW]`, `candidate NAME PKG`, `candidates_are NAME PKG=VER…`, `apt_install NAME PKG=VER… [-- apt-get options…]` (sets `APT_LOG`), `until_candidates LIMIT NAME… -- PKG=VER…` (sets `POLL_TOOK`), `apt_sent_line REGEX` (a test), `apt_sent REGEX` (or fail), `upstream_got HOST PATH-REGEX`. In `test.sh`: the `EXIT`, `INT` and `TERM` traps (`on_exit`), the `SRC_*` source lines, the suite paths `K8S CUDA NVIDIA JENKINS SUBLIME JITSI DISTS`, `write_config`, `setup`, `first_contact` (sets `UNSIGNED_FIRST_CONTACT`, Task 8's clock) and the run list at the end of the file. In `run.sh`: `E2E_FLAT_IMAGES`, `E2E_FLAT_UNSIGNED_IMAGES` (set but empty: none), `E2E_FLAT_NAME`; the container runs with `--init` and gets `E2E_IMAGE` and `E2E_UNSIGNED_WAIT`.

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
# ticks every PERIODIC/4 seconds (5 s at least): 7.5 s, rounded up here.
COOLDOWN=2
PERIODIC=30
TICK=8
# The longest a published change may take to reach a client that runs
# apt-get update once a cooldown: one cooldown for its check to fire,
# then the adoption and the flip barrier (phase A's quiet period is 1 s
# after the client's last anchor; measured 2–7 s; design note 5). by_client
# proves the client's check carried it, not the scheduler.
REACH=$((COOLDOWN + 20))

# apt-get update's exit status says nothing about failed items (spec §7):
# its output is grepped instead.
APT_ERRORS='Failed to fetch|Connection failed|timed out|Hash Sum mismatch|Mirror sync in progress'
# apt reports a signature failure in none of those words, keeps the old
# lists and exits 0, so the next update hides it: "GPG error" (apt 2.x),
# "OpenPGP signature verification failed" (apt 3.x, sqv), "is not signed",
# "error occurred during the signature verification", "Clearsigned file
# isn't valid", and "Conflicting distribution" for a wrong Suite.
# Release-less and unsigned repositories behind a captive portal print the
# same words (design note 11), so these words count only on lines naming
# the signed host or the mirror prefix (design note 17).
APT_SIGNED_HOSTS='//signed\.test/|/m-signed/'
APT_SIGNED_ERRORS="GPG error|OpenPGP signature verification failed|is not signed|signature verification|Clearsigned file isn't valid|Conflicting distribution"
# The commit-delay scenario's scope (spec §7: its own grep): every error
# but the phase-B-expiry straddle's — indexes newer than the InRelease a
# held client got (spec C8's documented residual).
APT_RESIDUAL='Hash Sum mismatch|unexpected size|Mirror sync in progress'

SCENARIO=setup
SCENARIO_START=$SECONDS
SCENARIO_MARK=0
DAEMON_PID=""
APT_RUNS=0
TIMINGS=()
FAILED=0
BASELINE=0

say() {
    printf '[e2e-flat %s %4ds] %s\n' "$E2E_IMAGE" "$SECONDS" "$*"
}

fail() {
    FAILED=1
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

# re QUOTED-TEXT: TEXT as an extended regex that matches it literally
# (`/` is not special in an ERE, so it stays as it is).
re() {
    printf '%s' "$1" | sed -e 's/[][\.*^$+?(){}|]/\\&/g'
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
    poll 30 "the daemon's admin listener" curl -fs -o /dev/null "$ADMIN/healthz"
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

# metric NAME-AND-LABELS: the value of one sample on /metrics (0 when it
# has none yet).
metric() {
    curl -fsS "$ADMIN/metrics" | awk -v m="$1" '$1 == m {v = $2} END {print v + 0}'
}

# baseline START HOST SUITE...: waits (10 s at most) until every suite's
# last successful check is no older than START less a cooldown — the
# checks the client updates since START triggered, or the one whose
# cooldown made them skip theirs — and sets BASELINE to the oldest.
baseline() {
    local start=$1
    shift
    poll 10 "the suites' checks after the client's update" baseline_met "$start" "$@"
}

baseline_met() {
    local start=$1 host=$2 s v min=""
    shift 2
    for s in "$@"; do
        v=$(suite "$host" "$s" last_success_unixtime)
        [[ -n $v ]] || return 1
        if [[ -z $min ]] || ((v < min)); then min=$v; fi
    done
    ((min >= start - COOLDOWN)) || return 1
    BASELINE=$min
}

# by_client: the client saw the publish before the scheduler could have
# checked the suites again — one periodic_refresh after their last
# success, BASELINE — so a client-triggered check carried it (spec C4's
# T1), not the periodic refresh (design note 5).
by_client() {
    local now
    now=$(date +%s)
    ((now < BASELINE + PERIODIC)) ||
        fail "the publish reached the client $((now - BASELINE))s after the suites' last success; the scheduler could have carried it (want under ${PERIODIC}s)"
    say "carried by the client's check: $((now - BASELINE))s after the suites' last success (the scheduler: ${PERIODIC}s)"
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

# apt_update NAME [--allow=REGEX] [apt-get options...]: apt-get update on
# client NAME; fails when its output holds an apt error (apt_errors),
# except lines matching REGEX. The output is kept in APT_LOG.
apt_update() {
    local name=$1 allow=""
    shift
    if [[ ${1:-} == --allow=* ]]; then
        allow=${1#--allow=}
        shift
    fi
    APT_RUNS=$((APT_RUNS + 1))
    APT_LOG=/clients/$name/update.$APT_RUNS.log
    capt "$name" apt-get update "$@" > "$APT_LOG" 2>&1 || true
    if [[ -n $(apt_errors "$APT_LOG" "$allow") ]]; then
        sed 's/^/    | /' "$APT_LOG"
        fail "apt-get update on client $name printed an error (above)"
    fi
}

# apt_errors FILE [ALLOW]: the error lines of apt output FILE — those
# matching APT_ERRORS, and signature errors naming the signed host or the
# mirror prefix — less the lines matching ALLOW. Every grep reads all its
# input: no early exit for pipefail to see.
apt_errors() {
    local errs
    errs=$({
        grep -E -- "$APT_ERRORS" "$1"
        grep -E -- "$APT_SIGNED_HOSTS" "$1" | grep -E -- "$APT_SIGNED_ERRORS"
    } || true)
    if [[ -n ${2:-} && -n $errs ]]; then
        errs=$(grep -Ev -- "$2" <<< "$errs" || true)
    fi
    printf '%s' "$errs"
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
        > "$APT_LOG" 2>&1 || [[ -n $(apt_errors "$APT_LOG") ]]; then
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

# apt_sent_line REGEX: client debug output (Debug::Acquire::http) in
# APT_LOG holds a request line apt sent that matches REGEX exactly. apt
# prints the header lines as sent, CRLF included. grep reads the file
# itself: no pipe, so grep -q's early exit cannot fail anything under
# pipefail.
apt_sent_line() {
    grep -Eq -- "^GET $1 HTTP/1\\.1"$'\r'"?\$" "$APT_LOG"
}

# apt_sent REGEX: apt_sent_line, or fail.
apt_sent() {
    apt_sent_line "$1" || fail "apt did not send GET $1 (Debug::Acquire::http output in $APT_LOG)"
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

# A command that fails outside fail (nginx not binding, dpkg, a publish's
# mv) exits through set -e: still name the scenario and show the log.
on_exit() {
    local rc=$?
    if ((rc != 0 && FAILED == 0)); then
        say "unexpected exit $rc in scenario $SCENARIO"
        tail -n 80 "$DAEMON_LOG" 2> /dev/null | sed -E 's/ client_addr=[^ ]+//'
    fi
}
trap on_exit EXIT
# Ctrl-C on docker run, or docker stop: end the run at the next command
# boundary (bash as PID 1 would ignore these signals without a handler).
trap 'fail "interrupted by a signal"' INT TERM

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
    # (tick 7.5 s) and the quietest flip barrier; the unsigned confirmation
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
    # The waits are not vacuous: a condition that never holds times out.
    ! poll_quietly 0 false || fail "poll_quietly returned success for a condition that never held"
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
    # The error checks are not vacuous on this apt: apt_update must fail an
    # update of a source the cache refuses (a host outside its allowlist),
    # and one of a signed source checked against a key that did not sign it
    # (apt then prints no "Failed to fetch": only the signature words count).
    client control "$PROXY" "deb [trusted=yes] http://nowhere.test/ /"
    if (apt_update control) > /clients/control/apt_update.out 2>&1; then
        sed 's/^/    | /' /clients/control/update.*.log
        fail "apt_update passed a failed update: the apt error grep is vacuous (above)"
    fi
    client control-sig "$PROXY" "deb [signed-by=/fixtures/keys/other.gpg] http://signed.test/debian acu main"
    if (apt_update control-sig) > /clients/control-sig/apt_update.out 2>&1; then
        sed 's/^/    | /' /clients/control-sig/update.*.log
        fail "apt_update passed a signature failure: the signed-source error list is vacuous (above)"
    fi

    UNSIGNED_FIRST_CONTACT=$SECONDS
    apt_update main -o Debug::Acquire::http=true
    # apt_sent is not vacuous: a line apt never sent does not match.
    if apt_sent_line 'http://signed\.test/jitsi/stable/no-such-file'; then
        fail "apt_sent matched a request line apt never sent"
    fi
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

    # Jitsi's Contents-amd64 through apt-file, and its Sources. (Outputs go
    # to files: a grep -q at the end of a pipe can fail it under pipefail.)
    capt main apt-file search /usr/share/acu-e2e/acu-jitsi > /clients/main/apt-file.out
    grep -q '^acu-jitsi: /usr/share/acu-e2e/acu-jitsi$' /clients/main/apt-file.out ||
        fail "apt-file did not find acu-jitsi's file in Contents-amd64"
    capt main apt-cache showsrc acu-jitsi > /clients/main/showsrc.out
    grep -q '^Package: acu-jitsi$' /clients/main/showsrc.out ||
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
#                             first-adoption scenario, which waits for its
#                             confirmation, 5 to 15 minutes after first
#                             contact (default: the oldest and the newest;
#                             set but empty: none)
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
    printf '%s\0' "${E2E_FLAT_UNSIGNED_IMAGES-debian:11 ubuntu:26.04}"
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
    # A container left by an interrupted run would take the name. --init
    # puts a real init first, so Ctrl-C and docker stop end the test.
    docker rm -f "$name-$tag" > /dev/null 2>&1 || true
    if ! docker run --rm --init --name "$name-$tag" \
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
[e2e-flat debian:12   11s] --- scenario first-contact passed (10s)
[e2e-flat debian:12   11s] stopping the daemon
[e2e-flat debian:12   12s] PASS in 12s: first-contact 10s
[e2e-flat] debian:12 PASS

[e2e-flat] debian:12: build …s, test 13s
[e2e-flat] OVERALL PASS (debian:12)
```

- [ ] **Step 6: Mutation checks (the harness has teeth)**

Run each, one at a time, reverting before the next, with `E2E_FLAT_IMAGES=debian:12 make e2e-flat`:
1. In `write_config`, `refuse_unvouched_debs = true` → `false`.
   Expected: `FAIL in scenario 'first-contact': an undeclared .deb on signed.test answered 200, want 502 (strict mode)`.
2. In `lib.sh`, `APT_ERRORS='Failed to fetch|…'` → `APT_ERRORS='no-such-apt-message'`.
   Expected: the control update's output, among it `    | E: Failed to fetch http://nowhere.test/Packages  403  Forbidden [IP: 127.0.0.1 3142]`, then `FAIL in scenario 'first-contact': apt_update passed a failed update: the apt error grep is vacuous (above)`.
3. In `apt_update`, `if [[ -n $(apt_errors "$APT_LOG" "$allow") ]]; then` → `if false; then` (exec review I-3: `apt_update` must apply its grep).
   Expected: the same output and line as 2.
4. In `lib.sh`, `APT_SIGNED_ERRORS="GPG error|…"` → `APT_SIGNED_ERRORS="no-such-apt-message"` (design review I-3).
   Expected: the wrong-key control's output — apt 2.6.1 prints `    | W: GPG error: http://signed.test/debian acu InRelease: The following signatures couldn't be verified because the public key is not available: NO_PUBKEY <16 hex>` and `    | E: The repository 'http://signed.test/debian acu InRelease' is not signed.`, and no `Failed to fetch` — then `FAIL in scenario 'first-contact': apt_update passed a signature failure: the signed-source error list is vacuous (above)`.
5. In `apt_sent_line`, the pattern → `"^GET "` (any request line).
   Expected: `FAIL in scenario 'first-contact': apt_sent matched a request line apt never sent`.
6. In `poll_quietly`, `((SECONDS - start < limit)) || return 1` → `|| return 0`.
   Expected: `FAIL in scenario 'setup': poll_quietly returned success for a condition that never held`.
7. In `first_contact`, add a line `false` before `client control …` (an exit outside `fail`, design review M-8).
   Expected: `unexpected exit 1 in scenario first-contact`, then the daemon's log tail.
8. No edit (design review M-9): run the image as `run.sh` does, `docker run -d --init --name acu-e2e-flat-sig -e E2E_IMAGE=debian:12 acu-e2e-flat:debian-12`; once `docker logs acu-e2e-flat-sig` shows `=== scenario first-contact`, run `docker kill --signal=INT acu-e2e-flat-sig`.
   Expected: the container exits within seconds (the planning run: 1 s; `docker inspect -f '{{.State.Status}} {{.State.ExitCode}}'` prints `exited 1`), its log ending `FAIL in scenario 'first-contact': interrupted by a signal` and the log tails. Then `docker rm -f acu-e2e-flat-sig`. Without the `INT` trap the container keeps running (the planning run, with and without `--init`): bash as PID 1 ignores the signal, and tini forwards it to bash alone.

- [ ] **Step 7: Lint and commit**

Run: `shellcheck -x -f gcc e2e/flat/*.sh`
Expected: exactly these findings — names a later task uses — and nothing else (`SRC_K8S_DOT` is Task 4's; `start_daemon` is called without arguments until Task 7 passes one; `UNSIGNED_FIRST_CONTACT` is Task 8's):
```
e2e/flat/test.sh:34:1: warning: SRC_K8S_DOT appears unused. Verify use (or export if used externally). [SC2034]
e2e/flat/test.sh:106:5: note: Use start_daemon "$@" if function's $1 should mean script's $1. [SC2119]
e2e/flat/test.sh:131:5: warning: UNSIGNED_FIRST_CONTACT appears unused. Verify use (or export if used externally). [SC2034]
```

```bash
git add e2e/flat/lib.sh e2e/flat/test.sh e2e/flat/run.sh Makefile
git commit -F - <<'EOF'
e2e/flat: the harness, make e2e-flat and the first-contact scenario

Every repository through one apt client: each signed suite adopts in its
upstream's form, the Release-less ones refresh without a Release, the
unsigned ones wait for confirmation; installs of every Filename shape are
vouched; apt's ./, %253a and by-hash/SHA512 request lines are pinned on
both sides; strict mode refuses an undeclared .deb; apt-file reads
Jitsi's Contents and deb-src its Sources. The update check fails a
refused host and a wrong signing key, signature errors included, so it
cannot pass vacuously on any apt; an exit or a signal names the
scenario.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

### Task 4: PR #8's end-to-end table, the older `Date` and the restart

Spec §9's H row and the overview's H line: "one signed flat repository configured as `deb <uri>/ /` on one client and `deb <uri> ./` on another; both report 2.0 within one cooldown of the v2 publish, and with the upstream stopped two fresh clients install 2.0 from the snapshots"; spec §7: "an older-Date publish is ignored; state survives a restart" (design note 5; Review Focus 2, 5). "Within one cooldown" is checked twice: on time (`until_candidates "$REACH"`) and on path (`by_client`: before the scheduler could have checked the suites). The restart is proven by `process.started_unixtime`, the upstream's absence by nginx's access log, and the older `Date` by the cache's own state, not apt's (apt alone keeps an existing client at 2.0). While nginx is down, `upstream-down` also proves `main.go`'s end of the `CheckBackoff` wiring (the overview's H1 carry, parked item 12): the k8s suite's checks fail, and its status-page row must show `checks failing ×k · backoff ends in …`; without the wiring it shows the count alone (`internal/admin`'s `annotateSuiteEntries`).

**Files:**
- Modify: `e2e/flat/test.sh`

**Interfaces:**
- Consumes: Task 3's `lib.sh` and `test.sh` names (`baseline`, `by_client`, `REACH`, `PERIODIC`).
- Produces: the clients `k8s-slash` (`SRC_K8S`), `k8s-dot` (`SRC_K8S_DOT`), `k8s-fresh`, `fresh-slash`, `fresh-dot`; the twin suite `$K8S/.` (adopted inline); `k8s` published at v2 when this task's scenarios end (the older-`Date` scenario publishes v1 and restores v2); `snapshot_ids` and `main_candidates` (the comparisons of `older-date` and `restart`); `backoff_shown` (Task 8 adds `whole_run` after it).

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

    local start
    start=$(date +%s)
    apt_update k8s-slash
    apt_update k8s-dot
    baseline "$start" signed.test "$K8S" "$K8S/."
    publish k8s v2
    until_candidates "$REACH" k8s-slash k8s-dot -- acu-k8s=2.0
    by_client
    say "both source forms report acu-k8s 2.0 ${POLL_TOOK}s after the publish (cooldown ${COOLDOWN}s)"
    apt_install k8s-slash acu-k8s=2.0
    apt_install k8s-dot acu-k8s=2.0
    passed
}

# apt ignores an older Date; so does the cache (spec C5, upstream_older).
# apt alone would keep the existing clients at 2.0, so the check that
# counts is the cache's: the snapshots stay and a fresh client gets 2.0.
older_date() {
    scenario older-date "a publish with an older Date is refused; clients keep 2.0"
    local s before
    before=$(snapshot_ids)
    publish k8s v1
    for s in "$K8S" "$K8S/."; do
        poll "$REACH" "signed.test$s to refuse the older Date" refused_older "$s"
    done
    apt_update k8s-slash
    apt_update k8s-dot
    candidates_are k8s-slash acu-k8s=2.0 || fail "k8s-slash went back to $(candidate k8s-slash acu-k8s)"
    candidates_are k8s-dot acu-k8s=2.0 || fail "k8s-dot went back to $(candidate k8s-dot acu-k8s)"
    [[ $(snapshot_ids) == "$before" ]] || fail "the older-Date publish changed a snapshot: $before -> $(snapshot_ids)"
    client k8s-fresh "$PROXY" "$SRC_K8S"
    apt_update k8s-fresh
    candidates_are k8s-fresh acu-k8s=2.0 || fail "a fresh client sees acu-k8s $(candidate k8s-fresh acu-k8s), want 2.0"
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
    local before after cands_before cands_after stopped_at
    apt_update main
    before=$(snapshot_ids)
    cands_before=$(main_candidates)
    stopped_at=$(date +%s)
    stop_daemon
    start_daemon
    (($(curl -fsS "$ADMIN/?format=json" | jq .process.started_unixtime) >= stopped_at)) ||
        fail "the daemon was not restarted"
    after=$(snapshot_ids)
    [[ $before == "$after" ]] || fail "snapshots changed across the restart: $before -> $after"
    apt_update main
    cands_after=$(main_candidates)
    [[ $cands_before == "$cands_after" ]] || fail "candidates changed across the restart: $cands_before -> $cands_after"
    curl -fsS -D /clients/main/k8s-InRelease.headers -o /dev/null -x "$PROXY" "http://signed.test$K8S/InRelease"
    grep -qi '^x-cache-snapshot:' /clients/main/k8s-InRelease.headers ||
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
# (one per source form) install 2.0 from the snapshots. Meanwhile the
# checks their updates trigger fail, and the status page shows the failing
# checks' backoff (backoff_shown).
upstream_down() {
    scenario upstream-down "upstream stopped: fresh clients of both forms install 2.0 from the snapshots"
    stop_nginx
    local upstream_lines
    upstream_lines=$(wc -l < "$NGINX_LOG")
    client fresh-slash "$PROXY" "$SRC_K8S"
    client fresh-dot "$PROXY" "$SRC_K8S_DOT"
    apt_update fresh-slash
    apt_update fresh-dot
    apt_install fresh-slash acu-k8s=2.0
    apt_install fresh-dot acu-k8s=2.0
    # Nothing came from the upstream: these clients were served from the
    # snapshots alone.
    (($(wc -l < "$NGINX_LOG") == upstream_lines)) || fail "the upstream answered during upstream-down"
    # The overview's H1 carry (parked item 12): main.go wires
    # admin.Config.CheckBackoff, without which a failing suite's row shows
    # only "checks failing ×k". A client-triggered check fails within a
    # cooldown of an update; the scheduler's own check of the suite comes
    # within periodic_refresh and a tick of its last success at the latest.
    poll $((PERIODIC + TICK + 10)) "the status page to show a failing check's backoff" backoff_shown
    say "the status page shows $K8S's failing checks with their backoff ${POLL_TOOK}s into the poll"
    start_nginx
    passed
}

# backoff_shown: a client update, whose check of $K8S fails while nginx is
# stopped (a refused connection ends a check unknown: spec C4's failed
# check), then $K8S's check_fail_count is positive and its row on the
# status page (the HTML at /, one <tr> per suite) shows "backoff ends in".
# internal/admin's annotateSuiteEntries writes that text only for a suite
# with a failing count, a last_check_at and a backoff the scheduler keeps
# (unscheduled is false): an adopted suite qualifies unless an
# anchor_absent or unsigned_disabled record paces it, and $K8S's only
# possible record is older-date's upstream_older.
backoff_shown() {
    local n
    apt_update fresh-slash
    n=$(suite signed.test "$K8S" check_fail_count)
    ((${n:-0} > 0)) || return 1
    curl -fsS -o /clients/fresh-slash/status.html "$ADMIN/"
    [[ -n $(awk -v RS='</tr>' -v p="$K8S" '(index($0, ">" p "<") || index($0, ">" p " ")) && index($0, "backoff ends in")' \
        /clients/fresh-slash/status.html) ]]
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
[e2e-flat debian:12   11s] === scenario pr8-table: one flat repository in both source forms; both see 2.0 after the v2 publish
[e2e-flat debian:12   12s] published k8s v2
[e2e-flat debian:12   17s] carried by the client's check: 6s after the suites' last success (the scheduler: 30s)
[e2e-flat debian:12   17s] both source forms report acu-k8s 2.0 5s after the publish (cooldown 2s)
[e2e-flat debian:12   17s] --- scenario pr8-table passed (6s)
[e2e-flat debian:12   17s] === scenario older-date: a publish with an older Date is refused; clients keep 2.0
[e2e-flat debian:12   18s] published k8s v1
[e2e-flat debian:12   18s] published k8s v2
[e2e-flat debian:12   18s] --- scenario older-date passed (1s)
[e2e-flat debian:12   18s] === scenario restart: state survives a restart
[e2e-flat debian:12   21s] --- scenario restart passed (3s)
[e2e-flat debian:12   21s] === scenario upstream-down: upstream stopped: fresh clients of both forms install 2.0 from the snapshots
[e2e-flat debian:12   …s] the status page shows /core:/stable:/v1.34/deb's failing checks with their backoff …s into the poll
[e2e-flat debian:12   …s] --- scenario upstream-down passed (…s)
[e2e-flat debian:12   …s] PASS in …s: first-contact 10s pr8-table 6s older-date 1s restart 3s upstream-down …s
```
nginx also prints `[notice] … signal process started` on stderr when `upstream-down` stops it; harmless. The `backoff_shown` line and the times after it are not measured: revision 3 added that poll without Docker. Record the poll's time here and in Task 9; expect a few seconds (a refused connection fails the client-triggered check at once: `fetch.Client.Conditional` makes one attempt). If the 48 s bound ever flakes, widen it with its reason; never drop the check.

- [ ] **Step 5: Mutation checks**

Run each, one at a time, reverting before the next, with `E2E_FLAT_IMAGES=debian:12 make e2e-flat`:
1. Delete `pr8_table`'s `publish k8s v2` line.
   Expected: `FAIL in scenario 'pr8-table': clients k8s-slash k8s-dot did not report acu-k8s=2.0 within 22s`.
2. In `pr8_table`, after `publish k8s v2`, add `sleep $((PERIODIC + TICK + 5))` (the scheduler gets its turn first; design review I-2).
   Expected: `FAIL in scenario 'pr8-table': the publish reached the client 44s after the suites' last success; the scheduler could have carried it (want under 30s)` (the seconds vary).
3. In `restart`, delete the `stop_daemon` and `start_daemon` lines (exec review I-1).
   Expected: `FAIL in scenario 'restart': the daemon was not restarted`.
4. In `upstream_down`, delete the `stop_nginx` and `start_nginx` lines (exec review I-2).
   Expected: `FAIL in scenario 'upstream-down': the upstream answered during upstream-down`.
5. A daemon mutant, in a scratch copy only (Global Constraints; the overview's H1 carry): in `cmd/apt-cacher-ultra/main.go`, delete the line `CheckBackoff:            freshness.CheckBackoff,` (`serveListeners`' `admin.Config`).
   Expected: `FAIL in scenario 'upstream-down': timed out after 48s waiting for the status page to show a failing check's backoff`. Not replayed in revision 3 (no Docker): run it once with the implementation.

`older-date`'s new checks (the snapshot ids, the fresh client) have no single-site mutant: a daemon that skips the `upstream_older` refusal fails the poll before them, and the planning runs found no edit that makes the cache adopt v1 while still logging the refusal. They are equalities a correct daemon passes by construction.

- [ ] **Step 6: Lint and commit**

Run: `shellcheck -x -f gcc e2e/flat/*.sh`
Expected: exactly these findings — names a later task uses — and nothing else (`setup`'s and `restart`'s `start_daemon` until Task 7 passes an argument; `UNSIGNED_FIRST_CONTACT` until Task 8):
```
e2e/flat/test.sh:106:5: note: Use start_daemon "$@" if function's $1 should mean script's $1. [SC2119]
e2e/flat/test.sh:131:5: warning: UNSIGNED_FIRST_CONTACT appears unused. Verify use (or export if used externally). [SC2034]
e2e/flat/test.sh:278:5: note: Use start_daemon "$@" if function's $1 should mean script's $1. [SC2119]
```

```bash
git add e2e/flat/test.sh
git commit -F - <<'EOF'
e2e/flat: PR #8's end-to-end table, the older Date and the restart

Both source forms of one signed flat repository report 2.0 within one
cooldown of its v2 publish, carried by the clients' own checks, and, with
the upstream stopped (its access log unchanged), fresh clients of both
forms install 2.0 from the snapshots, and the status page shows the
failing checks' backoff (main.go's CheckBackoff wiring); an older-Date
publish is refused
and changes no snapshot; after a real restart the snapshots and
candidates are unchanged.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

### Task 5: The frozen `Date` and the Release-less repositories (the phase G carry-forward)

Spec §7: "a v2 publish of a signed or anchorless repo reaches clients within one cooldown", the libnvidia-container shape (InRelease only, SHA512-only, frozen `Date`); the overview's *(G)* carry-forward: "a Release-less repository (a Gemfury-shaped `Packages.gz` with no `Release`): a republished index reaches a client within one `periodic_refresh`, and an upstream that then answers HTML keeps the old index" (design notes 5, 10, 11).

**Files:**
- Modify: `e2e/flat/test.sh`

**Interfaces:**
- Consumes: Task 3's names (`baseline`, `by_client`, `metric`); Task 2's captive switch `/run/acu-e2e/captive`.
- Produces: `nvidia`, `goreleaser` and `fury` published at v2; `update_until_rejected BEFORE`, `fury_clean`.

- [ ] **Step 1: Add the scenarios after `refused_older`**

```bash
# libnvidia-container's Date never changes: a new publish with the same
# Date adopts (spec C5: only an older Date is refused).
frozen_date() {
    scenario frozen-date "an InRelease-only, SHA512-only repository with a frozen Date publishes v2"
    local start
    start=$(date +%s)
    apt_update main
    baseline "$start" signed.test "$NVIDIA"
    publish nvidia v2
    until_candidates "$REACH" main -- acu-nvidia=2.0
    by_client
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
    local start rejected
    start=$(date +%s)
    apt_update main
    baseline "$start" plain.test /goreleaser
    publish goreleaser v2
    until_candidates "$REACH" main -- acu-goreleaser=2.0
    by_client
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
    rejected=$(metric 'acu_anchorless_sniff_rejected_total{host="plain.test"}')
    touch /run/acu-e2e/captive
    # A client during the outage keeps its index: the cache answers the
    # index from its copy, and the check an update triggers (once a
    # cooldown) rejects the portal's page. apt also reports the page it got
    # for the missing Release, as it would fetching directly.
    poll "$REACH" "an anchorless check to reject the HTML page" update_until_rejected "$rejected"
    candidates_are main acu-fury=2.0 || fail "behind the portal the client sees acu-fury $(candidate main acu-fury)"
    curl -fsS -o /clients/main/fury-Packages.gz -x "$PROXY" http://plain.test/fury/Packages.gz
    zcat /clients/main/fury-Packages.gz > /clients/main/fury-Packages
    grep -q '^Version: 2.0$' /clients/main/fury-Packages ||
        fail "the cache no longer serves /fury's 2.0 Packages.gz behind the captive portal"
    rm /run/acu-e2e/captive
    # The portal's pages the outage's misses cached as InRelease and
    # Release are deleted by the next anchorless check, which the next
    # client update triggers (spec C4 Dispatch 4: an anchor answered 404).
    poll "$REACH" "/fury's updates to be clean after the portal" fury_clean
    passed
}

# update_until_rejected BEFORE: a client update, and the sniff-rejection
# counter of plain.test has grown past BEFORE.
update_until_rejected() {
    apt_update main
    (($(metric 'acu_anchorless_sniff_rejected_total{host="plain.test"}') > $1))
}

# fury_clean: an update shows no error for /fury, and the cache answers
# /fury's InRelease 404 again (the portal's cached page is gone).
fury_clean() {
    apt_update main
    ! grep -E '^(E|Err|Error):.*plain\.test/fury' "$APT_LOG" > /dev/null &&
        [[ $(curl -s -o /dev/null -w '%{http_code}' -x "$PROXY" http://plain.test/fury/InRelease) == 404 ]]
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
[e2e-flat debian:12   20s] === scenario frozen-date: an InRelease-only, SHA512-only repository with a frozen Date publishes v2
[e2e-flat debian:12   21s] published nvidia v2
[e2e-flat debian:12   25s] carried by the client's check: 5s after the suites' last success (the scheduler: 30s)
[e2e-flat debian:12   25s] acu-nvidia 2.0 reached the client 4s after the publish
[e2e-flat debian:12   26s] --- scenario frozen-date passed (6s)
[e2e-flat debian:12   26s] === scenario anchorless: Release-less repositories: republished indexes, then a captive portal
[e2e-flat debian:12   26s] published goreleaser v2
[e2e-flat debian:12   31s] carried by the client's check: 6s after the suites' last success (the scheduler: 30s)
[e2e-flat debian:12   31s] acu-goreleaser 2.0 reached the client 5s after the publish (cooldown 2s)
[e2e-flat debian:12   31s] published fury v2
[e2e-flat debian:12   68s] the scheduler refreshed /fury 37s after the publish (periodic_refresh 30s)
[e2e-flat debian:12   69s] the upstream now answers an HTML page for every /fury path
[e2e-flat debian:12   72s] --- scenario anchorless passed (46s)
```
The scheduler's refresh comes up to `periodic_refresh` + one tick after the suite's last check (29–37 s after the publish in the planning runs); the 48 s bound is `PERIODIC + TICK + 10`.

- [ ] **Step 4: Mutation check**

Delete the `touch /run/acu-e2e/captive` line; rerun (the rejection counter must grow past its value before the portal, design review M-7).
Expected: `FAIL in scenario 'anchorless': timed out after 22s waiting for an anchorless check to reject the HTML page`. Restore it.

`fury_clean`'s `404` and `Error:` checks have no harness-side mutant: making them fail needs a daemon that keeps the portal's cached page, which the planning runs did not build.

- [ ] **Step 5: Lint and commit**

Run: `shellcheck -x -f gcc e2e/flat/*.sh`
Expected: exactly these findings — names a later task uses — and nothing else (as at Task 4; `restart` moved down):
```
e2e/flat/test.sh:106:5: note: Use start_daemon "$@" if function's $1 should mean script's $1. [SC2119]
e2e/flat/test.sh:131:5: warning: UNSIGNED_FIRST_CONTACT appears unused. Verify use (or export if used externally). [SC2034]
e2e/flat/test.sh:357:5: note: Use start_daemon "$@" if function's $1 should mean script's $1. [SC2119]
```

```bash
git add e2e/flat/test.sh
git commit -F - <<'EOF'
e2e/flat: the frozen Date and the Release-less repositories

A SHA512-only, InRelease-only repository whose Date never changes
publishes v2; a Release-less repository's republished index reaches a
client within one cooldown when clients ask (by the client's check) and
within one periodic_refresh when none does, and an upstream that then
answers HTML keeps the old index (the phase G carry-forward); after the
portal the cache answers the index's InRelease 404 again.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

### Task 6: Two suites flipping on one host, and mirror mode

Spec §7: "a mirror-mode run and a two-suites-one-host flip are included" (spec C8's shared client queue; design notes 1, 17). Both flips find their quiet window between the client's updates (`adoption_flip_barrier … outcome=held … held_requests=0` in the planning runs): the scenario proves a client sharing one connection across two suites that flip together sees no mismatch; a hold that holds requests is Task 7's. `mirror-mode` flips Sublime and the detached Jenkins suite: a client that got a `Release` and a `Release.gpg` from different snapshots would show a signature error, which only the signed-host signature grep catches (design review I-3). Both scenarios wait `REACH` and check `by_client`.

**Files:**
- Modify: `e2e/flat/test.sh`

**Interfaces:**
- Consumes: Task 3's names.
- Produces: the clients `pair` and `mirror` (`DIRECT`: no proxy, every source on `$MIRROR`); `cuda`, `dists`, `sublime` and `jenkins` published at v2.

- [ ] **Step 1: Add the scenarios after `fury_clean`**

```bash
# Two suites on one host flip at once while a client keeps updating both
# over its one connection to the host (spec C8).
two_suites() {
    scenario two-suites "two suites of one host flip together under a client's updates"
    client pair "$PROXY" "$SRC_CUDA" "$SRC_DISTS"
    local start
    start=$(date +%s)
    apt_update pair
    baseline "$start" signed.test "$CUDA" "$DISTS"
    publish cuda v2
    publish dists v2
    until_candidates "$REACH" pair -- acu-cuda=2.0 acu-epoch=1:2.0 acu-dists=2.0
    by_client
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
# all on one connection (spec C8, §3). The detached Jenkins suite flips here,
# under the normal barrier and the full grep with the signature words: a
# Release and a Release.gpg from different snapshots would show as a
# signature error, which no other word catches (design note 17).
mirror_mode() {
    scenario mirror-mode "signed flat repositories through a [[mirror]] prefix, with two flips"
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
    local start
    start=$(date +%s)
    apt_update mirror
    baseline "$start" signed.test "$SUBLIME" "$JENKINS"
    publish sublime v2
    publish jenkins v2
    until_candidates "$REACH" mirror -- acu-sublime=2.0 acu-jenkins=2.0 acu-jenkins-direct=2.0
    by_client
    say "the mirror-mode client saw acu-sublime and acu-jenkins 2.0 ${POLL_TOOK}s after the publish"
    log_has "$SCENARIO_MARK" "$(event adoption_flip_barrier)" "$(suite_kv "$JENKINS")" 'outcome=(held|quiet_cut_short)' ||
        fail "no held flip for $JENKINS: $(log_lines "$SCENARIO_MARK" "$(event adoption_flip_barrier)" "$(suite_kv "$JENKINS")")"
    apt_install mirror acu-sublime=2.0 acu-jenkins=2.0 acu-jenkins-direct=2.0
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
[e2e-flat debian:12   74s] === scenario two-suites: two suites of one host flip together under a client's updates
[e2e-flat debian:12   74s] published cuda v2
[e2e-flat debian:12   74s] published dists v2
[e2e-flat debian:12   81s] carried by the client's check: 7s after the suites' last success (the scheduler: 30s)
[e2e-flat debian:12   81s] both suites flipped; the client saw them 7s after the publish
[e2e-flat debian:12   82s] --- scenario two-suites passed (8s)
[e2e-flat debian:12   82s] === scenario mirror-mode: signed flat repositories through a [[mirror]] prefix, with two flips
[e2e-flat debian:12   83s] published sublime v2
[e2e-flat debian:12   83s] published jenkins v2
[e2e-flat debian:12   90s] carried by the client's check: 8s after the suites' last success (the scheduler: 30s)
[e2e-flat debian:12   90s] the mirror-mode client saw acu-sublime and acu-jenkins 2.0 7s after the publish
[e2e-flat debian:12   91s] --- scenario mirror-mode passed (9s)
```

- [ ] **Step 4: Lint and commit**

Run: `shellcheck -x -f gcc e2e/flat/*.sh`
Expected: exactly these findings — names a later task uses — and nothing else (as at Task 4; `restart` moved down):
```
e2e/flat/test.sh:106:5: note: Use start_daemon "$@" if function's $1 should mean script's $1. [SC2119]
e2e/flat/test.sh:131:5: warning: UNSIGNED_FIRST_CONTACT appears unused. Verify use (or export if used externally). [SC2034]
e2e/flat/test.sh:414:5: note: Use start_daemon "$@" if function's $1 should mean script's $1. [SC2119]
```

```bash
git add e2e/flat/test.sh
git commit -F - <<'EOF'
e2e/flat: two suites flipping on one host, and mirror mode

Two suites of one host publish together while a client keeps updating
both over its one connection; a mirror-mode client (every source on the
cache's address) installs and sees two flips, one of them the detached
Jenkins suite's, under the full grep with apt's signature errors; both
publishes are carried by the clients' checks.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

### Task 7: The 45 s commit

Spec §7: "A commit-delay hook of 45 s asserts no client timeouts, with its own scoped grep (the phase-B-expiry straddle is a documented residual)" (spec C8; design notes 12, 17). The daemon restarts with `ACU_E2E_COMMIT_DELAY=45s` (Task 1); a client updates every 3 s while the Jitsi suite flips (an inline suite nothing else flips; the detached Jenkins flip is Task 6's, under the full grep). The scope is subtractive (design review M-5): every update is grepped for every error but the residual's words (`apt_update --allow="$APT_RESIDUAL"`: a mismatch, an unexpected size, "Mirror sync in progress"), so a timeout, a signature error or a failed fetch fails it; mismatches are counted and reported. The flip must end `hold_expired`, and at least one held anchor request must have waited, none over 20 s. The daemon then restarts without the delay and a full-grep update must be clean.

**Files:**
- Modify: `e2e/flat/test.sh`

**Interfaces:**
- Consumes: Task 1's `e2e_commit_delay_enabled` line; Task 3's names (`APT_RESIDUAL`, `apt_update --allow=`); the request line's `flip_held_ms` and `adoption_flip_barrier`'s `outcome` (`docs/log-fields.md`).
- Produces: the client `delay`; `jitsi` published at v2.

- [ ] **Step 1: Add the scenario after `mirror_mode`**

```bash
# A commit slower than the flip barrier's 20 s hold (spec §7): no client
# times out. The straddle a phase-B expiry allows is a documented residual
# (spec C8), so this scenario's grep allows its words and nothing else
# (APT_RESIDUAL).
commit_delay() {
    scenario commit-delay "a 45 s commit: held anchor requests never reach apt's timeout"
    stop_daemon
    start_daemon ACU_E2E_COMMIT_DELAY=45s
    log_has "$SCENARIO_MARK" "$(event e2e_commit_delay_enabled)" "$(kv commit_delay 45s)" ||
        fail "the daemon did not enable the 45 s commit delay (built without -tags e2ehooks?)"
    client delay "$PROXY" "$SRC_JITSI" "$SRC_K8S" "$SRC_DISTS"
    apt_update delay
    publish jitsi v2
    local start=$SECONDS mismatches=0
    until log_has "$SCENARIO_MARK" "$(event adoption_success)" "$(suite_kv "$JITSI")"; do
        ((SECONDS - start < 150)) || fail "the delayed jitsi adoption did not commit within 150s"
        apt_update delay --allow="$APT_RESIDUAL"
        if grep -q 'Hash Sum mismatch' "$APT_LOG"; then
            mismatches=$((mismatches + 1))
        fi
        sleep 3
    done
    say "the delayed commit landed $((SECONDS - start))s after the publish; updates with the residual mismatch: $mismatches"
    log_has "$SCENARIO_MARK" "$(event adoption_flip_barrier)" "$(suite_kv "$JITSI")" "$(kv outcome hold_expired)" ||
        fail "the jitsi flip did not end hold_expired: $(log_lines "$SCENARIO_MARK" "$(event adoption_flip_barrier)")"
    local held max=0 ms
    held=$(log_lines "$SCENARIO_MARK" "$(event request)" "(^| )path=$(re "$JITSI")/(InRelease|Release)( |\$)" \
        'flip_held_ms=' | grep -oE 'flip_held_ms=[0-9]+' | cut -d= -f2 || true)
    [[ -n $held ]] || fail "no jitsi InRelease or Release request waited in the hold"
    for ms in $held; do ((ms > max)) && max=$ms; done
    ((max <= 20000)) || fail "a request waited ${max} ms in a hold, over 20 s"
    say "held requests: $(wc -w <<< "$held"), longest wait ${max} ms"
    stop_daemon
    start_daemon
    # The residual clears with the next update: no error of any kind.
    apt_update delay
    candidates_are delay acu-jitsi=2.0 || fail "the client sees acu-jitsi $(candidate delay acu-jitsi)"
    apt_install delay acu-jitsi=2.0
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
Expected, among the lines (the planning runs: the commit landed 49–52 s after the publish; one held request each time, 14.2–17.9 s; no mismatch):
```
[e2e-flat debian:12   85s] === scenario commit-delay: a 45 s commit: held anchor requests never reach apt's timeout
[e2e-flat debian:12   85s] stopping the daemon
[e2e-flat debian:12   86s] starting the daemon with ACU_E2E_COMMIT_DELAY=45s
[e2e-flat debian:12   86s] published jitsi v2
[e2e-flat debian:12  136s] the delayed commit landed 50s after the publish; updates with the residual mismatch: 0
[e2e-flat debian:12  136s] held requests: 1, longest wait 17857 ms
[e2e-flat debian:12  136s] stopping the daemon
[e2e-flat debian:12  137s] starting the daemon
[e2e-flat debian:12  138s] --- scenario commit-delay passed (53s)
```
The held request is one of Jitsi's anchors (`msg=request … path=/jitsi/stable/InRelease`, or `…/Release`, with `flip_held_ms=`), and the barrier line reads `adoption_flip_barrier … suite_path=/jitsi/stable … outcome=hold_expired`.

- [ ] **Step 4: Mutation check**

Replace `start_daemon ACU_E2E_COMMIT_DELAY=45s` with `start_daemon`; rerun.
Expected: `FAIL in scenario 'commit-delay': the daemon did not enable the 45 s commit delay (built without -tags e2ehooks?)`. Restore it.

- [ ] **Step 5: Lint and commit**

Run: `shellcheck -x -f gcc e2e/flat/*.sh`
Expected: exactly these findings — names a later task uses — and nothing else (`UNSIGNED_FIRST_CONTACT` until Task 8):
```
e2e/flat/test.sh:131:5: warning: UNSIGNED_FIRST_CONTACT appears unused. Verify use (or export if used externally). [SC2034]
```

```bash
git add e2e/flat/test.sh
git commit -F - <<'EOF'
e2e/flat: a 45 s commit times out no client

With the e2ehooks commit delay at 45 s, Jitsi's flip's hold expires at
20 s; no apt update prints any error but the phase-B-expiry straddle's
(a documented residual), the held requests waited under 20 s, and the
next update after the commit is clean.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

### Task 8: The unsigned first adoption and the whole-run assertions

Spec §7: "The unsigned first-adoption scenario (5-minute confirmation) runs on the oldest and newest images only", and "zero `unvouched_deb_refused` on the signed host" over the whole run (design notes 7, 8; deviation 2). The confirming observation is the record's first retry at least 5 minutes after the first one: the scheduler's probes in between see the same bytes before the retry time and stay `changed_deferred` (spec C7's gate, `checkFirst`'s `deferred` in `internal/freshness/first_adoption.go`), and the retries come 2, 4 and 8 minutes apart, each ±25 %. So the adoption lands 300–900 s after first contact (the planning runs' times are in Task 9), and the deadline is 900 s + `periodic_refresh` + one tick + 30 s = 968 s. Spec §7's "5-minute confirmation" is the floor, and the floor is asserted (design review I-1): `took >= 300` on the long-wait images, and on the short ones `whole_run` requires `/unsigned404` to be unadopted, which catches a broken floor whenever its first retry came before `whole_run`. The 403 variant is checked once its record's second retry has passed 5 minutes (design review M-4), and the unsigned host is shown unstrict by a served unvouched `.deb`.

**Files:**
- Modify: `e2e/flat/test.sh`

**Interfaces:**
- Consumes: `UNSIGNED_FIRST_CONTACT` (Task 3), `poll_quietly`, `suite`, `until_candidates`; the status JSON's `adoption_fail_reason`, `adoption_fail_count`, `adoption_fail_first_unixtime`, `adoption_next_retry_unixtime` and `last_check_unixtime`.
- Produces: `unsigned404` published at v2 on the long-wait images; `second_retry_past_5m PATH`, `record_of PATH`.

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
        say "=== scenario unsigned-first-adoption: skipped on this image (E2E_UNSIGNED_WAIT is not 1)"
        return 0
    fi
    scenario unsigned-first-adoption "an unsigned repository adopts after the 5-minute confirmation"
    local left=$((UNSIGNED_FIRST_CONTACT + UNSIGNED_DEADLINE - SECONDS)) took
    ((left > 0)) || left=1
    if ! poll_quietly "$left" adopted_as plain.test /unsigned404 unsigned; then
        fail "plain.test/unsigned404 did not adopt within ${UNSIGNED_DEADLINE}s of its first contact: $(record_of /unsigned404)"
    fi
    took=$((SECONDS - UNSIGNED_FIRST_CONTACT))
    # Spec C5 safeguard 2: never before 5 minutes. UNSIGNED_FIRST_CONTACT is
    # taken before the first update reaches the cache, so the first
    # observation is no earlier and this bound cannot fail a correct daemon.
    ((took >= 300)) || fail "plain.test/unsigned404 adopted ${took}s after its first contact, before the 5-minute confirmation"
    say "plain.test/unsigned404 adopted ${took}s after its first contact"
    # Updates of an established unsigned repository adopt at once.
    publish unsigned404 v2
    until_candidates "$REACH" main -- acu-unsigned404=2.0
    apt_install main acu-unsigned404=2.0
    # An unsigned snapshot never makes its host strict (spec C10): a .deb
    # no snapshot vouches is still served on plain.test.
    [[ $(curl -s -o /dev/null -w '%{http_code}' -x "$PROXY" http://plain.test/goreleaser/acu-goreleaser_2.0_amd64.deb) == 200 ]] ||
        fail "plain.test refused an unvouched .deb after an unsigned adoption"
    # The 403 variant needs an hour between its observations: once its
    # record's second retry has come 5 minutes or more after its first
    # observation — where a 5-minute gap would have confirmed it — it is
    # still unadopted.
    left=$((UNSIGNED_FIRST_CONTACT + UNSIGNED_DEADLINE - SECONDS))
    ((left > 0)) || left=1
    poll_quietly "$left" second_retry_past_5m /unsigned403 ||
        fail "plain.test/unsigned403's record never reached its second retry past 5 minutes: $(record_of /unsigned403)"
    [[ -z $(suite plain.test /unsigned403 current_snapshot_id) ]] ||
        fail "plain.test/unsigned403 adopted $((SECONDS - UNSIGNED_FIRST_CONTACT))s after its first contact, before its one-hour confirmation"
    [[ $(suite plain.test /unsigned403 adoption_fail_reason) == unsigned_unconfirmed ]] ||
        fail "plain.test/unsigned403 is not waiting for its confirmation: $(record_of /unsigned403)"
    passed
}

# second_retry_past_5m PATH: plain.test PATH's record has been written three
# times (the first observation and two retries) and its first observation
# is 5 minutes old — or PATH adopted, which clears the record (the caller
# then fails on the adoption).
second_retry_past_5m() {
    local count first
    [[ -z $(suite plain.test "$1" current_snapshot_id) ]] || return 0
    count=$(suite plain.test "$1" adoption_fail_count)
    first=$(suite plain.test "$1" adoption_fail_first_unixtime)
    [[ -n $count && -n $first ]] && ((count >= 3 && $(date +%s) - first >= 300))
}

# record_of PATH: plain.test PATH's failure record, from the status JSON.
record_of() {
    curl -fsS "$ADMIN/?format=json" | jq -c --arg p "$1" '.suites[] | select(.host == "plain.test" and .suite_path == $p) |
        {adoption_fail_reason, adoption_fail_count, adoption_fail_first_unixtime,
         adoption_next_retry_unixtime, last_check_unixtime, now: now | floor}'
}
```

- [ ] **Step 2: Add the whole-run assertions after `backoff_shown`**

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
    # The 5-minute floor where the unsigned scenario did not run: a run
    # this short must not have adopted plain.test/unsigned404 (it catches a
    # broken floor whenever that suite's first retry came before now).
    if [[ ${E2E_UNSIGNED_WAIT:-0} != 1 ]] && ((SECONDS - UNSIGNED_FIRST_CONTACT < 300)); then
        [[ -z $(suite plain.test /unsigned404 current_snapshot_id) ]] ||
            fail "plain.test/unsigned404 adopted $((SECONDS - UNSIGNED_FIRST_CONTACT))s after its first contact, before the 5-minute confirmation"
    fi
    local bad
    bad=$(log_lines 0 '(^| )msg=("[^"]*panic|[a-z_]*panicked( |$))|(^| )msg=package_hash_conflict( |$)')
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

The file now equals the planning run's `test.sh` plus revision 3's `backoff_shown` poll (Task 4); its run list reads `setup`, `first_contact`, `pr8_table`, `older_date`, `frozen_date`, `anchorless`, `two_suites`, `mirror_mode`, `commit_delay`, `unsigned_first_adoption`, `restart`, `upstream_down`, `whole_run`, `stop_daemon`, `say "PASS …"`.

- [ ] **Step 4: Run it on a short image and on a long-wait image**

Run: `E2E_FLAT_IMAGES=debian:12 make e2e-flat`
Expected: `=== scenario unsigned-first-adoption: skipped on this image (E2E_UNSIGNED_WAIT is not 1)`, `--- scenario whole-run passed (0s)`, `PASS in 1xxs` (133–143 s in the planning runs).

Run: `E2E_FLAT_IMAGES=debian:11 make e2e-flat`
Expected: `plain.test/unsigned404 adopted <300–900>s after its first contact`, `--- scenario unsigned-first-adoption passed`, `PASS in …s` (Task 9's table).

- [ ] **Step 5: Mutation checks**

1. In `whole_run`, replace the two lines that set `refused` for `signed.test` (`refused=$(log_lines 0 … | grep -Ev -- "$(kv path …acu-rogue…)" || true)`) with `refused=$(log_lines 0 "$(event unvouched_deb_refused)" "$(kv canonical_host signed.test)")`; rerun on `debian:12`.
   Expected: `FAIL in scenario 'whole-run': unvouched_deb_refused on signed.test: level=INFO msg=unvouched_deb_refused canonical_host=signed.test path=/core:/stable:/v1.34/deb/amd64/acu-rogue_1.0_amd64.deb current_snapshot_count=7`. Restore it.
2. A daemon mutant, in a scratch copy only (Global Constraints): in `internal/freshness/first_adoption.go`, `minConfirmGap = 5 * time.Minute` → `minConfirmGap = time.Minute`, and in `test.sh`'s run list delete `commit_delay` (otherwise the early adoption's commit can land in the 45 s delay window, and `commit-delay`'s `stop_daemon` times out waiting for it first); `E2E_FLAT_IMAGES=debian:11 make e2e-flat`.
   Expected: `FAIL in scenario 'unsigned-first-adoption': plain.test/unsigned404 adopted 112s after its first contact, before the 5-minute confirmation` (the seconds vary: the record's first retry, 90–150 s).
3. A daemon mutant, in a scratch copy only: in `internal/freshness/first_adoption.go`, `deniedConfirmGap = time.Hour` → `deniedConfirmGap = 5 * time.Minute`; `E2E_FLAT_IMAGES=debian:11 make e2e-flat`.
   Expected: `FAIL in scenario 'unsigned-first-adoption': plain.test/unsigned403 adopted 409s after its first contact, before its one-hour confirmation` (the seconds vary: the record's first retry 5 minutes or more after its first observation).

- [ ] **Step 6: Lint and commit**

Run: `shellcheck -x e2e/flat/*.sh`
Expected: no output.

```bash
git add e2e/flat/test.sh
git commit -F - <<'EOF'
e2e/flat: the unsigned first adoption and the whole-run assertions

On the oldest and newest images an unsigned repository adopts at its
first retry 5 minutes or more after first contact, never sooner, and then
updates at once; its host stays unstrict; its 403 variant still waits
after a retry where a 5-minute gap would have confirmed it. Over the
whole run no .deb was refused on the signed host but the deliberate one,
none on the unsigned host, and the daemon logged no panic or hash
conflict.

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

The planning runs. Revision 2 (2026-10-25: a fresh scratch tree at c8e6fe4 with this plan's files, `periodic_refresh` 30 s; one machine, Docker 29.8.2) and the draft (2026-10-24: 6786529 with the draft's files, `periodic_refresh` 20 s; a short image's test then took 119–135 s):

| Image | apt | Test, revision 2 | Unsigned adoption after first contact, revision 2 | Test, draft | Unsigned adoption, draft | `run.sh` build: revision 2 (stages cached) / draft (test stage cold) |
|---|---|---|---|---|---|---|
| `debian:11` | 2.2.4 | 384 s, 394 s | 373 s, 382 s | 320 s, 410 s, 397 s | 310 s, 397 s, 385 s | 22–43 s / 29 s |
| `ubuntu:22.04` | 2.4.14 | 137 s | — | 125 s, 127 s | — | 34 s / 51–58 s |
| `ubuntu:24.04` | 2.8.3 | 137 s | — | 123 s, 124 s | — | 33 s / 43 s |
| `debian:12` | 2.6.1 | 133 s, 134 s, 143 s | — | 119–135 s (four runs) | — | 18–22 s / 20 s (43 s with the Go stage cold) |
| `debian:13` | 3.0.3 | 139 s | — | 122 s, 123 s | — | 23 s / 30 s |
| `ubuntu:26.04` | 3.2.0 | 421 s, 417 s | 326 s, 356 s | 385 s | 373 s | 31–36 s / 33 s |

The final files ran once on every image; the other revision-2 times are earlier iterations of them (before the `INT` trap and `second_retry_past_5m`'s adoption check), with the same scenarios and bounds. A short image's test grew by about 15 s from the draft: the scheduler's `/fury` refresh waits a 30 s `periodic_refresh`, and `baseline` waits for each update's checks.

"Test" is the harness's own `PASS in` time; `run.sh`'s `test …s` adds the container's start and stop (2–5 s).

Per scenario (revision 2): `first-contact` 10–13 s, `pr8-table` 5–7 s, `older-date` 1–2 s, `frozen-date` 5–6 s, `anchorless` 39–48 s (mostly the scheduler's `/fury` refresh, 29–37 s after the publish against the 48 s bound), `two-suites` 5–8 s, `mirror-mode` 5–9 s, `commit-delay` 53–56 s (the commit landed 49–52 s after the publish; one held request each run, 14.2–17.9 s against 20 s), `restart` 2–4 s, `upstream-down` 1–2 s, `whole-run` 0 s; `unsigned-first-adoption` waits until the adoption (above), then until the 403 record's second retry has passed 5 minutes. The `REACH` waits took 2–7 s against 22 s. `by_client` saw every publish 4–8 s after the suites' last success, against the scheduler's 30 s. The final set (all six images, as two parallel batches) took 1,377 s of tests and 165 s of mostly cached builds: about 26 minutes sequentially. Worst case: the two long-wait images at their 968 s deadline, about 17 minutes each plus a cold build, and the four others about 2.5 minutes plus a cold build each — about 45 minutes sequentially.

- [ ] **Step 2: The Go and lint gates**

Run: `gofmt -l . && go vet ./... && go vet -tags e2ehooks ./cmd/apt-cacher-ultra/ && golangci-lint run ./... && golangci-lint run --build-tags e2ehooks ./cmd/... && go test -race -count=1 -timeout 30m ./... && go test -tags e2ehooks -count=1 ./cmd/apt-cacher-ultra/ && shellcheck -x e2e/flat/*.sh`
Expected: `gofmt -l` lists only the five drift files; `0 issues.` twice; every package `ok`; shellcheck prints nothing.

- [ ] **Step 3: Check the map — every item of spec §7 and §9's H row, and where it is asserted**

| Requirement | Where |
|---|---|
| `e2e/flat/`, `make e2e-flat`, the single-container pattern, in-container restart, `current -> v1\|v2` flips | Tasks 2–3 (`Dockerfile`, `run.sh`, `start_daemon`/`stop_daemon`, `publish`) |
| The six client images, `docker-*` apt.conf removed, images chosen by environment | `Dockerfile` (`rm -f /etc/apt/apt.conf.d/docker-*`), `run.sh` (`E2E_FLAT_IMAGES`); Step 1 |
| k8s (`amd64/…`, 403 for missing) | `first-contact` (vouched install; `README` answers 403) |
| CUDA (`./…`, an epoch `%3a` package) | `first-contact` (pins, vouched installs), `two-suites` (the 2.0 `%253a` pin) |
| libnvidia-container (InRelease only, SHA512-only, frozen `Date`) | `build-fixtures.sh`'s shape checks; `first-contact` (inline adoption, vouched), `frozen-date` |
| Jenkins (`sub/`, detached, `direct/`) | `first-contact` (detached adoption, both `.deb`s vouched), `mirror-mode` (the detached flip under the full grep) |
| Sublime (`files/`, host-root base) | `first-contact` (vouched), `mirror-mode` |
| Jitsi (by-hash SHA256+SHA512, binary `Release.gpg`, `Contents-amd64` via apt-file, `deb-src`) | `first-contact` (`by-hash/SHA512` pin, `apt-file search`, `apt-cache showsrc`), `commit-delay`; deviation 1 |
| Unsigned (404 and 403 variants) | `first-contact` (`unsigned_unconfirmed`, installs), `unsigned-first-adoption`, `whole-run` (the floor on short images); deviation 2 |
| Release-less (Gemfury shape) | `first-contact`, `anchorless` |
| A `dists/` repository | `first-contact` (and its wrong-key control), `two-suites` |
| Signed repositories on one host, unsigned and Release-less on a second | design note 1; `first-contact` (`acu-rogue` 502), `unsigned-first-adoption` (an unvouched `.deb` served on `plain.test`), `whole-run` |
| A mirror-mode run; a two-suites-one-host flip | `mirror-mode`; `two-suites` |
| Installs succeed | every `apt_install` |
| Request lines pinned (`./`, `%253a`, `by-hash/SHA512`) | `first-contact`, `pr8-table`, `two-suites`; `apt_sent_line`'s negative control |
| `apt-get` output grepped (exit status useless) | `apt_update` (with apt's signature errors on the signed host); `first-contact`'s refused-host and wrong-key controls |
| Zero `unvouched_deb_refused` on the signed host | `whole-run` (with `first-contact`'s lower bound) |
| A v2 publish of a signed repository reaches clients within one cooldown | `pr8-table`, `frozen-date`, `two-suites`, `mirror-mode` (`REACH`, and `by_client`; design note 5) |
| A v2 publish of an anchorless repository reaches clients within one cooldown | `anchorless` (`goreleaser`, `by_client`) |
| An older-`Date` publish is ignored | `older-date` (the refusal, unchanged snapshots, a fresh client) |
| State survives a restart | `restart` (`process.started_unixtime`; and the two restarts of `commit-delay`) |
| A 45 s commit delay, no client timeouts, a scoped grep | `commit-delay` (Task 1's seam; every error but the residual's) |
| The unsigned first adoption on the oldest and newest images only, after its 5-minute confirmation | `unsigned-first-adoption` (`E2E_FLAT_UNSIGNED_IMAGES`; the floor and the deadline; design note 7) |
| PR #8's end-to-end table | `pr8-table`, `upstream-down` (nginx's log unchanged) |
| The overview's H1 carry: `main.go`'s end of the `CheckBackoff` wiring (parked item 12) | `upstream-down` (`backoff_shown`: the status page's `backoff ends in`) |
| The *(G)* carry-forward (a Release-less repository: within one `periodic_refresh`; HTML keeps the old index) | `anchorless` (`fury`) |
| The CI change | Task 10 |
| The docs | Task 11 |
| CI's `deb-test` green for the RC (controller's addition) | Task 12 |

- [ ] **Step 4: Know what the planning runs fixed** (so a reviewer can tell a harness regression from a daemon one; none was a daemon bug, ruling R-H2-4)

1. apt's `Debug::Acquire::http` lines end in CRLF: `apt_sent_line`'s pattern allows the CR, and greps the file itself (design note 18).
2. Without a proxy (mirror mode) apt sends the origin form, `GET /m-signed/apt/stable/InRelease`, not an absolute URL.
3. apt asks a detached suite's `InRelease` first, and the hold covers it; `commit-delay` now flips the inline Jitsi suite and reads its `InRelease` and `Release` waits (design note 17).
4. An inline snapshot answers its suite's `Release` and `Release.gpg` 404 itself (spec §10 item 9): the 403 check asks `README`, a path no snapshot owns, and no check curls Jitsi's `Release.gpg`.
5. `grep -v` is a basic regex: `whole-run`'s exclusion of `acu-rogue` uses `grep -Ev` (the miss showed exactly the line Task 8's mutation check expects).
6. The sniff-rejection poll first waited for the scheduler: it runs an update per poll, and counts only rejections after the portal went up.
7. The unsigned deadline first assumed 5 minutes (design note 7).
8. The `sub/` and `a/b/` fixtures' `Release` files need a `Suite`, or apt warns "Conflicting distribution".
9. Jitsi's fixture first had no `InRelease` (deviation 1).
10. `debian:11`'s packages left `deb.debian.org` (design note 9), and its security suite stays: dropping it breaks the image build (`perl-base`); `nginx` became `nginx-light` (fewer mirror fetches, as e2e/deb uses).
11. v1 and v2 got distinct mtimes (design note 4).
12. `docker run --init` alone did not stop the test on SIGINT: bash as PID 1 needs the `INT`/`TERM` trap (design note 19).
13. The `minConfirmGap` mutant with `commit-delay` in the run fails in `commit-delay`'s `stop_daemon` (the early adoption's commit sat in the 45 s delay window, and shutdown waits for a commit), so Task 8's floor mutant drops `commit_delay` from the run list.
14. A `baseline` read before the update's own checks landed could take an older stamp; `baseline` waits for a stamp no older than the update's start less a cooldown (design note 5).

- [ ] **Step 5: No commit** (nothing changed). If a run fails, ruling R-H2-4 applies: a harness bug is fixed in the task that owns the file; a daemon bug gets its own task (a failing Go test first, the fix, the scenario that caught it), never a weakened assertion.

### Task 10: CI: one `e2e-flat` job per image; the seam's tests and guard

Ruling R-H2-3 and design note 13; design review M-10 and M-11, exec review m-7.

**Files:**
- Modify: `.github/workflows/ci.yaml`, `.github/workflows/release.yaml`

**Interfaces:**
- Consumes: `e2e/flat/run.sh`'s `E2E_FLAT_IMAGES` and `E2E_FLAT_UNSIGNED_IMAGES` (set but empty: no unsigned wait); Task 1's tagged tests (`TestBuildAdopter_E2ECommitDelay*`).
- Produces: the job `e2e-flat` (six matrix jobs, `fail-fast: false`); the `test` job's `e2ehooks seam` step and release-binary guard; `release.yaml`'s `Verify no e2e test seam` step.

- [ ] **Step 1: Run the seam's tests and guard in the `test` job**

In `.github/workflows/ci.yaml`'s `test` job:

old:
```yaml
      - run: go test -race -timeout 10m ./...
      - name: Verify portable Linux binary
        run: |
          CGO_ENABLED=1 make build
          program_headers="$(readelf --program-headers --wide build/apt-cacher-ultra)"
```
new:
```yaml
      - run: go test -race -timeout 10m ./...
      # e2e/flat's test binary seam (cmd/apt-cacher-ultra/e2ehooks.go),
      # compiled only with this tag.
      - name: e2ehooks seam
        run: |
          go vet -tags e2ehooks ./cmd/apt-cacher-ultra/
          go test -tags e2ehooks -count=1 -run 'E2ECommitDelay' ./cmd/apt-cacher-ultra/
      - name: Verify portable Linux binary
        run: |
          CGO_ENABLED=1 make build
          if grep -q ACU_E2E_ build/apt-cacher-ultra; then
            echo "Release binary contains the e2e test seam (ACU_E2E_)" >&2
            exit 1
          fi
          program_headers="$(readelf --program-headers --wide build/apt-cacher-ultra)"
```
(The untagged guard, `TestBuildAdopter_ReleaseBinaryIgnoresTheE2ECommitDelay`, already runs in `go test ./...`. `golangci-lint` with the tag stays a local gate, Task 9: CI's `lint` job runs `golangci-lint-action` untagged, and the `go vet -tags e2ehooks` above covers the tagged file in CI.)

- [ ] **Step 2: Add the `e2e-flat` job**

In `.github/workflows/ci.yaml`, before the comment `  # Fast unit tests for the apt-repo publishing scripts`, add:

```yaml
  # Spec §7 real-apt end-to-end test of flat repositories
  # (e2e/flat/README.md): nginx, the daemon and apt clients in one
  # container, one job per client image so every apt version (2.2 to 3.2)
  # runs on every change. The rows with `unsigned` (the oldest and newest
  # images) also wait for the unsigned confirmation, 5 to 15 minutes after
  # first contact; the others take about 2.5 minutes plus the image build.
  # No retry: a flaky scenario must show. A failure that names a
  # distribution mirror is re-run alone.
  e2e-flat:
    runs-on: ubuntu-latest
    strategy:
      fail-fast: false
      matrix:
        include:
          - { image: 'debian:11', unsigned: 'debian:11', timeout: 30 }
          - { image: 'ubuntu:22.04', unsigned: '', timeout: 15 }
          - { image: 'ubuntu:24.04', unsigned: '', timeout: 15 }
          - { image: 'debian:12', unsigned: '', timeout: 15 }
          - { image: 'debian:13', unsigned: '', timeout: 15 }
          - { image: 'ubuntu:26.04', unsigned: 'ubuntu:26.04', timeout: 30 }
    timeout-minutes: ${{ matrix.timeout }}
    steps:
      - uses: actions/checkout@v5
      - run: bash e2e/flat/run.sh
        env:
          E2E_FLAT_IMAGES: ${{ matrix.image }}
          E2E_FLAT_UNSIGNED_IMAGES: ${{ matrix.unsigned }}
```

- [ ] **Step 3: Guard the release build**

In `.github/workflows/release.yaml`, after the `Build binary` step (`run: make build VERSION="$VERSION"`) and before `- name: Install nfpm`, add:

```yaml

      # e2e/flat's test seam is compiled only with -tags e2ehooks: never
      # ship a binary that has it.
      - name: Verify no e2e test seam
        run: |
          if grep -q ACU_E2E_ build/apt-cacher-ultra; then
            echo "Release binary contains the e2e test seam (ACU_E2E_)" >&2
            exit 1
          fi
```

- [ ] **Step 4: Validate the workflows and the guard**

Run: `docker run --rm --network none -v "$PWD":/repo:ro -w /repo rhysd/actionlint:1.7.12 -no-color -oneline .github/workflows/ci.yaml && echo clean`
Expected: `clean` (the planning run; actionlint checks `timeout-minutes`' matrix expression and runs shellcheck on the `run:` lines). `rhysd/actionlint:1.7.12` is pinned; `go run …actionlint@…` needs the module proxy, which a sandboxed run may not reach.

Run: `docker run --rm --network none -v "$PWD":/repo:ro -w /repo rhysd/actionlint:1.7.12 -no-color -oneline .github/workflows/release.yaml`
Expected: only the finding that predates this plan, `.github/workflows/release.yaml:116:9: shellcheck reported issue in this script: SC2035:info:21:23: Use ./*glob* or -- *glob* so names with dashes won't become options [shellcheck]` (shellcheck SC2035 in the `Stage release artifacts` step, line 107 before Step 3's insert); none in the new step.

Run: `python3 -c "import yaml; j = yaml.safe_load(open('.github/workflows/ci.yaml'))['jobs']['e2e-flat']; print(j['timeout-minutes'], [(m['image'], m['unsigned'], m['timeout']) for m in j['strategy']['matrix']['include']])"`
Expected: `${{ matrix.timeout }} [('debian:11', 'debian:11', 30), ('ubuntu:22.04', '', 15), ('ubuntu:24.04', '', 15), ('debian:12', '', 15), ('debian:13', '', 15), ('ubuntu:26.04', 'ubuntu:26.04', 30)]`

Run: `go build -buildvcs=false -o /tmp/acu-release ./cmd/apt-cacher-ultra && go build -buildvcs=false -tags e2ehooks -o /tmp/acu-e2e ./cmd/apt-cacher-ultra && { grep -c ACU_E2E_ /tmp/acu-release; grep -c ACU_E2E_ /tmp/acu-e2e; }; rm -f /tmp/acu-release /tmp/acu-e2e`
Expected: `0` for the release build, a non-zero count for the tagged one (the guard can fail).

The jobs themselves run when the branch is next pushed; nothing here pushes. Expect each short job to take about 4–6 minutes on a GitHub runner (image pulls, a cold build, about 2.5 minutes of test) and each long-wait job 10–20. H3 compares the first CI run's job times with the timeouts (For H3).

- [ ] **Step 5: Commit**

```bash
git add .github/workflows/ci.yaml .github/workflows/release.yaml
git commit -F - <<'EOF'
ci: e2e-flat, one job per client image; guard the e2e test seam

Runs e2e/flat/run.sh for each of spec §7's six client images, so every
apt version from 2.2 to 3.2 is exercised on every change; the rows that
name an unsigned image (the oldest and newest) also wait for the
unsigned confirmation. The test job runs the e2ehooks seam's tests, and
the test job and the release workflow fail a binary that contains it.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

### Task 11: Documentation: `e2e/flat/README.md`, the Makefile help, the overview, and the list for H3

The brief's docs DoD: how to run it, image selection, what each scenario proves; the Makefile help; a "for H3" list (below, in this plan: H3 writes the README and the release notes). This task also marks the three overview items this phase closes (the H cross-phase line, the *(G)* Release-less item and the *(H1)* `CheckBackoff` carry). The times are the measured ones (exec review m-8): the unsigned adoption lands 5 to 15 minutes after first contact, which costs a long-wait image about 3 to 14 minutes more than a short one.

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
E2E_FLAT_IMAGES=debian:11 E2E_FLAT_UNSIGNED_IMAGES= bash e2e/flat/run.sh   # no unsigned wait
```

| Variable | Default | Meaning |
|---|---|---|
| `E2E_FLAT_IMAGES` | `debian:11 ubuntu:22.04 ubuntu:24.04 debian:12 debian:13 ubuntu:26.04` | client images to test, oldest apt first |
| `E2E_FLAT_UNSIGNED_IMAGES` | `debian:11 ubuntu:26.04` | images that also run the unsigned first adoption (it lands 5 to 15 minutes after first contact: about 3 to 14 minutes more); set but empty: none |
| `E2E_FLAT_NAME` | `acu-e2e-flat` | prefix of the image tags and container names |

It needs Docker with BuildKit and network access to Docker Hub
(`golang:1.25-bookworm`, `debian:bookworm-slim`, the client images), the
distribution mirrors the image build installs from and the Go module proxy.
Each image is built and run on its own; one failing does not stop the
others, and the exit status is non-zero if any failed. Ctrl-C (or
`docker stop`) ends a run at its next command.

Time: about 2.5 minutes per image plus its build (about a minute cold); the
two images that wait for the unsigned confirmation take about 3 to 14
minutes more (it lands 5 to 15 minutes after the first contact, which comes
in the run's first seconds, and the other scenarios overlap it: `apt` 2.2.4
on `debian:11` and 3.2.0 on `ubuntu:26.04` took 6.5 and 7 minutes in all).
CI runs one job per image (`.github/workflows/ci.yaml`, job `e2e-flat`).

## How it works

One container per client image (`Dockerfile`):

- `build` compiles the daemon with `-tags e2ehooks`, which adds one test
  seam: `ACU_E2E_COMMIT_DELAY` makes every adoption commit wait that long
  (`cmd/apt-cacher-ultra/e2ehooks.go`). Release builds never contain it;
  CI checks the release binary for the string.
- `fixtures` runs `build-fixtures.sh`: every repository in two versions
  (`/fixtures/<repo>/v1`, `v2`; packages `acu-<repo>` at `1.0` and `2.0`),
  signed with a throwaway key generated there (a second one, which signs
  nothing, is the wrong-key control's), and the web roots that link each
  repository's URL path to `/fixtures/<repo>/current -> v1`. A scenario
  publishes a version by flipping that symlink. The build fails if a
  fixture loses a shape a scenario relies on.
- `test` is the client image with nginx, curl, jq and apt-file; its
  `docker-*` apt.conf files are removed, so apt behaves as on a host.

`test.sh` (with `lib.sh`) starts nginx (`nginx.conf`) as the upstream for
two host names — `signed.test` for the signed repositories, `plain.test`
for the unsigned and Release-less ones — and the daemon with short timers
(`cooldown` 2 s, `periodic_refresh` 30 s, `flip_quiet_period` 1 s, strict
mode on). Each apt "client" is a state directory under `/clients`,
selected with `APT_CONFIG`. Assertions read apt's output (`apt-get
update`'s exit status says nothing, so it is grepped for `Failed to
fetch|Connection failed|timed out|Hash Sum mismatch|Mirror sync in
progress`, and for apt's signature errors on lines naming the signed host)
and its `Debug::Acquire::http` request lines, nginx's access log, the
daemon's log events, the admin status JSON and `/metrics`. Every check
that could pass without its behaviour has a control that shows it can
fail. Every wait is a bounded poll; a failure, or any unexpected exit,
prints the scenario, the daemon's log tail and nginx's.

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

They run in this order and share one cache. "By the client" means the
client saw the publish before the scheduler could have checked the suite
again (`periodic_refresh` after its last success), so the client's own
check carried it.

| Scenario | Proves |
|---|---|
| `first-contact` | One client with every repository: each signed suite adopts in its upstream's form (inline, or detached for Jenkins), the Release-less ones refresh without a `Release`, the unsigned ones wait for confirmation. Installs of every Filename shape succeed and are vouched (`validated_hash=true`). apt's `./`, `%253a` and `by-hash/SHA512` request lines are pinned, apt's and the cache's upstream ones. apt-file finds Jitsi's `Contents-amd64`; `deb-src` reads its `Sources`. Strict mode refuses `acu-rogue` (502), so "no refusal" elsewhere means something; the update check must fail a host the cache refuses and a signed source checked against the wrong key, and the request-line check must miss a line apt never sent, so those checks mean something on this apt. |
| `pr8-table` | PR #8's end-to-end table: one signed flat repository as `deb <uri>/ /` on one client and `deb <uri> ./` (its own suite, the twin) on another; after the v2 publish both report 2.0 within one cooldown (bounded at cooldown + 20 s; 2–7 s measured), by the client. |
| `older-date` | A publish with an older `Date` is refused (`upstream_older`) for both twins; the snapshots stay, and a fresh client gets 2.0 (apt alone would keep the old clients at 2.0). |
| `frozen-date` | libnvidia-container's v2, with the same frozen `Date`, reaches the client by its check and installs vouched. |
| `anchorless` | A Release-less repository's republished index reaches a client within one cooldown by its check (`goreleaser`) and within one `periodic_refresh` when no client asks (`fury`, the scheduler alone); behind a captive portal (every path an HTML page with 200) the old index stays — the cache's rejection counter grows and it still serves the 2.0 `Packages.gz` — and after the portal the pages its misses cached are deleted by the next check (the cache answers `InRelease` 404 again). |
| `two-suites` | Two suites of one host publish together while a client updates both over one connection: no update shows a mismatch, both flips are held, and the client's checks carried them. |
| `mirror-mode` | A client with every source behind the cache's `[[mirror]]` prefix (one apt queue for all) installs, and sees two flips — Sublime and the detached Jenkins — by its check, cleanly, signature errors included. |
| `commit-delay` | With a 45 s commit (`ACU_E2E_COMMIT_DELAY`), Jitsi's flip's hold expires at 20 s: anchor requests waited under 20 s and no update printed any error but the documented phase-B-expiry residual's (a mismatch: this scenario's grep allows those words and nothing else); the first update after the commit is clean. |
| `unsigned-first-adoption` | (`E2E_FLAT_UNSIGNED_IMAGES` only) The unsigned repository adopts no sooner than 5 minutes after first contact, at its record's first retry after that (5 to 15 minutes), then updates at once; its host stays unstrict; the 403 variant is still waiting after its record's second retry has passed 5 minutes (its gap is an hour). |
| `restart` | The daemon really restarted (`process.started_unixtime`), and kept the same snapshots and candidates; a clean update; anchors served from the snapshots. |
| `upstream-down` | PR #8's table, second half: with nginx stopped — its access log does not grow — fresh clients of both source forms install 2.0 from the snapshots. The checks their updates trigger fail, and the status page's row for the k8s suite shows `checks failing ×k · backoff ends in …`, the text `main.go`'s `CheckBackoff` wiring adds. |
| `whole-run` | No `unvouched_deb_refused` on `signed.test` but `acu-rogue`, none on `plain.test`; on a short image, `unsigned404` still unadopted (the 5-minute floor, when its first retry came first); no panic and no `package_hash_conflict` in the daemon's log. |

Two shapes of spec §7 are exercised only in part: apt (2.2 to 3.2) refuses
a binary detached `Release.gpg` ("unsupported binary format"), so Jitsi's
binary signature sits beside an `InRelease` that apt uses; and the 403
unsigned variant's first adoption needs an hour, past any bounded run. Go
tests cover both (`TestSniffReleaseGPG`,
`TestCheckFirst_UnsignedAfterADeniedSignatureWaitsAnHour`).

## Debugging a failure

The failure message names the scenario and prints the last lines of the
daemon's log and of nginx's access log. To look around inside, keep the
container (run `test.sh` once per container: it flips the fixtures and
keeps the cache, so start a new container to run it again):

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
  its `sources.list` at `archive.debian.org` for `bullseye` (main and
  security). Do the same for the next one (`old-releases.ubuntu.com` for
  Ubuntu).
- **A transient mirror failure** fails one image's build (each `apt-get`
  already retries three times); re-run that image.
- **Fixtures are dated relative to the build** (v1 two days before, v2 one
  day before, no `Valid-Until`), so a cached image stays valid.
- **Adding a scenario**: a function in `test.sh` that starts with
  `scenario NAME DESCRIPTION`, ends with `passed`, waits only through
  `poll` or `until_candidates`, and is added to the run list at the end of
  the file. Assert on snake_case log events, the status JSON or
  `/metrics`, not on prose log messages; never end a pipe with `grep -q`
  (`pipefail`); give a check that could pass without its behaviour a
  control.
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
# ubuntu:26.04, about 2.5 minutes each plus the build; debian:11 and
# ubuntu:26.04 also wait for the unsigned confirmation, which lands 5 to 15
# minutes after first contact (about 3 to 14 minutes more each).
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
a republished index reaches a client within one `periodic_refresh`, and an upstream that then answers HTML keeps the old index. *Done in H2 (Task 5: the `anchorless` scenario).*
```

old:
```
Breaking it loses only the text. *H2's.*
```
new:
```
Breaking it loses only the text. *Done in H2 (Task 4: the `upstream-down` scenario's `backoff_shown`).*
```

- [ ] **Step 4: Verify**

Run: `grep -c 'e2e-flat' Makefile && grep -n 'Done in H2' docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md && git diff --check`
Expected: `2` (the `.PHONY` line and the target); two overview lines (the `CheckBackoff` carry and the *(G)* item); `git diff --check` prints nothing.

Run: `go test -count=1 -run 'TestEveryEventIsDocumented|TestLogMessagesAreSnakeCase' ./internal/observability/`
Expected: `ok` (H1's guards: Task 1's `e2e_commit_delay_enabled` row still stands; this task adds no event).

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

What this phase leaves for H3 to write or check, each with its source:

1. **`README.md`, testing:** `make e2e-flat` runs real apt (2.2 to 3.2, six images) against fixtures of every supported repository layout through the cache, about 25 minutes sequentially with cached builds (Task 9 measured 24.5; about 45 at worst, cold, with both long-wait images at a third retry), one CI job per image; `e2e/flat/README.md` says how to run it and what each scenario proves. A binary built with `-tags e2ehooks` is a test binary and must never ship (`cmd/apt-cacher-ultra/e2ehooks.go`; CI and the release workflow check the shipped binary).
2. **The unsigned confirmation's real duration** (`docs/configuration.md`, *Anchor forms and unsigned repositories*, safeguard 2; the README's unsigned paragraph): "at least 5 minutes; with the retry schedule, usually 5 to 15" — a never-adopted unsigned repository adopts at the first retry of its `unsigned_unconfirmed` record at least 5 minutes after the first observation (ruling R-H2-6, design note 7).
3. **After a captive portal** (`docs/configuration.md`, *Flat repositories*, the limits of the refresh without a `Release`): the first `apt update` through the cache after the outage can still show the portal's page for a Release-less repository's `InRelease` or `Release`, which a client's miss cached during it; the check that update triggers deletes them, so the next update is clean (design note 11). The outage's misses also cache the portal's page as the repository's `Release.gpg`, and that refresh does not delete it: it deletes only the `InRelease` and `Release` rows (`internal/freshness/anchorless.go`, the anchors' delete loop; Task 5's observation). H3's text names it beside them. It is harmless while `Release` answers 404; a repository that later publishes a signed `Release` could show clients a transient signature error until it adopts. A post-1.1.0 follow-up candidate, for the maintainer to decide: the refresh also deletes a `Release.gpg` row that fails its sniff.
4. **A binary detached `Release.gpg`** (optional, *Flat repositories*): the cache adopts a repository that publishes only `Release` and a binary `Release.gpg`, but apt 2.2–3.2 refuses it ("unsupported binary format"), so such a repository is unusable through any path; Jitsi publishes an `InRelease` beside it (deviation 1).
5. **Spec §7**, in H3's revision (this phase leaves the spec alone: H1 took revision 12, and H3's revision records these with its own changes): the two deviations above, design note 5's reading of "within one cooldown" (by the client's check, before the scheduler's), and design note 7's timing of the unsigned confirmation. Two of these changed in execution (the reviews of Tasks 7 and 8); record them as built:
   - **Deviation 2:** the 403 check waits for a refusal of the `/unsigned403` record written 305 s or more after its first observation (`retry_past_5m`), then requires the suite still unadopted. That refusal is usually the second retry; a second retry before 305 s (about 6–8 % of runs) makes the check wait for the third, so it is never vacuous. The deviation's "a second retry before 300 s, about 6 % of runs, leaves that run's check vacuous" no longer holds.
   - **A third deviation, stricter than §7:** `commit-delay` runs every update under the full grep, not §7's scoped one. Its client cannot reach spec C8's phase-B straddle: it holds v1's lists, so it gets 304s and requests no index, and Jitsi's by-hash requests are served from the displaced snapshot. apt (2.6.1) resends a timed-out request silently, so a timeout is caught by each update's length (under 28 s) and by the daemon's request lines (no `client_canceled`, no `duration_ms` of 30000 or more); the longest `flip_held_ms` must be at most 20 500. The residual allowance this plan describes (`APT_RESIDUAL`, `apt_update --allow`: revision 2's design M-5 row, Task 3's Produces list, Task 9's map row, Task 11's README row) was removed in ce0059c.
6. **Release notes, 1.1.0:** the e2e harness is new (fixtures only); the live-upstream smoke (overview, "PR #8 parity") stays the manual step before the release.
7. **The overview's phase table:** H2's commit once it lands.
8. **The first CI run's job times** against Task 10's timeouts (15 and 30 minutes): if a short job takes over 10 minutes or a long-wait job over 25, raise its timeout with the measured reason (design review, "underspecified"; this plan measured one machine only). Expect short jobs to take about 5–8 minutes (135–144 s of test locally, plus image pulls and a cold build). Expect long-wait jobs to take about 8–12 minutes usually (363–407 s of test plus a 2–5 minute cold build), and about 18–21 when a record waits for its third retry (one local test took 855 s), above Task 10's "10–20" but inside 30 (Task 10's timeout analysis). The third-retry case is about 6–8 % of runs for each of the two unsigned records. That makes about 13 % of long-wait jobs (roughly one in seven or eight) and, with two long-wait jobs per CI run, roughly one run in four. These are derived from the retry schedule (2, then 4 minutes, each ±25 %), not measured, so a long-wait job near 20 minutes at about that rate is the schedule, not an outlier.
9. **`docs/log-fields.md`'s `flip_held_ms` row** says "at most 20000". That is nominal: the hold's timer fires a few milliseconds late, so a held request can log a little over 20000 (`commit-delay` allows 20 500; Task 9 measured at most 17 914). H3 decides whether the row should say so.
10. **This plan's body is the plan as approved.** The per-task reviews changed the harness after it: the signature filter (`//signed\.test[/ ]|/m-signed[/ ]` and a second wrong-key control, beside design note 17), curl time limits, the failure output (both log tails and the failing command, beside design note 19), `commit-delay`'s checks, the 403 check, `whole-run`'s checks and `run.sh`'s interrupt handling. `e2e/flat/README.md` describes the harness as built; where it and this plan differ, the README is right.

## Found outside this plan's scope (for the controller)

e2e/deb's phase 4 failure on this branch (CI's `deb-test`), found while planning, is Task 12 by the controller's ruling: a harness fix in `e2e/deb/test.sh`, with phases 2 and 3 tightened for the same cause. `release.yaml`'s `Stage release artifacts` step has a shellcheck finding that predates this plan (`.github/workflows/release.yaml:116:9: shellcheck reported issue in this script: SC2035:info:21:23: Use ./*glob* or -- *glob* so names with dashes won't become options [shellcheck]`); left as it is. Nothing else outside `e2e/flat` was found.

## Self-review against spec §7 (done while writing)

Every clause of §7's "Real-apt end-to-end" paragraph and of §9's H row maps to a scenario in Task 9's table or to deviation 1 or 2, and the overview's one H1 carry for H2 (`CheckBackoff`) to `upstream-down`. Revision 3 ran no image: its `backoff_shown` poll and Task 4's mutation 5 are the implementer's first run (the revision note lists what was replayed). Revision 2's files ran green on all six images (the final files once each; earlier iterations also on `debian:12`, `debian:11` and `ubuntu:26.04`) in a fresh scratch copy at c8e6fe4; the draft's files ran green on all six (and in the exec review). The staged `test.sh` of Tasks 3–7 each ran green on `debian:12`, their `shellcheck` findings are the ones each task's lint step lists, and applying Tasks 4–8 as worded to Task 3's file gives the final file byte for byte. The mutation checks of Tasks 2, 3, 4, 5, 7 and 8 were replayed in revision 2 and failed as stated (Task 8's daemon mutants on `debian:11`; the earliest of them on an iteration that lacked only the `INT` trap and `second_retry_past_5m`'s adoption check, which the code they mutate does not touch); Task 1's red step and mutation check and Task 12's were replayed in the draft round, and Task 1's file changed only in its comment (its Go checks re-ran clean). Task 2's smoke command ran as written. Task 12's patch applies to 6786529 (`patch --dry-run`), its failing run (Step 1) and its green runs on `ubuntu:24.04` and `ubuntu:26.04` (Step 3) were replayed, and its `test.sh` is shellcheck-clean.
