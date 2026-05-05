package fetch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestConditional_NotModified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != `"v1"` {
			t.Errorf("If-None-Match header missing: got %q", r.Header.Get("If-None-Match"))
		}
		if r.Header.Get("If-Modified-Since") != "Mon, 01 Jan 2024 00:00:00 GMT" {
			t.Errorf("If-Modified-Since header missing: got %q", r.Header.Get("If-Modified-Since"))
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	c := newTestClient(t, []string{`^127\.0\.0\.1$`})
	res, err := c.Conditional(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL + "/dists/noble/InRelease",
	}, `"v1"`, "Mon, 01 Jan 2024 00:00:00 GMT", 1<<20)
	if err != nil {
		t.Fatalf("Conditional: %v", err)
	}
	if res.Status != http.StatusNotModified {
		t.Errorf("Status = %d, want 304", res.Status)
	}
	if res.Body != nil {
		t.Errorf("Body should be nil on 304, got %d bytes", len(res.Body))
	}
}

func TestConditional_OK(t *testing.T) {
	body := []byte("Origin: Ubuntu\nLabel: Ubuntu\nSuite: noble\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v2"`)
		w.Header().Set("Last-Modified", "Tue, 02 Jan 2024 00:00:00 GMT")
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c := newTestClient(t, []string{`^127\.0\.0\.1$`})
	res, err := c.Conditional(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL + "/InRelease",
	}, "", "", 1<<20)
	if err != nil {
		t.Fatalf("Conditional: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Fatalf("Status = %d, want 200", res.Status)
	}
	if string(res.Body) != string(body) {
		t.Errorf("Body mismatch")
	}
	if res.ETag != `"v2"` {
		t.Errorf("ETag = %q", res.ETag)
	}
	if res.LastModified != "Tue, 02 Jan 2024 00:00:00 GMT" {
		t.Errorf("LastModified = %q", res.LastModified)
	}
}

func TestConditional_BodyTooLarge(t *testing.T) {
	body := make([]byte, 4096)
	for i := range body {
		body[i] = 'x'
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c := newTestClient(t, []string{`^127\.0\.0\.1$`})
	_, err := c.Conditional(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL + "/InRelease",
	}, "", "", 1024)
	if !errors.Is(err, ErrConditionalBodyTooLarge) {
		t.Fatalf("err = %v, want ErrConditionalBodyTooLarge", err)
	}
}

func TestConditional_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := newTestClient(t, []string{`^127\.0\.0\.1$`})
	_, err := c.Conditional(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL + "/InRelease",
	}, "", "", 1<<20)
	if !errors.Is(err, ErrUpstreamServerError) {
		t.Fatalf("err = %v, want ErrUpstreamServerError", err)
	}
}

func TestConditional_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(t, []string{`^127\.0\.0\.1$`})
	_, err := c.Conditional(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL + "/InRelease",
	}, "", "", 1<<20)
	if !errors.Is(err, ErrUpstreamStatus) {
		t.Fatalf("err = %v, want ErrUpstreamStatus", err)
	}
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *StatusError via errors.As", err)
	}
	if se.Code != http.StatusNotFound {
		t.Errorf("StatusError.Code = %d, want 404", se.Code)
	}
}

func TestConditional_HostNotAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, []string{`^never\.match$`})
	_, err := c.Conditional(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL + "/InRelease",
	}, "", "", 1<<20)
	if !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("err = %v, want ErrHostNotAllowed", err)
	}
}

func TestConditional_HostMismatch(t *testing.T) {
	c := newTestClient(t, []string{`.*`})
	_, err := c.Conditional(context.Background(), &Target{
		CanonicalHost: "lying.example",
		URL:           "http://127.0.0.1:1/foo",
	}, "", "", 1<<20)
	if !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("err = %v, want ErrInvalidURL", err)
	}
}

func TestConditional_CtxCanceled(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	// Order matters: release the handler before httptest's Close, which
	// otherwise WaitGroup-waits on the still-blocked goroutine forever.
	defer srv.Close()
	defer close(block)

	c := newTestClient(t, []string{`^127\.0\.0\.1$`})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := c.Conditional(ctx, &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL + "/InRelease",
	}, "", "", 1<<20)
	if err == nil {
		t.Fatalf("expected error")
	}
	// Either ctx error directly or wrapped via the http.Client.
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestConditional_NoMaxBody(t *testing.T) {
	c := newTestClient(t, []string{`.*`})
	_, err := c.Conditional(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           "http://127.0.0.1/InRelease",
	}, "", "", 0)
	if err == nil {
		t.Fatalf("expected error for maxBody=0")
	}
}

func TestConditional_NilTarget(t *testing.T) {
	c := newTestClient(t, []string{`.*`})
	_, err := c.Conditional(context.Background(), nil, "", "", 1024)
	if err == nil {
		t.Fatalf("expected error")
	}
}

func TestConditional_RedirectBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer srv.Close()

	c := newTestClient(t, []string{`^127\.0\.0\.1$`})
	_, err := c.Conditional(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL + "/InRelease",
	}, "", "", 1<<20)
	if !errors.Is(err, ErrRedirectBlocked) {
		t.Errorf("err = %v, want ErrRedirectBlocked", err)
	}
}
