package gc

import (
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
)

// GC metrics declared per SPEC5 §10.4.5. Names mirror the
// `gc_run_complete` log field names so an operator grepping logs and
// Prometheus together recognizes the same identifiers.
//
// Every numeric field of the gc_run_complete log line has a counter,
// histogram, or gauge equivalent. The phase label distinguishes
// startup-only contributions (pool_orphans_repaired) from steady-state
// periodic ticks; SPEC4 §9.6.2 specifies that the periodic phase
// always emits zero for those fields.
//
// AIDEV-NOTE: emission is centralized in emitGCMetrics, which is
// invoked at each gc_run_complete log emit (one for startup, one per
// periodic tick). Adding a new gc_run_complete field means adding both
// a metric here AND extending the SPEC5 §10.4.5 inventory.
var (
	gcPassDurationSeconds = metrics.NewHistogramWithCap(
		"acu_gc_pass_duration_seconds",
		"Wall-clock duration of each GC pass, including database queue waits, by phase and pass.",
		[]float64{0.001, 0.01, 0.1, 0.5, 1, 5, 10, 30, 60, 300, 600},
		metrics.DefaultMaxSeries,
		"phase", "pass",
	)
	gcURLPathRowsScannedTotal = metrics.NewCounterWithCap(
		"acu_gc_url_path_rows_scanned_total",
		"URL-path candidates evaluated by GC, including retained rows.",
		0,
	)
	gcURLPathRowsStampedTotal = metrics.NewCounterWithCap(
		"acu_gc_url_path_rows_stamped_total",
		"URL-path rows entering hold grace during GC.",
		0,
	)
	gcURLPathRowsClearedTotal = metrics.NewCounterWithCap(
		"acu_gc_url_path_rows_cleared_total",
		"URL-path hold-grace stamps cleared after retention re-qualification.",
		0,
	)

	gcRunsTotal = metrics.NewCounterWithCap(
		"acu_gc_runs_total",
		"Total GC runs completed, labeled by phase (`startup` or `periodic`) (SPEC5 §10.4.5).",
		metrics.DefaultMaxSeries,
		"phase",
	)

	gcBlobsReapedTotal = metrics.NewCounterWithCap(
		"acu_gc_blobs_reaped_total",
		"Total blobs reaped by GC (SPEC5 §10.4.5).",
		0,
	)

	gcBytesReclaimedTotal = metrics.NewCounterWithCap(
		"acu_gc_bytes_reclaimed_total",
		"Total bytes reclaimed by GC (SPEC5 §10.4.5).",
		0,
	)

	gcOrphanCandidatesReapedTotal = metrics.NewCounterWithCap(
		"acu_gc_orphan_candidates_reaped_total",
		"Total candidate suite_snapshot rows reaped (SPEC5 §10.4.5).",
		0,
	)

	gcDisplacedReapedTotal = metrics.NewCounterWithCap(
		"acu_gc_displaced_reaped_total",
		"Total displaced suite_snapshot rows beyond keep_displaced retired by snapshot-GC sub-job B, at startup and in periodic ticks alike (sub-job C deletes them later and counts them in acu_gc_retired_reaped_total).",
		0,
	)

	gcRetiredReapedTotal = metrics.NewCounterWithCap(
		"acu_gc_retired_reaped_total",
		"Total retired suite_snapshot rows reaped by snapshot-GC sub-job C: snapshots of reaped, evicted or forgotten suites, snapshots the legacy trust audit quarantined (spec C9), and displaced snapshots beyond keep_displaced a startup or periodic run retired (counted in acu_gc_displaced_reaped_total when retired).",
		0,
	)
	gcDisplacedPackageRowsDeletedTotal = metrics.NewCounterWithCap(
		"acu_gc_displaced_package_rows_deleted_total",
		"Total package_hash rows of displaced snapshots deleted by snapshot-GC sub-job D, in the startup pass, the periodic ticks and the retired runs; the snapshots' headers and index files stay (gc.keep_displaced).",
		0,
	)
	gcURLPathRowsReapedTotal = metrics.NewCounterWithCap(
		"acu_gc_url_path_rows_reaped_total",
		"Total url_path rows reaped by the URL-path TTL pass (SPEC4 §5 fourth class).",
		0,
	)

	gcPoolOrphansRepairedTotal = metrics.NewCounterWithCap(
		"acu_gc_pool_orphans_repaired_total",
		"Pool/ orphans repaired (startup-only contribution; periodic ticks emit 0) (SPEC5 §10.4.5).",
		0,
	)

	gcPoolOrphanBytesRepairedTotal = metrics.NewCounterWithCap(
		"acu_gc_pool_orphan_bytes_repaired_total",
		"Pool/ orphan bytes repaired (startup-only contribution) (SPEC5 §10.4.5).",
		0,
	)

	gcPoolUnlinkErrorsTotal = metrics.NewCounterWithCap(
		"acu_gc_pool_unlink_errors_total",
		"Total pool/ unlink errors during GC (SPEC5 §10.4.5).",
		0,
	)

	// gcPassFailuresTotal counts the failed passes, one per pass per run
	// however many of its sub-jobs failed, where their WARN gc_tick_failed
	// is logged (issue #14 follow-up, Tier 1c): pass is url_path, snapshot
	// or blob (runTick, at a start and in a periodic tick), retired
	// (retiredRun) or pool_scan (StartupPass's pool scan, which logs no
	// gc_tick_failed: main logs its error as the startup pass's failure).
	//
	// AIDEV-NOTE: a failure that was logged is counted, even when a
	// shutdown lands later in the same pass or run (the controller's
	// ruling on Task 5's review, 2026-11-09); a pass that only a shutdown
	// stopped logged nothing, did not fail, and is not counted, nor is a
	// write the writer never started before its deadline (notStarted).
	gcPassFailuresTotal = metrics.NewCounterWithCap(
		"acu_gc_pass_failures_total",
		"Total GC passes that failed, logged as WARN gc_tick_failed (one per pass per run, however many of its sub-jobs failed) or, for pool_scan, returned by the startup pool scan; by pass: url_path, snapshot, blob, retired, pool_scan.",
		metrics.DefaultMaxSeries,
		"pass",
	)

	gcRepresentationRowsPrunedTotal = metrics.NewCounterWithCap(
		"acu_gc_representation_rows_pruned_total",
		"Total stored upstream dates (representation_metadata rows) no URL or snapshot owns any more, released by the blob pass's once-a-run representation prune.",
		0,
	)

	gcDeadlineReachedTotal = metrics.NewCounterWithCap(
		"acu_gc_deadline_reached_total",
		"Per-phase counter incremented when a tick exits early due to gc.max_tick_duration (SPEC5 §10.4.5).",
		metrics.DefaultMaxSeries,
		"phase",
	)

	// gcRunDurationBuckets are SPEC5 §10.4.5 — 0.1s..600s covers
	// fast empty ticks and long startup walks.
	gcRunDurationBuckets = []float64{0.1, 0.5, 1, 5, 10, 30, 60, 300, 600}

	gcRunDurationSeconds = metrics.NewHistogramWithCap(
		"acu_gc_run_duration_seconds",
		"Wall-clock GC run duration in seconds, by phase (SPEC5 §10.4.5).",
		gcRunDurationBuckets,
		metrics.DefaultMaxSeries,
		"phase",
	)

	gcLastRunUnixtime = metrics.NewGaugeWithCap(
		"acu_gc_last_run_unixtime",
		"Unix-seconds timestamp of the last GC run completion, by phase (SPEC5 §10.4.5).",
		metrics.DefaultMaxSeries,
		"phase",
	)
)

func emitGCPassMetrics(phase string, tick tickResult) {
	for _, p := range []struct {
		name     string
		duration time.Duration
	}{
		{"url_path", tick.urlPathDuration},
		{"snapshot", tick.snapshotDuration},
		{"blob", tick.blobDuration},
		// The blob pass's representation prune, inside its blob sample
		// (issue #14 follow-up, Tier 1c); 0 when the pass ran no prune.
		{"representation_prune", tick.representationPruneDuration},
	} {
		if p.duration > 0 {
			gcPassDurationSeconds.Observe(p.duration.Seconds(), phase, p.name)
		}
	}
	gcURLPathRowsScannedTotal.Add(float64(tick.urlPath.scanned))
	gcURLPathRowsStampedTotal.Add(float64(tick.urlPath.stamped))
	gcURLPathRowsClearedTotal.Add(float64(tick.urlPath.cleared))
	if tick.representationRowsPruned > 0 {
		gcRepresentationRowsPrunedTotal.Add(float64(tick.representationRowsPruned))
	}
}

// emitGCMetrics records all GC metrics for one gc_run_complete log
// emit. Called from RunStartup and the periodic tick loop with the
// fields that mirror the structured log payload.
//
// poolOrphansRepaired and poolOrphanBytesRepaired are 0 for periodic
// ticks per SPEC4 §9.6.2; passing 0 emits a counter Add(0), which is
// a no-op for Prometheus consumers but keeps the call site uniform
// across phases.
func emitGCMetrics(
	phase string,
	durationSeconds float64,
	endUnix int64,
	blobsReaped int,
	bytesReclaimed int64,
	orphanCandidatesReaped int,
	displacedReaped int,
	retiredReaped int,
	displacedPackageRowsDeleted int,
	urlPathRowsReaped int,
	poolOrphansRepaired int,
	poolOrphanBytesRepaired int64,
	poolUnlinkErrors int,
	deadlineReached bool,
) {
	gcRunsTotal.Inc(phase)
	gcRunDurationSeconds.Observe(durationSeconds, phase)
	gcLastRunUnixtime.Set(float64(endUnix), phase)
	if blobsReaped > 0 {
		gcBlobsReapedTotal.Add(float64(blobsReaped))
	}
	if bytesReclaimed > 0 {
		gcBytesReclaimedTotal.Add(float64(bytesReclaimed))
	}
	if orphanCandidatesReaped > 0 {
		gcOrphanCandidatesReapedTotal.Add(float64(orphanCandidatesReaped))
	}
	if displacedReaped > 0 {
		gcDisplacedReapedTotal.Add(float64(displacedReaped))
	}
	if retiredReaped > 0 {
		gcRetiredReapedTotal.Add(float64(retiredReaped))
	}
	if displacedPackageRowsDeleted > 0 {
		gcDisplacedPackageRowsDeletedTotal.Add(float64(displacedPackageRowsDeleted))
	}
	if urlPathRowsReaped > 0 {
		gcURLPathRowsReapedTotal.Add(float64(urlPathRowsReaped))
	}
	if poolOrphansRepaired > 0 {
		gcPoolOrphansRepairedTotal.Add(float64(poolOrphansRepaired))
	}
	if poolOrphanBytesRepaired > 0 {
		gcPoolOrphanBytesRepairedTotal.Add(float64(poolOrphanBytesRepaired))
	}
	if poolUnlinkErrors > 0 {
		gcPoolUnlinkErrorsTotal.Add(float64(poolUnlinkErrors))
	}
	if deadlineReached {
		gcDeadlineReachedTotal.Inc(phase)
	}
}
