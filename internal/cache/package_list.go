package cache

import (
	"errors"
	"fmt"
	"sort"
)

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
		d        PackageListDiff
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
				return PackageListDiff{TooLarge: true}, nil
			}
			if err := advance(); err != nil {
				return PackageListDiff{}, err
			}
		case !haveOld || rows[order[i]].Path < old.Path:
			d.Added = append(d.Added, rows[order[i]])
			if tooLarge() {
				return PackageListDiff{TooLarge: true}, nil
			}
			i++
		default: // the same path in both
			if !samePackageRow(old, rows[order[i]]) {
				d.Changed = append(d.Changed, rows[order[i]])
				if tooLarge() {
					return PackageListDiff{TooLarge: true}, nil
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
