package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/usage"
)

func TestUsageServeRangesHeadErrorsAndPartialWrite(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified", "Mon, 02 Nov 2026 00:00:00 GMT")
		_, _ = io.WriteString(w, "abcdef")
	}))
	defer upstream.Close()
	h := newTestHandler(t, nil, nil)
	defer h.Close()
	u, err := usage.New(t.Context(), h.cache)
	if err != nil {
		t.Fatal(err)
	}
	h.usage = u
	for _, tc := range []struct {
		method, rangeHeader, modified string
		status                        int
		served, hits                  int64
	}{
		{"GET", "", "", 200, 6, 0},
		{"GET", "", "", 200, 12, 1},
		{"GET", "bytes=1-3", "", 206, 15, 2},
		{"HEAD", "", "", 200, 15, 3},
		{"GET", "", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat), 304, 15, 4},
		{"GET", "bytes=100-200", "", 416, 15, 4},
	} {
		r := proxyReq(tc.method, upstream.URL, "/pool/example.deb")
		r.Header.Set("Range", tc.rangeHeader)
		r.Header.Set("If-Modified-Since", tc.modified)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		s := u.Snapshot().Lifetime
		if w.Code != tc.status || s.ServedBytes != tc.served || s.CacheHitRequests != tc.hits {
			t.Fatalf("%+v: status %d, stats %+v", tc, w.Code, s)
		}
	}
	w := &shortUsageWriter{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(w, proxyReq("GET", upstream.URL, "/pool/example.deb"))
	s := u.Snapshot().Lifetime
	if s.ServedBytes != 17 || s.CacheHitBytes != 11 || s.Requests != 7 {
		t.Fatal(s)
	}
	before := s.Totals
	h.ServeHTTP(httptest.NewRecorder(), proxyReq("CONNECT", upstream.URL, "/"))
	h.ServeHTTP(httptest.NewRecorder(), proxyReq("POST", upstream.URL, "/"))
	if got := u.Snapshot().Lifetime.Totals; got != before {
		t.Fatalf("CONNECT/POST counted: %+v", got)
	}
}

type shortUsageWriter struct{ *httptest.ResponseRecorder }

func (w *shortUsageWriter) Write(p []byte) (int, error) {
	n := min(2, len(p))
	_, _ = w.ResponseRecorder.Write(p[:n])
	return n, io.ErrClosedPipe
}
