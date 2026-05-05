// Package fetch is the upstream HTTP client. It enforces SPEC §6.6
// hardening (host allowlist + post-resolution deny CIDRs) before any
// connection, and implements SPEC §6.3 resumable Range retries with
// If-Range validators for transient upstream failures.
//
// The package is decoupled from cache.BlobWriter: callers hand Fetch any
// FetchDst implementation (an io.Writer with Written/Truncate hooks).
// That keeps the fetcher unit-testable in isolation and lets a future
// freshness-checker plug in a /dev/null-style destination for HEAD-like
// validation requests.
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Sentinel errors. Callers identify retryable-vs-fatal failures with
// errors.Is.
var (
	ErrHostNotAllowed      = errors.New("fetch: upstream host not allowed by allowlist")
	ErrTargetDenied        = errors.New("fetch: resolved IP is in deny range")
	ErrUpstreamUnavailable = errors.New("fetch: upstream unavailable after retries")
	ErrUpstreamStatus      = errors.New("fetch: upstream returned non-success status")
	ErrUpstreamServerError = errors.New("fetch: upstream returned 5xx")
	ErrSizeMismatch        = errors.New("fetch: response body length disagrees with declared length")
	ErrInvalidContentRange = errors.New("fetch: upstream Content-Range header invalid or unexpected")
	ErrTotalSizeMismatch   = errors.New("fetch: 206 total size differs from initial 200 length")
)

const defaultUserAgent = "apt-cacher-ultra/0.1"

// Options carries the configurable knobs read from config.UpstreamConfig.
// Fetch expects these to be already-defaulted by config.Defaults() — zero
// timeouts mean "no timeout" at the transport level and rely on
// TotalTimeout (set by Defaults) for the overall budget.
type Options struct {
	ConnectTimeout   time.Duration
	TotalTimeout     time.Duration
	IdleReadTimeout  time.Duration // currently informational; per-byte read timeouts are a Phase 2 candidate.
	MaxRetries       int
	AllowedHostRegex []string
	DenyTargetRanges []string
	UserAgent        string

	// dialContext, when non-nil, replaces the deny-CIDR-protected dialer.
	// Test seam: httptest binds 127.0.0.1, which is in the default deny
	// list, so tests pass an empty list (or set this) to bypass.
	dialContext func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Client is a configured upstream fetcher: one *http.Client, a compiled
// allowlist, and a deny-CIDR list applied at dial time.
type Client struct {
	httpClient   *http.Client
	allow        []*regexp.Regexp
	maxRetries   int
	totalTimeout time.Duration
	userAgent    string
}

// Target identifies what to fetch. CanonicalHost is the cache-key host
// (the value the allowlist regex matches against). URL is the literal
// upstream URL the client GETs — typically scheme://host[:port]/path
// where host has been carried through Remap canonicalization but the
// authority may carry a port the cache key doesn't (proxy.Request's
// UpstreamURL does this split).
type Target struct {
	CanonicalHost string
	URL           string
}

// FetchDst is the destination for the response body. Truncate must reset
// the destination to its zero state — used when a resume retry returns
// 200 (validator changed) instead of 206, or when no validator was ever
// captured and a retry can't safely resume.
type FetchDst interface {
	io.Writer
	Written() int64
	Truncate() error
}

// FetchResult is the response metadata from a successful fetch. The body
// itself has already been streamed into the caller-supplied FetchDst.
//
// Status is always 200 on success — the internal resume mechanics (a
// 206 partial response or a validator-induced restart) are hidden from
// the caller. ContentLength is the *full* object size; it equals
// Written on the FetchDst at success.
type FetchResult struct {
	Status        int
	ContentType   string
	ContentLength int64
	ETag          string
	LastModified  string
}

// New constructs a Client from validated upstream options.
func New(opts Options) (*Client, error) {
	if opts.MaxRetries < 0 {
		return nil, fmt.Errorf("fetch: max_retries must be >= 0, got %d", opts.MaxRetries)
	}
	if opts.TotalTimeout < 0 {
		return nil, fmt.Errorf("fetch: total_timeout must be >= 0, got %v", opts.TotalTimeout)
	}
	if opts.ConnectTimeout < 0 {
		return nil, fmt.Errorf("fetch: connect_timeout must be >= 0, got %v", opts.ConnectTimeout)
	}
	allow, err := compileAllow(opts.AllowedHostRegex)
	if err != nil {
		return nil, err
	}
	deny, err := parseDenyCIDRs(opts.DenyTargetRanges)
	if err != nil {
		return nil, err
	}
	transport := newTransport(opts, deny)
	ua := opts.UserAgent
	if ua == "" {
		ua = defaultUserAgent
	}
	return &Client{
		httpClient: &http.Client{
			Transport: transport,
			// AIDEV-NOTE: no http.Client.Timeout. Per-fetch budget comes
			// from the ctx we wrap with TotalTimeout in Fetch. Setting
			// Client.Timeout in addition would fire even after our own
			// ctx cancel, double-cancelling for no benefit.
		},
		allow:        allow,
		maxRetries:   opts.MaxRetries,
		totalTimeout: opts.TotalTimeout,
		userAgent:    ua,
	}, nil
}

// Fetch streams target.URL into dst, applying SPEC §6.6 hardening (host
// allowlist + deny-CIDR check) and SPEC §6.3 resumable retries.
//
// Errors are classified for callers via errors.Is:
//
//   - ErrHostNotAllowed, ErrTargetDenied: hardening rejected the fetch
//     before any network I/O. Caller should surface 403.
//   - ErrUpstreamStatus: 4xx from upstream. Not retryable.
//   - ErrUpstreamUnavailable: ran out of retries on transient failures.
//     Caller may serve stale (metadata) or 502 (blob).
//   - context.Canceled / context.DeadlineExceeded: the ctx fired.
func (c *Client) Fetch(ctx context.Context, target *Target, dst FetchDst) (*FetchResult, error) {
	if target == nil {
		return nil, errors.New("fetch: nil target")
	}
	if dst == nil {
		return nil, errors.New("fetch: nil dst")
	}
	if err := c.checkAllowed(target.CanonicalHost); err != nil {
		return nil, err
	}
	if c.totalTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.totalTimeout)
		defer cancel()
	}

	var (
		haveInitial bool
		initial     *FetchResult
		attempts    int
		lastErr     error
	)

	for {
		if attempts > c.maxRetries {
			if lastErr == nil {
				lastErr = errors.New("retries exhausted with no error captured")
			}
			return nil, fmt.Errorf("%w: %v", ErrUpstreamUnavailable, lastErr)
		}

		sendRange := haveInitial && dst.Written() > 0
		var validator string
		if sendRange {
			validator = initial.ETag
			if validator == "" {
				validator = initial.LastModified
			}
			if validator == "" {
				// No validator captured — can't safely resume per SPEC
				// §6.3. Truncate and start over.
				if terr := dst.Truncate(); terr != nil {
					return nil, fmt.Errorf("fetch: truncate (no validator): %w", terr)
				}
				haveInitial = false
				initial = nil
				attempts++
				continue
			}
		}
		var expectedTotal int64
		if haveInitial {
			expectedTotal = initial.ContentLength
		}

		out, err := c.doAttempt(ctx, target, dst, sendRange, dst.Written(), validator, expectedTotal)

		// Capture (or refresh) initial when this attempt produced fresh
		// 200 headers — the first 200, or a validator-induced restart's
		// new 200. doAttempt has already truncated dst in the restart
		// case, so this assignment never trails stale Written bytes.
		if out != nil && out.Status == http.StatusOK {
			initial = out
			haveInitial = true
		}

		if err == nil {
			return &FetchResult{
				Status:        http.StatusOK,
				ContentType:   initial.ContentType,
				ContentLength: initial.ContentLength,
				ETag:          initial.ETag,
				LastModified:  initial.LastModified,
			}, nil
		}
		if !isRetryable(err) {
			return nil, err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		lastErr = err
		attempts++
	}
}

// doAttempt issues one HTTP GET, optionally with Range/If-Range, and
// streams the response body into dst. Returns a partially-filled
// *FetchResult plus an error when the response headers arrived but body
// streaming failed; the outer loop uses this to capture validators for
// the next retry.
func (c *Client) doAttempt(
	ctx context.Context,
	target *Target,
	dst FetchDst,
	sendRange bool,
	rangeStart int64,
	validator string,
	expectedTotal int64,
) (*FetchResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch: new request: %w", err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	if sendRange {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(rangeStart, 10)+"-")
		req.Header.Set("If-Range", validator)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if errors.Is(err, errDialDenied) {
			return nil, fmt.Errorf("%w: %v", ErrTargetDenied, err)
		}
		return nil, err
	}
	defer resp.Body.Close()

	out := &FetchResult{
		Status:        resp.StatusCode,
		ContentType:   resp.Header.Get("Content-Type"),
		ContentLength: resp.ContentLength,
		ETag:          resp.Header.Get("ETag"),
		LastModified:  resp.Header.Get("Last-Modified"),
	}

	switch resp.StatusCode {
	case http.StatusOK:
		if sendRange {
			// Server invalidated our resume (If-Range mismatch or just
			// ignored the Range header). SPEC §6.3: "the partial file is
			// discarded and the fetch restarts from byte 0".
			if err := dst.Truncate(); err != nil {
				return out, fmt.Errorf("fetch: truncate dst on restart: %w", err)
			}
		}
		if err := streamBody(resp, dst); err != nil {
			return out, err
		}
		if resp.ContentLength > 0 && dst.Written() != resp.ContentLength {
			return out, fmt.Errorf("%w: wrote %d, declared %d",
				ErrSizeMismatch, dst.Written(), resp.ContentLength)
		}
		return out, nil
	case http.StatusPartialContent:
		if !sendRange {
			return out, fmt.Errorf("%w: 206 to non-Range request", ErrInvalidContentRange)
		}
		first, _, total, err := parseContentRange(resp.Header.Get("Content-Range"))
		if err != nil {
			return out, err
		}
		if first != rangeStart {
			return out, fmt.Errorf("%w: 206 first=%d, expected %d",
				ErrInvalidContentRange, first, rangeStart)
		}
		if expectedTotal > 0 && total > 0 && total != expectedTotal {
			return out, fmt.Errorf("%w: 206 total=%d, expected %d",
				ErrTotalSizeMismatch, total, expectedTotal)
		}
		out.ContentLength = total
		if err := streamBody(resp, dst); err != nil {
			return out, err
		}
		if total > 0 && dst.Written() != total {
			return out, fmt.Errorf("%w: wrote %d, declared total %d",
				ErrSizeMismatch, dst.Written(), total)
		}
		return out, nil
	default:
		if resp.StatusCode >= 500 && resp.StatusCode < 600 {
			return out, fmt.Errorf("%w: status=%d", ErrUpstreamServerError, resp.StatusCode)
		}
		return out, fmt.Errorf("%w: status=%d", ErrUpstreamStatus, resp.StatusCode)
	}
}

// streamBody copies resp.Body into dst.
func streamBody(resp *http.Response, dst io.Writer) error {
	_, err := io.Copy(dst, resp.Body)
	return err
}

// parseContentRange parses "bytes <first>-<last>/<total>" or
// "bytes <first>-<last>/*" (returning total = -1 in the latter case).
//
// AIDEV-NOTE: we don't use net/http's internal parser because it's not
// exported. The format is narrow enough to handle inline.
func parseContentRange(s string) (int64, int64, int64, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "bytes ") {
		return 0, 0, 0, fmt.Errorf("%w: %q", ErrInvalidContentRange, s)
	}
	rest := strings.TrimPrefix(s, "bytes ")
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return 0, 0, 0, fmt.Errorf("%w: %q", ErrInvalidContentRange, s)
	}
	span := rest[:slash]
	totalStr := rest[slash+1:]
	dash := strings.Index(span, "-")
	if dash < 0 {
		return 0, 0, 0, fmt.Errorf("%w: %q", ErrInvalidContentRange, s)
	}
	first, err := strconv.ParseInt(span[:dash], 10, 64)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("%w: %q", ErrInvalidContentRange, s)
	}
	last, err := strconv.ParseInt(span[dash+1:], 10, 64)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("%w: %q", ErrInvalidContentRange, s)
	}
	var total int64
	if totalStr == "*" {
		total = -1
	} else {
		total, err = strconv.ParseInt(totalStr, 10, 64)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("%w: %q", ErrInvalidContentRange, s)
		}
	}
	if last < first {
		return 0, 0, 0, fmt.Errorf("%w: last < first in %q", ErrInvalidContentRange, s)
	}
	return first, last, total, nil
}

// isRetryable reports whether err merits another attempt. Allowlist,
// deny-range, ctx-cancel, 4xx are not retryable. 5xx, header anomalies,
// size mismatches, and generic IO/net errors are.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, ErrHostNotAllowed) || errors.Is(err, ErrTargetDenied) {
		return false
	}
	if errors.Is(err, ErrUpstreamStatus) {
		return false
	}
	return true
}
