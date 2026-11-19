package cache

import (
	"errors"
	"fmt"
	"maps"
	"math/rand"
	"slices"
	"sort"
	"strings"
	"testing"
)

// listRow is a package row for the difference tests: path, a hash made of
// one letter, a package name and version.
func listRow(path, letter, name, version string) PackageHash {
	return PackageHash{CanonicalScheme: "http", CanonicalHost: "h", Path: path,
		DeclaredSHA256: strings.Repeat(letter, 64), PackageName: name, Architecture: "amd64", Version: version}
}

// liveList yields rows in path order, as the live list is read, and counts
// the calls.
func liveList(rows []PackageHash) (next func() (PackageHash, bool, error), calls *int) {
	sorted := slices.Clone(rows)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	return unsortedList(sorted)
}

// unsortedList yields rows in the order given.
func unsortedList(rows []PackageHash) (next func() (PackageHash, bool, error), calls *int) {
	i, n := 0, 0
	return func() (PackageHash, bool, error) {
		n++
		if i == len(rows) {
			return PackageHash{}, false, nil
		}
		i++
		return rows[i-1], true, nil
	}, &n
}

func pathsOf(rows []PackageHash) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.Path)
	}
	return out
}

func TestDiffPackageRows_Cases(t *testing.T) {
	a := listRow("/pool/a.deb", "a", "a", "1")
	b := listRow("/pool/b.deb", "b", "b", "1")
	c := listRow("/pool/c.deb", "c", "c", "1")
	bHash := listRow("/pool/b.deb", "f", "b", "1")
	bName := listRow("/pool/b.deb", "b", "renamed", "1")
	bSnap := b
	bSnap.SnapshotID = 99
	for _, tc := range []struct {
		name                    string
		old, new                []PackageHash
		removed, changed, added []string
	}{
		{name: "both empty"},
		{name: "no live rows", new: []PackageHash{b, a}, added: []string{"/pool/a.deb", "/pool/b.deb"}},
		{name: "no new rows", old: []PackageHash{a, b}, removed: []string{"/pool/a.deb", "/pool/b.deb"}},
		{name: "identical", old: []PackageHash{a, b, c}, new: []PackageHash{c, b, a}},
		{name: "a hash changed", old: []PackageHash{a, b, c}, new: []PackageHash{a, bHash, c}, changed: []string{"/pool/b.deb"}},
		{name: "a name changed, the hash kept", old: []PackageHash{a, b}, new: []PackageHash{a, bName}, changed: []string{"/pool/b.deb"}},
		{name: "one removed, one added", old: []PackageHash{a, b}, new: []PackageHash{b, c},
			removed: []string{"/pool/a.deb"}, added: []string{"/pool/c.deb"}},
		{name: "only the snapshot id differs", old: []PackageHash{a, b}, new: []PackageHash{a, bSnap}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next, _ := liveList(tc.old)
			d, err := diffPackageRows(next, tc.new, -1)
			if err != nil {
				t.Fatal(err)
			}
			if d.TooLarge {
				t.Fatal("TooLarge with no limit")
			}
			if !slices.Equal(d.Removed, tc.removed) || !slices.Equal(pathsOf(d.Changed), tc.changed) || !slices.Equal(pathsOf(d.Added), tc.added) {
				t.Fatalf("removed %v changed %v added %v; want %v, %v, %v",
					d.Removed, pathsOf(d.Changed), pathsOf(d.Added), tc.removed, tc.changed, tc.added)
			}
			if want := len(tc.removed) + len(tc.changed) + len(tc.added); d.Size() != want {
				t.Fatalf("Size() = %d, want %d", d.Size(), want)
			}
		})
	}
	// A changed row is the new row, whole.
	next, _ := liveList([]PackageHash{a, b})
	d, err := diffPackageRows(next, []PackageHash{a, bName}, -1)
	if err != nil || len(d.Changed) != 1 || d.Changed[0] != bName {
		t.Fatalf("changed = %+v, %v; want the new row %+v", d.Changed, err, bName)
	}
}

// Whatever the two lists are, applying the difference to the live one gives
// exactly the new rows.
func TestDiffPackageRows_ApplyingItGivesTheNewRows(t *testing.T) {
	pool := []string{"/A", "/a", "/a+b", "/a~b", "/a.b", "/é", "/z"}
	for i := len(pool); i < 400; i++ {
		pool = append(pool, fmt.Sprintf("/pool/p%03d.deb", i))
	}
	letters := []string{"a", "b", "c"}
	names := []string{"x", "y"}
	versions := []string{"1", "2"}
	hosts := []string{"h", "other"}
	for seed := int64(1); seed <= 500; seed++ {
		rng := rand.New(rand.NewSource(seed))
		list := func() []PackageHash {
			n := rng.Intn(301)
			var out []PackageHash
			for _, i := range rng.Perm(len(pool))[:n] {
				r := listRow(pool[i], letters[rng.Intn(3)], names[rng.Intn(2)], versions[rng.Intn(2)])
				r.CanonicalHost = hosts[rng.Intn(20)/19] // rarely another host
				if rng.Intn(10) == 0 {
					r.FlatFilename = "f.deb"
				}
				out = append(out, r)
			}
			return out
		}
		old, want := list(), list()
		next, _ := liveList(old)
		d, err := diffPackageRows(next, want, -1)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		got := map[string]PackageHash{}
		for _, r := range old {
			got[r.Path] = r
		}
		for _, p := range d.Removed {
			if _, ok := got[p]; !ok {
				t.Fatalf("seed %d: removes %s, which the live list does not have", seed, p)
			}
			delete(got, p)
		}
		for _, r := range d.Changed {
			if _, ok := got[r.Path]; !ok {
				t.Fatalf("seed %d: changes %s, which the live list does not have", seed, r.Path)
			}
			got[r.Path] = r
		}
		for _, r := range d.Added {
			if _, ok := got[r.Path]; ok {
				t.Fatalf("seed %d: adds %s, which the live list has", seed, r.Path)
			}
			got[r.Path] = r
		}
		wantMap := map[string]PackageHash{}
		for _, r := range want {
			wantMap[r.Path] = r
		}
		if !maps.Equal(got, wantMap) {
			t.Fatalf("seed %d: the live list with the difference applied is not the new rows (%d rows, want %d)", seed, len(got), len(wantMap))
		}
		// Nothing is reported that did not change.
		for _, r := range d.Changed {
			for _, o := range old {
				if o.Path == r.Path && o == r {
					t.Fatalf("seed %d: %s is reported changed and is identical", seed, r.Path)
				}
			}
		}
	}
}

func TestDiffPackageRows_ADuplicatePathInTheNewRowsIsAnError(t *testing.T) {
	a := listRow("/pool/a.deb", "a", "a", "1")
	dup := listRow("/pool/a.deb", "b", "a", "2")
	for _, old := range [][]PackageHash{nil, {a}} {
		next, _ := liveList(old)
		_, err := diffPackageRows(next, []PackageHash{a, listRow("/pool/b.deb", "b", "b", "1"), dup}, -1)
		if !errors.Is(err, ErrDuplicatePackagePath) || !strings.Contains(err.Error(), "/pool/a.deb") {
			t.Fatalf("live rows %d: err = %v, want ErrDuplicatePackagePath naming /pool/a.deb", len(old), err)
		}
	}
}

func TestDiffPackageRows_ALiveListOutOfOrderIsAnError(t *testing.T) {
	a := listRow("/pool/a.deb", "a", "a", "1")
	b := listRow("/pool/b.deb", "b", "b", "1")
	for name, old := range map[string][]PackageHash{"descending": {b, a}, "repeated": {a, a}} {
		next, _ := unsortedList(old)
		if _, err := diffPackageRows(next, []PackageHash{a, b}, -1); err == nil {
			t.Fatalf("%s live list: no error", name)
		}
	}
}

func TestDiffPackageRows_ALiveListReadErrorIsReturned(t *testing.T) {
	boom := errors.New("boom")
	next := func() (PackageHash, bool, error) { return PackageHash{}, false, boom }
	if _, err := diffPackageRows(next, []PackageHash{listRow("/pool/a.deb", "a", "a", "1")}, -1); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the read's error", err)
	}
}

func TestDiffPackageRows_StopsOnceTheDifferenceIsOverTheLimit(t *testing.T) {
	var old, changed []PackageHash
	for i := 0; i < 100; i++ {
		p := fmt.Sprintf("/pool/p%03d.deb", i)
		old = append(old, listRow(p, "a", "p", "1"))
		changed = append(changed, listRow(p, "b", "p", "2"))
	}
	next, calls := liveList(old)
	d, err := diffPackageRows(next, changed, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !d.TooLarge || d.Removed != nil || d.Changed != nil || d.Added != nil {
		t.Fatalf("difference = %+v, want TooLarge and no rows", d)
	}
	if *calls > 12 {
		t.Fatalf("read %d live rows, want at most 12: it stops at the limit", *calls)
	}
	// Exactly at the limit is not too large.
	next, _ = liveList(old[:10])
	if d, err := diffPackageRows(next, changed[:10], 10); err != nil || d.TooLarge || len(d.Changed) != 10 {
		t.Fatalf("10 changed with limit 10: %+v, %v; want them all", d, err)
	}
}

func TestDiffPackageRows_DoesNotReorderItsInput(t *testing.T) {
	rows := []PackageHash{listRow("/pool/c.deb", "c", "c", "1"), listRow("/pool/a.deb", "a", "a", "1"), listRow("/pool/b.deb", "b", "b", "1")}
	before := slices.Clone(rows)
	next, _ := liveList(nil)
	if _, err := diffPackageRows(next, rows, -1); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rows, before) {
		t.Fatalf("the caller's rows were reordered: %v", pathsOf(rows))
	}
}
