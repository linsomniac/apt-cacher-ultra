package cache

import (
	"context"
	"fmt"
	"strings"
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
		id, _, err := c.InsertTrackedCandidateForTest(ctx, cand)
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

	// A-23: a flat suite "/flat/." is marked at its base "/flat".
	flat := seedBlob(t, c, "flat inline anchor (signed-seen)")
	commit("/flat/.", SnapshotCandidate{InReleaseHash: &flat},
		[]SnapshotMember{{Path: "InRelease", BlobHash: flat, DeclaredSHA256: flat}})
	for base, want := range map[string]bool{"/r/dists/inline": true, "/r/dists/detached": true, "/r/dists/unsigned": false,
		"/flat": true, "/flat/.": false} {
		at, ok, err := c.SignedSeen(ctx, "http", "h", base)
		if err != nil || ok != want || (ok && at != 1_800_000_000) {
			t.Errorf("%s: marker ok=%v at=%d err=%v, want ok=%v at the commit time", base, ok, at, err, want)
		}
	}
}

// Deferred minor A-24: a batch writes every mark, over several 500-row
// transactions, and duplicates within a batch keep the newest stamp.
func TestMarkSignedSeenBatch_ManyMarksInChunks(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	var marks []SignedSeenMark
	for i := range 1201 {
		marks = append(marks, SignedSeenMark{CanonicalScheme: "http", CanonicalHost: "h",
			Base: fmt.Sprintf("/r/dists/s%04d", i), SeenAt: int64(1000 + i)})
	}
	marks = append(marks,
		SignedSeenMark{CanonicalScheme: "http", CanonicalHost: "h", Base: "/dup", SeenAt: 100},
		SignedSeenMark{CanonicalScheme: "http", CanonicalHost: "h", Base: "/dup", SeenAt: 50})
	if err := c.MarkSignedSeenBatch(ctx, marks); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM suite_signed_seen WHERE base LIKE '/r/dists/s%'`).Scan(&n); err != nil || n != 1201 {
		t.Fatalf("markers = %d, %v; want 1201", n, err)
	}
	if at, _, _ := c.SignedSeen(ctx, "http", "h", "/r/dists/s1200"); at != 2200 {
		t.Fatalf("last mark of the third chunk = %d, want 2200", at)
	}
	if at, _, _ := c.SignedSeen(ctx, "http", "h", "/dup"); at != 100 {
		t.Fatalf("duplicate = %d, want the newest stamp 100", at)
	}
}

// Deferred minor A-25: a failing mark names the batch writer.
func TestMarkSignedSeenBatch_ErrorsArePrefixed(t *testing.T) {
	c := openCache(t)
	if _, err := c.db.Exec(`CREATE TRIGGER fail_mark BEFORE INSERT ON suite_signed_seen
BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatal(err)
	}
	err := c.MarkSignedSeenBatch(context.Background(), []SignedSeenMark{{CanonicalScheme: "http", CanonicalHost: "h", Base: "/b", SeenAt: 1}})
	if err == nil || !strings.HasPrefix(err.Error(), "MarkSignedSeenBatch: mark signed-seen http://h/b: ") {
		t.Fatalf("err = %v; want it prefixed \"MarkSignedSeenBatch: mark signed-seen http://h/b: \"", err)
	}
}
