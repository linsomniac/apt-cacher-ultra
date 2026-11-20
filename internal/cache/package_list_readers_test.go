package cache

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// unnamedLists returns the ids of the package lists no suite names.
func unnamedLists(t *testing.T, c *Cache) []int64 {
	t.Helper()
	rows, err := c.db.Query(`SELECT pl.package_list_id FROM package_list pl
 WHERE NOT EXISTS (SELECT 1 FROM suite_freshness sf WHERE sf.package_list_id = pl.package_list_id)
 ORDER BY pl.package_list_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// runPackageListGC runs sub-job D until it has nothing left.
func runPackageListGC(t *testing.T, c *Cache) (rows int) {
	t.Helper()
	for i := 0; i < 10_000; i++ {
		n, more, err := c.RunPackageListGCChunk(context.Background(), 0)
		if err != nil {
			t.Fatalf("RunPackageListGCChunk: %v", err)
		}
		rows += n
		if !more {
			return rows
		}
	}
	t.Fatal("RunPackageListGCChunk never ran dry")
	return 0
}

// readerText collects what every reader of the package lists answers,
// as one comparable text.
func readerText(t *testing.T, c *Cache, paths []string) string {
	t.Helper()
	ctx := context.Background()
	var b strings.Builder
	for _, p := range paths {
		b.WriteString("declared " + p + " = " + strings.Join(declared(t, c, p), ",") + "\n")
	}
	cands, err := c.ListIntegrityCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var hashes []string
	for _, ic := range cands {
		hashes = append(hashes, ic.SourceTable+":"+ic.BlobHash[:1])
	}
	slices.Sort(hashes)
	b.WriteString("integrity = " + strings.Join(hashes, ",") + "\n")
	cov, err := c.GetRepoCoverage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b.WriteString("coverage = " + strings.Join(cov.ArchitecturesSeen, ",") + " " +
		itoa(int(cov.PackageHashRowsBinary)) + "/" + itoa(int(cov.PackageHashRowsSource)) + "/" + itoa(int(cov.PackageHashRowsTotal)) + "\n")
	sum, err := c.GetCacheSummaryByHostArch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64", "source"} {
		e := sum["h"][arch]
		b.WriteString("summary " + arch + " = " + itoa(int(e.PackageHashCount)) + " rows, " + itoa(int(e.BlobCount)) + " blobs\n")
	}
	return b.String()
}

// A list no suite names (a rewritten list, a removed suite's, before GC)
// is invisible to every reader: vouching, the at-rest scan, coverage and
// the host summary, the url-path GC's retention, the hot-package triggers.
// Only the flat-rows flag, which may be true for no reason, sees it.
func TestPackageListReaders_AnUnnamedListIsInvisible(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/ubuntu/dists/jammy")
	seedRow(t, c, key, 1)
	const live, both, garbage = "/ubuntu/pool/live_1_amd64.deb", "/ubuntu/pool/both_1_amd64.deb", "/ubuntu/pool/garbage_1_arm64.deb"
	liveRows := []PackageHash{
		{CanonicalScheme: "http", CanonicalHost: "h", Path: live, DeclaredSHA256: hashOf('1'), PackageName: "live", Architecture: "amd64", Version: "1"},
		{CanonicalScheme: "http", CanonicalHost: "h", Path: both, DeclaredSHA256: hashOf('2'), PackageName: "both", Architecture: "amd64", Version: "1"},
	}
	adoptPackages(t, c, key, liveRows, true)
	paths := []string{live, both, garbage, "/flat/g_1_amd64.deb"}
	before := readerText(t, c, paths)
	if !strings.Contains(before, "declared "+live+" = 1@/ubuntu/dists/jammy") || !strings.Contains(before, "summary amd64 = 2 rows") {
		t.Fatalf("the fixture does not answer as expected:\n%s", before)
	}

	listID, err := c.InsertUnnamedPackageListForTest(ctx, []PackageHash{
		{CanonicalScheme: "http", CanonicalHost: "h", Path: both, DeclaredSHA256: hashOf('8'), PackageName: "both", Architecture: "amd64", Version: "9"},
		{CanonicalScheme: "http", CanonicalHost: "h", Path: garbage, DeclaredSHA256: hashOf('9'), PackageName: "garbage", Architecture: "arm64", Version: "1"},
		{CanonicalScheme: "http", CanonicalHost: "h", Path: "/flat/./g_1_amd64.deb", DeclaredSHA256: hashOf('7'), PackageName: "g", Architecture: "amd64", Version: "1", FlatFilename: "g_1_amd64.deb"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := unnamedLists(t, c); !slices.Equal(got, []int64{listID}) {
		t.Fatalf("unnamed lists = %v, want [%d]", got, listID)
	}
	c.flatRows.Store(true) // so the suffix fallback's query runs and must filter the list out itself
	if after := readerText(t, c, paths); after != before {
		t.Fatalf("an unnamed list changed a reader's answer.\nbefore:\n%s\nafter:\n%s", before, after)
	}

	// The url-path GC's retention: a cached file only the unnamed list
	// declares is not kept by the mirror rule, and the unnamed list's
	// version 9 of "both" does not push the live version 1 out of the
	// newest-N set.
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	set, err := c.topNVersionSetTx(ctx, tx, "http", "h", "/ubuntu/dists/jammy", "both", "amd64", 1, map[topNKey]map[string]struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := set["1"]; !ok || len(set) != 1 {
		t.Fatalf("newest-1 versions of both/amd64 = %v, want only the live list's 1", set)
	}
	_ = tx.Rollback()

	// The triggers: a request for the file only the unnamed list declares
	// records no interest.
	sha := hashOf('9')
	if _, err := c.db.Exec(`INSERT INTO blob (hash, size, created_at) VALUES (?, 1, 0)`, sha); err != nil {
		t.Fatal(err)
	}
	at := int64(5000)
	if err := c.PutURLPath(ctx, URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: garbage, BlobHash: &sha,
		UpstreamURL: "http://up/g", LastRequestedAt: &at, RequestCount: 1, LastFetchedAt: &at}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := c.db.QueryRow(`SELECT count(*) FROM hot_package WHERE package_name = 'garbage'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("hot_package rows for the unnamed list's package = %d (%v), want 0", n, err)
	}

	// The flat-rows prime reads live and unnamed lists alike (ruling
	// PG-31: true is never wrong).
	var exists bool
	if err := c.db.QueryRow(flatRowsExistSQL).Scan(&exists); err != nil || !exists {
		t.Fatalf("flatRowsExistSQL = %v, %v; want true: the unnamed list holds a flat row", exists, err)
	}
}

// The joins every request pays are index probes: from a request path to
// its entries, and from an entry's list to the suite that names it.
func TestPackageListReaders_QueryPlans(t *testing.T) {
	c := openCache(t)
	key := sk("h", "/ubuntu/dists/jammy")
	seedRow(t, c, key, 1)
	adoptPackages(t, c, key, []PackageHash{pkgRow("/ubuntu/pool/a.deb", hashOf('a'), "")}, true)

	exact := queryPlan(t, c, declaredExactSQL, "http", "h", "/ubuntu/pool/a.deb")
	for _, want := range []string{"USING INDEX idx_package_list_entry_path", "USING INDEX idx_suite_freshness_package_list"} {
		if !strings.Contains(exact, want) {
			t.Errorf("the exact lookup's plan lacks %q:\n%s", want, exact)
		}
	}
	if strings.Contains(exact, "SCAN sf") || strings.Contains(exact, "SCAN e") {
		t.Errorf("the exact lookup scans a table:\n%s", exact)
	}

	suffix := queryPlan(t, c, declaredBySuffixSQL(3), "http", "h", "a", "b", "c")
	if !strings.Contains(suffix, "USING INDEX idx_package_list_entry_flat") || !strings.Contains(suffix, "USING INDEX idx_suite_freshness_package_list") ||
		strings.Contains(suffix, "SCAN sf") {
		t.Errorf("the suffix fallback's plan:\n%s", suffix)
	}

	if prime := queryPlan(t, c, flatRowsExistSQL); !strings.Contains(prime, "INDEX idx_package_list_entry_flat") {
		t.Errorf("the flat-rows prime does not probe idx_package_list_entry_flat:\n%s", prime)
	}

	topN := queryPlan(t, c, topNVersionsSQL, "http", "h", "/ubuntu/dists/jammy", "p", "amd64")
	if !strings.Contains(topN, "idx_package_list_entry_pkg") || strings.Contains(topN, "SCAN e") {
		t.Errorf("the newest-N probe does not use idx_package_list_entry_pkg:\n%s", topN)
	}

	// The admin aggregates read a suite's list by the entries' primary key,
	// never by the host's (scheme, host) prefix of the path index, which
	// would walk every list of the host, unnamed ones included.
	const byList = "SEARCH p USING INDEX sqlite_autoindex_package_list_entry_1 (package_list_id=?)"
	for name, q := range map[string]string{"repo coverage": repoCoverageRowsSQL, "host summary counts": cacheSummaryCountsSQL} {
		if plan := queryPlan(t, c, q); !strings.Contains(plan, byList) {
			t.Errorf("the %s query does not read a list by its primary key:\n%s", name, plan)
		}
	}
	blobs := queryPlan(t, c, cacheSummaryBlobsSQL)
	for _, want := range []string{"SEARCH u USING INDEX idx_url_path_blob", "SEARCH p USING INDEX idx_package_list_entry_path (canonical_scheme=? AND canonical_host=? AND path=?)",
		"SEARCH sf USING INDEX idx_suite_freshness_package_list (package_list_id=?)"} {
		if !strings.Contains(blobs, want) {
			t.Errorf("the host summary's blob query lacks %q:\n%s", want, blobs)
		}
	}

	// GC's sub-job D runs inside a writer transaction (the final review's
	// M-4). Its select walks the lists in id order, without a sort, and asks
	// the suite index once per list; its chunk takes rowids from the primary
	// key's index and deletes by rowid.
	gcSelect := queryPlan(t, c, unnamedPackageListSelectSQL)
	if !strings.Contains(gcSelect, "SCAN pl") ||
		!strings.Contains(gcSelect, "SEARCH sf USING COVERING INDEX idx_suite_freshness_package_list (package_list_id=?)") ||
		strings.Contains(gcSelect, "SCAN sf") || strings.Contains(gcSelect, "TEMP B-TREE") {
		t.Errorf("GC's unnamed-list select:\n%s", gcSelect)
	}
	chunk := queryPlan(t, c, packageListChunkDeleteSQL, 1, 10)
	if !strings.Contains(chunk, "SEARCH package_list_entry USING INTEGER PRIMARY KEY (rowid=?)") ||
		!strings.Contains(chunk, "USING COVERING INDEX sqlite_autoindex_package_list_entry_1 (package_list_id=?)") ||
		strings.Contains(chunk, "SCAN ") {
		t.Errorf("GC's chunk delete:\n%s", chunk)
	}

	// The difference's read streams one list in path order off its primary
	// key: no sort, which would hold the whole list before its first row.
	live := queryPlan(t, c, liveListRowsSQL, 1)
	if !strings.Contains(live, "SEARCH package_list_entry USING INDEX sqlite_autoindex_package_list_entry_1 (package_list_id=?)") ||
		strings.Contains(live, "TEMP B-TREE") || strings.Contains(live, "SCAN ") {
		t.Errorf("the difference's read of the live list:\n%s", live)
	}

	// The hot_package triggers' SELECT, as stored, with NEW.x as parameters.
	var body string
	if err := c.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = 'hot_package_touch'`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	start, end := strings.Index(body, "SELECT"), strings.Index(body, "ON CONFLICT")
	if start < 0 || end < start {
		t.Fatalf("cannot find the trigger's SELECT in:\n%s", body)
	}
	sel := regexp.MustCompile(`NEW\.\w+`).ReplaceAllString(body[start:end], "?")
	trig := queryPlan(t, c, sel, "http", "h", "/ubuntu/pool/a.deb")
	for _, want := range []string{"idx_package_list_entry_path", "idx_suite_freshness_package_list"} {
		if !strings.Contains(trig, want) {
			t.Errorf("the trigger's plan lacks %q:\n%s", want, trig)
		}
	}
	if strings.Contains(trig, "SCAN e") || strings.Contains(trig, "SCAN sf") || strings.Contains(trig, "SCAN u") {
		t.Errorf("the trigger's SELECT scans a table:\n%s", trig)
	}
}

// Review focus 3: the daemon is stopped between a rewrite and GC. After
// the restart the old list is still on disk, still invisible, and one GC
// run removes it.
func TestPackageListReaders_AfterARestartTheOldListIsStillInvisible(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c, err := Open(ctx, dir, discardSlog())
	if err != nil {
		t.Fatal(err)
	}
	key := sk("h", "/ubuntu/dists/jammy")
	seedRow(t, c, key, 1)
	const path = "/ubuntu/pool/a_1_amd64.deb"
	// (adoptPackages names its anchor after the first row's path.)
	adoptPackages(t, c, key, []PackageHash{pkgRow("/ubuntu/pool/dropped.deb", hashOf('3'), ""), pkgRow(path, hashOf('1'), "")}, true)
	adoptPackages(t, c, key, []PackageHash{pkgRow(path, hashOf('2'), "")}, true)
	if got := unnamedLists(t, c); len(got) != 1 {
		t.Fatalf("unnamed lists after the second adoption = %v, want the first adoption's", got)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = Open(ctx, dir, discardSlog())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	check := func(when string) {
		t.Helper()
		if got := declared(t, c, path); !slices.Equal(got, []string{"2@/ubuntu/dists/jammy"}) {
			t.Fatalf("%s: declared = %v, want the second adoption's hash only", when, got)
		}
		if got := declared(t, c, "/ubuntu/pool/dropped.deb"); got != nil {
			t.Fatalf("%s: the dropped file is still vouched: %v", when, got)
		}
	}
	check("after the restart")
	if rows := runPackageListGC(t, c); rows != 2 {
		t.Fatalf("GC deleted %d rows, want the old list's 2", rows)
	}
	if got := unnamedLists(t, c); len(got) != 0 {
		t.Fatalf("unnamed lists after GC = %v, want none", got)
	}
	check("after GC")
}
