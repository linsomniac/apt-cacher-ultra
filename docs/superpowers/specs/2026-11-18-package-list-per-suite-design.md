# One package list per suite, written by difference

Date: 2026-11-18. Branch: `perf/package-rows-per-suite` (off `main`
421f5ea, which has the writer-wait logging). Status: approved by the
maintainer 2026-11-19 as written; built and measured the same day (phase
1; "Result" at the end). Plan:
`docs/superpowers/plans/2026-11-19-package-list-per-suite.md`.

## Why

An adoption can take long, and part of that is the one database writer:
every write in the daemon runs on one goroutine, in arrival order. The
largest single thing in that queue is the adoption's own commit, which
inserts the suite's whole package list again under the new snapshot's id
(`CommitAdoption`, step 3): 45k to 67k rows for an Ubuntu suite, 420k for
Debian trixie (issue #27). Client-miss writes, heartbeats and other
adoptions wait behind it, and the commit runs inside the flip barrier's
hold on clients (the lab's `hold_expired`, 11.77 s in one commit).

Measured on a copy of the maintainer's June production database (1.16M
package rows, 746 MB; scratch tool, one run each way, a busy laptop):

| Re-inserting jammy-updates (66,928 rows) | Time, two runs | Written to the WAL |
|---|---|---|
| as today | 2.6 s, 5.2 s | 140 MiB |
| rows sorted by path | 2.1 s, 6.8 s | 140 MiB |
| 100 rows per statement | 3.0 s, 17.3 s | 140 MiB |
| 64 MiB page cache | 5.2 s, 6.8 s | 140 MiB |
| only the changed rows (241) | 0.04 s, 0.09 s | 1 MiB |

Nothing inside the transaction helps: the cost is the bytes, and the time
follows disk load. The path-ordered primary key puts each new row beside
the older snapshots' entry for the same path, so one adoption rewrites
most of that host's index.

And the rows record almost nothing new. Between the snapshots that
database kept, 0 to 241 of 45k to 67k rows differed, and in over half of
the pairs none did. Package rows are 646 of its 656 MiB: 292 MiB of rows,
241 MiB of path index, 99 MiB of package/architecture index.

That database predates 1.1.0's hourly strip of displaced snapshots' rows
(sub-job D), so today's table holds fewer old rows and an adoption writes
somewhat less than 140 MiB. Not measured.

## Goal

An adoption writes the package rows that changed, not the whole list.

- A typical publication (well under 1% of rows changed) commits in
  milliseconds and writes on the order of 1 MiB.
- No case is slower than today, including a first adoption and a
  publication that changes every row.
- Every reader answers as it does today: which signed current snapshot
  declares which hash for which path.
- The table holds current rows only, so GC has no package rows to strip
  after an ordinary adoption.

## Non-goals

- Shortening the first adoption of a suite, or a publication that changes
  most of a suite. They keep today's cost in phase 1 (phase 2, below, is
  the follow-up that bounds their hold on the writer).
- Writer priorities, adoption slots, the flip barrier, warming. Separate
  work.
- A smaller database file. Freed pages are reused; SQLite returns them to
  the filesystem only on `VACUUM`, which stays the operator's choice.

## Anti-goals

- A package row that vouches for a file its suite's current snapshot does
  not declare, or a declared file left without its row. The first lets
  bytes through against the wrong hash's authority; the second loses
  vouching silently in non-strict mode and refuses a good file in strict
  mode. Either is worse than a slow commit.
- A reader that still reads the old table. The old table is dropped so
  that a missed reader is an SQL error in the tests, not a wrong answer.

## Approaches considered

**A. Keep per-snapshot rows; write them in chunks before the switch.**
The rows of a candidate snapshot are invisible until it is current, so
they can be inserted in 10,000-row transactions ahead of the flip. Same
bytes (measured: 3.3 to 5.6 s in all, longest transaction 1.0 to 1.2 s),
but no long hold and nothing inside the client hold. No schema change.
Needs chunked cleanup of abandoned candidates and safe re-staging on a
retry. Rejected as the main change: it leaves 99% of the writes redundant
and all of GC's package-row work. Its idea returns as phase 2.

**B. One package list per suite, updated by difference at the switch.**
Chosen (the maintainer, 2026-11-18). Measured above: 241 rows, 0.04 to
0.09 s, 1 MiB.

**C. Reuse the previous snapshot's rows when the list is identical.** A
pointer from the snapshot to the rows it shares. Needs the same reader and
GC changes as B and helps only the adoptions that changed nothing.
Rejected.

**D. Lead the key with the snapshot id.** A new snapshot's rows would then
append in one run (the "no path index" benchmark column of the issue #14
design: flat at about 1.0 s per 70k rows). Every lookup by path would
probe once per current suite of the host. Needs a table rebuild and the
reader changes of B for a 4 to 5 times gain where B gives 100. Rejected.

## Design

### Lists and entries

A *package list* is the set of package rows a suite's current snapshot
declares. It has an id of its own, never reused, and is not tied to a
snapshot id:

```sql
CREATE TABLE package_list (
  package_list_id INTEGER PRIMARY KEY AUTOINCREMENT
);

CREATE TABLE package_list_entry (
  package_list_id  INTEGER NOT NULL REFERENCES package_list(package_list_id),
  canonical_scheme TEXT NOT NULL,
  canonical_host   TEXT NOT NULL,
  path             TEXT NOT NULL,
  declared_sha256  TEXT NOT NULL CHECK (...as package_hash...),
  package_name     TEXT NOT NULL DEFAULT '',
  architecture     TEXT NOT NULL DEFAULT '',
  version          TEXT NOT NULL DEFAULT '',
  flat_filename    TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (package_list_id, path)
);
-- lookups by request path (vouching, the url_path joins, the triggers)
CREATE INDEX idx_package_list_entry_path
  ON package_list_entry(canonical_scheme, canonical_host, path);
-- newest-N versions of a package, per-architecture counts
CREATE INDEX idx_package_list_entry_pkg
  ON package_list_entry(package_list_id, package_name, architecture);
-- the flat-repository fallback, as today
CREATE INDEX idx_package_list_entry_flat
  ON package_list_entry(canonical_scheme, canonical_host, flat_filename)
  WHERE flat_filename <> '';

ALTER TABLE suite_freshness ADD COLUMN package_list_id INTEGER
  REFERENCES package_list(package_list_id);
ALTER TABLE suite_freshness ADD COLUMN package_list_generation INTEGER NOT NULL DEFAULT 0;
-- from an entry's list to its suite: every reader's join, both triggers,
-- and GC's "a list no suite names". Unique: a list belongs to one suite.
CREATE UNIQUE INDEX idx_suite_freshness_package_list
  ON suite_freshness(package_list_id) WHERE package_list_id IS NOT NULL;
```

Readers reach the suite from an entry through that index
(`JOIN suite_freshness sf ON sf.package_list_id = e.package_list_id`).
Today's readers go through `suite_snapshot` to the suite key because
`current_snapshot_id` has no index (design review M-2, the comment above
`declaredExactSQL`); the triggers fire on every `url_path` request stamp,
so this join must be an index probe, and a plan-pinning test says so.

`suite_freshness.package_list_id` names the suite's live list (NULL: none,
as for a suite without a current snapshot or with an unsigned one). A list
no suite names is garbage. The columns of an entry are `package_hash`'s,
with the list id in place of the snapshot id, so the Go type
(`cache.PackageHash`) and everything that builds rows in memory
(`buildPackageHashes`, `buildSourceHashes`, `buildPdiffHashes`, the hot
set's stage 2) are untouched.

Why a list id and not the suite key: a suite that is deleted and created
again gets a new list, so leftover rows can never be mistaken for its
current ones; garbage is found by id, in chunks, the way sub-job C finds a
retired snapshot's rows; and a whole new list can be written beside the
live one and swapped in, which is what a large change needs.

### The switch

`runShared` has the new rows in memory, as today. Before the flip barrier
it reads the suite's live list on a reader connection, in one read
transaction: the list id, the generation, and the entries in path order.
It merges them with the new rows (sorted by path) into three sets:
removed paths, changed rows, new rows.

`CommitAdoption` receives the new rows, the difference, and the list id
and generation the difference was computed against. Inside the flip
transaction, where the pointer moves today:

1. **Difference path**, when the suite's `package_list_id` and
   `package_list_generation` still equal the ones read, and the difference
   is at most half the size of the new list: delete the removed paths,
   update the changed rows, insert the new rows, all by primary key, in the
   live list. Bump the generation.
2. **Rewrite path**, otherwise (no live list: a first adoption; a list
   that changed since the read; a difference too large to be worth it):
   create a new list, insert every row, point the suite at it, bump the
   generation. The old list becomes garbage. This is today's cost, and
   today's code shape.
3. **Check:** after the difference path, the live list's row count must
   equal the number of new rows (one index count, a few milliseconds at
   67k rows). A mismatch means the difference was wrong. The transaction
   then rolls back to a savepoint taken before step 1 and takes the
   rewrite path instead, which is exact by construction; ERROR
   `package_list_mismatch` (the suite, both counts, the size of the
   difference) and a counter record it. The adoption still succeeds: a
   wrong list is never published, and a bug in the difference costs
   nothing but its report. A count is a tripwire, not a proof (a wrong
   hash on a changed row keeps the count); the model test is the proof.

An unsigned candidate has no rows: its commit sets `package_list_id` to
NULL (spec C5 safeguard 5, unchanged in effect).

The difference is computed against the rows that are in the table, not
against what the previous snapshot's files said. So whatever the list held
before, the list after the commit is exactly the new rows: the scheme
repairs drift instead of carrying it forward.

Step 1's guard is what makes that true at commit time. The suite's mutex
already keeps a reconcile or another adoption of the suite out between the
read and the commit; the generation, bumped in the same transaction by
every writer of the list, makes the rule hold by itself rather than by
inspection of who holds what.

`recordSnapshotHotPackages` (the join of the snapshot's rows with
`url_path` inside the commit) runs over the rows the commit wrote: the new
and the changed rows on the difference path, all of them on the rewrite
path. A changed row counts because the join matches on the hash, and a
warmed file's `url_path` row can already carry the new one. An entry that
did not change was recorded when it arrived. The function takes the list
and the paths written instead of a snapshot id.

Where the difference is computed: in `internal/cache`, by a function that
reads the live list and merges it with the new rows, which `runShared`
calls before the barrier and whose result it hands to `CommitAdoption`. A
commit given no difference takes the rewrite path, so the 127 test calls
of `CommitAdoption` keep their meaning.

### Readers

All SQL on the table is in `internal/cache`. Each reader moves from
"`package_hash` rows whose `snapshot_id` is a suite's
`current_snapshot_id`" to "`package_list_entry` rows whose
`package_list_id` is a suite's `package_list_id`", and keeps its join to
the current snapshot where it needs the snapshot (the unsigned filter,
`DeclaredHash.SnapshotID` for conflict logs):

| Reader | Used by |
|---|---|
| `declaredExactSQL`, `declaredBySuffixSQL` | vouching: the handler's hit, miss and prefetch checks |
| `flatRowsExistSQL` | the flat-rows flag at open |
| `ListIntegrityCandidates` | the at-rest scan |
| `GetRepoCoverage`, `GetCacheSummaryByHostArch` (both queries) | admin |
| `urlPathRetainedTx`, `topNVersionSetTx` | version-aware retention in the url-path GC pass |
| `recordSnapshotHotPackages` | the commit |
| triggers `hot_package_insert`, `hot_package_touch` | every `url_path` request stamp |
| `insertPackageHashTx` (probe, heal, insert) | reconcile |
| `GetPackageHash`, `DeletePackageHashForTest` | tests only |

`GetRepoCoverage` and the cache-summary query keep their join order (the
small suite table outside; `url_path` outside for the blob query: the
comments in `queries.go` say why). The writers and deleters that go with
the table: `CommitAdoption` step 3, sub-job A's cascade (`gc.go`, the
`IN` list), sub-job C's table loop, sub-job D's select and delete, and
the `gc` configuration comment that describes D.

Two of these change what they see, both narrowly:

- The two triggers today match rows of *any adopted* snapshot, so a
  request for a file only a displaced snapshot lists still records
  interest in its package, until sub-job D strips those rows (within the
  hour). They will match live lists only. The 2026-11-09 audit ("every
  reader is current-only") predates these triggers and
  `recordSnapshotHotPackages` (schema 10, 2026-11-14); its comment in
  `gc.go` and `TestPackageHashReaders_AnswerTheSameAfterTheStrip` still
  name `ComputeHotSet`, which no longer reads the table. The plan corrects
  both.
- `flatRowsExistSQL` today sees flat rows of displaced snapshots too.
  Reading live and garbage lists alike keeps "true is never wrong, false
  is never stale" (ruling PG-31).

Reconcile (`InsertReconciledMembers`) adds a recovered index's rows to the
suite's live list, with its existing current-snapshot guard, the same
probe, heal and insert per row, and a generation bump. A suite whose
current snapshot is signed always has a list after its first commit under
this schema; the migration gives every such suite one.

### What deletes rows

- **An ordinary adoption:** nothing afterwards. The difference path
  deleted the removed paths in the commit.
- **A rewrite, a suite's removal (reap, forget, cap eviction), a
  quarantine by the legacy audit:** the old list is left unnamed. A new GC
  sub-job in the snapshot pass deletes the entries of a list no suite
  names, 10,000 rows a transaction, then its header. It takes sub-job D's
  place in the pass and in the hourly retired run, with D's metrics
  renamed for what they now count. The quarantine sets the suite's
  `package_list_id` to NULL where it already sets `current_snapshot_id`
  to NULL.
- **Sub-jobs A and C** no longer touch package rows: a candidate holds
  none, and a retired snapshot holds none.

`gc_displaced_packages` (the write kind, 421f5ea) becomes
`gc_package_lists`.

### Migration (schema 11)

One new entry in `migrations` (append-only, like every one before it; no
existing entry is edited), pure SQL, in the one transaction
`applyMigration` gives it:

1. Create the two tables, the indexes and the two `suite_freshness`
   columns.
2. For every suite with a signed current snapshot: a list whose id is that
   snapshot's id (unique per suite; `AUTOINCREMENT` continues above the
   highest), and `suite_freshness.package_list_id` set to it.
3. Copy the current snapshots' `package_hash` rows into
   `package_list_entry`. Rows of displaced, retired and candidate snapshots
   are not copied: no reader could see them.
4. Drop the two `hot_package` triggers and create them again on the new
   table.
5. Drop `package_hash`.

It runs once, at the first start of the new version, before the daemon
listens; `schema_migrating` is logged as for every migration. Its time is
the copy of the current rows with three indexes: to be measured on the
June database and reported with the results. The release notes' "the
first restart can take minutes" warning covers it. There is no way back to
schema 10 short of the pre-upgrade backup, as for schema 10 itself.

### Phase 2 (not in this plan): large changes without a long hold

With lists as ids, a rewrite does not have to happen inside the flip. The
adoption can create the new list before the barrier, fill it in
10,000-row transactions (approach A's measurement: longest about 1 s), and
the flip then only points the suite at it. A candidate snapshot would name
its staged list, so GC keeps a list a live candidate names and frees it
with the candidate. That bounds the writer hold of a first adoption and of
a release that changes everything. It is left out here so that phase 1 can
be measured by itself; the schema above already allows it.

## Tests

Robustness over speed: each behaviour is pinned before it is built.

- **A model test of the switch:** random sequences over several suites of
  first adoptions, small and large differences, identical publications,
  unsigned adoptions, failed commits, reconciles, suite removal and
  re-creation, quarantine, and GC chunks between any two steps. After
  every step each suite's live list must equal, row for row, the rows its
  current snapshot declares; no garbage list may be readable through any
  reader; and after GC has run dry no unnamed list may remain.
- **The guard:** a difference computed against a list that then changed (a
  reconcile's row, a swapped list) takes the rewrite path and still ends
  exact. The count check turns a deliberately wrong difference into a
  rewrite of the list and an ERROR, with the adoption succeeding and the
  list exact (corrected 2026-11-19: this bullet still described the
  earlier "fail the adoption" variant, which "The switch" and call 2
  replaced before approval).
- **Every reader, before and after:** the existing reader tests, moved to
  the new table, plus the query-plan pins (`suite_freshness` by key, the
  partial flat index, the list index for the newest-N probe).
- **Migration:** a schema-10 database with current, displaced, retired and
  candidate snapshots, flat and non-flat rows, and an unsigned suite
  migrates to the same answers from every reader; a second open is a
  no-op; the triggers record interest against the new table.
- **Crash safety:** the flip is one transaction, as today; the existing
  commit-rollback tests cover the difference path too.
- **The simulations** (`internal/simtest`, the 90-day run) and the e2e
  suites run unchanged.

## Measurements

Before and after, same machine, runs back to back, each stated with its
load:

1. **The "PHP update" lab scenario** (`e2e/freshness-lab`, one suite, the
   2026-11 report's shape): publication to client-visible. Expected
   unchanged; it is the regression check.
2. **The six-suite burst at a real suite's size** (about 66k rows a
   suite), with 0.3% of the catalog changing per publication, as the June
   data did, and with all of it changing, the worst case. Per suite: the
   commit phase, `writer_wait_seconds` and `writer_ahead` (421f5ea), and
   publication to client-visible. The lab's burst gained
   `--catalog-churn` for this; until now every publication changed every
   row, which no real suite does.
3. **The commit alone on the June database:** the real `CommitAdoption`
   with a real suite's next list, time and WAL bytes.
4. **The migration on the June database:** time, and rows before and
   after.
5. **GC:** package rows deleted per adoption, before and after.

Baseline for 1 and 2, `main` 421f5ea, one run each on a shared host
(`e2e/freshness-lab/README.md`, "A real suite's size and a real
publication's change";
`e2e/freshness-lab/package-list-baseline-measurements.json`):

| | Publication to all candidates visible | Commit phase per suite | Longest wait of another write behind a commit |
|---|---|---|---|
| One suite, "PHP update" | 59.5 s | 0.002 s | none |
| Six suites of 66,000 rows, 0.3% changed | 189.8 s | 3.1 to 17.8 s (40.4 s in all) | 5.4 s (a client's `url_path_touch`) |
| Six suites of 66,000 rows, all changed | 223.8 s | 5.1 to 22.9 s (76.3 s in all) | 25.8 s (an adoption behind two commits) |

What to expect from this change, so that the result can be judged: the
commit seconds of the middle row, and the waits behind them, should go;
the single-suite row should not move (its 59.5 s is 39.3 s of detection
and 20.1 s of barrier); the last row should stay about where it is
(phase 1 rewrites a list that changed entirely). The bursts' other phases
are untouched by it: 5 to 9 s of index parsing per suite, the warming
budget, the barrier.

### Result (2026-11-19)

Branch 602de26 against `main` 421f5ea; the 0.3% burst was repeated on
2026-11-20 at cb73f3f, after the review's minor findings were done. The
detail, the conditions and the evidence files are in
`e2e/freshness-lab/README.md`, "One package list per suite: the result".

1 and 2, the lab, branch and main alternating on a quiet machine (the
baseline table above was taken under load; `main` is re-measured here):

| | `main` | Branch |
|---|---|---|
| One suite, "PHP update": publication to client-visible | 59.6 s, 61.1 s | 59.8 s, 59.7 s, 59.6 s |
| Six suites of 66,016 rows, 0.3% changed: commit phase, all six together (three runs of `main`, four of the branch) | 14.9 to 15.6 s | 0.18 to 0.41 s |
| to all visible, the same runs | 160.5 to 180.1 s | 135.5 to 160.1 s |
| Six suites, all changed: to all visible | 163.0 s | 153.6 s |
| commit phase per suite (in all) | 2.29 to 2.58 s (14.4 s) | 1.75 to 1.89 s (10.8 s) |

3 to 5, on the June database copy (two runs a side; the schema-11 step
four):

| | `main` | Branch |
|---|---|---|
| Commit of jammy-updates' next list, 241 of 66,928 rows changed | 2.0 to 2.5 s, 64 to 66 MiB of WAL | 0.05 s, 1.8 to 2.2 MiB (plus 0.25 s reading the live list beforehand, off the writer) |
| Commit that writes the whole list | the same | 1.8 to 2.3 s, 67 MiB |
| Package rows GC deletes after one adoption | 66,930 | none (after a whole-list commit: the old list) |
| The schema-11 migration: 1,159,431 rows before, 292,712 after | | 5.3 to 10.2 s |

Against the goals: a typical publication commits in 50 ms and writes
2 MiB (the goal said milliseconds and on the order of 1 MiB); no case
measured is slower than before, the whole-list commit included; the table
holds current rows only. Against the expectations above: the middle
row's commit seconds went, the single-suite row did not move, the last
row stayed about where it was.

What the measurements do not show, and what they showed that was not
asked:

- How much sooner the six suites are all visible. The branch was ahead
  in both sittings, but runs of one binary differ by about 20 s, as much
  as the two sides do; the first sitting's single pair (136.5 s against
  160.5 s) overstated it. The commit seconds are the firm result. The
  all-changed burst is still one run a side.
- Under heavy load from other work on the machine, one branch run still
  had commits of 12 to 16 s that wrote 428 rows each. A quiet rerun did
  not, and evicting the database file from the page cache does not
  reproduce it. Unexplained.
- GC's chunk delete was first keyed by path and held the writer about
  three times as long a chunk as `main`'s; it is keyed by rowid since,
  and is back at `main`'s level (0.30 s for the longest chunk).
- The host summary's row count (admin) is no longer answered from a
  covering index: 336 ms against 155 ms in `BenchmarkAdminCacheSummaries`.
- The largest database measured is 1.16M package rows. The migration's
  time and temporary space on a much larger one are not measured.

## Left to the builder

- Whether `package_list_entry` is a rowid table or `WITHOUT ROWID` (the
  issue #14 benchmark found the latter worse for a snapshot-keyed table;
  decide by benchmark here).
- The exact size at which the rewrite path beats the difference path
  ("half the new list" is the starting rule; measure).
- Names of the renamed GC metrics, and the admin page's labels.
- How the merge of old and new rows is chunked in memory for a 420k-row
  suite (the old list can be streamed in path order; the new rows are
  already in memory today).

## Calls made here that the maintainer may want to change

1. **The old table is dropped in the migration**, not kept beside the new
   one for a release. Keeping it would hold several hundred MiB for no
   reader.
2. **A wrong row count rewrites the list in the same transaction and logs
   an ERROR**, instead of failing the adoption. Failing would repeat at
   every retry against the same unchanged list, and a suite would stay on
   its old release until someone read the log.
3. **The `hot_package` triggers see live lists only** (above).
4. **Phase 2 waits for phase 1's numbers.**
5. **Which release this lands in is not decided here.** It is a schema
   bump (10 to 11) either way.

Added while building (the plan's "Calls made" and the executor's
rulings; the final review weighed them):

6. `CommitAdoption` keeps its signature and always writes the whole
   list; `CommitAdoptionDiff` takes the difference.
7. A difference is applied behind more than the row count: every delete
   and update must hit exactly one row, no insert may be refused by a
   constraint, and the difference must have been computed for the very
   rows the commit is given (a digest). Any of them failing writes the
   whole list and reports `package_list_mismatch`. A statement that fails
   any other way (a full disk, an I/O error) fails the commit, as on the
   whole-list path: it is not a defect in the difference.
8. Reconcile records hot-package interest for the rows it adds: no later
   commit visits an unchanged row.
9. The migration copies a current snapshot's rows whose scheme and host
   are its suite's own, and gives a list only to the suite a snapshot
   belongs to, so it cannot fail on the new keys. Every release's writers
   took both from the suite.
10. The admin aggregates keep their scheme and host terms (an existing
    test pins them), written so SQLite does not choose an index by them.
11. `package_list_entry` is a rowid table; the rewrite path is taken past
    half the new list (both by measurement, in the plan).
12. The commit records hot-package interest for the files the adoption
    warmed, after it writes their `url_path` rows: a warm changes a row's
    bytes without a request stamp, so no trigger fires, and no later
    commit visits an unchanged entry.

The final review's four minor findings (2026-11-19) were done on
2026-11-20: items 7 (its last sentence) and 12 above; query-plan pins for
GC's unnamed-list select, its chunk delete and the difference's read; a
test of a shutdown during that read; and the model test now puts a
quarantine and a forget-and-recreate between a difference's read and its
commit, hands commits a difference made for other rows and for the
scheme twin, heals rows through reconcile, and has https twins and flat
suites.
