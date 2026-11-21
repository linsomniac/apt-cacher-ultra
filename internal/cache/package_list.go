package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
)

// A package list is the set of package rows a suite's current snapshot
// declares (schema 11; docs/superpowers/specs/2026-11-18-package-list-per-suite-design.md).
// package_list holds its id, package_list_entry its rows, and
// suite_freshness.package_list_id names the list a suite serves from.
//
// AIDEV-NOTE: who may see an entry. A list a suite names is live: every
// reader reaches entries through `JOIN suite_freshness sf ON
// sf.package_list_id = e.package_list_id` (the unique partial index
// idx_suite_freshness_package_list), so an entry of a list no suite names
// — a rewritten list, a removed or quarantined suite's, until GC deletes
// it (RunPackageListGCChunk) — is invisible to vouching, the at-rest
// scan, the admin counts, the url-path GC's retention and the hot_package
// triggers. The one reader without that join is flatRowsExistSQL, whose
// answer may be true for no reason and never false for one (ruling
// PG-31). A new reader of package_list_entry must join the same way;
// TestPackageListReaders_AnUnnamedListIsInvisible is where it is pinned.
//
// AIDEV-NOTE: who writes a suite's list, each bumping
// suite_freshness.package_list_generation in the same transaction:
// CommitAdoption (a new list, or none for an unsigned candidate),
// InsertReconciledMembers (rows added to the live list), the legacy
// audit's quarantine (the suite names none). A suite's delete leaves its
// list unnamed with nothing to bump. A new writer must bump it too: the
// generation is what a commit checks a difference against.

// insertPackageListEntrySQL inserts one row into a list.
const insertPackageListEntrySQL = `
INSERT INTO package_list_entry (package_list_id, canonical_scheme, canonical_host, path,
                                declared_sha256, package_name, architecture, version, flat_filename)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`

// writePackageListTx creates a new package list holding rows and returns
// its id. Nothing names the list yet: the caller points a suite at it in
// the same transaction, or it is garbage. Each row's declared hash is
// shape-checked first; two rows with one path fail the list's primary
// key. rows is not reordered.
//
// AIDEV-NOTE: the rows go in by ascending path, so a new list (the highest
// id) appends to the right edge of the primary key instead of landing at
// 67,000 scattered places; the path index still takes them scattered.
// Measured on a production database copy: 67,000 rows, about 2 s and
// 67 MiB of WAL (the plan, "Measured before planning").
func writePackageListTx(ctx context.Context, tx *sql.Tx, rows []PackageHash) (int64, error) {
	res, err := tx.ExecContext(ctx, `INSERT INTO package_list DEFAULT VALUES`)
	if err != nil {
		return 0, fmt.Errorf("create package list: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("create package list: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, insertPackageListEntrySQL)
	if err != nil {
		return 0, fmt.Errorf("prepare package_hash insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	order := make([]int, len(rows))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool { return rows[order[i]].Path < rows[order[j]].Path })
	for _, i := range order {
		p := rows[i]
		if !validBlobHash(p.DeclaredSHA256) {
			return 0, fmt.Errorf("package_hash %q declared_sha256 %w", p.Path, ErrInvalidHash)
		}
		if _, err := stmt.ExecContext(ctx, id, p.CanonicalScheme, p.CanonicalHost, p.Path,
			p.DeclaredSHA256, p.PackageName, p.Architecture, p.Version, p.FlatFilename); err != nil {
			return 0, fmt.Errorf("insert package_hash %q: %w", p.Path, err)
		}
	}
	return id, nil
}

// PackageListDiff is what turns a suite's live package list into an
// adoption's new rows (the package-list design, "The switch"): the paths
// to delete, the rows to update and the rows to insert. ListID and
// Generation name the list it was computed against; a commit applies it
// only to that list at that generation.
type PackageListDiff struct {
	ListID     int64         // the live list the difference was computed against
	Generation int64         // suite_freshness.package_list_generation at that read
	Removed    []string      // paths the live list has and the new rows do not
	Changed    []PackageHash // new rows whose path the live list has with another column different
	Added      []PackageHash // new rows whose path the live list does not have
	TooLarge   bool          // more rows differ than the limit: the three slices are nil

	// rows identifies the new rows the difference was computed for
	// (digestPackageRows): a commit applies it only to those rows.
	rows packageRowsDigest
}

// packageRowsDigest identifies a set of package rows whatever their order:
// how many there are, and the sum of a hash of each.
type packageRowsDigest struct {
	count int
	sum   uint64
}

// digestPackageRows digests rows: every stored column of every row
// (SnapshotID is not stored), independent of the rows' order.
//
// AIDEV-NOTE: this binds a difference to the rows it was computed for
// (the final review's I-1). A PackageListDiff says how to turn one list
// into one particular set of rows; the commit's other checks (the list,
// its generation, the row count afterwards) do not notice a difference
// handed over with other rows of the same number. It is a tripwire
// against a caller's mistake — a retry loop, a later phase that stages
// rows — not a security boundary: FNV-1a per row, summed, so no sort and
// no allocation (about 15 ms for 67,000 rows, on the adopter's goroutine,
// before the commit is queued). A column added to PackageHash that is
// stored must be added here
// (TestDigestPackageRows_EveryStoredColumnCounts fails until it is).
func digestPackageRows(rows []PackageHash) packageRowsDigest {
	const (
		offset = 14695981039346656037
		prime  = 1099511628211
	)
	d := packageRowsDigest{count: len(rows)}
	for i := range rows {
		r := &rows[i]
		h := uint64(offset)
		for _, s := range [...]string{r.CanonicalScheme, r.CanonicalHost, r.Path, r.DeclaredSHA256,
			r.PackageName, r.Architecture, r.Version, r.FlatFilename} {
			for j := 0; j < len(s); j++ {
				h ^= uint64(s[j])
				h *= prime
			}
			h ^= 0xff // a field's end: "ab","c" is not "a","bc"
			h *= prime
		}
		d.sum += h
	}
	return d
}

// Size is the number of rows the difference writes.
func (d *PackageListDiff) Size() int {
	return len(d.Removed) + len(d.Changed) + len(d.Added)
}

// ErrDuplicatePackagePath: two rows of one list share a path. A list is
// keyed by path, so no write of such rows could succeed.
var ErrDuplicatePackagePath = errors.New("cache: two package rows share a path")

// samePackageRow reports whether two rows of one path store the same
// values. SnapshotID is not stored in a list and is not compared.
func samePackageRow(a, b PackageHash) bool {
	a.SnapshotID, b.SnapshotID = 0, 0
	return a == b
}

// diffPackageRows merges the live list, which next yields in ascending
// path order (ok = false at its end), with rows, in any order, into the
// difference that turns the first into the second. rows is not reordered
// and the live list is never held: only the rows that differ are kept.
// Once more than limit rows differ it returns at once with TooLarge set
// and no rows (limit < 0: no limit). ListID and Generation are the
// caller's to fill in.
//
// Two rows of rows with one path are ErrDuplicatePackagePath, found before
// the live list is read. A live list out of order, or repeating a path, is
// an error too: it cannot happen under the list's primary key, and the
// merge would silently answer wrong.
//
// AIDEV-NOTE: the merge needs next's order and Go's to be the same order.
// They are: SQLite's ORDER BY on a TEXT column with the default (BINARY)
// collation compares the UTF-8 bytes with memcmp, and Go compares strings
// byte-wise. TestDiffPackageList_AgreesWithSQLitesOrder pins it against
// the database (upper and lower case, '+', '~', non-ASCII). A column
// collation or a LIKE-style comparison here would break the difference.
func diffPackageRows(next func() (row PackageHash, ok bool, err error), rows []PackageHash, limit int) (PackageListDiff, error) {
	order := make([]int, len(rows))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool { return rows[order[i]].Path < rows[order[j]].Path })
	for i := 1; i < len(order); i++ {
		if rows[order[i]].Path == rows[order[i-1]].Path {
			return PackageListDiff{}, fmt.Errorf("%w: %s", ErrDuplicatePackagePath, rows[order[i]].Path)
		}
	}

	var (
		d        = PackageListDiff{rows: digestPackageRows(rows)}
		old      PackageHash
		haveOld  bool
		lastPath string
		readAny  bool
	)
	advance := func() error {
		row, ok, err := next()
		if err != nil {
			return fmt.Errorf("read the live package list: %w", err)
		}
		if ok && readAny && row.Path <= lastPath {
			return fmt.Errorf("the live package list is not in path order: %q after %q", row.Path, lastPath)
		}
		if ok {
			lastPath, readAny = row.Path, true
		}
		old, haveOld = row, ok
		return nil
	}
	if err := advance(); err != nil {
		return PackageListDiff{}, err
	}
	tooLarge := func() bool { return limit >= 0 && d.Size() > limit }
	i := 0
	for haveOld || i < len(order) {
		switch {
		case haveOld && (i == len(order) || old.Path < rows[order[i]].Path):
			d.Removed = append(d.Removed, old.Path)
			if tooLarge() {
				return PackageListDiff{TooLarge: true, rows: d.rows}, nil
			}
			if err := advance(); err != nil {
				return PackageListDiff{}, err
			}
		case !haveOld || rows[order[i]].Path < old.Path:
			d.Added = append(d.Added, rows[order[i]])
			if tooLarge() {
				return PackageListDiff{TooLarge: true, rows: d.rows}, nil
			}
			i++
		default: // the same path in both
			if !samePackageRow(old, rows[order[i]]) {
				d.Changed = append(d.Changed, rows[order[i]])
				if tooLarge() {
					return PackageListDiff{TooLarge: true, rows: d.rows}, nil
				}
			}
			i++
			if err := advance(); err != nil {
				return PackageListDiff{}, err
			}
		}
	}
	return d, nil
}

// How a commit wrote its suite's package list (PackageListWrite.Mode).
const (
	PackageListDifference = "difference" // the live list was changed in place
	PackageListRewrite    = "rewrite"    // a new list was written whole
	PackageListNone       = "none"       // an unsigned commit: the suite names no list
)

// PackageListWrite says how a commit wrote its suite's package list.
type PackageListWrite struct {
	Mode string
	// Reason says why a rewrite was not a difference: "first" (the suite
	// named no list), "no_difference" (the commit was given none), "large"
	// (more than half the new list differs), "list_changed" (the list was
	// written after the difference was read), "mismatch" (the difference
	// did not produce the new rows: a defect, reported by the adopter).
	// The adopter reports "disabled" in place of "no_difference" when it
	// gave none because adoption.write_package_list_differences is false.
	Reason string
	// Rows inserted, updated and deleted. A rewrite counts every row as
	// Added.
	Added, Changed, Removed int
	// A mismatch: the rows the list must hold, the rows it held with the
	// difference applied, and what was wrong.
	Expected, Found int
	Detail          string
}

// liveListRowsSQL reads one list in path order: the difference's read.
//
// AIDEV-NOTE: the order comes from the list's primary key (package_list_id,
// path), so the rows stream without a sort: a sort would hold the whole
// list (420,000 rows for the largest suite) in a temporary B-tree before
// the first row. TestPackageListReaders_QueryPlans pins it.
const liveListRowsSQL = `
SELECT canonical_scheme, canonical_host, path, declared_sha256,
       package_name, architecture, version, flat_filename
  FROM package_list_entry WHERE package_list_id = ? ORDER BY path`

// DiffPackageList reads key's live package list, on a reader connection in
// one read transaction, and returns what turns it into rows: the argument
// CommitAdoptionDiff takes. It returns nil, nil when the suite names no
// list (or has no row): there is nothing to differ from, and the commit
// writes a whole list. A difference of more than half of rows is returned
// TooLarge, without its rows: the commit would not apply it. rows is not
// reordered; two of its rows with one path are ErrDuplicatePackagePath.
//
// The list is streamed in path order (its primary key's) and never held:
// only the rows that differ are kept, at most len(rows)/2 of them.
func (c *Cache) DiffPackageList(ctx context.Context, key SuiteKey, rows []PackageHash) (*PackageListDiff, error) {
	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("DiffPackageList: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var (
		listID     sql.NullInt64
		generation int64
	)
	err = tx.QueryRowContext(ctx, `
SELECT package_list_id, package_list_generation FROM suite_freshness
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`,
		key.CanonicalScheme, key.CanonicalHost, key.SuitePath).Scan(&listID, &generation)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("DiffPackageList: read the suite's list: %w", err)
	}
	if !listID.Valid {
		return nil, nil
	}
	live, err := tx.QueryContext(ctx, liveListRowsSQL, listID.Int64)
	if err != nil {
		return nil, fmt.Errorf("DiffPackageList: read list %d: %w", listID.Int64, err)
	}
	defer func() { _ = live.Close() }()
	next := func() (PackageHash, bool, error) {
		if !live.Next() {
			return PackageHash{}, false, live.Err()
		}
		var p PackageHash
		if err := live.Scan(&p.CanonicalScheme, &p.CanonicalHost, &p.Path, &p.DeclaredSHA256,
			&p.PackageName, &p.Architecture, &p.Version, &p.FlatFilename); err != nil {
			return PackageHash{}, false, err
		}
		return p, true, nil
	}
	d, err := diffPackageRows(next, rows, len(rows)/2)
	if err != nil {
		return nil, fmt.Errorf("DiffPackageList: %w", err)
	}
	d.ListID, d.Generation = listID.Int64, generation
	return &d, nil
}

// commitPackageListTx brings key's package list to rows inside the flip
// transaction and returns the list the suite is to name (the caller's
// Step 6 points it there and bumps the generation) and how it got there.
// live and generation are the suite's package_list_id and
// package_list_generation, read in this transaction.
//
// The difference path: diff is applied to the live list in place, when it
// was computed for these rows (rowsDigest is digestPackageRows(rows),
// taken by the caller before the commit was queued), against this list at
// this generation, and is at most half of rows. The rewrite path,
// otherwise and whenever the difference turns out wrong: a new list
// holding rows; the live one is left unnamed, for GC.
//
// AIDEV-NOTE: this is the guard the package-list design rests on ("The
// switch", step 1). A difference says how to turn one particular state of
// the list into rows; applied to any other state it produces a list that
// is neither. package_list_generation, bumped by every writer of a
// suite's list in its own transaction (package_list.go has the list of
// them), is what makes "computed against this list" checkable here, in
// the transaction that applies it. The suite's mutex is why the check
// normally passes — it keeps another adoption and a reconcile of the
// suite out between the read and the commit — but it is not the proof:
// the proof is this comparison. A writer of a list that does not bump the
// generation breaks it silently: no tripwire has to notice (a stale
// difference can change rows and leave their number right).
// TestCommitDiff_ADifferenceReadBeforeAnotherWasAppliedInPlaceIsNotApplied
// shows that for the commit's own bump, and
// TestPackageLists_AModelOfTheSwitch puts each writer it models between a
// read and its commit. A new writer is covered only once it is added
// there.
func commitPackageListTx(ctx context.Context, tx *sql.Tx, key SuiteKey, live sql.NullInt64, generation int64,
	rows []PackageHash, rowsDigest packageRowsDigest, diff *PackageListDiff) (listID int64, w PackageListWrite, err error) {
	switch {
	case !live.Valid:
		w.Reason = "first"
	case diff == nil:
		w.Reason = "no_difference"
	case diff.rows != rowsDigest:
		// The difference was computed for other rows than these (or these
		// were changed since): a caller's defect. Nothing of it is applied.
		w.Reason, w.Expected = "mismatch", len(rows)
		w.Detail = fmt.Sprintf("the difference was computed for other rows than the commit was given (%d rows then, %d now, or their contents changed)",
			diff.rows.count, rowsDigest.count)
	case diff.TooLarge || diff.Size() > len(rows)/2:
		w.Reason = "large"
	case diff.ListID != live.Int64 || diff.Generation != generation:
		w.Reason = "list_changed"
	}
	if w.Reason == "" {
		found, detail, err := applyPackageListDiffTx(ctx, tx, key, live.Int64, len(rows), diff)
		if err != nil {
			return 0, PackageListWrite{}, err
		}
		if detail == "" {
			return live.Int64, PackageListWrite{Mode: PackageListDifference,
				Added: len(diff.Added), Changed: len(diff.Changed), Removed: len(diff.Removed)}, nil
		}
		w.Reason, w.Expected, w.Found, w.Detail = "mismatch", len(rows), found, detail
	}
	id, err := writePackageListTx(ctx, tx, rows)
	if err != nil {
		return 0, PackageListWrite{}, err
	}
	// Learn identities for requests made before this suite had the row.
	// Only real request stamps count; warming never creates or extends
	// interest.
	if err := recordListHotPackages(ctx, tx, id, key, nil); err != nil {
		return 0, PackageListWrite{}, err
	}
	w.Mode, w.Added = PackageListRewrite, len(rows)
	return id, w, nil
}

// applyPackageListDiffTx applies diff to list listID inside a savepoint:
// each removed path deleted, each changed row updated, each added row
// inserted, one statement a row, by the list's primary key; then it
// records hot-package interest for the changed and added rows. detail ==
// "" means the list now holds exactly want rows and every statement did
// what the difference said it would. Otherwise the difference was wrong:
// the savepoint is rolled back, so the list is as it was, and detail says
// what did not hold (found is the row count when that was it).
//
// AIDEV-NOTE: the tripwires, none of them a proof (a wrong hash on a
// changed row passes all three; the model test is the proof). Each DELETE
// and UPDATE must hit exactly one row; an INSERT must not be refused by a
// constraint (isConstraintViolation: any other statement error fails the
// commit); the final count must be the number of new rows. A failed
// tripwire is not an error:
// the caller writes the whole list instead, which is exact by
// construction, and the adopter reports it (ERROR package_list_mismatch).
// Failing the adoption would repeat at every retry against the same list.
// A malformed hash is an error, as on the rewrite path: the rows are bad,
// not the difference. One statement per row, never an IN list: a
// difference can exceed SQLite's 32,766 bound parameters.
func applyPackageListDiffTx(ctx context.Context, tx *sql.Tx, key SuiteKey, listID int64, want int, diff *PackageListDiff) (found int, detail string, err error) {
	for _, set := range [][]PackageHash{diff.Changed, diff.Added} {
		for _, p := range set {
			if !validBlobHash(p.DeclaredSHA256) {
				return 0, "", fmt.Errorf("package_hash %q declared_sha256 %w", p.Path, ErrInvalidHash)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT package_list`); err != nil {
		return 0, "", fmt.Errorf("package list savepoint: %w", err)
	}
	found, detail, err = runPackageListDiffTx(ctx, tx, key, listID, want, diff)
	if err != nil {
		return 0, "", err
	}
	if detail != "" {
		if _, err := tx.ExecContext(ctx, `ROLLBACK TO package_list`); err != nil {
			return 0, "", fmt.Errorf("undo a wrong package list difference (%s): %w", detail, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `RELEASE package_list`); err != nil {
		return 0, "", fmt.Errorf("package list savepoint release: %w", err)
	}
	return found, detail, nil
}

// runPackageListDiffTx is applyPackageListDiffTx's body between the
// savepoint and its end. A statement refused by a constraint is a tripwire
// (an added path the list already has: the difference is wrong). A
// statement that fails any other way — ctx is done, the disk is full, an
// I/O error — is an error, and the commit fails as it would on the rewrite
// path: that is a fault of the database, not a defect in the difference,
// and must not be reported as one (the final review's M-1).
func runPackageListDiffTx(ctx context.Context, tx *sql.Tx, key SuiteKey, listID int64, want int, diff *PackageListDiff) (found int, detail string, err error) {
	failed := func(what, path string, err error) (int, string, error) {
		if ctx.Err() != nil || !isConstraintViolation(err) {
			return 0, "", fmt.Errorf("package list difference: %s %q: %w", what, path, err)
		}
		return 0, fmt.Sprintf("%s %q failed: %v", what, path, err), nil
	}
	one := func(res sql.Result, what, path string) (string, error) {
		n, err := res.RowsAffected()
		if err != nil {
			return "", fmt.Errorf("package list difference: %s %q: %w", what, path, err)
		}
		if n != 1 {
			return fmt.Sprintf("%s %q touched %d rows, want 1", what, path, n), nil
		}
		return "", nil
	}
	del, err := tx.PrepareContext(ctx, `DELETE FROM package_list_entry WHERE package_list_id = ? AND path = ?`)
	if err != nil {
		return 0, "", fmt.Errorf("package list difference: prepare delete: %w", err)
	}
	defer func() { _ = del.Close() }()
	for _, path := range diff.Removed {
		res, err := del.ExecContext(ctx, listID, path)
		if err != nil {
			return failed("delete", path, err)
		}
		if detail, err := one(res, "delete", path); detail != "" || err != nil {
			return 0, detail, err
		}
	}
	upd, err := tx.PrepareContext(ctx, `
UPDATE package_list_entry
   SET canonical_scheme = ?, canonical_host = ?, declared_sha256 = ?,
       package_name = ?, architecture = ?, version = ?, flat_filename = ?
 WHERE package_list_id = ? AND path = ?`)
	if err != nil {
		return 0, "", fmt.Errorf("package list difference: prepare update: %w", err)
	}
	defer func() { _ = upd.Close() }()
	written := make([]string, 0, len(diff.Changed)+len(diff.Added))
	for _, p := range diff.Changed {
		res, err := upd.ExecContext(ctx, p.CanonicalScheme, p.CanonicalHost, p.DeclaredSHA256,
			p.PackageName, p.Architecture, p.Version, p.FlatFilename, listID, p.Path)
		if err != nil {
			return failed("update", p.Path, err)
		}
		if detail, err := one(res, "update", p.Path); detail != "" || err != nil {
			return 0, detail, err
		}
		written = append(written, p.Path)
	}
	ins, err := tx.PrepareContext(ctx, insertPackageListEntrySQL)
	if err != nil {
		return 0, "", fmt.Errorf("package list difference: prepare insert: %w", err)
	}
	defer func() { _ = ins.Close() }()
	for _, p := range diff.Added {
		if _, err := ins.ExecContext(ctx, listID, p.CanonicalScheme, p.CanonicalHost, p.Path,
			p.DeclaredSHA256, p.PackageName, p.Architecture, p.Version, p.FlatFilename); err != nil {
			return failed("insert", p.Path, err)
		}
		written = append(written, p.Path)
	}
	if err := recordListHotPackages(ctx, tx, listID, key, written); err != nil {
		return 0, "", err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM package_list_entry WHERE package_list_id = ?`, listID).Scan(&found); err != nil {
		return 0, "", fmt.Errorf("package list difference: count: %w", err)
	}
	if found != want {
		return found, fmt.Sprintf("the list holds %d rows, want %d", found, want), nil
	}
	return found, "", nil
}
