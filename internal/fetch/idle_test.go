package fetch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newIdleClient builds a Client for httptest whose only short bound is
// upstream.idle_read_timeout: the total timeout is long, so a test that
// ends quickly ended at the idle bound.
func newIdleClient(t *testing.T, idle time.Duration, retries int) *Client {
	t.Helper()
	c, err := New(Options{
		ConnectTimeout:   2 * time.Second,
		TotalTimeout:     time.Minute,
		IdleReadTimeout:  idle,
		MaxRetries:       retries,
		AllowedHostRegex: []string{`^127\.0\.0\.1$`},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// stallServer serves h and gives the handlers a channel that closes when
// the test ends, so a handler that stalls on purpose lets the server close.
func stallServer(t *testing.T, h func(w http.ResponseWriter, r *http.Request, release <-chan struct{})) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h(w, r, release)
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})
	return srv
}

func fetchWithin(t *testing.T, c *Client, url string, limit time.Duration) (*bufDst, time.Duration, error) {
	t.Helper()
	dst := &bufDst{}
	start := time.Now()
	type res struct{ err error }
	done := make(chan res, 1)
	go func() {
		_, err := c.Fetch(context.Background(), &Target{CanonicalHost: "127.0.0.1", URL: url}, dst)
		done <- res{err}
	}()
	select {
	case r := <-done:
		return dst, time.Since(start), r.err
	case <-time.After(limit):
		t.Fatalf("Fetch still running after %s: the stall was not cut at the idle read timeout", limit)
		return nil, 0, nil
	}
}

// An upstream that sends part of a body and then nothing is abandoned
// after upstream.idle_read_timeout, not after total_timeout: a stalled
// member or package must not hold an adoption for five minutes.
func TestFetch_StalledBodyEndsAtTheIdleReadTimeout(t *testing.T) {
	srv := stallServer(t, func(w http.ResponseWriter, r *http.Request, release <-chan struct{}) {
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte("0123456789"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	c := newIdleClient(t, 150*time.Millisecond, 0)

	_, took, err := fetchWithin(t, c, srv.URL+"/x.deb", 10*time.Second)
	if !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("err = %v, want ErrIdleTimeout", err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v carries a context error: callers would read it as a shutdown or their own deadline", err)
	}
	if got := ClassifyFetchOutcome(err); got != "idle_timeout" {
		t.Errorf("outcome = %q, want idle_timeout", got)
	}
	if took < 100*time.Millisecond {
		t.Errorf("ended after %s, before the idle read timeout", took)
	}
}

// The wait for the response headers is bounded the same way.
func TestFetch_StalledHeadersEndAtTheIdleReadTimeout(t *testing.T) {
	srv := stallServer(t, func(w http.ResponseWriter, r *http.Request, release <-chan struct{}) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	c := newIdleClient(t, 150*time.Millisecond, 0)

	_, _, err := fetchWithin(t, c, srv.URL+"/x.deb", 10*time.Second)
	if !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("err = %v, want ErrIdleTimeout", err)
	}
}

// The bound is on silence, not on the transfer: a body that keeps
// arriving, slowly, for longer than the idle read timeout completes.
func TestFetch_SlowSteadyBodyIsNotIdle(t *testing.T) {
	const chunks = 6
	srv := stallServer(t, func(w http.ResponseWriter, r *http.Request, release <-chan struct{}) {
		w.Header().Set("Content-Length", fmt.Sprint(chunks*4))
		for i := 0; i < chunks; i++ {
			_, _ = w.Write([]byte("abcd"))
			w.(http.Flusher).Flush()
			time.Sleep(80 * time.Millisecond)
		}
	})
	c := newIdleClient(t, 300*time.Millisecond, 0)

	dst, took, err := fetchWithin(t, c, srv.URL+"/x.deb", 10*time.Second)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if dst.Written() != chunks*4 {
		t.Errorf("wrote %d bytes, want %d", dst.Written(), chunks*4)
	}
	if took < 300*time.Millisecond {
		t.Fatalf("the transfer took %s: it must outlast the idle read timeout for this test to mean anything", took)
	}
}

// An idle timeout is an upstream fault like a dropped connection: the
// fetch retries it and resumes where the stalled attempt stopped.
func TestFetch_IdleTimeoutIsRetriedAndResumes(t *testing.T) {
	const body = "0123456789abcdefghij"
	var calls atomic.Int32
	srv := stallServer(t, func(w http.ResponseWriter, r *http.Request, release <-chan struct{}) {
		w.Header().Set("ETag", `"v1"`)
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			_, _ = w.Write([]byte(body[:8]))
			w.(http.Flusher).Flush()
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		if r.Header.Get("Range") != "bytes=8-" {
			t.Errorf("retry Range = %q, want bytes=8-", r.Header.Get("Range"))
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 8-%d/%d", len(body)-1, len(body)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte(body[8:]))
	})
	c := newIdleClient(t, 150*time.Millisecond, 1)

	dst, _, err := fetchWithin(t, c, srv.URL+"/x.deb", 10*time.Second)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if dst.String() != body {
		t.Errorf("body = %q, want %q", dst.String(), body)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("upstream requests = %d, want 2", got)
	}
}

// A freshness probe holds its suite's lock while it runs, so its stall is
// cut the same way.
func TestConditional_StalledBodyEndsAtTheIdleReadTimeout(t *testing.T) {
	srv := stallServer(t, func(w http.ResponseWriter, r *http.Request, release <-chan struct{}) {
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte("Origin: x\n"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	c := newIdleClient(t, 150*time.Millisecond, 0)

	done := make(chan error, 1)
	go func() {
		_, err := c.Conditional(context.Background(), &Target{CanonicalHost: "127.0.0.1", URL: srv.URL + "/InRelease"}, "", "", 1<<20)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrIdleTimeout) {
			t.Fatalf("err = %v, want ErrIdleTimeout", err)
		}
		if got := ClassifyConditionalOutcome(nil, err); got != "idle_timeout" {
			t.Errorf("outcome = %q, want idle_timeout", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Conditional still running after 10s: the stall was not cut at the idle read timeout")
	}
}

// Zero keeps the old behaviour (no idle bound): only the total timeout
// ends a stall. Config never passes zero (it selects the default).
func TestFetch_ZeroIdleReadTimeoutIsNoBound(t *testing.T) {
	srv := stallServer(t, func(w http.ResponseWriter, r *http.Request, release <-chan struct{}) {
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte("0123456789"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	c, err := New(Options{
		ConnectTimeout:   2 * time.Second,
		TotalTimeout:     400 * time.Millisecond,
		AllowedHostRegex: []string{`^127\.0\.0\.1$`},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, _, ferr := fetchWithin(t, c, srv.URL+"/x.deb", 10*time.Second)
	if !errors.Is(ferr, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the total timeout's DeadlineExceeded", ferr)
	}
}

// A fetch whose upstream went silent is tried again once, on the
// expectation that the silence was that connection's. A second silence
// ends the fetch: the remaining retries would each wait out the idle
// timeout again for an upstream that is evidently not answering, and a
// stalled file held an adoption four idle timeouts instead of two (the
// freshness lab, 2026-11-15).
func TestFetch_ASecondIdleTimeoutEndsTheFetch(t *testing.T) {
	var calls atomic.Int32
	srv := stallServer(t, func(w http.ResponseWriter, r *http.Request, release <-chan struct{}) {
		calls.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	c := newIdleClient(t, 100*time.Millisecond, 3) // three retries allowed

	_, took, err := fetchWithin(t, c, srv.URL+"/x.deb", 10*time.Second)
	if !errors.Is(err, ErrIdleTimeout) || !errors.Is(err, ErrUpstreamUnavailable) {
		t.Fatalf("err = %v, want ErrUpstreamUnavailable wrapping ErrIdleTimeout", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("upstream requests = %d, want 2: the attempt and one retry", got)
	}
	if took > 2*time.Second {
		t.Errorf("the fetch took %s, want about two idle timeouts", took)
	}
}

// Other retryable failures keep all their retries, also after an idle
// timeout: only a second silence ends the fetch early.
func TestFetch_OtherFailuresKeepTheirRetriesAfterAnIdleTimeout(t *testing.T) {
	var calls atomic.Int32
	srv := stallServer(t, func(w http.ResponseWriter, r *http.Request, release <-chan struct{}) {
		switch calls.Add(1) {
		case 1: // silent
			select {
			case <-release:
			case <-r.Context().Done():
			}
		case 2, 3: // a server error, twice
			w.WriteHeader(http.StatusBadGateway)
		default:
			_, _ = w.Write([]byte("ok"))
		}
	})
	c := newIdleClient(t, 100*time.Millisecond, 3)

	dst, _, err := fetchWithin(t, c, srv.URL+"/x.deb", 10*time.Second)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if dst.String() != "ok" || calls.Load() != 4 {
		t.Errorf("body %q after %d requests, want ok after 4", dst.String(), calls.Load())
	}
}

// A caller that can go on without the file says so (Target.NoIdleRetry):
// the first silence ends its fetch, whatever max_retries allows. Hot
// prefetch and the optional members of an adoption use it — the release is
// published without the file, which is better than waiting out a second
// idle timeout for it.
func TestFetch_NoIdleRetryEndsAtTheFirstSilence(t *testing.T) {
	var calls atomic.Int32
	srv := stallServer(t, func(w http.ResponseWriter, r *http.Request, release <-chan struct{}) {
		calls.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	c := newIdleClient(t, 100*time.Millisecond, 3)

	start := time.Now()
	_, err := c.Fetch(context.Background(), &Target{CanonicalHost: "127.0.0.1", URL: srv.URL + "/x.deb", NoIdleRetry: true}, &bufDst{})
	if !errors.Is(err, ErrIdleTimeout) || !errors.Is(err, ErrUpstreamUnavailable) {
		t.Fatalf("err = %v, want ErrUpstreamUnavailable wrapping ErrIdleTimeout", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("upstream requests = %d, want 1: no retry after a silence", got)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("the fetch took %s, want about one idle timeout", took)
	}
	if got := ClassifyFetchOutcome(err); got != "idle_timeout" {
		t.Errorf("outcome = %q, want idle_timeout", got)
	}
}

// NoIdleRetry is about silence only: every other retryable failure keeps
// its retries.
func TestFetch_NoIdleRetryKeepsTheOtherRetries(t *testing.T) {
	var calls atomic.Int32
	srv := stallServer(t, func(w http.ResponseWriter, r *http.Request, release <-chan struct{}) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	c := newIdleClient(t, 100*time.Millisecond, 3)

	dst := &bufDst{}
	if _, err := c.Fetch(context.Background(), &Target{CanonicalHost: "127.0.0.1", URL: srv.URL + "/x.deb", NoIdleRetry: true}, dst); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if dst.String() != "ok" || calls.Load() != 3 {
		t.Errorf("body %q after %d requests, want ok after 3", dst.String(), calls.Load())
	}
}
