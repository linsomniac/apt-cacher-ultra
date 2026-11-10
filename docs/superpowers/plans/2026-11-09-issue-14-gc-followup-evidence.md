# Issue #14 GC follow-up: evidence

## Before (18f652e + harness)

Date: 2026-11-09. Code under test: product code of 18f652e (HEAD d0cb751 adds only docs) plus the two env-gated harness tests.
Machine: `Linux seansxps15 6.18.56 #1-NixOS SMP PREEMPT_DYNAMIC x86_64 GNU/Linux`; CPU Intel(R) Core(TM) i7-10750H @ 2.60GHz, 12 threads.

### Long-run harness

Command: `ACU_LONGRUN=1 go test ./internal/gc -run TestIssue14LongRun -v -timeout 30m` (PASS, 463.5s; defaults: 70000 packages, 3s tick, 4 suites)

```
seeded 40 snapshots x 70000 package_hash rows in 1m53.231s
calibration: one displaced snapshot (70000 package_hash rows) deleted in 1ms (reaped 1)
day 1 periodic (nextPass=2)  dur=5.13s   deadline=true  retired=0   displaced_reaped=23  blobs_reaped=0   bytes_reclaimed=0     KiB | blobs=162 (106 MiB) snapshots=56 package_hash=3860001
day 2 periodic (nextPass=0)  dur=10ms    deadline=false retired=0   displaced_reaped=0   blobs_reaped=51  bytes_reclaimed=52224 KiB | blobs=143 (71 MiB) snapshots=72 package_hash=4980001
day 3 periodic (nextPass=0)  dur=2.86s   deadline=true  retired=1   displaced_reaped=32  blobs_reaped=2   bytes_reclaimed=1024  KiB | blobs=173 (86 MiB) snapshots=87 package_hash=6070001
restart 1 startup            dur=24.66s  deadline=true  retired=0   displaced_reaped=10  blobs_reaped=0   bytes_reclaimed=0     KiB | blobs=173 (86 MiB) snapshots=77 package_hash=5370001
restart 2 startup            dur=12.13s  deadline=true  retired=0   displaced_reaped=6   blobs_reaped=0   bytes_reclaimed=0     KiB | blobs=173 (86 MiB) snapshots=71 package_hash=4950001
restart 3 startup            dur=2.91s   deadline=true  retired=0   displaced_reaped=0   blobs_reaped=32  bytes_reclaimed=16384 KiB | blobs=141 (70 MiB) snapshots=71 package_hash=4910001
restart 4 startup            dur=2.53s   deadline=true  retired=1   displaced_reaped=0   blobs_reaped=2   bytes_reclaimed=1024  KiB | blobs=139 (69 MiB) snapshots=70 package_hash=4880001
restart 5 startup            dur=3.2s    deadline=true  retired=0   displaced_reaped=0   blobs_reaped=0   bytes_reclaimed=0     KiB | blobs=139 (69 MiB) snapshots=70 package_hash=4840001
restart 6 startup            dur=2.68s   deadline=true  retired=1   displaced_reaped=0   blobs_reaped=2   bytes_reclaimed=1024  KiB | blobs=137 (68 MiB) snapshots=69 package_hash=4810001
restart 7 startup            dur=2.89s   deadline=true  retired=0   displaced_reaped=0   blobs_reaped=0   bytes_reclaimed=0     KiB | blobs=137 (68 MiB) snapshots=69 package_hash=4760001
restart 8 startup            dur=2.5s    deadline=true  retired=1   displaced_reaped=0   blobs_reaped=2   bytes_reclaimed=1024  KiB | blobs=135 (67 MiB) snapshots=68 package_hash=4730001
```

### package_hash benchmark (variant base)

Command: `ACU_BENCH=1 ACU_BENCH_K=1,4 ACU_BENCH_VARIANTS=base go test ./internal/cache -run TestPackageHashBench -v -timeout 60m` (PASS, 102.6s)

```
RESULT k=1  rows=840000   variant=base      seed=17s      insert70k=1.355s   delete70k_chunked=904ms    per10k=129ms   delete70k_whole=577ms    db=320MiB
RESULT k=4  rows=3360000  variant=base      seed=1m9s     insert70k=1.488s   delete70k_chunked=1.138s   per10k=163ms   delete70k_whole=846ms    db=1172MiB
```

## After (branch fix/issue-14-followup, HEAD 57b1aaf)

Date: 2026-11-09. Same machine and the same commands as "Before"; product code of 57b1aaf (Tasks 1-6) plus the two harness tests.

### Long-run harness

Command: `ACU_LONGRUN=1 go test ./internal/gc -run TestIssue14LongRun -v -timeout 30m` (PASS, 328.9s; defaults: 70000 packages, 3s tick, 4 suites)

```
seeded 40 snapshots x 70000 package_hash rows in 1m31.898s

calibration: one displaced snapshot (70000 package_hash rows) deleted in 1ms (reaped 1)

day 1 periodic (nextPass=0)  dur=2.73s   deadline=true  retired=0   displaced_reaped=23  blobs_reaped=51  bytes_reclaimed=52224 KiB | blobs=111 (55 MiB) snapshots=56 package_hash=3890001

day 2 periodic (nextPass=2)  dur=3s      deadline=true  retired=1   displaced_reaped=16  blobs_reaped=0   bytes_reclaimed=0     KiB | blobs=143 (71 MiB) snapshots=71 package_hash=4960001

day 3 periodic (nextPass=0)  dur=0s      deadline=false retired=0   displaced_reaped=0   blobs_reaped=2   bytes_reclaimed=1024  KiB | blobs=173 (86 MiB) snapshots=87 package_hash=6080001

restart 1 startup            dur=2.5s    deadline=true  retired=0   displaced_reaped=32  blobs_reaped=0   bytes_reclaimed=0     KiB | blobs=173 (86 MiB) snapshots=87 package_hash=6030001

restart 2 startup            dur=2.74s   deadline=true  retired=1   displaced_reaped=0   blobs_reaped=2   bytes_reclaimed=1024  KiB | blobs=171 (85 MiB) snapshots=86 package_hash=5990001

restart 3 startup            dur=2.73s   deadline=true  retired=1   displaced_reaped=0   blobs_reaped=2   bytes_reclaimed=1024  KiB | blobs=169 (84 MiB) snapshots=85 package_hash=5940001

restart 4 startup            dur=2.72s   deadline=true  retired=0   displaced_reaped=0   blobs_reaped=0   bytes_reclaimed=0     KiB | blobs=169 (84 MiB) snapshots=85 package_hash=5900001

restart 5 startup            dur=3.02s   deadline=true  retired=1   displaced_reaped=0   blobs_reaped=0   bytes_reclaimed=0     KiB | blobs=169 (84 MiB) snapshots=84 package_hash=5870001

restart 6 startup            dur=2.42s   deadline=true  retired=0   displaced_reaped=0   blobs_reaped=2   bytes_reclaimed=1024  KiB | blobs=167 (83 MiB) snapshots=84 package_hash=5820001

restart 7 startup            dur=2.53s   deadline=true  retired=1   displaced_reaped=0   blobs_reaped=2   bytes_reclaimed=1024  KiB | blobs=165 (82 MiB) snapshots=83 package_hash=5770001

restart 8 startup            dur=2.61s   deadline=true  retired=1   displaced_reaped=0   blobs_reaped=2   bytes_reclaimed=1024  KiB | blobs=163 (81 MiB) snapshots=82 package_hash=5730001

```

Compared with "Before" (same workload, same 3 s budget):

- Blobs reaped on day 1: 51 (was 0). The blob pass now runs on the first tick; before, the snapshot pass ate the budget and the first blob reap came on day 2.
- Tick and startup durations stay at the budget: the worst is 3.02 s (restart 5), against 5.13 s on day 1 and 24.66 s, 12.13 s at restarts 1 and 2 before. No run overran its deadline by more than a few percent. This is the start deadline (Tier 1a) working: a write the writer cannot start before the pass deadline is given up.
- Day 3 ran in 0 s with `deadline=false` (was 2.86 s): the rotation had reached the blob pass alone, and it reaped 2 blobs.
- **Not better, stated plainly:** at the end of this scaled run more is left than "Before": 82 snapshots (was 68), 163 blobs / 81 MiB (was 135 / 67 MiB) and 5.73M `package_hash` rows (was 4.73M). The table did not shrink: its rows stay at about 70,000 x snapshots, so sub-job D stripped little in this run. What the run shows, and what it does not: with the start deadline every run stops at its budget, so a startup pass no longer deletes 6 to 10 snapshots in one overrunning transaction (the 24.66 s and 12.13 s "Before" startups), and the backlog drains by C's chunks within each budget instead. The harness has no hourly retired runs and does not print `displaced_package_rows_deleted`. In this workload sub-job C always has retired snapshots left, so the pass reaches its deadline in C and D (which runs after C) gets little or no time. The strip's effect on the table (G1) is therefore **not demonstrated by this harness**; it is pinned by `TestPeriodicTick_StripsDisplacedPackageRowsOnARealCache` and the D tests. A real cache adopts far less often than this harness's 16 adoptions a "day" against a 3 s budget (a 5 min budget is 100x longer, while a 10,000-row chunk costs the same 120-210 ms).
- Each "day" and restart reports `retired` and `displaced_reaped` per run; both are small for the same reason (chunked deletes, a short budget).

### Long-run harness at a 30 s budget (supplementary)

Command (not in "Before"): `ACU_LONGRUN=1 ACU_REPRO_TICK_MS=30000 go test ./internal/gc -run TestIssue14LongRun -v -timeout 30m` (PASS, 545.3s). The budget is 10x longer, so a chunk is a tenth of the share it was.

```
seeded 40 snapshots x 70000 package_hash rows in 1m52.515s
calibration: one displaced snapshot (70000 package_hash rows) deleted in 1ms (reaped 1)
day 1 periodic (nextPass=0)  dur=24.89s  deadline=true  retired=2   displaced_reaped=23  blobs_reaped=55  bytes_reclaimed=54272 KiB | blobs=107 (53 MiB) snapshots=54 package_hash=3730001
day 2 periodic (nextPass=0)  dur=24.72s  deadline=true  retired=3   displaced_reaped=16  blobs_reaped=6   bytes_reclaimed=3072  KiB | blobs=133 (66 MiB) snapshots=67 package_hash=4620001
day 3 periodic (nextPass=0)  dur=26.57s  deadline=true  retired=3   displaced_reaped=16  blobs_reaped=6   bytes_reclaimed=3072  KiB | blobs=159 (79 MiB) snapshots=80 package_hash=5560001
restart 1 startup            dur=24.4s   deadline=true  retired=7   displaced_reaped=16  blobs_reaped=14  bytes_reclaimed=7168  KiB | blobs=145 (72 MiB) snapshots=73 package_hash=5060001
restart 2 startup            dur=24.6s   deadline=true  retired=4   displaced_reaped=0   blobs_reaped=8   bytes_reclaimed=4096  KiB | blobs=137 (68 MiB) snapshots=69 package_hash=4770001
restart 3 startup            dur=24.22s  deadline=true  retired=5   displaced_reaped=0   blobs_reaped=10  bytes_reclaimed=5120  KiB | blobs=127 (63 MiB) snapshots=64 package_hash=4460001
restart 4 startup            dur=24.22s  deadline=true  retired=6   displaced_reaped=0   blobs_reaped=12  bytes_reclaimed=6144  KiB | blobs=115 (57 MiB) snapshots=58 package_hash=4050001
restart 5 startup            dur=24.32s  deadline=true  retired=10  displaced_reaped=0   blobs_reaped=20  bytes_reclaimed=10240 KiB | blobs=95 (47 MiB) snapshots=48 package_hash=3350001
restart 6 startup            dur=24.02s  deadline=true  retired=9   displaced_reaped=0   blobs_reaped=18  bytes_reclaimed=9216  KiB | blobs=77 (38 MiB) snapshots=39 package_hash=2710001
restart 7 startup            dur=24.38s  deadline=true  retired=9   displaced_reaped=0   blobs_reaped=18  bytes_reclaimed=9216  KiB | blobs=59 (29 MiB) snapshots=30 package_hash=2090001
restart 8 startup            dur=24.03s  deadline=true  retired=11  displaced_reaped=0   blobs_reaped=22  bytes_reclaimed=11264 KiB | blobs=37 (18 MiB) snapshots=19 package_hash=1300001
```

Reading it: every run ends at its 30 s budget (24.0 to 26.6 s, none over); blobs are reaped on every run from day 1 (55, 6, 6, then 8 to 22 at each restart); and the eight restarts drain the backlog (snapshots 73 to 19, blobs 145 to 37, `package_hash` 5.06M to 1.30M). The rows still track snapshots (1.30M rows over 19 snapshots is about 68,000 each), so here too the table shrinks because C deletes snapshots, and D, behind C, has not yet stripped the kept displaced ones (steady state would be about 280,000 rows: four current snapshots). The harness stops at the first startup with no deadline, which this workload does not reach in eight restarts.

### package_hash benchmark (variant base)

Command: `ACU_BENCH=1 ACU_BENCH_K=1,4 ACU_BENCH_VARIANTS=base go test ./internal/cache -run TestPackageHashBench -v -timeout 60m` (PASS, 99.0s)

```
RESULT k=1  rows=840000   variant=base      seed=15s      insert70k=1.289s   delete70k_chunked=839ms    per10k=120ms   delete70k_whole=570ms    db=320MiB
RESULT k=4  rows=3360000  variant=base      seed=1m6s     insert70k=1.97s    delete70k_chunked=1.47s    per10k=210ms   delete70k_whole=774ms    db=1172MiB
```

Comparison per the plan: base k=4 "Before" (3.36M rows, the table with `keep_displaced = 3` and no strip) against base k=1 "After" (0.84M rows, current snapshots only, the stripped steady state), "Before" k=4 first, then "After" k=1:

| | rows | insert 70k | delete 70k, 10k chunks | per 10k chunk | delete 70k, whole | db |
|---|---|---|---|---|---|---|
| Before, k=4 | 3.36M | 1.488 s | 1.138 s | 163 ms | 846 ms | 1172 MiB |
| After, k=1 | 0.84M | 1.289 s | 839 ms | 120 ms | 570 ms | 320 MiB |

The strip likely makes each insert and each chunked delete cheaper (a chunk 120 ms, not 163 ms), but by how much these runs cannot say: the gaps (some 13% and 26%) sit inside the machine's run-to-run noise. The second run of k=4 in this "After" session measured 1.97 s, 1.47 s and 210 ms, against 1.488 s, 1.138 s and 163 ms "Before": that noise is about 30%, so the k=1 against k=4 gap is likely real, all of it in the same direction, but its size is unmeasured. k=1 "Before" and "After" (1.355 s, 904 ms, 129 ms against 1.289 s, 839 ms, 120 ms) agree within noise, as they should (the benchmark runs the same code). The database file does not shrink on a strip (SQLite reuses the pages); `db=` is the file as seeded.
