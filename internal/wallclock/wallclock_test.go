package wallclock

import (
	"testing"
	"time"
)

func TestNow_TracksRealTimeWithZeroOffset(t *testing.T) {
	before := time.Now().Add(-time.Second)
	got := Now()
	after := time.Now().Add(time.Second)
	if got.Before(before) || got.After(after) {
		t.Fatalf("Now() = %v, want between %v and %v", got, before, after)
	}
}

func TestSetOffsetForTest_ShiftsAndRestores(t *testing.T) {
	restore := SetOffsetForTest(-2 * time.Hour)
	shifted := Now()
	if d := time.Since(shifted); d < 2*time.Hour-time.Minute || d > 2*time.Hour+time.Minute {
		t.Fatalf("offset not applied: time.Since(Now()) = %v, want ~2h", d)
	}
	n, u := Now().Unix(), Unix() // read once each: a second boundary may fall between them
	if u != n && u != n+1 {
		t.Fatalf("Unix() = %d disagrees with Now().Unix() = %d", u, n)
	}
	restore()
	if d := time.Since(Now()); d > time.Minute || d < -time.Minute {
		t.Fatalf("offset not restored: %v", d)
	}
}

func TestNow_HasNoMonotonicReading(t *testing.T) {
	// A wall-clock value must compare by wall time only, so test offsets
	// behave like real clock steps.
	if s := Now().String(); containsMonotonic(s) {
		t.Fatalf("Now() carries a monotonic reading: %s", s)
	}
}

func containsMonotonic(s string) bool {
	for i := 0; i+2 < len(s); i++ {
		if s[i:i+3] == "m=+" || s[i:i+3] == "m=-" {
			return true
		}
	}
	return false
}
