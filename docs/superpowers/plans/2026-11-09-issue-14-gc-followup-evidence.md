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

## After

(To be filled in by Task 7.)
