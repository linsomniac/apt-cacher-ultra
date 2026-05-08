// SPEC6 §5.3 / §9.7.6 rolling connect-outcome counter.
//
// The counter feeds the `tls_mitm_enabled_ca_undistributed` Warn the
// admin refresher emits when MITM is enabled but no client has
// successfully completed a TLS handshake against the cache in the
// last 30 minutes — the textbook "operator turned on MITM but the CA
// is not yet trusted by any client" signal.
//
// Outcome classification (per §5.3):
//
//   - **success** ("TLS handshake reached"): tunneled,
//     inner_method_rejected, inner_stream_failed.
//   - **failure** ("TLS-failure"): tls_failed, tls_handshake_timeout,
//     cert_gen_failed.
//   - **ignored**: bad_target, bad_host, ip_literal_host, bad_port,
//     denied_host. Pre-TLS rejections — configuration / client errors
//     that arrive before the CA-distribution question.
//
// Storage is 30 one-minute buckets in a circular array indexed by
// `unix_minute mod 30`. Record/read are O(1)/O(30); memory bound is
// `30 * sizeof(bucket)` regardless of traffic. The refresher reads
// the totals via Last30Min and emits the Warn when failures >= 1 AND
// successes == 0.

package proxy

import (
	"context"
	"sync"
	"time"
)

// ConnectStats is a 30-minute rolling counter of CONNECT outcomes
// classified per SPEC6 §5.3. Safe for concurrent use; methods are
// O(30) at most.
type ConnectStats struct {
	mu      sync.Mutex
	buckets [30]connectBucket
	nowFn   func() time.Time
}

// connectBucket is one minute of (success, failure) counts. The
// `minute` field holds the unix-minute identifier so a stale bucket
// (last touched > 30 minutes ago) is detected and zeroed on next
// access — this is how circular reuse stays correct without a
// per-minute prune sweep.
type connectBucket struct {
	minute   int64
	success  int
	failure  int
}

// NewConnectStats returns an empty stats counter. nowFn defaults
// to time.Now; tests pass a fake clock.
func NewConnectStats() *ConnectStats {
	return &ConnectStats{nowFn: time.Now}
}

// SetClockForTest overrides the time source. Test-only.
func (s *ConnectStats) SetClockForTest(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nowFn = now
}

// Record classifies `outcome` per §5.3 and bumps the appropriate
// bucket. Outcomes that don't fall into success/failure (pre-TLS
// rejections like bad_target) are silently ignored — they are not
// part of the §9.7.6 distribution-health signal.
func (s *ConnectStats) Record(outcome ConnectOutcome) {
	cls := classifyOutcome(outcome)
	if cls == outcomeIgnored {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.nowFn()
	m := now.Unix() / 60
	idx := int(m % 30)
	if s.buckets[idx].minute != m {
		s.buckets[idx] = connectBucket{minute: m}
	}
	switch cls {
	case outcomeSuccess:
		s.buckets[idx].success++
	case outcomeFailure:
		s.buckets[idx].failure++
	}
}

// Last30Min returns the (success, failure) counts over the rolling
// 30-minute window ending at the current clock time. Buckets whose
// `minute` field is more than 30 minutes stale are excluded — they
// are residue from a quiet period and would otherwise pollute the
// reading on circular reuse.
func (s *ConnectStats) Last30Min() (successes, failures int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.nowFn()
	currentMin := now.Unix() / 60
	cutoff := currentMin - 29 // include the current minute + the 29 before it
	for _, b := range s.buckets {
		if b.minute < cutoff {
			continue
		}
		if b.minute > currentMin {
			// Future-dated bucket — clock skew; skip defensively.
			continue
		}
		successes += b.success
		failures += b.failure
	}
	return successes, failures
}

// outcomeClass is the §5.3 partition of ConnectOutcome values.
type outcomeClass int

const (
	outcomeIgnored outcomeClass = iota
	outcomeSuccess
	outcomeFailure
)

func classifyOutcome(o ConnectOutcome) outcomeClass {
	switch o {
	case OutcomeTunneled, OutcomeInnerMethodRejected, OutcomeInnerStreamFailed:
		return outcomeSuccess
	case OutcomeTLSFailed, OutcomeTLSHandshakeTimeout, OutcomeCertGenFailed:
		return outcomeFailure
	default:
		return outcomeIgnored
	}
}

// RunUndistributedCAWatch is the SPEC6 §9.7.6 refresher. It wakes
// at every `interval` boundary, reads `stats.Last30Min()`, and calls
// `emitWarn` when the spec's predicate holds:
//
//	failures >= 1 AND successes == 0
//
// The predicate name is "operator turned on MITM but the CA is not
// yet trusted by any client" — pure no-traffic windows do NOT fire
// the warning (the predicate requires at least one TLS-failure to
// have been recorded).
//
// Returns when ctx is cancelled. The refresher does NOT take an
// initial firing on entry; the spec says "fired once per uptime
// hour" — the first opportunity is `interval` after start.
//
// `interval` is wired to 1h in production; tests pass shorter
// values together with stats.SetClockForTest to drive the predicate
// deterministically.
func RunUndistributedCAWatch(ctx context.Context, stats *ConnectStats, interval time.Duration, emitWarn func(successes, failures int)) {
	if stats == nil || emitWarn == nil || interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s, f := stats.Last30Min()
			if f >= 1 && s == 0 {
				emitWarn(s, f)
			}
		}
	}
}
