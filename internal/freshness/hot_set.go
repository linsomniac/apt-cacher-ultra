package freshness

import (
	"context"
	"fmt"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// hotSetEntry is one (.deb path, declared_sha256, upstream_url) tuple
// the SPEC3 §7.5 hot-prefetch loop will attempt to warm. The freshness
// adopter constructs upstream_url textually from the suite's canonical
// scheme/host plus the canonical path that cache.ComputeHotSet
// returned.
type hotSetEntry struct {
	path           string
	declaredSHA256 string
	upstreamURL    string
}

// computeHotSet wraps cache.ComputeHotSet with the freshness-side
// upstream URL composition. The candidate snapshot's package_hash
// rows are passed in memory because CommitAdoption hasn't yet
// inserted them when this runs (SPEC3 §7.5 steps 9–11 are deliberately
// ordered: build candidate rows → hot prefetch → flip transaction).
//
// Returns nil for the cases SPEC3 §7.5 step 9 enumerates as "empty
// hot set":
//   - no prior current_snapshot_id for this suite,
//   - hotWindowSeconds == 0,
//   - no eligible prior-snapshot rows have a fresh url_path.last_requested_at,
//   - no Stage-1 (Package, Arch) tuple matches a candidate row.
//
// nil hot set falls through naturally — the §7.5 loop iterates over an
// empty slice and the flip proceeds via Phase 2 path with
// prefetchedURLPaths = nil.
func (a *Adopter) computeHotSet(ctx context.Context, suite SuiteRef,
	candidateSnapshotID int64, candidatePackageHashes []cache.PackageHash,
	hotWindowSeconds int64) ([]hotSetEntry, error) {
	if hotWindowSeconds == 0 {
		return nil, nil
	}
	priorID, ok := a.priorCurrentSnapshotID(ctx, suite)
	if !ok {
		return nil, nil
	}
	rows, err := a.cache.ComputeHotSet(ctx,
		suite.CanonicalScheme, suite.CanonicalHost,
		priorID, candidateSnapshotID, candidatePackageHashes,
		hotWindowSeconds, a.now().Unix(), a.maxVersionsPerPackage)
	if err != nil {
		return nil, fmt.Errorf("freshness: hot-set: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]hotSetEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, hotSetEntry{
			path:           r.Path,
			declaredSHA256: r.DeclaredSHA256,
			upstreamURL:    suite.CanonicalScheme + "://" + suite.CanonicalHost + r.Path,
		})
	}
	return out, nil
}

// priorCurrentSnapshotID returns the suite's pre-flip current_snapshot_id
// from suite_freshness — the snapshot whose package_hash rows the
// SPEC3 §7.5.3 Stage 1 query mines. Returns (0, false) when the
// suite has no current snapshot (cold cache for this suite — no hot
// set possible). DB errors collapse to (0, false) so a transient
// SQLite hiccup doesn't poison adoption; the worst outcome is a single
// adoption that runs without hot-prefetch warming.
func (a *Adopter) priorCurrentSnapshotID(ctx context.Context, suite SuiteRef) (int64, bool) {
	fresh, err := a.cache.GetSuiteFreshness(ctx,
		suite.CanonicalScheme, suite.CanonicalHost, suite.SuitePath)
	if err != nil || fresh == nil || fresh.CurrentSnapshotID == nil {
		return 0, false
	}
	return *fresh.CurrentSnapshotID, true
}
