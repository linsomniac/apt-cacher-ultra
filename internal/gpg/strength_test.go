package gpg

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/linsomniac/apt-cacher-ultra/internal/apttext"
	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
)

// The signer downgrade guard's policy (strength.go, the hardening plan's
// Task 9d). The corpus rows of batch fx20 hold the table to apt's verdicts
// (TestVerifier_StrongSignerFollowsTheAptShapeTable); the tests here hold
// each rule on its own, the dated ones on both sides of their date at the
// verifier's clock (ruling G3), with throwaway keys go-crypto makes (no
// secret is committed). go-crypto makes no DSA key: the corpus' pk_dsa*
// rows hold DSA.

// policyMade is when the policy tests' keys are made: before every date the
// table names, so that a verification at either side of one can run.
var policyMade = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// noSaltNotation keeps go-crypto from adding its salt notation, which it
// can only make for the hashes a v6 signature takes (not SHA-1).
var noSaltNotation = new(bool)

// policyKey makes a signing key of cfg's algorithm at policyMade (go-crypto
// adds an encryption subkey too).
func policyKey(t *testing.T, name string, cfg packet.Config) *openpgp.Entity {
	t.Helper()
	cfg.Time = func() time.Time { return policyMade }
	e, err := openpgp.NewEntity("Policy "+name, "", name+"@example.invalid", &cfg)
	if err != nil {
		t.Fatalf("NewEntity %s: %v", name, err)
	}
	return e
}

// The algorithms the policy tests make keys of.
var (
	cfgRSA1024   = packet.Config{Algorithm: packet.PubKeyAlgoRSA, RSABits: 1024}
	cfgRSA2048   = packet.Config{Algorithm: packet.PubKeyAlgoRSA, RSABits: 2048}
	cfgRSA3072   = packet.Config{Algorithm: packet.PubKeyAlgoRSA, RSABits: 3072}
	cfgEd25519   = packet.Config{Algorithm: packet.PubKeyAlgoEdDSA, Curve: packet.Curve25519}
	cfgEd448     = packet.Config{Algorithm: packet.PubKeyAlgoEdDSA, Curve: packet.Curve448}
	cfgEd25519v  = packet.Config{Algorithm: packet.PubKeyAlgoEd25519}
	cfgP256      = packet.Config{Algorithm: packet.PubKeyAlgoECDSA, Curve: packet.CurveNistP256}
	cfgP384      = packet.Config{Algorithm: packet.PubKeyAlgoECDSA, Curve: packet.CurveNistP384}
	cfgP521      = packet.Config{Algorithm: packet.PubKeyAlgoECDSA, Curve: packet.CurveNistP521}
	cfgBP256     = packet.Config{Algorithm: packet.PubKeyAlgoECDSA, Curve: packet.CurveBrainpoolP256}
	cfgSecp256k1 = packet.Config{Algorithm: packet.PubKeyAlgoECDSA, Curve: packet.CurveSecP256k1}
)

// withSigningSubkey adds to e a signing subkey of cfg's algorithm, made at
// policyMade, and returns e.
func withSigningSubkey(t *testing.T, e *openpgp.Entity, cfg packet.Config) *openpgp.Entity {
	t.Helper()
	cfg.Time = func() time.Time { return policyMade }
	if err := e.AddSigningSubkey(&cfg); err != nil {
		t.Fatalf("AddSigningSubkey: %v", err)
	}
	return e
}

// resignSelf signs e's user IDs' self-signatures again with h, as gpg's
// --cert-digest-algo does (each identity then holds that one).
func resignSelf(t *testing.T, e *openpgp.Entity, h crypto.Hash) {
	t.Helper()
	for _, id := range e.Identities {
		sig := *id.SelfSignature
		sig.Hash = h
		if err := sig.SignUserId(id.UserId.Id, e.PrimaryKey, e.PrivateKey,
			&packet.Config{NonDeterministicSignaturesViaNotation: noSaltNotation}); err != nil {
			t.Fatalf("SignUserId with %v: %v", h, err)
		}
		id.SelfSignature, id.Signatures = &sig, []*packet.Signature{&sig}
	}
}

// resignBinding signs the binding of e's last subkey again, with binding
// and its back-signature with back.
func resignBinding(t *testing.T, e *openpgp.Entity, binding, back crypto.Hash) {
	t.Helper()
	cfg := &packet.Config{NonDeterministicSignaturesViaNotation: noSaltNotation}
	sub := &e.Subkeys[len(e.Subkeys)-1]
	bs := *sub.Sig.EmbeddedSignature
	bs.Hash = back
	if err := bs.CrossSignKey(sub.PublicKey, e.PrimaryKey, sub.PrivateKey, cfg); err != nil {
		t.Fatalf("CrossSignKey with %v: %v", back, err)
	}
	sig := *sub.Sig
	sig.Hash, sig.EmbeddedSignature = binding, &bs
	if err := sig.SignKey(sub.PublicKey, e.PrivateKey, cfg); err != nil {
		t.Fatalf("SignKey with %v: %v", binding, err)
	}
	sub.Sig = &sig
}

// readBackKey is e's public key as the keyring reader reads it (the
// self-signatures go-crypto keeps are the reader's choice).
func readBackKey(t *testing.T, e *openpgp.Entity) *openpgp.Entity {
	t.Helper()
	var buf bytes.Buffer
	if err := e.Serialize(&buf); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	kf, err := parseKeyringBytes(buf.Bytes())
	if err != nil || len(kf.entities) != 1 {
		t.Fatalf("the key read back: %v (%d keys)", err, len(kf.entities))
	}
	return kf.entities[0]
}

// signingKey is e's key that signs: its last signing subkey, else the
// primary key.
func signingKey(t *testing.T, e *openpgp.Entity) openpgp.Key {
	t.Helper()
	id := e.PrimaryKey.KeyId
	for _, sub := range e.Subkeys {
		if sub.Sig != nil && sub.Sig.FlagSign {
			id = sub.PublicKey.KeyId
		}
	}
	keys := openpgp.EntityList{e}.KeysByIdUsage(id, packet.KeyFlagSign)
	if len(keys) != 1 {
		t.Fatalf("%d signing keys of key ID %016X", len(keys), id)
	}
	return keys[0]
}

// strongAt is strongSigner's verdict on e's signing key, read back, for a
// packet made at at, at at.
func strongAt(t *testing.T, e *openpgp.Entity, at time.Time) (bool, string) {
	t.Helper()
	return strongSigner(signingKey(t, readBackKey(t, e)), at, at)
}

// rebindAt gives e's user IDs a new self-signature, as before but made at
// at, in place of the old (as gpg --quick-set-expire does).
func rebindAt(t *testing.T, e *openpgp.Entity, at time.Time) {
	t.Helper()
	for _, id := range e.Identities {
		sig := *id.SelfSignature
		sig.CreationTime = at
		if err := sig.SignUserId(id.UserId.Id, e.PrimaryKey, e.PrivateKey,
			&packet.Config{NonDeterministicSignaturesViaNotation: noSaltNotation}); err != nil {
			t.Fatal(err)
		}
		id.SelfSignature, id.Signatures = &sig, []*packet.Signature{&sig}
	}
}

// TestStrongSigner_TheBindingMustPrecedeThePacket: a binding chain
// signature made after the packet is not strong (debian:13's sqv judges
// the binding at the signature's time: ceph's real anchors,
// pk_rsa3072_rebound_minimal), for the primary's self-signature and for a
// subkey's binding; one made at the packet's very time, or before, is.
func TestStrongSigner_TheBindingMustPrecedeThePacket(t *testing.T) {
	rebound := policyKey(t, "rebound", cfgEd25519)
	at := time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC)
	rebindAt(t, rebound, at)
	k := signingKey(t, readBackKey(t, rebound))
	if got, why := strongSigner(k, at.Add(-time.Second), policyNow); got || !strings.Contains(why, "after the packet") {
		t.Errorf("a packet a second before its key's self-signature: strong %v (%q); want not strong", got, why)
	}
	if got, why := strongSigner(k, at, policyNow); !got {
		t.Errorf("a packet made with its key's self-signature: not strong (%s); want strong", why)
	}
	sub := withSigningSubkey(t, policyKey(t, "subrebound", cfgEd25519), cfgEd25519)
	last := &sub.Subkeys[len(sub.Subkeys)-1]
	last.Sig.CreationTime = at
	resignBinding(t, sub, crypto.SHA256, crypto.SHA256)
	sk := signingKey(t, readBackKey(t, sub))
	if got, why := strongSigner(sk, at.Add(-time.Second), policyNow); got || !strings.Contains(why, "(type 0x18) made at") {
		t.Errorf("a packet a second before its subkey's binding: strong %v (%q); want not strong", got, why)
	}
	if got, why := strongSigner(sk, at.Add(time.Hour), policyNow); !got {
		t.Errorf("a packet after its subkey's binding: not strong (%s); want strong", why)
	}
}

var policyNow = time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)

// TestStrongSigner_EachAlgorithmRule: each algorithm row of strongRules,
// for a key of that class signing alone, and every class go-crypto makes
// that no row names (Ed448, Ed25519 as algorithm 27) is not strong.
func TestStrongSigner_EachAlgorithmRule(t *testing.T) {
	for _, c := range []struct {
		name   string
		cfg    packet.Config
		strong bool
		why    string
	}{
		{"ed25519", cfgEd25519, true, ""},
		{"nistp256", cfgP256, true, ""},
		{"nistp384", cfgP384, true, ""},
		{"rsa3072", cfgRSA3072, true, ""},
		{"rsa2048", cfgRSA2048, true, ""},
		{"nistp521", cfgP521, false, "(nistp521) is refused"},
		{"rsa1024", cfgRSA1024, false, "(rsa1024) is refused"},
		{"brainpool", cfgBP256, false, "(brainpool) is refused"},
		{"secp256k1", cfgSecp256k1, false, "(secp256k1) is refused"},
		{"ed448", cfgEd448, false, "is not measured"},
		{"ed25519 as algorithm 27", cfgEd25519v, false, "is not measured"},
	} {
		got, why := strongAt(t, policyKey(t, c.name, c.cfg), policyNow)
		if got != c.strong || !strings.Contains(why, c.why) {
			t.Errorf("%s: strong %v (%q), want %v (%q)", c.name, got, why, c.strong, c.why)
		}
	}
}

// TestStrongSigner_ThePrimaryOfASigningSubkey: a subkey's primary key is
// judged by the primary column (debian:13's sqv judges it, the gpgv-based
// apt do not): P-521 is accepted there, RSA under 2048 bits, brainpool and
// secp256k1 are not; and a strong primary does not make a weak signing
// subkey strong.
func TestStrongSigner_ThePrimaryOfASigningSubkey(t *testing.T) {
	for _, c := range []struct {
		name         string
		primary, sub packet.Config
		strong       bool
		why          string
	}{
		{"ed25519 on ed25519", cfgEd25519, cfgEd25519, true, ""},
		{"ed25519 on nistp521", cfgP521, cfgEd25519, true, ""},
		{"ed25519 on rsa2048", cfgRSA2048, cfgEd25519, true, ""},
		{"ed25519 on rsa1024", cfgRSA1024, cfgEd25519, false, "primary key's algorithm (rsa1024) is refused"},
		{"ed25519 on brainpool", cfgBP256, cfgEd25519, false, "primary key's algorithm (brainpool) is refused"},
		{"ed25519 on secp256k1", cfgSecp256k1, cfgEd25519, false, "primary key's algorithm (secp256k1) is refused"},
		{"ed25519 on ed448", cfgEd448, cfgEd25519, false, "primary key's algorithm (algorithm 22 Curve448) is not measured"},
		{"nistp521 on ed25519", cfgEd25519, cfgP521, false, "its algorithm (nistp521) is refused"},
		{"rsa1024 on ed25519", cfgEd25519, cfgRSA1024, false, "its algorithm (rsa1024) is refused"},
	} {
		e := withSigningSubkey(t, policyKey(t, c.name, c.primary), c.sub)
		got, why := strongAt(t, e, policyNow)
		if got != c.strong || !strings.Contains(why, c.why) {
			t.Errorf("%s: strong %v (%q), want %v (%q)", c.name, got, why, c.strong, c.why)
		}
	}
}

// TestStrongSigner_DatedRules: each dated rule on both sides of its date,
// at the verifier's clock (ruling G3): RSA of 2048 to 3071 bits, as the
// signing key and as the primary, until 2030-02-01; SHA-1 and SHA-224 in a
// binding chain until 2026-02-01, for the primary's self-signature, a
// subkey's binding, and its back-signature alone.
func TestStrongSigner_DatedRules(t *testing.T) {
	rsaDate := time.Date(2030, 2, 1, 0, 0, 0, 0, time.UTC)
	hashDate := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	sha1Self := policyKey(t, "sha1self", cfgEd25519)
	resignSelf(t, sha1Self, crypto.SHA1)
	sha224Self := policyKey(t, "sha224self", cfgRSA3072)
	resignSelf(t, sha224Self, crypto.SHA224)
	sha1Bind := withSigningSubkey(t, policyKey(t, "sha1bind", cfgEd25519), cfgEd25519)
	resignBinding(t, sha1Bind, crypto.SHA1, crypto.SHA256)
	sha1Back := withSigningSubkey(t, policyKey(t, "sha1back", cfgEd25519), cfgEd25519)
	resignBinding(t, sha1Back, crypto.SHA256, crypto.SHA1)
	sha224Bind := withSigningSubkey(t, policyKey(t, "sha224bind", cfgEd25519), cfgEd25519)
	resignBinding(t, sha224Bind, crypto.SHA224, crypto.SHA224)
	for _, c := range []struct {
		name string
		e    *openpgp.Entity
		date time.Time
		why  string
	}{
		{"rsa2048 signing", policyKey(t, "rsa2048", cfgRSA2048), rsaDate, "(rsa2048) is refused from 2030-02-01"},
		{"rsa2048 primary", withSigningSubkey(t, policyKey(t, "rsa2048p", cfgRSA2048), cfgEd25519), rsaDate,
			"primary key's algorithm (rsa2048) is refused from 2030-02-01"},
		{"sha1 self-signature", sha1Self, hashDate, "(type 0x13) hashed with SHA-1"},
		{"sha224 self-signature", sha224Self, hashDate, "(type 0x13) hashed with SHA-224"},
		{"sha1 binding", sha1Bind, hashDate, "(type 0x18) hashed with SHA-1"},
		{"sha1 back-signature", sha1Back, hashDate, "(type 0x19) hashed with SHA-1"},
		{"sha224 binding and back-signature", sha224Bind, hashDate, "(type 0x18) hashed with SHA-224"},
	} {
		if got, why := strongAt(t, c.e, c.date.Add(-time.Second)); !got {
			t.Errorf("%s, a second before %s: not strong (%s); want strong", c.name, c.date.Format(time.DateOnly), why)
		}
		if got, why := strongAt(t, c.e, c.date); got || !strings.Contains(why, c.why) {
			t.Errorf("%s, at %s: strong %v (%q); want not strong (%q)", c.name, c.date.Format(time.DateOnly), got, why, c.why)
		}
	}
}

// TestStrongSigner_ReadsGoCryptosSelfSignatureInUse: the binding chain is
// the self-signature go-crypto uses (Entity.PrimarySelfSignature: the
// primary identity's), not any other: a key whose primary user ID is bound
// with SHA-512 and a second user ID with SHA-1 alone is strong, and the
// same key whose primary user ID is the SHA-1 one is not.
func TestStrongSigner_ReadsGoCryptosSelfSignatureInUse(t *testing.T) {
	two := func(primarySHA1 bool) *openpgp.Entity {
		e := policyKey(t, "two-uids", cfgEd25519)
		cfg := &packet.Config{Time: func() time.Time { return policyMade.Add(time.Hour) }}
		if err := e.AddUserId("Policy second", "", "second@example.invalid", cfg); err != nil {
			t.Fatalf("AddUserId: %v", err)
		}
		for name, id := range e.Identities {
			sig := *id.SelfSignature
			isPrimary := strings.Contains(name, "second") == primarySHA1
			sig.IsPrimaryId = &isPrimary
			sig.Hash = crypto.SHA512
			if strings.Contains(name, "second") {
				sig.Hash = crypto.SHA1
			}
			if err := sig.SignUserId(id.UserId.Id, e.PrimaryKey, e.PrivateKey,
				&packet.Config{NonDeterministicSignaturesViaNotation: noSaltNotation}); err != nil {
				t.Fatal(err)
			}
			id.SelfSignature, id.Signatures = &sig, []*packet.Signature{&sig}
		}
		return e
	}
	if got, why := strongAt(t, two(false), policyNow); !got {
		t.Errorf("primary user ID bound with SHA-512, another with SHA-1: not strong (%s); want strong", why)
	}
	if got, why := strongAt(t, two(true), policyNow); got || !strings.Contains(why, "SHA-1") {
		t.Errorf("primary user ID bound with SHA-1: strong %v (%q); want not strong", got, why)
	}
}

// TestStrongSigner_AnUnmeasuredBindingHashIsNotStrong: a self-signature
// hashed with SHA3-256, which no row of strongHashes names, is not strong.
func TestStrongSigner_AnUnmeasuredBindingHashIsNotStrong(t *testing.T) {
	e := policyKey(t, "sha3self", cfgEd25519)
	resignSelf(t, e, crypto.SHA3_256)
	if got, why := strongAt(t, e, policyNow); got || !strings.Contains(why, "hashed with SHA3-256") {
		t.Fatalf("a SHA3-256 self-signature: strong %v (%q); want not strong", got, why)
	}
}

// TestStrongSigner_AMissingLinkIsNotStrong: a subkey whose binding has no
// back-signature (go-crypto would find the link missing) is not strong.
func TestStrongSigner_AMissingLinkIsNotStrong(t *testing.T) {
	e := withSigningSubkey(t, policyKey(t, "noback", cfgEd25519), cfgEd25519)
	k := signingKey(t, e)
	k.SelfSignature = &packet.Signature{Hash: crypto.SHA256, SigType: packet.SigTypeSubkeyBinding}
	if got, why := strongSigner(k, policyNow, policyNow); got || !strings.Contains(why, "lacks a signature") {
		t.Fatalf("no back-signature: strong %v (%q); want not strong", got, why)
	}
}

// TestKeyClass_RSABands: RSA's size bands (Sequoia's): under 2048 bits,
// 2048 to 3071, 3072 or more, at their edges.
func TestKeyClass_RSABands(t *testing.T) {
	for bits, want := range map[int]string{1024: "rsa1024", 2047: "rsa1024", 2048: "rsa2048", 3071: "rsa2048", 3072: "rsa3072", 4096: "rsa3072"} {
		n := new(big.Int).Lsh(big.NewInt(1), uint(bits-1))
		n.Add(n, big.NewInt(1))
		pk := packet.NewRSAPublicKey(policyMade, &rsa.PublicKey{N: n, E: 65537})
		if got := keyClass(pk); got != want {
			t.Errorf("RSA of %d bits: class %q, want %q", bits, got, want)
		}
	}
}

// TestStrongRules_EachRowNamesItsSourceAndAClass: every row of the table
// has a source, a class keyClass can return, and no class two rows; a weak
// signer's row has no date (a date only ends a strong verdict).
func TestStrongRules_EachRowNamesItsSourceAndAClass(t *testing.T) {
	seen := make(map[string]bool)
	classes := regexp.MustCompile(`^(ed25519|nistp(256|384|521)|rsa(1024|2048|3072)|dsa|brainpool|secp256k1)$`)
	for _, r := range strongRules {
		if r.source == "" || !classes.MatchString(r.class) || seen[r.class] {
			t.Errorf("row %+v: want a source, a class keyClass returns, once", r)
		}
		seen[r.class] = true
		if !r.signer && !r.primary && !r.until.IsZero() {
			t.Errorf("row %s: a date on a row that is never strong", r.class)
		}
	}
	for _, r := range strongHashes {
		if r.source == "" {
			t.Errorf("hash row %v: no source", r.hash)
		}
	}
}

// strengthVerifier is a verifier of ents (broad trust), checking at at.
func strengthVerifier(t *testing.T, at time.Time, ents ...*openpgp.Entity) *Verifier {
	t.Helper()
	v, err := NewVerifier(VerifierConfig{Keyring: newKeyring(t, ents...), RequireSignature: true, AllowShortKeyID: true, Logger: silentLogger()})
	if err != nil {
		t.Fatal(err)
	}
	v.now = func() time.Time { return at }
	return v
}

// binarySignature is e's binary-mode (0x00) signature packet of text,
// hashed with h and made at at: go-crypto's own signing takes no SHA-1.
func binarySignature(t *testing.T, e *openpgp.Entity, text []byte, h crypto.Hash, at time.Time) []byte {
	t.Helper()
	cfg := &packet.Config{NonDeterministicSignaturesViaNotation: noSaltNotation, Time: func() time.Time { return at }}
	id := e.PrivateKey.KeyId
	sig := &packet.Signature{Version: 4, SigType: packet.SigTypeBinary, PubKeyAlgo: e.PrivateKey.PubKeyAlgo,
		Hash: h, CreationTime: at, IssuerKeyId: &id}
	hh, err := sig.PrepareSign(cfg)
	if err != nil {
		t.Fatal(err)
	}
	hh.Write(text)
	if err := sig.Sign(hh, e.PrivateKey, cfg); err != nil {
		t.Fatalf("Sign with %v: %v", h, err)
	}
	var buf bytes.Buffer
	if err := sig.Serialize(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestVerifier_ReportsTheFirstStrongCountedSigner: S2 accepts at the first
// counted packet as before, and goes on counting until a strong one: a weak
// signer first and a strong one second report both, the strong one Strong;
// a strong one first reports it alone; a weak one alone reports no strong
// signer; and the same in a detached anchor, where a strong key's packet
// S2 does not count (SHA-1, ruling HD-18) makes no strong signer.
func TestVerifier_ReportsTheFirstStrongCountedSigner(t *testing.T) {
	strong := policyKey(t, "strong", cfgEd25519)
	weak := policyKey(t, "weak", cfgRSA1024)
	at := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	v := strengthVerifier(t, at, strong, weak)
	text := []byte(fakeReleasePlaintext)
	sign := func(e *openpgp.Entity, h crypto.Hash) []byte {
		return signaturePacketsOf(t, clearsignWithConfig(t, e, text, &packet.Config{DefaultHash: h, Time: func() time.Time { return at }}))
	}
	sfp, wfp := upperFP(strong.PrimaryKey.Fingerprint), upperFP(weak.PrimaryKey.Fingerprint)
	for _, c := range []struct {
		name    string
		packets [][]byte
		strong  string
		counted []string
	}{
		{"weak then strong", [][]byte{sign(weak, crypto.SHA256), sign(strong, crypto.SHA256)}, sfp, []string{wfp + " (not strong: its algorithm (rsa1024) is refused", sfp}},
		{"strong then weak", [][]byte{sign(strong, crypto.SHA256), sign(weak, crypto.SHA256)}, sfp, []string{sfp}},
		{"weak alone", [][]byte{sign(weak, crypto.SHA256)}, "", []string{wfp + " (not strong"}},
		{"detached: strong", [][]byte{nil, binarySignature(t, strong, text, crypto.SHA256, at)}, sfp, []string{sfp}},
		{"detached: weak then strong", [][]byte{nil, binarySignature(t, weak, text, crypto.SHA256, at),
			binarySignature(t, strong, text, crypto.SHA512, at)}, sfp, []string{wfp + " (not strong", sfp}},
		{"detached: weak then strong's SHA-1", [][]byte{nil, binarySignature(t, weak, text, crypto.SHA256, at),
			binarySignature(t, strong, text, crypto.SHA1, at)}, "", []string{wfp + " (not strong"}},
	} {
		var (
			got freshness.Signers
			err error
		)
		if c.packets[0] == nil { // detached: the packets after the nil
			_, got, err = v.VerifyDetachedSigners(context.Background(), newSuite(), text, armorSignature(t, joined(c.packets[1:]...)))
		} else {
			_, got, err = v.VerifyInlineSigners(context.Background(), newSuite(), inlineWithSignatures(t, c.packets...))
		}
		if err != nil {
			t.Errorf("%s: %v; want it verified", c.name, err)
			continue
		}
		ok := got.Strong == c.strong && len(got.Counted) == len(c.counted)
		for i := 0; ok && i < len(c.counted); i++ {
			ok = strings.HasPrefix(got.Counted[i], c.counted[i])
		}
		if !ok {
			t.Errorf("%s: %+v; want strong %q, counted %q", c.name, got, c.strong, c.counted)
		}
	}
}

// TestVerifier_NamesTheUncountedPackets (the final review's F2-I2): an
// anchor counted by its weak key alone names, in Signers.Uncounted, every
// other packet with why: a key the keyring lacks ("not in the trust set",
// a vendor's rotated key), a packet S2 does not count (a SHA-1 hash), and
// a packet gpgv does not check (another signature type after the first).
func TestVerifier_NamesTheUncountedPackets(t *testing.T) {
	strong := policyKey(t, "strong", cfgEd25519)
	weak := policyKey(t, "weak", cfgRSA1024)
	unknown := policyKey(t, "unknown", cfgEd25519)
	at := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	v := strengthVerifier(t, at, strong, weak)
	text := []byte(fakeReleasePlaintext)
	fp := func(e *openpgp.Entity) string { return "fpr:" + upperFP(e.PrimaryKey.Fingerprint) }
	det := armorSignature(t, joined(binarySignature(t, weak, text, crypto.SHA256, at),
		binarySignature(t, unknown, text, crypto.SHA256, at), binarySignature(t, strong, text, crypto.SHA1, at)))
	_, got, err := v.VerifyDetachedSigners(context.Background(), newSuite(), text, det)
	if err != nil || got.Strong != "" || len(got.Counted) != 1 || len(got.Uncounted) != 2 ||
		!strings.HasPrefix(got.Uncounted[0], fp(unknown)+" (not in the trust set") ||
		!strings.HasPrefix(got.Uncounted[1], fp(strong)+" (S2: packet 3: hash algorithm 2") {
		t.Fatalf("weak, unknown, strong's SHA-1: %+v, %v; want the unknown and the SHA-1 packets uncounted, with why", got, err)
	}
	textSig := signaturePacketsOf(t, clearsignWithConfig(t, unknown, text,
		&packet.Config{DefaultHash: crypto.SHA256, Time: func() time.Time { return at }}))
	det = armorSignature(t, joined(binarySignature(t, weak, text, crypto.SHA256, at), textSig))
	_, got, err = v.VerifyDetachedSigners(context.Background(), newSuite(), text, det)
	if err != nil || len(got.Uncounted) != 1 || !strings.HasPrefix(got.Uncounted[0], fp(unknown)+" (not checked") {
		t.Fatalf("weak binary, then a text-mode packet: %+v, %v; want the second packet named as not checked", got, err)
	}
	if _, got, err = v.VerifyDetachedSigners(context.Background(), newSuite(), text,
		armorSignature(t, joined(binarySignature(t, strong, text, crypto.SHA256, at), binarySignature(t, unknown, text, crypto.SHA256, at)))); err != nil ||
		got.Strong == "" || len(got.Uncounted) != 0 {
		t.Fatalf("strong first: %+v, %v; want strong, and nothing examined after it", got, err)
	}
}

// TestVerifier_CountsOnlyTheSuitesTrustedSigners: a strong key outside the
// suite's pin set signs too, but S2 counts the pinned weak key's packet
// alone, so the anchor has no strong signer.
func TestVerifier_CountsOnlyTheSuitesTrustedSigners(t *testing.T) {
	strong := policyKey(t, "strong", cfgEd25519)
	weak := policyKey(t, "weak", cfgRSA1024)
	at := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	v, err := NewVerifier(VerifierConfig{
		Keyring: newKeyring(t, strong, weak), RequireSignature: true, AllowShortKeyID: true, Logger: silentLogger(),
		Pins: []SignerPin{{HostRegex: regexp.MustCompile(`^archive\.example\.com$`),
			Fingerprints: map[string]struct{}{upperFP(weak.PrimaryKey.Fingerprint): {}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	v.now = func() time.Time { return at }
	cfg := &packet.Config{DefaultHash: crypto.SHA256, Time: func() time.Time { return at }}
	text := []byte(fakeReleasePlaintext)
	body := inlineWithSignatures(t, signaturePacketsOf(t, clearsignWithConfig(t, strong, text, cfg)),
		signaturePacketsOf(t, clearsignWithConfig(t, weak, text, cfg)))
	if _, got, err := v.VerifyInlineSigners(context.Background(), newSuite(), body); err != nil || got.Strong != "" || len(got.Counted) != 1 {
		t.Fatalf("pinned to the weak key: %+v, %v; want verified, one counted signer, none strong", got, err)
	}
}

// TestVerifier_TheBypassReportsNoSigner: under accept_any_signer's bypass
// (no pin matches) no packet is verified, so an anchor that has a strong
// signer under the default verifier reports none (ruling G1): the guard
// never fires there.
func TestVerifier_TheBypassReportsNoSigner(t *testing.T) {
	strong := policyKey(t, "strong", cfgEd25519)
	at := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	body := clearsignWithConfig(t, strong, []byte(fakeReleasePlaintext), &packet.Config{DefaultHash: crypto.SHA256, Time: func() time.Time { return at }})
	if _, got, err := strengthVerifier(t, at, strong).VerifyInlineSigners(context.Background(), newSuite(), body); err != nil || got.Strong == "" {
		t.Fatalf("test premise: %+v, %v; want a strong signer", got, err)
	}
	v, err := NewVerifier(VerifierConfig{Keyring: newKeyring(t, strong), AcceptAnySigner: true, AllowShortKeyID: true, Logger: silentLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if _, got, err := v.VerifyInlineSigners(context.Background(), newSuite(), body); err != nil || got.Strong != "" || len(got.Counted) != 0 {
		t.Fatalf("under the bypass: %+v, %v; want accepted with no signer", got, err)
	}
}

// vendorStrong pins the strong signer the verifier reports for each vendor
// fixture (vendorAnchors), at vendorCheckTime: the signing key's
// fingerprint, or "" with why none is. Measured for the plan's Task 9d:
// mongodb's 7.0 key is bound by a SHA-1 self-signature alone, and
// debian:13's sqv refuses its real InRelease ("Signing key ... is not
// bound"), so it has none, as openresty's (not a fixture) has none.
var vendorStrong = map[string]string{
	"debian-trixie/InRelease":      "4CB50190207B4758A3F73A796ED0E7B82643E131",
	"elastic/InRelease":            "46095ACC8548582C1A2699A9D27D666CD88E42B4",
	"ms-code/InRelease":            "BC528686B50D79E339D3721CEB3E94ADBE1229CF",
	"ms-code/Release.gpg":          "BC528686B50D79E339D3721CEB3E94ADBE1229CF",
	"salt/InRelease":               "10857FFDD3F91EAE577A21D664CBBC8173D76B3F",
	"signal/InRelease":             "DBA36B5181D0C816F630E889D980A17457F6FB06",
	"nginx/InRelease":              "8540A6F18833A80E9C1653A42FD21310B49F6B46",
	"yarn/InRelease":               "FF7CB5667B542092084BBDC562D54FD4003F6525",
	"sublime/InRelease":            "EBC733B78AAB352DC773BF857FE2FA12CF6E38F2",
	"google-cloud-sdk/Release.gpg": "35BAA0B33E9EB396F59CA838C0BA5CE6DC6315A3",
	"mongodb/InRelease":            "",
	"brave/Release.gpg":            "DBF1A116C220B8C7164F98230686B78420038257",
	"cloudflared/InRelease":        "CC94B39C77AE7342A68B89628A682D308D4E5E73",
}

// TestVerifier_PinsTheVendorAnchorsStrongSigners: every vendor fixture's
// strong signer is vendorStrong's, and every fixture is listed there.
func TestVerifier_PinsTheVendorAnchorsStrongSigners(t *testing.T) {
	listed := 0
	for _, va := range vendorAnchors {
		v := vendorVerifier(t, va.dir)
		read := func(f string) []byte {
			b, err := os.ReadFile(filepath.Join(vendorDir, va.dir, f))
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
		name := va.dir + "/InRelease"
		var (
			got freshness.Signers
			err error
		)
		if va.inline {
			_, got, err = v.VerifyInlineSigners(context.Background(), newSuite(), read("InRelease"))
		} else {
			name = va.dir + "/Release.gpg"
			_, got, err = v.VerifyDetachedSigners(context.Background(), newSuite(), read("Release"), read("Release.gpg"))
		}
		want, ok := vendorStrong[name]
		if !ok {
			t.Errorf("%s: not in vendorStrong", name)
			continue
		}
		listed++
		if err != nil || got.Strong != want {
			t.Errorf("%s: strong signer %q (counted %v, %v); want %q", name, got.Strong, got.Counted, err, want)
		}
	}
	if listed != len(vendorStrong) {
		t.Errorf("vendorStrong has %d entries, %d fixtures are listed", len(vendorStrong), listed)
	}
}

// TestVerifier_SublimesStrongSignatureStrippedHasNoStrongSigner: the
// verifier lens's V-RC2 on the real vendor. Sublime's InRelease carries
// two signatures, by its first block's SHA-1-bound subkey and by its third
// block's key; its strong signer is the second. With the second packet
// dropped (no key needed), the anchor still verifies — the five gpgv-based
// apt accept it, debian:13's sqv refuses it — and has no strong signer:
// what the adopter's downgrade guard refuses (signer_downgrade).
func TestVerifier_SublimesStrongSignatureStrippedHasNoStrongSigner(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(vendorDir, "sublime", "InRelease"))
	if err != nil {
		t.Fatal(err)
	}
	v := vendorVerifier(t, "sublime")
	_, pkts := inlineParts(t, body)
	if len(pkts) != 2 {
		t.Fatalf("test premise: %d packets, want two", len(pkts))
	}
	head := body[:bytes.Index(body, []byte(apttext.SigArmorBegin))]
	stripped := joined(head, armorSignature(t, pkts[0].raw))
	_, got, err := v.VerifyInlineSigners(context.Background(), newSuite(), stripped)
	if err != nil || got.Strong != "" || len(got.Counted) != 1 || !strings.Contains(got.Counted[0], "SHA-1") {
		t.Fatalf("sublime, its second signature dropped: %+v, %v; want verified, one counted signer, not strong (SHA-1)", got, err)
	}
}
