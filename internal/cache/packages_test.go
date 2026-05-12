package cache

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// seedURLPath inserts a url_path row for the given (scheme, host, path)
// referencing the provided blob hash. Used by the ListCachedDebs /
// LookupCachedDebByName tests to set up multi-host scenarios.
func seedURLPath(t *testing.T, c *Cache, scheme, host, path, blobHash string) {
	t.Helper()
	u := URLPath{
		CanonicalScheme: scheme,
		CanonicalHost:   host,
		Path:            path,
		BlobHash:        &blobHash,
		UpstreamURL:     scheme + "://" + host + path,
	}
	if err := c.PutURLPath(context.Background(), u); err != nil {
		t.Fatalf("PutURLPath(%s %s %s): %v", scheme, host, path, err)
	}
}

func TestListCachedDebs_EmptyCacheReturnsNil(t *testing.T) {
	c := openCache(t)
	got, err := c.ListCachedDebs(context.Background(), "")
	if err != nil {
		t.Fatalf("ListCachedDebs: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty cache returned %d rows: %#v", len(got), got)
	}
}

func TestListCachedDebs_ReturnsOnlyDebs(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	hashDeb := seedBlob(t, c, "deb body")
	hashRel := seedBlob(t, c, "Release body")
	hashPkgs := seedBlob(t, c, "Packages.gz body")

	seedURLPath(t, c, "http", "archive.ubuntu.com",
		"/ubuntu/pool/main/n/nginx/nginx_1.18.0-1_amd64.deb", hashDeb)
	seedURLPath(t, c, "http", "archive.ubuntu.com",
		"/ubuntu/dists/noble/InRelease", hashRel)
	seedURLPath(t, c, "http", "archive.ubuntu.com",
		"/ubuntu/dists/noble/main/binary-amd64/Packages.gz", hashPkgs)

	got, err := c.ListCachedDebs(ctx, "")
	if err != nil {
		t.Fatalf("ListCachedDebs: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 row, got %d: %#v", len(got), got)
	}
	if got[0].Filename != "nginx_1.18.0-1_amd64.deb" {
		t.Errorf("Filename = %q, want nginx_1.18.0-1_amd64.deb", got[0].Filename)
	}
	if got[0].BlobHash != hashDeb {
		t.Errorf("BlobHash = %q, want %q", got[0].BlobHash, hashDeb)
	}
	if got[0].Size != int64(len("deb body")) {
		t.Errorf("Size = %d, want %d", got[0].Size, len("deb body"))
	}
	if got[0].Hosts == nil || got[0].Hosts[0] != "archive.ubuntu.com" {
		t.Errorf("Hosts = %v, want [archive.ubuntu.com]", got[0].Hosts)
	}
}

func TestListCachedDebs_SubstringFilter(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	hashA := seedBlob(t, c, "nginx body")
	hashB := seedBlob(t, c, "libnginx-mod body")
	hashC := seedBlob(t, c, "vim body")

	seedURLPath(t, c, "http", "archive.ubuntu.com", "/p/n/nginx_1.0_amd64.deb", hashA)
	seedURLPath(t, c, "http", "archive.ubuntu.com", "/p/l/libnginx-mod-foo_1.0_amd64.deb", hashB)
	seedURLPath(t, c, "http", "archive.ubuntu.com", "/p/v/vim_9.0_amd64.deb", hashC)

	got, err := c.ListCachedDebs(ctx, "nginx")
	if err != nil {
		t.Fatalf("ListCachedDebs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows for substring 'nginx', got %d: %#v", len(got), got)
	}
	names := []string{got[0].Filename, got[1].Filename}
	sort.Strings(names)
	want := []string{"libnginx-mod-foo_1.0_amd64.deb", "nginx_1.0_amd64.deb"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("filenames = %v, want %v", names, want)
	}
}

func TestListCachedDebs_DedupesSameBlobAcrossHosts(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	hash := seedBlob(t, c, "shared deb content")
	seedURLPath(t, c, "http", "archive.ubuntu.com", "/pool/foo_1.0_amd64.deb", hash)
	seedURLPath(t, c, "http", "mirror.example.org", "/pool/foo_1.0_amd64.deb", hash)
	seedURLPath(t, c, "https", "another.example.org", "/pool/foo_1.0_amd64.deb", hash)

	got, err := c.ListCachedDebs(ctx, "")
	if err != nil {
		t.Fatalf("ListCachedDebs: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 deduped row, got %d: %#v", len(got), got)
	}
	wantHosts := []string{"another.example.org", "archive.ubuntu.com", "mirror.example.org"}
	if !reflect.DeepEqual(got[0].Hosts, wantHosts) {
		t.Errorf("Hosts = %v, want %v", got[0].Hosts, wantHosts)
	}
}

func TestListCachedDebs_DifferentBlobsAreSeparateRows(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	hashA := seedBlob(t, c, "variant A bytes")
	hashB := seedBlob(t, c, "variant B bytes (longer for distinct size)")
	seedURLPath(t, c, "http", "archive.ubuntu.com", "/pool/foo_1.0_amd64.deb", hashA)
	seedURLPath(t, c, "http", "mirror.example.org", "/pool/foo_1.0_amd64.deb", hashB)

	got, err := c.ListCachedDebs(ctx, "")
	if err != nil {
		t.Fatalf("ListCachedDebs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows (one per distinct blob), got %d: %#v", len(got), got)
	}
	// Both must share filename but have distinct hashes.
	if got[0].Filename != "foo_1.0_amd64.deb" || got[1].Filename != "foo_1.0_amd64.deb" {
		t.Errorf("filenames = %q,%q; both want foo_1.0_amd64.deb", got[0].Filename, got[1].Filename)
	}
	if got[0].BlobHash == got[1].BlobHash {
		t.Errorf("expected distinct blob hashes, got %q twice", got[0].BlobHash)
	}
}

func TestListCachedDebs_ExcludesURLPathsWithoutBlob(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	// A url_path row whose blob_hash is NULL (the freshness probe / pre-fetch
	// case). Should NOT appear in the listing.
	u := URLPath{
		CanonicalScheme: "http",
		CanonicalHost:   "archive.ubuntu.com",
		Path:            "/pool/unfetched_1.0_amd64.deb",
		BlobHash:        nil,
		UpstreamURL:     "http://archive.ubuntu.com/pool/unfetched_1.0_amd64.deb",
	}
	if err := c.PutURLPath(ctx, u); err != nil {
		t.Fatalf("PutURLPath: %v", err)
	}

	got, err := c.ListCachedDebs(ctx, "")
	if err != nil {
		t.Fatalf("ListCachedDebs: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want 0 rows (no blob attached), got %d: %#v", len(got), got)
	}
}

func TestListCachedDebs_OrderedByFilenameThenHash(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	hashA := seedBlob(t, c, "a body")
	hashB := seedBlob(t, c, "b body")
	hashC := seedBlob(t, c, "c body")
	seedURLPath(t, c, "http", "h", "/pool/zlib_1.0_amd64.deb", hashB)
	seedURLPath(t, c, "http", "h", "/pool/curl_8.0_amd64.deb", hashC)
	seedURLPath(t, c, "http", "h", "/pool/abc_1.0_amd64.deb", hashA)

	got, err := c.ListCachedDebs(ctx, "")
	if err != nil {
		t.Fatalf("ListCachedDebs: %v", err)
	}
	want := []string{"abc_1.0_amd64.deb", "curl_8.0_amd64.deb", "zlib_1.0_amd64.deb"}
	for i, w := range want {
		if got[i].Filename != w {
			t.Errorf("row %d filename = %q, want %q", i, got[i].Filename, w)
		}
	}
}

func TestLookupCachedDebByName_NoMatchReturnsEmpty(t *testing.T) {
	c := openCache(t)
	hash := seedBlob(t, c, "x")
	seedURLPath(t, c, "http", "h", "/pool/foo_1.0_amd64.deb", hash)

	got, err := c.LookupCachedDebByName(context.Background(), "bar_1.0_amd64.deb")
	if err != nil {
		t.Fatalf("LookupCachedDebByName: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want 0 rows for unknown filename, got %d: %#v", len(got), got)
	}
}

func TestLookupCachedDebByName_ExactMatch(t *testing.T) {
	c := openCache(t)
	hashA := seedBlob(t, c, "a body")
	hashB := seedBlob(t, c, "b body")
	seedURLPath(t, c, "http", "h", "/pool/foo_1.0_amd64.deb", hashA)
	seedURLPath(t, c, "http", "h", "/pool/foobar_1.0_amd64.deb", hashB)

	got, err := c.LookupCachedDebByName(context.Background(), "foo_1.0_amd64.deb")
	if err != nil {
		t.Fatalf("LookupCachedDebByName: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 row for exact match, got %d: %#v", len(got), got)
	}
	if got[0].BlobHash != hashA {
		t.Errorf("BlobHash = %q, want %q", got[0].BlobHash, hashA)
	}
}

func TestLookupCachedDebByName_AmbiguousReturnsAll(t *testing.T) {
	c := openCache(t)
	hashA := seedBlob(t, c, "variant A")
	hashB := seedBlob(t, c, "variant B (different)")
	seedURLPath(t, c, "http", "archive.ubuntu.com", "/pool/foo_1.0_amd64.deb", hashA)
	seedURLPath(t, c, "http", "mirror.example.org", "/pool/foo_1.0_amd64.deb", hashB)

	got, err := c.LookupCachedDebByName(context.Background(), "foo_1.0_amd64.deb")
	if err != nil {
		t.Fatalf("LookupCachedDebByName: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows for ambiguous filename, got %d: %#v", len(got), got)
	}
	hashes := []string{got[0].BlobHash, got[1].BlobHash}
	sort.Strings(hashes)
	wantHashes := []string{hashA, hashB}
	sort.Strings(wantHashes)
	if !reflect.DeepEqual(hashes, wantHashes) {
		t.Errorf("blob hashes = %v, want %v", hashes, wantHashes)
	}
}

func TestCollapseCachedDebs_PredicateSkipsNonMatches(t *testing.T) {
	rows := []cachedDebRow{
		{Host: "h1", Path: "/pool/a.deb", BlobHash: strings.Repeat("a", 64), Size: 1, CreatedAt: 100},
		{Host: "h2", Path: "/pool/b.deb", BlobHash: strings.Repeat("b", 64), Size: 2, CreatedAt: 200},
	}
	got := collapseCachedDebs(rows, func(f string) bool { return f == "a.deb" })
	if len(got) != 1 || got[0].Filename != "a.deb" {
		t.Errorf("predicate did not filter as expected: got %#v", got)
	}
}
