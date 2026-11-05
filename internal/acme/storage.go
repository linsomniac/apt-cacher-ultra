// Package acme manages certificates for the daemon's own TLS listeners.
package acme

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const maxRecordSize = 3 << 20

type accountRecord struct {
	Version   int    `json:"version"`
	Directory string `json:"directory"`
	Key       []byte `json:"key"`
	URL       string `json:"url,omitempty"`
	Terms     string `json:"terms,omitempty"`
}

type certificateRecord struct {
	Version     int       `json:"version"`
	Directory   string    `json:"directory"`
	Name        string    `json:"name"`
	Certificate []byte    `json:"certificate"`
	Key         []byte    `json:"key"`
	RenewAt     time.Time `json:"renew_at"`
	ObtainedAt  time.Time `json:"obtained_at"`
}

type retryRecord struct {
	Version  int       `json:"version"`
	Failures int       `json:"failures"`
	Next     time.Time `json:"next_attempt"`
	Problem  string    `json:"problem"`
}

// store uses a directory descriptor throughout. Daemon-owned path components
// and records cannot redirect reads/writes through symlinks. The configured
// cache directory is an operator-controlled parent, not a remote input.
type store struct {
	fd                  int
	certName, retryName string
	// fail is an instance-local fault seam; production leaves it nil.
	fail func(string) error
}

func hashName(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }

func openStore(stateDir, directory, name string) (*store, error) {
	parent, err := unix.Open(filepath.Dir(stateDir), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("acme storage: cannot open parent directory")
	}
	defer func() { _ = unix.Close(parent) }()
	root, err := privateDir(parent, filepath.Base(stateDir))
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(root) }()
	fd, err := privateDir(root, hashName(directory))
	if err != nil {
		return nil, err
	}
	return &store{fd: fd, certName: hashName(name) + ".certificate.json", retryName: hashName(name) + ".retry.json"}, nil
}

func privateDir(parent int, name string) (int, error) {
	err := unix.Mkdirat(parent, name, 0700)
	if err != nil && !errors.Is(err, unix.EEXIST) {
		return -1, errors.New("acme storage: cannot create directory")
	}
	created := err == nil
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, errors.New("acme storage: expected private directory, not a symlink")
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || int(st.Uid) != os.Geteuid() || st.Mode&077 != 0 {
		_ = unix.Close(fd)
		return -1, errors.New("acme storage: unsafe directory ownership or permissions")
	}
	if created {
		if err := unix.Fsync(parent); err != nil {
			_ = unix.Close(fd)
			return -1, errors.New("acme storage: directory sync failed")
		}
	}
	return fd, nil
}

func (s *store) close() { _ = unix.Close(s.fd) }

func privateFile(fd int) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || int(st.Uid) != os.Geteuid() || st.Mode&077 != 0 || st.Nlink != 1 {
		return errors.New("acme storage: unsafe record type, ownership or permissions")
	}
	return nil
}

func (s *store) read(name string, value any) error {
	fd, err := unix.Openat(s.fd, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return os.ErrNotExist
	}
	if err != nil {
		return errors.New("acme storage: cannot open record")
	}
	f := os.NewFile(uintptr(fd), name)
	defer func() { _ = f.Close() }()
	if err := privateFile(fd); err != nil {
		return err
	}
	b, err := io.ReadAll(io.LimitReader(f, maxRecordSize+1))
	if err != nil || len(b) > maxRecordSize {
		return errors.New("acme storage: record unreadable or too large")
	}
	if err := json.Unmarshal(b, value); err != nil {
		return errors.New("acme storage: invalid record")
	}
	return nil
}

func (s *store) step(name string) error {
	if s.fail != nil {
		return s.fail(name)
	}
	return nil
}

// write commits one coherent record. A failure before rename preserves the old
// record. A directory-sync failure after rename is ambiguous durability: the
// complete new record may be visible, but callers must not publish or retry
// external operations until a restart/recovery validates durable state.
func (s *store) write(name string, value any) error {
	b, err := json.Marshal(value)
	if err != nil || len(b) > maxRecordSize {
		return errors.New("acme storage: record cannot be encoded")
	}
	tmp := ".pending-" + rand.Text()
	fd, err := unix.Openat(s.fd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return errors.New("acme storage: cannot create temporary record")
	}
	f := os.NewFile(uintptr(fd), tmp)
	defer func() { _ = f.Close() }()
	defer func() { _ = unix.Unlinkat(s.fd, tmp, 0) }()
	if err := s.step("write"); err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		return err
	}
	if err := f.Chmod(0600); err != nil {
		return err
	}
	if err := s.step("file_sync"); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := s.step("rename"); err != nil {
		return err
	}
	if err := unix.Renameat(s.fd, tmp, s.fd, name); err != nil {
		return err
	}
	if err := s.step("directory_sync"); err != nil {
		return err
	}
	return unix.Fsync(s.fd)
}

func (s *store) lock(ctx context.Context) (func(), error) {
	fd, err := unix.Openat(s.fd, ".lock", unix.O_CREAT|unix.O_RDWR|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, errors.New("acme storage: cannot open lock")
	}
	if err := privateFile(fd); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	timer := time.NewTicker(50 * time.Millisecond)
	defer timer.Stop()
	for {
		if err := ctx.Err(); err != nil {
			_ = unix.Close(fd)
			return nil, err
		}
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() { _ = unix.Flock(fd, unix.LOCK_UN); _ = unix.Close(fd) }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			_ = unix.Close(fd)
			return nil, errors.New("acme storage: lock failed")
		}
		select {
		case <-ctx.Done():
			_ = unix.Close(fd)
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
