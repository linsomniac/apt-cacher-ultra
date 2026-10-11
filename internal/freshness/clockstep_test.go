package freshness

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
)

func seedFakeAnchor(fc *fakeCache) {
	stale := testHex64
	fc.putURL(cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: "/r/dists/x/InRelease",
		BlobHash: &stale, UpstreamURL: "http://h/r/dists/x/InRelease", IsMetadata: true})
}

// I-7 / spec C4: a last_check_at stamp in the future (backward clock
// step) must not freeze checks; the check heals the stamp, after which
// the cooldown works again; the WARN is rate-limited.
func TestCheck_FutureLastCheckAtDoesNotFreezeAndHeals(t *testing.T) {
	fc := newFakeCache()
	seedFakeAnchor(fc)
	now := time.Unix(1_800_000_000, 0)
	future := now.Add(2 * time.Hour).Unix()
	_ = fc.PutSuiteFreshness(context.Background(), cache.SuiteFreshness{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", LastCheckAt: &future, LastSuccessAt: &future})
	up := &changedUpstream{body: []byte("new")}
	logs := &lockedBuffer{}
	c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8), Cooldown: time.Minute,
		Logger: slog.New(slog.NewTextHandler(logs, nil)), now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	c.Check(context.Background(), "http", "h", "/r/dists/x")
	c.Check(context.Background(), "http", "h", "/r/dists/x") // within the cooldown of the healed stamp
	if got := up.calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (frozen, or cooldown not restored after healing)", got)
	}
	got, _ := fc.suite("http", "h", "/r/dists/x")
	if got.LastCheckAt == nil || *got.LastCheckAt != now.Unix() {
		t.Fatalf("last_check_at not healed to now: %v", got.LastCheckAt)
	}
	// A second detection within a minute must not log again.
	c.clockStep("last_check_at", "http", "h", "/r/dists/y", future, now)
	if n := strings.Count(logs.String(), "clock_step_detected"); n != 1 {
		t.Fatalf("clock_step_detected lines = %d, want 1 (rate-limited)", n)
	}
}

// A stamp within the tolerance is "just now", not a clock step: the
// cooldown still applies.
func TestCheck_StampWithinToleranceKeepsCooldown(t *testing.T) {
	fc := newFakeCache()
	seedFakeAnchor(fc)
	now := time.Unix(1_800_000_000, 0)
	justAhead := now.Unix() + 1
	_ = fc.PutSuiteFreshness(context.Background(), cache.SuiteFreshness{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", LastCheckAt: &justAhead})
	up := &changedUpstream{body: []byte("new")}
	c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8), Cooldown: time.Minute,
		Logger: discardLogger(), now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	c.Check(context.Background(), "http", "h", "/r/dists/x")
	if got := up.calls.Load(); got != 0 {
		t.Fatalf("upstream calls = %d, want 0 (1 s ahead is within tolerance: cooldown applies)", got)
	}
}

func TestTick_FutureLastSuccessAtIsDue(t *testing.T) {
	fc := newFakeCache()
	seedFakeAnchor(fc)
	now := time.Unix(1_800_000_000, 0)
	future := now.Add(30 * 24 * time.Hour).Unix()
	_ = fc.PutSuiteFreshness(context.Background(), cache.SuiteFreshness{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", LastSuccessAt: &future})
	up := &changedUpstream{body: []byte("new")}
	c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute,
		Logger: discardLogger(), now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	c.tick(context.Background())
	if up.calls.Load() != 1 {
		t.Fatalf("scheduler skipped a suite whose last_success_at is in the future")
	}
}
