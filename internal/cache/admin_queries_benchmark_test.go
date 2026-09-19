package cache

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
)

// BenchmarkAdminCacheSummaries models one current package catalog plus nine
// retained catalogs. Only a small fraction of the declared packages are cached.
// Seeding and schema setup are excluded from the measured work.
func BenchmarkAdminCacheSummaries(b *testing.B) {
	ctx := context.Background()
	c, err := Open(ctx, b.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = c.Close() })
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	exec := func(query string, args ...any) {
		b.Helper()
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			b.Fatal(err)
		}
	}
	for snapshot := 1; snapshot <= 10; snapshot++ {
		hash := fmt.Sprintf("%064x", snapshot)
		exec(`INSERT INTO blob(hash, size, created_at) VALUES (?, 1000, 1)`, hash)
		exec(`INSERT INTO suite_snapshot(snapshot_id, canonical_scheme, canonical_host,
suite_path, inrelease_hash, created_at, adopted_at)
VALUES (?, 'https', 'mirror.example', '/dists/stable', ?, 1, 1)`, snapshot, hash)
		exec(`WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<100000)
INSERT INTO package_hash(canonical_scheme, canonical_host, path, declared_sha256,
snapshot_id, package_name, architecture, version)
SELECT 'https', 'mirror.example', '/pool/pkg-' || i || '.deb', ?, ?,
'pkg-' || i, CASE WHEN i%10=0 THEN 'source' WHEN i%2=0 THEN 'arm64' ELSE 'amd64' END,
'1.0' FROM n`, hash, snapshot)
	}
	exec(`INSERT INTO suite_freshness(canonical_scheme, canonical_host, suite_path,
current_snapshot_id) VALUES ('https', 'mirror.example', '/dists/stable', 10)`)
	exec(`WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<1000)
INSERT INTO url_path(canonical_scheme, canonical_host, path, blob_hash, upstream_url, is_metadata)
SELECT 'https', 'mirror.example', '/pool/pkg-' || i || '.deb', ?,
'https://mirror.example/pool/pkg-' || i || '.deb', 0 FROM n`, fmt.Sprintf("%064x", 10))
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	b.Run("RepoCoverage", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			r, err := c.GetRepoCoverage(ctx)
			if err != nil {
				b.Fatal(err)
			}
			if r.PackageHashRowsTotal != 100000 {
				b.Fatalf("current rows = %d", r.PackageHashRowsTotal)
			}
		}
	})
	b.Run("CacheSummary", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			r, err := c.GetCacheSummaryByHostArch(ctx)
			if err != nil {
				b.Fatal(err)
			}
			if len(r["mirror.example"]) != 3 {
				b.Fatalf("architecture buckets = %v", r)
			}
		}
	})
}
