package fetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

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
