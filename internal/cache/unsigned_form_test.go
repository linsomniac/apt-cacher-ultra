package cache

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// seedFormSnapshot inserts a suite row and an adopted snapshot of form
// (single holds the inline or unsigned anchor hash, release and gpg the
// detached pair), points the suite at it, and returns its id. Raw SQL:
// tests build forms adoption cannot reach here.
func seedFormSnapshot(t *testing.T, c *Cache, scheme, host, suite string, form AnchorForm, single, release, gpg *string, coverage bool) int64 {
	t.Helper()
	if _, err := c.db.Exec(`INSERT INTO suite_freshness (canonical_scheme, canonical_host, suite_path) VALUES (?, ?, ?)`,
		scheme, host, suite); err != nil {
		t.Fatal(err)
	}
	cov := 0
	if coverage {
		cov = 1
	}
	res, err := c.db.Exec(`INSERT INTO suite_snapshot
  (canonical_scheme, canonical_host, suite_path, inrelease_hash, release_hash, release_gpg_hash,
   anchor_form, created_at, adopted_at, heartbeat_at, package_coverage_complete)
  VALUES (?, ?, ?, ?, ?, ?, ?, 1, 1, 1, ?)`, scheme, host, suite, single, release, gpg, string(form), cov)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	if _, err := c.db.Exec(`UPDATE suite_freshness SET current_snapshot_id = ?
  WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`, id, scheme, host, suite); err != nil {
		t.Fatal(err)
	}
	return id
}

// Spec C5: an unsigned snapshot's Release hash sits in the single-anchor
// slot, so a snapshot names its anchors' hashes through its form.
func TestSuiteSnapshot_AnchorHash(t *testing.T) {
	single, rel, gpg := "single", "release", "gpg"
	for _, c := range []struct {
		form AnchorForm
		name string
		want *string
	}{
		{FormInline, "InRelease", &single},
		{FormInline, "Release", nil},
		{FormInline, "Release.gpg", nil},
		{FormDetached, "InRelease", nil},
		{FormDetached, "Release", &rel},
		{FormDetached, "Release.gpg", &gpg},
		{FormUnsigned, "InRelease", nil},
		{FormUnsigned, "Release", &single},
		{FormUnsigned, "Release.gpg", nil},
		{"", "InRelease", nil},
	} {
		s := SuiteSnapshot{Form: c.form, SingleAnchorHash: &single, ReleaseHash: &rel, ReleaseGPGHash: &gpg}
		got := s.AnchorHash(c.name)
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("%s snapshot: AnchorHash(%q) = %v, want %v", c.form, c.name, derefS(got), derefS(c.want))
		}
	}
}

func derefS(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// Spec C5 / overview E carry-forward: CommitAdoption Step 3c seeds an
// unsigned snapshot's <suite>/Release anchor — never <suite>/InRelease —
// and an unsigned commit writes no signed-seen marker.
func TestCommitAdoption_UnsignedSeedsItsReleaseAnchor(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, 1_800_000_000)()
	const suite = "/u/dists/s"
	rel := seedBlob(t, c, "Origin: u\nSHA256:\n")
	id, _, err := c.InsertTrackedCandidateForTest(ctx, SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h",
		SuitePath: suite, InReleaseHash: &rel, Form: FormUnsigned})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id, []SnapshotMember{{SnapshotID: id, Path: "Release", BlobHash: rel, DeclaredSHA256: rel}},
		nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	row, err := c.LookupURL(ctx, "http", "h", suite+"/Release")
	if err != nil || row.BlobHash == nil || *row.BlobHash != rel {
		t.Fatalf("Release anchor row = %+v, %v; want the snapshot's Release (%s)", row, err, rel)
	}
	if _, err := c.LookupURL(ctx, "http", "h", suite+"/InRelease"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unsigned snapshot seeded an InRelease anchor (err %v)", err)
	}
	if _, ok, err := c.SignedSeen(ctx, "http", "h", suite); err != nil || ok {
		t.Fatalf("an unsigned commit wrote a signed-seen marker (ok %v, err %v)", ok, err)
	}
	if snap, err := c.GetSuiteSnapshot(ctx, id); err != nil || snap.Form != FormUnsigned || snap.AdoptedAt == nil {
		t.Fatalf("snapshot = %+v, %v; want an adopted unsigned snapshot", snap, err)
	}
}

// Spec C5 safeguard 3, the second point: CommitAdoption re-reads the
// marker for the candidate's base inside its transaction (the /. twin
// shares it) and refuses an unsigned candidate — no pointer change, no
// rows — when one exists or the suite's current snapshot is signed. A
// signed candidate, and an unsigned one over an unsigned current snapshot,
// commit.
func TestCommitAdoption_RefusesAnUnsignedDowngrade(t *testing.T) {
	for _, tc := range []struct {
		name, suite, markerBase string
		prior                   AnchorForm // "" for none
		candidate               AnchorForm
		refused                 bool
	}{
		{"marker for the base", "/u/dists/s", "/u/dists/s", "", FormUnsigned, true},
		{"marker for the twin's base", "/flat/.", "/flat", "", FormUnsigned, true},
		{"signed current snapshot", "/u/dists/s", "", FormInline, FormUnsigned, true},
		{"detached current snapshot", "/u/dists/s", "", FormDetached, FormUnsigned, true},
		{"unsigned current snapshot", "/u/dists/s", "", FormUnsigned, FormUnsigned, false},
		{"no history", "/u/dists/s", "", "", FormUnsigned, false},
		{"signed candidate with a marker", "/u/dists/s", "/u/dists/s", "", FormInline, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := openCache(t)
			ctx := context.Background()
			defer stubNow(t, 1_800_000_000)()
			var priorID int64
			if tc.prior != "" {
				h := seedBlob(t, c, "prior anchor")
				g := seedBlob(t, c, "prior signature")
				switch tc.prior {
				case FormDetached:
					priorID = seedFormSnapshot(t, c, "http", "h", tc.suite, tc.prior, nil, &h, &g, true)
				default:
					priorID = seedFormSnapshot(t, c, "http", "h", tc.suite, tc.prior, &h, nil, nil, true)
				}
			} else if err := c.EnsureSuiteForTest(ctx, "http", "h", tc.suite); err != nil {
				t.Fatal(err)
			}
			if tc.markerBase != "" {
				if err := c.MarkSignedSeenBatch(ctx, []SignedSeenMark{{CanonicalScheme: "http", CanonicalHost: "h",
					Base: tc.markerBase, SeenAt: 1}}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.candidate == FormUnsigned {
				// The early exits read the same rule (CheckUnsignedDowngrade).
				if early := c.CheckUnsignedDowngrade(ctx, "h", tc.suite); errors.Is(early, ErrUnsignedDowngradeRefused) != tc.refused {
					t.Fatalf("CheckUnsignedDowngrade = %v, want refused %v", early, tc.refused)
				}
			}
			anchor := seedBlob(t, c, "candidate anchor")
			id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h",
				SuitePath: tc.suite, InReleaseHash: &anchor, Form: tc.candidate})
			if err != nil {
				t.Fatal(err)
			}
			name := "Release"
			if tc.candidate == FormInline {
				name = "InRelease"
			}
			err = c.CommitAdoption(ctx, id, []SnapshotMember{{SnapshotID: id, Path: name, BlobHash: anchor, DeclaredSHA256: anchor}},
				nil, nil, nil, false)
			sf, gerr := c.GetSuiteFreshness(ctx, "http", "h", tc.suite)
			if gerr != nil {
				t.Fatal(gerr)
			}
			if !tc.refused {
				if err != nil || sf.CurrentSnapshotID == nil || *sf.CurrentSnapshotID != id {
					t.Fatalf("commit = %v, current %v; want the candidate committed", err, sf.CurrentSnapshotID)
				}
				return
			}
			if !errors.Is(err, ErrUnsignedDowngradeRefused) {
				t.Fatalf("commit = %v, want ErrUnsignedDowngradeRefused", err)
			}
			if got := derefI64(sf.CurrentSnapshotID); (priorID == 0 && sf.CurrentSnapshotID != nil) || (priorID != 0 && got != priorID) {
				t.Fatalf("current snapshot moved to %d, want %d", got, priorID)
			}
			if members, err := c.ListSnapshotMembers(ctx, id); err != nil || len(members) != 0 {
				t.Fatalf("refused candidate has members %v (%v), want none", members, err)
			}
			if snap, err := c.GetSuiteSnapshot(ctx, id); err != nil || snap.AdoptedAt != nil {
				t.Fatalf("refused candidate = %+v (%v), want it unadopted", snap, err)
			}
			if _, err := c.LookupURL(ctx, "http", "h", tc.suite+"/Release"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("a refused commit seeded the Release anchor (err %v)", err)
			}
		})
	}
}

// Spec C5 safeguard 3 beyond the current snapshot and the suite's own
// scheme (design review M-7, M-8): a marker for the base under the other
// scheme, and an adopted, non-retired signed snapshot that is not current
// (a displaced one the legacy audit left unmarked) — of the suite, of its
// /. twin, or under the other scheme — refuse an unsigned candidate. A
// retired snapshot (quarantined) and a signed candidate never adopted are
// no history. CheckUnsignedDowngrade agrees with the commit every time.
func TestCommitAdoption_RefusesUnsignedForAnySignedHistory(t *testing.T) {
	type history struct {
		scheme, suite string
		form          AnchorForm
		current       bool
		adopted       bool
		retired       bool
	}
	for _, tc := range []struct {
		name                     string
		suite                    string // the unsigned candidate's, over http
		markerScheme, markerBase string
		snapshot                 *history
		refused                  bool
	}{
		{name: "marker under the other scheme", suite: "/u/dists/s", markerScheme: "https", markerBase: "/u/dists/s", refused: true},
		{name: "displaced signed snapshot", suite: "/u/dists/s",
			snapshot: &history{scheme: "http", suite: "/u/dists/s", form: FormInline, adopted: true}, refused: true},
		{name: "displaced detached snapshot of the twin", suite: "/flat",
			snapshot: &history{scheme: "http", suite: "/flat/.", form: FormDetached, adopted: true}, refused: true},
		{name: "signed snapshot under the other scheme", suite: "/u/dists/s",
			snapshot: &history{scheme: "https", suite: "/u/dists/s", form: FormInline, current: true, adopted: true}, refused: true},
		{name: "retired signed snapshot", suite: "/u/dists/s",
			snapshot: &history{scheme: "http", suite: "/u/dists/s", form: FormInline, adopted: true, retired: true}},
		{name: "signed candidate never adopted", suite: "/u/dists/s",
			snapshot: &history{scheme: "http", suite: "/u/dists/s", form: FormInline}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := openCache(t)
			ctx := context.Background()
			defer stubNow(t, 1_800_000_000)()
			if s := tc.snapshot; s != nil {
				h := seedBlob(t, c, "signed history")
				var id int64
				if s.form == FormDetached {
					g := seedBlob(t, c, "its signature")
					id = seedFormSnapshot(t, c, s.scheme, "h", s.suite, s.form, nil, &h, &g, true)
				} else {
					id = seedFormSnapshot(t, c, s.scheme, "h", s.suite, s.form, &h, nil, nil, true)
				}
				if !s.current {
					if _, err := c.db.Exec(`UPDATE suite_freshness SET current_snapshot_id = NULL WHERE current_snapshot_id = ?`, id); err != nil {
						t.Fatal(err)
					}
				}
				if !s.adopted {
					if _, err := c.db.Exec(`UPDATE suite_snapshot SET adopted_at = NULL WHERE snapshot_id = ?`, id); err != nil {
						t.Fatal(err)
					}
				}
				if s.retired {
					if _, err := c.db.Exec(`UPDATE suite_snapshot SET retired_at = 1 WHERE snapshot_id = ?`, id); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.markerBase != "" {
				if err := c.MarkSignedSeenBatch(ctx, []SignedSeenMark{{CanonicalScheme: tc.markerScheme, CanonicalHost: "h",
					Base: tc.markerBase, SeenAt: 1}}); err != nil {
					t.Fatal(err)
				}
			}
			if early := c.CheckUnsignedDowngrade(ctx, "h", tc.suite); errors.Is(early, ErrUnsignedDowngradeRefused) != tc.refused {
				t.Fatalf("CheckUnsignedDowngrade = %v, want refused %v", early, tc.refused)
			}
			rel := seedBlob(t, c, "unsigned Release")
			id, _, err := c.InsertTrackedCandidateForTest(ctx, SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h",
				SuitePath: tc.suite, InReleaseHash: &rel, Form: FormUnsigned})
			if err != nil {
				t.Fatal(err)
			}
			err = c.CommitAdoption(ctx, id, []SnapshotMember{{SnapshotID: id, Path: "Release", BlobHash: rel, DeclaredSHA256: rel}},
				nil, nil, nil, false)
			if tc.refused != errors.Is(err, ErrUnsignedDowngradeRefused) || (!tc.refused && err != nil) {
				t.Fatalf("commit = %v, want refused %v", err, tc.refused)
			}
			sf, err := c.GetSuiteFreshness(ctx, "http", "h", tc.suite)
			if err != nil {
				t.Fatal(err)
			}
			if committed := sf.CurrentSnapshotID != nil && *sf.CurrentSnapshotID == id; committed == tc.refused {
				t.Fatalf("current snapshot %v, candidate %d: want committed %v", sf.CurrentSnapshotID, id, !tc.refused)
			}
		})
	}
}

// Spec C5 / §7: url_path GC's identity guard (d) keeps the anchor of the
// current snapshot's form — an unsigned suite's <suite>/Release, not the
// <suite>/InRelease its single-anchor slot would suggest. Guards (b)/(c)
// cannot decide here: the rows hold other blobs than the snapshot's.
func TestRunURLPathGCBatch_IdentityGuardFollowsTheAnchorForm(t *testing.T) {
	c := openCache(t)
	const now = int64(2_000_000_000)
	defer stubNow(t, now)()
	anchor := seedBlob(t, c, "the snapshot's anchor")
	old := sql.NullInt64{Int64: now - 30*86400, Valid: true}
	for _, s := range []struct {
		suite string
		form  AnchorForm
	}{{"/u/dists/unsigned", FormUnsigned}, {"/u/dists/inline", FormInline}} {
		seedFormSnapshot(t, c, "http", "h", s.suite, s.form, &anchor, nil, nil, false)
		for _, name := range []string{"/InRelease", "/Release"} {
			putURLPathRow(t, c, "http", "h", s.suite+name, seedBlob(t, c, "served "+s.suite+name), old, true)
		}
	}
	drainURLPathGC(t, c, 100, 7*86400, 0, testMaxVersions)
	for _, w := range []struct {
		path string
		kept bool
	}{
		{"/u/dists/unsigned/Release", true},
		{"/u/dists/unsigned/InRelease", false},
		{"/u/dists/inline/InRelease", true},
		{"/u/dists/inline/Release", false},
	} {
		if got := urlPathExists(t, c, w.path); got != w.kept {
			t.Errorf("%s kept = %v, want %v", w.path, got, w.kept)
		}
	}
}

// Spec C5 safeguard 5 / C10: an unsigned current snapshot never vouches a
// .deb and never counts for strict mode, even if a package_hash row or a
// complete-coverage bit somehow exists for it.
func TestStrictModeReads_IgnoreUnsignedSnapshots(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, 1_800_000_000)()
	rel := seedBlob(t, c, "unsigned Release")
	id := seedFormSnapshot(t, c, "http", "h", "/u/dists/s", FormUnsigned, &rel, nil, nil, true)
	deb := seedBlob(t, c, "a .deb")
	seedPackageHash(t, c, "http", "h", "/u/pool/p.deb", deb, id, "p", "amd64", "1")
	if rows, err := c.DeclaredHashesForPath(ctx, "http", "h", "/u/pool/p.deb"); err != nil || len(rows) != 0 {
		t.Fatalf("DeclaredHashesForPath = %v, %v; want no row from an unsigned snapshot", rows, err)
	}
	if cov, err := c.HostCurrentSnapshotsCoverage(ctx, "http", "h"); err != nil || len(cov) != 0 {
		t.Fatalf("HostCurrentSnapshotsCoverage = %v, %v; want no unsigned snapshot", cov, err)
	}
	// A signed snapshot on the same host is still counted and still vouches.
	in := seedBlob(t, c, "signed InRelease")
	sid := seedFormSnapshot(t, c, "http", "h", "/s/dists/s", FormInline, &in, nil, nil, true)
	seedPackageHash(t, c, "http", "h", "/s/pool/p.deb", deb, sid, "p", "amd64", "1")
	if rows, err := c.DeclaredHashesForPath(ctx, "http", "h", "/s/pool/p.deb"); err != nil || len(rows) != 1 {
		t.Fatalf("DeclaredHashesForPath(signed) = %v, %v; want one row", rows, err)
	}
	if cov, err := c.HostCurrentSnapshotsCoverage(ctx, "http", "h"); err != nil || len(cov) != 1 || cov[0].SnapshotID != sid {
		t.Fatalf("HostCurrentSnapshotsCoverage = %v, %v; want the signed snapshot only", cov, err)
	}
}
