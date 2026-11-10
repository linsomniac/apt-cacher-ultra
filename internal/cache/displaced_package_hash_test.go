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

// AIDEV-NOTE: tests for snapshot-GC sub-job D (RunDisplacedPackageHashChunk,
// issue #14 follow-up Tier 2). TestPackageHashReaders_AnswerTheSameAfterTheStrip
// pins the spec's package_hash reader audit: every production reader gives
// the same answer after D strips displaced snapshots' package_hash rows. A
// new reader of displaced package_hash rows must join that test (and D's
// AIDEV-NOTE), or D must change.

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

// packageHashCount counts snapshot id's package_hash rows.
func packageHashCount(t *testing.T, c *Cache, id int64) int {
	t.Helper()
	var n int
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM package_hash WHERE snapshot_id = ?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// drainDisplaced runs sub-job D with the cache's own chunk until it reports
// nothing left, and returns the rows it deleted.
func drainDisplaced(t *testing.T, c *Cache) int {
	t.Helper()
	deleted := 0
	for i := 0; ; i++ {
		if i == 100 { // a stuck more = true fails here, not at the test timeout
			t.Fatal("sub-job D still reports more after 100 calls")
		}
		n, more, err := c.RunDisplacedPackageHashChunk(context.Background(), 0)
		if err != nil {
			t.Fatal(err)
		}
		deleted += n
		if !more {
			if n != 0 {
				t.Fatalf("sub-job D deleted %d rows in a call that reported nothing left", n)
			}
			return deleted
		}
	}
}

// Untouched-table dumps: every snapshot_member, snapshot_skipped_member and
// suite_snapshot row, in a fixed order.
const (
	dumpMembers  = `SELECT * FROM snapshot_member ORDER BY snapshot_id, path`
	dumpSkipped  = `SELECT * FROM snapshot_skipped_member ORDER BY snapshot_id, path`
	dumpHeaders  = `SELECT * FROM suite_snapshot ORDER BY snapshot_id`
	dumpPkgsOfIn = `SELECT * FROM package_hash WHERE snapshot_id IN (?, ?) ORDER BY snapshot_id, path`
)

// Issue #14 follow-up Tier 2: sub-job D strips the package_hash rows of
// adopted, non-current, non-retired snapshots — the two displaced ones and
// the one the legacy trust audit still has queued (the audit never reads
// package_hash) — and nothing else: the current snapshot's and the retired
// one's rows (sub-job C's) stay, the unadopted candidate has none to take,
// and every snapshot_member, snapshot_skipped_member and suite_snapshot row
// is as it was (forensics, the rollback check and by-hash serving read
// them).
func TestRunDisplacedPackageHashChunk_StripsOnlyAdoptedNonCurrentUnretired(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	b := newBlobs(t, c)
	key := sk("h", "/r/dists/x")
	seedRow(t, c, key, 1)
	gen := func(label string) int64 {
		return adoptGeneration(t, c, b, key,
			map[string]string{"InRelease": "InRelease " + label, "main/binary-amd64/Packages": "Packages " + label},
			[]string{"main/Contents-amd64.gz"},
			[]PackageHash{
				pkgRow("/r/pool/main/a/a_1_amd64.deb", b.of("a 1 "+label), ""),
				pkgRow("/r/pool/main/b/b_1_amd64.deb", b.of("b 1 "+label), ""),
				pkgRow("/r/pool/main/c/c_1_amd64.deb", b.of("c 1 "+label), ""),
			})
	}
	d1 := gen("d1")
	retired := gen("retired")
	d2 := gen("d2")
	cur := gen("current")
	retire(t, c, retired)
	queued := seedDisplacedAt(t, c, key, 5, true)
	for _, p := range []string{"a", "b", "c"} {
		seedPackageHash(t, c, "http", "h", "/r/pool/main/"+p+"/"+p+"_1_amd64.deb", b.of(p+" 1 queued"), queued, p, "amd64", "1")
	}
	h := b.of("Packages queued")
	if _, err := c.db.Exec(`INSERT INTO snapshot_member (snapshot_id, path, blob_hash, declared_sha256) VALUES (?, 'main/binary-amd64/Packages', ?, ?)`,
		queued, h, h); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`INSERT INTO snapshot_skipped_member (snapshot_id, path, declared_sha256, size, reason, skipped_at)
VALUES (?, 'main/Contents-amd64.gz', ?, 1, '4xx', 1)`, queued, h); err != nil {
		t.Fatal(err)
	}
	next := b.of("InRelease candidate")
	cand, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h",
		SuitePath: key.SuitePath, InReleaseHash: &next})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{d1, retired, d2, cur, queued} {
		if n := packageHashCount(t, c, id); n != 3 {
			t.Fatalf("premise: snapshot %d has %d package_hash rows, want 3", id, n)
		}
	}
	if n := packageHashCount(t, c, cand); n != 0 || !snapshotExists(t, c, cand) {
		t.Fatalf("premise: candidate %d: header %v, %d package_hash rows; want the header and none", cand, snapshotExists(t, c, cand), n)
	}
	members, skipped, headers := tableDump(t, c, dumpMembers), tableDump(t, c, dumpSkipped), tableDump(t, c, dumpHeaders)
	kept := tableDump(t, c, dumpPkgsOfIn, cur, retired)

	if n := drainDisplaced(t, c); n != 9 {
		t.Fatalf("sub-job D deleted %d rows, want the 3 rows of each of the 3 displaced snapshots", n)
	}
	for _, id := range []int64{d1, d2, queued} {
		if n := packageHashCount(t, c, id); n != 0 {
			t.Errorf("displaced snapshot %d keeps %d package_hash rows, want 0", id, n)
		}
	}
	if got := tableDump(t, c, dumpPkgsOfIn, cur, retired); !reflect.DeepEqual(got, kept) {
		t.Errorf("the current and retired snapshots' package_hash rows changed:\nbefore %v\nafter  %v", kept, got)
	}
	for _, d := range []struct {
		what, q string
		before  []string
	}{{"snapshot_member", dumpMembers, members}, {"snapshot_skipped_member", dumpSkipped, skipped}, {"suite_snapshot", dumpHeaders, headers}} {
		if got := tableDump(t, c, d.q); !reflect.DeepEqual(got, d.before) {
			t.Errorf("%s changed:\nbefore %v\nafter  %v", d.what, d.before, got)
		}
	}
}

// Sub-job D, defensively: an unadopted candidate or orphan holds no
// package_hash row in production (only CommitAdoption inserts them, in the
// transaction that adopts), so D's adopted_at term is pinned by rows seeded
// by hand onto one: D never takes them.
func TestRunDisplacedPackageHashChunk_NeverTouchesAnUnadoptedSnapshot(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	b := newBlobs(t, c)
	orphan := orphanCandidate(t, c, "http", "h", "/r/dists/x", b.of("an orphan's anchor"), 1)
	seedPackageHash(t, c, "http", "h", "/r/pool/main/o/o_1_amd64.deb", b.of("o 1"), orphan, "o", "amd64", "1")
	if n, more, err := c.RunDisplacedPackageHashChunk(ctx, 0); err != nil || n != 0 || more {
		t.Fatalf("RunDisplacedPackageHashChunk = %d, %v, %v; want nothing to work on (0, false)", n, more, err)
	}
	if n := packageHashCount(t, c, orphan); n != 1 {
		t.Fatalf("the unadopted snapshot keeps %d package_hash rows, want 1", n)
	}
}

// Sub-job D chunks a snapshot larger than one chunk: 25 rows at maxRows 10
// take three calls, each reporting more, and a fourth finds nothing left.
func TestRunDisplacedPackageHashChunk_ChunksALargeSnapshot(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	b := newBlobs(t, c)
	key := sk("h", "/r/dists/x")
	seedRow(t, c, key, 1)
	var phs []PackageHash
	for i := range 25 {
		phs = append(phs, pkgRow(fmt.Sprintf("/r/pool/main/p/p%02d_1_amd64.deb", i), b.of(fmt.Sprintf("p%02d", i)), ""))
	}
	displaced := adoptGeneration(t, c, b, key, map[string]string{"InRelease": "old"}, nil, phs)
	cur := adoptGeneration(t, c, b, key, map[string]string{"InRelease": "new"}, nil, phs)
	for i, want := range []struct {
		rows int
		more bool
		left int
	}{{10, true, 15}, {10, true, 5}, {5, true, 0}, {0, false, 0}} {
		rows, more, err := c.RunDisplacedPackageHashChunk(ctx, 10)
		if err != nil || rows != want.rows || more != want.more || packageHashCount(t, c, displaced) != want.left {
			t.Fatalf("call %d: (%d, %v, %v), %d rows left; want (%d, %v, nil), %d left",
				i+1, rows, more, err, packageHashCount(t, c, displaced), want.rows, want.more, want.left)
		}
		if n := packageHashCount(t, c, cur); n != 25 {
			t.Fatalf("call %d: the current snapshot has %d package_hash rows, want 25", i+1, n)
		}
	}
}

// Sub-job D with nothing to do: an empty cache.
func TestRunDisplacedPackageHashChunk_NothingToDo(t *testing.T) {
	c := openCache(t)
	if n, more, err := c.RunDisplacedPackageHashChunk(context.Background(), 0); err != nil || n != 0 || more {
		t.Fatalf("RunDisplacedPackageHashChunk on an empty cache = %d, %v, %v; want 0, false, nil", n, more, err)
	}
}

// The AIDEV-NOTE pin: D's candidate SELECT answers its EXISTS from
// idx_package_hash_snapshot, never by scanning package_hash.
func TestRunDisplacedPackageHashChunk_TheExistsUsesTheSnapshotIndex(t *testing.T) {
	c := openCache(t)
	plan := queryPlan(t, c, displacedPackageHashSelectSQL)
	if !strings.Contains(plan, "USING COVERING INDEX idx_package_hash_snapshot (snapshot_id=?)") || strings.Contains(plan, "SCAN p") {
		t.Fatalf("query plan does not probe idx_package_hash_snapshot for the EXISTS:\n%s", plan)
	}
}

// readerAnswers is every package_hash reader's answer in the spec's audit
// table, for one state of the cache.
type readerAnswers struct {
	Declared   map[string][]DeclaredHash
	Integrity  []IntegrityCandidate
	Coverage   RepoCoverage
	Summary    map[string]map[string]CacheSummaryEntry
	HotSets    map[string][]HotSetEntry
	URLRetains map[string]bool
}

// Issue #14 follow-up Tier 2, the first anti-goal: no reader that serves,
// vouches, reconciles, ranks, warms or reports gives a different answer
// after sub-job D strips displaced snapshots' package_hash rows. Three signed
// dists suites (one at the host root) and two signed flat suites (one nested
// in the other) on one host, each with a current snapshot and two displaced
// ones, all adopted through CommitAdoption. The displaced snapshots declare
// other hashes for paths the current ones list (and in another
// architecture, and source rows), plus paths only they list; one .deb path
// is in both /r dists suites; one flat row is in a displaced snapshot only.
// Fix round 1 of the Task 4 review added the shapes that make two readers
// discriminate:
//   - the newest-N ranking (topNVersionSetTx): suite a's displaced
//     snapshots list a withdrawn alpha 5, so a cold current alpha 2, kept
//     in the current newest 2 {3, 2}, would be reaped if displaced rows
//     were ranked ({5, 3});
//   - the flat fallback (Review Focus 4): the host-root dists suite's
//     displaced snapshots declare /flat/tool_2_amd64.deb as an exact path
//     (which, read, would suppress the fallback the flat suite's current
//     row answers it by), and the nested flat suite's displaced snapshots
//     carry tool_2_amd64.deb (a tie at base /flat) and tool_3_amd64.deb
//     under the deeper base /flat/deeper (which, read, would win the
//     longest-base choice for /flat/deeper/tool_3_amd64.deb over /flat's
//     current deeper/tool_3_amd64.deb).
//
// url_path rows give a hot set and both retention answers. Every reader's
// answer is recorded, D strips to completion, and the answers must be
// deep-equal.
//
// Readers (spec "Audit: who reads package_hash"): DeclaredHashesForPath
// (declaredExactSQL and, for the flat paths, declaredBySuffixSQL),
// ListIntegrityCandidates, GetRepoCoverage, GetCacheSummaryByHostArch,
// ComputeHotSet with each suite's current snapshot as the pre-flip prior,
// and urlPathRetainedTx (the mirror match and topNVersionSetTx). Not here:
// GetPackageHash (test-only), flatRowsExistSQL (read at Open, and the
// current flat snapshot keeps its row), and reconcile's insertPackageHashTx
// probe (it reads only a snapshot the same writer transaction checks is
// current).
func TestPackageHashReaders_AnswerTheSameAfterTheStrip(t *testing.T) {
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
	// rowsOf is each suite's package_hash rows for generation g: 0 and 1
	// are displaced, 2 is current.
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
	paths = append(paths, tableDump(t, c, `SELECT DISTINCT path FROM package_hash ORDER BY path`)...)

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
	var displacedRows int
	for _, s := range ids {
		for _, id := range s.displaced {
			displacedRows += packageHashCount(t, c, id)
		}
	}
	if displacedRows == 0 {
		t.Fatal("premise: the displaced snapshots hold no package_hash rows")
	}

	if n := drainDisplaced(t, c); n != displacedRows {
		t.Fatalf("sub-job D deleted %d rows, want the displaced snapshots' %d", n, displacedRows)
	}
	for _, s := range ids {
		for _, id := range s.displaced {
			if n := packageHashCount(t, c, id); n != 0 {
				t.Fatalf("displaced snapshot %d keeps %d package_hash rows after the strip", id, n)
			}
		}
	}

	if after := answers(); !reflect.DeepEqual(after, before) {
		t.Fatalf("a package_hash reader answers differently after the strip:\nbefore %+v\nafter  %+v", before, after)
	}
}
