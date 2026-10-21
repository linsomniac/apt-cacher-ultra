package freshness

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// gzBody is a body that passes the .gz sniff, different per s.
func gzBody(s string) []byte { return append(append([]byte{}, flatGz...), s...) }

// sniffRejected returns acu_anchorless_sniff_rejected_total for the flat
// rig's host as metrics.Default renders it.
func sniffRejected(t *testing.T) float64 {
	t.Helper()
	var buf bytes.Buffer
	metrics.Default.Render(&buf)
	for _, line := range strings.Split(buf.String(), "\n") {
		if v, ok := strings.CutPrefix(line, `acu_anchorless_sniff_rejected_total{host="`+flatRigHost+`"} `); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				t.Fatal(err)
			}
			return f
		}
	}
	return 0
}

// lookup returns the suite's url_path row of name, nil when it has none.
func (r *flatRig) lookup(name string) *cache.URLPath {
	r.t.Helper()
	u, err := r.cache.LookupURL(context.Background(), "http", flatRigHost, r.suite.SuitePath+"/"+name)
	switch {
	case errors.Is(err, cache.ErrNotFound):
		return nil
	case err != nil:
		r.t.Fatal(err)
	}
	return u
}

// Spec C4 Dispatch 3 → 4: a flat suite whose probe tree finds neither
// InRelease nor Release, with no signed evidence, refreshes its cached
// indexes anchorless — one conditional GET each, with its stored
// validators. A changed Packages.gz that passes its sniff replaces its
// blob (request_count and last_requested_at kept, last_fetched_at moved);
// a 304 keeps Sources.gz as it was; a 404 deletes Contents-amd64.gz; a
// plain Packages answered with an HTML page keeps its row and is counted.
// A plain Contents-* has no sniff (deviation 2): changed bytes delete
// Contents-i386, the same bytes keep Contents-all. The check is a
// success, anchorless_refreshed, and adopts nothing.
func TestCheck_AnchorlessRefreshRevalidatesTheIndexes(t *testing.T) {
	r := newFlatRig(t, "/fury")
	fetched := wallclock.Unix() - 3600
	oldPk, newPk, src, plain := gzBody("v1"), gzBody("v2"), gzBody("src"), []byte("Package: a\n")
	oldHash := r.cacheRow("Packages.gz", oldPk, 2, fetched, rigETag(oldPk))
	srcHash := r.cacheRow("Sources.gz", src, 1, fetched, rigETag(src))
	r.cacheRow("Contents-amd64.gz", gzBody("contents"), 1, fetched, "")
	plainHash := r.cacheRow("Packages", plain, 1, fetched, rigETag(plain))
	r.cacheRow("Contents-i386", []byte("usr/bin/a admin/a\n"), 1, fetched, "")
	allHash := r.cacheRow("Contents-all", []byte("usr/bin/b admin/b\n"), 1, fetched, "")
	r.up.setAt(r.url("Contents-i386"), []byte("usr/bin/a2 admin/a\n"))
	r.up.setAt(r.url("Contents-all"), []byte("usr/bin/b admin/b\n"))
	r.up.setAt(r.url("Packages.gz"), newPk)
	r.up.setAt(r.url("Sources.gz"), src)
	r.up.setAt(r.url("Packages"), []byte("<html>soft 404</html>"))
	rejected := sniffRejected(t)

	r.check()
	if got := r.lastResult(); got != "anchorless_refreshed" {
		t.Fatalf("result %q, want anchorless_refreshed", got)
	}
	for name, want := range map[string][]string{"Packages.gz": {rigETag(oldPk)}, "Sources.gz": {rigETag(src)},
		"Contents-amd64.gz": {""}, "Packages": {rigETag(plain)}, "InRelease": {""}, "Release": {""}} {
		if got := r.up.etagsSent(r.url(name)); !slices.Equal(got, want) {
			t.Errorf("%s: ETags sent %q, want %q", name, got, want)
		}
	}
	if n := r.requests("Release.gpg"); n != 0 {
		t.Errorf("Release.gpg probed %d times, want 0", n)
	}
	if u := r.lookup("Packages.gz"); u == nil || *u.BlobHash != hashOf(newPk) || *u.BlobHash == oldHash ||
		u.RequestCount != 2 || *u.LastRequestedAt != fetched || *u.LastFetchedAt <= fetched || *u.UpstreamETag != rigETag(newPk) {
		t.Errorf("Packages.gz row = %+v; want the new blob and ETag, its interest kept, last_fetched_at moved", u)
	}
	if u := r.lookup("Sources.gz"); u == nil || *u.BlobHash != srcHash || *u.LastFetchedAt != fetched {
		t.Errorf("Sources.gz row = %+v; want it untouched", u)
	}
	if u := r.lookup("Contents-amd64.gz"); u != nil {
		t.Errorf("Contents-amd64.gz row = %+v; want it deleted (404)", u)
	}
	if u := r.lookup("Packages"); u == nil || *u.BlobHash != plainHash || *u.LastFetchedAt != fetched {
		t.Errorf("Packages row = %+v; want it kept (the HTML failed its sniff)", u)
	}
	if u := r.lookup("Contents-i386"); u != nil {
		t.Errorf("Contents-i386 row = %+v; want it deleted (changed, and no sniff)", u)
	}
	if u := r.lookup("Contents-all"); u == nil || *u.BlobHash != allHash {
		t.Errorf("Contents-all row = %+v; want it kept (the same bytes)", u)
	}
	if got := sniffRejected(t) - rejected; got != 1 {
		t.Errorf("acu_anchorless_sniff_rejected_total rose by %v, want 1", got)
	}
	row := r.row()
	if row.CurrentSnapshotID != nil || row.LastSuccessAt == nil || *row.LastSuccessAt != 1_800_000_000 || row.CheckFailCount != 0 {
		t.Errorf("suite row = %+v; want a success and no snapshot", row)
	}
}

// Review Focus 2: an upstream that answers every request with an HTML
// page (a captive portal, a dead vendor's landing page) never replaces a
// flat index — the row stays, counted, its validators unchanged so the
// next check asks the same — and, since a sniff-rejected 200 is no
// content answer (design review M-7), every such check is a failed check:
// the streak backs off and the status page shows the suite failing, not
// healthy.
func TestCheck_AnchorlessSniffRejectionKeepsTheRow(t *testing.T) {
	r := newFlatRig(t, "/fury")
	fetched := wallclock.Unix() - 3600
	pk := gzBody("v1")
	h := r.cacheRow("Packages.gz", pk, 1, fetched, rigETag(pk))
	r.up.setAt(r.url("Packages.gz"), []byte("<!DOCTYPE html><html>Not Found</html>"))
	rejected := sniffRejected(t)
	for range 2 {
		r.check()
		r.clock.advance(20 * time.Minute)
	}
	if got := r.lastResult(); got != "failed" {
		t.Fatalf("result %q, want failed (a sniff-rejected 200 is no content answer)", got)
	}
	if row := r.row(); row.CheckFailCount != 2 || row.LastSuccessAt != nil {
		t.Fatalf("suite row = %+v; want two failed checks and no success", row)
	}
	if u := r.lookup("Packages.gz"); u == nil || *u.BlobHash != h || *u.LastFetchedAt != fetched || *u.UpstreamETag != rigETag(pk) {
		t.Fatalf("Packages.gz row = %+v; want it as it was", u)
	}
	if got, want := r.up.etagsSent(r.url("Packages.gz")), []string{rigETag(pk), rigETag(pk)}; !slices.Equal(got, want) {
		t.Fatalf("ETags sent %q, want %q", got, want)
	}
	if got := sniffRejected(t) - rejected; got != 2 {
		t.Fatalf("acu_anchorless_sniff_rejected_total rose by %v, want 2", got)
	}
}

// Design review M-7 beside M-1's arm: a 200 whose bytes fail their sniff
// is no content answer even when they are the cached bytes (a client's
// miss stored a captive portal's page as Packages.gz, and the portal
// still serves it). The sniff is judged before the bytes are compared, so
// the check fails and stores nothing, rather than taking the page for an
// unchanged index.
func TestCheck_AnchorlessCachedPageIsNoAnswerEvenUnchanged(t *testing.T) {
	r := newFlatRig(t, "/fury")
	page := []byte("<!DOCTYPE html><html>Not Found</html>")
	h := r.cacheRow("Packages.gz", page, 1, wallclock.Unix()-3600, "e-older")
	r.up.setAt(r.url("Packages.gz"), page)
	r.check()
	if got := r.lastResult(); got != "failed" {
		t.Fatalf("result %q, want failed (a sniff-rejected 200 is no content answer)", got)
	}
	if u := r.lookup("Packages.gz"); u == nil || *u.BlobHash != h || strOf(u.UpstreamETag) != "e-older" {
		t.Fatalf("Packages.gz row = %+v; want it as it was", u)
	}
}

// Spec C4 Dispatch 4's answers, one index row each: new bytes replace
// its blob; 404 and 410 delete it; 401 and 403 keep it but are no content
// answer (design review M-7: lost permissions are a failing check, not a
// healthy one); a 5xx, a transport error and a body over the cap keep it
// unknown — and with no content answer at all the check fails (the streak
// backs off), logging the cap once. A changed plain Contents-* is dropped
// (deviation 2) but is no content answer either. One answer is enough,
// and a 304 to a request carrying only If-Modified-Since (a row cached
// with upstream_lastmod and no ETag) is one (design review M-9).
func TestCheck_AnchorlessAnswersByStatus(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		kept    bool
		result  string
		failLog bool
	}{
		{"200 with new bytes", nil, true, "anchorless_refreshed", false},
		{"404", &fetch.StatusError{Code: 404}, false, "anchorless_refreshed", false},
		{"410", &fetch.StatusError{Code: 410}, false, "anchorless_refreshed", false},
		{"401", &fetch.StatusError{Code: 401}, true, "failed", false},
		{"403", &fetch.StatusError{Code: 403}, true, "failed", false},
		{"503", &fetch.StatusError{Code: 503}, true, "failed", false},
		{"a transport error", errors.New("connection reset by peer"), true, "failed", false},
		{"a body over the cap", fmt.Errorf("conditional: %w", fetch.ErrConditionalBodyTooLarge), true, "failed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newFlatRig(t, "/fury")
			pk := gzBody("v1")
			r.cacheRow("Packages.gz", pk, 1, wallclock.Unix()-3600, rigETag(pk))
			r.up.setAt(r.url("Packages.gz"), gzBody("v2"))
			r.up.setFail(r.url("Packages.gz"), tc.err)
			r.check()
			if got := r.lastResult(); got != tc.result {
				t.Fatalf("result %q, want %q", got, tc.result)
			}
			if kept := r.lookup("Packages.gz") != nil; kept != tc.kept {
				t.Fatalf("row kept = %v, want %v", kept, tc.kept)
			}
			row := r.row()
			if failed := tc.result == "failed"; failed != (row.CheckFailCount == 1) || failed != (row.LastSuccessAt == nil) {
				t.Fatalf("suite row = %+v; want failed = %v", row, failed)
			}
			if got := strings.Count(r.logs.String(), "anchorless index over its size cap"); got != map[bool]int{true: 1}[tc.failLog] {
				t.Fatalf("%d size-cap WARN lines, want %d", got, map[bool]int{true: 1}[tc.failLog])
			}
		})
	}
	t.Run("one answer is enough", func(t *testing.T) {
		r := newFlatRig(t, "/fury")
		pk, src := gzBody("v1"), gzBody("src")
		r.cacheRow("Packages.gz", pk, 1, wallclock.Unix()-3600, rigETag(pk))
		r.cacheRow("Sources.gz", src, 1, wallclock.Unix()-3600, rigETag(src))
		r.up.setFail(r.url("Packages.gz"), &fetch.StatusError{Code: 503})
		r.up.setAt(r.url("Sources.gz"), src)
		r.check()
		if got := r.lastResult(); got != "anchorless_unchanged" {
			t.Fatalf("result %q, want anchorless_unchanged", got)
		}
	})
	t.Run("a changed plain Contents-* and a 503", func(t *testing.T) {
		r := newFlatRig(t, "/fury")
		pk := gzBody("v1")
		r.cacheRow("Packages.gz", pk, 1, wallclock.Unix()-3600, rigETag(pk))
		r.cacheRow("Contents-amd64", []byte("usr/bin/a admin/a\n"), 1, wallclock.Unix()-3600, "")
		r.up.setFail(r.url("Packages.gz"), &fetch.StatusError{Code: 503})
		r.up.setAt(r.url("Contents-amd64"), []byte("usr/bin/a2 admin/a\n"))
		r.check()
		if got := r.lastResult(); got != "failed" {
			t.Fatalf("result %q, want failed (dropped, but no content answer: no sniff can judge it)", got)
		}
		if r.lookup("Contents-amd64") != nil {
			t.Fatal("the changed plain Contents-amd64 was kept, want it dropped for the miss path (deviation 2)")
		}
	})
	t.Run("a 304 to If-Modified-Since alone", func(t *testing.T) {
		r := newFlatRig(t, "/fury")
		fetched, stamp := wallclock.Unix()-3600, "Tue, 15 Nov 1994 08:12:31 GMT"
		h := poolBlob(t, r.cache, gzBody("v1"))
		if err := r.cache.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: "http", CanonicalHost: flatRigHost,
			Path: "/fury/Packages.gz", BlobHash: &h, UpstreamURL: r.url("Packages.gz"), IsMetadata: true, RequestCount: 1,
			LastRequestedAt: &fetched, LastFetchedAt: &fetched, UpstreamLastMod: &stamp}); err != nil {
			t.Fatal(err)
		}
		r.up.setAt(r.url("Packages.gz"), gzBody("v2"))
		r.up.setLastModified(r.url("Packages.gz"), stamp)
		r.check()
		if got := r.lastResult(); got != "anchorless_unchanged" {
			t.Fatalf("result %q, want anchorless_unchanged (a 304 to If-Modified-Since is an answer)", got)
		}
		if u := r.lookup("Packages.gz"); u == nil || *u.BlobHash != h || *u.LastFetchedAt != fetched || strOf(u.UpstreamLastMod) != stamp {
			t.Fatalf("Packages.gz row = %+v; want it as it was", u)
		}
	})
}

// Spec C4 Dispatch 4: the suite's cached anchor rows that upstream now
// answers 404 or 410 are deleted — the probe tree's own answers, no other
// request — a junk InRelease and an unsigned Release among them
// (deviation 1: neither is signed evidence); a junk Release.gpg, never
// probed when Release is absent, stays. Ruling PG-28: a Release answered
// 401 or 403 is deleted too, but only when the same check got a content
// answer for an index request (a 304, a 200 stored): upstream demonstrably
// serves the suite, so the 401/403 is S3's or CloudFront's "no such key".
// With no index answer — the index failing (a 503) or no index row — the
// Release stays: upstream may be blocking the cache outright. (A
// sniff-passing Release.gpg is signed evidence: the gate keeps the suite
// out, TestCheck_AnchorlessGateKeepsSignedSuitesOut.)
func TestCheck_AnchorlessDeletesTheAnchorsUpstreamDropped(t *testing.T) {
	pk := gzBody("v1")
	rel, _ := makeRelease(map[string][]byte{"Packages.gz": pk})
	for _, tc := range []struct {
		name       string
		releaseErr error
		index      string // Packages.gz: "304", "200" (changed bytes), "503", "" (no row cached)
		release    bool   // the Release row kept
		result     string
	}{
		{"Release answered 404", nil, "304", false, "anchorless_refreshed"},
		{"Release answered 401 beside a 304", &fetch.StatusError{Code: 401}, "304", false, "anchorless_refreshed"},
		{"Release answered 403 beside a changed Packages.gz", &fetch.StatusError{Code: 403}, "200", false, "anchorless_refreshed"},
		{"Release answered 401, no index answered", &fetch.StatusError{Code: 401}, "503", true, "failed"},
		{"Release answered 403, no index row", &fetch.StatusError{Code: 403}, "", true, "anchorless_refreshed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newFlatRig(t, "/fury")
			fetched := wallclock.Unix() - 3600
			if tc.index != "" {
				r.cacheRow("Packages.gz", pk, 1, fetched, rigETag(pk))
			}
			r.cacheRow("InRelease", []byte("<html>junk</html>"), 0, fetched, "")
			r.cacheRow("Release", rel, 1, fetched, "")
			r.cacheRow("Release.gpg", []byte("<html>junk</html>"), 0, fetched, "")
			switch tc.index {
			case "304":
				r.up.setAt(r.url("Packages.gz"), pk)
			case "200":
				r.up.setAt(r.url("Packages.gz"), gzBody("v2"))
			case "503":
				r.up.setFail(r.url("Packages.gz"), &fetch.StatusError{Code: 503})
			}
			r.up.setFail(r.url("InRelease"), &fetch.StatusError{Code: 410})
			if tc.releaseErr != nil {
				r.up.setFail(r.url("Release"), tc.releaseErr)
			}
			r.check()
			if got := r.lastResult(); got != tc.result {
				t.Fatalf("result %q, want %q", got, tc.result)
			}
			if r.lookup("InRelease") != nil || (r.lookup("Release") != nil) != tc.release ||
				r.lookup("Release.gpg") == nil || (r.lookup("Packages.gz") != nil) != (tc.index != "") {
				t.Fatalf("rows: InRelease %v, Release %v, Release.gpg %v, Packages.gz %v; want InRelease gone, Release kept = %v",
					r.lookup("InRelease"), r.lookup("Release"), r.lookup("Release.gpg"), r.lookup("Packages.gz"), tc.release)
			}
			if n := r.requests("Release.gpg"); n != 0 {
				t.Fatalf("Release.gpg requested %d times, want 0", n)
			}
		})
	}
	// The final review's scenario (spec lens I-1): a never-adopted flat
	// suite's cached unsigned Release declares Packages.gz v1; upstream, now
	// S3 or CloudFront, answers 403 for the InRelease and Release it no
	// longer has and serves Packages.gz v2. Keeping the Release beside the
	// refreshed Packages.gz would give a [trusted=yes] client a Hash Sum
	// mismatch that never clears.
	t.Run("a stale Release upstream answers 403 beside a refreshed Packages.gz", func(t *testing.T) {
		v2 := gzBody("v2")
		r := newFlatRig(t, "/fury")
		fetched := wallclock.Unix() - 3600
		r.cacheRow("Packages.gz", pk, 1, fetched, rigETag(pk))
		r.cacheRow("Release", rel, 1, fetched, "")
		r.up.setAt(r.url("Packages.gz"), v2)
		r.up.setFail(r.url("InRelease"), &fetch.StatusError{Code: 403})
		r.up.setFail(r.url("Release"), &fetch.StatusError{Code: 403})
		r.check()
		if got := r.lastResult(); got != "anchorless_refreshed" {
			t.Fatalf("result %q, want anchorless_refreshed", got)
		}
		if u := r.lookup("Packages.gz"); u == nil || *u.BlobHash != hashOf(v2) {
			t.Fatalf("Packages.gz row %+v, want v2", u)
		}
		if u := r.lookup("Release"); u != nil {
			t.Fatalf("Release row %+v kept beside the refreshed Packages.gz, want it deleted (ruling PG-28)", u)
		}
	})
}

// Spec C4 Dispatch 3's gate (plan design note 9, deviation 1): signed
// evidence keeps a flat suite out of the anchorless refresh — a
// signed-seen marker for its base, or a sniff-passing InRelease or
// Release.gpg cached under the suite or its /. twin, under either scheme
// (re-marked first) — and so does a dists/ suite's layout: the check is
// the failed check and anchor_absent record it was, and no index is
// requested. A gate that cannot read a cached anchor's pool file fails
// the check too (fail closed), with no record.
func TestCheck_AnchorlessGateKeepsSignedSuitesOut(t *testing.T) {
	signed := simtest.ClearsignedFixture("Origin: fury\n")
	cacheAt := func(t *testing.T, r *flatRig, scheme, path string, body []byte) string {
		t.Helper()
		h := poolBlob(t, r.cache, body)
		fetched := wallclock.Unix() - 3600
		if err := r.cache.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: scheme, CanonicalHost: flatRigHost,
			Path: path, BlobHash: &h, UpstreamURL: scheme + "://" + flatRigHost + path, IsMetadata: true, LastFetchedAt: &fetched}); err != nil {
			t.Fatal(err)
		}
		return h
	}
	for _, tc := range []struct {
		name   string
		suite  string
		setup  func(t *testing.T, r *flatRig)
		record bool
	}{
		{"a signed-seen marker", "/fury", func(t *testing.T, r *flatRig) {
			if err := r.cache.MarkSignedSeenBatch(context.Background(), []cache.SignedSeenMark{{CanonicalScheme: "https",
				CanonicalHost: flatRigHost, Base: "/fury", SeenAt: 1_700_000_000}}); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"a signed InRelease cached under the twin over https", "/fury", func(t *testing.T, r *flatRig) {
			cacheAt(t, r, "https", "/fury/./InRelease", signed)
		}, true},
		{"a Release.gpg cached", "/fury/.", func(t *testing.T, r *flatRig) {
			cacheAt(t, r, "http", "/fury/Release.gpg", armoredSig("fury"))
		}, true},
		{"a dists suite", "/r/dists/x", func(t *testing.T, r *flatRig) {
			r.cacheRow("InRelease", []byte("<html>junk</html>"), 1, wallclock.Unix()-3600, "")
		}, true},
		{"an unreadable pool file", "/fury", func(t *testing.T, r *flatRig) {
			h := cacheAt(t, r, "https", "/fury/InRelease", signed)
			if err := os.Remove(r.cache.BlobPath(h)); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(r.cache.BlobPath(h), 0o700); err != nil {
				t.Fatal(err)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newFlatRig(t, tc.suite)
			if tc.suite != "/r/dists/x" {
				r.cacheRow("Packages.gz", gzBody("v1"), 1, wallclock.Unix()-3600, "")
			}
			tc.setup(t, r)
			r.check()
			if got := r.lastResult(); got != "failed" {
				t.Fatalf("result %q, want failed", got)
			}
			if n := r.requests("Packages.gz"); n != 0 {
				t.Fatalf("Packages.gz requested %d times, want 0", n)
			}
			row := r.row()
			if got := row.AdoptionFailReason != nil && *row.AdoptionFailReason == cache.ReasonAnchorAbsent; got != tc.record {
				t.Fatalf("anchor_absent recorded = %v, want %v (row %+v)", got, tc.record, row)
			}
		})
	}
}

// An answered anchorless check clears what the absent anchors left: the
// anchor_absent record and a pending change (a stamp the anchorless form
// never clears by adopting would queue the suite every tick). The suite
// is next due one refresh interval later.
func TestCheck_AnchorlessClearsTheRecordAndThePendingChange(t *testing.T) {
	r := newFlatRig(t, "/fury")
	ctx := context.Background()
	pk := gzBody("v1")
	r.cacheRow("Packages.gz", pk, 1, wallclock.Unix()-3600, rigETag(pk))
	r.up.setAt(r.url("Packages.gz"), pk)
	if err := r.cache.RecordAdoptionFailure(ctx, "http", flatRigHost, "/fury", cache.AdoptionFailure{Hash: anchorAbsentKey,
		Reason: cache.ReasonAnchorAbsent, Count: 1, FirstAt: 1_799_990_000, LastAt: 1_799_990_000, NextRetryAt: 1_799_999_000}); err != nil {
		t.Fatal(err)
	}
	row := r.row()
	seen := int64(1_799_990_000)
	row.InReleaseChangeSeenAt = &seen
	if err := r.cache.UpdateSuiteFreshness(ctx, row); err != nil {
		t.Fatal(err)
	}
	r.check()
	if got := r.lastResult(); got != "anchorless_unchanged" {
		t.Fatalf("result %q, want anchorless_unchanged", got)
	}
	row = r.row()
	if row.AdoptionFailCount != 0 || row.AdoptionFailReason != nil || row.InReleaseChangeSeenAt != nil {
		t.Fatalf("suite row = %+v; want no record and no pending change", row)
	}
	now := r.clock.now()
	if r.checker.suiteDue(row, now.Add(time.Minute)) || !r.checker.suiteDue(row, now.Add(15*time.Minute)) {
		t.Fatalf("due a minute later %v, a refresh interval later %v; want false, true",
			r.checker.suiteDue(row, now.Add(time.Minute)), r.checker.suiteDue(row, now.Add(15*time.Minute)))
	}
}

// Ruling R-G4: a twin refreshes its own rows only — /fury/.'s refresh
// never requests, replaces or deletes /fury's Packages.gz.
func TestCheck_AnchorlessRefreshesItsOwnRowsOnly(t *testing.T) {
	r := newFlatRig(t, "/fury/.")
	fetched := wallclock.Unix() - 3600
	r.cacheRow("Packages.gz", gzBody("twin"), 1, fetched, "")
	other := poolBlob(t, r.cache, gzBody("suite"))
	if err := r.cache.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: "http", CanonicalHost: flatRigHost,
		Path: "/fury/Packages.gz", BlobHash: &other, UpstreamURL: "http://" + flatRigHost + "/fury/Packages.gz",
		IsMetadata: true, RequestCount: 1, LastFetchedAt: &fetched}); err != nil {
		t.Fatal(err)
	}
	r.check()
	if got := r.lastResult(); got != "anchorless_refreshed" {
		t.Fatalf("result %q, want anchorless_refreshed", got)
	}
	if r.lookup("Packages.gz") != nil {
		t.Fatal("the twin's Packages.gz survived its 404")
	}
	u, err := r.cache.LookupURL(context.Background(), "http", flatRigHost, "/fury/Packages.gz")
	if err != nil || *u.BlobHash != other || *u.LastFetchedAt != fetched {
		t.Fatalf("/fury/Packages.gz = %+v (%v); want it untouched", u, err)
	}
	if n := len(r.up.etagsSent("http://" + flatRigHost + "/fury/Packages.gz")); n != 0 {
		t.Fatalf("/fury/Packages.gz requested %d times, want 0", n)
	}
}

// A shutdown during the refresh ends the check with no write and no
// failed-check line (as a cancelled probe does); the rows after the cut
// are not requested.
func TestCheck_AnchorlessShutdownWritesNothing(t *testing.T) {
	r := newFlatRig(t, "/fury")
	pk, src := gzBody("v1"), gzBody("src")
	r.cacheRow("Packages.gz", pk, 1, wallclock.Unix()-3600, rigETag(pk))
	r.cacheRow("Sources.gz", src, 1, wallclock.Unix()-3600, rigETag(src))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.up.before = func(url string) {
		if url == r.url("Packages.gz") {
			cancel()
		}
	}
	r.up.setFail(r.url("Packages.gz"), context.Canceled)
	r.checker.Check(ctx, "http", flatRigHost, "/fury")
	r.checker.WaitForAdoptions()
	if got := r.lastResult(); got != "" {
		t.Fatalf("result %q, want no check line", got)
	}
	if row := r.row(); row.LastCheckAt != nil || row.CheckFailCount != 0 {
		t.Fatalf("suite row = %+v; want nothing written", row)
	}
	if n := r.requests("Sources.gz"); n != 0 {
		t.Fatalf("Sources.gz requested %d times, want 0", n)
	}
}

// Spec C4 Dispatch 4: "If a Release/InRelease later becomes present, the
// suite moves to the normal forms" — the next check adopts it.
func TestCheck_AnAnchorlessSuiteThatPublishesAnInReleaseAdopts(t *testing.T) {
	r := newFlatRig(t, "/fury")
	pk := gzBody("v1")
	r.cacheRow("Packages.gz", pk, 1, wallclock.Unix()-3600, rigETag(pk))
	r.up.setAt(r.url("Packages.gz"), pk)
	r.check()
	if got := r.lastResult(); got != "anchorless_unchanged" {
		t.Fatalf("first check: %q, want anchorless_unchanged", got)
	}
	pkgs := fakePackagesStanzas(map[string]string{"x_1_amd64.deb": testHex64})
	rel, _ := makeRelease(map[string][]byte{"Packages": pkgs})
	r.up.setAt(r.url("InRelease"), simtest.ClearsignedFixture(string(rel)))
	r.members.put(r.url("Packages"), pkgs)
	r.clock.advance(20 * time.Minute)
	r.check()
	if got, row := r.lastResult(), r.row(); got != "first_adoption" || row.CurrentSnapshotID == nil {
		t.Fatalf("second check: %q, current %v; want a first adoption", got, row.CurrentSnapshotID)
	}
}

// Ruling PG-6's note (whether Gemfury or goreleaser honour ETag or
// If-Modified-Since cannot be determined from the code): against a server
// that ignores both, a 200 with the bytes already cached changes nothing —
// the blob and last_fetched_at stay, so Last-Modified does not move — and
// the check is anchorless_unchanged, whether the server repeats the stored
// ETag or sends another (only the stored validators follow it).
func TestCheck_AnchorlessIgnoredValidatorsLeaveAnUnchangedRow(t *testing.T) {
	for _, stored := range []string{"the same ETag", "another ETag"} {
		t.Run(stored, func(t *testing.T) {
			r := newFlatRig(t, "/fury")
			fetched := wallclock.Unix() - 3600
			pk := gzBody("v1")
			etag := rigETag(pk)
			if stored == "another ETag" {
				etag = "e-older"
			}
			h := r.cacheRow("Packages.gz", pk, 1, fetched, etag)
			r.up.setAt(r.url("Packages.gz"), pk)
			r.up.ignoreValidators = true
			r.check()
			if got := r.lastResult(); got != "anchorless_unchanged" {
				t.Fatalf("result %q, want anchorless_unchanged", got)
			}
			if u := r.lookup("Packages.gz"); u == nil || *u.BlobHash != h || *u.LastFetchedAt != fetched || strOf(u.UpstreamETag) != rigETag(pk) {
				t.Fatalf("Packages.gz row = %+v; want its blob and last_fetched_at kept, the ETag upstream's", u)
			}
		})
	}
}

// Spec C4 Dispatch 3's gate fails closed (plan design note 9): when
// never-downgrade's read fails — here the signed-seen table cannot be
// read — the check fails with no record and no index is requested; a gate
// error never admits a suite to the anchorless refresh.
func TestCheck_AnchorlessGateFailsClosedOnAMarkerReadError(t *testing.T) {
	r := newFlatRig(t, "/fury")
	pk := gzBody("v1")
	r.cacheRow("Packages.gz", pk, 1, wallclock.Unix()-3600, rigETag(pk))
	r.up.setAt(r.url("Packages.gz"), gzBody("v2"))
	db, err := sql.Open("sqlite", filepath.Join(r.cache.Dir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`ALTER TABLE suite_signed_seen RENAME TO suite_signed_seen_gone`); err != nil {
		t.Fatal(err)
	}
	r.check()
	if got := r.lastResult(); got != "failed" {
		t.Fatalf("result %q, want failed (the gate could not rule out signed history)", got)
	}
	if n := r.requests("Packages.gz"); n != 0 {
		t.Fatalf("Packages.gz requested %d times, want 0", n)
	}
	if row := r.row(); row.AdoptionFailReason != nil {
		t.Fatalf("record %q, want none (an I/O fault is no evidence)", strOf(row.AdoptionFailReason))
	}
}

// Plan design note 10: a 304 to a request that carried no validators is no
// answer (classify treats it so): a row cached without an ETag or a
// Last-Modified, answered 304, is unknown — with no other row the check
// fails (the streak backs off) — and the row stays as it was.
func TestCheck_AnchorlessValidatorlessNotModifiedIsUnknown(t *testing.T) {
	r := newFlatRig(t, "/fury")
	fetched := wallclock.Unix() - 3600
	h := r.cacheRow("Packages.gz", gzBody("v1"), 1, fetched, "")
	r.up.mu.Lock()
	r.up.notModified = map[string]bool{r.url("Packages.gz"): true}
	r.up.mu.Unlock()
	r.check()
	if got := r.lastResult(); got != "failed" {
		t.Fatalf("result %q, want failed (a validator-less 304 answers nothing)", got)
	}
	if u := r.lookup("Packages.gz"); u == nil || *u.BlobHash != h || *u.LastFetchedAt != fetched {
		t.Fatalf("Packages.gz row = %+v; want it as it was", u)
	}
	if row := r.row(); row.CheckFailCount != 1 {
		t.Fatalf("check_fail_count = %d, want 1", row.CheckFailCount)
	}
}

// Plan design note 12: every anchorless request carries the 64 MiB body
// cap (fetch.Conditional reads a body into memory, so the cap bounds a
// check's memory whatever upstream sends).
func TestCheck_AnchorlessRequestsCarryTheBodyCap(t *testing.T) {
	r := newFlatRig(t, "/fury")
	pk := gzBody("v1")
	r.cacheRow("Packages.gz", pk, 1, wallclock.Unix()-3600, rigETag(pk))
	r.cacheRow("Contents-amd64", []byte("usr/bin/a admin/a\n"), 1, wallclock.Unix()-3600, "")
	r.up.setAt(r.url("Packages.gz"), pk)
	r.up.setAt(r.url("Contents-amd64"), []byte("usr/bin/a admin/a\n"))
	r.check()
	r.up.mu.Lock()
	defer r.up.mu.Unlock()
	for _, name := range []string{"Packages.gz", "Contents-amd64"} {
		if got := r.up.limits[r.url(name)]; got != 64<<20 {
			t.Errorf("%s requested with a body cap of %d, want %d", name, got, 64<<20)
		}
	}
}

// Plan design note 10's edge, pinned: a flat suite with no cached index
// row — its client fetched only an unsigned Release, which upstream now
// answers 404 — has nothing to revalidate, yet its anchor's 404 is still
// applied (the Release row is deleted) and the check is answered:
// anchorless_refreshed, a success. A failed check needs index requests
// with no content answer (ruling PG-15 composing this with design review
// M-7): with no request there is nothing to answer.
func TestCheck_AnchorlessWithNoIndexRowAnswersAndDeletesTheAnchor(t *testing.T) {
	r := newFlatRig(t, "/fury")
	rel, _ := makeRelease(map[string][]byte{"Packages.gz": gzBody("v1")})
	r.cacheRow("Release", rel, 1, wallclock.Unix()-3600, "")
	r.check()
	if got := r.lastResult(); got != "anchorless_refreshed" {
		t.Fatalf("result %q, want anchorless_refreshed", got)
	}
	if r.lookup("Release") != nil {
		t.Fatal("the cached Release upstream answers 404 was kept")
	}
	if row := r.row(); row.CheckFailCount != 0 || row.LastSuccessAt == nil {
		t.Fatalf("suite row = %+v; want an answered check", row)
	}
}

// Ruling PG-1: adoption.adopt_unsigned = false also stops the anchorless
// refresh. A flat suite the gate admits is refused as an unsigned
// candidate is — the record unsigned_disabled (the status page names the
// knob; the record paces the suite), an answered check changed_deferred
// with form anchorless and no pending change left (a stamp set before is
// cleared) — and no index is requested, replaced or deleted, nor a cached
// anchor upstream answers 410 or 404 (a junk InRelease, an unsigned
// Release). A check inside the pacing asks nothing; turned back on, the
// knob's next check after the pacing refreshes the suite and clears the
// record.
func TestCheck_TheKnobOffStopsTheAnchorlessRefresh(t *testing.T) {
	r := newFlatRig(t, "/fury")
	r.adopter.adoptUnsigned = false
	fetched := wallclock.Unix() - 3600
	pk := gzBody("v1")
	h := r.cacheRow("Packages.gz", pk, 1, fetched, rigETag(pk))
	rel, _ := makeRelease(map[string][]byte{"Packages.gz": pk})
	r.cacheRow("Release", rel, 1, fetched, "")
	r.cacheRow("InRelease", []byte("<html>junk</html>"), 0, fetched, "")
	r.up.setFail(r.url("InRelease"), &fetch.StatusError{Code: 410})
	r.up.setAt(r.url("Packages.gz"), gzBody("v2"))
	row := r.row()
	seen := int64(1_799_990_000)
	row.InReleaseChangeSeenAt = &seen
	if err := r.cache.UpdateSuiteFreshness(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	r.check()
	if got := r.lastResult(); got != "changed_deferred" {
		t.Fatalf("result %q, want changed_deferred", got)
	}
	if n := r.requests("Packages.gz"); n != 0 {
		t.Fatalf("Packages.gz requested %d times with the knob off, want 0", n)
	}
	if u := r.lookup("Packages.gz"); u == nil || *u.BlobHash != h || *u.LastFetchedAt != fetched {
		t.Fatalf("Packages.gz row = %+v; want it as first cached", u)
	}
	if r.lookup("Release") == nil || r.lookup("InRelease") == nil {
		t.Fatalf("Release %v, InRelease %v: a cached anchor upstream answers 404 or 410 was deleted with the knob off",
			r.lookup("Release"), r.lookup("InRelease"))
	}
	row = r.row()
	if strOf(row.AdoptionFailReason) != cache.ReasonUnsignedDisabled || row.LastSuccessAt == nil || row.CheckFailCount != 0 ||
		row.InReleaseChangeSeenAt != nil || row.CurrentSnapshotID != nil {
		t.Fatalf("suite row = %+v; want the unsigned_disabled record, an answered check, no pending change", row)
	}
	probes := r.requests("InRelease")
	r.clock.advance(time.Minute)
	r.check()
	if n := r.requests("InRelease"); n != probes {
		t.Fatalf("a check inside the pacing probed InRelease (%d, was %d)", n, probes)
	}
	r.adopter.adoptUnsigned = true
	r.clock.advance(15 * time.Minute)
	r.check()
	if got := r.lastResult(); got != "anchorless_refreshed" {
		t.Fatalf("knob on: result %q, want anchorless_refreshed", got)
	}
	if row := r.row(); row.AdoptionFailCount != 0 || row.AdoptionFailReason != nil {
		t.Fatalf("knob on: suite row = %+v; want the record cleared", row)
	}
}

// Ruling PG-1's other side: with the knob off, a flat suite the gate keeps
// out (signed evidence: a marker for its base) keeps the failed check and
// anchor_absent record it has with the knob on — the knob never hides a
// stripped signature behind unsigned_disabled.
func TestCheck_TheKnobOffKeepsSignedSuitesAnchorAbsent(t *testing.T) {
	r := newFlatRig(t, "/fury")
	r.adopter.adoptUnsigned = false
	r.cacheRow("Packages.gz", gzBody("v1"), 1, wallclock.Unix()-3600, "")
	if err := r.cache.MarkSignedSeenBatch(context.Background(), []cache.SignedSeenMark{{CanonicalScheme: "http",
		CanonicalHost: flatRigHost, Base: "/fury", SeenAt: 1_700_000_000}}); err != nil {
		t.Fatal(err)
	}
	r.check()
	if got := r.lastResult(); got != "failed" {
		t.Fatalf("result %q, want failed", got)
	}
	if row := r.row(); strOf(row.AdoptionFailReason) != cache.ReasonAnchorAbsent {
		t.Fatalf("record %q, want anchor_absent", strOf(row.AdoptionFailReason))
	}
}

// Design review M-1: a 200 whose bytes are the cached blob's never
// re-stores that blob — no pool re-hash, no PutBlob, which would restart
// its refcount_zeroed_at (where the blob GC grace starts) — and only the
// changed validators are written (cache.RefreshURLPathBlob, old → old):
// the blob and last_fetched_at stay, and the check is anchorless_unchanged.
// A plain Contents-* alike: it has no sniff, but its cached bytes need
// none, and without its new ETag it would be sent in full at every check.
func TestCheck_AnchorlessUnchangedBytesWriteOnlyTheirValidators(t *testing.T) {
	r := newFlatRig(t, "/fury")
	fetched := wallclock.Unix() - 3600
	pk := gzBody("v1")
	h := r.cacheRow("Packages.gz", pk, 1, fetched, "e-older")
	contents := []byte("usr/bin/a admin/a\n")
	hc := r.cacheRow("Contents-amd64", contents, 1, fetched, "c-older")
	db, err := sql.Open("sqlite", filepath.Join(r.cache.Dir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`UPDATE blob SET refcount_zeroed_at = 1 WHERE hash = ?`, h); err != nil {
		t.Fatal(err)
	}
	r.up.setAt(r.url("Packages.gz"), pk)
	r.up.setAt(r.url("Contents-amd64"), contents)
	r.check()
	if got := r.lastResult(); got != "anchorless_unchanged" {
		t.Fatalf("result %q, want anchorless_unchanged", got)
	}
	var zeroed sql.NullInt64
	if err := db.QueryRow(`SELECT refcount_zeroed_at FROM blob WHERE hash = ?`, h).Scan(&zeroed); err != nil {
		t.Fatal(err)
	}
	if !zeroed.Valid || zeroed.Int64 != 1 {
		t.Fatalf("blob refcount_zeroed_at = %v, want 1: the unchanged blob was stored again", zeroed)
	}
	if u := r.lookup("Packages.gz"); u == nil || *u.BlobHash != h || *u.LastFetchedAt != fetched || strOf(u.UpstreamETag) != rigETag(pk) {
		t.Fatalf("Packages.gz row = %+v; want the blob and last_fetched_at kept, upstream's ETag stored", u)
	}
	if u := r.lookup("Contents-amd64"); u == nil || *u.BlobHash != hc || *u.LastFetchedAt != fetched || strOf(u.UpstreamETag) != rigETag(contents) {
		t.Fatalf("Contents-amd64 row = %+v; want the blob and last_fetched_at kept, upstream's ETag stored", u)
	}
}

// Design review M-3: the refresh asks Packages*, then Sources*, then
// Contents-* (anchorlessRank), so a large, slow Contents-* cannot take the
// check's deadline from the indexes apt needs: with a Contents-amd64.gz
// that stalls past the deadline (asked last, it ends unknown), the changed
// Packages.gz is still replaced and the check is anchorless_refreshed. In
// path order Contents-amd64.gz comes first, and Packages.gz, asked after
// the deadline, would end unknown too: a failed check.
func TestCheck_AnchorlessAsksPackagesBeforeContents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newFlatRig(t, "/fury")
		fetched := int64(rigStart - 3600)
		pk := gzBody("v1")
		old := r.cacheRow("Packages.gz", pk, 1, fetched, rigETag(pk))
		r.cacheRow("Contents-amd64.gz", gzBody("contents"), 1, fetched, "")
		r.cacheRow("Sources.gz", gzBody("src"), 1, fetched, rigETag(gzBody("src")))
		r.up.setAt(r.url("Packages.gz"), gzBody("v2"))
		r.up.setAt(r.url("Sources.gz"), gzBody("src"))
		r.up.setAt(r.url("Contents-amd64.gz"), gzBody("contents v2"))
		var asked []string
		r.up.before = func(url string) {
			asked = append(asked, strings.TrimPrefix(url, r.url("")))
			if url == r.url("Contents-amd64.gz") {
				time.Sleep(checkDeadline + time.Minute) // a stalled body: past the check's deadline
			}
		}
		r.check()
		if want := []string{"InRelease", "Release", "Packages.gz", "Sources.gz", "Contents-amd64.gz"}; !slices.Equal(asked, want) {
			t.Fatalf("asked %v, want %v", asked, want)
		}
		if got := r.lastResult(); got != "anchorless_refreshed" {
			t.Fatalf("result %q, want anchorless_refreshed (the stalled Contents-* asked last)", got)
		}
		if u := r.lookup("Packages.gz"); u == nil || *u.BlobHash == old {
			t.Fatalf("Packages.gz row = %+v; want its new blob", u)
		}
	})
}

// Design review M-6: the over-cap WARN is rate-limited process-wide — at
// most one line a minute, carrying the count of the lines it suppressed —
// so minted anchorless suites cannot flood the log.
func TestCheck_AnchorlessOverCapWarnIsRateLimited(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newFlatRig(t, "/fury")
		pk := gzBody("v1")
		r.cacheRow("Packages.gz", pk, 1, rigStart-3600, rigETag(pk))
		r.up.setFail(r.url("Packages.gz"), fmt.Errorf("conditional: %w", fetch.ErrConditionalBodyTooLarge))
		for _, wait := range []time.Duration{0, 0, time.Minute, 0, time.Minute} {
			time.Sleep(wait)
			r.clock.advance(20 * time.Minute)
			r.check()
		}
		var suppressed []float64
		for _, line := range strings.Split(r.logs.String(), "\n") {
			var m map[string]any
			if line == "" || json.Unmarshal([]byte(line), &m) != nil || m["msg"] != "freshness: anchorless index over its size cap" {
				continue
			}
			n, _ := m["suppressed"].(float64)
			suppressed = append(suppressed, n)
		}
		if !slices.Equal(suppressed, []float64{0, 1, 1}) {
			t.Fatalf("over-cap WARN lines with suppressed %v, want [0 1 1]: one line a minute, each counting the one it hid", suppressed)
		}
	})
}

// seedFlatSuiteOn gives the flat rig's cache a second suite: path on host,
// interested at rigStart, with a client-created Packages.gz of body that
// upstream now answers with newBody.
func seedFlatSuiteOn(t *testing.T, r *flatRig, host, path string, body, newBody []byte) {
	t.Helper()
	ctx := context.Background()
	interest, fetched := int64(rigStart), int64(rigStart-3600)
	if err := r.cache.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: host,
		SuitePath: path, LastRequestedAt: &interest}); err != nil {
		t.Fatal(err)
	}
	h := poolBlob(t, r.cache, body)
	url := "http://" + host + path + "/Packages.gz"
	if err := r.cache.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: host, Path: path + "/Packages.gz",
		BlobHash: &h, UpstreamURL: url, IsMetadata: true, RequestCount: 1, LastRequestedAt: &fetched,
		LastFetchedAt: &fetched}); err != nil {
		t.Fatal(err)
	}
	r.up.setAt(url, newBody)
}

// Ruling PG-8 (plan design note 12): at most two anchorless bodies are in
// flight at once, process-wide, whatever the number of hosts — per-host
// slots cannot bound it. Three anchorless suites on three hosts, checked
// at once by one checker, hold two requests open and queue the third,
// which goes once a slot is free; all three checks end.
func TestCheck_AnchorlessBodiesAreBoundedProcessWide(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newFlatRig(t, "/fury")
		hosts := []string{flatRigHost, "fh2", "fh3"}
		r.cacheRow("Packages.gz", gzBody("v1"), 1, rigStart-3600, "")
		r.up.setAt(r.url("Packages.gz"), gzBody("v2"))
		for _, host := range hosts[1:] {
			seedFlatSuiteOn(t, r, host, "/fury", gzBody("v1"), gzBody("v2"))
		}
		release := make(chan struct{})
		var inFlight, peak atomic.Int32
		r.up.before = func(url string) {
			if !strings.HasSuffix(url, "/Packages.gz") {
				return
			}
			n := inFlight.Add(1)
			for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
			}
			<-release
			inFlight.Add(-1)
		}
		var wg sync.WaitGroup
		for _, host := range hosts {
			wg.Go(func() { r.checker.Check(context.Background(), "http", host, "/fury") })
		}
		synctest.Wait()
		if got := peak.Load(); got != 2 {
			t.Fatalf("peak anchorless bodies in flight %d, want 2", got)
		}
		close(release)
		wg.Wait()
		if got := strings.Count(r.logs.String(), `"result":"anchorless_refreshed"`); got != len(hosts) {
			t.Fatalf("%d anchorless_refreshed checks, want %d: every check ends", got, len(hosts))
		}
	})
}

// Ruling PG-8's slots survive a panic (Global Constraints: every
// goroutine recovers panics): a refresh that panics while it holds a slot
// gives it back as the panic unwinds to the check's recover
// (checkLockedSafe), so after two panicking checks — as many as there are
// slots — a third suite's refresh still gets one at once and ends
// anchorless_refreshed instead of waiting out its deadline.
func TestCheck_AnchorlessBodySlotsSurviveAPanic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newFlatRig(t, "/fury")
		hosts := []string{flatRigHost, "fh2", "fh3"}
		r.cacheRow("Packages.gz", gzBody("v1"), 1, rigStart-3600, "")
		r.up.setAt(r.url("Packages.gz"), gzBody("v2"))
		for _, host := range hosts[1:] {
			seedFlatSuiteOn(t, r, host, "/fury", gzBody("v1"), gzBody("v2"))
		}
		r.up.before = func(url string) {
			if strings.HasSuffix(url, "/Packages.gz") && !strings.HasPrefix(url, "http://fh3/") {
				panic("upstream boom")
			}
		}
		for _, host := range hosts[:2] {
			r.checker.Check(context.Background(), "http", host, "/fury")
		}
		if n := strings.Count(r.logs.String(), "freshness_check_panicked"); n != 2 {
			t.Fatalf("%d freshness_check_panicked lines, want 2: each check recovered its refresh's panic", n)
		}
		start := time.Now()
		r.checker.Check(context.Background(), "http", "fh3", "/fury")
		if got := r.lastResult(); got != "anchorless_refreshed" || time.Since(start) != 0 {
			t.Fatalf("result %q after %v, want anchorless_refreshed at once: a panic kept a body slot", got, time.Since(start))
		}
	})
}

// rigDB opens a second connection to the flat rig's cache database, for a
// test that injects a fault the cache's own API cannot produce.
func (r *flatRig) rigDB() *sql.DB {
	r.t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(r.cache.Dir(), "cache.db"))
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = db.Close() })
	return db
}

// suiteOn reads host's /fury suite row.
func (r *flatRig) suiteOn(host string) cache.SuiteFreshness {
	r.t.Helper()
	s, err := r.cache.GetSuiteFreshness(context.Background(), "http", host, "/fury")
	if err != nil {
		r.t.Fatal(err)
	}
	return *s
}

// Ruling PG-24 (the Task 10 review's M2, first property): a check waits for
// a body slot only within its own fetchCtx. Two stalled requests hold both
// slots; a third suite's check ends at its deadline — a failed check, its
// index never asked — or, when its context ends during the wait (a
// shutdown), at once, writing nothing.
func TestCheck_AnchorlessSlotWaitEndsWithTheCheck(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "the deadline", true: "a shutdown"}[shutdown], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := newFlatRig(t, "/fury")
				r.cacheRow("Packages.gz", gzBody("v1"), 1, rigStart-3600, "")
				r.up.setAt(r.url("Packages.gz"), gzBody("v2"))
				for _, host := range []string{"fh2", "fh3"} {
					seedFlatSuiteOn(t, r, host, "/fury", gzBody("v1"), gzBody("v2"))
				}
				release := make(chan struct{})
				r.up.before = func(url string) {
					if strings.HasSuffix(url, "/Packages.gz") && !strings.HasPrefix(url, "http://fh3/") {
						<-release // a stalled body keeps its slot
					}
				}
				var holders sync.WaitGroup
				for _, host := range []string{flatRigHost, "fh2"} {
					holders.Go(func() { r.checker.Check(context.Background(), "http", host, "/fury") })
				}
				synctest.Wait() // both slots held
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan struct{})
				start := time.Now()
				go func() {
					defer close(done)
					r.checker.Check(ctx, "http", "fh3", "/fury")
				}()
				synctest.Wait() // fh3's check waits for a slot
				if shutdown {
					cancel()
				}
				select {
				case <-done:
				case <-time.After(checkDeadline + time.Hour):
				}
				elapsed := time.Since(start)
				close(release)
				holders.Wait()
				<-done
				want := checkDeadline
				if shutdown {
					want = 0
				}
				if elapsed != want {
					t.Fatalf("fh3's check ended after %v, want %v: the slot wait must end with the check", elapsed, want)
				}
				if n := len(r.up.etagsSent("http://fh3/fury/Packages.gz")); n != 0 {
					t.Fatalf("fh3's Packages.gz asked %d times, want never (no slot)", n)
				}
				row := r.suiteOn("fh3")
				switch {
				case shutdown && (row.LastCheckAt != nil || row.CheckFailCount != 0):
					t.Fatalf("fh3 row %+v, want nothing written by a check a shutdown ended", row)
				case !shutdown && row.CheckFailCount != 1:
					t.Fatalf("fh3 check_fail_count %d, want 1 (a failed check at its deadline)", row.CheckFailCount)
				}
			})
		})
	}
}

// lateFetcher answers like its Fetcher, but the answer for url comes back
// only after the check's deadline has passed: the request's outcome is
// decided in time, and what the refresh writes for it is written after
// fetchCtx ended.
type lateFetcher struct {
	Fetcher
	url string
}

func (f lateFetcher) Conditional(ctx context.Context, target *fetch.Target, etag, lastmod string, maxBody int64) (*fetch.ConditionalResult, error) {
	res, err := f.Fetcher.Conditional(ctx, target, etag, lastmod, maxBody)
	if target.URL == f.url {
		time.Sleep(checkDeadline + time.Minute)
	}
	return res, err
}

// Plan design note 10 (the Task 10 review's M2, second property): the
// refresh's writes run under the lifecycle context, not fetchCtx, so an
// answer that arrives as the deadline passes is still committed: a
// replacement's pool file and row, and a 404's delete.
func TestCheck_AnchorlessWritesAfterTheDeadlineCommit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		upstream []byte // nil: 404
	}{{"a replacement", gzBody("v2")}, {"a delete", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := newFlatRig(t, "/fury")
				old := r.cacheRow("Packages.gz", gzBody("v1"), 1, rigStart-3600, "")
				if tc.upstream != nil {
					r.up.setAt(r.url("Packages.gz"), tc.upstream)
				}
				r.checker.fetcher = lateFetcher{Fetcher: r.up, url: r.url("Packages.gz")}
				start := time.Now()
				r.check()
				if time.Since(start) <= checkDeadline {
					t.Fatalf("the check ended after %v, want past the %v deadline", time.Since(start), checkDeadline)
				}
				if got := r.lastResult(); got != "anchorless_refreshed" {
					t.Fatalf("result %q, want anchorless_refreshed: the write after the deadline committed", got)
				}
				u := r.lookup("Packages.gz")
				switch {
				case tc.upstream == nil && u != nil:
					t.Fatalf("Packages.gz row %+v, want deleted", u)
				case tc.upstream != nil && (u == nil || *u.BlobHash == old || *u.BlobHash != hashOf(tc.upstream)):
					t.Fatalf("Packages.gz row %+v, want the new blob", u)
				}
			})
		})
	}
}

// Design review M-4 for the anchors (the Task 10 review's M3): the refresh
// deletes a cached anchor its probe answered 404 only while the row still
// holds the blob read before the probe tree ran, so a newer Release a
// client's miss stored during the probe's window is kept.
func TestCheck_AnchorlessKeepsAnAnchorAClientStoredDuringItsProbe(t *testing.T) {
	r := newFlatRig(t, "/fury")
	r.cacheRow("Release", []byte("Origin: fury\n"), 1, rigStart-3600, "")
	pk := gzBody("v1")
	r.cacheRow("Packages.gz", pk, 1, rigStart-3600, rigETag(pk))
	r.up.setAt(r.url("Packages.gz"), pk)
	newer := []byte("Origin: fury\nLabel: newer\n")
	r.up.before = func(url string) {
		if url == r.url("Release") {
			r.cacheRow("Release", newer, 2, rigStart, "") // a client's miss, while the probe is out
		}
	}
	r.check()
	if got := r.lastResult(); got != "anchorless_unchanged" {
		t.Fatalf("result %q, want anchorless_unchanged (a 304, nothing deleted)", got)
	}
	if u := r.lookup("Release"); u == nil || *u.BlobHash != hashOf(newer) {
		t.Fatalf("Release row %+v, want the client's newer Release kept", u)
	}
}

// The Task 10 review's M4: a delete that fails is logged and is no answer —
// its 404s stop counting, as a failed store makes its row unknown — so a
// check its 404s alone answered fails (the streak backs off) and the rows
// stay; another content answer still answers the check.
func TestCheck_AnchorlessFailedDeleteIsNoAnswer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cache  func(r *flatRig)
		result string
		err    string // the failed check's err prefix
	}{
		{"an index's 404", func(r *flatRig) { r.cacheRow("Packages.gz", gzBody("v1"), 1, rigStart-3600, "") }, "failed",
			"every anchorless index request ended unknown: delete anchorless rows: "},
		{"an anchor's 404, no index row", func(r *flatRig) { r.cacheRow("Release", []byte("Origin: fury\n"), 1, rigStart-3600, "") }, "failed",
			"delete anchorless rows: "},
		{"a 404 beside a 304", func(r *flatRig) {
			pk := gzBody("v1")
			r.cacheRow("Packages.gz", pk, 1, rigStart-3600, rigETag(pk))
			r.up.setAt(r.url("Packages.gz"), pk)
			r.cacheRow("Sources.gz", gzBody("src"), 1, rigStart-3600, "")
		}, "anchorless_unchanged", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newFlatRig(t, "/fury")
			tc.cache(r)
			if _, err := r.rigDB().Exec(`CREATE TRIGGER no_url_path_delete BEFORE DELETE ON url_path
BEGIN SELECT RAISE(ABORT, 'injected delete failure'); END`); err != nil {
				t.Fatal(err)
			}
			before := r.listed()
			r.check()
			if got := r.lastResult(); got != tc.result {
				t.Fatalf("result %q, want %s", got, tc.result)
			}
			if n := strings.Count(r.logs.String(), `"msg":"freshness: delete anchorless rows failed"`); n != 1 {
				t.Fatalf("%d delete failure WARNs, want 1", n)
			}
			if after := r.listed(); !slices.Equal(after, before) {
				t.Fatalf("rows %v after the check, want %v: the delete failed", after, before)
			}
			if want := map[string]int64{"failed": 1}[tc.result]; r.row().CheckFailCount != want {
				t.Fatalf("check_fail_count %d, want %d", r.row().CheckFailCount, want)
			}
			if tc.err != "" {
				recs := logRecords(t, r.logs, "freshness check failed")
				if len(recs) != 1 || !strings.HasPrefix(fmt.Sprint(recs[0]["err"]), tc.err) {
					t.Fatalf("failed check lines %v, want one whose err starts %q", recs, tc.err)
				}
			}
		})
	}
}

// listed is the suite's cached metadata row paths.
func (r *flatRig) listed() []string {
	r.t.Helper()
	rows, err := r.cache.ListSuiteMetadataRows(context.Background(), suiteKeyOf(r.suite))
	if err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for _, u := range rows {
		out = append(out, u.Path)
	}
	return out
}

// The Task 10 review's M5: a read fault of the refresh's index rows follows
// checkFirst's convention for a local read fault — one WARN naming the
// suite, no index request, no delete and no write (no check stamp, no
// streak, no record): the next check tries again. The fault is a row the
// read cannot scan; the gate's reads and the client-created Release (so
// checkFirst's own index read is skipped) get past it.
func TestCheck_AnchorlessIndexReadFaultIsALocalFault(t *testing.T) {
	r := newFlatRig(t, "/fury")
	r.cacheRow("Release", []byte("Origin: fury\n"), 1, rigStart-3600, "")
	r.cacheRow("Packages.gz", gzBody("v1"), 1, rigStart-3600, "")
	if _, err := r.rigDB().Exec(`INSERT INTO url_path(canonical_scheme,canonical_host,path,upstream_url,is_metadata,request_count)
 VALUES('http',?,'/fury/Contents-x','http://fh/fury/Contents-x',1,'not a number')`, flatRigHost); err != nil {
		t.Fatal(err)
	}
	r.check()
	warns := 0
	for _, line := range strings.Split(r.logs.String(), "\n") {
		var m map[string]any
		if line != "" && json.Unmarshal([]byte(line), &m) == nil && m["msg"] == "freshness: read anchor rows failed" {
			if m["level"] != "WARN" || m["suite_path"] != "/fury" {
				t.Fatalf("read failure line %v, want a WARN naming /fury", m)
			}
			warns++
		}
	}
	if warns != 1 || r.lastResult() != "" {
		t.Fatalf("%d read failure WARNs, last result %q; want 1 and no check line", warns, r.lastResult())
	}
	if n := r.requests("Packages.gz"); n != 0 {
		t.Fatalf("Packages.gz asked %d times, want never", n)
	}
	if r.lookup("Release") == nil {
		t.Fatal("the Release row was deleted, want it kept: nothing was decided")
	}
	if row := r.row(); row.LastCheckAt != nil || row.CheckFailCount != 0 || row.AdoptionFailReason != nil {
		t.Fatalf("suite row %+v, want no check stamp, no streak and no record", row)
	}
}
