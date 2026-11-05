package handler

import (
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
)

const originDate = "Mon, 01 Jan 2024 00:00:00 GMT"

type datedFetcher struct {
	body   []byte
	date   string
	before func()
}

func (*datedFetcher) HostAllowed(string) bool { return true }
func (f *datedFetcher) Fetch(_ context.Context, _ *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error) {
	if f.before != nil {
		f.before()
	}
	if _, err := dst.Write(f.body); err != nil {
		return nil, err
	}
	return &fetch.FetchResult{Status: 200, ContentLength: int64(len(f.body)), LastModified: f.date}, nil
}

func getHeaders(h *Handler, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	r := newProxyReqHostPort(method, "http", "127.0.0.1", path)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestLastModified_MissHitHeadAndDedup(t *testing.T) {
	f := &datedFetcher{body: []byte("Package: example\n"), date: originDate}
	h := newSeamHandler(t, f)
	defer h.Close()
	for _, path := range []string{"/flat/Packages", "/pool/example.deb"} {
		for _, method := range []string{"GET", "GET", "HEAD"} {
			w := getHeaders(h, method, path, nil)
			if w.Code != 200 || w.Header().Get("Last-Modified") != originDate {
				t.Fatalf("%s %s: %d %v", method, path, w.Code, w.Header())
			}
			if method == "HEAD" && w.Body.Len() != 0 {
				t.Fatal("HEAD has body")
			}
		}
	}
	// Identical bytes at another URL must keep that URL's own date.
	f.date = "Tue, 02 Jan 2024 00:00:00 GMT"
	w := getHeaders(h, "GET", "/other/Packages", nil)
	if w.Header().Get("Last-Modified") != f.date {
		t.Fatal(w.Header())
	}
	row, err := h.cache.LookupURL(context.Background(), "http", "127.0.0.1", "/flat/Packages")
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(24 * time.Hour)
	if err := os.Chtimes(h.cache.BlobPath(*row.BlobHash), future, future); err != nil {
		t.Fatal(err)
	}
	if w := getHeaders(h, "GET", "/flat/Packages", nil); w.Header().Get("Last-Modified") != originDate {
		t.Fatal(w.Header())
	}
}

func TestLastModified_UnknownAndInvalidAreOmitted(t *testing.T) {
	for _, date := range []string{"", "yesterday", "Mon, 01 Jan 2024 00:00:00 GMT\r\nX-Injected: yes", "Thu, 01 Jan 2099 00:00:00 GMT", strings.Repeat("x", 129)} {
		t.Run(date, func(t *testing.T) {
			h := newSeamHandler(t, &datedFetcher{body: []byte("Package: x\n"), date: date})
			defer h.Close()
			for i := 0; i < 2; i++ {
				w := getHeaders(h, "GET", "/flat/Packages", nil)
				if w.Code != 200 || w.Header().Get("Last-Modified") != "" || w.Header().Get("X-Injected") != "" {
					t.Fatal(w.Code, w.Header())
				}
			}
		})
	}
}

func TestValidators_ContentETagAndRanges(t *testing.T) {
	h := newSeamHandler(t, &datedFetcher{body: []byte("Package: example\n"), date: originDate})
	defer h.Close()
	path := "/flat/Packages"
	first := getHeaders(h, "GET", path, nil)
	tag := first.Header().Get("ETag")
	if tag == "" {
		t.Fatal("missing ETag")
	}
	cases := []struct {
		name    string
		headers map[string]string
		status  int
		body    string
	}{
		{"equal date", map[string]string{"If-Modified-Since": originDate}, 200, first.Body.String()},
		{"old synthetic future date", map[string]string{"If-Modified-Since": "Thu, 01 Jan 2099 00:00:00 GMT"}, 200, first.Body.String()},
		{"etag", map[string]string{"If-None-Match": tag}, 304, ""},
		{"weak etag", map[string]string{"If-None-Match": "W/" + tag}, 304, ""},
		{"etag takes precedence", map[string]string{"If-None-Match": `"old"`, "If-Modified-Since": originDate}, 200, first.Body.String()},
		{"if match", map[string]string{"If-Match": `"old"`}, 412, ""},
		{"range", map[string]string{"Range": "bytes=0-6"}, 206, "Package"},
		{"etag range", map[string]string{"Range": "bytes=0-6", "If-Range": tag}, 206, "Package"},
		{"date range", map[string]string{"Range": "bytes=0-6", "If-Range": originDate}, 200, first.Body.String()},
		{"old etag range", map[string]string{"Range": "bytes=0-6", "If-Range": `"old"`}, 200, first.Body.String()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := getHeaders(h, "GET", path, tc.headers)
			if w.Code != tc.status || w.Body.String() != tc.body {
				t.Fatalf("%d %q, want %d %q", w.Code, w.Body.String(), tc.status, tc.body)
			}
		})
	}
	// A different representation with the SAME upstream date must not be hidden.
	row, err := h.cache.LookupURL(context.Background(), "http", "127.0.0.1", path)
	if err != nil {
		t.Fatal(err)
	}
	hash := writeBlob(t, h, []byte("Package: changed\n"))
	date := originDate
	if ok, err := h.cache.RefreshURLPathBlob(context.Background(), "http", "127.0.0.1", path, *row.BlobHash, hash, nil, &date); err != nil || !ok {
		t.Fatal(ok, err)
	}
	w := getHeaders(h, "GET", path, map[string]string{"If-None-Match": tag, "If-Modified-Since": originDate})
	if w.Code != 200 || w.Header().Get("ETag") == tag || w.Header().Get("Last-Modified") != originDate {
		t.Fatal(w.Code, w.Header())
	}
}

func TestLastModified_StaleServePreservesDate(t *testing.T) {
	h := newTestHandlerWithServe(t, config.ServeConfig{ServeStaleWhenUpstreamDown: true}, nil)
	path := "/flat/Packages"
	hash := writeBlob(t, h, []byte("Package: x\n"))
	date := originDate
	if err := h.cache.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: "http", CanonicalHost: "127.0.0.1", Path: path, BlobHash: &hash, UpstreamURL: "http://127.0.0.1" + path, IsMetadata: true, UpstreamLastMod: &date}); err != nil {
		t.Fatal(err)
	}
	req, err := h.parser.Parse("http://127.0.0.1"+path, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	if !h.tryServeStale(w, newProxyReqHostPort("GET", "http", "127.0.0.1", path), req, 503, time.Now()) || w.Header().Get("Last-Modified") != originDate {
		t.Fatal(w.Code, w.Header())
	}
}
