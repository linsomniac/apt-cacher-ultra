package cache

import (
	"context"
	"fmt"
	"strings"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
)

// unadoptedFlatIndexRowsSQL is ListUnadoptedFlatIndexRows' query: the
// shape of unadoptedSuiteAnchorsSQL (design review M-14), probing
// url_path's key once per root index name of spec C2's flat member
// grammar (aptpath.FlatIndexNames, the list ListSuiteMetadataRows probes
// too) for every unadopted suite whose path has no "/dists/" (instr, not
// LIKE: LIKE ignores ASCII case).
//
// AIDEV-NOTE: keep the join's url_path terms bare columns, as in
// unadoptedSuiteAnchorsSQL: an expression on them turns each probe into a
// scan of the host's rows (TestListUnadoptedFlatIndexRows_ProbesURLPathByItsKey
// pins the plan).
var unadoptedFlatIndexRowsSQL = func() string {
	var names []string
	for _, n := range aptpath.FlatIndexNames() {
		names = append(names, "sf.suite_path || '/"+n+"'")
	}
	return `
SELECT sf.canonical_scheme, sf.canonical_host, sf.suite_path, u.path, u.blob_hash, u.last_fetched_at
  FROM suite_freshness sf
  JOIN url_path u
    ON u.canonical_scheme = sf.canonical_scheme
   AND u.canonical_host   = sf.canonical_host
   AND u.path IN (` + strings.Join(names, ", ") + `)
 WHERE sf.current_snapshot_id IS NULL
   AND instr(sf.suite_path, '/dists/') = 0
 ORDER BY sf.canonical_scheme, sf.canonical_host, sf.suite_path, u.path`
}()

// ListUnadoptedFlatIndexRows returns the cached root Packages* and
// Sources* rows of every flat suite (aptpath.IsFlatSuitePath) that has no
// current snapshot, in suite and path order: ruling R-G2's junk evidence
// for an anchorless flat suite. One read on the pool, in a reaper run that
// holds an unadopted flat suite (the reaper's hasUnadoptedFlatSuite gate).
func (c *Cache) ListUnadoptedFlatIndexRows(ctx context.Context) ([]SuiteAnchorRow, error) {
	rows, err := c.db.QueryContext(ctx, unadoptedFlatIndexRowsSQL)
	if err != nil {
		return nil, fmt.Errorf("ListUnadoptedFlatIndexRows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SuiteAnchorRow
	for rows.Next() {
		var a SuiteAnchorRow
		if err := rows.Scan(&a.CanonicalScheme, &a.CanonicalHost, &a.SuitePath, &a.Path, &a.BlobHash, &a.LastFetchedAt); err != nil {
			return nil, fmt.Errorf("ListUnadoptedFlatIndexRows: scan: %w", err)
		}
		if aptpath.IsFlatSuitePath(a.SuitePath) {
			out = append(out, a)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListUnadoptedFlatIndexRows: %w", err)
	}
	return out, nil
}
