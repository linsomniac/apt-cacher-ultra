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

- Docker Hub (`golang:1.27-bookworm`, `debian:bookworm-slim`, the client
  images);
- the distribution mirrors the image builds install from
  (`deb.debian.org`, `archive.debian.org`, `archive.ubuntu.com`,
  `security.ubuntu.com`);
- the Go module proxy (the build stage's `go mod download`).

The fixture packages are built for amd64; three, Jenkins's two and
Jitsi's, are `all`. On an arm64 host, such as Apple Silicon or an arm64
runner, apt finds no candidate for most of them and the run fails.

`run.sh` does not pull: it uses a client image already on the machine as
it is. To test what CI tests (CI starts with no images), `docker pull` the
client images first.

Each image is built and run on its own. One failing does not stop the
others, and the exit status is 1 if any failed. An unsupported image name
or `E2E_FLAT_NAME` exits 2 before anything is built. `run.sh` runs on
bash 3.2 or later, macOS's `/bin/bash` included.

Stopping a run:

- **Ctrl-C** stops the whole run. It reaches `docker run` too, which
  passes it to the container, so the running test ends at its next
  command, printing its logs. `run.sh` then exits 130 instead of going on
  to the next image.
- **A TERM sent to `run.sh` alone** is not passed to the container. The
  current image's build or test finishes first, and then `run.sh` exits
  143 instead of starting the next.
- **`docker stop` on one test container** ends that image's test at its
  next command. It counts as failed, and the run goes on.

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
  - Together that is about 13% of long-wait runs, roughly one in seven
    or eight. This is derived from the retry schedule (2, then 4
    minutes, each ±25%), not measured.
- **All six in sequence:** 24.5 minutes.

CI runs one job per image (`.github/workflows/ci.yaml`, job `e2e-flat`).
Each row's `unsigned` boolean sets both whether it runs the unsigned
scenario and its timeout: 15 minutes for the short images and 30 for the
two that wait. The test step runs under `timeout -s INT -k 60`, 3 minutes
inside the job's limit, so a hang usually still ends with the test's own
failure output: the INT reaches the container as Ctrl-C does. (In 7 local
trials docker run relayed that output 6 times; once, the container got the
INT and exited, but docker run returned without its last lines.) No CI run
has been timed yet. The estimates below are the local
test times above plus a fresh runner's 2–5 minute cold build (the H2
plan's Task 10):

- a short job takes about 5–8 minutes;
- a long-wait job takes about 8–12 minutes usually;
- a long-wait job takes about 16–21 minutes in the third-retry case: the
  14 minutes measured once, or up to about 16 by the harness's bounds,
  plus the build.

With two long-wait jobs, roughly one CI run in four has one that long
(derived, as above).

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
    `is not signed`, and any error line (`E:` on apt 2.x, `Error:` on
    3.x). An anchor answered 404 prints only such a line,
    `E: The repository '…' does not have a Release file` ("no longer has"
    for a client with old lists), and apt then keeps the old lists. These
    count only on lines naming the signed host or the mirror prefix
    (`//signed\.test[/ ]|/m-signed[/ ]`). The space in that pattern is for
    a host-root source, which apt names with no slash after the host:
    `http://signed.test apt/stable/`.
  - The captive portal's aftermath prints the same signature words and
    `E:` lines for `plain.test`, so they do not count there.
- **apt's `Debug::Acquire::http` request lines**, and nginx's access log:
  the request lines the cache sent upstream.
- **The daemon's own record:** its snake_case log events, the admin
  status JSON (`/?format=json`) and `/metrics`. One status-page text is
  read too: a failing suite's "backoff ends in".
- **The proxy's answers, read with curl:**
  - status codes: 502 for `acu-rogue`, 403 for the k8s shape's missing
    file, 404 for `/fury`'s `InRelease` after the portal, and 200 for an
    unvouched `.deb` on `plain.test`;
  - the `X-Cache-Snapshot` header on the k8s `InRelease` after the
    restart;
  - the body of `/fury`'s `Packages.gz` behind the portal.

The checks that could pass vacuously have controls on every image:

- `apt_update` must fail an update from a host the cache refuses
  (`nowhere.test`).
- It must also fail two signed sources checked against the wrong key: the
  `dists/` one, and the host-root one.
- It must fail a signed source with no `Release` at all
  (`signed.test/no-such-repo`, whose `InRelease` and `Release` nginx
  answers 404): apt prints only its `E:` line for it.
- The request-line check must not match a line apt never sent.
- `poll_quietly` must time out on `false`.
- Strict mode must refuse `acu-rogue` (502), so "no refusal" elsewhere
  means something.

Every wait is bounded:

- `poll` and `poll_quietly` try once a second;
- `until_candidates` runs one update per cooldown;
- `commit-delay`'s loop stops at 150 s.

Every curl has a time limit: 10 s, or 35 s for an anchor through the
proxy, which a flip can hold for 20 s. A failure prints the scenario,
whether the daemon is still running, the daemon log's first panic or
crash lines if it has any, the last 80 lines of the daemon's log but its
`admin_request` lines (with how many it left out) and the last 30 of
nginx's access log. An unexpected exit, such as a `set -e`
exit, also prints the failing command and its call chain.

The daemon runs as four processes in turn: from setup to `commit-delay`,
the 45 s one, from there to `restart`, and from `restart` to the end.
Each planned stop (`stop_daemon WHICH`) first checks that process:
`acu_flip_gate_panics_total` and `acu_freshness_unlogged_panics_total`,
the panics it recovered without logging them, are declared on `/metrics`
and 0, summed over every sample of each name. The counters live in
process memory, so each process is read as it ends; the 45 s one is the
only one whose commit outlasts its hold, the flip gate counter's only
source. After the SIGTERM the daemon must exit with status 0. A failure
names the stop, such as `the stop after the 45 s commit delay`.

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
check carried it. Its premise is asserted before each publish: the suite
has no adoption record (`adoption_fail_count` 0) and no pending change
(no `inrelease_change_seen_at_unixtime`), either of which would let the
scheduler check it sooner. "Within one cooldown" is bounded at the
cooldown plus 20 s (22 s); the measured waits were 4–8 s.

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
| `restart` | The daemon really restarted: `process.started_unixtime` is no earlier than the stop. It kept the same snapshots (every signed suite's id, none empty) and the main client's candidates, an update is clean, and the k8s `InRelease` is served from its snapshot (`X-Cache-Snapshot`). The clean update covers every signed suite's anchors: the main client keeps its old lists when one answers 404, but apt's `E:` line for it counts. |
| `upstream-down` | PR #8's table, second half. nginx is stopped, and its access log does not grow; fresh clients of both source forms install 2.0 from the snapshots. The checks their updates trigger fail. The k8s suite's `check_fail_count` is positive, and its row on the status page shows "backoff ends in", the text `main.go`'s `CheckBackoff` wiring adds. Then nginx starts again. It runs last before `whole-run`: every suite checked while nginx is down is left in a backoff (2 minutes after one failure). |
| `whole-run` | No `unvouched_deb_refused` on `signed.test` but `acu-rogue`'s, and none on `plain.test`. On a short image, `/unsigned404` is still unadopted and waiting `unsigned_unconfirmed` (checked while under 5 minutes after first contact). The line printed says whether a retry was refused by then, so whether this check could have caught a broken 5-minute floor. The daemon's log has no logged panic, no `package_hash_conflict` and no Go runtime crash text (`panic:` or `fatal error:` at a line's start); it is scanned again after the final stop. The daemon is running and answers `healthz`. The final stop then checks the last process's counters and exit status, as every planned stop does (above). |

Two shapes of spec §7 are exercised only in part, and Go tests cover
both:

- **A binary detached `Release.gpg`.** apt (2.2 to 3.2) refuses one
  ("unsupported binary format"), so Jitsi's binary signature sits beside
  an `InRelease` that apt uses. Two Go tests cover the cache's side:
  `TestSniffReleaseGPG` (the sniff's binary cases) and
  `TestVerifyDetached_HappyPath_Binary` (verifying a binary detached
  signature).
- **The 403 unsigned variant's first adoption.** It needs an hour, past
  any bounded run.
  `TestCheckFirst_UnsignedAfterADeniedSignatureWaitsAnHour` covers it.

## Debugging a failure

The failure message names the scenario. It then prints:

- `daemon pid N: running` or `gone` (a crash mid-scenario otherwise
  reads as whatever poll or update was running);
- the daemon log's first panic, conflict or crash lines, if it has any,
  since a long crash trace can push its `panic:` line out of the tail;
- the last 80 lines of the daemon's log, leaving out its `admin_request`
  lines (each status poll logs one; a count says how many), and the last
  30 of nginx's access log.

An unexpected exit also prints the failing command and its call chain,
each function with the file and line that called it. The command is the
text as written, so inside a helper it shows the helper's `"$@"`. A
helper called with a redirection, such as `capt … > FILE`, has left the
stack by then (bash 5.1 and 5.3): the chain starts at its caller.

When apt's output fails a check, apt's whole output is printed above the
failure line, each line marked `|`; for a failure on one apt version,
that is usually the part to read. A failure on one image only may also be
that image's nginx, not its apt: nginx comes from each client image. A
`did not report … within 22s` failure lists what each client reported
instead.

A failure at a planned stop names the stop: a counter that is not 0,
such as
`acu_flip_gate_panics_total is 1 at the stop after the 45 s commit delay`,
or an exit status, such as
`the daemon exited with status 2 at the restart's stop` (a Go crash exits
2, and its text is in the daemon log).

A `timed out after 40s waiting for the daemon to exit` failure is
`stop_daemon`'s bound. It covers the daemon's 30 s HTTP drain, its 1 s
tunnel grace and a margin; a planned stop has nothing in flight and takes
about a second. If it happens in `commit-delay`, one of two things kept a
commit in flight under the 45 s delay (the scenario's AIDEV-NOTE): a
broken 5-minute floor (an unsigned adoption at its 90–150 s first retry),
or a slow runner that reached 300 s after first contact, when that
adoption is due, before the stop. The say lines' times tell them apart:
the line
`stopping the daemon (the stop after the 45 s commit delay)`
less than 300 s after `=== scenario first-contact` points at the floor;
300 s or more, at the runner.

### Flake, environment or daemon bug?

CI does not retry a failed job, by design: a retry would hide a flaky
scenario. So before re-running a red job, keep its log and classify the
failure.

- **`[e2e-flat] <image> FAIL (build)`** is the image build, the only part
  that uses the network: a distribution mirror, Docker Hub or the Go
  module proxy. A mirror failure names the mirror, such as
  `Failed to fetch http://deb.debian.org/…` or `404  Not Found`. Re-run
  that job; a mirror that moved for good is a maintenance item (below).
  A build can also fail on the code itself (a Go compile error, a
  Dockerfile change): that fails the same way on every image, and on a
  re-run.
- **`[e2e-flat] <image> FAIL (test)`** is the test, which uses no network:
  its only upstream is its own nginx. So it is never a network flake.
- **The baseline is clean.** Every scenario passed on all six images in
  the H2 plan's runs, in its Task 9 run and in every review run since. A
  test failure is a harness, image or daemon change until shown
  otherwise.
- **Compare the apt version** that `setup` prints first
  (`apt 2.6.1; fixtures key …`) with the table above. The image tags
  float; a new apt is a harness maintenance item, neither a flake nor a
  daemon bug.
- **A timing bound against its measured values** (Task 9, one 12-core
  machine; the say lines print each run's own waits, and each
  `--- scenario X passed (Ns)` line and the final `PASS in …` line its
  times):

  | Bound | Measured |
  |---|---|
  | `REACH`, a publish reaching a client, 22 s | 4–8 s |
  | `by_client`, after the suites' last success, 30 s | 4–9 s |
  | `/fury`'s scheduler refresh, 48 s | 33–35 s |
  | an update in `commit-delay`, 28 s | 17–18 s (the held one) |
  | a held request (`flip_held_ms`), 20 500 ms | at most 17 914 ms |
  | `stop_daemon`, 40 s | about 1 s |
  | the unsigned adoption, 968 s after first contact | 325–855 s |

  A miss at one of these bounds on a loaded runner may be the runner:
  re-run it once, keeping the first log, and if it repeats it is real. A
  content check failing is never a timing flake: an apt error, a wrong
  version, a 502 or 404 where 200 was expected, a panic, a hash conflict,
  a counter or an exit status at a stop.
- **A daemon bug** gets a failing Go regression test first, then the fix,
  and the scenario that caught it stays as it is (ruling R-H2-4 of the H2
  plan, `docs/superpowers/plans/2026-09-28-flat-repositories-H2-e2e.md`).
  Never widen a bound or weaken an assertion to make a run pass. The
  plan's Task 9, Step 4 ("Know what the planning runs fixed") lists the
  harness failures the planning met: check a new failure against it to
  tell a harness regression from a daemon bug.

### Looking inside

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
scenario. `/var/log/acu-e2e/metrics.txt` is `/metrics` as the last
planned stop read it.
`APT_CONFIG=/clients/<name>/apt.conf apt-get update` repeats a client's
update.

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
  - Check what your scenario needs from the ones before it, and what it
    leaves for the ones after. Some depend on the order, and the comment
    above each such function says how: `older-date` reuses `pr8-table`'s
    clients; `mirror-mode` comes after `pr8-table` and before
    `commit-delay`; `unsigned-first-adoption` needs `anchorless`'s
    goreleaser 2.0; `upstream-down` needs `pr8-table`'s 2.0 `.deb`s in
    the cache.
  - Wait only through bounded polls (`poll`, `poll_quietly`,
    `until_candidates`) or a loop with its own bound, as `commit-delay`'s,
    never a bare `sleep`. Run curl through `CURL` (10 s), or with
    `--max-time 35` for an anchor through the proxy.
  - Stop the daemon only through `stop_daemon WHICH`, naming the stop: it
    checks the process's counters and exit status.
  - Assert on snake_case log events, the status JSON or `/metrics`, not on
    prose log messages.
  - Never end a pipe with `grep -q` (`pipefail`).
  - Give a check that could pass without its behaviour a control.
