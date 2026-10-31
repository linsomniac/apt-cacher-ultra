package cache

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
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
	adopt("/ubuntu/dists/noble", pkgRow("/ubuntu/pool/main/f/foo_1_amd64.deb", hashOf('e'), ""))
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
		{"a dists suite's pool, exact", "/ubuntu/pool/main/f/foo_1_amd64.deb", []string{"e@/ubuntu/dists/noble"}},
		// DotClean gives exactly the dists row's path, but a dists row's
		// flat_filename is "": it never answers the fallback.
		{"a dists row never answers the fallback", "/ubuntu/./pool/main/f/foo_1_amd64.deb", nil},
		{"a dists row under another base", "/mirror/ubuntu/pool/main/f/foo_1_amd64.deb", nil},
	} {
		if got := declared(t, c, tc.path); !slices.Equal(got, tc.want) {
			t.Errorf("%s: DeclaredHashesForPath(%s) = %v, want %v", tc.name, tc.path, got, tc.want)
		}
	}
}

// flatSplits' stops at the unit level (spec C6; the Task 11 review's
// Minor 1): a request path whose dot-cleaned form is relative, has an empty
// or ".." segment, or has more than maxFallbackSegments segments has no
// split. Through DeclaredHashesForPath the ".." and empty-segment stops
// are unobservable with valid rows — such a B is never an ancestor of a
// canonical FlatBase, and such an F never equals a validated
// flat_filename — so they are pinned here, on the function itself.
func TestFlatSplits(t *testing.T) {
	bases, names := flatSplits("/a/./b/x.deb")
	if !slices.Equal(bases, []string{"", "/a", "/a/b"}) || !slices.Equal(names, []string{"a/b/x.deb", "b/x.deb", "x.deb"}) {
		t.Fatalf("flatSplits(/a/./b/x.deb) = %q, %q", bases, names)
	}
	if _, names := flatSplits("/" + strings.Repeat("d/", maxFallbackSegments-1) + "x.deb"); len(names) != maxFallbackSegments {
		t.Fatalf("%d splits of a %d-segment path, want %d", len(names), maxFallbackSegments, maxFallbackSegments)
	}
	for _, p := range []string{
		"/a/../b/x.deb", "/a//x.deb", "/a/x.deb/", "a/x.deb", "./a/x.deb",
		"/" + strings.Repeat("d/", maxFallbackSegments) + "x.deb",
	} {
		if bases, names := flatSplits(p); bases != nil || names != nil {
			t.Errorf("flatSplits(%q) = %q, %q; want no split", p, bases, names)
		}
	}
	// Ruling PG-30: a split can match a row only with B no longer than a
	// suite path (aptpath.MaxSuitePathBytes) and F no longer than a
	// Filename (aptpath.MaxFilenameBytes), so other splits are skipped and
	// a dot-cleaned path longer than both plus a "/" has none. The bound is
	// on the dot-cleaned path: "/." segments a client adds do not count.
	base := "/" + strings.Repeat("a", aptpath.MaxSuitePathBytes-1)
	name := strings.Repeat("n", aptpath.MaxFilenameBytes)
	for _, tc := range []struct {
		path         string
		bases, names []string
	}{
		{base + "/" + name, []string{base}, []string{name}}, // at the bound: B = base only
		{base + "/" + name + "n", nil, nil},                 // one byte over
		{base + strings.Repeat("/.", 100) + "/" + name, []string{base}, []string{name}},
		{base + "a/x.deb", []string{""}, []string{base[1:] + "a/x.deb"}}, // B one byte over a suite path
		{"/d/" + name, []string{"/d"}, []string{name}},                   // "d/"+name is over a Filename
	} {
		if bases, names := flatSplits(tc.path); !slices.Equal(bases, tc.bases) || !slices.Equal(names, tc.names) {
			t.Errorf("flatSplits(%d-byte path) = %d bases %.40q, names %.40q; want %d", len(tc.path), len(bases), bases, names, len(tc.bases))
		}
	}
	// Bases and names are substrings of the dot-cleaned path, not strings
	// built per split: with strings.Join each of the 32 splits allocated
	// twice, and a long path made every request allocate 32 times its
	// length (the final review's concurrency lens I-2).
	long := "/" + strings.Repeat("d/", maxFallbackSegments-1) + "x.deb"
	if allocs := testing.AllocsPerRun(20, func() { flatSplits(long) }); allocs > 6 {
		t.Errorf("flatSplits of a %d-segment path allocates %v times, want at most 6", maxFallbackSegments, allocs)
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

// Spec C6 step 2 keeps EVERY row at the longest B, so step 3's dispatch
// sees two signed suites at one base that declare different hashes for one
// Filename, and refuses them as package_hash_conflict, instead of answering
// with whichever row SQLite returned first: the twins of residual 2 reached
// by a "./" request that misses their exact rows, and two channels of one
// host-root repository (Sublime's apt/stable and apt/dev, B = ""). A
// shorter B never joins the tie.
func TestDeclaredHashesForPath_TiesAtTheLongestBaseAreAllKept(t *testing.T) {
	c := openCache(t)
	for _, a := range []struct {
		suite string
		row   PackageHash
	}{
		{"/t", pkgRow("/t/x_1_amd64.deb", hashOf('a'), "x_1_amd64.deb")},
		{"/t/.", pkgRow("/t/x_1_amd64.deb", hashOf('b'), "x_1_amd64.deb")},
		{"/u", pkgRow("/u/t/x_1_amd64.deb", hashOf('f'), "t/x_1_amd64.deb")}, // B = "": shorter
		{"/apt/stable", pkgRow("/apt/stable/files/s_1_amd64.deb", hashOf('c'), "files/s_1_amd64.deb")},
		{"/apt/dev", pkgRow("/apt/dev/files/s_1_amd64.deb", hashOf('d'), "files/s_1_amd64.deb")},
	} {
		seedRow(t, c, sk("h", a.suite), 1)
		adoptPackages(t, c, sk("h", a.suite), []PackageHash{a.row}, true)
	}
	for _, tc := range []struct {
		name, path string
		want       []string
	}{
		{"the twins through ./", "/t/./x_1_amd64.deb", []string{"a@/t", "b@/t/."}},
		{"two channels at the host root", "/files/s_1_amd64.deb", []string{"c@/apt/stable", "d@/apt/dev"}},
	} {
		if got := declared(t, c, tc.path); !slices.Equal(got, tc.want) {
			t.Errorf("%s: DeclaredHashesForPath(%s) = %v, want every row at the longest base %v", tc.name, tc.path, got, tc.want)
		}
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

// Spec C6 "non-retired": a snapshot retired through either retirement
// path — the legacy audit's quarantine and the reaper's delete — vouches
// nothing, by its exact rows or through the fallback, although its
// package_hash rows are still there. Neither path leaves a current
// pointer at a retired snapshot: the quarantine clears the pointer before
// it stamps retired_at, and the reaper stamps retired_at and then deletes
// the suite's row, in one transaction (suite_reap.go deleteSuiteTx). This
// pins that against a future retirement path that leaves the pointer
// set.
func TestDeclaredHashesForPath_ARetiredSnapshotVouchesNothing(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	quarantined, reaped := sk("h", "/quarantined"), sk("h", "/reaped")
	seedRow(t, c, quarantined, 1)
	seedRow(t, c, reaped, 1)
	qID := adoptPackages(t, c, quarantined, []PackageHash{pkgRow("/quarantined/./q_1_amd64.deb", hashOf('a'), "q_1_amd64.deb")}, true)
	rID := adoptPackages(t, c, reaped, []PackageHash{pkgRow("/reaped/./r_1_amd64.deb", hashOf('b'), "r_1_amd64.deb")}, true)
	paths := []string{"/quarantined/./q_1_amd64.deb", "/quarantined/q_1_amd64.deb", "/reaped/./r_1_amd64.deb", "/reaped/r_1_amd64.deb"}
	for _, p := range paths {
		if got := declared(t, c, p); len(got) != 1 {
			t.Fatalf("before retiring: DeclaredHashesForPath(%s) = %v, want one row", p, got)
		}
	}
	if _, err := c.ApplyLegacyAuditDecisions(ctx, []LegacyAuditDecision{{SnapshotID: qID, Outcome: LegacyAuditQuarantine}}); err != nil {
		t.Fatal(err)
	}
	if del, _, err := c.ReapIdleSuite(ctx, reaped, &rID, ReapRegardless); err != nil || !del {
		t.Fatalf("ReapIdleSuite = %v, %v; want the suite reaped", del, err)
	}
	for _, id := range []int64{qID, rID} {
		var retired sql.NullInt64
		if err := c.db.QueryRowContext(ctx, `SELECT retired_at FROM suite_snapshot WHERE snapshot_id = ?`, id).Scan(&retired); err != nil || !retired.Valid {
			t.Fatalf("snapshot %d: retired_at %v, %v; want it retired", id, retired, err)
		}
		var rows int
		if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM package_hash WHERE snapshot_id = ?`, id).Scan(&rows); err != nil || rows != 1 {
			t.Fatalf("snapshot %d: %d package_hash rows, %v; want its row still there", id, rows, err)
		}
	}
	for _, p := range paths {
		if got := declared(t, c, p); got != nil {
			t.Errorf("after retiring: DeclaredHashesForPath(%s) = %v, want nothing", p, got)
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
//
// It is pinned at every IN size production builds, up to the worst case
// (maxFallbackSegments names), and no table is scanned.
func TestDeclaredHashesForPath_TheFallbackUsesThePartialIndex(t *testing.T) {
	c := openCache(t)
	for _, n := range []int{1, 3, maxFallbackSegments} {
		args := []any{"http", "h"}
		for i := range n {
			args = append(args, strings.Repeat("d/", i)+"b.deb")
		}
		plan := queryPlan(t, c, declaredBySuffixSQL(n), args...)
		if !strings.Contains(plan, "USING INDEX idx_package_hash_flat_filename") {
			t.Fatalf("n=%d: query plan does not use idx_package_hash_flat_filename:\n%s", n, plan)
		}
		if !strings.Contains(plan, suiteFreshnessByKey) || strings.Contains(plan, "SCAN ") {
			t.Fatalf("n=%d: query plan scans a table or does not probe suite_freshness by its key:\n%s", n, plan)
		}
	}
}

// Design review M-2 on the exact lookup (spec C6 step 1, every .deb
// request): suite_freshness is probed by its primary key, never scanned.
func TestDeclaredHashesForPath_TheExactLookupProbesSuiteFreshnessByItsKey(t *testing.T) {
	c := openCache(t)
	plan := queryPlan(t, c, declaredExactSQL, "http", "h", "/a/b.deb")
	if !strings.Contains(plan, suiteFreshnessByKey) || strings.Contains(plan, "SCAN ") {
		t.Fatalf("query plan scans a table or does not probe suite_freshness by its key:\n%s", plan)
	}
}

// Ruling PG-31 (the final review's concurrency lens m-1): the cache's
// "flat rows may exist" flag. Open primes it from package_hash — false on
// a cache with no flat row, true on one with — by one probe of the partial
// index; CommitAdoption and reconcile's insert set it before the
// transaction that inserts a flat row commits, so a fallback lookup right
// after finds the row. While it is false no fallback query runs: shown by
// a flat row written behind the cache's back (raw SQL, which no writer
// does), which the fallback finds only once a reopen has primed the flag.
func TestDeclaredHashesForPath_TheFlatRowsFlag(t *testing.T) {
	ctx := context.Background()
	flat := sk("h", "/repo")
	row := func() PackageHash { return pkgRow("/repo/x_1_amd64.deb", hashOf('a'), "x_1_amd64.deb") }
	const twin = "/repo/./x_1_amd64.deb" // misses the exact row: the fallback's to answer
	t.Run("a commit sets it", func(t *testing.T) {
		c := openCache(t)
		if c.flatRows.Load() {
			t.Fatal("a fresh cache's flag is true, want false")
		}
		seedRow(t, c, flat, 1)
		adoptPackages(t, c, flat, []PackageHash{row()}, true)
		if got := declared(t, c, twin); !slices.Equal(got, []string{"a@/repo"}) {
			t.Fatalf("fallback after CommitAdoption = %v, want [a@/repo]", got)
		}
	})
	t.Run("reconcile's insert sets it", func(t *testing.T) {
		c := openCache(t)
		seedRow(t, c, flat, 1)
		id := adoptPackages(t, c, flat, nil, true)
		r := row()
		r.SnapshotID = id
		if err := c.InsertReconciledMembers(ctx, id, nil, []PackageHash{r}); err != nil {
			t.Fatal(err)
		}
		if got := declared(t, c, twin); !slices.Equal(got, []string{"a@/repo"}) {
			t.Fatalf("fallback after InsertReconciledMembers = %v, want [a@/repo]", got)
		}
	})
	t.Run("Open primes it", func(t *testing.T) {
		dir := t.TempDir()
		c, err := Open(ctx, dir, discardSlog())
		if err != nil {
			t.Fatal(err)
		}
		seedRow(t, c, flat, 1)
		adoptPackages(t, c, flat, []PackageHash{row()}, true)
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		c, err = Open(ctx, dir, discardSlog())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		if !c.flatRows.Load() {
			t.Fatal("a cache opened over a flat row has its flag false, want true")
		}
		if got := declared(t, c, twin); !slices.Equal(got, []string{"a@/repo"}) {
			t.Fatalf("fallback after a reopen = %v, want [a@/repo]", got)
		}
		if plan := queryPlan(t, c, flatRowsExistSQL); !strings.Contains(plan, "INDEX idx_package_hash_flat_filename") {
			t.Fatalf("the prime does not probe idx_package_hash_flat_filename:\n%s", plan)
		}
	})
	t.Run("false skips the fallback", func(t *testing.T) {
		dir := t.TempDir()
		c, err := Open(ctx, dir, discardSlog())
		if err != nil {
			t.Fatal(err)
		}
		seedRow(t, c, flat, 1)
		r := row()
		r.FlatFilename = ""
		adoptPackages(t, c, flat, []PackageHash{r}, true)
		if _, err := c.db.Exec(`UPDATE package_hash SET flat_filename = 'x_1_amd64.deb'`); err != nil {
			t.Fatal(err)
		}
		if got := declared(t, c, twin); got != nil {
			t.Fatalf("fallback with the flag false = %v, want none: no fallback query runs", got)
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		c, err = Open(ctx, dir, discardSlog())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		if got := declared(t, c, twin); !slices.Equal(got, []string{"a@/repo"}) {
			t.Fatalf("fallback after the prime = %v, want [a@/repo]: the row was there all along", got)
		}
	})
}
