package freshness

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// junkSuite gives suite a row a client asked for just now — so idleness
// never reaps it — and no cached anchor row.
func (r *reaperRig) junkSuite(suite string) {
	r.t.Helper()
	ctx := context.Background()
	if err := r.cache.EnsureSuiteForTest(ctx, "http", "h", suite); err != nil {
		r.t.Fatal(err)
	}
	now := reapNow
	if err := r.cache.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h",
		SuitePath: suite, LastRequestedAt: &now}); err != nil {
		r.t.Fatal(err)
	}
}

// cacheAnchor caches body as suite's anchor name (InRelease, Release or
// Release.gpg), fetched at the unix time fetched.
func (r *reaperRig) cacheAnchor(suite, name, body string, fetched int64) {
	r.t.Helper()
	ctx := context.Background()
	h, err := r.adopter.writeBlobBytes(ctx, []byte(body))
	if err != nil {
		r.t.Fatal(err)
	}
	r.cacheAnchorBlob(suite, name, h, fetched)
}

// cacheAnchorBlob points suite's anchor name at blob h — a blob row whose
// pool file need not exist — fetched at the unix time fetched.
func (r *reaperRig) cacheAnchorBlob(suite, name, h string, fetched int64) {
	r.t.Helper()
	if err := r.cache.PutBlob(context.Background(), h, 1); err != nil {
		r.t.Fatal(err)
	}
	if err := r.cache.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h",
		Path: suite + "/" + name, BlobHash: &h, UpstreamURL: "http://h" + suite + "/" + name, IsMetadata: true,
		RequestCount: 1, LastFetchedAt: &fetched}); err != nil {
		r.t.Fatal(err)
	}
}

// record gives suite an adoption record with reason.
func (r *reaperRig) record(suite, reason string) {
	r.t.Helper()
	if err := r.cache.RecordAdoptionFailure(context.Background(), "http", "h", suite, cache.AdoptionFailure{Hash: "k",
		Reason: reason, Count: 1, FirstAt: reapNow, LastAt: reapNow, NextRetryAt: reapNow + 60}); err != nil {
		r.t.Fatal(err)
	}
}

const softPage = "<!DOCTYPE html><html><body>Not Found</body></html>\n"

// Ruling R-F3 with ruling PF-3's content rule: a tracked suite with no
// current snapshot whose cached anchor rows are all junk evidence, fetched
// at least 24 h ago, is reaped as legacy junk although a client asked for
// it just now — whatever its adoption record says: none, anchor_absent, a
// failed adoption's (untrusted_signer) or unsigned_disabled. Kept: a
// transient soft-404 younger than the grace; a plain-text Release cached
// as InRelease (a §6.1-quarantined plain-text suite, adopted unsigned
// through Release), with no record at all and its hash block past the
// first 512 bytes (the Release sniff reads 64 KiB); a suite with any
// anchor that passes its sniff; one with no anchor row; one whose pool
// file is missing (no evidence); an adopted suite. Each junk reap counts
// in acu_suites_reaped_total{reason="junk"}.
func TestReaper_ReapsLegacyJunkKeepsTransientSoft404(t *testing.T) {
	r := newReaperRig(t)
	old, young := reapNow-2*reapDay, reapNow-3600
	signed := string(simtest.ClearsignedFixture("Origin: x\nSHA256:\n"))
	for _, s := range []string{"/r/dists/junk", "/r/dists/absent", "/r/dists/detached", "/r/dists/failing",
		"/r/dists/disabled", "/r/dists/soft404", "/r/dists/plain", "/r/dists/good", "/r/dists/mixed",
		"/r/dists/norows", "/r/dists/missing", "/r/dists/adopted"} {
		r.junkSuite(s)
	}
	r.cacheAnchor("/r/dists/junk", "InRelease", softPage, old)
	r.cacheAnchor("/r/dists/absent", "InRelease", softPage, old)
	r.record("/r/dists/absent", cache.ReasonAnchorAbsent)
	r.cacheAnchor("/r/dists/detached", "Release", softPage, old)
	r.cacheAnchor("/r/dists/detached", "Release.gpg", softPage+"sig", old)
	r.cacheAnchor("/r/dists/failing", "InRelease", softPage, old)
	r.record("/r/dists/failing", "untrusted_signer") // upstream signs now; its keys are not installed
	r.cacheAnchor("/r/dists/disabled", "InRelease", softPage, old)
	r.record("/r/dists/disabled", cache.ReasonUnsignedDisabled)
	r.cacheAnchor("/r/dists/soft404", "InRelease", softPage, young)
	// A long Description puts the hash block past the InRelease sniff's
	// 512 bytes, as in many real Release files.
	r.cacheAnchor("/r/dists/plain", "InRelease", "Origin: x\nDescription: "+strings.Repeat("x", 600)+"\nSHA256:\n", old)
	r.cacheAnchor("/r/dists/good", "InRelease", signed, old)
	r.cacheAnchor("/r/dists/mixed", "InRelease", softPage, old)
	r.cacheAnchor("/r/dists/mixed", "Release", "Origin: x\nSHA256:\n "+testHex64+" 1 main/Packages\n", old)
	r.cacheAnchorBlob("/r/dists/missing", "InRelease", testHex64, old)
	r.cacheAnchor("/r/dists/adopted", "InRelease", softPage, old)
	r.adopt("/r/dists/adopted", "adopted anchor")
	junk0 := reapedTotal(t, "junk")
	r.reaper.runOnce(context.Background())
	for _, s := range []string{"/r/dists/junk", "/r/dists/absent", "/r/dists/detached", "/r/dists/failing", "/r/dists/disabled"} {
		if r.exists(s) {
			t.Errorf("%s was not reaped as junk", s)
		}
	}
	for _, s := range []string{"/r/dists/soft404", "/r/dists/plain", "/r/dists/good", "/r/dists/mixed", "/r/dists/norows",
		"/r/dists/missing", "/r/dists/adopted"} {
		if !r.exists(s) {
			t.Errorf("%s was reaped", s)
		}
	}
	for _, ev := range r.events("suite_reaped") {
		if ev["reason"] != "junk" {
			t.Errorf("suite_reaped %v, want reason junk", ev)
		}
	}
	if d := reapedTotal(t, "junk") - junk0; d != 5 {
		t.Fatalf(`acu_suites_reaped_total{reason="junk"} rose by %v, want 5`, d)
	}
}

// A junk sniff's verdict is kept by blob from one run to the next (the
// pool is content-addressed), so a run reads only the anchors it has not
// sniffed: here the second run reaps on the first run's verdict for an
// InRelease whose pool file has gone since.
func TestReaper_RemembersJunkVerdictsByBlob(t *testing.T) {
	r := newReaperRig(t)
	ctx := context.Background()
	old := reapNow - 2*reapDay
	r.junkSuite("/r/dists/x")
	r.cacheAnchor("/r/dists/x", "InRelease", softPage, old)
	r.cacheAnchor("/r/dists/x", "Release", softPage+"release", reapNow-3600)
	r.reaper.runOnce(ctx)
	if !r.exists("/r/dists/x") {
		t.Fatal("reaped while one anchor was younger than the grace")
	}
	row, err := r.cache.LookupURL(ctx, "http", "h", "/r/dists/x/InRelease")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(r.cache.BlobPath(*row.BlobHash)); err != nil {
		t.Fatal(err)
	}
	r.cacheAnchor("/r/dists/x", "Release", softPage+"release", old)
	r.reaper.runOnce(ctx)
	if r.exists("/r/dists/x") {
		t.Fatal("not reaped: the second run read the InRelease again instead of reusing its verdict")
	}
}

// Ruling R-F3, design note 8: the junk grace is 24 h — an anchor fetched
// 23 h ago may still be a transient soft-404, one fetched 25 h ago is junk
// evidence.
func TestReaper_TheJunkGraceIsADay(t *testing.T) {
	r := newReaperRig(t)
	r.junkSuite("/r/dists/young")
	r.junkSuite("/r/dists/old")
	r.cacheAnchor("/r/dists/young", "InRelease", softPage, reapNow-23*3600)
	r.cacheAnchor("/r/dists/old", "InRelease", softPage, reapNow-25*3600)
	r.reaper.runOnce(context.Background())
	if !r.exists("/r/dists/young") || r.exists("/r/dists/old") {
		t.Fatalf("fetched 23 h ago: exists %v; 25 h ago: exists %v; want kept and reaped",
			r.exists("/r/dists/young"), r.exists("/r/dists/old"))
	}
}

// Ruling PF-4 (design review I-4): a junk suite fails its clients'
// updates, so it is not held back by an idle backlog: with 30 idle suites
// waiting — more than a run reaps — the junk suite, asked for just now,
// goes in the first run.
func TestReaper_JunkIsNotStarvedByAnIdleBacklog(t *testing.T) {
	r := newReaperRig(t)
	for i := range 30 {
		r.seed(fmt.Sprintf("/r/dists/idle%02d", i), reapNow-31*reapDay-int64(i))
	}
	r.junkSuite("/r/dists/junk")
	r.cacheAnchor("/r/dists/junk", "InRelease", softPage, reapNow-2*reapDay)
	r.reaper.runOnce(context.Background())
	if r.exists("/r/dists/junk") {
		t.Fatal("the junk suite waited behind the idle backlog")
	}
	if n := len(r.events("suite_reaped")); n != reapsPerRun {
		t.Fatalf("%d reaped, want %d", n, reapsPerRun)
	}
}

// Design review M-4: pool reads that fail — here a directory where the
// blob should be — keep their suites (an I/O fault is never evidence) and
// are logged once per scan with their count, not once per anchor.
func TestReaper_JunkReadFailuresLogOncePerScan(t *testing.T) {
	r := newReaperRig(t)
	for _, s := range []string{"/r/dists/a", "/r/dists/b"} {
		h := strings.Repeat(s[len(s)-1:], 64)
		r.junkSuite(s)
		r.cacheAnchorBlob(s, "InRelease", h, reapNow-2*reapDay)
		if err := os.MkdirAll(r.cache.BlobPath(h), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r.reaper.runOnce(context.Background())
	if !r.exists("/r/dists/a") || !r.exists("/r/dists/b") {
		t.Fatal("an unreadable anchor was taken for junk evidence")
	}
	if ev := r.events("suite reaper: junk anchor reads failed"); len(ev) != 1 || ev[0]["count"] != float64(2) {
		t.Fatalf("read failure lines = %v, want one with count 2", ev)
	}
}
