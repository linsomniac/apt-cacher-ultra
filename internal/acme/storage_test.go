package acme

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/servertls"
	"github.com/linsomniac/apt-cacher-ultra/internal/testutil/certfixture"
	"golang.org/x/sys/unix"
)

func testStore(t *testing.T) *store {
	t.Helper()
	s, err := openStore(filepath.Join(t.TempDir(), "acme"), "https://ca.example/directory", "cache.example.net")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.close)
	return s
}

func TestAtomicRecordFailuresPreserveCoherentPair(t *testing.T) {
	a, b := certfixture.New(t), certfixture.New(t)
	old, next := certificateRecord{Version: 1, Certificate: a.Cert, Key: a.Key}, certificateRecord{Version: 1, Certificate: b.Cert, Key: b.Key}
	for _, step := range []string{"write", "file_sync", "rename", "directory_sync"} {
		t.Run(step, func(t *testing.T) {
			s := testStore(t)
			if err := s.write(s.certName, old); err != nil {
				t.Fatal(err)
			}
			s.fail = func(at string) error {
				if at == step {
					return errors.New("injected I/O failure")
				}
				return nil
			}
			if err := s.write(s.certName, next); err == nil {
				t.Fatal("fault not hit")
			}
			var got certificateRecord
			if err := s.read(s.certName, &got); err != nil {
				t.Fatal(err)
			}
			c, err := servertls.Parse(got.Certificate, got.Key, "", time.Now())
			if err != nil {
				t.Fatalf("interrupted save tore pair: %v", err)
			}
			want := a.Leaf
			if step == "directory_sync" {
				want = b.Leaf
			}
			if !c.Leaf.Equal(want) {
				t.Fatal("wrong generation after failure")
			}
			var info unix.Stat_t
			if err := unix.Fstatat(s.fd, s.certName, &info, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				t.Fatal(err)
			}
			if info.Mode&0777 != 0600 {
				t.Fatalf("record mode %o", info.Mode)
			}
		})
	}
}

func TestStoreRejectsUnsafePaths(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo", "public", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			s := testStore(t)
			switch kind {
			case "symlink":
				if err := unix.Symlinkat("/etc/passwd", s.fd, s.certName); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := unix.Mkfifoat(s.fd, s.certName, 0600); err != nil {
					t.Fatal(err)
				}
			default:
				if err := s.write(s.certName, certificateRecord{Version: 1}); err != nil {
					t.Fatal(err)
				}
				if kind == "public" {
					if err := unix.Fchmodat(s.fd, s.certName, 0644, 0); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "hardlink" {
					if err := unix.Linkat(s.fd, s.certName, s.fd, "alias", 0); err != nil {
						t.Fatal(err)
					}
				}
			}
			var rec certificateRecord
			if err := s.read(s.certName, &rec); err == nil {
				t.Fatal("unsafe path accepted")
			}
		})
	}
	parent, target := t.TempDir(), t.TempDir()
	if err := os.Symlink(target, filepath.Join(parent, "acme")); err != nil {
		t.Fatal(err)
	}
	if s, err := openStore(filepath.Join(parent, "acme"), "https://ca.example/directory", "cache.example.net"); err == nil {
		s.close()
		t.Fatal("state directory symlink accepted")
	}
}

func TestLockCancellation(t *testing.T) {
	s := testStore(t)
	release, err := s.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if release, err := s.lock(ctx); err == nil {
		release()
		t.Fatal("second writer got lock")
	}
}
