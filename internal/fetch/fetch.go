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
	"log/slog"
	"net"
	"net/http"
	"net/url"
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
	ErrInvalidURL          = errors.New("fetch: invalid target URL")
	ErrRedirectBlocked     = errors.New("fetch: upstream redirect blocked")
	ErrCacheWriteFailed    = errors.New("fetch: cache write failed")
)

// StatusError carries the upstream HTTP status code from any non-success
// response. The handler layer needs the actual status so it can
// passthrough an upstream 404 (apt frequently probes for index variants
// that don't exist), AND so the SPEC §10 upstream_status log field
// carries the real upstream code on 502-after-retries paths.
//
// errors.Is dispatches by code range:
//   - 4xx: matches ErrUpstreamStatus (passthrough path).
//   - 5xx: matches ErrUpstreamServerError (treat as upstream-down).
//   - 3xx (only seen wrapped with ErrRedirectBlocked): matches neither
//     status sentinel; the redirect sentinel routes the dispatch.
//
// "Any HTTP status carrier" lookups use errors.As(*StatusError) — that
// recovers the code regardless of which range it falls in (e.g. the
// handler's runFetch needs the status for X-Upstream-Status whether the
// upstream returned 5xx or a blocked-3xx).
type StatusError struct {
	Code int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("fetch: upstream status=%d", e.Code)
}

func (e *StatusError) Is(target error) bool {
	switch target {
	case ErrUpstreamStatus:
		return e.Code >= 400 && e.Code < 500
	case ErrUpstreamServerError:
		return e.Code >= 500 && e.Code < 600
	}
	return false
}

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

	// UnreachableCooldown gates the per-host fast-fail mechanism. When
	// > 0, a host whose dial just failed has subsequent dials within
	// this window collapsed to a single probe attempt of
	// UnreachableProbeTimeout, with retries suppressed on probe failure.
	// Zero disables the feature (legacy behavior — full ConnectTimeout
	// × MaxRetries budget on every miss). SPEC §1 "never hang."
	UnreachableCooldown     time.Duration
	UnreachableProbeTimeout time.Duration

	// Logger sinks the per-retry "fetch retry" Info line (SPEC §10).
	// nil falls back to slog.Default() — production main.run sets the
	// default before fetch.New is called.
	Logger *slog.Logger

	// dialContext, when non-nil, replaces the deny-CIDR-protected dialer.
	// Test seam: httptest binds 127.0.0.1, which is in the default deny
	// list, so tests pass an empty list (or set this) to bypass.
	dialContext func(ctx context.Context, network, addr string) (net.Conn, error)

	// now, when non-nil, replaces time.Now in the unreachableTracker.
	// Test seam for deterministic cooldown-window assertions.
	now func() time.Time
}

// Client is a configured upstream fetcher: one *http.Client, a compiled
// allowlist, and a deny-CIDR list applied at dial time.
type Client struct {
	httpClient   *http.Client
	allow        []*regexp.Regexp
	maxRetries   int
	totalTimeout time.Duration
	userAgent    string
	logger       *slog.Logger
	unreachable  *unreachableTracker // nil when UnreachableCooldown <= 0
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
	if opts.UnreachableCooldown < 0 {
		return nil, fmt.Errorf("fetch: unreachable_cooldown must be >= 0, got %v", opts.UnreachableCooldown)
	}
	if opts.UnreachableProbeTimeout < 0 {
		return nil, fmt.Errorf("fetch: unreachable_probe_timeout must be >= 0, got %v", opts.UnreachableProbeTimeout)
	}
	allow, err := compileAllow(opts.AllowedHostRegex)
	if err != nil {
		return nil, err
	}
	deny, err := parseDenyCIDRs(opts.DenyTargetRanges)
	if err != nil {
		return nil, err
	}
	tracker := newUnreachableTracker(opts.UnreachableCooldown, opts.UnreachableProbeTimeout, opts.now)
	transport := newTransport(opts, deny, tracker)
	ua := opts.UserAgent
	if ua == "" {
		ua = defaultUserAgent
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		httpClient: &http.Client{
			Transport: transport,
			// AIDEV-NOTE: no http.Client.Timeout. Per-fetch budget comes
			// from the ctx we wrap with TotalTimeout in Fetch. Setting
			// Client.Timeout in addition would fire even after our own
			// ctx cancel, double-cancelling for no benefit.
			//
			// Reject *all* upstream redirects in Phase 1. Without this
			// hook the http.Client would follow 3xx silently, which
			// bypasses the allowlist (the post-redirect host is never
			// regex-checked) and breaks the cache-key contract (the URL
			// the cache stores is the request URL, not the redirect
			// target). Operators whose archive uses redirects should
			// configure a Remap rule pointing at the redirect target.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) == 0 {
					return fmt.Errorf("%w: redirected to %s", ErrRedirectBlocked, req.URL)
				}
				return fmt.Errorf("%w: %s -> %s", ErrRedirectBlocked, via[len(via)-1].URL, req.URL)
			},
		},
		allow:        allow,
		maxRetries:   opts.MaxRetries,
		totalTimeout: opts.TotalTimeout,
		userAgent:    ua,
		logger:       logger,
		unreachable:  tracker,
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
	// Defense-in-depth: parse target.URL and require URL.Hostname() to
	// match target.CanonicalHost. The proxy package always keeps these
	// aligned, but the fetch package is security-sensitive and should
	// not rely on a caller invariant for SSRF posture.
	u, err := url.Parse(target.URL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("%w: scheme %q (only http/https supported)", ErrInvalidURL, scheme)
	}
	urlHost := normalizeHost(u.Hostname())
	if urlHost == "" {
		return nil, fmt.Errorf("%w: empty host in %q", ErrInvalidURL, target.URL)
	}
	canonHost := normalizeHost(target.CanonicalHost)
	if urlHost != canonHost {
		return nil, fmt.Errorf("%w: URL host %q != canonical host %q",
			ErrInvalidURL, urlHost, canonHost)
	}
	if err := c.checkAllowed(canonHost); err != nil {
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
			// Multi-%w preserves lastErr's chain (in particular a
			// *StatusError carrying the upstream 5xx code) so the
			// handler's errors.As lookup in runFetch still surfaces the
			// real upstream status for the X-Upstream-Status header and
			// the SPEC §10 upstream_status log field. Single-%w-with-%v
			// would drop the StatusError on retry exhaustion.
			return nil, fmt.Errorf("%w: %w", ErrUpstreamUnavailable, lastErr)
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
		// SPEC §10: emit one structured log per retry attempt so an
		// operator watching upstream flap can quantify it.
		c.logger.Info("fetch retry",
			"attempt", attempts+1,
			"max_retries", c.maxRetries,
			"canonical_host", target.CanonicalHost,
			"err", err,
		)
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
		// On a CheckRedirect rejection, http.Client returns both the
		// 3xx response and the wrapped error (with the body already
		// closed). Preserve the upstream status code as a StatusError
		// in the chain so SPEC §10 upstream_status reflects the real
		// 3xx the upstream emitted instead of 0. The narrowed
		// StatusError.Is keeps 3xx out of the ErrUpstreamStatus /
		// ErrUpstreamServerError dispatch — handler routing keys off
		// ErrRedirectBlocked, status carrier is errors.As-only.
		if errors.Is(err, ErrRedirectBlocked) && resp != nil {
			return nil, fmt.Errorf("%w: %w", err, &StatusError{Code: resp.StatusCode})
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
		first, last, total, err := parseContentRange(resp.Header.Get("Content-Range"))
		if err != nil {
			return out, err
		}
		if first != rangeStart {
			return out, fmt.Errorf("%w: 206 first=%d, expected %d",
				ErrInvalidContentRange, first, rangeStart)
		}
		// SPEC §6.3 requires the 206 total to match the initial 200's
		// Content-Length. A "*" total (unknown) cannot satisfy that, so
		// reject it whenever we have something to compare against.
		if expectedTotal > 0 {
			if total < 0 {
				return out, fmt.Errorf("%w: 206 total=* but expected %d",
					ErrInvalidContentRange, expectedTotal)
			}
			if total != expectedTotal {
				return out, fmt.Errorf("%w: 206 total=%d, expected %d",
					ErrTotalSizeMismatch, total, expectedTotal)
			}
		}
		if total > 0 && last >= total {
			return out, fmt.Errorf("%w: 206 last=%d >= total=%d",
				ErrInvalidContentRange, last, total)
		}
		out.ContentLength = total
		if err := streamBody(resp, dst); err != nil {
			return out, err
		}
		// The response covered [first, last] inclusive. Combined with
		// dst's pre-attempt cursor of `first`, dst.Written() must be
		// exactly last+1 — otherwise the body was short or long for
		// the declared range.
		if dst.Written() != last+1 {
			return out, fmt.Errorf("%w: 206 wrote up to %d, declared last=%d",
				ErrSizeMismatch, dst.Written()-1, last)
		}
		if total > 0 && dst.Written() != total {
			return out, fmt.Errorf("%w: wrote %d, declared total %d",
				ErrSizeMismatch, dst.Written(), total)
		}
		return out, nil
	default:
		// Both 4xx and 5xx return a *StatusError so the upstream code
		// is recoverable via errors.As anywhere up the chain (handler
		// runFetch needs this for X-Upstream-Status and SPEC §10
		// upstream_status). The Is method satisfies the
		// ErrUpstreamServerError sentinel for 5xx, which the handler
		// uses to route into respondUpstreamUnreachable.
		return out, &StatusError{Code: resp.StatusCode}
	}
}

// streamBody copies resp.Body into dst, tagging dst-side write failures
// with ErrCacheWriteFailed so callers can distinguish a cache-side fault
// (e.g. disk full) from an upstream-side fault. Without this distinction
// io.Copy returns the first error from either side; the outer retry loop
// would then re-attempt on a cache write failure that won't recover by
// re-asking the upstream.
func streamBody(resp *http.Response, dst io.Writer) error {
	_, err := io.Copy(&writeErrTagger{w: dst}, resp.Body)
	return err
}

// writeErrTagger wraps an io.Writer so any Write error is wrapped with
// ErrCacheWriteFailed. Read-side errors from io.Copy still bubble up
// unwrapped, so the outer Fetch loop retries those (network blips) and
// stops on cache write errors (no point re-fetching when our disk is full).
type writeErrTagger struct {
	w io.Writer
}

func (t *writeErrTagger) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if err != nil {
		return n, fmt.Errorf("%w: %v", ErrCacheWriteFailed, err)
	}
	return n, nil
}

// normalizeHost canonicalizes a hostname for equality comparison: lowercase
// + strip trailing FQDN dot + strip IPv6 brackets. The result is suitable
// for comparing url.URL.Hostname() (already bracket-stripped) with the
// canonical host carried in Target (which may keep brackets after passing
// through proxy.canonicalize for IPv6 literals).
func normalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	if len(h) >= 2 && h[0] == '[' && h[len(h)-1] == ']' {
		h = h[1 : len(h)-1]
	}
	return h
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
	// HTTP-status carriers: 4xx is not retryable (apt-style probing of
	// non-existent index variants must surface to the caller); 5xx is
	// retryable (transient upstream error). Production code emits
	// *StatusError values for both; check that path first so we recover
	// the code via errors.As. Fall through to the bare-sentinel check
	// for any error wrapping ErrUpstreamStatus without a StatusError in
	// its chain (e.g. external callers, fmt.Errorf-style wraps).
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code >= 500 && se.Code < 600
	}
	if errors.Is(err, ErrUpstreamStatus) {
		return false
	}
	if errors.Is(err, ErrRedirectBlocked) || errors.Is(err, ErrInvalidURL) {
		return false
	}
	if errors.Is(err, ErrHostUnreachable) {
		// Probe-attempt failure under an active cooldown. Retrying buys
		// nothing — we just consumed our short probe budget; the outer
		// loop should fail fast (SPEC §1 "never hang"). Without this,
		// the connect_timeout × max_retries hang returns whenever the
		// probe path also times out, defeating the cooldown's purpose.
		return false
	}
	if errors.Is(err, ErrCacheWriteFailed) {
		// SPEC §11 row 14: a cache-side write failure (disk full, I/O
		// error) won't get better by re-asking upstream. Surface it
		// fast so the handler can log loudly and 502.
		return false
	}
	return true
}
