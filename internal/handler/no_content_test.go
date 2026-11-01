package handler

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
)

// The generic metadata miss path has no declared hash. A 204 on a retry
// must clear the actual BlobWriter and its hash state before Finalize(0),
// otherwise the partial body can become a cached success.
func TestServeHTTP_NoContentMissThenHit(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial=%t", partial), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 && partial {
					conn, out, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					defer func() { _ = conn.Close() }()
					_, _ = fmt.Fprint(out, "HTTP/1.1 200 OK\r\nContent-Length: 11\r\nETag: \"v1\"\r\n\r\nhello ")
					_ = out.Flush()
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()
			h := newTestHandler(t, nil, nil)
			for _, cacheStatus := range []string{"MISS", "HIT"} {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, proxyReq(http.MethodGet, srv.URL, "/debian/dists/stable/main/binary-all/Packages"))
				if rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") != "0" {
					t.Fatalf("%s: status=%d, body=%q, Content-Length=%q", cacheStatus, rec.Code, rec.Body.String(), rec.Header().Get("Content-Length"))
				}
				if got := rec.Header().Get("X-Cache"); got != cacheStatus {
					t.Errorf("X-Cache=%q, want %q", got, cacheStatus)
				}
			}
			// Check the pool key as well as the served bytes: truncation
			// must reset the hasher, not only the file and byte counter.
			emptyHash := fmt.Sprintf("%x", sha256.Sum256(nil))
			if body, err := os.ReadFile(h.cache.BlobPath(emptyHash)); err != nil || len(body) != 0 {
				t.Errorf("empty blob: bytes=%d, error=%v", len(body), err)
			}
			wantCalls := int32(1)
			if partial {
				wantCalls++
			}
			if got := calls.Load(); got != wantCalls {
				t.Errorf("upstream calls=%d, want %d", got, wantCalls)
			}
		})
	}
}
