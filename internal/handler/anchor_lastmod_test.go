package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// tenYears moves the wall clock — and so every adopted_at — far past any
// pool file's real mtime, so the Last-Modified floor shows.
const tenYears = 10 * 365 * 24 * time.Hour

const lmSuite = "/ubuntu/dists/noble"

// lastModifiedGet GETs path from 127.0.0.1 with an optional
// If-Modified-Since and returns the status and Last-Modified.
func lastModifiedGet(t *testing.T, h *Handler, path, ims string) (int, string) {
	t.Helper()
	r := newProxyReqHostPort("GET", "http", "127.0.0.1", path)
	if ims != "" {
		r.Header.Set("If-Modified-Since", ims)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code, rec.Header().Get("Last-Modified")
}

func httpTime(unix int64) string { return time.Unix(unix, 0).UTC().Format(http.TimeFormat) }

// commitAnchor adopts an inline snapshot of lmSuite whose InRelease is body.
func commitAnchor(t *testing.T, h *Handler, body string) int64 {
	t.Helper()
	hash := writeBlob(t, h, []byte(body))
	return commitInlineSnapshot(t, h, "http", "127.0.0.1", lmSuite, hash,
		[]cache.SnapshotMember{{Path: "InRelease", BlobHash: hash, DeclaredSHA256: hash}}, nil)
}

func adoptedAt(t *testing.T, h *Handler, id int64) int64 {
	t.Helper()
	snap, err := h.cache.GetSuiteSnapshot(context.Background(), id)
	if err != nil || snap.AdoptedAt == nil {
		t.Fatalf("snapshot %d: %v (err %v)", id, snap, err)
	}
	return *snap.AdoptedAt
}

// Spec C8: a snapshot anchor carries max(pool mtime, adopted_at), and
// adopted_at grows by at least 1 s per flip — so after two flips in one
// second the previous anchor's If-Modified-Since still gets a 200, while
// the current anchor's gets a 304.
func TestServeHTTP_SnapshotAnchorLastModifiedIsMonotonic(t *testing.T) {
	restore := wallclock.SetOffsetForTest(tenYears)
	defer restore()
	h := newTestHandler(t, nil, nil)
	id1 := commitAnchor(t, h, "anchor one")
	code, lm1 := lastModifiedGet(t, h, lmSuite+"/InRelease", "")
	if code != http.StatusOK || lm1 != httpTime(adoptedAt(t, h, id1)) {
		t.Fatalf("first anchor: %d, Last-Modified %q; want 200 and adopted_at %q", code, lm1, httpTime(adoptedAt(t, h, id1)))
	}
	id2 := commitAnchor(t, h, "anchor two")
	code, lm2 := lastModifiedGet(t, h, lmSuite+"/InRelease", lm1)
	if code != http.StatusOK || lm2 != httpTime(adoptedAt(t, h, id2)) {
		t.Fatalf("after the flip, If-Modified-Since %q got %d (Last-Modified %q); want 200 with %q",
			lm1, code, lm2, httpTime(adoptedAt(t, h, id2)))
	}
	if code, _ := lastModifiedGet(t, h, lmSuite+"/InRelease", lm2); code != http.StatusNotModified {
		t.Fatalf("an unchanged anchor got %d, want 304", code)
	}
}

// The same across a backward clock step: adopted_at = prior + 1.
func TestServeHTTP_SnapshotAnchorLastModifiedSurvivesABackwardClockStep(t *testing.T) {
	restore := wallclock.SetOffsetForTest(tenYears + time.Hour)
	defer restore()
	h := newTestHandler(t, nil, nil)
	id1 := commitAnchor(t, h, "anchor before the step")
	_, lm1 := lastModifiedGet(t, h, lmSuite+"/InRelease", "")
	restoreStep := wallclock.SetOffsetForTest(tenYears) // the clock steps back an hour
	defer restoreStep()
	id2 := commitAnchor(t, h, "anchor after the step")
	if got, want := adoptedAt(t, h, id2), adoptedAt(t, h, id1)+1; got != want {
		t.Fatalf("adopted_at after the step = %d, want %d", got, want)
	}
	if code, lm2 := lastModifiedGet(t, h, lmSuite+"/InRelease", lm1); code != http.StatusOK || lm2 != httpTime(adoptedAt(t, h, id2)) {
		t.Fatalf("If-Modified-Since %q got %d (Last-Modified %q), want 200", lm1, code, lm2)
	}
}

// A first adoption passes the anchor clients were served from the
// url_path cache, even when the clock stepped back since: adopted_at =
// max(now, url_path anchor last_fetched_at + 1) (spec C8).
func TestServeHTTP_FirstAdoptionLastModifiedPassesTheServedAnchor(t *testing.T) {
	restore := wallclock.SetOffsetForTest(tenYears)
	defer restore()
	h := newTestHandler(t, nil, nil)
	ctx := context.Background()
	served := writeBlob(t, h, []byte("anchor served before adoption"))
	fetched := wallclock.Unix() + 3600 // fetched an hour "later": the clock has since stepped back
	mt := time.Unix(fetched, 0)
	if err := os.Chtimes(h.cache.BlobPath(served), mt, mt); err != nil {
		t.Fatal(err)
	}
	if err := h.cache.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: "127.0.0.1", Path: lmSuite + "/InRelease",
		BlobHash: &served, UpstreamURL: "http://127.0.0.1" + lmSuite + "/InRelease", IsMetadata: true, LastFetchedAt: &fetched}); err != nil {
		t.Fatal(err)
	}
	code, lm0 := lastModifiedGet(t, h, lmSuite+"/InRelease", "")
	if code != http.StatusOK || lm0 != httpTime(fetched) {
		t.Fatalf("url_path serve: %d, Last-Modified %q; want 200 and %q", code, lm0, httpTime(fetched))
	}
	commitAnchor(t, h, "anchor adopted first")
	if code, lm1 := lastModifiedGet(t, h, lmSuite+"/InRelease", lm0); code != http.StatusOK || lm1 != httpTime(fetched+1) {
		t.Fatalf("after the first adoption, If-Modified-Since %q got %d (Last-Modified %q); want 200 with %q",
			lm0, code, lm1, httpTime(fetched+1))
	}
}

// Release and Release.gpg of a detached snapshot get the floor too;
// index members keep their pool mtime.
func TestServeHTTP_DetachedAnchorsGetTheFloorIndexesDoNot(t *testing.T) {
	restore := wallclock.SetOffsetForTest(tenYears)
	defer restore()
	h := newTestHandler(t, nil, nil)
	ctx := context.Background()
	rel := writeBlob(t, h, []byte("Release body"))
	sig := writeBlob(t, h, []byte("Release.gpg body"))
	pkgs := writeBlob(t, h, []byte("Packages body"))
	if err := h.cache.PutSuiteFreshness(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "127.0.0.1", SuitePath: lmSuite}); err != nil {
		t.Fatal(err)
	}
	id, _, err := h.cache.InsertCandidateSnapshot(ctx, cache.SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
		SuitePath: lmSuite, ReleaseHash: &rel, ReleaseGPGHash: &sig})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.cache.CommitAdoption(ctx, id, []cache.SnapshotMember{
		{SnapshotID: id, Path: "Release", BlobHash: rel, DeclaredSHA256: rel},
		{SnapshotID: id, Path: "Release.gpg", BlobHash: sig, DeclaredSHA256: sig},
		{SnapshotID: id, Path: "main/binary-amd64/Packages", BlobHash: pkgs, DeclaredSHA256: pkgs},
	}, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"/Release", "/Release.gpg"} {
		if code, lm := lastModifiedGet(t, h, lmSuite+file, ""); code != http.StatusOK || lm != httpTime(adoptedAt(t, h, id)) {
			t.Errorf("%s: %d, Last-Modified %q; want adopted_at %q", file, code, lm, httpTime(adoptedAt(t, h, id)))
		}
	}
	st, err := os.Stat(h.cache.BlobPath(pkgs))
	if err != nil {
		t.Fatal(err)
	}
	if code, lm := lastModifiedGet(t, h, lmSuite+"/main/binary-amd64/Packages", ""); code != http.StatusOK || lm != st.ModTime().UTC().Format(http.TimeFormat) {
		t.Errorf("Packages: %d, Last-Modified %q; want its pool mtime", code, lm)
	}
}

// Design note 7: a snapshot anchor recovered from upstream (its pool file
// was lost) is served with the same floor.
func TestServeHTTP_RecoveredAnchorGetsTheFloor(t *testing.T) {
	restore := wallclock.SetOffsetForTest(tenYears)
	defer restore()
	body := []byte("InRelease recovered from upstream")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	defer srv.Close()
	h := newTestHandler(t, nil, nil)
	scheme, host, port := splitURL(t, srv.URL)
	hash := writeBlob(t, h, body)
	id := commitInlineSnapshot(t, h, scheme, hostKey(host, port), "/dists/noble", hash,
		[]cache.SnapshotMember{{Path: "InRelease", BlobHash: hash, DeclaredSHA256: hash}}, nil)
	if err := os.Remove(h.cache.BlobPath(hash)); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/dists/noble/InRelease"))
	if rec.Code != http.StatusOK || rec.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("recovery: %d, X-Cache %q; want 200 MISS", rec.Code, rec.Header().Get("X-Cache"))
	}
	if lm := rec.Header().Get("Last-Modified"); lm != httpTime(adoptedAt(t, h, id)) {
		t.Fatalf("recovered anchor Last-Modified %q, want adopted_at %q", lm, httpTime(adoptedAt(t, h, id)))
	}
}

// max(pool mtime, adopted_at) (spec C8): a pool file newer than adopted_at
// keeps its own mtime as Last-Modified.
func TestServeHTTP_SnapshotAnchorKeepsANewerPoolMtime(t *testing.T) {
	h := newTestHandler(t, nil, nil)
	hash := writeBlob(t, h, []byte("anchor with a newer pool mtime"))
	id := commitInlineSnapshot(t, h, "http", "127.0.0.1", lmSuite, hash,
		[]cache.SnapshotMember{{Path: "InRelease", BlobHash: hash, DeclaredSHA256: hash}}, nil)
	mt := time.Unix(adoptedAt(t, h, id)+3600, 0)
	if err := os.Chtimes(h.cache.BlobPath(hash), mt, mt); err != nil {
		t.Fatal(err)
	}
	if code, lm := lastModifiedGet(t, h, lmSuite+"/InRelease", ""); code != http.StatusOK || lm != httpTime(mt.Unix()) {
		t.Fatalf("%d, Last-Modified %q; want 200 with the newer pool mtime %q", code, lm, httpTime(mt.Unix()))
	}
}
