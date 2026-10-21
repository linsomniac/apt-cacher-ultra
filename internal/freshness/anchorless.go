package freshness

// Spec C4 for flat suites: Dispatch 3's client-created index rows, and
// (Dispatch 4) the anchorless refresh of a flat, Release-less suite.

import (
	"context"
	"fmt"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// suiteKeyOf is the cache key of suite.
func suiteKeyOf(suite SuiteRef) cache.SuiteKey {
	return cache.SuiteKey{CanonicalScheme: suite.CanonicalScheme, CanonicalHost: suite.CanonicalHost, SuitePath: suite.SuitePath}
}

// flatIndexClientCreated reports whether flat suite has a client-created
// root Packages* or Sources* url_path row (aptpath.FileFlatIndex,
// request_count > 0): spec C4 Dispatch 3 counts it beside the InRelease
// and Release rows, so a Release-less repository's suite — created by its
// index's serve — is not dormant (the E carry-forward; plan design note 8).
// A row hot prefetch or a reconstruction wrote has no request count.
func (a *Adopter) flatIndexClientCreated(ctx context.Context, suite SuiteRef) (bool, error) {
	rows, err := a.cache.ListSuiteMetadataRows(ctx, suiteKeyOf(suite))
	if err != nil {
		return false, fmt.Errorf("read flat index rows: %w", err)
	}
	for _, r := range rows {
		if r.RequestCount > 0 && aptpath.ClassifySuiteFile(suite.SuitePath, r.Path) == aptpath.FileFlatIndex {
			return true, nil
		}
	}
	return false, nil
}
