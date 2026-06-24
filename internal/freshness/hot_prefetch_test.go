package freshness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
)

func sha256OfBytes(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// hotPrefetchFixture wires a cache + Adopter with a single seeded
// prior snapshot whose package_hash rows are eligible for the
// SPEC3 §7.5.3 Stage-1 hot match. Each test then sets up a candidate
// snapshot whose package_hash rows the hot loop will attempt to
// warm. The Adopter's fakeFetcher is exposed so each test can shape
// its per-deb success / failure / hash-mismatch / hang behavior.
type hotPrefetchFixture struct {
	t                      *testing.T
	cache                  *cache.Cache
	adopter                *Adopter
	fetcher                *hotFakeFetcher
	suite                  SuiteRef
	priorSnapshot          int64
	candidate              int64
	debDeclared            string // sha256 of the deb the candidate vouches for
	debUpstreamURL         string
	debPath                string
	candidatePackageHashes []cache.PackageHash // passed to runHotPrefetch (Stage 2 in-memory)
}

func newHotPrefetchFixture(t *testing.T, budget time.Duration) *hotPrefetchFixture {
	t.Helper()
	dir := t.TempDir()
	c, err := cache.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ff := newHotFakeFetcher()
	ad, err := NewAdopter(AdoptionConfig{
		Cache:             c,
		Fetcher:           ff,
		Verifier:          passThroughVerifier{},
		HostLimiter:       hostsem.New(8),
		HotPackagesWindow: 24 * time.Hour,
		HotPrefetchBudget: budget,
	})
	if err != nil {
		t.Fatalf("NewAdopter: %v", err)
	}

	suite := SuiteRef{
		CanonicalScheme: "http",
		CanonicalHost:   "archive.example",
		SuitePath:       "/dists/noble",
	}

	// Seed prior snapshot: one .deb covered, with hot url_path. The
	// prior url_path row must FK-reference an existing blob.
	priorBlobBytes := []byte("old deb bytes")
	priorBlob := writeFixtureBlob(t, c, priorBlobBytes)
	priorRelease := writeFixtureBlob(t, c, []byte("InRelease v1"))
	priorID, _, err := c.InsertCandidateSnapshot(context.Background(), cache.SnapshotCandidate{
		CanonicalScheme: suite.CanonicalScheme,
		CanonicalHost:   suite.CanonicalHost,
		SuitePath:       suite.SuitePath,
		InReleaseHash:   &priorRelease,
	})
	if err != nil {
		t.Fatalf("InsertCandidateSnapshot prior: %v", err)
	}
	const debP = "/pool/main/h/hello/hello.deb"
	if err := c.PutSuiteFreshness(context.Background(), cache.SuiteFreshness{
		CanonicalScheme: suite.CanonicalScheme,
		CanonicalHost:   suite.CanonicalHost,
		SuitePath:       suite.SuitePath,
	}); err != nil {
		t.Fatalf("PutSuiteFreshness: %v", err)
	}
	if err := c.CommitAdoption(context.Background(), priorID,
		[]cache.SnapshotMember{{SnapshotID: priorID, Path: "InRelease", BlobHash: priorRelease, DeclaredSHA256: priorRelease}}, nil,

		[]cache.PackageHash{{
			CanonicalScheme: suite.CanonicalScheme,
			CanonicalHost:   suite.CanonicalHost,
			Path:            debP,
			DeclaredSHA256:  priorBlob,
			SnapshotID:      priorID,
			PackageName:     "hello",
			Architecture:    "amd64",
		}},
		nil, true); err != nil {
		t.Fatalf("commit prior: %v", err)
	}
	now := time.Now().Unix()
	if err := c.PutURLPath(context.Background(), cache.URLPath{
		CanonicalScheme: suite.CanonicalScheme,
		CanonicalHost:   suite.CanonicalHost,
		Path:            debP,
		BlobHash:        &priorBlob,
		UpstreamURL:     "http://archive.example" + debP,
		LastRequestedAt: &now,
		RequestCount:    1,
	}); err != nil {
		t.Fatalf("PutURLPath: %v", err)
	}

	// Seed candidate snapshot covering the new version of the same
	// (Package, Arch). The new path is what runHotPrefetch will try
	// to warm; we use the same path here for simplicity (a Phase 3
	// upgrade-in-place where the filename was stable).
	candidateRelease := writeFixtureBlob(t, c, []byte("InRelease v2"))
	candidateID, _, err := c.InsertCandidateSnapshot(context.Background(), cache.SnapshotCandidate{
		CanonicalScheme: suite.CanonicalScheme,
		CanonicalHost:   suite.CanonicalHost,
		SuitePath:       suite.SuitePath,
		InReleaseHash:   &candidateRelease,
	})
	if err != nil {
		t.Fatalf("InsertCandidateSnapshot candidate: %v", err)
	}
	newDebContent := []byte("new deb bytes v2")
	newDebHash := sha256OfBytes(newDebContent)
	// The candidate's package_hash row references the NEW hash.
	// We don't commit the candidate yet — runHotPrefetch reads
	// package_hash for the candidate snapshot before commit.
	if _, err := c.HostCurrentSnapshotsCoverage(context.Background(), suite.CanonicalScheme, suite.CanonicalHost); err != nil {
		t.Fatalf("HostCurrentSnapshotsCoverage: %v", err)
	}
	// Use the writer to insert the candidate's package_hash row
	// directly via a fresh InsertCandidateSnapshot+CommitAdoption
	// dance is overkill — rely on a direct DB write below would
	// require exposing a private API. Instead, drive it through
	// CommitAdoption, since runHotPrefetch is what we're testing
	// independently and the candidate must already be in DB before
	// we call runHotPrefetch.
	//
	// For test simplicity, we *commit* the candidate with the
	// hot-prefetch's expected package_hash rows inserted ahead of
	// time. The candidate's snapshot_id is what runHotPrefetch
	// uses for the Stage 2 lookup; the actual flip is irrelevant
	// to the prefetch loop's behavior.
	candPHs := []cache.PackageHash{{
		CanonicalScheme: suite.CanonicalScheme,
		CanonicalHost:   suite.CanonicalHost,
		Path:            debP,
		DeclaredSHA256:  newDebHash,
		SnapshotID:      candidateID,
		PackageName:     "hello",
		Architecture:    "amd64",
		Version:         "2.0",
	}}
	if err := c.CommitAdoption(context.Background(), candidateID,
		[]cache.SnapshotMember{{SnapshotID: candidateID, Path: "InRelease", BlobHash: candidateRelease, DeclaredSHA256: candidateRelease}}, nil,

		candPHs, nil, true); err != nil {
		t.Fatalf("commit candidate (test fixture): %v", err)
	}

	return &hotPrefetchFixture{
		t:                      t,
		cache:                  c,
		adopter:                ad,
		fetcher:                ff,
		suite:                  suite,
		priorSnapshot:          priorID,
		candidate:              candidateID,
		debDeclared:            newDebHash,
		debUpstreamURL:         "http://archive.example" + debP,
		debPath:                debP,
		candidatePackageHashes: candPHs,
	}
}

// TestRunHotPrefetch_Success: happy path — fetcher returns matching
// bytes; the loop produces one PrefetchedURLPath; stats.fetched = 1.
func TestRunHotPrefetch_Success(t *testing.T) {
	f := newHotPrefetchFixture(t, 5*time.Second)
	newDebBytes := []byte("new deb bytes v2")
	f.fetcher.setBody(f.debUpstreamURL, newDebBytes)

	rows, stats := f.adopter.runHotPrefetch(context.Background(), f.suite, f.candidate, f.candidatePackageHashes, nil)
	if stats.hotCount != 1 {
		t.Fatalf("hotCount=%d, want 1", stats.hotCount)
	}
	if stats.fetched != 1 || stats.failed != 0 || stats.mismatched != 0 || stats.unattempted != 0 {
		t.Errorf("buckets fetched=%d failed=%d mismatched=%d unattempted=%d, want 1/0/0/0",
			stats.fetched, stats.failed, stats.mismatched, stats.unattempted)
	}
	if len(rows) != 1 {
		t.Fatalf("rows len=%d, want 1", len(rows))
	}
	if rows[0].Path != f.debPath || rows[0].BlobHash != f.debDeclared {
		t.Errorf("row=%+v, want path=%s hash=%s", rows[0], f.debPath, f.debDeclared)
	}
}

// TestRunHotPrefetch_RetryExhausted: per-deb upstream failure goes
// to stats.failed. SPEC3 §7.5.5: "hot_prefetch_deb_failed" — loop
// continues, partial does NOT fire. Verifies the failure-bucket
// classification.
func TestRunHotPrefetch_RetryExhausted(t *testing.T) {
	f := newHotPrefetchFixture(t, 5*time.Second)
	f.fetcher.setError(f.debUpstreamURL, errors.New("simulated upstream 502"))

	_, stats := f.adopter.runHotPrefetch(context.Background(), f.suite, f.candidate, f.candidatePackageHashes, nil)
	if stats.failed != 1 || stats.fetched != 0 || stats.mismatched != 0 || stats.unattempted != 0 {
		t.Errorf("buckets fetched=%d failed=%d mismatched=%d unattempted=%d, want 0/1/0/0",
			stats.fetched, stats.failed, stats.mismatched, stats.unattempted)
	}
}

// TestRunHotPrefetch_HashMismatch: fetcher serves bytes whose
// computed hash disagrees with the declared sha. SPEC3 §7.5.5:
// "hot_prefetch_hash_mismatch" — temp blob discarded, NOT promoted
// to pool/. stats.mismatched++.
func TestRunHotPrefetch_HashMismatch(t *testing.T) {
	f := newHotPrefetchFixture(t, 5*time.Second)
	wrongBytes := []byte("hostile upstream sent these bytes instead")
	f.fetcher.setBody(f.debUpstreamURL, wrongBytes)

	rows, stats := f.adopter.runHotPrefetch(context.Background(), f.suite, f.candidate, f.candidatePackageHashes, nil)
	if stats.mismatched != 1 || stats.fetched != 0 || stats.failed != 0 {
		t.Errorf("buckets fetched=%d failed=%d mismatched=%d unattempted=%d, want 0/0/1/0",
			stats.fetched, stats.failed, stats.mismatched, stats.unattempted)
	}
	if len(rows) != 0 {
		t.Errorf("rows len=%d, want 0 — mismatched fetches must not be promoted to url_path", len(rows))
	}
	// SPEC3 §7.5: pool/ MUST NOT contain the wrong-hash bytes —
	// FinalizeExpectingHash discards on mismatch BEFORE rename.
	wrongHash := sha256OfBytes(wrongBytes)
	if exists, _ := f.cache.BlobExists(wrongHash); exists {
		t.Errorf("pool/<wrongHash=%s> exists; the SPEC3 \"do not promote\" contract was violated", wrongHash[:12])
	}
}

// TestRunHotPrefetch_BudgetElapsed: the prefetch wall-clock budget
// elapses while a fetch is in flight. SPEC3 §12.3 variant 1 contract:
// "the hung fetch was attempted but cancelled when prefetchCtx hit
// the budget" — bucket as failed (with hot_prefetch_deb_failed log),
// NOT unattempted. With N=1 hot deb and the only entry hung, the
// loop logs deb_failed for that path, increments stats.failed, and
// the loop ends naturally (no further iterations) so partial does
// NOT fire (queue empty at next top-of-iteration check, same shape
// as the §12.3 hung-LAST variant 3 contract).
func TestRunHotPrefetch_BudgetElapsed(t *testing.T) {
	f := newHotPrefetchFixture(t, 50*time.Millisecond)
	// Block the fetch past the budget. The fetch returns when ctx
	// cancellation propagates (real fetch.Client does the same).
	f.fetcher.setBlocking(f.debUpstreamURL, 5*time.Second)

	_, stats := f.adopter.runHotPrefetch(context.Background(), f.suite, f.candidate, f.candidatePackageHashes, nil)
	if stats.failed != 1 || stats.fetched != 0 || stats.mismatched != 0 || stats.unattempted != 0 {
		t.Errorf("buckets fetched=%d failed=%d mismatched=%d unattempted=%d, want 0/1/0/0",
			stats.fetched, stats.failed, stats.mismatched, stats.unattempted)
	}
}

// TestRunHotPrefetch_ParentCancelled: parent adoptionCtx cancellation
// (SIGTERM equivalent) does NOT emit adoption_hot_prefetch_partial.
// SPEC3 §7.5.5: "shutdown cancellation: adoption is aborted; partial
// is NOT logged." Driving with budget=0 (no internal timeout) so the
// only ctx that can complete is the parent.
func TestRunHotPrefetch_ParentCancelled(t *testing.T) {
	f := newHotPrefetchFixture(t, 0) // no budget
	f.fetcher.setBlocking(f.debUpstreamURL, 5*time.Second)

	parentCtx, cancel := context.WithCancel(context.Background())
	go func() {
		// Cancel parent shortly after runHotPrefetch starts.
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	defer cancel()
	rows, stats := f.adopter.runHotPrefetch(parentCtx, f.suite, f.candidate, f.candidatePackageHashes, nil)
	// We don't assert on log emission directly here (no log
	// recorder hooked up in this fixture); we assert the contract
	// shape: a parent-cancelled run still bucket-counts the
	// in-flight entry as unattempted but adopts the
	// "stats.unattempted=1, no partial event" contract by NOT
	// over-counting buckets.
	total := stats.fetched + stats.failed + stats.mismatched + stats.unattempted
	if total != stats.hotCount {
		t.Errorf("buckets total=%d, hotCount=%d — sum must equal hotCount",
			total, stats.hotCount)
	}
	if len(rows) != 0 {
		t.Errorf("rows len=%d, want 0 (no fetch completed before parent cancel)", len(rows))
	}
}

// TestRunHotPrefetch_EmptyHotSet: when computeHotSet returns nothing
// (no prior url_path is hot), the loop emits started + complete with
// hot_count=0 and returns nil/empty. Not a failure mode per se,
// but the bucket-arithmetic invariant must still hold.
func TestRunHotPrefetch_EmptyHotSet(t *testing.T) {
	f := newHotPrefetchFixture(t, 5*time.Second)
	// Hand the adopter a fresh suite with no prior snapshot. We do
	// this by passing a different suite; computeHotSet returns nil.
	rows, stats := f.adopter.runHotPrefetch(context.Background(),
		SuiteRef{
			CanonicalScheme: "http",
			CanonicalHost:   "archive.example",
			SuitePath:       "/dists/cold",
		},
		f.candidate, f.candidatePackageHashes, nil)
	if stats.hotCount != 0 {
		t.Errorf("hotCount=%d, want 0", stats.hotCount)
	}
	if len(rows) != 0 {
		t.Errorf("rows len=%d, want 0", len(rows))
	}
}

// TestBuildPackageHashes_AllowsDuplicatePackageArch: SPEC3 §7.5.3
// Stage 2 SQL is a multi-row SELECT — when the candidate snapshot
// has two distinct debPaths sharing a single (Package, Architecture),
// both rows survive into package_hash and both will be warmed by the
// hot-prefetch loop. Empirically rare in production apt repos but
// not forbidden by the spec, and rejecting it would block legitimate
// (if unusual) Release files.
func TestBuildPackageHashes_AllowsDuplicatePackageArch(t *testing.T) {
	dir := t.TempDir()
	c, err := cache.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	defer func() { _ = c.Close() }()

	logger := slog.Default()
	a := &Adopter{cache: c, logger: logger}

	debHashA := strings.Repeat("a", 64)
	debHashB := strings.Repeat("b", 64)
	pkgs := []byte(
		"Package: nginx\nArchitecture: amd64\nVersion: 1.0\n" +
			"Filename: pool/main/n/nginx/nginx_1.0_amd64.deb\n" +
			"Size: 1\nSHA256: " + debHashA + "\n\n" +
			"Package: nginx\nArchitecture: amd64\nVersion: 2.0\n" +
			"Filename: pool/main/n/nginx/nginx_2.0_amd64.deb\n" +
			"Size: 1\nSHA256: " + debHashB + "\n",
	)
	pkgsBlob := writeFixtureBlob(t, c, pkgs)

	suiteRef := SuiteRef{
		CanonicalScheme: "http",
		CanonicalHost:   "archive.example",
		SuitePath:       "/dists/noble",
	}
	members := []ReleaseMember{
		{Path: "main/binary-amd64/Packages", SHA256: pkgsBlob, Size: int64(len(pkgs))},
	}
	res, err := a.buildPackageHashes(suiteRef, 1, members, members)
	if err != nil {
		t.Fatalf("buildPackageHashes: unexpected error: %v", err)
	}
	if len(res.rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(res.rows))
	}
	seen := map[string]string{}
	for _, r := range res.rows {
		if r.PackageName != "nginx" || r.Architecture != "amd64" {
			t.Errorf("row has unexpected (Package, Arch): %q/%q", r.PackageName, r.Architecture)
		}
		seen[r.Path] = r.DeclaredSHA256
	}
	if seen["/pool/main/n/nginx/nginx_1.0_amd64.deb"] != debHashA {
		t.Errorf("missing/wrong v1 row: %v", seen)
	}
	if seen["/pool/main/n/nginx/nginx_2.0_amd64.deb"] != debHashB {
		t.Errorf("missing/wrong v2 row: %v", seen)
	}
}

// TestBuildPackageHashes_VersionlessStanzaKeptCoverageIntact: a binary
// Packages stanza without Version: (rare/odd index) must still produce a
// package_hash row (version=”) and must NOT downgrade the snapshot's
// coverage — one odd stanza can't degrade the whole suite's strict-mode
// posture or evict its still-valid blob. The version=” row is retained by
// the §3 current-snapshot guard-a fallback (the proven pre-version behavior).
func TestBuildPackageHashes_VersionlessStanzaKeptCoverageIntact(t *testing.T) {
	dir := t.TempDir()
	c, err := cache.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	defer func() { _ = c.Close() }()
	a := &Adopter{cache: c, logger: slog.Default()}

	withVer := strings.Repeat("a", 64)
	noVer := strings.Repeat("b", 64)
	pkgs := []byte(
		"Package: nginx\nArchitecture: amd64\nVersion: 1.0\n" +
			"Filename: pool/main/n/nginx/nginx_1.0_amd64.deb\n" +
			"Size: 1\nSHA256: " + withVer + "\n\n" +
			"Package: nginx\nArchitecture: amd64\n" + // no Version:
			"Filename: pool/main/n/nginx/nginx_2.0_amd64.deb\n" +
			"Size: 1\nSHA256: " + noVer + "\n",
	)
	pkgsBlob := writeFixtureBlob(t, c, pkgs)
	suiteRef := SuiteRef{CanonicalScheme: "http", CanonicalHost: "archive.example", SuitePath: "/dists/noble"}
	members := []ReleaseMember{{Path: "main/binary-amd64/Packages", SHA256: pkgsBlob, Size: int64(len(pkgs))}}

	res, err := a.buildPackageHashes(suiteRef, 1, members, members)
	if err != nil {
		t.Fatalf("buildPackageHashes: %v", err)
	}
	if len(res.rows) != 2 {
		t.Fatalf("got %d rows, want 2 (versionless stanza kept, not skipped)", len(res.rows))
	}
	byVer := map[string]bool{}
	for _, r := range res.rows {
		byVer[r.Version] = true
	}
	if !byVer["1.0"] || !byVer[""] {
		t.Errorf("expected rows with version 1.0 and '' (kept), got versions %v", byVer)
	}
	if !res.coverageComplete {
		t.Error("coverageComplete = false, want true (a single versionless stanza must not flip coverage)")
	}
}

// hotFakeFetcher extends the existing fakeFetcher with per-URL
// blocking simulation so budget-elapse tests can deterministically
// trigger ctx.Done.
type hotFakeFetcher struct {
	mu       sync.Mutex
	bodies   map[string][]byte
	errs     map[string]error
	blocking map[string]time.Duration
}

func newHotFakeFetcher() *hotFakeFetcher {
	return &hotFakeFetcher{
		bodies:   make(map[string][]byte),
		errs:     make(map[string]error),
		blocking: make(map[string]time.Duration),
	}
}

func (f *hotFakeFetcher) setBody(url string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bodies[url] = body
}

func (f *hotFakeFetcher) setError(url string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[url] = err
}

func (f *hotFakeFetcher) setBlocking(url string, dur time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blocking[url] = dur
}

func (f *hotFakeFetcher) Fetch(ctx context.Context, target *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error) {
	f.mu.Lock()
	body, hasBody := f.bodies[target.URL]
	errInj, hasErr := f.errs[target.URL]
	blockDur, hasBlock := f.blocking[target.URL]
	f.mu.Unlock()

	if hasBlock {
		select {
		case <-time.After(blockDur):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if hasErr {
		return nil, errInj
	}
	if !hasBody {
		return nil, fmt.Errorf("hotFakeFetcher: no canned response for %s", target.URL)
	}
	if _, err := dst.Write(body); err != nil {
		return nil, err
	}
	return &fetch.FetchResult{
		Status:        200,
		ContentLength: int64(len(body)),
	}, nil
}

// writeFixtureBlob persists bytes through cache.NewTempBlob and
// returns the sha256 hash. Mirrors the test seed pattern used in
// internal/cache/cache_test.go.
func writeFixtureBlob(t *testing.T, c *cache.Cache, content []byte) string {
	t.Helper()
	w, err := c.NewTempBlob()
	if err != nil {
		t.Fatalf("NewTempBlob: %v", err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatalf("Write: %v", err)
	}
	hash, err := w.Finalize(int64(len(content)))
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if err := c.PutBlob(context.Background(), hash, int64(len(content))); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	return hash
}
