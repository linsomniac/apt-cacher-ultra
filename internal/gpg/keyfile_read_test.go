package gpg

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestParseKeyringFile_RefusesAFileOverTheCapUnread: the third re-review's
// V-R3M2. A file larger than keyringMaxFile in a keyring directory (a disk
// image named .gpg) was read whole, 256 MiB and io.ReadAll's growth, at
// every start, before it was refused; its stat refuses it now before an
// octet of it is read: the refusal allocates next to nothing.
func TestParseKeyringFile_RefusesAFileOverTheCapUnread(t *testing.T) {
	path := filepath.Join(makeTestDir(t), "disk-image.gpg")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(keyringMaxFile + 1); err != nil { // sparse: no disk
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	_, err = parseKeyringFile(path)
	runtime.ReadMemStats(&m1)
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("a file of %d octets: %v; want it refused as larger than the cap", keyringMaxFile+1, err)
	}
	if got := m1.TotalAlloc - m0.TotalAlloc; got > 16<<20 {
		t.Errorf("refusing it allocated %d MiB; want it refused unread", got>>20)
	}
}

// TestReadKeyFile_ReadsAFileIntoABufferOfItsSize: a key file under the cap
// is read into one buffer of its size (and one octet more, which finds a
// file grown since its stat), a file one octet over a cap refused, one at
// it read; and a file whose stat gives no size, or which grows past it,
// read to its end under the cap (a /proc file, its stat's size 0).
func TestReadKeyFile_ReadsAFileIntoABufferOfItsSize(t *testing.T) {
	body := armoredKey(t, newTestEntity(t, "A", "a@example.com"))
	path := filepath.Join(makeTestDir(t), "a.asc")
	writeFile(t, path, body)
	b, err := readKeyFile(path, keyringMaxFile)
	if err != nil || !bytes.Equal(b, body) || cap(b) > len(body)+1 {
		t.Errorf("a key file of %d octets: %d octets in a buffer of %d, %v; want it read whole into one of %d", len(body), len(b), cap(b), err, len(body)+1)
	}
	if _, err := readKeyFile(path, len(body)); err != nil {
		t.Errorf("at a cap of its size: %v; want it read", err)
	}
	if _, err := readKeyFile(path, len(body)-1); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("at a cap one octet under its size: %v; want it refused as larger than the cap", err)
	}
	const proc = "/proc/self/cmdline"
	want, err := os.ReadFile(proc)
	if fi, serr := os.Stat(proc); err != nil || serr != nil || fi.Size() != 0 || len(want) < 8 {
		t.Skipf("test premise: %s, its stat's size 0 and its contents read, not here", proc)
	}
	if got, err := readKeyFile(proc, keyringMaxFile); err != nil || !bytes.Equal(got, want) {
		t.Errorf("%s: %q, %v; want %q", proc, got, err, want)
	}
	if _, err := readKeyFile(proc, 4); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("%s at a cap of 4: %v; want it refused as larger than the cap", proc, err)
	}
}
