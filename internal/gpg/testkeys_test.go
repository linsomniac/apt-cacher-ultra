package gpg

import (
	"bytes"
	"crypto"
	"fmt"
	"io"
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
	defer func() { _ = f.Close() }()

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
	defer func() { _ = f.Close() }()
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
	defer func() { _ = f.Close() }()
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

// detachSignWith produces a detached signature (Release.gpg
// equivalent) over message. When armored is true the result is
// ASCII-armored ("-----BEGIN PGP SIGNATURE-----..."), matching
// apt-ftparchive's `gpg --detach-sign --armor` output. When false
// the result is the binary signature packet bytes.
func detachSignWith(t *testing.T, e *openpgp.Entity, message []byte, armored bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	cfg := &packet.Config{
		DefaultHash: crypto.SHA256,
		Time:        time.Now,
	}
	var err error
	if armored {
		err = openpgp.ArmoredDetachSign(&buf, e, bytes.NewReader(message), cfg)
	} else {
		err = openpgp.DetachSign(&buf, e, bytes.NewReader(message), cfg)
	}
	if err != nil {
		t.Fatalf("detach sign: %v", err)
	}
	return buf.Bytes()
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

// buildMultiSigBlock returns a clearsigned message whose cleartext
// is taken from `current` and whose armored-signature body is the
// concatenation of `stale`'s signature packets followed by
// `current`'s. The result simulates an InRelease that carries an
// extra (stale) signature alongside the live one — a structure the
// per-packet verify-and-trust loop must traverse.
func buildMultiSigBlock(current, stale []byte) ([]byte, error) {
	cur, _ := clearsign.Decode(current)
	stl, _ := clearsign.Decode(stale)
	if cur == nil || stl == nil {
		return nil, errInvalidFixture
	}
	curSig, err := readArmoredBlockBody(cur)
	if err != nil {
		return nil, fmt.Errorf("decode current armor: %w", err)
	}
	stlSig, err := readArmoredBlockBody(stl)
	if err != nil {
		return nil, fmt.Errorf("decode stale armor: %w", err)
	}
	combined := append([]byte{}, stlSig...)
	combined = append(combined, curSig...)
	return assembleClearsignedBlock(cur.Plaintext, combined)
}

// substituteSignatures returns a clearsigned message whose cleartext
// is `base`'s but whose signature(s) come from `from`. The result
// is structurally a valid clearsigned message, but the signatures
// are "wrong" because they were made over different cleartext —
// useful to confirm the verifier rejects when no packet verifies.
func substituteSignatures(base, from []byte) ([]byte, error) {
	b, _ := clearsign.Decode(base)
	f, _ := clearsign.Decode(from)
	if b == nil || f == nil {
		return nil, errInvalidFixture
	}
	fromSig, err := readArmoredBlockBody(f)
	if err != nil {
		return nil, fmt.Errorf("decode from armor: %w", err)
	}
	return assembleClearsignedBlock(b.Plaintext, fromSig)
}

// readArmoredBlockBody pulls the binary signature bytes out of a
// clearsign.Block's already-de-armored signature reader.
func readArmoredBlockBody(b *clearsign.Block) ([]byte, error) {
	out, err := io.ReadAll(b.ArmoredSignature.Body)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// assembleClearsignedBlock builds a "clearsigned message" envelope
// around plaintext + binary signature packet bytes. Mirrors
// clearsign.Encode's framing: the LF immediately preceding the
// "-----BEGIN PGP SIGNATURE-----" line is treated as a structural
// separator (stripped from the signed content), so the assembler
// must write an extra LF after plaintext to preserve plaintext's
// own trailing newline.
func assembleClearsignedBlock(plaintext, sigPackets []byte) ([]byte, error) {
	var out bytes.Buffer
	out.WriteString("-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\n")
	out.Write(plaintext)
	// Ensure plaintext is followed by an extra LF before BEGIN
	// SIGNATURE. clearsign.Decode strips the LF immediately
	// preceding the marker as framing, so two LFs in succession
	// preserve plaintext's own LF terminator.
	if len(plaintext) > 0 && plaintext[len(plaintext)-1] != '\n' {
		out.WriteByte('\n')
	}
	out.WriteByte('\n')
	armorWriter, err := armor.Encode(&out, "PGP SIGNATURE", nil)
	if err != nil {
		return nil, err
	}
	if _, err := armorWriter.Write(sigPackets); err != nil {
		return nil, err
	}
	if err := armorWriter.Close(); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

var errInvalidFixture = fmt.Errorf("test fixture: clearsign.Decode returned nil")

// clearsignWithoutIssuerFingerprint signs plaintext using e but
// suppresses the IssuerFingerprint subpacket (33) so the resulting
// clearsigned block carries only the legacy 8-byte issuer keyid in
// subpacket 16. Real-world third-party signers (notably Docker,
// Microsoft) still publish InRelease bodies in this shape; the
// fixture lets the AllowShortKeyID tests exercise the SPEC2 §7.6.3
// fallback path against a cryptographically valid signature.
//
// Implementation: openpgp.Signature.Sign reads
// `priv.PublicKey.Fingerprint` and `priv.PublicKey.KeyId` directly
// into `sig.IssuerFingerprint` / `sig.IssuerKeyId`. By temporarily
// clearing PublicKey.Fingerprint before clearsign.Encode runs, the
// signing path emits a signature whose `IssuerFingerprint` is nil
// and whose hashed-subpackets area does not include subpacket 33 —
// so the digest is computed over (and verified against) bytes that
// genuinely lack the long-form fingerprint. KeyId remains the
// already-computed 8-byte short id, preserving subpacket 16.
func clearsignWithoutIssuerFingerprint(t *testing.T, e *openpgp.Entity, plaintext []byte) []byte {
	t.Helper()
	origFP := e.PrimaryKey.Fingerprint
	e.PrimaryKey.Fingerprint = nil
	defer func() { e.PrimaryKey.Fingerprint = origFP }()
	return clearsignWith(t, e, plaintext)
}
