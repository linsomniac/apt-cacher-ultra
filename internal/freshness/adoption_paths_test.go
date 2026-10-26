package freshness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
)

// Deferred minor E-10: a memberWriter is a fetch destination that cannot
// finalize itself — only verifyMember moves its bytes into pool/.
func TestMemberWriter_HasNoFinalize(t *testing.T) {
	var _ fetch.FetchDst = (*memberWriter)(nil)
	typ := reflect.TypeOf(&memberWriter{})
	for _, name := range []string{"Finalize", "FinalizeExpectingHash"} {
		if _, ok := typ.MethodByName(name); ok {
			t.Errorf("memberWriter has %s: a caller could skip verifyMember", name)
		}
	}
}

// Deferred minor E-11: a SHA512-only member whose bytes match the hash but
// not the declared size fails (Finalize's size check) and leaves nothing
// in the pool.
func TestVerifyMember_SHA512OnlySizeMismatch(t *testing.T) {
	env := newAdoptionTestEnv(t)
	body := []byte("Package: p\n")
	w, err := newMemberWriter(env.cache)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	_, err = verifyMember(w, ReleaseMember{Path: "p", SHA512: sha512Of(body)}, int64(len(body))+1, nil)
	if !errors.Is(err, cache.ErrSizeMismatch) {
		t.Fatalf("err = %v, want cache.ErrSizeMismatch", err)
	}
	if exists, xerr := env.cache.BlobExists(shaOf(body)); xerr != nil || exists {
		t.Fatalf("in pool %v (%v), want nothing", exists, xerr)
	}
}

// Deferred minor E-11: ParseRelease's two hash-block errors, in full (they
// were pinned by a prefix only), for each block kind.
func TestParseRelease_HashBlockErrors(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"no block", "Origin: Ubuntu\nMD5Sum:\n 0123456789abcdef0123456789abcdef 4533 x\n", "release: no SHA256 or SHA512 block"},
		{"empty SHA256", "Origin: Ubuntu\nSHA256:\nDescription: foo\n", "release: empty SHA256 and SHA512 blocks"},
		{"empty SHA512", "Origin: Ubuntu\nSHA512:\nDescription: foo\n", "release: empty SHA256 and SHA512 blocks"},
	} {
		if _, err := ParseRelease([]byte(tc.body)); err == nil || err.Error() != tc.want {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

// Deferred minors E-13 and E-14 (C-16): a failed read of the served anchor
// before the flip barrier is logged at WARN — and at DEBUG when a shutdown
// cut it short; either way the barrier runs (false).
func TestServedAnchorUnchanged_ReadFailure(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprintf("shutdown=%v", shutdown), func(t *testing.T) {
			c, err := cache.Open(context.Background(), t.TempDir(), discardLogger())
			if err != nil {
				t.Fatal(err)
			}
			logs := &lockedBuffer{}
			a, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: newFakeFetcher(), Verifier: passThroughVerifier{},
				HostLimiter: hostsem.New(8), Logger: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
			if err != nil {
				t.Fatal(err)
			}
			p := &adoptionPayload{form: adoptionFormInline, inlineHash: testHex64}
			if shutdown {
				stop, cancel := context.WithCancel(context.Background())
				cancel()
				p.stop = stop
			} else {
				_ = c.Close() // a read that fails for another reason than shutdown
			}
			suite := SuiteRef{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite}
			if a.servedAnchorUnchanged(context.Background(), suite, p) {
				t.Fatal("a failed read answered unchanged: the barrier would be skipped")
			}
			want := "WARN"
			if shutdown {
				want = "DEBUG"
			}
			recs := logRecords(t, logs, "adoption_served_anchor_read_failed")
			if len(recs) != 1 || recs[0]["level"] != want || recs[0]["path"] != rigSuite+"/InRelease" {
				t.Fatalf("records %v, want one at %s naming the anchor", recs, want)
			}
		})
	}
}

// Deferred minor E-14 (A-49): the startup marker pass says, at DEBUG,
// which cached anchor it skipped because its pool file could not be read.
func TestMarkFromCachedSignedAnchors_LogsASkippedAnchor(t *testing.T) {
	ctx := context.Background()
	c, err := cache.Open(ctx, t.TempDir(), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	// A tracked suite's InRelease row, last used at 1, whose blob row has no
	// pool file.
	stale, used := testHex64, int64(1)
	if err := c.PutBlob(ctx, stale, 1); err != nil {
		t.Fatal(err)
	}
	if err := c.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: rigHost, Path: rigSuite + "/InRelease",
		BlobHash: &stale, UpstreamURL: "http://h/r/dists/x/InRelease", IsMetadata: true, LastFetchedAt: &used}); err != nil {
		t.Fatal(err)
	}
	if err := c.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite}); err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuffer{}
	n, err := markFromCachedSignedAnchors(ctx, c, slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil || n != 0 {
		t.Fatalf("markers = %d, %v; want none", n, err)
	}
	recs := logRecords(t, logs, "legacy_trust_audit_cached_anchor_skipped")
	if len(recs) != 1 || recs[0]["level"] != "DEBUG" || recs[0]["path"] != rigSuite+"/InRelease" || recs[0]["err"] == nil {
		t.Fatalf("records %v, want one DEBUG naming the anchor and the error", recs)
	}
}

// Deferred minor GD-2 (ruling PG-21): ParseSources keeps validateMemberPath
// for a stanza's Directory and its files, and counts what it leaves out —
// every file of a stanza whose Directory fails, each file whose own path
// fails — keeping the first.
func TestParseSources_CountsTheSkippedPaths(t *testing.T) {
	h := strings.Repeat("a", 64)
	text := "Package: a\nDirectory: ./pool/a\nChecksums-Sha256:\n " + h + " 1 a_1.dsc\n " + h + " 2 a_1.tar.gz\n\n" +
		"Package: b\nDirectory: pool/main/b/b\nChecksums-Sha256:\n " + h + " 3 b_1.dsc\n " + h + " 4 b_1%2b.tar.gz\n"
	refs, stats, err := ParseSources([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Path != "pool/main/b/b/b_1.dsc" {
		t.Fatalf("refs %+v, want b_1.dsc alone", refs)
	}
	if stats.PathsSkipped != 3 || stats.FirstSkipped != "./pool/a/a_1.dsc" {
		t.Fatalf("skipped %d, first %q; want 3 and ./pool/a/a_1.dsc", stats.PathsSkipped, stats.FirstSkipped)
	}
}

// Deferred minor GD-2: an adoption whose Sources file had paths skipped
// says so once for that file (source_paths_skipped, INFO: review M-8: it
// repeats at every adoption, and the operator cannot act on it); the suite
// still adopts with the rows that passed.
func TestAdopter_SourcesWithSkippedPathsLogsOnce(t *testing.T) {
	logs := &lockedBuffer{}
	c, err := cache.Open(context.Background(), t.TempDir(), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ff := newFakeFetcher()
	ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: ff, Verifier: passThroughVerifier{}, HostLimiter: hostsem.New(8),
		Logger: slog.New(slog.NewJSONHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	suite := SuiteRef{CanonicalScheme: "http", CanonicalHost: "archive.ubuntu.com", SuitePath: "/ubuntu/dists/noble"}
	srcBody := fakeSourcesStanzas([]fakeSourceStanza{
		{pkg: "bash", directory: "pool/main/b/bash", files: map[string]string{"bash_5.1-2.dsc": strings.Repeat("a", 64)}},
		{pkg: "odd", directory: "pool/main/o/odd%2b", files: map[string]string{"odd_1.dsc": strings.Repeat("b", 64)}},
	})
	releaseText, _ := makeRelease(map[string][]byte{"main/source/Sources": srcBody})
	ff.put("http://archive.ubuntu.com/ubuntu/dists/noble/main/source/Sources", srcBody)
	trackSuite(t, c, suite)
	if err := ad.Run(context.Background(), suite, releaseText, "etag-1", ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	recs := logRecords(t, logs, "source_paths_skipped")
	if len(recs) != 1 || recs[0]["level"] != "INFO" || recs[0]["member_path"] != "main/source/Sources" ||
		recs[0]["count"] != float64(1) || recs[0]["first"] != "pool/main/o/odd%2b/odd_1.dsc" || recs[0]["canonical_scheme"] != "http" {
		t.Fatalf("source_paths_skipped records %v, want one INFO for the Sources file, count 1, first the odd path", recs)
	}
	sf, err := c.GetSuiteFreshness(context.Background(), "http", suite.CanonicalHost, suite.SuitePath)
	if err != nil || sf.CurrentSnapshotID == nil {
		t.Fatalf("not adopted (%v)", err)
	}
	if _, err := c.GetPackageHash(context.Background(), "http", suite.CanonicalHost, "/ubuntu/pool/main/b/bash/bash_5.1-2.dsc", *sf.CurrentSnapshotID); err != nil {
		t.Fatalf("the valid source row: %v", err)
	}
}
