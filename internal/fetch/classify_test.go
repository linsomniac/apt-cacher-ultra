package fetch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
)

// TestClassifyFetchOutcome_Success — nil err is "success".
func TestClassifyFetchOutcome_Success(t *testing.T) {
	if got := ClassifyFetchOutcome(nil); got != "success" {
		t.Errorf("nil err = %q, want success", got)
	}
}

// TestClassifyFetchOutcome_AllSentinels covers every row of the
// SPEC5 §10.4.2 precedence table that does NOT involve Conditional-
// specific behavior.
func TestClassifyFetchOutcome_AllSentinels(t *testing.T) {
	dialErr := &net.OpError{
		Op:  "dial",
		Net: "tcp",
		Err: errors.New("connection refused"),
	}
	dnsErr := &net.OpError{
		Op:  "dial",
		Net: "tcp",
		Err: &net.DNSError{Name: "nope.invalid", Err: "no such host"},
	}

	cases := []struct {
		name string
		err  error
		want string
	}{
		{"ErrHostNotAllowed", ErrHostNotAllowed, "host_not_allowed"},
		{"ErrTargetDenied", ErrTargetDenied, "target_denied"},
		{"ErrInvalidURL", ErrInvalidURL, "invalid_url"},
		{"ErrRedirectBlocked", ErrRedirectBlocked, "redirect_blocked"},
		{"ErrCacheWriteFailed", ErrCacheWriteFailed, "cache_write_failed"},
		{"ErrSizeMismatch", ErrSizeMismatch, "size_mismatch"},
		{"ErrInvalidContentRange", ErrInvalidContentRange, "invalid_content_range"},
		{"ErrTotalSizeMismatch", ErrTotalSizeMismatch, "total_size_mismatch"},
		{"ErrHostUnreachable", ErrHostUnreachable, "host_unreachable"},
		{"context.DeadlineExceeded", context.DeadlineExceeded, "timeout"},
		{"context.Canceled", context.Canceled, "canceled"},
		{"StatusError 503", &StatusError{Code: 503}, "5xx"},
		{"StatusError 404", &StatusError{Code: 404}, "4xx"},
		{"ErrUpstreamServerError bare", ErrUpstreamServerError, "5xx"},
		{"ErrUpstreamStatus bare", ErrUpstreamStatus, "4xx"},
		{"ErrUpstreamUnavailable", ErrUpstreamUnavailable, "unavailable"},
		{"DNS error", dnsErr, "dns_failed"},
		{"connect refused", dialErr, "connect_failed"},
		{"synthetic catch-all", errors.New("synthetic"), "error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyFetchOutcome(tc.err); got != tc.want {
				t.Errorf("ClassifyFetchOutcome(%v) = %q, want %q",
					tc.err, got, tc.want)
			}
		})
	}
}

// TestClassifyFetchOutcome_DialerCooldownPrecedence is the
// load-bearing test from SPEC5 §12.1.6. The dialer's cooldown-probe
// path wraps a single error with BOTH ErrUpstreamUnavailable AND
// ErrHostUnreachable (`%w: %w: %v` at internal/fetch/dialer.go:163).
// Inverting precedence here silently degrades operator visibility —
// every cooldown event would surface as "unavailable" instead of
// the more actionable "host_unreachable".
func TestClassifyFetchOutcome_DialerCooldownPrecedence(t *testing.T) {
	innerErr := errors.New("dial tcp: i/o timeout")
	wrapped := fmt.Errorf("%w: %w: %v",
		ErrUpstreamUnavailable, ErrHostUnreachable, innerErr)
	if got := ClassifyFetchOutcome(wrapped); got != "host_unreachable" {
		t.Errorf("dialer-cooldown wrap classified as %q, want host_unreachable.\n"+
			"err=%v", got, wrapped)
	}
}

// TestClassifyFetchOutcome_StatusErrorBeatsBareSentinel verifies a
// wrapped StatusError surfaces the code-class label rather than the
// bare-sentinel label.
func TestClassifyFetchOutcome_StatusErrorBeatsBareSentinel(t *testing.T) {
	// A *StatusError satisfies ErrUpstreamServerError via its Is
	// method when Code is 5xx. The classifier should pick "5xx"
	// (from errors.As(*StatusError)) rather than re-routing
	// through the bare-sentinel arm.
	wrapped := fmt.Errorf("%w: %w", ErrUpstreamUnavailable, &StatusError{Code: 502})
	if got := ClassifyFetchOutcome(wrapped); got != "5xx" {
		t.Errorf("wrapped StatusError(502) classified as %q, want 5xx", got)
	}
}

// TestClassifyConditionalOutcome_StatusBranches covers the success
// path's status-code branches.
func TestClassifyConditionalOutcome_StatusBranches(t *testing.T) {
	cases := []struct {
		name   string
		res    *ConditionalResult
		err    error
		want   string
	}{
		{"nil err, 200", &ConditionalResult{Status: 200}, nil, "cond_changed"},
		{"nil err, 304", &ConditionalResult{Status: 304}, nil, "cond_unchanged"},
		{"nil err, status 0 (zero value, programming error)", &ConditionalResult{}, nil, "error"},
		{"nil err, nil res (programming error)", nil, nil, "error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyConditionalOutcome(tc.res, tc.err); got != tc.want {
				t.Errorf("ClassifyConditionalOutcome(%v, %v) = %q, want %q",
					tc.res, tc.err, got, tc.want)
			}
		})
	}
}

// TestClassifyConditionalOutcome_BodyTooLarge verifies the
// Conditional-specific sentinel surfaces correctly. Fetch path does
// not classify ErrConditionalBodyTooLarge as body_too_large; it
// would fall to "error" because Fetch never produces it.
func TestClassifyConditionalOutcome_BodyTooLarge(t *testing.T) {
	if got := ClassifyConditionalOutcome(nil, ErrConditionalBodyTooLarge); got != "body_too_large" {
		t.Errorf("Conditional body-too-large = %q, want body_too_large", got)
	}
	// Fetch's classifier does not know about this sentinel.
	if got := ClassifyFetchOutcome(ErrConditionalBodyTooLarge); got != "error" {
		t.Errorf("Fetch body-too-large = %q, want error (catch-all)", got)
	}
}

// TestClassifyConditionalOutcome_Sentinels covers the rest of the
// classifier table for the Conditional helper. Most sentinels share
// the Fetch path; this just spot-checks that the routing is the same.
func TestClassifyConditionalOutcome_Sentinels(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"ErrHostNotAllowed", ErrHostNotAllowed, "host_not_allowed"},
		{"ErrUpstreamServerError", &StatusError{Code: 502}, "5xx"},
		{"ErrUpstreamStatus", &StatusError{Code: 404}, "4xx"},
		{"timeout", context.DeadlineExceeded, "timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyConditionalOutcome(nil, tc.err); got != tc.want {
				t.Errorf("ClassifyConditionalOutcome(nil, %v) = %q, want %q",
					tc.err, got, tc.want)
			}
		})
	}
}
