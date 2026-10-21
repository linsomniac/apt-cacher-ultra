package freshness

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
)

// Fixture suites for the layout-aware IndexTarget predicates (spec C5):
// the pkgs.k8s.io flat shape and a dists suite.
const (
	testFlatSuite  = "/core:/stable:/v1.34/deb"
	testDistsSuite = "/ubuntu/dists/noble"
)

// layoutSuites are the suites each layout-aware table runs under: a flat
// suite, its `deb <uri> ./` twin, and a dists suite (plan overview, "PR #8
// parity" item 2: the same member paths under a dists suite are no
// IndexTargets).
var layoutSuites = []struct {
	suite string
	flat  bool
}{{testFlatSuite, true}, {testFlatSuite + "/.", true}, {testDistsSuite, false}}

// PR #8 parity item 2, isIndexTarget: a flat suite's root Packages* and
// Sources* (and their pdiff Index) are IndexTargets; under a dists suite
// none of them is. A flat suite's nested binary-<arch>/Packages is none
// either (plan design note 3: apt never requests it for a flat source).
func TestIsIndexTarget_LayoutAware(t *testing.T) {
	targets := []string{"Packages", "Packages.gz", "Packages.xz", "Packages.zst", "Packages.diff/Index", "Sources", "Sources.gz"}
	never := []string{"main/Packages.gz", "Packages_1.0_all.deb", "InRelease", "Release", "Contents-amd64.gz", "Translation-en"}
	for _, l := range layoutSuites {
		for _, p := range targets {
			if got := isIndexTarget(l.suite, p); got != l.flat {
				t.Errorf("isIndexTarget(%q, %q) = %v, want %v", l.suite, p, got, l.flat)
			}
		}
		for _, p := range never {
			if isIndexTarget(l.suite, p) {
				t.Errorf("isIndexTarget(%q, %q) = true, want false", l.suite, p)
			}
		}
		if got := isIndexTarget(l.suite, "main/binary-amd64/Packages"); got == l.flat {
			t.Errorf("isIndexTarget(%q, main/binary-amd64/Packages) = %v, want %v", l.suite, got, !l.flat)
		}
	}
}

// PR #8 parity item 2, indexTargetGroup: a flat suite's Packages variants
// group under "Packages" with the pseudo-arch "all", its Sources under
// "source"; pdiff Index and nested shapes have no group. Under a dists
// suite none of these paths groups.
func TestIndexTargetGroup_LayoutAware(t *testing.T) {
	cases := []struct {
		path, group, arch string
		ok                bool
	}{
		{"Packages", "Packages", "all", true},
		{"Packages.gz", "Packages", "all", true},
		{"Packages.xz", "Packages", "all", true},
		{"Sources.gz", "Sources", "source", true},
		{"Packages.diff/Index", "", "", false},
		{"main/Packages.gz", "", "", false},
	}
	for _, l := range layoutSuites {
		for _, c := range cases {
			want := c
			if !l.flat {
				want.group, want.arch, want.ok = "", "", false
			}
			group, arch, ok := indexTargetGroup(l.suite, c.path)
			if group != want.group || arch != want.arch || ok != want.ok {
				t.Errorf("indexTargetGroup(%q, %q) = (%q, %q, %v), want (%q, %q, %v)",
					l.suite, c.path, group, arch, ok, want.group, want.arch, want.ok)
			}
		}
	}
}

// PR #8 parity item 2, archFromFilteredPath (the flat pdiff arch labels,
// plan design note 4): a flat suite's root Sources, Sources.xz and
// Sources.diff/Index are "source"; its root Packages and Packages.gz (and
// Packages.diff/Index) carry every architecture and are never filtered.
// Under a dists suite none of these root paths is filtered. A root
// Contents-<arch> is filtered under either layout (the optional shapes).
func TestArchFromFilteredPath_LayoutAware(t *testing.T) {
	cases := []struct {
		path     string
		arch     string
		filtered bool
	}{
		{"Sources", "source", true},
		{"Sources.xz", "source", true},
		{"Sources.diff/Index", "source", true},
		{"Packages", "", false},
		{"Packages.gz", "", false},
		{"Packages.diff/Index", "", false},
	}
	for _, l := range layoutSuites {
		for _, c := range cases {
			want := c
			if !l.flat {
				want.arch, want.filtered = "", false
			}
			arch, filtered := archFromFilteredPath(l.suite, c.path)
			if arch != want.arch || filtered != want.filtered {
				t.Errorf("archFromFilteredPath(%q, %q) = (%q, %v), want (%q, %v)",
					l.suite, c.path, arch, filtered, want.arch, want.filtered)
			}
		}
		if arch, filtered := archFromFilteredPath(l.suite, "Contents-amd64.gz"); arch != "amd64" || !filtered {
			t.Errorf("archFromFilteredPath(%q, Contents-amd64.gz) = (%q, %v), want (amd64, true)", l.suite, arch, filtered)
		}
	}
}

// PR #8 parity item 2, missingRequestableIndexGroups: a flat suite's root
// Packages declared but not fetched is flagged even with no allowlist (it
// is the "all" group); served by a compressed variant only, it is not; a
// flat root Sources absent is not flagged unless "source" is allowlisted.
// Under a dists suite none of these root members is required.
func TestMissingRequestableIndexGroups_LayoutAware(t *testing.T) {
	rm := func(p string) ReleaseMember { return ReleaseMember{Path: p} }
	amd64 := map[string]struct{}{"amd64": {}}
	amd64src := map[string]struct{}{"amd64": {}, "source": {}}
	cases := []struct {
		name              string
		declared, fetched []ReleaseMember
		allowlist         map[string]struct{}
		flatWant          []string
	}{
		{"flat Packages declared but not fetched is flagged", []ReleaseMember{rm("Packages"), rm("Packages.gz")}, nil, nil, []string{"Packages"}},
		{"flat Packages served by a compressed variant is fine", []ReleaseMember{rm("Packages"), rm("Packages.gz")}, []ReleaseMember{rm("Packages.gz")}, amd64, nil},
		{"flat Sources absent is not flagged when source is not allowlisted", []ReleaseMember{rm("Packages.gz"), rm("Sources.gz")}, []ReleaseMember{rm("Packages.gz")}, amd64, nil},
		{"flat Sources absent is flagged when source is allowlisted", []ReleaseMember{rm("Packages.gz"), rm("Sources.gz")}, []ReleaseMember{rm("Packages.gz")}, amd64src, []string{"Sources"}},
	}
	for _, l := range layoutSuites {
		for _, c := range cases {
			want := c.flatWant
			if !l.flat {
				want = nil
			}
			if got := missingRequestableIndexGroups(l.suite, c.declared, c.fetched, c.allowlist); !reflect.DeepEqual(got, want) {
				t.Errorf("%s, suite %q: missingRequestableIndexGroups = %v, want %v", c.name, l.suite, got, want)
			}
		}
	}
}

// PR #8 parity item 3: a flat suite adopts, its root Packages and
// Packages.gz fetched relative to the suite path, and the snapshot holds
// InRelease, Packages and Packages.gz. The `deb <uri> ./` twin adopts the
// same way under its own path (ruling R-G4: two suites, two snapshots).
func TestAdopter_FlatSuiteAdopts(t *testing.T) {
	for _, suitePath := range []string{testFlatSuite, testFlatSuite + "/."} {
		t.Run(suitePath, func(t *testing.T) {
			ctx := context.Background()
			env := newAdoptionTestEnv(t)
			suite := SuiteRef{CanonicalScheme: "https", CanonicalHost: "pkgs.k8s.io", SuitePath: suitePath}
			trackSuite(t, env.cache, suite)
			pkgs := fakePackagesStanzas(map[string]string{"amd64/kubelet_1.deb": strings.Repeat("a", 64)})
			gz := gzipBytes(pkgs)
			releaseText, _ := makeRelease(map[string][]byte{"Packages": pkgs, "Packages.gz": gz})
			base := "https://pkgs.k8s.io" + suitePath + "/"
			env.fetcher.put(base+"Packages", pkgs)
			env.fetcher.put(base+"Packages.gz", gz)
			if err := env.adopter.Run(ctx, suite, releaseText, "", ""); err != nil {
				t.Fatalf("Run: %v", err)
			}
			paths := snapshotPaths(t, env.cache, currentID(t, env.cache, suite))
			for _, want := range []string{"InRelease", "Packages", "Packages.gz"} {
				if _, ok := paths[want]; !ok {
					t.Errorf("snapshot missing member %q (have %v)", want, paths)
				}
			}
			for _, u := range env.fetcher.requested() {
				if !strings.HasPrefix(u, base) {
					t.Errorf("fetched %s, want every member under %s", u, base)
				}
			}
		})
	}
}

// PR #8 parity item 3: a flat suite's root Packages is an IndexTarget, so
// its failure fails the run even when optional failures are tolerated,
// and no snapshot without it is published. Translation-en fetches, so the
// all-members-4xx guard cannot be what fails the run. PR #8's assertion
// (ErrAdoptionMemberFetchFailed) is kept, and the cause is pinned: a 404
// is skipped and then refused by the serve-contract guard (the flat "all"
// group; ErrAdoptionGuardFailed, which wraps member_fetch_failed); a 503
// is the fatal IndexTarget fetch failure itself, never tolerated. The
// 404's skip line says what was skipped (reason 4xx_index_target). Under a
// dists suite the same root Packages.gz is an optional member: its 503 is
// tolerated and the suite adopts.
func TestAdopter_FlatPackagesFailureFatal(t *testing.T) {
	for _, c := range []struct {
		name  string
		suite string
		fail  func(f *fakeFetcher, url string)
		fatal bool
		guard bool
	}{
		{"flat, 404", testFlatSuite, (*fakeFetcher).fail404, true, true},
		{"flat, 503", testFlatSuite, (*fakeFetcher).fail503, true, false},
		{"twin, 404", testFlatSuite + "/.", (*fakeFetcher).fail404, true, true},
		{"twin, 503", testFlatSuite + "/.", (*fakeFetcher).fail503, true, false},
		{"dists, 503", testDistsSuite, (*fakeFetcher).fail503, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			env := newAdoptionTestEnv(t)
			ad := newTolerantAdopter(t, env)
			var logs lockedBuffer
			ad.logger = slog.New(slog.NewJSONHandler(&logs, nil))
			suite := SuiteRef{CanonicalScheme: "https", CanonicalHost: "pkgs.k8s.io", SuitePath: c.suite}
			trackSuite(t, env.cache, suite)
			pkgs := fakePackagesStanzas(map[string]string{"amd64/kubelet_1.deb": strings.Repeat("a", 64)})
			tr := []byte("Package: kubelet\nDescription-en: kubelet\n")
			releaseText, _ := makeRelease(map[string][]byte{"Packages.gz": gzipBytes(pkgs), "Translation-en": tr})
			base := "https://pkgs.k8s.io" + c.suite + "/"
			env.fetcher.put(base+"Translation-en", tr)
			c.fail(env.fetcher, base+"Packages.gz")
			err := ad.Run(ctx, suite, releaseText, "", "")
			sf, gerr := env.cache.GetSuiteFreshness(ctx, suite.CanonicalScheme, suite.CanonicalHost, suite.SuitePath)
			if gerr != nil {
				t.Fatal(gerr)
			}
			if !c.fatal {
				if err != nil || sf.CurrentSnapshotID == nil {
					t.Fatalf("Run = %v, current %v; want a dists suite's optional root member tolerated and the suite adopted", err, sf.CurrentSnapshotID)
				}
				return
			}
			var me *AdoptionMemberError
			if !errors.Is(err, ErrAdoptionMemberFetchFailed) || errors.Is(err, ErrAdoptionGuardFailed) != c.guard ||
				!errors.As(err, &me) || !strings.HasPrefix(me.Path, "Packages") {
				t.Fatalf("Run = %v; want member_fetch_failed naming the flat Packages (from the serve-contract guard: %v)", err, c.guard)
			}
			if sf.CurrentSnapshotID != nil {
				t.Fatalf("a snapshot without Packages was published (%d)", *sf.CurrentSnapshotID)
			}
			if c.guard && skipReason(logs.String(), "Packages.gz") != "4xx_index_target" {
				t.Fatalf("Packages.gz skipped as %q, want 4xx_index_target", skipReason(logs.String(), "Packages.gz"))
			}
		})
	}
}

// skipReason returns the reason of the adoption_member_skipped line for
// path in the JSON log text, "" without one.
func skipReason(text, path string) string {
	for _, line := range strings.Split(text, "\n") {
		var m map[string]any
		if line != "" && json.Unmarshal([]byte(line), &m) == nil && m["msg"] == "adoption_member_skipped" && m["path"] == path {
			r, _ := m["reason"].(string)
			return r
		}
	}
	return ""
}

// Deferred minor E-9: adoption_member_skipped names a SHA512-only member's
// declared hash (declared_sha512) — its declared_sha256 is empty — at all
// three skip sites: a 404 (Contents-amd64), the architecture filter
// (Contents-arm64, amd64 allowlisted) and a tolerated optional member's
// integrity failure (Contents-all served with other bytes of its declared
// size, so its hash check refuses it). A flat repository's Release may
// declare only SHA512 (libnvidia-container).
func TestAdopter_SkippedSHA512OnlyMemberNamesItsHash(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	ad := newReconcileAdopter(t, env, []string{"amd64"}) // tolerant, amd64 allowlisted
	var logs lockedBuffer
	ad.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	suite := SuiteRef{CanonicalScheme: "https", CanonicalHost: "nvidia.github.io", SuitePath: "/libnvidia-container/stable/deb/amd64"}
	trackSuite(t, env.cache, suite)
	pkgs := fakePackagesStanzas(map[string]string{"libnvidia-container1_1.17.0-1_amd64.deb": testHex64})
	members := map[string][]byte{
		"Packages":       pkgs,
		"Contents-amd64": []byte("usr/bin/nvidia-container-cli utils/libnvidia-container-tools\n"),
		"Contents-arm64": []byte("usr/bin/nvidia-container-cli utils/libnvidia-container-tools-arm64\n"),
		"Contents-all":   []byte("usr/share/doc/libnvidia-container/copyright doc/libnvidia-container\n"),
	}
	base := "https://nvidia.github.io" + suite.SuitePath + "/"
	env.fetcher.put(base+"Packages", pkgs)
	env.fetcher.fail404(base + "Contents-amd64")
	env.fetcher.put(base+"Contents-all", []byte("other bytes of the same length as the declared content!!!!!!!!!!!!!\n"))
	rel := hashRelease("Origin: nvidia\n", false, true, members, nil)
	if err := ad.Run(ctx, suite, rel, "", ""); err != nil {
		t.Fatal(err)
	}
	reasons := map[string]string{"Contents-amd64": "4xx", "Contents-arm64": "arch_not_in_allowlist", "Contents-all": "optional_member_integrity"}
	seen := map[string]bool{}
	for _, line := range strings.Split(logs.String(), "\n") {
		var m map[string]any
		if line == "" || json.Unmarshal([]byte(line), &m) != nil || m["msg"] != "adoption_member_skipped" {
			continue
		}
		path, _ := m["path"].(string)
		want, ok := reasons[path]
		if !ok || m["reason"] != want || m["declared_sha256"] != "" || m["declared_sha512"] != sha512Of(members[path]) {
			t.Fatalf("adoption_member_skipped = %v, want %s skipped (%s) with its declared_sha512", m, path, want)
		}
		seen[path] = true
	}
	if len(seen) != len(reasons) {
		t.Fatalf("skip lines for %v, want one for each of %v", seen, reasons)
	}
}

// Ruling R-G1 end to end: a signed suite whose Packages declares only
// SHA512 in every stanza adopts — its snapshot's coverage incomplete
// (package_coverage_incomplete, reason no_sha256), no .deb vouched — where
// it failed adoption_parse_failed at every check before. An HTML page
// declared as the Packages member (a broken publisher) still fails the
// adoption, and nothing is adopted.
func TestAdopter_PackagesWithoutAnySHA256AdoptsIncomplete(t *testing.T) {
	ctx := context.Background()
	deb := "/ubuntu/pool/main/a/a_1_amd64.deb"
	t.Run("no stanza has a SHA256", func(t *testing.T) {
		env := newAdoptionTestEnv(t)
		var logs lockedBuffer
		env.adopter.logger = slog.New(slog.NewJSONHandler(&logs, nil))
		pkgs := []byte("Package: a\nArchitecture: amd64\nVersion: 1\nFilename: pool/main/a/a_1_amd64.deb\nSHA512: " +
			strings.Repeat("a", 128) + "\n")
		env.fetcher.put(noble+"main/binary-amd64/Packages", pkgs)
		rel := hashRelease("Origin: x\n", false, true, map[string][]byte{"main/binary-amd64/Packages": pkgs}, nil)
		if err := env.adopter.Run(ctx, env.suite, rel, "", ""); err != nil {
			t.Fatalf("Run = %v; want the SHA512-only index adopted (ruling R-G1)", err)
		}
		snap, err := env.cache.GetSuiteSnapshot(ctx, currentID(t, env.cache, env.suite))
		if err != nil || snap.PackageCoverageComplete {
			t.Fatalf("snapshot %+v, %v; want package_coverage_complete = false", snap, err)
		}
		if rows, err := env.cache.DeclaredHashesForPath(ctx, "http", "archive.ubuntu.com", deb); err != nil || len(rows) != 0 {
			t.Fatalf("%s vouched by %v (%v), want nothing", deb, rows, err)
		}
		recs := logRecords(t, &logs, "package_coverage_incomplete")
		if len(recs) != 1 || recs[0]["reason"] != "no_sha256" || recs[0]["no_sha256_stanzas"] != float64(1) {
			t.Fatalf("package_coverage_incomplete = %v, want one line, reason no_sha256, 1 stanza", recs)
		}
	})
	t.Run("an HTML page", func(t *testing.T) {
		env := newAdoptionTestEnv(t)
		html := []byte("<html><body>Not Found</body></html>\n")
		env.fetcher.put(noble+"main/binary-amd64/Packages", html)
		rel := hashRelease("Origin: x\n", true, false, map[string][]byte{"main/binary-amd64/Packages": html}, nil)
		if err := env.adopter.Run(ctx, env.suite, rel, "", ""); !errors.Is(err, ErrAdoptionParseFailed) {
			t.Fatalf("Run = %v, want ErrAdoptionParseFailed", err)
		}
		sf, err := env.cache.GetSuiteFreshness(ctx, env.suite.CanonicalScheme, env.suite.CanonicalHost, env.suite.SuitePath)
		if err != nil || sf.CurrentSnapshotID != nil {
			t.Fatalf("row %+v, %v; want nothing adopted", sf, err)
		}
	})
}
