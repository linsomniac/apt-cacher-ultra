package fetch

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
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
			fmt.Fprintf(bw, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nETag: \"v1\"\r\nLast-Modified: Mon, 01 Jan 2024 00:00:00 GMT\r\nContent-Type: text/plain\r\n\r\n", len(full))
			bw.Write(full[:half])
			bw.Flush()
			conn.Close()
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
			fmt.Fprintf(bw, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nETag: \"v1\"\r\nContent-Type: application/octet-stream\r\n\r\n", len(first))
			bw.Write(first[:half])
			bw.Flush()
			conn.Close()
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
		fmt.Fprintf(bw, "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n")
		bw.Write([]byte("short"))
		bw.Flush()
		conn.Close()
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
			fmt.Fprintf(bw, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nETag: \"v1\"\r\n\r\n", len(full))
			bw.Write(full[:half])
			bw.Flush()
			conn.Close()
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
			fmt.Fprintf(bw, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nETag: \"v1\"\r\n\r\n", len(full))
			bw.Write(full[:half])
			bw.Flush()
			conn.Close()
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
			fmt.Fprintf(bw, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n", len(body))
			bw.Write(body[:half])
			bw.Flush()
			conn.Close()
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

// TestFetch_RedirectSchemeDowngradeBlocked verifies that an HTTPS upstream
// that 30x's to an HTTP target on an allowlisted host is still refused
// with ErrRedirectBlocked. The cache key is the original inbound
// (scheme, canonical host, path); allowing the downgrade would let an
// on-path attacker on the plaintext hop poison entries that downstream
// apt clients believe came from a verified TLS upstream.
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
		ConnectTimeout:   2 * time.Second,
		TotalTimeout:     5 * time.Second,
		MaxRetries:       0,
		AllowedHostRegex: []string{`^127\.0\.0\.1$`},
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
			fmt.Fprintf(bw, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nETag: \"v1\"\r\n\r\n", len(full))
			bw.Write(full[:half])
			bw.Flush()
			conn.Close()
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
			fmt.Fprintf(bw, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nETag: \"v1\"\r\n\r\n", len(full))
			bw.Write(full[:half])
			bw.Flush()
			conn.Close()
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
			fmt.Fprintf(bw, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nETag: \"v1\"\r\n\r\n", len(full))
			bw.Write(full[:half])
			bw.Flush()
			conn.Close()
			return
		}
		// 206 announces 5 bytes (6-10) but delivers 2.
		w.Header().Set("Content-Range", "bytes 6-10/11")
		w.Header().Set("Content-Length", "2")
		w.WriteHeader(http.StatusPartialContent)
		hj, _ := w.(http.Hijacker)
		conn, bw, _ := hj.Hijack()
		bw.Write([]byte("xx"))
		bw.Flush()
		conn.Close()
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
