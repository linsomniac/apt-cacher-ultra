package freshness

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// Spec C7/C4 retry schedule (C11: constants, not knobs). One schedule
// serves adoption failures and the scheduler's unknown-check backoff.
const (
	retryBaseDelay  = 2 * time.Minute
	retryMaxDelay   = time.Hour
	retryJitterFrac = 0.25

	// retryHorizon is the furthest ahead a next_retry_at can legitimately
	// be written (the 1 h cap plus 25 % jitter). A stored value further
	// ahead comes from a backward clock step and is clamped to now + 1 h.
	retryHorizon = retryMaxDelay + retryMaxDelay/4
)

// contentKey identifies the content an adoption attempted (spec C7).
// Any byte change is new content, attempted at once. Inline:
// sha256(InRelease). Detached: sha256(sha256(Release) || sha256(Release.gpg)),
// so a re-signature alone is new content too. Unsigned:
// sha256("unsigned\n" || Release), domain-separated from the inline key, so
// a record for an InRelease body never matches a Release with the same
// bytes (design review M-2: the "confirmed once" rule trusts a record with
// an equal key).
//
// AIDEV-NOTE: each form keys its own argument — inline the InRelease
// (inline), detached and unsigned the Release (release). Keying the
// unsigned Release through inline (nil) would give all unsigned content
// one key, and new content would be deferred (overview, B carry-forward).
// No stored record holds an unsigned key from before phase E.
func contentKey(form adoptionForm, inline, release, sig []byte) string {
	switch form {
	case adoptionFormDetached:
		r, g := sha256.Sum256(release), sha256.Sum256(sig)
		h := sha256.New()
		h.Write(r[:])
		h.Write(g[:])
		return hex.EncodeToString(h.Sum(nil))
	case adoptionFormUnsigned:
		h := sha256.New()
		h.Write([]byte("unsigned\n"))
		h.Write(release)
		return hex.EncodeToString(h.Sum(nil))
	}
	s := sha256.Sum256(inline)
	return hex.EncodeToString(s[:])
}

// CheckBackoff is the scheduler's backoff after a streak of failCount
// checks that ended unknown (check_fail_count, spec C4/C11): no failing
// suite is queued before last_check_at + CheckBackoff(failCount). The
// admin status page shows when that backoff ends (deferred minor B-63)
// through admin.Config.CheckBackoff, which main sets to this.
func CheckBackoff(failCount int64) time.Duration { return backoffDelay(failCount) }

// backoffDelay is min(2 min × 2^(count−1), 1 h) for a streak of count
// failures; 0 for count < 1.
func backoffDelay(count int64) time.Duration {
	if count < 1 {
		return 0
	}
	if count > 6 { // 2 min << 5 already exceeds the cap; avoid shift overflow
		return retryMaxDelay
	}
	return min(retryBaseDelay<<(count-1), retryMaxDelay)
}

// jitterDelay spreads d by ±25 %: u is uniform in [0, 1); u = 0.5 is d. A
// u outside [0, 1] (or NaN) is clamped, so the delay stays within ±25 %
// whatever the random seam returns (deferred minor B-17).
func jitterDelay(d time.Duration, u float64) time.Duration {
	if !(u >= 0) { // also NaN
		u = 0
	}
	u = min(u, 1)
	return d + time.Duration((u*2-1)*retryJitterFrac*float64(d))
}
