package freshness

import (
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// AIDEV-NOTE: split out of freshness.go (A-43; freshness.go had grown past
// 1,150 lines). Pure move — no behaviour change; see clockstep_test.go.

// clockStepTolerance: stamps are whole seconds, so a sub-second backward
// NTP step can make a just-written stamp look up to 1 s in the future.
// Only a stamp further ahead than this counts as a clock step.
//
// AIDEV-NOTE: internal/admin mirrors it as statusClockStepTolerance for the
// status page's backoff text (TestSuiteEntries_FailingChecksSayWhenTheBackoffEnds):
// change both together.
const clockStepTolerance = 2 * time.Second

// clockStepLogEvery rate-limits clock_step_detected to one line per
// minute per checker (monotonic).
const clockStepLogEvery = time.Minute

var processStart = time.Now()

// clockStep records a past-event stamp found in the future (spec C4).
//
// AIDEV-NOTE: only PAST-EVENT stamps (last_check_at, last_success_at,
// last_requested_at, adoption_fail_first_at/last_at, adopted_at) are
// "infinitely old" when in the future — for a gate that starts work. A
// gate that retires things reads a future stamp as now instead, never as
// old: cap eviction and the reaper (last_requested_at), the orphan-marker
// sweep (suite_signed_seen.last_seen_at, read as now, never rewritten;
// rulings R-F5, PF-1) and the legacy junk grace (url_path.last_fetched_at,
// no evidence while in the future; not reported). The reaper and the
// sweep report the furthest such stamp at their first run after a start
// and when it changes (design review M-10), not every run, and log it
// directly (clockStepUnlimited): bounded so already, it never goes through
// the one-line-a-minute limit, which a check's report could have used up
// (the Task 16 review's minor 7). adopted_at
// may legitimately lead the clock since spec C8's monotonic stamp
// (max(now, prior + 1)); no gate reads it as a past event today, so a
// check that starts to must allow for that. adoption_next_retry_at is a
// FUTURE-event stamp: a detection is logged through this function, but the
// stamp is clamped to now+1h (retryGateFor), never treated as infinitely
// old.
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
	c.logClockStep(stamp, scheme, host, suitePath, stored, now)
}

// clockStepUnlimited is clockStep for a caller that bounds its own reports
// to one per change — the reaper's furthest future last_requested_at and
// the marker sweep's furthest future last_seen_at (design review M-10):
// counted in acu_clock_steps_detected_total like every report, and logged
// directly, neither subject to nor using up the one-line-a-minute limit.
// Through the limit, a check's report in the same minute would swallow the
// line, and the reaper, having recorded it as reported, would not repeat
// it until the stamp changed (the Task 16 review's minor 7).
func (c *Checker) clockStepUnlimited(stamp, scheme, host, suitePath string, stored int64, now time.Time) {
	clockStepsTotal.Inc()
	c.logClockStep(stamp, scheme, host, suitePath, stored, now)
}

// logClockStep writes one clock_step_detected line.
func (c *Checker) logClockStep(stamp, scheme, host, suitePath string, stored int64, now time.Time) {
	c.logger.Warn("clock_step_detected",
		"stamp", stamp,
		"canonical_scheme", scheme,
		"canonical_host", host,
		"suite_path", suitePath,
		"stored_unixtime", stored,
		"now_unixtime", now.Unix(),
	)
}

// ReportClockStep reports a past-event stamp that cap eviction found in
// the future — a backward wall-clock step (cache.ClockStepReporter, ruling
// PD-11) — through the checker's rate-limited clock_step_detected and
// acu_clock_steps_detected_total, like every other gate (spec C4).
func (c *Checker) ReportClockStep(stamp string, key cache.SuiteKey, stored, now int64) {
	c.clockStep(stamp, key.CanonicalScheme, key.CanonicalHost, key.SuitePath, stored, time.Unix(now, 0))
}
