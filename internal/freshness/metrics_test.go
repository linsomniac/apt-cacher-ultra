package freshness

import (
	"errors"
	"fmt"
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
