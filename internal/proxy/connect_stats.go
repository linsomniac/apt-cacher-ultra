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
//     inner_method_rejected, inner_header_timeout, inner_header_too_large,
//     inner_stream_failed.
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
//
// Buckets are keyed by *uptime minutes since NewConnectStats* — not
// wall-clock unix minutes — so the §9.7.6 predicate is robust to
// NTP correction or operator-side clock skew. A backwards jump on
// the wall clock would otherwise either prematurely expire failures
// or hide recent buckets as "future"; uptime monotonically advances
// even when the wall clock doesn't.
type ConnectStats struct {
	mu      sync.Mutex
	buckets [30]connectBucket
	nowFn   func() time.Time
	start   time.Time // baseline captured at construction (or SetClockForTest)
}

// connectBucket is one minute of (success, failure) counts. The
// `minute` field holds the uptime-minute identifier so a stale
// bucket (last touched > 30 minutes ago) is detected and zeroed on
// next access — this is how circular reuse stays correct without a
// per-minute prune sweep.
type connectBucket struct {
	minute   int64
	success  int
	failure  int
}

// NewConnectStats returns an empty stats counter. nowFn defaults
// to time.Now; tests pass a fake clock.
func NewConnectStats() *ConnectStats {
	return &ConnectStats{nowFn: time.Now, start: time.Now()}
}

// SetClockForTest overrides the time source AND resets the uptime
// baseline to the new clock's current value. Tests must call this
// before recording — without the baseline reset, bucket math would
// mix construction-time wall-clock with the fake clock and produce
// nonsense uptime values. Test-only.
func (s *ConnectStats) SetClockForTest(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nowFn = now
	s.start = now()
	// Zero the buckets too so a fresh fake-clock test sees a clean slate.
	for i := range s.buckets {
		s.buckets[i] = connectBucket{}
	}
}

// Record classifies `outcome` per §5.3 and bumps the appropriate
// bucket. `tlsReached` resolves the ambiguity around
// `OutcomeInnerStreamFailed`: the spec class "TLS handshake reached"
// only applies when the handshake actually completed (post-handshake
// inner-stream errors). Pre-hijack / pre-200-write / pre-handshake
// `inner_stream_failed` cases pass tlsReached=false and are
// classified as ignored — those failures predate the question of CA
// trust. Other outcomes (tunneled, tls_failed, etc.) have a fixed
// classification and ignore the flag.
func (s *ConnectStats) Record(outcome ConnectOutcome, tlsReached bool) {
	cls := classifyOutcome(outcome, tlsReached)
	if cls == outcomeIgnored {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.uptimeMinuteLocked()
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
// 30-minute window ending at the current uptime minute. Buckets
// whose `minute` field is more than 30 minutes stale are excluded
// — they are residue from a quiet period and would otherwise
// pollute the reading on circular reuse.
func (s *ConnectStats) Last30Min() (successes, failures int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	currentMin := s.uptimeMinuteLocked()
	cutoff := currentMin - 29 // include the current minute + the 29 before it
	for _, b := range s.buckets {
		if b.minute < cutoff {
			continue
		}
		if b.minute > currentMin {
			// Future-dated bucket — defensive guard; should not
			// arise with monotonic uptime, but cheap to keep.
			continue
		}
		successes += b.success
		failures += b.failure
	}
	return successes, failures
}

// uptimeMinuteLocked returns the integer uptime minute since
// `s.start`. Wall-clock-independent: an NTP jump backwards or a
// manual `date` change does not affect this. Caller must hold s.mu.
func (s *ConnectStats) uptimeMinuteLocked() int64 {
	d := s.nowFn().Sub(s.start)
	if d < 0 {
		// Defensive: a SetClockForTest then advance-backwards
		// would land here. Treat as minute 0.
		return 0
	}
	return int64(d / time.Minute)
}

// outcomeClass is the §5.3 partition of ConnectOutcome values.
type outcomeClass int

const (
	outcomeIgnored outcomeClass = iota
	outcomeSuccess
	outcomeFailure
)

func classifyOutcome(o ConnectOutcome, tlsReached bool) outcomeClass {
	switch o {
	case OutcomeTunneled, OutcomeInnerMethodRejected,
		OutcomeInnerHeaderTimeout, OutcomeInnerHeaderTooLarge:
		// All four fire only post-handshake. Independent of the flag.
		return outcomeSuccess
	case OutcomeInnerStreamFailed:
		// Ambiguous: pre-handshake (hijack/write-200/etc.) or
		// post-handshake (set-deadline/read-inner). Only the
		// post-handshake case proves the CA was trusted.
		if tlsReached {
			return outcomeSuccess
		}
		return outcomeIgnored
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
