package gpg

import (
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

// silentLogger discards log output so tests don't pollute stdout but
// real Warn lines are still issued (and would surface if redirected).
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestLoadKeyring_EmptyDir(t *testing.T) {
	dir := makeTestDir(t)
	k, err := LoadKeyring([]string{dir}, silentLogger())
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if !k.Empty() {
		t.Fatalf("Empty=false, want true")
	}
	if k.Size() != 0 {
		t.Fatalf("Size=%d, want 0", k.Size())
	}
}

func TestLoadKeyring_MissingDir(t *testing.T) {
	// SPEC2 §7.6.1: nonexistent dirs are silently skipped — operators
	// commonly have only one of /etc/apt/trusted.gpg.d and
	// /etc/apt/keyrings populated.
	k, err := LoadKeyring([]string{"/nonexistent/path/x", "/nonexistent/path/y"}, silentLogger())
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if !k.Empty() {
		t.Fatalf("Empty=false, want true")
	}
}

func TestLoadKeyring_ArmoredFile(t *testing.T) {
	dir := makeTestDir(t)
	e := newTestEntity(t, "Test One", "one@example.com")
	writeArmoredPubKey(t, filepath.Join(dir, "one.asc"), e)

	k, err := LoadKeyring([]string{dir}, silentLogger())
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if k.Size() != 1 {
		t.Fatalf("Size=%d, want 1", k.Size())
	}
	fp := upperFP(e.PrimaryKey.Fingerprint)
	if !k.HasFingerprint(fp) {
		t.Fatalf("primary fp not present: %s", fp)
	}
}

func TestLoadKeyring_BinaryFile(t *testing.T) {
	dir := makeTestDir(t)
	e := newTestEntity(t, "Test Binary", "bin@example.com")
	writeBinaryPubKey(t, filepath.Join(dir, "bin.gpg"), e)

	k, err := LoadKeyring([]string{dir}, silentLogger())
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if k.Size() != 1 {
		t.Fatalf("Size=%d, want 1", k.Size())
	}
	fp := upperFP(e.PrimaryKey.Fingerprint)
	if !k.HasFingerprint(fp) {
		t.Fatalf("primary fp not present: %s", fp)
	}
}

func TestLoadKeyring_BothExtensions(t *testing.T) {
	dir := makeTestDir(t)
	a := newTestEntity(t, "Armored", "a@example.com")
	b := newTestEntity(t, "Binary", "b@example.com")
	writeArmoredPubKey(t, filepath.Join(dir, "a.asc"), a)
	writeBinaryPubKey(t, filepath.Join(dir, "b.gpg"), b)

	k, err := LoadKeyring([]string{dir}, silentLogger())
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if k.Size() != 2 {
		t.Fatalf("Size=%d, want 2", k.Size())
	}
	if !k.HasFingerprint(upperFP(a.PrimaryKey.Fingerprint)) {
		t.Fatal("missing armored fp")
	}
	if !k.HasFingerprint(upperFP(b.PrimaryKey.Fingerprint)) {
		t.Fatal("missing binary fp")
	}
}

func TestLoadKeyring_IgnoresUnknownExtensions(t *testing.T) {
	dir := makeTestDir(t)
	e := newTestEntity(t, "Real", "real@example.com")
	writeArmoredPubKey(t, filepath.Join(dir, "real.asc"), e)

	// Drop in files that should be ignored.
	writeFile(t, filepath.Join(dir, "README"), []byte("not a keyring"))
	writeFile(t, filepath.Join(dir, "junk.txt"), []byte("nope"))

	k, err := LoadKeyring([]string{dir}, silentLogger())
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if k.Size() != 1 {
		t.Fatalf("Size=%d, want 1", k.Size())
	}
}

func TestLoadKeyring_BadFileSkipped(t *testing.T) {
	// SPEC2 §7.6.1: parse failures are logged at WARN, not fatal.
	dir := makeTestDir(t)
	good := newTestEntity(t, "Good", "good@example.com")
	writeArmoredPubKey(t, filepath.Join(dir, "good.asc"), good)
	writeFile(t, filepath.Join(dir, "broken.gpg"), []byte("garbage not a keyring"))
	writeFile(t, filepath.Join(dir, "broken.asc"), []byte("-----BEGIN PGP PUBLIC KEY BLOCK-----\n\ninvalid armor body\n-----END PGP PUBLIC KEY BLOCK-----\n"))

	k, err := LoadKeyring([]string{dir}, silentLogger())
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if k.Size() != 1 {
		t.Fatalf("Size=%d (want 1, only the good key should survive)", k.Size())
	}
	if !k.HasFingerprint(upperFP(good.PrimaryKey.Fingerprint)) {
		t.Fatal("good key not present")
	}
}

func TestLoadKeyring_DedupesAcrossDirs(t *testing.T) {
	// Same key in both dirs should appear once.
	dir1 := makeTestDir(t)
	dir2 := makeTestDir(t)
	e := newTestEntity(t, "Dup", "dup@example.com")
	writeArmoredPubKey(t, filepath.Join(dir1, "dup.asc"), e)
	writeBinaryPubKey(t, filepath.Join(dir2, "dup.gpg"), e)

	k, err := LoadKeyring([]string{dir1, dir2}, silentLogger())
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if k.Size() != 1 {
		t.Fatalf("Size=%d, want 1", k.Size())
	}
}

func TestLoadKeyring_MultipleKeysInOneFile(t *testing.T) {
	// Some operators ship a single armored "bundle" with multiple
	// pubkeys (this is what `gpg --export --armor a b` emits — one
	// armor block, both keys' packets concatenated inside). Each
	// entity should land separately.
	dir := makeTestDir(t)
	a := newTestEntity(t, "A", "a@example.com")
	b := newTestEntity(t, "B", "b@example.com")
	writeArmoredPubKeyBundle(t, filepath.Join(dir, "bundle.asc"), a, b)

	k, err := LoadKeyring([]string{dir}, silentLogger())
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if k.Size() != 2 {
		t.Fatalf("Size=%d, want 2 (multi-key bundle)", k.Size())
	}
}

func TestKeyring_Subset(t *testing.T) {
	dir := makeTestDir(t)
	a := newTestEntity(t, "A", "a@example.com")
	b := newTestEntity(t, "B", "b@example.com")
	c := newTestEntity(t, "C", "c@example.com")
	writeArmoredPubKey(t, filepath.Join(dir, "a.asc"), a)
	writeArmoredPubKey(t, filepath.Join(dir, "b.asc"), b)
	writeArmoredPubKey(t, filepath.Join(dir, "c.asc"), c)

	k, err := LoadKeyring([]string{dir}, silentLogger())
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if k.Size() != 3 {
		t.Fatalf("Size=%d, want 3", k.Size())
	}

	// Pick two of three.
	want := map[string]struct{}{
		upperFP(a.PrimaryKey.Fingerprint): {},
		upperFP(c.PrimaryKey.Fingerprint): {},
	}
	subset := k.Subset(want)
	if len(subset) != 2 {
		t.Fatalf("Subset len=%d, want 2", len(subset))
	}
	gotFPs := map[string]bool{}
	for _, e := range subset {
		gotFPs[upperFP(e.PrimaryKey.Fingerprint)] = true
	}
	if !gotFPs[upperFP(a.PrimaryKey.Fingerprint)] || !gotFPs[upperFP(c.PrimaryKey.Fingerprint)] {
		t.Fatalf("Subset returned wrong entities")
	}
	if gotFPs[upperFP(b.PrimaryKey.Fingerprint)] {
		t.Fatalf("Subset included unrequested entity")
	}
}

func TestKeyring_Subset_EmptyInput(t *testing.T) {
	dir := makeTestDir(t)
	a := newTestEntity(t, "A", "a@example.com")
	writeArmoredPubKey(t, filepath.Join(dir, "a.asc"), a)
	k, _ := LoadKeyring([]string{dir}, silentLogger())
	subset := k.Subset(map[string]struct{}{})
	if len(subset) != 0 {
		t.Fatalf("empty fp set should produce empty subset, got %d", len(subset))
	}
}

func TestKeyring_HasFingerprint_CaseInsensitive(t *testing.T) {
	dir := makeTestDir(t)
	a := newTestEntity(t, "Case", "case@example.com")
	writeArmoredPubKey(t, filepath.Join(dir, "a.asc"), a)
	k, _ := LoadKeyring([]string{dir}, silentLogger())

	upper := upperFP(a.PrimaryKey.Fingerprint)
	lower := strings.ToLower(upper)
	mixed := strings.ToLower(upper[:20]) + strings.ToUpper(upper[20:])

	if !k.HasFingerprint(upper) {
		t.Fatal("upper not found")
	}
	if !k.HasFingerprint(lower) {
		t.Fatal("lower not found")
	}
	if !k.HasFingerprint(mixed) {
		t.Fatal("mixed not found")
	}
}

