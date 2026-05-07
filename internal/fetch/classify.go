package fetch

import (
	"context"
	"errors"
	"net"
)

// ClassifyFetchOutcome maps a fetch.Fetch() return to a single
// outcome label. err is the second return value from Fetch; nil →
// "success". The classifier is a TOTAL function — every fetch
// terminal return yields exactly one label.
//
// Precedence (first match wins) is specific-cause-first. The exact
// ordering is load-bearing because the dialer wraps a cooldown-probe
// failure with BOTH ErrUpstreamUnavailable and ErrHostUnreachable
// (`%w: %w: ...` at internal/fetch/dialer.go:163); checking
// ErrUpstreamUnavailable first would mask the more diagnostic
// `host_unreachable` label. SPEC5 §10.4.2 has the full precedence
// table.
//
// AIDEV-NOTE: SPEC5 §12.1.6 mandates that this function be tested
// against every row of the §10.4.2 precedence table. New sentinel
// errors added to fetch must be classified here AND in §10.4.2's
// table; a new errors.Is row without an outcome assignment falls
// through to "error".
func ClassifyFetchOutcome(err error) string {
	if err == nil {
		return "success"
	}
	return classifyError(err, false)
}

// ClassifyConditionalOutcome maps a fetch.Conditional() return to a
// single outcome label. The success path branches on res.Status: 200
// → "cond_changed", 304 → "cond_unchanged". A nil err with any other
// status is a programming error and falls through to "error".
func ClassifyConditionalOutcome(res *ConditionalResult, err error) string {
	if err == nil {
		if res == nil {
			return "error"
		}
		switch res.Status {
		case 200:
			return "cond_changed"
		case 304:
			return "cond_unchanged"
		default:
			return "error"
		}
	}
	return classifyError(err, true)
}

// classifyError walks the error chain in the SPEC5 §10.4.2
// precedence order and returns the first matching outcome label.
// `conditional` controls whether ErrConditionalBodyTooLarge is
// considered (it is only producible by Conditional()); leaving it
// out for ClassifyFetchOutcome is conservative — Fetch could in
// principle wrap a Conditional error in a future refactor, and we'd
// rather classify it specifically than fall to the catch-all.
//
// The 23 rows below mirror SPEC5 §10.4.2 precedence in execution
// order. Specific sentinels first, status-code carriers next, broad
// sentinels third, net dial errors last, catch-all final.
func classifyError(err error, conditional bool) string {
	// Rows 4-12: explicit sentinels in specific-cause-first order.
	switch {
	case errors.Is(err, ErrHostNotAllowed):
		return "host_not_allowed"
	case errors.Is(err, ErrTargetDenied):
		return "target_denied"
	case errors.Is(err, ErrInvalidURL):
		return "invalid_url"
	case errors.Is(err, ErrRedirectBlocked):
		return "redirect_blocked"
	}
	if conditional && errors.Is(err, ErrConditionalBodyTooLarge) {
		return "body_too_large"
	}
	switch {
	case errors.Is(err, ErrCacheWriteFailed):
		return "cache_write_failed"
	case errors.Is(err, ErrSizeMismatch):
		return "size_mismatch"
	case errors.Is(err, ErrInvalidContentRange):
		return "invalid_content_range"
	case errors.Is(err, ErrTotalSizeMismatch):
		return "total_size_mismatch"
	}

	// Row 13: host_unreachable BEFORE row 20 (unavailable). The
	// dialer wraps both sentinels on a cooldown-probe failure
	// (`%w: %w: ...` at dialer.go:163); putting host_unreachable
	// first surfaces the diagnostic label. Inverting this order
	// silently degrades operator visibility — pinned by the
	// §12.1.6 precedence test.
	if errors.Is(err, ErrHostUnreachable) {
		return "host_unreachable"
	}

	// Rows 14-15: context errors.
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}

	// Rows 16-17: StatusError carriers (specific code class first).
	// errors.As recovers the code from any wrap depth; this comes
	// BEFORE the bare-sentinel checks below so a fetch that wrapped
	// %w with StatusError surfaces the code-class label, not the
	// less-specific bare-sentinel label.
	var se *StatusError
	if errors.As(err, &se) {
		switch {
		case se.Code >= 500 && se.Code < 600:
			return "5xx"
		case se.Code >= 400 && se.Code < 500:
			return "4xx"
		}
	}

	// Rows 18-19: bare upstream-status sentinels (without
	// StatusError in chain).
	switch {
	case errors.Is(err, ErrUpstreamServerError):
		return "5xx"
	case errors.Is(err, ErrUpstreamStatus):
		return "4xx"
	}

	// Row 20: ErrUpstreamUnavailable AFTER ErrHostUnreachable.
	if errors.Is(err, ErrUpstreamUnavailable) {
		return "unavailable"
	}

	// Rows 21-22: net dial errors. *net.OpError can wrap a DNS
	// failure (*net.DNSError) or a connect failure (no DNSError in
	// chain). Check DNSError first so dns_failed wins when both
	// types are present in the chain.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns_failed"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return "connect_failed"
	}

	// Row 23: catch-all.
	return "error"
}
