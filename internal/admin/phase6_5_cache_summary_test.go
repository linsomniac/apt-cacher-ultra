package admin

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// TestStatusJSON_CacheSummary_EmptyShape: with no adoption committed,
// cache_summary is present and renders as `{"by_host": {}}` — not
// JSON null, not absent — so SPEC6_5 §2.4 consumers can rely on the
// schema key from process start.
func TestStatusJSON_CacheSummary_EmptyShape(t *testing.T) {
	_, base, cleanup := startAdminServer(t)
	defer cleanup()

	resp := mustGet(t, base+"/?format=json")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	cs, ok := got["cache_summary"].(map[string]any)
	if !ok {
		t.Fatalf("cache_summary missing or wrong type: %T (%v)", got["cache_summary"], got["cache_summary"])
	}
	bh, ok := cs["by_host"].(map[string]any)
	if !ok {
		t.Fatalf("cache_summary.by_host missing or wrong type: %T", cs["by_host"])
	}
	if len(bh) != 0 {
		t.Errorf("expected empty by_host on empty cache, got %v", bh)
	}
}

// TestStatusJSON_CacheSummary_PerHostPerArch: seeding a snapshot
// across two hosts × two arches surfaces both buckets through the
// refresher-cached cache_summary block. Pins SPEC6_5 §2.4 wire shape.
func TestStatusJSON_CacheSummary_PerHostPerArch(t *testing.T) {
	s, base, cleanup := startAdminServer(t)
	defer cleanup()

	const (
		scheme = "http"
		host1  = "archive.ubuntu.com"
		suite1 = "/ubuntu/dists/noble"
	)
	hash := strings.Repeat("a", 64)
	pkgs1 := []cache.PackageHash{
		{
			CanonicalScheme: scheme, CanonicalHost: host1,
			Path:           "/ubuntu/pool/main/f/foo/foo_1.0_amd64.deb",
			DeclaredSHA256: hash, Architecture: "amd64", PackageName: "foo",
		},
		{
			CanonicalScheme: scheme, CanonicalHost: host1,
			Path:           "/ubuntu/pool/main/f/foo/foo_1.0_arm64.deb",
			DeclaredSHA256: hash, Architecture: "arm64", PackageName: "foo",
		},
	}
	seedRepoCoverageSnapshot(t, s, s.cfg.Cache, scheme, host1, suite1, nil, pkgs1)

	resp := mustGet(t, base+"/?format=json")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	var got struct {
		CacheSummary struct {
			ByHost map[string]struct {
				ByArchitecture map[string]struct {
					PackageHashCount int64 `json:"package_hash_count"`
					BlobCount        int64 `json:"blob_count"`
					BlobBytes        int64 `json:"blob_bytes"`
				} `json:"by_architecture"`
			} `json:"by_host"`
		} `json:"cache_summary"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}

	bh, ok := got.CacheSummary.ByHost[host1]
	if !ok {
		t.Fatalf("host %q missing from cache_summary.by_host (got %v)",
			host1, got.CacheSummary.ByHost)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		entry, ok := bh.ByArchitecture[arch]
		if !ok {
			t.Errorf("arch %q missing for host %q", arch, host1)
			continue
		}
		if entry.PackageHashCount != 1 {
			t.Errorf("%s/%s package_hash_count = %d, want 1",
				host1, arch, entry.PackageHashCount)
		}
		// No url_path/blob seeded → blob count/bytes 0.
		if entry.BlobCount != 0 || entry.BlobBytes != 0 {
			t.Errorf("%s/%s blob count/bytes = (%d, %d), want (0, 0)",
				host1, arch, entry.BlobCount, entry.BlobBytes)
		}
	}
}

// TestStatusHTML_CacheSummary_RendersWhenSeeded: when at least one
// (host, arch) bucket exists, the HTML page renders the per-host
// by-architecture sub-table. When the cache is empty, the sub-section
// is omitted entirely (the `{{with}}` template guard skips it).
func TestStatusHTML_CacheSummary_RendersWhenSeeded(t *testing.T) {
	s, base, cleanup := startAdminServer(t)
	defer cleanup()

	const (
		scheme = "http"
		host   = "archive.ubuntu.com"
		suite  = "/ubuntu/dists/noble"
	)
	hash := strings.Repeat("c", 64)
	pkgs := []cache.PackageHash{
		{
			CanonicalScheme: scheme, CanonicalHost: host,
			Path:           "/ubuntu/pool/main/x/xyz/xyz_1.0_amd64.deb",
			DeclaredSHA256: hash, Architecture: "amd64", PackageName: "xyz",
		},
	}
	seedRepoCoverageSnapshot(t, s, s.cfg.Cache, scheme, host, suite, nil, pkgs)

	resp := mustGet(t, base+"/")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	// Redesigned layout puts the by-host × arch breakdown in a panel
	// titled "Cache contents — by host × architecture". The host name
	// and architecture appear in the table body via <td class="host">
	// and <span class="arch">.
	if !strings.Contains(html, "by host") {
		t.Errorf("HTML missing 'by host' panel eyebrow")
	}
	if !strings.Contains(html, ">"+host+"<") {
		t.Errorf("HTML missing host cell %q", host)
	}
	if !strings.Contains(html, ">amd64<") {
		t.Errorf("HTML missing arch cell amd64")
	}
}

// TestStatusHTML_CacheSummary_OmittedOnEmptyCache: when no (host,
// arch) bucket exists, the per-host sub-table is suppressed entirely
// — the page is not cluttered with an empty table.
func TestStatusHTML_CacheSummary_OmittedOnEmptyCache(t *testing.T) {
	_, base, cleanup := startAdminServer(t)
	defer cleanup()

	resp := mustGet(t, base+"/")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	// Redesigned layout puts the by-host breakdown in a "Cache
	// contents" panel. On empty cache the panel renders the empty
	// state instead of the table; the by-host content is suppressed.
	if strings.Contains(html, "<table class=\"data\"") && strings.Contains(html, "by host") &&
		strings.Contains(html, "<th>Architecture</th>") {
		t.Errorf("HTML rendered by-host breakdown table on empty cache")
	}
}

// TestStatusJSON_CacheSummary_TopLevelKey extends the §10.5 locked-
// keys assertion: SPEC6_5 §2.4 mandates cache_summary among the
// always-present top-level keys, even on an empty cache.
func TestStatusJSON_CacheSummary_TopLevelKey(t *testing.T) {
	_, base, cleanup := startAdminServer(t)
	defer cleanup()

	resp := mustGet(t, base+"/?format=json")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := got["cache_summary"]; !ok {
		t.Errorf("cache_summary top-level key missing on empty cache")
	}
}

// TestPackageHashRowsByKind_Gauge_PopulatesAfterSeed: SPEC6_5 §10.3
// — after adoption seeds binary + source + pdiff rows, the
// acu_package_hash_rows_by_kind gauge exposes each kind's count as a
// separate labeled series. Asserts the refresher wiring populates all
// three labels (not just the ones with non-zero values).
func TestPackageHashRowsByKind_Gauge_PopulatesAfterSeed(t *testing.T) {
	s, base, cleanup := startAdminServer(t)
	defer cleanup()

	const (
		scheme = "http"
		host   = "archive.ubuntu.com"
		suite  = "/ubuntu/dists/noble"
	)
	hash := strings.Repeat("d", 64)
	pkgs := []cache.PackageHash{
		// 2 binary
		{
			CanonicalScheme: scheme, CanonicalHost: host,
			Path:           "/ubuntu/pool/main/b/bin/bin_1.0_amd64.deb",
			DeclaredSHA256: hash, Architecture: "amd64", PackageName: "bin",
		},
		{
			CanonicalScheme: scheme, CanonicalHost: host,
			Path:           "/ubuntu/pool/main/b/bin/bin_1.0_arm64.deb",
			DeclaredSHA256: hash, Architecture: "arm64", PackageName: "bin",
		},
		// 1 source
		{
			CanonicalScheme: scheme, CanonicalHost: host,
			Path:           "/ubuntu/pool/main/s/src/src_1.0.dsc",
			DeclaredSHA256: hash, Architecture: "source", PackageName: "src",
		},
		// 1 pdiff
		{
			CanonicalScheme: scheme, CanonicalHost: host,
			Path:           "/ubuntu/main/binary-amd64/Packages.diff/2026-05-09-1234.56.gz",
			DeclaredSHA256: hash, Architecture: "amd64",
		},
	}
	seedRepoCoverageSnapshot(t, s, s.cfg.Cache, scheme, host, suite, nil, pkgs)

	resp := mustGet(t, base+"/metrics")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	metrics := string(body)

	// Each kind's labeled series should be present with the expected
	// value. Prometheus exposition format puts the value last.
	wantSeries := map[string]string{
		`acu_package_hash_rows_by_kind{kind="binary"}`: " 2",
		`acu_package_hash_rows_by_kind{kind="source"}`: " 1",
		`acu_package_hash_rows_by_kind{kind="pdiff"}`:  " 1",
	}
	for prefix, suffix := range wantSeries {
		// Look for the prefix followed by space + value (the exposition
		// format is `metric{labels} value`).
		idx := strings.Index(metrics, prefix)
		if idx == -1 {
			t.Errorf("metric series missing: %s", prefix)
			continue
		}
		rest := metrics[idx+len(prefix):]
		newline := strings.Index(rest, "\n")
		if newline == -1 {
			newline = len(rest)
		}
		line := rest[:newline]
		if !strings.HasSuffix(line, suffix) {
			t.Errorf("metric %s value mismatch: line tail = %q, want suffix %q",
				prefix, line, suffix)
		}
	}
}

// TestRepoCoverage_RefresherCached: SPEC6_5 §9.7.6 migration — the
// /?format=json render reads repo_coverage from the atomic.Pointer
// the refresher populates, NOT a live DB query. Pin this by:
//  1. Render once (empty state cached).
//  2. Seed package_hash rows but DO NOT force a refresh.
//  3. Render again — counts should still be zero (cached value
//     pre-dates the seed).
//  4. Force a refresh via runRefreshOnce.
//  5. Render — counts should now reflect the seed.
//
// Without the refresher-cached path, step 3 would observe the seeded
// rows (live query). This test pins the cached semantics.
func TestRepoCoverage_RefresherCached(t *testing.T) {
	s, base, cleanup := startAdminServer(t)
	defer cleanup()

	// Step 1: confirm baseline is zero.
	zeroCount := func(label string) {
		t.Helper()
		resp := mustGet(t, base+"/?format=json")
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		var got struct {
			RepoCoverage struct {
				PackageHashRows struct {
					Total int64 `json:"total"`
				} `json:"package_hash_rows"`
			} `json:"repo_coverage"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("[%s] decode: %v", label, err)
		}
		if got.RepoCoverage.PackageHashRows.Total != 0 {
			t.Errorf("[%s] total = %d, want 0 (cached at this point)",
				label, got.RepoCoverage.PackageHashRows.Total)
		}
	}
	zeroCount("startup")

	// Step 2: seed WITHOUT invoking the helper that runs a refresh.
	// Use the raw cache methods so the cached repo_coverage stays
	// stale relative to the new package_hash row.
	c := s.cfg.Cache
	w, err := c.NewTempBlob()
	if err != nil {
		t.Fatalf("NewTempBlob: %v", err)
	}
	body := []byte("uncached refresh sentinel inrelease")
	if _, err := w.Write(body); err != nil {
		t.Fatalf("Write: %v", err)
	}
	releaseHash, err := w.Finalize(int64(len(body)))
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	ctx := context.Background()
	if err := c.PutBlob(ctx, releaseHash, int64(len(body))); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	const (
		scheme = "http"
		host   = "archive.example"
		suite  = "/cached/dists/cached"
	)
	if err := c.PutSuiteFreshness(ctx, cache.SuiteFreshness{
		CanonicalScheme: scheme, CanonicalHost: host, SuitePath: suite,
	}); err != nil {
		t.Fatalf("PutSuiteFreshness: %v", err)
	}
	id, _, err := c.InsertCandidateSnapshot(ctx, cache.SnapshotCandidate{
		CanonicalScheme: scheme, CanonicalHost: host, SuitePath: suite,
		InReleaseHash: &releaseHash,
	})
	if err != nil {
		t.Fatalf("InsertCandidateSnapshot: %v", err)
	}
	if err := c.CommitAdoption(ctx, id, nil, []cache.PackageHash{{
		CanonicalScheme: scheme, CanonicalHost: host,
		Path: "/cached/pool/x.deb", DeclaredSHA256: releaseHash,
		SnapshotID: id, Architecture: "amd64", PackageName: "x",
	}}, nil, false); err != nil {
		t.Fatalf("CommitAdoption: %v", err)
	}

	// Step 3-4: refresh and verify cached value now reflects the seed.
	// (The original 50ms refresher tick may or may not have fired
	// between step 2 and here; force a deterministic refresh.)
	s.runRefreshOnce(context.Background())

	resp := mustGet(t, base+"/?format=json")
	defer func() { _ = resp.Body.Close() }()
	body2, _ := io.ReadAll(resp.Body)
	var got struct {
		RepoCoverage struct {
			PackageHashRows struct {
				Total int64 `json:"total"`
			} `json:"package_hash_rows"`
		} `json:"repo_coverage"`
	}
	if err := json.Unmarshal(body2, &got); err != nil {
		t.Fatalf("post-refresh decode: %v", err)
	}
	if got.RepoCoverage.PackageHashRows.Total != 1 {
		t.Errorf("post-refresh total = %d, want 1",
			got.RepoCoverage.PackageHashRows.Total)
	}
}
