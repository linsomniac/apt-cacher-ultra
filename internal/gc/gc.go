// Package gc implements the SPEC4 Phase 4 garbage collector: orphan
// blobs, orphan/displaced suite_snapshot rows, and pool/ orphan files.
//
// Architecture: a single dedicated goroutine runs the periodic tick
// loop (gc.interval delay after completion). Each tick splits its
// wall-clock budget (gc.max_tick_duration) once at the top into a deadline
// per pass (passDeadlines), then resumes URL-path, snapshot, and blob
// passes in order, each under its own deadline (issue #14).
// Snapshot before blob because snapshot DELETEs remove the FK
// references that the blob pass's NOT EXISTS reachability predicate
// consults — running them in the reverse order would leave one tick of
// blob latency on the table per displacement.
//
// All writes go through cache.submitWrite; the writer goroutine is the
// single serialization point. Reads (e.g. the per-batch SELECTs) run
// inside the same writer-tx as the DELETE for snapshot GC (see SPEC4
// §9.6.3 liveness revalidation), and inside a writer-tx for blob GC
// (the SELECT and the DELETE...RETURNING are atomic per batch).
//
// On lifecycleCtx cancel the goroutine exits at the next per-batch
// boundary; in-flight transactions commit or roll back atomically; any
// `os.Remove` calls already underway run to completion since they hold
// no SQL lock. Partial-batch work is re-picked-up next tick.
package gc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// Config wires the gc package to its dependencies. All fields are
// required; main constructs this once at startup.
type Config struct {
	Cache  *cache.Cache
	Logger *slog.Logger

	// Enabled mirrors gc.enabled. False short-circuits Run (no
	// goroutine work, no startup GC pass) and StartupScan and
	// StartupPass (no startup pool scan). Main emits gc_disabled Warn before
	// constructing the GC at all when Enabled = false; the field is
	// here so this package's tests can exercise the short-circuit
	// without going through main.
	Enabled bool

	// Interval is the periodic-tick cadence (gc.interval). Must be
	// > 0 when Enabled.
	Interval time.Duration

	// BatchSize is the per-batch DELETE LIMIT for the blob pass
	// (gc.batch_size). Must be >= 1 when Enabled.
	BatchSize int

	// SnapshotBatchSize is the per-batch DELETE LIMIT for the
	// snapshot pass (gc.snapshot_batch_size). Must be >= 1 when
	// Enabled.
	SnapshotBatchSize int

	// MaxTickDuration is the per-tick wall-clock budget, split into
	// the passes' deadlines (gc.max_tick_duration; passDeadlines). Must
	// be > 0 when Enabled.
	MaxTickDuration time.Duration

	// BlobGrace is the "since refcount reached 0" grace
	// (gc.blob_grace). Must be > 0 when Enabled.
	BlobGrace time.Duration

	// KeepDisplaced is the per-suite forensic retention count
	// (gc.keep_displaced). Must be >= 0 when Enabled.
	KeepDisplaced int

	// PoolScanWorkers is the worker pool size for the startup
	// pool/ orphan scan (gc.pool_scan_workers). Must be >= 1 when
	// Enabled.
	PoolScanWorkers int

	// HeartbeatStaleGrace is the runtime-derived
	// max(upstream.total_timeout × upstream.max_retries, 30m). The
	// snapshot pass's sub-job A reaps candidate rows whose
	// heartbeat_at is older than this. Must be > 0 when Enabled.
	HeartbeatStaleGrace time.Duration

	// URLPathTTL bounds how long a url_path row may sit untouched
	// (gc.url_path_ttl). 0 disables the URL-path pass entirely.
	// Validated by the config layer; the gc package treats 0 as a
	// short-circuit (no batches run).
	URLPathTTL time.Duration

	// HoldWindow is hold_packages.window: the grace a url_path row gets
	// (via url_path.dropped_at) after it leaves the kept set before it is
	// reaped (version-aware retention §3 rule 3). 0 = no grace (a row
	// failing the recency + mirror guards is deleted in the same scan).
	HoldWindow time.Duration

	// MaxVersionsPerPackage is retention.max_versions_per_package: the
	// url_path mirror guard keeps the newest N distinct versions per
	// (suite, package_name, architecture). Clamped to >= 1.
	MaxVersionsPerPackage int
}

// GC is the orchestrator. StartupScan() runs the §4.2 step 5 pool scan,
// which main blocks on before the listeners come up; Run() runs the step 6
// one-shot GC pass first, after them (2026-11-11), then owns the periodic
// tick loop. StartupPass() runs both in one call, for the tests.
type GC struct {
	cfg Config

	// store is cfg.Cache behind gcStore (the snapshot pass and the pool
	// scan); tests replace it.
	store gcStore

	// nextPass preserves progress through URL-path → snapshot → blob work.
	// A url-path or snapshot pass that hits its deadline moves it on, and
	// its tick goes on to the passes after it (issue #14); a blob pass that
	// hits its deadline leaves it at the blob pass for the next tick. Either
	// way no pass restarts its work ahead of the others and starves them
	// (the url-path pass resumes at urlPathCursor). A pass that panicked
	// moves it too, past that pass, for the next tick (skipPanickedPass); one
	// that failed moves it past that pass in its own tick, whose passes after
	// it then run (passFailed, ruling HD-15). Only the GC goroutine touches
	// this: the startup pass runs on it too, first (Run).
	nextPass pass

	// startup is the pending startup pass's pool scan: set by StartupScan,
	// which main runs before any listener, and taken by runStartupPass,
	// the first thing Run does, on the GC goroutine (the go statement that
	// starts Run orders the write before the read). nil when no scan ran,
	// the scan failed, or the pass has run.
	startup *startupScan

	// lastRunMu guards lastRun. Writes happen at the end of each
	// completed tick (Run goroutine and StartupPass — sequential, but
	// the field is read concurrently by status-page handler and the
	// metrics refresher).
	lastRunMu sync.Mutex
	lastRun   *LastRunSummary

	// urlPathCursor persists the url_path GC scan position across ticks.
	// A pass that hits its deadline before draining saves its place here
	// and resumes at the next tick that reaches it, instead of restarting
	// from the beginning — which would rescan the same prefix forever and
	// never reach rows later in (scheme,host,path) order. Its deadline no
	// longer ends the tick (issue #14): the snapshot and blob passes run
	// after it. It lives in memory, so every start begins a fresh scan.
	// Reset to empty once a pass drains. Touched
	// only by the tick goroutine (the startup pass runs on it, first), so
	// no lock is needed.
	urlPathCursor struct{ scheme, host, path string }

	// panicStacks holds the wheres whose gc_panicked line carried a stack
	// (logGCPanic): once per where per process, since main builds one GC.
	//
	// AIDEV-NOTE: a field, not a package global (review of H1 Task 7), so
	// each test's fresh GC logs its first panic's stack and can assert it
	// without depending on test order.
	panicStacks sync.Map
}

type pass uint8

const (
	urlPathPass pass = iota
	snapshotPass
	blobPass
)

// phaseStartup is runTick's phase for the one-shot startup pass, which
// Run runs first, after the listeners opened (2026-11-11; until then main
// ran it before any listener served). Its snapshot pass and the
// periodic ticks' ("periodic") alike run sub-job B, which only retires the
// displaced snapshots beyond keep_displaced, and sub-job C, which deletes
// them (DN-7 of the GC and upstream hardening plan; ruling PF-15, re-ruled
// for issue #14: runSnapshotPass). Until 2026-11-09 (issue #14 follow-up
// Tier 1b) the startup B deleted them itself (ruling HD-7); C now runs at
// a start, and the start deadline bounds every startup write, so it
// retires too.
const phaseStartup = "startup"

// LastRunSummary is the SPEC5 §9.6 / §9.7.8 in-memory captured copy
// of the most recently completed gc_run_complete payload. The
// status-page handler renders this; the §9.7.6 refresher mirrors
// fields into Phase 5 GC counters/gauges.
type LastRunSummary struct {
	Phase                  string // "startup" | "periodic"
	AtUnixTime             int64
	DurationSeconds        float64
	BlobsReaped            int
	BytesReclaimed         int64
	OrphanCandidatesReaped int
	DisplacedReaped        int
	RetiredReaped          int // snapshot-GC sub-job C (spec C9)
	// DisplacedPackageRowsDeleted is snapshot-GC sub-job D's count: the
	// package_hash rows of displaced snapshots it stripped (issue #14
	// follow-up, Tier 2).
	DisplacedPackageRowsDeleted int
	URLPathRowsReaped           int
	URLPathRowsScanned          int
	URLPathRowsStamped          int
	URLPathRowsCleared          int
	URLPathDurationSeconds      float64
	SnapshotDurationSeconds     float64
	BlobDurationSeconds         float64
	PoolOrphansRepaired         int
	PoolOrphanBytesRepaired     int64
	PoolUnlinkErrors            int
	DeadlineReached             bool
	// FailedPasses names the run's passes whose failure was logged (WARN
	// gc_tick_failed), in tick order, each once: "url_path", "snapshot",
	// "blob"; nil when none failed (issue #14 follow-up, Tier 1c). A
	// retired run's failure never reaches it (that run writes no summary),
	// nor a failed pool scan's (the startup pass then writes none).
	FailedPasses []string
	// RepresentationRowsPruned and RepresentationPruneSeconds are the blob
	// pass's representation prune: the stored upstream dates it released
	// and how long it took, its wait for the writer included (issue #14
	// follow-up, Tier 1c).
	RepresentationRowsPruned   int
	RepresentationPruneSeconds float64
}

// Interval returns the configured periodic-tick cadence. Pure accessor
// over Config.Interval — exposed so the admin status page's
// htmlRenderModel can carry the operator-configured value to the
// presentation layer without admin importing the GC config struct.
// See docs/admin-ui-spec.md §0.7 / §9.1 (GC watching threshold).
func (g *GC) Interval() time.Duration {
	return g.cfg.Interval
}

// LastRunSummary returns a copy of the most recently completed GC
// run's summary, or (zero, false) when no run has completed since
// process start. The returned struct is independent of subsequent
// runs: the caller may retain it without locking.
//
// AIDEV-NOTE: SPEC5 §9.6 — this is a pure accessor, not a behavioral
// change. Status-page handler (interactive) and metrics refresher
// (30s cadence) both read this; the lock-hold is brief.
func (g *GC) LastRunSummary() (LastRunSummary, bool) {
	g.lastRunMu.Lock()
	defer g.lastRunMu.Unlock()
	if g.lastRun == nil {
		return LastRunSummary{}, false
	}
	s := *g.lastRun
	s.FailedPasses = slices.Clone(s.FailedPasses) // the caller's own copy (nil stays nil)
	return s, true
}

// recordLastRun captures one completed tick's payload. Called at the
// same emit site as the gc_run_complete log line. The store is a
// pointer swap so the read path's defer-unlock copy is consistent —
// no torn reads under -race.
func (g *GC) recordLastRun(s LastRunSummary) {
	g.lastRunMu.Lock()
	g.lastRun = &s
	g.lastRunMu.Unlock()
}

// New validates Config and returns a ready GC. Returns an error if
// Enabled and any required tunable is invalid; main treats this as a
// startup failure.
func New(cfg Config) (*GC, error) {
	if cfg.Cache == nil {
		return nil, errors.New("gc: Cache is required")
	}
	if cfg.Logger == nil {
		return nil, errors.New("gc: Logger is required")
	}
	if !cfg.Enabled {
		return &GC{cfg: cfg, store: cfg.Cache}, nil
	}
	if cfg.Interval <= 0 {
		return nil, fmt.Errorf("gc: Interval must be > 0, got %s", cfg.Interval)
	}
	if cfg.BatchSize < 1 {
		return nil, fmt.Errorf("gc: BatchSize must be >= 1, got %d", cfg.BatchSize)
	}
	if cfg.SnapshotBatchSize < 1 {
		return nil, fmt.Errorf("gc: SnapshotBatchSize must be >= 1, got %d", cfg.SnapshotBatchSize)
	}
	if cfg.MaxTickDuration <= 0 {
		return nil, fmt.Errorf("gc: MaxTickDuration must be > 0, got %s", cfg.MaxTickDuration)
	}
	if cfg.BlobGrace <= 0 {
		return nil, fmt.Errorf("gc: BlobGrace must be > 0, got %s", cfg.BlobGrace)
	}
	if cfg.KeepDisplaced < 0 {
		return nil, fmt.Errorf("gc: KeepDisplaced must be >= 0, got %d", cfg.KeepDisplaced)
	}
	if cfg.PoolScanWorkers < 1 {
		return nil, fmt.Errorf("gc: PoolScanWorkers must be >= 1, got %d", cfg.PoolScanWorkers)
	}
	if cfg.HeartbeatStaleGrace <= 0 {
		return nil, fmt.Errorf("gc: HeartbeatStaleGrace must be > 0, got %s", cfg.HeartbeatStaleGrace)
	}
	return &GC{cfg: cfg, store: cfg.Cache}, nil
}

// startupScan is a completed startup pool scan, waiting for its startup
// pass (GC.startup).
type startupScan struct {
	scan scanResult
	took time.Duration
}

// StartupScan runs the SPEC4 §4.2 step 5 pool/ orphan scan, which main
// runs before any listener opens, and leaves step 6, the one-shot GC pass,
// to Run, which runs it first, after the listeners opened. Blocks until
// the scan completes or ctx is cancelled. A scan that fails (its pool/
// read) counts pass="pool_scan" and returns its error, which main logs;
// no startup pass runs at that start. A panic is recovered and returned
// as an error (gc_panicked, where=startup). When Enabled = false, returns
// nil at once.
//
// AIDEV-NOTE: the maintainer's decision, 2026-11-11 (a restart on
// adm1.dev waited 102 s before serving, 99 s of it sub-job D stripping the
// upgrade's backlog of displaced package rows; the first 1.1.0 start
// waited 4 min). The scan stays before the listeners: it unlinks a pool
// file with no blob row, and a fetch writes its pool file before its
// PutBlob row, so a scan beside live traffic could unlink a file a request
// just fetched. The passes run beside live traffic at every periodic tick
// already, and the startup phase changes nothing in them but log labels
// (phaseStartup), so they move to Run's goroutine: never beside a periodic
// tick or a retired run, and before Run's schedule starts. This supersedes
// the listener bound that rulings HD-7 and PF-15 (re-ruled for issue #14)
// accepted: a start no longer waits up to max_tick_duration before it
// serves; the start's GC takes as long as before, beside the traffic.
func (g *GC) StartupScan(ctx context.Context) (err error) {
	defer g.recoverPanicInto("startup", &err)
	g.startup = nil
	if !g.cfg.Enabled {
		return nil
	}
	start := time.Now()
	scan, err := g.runPoolScan(ctx)
	if err != nil {
		// Counted here, as main logs it (issue #14 follow-up Tier 1c); a
		// shutdown never fails the scan (runPoolScan returns only its
		// pool/ read's error), and no summary is written.
		gcPassFailuresTotal.Inc("pool_scan")
		return fmt.Errorf("gc startup: pool scan: %w", err)
	}
	if ctx.Err() != nil {
		// A shutdown stopped the scan part way (it returns no error then):
		// leave no startup pass pending; the next start scans again.
		return nil
	}
	g.startup = &startupScan{scan: scan, took: time.Since(start)}
	return nil
}

// StartupPass runs StartupScan and then the startup pass in one call, on
// the caller's goroutine: the tests that drive a whole start use it; main
// runs StartupScan before the listeners and leaves the pass to Run
// (2026-11-11). Returns the first error: a failed pool scan's, which ends
// it before the pass, or the pass's (runStartupPass). When Enabled = false,
// returns nil immediately.
func (g *GC) StartupPass(ctx context.Context) error {
	if err := g.StartupScan(ctx); err != nil {
		return err
	}
	return g.runStartupPass(ctx)
}

// runStartupPass runs the SPEC4 §4.2 step 6 one-shot GC pass after a
// successful StartupScan, and nothing when none is pending (no scan ran,
// it failed, or the pass already ran): Run calls it first, on the GC
// goroutine, after main opened the listeners. Returns the first failed
// pass's error, once the passes after it have run too (runTick, ruling
// HD-15: a failed pass no longer ends the one-shot pass, so a daemon
// restarted more often than gc.interval, which never runs a periodic tick,
// still runs every pass at every start) and the pass has written its
// gc_run_complete line, metrics and last-run summary, as a periodic tick
// does: each failed pass logged its own gc_tick_failed line (runTick), and
// Run logs the returned first failure as WARN "gc startup pass failed"
// (the fix wave's follow-up of HD-15; main logged it until 2026-11-11).
// Partial progress is preserved (DB rows / pool files reaped before cancel
// are real). The pass runs snapshot-GC sub-job C (spec C9's retired
// snapshots) and D within its snapshot deadline, so it may take up to
// max_tick_duration, beside the traffic since 2026-11-11 (StartupScan's
// AIDEV-NOTE); what its budget leaves, Run's retired run (an hour after
// the pass when gc.interval is longer, then hourly) and the periodic ticks
// delete. Its gc_run_complete line and summary carry the scan's counts,
// and its duration_ms is the scan's plus the pass's, not the time between.
//
// Order matters: the pool scan runs first so its
// `gc_pool_orphans_repaired` count reflects only pre-existing orphan
// files (not files just created by the same-tick blob GC pass).
//
// A panic is recovered and returned as an error (gc_panicked,
// where=startup), which Run logs: a panic that persisted data triggers
// would otherwise recur at every start (review I-1 of the H1 plan). A
// panic in its passes moves the rotation past the pass in progress, as a
// periodic tick's does, so the first periodic tick does not begin with
// the pass that panicked (re-review m-4).
func (g *GC) runStartupPass(ctx context.Context) (err error) {
	s := g.startup
	if s == nil {
		return nil
	}
	g.startup = nil
	scan := s.scan
	passesDone := true // a panic before runTick leaves the rotation as it is
	defer g.skipPanickedPass(&passesDone)
	defer g.recoverPanicInto("startup", &err)
	start := time.Now()

	// Step 6: one-shot GC pass, sub-job C included (ruling PF-15, re-ruled
	// for issue #14: runSnapshotPass). A failed pass is returned after the
	// line, the metrics and the summary below, which report what the passes
	// that ran did, as a periodic tick's do.
	passesDone = false
	tick, tickErr := g.runTick(ctx, phaseStartup)
	passesDone = true

	duration := s.took + time.Since(start)
	endUnix := wallclock.Unix()
	g.cfg.Logger.Info("gc_run_complete",
		"phase", "startup",
		"blobs_reaped", tick.blobsReaped,
		"bytes_reclaimed", tick.bytesReclaimed,
		"orphan_candidates_reaped", tick.orphanCandidatesReaped,
		"displaced_reaped", tick.displacedReaped,
		"retired_reaped", tick.retiredReaped,
		"displaced_package_rows_deleted", tick.displacedPackageRowsDeleted,
		"url_path_rows_reaped", tick.urlPathRowsReaped,
		"url_path_rows_scanned", tick.urlPath.scanned,
		"url_path_rows_stamped", tick.urlPath.stamped,
		"url_path_rows_cleared", tick.urlPath.cleared,
		"url_path_duration_ms", tick.urlPathDuration.Milliseconds(),
		"snapshot_duration_ms", tick.snapshotDuration.Milliseconds(),
		"blob_duration_ms", tick.blobDuration.Milliseconds(),
		"representation_rows_pruned", tick.representationRowsPruned,
		"representation_prune_ms", tick.representationPruneDuration.Milliseconds(),
		"pool_orphans_repaired", scan.orphansRepaired,
		"pool_orphan_bytes_repaired", scan.orphanBytesRepaired,
		"pool_unlink_errors", tick.poolUnlinkErrors+scan.unlinkErrors,
		"deadline_reached", tick.deadlineReached,
		"duration_ms", duration.Milliseconds(),
	)
	emitGCMetrics(
		"startup",
		duration.Seconds(),
		endUnix,
		tick.blobsReaped,
		tick.bytesReclaimed,
		tick.orphanCandidatesReaped,
		tick.displacedReaped,
		tick.retiredReaped,
		tick.displacedPackageRowsDeleted,
		tick.urlPathRowsReaped,
		scan.orphansRepaired,
		scan.orphanBytesRepaired,
		tick.poolUnlinkErrors+scan.unlinkErrors,
		tick.deadlineReached,
	)
	emitGCPassMetrics("startup", tick)
	g.recordLastRun(LastRunSummary{
		Phase:                       "startup",
		AtUnixTime:                  endUnix,
		DurationSeconds:             duration.Seconds(),
		BlobsReaped:                 tick.blobsReaped,
		BytesReclaimed:              tick.bytesReclaimed,
		OrphanCandidatesReaped:      tick.orphanCandidatesReaped,
		DisplacedReaped:             tick.displacedReaped,
		RetiredReaped:               tick.retiredReaped,
		DisplacedPackageRowsDeleted: tick.displacedPackageRowsDeleted,
		URLPathRowsReaped:           tick.urlPathRowsReaped,
		URLPathRowsScanned:          tick.urlPath.scanned,
		URLPathRowsStamped:          tick.urlPath.stamped,
		URLPathRowsCleared:          tick.urlPath.cleared,
		URLPathDurationSeconds:      tick.urlPathDuration.Seconds(),
		SnapshotDurationSeconds:     tick.snapshotDuration.Seconds(),
		BlobDurationSeconds:         tick.blobDuration.Seconds(),
		PoolOrphansRepaired:         scan.orphansRepaired,
		PoolOrphanBytesRepaired:     scan.orphanBytesRepaired,
		PoolUnlinkErrors:            tick.poolUnlinkErrors + scan.unlinkErrors,
		DeadlineReached:             tick.deadlineReached,
		FailedPasses:                tick.failedPasses,
		RepresentationRowsPruned:    tick.representationRowsPruned,
		RepresentationPruneSeconds:  tick.representationPruneDuration.Seconds(),
	})
	if tickErr != nil {
		return fmt.Errorf("gc startup: tick: %w", tickErr)
	}
	return nil
}

// Run owns the periodic tick goroutine. Returns when ctx is cancelled.
// It first runs the startup pass a StartupScan left (runStartupPass;
// since 2026-11-11, after main opened the listeners), then its schedule:
// the first tick fires `gc.interval` after that pass ended, and
// each tick fires `gc.interval` after the previous tick completed. A
// long tick cannot leave a pending timer firing that starts work again
// immediately, even when an individual batch exceeds the tick budget.
// When `gc.interval` is longer than an hour, Run also deletes the retired
// snapshots an hour after the start — after the startup pass ended, since
// 2026-11-11 — (retiredRun; the final review's R3 #2),
// and strips the displaced snapshots' package rows (sub-job D, issue #14
// follow-up Tier 2), and again every hour after that, an hour after the
// previous run ended (issue #14; unconditional since 2026-11-10).
//
// AIDEV-NOTE: issue #14 (the hardening plan's final review 2, G2-I2; the
// maintainer's decision of 2026-11-05). Since DN-7 a periodic tick's
// sub-job B only retires the displaced snapshots beyond keep_displaced, and
// chunked sub-job C deletes them, 1.6-2.3x slower than main's one
// transaction; with C run once a day (the default gc.interval) and once an
// hour after a start, a sustained displacement rate above one budget's C
// capacity grew the retired backlog without bound. Re-arming the retired
// run hourly while a run or a tick's snapshot pass ends at its deadline
// gives C up to 24 budgets a day while a backlog lasts, each bounded and
// chunked as before, on this goroutine (never beside a tick): no new knob,
// goroutine or table. A tick's snapshot deadline re-arms it even when only
// A ∪ B had work left; that run then finds nothing retired and costs one
// SELECT. Never re-armed after a failure or a panic (no hourly retry
// storm: the next periodic tick retries C, as before), nor when gc.interval
// is an hour or less, whose ticks already come hourly. Updated 2026-11-09
// (issue #14 follow-up Tier 2): the retired run and a tick's snapshot pass
// run sub-job D after C, which strips the displaced snapshots'
// package_hash rows, and D's deadline re-arms the run too, so the first
// 1.1.0 start's backlog of package rows (every displaced snapshot's) drains
// hourly, not daily. A run whose C failed goes on to D (HD-15 within the
// run); it re-arms only on D's deadline, so a failing C is retried hourly
// only while D has rows left.
// Superseded 2026-11-10 (the maintainer's decision, the follow-up's final
// review I-1): the run re-arms hourly unconditionally, an hour after the
// previous run ended, whatever it left: drained, at a deadline, failed or
// panicked. Under the conditional re-arm a suite displaced several times a
// day between periodic ticks kept its displaced package_hash rows until the
// next tick (a day by default), so the table shrank about 2x, not 4x; and
// a site whose upstreams update often should not need a shorter gc.interval
// for that. On a quiet site the run finds nothing and costs one SELECT per
// sub-job. The cost, accepted: a C or D that fails at every call logs one
// WARN gc_tick_failed an hour (and counts once an hour in
// acu_gc_pass_failures_total{pass="retired"}), not one a day. The periodic
// ticks do not move the hourly schedule: a run may come minutes after a
// tick, and find little.
//
// When Enabled = false, returns immediately — the goroutine is not
// started.
func (g *GC) Run(ctx context.Context) {
	if !g.cfg.Enabled {
		return
	}
	// The startup pass first, when StartupScan left one (2026-11-11: main
	// runs the scan before the listeners and this pass after them). Its
	// failure is logged here, as main logged StartupPass's; a shutdown
	// during it is not one. The schedule below starts when it ends.
	if err := g.runStartupPass(ctx); err != nil && ctx.Err() == nil {
		g.cfg.Logger.Warn("gc startup pass failed", "err", err)
	}
	if ctx.Err() != nil {
		return
	}
	t := time.NewTimer(g.cfg.Interval)
	defer t.Stop()
	// retiredDue stays nil when gc.interval is an hour or less: no separate
	// run, since the periodic ticks, within the hour, run sub-jobs C and D
	// themselves.
	var (
		rt         *time.Timer
		retiredDue <-chan time.Time
	)
	if g.cfg.Interval > retiredRunAfterStart {
		rt = time.NewTimer(retiredRunAfterStart)
		defer rt.Stop()
		retiredDue = rt.C
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-retiredDue:
			if ctx.Err() != nil {
				return
			}
			g.retiredRun(ctx)
			rt.Reset(retiredRunAfterStart)
		case <-t.C:
			if ctx.Err() != nil {
				return
			}
			g.periodicTick(ctx)
			t.Reset(g.cfg.Interval)
		}
	}
}

// periodicTick is one periodic tick: the passes (runTick), its
// gc_run_complete line, metrics and last-run summary. A panic is
// recovered here — logged gc_panicked, where=tick — and the next tick
// comes as scheduled (deferred minor F-21: sub-job C runs in this
// goroutine since phase F). The line, the metrics and the last-run
// summary stay inside the recovered function, so a tick that panicked
// leaves acu_gc_last_run_unixtime as it was: its age is the alert. A
// panic in a pass moves the rotation past that pass (skipPanickedPass).
// It no longer reports a backlog for Run's retired run: that run comes
// hourly whatever a tick left (issue #14, 2026-11-10).
func (g *GC) periodicTick(ctx context.Context) {
	passesDone := false
	defer g.skipPanickedPass(&passesDone)
	defer g.recoverPanic("tick")
	start := time.Now()
	// Each failed pass logged its own WARN gc_tick_failed as it failed
	// (runTick's failed); the first failure, which runTick returns, is for
	// runStartupPass's caller (Run, or a test's StartupPass), so a tick
	// logs none of its own for it.
	tick, _ := g.runTick(ctx, "periodic")
	passesDone = true
	duration := time.Since(start)
	endUnix := wallclock.Unix()
	g.cfg.Logger.Info("gc_run_complete",
		"phase", "periodic",
		"blobs_reaped", tick.blobsReaped,
		"bytes_reclaimed", tick.bytesReclaimed,
		"orphan_candidates_reaped", tick.orphanCandidatesReaped,
		"displaced_reaped", tick.displacedReaped,
		"retired_reaped", tick.retiredReaped,
		"displaced_package_rows_deleted", tick.displacedPackageRowsDeleted,
		"url_path_rows_reaped", tick.urlPathRowsReaped,
		"url_path_rows_scanned", tick.urlPath.scanned,
		"url_path_rows_stamped", tick.urlPath.stamped,
		"url_path_rows_cleared", tick.urlPath.cleared,
		"url_path_duration_ms", tick.urlPathDuration.Milliseconds(),
		"snapshot_duration_ms", tick.snapshotDuration.Milliseconds(),
		"blob_duration_ms", tick.blobDuration.Milliseconds(),
		"representation_rows_pruned", tick.representationRowsPruned,
		"representation_prune_ms", tick.representationPruneDuration.Milliseconds(),
		"pool_orphans_repaired", 0,
		"pool_orphan_bytes_repaired", int64(0),
		"pool_unlink_errors", tick.poolUnlinkErrors,
		"deadline_reached", tick.deadlineReached,
		"duration_ms", duration.Milliseconds(),
	)
	emitGCMetrics(
		"periodic",
		duration.Seconds(),
		endUnix,
		tick.blobsReaped,
		tick.bytesReclaimed,
		tick.orphanCandidatesReaped,
		tick.displacedReaped,
		tick.retiredReaped,
		tick.displacedPackageRowsDeleted,
		tick.urlPathRowsReaped,
		0,
		0,
		tick.poolUnlinkErrors,
		tick.deadlineReached,
	)
	emitGCPassMetrics("periodic", tick)
	g.recordLastRun(LastRunSummary{
		Phase:                       "periodic",
		AtUnixTime:                  endUnix,
		DurationSeconds:             duration.Seconds(),
		BlobsReaped:                 tick.blobsReaped,
		BytesReclaimed:              tick.bytesReclaimed,
		OrphanCandidatesReaped:      tick.orphanCandidatesReaped,
		DisplacedReaped:             tick.displacedReaped,
		RetiredReaped:               tick.retiredReaped,
		DisplacedPackageRowsDeleted: tick.displacedPackageRowsDeleted,
		URLPathRowsReaped:           tick.urlPathRowsReaped,
		URLPathRowsScanned:          tick.urlPath.scanned,
		URLPathRowsStamped:          tick.urlPath.stamped,
		URLPathRowsCleared:          tick.urlPath.cleared,
		URLPathDurationSeconds:      tick.urlPathDuration.Seconds(),
		SnapshotDurationSeconds:     tick.snapshotDuration.Seconds(),
		BlobDurationSeconds:         tick.blobDuration.Seconds(),
		PoolOrphansRepaired:         0,
		PoolOrphanBytesRepaired:     0,
		PoolUnlinkErrors:            tick.poolUnlinkErrors,
		DeadlineReached:             tick.deadlineReached,
		FailedPasses:                tick.failedPasses,
		RepresentationRowsPruned:    tick.representationRowsPruned,
		RepresentationPruneSeconds:  tick.representationPruneDuration.Seconds(),
	})
}

// gcStore is what the url-path pass, the snapshot pass and the pool scan
// call on the cache (design note 18 of the H1 plan): a seam, so tests reach
// sub-job C's deadline and chunk size without a 10,000-row fixture
// (deferred minors F-1, F-2), make a tick or a pool-scan job panic (F-21),
// and slow the url-path pass's batches past its deadline (issue #14's
// fix), and count the blob pass's once-a-tick representation prune (the
// merge review's M-I1). New sets it to Config.Cache; the blob pass's batches
// keep Config.Cache.
type gcStore interface {
	RunURLPathGCBatch(ctx context.Context, batchSize int, ttlSeconds, holdSeconds int64, maxVersions int, afterScheme, afterHost, afterPath string) (cache.URLPathGCBatchResult, error)
	RunSnapshotGCBatch(ctx context.Context, batchSize int, staleGraceSeconds int64, keepDisplaced int) (cache.SnapshotGCBatchResult, error)
	RunRetiredSnapshotGCChunk(ctx context.Context, maxRows int) (reaped int, more bool, err error)
	RunDisplacedPackageHashChunk(ctx context.Context, maxRows int) (rows int, more bool, err error)
	HashKnown(ctx context.Context, hash string) (bool, error)
	PruneRepresentationMetadata(ctx context.Context, limit int) (int, error)
}

// skipPanickedPass is deferred by periodicTick before its recoverPanic,
// and by runStartupPass before its recoverPanicInto, so it runs after the
// recover: when the passes did not finish (a recovered panic), it moves
// the rotation past the pass in progress — g.nextPass, which runTick
// advances only when a pass completes or fails (passFailed), never when
// one panics — so a pass that panics on every tick cannot starve the
// passes after it: the blob pass keeps reclaiming disk while a sub-job's
// bug waits for its fix (review M-1 of the H1 plan).
func (g *GC) skipPanickedPass(passesDone *bool) {
	if *passesDone {
		return
	}
	g.skipPass()
}

// skipPass moves the rotation past the pass in progress (g.nextPass).
func (g *GC) skipPass() {
	switch g.nextPass {
	case urlPathPass:
		g.nextPass = snapshotPass
	case snapshotPass:
		g.nextPass = blobPass
	default:
		g.nextPass = urlPathPass
	}
}

// passFailed is runTick's step for a pass that failed: it moves the
// rotation past that pass, as skipPanickedPass does for a panic, unless the
// GC's context has ended (a shutdown stopped the pass; it did not fail),
// and returns the pass's error, which runTick returns when no earlier pass
// of the tick failed.
//
// AIDEV-NOTE: the H3 final review R3 #5 and ruling HD-15 (the GC and
// upstream hardening plan's final review, G-I1 and G-M5). Before R3 #5
// only a panic moved the rotation: a pass that returned an error at every
// tick (a fault limited to its tables) was retried first at every tick,
// and the passes after it — the blob pass, which reclaims disk — never
// ran; the only sign was WARN gc_tick_failed per tick. R3 #5 moved the
// rotation past a failed pass but still ended the tick there, so a failing
// pass ran every other tick, and a daemon restarted more often than
// gc.interval, whose every start begins the rotation afresh and which
// never runs a periodic tick, still never ran the passes after it. Since
// HD-15 a failed pass no longer ends its tick: runTick goes on to the
// passes after it, each under its own deadline, in the startup pass and in a
// periodic tick alike, and returns the first failure; a failing pass is
// retried at every tick, and the others keep their full cadence. Safe:
// each pass re-checks its own predicates (the blob pass's reachability
// holds whatever the url-path or snapshot pass did or failed to do). Each
// failed pass logs its own WARN gc_tick_failed, as it fails (runTick's
// failed), and a startup pass in which one failed still writes its
// gc_run_complete line (StartupPass): the fix wave's follow-up. A failed
// url-path pass resumes, next time, at the cursor its last deadline saved.
func (g *GC) passFailed(ctx context.Context, err error) error {
	if ctx.Err() == nil {
		g.skipPass()
	}
	return err
}

// notStarted reports whether err is a write the writer goroutine never
// started before the pass's start deadline (cache.ErrWriteNotStarted): the
// op never ran, and the pass's deadline has passed (submitWrite gives up at
// its start deadline, never before). A pass takes its deadline branch for
// it, never passFailed (issue #14 Tier 1a).
//
// AIDEV-NOTE: the start-deadline pattern (issue #14 Tier 1a, G2-RR1-M1,
// 2026-11-09). Each pass derives wctx := cache.WithWriteStartDeadline(ctx,
// deadline) once, from its own pass deadline (retiredRun: its own), and
// gives wctx to every store or cache write it makes; at each call site
// notStarted(err) takes the pass's deadline branch (gc_tick_deadline_reached,
// deadlineHit = true, no error). Check it on the store call's own error,
// before the pass wraps it; the cache returns submitWrite's error unwrapped
// and the passes wrap with %w, so errors.Is holds either way. A new write
// call site (sub-job D, say) must follow it, or a write queued behind the
// writer runs past its pass's deadline again.
func notStarted(err error) bool {
	return errors.Is(err, cache.ErrWriteNotStarted)
}

// recoverPanic is deferred by each unit of work of GC's goroutines — a
// periodic tick, a pool-scan job, the pool-scan producer: it logs a
// recovered panic as ERROR gc_panicked (where, panic, and the stack the
// first time per where for this GC, so for the process) and lets its
// goroutine go on (deferred minor F-21; the overview's rule for
// internal/gc).
//
// AIDEV-NOTE: recover works only because this method IS the deferred
// function (defer g.recoverPanic(where)); a wrapping closure would make
// recover return nil and the panic would kill the daemon. A logger that
// panics in turn is contained too: that panic is dropped.
func (g *GC) recoverPanic(where string) {
	v := recover()
	if v == nil {
		return
	}
	g.logGCPanic(where, v)
}

// recoverPanicInto is recoverPanic for a function that returns an error
// (StartupScan, runStartupPass): it also sets *err, so the caller sees the panic as a
// failure. Like recoverPanic it works only as the deferred function itself
// (defer g.recoverPanicInto(where, &err)).
func (g *GC) recoverPanicInto(where string, err *error) {
	v := recover()
	if v == nil {
		return
	}
	*err = fmt.Errorf("gc %s: panicked: %v", where, v)
	g.logGCPanic(where, v)
}

// logGCPanic logs a recovered panic as ERROR gc_panicked; a logger that
// panics in turn is contained: that panic is dropped.
func (g *GC) logGCPanic(where string, v any) {
	defer func() { _ = recover() }()
	attrs := []any{"where", where, "panic", fmt.Sprint(v)}
	if _, seen := g.panicStacks.LoadOrStore(where, true); !seen {
		attrs = append(attrs, "stack", string(debug.Stack()))
	}
	g.cfg.Logger.Error("gc_panicked", attrs...)
}

// tickResult bundles one tick's per-pass outcomes for the
// gc_run_complete line.
type tickResult struct {
	blobsReaped            int
	bytesReclaimed         int64
	orphanCandidatesReaped int
	displacedReaped        int
	retiredReaped          int
	// displacedPackageRowsDeleted: sub-job D's package_hash rows (issue #14
	// follow-up, Tier 2).
	displacedPackageRowsDeleted int
	urlPathRowsReaped           int
	poolUnlinkErrors            int
	deadlineReached             bool
	urlPath                     urlPathResult
	urlPathDuration             time.Duration
	snapshotDuration            time.Duration
	blobDuration                time.Duration
	// representationRowsPruned and representationPruneDuration are the
	// blob pass's representation prune's (blobResult; issue #14 follow-up,
	// Tier 1c).
	representationRowsPruned    int
	representationPruneDuration time.Duration
	// failedPasses: LastRunSummary.FailedPasses (runTick's failedPass).
	failedPasses []string
}

// tickDeadlines are one tick's three pass deadlines (passDeadlines).
type tickDeadlines struct {
	urlPath, snapshot, blob time.Time
}

// passDeadlines splits one tick's budget, T = start + budget
// (gc.max_tick_duration), into its passes' deadlines, with f = budget/5:
// the url-path pass ends by T-2f, the snapshot pass by T-f and the blob
// pass by T. A pass that begins the tick may use the time up to its own
// deadline; the passes after it keep at least their floors (f each).
//
// AIDEV-NOTE: issue #14 (the hardening plan's final review 2, G2-I1; the
// maintainer's decision of 2026-11-05). One shared deadline let the first
// pass with a backlog spend it all, and its deadline ended the tick, so the
// blob pass, the only one that frees disk, never ran while the snapshot
// pass's backlog lasted. With the floors, and a deadline that moves the
// rotation on (runTick), every pass gets a turn in every tick. The floors
// are checked between batches, so one batch that overruns eats into the
// next pass's floor; batches are far shorter than f at any sane budget.
// Since 2026-11-09 (issue #14 Tier 1a, G2-RR1-M1) a batch's wait behind
// the writer no longer counts toward that overrun: each pass's deadline is
// also its writes' start deadline (notStarted), so the overrun is bounded
// by the one op the writer had already started at the deadline.
// If url_path scans need more than 3f, a smaller f (budget/10) is the dial.
func passDeadlines(start time.Time, budget time.Duration) tickDeadlines {
	end := start.Add(budget)
	f := budget / 5
	return tickDeadlines{urlPath: end.Add(-2 * f), snapshot: end.Add(-f), blob: end}
}

// runTick resumes ordered URL-path, snapshot, and blob passes, each under
// its own deadline in the tick's budget (passDeadlines). A completed pass
// is not repeated until all following passes have drained, even if that
// takes multiple ticks. A url-path or snapshot pass that hits its deadline
// does not end the tick (issue #14): the rotation moves past it, as for a
// failed pass, and the passes after it run under their own deadlines; the
// url-path pass resumes at its saved cursor, the snapshot pass re-selects,
// at the next tick that reaches them. A blob pass that hits its deadline
// leaves the rotation at the blob pass, so the next tick runs it first,
// with the whole budget. Each pass's deadline is also the start deadline of
// its writes (cache.WithWriteStartDeadline): a write still queued behind the
// writer goroutine at the deadline never runs, and the pass ends there as at
// its deadline, never as a failure (notStarted). A pass that fails while ctx
// lives does not end the tick (ruling HD-15): it logs WARN gc_tick_failed
// (the snapshot pass, one per failed sub-job, itself: subJobRun),
// passFailed moves the rotation past it, as if it had completed, and the
// passes after it run; runTick returns the tick's first failure. A pass that
// fails once ctx has ended (a shutdown) ends the tick at once, the rotation
// as it is, and logs nothing.
//
// AIDEV-NOTE: issue #14 (the hardening plan's final review 2, G2-I1; the
// maintainer's decision of 2026-11-05) extends ruling HD-15 to deadlines.
// Until then a deadline ended the tick with the rotation at the pass that
// ran out, so while the snapshot pass had more work than one budget (sub-job
// C's backlog, say) every tick ended in it, and neither the blob pass, the
// only one that frees disk, nor the url-path pass ran; at a start, whose
// url-path cursor is empty (it lives in memory), a scan longer than the
// budget kept every start from the passes after it. Safe for HD-15's
// reason: each pass re-checks its own predicates (the blob pass's
// reachability holds whatever the passes before it did or did not finish).
// A tick that reaches a deadline still reports deadline_reached; for a busy
// cache that is now a steady state while the backlog drains, not a sign
// that nothing was freed. Updated 2026-11-09 for issue #14 Tier 1a
// (G2-RR1-M1): the deadlines are checked between batches, so a batch
// submitted just before its pass's deadline but queued behind the writer
// (adoption commits, a large transaction) used to wait there, run late, and
// eat the next pass's share; the blob pass could then run past the tick's
// budget. Now each pass gives its writes its deadline as their start
// deadline (cache.WithWriteStartDeadline; notStarted's AIDEV-NOTE): a batch
// can no longer overrun its pass while it waits behind the writer, and the
// overrun G2-RR1-M1 describes is bounded by one op the writer had already
// started at the deadline (a started op is never interrupted).
// cache.ErrWriteNotStarted is the deadline (gc_tick_deadline_reached,
// deadline_reached, the rotation moved as at any deadline), never a
// failure: no gc_tick_failed and no passFailed.
//
// `phase` is phaseStartup ("startup", runStartupPass) or "periodic"
// (periodicTick); both run on Run's goroutine.
// It labels the gc_tick_deadline_reached events, so an operator can
// correlate; it changes no behaviour of the snapshot pass. Until 2026-11-09
// (issue #14 follow-up Tier 1b) it selected whether B deleted the displaced
// snapshots (phaseStartup) or retired them for C to delete (DN-7 of the GC
// and upstream hardening plan, ruling HD-7); B now always retires them, and
// both phases run sub-job C (runSnapshotPass; ruling PF-15, re-ruled for
// issue #14).
func (g *GC) runTick(ctx context.Context, phase string) (tickResult, error) {
	deadlines := passDeadlines(time.Now(), g.cfg.MaxTickDuration)

	var (
		res tickResult
		// firstErr is the tick's first failure, what runTick returns.
		firstErr error
	)
	// recordFailure takes a pass that failed with err: passFailed moves the
	// rotation past it unless ctx has ended, and err is kept unless an
	// earlier pass of this tick failed first. It reports whether the tick
	// goes on: not once ctx has ended, which never comes undone, so a tick
	// whose rotation passFailed left as it was always ends there. It logs
	// and counts nothing: failed does, for the passes that do not log their
	// own (the snapshot pass logs and counts its own: subJobRun), since a
	// failure is counted where it is logged and ctx may end in between.
	recordFailure := func(err error) bool {
		e := g.passFailed(ctx, err)
		if firstErr == nil {
			firstErr = e
		}
		return ctx.Err() == nil
	}
	// failedPass names a pass whose failure was logged in the run's
	// FailedPasses, once (issue #14 follow-up, Tier 1c).
	failedPass := func(name string) {
		if !slices.Contains(res.failedPasses, name) {
			res.failedPasses = append(res.failedPasses, name)
		}
	}
	// failed is recordFailure, and, while ctx lives, the pass's WARN
	// gc_tick_failed and its count under name (acu_gc_pass_failures_total,
	// FailedPasses): the url-path and blob passes'.
	//
	// AIDEV-NOTE: passFailed's test: a pass stopped by the end of ctx (a
	// shutdown) did not fail, whatever its error's shape, and logs nothing.
	// Each pass that fails logs its own line here, as it fails, in the
	// startup pass and a periodic tick alike — and nowhere else, so each
	// failure is logged once — so a second failed pass is not hidden behind
	// the first, nor a failure behind a shutdown later in the tick (ruling
	// HD-15's follow-up in the hardening plan's final fix wave). runTick
	// returns the first for runStartupPass's caller (Run), which logs it
	// once more, as the startup pass's failure, unless a shutdown followed. Updated 2026-11-09 (issue #14
	// follow-up Tier 2): the snapshot pass is the exception. Its sub-jobs
	// (A ∪ B, C, D) go on past a failed one, each logging its own line as it
	// fails (subJobRun's AIDEV-NOTE), so the pass returns its first failure
	// already logged, and runTick takes it with recordFailure alone: still
	// one line per failure. Every failed pass, logged here or by itself,
	// goes through recordFailure.
	//
	// AIDEV-NOTE: issue #14 follow-up Tier 1c, 2026-11-09. A failure is
	// counted (acu_gc_pass_failures_total{pass}, FailedPasses) exactly when
	// its gc_tick_failed line is logged, so the counter and the lines always
	// agree: here for the url-path and blob passes; for the snapshot pass,
	// subJobRun.next counts at its first logged line, and runTick names the
	// pass from snap.failureLogged before recordFailure, so a logged failure
	// stays counted and named even when a shutdown lands in a later sub-job
	// and recordFailure finds ctx ended (the controller's ruling on Task 5's
	// review). A pass that only a shutdown stopped logged nothing and counts
	// nothing; a write never started (notStarted) never reaches here.
	failed := func(name string, err error) bool {
		if !recordFailure(err) {
			return false
		}
		g.cfg.Logger.Warn("gc_tick_failed", "err", err)
		gcPassFailuresTotal.Inc(name)
		failedPass(name)
		return true
	}

	// URL-path TTL pass — runs first so any refcount decrements
	// land before the same-tick blob pass evaluates reachability.
	// Short-circuits when URLPathTTL is 0 (operator disabled the
	// pass).
	if g.nextPass == urlPathPass {
		if ttl := int64(g.cfg.URLPathTTL.Seconds()); ttl > 0 {
			hold := int64(g.cfg.HoldWindow.Seconds())
			maxV := g.cfg.MaxVersionsPerPackage
			if maxV < 1 {
				// Match the prefetch-side fallback (freshness NewAdopter) so the
				// two consumers of keepNewestNVersionSet never disagree on N when
				// the value reaches a consumer unset. Production always passes the
				// config default (3, Validate()-enforced >= 1); this is the
				// defense-in-depth floor for direct/test construction.
				maxV = 3
			}
			start := time.Now()
			urls, urlPathDeadline, err := g.runURLPathPass(ctx, deadlines.urlPath, phase, ttl, hold, maxV)
			res.urlPathDuration = time.Since(start)
			res.urlPath = urls
			res.urlPathRowsReaped += urls.deleted
			if urlPathDeadline {
				res.deadlineReached = true
			}
			switch {
			case err != nil: // passFailed moves the rotation to the snapshot pass
				if !failed("url_path", err) {
					return res, firstErr
				}
			default: // drained, or its deadline saved the cursor: the tick goes on (issue #14)
				g.nextPass = snapshotPass
			}
		} else {
			g.nextPass = snapshotPass // the pass is off
		}
	}

	// Snapshot pass.
	if g.nextPass == snapshotPass {
		start := time.Now()
		snap, snapDeadline, err := g.runSnapshotPass(ctx, deadlines.snapshot, phase)
		res.snapshotDuration = time.Since(start)
		res.orphanCandidatesReaped += snap.batch.OrphanReaped
		res.displacedReaped += snap.batch.DisplacedReaped
		res.retiredReaped += snap.retired
		res.displacedPackageRowsDeleted += snap.packageRowsStripped
		if snapDeadline {
			res.deadlineReached = true
		}
		if snap.failureLogged { // counted by subJobRun.next as it logged it; named here whatever ctx says now
			failedPass("snapshot")
		}
		switch {
		case err != nil: // passFailed moves the rotation to the blob pass; the pass logged its failures
			if !recordFailure(err) {
				return res, firstErr
			}
		default: // drained, or its deadline left work for the next tick: the tick goes on (issue #14)
			g.nextPass = blobPass
		}
	}

	// Blob pass, until the tick's end: at least the budget's last fifth.
	// When it hits its deadline the rotation stays here, so the next tick
	// resumes the blob pass first.
	start := time.Now()
	blob, blobDeadline, blobUnlinkErrs, err := g.runBlobPass(ctx, deadlines.blob, phase)
	res.blobDuration = time.Since(start)
	res.blobsReaped += blob.count
	res.bytesReclaimed += blob.bytes
	res.representationRowsPruned += blob.pruned
	res.representationPruneDuration += blob.pruneDuration
	res.poolUnlinkErrors += blobUnlinkErrs
	if blobDeadline {
		res.deadlineReached = true
	}
	switch {
	case err != nil: // the last pass: the tick ends either way
		failed("blob", err)
	case !blobDeadline:
		g.nextPass = urlPathPass
	}

	return res, firstErr
}

// blobResult bundles the blob pass's reap counts and its representation
// prune's outcome.
type blobResult struct {
	count int
	bytes int64
	// pruned and pruneDuration are the representation prune's: the dates
	// it released and how long its call took, its wait for the writer
	// included; both 0 when the pass ran no prune (issue #14 follow-up,
	// Tier 1c).
	pruned        int
	pruneDuration time.Duration
}

// poolDir returns pool/<hash[:2]>/<hash> the way Cache.BlobPath does,
// without going through Cache.BlobPath (which panics on a malformed
// hash — fine for request-path callers, but blob GC's hashes already
// came out of a SQL CHECK-validated column and we don't want a panic
// path to abort the unlink loop on a hypothetical DB corruption).
func (g *GC) poolPath(hash string) string {
	if len(hash) < 2 {
		return filepath.Join(g.cfg.Cache.Dir(), "pool", "_invalid", hash)
	}
	return filepath.Join(g.cfg.Cache.Dir(), "pool", hash[:2], hash)
}
