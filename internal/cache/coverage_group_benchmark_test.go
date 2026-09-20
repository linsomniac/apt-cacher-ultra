package cache

// The legacy query is retained only as a benchmark baseline. Exact-result
// checks compare production coverage with the former GROUP BY kind query
// before timing either implementation.

import (
	"context"
	"database/sql"
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
			c := seedCombinedAdminExperiment(b, fixture.groups)
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
