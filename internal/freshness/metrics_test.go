package freshness

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestClassifyAdoptionOutcome_TotalFunction pins the SPEC5 §10.4.3
// outcome enum to the freshness-layer error sentinels.
//
// Each row exercises one terminal-error path the Adopter can return.
// The unpinned_suite row uses an externally-wrapped error to mimic
// the gpg.Verifier's `%w: %w` shape (the runShared wrap that lets
// errors.Is recover the verifier's chain). If a future refactor
// inverts the sentinel order or changes the wrap, this test is the
// canary.
func TestClassifyAdoptionOutcome_TotalFunction(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, "success"},
		{"parse_failed", fmt.Errorf("%w: bad release", ErrAdoptionParseFailed), "parse_failed"},
		{"member_mismatch", fmt.Errorf("%w: hash diverged", ErrAdoptionMemberMismatch), "member_mismatch"},
		{"gpg_failed", fmt.Errorf("%w: verify failed", ErrAdoptionGPGFailed), "gpg_failed"},
		{
			// Mirrors the runShared wrap of an unpinned-pin verifier
			// return: gpg_failed wraps an inner err whose chain
			// includes ErrAdoptionUnpinnedSuite. Classifier MUST
			// route to unpinned_suite, not gpg_failed.
			name: "unpinned_suite_wins_over_gpg_failed",
			err:  fmt.Errorf("%w: %w", ErrAdoptionGPGFailed, fmt.Errorf("%w: no pin", ErrAdoptionUnpinnedSuite)),
			want: "unpinned_suite",
		},
		{
			"db_failed_falls_through_to_run_failed",
			fmt.Errorf("%w: write transaction", ErrAdoptionDBFailed),
			"run_failed",
		},
		{
			"member_fetch_falls_through_to_run_failed",
			fmt.Errorf("%w: upstream timeout", ErrAdoptionMemberFetchFailed),
			"run_failed",
		},
		{"unknown_error_falls_through", errors.New("something else"), "run_failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyAdoptionOutcome(tc.err)
			if got != tc.want {
				t.Errorf("classifyAdoptionOutcome(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestClassifyAdoptionReason exercises the SPEC5 §10.5
// recent_adoptions[].reason mapping. Specifically: success returns
// empty; non-gpg buckets mirror the outcome label; gpg_failed
// delegates to the injected classifier and falls back to the bucket
// when the classifier is nil or returns "".
func TestClassifyAdoptionReason(t *testing.T) {
	// Stub classifier — the real one (gpg.ClassifyVerifyErr) is
	// tested in the gpg package. Here we only exercise the
	// freshness-side dispatch.
	stub := func(err error) string {
		if err == nil {
			return ""
		}
		if strings.Contains(err.Error(), "untrusted") {
			return "untrusted_signer"
		}
		return ""
	}

	tests := []struct {
		name       string
		err        error
		outcome    string
		classifier func(error) string
		want       string
	}{
		{"nil_returns_empty", nil, "success", stub, ""},
		{
			"gpg_bucket_delegates_to_classifier",
			fmt.Errorf("%w: untrusted", ErrAdoptionGPGFailed),
			"gpg_failed",
			stub,
			"untrusted_signer",
		},
		{
			"gpg_bucket_falls_back_when_classifier_returns_empty",
			fmt.Errorf("%w: weird crypto thing", ErrAdoptionGPGFailed),
			"gpg_failed",
			stub,
			"gpg_failed",
		},
		{
			"gpg_bucket_falls_back_when_classifier_nil",
			fmt.Errorf("%w: untrusted", ErrAdoptionGPGFailed),
			"gpg_failed",
			nil,
			"gpg_failed",
		},
		{
			"parse_failed_mirrors_outcome",
			fmt.Errorf("%w: bad release", ErrAdoptionParseFailed),
			"parse_failed",
			stub,
			"parse_failed",
		},
		{
			"unpinned_suite_mirrors_outcome_not_consult_gpg_classifier",
			fmt.Errorf("%w: no pin", ErrAdoptionUnpinnedSuite),
			"unpinned_suite",
			stub,
			"unpinned_suite",
		},
		{
			"run_failed_mirrors_outcome",
			errors.New("transport gave up"),
			"run_failed",
			stub,
			"run_failed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyAdoptionReason(tc.err, tc.outcome, tc.classifier)
			if got != tc.want {
				t.Errorf("classifyAdoptionReason(%v, %q) = %q, want %q", tc.err, tc.outcome, got, tc.want)
			}
		})
	}
}
