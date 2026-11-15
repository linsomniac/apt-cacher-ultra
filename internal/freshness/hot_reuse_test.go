package freshness

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

func TestHotPrefetchReusesVerifiedContentOffline(t *testing.T) {
	f := newHotPrefetchFixture(t, time.Second)
	f.adopter.hotPackagesWindow = 300 * 24 * time.Hour
	f.fetcher.setBody(f.debUpstreamURL, []byte("new deb bytes v2"))
	_, first := f.adopter.runHotPrefetch(t.Context(), f.suite, f.candidate, f.candidatePackageHashes, nil)
	if first.fetched != 1 {
		t.Fatalf("first run: %+v", first)
	}
	f.fetcher.setError(f.debUpstreamURL, errors.New("upstream offline"))
	const date = "Mon, 01 Jan 2024 00:00:00 GMT"
	if err := f.cache.PutRepresentation(t.Context(), f.suite.CanonicalScheme, f.suite.CanonicalHost, f.debPath, f.debDeclared, date); err != nil {
		t.Fatal(err)
	}
	tracker := &blobHeartbeatTracker{}
	rows, second := f.adopter.runHotPrefetch(t.Context(), f.suite, f.candidate, f.candidatePackageHashes, tracker)
	if len(rows) != 1 || second.reused != 1 || second.fetched != 0 || second.failed != 0 || f.fetcher.calls != 1 {
		t.Fatalf("second run: rows=%v stats=%+v network calls=%d", rows, second, f.fetcher.calls)
	}
	if hashes := tracker.Snapshot(); len(hashes) != 1 || hashes[0] != f.debDeclared {
		t.Fatalf("heartbeat: %v", hashes)
	}
	got, err := f.cache.RepresentationLastModified(t.Context(), f.suite.CanonicalScheme, f.suite.CanonicalHost, f.debPath, f.debDeclared)
	if err != nil || got != date {
		t.Fatalf("origin date %q, %v", got, err)
	}
}

func TestHotPrefetchRepairsMissingOrCorruptBlob(t *testing.T) {
	for _, mode := range []string{"missing", "corrupt", "bad_upstream"} {
		t.Run(mode, func(t *testing.T) {
			f := newHotPrefetchFixture(t, time.Second)
			writeFixtureBlob(t, f.cache, []byte("new deb bytes v2"))
			if mode == "missing" {
				if err := os.Remove(f.cache.BlobPath(f.debDeclared)); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(f.cache.BlobPath(f.debDeclared), []byte("corrupt"), 0600); err != nil {
				t.Fatal(err)
			}
			payload := []byte("new deb bytes v2")
			if mode == "bad_upstream" {
				payload = []byte("wrong upstream bytes")
			}
			f.fetcher.setBody(f.debUpstreamURL, payload)
			rows, stats := f.adopter.runHotPrefetch(t.Context(), f.suite, f.candidate, f.candidatePackageHashes, nil)
			if mode == "bad_upstream" {
				if stats.mismatched != 1 || len(rows) != 0 {
					t.Fatalf("bad upstream: %+v %v", stats, rows)
				}
				if exists, _ := f.cache.BlobExists(f.debDeclared); exists {
					t.Fatal("corrupt blob survived")
				}
				return
			}
			if stats.fetched != 1 || stats.reused != 0 || len(rows) != 1 {
				t.Fatalf("repair: %+v %v", stats, rows)
			}
			got, err := hashFile(f.cache.BlobPath(f.debDeclared))
			if err != nil || got != f.debDeclared {
				t.Fatalf("repair hash: %s %v", got, err)
			}
		})
	}
}

func TestHotPrefetchConcurrentSuitesDownloadContentOnce(t *testing.T) {
	f := newHotPrefetchFixture(t, 5*time.Second)
	f.fetcher.setBody(f.debUpstreamURL, []byte("new deb bytes v2"))
	f.fetcher.setBlocking(f.debUpstreamURL, 20*time.Millisecond)
	entry := hotSetEntry{path: f.debPath, declaredSHA256: f.debDeclared, upstreamURL: f.debUpstreamURL}
	var wg sync.WaitGroup
	outcomes := make(chan hotFetchOutcome, 8)
	for range 8 {
		wg.Go(func() {
			_, outcome := f.adopter.fetchHotDeb(t.Context(), f.suite, entry, f.candidate)
			outcomes <- outcome
		})
	}
	wg.Wait()
	close(outcomes)
	counts := map[hotFetchOutcome]int{}
	for o := range outcomes {
		counts[o]++
	}
	if f.fetcher.calls != 1 || counts[hotFetchOK] != 1 || counts[hotFetchReused] != 7 || f.adopter.blobLocks.HostCount() != 0 {
		t.Fatalf("calls=%d outcomes=%v locks=%d", f.fetcher.calls, counts, f.adopter.blobLocks.HostCount())
	}
}

func TestHotPrefetchCancelledReuseDoesNotFetch(t *testing.T) {
	f := newHotPrefetchFixture(t, time.Second)
	writeFixtureBlob(t, f.cache, []byte("new deb bytes v2"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, stats := f.adopter.runHotPrefetch(ctx, f.suite, f.candidate, f.candidatePackageHashes, nil)
	if stats.fetched != 0 || f.fetcher.calls != 0 {
		t.Fatalf("stats=%+v calls=%d", stats, f.fetcher.calls)
	}
}

func TestHotPrefetchReusesAcrossHostsWithoutBorrowingOriginDate(t *testing.T) {
	f := newHotPrefetchFixture(t, time.Second)
	writeFixtureBlob(t, f.cache, []byte("new deb bytes v2"))
	const date = "Mon, 01 Jan 2024 00:00:00 GMT"
	if err := f.cache.PutRepresentation(t.Context(), f.suite.CanonicalScheme, f.suite.CanonicalHost, f.debPath, f.debDeclared, date); err != nil {
		t.Fatal(err)
	}
	suite := f.suite
	suite.CanonicalHost = "another-mirror.example"
	entry := hotSetEntry{path: f.debPath, declaredSHA256: f.debDeclared, upstreamURL: "http://another-mirror.example" + f.debPath}
	hash, outcome := f.adopter.fetchHotDeb(t.Context(), suite, entry, f.candidate)
	if hash != f.debDeclared || outcome != hotFetchReused || f.fetcher.calls != 0 {
		t.Fatalf("hash=%s outcome=%v calls=%d", hash, outcome, f.fetcher.calls)
	}
	got, err := f.cache.RepresentationLastModified(t.Context(), suite.CanonicalScheme, suite.CanonicalHost, entry.path, hash)
	if err != nil || got != "" {
		t.Fatalf("borrowed origin date: %q %v", got, err)
	}
}
