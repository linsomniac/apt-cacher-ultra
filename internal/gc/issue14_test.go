package gc

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// Issue #14's tests (the hardening plan's final review 2, G2-I1 and
// G2-I2): every pass gets its turn in every tick, and the retired backlog
// stays bounded.

// insertURLPathRow inserts a url_path row at path, last requested at
// lastRequested (1: long expired), holding no blob.
func insertURLPathRow(t *testing.T, db *sql.DB, path string, lastRequested int64) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO url_path
  (canonical_scheme, canonical_host, path, upstream_url, is_metadata, last_requested_at)
  VALUES ('http', 'example.test', ?, ?, 0, ?)`, path, "http://example.test"+path, lastRequested); err != nil {
		t.Fatal(err)
	}
}

// The tick budget splits into three deadlines, with f = max_tick_duration/5:
// the url-path pass ends by T-2f, the snapshot pass by T-f, the blob pass by
// T, so the passes after one that runs out keep a share of their own.
func TestPassDeadlines_SplitTheBudget(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	for _, tc := range []struct {
		budget                  time.Duration
		urlPath, snapshot, blob time.Duration
	}{
		{5 * time.Minute, 3 * time.Minute, 4 * time.Minute, 5 * time.Minute},
		{time.Minute, 36 * time.Second, 48 * time.Second, time.Minute},
	} {
		d := passDeadlines(start, tc.budget)
		if got := [3]time.Duration{d.urlPath.Sub(start), d.snapshot.Sub(start), d.blob.Sub(start)}; got !=
			[3]time.Duration{tc.urlPath, tc.snapshot, tc.blob} {
			t.Errorf("budget %s: deadlines at %v after the start, want [%s %s %s]", tc.budget, got, tc.urlPath, tc.snapshot, tc.blob)
		}
	}
}

// Issue #14 (G2-I1, the lens's probe P1 as a unit test): while sub-job C's
// backlog outlasts every tick's budget, a snapshot pass that hits its
// deadline no longer ends the tick. The rotation moves past it, as for a
// failed pass (ruling HD-15), and the blob pass runs in the same tick under
// its own share (here 60 s - 48 s): a dead blob is reaped in the first
// tick, the next tick begins at the url-path pass again, which reaps the
// expired row seeded for it, and C goes on draining, five 10 s chunks a
// tick. Before the fix each tick ended in the snapshot pass: no blob pass,
// and no url-path pass after the first.
func TestRunTick_ASnapshotDeadlineDoesNotDeferTheBlobPass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger, buf := captureLogger()
		f := &fakeStore{chunksMore: 900, chunkSleep: 10 * time.Second}
		g, c := storeGC(t, f, 2, logger)
		g.cfg.URLPathTTL = time.Hour
		db := dbOf(t, c)
		dead := seedReapableBlob(t, c, "an expired .deb nothing references")
		chunks := 0
		for tick := 1; tick <= 2; tick++ {
			insertURLPathRow(t, db, fmt.Sprintf("/old-%d.deb", tick), 1)
			res, err := g.runTick(context.Background(), "periodic")
			if err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			ran := len(f.chunkRows) - chunks
			chunks = len(f.chunkRows)
			f.mu.Unlock()
			if res.urlPathRowsReaped != 1 {
				t.Fatalf("tick %d reaped %d url_path rows, want 1: the url-path pass did not get its turn", tick, res.urlPathRowsReaped)
			}
			if !res.deadlineReached || ran != 5 {
				t.Fatalf("tick %d: deadline reached %v, %d sub-job C chunks; want the snapshot deadline after 5", tick, res.deadlineReached, ran)
			}
			if want := map[int]int{1: 1, 2: 0}[tick]; res.blobsReaped != want {
				t.Fatalf("tick %d reaped %d blobs, want %d: the blob pass did not run after the snapshot deadline", tick, res.blobsReaped, want)
			}
			if g.nextPass != urlPathPass {
				t.Fatalf("after tick %d the rotation is at pass %d, want the url-path pass (%d)", tick, g.nextPass, urlPathPass)
			}
		}
		if blobRowExists(t, db, dead) {
			t.Fatal("the dead blob survived two ticks")
		}
		for _, ev := range gcEvents(t, buf, "gc_tick_deadline_reached") {
			if ev["which"] != "snapshot" {
				t.Fatalf("deadline line %v: want only the snapshot pass's", ev)
			}
		}
	})
}

// Issue #14 (G2-I1), the url-path pass, at a start and in a periodic tick:
// a url_path scan longer than its share (36 s of a 1 min budget; six expired
// rows, one 10 s batch each) saves its cursor at its deadline, after four,
// and no longer ends the tick: the snapshot pass runs and the blob pass reaps a dead blob in
// the same start or tick. The url-path cursor lives in memory, so before
// the fix a daemon restarted more often than gc.interval whose scan
// outlasted the budget rescanned the same prefix at every start and never
// reached the snapshot or blob pass. The next tick resumes the scan at the
// cursor and drains it.
func TestRunTick_AURLPathDeadlineDoesNotDeferTheLaterPasses(t *testing.T) {
	for _, phase := range []string{phaseStartup, "periodic"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				logger, buf := captureLogger()
				f := &fakeStore{urlSleep: 10 * time.Second}
				g, c := storeGC(t, f, 2, logger)
				g.cfg.URLPathTTL = time.Hour
				g.cfg.BatchSize = 1
				db := dbOf(t, c)
				for i := range 6 {
					insertURLPathRow(t, db, fmt.Sprintf("/old-%d.deb", i), 1)
				}
				dead := seedReapableBlob(t, c, "a dead blob")
				if phase == phaseStartup {
					if err := g.StartupPass(context.Background()); err != nil {
						t.Fatal(err)
					}
				} else {
					g.periodicTick(context.Background())
				}
				s, _ := g.LastRunSummary()
				if s.Phase != phase || !s.DeadlineReached || s.URLPathRowsScanned != 4 || s.URLPathRowsReaped != 4 {
					t.Fatalf("summary %+v: want the %s pass's, its url-path deadline reached after 4 rows, each reaped", s, phase)
				}
				if f.batches == 0 {
					t.Fatal("the snapshot pass did not run after the url-path deadline")
				}
				if blobRowExists(t, db, dead) || s.BlobsReaped != 1 {
					t.Fatalf("blobs reaped %d: the blob pass did not run after the url-path deadline", s.BlobsReaped)
				}
				if g.nextPass != urlPathPass || g.urlPathCursor.path != "/old-3.deb" {
					t.Fatalf("rotation at pass %d, url-path cursor %+v; want the url-path pass, resuming after /old-3.deb",
						g.nextPass, g.urlPathCursor)
				}
				if ev := gcEvents(t, buf, "gc_tick_deadline_reached"); len(ev) != 1 || ev[0]["which"] != "url_path" {
					t.Fatalf("deadline lines %v, want the url-path pass's alone", ev)
				}
				g.periodicTick(context.Background())
				if s, _ := g.LastRunSummary(); s.URLPathRowsScanned != 2 || s.DeadlineReached || g.urlPathCursor.path != "" {
					t.Fatalf("next tick %+v, cursor %+v: want the scan resumed at the cursor and drained (2 rows)", s, g.urlPathCursor)
				}
			})
		})
	}
}

// backlogStore is fakeStore whose sub-job C works a retired backlog: each
// chunk takes chunkSleep and deletes one retired snapshot while any is left
// (1, more), and reports none left once it is empty (0, no more), as the
// cache's chunk does for small snapshots; and each periodic snapshot pass's
// sub-job B (its batch; at a start too since 2026-11-09) retires retiresPerPass more,
// as a periodic B retires the displaced snapshots of the day's adoptions.
type backlogStore struct {
	*fakeStore
	mu             sync.Mutex
	backlog        int
	maxBacklog     int
	retiresPerPass int
}

func (s *backlogStore) RunSnapshotGCBatch(ctx context.Context, n int, grace int64, keep int) (cache.SnapshotGCBatchResult, error) {
	s.mu.Lock()
	s.backlog += s.retiresPerPass
	s.maxBacklog = max(s.maxBacklog, s.backlog)
	s.mu.Unlock()
	return s.fakeStore.RunSnapshotGCBatch(ctx, n, grace, keep)
}

func (s *backlogStore) RunRetiredSnapshotGCChunk(context.Context, int) (int, bool, error) {
	time.Sleep(s.chunkSleep)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.backlog == 0 {
		return 0, false, nil
	}
	s.backlog--
	return 1, true, nil
}

func (s *backlogStore) left() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.backlog
}

// runFor runs g.Run for d of virtual time and stops it.
func runFor(g *GC, d time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { g.Run(ctx); close(done) }()
	time.Sleep(d)
	cancel()
	<-done
}

// Issue #14 (G2-I2, the cadence): while retiredRun's run ends at its
// deadline with retired snapshots left, Run repeats it an hour later, until
// one drains them. With five left and two 30 s chunks a 1 min run: runs an
// hour, two and three hours in (2, 2, then 1 and none left). Before the fix
// the run came once a process. Since 2026-11-09 (issue #14 follow-up Tier 2)
// the run goes on to sub-job D after C; the third run's C finds none left
// exactly at its deadline, so D starts there, its deadline reached before it
// could look (rows may be left), and the run re-arms once more: a fourth,
// four hours in, finds nothing for either and ends it.
func TestRun_RepeatsTheRetiredRunHourlyWhileABacklogRemains(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger, buf := captureLogger()
		g, _ := storeGC(t, &fakeStore{chunksMore: 5, chunkSleep: 30 * time.Second}, 2, logger)
		g.cfg.Interval = 24 * time.Hour
		runFor(g, 6*time.Hour)
		var got []string
		for _, r := range gcEvents(t, buf, "gc_retired_run_complete") {
			got = append(got, fmt.Sprintf("%v/%v", r["retired_reaped"], r["deadline_reached"]))
		}
		if fmt.Sprint(got) != "[2/true 2/true 1/true 0/false]" {
			t.Fatalf("retired runs (retired_reaped/deadline_reached) %v, want [2/true 2/true 1/true 0/false]: hourly until drained", got)
		}
	})
}

// Issue #14 (G2-I2): a periodic tick whose snapshot pass hits its deadline
// (here sub-job C, behind the 10 its sub-job B retired: five 10 s chunks by
// its 48 s) arms the retired run an hour later, which deletes the other
// five, when gc.interval (2 h here) is longer than an hour; the run an hour
// after the start found nothing and did not re-arm.
func TestPeriodicTick_ASnapshotDeadlineArmsTheRetiredRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger, buf := captureLogger()
		s := &backlogStore{fakeStore: &fakeStore{chunkSleep: 10 * time.Second}, retiresPerPass: 10}
		g, _ := storeGC(t, s, 2, logger)
		g.cfg.Interval = 2 * time.Hour
		runFor(g, 3*time.Hour+30*time.Minute)
		runs := gcEvents(t, buf, "gc_retired_run_complete")
		if len(runs) != 2 || runs[0]["retired_reaped"] != float64(0) || runs[1]["retired_reaped"] != float64(5) {
			t.Fatalf("retired runs %v, want two: an hour after the start (none left), and an hour after the tick (5)", runs)
		}
		if ticks := gcEvents(t, buf, "gc_run_complete"); len(ticks) != 1 || ticks[0]["deadline_reached"] != true {
			t.Fatalf("periodic ticks %v, want one, which reached its snapshot deadline", ticks)
		}
	})
}

// Issue #14 (G2-I2), no retry storm: a retired run that fails is not
// repeated hourly (the next periodic tick retries C, as before): one run,
// one gc_tick_failed, in five hours.
func TestRun_AFailedRetiredRunIsNotRepeatedHourly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger, buf := captureLogger()
		g, _ := storeGC(t, &fakeStore{chunkErrs: true}, 2, logger)
		g.cfg.Interval = 24 * time.Hour
		runFor(g, 5*time.Hour)
		if runs := gcEvents(t, buf, "gc_retired_run_complete"); len(runs) != 1 {
			t.Fatalf("retired runs %v, want one: a failed run is not repeated", runs)
		}
		if failed := gcEvents(t, buf, "gc_tick_failed"); len(failed) != 1 {
			t.Fatalf("gc_tick_failed lines %v, want one", failed)
		}
	})
}

// Issue #14 (G2-I2, the lens's probe P3 scaled down): each daily periodic
// tick's sub-job B retires 40 displaced snapshots, and one snapshot pass's C
// deletes 5 (five 10 s chunks by its 48 s; a chunk shorter than the blob
// pass's 12 s floor, as real chunks are), about the capacity C had a day
// before the fix, when the retired run (6 a run) came once a process. The
// hourly retired runs drain the rest within the day, so 20 hours after each
// of four ticks nothing retired is left, and the backlog never exceeds one
// day's 40. Before the fix it grew 35 a day.
func TestRun_TheRetiredBacklogStaysBoundedUnderASustainedDisplacementRate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger, _ := captureLogger()
		s := &backlogStore{fakeStore: &fakeStore{chunkSleep: 10 * time.Second}, retiresPerPass: 40}
		g, _ := storeGC(t, s, 2, logger)
		g.cfg.Interval = 24 * time.Hour
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { g.Run(ctx); close(done) }()
		time.Sleep(24*time.Hour + 20*time.Hour)
		var left []int
		for range 4 {
			left = append(left, s.left())
			time.Sleep(24 * time.Hour)
		}
		cancel()
		<-done
		if fmt.Sprint(left) != "[0 0 0 0]" || s.maxBacklog > 40 {
			t.Fatalf("retired left 20 h after each tick %v, at most %d; want [0 0 0 0], at most 40", left, s.maxBacklog)
		}
	})
}

// Issue #14 (G2-I2; ruling PF-15 re-ruled by the maintainer, 2026-11-05):
// the startup pass runs sub-job C after A ∪ B, within its snapshot
// deadline (48 s of a 1 min budget: five 10 s chunks of a backlog far
// larger), and its blob pass still reaps a dead blob, so the start ends
// within max_tick_duration (the bound HD-7 accepts) with the deadline
// reported; the retired run an hour later goes on with the backlog.
// TestStartupPass_DeletesRetiredSnapshotsAndReapsTheirBlobs pins what it
// deletes on a real cache.
func TestStartupPass_RunsSubJobCWithinItsSnapshotDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger, buf := captureLogger()
		f := &fakeStore{chunksMore: 900, chunkSleep: 10 * time.Second}
		g, c := storeGC(t, f, 2, logger)
		g.cfg.Interval = 24 * time.Hour
		dead := seedReapableBlob(t, c, "a dead blob")
		start := time.Now()
		if err := g.StartupPass(context.Background()); err != nil {
			t.Fatal(err)
		}
		if took := time.Since(start); took > time.Minute {
			t.Fatalf("the startup pass took %s, want at most max_tick_duration (1m)", took)
		}
		s, _ := g.LastRunSummary()
		if len(f.chunkRows) != 5 || s.RetiredReaped != 5 || !s.DeadlineReached {
			t.Fatalf("%d sub-job C chunks, summary %+v; want 5 chunks and retired_reaped 5 by the snapshot deadline", len(f.chunkRows), s)
		}
		if blobRowExists(t, dbOf(t, c), dead) || s.BlobsReaped != 1 {
			t.Fatalf("blobs reaped %d: the startup pass's blob pass did not run after sub-job C's deadline", s.BlobsReaped)
		}
		runFor(g, time.Hour+time.Minute)
		if runs := gcEvents(t, buf, "gc_retired_run_complete"); len(runs) != 1 || runs[0]["retired_reaped"] != float64(6) {
			t.Fatalf("retired runs %v, want the one an hour after the start, going on with the backlog (6 chunks)", runs)
		}
	})
}
