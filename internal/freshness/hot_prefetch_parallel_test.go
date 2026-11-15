package freshness

// Hot packages are warmed hotPrefetchWorkers at a time (the 2026-11-14
// publication-delay review): the prefetch runs before the new release is
// published, so its duration is time clients keep seeing the old one.

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"
)

// hotEntries builds n hot-set entries whose packages f's fetcher serves.
func hotEntries(f *hotPrefetchFixture, n int) []hotSetEntry {
	entries := make([]hotSetEntry, n)
	for i := range entries {
		body := []byte(fmt.Sprintf("package %02d, new version", i))
		path := fmt.Sprintf("/pool/main/p/pkg%02d/pkg%02d_2_amd64.deb", i, i)
		entries[i] = hotSetEntry{
			path:           path,
			declaredSHA256: sha256OfBytes(body),
			upstreamURL:    "http://archive.example" + path,
		}
		f.fetcher.setBody(entries[i].upstreamURL, body)
	}
	return entries
}

func TestPrefetchHotSet_PackagesAreFetchedConcurrently(t *testing.T) {
	f := newHotPrefetchFixture(t, time.Minute)
	entries := hotEntries(f, 10)
	gate := &gateFetcher{inner: f.fetcher, want: hotPrefetchWorkers, open: make(chan struct{})}
	f.adopter.fetcher = gate

	rows, stats := f.adopter.prefetchHotSet(context.Background(), f.suite, f.candidate, entries, nil)
	if stats.hotCount != 10 || stats.fetched != 10 || stats.failed+stats.mismatched+stats.unattempted+stats.reused != 0 {
		t.Fatalf("stats = %+v, want 10 fetched of 10", stats)
	}
	if got := gate.highest.Load(); got != hotPrefetchWorkers {
		t.Errorf("most package fetches in flight at once = %d, want %d", got, hotPrefetchWorkers)
	}
	// The rows the commit inserts keep the hot set's order, whatever
	// order the downloads finished in.
	if len(rows) != len(entries) {
		t.Fatalf("prefetched rows = %d, want %d", len(rows), len(entries))
	}
	for i, r := range rows {
		if r.Path != entries[i].path || r.BlobHash != entries[i].declaredSHA256 {
			t.Errorf("row %d = %s %s, want %s %s", i, r.Path, r.BlobHash, entries[i].path, entries[i].declaredSHA256)
		}
	}
}

// One package that fails does not stop the others.
func TestPrefetchHotSet_AFailedPackageDoesNotStopTheOthers(t *testing.T) {
	f := newHotPrefetchFixture(t, time.Minute)
	entries := hotEntries(f, 6)
	f.fetcher.setError(entries[2].upstreamURL, fmt.Errorf("upstream reset the connection"))

	rows, stats := f.adopter.prefetchHotSet(context.Background(), f.suite, f.candidate, entries, nil)
	if stats.fetched != 5 || stats.failed != 1 || stats.unattempted != 0 {
		t.Fatalf("stats = %+v, want 5 fetched, 1 failed", stats)
	}
	for _, r := range rows {
		if r.Path == entries[2].path {
			t.Errorf("the failed package has a prefetched row: %+v", r)
		}
	}
}

// When the budget passes, the packages in flight are failed and the ones
// not started are reported missing, in the hot set's order; the buckets
// still add up to the hot count (SPEC3 §10.2).
func TestPrefetchHotSet_BudgetReportsThePackagesNotStarted(t *testing.T) {
	f := newHotPrefetchFixture(t, 150*time.Millisecond)
	entries := hotEntries(f, 9)
	for _, e := range entries {
		f.fetcher.setBlocking(e.upstreamURL, 10*time.Second) // every fetch outlasts the budget
	}
	logs := &lockedBuffer{}
	f.adopter.logger = slog.New(slog.NewJSONHandler(logs, nil))

	rows, stats := f.adopter.prefetchHotSet(context.Background(), f.suite, f.candidate, entries, nil)
	if len(rows) != 0 {
		t.Errorf("prefetched rows = %d, want none", len(rows))
	}
	if stats.failed != hotPrefetchWorkers || stats.unattempted != 9-hotPrefetchWorkers || stats.fetched != 0 {
		t.Fatalf("stats = %+v, want %d failed (in flight at the budget) and %d unattempted",
			stats, hotPrefetchWorkers, 9-hotPrefetchWorkers)
	}
	if sum := stats.fetched + stats.reused + stats.failed + stats.mismatched + stats.unattempted; sum != stats.hotCount {
		t.Errorf("buckets add up to %d, hot count is %d", sum, stats.hotCount)
	}
	partial := logRecords(t, logs, "adoption_hot_prefetch_partial")
	if len(partial) != 1 {
		t.Fatalf("adoption_hot_prefetch_partial lines = %d, want one", len(partial))
	}
	missing, _ := partial[0]["missing"].([]any)
	if len(missing) != 9-hotPrefetchWorkers {
		t.Fatalf("missing = %v, want the %d packages not started", missing, 9-hotPrefetchWorkers)
	}
	for i, m := range missing {
		if want := entries[hotPrefetchWorkers+i].path; m != want {
			t.Errorf("missing[%d] = %v, want %s (hot-set order)", i, m, want)
		}
	}
}
