package gc

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

// scanResult bundles the outcome of one pool/ orphan scan for the
// gc_run_complete startup line.
type scanResult struct {
	orphansRepaired     int
	orphanBytesRepaired int64
	unlinkErrors        int
}

// scanCounters are one pool scan's tallies, shared by its workers.
type scanCounters struct {
	orphans, orphanBytes, unlinkErrors atomic.Int64
}

// runPoolScan implements SPEC4 §9.6.4: walk pool/<two-hex-prefix>/
// directories with a worker pool; for each file, ask the cache
// whether a blob row exists for that hash; if not, unlink it; if
// the filename doesn't satisfy sha256-hex shape, log
// gc_pool_malformed_name and leave the file alone; if the parent
// directory doesn't equal hash[:2], log gc_pool_misplaced_file and
// leave the file alone.
//
// Cancellable via ctx; partial progress is preserved (rows reaped
// before cancel are real). Returns a non-nil error only on a fatal
// failure (e.g. cannot read pool/ root).
func (g *GC) runPoolScan(ctx context.Context) (scanResult, error) {
	poolRoot := filepath.Join(g.cfg.Cache.Dir(), "pool")

	prefixEntries, err := os.ReadDir(poolRoot)
	if err != nil {
		// pool/ should always exist (cache.Open creates it). A read
		// error here is fatal for the scan but not for the daemon —
		// surface it; main decides whether to keep going.
		return scanResult{}, err
	}

	type job struct {
		prefix string
		name   string
	}
	jobs := make(chan job, 64)

	var (
		counts scanCounters
		wg     sync.WaitGroup
	)

	worker := func() {
		defer wg.Done()
		for j := range jobs {
			g.scanPoolFile(ctx, poolRoot, j.prefix, j.name, &counts)
		}
	}

	for i := 0; i < g.cfg.PoolScanWorkers; i++ {
		wg.Add(1)
		go worker()
	}

	// Producer: walk pool/<prefix>/<file>. Errors on a single
	// prefix are logged at Warn but the scan continues — losing
	// the contents of one prefix is better than aborting the whole
	// scan.
	feedDone := make(chan struct{})
	go func() {
		defer close(feedDone)
		defer close(jobs)
		// Runs first: a panic still closes jobs and feedDone, so the
		// workers drain and the scan ends (deferred minor F-21).
		defer g.recoverPanic("pool_scan_producer")
		for _, p := range prefixEntries {
			if err := ctx.Err(); err != nil {
				return
			}
			if !p.IsDir() {
				// Stray file at pool/<name> — not under a prefix
				// directory at all. Log and skip; the operator
				// decides what to do.
				g.cfg.Logger.Warn("gc_pool_unexpected_root_file",
					"path", p.Name(),
				)
				continue
			}
			prefixPath := filepath.Join(poolRoot, p.Name())
			files, err := os.ReadDir(prefixPath)
			if err != nil {
				g.cfg.Logger.Warn("gc_pool_scan_dir_failed",
					"prefix", p.Name(),
					"err", err.Error(),
				)
				continue
			}
			for _, f := range files {
				if f.IsDir() {
					// Unexpected — pool layout is two-deep only.
					// Log and skip.
					g.cfg.Logger.Warn("gc_pool_unexpected_subdir",
						"path", filepath.Join(p.Name(), f.Name()),
					)
					continue
				}
				select {
				case jobs <- job{prefix: p.Name(), name: f.Name()}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	<-feedDone
	wg.Wait()

	return scanResult{
		orphansRepaired:     int(counts.orphans.Load()),
		orphanBytesRepaired: counts.orphanBytes.Load(),
		unlinkErrors:        int(counts.unlinkErrors.Load()),
	}, nil
}

// scanPoolFile is one pool-scan job, a worker's turn: the file
// pool/<prefix>/<name> is checked and, when no blob row names it,
// unlinked, tallied in counts.
//
// AIDEV-NOTE: deferred minor F-21. A panic is recovered per job
// (gc_panicked, where=pool_scan_worker) and the worker goes on to the
// next: a worker that died would leave the producer blocked on jobs once
// every worker had, until shutdown, and the startup pass would never end
// (TestPoolScan_AWorkerPanicNeverStallsTheScan).
func (g *GC) scanPoolFile(ctx context.Context, poolRoot, prefix, name string, counts *scanCounters) {
	defer g.recoverPanic("pool_scan_worker")
	if err := ctx.Err(); err != nil {
		// Drain remaining jobs without doing work; the outer cancel
		// will close jobs and the worker's range loop exits.
		return
	}
	rel := filepath.Join(prefix, name)
	abs := filepath.Join(poolRoot, prefix, name)

	// Validate filename shape before any DB lookup. HashKnown rejects
	// malformed hashes, but routing through it would produce a
	// misleading "ErrInvalidHash" log line — gc_pool_malformed_name is
	// the right surface.
	if !validHashLite(name) {
		g.cfg.Logger.Warn("gc_pool_malformed_name",
			"path", rel,
		)
		return
	}

	// Prefix consistency check (SPEC4 §9.6.4): a file at
	// pool/<wrong-prefix>/<hash> is unreachable by Cache.BlobPath
	// regardless of whether a blob row exists. Logging-only;
	// auto-repair (move or unlink) is rejected per SPEC4 §11.
	if expected := name[:2]; expected != prefix {
		g.cfg.Logger.Warn("gc_pool_misplaced_file",
			"path", rel,
			"expected_prefix", expected,
			"actual_prefix", prefix,
		)
		return
	}

	known, err := g.store.HashKnown(ctx, name)
	if err != nil {
		// HashKnown's only documented error is the validBlobHash gate
		// (caught above) plus DB-level failures. Surface as Warn;
		// don't unlink on uncertainty.
		g.cfg.Logger.Warn("gc_pool_scan_lookup_failed",
			"path", rel,
			"err", err.Error(),
		)
		return
	}
	if known {
		// File is referenced; leave it alone.
		return
	}

	// Capture size before unlinking for the gc_run_complete bytes
	// count. A stat failure is non-fatal — proceed with unlink, just
	// don't tally bytes for this file.
	var size int64
	if st, err := os.Stat(abs); err == nil {
		size = st.Size()
	}

	if err := os.Remove(abs); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			g.cfg.Logger.Warn("gc_pool_unlink_failed",
				"hash", name,
				"err", err.Error(),
				"operation", "pool_scan",
			)
			counts.unlinkErrors.Add(1)
			return
		}
		// ENOENT — somebody else already removed it. Don't tally as
		// orphan-repaired (we didn't do the work); also don't tally as
		// error.
		return
	}
	counts.orphans.Add(1)
	counts.orphanBytes.Add(size)
}

// validHashLite is a local copy of cache.validBlobHash's predicate;
// duplicated here so the pool scan can decide between
// gc_pool_malformed_name and a call to Cache.HashKnown without
// round-tripping through the cache package's ErrInvalidHash error
// (which would produce a less informative log line).
func validHashLite(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
