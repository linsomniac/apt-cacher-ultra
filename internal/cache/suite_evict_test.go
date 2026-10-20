package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeGuard is a SuiteGuard: it holds any suite not marked busy, records
// the order it was asked in and how each hold was released, and runs
// onHold (if set) inside HoldSuite — before the writer operation.
type fakeGuard struct {
	mu       sync.Mutex
	busy     map[SuiteKey]bool
	asked    []SuiteKey
	released map[SuiteKey][]bool
	onHold   func(SuiteKey)
}

func newFakeGuard() *fakeGuard {
	return &fakeGuard{busy: map[SuiteKey]bool{}, released: map[SuiteKey][]bool{}}
}

func (g *fakeGuard) HoldSuite(key SuiteKey) (func(deleted bool), bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked = append(g.asked, key)
	if g.busy[key] {
		return nil, false
	}
	if g.onHold != nil {
		g.onHold(key)
	}
	return func(deleted bool) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.released[key] = append(g.released[key], deleted)
	}, true
}

const evictNow = int64(1_800_000_000)
const evictDay = int64(24 * 3600)

func sk(host, suite string) SuiteKey {
	return SuiteKey{CanonicalScheme: "http", CanonicalHost: host, SuitePath: suite}
}

// seedRow creates a row whose client-interest stamp is requested.
func seedRow(t *testing.T, c *Cache, key SuiteKey, requested int64) {
	t.Helper()
	res, err := c.CreateSuite(context.Background(), SuiteSeed{SuiteKey: key, RequestedAt: requested}, SuiteCaps{}, nil)
	if err != nil || res.Outcome != SuiteCreated {
		t.Fatalf("seed %v: %+v, %v", key, res, err)
	}
}

// adoptRow gives an existing row a current snapshot.
func adoptRow(t *testing.T, c *Cache, key SuiteKey) int64 {
	t.Helper()
	ctx := context.Background()
	h := seedBlob(t, c, "anchor of "+key.SuitePath)
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{CanonicalScheme: key.CanonicalScheme,
		CanonicalHost: key.CanonicalHost, SuitePath: key.SuitePath, InReleaseHash: &h})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id, []SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: h, DeclaredSHA256: h}},
		nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	return id
}

func rowExists(t *testing.T, c *Cache, key SuiteKey) bool {
	t.Helper()
	_, err := c.GetSuiteFreshness(context.Background(), key.CanonicalScheme, key.CanonicalHost, key.SuitePath)
	if err != nil && !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

// fourRows fills a cache to a global cap of 4: an adopted suite (the
// idlest, never evictable), two idle suites without a snapshot, and one
// requested an hour ago (not idle).
func fourRows(t *testing.T, c *Cache) {
	t.Helper()
	seedRow(t, c, sk("a", "/adopted"), evictNow-5*evictDay)
	adoptRow(t, c, sk("a", "/adopted"))
	seedRow(t, c, sk("b", "/idle"), evictNow-3*evictDay)
	seedRow(t, c, sk("c", "/idle2"), evictNow-2*evictDay)
	seedRow(t, c, sk("d", "/fresh"), evictNow-3600)
}

// Spec C3 / R-D1: at the global cap, CreateSuite evicts the
// least-recently-requested suite that has no current snapshot and has been
// idle for 24 h, under its mutex (the guard), in the creating transaction:
// the evictee's snapshots are retired (CommitAdoption refuses its
// candidate), its url_path rows are left to GC, and the guard is told the
// row is gone.
func TestCreateSuite_EvictsTheIdlestSuiteAtTheGlobalCap(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, evictNow)()
	fourRows(t, c)
	h := seedBlob(t, c, "candidate of /idle")
	cand, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "b",
		SuitePath: "/idle", InReleaseHash: &h})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.PutURLPath(ctx, URLPath{CanonicalScheme: "http", CanonicalHost: "b", Path: "/idle/InRelease",
		UpstreamURL: "http://b/idle/InRelease", IsMetadata: true}); err != nil {
		t.Fatal(err)
	}
	if err := c.MarkSignedSeenBatch(ctx, []SignedSeenMark{{CanonicalScheme: "http", CanonicalHost: "b", Base: "/idle",
		SeenAt: evictNow - 3*evictDay}}); err != nil {
		t.Fatal(err)
	}
	g := newFakeGuard()
	res, err := c.CreateSuite(ctx, SuiteSeed{SuiteKey: sk("e", "/new"), RequestedAt: evictNow}, SuiteCaps{Global: 4}, g)
	if err != nil || res.Outcome != SuiteCreated || res.Evicted == nil || *res.Evicted != sk("b", "/idle") {
		t.Fatalf("CreateSuite = %+v (evicted %v), %v; want created, evicting b/idle", res, res.Evicted, err)
	}
	if !slices.Equal(g.asked, []SuiteKey{sk("b", "/idle")}) || !slices.Equal(g.released[sk("b", "/idle")], []bool{true}) {
		t.Fatalf("guard asked %v, released %v; want b/idle held and released as deleted", g.asked, g.released)
	}
	if rowExists(t, c, sk("b", "/idle")) || !rowExists(t, c, sk("e", "/new")) || !rowExists(t, c, sk("a", "/adopted")) {
		t.Fatal("rows after the eviction: want b/idle gone, e/new and a/adopted present")
	}
	if snap, err := c.GetSuiteSnapshot(ctx, cand); err != nil || snap.RetiredAt == nil || *snap.RetiredAt != evictNow {
		t.Fatalf("evictee's candidate = %+v, %v; want retired at %d", snap, err, evictNow)
	}
	if err := c.CommitAdoption(ctx, cand, nil, nil, nil, nil, false); !errors.Is(err, ErrSnapshotRetired) {
		t.Fatalf("CommitAdoption of the evictee's candidate = %v, want ErrSnapshotRetired", err)
	}
	if _, err := c.LookupURL(ctx, "http", "b", "/idle/InRelease"); err != nil {
		t.Fatalf("the evictee's url_path row was deleted (%v): url_path GC owns it", err)
	}
	if _, ok, err := c.SignedSeen(ctx, "http", "b", "/idle"); err != nil || !ok {
		t.Fatalf("the evictee's signed-seen marker is gone (%v): markers survive reaping (spec C3)", err)
	}
}

// A busy candidate (a check or an adoption holds its mutex) is skipped for
// the next one; nothing busy is ever deleted.
func TestCreateSuite_EvictionSkipsBusySuites(t *testing.T) {
	c := openCache(t)
	defer stubNow(t, evictNow)()
	fourRows(t, c)
	g := newFakeGuard()
	g.busy[sk("b", "/idle")] = true
	res, err := c.CreateSuite(context.Background(), SuiteSeed{SuiteKey: sk("e", "/new"), RequestedAt: evictNow},
		SuiteCaps{Global: 4}, g)
	if err != nil || res.Outcome != SuiteCreated || res.Evicted == nil || *res.Evicted != sk("c", "/idle2") {
		t.Fatalf("CreateSuite = %+v, %v; want created, evicting c/idle2", res, err)
	}
	if !slices.Equal(g.asked, []SuiteKey{sk("b", "/idle"), sk("c", "/idle2")}) || len(g.released[sk("b", "/idle")]) != 0 {
		t.Fatalf("guard asked %v, released %v", g.asked, g.released)
	}
	if !rowExists(t, c, sk("b", "/idle")) {
		t.Fatal("the busy suite was deleted")
	}
}

// With nothing evictable (every suite adopted, recently requested or
// busy), a creation at the global cap is refused and nothing is deleted.
func TestCreateSuite_RefusesWhenNothingIsEvictable(t *testing.T) {
	c := openCache(t)
	defer stubNow(t, evictNow)()
	fourRows(t, c)
	g := newFakeGuard()
	g.busy[sk("b", "/idle")], g.busy[sk("c", "/idle2")] = true, true
	res, err := c.CreateSuite(context.Background(), SuiteSeed{SuiteKey: sk("e", "/new"), RequestedAt: evictNow},
		SuiteCaps{Global: 4}, g)
	if err != nil || res.Outcome != SuiteRejectedGlobal || res.Evicted != nil {
		t.Fatalf("CreateSuite = %+v, %v; want global_cap, nothing evicted", res, err)
	}
	if st, err := c.GetSuiteStats(context.Background()); err != nil || st.Tracked != 4 {
		t.Fatalf("tracked = %+v, %v; want 4", st, err)
	}
}

// Ruling PD-8: above the cap (a lowered max_suites, an upgraded v6 table)
// one eviction cannot make room, but each creation attempt still evicts
// one idle suite — and is refused — so the table converges under the 24 h
// rule: here the first attempt evicts f/idle3 and is refused, and the next
// one, at the cap, evicts b/idle and creates its row.
func TestCreateSuite_AboveTheCapEvictsOneAndRefuses(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, evictNow)()
	fourRows(t, c)
	seedRow(t, c, sk("f", "/idle3"), evictNow-4*evictDay)
	g := newFakeGuard()
	res, err := c.CreateSuite(ctx, SuiteSeed{SuiteKey: sk("e", "/new"), RequestedAt: evictNow}, SuiteCaps{Global: 4}, g)
	if err != nil || res.Outcome != SuiteRejectedGlobal || res.Evicted == nil || *res.Evicted != sk("f", "/idle3") {
		t.Fatalf("CreateSuite = %+v, %v; want global_cap, evicting f/idle3", res, err)
	}
	if !slices.Equal(g.released[sk("f", "/idle3")], []bool{true}) || rowExists(t, c, sk("f", "/idle3")) || rowExists(t, c, sk("e", "/new")) {
		t.Fatalf("released %v; want f/idle3 deleted and released as deleted, e/new not created", g.released)
	}
	res, err = c.CreateSuite(ctx, SuiteSeed{SuiteKey: sk("e", "/new2"), RequestedAt: evictNow}, SuiteCaps{Global: 4}, g)
	if err != nil || res.Outcome != SuiteCreated || res.Evicted == nil || *res.Evicted != sk("b", "/idle") {
		t.Fatalf("CreateSuite at the cap = %+v, %v; want created, evicting b/idle", res, err)
	}
	if st, err := c.GetSuiteStats(ctx); err != nil || st.Tracked != 4 {
		t.Fatalf("tracked = %+v, %v; want 4 (the cap)", st, err)
	}
}

// A creation refused at its host's cap evicts nothing, even at the global
// cap with an evictable suite: the per-host check comes first.
func TestCreateSuite_HostCapRefusalEvictsNothing(t *testing.T) {
	c := openCache(t)
	defer stubNow(t, evictNow)()
	fourRows(t, c) // hosts a, b, c, d (one row each); b and c are evictable
	res, err := c.CreateSuite(context.Background(), SuiteSeed{SuiteKey: sk("d", "/new"), RequestedAt: evictNow},
		SuiteCaps{Global: 4, PerHost: 1}, newFakeGuard())
	if err != nil || res.Outcome != SuiteRejectedHost || res.Evicted != nil {
		t.Fatalf("CreateSuite = %+v, %v; want host_cap, nothing evicted", res, err)
	}
	if !rowExists(t, c, sk("b", "/idle")) {
		t.Fatal("a host-cap refusal evicted a suite")
	}
}

// The compare-and-delete retires and deletes only the evictee, not a suite
// with the same path on another host.
func TestCreateSuite_EvictionTouchesOnlyTheEvictee(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, evictNow)()
	seedRow(t, c, sk("b", "/idle"), evictNow-3*evictDay)
	seedRow(t, c, sk("z", "/idle"), evictNow-3600)
	id := adoptRow(t, c, sk("z", "/idle"))
	res, err := c.CreateSuite(ctx, SuiteSeed{SuiteKey: sk("e", "/new"), RequestedAt: evictNow}, SuiteCaps{Global: 2}, newFakeGuard())
	if err != nil || res.Evicted == nil || *res.Evicted != sk("b", "/idle") {
		t.Fatalf("CreateSuite = %+v, %v; want b/idle evicted", res, err)
	}
	if !rowExists(t, c, sk("z", "/idle")) {
		t.Fatal("the eviction deleted a same-path suite on another host")
	}
	if snap, err := c.GetSuiteSnapshot(ctx, id); err != nil || snap.RetiredAt != nil {
		t.Fatalf("the eviction retired another host's snapshot: %+v, %v", snap, err)
	}
}

// R-D1: deleteSuiteTx is keyed by scheme too — evicting http://b/idle
// leaves an adopted https://b/idle (its row and its current snapshot)
// alone. Phase F's reaper reuses the delete for adopted suites, where a
// cross-scheme retire would leak the twin's pins (final review, tests I-2).
func TestCreateSuite_EvictionLeavesTheOtherSchemesTwin(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, evictNow)()
	seedRow(t, c, sk("b", "/idle"), evictNow-3*evictDay)
	twin := SuiteKey{CanonicalScheme: "https", CanonicalHost: "b", SuitePath: "/idle"}
	seedRow(t, c, twin, evictNow-3600)
	id := adoptRow(t, c, twin)
	res, err := c.CreateSuite(ctx, SuiteSeed{SuiteKey: sk("e", "/new"), RequestedAt: evictNow}, SuiteCaps{Global: 2}, newFakeGuard())
	if err != nil || res.Evicted == nil || *res.Evicted != sk("b", "/idle") {
		t.Fatalf("CreateSuite = %+v, %v; want http://b/idle evicted", res, err)
	}
	if !rowExists(t, c, twin) {
		t.Fatal("the eviction deleted the https twin's row")
	}
	if snap, err := c.GetSuiteSnapshot(ctx, id); err != nil || snap.RetiredAt != nil {
		t.Fatalf("the eviction retired the https twin's current snapshot: %+v, %v", snap, err)
	}
}

// Ruling PF-10 (design review M-9 widened to every suite-row delete): a
// cap eviction moves the evictee base's signed-seen markers, under both
// schemes, up to the eviction — MAX(last_seen_at, now) — so a dormant
// suite's year-old marker is not swept the day after its eviction made it
// an orphan. The evictee is a /. twin: its base's markers are the ones
// moved. Each case has the other scheme's old marker move up (a stamp
// limited to the evictee's scheme fails both); the first leaves the
// evictee's own scheme without a marker, which must stay absent (an
// upsert, markSignedSeen, would create it: never-downgrade would then
// refuse the base's unsigned return); the second has a stamp ahead of now
// on the evictee's scheme, never lowered (ruling PF-1). Another host's or
// another base's marker is left alone, and no marker is created.
func TestCreateSuite_EvictionMovesTheBaseMarkersUp(t *testing.T) {
	old, ahead := evictNow-400*evictDay, evictNow+evictDay
	other := []SignedSeenMark{{CanonicalScheme: "http", CanonicalHost: "g", Base: "/flat", SeenAt: old},
		{CanonicalScheme: "http", CanonicalHost: "b", Base: "/kept", SeenAt: old}}
	for _, tc := range []struct {
		name    string
		evictee SuiteKey
		marks   []SignedSeenMark // the evictee base's markers, then other's
		want    []int64          // each mark's stamp after the eviction
		absent  []SignedSeenMark // no marker before the eviction, none after it
	}{
		{name: "the other scheme's moves up and none is created",
			evictee: sk("b", "/flat/."),
			marks:   []SignedSeenMark{{CanonicalScheme: "https", CanonicalHost: "b", Base: "/flat", SeenAt: old}},
			want:    []int64{evictNow},
			absent:  []SignedSeenMark{{CanonicalScheme: "http", CanonicalHost: "b", Base: "/flat"}}},
		{name: "the other scheme's moves up and one ahead is kept",
			evictee: SuiteKey{CanonicalScheme: "https", CanonicalHost: "b", SuitePath: "/flat/."},
			marks: []SignedSeenMark{{CanonicalScheme: "http", CanonicalHost: "b", Base: "/flat", SeenAt: old},
				{CanonicalScheme: "https", CanonicalHost: "b", Base: "/flat", SeenAt: ahead}},
			want: []int64{evictNow, ahead}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := openCache(t)
			ctx := context.Background()
			defer stubNow(t, evictNow)()
			seedRow(t, c, tc.evictee, evictNow-3*evictDay)
			seedRow(t, c, sk("b", "/kept"), evictNow-3600) // requested within the day: not evictable
			marks := append(append([]SignedSeenMark{}, tc.marks...), other...)
			want := append(append([]int64{}, tc.want...), old, old)
			if err := c.MarkSignedSeenBatch(ctx, marks); err != nil {
				t.Fatal(err)
			}
			res, err := c.CreateSuite(ctx, SuiteSeed{SuiteKey: sk("e", "/new"), RequestedAt: evictNow}, SuiteCaps{Global: 2}, newFakeGuard())
			if err != nil || res.Evicted == nil || *res.Evicted != tc.evictee {
				t.Fatalf("CreateSuite = %+v, %v; want %v evicted", res, err, tc.evictee)
			}
			for i, m := range marks {
				at, ok, err := c.SignedSeen(ctx, m.CanonicalScheme, m.CanonicalHost, m.Base)
				if err != nil || !ok || at != want[i] {
					t.Errorf("marker %+v after the eviction: %d (present %v, %v); want %d", m, at, ok, err, want[i])
				}
			}
			for _, m := range tc.absent {
				if at, ok, err := c.SignedSeen(ctx, m.CanonicalScheme, m.CanonicalHost, m.Base); err != nil || ok {
					t.Errorf("marker %s://%s%s after the eviction: present %v at %d (%v); want none: an eviction creates no marker",
						m.CanonicalScheme, m.CanonicalHost, m.Base, ok, at, err)
				}
			}
			var n int
			if err := c.db.QueryRow(`SELECT COUNT(*) FROM suite_signed_seen`).Scan(&n); err != nil || n != len(marks) {
				t.Fatalf("%d markers after the eviction (%v); want %d: an eviction creates none", n, err, len(marks))
			}
		})
	}
}

// Design review M3 (final review, tests I-1): a CreateSuite whose writer
// operation fails — its context ends after the candidate is held, before
// the writer runs it — still releases the held candidate, once, as not
// deleted (a leaked hold keeps that suite's mutex locked: no check,
// adoption or eviction of it until a restart), and evicts and creates
// nothing.
func TestCreateSuite_AFailedSubmitReleasesItsCandidate(t *testing.T) {
	// The cancelled operation may still run on the writer after CreateSuite
	// returned (submitWrite's documented ctx path), so nowUnix is restored
	// only after the cache's Close (cleanups run last-in first-out): a
	// deferred restore would race with it.
	t.Cleanup(stubNow(t, evictNow))
	c := openCache(t)
	fourRows(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	g := newFakeGuard()
	g.onHold = func(SuiteKey) { cancel() }
	if _, err := c.CreateSuite(ctx, SuiteSeed{SuiteKey: sk("e", "/new"), RequestedAt: evictNow}, SuiteCaps{Global: 4}, g); err == nil {
		t.Fatal("premise: the submit fails on an ended context")
	}
	if got := g.released[sk("b", "/idle")]; !slices.Equal(got, []bool{false}) {
		t.Fatalf("released %v; want b/idle released once, as not deleted", g.released)
	}
	if !rowExists(t, c, sk("b", "/idle")) || rowExists(t, c, sk("e", "/new")) {
		t.Fatal("a failed creation evicted or created a row")
	}
}

// Final review, concurrency M1: a panic between the hold and the settle —
// injected right after the hold, through the test seam — still releases
// the held candidate, once, as not deleted, and continues to the caller.
// CreateSuite (onboarding) and StoreFetched (a miss) share the lifecycle.
func TestCreateSuite_APanicReleasesTheHeldCandidate(t *testing.T) {
	c := openCache(t)
	defer stubNow(t, evictNow)()
	fourRows(t, c)
	c.heldHook = func(SuiteKey) { panic("boom") }
	seed := SuiteSeed{SuiteKey: sk("e", "/new"), RequestedAt: evictNow}
	row := fetchedRow(t, c, seed.SuiteKey, "/InRelease")
	for _, tc := range []struct {
		name   string
		create func(g *fakeGuard)
	}{
		{"CreateSuite", func(g *fakeGuard) {
			_, _ = c.CreateSuite(context.Background(), seed, SuiteCaps{Global: 4}, g)
		}},
		{"StoreFetched", func(g *fakeGuard) {
			_, _ = c.StoreFetched(context.Background(), FetchedRow{Row: row, Suite: seed.SuiteKey, Create: &seed},
				SuiteCaps{Global: 4}, g)
		}},
	} {
		g := newFakeGuard()
		func() {
			defer func() {
				if v := recover(); v != "boom" {
					t.Errorf("%s: recovered %v, want the panic to continue to the caller", tc.name, v)
				}
			}()
			tc.create(g)
		}()
		if got := g.released[sk("b", "/idle")]; !slices.Equal(got, []bool{false}) {
			t.Errorf("%s: released %v; want b/idle released once, as not deleted", tc.name, g.released)
		}
	}
	if !rowExists(t, c, sk("b", "/idle")) || rowExists(t, c, seed.SuiteKey) {
		t.Fatal("a creation that panicked before its writer operation evicted or created a row")
	}
}

// Design review M4: a held candidate whose row a concurrent creation
// evicted before this one held it (here: deleted inside HoldSuite, the
// table refilled behind it) is released as gone — its in-memory state is
// stale — and the creation tries once more with a fresh candidate list; a
// second vanished candidate ends it with a refusal (at most two attempts).
// An unused candidate that vanished (room was left) is released as gone
// too.
func TestCreateSuite_RetriesOnceWhenTheCandidateVanished(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, evictNow)()
	fourRows(t, c)
	refills := 0
	evictedElsewhere := func(k SuiteKey) {
		if _, err := c.db.Exec(`DELETE FROM suite_freshness WHERE canonical_host = ? AND suite_path = ?`,
			k.CanonicalHost, k.SuitePath); err != nil {
			t.Error(err)
		}
		refills++
		seedRow(t, c, sk("r", fmt.Sprintf("/refill%d", refills)), evictNow-3600)
	}
	g := newFakeGuard()
	g.onHold = func(k SuiteKey) {
		if k == sk("b", "/idle") {
			evictedElsewhere(k)
		}
	}
	res, err := c.CreateSuite(ctx, SuiteSeed{SuiteKey: sk("e", "/new"), RequestedAt: evictNow}, SuiteCaps{Global: 4}, g)
	if err != nil || res.Outcome != SuiteCreated || res.Evicted == nil || *res.Evicted != sk("c", "/idle2") {
		t.Fatalf("CreateSuite = %+v, %v; want created on the second attempt, evicting c/idle2", res, err)
	}
	if !slices.Equal(g.released[sk("b", "/idle")], []bool{true}) || !slices.Equal(g.released[sk("c", "/idle2")], []bool{true}) {
		t.Fatalf("released %v; want b/idle released as gone and c/idle2 as deleted", g.released)
	}
	// Every candidate vanishes: two attempts, then a refusal.
	seedRow(t, c, sk("f", "/idle4"), evictNow-3*evictDay)
	seedRow(t, c, sk("g", "/idle5"), evictNow-3*evictDay)
	g2 := newFakeGuard()
	g2.onHold = evictedElsewhere
	res, err = c.CreateSuite(ctx, SuiteSeed{SuiteKey: sk("e", "/new2"), RequestedAt: evictNow}, SuiteCaps{Global: 6}, g2)
	if err != nil || res.Outcome != SuiteRejectedGlobal || len(g2.asked) != 2 {
		t.Fatalf("CreateSuite = %+v, %v, asked %v; want global_cap after two candidates", res, err, g2.asked)
	}
	for _, k := range g2.asked {
		if !slices.Equal(g2.released[k], []bool{true}) {
			t.Fatalf("released %v; want each vanished candidate released as gone", g2.released)
		}
	}
	// Below the cap a held candidate goes unused; if it vanished, it is
	// released as gone all the same.
	seedRow(t, c, sk("h", "/idle6"), evictNow-3*evictDay)
	g3 := newFakeGuard()
	g3.onHold = func(k SuiteKey) {
		if _, err := c.db.Exec(`DELETE FROM suite_freshness WHERE canonical_host = ? AND suite_path = ?`,
			k.CanonicalHost, k.SuitePath); err != nil {
			t.Error(err)
		}
	}
	res, err = c.CreateSuite(ctx, SuiteSeed{SuiteKey: sk("e", "/new3"), RequestedAt: evictNow}, SuiteCaps{Global: 10}, g3)
	if err != nil || res.Outcome != SuiteCreated || len(g3.asked) != 1 || !slices.Equal(g3.released[g3.asked[0]], []bool{true}) {
		t.Fatalf("CreateSuite = %+v, %v; asked %v, released %v; want created and the vanished candidate released as gone",
			res, err, g3.asked, g3.released)
	}
}

// Design review M5: within suiteEvictCandidates of the global cap a
// creation already holds a candidate (creations racing at the boundary
// each find their own), and releases it unused while there is room; well
// below the cap it holds none.
func TestCreateSuite_HoldsACandidateNearTheCap(t *testing.T) {
	c := openCache(t)
	defer stubNow(t, evictNow)()
	fourRows(t, c)
	g := newFakeGuard()
	res, err := c.CreateSuite(context.Background(), SuiteSeed{SuiteKey: sk("e", "/new"), RequestedAt: evictNow},
		SuiteCaps{Global: 4 + suiteEvictCandidates}, g)
	if err != nil || res.Outcome != SuiteCreated || res.Evicted != nil {
		t.Fatalf("CreateSuite = %+v, %v; want created, nothing evicted", res, err)
	}
	if !slices.Equal(g.asked, []SuiteKey{sk("b", "/idle")}) || !slices.Equal(g.released[sk("b", "/idle")], []bool{false}) {
		t.Fatalf("asked %v, released %v; want b/idle held and released unused", g.asked, g.released)
	}
	g = newFakeGuard()
	if _, err := c.CreateSuite(context.Background(), SuiteSeed{SuiteKey: sk("e", "/new2"), RequestedAt: evictNow},
		SuiteCaps{Global: 6 + suiteEvictCandidates}, g); err != nil {
		t.Fatal(err)
	}
	if len(g.asked) != 0 {
		t.Fatalf("asked %v well below the cap; want none", g.asked)
	}
}

// clockGuard is a fakeGuard that is also a ClockStepReporter.
type clockGuard struct {
	*fakeGuard
	reports []string
}

func (g *clockGuard) ReportClockStep(stamp string, key SuiteKey, stored, now int64) {
	g.reports = append(g.reports, fmt.Sprintf("%s %s%s %d %d", stamp, key.CanonicalHost, key.SuitePath, stored-now, now))
}

// Ruling PD-11 (design review M11): a suite whose last_requested_at lies
// more than 2 s in the future is never evicted (deleteSuiteTx), and the
// eviction tells a reporting guard, which rate-limits spec C4's
// clock_step_detected; a stamp within the slack is not a clock step.
func TestCreateSuite_ReportsAFutureInterestStamp(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, evictNow)()
	seedRow(t, c, sk("a", "/slack"), evictNow+futureStampSlack)
	g := &clockGuard{fakeGuard: newFakeGuard()}
	if _, err := c.CreateSuite(ctx, SuiteSeed{SuiteKey: sk("e", "/new"), RequestedAt: evictNow}, SuiteCaps{Global: 1}, g); err != nil {
		t.Fatal(err)
	}
	if len(g.reports) != 0 {
		t.Fatalf("reports = %q for a stamp within the slack, want none", g.reports)
	}
	seedRow(t, c, sk("a", "/future"), evictNow+evictDay)
	res, err := c.CreateSuite(ctx, SuiteSeed{SuiteKey: sk("e", "/new"), RequestedAt: evictNow}, SuiteCaps{Global: 2}, g)
	if err != nil || res.Outcome != SuiteRejectedGlobal || res.Evicted != nil {
		t.Fatalf("CreateSuite = %+v, %v; want global_cap, nothing evicted", res, err)
	}
	if want := fmt.Sprintf("last_requested_at a/future %d %d", evictDay, evictNow); !slices.Equal(g.reports, []string{want}) {
		t.Fatalf("reports = %q, want [%q]", g.reports, want)
	}
	if !rowExists(t, c, sk("a", "/future")) {
		t.Fatal("a suite requested in the future was evicted")
	}
}

// Deferred minor D-11: of several suites whose last_requested_at lies in
// the future, cap eviction reports the furthest one.
func TestCreateSuite_ReportsTheFurthestFutureStamp(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, evictNow)()
	seedRow(t, c, sk("a", "/near"), evictNow+evictDay)
	seedRow(t, c, sk("a", "/far"), evictNow+3*evictDay)
	seedRow(t, c, sk("a", "/mid"), evictNow+2*evictDay)
	g := &clockGuard{fakeGuard: newFakeGuard()}
	if _, err := c.CreateSuite(ctx, SuiteSeed{SuiteKey: sk("e", "/new"), RequestedAt: evictNow}, SuiteCaps{Global: 3}, g); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("last_requested_at a/far %d %d", 3*evictDay, evictNow); !slices.Equal(g.reports, []string{want}) {
		t.Fatalf("reports = %q, want [%q]", g.reports, want)
	}
}

// Deferred minor D-20 (design review M3): a creation whose submit fails
// although its transaction committed — its context ends right after the
// commit, before the result is handed back — purges the evictee as a
// successful one does: afterCreate reads the row, finds it gone, forgets
// the evictee's touch entry and releases it as deleted.
func TestCreateSuite_AFailedSubmitWhoseEvictionCommittedPurges(t *testing.T) {
	defer stubNow(t, evictNow)()
	c := openCache(t)
	fourRows(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	resume := make(chan struct{})
	var once sync.Once
	c.committedHook = func() { once.Do(func() { cancel(); <-resume }) }
	g := newFakeGuard()
	_, err := c.CreateSuite(ctx, SuiteSeed{SuiteKey: sk("e", "/new"), RequestedAt: evictNow}, SuiteCaps{Global: 4}, g)
	close(resume)
	if err == nil {
		t.Fatal("premise: the submit fails on the context that ended after the commit")
	}
	if rowExists(t, c, sk("b", "/idle")) {
		t.Fatal("premise: the eviction committed")
	}
	if got := g.released[sk("b", "/idle")]; !slices.Equal(got, []bool{true}) {
		t.Fatalf("released %v; want b/idle released once, as deleted", g.released)
	}
	c.touches.mu.Lock()
	_, touched := c.touches.last[sk("b", "/idle")]
	c.touches.mu.Unlock()
	if touched {
		t.Fatal("the evictee's touch entry stayed")
	}
}

// Deferred minor D-15: a failed suite-row creation names no caller it was
// not: the miss path (StoreFetched) and onboarding (CreateSuite) both say
// "create suite row", never "CreateSuite:". The eviction candidate's hold,
// the first database call of a creation with a cap set, says "hold
// eviction candidate" under the caller's operation (the Task 2 review's
// minor 3: before, no test reached its errors).
func TestCreateSuite_ErrorsNameTheOperation(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	if _, err := c.db.Exec(`CREATE TRIGGER inject_insert_failure BEFORE INSERT ON suite_freshness
BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatal(err)
	}
	seed := SuiteSeed{SuiteKey: sk("h", "/r/dists/x"), RequestedAt: 1}
	fetched := FetchedRow{Row: URLPath{CanonicalScheme: "http", CanonicalHost: "h",
		Path: "/r/dists/x/InRelease", UpstreamURL: "http://h/r/dists/x/InRelease", IsMetadata: true},
		Suite: seed.SuiteKey, Create: &seed}
	_, cerr := c.CreateSuite(ctx, seed, SuiteCaps{}, nil)
	_, serr := c.StoreFetched(ctx, fetched, SuiteCaps{}, nil)
	for name, err := range map[string]error{"CreateSuite": cerr, "StoreFetched": serr} {
		if err == nil || !strings.Contains(err.Error(), "create suite row: insert") || strings.Contains(err.Error(), "CreateSuite:") {
			t.Errorf("%s: err = %v, want it to say \"create suite row: insert\" and never \"CreateSuite:\"", name, err)
		}
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel() // the candidate count fails at once
	_, cerr = c.CreateSuite(cancelled, seed, SuiteCaps{Global: 1}, nil)
	_, serr = c.StoreFetched(cancelled, fetched, SuiteCaps{Global: 1}, nil)
	for name, err := range map[string]error{"CreateSuite": cerr, "StoreFetched": serr} {
		want := name + ": hold eviction candidate: count suites: "
		if err == nil || !strings.HasPrefix(err.Error(), want) || !errors.Is(err, context.Canceled) {
			t.Errorf("%s with a cancelled context: err = %v, want it to start %q and wrap context.Canceled", name, err, want)
		}
	}
}

// Design review M3: after a failed submit (no result), afterCreate decides
// by reading the held candidate's row: absent — this creation's eviction
// committed after all — releases it as deleted; present releases it as
// kept. A result that names the eviction needs no read.
func TestAfterCreate_ReadsTheCandidatesRowUnlessTheResultSays(t *testing.T) {
	c := openCache(t)
	defer stubNow(t, evictNow)()
	seedRow(t, c, sk("b", "/kept"), evictNow-3*evictDay)
	var got []bool
	held := func(k SuiteKey) *heldSuite {
		return &heldSuite{key: k, release: func(deleted bool) { got = append(got, deleted) }}
	}
	c.afterCreate(sk("e", "/new"), CreateResult{}, held(sk("b", "/kept")))
	c.afterCreate(sk("e", "/new"), CreateResult{}, held(sk("b", "/gone")))
	kept := sk("b", "/kept")
	c.afterCreate(sk("e", "/new"), CreateResult{Outcome: SuiteCreated, Evicted: &kept}, held(kept))
	if !slices.Equal(got, []bool{false, true, true}) {
		t.Fatalf("released %v; want [false true true] (present: kept; absent: deleted; evicted: deleted)", got)
	}
}

// SuiteCapUsage reports the table against the caps (ruling PD-8's startup
// WARN): the total, the hosts at or above the per-host cap — across
// schemes — and the fullest host.
func TestSuiteCapUsage(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	if u, err := c.SuiteCapUsage(ctx, 2); err != nil || u != (SuiteCapUsage{}) {
		t.Fatalf("empty table: %+v, %v", u, err)
	}
	for _, k := range []SuiteKey{sk("a", "/1"), {CanonicalScheme: "https", CanonicalHost: "a", SuitePath: "/2"}, sk("a", "/3"),
		sk("b", "/1"), sk("b", "/2"), sk("c", "/1")} {
		seedRow(t, c, k, evictNow)
	}
	u, err := c.SuiteCapUsage(ctx, 2)
	if err != nil || u != (SuiteCapUsage{Total: 6, HostsAtCap: 2, FullestHost: "a", FullestHostRows: 3}) {
		t.Fatalf("usage = %+v, %v; want 6 rows, 2 hosts at the cap, a fullest with 3", u, err)
	}
	if u, err := c.SuiteCapUsage(ctx, 0); err != nil || u.HostsAtCap != 0 || u.Total != 6 {
		t.Fatalf("no per-host cap: %+v, %v", u, err)
	}
}

// R-D1: the writer re-checks the candidate (compare-and-delete). A
// candidate requested after it was chosen — here inside HoldSuite, before
// the writer operation — is no longer idle: nothing is deleted. The
// creation retries once with a fresh list (FW-5); here that candidate,
// c/idle2, is requested in its window too, so after two attempts the
// creation is refused.
func TestCreateSuite_EvictionRechecksInTheWriter(t *testing.T) {
	c := openCache(t)
	defer stubNow(t, evictNow)()
	fourRows(t, c)
	g := newFakeGuard()
	g.onHold = func(k SuiteKey) {
		if _, err := c.db.Exec(`UPDATE suite_freshness SET last_requested_at = ? WHERE suite_path = ?`, evictNow, k.SuitePath); err != nil {
			t.Error(err)
		}
	}
	res, err := c.CreateSuite(context.Background(), SuiteSeed{SuiteKey: sk("e", "/new"), RequestedAt: evictNow},
		SuiteCaps{Global: 4}, g)
	if err != nil || res.Outcome != SuiteRejectedGlobal {
		t.Fatalf("CreateSuite = %+v, %v; want global_cap", res, err)
	}
	if !slices.Equal(g.released[sk("b", "/idle")], []bool{false}) || !rowExists(t, c, sk("b", "/idle")) {
		t.Fatalf("released %v; want b/idle kept", g.released)
	}
	if !slices.Equal(g.released[sk("c", "/idle2")], []bool{false}) || !rowExists(t, c, sk("c", "/idle2")) {
		t.Fatalf("released %v; want the retry's c/idle2 kept too", g.released)
	}
}

// Final review, concurrency M2 (ruling FW-5 1): a held candidate the
// writer keeps — requested between the candidate list and the
// transaction — makes the creation try once more with a fresh list, as a
// vanished one does, instead of refusing while another idle suite could
// make room. CreateSuite and StoreFetched both retry (at most two
// attempts; TestCreateSuite_EvictionRechecksInTheWriter keeps every
// candidate and is refused after two).
func TestCreateSuite_RetriesOnceWhenTheCandidateIsKept(t *testing.T) {
	for _, viaMiss := range []bool{false, true} {
		c := openCache(t)
		g := newFakeGuard()
		g.onHold = func(k SuiteKey) {
			if k == sk("b", "/idle") { // a client asks for b/idle in the window
				if _, err := c.db.Exec(`UPDATE suite_freshness SET last_requested_at = ? WHERE suite_path = '/idle'`, evictNow); err != nil {
					t.Error(err)
				}
			}
		}
		seed := SuiteSeed{SuiteKey: sk("e", "/new"), RequestedAt: evictNow}
		// The deferred restore runs even when a helper calls t.Fatal, so the
		// package clock is never left stubbed for later tests.
		res, err := func() (CreateResult, error) {
			defer stubNow(t, evictNow)()
			fourRows(t, c)
			if viaMiss {
				fr, err := c.StoreFetched(context.Background(), FetchedRow{Row: fetchedRow(t, c, seed.SuiteKey, "/InRelease"),
					Suite: seed.SuiteKey, Create: &seed}, SuiteCaps{Global: 4}, g)
				return fr.Create, err
			}
			return c.CreateSuite(context.Background(), seed, SuiteCaps{Global: 4}, g)
		}()
		if err != nil || res.Outcome != SuiteCreated || res.Evicted == nil || *res.Evicted != sk("c", "/idle2") {
			t.Fatalf("miss=%v: result %+v, %v; want created on the second attempt, evicting c/idle2", viaMiss, res, err)
		}
		if !slices.Equal(g.asked, []SuiteKey{sk("b", "/idle"), sk("c", "/idle2")}) ||
			!slices.Equal(g.released[sk("b", "/idle")], []bool{false}) || !slices.Equal(g.released[sk("c", "/idle2")], []bool{true}) {
			t.Fatalf("miss=%v: asked %v, released %v; want b/idle kept, c/idle2 evicted", viaMiss, g.asked, g.released)
		}
	}
}

// Final review, concurrency M2 (ruling FW-5 2) and Task 5 minor 3: at most
// eight candidates are listed (spec C3), and a refusal because every
// listed one is busy — held by checks, adoptions or other creations — is
// transient (CandidatesBusy): the handler caches it for a minute, not ten.
// Here eight idle suites are busy and a ninth, idle too, is never asked.
// A refusal with no candidate at all (everything adopted or recently
// requested) is not transient.
func TestCreateSuite_ARefusalWithEveryCandidateBusyIsTransient(t *testing.T) {
	c := openCache(t)
	defer stubNow(t, evictNow)()
	g := newFakeGuard()
	for i := range suiteEvictCandidates + 1 {
		k := sk(fmt.Sprintf("h%d", i), "/idle")
		seedRow(t, c, k, evictNow-evictDay-int64(suiteEvictCandidates-i)) // h0 the idlest, h8 the least idle
		if i < suiteEvictCandidates {
			g.busy[k] = true
		}
	}
	caps := SuiteCaps{Global: suiteEvictCandidates + 1}
	res, err := c.CreateSuite(context.Background(), SuiteSeed{SuiteKey: sk("e", "/new"), RequestedAt: evictNow}, caps, g)
	if err != nil || res.Outcome != SuiteRejectedGlobal || res.Evicted != nil || !res.CandidatesBusy {
		t.Fatalf("CreateSuite = %+v, %v; want global_cap with CandidatesBusy, nothing evicted", res, err)
	}
	if len(g.asked) != suiteEvictCandidates || slices.Contains(g.asked, sk(fmt.Sprintf("h%d", suiteEvictCandidates), "/idle")) {
		t.Fatalf("asked %v; want the eight idlest only (at most eight candidates)", g.asked)
	}
	// Nothing evictable: a lasting refusal.
	c2 := openCache(t)
	fourRows(t, c2)
	if _, err := c2.db.Exec(`UPDATE suite_freshness SET last_requested_at = ? WHERE current_snapshot_id IS NULL`, evictNow); err != nil {
		t.Fatal(err)
	}
	res, err = c2.CreateSuite(context.Background(), SuiteSeed{SuiteKey: sk("e", "/new"), RequestedAt: evictNow}, SuiteCaps{Global: 4}, newFakeGuard())
	if err != nil || res.Outcome != SuiteRejectedGlobal || res.CandidatesBusy {
		t.Fatalf("CreateSuite with nothing evictable = %+v, %v; want global_cap without CandidatesBusy", res, err)
	}
}

// Task 5 minor 4: the candidate list's cutoff is inclusive, as
// deleteSuiteTx's is (spec C3: idle for 24 h or more) — a suite last
// requested exactly 24 h ago is evicted.
func TestCreateSuite_ASuiteIdleExactly24HoursIsEvicted(t *testing.T) {
	c := openCache(t)
	defer stubNow(t, evictNow)()
	seedRow(t, c, sk("b", "/exact"), evictNow-evictDay)
	res, err := c.CreateSuite(context.Background(), SuiteSeed{SuiteKey: sk("e", "/new"), RequestedAt: evictNow}, SuiteCaps{Global: 1}, newFakeGuard())
	if err != nil || res.Outcome != SuiteCreated || res.Evicted == nil || *res.Evicted != sk("b", "/exact") {
		t.Fatalf("CreateSuite = %+v, %v; want created, evicting the suite idle exactly 24 h", res, err)
	}
}

// R-D1: deleteSuiteTx is a compare-and-delete phase F's reaper reuses: it
// deletes only a row still idle at the cutoff with the expected current
// snapshot (releasing that snapshot's pins), retires the suite's
// snapshots, and treats a future stamp (backward clock step) as not idle.
func TestDeleteSuiteTx_IsACompareAndDelete(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, evictNow)()
	key := sk("h", "/r/dists/x")
	seedRow(t, c, key, evictNow-2*evictDay)
	id := adoptRow(t, c, key)
	anchor, err := c.GetSnapshotMember(ctx, id, "InRelease")
	if err != nil {
		t.Fatal(err)
	}
	del := func(k SuiteKey, want *int64, cutoff int64) deleteOutcome {
		t.Helper()
		var out deleteOutcome
		if err := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
			tx, err := conn.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if out, err = deleteSuiteTx(ctx, tx, k, want, cutoff, evictNow); err != nil {
				return err
			}
			return tx.Commit()
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	other := id + 1
	for name, out := range map[string]deleteOutcome{
		"no current snapshot expected": del(key, nil, evictNow-evictDay),
		"another current snapshot":     del(key, &other, evictNow-evictDay),
		"requested after the cutoff":   del(key, &id, evictNow-3*evictDay),
	} {
		if out != suiteKept {
			t.Fatalf("%s: outcome %d, want kept", name, out)
		}
	}
	if out := del(sk("h", "/nope"), nil, evictNow); out != suiteNoRow {
		t.Fatalf("no such row: outcome %d, want suiteNoRow", out)
	}
	if del(key, &id, evictNow-evictDay) != suiteDeleted {
		t.Fatal("a qualifying row was not deleted")
	}
	if rowExists(t, c, key) {
		t.Fatal("row still present")
	}
	if b, err := c.GetBlob(ctx, anchor.BlobHash); err != nil || b.RefCount != 0 {
		t.Fatalf("anchor blob = %+v, %v; want its pin released (refcount 0)", b, err)
	}
	if snap, err := c.GetSuiteSnapshot(ctx, id); err != nil || snap.RetiredAt == nil {
		t.Fatalf("snapshot = %+v, %v; want retired", snap, err)
	}
	// A stamp in the future never qualifies for an idle cutoff
	// (ReapRegardless lets every row through: TestReapIdleSuite_*).
	future := sk("h", "/future")
	seedRow(t, c, future, evictNow+evictDay)
	if del(future, nil, evictNow) == suiteDeleted {
		t.Fatal("a row requested in the future was deleted")
	}
	// The cutoff is inclusive (spec C3: idle >= 24 h), and a row without an
	// interest stamp never qualifies for an idle cutoff.
	exact := sk("h", "/exact")
	seedRow(t, c, exact, evictNow-evictDay)
	if del(exact, nil, evictNow-evictDay) != suiteDeleted {
		t.Fatal("a suite idle exactly 24 h was not deleted")
	}
	null := sk("h", "/null")
	seedRow(t, c, null, evictNow-evictDay)
	if _, err := c.db.Exec(`UPDATE suite_freshness SET last_requested_at = NULL WHERE suite_path = '/null'`); err != nil {
		t.Fatal(err)
	}
	if del(null, nil, evictNow) != suiteKept {
		t.Fatal("a suite without an interest stamp was deleted")
	}
}

// Count, eviction and insert run in one writer transaction, so concurrent
// creations never exceed either cap.
func TestCreateSuite_ConcurrentCreationsNeverExceedTheCaps(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	caps := SuiteCaps{Global: 10, PerHost: 3}
	var wg sync.WaitGroup
	var mu sync.Mutex
	created := 0
	for h := range 8 {
		for s := range 5 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := c.CreateSuite(ctx, SuiteSeed{SuiteKey: sk(fmt.Sprintf("h%d", h), fmt.Sprintf("/s%d", s)),
					RequestedAt: nowUnix()}, caps, newFakeGuard())
				if err != nil {
					t.Error(err)
					return
				}
				if res.Outcome == SuiteCreated {
					mu.Lock()
					created++
					mu.Unlock()
				}
			}()
		}
	}
	wg.Wait()
	all, err := c.ListSuites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	perHost := map[string]int{}
	for _, s := range all {
		perHost[s.CanonicalHost]++
		if perHost[s.CanonicalHost] > 3 {
			t.Fatalf("host %s has %d rows, cap 3", s.CanonicalHost, perHost[s.CanonicalHost])
		}
	}
	if len(all) != 10 || created != 10 {
		t.Fatalf("rows = %d, created = %d; want 10 and 10 (the global cap)", len(all), created)
	}
}
