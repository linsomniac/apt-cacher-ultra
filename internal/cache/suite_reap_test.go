package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
)

// adoptWith commits a snapshot of key's suite (its row must exist) whose
// members are path → content, with members["InRelease"] as its anchor,
// and returns the snapshot's id and each member's blob hash.
func adoptWith(t *testing.T, c *Cache, key SuiteKey, members map[string]string) (int64, map[string]string) {
	t.Helper()
	ctx := context.Background()
	hashes := map[string]string{}
	for p, content := range members {
		hashes[p] = seedBlob(t, c, content)
	}
	anchor := hashes["InRelease"]
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{CanonicalScheme: key.CanonicalScheme,
		CanonicalHost: key.CanonicalHost, SuitePath: key.SuitePath, InReleaseHash: &anchor})
	if err != nil {
		t.Fatal(err)
	}
	var rows []SnapshotMember
	for p, h := range hashes {
		rows = append(rows, SnapshotMember{SnapshotID: id, Path: p, BlobHash: h, DeclaredSHA256: h})
	}
	if err := c.CommitAdoption(ctx, id, rows, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	return id, hashes
}

// urlRowExists reports whether (scheme, host, path) has a url_path row.
func urlRowExists(t *testing.T, c *Cache, scheme, host, path string) bool {
	t.Helper()
	_, err := c.LookupURL(context.Background(), scheme, host, path)
	if err != nil && !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

// isTouched reports whether key has a touch-coalescing entry.
func isTouched(c *Cache, key SuiteKey) bool {
	c.touches.mu.Lock()
	defer c.touches.mu.Unlock()
	_, ok := c.touches.last[key]
	return ok
}

// Spec C9 step 2 (ruling R-F1): ReapIdleSuite is deleteSuiteTx's
// compare-and-delete — kept (neither deleted nor gone) when the suite's
// current snapshot is not the one listed or a client asked for it after
// the cutoff — and on delete it releases the current snapshot's pins once,
// retires every snapshot of the suite (displaced and never-adopted ones
// too) and drops the touch entry. ReapRegardless deletes whatever the
// interest stamp says, NULL or future.
func TestReapIdleSuite_RetiresEverySnapshotAndDeletesTheRow(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, evictNow)()
	key := sk("h", "/r/dists/x")
	seedRow(t, c, key, evictNow-40*evictDay)
	old, _ := adoptWith(t, c, key, map[string]string{"InRelease": "v1", "main/Packages": "pkgs v1"})
	cur, hashes := adoptWith(t, c, key, map[string]string{"InRelease": "v2", "main/Packages": "pkgs v2"})
	next := seedBlob(t, c, "v3")
	cand, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h",
		SuitePath: key.SuitePath, InReleaseHash: &next})
	if err != nil {
		t.Fatal(err)
	}
	cutoff := evictNow - 30*evictDay
	if del, gone, err := c.ReapIdleSuite(ctx, key, &old, cutoff); err != nil || del || gone {
		t.Fatalf("another current snapshot listed: deleted %v, gone %v, err %v; want kept", del, gone, err)
	}
	if del, gone, err := c.ReapIdleSuite(ctx, key, &cur, evictNow-41*evictDay); err != nil || del || gone {
		t.Fatalf("requested after the cutoff: deleted %v, gone %v, err %v; want kept", del, gone, err)
	}
	if n := blobRefcount(t, c, hashes["main/Packages"]); n != 1 {
		t.Fatalf("a kept suite's current member has refcount %d, want its pin (1)", n)
	}
	c.noteTouch(key)
	if del, gone, err := c.ReapIdleSuite(ctx, key, &cur, cutoff); err != nil || !del || !gone {
		t.Fatalf("ReapIdleSuite = %v, %v, %v; want deleted and gone", del, gone, err)
	}
	if rowExists(t, c, key) || isTouched(c, key) {
		t.Fatalf("after the reap: row %v, touch entry %v; want neither", rowExists(t, c, key), isTouched(c, key))
	}
	for _, id := range []int64{old, cur, cand} {
		if snap, err := c.GetSuiteSnapshot(ctx, id); err != nil || snap.RetiredAt == nil {
			t.Fatalf("snapshot %d = %+v, %v; want retired", id, snap, err)
		}
	}
	if n := blobRefcount(t, c, hashes["main/Packages"]); n != 0 {
		t.Fatalf("the current member's refcount = %d, want its pin released once (0)", n)
	}
	for name, stamp := range map[string]sql.NullInt64{"NULL stamp": {}, "future stamp": {Int64: evictNow + evictDay, Valid: true}} {
		k := sk("h", "/r/dists/"+strings.Fields(name)[0])
		seedRow(t, c, k, evictNow)
		if _, err := c.db.Exec(`UPDATE suite_freshness SET last_requested_at = ? WHERE suite_path = ?`, stamp, k.SuitePath); err != nil {
			t.Fatal(err)
		}
		if del, gone, err := c.ReapIdleSuite(ctx, k, nil, evictNow); err != nil || del || gone {
			t.Fatalf("%s, idle cutoff: deleted %v, gone %v, err %v; want kept", name, del, gone, err)
		}
		if del, gone, err := c.ReapIdleSuite(ctx, k, nil, ReapRegardless); err != nil || !del || !gone {
			t.Fatalf("%s, ReapRegardless: deleted %v, gone %v, err %v; want deleted", name, del, gone, err)
		}
	}
}

// Spec C9 step 1: DeleteSuiteMetadata deletes exactly the suite's metadata
// url_path rows — the paths whose suite is this one — and never a nested
// suite's, a sibling sharing its prefix, a .deb, the /. twin's path, or the
// same path under the other scheme or another host. A second call deletes
// nothing.
func TestDeleteSuiteMetadata_DeletesOnlyTheSuitesRows(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/r/dists/x")
	blob := seedBlob(t, c, "metadata")
	put := func(scheme, host, path string) {
		putURLPathRow(t, c, scheme, host, path, blob, sql.NullInt64{Int64: 1, Valid: true}, true)
	}
	mine := []string{"/r/dists/x/InRelease", "/r/dists/x/Release", "/r/dists/x/Release.gpg",
		"/r/dists/x/main/binary-amd64/Packages.gz", "/r/dists/x/main/binary-amd64/by-hash/SHA256/" + strings.Repeat("ab", 32)}
	others := []string{
		"/r/dists/x/dists/y/InRelease",       // a nested suite's: SuitePath takes the last dists
		"/r/dists/xy/InRelease",              // a sibling sharing the prefix
		"/r/pool/main/f/foo/foo_1_amd64.deb", // no suite's metadata
		"/r/dists/x/./InRelease",             // the /. twin's path, not this suite's
	}
	for _, p := range append(append([]string{}, mine...), others...) {
		put("http", "h", p)
	}
	put("https", "h", "/r/dists/x/InRelease")
	put("http", "other", "/r/dists/x/InRelease")
	if n, err := c.DeleteSuiteMetadata(ctx, key); err != nil || n != len(mine) {
		t.Fatalf("DeleteSuiteMetadata = %d, %v; want the suite's %d rows", n, err, len(mine))
	}
	for _, p := range mine {
		if urlRowExists(t, c, "http", "h", p) {
			t.Errorf("%s was not deleted", p)
		}
	}
	for _, p := range others {
		if !urlRowExists(t, c, "http", "h", p) {
			t.Errorf("%s was deleted, want it kept", p)
		}
	}
	if !urlRowExists(t, c, "https", "h", "/r/dists/x/InRelease") || !urlRowExists(t, c, "http", "other", "/r/dists/x/InRelease") {
		t.Error("the other scheme's or another host's row was deleted")
	}
	if n, err := c.DeleteSuiteMetadata(ctx, key); err != nil || n != 0 {
		t.Fatalf("a second call = %d, %v; want nothing left to delete", n, err)
	}
}

// Design note 7 (the Task 3 review's Important 1): a key no suite path
// matches — the empty path, a non-canonical one, a /. twin before phase G
// — has no metadata rows. DeleteSuiteMetadata deletes nothing for it and
// leaves the host's pool .deb, its flat-repo file, a canonical suite's
// anchor and a row under the key's own prefix in place (a non-canonical
// row's cached files are url_path GC's: metadata_rows 0). Before the fix
// the empty path's range spanned the host and deleted the .deb and the
// flat-repo file, whose suite path is "" too.
func TestDeleteSuiteMetadata_ANonCanonicalKeyDeletesNothing(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	blob := seedBlob(t, c, "cached bytes")
	paths := []string{"/debian/pool/main/f/foo/foo_1_amd64.deb", "/flat/Packages", "/debian/dists/x/InRelease",
		"/a/dists/b/c/InRelease", "/flat/./Packages"}
	for _, p := range paths {
		putURLPathRow(t, c, "http", "h", p, blob, sql.NullInt64{Int64: 1, Valid: true}, false)
	}
	for _, suite := range []string{"", "/a/dists/b/c", "/flat/."} {
		if n, err := c.DeleteSuiteMetadata(ctx, sk("h", suite)); err != nil || n != 0 {
			t.Errorf("DeleteSuiteMetadata(%q) = %d, %v; want nothing deleted", suite, n, err)
		}
	}
	for _, p := range paths {
		if !urlRowExists(t, c, "http", "h", p) {
			t.Errorf("%s was deleted, want it kept", p)
		}
	}
}

// Spec C9 step 1: the rows go in writer transactions of at most 1,000,
// plain metadata first and the anchors last. A failure in the second batch
// leaves the first batch deleted and the anchors in place; the next call
// deletes the rest.
func TestDeleteSuiteMetadata_BatchesOfAThousandAnchorsLast(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/r/dists/x")
	blob := seedBlob(t, c, "index")
	tx, err := c.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	insert := func(p string) {
		if _, err := tx.Exec(`INSERT INTO url_path (canonical_scheme, canonical_host, path, blob_hash, upstream_url, is_metadata)
VALUES ('http', 'h', ?, ?, ?, 1)`, p, blob, "http://h"+p); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 1200 {
		insert(fmt.Sprintf("/r/dists/x/main/i%04d", i))
	}
	anchors := []string{"/r/dists/x/InRelease", "/r/dists/x/Release", "/r/dists/x/Release.gpg"}
	for _, p := range anchors {
		insert(p)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`CREATE TRIGGER fail_one BEFORE DELETE ON url_path WHEN OLD.path = '/r/dists/x/main/i1100'
BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if n, err := c.DeleteSuiteMetadata(ctx, key); err == nil || n != 1000 {
		t.Fatalf("with the second batch failing: %d deleted, err %v; want the first batch's 1,000 and an error", n, err)
	}
	for _, p := range anchors {
		if !urlRowExists(t, c, "http", "h", p) {
			t.Fatalf("%s was deleted before the plain metadata was gone", p)
		}
	}
	if _, err := c.db.Exec(`DROP TRIGGER fail_one`); err != nil {
		t.Fatal(err)
	}
	if n, err := c.DeleteSuiteMetadata(ctx, key); err != nil || n != 203 {
		t.Fatalf("the next call = %d, %v; want the 200 plain rows and 3 anchors left", n, err)
	}
}

// Spec §7 C9: batched metadata deletion never repeats a refcount
// decrement. A url_path row releases its blob's pin once, when it is
// deleted; the current snapshot's pins are released once, by the final
// step that deletes the row — not by a final step that finds the suite
// requested again, and not again by the next run.
func TestDeleteSuiteMetadata_NeverRepeatsADecrement(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, evictNow)()
	key := sk("h", "/r/dists/x")
	seedRow(t, c, key, evictNow-40*evictDay)
	cur, hashes := adoptWith(t, c, key, map[string]string{"InRelease": "anchor", "main/Packages": "pkgs", "main/Sources": "srcs"})
	// main/Packages is also cached in url_path (a client's miss), with the
	// pin its row holds; main/Sources only in the snapshot.
	putURLPathRow(t, c, "http", "h", "/r/dists/x/main/Packages", hashes["main/Packages"], sql.NullInt64{}, true)
	pkgs, srcs := hashes["main/Packages"], hashes["main/Sources"]
	if _, err := c.db.Exec(`UPDATE blob SET refcount = refcount + 1 WHERE hash = ?`, pkgs); err != nil {
		t.Fatal(err)
	}
	if p, s := blobRefcount(t, c, pkgs), blobRefcount(t, c, srcs); p != 2 || s != 1 {
		t.Fatalf("premise: Packages %d, Sources %d; want 2 (the snapshot's pin and its url_path row's) and 1", p, s)
	}
	cutoff := evictNow - 30*evictDay
	if _, err := c.DeleteSuiteMetadata(ctx, key); err != nil {
		t.Fatal(err)
	}
	if del, _, err := c.ReapIdleSuite(ctx, key, &cur, evictNow-41*evictDay); err != nil || del {
		t.Fatalf("a final step that finds the suite requested again: deleted %v, err %v; want kept", del, err)
	}
	if p, s := blobRefcount(t, c, pkgs), blobRefcount(t, c, srcs); p != 1 || s != 1 {
		t.Fatalf("after a kept reap: Packages %d, Sources %d; want 1 (its url_path row's pin released) and 1 (untouched)", p, s)
	}
	if n, err := c.DeleteSuiteMetadata(ctx, key); err != nil || n != 0 {
		t.Fatalf("the next run's batches = %d, %v; want nothing deleted twice", n, err)
	}
	if del, _, err := c.ReapIdleSuite(ctx, key, &cur, cutoff); err != nil || !del {
		t.Fatalf("the final step = %v, %v; want deleted", del, err)
	}
	if p, s := blobRefcount(t, c, pkgs), blobRefcount(t, c, srcs); p != 0 || s != 0 {
		t.Fatalf("after the reap: Packages %d, Sources %d; want each pin released once (0, 0)", p, s)
	}
}

// Spec C9 (D9) and the phase E carry-forward: ForgetSuite deletes the
// suite whatever its interest stamp and, only when no other suite of the
// host has a row for its base or its /. twin under either scheme, the
// base's signed-seen markers under both schemes and the cached anchor rows
// of the base and the twin under both schemes (ruling PF-2); otherwise it
// names the rows that kept them (ruling PF-7). Another host's suite of the
// same base does not keep them, and another host's anchor row stays.
func TestForgetSuite_DeletesTheBaseMarkersOnlyWithoutAnotherSuite(t *testing.T) {
	for _, tc := range []struct {
		name      string
		forgotten string    // the forgotten suite on http://h; "" is /flat
		other     *SuiteKey // another suite row, besides the forgotten one
		markers   int       // markers deleted
	}{
		{name: "no other suite", markers: 2},
		{name: "the /. twin keeps them", other: &SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/flat/."}},
		{name: "the other scheme's suite keeps them", other: &SuiteKey{CanonicalScheme: "https", CanonicalHost: "h", SuitePath: "/flat"}},
		{name: "the other scheme's twin keeps them", other: &SuiteKey{CanonicalScheme: "https", CanonicalHost: "h", SuitePath: "/flat/."}},
		{name: "another host's suite does not", other: &SuiteKey{CanonicalScheme: "http", CanonicalHost: "g", SuitePath: "/flat"}, markers: 2},
		// The Task 3 review's minor 1: forgetting the twin while its base
		// is tracked keeps the base's markers and anchors under both
		// schemes (the keeper read is of the base, not the twin's path).
		{name: "the base keeps a forgotten twin's", forgotten: "/flat/.",
			other: &SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/flat"}},
		{name: "the other scheme's base keeps a forgotten twin's", forgotten: "/flat/.",
			other: &SuiteKey{CanonicalScheme: "https", CanonicalHost: "h", SuitePath: "/flat"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := openCache(t)
			ctx := context.Background()
			key := sk("h", "/flat")
			if tc.forgotten != "" {
				key = sk("h", tc.forgotten)
			}
			seedRow(t, c, key, evictNow+evictDay) // requested in the future: forgotten anyway
			if tc.other != nil {
				seedRow(t, c, *tc.other, evictNow)
			}
			marks := []SignedSeenMark{{CanonicalScheme: "http", CanonicalHost: "h", Base: "/flat", SeenAt: 1},
				{CanonicalScheme: "https", CanonicalHost: "h", Base: "/flat", SeenAt: 1},
				{CanonicalScheme: "http", CanonicalHost: "g", Base: "/flat", SeenAt: 1}}
			if err := c.MarkSignedSeenBatch(ctx, marks); err != nil {
				t.Fatal(err)
			}
			// Signed anchors cached under the other scheme and at the twin
			// path, with no row of their own, and on another host.
			blob := seedBlob(t, c, "signed anchor")
			anchors := [][2]string{{"https", "/flat/InRelease"}, {"http", "/flat/./Release.gpg"}}
			for _, a := range anchors {
				putURLPathRow(t, c, a[0], "h", a[1], blob, sql.NullInt64{Int64: 1, Valid: true}, true)
			}
			putURLPathRow(t, c, "http", "g", "/flat/InRelease", blob, sql.NullInt64{Int64: 1, Valid: true}, true)
			c.noteTouch(key)
			res, err := c.ForgetSuite(ctx, key, nil)
			wantAnchors, wantKept := 0, []SuiteKey(nil)
			if tc.markers > 0 {
				wantAnchors = len(anchors)
			} else {
				wantKept = []SuiteKey{*tc.other}
			}
			if err != nil || !res.Deleted || !res.Gone || res.MarkersDeleted != tc.markers || res.AnchorRowsDeleted != wantAnchors ||
				!slices.Equal(res.KeptBy, wantKept) {
				t.Fatalf("ForgetSuite = %+v, %v; want deleted with %d markers, %d anchor rows, kept by %v", res, err, tc.markers,
					wantAnchors, wantKept)
			}
			for _, a := range anchors {
				if urlRowExists(t, c, a[0], "h", a[1]) == (tc.markers > 0) {
					t.Errorf("anchor row %s://h%s present %v, want %v", a[0], a[1], tc.markers == 0, tc.markers == 0)
				}
			}
			if !urlRowExists(t, c, "http", "g", "/flat/InRelease") {
				t.Error("another host's anchor row was deleted")
			}
			if rowExists(t, c, key) || isTouched(c, key) {
				t.Fatal("the forgotten suite's row or touch entry stayed")
			}
			for _, m := range marks {
				_, ok, err := c.SignedSeen(ctx, m.CanonicalScheme, m.CanonicalHost, m.Base)
				if err != nil {
					t.Fatal(err)
				}
				gone := m.CanonicalHost == "h" && tc.markers > 0
				if ok == gone {
					t.Errorf("marker %s://%s%s present %v, want %v", m.CanonicalScheme, m.CanonicalHost, m.Base, ok, !gone)
				}
			}
			if tc.other != nil && !rowExists(t, c, *tc.other) {
				t.Fatal("the other suite's row was deleted")
			}
		})
	}
	t.Run("an unknown suite", func(t *testing.T) {
		c := openCache(t)
		ctx := context.Background()
		if err := c.MarkSignedSeenBatch(ctx, []SignedSeenMark{{CanonicalScheme: "http", CanonicalHost: "h", Base: "/flat", SeenAt: 1}}); err != nil {
			t.Fatal(err)
		}
		if res, err := c.ForgetSuite(ctx, sk("h", "/flat"), nil); err != nil || res.Deleted || !res.Gone || res.MarkersDeleted != 0 {
			t.Fatalf("ForgetSuite = %+v, %v; want nothing deleted, and the suite gone (it has no row)", res, err)
		}
		if _, ok, err := c.SignedSeen(ctx, "http", "h", "/flat"); err != nil || !ok {
			t.Fatalf("the marker of a suite with no row was deleted (%v, %v)", ok, err)
		}
	})
	// The Task 3 review's minor 2: a forget whose compare-and-delete keeps
	// the row (adopted since the caller read it) is neither deleted nor
	// gone — the caller releases its hold with release(false) — and
	// deletes no marker.
	t.Run("a suite adopted since the row read", func(t *testing.T) {
		c := openCache(t)
		ctx := context.Background()
		key := sk("h", "/flat")
		seedRow(t, c, key, evictNow)
		adoptRow(t, c, key)
		if err := c.MarkSignedSeenBatch(ctx, []SignedSeenMark{{CanonicalScheme: "http", CanonicalHost: "h", Base: "/flat", SeenAt: 1}}); err != nil {
			t.Fatal(err)
		}
		c.noteTouch(key)
		if res, err := c.ForgetSuite(ctx, key, nil); err != nil || res.Deleted || res.Gone || res.MarkersDeleted != 0 {
			t.Fatalf("ForgetSuite = %+v, %v; want the row kept: neither deleted nor gone, no marker deleted", res, err)
		}
		if !rowExists(t, c, key) || !isTouched(c, key) {
			t.Fatal("the kept suite's row or touch entry was dropped")
		}
		if _, ok, err := c.SignedSeen(ctx, "http", "h", "/flat"); err != nil || !ok {
			t.Fatalf("a kept suite's marker was deleted (%v, %v)", ok, err)
		}
	})
}

// Spec §7 C9 "twin and nested suites untouched": reaping /r/dists/x and
// forgetting /flat leave the nested suite /r/dists/x/dists/y and the /.
// twin /flat/. as they were — rows, current snapshots, url_path rows and
// the shared base's markers.
func TestReap_LeavesTwinsAndNestedSuitesAlone(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, evictNow)()
	reaped, nested := sk("h", "/r/dists/x"), sk("h", "/r/dists/x/dists/y")
	forgotten, twin := sk("h", "/flat"), sk("h", "/flat/.")
	ids := map[SuiteKey]int64{}
	for _, k := range []SuiteKey{reaped, nested, forgotten, twin} {
		seedRow(t, c, k, evictNow-40*evictDay)
		ids[k], _ = adoptWith(t, c, k, map[string]string{"InRelease": "anchor of " + k.SuitePath})
		putURLPathRow(t, c, "http", "h", k.SuitePath+"/main/Packages", seedBlob(t, c, "pkgs of "+k.SuitePath), sql.NullInt64{}, true)
	}
	if err := c.MarkSignedSeenBatch(ctx, []SignedSeenMark{{CanonicalScheme: "http", CanonicalHost: "h", Base: "/flat", SeenAt: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteSuiteMetadata(ctx, reaped); err != nil {
		t.Fatal(err)
	}
	cur := ids[reaped]
	if del, _, err := c.ReapIdleSuite(ctx, reaped, &cur, evictNow-30*evictDay); err != nil || !del {
		t.Fatalf("reap %v = %v, %v", reaped, del, err)
	}
	if _, err := c.DeleteSuiteMetadata(ctx, forgotten); err != nil {
		t.Fatal(err)
	}
	cur = ids[forgotten]
	if res, err := c.ForgetSuite(ctx, forgotten, &cur); err != nil || !res.Deleted || res.MarkersDeleted != 0 ||
		!slices.Equal(res.KeptBy, []SuiteKey{twin}) {
		t.Fatalf("forget %v = %+v, %v; want deleted, the twin keeping the base's marker", forgotten, res, err)
	}
	for _, k := range []SuiteKey{nested, twin} {
		sf, err := c.GetSuiteFreshness(ctx, k.CanonicalScheme, k.CanonicalHost, k.SuitePath)
		if err != nil || sf.CurrentSnapshotID == nil || *sf.CurrentSnapshotID != ids[k] {
			t.Fatalf("%v: row %+v, %v; want it and its current snapshot kept", k, sf, err)
		}
		if snap, err := c.GetSuiteSnapshot(ctx, ids[k]); err != nil || snap.RetiredAt != nil {
			t.Fatalf("%v: snapshot %+v, %v; want it not retired", k, snap, err)
		}
		for _, p := range []string{k.SuitePath + "/InRelease", k.SuitePath + "/main/Packages"} {
			if !urlRowExists(t, c, "http", "h", p) {
				t.Fatalf("%s was deleted", p)
			}
		}
	}
	if _, ok, err := c.SignedSeen(ctx, "http", "h", "/flat"); err != nil || !ok {
		t.Fatalf("the twin's base marker = %v, %v; want kept", ok, err)
	}
}

// Spec C9 step 2 (the Step-8 decrement, once): a reap releases the pins of
// the suite's current snapshot only. A displaced snapshot's pins went when
// it was displaced; releasing them again would drive its blobs' refcounts
// below the references they still have (exec review X14).
func TestReapIdleSuite_ReleasesOnlyTheCurrentSnapshotsPins(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, evictNow)()
	key := sk("h", "/r/dists/x")
	seedRow(t, c, key, evictNow-40*evictDay)
	_, old := adoptWith(t, c, key, map[string]string{"InRelease": "v1", "main/Packages": "pkgs v1"})
	cur, hashes := adoptWith(t, c, key, map[string]string{"InRelease": "v2", "main/Packages": "pkgs v2"})
	if n := blobRefcount(t, c, old["main/Packages"]); n != 0 {
		t.Fatalf("premise: the displaced snapshot's member has refcount %d, want 0 (released at displacement)", n)
	}
	if del, _, err := c.ReapIdleSuite(ctx, key, &cur, evictNow-30*evictDay); err != nil || !del {
		t.Fatalf("ReapIdleSuite = %v, %v", del, err)
	}
	if o, n := blobRefcount(t, c, old["main/Packages"]), blobRefcount(t, c, hashes["main/Packages"]); o != 0 || n != 0 {
		t.Fatalf("refcounts after the reap: displaced member %d, current member %d; want 0 and 0", o, n)
	}
}

// Design note 13 (the E carry-forward's base and twin rule): forgetting a
// "/." twin deletes the markers of its base — the key every suite of the
// base shares (aptpath.MarkerBase) — under both schemes, when no other row
// of the base or the twin is left (exec review X15).
func TestForgetSuite_ATwinPathDeletesItsBasesMarkers(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	twin := sk("h", "/flat/.")
	seedRow(t, c, twin, evictNow)
	marks := []SignedSeenMark{{CanonicalScheme: "http", CanonicalHost: "h", Base: "/flat", SeenAt: 1},
		{CanonicalScheme: "https", CanonicalHost: "h", Base: "/flat", SeenAt: 1}}
	if err := c.MarkSignedSeenBatch(ctx, marks); err != nil {
		t.Fatal(err)
	}
	res, err := c.ForgetSuite(ctx, twin, nil)
	if err != nil || !res.Deleted || res.MarkersDeleted != 2 {
		t.Fatalf("ForgetSuite(/flat/.) = %+v, %v; want deleted with the base's 2 markers", res, err)
	}
	for _, m := range marks {
		if _, ok, err := c.SignedSeen(ctx, m.CanonicalScheme, m.CanonicalHost, m.Base); err != nil || ok {
			t.Fatalf("marker %s://h/flat survived (%v, %v)", m.CanonicalScheme, ok, err)
		}
	}
}

// Design review M-9: a reap moves the base's signed-seen markers, under
// both schemes, up to the reap (MAX(last_seen_at, now)), so a reaped
// suite's marker ages from the end of its tracking, not from its last
// sighting. A stamp ahead of now is never lowered (ruling PF-1), another
// host's marker is left alone, a twin's reap stamps its base's markers,
// and a reap that keeps the suite stamps nothing.
func TestReapIdleSuite_MovesTheBaseMarkersUpToTheReap(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, evictNow)()
	key, twin := sk("h", "/r/dists/x"), sk("h", "/flat/.")
	seedRow(t, c, key, evictNow-40*evictDay)
	seedRow(t, c, twin, evictNow-40*evictDay)
	old, ahead := evictNow-100*evictDay, evictNow+evictDay
	marks := []SignedSeenMark{{CanonicalScheme: "http", CanonicalHost: "h", Base: "/r/dists/x", SeenAt: old},
		{CanonicalScheme: "https", CanonicalHost: "h", Base: "/r/dists/x", SeenAt: ahead},
		{CanonicalScheme: "http", CanonicalHost: "g", Base: "/r/dists/x", SeenAt: old},
		{CanonicalScheme: "https", CanonicalHost: "h", Base: "/flat", SeenAt: old}}
	if err := c.MarkSignedSeenBatch(ctx, marks); err != nil {
		t.Fatal(err)
	}
	stamp := func(m SignedSeenMark) int64 {
		t.Helper()
		at, ok, err := c.SignedSeen(ctx, m.CanonicalScheme, m.CanonicalHost, m.Base)
		if err != nil || !ok {
			t.Fatalf("marker %+v: %v, %v", m, ok, err)
		}
		return at
	}
	if del, _, err := c.ReapIdleSuite(ctx, key, nil, evictNow-41*evictDay); err != nil || del {
		t.Fatalf("a kept reap = %v, %v", del, err)
	}
	if got := stamp(marks[0]); got != old {
		t.Fatalf("a kept reap stamped the marker %d, want it unchanged (%d)", got, old)
	}
	for _, k := range []SuiteKey{key, twin} {
		if del, _, err := c.ReapIdleSuite(ctx, k, nil, evictNow-30*evictDay); err != nil || !del {
			t.Fatalf("reap %v = %v, %v", k, del, err)
		}
	}
	for i, want := range []int64{evictNow, ahead, old, evictNow} {
		if got := stamp(marks[i]); got != want {
			t.Errorf("marker %+v: stamp %d after the reaps, want %d", marks[i], got, want)
		}
	}
}

// Design review M-2: a reap or a forget whose submit fails although its
// transaction committed — the caller's context ends right after the commit
// — is settled by a fresh read of the row, as afterCreate settles a
// creation: the suite is reported gone (its holder releases it with
// release(true)) and its touch entry is dropped. One whose transaction
// rolled back is not gone.
func TestReapAndForget_AFailedSubmitWhoseDeleteCommittedIsGone(t *testing.T) {
	for _, op := range []string{"reap", "forget"} {
		t.Run(op, func(t *testing.T) {
			defer stubNow(t, evictNow)()
			c := openCache(t)
			key := sk("h", "/r/dists/x")
			seedRow(t, c, key, evictNow-40*evictDay)
			c.noteTouch(key)
			ctx, cancel := context.WithCancel(context.Background())
			resume := make(chan struct{})
			var once sync.Once
			c.committedHook = func() { once.Do(func() { cancel(); <-resume }) }
			var gone bool
			var err error
			if op == "reap" {
				_, gone, err = c.ReapIdleSuite(ctx, key, nil, evictNow-30*evictDay)
			} else {
				var res ForgetResult
				res, err = c.ForgetSuite(ctx, key, nil)
				gone = res.Gone
			}
			close(resume)
			if err == nil || rowExists(t, c, key) {
				t.Fatalf("premise: the submit fails (%v) on a context that ended after the commit, which deleted the row", err)
			}
			if !gone || isTouched(c, key) {
				t.Fatalf("gone %v, touch entry %v; want the suite settled gone and its touch entry dropped", gone, isTouched(c, key))
			}
		})
	}
	for _, op := range []string{"reap", "forget"} {
		t.Run("rolled back "+op, func(t *testing.T) {
			defer stubNow(t, evictNow)()
			c := openCache(t)
			key := sk("h", "/r/dists/x")
			seedRow(t, c, key, evictNow-40*evictDay)
			c.noteTouch(key)
			if _, err := c.db.Exec(`CREATE TRIGGER fail_delete BEFORE DELETE ON suite_freshness
BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
				t.Fatal(err)
			}
			var del, gone bool
			var err error
			if op == "reap" {
				del, gone, err = c.ReapIdleSuite(context.Background(), key, nil, evictNow-30*evictDay)
			} else {
				var res ForgetResult
				res, err = c.ForgetSuite(context.Background(), key, nil)
				del, gone = res.Deleted, res.Gone
			}
			if err == nil || del || gone || !isTouched(c, key) {
				t.Fatalf("%s = deleted %v, gone %v, err %v, touch entry %v; want an error, the suite neither deleted nor gone, "+
					"its touch entry kept", op, del, gone, err, isTouched(c, key))
			}
		})
	}
}
