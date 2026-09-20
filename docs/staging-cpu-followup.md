# CPU after startup: September 20 staging log

The replacement `cpu-info/acu.log` shows substantial automatic repository work
after startup on the build containing admin aggregate reuse. Startup cleanup is
small in this run. The log establishes what ran, but does not divide the user's
reported **2m50s of CPU** between adoption, prefetch and admin calculations.

## Capture

There are 851 lines, including 850 JSON application events, spanning September
20 at **12:30:44–13:45:46** (75 minutes, 2 seconds). Times here use the log's
clock; the journal prefix does not include a timezone. The process PID is
2435081 and the startup version is **`1.0.1-6-g9253467`**, which includes the
reuse implementation from `7cded10` plus documentation changes.

The user reports no open admin dashboard, no metrics scraping, and clients
unlikely to be using the proxy. The capture contains no client-request or
admin-request events. It contains one startup and no restart. The exact time
of the 170-second CPU reading was not recorded, so do not assign it an exact
percentage using the log's end timestamp.

Effective settings include 30-second admin refresh, 15-minute per-suite freshness
refresh, two concurrent adoptions, the intentional 300-day hot window and
25-minute prefetch budget. GC and at-rest integrity intervals are both 24 hours.
The optional CPU profiler is disabled and logging is at info level, so successful
admin recalculations are not individually logged.

## Observed work

| Work | Observed result |
| --- | --- |
| Startup GC, including pool scan | One run, **1.007 seconds wall time**; 986 URL rows scanned, one displaced snapshot and three blobs reaped |
| Periodic GC / at-rest integrity | No runs in this capture |
| Freshness checks | **108 checks across 25 suites**, in **20 batches** about 3m45s apart |
| Unchanged repositories | 96 HTTP 304 results plus seven HTTP 200 responses with unchanged metadata contents |
| Changed repositories | Five changes, each followed by a successful adoption |
| Adopted package-hash rows | **103,784**, cumulative across the five snapshots |
| Package prefetch | **616 fetched**, zero failed, mismatched or unattempted |
| Failed admin queries | None logged; successful query counts require metrics |

The startup GC's URL/snapshot/blob passes report 684/235/9 ms respectively.
The remaining approximately 79 ms includes the pool scan and other overhead.
These are wall times, not CPU measurements, but they strongly deprioritize
startup cleanup as an explanation for this run's CPU total.

## Adoption and prefetch timeline

| Repository / suite | Change observed | Adoption completed | Prefetched packages | Prefetch start–completion window |
| --- | --- | --- | ---: | --- |
| Microsoft Ubuntu 24.04 / noble | 12:45:47 | 12:45:49 | 6 | 12:45:48–12:45:49 |
| Microsoft Ubuntu 22.04 / jammy | 12:45:47 | 12:45:52 | 5 | 12:45:50–12:45:52 |
| Ubuntu jammy-updates | 12:45:46 | 12:47:23 | **197** | 12:46:32–12:47:23 |
| Microsoft Ubuntu 24.04 / noble | 13:04:32 | 13:04:34 | 6 | 13:04:32–13:04:34 |
| Ubuntu noble-updates | 13:38:16 | 13:40:21 | **402** | 13:38:53–13:40:21 |

The two Ubuntu updates account for 599 of 616 prefetched packages. Their update
windows include downloading and verifying metadata, parsing package catalogs,
database work and prefetch. Durations include network waits and overlap other
work; adding these seconds does not produce a CPU total.

The two Microsoft noble adoptions each report 4,934 package-hash rows and six
prefetched packages. That is not proof that the six package hashes were
identical: per-package successful hashes and byte counts are absent from these
logs. The InRelease hashes changed between the two successful adoptions, so
this is not an immediate retry of the same snapshot.

Of 703 skipped-member warnings, 573 are architecture exclusions and 130 are
upstream 404s (114 `4xx`, 16 `4xx_index_target`). No integrity skips or adoption
failures are reported. Warning volume alone does not establish a CPU bottleneck.

## Updated priorities

1. **Reduce recurring admin recalculation cost.** All 20 freshness batches can
   invalidate the summaries through timestamp writes, even when the upstream
   returns 304. Active adoptions add blob writes and heartbeats. The earlier
   two-minute quiet profile establishes the cost between writes, not the cost
   over this full scheduler/adoption cycle. The later metrics snapshot below
   confirms 87 computations of each aggregate and makes these queries a
   concrete optimization target.
2. **Investigate verified package reuse during adoption.** Prefetch activity is
   now directly observed in the current run. Avoiding downloads and writes for
   already-present, hash-verified content remains promising, while preserving
   the same hot set. The redundant fraction and its CPU cost are still unknown.
   The corruption, cancellation, atomic publication and GC protections described
   in the [candidate review](idle-cpu-profile.md#follow-up-79-cpu-seconds-over-the-first-30-minutes)
   remain prerequisites.
3. **Deprioritize startup batching and quiet filesystem walks for this run.**
   Their measured costs are too small to be the first target here. Preserve
   freshness and integrity checks rather than reducing their frequency.

A smaller admin SQL experiment tested sharing the package catalog scan used
by coverage and summary counts, with the results below. Narrower invalidation is
another option: it must ignore irrelevant timestamp changes while preserving
the timestamps that freshness scheduling needs and detecting all writes that
actually affect the aggregates.

## Shared-scan benchmark prototype

The benchmark-only implementation in
[`internal/cache/combined_admin_benchmark_test.go`](../internal/cache/combined_admin_benchmark_test.go)
feeds both admin results from one package aggregate query. The pdiff-member
and cached-blob queries are unchanged. Complete results must match the current
production helpers before timing begins; fixture creation is excluded.

At commit `aa52082`, with 100,000 current and 900,000 retained package rows,
three runs of three iterations each gave these median combined query times on the development
machine (Go 1.26.5, Linux amd64, Intel i7-10750H):

| Implementation | Median wall time per pair |
| --- | ---: |
| Current separate helpers | 356.25 ms |
| Shared package scan | 259.27 ms |

That is about **27% less query wall time locally**, not a measurement of CPU
savings on staging or across the whole daemon. A second fixture also passed
exact-result comparison with multiple hosts and suites, shared cached blobs,
source and pdiff rows, empty architectures, case-sensitive path classification
and a current snapshot containing only metadata members.

Run the repeated timing comparison with:

```sh
go test ./internal/cache -run '^$' \
  -bench '^BenchmarkCombinedAdminExperiment/ExistingFixture/' \
  -benchtime=3x -count=3 -benchmem
```

Run both fixtures by omitting `/ExistingFixture/` from the benchmark pattern.
The measurements above predate the coverage-only production optimization below;
the benchmark compares against the current production helpers, so running it on
a newer commit changes its baseline.
No production query or refresh behavior changes in this experiment. Integrating
it must preserve the two stages' independent failure and retry handling and
define timeout and publication behavior; the prototype does not establish those
properties. The metrics snapshot below establishes that expensive queries
continue to run regularly on the affected process.

## Later cumulative metrics

The user supplied a full metrics response from the same reported build,
`1.0.1-6-g9253467`. Its process start time is 1789929045 and the most recent
synchronous admin refresh timestamp is 1789945759, approximately **4h38m34s**
later. The response reports process CPU of **405.66 seconds**, averaging about
**2.43% of one CPU core**. This is a longer observation window than the log
above, and its totals must not be attributed to the earlier 75-minute capture.

| Admin stage | Computations | Reuses | Total wall time | Mean wall time per computation |
| --- | ---: | ---: | ---: | ---: |
| Repository coverage | 87 | 464 | 150.322 s | 1.728 s |
| Cache summary | 87 | 464 | 49.784 s | 0.572 s |
| Pool filesystem walk | 551 | — | 18.338 s | 33.3 ms |
| Basic cache statistics | 551 | — | 7.723 s | 14.0 ms |
| Suite statistics | 551 | — | 0.312 s | 0.566 ms |
| Database revision check | 1,276 | — | 0.116 s | 0.091 ms |

All reported refresh failure counters are zero. Reuse avoided **84.2%** of
coverage and summary calculations (464 of 551 opportunities for each). The
remaining 87 pairs consumed **200.106 seconds wall time**, averaging 2.300
seconds per pair. That is about one computed pair every 3m12s over the capture,
consistent with freshness batches plus adoption writes, but the counters do
not identify each invalidating transaction.

These are not CPU timers: **200 seconds of query wall time is not proof that
these queries consumed half of the 405.66 CPU seconds**. Earlier CPU profiles
establish that these scans can be CPU-heavy. Together, the profile and current
metrics justify prioritizing cheaper aggregate queries before investigating
the small revision-observer overhead. Coverage accounts for about 75% of the
two queries' measured wall time, making a coverage-only SQL optimization a
useful alternative to coupling both stages in the shared-scan prototype.

The longer capture also reports seven successful adoptions, 404 freshness
checks (376 HTTP 304, 21 unchanged-content HTTP 200, seven changed), one startup
GC and no recorded periodic GC or at-rest integrity scans. Adoption durations
sum to 434.072 seconds wall time, including network and disk waits and possible
overlap. `acu_hot_prefetch_total` counts seven completed prefetch operations,
not individual package downloads; it cannot update the earlier log's package
count. There are no proxy-request or dashboard-render samples and only two
recorded metrics scrapes, reinforcing that this work runs without those clients.

## Implemented coverage-only optimization

`GetRepoCoverage` now groups package rows by snapshot and architecture, counting
pdiff rows with a conditional sum. It derives the remaining source/binary counts
from each group's architecture. The previous query also sorted/grouped by a
computed kind string. Removing that grouping key reduces work while keeping the
same selected rows, case-sensitive path classification and complete output.

The two coverage queries still share one read-only transaction. The separate
cache-summary stage, per-stage deadlines, error handling, revision checks and
refresh cadence are preserved. There is no schema change or additional write
overhead. Request serving, adoption, verification, GC and offline availability
are unaffected by this query change.

The durable `BenchmarkCoverageGrouping` keeps the old query as a benchmark-only
baseline and compares it with the actual production method. Exact outputs must
match before timing starts. Three iterations across three repeats, on the same
development machine as above, produced these medians:

| Fixture | Previous query | Production query | Reduction in query wall time |
| --- | ---: | ---: | ---: |
| One host / suite | 197.66 ms | 170.38 ms | 13.8% |
| Multiple hosts / suites | 206.95 ms | 181.16 ms | 12.5% |

Both fixtures contain 100,000 current and 900,000 retained package rows.
An earlier prototype run measured 16–18% improvement; the actual-production run
had more timing variability, so **about 12–14%** is the appropriate result to
report here. These are local query wall times, not staging CPU measurements or
daemon-wide savings. Reproduce with:

```sh
go test ./internal/cache -run '^$' \
  -bench '^BenchmarkCoverageGrouping/' -benchtime=3x -count=3 -benchmem
```

The existing aggregate regression fixture now includes a snapshot containing
only source/binary pdiff rows. It checks that those architectures and the source
snapshot remain represented even when their ordinary source/binary counts are
zero, and that a path matching both pdiff patterns counts once. Existing cases
cover empty architectures, mixed kinds, current/displaced snapshots, origin
filtering, shared blobs and member-only coverage.

Validation passed: `go test ./...`, `golangci-lint run ./cmd/... ./internal/...`,
and the repeated production-versus-baseline benchmarks above. Ultra-effort
reviewers checked the metrics interpretation, implementation plan and final
query semantics.

## Collecting another snapshot

No additional snapshot is needed to establish that admin computations recur.
For a future before/after comparison, this filter uses separate, short patterns
so copying it does not require keeping a long parenthesized expression intact:

```sh
curl -fsS http://127.0.0.1:6789/metrics | \
  grep -e '^process_cpu_seconds_total ' -e '^process_start_time_seconds ' \
       -e '^acu_admin_refresh_' | \
  grep -v '_bucket'
```

For each of `repo_coverage` and `cache_summary`, duration `_count` is the number
of actual attempts and `reused_total` is the number avoided. Duration `_sum` is
wall time, not CPU. Repeating the snapshot after five to ten minutes and keeping
the process identity provides a useful delta. An adoption-window CPU profile can
then separate hashing, SQL, decompression, network handling and heartbeat work
if needed. Enable the optional profiler for that later measurement; collect the
current process's counters before any restart resets them. A newline inserted
inside the earlier quoted extended expression makes grep parse separate,
unbalanced patterns. The resulting curl error 23 is a consequence of grep
closing the pipe, not an application metrics failure.

Two independent ultra-effort reviews checked the log totals and the resulting
plan. Raw logs and configuration remain untracked and unmodified.
