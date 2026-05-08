package proxy

import (
	"sync"
	"testing"
	"time"
)

// hitRateClock is a minimal monotonic-stepping fake clock for the
// 60-bucket rolling counter. Each Now call returns the configured t.
type hitRateClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *hitRateClock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *hitRateClock) Set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

func TestCertHitRate_EmptyWindow(t *testing.T) {
	r := NewCertHitRate(time.Now)
	hits, misses := r.Last60s()
	if hits != 0 || misses != 0 {
		t.Errorf("empty window: got %d hits / %d misses, want 0/0", hits, misses)
	}
}

func TestCertHitRate_BucketsWithinWindow(t *testing.T) {
	clk := &hitRateClock{t: time.Unix(1_700_000_000, 0)}
	r := NewCertHitRate(clk.Now)

	r.Note(true)
	r.Note(true)
	r.Note(false)

	hits, misses := r.Last60s()
	if hits != 2 || misses != 1 {
		t.Errorf("got %d hits / %d misses, want 2/1", hits, misses)
	}
}

func TestCertHitRate_StaleBucketsAreExcluded(t *testing.T) {
	clk := &hitRateClock{t: time.Unix(1_700_000_000, 0)}
	r := NewCertHitRate(clk.Now)

	// Bucket at second 0: 5 hits.
	for range 5 {
		r.Note(true)
	}

	// Advance clock past 60s — the 5 hits are now outside the window.
	clk.Set(time.Unix(1_700_000_000+90, 0))

	// One miss at second 90.
	r.Note(false)

	hits, misses := r.Last60s()
	if hits != 0 {
		t.Errorf("hits = %d, want 0 (the 5 hits are >60s old)", hits)
	}
	if misses != 1 {
		t.Errorf("misses = %d, want 1", misses)
	}
}

// TestCertHitRate_ClockRollbackExcludesFutureBuckets pins the
// codex-review finding: when the injectable clock moves backward
// (only reachable in tests), Last60s must NOT count buckets whose
// recorded second is greater than the current clock — the
// `b.second <= now` clause is the regression site. Without that
// check, stale samples would be resurrected by a backward jump
// because the (cutoff, now] window would slide past them.
func TestCertHitRate_ClockRollbackExcludesFutureBuckets(t *testing.T) {
	clk := &hitRateClock{t: time.Unix(1_700_000_000, 0)}
	r := NewCertHitRate(clk.Now)

	// Advance clock to second 100 of uptime, then record.
	clk.Set(time.Unix(1_700_000_100, 0))
	r.Note(true)
	r.Note(false)
	if hits, misses := r.Last60s(); hits != 1 || misses != 1 {
		t.Fatalf("baseline at uptime=100s: got %d/%d, want 1/1", hits, misses)
	}

	// Roll the clock back to uptime=10s — buckets recorded at
	// second 100 must NOT be counted as if they were inside the
	// (-50, 10] window. Without the b.second <= now guard, the
	// stale samples slide back into a sliding 60s window.
	clk.Set(time.Unix(1_700_000_010, 0))
	hits, misses := r.Last60s()
	if hits != 0 || misses != 0 {
		t.Errorf("clock rollback resurrected future-dated buckets: got %d/%d, want 0/0", hits, misses)
	}
}

func TestCertHitRate_BucketRolloverClearsCounts(t *testing.T) {
	// Bucketing is `uptime_second mod 60`. After 60s the index wraps
	// to the same bucket; the second-of-uptime mismatch must zero
	// the bucket before re-counting, otherwise stale counts pile up.
	clk := &hitRateClock{t: time.Unix(1_700_000_000, 0)}
	r := NewCertHitRate(clk.Now)

	r.Note(true) // bucket 0, second 0
	r.Note(true) // bucket 0, second 0

	clk.Set(time.Unix(1_700_000_000+60, 0)) // second 60 → bucket 0 again
	r.Note(false)                           // bucket 0 must zero, then count miss

	hits, misses := r.Last60s()
	if hits != 0 {
		t.Errorf("rollover did not clear bucket: got %d hits, want 0", hits)
	}
	if misses != 1 {
		t.Errorf("rollover bucket lost the miss: got %d, want 1", misses)
	}
}

func TestCertIssuance_EmptyThenRecorded(t *testing.T) {
	tracker := &CertIssuance{}
	if _, _, ok := tracker.Last(); ok {
		t.Errorf("empty tracker returned ok=true")
	}
	at := time.Unix(1_700_000_000, 0)
	tracker.Note("archive.ubuntu.com", at)
	host, gotAt, ok := tracker.Last()
	if !ok || host != "archive.ubuntu.com" || !gotAt.Equal(at) {
		t.Errorf("Last() = (%q, %v, %v), want (archive.ubuntu.com, %v, true)", host, gotAt, ok, at)
	}
	// Subsequent Note overwrites.
	at2 := at.Add(time.Minute)
	tracker.Note("security.debian.org", at2)
	host, gotAt, _ = tracker.Last()
	if host != "security.debian.org" || !gotAt.Equal(at2) {
		t.Errorf("after overwrite Last() = (%q, %v), want (security.debian.org, %v)", host, gotAt, at2)
	}
}

func TestPackageGlobals_RecordCertCacheLookupFeedsHitRate(t *testing.T) {
	clk := &hitRateClock{t: time.Unix(1_700_000_000, 0)}
	restore := SwapCertHitRateForTest(clk.Now)
	defer restore()

	RecordCertCacheLookup(true)
	RecordCertCacheLookup(true)
	RecordCertCacheLookup(false)

	hits, misses := CertHitRate60s()
	if hits != 2 || misses != 1 {
		t.Errorf("global: got %d hits / %d misses, want 2/1", hits, misses)
	}
}

func TestPackageGlobals_NoteCertIssuedFeedsLast(t *testing.T) {
	ResetCertIssuanceForTest()
	defer ResetCertIssuanceForTest()

	if _, _, ok := LastCertIssued(); ok {
		t.Errorf("LastCertIssued() returned ok=true on empty tracker")
	}
	at := time.Unix(1_700_000_000, 0)
	NoteCertIssued("apt.example.com", at)
	host, gotAt, ok := LastCertIssued()
	if !ok || host != "apt.example.com" || !gotAt.Equal(at) {
		t.Errorf("LastCertIssued() = (%q, %v, %v), want (apt.example.com, %v, true)", host, gotAt, ok, at)
	}
}
