package cache

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func commitCandidate(t *testing.T, c *Cache, sc SnapshotCandidate, content string) int64 {
	t.Helper()
	ctx := context.Background()
	h := seedBlob(t, c, content)
	sc.CanonicalScheme, sc.CanonicalHost, sc.SuitePath, sc.InReleaseHash = "http", "h", afSuite, &h
	id, _, err := c.InsertTrackedCandidateForTest(ctx, sc)
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
	orNeg := func(p *int64) int64 {
		if p == nil {
			return -1
		}
		return *p
	}
	if s.CheckFailCount != 2 || orNeg(s.LastCheckAt) != check || orNeg(s.LastSuccessAt) != success {
		t.Fatalf("flip touched check state: fail=%d check=%d success=%d", s.CheckFailCount, orNeg(s.LastCheckAt), orNeg(s.LastSuccessAt))
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
	id, _, err := c.InsertTrackedCandidateForTest(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := c.GetSuiteSnapshot(ctx, id)
	if err != nil || snap.ReleaseDate == nil || *snap.ReleaseDate != d1 {
		t.Fatalf("release_date = %v (err %v), want %d", snap.ReleaseDate, err, d1)
	}
	sc.ReleaseDate = &d2
	id2, reused, err := c.InsertTrackedCandidateForTest(ctx, sc)
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
	_, _, err := c.InsertTrackedCandidateForTest(context.Background(), SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: afSuite, InReleaseHash: &h})
	var nk *NaturalKeyAdoptedError
	if !errors.As(err, &nk) || nk.SnapshotID != id || !errors.Is(err, ErrSnapshotNaturalKeyAdopted) {
		t.Fatalf("err = %v, want *NaturalKeyAdoptedError{%d} wrapping ErrSnapshotNaturalKeyAdopted", err, id)
	}
}

func adoptedAtOf(t *testing.T, c *Cache, id int64) int64 {
	t.Helper()
	snap, err := c.GetSuiteSnapshot(context.Background(), id)
	if err != nil || snap.AdoptedAt == nil {
		t.Fatalf("snapshot %d: adopted_at %v (err %v)", id, snap, err)
	}
	return *snap.AdoptedAt
}

// Spec C8 monotonic Last-Modified: adopted_at = max(now, prior + 1), so
// two flips in one second, or one after a backward clock step, still move
// it forward; a forward step is taken as is.
func TestCommitAdoption_AdoptedAtIsMonotonic(t *testing.T) {
	c := openCache(t)
	stamp := func(now int64, content string) int64 {
		restore := stubNow(t, now)
		defer restore()
		return adoptedAtOf(t, c, commitCandidate(t, c, SnapshotCandidate{}, content))
	}
	got := []int64{
		stamp(1_000_000, "monotonic one"),
		stamp(1_000_000, "monotonic two"),  // same second
		stamp(500_000, "monotonic three"),  // the clock stepped back
		stamp(2_000_000, "monotonic four"), // and forward
	}
	if want := []int64{1_000_000, 1_000_001, 1_000_002, 2_000_000}; !slices.Equal(got, want) {
		t.Fatalf("adopted_at = %v, want %v", got, want)
	}
}

// putAnchor caches afSuite's anchor file, served (fetched) at fetched
// (deferred minor C-13: three tests wrote it out).
func putAnchor(t *testing.T, c *Cache, file string, fetched int64) {
	t.Helper()
	h := seedBlob(t, c, "served "+file)
	if err := c.PutURLPath(context.Background(), URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: afSuite + file,
		BlobHash: &h, UpstreamURL: "http://h" + afSuite + file, IsMetadata: true, LastFetchedAt: &fetched}); err != nil {
		t.Fatal(err)
	}
}

// A first adoption must pass the anchors clients were served from the
// url_path cache: max(now, the newest anchor last_fetched_at + 1), read
// before CommitAdoption Step 3c re-seeds those rows with now.
func TestCommitAdoption_FirstAdoptionPassesTheServedAnchors(t *testing.T) {
	c := openCache(t)
	putAnchor(t, c, "/InRelease", 1_400_000)
	putAnchor(t, c, "/Release", 1_500_000)
	putAnchor(t, c, "/Release.gpg", 1_600_000)
	restore := stubNow(t, 1_000_000) // the clock stepped back after those fetches
	id := commitCandidate(t, c, SnapshotCandidate{}, "first adoption")
	restore()
	if got := adoptedAtOf(t, c, id); got != 1_600_001 {
		t.Fatalf("adopted_at = %d, want 1600001 (newest anchor fetch + 1)", got)
	}
}

func TestCommitAdoption_FirstAdoptionKeepsNowPastOlderAnchors(t *testing.T) {
	c := openCache(t)
	putAnchor(t, c, "/InRelease", 900_000)
	restore := stubNow(t, 1_000_000)
	id := commitCandidate(t, c, SnapshotCandidate{}, "first adoption, newer clock")
	restore()
	if got := adoptedAtOf(t, c, id); got != 1_000_000 {
		t.Fatalf("adopted_at = %d, want now (1000000)", got)
	}
}

// Each anchor file a client may have been served counts on its own: a
// detached suite's clients hold Release (and Release.gpg), not InRelease.
func TestCommitAdoption_FirstAdoptionPassesEachServedAnchorFile(t *testing.T) {
	for _, file := range []string{"/InRelease", "/Release", "/Release.gpg"} {
		t.Run(file, func(t *testing.T) {
			c := openCache(t)
			putAnchor(t, c, file, 1_600_000)
			restore := stubNow(t, 1_000_000)
			id := commitCandidate(t, c, SnapshotCandidate{}, "first adoption after "+file)
			restore()
			if got := adoptedAtOf(t, c, id); got != 1_600_001 {
				t.Fatalf("adopted_at = %d, want 1600001 (the served %s's fetch + 1)", got, file)
			}
		})
	}
}
