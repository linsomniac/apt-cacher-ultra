package cache

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// Ruling R-F3's evidence: ListUnadoptedSuiteAnchors lists the cached
// InRelease, Release and Release.gpg rows of the suites without a current
// snapshot, with their blob and fetch stamp — not an adopted suite's, not
// another file of the suite, not a nested suite's or another host's.
func TestListUnadoptedSuiteAnchors_ListsOnlyUnadoptedSuitesAnchors(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	unadopted, adopted := sk("h", "/r/dists/x"), sk("h", "/r/dists/y")
	seedRow(t, c, unadopted, 1)
	seedRow(t, c, adopted, 1)
	adoptWith(t, c, adopted, map[string]string{"InRelease": "y anchor"})
	blob := seedBlob(t, c, "anchor")
	fetched := int64(1_700_000_000)
	put := func(host, path string) {
		t.Helper()
		if err := c.PutURLPath(ctx, URLPath{CanonicalScheme: "http", CanonicalHost: host, Path: path, BlobHash: &blob,
			UpstreamURL: "http://" + host + path, IsMetadata: true, RequestCount: 1, LastFetchedAt: &fetched}); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"/r/dists/x/InRelease", "/r/dists/x/Release", "/r/dists/x/Release.gpg",
		"/r/dists/x/main/binary-amd64/Packages", "/r/dists/x/dists/z/InRelease", "/r/dists/y/Release"} {
		put("h", p)
	}
	put("other", "/r/dists/x/InRelease")
	got, err := c.ListUnadoptedSuiteAnchors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, a := range got {
		if a.SuiteKey != unadopted || a.BlobHash == nil || *a.BlobHash != blob || a.LastFetchedAt == nil || *a.LastFetchedAt != fetched {
			t.Errorf("row %+v, want %v's with blob %s fetched at %d", a, unadopted, blob, fetched)
		}
		paths = append(paths, a.Path)
	}
	want := []string{"/r/dists/x/InRelease", "/r/dists/x/Release", "/r/dists/x/Release.gpg"}
	if !slices.Equal(paths, want) {
		t.Errorf("paths = %q, want %q", paths, want)
	}
}

// Design review M-14: the junk scan's query drives from suite_freshness
// and probes url_path by its unique key — (canonical_scheme,
// canonical_host, path) for each anchor path — never a scan of url_path,
// which would make the hourly read grow with the cache rather than with
// the suites.
func TestListUnadoptedSuiteAnchors_ProbesURLPathByItsKey(t *testing.T) {
	c := openCache(t)
	rows, err := c.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+unadoptedSuiteAnchorsSQL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	probed := false
	for _, d := range plan {
		if strings.HasPrefix(d, "SCAN u") {
			t.Fatalf("plan %q scans url_path", plan)
		}
		if strings.HasPrefix(d, "SEARCH u USING") && strings.Contains(d, "(canonical_scheme=? AND canonical_host=? AND path=?)") {
			probed = true
		}
	}
	if !probed {
		t.Fatalf("plan %q: want url_path probed by (canonical_scheme, canonical_host, path)", plan)
	}
}
