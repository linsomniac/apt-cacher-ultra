package cache

import (
	"context"
	"database/sql"
	"errors"
	"runtime"
	"testing"
)

// fetchedRow is the url_path row of a miss of key's file, over a stored
// blob (url_path.blob_hash references blob).
func fetchedRow(t *testing.T, c *Cache, key SuiteKey, file string) URLPath {
	t.Helper()
	h := seedBlob(t, c, "body of "+key.CanonicalHost+key.SuitePath+file)
	at := int64(1_800_000_000)
	return URLPath{CanonicalScheme: key.CanonicalScheme, CanonicalHost: key.CanonicalHost, Path: key.SuitePath + file,
		BlobHash: &h, UpstreamURL: "http://" + key.CanonicalHost + key.SuitePath + file, IsMetadata: true,
		LastRequestedAt: &at, RequestCount: 1, LastFetchedAt: &at}
}

// Spec C3: one writer transaction stores the url_path row, creates the
// suite row and marks the suite's base — before the handler serves.
func TestStoreFetched_StoresRowSuiteAndMarkerTogether(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/r/dists/x")
	res, err := c.StoreFetched(ctx, FetchedRow{Row: fetchedRow(t, c, key, "/InRelease"), Suite: key,
		Create: &SuiteSeed{SuiteKey: key, RequestedAt: 1_800_000_000}, MarkSeenAt: 1_800_000_000}, SuiteCaps{}, nil)
	if err != nil || res.Create.Outcome != SuiteCreated || !res.Tracked || !res.Marked {
		t.Fatalf("StoreFetched = %+v, %v; want created, tracked, marked", res, err)
	}
	if _, err := c.LookupURL(ctx, "http", "h", "/r/dists/x/InRelease"); err != nil {
		t.Fatalf("url_path row: %v", err)
	}
	if at, ok, err := c.SignedSeen(ctx, "http", "h", "/r/dists/x"); err != nil || !ok || at != 1_800_000_000 {
		t.Fatalf("marker = %d %v %v", at, ok, err)
	}
	// A refresh miss of a tracked suite: no creation, marker still written.
	res, err = c.StoreFetched(ctx, FetchedRow{Row: fetchedRow(t, c, key, "/InRelease"), Suite: key,
		Create: &SuiteSeed{SuiteKey: key, RequestedAt: 1}, MarkSeenAt: 1_800_000_500}, SuiteCaps{}, nil)
	if err != nil || res.Create.Outcome != SuiteExists || !res.Tracked || !res.Marked {
		t.Fatalf("refresh StoreFetched = %+v, %v; want exists, tracked, marked", res, err)
	}
	if at, _, _ := c.SignedSeen(ctx, "http", "h", "/r/dists/x"); at != 1_800_000_500 {
		t.Fatalf("marker after the refresh = %d, want 1800000500", at)
	}
}

// Spec C3: markers only for suites with a row. An untracked suite's
// signed anchor, or one refused at a cap, stores its url_path row with no
// marker.
func TestStoreFetched_MarksOnlyTrackedSuites(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	gpgOnly := sk("h", "/r/dists/untracked")
	res, err := c.StoreFetched(ctx, FetchedRow{Row: fetchedRow(t, c, gpgOnly, "/Release.gpg"), Suite: gpgOnly,
		MarkSeenAt: 7}, SuiteCaps{}, nil)
	if err != nil || res.Tracked || res.Marked || res.Create.Outcome != 0 {
		t.Fatalf("StoreFetched = %+v, %v; want untracked, unmarked, no creation", res, err)
	}
	refused := sk("h", "/r/dists/refused")
	seedRow(t, c, sk("h", "/r/dists/first"), nowUnix())
	res, err = c.StoreFetched(ctx, FetchedRow{Row: fetchedRow(t, c, refused, "/InRelease"), Suite: refused,
		Create: &SuiteSeed{SuiteKey: refused, RequestedAt: 7}, MarkSeenAt: 7}, SuiteCaps{Global: 10, PerHost: 1}, nil)
	if err != nil || res.Create.Outcome != SuiteRejectedHost || res.Tracked || res.Marked {
		t.Fatalf("StoreFetched at the host cap = %+v, %v; want host_cap, untracked, unmarked", res, err)
	}
	for _, p := range []string{"/r/dists/untracked/Release.gpg", "/r/dists/refused/InRelease"} {
		if _, err := c.LookupURL(ctx, "http", "h", p); err != nil {
			t.Fatalf("%s: url_path row not stored: %v", p, err)
		}
	}
	for _, k := range []SuiteKey{gpgOnly, refused} {
		if _, ok, err := c.SignedSeen(ctx, "http", "h", k.SuitePath); err != nil || ok {
			t.Fatalf("%s: marker written for an untracked suite (%v)", k.SuitePath, err)
		}
	}
}

// Spec C3: a failed writer operation stores nothing — no url_path row, no
// suite row, no marker — and returns the error (the handler then answers
// 502 with no body).
func TestStoreFetched_AFailedOperationStoresNothing(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	if _, err := c.db.Exec(`CREATE TRIGGER fail_mark BEFORE INSERT ON suite_signed_seen
BEGIN SELECT RAISE(ABORT, 'injected marker failure'); END`); err != nil {
		t.Fatal(err)
	}
	key := sk("h", "/r/dists/x")
	_, err := c.StoreFetched(ctx, FetchedRow{Row: fetchedRow(t, c, key, "/InRelease"), Suite: key,
		Create: &SuiteSeed{SuiteKey: key, RequestedAt: 1}, MarkSeenAt: 1}, SuiteCaps{}, nil)
	if err == nil {
		t.Fatal("StoreFetched succeeded with a failing marker insert")
	}
	if _, err := c.LookupURL(ctx, "http", "h", "/r/dists/x/InRelease"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("url_path row stored by a failed operation (%v)", err)
	}
	if rowExists(t, c, key) {
		t.Fatal("suite row created by a failed operation")
	}
}

// StoreFetched creates under the caps with cap eviction, like CreateSuite,
// and releases the held candidate.
func TestStoreFetched_EvictsAtTheGlobalCap(t *testing.T) {
	c := openCache(t)
	defer stubNow(t, evictNow)()
	fourRows(t, c)
	g := newFakeGuard()
	key := sk("e", "/new")
	res, err := c.StoreFetched(context.Background(), FetchedRow{Row: fetchedRow(t, c, key, "/InRelease"), Suite: key,
		Create: &SuiteSeed{SuiteKey: key, RequestedAt: evictNow}}, SuiteCaps{Global: 4}, g)
	if err != nil || res.Create.Outcome != SuiteCreated || res.Create.Evicted == nil || *res.Create.Evicted != sk("b", "/idle") {
		t.Fatalf("StoreFetched = %+v, %v; want created, evicting b/idle", res, err)
	}
	if len(g.released[sk("b", "/idle")]) != 1 || !g.released[sk("b", "/idle")][0] {
		t.Fatalf("released %v; want b/idle released as deleted", g.released)
	}
}

// Spec C3 FIFO ordering: the writer is a FIFO queue drained by one
// goroutine, so a writer operation enqueued after a StoreFetched that
// marks — phase E's unsigned CommitAdoption, with its in-transaction marker
// re-read — sees the marker.
func TestStoreFetched_MarkerPrecedesALaterWriterOperation(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/r/dists/x")
	seedRow(t, c, key, nowUnix())
	row := fetchedRow(t, c, key, "/InRelease") // its blob is written before the writer is blocked
	block, blocked := make(chan struct{}), make(chan struct{})
	go func() {
		_ = c.submitWrite(ctx, func(context.Context, *sql.Conn) error {
			close(blocked)
			<-block
			return nil
		})
	}()
	<-blocked // the writer is busy; what follows queues in order
	waitQueued := func(n int) {
		t.Helper()
		for len(c.writes) < n {
			runtime.Gosched()
		}
	}
	storeErr := make(chan error, 1)
	go func() {
		_, err := c.StoreFetched(ctx, FetchedRow{Row: row, Suite: key, MarkSeenAt: 9}, SuiteCaps{}, nil)
		storeErr <- err
	}()
	waitQueued(1)
	var sawMarker bool
	later := make(chan error, 1)
	go func() {
		later <- c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
			var n int
			err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM suite_signed_seen
 WHERE canonical_scheme = 'http' AND canonical_host = 'h' AND base = '/r/dists/x'`).Scan(&n)
			sawMarker = n == 1
			return err
		})
	}()
	waitQueued(2)
	close(block)
	if err := <-storeErr; err != nil {
		t.Fatal(err)
	}
	if err := <-later; err != nil {
		t.Fatal(err)
	}
	if !sawMarker {
		t.Fatal("a writer operation enqueued after StoreFetched did not see its marker")
	}
}

// A failed operation releases its held eviction candidate, as not deleted
// (the checker's suite mutex must not leak), and commits no eviction.
func TestStoreFetched_AFailedOperationReleasesItsCandidate(t *testing.T) {
	c := openCache(t)
	defer stubNow(t, evictNow)()
	fourRows(t, c)
	key := sk("e", "/new")
	row := fetchedRow(t, c, key, "/InRelease")
	if _, err := c.db.Exec(`CREATE TRIGGER fail_insert BEFORE INSERT ON suite_freshness
BEGIN SELECT RAISE(ABORT, 'injected insert failure'); END`); err != nil {
		t.Fatal(err)
	}
	g := newFakeGuard()
	if _, err := c.StoreFetched(context.Background(), FetchedRow{Row: row, Suite: key,
		Create: &SuiteSeed{SuiteKey: key, RequestedAt: evictNow}}, SuiteCaps{Global: 4}, g); err == nil {
		t.Fatal("premise: the insert fails")
	}
	if got := g.released[sk("b", "/idle")]; len(got) != 1 || got[0] {
		t.Fatalf("released %v; want b/idle released once, as not deleted", g.released)
	}
	if !rowExists(t, c, sk("b", "/idle")) {
		t.Fatal("the failed operation's eviction was committed")
	}
}

// Ruling PD-6: with no adopter, a refresh miss of an InRelease re-stamps an
// existing row without a current snapshot with today's Phase-1 seed
// columns, UPDATE-only — the miss's stamps and validators, the pending
// change and the failure streak cleared — and leaves last_requested_at and
// the failure record alone. A row with a current snapshot, and a suite
// without a row, are left as they are.
func TestStoreFetched_RestampsOnlyATrackedUnadoptedRow(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", afSuite)
	old, seen, etag := int64(1_700_000_000), int64(1_700_000_100), "old"
	if err := c.SeedSuiteForTest(ctx, SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: afSuite,
		LastCheckAt: &old, LastSuccessAt: &old, InReleaseETag: &etag, InReleaseChangeSeenAt: &seen, CheckFailCount: 4}); err != nil {
		t.Fatal(err)
	}
	if err := c.RecordAdoptionFailure(ctx, "http", "h", afSuite, sampleFailure()); err != nil {
		t.Fatal(err)
	}
	requested := derefI64(getSuite(t, c).LastRequestedAt)
	stamp, tag := int64(1_800_000_500), `"v2"`
	seed := &SuiteSeed{SuiteKey: key, LastCheckAt: &stamp, LastSuccessAt: &stamp, InReleaseETag: &tag, RequestedAt: stamp}
	res, err := c.StoreFetched(ctx, FetchedRow{Row: fetchedRow(t, c, key, "/InRelease"), Suite: key, Create: seed,
		Restamp: seed, MarkSeenAt: stamp}, SuiteCaps{}, nil)
	if err != nil || res.Create.Outcome != SuiteExists || !res.Tracked {
		t.Fatalf("StoreFetched = %+v, %v; want exists, tracked", res, err)
	}
	s := getSuite(t, c)
	if derefI64(s.LastCheckAt) != stamp || derefI64(s.LastSuccessAt) != stamp || derefStr(s.InReleaseETag) != tag ||
		s.InReleaseLastMod != nil || s.InReleaseChangeSeenAt != nil || s.CheckFailCount != 0 {
		t.Fatalf("row = %+v; want the miss's stamps and validators, the change and the streak cleared", s)
	}
	if derefI64(s.LastRequestedAt) != requested {
		t.Fatalf("last_requested_at = %d, want %d (a refresh miss is no creation)", derefI64(s.LastRequestedAt), requested)
	}
	assertRecord(t, "after the re-stamp", s, sampleFailure())
	commitCandidate(t, c, SnapshotCandidate{}, "re-stamp snapshot")
	later := int64(1_800_000_900)
	adopted := &SuiteSeed{SuiteKey: key, LastCheckAt: &later, LastSuccessAt: &later, RequestedAt: later}
	if _, err := c.StoreFetched(ctx, FetchedRow{Row: fetchedRow(t, c, key, "/InRelease"), Suite: key, Create: adopted,
		Restamp: adopted}, SuiteCaps{}, nil); err != nil {
		t.Fatal(err)
	}
	if got := derefI64(getSuite(t, c).LastCheckAt); got != stamp {
		t.Fatalf("an adopted row was re-stamped: last_check_at = %d, want %d", got, stamp)
	}
	none := sk("h", "/r/dists/none")
	if _, err := c.StoreFetched(ctx, FetchedRow{Row: fetchedRow(t, c, none, "/InRelease"), Suite: none,
		Restamp: &SuiteSeed{SuiteKey: none, LastCheckAt: &later, LastSuccessAt: &later}}, SuiteCaps{}, nil); err != nil {
		t.Fatal(err)
	}
	if rowExists(t, c, none) {
		t.Fatal("a re-stamp created a row")
	}
}

// Ruling PD-5: a miss that creates its suite's row marks, in the same
// transaction, the suite's cached signed anchors the caller sniffed (here
// a Release miss with an adopter wired and a cached Release.gpg).
func TestStoreFetched_CreationMarksCachedSignedAnchors(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/r/dists/d")
	gpg := fetchedRow(t, c, key, "/Release.gpg")
	if err := c.PutURLPath(ctx, gpg); err != nil {
		t.Fatal(err)
	}
	res, err := c.StoreFetched(ctx, FetchedRow{Row: fetchedRow(t, c, key, "/Release"), Suite: key,
		Create: &SuiteSeed{SuiteKey: key, RequestedAt: 1_800_000_001, Cached: []CachedAnchor{
			{Path: gpg.Path, BlobHash: *gpg.BlobHash, Signed: true, SeenAt: 1_800_000_000}}}}, SuiteCaps{}, nil)
	if err != nil || res.Create.Outcome != SuiteCreated || res.Create.Unproven || res.Marked {
		t.Fatalf("StoreFetched = %+v, %v; want created, proven, no marker for the Release itself", res, err)
	}
	if at, ok, err := c.SignedSeen(ctx, "http", "h", "/r/dists/d"); err != nil || !ok || at != 1_800_000_000 {
		t.Fatalf("marker = %d %v %v, want the cached Release.gpg's stamp", at, ok, err)
	}
}

// Design review M4: like CreateSuite, StoreFetched tries once more when its
// held candidate was evicted by a concurrent creation before it ran.
func TestStoreFetched_RetriesOnceWhenTheCandidateVanished(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, evictNow)()
	fourRows(t, c)
	g := newFakeGuard()
	g.onHold = func(k SuiteKey) {
		if k != sk("b", "/idle") {
			return
		}
		if _, err := c.db.Exec(`DELETE FROM suite_freshness WHERE canonical_host = 'b' AND suite_path = '/idle'`); err != nil {
			t.Error(err)
		}
		seedRow(t, c, sk("r", "/refill"), evictNow-3600)
	}
	key := sk("e", "/new")
	res, err := c.StoreFetched(ctx, FetchedRow{Row: fetchedRow(t, c, key, "/InRelease"), Suite: key,
		Create: &SuiteSeed{SuiteKey: key, RequestedAt: evictNow}}, SuiteCaps{Global: 4}, g)
	if err != nil || res.Create.Outcome != SuiteCreated || res.Create.Evicted == nil || *res.Create.Evicted != sk("c", "/idle2") ||
		!res.Tracked {
		t.Fatalf("StoreFetched = %+v, %v; want created on the second attempt, evicting c/idle2", res, err)
	}
	if len(g.released[sk("b", "/idle")]) != 1 || !g.released[sk("b", "/idle")][0] {
		t.Fatalf("released %v; want b/idle released once, as gone", g.released)
	}
	if _, err := c.LookupURL(ctx, "http", "e", "/new/InRelease"); err != nil {
		t.Fatalf("url_path row: %v", err)
	}
}
