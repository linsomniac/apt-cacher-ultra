package fetch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/usage"
)

func TestUsageFetchRestartAndConditional(t *testing.T) {
	c, err := cache.Open(t.Context(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	u, err := usage.New(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var resumes atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/retry":
			if calls.Add(1) == 1 {
				w.Header().Set("Content-Length", "6")
				_, _ = io.WriteString(w, "abc")
				return
			}
			_, _ = io.WriteString(w, "abcdef")
		case "/conditional":
			if r.Header.Get("If-None-Match") != "" {
				w.WriteHeader(304)
				return
			}
			_, _ = io.WriteString(w, "changed")
		case "/redirect":
			w.Header().Set("Location", "/conditional")
			w.WriteHeader(302)
			_, _ = io.WriteString(w, "redirect")
		case "/oversize":
			_, _ = io.WriteString(w, "too large")
		case "/resume":
			w.Header().Set("ETag", `"stable"`)
			if resumes.Add(1) == 1 {
				w.Header().Set("Content-Length", "6")
				_, _ = io.WriteString(w, "abc")
				return
			}
			if r.Header.Get("Range") != "bytes=3-" {
				t.Errorf("resume range: %q", r.Header.Get("Range"))
			}
			w.Header().Set("Content-Range", "bytes 3-5/6")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, "def")
		}
	}))
	defer upstream.Close()
	fc, err := New(Options{Usage: u, MaxRetries: 1, TotalTimeout: time.Second * 5, AllowedHostRegex: []string{`^127\.0\.0\.1$`}})
	if err != nil {
		t.Fatal(err)
	}
	target := func(path string) *Target { return &Target{CanonicalHost: "127.0.0.1", URL: upstream.URL + path} }
	dst := &bufDst{}
	if _, err := fc.Fetch(context.Background(), target("/retry"), dst); err != nil {
		t.Fatal(err)
	}
	if dst.String() != "abcdef" || u.Snapshot().Lifetime.FetchedBytes != 9 {
		t.Fatal(dst.String(), u.Snapshot())
	}
	for _, etag := range []string{"", `"unchanged"`} {
		if _, err := fc.Conditional(t.Context(), target("/conditional"), etag, "", 100); err != nil {
			t.Fatal(err)
		}
	}
	if got := u.Snapshot().Lifetime.FetchedBytes; got != 16 {
		t.Fatal(got)
	}
	if _, err := fc.Fetch(t.Context(), target("/redirect"), &bufDst{}); err != nil {
		t.Fatal(err)
	}
	if got := u.Snapshot().Lifetime.FetchedBytes; got != 31 {
		t.Fatalf("redirect reads: %d want 31", got)
	}
	// Rejected Content-Length is not data read.
	if _, err := fc.Conditional(t.Context(), target("/oversize"), "", "", 1); err == nil {
		t.Fatal("expected body limit")
	}
	if got := u.Snapshot().Lifetime.FetchedBytes; got != 31 {
		t.Fatal(fmt.Sprint(got))
	}
	dst = &bufDst{}
	if _, err := fc.Fetch(t.Context(), target("/resume"), dst); err != nil {
		t.Fatal(err)
	}
	if got := u.Snapshot().Lifetime.FetchedBytes; got != 37 || dst.String() != "abcdef" {
		t.Fatalf("resumed bytes: %d, body %q", got, dst.String())
	}
}
