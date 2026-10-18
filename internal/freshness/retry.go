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
// so a re-signature alone is new content too. (Phase E adds unsigned:
// sha256(Release).)
func contentKey(form adoptionForm, inline, release, sig []byte) string {
	if form == adoptionFormDetached {
		r, g := sha256.Sum256(release), sha256.Sum256(sig)
		h := sha256.New()
		h.Write(r[:])
		h.Write(g[:])
		return hex.EncodeToString(h.Sum(nil))
	}
	s := sha256.Sum256(inline)
	return hex.EncodeToString(s[:])
}

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

// jitterDelay spreads d by ±25 %: u is uniform in [0, 1); u = 0.5 is d.
func jitterDelay(d time.Duration, u float64) time.Duration {
	return d + time.Duration((u*2-1)*retryJitterFrac*float64(d))
}
