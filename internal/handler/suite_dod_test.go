package handler

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"
	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// mintedUpstream answers every suite file of any suite on any host with
// bytes that pass the sniffs — a hostile or overly permissive upstream,
// the worst case for row creation.
type mintedUpstream struct{}

func (mintedUpstream) HostAllowed(string) bool { return true }

func (mintedUpstream) Fetch(_ context.Context, target *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error) {
	var body []byte
	switch {
	case strings.HasSuffix(target.URL, "/InRelease"):
		body = simtest.ClearsignedFixture("Origin: minted\nLabel: " + target.URL + "\n")
	case strings.HasSuffix(target.URL, "/Release.gpg"):
		body = rowGPG
	default:
		body = []byte("minted " + target.URL)
	}
	if _, err := dst.Write(body); err != nil {
		return nil, err
	}
	return &fetch.FetchResult{Status: 200, ContentLength: int64(len(body))}, nil
}

// Phase D DoD (spec §9; Review Focus 1): minted dists/ codenames, their
// Release.gpg and by-hash names on many hosts, from an upstream that
// answers all of them with sniff-passing bytes, create rows only up to the
// caps — 4 per host and 40 in all, the global cap first evicting the
// suites idle for a day without a current snapshot (design review I5) —
// markers only for tracked suites, and no growth of the checker's lock
// map, the flip gate or the touch map beyond the tracked suites: each
// holds exactly one entry per surviving row, so an evicted suite left all
// three. The rejection cache holds the refused suites only. The gate is
// the adopter's, so eviction purges the gate the handler registers on, as
// in production (design note 21). A bubble, so the evictees' serves can be
// a quiet period old: the purge keeps a live entry (ruling R-F2).
func TestServeHTTP_MintedSuitesAtScale(t *testing.T) {
	synctest.Test(t, testMintedSuitesAtScale)
}

func testMintedSuitesAtScale(t *testing.T) {
	c := simtest.OpenCache(t)
	ctx := context.Background()
	// The gate sweeps an entry idle for twice its quiet period (30 s at
	// the longest quiet period, 15 s; deferred minor D-39); no entry is
	// that idle when the counts are read, so they are exact.
	gate, err := flipgate.New(flipgate.Config{Quiet: flipgate.MaxQuiet})
	if err != nil {
		t.Fatal(err)
	}
	// Every check answers 304, so no check sees a change and nothing adopts:
	// the adopter is here for its gate.
	up := &simtest.StaticConditional{Status: http.StatusNotModified}
	ad, err := freshness.NewAdopter(freshness.AdoptionConfig{Cache: c, Fetcher: &memberStore{bodies: map[string][]byte{}},
		Verifier: plainVerifier{}, HostLimiter: hostsem.New(64), Logger: simtest.DiscardLogger(), FlipGate: gate})
	if err != nil {
		t.Fatal(err)
	}
	const hosts, perHost, perHostCap, maxSuites = 11, 8, 4, 40
	ck, err := freshness.New(freshness.Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(64),
		Logger: simtest.DiscardLogger(), MaxSuites: maxSuites, Adopter: ad})
	if err != nil {
		t.Fatal(err)
	}
	parser, err := proxy.New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	newHandler := func() *Handler {
		h, err := New(Config{Parser: parser, Cache: c, Fetch: mintedUpstream{}, Logger: simtest.DiscardLogger(),
			HostLimiter: hostsem.New(64), Freshness: ck, AdoptionEnabled: true, FlipGate: ad.FlipGate(),
			MaxSuites: maxSuites, SuiteGuard: ck})
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	get := func(h *Handler, url string) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", url, rec.Code)
		}
	}
	// Four suites a client used two days ago, unadopted: the global cap's
	// eviction candidates. Their miss and url_path hit give each a
	// lock-map, gate and touch-map entry.
	idle := newHandler()
	for j := range 4 {
		base := fmt.Sprintf("http://idle.example/r/dists/old%d", j)
		get(idle, base+"/InRelease")
		get(idle, base+"/InRelease")
	}
	idle.Close() // drains their T1 checks
	// Ruling R-F2: an evictee's gate entry is purged only once its last
	// serve is a quiet period old (a live entry expires on its own); twice
	// that would let the sweep drop it first.
	time.Sleep(flipgate.MaxQuiet)
	db := sqliteFile(t, filepath.Join(c.Dir(), "cache.db"))
	if _, err := db.Exec(`UPDATE suite_freshness SET last_requested_at = ? WHERE canonical_host = 'idle.example'`,
		time.Now().Add(-48*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	h := newHandler()
	byHash := "/main/by-hash/SHA256/" + strings.Repeat("ab", 32)
	for i := range hosts {
		for j := range perHost {
			base := fmt.Sprintf("http://h%02d.example/r/dists/minted%03d", i, j)
			for _, p := range []string{"/InRelease", "/Release.gpg", byHash, "/InRelease"} {
				get(h, base+p)
			}
		}
	}
	h.Close() // drains the hits' T1 checks
	all, err := c.ListSuites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tracked := map[string]bool{}
	rowsOf := map[string]int{}
	for _, s := range all {
		tracked[s.CanonicalHost+s.SuitePath] = true
		rowsOf[s.CanonicalHost]++
	}
	if len(all) != maxSuites {
		t.Fatalf("rows = %d, want %d (the global cap)", len(all), maxSuites)
	}
	// Ten hosts fill their per-host cap (the tenth only after the global
	// cap evicted the idle suites); the eleventh is refused at the global
	// cap with no row of its own.
	if n := rowsOf["idle.example"]; n != 0 {
		t.Fatalf("%d idle suites survive, want all 4 evicted at the global cap", n)
	}
	for i := range hosts - 1 {
		if n := rowsOf[fmt.Sprintf("h%02d.example", i)]; n != perHostCap {
			t.Fatalf("h%02d: %d rows, want the per-host cap %d", i, n, perHostCap)
		}
	}
	if n := rowsOf[fmt.Sprintf("h%02d.example", hosts-1)]; n != 0 {
		t.Fatalf("h%02d: %d rows, want 0 (refused at the global cap)", hosts-1, n)
	}
	for i := range hosts {
		for j := range perHost {
			host, suite := fmt.Sprintf("h%02d.example", i), fmt.Sprintf("/r/dists/minted%03d", j)
			_, ok, err := c.SignedSeen(ctx, "http", host, suite)
			if err != nil {
				t.Fatal(err)
			}
			if ok != tracked[host+suite] {
				t.Fatalf("%s%s: marker %v, tracked %v; want a marker exactly for tracked suites", host, suite, ok, tracked[host+suite])
			}
		}
	}
	if n := ck.LockCount(); n != len(all) {
		t.Errorf("lock map = %d entries, want one per tracked suite (%d): an evicted suite leaves it", n, len(all))
	}
	if n := gate.Len(); n != len(all) {
		t.Errorf("flip gate = %d entries, want one per tracked suite (%d): an evicted suite leaves it", n, len(all))
	}
	if n := c.TouchedSuites(); n != len(all) {
		t.Errorf("touch map = %d entries, want one per tracked suite (%d): an evicted suite leaves it", n, len(all))
	}
	if n, want := h.rejections.len(), (hosts-1)*(perHost-perHostCap)+perHost; n != want {
		t.Errorf("rejection cache = %d entries, want the %d refused suites", n, want)
	}
}

// firstByteProbe records, at the first body byte, whether the suite has a
// row and its base a marker.
type firstByteProbe struct {
	*httptest.ResponseRecorder
	c         *cache.Cache
	suite     string
	seen      bool
	row, mark bool
}

func (p *firstByteProbe) Write(b []byte) (int, error) {
	if !p.seen {
		p.seen = true
		ok, err := p.c.HasSuiteRow(context.Background(), cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "127.0.0.1", SuitePath: p.suite})
		p.row = err == nil && ok
		_, p.mark, _ = p.c.SignedSeen(context.Background(), "http", "127.0.0.1", aptpath.MarkerBase(p.suite))
	}
	return p.ResponseRecorder.Write(b)
}

// sqliteFile opens path on its own connection (fixtures the cache API does
// not write).
func sqliteFile(t *testing.T, path string) *sql.DB {
	t.Helper()
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// Phase D DoD (spec §9; Review Focus 2): a cache a v6 binary wrote — a
// dists/ InRelease cached, no suite row — upgraded in place. The startup
// pass marks nothing (the suite is untracked); the anchor's first hit
// onboards the suite, with its marker, before the first body byte.
func TestServeHTTP_OnboardsADistsSuiteFromAV6Cache(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	if err := cache.MigrateToVersionForTest(ctx, dir, 6); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(rowInRelease)
	hash := hex.EncodeToString(sum[:])
	db := sqliteFile(t, filepath.Join(dir, "cache.db"))
	for _, q := range []string{
		`INSERT INTO blob(hash,size,created_at,refcount) VALUES(?,?,1,0)`,
		`INSERT INTO url_path(canonical_scheme,canonical_host,path,blob_hash,upstream_url,is_metadata,last_requested_at,request_count)
		 VALUES('http','127.0.0.1','/u/dists/s/InRelease',?,'http://127.0.0.1/u/dists/s/InRelease',1,1700000000,3)`,
	} {
		args := []any{hash}
		if strings.Contains(q, "INTO blob") {
			args = append(args, len(rowInRelease))
		}
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
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
		t.Fatalf("startup pass = %+v, %v; want no marker for the untracked suite", res, err)
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
	p := &firstByteProbe{ResponseRecorder: httptest.NewRecorder(), c: c, suite: "/u/dists/s"}
	h.ServeHTTP(p, newProxyReqHostPort("GET", "http", "127.0.0.1", "/u/dists/s/InRelease"))
	if p.Code != http.StatusOK || p.Header().Get(hdrXCache) != cacheHit || !p.seen {
		t.Fatalf("hit = %d %q, body written %v", p.Code, p.Header().Get(hdrXCache), p.seen)
	}
	if !p.row || !p.mark {
		t.Fatalf("at the first body byte row=%v marker=%v, want both", p.row, p.mark)
	}
	s, err := c.GetSuiteFreshness(ctx, "http", "127.0.0.1", "/u/dists/s")
	if err != nil || s.LastCheckAt != nil || s.LastSuccessAt != nil {
		t.Fatalf("onboarded row = %+v, %v; want NULL check stamps (its first check runs at once)", s, err)
	}
}

// assertHitPathInvariant checks deviation 1's invariant: every
// sniff-passing InRelease or Release.gpg url_path row of a tracked suite
// has a marker for its base. Every read error fails the test: the oracle
// never skips a row it could not check.
func assertHitPathInvariant(t *testing.T, c *cache.Cache, db *sql.DB, when string) {
	t.Helper()
	ctx := context.Background()
	rows, err := db.Query(`SELECT canonical_scheme, canonical_host, path, blob_hash FROM url_path
 WHERE path LIKE '%/InRelease' OR path LIKE '%/Release.gpg'`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	checked := 0
	for rows.Next() {
		var scheme, host, path, hash string
		if err := rows.Scan(&scheme, &host, &path, &hash); err != nil {
			t.Fatal(err)
		}
		suite := strings.TrimSuffix(strings.TrimSuffix(path, "/InRelease"), "/Release.gpg")
		ok, err := c.HasSuiteRow(ctx, cache.SuiteKey{CanonicalScheme: scheme, CanonicalHost: host, SuitePath: suite})
		if err != nil {
			t.Fatalf("%s: %s %s%s: HasSuiteRow: %v", when, scheme, host, suite, err)
		}
		if !ok {
			continue
		}
		body, err := os.ReadFile(c.BlobPath(hash))
		if err != nil {
			t.Fatal(err)
		}
		file := aptpath.ClassifySuiteFile(suite, path)
		if !file.Sniff(path, body) {
			continue
		}
		checked++
		_, marked, err := c.SignedSeen(ctx, scheme, host, aptpath.MarkerBase(suite))
		if err != nil {
			t.Fatalf("%s: %s %s%s: SignedSeen: %v", when, scheme, host, path, err)
		}
		if !marked {
			t.Errorf("%s: %s of a tracked suite has no marker", when, path)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: scanning url_path: %v", when, err)
	}
	if checked == 0 {
		t.Fatalf("%s: no tracked signed anchor to check", when)
	}
}

// Deviation 1, spec §7: after first-contact and refresh misses, a cap
// refusal later lifted, legacy onboarding, an adoption whose anchor row
// the checker reconstructs, and a restart, every sniff-passing anchor row
// of a tracked suite has a marker for its base. Since a creation marks in
// its own transaction (ruling PD-5), the invariant holds at every
// committed state but one, which spec C3 accepts: an unproven creation (a
// cached anchor that changed, appeared or could not be read during the
// creation) is tracked with that anchor unmarked from its commit until its
// backstop runs — right after, before the response — and, if the backstop
// fails, until the startup pass at the next start. The test checks the
// invariant after every step (none of them is unproven).
func TestHitPathInvariant_EveryTrackedSignedAnchorHasAMarker(t *testing.T) {
	r := newSuiteRig(t, true, 10) // per-host cap 1
	ctx := context.Background()
	db := r.secondConn()
	r.serve("/r/dists/a/InRelease", upFile{body: rowInRelease})
	r.serve("/r/dists/b/InRelease", upFile{body: rowInRelease})
	r.serve("/r/dists/b/Release.gpg", upFile{body: rowGPG})
	r.get("/r/dists/a/InRelease") // first contact: tracked
	assertHitPathInvariant(t, r.c, db, "after a's first contact")
	// b is refused at the per-host cap; its anchors are still served and
	// cached, for the onboarding below to find.
	for _, p := range []string{"/r/dists/b/InRelease", "/r/dists/b/Release.gpg"} {
		if rec := r.get(p); rec.Code != http.StatusOK {
			t.Fatalf("premise: refused %s = %d, want 200", p, rec.Code)
		}
		if _, err := r.c.LookupURL(ctx, "http", "127.0.0.1", p); err != nil {
			t.Fatalf("premise: refused %s was not cached (%v)", p, err)
		}
	}
	if r.suite("/r/dists/b") != nil {
		t.Fatal("premise: b must be refused")
	}
	assertHitPathInvariant(t, r.c, db, "after b's refusal")
	// The cap is lifted (a restart with a larger max_suites): a second
	// handler over the same cache.
	h2, err := New(Config{Parser: r.h.parser, Cache: r.c, Fetch: r.h.fetch, Logger: silentLogger(),
		HostLimiter: hostsem.New(4), AdoptionEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h2.Close)
	get2 := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		h2.ServeHTTP(rec, proxyReq("GET", r.srv.URL, path))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", path, rec.Code)
		}
		return rec
	}
	// onboard2 serves a cached anchor through h2 — a url_path hit — and
	// requires that it tracked the suite: onboarding fails open by design
	// (design note 23), so the invariant checks below would otherwise pass
	// with the suite left untracked.
	onboard2 := func(path, suite string) {
		t.Helper()
		if rec := get2(path); rec.Header().Get(hdrXCache) != cacheHit {
			t.Fatalf("%s: X-Cache %q, want %q (onboarding is a url_path hit)", path, rec.Header().Get(hdrXCache), cacheHit)
		}
		if r.suite(suite) == nil {
			t.Fatalf("%s: %s was not onboarded (no row)", path, suite)
		}
	}
	onboard2("/r/dists/b/InRelease", "/r/dists/b") // onboards b, marking its cached anchors
	assertHitPathInvariant(t, r.c, db, "after b's onboarding")
	r.cacheAnchor("/r/dists/c/InRelease", rowInRelease, 1_700_000_000)
	r.cacheAnchor("/r/dists/c/Release.gpg", rowGPG, 1_700_000_000)
	if rec := get2("/r/dists/c/Release.gpg"); rec.Header().Get(hdrXCache) != cacheHit || r.suite("/r/dists/c") != nil {
		t.Fatalf("c's Release.gpg: X-Cache %q, row %v; want a hit that creates no row (Release.gpg never does)",
			rec.Header().Get(hdrXCache), r.suite("/r/dists/c") != nil)
	}
	onboard2("/r/dists/c/InRelease", "/r/dists/c") // onboards c
	assertHitPathInvariant(t, r.c, db, "after c's onboarding")
	r.cacheAnchor("/r/dists/e/Release", rowRelease, 1_700_000_000)
	r.cacheAnchor("/r/dists/e/Release.gpg", rowGPG, 1_700_000_000)
	// Onboards e: Release marks nothing itself; the creating transaction
	// marks its cached Release.gpg (SuiteSeed.Cached).
	onboard2("/r/dists/e/Release", "/r/dists/e")
	assertHitPathInvariant(t, r.c, db, "after e's onboarding")
	if _, err := db.Exec(`DELETE FROM url_path WHERE path = '/r/dists/a/InRelease';
DELETE FROM suite_signed_seen WHERE base = '/r/dists/a'`); err != nil {
		t.Fatal(err)
	}
	get2("/r/dists/a/InRelease") // refresh miss
	assertHitPathInvariant(t, r.c, db, "after a's refresh miss")
	// d: adopted (CommitAdoption marks and seeds its anchor row); the row
	// is then lost, and the checker reconstructs it from the snapshot
	// (design review I4: without the delete, reconstructAnchor never runs).
	inrel := writeBlob(t, r.h, rowInRelease)
	commitInlineSnapshot(t, r.h, "http", "127.0.0.1", "/r/dists/d", inrel,
		[]cache.SnapshotMember{{Path: "InRelease", BlobHash: inrel, DeclaredSHA256: inrel}}, nil)
	if _, err := db.Exec(`DELETE FROM url_path WHERE path = '/r/dists/d/InRelease'`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.LookupURL(ctx, "http", "127.0.0.1", "/r/dists/d/InRelease"); !errors.Is(err, cache.ErrNotFound) {
		t.Fatalf("premise: d's anchor row is gone (%v)", err)
	}
	ck, err := freshness.New(freshness.Config{Cache: r.c, Fetcher: &simtest.StaticConditional{Status: http.StatusNotModified},
		HostLimiter: hostsem.New(4), Logger: silentLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ck.Check(ctx, "http", "127.0.0.1", "/r/dists/d")
	if row, err := r.c.LookupURL(ctx, "http", "127.0.0.1", "/r/dists/d/InRelease"); err != nil || row.BlobHash == nil || *row.BlobHash != inrel {
		t.Fatalf("the checker did not reconstruct d's anchor row from its snapshot (%+v, %v)", row, err)
	}
	assertHitPathInvariant(t, r.c, db, "after serving")
	// A restart: markers lost (an earlier binary never wrote them), healed
	// by the startup pass before anything serves.
	if _, err := db.Exec(`DELETE FROM suite_signed_seen`); err != nil {
		t.Fatal(err)
	}
	if _, err := freshness.RunLegacyTrustAudit(ctx, r.c, silentLogger()); err != nil {
		t.Fatal(err)
	}
	assertHitPathInvariant(t, r.c, db, "after a restart")
}
