package cache

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// openV10Cache opens a cache directory at exactly schema 10, the last
// schema that kept package rows per snapshot in package_hash.
func openV10Cache(t *testing.T) (*sql.DB, string) {
	t.Helper()
	return openCacheAtSchema(t, 10)
}

// openCacheAtSchema opens a cache directory with migrations 0 to
// version-1 applied, leaving the database at exactly that schema.
func openCacheAtSchema(t *testing.T, version int) (*sql.DB, string) {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"pool", "tmp", "staging"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	db, err := openDB(filepath.Join(dir, "cache.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	for v := 0; v < version; v++ {
		if err := applyMigration(context.Background(), db, v); err != nil {
			_ = db.Close()
			t.Fatalf("applyMigration v%d→v%d: %v", v, v+1, err)
		}
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, dir
}

// v10 builds schema-10 rows by SQL, as a 1.1.0-rc2 daemon left them.
type v10 struct {
	t  *testing.T
	db *sql.DB
	n  int
}

func (f *v10) exec(q string, args ...any) sql.Result {
	f.t.Helper()
	res, err := f.db.Exec(q, args...)
	if err != nil {
		f.t.Fatalf("%s: %v", q, err)
	}
	return res
}

// blob inserts a blob row and returns its hash.
func (f *v10) blob() string {
	f.n++
	h := strings.Repeat("0", 60) + strings.Repeat(string("0123456789abcdef"[f.n/256%16]), 1) +
		strings.Repeat(string("0123456789abcdef"[f.n/16%16]), 1) + strings.Repeat(string("0123456789abcdef"[f.n%16]), 2)
	f.exec(`INSERT INTO blob (hash, size, created_at) VALUES (?, 1, 0)`, h)
	return h
}

func (f *v10) suite(key SuiteKey) {
	f.exec(`INSERT INTO suite_freshness (canonical_scheme, canonical_host, suite_path) VALUES (?, ?, ?)`,
		key.CanonicalScheme, key.CanonicalHost, key.SuitePath)
}

// snapshot inserts a snapshot of key: form 'inline' or 'unsigned', state
// "current", "displaced", "retired" or "candidate".
func (f *v10) snapshot(key SuiteKey, form, state string) int64 {
	f.t.Helper()
	var adopted, retired any
	if state != "candidate" {
		adopted = 100 + f.n
	}
	if state == "retired" {
		retired = 500
	}
	res := f.exec(`INSERT INTO suite_snapshot
  (canonical_scheme, canonical_host, suite_path, inrelease_hash, anchor_form, created_at, adopted_at, retired_at, heartbeat_at)
VALUES (?, ?, ?, ?, ?, 1, ?, ?, 1)`, key.CanonicalScheme, key.CanonicalHost, key.SuitePath, f.blob(), form, adopted, retired)
	id, _ := res.LastInsertId()
	if state == "current" {
		f.exec(`UPDATE suite_freshness SET current_snapshot_id = ?
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`, id, key.CanonicalScheme, key.CanonicalHost, key.SuitePath)
	}
	return id
}

func (f *v10) row(id int64, r PackageHash) {
	f.exec(`INSERT INTO package_hash (canonical_scheme, canonical_host, path, declared_sha256, snapshot_id,
                          package_name, architecture, version, flat_filename)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, r.CanonicalScheme, r.CanonicalHost, r.Path, r.DeclaredSHA256, id,
		r.PackageName, r.Architecture, r.Version, r.FlatFilename)
}

// requested gives path a url_path row a client asked for at time at,
// holding the blob whose hash is sha.
func (f *v10) requested(host, path, sha string, at int64) {
	f.exec(`INSERT OR IGNORE INTO blob (hash, size, created_at) VALUES (?, 47, 0)`, sha)
	f.exec(`INSERT INTO url_path (canonical_scheme, canonical_host, path, blob_hash, upstream_url,
                      is_metadata, last_requested_at, request_count, last_fetched_at)
VALUES ('http', ?, ?, ?, 'http://up/x', 0, ?, 1, ?)`, host, path, sha, at, at)
}

func mv11Row(host, path, letter, name, arch, version, flat string) PackageHash {
	return PackageHash{CanonicalScheme: "http", CanonicalHost: host, Path: path, DeclaredSHA256: strings.Repeat(letter, 64),
		PackageName: name, Architecture: arch, Version: version, FlatFilename: flat}
}

// migrateV11 applies the schema 10 → 11 migration to db and opens the
// cache over it.
func migrateV11(t *testing.T, db *sql.DB, dir string) *Cache {
	t.Helper()
	ctx := context.Background()
	if err := applyMigration(ctx, db, 10); err != nil {
		t.Fatalf("applyMigration v10→v11: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	c, err := Open(ctx, dir, discardSlog())
	if err != nil {
		t.Fatalf("Open after the migration: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func declaredOn(t *testing.T, c *Cache, host, path string) []string {
	t.Helper()
	rows, err := c.DeclaredHashesForPath(context.Background(), "http", host, path)
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

func listIDOf(t *testing.T, c *Cache, key SuiteKey) sql.NullInt64 {
	t.Helper()
	var id sql.NullInt64
	if err := c.db.QueryRow(`SELECT package_list_id FROM suite_freshness
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`,
		key.CanonicalScheme, key.CanonicalHost, key.SuitePath).Scan(&id); err != nil {
		t.Fatalf("package_list_id of %v: %v", key, err)
	}
	return id
}

// The schema-11 migration moves the current signed snapshots' package rows
// into one list per suite, and every reader answers from them what it
// answered from package_hash: the current rows, nothing of a displaced, a
// retired or a candidate snapshot, nothing of an unsigned one.
func TestMigrationV11_EveryReaderAnswersAsBefore(t *testing.T) {
	db, dir := openV10Cache(t)
	f := &v10{t: t, db: db}
	a := SuiteKey{CanonicalScheme: "http", CanonicalHost: "a.test", SuitePath: "/ubuntu/dists/jammy"}
	b := SuiteKey{CanonicalScheme: "http", CanonicalHost: "a.test", SuitePath: "/repo"}
	u := SuiteKey{CanonicalScheme: "http", CanonicalHost: "a.test", SuitePath: "/plain/dists/stable"}
	for _, k := range []SuiteKey{a, b, u} {
		f.suite(k)
	}
	const x, y, gone = "/ubuntu/pool/main/x_1_amd64.deb", "/ubuntu/pool/main/y_2_amd64.deb", "/ubuntu/pool/main/gone_1_amd64.deb"
	aRet := f.snapshot(a, "inline", "retired")
	aDisp := f.snapshot(a, "inline", "displaced")
	aCur := f.snapshot(a, "inline", "current")
	aCand := f.snapshot(a, "inline", "candidate")
	f.row(aRet, mv11Row("a.test", x, "1", "x", "amd64", "0.8", ""))
	f.row(aDisp, mv11Row("a.test", x, "2", "x", "amd64", "0.9", ""))
	f.row(aDisp, mv11Row("a.test", gone, "3", "gone", "amd64", "1", ""))
	f.row(aCur, mv11Row("a.test", x, "4", "x", "amd64", "1", ""))
	f.row(aCur, mv11Row("a.test", y, "5", "y", "amd64", "2", ""))
	f.row(aCur, mv11Row("a.test", "/ubuntu/dists/jammy/main/source/s_1.dsc", "6", "s", "source", "", ""))
	f.row(aCand, mv11Row("a.test", x, "7", "x", "amd64", "1.1", ""))
	bCur := f.snapshot(b, "inline", "current")
	f.row(bCur, mv11Row("a.test", "/repo/./f_1_amd64.deb", "a", "f", "amd64", "1", "f_1_amd64.deb"))
	uCur := f.snapshot(u, "unsigned", "current")
	f.row(uCur, mv11Row("a.test", "/plain/pool/p_1_amd64.deb", "b", "p", "amd64", "1", ""))
	f.requested("a.test", y, strings.Repeat("5", 64), 7000)

	c := migrateV11(t, db, dir)
	ctx := context.Background()

	var n int
	if err := c.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'package_hash'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("package_hash still exists (%d, %v)", n, err)
	}
	if v, err := readSchemaVersion(ctx, c.db); err != nil || v != 11 {
		t.Fatalf("schema version = %d, %v; want 11", v, err)
	}
	if got := listIDOf(t, c, a); !got.Valid || got.Int64 != aCur {
		t.Fatalf("suite A's list = %v, want its current snapshot's id %d", got, aCur)
	}
	if got := listIDOf(t, c, u); got.Valid {
		t.Fatalf("the unsigned suite names list %d, want none", got.Int64)
	}

	// Vouching.
	for path, want := range map[string][]string{
		x:                           {"4@/ubuntu/dists/jammy"},
		y:                           {"5@/ubuntu/dists/jammy"},
		gone:                        nil, // only the displaced snapshot listed it
		"/repo/./f_1_amd64.deb":     {"a@/repo"},
		"/repo/f_1_amd64.deb":       {"a@/repo"}, // the suffix fallback
		"/plain/pool/p_1_amd64.deb": nil,         // an unsigned snapshot never vouches
	} {
		if got := declaredOn(t, c, "a.test", path); !slices.Equal(got, want) {
			t.Errorf("DeclaredHashesForPath(%s) = %v, want %v", path, got, want)
		}
	}
	rows, err := c.DeclaredHashesForPath(ctx, "http", "a.test", x)
	if err != nil || len(rows) != 1 || rows[0].SnapshotID != aCur || rows[0].PackageName != "x" {
		t.Fatalf("DeclaredHashesForPath(x) = %+v, %v; want snapshot %d, package x", rows, err, aCur)
	}

	// The rows of each snapshot, as the test helpers see them.
	cur, err := c.PackageRowsOfSnapshotForTest(ctx, aCur)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pathsOf(cur), []string{"/ubuntu/dists/jammy/main/source/s_1.dsc", x, y}; !slices.Equal(got, want) {
		t.Fatalf("A's current rows = %v, want %v", got, want)
	}
	want := mv11Row("a.test", y, "5", "y", "amd64", "2", "")
	want.SnapshotID = aCur
	if cur[2] != want {
		t.Fatalf("a migrated row = %+v, want %+v", cur[2], want)
	}
	for name, id := range map[string]int64{"displaced": aDisp, "retired": aRet, "candidate": aCand, "unsigned": uCur} {
		if rows, err := c.PackageRowsOfSnapshotForTest(ctx, id); err != nil || len(rows) != 0 {
			t.Errorf("the %s snapshot has %d rows after the migration (%v), want none", name, len(rows), err)
		}
	}
	// Nothing but the current signed rows was copied.
	if err := c.db.QueryRow(`SELECT count(*) FROM package_list_entry`).Scan(&n); err != nil || n != 4 {
		t.Fatalf("package_list_entry holds %d rows (%v), want 4", n, err)
	}

	// The at-rest scan's candidates.
	cands, err := c.ListIntegrityCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var fromRows []string
	for _, ic := range cands {
		if ic.SourceTable == "package_hash" {
			fromRows = append(fromRows, ic.BlobHash[:1])
			if ic.SnapshotID != aCur && ic.SnapshotID != bCur {
				t.Errorf("integrity candidate %s names snapshot %d", ic.BlobHash[:1], ic.SnapshotID)
			}
		}
	}
	slices.Sort(fromRows)
	if want := []string{"4", "5", "6", "a"}; !slices.Equal(fromRows, want) {
		t.Fatalf("integrity candidates from package rows = %v, want %v", fromRows, want)
	}

	// Admin.
	cov, err := c.GetRepoCoverage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cov.PackageHashRowsBinary != 3 || cov.PackageHashRowsSource != 1 || cov.PackageHashRowsTotal != 4 ||
		!slices.Equal(cov.ArchitecturesSeen, []string{"amd64", "source"}) || cov.SnapshotsWithSources != 1 {
		t.Fatalf("GetRepoCoverage = %+v; want 3 binary, 1 source, 4 total, [amd64 source], 1 snapshot with sources", cov)
	}
	sum, err := c.GetCacheSummaryByHostArch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := sum["a.test"]["amd64"]; got != (CacheSummaryEntry{PackageHashCount: 3, BlobCount: 1, BlobBytes: 47}) {
		t.Fatalf("cache summary a.test/amd64 = %+v; want 3 rows, 1 blob of 47 bytes", got)
	}
	if got := sum["a.test"]["source"]; got != (CacheSummaryEntry{PackageHashCount: 1}) {
		t.Fatalf("cache summary a.test/source = %+v; want 1 row", got)
	}
}

func TestMigrationV11_ASecondOpenChangesNothing(t *testing.T) {
	db, dir := openV10Cache(t)
	f := &v10{t: t, db: db}
	a := SuiteKey{CanonicalScheme: "http", CanonicalHost: "a.test", SuitePath: "/ubuntu/dists/jammy"}
	f.suite(a)
	cur := f.snapshot(a, "inline", "current")
	f.row(cur, mv11Row("a.test", "/ubuntu/pool/x.deb", "4", "x", "amd64", "1", ""))
	c := migrateV11(t, db, dir)
	ctx := context.Background()
	before, err := c.PackageRowsOfSnapshotForTest(ctx, cur)
	if err != nil || len(before) != 1 {
		t.Fatalf("rows after the migration = %v, %v", before, err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = Open(ctx, dir, discardSlog())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	after, err := c.PackageRowsOfSnapshotForTest(ctx, cur)
	if err != nil || !slices.Equal(after, before) {
		t.Fatalf("rows after a second open = %v, %v; want %v", after, err, before)
	}
	if v, err := readSchemaVersion(ctx, c.db); err != nil || v != CurrentSchemaVersion {
		t.Fatalf("schema version = %d, %v", v, err)
	}
}

func hotStamp(t *testing.T, c *Cache, key SuiteKey, name string) (int64, bool) {
	t.Helper()
	var at int64
	err := c.db.QueryRow(`SELECT last_requested_at FROM hot_package
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ? AND package_name = ? AND architecture = 'amd64'`,
		key.CanonicalScheme, key.CanonicalHost, key.SuitePath, name).Scan(&at)
	if err == sql.ErrNoRows {
		return 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return at, true
}

// After the migration the hot_package triggers read the lists: a request
// for a file a live list declares records interest in its package; a
// request for a file only a displaced snapshot listed records none.
func TestMigrationV11_TheTriggersRecordInterestAgainstTheLists(t *testing.T) {
	db, dir := openV10Cache(t)
	f := &v10{t: t, db: db}
	a := SuiteKey{CanonicalScheme: "http", CanonicalHost: "a.test", SuitePath: "/ubuntu/dists/jammy"}
	f.suite(a)
	disp := f.snapshot(a, "inline", "displaced")
	cur := f.snapshot(a, "inline", "current")
	const live, old = "/ubuntu/pool/live_1_amd64.deb", "/ubuntu/pool/old_1_amd64.deb"
	f.row(cur, mv11Row("a.test", live, "4", "live", "amd64", "1", ""))
	f.row(disp, mv11Row("a.test", old, "3", "old", "amd64", "1", ""))
	c := migrateV11(t, db, dir)
	ctx := context.Background()

	for path, letter := range map[string]string{live: "4", old: "3"} {
		sha := strings.Repeat(letter, 64)
		if _, err := c.db.Exec(`INSERT INTO blob (hash, size, created_at) VALUES (?, 1, 0)`, sha); err != nil {
			t.Fatal(err)
		}
		at := int64(9000)
		if err := c.PutURLPath(ctx, URLPath{CanonicalScheme: "http", CanonicalHost: "a.test", Path: path, BlobHash: &sha,
			UpstreamURL: "http://up" + path, LastRequestedAt: &at, RequestCount: 1, LastFetchedAt: &at}); err != nil {
			t.Fatalf("PutURLPath(%s): %v", path, err)
		}
	}
	first, ok := hotStamp(t, c, a, "live")
	if !ok {
		t.Fatal("a stored request for a file the live list declares recorded no interest (the insert trigger)")
	}
	if _, ok := hotStamp(t, c, a, "old"); ok {
		t.Fatal("a request for a file only a displaced snapshot listed recorded interest")
	}
	if _, err := c.db.Exec(`UPDATE url_path SET last_requested_at = ? WHERE path = ?`, first+500, live); err != nil {
		t.Fatal(err)
	}
	if again, _ := hotStamp(t, c, a, "live"); again != first+500 {
		t.Fatalf("a later request stamp left the interest at %d, want %d (the touch trigger)", again, first+500)
	}
}

// Review focus 4: rows a healthy daemon never wrote do not stop the
// migration.
func TestMigrationV11_OddRows(t *testing.T) {
	db, dir := openV10Cache(t)
	f := &v10{t: t, db: db}
	empty := SuiteKey{CanonicalScheme: "http", CanonicalHost: "a.test", SuitePath: "/meta/dists/only"}
	foreign := SuiteKey{CanonicalScheme: "http", CanonicalHost: "a.test", SuitePath: "/ubuntu/dists/jammy"}
	never := SuiteKey{CanonicalScheme: "http", CanonicalHost: "a.test", SuitePath: "/never/dists/adopted"}
	stray := SuiteKey{CanonicalScheme: "http", CanonicalHost: "b.test", SuitePath: "/ubuntu/dists/jammy"}
	for _, k := range []SuiteKey{empty, foreign, never, stray} {
		f.suite(k)
	}
	emptyCur := f.snapshot(empty, "inline", "current") // signed, no package rows
	foreignCur := f.snapshot(foreign, "inline", "current")
	f.row(foreignCur, mv11Row("a.test", "/ubuntu/pool/x.deb", "4", "x", "amd64", "1", ""))
	f.row(foreignCur, mv11Row("other.test", "/ubuntu/pool/x.deb", "9", "x", "amd64", "1", "")) // not its suite's host
	// A suite whose pointer names another suite's snapshot.
	f.exec(`UPDATE suite_freshness SET current_snapshot_id = ? WHERE canonical_host = 'b.test'`, foreignCur)
	// A suite whose pointer names no snapshot at all: the foreign key
	// forbids it, but only on a connection that enforces foreign keys (the
	// sqlite3 shell does not by default).
	dangling := SuiteKey{CanonicalScheme: "http", CanonicalHost: "a.test", SuitePath: "/dangling/dists/s"}
	f.suite(dangling)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`PRAGMA foreign_keys = OFF`,
		`UPDATE suite_freshness SET current_snapshot_id = 987654 WHERE suite_path = '/dangling/dists/s'`,
		`PRAGMA foreign_keys = ON`} {
		if _, err := conn.ExecContext(context.Background(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	c := migrateV11(t, db, dir)
	ctx := context.Background()
	if got := listIDOf(t, c, empty); !got.Valid || got.Int64 != emptyCur {
		t.Fatalf("the signed suite without rows names list %v, want %d (an empty list)", got, emptyCur)
	}
	if got := listIDOf(t, c, never); got.Valid {
		t.Fatalf("the never-adopted suite names list %d, want none", got.Int64)
	}
	if got := listIDOf(t, c, stray); got.Valid {
		t.Fatalf("a suite pointing at another suite's snapshot names list %d, want none", got.Int64)
	}
	if got := listIDOf(t, c, dangling); got.Valid {
		t.Fatalf("a suite pointing at no snapshot names list %d, want none", got.Int64)
	}
	rows, err := c.PackageRowsOfSnapshotForTest(ctx, foreignCur)
	if err != nil || len(rows) != 1 || rows[0].CanonicalHost != "a.test" {
		t.Fatalf("rows = %+v, %v; want the suite's own row only", rows, err)
	}
	if got := declaredOn(t, c, "other.test", "/ubuntu/pool/x.deb"); got != nil {
		t.Fatalf("the row of another host vouches: %v", got)
	}
}

// A list made after the migration never takes an id a migrated list has
// (a migrated list's id is its suite's current snapshot's id).
func TestMigrationV11_ANewListIdIsAboveEveryMigratedOne(t *testing.T) {
	db, dir := openV10Cache(t)
	f := &v10{t: t, db: db}
	a := SuiteKey{CanonicalScheme: "http", CanonicalHost: "a.test", SuitePath: "/ubuntu/dists/jammy"}
	f.suite(a)
	for i := 0; i < 6; i++ {
		f.snapshot(a, "inline", "displaced")
	}
	cur := f.snapshot(a, "inline", "current") // id 7
	c := migrateV11(t, db, dir)
	other := SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/other/dists/s"}
	seedRow(t, c, other, 1)
	adoptPackages(t, c, other, []PackageHash{pkgRow("/other/pool/a.deb", hashOf('a'), "")}, true)
	if got := listIDOf(t, c, other); !got.Valid || got.Int64 <= cur {
		t.Fatalf("the new list's id = %v, want one above the migrated %d", got, cur)
	}
}
