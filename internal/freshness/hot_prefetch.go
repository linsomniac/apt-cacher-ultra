package freshness

import (
	"context"
	"errors"
	"fmt"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
)

// hotPrefetchStats is the per-adoption tally that fills the SPEC3
// §10.2 adoption_hot_prefetch_complete log line. The four bucket
// counts always sum to hotCount by construction — every iteration of
// the loop lands in exactly one bucket (success, retry-exhausted
// failure, hash mismatch, or budget-cancelled before attempt).
type hotPrefetchStats struct {
	hotCount    int
	fetched     int
	failed      int
	mismatched  int
	unattempted int
}

// runHotPrefetch executes the SPEC3 §7.5 step 9 + 10 hot-set
// computation and prefetch loop. It returns the prefetched url_path
// rows for inclusion in the atomic flip (CommitAdoption) plus the
// summary stats for the adoption_hot_prefetch_complete log line.
//
// The function is intentionally infallible: every per-deb error
// surfaces via its own log event (hot_prefetch_deb_failed,
// hot_prefetch_hash_mismatch) and bucketed counts. Adoption never
// aborts because of a hot-prefetch problem — the post-flip request
// path will rebuild the missing url_path on demand via the standard
// cache-miss flow. The only cancellations the caller cares about are
// budget elapse (handled internally — emits adoption_hot_prefetch_partial
// once with the unattempted-paths list) and parent context shutdown
// (handled by checking ctx.Err() at the top of every iteration).
func (a *Adopter) runHotPrefetch(adoptionCtx context.Context, suite SuiteRef,
	snapshotID int64) ([]cache.PrefetchedURLPath, hotPrefetchStats) {
	var stats hotPrefetchStats

	// SPEC3 §7.5 step 9: build the hot set.
	hotWindowSeconds := int64(a.hotPackagesWindow.Seconds())
	hotSet, err := a.computeHotSet(adoptionCtx, suite, snapshotID, hotWindowSeconds)
	if err != nil {
		// Hot-set computation failure is non-fatal: log and skip the
		// loop. The flip still proceeds — operators see a warning,
		// post-flip requests fall back to the cache-miss path on first
		// hit. A 502-storm here would be hugely disproportionate to
		// what's a best-effort optimization.
		a.logger.Warn("adoption: hot-set computation failed (skipping prefetch)",
			"canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath,
			"snapshot_id", snapshotID,
			"err", err,
		)
		return nil, stats
	}
	stats.hotCount = len(hotSet)

	// SPEC3 §7.5 step 10: log the start regardless of hot_count, so an
	// operator confirming "did the loop run at all?" sees a started
	// line on every adoption. This pairs with the always-on
	// adoption_hot_prefetch_complete in runShared.
	budgetSeconds := int64(0)
	if a.hotPrefetchBudget > 0 {
		budgetSeconds = int64(a.hotPrefetchBudget.Seconds())
	}
	a.logger.Info("adoption_hot_prefetch_started",
		"canonical_host", suite.CanonicalHost,
		"suite_path", suite.SuitePath,
		"snapshot_id", snapshotID,
		"hot_count", stats.hotCount,
		"budget_seconds", budgetSeconds,
	)
	if stats.hotCount == 0 {
		return nil, stats
	}

	// Set up the prefetch context. SPEC3 §7.5 step 10: prefetchCtx is
	// derived from adoptionCtx — a parent SIGTERM/lifecycle cancel
	// propagates here — but only the prefetch loop sees the budget
	// timeout. The flip below uses adoptionCtx directly so a
	// budget-elapsed prefetch never aborts the flip.
	prefetchCtx := adoptionCtx
	prefetchCancel := func() {}
	if a.hotPrefetchBudget > 0 {
		prefetchCtx, prefetchCancel = context.WithTimeout(adoptionCtx, a.hotPrefetchBudget)
	}
	defer prefetchCancel()

	prefetched := make([]cache.PrefetchedURLPath, 0, stats.hotCount)

	for i, entry := range hotSet {
		// Distinguish budget elapse from parent shutdown. Budget elapse
		// emits adoption_hot_prefetch_partial once and breaks the loop;
		// parent shutdown is Phase 2 §9.5 semantics — abandon the
		// candidate by returning what we have so far, but the caller's
		// CommitAdoption (running under adoptionCtx, which is also
		// cancelled) will fail naturally and the flip won't happen.
		if err := prefetchCtx.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				stats.unattempted = stats.hotCount - i
				missing := make([]string, 0, stats.unattempted)
				for _, e := range hotSet[i:] {
					missing = append(missing, e.path)
				}
				a.logger.Warn("adoption_hot_prefetch_partial",
					"canonical_host", suite.CanonicalHost,
					"suite_path", suite.SuitePath,
					"snapshot_id", snapshotID,
					"missing", missing,
				)
			}
			break
		}
		blobHash, outcome := a.fetchHotDeb(prefetchCtx, suite, entry, snapshotID)
		switch outcome {
		case hotFetchOK:
			prefetched = append(prefetched, cache.PrefetchedURLPath{
				CanonicalScheme: suite.CanonicalScheme,
				CanonicalHost:   suite.CanonicalHost,
				Path:            entry.path,
				BlobHash:        blobHash,
				UpstreamURL:     entry.upstreamURL,
			})
			stats.fetched++
		case hotFetchFailed:
			stats.failed++
		case hotFetchMismatch:
			stats.mismatched++
		case hotFetchCancelled:
			// fetchHotDeb returns hotFetchCancelled when prefetchCtx
			// itself is done. Distinguish budget elapse (DeadlineExceeded
			// on the prefetch context) from parent adoptionCtx
			// cancellation (SIGTERM / scheduler LifetimeCtx propagating
			// through). SPEC3 §10.2: adoption_hot_prefetch_partial fires
			// ONLY on budget elapse; on parent cancellation we abandon
			// silently and the caller's CommitAdoption will fail
			// naturally under the same cancelled adoptionCtx.
			stats.unattempted = stats.hotCount - i
			if errors.Is(prefetchCtx.Err(), context.DeadlineExceeded) {
				missing := make([]string, 0, stats.unattempted)
				for _, e := range hotSet[i:] {
					missing = append(missing, e.path)
				}
				a.logger.Warn("adoption_hot_prefetch_partial",
					"canonical_host", suite.CanonicalHost,
					"suite_path", suite.SuitePath,
					"snapshot_id", snapshotID,
					"missing", missing,
				)
			}
			return prefetched, stats
		}
	}
	return prefetched, stats
}

// hotFetchOutcome is the per-deb categorical result the caller pivots
// on to update the right stats bucket. Each maps 1:1 to one of the
// SPEC3 §10.2 events (or no event, in the OK case).
type hotFetchOutcome int

const (
	hotFetchOK hotFetchOutcome = iota
	hotFetchFailed
	hotFetchMismatch
	hotFetchCancelled
)

// fetchHotDeb attempts to warm one hot .deb into pool/. Returns the
// resulting blob hash on success, plus the outcome category. SPEC3
// §7.5 step 10:
//
//   - per-deb total budget = upstream.total_timeout × max_retries (the
//     fetch.Client enforces this; we do not layer a second budget on
//     top — the prefetchCtx wall-clock guard is the only cap).
//   - hash mismatch discards the temp blob without promoting to pool/
//     (defensively guards against a hostile upstream serving bytes
//     whose hash disagrees with the snapshot's declaration).
//   - per-deb retry exhaustion logs hot_prefetch_deb_failed and the
//     loop continues to the next entry — one bad deb does not stall
//     the rest of the warm.
func (a *Adopter) fetchHotDeb(ctx context.Context, suite SuiteRef,
	entry hotSetEntry, snapshotID int64) (string, hotFetchOutcome) {
	// SPEC §9.3 / §9.3.1: hot-prefetch fetches share the same per-host
	// budget as metadata-member fetches. Sequential within an adoption
	// keeps fan-out exactly the same as Phase 2.
	release, err := a.hostSem.Acquire(ctx, suite.CanonicalHost)
	if err != nil {
		// Acquire failed — typically because ctx was cancelled. Treat
		// as "cancelled" so the caller knows not to log deb_failed
		// (which is the per-deb retry-exhaustion bucket).
		if errors.Is(err, context.DeadlineExceeded) {
			return "", hotFetchCancelled
		}
		return "", hotFetchCancelled
	}
	defer release()

	target := &fetch.Target{
		CanonicalHost: suite.CanonicalHost,
		URL:           entry.upstreamURL,
	}
	w, err := a.cache.NewTempBlob()
	if err != nil {
		a.logger.Warn("hot_prefetch_deb_failed",
			"canonical_host", suite.CanonicalHost,
			"path", entry.path,
			"snapshot_id", snapshotID,
			"err", fmt.Errorf("NewTempBlob: %w", err),
		)
		return "", hotFetchFailed
	}
	defer func() { _ = w.Abort() }() // no-op once Finalize wins

	res, err := a.fetcher.Fetch(ctx, target, w)
	if err != nil {
		// Disambiguate prefetch-context cancellation from upstream
		// failure. SPEC3 §7.5: budget elapse cancels prefetchCtx, and
		// only that case should be reported as "cancelled" (which the
		// caller then maps onto adoption_hot_prefetch_partial). The
		// fetch.Client also surfaces its internal total_timeout as
		// context.DeadlineExceeded under some flag combinations, but
		// without the prefetchCtx itself being done — those are
		// genuine per-deb retry-exhaustion failures, not budget-elapse.
		// Predicate: the outer ctx must be the one that's done.
		if ctx.Err() != nil {
			return "", hotFetchCancelled
		}
		a.logger.Warn("hot_prefetch_deb_failed",
			"canonical_host", suite.CanonicalHost,
			"path", entry.path,
			"snapshot_id", snapshotID,
			"err", err,
		)
		return "", hotFetchFailed
	}
	// SPEC3 §7.5 step 10: "discard the temp blob — do NOT promote to
	// pool." FinalizeExpectingHash gates on the declared hash BEFORE
	// any rename happens; on mismatch the temp is removed and
	// pool/ is never touched. Plain Finalize would move the bytes to
	// pool/<observed> first and only then compare — leaving an orphan
	// behind that violates the spec's "do not promote" contract.
	hashHex, err := w.FinalizeExpectingHash(entry.declaredSHA256, res.ContentLength)
	if err != nil {
		if errors.Is(err, cache.ErrHashMismatch) {
			a.logger.Warn("hot_prefetch_hash_mismatch",
				"canonical_host", suite.CanonicalHost,
				"path", entry.path,
				"snapshot_id", snapshotID,
				"declared_sha256", entry.declaredSHA256,
				"observed_sha256", hashHex,
			)
			return "", hotFetchMismatch
		}
		a.logger.Warn("hot_prefetch_deb_failed",
			"canonical_host", suite.CanonicalHost,
			"path", entry.path,
			"snapshot_id", snapshotID,
			"err", fmt.Errorf("finalize: %w", err),
		)
		return "", hotFetchFailed
	}
	if err := a.cache.PutBlob(ctx, hashHex, w.Written()); err != nil {
		a.logger.Warn("hot_prefetch_deb_failed",
			"canonical_host", suite.CanonicalHost,
			"path", entry.path,
			"snapshot_id", snapshotID,
			"err", fmt.Errorf("PutBlob: %w", err),
		)
		return "", hotFetchFailed
	}
	return hashHex, hotFetchOK
}
