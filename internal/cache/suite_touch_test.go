package cache

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func requestedAt(t *testing.T, c *Cache, key SuiteKey) int64 {
	t.Helper()
	s, err := c.GetSuiteFreshness(context.Background(), key.CanonicalScheme, key.CanonicalHost, key.SuitePath)
	if err != nil {
		t.Fatal(err)
	}
	return derefI64(s.LastRequestedAt)
}

// Spec C3: touches are coalesced to one write per suite per hour, in
// monotonic time; the row's creation counts as its first touch.
func TestTouchSuite_WritesAtMostOncePerHour(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := openCache(t)
		ctx := context.Background()
		key := sk("h", "/r/dists/x")
		t0 := nowUnix()
		seedRow(t, c, key, t0)
		touch := func(after time.Duration, want int64) {
			t.Helper()
			time.Sleep(after)
			if err := c.TouchSuite(ctx, key); err != nil {
				t.Fatal(err)
			}
			if got := requestedAt(t, c, key); got != want {
				t.Fatalf("after %v more: last_requested_at = %d, want %d", after, got-t0, want-t0)
			}
		}
		touch(10*time.Minute, t0)      // within the creation's hour: no write
		touch(50*time.Minute, t0+3600) // an hour after the creation: written
		touch(59*time.Minute, t0+3600) // within that write's hour: no write
		touch(time.Minute, t0+7200)    // an hour after it: written
	})
}

// Concurrent touches of one suite write once.
func TestTouchSuite_ConcurrentTouchesWriteOnce(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/r/dists/x")
	if err := c.SeedSuiteForTest(ctx, SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`CREATE TABLE touch_writes (n INTEGER);
CREATE TRIGGER count_touch AFTER UPDATE OF last_requested_at ON suite_freshness
BEGIN INSERT INTO touch_writes VALUES (1); END`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.TouchSuite(ctx, key); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var n int
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM touch_writes`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("20 concurrent touches wrote %d times, want 1", n)
	}
}

// Spec C3: the coalescing map holds only tracked suites — a touch of a
// suite without a row writes nothing, creates nothing and leaves no entry
// — and an eviction purges the evictee's entry.
func TestTouchSuite_MapHoldsOnlyTrackedSuites(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, evictNow)()
	for i := range 50 {
		if err := c.TouchSuite(ctx, sk("minted", "/dists/"+strings.Repeat("x", i+1))); err != nil {
			t.Fatal(err)
		}
	}
	if n := c.TouchedSuites(); n != 0 {
		t.Fatalf("map holds %d entries after touching 50 untracked suites, want 0", n)
	}
	if rowExists(t, c, sk("minted", "/dists/x")) {
		t.Fatal("a touch created a row")
	}
	fourRows(t, c) // four creations: four entries
	if _, err := c.CreateSuite(ctx, SuiteSeed{SuiteKey: sk("e", "/new"), RequestedAt: evictNow}, SuiteCaps{Global: 4},
		newFakeGuard()); err != nil {
		t.Fatal(err)
	}
	c.touches.mu.Lock()
	_, evictee := c.touches.last[sk("b", "/idle")]
	_, created := c.touches.last[sk("e", "/new")]
	n := len(c.touches.last)
	c.touches.mu.Unlock()
	if evictee || !created || n != 4 {
		t.Fatalf("map after the eviction: evictee %v, created %v, %d entries; want false, true, 4", evictee, created, n)
	}
}

// At touchSweepAt entries the map drops the ones older than touchEvery.
func TestTouchSuite_MapIsSwept(t *testing.T) {
	c := openCache(t)
	c.touches.mu.Lock()
	c.touches.last = make(map[SuiteKey]time.Time)
	old := time.Now().Add(-2 * touchEvery)
	for i := range touchSweepAt - 1 {
		c.touches.last[sk("old", "/"+strings.Repeat("o", i+1))] = old
	}
	c.touches.last[sk("fresh", "/f")] = time.Now()
	c.touches.mu.Unlock()
	c.noteTouch(sk("new", "/n"))
	if n := c.TouchedSuites(); n != 2 {
		t.Fatalf("map after the sweep = %d entries, want 2 (the fresh one and the new one)", n)
	}
}

// Design review M9: once swept, the map is swept again only after
// touchSweepEvery or once it has doubled, so a map of fresh entries costs
// no O(map) scan per claim.
func TestTouchSuite_SweepIsRateLimited(t *testing.T) {
	c := openCache(t)
	c.touches.mu.Lock()
	c.touches.last = make(map[SuiteKey]time.Time)
	now := time.Now()
	for i := range touchSweepAt {
		c.touches.last[sk("fresh", "/"+strings.Repeat("f", i+1))] = now
	}
	c.touches.mu.Unlock()
	c.noteTouch(sk("new", "/1")) // a sweep: nothing is old; it keeps 4096
	c.touches.mu.Lock()
	old := now.Add(-2 * touchEvery)
	for i := range 100 {
		c.touches.last[sk("fresh", "/"+strings.Repeat("f", i+1))] = old
	}
	c.touches.mu.Unlock()
	c.noteTouch(sk("new", "/2")) // within the minute and not doubled: no sweep
	if n := c.TouchedSuites(); n != touchSweepAt+2 {
		t.Fatalf("map = %d entries, want %d (no second sweep within the minute)", n, touchSweepAt+2)
	}
	c.touches.mu.Lock()
	c.touches.swept = c.touches.swept.Add(-touchSweepEvery)
	c.touches.mu.Unlock()
	c.noteTouch(sk("new", "/3")) // a minute later: swept
	if n := c.TouchedSuites(); n != touchSweepAt+3-100 {
		t.Fatalf("map = %d entries, want %d (the 100 old entries swept)", n, touchSweepAt+3-100)
	}
}

// Spec C3: a vouched .deb serve touches the suite whose current snapshot
// declared it, so DeclaredHashesForPath names that suite.
func TestDeclaredHashesForPath_NamesTheVouchingSuites(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	const deb = "/ubuntu/pool/main/h/hello/hello_1_amd64.deb"
	debHash := strings.Repeat("a", 64)
	for _, suite := range []string{"/ubuntu/dists/noble", "/ubuntu/dists/noble-updates"} {
		h := seedBlob(t, c, "anchor of "+suite)
		id, _, err := c.InsertTrackedCandidateForTest(ctx, SnapshotCandidate{CanonicalScheme: "http",
			CanonicalHost: "archive.example", SuitePath: suite, InReleaseHash: &h})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.CommitAdoption(ctx, id, []SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: h, DeclaredSHA256: h}},
			nil, []PackageHash{{CanonicalScheme: "http", CanonicalHost: "archive.example", Path: deb,
				DeclaredSHA256: debHash, SnapshotID: id}}, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := c.DeclaredHashesForPath(ctx, "http", "archive.example", deb)
	if err != nil {
		t.Fatal(err)
	}
	var suites []string
	for _, r := range rows {
		suites = append(suites, r.SuitePath)
	}
	slices.Sort(suites)
	if !slices.Equal(suites, []string{"/ubuntu/dists/noble", "/ubuntu/dists/noble-updates"}) {
		t.Fatalf("vouching suites = %v", suites)
	}
}
