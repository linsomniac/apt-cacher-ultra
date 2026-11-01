package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestFetch_NoContentIsEmptySuccess(t *testing.T) {
	for _, initial := range []string{"", "stale bytes"} {
		t.Run(fmt.Sprintf("initial_bytes=%d", len(initial)), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("ETag", `"empty"`)
				w.Header().Set("Last-Modified", "Mon, 01 Jan 2024 00:00:00 GMT")
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()

			dst := &bufDst{}
			_, _ = dst.Write([]byte(initial))
			client := newTestClient(t, []string{`^127\.0\.0\.1$`})
			res, err := client.Fetch(context.Background(), &Target{
				CanonicalHost: "127.0.0.1", URL: srv.URL,
			}, dst)
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != http.StatusOK || res.ContentLength != 0 || dst.Written() != 0 || dst.String() != "" {
				t.Fatalf("result=%+v, written=%d, body=%q; want empty 200", res, dst.Written(), dst.String())
			}
			if res.ContentType != "application/octet-stream" || res.ETag != `"empty"` || res.LastModified != "Mon, 01 Jan 2024 00:00:00 GMT" {
				t.Errorf("response headers lost: %+v", res)
			}
			if got := calls.Load(); got != 1 {
				t.Errorf("requests=%d, want 1", got)
			}
		})
	}
}

type noContentDst struct {
	bufDst
	truncateErr error
}

func (d *noContentDst) Truncate() error {
	if d.truncateErr != nil {
		return d.truncateErr
	}
	return d.bufDst.Truncate()
}

func TestFetch_EmptyResponseAfterPartialDownload(t *testing.T) {
	for _, tc := range []struct {
		name, validatorHeader, validator string
		status                           int
		truncateErr                      error
	}{
		{name: "etag", status: http.StatusNoContent, validatorHeader: "ETag", validator: `"v1"`},
		{name: "last_modified", status: http.StatusNoContent, validatorHeader: "Last-Modified", validator: "Mon, 01 Jan 2024 00:00:00 GMT"},
		{name: "no_validator", status: http.StatusNoContent},
		// UnexpectedEOF is normally retryable; a failed destination reset
		// must instead stop with ErrCacheWriteFailed on every restart path.
		{name: "204_truncate_failure", status: http.StatusNoContent, validatorHeader: "ETag", validator: `"v1"`, truncateErr: io.ErrUnexpectedEOF},
		{name: "200_truncate_failure", status: http.StatusOK, validatorHeader: "ETag", validator: `"v1"`, truncateErr: io.ErrUnexpectedEOF},
		{name: "no_validator_truncate_failure", status: http.StatusNoContent, truncateErr: io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					conn, out, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					defer func() { _ = conn.Close() }()
					_, _ = fmt.Fprint(out, "HTTP/1.1 200 OK\r\nContent-Length: 11\r\n")
					if tc.validatorHeader != "" {
						_, _ = fmt.Fprintf(out, "%s: %s\r\n", tc.validatorHeader, tc.validator)
					}
					_, _ = fmt.Fprint(out, "\r\nhello ")
					_ = out.Flush()
					return
				}
				wantRange := ""
				if tc.validatorHeader != "" {
					wantRange = "bytes=6-"
				}
				if r.Header.Get("Range") != wantRange || r.Header.Get("If-Range") != tc.validator {
					t.Errorf("retry headers: Range=%q, If-Range=%q", r.Header.Get("Range"), r.Header.Get("If-Range"))
				}
				w.Header().Set("ETag", `"empty"`)
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			dst := &noContentDst{truncateErr: tc.truncateErr}
			client := newTestClient(t, []string{`^127\.0\.0\.1$`})
			if tc.truncateErr == nil {
				// A restart without a validator must not spend the only
				// retry on truncation before issuing the second request.
				client.maxRetries = 1
			}
			res, err := client.Fetch(context.Background(), &Target{CanonicalHost: "127.0.0.1", URL: srv.URL}, dst)
			wantCalls := int32(2)
			if tc.truncateErr != nil && tc.validator == "" {
				wantCalls = 1 // reset fails before another request can be sent
			}
			if got := calls.Load(); got != wantCalls {
				t.Errorf("requests=%d, want %d", got, wantCalls)
			}
			if tc.truncateErr != nil {
				if !errors.Is(err, ErrCacheWriteFailed) || res != nil {
					t.Fatalf("result=%+v, error=%v; want cache write failure", res, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != http.StatusOK || res.ContentLength != 0 || dst.Written() != 0 || dst.String() != "" {
				t.Fatalf("result=%+v, written=%d, body=%q; want empty 200", res, dst.Written(), dst.String())
			}
			if res.ETag != `"empty"` || res.LastModified != "" {
				t.Errorf("stale validators after restart: %+v", res)
			}
		})
	}
}
