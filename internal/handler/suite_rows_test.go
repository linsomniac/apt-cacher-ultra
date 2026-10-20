package handler

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// Spec C3 fixtures: bodies that pass, and one that fails, the sniffs.
var (
	rowInRelease = simtest.ClearsignedFixture("Origin: T\nSuite: s\n")
	rowRelease   = []byte("Origin: T\nSHA256:\n " + strings.Repeat("a", 64) + " 1 main/binary-amd64/Packages\n")
	rowGPG       = []byte("-----BEGIN PGP SIGNATURE-----\n\nwsBcBAABCAAQ\n=AAAA\n-----END PGP SIGNATURE-----\n")
	rowHTML      = []byte("<html><body>Not Found</body></html>\n")
)

// upFile is one file the rig's upstream serves.
type upFile struct {
	body          []byte
	etag, lastMod string
}

// suiteRig is a handler over a real cache and an httptest upstream whose
// files the test sets (spec C3 rows and markers).
type suiteRig struct {
	t     *testing.T
	h     *Handler
	c     *cache.Cache
	dir   string
	srv   *httptest.Server
	logs  *syncLog
	fresh *recordingFreshness

	mu    sync.Mutex
	files map[string]upFile
}

func newSuiteRig(t *testing.T, adoption bool, maxSuites int) *suiteRig {
	t.Helper()
	r := &suiteRig{t: t, dir: t.TempDir(), logs: &syncLog{}, fresh: newRecordingFreshness(), files: map[string]upFile{}}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		f, ok := r.files[req.URL.Path]
		r.mu.Unlock()
		if !ok {
			http.NotFound(w, req)
			return
		}
		if f.etag != "" {
			w.Header().Set("ETag", f.etag)
		}
		if f.lastMod != "" {
			w.Header().Set("Last-Modified", f.lastMod)
		}
		_, _ = w.Write(f.body)
	}))
	t.Cleanup(r.srv.Close)
	c, err := cache.Open(context.Background(), r.dir, silentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	parser, err := proxy.New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	fc, err := fetch.New(fetch.Options{ConnectTimeout: 2 * time.Second, TotalTimeout: 5 * time.Second, MaxRetries: 2,
		AllowedHostRegex: []string{`^127\.0\.0\.1$`}, Logger: silentLogger()})
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{Parser: parser, Cache: c, Fetch: fc, HostLimiter: hostsem.New(4),
		Logger: slog.New(slog.NewJSONHandler(r.logs, nil)), Freshness: r.fresh,
		AdoptionEnabled: adoption, MaxSuites: maxSuites})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close) // registered after the cache's cleanup, so it runs first
	r.h, r.c = h, c
	return r
}

func (r *suiteRig) serve(path string, f upFile) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.files[path] = f
}

func (r *suiteRig) get(path string) *httptest.ResponseRecorder {
	r.t.Helper()
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, proxyReq("GET", r.srv.URL, path))
	return rec
}

// suite returns the suite's row, or nil when it has none.
func (r *suiteRig) suite(suitePath string) *cache.SuiteFreshness {
	r.t.Helper()
	s, err := r.c.GetSuiteFreshness(context.Background(), "http", "127.0.0.1", suitePath)
	if errors.Is(err, cache.ErrNotFound) {
		return nil
	}
	if err != nil {
		r.t.Fatal(err)
	}
	return s
}

func (r *suiteRig) marked(base string) bool {
	r.t.Helper()
	_, ok, err := r.c.SignedSeen(context.Background(), "http", "127.0.0.1", base)
	if err != nil {
		r.t.Fatal(err)
	}
	return ok
}

// R-D4: with no adopter, the first-contact miss of <suite>/InRelease
// creates the row today's handler creates — every column, NULL validators
// for a miss without them — and no T1 fires; a refresh miss re-stamps the
// row as today's seed upsert did (ruling PD-6); no other path creates a
// row, and an InRelease that fails the sniff creates none.
func TestServeHTTP_AdoptionDisabledSeedParity(t *testing.T) {
	r := newSuiteRig(t, false, 0)
	const lm = "Mon, 01 Jan 2024 00:00:00 GMT"
	r.serve("/r/dists/s/InRelease", upFile{body: rowInRelease, etag: `"e1"`, lastMod: lm})
	before := wallclock.Unix()
	if rec := r.get("/r/dists/s/InRelease"); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), rowInRelease) {
		t.Fatalf("miss = %d %q", rec.Code, rec.Body.String())
	}
	after := wallclock.Unix()
	s := r.suite("/r/dists/s")
	if s == nil || s.LastRequestedAt == nil {
		t.Fatalf("row = %+v, want the seed", s)
	}
	stamp := *s.LastRequestedAt
	if stamp < before || stamp > after || derefI64(s.LastCheckAt) != stamp || derefI64(s.LastSuccessAt) != stamp {
		t.Fatalf("stamps check=%v success=%v requested=%d, want all the miss's second in [%d, %d]",
			s.LastCheckAt, s.LastSuccessAt, stamp, before, after)
	}
	if derefStr(s.InReleaseETag) != `"e1"` || derefStr(s.InReleaseLastMod) != lm {
		t.Fatalf("validators = %v %v, want the miss's", s.InReleaseETag, s.InReleaseLastMod)
	}
	if s.InReleaseChangeSeenAt != nil || s.CurrentSnapshotID != nil || s.CheckFailCount != 0 {
		t.Fatalf("row = %+v, want no change-seen stamp, no snapshot, no failed checks", s)
	}
	if s.AdoptionFailHash != nil || s.AdoptionFailReason != nil || s.AdoptionFailCount != 0 || s.AdoptionFailFirstAt != nil ||
		s.AdoptionFailLastAt != nil || s.AdoptionNextRetryAt != nil || s.AdoptionFailETag != nil || s.AdoptionFailLastMod != nil {
		t.Fatalf("row = %+v, want an empty failure record", s)
	}
	// A miss without validators seeds NULL validators, as today's seed does.
	r.serve("/r/dists/n/InRelease", upFile{body: rowInRelease})
	if rec := r.get("/r/dists/n/InRelease"); rec.Code != http.StatusOK {
		t.Fatalf("validator-less miss = %d", rec.Code)
	}
	if n := r.suite("/r/dists/n"); n == nil || n.InReleaseETag != nil || n.InReleaseLastMod != nil {
		t.Fatalf("validator-less seed = %+v, want NULL validators", n)
	}
	// Ruling PD-6: a refresh miss (the url_path anchor gone, upstream moved
	// on) re-stamps the row with the second miss's stamps and validators,
	// clears the pending change and the failed checks, and keeps the
	// interest stamp — today's seed upsert, UPDATE-only.
	if _, err := r.secondConn().Exec(`DELETE FROM url_path WHERE path = '/r/dists/s/InRelease';
UPDATE suite_freshness SET last_check_at = 7, last_success_at = 7, inrelease_change_seen_at = 5, check_fail_count = 3
 WHERE suite_path = '/r/dists/s'`); err != nil {
		t.Fatal(err)
	}
	const lm2 = "Tue, 02 Jan 2024 00:00:00 GMT"
	r.serve("/r/dists/s/InRelease", upFile{body: simtest.ClearsignedFixture("Origin: T\nSuite: s\nVersion: 2\n"), etag: `"e2"`, lastMod: lm2})
	before = wallclock.Unix()
	if rec := r.get("/r/dists/s/InRelease"); rec.Code != http.StatusOK {
		t.Fatalf("refresh miss = %d", rec.Code)
	}
	after = wallclock.Unix()
	s = r.suite("/r/dists/s")
	if stamp2 := derefI64(s.LastCheckAt); stamp2 < before || stamp2 > after || derefI64(s.LastSuccessAt) != stamp2 {
		t.Fatalf("after the refresh miss check=%v success=%v, want the second miss's second in [%d, %d]",
			s.LastCheckAt, s.LastSuccessAt, before, after)
	}
	if derefStr(s.InReleaseETag) != `"e2"` || derefStr(s.InReleaseLastMod) != lm2 || s.InReleaseChangeSeenAt != nil ||
		s.CheckFailCount != 0 || s.CurrentSnapshotID != nil {
		t.Fatalf("after the refresh miss row = %+v, want the second miss's validators, no change, no failed checks", s)
	}
	if derefI64(s.LastRequestedAt) != stamp {
		t.Fatalf("last_requested_at = %d, want %d (a refresh miss is no creation)", derefI64(s.LastRequestedAt), stamp)
	}
	// The re-stamp is gated on the sniff (final review, tests Minor 4): a
	// refresh miss answered with an HTML page records no successful check
	// and takes none of the page's validators.
	if _, err := r.secondConn().Exec(`DELETE FROM url_path WHERE path = '/r/dists/s/InRelease'`); err != nil {
		t.Fatal(err)
	}
	r.serve("/r/dists/s/InRelease", upFile{body: rowHTML, etag: `"html"`, lastMod: "Wed, 03 Jan 2024 00:00:00 GMT"})
	if rec := r.get("/r/dists/s/InRelease"); rec.Code != http.StatusOK {
		t.Fatalf("HTML refresh miss = %d", rec.Code)
	}
	if h := r.suite("/r/dists/s"); derefI64(h.LastCheckAt) != derefI64(s.LastCheckAt) || derefI64(h.LastSuccessAt) != derefI64(s.LastSuccessAt) ||
		derefStr(h.InReleaseETag) != `"e2"` || derefStr(h.InReleaseLastMod) != lm2 {
		t.Fatalf("after an HTML refresh miss row = %+v, want it as the second miss left it", h)
	}
	for path, f := range map[string]upFile{
		"/r/dists/t/Release":       {body: rowRelease},
		"/r/dists/u/Release.gpg":   {body: rowGPG},
		"/r/dists/v/InRelease":     {body: rowHTML}, // fails the sniff
		"/r/dists/w/main/Packages": {body: []byte("Package: p\n")},
	} {
		r.serve(path, f)
		if rec := r.get(path); rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", path, rec.Code)
		}
	}
	for _, suite := range []string{"/r/dists/t", "/r/dists/u", "/r/dists/v", "/r/dists/w"} {
		if r.suite(suite) != nil {
			t.Errorf("%s: a row was created", suite)
		}
	}
	r.h.Close() // drains any freshness goroutine
	if calls := r.fresh.snapshot(); len(calls) != 0 {
		t.Fatalf("misses fired T1: %+v", calls)
	}
}

// Design note 1, spec C3: with an adopter, a sniff-passing InRelease or
// Release miss creates a row whose check stamps and validators are NULL
// (its first check runs at once); Release.gpg never creates one.
func TestServeHTTP_AdopterWiredMissesCreateUnstampedRows(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	r.serve("/r/dists/a/InRelease", upFile{body: rowInRelease, etag: `"e1"`})
	r.serve("/r/dists/b/Release", upFile{body: rowRelease, etag: `"e2"`})
	r.serve("/r/dists/c/Release", upFile{body: rowHTML})
	r.serve("/r/dists/d/Release.gpg", upFile{body: rowGPG})
	for _, p := range []string{"/r/dists/a/InRelease", "/r/dists/b/Release", "/r/dists/c/Release", "/r/dists/d/Release.gpg"} {
		if rec := r.get(p); rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", p, rec.Code)
		}
	}
	for _, suite := range []string{"/r/dists/a", "/r/dists/b"} {
		s := r.suite(suite)
		if s == nil || s.LastCheckAt != nil || s.LastSuccessAt != nil || s.InReleaseETag != nil || s.LastRequestedAt == nil {
			t.Fatalf("%s: row = %+v, want created with NULL check stamps and validators", suite, s)
		}
	}
	for _, suite := range []string{"/r/dists/c", "/r/dists/d"} {
		if r.suite(suite) != nil {
			t.Errorf("%s: a row was created (HTML Release, or Release.gpg)", suite)
		}
	}
	if r.marked("/r/dists/b") {
		t.Fatal("an unsigned Release miss wrote a signed-seen marker")
	}
	// Design note 5: with an adopter, a refresh miss leaves the row as it
	// is — here the state its checks wrote.
	if _, err := r.secondConn().Exec(`DELETE FROM url_path WHERE path = '/r/dists/a/InRelease';
UPDATE suite_freshness SET last_check_at = 7, last_success_at = 6, inrelease_etag = 'checked', inrelease_change_seen_at = 5,
       check_fail_count = 2 WHERE suite_path = '/r/dists/a'`); err != nil {
		t.Fatal(err)
	}
	r.serve("/r/dists/a/InRelease", upFile{body: simtest.ClearsignedFixture("Origin: T\nVersion: 2\n"), etag: `"e9"`})
	if rec := r.get("/r/dists/a/InRelease"); rec.Code != http.StatusOK {
		t.Fatalf("refresh miss = %d", rec.Code)
	}
	if s := r.suite("/r/dists/a"); s == nil || derefI64(s.LastCheckAt) != 7 || derefI64(s.LastSuccessAt) != 6 ||
		derefStr(s.InReleaseETag) != "checked" || derefI64(s.InReleaseChangeSeenAt) != 5 || s.CheckFailCount != 2 {
		t.Fatalf("after an adopter-wired refresh miss row = %+v, want it as its checks left it", s)
	}
}

// Ruling PD-5: a miss that creates its suite's row marks, in the creating
// transaction, the signed anchors the suite already has cached — here a
// Release.gpg stored while the suite was untracked — and no plain-text
// one; a cached anchor that changed after it was sniffed makes the
// creation unproven, and the backstop marks it after the commit.
func TestServeHTTP_CreatingMissMarksCachedSignedAnchors(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	r.serve("/r/dists/g/Release.gpg", upFile{body: rowGPG})
	r.get("/r/dists/g/Release.gpg")
	if r.marked("/r/dists/g") {
		t.Fatal("premise: an untracked suite's Release.gpg is unmarked")
	}
	r.serve("/r/dists/g/Release", upFile{body: rowRelease})
	if rec := r.get("/r/dists/g/Release"); rec.Code != http.StatusOK || r.suite("/r/dists/g") == nil || !r.marked("/r/dists/g") {
		t.Fatalf("Release miss = %d; want the row created and its cached Release.gpg marked", rec.Code)
	}
	r.serve("/r/dists/p/InRelease", upFile{body: rowHTML}) // an HTML soft-404, cached
	r.get("/r/dists/p/InRelease")
	r.serve("/r/dists/p/Release", upFile{body: rowRelease})
	if r.get("/r/dists/p/Release"); r.suite("/r/dists/p") == nil || r.marked("/r/dists/p") {
		t.Fatal("p: want the row created, with no marker for its plain-text cached InRelease")
	}
	r.serve("/r/dists/q/Release.gpg", upFile{body: []byte("not a signature\n")})
	r.get("/r/dists/q/Release.gpg")
	r.h.cachedAnchorsHook = func() { // the cached Release.gpg changes after it was sniffed
		r.h.cachedAnchorsHook = nil
		h := writeBlob(t, r.h, rowGPG)
		at := wallclock.Unix()
		if err := r.c.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
			Path: "/r/dists/q/Release.gpg", BlobHash: &h, UpstreamURL: r.srv.URL + "/r/dists/q/Release.gpg", IsMetadata: true,
			LastFetchedAt: &at}); err != nil {
			t.Error(err)
		}
	}
	r.serve("/r/dists/q/Release", upFile{body: rowRelease})
	if r.get("/r/dists/q/Release"); r.suite("/r/dists/q") == nil || !r.marked("/r/dists/q") {
		t.Fatal("q: want the row created and the changed Release.gpg marked by the backstop")
	}
}

// Ruling PD-9: a suite path over 1 KiB is no suite — its anchors are
// served, but create no row and no marker.
func TestServeHTTP_OverlongSuitePathIsServedUntracked(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	suite := "/r/dists/" + strings.Repeat("x", aptpath.MaxSuitePathBytes)
	for _, f := range []struct {
		name string
		body []byte
	}{{"/InRelease", rowInRelease}, {"/Release", rowRelease}, {"/Release.gpg", rowGPG}} {
		r.serve(suite+f.name, upFile{body: f.body})
		if rec := r.get(suite + f.name); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), f.body) {
			t.Fatalf("%s = %d", f.name, rec.Code)
		}
	}
	if r.suite(suite) != nil || r.marked(suite) {
		t.Fatal("an overlong suite path got a row or a marker")
	}
}

// markWatcher records, at the first body byte written to the client,
// whether the suite's row and its base's marker existed.
type markWatcher struct {
	*httptest.ResponseRecorder
	r          *suiteRig
	suite      string
	checked    bool
	row, mark  bool
	checkedErr error
}

func (w *markWatcher) Write(p []byte) (int, error) {
	if !w.checked {
		w.checked = true
		w.row = w.r.suite(w.suite) != nil
		_, w.mark, w.checkedErr = w.r.c.SignedSeen(context.Background(), "http", "127.0.0.1", aptpath.MarkerBase(w.suite))
	}
	return w.ResponseRecorder.Write(p)
}

func (r *suiteRig) watch(path, suite string) *markWatcher {
	r.t.Helper()
	w := &markWatcher{ResponseRecorder: httptest.NewRecorder(), r: r, suite: suite}
	r.h.ServeHTTP(w, proxyReq("GET", r.srv.URL, path))
	if w.Code != http.StatusOK || !w.checked || w.checkedErr != nil {
		r.t.Fatalf("%s: status %d, body written %v, read error %v", path, w.Code, w.checked, w.checkedErr)
	}
	return w
}

// Spec C3 miss-path ordering: the signed-seen marker (and, on first
// contact, the suite row) exists before the first body byte reaches the
// client — on first contact, on a refresh miss, and for a tracked suite's
// Release.gpg.
func TestServeHTTP_MarkerPrecedesTheFirstBodyByte(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	r.serve("/r/dists/s/InRelease", upFile{body: rowInRelease})
	if w := r.watch("/r/dists/s/InRelease", "/r/dists/s"); !w.row || !w.mark {
		t.Fatalf("first contact: at the first body byte row=%v marker=%v, want both", w.row, w.mark)
	}
	// A refresh miss: the row exists, its url_path row and marker are gone.
	db := r.secondConn()
	if _, err := db.Exec(`DELETE FROM url_path WHERE path = '/r/dists/s/InRelease'; DELETE FROM suite_signed_seen`); err != nil {
		t.Fatal(err)
	}
	if w := r.watch("/r/dists/s/InRelease", "/r/dists/s"); !w.mark {
		t.Fatal("refresh miss: no marker at the first body byte")
	}
	r.serve("/r/dists/s/Release.gpg", upFile{body: rowGPG})
	if _, err := db.Exec(`DELETE FROM suite_signed_seen`); err != nil {
		t.Fatal(err)
	}
	if w := r.watch("/r/dists/s/Release.gpg", "/r/dists/s"); !w.mark {
		t.Fatal("Release.gpg of a tracked suite: no marker at the first body byte")
	}
}

// secondConn opens cache.db on a second connection (the test injects
// failures and rewinds state the cache's API does not expose).
func (r *suiteRig) secondConn() *sql.DB {
	r.t.Helper()
	return sqliteFile(r.t, filepath.Join(r.dir, "cache.db"))
}

// Spec C3: a failed writer operation serves no body — 502, as a url_path
// write failure always has — and stores nothing.
func TestServeHTTP_FailedMissWriteServesNoBody(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	r.serve("/r/dists/s/InRelease", upFile{body: rowInRelease})
	if _, err := r.secondConn().Exec(`CREATE TRIGGER fail_mark BEFORE INSERT ON suite_signed_seen
BEGIN SELECT RAISE(ABORT, 'injected marker failure'); END`); err != nil {
		t.Fatal(err)
	}
	rec := r.get("/r/dists/s/InRelease")
	if rec.Code != http.StatusBadGateway || bytes.Contains(rec.Body.Bytes(), []byte("Origin: T")) {
		t.Fatalf("status %d body %q; want 502 without the anchor", rec.Code, rec.Body.String())
	}
	if r.suite("/r/dists/s") != nil {
		t.Fatal("the failed operation created the suite row")
	}
	if _, err := r.c.LookupURL(context.Background(), "http", "127.0.0.1", "/r/dists/s/InRelease"); !errors.Is(err, cache.ErrNotFound) {
		t.Fatalf("the failed operation stored the url_path row (%v)", err)
	}
}

// unreadableBlob makes body's pool file unreadable — a directory where the
// file belongs, so opening it succeeds and reading it fails (EISDIR), as a
// disk fault would; a miss of the same bytes keeps it (Finalize dedups) —
// and returns a func that puts the file back.
func (r *suiteRig) unreadableBlob(body []byte) (restore func()) {
	r.t.Helper()
	p := r.c.BlobPath(sha256Hex(body))
	aside, had := p+".aside", false
	if _, err := os.Stat(p); err == nil {
		if err := os.Rename(p, aside); err != nil {
			r.t.Fatal(err)
		}
		had = true
	}
	if err := os.MkdirAll(p, 0o750); err != nil {
		r.t.Fatal(err)
	}
	return func() {
		if err := os.Remove(p); err != nil {
			r.t.Error(err)
		}
		if had {
			if err := os.Rename(aside, p); err != nil {
				r.t.Error(err)
			}
		}
	}
}

// warnPaths lists the path and suite_path of each WARN record msg, failing
// on a record without err or with another canonical_host.
func (r *suiteRig) warnPaths(msg string) []string {
	r.t.Helper()
	var out []string
	for _, rec := range logRecords(r.logs, msg, slog.LevelWarn) {
		if rec["err"] == nil || rec["canonical_host"] != "127.0.0.1" {
			r.t.Errorf("%s record %v: want err and canonical_host", msg, rec)
		}
		out = append(out, fmt.Sprint(rec["path"], " ", rec["suite_path"]))
	}
	return out
}

// Task 9 review (ruling a): a fetched suite file whose pool file cannot be
// read back for its sniff is logged WARN. A signed anchor's miss then fails
// like a failed writer operation — 502, no body, nothing stored — since its
// sniff decides its marker; any other suite file is stored and served, and
// creates no row from a body nobody sniffed.
func TestServeHTTP_UnreadableFetchedSuiteFile(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	r.serve("/r/dists/s/InRelease", upFile{body: rowInRelease})
	r.unreadableBlob(rowInRelease)
	rec := r.get("/r/dists/s/InRelease")
	if rec.Code != http.StatusBadGateway || bytes.Contains(rec.Body.Bytes(), []byte("Origin: T")) {
		t.Fatalf("InRelease: status %d body %q; want 502 without the anchor", rec.Code, rec.Body.String())
	}
	if r.suite("/r/dists/s") != nil || r.marked("/r/dists/s") {
		t.Fatal("InRelease: an unread body created a row or a marker")
	}
	if _, err := r.c.LookupURL(context.Background(), "http", "127.0.0.1", "/r/dists/s/InRelease"); !errors.Is(err, cache.ErrNotFound) {
		t.Fatalf("InRelease: the failed miss stored the url_path row (%v)", err)
	}
	r.serve("/r/dists/t/Release", upFile{body: rowRelease})
	r.unreadableBlob(rowRelease)
	r.get("/r/dists/t/Release") // the serve reads the directory too: its status is the injection's
	if r.suite("/r/dists/t") != nil {
		t.Fatal("Release: an unread body created a row")
	}
	if _, err := r.c.LookupURL(context.Background(), "http", "127.0.0.1", "/r/dists/t/Release"); err != nil {
		t.Fatalf("Release: the url_path row was not stored (%v)", err)
	}
	want := []string{"/r/dists/s/InRelease /r/dists/s", "/r/dists/t/Release /r/dists/t"}
	if got := r.warnPaths("suite file head read failed"); !slices.Equal(got, want) {
		t.Fatalf("suite file head read failed for %q, want %q", got, want)
	}
}

// Task 9 review (rulings b, c): a creation that cannot read one of its
// suite's cached signed anchors logs WARN and is unproven — never "unsigned,
// hash unchanged" — so the backstop re-reads the anchor after the commit and
// marks it; a backstop read that fails is logged WARN too (a missing row is
// not). For g the cached Release.gpg is unreadable while the Release miss
// reads it and readable again for the backstop; for h it stays unreadable.
func TestServeHTTP_UnreadableCachedAnchorMakesTheCreationUnproven(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	r.serve("/r/dists/g/Release.gpg", upFile{body: rowGPG})
	if rec := r.get("/r/dists/g/Release.gpg"); rec.Code != http.StatusOK || r.suite("/r/dists/g") != nil {
		t.Fatalf("premise: Release.gpg = %d, cached for an untracked suite", rec.Code)
	}
	restore := r.unreadableBlob(rowGPG)
	r.h.cachedAnchorsHook = func() { // right after cachedAnchors' read, before the creating transaction
		r.h.cachedAnchorsHook = nil
		restore()
	}
	r.serve("/r/dists/g/Release", upFile{body: rowRelease})
	if rec := r.get("/r/dists/g/Release"); rec.Code != http.StatusOK || r.suite("/r/dists/g") == nil {
		t.Fatalf("g: Release miss = %d; want the row created", rec.Code)
	}
	if !r.marked("/r/dists/g") {
		t.Fatal("g: the cached Release.gpg the creation could not read is unmarked; want the backstop to mark it")
	}
	r.serve("/r/dists/h/Release.gpg", upFile{body: rowGPG})
	if rec := r.get("/r/dists/h/Release.gpg"); rec.Code != http.StatusOK || r.suite("/r/dists/h") != nil {
		t.Fatalf("premise: h's Release.gpg = %d, cached for an untracked suite", rec.Code)
	}
	defer r.unreadableBlob(rowGPG)()
	r.serve("/r/dists/h/Release", upFile{body: rowRelease})
	if rec := r.get("/r/dists/h/Release"); rec.Code != http.StatusOK || r.suite("/r/dists/h") == nil {
		t.Fatalf("h: Release miss = %d; want the row created", rec.Code)
	}
	if r.marked("/r/dists/h") {
		t.Fatal("h: marked from a Release.gpg nobody could read")
	}
	want := []string{"/r/dists/g/Release.gpg /r/dists/g", "/r/dists/h/Release.gpg /r/dists/h"}
	if got := r.warnPaths("suite cached anchor read failed"); !slices.Equal(got, want) {
		t.Fatalf("suite cached anchor read failed for %q, want %q", got, want)
	}
	want = []string{"/r/dists/h/Release.gpg /r/dists/h"} // no line for the absent InRelease rows
	if got := r.warnPaths("suite signed-seen backstop read failed"); !slices.Equal(got, want) {
		t.Fatalf("suite signed-seen backstop read failed for %q, want %q", got, want)
	}
}

// Spec C3: markers only for suites with a row — an untracked suite's
// Release.gpg, and a suite refused at a cap, get none (the url_path rows
// are stored).
func TestServeHTTP_MarkersOnlyForTrackedSuites(t *testing.T) {
	r := newSuiteRig(t, true, 10) // per-host cap 1
	r.serve("/r/dists/a/InRelease", upFile{body: rowInRelease})
	r.serve("/r/dists/b/InRelease", upFile{body: rowInRelease})
	r.serve("/r/dists/c/Release.gpg", upFile{body: rowGPG})
	for _, p := range []string{"/r/dists/a/InRelease", "/r/dists/b/InRelease", "/r/dists/c/Release.gpg"} {
		if rec := r.get(p); rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", p, rec.Code)
		}
	}
	if !r.marked("/r/dists/a") || r.marked("/r/dists/b") || r.marked("/r/dists/c") {
		t.Fatalf("markers a=%v b=%v c=%v; want only the tracked suite a", r.marked("/r/dists/a"), r.marked("/r/dists/b"), r.marked("/r/dists/c"))
	}
	if r.suite("/r/dists/b") != nil || r.suite("/r/dists/c") != nil {
		t.Fatal("a row beyond the per-host cap, or for a Release.gpg")
	}
}

// Spec C3 / R-D5: a refusal at a cap is cached for 10 minutes (the next
// request costs no writer operation), counted once
// (acu_suite_seed_rejected_total), shown by the gauge
// (acu_suites_seed_rejected) and logged WARN suite_seed_rejected.
func TestServeHTTP_SuiteCapRejectionIsCachedCountedAndLogged(t *testing.T) {
	r := newSuiteRig(t, true, 10) // per-host cap 1
	r.serve("/r/dists/a/InRelease", upFile{body: rowInRelease})
	r.serve("/r/dists/b/InRelease", upFile{body: rowInRelease})
	r.get("/r/dists/a/InRelease")
	before := sumMetricLines(t, []string{"acu_suite_seed_rejected_total"})["acu_suite_seed_rejected_total"]
	r.get("/r/dists/b/InRelease")
	db := r.secondConn()
	if _, err := db.Exec(`DELETE FROM url_path WHERE path = '/r/dists/b/InRelease'`); err != nil {
		t.Fatal(err)
	}
	r.get("/r/dists/b/InRelease") // a second miss within the window: the cache answers
	r.get("/r/dists/b/InRelease") // a url_path hit within the window: onboarding asks the cache too
	got := sumMetricLines(t, []string{"acu_suite_seed_rejected_total", "acu_suites_seed_rejected"})
	if d := got["acu_suite_seed_rejected_total"] - before; d != 1 {
		t.Fatalf("acu_suite_seed_rejected_total rose by %v, want 1 (the repeat is cached)", d)
	}
	if got["acu_suites_seed_rejected"] != 1 || r.h.rejections.len() != 1 {
		t.Fatalf("gauge %v, cache %d; want 1 and 1", got["acu_suites_seed_rejected"], r.h.rejections.len())
	}
	recs := logRecords(r.logs, "suite_seed_rejected", slog.LevelWarn)
	if len(recs) != 1 || recs[0]["suite_path"] != "/r/dists/b" || recs[0]["cap"] != "host_cap" ||
		recs[0]["max_suites"] != float64(10) || recs[0]["detail"] != "suite will not refresh" {
		t.Fatalf("suite_seed_rejected records = %+v", recs)
	}
}

// The rejection cache holds a refusal for exactly 10 minutes (monotonic
// time) and stays bounded: full of live entries, it clears itself.
func TestSeedRejections_TTLAndBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var s seedRejections
		k := cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/s"}
		s.add(k, seedRejectTTL)
		time.Sleep(seedRejectTTL - time.Second)
		if !s.rejected(k) {
			t.Fatal("expired before 10 minutes")
		}
		time.Sleep(time.Second)
		if s.rejected(k) || s.len() != 0 {
			t.Fatalf("still cached after 10 minutes (%d entries)", s.len())
		}
		for i := range seedRejectMax {
			s.add(cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/" + strings.Repeat("x", i+1)}, seedRejectTTL)
		}
		s.add(k, seedRejectTTL)
		if n := s.len(); n != 1 {
			t.Fatalf("after overflowing with live entries: %d entries, want 1 (cleared, then the new one)", n)
		}
		// An expired entry is swept when another suite is asked about (the
		// sweep timer is stopped, so only that sweep can drop it).
		s.close()
		time.Sleep(seedRejectTTL + seedRejectSweep)
		other := cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/other"}
		if s.rejected(other) || s.len() != 0 {
			t.Fatalf("after the TTL: %d entries, want 0 (swept)", s.len())
		}
	})
}

// Design review M10: without traffic, the cache's sweep timer expires its
// refusals and keeps acu_suites_seed_rejected current; Close stops it.
func TestSeedRejections_ExpireWithoutTraffic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var s seedRejections
		s.add(cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/s"}, seedRejectTTL)
		if got := sumMetricLines(t, []string{"acu_suites_seed_rejected"})["acu_suites_seed_rejected"]; got != 1 {
			t.Fatalf("gauge = %v, want 1", got)
		}
		time.Sleep(seedRejectTTL + seedRejectSweep)
		synctest.Wait()
		if s.len() != 0 {
			t.Fatalf("%d entries after the TTL with no traffic, want 0", s.len())
		}
		if got := sumMetricLines(t, []string{"acu_suites_seed_rejected"})["acu_suites_seed_rejected"]; got != 0 {
			t.Fatalf("gauge = %v after the TTL with no traffic, want 0", got)
		}
		s.add(cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/t"}, seedRejectTTL)
		s.close()
		time.Sleep(seedRejectTTL + seedRejectSweep)
		synctest.Wait()
		if s.len() != 1 {
			t.Fatalf("%d entries; want the closed cache's timer stopped (1 entry left)", s.len())
		}
	})
}

// A creation that found its row already there is no refusal: nothing is
// cached, counted or logged.
func TestNoteCreate_AnExistingRowIsNoRefusal(t *testing.T) {
	logs := &syncLog{}
	h := &Handler{logger: slog.New(slog.NewJSONHandler(logs, nil)), maxSuites: 10}
	before := sumMetricLines(t, []string{"acu_suite_seed_rejected_total"})["acu_suite_seed_rejected_total"]
	h.noteCreate(cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/s"}, cache.CreateResult{Outcome: cache.SuiteExists})
	if h.rejections.len() != 0 || len(logRecords(logs, "", slog.LevelDebug)) != 0 ||
		sumMetricLines(t, []string{"acu_suite_seed_rejected_total"})["acu_suite_seed_rejected_total"] != before {
		t.Fatal("an existing row was treated as a refusal")
	}
}

// R-D5: suite_seed_rejected is rate-limited to one line per minute
// process-wide; the next line carries how many were suppressed.
func TestNoteCreate_WarnIsRateLimited(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs := &syncLog{}
		h := &Handler{logger: slog.New(slog.NewJSONHandler(logs, nil)), maxSuites: 10}
		refuse := func(suite string) {
			h.noteCreate(cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: suite},
				cache.CreateResult{Outcome: cache.SuiteRejectedGlobal})
		}
		refuse("/a")
		refuse("/b")
		refuse("/c")
		time.Sleep(seedWarnEvery)
		refuse("/d")
		recs := logRecords(logs, "suite_seed_rejected", slog.LevelWarn)
		if len(recs) != 2 || recs[0]["suppressed"] != float64(0) || recs[1]["suppressed"] != float64(2) ||
			recs[1]["suite_path"] != "/d" || recs[1]["cap"] != "global_cap" {
			t.Fatalf("records = %+v; want /a (suppressed 0) and /d (suppressed 2)", recs)
		}
		// The count restarts with each line (final review, tests Minor 1):
		// the third window's line carries only its own window's one.
		refuse("/e")
		time.Sleep(seedWarnEvery)
		refuse("/f")
		recs = logRecords(logs, "suite_seed_rejected", slog.LevelWarn)
		if len(recs) != 3 || recs[2]["suite_path"] != "/f" || recs[2]["suppressed"] != float64(1) {
			t.Fatalf("records = %+v; want a third line, /f, with suppressed 1 (/e alone)", recs)
		}
	})
}

// Design note 24 (final review, tests Minor 1; Task 9 minor 2):
// Handler.Close stops the rejection cache's sweep timer, so a closed
// handler's refusals stay where they are instead of a timer firing after
// shutdown.
func TestHandlerClose_StopsTheRejectionSweep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &Handler{logger: silentLogger(), maxSuites: 10, lifecycleCancel: func() {}}
		h.noteCreate(cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/s"},
			cache.CreateResult{Outcome: cache.SuiteRejectedGlobal})
		h.Close()
		time.Sleep(seedRejectTTL + seedRejectSweep)
		synctest.Wait()
		if n := h.rejections.len(); n != 1 {
			t.Fatalf("%d entries after Close and the TTL; want 1 (Close stopped the sweep timer)", n)
		}
	})
}

// Design note 16 (final review, tests Minor 1; Task 9 minor 2): New wires
// the rejection sweep's panic report to ERROR handler_goroutine_panicked
// with where = seed_rejections.
func TestNew_WiresTheRejectionSweepsPanicReport(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	if r.h.rejections.onPanic == nil {
		t.Fatal("New left the rejection sweep's onPanic unwired: a recovered panic would go unreported")
	}
	r.h.rejections.onPanic("boom")
	recs := logRecords(r.logs, "handler_goroutine_panicked", slog.LevelError)
	if len(recs) != 1 || recs[0]["where"] != "seed_rejections" || recs[0]["panic"] != "boom" {
		t.Fatalf("handler_goroutine_panicked records = %v; want one with where seed_rejections and the panic", recs)
	}
}

// Final review, tests Minor 1 (Task 9 minor 2): a backstop whose mark
// fails is logged WARN suite signed-seen mark failed — the operator's
// only sign that a tracked suite's signed anchor waits for the startup
// pass — and the response is served.
func TestServeHTTP_BackstopMarkFailureIsAWarning(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	r.serve("/r/dists/g/Release.gpg", upFile{body: []byte("not a signature\n")})
	r.get("/r/dists/g/Release.gpg")
	r.h.cachedAnchorsHook = func() { // the cached Release.gpg becomes a signature after the sniff
		r.h.cachedAnchorsHook = nil
		r.cacheAnchor("/r/dists/g/Release.gpg", rowGPG, 1_700_000_000)
	}
	if _, err := r.secondConn().Exec(`CREATE TRIGGER fail_mark BEFORE INSERT ON suite_signed_seen
BEGIN SELECT RAISE(ABORT, 'injected marker failure'); END`); err != nil {
		t.Fatal(err)
	}
	r.serve("/r/dists/g/Release", upFile{body: rowRelease})
	if rec := r.get("/r/dists/g/Release"); rec.Code != http.StatusOK || r.suite("/r/dists/g") == nil {
		t.Fatalf("Release miss = %d; want 200 and the row created", rec.Code)
	}
	recs := logRecords(r.logs, "suite signed-seen mark failed", slog.LevelDebug)
	if len(recs) != 1 || recs[0]["level"] != "WARN" || recs[0]["err"] == nil || recs[0]["suite_path"] != "/r/dists/g" {
		t.Fatalf("suite signed-seen mark failed records = %v; want one WARN with err and suite_path", recs)
	}
}

// Final review, concurrency M2 (ruling FW-5 2): a global-cap refusal
// while every eviction candidate was busy is transient — cached for a
// minute, not ten, and its suite_seed_rejected line (under the same
// once-a-minute limit) says so instead of "suite will not refresh". A
// lasting refusal is still cached for ten minutes.
func TestNoteCreate_ABusyRefusalIsCachedForAMinute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs := &syncLog{}
		h := &Handler{logger: slog.New(slog.NewJSONHandler(logs, nil)), maxSuites: 10}
		defer h.rejections.close()
		lasting := cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/full"}
		busy := cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/busy"}
		h.noteCreate(lasting, cache.CreateResult{Outcome: cache.SuiteRejectedGlobal})
		time.Sleep(seedWarnEvery) // the busy refusal gets its own line
		h.noteCreate(busy, cache.CreateResult{Outcome: cache.SuiteRejectedGlobal, CandidatesBusy: true})
		time.Sleep(seedRejectBusyTTL - time.Second)
		if !h.rejections.rejected(busy) {
			t.Fatal("a busy refusal expired before a minute")
		}
		time.Sleep(time.Second)
		if h.rejections.rejected(busy) {
			t.Fatal("a busy refusal is still cached after a minute; want it retried")
		}
		if !h.rejections.rejected(lasting) {
			t.Fatal("a lasting refusal expired after two minutes; want ten")
		}
		recs := logRecords(logs, "suite_seed_rejected", slog.LevelWarn)
		if len(recs) != 2 || recs[0]["detail"] != "suite will not refresh" || recs[1]["suite_path"] != "/busy" ||
			recs[1]["detail"] != "eviction candidates busy; retried after 1 minute" {
			t.Fatalf("suite_seed_rejected records = %+v; want the lasting refusal's usual detail and the busy one's", recs)
		}
	})
}

// staticUpstream serves fixed bodies by URL to the handler (a flip-stack
// handler's own upstream answers 404).
type staticUpstream map[string][]byte

func (u staticUpstream) HostAllowed(string) bool { return true }

func (u staticUpstream) Fetch(_ context.Context, target *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error) {
	body, ok := u[target.URL]
	if !ok {
		return nil, &fetch.StatusError{Code: 404}
	}
	if _, err := dst.Write(body); err != nil {
		return nil, err
	}
	return &fetch.FetchResult{Status: 200, ContentLength: int64(len(body))}, nil
}

// Phase C carry-forward: the serve that creates a suite's row registers
// with the flip barrier right after the creation, like any tracked
// anchor serve — a flip right after it waits out the quiet period.
func TestServeHTTP_RowCreatingServeRegistersWithTheBarrier(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newFlipStack(t, 10*time.Second)
		const suite = "/n/dists/s"
		s.h.fetch = staticUpstream{"http://" + flipSimHost + suite + "/InRelease": rowInRelease}
		if _, code := s.get(suite+"/InRelease", 0); code != http.StatusOK {
			t.Fatalf("first contact = %d", code)
		}
		if n := s.gate.Len(); n != 1 {
			t.Fatalf("gate entries after the row-creating serve = %d, want 1", n)
		}
		start := time.Now()
		if _, err := s.gate.Flip(context.Background(), flipgate.Key("http", flipSimHost, suite), func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		if got := time.Since(start); got != 10*time.Second {
			t.Fatalf("a flip right after the serve waited %v, want the 10s quiet period", got)
		}
	})
}

// holdUntilTracked starts a flip of suite (idle: its hold starts at once)
// whose hold lasts until the suite has a row — at most a minute of the
// bubble's time, so a test whose request never creates it fails instead
// of hanging — plus linger, and then runs commit; it returns once the hold
// is in place, with a channel closed when the flip is done.
func (s *flipStack) holdUntilTracked(suite string, linger time.Duration, commit func() error) chan struct{} {
	s.t.Helper()
	ctx := context.Background()
	key := cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: flipSimHost, SuitePath: suite}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := s.gate.Flip(ctx, flipgate.Key("http", flipSimHost, suite), func() error {
			for start := time.Now(); time.Since(start) < time.Minute; time.Sleep(10 * time.Millisecond) {
				if ok, err := s.c.SuiteExists(ctx, key); err != nil || ok {
					break
				}
			}
			time.Sleep(linger)
			return commit()
		})
		if err != nil {
			s.t.Error(err)
		}
	}()
	synctest.Wait()
	return done
}

// Design note 9: a row-creating serve whose late registration waited in a
// flip's hold is answered from what is current after the flip — here the
// snapshot the flip committed — not with the bytes its miss fetched.
func TestServeHTTP_LateRegistrationThatWaitedServesTheNewSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs := &syncLog{}
		s := newFlipStackLogging(t, 10*time.Second, slog.New(slog.NewJSONHandler(logs, nil)))
		const suite = "/n/dists/s"
		s.h.fetch = staticUpstream{"http://" + flipSimHost + suite + "/InRelease": rowInRelease}
		snapBody := simtest.ClearsignedFixture("Origin: snapshot\n")
		hash := writeBlob(t, s.h, snapBody)
		ctx := context.Background()
		cand, _, err := s.c.InsertCandidateSnapshot(ctx, cache.SnapshotCandidate{CanonicalScheme: "http",
			CanonicalHost: flipSimHost, SuitePath: suite, InReleaseHash: &hash})
		if err != nil {
			t.Fatal(err)
		}
		// The commit waits for the row the client's miss creates.
		flipped := s.holdUntilTracked(suite, 0, func() error {
			return s.c.CommitAdoption(ctx, cand, []cache.SnapshotMember{{SnapshotID: cand, Path: "InRelease",
				BlobHash: hash, DeclaredSHA256: hash}}, nil, nil, nil, false)
		})
		rec := httptest.NewRecorder()
		s.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+flipSimHost+suite+"/InRelease", nil))
		<-flipped
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), snapBody) {
			t.Fatalf("status %d body %q; want the committed snapshot's InRelease", rec.Code, rec.Body.String())
		}
		// Design review M1: the line keeps the miss's outcome and upstream
		// status, and says the request waited.
		lines := logRecords(logs, "request", slog.LevelInfo)
		if len(lines) != 1 || lines[0]["outcome"] != "miss" || lines[0]["upstream_status"] != float64(200) || lines[0]["flip_held_ms"] == nil {
			t.Fatalf("request lines %v, want one miss line with upstream_status 200 and flip_held_ms", lines)
		}
	})
}

// Final fix wave FW-4 (c) (final tests review, Important 3): a late
// registration — a creating miss's, an onboarding hit's — re-dispatches
// through the snapshot path whatever its wait in a hold read. Here a flip
// commits right before the registration (no serve of the suite was
// registered, so it held nothing) and the registration's Held() is 0, as
// for a hold released at the clock reading the request joined it (a
// coarse clocksource, or one instant in a bubble: the cause of the
// OnboardingWaiterRegistersItsOwnServe flake). Inferring "held" from
// Held() > 0 served the bytes read or fetched before the commit: a C8
// straddle.
func TestServeHTTP_LateRegistrationAfterACommitServesTheNewSnapshot(t *testing.T) {
	for _, path := range []string{"creating miss", "onboarding hit"} {
		t.Run(path, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newFlipStack(t, 10*time.Second)
				const suite = "/n/dists/s"
				if path == "creating miss" {
					s.h.fetch = staticUpstream{"http://" + flipSimHost + suite + "/InRelease": rowInRelease}
				} else {
					s.cacheRow(suite+"/InRelease", rowInRelease, wallclock.Unix()) // a legacy anchor: the hit onboards
				}
				snapBody := simtest.ClearsignedFixture("Origin: snapshot\n")
				hash := writeBlob(t, s.h, snapBody)
				ctx := context.Background()
				cand, _, err := s.c.InsertCandidateSnapshot(ctx, cache.SnapshotCandidate{CanonicalScheme: "http",
					CanonicalHost: flipSimHost, SuitePath: suite, InReleaseHash: &hash})
				if err != nil {
					t.Fatal(err)
				}
				var committed sync.Once
				flipped := false
				// The registration's read: once the suite has its row, a flip
				// commits the snapshot before the read answers.
				s.h.suiteExistsForTest = func(key cache.SuiteKey) (bool, error) {
					ok, err := s.c.SuiteExists(ctx, key)
					if err == nil && ok {
						committed.Do(func() {
							_, ferr := s.gate.Flip(ctx, flipgate.Key("http", flipSimHost, suite), func() error {
								return s.c.CommitAdoption(ctx, cand, []cache.SnapshotMember{{SnapshotID: cand, Path: "InRelease",
									BlobHash: hash, DeclaredSHA256: hash}}, nil, nil, nil, false)
							})
							if ferr != nil {
								t.Error(ferr)
							}
							flipped = true
						})
					}
					return ok, err
				}
				rec := httptest.NewRecorder()
				s.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+flipSimHost+suite+"/InRelease", nil))
				if !flipped {
					t.Fatal("setup: the flip never ran before the registration")
				}
				if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), snapBody) {
					t.Fatalf("status %d body %q; want the snapshot committed before the registration", rec.Code, rec.Body.String())
				}
			})
		})
	}
}

// Design review M1: a row-creating serve whose client leaves during its
// late registration's wait is a client abort, as on the entry path — 503,
// no WARN — and its line keeps the miss's upstream status.
func TestServeHTTP_LateRegistrationClientThatLeftIsAClientAbort(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs := &syncLog{}
		s := newFlipStackLogging(t, 10*time.Second, slog.New(slog.NewJSONHandler(logs, nil)))
		const suite = "/n/dists/s"
		s.h.fetch = staticUpstream{"http://" + flipSimHost + suite + "/InRelease": rowInRelease}
		// The hold ends two seconds after the client's miss created the row.
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
			t.Errorf("a client that left during the late wait logged %d WARN or worse, first %v", len(warns), warns[0])
		}
		lines := logRecords(logs, "request", slog.LevelInfo)
		if len(lines) != 1 || lines[0]["outcome"] != "client_canceled" || lines[0]["upstream_status"] != float64(200) ||
			lines[0]["flip_held_ms"] == nil {
			t.Fatalf("request lines %v, want one client_canceled line with upstream_status 200 and flip_held_ms", lines)
		}
	})
}

// cacheRow stores body as the url_path row of path on the flip stack's
// host (no suite row, no marker), requested and fetched at at.
func (s *flipStack) cacheRow(path string, body []byte, at int64) {
	s.t.Helper()
	hash := writeBlob(s.t, s.h, body)
	if err := s.c.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: "http", CanonicalHost: flipSimHost,
		Path: path, BlobHash: &hash, UpstreamURL: "http://" + flipSimHost + path, IsMetadata: true,
		LastRequestedAt: &at, RequestCount: 1, LastFetchedAt: &at}); err != nil {
		s.t.Fatal(err)
	}
}

// signsAfterTheSniff makes the next creation on suite unproven: the
// suite's cached Release.gpg is no signature when the creation sniffs it
// and becomes one, stamped at, right after (cachedAnchorsHook), so only
// the backstop can mark it.
func (s *flipStack) signsAfterTheSniff(suite string, at int64) {
	s.t.Helper()
	s.cacheRow(suite+"/Release.gpg", []byte("not a signature\n"), at)
	s.h.cachedAnchorsHook = func() {
		s.h.cachedAnchorsHook = nil
		s.cacheRow(suite+"/Release.gpg", rowGPG, at)
	}
}

// marked reports whether suite's base on the flip stack's host has a
// signed-seen marker.
func (s *flipStack) marked(suite string) bool {
	s.t.Helper()
	_, ok, err := s.c.SignedSeen(context.Background(), "http", flipSimHost, aptpath.MarkerBase(suite))
	if err != nil {
		s.t.Fatal(err)
	}
	return ok
}

// FW-4 (final review, concurrency M5; Task 9 minor 1): the miss path runs
// an unproven creation's backstop right after its late registration,
// before the client-left and held re-dispatch returns, so neither return
// leaves the changed cached anchor unmarked. The InRelease miss marks the
// base itself, at the miss's second; the suite's cached Release.gpg,
// which became a signature after it was sniffed, carries a later stamp,
// and a marker keeps the newest — so the marker's stamp shows whether the
// backstop ran.
func TestServeHTTP_LateRegistrationReturnsRunTheBackstop(t *testing.T) {
	for _, tc := range []struct {
		name     string
		linger   time.Duration
		leaveAt  time.Duration
		snapshot bool // the flip commits a snapshot, which the re-dispatch serves
		wantCode int
	}{
		{"client left during the wait", 2 * time.Second, time.Second, false, http.StatusServiceUnavailable},
		{"served from the new snapshot", 0, 0, true, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				logs := &syncLog{}
				s := newFlipStackLogging(t, 10*time.Second, slog.New(slog.NewJSONHandler(logs, nil)))
				const suite = "/n/dists/s"
				ctx := context.Background()
				s.h.fetch = staticUpstream{"http://" + flipSimHost + suite + "/InRelease": rowInRelease}
				gpgAt := wallclock.Unix() + 1000
				s.signsAfterTheSniff(suite, gpgAt)
				commit := func() error { return nil }
				snapBody := simtest.ClearsignedFixture("Origin: snapshot\n")
				if tc.snapshot {
					hash := writeBlob(t, s.h, snapBody)
					cand, _, err := s.c.InsertCandidateSnapshot(ctx, cache.SnapshotCandidate{CanonicalScheme: "http",
						CanonicalHost: flipSimHost, SuitePath: suite, InReleaseHash: &hash})
					if err != nil {
						t.Fatal(err)
					}
					commit = func() error {
						return s.c.CommitAdoption(ctx, cand, []cache.SnapshotMember{{SnapshotID: cand, Path: "InRelease",
							BlobHash: hash, DeclaredSHA256: hash}}, nil, nil, nil, false)
					}
				}
				flipped := s.holdUntilTracked(suite, tc.linger, commit)
				rctx, cancel := context.WithCancel(ctx)
				defer cancel()
				if tc.leaveAt > 0 {
					time.AfterFunc(tc.leaveAt, cancel)
				}
				rec := httptest.NewRecorder()
				s.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+flipSimHost+suite+"/InRelease", nil).WithContext(rctx))
				<-flipped
				if rec.Code != tc.wantCode || (tc.snapshot && !bytes.Equal(rec.Body.Bytes(), snapBody)) {
					t.Errorf("%s: status %d body %q, want %d (and the snapshot's InRelease)", tc.name, rec.Code, rec.Body.String(), tc.wantCode)
				}
				// Premise: the late registration waited in the hold (the path
				// with the early returns).
				if lines := logRecords(logs, "request", slog.LevelInfo); len(lines) != 1 || lines[0]["flip_held_ms"] == nil {
					t.Errorf("%s: request lines %v, want one with flip_held_ms", tc.name, lines)
				}
				if at, ok, err := s.c.SignedSeen(ctx, "http", flipSimHost, suite); err != nil || !ok || at != gpgAt {
					t.Errorf("%s: marker = %d %v %v, want the changed Release.gpg's %d: the backstop must run before this return",
						tc.name, at, ok, err, gpgAt)
				}
			})
		})
	}
}

// FW-4 (final review, tests' challenge to the Task 9 ruling d fallback):
// the backstop that runs before the body is bounded at backstopTimeout,
// well under apt's 30 s inactivity timeout, on the miss path and after
// onboarding. backstopHook waits on the backstop's own context: a pool or
// writer that never answers. The body is served once the bound passes,
// the backstop's next read fails (WARN) and the anchor stays unmarked
// for the startup pass.
func TestServeHTTP_BackstopIsBoundedBeforeTheBody(t *testing.T) {
	for _, hit := range []bool{false, true} {
		t.Run(map[bool]string{false: "miss", true: "onboarding"}[hit], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				logs := &syncLog{}
				s := newFlipStackLogging(t, 0, slog.New(slog.NewJSONHandler(logs, nil)))
				const suite = "/n/dists/s"
				s.h.adoptionEnabled = true // a Release creates the row; it marks nothing itself
				if hit {
					s.cacheRow(suite+"/Release", rowRelease, wallclock.Unix()) // onboarding: a url_path hit on a legacy anchor
				} else {
					s.h.fetch = staticUpstream{"http://" + flipSimHost + suite + "/Release": rowRelease}
				}
				s.signsAfterTheSniff(suite, wallclock.Unix())
				ran := false
				s.h.backstopHook = func(ctx context.Context) {
					ran = true
					select {
					case <-ctx.Done():
					case <-time.After(time.Hour): // the bound is gone: the test then fails on the elapsed time
					}
				}
				start := time.Now()
				rec := httptest.NewRecorder()
				s.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+flipSimHost+suite+"/Release", nil))
				elapsed := time.Since(start)
				if rec.Code != http.StatusOK || !ran {
					t.Fatalf("hit=%v: status %d, backstop ran %v; want 200 after a backstop", hit, rec.Code, ran)
				}
				if elapsed != backstopTimeout {
					t.Fatalf("hit=%v: served after %v, want after the backstop's %v bound (apt gives up at 30 s)", hit, elapsed, backstopTimeout)
				}
				// Both anchor reads (InRelease, Release.gpg) fail on the ended
				// context and are logged; the base stays unmarked.
				if n := len(logRecords(logs, "suite signed-seen backstop read failed", slog.LevelWarn)); n != 2 || s.marked(suite) {
					t.Fatalf("hit=%v: %d backstop read failures logged, marked %v; want 2 and unmarked", hit, n, s.marked(suite))
				}
			})
		})
	}
}

// FW-4 (final review, concurrency M6): the backstop stamps a sniff-passing
// cached anchor whose url_path row has neither last_requested_at nor
// last_fetched_at with the current time, as the creating transaction's
// marks do, instead of leaving it unmarked (fail-open).
func TestServeHTTP_BackstopStampsAnAnchorWithoutStamps(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	r.serve("/r/dists/g/Release.gpg", upFile{body: []byte("not a signature\n")})
	r.get("/r/dists/g/Release.gpg")
	r.h.cachedAnchorsHook = func() { // the cached Release.gpg becomes a signature, its row without stamps
		r.h.cachedAnchorsHook = nil
		h := writeBlob(t, r.h, rowGPG)
		if err := r.c.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
			Path: "/r/dists/g/Release.gpg", BlobHash: &h, UpstreamURL: r.srv.URL + "/r/dists/g/Release.gpg",
			IsMetadata: true}); err != nil {
			t.Error(err)
		}
	}
	r.serve("/r/dists/g/Release", upFile{body: rowRelease})
	before := wallclock.Unix()
	if rec := r.get("/r/dists/g/Release"); rec.Code != http.StatusOK || r.suite("/r/dists/g") == nil {
		t.Fatalf("Release miss = %d; want the row created", rec.Code)
	}
	after := wallclock.Unix()
	at, ok, err := r.c.SignedSeen(context.Background(), "http", "127.0.0.1", "/r/dists/g")
	if err != nil || !ok || at < before || at > after {
		t.Fatalf("marker = %d %v %v; want the backstop's mark at the current second, in [%d, %d]", at, ok, err, before, after)
	}
}

func derefI64(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

func derefStr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}
