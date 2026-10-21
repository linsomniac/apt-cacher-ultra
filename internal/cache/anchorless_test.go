package cache

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
)

// listedPaths returns the paths of key's suite metadata rows.
func listedPaths(t *testing.T, c *Cache, key SuiteKey) []string {
	t.Helper()
	rows, err := c.ListSuiteMetadataRows(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		out = append(out, r.Path)
	}
	return out
}

// Spec C4 (Dispatch 3, the anchorless refresh): ListSuiteMetadataRows
// lists the suite's own anchors, root indexes and Contents-* rows, in path
// order, with their columns — and nothing of its "./" twin, a nested or
// sibling suite, its .debs (a Gemfury-style repository keeps them in the
// suite's own directory), its by-hash names or pdiff Index (no caller
// reads them; design review M-5), or a file no grammar maps to it. A key
// no grammar produces lists nothing.
func TestListSuiteMetadataRows_TheSuitesRowsOnly(t *testing.T) {
	c := openCache(t)
	blob := seedBlob(t, c, "flat metadata")
	hex := strings.Repeat("ab", 32)
	mine := []string{"/flat/Contents-amd64.gz", "/flat/Contents-udeb-arm64", "/flat/InRelease", "/flat/Packages.gz",
		"/flat/Release.gpg", "/flat/Sources.xz"}
	others := []string{"/flat/./Packages.gz", "/flat/amd64/x_1_amd64.deb", "/flat/x_1_amd64.deb", "/flat/sub/Packages",
		"/flat2/Packages", "/flat/Translation-en", "/flat/Packages.diff/Index", "/flat/by-hash/SHA256/" + hex,
		"/flat/Contents-amd64.diff/Index", "/InRelease"}
	for _, p := range append(append([]string{}, mine...), others...) {
		putURLPathRow(t, c, "http", "h", p, blob, sql.NullInt64{Int64: 1_700_000_000, Valid: true}, true)
	}
	putURLPathRow(t, c, "https", "h", "/flat/Packages", blob, sql.NullInt64{}, true) // the other scheme's
	if got := listedPaths(t, c, sk("h", "/flat")); !slices.Equal(got, mine) {
		t.Fatalf("ListSuiteMetadataRows(/flat) = %v, want %v", got, mine)
	}
	rows, err := c.ListSuiteMetadataRows(context.Background(), sk("h", "/flat"))
	if err != nil {
		t.Fatal(err)
	}
	if r := rows[3]; r.Path != "/flat/Packages.gz" || r.BlobHash == nil || *r.BlobHash != blob || r.RequestCount != 1 ||
		r.LastRequestedAt == nil || *r.LastRequestedAt != 1_700_000_000 || !r.IsMetadata || r.UpstreamURL != "http://h/flat/Packages.gz" {
		t.Fatalf("row = %+v, want its columns", r)
	}
	if got, want := listedPaths(t, c, sk("h", "/flat/.")), []string{"/flat/./Packages.gz"}; !slices.Equal(got, want) {
		t.Fatalf("ListSuiteMetadataRows(/flat/.) = %v, want %v", got, want)
	}
	for _, key := range []string{"/flat/./.", ""} { // the empty key would probe the host root's names
		if got := listedPaths(t, c, sk("h", key)); got != nil {
			t.Fatalf("ListSuiteMetadataRows(%q), a key no grammar produces, = %v, want nothing", key, got)
		}
	}
}

// Design review M-5: ListSuiteMetadataRows probes url_path's key — by
// name, and one Contents-* range — and never scans the table or ranges
// over the suite's directory, whose .debs a Gemfury-style repository
// keeps beside its indexes.
func TestListSuiteMetadataRows_ProbesURLPathByItsKey(t *testing.T) {
	c := openCache(t)
	args := []any{"http", "h"}
	for _, n := range suiteRowNames {
		args = append(args, "/flat/"+n)
	}
	args = append(args, "http", "h", "/flat/Contents-", "/flat/Contents.")
	plan := queryPlan(t, c, listSuiteMetadataRowsSQL, args...)
	for _, want := range []string{
		"USING INDEX sqlite_autoindex_url_path_1 (canonical_scheme=? AND canonical_host=? AND path=?)",
		"USING INDEX sqlite_autoindex_url_path_1 (canonical_scheme=? AND canonical_host=? AND path>? AND path<?)",
	} {
		if !strings.Contains(plan, want) {
			t.Fatalf("query plan lacks %q:\n%s", want, plan)
		}
	}
	if strings.Contains(plan, "SCAN") {
		t.Fatalf("query plan scans url_path:\n%s", plan)
	}
}

// putFetchedRow stores the url_path row of host h at path with blob, as a
// client's miss stored it: request_count 3, last_requested_at
// 1_700_000_000, last_fetched_at fetched, upstream_etag "e1".
func putFetchedRow(t *testing.T, c *Cache, path, blob string, fetched int64) {
	t.Helper()
	requested, etag := int64(1_700_000_000), "e1"
	if err := c.PutURLPath(context.Background(), URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: path,
		BlobHash: &blob, UpstreamURL: "http://h" + path, IsMetadata: true, LastRequestedAt: &requested,
		RequestCount: 3, LastFetchedAt: &fetched, UpstreamETag: &etag}); err != nil {
		t.Fatal(err)
	}
}

func lookupRow(t *testing.T, c *Cache, path string) *URLPath {
	t.Helper()
	u, err := c.LookupURL(context.Background(), "http", "h", path)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func strOf(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// Spec C4 Dispatch 4's dedicated cache method: RefreshURLPathBlob points
// a row at its new blob with the response's validators, keeps
// last_requested_at and request_count, and moves last_fetched_at to
// MAX(now, previous + 1) — only when the blob changes (plan design note
// 13). A row whose blob is no longer the one the caller read is left
// alone, and a malformed hash is refused.
func TestRefreshURLPathBlob_ReplacesTheBlobAndKeepsTheInterest(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	old, newer := seedBlob(t, c, "Package: a\n"), seedBlob(t, c, "Package: b\n")
	defer stubNow(t, 1_800_000_000)()
	etag, lastmod := "e2", "Tue, 15 Nov 1994 08:12:31 GMT"

	putFetchedRow(t, c, "/flat/Packages", old, 1_799_999_000)
	if ok, err := c.RefreshURLPathBlob(ctx, "http", "h", "/flat/Packages", old, newer, &etag, &lastmod); err != nil || !ok {
		t.Fatalf("RefreshURLPathBlob = %v, %v; want true", ok, err)
	}
	u := lookupRow(t, c, "/flat/Packages")
	if *u.BlobHash != newer || strOf(u.UpstreamETag) != etag || strOf(u.UpstreamLastMod) != lastmod ||
		u.RequestCount != 3 || *u.LastRequestedAt != 1_700_000_000 || *u.LastFetchedAt != 1_800_000_000 {
		t.Fatalf("row = %+v; want the new blob and validators, the interest kept, last_fetched_at now", u)
	}

	// Within the second of the previous fetch: previous + 1.
	putFetchedRow(t, c, "/flat/Sources", old, 1_800_000_000)
	if ok, err := c.RefreshURLPathBlob(ctx, "http", "h", "/flat/Sources", old, newer, nil, nil); err != nil || !ok {
		t.Fatalf("RefreshURLPathBlob = %v, %v; want true", ok, err)
	}
	if u := lookupRow(t, c, "/flat/Sources"); *u.LastFetchedAt != 1_800_000_001 || u.UpstreamETag != nil || u.UpstreamLastMod != nil {
		t.Fatalf("row = %+v; want last_fetched_at 1800000001 and no validators", u)
	}

	// The same blob: the validators only.
	putFetchedRow(t, c, "/flat/Packages.gz", old, 1_799_999_000)
	if ok, err := c.RefreshURLPathBlob(ctx, "http", "h", "/flat/Packages.gz", old, old, &etag, nil); err != nil || !ok {
		t.Fatalf("RefreshURLPathBlob = %v, %v; want true", ok, err)
	}
	if u := lookupRow(t, c, "/flat/Packages.gz"); *u.LastFetchedAt != 1_799_999_000 || strOf(u.UpstreamETag) != etag {
		t.Fatalf("row = %+v; want last_fetched_at kept and the new ETag", u)
	}

	// A row that changed since it was read is left alone.
	putFetchedRow(t, c, "/flat/Packages.xz", newer, 1_799_999_000)
	if ok, err := c.RefreshURLPathBlob(ctx, "http", "h", "/flat/Packages.xz", old, newer, &etag, nil); err != nil || ok {
		t.Fatalf("RefreshURLPathBlob of a changed row = %v, %v; want false", ok, err)
	}
	if u := lookupRow(t, c, "/flat/Packages.xz"); strOf(u.UpstreamETag) != "e1" || *u.LastFetchedAt != 1_799_999_000 {
		t.Fatalf("a changed row was written: %+v", u)
	}
	if ok, err := c.RefreshURLPathBlob(ctx, "http", "h", "/flat/Missing", old, newer, nil, nil); err != nil || ok {
		t.Fatalf("RefreshURLPathBlob of a missing row = %v, %v; want false", ok, err)
	}
	if _, err := c.RefreshURLPathBlob(ctx, "http", "h", "/flat/Packages", newer, "../../etc/passwd", nil, nil); !errors.Is(err, ErrInvalidHash) {
		t.Fatalf("a malformed hash: err %v, want ErrInvalidHash", err)
	}
}

// DeleteURLPathRows deletes the rows given, in one transaction, and counts
// them — each only while it still holds the blob it was read with (design
// review M-4): a row a client's miss stored anew since the read is kept,
// and so is one read without a blob that has one; a row gone counts
// nothing, and other rows stay.
func TestDeleteURLPathRows_DeletesTheRowsNamed(t *testing.T) {
	c := openCache(t)
	blob, fresh := seedBlob(t, c, "Package: a\n"), seedBlob(t, c, "Package: b\n")
	for _, p := range []string{"/flat/InRelease", "/flat/Packages", "/flat/Release", "/flat/Sources"} {
		putFetchedRow(t, c, p, blob, 1_799_999_000)
	}
	read := func(p string) URLPath { return *lookupRow(t, c, p) }
	gone, blobless := read("/flat/Packages"), read("/flat/Sources")
	gone.Path, blobless.BlobHash = "/flat/Gone", nil
	rows := []URLPath{read("/flat/InRelease"), read("/flat/Packages"), read("/flat/Release"), gone, blobless}
	putFetchedRow(t, c, "/flat/InRelease", fresh, 1_800_000_000) // a miss stored it anew after the read
	n, err := c.DeleteURLPathRows(context.Background(), rows)
	if err != nil || n != 2 {
		t.Fatalf("DeleteURLPathRows = %d, %v; want 2", n, err)
	}
	if got := listedPaths(t, c, sk("h", "/flat")); !slices.Equal(got, []string{"/flat/InRelease", "/flat/Sources"}) {
		t.Fatalf("rows left = %v, want [/flat/InRelease /flat/Sources]: the row stored anew and the one read blobless kept", got)
	}
}

// Ruling PG-4 on plan design note 10: RefreshURLPathBlob changes no blob
// refcount — blob GC finds a url_path row's blob through its NOT EXISTS
// clause — so the blob a refresh replaced is reaped by the next GC batch
// past the grace while the live one is kept, and the live one is reaped
// once its row is deleted.
func TestRefreshURLPathBlob_GCFreesTheReplacedBlobAndKeepsTheLiveOne(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	restore := stubNow(t, 1_800_000_000)
	old, newer := seedBlob(t, c, "Package: a\n"), seedBlob(t, c, "Package: b\n")
	putFetchedRow(t, c, "/flat/Packages", old, 1_799_999_000)
	if ok, err := c.RefreshURLPathBlob(ctx, "http", "h", "/flat/Packages", old, newer, nil, nil); err != nil || !ok {
		t.Fatalf("RefreshURLPathBlob = %v, %v; want true", ok, err)
	}
	restore()
	defer stubNow(t, 1_800_000_100)()
	gc := func() []string {
		t.Helper()
		reaped, err := c.RunBlobGCBatch(ctx, 100, 0)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, b := range reaped {
			out = append(out, b.Hash)
		}
		return out
	}
	if got := gc(); !slices.Equal(got, []string{old}) {
		t.Fatalf("GC after the refresh reaped %v, want only the replaced blob %s", got, old)
	}
	if n, err := c.DeleteURLPathRows(ctx, []URLPath{*lookupRow(t, c, "/flat/Packages")}); err != nil || n != 1 {
		t.Fatalf("DeleteURLPathRows = %d, %v; want 1", n, err)
	}
	if got := gc(); !slices.Equal(got, []string{newer}) {
		t.Fatalf("GC after the row's delete reaped %v, want the refreshed blob %s", got, newer)
	}
}
