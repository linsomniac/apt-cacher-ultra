package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// strictSnap is one current snapshot of a strict matrix row: its suite,
// its coverage, and its package_hash rows — each .deb's path (at the
// suite's FlatBase, as spec C6 materialises it) and flat_filename ("" for
// a dists/ row) — declaring the strict rig upstream's bytes, or the
// SHA-256 declared when it is set.
type strictSnap struct {
	suite    string
	complete bool
	debs     map[string]string
	declared string
}

// commitStrictSnap adopts s on host through the cache (an inline
// snapshot) and returns its id.
func commitStrictSnap(t *testing.T, h *Handler, scheme, host string, s strictSnap) int64 {
	t.Helper()
	ctx := context.Background()
	inrel := writeBlob(t, h, simtest.ClearsignedFixture("InRelease for "+s.suite))
	if err := h.cache.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: scheme, CanonicalHost: host,
		SuitePath: s.suite}); err != nil {
		t.Fatal(err)
	}
	id, _, err := h.cache.InsertTrackedCandidateForTest(ctx, cache.SnapshotCandidate{CanonicalScheme: scheme,
		CanonicalHost: host, SuitePath: s.suite, InReleaseHash: &inrel})
	if err != nil {
		t.Fatal(err)
	}
	declared := sha256Hex([]byte("upstream bytes"))
	if s.declared != "" {
		declared = s.declared
	}
	var rows []cache.PackageHash
	for path, flat := range s.debs {
		rows = append(rows, cache.PackageHash{SnapshotID: id, CanonicalScheme: scheme, CanonicalHost: host, Path: path,
			FlatFilename: flat, DeclaredSHA256: declared})
	}
	if err := h.cache.CommitAdoption(ctx, id, []cache.SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: inrel,
		DeclaredSHA256: inrel}}, nil, rows, nil, s.complete); err != nil {
		t.Fatal(err)
	}
	return id
}

// PR #8 parity item 4 (plan design note 17): PR #8's nine
// TestPhase3StrictMode_FlatSnapshotScopedToItsDirectory cases under C6
// vouching and C10's host-wide rule, and three new ones, through the real
// handler. A vouched .deb is served after its hash check — a fallback-
// vouched one whose bytes differ from the declaration is refused
// (hash_validation_failure) — and an unvouched one on a host whose every
// signed current snapshot is complete is refused (502, Retry-After 60,
// unvouched_deb_refused) before upstream is asked, whatever its layout;
// an incomplete snapshot of either layout turns refusal off for its host
// (unvouched_deb_passthrough_no_coverage names it); and a .deb of a
// pkgs.k8s.io minor whose first adoption is running is served once that
// adoption commits within C10's wait, but refused while it is only queued
// (the refusal asked whether an adoption was in flight). Two rows serve a
// cached .deb: the hit path vouches through the fallback and refuses as
// the miss path does.
func TestServeHTTP_FlatStrictMatrix(t *testing.T) {
	const unknownDists = "/ubuntu/pool/main/u/unknown_1_amd64.deb"
	k8s := func(minor string) strictSnap {
		suite := "/core:/stable:/v1." + minor + "/deb"
		return strictSnap{suite: suite, complete: true,
			debs: map[string]string{suite + "/amd64/kubectl_1." + minor + ".0-1.1_amd64.deb": "amd64/kubectl_1." + minor + ".0-1.1_amd64.deb"}}
	}
	var (
		dists     = strictSnap{suite: "/ubuntu/dists/noble", complete: true, debs: map[string]string{"/ubuntu/pool/main/h/hello_1_amd64.deb": ""}}
		incDists  = strictSnap{suite: "/ubuntu/dists/noble", complete: false}
		flat      = strictSnap{suite: "/flat", complete: true, debs: map[string]string{"/flat/amd64/x.deb": "amd64/x.deb"}}
		incFlat   = strictSnap{suite: "/flat", complete: false, debs: map[string]string{"/flat/amd64/x.deb": "amd64/x.deb"}}
		twin      = strictSnap{suite: "/flat/.", complete: true, debs: map[string]string{"/flat/amd64/x.deb": "amd64/x.deb"}}
		twinOther = strictSnap{suite: "/flat/.", complete: true, debs: map[string]string{"/flat/amd64/x.deb": "amd64/x.deb"},
			declared: sha256Hex([]byte("other bytes"))}
		jenkins    = strictSnap{suite: "/r/sub", complete: true, debs: map[string]string{"/r/sub/amd64/x.deb": "amd64/x.deb"}}
		thirdMinor = k8s("35")
	)
	for _, tc := range []struct {
		name  string
		snaps []strictSnap
		path  string
		want  int
		// adoption: "running" commits thirdMinor during C10's wait,
		// "queued" reports no adoption in flight, "" wires none.
		adoption string
		// passthrough: the index in snaps of the snapshot
		// unvouched_deb_passthrough_no_coverage names; -1 for none.
		passthrough int
		// cached: the .deb is in the cache already (the rig upstream's
		// bytes), so the request takes the hit path.
		cached bool
		// mismatch: a 502 is the hash check's (hash_validation_failure,
		// after the fetch), not the strict refusal's.
		mismatch bool
	}{
		{"complete dists and flat: an unvouched dists .deb", []strictSnap{dists, flat}, unknownDists, 502, "", -1, false, false},
		{"the same host: a declared flat .deb", []strictSnap{dists, flat}, "/flat/amd64/x.deb", 200, "", -1, false, false},
		{"the same host: an undeclared .deb under the flat suite", []strictSnap{dists, flat}, "/flat/amd64/other.deb", 502, "", -1, false, false},
		{"suite /flat/.: a declared .deb at its FlatBase", []strictSnap{dists, twin}, "/flat/amd64/x.deb", 200, "", -1, false, false},
		{"suite /flat/.: the declared .deb by its ./ path", []strictSnap{dists, twin}, "/flat/./amd64/x.deb", 200, "", -1, false, false},
		// PR #8 parity: still refused — row 1's mechanism (the fallback's
		// ".." stop is unreachable with valid rows; TestFlatSplits pins it).
		{"a .. path", []strictSnap{dists, flat}, "/flat/../ubuntu/pool/unknown.deb", 502, "", -1, false, false},
		{"the Jenkins shape: a parent base", []strictSnap{dists, jenkins}, "/r/amd64/x.deb", 200, "", -1, false, false},
		{"a flat-only host: an undeclared .deb elsewhere", []strictSnap{flat}, "/elsewhere/y_1_amd64.deb", 502, "", -1, false, false},
		{"an incomplete dists snapshot and a flat one: an unvouched .deb", []strictSnap{incDists, flat}, unknownDists, 200, "", 0, false, false},
		{"complete dists and an incomplete flat snapshot: an unvouched dists .deb", []strictSnap{dists, incFlat}, unknownDists, 200, "", 1, false, false},
		{"pkgs.k8s.io: a third minor's first adoption is running", []strictSnap{k8s("33"), k8s("34")},
			"/core:/stable:/v1.35/deb/amd64/kubectl_1.35.0-1.1_amd64.deb", 200, "running", -1, false, false},
		{"pkgs.k8s.io: a third minor's first adoption is queued", []strictSnap{k8s("33"), k8s("34")},
			"/core:/stable:/v1.35/deb/amd64/kubectl_1.35.0-1.1_amd64.deb", 502, "queued", -1, false, false},
		{"suite /flat/.: by its ./ path, bytes other than declared", []strictSnap{dists, twinOther}, "/flat/./amd64/x.deb", 502, "", -1, false, true},
		{"the hit path: suite /flat/.'s cached .deb by its ./ path", []strictSnap{dists, twin}, "/flat/./amd64/x.deb", 200, "", -1, true, false},
		{"the hit path: a cached undeclared .deb on a complete host", []strictSnap{dists, flat}, "/flat/amd64/other.deb", 502, "", -1, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			h, srv, logs := newPhase3StrictHandlerCapturing(t, true, true, &hits)
			scheme, host, port := splitURL(t, srv.URL)
			canon := hostKey(host, port)
			var ids []int64
			for _, s := range tc.snaps {
				ids = append(ids, commitStrictSnap(t, h, scheme, canon, s))
			}
			if tc.cached {
				blob := writeBlob(t, h, []byte("upstream bytes"))
				if err := h.cache.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: scheme, CanonicalHost: canon,
					Path: tc.path, BlobHash: &blob, UpstreamURL: srv.URL + tc.path}); err != nil {
					t.Fatal(err)
				}
			}
			a := &fakeAdoptions{}
			switch tc.adoption {
			case "running":
				a.inFlight, a.ok = true, true
				a.during = func() {
					commitStrictSnap(t, h, scheme, canon, thirdMinor)
					a.inFlight = false
				}
				h.adoptions = a
			case "queued":
				h.adoptions = a
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, proxyReq("GET", srv.URL, tc.path))
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d", rec.Code, tc.want)
			}
			refusal := "unvouched_deb_refused"
			if tc.mismatch {
				refusal = "hash_validation_failure"
			}
			refused := logs.find(refusal)
			switch {
			case tc.want == http.StatusOK && rec.Body.String() != "upstream bytes":
				t.Fatalf("body %q, want the upstream's", rec.Body.String())
			case tc.want == http.StatusOK && len(refused) != 0:
				t.Fatalf("served, but logged %v", refused)
			case tc.want == http.StatusOK && tc.cached && rec.Header().Get(hdrXCache) != cacheHit:
				t.Fatalf("X-Cache %q, want a hit", rec.Header().Get(hdrXCache))
			case tc.want == http.StatusBadGateway && (len(refused) != 1 || rec.Header().Get("Retry-After") != "60"):
				t.Fatalf("refused with Retry-After %q and %d %s lines, want 60 and 1",
					rec.Header().Get("Retry-After"), len(refused), refusal)
			}
			// Upstream is asked once by a served miss and by the hash
			// check's fetch, never by a hit or a strict refusal (SPEC3
			// §6.2: refused before any upstream dial).
			wantHits := int32(1)
			if tc.cached || (tc.want == http.StatusBadGateway && !tc.mismatch) {
				wantHits = 0
			}
			if n := hits.Load(); n != wantHits {
				t.Fatalf("upstream asked %d times, want %d", n, wantHits)
			}
			pass := logs.find("unvouched_deb_passthrough_no_coverage")
			if tc.passthrough < 0 {
				if len(pass) != 0 {
					t.Fatalf("unexpected passthrough line %v", pass)
				}
			} else if len(pass) != 1 || pass[0]["incomplete_snapshot_id"] != float64(ids[tc.passthrough]) {
				t.Fatalf("passthrough lines %v, want one naming snapshot %d", pass, ids[tc.passthrough])
			}
			if waits := a.waits.Load(); waits != map[string]int32{"running": 1}[tc.adoption] {
				t.Fatalf("waited %d times for adoptions in flight", waits)
			}
			// C10: the refusal asked whether an adoption was in flight —
			// "queued" is told apart from "no waiter wired".
			if asks := a.asks.Load(); asks != map[string]int32{"running": 1, "queued": 1}[tc.adoption] {
				t.Fatalf("asked %d times about adoptions in flight", asks)
			}
		})
	}
}

// PR #8's TestPhase3StrictMode_SnapshotCountExcludesOutOfScopeFlat, under
// C10: current_snapshot_count counts every signed current snapshot on the
// host — a flat suite's and its /. twin's included — and never an
// unsigned snapshot or an anchorless suite (a row without a snapshot).
func TestServeHTTP_CurrentSnapshotCountCountsFlatSnapshots(t *testing.T) {
	h, srv, logs := newPhase3StrictHandlerCapturing(t, true, true, nil)
	scheme, host, port := splitURL(t, srv.URL)
	canon := hostKey(host, port)
	for _, s := range []strictSnap{{suite: "/ubuntu/dists/noble", complete: true}, {suite: "/flat", complete: true},
		{suite: "/flat/.", complete: true}} {
		commitStrictSnap(t, h, scheme, canon, s)
	}
	commitUnsigned(t, h, scheme, canon, "/unsigned", "/unsigned/amd64/u_1_amd64.deb")
	if err := h.cache.SeedSuiteForTest(context.Background(), cache.SuiteFreshness{CanonicalScheme: scheme,
		CanonicalHost: canon, SuitePath: "/anchorless"}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/pool/main/u/unknown_1_amd64.deb"))
	refused := logs.find("unvouched_deb_refused")
	if rec.Code != http.StatusBadGateway || len(refused) != 1 || refused[0]["current_snapshot_count"] != float64(3) {
		t.Fatalf("status %d, refusals %v; want 502 with current_snapshot_count 3", rec.Code, refused)
	}
}

// Spec C6 step 3 through the suffix fallback (the Task 5 review's
// Important 1): two signed current suites at one base that declare
// different hashes for one Filename are a package_hash_conflict — 502,
// Retry-After 60, refused before upstream is asked — never an
// order-dependent answer. The twins of residual 2 reached by a "./"
// request that misses their exact rows, and two channels of a host-root
// repository (Sublime's apt/stable and apt/dev, B = "").
func TestServeHTTP_AFallbackTieIsAConflict(t *testing.T) {
	for _, tc := range []struct {
		name  string
		snaps []strictSnap
		path  string
	}{
		{"the twins through ./", []strictSnap{
			{suite: "/t", complete: true, debs: map[string]string{"/t/x_1_amd64.deb": "x_1_amd64.deb"}},
			{suite: "/t/.", complete: true, debs: map[string]string{"/t/x_1_amd64.deb": "x_1_amd64.deb"},
				declared: sha256Hex([]byte("newer bytes"))},
		}, "/t/./x_1_amd64.deb"},
		{"two channels at the host root", []strictSnap{
			{suite: "/apt/stable", complete: true, debs: map[string]string{"/apt/stable/files/s_1_amd64.deb": "files/s_1_amd64.deb"}},
			{suite: "/apt/dev", complete: true, debs: map[string]string{"/apt/dev/files/s_1_amd64.deb": "files/s_1_amd64.deb"},
				declared: sha256Hex([]byte("dev bytes"))},
		}, "/files/s_1_amd64.deb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			h, srv, logs := newPhase3StrictHandlerCapturing(t, true, true, &hits)
			scheme, host, port := splitURL(t, srv.URL)
			canon := hostKey(host, port)
			for _, s := range tc.snaps {
				commitStrictSnap(t, h, scheme, canon, s)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, proxyReq("GET", srv.URL, tc.path))
			conflicts := logs.find("package_hash_conflict")
			if rec.Code != http.StatusBadGateway || rec.Header().Get("Retry-After") != "60" || len(conflicts) != 1 {
				t.Fatalf("status %d, Retry-After %q, package_hash_conflict %v; want 502, 60 and one conflict line",
					rec.Code, rec.Header().Get("Retry-After"), conflicts)
			}
			d, _ := conflicts[0]["declared"].([]any)
			if len(d) != 2 {
				t.Fatalf("conflict declared %v, want both suites' rows", conflicts[0]["declared"])
			}
			// The final review's tests lens M-5 (ruling PG-33): each row
			// names its suite, so an operator sees that the two rows are
			// the twins (or the two channels) without joining snapshot ids.
			var suites, want []string
			for _, e := range d {
				m, _ := e.(map[string]any)
				s, _ := m["suite_path"].(string)
				suites = append(suites, s)
			}
			for _, s := range tc.snaps {
				want = append(want, s.suite)
			}
			slices.Sort(suites)
			slices.Sort(want)
			if !slices.Equal(suites, want) {
				t.Fatalf("conflict rows name suites %q, want %q", suites, want)
			}
			if n := hits.Load(); n != 0 {
				t.Fatalf("upstream asked %d times, want 0 (the conflict is refused before the fetch)", n)
			}
		})
	}
}
