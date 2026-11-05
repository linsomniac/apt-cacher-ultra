package gc

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"testing/synctest"
	"time"
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
