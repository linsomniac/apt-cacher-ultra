package freshness

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// Ruling R-G2 (the F carry-forward; Review Focus 2), as amended by ruling
// PG-7: an unadopted flat suite whose base has no signed evidence is
// judged junk on its cached anchor rows and its cached root
// Packages*/Sources* rows together, by the anchors' rules — every one
// fails its sniff (the flat index sniff for an index's name) and was
// fetched at least 24 h ago, whatever the suite's record says; a flat
// suite with signed evidence, and a dists/ suite, by its anchors alone.
// Reaped: HTML indexes (with or without an anchor_absent record); a junk
// InRelease beside an HTML Packages.gz (everything junk); a signed suite
// (a marker for its base) whose junk InRelease decides alone though its
// Packages.gz is good; a Packages.gz whose bytes are plain text (no gzip
// magic); junk InRelease and Release rows with no index row at all (a
// removed Release-less repository on a soft-404 host: the refresh deleted
// its indexes on their 404s and keeps the HTML anchors upstream still
// answers 200, so only this reap removes it — the final review's tests
// lens I-1). Kept: the same junk InRelease and good Packages.gz without the
// marker — a healthy Release-less repository on a soft-404 host (ruling
// PG-7: anchors-alone would reap it daily); an HTML index younger than the
// grace; a good index; one good index beside a junk one; a suite whose
// sniff-passing InRelease decides alone though its Packages.gz is HTML; a
// missing pool file (no evidence); a dists/ suite's root Packages (no flat
// index); the same plain-text bytes cached as Packages, where they pass (a
// verdict is remembered per blob and name); and /junk's ./ twin, judged by
// its own good index (ruling R-G4); and /empty, with no cached anchor or
// index row at all (R-G2: junk only with at least one row). The
// signed-evidence read is lazy: only the suites the two rules judge
// differently read it. A reaped suite's cached indexes go with it.
func TestReaper_ReapsAJunkAnchorlessFlatSuite(t *testing.T) {
	r := newReaperRig(t)
	old, young := reapNow-2*reapDay, reapNow-3600
	gz := "\x1f\x8b\x08\x00 an index"
	signed := string(simtest.ClearsignedFixture("Origin: x\nSHA256:\n"))
	reaped := []string{"/junk", "/absent", "/junk-anchors-signed", "/junk-everything", "/gz-name", "/junk-anchors-only"}
	kept := []string{"/junk/.", "/junk-anchors", "/young", "/good", "/mixed", "/signed", "/missing", "/r/dists/x", "/plain-name",
		"/empty"}
	for _, s := range append(append([]string{}, reaped...), kept...) {
		r.junkSuite(s)
	}
	r.cacheAnchor("/junk", "Packages.gz", softPage, old)
	r.cacheAnchor("/junk", "Sources", softPage, old)
	r.cacheAnchor("/junk/.", "Packages.gz", gz, old)
	r.cacheAnchor("/absent", "Packages.xz", softPage, old)
	r.record("/absent", cache.ReasonAnchorAbsent)
	for _, s := range []string{"/junk-anchors", "/junk-anchors-signed"} {
		r.cacheAnchor(s, "InRelease", softPage, old)
		r.cacheAnchor(s, "Packages.gz", gz, old)
	}
	if err := r.cache.MarkSignedSeenBatch(context.Background(), []cache.SignedSeenMark{{CanonicalScheme: "http",
		CanonicalHost: "h", Base: "/junk-anchors-signed", SeenAt: reapNow - 3*reapDay}}); err != nil {
		t.Fatal(err)
	}
	r.cacheAnchor("/junk-everything", "InRelease", softPage, old)
	r.cacheAnchor("/junk-everything", "Packages.gz", softPage, old)
	r.cacheAnchor("/gz-name", "Packages.gz", "Package: a\n", old)
	r.cacheAnchor("/junk-anchors-only", "InRelease", softPage, old)
	r.cacheAnchor("/junk-anchors-only", "Release", softPage+"release", old)
	r.cacheAnchor("/plain-name", "Packages", "Package: a\n", old)
	r.cacheAnchor("/young", "Packages.gz", softPage, young)
	r.cacheAnchor("/good", "Packages.gz", gz, old)
	r.cacheAnchor("/mixed", "Packages.gz", softPage, old)
	r.cacheAnchor("/mixed", "Sources.gz", gz, old)
	r.cacheAnchor("/signed", "InRelease", signed, old)
	r.cacheAnchor("/signed", "Packages.gz", softPage, old)
	r.cacheAnchorBlob("/missing", "Packages.gz", testHex64, old)
	r.cacheAnchor("/r/dists/x", "Packages", softPage, old)
	var read []string
	r.reaper.signedEvidence = func(ctx context.Context, host, suitePath string) error {
		read = append(read, suitePath)
		return r.cache.CheckUnsignedDowngrade(ctx, host, suitePath)
	}
	junk0 := reapedTotal(t, "junk")
	r.reaper.runOnce(context.Background())
	slices.Sort(read)
	if want := []string{"/absent", "/gz-name", "/junk", "/junk-anchors", "/junk-anchors-signed"}; !slices.Equal(read, want) {
		t.Errorf("signed evidence read for %v, want only %v: the suites the two rules judge differently", read, want)
	}
	for _, s := range reaped {
		if r.exists(s) {
			t.Errorf("%s was not reaped as junk", s)
		}
	}
	for _, s := range kept {
		if !r.exists(s) {
			t.Errorf("%s was reaped", s)
		}
	}
	if d := reapedTotal(t, "junk") - junk0; d != float64(len(reaped)) {
		t.Fatalf(`acu_suites_reaped_total{reason="junk"} rose by %v, want %d`, d, len(reaped))
	}
	if _, err := r.cache.LookupURL(context.Background(), "http", "h", "/junk/./Packages.gz"); err != nil {
		t.Fatalf("/junk/./Packages.gz: %v; want the twin's index kept", err)
	}
	for _, p := range []string{"/junk/Packages.gz", "/junk/Sources", "/junk-everything/Packages.gz", "/junk-everything/InRelease"} {
		if _, err := r.cache.LookupURL(context.Background(), "http", "h", p); !errors.Is(err, cache.ErrNotFound) {
			t.Errorf("%s: %v; want the reaped suite's row deleted, not served as a hit", p, err)
		}
	}
	if ev := r.events("suite reaper: junk anchor reads failed"); len(ev) != 0 {
		t.Fatalf("read failure lines = %v, want none", ev)
	}
}

// The F carry-forward (TestDoD_ForgetIsTheEscapeHatchForADowngradeRefusal's
// twin subtest, on a real flat suite): /repo, adopted signed (its probe
// marked the base), is reaped, comes back unsigned and is refused as
// unsigned_downgrade_refused. POST /forget is the escape hatch: with no
// other row for the base the markers go and the suite adopts unsigned
// after the transition's two observations; with its ./ twin keeping a row
// the markers stay and the suite stays refused.
func TestDoD_ForgetIsTheEscapeHatchForAFlatSuitesDowngradeRefusal(t *testing.T) {
	for _, twin := range []bool{false, true} {
		t.Run(map[bool]string{false: "no other row", true: "the ./ twin keeps a row"}[twin], func(t *testing.T) {
			ctx := context.Background()
			r := newFlatRig(t, "/repo")
			pkgs := fakePackagesStanzas(map[string]string{"x_1_amd64.deb": testHex64})
			signedRel, _ := makeRelease(map[string][]byte{"Packages": pkgs})
			in := simtest.ClearsignedFixture(string(signedRel))
			r.cacheRow("InRelease", in, 1, r.clock.now().Unix(), "")
			r.up.setAt(r.url("InRelease"), in)
			r.members.put(r.url("Packages"), pkgs)
			r.check()
			if r.row().CurrentSnapshotID == nil {
				t.Fatalf("premise: the signed first adoption failed (%q)", r.lastResult())
			}
			rp, err := NewReaper(ReaperConfig{Cache: r.cache, Checker: r.checker, IdleTTL: 30 * 24 * time.Hour,
				Logger: slog.New(slog.NewJSONHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
			if err != nil {
				t.Fatal(err)
			}
			r.clock.advance(reapAge)
			rp.lastMono = time.Time{}
			rp.runOnce(ctx)
			if _, err := r.cache.GetSuiteFreshness(ctx, "http", flatRigHost, "/repo"); !errors.Is(err, cache.ErrNotFound) {
				t.Fatalf("premise: /repo was not reaped (%v)", err)
			}
			if _, ok, err := r.cache.SignedSeen(ctx, "http", flatRigHost, "/repo"); err != nil || !ok {
				t.Fatalf("premise: the reap took the marker (%v, %v)", ok, err)
			}
			r.up.mu.Lock()
			delete(r.up.bodies, r.url("InRelease")) // the vendor stops signing
			r.up.mu.Unlock()
			rel, _ := makeRelease(map[string][]byte{"Packages": pkgs})
			r.up.setAt(r.url("Release"), rel)
			r.adopter.adoptUnsigned = true // the flat rig's default (rulings PG-9, PG-17), which the unsigned adoption below needs
			comeBack := func() {
				t.Helper()
				if res, err := r.cache.CreateSuite(ctx, cache.SuiteSeed{SuiteKey: suiteKeyOf(r.suite),
					RequestedAt: r.clock.now().Unix()}, cache.SuiteCaps{}, nil); err != nil || res.Outcome != cache.SuiteCreated {
					t.Fatalf("re-creation = %+v, %v", res, err)
				}
				r.cacheRow("Release", rel, 1, r.clock.now().Unix(), "")
			}
			comeBack()
			r.check()
			if row := r.row(); row.CurrentSnapshotID != nil || strOf(row.AdoptionFailReason) != cache.ReasonUnsignedDowngradeRefused {
				t.Fatalf("back unsigned: adopted %v, record %q; want unsigned_downgrade_refused", row.CurrentSnapshotID != nil,
					strOf(row.AdoptionFailReason))
			}
			if twin {
				if err := r.cache.EnsureSuiteForTest(ctx, "http", flatRigHost, "/repo/."); err != nil {
					t.Fatal(err)
				}
			}
			if res, err := rp.Forget(ctx, "http", flatRigHost, "/repo"); err != nil || !res.Deleted {
				t.Fatalf("forget = %+v, %v", res, err)
			}
			if _, marked, err := r.cache.SignedSeen(ctx, "http", flatRigHost, "/repo"); err != nil || marked != twin {
				t.Fatalf("after /forget: marker %v (%v); want %v", marked, err, twin)
			}
			comeBack()
			r.clock.advance(time.Minute)
			r.check()
			r.clock.advance(16 * time.Minute)
			r.check()
			row := r.row()
			switch {
			case twin && (row.CurrentSnapshotID != nil || strOf(row.AdoptionFailReason) != cache.ReasonUnsignedDowngradeRefused):
				t.Fatalf("twin kept: adopted %v, record %q; want still refused", row.CurrentSnapshotID != nil, strOf(row.AdoptionFailReason))
			case !twin && row.CurrentSnapshotID == nil:
				t.Fatalf("no twin: record %q (%q); want the unsigned Release adopted", strOf(row.AdoptionFailReason), r.lastResult())
			case !twin:
				if snap, err := r.cache.GetSuiteSnapshot(ctx, *row.CurrentSnapshotID); err != nil || snap.Form != cache.FormUnsigned {
					t.Fatalf("no twin: snapshot %+v, %v; want the unsigned form", snap, err)
				}
			}
		})
	}
}

// Ruling R-G5 (deferred minor F-12) with ruling PG-7's gate: the reaper
// reads flat index rows only when a run holds an unadopted flat suite —
// with or without anchor rows, the suites ruling PG-7 may judge by their
// indexes.
func TestHasUnadoptedFlatSuite(t *testing.T) {
	id := int64(7)
	row := func(suite string, current *int64) cache.SuiteWithAdoption {
		return cache.SuiteWithAdoption{SuiteFreshness: cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h",
			SuitePath: suite, CurrentSnapshotID: current}}
	}
	for _, tc := range []struct {
		name string
		rows []cache.SuiteWithAdoption
		want bool
	}{
		{"an unadopted flat suite", []cache.SuiteWithAdoption{row("/r/dists/x", nil), row("/flat", nil)}, true},
		{"an adopted flat suite", []cache.SuiteWithAdoption{row("/flat", &id)}, false},
		{"an unadopted ./ twin", []cache.SuiteWithAdoption{row("/flat/.", nil)}, true},
		{"dists suites only", []cache.SuiteWithAdoption{row("/r/dists/x", nil), row("/r/dists/y", nil)}, false},
		{"none", nil, false},
	} {
		if got := hasUnadoptedFlatSuite(tc.rows); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Ruling PG-6's other branch (rulings R-G5, PG-7, PG-9): a run that holds
// no unadopted flat suite never reads the flat index rows — a cache of
// dists/ suites pays nothing for R-G2 — and one that holds one reads them
// once.
func TestReaper_ReadsFlatIndexesOnlyWithAnUnadoptedFlatSuite(t *testing.T) {
	r := newReaperRig(t)
	reads := 0
	r.reaper.flatIndexRows = func(ctx context.Context) ([]cache.SuiteAnchorRow, error) {
		reads++
		return r.cache.ListUnadoptedFlatIndexRows(ctx)
	}
	r.junkSuite("/r/dists/x")
	r.reaper.runOnce(context.Background())
	if reads != 0 {
		t.Fatalf("a run of dists/ suites read the flat index rows %d times, want 0", reads)
	}
	r.junkSuite("/flat")
	r.reaper.runOnce(context.Background())
	if reads != 1 {
		t.Fatalf("a run with an unadopted flat suite read the flat index rows %d times, want 1", reads)
	}
}

// Design review M-13: a failed read of the flat index rows is logged once
// and leaves every unadopted flat suite unjudged for the run — an I/O
// fault is never evidence, so neither an anchorless junk suite nor one
// whose junk anchors would decide alone is reaped — while the run goes
// on: a junk dists/ suite is still reaped.
func TestReaper_AFailedFlatIndexReadJudgesNoFlatSuite(t *testing.T) {
	r := newReaperRig(t)
	old := reapNow - 2*reapDay
	for _, s := range []string{"/junk", "/junk-everything", "/r/dists/junk"} {
		r.junkSuite(s)
	}
	r.cacheAnchor("/junk", "Packages.gz", softPage, old)
	r.cacheAnchor("/junk-everything", "InRelease", softPage, old)
	r.cacheAnchor("/junk-everything", "Packages.gz", softPage, old)
	r.cacheAnchor("/r/dists/junk", "InRelease", softPage, old)
	r.reaper.flatIndexRows = func(context.Context) ([]cache.SuiteAnchorRow, error) {
		return nil, errors.New("disk I/O error")
	}
	r.reaper.runOnce(context.Background())
	if ev := r.events("suite reaper: list unadopted flat indexes failed"); len(ev) != 1 {
		t.Fatalf("%d flat index read failure lines, want 1", len(ev))
	}
	for _, s := range []string{"/junk", "/junk-everything"} {
		if !r.exists(s) {
			t.Errorf("%s was reaped although its flat index rows could not be read", s)
		}
	}
	if r.exists("/r/dists/junk") {
		t.Error("/r/dists/junk was not reaped: the run stopped at the failed read")
	}
}

// Ruling PG-7: a failed read of a flat suite's signed evidence gives that
// suite no verdict for the run (an I/O fault is never evidence), logged
// once with the count, and the run goes on.
func TestReaper_AFailedSignedEvidenceReadJudgesNothing(t *testing.T) {
	r := newReaperRig(t)
	old := reapNow - 2*reapDay
	for _, s := range []string{"/junk", "/junk-anchors", "/r/dists/junk"} {
		r.junkSuite(s)
	}
	r.cacheAnchor("/junk", "Packages.gz", softPage, old)
	r.cacheAnchor("/junk-anchors", "InRelease", softPage, old)
	r.cacheAnchor("/junk-anchors", "Packages.gz", "\x1f\x8b\x08\x00 an index", old)
	r.cacheAnchor("/r/dists/junk", "InRelease", softPage, old)
	r.reaper.signedEvidence = func(context.Context, string, string) error { return errors.New("disk I/O error") }
	r.reaper.runOnce(context.Background())
	ev := r.events("suite reaper: read signed evidence failed")
	if len(ev) != 1 || ev[0]["count"] != float64(2) {
		t.Fatalf("signed evidence failure lines = %v, want one counting 2", ev)
	}
	for _, s := range []string{"/junk", "/junk-anchors"} {
		if !r.exists(s) {
			t.Errorf("%s was reaped although its signed evidence could not be read", s)
		}
	}
	if r.exists("/r/dists/junk") {
		t.Error("/r/dists/junk was not reaped: the run stopped at the failed read")
	}
}
