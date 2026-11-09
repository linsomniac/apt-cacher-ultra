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
	// goroutine work) and StartupPass (no startup pool scan / no
	// startup GC pass). Main emits gc_disabled Warn before
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

// GC is the orchestrator. Run() owns the periodic tick loop;
// StartupPass() runs the §4.2 step 5 + step 6 startup sequence (pool
// scan + one-shot GC pass) blocking before listeners come up.
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
	// this, after the sequential startup pass.
	nextPass pass

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
	// only by the tick goroutine (StartupPass runs before Run starts), so
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

// phaseStartup is runTick's phase for StartupPass's one-shot pass, the
// pass main runs before any listener serves. Its snapshot pass's sub-job
// B deletes the displaced snapshots beyond keep_displaced; the periodic
// ticks' ("periodic") B only retires those snapshots, for C to delete (DN-7
// of the GC and upstream hardening plan). Both run sub-job C (ruling PF-15,
// re-ruled for issue #14: runSnapshotPass).
const phaseStartup = "startup"

// LastRunSummary is the SPEC5 §9.6 / §9.7.8 in-memory captured copy
// of the most recently completed gc_run_complete payload. The
// status-page handler renders this; the §9.7.6 refresher mirrors
// fields into Phase 5 GC counters/gauges.
type LastRunSummary struct {
	Phase                   string // "startup" | "periodic"
	AtUnixTime              int64
	DurationSeconds         float64
	BlobsReaped             int
	BytesReclaimed          int64
	OrphanCandidatesReaped  int
	DisplacedReaped         int
	RetiredReaped           int // snapshot-GC sub-job C (spec C9)
	URLPathRowsReaped       int
	URLPathRowsScanned      int
	URLPathRowsStamped      int
	URLPathRowsCleared      int
	URLPathDurationSeconds  float64
	SnapshotDurationSeconds float64
	BlobDurationSeconds     float64
	PoolOrphansRepaired     int
	PoolOrphanBytesRepaired int64
	PoolUnlinkErrors        int
	DeadlineReached         bool
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
	return *g.lastRun, true
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

// StartupPass runs the SPEC4 §4.2 step 5 (pool/ orphan scan) and step
// 6 (one-shot GC pass) sequence. Blocks until both complete or ctx is
// cancelled. Returns the first error encountered: a failed pool scan's,
// which ends it before the one-shot pass, or the first failed pass's, once
// the passes after it have run too (runTick, ruling HD-15: a failed pass
// no longer ends the one-shot pass, so a daemon restarted more often than
// gc.interval, which never runs a periodic tick, still runs every pass at
// every start) and the pass has written its gc_run_complete line, metrics
// and last-run summary, as a periodic tick does: each failed pass logged
// its own gc_tick_failed line (runTick), and main logs the returned first
// failure as the startup pass's (the fix wave's follow-up of HD-15).
// Partial progress is preserved (DB rows / pool files reaped before cancel
// are real). The one-shot pass runs snapshot-GC sub-job C (spec C9's
// retired snapshots) within its snapshot deadline, so a start with a
// retired backlog may wait up to max_tick_duration before the listeners
// open, the bound ruling HD-7 accepts (ruling PF-15, re-ruled for issue
// #14: runSnapshotPass); what its budget leaves, Run's retired run (an hour
// after the start when gc.interval is longer, hourly while a backlog lasts)
// and the periodic ticks delete.
//
// Order matters: pool scan runs first so its
// `gc_pool_orphans_repaired` count reflects only pre-existing orphan
// files (not files just created by the same-tick blob GC pass).
//
// When Enabled = false, returns nil immediately.
//
// A panic is recovered and returned as an error (gc_panicked,
// where=startup), which main logs before it serves: this pass runs on
// main's goroutine before any listener, and a panic that persisted data
// triggers would otherwise recur at every start (review I-1 of the H1
// plan). A panic in its passes moves the rotation past the pass in
// progress, as a periodic tick's does, so the first periodic tick does not
// begin with the pass that panicked (re-review m-4).
func (g *GC) StartupPass(ctx context.Context) (err error) {
	passesDone := true // a panic before runTick leaves the rotation as it is
	defer g.skipPanickedPass(&passesDone)
	defer g.recoverPanicInto("startup", &err)
	if !g.cfg.Enabled {
		return nil
	}
	start := time.Now()

	// Step 5: pool/ orphan scan.
	scan, err := g.runPoolScan(ctx)
	if err != nil {
		return fmt.Errorf("gc startup: pool scan: %w", err)
	}

	// Step 6: one-shot GC pass, sub-job C included (ruling PF-15, re-ruled
	// for issue #14: runSnapshotPass). A failed pass is returned after the
	// line, the metrics and the summary below, which report what the passes
	// that ran did, as a periodic tick's do.
	passesDone = false
	tick, tickErr := g.runTick(ctx, phaseStartup)
	passesDone = true

	duration := time.Since(start)
	endUnix := wallclock.Unix()
	g.cfg.Logger.Info("gc_run_complete",
		"phase", "startup",
		"blobs_reaped", tick.blobsReaped,
		"bytes_reclaimed", tick.bytesReclaimed,
		"orphan_candidates_reaped", tick.orphanCandidatesReaped,
		"displaced_reaped", tick.displacedReaped,
		"retired_reaped", tick.retiredReaped,
		"url_path_rows_reaped", tick.urlPathRowsReaped,
		"url_path_rows_scanned", tick.urlPath.scanned,
		"url_path_rows_stamped", tick.urlPath.stamped,
		"url_path_rows_cleared", tick.urlPath.cleared,
		"url_path_duration_ms", tick.urlPathDuration.Milliseconds(),
		"snapshot_duration_ms", tick.snapshotDuration.Milliseconds(),
		"blob_duration_ms", tick.blobDuration.Milliseconds(),
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
		tick.urlPathRowsReaped,
		scan.orphansRepaired,
		scan.orphanBytesRepaired,
		tick.poolUnlinkErrors+scan.unlinkErrors,
		tick.deadlineReached,
	)
	emitGCPassMetrics("startup", tick)
	g.recordLastRun(LastRunSummary{
		Phase:                   "startup",
		AtUnixTime:              endUnix,
		DurationSeconds:         duration.Seconds(),
		BlobsReaped:             tick.blobsReaped,
		BytesReclaimed:          tick.bytesReclaimed,
		OrphanCandidatesReaped:  tick.orphanCandidatesReaped,
		DisplacedReaped:         tick.displacedReaped,
		RetiredReaped:           tick.retiredReaped,
		URLPathRowsReaped:       tick.urlPathRowsReaped,
		URLPathRowsScanned:      tick.urlPath.scanned,
		URLPathRowsStamped:      tick.urlPath.stamped,
		URLPathRowsCleared:      tick.urlPath.cleared,
		URLPathDurationSeconds:  tick.urlPathDuration.Seconds(),
		SnapshotDurationSeconds: tick.snapshotDuration.Seconds(),
		BlobDurationSeconds:     tick.blobDuration.Seconds(),
		PoolOrphansRepaired:     scan.orphansRepaired,
		PoolOrphanBytesRepaired: scan.orphanBytesRepaired,
		PoolUnlinkErrors:        tick.poolUnlinkErrors + scan.unlinkErrors,
		DeadlineReached:         tick.deadlineReached,
	})
	if tickErr != nil {
		return fmt.Errorf("gc startup: tick: %w", tickErr)
	}
	return nil
}

// Run owns the periodic tick goroutine. Returns when ctx is cancelled.
// Each tick fires `gc.interval` after the previous tick completed. A
// long tick cannot leave a pending timer firing that starts work again
// immediately, even when an individual batch exceeds the tick budget.
// When `gc.interval` is longer than an hour, Run also deletes the retired
// snapshots an hour after the start (retiredRun; the final review's R3 #2),
// and again an hour after any retired run or periodic tick whose sub-job C
// stopped at its deadline with retired snapshots left (issue #14), until
// one drains them.
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
// is an hour or less, whose ticks already come hourly.
//
// When Enabled = false, returns immediately — the goroutine is not
// started.
func (g *GC) Run(ctx context.Context) {
	if !g.cfg.Enabled {
		return
	}
	t := time.NewTimer(g.cfg.Interval)
	defer t.Stop()
	// nil when gc.interval is an hour or less: no separate run, since the
	// periodic ticks, within the hour, run sub-job C themselves.
	var (
		rt         *time.Timer
		retiredDue <-chan time.Time
	)
	if g.cfg.Interval > retiredRunAfterStart {
		rt = time.NewTimer(retiredRunAfterStart)
		defer rt.Stop()
		retiredDue = rt.C
	}
	// rearmRetired arms the retired run an hour from now, unless it is off
	// or already armed.
	rearmRetired := func() {
		if rt == nil || retiredDue != nil {
			return
		}
		rt.Reset(retiredRunAfterStart)
		retiredDue = rt.C
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-retiredDue:
			retiredDue = nil
			if ctx.Err() != nil {
				return
			}
			if g.retiredRun(ctx) {
				rearmRetired()
			}
		case <-t.C:
			if ctx.Err() != nil {
				return
			}
			if g.periodicTick(ctx) {
				rearmRetired()
			}
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
// It reports snapshotBacklog, for Run's retired run (issue #14): its
// snapshot pass stopped at its deadline with work left; false when a pass
// panicked.
func (g *GC) periodicTick(ctx context.Context) (snapshotBacklog bool) {
	passesDone := false
	defer g.skipPanickedPass(&passesDone)
	defer g.recoverPanic("tick")
	start := time.Now()
	// Each failed pass logged its own WARN gc_tick_failed as it failed
	// (runTick's failed); the first failure, which runTick returns, is for
	// StartupPass's caller, so a tick logs none of its own for it.
	tick, _ := g.runTick(ctx, "periodic")
	passesDone = true
	snapshotBacklog = tick.snapshotDeadlineReached
	duration := time.Since(start)
	endUnix := wallclock.Unix()
	g.cfg.Logger.Info("gc_run_complete",
		"phase", "periodic",
		"blobs_reaped", tick.blobsReaped,
		"bytes_reclaimed", tick.bytesReclaimed,
		"orphan_candidates_reaped", tick.orphanCandidatesReaped,
		"displaced_reaped", tick.displacedReaped,
		"retired_reaped", tick.retiredReaped,
		"url_path_rows_reaped", tick.urlPathRowsReaped,
		"url_path_rows_scanned", tick.urlPath.scanned,
		"url_path_rows_stamped", tick.urlPath.stamped,
		"url_path_rows_cleared", tick.urlPath.cleared,
		"url_path_duration_ms", tick.urlPathDuration.Milliseconds(),
		"snapshot_duration_ms", tick.snapshotDuration.Milliseconds(),
		"blob_duration_ms", tick.blobDuration.Milliseconds(),
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
		tick.urlPathRowsReaped,
		0,
		0,
		tick.poolUnlinkErrors,
		tick.deadlineReached,
	)
	emitGCPassMetrics("periodic", tick)
	g.recordLastRun(LastRunSummary{
		Phase:                   "periodic",
		AtUnixTime:              endUnix,
		DurationSeconds:         duration.Seconds(),
		BlobsReaped:             tick.blobsReaped,
		BytesReclaimed:          tick.bytesReclaimed,
		OrphanCandidatesReaped:  tick.orphanCandidatesReaped,
		DisplacedReaped:         tick.displacedReaped,
		RetiredReaped:           tick.retiredReaped,
		URLPathRowsReaped:       tick.urlPathRowsReaped,
		URLPathRowsScanned:      tick.urlPath.scanned,
		URLPathRowsStamped:      tick.urlPath.stamped,
		URLPathRowsCleared:      tick.urlPath.cleared,
		URLPathDurationSeconds:  tick.urlPathDuration.Seconds(),
		SnapshotDurationSeconds: tick.snapshotDuration.Seconds(),
		BlobDurationSeconds:     tick.blobDuration.Seconds(),
		PoolOrphansRepaired:     0,
		PoolOrphanBytesRepaired: 0,
		PoolUnlinkErrors:        tick.poolUnlinkErrors,
		DeadlineReached:         tick.deadlineReached,
	})
	return snapshotBacklog
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
	RunSnapshotGCBatch(ctx context.Context, batchSize int, staleGraceSeconds int64, keepDisplaced int, retireDisplaced bool) (cache.SnapshotGCBatchResult, error)
	RunRetiredSnapshotGCChunk(ctx context.Context, maxRows int) (reaped int, more bool, err error)
	HashKnown(ctx context.Context, hash string) (bool, error)
	PruneRepresentationMetadata(ctx context.Context, limit int) (int, error)
}

// skipPanickedPass is deferred by periodicTick before its recoverPanic,
// and by StartupPass before its recoverPanicInto, so it runs after the
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
// (StartupPass): it also sets *err, so the caller sees the panic as a
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
	urlPathRowsReaped      int
	poolUnlinkErrors       int
	deadlineReached        bool
	// snapshotDeadlineReached: the snapshot pass stopped at its deadline
	// with work left (A ∪ B, or sub-job C); Run re-arms its retired run.
	snapshotDeadlineReached bool
	urlPath                 urlPathResult
	urlPathDuration         time.Duration
	snapshotDuration        time.Duration
	blobDuration            time.Duration
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
// with the whole budget. A pass that fails while ctx lives does not end the
// tick (ruling HD-15): it logs WARN gc_tick_failed, passFailed moves the
// rotation past it, as if it had completed, and the passes after it run;
// runTick returns the tick's first failure. A pass that fails once ctx has
// ended (a shutdown) ends the tick at once, the rotation as it is, and logs
// nothing.
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
// that nothing was freed.
//
// `phase` is phaseStartup ("startup", StartupPass) or "periodic" (Run).
// It labels the gc_tick_deadline_reached events, so an operator can
// correlate, and it selects behaviour: in phaseStartup the snapshot pass's
// sub-job B deletes the displaced snapshots; in any other phase its B
// retires them for C to delete (DN-7 of the GC and upstream hardening
// plan). Both run sub-job C (runSnapshotPass; ruling PF-15, re-ruled for
// issue #14).
func (g *GC) runTick(ctx context.Context, phase string) (tickResult, error) {
	deadlines := passDeadlines(time.Now(), g.cfg.MaxTickDuration)

	var (
		res tickResult
		// firstErr is the tick's first failure, what runTick returns.
		firstErr error
	)
	// failed takes a pass that failed with err: passFailed moves the
	// rotation past it unless ctx has ended, err is kept unless an earlier
	// pass of this tick failed first, and, while ctx lives, the pass logs
	// WARN gc_tick_failed. It reports whether the tick goes on: not once ctx
	// has ended, which never comes undone, so a tick whose rotation
	// passFailed left as it was always ends there.
	//
	// AIDEV-NOTE: passFailed's test: a pass stopped by the end of ctx (a
	// shutdown) did not fail, whatever its error's shape, and logs nothing.
	// Each pass that fails logs its own line here, as it fails, in the
	// startup pass and a periodic tick alike — and nowhere else, so each
	// failure is logged once — so a second failed pass is not hidden behind
	// the first, nor a failure behind a shutdown later in the tick (ruling
	// HD-15's follow-up in the hardening plan's final fix wave). runTick
	// returns the first for StartupPass's caller, which logs it once more,
	// as the startup pass's failure.
	failed := func(err error) bool {
		e := g.passFailed(ctx, err)
		if firstErr == nil {
			firstErr = e
		}
		if ctx.Err() != nil {
			return false
		}
		g.cfg.Logger.Warn("gc_tick_failed", "err", e)
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
				if !failed(err) {
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
		if snapDeadline {
			res.deadlineReached = true
			res.snapshotDeadlineReached = true
		}
		switch {
		case err != nil: // passFailed moves the rotation to the blob pass
			if !failed(err) {
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
	res.poolUnlinkErrors += blobUnlinkErrs
	if blobDeadline {
		res.deadlineReached = true
	}
	switch {
	case err != nil: // the last pass: the tick ends either way
		failed(err)
	case !blobDeadline:
		g.nextPass = urlPathPass
	}

	return res, firstErr
}

// blobResult bundles the blob pass's reap counts.
type blobResult struct {
	count int
	bytes int64
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
