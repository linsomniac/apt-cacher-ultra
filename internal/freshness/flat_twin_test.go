package freshness

import (
	"context"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// Deferred minor E-19 (spec C3, ruling PE-5): a probe that finds a
// signature marks the suite's base (aptpath.MarkerBase), which a /. twin
// shares — the probe of /repo/. marks /repo, never /repo/. itself. The
// adoption that follows fails (no Packages upstream), so the probe's
// marker is the only one written.
func TestCheck_ATwinsProbeMarksItsBase(t *testing.T) {
	r := newFlatRig(t, "/repo/.")
	in := simtest.ClearsignedFixture("Origin: repo\n")
	r.cacheRow("InRelease", in, 1, r.clock.now().Unix(), "")
	r.up.setAt(r.url("InRelease"), in)
	r.check()
	ctx := context.Background()
	if _, ok, err := r.cache.SignedSeen(ctx, "http", flatRigHost, "/repo"); err != nil || !ok {
		t.Fatalf("the base /repo has no marker (%v, %v)", ok, err)
	}
	if _, ok, err := r.cache.SignedSeen(ctx, "http", flatRigHost, "/repo/."); err != nil || ok {
		t.Fatalf("a marker for /repo/. itself (%v, %v); want the base's only", ok, err)
	}
	if row := r.row(); row.CurrentSnapshotID != nil {
		t.Fatal("premise: the adoption committed (and could have written a marker of its own)")
	}
}
