package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
)

// recFetcher is an in-memory upstream that records the URLs it is asked
// for and answers every request with body.
type recFetcher struct {
	body []byte
	mu   sync.Mutex
	urls []string
}

func (*recFetcher) HostAllowed(string) bool { return true }

func (r *recFetcher) Fetch(_ context.Context, tg *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error) {
	r.mu.Lock()
	r.urls = append(r.urls, tg.URL)
	r.mu.Unlock()
	if _, err := dst.Write(r.body); err != nil {
		return nil, err
	}
	return &fetch.FetchResult{Status: 200, ContentLength: int64(len(r.body))}, nil
}

func (r *recFetcher) fetched() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.urls...)
}

func newSeamHandler(t *testing.T, f Fetcher) *Handler {
	t.Helper()
	parser, err := proxy.New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := cache.Open(context.Background(), t.TempDir(), silentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	h, err := New(Config{Parser: parser, Cache: c, Fetch: f, Logger: silentLogger(), HostLimiter: hostsem.New(4)})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// The seam exists so virtual-clock simulations can drive the handler:
// the real cache and an in-memory upstream run inside a synctest bubble.
func TestHandler_InMemoryFetcherInsideBubble(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		up := &recFetcher{body: []byte("deb")}
		h := newSeamHandler(t, up)
		for i := 0; i < 3; i++ {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://up.example/pool/x.deb", nil))
			if rec.Code != 200 || rec.Body.String() != "deb" {
				t.Fatalf("request %d: status=%d body=%q", i, rec.Code, rec.Body.String())
			}
			time.Sleep(time.Hour)
		}
		h.Close()
		if got := up.fetched(); len(got) != 1 {
			t.Fatalf("upstream fetches = %v, want 1 (later requests are cache hits)", got)
		}
	})
}

// Review Focus 2 end to end: apt's request line reaches upstream in apt's
// encoding, and a differently-encoded request for the same file hits the
// same cache key.
func TestHandler_EncodingEndToEnd(t *testing.T) {
	up := &recFetcher{body: []byte("deb bytes")}
	h := newSeamHandler(t, up)
	defer h.Close()
	get := func(target string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		return rec.Code
	}
	if code := get("http://up.example/pool/libstdc%2b%2b6_1%253a2~24.04_amd64.deb"); code != 200 {
		t.Fatalf("first GET = %d", code)
	}
	if code := get("http://up.example/pool/libstdc++6_1%253a2%7e24.04_amd64.deb"); code != 200 {
		t.Fatalf("second GET = %d", code)
	}
	urls := up.fetched()
	if len(urls) != 1 {
		t.Fatalf("upstream fetches = %v, want exactly 1 (same cache key)", urls)
	}
	if want := "http://up.example/pool/libstdc%2b%2b6_1%253a2%7e24.04_amd64.deb"; urls[0] != want {
		t.Fatalf("upstream URL = %q, want %q", urls[0], want)
	}
	u, err := url.Parse(urls[0])
	if err != nil || u.Path != "/pool/libstdc++6_1%3a2~24.04_amd64.deb" {
		t.Fatalf("upstream URL decodes to %q (%v)", u.Path, err)
	}
}
