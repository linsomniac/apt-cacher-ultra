package freshness

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// adoptFlatRig adopts the flat rig's suite inline from a Release whose
// SHA256 block declares the root Packages pkgs, and fails the test unless
// the check was a first adoption.
func (r *flatRig) adoptFlatRig(pkgs []byte) {
	r.t.Helper()
	in := simtest.ClearsignedFixture(string(hashRelease("Origin: flat\n", true, false, map[string][]byte{"Packages": pkgs}, nil)))
	r.cacheRow("InRelease", in, 1, r.clock.now().Unix(), "")
	r.up.setAt(r.url("InRelease"), in)
	r.members.put(r.url("Packages"), pkgs)
	r.check()
	if r.lastResult() != "first_adoption" || r.row().CurrentSnapshotID == nil {
		r.t.Fatalf("%s: result %q; want a first adoption", r.suite.SuitePath, r.lastResult())
	}
}

// vouches reports whether a request for path on the flat rig's host is
// vouched (spec C6: DeclaredHashesForPath finds testHex64).
func (r *flatRig) vouches(path string) bool {
	r.t.Helper()
	rows, err := r.cache.DeclaredHashesForPath(context.Background(), "http", flatRigHost, path)
	if err != nil {
		r.t.Fatal(err)
	}
	return len(rows) == 1 && rows[0].DeclaredSHA256 == testHex64
}

// The E carry-forward (spec §3, libnvidia-container; phase E's DoD was
// dists/-shaped): the real flat layout — "deb …/stable/deb/amd64 /",
// an InRelease-only suite whose Release has only a SHA512 block and a
// Date frozen in 2018, and a SHA256 Packages whose Filenames start "./" —
// adopts on its first check, and vouches its .deb by the path apt
// requests, with or without the "./".
func TestDoD_NvidiaFlatSuiteAdopts(t *testing.T) {
	const suite = "/libnvidia-container/stable/deb/amd64"
	r := newFlatRig(t, suite)
	pkgs := fakePackagesStanzas(map[string]string{"./libnvidia-container1_1.17.8-1_amd64.deb": testHex64})
	text := fmt.Sprintf("Origin: https://nvidia.github.io/libnvidia-container/stable/deb/amd64\n"+
		"Label: https://nvidia.github.io/libnvidia-container/stable/deb/amd64\n"+
		"Date: Wed, 09 May 2018 22:33:58 GMT\n"+
		"SHA512:\n %s %d Packages\n", sha512Of(pkgs), len(pkgs))
	in := simtest.ClearsignedFixture(text)
	r.cacheRow("InRelease", in, 1, r.clock.now().Unix(), "")
	r.up.setAt(r.url("InRelease"), in)
	r.members.put(r.url("Packages"), pkgs)
	r.check()
	if r.lastResult() != "first_adoption" || r.row().CurrentSnapshotID == nil {
		t.Fatalf("result %q; want a first adoption", r.lastResult())
	}
	for _, p := range []string{suite + "/./libnvidia-container1_1.17.8-1_amd64.deb", suite + "/libnvidia-container1_1.17.8-1_amd64.deb"} {
		if !r.vouches(p) {
			t.Errorf("%s is not vouched", p)
		}
	}
}

// Review Focus 3 end to end (spec C5, C6; Tasks 4 and 5 pin the parts):
// each real Filename shape adopts through its suite's Packages and
// vouches the path apt requests for it — and nothing else vouches.
func TestDoD_EveryRealFilenameShapeAdoptsAndVouches(t *testing.T) {
	deep := "/" + strings.TrimSuffix(strings.Repeat("d/", 31), "/") // 31 segments
	for _, tc := range []struct {
		name, suite, filename string
		vouched, refused      []string
	}{
		{"pkgs.k8s.io (amd64/)", "/core:/stable:/v1.34/deb", "amd64/kubectl_1.34.1-1.1_amd64.deb",
			[]string{"/core:/stable:/v1.34/deb/amd64/kubectl_1.34.1-1.1_amd64.deb"},
			[]string{
				"/core:/stable:/v1.34/deb/../deb/amd64/kubectl_1.34.1-1.1_amd64.deb", // a .. segment
				"/core:/stable:/v1.34/deb//amd64/kubectl_1.34.1-1.1_amd64.deb",       // an empty segment
				"/core:/stable:/v1.34/deb2/amd64/kubectl_1.34.1-1.1_amd64.deb",       // a sibling directory
				"/core:/stable:/v1.34/deb/amd64/kubectl_1.34.2-1.1_amd64.deb",        // not declared
			}},
		{"CUDA (./)", "/compute/cuda/repos/ubuntu2404/x86_64", "./cuda-toolkit-12-6_12.6.3-1_amd64.deb",
			[]string{"/compute/cuda/repos/ubuntu2404/x86_64/./cuda-toolkit-12-6_12.6.3-1_amd64.deb",
				"/compute/cuda/repos/ubuntu2404/x86_64/cuda-toolkit-12-6_12.6.3-1_amd64.deb"}, nil},
		{"Jenkins (binary/: the parent base)", "/debian-stable/binary", "binary/jenkins_2.462.3_all.deb",
			[]string{"/debian-stable/binary/jenkins_2.462.3_all.deb"},
			[]string{"/other/binary/jenkins_2.462.3_all.deb"}},
		{"Jenkins (direct/: the parent base)", "/debian-stable/binary", "direct/jenkins_2.400_all.deb",
			[]string{"/debian-stable/direct/jenkins_2.400_all.deb"}, nil},
		{"Sublime Text (files/: the host-root base)", "/apt/stable", "files/sublime-text_build-4180_amd64.deb",
			[]string{"/files/sublime-text_build-4180_amd64.deb"}, nil},
		{"an epoch (%3a)", "/repo", "amd64/foo_1%3a2.0-1_amd64.deb",
			[]string{"/repo/amd64/foo_1%3a2.0-1_amd64.deb"}, []string{"/repo/amd64/foo_1:2.0-1_amd64.deb"}},
		{"a 31-segment suite: 33 segments are past the fallback", deep, "./amd64/x_1_amd64.deb",
			[]string{deep + "/./amd64/x_1_amd64.deb"}, []string{deep + "/amd64/x_1_amd64.deb"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newFlatRig(t, tc.suite)
			r.adoptFlatRig(fakePackagesStanzas(map[string]string{tc.filename: testHex64}))
			for _, p := range tc.vouched {
				if !r.vouches(p) {
					t.Errorf("%s is not vouched", p)
				}
			}
			for _, p := range tc.refused {
				if r.vouches(p) {
					t.Errorf("%s is vouched, want nothing", p)
				}
			}
		})
	}
	t.Run("an unsigned snapshot vouches nothing", func(t *testing.T) {
		r := newFlatRig(t, "/unsigned")
		r.adopter.adoptUnsigned = true
		pkgs := fakePackagesStanzas(map[string]string{"amd64/u_1_amd64.deb": testHex64})
		r.members.put(r.url("Packages"), pkgs)
		rel := hashRelease("Origin: unsigned\n", true, false, map[string][]byte{"Packages": pkgs}, nil)
		if err := r.adopter.RunUnsigned(context.Background(), r.suite, rel, "", ""); err != nil || r.row().CurrentSnapshotID == nil {
			t.Fatalf("RunUnsigned = %v; want an unsigned snapshot", err)
		}
		if r.vouches("/unsigned/amd64/u_1_amd64.deb") {
			t.Fatal("an unsigned snapshot vouched its .deb")
		}
	})
}

// timedVerifier refuses every signature as an untrusted signer and
// records when each verification ran (bubble time).
type timedVerifier struct {
	mu sync.Mutex
	at []time.Time
}

func (v *timedVerifier) record() ([]byte, error) {
	v.mu.Lock()
	v.at = append(v.at, time.Now())
	v.mu.Unlock()
	return nil, errRigUntrusted
}

func (v *timedVerifier) VerifyInline(context.Context, SuiteRef, []byte) ([]byte, error) {
	return v.record()
}

func (v *timedVerifier) VerifyDetached(context.Context, SuiteRef, []byte, []byte) ([]byte, error) {
	return v.record()
}

func (v *timedVerifier) times() []time.Time {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]time.Time(nil), v.at...)
}

// Review Focus 1, spec §9's G row ("flat legacy onboarding on a v6
// fixture: an untrusted flat suite makes ≤ 1 attempt per backoff step"):
// a flat InRelease a v6 binary cached, with no suite row, is upgraded in
// place; the startup pass marks nothing (the suite is untracked); its
// first serve onboards the suite with its base's marker (the handler's
// cache call, CreateSuite with the cached signed anchor:
// TestServeHTTP_OnboardsAFlatSuiteFromAV6Cache pins the handler's side).
// Its signer is not trusted: under the real scheduler for three hours each
// adoption attempt comes at least one backoff step after the last — 2 min
// doubling to 1 h — the suite keeps retrying, and the marker stays.
func TestDoD_UntrustedFlatSuiteFromAV6CacheKeepsTheBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		dir := t.TempDir()
		if err := cache.MigrateToVersionForTest(ctx, dir, 6); err != nil {
			t.Fatal(err)
		}
		pkgs := fakePackagesStanzas(map[string]string{"amd64/x_1_amd64.deb": testHex64})
		in := simtest.ClearsignedFixture(string(hashRelease("Origin: flat\n", true, false, map[string][]byte{"Packages": pkgs}, nil)))
		hash := hashOf(in)
		db, err := sql.Open("sqlite", filepath.Join(dir, "cache.db"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO blob(hash,size,created_at,refcount) VALUES(?,?,1,0)`, hash, len(in)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO url_path(canonical_scheme,canonical_host,path,blob_hash,upstream_url,is_metadata,last_requested_at,request_count)
		 VALUES('http',?,'/repo/InRelease',?,?,1,?,3)`, flatRigHost, hash, "http://"+flatRigHost+"/repo/InRelease", time.Now().Unix()); err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
		c, err := cache.Open(ctx, dir, discardLogger()) // migrates v6 → v7
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		if got := poolBlob(t, c, in); got != hash { // the pool file the v6 binary wrote
			t.Fatalf("pool file %s, want %s", got, hash)
		}
		if res, err := RunLegacyTrustAudit(ctx, c, discardAuditLogger()); err != nil || res.AnchorMarkers != 0 {
			t.Fatalf("startup pass = %+v, %v; want no marker for the untracked flat suite", res, err)
		}
		key := cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: flatRigHost, SuitePath: "/repo"}
		if res, err := c.CreateSuite(ctx, cache.SuiteSeed{SuiteKey: key, RequestedAt: time.Now().Unix(),
			Cached: []cache.CachedAnchor{{Path: "/repo/InRelease", BlobHash: hash, Signed: true}}}, cache.SuiteCaps{}, nil); err != nil ||
			res.Outcome != cache.SuiteCreated {
			t.Fatalf("onboarding = %+v, %v", res, err)
		}
		if _, ok, err := c.SignedSeen(ctx, "http", flatRigHost, "/repo"); err != nil || !ok {
			t.Fatalf("onboarding wrote no marker (%v, %v)", ok, err)
		}
		ver := &timedVerifier{}
		members := newFakeFetcher()
		members.put("http://"+flatRigHost+"/repo/Packages", pkgs)
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: ver, HostLimiter: hostsem.New(8),
			MaxConcurrent: 2, Logger: simtest.DiscardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		up := simtest.NewUpstream()
		up.Set("http://"+flatRigHost+"/repo/InRelease", simtest.Serve(in))
		_, stop := startSched(t, Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute,
			Cooldown: time.Minute, Adopter: ad, Logger: simtest.DiscardLogger(), rand: func() float64 { return 0.5 }})
		time.Sleep(3 * time.Hour)
		stop()
		at := ver.times()
		if len(at) < 6 {
			t.Fatalf("%d adoption attempts in 3 h, want at least 6: the suite must keep retrying", len(at))
		}
		// Spec C11's schedule, written out (not backoffDelay: the oracle must
		// not be the code under test), and the scheduler's fast tick
		// (periodic_refresh / 4) as the slack after each step.
		steps := []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour}
		const tick = 15 * time.Minute / 4
		for i := 1; i < len(at); i++ {
			step := steps[min(i, len(steps))-1]
			if gap := at[i].Sub(at[i-1]); gap < step || gap > step+tick {
				t.Fatalf("attempt %d came %v after attempt %d, want within one tick (%v) after its backoff step %v", i+1, gap, i, tick, step)
			}
		}
		if _, ok, err := c.SignedSeen(ctx, "http", flatRigHost, "/repo"); err != nil || !ok {
			t.Fatalf("the marker went (%v, %v)", ok, err)
		}
		if row, err := c.GetSuiteFreshness(ctx, "http", flatRigHost, "/repo"); err != nil || row.CurrentSnapshotID != nil {
			t.Fatalf("row %+v, %v; want nothing adopted", row, err)
		}
	})
}
