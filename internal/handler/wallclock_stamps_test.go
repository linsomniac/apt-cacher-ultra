package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// Deferred minor A-3 (handler half): every stamp the handler persists or
// reports comes from wallclock, so a test offset moves it; time.Now would
// not. (The GC's stamps stay listed in the deferred file.)
func TestHandler_StampsFollowTheWallClock(t *testing.T) {
	restore := wallclock.SetOffsetForTest(48 * time.Hour)
	defer restore()
	ctx := context.Background()
	h := newSeamHandler(t, &recFetcher{body: []byte("deb bytes")})
	defer h.Close()
	near := func(what string, got *int64) {
		t.Helper()
		now := wallclock.Unix()
		if got == nil {
			t.Errorf("%s is unset, want the wall clock %d", what, now)
			return
		}
		if *got < now-5 || *got > now+5 {
			t.Errorf("%s = %d, want the wall clock %d (time.Now is %d)", what, *got, now, time.Now().Unix())
		}
	}
	get := func(target string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		return rec
	}
	// A miss stores its url_path row with wall-clock stamps.
	if rec := get("http://up.example/pool/x.deb"); rec.Code != http.StatusOK {
		t.Fatalf("miss: %d", rec.Code)
	}
	row, err := h.cache.LookupURL(ctx, "http", "up.example", "/pool/x.deb")
	if err != nil {
		t.Fatal(err)
	}
	near("miss last_fetched_at", row.LastFetchedAt)
	near("miss last_requested_at", row.LastRequestedAt)
	// A hit reports X-Cache-Age against the wall clock.
	fetched := wallclock.Unix() - 100
	row.LastFetchedAt = &fetched
	if err := h.cache.PutURLPath(ctx, *row); err != nil {
		t.Fatal(err)
	}
	if age, _ := strconv.Atoi(get("http://up.example/pool/x.deb").Header().Get("X-Cache-Age")); age < 100 || age > 105 {
		t.Errorf("hit X-Cache-Age = %d, want about 100", age)
	}
	// A by-hash content-addressed serve stores its row with wall-clock stamps.
	bh, err := h.parser.Parse("http://up.example/u/dists/s/by-hash/SHA256/"+*row.BlobHash, "up.example")
	if err != nil {
		t.Fatal(err)
	}
	h.recordByHashURLPath(bh, *row.BlobHash)
	byHash, err := h.cache.LookupURL(ctx, "http", "up.example", bh.Path)
	if err != nil {
		t.Fatal(err)
	}
	near("by-hash last_fetched_at", byHash.LastFetchedAt)
	near("by-hash last_requested_at", byHash.LastRequestedAt)
	// A stale serve reports X-Cache-Age against the wall clock.
	if rec := get("http://up.example/u/dists/s/InRelease"); rec.Code != http.StatusOK {
		t.Fatalf("metadata miss: %d", rec.Code)
	}
	md, err := h.cache.LookupURL(ctx, "http", "up.example", "/u/dists/s/InRelease")
	if err != nil {
		t.Fatal(err)
	}
	staleAt := wallclock.Unix() - 200
	md.LastFetchedAt = &staleAt
	if err := h.cache.PutURLPath(ctx, *md); err != nil {
		t.Fatal(err)
	}
	h.serve.ServeStaleWhenUpstreamDown = true
	mdReq, err := h.parser.Parse("http://up.example/u/dists/s/InRelease", "up.example")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if !h.tryServeStale(rec, httptest.NewRequest(http.MethodGet, "http://up.example/u/dists/s/InRelease", nil), mdReq, 0, time.Now()) {
		t.Fatal("stale serve refused")
	}
	if age, _ := strconv.Atoi(rec.Header().Get("X-Cache-Age")); age < 200 || age > 205 {
		t.Errorf("stale X-Cache-Age = %d, want about 200", age)
	}
}
