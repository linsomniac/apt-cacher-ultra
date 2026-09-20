package cache

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

// The admin aggregates must retain their current-snapshot and blob-dedup
// semantics across schemes, suites, legacy rows, and source/pdiff overlap.
func TestAdminAggregatesCurrentSnapshots(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	hash := func(n int) string { return fmt.Sprintf("%064x", n) }
	for _, blob := range []struct{ id, size int }{{100, 123}, {101, 456}} {
		exec(`INSERT INTO blob(hash, size, created_at) VALUES (?, ?, 1)`, hash(blob.id), blob.size)
	}
	type snapshot struct {
		scheme, host, suite string
		current             bool
	}
	snapshots := []snapshot{
		{"https", "mirror.example", "/dists/stable", true},
		{"https", "mirror.example", "/dists/testing", true},
		{"http", "mirror.example", "/dists/stable", true},
		{"https", "other.example", "/dists/stable", true},
		{"https", "mirror.example", "/dists/stable", false}, // retained old catalog
		{"https", "mirror.example", "/dists/members-only", true},
		{"https", "mirror.example", "/dists/candidate", false},
		{"https", "other.example", "/dists/pdiff-only", true},
	}
	for i, s := range snapshots {
		id := i + 1
		var adoptedAt any = 1
		if id == 7 {
			adoptedAt = nil
		}
		exec(`INSERT INTO blob(hash, size, created_at) VALUES (?, 1, 1)`, hash(id))
		exec(`INSERT INTO suite_snapshot(snapshot_id, canonical_scheme, canonical_host,
suite_path, inrelease_hash, created_at, adopted_at) VALUES (?, ?, ?, ?, ?, 1, ?)`,
			id, s.scheme, s.host, s.suite, hash(id), adoptedAt)
		if s.current {
			exec(`INSERT INTO suite_freshness(canonical_scheme, canonical_host, suite_path,
current_snapshot_id) VALUES (?, ?, ?, ?)`, s.scheme, s.host, s.suite, id)
		}
	}
	for _, p := range []struct {
		snapshot   int
		path, arch string
	}{
		{1, "/pool/shared.deb", "amd64"},
		{1, "/pool/source.dsc", "source"},
		{1, "/pool/legacy.deb", ""},
		{1, "/dists/stable/main/source/Sources.diff/patch.gz", "source"},
		{1, "/pool/packages.diff/patch.gz", "amd64"}, // lowercase is not pdiff
		{1, "/dists/stable/main/binary-amd64/Packages.diff/patch.gz", ""},
		{2, "/pool/shared.deb", "amd64"}, // same cached blob in two current suites
		{2, "/pool/testing-src.dsc", "source"},
		{2, "/pool/uncached.deb", "arm64"},
		{3, "/pool/shared.deb", "amd64"}, // same host/path, different scheme and blob
		{4, "/pool/shared.deb", "arm64"},
		{5, "/pool/old.deb", "riscv64"},
		{7, "/pool/candidate.deb", "mips"},
		// A pdiff-only group still contributes its architecture and source
		// snapshot, even though subtracting pdiff leaves zero ordinary rows.
		// Matching both path patterns must count this row only once.
		{8, "/dists/pdiff-only/main/source/Sources.diff/Packages.diff/patch.gz", "source"},
		{8, "/dists/pdiff-only/main/binary-ppc64el/Packages.diff/patch.gz", "ppc64el"},
	} {
		s := snapshots[p.snapshot-1]
		exec(`INSERT INTO package_hash(canonical_scheme, canonical_host, path,
declared_sha256, snapshot_id, architecture) VALUES (?, ?, ?, ?, ?, ?)`,
			s.scheme, s.host, p.path, hash(100), p.snapshot, p.arch)
	}
	// Keep the scheme/host predicates: a snapshot ID alone does not prove
	// that a package row belongs to the current suite's canonical origin.
	exec(`INSERT INTO package_hash(canonical_scheme, canonical_host, path,
declared_sha256, snapshot_id, architecture) VALUES ('http', 'mirror.example',
'/pool/wrong-scheme.deb', ?, 1, 'wrong-scheme')`, hash(100))
	exec(`INSERT INTO package_hash(canonical_scheme, canonical_host, path,
declared_sha256, snapshot_id, architecture) VALUES ('https', 'wrong.example',
'/pool/wrong-host.deb', ?, 1, 'wrong-host')`, hash(100))
	for _, m := range []struct {
		snapshot int
		path     string
	}{
		{1, "main/source/Sources.diff/Index"},
		{1, "main/binary-amd64/Packages.diff/Index"}, // count the snapshot once
		{2, "main/binary-amd64/packages.diff/Index"}, // case-sensitive classifier
		{3, "main/source/Sources.diff/Index"},
		{5, "main/binary-amd64/Packages.diff/Index"}, // old snapshot excluded
		{6, "main/binary-amd64/Packages.diff/Index"}, // no package_hash rows
	} {
		exec(`INSERT INTO snapshot_member(snapshot_id, path, blob_hash, declared_sha256)
VALUES (?, ?, ?, ?)`, m.snapshot, m.path, hash(100), hash(100))
	}
	for _, u := range []struct {
		scheme, host, path string
		blob               int
	}{
		{"https", "mirror.example", "/pool/shared.deb", 100},
		{"https", "mirror.example", "/pool/source.dsc", 100},
		{"http", "mirror.example", "/pool/shared.deb", 101},
		{"https", "other.example", "/pool/shared.deb", 100},
		{"https", "mirror.example", "/pool/old.deb", 101},
	} {
		exec(`INSERT INTO url_path(canonical_scheme, canonical_host, path,
blob_hash, upstream_url, is_metadata) VALUES (?, ?, ?, ?, ?, 0)`,
			u.scheme, u.host, u.path, hash(u.blob), u.scheme+"://"+u.host+u.path)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	coverage, err := c.GetRepoCoverage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantCoverage := RepoCoverage{
		ArchitecturesSeen:     []string{"amd64", "arm64", "ppc64el", "source"},
		SnapshotsWithSources:  3,
		SnapshotsWithPdiff:    3,
		PackageHashRowsBinary: 6,
		PackageHashRowsSource: 2,
		PackageHashRowsPdiff:  4,
		PackageHashRowsTotal:  13,
	}
	if !reflect.DeepEqual(coverage, wantCoverage) {
		t.Errorf("coverage = %+v, want %+v", coverage, wantCoverage)
	}
	summary, err := c.GetCacheSummaryByHostArch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantSummary := map[string]map[string]CacheSummaryEntry{
		"mirror.example": {
			"amd64":  {PackageHashCount: 4, BlobCount: 2, BlobBytes: 579},
			"arm64":  {PackageHashCount: 1},
			"source": {PackageHashCount: 3, BlobCount: 1, BlobBytes: 123},
		},
		"other.example": {
			"arm64":   {PackageHashCount: 1, BlobCount: 1, BlobBytes: 123},
			"ppc64el": {PackageHashCount: 1},
			"source":  {PackageHashCount: 1},
		},
	}
	if !reflect.DeepEqual(summary, wantSummary) {
		t.Errorf("summary = %+v, want %+v", summary, wantSummary)
	}
}
