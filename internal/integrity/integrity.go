// Package integrity implements the SPEC2 §6.5 at-rest scan: a
// dedicated worker pool walks every blob a current snapshot pins,
// hashes it, and removes the file (logging at_rest_corruption) on
// mismatch. Subsequent requests miss, refetch, and re-validate against
// the same declared hash.
//
// The scanner is read-mostly with respect to the cache: it only writes
// when a corruption is detected, and only via Cache.DiscardFinalizedBlob
// (which removes pool/<hash> without touching the DB). Phase 4 GC will
// later eviction the orphaned blob row when refcount reaches zero;
// the scanner does not pre-empt it.
package integrity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// Scanner runs the §6.5 at-rest scan on a fixed cadence.
type Scanner struct {
	cache    *cache.Cache
	interval time.Duration
	workers  int
	logger   *slog.Logger
	now      func() time.Time
}

// Config configures a Scanner. Workers is required when Interval > 0;
// the config layer (config.IntegrityConfig) enforces this and the
// constructor revalidates as a defense in depth.
type Config struct {
	Cache    *cache.Cache
	Interval time.Duration
	Workers  int
	Logger   *slog.Logger
	// Now is injectable for tests; defaults to time.Now.
	Now func() time.Time
}

// New constructs a Scanner from cfg. Returns an error on a nil cache,
// nil logger, negative interval, or workers < 1 (when interval > 0).
// A zero interval is allowed: Run returns immediately, ScanOnce still
// works for tests / manual invocation.
func New(cfg Config) (*Scanner, error) {
	if cfg.Cache == nil {
		return nil, errors.New("integrity: cache is required")
	}
	if cfg.Logger == nil {
		return nil, errors.New("integrity: logger is required")
	}
	if cfg.Interval < 0 {
		return nil, fmt.Errorf("integrity: interval must be >= 0, got %v", cfg.Interval)
	}
	if cfg.Interval > 0 && cfg.Workers < 1 {
		return nil, fmt.Errorf("integrity: workers must be >= 1 when interval > 0, got %d", cfg.Workers)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	workers := cfg.Workers
	if workers < 1 {
		workers = 1
	}
	return &Scanner{
		cache:    cfg.Cache,
		interval: cfg.Interval,
		workers:  workers,
		logger:   cfg.Logger,
		now:      cfg.Now,
	}, nil
}

// Run starts the periodic scan loop. Returns when ctx is cancelled.
// Interval = 0 disables the scan (Run returns immediately). The first
// scan fires after one full interval — startup is not the right time
// to spend cycles hashing the entire pool.
//
// Each scan runs to completion before the next ticker tick can start
// it again; a scan slower than the interval simply means scans run
// back-to-back.
func (s *Scanner) Run(ctx context.Context) {
	if s.interval <= 0 {
		return
	}
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.ScanOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.logger.Warn("integrity: scan failed", "err", err)
			}
		}
	}
}

// ScanOnce performs a single pass of the at-rest scan. Public so tests
// can drive a deterministic single pass without waiting on a ticker.
//
// The pass:
//  1. Lists every blob pinned by a current snapshot (via
//     ListIntegrityCandidates).
//  2. Distributes the work across s.workers goroutines.
//  3. For each candidate, opens pool/<BlobHash>, streams sha256, and
//     compares to BlobHash. Missing files are skipped (the row will
//     re-fetch on the next request — a different inconsistency the
//     scanner does not own). Hash matches are silent.
//  4. On mismatch: emit at_rest_corruption Error per SPEC2 §10.2,
//     then DiscardFinalizedBlob to remove pool/<BlobHash>. The
//     scanner is the only writer; concurrent scans cannot race
//     because Run runs ScanOnce serially.
//
// Returns the underlying error if cache enumeration fails. Per-blob
// errors are logged and aggregated into mismatch_count / scan_failed
// fields rather than aborting the pass — a single unreadable file
// must not stall the scan over the rest of the pool.
func (s *Scanner) ScanOnce(ctx context.Context) error {
	start := s.now()
	candidates, err := s.cache.ListIntegrityCandidates(ctx)
	if err != nil {
		return fmt.Errorf("integrity: list candidates: %w", err)
	}
	s.logger.Info("at_rest_scan_started", "blob_count", len(candidates))
	if len(candidates) == 0 {
		s.logger.Info("at_rest_scan_finished",
			"blob_count", 0, "mismatch_count", 0,
			"duration_ms", s.now().Sub(start).Milliseconds())
		atRestScansTotal.Inc()
		return nil
	}

	work := make(chan cache.IntegrityCandidate)
	var (
		wg         sync.WaitGroup
		mismatches int64
		ioErrors   int64
	)
	workers := s.workers
	if workers > len(candidates) {
		workers = len(candidates)
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range work {
				if err := s.verifyOne(ctx, c); err != nil {
					switch {
					case errors.Is(err, errHashMismatch):
						atomic.AddInt64(&mismatches, 1)
					case errors.Is(err, context.Canceled):
						return
					default:
						atomic.AddInt64(&ioErrors, 1)
					}
				}
			}
		}()
	}
	for _, c := range candidates {
		select {
		case <-ctx.Done():
			close(work)
			wg.Wait()
			return ctx.Err()
		case work <- c:
		}
	}
	close(work)
	wg.Wait()

	s.logger.Info("at_rest_scan_finished",
		"blob_count", len(candidates),
		"mismatch_count", atomic.LoadInt64(&mismatches),
		"io_error_count", atomic.LoadInt64(&ioErrors),
		"duration_ms", s.now().Sub(start).Milliseconds())
	atRestScansTotal.Inc()
	return nil
}

// errHashMismatch is the sentinel returned by verifyOne when the file
// at pool/<BlobHash> hashes to something other than BlobHash. It is
// classified separately from io errors in the per-pass tally.
var errHashMismatch = errors.New("integrity: hash mismatch")

// verifyOne hashes the single blob at pool/<c.BlobHash>. Returns nil
// for both "match" and "file missing" — both are non-actionable for
// the scanner, but the missing case is logged at Debug. Returns
// errHashMismatch (already logged) when the file's content disagrees
// with its filename. Other errors (open failures, read failures) are
// logged at Warn and returned wrapped so the caller can tally them.
func (s *Scanner) verifyOne(ctx context.Context, c cache.IntegrityCandidate) error {
	blobPath := s.cache.BlobPath(c.BlobHash)
	got, err := hashFile(ctx, blobPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// Missing blob is not a corruption; the snapshot_member or
		// package_hash row references a blob the cache no longer has
		// on disk. SPEC2 §6.1 / §6.2 handle this on the request path
		// (fail closed for adopted-suite metadata, refetch for .deb
		// miss). The scanner just notes it and moves on.
		s.logger.Debug("integrity: blob missing", "blob_hash", c.BlobHash,
			"snapshot_id", c.SnapshotID, "source", c.SourceTable)
		return nil
	case errors.Is(err, context.Canceled):
		return err
	case err != nil:
		s.logger.Warn("integrity: hash blob failed", "err", err,
			"blob_hash", c.BlobHash, "snapshot_id", c.SnapshotID)
		return err
	}
	if got == c.BlobHash {
		return nil
	}

	// SPEC2 §10.2 at_rest_corruption schema: blob_hash, declared_sha256,
	// snapshot_id. The source table is included as a fourth field so
	// operators can tell whether a package or a metadata blob went bad.
	s.logger.Error("at_rest_corruption",
		"blob_hash", c.BlobHash,
		"declared_sha256", c.DeclaredSHA256,
		"observed_sha256", got,
		"snapshot_id", c.SnapshotID,
		"source", c.SourceTable)
	atRestCorruptionTotal.Inc()
	hashValidationFailureTotal.Inc("at_rest")

	if err := s.cache.DiscardFinalizedBlob(c.BlobHash); err != nil {
		s.logger.Warn("integrity: discard blob failed", "err", err,
			"blob_hash", c.BlobHash)
		// Even if removal failed, we still report the mismatch — it's
		// the more important signal. Subsequent scans will retry.
	}
	return errHashMismatch
}

// hashFile streams the file's bytes through sha256, honoring ctx
// cancellation between chunks. Returns os.ErrNotExist (verbatim) when
// the file is absent so callers can errors.Is-classify it. Errors
// from os.Open / Read are returned wrapped.
func hashFile(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	buf := make([]byte, 64*1024)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, rerr := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", fmt.Errorf("read: %w", rerr)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
