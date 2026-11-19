# One Package List Per Suite Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An adoption writes the package rows that changed, not the suite's whole list, and the table holds current rows only.

**Architecture:** The rows move from `package_hash` (keyed by snapshot) to `package_list_entry` (keyed by a list id a suite names in `suite_freshness.package_list_id`). The adopter reads the live list before the flip barrier and hands `CommitAdoption` the difference; the flip transaction applies it when the list is still the one that was read, and otherwise writes a whole new list. A list no suite names is garbage, deleted by GC in chunks.

**Tech Stack:** Go, SQLite through `modernc.org/sqlite` (WAL, one writer goroutine), the repository's own test helpers; the Docker freshness lab for the timings.

**Spec:** `docs/superpowers/specs/2026-11-18-package-list-per-suite-design.md` (approved by the maintainer 2026-11-19). Read it first; this plan does not repeat its reasoning.

## Global Constraints

- Branch `perf/package-rows-per-suite`. Nothing is pushed. `main` is not touched.
- The checkout is shared with other sessions: run `git status --short` before every commit, add files by name (never `git add -A`: untracked review documents sit in `docs/`), and read diffs with `git diff --no-ext-diff`.
- `migrations` in `internal/cache/schema.go` is append-only: add entry 10 (schema 10 to 11), edit no earlier entry. Production SQL does not read SQLite's clock (`TestNoSQLiteClockFunctionsInProductionCode`).
- Every behaviour is pinned by a test that was seen to fail first. A test that cannot fail first (the model test) is proven by the mutants its task lists.
- Each task ends with `go test ./... -count=1` passing and `gofmt -l internal cmd` listing only files that were already listed on `main` (`internal/cache/cache_test.go`, `internal/cache/gc_urlpath_test.go`).
- `AIDEV-NOTE` comments: grep the file for existing ones before editing, update the ones the change makes wrong, remove none without replacing what they recorded, add one where the new code is trust-bearing or surprising.
- Every new log event or field gets its row in `docs/log-fields.md` in the task that adds it.
- Names that reach operators keep the words `package_hash` (the admin JSON's `package_hash_count` and `package_hash_rows`, `acu_package_hash_rows_by_kind`, the log events `package_hash_conflict`, `package_hash_mismatch`, `package_hash_miss`, the integrity scan's `source` value). They name a package's declared hash, which is still what a row is. Only the table, and what sub-job D counted, are renamed.
- Commit messages end with `Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>`.
- The Go type `cache.PackageHash` keeps every field. `SnapshotID` is no longer stored; writers ignore it and `GetPackageHash` fills it with the snapshot asked for.

## Measured before planning (2026-11-19, a copy of the June database, load average 3 to 7)

A scratch tool built the new tables on the copy (schema 4 then, 1,159,431 package rows, 292,712 of them current), twice per table shape:

| | Rowid table | `WITHOUT ROWID` |
|---|---|---|
| Copy of the current rows, indexes, drop of the old table (one transaction) | 9.9 s, 10.5 s; 169 MiB of WAL | 8.4 s, 10.6 s; 166 MiB |
| A whole new list of 66,928 rows | 2.1 s, 2.3 s; 67 MiB | 1.8 s, 2.2 s; 67 MiB |
| Deleting the old list, 10,000 rows a transaction | 1.5 s, 1.6 s; longest 0.27 s | 1.5 s, 2.0 s; longest 0.34 s |
| A difference of 241 rows | 0.03 s, 0.04 s; 2.9 MiB | 0.03 s; 2.9 MiB |
| A difference of 1% / 10% / 25% / 50% of the list | 0.1 / 0.6 to 0.8 / 1.1 to 1.5 / 2.1 s; 7 / 38 / 57 / 68 MiB | 0.1 / 0.5 to 0.7 / 0.9 to 1.0 / 1.5 s; 7 / 41 / 57 / 65 MiB |
| 20,000 lookups by path | 21 to 22 µs each | 27 µs each |
| Reading the live list in path order | 0.24 s | 0.17 s |
| Pages in use afterwards (656 MiB before) | 177 MiB | 174 MiB |

Decisions taken from it, both left to the builder by the spec:

- **`package_list_entry` is a rowid table.** The shapes are equal within the noise; the lookup, which every package request pays, was faster on the rowid table in every run; and it is the shape `package_hash` has today.
- **The rewrite path is taken when the difference is more than half the new list** (`diff.Size() > len(rows)/2`), as the spec proposed. At half, a difference costs what a rewrite costs (2.1 s and 68 MiB against 2.1 to 2.3 s and 67 MiB) and leaves no old list for GC.

The chunk delete used there is keyed by the primary key (`package_list_id = ? AND path IN (SELECT path … LIMIT ?)`); Task 3 uses that statement.

## Calls made in this plan (the spec left them open or did not foresee them)

1. `CommitAdoption` keeps its signature and always rewrites. A new `CommitAdoptionDiff` takes the difference; the adopter calls it. The spec's "a commit given no difference takes the rewrite path, so the test calls keep their meaning" is met without touching 127 call sites.
2. Reconcile records hot-package interest for the rows it inserts. Today the next adoption's join over the whole snapshot did that; on the difference path an unchanged row is not visited again.
3. The migration copies a current snapshot's rows whose scheme and host are the suite's own (the join `GetRepoCoverage` already uses), so it cannot fail on the new primary key. Every writer has always taken both from the suite (`buildPackageHashes`, `buildSourceHashes`, `buildPdiffHashes`).
4. The new triggers write the suite's own scheme and host into `hot_package`, not the entry's, so the row always satisfies its foreign key.
5. The mismatch ERROR and its counter are raised by the adopter from the commit's result; `internal/cache` has no metrics of its own.
6. An unsigned candidate's commit writes none of the rows it is given (the adopter builds none) and clears the suite's list.

## Review Focus

1. **A difference of more than 32,766 rows** (SQLite's bound-parameter limit): a 100,000-row list losing 40,000 paths must commit. Rows are written one prepared statement per row, never as an `IN` list. Test in Task 5.
2. **Paths whose byte order is easy to get wrong** (upper against lower case, `+`, `~`, non-ASCII): Go's string order and SQLite's `ORDER BY path` must agree, or the merge reports a list as different from itself. Test in Task 5.
3. **A restart with an unnamed list on disk** (the daemon was stopped between a rewrite and GC): after reopening, every reader answers from the live list only and one GC run removes the old one. Test in Task 3.
4. **A schema-10 database with odd rows:** a suite whose `current_snapshot_id` names a missing snapshot, a signed current snapshot with no package rows (it gets an empty list), a row whose host is not its suite's (not copied). The migration succeeds on all three. Test in Task 3.
5. **Two new rows with one path:** `DiffPackageList` and both commit paths fail with an error naming the path, and nothing is written. Tests in Tasks 2 and 5.

---

### Task 1: Tests reach package rows through helpers

No behaviour changes and the schema stays 10. The tests that read or seed `package_hash` by raw SQL for a suite's *current* snapshot move to helpers, so Task 3 changes two helper bodies instead of those tests, and the tests' meaning is proven unchanged against untouched production code.

**Files:**
- Modify: `internal/cache/testing_helpers.go`
- Modify (convert sites): `internal/cache/adoption_benchmark_test.go`, `flat_package_hash_test.go`, `flat_lookup_test.go`, `version_retention_fixes_test.go`, `gc_urlpath_test.go`, `cache_test.go` (the sites at the current schema only), `internal/freshness/unsigned_test.go`, `internal/freshness/flat_coverage_test.go`, `internal/admin/aggregate_refresh_test.go`
- Test: `internal/cache/testing_helpers_test.go` (create)

**Interfaces:**
- Produces:
  - `func (c *Cache) PackageRowsOfSnapshotForTest(ctx context.Context, snapshotID int64) ([]PackageHash, error)`: the package rows the snapshot holds, in path order.
  - `func (c *Cache) InsertPackageRowForTest(ctx context.Context, snapshotID int64, row PackageHash) error`: one row for the snapshot, through the writer, no validation beyond the schema's.
  - `GetPackageHash` and `DeletePackageHashForTest` keep their signatures.

- [ ] **Step 1: Write the failing test** in `testing_helpers_test.go`:

```go
func TestPackageRowHelpers_SeeTheCurrentSnapshotsRows(t *testing.T) {
	c := openCache(t)
	ctx := t.Context()
	id := adoptWithPackageRows(t, c, "/ubuntu/dists/jammy", []PackageHash{
		pkgRow("/ubuntu/pool/b.deb", "b"), pkgRow("/ubuntu/pool/a.deb", "a"),
	})
	if err := c.InsertPackageRowForTest(ctx, id, pkgRow("/ubuntu/pool/c.deb", "c")); err != nil {
		t.Fatal(err)
	}
	rows, err := c.PackageRowsOfSnapshotForTest(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, r := range rows {
		paths = append(paths, r.Path)
	}
	if want := []string{"/ubuntu/pool/a.deb", "/ubuntu/pool/b.deb", "/ubuntu/pool/c.deb"}; !slices.Equal(paths, want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
}
```

`adoptWithPackageRows` (create the suite, insert a signed candidate, `CommitAdoption` with the rows, return the snapshot id) and `pkgRow(path, hashLetter)` (a row for `http`/`archive.ubuntu.com` whose hash is the letter 64 times and whose package name is the file's base name) are new test helpers in the same file; build them from the fixtures `cache_test.go` already uses for a signed adoption.

- [ ] **Step 2: Run it.** `go test ./internal/cache/ -run TestPackageRowHelpers -count=1`. Expected: build failure, the two methods are undefined.

- [ ] **Step 3: Implement the two helpers** in `testing_helpers.go` on `package_hash` (`WHERE snapshot_id = ? ORDER BY path`; the insert through `submitWrite(ctx, writeTest, …)`).

- [ ] **Step 4: Convert the sites.** Rule: convert a raw-SQL site only when the snapshot it names is a suite's current snapshot at that point in the test. A count becomes `len` of `PackageRowsOfSnapshotForTest`; a single-column read becomes `GetPackageHash`; a raw `INSERT INTO package_hash` becomes `InsertPackageRowForTest`. Leave untouched: tests that run at an old schema version (`openV1Cache` to `openV4Cache`, `applyMigration`), `displaced_package_hash_test.go`, `package_hash_bench_test.go`, `coverage_group_benchmark_test.go`, `admin_queries_benchmark_test.go`, `admin_queries_test.go`, `hot_packages_test.go`, `snapshot_gc_retired_test.go`, `internal/gc/issue14_longrun_test.go`, and any site about a displaced, retired or candidate snapshot. Task 3 handles those.

- [ ] **Step 5: Verify.** `go test ./... -count=1`: all pass. `grep -rn "package_hash" --include='*_test.go' internal cmd | grep -c "FROM package_hash\|INTO package_hash"` is lower than before; note the remaining files for Task 3.

- [ ] **Step 6: Commit** `test(cache): reach package rows through helpers`.

---

### Task 2: The difference between two package lists

A pure function: no database, no schema.

**Files:**
- Create: `internal/cache/package_list.go`, `internal/cache/package_list_test.go`

**Interfaces:**
- Produces:

```go
// PackageListDiff is what turns a suite's live package list into an
// adoption's new rows.
type PackageListDiff struct {
	ListID     int64         // the live list the difference was computed against
	Generation int64         // suite_freshness.package_list_generation at that read
	Removed    []string      // paths the live list has and the new rows do not
	Changed    []PackageHash // new rows whose path the live list has with another column different
	Added      []PackageHash // new rows whose path the live list does not have
	TooLarge   bool          // more than limit rows differ: the three slices are nil
}

func (d *PackageListDiff) Size() int // len(Removed)+len(Changed)+len(Added)

var ErrDuplicatePackagePath = errors.New("cache: two package rows share a path")

// diffPackageRows merges the live list, which next yields in ascending
// path order (ok=false at its end), with rows, in any order.
func diffPackageRows(next func() (row PackageHash, ok bool, err error),
	rows []PackageHash, limit int) (PackageListDiff, error)
```

  A row is *changed* when any of `CanonicalScheme`, `CanonicalHost`, `DeclaredSHA256`, `PackageName`, `Architecture`, `Version`, `FlatFilename` differs; `SnapshotID` is not compared. `rows` is not reordered (sort a slice of indexes). The live list is never held in memory. Two rows of `rows` with one path: `ErrDuplicatePackagePath` wrapping the path. A live list out of order or repeating a path: an error (it cannot happen under the primary key; it is the merge's precondition). Once more than `limit` rows differ, return at once with `TooLarge` set and the slices nil; `limit < 0` means no limit.

- [ ] **Step 1: Write the failing tests:**
  - `TestDiffPackageRows_Cases`: a table over (old, new) pairs: both empty; old empty; new empty; identical; one hash changed; one package name changed with the hash kept; one path removed and one added; `SnapshotID` differing only (no difference).
  - `TestDiffPackageRows_ApplyingItGivesTheNewRows`: 500 seeded rounds; each builds a random old list (0 to 300 rows from a pool of 400 paths that includes `/A`, `/a`, `/a+b`, `/a~b`, `/é`) and a random new one, computes the difference with no limit, applies it to a map of the old list, and requires the map to equal the new rows exactly. The seed is in the failure message.
  - `TestDiffPackageRows_ADuplicatePathInTheNewRowsIsAnError`: `errors.Is(err, ErrDuplicatePackagePath)` and the message holds the path.
  - `TestDiffPackageRows_StopsOnceTheDifferenceIsOverTheLimit`: 100 rows all changed, limit 10: `TooLarge`, nil slices, and `next` was called at most 12 times.
  - `TestDiffPackageRows_DoesNotReorderItsInput`: the caller's slice is element for element as passed.

- [ ] **Step 2: Run them.** `go test ./internal/cache/ -run TestDiffPackageRows -count=1`. Expected: build failure.

- [ ] **Step 3: Implement** `PackageListDiff`, `Size`, `ErrDuplicatePackagePath`, `diffPackageRows`. One merge loop over the sorted index and `next`.

- [ ] **Step 4: Run them:** pass. `go test ./... -count=1`: pass.

- [ ] **Step 5: Commit** `feat(cache): the difference between two package lists`.

---

### Task 3: Schema 11: the rows move to package lists

The switch. After it every adoption still writes its whole list (the rewrite path only), now as a new list, and GC deletes the old one. Nothing reads or writes `package_hash`.

**Files:**
- Modify: `internal/cache/schema.go` (entry 10, `CurrentSchemaVersion = 11`), `adoption.go` (commit step 3, step 6, the three reader queries, `ListIntegrityCandidates`, `GetPackageHash`, the comments), `hot_packages.go`, `reconcile.go`, `queries.go` (`GetRepoCoverage`, both queries of `GetCacheSummaryByHostArch`), `gc_urlpath.go` (`urlPathRetainedTx`, `topNVersionSetTx`), `gc.go` (sub-jobs A, C, D), `legacy_audit.go` (`quarantineSnapshotTx`), `writer_wait.go` (the write kind), `cache.go` and `types.go` (comments), `testing_helpers.go` (helper bodies)
- Modify: `internal/gc/gc.go` (the store interface's method name), `internal/gc/snapshot.go` (the one call), `internal/gc/gc_store_test.go`, `internal/gc/subjob_d_test.go` (the fake store's method)
- Modify: `docs/log-fields.md` (the write-kind row)
- Create: `internal/cache/schema_v11_test.go`, `internal/cache/package_list_gc_test.go`, `internal/cache/package_list_readers_test.go`
- Delete: `internal/cache/displaced_package_hash_test.go`, `internal/cache/package_hash_bench_test.go` (both are about the table that goes; the first's reader pins move to `package_list_readers_test.go`)
- Modify (the sites Task 1 left): `admin_queries_test.go`, `admin_queries_benchmark_test.go`, `coverage_group_benchmark_test.go`, `hot_packages_test.go`, `snapshot_gc_retired_test.go`, `schema_v6_test.go`, `flat_lookup_test.go` (index names), `internal/gc/issue14_longrun_test.go`, and whatever else `go test ./...` shows

**Interfaces:**
- Consumes: Task 1's helpers.
- Produces:
  - `func (c *Cache) RunPackageListGCChunk(ctx context.Context, maxRows int) (rows int, more bool, err error)`: replaces `RunDisplacedPackageHashChunk`, same contract shape. One writer transaction: the lowest-id list no suite names; up to `maxRows` of its entries deleted; when the delete comes up short, its header too. `rows` counts entries; `more` is false only when no unnamed list existed.
  - `writeGCPackageLists`, named `gc_package_lists`: replaces `writeGCDisplacedPackages`.
  - `func recordListHotPackages(ctx context.Context, tx *sql.Tx, listID int64, key SuiteKey, paths []string) error`: replaces `recordSnapshotHotPackages`. `paths == nil`: every entry of the list, in one statement; otherwise one prepared statement run per path. The `hot_package` row takes its scheme, host and suite path from `key`.
  - `func insertPackageHashTx(ctx context.Context, tx *sql.Tx, listID int64, ph PackageHash) (wrote bool, err error)`: probe by `(package_list_id, path)`, the same agree / heal / conflict rules, `wrote` true for an insert or a heal.
  - `func (c *Cache) InsertUnnamedPackageListForTest(ctx context.Context, rows []PackageHash) (listID int64, err error)`: a list no suite names, for the tests that used a displaced snapshot's rows.
  - `CommitAdoption`: signature unchanged. A signed candidate always leaves its suite naming a list (empty when there are no rows); an unsigned one leaves `package_list_id` NULL. Either bumps `package_list_generation`.

**The migration** (entry 10 of `migrations`, exactly this; it is not editable once merged):

```sql
CREATE TABLE package_list (
  package_list_id INTEGER PRIMARY KEY AUTOINCREMENT
);
CREATE TABLE package_list_entry (
  package_list_id  INTEGER NOT NULL REFERENCES package_list(package_list_id),
  canonical_scheme TEXT NOT NULL,
  canonical_host   TEXT NOT NULL,
  path             TEXT NOT NULL,
  declared_sha256  TEXT NOT NULL
                     CHECK (length(declared_sha256) = 64
                            AND declared_sha256 NOT GLOB '*[^0-9a-f]*'),
  package_name     TEXT NOT NULL DEFAULT '',
  architecture     TEXT NOT NULL DEFAULT '',
  version          TEXT NOT NULL DEFAULT '',
  flat_filename    TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (package_list_id, path)
);
ALTER TABLE suite_freshness ADD COLUMN package_list_id INTEGER
  REFERENCES package_list(package_list_id);
ALTER TABLE suite_freshness ADD COLUMN package_list_generation INTEGER NOT NULL DEFAULT 0;
INSERT INTO package_list (package_list_id)
SELECT sf.current_snapshot_id
  FROM suite_freshness sf
  JOIN suite_snapshot ss ON ss.snapshot_id      = sf.current_snapshot_id
                        AND ss.canonical_scheme = sf.canonical_scheme
                        AND ss.canonical_host   = sf.canonical_host
                        AND ss.suite_path       = sf.suite_path
 WHERE ss.anchor_form <> 'unsigned';
UPDATE suite_freshness SET package_list_id = current_snapshot_id
 WHERE current_snapshot_id IN (SELECT package_list_id FROM package_list);
CREATE UNIQUE INDEX idx_suite_freshness_package_list
  ON suite_freshness(package_list_id) WHERE package_list_id IS NOT NULL;
INSERT INTO package_list_entry
  (package_list_id, canonical_scheme, canonical_host, path, declared_sha256,
   package_name, architecture, version, flat_filename)
SELECT sf.package_list_id, p.canonical_scheme, p.canonical_host, p.path, p.declared_sha256,
       p.package_name, p.architecture, p.version, p.flat_filename
  FROM suite_freshness sf
 CROSS JOIN package_hash p
    ON p.canonical_scheme = sf.canonical_scheme
   AND p.canonical_host   = sf.canonical_host
   AND p.snapshot_id      = sf.package_list_id
 ORDER BY sf.package_list_id, p.path;
CREATE INDEX idx_package_list_entry_path
  ON package_list_entry(canonical_scheme, canonical_host, path);
CREATE INDEX idx_package_list_entry_pkg
  ON package_list_entry(package_list_id, package_name, architecture);
CREATE INDEX idx_package_list_entry_flat
  ON package_list_entry(canonical_scheme, canonical_host, flat_filename)
  WHERE flat_filename <> '';
DROP TRIGGER hot_package_insert;
DROP TRIGGER hot_package_touch;
CREATE TRIGGER hot_package_insert AFTER INSERT ON url_path
 WHEN NEW.last_requested_at IS NOT NULL
 BEGIN
INSERT INTO hot_package (canonical_scheme, canonical_host, suite_path, package_name, architecture, last_requested_at)
SELECT sf.canonical_scheme, sf.canonical_host, sf.suite_path,
       e.package_name, e.architecture, MAX(u.last_requested_at)
  FROM url_path u JOIN package_list_entry e
    ON e.canonical_scheme = u.canonical_scheme AND e.canonical_host = u.canonical_host
   AND e.path = u.path AND e.declared_sha256 = u.blob_hash
  JOIN suite_freshness sf ON sf.package_list_id = e.package_list_id
 WHERE u.last_requested_at IS NOT NULL AND e.package_name <> '' AND e.architecture <> ''
   AND u.canonical_scheme = NEW.canonical_scheme AND u.canonical_host = NEW.canonical_host AND u.path = NEW.path
 GROUP BY sf.canonical_scheme, sf.canonical_host, sf.suite_path, e.package_name, e.architecture
 ON CONFLICT(canonical_scheme, canonical_host, suite_path, package_name, architecture)
 DO UPDATE SET last_requested_at = MAX(hot_package.last_requested_at, excluded.last_requested_at);
 END;
CREATE TRIGGER hot_package_touch AFTER UPDATE OF last_requested_at ON url_path
 WHEN NEW.last_requested_at IS NOT NULL
 BEGIN
  -- the same INSERT … SELECT as hot_package_insert, verbatim
 END;
DROP TABLE package_hash;
```

(`hot_package_touch` carries the full statement in the file; SQL comments are not allowed to stand in for it.) The list id of a migrated suite is its current snapshot's id, as the spec says; `AUTOINCREMENT` continues above the highest.

**The readers** (the spec's table). Each becomes "entries whose list a suite names", keeping today's column lists and Go scanning:

| Reader | New `FROM` and joins |
|---|---|
| `declaredExactSQL`, `declaredBySuffixSQL` | `package_list_entry e JOIN suite_freshness sf ON sf.package_list_id = e.package_list_id JOIN suite_snapshot ss ON ss.snapshot_id = sf.current_snapshot_id`; `WHERE` as today on `e.`, plus `ss.anchor_form <> 'unsigned'`; the snapshot id selected is `sf.current_snapshot_id` |
| `flatRowsExistSQL` | `SELECT EXISTS(SELECT 1 FROM package_list_entry WHERE flat_filename <> '')` (live and unnamed lists alike: ruling PG-31) |
| `ListIntegrityCandidates`, second arm | `package_list_entry e JOIN suite_freshness sf ON sf.package_list_id = e.package_list_id`; `source` stays `'package_hash'`; the snapshot id is `sf.current_snapshot_id` |
| `GetRepoCoverage` aggregate | `suite_freshness sf CROSS JOIN package_list_entry e ON e.package_list_id = sf.package_list_id`, grouped as today |
| `GetCacheSummaryByHostArch` counts | the same join, `WHERE e.architecture != ''` |
| `GetCacheSummaryByHostArch` blobs | `url_path u JOIN blob b … CROSS JOIN package_list_entry e ON e.canonical_scheme = u.canonical_scheme AND e.canonical_host = u.canonical_host AND e.path = u.path JOIN suite_freshness sf ON sf.package_list_id = e.package_list_id` (`url_path` stays outside: the comment there says why) |
| `urlPathRetainedTx` mirror match | `package_list_entry e JOIN suite_freshness sf ON sf.package_list_id = e.package_list_id WHERE e.canonical_scheme = ? AND e.canonical_host = ? AND e.path = ? AND e.declared_sha256 = ?`; the suite path is `sf.suite_path` |
| `topNVersionSetTx` | `suite_freshness sf JOIN package_list_entry e ON e.package_list_id = sf.package_list_id WHERE sf.canonical_scheme = ? AND sf.canonical_host = ? AND sf.suite_path = ? AND e.package_name = ? AND e.architecture = ? AND e.version <> ''` |
| `GetPackageHash`, `DeletePackageHashForTest`, Task 1's helpers | the entry in the list of the suite whose `current_snapshot_id` is the snapshot asked for (none for any other snapshot) |

**The writers:**
- `CommitAdoption` step 3: read the suite's row (`package_list_id`, `package_list_generation`) by the candidate's key; no row is `ErrSuiteGone` in step 6's wording. Signed: `INSERT INTO package_list DEFAULT VALUES`, every row into the new list with today's hash validation, `recordListHotPackages(tx, newList, key, nil)`. Unsigned: no row is written. Step 6's `UPDATE` also sets `package_list_id` (the new list, or NULL) and `package_list_generation = package_list_generation + 1`.
- `InsertReconciledMembers`: the current-snapshot guard also reads the suite's key and `package_list_id`. Rows to insert and a NULL list: an error (`suite has no package list`), nothing written. Otherwise `insertPackageHashTx` per row into that list; when any wrote, bump the generation and `recordListHotPackages` over the paths written.
- `quarantineSnapshotTx`: where it clears `current_snapshot_id`, also `package_list_id = NULL, package_list_generation = package_list_generation + 1`.
- GC: sub-job A's cascade loses its `package_hash` statement; sub-job C's table loop is `snapshot_member`, `snapshot_skipped_member`; sub-job D is `RunPackageListGCChunk` with

```sql
SELECT pl.package_list_id FROM package_list pl
 WHERE NOT EXISTS (SELECT 1 FROM suite_freshness sf WHERE sf.package_list_id = pl.package_list_id)
 ORDER BY pl.package_list_id LIMIT 1;
DELETE FROM package_list_entry WHERE package_list_id = ?1 AND path IN
  (SELECT path FROM package_list_entry WHERE package_list_id = ?1 LIMIT ?2);
DELETE FROM package_list WHERE package_list_id = ?1;  -- only when the delete above came up short
```

**Comments to correct** (the spec's "The plan corrects both"): the audit note above the old `RunDisplacedPackageHashChunk` and the note at `declaredExactSQL` are rewritten for lists: who reads entries, that a list no suite names is invisible to every reader but `flatRowsExistSQL`, and that `ComputeHotSet` does not read the table. `Cache.flatRows`' note names the new writers. Sub-job C's and `retiredChunkRows`' notes keep their measurements and say which table they were taken on.

- [ ] **Step 1: Write the failing tests.**

  `schema_v11_test.go`:
  - `TestMigrationV11_EveryReaderAnswersAsBefore`: a helper `openV10Cache(t)` (as `openV4Cache` does, `applyMigration` for 0 through 9) builds, by raw SQL at schema 10: suite A (signed) with a current, a displaced, a retired and a candidate snapshot, each with its own package rows, one path with a different hash in each; suite B (signed, flat) whose rows have `flat_filename`; suite C (unsigned current snapshot) with a package row; `url_path` rows with `last_requested_at` for one path of A's current and one of A's displaced snapshot. It records, through SQL written in the test against `package_hash`, the expected answer of each reader (the current signed rows). Then `applyMigration(ctx, db, 10)`, open a `Cache` on the directory, and require: `DeclaredHashesForPath` for a path of A (the current hash, A's current snapshot id), for B's flat request path, for C's path (none), for the displaced-only path (none); `ListIntegrityCandidates`; `GetRepoCoverage`; `GetCacheSummaryByHostArch`; `PackageRowsOfSnapshotForTest` of A's current snapshot equal to its schema-10 rows and empty for the displaced one; `package_list_id` of A equal to A's current snapshot id, NULL for C; no table `package_hash`.
  - `TestMigrationV11_ASecondOpenChangesNothing`: open, close, open: schema version 11, the same rows.
  - `TestMigrationV11_TheTriggersRecordInterestAgainstTheLists`: after the migration, `TouchURLPath` (or the insert path) on a path A's live list declares adds or advances A's `hot_package` row; on a path only an unnamed list declares, nothing.
  - `TestMigrationV11_OddRows` (Review Focus 4): a suite whose `current_snapshot_id` names no snapshot keeps a NULL list; a signed current snapshot without rows gets a list with no entries; a row of a current snapshot whose host is not its suite's is not copied; the migration succeeds.
  - `TestMigrationV11_AListIdNeverCollidesWithAMigratedOne`: after migrating a suite whose current snapshot id is 7, a `CommitAdoption` on another suite gets a list id above 7.

  `package_list_readers_test.go`:
  - `TestPackageListReaders_AnUnnamedListIsInvisible`: a suite with a live list, plus `InsertUnnamedPackageListForTest` rows for the same host with other hashes and other paths: every reader of the table above answers exactly as without the unnamed list; `flatRowsExistSQL` alone sees an unnamed flat row.
  - `TestPackageListReaders_QueryPlans`: `queryPlan` of `declaredExactSQL` contains `USING INDEX idx_package_list_entry_path` and `USING INDEX idx_suite_freshness_package_list` and no `SCAN sf`; of `declaredBySuffixSQL(3)` contains `idx_package_list_entry_flat`; of `flatRowsExistSQL` contains `idx_package_list_entry_flat`; of the `topNVersionSetTx` query contains `idx_package_list_entry_pkg`; of the trigger's `SELECT` (the text with `NEW.` replaced by parameters) contains both indexes of the first.
  - `TestPackageListReaders_AfterARestartTheOldListIsStillInvisible` (Review Focus 3): adopt, adopt again (a rewrite), close and reopen the cache: `DeclaredHashesForPath` answers the second adoption's hash only; then `RunPackageListGCChunk` until `more` is false leaves no unnamed list and the live answers unchanged.

  `package_list_gc_test.go`:
  - `TestPackageListGC_DeletesAnUnnamedListInChunks`: an unnamed list of 25 rows, `maxRows` 10: calls return (10, true), (10, true), (5, true), (0, false); the header is gone after the third; a named list beside it is untouched throughout.
  - `TestPackageListGC_LeavesEveryNamedList`: three suites, no unnamed list: (0, false) and every row still there.
  - `TestPackageListGC_TakesTheListsOfARemovedAndOfAQuarantinedSuite`: `ForgetSuite` on one suite and `ApplyLegacyAuditDecisions` quarantining another's current snapshot; both lists go, a third suite's stays.
  - `TestPackageListGC_DefaultChunkIsTenThousand`: `effectiveChunkRows(0) == 10_000` (the existing pin, kept).

  In existing files: `TestCommitAdoption_ASignedCommitNamesANewListAndAnUnsignedOneNone` (generation bumped in both); `TestInsertReconciledMembers_AddsToTheLiveListAndBumpsTheGeneration`; `TestInsertReconciledMembers_RefusesRowsForASuiteWithoutAList`; `TestInsertReconciledMembers_RecordsInterestForTheRowsItInserts` (a `url_path` row requested before the reconcile gets its `hot_package` row from the reconcile); `TestQuarantine_ClearsTheSuitesList`.

- [ ] **Step 2: Run them.** `go test ./internal/cache/ -run 'MigrationV11|PackageListReaders|PackageListGC|ASignedCommitNames|InsertReconciledMembers_|Quarantine_ClearsTheSuitesList' -count=1`. Expected: build failure (`RunPackageListGCChunk`, `InsertUnnamedPackageListForTest`), then, with stubs, failures on "no such table: package_list".

- [ ] **Step 3: Implement,** in this order so the compiler leads: the migration and `CurrentSchemaVersion`; `CommitAdoption`; `recordListHotPackages`; the readers; reconcile; quarantine; GC; the write kind and its `docs/log-fields.md` row; the helpers; `internal/gc`'s method name.

- [ ] **Step 4: Move the remaining tests.** Sites about a displaced snapshot's rows become sites about an unnamed list (`InsertUnnamedPackageListForTest`) where they pinned "not current is not counted", and are deleted where they pinned sub-job D's stripping of a displaced snapshot (the new GC tests replace them). `schema_v6_test.go` reads `version` from `package_list_entry`. Index names in plan assertions follow the table above.

- [ ] **Step 5: Verify.** `go test ./... -count=1` passes. `grep -rn "package_hash" --include='*.go' internal cmd | grep -v _test.go | grep -i "from \|into \|update \|join \|delete "` shows only `schema.go` entries 0 to 10. `go test -race ./internal/cache/ ./internal/gc/ -count=1` passes.

- [ ] **Step 6: Commit** `feat(cache)!: package rows live in one list per suite (schema 11)`.

---

### Task 4: Sub-job D's names follow what it counts

**Files:**
- Modify: `internal/gc/gc.go`, `internal/gc/snapshot.go`, `internal/gc/metrics.go`, `internal/admin/status.go`, `internal/admin/status_template.go`, `internal/admin/testdata/*.json` (the three golden files), `internal/config/config.go` (the `gc` comments), `docs/log-fields.md`, `docs/admin-ui-spec.md`, `docs/configuration.md`
- Test: the existing tests of those names (`internal/gc/subjob_d_test.go`, the admin JSON contract tests)

**Interfaces:**
- Produces (old name, then new):
  - metric `acu_gc_displaced_package_rows_deleted_total` → `acu_gc_package_list_rows_deleted_total`, help text "Package rows of lists no suite names any more (a rewritten list, a removed or quarantined suite), deleted by snapshot-GC sub-job D."
  - log field `displaced_package_rows_deleted` → `package_list_rows_deleted` (on `gc_run_complete` and the retired run's line)
  - admin JSON `gc.last_run_displaced_package_rows_deleted` → `gc.last_run_package_list_rows_deleted`; the status page's label "Package-list rows deleted"
  - Go: `DisplacedPackageRowsDeleted` → `PackageListRowsDeleted`, `displacedPackageRowsDeleted` → `packageListRowsDeleted`, `packageRowsStripped` → `packageListRowsDeleted`, error prefix `displaced package_hash gc chunk:` → `package list gc chunk:`

- [ ] **Step 1: Change the tests' expectations** to the new names (the metric name in the metrics test, the log field in `subjob_d_test.go`, the golden files, `gc_tick_failed`'s documented prefixes).
- [ ] **Step 2: Run** `go test ./internal/gc/ ./internal/admin/ -count=1`. Expected: failures naming the old strings.
- [ ] **Step 3: Rename** in the code; rewrite the comments in `internal/gc/snapshot.go` and `gc.go` that describe D (what it deletes now, that an ordinary adoption leaves it nothing) and `config.go`'s `keep_displaced` comment (a kept displaced snapshot has no package rows to strip).
- [ ] **Step 4: Docs.** `docs/log-fields.md` (the GC section and `gc_tick_failed`), `docs/admin-ui-spec.md` (the field's row), `docs/configuration.md` (`keep_displaced`).
- [ ] **Step 5: Verify.** `go test ./... -count=1` passes; `grep -rn "displaced_package\|DisplacedPackage\|displacedPackage" internal cmd docs/log-fields.md docs/admin-ui-spec.md docs/configuration.md` finds nothing.
- [ ] **Step 6: Commit** `refactor(gc): name sub-job D for the package lists it deletes`.

---

### Task 5: The commit applies a difference

**Files:**
- Modify: `internal/cache/package_list.go` (`DiffPackageList`, `PackageListWrite`), `internal/cache/adoption.go` (`CommitAdoptionDiff`; `CommitAdoption` calls it with nil)
- Test: `internal/cache/package_list_commit_test.go` (create)

**Interfaces:**
- Consumes: Task 2's `diffPackageRows`, Task 3's lists and `recordListHotPackages`.
- Produces:

```go
const (
	PackageListDifference = "difference" // the live list was changed in place
	PackageListRewrite    = "rewrite"    // a new list was written whole
	PackageListNone       = "none"       // an unsigned commit: the suite names no list
)

// PackageListWrite says how a commit wrote its suite's package list.
type PackageListWrite struct {
	Mode   string
	Reason string // for a rewrite: "first", "no_difference", "large", "list_changed", "mismatch"
	Added, Changed, Removed int // rows inserted, updated, deleted; a rewrite counts every row as Added
	Expected, Found         int // a mismatch's row counts; zero otherwise
}

// DiffPackageList reads key's live package list on a reader connection,
// in one read transaction, and returns what turns it into rows.
// nil, nil: the suite names no list (or has no row).
func (c *Cache) DiffPackageList(ctx context.Context, key SuiteKey, rows []PackageHash) (*PackageListDiff, error)

func (c *Cache) CommitAdoptionDiff(ctx context.Context, snapshotID int64,
	members []SnapshotMember, skipped []SkippedMember, packageHashes []PackageHash,
	prefetchedURLPaths []PrefetchedURLPath, coverageComplete bool,
	diff *PackageListDiff) (PackageListWrite, error)
```

  `DiffPackageList` passes `len(rows)/2` as the limit. In the commit, for a signed candidate:
  1. The suite names no list: rewrite, reason `first`.
  2. `diff == nil`: rewrite, `no_difference`. `diff.TooLarge` or `diff.Size() > len(packageHashes)/2`: rewrite, `large`. `diff.ListID` or `diff.Generation` is not the suite's: rewrite, `list_changed`.
  3. Otherwise `SAVEPOINT package_list`; delete each `Removed` path, update each `Changed` row (every column but the key), insert each `Added` row, each by one prepared statement per row, with the hash validation; `recordListHotPackages` over the changed and added paths; then count the list's entries. The count equals `len(packageHashes)`: `RELEASE`, mode `difference`. It does not: `ROLLBACK TO package_list`, `RELEASE`, then the rewrite, reason `mismatch`, `Expected` and `Found` set.

  The writer operation stores its result in a local the method returns after `submitWrite` (the rule at `InsertCandidateSnapshot`: never the named results).

- [ ] **Step 1: Write the failing tests** in `package_list_commit_test.go`. A helper `publish(t, c, key, rows) (snapshotID int64, w PackageListWrite)` inserts a signed candidate with a fresh anchor hash, calls `DiffPackageList` and `CommitAdoptionDiff`; `liveRows(t, c, key)` returns the suite's list in path order.
  - `TestCommitDiff_ASmallChangeIsWrittenInPlace`: 1,000 rows, then a publication with 3 removed, 4 changed (two a hash, two a version only), 5 added: mode `difference`, counts 5/4/3, the live rows equal the new rows exactly, `package_list_id` unchanged, the generation one higher, no unnamed list exists.
  - `TestCommitDiff_AnIdenticalListWritesNoRow`: mode `difference`, counts zero, the list's rowids unchanged (read them before and after).
  - `TestCommitDiff_TheFirstAdoptionRewrites`: reason `first`.
  - `TestCommitDiff_MoreThanHalfRewrites`: 100 rows, 51 changed: mode `rewrite`, reason `large`, a new list id, the old list unnamed. And 50 changed: mode `difference`.
  - `TestCommitDiff_AListThatChangedSinceTheReadIsRewritten`: compute the difference, then `InsertReconciledMembers` adds a row, then commit with the stale difference: `rewrite`, `list_changed`, the live rows equal the new rows exactly (the reconciled row is gone unless the new rows have it). The same with a second adoption between the read and the commit.
  - `TestCommitDiff_AWrongDifferenceIsCaughtAndTheListRewritten`: drop one path from `diff.Removed` by hand: the commit succeeds, mode `rewrite`, reason `mismatch`, `Expected` and `Found` one apart, the live rows exact, the snapshot current.
  - `TestCommitDiff_AFailingCommitLeavesTheListAsItWas`: a member with a malformed blob hash beside a valid difference: the commit fails, the live rows and the generation are the previous snapshot's.
  - `TestCommitDiff_AnUnsignedCommitClearsTheList`: mode `none`; the previous list is unnamed.
  - `TestCommitDiff_FortyThousandRemovedPathsCommit` (Review Focus 1): 100,000 rows, a publication without 40,000 of them: mode `difference`, 60,000 live rows. Skipped under `-short`.
  - `TestDiffPackageList_AgreesWithSQLitesOrder` (Review Focus 2): a list whose paths include `/A`, `/a`, `/a+b`, `/a~b`, `/a.b`, `/é`, `/z`: the difference against the same rows is empty.
  - `TestDiffPackageList_TwoRowsWithOnePathFail` and `TestCommitDiff_TwoRowsWithOnePathFail` (Review Focus 5): `ErrDuplicatePackagePath` from the first; from a commit given such rows with a nil difference, an error and the previous list intact.
  - `TestCommitDiff_RecordsInterestForChangedAndNewRows`: a `url_path` row requested and holding the new hash, for a path the publication changes and for one it adds: both `hot_package` rows exist after the commit; a `hot_package` row of an unchanged entry is not rewritten (its `last_requested_at` set to a marker value by hand survives).
  - `TestCommitAdoption_WithoutADifferenceStillRewrites`: the plain `CommitAdoption` on a suite with a live list gives a new list id.

- [ ] **Step 2: Run them.** `go test ./internal/cache/ -run 'TestCommitDiff|TestDiffPackageList|WithoutADifferenceStillRewrites' -count=1`. Expected: build failure.

- [ ] **Step 3: Implement** `DiffPackageList` (one `BeginTx` read-only on `c.db`; the suite's list id and generation, then the entries `ORDER BY path` streamed into `diffPackageRows`), `PackageListWrite`, `CommitAdoptionDiff`. Add an `AIDEV-NOTE` at the guard: the generation, bumped by every writer of a list in its own transaction, is what makes "the difference was computed against this list" true at commit time; the suite's mutex is the reason it normally holds, not the proof.

- [ ] **Step 4: Run them:** pass. `go test ./... -count=1` and `go test -race ./internal/cache/ -count=1` pass.

- [ ] **Step 5: Commit** `feat(cache): a commit writes the difference to its suite's package list`.

---

### Task 6: A model test of the lists

The spec's proof. It cannot fail before the code exists, so it is proven by mutants.

**Files:**
- Create: `internal/cache/package_list_model_test.go`

**Interfaces:**
- Consumes: everything Tasks 3 and 5 produce.

- [ ] **Step 1: Write the test** `TestPackageLists_AModelOfTheSwitch`: for each of 40 seeds (the seed is in every failure message; `ACU_MODEL_SEED` runs one seed, `ACU_MODEL_STEPS` overrides the 300 steps), four suites on two hosts that share package paths, and a Go model of each suite: its current snapshot's rows, or none. Each step picks one operation at random:
  - a publication through `DiffPackageList` and `CommitAdoptionDiff`: a first adoption, a small difference (0 to 5 rows), a large one (over half), an identical list, an empty list;
  - a publication whose difference is made stale before the commit (a reconcile or another publication in between);
  - a publication through plain `CommitAdoption`;
  - an unsigned publication (only for a suite with no signed history: a fresh suite);
  - a commit that fails (a malformed member hash);
  - a reconcile adding 1 to 3 rows (the model adds them);
  - `ForgetSuite`, and later the suite's creation again;
  - a quarantine of a suite's current snapshot (`ApplyLegacyAuditDecisions`);
  - one `RunPackageListGCChunk` with a `maxRows` of 1 to 50.

  After every step, for every suite: `liveRows` equals the model's rows exactly (all columns but `SnapshotID`), or the suite names no list when the model has none; `DeclaredHashesForPath` for three sampled paths answers from the model (each declaring suite's hash, nothing from any unnamed list); the generation never decreased. At the end: `RunPackageListGCChunk` until `more` is false, then no `package_list` row is unnamed and `package_list_entry` holds exactly the models' rows.

- [ ] **Step 2: Run it.** `go test ./internal/cache/ -run TestPackageLists_AModelOfTheSwitch -count=1`. Expected: pass. If it fails, the failure is a bug in Tasks 3 or 5: fix it there, with its own failing test first.

- [ ] **Step 3: Prove it can fail.** Apply each mutant alone, run the test, require a failure, revert (a script in the scratchpad, not committed; record the outcome in the commit message):
  1. the commit skips the list-id and generation comparison;
  2. the difference path skips the `Removed` deletes and the count check;
  3. `InsertReconciledMembers` does not bump the generation;
  4. `quarantineSnapshotTx` leaves `package_list_id` set;
  5. `RunPackageListGCChunk`'s select loses its `NOT EXISTS`;
  6. `diffPackageRows` compares the hash only.

  A mutant that survives means the model misses that behaviour: extend the model until it is killed.

- [ ] **Step 4: Verify.** `go test ./... -count=1`; `go test -race ./internal/cache/ -run TestPackageLists_AModelOfTheSwitch -count=1`.

- [ ] **Step 5: Commit** `test(cache): a model of the package lists, proven by six mutants`.

---

### Task 7: The adopter hands the commit a difference

**Files:**
- Modify: `internal/freshness/adoption.go` (`runShared`: the read before the barrier, the commit call), `internal/freshness/adoption_log.go` (the fields), `internal/freshness/metrics.go` (the counter), `docs/log-fields.md`
- Test: `internal/freshness/package_list_test.go` (create)

**Interfaces:**
- Consumes: `cache.DiffPackageList`, `cache.CommitAdoptionDiff`, `cache.PackageListWrite`.
- Produces:
  - On `adoption_finished`, when the attempt committed: `package_list_write` (`difference`, `rewrite`, `none`), `package_list_rewrite_reason` (only for a rewrite), `package_rows_added`, `package_rows_changed`, `package_rows_removed`.
  - ERROR `package_list_mismatch`: `canonical_host`, `suite_path`, `snapshot_id`, `expected_rows`, `found_rows`, `added`, `changed`, `removed`. The adoption succeeded and the list is exact; the line reports a defect in the difference.
  - Counter `acu_adoption_package_list_writes_total{mode, reason, host}`, built like `acu_hot_prefetch_total`.

  In `runShared`, after the unsigned re-mark and before `attempt.nextPhase("flip_wait")`, still in the `prepare_commit` phase: for a signed form, `DiffPackageList` under `p.barrierCtx(ctx)` bounded by `commitTimeout`. A read error is not an adoption failure: WARN `package_list_diff_failed` (`canonical_host`, `suite_path`, `snapshot_id`, `err`) and a nil difference, which the commit rewrites. A shutdown during the read returns as the re-mark's does (`p.stopErr`). The commit closure calls `CommitAdoptionDiff` and stores the result on the attempt.

- [ ] **Step 1: Write the failing tests** in `package_list_test.go`, with the package's existing adoption fixtures (a signed suite served by the test upstream, a recording logger as `work_log_test.go` uses):
  - `TestAdoption_AChangedPublicationWritesOnlyTheDifference`: adopt a suite of 40 packages; publish again with one package's version changed (one path removed, one added); `adoption_finished` of the second carries `package_list_write=difference`, `package_rows_added=1`, `package_rows_removed=1`, `package_rows_changed=0`; the suite's `package_list_id` is the first adoption's.
  - `TestAdoption_TheFirstAdoptionReportsARewrite`: `package_list_write=rewrite`, `package_list_rewrite_reason=first`, `package_rows_added=40`.
  - `TestAdoption_AFailedListReadStillAdopts`: a test seam makes the read fail (`SetPackageListDiffForTest(func(...) (*cache.PackageListDiff, error))` on the adopter, beside its other test seams): WARN `package_list_diff_failed`, the adoption succeeds, `package_list_rewrite_reason=no_difference`.
  - `TestAdoption_AMismatchIsLoggedAsAnError`: the seam returns a difference missing one removed path: ERROR `package_list_mismatch` with both counts, the adoption succeeds, the vouched hash of a changed package is the new one.
  - `TestAdoption_AFailedAttemptCarriesNoPackageListFields`: an attempt that fails before the commit has none of the five fields.

- [ ] **Step 2: Run them.** `go test ./internal/freshness/ -run 'TestAdoption_(AChangedPublication|TheFirstAdoptionReports|AFailedListRead|AMismatchIsLogged|AFailedAttemptCarries)' -count=1`. Expected: build failure on the seam, then missing fields.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Docs.** `docs/log-fields.md`: the five fields under `adoption_finished`; rows for `package_list_mismatch` and `package_list_diff_failed`; the counter where the adoption metrics are listed.

- [ ] **Step 5: Verify.** `go test ./... -count=1`; `go test -race ./internal/cache/ ./internal/freshness/ ./internal/gc/ ./internal/handler/ ./internal/observability/ -count=1`; `golangci-lint run` (three errcheck findings in `cmd/apt-cacher-ultra/ca_constraints_log_test.go` and `readiness_test.go` exist on `main` and are not this branch's).

- [ ] **Step 6: Commit** `feat(freshness): an adoption writes the package rows that changed`.

---

### Task 8: Measure it

The maintainer's request: the "PHP update" timing and whatever else quantifies the change. The runs and their baseline are the spec's "Measurements" section.

**Files:**
- Modify: `e2e/freshness-lab/burst.py` (the summary records the five `adoption_finished` fields of Task 7), `e2e/freshness-lab/README.md` (the results beside the baseline table)
- Create: `e2e/freshness-lab/package-list-result-measurements.json`, `internal/cache/realdb_measure_test.go`
- Modify: `docs/superpowers/specs/2026-11-18-package-list-per-suite-design.md` (the result table beside the baseline), `Changelog.md` (Unreleased: schema 11, what an adoption writes, the renamed GC metric and fields, the new log fields), `docs/open-decisions.md` (a "Decided" entry: the calls of this plan and the numbers)

**Interfaces:**
- Produces: `TestMeasure_CommitOnARealDatabase`, skipped unless `ACU_MEASURE_DB` names a `cache.db`. It copies the file into `t.TempDir()`, opens it (which migrates it, timing the open and logging each `schema_migrating` step), takes the suite with the largest list, and for a next list with 241 rows changed (a third removed, changed, added) reports time and WAL growth of: the difference commit, and a commit of the same list with a nil difference. It prints one line per result and the row counts before and after the migration.

- [ ] **Step 1: The lab, after.** Build the image from the branch, then the three runs of the baseline, back to back, load noted before and after each: `php` (one suite), `burst-real` (`--catalog-size 33000 --catalog-churn 0.003`), `burst-total` (`--catalog-churn 1.0`), with the options the baseline's JSON records. Per suite: the commit phase, `writer_wait_seconds`, `writer_ahead`, `package_list_write`, publication to client-visible; per run: any `database_write_waited` line and what it waited behind.
- [ ] **Step 2: The lab, before, again.** One fresh `burst-real` on `main` 421f5ea in the same sitting, so the comparison does not rest on yesterday's load.
- [ ] **Step 3: The commit and the migration on the June database copy** (`…/scratchpad/pkgrows/db/cache.db`, schema 4): `ACU_MEASURE_DB=… go test ./internal/cache/ -run TestMeasure_CommitOnARealDatabase -count=1 -v`, twice. Before: the same test with its row read written against `package_hash` and the plain `CommitAdoption`, run in a `git archive main` export, twice.
- [ ] **Step 4: GC.** From the burst runs' databases: package rows left for GC after the run (before: rows of non-current snapshots; after: entries of unnamed lists).
- [ ] **Step 5: Write up.** The result table in the README and the spec, each number with its run count and load; what did not move and why; anything that got worse.
- [ ] **Step 6: Verify and commit.** `go test ./... -count=1`. Commit `docs: package lists measured against the baseline`.

---

## After the last task

One fresh reviewer reads the whole branch against the spec (the migration, the commit's two paths, every reader, GC). Then the maintainer decides the release and whether phase 2 (staging a large rewrite before the flip) is worth building.
