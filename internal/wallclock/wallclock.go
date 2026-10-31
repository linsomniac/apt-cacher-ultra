// Package wallclock is the single source of wall-clock time for values
// that are persisted, or compared against persisted values (Unix stamps
// in SQLite). Durations, deadlines and in-memory timing keep using
// time.Now / time.Since, which carry a monotonic reading.
//
// AIDEV-NOTE: tests step the wall clock backwards and forwards through
// SetOffsetForTest. testing/synctest alone can only move time forward,
// and clock-step handling (spec C4) must be provable. Never read the
// wall clock for a persisted stamp any other way.
package wallclock

import (
	"sync/atomic"
	"time"
)

var offsetNanos atomic.Int64

// Now returns the current wall-clock time, shifted by the test offset
// (always zero in production). The monotonic reading is stripped so
// comparisons use wall time, exactly like persisted Unix stamps.
func Now() time.Time {
	return time.Now().Round(0).Add(time.Duration(offsetNanos.Load()))
}

// Unix returns Now().Unix().
func Unix() int64 { return Now().Unix() }

// SetOffsetForTest shifts Now by d and returns a function that restores
// the previous offset. Only tests call it (TestSetOffsetForTest_OnlyTestsCallIt
// walks cmd/ and internal/). Restores run in the reverse order of the calls
// — last in, first out, as deferred calls and t.Cleanup run them — since
// each restores the offset in force when its call was made (deferred
// minor A-2).
func SetOffsetForTest(d time.Duration) (restore func()) {
	prev := offsetNanos.Swap(int64(d))
	return func() { offsetNanos.Store(prev) }
}
