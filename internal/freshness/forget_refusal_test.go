package freshness

import (
	"context"
	"testing"
)

// Deferred minor F-15 (design note 8): a forget that deletes nothing —
// answered 409 alike, with no existence oracle (spec C9) — says why at
// DEBUG (suite_not_forgotten): no_suite for a suite without a row, busy
// for one held, no_suite again for one gone under the hold.
func TestReaper_ForgetSaysWhyItForgotNothing(t *testing.T) {
	ctx := context.Background()
	r := newReaperRig(t)
	if res, err := r.reaper.Forget(ctx, "http", "h", "/r/dists/none"); res.Deleted || err != nil {
		t.Fatalf("unknown: %+v, %v", res, err)
	}
	r.seed("/r/dists/busy", reapNow)
	release, held := r.checker.HoldSuite(reapKey("/r/dists/busy"))
	if !held {
		t.Fatal("premise: the suite is held")
	}
	if res, err := r.reaper.Forget(ctx, "http", "h", "/r/dists/busy"); res.Deleted || err != nil {
		t.Fatalf("busy: %+v, %v", res, err)
	}
	release(false)
	r.seed("/r/dists/gone", reapNow)
	r.checker.lockLoadedHook = func(string) {
		r.checker.lockLoadedHook = nil
		if res, err := r.cache.ForgetSuite(ctx, reapKey("/r/dists/gone"), nil); err != nil || !res.Deleted {
			t.Errorf("ForgetSuite = %+v, %v", res, err)
		}
	}
	if res, err := r.reaper.Forget(ctx, "http", "h", "/r/dists/gone"); res.Deleted || err != nil {
		t.Fatalf("gone under the hold: %+v, %v", res, err)
	}
	// The literals, not the constants: the reasons are a log contract
	// (docs/log-fields.md), and the test pins what goes on the wire.
	want := map[string]string{"/r/dists/none": "no_suite", "/r/dists/busy": "busy", "/r/dists/gone": "no_suite"}
	lines := r.events("suite_not_forgotten")
	for _, m := range lines {
		path, _ := m["suite_path"].(string)
		if m["level"] != "DEBUG" || m["canonical_scheme"] != "http" || m["canonical_host"] != "h" ||
			m["reason"] != want[path] {
			t.Errorf("line %v, want DEBUG with the reason %q", m, want[path])
		}
	}
	if len(lines) != 3 {
		t.Fatalf("%d suite_not_forgotten lines, want 3", len(lines))
	}
	if _, err := r.reaper.Forget(ctx, "http", "h", "/r/dists/none"); err != nil || len(r.events("suite_forgotten")) != 0 {
		t.Fatalf("a forget that deleted nothing: err %v, or it logged suite_forgotten", err)
	}
}
