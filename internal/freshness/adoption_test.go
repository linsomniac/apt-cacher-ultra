package freshness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
)

// passThroughVerifier returns the input verbatim. It exists only for
// step-2 tests; production wiring will use the real GPG verifier from
// step 3.
type passThroughVerifier struct{}

func (passThroughVerifier) VerifyInline(ctx context.Context, suite SuiteRef, inRelease []byte) ([]byte, error) {
	return inRelease, nil
}

// failingVerifier always returns an error; used to exercise the
// ErrAdoptionGPGFailed path.
type failingVerifier struct{ err error }

func (v failingVerifier) VerifyInline(ctx context.Context, suite SuiteRef, inRelease []byte) ([]byte, error) {
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
	pkgs := []byte("Packages content")
	pkgsGz := []byte("Packages.gz content (compressed bytes)")
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
	// Three member fetches expected (one per Release member).
	if got := env.fetcher.calls.Load(); got != 3 {
		t.Errorf("fetch calls = %d, want 3", got)
	}

	// suite_freshness pointer flipped.
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
	// + 3 by-hash aliases (one per declared, all in distinct dirs)
	// = 7 rows.
	got, err := env.cache.ListSnapshotMembers(context.Background(), snapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 7 {
		t.Errorf("snapshot_member count = %d, want 7", len(got))
	}

	// Spot-check that declared paths landed correctly, with their
	// declared sha256 matching the fetched bytes' actual hash.
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
		if found.DeclaredSHA256 != want {
			t.Errorf("%q declared_sha256 = %s, want %s", p, found.DeclaredSHA256, want)
		}
		if found.BlobHash != want {
			t.Errorf("%q blob_hash = %s, want %s (declared)", p, found.BlobHash, want)
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

	// At least one by-hash alias landed in the right dir shape.
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
	// pool blob and skip the upstream fetch.
	env := newAdoptionTestEnv(t)
	body := []byte("same content twice")
	r1, _ := makeRelease(map[string][]byte{"main/binary-amd64/Packages": body})
	env.fetcher.put("http://archive.ubuntu.com/ubuntu/dists/noble/main/binary-amd64/Packages", body)

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
	r2 = append(r2, '\n') // mutate one byte to change the InRelease hash
	if err := env.adopter.Run(context.Background(), SuiteRef{
		CanonicalScheme: env.suite.CanonicalScheme,
		CanonicalHost:   env.suite.CanonicalHost,
		SuitePath:       env.suite.SuitePath,
	}, r2, "etag2", ""); err != nil {
		t.Fatalf("Run #2: %v", err)
	}
	if got := env.fetcher.calls.Load(); got != firstCalls {
		t.Errorf("second adoption did %d more fetches, expected pool reuse",
			got-firstCalls)
	}
}

func TestAdopter_PoolCorruptionDetectedAndRefetched(t *testing.T) {
	env := newAdoptionTestEnv(t)
	declared := []byte("real content")
	corruptBody := []byte("rotted bytes that don't match declared")
	releaseText, hashes := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": declared,
	})
	declaredHash := hashes["main/binary-amd64/Packages"]
	env.fetcher.put("http://archive.ubuntu.com/ubuntu/dists/noble/main/binary-amd64/Packages", declared)

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
	releaseA, _ := makeRelease(map[string][]byte{"main/Packages": body})
	releaseB, _ := makeRelease(map[string][]byte{"main/Packages": []byte("other body")})
	bf.put("http://x.example/p1/main/Packages", body)
	bf.put("http://x.example/p2/main/Packages", []byte("other body"))

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

