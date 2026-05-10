package admin

import (
	"math"
	"sync/atomic"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
)

// refresherGauges owns every gauge the SPEC5 §9.7.6 refresher
// recomputes. Declared together so the New() registration site
// is one place to find every metric, and so a fresh registry per
// test (admin_test.go uses metrics.NewRegistry()) means each test
// instance gets its own series storage with no global coupling.
//
// The unlabeled gauges are constructed with cap=0 (unbounded; they
// only ever produce a single series). The labeled gauges
// (per_host_*) carry the configured admin.metric_series_cap so a
// cardinality blow-up from `host` strings cannot exhaust process
// memory.
//
// AIDEV-NOTE: gauge declarations live here rather than in init()
// so admin can be constructed against arbitrary registries —
// metrics.Default in production, metrics.NewRegistry() in tests.
type refresherGauges struct {
	// Database-derivable (cache.GetCacheStats).
	blobsDBCount             *metrics.Gauge
	blobsDBTotalBytes        *metrics.Gauge
	blobsZeroRefcountBacklog *metrics.Gauge
	urlPathsTracked          *metrics.Gauge

	// Suite/snapshot counts (cache.GetSuiteStats).
	suitesTracked      *metrics.Gauge
	snapshotsCurrent   *metrics.Gauge
	snapshotsDisplaced *metrics.Gauge

	// Filesystem (filepath.Walk on pool/).
	poolDiskBytes *metrics.Gauge

	// Hostsem snapshot (hostsem.Snapshot, hostsem.HostCount).
	activeHosts     *metrics.Gauge
	perHostInflight *metrics.Gauge // labels: host
	perHostCapacity *metrics.Gauge // labels: host

	// SPEC6_5 §10.3: per-kind package_hash row counts. Labeled by
	// kind ∈ {binary, source, pdiff}; cardinality is a closed enum
	// (3 values), well under metric_series_cap.
	packageHashRowsByKind *metrics.Gauge // labels: kind
}

// newRefresherGauges declares every gauge the refresher owns and
// registers them on the given registry. capLimit is admin.metric_series_cap
// — applied to the per-host gauges; unlabeled gauges use cap=0.
func newRefresherGauges(r *metrics.Registry, capLimit int) *refresherGauges {
	return &refresherGauges{
		blobsDBCount: metrics.NewGaugeWithCapIn(r,
			"acu_blobs_db_count",
			"Number of blob rows in the cache database (refresher-driven).",
			0),
		blobsDBTotalBytes: metrics.NewGaugeWithCapIn(r,
			"acu_blobs_db_total_bytes",
			"Sum of blob.size across the cache database (refresher-driven).",
			0),
		blobsZeroRefcountBacklog: metrics.NewGaugeWithCapIn(r,
			"acu_blobs_zero_refcount_backlog",
			"Blobs with refcount<=0 awaiting GC reaping (refresher-driven).",
			0),
		urlPathsTracked: metrics.NewGaugeWithCapIn(r,
			"acu_url_paths_tracked",
			"Number of url_path rows in the cache database (refresher-driven).",
			0),
		suitesTracked: metrics.NewGaugeWithCapIn(r,
			"acu_suites_tracked",
			"Number of suite_freshness rows (refresher-driven).",
			0),
		snapshotsCurrent: metrics.NewGaugeWithCapIn(r,
			"acu_snapshots_current",
			"Suites whose current_snapshot_id is non-NULL (refresher-driven).",
			0),
		snapshotsDisplaced: metrics.NewGaugeWithCapIn(r,
			"acu_snapshots_displaced",
			"Adopted suite_snapshot rows that are no longer current (refresher-driven).",
			0),
		poolDiskBytes: metrics.NewGaugeWithCapIn(r,
			"acu_pool_disk_bytes",
			"On-disk byte total under pool/ (filepath.Walk; refresher-driven).",
			0),
		activeHosts: metrics.NewGaugeWithCapIn(r,
			"acu_active_hosts",
			"Distinct upstream hosts with hostsem state (refresher-driven).",
			0),
		perHostInflight: metrics.NewGaugeWithCapIn(r,
			"acu_per_host_inflight",
			"In-flight hostsem slots per host (refresher-driven).",
			capLimit, "host"),
		perHostCapacity: metrics.NewGaugeWithCapIn(r,
			"acu_per_host_capacity",
			"Configured hostsem slot capacity per host (refresher-driven).",
			capLimit, "host"),
		packageHashRowsByKind: metrics.NewGaugeWithCapIn(r,
			"acu_package_hash_rows_by_kind",
			"package_hash row count per kind (SPEC6_5 §10.3); kind ∈ {binary, source, pdiff}.",
			capLimit, "kind"),
	}
}

// startupGauges holds the SPEC5 §10.4.7 build/process info gauges,
// set once at New() and never mutated thereafter. These are not
// refresher-recomputed; they reflect process-lifetime constants.
//
// processStartTimeSeconds duplicates acu_process_start_unixtime
// under the conventional Prometheus name (§10.4.7 — both are
// emitted because the former is the convention and the latter is
// the application-namespaced form).
type startupGauges struct {
	buildInfo               *metrics.Gauge // labels: version, go_version, vcs_revision
	processStartUnixtime    *metrics.Gauge
	processStartTimeSeconds *metrics.Gauge
}

// newStartupGauges declares the §10.4.7 startup-fixed gauges,
// registers them, and sets their values from the supplied BuildInfo
// + start time. The build_info gauge always equals 1 (the labels
// carry the data — Prometheus convention for info-shaped metrics).
func newStartupGauges(r *metrics.Registry, capLimit int, info BuildInfo, start time.Time) *startupGauges {
	sg := &startupGauges{
		buildInfo: metrics.NewGaugeWithCapIn(r,
			"acu_build_info",
			"Build information (gauge=1, value carried in labels).",
			capLimit, "version", "go_version", "vcs_revision"),
		processStartUnixtime: metrics.NewGaugeWithCapIn(r,
			"acu_process_start_unixtime",
			"Process start time as unix seconds.",
			0),
		processStartTimeSeconds: metrics.NewGaugeWithCapIn(r,
			"process_start_time_seconds",
			"Process start time as unix seconds (Prometheus convention).",
			0),
	}
	sg.buildInfo.Set(1, info.Version, info.GoVersion, info.VCSRevision)
	startUnix := float64(start.Unix())
	sg.processStartUnixtime.Set(startUnix)
	sg.processStartTimeSeconds.Set(startUnix)
	return sg
}

// processGauges holds the SPEC5 §10.4.7 process-collector metrics
// — Prometheus-standard, unprefixed names per the §10.4
// naming-convention exception. Sourced from /proc/self/* on Linux
// and refreshed on the same cadence as the refresher gauges.
//
// The CPU counter tracks delta from the prior reading because the
// metrics.Counter primitive only supports Add. priorCPUSecondsBits
// holds the prior reading as math.Float64bits stored in an atomic
// — production callers always run on the refresher goroutine, but
// tests may drive runRefreshOnce directly, and atomic load/store
// makes that pattern race-free without a per-pass mutex.
type processGauges struct {
	cpuSecondsTotal     *metrics.Counter
	residentMemoryBytes *metrics.Gauge
	virtualMemoryBytes  *metrics.Gauge
	openFDs             *metrics.Gauge
	maxFDs              *metrics.Gauge

	priorCPUSecondsBits atomic.Uint64
}

// loadPriorCPU returns the most recent CPU-seconds reading observed
// by the refresher.
func (p *processGauges) loadPriorCPU() float64 {
	return math.Float64frombits(p.priorCPUSecondsBits.Load())
}

// storePriorCPU records the latest CPU-seconds reading for the
// next refresher pass to compute its delta against.
func (p *processGauges) storePriorCPU(v float64) {
	p.priorCPUSecondsBits.Store(math.Float64bits(v))
}

// newProcessGauges declares the §10.4.7 process metrics on the
// given registry. Caps are unbounded: each metric has exactly one
// series (the process itself).
//
// AIDEV-NOTE: the cpu counter is primed with Add(0) so its single
// series is materialized BEFORE the first refreshProcessMetrics
// runs. Without this, a scrape that happens between New() and the
// first refresh — or on non-Linux where readProcStats returns 0
// and the delta>0 guard skips Add — would render HELP/TYPE without
// a sample, which some Prometheus client tooling rejects.
func newProcessGauges(r *metrics.Registry) *processGauges {
	pg := &processGauges{
		cpuSecondsTotal: metrics.NewCounterWithCapIn(r,
			"process_cpu_seconds_total",
			"Total CPU time (user+system) consumed by the process.",
			0),
		residentMemoryBytes: metrics.NewGaugeWithCapIn(r,
			"process_resident_memory_bytes",
			"Resident set size from /proc/self/statm.",
			0),
		virtualMemoryBytes: metrics.NewGaugeWithCapIn(r,
			"process_virtual_memory_bytes",
			"Virtual memory size from /proc/self/statm.",
			0),
		openFDs: metrics.NewGaugeWithCapIn(r,
			"process_open_fds",
			"Number of open file descriptors (count of /proc/self/fd entries).",
			0),
		maxFDs: metrics.NewGaugeWithCapIn(r,
			"process_max_fds",
			"Soft limit on open file descriptors (RLIMIT_NOFILE).",
			0),
	}
	pg.cpuSecondsTotal.Add(0)
	return pg
}
