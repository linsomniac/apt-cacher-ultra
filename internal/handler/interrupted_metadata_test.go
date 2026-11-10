package handler

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// Issue #29's signature-error investigation found an interrupted same-size
// overwrite in apt-cacher-ng. Exercise ACU's real HTTP fetch, blob storage and
// restart path: exact bytes must survive; no new prefix + old tail may escape.
// The clearsign fixture supplies the envelope; this tests byte preservation,
// not the signature verifier (which has its own cryptographic test corpus).
func TestServeHTTP_InterruptedSameSizeMetadataReplacement(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "retry", true: "restart_then_retry"}[restart], func(t *testing.T) {
			ctx := context.Background()
			old := simtest.ClearsignedFixture("Origin: old\nDescription: " + strings.Repeat("a", 32768) + "\n")
			replacement := simtest.ClearsignedFixture("Origin: new\nDescription: " + strings.Repeat("b", 32768) + "\n")
			if len(old) != len(replacement) {
				t.Fatal("fixture requires equal-length versions")
			}
			var phase, partials, resumed atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch phase.Load() {
				case 0:
					w.Header().Set("Last-Modified", "Mon, 01 Jan 2024 00:00:00 GMT")
					w.Header().Set("ETag", `"old"`)
					w.Header().Set("Content-Length", strconv.Itoa(len(old)))
					_, _ = w.Write(old)
				case 1:
					if r.Header.Get("Range") != "" {
						resumed.Add(1)
						if r.Header.Get("If-Range") != `"new"` {
							t.Errorf("resume validator = %q, want new ETag", r.Header.Get("If-Range"))
						}
						http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
						return
					}
					partials.Add(1)
					w.Header().Set("Last-Modified", "Tue, 02 Jan 2024 00:00:00 GMT")
					w.Header().Set("ETag", `"new"`)
					w.Header().Set("Content-Length", strconv.Itoa(len(replacement)))
					_, _ = w.Write(replacement[:16384])
					// Returning with a short Content-Length closes the connection.
				case 2:
					if r.Header.Get("Range") != "" || r.Header.Get("If-Range") != "" {
						t.Errorf("fresh request reused failed download state: %v", r.Header)
					}
					w.Header().Set("Last-Modified", "Tue, 02 Jan 2024 00:00:00 GMT")
					w.Header().Set("ETag", `"new"`)
					w.Header().Set("Content-Length", strconv.Itoa(len(replacement)))
					_, _ = w.Write(replacement)
				}
			}))
			defer srv.Close()
			h := newTestHandler(t, nil, nil)
			t.Cleanup(h.Close)
			path := "/debian/dists/stable/InRelease"
			get := func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, proxyReq("GET", srv.URL, path))
				return w
			}
			if w := get(); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), old) {
				t.Fatalf("initial request = %d, body length %d", w.Code, w.Body.Len())
			}
			scheme, host, port := splitURL(t, srv.URL)
			key := cache.SuiteKey{CanonicalScheme: scheme, CanonicalHost: hostKey(host, port), SuitePath: "/debian/dists/stable"}
			row, err := h.cache.LookupURL(ctx, scheme, key.CanonicalHost, path)
			if err != nil || row.BlobHash == nil {
				t.Fatalf("initial cache row: %v, %v", row, err)
			}
			oldHash := *row.BlobHash
			// This is the operation freshness performs when it detects a changed
			// legacy anchor. It drops URL bindings, retaining immutable blobs.
			if err := h.cache.InvalidateLegacyMetadata(ctx, key, path, oldHash); err != nil {
				t.Fatal(err)
			}
			phase.Store(1)
			if w := get(); w.Code != 502 {
				t.Fatalf("interrupted replacement = %d, want 502", w.Code)
			}
			if partials.Load() == 0 || resumed.Load() == 0 {
				t.Fatal("did not exercise both interrupted transfer and failed Range retry")
			}
			checkUnpublished := func() {
				t.Helper()
				if _, err := h.cache.LookupURL(ctx, scheme, key.CanonicalHost, path); !errors.Is(err, cache.ErrNotFound) {
					t.Fatalf("failed replacement published a URL row: %v", err)
				}
				if b, err := os.ReadFile(h.cache.BlobPath(oldHash)); err != nil || !bytes.Equal(b, old) {
					t.Fatalf("previous immutable blob was changed: %v", err)
				}
			}
			checkUnpublished()
			if restart {
				dir := h.cache.Dir()
				h.Close()
				if err := h.cache.Close(); err != nil {
					t.Fatal(err)
				}
				c, err := cache.Open(ctx, dir, silentLogger())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = c.Close() })
				h, err = New(Config{Parser: h.parser, Cache: c, Fetch: h.fetch, HostLimiter: h.sem, Logger: silentLogger()})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(h.Close)
				checkUnpublished()
			}
			phase.Store(2)
			for i := 0; i < 2; i++ { // successful miss, then cached hit
				w := get()
				if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), replacement) {
					t.Fatalf("recovered request %d = %d; did not serve exact replacement bytes", i, w.Code)
				}
				if w.Header().Get("Last-Modified") != "Tue, 02 Jan 2024 00:00:00 GMT" {
					t.Fatalf("recovered request has stale validators: %v", w.Header())
				}
			}
		})
	}
}
