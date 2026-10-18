package freshness

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// setStall makes every rig upstream request block until its context ends.
func (u *rigUpstream) setStall(v bool) { u.mu.Lock(); u.stall = v; u.mu.Unlock() }

// realStreakChecker runs a checker over the real cache, whose writes
// honour their context: an expired check deadline must not leak into the
// bookkeeping write.
func realStreakChecker(t *testing.T, f Fetcher) (*Checker, *cache.Cache) {
	t.Helper()
	c := simtest.OpenCache(t)
	seedStaleAnchorAt(t, c, "h", "/r/dists/x")
	chk, err := New(Config{Cache: c, Fetcher: f, HostLimiter: hostsem.New(8), Logger: simtest.DiscardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	return chk, c
}

func realRow(t *testing.T, c *cache.Cache) cache.SuiteFreshness {
	t.Helper()
	s, err := c.GetSuiteFreshness(context.Background(), "http", "h", "/r/dists/x")
	if err != nil {
		t.Fatal(err)
	}
	return *s
}

func streakChecker(t *testing.T, f Fetcher) (*Checker, *fakeCache) {
	t.Helper()
	fc := newFakeCache()
	seedFakeAnchor(fc)
	fc.putSuite(cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"})
	c, err := New(Config{Cache: fc, Fetcher: f, HostLimiter: hostsem.New(8), Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	return c, fc
}

func streakOf(t *testing.T, fc *fakeCache) cache.SuiteFreshness {
	t.Helper()
	s, ok := fc.suite("http", "h", "/r/dists/x")
	if !ok {
		t.Fatal("suite row missing")
	}
	return s
}

// Spec C4: check_fail_count is the streak of checks that ended unknown;
// any answered check resets it.
func TestCheck_UnknownStreakCountsAndResets(t *testing.T) {
	up := &rigUpstream{bodies: map[string][]byte{}, sent: map[string][]string{}} // 404: unknown in phase B
	c, fc := streakChecker(t, up)
	for i := int64(1); i <= 3; i++ {
		c.Check(context.Background(), "http", "h", "/r/dists/x")
		if got := streakOf(t, fc).CheckFailCount; got != i {
			t.Fatalf("after failure %d: streak = %d", i, got)
		}
	}
	up.set([]byte("new"))
	c.Check(context.Background(), "http", "h", "/r/dists/x")
	if got := streakOf(t, fc).CheckFailCount; got != 0 {
		t.Fatalf("after an answered check: streak = %d, want 0", got)
	}
}

type oddStatusUpstream struct{}

func (oddStatusUpstream) Conditional(context.Context, *fetch.Target, string, string, int64) (*fetch.ConditionalResult, error) {
	return &fetch.ConditionalResult{Status: 206}, nil
}

func TestCheck_UnexpectedStatusExtendsTheStreak(t *testing.T) {
	c, fc := streakChecker(t, oddStatusUpstream{})
	c.Check(context.Background(), "http", "h", "/r/dists/x")
	if got := streakOf(t, fc).CheckFailCount; got != 1 {
		t.Fatalf("streak = %d, want 1", got)
	}
}

func TestCheck_PanicExtendsTheStreak(t *testing.T) {
	up := &changedUpstream{body: []byte("x")}
	up.panic.Store(true)
	c, fc := streakChecker(t, up)
	c.Check(context.Background(), "http", "h", "/r/dists/x")
	if s := streakOf(t, fc); s.CheckFailCount != 1 || s.LastCheckAt == nil {
		t.Fatalf("streak = %d, last_check_at = %v; want 1 and set", s.CheckFailCount, s.LastCheckAt)
	}
}

// Spec C4: the upstream part of every check runs under a 2-minute
// deadline (the D of the scheduler's latency bound); hitting it is an
// unknown end, and it is recorded: against the real cache, whose write
// would fail if it inherited the expired deadline.
func TestCheck_DeadlineBoundsAStalledUpstream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		up := &rigUpstream{bodies: map[string][]byte{}, sent: map[string][]string{}}
		up.setStall(true)
		if checkDeadline != 2*time.Minute {
			t.Fatalf("checkDeadline = %v, want 2m (spec C4/C11)", checkDeadline)
		}
		chk, c := realStreakChecker(t, up)
		start := time.Now()
		chk.Check(context.Background(), "http", "h", "/r/dists/x")
		if d := time.Since(start); d != checkDeadline {
			t.Fatalf("check took %v, want %v", d, checkDeadline)
		}
		if s := realRow(t, c); s.CheckFailCount != 1 || s.LastCheckAt == nil {
			t.Fatalf("streak %d last_check_at %v; the deadline-hit check was not recorded", s.CheckFailCount, s.LastCheckAt)
		}
	})
}

// The detached form keeps the streak too.
func TestCheck_DetachedUnknownEndExtendsTheStreak(t *testing.T) {
	fc := newFakeCache()
	bh := testHex64
	for _, p := range []string{"/r/dists/x/Release", "/r/dists/x/Release.gpg"} {
		fc.putURL(cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: p, BlobHash: &bh,
			UpstreamURL: "http://h" + p, IsMetadata: true})
	}
	fc.putSuite(cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"})
	up := &rigUpstream{bodies: map[string][]byte{}, sent: map[string][]string{}} // 404 everywhere
	c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8), Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	c.Check(context.Background(), "http", "h", "/r/dists/x")
	if up.callsToRelease() == 0 {
		t.Fatal("the check did not take the detached path")
	}
	if got := streakOf(t, fc).CheckFailCount; got != 1 {
		t.Fatalf("detached streak = %d, want 1", got)
	}
}

func (u *rigUpstream) callsToRelease() int { return len(u.etagsSent("http://h/r/dists/x/Release")) }

// A cancelled caller (shutdown) says nothing about upstream: no failure.
func TestCheck_CancelledCallerRecordsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		up := &rigUpstream{bodies: map[string][]byte{}, sent: map[string][]string{}}
		up.setStall(true)
		chk, c := realStreakChecker(t, up)
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(time.Second, cancel)
		chk.Check(ctx, "http", "h", "/r/dists/x")
		if s := realRow(t, c); s.CheckFailCount != 0 || s.LastCheckAt != nil {
			t.Fatalf("cancelled check recorded streak %d, last_check_at %v", s.CheckFailCount, s.LastCheckAt)
		}
	})
}

// Both forms. checkLockedInline and checkLockedDetached carry the same
// streak and deadline edits; these cover each copy.

var streakForms = []string{"inline", "detached"}

// seedFakeForm gives the fake suite /r/dists/x on host h stale anchor rows
// for form (inline: InRelease; detached: Release and Release.gpg, which
// the first-ever check reaches through the inline fallback) and a suite
// row starting at streak.
func seedFakeForm(fc *fakeCache, form string, streak int64) {
	bh := testHex64
	paths := []string{"/r/dists/x/InRelease"}
	if form == "detached" {
		paths = []string{"/r/dists/x/Release", "/r/dists/x/Release.gpg"}
	}
	for _, p := range paths {
		fc.putURL(cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: p, BlobHash: &bh,
			UpstreamURL: "http://h" + p, IsMetadata: true})
	}
	fc.putSuite(cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", CheckFailCount: streak})
}

func formChecker(t *testing.T, form string, streak int64, f Fetcher, logger *slog.Logger) (*Checker, *fakeCache) {
	t.Helper()
	fc := newFakeCache()
	seedFakeForm(fc, form, streak)
	c, err := New(Config{Cache: fc, Fetcher: f, HostLimiter: hostsem.New(8), Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	return c, fc
}

func TestCheck_AnsweredCheckResetsTheStreakBothForms(t *testing.T) {
	for _, form := range streakForms {
		t.Run(form, func(t *testing.T) {
			up := &rigUpstream{bodies: map[string][]byte{}, sent: map[string][]string{}}
			for _, u := range []string{"http://h/r/dists/x/InRelease", "http://h/r/dists/x/Release", "http://h/r/dists/x/Release.gpg"} {
				up.setAt(u, []byte("new"))
			}
			c, fc := formChecker(t, form, 2, up, discardLogger())
			c.Check(context.Background(), "http", "h", "/r/dists/x")
			if form == "detached" && up.callsToRelease() == 0 {
				t.Fatal("the check did not take the detached path")
			}
			if s := streakOf(t, fc); s.CheckFailCount != 0 || s.LastSuccessAt == nil {
				t.Fatalf("streak = %d, last_success_at = %v; want 0 and set", s.CheckFailCount, s.LastSuccessAt)
			}
		})
	}
}

// The unexpected-status arm runs after the optimistic success bump: it
// must extend the streak the check started with, not restart it at 1.
func TestCheck_UnexpectedStatusExtendsAnExistingStreakBothForms(t *testing.T) {
	for _, form := range streakForms {
		t.Run(form, func(t *testing.T) {
			c, fc := formChecker(t, form, 2, oddStatusUpstream{}, discardLogger())
			c.Check(context.Background(), "http", "h", "/r/dists/x")
			if s := streakOf(t, fc); s.CheckFailCount != 3 || s.LastSuccessAt != nil {
				t.Fatalf("streak = %d, last_success_at = %v; want 3 and unset", s.CheckFailCount, s.LastSuccessAt)
			}
		})
	}
}

// The deadline bounds both forms' conditional GETs, and the deadline-hit
// check is recorded (fakeCache, like the real cache, refuses a write
// handed a finished context).
func TestCheck_DeadlineBoundsBothForms(t *testing.T) {
	for _, form := range streakForms {
		t.Run(form, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				up := &rigUpstream{bodies: map[string][]byte{}, sent: map[string][]string{}}
				up.setStall(true)
				c, fc := formChecker(t, form, 2, up, discardLogger())
				start := time.Now()
				c.Check(context.Background(), "http", "h", "/r/dists/x")
				if d := time.Since(start); d != checkDeadline {
					t.Fatalf("check took %v, want %v", d, checkDeadline)
				}
				if form == "detached" && up.callsToRelease() == 0 {
					t.Fatal("the check did not take the detached path")
				}
				if s := streakOf(t, fc); s.CheckFailCount != 3 || s.LastCheckAt == nil {
					t.Fatalf("streak %d last_check_at %v; the deadline-hit check was not recorded", s.CheckFailCount, s.LastCheckAt)
				}
			})
		})
	}
}

// cancellingUpstream cancels the caller's context mid-request, as a
// shutdown would.
type cancellingUpstream struct{ cancel context.CancelFunc }

func (u cancellingUpstream) Conditional(ctx context.Context, _ *fetch.Target, _, _ string, _ int64) (*fetch.ConditionalResult, error) {
	u.cancel()
	return nil, ctx.Err()
}

// The cancelled-caller branch: the check is abandoned, not counted as
// failed. (Against a cache the bookkeeping write fails anyway; the log is
// what tells the branch apart.)
func TestCheck_CancelledCallerIsNotAFailedCheckBothForms(t *testing.T) {
	for _, form := range streakForms {
		t.Run(form, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			logs := &lockedBuffer{}
			c, _ := formChecker(t, form, 0, cancellingUpstream{cancel: cancel},
				slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			c.Check(ctx, "http", "h", "/r/dists/x")
			if out := logs.String(); !strings.Contains(out, "freshness: check cancelled") || strings.Contains(out, "freshness check failed") {
				t.Fatalf("cancelled check not abandoned as such; logs:\n%s", out)
			}
		})
	}
}
