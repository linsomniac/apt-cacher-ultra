//go:build linux

package cache

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// AIDEV-NOTE: a measurement, not a test: it asserts nothing about speed
// and is skipped unless ACU_MEASURE_DB names a cache.db to measure on (a
// copy of a production database; it is copied again, never opened in
// place). It reports what the schema migration and one adoption commit
// cost on real data: the numbers the package-list design's "Measurements"
// section quotes. Run it alone, on a quiet machine, more than once:
//
//	ACU_MEASURE_DB=/path/to/cache.db go test ./internal/cache/ -run TestMeasure_CommitOnARealDatabase -count=1 -v

// stepTimes records when each schema migration step started and ended.
type stepTimes struct {
	mu    sync.Mutex
	start map[int64]time.Time
	took  map[int64]time.Duration
}

func (s *stepTimes) Enabled(context.Context, slog.Level) bool { return true }
func (s *stepTimes) WithAttrs([]slog.Attr) slog.Handler       { return s }
func (s *stepTimes) WithGroup(string) slog.Handler            { return s }
func (s *stepTimes) Handle(_ context.Context, r slog.Record) error {
	if r.Message != "schema_migrating" && r.Message != "schema migrated" {
		return nil
	}
	var from int64
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "from" {
			from = a.Value.Int64()
		}
		return true
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Message == "schema_migrating" {
		s.start[from] = time.Now()
	} else {
		s.took[from] = time.Since(s.start[from])
	}
	return nil
}

// evictFromPageCache asks the kernel to drop the database file's clean
// pages from its page cache, so the next reads of it come from the disk:
// what a commit costs on a machine that has not touched those pages
// lately. Call it after a checkpoint (the pages are clean then).
func evictFromPageCache(t *testing.T, dir string) {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED); err != nil {
		t.Fatalf("fadvise: %v", err)
	}
}

func walBytes(dir string) int64 {
	st, err := os.Stat(filepath.Join(dir, "cache.db-wal"))
	if err != nil {
		return 0
	}
	return st.Size()
}

func TestMeasure_CommitOnARealDatabase(t *testing.T) {
	src := os.Getenv("ACU_MEASURE_DB")
	if src == "" {
		t.Skip("set ACU_MEASURE_DB to a copy of a cache.db")
	}
	ctx := context.Background()
	dir := t.TempDir()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(filepath.Join(dir, "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	_ = in.Close()
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}

	// Before: what the database holds at the schema it was copied at.
	raw, err := sql.Open("sqlite", filepath.Join(dir, "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	var version, total, current int64
	if err := raw.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version < 11 {
		if err := raw.QueryRow(`SELECT (SELECT count(*) FROM package_hash),
 (SELECT count(*) FROM package_hash p JOIN suite_freshness sf ON sf.current_snapshot_id = p.snapshot_id)`).Scan(&total, &current); err != nil {
			t.Fatal(err)
		}
	}
	_ = raw.Close()
	t.Logf("before: schema %d, %d package rows, %d of them of current snapshots", version, total, current)

	steps := &stepTimes{start: map[int64]time.Time{}, took: map[int64]time.Duration{}}
	opened := time.Now()
	c, err := Open(ctx, dir, slog.New(steps))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	t.Logf("open, with every migration: %.2fs", time.Since(opened).Seconds())
	var froms []int64
	for from := range steps.took {
		froms = append(froms, from)
	}
	slices.Sort(froms)
	for _, from := range froms {
		t.Logf("  schema %d -> %d: %.2fs", from, from+1, steps.took[from].Seconds())
	}
	var entries, lists int64
	if err := c.db.QueryRow(`SELECT (SELECT count(*) FROM package_list_entry), (SELECT count(*) FROM package_list)`).Scan(&entries, &lists); err != nil {
		t.Fatal(err)
	}
	t.Logf("after: schema %d, %d package rows in %d lists", CurrentSchemaVersion, entries, lists)

	// The suite with the largest list.
	var (
		key      SuiteKey
		snapshot int64
		n        int
	)
	if err := c.db.QueryRow(`
SELECT sf.canonical_scheme, sf.canonical_host, sf.suite_path, sf.current_snapshot_id, count(*)
  FROM suite_freshness sf JOIN package_list_entry e ON e.package_list_id = sf.package_list_id
 GROUP BY sf.package_list_id ORDER BY count(*) DESC LIMIT 1`).Scan(&key.CanonicalScheme, &key.CanonicalHost, &key.SuitePath, &snapshot, &n); err != nil {
		t.Fatalf("no suite with a package list: %v", err)
	}
	t.Logf("suite: %s%s, %d rows", key.CanonicalHost, key.SuitePath, n)

	checkpoint := func() {
		t.Helper()
		if _, err := c.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			t.Fatal(err)
		}
	}
	// next is the suite's list with 241 rows different, the most the June
	// data showed between two kept snapshots: 80 gone, 80 with another
	// hash, 81 new.
	generation := 0
	next := func() []PackageHash {
		t.Helper()
		generation++
		rows, err := c.PackageRowsOfSnapshotForTest(ctx, snapshot)
		if err != nil {
			t.Fatal(err)
		}
		stride := len(rows) / 161
		out := make([]PackageHash, 0, len(rows)+81)
		for i, r := range rows {
			switch {
			case i%stride == 0 && i/stride < 80:
				continue // removed
			case i%stride == 1 && i/stride < 80:
				r.DeclaredSHA256 = fmt.Sprintf("%064x", generation*1_000_000+i)
			case i%stride == 2 && i/stride < 81:
				added := r
				added.Path = fmt.Sprintf("%s.measure%d", r.Path, generation)
				out = append(out, added)
			}
			out = append(out, r)
		}
		return out
	}
	commit := func(label string, withDiff, cold bool) {
		t.Helper()
		rows := next()
		anchor := seedBlob(t, c, fmt.Sprintf("measure %s %d", label, generation))
		id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{CanonicalScheme: key.CanonicalScheme,
			CanonicalHost: key.CanonicalHost, SuitePath: key.SuitePath, InReleaseHash: &anchor})
		if err != nil {
			t.Fatal(err)
		}
		members := []SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: anchor, DeclaredSHA256: anchor}}
		var diff *PackageListDiff
		var read time.Duration
		if cold {
			checkpoint()
			evictFromPageCache(t, dir)
		}
		if withDiff {
			t0 := time.Now()
			if diff, err = c.DiffPackageList(ctx, key, rows); err != nil {
				t.Fatal(err)
			}
			read = time.Since(t0)
		}
		if !cold {
			checkpoint()
		}
		t0 := time.Now()
		w, err := c.CommitAdoptionDiff(ctx, id, members, nil, rows, nil, true, diff)
		if err != nil {
			t.Fatal(err)
		}
		took := time.Since(t0)
		t.Logf("%s: commit %.3fs, %.1f MiB of WAL, read of the live list before it %.3fs; %s %s +%d ~%d -%d (%d rows)",
			label, took.Seconds(), float64(walBytes(dir))/(1<<20), read.Seconds(), w.Mode, w.Reason, w.Added, w.Changed, w.Removed, len(rows))
		snapshot = id
	}
	commit("difference", true, false)
	commit("difference again", true, false)
	commit("difference, the database file evicted from the page cache first", true, true)
	commit("rewrite (no difference handed over)", false, false)
	commit("rewrite, the database file evicted from the page cache first", false, true)

	// GC of the list the rewrite left.
	checkpoint()
	t0 := time.Now()
	var deleted, chunks int
	var longest time.Duration
	for {
		c0 := time.Now()
		rows, more, err := c.RunPackageListGCChunk(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		if d := time.Since(c0); d > longest {
			longest = d
		}
		deleted += rows
		chunks++
		if !more {
			break
		}
	}
	t.Logf("gc of the unnamed lists: %d rows in %d chunks, %.3fs in all, longest chunk %.3fs, %.1f MiB of WAL",
		deleted, chunks, time.Since(t0).Seconds(), longest.Seconds(), float64(walBytes(dir))/(1<<20))
	commit("difference after the rewrites", true, false)
}
