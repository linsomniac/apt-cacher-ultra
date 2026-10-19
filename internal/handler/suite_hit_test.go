package handler

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// cacheAnchor stores body as the url_path row of path, as a binary before
// phase D left it: no suite row, no marker. seen is its request and fetch
// stamp.
func (r *suiteRig) cacheAnchor(path string, body []byte, seen int64) {
	r.t.Helper()
	hash := writeBlob(r.t, r.h, body)
	if err := r.c.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
		Path: path, BlobHash: &hash, UpstreamURL: r.srv.URL + path, IsMetadata: true,
		LastRequestedAt: &seen, RequestCount: 1, LastFetchedAt: &seen}); err != nil {
		r.t.Fatal(err)
	}
}

// waitFor polls cond for up to 2 s (asynchronous touches and T1).
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Spec C3 legacy onboarding (deviation 2): a url_path hit on a cached
// sniff-passing anchor of a suite without a row creates the row (NULL
// check stamps, design note 3) before the body, marks its base in the
// creating transaction — the anchor in hand and the suite's other cached
// signed anchors (ruling PD-5) — and the hit's T1 then checks the suite. A
// plain-text InRelease onboards nothing and fires no T1 (ruling PD-11:
// T1 only for tracked suites); without an adopter a Release does not
// onboard either; a plain-text cached InRelease earns an onboarded
// suite no marker.
func TestServeHTTP_HitOnboardsALegacyAnchor(t *testing.T) {
	for _, adoption := range []bool{false, true} {
		r := newSuiteRig(t, adoption, 0)
		r.cacheAnchor("/r/dists/s/InRelease", rowInRelease, 1_700_000_000)
		if rec := r.get("/r/dists/s/InRelease"); rec.Code != http.StatusOK || rec.Header().Get(hdrXCache) != cacheHit {
			t.Fatalf("adoption=%v: hit = %d %q", adoption, rec.Code, rec.Header().Get(hdrXCache))
		}
		s := r.suite("/r/dists/s")
		if s == nil || s.LastCheckAt != nil || s.LastSuccessAt != nil || s.InReleaseETag != nil || s.LastRequestedAt == nil {
			t.Fatalf("adoption=%v: row = %+v, want onboarded with NULL check stamps", adoption, s)
		}
		if !r.marked("/r/dists/s") {
			t.Fatalf("adoption=%v: the onboarded InRelease earned no marker", adoption)
		}
		select {
		case <-r.fresh.signal:
		case <-time.After(2 * time.Second):
			t.Fatalf("adoption=%v: the hit fired no T1", adoption)
		}

		r.cacheAnchor("/r/dists/p/InRelease", []byte("Origin: plain\n"), 1_700_000_000)
		r.get("/r/dists/p/InRelease")
		if r.suite("/r/dists/p") != nil {
			t.Errorf("adoption=%v: a plain-text InRelease onboarded its suite", adoption)
		}

		r.cacheAnchor("/r/dists/d/Release", rowRelease, 1_700_000_000)
		r.cacheAnchor("/r/dists/d/Release.gpg", rowGPG, 1_700_000_100)
		r.get("/r/dists/d/Release")
		if got := r.suite("/r/dists/d") != nil; got != adoption {
			t.Fatalf("adoption=%v: Release hit created a row = %v", adoption, got)
		}
		if adoption {
			// Release itself earns no marker; the cached Release.gpg does,
			// at its own stamp.
			if at, ok, err := r.c.SignedSeen(context.Background(), "http", "127.0.0.1", "/r/dists/d"); err != nil || !ok || at != 1_700_000_100 {
				t.Fatalf("marker = %d %v %v, want the cached Release.gpg's 1700000100", at, ok, err)
			}
			// A plain-text cached InRelease earns the onboarded suite no marker.
			r.cacheAnchor("/r/dists/q/InRelease", []byte("Origin: plain\n"), 1_700_000_000)
			r.cacheAnchor("/r/dists/q/Release", rowRelease, 1_700_000_000)
			r.get("/r/dists/q/Release")
			if r.suite("/r/dists/q") == nil || r.marked("/r/dists/q") {
				t.Fatal("q: want onboarded through Release, with no marker for its plain-text InRelease")
			}
		}
		r.h.Close() // drains the T1 goroutines
		for _, c := range r.fresh.snapshot() {
			if c.suitePath == "/r/dists/p" || (!adoption && c.suitePath == "/r/dists/d") {
				t.Errorf("adoption=%v: T1 fired for the untracked suite %s", adoption, c.suitePath)
			}
		}
	}
}

// Ruling PD-5 on the hit path: an onboarding whose suite's cached
// Release.gpg changed after it was sniffed is unproven, and the backstop
// marks the changed anchor after the commit.
func TestServeHTTP_OnboardingBackstopMarksAChangedAnchor(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	r.cacheAnchor("/r/dists/b/Release", rowRelease, 1_700_000_000)
	r.cacheAnchor("/r/dists/b/Release.gpg", []byte("not a signature\n"), 1_700_000_000)
	r.h.cachedAnchorsHook = func() { // the cached Release.gpg becomes a real signature after the sniff
		r.h.cachedAnchorsHook = nil
		r.cacheAnchor("/r/dists/b/Release.gpg", rowGPG, 1_700_000_200)
	}
	if rec := r.get("/r/dists/b/Release"); rec.Code != http.StatusOK || r.suite("/r/dists/b") == nil {
		t.Fatalf("onboarding hit = %d", rec.Code)
	}
	if at, ok, err := r.c.SignedSeen(context.Background(), "http", "127.0.0.1", "/r/dists/b"); err != nil || !ok || at != 1_700_000_200 {
		t.Fatalf("marker = %d %v %v, want the changed Release.gpg marked by the backstop at 1700000200", at, ok, err)
	}
}

// sfWaiters is how many callers wait on key's in-flight singleflight call.
func sfWaiters(g *sfGroup, key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c, ok := g.calls[key]; ok {
		return c.waiters
	}
	return 0
}

// Design review M8: concurrent hits on one untracked suite share one
// onboarding (one writer operation, not one per request), and every one
// of them is served from the onboarded suite.
func TestServeHTTP_ConcurrentHitsShareOneOnboarding(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	r.cacheAnchor("/r/dists/s/InRelease", rowInRelease, 1_700_000_000)
	var calls atomic.Int32
	release := make(chan struct{})
	r.h.cachedAnchorsHook = func() { // runs inside each onboarding's creation
		if calls.Add(1) == 1 {
			<-release // the first waits until the others queue behind it
		}
	}
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = r.get("/r/dists/s/InRelease").Code
		}()
	}
	queued := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if sfWaiters(r.h.sf, "onboard|http|127.0.0.1|/r/dists/s") == len(codes)-1 {
			queued = true
			break
		}
	}
	close(release)
	wg.Wait()
	if !queued || calls.Load() != 1 {
		t.Fatalf("queued %v, %d onboardings ran; want the hits to share one", queued, calls.Load())
	}
	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("hit %d = %d", i, code)
		}
	}
	if r.suite("/r/dists/s") == nil || !r.marked("/r/dists/s") {
		t.Fatal("the shared onboarding did not create and mark the suite")
	}
}

// requested reads a suite's client-interest stamp.
func (r *suiteRig) requested(suite string) int64 {
	r.t.Helper()
	s := r.suite(suite)
	if s == nil {
		r.t.Fatalf("%s has no row", suite)
	}
	return derefI64(s.LastRequestedAt)
}

// trackOld gives suite a row whose client-interest stamp is old and whose
// touch is due.
func (r *suiteRig) trackOld(suite string) {
	r.t.Helper()
	if err := r.c.SeedSuiteForTest(context.Background(), cache.SuiteFreshness{CanonicalScheme: "http",
		CanonicalHost: "127.0.0.1", SuitePath: suite}); err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.secondConn().Exec(`UPDATE suite_freshness SET last_requested_at = 1000 WHERE suite_path = ?`, suite); err != nil {
		r.t.Fatal(err)
	}
}

// Spec C3 client interest: a tracked suite's anchor served from a
// snapshot, the url_path cache, the miss path, or stale records interest
// (TouchSuite).
func TestServeHTTP_AnchorServesTouchTheirSuite(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	r.trackOld("/r/dists/hit")
	r.cacheAnchor("/r/dists/hit/InRelease", rowInRelease, 1_700_000_000)
	r.get("/r/dists/hit/InRelease")
	waitFor(t, "the hit's touch", func() bool { return r.requested("/r/dists/hit") > 1000 })

	r.trackOld("/r/dists/miss")
	r.serve("/r/dists/miss/Release.gpg", upFile{body: rowGPG})
	r.get("/r/dists/miss/Release.gpg")
	waitFor(t, "the miss's touch", func() bool { return r.requested("/r/dists/miss") > 1000 })

	r.trackOld("/r/dists/stale")
	r.cacheAnchor("/r/dists/stale/InRelease", rowInRelease, 1_700_000_000)
	r.h.serve = config.ServeConfig{ServeStaleWhenUpstreamDown: true}
	req, err := r.h.parser.Parse(r.srv.URL+"/r/dists/stale/InRelease", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if !r.h.tryServeStale(httptest.NewRecorder(), proxyReq("GET", r.srv.URL, "/r/dists/stale/InRelease"), req, 0, time.Now()) {
		t.Fatal("no stale serve")
	}
	waitFor(t, "the stale serve's touch", func() bool { return r.requested("/r/dists/stale") > 1000 })

	// commitInlineSnapshot seeds its row through SeedSuiteForTest, which
	// notes no touch, so the snapshot serve's touch is due.
	inrel := writeBlob(t, r.h, rowInRelease)
	commitInlineSnapshot(t, r.h, "http", "127.0.0.1", "/r/dists/snap", inrel,
		[]cache.SnapshotMember{{Path: "InRelease", BlobHash: inrel, DeclaredSHA256: inrel}}, nil)
	if _, err := r.secondConn().Exec(`UPDATE suite_freshness SET last_requested_at = 1000 WHERE suite_path = '/r/dists/snap'`); err != nil {
		t.Fatal(err)
	}
	r.get("/r/dists/snap/InRelease")
	waitFor(t, "the snapshot serve's touch", func() bool { return r.requested("/r/dists/snap") > 1000 })
}

// Spec C3 client interest: an anchor served by the snapshot-member
// recovery path (its pool file was lost and fetched again) touches its
// suite too.
func TestServeHTTP_RecoveredAnchorServeTouchesItsSuite(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	inrel := writeBlob(t, r.h, rowInRelease)
	commitInlineSnapshot(t, r.h, "http", "127.0.0.1", "/r/dists/rec", inrel,
		[]cache.SnapshotMember{{Path: "InRelease", BlobHash: inrel, DeclaredSHA256: inrel}}, nil)
	if err := os.Remove(r.h.cache.BlobPath(inrel)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.secondConn().Exec(`UPDATE suite_freshness SET last_requested_at = 1000 WHERE suite_path = '/r/dists/rec'`); err != nil {
		t.Fatal(err)
	}
	r.serve("/r/dists/rec/InRelease", upFile{body: rowInRelease})
	if rec := r.get("/r/dists/rec/InRelease"); rec.Code != http.StatusOK || rec.Header().Get(hdrXCache) != cacheMiss {
		t.Fatalf("recovery = %d %q, want 200 MISS", rec.Code, rec.Header().Get(hdrXCache))
	}
	waitFor(t, "the recovery serve's touch", func() bool { return r.requested("/r/dists/rec") > 1000 })
}

// Spec C3: a hash-validated .deb served by the miss path touches the
// suite whose current snapshot vouched it.
func TestServeHTTP_VouchedDebMissTouchesItsSuite(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	deb := []byte("a vouched .deb fetched on a miss")
	const debPath = "/r/pool/main/h/hello_2_amd64.deb"
	r.serve(debPath, upFile{body: deb})
	inrel := writeBlob(t, r.h, rowInRelease)
	commitInlineSnapshot(t, r.h, "http", "127.0.0.1", "/r/dists/s", inrel,
		[]cache.SnapshotMember{{Path: "InRelease", BlobHash: inrel, DeclaredSHA256: inrel}},
		[]cache.PackageHash{{CanonicalScheme: "http", CanonicalHost: "127.0.0.1", Path: debPath, DeclaredSHA256: sha256Hex(deb)}})
	if _, err := r.secondConn().Exec(`UPDATE suite_freshness SET last_requested_at = 1000`); err != nil {
		t.Fatal(err)
	}
	if rec := r.get(debPath); rec.Code != http.StatusOK || rec.Body.String() != string(deb) {
		t.Fatalf("deb miss = %d %q", rec.Code, rec.Body.String())
	}
	waitFor(t, "the vouched .deb miss's touch", func() bool { return r.requested("/r/dists/s") > 1000 })
}

// cacheLegacyAnchor stores body as the url_path anchor of suite on the
// flip stack's host, with no suite row (a binary before phase D).
func (s *flipStack) cacheLegacyAnchor(suite string, body []byte) {
	s.t.Helper()
	hash := writeBlob(s.t, s.h, body)
	at := wallclock.Unix()
	if err := s.c.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: "http", CanonicalHost: flipSimHost,
		Path: suite + "/InRelease", BlobHash: &hash, UpstreamURL: "http://" + flipSimHost + suite + "/InRelease",
		IsMetadata: true, LastRequestedAt: &at, RequestCount: 1, LastFetchedAt: &at}); err != nil {
		s.t.Fatal(err)
	}
}

// Design note 9 on the hit path: an onboarding whose late registration
// waited in a flip's hold serves what is current after the flip — the
// snapshot it committed — and its line says it waited (flip_held_ms).
func TestServeHTTP_OnboardingThatWaitedServesTheNewSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs := &syncLog{}
		s := newFlipStackLogging(t, 10*time.Second, slog.New(slog.NewJSONHandler(logs, nil)))
		const suite = "/n/dists/s"
		s.cacheLegacyAnchor(suite, rowInRelease)
		snapBody := simtest.ClearsignedFixture("Origin: snapshot\n")
		hash := writeBlob(t, s.h, snapBody)
		ctx := context.Background()
		cand, _, err := s.c.InsertCandidateSnapshot(ctx, cache.SnapshotCandidate{CanonicalScheme: "http",
			CanonicalHost: flipSimHost, SuitePath: suite, InReleaseHash: &hash})
		if err != nil {
			t.Fatal(err)
		}
		flipped := s.holdUntilTracked(suite, time.Second, func() error {
			return s.c.CommitAdoption(ctx, cand, []cache.SnapshotMember{{SnapshotID: cand, Path: "InRelease",
				BlobHash: hash, DeclaredSHA256: hash}}, nil, nil, nil, false)
		})
		rec := httptest.NewRecorder()
		s.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+flipSimHost+suite+"/InRelease", nil))
		<-flipped
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), snapBody) {
			t.Fatalf("status %d body %q; want the committed snapshot's InRelease", rec.Code, rec.Body.String())
		}
		lines := logRecords(logs, "request", slog.LevelInfo)
		if len(lines) != 1 || lines[0]["outcome"] != "hit" || lines[0]["flip_held_ms"] == nil {
			t.Fatalf("request lines %v, want one hit line with flip_held_ms", lines)
		}
	})
}

// Design review M1 on the hit path: an onboarding hit whose client leaves
// during the late registration's wait is a client abort — 503, no WARN.
func TestServeHTTP_OnboardingClientThatLeftIsAClientAbort(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs := &syncLog{}
		s := newFlipStackLogging(t, 10*time.Second, slog.New(slog.NewJSONHandler(logs, nil)))
		const suite = "/n/dists/s"
		s.cacheLegacyAnchor(suite, rowInRelease)
		flipped := s.holdUntilTracked(suite, 2*time.Second, func() error { return nil })
		rctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(time.Second, cancel) // the client leaves during the late wait
		rec := httptest.NewRecorder()
		s.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+flipSimHost+suite+"/InRelease", nil).WithContext(rctx))
		<-flipped
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status %d, want 503", rec.Code)
		}
		if warns := logRecords(logs, "", slog.LevelWarn); len(warns) != 0 {
			t.Errorf("a client that left during the onboarding wait logged %d WARN or worse, first %v", len(warns), warns[0])
		}
		lines := logRecords(logs, "request", slog.LevelInfo)
		if len(lines) != 1 || lines[0]["outcome"] != "client_canceled" || lines[0]["flip_held_ms"] == nil {
			t.Fatalf("request lines %v, want one client_canceled line with flip_held_ms", lines)
		}
	})
}

// Spec C3: a hash-validated .deb serve touches every suite whose current
// snapshot vouched it, so install-only clients keep their suite alive.
func TestServeHTTP_VouchedDebServeTouchesItsSuite(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	deb := []byte("a vouched .deb")
	debHash := writeBlob(t, r.h, deb)
	inrel := writeBlob(t, r.h, rowInRelease)
	const debPath = "/r/pool/main/h/hello_1_amd64.deb"
	commitInlineSnapshot(t, r.h, "http", "127.0.0.1", "/r/dists/s", inrel,
		[]cache.SnapshotMember{{Path: "InRelease", BlobHash: inrel, DeclaredSHA256: inrel}},
		[]cache.PackageHash{{CanonicalScheme: "http", CanonicalHost: "127.0.0.1", Path: debPath, DeclaredSHA256: debHash}})
	if _, err := r.secondConn().Exec(`UPDATE suite_freshness SET last_requested_at = 1000`); err != nil {
		t.Fatal(err)
	}
	now := int64(1_700_000_000)
	if err := r.c.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
		Path: debPath, BlobHash: &debHash, UpstreamURL: r.srv.URL + debPath, LastRequestedAt: &now, RequestCount: 1,
		LastFetchedAt: &now}); err != nil {
		t.Fatal(err)
	}
	if rec := r.get(debPath); rec.Code != http.StatusOK || rec.Body.String() != string(deb) {
		t.Fatalf("deb hit = %d %q", rec.Code, rec.Body.String())
	}
	waitFor(t, "the vouched .deb's touch", func() bool { return r.requested("/r/dists/s") > 1000 })
}

// Spec C3: which serves record interest in which suite.
func TestServedSuite_Rules(t *testing.T) {
	key := cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/s"}
	for _, tc := range []struct {
		file    aptpath.SuiteFile
		tracked bool
		status  int
		want    bool
	}{
		{aptpath.FileInRelease, true, 200, true},
		{aptpath.FileRelease, true, 304, true},
		{aptpath.FileReleaseGPG, true, 206, true},
		{aptpath.FileFlatIndex, true, 200, true},
		{aptpath.FileInRelease, true, 404, false},
		{aptpath.FileInRelease, false, 200, false},
		{aptpath.NotSuiteFile, true, 200, false},
	} {
		got := servedSuite(&suiteReq{key: key, file: tc.file, tracked: tc.tracked}, tc.status)
		if (got != nil) != tc.want || (got != nil && got[0] != key) {
			t.Errorf("file %v tracked %v status %d: %v, want touch=%v", tc.file, tc.tracked, tc.status, got, tc.want)
		}
	}
	req := &proxy.Request{CanonicalScheme: "http", CanonicalHost: "h"}
	if got := vouchedSuites(req, packageValidation{Validated: true, Suites: []string{"/a", "/b"}}, 200); len(got) != 2 ||
		got[1] != (cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/b"}) {
		t.Errorf("vouched = %v", got)
	}
	if vouchedSuites(req, packageValidation{Suites: []string{"/a"}}, 200) != nil ||
		vouchedSuites(req, packageValidation{Validated: true, Suites: []string{"/a"}}, 404) != nil {
		t.Error("an unvalidated or failed serve touched a suite")
	}
	rows := []cache.DeclaredHash{{DeclaredSHA256: "x", SuitePath: "/a"}, {DeclaredSHA256: "x", SuitePath: "/a"},
		{DeclaredSHA256: "y", SuitePath: "/b"}, {DeclaredSHA256: "x", SuitePath: "/c"}}
	if got := vouchingSuites(rows, "x"); !slices.Equal(got, []string{"/a", "/c"}) {
		t.Errorf("vouchingSuites = %v, want [/a /c]", got)
	}
}

// Deferred minor C-23: anchorStart reads nothing while the barrier is off,
// and one EXISTS per anchor request while it is on.
func TestAnchorStart_ReadsOnlyWhileTheBarrierIsOn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		off := newFlipStack(t, 0)
		off.adopt("/u/dists/s", 1)
		off.get("/u/dists/s/InRelease", 0)
		if n := off.h.existsReads.Load(); n != 0 {
			t.Fatalf("barrier off: %d registration reads, want 0", n)
		}
		on := newFlipStack(t, 10*time.Second)
		on.adopt("/u/dists/s", 1)
		on.get("/u/dists/s/InRelease", 0)
		on.get("/u/dists/s/main/binary-amd64/Packages", 0)
		if n := on.h.existsReads.Load(); n != 1 {
			t.Fatalf("barrier on: %d registration reads for one anchor and one index, want 1", n)
		}
	})
}

// Deferred minor C-22: a failed registration read registers the anchor
// serve anyway (the safe direction: a missed registration could hand the
// client the old anchor after a flip).
func TestAnchorStart_RegistersOnAReadError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newFlipStack(t, 10*time.Second)
		s.h.suiteExistsForTest = func(cache.SuiteKey) (bool, error) { return false, errors.New("injected read failure") }
		s.get("/u/dists/s/InRelease", 0)
		if n := s.gate.Len(); n != 1 {
			t.Fatalf("gate entries after a failed read = %d, want 1 (registered)", n)
		}
	})
}

// Deferred minor C-24: the touch goroutine recovers its panic and logs it
// once with the stack.
func TestTouchAsync_RecoversItsPanic(t *testing.T) {
	logs := &syncLog{}
	h := &Handler{logger: slog.New(slog.NewJSONHandler(logs, nil))} // nil cache: the touch panics
	touchPanicStackLogged.Store(false)
	h.touchAsync(&proxy.Request{CanonicalScheme: "http", CanonicalHost: "h", Path: "/r/dists/s/InRelease"}, true, nil)
	recs := logRecords(logs, "handler_goroutine_panicked", slog.LevelError)
	if len(recs) != 1 || recs[0]["where"] != "touch" || recs[0]["panic"] == nil || !strings.Contains(recs[0]["stack"].(string), "touchAsync") {
		t.Fatalf("records = %+v; want one with where=touch, the panic and its stack", recs)
	}
}
