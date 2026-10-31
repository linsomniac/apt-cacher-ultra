package observability

import (
	"sync"
	"time"
)

// LogLimiter rate-limits one log line: at most one line per interval,
// counting the calls it suppressed in between so the next line can say how
// many it hid. Its zero value is ready to use, and it is safe for
// concurrent use. It starts no goroutine.
//
// AIDEV-NOTE: ruling PG-22 (phase G). This is the one copy of the
// one-line-a-minute WARN limiter: suite_seed_rejected (handler
// noteCreate), flat_host_root_unsupported (handler noteFlatHostRoot) and
// the anchorless over-cap WARN (freshness noteAnchorlessOverCap) all use
// it. Its two subtle points live here only: the first call is always
// allowed (a zero last), and the suppressed count resets when a line is
// allowed. Pass monotonic time (time.Now()), so a wall-clock step can
// neither unthrottle it nor silence it. Emit the line after Allow returns:
// the lock covers only the bookkeeping.
type LogLimiter struct {
	mu         sync.Mutex
	last       time.Time
	suppressed int
}

// Allow reports whether a line may be emitted at now, at most one per
// every (the call site's constant). When it may (ok), suppressed is the
// number of calls refused since the previous allowed one, and the count
// starts again from zero; when it may not, the call is counted and
// suppressed is 0.
func (l *LogLimiter) Allow(now time.Time, every time.Duration) (ok bool, suppressed int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.last.IsZero() && now.Sub(l.last) < every {
		l.suppressed++
		return false, 0
	}
	suppressed = l.suppressed
	l.last, l.suppressed = now, 0
	return true, suppressed
}
