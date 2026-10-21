package freshness

import (
	"context"
	"database/sql"
	"log/slog"
	"maps"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// packageHashRows reads snapshot id's package_hash rows: path →
// flat_filename.
func packageHashRows(t *testing.T, c *cache.Cache, id int64) map[string]string {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(c.Dir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT path, flat_filename FROM package_hash WHERE snapshot_id = ?`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var p, f string
		if err := rows.Scan(&p, &f); err != nil {
			t.Fatal(err)
		}
		out[p] = f
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// adoptFlat adopts suite (given a row here) from a Release declaring
// members (path → body), each served at the suite's URL, through env's
// adopter; it returns the current snapshot.
func adoptFlat(t *testing.T, env *adoptionTestEnv, suite SuiteRef, members map[string][]byte) *cache.SuiteSnapshot {
	t.Helper()
	ctx := context.Background()
	trackSuite(t, env.cache, suite)
	base := suite.CanonicalScheme + "://" + suite.CanonicalHost + suite.SuitePath + "/"
	for p, body := range members {
		env.fetcher.put(base+p, body)
	}
	rel, _ := makeRelease(members)
	if err := env.adopter.Run(ctx, suite, rel, "", ""); err != nil {
		t.Fatalf("Run(%s): %v", suite.SuitePath, err)
	}
	snap, err := env.cache.GetSuiteSnapshot(ctx, currentID(t, env.cache, suite))
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// Spec C6 materialisation, every real Filename shape of spec §3's table: a
// flat suite's stanza with literal Filename F gives the row
// FlatBase(suite) + "/" + F with flat_filename DotClean(F), and coverage
// is complete. The `deb <uri> ./` twin materialises at the same base as
// its suite (FlatBase strips the "/."). A dists suite's rows keep "".
func TestAdopter_MaterialisesFlatPackageRows(t *testing.T) {
	for _, c := range []struct {
		name, host, suite, filename, path, flat string
	}{
		{"pkgs.k8s.io", "pkgs.k8s.io", "/core:/stable:/v1.34/deb", "amd64/kubectl_1.34.1-1.1_amd64.deb",
			"/core:/stable:/v1.34/deb/amd64/kubectl_1.34.1-1.1_amd64.deb", "amd64/kubectl_1.34.1-1.1_amd64.deb"},
		{"NVIDIA CUDA", "developer.download.nvidia.com", "/compute/cuda/repos/ubuntu2204/x86_64", "./cuda-toolkit_12.6.0-1_amd64.deb",
			"/compute/cuda/repos/ubuntu2204/x86_64/./cuda-toolkit_12.6.0-1_amd64.deb", "cuda-toolkit_12.6.0-1_amd64.deb"},
		{"NVIDIA CUDA through ./", "developer.download.nvidia.com", "/compute/cuda/repos/ubuntu2204/x86_64/.", "./cuda-toolkit_12.6.0-1_amd64.deb",
			"/compute/cuda/repos/ubuntu2204/x86_64/./cuda-toolkit_12.6.0-1_amd64.deb", "cuda-toolkit_12.6.0-1_amd64.deb"},
		{"libnvidia-container", "nvidia.github.io", "/libnvidia-container/stable/deb/amd64", "./libnvidia-container1_1.17.0-1_amd64.deb",
			"/libnvidia-container/stable/deb/amd64/./libnvidia-container1_1.17.0-1_amd64.deb", "libnvidia-container1_1.17.0-1_amd64.deb"},
		{"Jenkins binary/", "pkg.jenkins.io", "/debian-stable/binary", "binary/jenkins_2.479.3_all.deb",
			"/debian-stable/binary/binary/jenkins_2.479.3_all.deb", "binary/jenkins_2.479.3_all.deb"},
		{"Jenkins direct/", "pkg.jenkins.io", "/debian-stable/binary", "direct/jenkins_2.479.3_all.deb",
			"/debian-stable/binary/direct/jenkins_2.479.3_all.deb", "direct/jenkins_2.479.3_all.deb"},
		{"Sublime files/", "download.sublimetext.com", "/apt/stable", "files/sublime-text_build-4180_amd64.deb",
			"/apt/stable/files/sublime-text_build-4180_amd64.deb", "files/sublime-text_build-4180_amd64.deb"},
		{"an epoch, %-encoded by the publisher", "h", "/repo", "pool/p/pkg_1%3a2.0_amd64.deb",
			"/repo/pool/p/pkg_1%3a2.0_amd64.deb", "pool/p/pkg_1%3a2.0_amd64.deb"},
		{"a dists suite", "archive.ubuntu.com", "/ubuntu/dists/noble", "pool/main/f/foo_1_amd64.deb",
			"/ubuntu/pool/main/f/foo_1_amd64.deb", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := newAdoptionTestEnv(t)
			suite := SuiteRef{CanonicalScheme: "https", CanonicalHost: c.host, SuitePath: c.suite}
			index := "Packages"
			if c.flat == "" {
				index = "main/binary-amd64/Packages"
			}
			pkgs := fakePackagesStanzas(map[string]string{c.filename: testHex64})
			snap := adoptFlat(t, env, suite, map[string][]byte{index: pkgs, index + ".gz": gzipBytes(pkgs)})
			if got, want := packageHashRows(t, env.cache, snap.SnapshotID), map[string]string{c.path: c.flat}; !maps.Equal(got, want) {
				t.Fatalf("package_hash rows = %v, want %v", got, want)
			}
			if !snap.PackageCoverageComplete {
				t.Fatal("package_coverage_complete = false, want true")
			}
		})
	}
}

// Spec C6 for a flat suite's other members (plan design note 6): only its
// root Packages is its Packages directory — a nested Packages a flat
// Release declares neither gives rows nor counts for coverage — and its
// Sources and pdiff Index build no rows (Sources and pdiff hashing stay
// dists-only). A flat Release with no root Packages at all has incomplete
// coverage (no_packages_members), and a root Packages whose stanzas all
// lack a SHA256 (ruling R-G1) adopts with incomplete coverage (no_sha256).
func TestAdopter_FlatCoverageReadsTheRootPackagesOnly(t *testing.T) {
	suite := SuiteRef{CanonicalScheme: "https", CanonicalHost: "h", SuitePath: "/repo"}
	pkgs := fakePackagesStanzas(map[string]string{"./a_1_amd64.deb": testHex64})
	nested := fakePackagesStanzas(map[string]string{"./nested_1_amd64.deb": strings.Repeat("b", 64)})
	sources := []byte("Package: a\nBinary: a\nVersion: 1\nDirectory: .\nChecksums-Sha256:\n " + testHex64 + " 10 a_1.dsc\n")
	pdiff := []byte("SHA256-Current: " + testHex64 + " 10\nSHA256-Patches:\n " + testHex64 + " 10 2026-10-20-0000.00\n")
	t.Run("root Packages only", func(t *testing.T) {
		env := newAdoptionTestEnv(t)
		snap := adoptFlat(t, env, suite, map[string][]byte{"Packages": pkgs, "sub/Packages": nested, "Sources": sources,
			"Packages.diff/Index": pdiff})
		if got, want := packageHashRows(t, env.cache, snap.SnapshotID), map[string]string{"/repo/./a_1_amd64.deb": "a_1_amd64.deb"}; !maps.Equal(got, want) {
			t.Fatalf("package_hash rows = %v, want only the root Packages' %v", got, want)
		}
		if !snap.PackageCoverageComplete {
			t.Fatal("package_coverage_complete = false, want true (a nested Packages is not the flat suite's)")
		}
	})
	t.Run("no root Packages", func(t *testing.T) {
		env := newAdoptionTestEnv(t)
		var logs lockedBuffer
		env.adopter.logger = slog.New(slog.NewJSONHandler(&logs, nil))
		snap := adoptFlat(t, env, suite, map[string][]byte{"sub/Packages": nested, "Sources": sources})
		if rows := packageHashRows(t, env.cache, snap.SnapshotID); len(rows) != 0 || snap.PackageCoverageComplete {
			t.Fatalf("rows %v, coverage %v; want none and incomplete", rows, snap.PackageCoverageComplete)
		}
		if recs := logRecords(t, &logs, "package_coverage_incomplete"); len(recs) != 1 || recs[0]["reason"] != "no_packages_members" {
			t.Fatalf("package_coverage_incomplete = %v, want reason no_packages_members", recs)
		}
	})
	t.Run("no stanza has a SHA256", func(t *testing.T) {
		env := newAdoptionTestEnv(t)
		var logs lockedBuffer
		env.adopter.logger = slog.New(slog.NewJSONHandler(&logs, nil))
		gaps := []byte("Package: a\nArchitecture: amd64\nVersion: 1\nFilename: ./a_1_amd64.deb\nSHA512: " + strings.Repeat("a", 128) + "\n")
		snap := adoptFlat(t, env, suite, map[string][]byte{"Packages": gaps})
		if rows := packageHashRows(t, env.cache, snap.SnapshotID); len(rows) != 0 || snap.PackageCoverageComplete {
			t.Fatalf("rows %v, coverage %v; want none and incomplete", rows, snap.PackageCoverageComplete)
		}
		if recs := logRecords(t, &logs, "package_coverage_incomplete"); len(recs) != 1 || recs[0]["reason"] != "no_sha256" {
			t.Fatalf("package_coverage_incomplete = %v, want reason no_sha256", recs)
		}
	})
}
