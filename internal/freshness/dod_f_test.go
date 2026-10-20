package freshness

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/gc"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// simReapEvery is the ninety-day simulation's reaper interval (deliberate
// deviation 2): six hours, not the hourly reapInterval, so ninety days fit
// the 5 s -race budget; the uptime floor stays one hour. Task 6 and
// TestDoD_ProductionCadenceAcrossTwelveHourlyRestarts pin the hourly
// cadence.
const simReapEvery = 6 * time.Hour

// simCadence is a simulation boot's reaper interval and gc.url_path_ttl.
type simCadence struct {
	reapEvery  time.Duration
	urlPathTTL time.Duration
}

var (
	// coarseCadence is deliberate deviation 2's: the reaper every six
	// hours, no url_path TTL pass.
	coarseCadence = simCadence{reapEvery: simReapEvery}
	// productionCadence is the shipped one: the reaper hourly
	// (reapInterval), gc.url_path_ttl at its default, 168h.
	productionCadence = simCadence{reapEvery: reapInterval, urlPathTTL: 168 * time.Hour}
)

// simBoot is one start of the suite lifecycle over the cache in dir: the
// adopter, the checker (whose scheduler does not run: the simulation's
// clients adopt through the adopter, as a T1 check would), the reaper (its
// one-hour floor, then every reapEvery of its cadence) and GC
// (gc.interval = 24h after its startup pass; the url_path TTL pass only at
// the production cadence, deliberate deviation 2) under one lifetime.
type simBoot struct {
	c       *cache.Cache
	ad      *Adopter
	checker *Checker
	reaper  *Reaper
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// startSimBoot starts a boot at the coarse cadence.
func startSimBoot(t *testing.T, dir string, members *fakeFetcher, logs *lockedBuffer) *simBoot {
	t.Helper()
	return startSimBootAt(t, dir, members, logs, coarseCadence)
}

// startSimBootAt starts a boot at cad.
func startSimBootAt(t *testing.T, dir string, members *fakeFetcher, logs *lockedBuffer, cad simCadence) *simBoot {
	t.Helper()
	ctx := context.Background()
	c, err := cache.Open(ctx, dir, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	b := &simBoot{c: c}
	if b.ad, err = NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
		HostLimiter: hostsem.New(8), Logger: discardLogger()}); err != nil {
		t.Fatal(err)
	}
	if b.checker, err = New(Config{Cache: c, Fetcher: &simtest.StaticConditional{Status: 304}, HostLimiter: hostsem.New(8),
		Adopter: b.ad, Logger: logger}); err != nil {
		t.Fatal(err)
	}
	if b.reaper, err = NewReaper(ReaperConfig{Cache: c, Checker: b.checker, IdleTTL: 30 * 24 * time.Hour, Logger: logger}); err != nil {
		t.Fatal(err)
	}
	b.reaper.interval = cad.reapEvery
	g, err := gc.New(gc.Config{Cache: c, Logger: discardLogger(), Enabled: true, Interval: 24 * time.Hour, BatchSize: 100,
		SnapshotBatchSize: 10, MaxTickDuration: 5 * time.Minute, BlobGrace: 5 * time.Minute, KeepDisplaced: 3,
		PoolScanWorkers: 1, HeartbeatStaleGrace: 30 * time.Minute, URLPathTTL: cad.urlPathTTL, HoldWindow: 24 * time.Hour,
		MaxVersionsPerPackage: 3})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.StartupPass(ctx); err != nil {
		t.Fatal(err)
	}
	life, cancel := context.WithCancel(ctx)
	b.cancel = cancel
	b.wg.Add(2)
	go func() { defer b.wg.Done(); b.reaper.Run(life) }()
	go func() { defer b.wg.Done(); g.Run(life) }()
	return b
}

// stop ends the boot: its loops, then the cache.
func (b *simBoot) stop(t *testing.T) {
	t.Helper()
	b.cancel()
	b.wg.Wait()
	if err := b.c.Close(); err != nil {
		t.Fatal(err)
	}
}

func simKey(suite string) cache.SuiteKey {
	return cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: suite}
}

// exists reports whether suite has a row.
func (b *simBoot) exists(t *testing.T, suite string) bool {
	t.Helper()
	ok, err := b.c.HasSuiteRow(context.Background(), simKey(suite))
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// use is a client's apt update of suite: a hit touches its row; a miss —
// the suite has no row — creates it, and its T1 check adopts the upstream
// InRelease at once. It reports whether the suite was (re)created.
func (b *simBoot) use(t *testing.T, suite string, inRelease []byte) bool {
	t.Helper()
	ctx := context.Background()
	key := simKey(suite)
	if b.exists(t, suite) {
		if err := b.c.TouchSuite(ctx, key); err != nil {
			t.Fatal(err)
		}
		return false
	}
	res, err := b.c.CreateSuite(ctx, cache.SuiteSeed{SuiteKey: key, RequestedAt: wallclock.Unix()}, cache.SuiteCaps{}, b.checker)
	if err != nil || res.Outcome != cache.SuiteCreated {
		t.Fatalf("%s: CreateSuite = %+v, %v", suite, res, err)
	}
	if err := b.ad.Run(ctx, SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: suite}, inRelease, "", ""); err != nil {
		t.Fatalf("%s: adoption: %v", suite, err)
	}
	return true
}

// count runs a COUNT query on its own connection to the boot's cache.
func (b *simBoot) count(t *testing.T, query string) int {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(b.c.Dir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// simSuites builds the suites' upstream: each a clearsigned InRelease of
// its own (an anchor blob no other suite shares) over one shared Packages.
func simSuites(members *fakeFetcher, suites ...string) map[string][]byte {
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo_1.deb": testHex64})
	out := map[string][]byte{}
	for _, s := range suites {
		members.put("http://h"+s+"/main/binary-amd64/Packages", pkgs)
		out[s] = simtest.ClearsignedFixture(string(hashRelease("Origin: "+s+"\n", true, false,
			map[string][]byte{"main/binary-amd64/Packages": pkgs}, nil)))
	}
	return out
}

// reapedLines counts the suite_reaped lines in logs by suite path.
func reapedLines(logs *lockedBuffer) map[string]int {
	out := map[string]int{}
	for _, line := range strings.Split(logs.String(), "\n") {
		var m map[string]any
		if line != "" && json.Unmarshal([]byte(line), &m) == nil && m["msg"] == "suite_reaped" {
			out[m["suite_path"].(string)]++
		}
	}
	return out
}

// Spec §7 C9, Review Focus 1: a 30-day forward step of the wall clock
// makes every suite look idle. Taken across a restart (the cache was off,
// or its clock was set while it was), the reaper's first run, an uptime
// floor after the start, reaps them all — false reaps. Taken while the
// reaper runs (a resumed VM or laptop), the run that sees it reports
// clock_step_detected (stamp=wall_clock) and reaps nothing: reaping waits
// an uptime floor more (design review M-8), and the suites no client
// asked for meanwhile go at the next run. Each false reap self-heals at
// its client's next request: the miss re-creates the row and the
// unchanged content re-adopts at once, and the markers, which outlive
// reaping, keep the suites' signed history.
func TestDoD_ForwardStepFalseReapsSelfHeal(t *testing.T) {
	for _, atRestart := range []bool{true, false} {
		name := "while running"
		if atRestart {
			name = "at a restart"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := context.Background()
				dir := t.TempDir()
				members := newFakeFetcher()
				suites := []string{"/r/dists/s0", "/r/dists/s1", "/r/dists/s2"}
				inRelease := simSuites(members, suites...)
				logs := &lockedBuffer{}
				b := startSimBoot(t, dir, members, logs)
				defer func() { b.stop(t) }()
				for _, s := range suites {
					b.use(t, s, inRelease[s])
				}
				if atRestart {
					b.stop(t)
				}
				restore := wallclock.SetOffsetForTest(30*24*time.Hour + time.Minute)
				defer restore()
				if atRestart {
					b = startSimBoot(t, dir, members, logs)
				}
				time.Sleep(time.Hour + time.Minute) // past the uptime floor: the first run
				if !atRestart {
					for _, s := range suites {
						if !b.exists(t, s) {
							t.Fatalf("%s reaped by the run that saw the forward step", s)
						}
					}
					if !strings.Contains(logs.String(), `"msg":"clock_step_detected","stamp":"wall_clock"`) {
						t.Fatal("no clock_step_detected line for the wall clock's step")
					}
					time.Sleep(simReapEvery) // the next run, past the deferral
				}
				for _, s := range suites {
					if b.exists(t, s) {
						t.Fatalf("%s survived the forward step", s)
					}
					if _, ok, err := b.c.SignedSeen(ctx, "http", "h", s); err != nil || !ok {
						t.Fatalf("%s's marker did not outlive the false reap (%v, %v)", s, ok, err)
					}
				}
				for _, s := range suites {
					if !b.use(t, s, inRelease[s]) {
						t.Fatalf("%s: the client's return found a row", s)
					}
					sf, err := b.c.GetSuiteFreshness(ctx, "http", "h", s)
					if err != nil || sf.CurrentSnapshotID == nil {
						t.Fatalf("%s: not re-adopted (%+v, %v)", s, sf, err)
					}
					snap, err := b.c.GetSuiteSnapshot(ctx, *sf.CurrentSnapshotID)
					if err != nil || strOf(snap.SingleAnchorHash) != shaOf(inRelease[s]) {
						t.Fatalf("%s: re-adopted %+v, %v; want its unchanged anchor", s, snap, err)
					}
				}
				if got := reapedLines(logs); len(got) != len(suites) {
					t.Fatalf("suite_reaped %v, want the three false reaps", got)
				}
			})
		})
	}
}

// Spec §7 C9, §9 F (deliberate deviation 2): ninety virtual days of the
// reaper and of GC at gc.interval = 24h, restarted every fifth day and an
// hour before three suites' deadlines (the uptime floor then delays their
// reap). Suites used daily are never reaped; one used again on day 29
// (near the TTL) lasts until day 59; a suite whose last use was day L is
// there on day L+30 and gone on day L+31 — a busy one too, whose check
// holds its mutex across the run that would reap it; its anchor blob is
// gone within two GC ticks; a reaped suite whose client returns re-adopts
// at once; a five-day backward clock step reaps nothing; at the end the
// suite tables and the in-memory maps hold the live suites only, and
// after every restart the goroutines are back at the first boot's count
// (design review M-6).
func TestDoD_NinetyDaysOfReapingWithRestarts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		members := newFakeFetcher()
		active := []string{"/r/dists/a0", "/r/dists/a1"}
		uses := map[string][]int{"/r/dists/b0": {0}, "/r/dists/b1": {0, 10}, "/r/dists/b2": {0, 20}, "/r/dists/n0": {0, 29}}
		last := map[string]int{"/r/dists/b0": 0, "/r/dists/b1": 10, "/r/dists/b2": 20, "/r/dists/n0": 29}
		const busy, returning, back = "/r/dists/b1", "/r/dists/c0", 45 // c0: used on day 0, back on day 45
		inRelease := simSuites(members, "/r/dists/a0", "/r/dists/a1", "/r/dists/b0", "/r/dists/b1", "/r/dists/b2",
			"/r/dists/n0", returning)
		logs := &lockedBuffer{}
		b := startSimBoot(t, dir, members, logs)
		defer func() { b.stop(t) }()
		base := baseGoroutines()
		restart := func() {
			b.stop(t)
			b = startSimBoot(t, dir, members, logs)
			if n := sampleGoroutines(base); n > base {
				t.Fatalf("after a restart: %d goroutines, the first boot had %d", n, base)
			}
		}
		for day := 0; day < 90; day++ {
			if day > 0 && day%5 == 0 { // 00:00, every fifth day
				restart()
			}
			time.Sleep(8 * time.Hour)
			if day == 30 || day == 40 || day == 50 { // 08:00, an hour before b0's, b1's and b2's deadlines
				restart()
			}
			time.Sleep(time.Hour)
			// 09:00: the clients.
			for _, s := range active {
				if created := b.use(t, s, inRelease[s]); created != (day == 0) {
					t.Fatalf("day %d: active suite %s created %v: reaped while in daily use", day, s, created)
				}
			}
			for s, days := range uses {
				for _, d := range days {
					if d == day {
						b.use(t, s, inRelease[s])
					}
				}
			}
			if day == 0 || day >= back {
				if created := b.use(t, returning, inRelease[returning]); created != (day == 0 || day == back) {
					t.Fatalf("day %d: %s created %v, want a row only on its first use and on its return", day, returning, created)
				}
			}
			if day == 31 && b.exists(t, returning) {
				t.Fatalf("day %d: %s not reaped", day, returning)
			}
			for s, l := range last {
				switch day {
				case l + 30:
					if !b.exists(t, s) {
						t.Fatalf("day %d: %s reaped 30 days after its last use, before the TTL", day, s)
					}
				case l + 31:
					if b.exists(t, s) {
						t.Fatalf("day %d: %s not reaped 31 days after its last use", day, s)
					}
				case l + 33:
					if ok, err := b.c.BlobExists(shaOf(inRelease[s])); err != nil || ok {
						t.Fatalf("day %d: %s's anchor blob outlived two GC ticks (%v, %v)", day, s, ok, err)
					}
				}
			}
			switch day {
			case last[busy] + 30:
				// The 08:00 restart puts the runs at 09:00 (the deadline:
				// not yet idle), 15:00 and 21:00. The busy suite's check
				// holds its mutex from 14:30 to 16:00, across the 15:00
				// run; the 21:00 run reaps it.
				time.Sleep(5*time.Hour + 30*time.Minute)
				release, ok := b.checker.HoldSuite(simKey(busy))
				if !ok {
					t.Fatalf("day %d: %s busy", day, busy)
				}
				time.Sleep(90 * time.Minute)
				if !b.exists(t, busy) {
					t.Fatalf("day %d: a held suite was reaped", day)
				}
				release(false)
				time.Sleep(8 * time.Hour)
			case 61:
				// 12:00: the wall clock steps five days back for twelve hours.
				time.Sleep(3 * time.Hour)
				restore := wallclock.SetOffsetForTest(-5 * 24 * time.Hour)
				time.Sleep(12 * time.Hour)
				restore()
			default:
				time.Sleep(15 * time.Hour)
			}
		}
		live := len(active) + 1
		if n := b.count(t, `SELECT COUNT(*) FROM suite_freshness`); n != live {
			t.Errorf("%d suite rows, want the %d live suites'", n, live)
		}
		if n := b.count(t, `SELECT COUNT(*) FROM suite_snapshot`); n != live {
			t.Errorf("%d suite_snapshot rows, want the %d live suites' current ones", n, live)
		}
		if b.checker.LockCount() > live || b.c.TouchedSuites() > live {
			t.Errorf("lock map %d, touch map %d entries; want at most %d", b.checker.LockCount(), b.c.TouchedSuites(), live)
		}
		want := map[string]int{"/r/dists/b0": 1, "/r/dists/b1": 1, "/r/dists/b2": 1, "/r/dists/n0": 1, returning: 1}
		got := reapedLines(logs)
		if len(got) != len(want) {
			t.Fatalf("suite_reaped %v, want once each for %v", got, want)
		}
		for s := range want {
			if got[s] != 1 {
				t.Fatalf("suite_reaped %v, want once each for %v", got, want)
			}
		}
	})
}

// Design review (deliberate deviation 2's other half): four virtual days
// at the production cadence — the reaper hourly after its one-hour floor,
// gc.url_path_ttl = 168h, a restart every twelve hours. A suite whose
// deadline falls 30 minutes before a restart, after the last run of the
// boot before, is reaped at the next boot's floor, never at its start; a
// suite whose anchor rows url_path GC deleted days
// before its deadline is still reaped, its marker kept; a suite in daily
// use is never reaped; after every restart the goroutines are back at the
// first boot's count.
func TestDoD_ProductionCadenceAcrossTwelveHourlyRestarts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		dir := t.TempDir()
		members := newFakeFetcher()
		const active, edge, gcd = "/r/dists/active", "/r/dists/edge", "/r/dists/gcd"
		inRelease := simSuites(members, active)
		logs := &lockedBuffer{}
		start := time.Now()
		ttl := 30 * 24 * time.Hour
		b := startSimBootAt(t, dir, members, logs, productionCadence)
		defer func() { b.stop(t) }()
		base := baseGoroutines()
		seed := func(suite string, requested time.Time) {
			t.Helper()
			if err := b.c.EnsureSuiteForTest(ctx, "http", "h", suite); err != nil {
				t.Fatal(err)
			}
			at := requested.Unix()
			if err := b.c.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h",
				SuitePath: suite, LastRequestedAt: &at}); err != nil {
				t.Fatal(err)
			}
		}
		// edge's deadline: 35 h 30 m, between the last run before the
		// restart at 36 h (35 h) and that restart.
		seed(edge, start.Add(35*time.Hour+30*time.Minute-ttl))
		// gcd's deadline: 54 h; its cached InRelease, unused for 8 days,
		// is past url_path_ttl, and its base was last seen signed then.
		seed(gcd, start.Add(54*time.Hour-ttl))
		old := start.Add(-8 * 24 * time.Hour).Unix()
		h, err := b.ad.writeBlobBytes(ctx, simtest.ClearsignedFixture("Origin: gcd\nSHA256:\n"))
		if err != nil {
			t.Fatal(err)
		}
		if err := b.c.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: gcd + "/InRelease",
			BlobHash: &h, UpstreamURL: "http://h" + gcd + "/InRelease", IsMetadata: true, RequestCount: 1,
			LastRequestedAt: &old, LastFetchedAt: &old}); err != nil {
			t.Fatal(err)
		}
		if err := b.c.MarkSignedSeenBatch(ctx, []cache.SignedSeenMark{{CanonicalScheme: "http", CanonicalHost: "h",
			Base: gcd, SeenAt: old}}); err != nil {
			t.Fatal(err)
		}
		gcdAnchor := func() bool {
			t.Helper()
			_, err := b.c.LookupURL(ctx, "http", "h", gcd+"/InRelease")
			if err != nil && !errors.Is(err, cache.ErrNotFound) {
				t.Fatal(err)
			}
			return err == nil
		}
		for half := 0; half < 8; half++ { // twelve hours each
			if half > 0 {
				b.stop(t)
				b = startSimBootAt(t, dir, members, logs, productionCadence)
				if n := sampleGoroutines(base); n > base {
					t.Fatalf("after the restart at %d h: %d goroutines, the first boot had %d", 12*half, n, base)
				}
			}
			for step := 0; step < 2; step++ { // the active suite's client, every six hours
				b.use(t, active, inRelease[active])
				switch {
				case half == 2 && step == 1: // 30 h; then up to 35 h 59 m: the boot's last run, at 35 h
					time.Sleep(5*time.Hour + 59*time.Minute)
					if !b.exists(t, edge) {
						t.Fatalf("%s reaped before its deadline", edge)
					}
					time.Sleep(time.Minute)
				case half == 3 && step == 0: // 36 h: the restart; the floor's run at 37 h
					time.Sleep(time.Hour - time.Minute)
					if !b.exists(t, edge) {
						t.Fatalf("%s reaped before the boot's uptime floor", edge)
					}
					time.Sleep(2 * time.Minute)
					if b.exists(t, edge) {
						t.Fatalf("%s not reaped at the boot's floor, 90 minutes past its deadline", edge)
					}
					time.Sleep(5*time.Hour - time.Minute)
				case half == 4 && step == 0: // 48 h: url_path GC has deleted gcd's anchor; gcd is still tracked
					if gcdAnchor() || !b.exists(t, gcd) {
						t.Fatalf("at 48 h: %s's anchor row %v, its row %v; want the anchor deleted by url_path GC, the row kept",
							gcd, gcdAnchor(), b.exists(t, gcd))
					}
					time.Sleep(6 * time.Hour)
				case half == 4 && step == 1: // 54 h: the deadline; the run at 55 h reaps it
					time.Sleep(30 * time.Minute)
					if !b.exists(t, gcd) {
						t.Fatalf("%s reaped before its deadline", gcd)
					}
					time.Sleep(time.Hour)
					if b.exists(t, gcd) {
						t.Fatalf("%s not reaped although url_path GC took its anchor", gcd)
					}
					if at, ok, err := b.c.SignedSeen(ctx, "http", "h", gcd); err != nil || !ok || at < old {
						t.Fatalf("%s's marker after the reap: %d, %v, %v; want it kept", gcd, at, ok, err)
					}
					time.Sleep(4*time.Hour + 30*time.Minute)
				default:
					time.Sleep(6 * time.Hour)
				}
			}
		}
		if !b.exists(t, active) {
			t.Fatalf("%s, in use every six hours, was reaped", active)
		}
		if got := reapedLines(logs); len(got) != 2 || got[edge] != 1 || got[gcd] != 1 {
			t.Fatalf("suite_reaped %v, want %s and %s once each", got, edge, gcd)
		}
	})
}
