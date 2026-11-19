package cache

import (
	"database/sql"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

// AIDEV-NOTE: the readers' SQL as it was at schema 10 (main 421f5ea, the
// last commit that read package_hash), kept here for one purpose: the
// schema-11 migration can never be edited once released, and this is the
// record of what "every reader answers as before" was checked against.
// Do not update these to follow production; production's side of the
// comparison is the constants the readers themselves run.
const (
	v10ReaderExact = `
SELECT DISTINCT p.declared_sha256, p.snapshot_id, p.package_name, sf.suite_path
  FROM package_hash p
  JOIN suite_snapshot ss  ON ss.snapshot_id = p.snapshot_id
  JOIN suite_freshness sf ON sf.canonical_scheme    = ss.canonical_scheme
                         AND sf.canonical_host      = ss.canonical_host
                         AND sf.suite_path          = ss.suite_path
                         AND sf.current_snapshot_id = p.snapshot_id
 WHERE p.canonical_scheme = ?
   AND p.canonical_host   = ?
   AND p.path             = ?
   AND ss.anchor_form    <> 'unsigned'`
	v10ReaderSuffix = `
SELECT DISTINCT p.declared_sha256, p.snapshot_id, p.package_name, sf.suite_path, p.flat_filename
  FROM package_hash p
  JOIN suite_snapshot ss  ON ss.snapshot_id = p.snapshot_id
  JOIN suite_freshness sf ON sf.canonical_scheme    = ss.canonical_scheme
                         AND sf.canonical_host      = ss.canonical_host
                         AND sf.suite_path          = ss.suite_path
                         AND sf.current_snapshot_id = p.snapshot_id
 WHERE p.canonical_scheme = ?
   AND p.canonical_host   = ?
   AND p.flat_filename   <> ''
   AND p.flat_filename IN (?)
   AND ss.anchor_form    <> 'unsigned'`
	v10ReaderIntegrity = `
  SELECT p.declared_sha256 AS blob_hash,
         p.declared_sha256 AS declared_sha256,
         p.snapshot_id     AS snapshot_id,
         'package_hash'    AS source
    FROM package_hash p
    JOIN suite_freshness sf ON sf.current_snapshot_id = p.snapshot_id`
	v10ReaderCoverage = `
SELECT sf.current_snapshot_id, p.architecture, count(*) AS n,
  sum(CASE
    WHEN p.path GLOB '*/Packages.diff/*' OR p.path GLOB '*/Sources.diff/*' THEN 1
    ELSE 0
  END) AS pdiff
FROM suite_freshness sf
CROSS JOIN package_hash p
  ON sf.canonical_scheme = p.canonical_scheme
 AND sf.canonical_host   = p.canonical_host
 AND sf.current_snapshot_id = p.snapshot_id
GROUP BY sf.current_snapshot_id, p.architecture`
	v10ReaderCounts = `
SELECT p.canonical_host, p.architecture, count(*)
FROM package_hash p
JOIN suite_freshness sf
  ON sf.canonical_scheme = p.canonical_scheme
 AND sf.canonical_host   = p.canonical_host
 AND sf.current_snapshot_id = p.snapshot_id
WHERE p.architecture != ''
GROUP BY p.canonical_host, p.architecture`
	v10ReaderBlobs = `
SELECT canonical_host, architecture,
       count(*) AS blob_count,
       COALESCE(sum(size), 0) AS blob_bytes
FROM (
    SELECT DISTINCT p.canonical_host, p.architecture, b.hash, b.size
    FROM url_path u
    JOIN blob b ON b.hash = u.blob_hash
    CROSS JOIN package_hash p
      ON p.canonical_scheme = u.canonical_scheme
     AND p.canonical_host   = u.canonical_host
     AND p.path             = u.path
    JOIN suite_freshness sf
      ON sf.canonical_scheme = p.canonical_scheme
     AND sf.canonical_host   = p.canonical_host
     AND sf.current_snapshot_id = p.snapshot_id
    WHERE u.blob_hash IS NOT NULL AND p.architecture != ''
)
GROUP BY canonical_host, architecture`
	v10ReaderMirror = `
SELECT ss.suite_path, ph.package_name, ph.architecture, ph.version
  FROM package_hash ph
  JOIN suite_snapshot ss ON ss.snapshot_id = ph.snapshot_id
 WHERE ph.canonical_scheme = ? AND ph.canonical_host = ? AND ph.path = ? AND ph.declared_sha256 = ?
   AND ph.snapshot_id IN (SELECT current_snapshot_id FROM suite_freshness WHERE current_snapshot_id IS NOT NULL)`
	v10ReaderTopN = `
SELECT DISTINCT ph.version
  FROM package_hash ph
  JOIN suite_snapshot ss ON ss.snapshot_id = ph.snapshot_id
 WHERE ss.canonical_scheme = ? AND ss.canonical_host = ? AND ss.suite_path = ?
   AND ph.canonical_scheme = ? AND ph.canonical_host = ?
   AND ph.package_name = ? AND ph.architecture = ? AND ph.version <> ''
   AND ph.snapshot_id IN (SELECT current_snapshot_id FROM suite_freshness WHERE current_snapshot_id IS NOT NULL)`
)

// queryAnswer runs q and returns its rows as sorted text.
func queryAnswer(t *testing.T, db *sql.DB, q string, args ...any) string {
	t.Helper()
	rows, err := db.Query(q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer func() { _ = rows.Close() }()
	cols, _ := rows.Columns()
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
		parts := make([]string, len(vals))
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			parts[i] = fmt.Sprint(v)
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

type readerQuery struct {
	name     string
	old, new string
	args     []any
	newArgs  []any // nil: args
}

// The schema-11 migration changes no reader's answer (the final review's
// I-2). Random schema-10 databases of rows a released daemon could have
// written: http and https twins on two hosts, dists suites and a flat
// suite with its /. twin, inline and detached current snapshots, unsigned
// ones, signed ones without package rows, suites with none, displaced,
// retired and candidate snapshots with rows of their own, snapshots still
// queued for the legacy trust audit, and cached files clients asked for.
// Every reader's query as schema 10 ran it is answered before the
// migration, and the query production runs now is answered after it:
// vouching (exact and by flat name), the at-rest scan's package rows, the
// coverage and host-summary aggregates, the url-path GC's mirror match and
// its newest-N ranking. The answers must be the same, the lists exactly
// the signed current snapshots', and the database sound.
func TestMigrationV11_EveryReaderAnswersAsSchema10DidOnRandomDatabases(t *testing.T) {
	for seed := int64(1); seed <= 25; seed++ {
		rng := rand.New(rand.NewSource(seed))
		db, dir := openV10Cache(t)
		f := &v10{t: t, db: db}
		letters := "0123456789abcdef"
		sha := func() string { return strings.Repeat(string(letters[rng.Intn(4)]), 64) }

		type suite struct {
			key  SuiteKey
			flat bool
		}
		var suites []suite
		for _, scheme := range []string{"http", "https"} {
			for _, host := range []string{"h1", "h2"} {
				for _, sp := range []string{"/a/dists/s", "/a/dists/t", "/flat", "/flat/."} {
					if rng.Intn(4) == 0 {
						continue
					}
					k := SuiteKey{CanonicalScheme: scheme, CanonicalHost: host, SuitePath: sp}
					f.suite(k)
					suites = append(suites, suite{k, strings.HasPrefix(sp, "/flat")})
				}
			}
		}
		pool := []string{"/a/pool/x_1.deb", "/a/pool/x_2.deb", "/a/pool/y_1.deb", "/a/pool/z_1.deb", "/a/pool/s_1.dsc",
			"/a/dists/s/main/binary-amd64/Packages.diff/2026-01-01-0000.00.gz", "/a/dists/s/main/source/Sources.diff/2026-01-01-0000.00.gz"}
		flatPool := []string{"/flat/./f_1.deb", "/flat/./g_1.deb", "/flat/sub/./h_1.deb"}
		names := []string{"x", "y", "s", ""}
		arches := []string{"amd64", "arm64", "source", ""}
		versions := []string{"", "1", "2", "1:2~rc1+b1"}
		rowsFor := func(s suite, id int64) {
			p := pool
			if s.flat {
				p = flatPool
			}
			for _, path := range p {
				if rng.Intn(3) == 0 {
					continue
				}
				r := PackageHash{CanonicalScheme: s.key.CanonicalScheme, CanonicalHost: s.key.CanonicalHost, Path: path,
					DeclaredSHA256: sha(), PackageName: names[rng.Intn(len(names))], Architecture: arches[rng.Intn(len(arches))],
					Version: versions[rng.Intn(len(versions))]}
				if s.flat {
					r.FlatFilename = strings.Replace(strings.TrimPrefix(path, "/flat/"), "./", "", 1)
				}
				f.row(id, r)
			}
		}
		detached := func(k SuiteKey, state string) int64 {
			var adopted, retired any
			if state != "candidate" {
				adopted = 100 + f.n
			}
			if state == "retired" {
				retired = 500
			}
			res := f.exec(`INSERT INTO suite_snapshot
  (canonical_scheme, canonical_host, suite_path, release_hash, release_gpg_hash, anchor_form, created_at, adopted_at, retired_at, heartbeat_at)
VALUES (?, ?, ?, ?, ?, 'detached', 1, ?, ?, 1)`, k.CanonicalScheme, k.CanonicalHost, k.SuitePath, f.blob(), f.blob(), adopted, retired)
			id, _ := res.LastInsertId()
			if state == "current" {
				f.exec(`UPDATE suite_freshness SET current_snapshot_id = ?
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`, id, k.CanonicalScheme, k.CanonicalHost, k.SuitePath)
			}
			return id
		}
		var signedCurrent, unsignedCurrent int
		for _, s := range suites {
			for _, state := range []string{"retired", "displaced", "displaced"} {
				if rng.Intn(2) == 0 {
					rowsFor(s, f.snapshot(s.key, "inline", state))
				}
			}
			switch rng.Intn(6) {
			case 0: // never adopted, or quarantined: no current snapshot
			case 1: // an unsigned current snapshot: no rows (no writer makes any)
				f.snapshot(s.key, "unsigned", "current")
				unsignedCurrent++
			case 2:
				rowsFor(s, detached(s.key, "current"))
				signedCurrent++
			case 3: // a signed current snapshot without rows
				f.snapshot(s.key, "inline", "current")
				signedCurrent++
			default:
				id := f.snapshot(s.key, "inline", "current")
				rowsFor(s, id)
				signedCurrent++
				if rng.Intn(3) == 0 { // still queued for the legacy audit
					f.exec(`INSERT OR IGNORE INTO legacy_trust_audit (snapshot_id) VALUES (?)`, id)
				}
			}
			if rng.Intn(3) == 0 {
				rowsFor(s, f.snapshot(s.key, "inline", "candidate"))
			}
		}
		// Cached files clients asked for.
		type cached struct{ scheme, host, path, sha string }
		var files []cached
		for _, scheme := range []string{"http", "https"} {
			for _, host := range []string{"h1", "h2"} {
				for _, path := range append(append([]string{}, pool...), flatPool...) {
					if rng.Intn(2) == 0 {
						continue
					}
					h := sha()
					f.exec(`INSERT OR IGNORE INTO blob (hash, size, created_at) VALUES (?, 47, 0)`, h)
					f.exec(`INSERT INTO url_path (canonical_scheme, canonical_host, path, blob_hash, upstream_url,
                      is_metadata, last_requested_at, request_count, last_fetched_at)
VALUES (?, ?, ?, ?, 'http://up/x', 0, ?, 1, 1)`, scheme, host, path, h, 1000+rng.Intn(1000))
					files = append(files, cached{scheme, host, path, h})
				}
			}
		}

		var qs []readerQuery
		for _, scheme := range []string{"http", "https"} {
			for _, host := range []string{"h1", "h2"} {
				for _, path := range append(append([]string{}, pool...), flatPool...) {
					qs = append(qs, readerQuery{name: "exact " + scheme + host + path, old: v10ReaderExact, new: declaredExactSQL, args: []any{scheme, host, path}})
				}
				for _, name := range []string{"f_1.deb", "g_1.deb", "sub/h_1.deb", "h_1.deb"} {
					qs = append(qs, readerQuery{name: "suffix " + scheme + host + name, old: v10ReaderSuffix, new: declaredBySuffixSQL(1), args: []any{scheme, host, name}})
				}
			}
		}
		qs = append(qs,
			readerQuery{name: "integrity", old: v10ReaderIntegrity, new: integrityPackageRowsSQL},
			readerQuery{name: "coverage", old: v10ReaderCoverage, new: repoCoverageRowsSQL},
			readerQuery{name: "counts", old: v10ReaderCounts, new: cacheSummaryCountsSQL},
			readerQuery{name: "blobs", old: v10ReaderBlobs, new: cacheSummaryBlobsSQL})
		for _, c := range files {
			qs = append(qs, readerQuery{name: "mirror " + c.scheme + c.host + c.path, old: v10ReaderMirror, new: mirrorMatchSQL, args: []any{c.scheme, c.host, c.path, c.sha}})
		}
		for _, s := range suites {
			for _, name := range names {
				for _, arch := range arches {
					k := s.key
					qs = append(qs, readerQuery{name: "topN " + k.CanonicalScheme + k.CanonicalHost + k.SuitePath + " " + name + "/" + arch, old: v10ReaderTopN, new: topNVersionsSQL,
						args:    []any{k.CanonicalScheme, k.CanonicalHost, k.SuitePath, k.CanonicalScheme, k.CanonicalHost, name, arch},
						newArgs: []any{k.CanonicalScheme, k.CanonicalHost, k.SuitePath, name, arch}})
				}
			}
		}
		before := make([]string, len(qs))
		nonEmpty := 0
		for i, q := range qs {
			before[i] = queryAnswer(t, db, q.old, q.args...)
			if before[i] != "" {
				nonEmpty++
			}
		}
		var currentRows, allRows int
		if err := db.QueryRow(`SELECT (SELECT count(*) FROM package_hash p JOIN suite_freshness sf ON sf.current_snapshot_id = p.snapshot_id),
 (SELECT count(*) FROM package_hash)`).Scan(&currentRows, &allRows); err != nil {
			t.Fatal(err)
		}

		c := migrateV11(t, db, dir)
		diffs := 0
		for i, q := range qs {
			args := q.args
			if q.newArgs != nil {
				args = q.newArgs
			}
			if after := queryAnswer(t, c.db, q.new, args...); after != before[i] {
				diffs++
				t.Errorf("seed %d: %s\n before:\n%s\n after:\n%s", seed, q.name, before[i], after)
			}
		}
		var entries, lists, namedLists int
		if err := c.db.QueryRow(`SELECT (SELECT count(*) FROM package_list_entry), (SELECT count(*) FROM package_list),
 (SELECT count(*) FROM suite_freshness WHERE package_list_id IS NOT NULL)`).Scan(&entries, &lists, &namedLists); err != nil {
			t.Fatal(err)
		}
		if entries != currentRows || lists != signedCurrent || namedLists != signedCurrent {
			t.Errorf("seed %d: %d entries (current rows before: %d), %d lists, %d named (signed current suites: %d)",
				seed, entries, currentRows, lists, namedLists, signedCurrent)
		}
		if u := unnamedLists(t, c); len(u) != 0 {
			t.Errorf("seed %d: unnamed lists after the migration: %v", seed, u)
		}
		// The foreign keys still hold.
		if v := queryAnswer(t, c.db, `PRAGMA foreign_key_check`); v != "" {
			t.Errorf("seed %d: foreign_key_check:\n%s", seed, v)
		}
		if v := queryAnswer(t, c.db, `PRAGMA integrity_check`); v != "ok" {
			t.Errorf("seed %d: integrity_check:\n%s", seed, v)
		}
		if nonEmpty < 10 || signedCurrent == 0 {
			t.Errorf("seed %d: the fixture is too thin to mean anything: %d queries with an answer, %d signed current suites", seed, nonEmpty, signedCurrent)
		}
		if seed <= 3 {
			t.Logf("seed %d: %d suites (%d signed current, %d unsigned current), %d package_hash rows (%d current), %d queries (%d with an answer), %d differ",
				seed, len(suites), signedCurrent, unsignedCurrent, allRows, currentRows, len(qs), nonEmpty, diffs)
		}
	}
}
