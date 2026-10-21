package cache

import (
	"context"
	"database/sql"
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
