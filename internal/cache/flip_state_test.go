package cache

import (
	"context"
	"errors"
	"testing"
)

func commitCandidate(t *testing.T, c *Cache, sc SnapshotCandidate, content string) int64 {
	t.Helper()
	ctx := context.Background()
	h := seedBlob(t, c, content)
	sc.CanonicalScheme, sc.CanonicalHost, sc.SuitePath, sc.InReleaseHash = "http", "h", afSuite, &h
	id, _, err := c.InsertCandidateSnapshot(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id, []SnapshotMember{
		{SnapshotID: id, Path: "InRelease", BlobHash: h, DeclaredSHA256: h}}, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	return id
}

// Spec C4 "Validators" and "inrelease_change_seen_at", C7 "cleared in the
// flip transaction": after a flip the suite row describes the adopted
// anchor and carries no pending-change or failure state.
func TestCommitAdoption_FlipDescribesAdoptedAnchor(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	oldETag, oldLM, seen, check, success := "old-etag", "old-lm", int64(123), int64(456), int64(789)
	putSuite(t, c, SuiteFreshness{InReleaseETag: &oldETag, InReleaseLastMod: &oldLM,
		InReleaseChangeSeenAt: &seen, LastCheckAt: &check, LastSuccessAt: &success, CheckFailCount: 2})
	if err := c.RecordAdoptionFailure(ctx, "http", "h", afSuite, sampleFailure()); err != nil {
		t.Fatal(err)
	}
	newETag, newLM := "new-etag", "new-lm"
	commitCandidate(t, c, SnapshotCandidate{InReleaseETag: &newETag, InReleaseLastMod: &newLM}, "flip anchor")

	s := getSuite(t, c)
	if s.InReleaseETag == nil || *s.InReleaseETag != newETag || s.InReleaseLastMod == nil || *s.InReleaseLastMod != newLM {
		t.Fatalf("validators = %v/%v, want the adopted anchor's", s.InReleaseETag, s.InReleaseLastMod)
	}
	if s.InReleaseChangeSeenAt != nil {
		t.Fatalf("inrelease_change_seen_at = %d, want cleared", *s.InReleaseChangeSeenAt)
	}
	assertNoRecord(t, "after flip", s)
	if s.CheckFailCount != 2 || *s.LastCheckAt != check || *s.LastSuccessAt != success {
		t.Fatalf("flip touched check state: fail=%d check=%v success=%v", s.CheckFailCount, *s.LastCheckAt, *s.LastSuccessAt)
	}
}

// A candidate without validators must not leave the previous anchor's
// validators on the row (they would describe content no longer served).
func TestCommitAdoption_CandidateWithoutValidatorsClearsOldOnes(t *testing.T) {
	c := openCache(t)
	oldETag := "old-etag"
	putSuite(t, c, SuiteFreshness{InReleaseETag: &oldETag})
	commitCandidate(t, c, SnapshotCandidate{}, "anchor without validators")
	if s := getSuite(t, c); s.InReleaseETag != nil || s.InReleaseLastMod != nil {
		t.Fatalf("validators = %v/%v, want NULL", s.InReleaseETag, s.InReleaseLastMod)
	}
}

// Spec C5: the verified Date is stored as suite_snapshot.release_date; a
// reused candidate takes the retry's value.
func TestInsertCandidateSnapshot_StoresReleaseDate(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	h := seedBlob(t, c, "dated inrelease")
	d1, d2 := int64(1_700_000_000), int64(1_700_000_500)
	sc := SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: afSuite, InReleaseHash: &h, ReleaseDate: &d1}
	id, _, err := c.InsertCandidateSnapshot(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := c.GetSuiteSnapshot(ctx, id)
	if err != nil || snap.ReleaseDate == nil || *snap.ReleaseDate != d1 {
		t.Fatalf("release_date = %v (err %v), want %d", snap.ReleaseDate, err, d1)
	}
	sc.ReleaseDate = &d2
	id2, reused, err := c.InsertCandidateSnapshot(ctx, sc)
	if err != nil || !reused || id2 != id {
		t.Fatalf("reuse = %d, %v, %v", id2, reused, err)
	}
	if snap, _ := c.GetSuiteSnapshot(ctx, id); snap.ReleaseDate == nil || *snap.ReleaseDate != d2 {
		t.Fatalf("reused release_date = %v, want %d", snap.ReleaseDate, d2)
	}
}

func TestInsertCandidateSnapshot_NaturalKeyErrorNamesTheSnapshot(t *testing.T) {
	c := openCache(t)
	id := commitCandidate(t, c, SnapshotCandidate{}, "adopted once")
	h := seedBlob(t, c, "adopted once")
	_, _, err := c.InsertCandidateSnapshot(context.Background(), SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: afSuite, InReleaseHash: &h})
	var nk *NaturalKeyAdoptedError
	if !errors.As(err, &nk) || nk.SnapshotID != id || !errors.Is(err, ErrSnapshotNaturalKeyAdopted) {
		t.Fatalf("err = %v, want *NaturalKeyAdoptedError{%d} wrapping ErrSnapshotNaturalKeyAdopted", err, id)
	}
}
