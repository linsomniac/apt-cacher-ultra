package cache

// The legacy query is retained only as a benchmark baseline. Exact-result
// checks compare production coverage with the former GROUP BY kind query
// before timing either implementation.

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"testing"
)

func BenchmarkCoverageGrouping(b *testing.B) {
	for _, fixture := range []struct {
		name   string
		groups int
	}{
		{"ExistingFixture", 1},
		{"MultipleHostsAndSuites", 4},
	} {
		b.Run(fixture.name, func(b *testing.B) {
			ctx := context.Background()
			c := seedCoverageGroupingBenchmark(b, fixture.groups)
			want, err := coverageGroupByKindBaseline(ctx, c)
			if err != nil {
				b.Fatal(err)
			}
			got, err := c.GetRepoCoverage(ctx)
			if err != nil {
				b.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				b.Fatalf("coverage mismatch:\ngot  %+v\nwant %+v", got, want)
			}
			if got.PackageHashRowsTotal != 100000 {
				b.Fatalf("current row count = %d, want 100000", got.PackageHashRowsTotal)
			}
			if fixture.groups > 1 && (got.PackageHashRowsPdiff == 0 || got.SnapshotsWithPdiff != 5) {
				b.Fatalf("fixture missing pdiff coverage cases: %+v", got)
			}
			for _, query := range []struct {
				name string
				run  func(context.Context) (RepoCoverage, error)
			}{
				{"GroupByKind", func(ctx context.Context) (RepoCoverage, error) {
					return coverageGroupByKindBaseline(ctx, c)
				}},
				{"ConditionalSum", c.GetRepoCoverage},
			} {
				b.Run(query.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						if _, err := query.run(ctx); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}

func coverageGroupByKindBaseline(ctx context.Context, c *Cache) (RepoCoverage, error) {
	var coverage RepoCoverage
	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return coverage, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `
SELECT sf.current_snapshot_id, p.architecture,
  CASE
    WHEN p.path GLOB '*/Packages.diff/*' OR p.path GLOB '*/Sources.diff/*' THEN 'pdiff'
    WHEN p.architecture = 'source' THEN 'source'
    WHEN p.architecture != '' THEN 'binary'
    ELSE 'other'
  END AS kind,
  count(*) AS n
FROM suite_freshness sf
CROSS JOIN package_hash p
  ON sf.canonical_scheme = p.canonical_scheme
 AND sf.canonical_host   = p.canonical_host
 AND sf.current_snapshot_id = p.snapshot_id
GROUP BY sf.current_snapshot_id, p.architecture, kind`)
	if err != nil {
		return coverage, err
	}
	architectures := make(map[string]struct{})
	sourceSnapshots := make(map[int64]struct{})
	for rows.Next() {
		var snapshotID, n int64
		var arch, kind string
		if err := rows.Scan(&snapshotID, &arch, &kind, &n); err != nil {
			_ = rows.Close()
			return coverage, err
		}
		if arch != "" {
			architectures[arch] = struct{}{}
		}
		if arch == "source" {
			sourceSnapshots[snapshotID] = struct{}{}
		}
		switch kind {
		case "binary":
			coverage.PackageHashRowsBinary += n
		case "source":
			coverage.PackageHashRowsSource += n
		case "pdiff":
			coverage.PackageHashRowsPdiff += n
		}
		// The legacy behavior includes empty-architecture rows in the
		// total and recognizes their pdiff paths independently of arch.
		coverage.PackageHashRowsTotal += n
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return coverage, err
	}
	_ = rows.Close()
	for arch := range architectures {
		coverage.ArchitecturesSeen = append(coverage.ArchitecturesSeen, arch)
	}
	sort.Strings(coverage.ArchitecturesSeen)
	coverage.SnapshotsWithSources = int64(len(sourceSnapshots))
	// The member-only pdiff coverage query is unchanged.
	if err := tx.QueryRowContext(ctx, `
SELECT count(DISTINCT sf.current_snapshot_id)
FROM suite_freshness sf
CROSS JOIN snapshot_member m
  ON sf.current_snapshot_id = m.snapshot_id
WHERE m.path GLOB '*/Packages.diff/Index'
   OR m.path GLOB '*/Sources.diff/Index'`).Scan(&coverage.SnapshotsWithPdiff); err != nil {
		return coverage, err
	}
	return coverage, nil
}

// Keep both benchmark datasets at 100k current and 900k retained rows,
// including their cached-blob layout, for comparable measurements.
func seedCoverageGroupingBenchmark(b *testing.B, groups int) *Cache {
	b.Helper()
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
	if groups > 1 {
		// The same blobs are reachable via multiple paths, architectures,
		// suites, and hosts. Distinct blob counts are per host/architecture.
		exec(`WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i<49)
INSERT INTO blob(hash, size, created_at)
SELECT printf('%064x', 100000+i), 1000+3*i, 1 FROM n`)
	}
	for group := 0; group < groups; group++ {
		host, suite := "mirror.example", "/dists/stable"
		if groups > 1 {
			host = fmt.Sprintf("mirror-%d.example", group/2)
			suite = fmt.Sprintf("/dists/suite-%d", group%2)
		}
		for generation := 1; generation <= 10; generation++ {
			snapshot := group*10 + generation
			hash := fmt.Sprintf("%064x", snapshot)
			exec(`INSERT INTO blob(hash, size, created_at) VALUES (?, 1000, 1)`, hash)
			exec(`INSERT INTO suite_snapshot(snapshot_id, canonical_scheme, canonical_host,
suite_path, inrelease_hash, created_at, adopted_at)
VALUES (?, 'https', ?, ?, ?, 1, 1)`, snapshot, host, suite, hash)
			if groups == 1 {
				exec(`WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<100000)
INSERT INTO package_hash(canonical_scheme, canonical_host, path, declared_sha256,
snapshot_id, package_name, architecture, version)
SELECT 'https', 'mirror.example', '/pool/pkg-' || i || '.deb', ?, ?,
'pkg-' || i, CASE WHEN i%10=0 THEN 'source' WHEN i%2=0 THEN 'arm64' ELSE 'amd64' END,
'1.0' FROM n`, hash, snapshot)
			} else {
				exec(`WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<?)
INSERT INTO package_hash(canonical_scheme, canonical_host, path, declared_sha256,
snapshot_id, package_name, architecture, version)
SELECT 'https', ?, ? ||
CASE WHEN i%31=0 THEN '/Packages.diff/'
     WHEN i%37=0 THEN '/Sources.diff/'
     WHEN i%41=0 THEN '/packages.diff/'
     ELSE '/pool/' END || 'pkg-' || i || '.deb', ?, ?,
'pkg-' || i, CASE WHEN i%17=0 THEN '' WHEN i%10=0 THEN 'source'
                 WHEN i%2=0 THEN 'arm64' ELSE 'amd64' END, '1.0' FROM n`,
					100000/groups, host, suite, hash, snapshot)
			}
		}
		current := group*10 + 10
		exec(`INSERT INTO suite_freshness(canonical_scheme, canonical_host, suite_path,
current_snapshot_id) VALUES ('https', ?, ?, ?)`, host, suite, current)
		if groups == 1 {
			exec(`WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<1000)
INSERT INTO url_path(canonical_scheme, canonical_host, path, blob_hash, upstream_url, is_metadata)
SELECT 'https', 'mirror.example', '/pool/pkg-' || i || '.deb', ?,
'https://mirror.example/pool/pkg-' || i || '.deb', 0 FROM n`, fmt.Sprintf("%064x", current))
		} else {
			exec(`INSERT INTO url_path(canonical_scheme, canonical_host, path, blob_hash, upstream_url, is_metadata)
SELECT canonical_scheme, canonical_host, path,
printf('%064x', 100000+rowid%50), 'https://' || canonical_host || path, 0
FROM package_hash WHERE snapshot_id=? ORDER BY path LIMIT ?`, current, 1000/groups)
			exec(`INSERT INTO snapshot_member(snapshot_id, path, blob_hash, declared_sha256)
VALUES (?, 'main/Packages.diff/Index', ?, ?)`, current, fmt.Sprintf("%064x", current), fmt.Sprintf("%064x", current))
		}
	}
	if groups > 1 {
		// A current member-only snapshot contributes pdiff coverage but
		// must not create an empty host bucket in cache_summary.
		hash := fmt.Sprintf("%064x", 1)
		exec(`INSERT INTO suite_snapshot(snapshot_id, canonical_scheme, canonical_host,
suite_path, inrelease_hash, created_at, adopted_at)
VALUES (1000, 'https', 'member-only.example', '/dists/stable', ?, 1, 1)`, hash)
		exec(`INSERT INTO suite_freshness(canonical_scheme, canonical_host, suite_path,
current_snapshot_id) VALUES ('https', 'member-only.example', '/dists/stable', 1000)`)
		exec(`INSERT INTO snapshot_member(snapshot_id, path, blob_hash, declared_sha256)
VALUES (1000, 'main/Sources.diff/Index', ?, ?)`, hash, hash)
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	return c
}
