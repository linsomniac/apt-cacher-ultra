package cache

import (
	"context"
	"fmt"
	"strings"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
)

// suiteRowNames are the member names ListSuiteMetadataRows probes by key:
// spec C2's anchors and root indexes — Packages and Sources, plain or with
// one of the six compression extensions (the names
// unadoptedFlatIndexRowsSQL probes too).
var suiteRowNames = func() []string {
	names := []string{"InRelease", "Release", "Release.gpg"}
	for _, stem := range []string{"Packages", "Sources"} {
		for _, ext := range []string{"", ".gz", ".xz", ".bz2", ".lzma", ".lz4", ".zst"} {
			names = append(names, stem+ext)
		}
	}
	return names
}()

// listSuiteMetadataRowsSQL is ListSuiteMetadataRows' query: url_path's
// key probed once per suiteRowNames name under the suite, and one key
// range over the suite's Contents-* names ("Contents-" up to "Contents.",
// the next byte). Two SELECTs, not one with an OR: SQLite plans that OR
// as a range over the whole host.
var listSuiteMetadataRowsSQL = `
SELECT canonical_scheme, canonical_host, path, blob_hash, upstream_url,
       is_metadata, last_requested_at, request_count, last_fetched_at,
       upstream_etag, upstream_lastmod
  FROM url_path
 WHERE canonical_scheme = ? AND canonical_host = ?
   AND path IN (` + strings.TrimSuffix(strings.Repeat("?, ", len(suiteRowNames)), ", ") + `)
UNION ALL
SELECT canonical_scheme, canonical_host, path, blob_hash, upstream_url,
       is_metadata, last_requested_at, request_count, last_fetched_at,
       upstream_etag, upstream_lastmod
  FROM url_path
 WHERE canonical_scheme = ? AND canonical_host = ? AND path >= ? AND path < ?
 ORDER BY path`

// ListSuiteMetadataRows returns key's suite's anchor, root index and
// Contents-* url_path rows, in path order: the rows of key's scheme and
// host named InRelease, Release, Release.gpg, Packages*, Sources* or
// Contents-* directly under the suite whose aptpath.SuitePath is the suite
// — never the "/." twin's, a nested or sibling suite's, its .debs, its
// by-hash names or pdiff Index (no caller reads them), or a file no
// grammar maps to it. A key no grammar produces
// (aptpath.IsCanonicalSuitePath) has none. Spec C4: Dispatch 3 reads a
// flat suite's client-created index rows from it, and the anchorless
// refresh revalidates the indexes and deletes the anchors upstream
// dropped.
//
// AIDEV-NOTE: design review M-5 (phase G). The read probes url_path's key
// by name (17 names) plus one Contents-* range, never a range over the
// suite's directory: a Gemfury-style repository keeps its .debs in the
// suite's own directory, so a directory range read every cached .deb of
// the repository, twice per check. TestListSuiteMetadataRows_ProbesURLPathByItsKey
// pins the plan; keep suiteRowNames in step with spec C2's flat member
// grammar (aptpath.flatMemberRE).
func (c *Cache) ListSuiteMetadataRows(ctx context.Context, key SuiteKey) ([]URLPath, error) {
	if !aptpath.IsCanonicalSuitePath(key.SuitePath) {
		return nil, nil
	}
	args := make([]any, 0, len(suiteRowNames)+6)
	args = append(args, key.CanonicalScheme, key.CanonicalHost)
	for _, n := range suiteRowNames {
		args = append(args, key.SuitePath+"/"+n)
	}
	args = append(args, key.CanonicalScheme, key.CanonicalHost, key.SuitePath+"/Contents-", key.SuitePath+"/Contents.")
	rows, err := c.db.QueryContext(ctx, listSuiteMetadataRowsSQL, args...)
	if err != nil {
		return nil, fmt.Errorf("ListSuiteMetadataRows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []URLPath
	for rows.Next() {
		var (
			u    URLPath
			isMD int64
		)
		if err := rows.Scan(&u.CanonicalScheme, &u.CanonicalHost, &u.Path, &u.BlobHash, &u.UpstreamURL,
			&isMD, &u.LastRequestedAt, &u.RequestCount, &u.LastFetchedAt, &u.UpstreamETag, &u.UpstreamLastMod); err != nil {
			return nil, fmt.Errorf("ListSuiteMetadataRows: scan: %w", err)
		}
		if aptpath.SuitePath(u.Path) != key.SuitePath {
			continue
		}
		u.IsMetadata = isMD != 0
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListSuiteMetadataRows: %w", err)
	}
	return out, nil
}
