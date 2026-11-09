package fetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
)

// fetchOutcomes reads acu_fetch_total{outcome, host="127.0.0.1"} for each
// outcome named (0 for a series not yet created).
func fetchOutcomes(t *testing.T, outcomes ...string) map[string]float64 {
	t.Helper()
	var buf bytes.Buffer
	metrics.Default.Render(&buf)
	out := make(map[string]float64, len(outcomes))
	for _, o := range outcomes {
		prefix := `acu_fetch_total{outcome="` + o + `",host="127.0.0.1"} `
		for _, line := range strings.Split(buf.String(), "\n") {
			if v, ok := strings.CutPrefix(line, prefix); ok {
				f, err := strconv.ParseFloat(v, 64)
				if err != nil {
					t.Fatal(err)
				}
				out[o] = f
			}
		}
	}
	return out
}

// checkNoContent asserts that res reports its upstream 204 (the
// maintainer's ruling of 2026-11-05, F2-M3): NoContent set,
// UpstreamStatus 204, Status still the normalized 200, and the fetch
// counted as outcome no_content, not success.
func checkNoContent(t *testing.T, res *FetchResult, before map[string]float64) {
	t.Helper()
	if !res.NoContent || res.UpstreamStatus() != http.StatusNoContent || res.Status != http.StatusOK {
		t.Errorf("NoContent=%t, UpstreamStatus=%d, Status=%d; want true, 204, 200", res.NoContent, res.UpstreamStatus(), res.Status)
	}
	after := fetchOutcomes(t, "no_content", "success")
	if after["no_content"]-before["no_content"] != 1 || after["success"] != before["success"] {
		t.Errorf("acu_fetch_total no_content %v -> %v, success %v -> %v; want one no_content, no success",
			before["no_content"], after["no_content"], before["success"], after["success"])
	}
}

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
			before := fetchOutcomes(t, "no_content", "success")
			res, err := client.Fetch(context.Background(), &Target{
				CanonicalHost: "127.0.0.1", URL: srv.URL,
			}, dst)
			if err != nil {
				t.Fatal(err)
			}
			checkNoContent(t, res, before)
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
			before := fetchOutcomes(t, "no_content", "success")
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
			checkNoContent(t, res, before)
		})
	}
}

// Only a 204 sets NoContent: an empty 200, and a resumed download that a
// 206 completes, report the 200 they are and count as success.
func TestFetch_NoContentOnlyForA204(t *testing.T) {
	for _, tc := range []struct {
		name   string
		resume bool
	}{
		{name: "empty_200"},
		{name: "resumed_206", resume: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				n := calls.Add(1)
				if !tc.resume {
					w.WriteHeader(http.StatusOK)
					return
				}
				if n == 1 {
					conn, out, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					defer func() { _ = conn.Close() }()
					_, _ = fmt.Fprint(out, "HTTP/1.1 200 OK\r\nContent-Length: 11\r\nETag: \"v1\"\r\n\r\nhello ")
					_ = out.Flush()
					return
				}
				w.Header().Set("Content-Range", "bytes 6-10/11")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write([]byte("world"))
			}))
			defer srv.Close()
			dst := &bufDst{}
			client := newTestClient(t, []string{`^127\.0\.0\.1$`})
			before := fetchOutcomes(t, "no_content", "success")
			res, err := client.Fetch(context.Background(), &Target{CanonicalHost: "127.0.0.1", URL: srv.URL}, dst)
			if err != nil {
				t.Fatal(err)
			}
			if res.NoContent || res.UpstreamStatus() != http.StatusOK || res.Status != http.StatusOK {
				t.Errorf("NoContent=%t, UpstreamStatus=%d, Status=%d; want false, 200, 200", res.NoContent, res.UpstreamStatus(), res.Status)
			}
			if want := map[bool]string{false: "", true: "hello world"}[tc.resume]; dst.String() != want {
				t.Errorf("body %q, want %q", dst.String(), want)
			}
			after := fetchOutcomes(t, "no_content", "success")
			if after["success"]-before["success"] != 1 || after["no_content"] != before["no_content"] {
				t.Errorf("acu_fetch_total success %v -> %v, no_content %v -> %v; want one success",
					before["success"], after["success"], before["no_content"], after["no_content"])
			}
		})
	}
}
