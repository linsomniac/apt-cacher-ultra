# Staging CPU evidence

Analysis of the supplied `cpu-info/acu.log` and `cpu-info/config.toml` following
the [initial review](idle-cpu-review.md). Raw logs and configuration remain
untracked. Times below use the log's server timestamps; its timezone is not
specified in the file.

Follow-up: the [supplied idle CPU profile](idle-cpu-profile.md) confirms admin
aggregation dominates that later two-minute sample and documents the resulting
optimization. A verified capture after deploying it shows approximately 98%
less sampled idle CPU. The log-based findings below describe the older build's
workload; its lifetime CPU totals are not directly comparable to idle samples.

## Capture and configuration

The capture has 290,122 lines, spanning September 6 at 03:15:46 through September
19 at 15:36:29: approximately 13.51 days. There is one restart during the capture.
The startup record at log line 23353 identifies build
`2026.07.22T140916.a0254e3` and records these effective settings:

| Setting | Value |
| --- | --- |
| GC | Enabled, every hour; five-minute between-batch budget |
| GC batch sizes | 100 URL/blob rows, 10 snapshots |
| Admin refresh | Enabled, every 30 seconds |
| Freshness refresh | Every 15 minutes; at most two concurrent adoptions |
| At-rest integrity | Every 24 hours, four workers |
| Hot-package window | **300 days**, explicitly configured |
| Hot-prefetch budget | **25 minutes**, explicitly configured |

The configuration omits `[gc]` and leaves `admin.gauge_refresh` unset. The
branch's new daily GC default will therefore apply to this configuration after
upgrading; admin refresh will still default to 30 seconds.

Systemd's shutdown summary at line 23349 reports 7h49m31.803s CPU over
2d8h44m53.808s elapsed: approximately **13.79% of one CPU** averaged across that
process's lifetime, with a 5.1G memory peak. That lifetime begins before the
capture, so these totals cannot be directly compared with all maintenance
durations below. It is not an idle-only CPU measurement.

## GC is unlikely to dominate sustained CPU here

| Observed work | Count | Combined wall time | Median | Maximum |
| --- | ---: | ---: | ---: | ---: |
| Periodic GC | 323 | 1,214.610s (20.24 minutes) | 1.746s | 27.620s |
| Startup GC, including pool scan | 1 | 165.697s | — | — |
| At-rest integrity scans | 13 | 852.246s (14.20 minutes) | 58.159s | 109.497s |

Periodic GC occupied approximately **0.104% of captured elapsed time**. This is
wall time, including I/O and writer-queue waits, not measured CPU consumption.
Nevertheless, it strongly weakens the hypothesis that hourly GC explains
sustained CPU use of several percent on this installation.

Every GC completion reports `deadline_reached=false`, with no GC deadline or
failure events. There is no observed catch-up storm or cleanup starvation.
The branch's scheduling fixes remain safeguards against those cases, rather
than demonstrated fixes for this particular capture.

116 periodic runs reclaimed nothing; those runs total 126.804 seconds and each
took at most 2.193 seconds. Runs deleting displaced snapshots account for 88.6%
of periodic GC wall time. Daily GC reduces repeated checks, but much of the
deletion work must still happen in larger batches. It should not be presented
as a 24-fold reduction in GC CPU.

Useful source lines: 31 (ordinary empty GC), 4497 (longest periodic GC, deleting
nine displaced snapshots), 23357 (startup GC), and 183179 (longest empty GC).
Integrity scans report no mismatches or I/O errors. Their roughly 118,000–122,000
`blob_count` values are candidate counts, including declarations for uncached
packages; they do not establish physical cache size or files actually hashed.

## Stronger candidates

**Admin aggregation remains the leading candidate for CPU during genuinely
quiet periods.** The background refresher runs regardless of dashboard visitors.
Three queries hit their ten-second timeout shortly after restart:

| Log line | Timestamp | Query |
| --- | --- | --- |
| 23362 | September 7, 20:10:23 | Repository coverage: architecture enumeration |
| 23363 | September 7, 20:10:33 | Cache summary: package counts |
| 23364 | September 7, 20:11:13 | Repository coverage: source-snapshot count |

These are startup-period observations, not proof that steady-state queries
always take ten seconds. Successful refreshes have no timings in this build's
info logs. There are no `admin_request` events, but that does not remove the
unconditional background work. The new per-stage metrics and optional profiler
are needed to attribute the remaining cost.

**No client requests does not imply no adoption work.** There are 87 gaps of at
least ten minutes between logged requests; 54 include successful adoptions.
September 17 from 04:15:27 through 22:50:38 has no request events, but contains:

- 53 successful adoptions and 8,436 successful package prefetches.
- 1,214,627 package-hash rows processed across those adoptions.
- 18 GC runs totaling 128.216 seconds, and one 109.497-second integrity scan.

Request boundaries are log lines 235449 and 254056. These are cumulative work
counts, not distinct packages or current database row counts.

Across the entire capture, 630 successful adoptions produced 11,086,195
package-hash rows cumulatively. There were 109,987 selected hot-package prefetches:
109,985 succeeded, two failed, none reported hash mismatches, and none were left
unattempted. Median time from prefetch start to its post-commit completion log
was 24 seconds; the longest was 1,193 seconds (lines 107778–107959). Durations
include network/database waits and overlap other work; they are not CPU seconds.

Preserve the intentional 300-day hot window and its offline working set.
Code review found a more appropriate optimization candidate: `fetchHotDeb` in
`internal/freshness/hot_prefetch.go` always fetches upstream and writes/hashes a
temporary blob, although selected versions can already be cached. Metadata
adoption already has hash-verified pool reuse in `adoptMember`. Equivalent
package reuse could avoid redundant downloads and writes while retaining
validation and the same warm set. This capture cannot determine the redundant
fraction. Any implementation must preserve cancellation, blob heartbeat/grace
protection and atomic adoption, and report reused versus downloaded work.

The 163,504 skipped-member messages comprise 132,210 intentional architecture
exclusions and 31,294 upstream 404s, with no integrity skips. They identify bursty
logging and adoption work, not a continuous busy retry loop. A separate cluster
of 34 failed attempts against an already-adopted snapshot on September 11 totals
approximately four seconds from matched trigger to failure and does not plausibly
explain sustained CPU throughout the capture.

## Follow-up measurement

The final hour of this log contains 103 freshness checks, one 5.354-second GC run, and no
logged requests or adoptions. A similar window is useful: high CPU while also
confirming no adoption remains in progress would further implicate other
background work.

Preferred measurement: on the branch build, enable the optional admin profiler
and collect a 120-second CPU profile in such a quiet window, with the matching
log interval and `/metrics` readings. Use the commands in the
[profiling guide](idle-cpu-review.md#optional-profiles). Existing admin access
controls apply. Compare unprofiled process CPU too; profiling adds overhead.

Without upgrading, an alternative comparison is to set `gauge_refresh = "5m"`
in the existing `[admin]` section. Record steady-state process CPU before and
after the restart, excluding startup cleanup/adoption bursts and using comparable
workloads. This reduces observation frequency tenfold and delays metrics; it does
not change serving, freshness, prefetch or integrity validation. A large CPU
reduction would implicate the refresher without identifying which of its SQL
queries or pool walks dominates.

No daemon or supplied configuration was changed during this analysis. The
evidence supports prioritizing admin profiling and investigating verified
prefetch reuse, while keeping daily GC as a modest housekeeping improvement.
