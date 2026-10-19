package cache

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// commitWithMembers adopts an inline snapshot of suite on host h whose
// members are its InRelease plus extra (path → blob content).
func commitWithMembers(t *testing.T, c *Cache, suite, label string, extra map[string]string) int64 {
	t.Helper()
	ctx := context.Background()
	inrel := seedBlob(t, c, "InRelease "+label)
	id, _, err := c.InsertTrackedCandidateForTest(ctx, SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: suite, InReleaseHash: &inrel})
	if err != nil {
		t.Fatal(err)
	}
	members := []SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: inrel, DeclaredSHA256: inrel}}
	for p, content := range extra {
		b := seedBlob(t, c, content)
		members = append(members, SnapshotMember{SnapshotID: id, Path: p, BlobHash: b, DeclaredSHA256: b})
	}
	if err := c.CommitAdoption(ctx, id, members, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	return id
}

func snapshotIDs(ms []SnapshotMember) []int64 {
	out := make([]int64, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.SnapshotID)
	}
	return out
}

// Spec C8 retained by-hash, the cache half: the lookup searches the
// suite's newest limit adopted, non-retired snapshots other than the
// current one and returns the member from those that hold it, newest
// first. limit bounds the snapshots searched, not the rows returned.
func TestDisplacedSnapshotMembers(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	const suite = "/r/dists/x"
	alias := "main/binary-amd64/by-hash/SHA256/" + strings.Repeat("a", 64)
	s1 := commitWithMembers(t, c, suite, "one", map[string]string{alias: "content one"})
	s2 := commitWithMembers(t, c, suite, "two", map[string]string{alias: "content two"})
	commitWithMembers(t, c, suite, "three", nil)                                           // newest displaced, no alias
	s4 := commitWithMembers(t, c, suite, "four", map[string]string{alias: "content four"}) // current
	commitWithMembers(t, c, "/r/dists/y", "other suite", map[string]string{alias: "other suite"})

	lookup := func(limit int) []int64 {
		t.Helper()
		got, err := c.DisplacedSnapshotMembers(ctx, "http", "h", suite, alias, s4, limit)
		if err != nil {
			t.Fatal(err)
		}
		return snapshotIDs(got)
	}
	if got := lookup(10); !slices.Equal(got, []int64{s2, s1}) {
		t.Fatalf("limit 10: snapshots %v, want [%d %d] (newest first; current and other suites excluded)", got, s2, s1)
	}
	if got := lookup(1); len(got) != 0 {
		t.Fatalf("limit 1: snapshots %v, want none (the newest displaced snapshot lacks the member)", got)
	}
	if got := lookup(2); !slices.Equal(got, []int64{s2}) {
		t.Fatalf("limit 2: snapshots %v, want [%d]", got, s2)
	}
	if got := lookup(0); len(got) != 0 {
		t.Fatalf("limit 0: snapshots %v, want none", got)
	}
	if _, err := c.ApplyLegacyAuditDecisions(ctx, []LegacyAuditDecision{{SnapshotID: s2, Outcome: LegacyAuditQuarantine}}); err != nil {
		t.Fatal(err)
	}
	got, err := c.DisplacedSnapshotMembers(ctx, "http", "h", suite, alias, s4, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(snapshotIDs(got), []int64{s1}) {
		t.Fatalf("after retiring %d: snapshots %v, want [%d] (retired snapshots never serve)", s2, snapshotIDs(got), s1)
	}
	if got[0].Path != alias || got[0].BlobHash != got[0].DeclaredSHA256 {
		t.Fatalf("member = %+v, want the alias row with its binding", got[0])
	}
}
