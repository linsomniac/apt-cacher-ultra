package cache

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
)

// Ruling R-G2 (plan design note 15): ListUnadoptedFlatIndexRows lists the
// cached root Packages* and Sources* rows of every unadopted flat suite —
// a /. twin's under the twin, a suite whose path holds "DISTS" (no dists
// segment: flat) too — and nothing else: no anchor, Contents-*, pdiff
// Index or nested suite's index, no adopted suite's, no dists/ suite's
// root Packages, no non-canonical row's (an empty segment: no flat
// suite).
func TestListUnadoptedFlatIndexRows_ListsTheRootIndexesOfUnadoptedFlatSuites(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	for _, k := range []SuiteKey{sk("h", "/flat"), sk("h", "/flat/."), sk("h", "/x/DISTS/y"), sk("h", "/adopted"), sk("h", "/r/dists/x"),
		sk("h", "/a//b")} {
		seedRow(t, c, k, 1)
	}
	adoptWith(t, c, sk("h", "/adopted"), map[string]string{"InRelease": "adopted anchor"})
	blob := seedBlob(t, c, "an index")
	fetched := int64(1_700_000_000)
	for _, p := range []string{"/flat/Packages", "/flat/Packages.gz", "/flat/Sources.zst", "/flat/InRelease",
		"/flat/Contents-amd64.gz", "/flat/Packages.diff/Index", "/flat/amd64/Packages", "/flat/Translation-en",
		"/flat/./Packages.xz", "/x/DISTS/y/Packages", "/adopted/Packages", "/r/dists/x/Packages", "/a//b/Packages"} {
		if err := c.PutURLPath(ctx, URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: p, BlobHash: &blob,
			UpstreamURL: "http://h" + p, IsMetadata: true, RequestCount: 1, LastFetchedAt: &fetched}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := c.ListUnadoptedFlatIndexRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, a := range got {
		if a.SuitePath+a.Path[strings.LastIndexByte(a.Path, '/'):] != a.Path || a.BlobHash == nil || *a.BlobHash != blob ||
			a.LastFetchedAt == nil || *a.LastFetchedAt != fetched {
			t.Errorf("row %+v: want a root index of its suite with blob %s fetched at %d", a, blob, fetched)
		}
		paths = append(paths, a.Path)
	}
	want := []string{"/flat/Packages", "/flat/Packages.gz", "/flat/Sources.zst", "/flat/./Packages.xz", "/x/DISTS/y/Packages"}
	if !slices.Equal(paths, want) {
		t.Fatalf("paths = %q, want %q", paths, want)
	}
}

// Design review M-14's plan rule, for R-G2's read: it drives from
// suite_freshness and probes url_path by its unique key, never a scan.
func TestListUnadoptedFlatIndexRows_ProbesURLPathByItsKey(t *testing.T) {
	c := openCache(t)
	rows, err := c.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+unadoptedFlatIndexRowsSQL)
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

// Spec §9 G row and the F carry-forward ("reaping /repo leaves /repo/.
// intact"), on a real flat suite and its ./ twin: the reap of /repo
// deletes its metadata rows — anchors, indexes, by-hash — its row and its
// snapshot's currency; the twin keeps its row, current snapshot, rows and
// the marker of the base they share.
func TestReap_AFlatSuiteLeavesItsTwin(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, evictNow)()
	repo, twin := sk("h", "/repo"), sk("h", "/repo/.")
	ids := map[SuiteKey]int64{}
	hex := strings.Repeat("cd", 32)
	for _, k := range []SuiteKey{repo, twin} {
		seedRow(t, c, k, evictNow-40*evictDay)
		ids[k], _ = adoptWith(t, c, k, map[string]string{"InRelease": "anchor of " + k.SuitePath})
		for _, name := range []string{"Packages.gz", "by-hash/SHA256/" + hex} { // the adoption cached InRelease
			putURLPathRow(t, c, "http", "h", k.SuitePath+"/"+name, seedBlob(t, c, name+" of "+k.SuitePath), sql.NullInt64{Int64: evictNow, Valid: true}, true)
		}
	}
	if err := c.MarkSignedSeenBatch(ctx, []SignedSeenMark{{CanonicalScheme: "http", CanonicalHost: "h", Base: "/repo", SeenAt: 1}}); err != nil {
		t.Fatal(err)
	}
	if n, err := c.DeleteSuiteMetadata(ctx, repo); err != nil || n != 3 {
		t.Fatalf("DeleteSuiteMetadata(/repo) = %d, %v; want its 3 rows", n, err)
	}
	cur := ids[repo]
	if del, _, err := c.ReapIdleSuite(ctx, repo, &cur, evictNow-30*evictDay); err != nil || !del {
		t.Fatalf("reap /repo = %v, %v", del, err)
	}
	if ok, err := c.HasSuiteRow(ctx, repo); err != nil || ok {
		t.Fatalf("/repo's row: %v, %v; want it gone", ok, err)
	}
	sf, err := c.GetSuiteFreshness(ctx, "http", "h", "/repo/.")
	if err != nil || sf.CurrentSnapshotID == nil || *sf.CurrentSnapshotID != ids[twin] {
		t.Fatalf("/repo/.: row %+v, %v; want it and its current snapshot kept", sf, err)
	}
	for _, name := range []string{"InRelease", "Packages.gz", "by-hash/SHA256/" + hex} {
		if urlRowExists(t, c, "http", "h", "/repo/"+name) {
			t.Errorf("/repo/%s was not deleted", name)
		}
		if !urlRowExists(t, c, "http", "h", "/repo/./"+name) {
			t.Errorf("/repo/./%s was deleted", name)
		}
	}
	if _, ok, err := c.SignedSeen(ctx, "http", "h", "/repo"); err != nil || !ok {
		t.Fatalf("the base's marker = %v, %v; want kept", ok, err)
	}
}
