package cache

import (
	"context"
	"errors"
	"testing"
)

// formOf reads the stored anchor_form of a snapshot row.
func formOf(t *testing.T, c *Cache, id int64) string {
	t.Helper()
	var f string
	if err := c.db.QueryRow(`SELECT anchor_form FROM suite_snapshot WHERE snapshot_id = ?`, id).Scan(&f); err != nil {
		t.Fatal(err)
	}
	return f
}

func putBlobs(t *testing.T, c *Cache, hashes ...string) {
	t.Helper()
	for _, h := range hashes {
		if err := c.PutBlob(context.Background(), h, 1); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInsertCandidateSnapshot_DerivesAndStoresForm(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	putBlobs(t, c, hA, hB, hC)
	inl := hA
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", InReleaseHash: &inl})
	if err != nil {
		t.Fatal(err)
	}
	if got := formOf(t, c, id); got != "inline" {
		t.Fatalf("derived form = %q, want inline", got)
	}

	rel, sig := hB, hC
	id2, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/y", ReleaseHash: &rel, ReleaseGPGHash: &sig})
	if err != nil {
		t.Fatal(err)
	}
	if got := formOf(t, c, id2); got != "detached" {
		t.Fatalf("derived form = %q, want detached", got)
	}

	// Unsigned must be explicit and uses the single-anchor slot; the form
	// is part of the natural key, so it does not collide with the inline row.
	un := hA
	id3, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", InReleaseHash: &un, Form: FormUnsigned})
	if err != nil {
		t.Fatalf("unsigned insert: %v", err)
	}
	if id3 == id || formOf(t, c, id3) != "unsigned" {
		t.Fatalf("unsigned candidate: id=%d (inline id=%d) form=%q", id3, id, formOf(t, c, id3))
	}

	// Inconsistent shapes are rejected before touching SQLite.
	bad := []SnapshotCandidate{
		{ReleaseHash: &rel, ReleaseGPGHash: &sig, Form: FormUnsigned},
		{InReleaseHash: &inl, Form: FormDetached},
		{InReleaseHash: &inl, ReleaseHash: &rel, ReleaseGPGHash: &sig},
		{ReleaseHash: &rel},
		{},
		{InReleaseHash: &inl, Form: "bogus"},
	}
	for i, sc := range bad {
		sc.CanonicalScheme, sc.CanonicalHost, sc.SuitePath = "http", "h", "/r/dists/z"
		if _, _, err := c.InsertCandidateSnapshot(ctx, sc); !errors.Is(err, ErrInvalidAnchorForm) {
			t.Errorf("bad[%d]: err = %v, want ErrInvalidAnchorForm", i, err)
		}
	}
}

// The collision lookup must include the form: re-inserting an unsigned
// candidate must reuse the unsigned row, never the inline row with the
// same hash (an inline row vouches .debs; an unsigned one must not).
func TestInsertCandidateSnapshot_LookupIncludesForm(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	putBlobs(t, c, hA)
	h := hA
	inl := SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", InReleaseHash: &h}
	idInl, _, err := c.InsertCandidateSnapshot(ctx, inl)
	if err != nil {
		t.Fatal(err)
	}
	un := inl
	un.Form = FormUnsigned
	idUn, _, err := c.InsertCandidateSnapshot(ctx, un)
	if err != nil {
		t.Fatal(err)
	}
	idUn2, reused, err := c.InsertCandidateSnapshot(ctx, un)
	if err != nil || !reused || idUn2 != idUn || idUn2 == idInl {
		t.Fatalf("unsigned re-insert: id=%d reused=%v err=%v, want reuse of %d (inline row %d)", idUn2, reused, err, idUn, idInl)
	}
	if got := formOf(t, c, idUn2); got != "unsigned" {
		t.Fatalf("reused row form = %q, want unsigned", got)
	}
}

// The collision lookup must skip retired incarnations: with a retired
// and a live row for the same content, a re-insert reuses the live one.
func TestInsertCandidateSnapshot_LookupSkipsRetiredIncarnation(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	putBlobs(t, c, hA)
	h := hA
	cand := SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", InReleaseHash: &h}
	id, _, err := c.InsertCandidateSnapshot(ctx, cand)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`UPDATE suite_snapshot SET adopted_at = 1, retired_at = 2 WHERE snapshot_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	id2, reused, err := c.InsertCandidateSnapshot(ctx, cand) // live candidate beside the retired row
	if err != nil || reused || id2 == id {
		t.Fatalf("insert beside retired row: id2=%d reused=%v err=%v (want a fresh candidate, not ErrSnapshotNaturalKeyAdopted)", id2, reused, err)
	}
	id3, reused, err := c.InsertCandidateSnapshot(ctx, cand) // collides: the lookup must skip the retired row
	if err != nil || !reused || id3 != id2 {
		t.Fatalf("id3=%d reused=%v err=%v; want reuse of live candidate %d", id3, reused, err, id2)
	}
}

func TestGetSuiteSnapshot_ReturnsFormAndLifecycleFields(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	putBlobs(t, c, hA, hB, hC)
	h := hA
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", InReleaseHash: &h, Form: FormUnsigned})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`UPDATE suite_snapshot SET release_date = 1700000000, retired_at = 1700000500 WHERE snapshot_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	s, err := c.GetSuiteSnapshot(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if s.Form != FormUnsigned || s.SingleAnchorHash == nil || *s.SingleAnchorHash != hA {
		t.Fatalf("unsigned snapshot = %+v", s)
	}
	if s.ReleaseDate == nil || *s.ReleaseDate != 1700000000 || s.RetiredAt == nil || *s.RetiredAt != 1700000500 {
		t.Fatalf("release_date/retired_at not scanned: %+v", s)
	}

	rel, sig := hB, hC
	id2, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/y", ReleaseHash: &rel, ReleaseGPGHash: &sig})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := c.GetSuiteSnapshot(ctx, id2)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Form != FormDetached || s2.SingleAnchorHash != nil || s2.ReleaseDate != nil || s2.RetiredAt != nil {
		t.Fatalf("detached snapshot = %+v", s2)
	}
	// A-22: an InRelease candidate with no explicit form is inline.
	inl := hA
	id3, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/z", InReleaseHash: &inl})
	if err != nil {
		t.Fatal(err)
	}
	s3, err := c.GetSuiteSnapshot(ctx, id3)
	if err != nil {
		t.Fatal(err)
	}
	if s3.Form != FormInline || s3.SingleAnchorHash == nil || *s3.SingleAnchorHash != hA || s3.ReleaseHash != nil {
		t.Fatalf("inline snapshot = %+v", s3)
	}
}
