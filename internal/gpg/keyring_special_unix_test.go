//go:build unix

package gpg

// Tests of the keyring loader against special files in a keyring directory
// (the fourth re-review's V-R4M1): LoadKeyring stats a path before opening
// it and skips what is not a regular file. A FIFO (opened for reading, open
// waits for a writer) blocked the start forever; a device, or a symlink to
// one, was read to the cap at every start.

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestLoadKeyring_SkipsWhatIsNotARegularFile: a FIFO named .gpg, a symlink
// to /dev/zero named .asc and a dangling symlink beside a real key file and
// a symlink to another (as /usr/share/keyrings holds them): both keys are
// loaded, each special path is skipped with a WARN naming it, and the load
// returns (a FIFO is never opened: a goroutine that opens it for reading
// waits until the test, in its cleanup, opens it for writing).
func TestLoadKeyring_SkipsWhatIsNotARegularFile(t *testing.T) {
	a := newTestEntity(t, "A", "a@example.com")
	b := newTestEntity(t, "B", "b@example.com")
	dir := makeTestDir(t)
	writeFile(t, filepath.Join(dir, "a.gpg"), binaryKey(t, a))
	elsewhere := makeTestDir(t)
	writeFile(t, filepath.Join(elsewhere, "b.gpg"), binaryKey(t, b))
	if err := os.Symlink(filepath.Join(elsewhere, "b.gpg"), filepath.Join(dir, "b-link.gpg")); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo.gpg")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("test premise: mkfifo: %v", err)
	}
	if _, err := os.Stat("/dev/zero"); err != nil {
		t.Skipf("test premise: /dev/zero: %v", err)
	}
	if err := os.Symlink("/dev/zero", filepath.Join(dir, "zero.asc")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "gone.gpg"), filepath.Join(dir, "dangling.gpg")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Releases a reader blocked on the FIFO, should the loader have
		// opened it (O_NONBLOCK: ENXIO when none waits, which is the pass).
		if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
	})
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	type result struct {
		keyring *Keyring
		err     error
	}
	done := make(chan result, 1)
	go func() {
		k, err := LoadKeyring([]string{dir}, logger)
		done <- result{k, err}
	}()
	var got result
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("LoadKeyring did not return in 10 s: it opened the FIFO")
	}
	if got.err != nil || got.keyring.Size() != 2 || !got.keyring.HasFingerprint(upperFP(a.PrimaryKey.Fingerprint)) || !got.keyring.HasFingerprint(upperFP(b.PrimaryKey.Fingerprint)) {
		t.Fatalf("LoadKeyring: %v, %d keys; want A and B (B through its symlink)", got.err, got.keyring.Size())
	}
	for _, name := range []string{"fifo.gpg", "zero.asc", "dangling.gpg"} {
		if !strings.Contains(logs.String(), name) {
			t.Errorf("no WARN names %s:\n%s", name, logs.String())
		}
	}
	for _, name := range []string{"a.gpg", "b-link.gpg"} {
		if strings.Contains(logs.String(), name) {
			t.Errorf("a WARN names the regular key file %s:\n%s", name, logs.String())
		}
	}
	if !strings.Contains(logs.String(), "not a regular file") {
		t.Errorf("no WARN says why the special files are skipped:\n%s", logs.String())
	}
}
