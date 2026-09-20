# Staging idle CPU profile

The supplied idle CPU profiles identify admin database aggregation as the
dominant CPU consumer in both baseline captures. They confirm the leading candidate
from the [code review](idle-cpu-review.md) and [log analysis](staging-cpu-findings.md).
The user replaces `cpu-info/acu-idle.cpu.pprof` with each new capture; identify
captures by their timestamp and embedded build ID. Raw profiles remain untracked.

## Measured work

The profile started September 20, 2026 at 10:24:17 MDT and covers 120 seconds.
It contains **7.00 CPU seconds**, averaging **5.83% of one CPU**. Its executable
build ID is `70ff6ced14c0c3c01086134a53d08ab25af7a5ca`, matching the portable
branch build at commit `72e1298` (`1.0.1-3-g72e1298`). This build already includes
the first coverage-query optimization, daily GC default and profiling support.

| Work | Sampled CPU seconds | Share of total CPU |
| --- | ---: | ---: |
| Repository coverage (`GetRepoCoverage`) | 5.06 | 72.29% |
| Cache summary (`GetCacheSummaryByHostArch`) | 1.70 | 24.29% |
| Other admin database statistics | 0.09 | 1.29% |
| Admin pool filesystem walk | 0.12 | 1.71% |
| Remaining sampled work | 0.03 | 0.43% |

Percentages are rounded. The synchronous admin refresher accounts for 6.85 CPU
seconds (97.86%); the asynchronous pool walk is additional. The two expensive
aggregates alone account for **96.57%**. SQLite execution, sorting and associated
system calls dominate their stacks. They run without dashboard visitors or
metrics scrapes.

No cache-GC, Go runtime-GC or freshness/adoption application stack appears in
the sampled work. That does not establish zero cost for those activities in
other windows. This is a two-minute idle sample, not the older process's
13.79% lifetime CPU measurement; the two figures are not a before/after pair.

Reproduce the attribution with the measured executable:

```sh
go tool pprof -top -cum ./apt-cacher-ultra cpu-info/acu-idle.cpu.pprof
go tool pprof -list 'GetRepoCoverage|GetCacheSummaryByHostArch' \
  ./apt-cacher-ultra cpu-info/acu-idle.cpu.pprof
```

Keep the matching executable before replacing it with another build. On a Go
installation that omits the `pprof` executable but includes its source,
`go build -o /tmp/acu-pprof cmd/pprof` produces an equivalent analysis tool.

## Second capture: still the earlier executable

The replacement profile starts at **10:54:51 MDT** on September 20 and contains
**8.47 CPU seconds over 120.01 seconds**, approximately **7.06% of one CPU**.
It has the same embedded executable build ID as the first capture:
`70ff6ced14c0c3c01086134a53d08ab25af7a5ca`, matching `1.0.1-3-g72e1298`.

Coverage accounts for 6.09 CPU seconds (71.90%) and cache summaries for 2.13
(25.15%): together **97.05%**. Other synchronous admin statistics account for
0.07 seconds, and the pool walk for 0.12 seconds. Go GC has one 0.01-second
sample; no cache-cleanup or freshness/adoption application stack appears.

This strengthens the attribution to repeated admin aggregation, but **does not
measure the reuse fix** committed as `7cded10`. The locally rebuilt executable
containing that fix reports `1.0.1-4-g7cded10`, with ELF build ID
`e419561b4c7bf6816cc1a2890fe73ea6ed2ccb29`. The second profile's raw mapping
metadata was checked against both executables, independently of symbol lookup.
Its higher CPU total cannot be interpreted as a regression in the new code.

Before the next capture, compare the running and installed executable versions
on the staging host:

```sh
acu_pid=$(systemctl show -p MainPID --value apt-cacher-ultra)
readlink "/proc/$acu_pid/exe"
"/proc/$acu_pid/exe" -version
/usr/sbin/apt-cacher-ultra -version
```

The running process must contain `7cded10` or a later descendant. A replacement
installed file does not establish which executable is running; restart after
installing the new binary and verify the process version before profiling.
The profile alone does not identify why the earlier executable was captured
or which version is running now.

## Implemented response

The admin refresher now reuses each successful coverage and cache-summary result
while the database is unchanged. It still checks at `admin.gauge_refresh`
(default 30 seconds after the previous pass completes), and computes initial
results before serving the admin listener. No configuration change is required.

Change detection uses SQLite's `PRAGMA main.data_version` on one dedicated,
lazy connection. This detects commits by the application writer and other
connections, including external tools. The connection performs no writes and
holds no transaction between polls. Version comparisons must use the same
connection; a replacement gets a new identity so it cannot accidentally match
an old token. These are SQLite's documented
[data-version semantics](https://www.sqlite.org/pragma.html#pragma_data_version).

For each aggregate, the refresher compares revisions before and after a
successful query. An overlapping commit prevents reuse on the next pass, while
the successfully computed result is still published. Query or observation
failures also prevent reuse and retry on the next pass. Failed stages retain
their previous displayed values, as before. Each stage has its own validity
state, so a failure in one does not force successful unchanged stages to repeat.

Any committed database change conservatively invalidates both aggregates.
This covers adoption, reconciliation and repair within the current snapshot,
URL/blob updates, and deletions. It deliberately does not rely on snapshot IDs
alone. Frequent writes can still cause frequent recomputation; this change
targets redundant work during quiet periods.

Cache/suite counts, process metrics, host concurrency and filesystem walks keep
their existing cadence. Pool files can change independently of database commits.
Serving, offline availability, adoption, hash/signature checks and integrity
validation are unaffected. There is no schema migration or writer-path change.

## Observability and verification

`acu_admin_refresh_reused_total{stage="repo_coverage"}` and the corresponding
`cache_summary` series count avoided recomputations. Reuse does not add an
artificial zero-duration query sample or advance that stage's
`acu_admin_refresh_last_success_unixtime`. A computation timestamp can therefore
age while the result remains current. The existing duration/failure/success
metrics gain a `database_revision` stage for the cheap change checks.

Regression tests cover idle reuse, updates and repairs, errors, writes crossing
a refresh, independent filesystem updates, connection replacement, external
commits and shutdown. A WAL checkpoint test checks that observation does not
leave a read transaction open. The existing aggregate benchmark also measures
revision polling against its million-row fixture:

| Operation | Local elapsed time per operation |
| --- | ---: |
| Repository coverage computation | 202.9 ms |
| Cache summary computation | 143.8 ms |
| Database revision check | 6.0 µs |

These are three measured iterations on Go 1.26.5, Linux amd64, Intel i7-10750H.
They measure individual helpers, excluding admin metrics and scheduling overhead.
An unchanged refresh needs two revision checks and avoids both computations;
an actual computation also checks its revision afterward. These local timings
are not a prediction of CPU consumption on the staging server.

```sh
go test ./internal/cache -run '^$' -bench '^BenchmarkAdminCacheSummaries$' -benchtime=3x -count=1
```

The sampled 96.57% identifies work eligible for avoidance when data stays
unchanged; it is **not a measured reduction from the new build**. Collect another
120-second idle profile and before/after `/metrics` snapshots using the
[profiling guide](idle-cpu-review.md#optional-profiles), keeping configuration and
workload comparable and excluding startup. Reuse counters should rise while
aggregate computation counts remain steady between writes. Compare unprofiled
process CPU too, since sampling adds overhead. A separate adoption-window
profile can guide the prefetch investigation without weakening the offline
working set.

Validation passed: `go test ./...`, `go test -race -timeout 5m ./...`,
`go vet ./...`, and `golangci-lint run ./cmd/... ./internal/...`. Independent
ultra-effort reviews covered the profile, plan, implementation and tests. Docker
end-to-end and package-install suites were not rerun for this change. The raw
profile, logs and supplied configuration remain untracked and unmodified.
