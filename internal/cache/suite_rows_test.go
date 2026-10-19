package cache

import (
	"context"
	"errors"
	"testing"
)

// Spec C3: CreateSuite creates an absent row with the seed's stamps and
// the creation's interest stamp, and never touches an existing one.
func TestCreateSuite_CreatesOnlyAnAbsentRow(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: afSuite}
	check, etag := int64(1_800_000_100), "e1"
	res, err := c.CreateSuite(ctx, SuiteSeed{SuiteKey: key, LastCheckAt: &check, LastSuccessAt: &check,
		InReleaseETag: &etag, RequestedAt: 1_800_000_000}, SuiteCaps{}, nil)
	if err != nil || res.Outcome != SuiteCreated || res.Evicted != nil {
		t.Fatalf("CreateSuite = %+v, %v; want a creation", res, err)
	}
	s := getSuite(t, c)
	if derefI64(s.LastCheckAt) != check || derefI64(s.LastSuccessAt) != check || derefStr(s.InReleaseETag) != etag ||
		s.InReleaseLastMod != nil || derefI64(s.LastRequestedAt) != 1_800_000_000 || s.CurrentSnapshotID != nil ||
		s.InReleaseChangeSeenAt != nil || s.CheckFailCount != 0 {
		t.Fatalf("row = %+v, want the seed's stamps", s)
	}
	assertNoRecord(t, "created row", s)
	other := int64(1)
	res, err = c.CreateSuite(ctx, SuiteSeed{SuiteKey: key, LastCheckAt: &other, RequestedAt: 1}, SuiteCaps{}, nil)
	if err != nil || res.Outcome != SuiteExists {
		t.Fatalf("second CreateSuite = %+v, %v; want exists", res, err)
	}
	if s := getSuite(t, c); derefI64(s.LastCheckAt) != check || derefI64(s.LastRequestedAt) != 1_800_000_000 {
		t.Fatalf("an existing row was overwritten: %+v", s)
	}
}

// Spec C3 caps: max_suites/10 rows per canonical host — http and https of
// a host count together — and max_suites in all. A refusal writes nothing;
// zero caps are unbounded.
func TestCreateSuite_EnforcesTheCaps(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	create := func(scheme, host, suite string, caps SuiteCaps) CreateOutcome {
		t.Helper()
		// Freshly requested rows: none is idle enough to evict (Task 5).
		res, err := c.CreateSuite(ctx, SuiteSeed{SuiteKey: SuiteKey{CanonicalScheme: scheme, CanonicalHost: host, SuitePath: suite},
			RequestedAt: nowUnix()}, caps, nil)
		if err != nil {
			t.Fatal(err)
		}
		return res.Outcome
	}
	caps := SuiteCaps{Global: 4, PerHost: 2}
	steps := []struct {
		scheme, host, suite string
		want                CreateOutcome
	}{
		{"http", "a", "/1", SuiteCreated},
		{"https", "a", "/2", SuiteCreated},
		{"http", "a", "/3", SuiteRejectedHost},
		{"http", "a", "/1", SuiteExists},
		{"http", "b", "/1", SuiteCreated},
		{"http", "c", "/1", SuiteCreated},
		{"http", "d", "/1", SuiteRejectedGlobal},
	}
	for i, s := range steps {
		if got := create(s.scheme, s.host, s.suite, caps); got != s.want {
			t.Fatalf("step %d (%s://%s%s): %v, want %v", i, s.scheme, s.host, s.suite, got, s.want)
		}
	}
	if st, err := c.GetSuiteStats(ctx); err != nil || st.Tracked != 4 {
		t.Fatalf("tracked = %+v (%v), want 4 rows", st, err)
	}
	if got := create("http", "d", "/1", SuiteCaps{}); got != SuiteCreated {
		t.Fatalf("uncapped creation = %v, want created", got)
	}
}

// Spec C3: a creation whose request served sniff-passing signed metadata
// marks the suite's base in the creating transaction ("/repo/." and
// "/repo" share one); a creation without it, an existing row and a refusal
// mark nothing.
func TestCreateSuite_MarksSignedSeenInTheCreatingTransaction(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	create := func(host, suite string, signedAt int64, caps SuiteCaps) {
		t.Helper()
		if _, err := c.CreateSuite(ctx, SuiteSeed{SuiteKey: SuiteKey{CanonicalScheme: "http", CanonicalHost: host, SuitePath: suite},
			RequestedAt: 5, SignedSeenAt: signedAt}, caps, nil); err != nil {
			t.Fatal(err)
		}
	}
	marked := func(host, base string) bool {
		t.Helper()
		_, ok, err := c.SignedSeen(ctx, "http", host, base)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	create("h", "/repo/.", 7, SuiteCaps{})
	if at, ok, err := c.SignedSeen(ctx, "http", "h", "/repo"); err != nil || !ok || at != 7 {
		t.Fatalf("marker = %d %v %v, want 7 for base /repo", at, ok, err)
	}
	create("h", "/r/dists/plain", 0, SuiteCaps{})
	if marked("h", "/r/dists/plain") {
		t.Fatal("a creation without signed metadata wrote a marker")
	}
	create("h", "/r/dists/plain", 9, SuiteCaps{})
	if marked("h", "/r/dists/plain") {
		t.Fatal("finding an existing row wrote a marker")
	}
	create("h2", "/s", 9, SuiteCaps{Global: 1})
	if marked("h2", "/s") {
		t.Fatal("a refused creation wrote a marker")
	}
}

// Ruling PD-5: a creation marks its base, in the creating transaction, for
// every cached signed anchor whose url_path row still holds the blob that
// passed the sniff (with the newest of their stamps), and for no
// plain-text one. A listed row that changed or appeared since the read is
// left to the caller (Unproven); a failing marker write creates no row.
func TestCreateSuite_MarksCachedSignedAnchors(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	put := func(path, body string) string {
		t.Helper()
		h := seedBlob(t, c, body)
		if err := c.PutURLPath(ctx, URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: path, BlobHash: &h,
			UpstreamURL: "http://h" + path, IsMetadata: true}); err != nil {
			t.Fatal(err)
		}
		return h
	}
	create := func(suite string, cached ...CachedAnchor) (CreateResult, error) {
		t.Helper()
		return c.CreateSuite(ctx, SuiteSeed{SuiteKey: SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: suite},
			RequestedAt: 1_800_000_009, Cached: cached}, SuiteCaps{}, nil)
	}
	marker := func(base string) (int64, bool) {
		t.Helper()
		at, ok, err := c.SignedSeen(ctx, "http", "h", base)
		if err != nil {
			t.Fatal(err)
		}
		return at, ok
	}
	// Two cached signed anchors: the base is marked once, with the newer stamp.
	inA, gpgA := put("/r/dists/a/InRelease", "inrelease a"), put("/r/dists/a/Release.gpg", "sig a")
	res, err := create("/r/dists/a",
		CachedAnchor{Path: "/r/dists/a/InRelease", BlobHash: inA, Signed: true, SeenAt: 1_700_000_500},
		CachedAnchor{Path: "/r/dists/a/Release.gpg", BlobHash: gpgA, Signed: true, SeenAt: 1_700_000_000})
	if err != nil || res.Outcome != SuiteCreated || res.Unproven {
		t.Fatalf("a: %+v, %v; want created, proven", res, err)
	}
	if at, ok := marker("/r/dists/a"); !ok || at != 1_700_000_500 {
		t.Fatalf("a: marker %d %v, want the newest cached stamp 1700000500", at, ok)
	}
	// A plain-text cached InRelease (an HTML soft-404) and an absent
	// Release.gpg: nothing to mark.
	htmlB := put("/r/dists/b/InRelease", "<html>Not Found</html>")
	if res, err := create("/r/dists/b", CachedAnchor{Path: "/r/dists/b/InRelease", BlobHash: htmlB, SeenAt: 5},
		CachedAnchor{Path: "/r/dists/b/Release.gpg"}); err != nil || res.Unproven {
		t.Fatalf("b: %+v, %v", res, err)
	}
	if _, ok := marker("/r/dists/b"); ok {
		t.Fatal("b: a plain-text cached anchor earned a marker")
	}
	// A signed anchor whose row's stamps were unknown: the creation's stamp.
	gpgC := put("/r/dists/c/Release.gpg", "sig c")
	if _, err := create("/r/dists/c", CachedAnchor{Path: "/r/dists/c/Release.gpg", BlobHash: gpgC, Signed: true}); err != nil {
		t.Fatal(err)
	}
	if at, ok := marker("/r/dists/c"); !ok || at != 1_800_000_009 {
		t.Fatalf("c: marker %d %v, want the creation's stamp", at, ok)
	}
	// A row whose blob changed since the read, and one that appeared:
	// the stale sniff marks nothing, and the caller is told.
	put("/r/dists/d/InRelease", "a newer inrelease d")
	put("/r/dists/d/Release.gpg", "a late sig d")
	res, err = create("/r/dists/d",
		CachedAnchor{Path: "/r/dists/d/InRelease", BlobHash: "an older blob", Signed: true, SeenAt: 5},
		CachedAnchor{Path: "/r/dists/d/Release.gpg"})
	if err != nil || res.Outcome != SuiteCreated || !res.Unproven {
		t.Fatalf("d: %+v, %v; want created, unproven", res, err)
	}
	if _, ok := marker("/r/dists/d"); ok {
		t.Fatal("d: a changed row was marked from a stale sniff")
	}
	// A cached anchor row deleted since the read: nothing to mark, nothing unproven.
	if res, err := create("/r/dists/e", CachedAnchor{Path: "/r/dists/e/InRelease", BlobHash: "gone", Signed: true, SeenAt: 5}); err != nil || res.Unproven {
		t.Fatalf("e: %+v, %v", res, err)
	}
	if _, ok := marker("/r/dists/e"); ok {
		t.Fatal("e: a vanished row was marked")
	}
	// A failing marker write fails the creation: no row is tracked with an
	// unmarked signed anchor.
	if _, err := c.db.Exec(`CREATE TRIGGER fail_mark BEFORE INSERT ON suite_signed_seen
BEGIN SELECT RAISE(ABORT, 'injected marker failure'); END`); err != nil {
		t.Fatal(err)
	}
	gpgF := put("/r/dists/f/Release.gpg", "sig f")
	if _, err := create("/r/dists/f", CachedAnchor{Path: "/r/dists/f/Release.gpg", BlobHash: gpgF, Signed: true, SeenAt: 5}); err == nil {
		t.Fatal("f: the creation succeeded with a failing marker write")
	}
	if _, err := c.GetSuiteFreshness(ctx, "http", "h", "/r/dists/f"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("f: the row was created without its marker (%v)", err)
	}
}

// Spec C3: UpdateSuiteFreshness is UPDATE-only — it never creates a row —
// and writes a check's columns only: not current_snapshot_id, not
// last_requested_at, not the failure record.
func TestUpdateSuiteFreshness_IsUpdateOnly(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	check := int64(100)
	if err := c.UpdateSuiteFreshness(ctx, SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: afSuite,
		LastCheckAt: &check}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetSuiteFreshness(ctx, "http", "h", afSuite); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateSuiteFreshness created a row: %v", err)
	}
	if _, err := c.CreateSuite(ctx, SuiteSeed{SuiteKey: SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: afSuite},
		RequestedAt: 1_800_000_000}, SuiteCaps{}, nil); err != nil {
		t.Fatal(err)
	}
	id := commitCandidate(t, c, SnapshotCandidate{}, "update-only snapshot")
	if err := c.RecordAdoptionFailure(ctx, "http", "h", afSuite, sampleFailure()); err != nil {
		t.Fatal(err)
	}
	etag, seen, other := "e2", int64(300), int64(7)
	if err := c.UpdateSuiteFreshness(ctx, SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: afSuite,
		LastCheckAt: &check, LastSuccessAt: &check, InReleaseETag: &etag, InReleaseChangeSeenAt: &seen,
		CheckFailCount: 3, CurrentSnapshotID: &other, LastRequestedAt: &other}); err != nil {
		t.Fatal(err)
	}
	s := getSuite(t, c)
	if derefI64(s.LastCheckAt) != 100 || derefI64(s.LastSuccessAt) != 100 || derefStr(s.InReleaseETag) != "e2" ||
		derefI64(s.InReleaseChangeSeenAt) != 300 || s.CheckFailCount != 3 {
		t.Fatalf("check columns not written: %+v", s)
	}
	if derefI64(s.CurrentSnapshotID) != id || derefI64(s.LastRequestedAt) != 1_800_000_000 {
		t.Fatalf("UpdateSuiteFreshness wrote current_snapshot_id or last_requested_at: %+v", s)
	}
	assertRecord(t, "after UpdateSuiteFreshness", s, sampleFailure())
}

// UpdateSuiteFreshness writes only the row of its own scheme: http and
// https of one host and path are two suites.
func TestUpdateSuiteFreshness_IsSchemeScoped(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	for _, scheme := range []string{"http", "https"} {
		if _, err := c.CreateSuite(ctx, SuiteSeed{SuiteKey: SuiteKey{CanonicalScheme: scheme, CanonicalHost: "h", SuitePath: afSuite},
			RequestedAt: 1}, SuiteCaps{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	check := int64(77)
	if err := c.UpdateSuiteFreshness(ctx, SuiteFreshness{CanonicalScheme: "https", CanonicalHost: "h", SuitePath: afSuite,
		LastCheckAt: &check}); err != nil {
		t.Fatal(err)
	}
	if s := getSuite(t, c); s.LastCheckAt != nil {
		t.Fatalf("an https check wrote the http row: %+v", s)
	}
}

// Spec C3: CommitAdoption Step 6 is UPDATE-only. A suite without a row is
// refused with ErrSuiteGone and the flip rolls back whole: no row, no
// member, no pin, no marker, and the candidate stays a candidate.
func TestCommitAdoption_RefusesAGoneSuite(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	adopted := seedBlob(t, c, "inrelease bytes (gone suite)")
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/gone", InReleaseHash: &adopted})
	if err != nil {
		t.Fatal(err)
	}
	err = c.CommitAdoption(ctx, id, []SnapshotMember{
		{SnapshotID: id, Path: "InRelease", BlobHash: adopted, DeclaredSHA256: adopted},
	}, nil, nil, nil, false)
	if !errors.Is(err, ErrSuiteGone) {
		t.Fatalf("CommitAdoption = %v, want ErrSuiteGone", err)
	}
	if _, err := c.GetSuiteFreshness(ctx, "http", "h", "/r/dists/gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the commit created a suite row: %v", err)
	}
	if snap, err := c.GetSuiteSnapshot(ctx, id); err != nil || snap.AdoptedAt != nil {
		t.Fatalf("candidate = %+v (%v), want it unadopted", snap, err)
	}
	if ms, err := c.ListSnapshotMembers(ctx, id); err != nil || len(ms) != 0 {
		t.Fatalf("members = %v (%v), want none", ms, err)
	}
	if b, err := c.GetBlob(ctx, adopted); err != nil || b.RefCount != 0 {
		t.Fatalf("blob = %+v (%v), want refcount 0", b, err)
	}
	if _, ok, err := c.SignedSeen(ctx, "http", "h", "/r/dists/gone"); err != nil || ok {
		t.Fatalf("marker written (%v)", err)
	}
}

func derefI64(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

func derefStr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}
