// Package gc implements the SPEC4 Phase 4 garbage collector: orphan
// blobs, orphan/displaced suite_snapshot rows, and pool/ orphan files.
//
// Architecture: a single dedicated goroutine runs the periodic tick
// loop (gc.interval cadence). Each tick computes a wall-clock deadline
// (gc.max_tick_duration) once at the top, then runs the snapshot pass
// followed by the blob pass; both passes share that one deadline.
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
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
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

	// MaxTickDuration is the per-tick wall-clock budget shared
	// across both passes (gc.max_tick_duration). Must be > 0 when
	// Enabled.
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
}

// GC is the orchestrator. Run() owns the periodic tick loop;
// StartupPass() runs the §4.2 step 5 + step 6 startup sequence (pool
// scan + one-shot GC pass) blocking before listeners come up.
type GC struct {
	cfg Config
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
		return &GC{cfg: cfg}, nil
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
	return &GC{cfg: cfg}, nil
}

// StartupPass runs the SPEC4 §4.2 step 5 (pool/ orphan scan) and step
// 6 (one-shot GC pass) sequence. Blocks until both complete or ctx is
// cancelled. Returns the first error encountered; partial progress is
// preserved (DB rows / pool files reaped before cancel are real).
//
// Order matters: pool scan runs first so its
// `gc_pool_orphans_repaired` count reflects only pre-existing orphan
// files (not files just created by the same-tick blob GC pass).
//
// When Enabled = false, returns nil immediately.
func (g *GC) StartupPass(ctx context.Context) error {
	if !g.cfg.Enabled {
		return nil
	}
	start := time.Now()

	// Step 5: pool/ orphan scan.
	scan, err := g.runPoolScan(ctx)
	if err != nil {
		return fmt.Errorf("gc startup: pool scan: %w", err)
	}

	// Step 6: one-shot GC pass.
	tick, err := g.runTick(ctx, "startup")
	if err != nil {
		return fmt.Errorf("gc startup: tick: %w", err)
	}

	g.cfg.Logger.Info("gc_run_complete",
		"phase", "startup",
		"blobs_reaped", tick.blobsReaped,
		"bytes_reclaimed", tick.bytesReclaimed,
		"orphan_candidates_reaped", tick.orphanCandidatesReaped,
		"displaced_reaped", tick.displacedReaped,
		"pool_orphans_repaired", scan.orphansRepaired,
		"pool_orphan_bytes_repaired", scan.orphanBytesRepaired,
		"pool_unlink_errors", tick.poolUnlinkErrors+scan.unlinkErrors,
		"deadline_reached", tick.deadlineReached,
		"duration_ms", time.Since(start).Milliseconds(),
	)
	return nil
}

// Run owns the periodic tick goroutine. Returns when ctx is cancelled.
// Each tick fires `gc.interval` after the previous tick *started* (a
// long tick simply pushes the next firing back; we don't queue
// missed ticks).
//
// When Enabled = false, returns immediately — the goroutine is not
// started.
func (g *GC) Run(ctx context.Context) {
	if !g.cfg.Enabled {
		return
	}
	t := time.NewTicker(g.cfg.Interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			start := time.Now()
			tick, err := g.runTick(ctx, "periodic")
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				g.cfg.Logger.Warn("gc_tick_failed", "err", err)
			}
			g.cfg.Logger.Info("gc_run_complete",
				"phase", "periodic",
				"blobs_reaped", tick.blobsReaped,
				"bytes_reclaimed", tick.bytesReclaimed,
				"orphan_candidates_reaped", tick.orphanCandidatesReaped,
				"displaced_reaped", tick.displacedReaped,
				"pool_orphans_repaired", 0,
				"pool_orphan_bytes_repaired", int64(0),
				"pool_unlink_errors", tick.poolUnlinkErrors,
				"deadline_reached", tick.deadlineReached,
				"duration_ms", time.Since(start).Milliseconds(),
			)
		}
	}
}

// tickResult bundles one tick's per-pass outcomes for the
// gc_run_complete line.
type tickResult struct {
	blobsReaped            int
	bytesReclaimed         int64
	orphanCandidatesReaped int
	displacedReaped        int
	poolUnlinkErrors       int
	deadlineReached        bool
}

// runTick executes one snapshot pass + one blob pass under a single
// shared deadline computed at tick start (SPEC4 §9.6.1). Snapshot
// pass first; blob pass second.
//
// `phase` is "startup" or "periodic" — passed through to
// gc_tick_deadline_reached events so an operator can correlate.
func (g *GC) runTick(ctx context.Context, phase string) (tickResult, error) {
	deadline := time.Now().Add(g.cfg.MaxTickDuration)

	var res tickResult

	// Snapshot pass.
	snap, snapDeadline, err := g.runSnapshotPass(ctx, deadline, phase)
	res.orphanCandidatesReaped += snap.OrphanReaped
	res.displacedReaped += snap.DisplacedReaped
	if snapDeadline {
		res.deadlineReached = true
	}
	if err != nil {
		return res, err
	}

	// Blob pass — receives the same deadline. If snapshot pass
	// exhausted it, blob pass exits immediately at its first
	// per-batch deadline check.
	blob, blobDeadline, blobUnlinkErrs, err := g.runBlobPass(ctx, deadline, phase)
	res.blobsReaped += blob.count
	res.bytesReclaimed += blob.bytes
	res.poolUnlinkErrors += blobUnlinkErrs
	if blobDeadline {
		res.deadlineReached = true
	}
	if err != nil {
		return res, err
	}

	return res, nil
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
