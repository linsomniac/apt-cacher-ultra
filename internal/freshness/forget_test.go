package freshness

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
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
			ev[0]["anchor_rows"] != float64(1) || ev[0]["canonical_scheme"] != "http" || ev[0]["canonical_host"] != "h" ||
			ev[0]["suite_path"] != "/r/dists/x" {
			t.Fatalf("suite_forgotten lines %v, want one for http h /r/dists/x with metadata_rows 1, markers_deleted 2, "+
				"anchor_rows 1", ev)
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
// PF-7). Inside a synctest bubble the rig's pinned clock stands still, so
// the forget's marker stamp (PF-10: MAX(last_seen_at, the cache's now)) is
// exactly reapNow however long the machine takes to get there (the Task 9
// re-review: outside a bubble the pinned clock runs on in real time, and
// the assertion failed past one second).
func TestReaper_ForgetAnswersFalseForBusyAndUnknown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
	})
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

// failingTrigger makes every DELETE on table in r's cache fail, through a
// trigger created over a second connection to its database.
func (r *reaperRig) failingTrigger(table string) {
	r.t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(r.cache.Dir(), "cache.db"))
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TRIGGER fail_delete_` + table + ` BEFORE DELETE ON ` + table +
		` BEGIN SELECT RAISE(ABORT, 'injected fault'); END`); err != nil {
		r.t.Fatal(err)
	}
}

// The Task 10 review's minor 1: a database failure at any step after the
// first read is an error — the endpoint's 500 and its WARN — never "not
// deleted" (the 409 for busy or unknown): the re-read under the hold, the
// metadata batches, the final delete. The hold is released as the cache
// answers (design review M-2): a step that failed keeps the suite and its
// lock-map entry; a final delete that committed although its submit
// failed leaves the suite gone and its entry dropped, and is logged INFO
// suite_forgotten with settled=true and the metadata rows, without the
// marker and anchor counts it cannot know (final review spec m2, tests
// m-7: the reaper's suite_reaped settled=true). A step that failed logs
// no suite_forgotten.
func TestReaper_ForgetReportsEachStepsDatabaseError(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault func(r *reaperRig, cancel context.CancelFunc, resume <-chan struct{})
		gone  bool
	}{
		{"the re-read under the hold", func(r *reaperRig, _ context.CancelFunc, _ <-chan struct{}) {
			r.reaper.reapHook = func(context.Context, cache.SuiteKey) { _ = r.cache.Close() }
		}, false},
		{"the metadata batches", func(r *reaperRig, _ context.CancelFunc, _ <-chan struct{}) {
			r.failingTrigger("url_path")
		}, false},
		{"the final delete", func(r *reaperRig, _ context.CancelFunc, _ <-chan struct{}) {
			r.failingTrigger("suite_freshness")
		}, false},
		{"a final delete that committed, its submit failed", func(r *reaperRig, cancel context.CancelFunc, resume <-chan struct{}) {
			var once sync.Once
			r.cache.SetCommittedHookForTest(func() { once.Do(func() { cancel(); <-resume }) })
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReaperRig(t)
			r.seed("/r/dists/x", reapNow) // leaves a lock-map entry
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			resume := make(chan struct{})
			tc.fault(r, cancel, resume)
			res, err := r.reaper.Forget(ctx, "http", "h", "/r/dists/x")
			close(resume)
			if err == nil || res.Deleted {
				t.Fatalf("Forget = %+v, %v; want an error (a 500), not a 409's not-deleted", res, err)
			}
			forgotten := r.events("suite_forgotten")
			if tc.gone {
				if r.exists("/r/dists/x") || r.checker.LockCount() != 0 {
					t.Fatalf("suite exists %v, %d lock-map entries; want it gone and its entry dropped (release(true))",
						r.exists("/r/dists/x"), r.checker.LockCount())
				}
				if len(forgotten) != 1 {
					t.Fatalf("suite_forgotten lines %v; want one for the forget that committed", forgotten)
				}
				ev := forgotten[0]
				_, markers := ev["markers_deleted"]
				_, anchors := ev["anchor_rows"]
				if ev["level"] != "INFO" || ev["settled"] != true || ev["canonical_scheme"] != "http" || ev["canonical_host"] != "h" ||
					ev["suite_path"] != "/r/dists/x" || ev["metadata_rows"] != float64(1) || markers || anchors {
					t.Fatalf("suite_forgotten = %v; want INFO, settled, the suite and its 1 metadata row, no unknown counts", ev)
				}
				return
			}
			if len(forgotten) != 0 {
				t.Fatalf("suite_forgotten lines %v; want none for a forget that failed", forgotten)
			}
			if r.checker.LockCount() != 1 {
				t.Fatalf("%d lock-map entries; want the kept suite's entry kept (release(false))", r.checker.LockCount())
			}
		})
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

// The Task 6 re-review's out-of-scope 3, as the reaper's per-candidate
// rule (design note 23): a panic under Forget's hold releases the suite as
// the cache then answers — a fresh read of the row — not as Forget last
// saw it, and the panic goes on to net/http's recovery unchanged. It is
// logged first, with the suite: one ERROR freshness_check_panicked with
// where=forget and the suite's scheme, host and path (final review spec
// m1). A reapHook stands in for a panic after a commit: it deletes the row
// through the cache, then panics, and the deleted suite's lock-map entry
// and per-suite state go (release(true)); one that panics with the row
// present keeps them (release(false)).
func TestReaper_ForgetReleasesAsTheCacheAnswersAfterAPanic(t *testing.T) {
	for _, tc := range []struct {
		name   string
		delete bool // the hook deletes the row before it panics
	}{
		{"the row gone before the panic", true},
		{"the row present at the panic", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReaperRig(t)
			r.seed("/r/dists/x", reapNow) // leaves a lock-map entry
			key := suiteKey("http", "h", "/r/dists/x")
			r.checker.absentAnswered.Store(key, struct{}{})
			r.reaper.reapHook = func(_ context.Context, k cache.SuiteKey) {
				r.reaper.reapHook = nil
				if tc.delete {
					if res, err := r.cache.ForgetSuite(context.Background(), k, nil); err != nil || !res.Deleted {
						t.Errorf("ForgetSuite = %+v, %v; want the row deleted", res, err)
					}
				}
				panic("forget bug")
			}
			var got any
			func() {
				defer func() { got = recover() }()
				_, _ = r.reaper.Forget(context.Background(), "http", "h", "/r/dists/x")
			}()
			if got != "forget bug" {
				t.Fatalf("Forget's panic = %v, want \"forget bug\" passed on to the caller (net/http's recovery)", got)
			}
			if ev := r.events("freshness_check_panicked"); len(ev) != 1 || ev[0]["level"] != "ERROR" ||
				ev[0]["where"] != "forget" || ev[0]["canonical_scheme"] != "http" || ev[0]["canonical_host"] != "h" ||
				ev[0]["suite_path"] != "/r/dists/x" || ev[0]["panic"] != "forget bug" {
				t.Fatalf("panic lines = %v; want one ERROR with where=forget naming http://h/r/dists/x", ev)
			}
			if r.exists("/r/dists/x") == tc.delete {
				t.Fatalf("premise: row exists %v, want %v", r.exists("/r/dists/x"), !tc.delete)
			}
			want := 1 // present: release(false) keeps the entry
			if tc.delete {
				want = 0 // gone: release(true) drops it
			}
			if n := r.checker.LockCount(); n != want {
				t.Fatalf("%d lock-map entries after the panic; want %d (released as the cache answers)", n, want)
			}
			if _, ok := r.checker.absentAnswered.Load(key); ok == tc.delete {
				t.Fatalf("per-suite state kept %v after the panic; want %v", ok, !tc.delete)
			}
		})
	}
}
