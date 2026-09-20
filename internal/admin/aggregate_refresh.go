package admin

import (
	"context"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

type aggregateRevision struct {
	revision cache.DatabaseRevision
	valid    bool
}

// refreshDatabaseAggregates avoids scanning large package catalogs again when
// their inputs have not changed. These two results depend only on stored DB
// contents, with no wall-clock expiry. Other gauges (particularly the pool walk)
// keep their own refresh cadence because database revisions do not cover them.
func (s *Server) refreshDatabaseAggregates(ctx context.Context) {
	s.aggregateMu.Lock()
	defer s.aggregateMu.Unlock()
	s.refreshAggregate(ctx, "repo_coverage", &s.coverageRevision, s.refreshRepoCoverage)
	s.refreshAggregate(ctx, "cache_summary", &s.summaryRevision, s.refreshCacheSummary)
}

// Caller holds aggregateMu. Failed queries and writes overlapping a computation
// invalidate only that stage. Successful results are still published during
// sustained writes, just as before; they must be recomputed on the next pass.
func (s *Server) refreshAggregate(ctx context.Context, stage string, stamp *aggregateRevision, refresh func(context.Context) bool) {
	before, err := s.databaseRevision(ctx)
	if err == nil && stamp.valid && stamp.revision == before {
		s.gauges.refreshReused.Inc(stage)
		return
	}
	stamp.valid = false
	if !refresh(ctx) || err != nil {
		return
	}
	after, err := s.databaseRevision(ctx)
	if err == nil && before == after {
		stamp.revision = after
		stamp.valid = true
	}
}

func (s *Server) databaseRevision(parent context.Context) (cache.DatabaseRevision, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	revision, err := s.cfg.Cache.DatabaseRevision(ctx)
	s.observeRefresh("database_revision", start, err)
	if err != nil {
		s.logRefresherFailure("database_revision", err, time.Since(start))
	}
	return revision, err
}
