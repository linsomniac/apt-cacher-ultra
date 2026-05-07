package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// openCache opens a fresh cache rooted at t.TempDir() with sane defaults
// for tests. Caller does not need to remember to Close — the helper
// registers cleanup.
func openCache(t *testing.T) *Cache {
	t.Helper()
	dir := t.TempDir()
	c, err := Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestOpen_FreshDirectoryCreatesLayout(t *testing.T) {
	c := openCache(t)
	for _, sub := range []string{"pool", "tmp", "staging"} {
		st, err := os.Stat(filepath.Join(c.Dir(), sub))
		if err != nil || !st.IsDir() {
			t.Errorf("%s/ missing or not dir: %v", sub, err)
		}
	}
	if _, err := os.Stat(filepath.Join(c.Dir(), "cache.db")); err != nil {
		t.Errorf("cache.db missing: %v", err)
	}
}

func TestOpen_AppliesInitialMigration(t *testing.T) {
	c := openCache(t)
	v, err := readSchemaVersion(context.Background(), c.db)
	if err != nil {
		t.Fatalf("readSchemaVersion: %v", err)
	}
	if v != CurrentSchemaVersion {
		t.Errorf("schema version = %d, want %d", v, CurrentSchemaVersion)
	}
	// Tables exist and accept the expected columns: try a bounded SELECT
	// against each one.
	for _, q := range []string{
		`SELECT count(*) FROM blob`,
		`SELECT count(*) FROM url_path`,
		`SELECT count(*) FROM suite_freshness`,
		`SELECT count(*) FROM schema_version`,
	} {
		var n int
		if err := c.db.QueryRow(q).Scan(&n); err != nil {
			t.Errorf("%q: %v", q, err)
		}
	}
}

func TestOpen_RejectsFutureSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Bump schema_version to a value newer than this binary supports.
	if _, err := c.db.Exec(`UPDATE schema_version SET version = ?`, CurrentSchemaVersion+1); err != nil {
		t.Fatalf("bump version: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err = Open(context.Background(), dir, nil)
	if err == nil {
		t.Fatal("Open(future schema): expected error, got nil")
	}
	if !strings.Contains(err.Error(), "newer than this binary supports") {
		t.Errorf("error %q does not name the version mismatch", err)
	}
}

func TestOpen_ReopenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	c1, err := Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Open #1: %v", err)
	}
	if err := c1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	c2, err := Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Open #2: %v", err)
	}
	defer c2.Close()
	v, _ := readSchemaVersion(context.Background(), c2.db)
	if v != CurrentSchemaVersion {
		t.Errorf("after reopen, version = %d", v)
	}
}

func TestBlobRoundtrip(t *testing.T) {
	c := openCache(t)
	body := []byte("dpkg-1.21.1ubuntu2.3_amd64.deb contents")
	expected := sha256.Sum256(body)
	expectedHex := hex.EncodeToString(expected[:])

	w, err := c.NewTempBlob()
	if err != nil {
		t.Fatalf("NewTempBlob: %v", err)
	}
	if _, err := io.Copy(w, bytes.NewReader(body)); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	hash, err := w.Finalize(int64(len(body)))
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if hash != expectedHex {
		t.Errorf("hash = %s, want %s", hash, expectedHex)
	}

	// Path reflects the bucket scheme.
	want := filepath.Join(c.Dir(), "pool", expectedHex[:2], expectedHex)
	if got := c.BlobPath(hash); got != want {
		t.Errorf("BlobPath = %s, want %s", got, want)
	}
	exists, err := c.BlobExists(hash)
	if err != nil || !exists {
		t.Errorf("BlobExists = %v, %v", exists, err)
	}

	// On-disk content matches input.
	got, err := os.ReadFile(c.BlobPath(hash))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("on-disk content mismatch")
	}
}

func TestBlobFinalize_SizeMismatchDiscards(t *testing.T) {
	c := openCache(t)
	w, err := c.NewTempBlob()
	if err != nil {
		t.Fatalf("NewTempBlob: %v", err)
	}
	if _, err := w.Write([]byte("short")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	_, err = w.Finalize(999)
	if !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("Finalize: got %v, want ErrSizeMismatch", err)
	}
	// tmp/ should be empty after a discard.
	entries, _ := os.ReadDir(filepath.Join(c.Dir(), "tmp"))
	if len(entries) != 0 {
		t.Errorf("tmp/ has %d entries after size-mismatch discard, want 0", len(entries))
	}
}

func TestBlobTruncate_ResetsToZero(t *testing.T) {
	c := openCache(t)
	w, err := c.NewTempBlob()
	if err != nil {
		t.Fatalf("NewTempBlob: %v", err)
	}

	// Write some bytes that would have hashed to a specific value.
	if _, err := w.Write([]byte("partial fetched bytes that should be discarded")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if w.Written() == 0 {
		t.Fatalf("expected non-zero Written before Truncate")
	}

	if err := w.Truncate(); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if w.Written() != 0 {
		t.Errorf("Written after Truncate = %d, want 0", w.Written())
	}

	// Now write the actual content and Finalize. The hash must match
	// hashing only the final bytes (proving the hasher was reset).
	body := []byte("the real bytes")
	if _, err := w.Write(body); err != nil {
		t.Fatalf("post-Truncate Write: %v", err)
	}
	hash, err := w.Finalize(int64(len(body)))
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	// Compare against an independently hashed "the real bytes" — if
	// the truncate left the hasher dirty, hashes would diverge.
	want := sha256.Sum256(body)
	if hash != hex.EncodeToString(want[:]) {
		t.Errorf("hash mismatch: got %s, hash should match sha256(%q)", hash, body)
	}

	// On-disk file should hold exactly len(body).
	st, err := os.Stat(c.BlobPath(hash))
	if err != nil {
		t.Fatalf("Stat blob: %v", err)
	}
	if st.Size() != int64(len(body)) {
		t.Errorf("blob size=%d, want %d", st.Size(), len(body))
	}
}

func TestBlobTruncate_AfterFinalizeRejects(t *testing.T) {
	c := openCache(t)
	w, err := c.NewTempBlob()
	if err != nil {
		t.Fatalf("NewTempBlob: %v", err)
	}
	if _, err := w.Write([]byte("done")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := w.Finalize(4); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if err := w.Truncate(); err == nil {
		t.Error("Truncate after Finalize should error")
	}
}

func TestBlobAbort_RemovesTempFile(t *testing.T) {
	c := openCache(t)
	w, err := c.NewTempBlob()
	if err != nil {
		t.Fatalf("NewTempBlob: %v", err)
	}
	_, _ = w.Write([]byte("partial"))
	if err := w.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(c.Dir(), "tmp"))
	if len(entries) != 0 {
		t.Errorf("tmp/ has %d entries after abort, want 0", len(entries))
	}
	// Idempotent: a second Abort is a no-op.
	if err := w.Abort(); err != nil {
		t.Errorf("second Abort: %v", err)
	}
}

func TestBlobFinalize_DuplicateHashIsNoOp(t *testing.T) {
	c := openCache(t)
	body := []byte("identical content")

	for i := 0; i < 2; i++ {
		w, err := c.NewTempBlob()
		if err != nil {
			t.Fatalf("NewTempBlob: %v", err)
		}
		_, _ = w.Write(body)
		if _, err := w.Finalize(int64(len(body))); err != nil {
			t.Fatalf("Finalize #%d: %v", i, err)
		}
	}
	// pool/ must contain exactly one file with the expected name.
	hash := sha256.Sum256(body)
	target := c.BlobPath(hex.EncodeToString(hash[:]))
	if _, err := os.Stat(target); err != nil {
		t.Errorf("pool/ missing target: %v", err)
	}
	tmpEntries, _ := os.ReadDir(filepath.Join(c.Dir(), "tmp"))
	if len(tmpEntries) != 0 {
		t.Errorf("tmp/ has %d leftover entries, want 0", len(tmpEntries))
	}
}

func TestSweepTmp_OnlyReapsStale(t *testing.T) {
	c := openCache(t)
	tmpDir := filepath.Join(c.Dir(), "tmp")

	stale := filepath.Join(tmpDir, "stale-orphan")
	fresh := filepath.Join(tmpDir, "in-flight-now")
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0o640); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}
	// Backdate "stale" beyond the cutoff.
	old := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if err := c.SweepTmp(5 * time.Minute); err != nil {
		t.Fatalf("SweepTmp: %v", err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale file survived sweep: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh file killed by sweep: %v", err)
	}
}

func TestURLPath_PutLookupRoundtrip(t *testing.T) {
	c := openCache(t)
	hash := strings.Repeat("a", 64)
	// blob_hash has a FK constraint to blob(hash); insert the blob row
	// first (PRAGMA foreign_keys = ON is enforced).
	if err := c.PutBlob(context.Background(), hash, 42); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	etag := `"abc"`
	u := URLPath{
		CanonicalScheme: "http",
		CanonicalHost:   "archive.ubuntu.com",
		Path:            "/ubuntu/dists/noble/InRelease",
		BlobHash:        &hash,
		UpstreamURL:     "http://archive.ubuntu.com/ubuntu/dists/noble/InRelease",
		IsMetadata:      true,
		RequestCount:    7,
		UpstreamETag:    &etag,
	}
	if err := c.PutURLPath(context.Background(), u); err != nil {
		t.Fatalf("PutURLPath: %v", err)
	}
	got, err := c.LookupURL(context.Background(), u.CanonicalScheme, u.CanonicalHost, u.Path)
	if err != nil {
		t.Fatalf("LookupURL: %v", err)
	}
	if got.UpstreamURL != u.UpstreamURL || !got.IsMetadata || got.RequestCount != 7 {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
	if got.BlobHash == nil || *got.BlobHash != hash {
		t.Errorf("blob_hash = %v, want %s", got.BlobHash, hash)
	}
	if got.UpstreamETag == nil || *got.UpstreamETag != etag {
		t.Errorf("etag = %v, want %s", got.UpstreamETag, etag)
	}
}

func TestURLPath_BlobHashFKEnforced(t *testing.T) {
	c := openCache(t)
	bogus := strings.Repeat("9", 64)
	u := URLPath{
		CanonicalScheme: "http",
		CanonicalHost:   "x.example.com",
		Path:            "/p",
		BlobHash:        &bogus,
		UpstreamURL:     "http://x/p",
	}
	err := c.PutURLPath(context.Background(), u)
	if err == nil {
		t.Fatal("expected FK violation for unreferenced blob_hash, got nil")
	}
	if !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Errorf("error %q does not mention FOREIGN KEY", err)
	}
}

func TestURLPath_Lookup_MissReturnsErrNotFound(t *testing.T) {
	c := openCache(t)
	_, err := c.LookupURL(context.Background(), "http", "nope.example.com", "/missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestTouchURLPath_IncrementsCounters(t *testing.T) {
	c := openCache(t)
	u := URLPath{
		CanonicalScheme: "http",
		CanonicalHost:   "x.example.com",
		Path:            "/p",
		UpstreamURL:     "http://x.example.com/p",
		RequestCount:    0,
	}
	if err := c.PutURLPath(context.Background(), u); err != nil {
		t.Fatalf("PutURLPath: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := c.TouchURLPath(context.Background(), u.CanonicalScheme, u.CanonicalHost, u.Path); err != nil {
			t.Fatalf("Touch #%d: %v", i, err)
		}
	}
	got, _ := c.LookupURL(context.Background(), u.CanonicalScheme, u.CanonicalHost, u.Path)
	if got.RequestCount != 3 {
		t.Errorf("request_count = %d, want 3", got.RequestCount)
	}
	if got.LastRequestedAt == nil {
		t.Errorf("last_requested_at remained nil")
	}
}

func TestSuiteFreshness_Roundtrip(t *testing.T) {
	c := openCache(t)
	now := nowUnix()
	etag := `"sha-of-inrelease"`
	s := SuiteFreshness{
		CanonicalScheme: "http",
		CanonicalHost:   "archive.ubuntu.com",
		SuitePath:       "/ubuntu/dists/noble",
		LastCheckAt:     &now,
		LastSuccessAt:   &now,
		InReleaseETag:   &etag,
	}
	if err := c.PutSuiteFreshness(context.Background(), s); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := c.GetSuiteFreshness(context.Background(), s.CanonicalScheme, s.CanonicalHost, s.SuitePath)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LastCheckAt == nil || *got.LastCheckAt != now {
		t.Errorf("last_check_at roundtrip failed: %v", got.LastCheckAt)
	}
}

func TestListSuites(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	got, err := c.ListSuites(ctx)
	if err != nil {
		t.Fatalf("empty ListSuites: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty ListSuites returned %d rows", len(got))
	}

	now := nowUnix()
	rows := []SuiteFreshness{
		{CanonicalScheme: "http", CanonicalHost: "archive.ubuntu.com", SuitePath: "/ubuntu/dists/noble", LastSuccessAt: &now},
		{CanonicalScheme: "http", CanonicalHost: "archive.ubuntu.com", SuitePath: "/ubuntu/dists/jammy", LastSuccessAt: &now},
		{CanonicalScheme: "https", CanonicalHost: "deb.debian.org", SuitePath: "/debian/dists/bookworm", LastSuccessAt: &now},
	}
	for _, r := range rows {
		if err := c.PutSuiteFreshness(ctx, r); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	got, err = c.ListSuites(ctx)
	if err != nil {
		t.Fatalf("ListSuites: %v", err)
	}
	if len(got) != len(rows) {
		t.Errorf("got %d rows, want %d", len(got), len(rows))
	}
	seen := make(map[string]bool)
	for _, r := range got {
		seen[r.CanonicalScheme+"|"+r.CanonicalHost+"|"+r.SuitePath] = true
	}
	for _, r := range rows {
		key := r.CanonicalScheme + "|" + r.CanonicalHost + "|" + r.SuitePath
		if !seen[key] {
			t.Errorf("missing row %q in ListSuites", key)
		}
	}
}

// TestConcurrentWrites verifies that the single-writer goroutine
// serializes writes from many goroutines without SQLITE_BUSY errors.
// This is the gating concurrency invariant for SPEC §9.4.
func TestConcurrentWrites(t *testing.T) {
	c := openCache(t)
	const goroutines = 32
	const opsPerGoroutine = 50

	var wg sync.WaitGroup
	errs := make(chan error, goroutines*opsPerGoroutine)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				u := URLPath{
					CanonicalScheme: "http",
					CanonicalHost:   "archive.ubuntu.com",
					Path:            filepath.Join("/p", "g"+itoa(g), "i"+itoa(i)),
					UpstreamURL:     "http://x/y",
				}
				if err := c.PutURLPath(context.Background(), u); err != nil {
					errs <- err
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent write: %v", err)
	}

	// Verify all rows landed.
	var n int
	if err := c.db.QueryRow(`SELECT count(*) FROM url_path`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if want := goroutines * opsPerGoroutine; n != want {
		t.Errorf("row count = %d, want %d", n, want)
	}
}

// TestClose_RejectsFurtherWrites ensures that submitting a write to a
// closed cache returns ErrClosed rather than blocking or panicking.
func TestClose_RejectsFurtherWrites(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	err = c.PutBlob(context.Background(), strings.Repeat("0", 64), 1)
	if !errors.Is(err, ErrClosed) {
		t.Errorf("PutBlob after Close: got %v, want ErrClosed", err)
	}
	// Idempotent close.
	if err := c.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestClose_DoesNotStrandSubmitWriters runs many concurrent submitters
// against a cache that gets closed mid-flight. Every submitter must
// return within a short bounded time — none may hang on req.res after
// the writer goroutine has exited. Regression for codex review #1.
func TestClose_DoesNotStrandSubmitWriters(t *testing.T) {
	c := openCache(t)
	const submitters = 64

	var wg sync.WaitGroup
	wg.Add(submitters)
	hash := strings.Repeat("0", 64)
	for i := 0; i < submitters; i++ {
		go func(i int) {
			defer wg.Done()
			// Every error here is acceptable — we only care that the
			// goroutine returns.
			_ = c.PutBlob(context.Background(), hash, int64(i+1))
		}(i)
	}

	// Give the submitters a moment to crowd in, then yank the cache out.
	time.Sleep(2 * time.Millisecond)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		// All submitters returned. Pass.
	case <-time.After(5 * time.Second):
		t.Fatal("submitters did not return within 5s after Close — at least one is stranded")
	}
}

func TestPutBlob_RejectsMalformedHash(t *testing.T) {
	c := openCache(t)
	cases := []string{
		"",                      // empty
		strings.Repeat("a", 63), // too short
		strings.Repeat("a", 65), // too long
		strings.Repeat("g", 64), // non-hex
		strings.Repeat("A", 64), // uppercase forbidden
		"../../../etc/passwd",
	}
	for _, h := range cases {
		err := c.PutBlob(context.Background(), h, 1)
		if !errors.Is(err, ErrInvalidHash) {
			t.Errorf("PutBlob(%q): got %v, want ErrInvalidHash", h, err)
		}
	}
}

func TestSchema_RejectsMalformedHashAtDBLayer(t *testing.T) {
	c := openCache(t)
	// Bypass the Go API and try to stuff a malformed hash directly. The
	// SQLite CHECK constraint must reject it.
	_, err := c.db.Exec(`INSERT INTO blob (hash, size, created_at) VALUES (?, 1, 0)`,
		strings.Repeat("z", 64))
	if err == nil {
		t.Fatal("CHECK constraint missing: malformed hex hash was accepted")
	}
	if !strings.Contains(err.Error(), "CHECK") && !strings.Contains(err.Error(), "constraint") {
		t.Errorf("error %q does not mention CHECK constraint", err)
	}
}

func TestBlobPath_PanicsOnMalformedHash(t *testing.T) {
	c := openCache(t)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on malformed hash; got nothing")
		}
	}()
	_ = c.BlobPath("..")
}

func TestBlobExists_RejectsMalformedHash(t *testing.T) {
	c := openCache(t)
	_, err := c.BlobExists("not-a-hash")
	if !errors.Is(err, ErrInvalidHash) {
		t.Errorf("BlobExists: got %v, want ErrInvalidHash", err)
	}
}

func TestOpen_PathWithMetacharsInDir(t *testing.T) {
	// A directory whose name contains `?` would corrupt a string-built
	// SQLite DSN. With url.URL escaping, the path must round-trip
	// cleanly. Regression for codex review #4.
	parent := t.TempDir()
	weird := filepath.Join(parent, "tricky?name#test")
	if err := os.MkdirAll(weird, 0o750); err != nil {
		t.Fatalf("mkdir %q: %v", weird, err)
	}
	c, err := Open(context.Background(), weird, nil)
	if err != nil {
		t.Fatalf("Open(%q): %v", weird, err)
	}
	defer c.Close()
	if _, err := c.db.Exec(`INSERT INTO blob (hash, size, created_at) VALUES (?, 1, 0)`,
		strings.Repeat("a", 64)); err != nil {
		t.Errorf("simple insert into DB at weird path: %v", err)
	}
}

func TestHashReader(t *testing.T) {
	body := []byte("hello world")
	want := sha256.Sum256(body)
	got, err := hashReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("hashReader: %v", err)
	}
	if got != hex.EncodeToString(want[:]) {
		t.Errorf("hash mismatch: got %s, want %s", got, hex.EncodeToString(want[:]))
	}
}

// openV1Cache opens a cache directory and runs ONLY the v0→v1 migration,
// leaving the database at schema_version = 1. Used to exercise the v1→v2
// migration in isolation — calling Open() jumps straight to v2.
//
// Returns the bare *sql.DB and the directory; caller closes both. We
// deliberately don't construct a *Cache, because Cache.Close drives the
// writer goroutine through cache.db at v2-shape; using the same handle
// for migration tests keeps the surface narrow.
func openV1Cache(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"pool", "tmp", "staging"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	db, err := openDB(filepath.Join(dir, "cache.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	if err := applyMigration(context.Background(), db, 0); err != nil {
		_ = db.Close()
		t.Fatalf("applyMigration v0→v1: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, dir
}

// TestMigration_V1ToV2_AddsTablesAndColumn verifies the new tables and
// the suite_freshness.current_snapshot_id column appear with the
// expected shape after applying migrations[1].
func TestMigration_V1ToV2_AddsTablesAndColumn(t *testing.T) {
	db, _ := openV1Cache(t)
	ctx := context.Background()

	// Sanity check: at v1 the new tables don't exist.
	for _, tbl := range []string{"suite_snapshot", "snapshot_member", "package_hash"} {
		var n int
		err := db.QueryRow(
			`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, tbl,
		).Scan(&n)
		if err != nil {
			t.Fatalf("probe %s: %v", tbl, err)
		}
		if n != 0 {
			t.Errorf("v1 db already has %s table; expected pristine v1", tbl)
		}
	}

	// Run v1 → v2.
	if err := applyMigration(ctx, db, 1); err != nil {
		t.Fatalf("applyMigration v1→v2: %v", err)
	}

	// All three new tables exist and accept count(*).
	for _, q := range []string{
		`SELECT count(*) FROM suite_snapshot`,
		`SELECT count(*) FROM snapshot_member`,
		`SELECT count(*) FROM package_hash`,
	} {
		var n int
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Errorf("%q: %v", q, err)
		}
	}

	// suite_freshness gained current_snapshot_id and accepts NULL on
	// pre-existing rows. Probe via PRAGMA table_info.
	rows, err := db.Query(`PRAGMA table_info(suite_freshness)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info: %v", err)
	}
	defer rows.Close()
	saw := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if name == "current_snapshot_id" {
			saw = true
			if ctype != "INTEGER" {
				t.Errorf("current_snapshot_id type=%q, want INTEGER", ctype)
			}
			if notnull != 0 {
				t.Errorf("current_snapshot_id should be nullable; notnull=%d", notnull)
			}
		}
	}
	if !saw {
		t.Error("current_snapshot_id column not added to suite_freshness")
	}

	// schema_version row reports 2 after the migration.
	v, err := readSchemaVersion(ctx, db)
	if err != nil {
		t.Fatalf("readSchemaVersion: %v", err)
	}
	if v != 2 {
		t.Errorf("schema_version = %d, want 2", v)
	}
}

// TestMigration_V1ToV2_PreservesV1Data verifies that pre-existing
// blob/url_path/suite_freshness rows survive the migration intact.
// The "trusted-until-replaced" rule (SPEC2 §4.3.2) requires this.
func TestMigration_V1ToV2_PreservesV1Data(t *testing.T) {
	db, _ := openV1Cache(t)
	ctx := context.Background()

	// Seed v1-shaped rows.
	hash := strings.Repeat("a", 64)
	if _, err := db.Exec(`INSERT INTO blob (hash, size, created_at, refcount) VALUES (?, 42, 100, 1)`, hash); err != nil {
		t.Fatalf("seed blob: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO url_path
		   (canonical_scheme, canonical_host, path, blob_hash,
		    upstream_url, is_metadata, request_count)
		   VALUES ('http', 'archive.ubuntu.com', '/p', ?, 'http://x', 0, 5)`,
		hash,
	); err != nil {
		t.Fatalf("seed url_path: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO suite_freshness
		   (canonical_scheme, canonical_host, suite_path)
		   VALUES ('http', 'archive.ubuntu.com', '/ubuntu/dists/noble')`,
	); err != nil {
		t.Fatalf("seed suite_freshness: %v", err)
	}

	// Apply v1 → v2.
	if err := applyMigration(ctx, db, 1); err != nil {
		t.Fatalf("applyMigration v1→v2: %v", err)
	}

	// Verify rows survive unchanged.
	var size int64
	var refcount int
	if err := db.QueryRow(`SELECT size, refcount FROM blob WHERE hash=?`, hash).Scan(&size, &refcount); err != nil {
		t.Fatalf("query blob: %v", err)
	}
	if size != 42 || refcount != 1 {
		t.Errorf("blob row mutated: size=%d refcount=%d", size, refcount)
	}

	var rc int
	if err := db.QueryRow(
		`SELECT request_count FROM url_path
		   WHERE canonical_scheme='http' AND canonical_host='archive.ubuntu.com' AND path='/p'`,
	).Scan(&rc); err != nil {
		t.Fatalf("query url_path: %v", err)
	}
	if rc != 5 {
		t.Errorf("url_path.request_count mutated: got %d, want 5", rc)
	}

	// suite_freshness row survives and current_snapshot_id is NULL.
	var snap sql.NullInt64
	if err := db.QueryRow(
		`SELECT current_snapshot_id FROM suite_freshness
		   WHERE canonical_scheme='http' AND canonical_host='archive.ubuntu.com'
		     AND suite_path='/ubuntu/dists/noble'`,
	).Scan(&snap); err != nil {
		t.Fatalf("query suite_freshness: %v", err)
	}
	if snap.Valid {
		t.Errorf("current_snapshot_id should be NULL on migrated v1 row; got %d", snap.Int64)
	}
}

// TestMigration_V1ToV2_NewTablesEnforceFKs verifies the FK and CHECK
// constraints on the v2 tables are wired up correctly. A snapshot_member
// row pointing at a non-existent snapshot or non-existent blob must be
// rejected.
func TestMigration_V1ToV2_NewTablesEnforceFKs(t *testing.T) {
	db, _ := openV1Cache(t)
	ctx := context.Background()
	if err := applyMigration(ctx, db, 1); err != nil {
		t.Fatalf("applyMigration v1→v2: %v", err)
	}

	// Insert a real blob for the positive cases.
	hash := strings.Repeat("a", 64)
	if _, err := db.Exec(`INSERT INTO blob (hash, size, created_at) VALUES (?, 1, 0)`, hash); err != nil {
		t.Fatalf("seed blob: %v", err)
	}

	// suite_snapshot: inrelease_hash FK must resolve to blob.hash.
	bogus := strings.Repeat("b", 64)
	_, err := db.Exec(
		`INSERT INTO suite_snapshot
		   (canonical_scheme, canonical_host, suite_path,
		    inrelease_hash, created_at)
		   VALUES ('http', 'x', '/s', ?, 0)`,
		bogus,
	)
	if err == nil {
		t.Error("suite_snapshot accepted dangling inrelease_hash; FK not enforced")
	}

	// Real snapshot insert succeeds.
	res, err := db.Exec(
		`INSERT INTO suite_snapshot
		   (canonical_scheme, canonical_host, suite_path,
		    inrelease_hash, created_at)
		   VALUES ('http', 'x', '/s', ?, 0)`,
		hash,
	)
	if err != nil {
		t.Fatalf("real suite_snapshot: %v", err)
	}
	snapID, _ := res.LastInsertId()

	// snapshot_member: declared_sha256 CHECK rejects malformed.
	for _, bad := range []string{
		strings.Repeat("g", 64), // non-hex
		strings.Repeat("a", 63), // too short
		strings.Repeat("A", 64), // uppercase
	} {
		_, err := db.Exec(
			`INSERT INTO snapshot_member (snapshot_id, path, blob_hash, declared_sha256)
			   VALUES (?, ?, ?, ?)`,
			snapID, "p"+bad[:8], hash, bad,
		)
		if err == nil {
			t.Errorf("snapshot_member accepted malformed declared_sha256 %q", bad)
		}
	}

	// snapshot_member: well-formed insert is accepted.
	if _, err := db.Exec(
		`INSERT INTO snapshot_member (snapshot_id, path, blob_hash, declared_sha256)
		   VALUES (?, ?, ?, ?)`,
		snapID, "main/Packages", hash, hash,
	); err != nil {
		t.Errorf("valid snapshot_member rejected: %v", err)
	}

	// snapshot_member: PK (snapshot_id, path) prevents duplicate paths.
	_, err = db.Exec(
		`INSERT INTO snapshot_member (snapshot_id, path, blob_hash, declared_sha256)
		   VALUES (?, ?, ?, ?)`,
		snapID, "main/Packages", hash, hash,
	)
	if err == nil {
		t.Error("snapshot_member accepted duplicate (snapshot_id, path)")
	}

	// snapshot_member: dangling snapshot_id is rejected (FK to suite_snapshot).
	_, err = db.Exec(
		`INSERT INTO snapshot_member (snapshot_id, path, blob_hash, declared_sha256)
		   VALUES (?, ?, ?, ?)`,
		snapID+99999, "main/i-do-not-exist", hash, hash,
	)
	if err == nil {
		t.Error("snapshot_member accepted dangling snapshot_id; FK not enforced")
	}

	// snapshot_member: dangling blob_hash is rejected (FK to blob).
	_, err = db.Exec(
		`INSERT INTO snapshot_member (snapshot_id, path, blob_hash, declared_sha256)
		   VALUES (?, ?, ?, ?)`,
		snapID, "main/dangle-blob", strings.Repeat("c", 64), hash,
	)
	if err == nil {
		t.Error("snapshot_member accepted dangling blob_hash; FK not enforced")
	}

	// package_hash: CHECK on declared_sha256 + FK on snapshot_id.
	_, err = db.Exec(
		`INSERT INTO package_hash
		   (canonical_scheme, canonical_host, path, declared_sha256, snapshot_id)
		   VALUES ('http', 'x', '/pool/foo.deb', ?, ?)`,
		strings.Repeat("z", 64), snapID,
	)
	if err == nil {
		t.Error("package_hash accepted malformed declared_sha256")
	}

	// package_hash: dangling snapshot_id is rejected.
	_, err = db.Exec(
		`INSERT INTO package_hash
		   (canonical_scheme, canonical_host, path, declared_sha256, snapshot_id)
		   VALUES ('http', 'x', '/pool/foo.deb', ?, ?)`,
		hash, snapID+99999,
	)
	if err == nil {
		t.Error("package_hash accepted dangling snapshot_id; FK not enforced")
	}

	if _, err := db.Exec(
		`INSERT INTO package_hash
		   (canonical_scheme, canonical_host, path, declared_sha256, snapshot_id)
		   VALUES ('http', 'x', '/pool/foo.deb', ?, ?)`,
		hash, snapID,
	); err != nil {
		t.Errorf("valid package_hash rejected: %v", err)
	}

	// suite_freshness.current_snapshot_id is FK-checked too: pointing it at
	// a non-existent snapshot must fail.
	if _, err := db.Exec(
		`INSERT INTO suite_freshness
		   (canonical_scheme, canonical_host, suite_path, current_snapshot_id)
		   VALUES ('http', 'x', '/dangling-suite', ?)`,
		snapID+99999,
	); err == nil {
		t.Error("suite_freshness accepted dangling current_snapshot_id; FK not enforced")
	}
	// And pointing it at a real snapshot succeeds.
	if _, err := db.Exec(
		`INSERT INTO suite_freshness
		   (canonical_scheme, canonical_host, suite_path, current_snapshot_id)
		   VALUES ('http', 'x', '/real-suite', ?)`,
		snapID,
	); err != nil {
		t.Errorf("valid suite_freshness with current_snapshot_id rejected: %v", err)
	}
}

// TestMigration_V1ToV2_HashModeCheck verifies the suite_snapshot CHECK
// constraint enforcing exactly-one-of (inrelease_hash) or (release_hash
// AND release_gpg_hash). Without this CHECK an all-NULL row would slip
// through and bypass the COALESCE-based UNIQUE index entirely.
func TestMigration_V1ToV2_HashModeCheck(t *testing.T) {
	db, _ := openV1Cache(t)
	ctx := context.Background()
	if err := applyMigration(ctx, db, 1); err != nil {
		t.Fatalf("applyMigration v1→v2: %v", err)
	}

	hash := strings.Repeat("a", 64)
	hashB := strings.Repeat("b", 64)
	if _, err := db.Exec(`INSERT INTO blob (hash, size, created_at) VALUES (?, 1, 0)`, hash); err != nil {
		t.Fatalf("seed blob hash: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO blob (hash, size, created_at) VALUES (?, 1, 0)`, hashB); err != nil {
		t.Fatalf("seed blob hashB: %v", err)
	}

	// All-NULL: no inrelease_hash, no release_hash. Must be rejected.
	_, err := db.Exec(
		`INSERT INTO suite_snapshot
		   (canonical_scheme, canonical_host, suite_path, created_at)
		   VALUES ('http', 'x', '/s', 0)`,
	)
	if err == nil {
		t.Error("suite_snapshot accepted all-null hashes; CHECK constraint missing")
	}

	// Both modes set: inrelease_hash AND release_hash both populated. Must be rejected.
	_, err = db.Exec(
		`INSERT INTO suite_snapshot
		   (canonical_scheme, canonical_host, suite_path,
		    inrelease_hash, release_hash, release_gpg_hash, created_at)
		   VALUES ('http', 'x', '/s', ?, ?, ?, 0)`,
		hash, hash, hash,
	)
	if err == nil {
		t.Error("suite_snapshot accepted both inline+detached fields populated; CHECK constraint missing")
	}

	// Detached form with release_hash but missing release_gpg_hash. Must be rejected.
	_, err = db.Exec(
		`INSERT INTO suite_snapshot
		   (canonical_scheme, canonical_host, suite_path, release_hash, created_at)
		   VALUES ('http', 'x', '/s', ?, 0)`,
		hash,
	)
	if err == nil {
		t.Error("suite_snapshot accepted release_hash without release_gpg_hash; CHECK constraint missing")
	}

	// Detached form with release_gpg_hash but missing release_hash. Must be rejected.
	_, err = db.Exec(
		`INSERT INTO suite_snapshot
		   (canonical_scheme, canonical_host, suite_path, release_gpg_hash, created_at)
		   VALUES ('http', 'x', '/s', ?, 0)`,
		hash,
	)
	if err == nil {
		t.Error("suite_snapshot accepted release_gpg_hash without release_hash; CHECK constraint missing")
	}

	// Inline form (only inrelease_hash) is accepted.
	if _, err := db.Exec(
		`INSERT INTO suite_snapshot
		   (canonical_scheme, canonical_host, suite_path, inrelease_hash, created_at)
		   VALUES ('http', 'x', '/inline', ?, 0)`,
		hash,
	); err != nil {
		t.Errorf("valid inline suite_snapshot rejected: %v", err)
	}

	// Detached form (release_hash + release_gpg_hash) is accepted.
	if _, err := db.Exec(
		`INSERT INTO suite_snapshot
		   (canonical_scheme, canonical_host, suite_path, release_hash, release_gpg_hash, created_at)
		   VALUES ('http', 'x', '/detached', ?, ?, 0)`,
		hash, hashB,
	); err != nil {
		t.Errorf("valid detached suite_snapshot rejected: %v", err)
	}
}

// TestMigration_V1ToV2_NaturalKeyUniqueIndex verifies the COALESCE-based
// UNIQUE INDEX on suite_snapshot rejects re-adopting the same content,
// across both the inline (inrelease_hash set) and detached (release_hash
// set) forms.
func TestMigration_V1ToV2_NaturalKeyUniqueIndex(t *testing.T) {
	db, _ := openV1Cache(t)
	ctx := context.Background()
	if err := applyMigration(ctx, db, 1); err != nil {
		t.Fatalf("applyMigration v1→v2: %v", err)
	}
	hashA := strings.Repeat("a", 64)
	hashB := strings.Repeat("b", 64)
	for _, h := range []string{hashA, hashB} {
		if _, err := db.Exec(`INSERT INTO blob (hash, size, created_at) VALUES (?, 1, 0)`, h); err != nil {
			t.Fatalf("seed blob: %v", err)
		}
	}

	// Inline form: insert succeeds once, second identical insert fails.
	if _, err := db.Exec(
		`INSERT INTO suite_snapshot
		   (canonical_scheme, canonical_host, suite_path, inrelease_hash, created_at)
		   VALUES ('http', 'x', '/s', ?, 0)`,
		hashA,
	); err != nil {
		t.Fatalf("first inline insert: %v", err)
	}
	_, err := db.Exec(
		`INSERT INTO suite_snapshot
		   (canonical_scheme, canonical_host, suite_path, inrelease_hash, created_at)
		   VALUES ('http', 'x', '/s', ?, 0)`,
		hashA,
	)
	if err == nil {
		t.Error("duplicate inline (inrelease_hash) snapshot accepted; UNIQUE index missing")
	}

	// A different inrelease_hash for the same suite is allowed (a real
	// upstream change advances inrelease_hash).
	if _, err := db.Exec(
		`INSERT INTO suite_snapshot
		   (canonical_scheme, canonical_host, suite_path, inrelease_hash, created_at)
		   VALUES ('http', 'x', '/s', ?, 0)`,
		hashB,
	); err != nil {
		t.Errorf("distinct inrelease_hash for same suite rejected: %v", err)
	}

	// Detached form on a different suite: same uniqueness on release_hash.
	if _, err := db.Exec(
		`INSERT INTO suite_snapshot
		   (canonical_scheme, canonical_host, suite_path, release_hash, release_gpg_hash, created_at)
		   VALUES ('http', 'x', '/det', ?, ?, 0)`,
		hashA, hashB,
	); err != nil {
		t.Fatalf("first detached insert: %v", err)
	}
	_, err = db.Exec(
		`INSERT INTO suite_snapshot
		   (canonical_scheme, canonical_host, suite_path, release_hash, release_gpg_hash, created_at)
		   VALUES ('http', 'x', '/det', ?, ?, 0)`,
		hashA, hashB,
	)
	if err == nil {
		t.Error("duplicate detached (release_hash) snapshot accepted; UNIQUE index missing")
	}
}

// TestMigration_V1ToV2_AtomicRollback verifies an interrupted v1→v2
// migration leaves the database at v1, not partially applied. Simulated
// by injecting a pre-existing object that collides with one of the
// CREATE statements (here: an existing index name) so the migration
// transaction must roll back.
func TestMigration_V1ToV2_AtomicRollback(t *testing.T) {
	db, _ := openV1Cache(t)
	ctx := context.Background()

	// Plant a pre-existing object with a name the migration tries to
	// create. The CREATE UNIQUE INDEX in the migration will hit this
	// and the whole transaction aborts.
	if _, err := db.Exec(
		`CREATE TABLE idx_suite_snapshot_natural (x INTEGER PRIMARY KEY)`,
	); err != nil {
		t.Fatalf("plant collision: %v", err)
	}

	err := applyMigration(ctx, db, 1)
	if err == nil {
		t.Fatal("expected migration error on name collision; got nil")
	}

	// Schema version is still 1.
	v, err := readSchemaVersion(ctx, db)
	if err != nil {
		t.Fatalf("readSchemaVersion: %v", err)
	}
	if v != 1 {
		t.Errorf("after rollback, schema_version = %d, want 1", v)
	}

	// Tables that the migration was creating must NOT exist (the tx
	// rolled back).
	for _, tbl := range []string{"suite_snapshot", "snapshot_member", "package_hash"} {
		var n int
		err := db.QueryRow(
			`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, tbl,
		).Scan(&n)
		if err != nil {
			t.Fatalf("probe %s: %v", tbl, err)
		}
		if n != 0 {
			t.Errorf("after rollback, %s table exists; tx was not atomic", tbl)
		}
	}
}

// openV2Cache opens a cache directory and runs migrations 0→1→2,
// leaving the database at schema_version = 2. Used to exercise the
// v2→v3 migration in isolation.
func openV2Cache(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"pool", "tmp", "staging"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	db, err := openDB(filepath.Join(dir, "cache.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	for v := 0; v < 2; v++ {
		if err := applyMigration(context.Background(), db, v); err != nil {
			_ = db.Close()
			t.Fatalf("applyMigration v%d→v%d: %v", v, v+1, err)
		}
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, dir
}

// TestMigration_V2ToV3_AddsColumnsAndIndex verifies migrations[2]
// adds package_name + architecture to package_hash, the
// idx_package_hash_pkg_arch index, and package_coverage_complete on
// suite_snapshot. SPEC3 §4.3.1.
func TestMigration_V2ToV3_AddsColumnsAndIndex(t *testing.T) {
	db, _ := openV2Cache(t)
	ctx := context.Background()

	// At v2, the new columns / index do not exist.
	for _, tc := range []struct{ table, col string }{
		{"package_hash", "package_name"},
		{"package_hash", "architecture"},
		{"suite_snapshot", "package_coverage_complete"},
	} {
		if hasColumn(t, db, tc.table, tc.col) {
			t.Errorf("v2 db already has %s.%s; expected pristine v2", tc.table, tc.col)
		}
	}
	if hasIndex(t, db, "idx_package_hash_pkg_arch") {
		t.Error("v2 db already has idx_package_hash_pkg_arch; expected pristine v2")
	}

	if err := applyMigration(ctx, db, 2); err != nil {
		t.Fatalf("applyMigration v2→v3: %v", err)
	}

	for _, tc := range []struct{ table, col string }{
		{"package_hash", "package_name"},
		{"package_hash", "architecture"},
		{"suite_snapshot", "package_coverage_complete"},
	} {
		if !hasColumn(t, db, tc.table, tc.col) {
			t.Errorf("after migration, %s.%s missing", tc.table, tc.col)
		}
	}
	if !hasIndex(t, db, "idx_package_hash_pkg_arch") {
		t.Error("after migration, idx_package_hash_pkg_arch missing")
	}

	v, err := readSchemaVersion(ctx, db)
	if err != nil {
		t.Fatalf("readSchemaVersion: %v", err)
	}
	if v != 3 {
		t.Errorf("schema_version = %d, want 3", v)
	}
}

// TestMigration_V2ToV3_PreservesV2Data: pre-existing package_hash and
// suite_snapshot rows survive intact. The new columns default — empty
// string for the package_hash text columns, 0 for
// package_coverage_complete. SPEC3 §4.3.2.
func TestMigration_V2ToV3_PreservesV2Data(t *testing.T) {
	db, _ := openV2Cache(t)
	ctx := context.Background()

	hashA := strings.Repeat("a", 64)
	hashB := strings.Repeat("b", 64)

	if _, err := db.Exec(`INSERT INTO blob (hash, size, created_at) VALUES (?, 1, 0)`, hashA); err != nil {
		t.Fatalf("seed blob A: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO blob (hash, size, created_at) VALUES (?, 1, 0)`, hashB); err != nil {
		t.Fatalf("seed blob B: %v", err)
	}
	res, err := db.Exec(`INSERT INTO suite_snapshot
	    (canonical_scheme, canonical_host, suite_path, inrelease_hash, created_at)
	    VALUES ('http', 'x.example', '/p', ?, 100)`, hashA)
	if err != nil {
		t.Fatalf("seed v2 suite_snapshot: %v", err)
	}
	snapID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO package_hash
	    (canonical_scheme, canonical_host, path, declared_sha256, snapshot_id)
	    VALUES ('http', 'x.example', '/pool/foo.deb', ?, ?)`, hashB, snapID); err != nil {
		t.Fatalf("seed v2 package_hash: %v", err)
	}

	if err := applyMigration(ctx, db, 2); err != nil {
		t.Fatalf("applyMigration v2→v3: %v", err)
	}

	// Pre-v3 package_hash row: package_name + architecture default to
	// empty strings.
	var pkg, arch string
	if err := db.QueryRow(`SELECT package_name, architecture FROM package_hash`).Scan(&pkg, &arch); err != nil {
		t.Fatalf("read post-migration package_hash: %v", err)
	}
	if pkg != "" {
		t.Errorf("pre-v3 package_hash.package_name = %q, want empty string", pkg)
	}
	if arch != "" {
		t.Errorf("pre-v3 package_hash.architecture = %q, want empty string", arch)
	}

	// Pre-v3 suite_snapshot row: package_coverage_complete defaults to 0.
	var coverage int
	if err := db.QueryRow(`SELECT package_coverage_complete FROM suite_snapshot WHERE snapshot_id = ?`, snapID).Scan(&coverage); err != nil {
		t.Fatalf("read post-migration coverage: %v", err)
	}
	if coverage != 0 {
		t.Errorf("pre-v3 suite_snapshot.package_coverage_complete = %d, want 0 (conservative default)", coverage)
	}
}

// TestMigration_V2ToV3_RejectsBadCoverageValue confirms the CHECK on
// package_coverage_complete actually fires. A future caller that
// tries to write a sentinel like -1 must fail closed.
func TestMigration_V2ToV3_RejectsBadCoverageValue(t *testing.T) {
	db, _ := openV2Cache(t)
	ctx := context.Background()
	if err := applyMigration(ctx, db, 2); err != nil {
		t.Fatalf("applyMigration v2→v3: %v", err)
	}

	hash := strings.Repeat("a", 64)
	if _, err := db.Exec(`INSERT INTO blob (hash, size, created_at) VALUES (?, 1, 0)`, hash); err != nil {
		t.Fatalf("seed blob: %v", err)
	}
	_, err := db.Exec(`INSERT INTO suite_snapshot
	    (canonical_scheme, canonical_host, suite_path, inrelease_hash, created_at, package_coverage_complete)
	    VALUES ('http', 'x.example', '/p', ?, 100, 2)`, hash)
	if err == nil {
		t.Fatal("expected CHECK violation for package_coverage_complete = 2; got nil")
	}
}

// hasColumn / hasIndex are tiny PRAGMA / sqlite_master probes for the
// migration assertions above.
func hasColumn(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid           int
			name, ctype   string
			notnull, pk   int
			dfltValueRaw  sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValueRaw, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if name == column {
			return true
		}
	}
	return false
}

func hasIndex(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, name,
	).Scan(&n); err != nil {
		t.Fatalf("probe index %s: %v", name, err)
	}
	return n > 0
}

// itoa avoids pulling strconv into the test file just for goroutine IDs.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// seedBlob persists a blob row + on-disk file with the given content
// via the cache's normal write path, returning the sha256 hex hash.
// Used by adoption tests as a substitute for the real fetch+writeBlob
// flow that lives in the freshness package.
func seedBlob(t *testing.T, c *Cache, content string) string {
	t.Helper()
	w, err := c.NewTempBlob()
	if err != nil {
		t.Fatalf("NewTempBlob: %v", err)
	}
	if _, err := w.Write([]byte(content)); err != nil {
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

// blobRefcount reads blob.refcount for a hash. Tests use this to
// assert the SPEC2 §7.5.1 refcount bookkeeping.
func blobRefcount(t *testing.T, c *Cache, hash string) int64 {
	t.Helper()
	var n int64
	err := c.db.QueryRow(`SELECT refcount FROM blob WHERE hash = ?`, hash).Scan(&n)
	if err != nil {
		t.Fatalf("read refcount of %s: %v", hash, err)
	}
	return n
}

func TestInsertCandidateSnapshot_InlineMode(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	h := seedBlob(t, c, "fake InRelease bytes")

	etag := `"etag-1"`
	lastmod := "Thu, 25 Apr 2024 15:08:24 UTC"
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme:  "http",
		CanonicalHost:    "archive.ubuntu.com",
		SuitePath:        "/ubuntu/dists/noble",
		InReleaseHash:    &h,
		InReleaseETag:    &etag,
		InReleaseLastMod: &lastmod,
	})
	if err != nil {
		t.Fatalf("InsertCandidateSnapshot: %v", err)
	}
	if id <= 0 {
		t.Fatalf("snapshot_id = %d, want positive", id)
	}

	got, err := c.GetSuiteSnapshot(ctx, id)
	if err != nil {
		t.Fatalf("GetSuiteSnapshot: %v", err)
	}
	if got.AdoptedAt != nil {
		t.Errorf("candidate adopted_at = %v, want NULL", *got.AdoptedAt)
	}
	if got.InReleaseHash == nil || *got.InReleaseHash != h {
		t.Errorf("InReleaseHash mismatch: %v", got.InReleaseHash)
	}
	if got.ReleaseHash != nil || got.ReleaseGPGHash != nil {
		t.Errorf("detached fields should be NULL: %+v", got)
	}
}

func TestInsertCandidateSnapshot_DetachedMode(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	rh := seedBlob(t, c, "fake Release bytes")
	gh := seedBlob(t, c, "fake Release.gpg bytes")

	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "deb.debian.org",
		SuitePath:       "/debian/dists/bookworm",
		ReleaseHash:     &rh,
		ReleaseGPGHash:  &gh,
	})
	if err != nil {
		t.Fatalf("InsertCandidateSnapshot detached: %v", err)
	}
	got, err := c.GetSuiteSnapshot(ctx, id)
	if err != nil {
		t.Fatalf("GetSuiteSnapshot: %v", err)
	}
	if got.InReleaseHash != nil {
		t.Errorf("inline field should be NULL in detached mode: %v", got.InReleaseHash)
	}
	if got.ReleaseHash == nil || *got.ReleaseHash != rh {
		t.Errorf("ReleaseHash mismatch: %v", got.ReleaseHash)
	}
	if got.ReleaseGPGHash == nil || *got.ReleaseGPGHash != gh {
		t.Errorf("ReleaseGPGHash mismatch: %v", got.ReleaseGPGHash)
	}
}

func TestInsertCandidateSnapshot_RejectsDanglingBlobFK(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	dangling := strings.Repeat("0", 64) // valid hex shape but no blob row
	_, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "archive.ubuntu.com",
		SuitePath:       "/ubuntu/dists/noble",
		InReleaseHash:   &dangling,
	})
	if err == nil {
		t.Fatal("expected FK violation, got nil")
	}
}

func TestInsertCandidateSnapshot_RejectsBothModes(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	ih := seedBlob(t, c, "inline")
	rh := seedBlob(t, c, "release")
	gh := seedBlob(t, c, "release-gpg")

	_, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "x.example",
		SuitePath:       "/p",
		InReleaseHash:   &ih,
		ReleaseHash:     &rh,
		ReleaseGPGHash:  &gh,
	})
	if err == nil {
		t.Fatal("expected CHECK violation for both-modes, got nil")
	}
}

func TestInsertCandidateSnapshot_RejectsAllNull(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	_, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "x.example",
		SuitePath:       "/p",
	})
	if err == nil {
		t.Fatal("expected CHECK violation for all-NULL, got nil")
	}
}

// TestInsertCandidateSnapshot_NaturalKeyReuse asserts the
// idempotent-on-collision behavior: a second insert with the same
// natural key reuses the existing unadopted candidate's snapshot_id
// instead of failing with a UNIQUE constraint error. This is the
// fix for the "InsertCandidateSnapshot WARN storm" production
// incident — see plan
// "Fix idx_suite_snapshot_natural UNIQUE-constraint storm".
func TestInsertCandidateSnapshot_NaturalKeyReuse(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	h := seedBlob(t, c, "same InRelease")
	cand := SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "archive.ubuntu.com",
		SuitePath:       "/ubuntu/dists/noble",
		InReleaseHash:   &h,
	}
	id1, reused1, err := c.InsertCandidateSnapshot(ctx, cand)
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if reused1 {
		t.Errorf("first insert: reused=true, want false")
	}
	if id1 == 0 {
		t.Fatalf("first insert: id=0, want >0")
	}
	id2, reused2, err := c.InsertCandidateSnapshot(ctx, cand)
	if err != nil {
		t.Fatalf("second insert: want nil err (reuse), got %v", err)
	}
	if !reused2 {
		t.Errorf("second insert: reused=false, want true")
	}
	if id2 != id1 {
		t.Errorf("second insert: id=%d, want %d (reused row)", id2, id1)
	}
}

// TestInsertCandidateSnapshot_NaturalKeyAdoptedConflict asserts that
// a natural-key collision against an *adopted* row (adopted_at IS NOT
// NULL) returns ErrSnapshotNaturalKeyAdopted instead of silently
// reusing the row — auto-reusing an adopted row would bypass the
// snapshot lifecycle and refcount accounting.
func TestInsertCandidateSnapshot_NaturalKeyAdoptedConflict(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	h := seedBlob(t, c, "adopted InRelease")
	cand := SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "archive.ubuntu.com",
		SuitePath:       "/ubuntu/dists/noble",
		InReleaseHash:   &h,
	}
	id, _, err := c.InsertCandidateSnapshot(ctx, cand)
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := c.CommitAdoption(ctx, id, nil, nil, nil, false); err != nil {
		t.Fatalf("CommitAdoption: %v", err)
	}
	id2, reused, err := c.InsertCandidateSnapshot(ctx, cand)
	if err == nil {
		t.Fatalf("second insert (post-adoption): want error, got id=%d reused=%v", id2, reused)
	}
	if !errors.Is(err, ErrSnapshotNaturalKeyAdopted) {
		t.Errorf("second insert: want ErrSnapshotNaturalKeyAdopted, got %v", err)
	}
	if id2 != 0 {
		t.Errorf("second insert: id=%d, want 0 on error", id2)
	}
	if reused {
		t.Errorf("second insert: reused=true on error")
	}
}

// TestInsertCandidateSnapshot_ReuseRefreshesMutableCols covers the
// detached-mode case where the same Release bytes are paired with a
// different Release.gpg signature on retry. The natural key only
// considers COALESCE(inrelease_hash, release_hash), so the orphan is
// reusable; but its release_gpg_hash column must be refreshed to the
// retry's signature so it stays consistent with the snapshot_member
// row CommitAdoption will write at path "Release.gpg". Validators are
// also refreshed for the same reason. Regression guard for codex
// review finding on commit f5bf699.
func TestInsertCandidateSnapshot_ReuseRefreshesMutableCols(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	releaseHash := seedBlob(t, c, "Release bytes (stable)")
	sigHash1 := seedBlob(t, c, "Release.gpg sig v1")
	sigHash2 := seedBlob(t, c, "Release.gpg sig v2")
	etag1, lm1 := "etag-v1", "lastmod-v1"
	etag2, lm2 := "etag-v2", "lastmod-v2"

	// First attempt: detached candidate with sig v1.
	id1, reused1, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme:  "http",
		CanonicalHost:    "archive.ubuntu.com",
		SuitePath:        "/ubuntu/dists/noble",
		ReleaseHash:      &releaseHash,
		ReleaseGPGHash:   &sigHash1,
		InReleaseETag:    &etag1,
		InReleaseLastMod: &lm1,
	})
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if reused1 {
		t.Errorf("first insert: reused=true, want false")
	}

	// Second attempt: same Release bytes, fresh signature + validators.
	id2, reused2, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme:  "http",
		CanonicalHost:    "archive.ubuntu.com",
		SuitePath:        "/ubuntu/dists/noble",
		ReleaseHash:      &releaseHash,
		ReleaseGPGHash:   &sigHash2,
		InReleaseETag:    &etag2,
		InReleaseLastMod: &lm2,
	})
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if !reused2 {
		t.Errorf("second insert: reused=false, want true")
	}
	if id2 != id1 {
		t.Fatalf("second insert: id=%d, want %d (reused)", id2, id1)
	}

	// The reused row must now report the v2 signature and validators —
	// without this refresh, suite_snapshot.release_gpg_hash would lie
	// about which signature blob is paired with this Release.
	snap, err := c.GetSuiteSnapshot(ctx, id2)
	if err != nil {
		t.Fatalf("GetSuiteSnapshot: %v", err)
	}
	if snap.ReleaseGPGHash == nil || *snap.ReleaseGPGHash != sigHash2 {
		got := "<nil>"
		if snap.ReleaseGPGHash != nil {
			got = *snap.ReleaseGPGHash
		}
		t.Errorf("release_gpg_hash = %s, want %s", got, sigHash2)
	}
	if snap.InReleaseETag == nil || *snap.InReleaseETag != etag2 {
		got := "<nil>"
		if snap.InReleaseETag != nil {
			got = *snap.InReleaseETag
		}
		t.Errorf("inrelease_etag = %q, want %q", got, etag2)
	}
	if snap.InReleaseLastMod == nil || *snap.InReleaseLastMod != lm2 {
		got := "<nil>"
		if snap.InReleaseLastMod != nil {
			got = *snap.InReleaseLastMod
		}
		t.Errorf("inrelease_lastmod = %q, want %q", got, lm2)
	}
}

func TestCommitAdoption_FirstAdoption(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	// Verified Release blob.
	releaseBlob := seedBlob(t, c, "fake InRelease")
	// Three member blobs.
	pkgsBlob := seedBlob(t, c, "Packages content")
	pkgsGzBlob := seedBlob(t, c, "Packages.gz content")
	srcBlob := seedBlob(t, c, "Sources content")

	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "archive.ubuntu.com",
		SuitePath:       "/ubuntu/dists/noble",
		InReleaseHash:   &releaseBlob,
	})
	if err != nil {
		t.Fatalf("InsertCandidateSnapshot: %v", err)
	}

	members := []SnapshotMember{
		{SnapshotID: id, Path: "InRelease", BlobHash: releaseBlob, DeclaredSHA256: releaseBlob},
		{SnapshotID: id, Path: "main/binary-amd64/Packages", BlobHash: pkgsBlob, DeclaredSHA256: pkgsBlob},
		{SnapshotID: id, Path: "main/binary-amd64/Packages.gz", BlobHash: pkgsGzBlob, DeclaredSHA256: pkgsGzBlob},
		{SnapshotID: id, Path: "main/source/Sources", BlobHash: srcBlob, DeclaredSHA256: srcBlob},
	}
	debHash := strings.Repeat("a", 64)
	pkgs := []PackageHash{
		{
			CanonicalScheme: "http",
			CanonicalHost:   "archive.ubuntu.com",
			Path:            "/ubuntu/pool/main/f/foo/foo_1.deb",
			DeclaredSHA256:  debHash,
			SnapshotID:      id,
		},
	}
	if err := c.CommitAdoption(ctx, id, members, pkgs, nil, false); err != nil {
		t.Fatalf("CommitAdoption: %v", err)
	}

	// adopted_at stamped.
	got, err := c.GetSuiteSnapshot(ctx, id)
	if err != nil {
		t.Fatalf("GetSuiteSnapshot post-commit: %v", err)
	}
	if got.AdoptedAt == nil {
		t.Errorf("adopted_at not set after commit")
	}

	// suite_freshness pointer flipped.
	sf, err := c.GetSuiteFreshness(ctx, "http", "archive.ubuntu.com", "/ubuntu/dists/noble")
	if err != nil {
		t.Fatalf("GetSuiteFreshness post-commit: %v", err)
	}
	if sf.CurrentSnapshotID == nil || *sf.CurrentSnapshotID != id {
		t.Errorf("current_snapshot_id = %v, want %d", sf.CurrentSnapshotID, id)
	}

	// All four member blobs have refcount = 1.
	for _, h := range []string{releaseBlob, pkgsBlob, pkgsGzBlob, srcBlob} {
		if got := blobRefcount(t, c, h); got != 1 {
			t.Errorf("refcount %s = %d, want 1", h, got)
		}
	}

	// snapshot_member rows exist.
	gotMembers, err := c.ListSnapshotMembers(ctx, id)
	if err != nil {
		t.Fatalf("ListSnapshotMembers: %v", err)
	}
	if len(gotMembers) != len(members) {
		t.Errorf("got %d snapshot_member rows, want %d", len(gotMembers), len(members))
	}

	// package_hash row exists and is queryable.
	ph, err := c.GetPackageHash(ctx,
		"http", "archive.ubuntu.com", "/ubuntu/pool/main/f/foo/foo_1.deb", id)
	if err != nil {
		t.Fatalf("GetPackageHash: %v", err)
	}
	if ph.DeclaredSHA256 != debHash {
		t.Errorf("package_hash.declared_sha256 = %s, want %s", ph.DeclaredSHA256, debHash)
	}
}

func TestCommitAdoption_DisplacesPrior(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	// First snapshot with two members.
	r1 := seedBlob(t, c, "Release v1")
	m1 := seedBlob(t, c, "Member1 v1")
	m2 := seedBlob(t, c, "Member2 v1")
	id1, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "x.example",
		SuitePath:       "/p",
		InReleaseHash:   &r1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id1, []SnapshotMember{
		{SnapshotID: id1, Path: "InRelease", BlobHash: r1, DeclaredSHA256: r1},
		{SnapshotID: id1, Path: "M1", BlobHash: m1, DeclaredSHA256: m1},
		{SnapshotID: id1, Path: "M2", BlobHash: m2, DeclaredSHA256: m2},
	}, nil, nil, false); err != nil {
		t.Fatalf("commit #1: %v", err)
	}

	// Second snapshot: replaces M2 with M2v2; carries r1 and m1 forward.
	r2 := seedBlob(t, c, "Release v2")
	m2v2 := seedBlob(t, c, "Member2 v2")
	id2, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "x.example",
		SuitePath:       "/p",
		InReleaseHash:   &r2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id2, []SnapshotMember{
		{SnapshotID: id2, Path: "InRelease", BlobHash: r2, DeclaredSHA256: r2},
		{SnapshotID: id2, Path: "M1", BlobHash: m1, DeclaredSHA256: m1},
		{SnapshotID: id2, Path: "M2", BlobHash: m2v2, DeclaredSHA256: m2v2},
	}, nil, nil, false); err != nil {
		t.Fatalf("commit #2: %v", err)
	}

	// suite_freshness now points to id2.
	sf, _ := c.GetSuiteFreshness(ctx, "http", "x.example", "/p")
	if sf.CurrentSnapshotID == nil || *sf.CurrentSnapshotID != id2 {
		t.Errorf("current_snapshot_id = %v, want %d", sf.CurrentSnapshotID, id2)
	}

	// Refcount expectations:
	//  - r1: was in snap1 only → +1 then -1 → 0.
	//  - r2: in snap2 only → +1.
	//  - m1: carried (in both) → +1 (snap1 commit) +1 -1 (snap2 commit) → 1.
	//  - m2: in snap1 only → +1 then -1 → 0.
	//  - m2v2: in snap2 only → +1.
	cases := []struct {
		hash string
		want int64
	}{
		{r1, 0},
		{r2, 1},
		{m1, 1},
		{m2, 0},
		{m2v2, 1},
	}
	for _, tc := range cases {
		if got := blobRefcount(t, c, tc.hash); got != tc.want {
			t.Errorf("refcount %s = %d, want %d", tc.hash, got, tc.want)
		}
	}
}

func TestCommitAdoption_RejectsAlreadyAdopted(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	r := seedBlob(t, c, "InRelease")
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http",
		CanonicalHost:   "x.example",
		SuitePath:       "/p",
		InReleaseHash:   &r,
	})
	if err != nil {
		t.Fatal(err)
	}
	members := []SnapshotMember{
		{SnapshotID: id, Path: "InRelease", BlobHash: r, DeclaredSHA256: r},
	}
	if err := c.CommitAdoption(ctx, id, members, nil, nil, false); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	// Capture pre-state to confirm a second commit changes nothing.
	pre := blobRefcount(t, c, r)

	err = c.CommitAdoption(ctx, id, members, nil, nil, false)
	if !errors.Is(err, ErrSnapshotAlreadyAdopted) {
		t.Fatalf("second commit: got %v, want ErrSnapshotAlreadyAdopted", err)
	}
	if got := blobRefcount(t, c, r); got != pre {
		t.Errorf("refcount changed on rejected re-commit: was %d, now %d", pre, got)
	}
}

func TestCommitAdoption_NonexistentSnapshot(t *testing.T) {
	c := openCache(t)
	err := c.CommitAdoption(context.Background(), 99999, nil, nil, nil, false)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("got %v, want not-found error", err)
	}
}

func TestCommitAdoption_RejectsMalformedMemberHash(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	r := seedBlob(t, c, "InRelease")
	id, _, _ := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "x.example", SuitePath: "/p",
		InReleaseHash: &r,
	})
	bad := []SnapshotMember{
		{SnapshotID: id, Path: "InRelease", BlobHash: r, DeclaredSHA256: r},
		{SnapshotID: id, Path: "M1", BlobHash: "not-a-hex", DeclaredSHA256: r},
	}
	err := c.CommitAdoption(ctx, id, bad, nil, nil, false)
	if err == nil || !errors.Is(err, ErrInvalidHash) {
		t.Fatalf("got %v, want ErrInvalidHash", err)
	}
	// Atomicity: the first member must not have been committed.
	if got, err := c.ListSnapshotMembers(ctx, id); err != nil || len(got) != 0 {
		t.Errorf("expected zero members after rollback, got %d, err=%v", len(got), err)
	}
	if got := blobRefcount(t, c, r); got != 0 {
		t.Errorf("refcount changed despite rollback: %d", got)
	}
}

func TestCommitAdoption_RejectsDanglingBlobFK(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	r := seedBlob(t, c, "InRelease")
	id, _, _ := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "x.example", SuitePath: "/p",
		InReleaseHash: &r,
	})
	dangling := strings.Repeat("b", 64) // valid shape, no blob row
	bad := []SnapshotMember{
		{SnapshotID: id, Path: "InRelease", BlobHash: r, DeclaredSHA256: r},
		{SnapshotID: id, Path: "M1", BlobHash: dangling, DeclaredSHA256: dangling},
	}
	err := c.CommitAdoption(ctx, id, bad, nil, nil, false)
	if err == nil {
		t.Fatal("expected FK error, got nil")
	}
	// First member must not have survived rollback.
	if got, _ := c.ListSnapshotMembers(ctx, id); len(got) != 0 {
		t.Errorf("expected zero members after FK rollback, got %d", len(got))
	}
}

func TestCommitAdoption_RejectsDuplicatePath(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	r := seedBlob(t, c, "InRelease")
	m := seedBlob(t, c, "M1")
	id, _, _ := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "x.example", SuitePath: "/p",
		InReleaseHash: &r,
	})
	dupe := []SnapshotMember{
		{SnapshotID: id, Path: "Path", BlobHash: r, DeclaredSHA256: r},
		{SnapshotID: id, Path: "Path", BlobHash: m, DeclaredSHA256: m},
	}
	err := c.CommitAdoption(ctx, id, dupe, nil, nil, false)
	if err == nil {
		t.Fatal("expected unique violation, got nil")
	}
	if got, _ := c.ListSnapshotMembers(ctx, id); len(got) != 0 {
		t.Errorf("expected zero members after rollback, got %d", len(got))
	}
}

func TestCommitAdoption_EmptyMembersStillFlips(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	r := seedBlob(t, c, "InRelease")
	id, _, _ := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "x.example", SuitePath: "/p",
		InReleaseHash: &r,
	})
	if err := c.CommitAdoption(ctx, id, nil, nil, nil, false); err != nil {
		t.Fatalf("CommitAdoption: %v", err)
	}
	sf, err := c.GetSuiteFreshness(ctx, "http", "x.example", "/p")
	if err != nil {
		t.Fatalf("GetSuiteFreshness: %v", err)
	}
	if sf.CurrentSnapshotID == nil || *sf.CurrentSnapshotID != id {
		t.Errorf("current_snapshot_id = %v, want %d", sf.CurrentSnapshotID, id)
	}
	got, _ := c.GetSuiteSnapshot(ctx, id)
	if got.AdoptedAt == nil {
		t.Errorf("adopted_at not stamped on empty-members commit")
	}
}

func TestCommitAdoption_PreservesExistingSuiteFreshnessColumns(t *testing.T) {
	// The freshness checker writes last_check_at, validators, etc. before
	// adoption fires. CommitAdoption must flip current_snapshot_id without
	// clobbering those columns.
	c := openCache(t)
	ctx := context.Background()
	now := nowUnix()
	etag := `"upstream-etag"`
	if err := c.PutSuiteFreshness(ctx, SuiteFreshness{
		CanonicalScheme: "http",
		CanonicalHost:   "x.example",
		SuitePath:       "/p",
		LastCheckAt:     &now,
		LastSuccessAt:   &now,
		InReleaseETag:   &etag,
	}); err != nil {
		t.Fatal(err)
	}
	r := seedBlob(t, c, "InRelease")
	id, _, _ := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "x.example", SuitePath: "/p",
		InReleaseHash: &r,
	})
	if err := c.CommitAdoption(ctx, id,
		[]SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: r, DeclaredSHA256: r}},
		nil, nil, false); err != nil {
		t.Fatal(err)
	}
	sf, err := c.GetSuiteFreshness(ctx, "http", "x.example", "/p")
	if err != nil {
		t.Fatal(err)
	}
	if sf.LastCheckAt == nil || *sf.LastCheckAt != now {
		t.Errorf("last_check_at clobbered: %v", sf.LastCheckAt)
	}
	if sf.InReleaseETag == nil || *sf.InReleaseETag != etag {
		t.Errorf("inrelease_etag clobbered: %v", sf.InReleaseETag)
	}
	if sf.CurrentSnapshotID == nil || *sf.CurrentSnapshotID != id {
		t.Errorf("current_snapshot_id not flipped: %v", sf.CurrentSnapshotID)
	}
}

func TestPutSuiteFreshness_PreservesCurrentSnapshotID(t *testing.T) {
	// Phase 1 freshness checks call PutSuiteFreshness on every probe.
	// The Phase 2 adoption pointer must survive those writes — otherwise
	// every freshness tick would silently un-adopt the suite.
	c := openCache(t)
	ctx := context.Background()
	r := seedBlob(t, c, "InRelease")
	id, _, _ := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "x.example", SuitePath: "/p",
		InReleaseHash: &r,
	})
	if err := c.CommitAdoption(ctx, id,
		[]SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: r, DeclaredSHA256: r}},
		nil, nil, false); err != nil {
		t.Fatal(err)
	}
	// Now do a Phase-1-style freshness write on the same suite.
	now := nowUnix()
	if err := c.PutSuiteFreshness(ctx, SuiteFreshness{
		CanonicalScheme: "http",
		CanonicalHost:   "x.example",
		SuitePath:       "/p",
		LastCheckAt:     &now,
		LastSuccessAt:   &now,
	}); err != nil {
		t.Fatal(err)
	}
	sf, err := c.GetSuiteFreshness(ctx, "http", "x.example", "/p")
	if err != nil {
		t.Fatal(err)
	}
	if sf.CurrentSnapshotID == nil || *sf.CurrentSnapshotID != id {
		t.Errorf("PutSuiteFreshness clobbered current_snapshot_id: got %v, want %d",
			sf.CurrentSnapshotID, id)
	}
}

func TestGetSnapshotMember_NotFound(t *testing.T) {
	c := openCache(t)
	_, err := c.GetSnapshotMember(context.Background(), 9999, "no/such/path")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestGetPackageHash_NotFound(t *testing.T) {
	c := openCache(t)
	_, err := c.GetPackageHash(context.Background(),
		"http", "x.example", "/no", 9999)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestGetSuiteSnapshot_NotFound(t *testing.T) {
	c := openCache(t)
	_, err := c.GetSuiteSnapshot(context.Background(), 9999)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// TestDeclaredHashesForPath_EmptyWhenNoSnapshotCovers covers SPEC2 §6.1
// step 3: a path with no package_hash row under any current snapshot
// returns nil — the Phase 1 trust-upstream regime.
func TestDeclaredHashesForPath_EmptyWhenNoSnapshotCovers(t *testing.T) {
	c := openCache(t)
	got, err := c.DeclaredHashesForPath(context.Background(),
		"http", "archive.example", "/pool/main/x/x_1.deb")
	if err != nil {
		t.Fatalf("DeclaredHashesForPath: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected nil, got %v", got)
	}
}

// TestDeclaredHashesForPath_ReturnsCurrentSnapshotRowsOnly covers the
// join: only rows whose snapshot_id appears as suite_freshness.current_snapshot_id
// count. An orphaned snapshot's package_hash row must NOT surface.
func TestDeclaredHashesForPath_ReturnsCurrentSnapshotRowsOnly(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	const (
		scheme = "http"
		host   = "archive.example"
		debP   = "/pool/main/h/hello/hello.deb"
	)
	debHash := strings.Repeat("a", 64)

	// First snapshot: covers the .deb. Will be displaced.
	r1 := seedBlob(t, c, "InRelease v1")
	id1, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: scheme, CanonicalHost: host,
		SuitePath: "/dists/noble", InReleaseHash: &r1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id1,
		[]SnapshotMember{{SnapshotID: id1, Path: "InRelease", BlobHash: r1, DeclaredSHA256: r1}},
		[]PackageHash{{
			CanonicalScheme: scheme, CanonicalHost: host, Path: debP,
			DeclaredSHA256: debHash, SnapshotID: id1,
		}},
		nil, false); err != nil {
		t.Fatal(err)
	}

	// Second snapshot: same suite, replaces id1 as current. Carries the
	// .deb forward with the same declared hash.
	r2 := seedBlob(t, c, "InRelease v2")
	id2, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: scheme, CanonicalHost: host,
		SuitePath: "/dists/noble", InReleaseHash: &r2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id2,
		[]SnapshotMember{{SnapshotID: id2, Path: "InRelease", BlobHash: r2, DeclaredSHA256: r2}},
		[]PackageHash{{
			CanonicalScheme: scheme, CanonicalHost: host, Path: debP,
			DeclaredSHA256: debHash, SnapshotID: id2,
		}},
		nil, false); err != nil {
		t.Fatal(err)
	}

	got, err := c.DeclaredHashesForPath(ctx, scheme, host, debP)
	if err != nil {
		t.Fatalf("DeclaredHashesForPath: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("len=%d, want 1 (only current snapshot's row): %v", len(got), got)
	}
	if len(got) > 0 && got[0].SnapshotID != id2 {
		t.Errorf("snapshot_id=%d, want %d (current)", got[0].SnapshotID, id2)
	}
}

// TestDeclaredHashesForPath_TwoSuitesDistinctHashes covers the §6.1
// conflict surface: same .deb path, two suites currently adopted, two
// different declared hashes. Both rows return.
func TestDeclaredHashesForPath_TwoSuitesDistinctHashes(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	const (
		scheme = "http"
		host   = "archive.example"
		debP   = "/pool/main/h/hello/hello.deb"
	)
	hashA := strings.Repeat("a", 64)
	hashB := strings.Repeat("b", 64)

	rA := seedBlob(t, c, "InRelease A")
	rB := seedBlob(t, c, "InRelease B")
	idA, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: scheme, CanonicalHost: host,
		SuitePath: "/dists/A", InReleaseHash: &rA,
	})
	if err != nil {
		t.Fatal(err)
	}
	idB, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: scheme, CanonicalHost: host,
		SuitePath: "/dists/B", InReleaseHash: &rB,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, idA,
		[]SnapshotMember{{SnapshotID: idA, Path: "InRelease", BlobHash: rA, DeclaredSHA256: rA}},
		[]PackageHash{{
			CanonicalScheme: scheme, CanonicalHost: host, Path: debP,
			DeclaredSHA256: hashA, SnapshotID: idA,
		}},
		nil, false); err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, idB,
		[]SnapshotMember{{SnapshotID: idB, Path: "InRelease", BlobHash: rB, DeclaredSHA256: rB}},
		[]PackageHash{{
			CanonicalScheme: scheme, CanonicalHost: host, Path: debP,
			DeclaredSHA256: hashB, SnapshotID: idB,
		}},
		nil, false); err != nil {
		t.Fatal(err)
	}

	got, err := c.DeclaredHashesForPath(ctx, scheme, host, debP)
	if err != nil {
		t.Fatalf("DeclaredHashesForPath: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len=%d, want 2: %v", len(got), got)
	}
	hashes := map[string]bool{}
	for _, d := range got {
		hashes[d.DeclaredSHA256] = true
	}
	if !hashes[hashA] || !hashes[hashB] {
		t.Errorf("got hashes %v, want both %s and %s", hashes, hashA, hashB)
	}
}

// TestLookupSnapshotMember_ReturnsBlobOfCurrentSnapshot is the §6.1
// metadata fast-path query: hides the suite_freshness join behind a
// single read.
func TestLookupSnapshotMember_ReturnsBlobOfCurrentSnapshot(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	scheme, host, suite := "http", "archive.example", "/dists/noble"
	r := seedBlob(t, c, "InRelease bytes")
	pkg := seedBlob(t, c, "Packages bytes")
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: scheme, CanonicalHost: host,
		SuitePath: suite, InReleaseHash: &r,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id, []SnapshotMember{
		{SnapshotID: id, Path: "InRelease", BlobHash: r, DeclaredSHA256: r},
		{SnapshotID: id, Path: "main/binary-amd64/Packages", BlobHash: pkg, DeclaredSHA256: pkg},
	}, nil, nil, false); err != nil {
		t.Fatal(err)
	}

	got, err := c.LookupSnapshotMember(ctx, scheme, host, suite, "main/binary-amd64/Packages")
	if err != nil {
		t.Fatalf("LookupSnapshotMember: %v", err)
	}
	if got.SnapshotID != id {
		t.Errorf("SnapshotID=%d, want %d", got.SnapshotID, id)
	}
	if got.BlobHash != pkg {
		t.Errorf("BlobHash=%s, want %s", got.BlobHash, pkg)
	}
}

// TestLookupSnapshotMember_NotFoundWhenSuiteHasNoSnapshot exercises the
// suite-row-exists-but-current_snapshot_id-IS-NULL case.
func TestLookupSnapshotMember_NotFoundWhenSuiteHasNoSnapshot(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	if err := c.PutSuiteFreshness(ctx, SuiteFreshness{
		CanonicalScheme: "http", CanonicalHost: "x.example",
		SuitePath: "/dists/noble",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := c.LookupSnapshotMember(ctx, "http", "x.example", "/dists/noble", "InRelease")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// TestLookupSnapshotMember_NotFoundWhenPathMissing covers the §6.1
// step-2-then-404 case: snapshot adopted but the path isn't a member.
func TestLookupSnapshotMember_NotFoundWhenPathMissing(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	scheme, host, suite := "http", "archive.example", "/dists/noble"
	r := seedBlob(t, c, "InRelease bytes")
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: scheme, CanonicalHost: host,
		SuitePath: suite, InReleaseHash: &r,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id,
		[]SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: r, DeclaredSHA256: r}},
		nil, nil, false); err != nil {
		t.Fatal(err)
	}

	_, err = c.LookupSnapshotMember(ctx, scheme, host, suite, "main/binary-amd64/Packages")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound (path not in snapshot)", err)
	}
}

// TestEvictURLPath_DeletesRowAndDecrementsRefcount verifies the §6.1
// step-5 bookkeeping. Pre-condition: a url_path row pointing at a blob
// whose refcount is positive (e.g. because the blob is also a snapshot
// member). After eviction: row gone + refcount decremented.
func TestEvictURLPath_DeletesRowAndDecrementsRefcount(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	const (
		scheme = "http"
		host   = "archive.example"
		debP   = "/pool/main/h/hello/hello.deb"
	)
	hash := seedBlob(t, c, "blob bytes")

	// Snapshot adoption bumps refcount to 1 via the snapshot_member.
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: scheme, CanonicalHost: host,
		SuitePath: "/dists/noble", InReleaseHash: &hash,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id, []SnapshotMember{
		{SnapshotID: id, Path: "InRelease", BlobHash: hash, DeclaredSHA256: hash},
	}, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	if got := blobRefcount(t, c, hash); got != 1 {
		t.Fatalf("pre-evict refcount=%d, want 1", got)
	}

	if err := c.PutURLPath(ctx, URLPath{
		CanonicalScheme: scheme, CanonicalHost: host, Path: debP,
		BlobHash: &hash, UpstreamURL: "http://up/" + debP, IsMetadata: false,
	}); err != nil {
		t.Fatal(err)
	}

	if err := c.EvictURLPath(ctx, scheme, host, debP); err != nil {
		t.Fatalf("EvictURLPath: %v", err)
	}

	if _, err := c.LookupURL(ctx, scheme, host, debP); !errors.Is(err, ErrNotFound) {
		t.Errorf("post-evict LookupURL = %v, want ErrNotFound", err)
	}
	if got := blobRefcount(t, c, hash); got != 0 {
		t.Errorf("post-evict refcount=%d, want 0", got)
	}
}

// TestEvictURLPath_IdempotentOnMissingRow covers the concurrent-eviction
// race: a second EvictURLPath after the row is gone is a clean no-op.
func TestEvictURLPath_IdempotentOnMissingRow(t *testing.T) {
	c := openCache(t)
	if err := c.EvictURLPath(context.Background(),
		"http", "x.example", "/no/such/path"); err != nil {
		t.Errorf("EvictURLPath on missing row: %v", err)
	}
}

// TestEvictURLPath_NoBlobHashSkipsDecrement covers a defensive
// edge case: a url_path row whose blob_hash is NULL (a freshness check
// that recorded validators but never finalized a body) evicts cleanly
// without attempting a phantom refcount decrement on a NULL hash.
func TestEvictURLPath_NoBlobHashSkipsDecrement(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	if err := c.PutURLPath(ctx, URLPath{
		CanonicalScheme: "http", CanonicalHost: "x.example", Path: "/p",
		BlobHash: nil, UpstreamURL: "http://x/p", IsMetadata: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.EvictURLPath(ctx, "http", "x.example", "/p"); err != nil {
		t.Errorf("EvictURLPath with NULL blob_hash: %v", err)
	}
	if _, err := c.LookupURL(ctx, "http", "x.example", "/p"); !errors.Is(err, ErrNotFound) {
		t.Errorf("post-evict LookupURL = %v, want ErrNotFound", err)
	}
}

// TestHostCurrentSnapshotsCoverage_ReturnsRowsPerCurrentSnapshot covers
// the SPEC3 §6.1 strict-mode lookup: per (scheme, host), one row per
// snapshot whose suite_freshness.current_snapshot_id matches, with the
// snapshot's package_coverage_complete bit. Rows whose snapshot is no
// longer current (displaced by adoption) are excluded.
func TestHostCurrentSnapshotsCoverage_ReturnsRowsPerCurrentSnapshot(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	scheme, host := "http", "archive.example"

	// Suite A: covered (coverage_complete = true). Suite B:
	// uncovered. Suite C: same host but never adopted (no row in
	// the result).
	rA := seedBlob(t, c, "InRelease A")
	rB := seedBlob(t, c, "InRelease B")
	rC := seedBlob(t, c, "InRelease C")
	idA, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: scheme, CanonicalHost: host,
		SuitePath: "/dists/A", InReleaseHash: &rA,
	})
	if err != nil {
		t.Fatal(err)
	}
	idB, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: scheme, CanonicalHost: host,
		SuitePath: "/dists/B", InReleaseHash: &rB,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Build C as a candidate but never CommitAdoption it.
	if _, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: scheme, CanonicalHost: host,
		SuitePath: "/dists/C", InReleaseHash: &rC,
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, idA,
		[]SnapshotMember{{SnapshotID: idA, Path: "InRelease", BlobHash: rA, DeclaredSHA256: rA}},
		nil, nil, true); err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, idB,
		[]SnapshotMember{{SnapshotID: idB, Path: "InRelease", BlobHash: rB, DeclaredSHA256: rB}},
		nil, nil, false); err != nil {
		t.Fatal(err)
	}

	got, err := c.HostCurrentSnapshotsCoverage(ctx, scheme, host)
	if err != nil {
		t.Fatalf("HostCurrentSnapshotsCoverage: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2 (A current + B current; C never adopted)", len(got))
	}
	gotMap := make(map[int64]bool, len(got))
	for _, sc := range got {
		gotMap[sc.SnapshotID] = sc.PackageCoverageComplete
	}
	if !gotMap[idA] {
		t.Errorf("A coverage = false, want true")
	}
	if gotMap[idB] {
		t.Errorf("B coverage = true, want false")
	}
}

// TestHostCurrentSnapshotsCoverage_EmptyHost: a host with zero adopted
// suites returns an empty slice + nil error. The strict-mode predicate
// reads this as "no contract on this host" and falls through to
// trust-upstream.
func TestHostCurrentSnapshotsCoverage_EmptyHost(t *testing.T) {
	c := openCache(t)
	got, err := c.HostCurrentSnapshotsCoverage(context.Background(), "http", "nonexistent.example")
	if err != nil {
		t.Fatalf("HostCurrentSnapshotsCoverage: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d rows, want 0", len(got))
	}
}

// TestComputeHotSet_TwoStageMatch covers the SPEC3 §7.5.3 happy path: a
// prior snapshot's hot package_hash row JOINs against a fresh
// url_path.last_requested_at, Stage 1 yields a (Package, Arch) pair,
// Stage 2 looks it up in the candidate snapshot and returns the new
// path + declared sha256.
func TestComputeHotSet_TwoStageMatch(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	scheme, host := "http", "archive.example"

	rOld := seedBlob(t, c, "InRelease v1")
	rNew := seedBlob(t, c, "InRelease v2")
	idPrior, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: scheme, CanonicalHost: host,
		SuitePath: "/dists/noble", InReleaseHash: &rOld,
	})
	if err != nil {
		t.Fatal(err)
	}
	idNew, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: scheme, CanonicalHost: host,
		SuitePath: "/dists/noble", InReleaseHash: &rNew,
	})
	if err != nil {
		t.Fatal(err)
	}

	debHashOld := seedBlob(t, c, "nginx 1.0 deb bytes")
	debHashNew := seedBlob(t, c, "nginx 2.0 deb bytes")
	pathOld := "/pool/main/n/nginx/nginx_1.0_amd64.deb"
	pathNew := "/pool/main/n/nginx/nginx_2.0_amd64.deb"

	// Adopt prior snapshot with package_hash row covering the old deb.
	if err := c.CommitAdoption(ctx, idPrior,
		[]SnapshotMember{{SnapshotID: idPrior, Path: "InRelease", BlobHash: rOld, DeclaredSHA256: rOld}},
		[]PackageHash{{
			CanonicalScheme: scheme, CanonicalHost: host, Path: pathOld,
			DeclaredSHA256: debHashOld, SnapshotID: idPrior,
			PackageName: "nginx", Architecture: "amd64",
		}},
		nil, true); err != nil {
		t.Fatalf("commit prior: %v", err)
	}
	// A url_path row at pathOld with a fresh last_requested_at: the
	// hotness signal Stage 1 keys on.
	now := nowUnix()
	if err := c.PutURLPath(ctx, URLPath{
		CanonicalScheme: scheme, CanonicalHost: host, Path: pathOld,
		BlobHash:        ptrStr(debHashOld),
		UpstreamURL:     "http://archive.example" + pathOld,
		LastRequestedAt: &now, RequestCount: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// Adopt the new snapshot. It carries the NEW path with the SAME
	// (Package, Arch). The hot-set query should resolve the old hot
	// pair to the new path. Build the candidate's PackageHash rows
	// once and reuse for both CommitAdoption and ComputeHotSet —
	// since SPEC3's runHotPrefetch precedes CommitAdoption, the
	// candidate's rows are passed in memory rather than queried.
	candPHs := []PackageHash{{
		CanonicalScheme: scheme, CanonicalHost: host, Path: pathNew,
		DeclaredSHA256: debHashNew, SnapshotID: idNew,
		PackageName: "nginx", Architecture: "amd64",
	}}
	if err := c.CommitAdoption(ctx, idNew,
		[]SnapshotMember{{SnapshotID: idNew, Path: "InRelease", BlobHash: rNew, DeclaredSHA256: rNew}},
		candPHs, nil, true); err != nil {
		t.Fatalf("commit new: %v", err)
	}

	got, err := c.ComputeHotSet(ctx, scheme, host, idPrior, idNew, candPHs, 86400, now)
	if err != nil {
		t.Fatalf("ComputeHotSet: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1: %+v", len(got), got)
	}
	if got[0].Path != pathNew {
		t.Errorf("Path = %q, want %q", got[0].Path, pathNew)
	}
	if got[0].DeclaredSHA256 != debHashNew {
		t.Errorf("DeclaredSHA256 = %q, want %q", got[0].DeclaredSHA256, debHashNew)
	}
}

// TestComputeHotSet_ExcludesPreV3Rows: package_hash rows with empty
// package_name/architecture (post-migration, pre-Phase-3 adoptions)
// are filtered out by Stage 1's <> '' predicate. SPEC3 §4.3.2.
func TestComputeHotSet_ExcludesPreV3Rows(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	scheme, host := "http", "archive.example"

	rOld := seedBlob(t, c, "InRelease pre-v3")
	rNew := seedBlob(t, c, "InRelease v3")
	idPrior, _, _ := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: scheme, CanonicalHost: host,
		SuitePath: "/dists/noble", InReleaseHash: &rOld,
	})
	idNew, _, _ := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: scheme, CanonicalHost: host,
		SuitePath: "/dists/noble", InReleaseHash: &rNew,
	})
	debHash := seedBlob(t, c, "qemu deb bytes")
	pathOld := "/pool/main/q/qemu/qemu_1.0_amd64.deb"

	// Prior commits a package_hash row with empty package_name/arch
	// (the pre-v3 default).
	if err := c.CommitAdoption(ctx, idPrior,
		[]SnapshotMember{{SnapshotID: idPrior, Path: "InRelease", BlobHash: rOld, DeclaredSHA256: rOld}},
		[]PackageHash{{
			CanonicalScheme: scheme, CanonicalHost: host, Path: pathOld,
			DeclaredSHA256: debHash, SnapshotID: idPrior,
			// PackageName: "" Architecture: "" — pre-v3 defaults
		}},
		nil, false); err != nil {
		t.Fatal(err)
	}
	now := nowUnix()
	_ = c.PutURLPath(ctx, URLPath{
		CanonicalScheme: scheme, CanonicalHost: host, Path: pathOld,
		BlobHash: ptrStr(debHash), UpstreamURL: "u",
		LastRequestedAt: &now, RequestCount: 1,
	})
	_ = c.CommitAdoption(ctx, idNew,
		[]SnapshotMember{{SnapshotID: idNew, Path: "InRelease", BlobHash: rNew, DeclaredSHA256: rNew}},
		nil, nil, false)

	// Candidate is empty (idNew committed with nil package hashes —
	// upstream removed the pre-v3 entry too). Even if the candidate
	// somehow had a v3 row for the same path, Stage 1's <> ''
	// predicate filters out the prior's pre-v3 rows so no Stage 2
	// lookup ever runs.
	got, err := c.ComputeHotSet(ctx, scheme, host, idPrior, idNew, nil, 86400, now)
	if err != nil {
		t.Fatalf("ComputeHotSet: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d entries; want 0 (pre-v3 rows must be excluded)", len(got))
	}
}

// TestComputeHotSet_DroppedPackageNotInCandidate: a hot pair whose
// (Package, Arch) is no longer in the candidate snapshot does not
// graduate to the prefetch list — there is no new path to fetch.
func TestComputeHotSet_DroppedPackageNotInCandidate(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	scheme, host := "http", "archive.example"

	rOld := seedBlob(t, c, "InRelease v1 dropping")
	rNew := seedBlob(t, c, "InRelease v2 dropped")
	idPrior, _, _ := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: scheme, CanonicalHost: host,
		SuitePath: "/dists/noble", InReleaseHash: &rOld,
	})
	idNew, _, _ := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: scheme, CanonicalHost: host,
		SuitePath: "/dists/noble", InReleaseHash: &rNew,
	})
	debHash := seedBlob(t, c, "gone deb bytes")
	pathOld := "/pool/main/g/gone/gone_1.0_amd64.deb"
	if err := c.CommitAdoption(ctx, idPrior,
		[]SnapshotMember{{SnapshotID: idPrior, Path: "InRelease", BlobHash: rOld, DeclaredSHA256: rOld}},
		[]PackageHash{{
			CanonicalScheme: scheme, CanonicalHost: host, Path: pathOld,
			DeclaredSHA256: debHash, SnapshotID: idPrior,
			PackageName: "gone", Architecture: "amd64",
		}},
		nil, true); err != nil {
		t.Fatal(err)
	}
	now := nowUnix()
	_ = c.PutURLPath(ctx, URLPath{
		CanonicalScheme: scheme, CanonicalHost: host, Path: pathOld,
		BlobHash: ptrStr(debHash), UpstreamURL: "u",
		LastRequestedAt: &now, RequestCount: 1,
	})
	// New snapshot has no package_hash for "gone" — upstream removed it.
	if err := c.CommitAdoption(ctx, idNew,
		[]SnapshotMember{{SnapshotID: idNew, Path: "InRelease", BlobHash: rNew, DeclaredSHA256: rNew}},
		nil, nil, true); err != nil {
		t.Fatal(err)
	}

	// Candidate's in-memory PackageHash slice is empty (upstream
	// dropped the package). Stage 2 lookup misses → drop from hot set.
	got, err := c.ComputeHotSet(ctx, scheme, host, idPrior, idNew, nil, 86400, now)
	if err != nil {
		t.Fatalf("ComputeHotSet: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d entries; want 0 (package was removed in candidate)", len(got))
	}
}

// TestComputeHotSet_WindowZeroReturnsEmpty: hot_packages.window = 0
// disables prefetch entirely (SPEC3 §5.1).
func TestComputeHotSet_WindowZeroReturnsEmpty(t *testing.T) {
	c := openCache(t)
	got, err := c.ComputeHotSet(context.Background(), "http", "x", 1, 2, nil, 0, 100)
	if err != nil {
		t.Fatalf("ComputeHotSet: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d entries with window=0, want 0", len(got))
	}
}

// TestComputeHotSet_PriorIDZeroReturnsEmpty: a fresh suite with no
// prior current_snapshot_id has nothing to mine. SPEC3 §7.5 step 9
// "no prior current_snapshot_id for this suite" → empty hot set.
func TestComputeHotSet_PriorIDZeroReturnsEmpty(t *testing.T) {
	c := openCache(t)
	got, err := c.ComputeHotSet(context.Background(), "http", "x", 0, 1, nil, 86400, 100)
	if err != nil {
		t.Fatalf("ComputeHotSet: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d entries with priorID=0, want 0", len(got))
	}
}

// TestComputeHotSet_RejectsCandidateMismatch covers the contract
// validation Stage 2 enforces in-memory: the in-flight Stage 2 SQL
// scoped its lookup by (scheme, host, snapshot_id), and the in-memory
// form must reject candidate rows whose metadata disagrees so a
// caller bug doesn't silently warm the wrong snapshot's rows. SPEC3
// §7.5.3.
func TestComputeHotSet_RejectsCandidateMismatch(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	scheme, host := "http", "archive.example"

	rOld := seedBlob(t, c, "InRelease v1 mismatch")
	rNew := seedBlob(t, c, "InRelease v2 mismatch")
	idPrior, _, _ := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: scheme, CanonicalHost: host,
		SuitePath: "/dists/noble", InReleaseHash: &rOld,
	})
	idNew, _, _ := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: scheme, CanonicalHost: host,
		SuitePath: "/dists/noble", InReleaseHash: &rNew,
	})
	debHash := seedBlob(t, c, "deb bytes mismatch")
	pathOld := "/pool/main/m/match/match_1.0_amd64.deb"
	if err := c.CommitAdoption(ctx, idPrior,
		[]SnapshotMember{{SnapshotID: idPrior, Path: "InRelease", BlobHash: rOld, DeclaredSHA256: rOld}},
		[]PackageHash{{
			CanonicalScheme: scheme, CanonicalHost: host, Path: pathOld,
			DeclaredSHA256: debHash, SnapshotID: idPrior,
			PackageName: "match", Architecture: "amd64",
		}},
		nil, true); err != nil {
		t.Fatal(err)
	}
	now := nowUnix()
	_ = c.PutURLPath(ctx, URLPath{
		CanonicalScheme: scheme, CanonicalHost: host, Path: pathOld,
		BlobHash: ptrStr(debHash), UpstreamURL: "u",
		LastRequestedAt: &now, RequestCount: 1,
	})

	mismatched := []PackageHash{{
		CanonicalScheme: scheme, CanonicalHost: host,
		// SnapshotID intentionally != idNew — caller error.
		SnapshotID:   idPrior,
		Path:         pathOld,
		DeclaredSHA256: debHash,
		PackageName:    "match",
		Architecture:   "amd64",
	}}
	_, err := c.ComputeHotSet(ctx, scheme, host, idPrior, idNew, mismatched, 86400, now)
	if !errors.Is(err, ErrHotSetCandidateMismatch) {
		t.Errorf("err = %v, want ErrHotSetCandidateMismatch", err)
	}
}

// TestComputeHotSet_RejectsCandidateDuplicate covers the second
// in-memory contract: duplicate (Package, Architecture) keys in the
// candidate slice are a programming error (buildPackageHashes
// rejects them in production). The cache layer fails closed rather
// than silently picking one row.
func TestComputeHotSet_RejectsCandidateDuplicate(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	scheme, host := "http", "archive.example"

	rOld := seedBlob(t, c, "InRelease v1 dup")
	idPrior, _, _ := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: scheme, CanonicalHost: host,
		SuitePath: "/dists/noble", InReleaseHash: &rOld,
	})
	debHash := seedBlob(t, c, "dup deb bytes")
	pathOld := "/pool/main/d/dup/dup_1.0_amd64.deb"
	if err := c.CommitAdoption(ctx, idPrior,
		[]SnapshotMember{{SnapshotID: idPrior, Path: "InRelease", BlobHash: rOld, DeclaredSHA256: rOld}},
		[]PackageHash{{
			CanonicalScheme: scheme, CanonicalHost: host, Path: pathOld,
			DeclaredSHA256: debHash, SnapshotID: idPrior,
			PackageName: "dup", Architecture: "amd64",
		}},
		nil, true); err != nil {
		t.Fatal(err)
	}
	now := nowUnix()
	_ = c.PutURLPath(ctx, URLPath{
		CanonicalScheme: scheme, CanonicalHost: host, Path: pathOld,
		BlobHash: ptrStr(debHash), UpstreamURL: "u",
		LastRequestedAt: &now, RequestCount: 1,
	})

	const idCandidate = int64(999)
	dup := []PackageHash{
		{
			CanonicalScheme: scheme, CanonicalHost: host,
			SnapshotID: idCandidate,
			Path:       "/pool/main/d/dup/dup_2.0_amd64.deb",
			DeclaredSHA256: debHash,
			PackageName: "dup", Architecture: "amd64",
		},
		{
			CanonicalScheme: scheme, CanonicalHost: host,
			SnapshotID: idCandidate,
			Path:       "/pool/main/d/dup/dup_3.0_amd64.deb",
			DeclaredSHA256: debHash,
			PackageName: "dup", Architecture: "amd64",
		},
	}
	_, err := c.ComputeHotSet(ctx, scheme, host, idPrior, idCandidate, dup, 86400, now)
	if !errors.Is(err, ErrHotSetCandidateDuplicate) {
		t.Errorf("err = %v, want ErrHotSetCandidateDuplicate", err)
	}
}

// TestCommitAdoption_PrefetchedURLPath_PreservesHotness: the SPEC3
// §7.5.1 "deliberately diverges from PutURLPath" semantics —
// last_requested_at + request_count must survive the upsert when the
// hot-prefetch loop warms a path that already had a prior url_path
// row (e.g. the unversioned alias was stable across the version bump).
func TestCommitAdoption_PrefetchedURLPath_PreservesHotness(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	scheme, host := "http", "archive.example"
	const debP = "/pool/main/h/hot/hot.deb"

	// Pre-existing url_path row with a hotness signal.
	priorBlob := seedBlob(t, c, "old hot deb")
	hotTime := int64(1234567890)
	if err := c.PutURLPath(ctx, URLPath{
		CanonicalScheme: scheme, CanonicalHost: host, Path: debP,
		BlobHash: ptrStr(priorBlob), UpstreamURL: "http://archive.example" + debP,
		LastRequestedAt: &hotTime, RequestCount: 42,
	}); err != nil {
		t.Fatal(err)
	}

	r := seedBlob(t, c, "InRelease for hot test")
	id, _, _ := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: scheme, CanonicalHost: host,
		SuitePath: "/dists/noble", InReleaseHash: &r,
	})

	// Hot prefetch warmed a NEW blob for the same path.
	newBlob := seedBlob(t, c, "new hot deb v2")
	if err := c.CommitAdoption(ctx, id,
		[]SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: r, DeclaredSHA256: r}},
		nil,
		[]PrefetchedURLPath{{
			CanonicalScheme: scheme, CanonicalHost: host, Path: debP,
			BlobHash: newBlob, UpstreamURL: "http://archive.example" + debP,
		}},
		false); err != nil {
		t.Fatalf("CommitAdoption: %v", err)
	}

	row, err := c.LookupURL(ctx, scheme, host, debP)
	if err != nil {
		t.Fatalf("LookupURL: %v", err)
	}
	// blob_hash flipped to new.
	if row.BlobHash == nil || *row.BlobHash != newBlob {
		t.Errorf("blob_hash = %v, want %s (new)", row.BlobHash, newBlob)
	}
	// last_requested_at + request_count preserved.
	if row.LastRequestedAt == nil || *row.LastRequestedAt != hotTime {
		t.Errorf("last_requested_at = %v, want %d (hotness must survive upsert per SPEC3 §7.5.1)", row.LastRequestedAt, hotTime)
	}
	if row.RequestCount != 42 {
		t.Errorf("request_count = %d, want 42 (hotness must survive upsert)", row.RequestCount)
	}
}

// TestCommitAdoption_PrefetchedURLPath_FreshInsert: a path with no prior
// url_path row gets inserted with last_requested_at = NULL and
// request_count = 0 — this is a brand-new path; there is no hotness
// signal to preserve.
func TestCommitAdoption_PrefetchedURLPath_FreshInsert(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	scheme, host := "http", "archive.example"
	const debP = "/pool/main/n/new/new_1.0_amd64.deb"

	r := seedBlob(t, c, "InRelease fresh")
	id, _, _ := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: scheme, CanonicalHost: host,
		SuitePath: "/dists/noble", InReleaseHash: &r,
	})
	newBlob := seedBlob(t, c, "fresh deb")
	if err := c.CommitAdoption(ctx, id,
		[]SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: r, DeclaredSHA256: r}},
		nil,
		[]PrefetchedURLPath{{
			CanonicalScheme: scheme, CanonicalHost: host, Path: debP,
			BlobHash: newBlob, UpstreamURL: "http://archive.example" + debP,
		}},
		false); err != nil {
		t.Fatalf("CommitAdoption: %v", err)
	}

	row, err := c.LookupURL(ctx, scheme, host, debP)
	if err != nil {
		t.Fatalf("LookupURL: %v", err)
	}
	if row.BlobHash == nil || *row.BlobHash != newBlob {
		t.Errorf("blob_hash = %v, want %s", row.BlobHash, newBlob)
	}
	if row.LastRequestedAt != nil {
		t.Errorf("last_requested_at = %v, want nil (fresh insert)", row.LastRequestedAt)
	}
	if row.RequestCount != 0 {
		t.Errorf("request_count = %d, want 0 (fresh insert)", row.RequestCount)
	}
}

// TestCommitAdoption_StampsCoverage: SPEC3 §7.5.4 — the
// coverageComplete arg lands on the suite_snapshot row atomically
// with the flip, readable post-commit via GetSuiteSnapshot.
func TestCommitAdoption_StampsCoverage(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	scheme, host := "http", "archive.example"
	r := seedBlob(t, c, "InRelease coverage test")
	id, _, _ := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: scheme, CanonicalHost: host,
		SuitePath: "/dists/noble", InReleaseHash: &r,
	})
	if err := c.CommitAdoption(ctx, id,
		[]SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: r, DeclaredSHA256: r}},
		nil, nil, true); err != nil {
		t.Fatal(err)
	}
	snap, err := c.GetSuiteSnapshot(ctx, id)
	if err != nil {
		t.Fatalf("GetSuiteSnapshot: %v", err)
	}
	if !snap.PackageCoverageComplete {
		t.Errorf("package_coverage_complete = false, want true (CommitAdoption did not stamp coverageComplete=true)")
	}
}

// ptrStr is a one-line *string helper for the test fixtures above.
func ptrStr(s string) *string { return &s }
