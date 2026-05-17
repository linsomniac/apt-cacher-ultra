package gc

import (
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
		"Total displaced suite_snapshot rows reaped (SPEC5 §10.4.5).",
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
