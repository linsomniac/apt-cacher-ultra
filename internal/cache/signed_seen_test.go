package cache

import (
	"context"
	"testing"
)

func TestMarkSignedSeenBatch_KeepsNewestStamp(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	mark := func(at int64) {
		t.Helper()
		if err := c.MarkSignedSeenBatch(ctx, []SignedSeenMark{{CanonicalScheme: "http", CanonicalHost: "h", Base: "/r/dists/x", SeenAt: at}}); err != nil {
			t.Fatal(err)
		}
	}
	mark(100)
	mark(50)
	if at, ok, err := c.SignedSeen(ctx, "http", "h", "/r/dists/x"); err != nil || !ok || at != 100 {
		t.Fatalf("after 100 then 50: at=%d ok=%v err=%v, want 100", at, ok, err)
	}
	mark(200)
	if at, _, _ := c.SignedSeen(ctx, "http", "h", "/r/dists/x"); at != 200 {
		t.Fatalf("after 200: at=%d", at)
	}
	if _, ok, err := c.SignedSeen(ctx, "http", "h", "/r/dists/other"); err != nil || ok {
		t.Fatalf("unknown base: ok=%v err=%v", ok, err)
	}
}

// Spec C3: CommitAdoption records signed-seen history for signed forms,
// in its own transaction; an unsigned snapshot never does.
func TestCommitAdoption_SignedFormsRecordSignedSeen(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, 1_800_000_000)()
	commit := func(suite string, cand SnapshotCandidate, members []SnapshotMember) {
		t.Helper()
		cand.CanonicalScheme, cand.CanonicalHost, cand.SuitePath = "http", "h", suite
		id, _, err := c.InsertCandidateSnapshot(ctx, cand)
		if err != nil {
			t.Fatal(err)
		}
		for i := range members {
			members[i].SnapshotID = id
		}
		if err := c.CommitAdoption(ctx, id, members, nil, nil, nil, false); err != nil {
			t.Fatalf("CommitAdoption %s: %v", suite, err)
		}
	}
	inl := seedBlob(t, c, "inline anchor (signed-seen)")
	commit("/r/dists/inline", SnapshotCandidate{InReleaseHash: &inl},
		[]SnapshotMember{{Path: "InRelease", BlobHash: inl, DeclaredSHA256: inl}})
	rel, sig := seedBlob(t, c, "detached Release (signed-seen)"), seedBlob(t, c, "detached Release.gpg (signed-seen)")
	commit("/r/dists/detached", SnapshotCandidate{ReleaseHash: &rel, ReleaseGPGHash: &sig},
		[]SnapshotMember{{Path: "Release", BlobHash: rel, DeclaredSHA256: rel}, {Path: "Release.gpg", BlobHash: sig, DeclaredSHA256: sig}})
	un := seedBlob(t, c, "unsigned Release (signed-seen)")
	commit("/r/dists/unsigned", SnapshotCandidate{InReleaseHash: &un, Form: FormUnsigned},
		[]SnapshotMember{{Path: "Release", BlobHash: un, DeclaredSHA256: un}})

	for suite, want := range map[string]bool{"/r/dists/inline": true, "/r/dists/detached": true, "/r/dists/unsigned": false} {
		at, ok, err := c.SignedSeen(ctx, "http", "h", suite)
		if err != nil || ok != want || (ok && at != 1_800_000_000) {
			t.Errorf("%s: marker ok=%v at=%d err=%v, want ok=%v at the commit time", suite, ok, at, err, want)
		}
	}
}
