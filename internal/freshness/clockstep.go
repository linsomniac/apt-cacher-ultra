package freshness

import "time"

// AIDEV-NOTE: split out of freshness.go (A-43; freshness.go had grown past
// 1,150 lines). Pure move — no behaviour change; see clockstep_test.go.

// clockStepTolerance: stamps are whole seconds, so a sub-second backward
// NTP step can make a just-written stamp look up to 1 s in the future.
// Only a stamp further ahead than this counts as a clock step.
const clockStepTolerance = 2 * time.Second

// clockStepLogEvery rate-limits clock_step_detected to one line per
// minute per checker (monotonic).
const clockStepLogEvery = time.Minute

var processStart = time.Now()

// clockStep records a past-event stamp found in the future (spec C4).
//
// AIDEV-NOTE: only PAST-EVENT stamps (last_check_at, last_success_at,
// last_requested_at, adoption_fail_first_at/last_at, adopted_at) are
// "infinitely old" when in the future. adopted_at may legitimately lead
// the clock since spec C8's monotonic stamp (max(now, prior + 1)); no gate
// reads it as a past event today, so a check that starts to must allow
// for that. adoption_next_retry_at is a FUTURE-event stamp: a detection
// is logged through this function, but the stamp is clamped to now+1h
// (retryGateFor), never treated as infinitely old.
func (c *Checker) clockStep(stamp, scheme, host, suitePath string, stored int64, now time.Time) {
	clockStepsTotal.Inc()
	last := c.lastClockStepLog.Load()
	mono := int64(time.Since(processStart))
	if last != 0 && time.Duration(mono-last) < clockStepLogEvery {
		return
	}
	if !c.lastClockStepLog.CompareAndSwap(last, mono) {
		return
	}
	c.logger.Warn("clock_step_detected",
		"stamp", stamp,
		"canonical_scheme", scheme,
		"canonical_host", host,
		"suite_path", suitePath,
		"stored_unixtime", stored,
		"now_unixtime", now.Unix(),
	)
}
