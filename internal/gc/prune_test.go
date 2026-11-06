package gc

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// The merge review's M-I1 (the maintainer, 2026-11-06): the representation
// prune runs once a tick, at the start of the blob pass, not in every blob
// batch. seedPruneFixture leaves the cache with one unowned, day-old date
// for a blob a live url_path row keeps (the prune's only work) and n
// reapable blobs (n/100 rounded up blob batches at storeGC's batch size).
func seedPruneFixture(t *testing.T, c *cache.Cache, n int) (shared string) {
	t.Helper()
	ctx := context.Background()
	shared = putPoolBlob(t, c, "shared content")
	if err := c.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: "/live",
		BlobHash: &shared, UpstreamURL: "http://h/live"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/live", "/orphan"} {
		if err := c.PutRepresentation(ctx, "http", "h", path, shared, "Mon, 01 Jan 2024 00:00:00 GMT"); err != nil {
			t.Fatal(err)
		}
	}
	db := dbOf(t, c)
	if _, err := db.Exec(`UPDATE representation_metadata SET observed_at = 1`); err != nil {
		t.Fatal(err)
	}
	// Requested now, so the url-path pass keeps /live.
	if _, err := db.Exec(`UPDATE url_path SET last_requested_at = ? WHERE path = '/live'`, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	for i := range n {
		seedReapableBlob(t, c, fmt.Sprintf("dead blob %d", i))
	}
	return shared
}

// checkPruned fails unless the unowned date is gone and the owned one kept.
func checkPruned(t *testing.T, c *cache.Cache, shared string) {
	t.Helper()
	for path, want := range map[string]string{"/live": "Mon, 01 Jan 2024 00:00:00 GMT", "/orphan": ""} {
		got, err := c.RepresentationLastModified(context.Background(), "http", "h", path, shared)
		if err != nil || got != want {
			t.Fatalf("date of %s = %q, %v; want %q", path, got, err, want)
		}
	}
}

// A periodic tick prunes exactly once, however many blob batches its blob
// pass runs (three here) and with every pass running (the url-path pass is
// on), and the unowned date goes.
func TestPeriodicTick_PrunesRepresentationMetadataOnce(t *testing.T) {
	logger, _ := captureLogger()
	f := &fakeStore{}
	g, c := storeGC(t, f, 2, logger)
	g.cfg.URLPathTTL = time.Hour
	shared := seedPruneFixture(t, c, 250)
	g.periodicTick(context.Background())
	s, _ := g.LastRunSummary()
	if f.prunes != 1 || s.BlobsReaped != 250 {
		t.Fatalf("%d prunes, %d blobs reaped; want 1 prune for the tick and all 250 blobs", f.prunes, s.BlobsReaped)
	}
	checkPruned(t, c, shared)
}

// The startup pass prunes once too, within its budget.
func TestStartupPass_PrunesRepresentationMetadataOnce(t *testing.T) {
	logger, _ := captureLogger()
	f := &fakeStore{}
	g, c := storeGC(t, f, 2, logger)
	g.cfg.URLPathTTL = time.Hour
	shared := seedPruneFixture(t, c, 150)
	if err := g.StartupPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.prunes != 1 {
		t.Fatalf("%d prunes in the startup pass, want 1", f.prunes)
	}
	checkPruned(t, c, shared)
}

// The prune never runs past the blob pass's deadline: a tick whose budget
// is already spent leaves it, with the blob pass, to the next tick.
func TestRunTick_NoPrunePastTheBlobDeadline(t *testing.T) {
	logger, _ := captureLogger()
	f := &fakeStore{}
	g, c := storeGC(t, f, 2, logger)
	seedPruneFixture(t, c, 0)
	g.cfg.MaxTickDuration = time.Nanosecond
	g.nextPass = blobPass
	if _, err := g.runTick(context.Background(), "periodic"); err != nil {
		t.Fatal(err)
	}
	if f.prunes != 0 {
		t.Fatalf("%d prunes past the blob deadline, want 0", f.prunes)
	}
}

// A failed prune does not stop the blob batches: they reap, and the prune's
// failure is the blob pass's, one WARN gc_tick_failed (ruling HD-15).
func TestPeriodicTick_AFailedPruneStillReapsAndIsTheBlobPassFailure(t *testing.T) {
	logger, buf := captureLogger()
	f := &fakeStore{pruneErrs: true}
	g, c := storeGC(t, f, 2, logger)
	seedPruneFixture(t, c, 120)
	g.periodicTick(context.Background())
	if s, _ := g.LastRunSummary(); s.BlobsReaped != 120 {
		t.Fatalf("blobs reaped %d, want 120: a failed prune stopped the batches", s.BlobsReaped)
	}
	ev := gcEvents(t, buf, "gc_tick_failed")
	if len(ev) != 1 {
		t.Fatalf("gc_tick_failed lines %v, want one", ev)
	}
	if msg, _ := ev[0]["err"].(string); msg != "representation metadata prune: prune fault" {
		t.Fatalf("gc_tick_failed err %q, want the prune's", msg)
	}
}
