# e2e/live: the live-upstream smoke

A manual check — before 1.1.0, and before later releases that change the
request path, adoption or vouching — that apt-cacher-ultra fronts real flat
repositories as their users configure them: pkgs.k8s.io, NVIDIA CUDA,
libnvidia-container, pkg.jenkins.io and download.sublimetext.com. It runs a
daemon built as a release builds it, with snapshot adoption on and strict
mode on (`integrity.refuse_unvouched_debs = true`), and real apt clients
(apt 2.6, `debian:12`) in one container.

`e2e/flat` tests every repository shape against fixtures, on six apt
versions, in CI; this smoke checks the live repositories those fixtures
copy. It depends on five vendors' servers, so it never runs in CI.

## Running it

```sh
make e2e-live
# or, with another image and container name:
E2E_LIVE_NAME=my-e2e-live bash e2e/live/run.sh
```

It needs Docker on an amd64 host (the CUDA repository and the packages are
amd64), and network access to Docker Hub, the Go module proxy (the build
stage's `go mod download`), `deb.debian.org` (the image build) and the five
vendors. It takes about 5 minutes: the build
(under a minute with cached layers) and about 4 minutes of smoke, most of
it waiting for the scheduler's tick (`periodic_refresh / 4`, 225 s by
default) that admits the first adoptions a client's check found no free
slot for. Ctrl-C stops it.

A run removes any container of its name first, a running one too, so two
runs with the same `E2E_LIVE_NAME` collide: give each checkout its own
name.

## What it checks

| Step | What it proves |
|---|---|
| `setup` | The vendors' keys (fetched from the vendors, for the clients and the daemon) and the daemon start; each client is an apt state directory with its own sources. |
| `first-contact` | Every client's first `apt-get update` through the cache is clean under e2e/flat's error grep (`apt_errors`: failures, timeouts, hash mismatches, and any signature error or `E:`/`Error:` line naming one of the five hosts), and every source line it prints (`Hit`, `Get`, `Ign`, `Err`) matches the smoke's signed-host filter, so each host's part of the filter is proven. A control source — a Kubernetes minor that does not exist, answered 403 — must print an error the grep catches, and its error lines must match the signed-host filter; a second control, a Sublime-shaped (host-root) source at a suite that does not exist, answered 404, must be caught by that filter alone. |
| `adoptions` | Every suite adopts, in the form its upstream publishes (inline, or detached for pkg.jenkins.io), and logs `adoption_success`: both source forms of two Kubernetes minors (four suites), CUDA, libnvidia-container (a SHA512-only `Release`), Jenkins and Sublime Text (a host-root base). |
| `updates` | Every client's next `apt-get update`, from the snapshots, is clean, its source lines again all matching the filter. |
| `strict` | On each of the five hosts, a `.deb` no `Packages` declares is refused (502, `unvouched_deb_refused`) before any upstream request: strict mode is on, so the installs below happen under it. |
| `installs` | One package per repository installs through the cache, and the daemon served that package's `.deb` vouched (`validated_hash=true` and its `package_name`): `kubectl` from each Kubernetes minor (one per source form), `cuda-toolkit-12-config-common` (CUDA's `./` Filenames), `nvidia-container-toolkit-base` (libnvidia-container's `./` Filenames), `jenkins` (Filenames one level above its `Release`) and `sublime-merge` (Filenames at the host root). |
| `whole-run` | No `unvouched_deb_refused` but the controls', no panic or hash conflict in the log, a clean stop, and no panic logged during it. |

## A failure: the cache, or the vendor?

The output names the step that failed and prints what it saw: apt's
output, the suites' status entries, or a strict control's coverage lines,
then the daemon log's tail, its `admin_request` lines left out.

- **The vendor:** a key that does not download or no longer signs (apt
  prints `NO_PUBKEY`, and the daemon's adoption fails its signature
  check), a retired Kubernetes minor (the first update fails with an error
  naming 403), a package that is gone (`no candidate`), or a server that
  answers 5xx or not at all (`Failed to fetch`). Check the vendor directly
  (`curl -fsSIL https://pkgs.k8s.io/core:/stable:/v1.37/deb/InRelease`:
  pkgs.k8s.io redirects every path, so only `-L` makes a missing file
  fail); fix the smoke's values (*Maintenance*) or run it again later.
- **A strict control answered 403 or 404 instead of 502:** strict mode
  was off for its host. Before it fails, the smoke prints the host's
  `package_coverage_incomplete` lines from the whole daemon log (each
  names the suite, the snapshot and a `reason`), or says there are none.
  It is the vendor's only when the logged reason is visible in the
  vendor's own files: `no_sha256`, a stanza of the suite's `Packages` with
  a `Filename` but no `SHA256`; `no_packages_members`, a `Release` that
  lists no root `Packages*` file. Such a snapshot cannot vouch for every
  package, which turns strict mode off for the whole host by design. The
  smoke then cannot pass on that host until the vendor's files change:
  record the reason and the file, and run it again later. Any other
  reason, a reason the vendor's files do not show, or no line at all is
  the cache's.
- **The cache:** anything else — an adoption that fails while upstream
  answers and the key is right, an update error a direct `apt update`
  does not show, a refusal of a package `Packages` declares, strict mode
  off for a host whose files give it no reason, a panic. That is a
  finding: reproduce it in a Go test or in `e2e/flat`, fix it there, and
  never change the smoke to pass around it.

## Maintenance

The values to change sit at the top of `smoke.sh`:

- **Kubernetes minors** (`K8S_A`, `K8S_B`): two minors pkgs.k8s.io
  publishes, `K8S_A` the older (one client installs the newest `kubectl`,
  which must be `K8S_B`'s, the other `K8S_A`'s newest). Move them when one
  is retired: pkgs.k8s.io answers 403 for a minor it does not publish, so
  the first update fails with an error naming 403.
- **The first control's premise:** the control's source is the minor
  `v1.0`, which must stay unpublished. Today it answers 403 (`curl -sL -o
  /dev/null -w '%{http_code}'
  https://pkgs.k8s.io/core:/stable:/v1.0/deb/InRelease`); a 404 passes the
  control too, which needs only an error line naming the host. The
  control fails, naming v1.0, only when apt prints none.
- **Forms** (`suite_expectations`): the form each suite adopts in
  (`inline`, or `detached` for pkg.jenkins.io); a vendor that adds or
  drops `InRelease` changes it, and `adoptions` then fails with "not
  every suite adopted in its form".
- **Keys** (`*_KEY`): the URLs the vendors' install instructions name.
  pkg.jenkins.io moved from `jenkins.io-2023.key` to `jenkins.io-2026.key`
  in 2026: its `Release.gpg` is signed by key `7198F4B714ABFC68`.
- **CUDA's distribution:** the repository is per distribution
  (`debian12`) and must match the image (`debian:12`); move both
  together, with the key file the repository's directory lists
  (`3bf863cc.pub` today).
- **Packages:** each is small and needs nothing outside the image. When a
  vendor adds a dependency, the install's apt error names it: add it to
  the Dockerfile's second package list.

## Results

| Date | Result | Times |
|---|---|---|
| 2026-10-26, 19:00 and 19:21 UTC (planning, phase H3) | both runs: all 8 suites adopted (4 Kubernetes, inline; CUDA, libnvidia-container and Sublime Text inline; Jenkins detached), every update clean, 6 packages installed vouched, 5 strict refusals (the controls), no other refusal | build 51 and 20 s, smoke 245 and 240 s (adoptions 221 and 224 s) |
| 2026-10-27, 14:00 UTC (planning, phase H3, with the host-root control) | the same, and both controls caught by the error grep | build 17 s, smoke 238 s (adoptions 223 s) |
| 2026-10-27, 20:47 UTC (phase H3, the final review's gate at 2b21024) | all 8 suites adopted in their forms (CUDA 9,763 packages), every update clean, both controls caught, 6 packages installed vouched (`kubectl` 1.37.1 and 1.36.5, `cuda-toolkit-12-config-common` 12.9.79-1, `nvidia-container-toolkit-base` 1.20.1-1, `jenkins` 2.580.1, `sublime-merge` 2132), 5 strict refusals, no other refusal | build 21 s, smoke 244 s (adoptions 221 s) |
| 2026-10-28, 01:57 UTC (phase H3, the final tree, 5381196) | the same (CUDA 9,768 packages) | build 19 s, smoke 243 s (adoptions 224 s) |

Each run adds its row here.
