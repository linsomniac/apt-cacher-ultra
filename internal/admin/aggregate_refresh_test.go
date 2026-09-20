package admin

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
)

// These tests drive real refreshes and SQLite commits without listeners or
// timers. Waiting for each pool walk also keeps its filesystem work deterministic.
func aggregateTestServer(t *testing.T) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c, err := cache.Open(context.Background(), t.TempDir(), logger)
	if err != nil {
		t.Fatal(err)
	}
	r := metrics.NewRegistry()
	s := &Server{
		cfg:    Config{Cache: c, Registry: r, HostLimiter: hostsem.New(8)},
		logger: logger,
		gauges: newRefresherGauges(r, 32),
		proc:   newProcessGauges(r),
	}
	t.Cleanup(func() {
		s.walkWg.Wait()
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func assertAggregateMetric(t *testing.T, s *Server, sample string, want float64) {
	t.Helper()
	var rendered strings.Builder
	s.cfg.Registry.Render(&rendered)
	for _, line := range strings.Split(rendered.String(), "\n") {
		if value, ok := strings.CutPrefix(line, sample+" "); ok {
			got, err := strconv.ParseFloat(value, 64)
			if err != nil || got != want {
				t.Errorf("%s = %q, want %g", sample, value, want)
			}
			return
		}
	}
	// A lazily created counter with no events has no sample yet.
	if want != 0 {
		t.Errorf("missing %s, want %g", sample, want)
	}
}

func assertAggregateRuns(t *testing.T, s *Server, stage string, want float64) {
	t.Helper()
	assertAggregateMetric(t, s, `acu_admin_refresh_duration_seconds_count{stage="`+stage+`"}`, want)
}

func TestAggregateRefreshUnchangedDatabaseKeepsOtherWorkRunning(t *testing.T) {
	s := aggregateTestServer(t)
	ctx := context.Background()
	s.runRefreshOnce(ctx)
	s.walkWg.Wait()
	coverage, summary := s.repoCoverage.Load(), s.cacheSummaryByHostArch.Load()
	if coverage == nil || summary == nil {
		t.Fatal("initial refresh did not populate aggregates")
	}
	// A filesystem-only change must still appear in the disk gauge even
	// though the database and both expensive aggregates remain unchanged.
	if err := os.WriteFile(filepath.Join(s.cfg.Cache.Dir(), "pool", "orphan"), []byte("seven!!"), 0o600); err != nil {
		t.Fatal(err)
	}
	release, err := s.cfg.HostLimiter.Acquire(ctx, "example.test")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	s.runRefreshOnce(ctx)
	s.walkWg.Wait()
	if s.repoCoverage.Load() != coverage || s.cacheSummaryByHostArch.Load() != summary {
		t.Error("unchanged database rebuilt aggregate objects")
	}
	for _, stage := range []string{"repo_coverage", "cache_summary"} {
		assertAggregateRuns(t, s, stage, 1)
		assertAggregateMetric(t, s, `acu_admin_refresh_reused_total{stage="`+stage+`"}`, 1)
	}
	for _, stage := range []string{"cache_stats", "suite_stats", "pool_walk"} {
		assertAggregateRuns(t, s, stage, 2)
	}
	assertAggregateMetric(t, s, "acu_pool_disk_bytes", 7)
	assertAggregateMetric(t, s, "acu_active_hosts", 1)
}

func TestAggregateRefreshReconcileAndCachedPackageInvalidate(t *testing.T) {
	s := aggregateTestServer(t)
	c, ctx := s.cfg.Cache, context.Background()
	const host, suite, path = "example.test", "/dists/stable", "/pool/source.dsc"
	id := seedRepoCoverageSnapshot(t, s, c, "https", host, suite, nil, nil)
	s.walkWg.Wait()
	oldCoverage, oldSummary := s.repoCoverage.Load(), s.cacheSummaryByHostArch.Load()
	anchor, err := c.GetSnapshotMember(ctx, id, "InRelease")
	if err != nil {
		t.Fatal(err)
	}
	packageHash := strings.Repeat("a", 64)
	err = c.InsertReconciledMembers(ctx, id, []cache.SnapshotMember{{
		SnapshotID: id, Path: "main/Sources.diff/Index", BlobHash: anchor.BlobHash,
		DeclaredSHA256: anchor.DeclaredSHA256,
	}}, []cache.PackageHash{{
		CanonicalScheme: "https", CanonicalHost: host, Path: path,
		SnapshotID: id, DeclaredSHA256: packageHash, PackageName: "source", Architecture: "source",
	}})
	if err != nil {
		t.Fatal(err)
	}
	s.refreshDatabaseAggregates(ctx)
	got := s.repoCoverage.Load()
	if got == oldCoverage || got.PackageHashRowsTotal != 1 || got.SnapshotsWithSources != 1 || got.SnapshotsWithPdiff != 1 {
		t.Fatalf("in-place reconcile did not refresh coverage: %+v", got)
	}
	summary := s.cacheSummaryByHostArch.Load()
	if summary == oldSummary || (*summary)[host]["source"] != (cache.CacheSummaryEntry{PackageHashCount: 1}) {
		t.Fatalf("uncached reconciled package summary = %+v", summary)
	}
	fresh, err := c.GetSuiteFreshness(ctx, "https", host, suite)
	if err != nil || fresh == nil || fresh.CurrentSnapshotID == nil || *fresh.CurrentSnapshotID != id {
		t.Fatalf("test must retain the same current snapshot: %+v, %v", fresh, err)
	}
	// Caching a package changes bytes/counts without changing snapshots or
	// their package_hash rows; a snapshot-ID-only fingerprint would miss it.
	if err := c.PutBlob(ctx, packageHash, 47); err != nil {
		t.Fatal(err)
	}
	if err := c.PutURLPath(ctx, cache.URLPath{
		CanonicalScheme: "https", CanonicalHost: host, Path: path,
		BlobHash: &packageHash, UpstreamURL: "https://" + host + path,
	}); err != nil {
		t.Fatal(err)
	}
	s.refreshDatabaseAggregates(ctx)
	summary = s.cacheSummaryByHostArch.Load()
	want := cache.CacheSummaryEntry{PackageHashCount: 1, BlobCount: 1, BlobBytes: 47}
	if (*summary)[host]["source"] != want {
		t.Errorf("cached package summary = %+v, want %+v", (*summary)[host]["source"], want)
	}
	s.refreshDatabaseAggregates(ctx)
	if s.cacheSummaryByHostArch.Load() != summary {
		t.Error("unchanged summary after package cache fill was rebuilt")
	}
	assertAggregateRuns(t, s, "repo_coverage", 3)
	assertAggregateRuns(t, s, "cache_summary", 3)
}

func TestAggregateRefreshExternalCommitInvalidates(t *testing.T) {
	s := aggregateTestServer(t)
	c, ctx := s.cfg.Cache, context.Background()
	id := seedRepoCoverageSnapshot(t, s, c, "https", "example.test", "/dists/stable", nil, nil)
	s.walkWg.Wait()
	before := s.repoCoverage.Load()
	dsn := (&url.URL{Scheme: "file", Path: filepath.Join(c.Dir(), "cache.db")}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, `INSERT INTO package_hash
(canonical_scheme, canonical_host, path, declared_sha256, snapshot_id, architecture)
VALUES ('https', 'example.test', '/pool/externally-added.deb', ?, ?, 'amd64')`, strings.Repeat("b", 64), id); err != nil {
		t.Fatal(err)
	}
	s.refreshDatabaseAggregates(ctx)
	if got := s.repoCoverage.Load(); got == before || got.PackageHashRowsBinary != 1 {
		t.Errorf("external connection's commit did not refresh coverage: %+v", got)
	}
	assertAggregateRuns(t, s, "repo_coverage", 2)
	assertAggregateRuns(t, s, "cache_summary", 2)
}

func TestAggregateRefreshCommitDuringComputationPreventsReuse(t *testing.T) {
	s := aggregateTestServer(t)
	ctx := context.Background()
	var stamp aggregateRevision
	calls := 0
	refresh := func(ctx context.Context) bool {
		calls++
		ok := s.refreshRepoCoverage(ctx)
		if calls == 1 {
			// Commit after the read but before the post-read revision check.
			if err := s.cfg.Cache.PutBlob(ctx, strings.Repeat("c", 64), 1); err != nil {
				t.Fatal(err)
			}
		}
		return ok
	}
	s.aggregateMu.Lock()
	defer s.aggregateMu.Unlock()
	s.refreshAggregate(ctx, "repo_coverage", &stamp, refresh)
	if stamp.valid || s.repoCoverage.Load() == nil {
		t.Fatal("overlapping commit must permit publishing but prohibit reuse")
	}
	first := s.repoCoverage.Load()
	s.refreshAggregate(ctx, "repo_coverage", &stamp, refresh)
	if !stamp.valid || calls != 2 || s.repoCoverage.Load() == first {
		t.Fatal("unstable result was not retried and replaced")
	}
	s.refreshAggregate(ctx, "repo_coverage", &stamp, refresh)
	if calls != 2 {
		t.Fatal("stable replacement was not reused")
	}
}

func TestAggregateRefreshFailedStageRetriesIndependently(t *testing.T) {
	s := aggregateTestServer(t)
	ctx := context.Background()
	s.refreshDatabaseAggregates(ctx)
	priorCoverage := s.repoCoverage.Load()
	if err := s.cfg.Cache.PutBlob(ctx, strings.Repeat("d", 64), 1); err != nil {
		t.Fatal(err)
	}
	s.aggregateMu.Lock()
	s.refreshAggregate(ctx, "cache_summary", &s.summaryRevision, s.refreshCacheSummary)
	failedCtx, cancel := context.WithCancel(ctx)
	cancel()
	s.refreshAggregate(ctx, "repo_coverage", &s.coverageRevision, func(context.Context) bool {
		return s.refreshRepoCoverage(failedCtx)
	})
	s.aggregateMu.Unlock()
	if s.coverageRevision.valid || s.repoCoverage.Load() != priorCoverage {
		t.Fatal("failed query must invalidate reuse and retain the prior result")
	}
	priorSummary := s.cacheSummaryByHostArch.Load()
	// No writes between the failed read and this retry.
	s.refreshDatabaseAggregates(ctx)
	if s.repoCoverage.Load() == priorCoverage || s.cacheSummaryByHostArch.Load() != priorSummary {
		t.Error("failed stage must retry while successful stage reuses its result")
	}
	assertAggregateRuns(t, s, "repo_coverage", 3)
	assertAggregateRuns(t, s, "cache_summary", 2)
	assertAggregateMetric(t, s, `acu_admin_refresh_failures_total{stage="repo_coverage"}`, 1)
	assertAggregateMetric(t, s, `acu_admin_refresh_reused_total{stage="cache_summary"}`, 1)
}

func TestAggregateRefreshRevisionErrorInvalidatesBaseline(t *testing.T) {
	s := aggregateTestServer(t)
	ctx := context.Background()
	s.refreshDatabaseAggregates(ctx)
	coverage, summary := s.repoCoverage.Load(), s.cacheSummaryByHostArch.Load()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	s.refreshDatabaseAggregates(canceled)
	if s.coverageRevision.valid || s.summaryRevision.valid {
		t.Fatal("failed revision observation retained reusable baseline")
	}
	if s.repoCoverage.Load() != coverage || s.cacheSummaryByHostArch.Load() != summary {
		t.Fatal("failed observation replaced successful results")
	}
	s.refreshDatabaseAggregates(ctx)
	if s.repoCoverage.Load() == coverage || s.cacheSummaryByHostArch.Load() == summary {
		t.Fatal("revision observation failure did not force retry without a write")
	}
	if !s.coverageRevision.valid || !s.summaryRevision.valid {
		t.Fatal("successful retry did not restore reusable baselines")
	}
}

func TestAggregateRefreshClosedCacheDoesNotReuse(t *testing.T) {
	s := aggregateTestServer(t)
	ctx := context.Background()
	s.refreshDatabaseAggregates(ctx)
	coverage, summary := s.repoCoverage.Load(), s.cacheSummaryByHostArch.Load()
	if err := s.cfg.Cache.Close(); err != nil {
		t.Fatal(err)
	}
	s.refreshDatabaseAggregates(ctx)
	if s.coverageRevision.valid || s.summaryRevision.valid {
		t.Error("closed cache retained reusable baselines")
	}
	if s.repoCoverage.Load() != coverage || s.cacheSummaryByHostArch.Load() != summary {
		t.Error("closed cache did not retain prior successful values")
	}
	for _, stage := range []string{"repo_coverage", "cache_summary"} {
		assertAggregateRuns(t, s, stage, 2)
		assertAggregateMetric(t, s, `acu_admin_refresh_failures_total{stage="`+stage+`"}`, 1)
		assertAggregateMetric(t, s, `acu_admin_refresh_reused_total{stage="`+stage+`"}`, 0)
	}
}
