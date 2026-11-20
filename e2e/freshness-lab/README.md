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

## Release 1.0.5 versus main (2026-11-15)

The same client-driven burst was rerun against clean source archives of
release **1.0.5** (`563c5719b9db`) and **main** (`5b65bd96e967`). Both were built
with Go 1.26.8 and `CGO_ENABLED=0`, then run sequentially in the same isolated
Docker image. [v1.0.5-main-measurements.json](v1.0.5-main-measurements.json)
records full commit IDs, binary/image/harness hashes, options, per-suite
observations, installation checks, host resource samples and raw-log locations.

The workload matches the earlier continuous-client burst: six suites on two
hostnames, 5,000 catalog rows per index, eight real hot packages per suite,
150 ms metadata-member delays and 30 s package delays. APT polls every five
seconds after each update completes. Both use a 60 s cooldown and two adoption
slots; refresh and warming settings are omitted to exercise each binary's
defaults (15 min/5 min for 1.0.5; 1 min/15 s for main).

| Time from measured upstream publication | 1.0.5 | Main |
| --- | ---: | ---: |
| All six changes detected | 47.75 s | 52.91 s |
| First updated package candidates visible to APT | 306.01 s (5m06s) | 53.16 s |
| All six snapshots published | 789.04 s (13m09s) | 163.39 s (2m43s) |
| All 48 updated candidates visible to APT | **794.15 s (13m14s)** | **163.69 s (2m44s)** |

Main reduced full client-visible delay by **630.46 s (10m30s), or 79.4%**,
giving **4.85 times faster** availability in this run. Each run successfully
installed all 48 packages at versions 1.0, 1.1 and 2.0 and verified the installed
versions and version files. Neither run had an APT failure or measured
WARN/ERROR daemon log. The two disposable containers exited successfully.

The main difference was after detection. On 1.0.5, the first pair of suites
began warming at 49 s, the second at 305 s and the last at 548 s. Each downloaded
its eight packages serially; the interval from warming start through commit
was 240–253 s per suite. Its last snapshot stayed unpublished for more than
twelve minutes despite detection in under one minute.

Main's warming phases each lasted about 15 s. All new packages remained
unwarmed: each suite recorded four in-flight downloads canceled by the budget
and four unattempted downloads. Those cancellations were logged at INFO, and
publication succeeded. Five suites spent about 4.3 s in `flip_wait`; the last
spent 68.23 s there under continued client polling. The INFO watchdog identified
that suite and phase while it waited. Commit phases were below 0.4 s in this
run, whereas the earlier shared-host run had substantially longer database and
barrier waits. This variation is another reason to avoid a five-minute SLA.

### Slower setup transition on main

The harness also exposes a separate result worth following up. Before the
measured burst, it publishes version 1.1 to establish adopted snapshots and
hot-package history. This transition has **no injected origin delays**, but
starts with pass-through or only partially adopted cache state. All 1.1
candidates became visible in **64.28 s on 1.0.5 versus 394.46 s (6m34s) on
main**. Main's logs show five queued first adoptions and one update of an
existing snapshot, with approximately 67–70 s of `flip_wait` per adoption.
The queue and consistency barrier deserve a separate cold-cache investigation.
This setup observation is retained in the JSON; it is not included in the
established-snapshot 2.0 burst times above.

These are one run per version on a shared development host, not production
percentiles. Client requests trigger freshness checks, so this comparison does
not isolate the periodic scheduler. The fixture uses canonical indexes without
Acquire-By-Hash. Injected download delays are removed before the final install;
successful installation demonstrates on-demand retrieval, not download latency
during an ongoing outage or offline availability at publication. The source
and harness were unchanged for these runs; only these experiment records were
added to the repository.

To repeat the workload with a freshly built static binary and a new output
directory:

```sh
ACU_FRESHNESS_LAB_SKIP_BUILD=1 bash e2e/freshness-lab/run.sh \
  /path/to/acu-binary /tmp/acu-new-comparison-run \
  --burst --suites 6 --hosts 2 --catalog-size 5000 --hot-count 8 \
  --member-delay 0.15 --deb-delay 30 --max-wait 1800
```

## The setup transition after the barrier fix (2026-11-15)

The slower setup transition above came from the publication barrier, and
the daemon's own logs of that run say how. Each of the six switches spent
`phase_a_seconds` = 60.00 waiting for the suite to go quiet by itself — ten
seconds without an `InRelease` request — which a client that runs `apt-get
update` every five seconds never allows; then it held one request for 7–10 s
and switched. An adoption keeps its slot while it waits, and first adoptions
may use only one of the two slots, so the five queued first adoptions ran
back to back at about 68 s each. They needed the barrier at all because 1.1
was published seven seconds after first contact, while they were still
queued: the release they adopted was no longer the one clients had been
served. Release 1.0.5 has no barrier; its 64 s is a 60 s cooldown and six
sub-second adoptions, with nothing protecting a client that holds the
previous `InRelease`. The same 60 s is the last switch of the 2.0 burst
above (68.23 s in `flip_wait`); the other five found a pause after 4.3 s only
because revalidation waits had stretched the client's polling past ten
seconds.

The barrier now stops waiting for a suite that is being read: once one of
the suite's `InRelease`/`Release` responses has finished during the wait, a
new request for it has arrived since, and the suite is still not quiet a
quiet period after that response, it goes on to hold. A suite nobody asks
for meanwhile, the wait for other suites' holds, and the conditions for the
switch itself are unchanged. The same workload, with the
same harness and image as the comparison above, was run twice with that
build, one run after the other.
[barrier-fix-measurements.json](barrier-fix-measurements.json) records every
switch's barrier timings, the held requests and the evidence hashes.

| | 1.0.5 | Main before | Fix, run 1 | Fix, run 2 |
| --- | ---: | ---: | ---: | ---: |
| Setup: all 1.1 candidates visible | 64.28 s | 394.46 s | 106.32 s | 111.12 s |
| Burst: all 48 2.0 candidates visible | 794.15 s | 163.69 s | 134.35 s | 133.13 s |
| Longest wait before a switch held (`phase_a_seconds`) | — | 60.00 s | 15.34 s | 25.41 s |
| Longest hold | — | 10.08 s | 6.19 s | 6.09 s |
| Requests held, and the longest | — | 10, 5.0 s | 11, 5.3 s | 10, 5.1 s |
| Longest `apt-get update` | 4.92 s | 10.30 s | 10.72 s | 10.53 s |

Every switch in both runs ended inside its hold (`held`); none was cut
short, expired or went unheld, and no switch waited for another suite's
hold token. All 48 packages installed at each of the three versions. A busy
suite now takes about 20 s through the barrier: around 14 s until its pause
is known to be broken (the first poll after the adoption is ready, then the
quiet period), and a hold of about 6 s. Six of them in a row is the 106–111 s.
The longest wait, 25.41 s, was a switch that found another suite's hold in
its way and waited for it, and for the quiet period after it, without
holding.
What remains above 1.0.5 is that sequence: one suite at a time, each given
the quiet period the barrier exists for. `adoption.flip_quiet_period` sets
its length (10 s here); `0s` removes the barrier and the protection.

Two simpler changes were measured first, as throwaway builds, and rejected.
Holding at once (no wait for a pause) switched the setup in 69.07 s, but
holds then follow each other without a gap and one client is caught by each
in turn: one `apt-get update` took 47.94 s. Bounding the whole first phase
by the quiet period gave 113.09 s and 140.95 s, but a switch that finds
another suite's hold in its way then holds straight after it; one request
waited 13.2 s in that run, and the pattern can chain whenever a hold
outlasts the quiet period. The change made keeps the wait for other suites'
holds as it was.

These are two runs on a shared development host under other load. Whether
a pause is found depends on how the five-second poll lines up with the
ten-second quiet period, so single runs of this workload differ by tens of
seconds; the comparison that matters is the 60 s per switch that is gone.

## Comparison after merging the barrier fix (2026-11-15)

After `fix/flip-barrier-busy-suite` reached local main at
`fb92fe333474a300d02ba68f0155fa48ceed99ed`, the same benchmark was repeated
against release 1.0.5 (`563c5719b9db94b803dbed14e50ab5512ef95408`). The control
ran while the merge completed; main was built from a clean archive only after
the branch's inclusion in main was verified and the control had finished.
A second main run used the identical binary to investigate a hold expiry in
the first run. All three runs were sequential, with a fresh cache each time.

[post-merge-main-measurements.json](post-merge-main-measurements.json) preserves
the merge verification, source and binary hashes, unchanged harness/image,
options, resource samples, per-suite timings, barrier outcomes, request holds,
installation verification and raw evidence hashes. The workload is unchanged:
six suites, two hostnames, 5,000 catalog rows per index, 48 hot packages,
150 ms member delays, 30 s package delays and five-second client polling.
Each version uses its own refresh/warming defaults, with a 60 s cooldown and
two adoption slots. Go 1.26.8 and `CGO_ENABLED=0` were used for both builds.

| Client-visible result | 1.0.5, fresh run | Earlier main `5b65bd9` | Merged main, run 1 | Merged main, repeat |
| --- | ---: | ---: | ---: | ---: |
| Setup: all 1.1 candidates | 65.91 s | 394.46 s | 115.82 s | 117.83 s |
| Burst: first 2.0 candidates | 286.53 s | 53.16 s | 46.18 s | 25.03 s |
| Burst: all 48 2.0 candidates | **781.73 s (13m02s)** | **163.69 s (2m44s)** | **150.43 s (2m30s)** | **128.12 s (2m08s)** |

The earlier-main column is the historical run above, not another run in this
batch. The merged binary's observed burst delay was 80.8–83.6% lower than the
fresh 1.0.5 control and 8.1–21.7% lower than the earlier main. Setup delay was
70.1–70.6% lower than the earlier main. Setup remains slower than 1.0.5, which
has no publication barrier. Both merged-main runs used about 15 s of warming
per suite and published with the new packages unwarmed; 1.0.5 downloaded all
eight packages per suite before publication.

### Barrier behavior and the repeat

All six setup switches completed within their holds in both main runs. The
longest waits before holding were 16.18 s and 19.62 s, versus the earlier
main's repeated 60 s waits. The longest such waits during the measured bursts
were 25.19 s and 14.95 s. The busy-suite change therefore removed the repeated
minute-long waits in these runs while preserving the barrier.

The first main burst also exposed the existing slow-commit fallback. Its last
suite spent **11.77 s in commit**, and the barrier logged **`hold_expired`**
when its 20 s hold bound was reached. The request log recorded a 10.54 s hold;
that request received the new snapshot. The longest measured `apt-get update`
took 13.11 s. APT completed successfully without a reported consistency error,
but this event is retained as a stress observation, not described as a normal
within-hold switch. The code deliberately releases a hold at its bound without
canceling an in-progress commit; this benchmark does not establish why that
commit took longer or prove a new regression in the barrier change.

The unchanged-binary repeat had **no expired holds**: all twelve setup/burst
switches completed within their holds, measured commit phases stayed below
0.41 s, and the longest individual request hold was 5.10 s across setup and
burst. Its longest `apt-get update` was 10.64 s during setup and 5.40 s during
the measured burst. The first run's slow-commit event did not recur in this
repeat; that is not a guarantee it cannot recur under load.

Every run installed all 48 packages at versions 1.0, 1.1 and 2.0 and verified
the installed versions and version files. All three containers exited
successfully. There were no measured WARN/ERROR daemon records; the hold expiry
is an INFO event, illustrating why its structured outcome must also be checked.

The host was busy: the first two runs started with one-minute load averages
near 25 on 12 CPUs, much higher than the earlier comparison. Shared-host CPU
and I/O load and polling/cooldown phase affect the timings. These few samples
support the phase-level finding, not precise production speedup percentages
or a five-minute guarantee. As before, the fixture uses canonical indexes
without Acquire-By-Hash, client requests can trigger checks, and download
delays are removed before the final installation. These results measure
candidate availability, not installation latency during a continuing outage.

## A real suite's size and a real publication's change (2026-11-18)

Until now every publication of the burst changed every catalog entry, and
the catalog was 5,000 entries. A real suite is the other way round: the
maintainer's June production database held 45,000 to 67,000 package rows a
suite, of which 0 to 241 differed between the snapshots it kept. Two
options make the burst say something about that:

- `--catalog-churn F`: the share of catalog entries that take each new
  version; the rest stay at 1.0. The default, 1.0, is the old behaviour
  and the old fixture bytes.
- `--xz-preset N`: the xz preset of the indexes (default 6, the old
  bytes); 1 shortens the setup of a large catalog.

`--catalog-size 33000` gives about 66,000 package rows a suite (two
architectures; both components carry the same paths).

```sh
ACU_FRESHNESS_LAB_SKIP_BUILD=1 bash e2e/freshness-lab/run.sh \
  /path/to/acu-binary /tmp/acu-burst-real \
  --burst --suites 6 --hosts 2 --catalog-size 33000 --catalog-churn 0.003 \
  --hot-count 8 --member-delay 0.15 --deb-delay 8 --xz-preset 1 --max-wait 1800
```

Baseline, `main` 421f5ea, before the package-list-per-suite work
(`docs/superpowers/specs/2026-11-18-package-list-per-suite-design.md`);
one run each, one after the other, on a shared host with a load average
of 7 to 9 (29 when the single-suite run began).
[package-list-baseline-measurements.json](package-list-baseline-measurements.json)
has every attempt's phases and every `database_write_waited` line.

| | Publication to all candidates visible | Commit phase per suite | Longest wait of another write behind a commit |
| --- | ---: | ---: | ---: |
| One suite, the "PHP update" scenario | 59.5 s | 0.002 s | none |
| Six suites of 66,000 rows, 0.3% changed | 189.8 s | 3.1 to 17.8 s (40.4 s in all) | 5.4 s (a client's `url_path_touch`) |
| Six suites of 66,000 rows, all changed | 223.8 s | 5.1 to 22.9 s (76.3 s in all) | 25.8 s (an adoption behind two commits) |

At a real suite's size the commit is seconds where the 5,000-entry catalog
showed under 0.4 s, and it is seconds whether 0.3% or all of the rows
changed: the whole list is written either way. The single-suite scenario's
59.5 s is detection (39.3 s) and the publication barrier (20.1 s); its
commit is 2 ms, so a cheaper commit cannot show there. In the bursts the
other phases per suite were 5 to 9 s parsing indexes (6 to 22 s with every
row changed), 15 to 24 s of warming (the injected 8 s package delay
exhausts the 15 s budget) and 3 to 47 s in the barrier.


## One package list per suite: the result (2026-11-19)

The change the baseline above was taken for: a suite's package rows are
one list, and an adoption writes the rows a publication changed instead
of the whole list
(`docs/superpowers/specs/2026-11-18-package-list-per-suite-design.md`).
Branch `perf/package-rows-per-suite` against `main` 421f5ea, the same
three scenarios, branch and main alternating on a machine doing nothing
else of the measurer's (load average 0.4 to 2.5 at each run's start). The
first sitting (2026-11-19, branch at 602de26) ran each burst once a side
and the single suite twice; a second (2026-11-20, branch at cb73f3f) ran
the 0.3% burst twice more a side, after a third branch run showed how far
its publication time moves between runs. The fourth branch run of that
burst is the first sitting's earlier one, at 4aa542c.
[package-list-result-measurements.json](package-list-result-measurements.json)
has every attempt's phases, how each commit wrote its list, and every
`database_write_waited` line, for all of them.

| | `main` | Branch |
| --- | ---: | ---: |
| **One suite, the "PHP update" scenario:** publication to client-visible | 59.6 s, 61.1 s | 59.8 s, 59.7 s, 59.6 s |
| **Six suites of 66,016 rows, 0.3% changed:** commit phase per suite | 2.26 to 3.12 s | 0.02 to 0.09 s |
| commit phase, the six suites together, per run | 14.9 s, 15.0 s, 15.6 s | 0.18 s, 0.35 s, 0.40 s, 0.41 s |
| publication to all candidates visible, per run | 160.5 s, 164.1 s, 180.1 s | 135.5 s, 136.5 s, 153.9 s, 160.1 s |
| writes that waited 2 s or more | none | none |
| package rows written per commit | 66,016 | 428 (214 in, 214 out) |
| database file after the run | 322 MiB | 176 MiB |
| **Six suites of 66,016 rows, all changed:** publication to all candidates visible | 163.0 s | 153.6 s |
| commit phase per suite (in all) | 2.29 to 2.58 s (14.4 s) | 1.75 to 1.89 s (10.8 s) |
| writes that waited 2 s or more behind a commit | 1 (a `blob_put`, 2.0 s) | none |

What it says:

- **A typical publication's commit is no longer seconds.** With 0.3% of
  the rows changed (the June data's shape) each commit wrote 428 rows
  where it wrote 66,016, and took 0.02 to 0.09 s where it took 2.3 to
  3.1 s: 15 s of commit per publication of the six suites became 0.2 to
  0.4 s, in every run.
- **How much sooner the six suites are all visible is not established.**
  The branch's four runs took 135.5 to 160.1 s and `main`'s three 160.5 to
  180.1 s. In each sitting the branch was ahead (136.5 s against 160.5 s
  the first day; 153.9 and 160.1 s against 164.1 and 180.1 s the second),
  but runs of one binary differ by 20 s, as much as the two sides do: the
  time is set by the publication barrier and the two adoption slots, and
  the barrier's waits alone summed to between 86 and 126 s a run. From
  the mechanism the gain should be the commit seconds along the slower
  slot's chain of suites, roughly 7 to 15 s here; the runs are consistent
  with that and do not pin it down. The first sitting's single pair
  (136.5 s against 160.5 s) overstated it.
- **The single-suite scenario did not move,** as expected: its minute is
  detection (39 s) and the publication barrier (20 s), and its commit was
  2 to 4 ms before and after. (Its one-package list is written whole on
  the branch: one changed row of one is more than half.)
- **A publication that changes everything costs about what it did,** a
  little less in these runs: the whole list is written as a new one
  (1.8 s a commit against 2.4 s).
- **The other phases are where the rest of the time is,** unchanged by
  this: 4.6 to 5.3 s parsing indexes per suite, the 15 s warming budget
  (the injected 8 s package delay exhausts it), and 12 to 35 s in the
  barrier.

Read with the baseline above: that table's 3 to 23 s commits were taken
on a host with a load average of 7 to 9. On a quiet machine `main`'s
commits are 2.3 to 2.7 s; the gain in this lab is from there. Load
stretches both. In a first pass the same day, while a reviewer's
experiments and compiles ran on the machine (load 12 to 27), `main`'s
commits took 2.4 to 30.6 s (61.6 s in all, 211.3 s to all visible) and
the branch's 0.07 to 16.4 s (42.3 s in all, 226.1 s to all visible):
three of its six commits took 12 to 16 s although each wrote 428 rows,
and client `url_path_touch` writes took 0.6 to 1.5 s each. A second
branch run minutes later at a load of 3 gave 0.02 to 0.06 s per commit
and 135.5 s. So on a machine whose disk is busy with something else, a
small commit can still stall for seconds; what in it stalls was not
found (evicting the database file from the page cache before a commit
does not reproduce it: below). The file's `first_pass_not_quiet` has
those headlines.

### The same commit on a real database

`TestMeasure_CommitOnARealDatabase` (`internal/cache`, skipped unless
`ACU_MEASURE_DB` names a `cache.db` copy) runs the real migration and the
real `CommitAdoption` on a copy of the maintainer's June production
database (schema 4 then; 1,159,431 package rows, 292,712 of them of
current snapshots, 24 suites with a current snapshot), for
`jammy-updates` (66,928 rows) with 241 rows changed per publication, the
most that database showed between two kept snapshots. Two runs on each
side, load average about 1; `main` 421f5ea ran the same steps with its
own reads and its plain commit.

| | `main` | Branch |
| --- | ---: | ---: |
| Commit, 241 of 66,928 rows changed | 2.0 to 2.5 s, 64 to 66 MiB of WAL (2.2 to 3.3 s and 140 to 144 MiB while the upgrade's displaced rows are still in the table) | 0.05 s, 1.8 to 2.2 MiB |
| the read of the live list before it (a reader connection, before the barrier) | none | 0.25 s |
| the same with the database file evicted from the page cache first | 3.5 s | 0.08 to 0.09 s (the read: 0.9 s) |
| Commit that writes the whole list (a first adoption; more than half changed) | 2.0 to 2.5 s, 64 to 66 MiB | 1.8 to 2.3 s, 67 MiB |
| Package rows left for GC by one adoption | 66,930 (1.0 to 1.5 s in 8 writer transactions, longest 0.16 to 0.29 s, 9 MiB) | none. A whole-list commit leaves the old list: two of them (133,863 rows) took 3.4 to 3.7 s in 15 transactions, longest 0.30 to 0.31 s, 12 MiB |
| The upgrade's backlog of displaced snapshots' rows (1,000,576) | GC's first runs: 35 to 51 s in 147 writer transactions, longest 1.0 to 1.3 s | gone with the old table, in the migration |
| First open of the schema-4 copy, every migration | 3.5 s | 8.9 to 13.8 s; the schema-11 step alone 5.3 to 10.2 s (four runs: 5.3, 5.3, 6.2, 10.2 s) |
| Package rows in the database afterwards | 1,159,431 | 292,712 in 24 lists |

The branch's GC figures are with the chunk delete keyed by rowid (the
commit after 602de26). Keyed by path, as at 602de26, the longest chunk
was 0.46 to 0.76 s; the lab runs above do not reach GC.
