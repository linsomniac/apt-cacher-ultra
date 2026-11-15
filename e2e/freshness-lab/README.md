# Manual freshness laboratory

This is a diagnostic experiment, **not part of the normal test suite or CI**.
It runs an actual ACU binary, a disposable signed APT repository, and a Debian
13 APT client inside one container. Runtime networking is disabled; the three
participants use loopback. It never mounts the host's APT state or a real cache.

The experiment distinguishes upstream publication, change detection, snapshot
publication, client list refresh, and successful installation. It was added to
investigate the October 2026 PHP update report against ACU 1.0.2. Injected faults
reproduce mechanisms, **not the customer's unknown root cause**.

## Run

Docker and a Linux amd64 ACU binary are required. Image construction downloads
Debian packages; the actual experiment has no external network access. Use a
fresh output directory for every run. Repository signing keys are generated
inside the disposable container and are not retained.

```sh
# Build the incident version without changing the working tree.
source_dir=$(mktemp -d /tmp/acu-102.XXXXXX)
git archive 1.0.2 | tar -x -C "$source_dir"
(cd "$source_dir" && CGO_ENABLED=0 go build -buildvcs=false \
  -ldflags '-X main.Version=1.0.2-freshness-lab' \
  -o apt-cacher-ultra ./cmd/apt-cacher-ultra)

bash e2e/freshness-lab/run.sh "$source_dir/apt-cacher-ultra" \
  /tmp/acu-102-baseline --scenario baseline

# Reuse the image for subsequent experiments.
ACU_FRESHNESS_LAB_SKIP_BUILD=1 bash e2e/freshness-lab/run.sh \
  "$source_dir/apt-cacher-ultra" /tmp/acu-102-slow \
  --scenario slow-prefetch --delay 90

ACU_FRESHNESS_LAB_SKIP_BUILD=1 bash e2e/freshness-lab/run.sh \
  "$source_dir/apt-cacher-ultra" /tmp/acu-102-repair \
  --scenario busy-repair --delay 90

# Test a shorter prepublication warming budget with the same upstream fault.
# Five seconds is an experimental contrast, not a proposed production default.
ACU_FRESHNESS_LAB_SKIP_BUILD=1 bash e2e/freshness-lab/run.sh \
  "$source_dir/apt-cacher-ultra" /tmp/acu-102-short-budget \
  --scenario slow-prefetch --delay 90 --prefetch-budget 5s

# Exercise the full default five-minute prefetch budget. Allow about seven
# minutes including baseline setup. The configured fault lasts 330 seconds,
# so the prefetch deadline, rather than origin recovery, ends the wait.
ACU_FRESHNESS_LAB_SKIP_BUILD=1 bash e2e/freshness-lab/run.sh \
  "$source_dir/apt-cacher-ultra" /tmp/acu-102-full-budget \
  --scenario slow-prefetch --delay 330
```

The default cooldown is **60 seconds**. Refresh and prefetch budget are now
omitted from the generated configuration unless explicitly supplied, so each
binary uses its own defaults. The historical measurements below used **15
minutes** and **5 minutes**, respectively; the updated defaults are **1 minute**
and **15 seconds**. The client runs `apt-get update` every five
seconds, retaining its list files and using normal conditional requests. There
is no accelerated daemon clock. `--cooldown`, `--refresh`, `--prefetch-budget`,
`--client-interval`, `--delay`, and `--max-wait` are configurable and recorded.

All runs install version 1.0 of `acu-lab-php8.4`, publish and adopt 1.1, install
1.1 through ACU to establish a hot package, then measure a publication of 2.0.
This extra baseline matters: 1.0.2 does not eagerly adopt unchanged metadata.
After APT sees 2.0, the harness installs it and verifies both dpkg's version and
the package's payload. Versions are synthetic; no actual PHP package is used.

The client uses a mirror-style URL resembling the incident:
`/debian/security.debian.org/debian-security/dists/trixie-security/InRelease`.
ACU maps it to `http://repo.test/debian-security/`; the local origin listens on
port 80 to avoid confusing this experiment with non-default-port URL issues.
Both ACU and APT verify the fixture's signature using its throwaway public key.

## Scenarios

| Scenario | Injected behavior | Question answered |
| --- | --- | --- |
| `baseline` | Healthy origin and small indexes | How much latency comes from cooldown, asynchronous serving, and publication? |
| `slow-prefetch` | Delay the 2.0 `.deb` response before headers | Does a package download keep already-verified new metadata unpublished? |
| `busy-repair` | Initially serve incorrect optional dep11 bytes; after adoption, delay their repair; publish 2.0 while repair is running | Can maintenance of the old snapshot prevent detection of a new security update despite repeated client requests? |

The repair case sets `member_retry_count = 0` to establish a degraded snapshot
without spending a minute in setup retries. This is recorded in `config.toml`.
It does not shorten the checker cooldown, refresh cadence, or injected repair
delay. The stalled repair is armed before publishing the measured generation.

The prefetch fault is removed before the final installation. This separates
metadata visibility from the later package download, and demonstrates the
tradeoff of a short prefetch budget: metadata can be current before the new
package is cached. It does **not** prove offline availability at publication.

GC, at-rest scans and the admin server are disabled to isolate the mechanism.
Everything is amd64 with a tiny catalog; this is not a throughput benchmark or
a simulation of a full Debian mirror. The fixture deliberately does not
advertise Acquire-By-Hash; it exercises signed canonical indexes, so current
HEAD's flip barrier is relevant. By-hash behavior and slow readers require a
separate coherence experiment before changing publication policy.

## Evidence

Each run retains:

- `timeline.jsonl`: monotonic elapsed time and wall time for origin requests,
  responses, publication, client observations, installation and captured ACU
  logs. ACU 1.0.2's JSON log lacks timestamps, so the harness timestamps lines
  when it receives them; these are observation timestamps, not internal timers.
- `acu.jsonl`: the daemon's original INFO output, unchanged.
- `apt-NNN.log`: every `apt-get update` and `apt-cache policy` result.
- `install-VERSION.log`: real package installation evidence.
- `config.toml`, `setup.log`, and `summary.json`.
- `cache/`: the disposable cache copied after daemon shutdown for inspection.

Each client observation records the body SHA256, snapshot ID, HTTP
Last-Modified, package candidate, and read-only database freshness state. The
origin digest is read from the active immutable fixture directly, avoiding
extra upstream diagnostic requests. A cached InRelease GET follows each APT
update and **also triggers T1**. Consequently these are client-driven runs,
not measurements of the periodic scheduler in isolation.

`summary.json` reports detection, prefetch start and adoption-success timestamps
relative to publication, plus the time when the client actually sees 2.0.
Publication uses different whole seconds to avoid accidentally testing
same-second HTTP timestamp collisions. Fetches and waits are bounded; any APT
error aborts the experiment instead of treating a retained old list as success.

The default five-second polling can itself prolong current HEAD's ten-second
flip quiet period. This is a useful load scenario, but should be compared with
a quieter client cadence before extrapolating a current-version baseline.

## Measured results (2026-11-14)

[measurements.json](measurements.json) preserves the source commits, compiler,
binary hashes, options, phase timings and raw-result locations. These are
single runs on a shared development host, with tiny synthetic catalogs. They
show where time is spent, not production performance percentiles. Both binaries
were rebuilt from clean archives using Go 1.26.8, not taken from a customer's
installation. Each listed run installed and verified versions 1.0, 1.1 and 2.0.

| Binary / scenario | Publication to detection | Publication to adoption success | Publication to client candidate |
| --- | ---: | ---: | ---: |
| 1.0.2, healthy | 55.9 s | 56.0 s | 61.1 s |
| 1.0.2, 90 s stalled hot package | 55.7 s | 163.4 s | 164.5 s |
| 1.0.2, same stall, 5 s prefetch budget | 55.7 s | 60.7 s | 60.8 s |
| 1.0.2, stalled package exhausts default 5 min prefetch budget | 55.6 s | 358.6 s | 364.1 s |
| 1.0.2, 90 s optional repair | 105.2 s | 108.9 s | 113.1 s |
| Clean HEAD, healthy, continuous polling | 60.1 s | 128.8 s | 128.9 s |
| Clean HEAD, 90 s optional repair | 92.3 s | 158.2 s | 158.3 s |

The 1.0.2 repair run records **39 client request logs and zero completed
freshness checks for 105.2 seconds** after the new publication, with no upstream
InRelease request in that window. The old snapshot keeps serving while repair
owns the suite lock. This directly reproduces the reported logging pattern,
without establishing whether repair caused the actual customer incident.

The full-budget run demonstrates a **6m04s client-visible delay with default
freshness and prefetch timings**, despite detection at 56 seconds. A 330-second
origin stall causes prefetch to reach its five-minute deadline; the failed
prefetch is logged, the verified metadata is published, and the subsequent
installation fetches the package on demand. Merely checking InRelease more
often does not remove this publication dependency.

The origin's injected delay is 90 seconds, but some observed intervals exceed
that by more than the five-second client cadence. Local processing, I/O and
scheduling also contribute; do not assign all elapsed time to the injected
network delay. The full event stream separates origin response completion from
the later repair/adoption completion.

In the clean-HEAD healthy run, its barrier log separately reports 60.0 seconds
waiting for quiet and another 5.1 seconds holding a request. In the repair run,
the newly published InRelease is still not checked until repair ends. These
observations distinguish the current consistency barrier from the older
mutex/prefetch behavior. No existing working-tree change was included.

## Independent review

[fable-review.txt](fable-review.txt) is the requested Claude Code review using
**Fable 5.2**. [review-provenance.json](review-provenance.json) records the model
and scope. The reviewer received a fixed bundle of relevant 1.0.2 source,
current HEAD admission code, a narrow diff of pre-existing local work, and
anonymized incident timings. It had no tools or filesystem access. This is an
independent analysis of those excerpts, not a full-tree audit.

Read its proposed numeric incident lag bounds cautiously: neither a file's
HTTP Last-Modified nor a CDN Age header establishes when that generation was
available at ACU's own upstream connection. The sample proves different bodies
were served, but does not establish the incident's total duration. The review's
speculative bugs and proposed changes require verification; its incomplete
view of HEAD is explicitly qualified in the response.

Useful next experiments are a multi-suite publication burst, global adoption
slot saturation, host fetch-slot saturation, publication of v3 while v2 is
stalled, and an upstream CDN/validator matrix. These are not yet implemented.

## Incident interpretation and change candidates

The email confirms adoption was enabled on 1.0.2; the adoption-disabled freeze
is therefore not the explanation for this incident. An unconditional 200 with
`X-Cache-Snapshot: 1920` and a different body from upstream establishes that an
older/different adopted release was being served. It does not distinguish a
change not yet detected from a replacement detected but not yet published.

The origin's stated Last-Modified to the cached sample is 11m19s, inside the
default background interval. That date is not a measured CDN availability
time. ACU 1.0.2 instead emits the pool file's local mtime as Last-Modified, so
subtracting the two HTTP dates does not measure refresh latency. The email
does not preserve repeated observations over a known duration.

The `apt-cache policy php` example queries the `php-defaults` dependency
package. Debian lists [php at 2:8.4+96](https://packages.debian.org/trixie/php)
while the [php8.4 source and runtime packages](https://packages.debian.org/source/stable/php8.4)
carry 8.4.27-1~deb13u1. A follow-up should compare `php8.4-cli`, `php8.4-common`
or `php8.4-fpm` and the actual cached Packages entries. A different InRelease
alone does not prove that this particular security version was absent.

The concrete 1.0.2 mechanism is in `internal/freshness/freshness.go:300`:
`Check` returns silently when its suite mutex is busy. The lock passes to
adoption, and also to the repair/reconcile goroutine at lines 337-344. In
`internal/freshness/adoption.go:859`, adoption waits for a global slot while
already holding that mutex; `runHotPrefetch` at line 1400 precedes the commit
at line 1414. These line numbers refer to **tag 1.0.2**, not today's checkout.
Thus optional repair can suppress detection, and package warming can suppress
publication. INFO client-request logging continues in both cases.

Clean HEAD `1ae10560e4219b44033ded642e4c0a4d088347ea` already has nonblocking
adoption admission, four scheduler check workers, a two-minute upstream-check
deadline, and a 30-minute adoption/repair deadline. It still holds the suite
mutex during repair and still warms packages before publishing metadata.
The 30-minute bound improves eventual recovery but does not meet a five-minute
freshness objective. The current flip barrier also adds intentional latency
under continuous client traffic; it protects canonical-index consistency.

Priorities for a subsequent production change:

1. Give release checks priority over optional repair/reconcile. Use short,
   cancellable maintenance batches or separate observation from mutation, and
   retain pending triggers instead of silently discarding them. Preserve the
   snapshot identity checks around repair and commit.
2. Give prepublication warming a short explicit budget, or publish verified
   metadata first and warm packages afterward. Keep an opt-in mode for users
   requiring packages warmed before publication; describe the offline tradeoff.
3. Expose the reason a check did not run, the active phase and its age, time
   since the last completed upstream check, and time since first seeing newer
   metadata. A completed-attempt duration alone does not diagnose a stuck run.
4. Reduce the background interval if five minutes is the target. One minute
   leaves room for index processing; a five-minute interval plus scheduler
   rounding and adoption does not promise five-minute availability.
5. Profile full catalogs and bursts before choosing concurrency changes. Keep
   signature/hash verification and snapshot coherence; test old by-hash reads
   before optimizing the flip barrier.

Pre-existing working-tree changes add adoption phase logs and reuse cached hot
package blobs. Those help diagnostics and redundant transfers, but a new
security-package version still needs its first download. The measurements here
use clean tag/HEAD binaries and do not claim to validate those unrelated edits.

## Publication burst experiment

The optional burst mode creates six suites split across two upstream hostnames
(or `--hosts 1` for a single-host burst). Each suite declares twelve metadata
files: main/contrib, amd64/arm64, plain/gzip/xz Packages. Main and contrib
are identical, giving six distinct blobs per suite and exercising content reuse.
Each index has 5,000
catalog entries and eight real hot packages; each real package has a 128 KiB
payload. The filler artifacts are catalog-only and cannot be downloaded.
The fixture installs all real packages before and after publication.

```sh
ACU_FRESHNESS_LAB_SKIP_BUILD=1 bash e2e/freshness-lab/run.sh \
  /tmp/acu-publication-after /tmp/acu-burst-after \
  --burst --suites 6 --hosts 2 --catalog-size 5000 --hot-count 8 \
  --member-delay 0.15 --deb-delay 8 --max-wait 1200

# No APT/cache requests during measured detection and adoption. Only a read-only
# DB observer runs; APT updates and installs after all snapshots are published.
ACU_FRESHNESS_LAB_SKIP_BUILD=1 bash e2e/freshness-lab/run.sh \
  /tmp/acu-publication-after /tmp/acu-burst-periodic \
  --burst --periodic-only --max-wait 1200
```

Both hosts resolve to the container's isolated loopback origin. Origin delays
apply only to generation 2.0 and are removed before the final installation,
as in the single-suite experiment. Each suite has distinct package content, so local
reuse cannot turn the burst into one shared warm. `summary.json` includes each
suite's detection, adoption start, publication and phase durations. Run compared
binaries sequentially to avoid making them compete for host resources. This
exercises larger catalogs and queueing, but is not a full Debian/Ubuntu mirror
benchmark or a five-minute availability guarantee.


## Implementation and follow-up measurements (2026-11-15)

The starting point was `fix/publication-delay` at `59f69a29b4ef`.
That overnight work already added concurrent metadata/hot-package fetches,
cancellable automatic maintenance, shorter mirror-sync retries, request-time
revalidation, and adoption phase/lag logs. This follow-up keeps those changes
and adds:

- One-minute background refresh and a 15-second default prepublication warming
  budget, including hot-set computation. Operators can explicitly retain a
  larger budget; warming remains best effort, not a publication prerequisite.
- Manual reconcile yields like automatic repair. Unchanged metadata probes run
  beside maintenance so a slow repair can finish across refresh intervals.
  One extra metadata-probe slot per host keeps bulk downloads from starving
  those probes. Changed/inconclusive metadata enters the ordinary locked
  check and verification path.
- Independent work watchdogs from verification/candidate creation through
  failure recording, including database writes, upstream operations, host
  slots and content locks. Busy logs identify the owner and phase, are rate
  limited, and complement pending-change age/count metrics and request
  last-check/pending-age fields.

Four Claude Code reviews used the canonical `claude-fable-5-2` model, with
fixed source bundles and tools disabled. The final reserved-slot review found
no concrete blocker in its scoped files. Its conditional concern was checked:
`probe()` delegates directly to `Conditional()` and neither acquires the bulk
host semaphore. The earlier reviews led to regression tests for unchanged
repair progress, cancellation/no-op stamps, pending state, held signature
upgrades, reserved probe capacity, panic cleanup and blocked database writers.
Review text and input hashes are in `implementation-review-provenance.json`.
Residual policy/refusal or inconclusive upstream states can still interrupt
maintenance on due checks; they use the normal retry/security state machine.

The continuous-client burst used six suites on two hostnames, 5,000 catalog
rows per index, 48 real packages, 150 ms member delays and 30 s package delays.
Both binaries used their own default refresh/warming settings and the same
60 s cooldown. All 48 packages were installed at versions 1.0, 1.1 and 2.0.

| Experiment | Earlier result | Follow-up client visibility |
| --- | ---: | ---: |
| Six-suite burst, overnight branch versus follow-up | 508.54 s | 243.21 s |
| One healthy suite, historical 1.0.2 versus follow-up | 61.1 s | 67.43 s |
| One package stalled 330 s, historical 1.0.2 versus follow-up | 364.1 s | 121.70 s |
| Optional repair fault lasting 90 s, historical 1.0.2 versus follow-up | 113.1 s | 185.38 s |

The repair row is deliberately included despite worse publication latency.
Detection improved from 105.2 s to 58.2 s, but the fault delays **every** request
for the optional member, including the new adoption. That adoption spent
about 60 s in members and 67 s waiting for the publication barrier. This
experiment therefore measures more than contention with the old repair. The
overnight branch also introduced a 60 s upstream-silence timeout, which ends
the 90 s repair stall near the next due check; deterministic blocked-fetch
tests separately establish cancellation priority and unchanged-repair progress.

The burst's warming phase normally fell from about 60 s to 15 s per suite.
One follow-up warm phase lasted 39 s because its post-fetch database heartbeat
also waited. Commit phases ranged up to 34 s, and publication barriers up to
71 s. The new live logs exposed `database_write` and `flip_wait` separately.
The warming budget cancels best-effort work; it is not a hard limit on database
synchronization, commit, queueing, or client-coherence waits.

With the final binary and the same six-suite workload, background-only
publication took **153.70 s**; the subsequent APT update saw all candidates at
**153.91 s**. No client cache requests ran during measured detection/adoption.
Detection occurred 19.10–76.99 s after publication, including scheduler phase
and local write delays. All 48 packages installed successfully afterwards.
This validates the one-minute background cadence independently of T1 requests.

The final-binary repair rerun detected the update in **60.37 s**, published in
**158.21 s**, and the client saw it in **169.95 s**; installation succeeded.
The intermediate binary's 185.38 s observation remains in the table above so
both measurements are retained. Neither repair run beats the historical
113.1 s publication result, despite substantially earlier detection.

These are single runs on a shared host, with variable filesystem/test load and
refresh phase. They establish mechanisms and a useful improvement, not a
latency distribution or five-minute guarantee. The continuous-client runs used
an intermediate implementation binary; final maintenance refinements are
covered by the final-binary experiments and focused race tests. Detailed
options, binary hashes, per-suite timings and raw-output locations are recorded
in `publication-measurements.json`. The original measurements remain unchanged
in `measurements.json`.

Existing installations must change explicit old settings to receive the new
latency defaults:

```toml
[freshness]
periodic_refresh = "1m"

[adoption]
hot_prefetch_budget = "15s"
```

A startup warning identifies settings that retain long refresh/warming delays.
Packages left unwarmed are fetched on demand. Faults are removed before the
lab's final installation: the experiment establishes metadata visibility and
installability, not package-download latency during a continuing outage.


### Validation

The complete `internal/freshness` suite passes on the final implementation.
Focused `-race` coverage passes for maintenance probes, repair/reconcile yield,
blocked-writer watchdogs and panic cleanup. The affected config, fetch, cache,
host-semaphore, observability and command package tests also passed, as did
`go vet`, `git diff --check`, Python parsing and shell syntax checks.

The handler suite passed all functional regressions after updating expected
budget-expiry log names. Its load-sensitive `TestChaos_HungUpstreamGated`
latency assertion measured p99 117 ms against a 100 ms threshold during the
combined run; rerunning that test alone passed. This host also had another
full race-test run in progress, so the wall-clock experiments should not be
treated as a controlled throughput benchmark. No timing thresholds were relaxed.
