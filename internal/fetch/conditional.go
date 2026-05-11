package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrConditionalBodyTooLarge is returned when a 200 response body exceeds
// the per-call ceiling. The freshness checker bounds memory because the
// body lives entirely in RAM (we hash it and compare to the cached blob).
var ErrConditionalBodyTooLarge = errors.New("fetch: conditional GET body exceeds limit")

// ConditionalResult is the outcome of a successful Conditional call.
//
// Status is 304 (no change) or 200 (changed; Body is filled). Other
// statuses surface as errors instead — see Conditional.
type ConditionalResult struct {
	Status       int
	ContentType  string
	ETag         string
	LastModified string
	Body         []byte // populated only when Status == 200
}

// Conditional issues a single GET to target.URL with If-None-Match and/or
// If-Modified-Since headers, applying the same SPEC §6.6 hardening as
// Fetch (allowlist + deny-CIDR + redirect policy: a 3xx to an allowlisted
// host is followed; one to an un-allowlisted host, an HTTPS→HTTP scheme
// downgrade, or a non-http(s) scheme is refused with ErrRedirectBlocked.
// See fetch.go's CheckRedirect for the full policy — the same *http.Client
// is shared, so the policy is uniform with Fetch).
//
// Unlike Fetch, no retries: SPEC §7.2 says "log on failure, bump
// last_check_at, move on" — the freshness layer handles the retry
// cadence via the periodic scheduler.
//
// Successful returns:
//   - *ConditionalResult with Status = 304 and a nil Body.
//   - *ConditionalResult with Status = 200 and Body filled (up to maxBody
//     bytes). ErrConditionalBodyTooLarge if the body exceeds maxBody.
//
// Error returns (errors.Is classification matches Fetch):
//   - ErrInvalidURL, ErrHostNotAllowed, ErrTargetDenied, ErrRedirectBlocked
//   - ErrUpstreamStatus (with *StatusError) for 4xx
//   - ErrUpstreamServerError for 5xx
//   - context.Canceled / context.DeadlineExceeded
//   - generic transport errors
func (c *Client) Conditional(
	ctx context.Context,
	target *Target,
	etag, lastmod string,
	maxBody int64,
) (result *ConditionalResult, retErr error) {
	// SPEC5 §10.4.2: every Conditional terminal return drives one
	// fetch-counter increment and one duration observation, with
	// outcome derived from the classifier (cond_changed /
	// cond_unchanged on success, sentinel match on failure).
	start := time.Now()
	host := hostLabel(target)
	defer func() {
		outcome := ClassifyConditionalOutcome(result, retErr)
		fetchTotal.Inc(outcome, host)
		fetchDurationSeconds.Observe(time.Since(start).Seconds(), outcome, host)
	}()

	if target == nil {
		return nil, errors.New("fetch: nil target")
	}
	if maxBody <= 0 {
		return nil, errors.New("fetch: maxBody must be > 0")
	}

	// SSRF posture: same checks as Fetch — never trust target.URL alone.
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

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch: new request: %w", err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if lastmod != "" {
		req.Header.Set("If-Modified-Since", lastmod)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if errors.Is(err, errDialDenied) {
			return nil, fmt.Errorf("%w: %v", ErrTargetDenied, err)
		}
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	out := &ConditionalResult{
		Status:       resp.StatusCode,
		ContentType:  resp.Header.Get("Content-Type"),
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
	}

	switch {
	case resp.StatusCode == http.StatusNotModified:
		// 304 keeps prior validators by RFC; expose whatever upstream
		// echoed but do not require them.
		return out, nil
	case resp.StatusCode == http.StatusOK:
		// LimitReader caps at maxBody bytes. If upstream sent more, we
		// surface ErrConditionalBodyTooLarge so the freshness layer
		// can log and back off — silently truncating would yield a
		// false hash mismatch on next compare.
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		if err != nil {
			return nil, fmt.Errorf("fetch: read conditional body: %w", err)
		}
		if int64(len(body)) > maxBody {
			return nil, fmt.Errorf("%w: >%d bytes", ErrConditionalBodyTooLarge, maxBody)
		}
		out.Body = body
		return out, nil
	case resp.StatusCode >= 500 && resp.StatusCode < 600:
		return nil, fmt.Errorf("%w: status=%d", ErrUpstreamServerError, resp.StatusCode)
	default:
		// Includes 3xx (CheckRedirect already rejected them upstream of
		// this code path on follow, but a non-redirected 3xx body could
		// in principle arrive — treat as a status anomaly), 4xx (not
		// retryable per fetch policy), and any other non-success.
		return nil, &StatusError{Code: resp.StatusCode}
	}
}
