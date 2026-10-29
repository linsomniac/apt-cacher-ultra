package fetch

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// bufDst is the in-memory FetchDst used by tests. It tracks Written
// independently of the buffer to mirror the BlobWriter contract (Written
// reports the cumulative bytes accepted, and Truncate resets to zero).
type bufDst struct {
	buf     bytes.Buffer
	written int64
}

func (b *bufDst) Write(p []byte) (int, error) {
	n, err := b.buf.Write(p)
	b.written += int64(n)
	return n, err
}

func (b *bufDst) Written() int64 { return b.written }

func (b *bufDst) Truncate() error {
	b.buf.Reset()
	b.written = 0
	return nil
}

func (b *bufDst) String() string { return b.buf.String() }

// newTestClient builds a permissive Client suitable for httptest. The
// caller passes the allow regexes (typically `^127\.0\.0\.1$`); the
// deny-CIDR list is empty so 127.0.0.1 (where httptest binds) is
// reachable.
func newTestClient(t *testing.T, allow []string) *Client {
	t.Helper()
	c, err := New(Options{
		ConnectTimeout:   2 * time.Second,
		TotalTimeout:     5 * time.Second,
		MaxRetries:       3,
		AllowedHostRegex: allow,
		DenyTargetRanges: nil, // explicit empty: skip post-resolution check
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestNew_BadAllowRegex(t *testing.T) {
	_, err := New(Options{AllowedHostRegex: []string{"["}})
	if err == nil {
		t.Fatalf("expected compile error")
	}
	if !strings.Contains(err.Error(), "allowed_host_regex") {
		t.Errorf("want allowed_host_regex in error, got %v", err)
	}
}

func TestNew_BadCIDR(t *testing.T) {
	_, err := New(Options{DenyTargetRanges: []string{"not-a-cidr"}})
	if err == nil {
		t.Fatalf("expected CIDR error")
	}
	if !strings.Contains(err.Error(), "deny_target_ranges") {
		t.Errorf("want deny_target_ranges in error, got %v", err)
	}
}

func TestNew_NegativeMaxRetries(t *testing.T) {
	_, err := New(Options{MaxRetries: -1})
	if err == nil {
		t.Fatalf("expected error for negative max_retries")
	}
}

func TestNew_NegativeTotalTimeout(t *testing.T) {
	_, err := New(Options{TotalTimeout: -1})
	if err == nil {
		t.Fatalf("expected error for negative total_timeout")
	}
}

func TestFetch_BasicSuccess(t *testing.T) {
	body := []byte("hello world")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Last-Modified", "Mon, 01 Jan 2024 00:00:00 GMT")
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	client := newTestClient(t, []string{`^127\.0\.0\.1$`})
	dst := &bufDst{}
	res, err := client.Fetch(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL + "/foo",
	}, dst)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("Status: got %d, want 200", res.Status)
	}
	if res.ETag != `"v1"` {
		t.Errorf("ETag: got %q", res.ETag)
	}
	if res.LastModified == "" {
		t.Errorf("LastModified missing")
	}
	if res.ContentLength != int64(len(body)) {
		t.Errorf("ContentLength: got %d, want %d", res.ContentLength, len(body))
	}
	if dst.String() != string(body) {
		t.Errorf("body: got %q, want %q", dst.String(), body)
	}
	if dst.Written() != int64(len(body)) {
		t.Errorf("Written: got %d, want %d", dst.Written(), len(body))
	}
}

func TestFetch_HostNotAllowed(t *testing.T) {
	client, err := New(Options{
		AllowedHostRegex: []string{`^archive\.ubuntu\.com$`},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.Fetch(context.Background(), &Target{
		CanonicalHost: "evil.example.com",
		URL:           "http://evil.example.com/",
	}, &bufDst{})
	if !errors.Is(err, ErrHostNotAllowed) {
		t.Errorf("want ErrHostNotAllowed, got %v", err)
	}
}

func TestFetch_EmptyAllowDeniesAll(t *testing.T) {
	client, err := New(Options{AllowedHostRegex: []string{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.Fetch(context.Background(), &Target{
		CanonicalHost: "archive.ubuntu.com",
		URL:           "http://archive.ubuntu.com/",
	}, &bufDst{})
	if !errors.Is(err, ErrHostNotAllowed) {
		t.Errorf("want ErrHostNotAllowed, got %v", err)
	}
}

func TestFetch_DenyTargetRange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hi"))
	}))
	defer srv.Close()

	client, err := New(Options{
		AllowedHostRegex: []string{`^127\.0\.0\.1$`},
		DenyTargetRanges: []string{"127.0.0.0/8"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.Fetch(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL,
	}, &bufDst{})
	if !errors.Is(err, ErrTargetDenied) {
		t.Errorf("want ErrTargetDenied, got %v", err)
	}
}

func TestFetch_404NoRetry(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	client := newTestClient(t, []string{`^127\.0\.0\.1$`})
	_, err := client.Fetch(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL,
	}, &bufDst{})
	if !errors.Is(err, ErrUpstreamStatus) {
		t.Errorf("want ErrUpstreamStatus, got %v", err)
	}
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("want *StatusError, got %T: %v", err, err)
	}
	if se.Code != http.StatusNotFound {
		t.Errorf("StatusError.Code = %d, want %d", se.Code, http.StatusNotFound)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts: got %d, want 1 (4xx is non-retryable)", got)
	}
}

func TestFetch_500ThenSuccess(t *testing.T) {
	body := []byte("recovered")
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	client := newTestClient(t, []string{`^127\.0\.0\.1$`})
	dst := &bufDst{}
	res, err := client.Fetch(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL,
	}, dst)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("Status: got %d, want 200", res.Status)
	}
	if dst.String() != string(body) {
		t.Errorf("body: got %q, want %q", dst.String(), body)
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts: got %d, want 3", got)
	}
}

func TestFetch_500RetriesExhausted(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()

	client, err := New(Options{
		AllowedHostRegex: []string{`^127\.0\.0\.1$`},
		MaxRetries:       2,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.Fetch(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL,
	}, &bufDst{})
	if !errors.Is(err, ErrUpstreamUnavailable) {
		t.Errorf("want ErrUpstreamUnavailable, got %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts: got %d, want 3 (1 initial + 2 retries)", got)
	}
}

// TestFetch_ResumeWithRange verifies SPEC §6.3 resumable Range retries:
// initial fetch sends 6 bytes of an 11-byte body then drops the conn;
// the retry sends `Range: bytes=6-` with the captured ETag, and the
// server returns 206 Partial Content with the remaining bytes.
func TestFetch_ResumeWithRange(t *testing.T) {
	full := []byte("hello world") // 11 bytes
	const half = 6                // "hello "
	var (
		attempts    atomic.Int32
		lastRange   atomic.Value // string
		lastIfRange atomic.Value
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		lastRange.Store(r.Header.Get("Range"))
		lastIfRange.Store(r.Header.Get("If-Range"))
		if n == 1 {
			// First attempt: write half the body and drop the conn.
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("not Hijacker")
			}
			conn, bw, err := hj.Hijack()
			if err != nil {
				t.Fatalf("Hijack: %v", err)
			}
			_, _ = fmt.Fprintf(bw, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nETag: \"v1\"\r\nLast-Modified: Mon, 01 Jan 2024 00:00:00 GMT\r\nContent-Type: text/plain\r\n\r\n", len(full))
			_, _ = bw.Write(full[:half])
			_ = bw.Flush()
			_ = conn.Close()
			return
		}
		// Subsequent attempts: serve as a 206 from the byte the client requested.
		if r.Header.Get("Range") == "" {
			t.Errorf("expected Range header on retry, got none")
		}
		if r.Header.Get("If-Range") != `"v1"` {
			t.Errorf("expected If-Range \"v1\", got %q", r.Header.Get("If-Range"))
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", half, len(full)-1, len(full)))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(full)-half))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(full[half:])
	}))
	defer srv.Close()

	client := newTestClient(t, []string{`^127\.0\.0\.1$`})
	dst := &bufDst{}
	res, err := client.Fetch(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL,
	}, dst)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("Status: got %d, want 200 (206 is hidden)", res.Status)
	}
	if dst.String() != string(full) {
		t.Errorf("body: got %q, want %q", dst.String(), full)
	}
	if dst.Written() != int64(len(full)) {
		t.Errorf("Written: got %d, want %d", dst.Written(), len(full))
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("attempts: got %d, want 2", got)
	}
	if lr, _ := lastRange.Load().(string); lr != fmt.Sprintf("bytes=%d-", half) {
		t.Errorf("last Range: got %q, want bytes=%d-", lr, half)
	}
	if lir, _ := lastIfRange.Load().(string); lir != `"v1"` {
		t.Errorf("last If-Range: got %q", lir)
	}
}

// TestFetch_RestartOnValidatorChange verifies SPEC §6.3 fallback: when a
// retry's Range request returns 200 instead of 206 (the server says the
// validator no longer matches), the partial bytes are discarded and the
// fetch restarts from byte 0.
func TestFetch_RestartOnValidatorChange(t *testing.T) {
	first := []byte("oldoldoldol")  // 11 bytes; first attempt sends partial
	second := []byte("brandnewBC!") // 11 bytes; new full body after validator change
	const half = 5
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		switch n {
		case 1:
			hj, _ := w.(http.Hijacker)
			conn, bw, _ := hj.Hijack()
			_, _ = fmt.Fprintf(bw, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nETag: \"v1\"\r\nContent-Type: application/octet-stream\r\n\r\n", len(first))
			_, _ = bw.Write(first[:half])
			_ = bw.Flush()
			_ = conn.Close()
		case 2:
			// Retry sent Range. Server returns 200 (validator changed).
			if r.Header.Get("Range") == "" {
				t.Errorf("expected Range on retry")
			}
			w.Header().Set("ETag", `"v2"`)
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(second)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(second)
		default:
			t.Errorf("unexpected attempt %d", n)
		}
	}))
	defer srv.Close()

	client := newTestClient(t, []string{`^127\.0\.0\.1$`})
	dst := &bufDst{}
	res, err := client.Fetch(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL,
	}, dst)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if dst.String() != string(second) {
		t.Errorf("body: got %q, want %q (post-restart)", dst.String(), second)
	}
	if res.ETag != `"v2"` {
		t.Errorf("ETag: got %q, want \"v2\" (refreshed validator)", res.ETag)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("attempts: got %d, want 2", got)
	}
}

func TestFetch_ContentLengthMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Declare 100 bytes, send 5 by hijacking and closing.
		hj, _ := w.(http.Hijacker)
		conn, bw, _ := hj.Hijack()
		_, _ = fmt.Fprintf(bw, "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n")
		_, _ = bw.Write([]byte("short"))
		_ = bw.Flush()
		_ = conn.Close()
	}))
	defer srv.Close()

	client, err := New(Options{
		AllowedHostRegex: []string{`^127\.0\.0\.1$`},
		MaxRetries:       0, // exhaust on first attempt so we see the error
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.Fetch(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL,
	}, &bufDst{})
	// With max_retries=0, a transient mid-body EOF (or size mismatch on
	// retry) wraps as ErrUpstreamUnavailable.
	if !errors.Is(err, ErrUpstreamUnavailable) {
		t.Errorf("want ErrUpstreamUnavailable, got %v", err)
	}
}

func TestFetch_ContextCanceled(t *testing.T) {
	// Server holds the request open until the client disconnects, then
	// returns. Listening on r.Context().Done() lets srv.Close() drain
	// immediately once the test cancels its ctx.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	client := newTestClient(t, []string{`^127\.0\.0\.1$`})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := client.Fetch(ctx, &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL,
	}, &bufDst{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
}

func TestFetch_BadContentRange(t *testing.T) {
	full := []byte("hello world") // 11 bytes
	const half = 6
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			// First attempt: half-body then drop.
			hj, _ := w.(http.Hijacker)
			conn, bw, _ := hj.Hijack()
			_, _ = fmt.Fprintf(bw, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nETag: \"v1\"\r\n\r\n", len(full))
			_, _ = bw.Write(full[:half])
			_ = bw.Flush()
			_ = conn.Close()
			return
		}
		// Retry: 206 with bogus Content-Range (wrong start offset).
		w.Header().Set("Content-Range", "bytes 0-4/11")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("WORLD"))
	}))
	defer srv.Close()

	client, err := New(Options{
		AllowedHostRegex: []string{`^127\.0\.0\.1$`},
		MaxRetries:       1, // one retry budget — second attempt's bad range exhausts it
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.Fetch(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL,
	}, &bufDst{})
	// Bad Content-Range is retryable, but we've used our budget. The
	// outer error is ErrUpstreamUnavailable.
	if !errors.Is(err, ErrUpstreamUnavailable) {
		t.Errorf("want ErrUpstreamUnavailable, got %v", err)
	}
}

func TestFetch_TotalSizeChangeOn206(t *testing.T) {
	// First attempt: declare total=11, send 6 bytes, drop.
	// Retry sends Range; server returns 206 but with /99 — total mismatch.
	const half = 6
	full := []byte("hello world")
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			hj, _ := w.(http.Hijacker)
			conn, bw, _ := hj.Hijack()
			_, _ = fmt.Fprintf(bw, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nETag: \"v1\"\r\n\r\n", len(full))
			_, _ = bw.Write(full[:half])
			_ = bw.Flush()
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Range", "bytes 6-98/99")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(make([]byte, 99-6))
	}))
	defer srv.Close()

	client, err := New(Options{
		AllowedHostRegex: []string{`^127\.0\.0\.1$`},
		MaxRetries:       1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.Fetch(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL,
	}, &bufDst{})
	if !errors.Is(err, ErrUpstreamUnavailable) {
		t.Errorf("want ErrUpstreamUnavailable wrapping ErrTotalSizeMismatch, got %v", err)
	}
}

func TestFetch_RestartWhenNoValidator(t *testing.T) {
	// First attempt: 200 with no ETag, no Last-Modified; mid-body drop.
	// Outer loop should refuse to send Range without a validator,
	// truncate, and restart. Second attempt succeeds.
	body := []byte("complete!!!") // 11 bytes
	const half = 4
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if r.Header.Get("Range") != "" {
			t.Errorf("attempt %d: should not send Range without validator", n)
		}
		if n == 1 {
			hj, _ := w.(http.Hijacker)
			conn, bw, _ := hj.Hijack()
			_, _ = fmt.Fprintf(bw, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n", len(body))
			_, _ = bw.Write(body[:half])
			_ = bw.Flush()
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	client := newTestClient(t, []string{`^127\.0\.0\.1$`})
	dst := &bufDst{}
	res, err := client.Fetch(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL,
	}, dst)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if dst.String() != string(body) {
		t.Errorf("body: got %q, want %q", dst.String(), body)
	}
	if res.ContentLength != int64(len(body)) {
		t.Errorf("ContentLength: got %d, want %d", res.ContentLength, len(body))
	}
}

func TestParseContentRange(t *testing.T) {
	cases := []struct {
		in          string
		first, last int64
		total       int64
		wantErr     bool
	}{
		{"bytes 0-9/10", 0, 9, 10, false},
		{"bytes 5-9/10", 5, 9, 10, false},
		{"bytes 0-99/*", 0, 99, -1, false},
		{"  bytes 0-9/10  ", 0, 9, 10, false},
		{"items 0-9/10", 0, 0, 0, true},
		{"bytes 0-9", 0, 0, 0, true},
		{"bytes /10", 0, 0, 0, true},
		{"bytes a-9/10", 0, 0, 0, true},
		{"bytes 0-b/10", 0, 0, 0, true},
		{"bytes 9-0/10", 0, 0, 0, true},
		{"bytes 0-9/x", 0, 0, 0, true},
	}
	for _, tc := range cases {
		first, last, total, err := parseContentRange(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseContentRange(%q): want error, got (%d, %d, %d)", tc.in, first, last, total)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseContentRange(%q): got %v", tc.in, err)
			continue
		}
		if first != tc.first || last != tc.last || total != tc.total {
			t.Errorf("parseContentRange(%q): got (%d, %d, %d), want (%d, %d, %d)",
				tc.in, first, last, total, tc.first, tc.last, tc.total)
		}
	}
}

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{context.Canceled, false},
		{context.DeadlineExceeded, false},
		{fmt.Errorf("%w: example.com", ErrHostNotAllowed), false},
		{fmt.Errorf("%w: 1.2.3.4", ErrTargetDenied), false},
		{fmt.Errorf("%w: status=404", ErrUpstreamStatus), false},
		{&StatusError{Code: 404}, false},
		{&StatusError{Code: 451}, false},
		{fmt.Errorf("%w: status=500", ErrUpstreamServerError), true},
		{fmt.Errorf("%w: bad", ErrInvalidContentRange), true},
		{fmt.Errorf("%w: short", ErrSizeMismatch), true},
		{fmt.Errorf("%w: more than 10000 bytes", ErrBodyTooLarge), false},
		{fmt.Errorf("%w: redir", ErrRedirectBlocked), false},
		{fmt.Errorf("%w: scheme \"ftp\"", ErrInvalidURL), false},
		{errors.New("some IO error"), true},
	}
	for _, tc := range cases {
		got := isRetryable(tc.err)
		if got != tc.want {
			t.Errorf("isRetryable(%v): got %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestAddrInDeny(t *testing.T) {
	deny, err := parseDenyCIDRs([]string{
		"127.0.0.0/8", "10.0.0.0/8", "::1/128",
	})
	if err != nil {
		t.Fatalf("parseDenyCIDRs: %v", err)
	}
	cases := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},
		{"127.255.255.255", true},
		{"10.0.0.1", true},
		{"::1", true},
		{"8.8.8.8", false},
		{"2606:4700:4700::1111", false},
	}
	for _, tc := range cases {
		ip, err := netip.ParseAddr(tc.ip)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.ip, err)
		}
		got, _ := addrInDeny(ip, deny)
		if got != tc.want {
			t.Errorf("addrInDeny(%s): got %v, want %v", tc.ip, got, tc.want)
		}
	}
}

// TestAddrInDeny_IPv4MappedIPv6 covers the SSRF defense for dual-stack
// sockets that present an IPv4 destination as ::ffff:a.b.c.d. Without the
// Unmap fallback, a deny entry for 169.254.0.0/16 would let through the
// mapped form ::ffff:169.254.169.254 (cloud metadata over IPv6).
func TestAddrInDeny_IPv4MappedIPv6(t *testing.T) {
	deny, err := parseDenyCIDRs([]string{
		"169.254.0.0/16",
		"10.0.0.0/8",
		"127.0.0.0/8",
	})
	if err != nil {
		t.Fatalf("parseDenyCIDRs: %v", err)
	}
	cases := []struct {
		ip   string
		want bool
	}{
		{"::ffff:169.254.169.254", true}, // metadata, mapped form
		{"::ffff:10.0.0.1", true},        // RFC1918, mapped
		{"::ffff:127.0.0.1", true},       // loopback, mapped
		{"::ffff:8.8.8.8", false},        // public, mapped — pass
	}
	for _, tc := range cases {
		ip, err := netip.ParseAddr(tc.ip)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.ip, err)
		}
		got, _ := addrInDeny(ip, deny)
		if got != tc.want {
			t.Errorf("addrInDeny(%s): got %v, want %v", tc.ip, got, tc.want)
		}
	}
}

// TestFetch_RedirectBlocked verifies that 3xx responses to a host NOT in
// the allowlist are refused with ErrRedirectBlocked. Without CheckRedirect,
// http.Client would silently follow — bypassing the allowlist.
func TestFetch_RedirectBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, "http://other.example.com/", http.StatusFound)
	}))
	defer srv.Close()

	client := newTestClient(t, []string{`^127\.0\.0\.1$`})
	_, err := client.Fetch(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL,
	}, &bufDst{})
	if !errors.Is(err, ErrRedirectBlocked) {
		t.Errorf("want ErrRedirectBlocked, got %v", err)
	}
}

// TestFetch_RedirectSchemeDowngradeBlocked verifies that, when
// AllowHTTPSToHTTPRedirect is false, an HTTPS upstream that 30x's to an
// HTTP target on an allowlisted host is refused with ErrRedirectBlocked.
// Operators deploying this option opt out of trusting their plaintext
// hop — typical when the cached content is NOT covered by an apt-secure
// signature chain (e.g. unsigned third-party tarballs).
func TestFetch_RedirectSchemeDowngradeBlocked(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("attacker-controlled bytes"))
	}))
	defer plain.Close()

	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/payload", http.StatusFound)
	}))
	defer tlsSrv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(tlsSrv.Certificate())
	restore := SetRootCAsForTest(pool)
	defer restore()

	c, err := New(Options{
		ConnectTimeout:           2 * time.Second,
		TotalTimeout:             5 * time.Second,
		MaxRetries:               0,
		AllowedHostRegex:         []string{`^127\.0\.0\.1$`},
		AllowHTTPSToHTTPRedirect: false,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dst := &bufDst{}
	_, err = c.Fetch(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           tlsSrv.URL + "/start",
	}, dst)
	if !errors.Is(err, ErrRedirectBlocked) {
		t.Fatalf("want ErrRedirectBlocked, got %v", err)
	}
	if !strings.Contains(err.Error(), "scheme downgrade") {
		t.Errorf("error %q should mention scheme downgrade", err)
	}
	if dst.Written() != 0 {
		t.Errorf("dst.Written() = %d; downgrade-blocked fetch must not stream the redirect body", dst.Written())
	}
}

// TestFetch_RedirectSchemeDowngradeFollowedByDefault is the companion to
// TestFetch_RedirectSchemeDowngradeBlocked: with AllowHTTPSToHTTPRedirect
// true (the production default — see config.defaultConfig), the same
// HTTPS→HTTP redirect to an allowlisted host is followed and the body
// from the plaintext hop is streamed into dst. The threat model here is
// covered by apt-secure's GPG-signed InRelease + per-artifact hash pins;
// poisoned bytes still fail signature/hash verification client-side.
func TestFetch_RedirectSchemeDowngradeFollowedByDefault(t *testing.T) {
	wantBody := []byte("downgrade payload")
	var plainHits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		plainHits.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(wantBody)))
		_, _ = w.Write(wantBody)
	}))
	defer plain.Close()

	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/payload", http.StatusFound)
	}))
	defer tlsSrv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(tlsSrv.Certificate())
	restore := SetRootCAsForTest(pool)
	defer restore()

	c, err := New(Options{
		ConnectTimeout:           2 * time.Second,
		TotalTimeout:             5 * time.Second,
		MaxRetries:               0,
		AllowedHostRegex:         []string{`^127\.0\.0\.1$`},
		AllowHTTPSToHTTPRedirect: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dst := &bufDst{}
	res, err := c.Fetch(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           tlsSrv.URL + "/start",
	}, dst)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("Status = %d, want 200", res.Status)
	}
	if dst.String() != string(wantBody) {
		t.Errorf("body = %q, want %q", dst.String(), string(wantBody))
	}
	if got := plainHits.Load(); got != 1 {
		t.Errorf("plain HTTP hits = %d, want 1 (downgrade was supposed to be followed)", got)
	}
}

// TestFetch_RedirectFollowedToAllowedHost verifies that a 3xx whose target
// host IS in the allowlist is followed transparently and the final body is
// streamed into dst. The allow regex `^127\.0\.0\.1$` matches both
// httptest servers (both bind 127.0.0.1, just on different ports), which
// is the same shape as the real packages.microsoft.com → CDN handoff
// where the operator has put both hosts in allowed_host_regex.
func TestFetch_RedirectFollowedToAllowedHost(t *testing.T) {
	wantBody := []byte("redirected payload")
	var firstHits, finalHits atomic.Int32
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		finalHits.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(wantBody)))
		_, _ = w.Write(wantBody)
	}))
	defer final.Close()

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstHits.Add(1)
		http.Redirect(w, r, final.URL+"/payload", http.StatusFound)
	}))
	defer first.Close()

	client := newTestClient(t, []string{`^127\.0\.0\.1$`})
	dst := &bufDst{}
	res, err := client.Fetch(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           first.URL + "/start",
	}, dst)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("Status = %d, want 200", res.Status)
	}
	if dst.String() != string(wantBody) {
		t.Errorf("body = %q, want %q", dst.String(), string(wantBody))
	}
	if got := firstHits.Load(); got != 1 {
		t.Errorf("first server hits = %d, want 1", got)
	}
	if got := finalHits.Load(); got != 1 {
		t.Errorf("final server hits = %d, want 1", got)
	}
}

// TestFetch_URLHostMismatch is the defense-in-depth check: if a buggy
// caller passes CanonicalHost="archive.ubuntu.com" but URL="http://evil/",
// fetch must refuse before any network I/O.
func TestFetch_URLHostMismatch(t *testing.T) {
	client, err := New(Options{
		AllowedHostRegex: []string{`^archive\.ubuntu\.com$`, `^evil\.example\.com$`},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.Fetch(context.Background(), &Target{
		CanonicalHost: "archive.ubuntu.com",
		URL:           "http://evil.example.com/x",
	}, &bufDst{})
	if !errors.Is(err, ErrInvalidURL) {
		t.Errorf("want ErrInvalidURL, got %v", err)
	}
}

func TestFetch_RejectsNonHTTPScheme(t *testing.T) {
	client, err := New(Options{AllowedHostRegex: []string{`.*`}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.Fetch(context.Background(), &Target{
		CanonicalHost: "example.com",
		URL:           "ftp://example.com/foo",
	}, &bufDst{})
	if !errors.Is(err, ErrInvalidURL) {
		t.Errorf("want ErrInvalidURL, got %v", err)
	}
}

func TestFetch_HostMatchAcrossCaseAndDot(t *testing.T) {
	body := []byte("ok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	// Caller passed mixed-case CanonicalHost with trailing dot; URL.Hostname()
	// is bare lowercase. normalizeHost should reconcile both to "127.0.0.1".
	client := newTestClient(t, []string{`^127\.0\.0\.1$`})
	dst := &bufDst{}
	_, err := client.Fetch(context.Background(), &Target{
		CanonicalHost: "127.0.0.1.",
		URL:           srv.URL,
	}, dst)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if dst.String() != string(body) {
		t.Errorf("body: %q", dst.String())
	}
}

// TestFetch_206RejectsUnknownTotal verifies that a 206 with "*" total is
// rejected when we have a known expected total from the initial 200.
// Without this check, a server could respond with bytes 6-10/* and the
// "total matches" rule from SPEC §6.3 could not be enforced.
func TestFetch_206RejectsUnknownTotal(t *testing.T) {
	full := []byte("hello world")
	const half = 6
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			hj, _ := w.(http.Hijacker)
			conn, bw, _ := hj.Hijack()
			_, _ = fmt.Fprintf(bw, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nETag: \"v1\"\r\n\r\n", len(full))
			_, _ = bw.Write(full[:half])
			_ = bw.Flush()
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Range", "bytes 6-10/*")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(full[half:])
	}))
	defer srv.Close()

	client, err := New(Options{
		AllowedHostRegex: []string{`^127\.0\.0\.1$`},
		MaxRetries:       1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.Fetch(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL,
	}, &bufDst{})
	if !errors.Is(err, ErrUpstreamUnavailable) {
		t.Errorf("want ErrUpstreamUnavailable wrapping ErrInvalidContentRange, got %v", err)
	}
}

// TestFetch_206RejectsLastBeyondTotal: server claims bytes 6-99/11 — the
// last byte index is past the stated total. Reject as malformed.
func TestFetch_206RejectsLastBeyondTotal(t *testing.T) {
	full := []byte("hello world")
	const half = 6
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			hj, _ := w.(http.Hijacker)
			conn, bw, _ := hj.Hijack()
			_, _ = fmt.Fprintf(bw, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nETag: \"v1\"\r\n\r\n", len(full))
			_, _ = bw.Write(full[:half])
			_ = bw.Flush()
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Range", "bytes 6-99/11")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()

	client, err := New(Options{
		AllowedHostRegex: []string{`^127\.0\.0\.1$`},
		MaxRetries:       1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.Fetch(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL,
	}, &bufDst{})
	if !errors.Is(err, ErrUpstreamUnavailable) {
		t.Errorf("want ErrUpstreamUnavailable wrapping ErrInvalidContentRange, got %v", err)
	}
}

// TestFetch_206BodyShorterThanRange: server declares bytes 6-10 but
// delivers fewer bytes than that. The dst.Written != last+1 check
// must catch the mismatch.
func TestFetch_206BodyShorterThanRange(t *testing.T) {
	full := []byte("hello world")
	const half = 6
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			hj, _ := w.(http.Hijacker)
			conn, bw, _ := hj.Hijack()
			_, _ = fmt.Fprintf(bw, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nETag: \"v1\"\r\n\r\n", len(full))
			_, _ = bw.Write(full[:half])
			_ = bw.Flush()
			_ = conn.Close()
			return
		}
		// 206 announces 5 bytes (6-10) but delivers 2.
		w.Header().Set("Content-Range", "bytes 6-10/11")
		w.Header().Set("Content-Length", "2")
		w.WriteHeader(http.StatusPartialContent)
		hj, _ := w.(http.Hijacker)
		conn, bw, _ := hj.Hijack()
		_, _ = bw.Write([]byte("xx"))
		_ = bw.Flush()
		_ = conn.Close()
	}))
	defer srv.Close()

	client, err := New(Options{
		AllowedHostRegex: []string{`^127\.0\.0\.1$`},
		MaxRetries:       1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.Fetch(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL,
	}, &bufDst{})
	if !errors.Is(err, ErrUpstreamUnavailable) {
		t.Errorf("want ErrUpstreamUnavailable, got %v", err)
	}
}

// writeErrorDst is a FetchDst whose Write always fails. Used by
// TestFetch_DstWriteErrorExhaustsAsUnavailable to simulate the cache-disk-
// full failure mode (SPEC §11 row 14) without provoking real ENOSPC.
type writeErrorDst struct {
	err     error
	written int64
}

func (w *writeErrorDst) Write(p []byte) (int, error) { return 0, w.err }
func (w *writeErrorDst) Written() int64              { return w.written }
func (w *writeErrorDst) Truncate() error             { w.written = 0; return nil }

// TestFetch_DstWriteErrorReturnsCacheWriteFailed covers SPEC §11 row 14:
// a cache-side write failure (e.g. disk full) is tagged as
// ErrCacheWriteFailed by streamBody, classified non-retryable, and
// returned without re-asking upstream. Re-trying the upstream would just
// stream more bytes into the same broken disk; the handler maps this
// sentinel to a loud slog.Error so an operator sees the actual condition.
func TestFetch_DstWriteErrorReturnsCacheWriteFailed(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 256)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	client := newTestClient(t, []string{`^127\.0\.0\.1$`})
	dst := &writeErrorDst{err: errors.New("simulated disk full")}
	_, err := client.Fetch(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL + "/foo",
	}, dst)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrCacheWriteFailed) {
		t.Errorf("want ErrCacheWriteFailed, got %v", err)
	}
	if errors.Is(err, ErrUpstreamUnavailable) {
		t.Errorf("must not be wrapped as ErrUpstreamUnavailable: %v", err)
	}
	if !strings.Contains(err.Error(), "simulated disk full") {
		t.Errorf("expected error to surface underlying cause, got %v", err)
	}
	// No retries — cache write errors don't get better by re-fetching.
	if got := hits.Load(); got != 1 {
		t.Errorf("upstream hits=%d, want 1 (no retry on cache write error)", got)
	}
}

func TestNormalizeHost(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"archive.ubuntu.com", "archive.ubuntu.com"},
		{"ARCHIVE.UBUNTU.COM", "archive.ubuntu.com"},
		{"archive.ubuntu.com.", "archive.ubuntu.com"},
		{"ARCHIVE.UBUNTU.COM.", "archive.ubuntu.com"},
		{"[::1]", "::1"},
		{"[::FFFF:127.0.0.1]", "::ffff:127.0.0.1"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := normalizeHost(tc.in); got != tc.want {
			t.Errorf("normalizeHost(%q): got %q, want %q", tc.in, got, tc.want)
		}
	}
}

// gzipped returns b gzip-compressed: the body of a Content-Encoding: gzip
// response.
func gzipped(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// gzipWhenAskedUpstream answers every request with body, gzip-encoded (gz)
// only when the request's Accept-Encoding asks for gzip, as a web server
// that compresses on the fly does. It records each request's
// Accept-Encoding values and protocol major version.
type gzipWhenAskedUpstream struct {
	body, gz []byte

	mu       sync.Mutex
	encoding [][]string // r.Header.Values("Accept-Encoding"), per request
	proto    []int      // r.ProtoMajor, per request
}

func (u *gzipWhenAskedUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	values := r.Header.Values("Accept-Encoding")
	u.mu.Lock()
	u.encoding = append(u.encoding, values)
	u.proto = append(u.proto, r.ProtoMajor)
	u.mu.Unlock()
	for _, v := range values {
		if strings.Contains(v, "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Content-Length", fmt.Sprint(len(u.gz)))
			_, _ = w.Write(u.gz)
			return
		}
	}
	w.Header().Set("Content-Length", fmt.Sprint(len(u.body)))
	_, _ = w.Write(u.body)
}

func (u *gzipWhenAskedUpstream) seen() (encoding [][]string, proto []int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([][]string(nil), u.encoding...), append([]int(nil), u.proto...)
}

// TestFetch_SendsNoAcceptEncoding: the upstream transport asks for no
// Content-Encoding (the 2026-09 security review's VA-3, ruling HD-5), so an
// upstream that compresses only when asked sends the identity body, and
// that is what lands in dst. Go's default transport sends
// "Accept-Encoding: gzip" and decodes the answer, which left the decoded
// body without a declared length.
func TestFetch_SendsNoAcceptEncoding(t *testing.T) {
	body := []byte(strings.Repeat("Package: identity\n", 64))
	up := &gzipWhenAskedUpstream{body: body, gz: gzipped(t, body)}
	srv := httptest.NewServer(up)
	defer srv.Close()

	client := newTestClient(t, []string{`^127\.0\.0\.1$`})
	dst := &bufDst{}
	if _, err := client.Fetch(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL + "/dists/stable/main/binary-amd64/Packages",
	}, dst); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	encoding, _ := up.seen()
	if len(encoding) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(encoding))
	}
	if len(encoding[0]) != 0 {
		t.Errorf("Accept-Encoding sent: %q, want none", encoding[0])
	}
	if dst.String() != string(body) {
		t.Errorf("dst holds %d bytes, want the %d-byte identity body", dst.Written(), len(body))
	}
}

// TestFetch_SendsNoAcceptEncodingOverHTTP2 is TestFetch_SendsNoAcceptEncoding
// over HTTP/2: the HTTP/2 transport honours DisableCompression too.
func TestFetch_SendsNoAcceptEncodingOverHTTP2(t *testing.T) {
	body := []byte(strings.Repeat("Package: identity over h2\n", 64))
	up := &gzipWhenAskedUpstream{body: body, gz: gzipped(t, body)}
	srv := httptest.NewUnstartedServer(up)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	// newTransport reads the pool once, at construction: install it before
	// building the client (dialer_tls_seam_test.go).
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	restore := SetRootCAsForTest(pool)
	defer restore()

	client := newTestClient(t, []string{`^127\.0\.0\.1$`})
	dst := &bufDst{}
	if _, err := client.Fetch(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL + "/dists/stable/main/binary-amd64/Packages",
	}, dst); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	encoding, proto := up.seen()
	if len(proto) != 1 || proto[0] != 2 {
		t.Fatalf("upstream saw requests of HTTP major versions %v, want one HTTP/2 request", proto)
	}
	if len(encoding[0]) != 0 {
		t.Errorf("Accept-Encoding sent over HTTP/2: %q, want none", encoding[0])
	}
	if dst.String() != string(body) {
		t.Errorf("dst holds %d bytes, want the %d-byte identity body", dst.Written(), len(body))
	}
}

// TestFetch_StoresAGzipBodyItDidNotAskFor: an upstream that answers with
// Content-Encoding: gzip unasked (an Apache serving a .gz file that way)
// has its bytes stored as sent, compressed, with their declared length —
// not decoded, as the bytes of the file it names are those compressed
// bytes.
func TestFetch_StoresAGzipBodyItDidNotAskFor(t *testing.T) {
	gz := gzipped(t, []byte(strings.Repeat("Package: compressed\n", 64)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", fmt.Sprint(len(gz)))
		_, _ = w.Write(gz)
	}))
	defer srv.Close()

	client := newTestClient(t, []string{`^127\.0\.0\.1$`})
	dst := &bufDst{}
	res, err := client.Fetch(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL + "/dists/stable/main/binary-amd64/Packages.gz",
	}, dst)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !bytes.Equal(dst.buf.Bytes(), gz) {
		t.Errorf("dst holds %d bytes, want the %d compressed bytes the upstream sent, unchanged", dst.Written(), len(gz))
	}
	if res.ContentLength != int64(len(gz)) {
		t.Errorf("ContentLength = %d, want the declared %d", res.ContentLength, len(gz))
	}
}

// writeFlushedBody answers a 200 without a Content-Length and writes n
// bytes in 4 KiB writes, flushing after each, so the body goes out chunked.
// It stops at the first failed write.
func writeFlushedBody(w http.ResponseWriter, n int) {
	flusher := w.(http.Flusher)
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	chunk := bytes.Repeat([]byte("x"), 4096)
	for left := n; left > 0; left -= len(chunk) {
		if left < len(chunk) {
			chunk = chunk[:left]
		}
		if _, err := w.Write(chunk); err != nil {
			return
		}
		flusher.Flush()
	}
}

// cutChunked200 answers with a chunked 200 that carries an ETag, sends n
// body bytes in one chunk and drops the connection: a download cut
// mid-body, which Fetch resumes with a Range request.
func cutChunked200(t *testing.T, w http.ResponseWriter, n int) {
	conn, bw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Errorf("Hijack: %v", err)
		return
	}
	defer func() { _ = conn.Close() }()
	_, _ = fmt.Fprintf(bw, "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nETag: \"v1\"\r\n\r\n%x\r\n", n)
	_, _ = bw.Write(bytes.Repeat([]byte("c"), n))
	_, _ = bw.WriteString("\r\n")
	_ = bw.Flush()
}

// TestFetch_MaxBytesStopsAChunkedBodyThatRunsOver: a body without a
// declared length that runs past Target.MaxBytes fails with
// ErrBodyTooLarge as soon as it does (the 2026-09 security review's A-2),
// and is not retried.
func TestFetch_MaxBytesStopsAChunkedBodyThatRunsOver(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writeFlushedBody(w, 1<<20)
	}))
	defer srv.Close()

	client := newTestClient(t, []string{`^127\.0\.0\.1$`})
	dst := &bufDst{}
	_, err := client.Fetch(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL + "/big",
		MaxBytes:      10000,
	}, dst)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("Fetch error = %v, want ErrBodyTooLarge", err)
	}
	if dst.Written() > 10001 {
		t.Errorf("dst.Written() = %d, want at most 10001 (MaxBytes and the one byte that shows the body runs over)", dst.Written())
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("upstream saw %d requests, want 1 (ErrBodyTooLarge is not retried)", got)
	}
}

// TestFetch_MaxBytesRefusesADeclaredLengthOverIt: a Content-Length over
// Target.MaxBytes fails before any of the body is read.
func TestFetch_MaxBytesRefusesADeclaredLengthOverIt(t *testing.T) {
	var requests atomic.Int32
	body := bytes.Repeat([]byte("d"), 50000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Length", "50000")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	client := newTestClient(t, []string{`^127\.0\.0\.1$`})
	dst := &bufDst{}
	_, err := client.Fetch(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL + "/declared-big",
		MaxBytes:      10000,
	}, dst)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("Fetch error = %v, want ErrBodyTooLarge", err)
	}
	if dst.Written() != 0 {
		t.Errorf("dst.Written() = %d, want 0 (refused before the body is read)", dst.Written())
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("upstream saw %d requests, want 1 (ErrBodyTooLarge is not retried)", got)
	}
}

// TestFetch_MaxBytesAcceptsABodyOfExactlyIt: a body of exactly
// Target.MaxBytes bytes passes, declared or not.
func TestFetch_MaxBytesAcceptsABodyOfExactlyIt(t *testing.T) {
	body := bytes.Repeat([]byte("e"), 10000)
	for _, declared := range []bool{true, false} {
		t.Run(fmt.Sprintf("Content-Length=%t", declared), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if declared {
					w.Header().Set("Content-Length", fmt.Sprint(len(body)))
				} else {
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush() // headers out first: the body goes chunked
				}
				_, _ = w.Write(body)
			}))
			defer srv.Close()

			client := newTestClient(t, []string{`^127\.0\.0\.1$`})
			dst := &bufDst{}
			res, err := client.Fetch(context.Background(), &Target{
				CanonicalHost: "127.0.0.1",
				URL:           srv.URL + "/exact",
				MaxBytes:      10000,
			}, dst)
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if !bytes.Equal(dst.buf.Bytes(), body) {
				t.Errorf("dst holds %d bytes, want the %d-byte body", dst.Written(), len(body))
			}
			wantLength := int64(-1)
			if declared {
				wantLength = int64(len(body))
			}
			if res.ContentLength != wantLength {
				t.Errorf("ContentLength = %d, want %d (the response's declared length)", res.ContentLength, wantLength)
			}
		})
	}
}

// TestFetch_MaxBytesRefusesAResumeDeclaringMore: a chunked 200 cut after
// 5,000 bytes is resumed, and a 206 whose Content-Range declares a total
// over Target.MaxBytes fails with ErrBodyTooLarge before its body is read,
// and is not retried.
func TestFetch_MaxBytesRefusesAResumeDeclaringMore(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch requests.Add(1) {
		case 1:
			cutChunked200(t, w, 5000)
		case 2:
			if got := r.Header.Get("Range"); got != "bytes=5000-" {
				t.Errorf("resume Range = %q, want bytes=5000-", got)
			}
			w.Header().Set("Content-Range", "bytes 5000-49999/50000")
			w.Header().Set("Content-Length", "45000")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(bytes.Repeat([]byte("r"), 45000))
		default:
			http.Error(w, "unexpected request", http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()

	client := newTestClient(t, []string{`^127\.0\.0\.1$`})
	dst := &bufDst{}
	_, err := client.Fetch(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL + "/resumed",
		MaxBytes:      10000,
	}, dst)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("Fetch error = %v, want ErrBodyTooLarge", err)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("upstream saw %d requests, want 2 (the cut 200 and its resume; ErrBodyTooLarge is not retried)", got)
	}
	if dst.Written() != 5000 {
		t.Errorf("dst.Written() = %d, want 5000 (the 206 is refused before its body is read)", dst.Written())
	}
}

// TestFetch_MaxBytesAcceptsAResumeOfExactlyIt: a chunked 200 cut after
// 5,000 bytes whose 206 declares bytes 5000-9999/10000 ends at exactly
// Target.MaxBytes, and passes: the 206's last is an index, so an object
// of MaxBytes bytes ends at last MaxBytes-1, and its total is MaxBytes.
func TestFetch_MaxBytesAcceptsAResumeOfExactlyIt(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch requests.Add(1) {
		case 1:
			cutChunked200(t, w, 5000)
		case 2:
			if got := r.Header.Get("Range"); got != "bytes=5000-" {
				t.Errorf("resume Range = %q, want bytes=5000-", got)
			}
			w.Header().Set("Content-Range", "bytes 5000-9999/10000")
			w.Header().Set("Content-Length", "5000")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(bytes.Repeat([]byte("r"), 5000))
		default:
			http.Error(w, "unexpected request", http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()

	client := newTestClient(t, []string{`^127\.0\.0\.1$`})
	dst := &bufDst{}
	if _, err := client.Fetch(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL + "/resumed-exact",
		MaxBytes:      10000,
	}, dst); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	want := append(bytes.Repeat([]byte("c"), 5000), bytes.Repeat([]byte("r"), 5000)...)
	if !bytes.Equal(dst.buf.Bytes(), want) {
		t.Errorf("dst holds %d bytes, want the 5,000 cut bytes and the 5,000 resumed ones", dst.Written())
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("upstream saw %d requests, want 2 (the cut 200 and its resume)", got)
	}
}

// TestFetch_MaxBytesStopsAResumedBodyThatRunsOver: the resume of a cut
// chunked 200 whose 206 declares a range within Target.MaxBytes but whose
// body runs past it fails with ErrBodyTooLarge as soon as it does, and is
// not retried. Without the cap at the 206's read it would end as
// ErrSizeMismatch, which is retried.
func TestFetch_MaxBytesStopsAResumedBodyThatRunsOver(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch requests.Add(1) {
		case 1:
			cutChunked200(t, w, 5000)
		case 2:
			if got := r.Header.Get("Range"); got != "bytes=5000-" {
				t.Errorf("resume Range = %q, want bytes=5000-", got)
			}
			w.Header().Set("Content-Range", "bytes 5000-9999/*")
			w.WriteHeader(http.StatusPartialContent)
			w.(http.Flusher).Flush() // headers out first: the body goes chunked
			_, _ = w.Write(bytes.Repeat([]byte("o"), 20000))
		default:
			http.Error(w, "unexpected request", http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()

	client := newTestClient(t, []string{`^127\.0\.0\.1$`})
	dst := &bufDst{}
	_, err := client.Fetch(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL + "/resumed-long",
		MaxBytes:      10000,
	}, dst)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("Fetch error = %v, want ErrBodyTooLarge", err)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("upstream saw %d requests, want 2 (the cut 200 and its resume; ErrBodyTooLarge is not retried)", got)
	}
	if dst.Written() > 10001 {
		t.Errorf("dst.Written() = %d, want at most 10001", dst.Written())
	}
}

// TestFetch_MaxBytesOnARestartedDownload: a chunked 200 cut after 5,000
// bytes is resumed, and the upstream answers the Range with a new 200 (a
// new ETag: the file was republished), which restarts the download from
// byte 0 (SPEC §6.3). Target.MaxBytes bounds the new file alone, never the
// 5,000 bytes the restart discards: a new body of exactly MaxBytes passes,
// declared or chunked, and dst holds just its bytes; one over it fails with
// ErrBodyTooLarge — a declared one before any of its body is read, a
// chunked one at MaxBytes and the one byte that shows it runs over — and is
// not retried (the hardening plan's final review, F-M1).
func TestFetch_MaxBytesOnARestartedDownload(t *testing.T) {
	for _, tc := range []struct {
		name     string
		n        int  // the new file's size
		declared bool // the new 200 carries a Content-Length
		over     bool // n is over MaxBytes
	}{
		{"exactly MaxBytes, declared", 10000, true, false},
		{"exactly MaxBytes, chunked", 10000, false, false},
		{"over MaxBytes, declared", 50000, true, true},
		{"over MaxBytes, chunked", 1 << 20, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch requests.Add(1) {
				case 1:
					cutChunked200(t, w, 5000)
				case 2:
					if got := r.Header.Get("Range"); got != "bytes=5000-" {
						t.Errorf("resume Range = %q, want bytes=5000-", got)
					}
					w.Header().Set("ETag", `"v2"`)
					if tc.declared {
						w.Header().Set("Content-Length", fmt.Sprint(tc.n))
						_, _ = w.Write(bytes.Repeat([]byte("x"), tc.n))
						return
					}
					writeFlushedBody(w, tc.n) // x bytes, chunked
				default:
					http.Error(w, "unexpected request", http.StatusServiceUnavailable)
				}
			}))
			defer srv.Close()

			client := newTestClient(t, []string{`^127\.0\.0\.1$`})
			dst := &bufDst{}
			_, err := client.Fetch(context.Background(), &Target{
				CanonicalHost: "127.0.0.1",
				URL:           srv.URL + "/republished",
				MaxBytes:      10000,
			}, dst)
			if got := requests.Load(); got != 2 {
				t.Errorf("upstream saw %d requests, want 2 (the cut 200 and the restarting one; ErrBodyTooLarge is not retried)", got)
			}
			switch {
			case !tc.over:
				if err != nil {
					t.Fatalf("Fetch: %v, want the restarted body of exactly MaxBytes to pass", err)
				}
				if !bytes.Equal(dst.buf.Bytes(), bytes.Repeat([]byte("x"), tc.n)) {
					t.Errorf("dst holds %d bytes, want just the new file's %d", dst.Written(), tc.n)
				}
			case !errors.Is(err, ErrBodyTooLarge):
				t.Fatalf("Fetch error = %v, want ErrBodyTooLarge", err)
			case tc.declared:
				if bytes.Contains(dst.buf.Bytes(), []byte("x")) {
					t.Errorf("dst holds %d bytes of the new file, want none: a declared length over MaxBytes is refused before its body is read",
						bytes.Count(dst.buf.Bytes(), []byte("x")))
				}
			default:
				if dst.Written() != 10001 {
					t.Errorf("dst.Written() = %d, want 10001: MaxBytes of the new file and the one byte that shows it runs over", dst.Written())
				}
			}
		})
	}
}

// TestFetch_MaxBytesAtMaxInt64ReadsTheWholeBody: Target.MaxBytes
// math.MaxInt64 is a limit no body reaches. The one byte past it that
// streamBody reads, to tell "runs over" from "ends there", must not wrap
// the read limit negative, which would read nothing.
func TestFetch_MaxBytesAtMaxInt64ReadsTheWholeBody(t *testing.T) {
	for _, declared := range []bool{false, true} {
		t.Run(fmt.Sprintf("Content-Length=%t", declared), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if declared {
					w.Header().Set("Content-Length", "100000")
					_, _ = w.Write(bytes.Repeat([]byte("x"), 100000))
					return
				}
				writeFlushedBody(w, 100000)
			}))
			defer srv.Close()

			client := newTestClient(t, []string{`^127\.0\.0\.1$`})
			dst := &bufDst{}
			res, err := client.Fetch(context.Background(), &Target{
				CanonicalHost: "127.0.0.1",
				URL:           srv.URL + "/unbounded",
				MaxBytes:      math.MaxInt64,
			}, dst)
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if dst.Written() != 100000 {
				t.Errorf("dst.Written() = %d, want 100000 (the whole body)", dst.Written())
			}
			wantLength := int64(-1)
			if declared {
				wantLength = 100000
			}
			if res.ContentLength != wantLength {
				t.Errorf("ContentLength = %d, want %d (the response's declared length)", res.ContentLength, wantLength)
			}
		})
	}
}

// TestFetch_ZeroMaxBytesIsNoLimit: Target.MaxBytes 0 sets no limit.
func TestFetch_ZeroMaxBytesIsNoLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeFlushedBody(w, 100000)
	}))
	defer srv.Close()

	client := newTestClient(t, []string{`^127\.0\.0\.1$`})
	dst := &bufDst{}
	res, err := client.Fetch(context.Background(), &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL + "/unlimited",
		MaxBytes:      0,
	}, dst)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if dst.Written() != 100000 {
		t.Errorf("dst.Written() = %d, want 100000", dst.Written())
	}
	if res.ContentLength != -1 {
		t.Errorf("ContentLength = %d, want -1 (the body is chunked)", res.ContentLength)
	}
}
