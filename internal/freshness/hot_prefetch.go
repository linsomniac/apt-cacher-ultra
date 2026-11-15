package freshness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/integrity"
)

// hotPrefetchStats is the per-adoption tally that fills the SPEC3
// §10.2 adoption_hot_prefetch_complete log line. The five bucket
// counts always sum to hotCount by construction — every iteration of
// the loop lands in exactly one bucket (success, retry-exhausted
// failure, hash mismatch, or budget-cancelled before attempt).
type hotPrefetchStats struct {
	hotCount    int
	fetched     int
	reused      int
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
// a deadline — hot_prefetch_budget or adoption.max_duration — (handled
// internally — emits adoption_hot_prefetch_partial once with the
// unattempted-paths list) and parent context shutdown (handled by
// checking ctx.Err() at the top of every iteration; runShared's stopErr
// check then abandons the attempt before its commit).
func (a *Adopter) runHotPrefetch(adoptionCtx context.Context, suite SuiteRef,
	snapshotID int64, candidatePackageHashes []cache.PackageHash,
	tracker *blobHeartbeatTracker) ([]cache.PrefetchedURLPath, hotPrefetchStats) {
	var stats hotPrefetchStats
	if attempt := adoptionLogFromContext(adoptionCtx); attempt != nil {
		defer func() { attempt.hot = stats }()
	}

	// SPEC3 §7.5 step 9: build the hot set. Pass the candidate's
	// in-memory package_hash rows — Stage 2 of ComputeHotSet looks
	// them up there, since CommitAdoption hasn't inserted them yet.
	// snapshotID is also passed through so Stage 2 can validate the
	// caller-supplied rows belong to this candidate (rejects accidental
	// cross-snapshot or cross-suite slices via ErrHotSetCandidateMismatch).
	hotWindowSeconds := int64(a.hotPackagesWindow.Seconds())
	hotSet, err := a.computeHotSet(adoptionCtx, suite, snapshotID, candidatePackageHashes, hotWindowSeconds)
	if err != nil {
		// Hot-set computation failure is non-fatal: log and skip the
		// loop. The flip still proceeds — operators see a warning,
		// post-flip requests fall back to the cache-miss path on first
		// hit. A 502-storm here would be hugely disproportionate to
		// what's a best-effort optimization.
		a.adoptionLogger(adoptionCtx).Warn("adoption: hot-set computation failed (skipping prefetch)",
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
	a.adoptionLogger(adoptionCtx).Info("adoption_hot_prefetch_started",
		"canonical_host", suite.CanonicalHost,
		"suite_path", suite.SuitePath,
		"snapshot_id", snapshotID,
		"hot_count", stats.hotCount,
		"budget_seconds", budgetSeconds,
	)
	hotPrefetchTotal.Inc("started", suite.CanonicalHost)
	if stats.hotCount == 0 {
		return nil, stats
	}
	// stats is assigned, not returned directly: the deferred update of the
	// attempt's record above reads it.
	var rows []cache.PrefetchedURLPath
	rows, stats = a.prefetchHotSet(adoptionCtx, suite, snapshotID, hotSet, tracker)
	return rows, stats
}

// prefetchHotSet is the SPEC3 §7.5 step 10 loop over a computed hot set.
func (a *Adopter) prefetchHotSet(adoptionCtx context.Context, suite SuiteRef, snapshotID int64,
	hotSet []hotSetEntry, tracker *blobHeartbeatTracker) ([]cache.PrefetchedURLPath, hotPrefetchStats) {
	stats := hotPrefetchStats{hotCount: len(hotSet)}

	// Set up the prefetch context. SPEC3 §7.5 step 10: prefetchCtx is
	// derived from adoptionCtx — a parent SIGTERM/lifecycle cancel
	// propagates here — but only the prefetch loop sees the budget
	// timeout, so a budget-elapsed prefetch never aborts the flip. The
	// flip itself (runShared Step 11) runs under its own commit context
	// (WithoutCancel + commitTimeout, spec C4); a shutdown before it
	// starts skips it.
	var (
		prefetchCtx    context.Context
		prefetchCancel context.CancelFunc
	)
	if a.hotPrefetchBudget > 0 {
		prefetchCtx, prefetchCancel = context.WithTimeout(adoptionCtx, a.hotPrefetchBudget)
	} else {
		prefetchCtx, prefetchCancel = context.WithCancel(adoptionCtx)
	}
	defer prefetchCancel()

	// Each package is fetched by one of HotPrefetchWorkers workers, in
	// the hot set's order of dispatch.
	//
	// AIDEV-NOTE: the prefetch runs before the new release is published,
	// so its duration is time clients keep seeing the old one; packages
	// used to be warmed one after another (the 2026-11-14
	// publication-delay review). The fetches share the per-host limit
	// with members and client downloads (fetchHotDeb takes a slot each).
	//
	// SPEC3 §7.5 step 10 / §10.2 contract, unchanged in substance:
	//   - a package in flight when a deadline passes — hot_prefetch_budget
	//     or the adoption context's own, adoption.max_duration — is
	//     attempted: fetchHotDeb logs hot_prefetch_deb_failed and it
	//     counts as failed;
	//   - a package not started by then (or cancelled before its fetch
	//     began: hotFetchCancelled) is unattempted, and the unattempted
	//     paths are logged once, in hot-set order, as
	//     adoption_hot_prefetch_partial — on a deadline only. A parent
	//     shutdown (Canceled) ends the prefetch silently; runShared's
	//     stopErr check before the flip barrier then abandons the attempt,
	//     so nothing commits;
	//   - the adoption goes on to its commit either way, under its own
	//     bound (WithoutCancel + commitTimeout, spec C4).
	type hotResult struct {
		outcome   hotFetchOutcome
		hash      string
		attempted bool
	}
	results := make([]hotResult, len(hotSet))
	forEachConcurrently(HotPrefetchWorkers, len(hotSet), prefetchCancel, func(i int) bool {
		if prefetchCtx.Err() != nil {
			return false // entry i and everything after it stay unattempted
		}
		entry := hotSet[i]
		// Track the expected hash before local verification, which can take
		// longer than the blob grace on slow disks. The periodic heartbeat
		// protects reused, unreferenced content until the atomic URL commit.
		// For missing files this is harmless: heartbeat skips absent rows.
		if tracker != nil {
			tracker.Add(entry.declaredSHA256)
		}
		hash, outcome := a.fetchHotDeb(prefetchCtx, suite, entry, snapshotID)
		// SPEC4 §7.5.2 site 4: heartbeat after every per-deb fetch
		// terminates (success, failure, mismatch, or cancel). Uses
		// adoptionCtx, not prefetchCtx, so a budget-elapsed
		// prefetchCtx doesn't suppress the heartbeat — adoption is
		// still alive even if the budget tripped. fetchHotDeb's
		// per-deb fetches can each consume upstream.total_timeout;
		// without this site the gap between the last member-fetch
		// heartbeat and the next one (site 5 pre-CommitAdoption) could
		// be the whole prefetch under pathological upstreams.
		a.heartbeat(adoptionCtx, suite.CanonicalHost, snapshotID, tracker)
		// Each worker writes its own index: no two share an element.
		results[i] = hotResult{outcome: outcome, hash: hash, attempted: outcome != hotFetchCancelled}
		return outcome != hotFetchCancelled
	})

	prefetched := make([]cache.PrefetchedURLPath, 0, stats.hotCount)
	var missing []string
	for i, r := range results {
		entry := hotSet[i]
		if !r.attempted {
			stats.unattempted++
			missing = append(missing, entry.path)
			continue
		}
		switch r.outcome {
		case hotFetchOK, hotFetchReused:
			prefetched = append(prefetched, cache.PrefetchedURLPath{
				CanonicalScheme: suite.CanonicalScheme,
				CanonicalHost:   suite.CanonicalHost,
				Path:            entry.path,
				BlobHash:        r.hash,
				UpstreamURL:     entry.upstreamURL,
			})
			if r.outcome == hotFetchReused {
				stats.reused++
			} else {
				stats.fetched++
			}
		case hotFetchFailed:
			// Includes both retry-exhaustion AND in-flight-at-budget
			// cancellation per SPEC3 §12.3 variant 1/3 contract: "the
			// hung fetch was attempted but cancelled when prefetchCtx
			// hit the budget" maps to failed (deb_failed log already
			// emitted by fetchHotDeb).
			stats.failed++
			hotPrefetchTotal.Inc("deb_failed", suite.CanonicalHost)
		case hotFetchMismatch:
			stats.mismatched++
			hotPrefetchTotal.Inc("hash_mismatch", suite.CanonicalHost)
		}
	}
	if len(missing) > 0 && errors.Is(prefetchCtx.Err(), context.DeadlineExceeded) {
		a.adoptionLogger(adoptionCtx).Warn("adoption_hot_prefetch_partial",
			"canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath,
			"snapshot_id", snapshotID,
			"missing", missing,
		)
		hotPrefetchTotal.Inc("partial", suite.CanonicalHost)
	}
	return prefetched, stats
}

// hotFetchOutcome is the per-deb categorical result the caller pivots
// on to update the right stats bucket. Each maps 1:1 to one of the
// SPEC3 §10.2 events (or no event, in the OK case).
type hotFetchOutcome int

const (
	hotFetchOK hotFetchOutcome = iota
	hotFetchReused
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
	unlock, err := a.blobLocks.Acquire(ctx, entry.declaredSHA256)
	if err != nil {
		return "", hotFetchCancelled
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		return "", hotFetchCancelled
	}
	// Check by content hash, not URL: versions and mirrors can share bytes.
	// Do this before taking a host slot so a cached package needs no upstream.
	reused, err := a.reuseHotBlob(ctx, suite, entry)
	if err != nil {
		if ctx.Err() != nil {
			return "", hotFetchCancelled
		}
		a.adoptionLogger(ctx).Warn("hot_prefetch_deb_failed", "canonical_host", suite.CanonicalHost,
			"path", entry.path, "snapshot_id", snapshotID, "err", err)
		return "", hotFetchFailed
	}
	if reused {
		return entry.declaredSHA256, hotFetchReused
	}
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
		a.adoptionLogger(ctx).Warn("hot_prefetch_deb_failed",
			"canonical_host", suite.CanonicalHost,
			"path", entry.path,
			"snapshot_id", snapshotID,
			"err", fmt.Errorf("NewTempBlob: %w", err),
		)
		return "", hotFetchFailed
	}
	defer func() { _ = w.Abort() }() // no-op once Finalize wins

	res, err := a.fetcher.Fetch(fetch.WithPurpose(ctx, "hot_prefetch"), target, w)
	if err != nil {
		// Three-way disambiguation:
		//   - ctx.Err() == DeadlineExceeded: the budget, or
		//     adoption.max_duration (the parent's deadline), elapsed
		//     with this deb in flight. SPEC3 §12.3 variant 1: "the
		//     hung fetch was attempted but cancelled when prefetchCtx
		//     hit the budget" → log hot_prefetch_deb_failed (with the wrapped
		//     context-cancellation error) and bucket as failed. The
		//     subsequent loop iteration's top check sees prefetchCtx
		//     done and emits the once-fires partial event with paths
		//     strictly AFTER this one.
		//   - ctx.Err() == Canceled (non-deadline): parent adoptionCtx
		//     was cancelled (SIGTERM / scheduler shutdown). Phase 2
		//     §9.5 semantics: abandon silently with no per-deb log.
		//   - ctx.Err() == nil: genuine upstream failure (retries
		//     exhausted, transport error, etc.). The fetch.Client's
		//     internal total_timeout surfaces here too — the outer
		//     prefetchCtx is still alive, so this is bucketed as a
		//     real per-deb retry-exhaustion failure.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			a.adoptionLogger(ctx).Warn("hot_prefetch_deb_failed",
				"canonical_host", suite.CanonicalHost,
				"path", entry.path,
				"snapshot_id", snapshotID,
				"err", err,
			)
			return "", hotFetchFailed
		}
		if ctx.Err() != nil {
			return "", hotFetchCancelled
		}
		a.adoptionLogger(ctx).Warn("hot_prefetch_deb_failed",
			"canonical_host", suite.CanonicalHost,
			"path", entry.path,
			"snapshot_id", snapshotID,
			"err", err,
		)
		return "", hotFetchFailed
	}
	// AIDEV-NOTE: the maintainer's ruling of 2026-11-05 (F2-I1). A .deb
	// is a package file, so an upstream 204 for it is an upstream error,
	// not an empty .deb, even when its declared hash is the empty file's
	// (TestRunHotPrefetch_NoContentIsAFailure). The deferred Abort drops
	// the temp; nothing reaches the pool.
	if res.NoContent {
		a.adoptionLogger(ctx).Warn("hot_prefetch_deb_failed",
			"canonical_host", suite.CanonicalHost,
			"path", entry.path,
			"snapshot_id", snapshotID,
			"err", errors.New("upstream answered 204 No Content"),
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
			a.adoptionLogger(ctx).Warn("hot_prefetch_hash_mismatch",
				"canonical_host", suite.CanonicalHost,
				"path", entry.path,
				"snapshot_id", snapshotID,
				"declared_sha256", entry.declaredSHA256,
				"observed_sha256", hashHex,
			)
			return "", hotFetchMismatch
		}
		a.adoptionLogger(ctx).Warn("hot_prefetch_deb_failed",
			"canonical_host", suite.CanonicalHost,
			"path", entry.path,
			"snapshot_id", snapshotID,
			"err", fmt.Errorf("finalize: %w", err),
		)
		return "", hotFetchFailed
	}
	if err := a.cache.PutBlob(ctx, hashHex, w.Written()); err != nil {
		a.adoptionLogger(ctx).Warn("hot_prefetch_deb_failed",
			"canonical_host", suite.CanonicalHost,
			"path", entry.path,
			"snapshot_id", snapshotID,
			"err", fmt.Errorf("PutBlob: %w", err),
		)
		return "", hotFetchFailed
	}
	if err := a.cache.PutRepresentation(ctx, suite.CanonicalScheme, suite.CanonicalHost, entry.path, hashHex, res.LastModified); err != nil {
		a.adoptionLogger(ctx).Warn("hot_prefetch_deb_failed", "canonical_host", suite.CanonicalHost, "path", entry.path, "err", err)
		return "", hotFetchFailed
	}
	return hashHex, hotFetchOK
}

// reuseHotBlob revalidates disk content against the candidate's declared hash.
// Refresh the GC grace before reading, then PutBlob before returning it to the
// adoption heartbeat/atomic URL commit. Preserve URL-specific origin dates;
// local reuse is not a new upstream observation.
func (a *Adopter) reuseHotBlob(ctx context.Context, suite SuiteRef, entry hotSetEntry) (bool, error) {
	exists, err := a.cache.BlobExists(entry.declaredSHA256)
	if err != nil || !exists {
		return false, err
	}
	if err := a.cache.HeartbeatBlobs(ctx, []string{entry.declaredSHA256}); err != nil {
		return false, err
	}
	f, err := os.Open(a.cache.BlobPath(entry.declaredSHA256))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, hotContextReader{ctx: ctx, r: f})
	if err != nil {
		return false, err
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if actual != entry.declaredSHA256 {
		a.adoptionLogger(ctx).Warn("pool_corruption_during_adoption", "canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath, "path", entry.path,
			"declared_sha256", entry.declaredSHA256, "actual_sha256", actual)
		integrity.IncPoolCorruptionDuringAdoption()
		// FinalizeExpectingHash deduplicates against an existing pool file.
		// Remove the bad copy so the verified download can replace it.
		return false, a.cache.DiscardFinalizedBlob(entry.declaredSHA256)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := a.cache.PutBlob(ctx, entry.declaredSHA256, n); err != nil {
		return false, err
	}
	return true, nil
}

type hotContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r hotContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
