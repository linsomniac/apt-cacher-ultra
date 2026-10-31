package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// cacheURLPath stores body as host 127.0.0.1's url_path row at path,
// last fetched at fetched, its pool file's mtime set to mtime.
func cacheURLPath(t *testing.T, h *Handler, path string, body []byte, fetched int64, mtime time.Time, isMetadata bool) {
	t.Helper()
	hash := writeBlob(t, h, body)
	if err := os.Chtimes(h.cache.BlobPath(hash), mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if err := h.cache.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
		Path: path, BlobHash: &hash, UpstreamURL: "http://127.0.0.1" + path, IsMetadata: isMetadata,
		RequestCount: 1, LastRequestedAt: &fetched, LastFetchedAt: &fetched}); err != nil {
		t.Fatal(err)
	}
}

// Spec C4 (plan design note 14): url_path-served metadata answers
// Last-Modified = max(pool mtime, last_fetched_at) — at a hit, a miss and
// a stale serve — so apt's If-Modified-Since sees a replacement whose
// bytes an earlier fetch already pooled; a .deb keeps its pool file's
// mtime.
func TestServeHTTP_URLPathMetadataLastModifiedIsTheLaterStamp(t *testing.T) {
	mtime := time.Unix(wallclock.Unix()-60, 0)
	ahead, behind := wallclock.Unix()+3600, wallclock.Unix()-3600

	t.Run("hits", func(t *testing.T) {
		h := newTestHandler(t, nil, nil)
		cacheURLPath(t, h, "/flat/Packages.gz", flatPackagesGz, ahead, mtime, true)
		cacheURLPath(t, h, "/flat/Sources", []byte("Package: s\n"), behind, mtime, true)
		cacheURLPath(t, h, "/flat/x_1_amd64.deb", []byte("deb bytes"), ahead, mtime, false)
		for path, want := range map[string]int64{"/flat/Packages.gz": ahead, "/flat/Sources": mtime.Unix(), "/flat/x_1_amd64.deb": mtime.Unix()} {
			if code, lm := lastModifiedGet(t, h, path, ""); code != http.StatusOK || lm != httpTime(want) {
				t.Errorf("%s: %d, Last-Modified %q; want 200 and %q", path, code, lm, httpTime(want))
			}
		}
	})

	t.Run("misses", func(t *testing.T) {
		restore := wallclock.SetOffsetForTest(time.Hour) // the row's stamp leads the pool file's mtime
		defer restore()
		h := newTestHandler(t, nil, nil)
		h.fetch = staticUpstream{"http://127.0.0.1/flat/Packages": []byte("Package: a\n"),
			"http://127.0.0.1/flat/y_1_amd64.deb": []byte("deb bytes")}
		for path, metadata := range map[string]bool{"/flat/Packages": true, "/flat/y_1_amd64.deb": false} {
			code, lm := lastModifiedGet(t, h, path, "")
			row, err := h.cache.LookupURL(context.Background(), "http", "127.0.0.1", path)
			if err != nil || code != http.StatusOK {
				t.Fatalf("%s: %d, row %v (%v)", path, code, row, err)
			}
			st, err := os.Stat(h.cache.BlobPath(*row.BlobHash))
			if err != nil {
				t.Fatal(err)
			}
			want := st.ModTime().Unix()
			if metadata {
				want = *row.LastFetchedAt
			}
			if lm != httpTime(want) || *row.LastFetchedAt <= st.ModTime().Unix() {
				t.Errorf("%s: Last-Modified %q; want %q (row stamp %d, pool mtime %d)", path, lm, httpTime(want),
					*row.LastFetchedAt, st.ModTime().Unix())
			}
		}
	})

	t.Run("a stale serve", func(t *testing.T) {
		h := newTestHandlerWithServe(t, config.ServeConfig{ServeStaleWhenUpstreamDown: true}, nil)
		cacheURLPath(t, h, "/flat/Packages.xz", []byte("\xfd7zXZ\x00stale"), ahead, mtime, true)
		req, err := h.parser.Parse("http://127.0.0.1/flat/Packages.xz", "127.0.0.1")
		if err != nil || !req.IsMetadata {
			t.Fatalf("Parse: %+v, %v", req, err)
		}
		rec := httptest.NewRecorder()
		if !h.tryServeStale(rec, newProxyReqHostPort("GET", "http", "127.0.0.1", "/flat/Packages.xz"), req, 0, time.Now()) {
			t.Fatal("tryServeStale = false, want a stale serve")
		}
		if lm := rec.Header().Get("Last-Modified"); lm != httpTime(ahead) {
			t.Fatalf("stale Last-Modified %q, want %q", lm, httpTime(ahead))
		}
	})
}

// Ruling R-G3: a miss reads its row's last_fetched_at after the pool file
// is final, so no url_path anchor's pool file is newer than its row's
// stamp — the stamp adoptionStampTx's first-adoption floor passes — even
// when the body arrives over a second after the request. The served
// Last-Modified is the row's stamp, and the first adoption's anchor is
// stamped after it.
func TestServeHTTP_AMissStampsItsRowAfterItsPoolFile(t *testing.T) {
	h := newTestHandler(t, nil, nil)
	body := []byte("-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\nOrigin: slow\n-----BEGIN PGP SIGNATURE-----\n\nx\n-----END PGP SIGNATURE-----\n")
	var fetches atomic.Int32
	h.fetch = slowUpstream{staticUpstream: staticUpstream{"http://127.0.0.1" + lmSuite + "/InRelease": body},
		delay: 1100 * time.Millisecond, fetches: &fetches}
	code, lm0 := lastModifiedGet(t, h, lmSuite+"/InRelease", "")
	row, err := h.cache.LookupURL(context.Background(), "http", "127.0.0.1", lmSuite+"/InRelease")
	if err != nil || code != http.StatusOK {
		t.Fatalf("miss: %d, row %v (%v)", code, row, err)
	}
	st, err := os.Stat(h.cache.BlobPath(*row.BlobHash))
	if err != nil {
		t.Fatal(err)
	}
	if *row.LastFetchedAt < st.ModTime().Unix() || lm0 != httpTime(*row.LastFetchedAt) {
		t.Fatalf("row stamp %d, pool mtime %d, Last-Modified %q; want the stamp at or after the pool file, served",
			*row.LastFetchedAt, st.ModTime().Unix(), lm0)
	}
	id := commitAnchor(t, h, "anchor adopted first")
	if code, lm1 := lastModifiedGet(t, h, lmSuite+"/InRelease", lm0); code != http.StatusOK || lm1 != httpTime(adoptedAt(t, h, id)) {
		t.Fatalf("after the first adoption, If-Modified-Since %q got %d (Last-Modified %q); want 200", lm0, code, lm1)
	}
}

// Design review M-9 (plan design notes 13 and 14, end to end): an
// anchorless replacement made within the second of its row's last fetch,
// onto a pool blob an earlier fetch left older still, moves Last-Modified
// past what a client already holds — last_fetched_at becomes the previous
// stamp + 1, which beats the older pool mtime — so that client's
// If-Modified-Since is answered 200 with the new stamp, and its next one,
// carrying the new stamp, 304.
func TestServeHTTP_AnAnchorlessReplacementPassesIfModifiedSince(t *testing.T) {
	h := newTestHandler(t, nil, nil)
	ctx := context.Background()
	fetched := wallclock.Unix() + 5 // the replacement comes within its second: now < fetched + 1
	cacheURLPath(t, h, "/flat/Packages.gz", flatPackagesGz, fetched, time.Unix(fetched-60, 0), true)
	if code, lm := lastModifiedGet(t, h, "/flat/Packages.gz", ""); code != http.StatusOK || lm != httpTime(fetched) {
		t.Fatalf("before the replacement: %d, Last-Modified %q; want 200 and %q", code, lm, httpTime(fetched))
	}
	row, err := h.cache.LookupURL(ctx, "http", "127.0.0.1", "/flat/Packages.gz")
	if err != nil {
		t.Fatal(err)
	}
	newer := writeBlob(t, h, append(append([]byte{}, flatPackagesGz...), "newer"...))
	older := time.Unix(fetched-3600, 0)
	if err := os.Chtimes(h.cache.BlobPath(newer), older, older); err != nil {
		t.Fatal(err)
	}
	if ok, err := h.cache.RefreshURLPathBlob(ctx, "http", "127.0.0.1", "/flat/Packages.gz", *row.BlobHash, newer, nil, nil); err != nil || !ok {
		t.Fatalf("RefreshURLPathBlob = %v, %v; want true", ok, err)
	}
	if code, lm := lastModifiedGet(t, h, "/flat/Packages.gz", httpTime(fetched)); code != http.StatusOK || lm != httpTime(fetched+1) {
		t.Fatalf("If-Modified-Since %q after the replacement: %d, Last-Modified %q; want 200 and %q",
			httpTime(fetched), code, lm, httpTime(fetched+1))
	}
	if code, _ := lastModifiedGet(t, h, "/flat/Packages.gz", httpTime(fetched+1)); code != http.StatusNotModified {
		t.Fatalf("If-Modified-Since %q: %d, want 304", httpTime(fetched+1), code)
	}
}
