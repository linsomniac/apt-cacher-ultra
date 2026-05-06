package gpg

import (
	"bytes"
	"crypto"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// AIDEV-NOTE: keypair generation uses RSA-2048; openpgp's default is
// adequate for tests but we pin the algorithm/size so the runtime cost
// of `go test` is predictable. Default openpgp.NewEntity uses RSA-2048.

// newTestEntity creates an openpgp signing entity for use in tests.
// The returned entity is unencrypted (no passphrase) — fine for test
// fixtures, never for production keys.
func newTestEntity(t *testing.T, name, email string) *openpgp.Entity {
	t.Helper()
	cfg := &packet.Config{
		Algorithm: packet.PubKeyAlgoRSA,
		RSABits:   2048,
		Time:      func() time.Time { return time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC) },
	}
	e, err := openpgp.NewEntity(name, "", email, cfg)
	if err != nil {
		t.Fatalf("NewEntity %q: %v", name, err)
	}
	return e
}

// newExpiredEntity creates a signing key whose self-signature
// already expired before "now". openpgp signature checks consult the
// self-signature expiration when picking a usable key.
func newExpiredEntity(t *testing.T, name, email string) *openpgp.Entity {
	t.Helper()
	// Issue the self-signature with a key lifetime of 1 second, but
	// using a creation time well in the past — by the time the test
	// runs, the key is expired.
	past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	cfg := &packet.Config{
		Algorithm:       packet.PubKeyAlgoRSA,
		RSABits:         2048,
		Time:            func() time.Time { return past },
		KeyLifetimeSecs: 1, // 1 second after creation = always expired now
	}
	e, err := openpgp.NewEntity(name, "", email, cfg)
	if err != nil {
		t.Fatalf("NewEntity %q (expired): %v", name, err)
	}
	return e
}

// writeArmoredPubKey serializes the public part of e to path as
// ASCII-armor (".asc" form). Mirrors `gpg --export --armor`.
func writeArmoredPubKey(t *testing.T, path string, e *openpgp.Entity) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()

	w, err := armor.Encode(f, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatalf("armor.Encode: %v", err)
	}
	if err := e.Serialize(w); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("armor close: %v", err)
	}
}

// writeBinaryPubKey serializes the public part of e to path in
// binary form (".gpg"). Mirrors `gpg --export`.
func writeBinaryPubKey(t *testing.T, path string, e *openpgp.Entity) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	if err := e.Serialize(f); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
}

// writeArmoredPubKeyBundle writes multiple pubkey entities into a
// single armored block — the format `gpg --export --armor a b`
// produces.
func writeArmoredPubKeyBundle(t *testing.T, path string, entities ...*openpgp.Entity) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	w, err := armor.Encode(f, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatalf("armor.Encode: %v", err)
	}
	for _, e := range entities {
		if err := e.Serialize(w); err != nil {
			t.Fatalf("Serialize: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("armor close: %v", err)
	}
}

// clearsignWith produces a clearsigned message body equivalent to what
// apt's repo-signing tooling emits for InRelease. The signature is
// over the canonicalized cleartext.
func clearsignWith(t *testing.T, e *openpgp.Entity, plaintext []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := clearsign.Encode(&buf, e.PrivateKey, &packet.Config{
		DefaultHash: crypto.SHA256,
		Time:        time.Now,
	})
	if err != nil {
		t.Fatalf("clearsign.Encode: %v", err)
	}
	if _, err := w.Write(plaintext); err != nil {
		t.Fatalf("clearsign write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("clearsign close: %v", err)
	}
	return buf.Bytes()
}

// makeTestDir returns a tempdir wired into the test cleanup hook.
func makeTestDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	return d
}

// writeFile is a tiny helper for "drop these bytes into a file."
func writeFile(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir parents %s: %v", path, err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
