package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

func TestLegacyRefresh_ClientGetsChangedAnchorAndIndexes(t *testing.T) {
	for _, anchor := range []string{"InRelease", "Release"} {
		t.Run(anchor, func(t *testing.T) {
			ctx := context.Background()
			path := "/r/dists/stable/" + anchor
			body := func(version string) []byte {
				b := []byte("Origin: example\nDate: Mon, 01 Jan 2024 00:00:00 UTC\nDescription: " + version + "\nSHA256:\n")
				if anchor == "InRelease" {
					return simtest.ClearsignedFixture(string(b))
				}
				return b
			}
			f := &datedFetcher{body: body("old expired release"), date: originDate}
			h := newSeamHandler(t, f)
			defer h.Close()
			first := getHeaders(h, "GET", path, nil)
			if first.Code != 200 {
				t.Fatal(first.Code, first.Body.String())
			}
			key := cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "127.0.0.1", SuitePath: "/r/dists/stable"}
			if _, err := h.cache.GetSuiteFreshness(ctx, key.CanonicalScheme, key.CanonicalHost, key.SuitePath); err != nil {
				t.Fatalf("anchor was not tracked: %v", err)
			}
			f.body = []byte("Package: old\n")
			indexPath := key.SuitePath + "/main/binary-amd64/Packages"
			if w := getHeaders(h, "GET", indexPath, nil); w.Code != 200 {
				t.Fatal(w.Code)
			}
			checker, err := freshness.New(freshness.Config{Cache: h.cache, Fetcher: &simtest.StaticConditional{Status: 200, Body: body("new release")}, HostLimiter: hostsem.New(2), Logger: silentLogger()})
			if err != nil {
				t.Fatal(err)
			}
			checker.Check(ctx, key.CanonicalScheme, key.CanonicalHost, key.SuitePath)
			for _, p := range []string{path, indexPath} {
				if _, err := h.cache.LookupURL(ctx, "http", "127.0.0.1", p); !errors.Is(err, cache.ErrNotFound) {
					t.Fatalf("%s not invalidated: %v", p, err)
				}
			}
			f.body = body("new release")
			f.date = "Tue, 02 Jan 2024 00:00:00 GMT"
			w := getHeaders(h, "GET", path, map[string]string{"If-Modified-Since": time.Now().Add(time.Hour).UTC().Format(http.TimeFormat), "If-None-Match": first.Header().Get("ETag")})
			if w.Code != 200 || !strings.Contains(w.Body.String(), "new release") || w.Header().Get("Last-Modified") != f.date {
				t.Fatal(w.Code, w.Header(), w.Body.String())
			}
			f.body = []byte("Package: new\n")
			if w := getHeaders(h, "GET", indexPath, nil); w.Code != 200 || w.Body.String() != "Package: new\n" {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
}

func TestLegacyRefresh_InFlightFetchCannotResurrectOldIndex(t *testing.T) {
	ctx := context.Background()
	f := &datedFetcher{body: simtest.ClearsignedFixture("Origin: example\n"), date: originDate}
	h := newSeamHandler(t, f)
	defer h.Close()
	key := cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "127.0.0.1", SuitePath: "/r/dists/stable"}
	anchor := key.SuitePath + "/InRelease"
	if w := getHeaders(h, "GET", anchor, nil); w.Code != 200 {
		t.Fatal(w.Code)
	}
	row, err := h.cache.LookupURL(ctx, "http", "127.0.0.1", anchor)
	if err != nil {
		t.Fatal(err)
	}
	f.body = []byte("Package: old\n")
	f.before = func() {
		if err := h.cache.InvalidateLegacyMetadata(ctx, key, anchor, *row.BlobHash); err != nil {
			t.Fatal(err)
		}
	}
	path := key.SuitePath + "/main/binary-amd64/Packages"
	w := getHeaders(h, "GET", path, nil)
	if w.Code != 503 || w.Header().Get("Retry-After") == "" || strings.Contains(w.Body.String(), "Package: old") {
		t.Fatal(w.Code, w.Header(), w.Body.String())
	}
	if _, err := h.cache.LookupURL(ctx, "http", "127.0.0.1", path); !errors.Is(err, cache.ErrNotFound) {
		t.Fatal("old index published", err)
	}
	f.before = nil
	f.body = []byte("Package: new\n")
	if w := getHeaders(h, "GET", path, nil); w.Code != 200 || w.Body.String() != "Package: new\n" {
		t.Fatal(w.Code, w.Body.String())
	}
}

type failedConditional struct{}

func (failedConditional) Conditional(context.Context, *fetch.Target, string, string, int64) (*fetch.ConditionalResult, error) {
	return nil, errors.New("upstream unavailable")
}

func TestLegacyRefresh_UpstreamFailureRetainsCachedBytes(t *testing.T) {
	h := newSeamHandler(t, &datedFetcher{body: simtest.ClearsignedFixture("Origin: example\n"), date: originDate})
	defer h.Close()
	path := "/r/dists/stable/InRelease"
	first := getHeaders(h, "GET", path, nil)
	c, err := freshness.New(freshness.Config{Cache: h.cache, Fetcher: failedConditional{}, HostLimiter: hostsem.New(1), Logger: silentLogger()})
	if err != nil {
		t.Fatal(err)
	}
	c.Check(context.Background(), "http", "127.0.0.1", "/r/dists/stable")
	w := getHeaders(h, "GET", path, nil)
	if w.Code != 200 || w.Body.String() != first.Body.String() {
		t.Fatal(w.Code, w.Body.String())
	}
	s, err := h.cache.GetSuiteFreshness(context.Background(), "http", "127.0.0.1", "/r/dists/stable")
	if err != nil || s.CheckFailCount != 1 {
		t.Fatal(s, err)
	}
}

type failingInvalidation struct{ *cache.Cache }

func (failingInvalidation) InvalidateLegacyMetadata(context.Context, cache.SuiteKey, string, string) error {
	return errors.New("invalidation disk fault")
}

func TestLegacyRefresh_PublicationFailureKeepsFailureStreak(t *testing.T) {
	h := newSeamHandler(t, &datedFetcher{body: simtest.ClearsignedFixture("Origin: old\n"), date: originDate})
	defer h.Close()
	path := "/r/dists/stable/InRelease"
	first := getHeaders(h, "GET", path, nil)
	c, err := freshness.New(freshness.Config{Cache: failingInvalidation{h.cache}, Fetcher: &simtest.StaticConditional{Status: 200, Body: simtest.ClearsignedFixture("Origin: new\n")}, HostLimiter: hostsem.New(1), Logger: silentLogger()})
	if err != nil {
		t.Fatal(err)
	}
	for i := int64(1); i <= 2; i++ {
		c.Check(context.Background(), "http", "127.0.0.1", "/r/dists/stable")
		s, err := h.cache.GetSuiteFreshness(context.Background(), "http", "127.0.0.1", "/r/dists/stable")
		if err != nil || s.CheckFailCount != i {
			t.Fatal(s, err)
		}
	}
	w := getHeaders(h, "GET", path, nil)
	if w.Code != 200 || w.Body.String() != first.Body.String() {
		t.Fatal(w.Code, w.Body.String())
	}
}

type legacyConditionalFunc func(context.Context, *fetch.Target, string, string, int64) (*fetch.ConditionalResult, error)

func (f legacyConditionalFunc) Conditional(ctx context.Context, tg *fetch.Target, etag, lastmod string, n int64) (*fetch.ConditionalResult, error) {
	return f(ctx, tg, etag, lastmod, n)
}

func TestLegacyRefresh_SiblingReleaseCannotSupplyInReleaseValidator(t *testing.T) {
	f := &datedFetcher{body: simtest.ClearsignedFixture("Origin: old\n"), date: originDate}
	h := newSeamHandler(t, f)
	defer h.Close()
	path := "/r/dists/stable/InRelease"
	if w := getHeaders(h, "GET", path, nil); w.Code != 200 {
		t.Fatal(w.Code)
	}
	f.body = []byte("Origin: sibling\nSHA256:\n")
	f.date = "Wed, 03 Jan 2024 00:00:00 GMT"
	if w := getHeaders(h, "GET", "/r/dists/stable/Release", nil); w.Code != 200 {
		t.Fatal(w.Code)
	}
	up := legacyConditionalFunc(func(_ context.Context, _ *fetch.Target, _ string, lastmod string, _ int64) (*fetch.ConditionalResult, error) {
		if lastmod != originDate {
			t.Errorf("InRelease got sibling validator %q", lastmod)
			return &fetch.ConditionalResult{Status: 304}, nil
		}
		return &fetch.ConditionalResult{Status: 200, Body: simtest.ClearsignedFixture("Origin: new\n")}, nil
	})
	c, err := freshness.New(freshness.Config{Cache: h.cache, Fetcher: up, HostLimiter: hostsem.New(1), Logger: silentLogger()})
	if err != nil {
		t.Fatal(err)
	}
	c.Check(context.Background(), "http", "127.0.0.1", "/r/dists/stable")
	if _, err := h.cache.LookupURL(context.Background(), "http", "127.0.0.1", path); !errors.Is(err, cache.ErrNotFound) {
		t.Fatal("stale anchor retained", err)
	}
}

func TestLegacyRefresh_UnchangedBodyRotatesItsOwnValidators(t *testing.T) {
	body := simtest.ClearsignedFixture("Origin: same\n")
	h := newSeamHandler(t, &datedFetcher{body: body, date: originDate})
	defer h.Close()
	path := "/r/dists/stable/InRelease"
	if w := getHeaders(h, "GET", path, nil); w.Code != 200 {
		t.Fatal(w.Code)
	}
	const newDate = "Tue, 02 Jan 2024 00:00:00 GMT"
	calls := 0
	up := legacyConditionalFunc(func(_ context.Context, _ *fetch.Target, etag, lastmod string, _ int64) (*fetch.ConditionalResult, error) {
		calls++
		if calls == 1 {
			return &fetch.ConditionalResult{Status: 200, Body: body, ETag: `"new"`, LastModified: newDate}, nil
		}
		if etag != `"new"` || lastmod != newDate {
			t.Errorf("validators not rotated: %q %q", etag, lastmod)
		}
		return &fetch.ConditionalResult{Status: 304}, nil
	})
	c, err := freshness.New(freshness.Config{Cache: h.cache, Fetcher: up, HostLimiter: hostsem.New(1), Logger: silentLogger()})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		c.Check(context.Background(), "http", "127.0.0.1", "/r/dists/stable")
	}
	if calls != 2 {
		t.Fatal(calls)
	}
	if w := getHeaders(h, "GET", path, nil); w.Header().Get("Last-Modified") != newDate {
		t.Fatal(w.Header())
	}
}

func TestLegacyRefresh_InFlightByHashSurvivesInvalidation(t *testing.T) {
	ctx := context.Background()
	f := &datedFetcher{body: simtest.ClearsignedFixture("Origin: old\n"), date: originDate}
	h := newSeamHandler(t, f)
	defer h.Close()
	key := cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "127.0.0.1", SuitePath: "/r/dists/stable"}
	anchor := key.SuitePath + "/InRelease"
	if w := getHeaders(h, "GET", anchor, nil); w.Code != 200 {
		t.Fatal(w.Code)
	}
	row, err := h.cache.LookupURL(ctx, "http", "127.0.0.1", anchor)
	if err != nil {
		t.Fatal(err)
	}
	f.body = []byte("Package: immutable\n")
	path := key.SuitePath + "/main/binary-amd64/by-hash/SHA256/" + sha256Hex(f.body)
	f.before = func() {
		if err := h.cache.InvalidateLegacyMetadata(ctx, key, anchor, *row.BlobHash); err != nil {
			t.Fatal(err)
		}
	}
	if w := getHeaders(h, "GET", path, nil); w.Code != 200 || w.Body.String() != string(f.body) {
		t.Fatal(w.Code, w.Body.String())
	}
}
