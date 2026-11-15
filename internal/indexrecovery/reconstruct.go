// Package indexrecovery reconstructs repository indexes without changing the
// bytes promised by their Release. HTTP headers are never a trust source.
package indexrecovery

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"errors"
	"fmt"
	"io"
	"os"
	"path"

	"github.com/ulikunitz/xz"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// MaxBytes bounds both representations. It matches the existing
// Packages/Sources parsing limit; the declared size is a tighter cap.
const MaxBytes int64 = 256 << 20

var (
	ErrInvalid = errors.New("index reconstruction: invalid representation")
	ErrStorage = errors.New("index reconstruction: storage failure")
)

// Member is a declaration from one Release, or a previously verified snapshot
// member's content hash and blob size. Both inputs MUST belong to the same
// snapshot. At least one strong hash is required for each representation.
type Member struct {
	Path   string
	SHA256 string
	SHA512 string
	Size   int64
}

// PlainIndex restricts reconstruction to full, uncompressed index files.
// Neither packages, release anchors, pdiffs nor compressed indexes qualify.
func PlainIndex(p string) bool {
	return path.Base(p) == "Packages" || path.Base(p) == "Sources"
}

func (m Member) validSize() bool { return m.Size >= 0 && m.Size <= MaxBytes }

func (m Member) check(n int64, sha256, sha512 string) error {
	if n != m.Size || (m.SHA256 == "" && m.SHA512 == "") ||
		(m.SHA256 != "" && sha256 != m.SHA256) || (m.SHA512 != "" && sha512 != m.SHA512) {
		return fmt.Errorf("%w: size or hash mismatch for %s", ErrInvalid, m.Path)
	}
	return nil
}

// Reconstruct reads a cached compressed sibling and writes the exact plain
// member to the pool. It verifies BOTH representations, including zero sizes,
// every supplied digest, codec trailers and trailing data, before publishing
// any output. Hashing the compressed bytes as they are decoded also detects
// corruption since the caller fetched or verified that blob. No URL is inferred
// from Content-Location, Content-Type, or other untrusted response headers.
func Reconstruct(ctx context.Context, c *cache.Cache, plain, compressed Member, blobHash string) (string, error) {
	if !PlainIndex(plain.Path) || !plain.validSize() || !compressed.validSize() ||
		(compressed.Path != plain.Path+".gz" && compressed.Path != plain.Path+".xz") {
		return "", fmt.Errorf("%w: unsupported sibling or size", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f, err := os.Open(c.BlobPath(blobHash))
	if err != nil {
		return "", fmt.Errorf("%w: open compressed blob: %w", ErrStorage, err)
	}
	defer func() { _ = f.Close() }()
	// Revalidate cached input before invoking a codec. A pinned filename alone
	// does not prove that a blob has survived intact at rest. Hash it again
	// during decoding as well, so a concurrent replacement cannot supply
	// unchecked compressed bytes between verification and use.
	in256, in512, out512 := sha256.New(), sha512.New(), sha512.New()
	n, err := io.Copy(io.MultiWriter(in256, in512), io.LimitReader(contextReader{ctx, f}, compressed.Size+1))
	if err != nil {
		return "", fmt.Errorf("%w: read compressed blob: %w", ErrStorage, err)
	}
	if err := compressed.check(n, fmt.Sprintf("%x", in256.Sum(nil)), fmt.Sprintf("%x", in512.Sum(nil))); err != nil {
		return "", err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("%w: rewind compressed blob: %w", ErrStorage, err)
	}
	in256.Reset()
	in512.Reset()
	w, err := c.NewTempBlob()
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrStorage, err)
	}
	defer func() { _ = w.Abort() }()
	limited := &io.LimitedReader{R: contextReader{ctx, f}, N: compressed.Size + 1}
	encoded := io.TeeReader(limited, io.MultiWriter(in256, in512))
	dst := &recordWriter{Writer: io.MultiWriter(w, out512), ctx: ctx}
	if path.Ext(compressed.Path) == ".gz" {
		err = CopyGzip(dst, encoded, plain.Size)
	} else {
		var r *xz.Reader
		r, err = xz.NewReader(encoded)
		if err == nil {
			_, err = io.Copy(dst, io.LimitReader(r, plain.Size+1))
		}
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if dst.err != nil {
		return "", fmt.Errorf("%w: write plain blob: %w", ErrStorage, dst.err)
	}
	if err != nil {
		return "", fmt.Errorf("%w: decompress %s: %w", ErrInvalid, compressed.Path, err)
	}
	if err := compressed.check(compressed.Size+1-limited.N, fmt.Sprintf("%x", in256.Sum(nil)), fmt.Sprintf("%x", in512.Sum(nil))); err != nil {
		return "", err
	}
	hash := w.HashSum()
	if err := plain.check(w.Written(), hash, fmt.Sprintf("%x", out512.Sum(nil))); err != nil {
		return "", err
	}
	// Finalize deduplicates without reading an existing pool file. Never keep a
	// corrupt file in place of the newly reconstructed, fully verified bytes.
	if err := checkExisting(ctx, c, hash); err != nil {
		return "", err
	}
	if _, err := w.FinalizeExpectingHash(hash, plain.Size); err != nil {
		return "", fmt.Errorf("%w: finalize: %w", ErrStorage, err)
	}
	if err := c.PutBlob(ctx, hash, plain.Size); err != nil {
		return "", fmt.Errorf("%w: %w", ErrStorage, err)
	}
	return hash, nil
}

func checkExisting(ctx context.Context, c *cache.Cache, hash string) error {
	f, err := os.Open(c.BlobPath(hash))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrStorage, err)
	}
	h := sha256.New()
	_, err = io.Copy(h, io.LimitReader(contextReader{ctx, f}, MaxBytes+1))
	_ = f.Close()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrStorage, err)
	}
	if fmt.Sprintf("%x", h.Sum(nil)) != hash {
		if err := os.Remove(c.BlobPath(hash)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: evict corrupt plain blob: %w", ErrStorage, err)
		}
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

type recordWriter struct {
	io.Writer
	ctx context.Context
	err error
}

func (w *recordWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := w.Writer.Write(p)
	if err != nil {
		w.err = err
	}
	return n, err
}
