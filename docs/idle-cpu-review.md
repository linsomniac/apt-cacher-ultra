# Idle CPU investigation

Review prompted by [issue #4](https://github.com/linsomniac/apt-cacher-ultra/issues/4).
The issue reports CPU use both with and without network activity. Code review
identifies substantial work that runs without proxy requests; it does not by
itself establish which work dominates an affected deployment.

The subsequent [staging log analysis](staging-cpu-findings.md) finds periodic GC occupies
only about 0.1% of the captured time and prioritizes admin profiling and verified
package-prefetch reuse for further investigation.

The subsequent [idle CPU profile](idle-cpu-profile.md) attributes 96.57% of its
sampled CPU to repository coverage and cache-summary queries. These aggregates
now reuse successful results while SQLite reports no committed database change.
The verified capture after this fix shows **0.15 CPU-seconds over two minutes**,
versus 7.00–8.47 seconds in the baselines: approximately **98% less sampled idle
CPU**, averaging 0.125% of one CPU. See the profile report for measurement limits.

Here, cache GC means deleting obsolete cache records and files. Go's runtime
garbage collector is a different subsystem. No production path explicitly calls
`runtime.GC`; changing `GOGC` is not part of this fix.

## Findings and action

| Candidate | Evidence | Action |
| --- | --- | --- |
| Admin repository aggregates | The background refresher checks statistics every `admin.gauge_refresh` (default 30s), even with no admin visitors. Coverage and summary repeatedly traversed `package_hash`, potentially millions of rows; together they account for 96.57% of the supplied idle CPU profile. | Combine coverage queries into one current-snapshot package pass; reuse coverage and summary while SQLite reports no database changes. Measure computations, failures and reuse separately. |
| Admin filesystem walk | Every refresh also traverses and stats files under `pool/`. A guard prevents simultaneous walks, but a large idle cache still incurs repeated filesystem work. | Measure the walk separately. Existing `admin.gauge_refresh` allows observation-only tuning; incremental disk accounting remains a profile-driven follow-up. |
| Cache GC | Formerly hourly. URL retention revisits old or never-requested rows even when all are protected. Per-row reachability and version checks can consume CPU while deleting nothing. | Default to 24h, retain explicit configured intervals and startup cleanup, report scanned/stamped/cleared rows and per-pass duration. |
| Maintenance overrun | Fixed tickers can leave an immediately due tick after a slow run. GC's budget is checked between batches; a large SQL batch can exceed it. | GC and admin refresh wait a full configured interval after completion. Resume unfinished GC stages before restarting earlier stages, preventing later passes from being repeatedly deferred. |
| Freshness/adoption | Default refresh is 15m, with a scheduler check every 3m45s. Repository updates can trigger downloads, signature verification, decompression, package parsing, database writes and prefetch without client requests. | Preserve behavior: this work supports readiness and offline service. Use profiles and existing adoption logs to distinguish it from housekeeping. |
| At-rest integrity | Default daily scan hashes blobs pinned by current snapshots with four workers. Enumeration also visits declared packages that were never downloaded, allocating a deduplication map and attempting to open their pool files. It can produce CPU and disk activity without network traffic. An overlong scan can run back-to-back under its existing ticker. | Preserve integrity validation. Correlate `at_rest_scan_started` / `at_rest_scan_finished` with process measurements before changing implementation. |

The writer goroutine, host semaphore and periodic certificate watchers inspected
do not have an obvious idle busy loop. Go runtime GC can still appear in a CPU
profile because SQL and metadata processing allocate memory; that would be a
reason to reduce allocations in those paths, not to disable validation.

## Local benchmark

`BenchmarkAdminCacheSummaries` seeds ten snapshots with 100,000 package rows
each: one current catalog, nine retained catalogs, three architecture buckets,
and 1,000 cached URLs. Setup is excluded from timing. With Go 1.26.5 on Linux
amd64 (Intel i7-10750H), three measured iterations gave:

| Helper | Before | After |
| --- | ---: | ---: |
| `GetRepoCoverage` | 351.7 ms/op | 199.1 ms/op |
| `GetCacheSummaryByHostArch` | 146.0 ms/op | 149.8 ms/op |

Coverage elapsed time fell approximately 43%. Summary showed no improvement;
its attempted rewrite was reverted. Query-plan inspection identified the former
source-snapshot count scanning all retained `package_hash` rows; the combined
query instead seeks current catalogs through the existing index. These results
measure this synthetic workload's query time, not a 43% reduction in daemon CPU.

Reproduce with:

```sh
go test ./internal/cache -run '^$' -bench '^BenchmarkAdminCacheSummaries$' -benchtime=3x -count=1
```

The baseline is commit `4595944b758ac1cbf79d1fee24365c8466e2663d` with the new
benchmark file copied into it, before the query changes. Run comparisons on the
same machine and storage, with identical fixture and settings.

## Reliability and configuration

Serving, signature/hash verification, upstream retry behavior, freshness,
prefetch, at-rest validation, retention eligibility, live-adoption heartbeats and
blob grace are preserved. There is no database schema migration.

Daily GC means obsolete data stays on disk longer. The existing five-minute
budget is a **between-batch budget**, not a hard interrupt of a running SQL
transaction. A large backlog can take multiple scheduled runs to drain, and
URL hold/grace transitions can add further intervals before physical deletion.
Startup still scans for pool orphans and performs a GC pass. Monitor free space,
GC deadline counts and actual reclaim progress; a smaller explicit interval
remains available when disk pressure warrants it.

Existing configuration files that explicitly set `interval = "1h"` keep that
value. To opt into daily collection, edit the existing section and restart:

```toml
[gc]
interval = "24h"
```

For an observation-only comparison on an existing release, change the existing
admin section's `gauge_refresh` from `"30s"` to `"5m"` and compare **steady-state**
idle CPU after startup work has completed. It delays metric/cache-summary
updates; it does not change proxy serving, adoption or integrity validation.
The configured range is greater than zero and at most one hour. With completion
based scheduling, the effective refresh period includes the time spent working.
Keep this setting identical across builds when measuring the SQL improvements.

## Evidence from a running process

Collect the version, relevant configuration with secrets removed, cache size,
whether the dashboard is open or metrics are scraped, and a quiet ten-minute
window. For a systemd installation:

```sh
systemctl show apt-cacher-ultra -p MainPID -p ExecStart -p CPUUsageNSec -p ActiveEnterTimestamp
journalctl -u apt-cacher-ultra --since '10 minutes ago' --no-pager > acu-idle.log
curl --fail --silent --show-error http://127.0.0.1:6789/metrics > acu-metrics-before.txt
# Repeat this after the observation window:
curl --fail --silent --show-error http://127.0.0.1:6789/metrics > acu-metrics-after.txt
```

Use the configured admin address and authentication where applicable. Compare
the change in `process_cpu_seconds_total` with elapsed wall seconds (multiply
by 100 for percentage of one CPU). The counter is sampled by the admin refresher,
so allow a refresh after the window; `pidstat -p PID 1 600`, when installed, gives
an independent process sample without relying on that cadence. Avoid repeated
status-page polling during the quiet sample because it executes live queries.

Existing useful metrics include `acu_gc_run_duration_seconds`,
`acu_gc_deadline_reached_total`, `acu_gc_url_path_rows_reaped_total`,
`acu_blobs_actually_reapable`, and `acu_pool_disk_bytes`. Wall-clock maintenance
duration includes I/O and waiting; it is **not** CPU time and must not be summed
as though it were.

New admin metrics are always available with the admin listener enabled:

| Metric | Meaning |
| --- | --- |
| `acu_admin_refresh_duration_seconds{stage=...}` | Duration histogram, including failed attempts. `_count` shows how often work ran. |
| `acu_admin_refresh_failures_total{stage=...}` | Failed attempts, including cancellation. Existing gauge values survive errors. |
| `acu_admin_refresh_last_success_unixtime{stage=...}` | Last successful computation; can age while an unchanged aggregate remains valid. |
| `acu_admin_refresh_reused_total{stage=...}` | Coverage/summary recomputations avoided because their database inputs are unchanged. |

Stages are the fixed labels `cache_stats`, `suite_stats`, `repo_coverage`,
`cache_summary`, `database_revision`, and `pool_walk`. Debug logging adds `admin_refresh_complete`
with `stage`, `duration_ms`, and `success`. Routine successful refreshes do not
add info-level log traffic. Each stage adds only a few metric updates and clock
reads, without per-row instrumentation.

GC also exports `acu_gc_pass_duration_seconds{phase=...,pass=...}` for
`url_path`, `snapshot`, and `blob`, plus
`acu_gc_url_path_rows_scanned_total`, `acu_gc_url_path_rows_stamped_total`, and
`acu_gc_url_path_rows_cleared_total`. The existing `gc_run_complete` event now
includes these three row counts and `url_path_duration_ms`,
`snapshot_duration_ms`, and `blob_duration_ms`. A high scanned count with few
deletions makes repeated retention checks visible.

## Optional profiles

On a build containing this change, edit the existing admin section and restart:

```toml
[admin]
pprof_enabled = true
```

The option defaults to false. It exposes only GET CPU, heap and goroutine
profiles on the existing admin listener, through its existing authentication
and request logging. Use the loopback listener or an SSH tunnel; profiles can
reveal internal function names and runtime details. No profiling route is added
to the proxy listener. CPU sampling runs only for the requested capture:

```sh
curl --fail --silent --show-error --max-time 320 \
  'http://127.0.0.1:6789/debug/pprof/profile?seconds=120' -o acu-idle.cpu.pprof
curl --fail --silent --show-error \
  http://127.0.0.1:6789/debug/pprof/heap -o acu-idle.heap.pprof
curl --fail --silent --show-error \
  http://127.0.0.1:6789/debug/pprof/goroutine -o acu-idle.goroutine.pprof
go tool pprof -top ./apt-cacher-ultra acu-idle.cpu.pprof
```

Use the executable from the measured build for local symbolization. CPU capture
defaults to 30s and accepts whole seconds from 1 through 300. Only one CPU
capture can run at a time; a competing capture gets HTTP 409. Disconnecting the
client or shutting down the admin server stops an active capture. Heap capture
does not force Go garbage collection. There is no pprof index, command-line dump,
trace endpoint or arbitrary profile-name routing.

Capture one idle window and, separately, a window containing a GC or adoption
run. The profile's cumulative call stacks can distinguish admin SQL work,
retention evaluation, hashing/decompression and runtime garbage collection.
Disable profiling again when done. Sampling itself adds overhead, so compare
unprofiled process CPU before/after as well.

## Follow-up decisions

The supplied profiles led to aggregate reuse until a committed database change,
with failed-read retries and detection of mutations during a refresh, and then
confirmed approximately 98% lower sampled idle CPU. The remaining work is small
in absolute terms, so this evidence does not warrant further idle-path changes.
Separate adoption profiles can evaluate verified prefetch reuse; filesystem
accounting and GC SQL remain candidates if future measurements justify them.
Preserve reachability, hash, signature and offline-serving guarantees throughout.

## Validation

Three independent ultra-effort reviews covered the initial plan, background work,
GC, profiling lifecycle and final changes. Local checks passed:

- `go test ./...`
- `go test -race -timeout 5m ./...`
- `go vet ./...`
- `golangci-lint run ./cmd/... ./internal/...`

Full-workspace lint also visits an existing untracked `debug/dbq` utility, whose
two unchecked `Close` calls remain outside this change. Docker end-to-end and
package-install suites were not run. The user subsequently deployed the branch
build to staging and supplied the [before/after profiles](idle-cpu-profile.md),
which establish the improvement in those idle windows.
