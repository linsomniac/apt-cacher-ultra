package indexrecovery

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/ulikunitz/xz"
)

func compressedFixture(t *testing.T, codec string, body []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	var w io.WriteCloser
	if codec == ".gz" {
		w = gzip.NewWriter(&b)
	} else {
		var err error
		w, err = xz.NewWriter(&b)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func declaration(p string, b []byte) Member {
	return Member{Path: p, Size: int64(len(b)), SHA256: fmt.Sprintf("%x", sha256.Sum256(b)), SHA512: fmt.Sprintf("%x", sha512.Sum512(b))}
}

func TestReconstruct(t *testing.T) {
	for _, codec := range []string{".gz", ".xz"} {
		for _, mode := range []string{"valid", "empty", "sha512_only", "sha256_only", "concatenated", "padding", "wrong_plain_sha256", "wrong_plain_sha512", "wrong_compressed_sha256", "wrong_compressed_sha512", "plain_size_short", "plain_size_long", "compressed_size_short", "compressed_size_long", "zero_size_lie", "no_plain_hash", "no_compressed_hash", "other_path", "other_release", "truncated", "trailing_garbage", "corrupt_trailer", "over_cap", "canceled", "corrupt_existing_plain", "corrupt_cached_compressed"} {
			t.Run(codec+"/"+mode, func(t *testing.T) {
				ctx := context.Background()
				c, err := cache.Open(ctx, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = c.Close() }()
				plain := bytes.Repeat([]byte("Package: hello\n"), 200)
				if mode == "empty" {
					plain = nil
				}
				compressed := compressedFixture(t, codec, plain)
				if mode == "concatenated" {
					compressed = append(compressedFixture(t, codec, plain[:len(plain)/2]), compressedFixture(t, codec, plain[len(plain)/2:])...)
				}
				if mode == "padding" {
					compressed = append(compressed, make([]byte, 8192)...)
				}
				if mode == "other_release" {
					compressed = compressedFixture(t, codec, bytes.Repeat([]byte("Package: wrong\n"), 200))
				}
				if mode == "truncated" {
					compressed = compressed[:len(compressed)-1]
				}
				if mode == "trailing_garbage" {
					compressed = append(compressed, []byte("junk")...)
				}
				if mode == "corrupt_trailer" {
					compressed[len(compressed)-5] ^= 1
				}
				p, s := declaration("main/source/Sources", plain), declaration("main/source/Sources"+codec, compressed)
				w, err := c.NewTempBlob()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := w.Write(compressed); err != nil {
					t.Fatal(err)
				}
				hash, err := w.Finalize(int64(len(compressed)))
				if err != nil {
					t.Fatal(err)
				}
				success := false
				switch mode {
				case "valid", "empty", "concatenated", "padding":
					success = true
				case "sha512_only":
					p.SHA256 = ""
					s.SHA256 = ""
					success = true
				case "sha256_only":
					p.SHA512 = ""
					s.SHA512 = ""
					success = true
				case "wrong_plain_sha256":
					p.SHA256 = strings.Repeat("a", 64)
				case "wrong_plain_sha512":
					p.SHA512 = strings.Repeat("a", 128)
				case "wrong_compressed_sha256":
					s.SHA256 = strings.Repeat("a", 64)
				case "wrong_compressed_sha512":
					s.SHA512 = strings.Repeat("a", 128)
				case "plain_size_short":
					p.Size--
				case "plain_size_long":
					p.Size++
				case "compressed_size_short":
					s.Size--
				case "compressed_size_long":
					s.Size++
				case "zero_size_lie":
					p.Size = 0
				case "no_plain_hash":
					p.SHA256 = ""
					p.SHA512 = ""
				case "no_compressed_hash":
					s.SHA256 = ""
					s.SHA512 = ""
				case "other_path":
					s.Path = "other/source/Sources" + codec
				case "over_cap":
					p.Size = MaxBytes + 1
				case "canceled":
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				case "corrupt_existing_plain":
					if err := os.MkdirAll(filepath.Dir(c.BlobPath(p.SHA256)), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(c.BlobPath(p.SHA256), []byte("corrupt"), 0600); err != nil {
						t.Fatal(err)
					}
					success = true
				case "corrupt_cached_compressed":
					if err := os.WriteFile(c.BlobPath(hash), []byte("corrupt"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				got, err := Reconstruct(ctx, c, p, s, hash)
				if success {
					if err != nil {
						t.Fatal(err)
					}
					body, err := os.ReadFile(c.BlobPath(got))
					if err != nil || !bytes.Equal(body, plain) {
						t.Fatalf("wrong output %q: %v", body, err)
					}
					blob, err := c.GetBlob(ctx, got)
					if err != nil || blob.Size != int64(len(plain)) {
						t.Fatalf("blob=%+v err=%v", blob, err)
					}
				} else {
					if err == nil {
						t.Fatal("accepted invalid representation")
					}
					if exists, _ := c.BlobExists(fmt.Sprintf("%x", sha256.Sum256(plain))); exists {
						t.Fatal("published unverified plaintext")
					}
				}
				tmp, err := os.ReadDir(filepath.Join(c.Dir(), "tmp"))
				if err != nil || len(tmp) != 0 {
					t.Fatalf("leaked temp files: %v, %v", tmp, err)
				}
			})
		}
	}
}
