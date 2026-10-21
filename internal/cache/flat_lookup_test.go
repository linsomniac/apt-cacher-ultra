package cache

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// hashOf is a 64-hex declared hash made of one repeated digit.
func hashOf(d byte) string { return strings.Repeat(string(d), 64) }

// declared runs DeclaredHashesForPath on host h and returns the rows'
// "hash@suite" pairs, sorted.
func declared(t *testing.T, c *Cache, path string) []string {
	t.Helper()
	rows, err := c.DeclaredHashesForPath(context.Background(), "http", "h", path)
	if err != nil {
		t.Fatalf("DeclaredHashesForPath(%s): %v", path, err)
	}
	var out []string
	for _, r := range rows {
		out = append(out, r.DeclaredSHA256[:1]+"@"+r.SuitePath)
	}
	slices.Sort(out)
	return out
}

// Spec C6's lookup over every real Filename shape (spec §3's table): the
// exact row (k8s, CUDA's "./", a `deb <uri> ./` twin's row at its
// FlatBase), and, only when no exact row exists, the suffix fallback —
// CUDA's dot-cleaned form, Jenkins' binary/ and direct/ (the parent base),
// Sublime's files/ (the host-root base), a twin's "./" path after
// DotClean. The fallback stops at "..", an empty segment and more than 32
// segments, and never crosses to a sibling directory or an unrelated base
// unless a Filename names it: suite /a/flat2's "flat/x_1_amd64.deb"
// vouches /a/flat/x_1_amd64.deb (B = /a, an ancestor of both — Jenkins'
// direct/ rule; ruling PG-13, residual 1: a row is a constraint across
// tenants, never a grant).
func TestDeclaredHashesForPath_FlatShapes(t *testing.T) {
	c := openCache(t)
	adopt := func(suite string, rows ...PackageHash) {
		seedRow(t, c, sk("h", suite), 1)
		adoptPackages(t, c, sk("h", suite), rows, true)
	}
	adopt("/core:/stable:/v1.34/deb", pkgRow("/core:/stable:/v1.34/deb/amd64/kubectl_1.34_amd64.deb", hashOf('1'), "amd64/kubectl_1.34_amd64.deb"))
	adopt("/cuda/x86_64", pkgRow("/cuda/x86_64/./cuda_12.6_amd64.deb", hashOf('2'), "cuda_12.6_amd64.deb"))
	adopt("/debian-stable/binary",
		pkgRow("/debian-stable/binary/binary/jenkins_2.4_all.deb", hashOf('3'), "binary/jenkins_2.4_all.deb"),
		pkgRow("/debian-stable/binary/direct/jenkins_2.5_all.deb", hashOf('4'), "direct/jenkins_2.5_all.deb"))
	adopt("/apt/stable", pkgRow("/apt/stable/files/sublime_4180_amd64.deb", hashOf('5'), "files/sublime_4180_amd64.deb"))
	adopt("/flat/.", pkgRow("/flat/amd64/x_1_amd64.deb", hashOf('6'), "amd64/x_1_amd64.deb"))
	deep := "/" + strings.TrimSuffix(strings.Repeat("d/", 30), "/") // a 30-segment flat suite
	adopt(deep, pkgRow(deep+"/x/y_1_amd64.deb", hashOf('7'), "x/y_1_amd64.deb"))
	deeper := deep + "/e" // 31 segments
	adopt(deeper, pkgRow(deeper+"/x/z_1_amd64.deb", hashOf('8'), "x/z_1_amd64.deb"))
	adopt("/a/flat2", pkgRow("/a/flat2/flat/x_1_amd64.deb", hashOf('9'), "flat/x_1_amd64.deb"))
	for _, tc := range []struct {
		name, path string
		want       []string
	}{
		{"k8s, exact", "/core:/stable:/v1.34/deb/amd64/kubectl_1.34_amd64.deb", []string{"1@/core:/stable:/v1.34/deb"}},
		{"CUDA ./, exact", "/cuda/x86_64/./cuda_12.6_amd64.deb", []string{"2@/cuda/x86_64"}},
		{"CUDA dot-cleaned, fallback", "/cuda/x86_64/cuda_12.6_amd64.deb", []string{"2@/cuda/x86_64"}},
		{"Jenkins binary/, parent base", "/debian-stable/binary/jenkins_2.4_all.deb", []string{"3@/debian-stable/binary"}},
		{"Jenkins direct/, parent base", "/debian-stable/direct/jenkins_2.5_all.deb", []string{"4@/debian-stable/binary"}},
		{"Sublime files/, host-root base", "/files/sublime_4180_amd64.deb", []string{"5@/apt/stable"}},
		{"twin, exact row at FlatBase", "/flat/amd64/x_1_amd64.deb", []string{"6@/flat/."}},
		{"twin, ./ after DotClean", "/flat/./amd64/x_1_amd64.deb", []string{"6@/flat/."}},
		{"32 segments after DotClean", deep + "/./x/y_1_amd64.deb", []string{"7@" + deep}},
		{"33 segments after DotClean", deeper + "/./x/z_1_amd64.deb", nil},
		{"the fallback stops at ..", "/flat/../flat/amd64/x_1_amd64.deb", nil},
		{"the fallback stops at an empty segment", "/flat//amd64/x_1_amd64.deb", nil},
		{"a sibling directory", "/flat2/amd64/x_1_amd64.deb", nil},
		{"a sibling a Filename names", "/a/flat/x_1_amd64.deb", []string{"9@/a/flat2"}},
		{"a prefix of the base's last segment", "/fla/amd64/x_1_amd64.deb", nil},
		{"an unrelated base", "/other/files/sublime_4180_amd64.deb", nil},
		{"a dists suite's pool", "/ubuntu/pool/main/f/foo_1_amd64.deb", nil},
	} {
		if got := declared(t, c, tc.path); !slices.Equal(got, tc.want) {
			t.Errorf("%s: DeclaredHashesForPath(%s) = %v, want %v", tc.name, tc.path, got, tc.want)
		}
	}
}

// Spec C6 step 2's tiebreak: among the rows the fallback keeps, those with
// the longest B — the most specific base — win. "/a/./b/c/x.deb" is
// <uri>/<Filename> both for suite /a (Filename b/c/x.deb, B /a) and for
// suite /a/b/c/sub (a `sub/` source of <uri> /a/b/c: Filename x.deb, B
// /a/b/c); the nested base wins, so the two declarations never conflict.
// The exact row still wins over any fallback.
func TestDeclaredHashesForPath_TheMostSpecificBaseWins(t *testing.T) {
	c := openCache(t)
	for suite, row := range map[string]PackageHash{
		"/a":         pkgRow("/a/b/c/x_1_amd64.deb", hashOf('a'), "b/c/x_1_amd64.deb"),
		"/a/b/c/sub": pkgRow("/a/b/c/sub/x_1_amd64.deb", hashOf('b'), "x_1_amd64.deb"),
	} {
		seedRow(t, c, sk("h", suite), 1)
		adoptPackages(t, c, sk("h", suite), []PackageHash{row}, true)
	}
	if got, want := declared(t, c, "/a/./b/c/x_1_amd64.deb"), []string{"b@/a/b/c/sub"}; !slices.Equal(got, want) {
		t.Fatalf("fallback = %v, want the nested base's row %v", got, want)
	}
	if got, want := declared(t, c, "/a/b/c/x_1_amd64.deb"), []string{"a@/a"}; !slices.Equal(got, want) {
		t.Fatalf("exact = %v, want /a's exact row %v (no fallback)", got, want)
	}
}

// Spec C6 "current signed non-retired snapshots only": the fallback sees
// neither an unsigned snapshot's rows (spec C5 safeguard 5) nor a
// displaced snapshot's.
func TestDeclaredHashesForPath_TheFallbackReadsCurrentSignedSnapshotsOnly(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	unsigned, displaced := sk("h", "/unsigned"), sk("h", "/displaced")
	seedRow(t, c, unsigned, 1)
	seedRow(t, c, displaced, 1)
	rel := seedBlob(t, c, "an unsigned Release")
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h",
		SuitePath: unsigned.SuitePath, InReleaseHash: &rel, Form: FormUnsigned})
	if err != nil {
		t.Fatal(err)
	}
	row := pkgRow("/unsigned/./u_1_amd64.deb", hashOf('c'), "u_1_amd64.deb")
	row.SnapshotID = id
	if err := c.CommitAdoption(ctx, id, []SnapshotMember{{SnapshotID: id, Path: "Release", BlobHash: rel, DeclaredSHA256: rel}},
		nil, []PackageHash{row}, nil, true); err != nil {
		t.Fatal(err)
	}
	adoptPackages(t, c, displaced, []PackageHash{pkgRow("/displaced/./d_1_amd64.deb", hashOf('d'), "d_1_amd64.deb")}, true)
	adoptPackages(t, c, displaced, nil, true) // a newer snapshot displaces it
	for _, p := range []string{"/unsigned/u_1_amd64.deb", "/displaced/d_1_amd64.deb"} {
		if got := declared(t, c, p); got != nil {
			t.Errorf("DeclaredHashesForPath(%s) = %v, want nothing", p, got)
		}
	}
}

// queryPlan returns the detail lines of EXPLAIN QUERY PLAN for q, joined.
func queryPlan(t *testing.T, c *Cache, q string, args ...any) string {
	t.Helper()
	rows, err := c.db.Query(`EXPLAIN QUERY PLAN `+q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(plan, "\n")
}

// suiteFreshnessByKey is the plan line of a suite_freshness probe by its
// primary key (design review M-2: current_snapshot_id has no index, so a
// join on it alone scans the table once per matched row).
const suiteFreshnessByKey = "SEARCH sf USING INDEX sqlite_autoindex_suite_freshness_1 (canonical_scheme=? AND canonical_host=? AND suite_path=?)"

// AIDEV-NOTE pin: the fallback's query uses the partial index over
// flat_filename (spec C6: "an EXPLAIN QUERY PLAN assertion pins it"), and
// probes suite_freshness by its primary key, never scanning it (design
// review M-2).
func TestDeclaredHashesForPath_TheFallbackUsesThePartialIndex(t *testing.T) {
	c := openCache(t)
	plan := queryPlan(t, c, declaredBySuffixSQL(3), "http", "h", "a/b.deb", "b.deb", "x/a/b.deb")
	if !strings.Contains(plan, "USING INDEX idx_package_hash_flat_filename") {
		t.Fatalf("query plan does not use idx_package_hash_flat_filename:\n%s", plan)
	}
	if !strings.Contains(plan, suiteFreshnessByKey) || strings.Contains(plan, "SCAN sf") {
		t.Fatalf("query plan does not probe suite_freshness by its key:\n%s", plan)
	}
}

// Design review M-2 on the exact lookup (spec C6 step 1, every .deb
// request): suite_freshness is probed by its primary key, never scanned.
func TestDeclaredHashesForPath_TheExactLookupProbesSuiteFreshnessByItsKey(t *testing.T) {
	c := openCache(t)
	plan := queryPlan(t, c, declaredExactSQL, "http", "h", "/a/b.deb")
	if !strings.Contains(plan, suiteFreshnessByKey) || strings.Contains(plan, "SCAN sf") {
		t.Fatalf("query plan does not probe suite_freshness by its key:\n%s", plan)
	}
}
