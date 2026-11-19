package cache

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// AIDEV-NOTE: TestPackageListReaders_AnswerTheSameBeforeAndAfterGC is the
// reader audit for package lists (it was the audit of sub-job D's strip of
// displaced snapshots' package_hash rows, before schema 11): every
// production reader gives the same answer while the lists earlier
// adoptions wrote are still on disk, unnamed, as after GC has deleted
// them. A new reader of package_list_entry must join that test (and the
// AIDEV-NOTE at RunPackageListGCChunk). Sub-job D's own tests are in
// package_list_gc_test.go.

// blobs seeds each content as a blob once and returns its hash (seedBlob
// keyed by content, so a fixture can name one content twice).
type blobs struct {
	t    *testing.T
	c    *Cache
	seen map[string]string
}

func newBlobs(t *testing.T, c *Cache) *blobs {
	return &blobs{t: t, c: c, seen: map[string]string{}}
}

func (b *blobs) of(content string) string {
	b.t.Helper()
	if h, ok := b.seen[content]; ok {
		return h
	}
	h := seedBlob(b.t, b.c, content)
	b.seen[content] = h
	return h
}

// adoptGeneration commits a signed (inline) snapshot of key through
// InsertCandidateSnapshot and CommitAdoption, as the freshness adopter does:
// members are path → content (members["InRelease"] is the anchor), skipped
// are the suite-relative paths of Release members it declared but did not
// fetch, and phs its package_hash rows (snapshot ids filled in here). It
// returns the snapshot's id.
func adoptGeneration(t *testing.T, c *Cache, b *blobs, key SuiteKey, members map[string]string, skipped []string, phs []PackageHash) int64 {
	t.Helper()
	ctx := context.Background()
	anchor := b.of(members["InRelease"])
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{CanonicalScheme: key.CanonicalScheme,
		CanonicalHost: key.CanonicalHost, SuitePath: key.SuitePath, InReleaseHash: &anchor, PackageCoverageComplete: true})
	if err != nil {
		t.Fatal(err)
	}
	var ms []SnapshotMember
	for p, content := range members {
		h := b.of(content)
		ms = append(ms, SnapshotMember{SnapshotID: id, Path: p, BlobHash: h, DeclaredSHA256: h})
	}
	var ss []SkippedMember
	for _, p := range skipped {
		ss = append(ss, SkippedMember{SnapshotID: id, Path: p, DeclaredSHA256: b.of(fmt.Sprintf("skipped %s of %d", p, id)),
			Size: 1, Reason: "4xx"})
	}
	rows := slices.Clone(phs)
	for i := range rows {
		rows[i].SnapshotID = id
	}
	if err := c.CommitAdoption(ctx, id, ms, ss, rows, nil, true); err != nil {
		t.Fatal(err)
	}
	return id
}

// tableDump returns every row q selects, its columns joined by "|", in the
// query's order: a before/after comparison of whole tables (a one-column
// query yields its values as they are).
func tableDump(t *testing.T, c *Cache, q string, args ...any) []string {
	t.Helper()
	rows, err := c.db.Query(q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		cells := make([]string, len(vals))
		for i, v := range vals {
			cells[i] = fmt.Sprint(v)
		}
		out = append(out, strings.Join(cells, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// readerAnswers is the answer of every reader of the package lists (the
// design's "Readers" table), for one state of the cache.
type readerAnswers struct {
	Declared   map[string][]DeclaredHash
	Integrity  []IntegrityCandidate
	Coverage   RepoCoverage
	Summary    map[string]map[string]CacheSummaryEntry
	HotSets    map[string][]HotSetEntry
	URLRetains map[string]bool
}

// The package-list design's first anti-goal: no reader that serves,
// vouches, reconciles, ranks, warms or reports reads a row its suite's
// current snapshot does not declare. Three signed dists suites (one at the
// host root) and two signed flat suites (one nested in the other) on one
// host, each adopted three times through CommitAdoption, so each has a
// live list (generation 2) and two unnamed ones (generations 0 and 1)
// that GC has not deleted yet. The earlier generations declare other
// hashes for paths the current ones list (and in another architecture,
// and source rows), plus paths only they list; one .deb path is in both
// /r dists suites; one flat row is in an earlier generation only. The
// shapes that make two readers discriminate (from the issue #14 review):
//   - the newest-N ranking (topNVersionSetTx): suite a's earlier
//     generations list a withdrawn alpha 5, so a cold current alpha 2,
//     kept in the current newest 2 {3, 2}, would be reaped if an unnamed
//     list were ranked ({5, 3});
//   - the flat fallback: the host-root dists suite's earlier generations
//     declare /flat/tool_2_amd64.deb as an exact path (which, read, would
//     suppress the fallback the flat suite's current row answers it by),
//     and the nested flat suite's earlier generations carry
//     tool_2_amd64.deb (a tie at base /flat) and tool_3_amd64.deb under
//     the deeper base /flat/deeper (which, read, would win the
//     longest-base choice for /flat/deeper/tool_3_amd64.deb over /flat's
//     current deeper/tool_3_amd64.deb).
//
// url_path rows give a hot set and both retention answers. Every reader's
// answer is recorded with the unnamed lists on disk, GC deletes them, and
// the answers must be deep-equal. The premises check the answers
// themselves: they are the current rows'.
//
// Readers: DeclaredHashesForPath (declaredExactSQL and, for the flat
// paths, declaredBySuffixSQL), ListIntegrityCandidates, GetRepoCoverage,
// GetCacheSummaryByHostArch, ComputeHotSet (it reads hot_package, which
// the triggers and the commits fill from the lists), and urlPathRetainedTx
// (the mirror match and topNVersionSetTx). Not here: GetPackageHash
// (test-only), flatRowsExistSQL (it reads unnamed lists on purpose), and
// reconcile's insertPackageHashTx probe (it reads only the list the same
// writer transaction found the suite naming).
func TestPackageListReaders_AnswerTheSameBeforeAndAfterGC(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	b := newBlobs(t, c)
	const now = int64(1_800_000_000)
	a, bb, flat := sk("h", "/r/dists/a"), sk("h", "/r/dists/b"), sk("h", "/flat")
	root, deeper := sk("h", "/dists/c"), sk("h", "/flat/deeper") // a host-root dists suite; a flat suite nested in /flat
	suites := []SuiteKey{a, bb, root, flat, deeper}
	isFlat := map[SuiteKey]bool{flat: true, deeper: true}
	row := func(path, content, name, arch, version, flatName string) PackageHash {
		return PackageHash{CanonicalScheme: "http", CanonicalHost: "h", Path: path, DeclaredSHA256: b.of(content),
			PackageName: name, Architecture: arch, Version: version, FlatFilename: flatName}
	}
	pool := "/r/pool/main/"
	// rowsOf is each suite's package rows for generation g: 0 and 1 are
	// the earlier adoptions' (unnamed lists by the end), 2 is current.
	rowsOf := func(key SuiteKey, g int) []PackageHash {
		cur := g == 2
		tag := fmt.Sprintf(" %s gen %d", key.SuitePath, g) // a displaced snapshot's content (so another hash)
		if cur {
			tag = ""
		}
		switch key {
		case a:
			rs := []PackageHash{
				row(pool+"a/alpha_2_amd64.deb", "alpha 2"+tag, "alpha", "amd64", "2", ""),
				row(pool+"a/alpha_3_amd64.deb", "alpha 3"+tag, "alpha", "amd64", "3", ""),
				row(pool+"s/shared_1_amd64.deb", "shared 1"+tag, "shared", "amd64", "1", ""),
				row("/r/dists/a/main/binary-amd64/Packages.diff/T-1.gz", "pdiff a"+tag, "", "amd64", "", ""),
			}
			if cur {
				return append(rs,
					row(pool+"a/alpha_1_amd64.deb", "alpha 1", "alpha", "amd64", "1", ""),
					row(pool+"a/alpha_3.dsc", "alpha 3 dsc", "alpha", "source", "", ""))
			}
			return append(rs,
				row(pool+"a/alpha_0_amd64.deb", "alpha 0"+tag, "alpha", "amd64", "0", ""), // displaced only
				row(pool+"a/alpha_5_amd64.deb", "alpha 5"+tag, "alpha", "amd64", "5", ""), // withdrawn: displaced only, the newest
				row(pool+"a/alpha_2_i386.deb", "alpha 2 i386"+tag, "alpha", "i386", "2", ""))
		case bb:
			rs := []PackageHash{
				row(pool+"b/beta_1_amd64.deb", "beta 1"+tag, "beta", "amd64", "1", ""),
				row(pool+"s/shared_1_amd64.deb", "shared 1"+tag, "shared", "amd64", "1", ""),
			}
			if cur {
				return rs
			}
			return append(rs,
				row(pool+"b/beta_0_amd64.deb", "beta 0"+tag, "beta", "amd64", "0", ""),
				row(pool+"b/beta_1.dsc", "beta 1 dsc"+tag, "beta", "source", "", ""))
		case root: // archive root "/": Filename "flat/tool_2_amd64.deb" is the path /flat/tool_2_amd64.deb
			rs := []PackageHash{row("/pool/main/g/gamma_1_amd64.deb", "gamma 1"+tag, "gamma", "amd64", "1", "")}
			if cur {
				return rs
			}
			return append(rs, row("/flat/tool_2_amd64.deb", "tool 2 at the root"+tag, "tool", "amd64", "2", ""))
		case flat:
			rs := []PackageHash{row("/flat/./tool_2_amd64.deb", "tool 2"+tag, "tool", "amd64", "2", "tool_2_amd64.deb")}
			if cur {
				rs = append(rs, row("/flat/./deeper/tool_3_amd64.deb", "tool 3", "tool", "amd64", "3", "deeper/tool_3_amd64.deb"))
			}
			if g == 0 {
				rs = append(rs, row("/flat/./tool_1_amd64.deb", "tool 1"+tag, "tool", "amd64", "1", "tool_1_amd64.deb"))
			}
			return rs
		default: // deeper
			rs := []PackageHash{row("/flat/deeper/./gadget_1_amd64.deb", "gadget 1"+tag, "gadget", "amd64", "1", "gadget_1_amd64.deb")}
			if cur {
				return rs
			}
			return append(rs,
				row("/flat/deeper/./tool_2_amd64.deb", "tool 2 deeper"+tag, "tool", "amd64", "2", "tool_2_amd64.deb"),
				row("/flat/deeper/./tool_3_amd64.deb", "tool 3 deeper"+tag, "tool", "amd64", "3", "tool_3_amd64.deb"))
		}
	}
	type suiteIDs struct{ displaced, current []int64 }
	ids := map[SuiteKey]*suiteIDs{}
	for _, key := range suites {
		seedRow(t, c, key, 1)
		ids[key] = &suiteIDs{}
		index := "main/binary-amd64/Packages"
		if isFlat[key] {
			index = "Packages"
		}
		for g := range 3 {
			members := map[string]string{
				"InRelease": fmt.Sprintf("InRelease %s gen %d", key.SuitePath, g),
				index:       fmt.Sprintf("Packages %s gen %d", key.SuitePath, g),
			}
			if !isFlat[key] {
				members["main/binary-amd64/Packages.diff/Index"] = fmt.Sprintf("pdiff Index %s gen %d", key.SuitePath, g)
			}
			id := adoptGeneration(t, c, b, key, members, []string{"Contents-amd64.gz"}, rowsOf(key, g))
			if g < 2 {
				ids[key].displaced = append(ids[key].displaced, id)
			} else {
				ids[key].current = append(ids[key].current, id)
			}
		}
	}

	// url_path rows: a hot set (requested a minute ago) and cold rows (40
	// days ago) cached under the current hash, a displaced hash, or for a
	// path only displaced snapshots list.
	hot, cold := sql.NullInt64{Int64: now - 60, Valid: true}, sql.NullInt64{Int64: now - 40*86400, Valid: true}
	urls := []struct {
		path, content string
		at            sql.NullInt64
	}{
		{pool + "a/alpha_3_amd64.deb", "alpha 3", hot},
		{pool + "b/beta_1_amd64.deb", "beta 1", hot},
		{pool + "s/shared_1_amd64.deb", "shared 1", hot},
		{"/flat/./tool_2_amd64.deb", "tool 2", hot},
		{pool + "a/alpha_0_amd64.deb", "alpha 0 /r/dists/a gen 0", hot},
		{pool + "a/alpha_1_amd64.deb", "alpha 1", cold},                                         // current hash, not in the newest 2
		{pool + "a/alpha_2_amd64.deb", "alpha 2", cold},                                         // current hash, in the current newest 2 (not with alpha 5)
		{"/r/dists/a/main/binary-amd64/Packages.diff/T-1.gz", "pdiff a /r/dists/a gen 1", cold}, // a displaced hash
		{pool + "a/alpha_2_i386.deb", "alpha 2 i386 /r/dists/a gen 0", cold},
		{pool + "a/alpha_3.dsc", "alpha 3 dsc", cold}, // version-less: the fallback keeps it
		{pool + "b/beta_0_amd64.deb", "beta 0 /r/dists/b gen 1", cold},
		{"/flat/./tool_1_amd64.deb", "tool 1 /flat gen 0", cold},
		{pool + "b/beta_1.dsc", "beta 1 dsc /r/dists/b gen 0", sql.NullInt64{}},
	}
	for _, u := range urls {
		putURLPathRow(t, c, "http", "h", u.path, b.of(u.content), u.at, false)
	}

	// The hot-set reader runs before a flip: each suite has a candidate
	// (unadopted, as the adopter's) whose in-memory rows are the current
	// ones with a newer alpha.
	cands := map[SuiteKey]int64{}
	candRows := map[SuiteKey][]PackageHash{}
	for _, key := range suites {
		anchor := b.of("InRelease candidate " + key.SuitePath)
		id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h",
			SuitePath: key.SuitePath, InReleaseHash: &anchor})
		if err != nil {
			t.Fatal(err)
		}
		cands[key] = id
		rs := rowsOf(key, 2)
		if key == a {
			rs = append(rs, row(pool+"a/alpha_4_amd64.deb", "alpha 4", "alpha", "amd64", "4", ""))
		}
		for i := range rs {
			rs[i].SnapshotID = id
		}
		candRows[key] = rs
	}

	paths := []string{"/flat/tool_2_amd64.deb", "/flat/tool_1_amd64.deb", "/flat/deeper/tool_3_amd64.deb"} // the suffix fallback's
	paths = append(paths, tableDump(t, c, `SELECT DISTINCT path FROM package_list_entry ORDER BY path`)...)

	answers := func() readerAnswers {
		t.Helper()
		var r readerAnswers
		r.Declared = map[string][]DeclaredHash{}
		for _, p := range paths {
			d, err := c.DeclaredHashesForPath(ctx, "http", "h", p)
			if err != nil {
				t.Fatal(err)
			}
			slices.SortFunc(d, func(x, y DeclaredHash) int {
				return cmp.Or(cmp.Compare(x.SnapshotID, y.SnapshotID), cmp.Compare(x.DeclaredSHA256, y.DeclaredSHA256))
			})
			r.Declared[p] = d
		}
		var err error
		if r.Integrity, err = c.ListIntegrityCandidates(ctx); err != nil {
			t.Fatal(err)
		}
		slices.SortFunc(r.Integrity, func(x, y IntegrityCandidate) int { return cmp.Compare(x.BlobHash, y.BlobHash) })
		if r.Coverage, err = c.GetRepoCoverage(ctx); err != nil {
			t.Fatal(err)
		}
		if r.Summary, err = c.GetCacheSummaryByHostArch(ctx); err != nil {
			t.Fatal(err)
		}
		r.HotSets = map[string][]HotSetEntry{}
		for _, key := range suites {
			hs, err := c.ComputeHotSet(ctx, "http", "h", ids[key].current[0], cands[key], slices.Clone(candRows[key]), 86400, now, 2)
			if err != nil {
				t.Fatal(err)
			}
			r.HotSets[key.SuitePath] = hs
		}
		tx, err := c.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		// Each url_path row's own columns, as RunURLPathGCBatch passes them.
		type urlRow struct {
			path    string
			blob    sql.NullString
			lastReq sql.NullInt64
		}
		var urows []urlRow
		rs, err := tx.QueryContext(ctx, `SELECT path, blob_hash, last_requested_at FROM url_path
 WHERE canonical_scheme = 'http' AND canonical_host = 'h' ORDER BY path`)
		if err != nil {
			t.Fatal(err)
		}
		for rs.Next() {
			var u urlRow
			if err := rs.Scan(&u.path, &u.blob, &u.lastReq); err != nil {
				t.Fatal(err)
			}
			urows = append(urows, u)
		}
		if err := rs.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rs.Close()
		if len(urows) < len(urls) { // the seeded rows, plus any adoption wrote (the anchors')
			t.Fatalf("read %d url_path rows, want at least the %d seeded", len(urows), len(urls))
		}
		r.URLRetains = map[string]bool{}
		for _, u := range urows {
			keep, err := c.urlPathRetainedTx(ctx, tx, "http", "h", u.path, u.blob, u.lastReq, now-7*86400, 2,
				map[topNKey]map[string]struct{}{})
			if err != nil {
				t.Fatal(err)
			}
			r.URLRetains[u.path] = keep
		}
		return r
	}

	before := answers()
	// Premises: the fixture exercises each reader (an empty answer would
	// pass vacuously).
	if len(before.Declared[pool+"s/shared_1_amd64.deb"]) != 2 || len(before.Declared["/flat/tool_2_amd64.deb"]) != 1 ||
		len(before.Declared["/flat/tool_1_amd64.deb"]) != 0 || len(before.Declared[pool+"a/alpha_0_amd64.deb"]) != 0 {
		t.Fatalf("premise: DeclaredHashesForPath answers %+v", before.Declared)
	}
	// Review Focus 4: each flat request path answers with only the /flat
	// current snapshot's row, by the suffix fallback — not the host-root
	// suite's displaced exact row, nor /flat/deeper's displaced rows (a tie
	// at base /flat for tool_2, the longer base for tool_3).
	for p, content := range map[string]string{"/flat/tool_2_amd64.deb": "tool 2", "/flat/deeper/tool_3_amd64.deb": "tool 3"} {
		want := []DeclaredHash{{DeclaredSHA256: b.of(content), SnapshotID: ids[flat].current[0], PackageName: "tool", SuitePath: flat.SuitePath}}
		if got := before.Declared[p]; !reflect.DeepEqual(got, want) {
			t.Fatalf("premise: DeclaredHashesForPath(%s) = %+v, want only the /flat current row %+v", p, got, want)
		}
	}
	// The newest-N ranking reads only the current snapshot: alpha 2 (cold,
	// current hash) is in the current newest 2 {3, 2}, alpha 1 is not.
	if !before.URLRetains[pool+"a/alpha_2_amd64.deb"] || before.URLRetains[pool+"a/alpha_1_amd64.deb"] {
		t.Fatalf("premise: retention of alpha 2 %v (want kept), alpha 1 %v (want reaped)",
			before.URLRetains[pool+"a/alpha_2_amd64.deb"], before.URLRetains[pool+"a/alpha_1_amd64.deb"])
	}
	if len(before.Integrity) == 0 || before.Coverage.PackageHashRowsTotal == 0 || len(before.Summary["h"]) == 0 ||
		len(before.HotSets[a.SuitePath]) == 0 || len(before.HotSets[flat.SuitePath]) == 0 {
		t.Fatalf("premise: an empty answer: integrity %d, coverage %+v, summary %+v, hot sets %+v",
			len(before.Integrity), before.Coverage, before.Summary, before.HotSets)
	}
	if slices.Contains(before.Coverage.ArchitecturesSeen, "i386") {
		t.Fatalf("premise: architectures %v include the displaced-only i386", before.Coverage.ArchitecturesSeen)
	}
	var kept, reaped int
	for _, k := range before.URLRetains {
		if k {
			kept++
		} else {
			reaped++
		}
	}
	if kept == 0 || reaped == 0 {
		t.Fatalf("premise: retention answers %v are not mixed", before.URLRetains)
	}
	// Each suite's two earlier lists are still on disk, unnamed.
	if got := unnamedLists(t, c); len(got) != 2*len(suites) {
		t.Fatalf("premise: %d unnamed lists, want %d (two earlier generations a suite)", len(got), 2*len(suites))
	}
	var unnamedRows int
	if err := c.db.QueryRow(`SELECT count(*) FROM package_list_entry e
 WHERE NOT EXISTS (SELECT 1 FROM suite_freshness sf WHERE sf.package_list_id = e.package_list_id)`).Scan(&unnamedRows); err != nil {
		t.Fatal(err)
	}
	if unnamedRows == 0 {
		t.Fatal("premise: the unnamed lists hold no rows")
	}

	if n := runPackageListGC(t, c); n != unnamedRows {
		t.Fatalf("sub-job D deleted %d rows, want the unnamed lists' %d", n, unnamedRows)
	}
	if got := unnamedLists(t, c); len(got) != 0 {
		t.Fatalf("unnamed lists after GC: %v", got)
	}

	if after := answers(); !reflect.DeepEqual(after, before) {
		t.Fatalf("a reader of the package lists answers differently once GC has deleted the unnamed ones:\nbefore %+v\nafter  %+v", before, after)
	}
}
