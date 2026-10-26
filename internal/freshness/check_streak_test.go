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

// Both forms. checkInline and checkDetached carry the same
// streak and deadline edits; these cover each copy.

var streakForms = []string{"inline", "detached"}

// formAnchorRows are stale anchor url_path rows of suite /r/dists/x on
// host h for form (inline: InRelease; detached: Release and Release.gpg,
// which the first-ever check reaches through the inline fallback).
func formAnchorRows(form string) []cache.URLPath {
	bh := testHex64
	paths := []string{"/r/dists/x/InRelease"}
	if form == "detached" {
		paths = []string{"/r/dists/x/Release", "/r/dists/x/Release.gpg"}
	}
	rows := make([]cache.URLPath, 0, len(paths))
	for _, p := range paths {
		rows = append(rows, cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: p, BlobHash: &bh,
			UpstreamURL: "http://h" + p, IsMetadata: true})
	}
	return rows
}

// seedFakeForm gives the fake suite form's anchor rows and a suite row
// starting at streak.
func seedFakeForm(fc *fakeCache, form string, streak int64) {
	for _, u := range formAnchorRows(form) {
		fc.putURL(u)
	}
	fc.putSuite(cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", CheckFailCount: streak})
}

// realFormChecker runs a checker for form over the real cache (whose
// writes honour their context), with sem as its host limiter and a Debug
// log into logs.
func realFormChecker(t *testing.T, form string, f Fetcher, sem *hostsem.Sem, logs *lockedBuffer) (*Checker, *cache.Cache) {
	t.Helper()
	ctx := context.Background()
	c := simtest.OpenCache(t)
	if err := c.PutBlob(ctx, testHex64, 1); err != nil {
		t.Fatal(err)
	}
	for _, u := range formAnchorRows(form) {
		if err := c.PutURLPath(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}); err != nil {
		t.Fatal(err)
	}
	chk, err := New(Config{Cache: c, Fetcher: f, HostLimiter: sem,
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	if err != nil {
		t.Fatal(err)
	}
	return chk, c
}

// hasFailedLine reports whether the text log holds a "freshness check
// failed" line for form with result=failed.
func hasFailedLine(logs, form string) bool {
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, `msg="freshness check failed"`) && strings.Contains(line, "form="+form) &&
			strings.Contains(line, "result=failed") {
			return true
		}
	}
	return false
}

// holdHostSlot takes the only slot of a one-slot limiter for host h; the
// slot stays held until the test ends.
func holdHostSlot(t *testing.T) *hostsem.Sem {
	t.Helper()
	sem := hostsem.New(1)
	release, err := sem.Acquire(context.Background(), "h")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	return sem
}

// Spec C4: the check deadline hit while waiting for the host slot is an
// unknown end like any other, so a host whose slots stay saturated moves
// its suites to the scheduler's failing class.
func TestCheck_DeadlineAtTheHostSlotIsAnUnknownEndBothForms(t *testing.T) {
	for _, form := range streakForms {
		t.Run(form, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				sem := holdHostSlot(t)
				up := &changedUpstream{body: []byte("new")}
				logs := &lockedBuffer{}
				chk, c := realFormChecker(t, form, up, sem, logs)
				start := time.Now()
				chk.Check(context.Background(), "http", "h", "/r/dists/x")
				if d := time.Since(start); d != checkDeadline {
					t.Fatalf("check took %v, want %v", d, checkDeadline)
				}
				if n := up.calls.Load(); n != 0 {
					t.Fatalf("upstream contacted %d times without a host slot", n)
				}
				if s := realRow(t, c); s.CheckFailCount != 1 || s.LastCheckAt == nil || s.LastSuccessAt != nil {
					t.Fatalf("streak %d last_check_at %v last_success_at %v; want 1, set, unset",
						s.CheckFailCount, s.LastCheckAt, s.LastSuccessAt)
				}
				if !hasFailedLine(logs.String(), form) {
					t.Fatalf("no failed-check line for form %s; logs:\n%s", form, logs.String())
				}
			})
		})
	}
}

// A caller cancelled while the check waits for the host slot records
// nothing. (The real cache would refuse the write anyway; the log tells
// the branch apart.)
func TestCheck_CancelledCallerAtTheHostSlotRecordsNothingBothForms(t *testing.T) {
	for _, form := range streakForms {
		t.Run(form, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				sem := holdHostSlot(t)
				logs := &lockedBuffer{}
				chk, c := realFormChecker(t, form, &changedUpstream{body: []byte("new")}, sem, logs)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				time.AfterFunc(time.Second, cancel)
				start := time.Now()
				chk.Check(ctx, "http", "h", "/r/dists/x")
				if d := time.Since(start); d != time.Second {
					t.Fatalf("check took %v, want 1s (the cancellation)", d)
				}
				if s := realRow(t, c); s.CheckFailCount != 0 || s.LastCheckAt != nil {
					t.Fatalf("cancelled check recorded streak %d, last_check_at %v", s.CheckFailCount, s.LastCheckAt)
				}
				if out := logs.String(); strings.Contains(out, "freshness check failed") {
					t.Fatalf("cancelled check logged as failed; logs:\n%s", out)
				}
			})
		})
	}
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
// must extend the streak the check started with, not restart it at 1,
// and keep the previous success stamp (a 206 must not make the suite
// look never-succeeded).
func TestCheck_UnexpectedStatusExtendsTheStreakAndKeepsSuccessBothForms(t *testing.T) {
	for _, form := range streakForms {
		t.Run(form, func(t *testing.T) {
			c, fc := formChecker(t, form, 2, oddStatusUpstream{}, discardLogger())
			prev := int64(1_700_000_000)
			s := streakOf(t, fc)
			s.LastSuccessAt = &prev
			fc.putSuite(s)
			c.Check(context.Background(), "http", "h", "/r/dists/x")
			if s := streakOf(t, fc); s.CheckFailCount != 3 || i64Of(s.LastSuccessAt) != prev {
				t.Fatalf("streak = %d, last_success_at = %d; want 3 and %d", s.CheckFailCount, i64Of(s.LastSuccessAt), prev)
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
// what tells the branch apart.) The line names the suite's scheme, in its
// twin freshness_check_ended_by_shutdown's key order, so an http suite's
// line is told from its https namesake's (the H1 final review's prod
// minor 4).
func TestCheck_CancelledCallerIsNotAFailedCheckBothForms(t *testing.T) {
	for _, form := range streakForms {
		t.Run(form, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			logs := &lockedBuffer{}
			c, _ := formChecker(t, form, 0, cancellingUpstream{cancel: cancel},
				slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			c.Check(ctx, "http", "h", "/r/dists/x")
			const line = "msg=freshness_check_cancelled canonical_scheme=http canonical_host=h suite_path=/r/dists/x"
			if out := logs.String(); !strings.Contains(out, line) || strings.Contains(out, "freshness check failed") {
				t.Fatalf("cancelled check not abandoned as such; logs:\n%s", out)
			}
		})
	}
}
