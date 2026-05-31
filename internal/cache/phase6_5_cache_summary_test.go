package cache

import (
	"context"
	"strings"
	"testing"
)

// TestGetCacheSummaryByHostArch_EmptyCache: with no adoption committed,
// the map is empty (not nil — Go maps differ here). SPEC6_5 §2.4
// surface treats empty as "no host has package_hash rows yet".
func TestGetCacheSummaryByHostArch_EmptyCache(t *testing.T) {
	c := openCache(t)
	got, err := c.GetCacheSummaryByHostArch(context.Background())
	if err != nil {
		t.Fatalf("GetCacheSummaryByHostArch: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map on empty cache, got %v", got)
	}
}

// TestGetCacheSummaryByHostArch_MultiArchAndHost: seeds two hosts with
// disjoint architectures and asserts both query halves (package_hash
// count + blob count/bytes) attribute to the right (host, arch) bucket.
func TestGetCacheSummaryByHostArch_MultiArchAndHost(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	hashes := map[string][]byte{
		"amd64-foo":  []byte("amd64 foo body"),
		"arm64-foo":  []byte("arm64 foo body — slightly longer"),
		"source-bar": []byte("source bar payload"),
		"sec-amd64":  []byte("sec amd64 deb bytes"),
	}
	blobIDs := map[string]string{}
	for k, body := range hashes {
		w, err := c.NewTempBlob()
		if err != nil {
			t.Fatalf("NewTempBlob[%s]: %v", k, err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatalf("Write[%s]: %v", k, err)
		}
		h, err := w.Finalize(int64(len(body)))
		if err != nil {
			t.Fatalf("Finalize[%s]: %v", k, err)
		}
		if err := c.PutBlob(ctx, h, int64(len(body))); err != nil {
			t.Fatalf("PutBlob[%s]: %v", k, err)
		}
		blobIDs[k] = h
	}

	type seed struct {
		scheme, host, suite, debPath, arch, pkg, blobKey string
	}
	seeds := []seed{
		{"http", "archive.ubuntu.com", "/ubuntu/dists/noble",
			"/ubuntu/pool/main/f/foo/foo_1.0_amd64.deb", "amd64", "foo", "amd64-foo"},
		{"http", "archive.ubuntu.com", "/ubuntu/dists/noble",
			"/ubuntu/pool/main/f/foo/foo_1.0_arm64.deb", "arm64", "foo", "arm64-foo"},
		{"http", "archive.ubuntu.com", "/ubuntu/dists/noble",
			"/ubuntu/pool/main/b/bar/bar_2.0.dsc", "source", "bar", "source-bar"},
		{"http", "security.ubuntu.com", "/ubuntu/dists/noble-security",
			"/ubuntu/pool/main/s/sec/sec_0.1_amd64.deb", "amd64", "sec", "sec-amd64"},
	}

	suitesSeen := map[string]bool{}
	for _, s := range seeds {
		key := s.host + "|" + s.suite
		if !suitesSeen[key] {
			if err := c.PutSuiteFreshness(ctx, SuiteFreshness{
				CanonicalScheme: s.scheme, CanonicalHost: s.host, SuitePath: s.suite,
			}); err != nil {
				t.Fatalf("PutSuiteFreshness[%s]: %v", key, err)
			}
			suitesSeen[key] = true
		}
	}

	suiteToSnapshot := map[string]int64{}
	for key := range suitesSeen {
		parts := strings.SplitN(key, "|", 2)
		host, suite := parts[0], parts[1]
		releaseHash := blobIDs["amd64-foo"] // arbitrary; FK target
		id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
			CanonicalScheme: "http", CanonicalHost: host, SuitePath: suite,
			InReleaseHash: &releaseHash,
		})
		if err != nil {
			t.Fatalf("InsertCandidateSnapshot[%s]: %v", key, err)
		}
		suiteToSnapshot[key] = id
	}

	var pkgHashes []PackageHash
	for _, s := range seeds {
		key := s.host + "|" + s.suite
		blob := blobIDs[s.blobKey]
		pkgHashes = append(pkgHashes, PackageHash{
			CanonicalScheme: s.scheme,
			CanonicalHost:   s.host,
			Path:            s.debPath,
			DeclaredSHA256:  blob,
			SnapshotID:      suiteToSnapshot[key],
			PackageName:     s.pkg,
			Architecture:    s.arch,
		})
		if err := c.PutURLPath(ctx, URLPath{
			CanonicalScheme: s.scheme,
			CanonicalHost:   s.host,
			Path:            s.debPath,
			BlobHash:        &blob,
			UpstreamURL:     "http://" + s.host + s.debPath,
		}); err != nil {
			t.Fatalf("PutURLPath[%s]: %v", s.debPath, err)
		}
	}

	bySnap := map[int64][]PackageHash{}
	for _, p := range pkgHashes {
		bySnap[p.SnapshotID] = append(bySnap[p.SnapshotID], p)
	}
	for id, phs := range bySnap {
		if err := c.CommitAdoption(ctx, id, nil, phs, nil, false); err != nil {
			t.Fatalf("CommitAdoption[%d]: %v", id, err)
		}
	}

	got, err := c.GetCacheSummaryByHostArch(ctx)
	if err != nil {
		t.Fatalf("GetCacheSummaryByHostArch: %v", err)
	}
	want := map[string]map[string]CacheSummaryEntry{
		"archive.ubuntu.com": {
			"amd64":  {PackageHashCount: 1, BlobCount: 1, BlobBytes: int64(len(hashes["amd64-foo"]))},
			"arm64":  {PackageHashCount: 1, BlobCount: 1, BlobBytes: int64(len(hashes["arm64-foo"]))},
			"source": {PackageHashCount: 1, BlobCount: 1, BlobBytes: int64(len(hashes["source-bar"]))},
		},
		"security.ubuntu.com": {
			"amd64": {PackageHashCount: 1, BlobCount: 1, BlobBytes: int64(len(hashes["sec-amd64"]))},
		},
	}
	if len(got) != len(want) {
		t.Fatalf("host count = %d, want %d (got %v)", len(got), len(want), got)
	}
	for host, wantArches := range want {
		gotArches, ok := got[host]
		if !ok {
			t.Errorf("host %q missing from result", host)
			continue
		}
		if len(gotArches) != len(wantArches) {
			t.Errorf("host %q arch count = %d, want %d (got %v)",
				host, len(gotArches), len(wantArches), gotArches)
		}
		for arch, wantEntry := range wantArches {
			gotEntry, ok := gotArches[arch]
			if !ok {
				t.Errorf("host %q arch %q missing", host, arch)
				continue
			}
			if gotEntry != wantEntry {
				t.Errorf("%s/%s = %+v, want %+v", host, arch, gotEntry, wantEntry)
			}
		}
	}
}

// TestGetCacheSummaryByHostArch_NoBlobForUrlPath: a package_hash row
// whose path has no url_path row (URL declared but never requested)
// contributes 1 to PackageHashCount and 0 to BlobCount / BlobBytes —
// the (host, arch) bucket exists from query 1 but is invisible to
// query 2.
func TestGetCacheSummaryByHostArch_NoBlobForUrlPath(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	w, err := c.NewTempBlob()
	if err != nil {
		t.Fatalf("NewTempBlob: %v", err)
	}
	body := []byte("inrelease body")
	if _, err := w.Write(body); err != nil {
		t.Fatalf("Write: %v", err)
	}
	releaseHash, err := w.Finalize(int64(len(body)))
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if err := c.PutBlob(ctx, releaseHash, int64(len(body))); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}

	if err := c.PutSuiteFreshness(ctx, SuiteFreshness{
		CanonicalScheme: "http",
		CanonicalHost:   "deb.debian.org",
		SuitePath:       "/debian/dists/bookworm",
	}); err != nil {
		t.Fatalf("PutSuiteFreshness: %v", err)
	}
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "deb.debian.org",
		SuitePath:       "/debian/dists/bookworm",
		InReleaseHash:   &releaseHash,
	})
	if err != nil {
		t.Fatalf("InsertCandidateSnapshot: %v", err)
	}
	if err := c.CommitAdoption(ctx, id, nil, []PackageHash{{
		CanonicalScheme: "http",
		CanonicalHost:   "deb.debian.org",
		Path:            "/debian/pool/main/a/aaa/aaa_1.0_amd64.deb",
		DeclaredSHA256:  releaseHash,
		SnapshotID:      id,
		PackageName:     "aaa",
		Architecture:    "amd64",
	}}, nil, false); err != nil {
		t.Fatalf("CommitAdoption: %v", err)
	}

	got, err := c.GetCacheSummaryByHostArch(ctx)
	if err != nil {
		t.Fatalf("GetCacheSummaryByHostArch: %v", err)
	}
	entry, ok := got["deb.debian.org"]["amd64"]
	if !ok {
		t.Fatalf("(deb.debian.org, amd64) missing from result: %v", got)
	}
	if entry.PackageHashCount != 1 {
		t.Errorf("PackageHashCount = %d, want 1", entry.PackageHashCount)
	}
	if entry.BlobCount != 0 {
		t.Errorf("BlobCount = %d, want 0 (no url_path → no blob)", entry.BlobCount)
	}
	if entry.BlobBytes != 0 {
		t.Errorf("BlobBytes = %d, want 0 (no url_path → no blob)", entry.BlobBytes)
	}
}

// TestGetCacheSummaryByHostArch_SharedBlobDedup: two distinct .deb
// paths in the same (host, arch) current snapshot that resolve to the
// SAME blob must count that blob ONCE (the blob subquery is DISTINCT on
// hash), while PackageHashCount counts both package_hash rows. This pins
// the DISTINCT semantics that the blob-subquery rewrite must preserve.
func TestGetCacheSummaryByHostArch_SharedBlobDedup(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	body := []byte("shared deb payload counted once")
	w, err := c.NewTempBlob()
	if err != nil {
		t.Fatalf("NewTempBlob: %v", err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatalf("Write: %v", err)
	}
	h, err := w.Finalize(int64(len(body)))
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if err := c.PutBlob(ctx, h, int64(len(body))); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}

	const host, suite = "archive.ubuntu.com", "/ubuntu/dists/noble"
	if err := c.PutSuiteFreshness(ctx, SuiteFreshness{
		CanonicalScheme: "http", CanonicalHost: host, SuitePath: suite,
	}); err != nil {
		t.Fatalf("PutSuiteFreshness: %v", err)
	}
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: host, SuitePath: suite, InReleaseHash: &h,
	})
	if err != nil {
		t.Fatalf("InsertCandidateSnapshot: %v", err)
	}

	// Two distinct deb paths (e.g. a binNMU rebuild) sharing one blob.
	paths := []string{
		"/ubuntu/pool/main/f/foo/foo_1.0_amd64.deb",
		"/ubuntu/pool/main/f/foo/foo_1.0+b1_amd64.deb",
	}
	var phs []PackageHash
	for _, p := range paths {
		bh := h
		phs = append(phs, PackageHash{
			CanonicalScheme: "http", CanonicalHost: host, Path: p,
			DeclaredSHA256: h, SnapshotID: id, PackageName: "foo", Architecture: "amd64",
		})
		if err := c.PutURLPath(ctx, URLPath{
			CanonicalScheme: "http", CanonicalHost: host, Path: p,
			BlobHash: &bh, UpstreamURL: "http://" + host + p,
		}); err != nil {
			t.Fatalf("PutURLPath[%s]: %v", p, err)
		}
	}
	if err := c.CommitAdoption(ctx, id, nil, phs, nil, false); err != nil {
		t.Fatalf("CommitAdoption: %v", err)
	}

	got, err := c.GetCacheSummaryByHostArch(ctx)
	if err != nil {
		t.Fatalf("GetCacheSummaryByHostArch: %v", err)
	}
	e, ok := got[host]["amd64"]
	if !ok {
		t.Fatalf("(%s, amd64) missing: %v", host, got)
	}
	if e.PackageHashCount != 2 {
		t.Errorf("PackageHashCount = %d, want 2 (both paths)", e.PackageHashCount)
	}
	if e.BlobCount != 1 {
		t.Errorf("BlobCount = %d, want 1 (shared blob deduped)", e.BlobCount)
	}
	if e.BlobBytes != int64(len(body)) {
		t.Errorf("BlobBytes = %d, want %d (counted once)", e.BlobBytes, len(body))
	}
}

// TestGetCacheSummaryByHostArch_ExcludesEmptyArchRows: pre-v3 rows
// with empty architecture cannot be attributed to any bucket — they
// must not surface (would otherwise appear under an empty-string arch
// key).
func TestGetCacheSummaryByHostArch_ExcludesEmptyArchRows(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	w, err := c.NewTempBlob()
	if err != nil {
		t.Fatalf("NewTempBlob: %v", err)
	}
	body := []byte("legacy v2 inrelease")
	if _, err := w.Write(body); err != nil {
		t.Fatalf("Write: %v", err)
	}
	releaseHash, err := w.Finalize(int64(len(body)))
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if err := c.PutBlob(ctx, releaseHash, int64(len(body))); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}

	if err := c.PutSuiteFreshness(ctx, SuiteFreshness{
		CanonicalScheme: "http", CanonicalHost: "legacy.example", SuitePath: "/legacy/dists/old",
	}); err != nil {
		t.Fatalf("PutSuiteFreshness: %v", err)
	}
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "legacy.example", SuitePath: "/legacy/dists/old",
		InReleaseHash: &releaseHash,
	})
	if err != nil {
		t.Fatalf("InsertCandidateSnapshot: %v", err)
	}
	if err := c.CommitAdoption(ctx, id, nil, []PackageHash{{
		CanonicalScheme: "http", CanonicalHost: "legacy.example",
		Path:           "/legacy/pool/anything.deb",
		DeclaredSHA256: releaseHash,
		SnapshotID:     id,
		// PackageName & Architecture: zero (legacy v2 row)
	}}, nil, false); err != nil {
		t.Fatalf("CommitAdoption: %v", err)
	}

	got, err := c.GetCacheSummaryByHostArch(ctx)
	if err != nil {
		t.Fatalf("GetCacheSummaryByHostArch: %v", err)
	}
	if entry, ok := got["legacy.example"]; ok {
		if _, hasEmpty := entry[""]; hasEmpty {
			t.Errorf("empty-arch bucket leaked into cache_summary: %v", entry)
		}
		// The host may still appear with an empty inner map (no rows
		// matched), or may be missing entirely — either is acceptable;
		// the contract is "no empty-string arch key".
	}
}
