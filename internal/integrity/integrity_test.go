package integrity

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// safeBuf is a goroutine-safe wrapper around bytes.Buffer for capturing
// slog output across the scanner's worker pool.
type safeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// openCache opens a fresh cache rooted at t.TempDir() with cleanup.
func openCache(t *testing.T) *cache.Cache {
	t.Helper()
	dir := t.TempDir()
	c, err := cache.Open(context.Background(), dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// seedBlob writes content into pool/ via the cache and returns the hash.
// Mirrors the seedBlob helper in internal/cache/cache_test.go.
func seedBlob(t *testing.T, c *cache.Cache, content []byte) string {
	t.Helper()
	w, err := c.NewTempBlob()
	if err != nil {
		t.Fatalf("NewTempBlob: %v", err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatalf("Write: %v", err)
	}
	hash, err := w.Finalize(int64(len(content)))
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if err := c.PutBlob(context.Background(), hash, int64(len(content))); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	return hash
}

// commitSnapshot adopts a single inline snapshot for the suite, with the
// given member rows. The snapshot's InRelease blob (releaseHash) is
// the metadata trust anchor. Returns the snapshot id.
func commitSnapshot(
	t *testing.T,
	c *cache.Cache,
	scheme, host, suitePath, releaseHash string,
	members []cache.SnapshotMember,
	pkgs []cache.PackageHash,
) int64 {
	t.Helper()
	ctx := context.Background()
	id, _, err := c.InsertCandidateSnapshot(ctx, cache.SnapshotCandidate{
		CanonicalScheme: scheme,
		CanonicalHost:   host,
		SuitePath:       suitePath,
		InReleaseHash:   &releaseHash,
	})
	if err != nil {
		t.Fatalf("InsertCandidateSnapshot: %v", err)
	}
	if err := c.CommitAdoption(ctx, id, members, pkgs, nil, false); err != nil {
		t.Fatalf("CommitAdoption: %v", err)
	}
	return id
}

// corruptBlob overwrites the file at pool/<hash> with bytes that hash
// to something else. Returns the path so the caller can re-stat it.
func corruptBlob(t *testing.T, c *cache.Cache, hash string) string {
	t.Helper()
	p := c.BlobPath(hash)
	if err := os.WriteFile(p, []byte("CORRUPTED CONTENT NOT MATCHING HASH"), 0o640); err != nil {
		t.Fatalf("corrupt blob %s: %v", hash, err)
	}
	return p
}

// TestScanOnce_TwoSnapshots_OneCorrupted is the SPEC2 §12.5 fixture:
// prime the cache with two snapshots, corrupt one blob in pool/
// directly, run the scan, verify the corrupted blob is removed and
// at_rest_corruption is logged with the right snapshot id; the
// uncorrupted blob is left alone.
func TestScanOnce_TwoSnapshots_OneCorrupted(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	// Snapshot A: noble/InRelease + a Packages member.
	relA := seedBlob(t, c, []byte("noble InRelease"))
	pkgA := seedBlob(t, c, []byte("noble main/binary-amd64/Packages"))
	idA := commitSnapshot(t, c, "http", "archive.ubuntu.com", "/ubuntu/dists/noble",
		relA,
		[]cache.SnapshotMember{
			{Path: "InRelease", BlobHash: relA, DeclaredSHA256: relA},
			{Path: "main/binary-amd64/Packages", BlobHash: pkgA, DeclaredSHA256: pkgA},
		},
		[]cache.PackageHash{
			{
				CanonicalScheme: "http", CanonicalHost: "archive.ubuntu.com",
				Path:           "/ubuntu/pool/main/h/hello/hello.deb",
				DeclaredSHA256: pkgA, // any valid hex; we only verify if file exists
			},
		},
	)
	if err := c.PutSuiteFreshness(ctx, cache.SuiteFreshness{
		CanonicalScheme: "http", CanonicalHost: "archive.ubuntu.com",
		SuitePath: "/ubuntu/dists/noble", CurrentSnapshotID: &idA,
	}); err != nil {
		t.Fatalf("PutSuiteFreshness A: %v", err)
	}

	// Snapshot B: bookworm/InRelease + a Packages member.
	relB := seedBlob(t, c, []byte("bookworm InRelease"))
	pkgB := seedBlob(t, c, []byte("bookworm main/binary-amd64/Packages"))
	idB := commitSnapshot(t, c, "http", "deb.debian.org", "/debian/dists/bookworm",
		relB,
		[]cache.SnapshotMember{
			{Path: "InRelease", BlobHash: relB, DeclaredSHA256: relB},
			{Path: "main/binary-amd64/Packages", BlobHash: pkgB, DeclaredSHA256: pkgB},
		},
		nil,
	)
	if err := c.PutSuiteFreshness(ctx, cache.SuiteFreshness{
		CanonicalScheme: "http", CanonicalHost: "deb.debian.org",
		SuitePath: "/debian/dists/bookworm", CurrentSnapshotID: &idB,
	}); err != nil {
		t.Fatalf("PutSuiteFreshness B: %v", err)
	}

	// Corrupt the Packages blob of snapshot A.
	corruptBlob(t, c, pkgA)

	var buf safeBuf
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s, err := New(Config{Cache: c, Interval: 0, Workers: 2, Logger: logger})
	if err != nil {
		t.Fatalf("integrity.New: %v", err)
	}
	if err := s.ScanOnce(ctx); err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}

	// Corrupted blob: file is gone.
	if _, err := os.Stat(c.BlobPath(pkgA)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("corrupted blob %s should be removed: stat err = %v", pkgA, err)
	}
	// Uncorrupted blobs: still present.
	for _, h := range []string{relA, relB, pkgB} {
		if _, err := os.Stat(c.BlobPath(h)); err != nil {
			t.Errorf("intact blob %s missing: %v", h, err)
		}
	}

	out := buf.String()
	if !strings.Contains(out, "at_rest_corruption") {
		t.Errorf("expected at_rest_corruption log line; got:\n%s", out)
	}
	if !strings.Contains(out, "blob_hash="+pkgA) {
		t.Errorf("expected at_rest_corruption to name pkgA hash %s; got:\n%s", pkgA, out)
	}
	// The first-found snapshot for this blob is A's snapshot_member row.
	wantSnap := "snapshot_id=" + itoa(idA)
	if !strings.Contains(out, wantSnap) {
		t.Errorf("expected at_rest_corruption to name snapshot id %d; got:\n%s", idA, out)
	}
	if !strings.Contains(out, "at_rest_scan_started") || !strings.Contains(out, "at_rest_scan_finished") {
		t.Errorf("expected scan_started/finished events; got:\n%s", out)
	}
	if !strings.Contains(out, "mismatch_count=1") {
		t.Errorf("expected mismatch_count=1; got:\n%s", out)
	}
}

// TestScanOnce_NoCorruption_ScanIsClean validates that a healthy pool
// produces no at_rest_corruption events and no DiscardFinalizedBlob
// calls.
func TestScanOnce_NoCorruption_ScanIsClean(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	rel := seedBlob(t, c, []byte("InRelease"))
	pkg := seedBlob(t, c, []byte("Packages"))
	id := commitSnapshot(t, c, "http", "archive.ubuntu.com", "/ubuntu/dists/noble",
		rel,
		[]cache.SnapshotMember{
			{Path: "InRelease", BlobHash: rel, DeclaredSHA256: rel},
			{Path: "main/binary-amd64/Packages", BlobHash: pkg, DeclaredSHA256: pkg},
		},
		nil,
	)
	if err := c.PutSuiteFreshness(ctx, cache.SuiteFreshness{
		CanonicalScheme: "http", CanonicalHost: "archive.ubuntu.com",
		SuitePath: "/ubuntu/dists/noble", CurrentSnapshotID: &id,
	}); err != nil {
		t.Fatalf("PutSuiteFreshness: %v", err)
	}

	var buf safeBuf
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s, err := New(Config{Cache: c, Interval: 0, Workers: 1, Logger: logger})
	if err != nil {
		t.Fatalf("integrity.New: %v", err)
	}
	if err := s.ScanOnce(ctx); err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "at_rest_corruption") {
		t.Errorf("clean scan emitted at_rest_corruption: %s", out)
	}
	if !strings.Contains(out, "mismatch_count=0") {
		t.Errorf("expected mismatch_count=0; got:\n%s", out)
	}
	for _, h := range []string{rel, pkg} {
		if _, err := os.Stat(c.BlobPath(h)); err != nil {
			t.Errorf("intact blob %s missing after clean scan: %v", h, err)
		}
	}
}

// TestScanOnce_DisplacedSnapshotNotScanned verifies the suite_freshness
// join: a snapshot whose adopted_at is set but whose current_snapshot_id
// has been replaced is not scanned. SPEC2 §6.5 wording: "every
// snapshot_member and package_hash row whose blob is on disk" reads as
// "every", but §10.2 names "first-found is reported" — taken with the
// §7.5.1 invariant that only current snapshots are the contract, the
// scanner should ignore displaced snapshots. Their blobs hit refcount-0
// after the next adoption flip and Phase 4 GC reaps them; the integrity
// scanner does not duplicate that work.
func TestScanOnce_DisplacedSnapshotNotScanned(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	scheme, host, suite := "http", "archive.ubuntu.com", "/ubuntu/dists/noble"

	relOld := seedBlob(t, c, []byte("noble InRelease v1"))
	pkgOld := seedBlob(t, c, []byte("noble Packages v1"))
	idOld := commitSnapshot(t, c, scheme, host, suite, relOld,
		[]cache.SnapshotMember{
			{Path: "InRelease", BlobHash: relOld, DeclaredSHA256: relOld},
			{Path: "main/binary-amd64/Packages", BlobHash: pkgOld, DeclaredSHA256: pkgOld},
		},
		nil,
	)
	if err := c.PutSuiteFreshness(ctx, cache.SuiteFreshness{
		CanonicalScheme: scheme, CanonicalHost: host, SuitePath: suite,
		CurrentSnapshotID: &idOld,
	}); err != nil {
		t.Fatalf("PutSuiteFreshness old: %v", err)
	}

	// Adopt a new snapshot (CommitAdoption flips current_snapshot_id).
	relNew := seedBlob(t, c, []byte("noble InRelease v2"))
	pkgNew := seedBlob(t, c, []byte("noble Packages v2"))
	idNew, _, err := c.InsertCandidateSnapshot(ctx, cache.SnapshotCandidate{
		CanonicalScheme: scheme, CanonicalHost: host, SuitePath: suite,
		InReleaseHash: &relNew,
	})
	if err != nil {
		t.Fatalf("InsertCandidateSnapshot v2: %v", err)
	}
	if err := c.CommitAdoption(ctx, idNew,
		[]cache.SnapshotMember{
			{Path: "InRelease", BlobHash: relNew, DeclaredSHA256: relNew},
			{Path: "main/binary-amd64/Packages", BlobHash: pkgNew, DeclaredSHA256: pkgNew},
		}, nil, nil, false,
	); err != nil {
		t.Fatalf("CommitAdoption v2: %v", err)
	}

	// Corrupt the OLD packages blob. It still exists in pool/ (refcount
	// went to 0 but Phase 4 GC hasn't run), but it is no longer
	// referenced by any current snapshot.
	corruptBlob(t, c, pkgOld)

	var buf safeBuf
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s, err := New(Config{Cache: c, Interval: 0, Workers: 1, Logger: logger})
	if err != nil {
		t.Fatalf("integrity.New: %v", err)
	}
	if err := s.ScanOnce(ctx); err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, "at_rest_corruption") {
		t.Errorf("scanner should ignore displaced-snapshot blob; got at_rest_corruption:\n%s", out)
	}
	// Corrupted file still on disk — Phase 4 GC's job, not ours.
	if _, err := os.Stat(c.BlobPath(pkgOld)); err != nil {
		t.Errorf("displaced blob should remain on disk; stat err = %v", err)
	}
}

// TestScanOnce_MissingFile_NotReportedAsCorruption verifies that a
// snapshot row pointing at a blob whose pool file has been deleted
// is not reported as a corruption — the request path will fail closed
// or refetch, which is the correct surface, not the scanner's.
func TestScanOnce_MissingFile_NotReportedAsCorruption(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	rel := seedBlob(t, c, []byte("InRelease"))
	pkg := seedBlob(t, c, []byte("Packages"))
	id := commitSnapshot(t, c, "http", "archive.ubuntu.com", "/ubuntu/dists/noble", rel,
		[]cache.SnapshotMember{
			{Path: "InRelease", BlobHash: rel, DeclaredSHA256: rel},
			{Path: "main/binary-amd64/Packages", BlobHash: pkg, DeclaredSHA256: pkg},
		},
		nil,
	)
	if err := c.PutSuiteFreshness(ctx, cache.SuiteFreshness{
		CanonicalScheme: "http", CanonicalHost: "archive.ubuntu.com",
		SuitePath: "/ubuntu/dists/noble", CurrentSnapshotID: &id,
	}); err != nil {
		t.Fatalf("PutSuiteFreshness: %v", err)
	}

	// Delete the file (simulating an out-of-band rm or filesystem fault).
	if err := os.Remove(c.BlobPath(pkg)); err != nil {
		t.Fatalf("rm blob: %v", err)
	}

	var buf safeBuf
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s, err := New(Config{Cache: c, Interval: 0, Workers: 1, Logger: logger})
	if err != nil {
		t.Fatalf("integrity.New: %v", err)
	}
	if err := s.ScanOnce(ctx); err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "at_rest_corruption") {
		t.Errorf("missing-file should not be classified as corruption: %s", out)
	}
	if !strings.Contains(out, "blob missing") {
		t.Errorf("expected debug 'blob missing'; got:\n%s", out)
	}
}

// TestScanOnce_EmptyPoolNoCandidates verifies the trivial "fresh
// install, no adoptions" case logs scan_started/finished with
// blob_count=0 and exits cleanly.
func TestScanOnce_EmptyPoolNoCandidates(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	var buf safeBuf
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s, err := New(Config{Cache: c, Interval: 0, Workers: 1, Logger: logger})
	if err != nil {
		t.Fatalf("integrity.New: %v", err)
	}
	if err := s.ScanOnce(ctx); err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "blob_count=0") {
		t.Errorf("expected blob_count=0; got:\n%s", out)
	}
}

// TestRun_ZeroIntervalReturnsImmediately validates that the scheduler
// short-circuits when integrity.validate_at_rest_interval is 0 — the
// supported way for an operator to disable scanning.
func TestRun_ZeroIntervalReturnsImmediately(t *testing.T) {
	c := openCache(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := New(Config{Cache: c, Interval: 0, Workers: 1, Logger: logger})
	if err != nil {
		t.Fatalf("integrity.New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	select {
	case <-done:
		// expected — Run returned immediately
	case <-time.After(20 * time.Millisecond):
		t.Fatal("Run did not return immediately on Interval=0")
	}
}

// TestNew_RejectsBadConfig validates the constructor's input
// validation (defense in depth on top of the config-layer checks).
func TestNew_RejectsBadConfig(t *testing.T) {
	c := openCache(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cases := []struct {
		name string
		cfg  Config
	}{
		{"nil cache", Config{Logger: logger, Interval: time.Second, Workers: 1}},
		{"nil logger", Config{Cache: c, Interval: time.Second, Workers: 1}},
		{"negative interval", Config{Cache: c, Logger: logger, Interval: -time.Second, Workers: 1}},
		{"zero workers when interval > 0", Config{Cache: c, Logger: logger, Interval: time.Second, Workers: 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}

// TestListIntegrityCandidates_DedupsAcrossTables validates the §6.5
// query-side dedup: when the same blob hash is pinned by both a
// snapshot_member and a package_hash row in the same current snapshot,
// only one IntegrityCandidate is emitted (snapshot_member wins per
// "first-found").
func TestListIntegrityCandidates_DedupsAcrossTables(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	rel := seedBlob(t, c, []byte("InRelease"))
	body := seedBlob(t, c, []byte("metadata blob also referenced by package_hash"))
	id := commitSnapshot(t, c, "http", "archive.ubuntu.com", "/ubuntu/dists/noble", rel,
		[]cache.SnapshotMember{
			{Path: "InRelease", BlobHash: rel, DeclaredSHA256: rel},
			{Path: "main/binary-amd64/Packages", BlobHash: body, DeclaredSHA256: body},
		},
		[]cache.PackageHash{
			{
				CanonicalScheme: "http", CanonicalHost: "archive.ubuntu.com",
				Path:           "/ubuntu/pool/main/h/hello/hello.deb",
				DeclaredSHA256: body, // pathologically equal to a metadata hash
			},
		},
	)
	if err := c.PutSuiteFreshness(ctx, cache.SuiteFreshness{
		CanonicalScheme: "http", CanonicalHost: "archive.ubuntu.com",
		SuitePath: "/ubuntu/dists/noble", CurrentSnapshotID: &id,
	}); err != nil {
		t.Fatalf("PutSuiteFreshness: %v", err)
	}
	got, err := c.ListIntegrityCandidates(ctx)
	if err != nil {
		t.Fatalf("ListIntegrityCandidates: %v", err)
	}
	hashes := make(map[string]string)
	for _, ic := range got {
		hashes[ic.BlobHash] = ic.SourceTable
	}
	if hashes[body] != "snapshot_member" {
		t.Errorf("expected snapshot_member to win for shared blob %s; got source=%q", body, hashes[body])
	}
	// Total rows: rel (snapshot_member) + body (snapshot_member, dedup'd
	// against package_hash) = 2.
	if len(got) != 2 {
		t.Errorf("expected 2 dedup'd candidates, got %d: %+v", len(got), got)
	}
}

// itoa is a small helper to avoid pulling in strconv just for log
// substring assertions.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		pos--
		b[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
