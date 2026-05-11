package handler

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
)

// TestPhase2Migration_V1ToV2EndToEnd is the SPEC2 §12.6 gate: a
// cache_dir written by a Phase 1 binary (schema_version = 1, only
// url_path rows, no suite_snapshot) is opened by a Phase 2 binary
// with adoption enabled, and:
//
//  1. The DB migrates cleanly to schema_version = 2.
//  2. The original url_path rows still serve the existing requests
//     (Phase 1 trust-upstream regime).
//  3. The next InRelease change at upstream triggers a successful
//     adoption.
//  4. After adoption, the same metadata path now serves with
//     X-Cache-Snapshot set in the response.
//
// AIDEV-NOTE: cache/cache_test.go's TestMigration_V1ToV2_* cover the
// SQL migration in isolation — schema shape, FK constraints,
// pre-existing row preservation. This test exercises the end-to-end
// path through the production handler stack, which the cache-layer
// tests cannot reach.
func TestPhase2Migration_V1ToV2EndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("phase 2 migration test skipped in -short mode")
	}

	snapA := makeChaos2Snapshot("A")
	snapB := makeChaos2Snapshot("B")

	var current atomic.Pointer[chaos2Snapshot]
	current.Store(&snapA)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		snap := current.Load()
		body, etag := chaos2BodyAndETagFor(snap, r.URL.Path)
		if body == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("ETag", etag)
		w.Header().Set("Last-Modified", "Mon, 01 Jan 2024 00:00:00 GMT")
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	upstreamURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	// Phase 1: build a v1-schema cache_dir with Phase 1 url_path rows
	// for InRelease, Packages.gz, and one .deb. Each row points at a
	// blob already on disk in pool/. Mimics the state the Phase 1
	// binary would leave behind after one apt-style request set.
	cacheDir := buildV1MigrationFixture(t, upstreamURL, &snapA)

	// Phase 2: open the cache_dir with the current binary's cache.Open,
	// which detects schema_version = 1 and runs the v1 → v2 migration.
	c, err := cache.Open(context.Background(), cacheDir, silentLogger())
	if err != nil {
		t.Fatalf("cache.Open after Phase 1 fixture: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	// Sanity: schema_version is now 2.
	v := readSchemaVersionForTest(t, cacheDir)
	if v != cache.CurrentSchemaVersion {
		t.Fatalf("schema_version after migration = %d, want %d", v, cache.CurrentSchemaVersion)
	}
	// Sanity: the v1 url_path row survives the migration. LookupURL is
	// the production code path; this confirms the seeded row is
	// reachable by canonical_scheme/host/path that the handler will
	// derive from a request to srv.URL.
	row, err := c.LookupURL(context.Background(), "http", upstreamURL.Hostname(), chaos2Suite+"/InRelease")
	if err != nil {
		t.Fatalf("LookupURL post-migration: %v (host=%q)", err, upstreamURL.Hostname())
	}
	if row == nil || row.BlobHash == nil {
		t.Fatalf("LookupURL post-migration: row=%+v (expected v1 row to survive migration)", row)
	}

	// Phase 3: wire a real Phase 2 handler stack against the migrated
	// cache. Use the chaos2 verifier (pass-through) and the
	// port-rewriting AdoptionFetcher so adoption can talk to httptest.
	stack := newPhase2MigrationStack(t, c, upstreamURL)
	defer stack.handler.Close()

	// Phase 4: verify the v1 url_path rows still serve as cache hits.
	// trySnapshotHit returns "not adopted" (CurrentSnapshotID = NULL);
	// tryURLPathHit finds the migrated url_path row and serves the
	// blob bytes. X-Cache-Snapshot is NOT set on a Phase 1 hit.
	primedPaths := []string{
		chaos2Suite + "/InRelease",
		chaos2Suite + "/" + chaos2PackagesGzPath,
		"/ubuntu/" + chaos2DebRels[0],
	}
	for _, p := range primedPaths {
		rec := httptest.NewRecorder()
		stack.handler.ServeHTTP(rec, proxyReq("GET", srv.URL, p))
		if rec.Code != http.StatusOK {
			t.Errorf("post-migration phase-1 hit %s: status=%d body=%q",
				p, rec.Code, rec.Body.String())
			continue
		}
		want := chaos2WantBody(&snapA, p)
		if got := rec.Body.Bytes(); !bytesEqualForTest(got, want) {
			t.Errorf("post-migration phase-1 hit %s: body mismatch (got %d bytes, want %d)",
				p, len(got), len(want))
		}
		if got := rec.Header().Get("X-Cache"); got != "HIT" {
			t.Errorf("post-migration phase-1 hit %s: X-Cache=%q, want HIT", p, got)
		}
		if got := rec.Header().Get("X-Cache-Snapshot"); got != "" {
			t.Errorf("post-migration phase-1 hit %s: X-Cache-Snapshot=%q, want empty (no adoption yet)",
				p, got)
		}
	}

	// Phase 5: switch upstream to B and trigger an adoption. The next
	// InRelease GET fires freshness; the conditional GET sees the new
	// ETag, observes "changed", spawns adoption. Adoption's member
	// fetch reaches upstream via the port-rewriting fetcher and the
	// flip transaction commits.
	current.Store(&snapB)

	rec := httptest.NewRecorder()
	stack.handler.ServeHTTP(rec, proxyReq("GET", srv.URL, chaos2Suite+"/InRelease"))
	if rec.Code != http.StatusOK {
		t.Fatalf("trigger adoption: status=%d body=%q", rec.Code, rec.Body.String())
	}
	// The trigger request itself must still return A's bytes — the
	// migrated v1 url_path row is the trust anchor until adoption
	// commits, and adoption fires asynchronously after the response
	// is sent. A regression that promoted unverified upstream bytes
	// into the response body during the trigger request would surface
	// here as a "got B's bytes for the trigger" failure.
	if got := rec.Body.Bytes(); !bytesEqualForTest(got, snapA.inRelease) {
		t.Errorf("trigger adoption: body=%d bytes, want A's InRelease (%d bytes) — adoption appears to have leaked unverified B bytes",
			len(got), len(snapA.inRelease))
	}
	if got := rec.Header().Get("X-Cache-Snapshot"); got != "" {
		t.Errorf("trigger adoption: X-Cache-Snapshot=%q, want empty (adoption fires async after response is sent)",
			got)
	}
	if err := waitForMigrationFlip(t, stack, upstreamURL, srv.URL, 15*time.Second); err != nil {
		t.Fatalf("adoption flip never happened: %v", err)
	}
	stack.checker.WaitForAdoptions()

	// Phase 6: post-adoption metadata GETs must serve via snapshot_member
	// with X-Cache-Snapshot set. The .deb path also flips: package_hash
	// now declares B's hash, so tryURLPathHit's defense-in-depth check
	// evicts the stale A-rooted url_path row and the §6.2 miss path
	// refetches B's bytes from upstream.
	suite, err := c.GetSuiteFreshness(context.Background(),
		"http", upstreamURL.Hostname(), chaos2Suite)
	if err != nil || suite == nil || suite.CurrentSnapshotID == nil {
		t.Fatalf("post-adoption: suite_freshness=%+v err=%v", suite, err)
	}
	wantSnapshotID := strconv.FormatInt(*suite.CurrentSnapshotID, 10)

	metaPaths := []string{
		chaos2Suite + "/InRelease",
		chaos2Suite + "/" + chaos2PackagesGzPath,
	}
	for _, p := range metaPaths {
		rec := httptest.NewRecorder()
		stack.handler.ServeHTTP(rec, proxyReq("GET", srv.URL, p))
		if rec.Code != http.StatusOK {
			t.Errorf("post-adoption %s: status=%d body=%q",
				p, rec.Code, rec.Body.String())
			continue
		}
		want := chaos2WantBody(&snapB, p)
		if got := rec.Body.Bytes(); !bytesEqualForTest(got, want) {
			t.Errorf("post-adoption %s: body mismatch (got %d bytes, want B's %d bytes)",
				p, len(got), len(want))
		}
		if got := rec.Header().Get("X-Cache-Snapshot"); got != wantSnapshotID {
			t.Errorf("post-adoption %s: X-Cache-Snapshot=%q, want %q",
				p, got, wantSnapshotID)
		}
	}

	// Post-adoption .deb defense: the migrated v1 url_path row for
	// pkg1 still points at A's blob, but B's snapshot's package_hash
	// now declares B's SHA256 for the same path. SPEC2 §6.1 step 5
	// says tryURLPathHit must detect the divergence and evict the
	// stale row, then the §6.2 miss path refetches B's bytes from
	// upstream, validates them against B's declared hash, and stores
	// the new url_path -> B mapping. First request: X-Cache=MISS,
	// body=B's. Follow-up: X-Cache=HIT, body=B's, url_path now
	// pointing at B's blob.
	debPath := "/ubuntu/" + chaos2DebRels[0]
	wantDeb := snapB.debBodies[chaos2DebRels[0]]

	rec = httptest.NewRecorder()
	stack.handler.ServeHTTP(rec, proxyReq("GET", srv.URL, debPath))
	if rec.Code != http.StatusOK {
		t.Fatalf("post-adoption deb refetch: status=%d body=%q",
			rec.Code, rec.Body.String())
	}
	if got := rec.Body.Bytes(); !bytesEqualForTest(got, wantDeb) {
		t.Errorf("post-adoption deb refetch: body=%d bytes, want B's %d bytes — package_hash defense did not refetch B",
			len(got), len(wantDeb))
	}
	if got := rec.Header().Get("X-Cache"); got != "MISS" {
		t.Errorf("post-adoption deb refetch: X-Cache=%q, want MISS (stale A-rooted url_path row should have been evicted, forcing miss-path refetch)", got)
	}

	rec = httptest.NewRecorder()
	stack.handler.ServeHTTP(rec, proxyReq("GET", srv.URL, debPath))
	if rec.Code != http.StatusOK {
		t.Fatalf("post-adoption deb second hit: status=%d body=%q",
			rec.Code, rec.Body.String())
	}
	if got := rec.Body.Bytes(); !bytesEqualForTest(got, wantDeb) {
		t.Errorf("post-adoption deb second hit: body=%d bytes, want B's %d bytes",
			len(got), len(wantDeb))
	}
	if got := rec.Header().Get("X-Cache"); got != "HIT" {
		t.Errorf("post-adoption deb second hit: X-Cache=%q, want HIT (refetched url_path row should now satisfy package_hash and serve from cache)", got)
	}
}

// buildV1MigrationFixture creates a cache_dir at v1 schema with
// pre-populated url_path/blob/suite_freshness rows and the
// corresponding blob files in pool/. Mimics the on-disk state a
// Phase 1 binary would leave after priming. Returns the directory
// path; caller does not need to clean up (t.TempDir handles it).
func buildV1MigrationFixture(t *testing.T, upstream *url.URL, snap *chaos2Snapshot) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"pool", "tmp", "staging"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}

	// Open SQLite directly and run only the v1 schema. cache.Open
	// always migrates to CurrentSchemaVersion, so we bypass it here
	// and do the v1 setup manually. The DDL is duplicated from
	// cache.migrations[0] (frozen Phase 1 schema; SPEC §4.3).
	dbPath := filepath.Join(dir, "cache.db")
	db, err := sql.Open("sqlite", buildSQLiteDSNForTest(dbPath, false))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(v1SchemaDDLForTest); err != nil {
		t.Fatalf("apply v1 schema: %v", err)
	}

	// Insert blobs + url_path rows + the InRelease seed in
	// suite_freshness. Build URL paths and upstream URLs that match
	// what the Phase 2 handler will see when it parses an apt-style
	// request against srv.URL.
	now := time.Now().Unix()
	host := upstream.Hostname()
	urlPaths := []struct {
		path string
		body []byte
	}{
		{chaos2Suite + "/InRelease", snap.inRelease},
		{chaos2Suite + "/" + chaos2PackagesGzPath, snap.packagesGz},
	}
	// One .deb is sufficient for the SPEC2 §12.6 fixture; use pkg1.
	debRel := chaos2DebRels[0]
	urlPaths = append(urlPaths, struct {
		path string
		body []byte
	}{
		"/ubuntu/" + debRel, snap.debBodies[debRel],
	})

	for _, up := range urlPaths {
		hash := chaos2Sha256Hex(up.body)
		// Write the blob to disk first. cache.BlobPath shards by the
		// first two hex chars (pool/<hh>/<full-hash>); replicate that
		// layout so the migrated cache's serveBlobWithHeaders ->
		// BlobExists check finds the file.
		shardDir := filepath.Join(dir, "pool", hash[:2])
		if err := os.MkdirAll(shardDir, 0o750); err != nil {
			t.Fatalf("mkdir shard %s: %v", shardDir, err)
		}
		blobPath := filepath.Join(shardDir, hash)
		if err := os.WriteFile(blobPath, up.body, 0o640); err != nil {
			t.Fatalf("write blob %s: %v", blobPath, err)
		}
		if _, err := db.Exec(
			`INSERT OR IGNORE INTO blob (hash, size, created_at, refcount) VALUES (?, ?, ?, 0)`,
			hash, len(up.body), now,
		); err != nil {
			t.Fatalf("insert blob %s: %v", hash, err)
		}
		isMetadata := 0
		if up.path == chaos2Suite+"/InRelease" || up.path == chaos2Suite+"/"+chaos2PackagesGzPath {
			isMetadata = 1
		}
		upstreamURLStr := fmt.Sprintf("%s://%s%s", upstream.Scheme, upstream.Host, up.path)
		if _, err := db.Exec(
			`INSERT INTO url_path
			   (canonical_scheme, canonical_host, path, blob_hash,
			    upstream_url, is_metadata, last_requested_at, request_count,
			    last_fetched_at, upstream_etag, upstream_lastmod)
			   VALUES ('http', ?, ?, ?, ?, ?, ?, 1, ?, ?, ?)`,
			host, up.path, hash, upstreamURLStr, isMetadata,
			now, now, `"A"`, "Mon, 01 Jan 2024 00:00:00 GMT",
		); err != nil {
			t.Fatalf("insert url_path %s: %v", up.path, err)
		}
	}

	// suite_freshness seed so the conditional GET has an ETag to send.
	if _, err := db.Exec(
		`INSERT INTO suite_freshness
		   (canonical_scheme, canonical_host, suite_path,
		    last_check_at, last_success_at, inrelease_etag, inrelease_lastmod)
		   VALUES ('http', ?, ?, ?, ?, ?, ?)`,
		host, chaos2Suite, now, now, `"A"`, "Mon, 01 Jan 2024 00:00:00 GMT",
	); err != nil {
		t.Fatalf("insert suite_freshness: %v", err)
	}

	return dir
}

// readSchemaVersionForTest opens the SQLite DB read-only and returns
// the schema_version row's value. Used to assert the v1 → v2
// migration ran on cache.Open without poking at cache internals.
func readSchemaVersionForTest(t *testing.T, dir string) int {
	t.Helper()
	dbPath := filepath.Join(dir, "cache.db")
	db, err := sql.Open("sqlite", buildSQLiteDSNForTest(dbPath, true))
	if err != nil {
		t.Fatalf("readSchemaVersionForTest: open: %v", err)
	}
	defer func() { _ = db.Close() }()
	var v int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&v); err != nil {
		t.Fatalf("readSchemaVersionForTest: query: %v", err)
	}
	return v
}

// buildSQLiteDSNForTest assembles a sqlite file: DSN through net/url
// so a path containing metacharacters (`?`, `#`, spaces) is
// percent-encoded rather than hijacking DSN parsing. Mirrors
// cache.openDB's pragma-via-RawQuery construction. readOnly attaches
// mode=ro for DSNs that only read.
//
// AIDEV-NOTE: keep the pragma surface narrow — these test helpers do
// not need WAL/synchronous tuning; foreign_keys(ON) matches the
// production behavior, and mode=ro is a defense for the
// readSchemaVersionForTest helper which only reads.
func buildSQLiteDSNForTest(path string, readOnly bool) string {
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(ON)")
	if readOnly {
		q.Add("mode", "ro")
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}
	return u.String()
}

// newPhase2MigrationStack wires the handler stack against an
// already-Open()ed cache, so the test can drive the migrated cache
// directly. Mirrors newPhase2ChaosStackWithVerifier but takes the
// cache rather than constructing one — the migration test needs to
// open the cache_dir itself to exercise the migration code path.
func newPhase2MigrationStack(t *testing.T, c *cache.Cache, upstream *url.URL) *phase2ChaosStack {
	t.Helper()
	parser, err := proxy.New(nil, nil)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	fc, err := fetch.New(fetch.Options{
		ConnectTimeout:   2 * time.Second,
		TotalTimeout:     5 * time.Second,
		MaxRetries:       0,
		AllowedHostRegex: []string{`^127\.0\.0\.1$`, `^::1$`},
		DenyTargetRanges: nil,
		Logger:           silentLogger(),
	})
	if err != nil {
		t.Fatalf("fetch.New: %v", err)
	}
	limiter := hostsem.New(8)
	adopter, err := freshness.NewAdopter(freshness.AdoptionConfig{
		Cache:       c,
		Fetcher:     &chaos2RewritingFetcher{upstream: upstream, inner: fc},
		Verifier:    chaos2PassVerifier{},
		HostLimiter: limiter,
		Logger:      silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewAdopter: %v", err)
	}
	checker, err := freshness.New(freshness.Config{
		Cache:       c,
		Fetcher:     fc,
		HostLimiter: limiter,
		Cooldown:    0,
		Refresh:     10 * time.Minute,
		Logger:      silentLogger(),
		Adopter:     adopter,
		LifetimeCtx: context.Background(),
	})
	if err != nil {
		t.Fatalf("freshness.New: %v", err)
	}
	h, err := New(Config{
		Parser:      parser,
		Cache:       c,
		Fetch:       fc,
		HostLimiter: limiter,
		Logger:      silentLogger(),
		Freshness:   checker,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &phase2ChaosStack{handler: h, checker: checker, adopter: adopter}
}

// waitForMigrationFlip polls suite_freshness.current_snapshot_id
// until it's set (any non-NULL value), nudging with InRelease GETs
// to keep maybeFireFreshness firing in case the per-suite TryLock
// missed earlier attempts. The migration test starts with
// CurrentSnapshotID = NULL (never adopted), so any non-NULL value
// indicates the first adoption has flipped.
func waitForMigrationFlip(t *testing.T, stack *phase2ChaosStack, upstreamURL *url.URL, srvURL string, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		suite, err := stack.handler.cache.GetSuiteFreshness(context.Background(),
			"http", upstreamURL.Hostname(), chaos2Suite)
		if err == nil && suite != nil && suite.CurrentSnapshotID != nil {
			return nil
		}
		rec := httptest.NewRecorder()
		stack.handler.ServeHTTP(rec, proxyReq("GET", srvURL, chaos2Suite+"/InRelease"))
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("current_snapshot_id still NULL after %v", timeout)
}

// v1SchemaDDLForTest is a verbatim copy of the v0 → v1 migration in
// internal/cache/schema.go's migrations[0]. Duplicated here because
// the cache package's migrations slice is unexported, and tests
// outside the cache package need a way to construct a v1-schema
// database to exercise migration paths through the handler.
//
// AIDEV-NOTE: keep this in sync with cache/schema.go::migrations[0].
// The v1 schema is frozen — a Phase 1 binary's on-disk state — so
// drift here would be a real bug, not a maintenance issue.
const v1SchemaDDLForTest = `
CREATE TABLE blob (
  hash         TEXT PRIMARY KEY
                 CHECK (length(hash) = 64 AND hash NOT GLOB '*[^0-9a-f]*'),
  size         INTEGER NOT NULL,
  created_at   INTEGER NOT NULL,
  refcount     INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE url_path (
  canonical_scheme  TEXT NOT NULL,
  canonical_host    TEXT NOT NULL,
  path              TEXT NOT NULL,
  blob_hash         TEXT REFERENCES blob(hash),
  upstream_url      TEXT NOT NULL,
  is_metadata       INTEGER NOT NULL,
  last_requested_at INTEGER,
  request_count     INTEGER NOT NULL DEFAULT 0,
  last_fetched_at   INTEGER,
  upstream_etag     TEXT,
  upstream_lastmod  TEXT,
  PRIMARY KEY (canonical_scheme, canonical_host, path)
);

CREATE INDEX idx_url_path_metadata ON url_path(is_metadata);
CREATE INDEX idx_url_path_last_req ON url_path(last_requested_at);

CREATE TABLE suite_freshness (
  canonical_scheme         TEXT NOT NULL,
  canonical_host           TEXT NOT NULL,
  suite_path               TEXT NOT NULL,
  last_check_at            INTEGER,
  last_success_at          INTEGER,
  inrelease_etag           TEXT,
  inrelease_lastmod        TEXT,
  inrelease_change_seen_at INTEGER,
  PRIMARY KEY (canonical_scheme, canonical_host, suite_path)
);

CREATE TABLE schema_version (
  version INTEGER PRIMARY KEY
);
INSERT INTO schema_version VALUES (1);
`
