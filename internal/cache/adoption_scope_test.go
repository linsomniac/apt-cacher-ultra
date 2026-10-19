package cache

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
)

// commitFor adopts an inline snapshot of (scheme, host, suite) whose anchor
// is content and whose extra member path holds memberContent, creating the
// suite's row first; it returns the snapshot id.
func commitFor(t *testing.T, c *Cache, scheme, host, suite, content, path, memberContent string) int64 {
	t.Helper()
	ctx := context.Background()
	h := seedBlob(t, c, content)
	id, _, err := c.InsertTrackedCandidateForTest(ctx, SnapshotCandidate{CanonicalScheme: scheme, CanonicalHost: host,
		SuitePath: suite, InReleaseHash: &h})
	if err != nil {
		t.Fatal(err)
	}
	members := []SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: h, DeclaredSHA256: h}}
	if path != "" {
		m := seedBlob(t, c, memberContent)
		members = append(members, SnapshotMember{SnapshotID: id, Path: path, BlobHash: m, DeclaredSHA256: m})
	}
	if err := c.CommitAdoption(ctx, id, members, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	return id
}

// Deferred minor C-10: the adoption stamp (spec C8) reads only its own
// suite's prior snapshot and anchor rows — the same suite path on another
// host or scheme, adopted later or fetched in the future, does not move it.
func TestAdoptionStamp_IsScopedToTheSuitesSchemeAndHost(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, 1_800_000_000)()
	const suite = "/r/dists/x"
	other := commitFor(t, c, "http", "b", suite, "b's anchor", "", "")
	if _, err := c.db.Exec(`UPDATE suite_snapshot SET adopted_at = 1800009999 WHERE snapshot_id = ?`, other); err != nil {
		t.Fatal(err)
	}
	future := int64(1_800_005_000)
	for _, k := range []struct{ scheme, host string }{{"http", "b"}, {"https", "a"}} {
		h := seedBlob(t, c, "anchor of "+k.scheme+"://"+k.host)
		if err := c.PutURLPath(ctx, URLPath{CanonicalScheme: k.scheme, CanonicalHost: k.host, Path: suite + "/InRelease",
			BlobHash: &h, UpstreamURL: k.scheme + "://" + k.host + suite + "/InRelease", IsMetadata: true, LastFetchedAt: &future}); err != nil {
			t.Fatal(err)
		}
	}
	id := commitFor(t, c, "http", "a", suite, "a's anchor", "", "")
	snap, err := c.GetSuiteSnapshot(ctx, id)
	if err != nil || snap.AdoptedAt == nil || *snap.AdoptedAt != 1_800_000_000 {
		t.Fatalf("first adoption on host a stamped %d (%v), want now (1800000000): other hosts and schemes do not count",
			derefI64(snap.AdoptedAt), err)
	}
}

// Deferred minor C-10: displaced snapshots are ordered by adoption, not by
// id — an older candidate adopted later is the newer displaced snapshot —
// and a same-path suite on another host is not one of them.
func TestDisplacedSnapshotMembers_OrdersByAdoptionAndScopesToTheSuite(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, 1_800_000_000)()
	const suite, alias = "/r/dists/x", "main/by-hash/SHA256/shared"
	key := SuiteKey{CanonicalScheme: "http", CanonicalHost: "a", SuitePath: suite}
	if _, err := c.CreateSuite(ctx, SuiteSeed{SuiteKey: key, RequestedAt: 1}, SuiteCaps{}, nil); err != nil {
		t.Fatal(err)
	}
	cand := func(content, member string) (int64, []SnapshotMember) {
		t.Helper()
		h, m := seedBlob(t, c, content), seedBlob(t, c, member)
		id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "a",
			SuitePath: suite, InReleaseHash: &h})
		if err != nil {
			t.Fatal(err)
		}
		return id, []SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: h, DeclaredSHA256: h},
			{SnapshotID: id, Path: alias, BlobHash: m, DeclaredSHA256: m}}
	}
	older, olderMembers := cand("older candidate", "member of the older candidate")
	newer, newerMembers := cand("newer candidate", "member of the newer candidate")
	for _, x := range []struct {
		id      int64
		members []SnapshotMember
	}{{newer, newerMembers}, {older, olderMembers}} { // the older id is adopted last
		if err := c.CommitAdoption(ctx, x.id, x.members, nil, nil, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	current := commitFor(t, c, "http", "a", suite, "current anchor", "", "")
	commitFor(t, c, "http", "b", suite, "b's anchor", alias, "b's member")
	rows, err := c.DisplacedSnapshotMembers(ctx, "http", "a", suite, alias, current, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].SnapshotID != older || rows[1].SnapshotID != newer {
		t.Fatalf("displaced members = %+v, want snapshot %d (adopted last) then %d, and none of host b", rows, older, newer)
	}
	// The limit keeps the newest by adoption too.
	rows, err = c.DisplacedSnapshotMembers(ctx, "http", "a", suite, alias, current, 1)
	if err != nil || len(rows) != 1 || rows[0].SnapshotID != older {
		t.Fatalf("limit 1: displaced members = %+v, %v; want only snapshot %d", rows, err, older)
	}
}

// Deferred minor A-18: the package's cache helper logs nothing (test output
// stays pristine).
func TestOpenCache_LogsNothing(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	openCache(t)
	if buf.Len() != 0 {
		t.Fatalf("openCache logged:\n%s", buf.String())
	}
}
