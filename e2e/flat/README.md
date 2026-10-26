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
| `E2E_FLAT_IMAGES` | `debian:11 ubuntu:22.04 ubuntu:24.04 debian:12 debian:13 ubuntu:26.04` | the client images to test, in this order; each must be `debian:N` or `ubuntu:NN.NN` |
| `E2E_FLAT_UNSIGNED_IMAGES` | `debian:11 ubuntu:26.04` | the images that also run `unsigned-first-adoption`, which waits for the unsigned confirmation (5 to 15 minutes after first contact); set but empty: none |
| `E2E_FLAT_NAME` | `acu-e2e-flat` | the image name and container name prefix: for `debian:12`, image `<name>:debian-12` and container `<name>-debian-12` |

The images and their apt, as of October 2026:

| Image | apt | Unsigned wait by default |
|---|---|---|
| `debian:11` | 2.2.4 | yes |
| `ubuntu:22.04` | 2.4.14 | |
| `ubuntu:24.04` | 2.8.3 | |
| `debian:12` | 2.6.1 | |
| `debian:13` | 3.0.3 | |
| `ubuntu:26.04` | 3.2.0 | yes |

It needs Docker on an amd64 host, and network access to:

- Docker Hub (`golang:1.25-bookworm`, `debian:bookworm-slim`, the client
  images);
- the distribution mirrors the image builds install from
  (`deb.debian.org`, `archive.debian.org`, `archive.ubuntu.com`,
  `security.ubuntu.com`);
- the Go module proxy (the build stage's `go mod download`).

The fixture packages are built for amd64 (two are `all`). On an arm64
host, such as Apple Silicon or an arm64 runner, apt finds no candidate for
most of them and the run fails.

`run.sh` does not pull: it uses a client image already on the machine as
it is. To test what CI tests (CI starts with no images), `docker pull` the
client images first.

Each image is built and run on its own. One failing does not stop the
others, and the exit status is 1 if any failed. An unsupported image name
or `E2E_FLAT_NAME` exits 2 before anything is built.

Ctrl-C stops the whole run: the running test ends at its next command,
printing its logs, and `run.sh` exits 130 instead of going on to the next
image (143 for a TERM). `docker stop` on one test container ends that
image's test the same way; it counts as failed, and the run goes on.

Two runs on one machine with the same `E2E_FLAT_NAME` share image tags and
container names, and each run removes a container of its name first. Give
each checkout its own name.

**Time** (measured on one machine, 12 cores):

- **A short image:** its test takes 135–144 s, plus its build (seconds
  with cached layers, about a minute cold).
- **The two images that wait for the unsigned confirmation:** 407 s
  (`debian:11`) and 363 s (`ubuntu:26.04`).
  - The confirmation lands 5 to 15 minutes after first contact, which
    comes in the run's first seconds, and the other scenarios overlap it.
  - Either unsigned record's second retry can come before 5 minutes,
    about 6–8% of runs for each. The scenario then waits for that record's
    third retry, and the image takes about 14 minutes (one such run took
    855 s).
- **All six in sequence:** 24.5 minutes.

CI runs one job per image (`.github/workflows/ci.yaml`, job `e2e-flat`).
The timeout is 15 minutes for the short images and 30 for the two that
wait. A long-wait job can take about 18–21 minutes in the third-retry case.

## How it works

One container per client image (`Dockerfile`), built in three stages:

- **`build`** compiles the daemon with `-tags e2ehooks`, which adds one
  test seam (`cmd/apt-cacher-ultra/e2ehooks.go`).
  - `ACU_E2E_COMMIT_DELAY` (a Go duration) makes every adoption commit
    wait that long first. The daemon logs WARN `e2e_commit_delay_enabled`.
  - Release builds compile its no-op twin (`e2ehooks_off.go`).
  - CI's `test` job and the release workflow fail a binary that contains
    `ACU_E2E_` or was built with the tag.
- **`fixtures`** runs `build-fixtures.sh`.
  - Every repository comes in two versions: `/fixtures/<repo>/v1` and
    `v2`, with packages `acu-<repo>` at `1.0` and `2.0`.
  - They are signed with a throwaway key generated there; no key is
    committed. A second key, which signs nothing, is the wrong-key
    controls'.
  - The web roots link each repository's URL path to
    `/fixtures/<repo>/current -> v1`. A scenario publishes a version by
    flipping that symlink with one rename (`publish`).
  - The build fails if libnvidia-container's fixture loses a shape a
    scenario relies on: its `InRelease`, the same frozen `Date` in both
    versions, or a `Release` with a SHA512 block and no other.
- **`test`** is the client image with nginx, curl, jq and apt-file. Its
  `docker-*` apt.conf files are removed, so apt behaves as on a host. Its
  command is `test.sh`, which sources `lib.sh`.

`test.sh` starts nginx (`nginx.conf`) as the upstream for two host names,
both `127.0.0.1` in `/etc/hosts`:

- `signed.test` for the signed repositories;
- `plain.test` for the unsigned and Release-less ones.

It then starts the daemon with short timers, strict mode and a mirror:

- `cooldown` 2 s, `periodic_refresh` 30 s (a 7.5 s scheduler tick) and
  `flip_quiet_period` 1 s;
- strict mode on (`refuse_unvouched_debs`), with the fixtures' key
  trusted for `signed.test`;
- a `[[mirror]]` prefix, `/m-signed`, for `http://signed.test/`.

Each apt "client" is a state directory under `/clients`, selected with
`APT_CONFIG`. dpkg's database is shared, so every install check verifies
the installed file's version and then purges the packages.

Assertions read:

- **apt's output.** `apt-get update`'s exit status says nothing, so
  `apt_update` and `apt_install` grep it for two kinds of error.
  - `Failed to fetch|Connection failed|timed out|Hash Sum mismatch|Mirror sync in progress`.
  - apt's signature errors, such as `GPG error` (apt 2.x),
    `OpenPGP signature verification failed` (apt 3.x) and
    `is not signed`. They count only on lines naming the signed host or
    the mirror prefix (`//signed\.test[/ ]|/m-signed[/ ]`). The space in
    that pattern is for a host-root source, which apt names with no slash
    after the host: `http://signed.test apt/stable/`.
  - The captive portal's aftermath prints the same signature words for
    `plain.test`, so they do not count there.
- **apt's `Debug::Acquire::http` request lines**, and nginx's access log:
  the request lines the cache sent upstream.
- **The daemon's own record:** its snake_case log events, the admin
  status JSON (`/?format=json`) and `/metrics`. One status-page text is
  read too: a failing suite's "backoff ends in".

The checks that could pass vacuously have controls on every image:

- `apt_update` must fail an update from a host the cache refuses
  (`nowhere.test`).
- It must also fail two signed sources checked against the wrong key: the
  `dists/` one, and the host-root one.
- The request-line check must not match a line apt never sent.
- `poll_quietly` must time out on `false`.
- Strict mode must refuse `acu-rogue` (502), so "no refusal" elsewhere
  means something.

Every wait is bounded:

- `poll` and `poll_quietly` try once a second;
- `until_candidates` runs one update per cooldown;
- `commit-delay`'s loop stops at 150 s.

Every curl has a time limit: 10 s, or 35 s for an anchor through the
proxy, which a flip can hold for 20 s. A failure prints the scenario, the
last 80 lines of the daemon's log and the last 30 of nginx's access log.
An unexpected exit, such as a `set -e` exit, also prints the failing
command.

## The fixtures

| Path | Reproduces | Shape |
|---|---|---|
| `signed.test/core:/stable:/v1.34/deb` | pkgs.k8s.io | `deb <uri>/ /` (and `deb <uri> ./`, the twin), Filenames `amd64/…`, `InRelease` only, 403 for a missing file; plus `acu-rogue`, on the upstream but in no `Packages` |
| `signed.test/cuda/ubuntu2204/x86_64` | NVIDIA CUDA | `deb <uri>/ /`, Filenames `./…`, `InRelease`, `Release` and an armored `Release.gpg`; an epoch package in a file named `…_1%3a<v>_amd64.deb` |
| `signed.test/libnvidia-container/stable/deb/amd64` | libnvidia-container | `deb <uri> /`, Filenames `./…`, `InRelease` only, a SHA512-only `Release`, `Date` frozen at 2018 |
| `signed.test/debian-stable` | pkg.jenkins.io | `deb <uri> binary/`, detached (an armored `Release.gpg`), Filenames `binary/…` and `direct/…` |
| `signed.test/apt/stable` + `signed.test/files` | download.sublimetext.com | `deb <host-root> apt/stable/`, Filenames `files/…` at the host root |
| `signed.test/jitsi` | download.jitsi.org | `deb <uri> stable/` and `deb-src`, `Acquire-By-Hash`, SHA256 and SHA512, `Contents-amd64`, a binary `Release.gpg` beside `InRelease` |
| `signed.test/debian` | a `dists/` archive | `deb <uri> acu main`, inline |
| `plain.test/unsigned404`, `plain.test/unsigned403` | an unsigned repository | `deb [trusted=yes] <uri>/ /`, `Release` only; missing files 404, or 403 |
| `plain.test/fury`, `plain.test/goreleaser` | apt.fury.io, repo.goreleaser.com | `deb [trusted=yes] <uri>/ /`, `Packages` only, no `Release`; Filenames without `./` (fury) and with it (goreleaser) |

## The scenarios

They run in this order and share one cache. "By the client" means the
client saw the publish before the scheduler could have checked the suite
again (`periodic_refresh` after its last success), so the client's own
check carried it. "Within one cooldown" is bounded at cooldown + 20 s
(22 s); the measured waits were 4–8 s.

| Scenario | Proves |
|---|---|
| `first-contact` | One client with every repository. Each signed suite adopts in its upstream's form (inline, or detached for Jenkins); the Release-less ones refresh without a `Release`; the unsigned ones wait for confirmation (`unsigned_unconfirmed`). Installs of every Filename shape succeed, and every signed `.deb` is vouched (`validated_hash=true`). apt's `./`, `%253a` and `by-hash/SHA512` request lines are pinned, and the cache's `./` and `%253a` lines upstream too. apt never fetches Jitsi's binary `Release.gpg`. apt-file finds Jitsi's `Contents-amd64`, and `apt-cache showsrc` reads its `Sources`. Strict mode refuses `acu-rogue` (502, with its `unvouched_deb_refused` line). The k8s shape's 403 for a missing file passes through the cache. The update and request-line controls (above) run here. |
| `pr8-table` | PR #8's end-to-end table. One signed flat repository as `deb <uri>/ /` on one client and `deb <uri> ./` (its own suite, the twin) on another; apt's `./InRelease` and `./Packages.gz` lines are pinned. After the v2 publish both report 2.0 within one cooldown, by the client, and install it. |
| `older-date` | A publish with an older `Date` is refused (`upstream_older`) for both twins. The snapshots stay, and a fresh client gets 2.0 (apt alone would keep the old clients at 2.0). |
| `frozen-date` | libnvidia-container's v2, with the same frozen `Date`, reaches the client within one cooldown, by the client, and installs vouched. |
| `anchorless` | A Release-less repository's republished index reaches a client within one cooldown, by the client (`goreleaser`). With no client asking (`fury`), the scheduler alone refreshes it within `periodic_refresh` + a tick + 10 s of the publish (48 s; measured 33–35 s). Then a captive portal: every `/fury` path answers an HTML page with 200. Behind it the client keeps 2.0, the cache's sniff-rejection counter for `plain.test` grows, and the cache still serves the 2.0 `Packages.gz`. After the portal, the pages its misses cached as `InRelease` and `Release` are deleted by the next check: an update shows no `/fury` error, and the cache answers `InRelease` 404 again. |
| `two-suites` | Two suites of one host (CUDA, `dists/`) publish together while a client updates both. Every update is clean, both flips run the barrier (`held` or `quiet_cut_short`), and the client's checks carried them. The epoch's `%253a` 2.0 request line is pinned. |
| `mirror-mode` | A client with four sources behind the cache's `[[mirror]]` prefix, which apt queues on one connection. apt sends the origin-form request line. Sublime's `.deb` is vouched. Two flips, Sublime and the detached Jenkins, reach it by its check, each through the barrier, and every update is clean, signature errors included. |
| `commit-delay` | The daemon restarts with `ACU_E2E_COMMIT_DELAY=45s` (its `e2e_commit_delay_enabled` line is required). Jitsi's v2 publish then commits 45 s late while a client updates in a loop, 3 s apart, and the flip ends `hold_expired`. Every update runs under the full grep, stricter than spec §7's scoped one: this client holds v1's lists, so it cannot reach the phase-B straddle, and a mismatch here would be a bug. apt resends a timed-out request silently (2.6.1), so two other checks catch a timeout: each update takes under 28 s, and no `signed.test` request line is `client_canceled` or lasts 30 s or more. At least one Jitsi `InRelease`/`Release` request waited in the hold, and none longer than 20 s (20 500 ms, for the timer's latency). The daemon then restarts without the delay (asserted), and the client gets and installs 2.0. |
| `unsigned-first-adoption` | (`E2E_FLAT_UNSIGNED_IMAGES` only) `/unsigned404` adopts no sooner than 5 minutes after first contact (asserted) and within 968 s. It lands at its record's first retry 5 minutes or more after the first observation, 5 to 15 minutes in practice. Its v2 then reaches the client within one cooldown. An unvouched `.deb` on `plain.test` still answers 200: an unsigned snapshot never makes its host strict. `/unsigned403` needs an hour between its observations. Once its record has logged a refusal written 305 s or more after its first observation, where a 5-minute gap would have confirmed it, it must still be unadopted and waiting `unsigned_unconfirmed`. That refusal is usually the second retry. When the second comes before 305 s (about 6–8% of runs), the check waits for the third, so it is never vacuous. |
| `restart` | The daemon really restarted: `process.started_unixtime` is no earlier than the stop. It kept the same snapshots and the main client's candidates, an update is clean, and the k8s `InRelease` is served from its snapshot (`X-Cache-Snapshot`). |
| `upstream-down` | PR #8's table, second half. nginx is stopped, and its access log does not grow; fresh clients of both source forms install 2.0 from the snapshots. The checks their updates trigger fail. The k8s suite's `check_fail_count` is positive, and its row on the status page shows "backoff ends in", the text `main.go`'s `CheckBackoff` wiring adds. Then nginx starts again. It runs last before `whole-run`: every suite checked while nginx is down is left in a backoff (2 minutes after one failure). |
| `whole-run` | No `unvouched_deb_refused` on `signed.test` but `acu-rogue`'s, and none on `plain.test`. On a short image, `/unsigned404` is still unadopted (checked while under 5 minutes after first contact). The line printed says whether a retry was refused by then, so whether this check could have caught a broken 5-minute floor. The daemon's log has no logged panic, no `package_hash_conflict` and no Go runtime crash text (`panic:` or `fatal error:` at a line's start); it is scanned again after the final stop. `acu_flip_gate_panics_total` and `acu_freshness_unlogged_panics_total` are declared and 0. The daemon is running and answers `healthz`. |

Two shapes of spec §7 are exercised only in part, and Go tests cover
both:

- **A binary detached `Release.gpg`.** apt (2.2 to 3.2) refuses one
  ("unsupported binary format"), so Jitsi's binary signature sits beside
  an `InRelease` that apt uses. `TestSniffReleaseGPG` covers it.
- **The 403 unsigned variant's first adoption.** It needs an hour, past
  any bounded run.
  `TestCheckFirst_UnsignedAfterADeniedSignatureWaitsAnHour` covers it.

## Debugging a failure

The failure message names the scenario. It prints the last 80 lines of
the daemon's log and the last 30 of nginx's access log. An unexpected
exit also prints the failing command. A failure on one image only may be
that image's nginx, not its apt: nginx comes from each client image.

To look around inside, keep the container. Run `test.sh` once per
container: it flips the fixtures and keeps the cache, so start a new
container to run it again.

```sh
docker build -f e2e/flat/Dockerfile --build-arg CLIENT_IMAGE=debian:12 -t acu-e2e-flat:debian-12 .
docker run -d --name acu-e2e-flat-debug acu-e2e-flat:debian-12 sleep infinity
docker exec -e E2E_IMAGE=debian:12 acu-e2e-flat-debug bash /usr/local/lib/acu-e2e/test.sh
docker exec -it acu-e2e-flat-debug bash   # /var/log/acu-e2e, /clients/<name>/*.log
docker rm -f acu-e2e-flat-debug
```

Add `-e E2E_UNSIGNED_WAIT=1` to the `test.sh` line to run the unsigned
scenario too. The daemon's log carries an
`msg=e2e_harness scenario=<name> unixtime=…` line at the start of each
scenario. `APT_CONFIG=/clients/<name>/apt.conf apt-get update` repeats a
client's update.

## Maintenance

- **An image past its end of life.** When a release's packages move to
  an archive mirror, its image build fails installing nginx, with
  `404  Not Found` from the mirror.
  - `debian:11` is already there: the `Dockerfile` points its
    `sources.list` at `archive.debian.org` for `bullseye` (main and
    security). Do the same for the next one (`old-releases.ubuntu.com`
    for Ubuntu).
  - The `fixtures` stage's `debian:bookworm-slim` meets the same move
    when bookworm's LTS ends (2028-06-30). Bump it then.
- **A transient mirror failure** fails one image's build. Each `apt-get`
  already retries three times; re-run that image.
- **A cached fixtures layer stays valid indefinitely.**
  - v1 is dated two days before the build and v2 one day before.
  - No fixture has `Valid-Until`, apt and the daemon refuse only a `Date`
    in the future, and the key never expires.
  - `docker build --no-cache-filter fixtures …` regenerates the key and
    the dates; a cold timing needs it.
- **Any change in the build context** (the repo, less `.dockerignore`'s
  entries), an `e2e/flat` script included, re-runs the `build` stage's
  `COPY . .` and `go build` locally, about 18 s.
- **Adding a scenario.**
  - Write a function in `test.sh` that starts with
    `scenario NAME DESCRIPTION` and ends with `passed`, and add it to the
    run list at the end of the file.
  - Keep `upstream_down` last before `whole_run`, and keep `commit_delay`
    ending well before 5 minutes after first contact (the AIDEV-NOTEs say
    why).
  - Wait only through bounded polls (`poll`, `poll_quietly`,
    `until_candidates`), never a bare `sleep`. Run curl through `CURL`
    (10 s), or with `--max-time 35` for an anchor through the proxy.
  - Assert on snake_case log events, the status JSON or `/metrics`, not on
    prose log messages.
  - Never end a pipe with `grep -q` (`pipefail`).
  - Give a check that could pass without its behaviour a control.
