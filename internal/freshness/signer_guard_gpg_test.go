package freshness_test

// The signer downgrade guard (signer_guard.go, the hardening plan's Task
// 9d) with internal/gpg's verifier: the adopter, over a real cache, refuses
// a candidate that has lost the strong signer of the suite's current
// anchor, and adopts in every other case the plan lists. Keys are
// throwaway ones go-crypto makes, or the public keys of the apt-shape
// corpus' batch fx20 (no secret is committed). An external test: it runs
// internal/gpg, which imports this package.

import (
	"bytes"
	"crypto"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
	"github.com/linsomniac/apt-cacher-ultra/internal/gpg"
)

// guardKeys are the throwaway keys of the guard's tests: strong (Ed25519)
// and other (a second strong key, P-256), and weak, an RSA-3072 key whose
// self-signature is SHA-1 (as openresty's and sublime's first key are:
// debian:13's sqv refuses it, the gpgv-based apt accept it).
type guardKeys struct{ strong, other, weak *openpgp.Entity }

func newGuardKeys(t *testing.T) guardKeys {
	t.Helper()
	mk := func(name string, cfg packet.Config) *openpgp.Entity {
		e, err := openpgp.NewEntity("Guard "+name, "", name+"@example.invalid", &cfg)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	weak := mk("weak", packet.Config{Algorithm: packet.PubKeyAlgoRSA, RSABits: 3072})
	for _, id := range weak.Identities {
		sig := *id.SelfSignature
		sig.Hash = crypto.SHA1
		if err := sig.SignUserId(id.UserId.Id, weak.PrimaryKey, weak.PrivateKey,
			&packet.Config{NonDeterministicSignaturesViaNotation: new(bool)}); err != nil {
			t.Fatal(err)
		}
		id.SelfSignature, id.Signatures = &sig, []*packet.Signature{&sig}
	}
	return guardKeys{
		strong: mk("strong", packet.Config{Algorithm: packet.PubKeyAlgoEdDSA, Curve: packet.Curve25519}),
		other:  mk("other", packet.Config{Algorithm: packet.PubKeyAlgoECDSA, Curve: packet.CurveNistP256}),
		weak:   weak,
	}
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// guardVerifier is internal/gpg's verifier of ents, under the defaults (no
// pin), or under accept_any_signer's bypass.
func guardVerifier(t *testing.T, acceptAny bool, ents ...*openpgp.Entity) *gpg.Verifier {
	t.Helper()
	var srcs []gpg.EmbeddedSource
	for i, e := range ents {
		var buf bytes.Buffer
		if err := e.Serialize(&buf); err != nil {
			t.Fatal(err)
		}
		srcs = append(srcs, gpg.EmbeddedSource{Name: fmt.Sprintf("k%d.gpg", i), Data: buf.Bytes()})
	}
	return verifierOf(t, acceptAny, srcs...)
}

func verifierOf(t *testing.T, acceptAny bool, srcs ...gpg.EmbeddedSource) *gpg.Verifier {
	t.Helper()
	keyring, err := gpg.LoadKeyringWithEmbedded(nil, srcs, quiet)
	if err != nil {
		t.Fatal(err)
	}
	v, err := gpg.NewVerifier(gpg.VerifierConfig{Keyring: keyring, RequireSignature: true, AllowShortKeyID: true,
		AcceptAnySigner: acceptAny, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// packages is the rig's one member, an empty flat Packages.
var packages []byte

// releaseText is a flat Release labelled label, declaring the empty
// Packages.
func releaseText(label string) []byte {
	sum := sha256.Sum256(packages)
	return []byte(fmt.Sprintf("Origin: guard\nLabel: %s\nSuite: stable\nSHA256:\n %s %d Packages\n",
		label, hex.EncodeToString(sum[:]), len(packages)))
}

// inline is text clearsigned by signers, in order (one armor block).
func inline(t *testing.T, text []byte, signers ...*openpgp.Entity) []byte {
	t.Helper()
	var keys []*packet.PrivateKey
	for _, e := range signers {
		keys = append(keys, e.PrivateKey)
	}
	var buf bytes.Buffer
	w, err := clearsign.EncodeMulti(&buf, keys, &packet.Config{DefaultHash: crypto.SHA256})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(text); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	buf.WriteByte('\n')
	return buf.Bytes()
}

// detached is text's Release.gpg signed by signers, in order: their
// binary-mode packets in one armor block.
func detached(t *testing.T, text []byte, signers ...*openpgp.Entity) []byte {
	t.Helper()
	var pkts bytes.Buffer
	for _, e := range signers {
		if err := openpgp.DetachSign(&pkts, e, bytes.NewReader(text), &packet.Config{DefaultHash: crypto.SHA256}); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, "PGP SIGNATURE", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(pkts.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	buf.WriteByte('\n')
	return buf.Bytes()
}

// newRig is a GuardRig whose adopter verifies with v and whose upstream
// serves the empty Packages.
func newRig(t *testing.T, v freshness.Verifier) *freshness.GuardRig {
	t.Helper()
	r := freshness.NewGuardRig(t, v)
	r.Serve("Packages", packages)
	return r
}

// adopted fails t unless err is nil and the suite's current snapshot moved
// from was.
func adopted(t *testing.T, r *freshness.GuardRig, what string, was int64, err error) {
	t.Helper()
	if err != nil || r.Current() == was {
		t.Fatalf("%s: %v (current %d, was %d); want it adopted", what, err, r.Current(), was)
	}
}

// refused fails t unless err is the signer downgrade refusal, the current
// snapshot is still was, and the adopter logged adoption_signer_downgrade
// at WARN naming strong as the current strong signer and weak among the new
// anchor's signers.
func refused(t *testing.T, r *freshness.GuardRig, what string, was int64, err error, strong, weak string) {
	t.Helper()
	if !errors.Is(err, freshness.ErrAdoptionSignerDowngrade) || r.Current() != was {
		t.Fatalf("%s: %v (current %d, was %d); want ErrAdoptionSignerDowngrade, the current snapshot kept", what, err, r.Current(), was)
	}
	for _, line := range strings.Split(r.Logs(), "\n") {
		if strings.Contains(line, `"msg":"adoption_signer_downgrade"`) && strings.Contains(line, `"level":"WARN"`) &&
			strings.Contains(line, `"current_strong_signer":"`+strong+`"`) && strings.Contains(line, `"new_signers":[`) &&
			strings.Contains(line, weak) {
			return
		}
	}
	t.Fatalf("%s: no WARN adoption_signer_downgrade naming %s and %s in:\n%s", what, strong, weak, r.Logs())
}

func fpOf(e *openpgp.Entity) string {
	return strings.ToUpper(hex.EncodeToString(e.PrimaryKey.Fingerprint))
}

// The plan's first case and the strip it exists for: the current anchor
// is signed by a strong and a weak key, the candidate by the weak one
// alone: signer_downgrade, the current snapshot kept, no member fetched.
// The same for the detached form, and for a detached candidate after an
// inline current anchor.
func TestSignerGuard_RefusesTheStrongSignersStrip(t *testing.T) {
	k := newGuardKeys(t)
	r := newRig(t, guardVerifier(t, false, k.strong, k.weak))
	if err := r.Run(inline(t, releaseText("v1"), k.strong, k.weak)); err != nil {
		t.Fatalf("v1 by strong and weak: %v", err)
	}
	cur := r.Current()
	refused(t, r, "v2 inline by weak alone", cur, r.Run(inline(t, releaseText("v2"), k.weak)), fpOf(k.strong), fpOf(k.weak))
	refused(t, r, "v2 detached by weak alone", cur, r.RunDetached(releaseText("v2d"), detached(t, releaseText("v2d"), k.weak)),
		fpOf(k.strong), fpOf(k.weak))

	d := newRig(t, guardVerifier(t, false, k.strong, k.weak))
	if err := d.RunDetached(releaseText("v1"), detached(t, releaseText("v1"), k.weak, k.strong)); err != nil {
		t.Fatalf("detached v1 by weak and strong: %v", err)
	}
	cur = d.Current()
	refused(t, d, "detached v2 by weak alone", cur, d.RunDetached(releaseText("v2"), detached(t, releaseText("v2"), k.weak)),
		fpOf(k.strong), fpOf(k.weak))
	adopted(t, d, "detached v3 by weak and strong", cur, d.RunDetached(releaseText("v3"), detached(t, releaseText("v3"), k.weak, k.strong)))
}

// The plan's other cases, each adopted: the candidate signed by the strong
// and the weak key; a suite whose current anchor has only the weak key's
// signature, its candidate too (openresty); a candidate signed only by
// another strong key; the strong key removed from the keyring (a restart
// with the new keyring: the current anchor's strong packet is then a key
// the cache lacks); and a first adoption by the weak key alone (the Open
// gap: no current snapshot to compare with, ruling G6).
func TestSignerGuard_AdoptsWhatLosesNoStrongSigner(t *testing.T) {
	k := newGuardKeys(t)
	full := func(t *testing.T) (*freshness.GuardRig, int64) {
		t.Helper()
		r := newRig(t, guardVerifier(t, false, k.strong, k.other, k.weak))
		if err := r.Run(inline(t, releaseText("v1"), k.strong, k.weak)); err != nil {
			t.Fatalf("v1: %v", err)
		}
		return r, r.Current()
	}
	t.Run("strong and weak", func(t *testing.T) {
		r, cur := full(t)
		adopted(t, r, "v2 by strong and weak", cur, r.Run(inline(t, releaseText("v2"), k.weak, k.strong)))
	})
	t.Run("weak to weak", func(t *testing.T) {
		r := newRig(t, guardVerifier(t, false, k.strong, k.weak))
		if err := r.Run(inline(t, releaseText("v1"), k.weak)); err != nil {
			t.Fatalf("v1 by weak: %v", err)
		}
		adopted(t, r, "v2 by weak", r.Current(), r.Run(inline(t, releaseText("v2"), k.weak)))
	})
	t.Run("another strong key", func(t *testing.T) {
		r, cur := full(t)
		adopted(t, r, "v2 by the other strong key", cur, r.Run(inline(t, releaseText("v2"), k.other)))
	})
	t.Run("strong key removed", func(t *testing.T) {
		r, cur := full(t)
		r.SetVerifier(guardVerifier(t, false, k.weak))
		adopted(t, r, "v2 by weak, the strong key gone", cur, r.Run(inline(t, releaseText("v2"), k.weak)))
	})
	t.Run("first adoption by weak", func(t *testing.T) {
		r := newRig(t, guardVerifier(t, false, k.strong, k.weak))
		adopted(t, r, "v1 by weak, no current snapshot", 0, r.Run(inline(t, releaseText("v1"), k.weak)))
	})
}

// Ruling G1: under accept_any_signer's bypass (no pin matches) no packet
// is verified, so no anchor has a strong signer and the guard never fires.
func TestSignerGuard_NeverFiresUnderTheBypass(t *testing.T) {
	k := newGuardKeys(t)
	r := newRig(t, guardVerifier(t, true, k.strong, k.weak))
	if err := r.Run(inline(t, releaseText("v1"), k.strong, k.weak)); err != nil {
		t.Fatalf("v1: %v", err)
	}
	adopted(t, r, "v2 by weak under the bypass", r.Current(), r.Run(inline(t, releaseText("v2"), k.weak)))
}

// Ruling G2: the guard fails open on its own errors. With the current
// anchor's pool file gone, it stands down (INFO adoption_signer_guard_skipped,
// reason anchor_missing) and the strip is adopted as before Task 9d.
func TestSignerGuard_StandsDownWhenTheCurrentAnchorIsGone(t *testing.T) {
	k := newGuardKeys(t)
	r := newRig(t, guardVerifier(t, false, k.strong, k.weak))
	if err := r.Run(inline(t, releaseText("v1"), k.strong, k.weak)); err != nil {
		t.Fatalf("v1: %v", err)
	}
	cur := r.Current()
	r.RemoveCurrentAnchor()
	adopted(t, r, "v2 by weak, the current anchor unreadable", cur, r.Run(inline(t, releaseText("v2"), k.weak)))
	if !strings.Contains(r.Logs(), `"level":"INFO","msg":"adoption_signer_guard_skipped"`) ||
		!strings.Contains(r.Logs(), `"reason":"anchor_missing"`) {
		t.Fatalf("no INFO adoption_signer_guard_skipped with reason anchor_missing in:\n%s", r.Logs())
	}
}

// corpusDir is internal/gpg's apt-shape corpus.
const guardCorpus = "../gpg/testdata/aptshapes"

// The plan's "a weak key of each algorithm class in place of the SHA-1-bound
// one", on the measured anchors of the corpus' batch fx20: each
// pk_ed25519_plus_<weak> (an Ed25519 key's and the weak key's signatures of
// the corpus Release, accepted by all six images) adopted, then its strip,
// pk_<weak> (the weak key's alone, refused by debian:13, ubuntu:24.04 or
// ubuntu:26.04), refused: a SHA-1 and a SHA-224 self-signature, DSA, RSA
// under 2048 bits, P-521, brainpool, secp256k1, an RSA-1024 primary under an
// Ed25519 signing subkey, and a SHA-1 subkey binding.
func TestSignerGuard_RefusesTheStripOfEachWeakClass(t *testing.T) {
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(guardCorpus, name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, weak := range []string{
		"rsa3072_sha1bound", "rsa3072_sha224bound", "dsa2048", "rsa1024", "nistp521", "bp256", "secp256k1",
		"ed25519_on_rsa1024", "ed25519_on_ed25519_sha1bind",
	} {
		t.Run(weak, func(t *testing.T) {
			pair := "pk_ed25519_plus_" + weak
			v := verifierOf(t, false, gpg.EmbeddedSource{Name: pair + ".keys.asc", Data: read(pair + ".keys.asc")})
			r := freshness.NewGuardRig(t, v)
			r.Serve("Packages", nil)
			if err := r.Run(read(pair + ".InRelease")); err != nil {
				t.Fatalf("%s: %v; want it adopted", pair, err)
			}
			cur := r.Current()
			err := r.Run(read("pk_" + weak + ".InRelease"))
			if !errors.Is(err, freshness.ErrAdoptionSignerDowngrade) || r.Current() != cur {
				t.Fatalf("pk_%s after %s: %v; want ErrAdoptionSignerDowngrade, the current snapshot kept", weak, pair, err)
			}
		})
	}
}

// The Date rule still runs first, and the guard compares no clock of its
// own with an anchor's Date: a candidate the weak key signs, dated after
// the current anchor, is refused all the same.
func TestSignerGuard_IgnoresTheAnchorsDate(t *testing.T) {
	k := newGuardKeys(t)
	r := newRig(t, guardVerifier(t, false, k.strong, k.weak))
	dated := func(label string, at time.Time) []byte {
		return append([]byte("Date: "+at.UTC().Format(time.RFC1123)+"\n"), releaseText(label)...)
	}
	if err := r.Run(inline(t, dated("v1", time.Now().Add(-48*time.Hour)), k.strong, k.weak)); err != nil {
		t.Fatalf("v1: %v", err)
	}
	cur := r.Current()
	refused(t, r, "v2 by weak, dated later", cur, r.Run(inline(t, dated("v2", time.Now().Add(-time.Hour)), k.weak)),
		fpOf(k.strong), fpOf(k.weak))
}
