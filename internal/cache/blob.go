package cache

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"time"
)

// ErrSizeMismatch is returned by BlobWriter.Finalize when the written
// byte-count differs from the upstream's declared Content-Length.
var ErrSizeMismatch = errors.New("cache: blob size mismatch")

// ErrInvalidHash is returned when a caller hands the cache something that
// is not a valid sha256 hex digest.
var ErrInvalidHash = errors.New("cache: invalid blob hash")

// validBlobHash reports whether s is exactly 64 lowercase hex characters
// (a sha256 hex digest). This is the canonical form the schema CHECK
// constraint enforces; callers must pass it through here before any path
// computation or SQL interpolation, as a defense-in-depth against path
// traversal if a malformed value ever reaches the API.
func validBlobHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// BlobPath returns the absolute on-disk path the cache will store the blob
// at when finalized. Panics on a malformed hash; that should never reach
// this function because every entry point validates first.
func (c *Cache) BlobPath(hash string) string {
	if !validBlobHash(hash) {
		// AIDEV-NOTE: panicking here turns "bad hash leaked into BlobPath"
		// from a quiet path-traversal bug into a visible test/CI failure.
		// Callers must validate at their boundary; nothing inside the
		// cache package should produce a non-hex hash.
		panic(fmt.Errorf("%w: %q", ErrInvalidHash, hash))
	}
	return filepath.Join(c.dir, "pool", hash[:2], hash)
}

// BlobExists reports whether a finalized blob with this hash is on disk.
// Returns ErrInvalidHash if the hash is not 64 hex chars, so callers can
// distinguish "not in the cache" from "you passed garbage".
func (c *Cache) BlobExists(hash string) (bool, error) {
	if !validBlobHash(hash) {
		return false, fmt.Errorf("%w: %q", ErrInvalidHash, hash)
	}
	st, err := os.Stat(c.BlobPath(hash))
	switch {
	case err == nil:
		return st.Mode().IsRegular(), nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// NewTempBlob creates a fresh, empty file under tmp/ ready to receive a
// download. Caller writes into the returned BlobWriter and then calls
// Finalize (success) or Abort (failure or shutdown). The temp filename
// uses cryptographically random bytes so concurrent downloads cannot
// collide across goroutines or restarts.
func (c *Cache) NewTempBlob() (*BlobWriter, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return nil, fmt.Errorf("cache: tmp id: %w", err)
	}
	name := hex.EncodeToString(buf[:])
	tmpPath := filepath.Join(c.dir, "tmp", name)
	f, err := os.OpenFile(tmpPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return nil, fmt.Errorf("cache: open tmp blob: %w", err)
	}
	return &BlobWriter{
		cache:    c,
		tmpPath:  tmpPath,
		file:     f,
		hasher:   sha256.New(),
		finished: false,
	}, nil
}

// BlobWriter accumulates bytes for a download, hashing as it goes. Closing
// (via Finalize or Abort) is mandatory; leaking a BlobWriter leaves a
// partial file under tmp/ that the next SweepTmp will reclaim.
type BlobWriter struct {
	cache    *Cache
	tmpPath  string
	file     *os.File
	hasher   hash.Hash
	written  int64
	finished bool
}

// Write writes p to the temp file and feeds the hasher. Concurrent calls
// from multiple goroutines on the same BlobWriter are not supported.
func (w *BlobWriter) Write(p []byte) (int, error) {
	if w.finished {
		return 0, errors.New("cache: BlobWriter already closed")
	}
	n, err := w.file.Write(p)
	if n > 0 {
		w.hasher.Write(p[:n])
		w.written += int64(n)
	}
	return n, err
}

// Written returns the cumulative bytes written so far. Useful for
// resumable Range fetches that need to report progress.
func (w *BlobWriter) Written() int64 { return w.written }

// Truncate resets the BlobWriter to its zero state: temp file emptied,
// hasher reset, written counter back to 0. The fetch layer calls this
// when a resume retry's If-Range validator no longer matches and the
// partial bytes must be discarded before restarting from byte 0
// (SPEC §6.3). Truncating after Finalize/Abort is an error.
func (w *BlobWriter) Truncate() error {
	if w.finished {
		return errors.New("cache: BlobWriter already closed")
	}
	if err := w.file.Truncate(0); err != nil {
		return fmt.Errorf("cache: truncate tmp blob: %w", err)
	}
	if _, err := w.file.Seek(0, 0); err != nil {
		return fmt.Errorf("cache: seek tmp blob: %w", err)
	}
	w.hasher = sha256.New()
	w.written = 0
	return nil
}

// Finalize fsyncs the temp file, verifies the byte count against
// expectedSize (when nonzero), and atomically moves the file into pool/
// under its content-addressed path. If a blob with the same hash is
// already present, the temp is removed and the existing path is returned.
func (w *BlobWriter) Finalize(expectedSize int64) (string, error) {
	if w.finished {
		return "", errors.New("cache: BlobWriter already closed")
	}
	w.finished = true

	if expectedSize > 0 && w.written != expectedSize {
		_ = w.file.Close()
		_ = os.Remove(w.tmpPath)
		return "", fmt.Errorf("%w: wrote %d bytes, expected %d",
			ErrSizeMismatch, w.written, expectedSize)
	}

	if err := w.file.Sync(); err != nil {
		_ = w.file.Close()
		_ = os.Remove(w.tmpPath)
		return "", fmt.Errorf("cache: fsync tmp blob: %w", err)
	}
	if err := w.file.Close(); err != nil {
		_ = os.Remove(w.tmpPath)
		return "", fmt.Errorf("cache: close tmp blob: %w", err)
	}

	hashHex := hex.EncodeToString(w.hasher.Sum(nil))
	dstPath := w.cache.BlobPath(hashHex)
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o750); err != nil {
		_ = os.Remove(w.tmpPath)
		return "", fmt.Errorf("cache: mkdir bucket: %w", err)
	}

	// AIDEV-NOTE: another fetch may have raced us to the same content.
	// Rename will overwrite the destination atomically on Linux, which is
	// fine — both files have the same content by definition. Still, prefer
	// to leave the existing file alone so concurrent readers' open file
	// descriptors stay valid.
	if _, err := os.Stat(dstPath); err == nil {
		_ = os.Remove(w.tmpPath)
		// SPEC §10: blob write event still emitted on dedup path so log
		// consumers see one line per successful Finalize regardless of
		// whether bytes actually landed in pool/.
		w.cache.logger.Debug("blob written", "hash", hashHex, "size_bytes", w.written, "deduped", true)
		return hashHex, nil
	}
	if err := os.Rename(w.tmpPath, dstPath); err != nil {
		_ = os.Remove(w.tmpPath)
		return "", fmt.Errorf("cache: rename into pool: %w", err)
	}
	w.cache.logger.Debug("blob written", "hash", hashHex, "size_bytes", w.written, "deduped", false)
	return hashHex, nil
}

// Abort discards the in-progress blob without finalizing. Idempotent.
func (w *BlobWriter) Abort() error {
	if w.finished {
		return nil
	}
	w.finished = true
	cerr := w.file.Close()
	rerr := os.Remove(w.tmpPath)
	if cerr != nil && !errors.Is(cerr, os.ErrClosed) {
		return cerr
	}
	if rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		return rerr
	}
	return nil
}

// DiscardFinalizedBlob removes the on-disk pool/<hash> file without
// touching the blob table — a no-op if the file isn't there. Used by
// SPEC2 §6.2 .deb miss-path validation: the bytes have been Finalize'd
// (so they live in pool/) but the hash doesn't match the snapshot's
// declared value, so we drop them before any DB row references them.
//
// Idempotent and safe under concurrency: a parallel fetch that just
// re-finalized the same hash will simply re-create the file. Safe to
// call without holding any singleflight lock.
//
// AIDEV-NOTE: validBlobHash is enforced before the os.Remove so a
// caller mistake cannot turn this into a path-traversal primitive.
func (c *Cache) DiscardFinalizedBlob(hash string) error {
	if !validBlobHash(hash) {
		return fmt.Errorf("%w: %q", ErrInvalidHash, hash)
	}
	if err := os.Remove(c.BlobPath(hash)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cache: remove blob: %w", err)
	}
	return nil
}

// SweepTmp deletes any file under tmp/ whose mtime is older than maxAge.
// Run at startup to reap orphans from a previous crash (SPEC §4.2).
func (c *Cache) SweepTmp(maxAge time.Duration) error {
	tmpDir := filepath.Join(c.dir, "tmp")
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return fmt.Errorf("cache: read tmp/: %w", err)
	}
	cutoff := time.Now().Add(-maxAge)
	var firstErr error
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if info.IsDir() {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(tmpDir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// hashReader is a small helper for callers who want to compute the sha256
// of an io.Reader without buffering the whole thing.
//
// AIDEV-NOTE: BlobWriter already does inline hashing during Write, so
// this is only used in tests and out-of-band integrity checks.
func hashReader(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
