package freshness

import (
	"context"
	"fmt"
	"maps"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
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
// file is missing (no evidence, and no read failure either); an adopted
// suite. Each junk reap counts in acu_suites_reaped_total{reason="junk"}.
// The detached suite's Release.gpg is a real-sized HTML page, longer than
// its 512-byte head cap: its head is sniffed all the same (the Task 7
// review's minor 1).
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
	r.cacheAnchor("/r/dists/detached", "Release.gpg", softPage+strings.Repeat("<!-- pad -->", 60), old)
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
	if ev := r.events("suite reaper: junk anchor reads failed"); len(ev) != 0 {
		t.Fatalf("read failure lines = %v, want none: a missing pool file is no read failure (the Task 7 review's minor 2)", ev)
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
// less than 24 h ago may still be a transient soft-404, one fetched 24 h
// ago or earlier is junk evidence. Both sides of the boundary are pinned
// to the second (the Task 7 review's minor 3).
func TestReaper_TheJunkGraceIsADay(t *testing.T) {
	r := newReaperRig(t)
	fetched := map[string]int64{
		"/r/dists/h23":    reapNow - 23*3600,
		"/r/dists/almost": reapNow - 24*3600 + 1,
		"/r/dists/day":    reapNow - 24*3600,
		"/r/dists/h25":    reapNow - 25*3600,
	}
	for s, at := range fetched {
		r.junkSuite(s)
		r.cacheAnchor(s, "InRelease", softPage, at)
	}
	r.reaper.runOnce(context.Background())
	for s, reaped := range map[string]bool{"/r/dists/h23": false, "/r/dists/almost": false, "/r/dists/day": true, "/r/dists/h25": true} {
		if r.exists(s) == reaped {
			t.Errorf("%s, fetched %d s before now: reaped %v, want %v", s, reapNow-fetched[s], !reaped, reaped)
		}
	}
}

// Design note 8: a verdict is kept by (anchor kind, blob) — what a blob's
// head is to one kind of anchor — never by blob alone (the Task 7 review's
// minor 4): an armored signature is a good Release.gpg but junk as an
// InRelease, so the first run's verdict for one suite's Release.gpg does
// not keep another suite whose InRelease is the same blob.
func TestReaper_AJunkVerdictIsKeptByAnchorKindAndBlob(t *testing.T) {
	r := newReaperRig(t)
	sig := "-----BEGIN PGP SIGNATURE-----\n\niQEzBAEBCAAdFiEE\n-----END PGP SIGNATURE-----\n"
	old := reapNow - 2*reapDay
	r.junkSuite("/r/dists/detached")
	r.cacheAnchor("/r/dists/detached", "Release.gpg", sig, old)
	r.reaper.runOnce(context.Background())
	if !r.exists("/r/dists/detached") {
		t.Fatal("a suite whose Release.gpg passes its sniff was reaped")
	}
	r.junkSuite("/r/dists/inline")
	r.cacheAnchor("/r/dists/inline", "InRelease", sig, old)
	r.reaper.runOnce(context.Background())
	if r.exists("/r/dists/inline") || !r.exists("/r/dists/detached") {
		t.Fatalf("inline exists %v, detached exists %v; want the signature cached as InRelease reaped as junk, "+
			"the detached suite kept", r.exists("/r/dists/inline"), r.exists("/r/dists/detached"))
	}
}

// A run tries a suite once (the Task 7 review's minor 4): an idle suite
// whose anchors are also junk is not listed again by the junk scan — it
// is reaped once, as idle — so it takes one of the run's reapsPerRun
// tries, not two.
func TestReaper_TriesASuiteOncePerRun(t *testing.T) {
	r := newReaperRig(t)
	r.seed("/r/dists/both", reapNow-31*reapDay)
	r.cacheAnchor("/r/dists/both", "InRelease", softPage, reapNow-2*reapDay)
	tries := 0
	r.reaper.reapHook = func(context.Context, cache.SuiteKey) { tries++ }
	r.reaper.runOnce(context.Background())
	if r.exists("/r/dists/both") || tries != 1 {
		t.Fatalf("exists %v after %d tries; want it reaped at the first and only try", r.exists("/r/dists/both"), tries)
	}
	if ev := r.events("suite_reaped"); len(ev) != 1 || ev[0]["reason"] != "idle" {
		t.Fatalf("suite_reaped = %v, want one with reason idle", ev)
	}
}

// Only blobs read and sniffed are evidence (the Task 7 review's minor 5,
// controller ruling): an anchor aptpath.ClassifySuiteFile calls
// NotSuiteFile — any anchor of a suite path longer than MaxSuitePathBytes
// — is never junk evidence, whatever it holds, and is not read.
func TestReaper_AnOverLongSuitePathIsNoJunkEvidence(t *testing.T) {
	r := newReaperRig(t)
	reads := 0
	r.reaper.readHead = func(path string, limit int64) ([]byte, error) { reads++; return readCapped(path, limit) }
	signed := "/r/dists/s" + strings.Repeat("x", aptpath.MaxSuitePathBytes)
	html := "/r/dists/h" + strings.Repeat("x", aptpath.MaxSuitePathBytes)
	for s, body := range map[string]string{signed: string(simtest.ClearsignedFixture("Origin: x\nSHA256:\n")), html: softPage} {
		r.junkSuite(s)
		r.cacheAnchor(s, "InRelease", body, reapNow-2*reapDay)
	}
	r.reaper.runOnce(context.Background())
	if !r.exists(signed) || !r.exists(html) {
		t.Fatalf("over-long suites: signed exists %v, html exists %v; want both kept (no evidence)", r.exists(signed), r.exists(html))
	}
	if reads != 0 {
		t.Fatalf("%d pool reads for anchors that are no suite file; want none", reads)
	}
}

// Design note 8 (the Task 7 review's minor 6): a scan reads a blob once,
// however many suites' anchors share it — one host's soft-404 page cached
// for all its suites is the typical legacy case — reusing this run's
// verdict as well as the previous run's.
func TestReaper_AJunkScanReadsASharedBlobOnce(t *testing.T) {
	r := newReaperRig(t)
	reads := 0
	r.reaper.readHead = func(path string, limit int64) ([]byte, error) { reads++; return readCapped(path, limit) }
	for _, s := range []string{"/r/dists/a", "/r/dists/b", "/r/dists/c"} {
		r.junkSuite(s)
		r.cacheAnchor(s, "InRelease", softPage, reapNow-2*reapDay) // one blob: the pool is content-addressed
	}
	r.reaper.runOnce(context.Background())
	if r.exists("/r/dists/a") || r.exists("/r/dists/b") || r.exists("/r/dists/c") {
		t.Fatal("a suite whose shared anchor is junk was kept")
	}
	if reads != 1 {
		t.Fatalf("%d pool reads of one shared blob; want 1", reads)
	}
}

// The Task 7 review's minor 7 (= the Task 11 review's minor 5): the junk
// scan checks the run's context per suite, so a shutdown does not wait
// for the pool reads of every anchor not yet sniffed (about 330 MB for
// 5,000 never-adopted suites after a restart). Cut short, it returns no
// candidates and keeps the previous run's verdicts as they were.
func TestReaper_AJunkScanStopsAtShutdown(t *testing.T) {
	r := newReaperRig(t)
	old := reapNow - 2*reapDay
	r.junkSuite("/r/dists/good")
	r.cacheAnchor("/r/dists/good", "InRelease", string(simtest.ClearsignedFixture("Origin: x\nSHA256:\n")), old)
	r.reaper.runOnce(context.Background()) // remembers the good anchor's verdict
	before := maps.Clone(r.reaper.sniffs)
	if len(before) != 1 {
		t.Fatalf("premise: %d verdicts after the first run, want 1", len(before))
	}
	for i := range 3 {
		s := fmt.Sprintf("/r/dists/j%d", i)
		r.junkSuite(s)
		r.cacheAnchor(s, "InRelease", fmt.Sprintf("%s<!-- %d -->", softPage, i), old)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reads := 0
	r.reaper.readHead = func(path string, limit int64) ([]byte, error) { reads++; cancel(); return readCapped(path, limit) }
	rows, err := r.cache.ListSuitesWithAdoption(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := r.reaper.junkCandidates(ctx, rows, nil, time.Unix(reapNow, 0)); len(got) != 0 || reads != 1 {
		t.Fatalf("a scan whose context ended at its first read: %d candidates after %d reads; want none after 1", len(got), reads)
	}
	if !maps.Equal(r.reaper.sniffs, before) {
		t.Fatalf("verdicts after the cut-short scan = %v, want the previous run's %v", r.reaper.sniffs, before)
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
// are logged once per scan with their count, not once per anchor, naming
// the first failure's suite scheme, host and anchor path (the scheme tells
// an http copy of a suite from its https copy: the Task 6 re-review's
// out-of-scope 2).
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
	ev := r.events("suite reaper: junk anchor reads failed")
	if len(ev) != 1 || ev[0]["count"] != float64(2) {
		t.Fatalf("read failure lines = %v, want one with count 2", ev)
	}
	if ev[0]["canonical_scheme"] != "http" || ev[0]["canonical_host"] != "h" || ev[0]["path"] != "/r/dists/a/InRelease" {
		t.Fatalf("read failure line = %v, want the first failure named: http, h, /r/dists/a/InRelease", ev[0])
	}
}
