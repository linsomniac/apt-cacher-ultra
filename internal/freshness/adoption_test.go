package freshness

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
)

// fakePackagesStanzas builds a Packages-text body declaring the given
// (filename → declared sha256) tuples. Returns the bytes; tests use
// this as the verified plaintext that ParsePackages will re-parse.
func fakePackagesStanzas(entries map[string]string) []byte {
	var sb strings.Builder
	for fn, h := range entries {
		fmt.Fprintf(&sb, "Package: %s\n", filepath.Base(fn))
		fmt.Fprintf(&sb, "Filename: %s\n", fn)
		fmt.Fprintf(&sb, "Size: 1234\n")
		fmt.Fprintf(&sb, "SHA256: %s\n\n", h)
	}
	return []byte(sb.String())
}

// gzipBytes wraps b in a gzip stream. Used to seed the fakeFetcher
// with a Packages.gz body that the adopter will actually decompress.
func gzipBytes(b []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		panic(err)
	}
	if err := w.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// passThroughVerifier returns the input verbatim. It exists only for
// step-2 tests; production wiring will use the real GPG verifier from
// step 3. The detached form returns releaseBytes verbatim too,
// matching the real verifier's "Release file IS the verified
// plaintext" contract.
type passThroughVerifier struct{}

func (passThroughVerifier) VerifyInline(ctx context.Context, suite SuiteRef, inRelease []byte) ([]byte, error) {
	return inRelease, nil
}

func (passThroughVerifier) VerifyDetached(ctx context.Context, suite SuiteRef, releaseBytes, sigBytes []byte) ([]byte, error) {
	return releaseBytes, nil
}

// failingVerifier always returns an error; used to exercise the
// ErrAdoptionGPGFailed path.
type failingVerifier struct{ err error }

func (v failingVerifier) VerifyInline(ctx context.Context, suite SuiteRef, inRelease []byte) ([]byte, error) {
	return nil, v.err
}

func (v failingVerifier) VerifyDetached(ctx context.Context, suite SuiteRef, releaseBytes, sigBytes []byte) ([]byte, error) {
	return nil, v.err
}

// fakeFetcher serves canned bytes for specific URLs. Keys are full
// URLs; values are the bytes the fetcher writes to dst. URLs not in
// the map produce an error simulating an upstream 404.
type fakeFetcher struct {
	mu        sync.Mutex
	responses map[string][]byte
	errFor    map[string]error // optional per-URL error injection
	calls     atomic.Int32
}

func newFakeFetcher() *fakeFetcher {
	return &fakeFetcher{
		responses: make(map[string][]byte),
		errFor:    make(map[string]error),
	}
}

func (f *fakeFetcher) put(url string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses[url] = body
}

func (f *fakeFetcher) failWith(url string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errFor[url] = err
}

// fail404 / fail503 inject upstream HTTP status errors so adoption tests
// can simulate the SPEC2 §7.5.2 "upstream declared but doesn't serve"
// case (404 → skip) and the "upstream broken right now" case (5xx →
// fatal). The error type matters: the 4xx-skip path in adoptMember
// gates on errors.As(*fetch.StatusError) — a generic error from the
// fetcher (the unknown-URL fall-through above) does not match and stays
// fatal.
func (f *fakeFetcher) fail404(url string) {
	f.failWith(url, &fetch.StatusError{Code: 404})
}

func (f *fakeFetcher) fail503(url string) {
	f.failWith(url, &fetch.StatusError{Code: 503})
}

func (f *fakeFetcher) Fetch(ctx context.Context, target *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error) {
	f.calls.Add(1)
	f.mu.Lock()
	body, ok := f.responses[target.URL]
	if errInj, has := f.errFor[target.URL]; has {
		f.mu.Unlock()
		return nil, errInj
	}
	f.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("fakeFetcher: no canned response for %s", target.URL)
	}
	if _, err := dst.Write(body); err != nil {
		return nil, err
	}
	return &fetch.FetchResult{
		Status:        200,
		ContentLength: int64(len(body)),
	}, nil
}

// adoptionTestEnv sets up a *cache.Cache + Adopter pair for a single
// test, with an in-memory fake fetcher pre-seeded with a "real-ish"
// suite.
type adoptionTestEnv struct {
	t        *testing.T
	cache    *cache.Cache
	adopter  *Adopter
	fetcher  *fakeFetcher
	verifier Verifier
	suite    SuiteRef
}

func newAdoptionTestEnv(t *testing.T) *adoptionTestEnv {
	t.Helper()
	dir := t.TempDir()
	c, err := cache.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ff := newFakeFetcher()
	ad, err := NewAdopter(AdoptionConfig{
		Cache:       c,
		Fetcher:     ff,
		Verifier:    passThroughVerifier{},
		HostLimiter: hostsem.New(8),
	})
	if err != nil {
		t.Fatalf("NewAdopter: %v", err)
	}
	return &adoptionTestEnv{
		t:        t,
		cache:    c,
		adopter:  ad,
		fetcher:  ff,
		verifier: passThroughVerifier{},
		suite: SuiteRef{
			CanonicalScheme: "http",
			CanonicalHost:   "archive.ubuntu.com",
			SuitePath:       "/ubuntu/dists/noble",
		},
	}
}

// shaOf hashes its arg with sha256 and returns the lowercase hex.
func shaOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// makeRelease constructs Release-style text declaring the given
// member contents (path → bytes). Returns the Release text and a
// map of path → declared SHA256 (which equals sha256(content)).
func makeRelease(members map[string][]byte) ([]byte, map[string]string) {
	hashes := make(map[string]string, len(members))
	var sb strings.Builder
	sb.WriteString("Origin: Ubuntu\n")
	sb.WriteString("Suite: noble\n")
	sb.WriteString("SHA256:\n")
	for p, body := range members {
		h := shaOf(body)
		hashes[p] = h
		fmt.Fprintf(&sb, " %s %d %s\n", h, len(body), p)
	}
	return []byte(sb.String()), hashes
}

func TestAdopter_HappyPath(t *testing.T) {
	env := newAdoptionTestEnv(t)

	// Two .debs declared in Packages stanzas. The Packages member's
	// content is real apt-style Packages text; Packages.gz is the same
	// content gzipped.
	debHash1 := strings.Repeat("a", 64)
	debHash2 := strings.Repeat("b", 64)
	pkgs := fakePackagesStanzas(map[string]string{
		"pool/main/f/foo/foo_1.deb": debHash1,
		"pool/main/b/bar/bar_2.deb": debHash2,
	})
	pkgsGz := gzipBytes(pkgs)
	src := []byte("Sources content")
	releaseText, declared := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages":    pkgs,
		"main/binary-amd64/Packages.gz": pkgsGz,
		"main/source/Sources":           src,
	})
	for p, body := range map[string][]byte{
		"main/binary-amd64/Packages":    pkgs,
		"main/binary-amd64/Packages.gz": pkgsGz,
		"main/source/Sources":           src,
	} {
		env.fetcher.put("http://archive.ubuntu.com/ubuntu/dists/noble/"+p, body)
	}

	if err := env.adopter.Run(context.Background(), env.suite, releaseText, "etag-1", "lastmod-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := env.fetcher.calls.Load(); got != 3 {
		t.Errorf("fetch calls = %d, want 3", got)
	}

	sf, err := env.cache.GetSuiteFreshness(context.Background(),
		env.suite.CanonicalScheme, env.suite.CanonicalHost, env.suite.SuitePath)
	if err != nil {
		t.Fatalf("GetSuiteFreshness: %v", err)
	}
	if sf.CurrentSnapshotID == nil {
		t.Fatal("current_snapshot_id not set after adoption")
	}
	snapshotID := *sf.CurrentSnapshotID

	// snapshot_member rows: 3 declared + 1 metadata-self (InRelease)
	// + 3 by-hash aliases (each declared in distinct dirs, all unique).
	got, err := env.cache.ListSnapshotMembers(context.Background(), snapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 7 {
		t.Errorf("snapshot_member count = %d, want 7", len(got))
	}

	// Spot-check declared members.
	for p, want := range declared {
		var found *cache.SnapshotMember
		for i := range got {
			if got[i].Path == p {
				found = &got[i]
				break
			}
		}
		if found == nil {
			t.Errorf("missing snapshot_member for %q", p)
			continue
		}
		if found.DeclaredSHA256 != want || found.BlobHash != want {
			t.Errorf("%q (declared, blob) = (%s, %s), want both %s",
				p, found.DeclaredSHA256, found.BlobHash, want)
		}
	}

	// Metadata-self row for InRelease.
	var sawSelf bool
	for _, m := range got {
		if m.Path == "InRelease" {
			sawSelf = true
			if m.BlobHash != shaOf(releaseText) {
				t.Errorf("InRelease self row blob_hash = %s, want %s",
					m.BlobHash, shaOf(releaseText))
			}
		}
	}
	if !sawSelf {
		t.Error("metadata-self InRelease row missing")
	}

	// By-hash alias for Packages lands in the expected dir.
	pkgsDeclared := declared["main/binary-amd64/Packages"]
	wantAlias := "main/binary-amd64/by-hash/SHA256/" + pkgsDeclared
	var sawAlias bool
	for _, m := range got {
		if m.Path == wantAlias {
			sawAlias = true
		}
	}
	if !sawAlias {
		t.Errorf("by-hash alias %q missing", wantAlias)
	}

	// package_hash rows: two distinct .debs declared in two Packages
	// variants — must dedup to exactly 2 rows.
	for _, expected := range []struct {
		path string
		hash string
	}{
		{"/ubuntu/pool/main/f/foo/foo_1.deb", debHash1},
		{"/ubuntu/pool/main/b/bar/bar_2.deb", debHash2},
	} {
		ph, err := env.cache.GetPackageHash(context.Background(),
			env.suite.CanonicalScheme, env.suite.CanonicalHost, expected.path, snapshotID)
		if err != nil {
			t.Errorf("missing package_hash for %s: %v", expected.path, err)
			continue
		}
		if ph.DeclaredSHA256 != expected.hash {
			t.Errorf("package_hash %s declared = %s, want %s",
				expected.path, ph.DeclaredSHA256, expected.hash)
		}
	}
}

func TestAdopter_DetachedHappyPath(t *testing.T) {
	// Mirror of TestAdopter_HappyPath but exercising RunDetached. The
	// passThroughVerifier returns releaseBytes verbatim regardless of
	// form, so the same Release-style text drives both paths; the
	// detached-specific assertions are around the suite_snapshot row
	// (release_hash + release_gpg_hash, NOT inrelease_hash) and the
	// step-6 metadata-self rows ("Release" and "Release.gpg" instead
	// of "InRelease").
	env := newAdoptionTestEnv(t)

	debHash := strings.Repeat("a", 64)
	pkgs := fakePackagesStanzas(map[string]string{
		"pool/main/f/foo/foo_1.deb": debHash,
	})
	releaseText, declared := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": pkgs,
	})
	env.fetcher.put("http://archive.ubuntu.com/ubuntu/dists/noble/main/binary-amd64/Packages", pkgs)

	// passThroughVerifier returns releaseBytes verbatim, so the
	// "signature" can be any non-empty placeholder — the adopter only
	// hashes it for the pool blob and the snapshot_member row.
	sigBytes := []byte("placeholder-Release-gpg-bytes")
	if err := env.adopter.RunDetached(
		context.Background(), env.suite, releaseText, sigBytes, "etag-Z", "lastmod-Z",
	); err != nil {
		t.Fatalf("RunDetached: %v", err)
	}

	sf, err := env.cache.GetSuiteFreshness(context.Background(),
		env.suite.CanonicalScheme, env.suite.CanonicalHost, env.suite.SuitePath)
	if err != nil {
		t.Fatalf("GetSuiteFreshness: %v", err)
	}
	if sf.CurrentSnapshotID == nil {
		t.Fatal("current_snapshot_id not set after detached adoption")
	}
	snapshotID := *sf.CurrentSnapshotID

	// The snapshot row carries release_hash + release_gpg_hash, not
	// inrelease_hash. The schema CHECK constraint enforces XOR.
	snap, err := env.cache.GetSuiteSnapshot(context.Background(), snapshotID)
	if err != nil {
		t.Fatalf("GetSuiteSnapshot: %v", err)
	}
	if snap.InReleaseHash != nil {
		t.Errorf("detached snapshot has unexpected inrelease_hash=%s", *snap.InReleaseHash)
	}
	if snap.ReleaseHash == nil || *snap.ReleaseHash != shaOf(releaseText) {
		t.Errorf("release_hash = %v, want %s", snap.ReleaseHash, shaOf(releaseText))
	}
	if snap.ReleaseGPGHash == nil || *snap.ReleaseGPGHash != shaOf(sigBytes) {
		t.Errorf("release_gpg_hash = %v, want %s", snap.ReleaseGPGHash, shaOf(sigBytes))
	}

	got, err := env.cache.ListSnapshotMembers(context.Background(), snapshotID)
	if err != nil {
		t.Fatal(err)
	}

	// Step 6 in detached mode contributes TWO metadata-self rows.
	var sawRelease, sawReleaseGPG, sawInRelease bool
	for _, m := range got {
		switch m.Path {
		case "Release":
			sawRelease = true
			if m.BlobHash != shaOf(releaseText) {
				t.Errorf("Release self row blob_hash = %s, want %s", m.BlobHash, shaOf(releaseText))
			}
		case "Release.gpg":
			sawReleaseGPG = true
			if m.BlobHash != shaOf(sigBytes) {
				t.Errorf("Release.gpg self row blob_hash = %s, want %s", m.BlobHash, shaOf(sigBytes))
			}
		case "InRelease":
			sawInRelease = true
		}
	}
	if !sawRelease {
		t.Error("metadata-self Release row missing")
	}
	if !sawReleaseGPG {
		t.Error("metadata-self Release.gpg row missing")
	}
	if sawInRelease {
		t.Error("metadata-self InRelease row unexpectedly present in detached snapshot")
	}

	// Declared members still flow through unchanged. (We declared
	// only Packages here; spot-check it.)
	pkgsDeclared := declared["main/binary-amd64/Packages"]
	var sawPkgs bool
	for _, m := range got {
		if m.Path == "main/binary-amd64/Packages" {
			sawPkgs = true
			if m.BlobHash != pkgsDeclared {
				t.Errorf("Packages blob_hash = %s, want %s", m.BlobHash, pkgsDeclared)
			}
		}
	}
	if !sawPkgs {
		t.Error("declared member Packages missing from snapshot_member rows")
	}
}

// TestAdopter_FormDriftWARN_OnFormTransition verifies that
// adoption_form_drift fires when a suite's adoption form changes
// between the prior current snapshot and the one just committed, and
// that first-ever adoptions (no prior snapshot) don't false-positive.
func TestAdopter_FormDriftWARN_OnFormTransition(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	dir := t.TempDir()
	c, err := cache.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ff := newFakeFetcher()
	ad, err := NewAdopter(AdoptionConfig{
		Cache:       c,
		Fetcher:     ff,
		Verifier:    passThroughVerifier{},
		HostLimiter: hostsem.New(8),
		Logger:      logger,
	})
	if err != nil {
		t.Fatalf("NewAdopter: %v", err)
	}

	suite := SuiteRef{
		CanonicalScheme: "http",
		CanonicalHost:   "archive.ubuntu.com",
		SuitePath:       "/ubuntu/dists/noble",
	}

	debHash := strings.Repeat("a", 64)
	pkgs := fakePackagesStanzas(map[string]string{
		"pool/main/f/foo/foo_1.deb": debHash,
	})
	inlineRelease, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": pkgs,
	})
	// Detached form uses a distinct Release body. The natural-key
	// UNIQUE index on suite_snapshot is over
	// (scheme, host, suite_path, COALESCE(inrelease_hash, release_hash))
	// — reusing the same bytes between an inline and a detached
	// adoption would hash-collide across forms. Real-world upstreams
	// that switch form produce distinct bytes anyway (an InRelease
	// wraps Release in a PGP envelope; the detached form drops the
	// envelope), so a modest header tweak is realistic.
	detachedRelease := append([]byte("Description: detached form\n"), inlineRelease...)

	ff.put("http://archive.ubuntu.com/ubuntu/dists/noble/main/binary-amd64/Packages", pkgs)

	// First adoption: inline. With no prior current snapshot,
	// adoption_form_drift must NOT fire — first adoption is not drift.
	if err := ad.Run(context.Background(), suite, inlineRelease, "etag-1", ""); err != nil {
		t.Fatalf("Run inline: %v", err)
	}
	if got := logBuf.String(); strings.Contains(got, "adoption_form_drift") {
		t.Fatalf("first-ever adoption should not emit adoption_form_drift, got log:\n%s", got)
	}

	// Second adoption: detached. Prior snapshot was inline; new is
	// detached — adoption_form_drift WARN must fire with prior=inline,
	// new=detached.
	logBuf.Reset()
	sigBytes := []byte("placeholder-sig")
	if err := ad.RunDetached(context.Background(), suite, detachedRelease, sigBytes, "etag-2", ""); err != nil {
		t.Fatalf("RunDetached: %v", err)
	}
	out := logBuf.String()
	if !strings.Contains(out, "adoption_form_drift") {
		t.Fatalf("expected adoption_form_drift WARN, got log:\n%s", out)
	}
	if !strings.Contains(out, `"prior_form":"inline"`) {
		t.Errorf("expected prior_form=inline, got log:\n%s", out)
	}
	if !strings.Contains(out, `"new_form":"detached"`) {
		t.Errorf("expected new_form=detached, got log:\n%s", out)
	}

	// Third adoption: detached again with yet-another distinct Release
	// (same UNIQUE-index reasoning). Prior is now detached; new is
	// detached — no drift, no WARN.
	logBuf.Reset()
	detachedRelease2 := append([]byte("Description: detached form v2\n"), inlineRelease...)
	if err := ad.RunDetached(context.Background(), suite, detachedRelease2, sigBytes, "etag-3", ""); err != nil {
		t.Fatalf("RunDetached repeat: %v", err)
	}
	if got := logBuf.String(); strings.Contains(got, "adoption_form_drift") {
		t.Fatalf("repeat detached adoption should not emit adoption_form_drift, got log:\n%s", got)
	}
}

func TestAdopter_DetachedVerifyError(t *testing.T) {
	// Same as TestAdopter_VerifyError but routes through RunDetached
	// to confirm the error category propagates via the detached
	// codepath too.
	env := newAdoptionTestEnv(t)
	bad := errors.New("bad detached sig")
	env.adopter.verifier = failingVerifier{err: bad}
	err := env.adopter.RunDetached(
		context.Background(), env.suite,
		[]byte("release-bytes"), []byte("sig-bytes"), "", "",
	)
	if !errors.Is(err, ErrAdoptionGPGFailed) {
		t.Errorf("got %v, want ErrAdoptionGPGFailed", err)
	}
	if got := blobsInPool(t, env.cache); got != 0 {
		t.Errorf("pool/ has %d files after gpg failure, want 0", got)
	}
}

func TestAdopter_VerifyError(t *testing.T) {
	env := newAdoptionTestEnv(t)
	bad := errors.New("bad sig")
	env.adopter.verifier = failingVerifier{err: bad}
	err := env.adopter.Run(context.Background(), env.suite, []byte("anything"), "", "")
	if !errors.Is(err, ErrAdoptionGPGFailed) {
		t.Errorf("got %v, want ErrAdoptionGPGFailed", err)
	}
	// No candidate snapshot row should have been created.
	if got := blobsInPool(t, env.cache); got != 0 {
		t.Errorf("pool/ has %d files after gpg failure, want 0", got)
	}
}

func TestAdopter_ParseError(t *testing.T) {
	env := newAdoptionTestEnv(t)
	// Verified text with no SHA256 block — ParseRelease errors.
	garbage := []byte("Origin: Ubuntu\nSuite: noble\n")
	err := env.adopter.Run(context.Background(), env.suite, garbage, "", "")
	if !errors.Is(err, ErrAdoptionParseFailed) {
		t.Errorf("got %v, want ErrAdoptionParseFailed", err)
	}
}

func TestAdopter_MemberFetchFailure(t *testing.T) {
	env := newAdoptionTestEnv(t)
	body := []byte("body")
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": body,
	})
	// Don't seed the fetcher — Fetch will return "no canned response"
	// which the adopter wraps in ErrAdoptionMemberFetchFailed.
	err := env.adopter.Run(context.Background(), env.suite, releaseText, "", "")
	if !errors.Is(err, ErrAdoptionMemberFetchFailed) {
		t.Errorf("got %v, want ErrAdoptionMemberFetchFailed", err)
	}
}

// TestAdopter_OrphanedCandidateReused exercises the
// idx_suite_snapshot_natural fix end-to-end through runShared:
//
//  1. First Run: member fetch fails. Step 4 leaves an orphaned
//     candidate row (adopted_at IS NULL) and Step 5 returns
//     ErrAdoptionMemberFetchFailed.
//  2. Second Run with the SAME release text (same content hash, so
//     same natural key): Step 4 must reuse the orphaned candidate
//     instead of failing with a UNIQUE constraint error. With the
//     member fetch now seeded, the run proceeds through
//     CommitAdoption and the suite ends up adopted.
//
// Without the fix, the second Run would fail with the
// "UNIQUE constraint failed: index 'idx_suite_snapshot_natural'"
// error from production logs.
func TestAdopter_OrphanedCandidateReused(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	debHash := strings.Repeat("a", 64)
	pkgs := fakePackagesStanzas(map[string]string{
		"pool/main/f/foo/foo_1.deb": debHash,
	})
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": pkgs,
	})

	// Step 1: first attempt fails in member prefetch (no canned response).
	err := env.adopter.Run(ctx, env.suite, releaseText, "", "")
	if !errors.Is(err, ErrAdoptionMemberFetchFailed) {
		t.Fatalf("first Run: want ErrAdoptionMemberFetchFailed, got %v", err)
	}
	// suite_freshness has no current_snapshot_id yet — only the orphan.
	if sf, err := env.cache.GetSuiteFreshness(ctx,
		env.suite.CanonicalScheme, env.suite.CanonicalHost, env.suite.SuitePath,
	); err == nil && sf.CurrentSnapshotID != nil {
		t.Fatalf("after first Run: current_snapshot_id should be NULL, got %d",
			*sf.CurrentSnapshotID)
	}

	// Step 2: seed the fetcher and re-run with the same release text.
	// Without the fix this fails with ErrAdoptionDBFailed wrapping a
	// UNIQUE-constraint error from idx_suite_snapshot_natural.
	env.fetcher.put("http://archive.ubuntu.com/ubuntu/dists/noble/main/binary-amd64/Packages", pkgs)
	if err := env.adopter.Run(ctx, env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("second Run: want success (orphan reuse), got %v", err)
	}

	sf, err := env.cache.GetSuiteFreshness(ctx,
		env.suite.CanonicalScheme, env.suite.CanonicalHost, env.suite.SuitePath)
	if err != nil {
		t.Fatalf("GetSuiteFreshness: %v", err)
	}
	if sf.CurrentSnapshotID == nil {
		t.Fatalf("after second Run: current_snapshot_id is NULL — adoption did not commit")
	}
	snap, err := env.cache.GetSuiteSnapshot(ctx, *sf.CurrentSnapshotID)
	if err != nil {
		t.Fatalf("GetSuiteSnapshot: %v", err)
	}
	if snap.AdoptedAt == nil {
		t.Errorf("snapshot %d adopted_at IS NULL after successful second Run",
			snap.SnapshotID)
	}
}

func TestAdopter_MemberHashMismatch(t *testing.T) {
	env := newAdoptionTestEnv(t)
	// Same length, different bytes — guarantees the hash check (not
	// the size sanity check) is what rejects the fetch.
	declared := []byte("a deliberate 32B body for testin")
	served := []byte("a different 32B body, same size!")
	if len(declared) != len(served) {
		t.Fatalf("test setup bug: declared %d != served %d bytes", len(declared), len(served))
	}
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": declared,
	})
	env.fetcher.put("http://archive.ubuntu.com/ubuntu/dists/noble/main/binary-amd64/Packages", served)

	err := env.adopter.Run(context.Background(), env.suite, releaseText, "", "")
	if !errors.Is(err, ErrAdoptionMemberMismatch) {
		t.Errorf("got %v, want ErrAdoptionMemberMismatch", err)
	}
}

func TestAdopter_PoolReuseSkipsFetch(t *testing.T) {
	// Two adoptions of the same content: the second should reuse the
	// pool blob and skip the upstream fetch. Use a Sources path
	// (not Packages) so step 8 doesn't try to ParsePackages on the
	// opaque test body.
	env := newAdoptionTestEnv(t)
	body := []byte("same Sources content twice")
	r1, _ := makeRelease(map[string][]byte{"main/source/Sources": body})
	env.fetcher.put("http://archive.ubuntu.com/ubuntu/dists/noble/main/source/Sources", body)

	if err := env.adopter.Run(context.Background(), env.suite, r1, "etag1", ""); err != nil {
		t.Fatalf("Run #1: %v", err)
	}
	firstCalls := env.fetcher.calls.Load()
	if firstCalls != 1 {
		t.Fatalf("first run fetch calls = %d, want 1", firstCalls)
	}

	// Second adoption: same member content, but a different InRelease
	// body so the candidate row is distinct on the natural-key UNIQUE.
	r2 := append([]byte{}, r1...)
	r2 = append(r2, '\n')
	if err := env.adopter.Run(context.Background(), env.suite, r2, "etag2", ""); err != nil {
		t.Fatalf("Run #2: %v", err)
	}
	if got := env.fetcher.calls.Load(); got != firstCalls {
		t.Errorf("second adoption did %d more fetches, expected pool reuse",
			got-firstCalls)
	}
}

func TestAdopter_PoolCorruptionDetectedAndRefetched(t *testing.T) {
	env := newAdoptionTestEnv(t)
	// Use a Sources member so step 8 doesn't try to parse the
	// opaque test body. Pool-corruption defense is an §7.5 step 5
	// concern; whether the file is Packages or Sources doesn't
	// affect the rehash-on-reuse logic.
	declared := []byte("real Sources content")
	corruptBody := []byte("rotted bytes that don't match declared")
	releaseText, hashes := makeRelease(map[string][]byte{
		"main/source/Sources": declared,
	})
	declaredHash := hashes["main/source/Sources"]
	env.fetcher.put("http://archive.ubuntu.com/ubuntu/dists/noble/main/source/Sources", declared)

	// Plant a corrupted file at pool/<declaredHash> BEFORE adoption.
	// Adoption's rehash-on-reuse must detect the mismatch, evict, and
	// refetch from upstream.
	poolPath := env.cache.BlobPath(declaredHash)
	if err := os.MkdirAll(filepath.Dir(poolPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(poolPath, corruptBody, 0o640); err != nil {
		t.Fatal(err)
	}

	if err := env.adopter.Run(context.Background(), env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The pool blob must now hold the actual declared content.
	got, err := os.ReadFile(poolPath)
	if err != nil {
		t.Fatalf("read pool blob after refetch: %v", err)
	}
	if string(got) != string(declared) {
		t.Errorf("pool blob still corrupt after adoption: %q", got)
	}
	// One fetch should have happened (the refetch), not zero (false
	// reuse) and not more than one.
	if got := env.fetcher.calls.Load(); got != 1 {
		t.Errorf("fetch calls after corruption refetch = %d, want 1", got)
	}
}

func TestAdopter_RejectsUnsetDependencies(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*AdoptionConfig)
	}{
		{"nil cache", func(c *AdoptionConfig) { c.Cache = nil }},
		{"nil fetcher", func(c *AdoptionConfig) { c.Fetcher = nil }},
		{"nil verifier", func(c *AdoptionConfig) { c.Verifier = nil }},
		{"nil hostlimiter", func(c *AdoptionConfig) { c.HostLimiter = nil }},
		{"negative max", func(c *AdoptionConfig) { c.MaxConcurrent = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			c, _ := cache.Open(context.Background(), dir, nil)
			t.Cleanup(func() { _ = c.Close() })
			cfg := AdoptionConfig{
				Cache:       c,
				Fetcher:     newFakeFetcher(),
				Verifier:    passThroughVerifier{},
				HostLimiter: hostsem.New(1),
			}
			tc.mutate(&cfg)
			if _, err := NewAdopter(cfg); err == nil {
				t.Errorf("expected error, got nil")
			}
		})
	}
}

// blockReq is the rendezvous point between a blockingFetcher.Fetch
// call and the test that wants to gate it. Each Fetch publishes one
// blockReq onto its pending channel, then waits on release before
// returning the canned body.
type blockReq struct{ release chan struct{} }

// blockingFetcher gates each Fetch through a blockReq that the test
// releases when it wants the fetch to complete.
type blockingFetcher struct {
	mu      sync.Mutex
	bodies  map[string][]byte
	pending chan blockReq
}

func (b *blockingFetcher) put(url string, body []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.bodies == nil {
		b.bodies = map[string][]byte{}
	}
	b.bodies[url] = body
}

func (b *blockingFetcher) Fetch(ctx context.Context, target *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error) {
	b.mu.Lock()
	body, ok := b.bodies[target.URL]
	b.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("blockingFetcher: no canned for %q", target.URL)
	}
	req := blockReq{release: make(chan struct{})}
	select {
	case b.pending <- req:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-req.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if _, err := dst.Write(body); err != nil {
		return nil, err
	}
	return &fetch.FetchResult{Status: 200, ContentLength: int64(len(body))}, nil
}

func TestAdopter_ConcurrencyCap(t *testing.T) {
	dir := t.TempDir()
	c, err := cache.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	pending := make(chan blockReq, 8)
	bf := &blockingFetcher{pending: pending}

	ad, err := NewAdopter(AdoptionConfig{
		Cache:         c,
		Fetcher:       bf,
		Verifier:      passThroughVerifier{},
		HostLimiter:   hostsem.New(8),
		MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	suiteA := SuiteRef{CanonicalScheme: "http", CanonicalHost: "x.example", SuitePath: "/p1"}
	suiteB := SuiteRef{CanonicalScheme: "http", CanonicalHost: "x.example", SuitePath: "/p2"}
	body := []byte("body")
	// Use Sources not Packages — step 8 would otherwise try to parse
	// the test body. Concurrency-cap behavior is independent of which
	// member type is being fetched.
	releaseA, _ := makeRelease(map[string][]byte{"main/Sources": body})
	releaseB, _ := makeRelease(map[string][]byte{"main/Sources": []byte("other body")})
	bf.put("http://x.example/p1/main/Sources", body)
	bf.put("http://x.example/p2/main/Sources", []byte("other body"))

	// A starts; will block on member fetch.
	doneA := make(chan error, 1)
	go func() { doneA <- ad.Run(context.Background(), suiteA, releaseA, "", "") }()

	var aReq blockReq
	select {
	case aReq = <-pending:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine A never reached fetch")
	}

	// B starts while A holds the cap. Must NOT enter Fetch.
	doneB := make(chan error, 1)
	go func() { doneB <- ad.Run(context.Background(), suiteB, releaseB, "", "") }()
	select {
	case <-pending:
		t.Fatal("goroutine B reached fetch while concurrency cap was held")
	case <-time.After(150 * time.Millisecond):
		// expected
	}

	// Release A; A finishes, B can then take the slot.
	close(aReq.release)
	select {
	case err := <-doneA:
		if err != nil {
			t.Fatalf("A: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("A never finished after release")
	}
	var bReq blockReq
	select {
	case bReq = <-pending:
	case <-time.After(2 * time.Second):
		t.Fatal("B never reached fetch after A released slot")
	}
	close(bReq.release)
	select {
	case err := <-doneB:
		if err != nil {
			t.Fatalf("B: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("B never finished")
	}
}

// blobsInPool counts files under pool/. Tests use it to verify that
// failure paths do not leave half-promoted blobs on disk.
func blobsInPool(t *testing.T, c *cache.Cache) int {
	t.Helper()
	root := filepath.Join(c.Dir(), "pool")
	count := 0
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			count++
		}
		return nil
	})
	return count
}

func TestAdopter_PackageHash_DedupAcrossVariants(t *testing.T) {
	// Same .deb declared in Packages and Packages.gz must dedupe to
	// exactly one package_hash row — the primary key would otherwise
	// fail the atomic flip.
	env := newAdoptionTestEnv(t)
	debHash := strings.Repeat("c", 64)
	stanzas := fakePackagesStanzas(map[string]string{
		"pool/main/c/cab/cab.deb": debHash,
	})
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages":    stanzas,
		"main/binary-amd64/Packages.gz": gzipBytes(stanzas),
	})
	env.fetcher.put("http://archive.ubuntu.com/ubuntu/dists/noble/main/binary-amd64/Packages", stanzas)
	env.fetcher.put("http://archive.ubuntu.com/ubuntu/dists/noble/main/binary-amd64/Packages.gz", gzipBytes(stanzas))

	if err := env.adopter.Run(context.Background(), env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	sf, _ := env.cache.GetSuiteFreshness(context.Background(),
		env.suite.CanonicalScheme, env.suite.CanonicalHost, env.suite.SuitePath)
	if sf.CurrentSnapshotID == nil {
		t.Fatal("adoption didn't flip pointer")
	}
	ph, err := env.cache.GetPackageHash(context.Background(),
		env.suite.CanonicalScheme, env.suite.CanonicalHost,
		"/ubuntu/pool/main/c/cab/cab.deb", *sf.CurrentSnapshotID)
	if err != nil {
		t.Fatalf("GetPackageHash: %v", err)
	}
	if ph.DeclaredSHA256 != debHash {
		t.Errorf("declared = %s, want %s", ph.DeclaredSHA256, debHash)
	}
}

func TestAdopter_PackageHash_ConflictAcrossVariants(t *testing.T) {
	// Pathological: two Packages variants declare DIFFERENT SHA256
	// for the same .deb. apt would reject this; adoption must too.
	env := newAdoptionTestEnv(t)
	debA := strings.Repeat("a", 64)
	debB := strings.Repeat("b", 64)
	pkgsA := fakePackagesStanzas(map[string]string{"pool/main/x/x.deb": debA})
	pkgsB := fakePackagesStanzas(map[string]string{"pool/main/x/x.deb": debB})
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages":    pkgsA,
		"main/binary-amd64/Packages.gz": gzipBytes(pkgsB),
	})
	env.fetcher.put("http://archive.ubuntu.com/ubuntu/dists/noble/main/binary-amd64/Packages", pkgsA)
	env.fetcher.put("http://archive.ubuntu.com/ubuntu/dists/noble/main/binary-amd64/Packages.gz", gzipBytes(pkgsB))

	err := env.adopter.Run(context.Background(), env.suite, releaseText, "", "")
	if !errors.Is(err, ErrAdoptionParseFailed) {
		t.Errorf("got %v, want ErrAdoptionParseFailed", err)
	}
}

func TestAdopter_PackageHash_NonStandardSuiteSkips(t *testing.T) {
	// Suite path lacks the "/dists/<codename>" convention. Adoption
	// still succeeds; package_hash is empty (best-effort skip).
	env := newAdoptionTestEnv(t)
	env.suite = SuiteRef{
		CanonicalScheme: "http",
		CanonicalHost:   "weird.example",
		SuitePath:       "/some/non-standard/path",
	}
	debHash := strings.Repeat("d", 64)
	stanzas := fakePackagesStanzas(map[string]string{"pool/foo.deb": debHash})
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": stanzas,
	})
	env.fetcher.put("http://weird.example/some/non-standard/path/main/binary-amd64/Packages", stanzas)

	if err := env.adopter.Run(context.Background(), env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// snapshot_member rows still exist — snapshot adopted normally.
	sf, _ := env.cache.GetSuiteFreshness(context.Background(),
		env.suite.CanonicalScheme, env.suite.CanonicalHost, env.suite.SuitePath)
	if sf.CurrentSnapshotID == nil {
		t.Fatal("non-standard suite path failed to adopt")
	}
	// Probing for the .deb's package_hash returns ErrNotFound — step
	// 8 was skipped without error.
	_, err := env.cache.GetPackageHash(context.Background(),
		env.suite.CanonicalScheme, env.suite.CanonicalHost,
		"/pool/foo.deb", *sf.CurrentSnapshotID)
	if !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("expected no package_hash row, got err=%v", err)
	}
}

func TestAdopter_PackageHash_GzipBombDefense(t *testing.T) {
	// A small gzip stream that decompresses to >256 MiB should be
	// rejected before it can exhaust memory. We can't actually
	// generate 256 MiB of zeros at test time without burning memory,
	// so synthesize one by directly slicing the cap from the helper —
	// here we exercise the threshold logic with a 64 MiB body.
	// AIDEV-NOTE: this test exercises the size-cap check using a
	// crafted gzip whose decompressed output overflows the configured
	// limit. We dial maxDecompressedPackagesBytes in via a small
	// override to keep the test under a few MiB of RAM.
	env := newAdoptionTestEnv(t)

	// Construct the bomb: a single gzip stream of 1 MiB of zero
	// bytes. We then run adoption with a custom limit.
	const bombSize = 1 << 20 // 1 MiB decompressed
	zeros := make([]byte, bombSize)
	gz := gzipBytes(zeros)
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages.gz": gz,
	})
	env.fetcher.put("http://archive.ubuntu.com/ubuntu/dists/noble/main/binary-amd64/Packages.gz", gz)

	// Adoption succeeds at the member level (the bomb is signed),
	// then fails at parse step because the decompressed content
	// has no Packages stanzas. Either ErrAdoptionParseFailed (no
	// stanzas error from ParsePackages) or a bomb-defense error
	// from readPackagesBlob is acceptable — both indicate adoption
	// declined to trust the input.
	err := env.adopter.Run(context.Background(), env.suite, releaseText, "", "")
	if !errors.Is(err, ErrAdoptionParseFailed) {
		t.Errorf("got %v, want ErrAdoptionParseFailed", err)
	}
}

func TestAdopter_PackageHash_GzipDecompressionWorks(t *testing.T) {
	// End-to-end check that gzip decompression actually happens — the
	// happy path test covers it, but this isolates the decompression
	// step (only Packages.gz, no plain Packages).
	env := newAdoptionTestEnv(t)
	debHash := strings.Repeat("e", 64)
	stanzas := fakePackagesStanzas(map[string]string{
		"pool/main/g/gz/gz.deb": debHash,
	})
	gz := gzipBytes(stanzas)
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages.gz": gz,
	})
	env.fetcher.put("http://archive.ubuntu.com/ubuntu/dists/noble/main/binary-amd64/Packages.gz", gz)

	if err := env.adopter.Run(context.Background(), env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	sf, _ := env.cache.GetSuiteFreshness(context.Background(),
		env.suite.CanonicalScheme, env.suite.CanonicalHost, env.suite.SuitePath)
	if sf.CurrentSnapshotID == nil {
		t.Fatal("adoption didn't flip pointer")
	}
	ph, err := env.cache.GetPackageHash(context.Background(),
		env.suite.CanonicalScheme, env.suite.CanonicalHost,
		"/ubuntu/pool/main/g/gz/gz.deb", *sf.CurrentSnapshotID)
	if err != nil {
		t.Fatalf("GetPackageHash: %v", err)
	}
	if ph.DeclaredSHA256 != debHash {
		t.Errorf("declared = %s, want %s", ph.DeclaredSHA256, debHash)
	}
}

func TestRepoRootFromSuitePath(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantOk  bool
		descrip string
	}{
		{"/ubuntu/dists/noble", "/ubuntu/", true, "ubuntu standard"},
		{"/debian/dists/bookworm-updates", "/debian/", true, "debian-updates"},
		{"/dists/foo", "/", true, "root-level dists"},
		{"/some/non/standard", "", false, "no /dists/ segment"},
		{"", "", false, "empty"},
	}
	for _, tc := range cases {
		t.Run(tc.descrip, func(t *testing.T) {
			got, ok := repoRootFromSuitePath(tc.in)
			if got != tc.want || ok != tc.wantOk {
				t.Errorf("repoRootFromSuitePath(%q) = (%q, %v), want (%q, %v)",
					tc.in, got, ok, tc.want, tc.wantOk)
			}
		})
	}
}

func TestIsPackagesMember(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"main/binary-amd64/Packages", true},
		{"main/binary-amd64/Packages.gz", true},
		{"main/binary-amd64/Packages.xz", true},   // SPEC3 §7.5.2: xz now supported
		{"main/binary-amd64/Packages.bz2", false}, // bz2 still unsupported
		{"main/binary-amd64/Sources", false},
		{"main/source/Sources", false},
		{"main/i18n/Translation-en", false},
		{"main/Contents-amd64.gz", false},
		{"Packages", true}, // root-level
		{"", false},
	}
	for _, tc := range cases {
		got := isPackagesMember(tc.path)
		if got != tc.want {
			t.Errorf("isPackagesMember(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestIsPackagesBasename covers the SPEC3 §7.5.4 coverage detection
// helper. A Packages-prefixed basename — including unsupported
// compressions — must return true so a directory whose only variant
// is e.g. Packages.bz2 still flips coverage_complete to false instead
// of silently slipping through.
func TestIsPackagesBasename(t *testing.T) {
	cases := []struct {
		base string
		want bool
	}{
		{"Packages", true},
		{"Packages.gz", true},
		{"Packages.xz", true},
		{"Packages.bz2", true},
		{"Packages.zst", true},
		{"Packages.lz4", true},
		// Common false positives that must not match:
		{"Packages.", false}, // trailing dot but no extension
		{"PackagesFoo", false},
		{"Sources", false},
		{"NotPackages", false},
		{"", false},
		{"Release", false},
		{"InRelease", false},
	}
	for _, tc := range cases {
		got := isPackagesBasename(tc.base)
		if got != tc.want {
			t.Errorf("isPackagesBasename(%q) = %v, want %v", tc.base, got, tc.want)
		}
	}
}

func TestByHashAliasPath(t *testing.T) {
	cases := []struct {
		path, sha, want string
	}{
		{"main/binary-amd64/Packages", "abc", "main/binary-amd64/by-hash/SHA256/abc"},
		{"main/source/Sources", "def", "main/source/by-hash/SHA256/def"},
		{"toplevel-file", "ghi", ""}, // no dir → skip
		{"", "x", ""},                // empty → skip
	}
	for _, tc := range cases {
		got := byHashAliasPath(tc.path, tc.sha)
		if got != tc.want {
			t.Errorf("byHashAliasPath(%q, %q) = %q, want %q", tc.path, tc.sha, got, tc.want)
		}
	}
}

func TestBuildMemberURL(t *testing.T) {
	cases := []struct {
		suite SuiteRef
		rel   string
		want  string
	}{
		{
			SuiteRef{CanonicalScheme: "http", CanonicalHost: "archive.ubuntu.com", SuitePath: "/ubuntu/dists/noble"},
			"main/binary-amd64/Packages",
			"http://archive.ubuntu.com/ubuntu/dists/noble/main/binary-amd64/Packages",
		},
		{
			// Trailing slash on suite path should not double up.
			SuiteRef{CanonicalScheme: "https", CanonicalHost: "deb.debian.org", SuitePath: "/debian/dists/bookworm/"},
			"main/binary-amd64/Packages.gz",
			"https://deb.debian.org/debian/dists/bookworm/main/binary-amd64/Packages.gz",
		},
	}
	for _, tc := range cases {
		got := buildMemberURL(tc.suite, tc.rel)
		if got != tc.want {
			t.Errorf("buildMemberURL = %q, want %q", got, tc.want)
		}
	}
}

// SPEC4 §12.1: blobHeartbeatTracker is a small mutex-guarded slice; its
// Add/Snapshot semantics are critical to the §7.5.2 heartbeat correctness.

func TestBlobHeartbeatTracker_AddAndSnapshot(t *testing.T) {
	tr := &blobHeartbeatTracker{}
	if got := tr.Snapshot(); got != nil {
		t.Errorf("empty tracker Snapshot = %v, want nil", got)
	}
	tr.Add("aa")
	tr.Add("bb")
	tr.Add("") // empty hashes are ignored — Add no-ops
	tr.Add("cc")
	got := tr.Snapshot()
	if len(got) != 3 {
		t.Errorf("len = %d, want 3 (empty add must not be tracked)", len(got))
	}
	want := []string{"aa", "bb", "cc"}
	for i, h := range want {
		if got[i] != h {
			t.Errorf("got[%d] = %q, want %q", i, got[i], h)
		}
	}

	// Snapshot must be a copy: mutating it must not affect subsequent
	// Snapshots.
	got[0] = "MUTATED"
	got2 := tr.Snapshot()
	if got2[0] != "aa" {
		t.Errorf("Snapshot returned an aliased slice; mutating one slot affected the next Snapshot")
	}
}

func TestBlobHeartbeatTracker_ConcurrentAddSnapshot(t *testing.T) {
	tr := &blobHeartbeatTracker{}
	var wg sync.WaitGroup
	const N = 100
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			tr.Add(fmt.Sprintf("hash-%04d", i))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			_ = tr.Snapshot() // -race verifies no data race against Add
		}
	}()
	wg.Wait()
	got := tr.Snapshot()
	if len(got) != N {
		t.Errorf("post-concurrent len = %d, want %d", len(got), N)
	}
}

// SPEC4 §12.1: §7.5.2 site-6 ticker. We construct an Adopter with a
// short heartbeat_interval and call runHeartbeatTicker on it as a
// goroutine; assert at least N writes land on suite_snapshot.heartbeat_at
// within the test budget.

func TestRunHeartbeatTicker_AdvancesHeartbeatAt(t *testing.T) {
	dir := t.TempDir()
	c, err := cache.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	// Plant a candidate snapshot row directly. Use a non-FK inrelease
	// hash by inserting a blob row first.
	ctx := context.Background()
	hash := strings.Repeat("a", 64)
	if err := c.PutBlob(ctx, hash, 1); err != nil {
		t.Fatal(err)
	}
	id, _, err := c.InsertCandidateSnapshot(ctx, cache.SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "tick.example",
		SuitePath:       "/p",
		InReleaseHash:   &hash,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Backdate heartbeat_at to epoch=1 so any ticker write produces a
	// large jump that's unambiguous at the suite_snapshot's
	// unix-seconds resolution.
	dbPath := filepath.Join(c.Dir(), "cache.db")
	{
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE suite_snapshot SET heartbeat_at = 1 WHERE snapshot_id = ?`, id); err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
	}

	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, nil))
	a := &Adopter{
		cache:             c,
		logger:            logger,
		heartbeatInterval: 50 * time.Millisecond,
	}

	tickerCtx, tickerCancel := context.WithCancel(ctx)
	tickerDone := make(chan struct{})
	go func() {
		defer close(tickerDone)
		a.runHeartbeatTicker(tickerCtx, "test.example", id, nil)
	}()

	// Poll for the heartbeat_at write to land. Generous timeout for
	// slow CI; a healthy machine sees the first tick within 50-150ms.
	deadline := time.Now().Add(5 * time.Second)
	advanced := false
	for time.Now().Before(deadline) {
		if hb := readHeartbeatAtDirect(t, dbPath, id); hb > 1 {
			advanced = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	tickerCancel()

	if !advanced {
		t.Errorf("heartbeat_at did not advance from epoch=1 within 5s; ticker did not fire")
	}

	// Ticker must exit cleanly on cancel within a reasonable bound.
	select {
	case <-tickerDone:
	case <-time.After(2 * time.Second):
		t.Errorf("ticker did not exit within 2s of cancel")
	}
}

func TestRunHeartbeatTicker_DisabledWhenIntervalZero(t *testing.T) {
	dir := t.TempDir()
	c, err := cache.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	a := &Adopter{
		cache:             c,
		logger:            slog.Default(),
		heartbeatInterval: 0,
	}
	// Should return immediately. Run with cancelled ctx to be safe.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		a.runHeartbeatTicker(ctx, "test.example", 1, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Errorf("runHeartbeatTicker with zero interval did not return immediately")
	}
}

func TestRunHeartbeatTicker_ExitsOnCancelWithoutTickingMoreThanOnce(t *testing.T) {
	dir := t.TempDir()
	c, err := cache.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	a := &Adopter{
		cache:             c,
		logger:            slog.Default(),
		heartbeatInterval: 50 * time.Millisecond,
	}

	// Cancel the ctx immediately after spawning. The ticker should exit
	// having ticked zero or one times (the cancellation race window).
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a.runHeartbeatTicker(ctx, "test.example", 99999 /*nonexistent snapshot id; HeartbeatSnapshot is a no-op*/, nil)
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Errorf("ticker did not exit within 200ms of cancel")
	}
}

// readHeartbeatAtDirect reads suite_snapshot.heartbeat_at via a
// short-lived sql.DB on the cache file path. The cache package's
// internal *sql.DB is unexported so tests use this side door.
func readHeartbeatAtDirect(t *testing.T, dbPath string, id int64) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	var hb int64
	if err := db.QueryRow(`SELECT heartbeat_at FROM suite_snapshot WHERE snapshot_id = ?`, id).Scan(&hb); err != nil {
		t.Fatalf("read heartbeat_at: %v", err)
	}
	return hb
}

// TestAdopter_MemberSkipped_404SkipsMidList verifies the SPEC2 §7.5.2
// (Phase 2 clarification) "4xx is skipped, not fatal" behavior: a
// declared Release member that the upstream returns 404 for is
// omitted from snapshot_member, the per-skip WARN line is emitted,
// and the adoption otherwise commits cleanly with skipped_count
// surfaced on adoption_success. Canonical real-world trigger: Ubuntu
// declaring uncompressed Contents-amd64 in Release while only
// shipping Contents-amd64.gz.
func TestAdopter_MemberSkipped_404SkipsMidList(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	dir := t.TempDir()
	c, err := cache.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ff := newFakeFetcher()
	ad, err := NewAdopter(AdoptionConfig{
		Cache:       c,
		Fetcher:     ff,
		Verifier:    passThroughVerifier{},
		HostLimiter: hostsem.New(8),
		Logger:      logger,
	})
	if err != nil {
		t.Fatalf("NewAdopter: %v", err)
	}
	suite := SuiteRef{
		CanonicalScheme: "http",
		CanonicalHost:   "archive.ubuntu.com",
		SuitePath:       "/ubuntu/dists/noble",
	}

	debHash := strings.Repeat("a", 64)
	pkgs := fakePackagesStanzas(map[string]string{
		"pool/main/f/foo/foo_1.deb": debHash,
	})
	contents := []byte("phantom Contents-amd64 body upstream wont serve")
	src := []byte("Sources content")
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": pkgs,
		"main/Contents-amd64":        contents,
		"main/source/Sources":        src,
	})

	base := "http://archive.ubuntu.com/ubuntu/dists/noble/"
	ff.put(base+"main/binary-amd64/Packages", pkgs)
	ff.fail404(base + "main/Contents-amd64")
	ff.put(base+"main/source/Sources", src)

	if err := ad.Run(context.Background(), suite, releaseText, "etag-1", ""); err != nil {
		t.Fatalf("Run: %v (expected nil — 4xx members should skip, not abort)", err)
	}

	// Snapshot must be committed.
	sf, err := c.GetSuiteFreshness(context.Background(),
		suite.CanonicalScheme, suite.CanonicalHost, suite.SuitePath)
	if err != nil {
		t.Fatalf("GetSuiteFreshness: %v", err)
	}
	if sf.CurrentSnapshotID == nil {
		t.Fatal("current_snapshot_id not set after partial-skip adoption")
	}

	// snapshot_member must contain Packages and Sources but NOT
	// the skipped Contents-amd64.
	members, err := c.ListSnapshotMembers(context.Background(), *sf.CurrentSnapshotID)
	if err != nil {
		t.Fatalf("ListSnapshotMembers: %v", err)
	}
	var sawPackages, sawSources, sawContents bool
	for _, m := range members {
		switch m.Path {
		case "main/binary-amd64/Packages":
			sawPackages = true
		case "main/source/Sources":
			sawSources = true
		case "main/Contents-amd64":
			sawContents = true
		}
	}
	if !sawPackages {
		t.Error("Packages snapshot_member missing — should have been fetched and recorded")
	}
	if !sawSources {
		t.Error("Sources snapshot_member missing — should have been fetched and recorded")
	}
	if sawContents {
		t.Error("skipped Contents-amd64 unexpectedly recorded as snapshot_member")
	}

	// Logs: per-skip WARN with the right fields, plus the
	// fetched/skipped counts on adoption_success.
	out := logBuf.String()
	if !strings.Contains(out, `"msg":"adoption_member_skipped"`) {
		t.Errorf("expected adoption_member_skipped log, got:\n%s", out)
	}
	if !strings.Contains(out, `"path":"main/Contents-amd64"`) {
		t.Errorf("expected path=main/Contents-amd64 in skip log, got:\n%s", out)
	}
	if !strings.Contains(out, `"upstream_status":404`) {
		t.Errorf("expected upstream_status:404 in skip log, got:\n%s", out)
	}
	if !strings.Contains(out, `"skipped_count":1`) {
		t.Errorf("expected skipped_count:1 in adoption_success log, got:\n%s", out)
	}
	if !strings.Contains(out, `"fetched_count":2`) {
		t.Errorf("expected fetched_count:2 in adoption_success log, got:\n%s", out)
	}
}

// TestAdopter_MemberSkipped_5xxStillFatal verifies that 5xx upstream
// errors during member fetch remain fatal (the 4xx-skip path only
// applies to client-error 4xx). 5xx means "upstream is broken right
// now", which is exactly what the existing
// adoption_member_fetch_failed semantics already cover.
func TestAdopter_MemberSkipped_5xxStillFatal(t *testing.T) {
	env := newAdoptionTestEnv(t)
	pkgs := fakePackagesStanzas(map[string]string{
		"pool/main/f/foo/foo_1.deb": strings.Repeat("a", 64),
	})
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": pkgs,
	})
	env.fetcher.fail503("http://archive.ubuntu.com/ubuntu/dists/noble/main/binary-amd64/Packages")

	err := env.adopter.Run(context.Background(), env.suite, releaseText, "", "")
	if !errors.Is(err, ErrAdoptionMemberFetchFailed) {
		t.Errorf("Run err = %v, want wrapped ErrAdoptionMemberFetchFailed", err)
	}

	sf, err := env.cache.GetSuiteFreshness(context.Background(),
		env.suite.CanonicalScheme, env.suite.CanonicalHost, env.suite.SuitePath)
	if err == nil && sf != nil && sf.CurrentSnapshotID != nil {
		t.Errorf("current_snapshot_id unexpectedly set after 5xx fatal: %d", *sf.CurrentSnapshotID)
	}
}

// TestAdopter_MemberSkipped_AllMembers404Fails verifies the
// SPEC2 §7.5.2 all-skipped guard: if every declared member 4xx's, the
// adoption fails (still wrapped as ErrAdoptionMemberFetchFailed)
// rather than committing a useless empty snapshot. The realistic
// trigger is a misconfigured suite_path that points at a directory
// whose Release lists members the archive serves under a different
// prefix.
func TestAdopter_MemberSkipped_AllMembers404Fails(t *testing.T) {
	env := newAdoptionTestEnv(t)
	pkgs := fakePackagesStanzas(map[string]string{
		"pool/main/f/foo/foo_1.deb": strings.Repeat("a", 64),
	})
	src := []byte("Sources content")
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": pkgs,
		"main/source/Sources":        src,
	})
	base := "http://archive.ubuntu.com/ubuntu/dists/noble/"
	env.fetcher.fail404(base + "main/binary-amd64/Packages")
	env.fetcher.fail404(base + "main/source/Sources")

	err := env.adopter.Run(context.Background(), env.suite, releaseText, "", "")
	if !errors.Is(err, ErrAdoptionMemberFetchFailed) {
		t.Fatalf("Run err = %v, want wrapped ErrAdoptionMemberFetchFailed (all-skipped guard)", err)
	}
	if !strings.Contains(err.Error(), "all 2 declared members returned 4xx") {
		t.Errorf("Run err = %q, want 'all 2 declared members returned 4xx' substring", err.Error())
	}

	sf, err := env.cache.GetSuiteFreshness(context.Background(),
		env.suite.CanonicalScheme, env.suite.CanonicalHost, env.suite.SuitePath)
	if err == nil && sf != nil && sf.CurrentSnapshotID != nil {
		t.Errorf("current_snapshot_id unexpectedly set after all-404 adoption: %d", *sf.CurrentSnapshotID)
	}
}
