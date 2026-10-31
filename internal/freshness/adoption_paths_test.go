package freshness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
)

// Deferred minor E-10: a memberWriter is a fetch destination that cannot
// finalize itself — only verifyMember moves its bytes into pool/. Its
// method set is exactly the fetch.FetchDst methods and Abort (Task 10
// review M-6), so any new forwarder (HashSum, a finalizer) is a decision.
func TestMemberWriter_HasNoFinalize(t *testing.T) {
	var _ fetch.FetchDst = (*memberWriter)(nil)
	typ := reflect.TypeOf(&memberWriter{})
	for _, name := range []string{"Finalize", "FinalizeExpectingHash"} {
		if _, ok := typ.MethodByName(name); ok {
			t.Errorf("memberWriter has %s: a caller could skip verifyMember", name)
		}
	}
	var got []string
	for i := range typ.NumMethod() { // exported methods, in lexicographic order
		got = append(got, typ.Method(i).Name)
	}
	if want := []string{"Abort", "Truncate", "Write", "Written"}; !slices.Equal(got, want) {
		t.Errorf("memberWriter's methods are %v, want exactly %v", got, want)
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
			t.Cleanup(func() { _ = c.Close() }) // idempotent: shutdown=false closes it early
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
// still adopts with the rows that passed, and the refused paths get none.
// The refused stanza has two files, so once per file is not once per path
// (Task 10 review M-5).
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
		{pkg: "odd", directory: "pool/main/o/odd%2b", files: map[string]string{
			"odd_1.dsc": strings.Repeat("b", 64), "odd_1.tar.gz": strings.Repeat("c", 64)}},
	})
	releaseText, _ := makeRelease(map[string][]byte{"main/source/Sources": srcBody})
	ff.put("http://archive.ubuntu.com/ubuntu/dists/noble/main/source/Sources", srcBody)
	trackSuite(t, c, suite)
	if err := ad.Run(context.Background(), suite, releaseText, "etag-1", ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	sf, err := c.GetSuiteFreshness(context.Background(), "http", suite.CanonicalHost, suite.SuitePath)
	if err != nil || sf.CurrentSnapshotID == nil {
		t.Fatalf("not adopted (%v)", err)
	}
	// fakeSourcesStanzas writes a stanza's files in map order: either odd
	// file may be the first.
	recs := logRecords(t, logs, "source_paths_skipped")
	if len(recs) != 1 || recs[0]["level"] != "INFO" || recs[0]["member_path"] != "main/source/Sources" ||
		recs[0]["count"] != float64(2) ||
		(recs[0]["first"] != "pool/main/o/odd%2b/odd_1.dsc" && recs[0]["first"] != "pool/main/o/odd%2b/odd_1.tar.gz") ||
		recs[0]["canonical_scheme"] != "http" || recs[0]["canonical_host"] != suite.CanonicalHost ||
		recs[0]["suite_path"] != suite.SuitePath || recs[0]["snapshot_id"] != float64(*sf.CurrentSnapshotID) {
		t.Fatalf("source_paths_skipped records %v, want one INFO for the Sources file of snapshot %d, count 2, first an odd path",
			recs, *sf.CurrentSnapshotID)
	}
	if _, err := c.GetPackageHash(context.Background(), "http", suite.CanonicalHost, "/ubuntu/pool/main/b/bash/bash_5.1-2.dsc", *sf.CurrentSnapshotID); err != nil {
		t.Fatalf("the valid source row: %v", err)
	}
	for _, refused := range []string{"odd_1.dsc", "odd_1.tar.gz"} {
		p := "/ubuntu/pool/main/o/odd%2b/" + refused
		if _, err := c.GetPackageHash(context.Background(), "http", suite.CanonicalHost, p, *sf.CurrentSnapshotID); !errors.Is(err, cache.ErrNotFound) {
			t.Fatalf("the refused path %s: err = %v, want cache.ErrNotFound", p, err)
		}
	}
}

// Task 10 review I-1: a non-empty Sources file that yields no row names
// its cause — every path refused by validateMemberPath (the stats still
// count them), or no usable stanza at all: one without the fields, or
// whose every Checksums-Sha256 entry is malformed (parked item 15 of the
// H1 final review: the text says well-formed, true of both).
func TestParseSources_NoRowNamesTheCause(t *testing.T) {
	h := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, body, want string
		skipped          int
	}{
		{"every path refused", "Package: a\nDirectory: ./pool/a\nChecksums-Sha256:\n " + h + " 1 a_1.dsc\n " + h + " 2 a_1.tar.gz\n",
			"sources: every path refused by validateMemberPath (2 skipped)", 2},
		{"no usable stanza", "Package: a\nDirectory: pool/a\nFiles:\n 0123456789abcdef0123456789abcdef 1 a_1.dsc\n",
			"sources: no stanza with Package, Directory and a well-formed Checksums-Sha256 entry", 0},
		{"every entry malformed", "Package: a\nDirectory: pool/a\nChecksums-Sha256:\n nothex 1 a_1.dsc\n " + h + " a_1.tar.gz\n",
			"sources: no stanza with Package, Directory and a well-formed Checksums-Sha256 entry", 0},
	} {
		_, stats, err := ParseSources([]byte(tc.body))
		if err == nil || err.Error() != tc.want || stats.PathsSkipped != tc.skipped {
			t.Errorf("%s: err = %v, skipped %d; want %q, %d", tc.name, err, stats.PathsSkipped, tc.want, tc.skipped)
		}
	}
}

// Task 10 review I-1: a Sources file whose every path validateMemberPath
// refuses — its Directory fields start "./", as `dpkg-scansources ./pool`
// writes them — lost all its source rows, so it still says so once (INFO
// source_paths_skipped), and its WARN source_parse_failed names that
// cause, not missing fields. The suite still adopts (source coverage is
// best-effort).
func TestAdopter_SourcesWithEveryPathRefusedSaysWhy(t *testing.T) {
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
	suite := SuiteRef{CanonicalScheme: "http", CanonicalHost: "repo.example", SuitePath: "/debian/dists/stable"}
	srcBody := fakeSourcesStanzas([]fakeSourceStanza{
		{pkg: "a", directory: "./pool/main/a/a", files: map[string]string{"a_1.dsc": strings.Repeat("a", 64)}},
		{pkg: "b", directory: "./pool/main/b/b", files: map[string]string{
			"b_1.dsc": strings.Repeat("b", 64), "b_1.tar.gz": strings.Repeat("c", 64)}},
	})
	releaseText, _ := makeRelease(map[string][]byte{"main/source/Sources": srcBody})
	ff.put("http://repo.example/debian/dists/stable/main/source/Sources", srcBody)
	trackSuite(t, c, suite)
	if err := ad.Run(context.Background(), suite, releaseText, "etag-1", ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	skipped := logRecords(t, logs, "source_paths_skipped")
	if len(skipped) != 1 || skipped[0]["level"] != "INFO" || skipped[0]["member_path"] != "main/source/Sources" ||
		skipped[0]["count"] != float64(3) || skipped[0]["first"] != "./pool/main/a/a/a_1.dsc" {
		t.Fatalf("source_paths_skipped records %v, want one INFO for the Sources file, count 3, first ./pool/main/a/a/a_1.dsc", skipped)
	}
	failed := logRecords(t, logs, "source_parse_failed")
	if len(failed) != 1 || failed[0]["level"] != "WARN" || failed[0]["member_path"] != "main/source/Sources" ||
		failed[0]["error"] != "sources: every path refused by validateMemberPath (3 skipped)" {
		t.Fatalf("source_parse_failed records %v, want one WARN naming the refused paths", failed)
	}
	sf, err := c.GetSuiteFreshness(context.Background(), "http", suite.CanonicalHost, suite.SuitePath)
	if err != nil || sf.CurrentSnapshotID == nil {
		t.Fatalf("not adopted (%v)", err)
	}
}
