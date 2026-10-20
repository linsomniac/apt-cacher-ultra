package freshness

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/gc"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

const reapAge = 31 * 24 * time.Hour // past gc.suite_idle_ttl's 30 days

// newRigReaper is a reaper over the retry rig's cache and checker (whose
// adopter holds the adopt_unsigned knob), logging to the rig's logs.
func newRigReaper(r *retryRig) *Reaper {
	r.t.Helper()
	rp, err := NewReaper(ReaperConfig{Cache: r.cache, Checker: r.checker, IdleTTL: 30 * 24 * time.Hour,
		Logger: slog.New(slog.NewJSONHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	if err != nil {
		r.t.Fatal(err)
	}
	return rp
}

// clientFetchedRelease stands in for a client fetching the rig suite's
// Release from upstream: the url_path row the handler's miss stores.
func (r *retryRig) clientFetchedRelease(body []byte) {
	r.t.Helper()
	ctx := context.Background()
	h, err := r.adopter.writeBlobBytes(ctx, body)
	if err != nil {
		r.t.Fatal(err)
	}
	now := r.clock.now().Unix()
	if err := r.cache.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: rigHost,
		Path: rigSuite + "/Release", BlobHash: &h, UpstreamURL: rigReleaseURL, IsMetadata: true,
		RequestCount: 1, LastRequestedAt: &now, LastFetchedAt: &now}); err != nil {
		r.t.Fatal(err)
	}
}

// reapRig ages the rig suite past the TTL and runs the reaper once; the
// suite must be gone after. The rig's clock jumps the 31 days at once;
// for a daemon that lived through them the monotonic clock would have
// moved as far, so the forward-step guard (design review M-8) is told
// so: its last monotonic reading is cleared.
func (r *retryRig) reapRig(rp *Reaper) {
	r.t.Helper()
	r.clock.advance(reapAge)
	rp.lastMono = time.Time{}
	rp.runOnce(context.Background())
	// Only ErrNotFound proves the delete: any other error is a failed read
	// (the Task 15 review's minor 9).
	if _, err := r.cache.GetSuiteFreshness(context.Background(), "http", rigHost, rigSuite); !errors.Is(err, cache.ErrNotFound) {
		r.t.Fatalf("the idle rig suite was not reaped (%v)", err)
	}
}

// clientReturns stands in for a client's next request after a reap or a
// forget: the anchor miss re-creates the suite's row, and the anchor's
// url_path row — InRelease, with a stale blob, so the check sees a change
// — is the client-created anchor the first adoption needs.
func (r *retryRig) clientReturns() {
	r.t.Helper()
	res, err := r.cache.CreateSuite(context.Background(), cache.SuiteSeed{SuiteKey: cache.SuiteKey{CanonicalScheme: "http",
		CanonicalHost: rigHost, SuitePath: rigSuite}, RequestedAt: r.clock.now().Unix()}, cache.SuiteCaps{}, nil)
	if err != nil || res.Outcome != cache.SuiteCreated {
		r.t.Fatalf("re-creation = %+v, %v", res, err)
	}
	seedStaleAnchor(r.t, r.cache)
}

// retiredCount is the number of retired snapshots of the rig suite.
func (r *retryRig) retiredCount() int {
	r.t.Helper()
	return countRows(r.t, r.cache, `SELECT COUNT(*) FROM suite_snapshot WHERE suite_path = ? AND retired_at IS NOT NULL`,
		rigSuite)
}

// firstPeriodicTick runs g's periodic loop until its first tick has
// completed (LastRunSummary's phase is then "periodic"), then stops it
// and waits for Run to return. g's interval must be short: the first tick
// comes one interval after the loop starts. It waits at most 10 s.
func firstPeriodicTick(t *testing.T, g *gc.GC) {
	t.Helper()
	life, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { g.Run(life); close(done) }()
	defer func() { cancel(); <-done }()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		if s, ok := g.LastRunSummary(); ok && s.Phase == "periodic" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no periodic GC tick completed within 10 s")
		}
	}
}

// Spec §7 C9: a reaped suite whose client returns before any GC tick
// re-adopts its unchanged upstream content within one check — the retired
// snapshot with the same natural key refuses nothing — and the GC tick
// that then deletes the retired snapshot leaves the new one whole. The
// startup GC pass, which runs before the listeners serve, leaves the
// retired snapshot (ruling PF-15: no sub-job C there); the first periodic
// tick deletes it.
func TestDoD_ReturnBeforeGCReadoptsWithinOneCheck(t *testing.T) {
	ctx := context.Background()
	r := newRetryRig(t, time.Minute)
	rp := newRigReaper(r)
	r.adopt()
	old := r.currentSnapshot()
	r.reapRig(rp)
	r.clientReturns()
	r.check()
	snap := r.currentSnapshot()
	if snap == nil || snap.SnapshotID == old.SnapshotID || strOf(snap.SingleAnchorHash) != strOf(old.SingleAnchorHash) {
		t.Fatalf("after one check: snapshot %+v; want the unchanged anchor adopted as a new snapshot (old %d)", snap, old.SnapshotID)
	}
	g, err := gc.New(gc.Config{Cache: r.cache, Logger: discardLogger(), Enabled: true, Interval: time.Millisecond, BatchSize: 100,
		SnapshotBatchSize: 10, MaxTickDuration: 5 * time.Minute, BlobGrace: time.Nanosecond, KeepDisplaced: 3,
		PoolScanWorkers: 1, HeartbeatStaleGrace: 30 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.StartupPass(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.cache.GetSuiteSnapshot(ctx, old.SnapshotID); err != nil {
		t.Fatalf("the startup GC pass deleted the retired snapshot (%v): it runs no sub-job C", err)
	}
	firstPeriodicTick(t, g)
	if _, err := r.cache.GetSuiteSnapshot(ctx, old.SnapshotID); !errors.Is(err, cache.ErrNotFound) {
		t.Fatalf("the retired snapshot survived the first periodic GC tick (%v)", err)
	}
	m, err := r.cache.GetSnapshotMember(ctx, snap.SnapshotID, "main/binary-amd64/Packages")
	if err != nil {
		t.Fatalf("the new snapshot lost its Packages member: %v", err)
	}
	for _, h := range []string{m.BlobHash, strOf(snap.SingleAnchorHash)} {
		if ok, err := r.cache.BlobExists(h); err != nil || !ok {
			t.Fatalf("blob %s of the new snapshot: exists %v, %v", h, ok, err)
		}
	}
}

// Spec §7 C9 (D9): POST /forget then the client's next request re-adopts
// — after an upstream rollback that the adopted history refused, the
// older content; then, forgotten again, the unchanged content at once.
func TestDoD_ForgetThenRequestReadoptsAndRollbackAdopts(t *testing.T) {
	ctx := context.Background()
	r := newRetryRig(t, time.Minute)
	rp := newRigReaper(r)
	d1, d2 := r.clock.now().Add(-2*time.Hour).Unix(), r.clock.now().Add(-time.Hour).Unix()
	v1, v2 := r.release("v1", d1), r.release("v2", d2)
	r.up.set(v1)
	r.check()
	r.up.set(v2)
	r.clock.advance(2 * time.Minute)
	r.check()
	if strOf(r.currentSnapshot().SingleAnchorHash) != hashOf(v2) {
		t.Fatal("premise: v2 is not current")
	}
	r.up.set(v1) // the upstream rolls back
	r.clock.advance(2 * time.Minute)
	r.check()
	if strOf(r.currentSnapshot().SingleAnchorHash) != hashOf(v2) {
		t.Fatal("the rollback was adopted before /forget")
	}
	for i := range 2 {
		if res, err := rp.Forget(ctx, "http", rigHost, rigSuite); err != nil || !res.Deleted {
			t.Fatalf("forget %d = %+v, %v", i, res, err)
		}
		r.clientReturns()
		r.clock.advance(2 * time.Minute)
		r.check()
		if snap := r.currentSnapshot(); snap == nil || strOf(snap.SingleAnchorHash) != hashOf(v1) {
			t.Fatalf("after forget %d and one check: snapshot %+v; want v1 adopted", i, snap)
		}
	}
}

// Spec §7 C9: with gc.enabled = false nothing deletes retired snapshots,
// yet a suite reaped twice re-adopts its unchanged content each time: the
// retired snapshots that share its natural key refuse nothing.
func TestDoD_GCDisabledStillReadopts(t *testing.T) {
	r := newRetryRig(t, time.Minute)
	rp := newRigReaper(r)
	r.adopt()
	anchor := strOf(r.currentSnapshot().SingleAnchorHash)
	for i := range 2 {
		r.reapRig(rp)
		r.clientReturns()
		r.check()
		if snap := r.currentSnapshot(); snap == nil || strOf(snap.SingleAnchorHash) != anchor {
			t.Fatalf("return %d: snapshot %+v; want the unchanged anchor re-adopted", i, snap)
		}
	}
	if n := r.retiredCount(); n != 2 {
		t.Fatalf("%d retired snapshots, want the 2 reaped ones, kept (no GC)", n)
	}
}

// downgradeRefusedAfterReap leaves the rig suite as the overview's F line
// describes: adopted signed under the phase-A binary (no marker of its own
// commit; the startup pass marks it), reaped by the F binary, back with an
// unsigned upstream and refused as unsigned_downgrade_refused. It returns
// the reaper and the unsigned Release.
func downgradeRefusedAfterReap(t *testing.T) (*retryRig, *Reaper, []byte) {
	t.Helper()
	ctx := context.Background()
	r := newRetryRig(t, time.Minute)
	rp := newRigReaper(r)
	r.adopt()
	id := *r.row().CurrentSnapshotID
	if err := r.cache.ClearSignedSeenForTest(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.cache.EnqueueLegacyTrustAudit(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := RunLegacyTrustAudit(ctx, r.cache, discardLogger()); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := r.cache.SignedSeen(ctx, "http", rigHost, rigSuite); err != nil || !ok {
		t.Fatalf("premise: the startup pass wrote no marker (%v, %v)", ok, err)
	}
	r.reapRig(rp)
	if _, ok, err := r.cache.SignedSeen(ctx, "http", rigHost, rigSuite); err != nil || !ok {
		t.Fatalf("the reap took the marker (%v, %v): markers outlive reaping", ok, err)
	}
	r.up.mu.Lock()
	delete(r.up.bodies, rigAnchorURL) // the vendor stops signing: InRelease 404
	r.up.mu.Unlock()
	rel := r.unsignedRelease("u1", nil)
	r.adopter.adoptUnsigned = true
	r.reCreateForRelease(rel)
	r.check()
	if row := r.row(); row.CurrentSnapshotID != nil || strOf(row.AdoptionFailReason) != cache.ReasonUnsignedDowngradeRefused {
		t.Fatalf("back unsigned: adopted %v, record %q; want unsigned_downgrade_refused", row.CurrentSnapshotID != nil,
			strOf(row.AdoptionFailReason))
	}
	return r, rp, rel
}

// reCreateForRelease stands in for a client of an unsigned upstream after
// a reap or a forget: InRelease answers 404, so the Release miss re-creates
// the row (an adopter is wired) and is the client-created anchor.
func (r *retryRig) reCreateForRelease(rel []byte) {
	r.t.Helper()
	res, err := r.cache.CreateSuite(context.Background(), cache.SuiteSeed{SuiteKey: cache.SuiteKey{CanonicalScheme: "http",
		CanonicalHost: rigHost, SuitePath: rigSuite}, RequestedAt: r.clock.now().Unix()}, cache.SuiteCaps{}, nil)
	if err != nil || res.Outcome != cache.SuiteCreated {
		r.t.Fatalf("re-creation = %+v, %v", res, err)
	}
	r.clientFetchedRelease(rel)
}

// Overview F line, spec §7 C9: a signed snapshot adopted under the
// phase-A binary, reaped by the F binary, returns with an unsigned
// upstream: never-downgrade still refuses it — markers outlive reaping —
// at every later check.
func TestDoD_NeverDowngradeAfterReap(t *testing.T) {
	r, _, _ := downgradeRefusedAfterReap(t)
	for range 3 {
		r.clock.advance(time.Hour)
		r.check()
		if row := r.row(); row.CurrentSnapshotID != nil {
			t.Fatal("an unsigned snapshot was adopted for a suite with signed history")
		}
	}
}

// Phase E carry-forward (spec C5 threat notes): POST /forget is the
// escape hatch for unsigned_downgrade_refused. With no other suite row for
// the base, /forget deletes its markers and the suite adopts unsigned
// after the transition's two observations; with the /. twin keeping a
// row, the markers stay and the suite stays refused. Ruling PF-2: a signed
// InRelease still cached under the other scheme, with no suite row there,
// goes with the markers — left in place, the unsigned commit's re-mark
// (ruling R-E1) would read it and mark the base again, keeping the hatch
// shut with no row left to forget.
func TestDoD_ForgetIsTheEscapeHatchForADowngradeRefusal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(context.Context, *retryRig)
		twin  bool // the markers stay and the suite stays refused
	}{
		{name: "no other row"},
		{name: "the twin keeps a row", twin: true, setup: func(ctx context.Context, r *retryRig) {
			if err := r.cache.EnsureSuiteForTest(ctx, "http", rigHost, rigSuite+"/."); err != nil {
				r.t.Fatal(err)
			}
		}},
		{name: "the other scheme cached a signed anchor", setup: func(ctx context.Context, r *retryRig) {
			h, err := r.adopter.writeBlobBytes(ctx, simtest.ClearsignedFixture("Origin: signed\nSHA256:\n"))
			if err != nil {
				r.t.Fatal(err)
			}
			if err := r.cache.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "https", CanonicalHost: rigHost,
				Path: rigSuite + "/InRelease", BlobHash: &h, UpstreamURL: "https://" + rigHost + rigSuite + "/InRelease",
				IsMetadata: true, RequestCount: 1}); err != nil {
				r.t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r, rp, rel := downgradeRefusedAfterReap(t)
			if tc.setup != nil {
				tc.setup(ctx, r)
			}
			if res, err := rp.Forget(ctx, "http", rigHost, rigSuite); err != nil || !res.Deleted {
				t.Fatalf("forget = %+v, %v", res, err)
			}
			if _, marked, err := r.cache.SignedSeen(ctx, "http", rigHost, rigSuite); err != nil || marked != tc.twin {
				t.Fatalf("after /forget: marker %v (%v); want %v", marked, err, tc.twin)
			}
			r.reCreateForRelease(rel)
			r.clock.advance(time.Minute)
			r.check()
			r.clock.advance(16 * time.Minute)
			r.check()
			row := r.row()
			switch {
			case tc.twin && (row.CurrentSnapshotID != nil || strOf(row.AdoptionFailReason) != cache.ReasonUnsignedDowngradeRefused):
				t.Fatalf("twin kept: adopted %v, record %q; want still refused", row.CurrentSnapshotID != nil, strOf(row.AdoptionFailReason))
			case !tc.twin:
				if snap := r.currentSnapshot(); snap == nil || snap.Form != cache.FormUnsigned || strOf(snap.SingleAnchorHash) != hashOf(rel) {
					t.Fatalf("snapshot %+v, record %q; want the unsigned Release adopted", snap, strOf(row.AdoptionFailReason))
				}
			}
		})
	}
}
