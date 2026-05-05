package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	c, err := Open(context.Background(), dir)
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
	c, err := Open(context.Background(), dir)
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
	_, err = Open(context.Background(), dir)
	if err == nil {
		t.Fatal("Open(future schema): expected error, got nil")
	}
	if !strings.Contains(err.Error(), "newer than this binary supports") {
		t.Errorf("error %q does not name the version mismatch", err)
	}
}

func TestOpen_ReopenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	c1, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("Open #1: %v", err)
	}
	if err := c1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	c2, err := Open(context.Background(), dir)
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
	c, err := Open(context.Background(), dir)
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
		"",                   // empty
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
	c, err := Open(context.Background(), weird)
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
