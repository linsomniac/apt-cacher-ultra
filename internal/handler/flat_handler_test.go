package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
)

// Flat index bodies that pass, and one that fails, the flat index sniff
// (spec C3: the compression magic for the extension, or "Package:").
var (
	flatPackagesGz = []byte{0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03}
	flatPackages   = []byte("Package: hello\nVersion: 1\nArchitecture: amd64\nFilename: ./hello_1_amd64.deb\n")
)

// Spec C3 (the D carry-forward: the handler already sniffs, creates rows
// for and touches a flat index; phase G's grammar makes the paths
// reachable): with an adopter wired, a sniff-passing root Packages* or
// Sources* miss of a flat directory creates its suite's row — the suite of
// a Release-less repository — and a `./` twin's its own; so does any other
// directory's (the grammar's suite is the literal directory, so a
// Packages under /nested/sub/main is that directory's root index). An HTML
// page, a dists suite's root Packages.gz, a flat Contents-* and a flat
// Release.gpg create none. Without an adopter a flat index creates none
// (spec C3: InRelease only).
func TestServeHTTP_FlatIndexMissCreatesItsSuiteRow(t *testing.T) {
	for _, adoption := range []bool{true, false} {
		r := newSuiteRig(t, adoption, 0)
		files := map[string][]byte{
			"/fury/Packages.gz":          flatPackagesGz,
			"/fury/./Packages":           flatPackages,
			"/goreleaser/Sources.xz":     {0xfd, '7', 'z', 'X', 'Z', 0x00, 0x00},
			"/soft404/Packages.gz":       rowHTML,
			"/u/dists/s/Packages.gz":     flatPackagesGz,
			"/contents/Contents-amd64":   []byte("usr/bin/hello utils/hello\n"),
			"/gpg/Release.gpg":           rowGPG,
			"/nested/sub/main/Packages":  flatPackages,
			"/core:/stable:/v1.34/deb/x": []byte("not a suite file"),
		}
		for p, b := range files {
			r.serve(p, upFile{body: b})
			if rec := r.get(p); rec.Code != http.StatusOK {
				t.Fatalf("adoption=%v: %s = %d", adoption, p, rec.Code)
			}
		}
		for suite, want := range map[string]bool{
			"/fury": adoption, "/fury/.": adoption, "/goreleaser": adoption,
			"/nested/sub/main": adoption,
			"/soft404":         false, "/u/dists/s": false, "/contents": false, "/gpg": false,
		} {
			if got := r.suite(suite) != nil; got != want {
				t.Errorf("adoption=%v: %s has a row = %v, want %v", adoption, suite, got, want)
			}
		}
		if adoption && r.marked("/fury") {
			t.Error("a flat index earned a signed-seen marker")
		}
	}
}

// Spec C3 legacy onboarding for flat suites (Review Focus 1): a url_path
// hit on a cached sniff-passing flat anchor or flat index of a suite
// without a row creates the row before the body — a flat InRelease with
// its base's marker in the creating transaction, its `./` twin's InRelease
// its own row under the same base, a detached flat suite's Release
// marking its cached Release.gpg, and (adopter wired) a Release-less
// suite's cached Packages.gz with no marker — and the hit's T1 checks it.
// A cached flat Contents-* onboards nothing.
func TestServeHTTP_HitOnboardsAFlatSuite(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	const k8s = "/core:/stable:/v1.34/deb"
	r.cacheAnchor(k8s+"/InRelease", rowInRelease, 1_700_000_000)
	r.cacheAnchor(k8s+"/./InRelease", rowInRelease, 1_700_000_000)
	r.cacheAnchor("/jenkins/binary/Release", rowRelease, 1_700_000_000)
	r.cacheAnchor("/jenkins/binary/Release.gpg", rowGPG, 1_700_000_100)
	r.cacheAnchor("/fury/Packages.gz", flatPackagesGz, 1_700_000_000)
	r.cacheAnchor("/contents/Contents-amd64.gz", flatPackagesGz, 1_700_000_000)
	for _, p := range []string{k8s + "/InRelease", k8s + "/./InRelease", "/jenkins/binary/Release", "/fury/Packages.gz",
		"/contents/Contents-amd64.gz"} {
		if rec := r.get(p); rec.Code != http.StatusOK || rec.Header().Get(hdrXCache) != cacheHit {
			t.Fatalf("%s = %d %q, want a hit", p, rec.Code, rec.Header().Get(hdrXCache))
		}
	}
	for _, suite := range []string{k8s, k8s + "/.", "/jenkins/binary", "/fury"} {
		if s := r.suite(suite); s == nil || s.LastCheckAt != nil {
			t.Fatalf("%s: row %+v, want onboarded with NULL check stamps", suite, s)
		}
	}
	if r.suite("/contents") != nil {
		t.Fatal("a cached flat Contents-* onboarded its suite")
	}
	if !r.marked(k8s) {
		t.Fatal("the onboarded flat InRelease earned its base no marker")
	}
	if at, ok, err := r.c.SignedSeen(context.Background(), "http", "127.0.0.1", "/jenkins/binary"); err != nil || !ok || at != 1_700_000_100 {
		t.Fatalf("detached flat suite's marker = %d %v %v, want its cached Release.gpg's 1700000100", at, ok, err)
	}
	if r.marked("/fury") {
		t.Fatal("a Release-less flat suite's onboarding wrote a marker")
	}
	r.h.Close() // drains the T1 goroutines
	checked := map[string]bool{}
	for _, c := range r.fresh.snapshot() {
		checked[c.suitePath] = true
	}
	for _, suite := range []string{k8s, k8s + "/.", "/jenkins/binary", "/fury"} {
		if !checked[suite] {
			t.Errorf("no T1 for the onboarded %s (T1 for %v)", suite, checked)
		}
	}
	if checked["/contents"] {
		t.Error("T1 fired for the untracked /contents")
	}
}

// Spec C3 client interest, the flat root-index touch: a tracked flat
// suite's root Packages* and Sources* serves — hit or miss, its `./`
// twin's as well — record interest (TouchSuite); a flat Contents-* is no
// suite file, so its serve records none.
func TestServeHTTP_FlatRootIndexServesTouchTheirSuite(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	r.trackOld("/hit")
	r.cacheAnchor("/hit/Packages.gz", flatPackagesGz, 1_700_000_000)
	r.get("/hit/Packages.gz")
	waitFor(t, "the root Packages.gz hit's touch", func() bool { return r.requested("/hit") > 1000 })

	r.trackOld("/miss")
	r.serve("/miss/Sources.xz", upFile{body: []byte{0xfd, '7', 'z', 'X', 'Z', 0x00, 0x00}})
	r.get("/miss/Sources.xz")
	waitFor(t, "the root Sources.xz miss's touch", func() bool { return r.requested("/miss") > 1000 })

	r.trackOld("/twin/.")
	r.cacheAnchor("/twin/./Packages", flatPackages, 1_700_000_000)
	r.get("/twin/./Packages")
	waitFor(t, "the twin's root Packages hit's touch", func() bool { return r.requested("/twin/.") > 1000 })

	if f := aptpath.ClassifySuiteFile("/hit", "/hit/Contents-amd64.gz"); f.Touches() {
		t.Fatalf("a flat Contents-* is suite file %d, want none (no touch)", f)
	}
}

// Spec C3 and C6: a .deb a flat snapshot vouches through the suffix
// fallback — the Jenkins shape, whose Filenames are relative to the
// suite's parent — is served validated and touches the flat suite, so an
// install-only client keeps it alive.
func TestServeHTTP_VouchedFlatDebTouchesItsSuite(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	deb := []byte("jenkins 2.479.3, a vouched flat .deb")
	sum := sha256.Sum256(deb)
	inrel := writeBlob(t, r.h, rowInRelease)
	commitInlineSnapshot(t, r.h, "http", "127.0.0.1", "/jenkins/binary", inrel,
		[]cache.SnapshotMember{{Path: "InRelease", BlobHash: inrel, DeclaredSHA256: inrel}},
		[]cache.PackageHash{{CanonicalScheme: "http", CanonicalHost: "127.0.0.1", Path: "/jenkins/binary/binary/jenkins_2.479.3_all.deb",
			DeclaredSHA256: hex.EncodeToString(sum[:]), PackageName: "jenkins", Architecture: "all", Version: "2.479.3",
			FlatFilename: "binary/jenkins_2.479.3_all.deb"}})
	if _, err := r.secondConn().Exec(`UPDATE suite_freshness SET last_requested_at = 1000`); err != nil {
		t.Fatal(err)
	}
	r.serve("/jenkins/binary/jenkins_2.479.3_all.deb", upFile{body: deb})
	rec := r.get("/jenkins/binary/jenkins_2.479.3_all.deb")
	if rec.Code != http.StatusOK || rec.Body.String() != string(deb) {
		t.Fatalf("vouched flat .deb = %d %q", rec.Code, rec.Body.String())
	}
	waitFor(t, "the vouched flat .deb's touch", func() bool { return r.requested("/jenkins/binary") > 1000 })
}

// Spec §9's G row and Review Focus 1: a cache a v6 binary wrote — a flat
// repository's InRelease cached under its directory and under its `./`
// twin, no suite rows — upgraded in place. The startup pass marks nothing
// (both suites are untracked); each anchor's first hit onboards its own
// suite, the base's marker committed before the first body byte (legacy
// flat onboarding writes the marker the startup pass cannot: the E
// carry-forward).
func TestServeHTTP_OnboardsAFlatSuiteFromAV6Cache(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	if err := cache.MigrateToVersionForTest(ctx, dir, 6); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(rowInRelease)
	hash := hex.EncodeToString(sum[:])
	db := sqliteFile(t, filepath.Join(dir, "cache.db"))
	if _, err := db.Exec(`INSERT INTO blob(hash,size,created_at,refcount) VALUES(?,?,1,0)`, hash, len(rowInRelease)); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/repo/InRelease", "/repo/./InRelease"} {
		if _, err := db.Exec(`INSERT INTO url_path(canonical_scheme,canonical_host,path,blob_hash,upstream_url,is_metadata,last_requested_at,request_count)
		 VALUES('http','127.0.0.1',?,?,?,1,1700000000,3)`, p, hash, "http://127.0.0.1"+p); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	c, err := cache.Open(ctx, dir, silentLogger()) // migrates v6 → v7
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	w, err := c.NewTempBlob() // the pool file the v6 binary wrote
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(rowInRelease); err != nil {
		t.Fatal(err)
	}
	if got, err := w.Finalize(int64(len(rowInRelease))); err != nil || got != hash {
		t.Fatalf("pool file = %s, %v; want %s", got, err, hash)
	}
	if res, err := freshness.RunLegacyTrustAudit(ctx, c, silentLogger()); err != nil || res.AnchorMarkers != 0 {
		t.Fatalf("startup pass = %+v, %v; want no marker for the untracked flat suites", res, err)
	}
	parser, err := proxy.New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{Parser: parser, Cache: c, Fetch: noUpstream{}, Logger: silentLogger(), HostLimiter: hostsem.New(4),
		AdoptionEnabled: true, MaxSuites: 5000})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	for _, tc := range []struct{ path, suite string }{{"/repo/InRelease", "/repo"}, {"/repo/./InRelease", "/repo/."}} {
		p := &firstByteProbe{ResponseRecorder: httptest.NewRecorder(), c: c, suite: tc.suite}
		h.ServeHTTP(p, newProxyReqHostPort("GET", "http", "127.0.0.1", tc.path))
		if p.Code != http.StatusOK || p.Header().Get(hdrXCache) != cacheHit || !p.seen {
			t.Fatalf("%s: hit = %d %q, body written %v", tc.path, p.Code, p.Header().Get(hdrXCache), p.seen)
		}
		if p.err != nil || !p.row || !p.mark {
			t.Fatalf("%s: at the first body byte row=%v marker=%v (read error %v), want both", tc.path, p.row, p.mark, p.err)
		}
	}
	all, err := c.ListSuites(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("suite rows = %+v, %v; want /repo and /repo/.", all, err)
	}
	if _, ok, err := c.SignedSeen(ctx, "http", "127.0.0.1", "/repo"); err != nil || !ok {
		t.Fatalf("the twins' shared base marker = %v, %v", ok, err)
	}
}
