package freshness

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// Spec C9 (D9): Forget deletes a suite regardless of idleness — one a
// client asked for just now, with a current snapshot — through the
// reaper's steps (ruling R-F1): its metadata rows, its snapshots retired,
// its row, its lock-map, flip-gate and touch entries; and, with no other
// suite of the base left, the base's markers under both schemes and its
// cached anchors under the other scheme too (ruling PF-2). It logs
// suite_forgotten.
func TestReaper_ForgetDeletesASuiteRegardlessOfIdleness(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		r := newReaperRig(t)
		r.seed("/r/dists/x", reapNow)
		snap := r.adopt("/r/dists/x", "anchor x")
		if err := r.cache.MarkSignedSeenBatch(ctx, []cache.SignedSeenMark{
			{CanonicalScheme: "http", CanonicalHost: "h", Base: "/r/dists/x", SeenAt: reapNow},
			{CanonicalScheme: "https", CanonicalHost: "h", Base: "/r/dists/x", SeenAt: reapNow},
		}); err != nil {
			t.Fatal(err)
		}
		r.cacheOtherSchemeAnchor("/r/dists/x")
		time.Sleep(r.gate.Quiet()) // the seeded serve is a quiet period old
		res, err := r.reaper.Forget(ctx, "http", "h", "/r/dists/x")
		if err != nil || !res.Deleted || res.MarkersDeleted != 2 || res.AnchorRowsDeleted != 1 || len(res.KeptBy) != 0 {
			t.Fatalf("Forget = %+v, %v; want deleted, 2 markers and 1 anchor row, kept by nothing", res, err)
		}
		if r.exists("/r/dists/x") {
			t.Fatal("the suite row survived")
		}
		if got, err := r.cache.GetSuiteSnapshot(ctx, snap); err != nil || got.RetiredAt == nil {
			t.Fatalf("snapshot %+v, %v; want retired", got, err)
		}
		for _, scheme := range []string{"http", "https"} {
			if _, err := r.cache.LookupURL(ctx, scheme, "h", "/r/dists/x/InRelease"); !errors.Is(err, cache.ErrNotFound) {
				t.Fatalf("%s://h/r/dists/x/InRelease: %v; want the anchor row deleted", scheme, err)
			}
		}
		if r.markerAt("/r/dists/x") != -1 {
			t.Fatal("the base's marker survived")
		}
		if l, g, tm := r.checker.LockCount(), r.gate.Len(), r.cache.TouchedSuites(); l != 0 || g != 0 || tm != 0 {
			t.Fatalf("lock map %d, flip gate %d, touch map %d entries; want none", l, g, tm)
		}
		ev := r.events("suite_forgotten")
		if len(ev) != 1 || ev[0]["metadata_rows"] != float64(1) || ev[0]["markers_deleted"] != float64(2) ||
			ev[0]["anchor_rows"] != float64(1) {
			t.Fatalf("suite_forgotten lines %v, want one with metadata_rows 1, markers_deleted 2, anchor_rows 1", ev)
		}
	})
}

// cacheOtherSchemeAnchor caches suite's InRelease under https: an anchor
// no https suite row owns (ruling PF-2's case).
func (r *reaperRig) cacheOtherSchemeAnchor(suite string) {
	r.t.Helper()
	ctx := context.Background()
	h, err := r.adopter.writeBlobBytes(ctx, []byte("https anchor of "+suite))
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.cache.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "https", CanonicalHost: "h", Path: suite + "/InRelease",
		BlobHash: &h, UpstreamURL: "https://h" + suite + "/InRelease", IsMetadata: true, RequestCount: 1}); err != nil {
		r.t.Fatal(err)
	}
}

// Spec C9: busy and unknown are one answer — not deleted, no error — and
// an unknown suite gets no lock-map entry (I-4). A busy suite keeps its
// row. Markers another suite with the base keeps (here the other scheme's
// suite) survive the forget, and the result names that suite (ruling
// PF-7).
func TestReaper_ForgetAnswersFalseForBusyAndUnknown(t *testing.T) {
	ctx := context.Background()
	r := newReaperRig(t)
	stored := 0
	r.checker.lockLoadedHook = func(string) { stored++ }
	if res, err := r.reaper.Forget(ctx, "http", "h", "/r/dists/none"); res.Deleted || err != nil || stored != 0 {
		t.Fatalf("unknown: %+v, %v, %d lock-map loads; want not deleted, nil, none (I-4: no entry for a suite without a row)",
			res, err, stored)
	}
	r.checker.lockLoadedHook = nil
	r.seed("/r/dists/busy", reapNow)
	release, held := r.checker.HoldSuite(reapKey("/r/dists/busy"))
	if !held {
		t.Fatal("busy")
	}
	res, err := r.reaper.Forget(ctx, "http", "h", "/r/dists/busy")
	release(false)
	if res.Deleted || err != nil || !r.exists("/r/dists/busy") {
		t.Fatalf("busy: %+v, %v, row kept %v; want not deleted, nil, kept", res, err, r.exists("/r/dists/busy"))
	}
	r.seed("/r/dists/both", reapNow)
	if err := r.cache.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "https", CanonicalHost: "h",
		SuitePath: "/r/dists/both"}); err != nil {
		t.Fatal(err)
	}
	if err := r.cache.MarkSignedSeenBatch(ctx, []cache.SignedSeenMark{{CanonicalScheme: "http", CanonicalHost: "h",
		Base: "/r/dists/both", SeenAt: reapNow}}); err != nil {
		t.Fatal(err)
	}
	res, err = r.reaper.Forget(ctx, "http", "h", "/r/dists/both")
	want := cache.SuiteKey{CanonicalScheme: "https", CanonicalHost: "h", SuitePath: "/r/dists/both"}
	if !res.Deleted || err != nil || r.markerAt("/r/dists/both") != reapNow || len(res.KeptBy) != 1 || res.KeptBy[0] != want {
		t.Fatalf("other scheme's suite: %+v, %v, marker %d; want forgotten, the marker kept by %v", res, err,
			r.markerAt("/r/dists/both"), want)
	}
}

// The same as the reaper's design note 5, for POST /forget: the row read
// before the hold finds the suite, the re-read under it finds it gone; the
// entry the hold stored goes (release(true)).
func TestReaper_ForgetDropsTheLockEntryOfASuiteGoneUnderTheHold(t *testing.T) {
	ctx := context.Background()
	r := newReaperRig(t)
	r.seed("/r/dists/x", reapNow) // leaves a lock-map entry
	r.checker.lockLoadedHook = func(string) {
		r.checker.lockLoadedHook = nil
		if res, err := r.cache.ForgetSuite(ctx, reapKey("/r/dists/x"), nil); err != nil || !res.Deleted {
			t.Errorf("ForgetSuite = %+v, %v", res, err)
		}
	}
	if res, err := r.reaper.Forget(ctx, "http", "h", "/r/dists/x"); res.Deleted || err != nil {
		t.Fatalf("Forget = %+v, %v; want not deleted (gone), nil", res, err)
	}
	if n := r.checker.LockCount(); n != 0 {
		t.Fatalf("%d lock entries; want the vanished suite's entry dropped", n)
	}
}

// Design review M-2: Forget's work runs within suiteHoldTimeout (5 min) —
// the admin endpoint hands it a context a disconnect cannot cancel, so a
// wedged writer would otherwise hold the suite and the request for good.
func TestReaper_ForgetBoundsItsWorkAtFiveMinutes(t *testing.T) {
	r := newReaperRig(t)
	r.seed("/r/dists/x", reapNow)
	var deadline, at time.Time
	var ok bool
	r.reaper.reapHook = func(ctx context.Context, _ cache.SuiteKey) { deadline, ok = ctx.Deadline(); at = time.Now() }
	start := time.Now()
	if _, err := r.reaper.Forget(context.WithoutCancel(context.Background()), "http", "h", "/r/dists/x"); err != nil {
		t.Fatal(err)
	}
	if !ok || deadline.Before(start.Add(5*time.Minute)) || deadline.After(at.Add(5*time.Minute)) {
		t.Fatalf("Forget's deadline = %v (set %v), want 5 min after its start", deadline, ok)
	}
}

// A database failure is an error (the endpoint's 500), not a 409.
func TestReaper_ForgetReportsADatabaseError(t *testing.T) {
	r := newReaperRig(t)
	r.seed("/r/dists/x", reapNow)
	if err := r.cache.Close(); err != nil {
		t.Fatal(err)
	}
	if res, err := r.reaper.Forget(context.Background(), "http", "h", "/r/dists/x"); res.Deleted || err == nil {
		t.Fatalf("Forget on a closed cache = %+v, %v; want an error", res, err)
	}
}
