package fetch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/observability"
	"github.com/linsomniac/apt-cacher-ultra/internal/usage"
)

func TestUsageFetchRestartAndConditional(t *testing.T) {
	trace := observability.NewAdoptionTrace()
	ctx := observability.WithAdoptionTrace(context.Background(), trace)
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
	var logs bytes.Buffer
	fc, err := New(Options{Usage: u, Logger: slog.New(slog.NewJSONHandler(&logs, nil)), MaxRetries: 1, TotalTimeout: time.Second * 5, AllowedHostRegex: []string{`^127\.0\.0\.1$`}})
	if err != nil {
		t.Fatal(err)
	}
	target := func(path string) *Target { return &Target{CanonicalHost: "127.0.0.1", URL: upstream.URL + path} }
	dst := &bufDst{}
	if _, err := fc.Fetch(WithPurpose(ctx, "hot_prefetch"), target("/retry"), dst); err != nil {
		t.Fatal(err)
	}
	if dst.String() != "abcdef" || u.Snapshot().Lifetime.FetchedBytes != 9 {
		t.Fatal(dst.String(), u.Snapshot())
	}
	for _, etag := range []string{"", `"unchanged"`} {
		if _, err := fc.Conditional(ctx, target("/conditional"), etag, "", 100); err != nil {
			t.Fatal(err)
		}
	}
	if got := u.Snapshot().Lifetime.FetchedBytes; got != 16 {
		t.Fatal(got)
	}
	if _, err := fc.Fetch(ctx, target("/redirect?token=secret-token"), &bufDst{}); err != nil {
		t.Fatal(err)
	}
	if got := u.Snapshot().Lifetime.FetchedBytes; got != 31 {
		t.Fatalf("redirect reads: %d want 31", got)
	}
	// Rejected Content-Length is not data read.
	if _, err := fc.Conditional(ctx, target("/oversize"), "", "", 1); err == nil {
		t.Fatal("expected body limit")
	}
	if got := u.Snapshot().Lifetime.FetchedBytes; got != 31 {
		t.Fatal(fmt.Sprint(got))
	}
	dst = &bufDst{}
	if _, err := fc.Fetch(ctx, target("/resume"), dst); err != nil {
		t.Fatal(err)
	}
	if got := u.Snapshot().Lifetime.FetchedBytes; got != 37 || dst.String() != "abcdef" {
		t.Fatalf("resumed bytes: %d, body %q", got, dst.String())
	}
	var total int64
	var responses, partial, hot, redirects, unchanged, resumed int
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var event struct {
			Msg        string `json:"msg"`
			AdoptionID string `json:"adoption_id"`
			Bytes      int64  `json:"bytes_read"`
			Status     int    `json:"status"`
			Complete   bool   `json:"complete"`
			ReadError  bool   `json:"read_error"`
			Purpose    string `json:"purpose"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.Msg != "upstream_response" {
			continue
		}
		if event.AdoptionID != trace.ID {
			t.Fatalf("lost attempt ID: %+v", event)
		}
		total += event.Bytes
		responses++
		if event.ReadError && !event.Complete {
			partial++
		}
		if event.Purpose == "hot_prefetch" {
			hot++
		}
		switch event.Status {
		case 302:
			redirects++
		case 304:
			unchanged++
		case 206:
			resumed++
		}
	}
	if total != u.Snapshot().Lifetime.FetchedBytes || responses != 9 || partial != 2 || hot != 2 || redirects != 1 || unchanged != 1 || resumed != 1 {
		t.Fatalf("logged bytes=%d responses=%d partial=%d hot=%d redirects=%d unchanged=%d resumed=%d\n%s", total, responses, partial, hot, redirects, unchanged, resumed, logs.String())
	}
	if traffic := trace.Traffic(); traffic.Bytes != 37 || traffic.Responses != 9 || traffic.HotPrefetchBytes != 9 || traffic.MemberBytes != 0 {
		t.Fatalf("adoption traffic: %+v", traffic)
	}
	if strings.Contains(logs.String(), "secret-token") {
		t.Fatal("query secret leaked to logs")
	}
}
