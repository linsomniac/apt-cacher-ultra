package admin

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// withAdoptionArchitectures injects the SPEC6_5 §5.1 architectures
// allowlist into the admin Config so the §2.4 repo_coverage.architectures_filter
// field can be exercised end-to-end.
func withAdoptionArchitectures(arches []string) adminOpt {
	return func(cfg *Config) {
		cfg.AdoptionArchitectures = arches
	}
}

// TestStatusJSON_RepoCoverage_EmptyCache: SPEC6_5 §2.4. With no
// adoption committed, repo_coverage is present with zero counts and
// empty arrays — never JSON null — so consumers can rely on the
// schema shape from process start.
func TestStatusJSON_RepoCoverage_EmptyCache(t *testing.T) {
	_, base, cleanup := startAdminServer(t)
	defer cleanup()

	resp := mustGet(t, base+"/?format=json")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	var got struct {
		RepoCoverage struct {
			ArchitecturesSeen    []string `json:"architectures_seen"`
			ArchitecturesFilter  []string `json:"architectures_filter"`
			SnapshotsWithSources int64    `json:"snapshots_with_sources"`
			SnapshotsWithPdiff   int64    `json:"snapshots_with_pdiff"`
			PackageHashRows      struct {
				Binary int64 `json:"binary"`
				Source int64 `json:"source"`
				Pdiff  int64 `json:"pdiff"`
				Total  int64 `json:"total"`
			} `json:"package_hash_rows"`
		} `json:"repo_coverage"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}

	rc := got.RepoCoverage
	if rc.ArchitecturesSeen == nil {
		t.Errorf("architectures_seen is null; want [] (empty array)")
	}
	if rc.ArchitecturesFilter == nil {
		t.Errorf("architectures_filter is null; want [] (empty array)")
	}
	if rc.SnapshotsWithSources != 0 ||
		rc.SnapshotsWithPdiff != 0 ||
		rc.PackageHashRows.Total != 0 {
		t.Errorf("expected all zero counts on empty cache, got %+v", rc)
	}
}

// TestStatusJSON_RepoCoverage_FilterEchoed: SPEC6_5 §2.4
// architectures_filter mirrors the operator's [adoption].architectures
// allowlist verbatim.
func TestStatusJSON_RepoCoverage_FilterEchoed(t *testing.T) {
	_, base, cleanup := startAdminServer(t,
		withAdoptionArchitectures([]string{"amd64", "arm64", "source"}))
	defer cleanup()

	resp := mustGet(t, base+"/?format=json")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	var got struct {
		RepoCoverage struct {
			ArchitecturesFilter []string `json:"architectures_filter"`
		} `json:"repo_coverage"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []string{"amd64", "arm64", "source"}
	if len(got.RepoCoverage.ArchitecturesFilter) != len(want) {
		t.Fatalf("filter len = %d, want %d (%v)",
			len(got.RepoCoverage.ArchitecturesFilter), len(want),
			got.RepoCoverage.ArchitecturesFilter)
	}
	for i := range want {
		if got.RepoCoverage.ArchitecturesFilter[i] != want[i] {
			t.Errorf("filter[%d] = %q, want %q",
				i, got.RepoCoverage.ArchitecturesFilter[i], want[i])
		}
	}
}

// seedRepoCoverageSnapshot commits a single inline snapshot containing
// the given package_hash rows and an InRelease metadata-self entry,
// then forces a refresher pass so the cached repo_coverage /
// cache_summary atomic.Pointers reflect the seeded state before the
// caller's HTTP probe. Used to drive the SPEC6_5 §2.4 / §2.5 status
// assertions without running the full freshness adoption flow.
//
// AIDEV-NOTE: the SPEC6_5 §9.7.6 migration of repo_coverage to the
// refresher-cached path means a /?format=json hit issued right after
// CommitAdoption returns would otherwise see stale (pre-seed) cached
// values. Explicitly invoking runRefreshOnce closes the test-timing
// race without depending on the test's wall-clock vs gauge_refresh
// tick interval.
func seedRepoCoverageSnapshot(t *testing.T, s *Server, c *cache.Cache,
	scheme, host, suite string,
	memberPaths []string, pkgHashes []cache.PackageHash) int64 {
	t.Helper()

	w, err := c.NewTempBlob()
	if err != nil {
		t.Fatalf("NewTempBlob: %v", err)
	}
	body := []byte("InRelease for " + suite)
	if _, err := w.Write(body); err != nil {
		t.Fatalf("Write: %v", err)
	}
	releaseHash, err := w.Finalize(int64(len(body)))
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if err := c.PutBlob(context.Background(), releaseHash, int64(len(body))); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}

	if err := c.PutSuiteFreshness(context.Background(), cache.SuiteFreshness{
		CanonicalScheme: scheme,
		CanonicalHost:   host,
		SuitePath:       suite,
	}); err != nil {
		t.Fatalf("PutSuiteFreshness: %v", err)
	}
	id, _, err := c.InsertCandidateSnapshot(context.Background(),
		cache.SnapshotCandidate{
			CanonicalScheme: scheme,
			CanonicalHost:   host,
			SuitePath:       suite,
			InReleaseHash:   &releaseHash,
		})
	if err != nil {
		t.Fatalf("InsertCandidateSnapshot: %v", err)
	}

	members := []cache.SnapshotMember{
		{SnapshotID: id, Path: "InRelease", BlobHash: releaseHash, DeclaredSHA256: releaseHash},
	}
	for _, mp := range memberPaths {
		members = append(members, cache.SnapshotMember{
			SnapshotID:     id,
			Path:           mp,
			BlobHash:       releaseHash, // reuse release blob for member-FK
			DeclaredSHA256: releaseHash,
		})
	}
	for i := range pkgHashes {
		pkgHashes[i].SnapshotID = id
	}
	if err := c.CommitAdoption(context.Background(), id, members, pkgHashes, nil, false); err != nil {
		t.Fatalf("CommitAdoption: %v", err)
	}
	s.runRefreshOnce(context.Background())
	return id
}

// TestStatusJSON_RepoCoverage_PerKindCounts: SPEC6_5 §2.4
// package_hash_rows breakdown distinguishes binary vs source vs pdiff
// based on the path-pattern + architecture predicate.
func TestStatusJSON_RepoCoverage_PerKindCounts(t *testing.T) {
	s, base, cleanup := startAdminServer(t)
	defer cleanup()

	const (
		scheme = "http"
		host   = "archive.ubuntu.com"
		suite  = "/ubuntu/dists/noble"
	)
	hash := strings.Repeat("a", 64)
	pkgs := []cache.PackageHash{
		// 2 binary rows
		{
			CanonicalScheme: scheme, CanonicalHost: host,
			Path:           "/ubuntu/pool/main/f/foo/foo_1.0_amd64.deb",
			DeclaredSHA256: hash, Architecture: "amd64", PackageName: "foo",
		},
		{
			CanonicalScheme: scheme, CanonicalHost: host,
			Path:           "/ubuntu/pool/main/f/foo/foo_1.0_arm64.deb",
			DeclaredSHA256: hash, Architecture: "arm64", PackageName: "foo",
		},
		// 1 source row
		{
			CanonicalScheme: scheme, CanonicalHost: host,
			Path:           "/ubuntu/pool/main/b/bash/bash_5.1-2.dsc",
			DeclaredSHA256: hash, Architecture: "source", PackageName: "bash",
		},
		// 1 pdiff row
		{
			CanonicalScheme: scheme, CanonicalHost: host,
			Path:           "/ubuntu/main/binary-amd64/Packages.diff/2026-05-09-1234.56.gz",
			DeclaredSHA256: hash, Architecture: "amd64",
		},
	}
	memberPaths := []string{
		"main/binary-amd64/Packages.diff/Index", // drives snapshots_with_pdiff
	}
	seedRepoCoverageSnapshot(t, s, s.cfg.Cache, scheme, host, suite, memberPaths, pkgs)

	resp := mustGet(t, base+"/?format=json")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	var got struct {
		RepoCoverage struct {
			ArchitecturesSeen    []string `json:"architectures_seen"`
			SnapshotsWithSources int64    `json:"snapshots_with_sources"`
			SnapshotsWithPdiff   int64    `json:"snapshots_with_pdiff"`
			PackageHashRows      struct {
				Binary int64 `json:"binary"`
				Source int64 `json:"source"`
				Pdiff  int64 `json:"pdiff"`
				Total  int64 `json:"total"`
			} `json:"package_hash_rows"`
		} `json:"repo_coverage"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}

	rc := got.RepoCoverage
	if rc.PackageHashRows.Binary != 2 {
		t.Errorf("binary count = %d, want 2", rc.PackageHashRows.Binary)
	}
	if rc.PackageHashRows.Source != 1 {
		t.Errorf("source count = %d, want 1", rc.PackageHashRows.Source)
	}
	if rc.PackageHashRows.Pdiff != 1 {
		t.Errorf("pdiff count = %d, want 1", rc.PackageHashRows.Pdiff)
	}
	if rc.PackageHashRows.Total != 4 {
		t.Errorf("total count = %d, want 4", rc.PackageHashRows.Total)
	}
	if rc.SnapshotsWithSources != 1 {
		t.Errorf("snapshots_with_sources = %d, want 1", rc.SnapshotsWithSources)
	}
	if rc.SnapshotsWithPdiff != 1 {
		t.Errorf("snapshots_with_pdiff = %d, want 1", rc.SnapshotsWithPdiff)
	}
	// architectures_seen union: amd64, arm64, source (sorted by SQL).
	want := map[string]bool{"amd64": true, "arm64": true, "source": true}
	if len(rc.ArchitecturesSeen) != len(want) {
		t.Errorf("architectures_seen len = %d, want %d (got %v)",
			len(rc.ArchitecturesSeen), len(want), rc.ArchitecturesSeen)
	}
	for _, a := range rc.ArchitecturesSeen {
		if !want[a] {
			t.Errorf("unexpected architecture %q in architectures_seen", a)
		}
	}
}

// TestStatusHTML_RepoCoverage_RendersSection: SPEC6_5 §2.5. The HTML
// page contains the "Repository coverage" header and renders the
// table cells without panicking on empty / unfiltered state.
func TestStatusHTML_RepoCoverage_RendersSection(t *testing.T) {
	_, base, cleanup := startAdminServer(t,
		withAdoptionArchitectures([]string{"amd64", "source"}))
	defer cleanup()

	resp := mustGet(t, base+"/")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	html := string(body)
	if !strings.Contains(html, ">Repository coverage<") {
		t.Errorf("HTML missing 'Repository coverage' section header")
	}
	if !strings.Contains(html, "Architectures seen") {
		t.Errorf("HTML missing 'Architectures seen' row")
	}
	if !strings.Contains(html, "Architectures filter") {
		t.Errorf("HTML missing 'Architectures filter' row")
	}
	// Architectures filter render: the seeded list is amd64 + source.
	if !strings.Contains(html, "<code>amd64</code>") || !strings.Contains(html, "<code>source</code>") {
		t.Errorf("HTML missing rendered filter values amd64/source")
	}
	if !strings.Contains(html, "package_hash kind") {
		t.Errorf("HTML missing per-kind row count table")
	}
}

// TestStatusHTML_RepoCoverage_UnfilteredRendersHint: when the operator
// has no architectures filter, the HTML renders an explicit
// "(unfiltered)" hint so the page never shows a confusingly-empty cell.
func TestStatusHTML_RepoCoverage_UnfilteredRendersHint(t *testing.T) {
	_, base, cleanup := startAdminServer(t)
	defer cleanup()

	resp := mustGet(t, base+"/")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)
	if !strings.Contains(html, "(unfiltered") {
		t.Errorf("HTML missing the (unfiltered) hint when no filter is configured; got:\n%s", html)
	}
}

// TestStatusJSON_RepoCoverage_PdiffPathClassifierIsCaseSensitive:
// regression for codex review on commit 898cbfe — the SQLite default
// LIKE operator is ASCII case-insensitive, which would let a lowercase
// "packages.diff/foo.gz" path leak into the pdiff bucket here even
// though handler.classifyPath would not call it pdiff at serve time.
// GetRepoCoverage uses GLOB (case-sensitive) for parity with the
// classifier; this test pins that property by seeding lowercase
// near-matches and asserting they are NOT counted as pdiff.
func TestStatusJSON_RepoCoverage_PdiffPathClassifierIsCaseSensitive(t *testing.T) {
	s, base, cleanup := startAdminServer(t)
	defer cleanup()

	const (
		scheme = "http"
		host   = "archive.ubuntu.com"
		suite  = "/ubuntu/dists/noble"
	)
	hash := strings.Repeat("b", 64)
	pkgs := []cache.PackageHash{
		// Lowercase "packages.diff" — handler.classifyPath calls this
		// "other", not "pdiff". GetRepoCoverage must agree by using GLOB.
		{
			CanonicalScheme: scheme, CanonicalHost: host,
			Path:           "/ubuntu/main/binary-amd64/packages.diff/2026-05-09-1234.56.gz",
			DeclaredSHA256: hash, Architecture: "amd64",
		},
		// Lowercase "sources.diff" — same case, source bucket.
		{
			CanonicalScheme: scheme, CanonicalHost: host,
			Path:           "/ubuntu/main/source/sources.diff/2026-05-09-1234.56.gz",
			DeclaredSHA256: hash, Architecture: "source", PackageName: "bash",
		},
	}
	memberPaths := []string{
		// Lowercase Index path — must NOT inflate snapshots_with_pdiff.
		"main/binary-amd64/packages.diff/Index",
	}
	seedRepoCoverageSnapshot(t, s, s.cfg.Cache, scheme, host, suite, memberPaths, pkgs)

	resp := mustGet(t, base+"/?format=json")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	var got struct {
		RepoCoverage struct {
			SnapshotsWithPdiff int64 `json:"snapshots_with_pdiff"`
			PackageHashRows    struct {
				Binary int64 `json:"binary"`
				Source int64 `json:"source"`
				Pdiff  int64 `json:"pdiff"`
			} `json:"package_hash_rows"`
		} `json:"repo_coverage"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}

	rc := got.RepoCoverage
	if rc.PackageHashRows.Pdiff != 0 {
		t.Errorf("lowercase packages.diff/sources.diff paths leaked into pdiff bucket: got %d, want 0",
			rc.PackageHashRows.Pdiff)
	}
	if rc.SnapshotsWithPdiff != 0 {
		t.Errorf("lowercase packages.diff/Index leaked into snapshots_with_pdiff: got %d, want 0",
			rc.SnapshotsWithPdiff)
	}
	// Sanity: the rows still land somewhere — the amd64 row goes to
	// "other" (kind=other isn't surfaced in the JSON contract, but it
	// counts toward Total via PackageHashRowsTotal). The source row
	// goes to source.
	if rc.PackageHashRows.Source != 1 {
		t.Errorf("lowercase sources.diff path with arch=source did not fall to source bucket: got %d, want 1",
			rc.PackageHashRows.Source)
	}
}
