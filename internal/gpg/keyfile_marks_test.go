package gpg

// Tests of the key-file reader's marks for S2 (the fourth re-review's
// V-R4I1; the controller's ruling R2: integrity fails closed). A revocation
// or a newer binding the cache cannot read, which gpgv honours, must not
// make a key or subkey usable that every gpgv-based apt treats as revoked or
// expired. The rows are V's kr14 (final/V/rr4/meas/kr14, measured in the
// six images: REVKEYSIG or EXPKEYSIG in the five gpgv-based apt, accepted by
// debian:13's sqv, which reads none of these signatures either), generated
// here: P, a primary key, K its signing subkey, each extra signature made
// for real with P's (and K's) private half, as mkkr14.py made them.

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/binary"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	pgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	_ "golang.org/x/crypto/ripemd160" //nolint:staticcheck // registers crypto.RIPEMD160: a hash go-crypto does not read and gpgv does, which these tests sign with
)

// Hash algorithm ids (RFC 9580 §9.5) the crafted signatures use.
const (
	hashRIPEMD160 = 3
	hashSHA256    = 8
)

// The crafted signatures' creation time: after K's binding (2024-01-02).
var kr14Created = time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)

// subpacket frames one signature subpacket of typ with data (one-octet
// length, RFC 9580 §5.2.3.7).
func subpacket(t *testing.T, typ byte, data []byte) []byte {
	t.Helper()
	if len(data) >= 191 {
		t.Fatalf("test premise: a subpacket of %d octets", len(data))
	}
	return joined([]byte{byte(len(data) + 1), typ}, data)
}

// createdAt is the creation time subpacket (2) for when.
func createdAt(t *testing.T, when time.Time) []byte {
	t.Helper()
	return subpacket(t, 2, binary.BigEndian.AppendUint32(nil, uint32(when.Unix())))
}

// issuerFpr is the issuer fingerprint subpacket (33) of pk, a v4 key.
func issuerFpr(t *testing.T, pk *packet.PublicKey) []byte {
	t.Helper()
	return subpacket(t, 33, joined([]byte{4}, pk.Fingerprint))
}

// keyMaterial is what a signature over keys hashes (RFC 9580 §5.2.4): each
// key's packet body behind 0x99 and its two-octet length.
func keyMaterial(t *testing.T, keys ...*packet.PublicKey) []byte {
	t.Helper()
	var out []byte
	for _, pk := range keys {
		var buf bytes.Buffer
		if err := pk.Serialize(&buf); err != nil {
			t.Fatal(err)
		}
		pkts, err := packetsOf(buf.Bytes())
		if err != nil || len(pkts) != 1 {
			t.Fatalf("test premise: a key serializes to %d packets, %v", len(pkts), err)
		}
		out = append(out, 0x99)
		out = binary.BigEndian.AppendUint16(out, uint16(len(pkts[0].body)))
		out = append(out, pkts[0].body...)
	}
	return out
}

// uidMaterial is what a certification of uid hashes after the key: 0xb4,
// the user ID's four-octet length and the user ID.
func uidMaterial(uid string) []byte {
	return joined([]byte{0xb4}, binary.BigEndian.AppendUint32(nil, uint32(len(uid))), []byte(uid))
}

// rsaKeySig crafts a v4 signature packet of sigType by signer's primary RSA
// key over material (the key, keys or key and user ID it stands on), with
// the hashed subpacket area hashed (a creation time first) and hash
// (hashID its OpenPGP id), its unhashed area the issuer key ID (16), as
// gpg writes one: signatures go-crypto does not make (RIPEMD-160) or would
// not (a malformed subpacket), each verifiable by gpgv.
func rsaKeySig(t *testing.T, signer *openpgp.Entity, sigType byte, hash crypto.Hash, hashID byte, hashed, material []byte) []byte {
	t.Helper()
	return rsaKeySigUnhashed(t, signer, sigType, hash, hashID, hashed, subpacket(t, 16, binary.BigEndian.AppendUint64(nil, signer.PrimaryKey.KeyId)), material)
}

// rsaKeySigUnhashed is rsaKeySig with the unhashed subpacket area given
// (nil: none, so the signature names its issuer by whatever the hashed area
// holds, a fingerprint alone, say).
func rsaKeySigUnhashed(t *testing.T, signer *openpgp.Entity, sigType byte, hash crypto.Hash, hashID byte, hashed, unhashed, material []byte) []byte {
	t.Helper()
	priv, ok := signer.PrivateKey.PrivateKey.(*rsa.PrivateKey)
	if !ok {
		t.Fatalf("test premise: %T is no RSA key", signer.PrivateKey.PrivateKey)
	}
	head := []byte{4, sigType, byte(packet.PubKeyAlgoRSA), hashID}
	head = binary.BigEndian.AppendUint16(head, uint16(len(hashed)))
	head = append(head, hashed...)
	trailer := binary.BigEndian.AppendUint32([]byte{4, 0xff}, uint32(len(head)))
	h := hash.New()
	h.Write(material)
	h.Write(head)
	h.Write(trailer)
	digest := h.Sum(nil)
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, hash, digest)
	if err != nil {
		t.Fatalf("SignPKCS1v15 with %v: %v", hash, err)
	}
	body := joined(head, binary.BigEndian.AppendUint16(nil, uint16(len(unhashed))), unhashed, digest[:2])
	body = binary.BigEndian.AppendUint16(body, uint16(priv.N.BitLen()))
	body = append(body, sig...)
	return newPacket(tagSignature, body)
}

// rsaKeySigLegacy crafts a version 2 or 3 signature packet (RFC 4880
// §5.2.2; gpg reads both alike) of sigType by signer's primary RSA key over
// material, SHA-256, made at when: the type and time hashed after the
// material, the issuer's key ID in the clear. As V's mkkr17.py and
// mkkr20.py make them.
func rsaKeySigLegacy(t *testing.T, version byte, signer *openpgp.Entity, sigType byte, when time.Time, material []byte) []byte {
	t.Helper()
	priv, ok := signer.PrivateKey.PrivateKey.(*rsa.PrivateKey)
	if !ok {
		t.Fatalf("test premise: %T is no RSA key", signer.PrivateKey.PrivateKey)
	}
	hashed := binary.BigEndian.AppendUint32([]byte{sigType}, uint32(when.Unix()))
	h := crypto.SHA256.New()
	h.Write(material)
	h.Write(hashed)
	digest := h.Sum(nil)
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest)
	if err != nil {
		t.Fatal(err)
	}
	body := joined([]byte{version, 5}, hashed, binary.BigEndian.AppendUint64(nil, signer.PrimaryKey.KeyId),
		[]byte{byte(packet.PubKeyAlgoRSA), hashSHA256}, digest[:2])
	body = binary.BigEndian.AppendUint16(body, uint16(priv.N.BitLen()))
	return newPacket(tagSignature, append(body, sig...))
}

// rsaKeySigV3 is rsaKeySigLegacy for version 3, made at kr14Created.
func rsaKeySigV3(t *testing.T, signer *openpgp.Entity, sigType byte, material []byte) []byte {
	t.Helper()
	return rsaKeySigLegacy(t, 3, signer, sigType, kr14Created, material)
}

// serializedSig is sig as a packet.
func serializedSig(t *testing.T, sig *packet.Signature) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := sig.Serialize(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// kr14Keys are the test's P and K: P an RSA primary key (2024-01-01) with a
// user ID and an encryption subkey, K its signing subkey (2024-01-02), as
// openpgp.NewEntity and AddSigningSubkey make them; signingSubkey adds
// another such subkey, made later.
type kr14Keys struct {
	p *openpgp.Entity
	k *openpgp.Subkey
}

func newKR14Keys(t *testing.T) kr14Keys {
	t.Helper()
	p := newTestEntity(t, "Strict P", "p@example.invalid")
	k := signingSubkey(t, p, time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC))
	return kr14Keys{p: p, k: k}
}

func signingSubkey(t *testing.T, e *openpgp.Entity, when time.Time) *openpgp.Subkey {
	t.Helper()
	if err := e.AddSigningSubkey(&packet.Config{
		Algorithm: packet.PubKeyAlgoRSA,
		RSABits:   2048,
		Time:      func() time.Time { return when },
	}); err != nil {
		t.Fatalf("AddSigningSubkey: %v", err)
	}
	return &e.Subkeys[len(e.Subkeys)-1]
}

// ring is P's binary keyring (as gpg exports it) with extra packets: each
// of afterPrimary right after the primary key packet, each of atEnd after
// the last packet (K's binding, so standing on K).
func (kk kr14Keys) ring(t *testing.T, afterPrimary, atEnd [][]byte) []byte {
	t.Helper()
	tags, pkts := rawPacketList(t, binaryKey(t, kk.p))
	if tags[0] != 6 || tags[len(tags)-1] != 2 || tags[len(tags)-2] != 14 {
		t.Fatalf("test premise: P's packets have tags %v", tags)
	}
	out := append([]byte{}, pkts[0]...)
	out = append(out, joined(afterPrimary...)...)
	out = append(out, joined(pkts[1:]...)...)
	return append(out, joined(atEnd...)...)
}

// The kr14 signatures, each by P over P and K (or P alone), as mkkr14.py
// makes them: a revocation of K (0x28) or of P (0x20) with a reason
// subpacket, well formed (0x02, compromised) or empty; a newer binding of K
// (0x18) that expires 200 days after K was made (expired now), with an
// extra hashed subpacket or none.
func (kk kr14Keys) subrev(t *testing.T, hash crypto.Hash, hashID byte, reason []byte) []byte {
	t.Helper()
	return rsaKeySig(t, kk.p, byte(packet.SigTypeSubkeyRevocation), hash, hashID,
		joined(createdAt(t, kr14Created), issuerFpr(t, kk.p.PrimaryKey), subpacket(t, 29, reason)),
		keyMaterial(t, kk.p.PrimaryKey, kk.k.PublicKey))
}

func (kk kr14Keys) keyrev(t *testing.T, hash crypto.Hash, hashID byte, reason []byte) []byte {
	t.Helper()
	return rsaKeySig(t, kk.p, byte(packet.SigTypeKeyRevocation), hash, hashID,
		joined(createdAt(t, kr14Created), issuerFpr(t, kk.p.PrimaryKey), subpacket(t, 29, reason)),
		keyMaterial(t, kk.p.PrimaryKey))
}

func (kk kr14Keys) binding(t *testing.T, hash crypto.Hash, hashID byte, when time.Time, extra []byte) []byte {
	t.Helper()
	return rsaKeySig(t, kk.p, byte(packet.SigTypeSubkeyBinding), hash, hashID,
		joined(createdAt(t, when), issuerFpr(t, kk.p.PrimaryKey), subpacket(t, 27, []byte{byte(packet.KeyFlagSign)}),
			subpacket(t, 9, binary.BigEndian.AppendUint32(nil, 200*86400)), extra),
		keyMaterial(t, kk.p.PrimaryKey, kk.k.PublicKey))
}

// The controls, well formed, made by go-crypto: P's revocation of itself,
// of K, and a newer binding of K that expires 200 days after K was made
// (with K's back-signature, as a signing subkey's binding needs).
func (kk kr14Keys) goCryptoSig(t *testing.T, sigType packet.SignatureType) *packet.Signature {
	t.Helper()
	reason := packet.KeyCompromised
	return &packet.Signature{
		Version: 4, SigType: sigType, PubKeyAlgo: kk.p.PrimaryKey.PubKeyAlgo, Hash: crypto.SHA256,
		CreationTime: kr14Created, IssuerKeyId: &kk.p.PrimaryKey.KeyId, IssuerFingerprint: kk.p.PrimaryKey.Fingerprint,
		RevocationReason: &reason, RevocationReasonText: "compromised",
	}
}

func (kk kr14Keys) keyrevOK(t *testing.T) []byte {
	t.Helper()
	sig := kk.goCryptoSig(t, packet.SigTypeKeyRevocation)
	if err := sig.RevokeKey(kk.p.PrimaryKey, kk.p.PrivateKey, &packet.Config{Time: func() time.Time { return kr14Created }}); err != nil {
		t.Fatal(err)
	}
	return serializedSig(t, sig)
}

func (kk kr14Keys) subrevOK(t *testing.T) []byte {
	t.Helper()
	sig := kk.goCryptoSig(t, packet.SigTypeSubkeyRevocation)
	if err := sig.RevokeSubkey(kk.k.PublicKey, kk.p.PrivateKey, &packet.Config{Time: func() time.Time { return kr14Created }}); err != nil {
		t.Fatal(err)
	}
	return serializedSig(t, sig)
}

func (kk kr14Keys) bindingOK(t *testing.T) []byte {
	t.Helper()
	cfg := &packet.Config{Time: func() time.Time { return kr14Created }}
	back := &packet.Signature{Version: 4, SigType: packet.SigTypePrimaryKeyBinding, PubKeyAlgo: kk.k.PublicKey.PubKeyAlgo, Hash: crypto.SHA256, CreationTime: kr14Created}
	if err := back.CrossSignKey(kk.k.PublicKey, kk.p.PrimaryKey, kk.k.PrivateKey, cfg); err != nil {
		t.Fatal(err)
	}
	sig := kk.goCryptoSig(t, packet.SigTypeSubkeyBinding)
	sig.RevocationReason, sig.RevocationReasonText = nil, ""
	lifetime := uint32(200 * 86400)
	sig.FlagsValid, sig.FlagSign, sig.KeyLifetimeSecs, sig.EmbeddedSignature = true, true, &lifetime, back
	if err := sig.SignKey(kk.k.PublicKey, kk.p.PrivateKey, cfg); err != nil {
		t.Fatal(err)
	}
	return serializedSig(t, sig)
}

// kr14Rows are V's kr14 cases over kk: the keyring and what S2 must make of
// K's good signature (nil: adopted; a sentinel: refused with it), and
// whether the refusal is the reader's mark (marked) rather than go-crypto's
// own verdict on a signature it reads.
type kr14Row struct {
	name   string
	ring   []byte
	want   error
	marked bool
}

func kr14Rows(t *testing.T, kk kr14Keys) []kr14Row {
	t.Helper()
	reasonOK, reasonEmpty := []byte("\x02compromised"), []byte{}
	at := func(afterPrimary, atEnd []byte) []byte {
		var ap, ae [][]byte
		if afterPrimary != nil {
			ap = [][]byte{afterPrimary}
		}
		if atEnd != nil {
			ae = [][]byte{atEnd}
		}
		return kk.ring(t, ap, ae)
	}
	return []kr14Row{
		{"kr14_base: P and K as exported", at(nil, nil), nil, false},
		{"kr14_r1_subrev_ripemd: K revoked, RIPEMD-160", at(nil, kk.subrev(t, crypto.RIPEMD160, hashRIPEMD160, reasonOK)), pgperrors.ErrKeyRevoked, true},
		{"kr14_r2_subrev_badsub: K revoked, SHA-256, an empty reason subpacket", at(nil, kk.subrev(t, crypto.SHA256, hashSHA256, reasonEmpty)), pgperrors.ErrKeyRevoked, true},
		{"kr14_r3_keyrev_badsub: P revoked, SHA-256, an empty reason subpacket", at(kk.keyrev(t, crypto.SHA256, hashSHA256, reasonEmpty), nil), pgperrors.ErrKeyRevoked, true},
		{"kr14_r4_keyrev_ripemd: P revoked, RIPEMD-160", at(kk.keyrev(t, crypto.RIPEMD160, hashRIPEMD160, reasonOK), nil), pgperrors.ErrKeyRevoked, true},
		{"kr14_r5_keyrev_ok: P revoked, well formed (control)", at(kk.keyrevOK(t), nil), pgperrors.ErrKeyRevoked, false},
		{"kr14_r6_subrev_ok: K revoked, well formed (control)", at(nil, kk.subrevOK(t)), pgperrors.ErrKeyRevoked, false},
		{"kr14_b1_bind_ripemd: a newer binding of K, RIPEMD-160, expired", at(nil, kk.binding(t, crypto.RIPEMD160, hashRIPEMD160, kr14Created, nil)), pgperrors.ErrKeyExpired, true},
		{"kr14_b2_bind_badsub: the same, SHA-256, a one-octet trust subpacket", at(nil, kk.binding(t, crypto.SHA256, hashSHA256, kr14Created, subpacket(t, 5, []byte{1}))), pgperrors.ErrKeyExpired, true},
		{"kr14_b3_bind_ok: the same, well formed (control)", at(nil, kk.bindingOK(t)), pgperrors.ErrKeyExpired, false},
	}
}

// kr14Verifier loads files into a keyring directory and returns a verifier
// over it, with the short-keyid fallback on.
func kr14Verifier(t *testing.T, files map[string][]byte) *Verifier {
	t.Helper()
	dir := makeTestDir(t)
	for name, b := range files {
		writeFile(t, filepath.Join(dir, name), b)
	}
	keyring, err := LoadKeyring([]string{dir}, silentLogger())
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewVerifier(VerifierConfig{Keyring: keyring, RequireSignature: true, AllowShortKeyID: true, Logger: silentLogger()})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// signedBy clearsigns fakeReleasePlaintext (or text) with sub, now.
func signedBy(t *testing.T, sub *openpgp.Subkey, text string) []byte {
	t.Helper()
	return signaturePacketsOf(t, clearsignWithConfig(t, &openpgp.Entity{PrivateKey: sub.PrivateKey}, []byte(text),
		&packet.Config{DefaultHash: crypto.SHA256, Time: time.Now}))
}

// TestVerifier_UnreadableRevocationsAndBindingsAreNotCounted: V's kr14 rows
// (the fourth re-review's V-R4I1). With P's keyring beside A's, K's good
// signature alone is adopted only from the keyring as exported: a
// revocation of K or of P, or a newer expired binding of K, refuses it
// whether go-crypto reads the signature (the controls) or not (RIPEMD-160,
// which its packet reader skips without a word; a malformed subpacket,
// which the strict re-read dropped), with the mark's reason; and S1 still
// knows K: K's bad signature beside A's good one is refused as every
// gpgv-based apt refuses it (BADSIG).
func TestVerifier_UnreadableRevocationsAndBindingsAreNotCounted(t *testing.T) {
	kk := newKR14Keys(t)
	a := newTestEntity(t, "A", "a@example.com")
	kGood, kBad := signedBy(t, kk.k, fakeReleasePlaintext), signedBy(t, kk.k, "Origin: Other\n")
	aGood := signaturePacketsOf(t, clearsignWith(t, a, []byte(fakeReleasePlaintext)))
	for _, row := range kr14Rows(t, kk) {
		v := kr14Verifier(t, map[string][]byte{"00-a.gpg": binaryKey(t, a), "20-p.gpg": row.ring})
		_, err := v.VerifyInline(context.Background(), newSuite(), inlineWithSignatures(t, kGood))
		switch {
		case row.want == nil && err != nil:
			t.Errorf("%s: K's good signature: %v; want it adopted", row.name, err)
		case row.want != nil && !errors.Is(err, row.want):
			t.Errorf("%s: K's good signature: %v; want %v", row.name, err, row.want)
		case row.want != nil && row.marked != strings.Contains(err.Error(), "is not counted:"):
			t.Errorf("%s: K's good signature: %v; want the reader's mark named: %v", row.name, err, row.marked)
		}
		if _, err := v.VerifyInline(context.Background(), newSuite(), inlineWithSignatures(t, aGood, kBad)); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: A's good signature and K's bad one: %v; want ErrBadSignature (gpgv's BADSIG: S1 knows K)", row.name, err)
		}
	}
}

// TestVerifier_AMarkIsPerComponentAndMerged: the mark's grain and its
// reach. A mark on K is K's alone: P's other signing subkey K2 still counts
// (gpgv's verdict is per key: a key whose old subkey carries an unreadable
// revocation signs by its current one); a mark on P reaches K2 (REVKEYSIG
// for every subkey of a revoked key), also when K2 comes from a later copy
// of the key. A mark from any copy of a key holds, whichever copy is read
// first (as a revocation in any copy counts), and holds for a signature
// that names K by its short key ID alone.
func TestVerifier_AMarkIsPerComponentAndMerged(t *testing.T) {
	kk := newKR14Keys(t)
	reasonOK := []byte("\x02compromised")
	kRevoked := kk.subrev(t, crypto.RIPEMD160, hashRIPEMD160, reasonOK)
	pRevoked := kk.keyrev(t, crypto.RIPEMD160, hashRIPEMD160, reasonOK)
	before := kk.ring(t, nil, nil)
	kRevokedRing := kk.ring(t, nil, [][]byte{kRevoked}) // P and K, K revoked (the packet stands on K, the last subkey)
	pRevokedRing := kk.ring(t, [][]byte{pRevoked}, nil)
	kGood := signedBy(t, kk.k, fakeReleasePlaintext)
	k2 := signingSubkey(t, kk.p, time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)) // K2 is in no ring above
	k2Good := signedBy(t, k2, fakeReleasePlaintext)
	withK2 := binaryKey(t, kk.p) // P, K and K2
	for _, tc := range []struct {
		name   string
		files  map[string][]byte
		signed []byte
		want   error
	}{
		{"K revoked unreadably, K2 in the same file: K2 counts", map[string][]byte{"p.gpg": joined(kRevokedRing, withK2)}, k2Good, nil},
		{"K revoked unreadably, K2 in another file: K2 counts", map[string][]byte{"10-p.gpg": kRevokedRing, "20-p.gpg": withK2}, k2Good, nil},
		{"K revoked unreadably, K2 beside it: K does not count", map[string][]byte{"10-p.gpg": kRevokedRing, "20-p.gpg": withK2}, kGood, pgperrors.ErrKeyRevoked},
		{"P revoked unreadably: K2 does not count", map[string][]byte{"p.gpg": joined(pRevokedRing, withK2)}, k2Good, pgperrors.ErrKeyRevoked},
		{"P revoked unreadably in one file, K2 from another: K2 does not count", map[string][]byte{"10-p.gpg": pRevokedRing, "20-p.gpg": withK2}, k2Good, pgperrors.ErrKeyRevoked},
		{"P revoked unreadably in the later file: K2 does not count", map[string][]byte{"10-p.gpg": withK2, "20-p.gpg": pRevokedRing}, k2Good, pgperrors.ErrKeyRevoked},
		{"K revoked unreadably in the later file, a clean copy first: K does not count", map[string][]byte{"10-p.gpg": before, "20-p.gpg": kRevokedRing}, kGood, pgperrors.ErrKeyRevoked},
		{"the clean copy alone (control): K counts", map[string][]byte{"p.gpg": before}, kGood, nil},
	} {
		v := kr14Verifier(t, tc.files)
		_, err := v.VerifyInline(context.Background(), newSuite(), inlineWithSignatures(t, tc.signed))
		switch {
		case tc.want == nil && err != nil:
			t.Errorf("%s: %v; want it adopted", tc.name, err)
		case tc.want != nil && !errors.Is(err, tc.want):
			t.Errorf("%s: %v; want %v", tc.name, err, tc.want)
		}
	}
	// The short-keyid fallback: K's signature without an issuer fingerprint
	// (subpacket 33), as Docker's and Microsoft's are, on the revoked K.
	fpr := kk.k.PublicKey.Fingerprint
	kk.k.PublicKey.Fingerprint = nil
	kShort := signedBy(t, kk.k, fakeReleasePlaintext)
	kk.k.PublicKey.Fingerprint = fpr
	v := kr14Verifier(t, map[string][]byte{"p.gpg": kRevokedRing})
	if _, err := v.VerifyInline(context.Background(), newSuite(), inlineWithSignatures(t, kShort)); !errors.Is(err, pgperrors.ErrKeyRevoked) || !strings.Contains(err.Error(), "is not counted:") {
		t.Errorf("K's signature by short key ID, K revoked unreadably: %v; want the mark's refusal", err)
	}
	if _, err := kr14Verifier(t, map[string][]byte{"p.gpg": before}).VerifyInline(context.Background(), newSuite(), inlineWithSignatures(t, kShort)); err != nil {
		t.Errorf("K's signature by short key ID, the clean copy (control): %v; want it adopted", err)
	}
}

// TestVerifier_WhatGpgvIgnoresIsNotMarked: the mark's bounds. A signature
// the cache cannot read marks nothing where gpgv would not act on it: a
// binding of K older than the one kept (gpgv takes the newest), another
// key's certification of P's user ID (RIPEMD-160 certifications stand on
// real keys in debian-keyring). And what the cache could not verify, the
// key's own revocation that does not verify (corrupt), marks the component
// — go-crypto dropped such a key whole, so S1 did not know it; now S1
// refuses its bad signature beside a good one, and S2 does not count it, as
// before (fix round 3's "a key revocation go-crypto cannot verify still
// drops the key", pinned here for S2). A revocation by another key the key
// never designated is ignored, as every supported apt ignores it (the fifth
// re-review's kr17_tp_keyrev: ok in all six images; the designated case is
// TestVerifier_ThirdPartyRevocationsMarkOnlyWhenDesignated).
func TestVerifier_WhatGpgvIgnoresIsNotMarked(t *testing.T) {
	kk := newKR14Keys(t)
	x := newTestEntity(t, "X", "x@example.com")
	a := newTestEntity(t, "A", "a@example.com")
	kGood, kBad := signedBy(t, kk.k, fakeReleasePlaintext), signedBy(t, kk.k, "Origin: Other\n")
	aGood := signaturePacketsOf(t, clearsignWith(t, a, []byte(fakeReleasePlaintext)))
	reasonOK := []byte("\x02compromised")
	older := kk.binding(t, crypto.RIPEMD160, hashRIPEMD160, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), nil)
	uid := firstUID(kk.p)
	xCert := rsaKeySig(t, x, byte(packet.SigTypeGenericCert), crypto.RIPEMD160, hashRIPEMD160,
		joined(createdAt(t, kr14Created), issuerFpr(t, x.PrimaryKey)), joined(keyMaterial(t, kk.p.PrimaryKey), uidMaterial(uid)))
	tags, pkts := rawPacketList(t, binaryKey(t, kk.p))
	if tags[1] != 13 || tags[2] != 2 {
		t.Fatalf("test premise: P's packets have tags %v, want its user ID and self-signature second and third", tags)
	}
	withXCert := joined(joined(pkts[:3]...), xCert, joined(pkts[3:]...))
	xCertSHA := rsaKeySig(t, x, byte(packet.SigTypeGenericCert), crypto.SHA256, hashSHA256,
		joined(createdAt(t, kr14Created), issuerFpr(t, x.PrimaryKey)), joined(keyMaterial(t, kk.p.PrimaryKey), uidMaterial(uid)))
	xCertRev := rsaKeySig(t, x, byte(packet.SigTypeCertificationRevocation), crypto.SHA256, hashSHA256,
		joined(createdAt(t, kr14Created), issuerFpr(t, x.PrimaryKey), subpacket(t, 29, []byte("\x20no longer"))), joined(keyMaterial(t, kk.p.PrimaryKey), uidMaterial(uid)))
	withXCertRev := joined(joined(pkts[:3]...), xCertSHA, xCertRev, joined(pkts[3:]...))
	xCertRevRIPEMD := rsaKeySig(t, x, byte(packet.SigTypeCertificationRevocation), crypto.RIPEMD160, hashRIPEMD160,
		joined(createdAt(t, kr14Created), issuerFpr(t, x.PrimaryKey), subpacket(t, 29, []byte("\x20no longer"))), joined(keyMaterial(t, kk.p.PrimaryKey), uidMaterial(uid)))
	withXCertRevRIPEMD := joined(joined(pkts[:3]...), xCert, xCertRevRIPEMD, joined(pkts[3:]...))
	corrupt := kk.keyrevOK(t)
	corrupt[len(corrupt)-1] ^= 0x01 // the signature's last octet: it parses and does not verify
	byX := rsaKeySig(t, x, byte(packet.SigTypeKeyRevocation), crypto.SHA256, hashSHA256,
		joined(createdAt(t, kr14Created), issuerFpr(t, x.PrimaryKey), subpacket(t, 29, reasonOK)), keyMaterial(t, kk.p.PrimaryKey))
	corruptSub := kk.subrevOK(t)
	corruptSub[len(corruptSub)-1] ^= 0x01
	corruptMismatch := rsaKeySig(t, kk.p, byte(packet.SigTypeKeyRevocation), crypto.SHA256, hashSHA256,
		joined(createdAt(t, kr14Created), issuerFpr(t, x.PrimaryKey), subpacket(t, 29, reasonOK)), keyMaterial(t, kk.p.PrimaryKey))
	corruptMismatch[len(corruptMismatch)-1] ^= 0x01
	for _, tc := range []struct {
		name string
		ring []byte
		want error
	}{
		{"an older binding of K the cache cannot read: K counts", kk.ring(t, nil, [][]byte{older}), nil},
		{"another key's certification of P's user ID the cache cannot read: K counts", withXCert, nil},
		{"another key's certification of P's user ID, newer than P's own, and its revocation of it (0x30): K counts", withXCertRev, nil},
		{"the same certification and revocation (0x30) by another key, both unreadable: K counts", withXCertRevRIPEMD, nil},
		{"P's own revocation that does not verify: P is known, not counted", kk.ring(t, [][]byte{corrupt}, nil), pgperrors.ErrKeyRevoked},
		{"a revocation of P by another key P never designated: ignored, as every apt ignores it (kr17_tp_keyrev); K counts", kk.ring(t, [][]byte{byX}, nil), nil},
		{"P's own revocation of K that does not verify: K is known, not counted", kk.ring(t, nil, [][]byte{corruptSub}), pgperrors.ErrKeyRevoked},
		// The key's own unreadable revocation whose issuer fingerprint names
		// another key while its key ID names P (the sixth re-review's V-R6I3;
		// REVKEYSIG in gpgv 2.2): either naming the key fails closed.
		{"kr23_self_er_fprmis_keyrev: P's unreadable 0x20 naming X by fingerprint and P by key ID: not counted", kk.ring(t, [][]byte{rsaKeySig(t, kk.p, byte(packet.SigTypeKeyRevocation), crypto.RIPEMD160, hashRIPEMD160, joined(createdAt(t, kr14Created), issuerFpr(t, x.PrimaryKey), subpacket(t, 29, reasonOK)), keyMaterial(t, kk.p.PrimaryKey))}, nil), pgperrors.ErrKeyRevoked},
		{"kr23_self_er_fprmis_subrev: P's unreadable 0x28 of K, the same issuers: K not counted", kk.ring(t, nil, [][]byte{rsaKeySig(t, kk.p, byte(packet.SigTypeSubkeyRevocation), crypto.RIPEMD160, hashRIPEMD160, joined(createdAt(t, kr14Created), issuerFpr(t, x.PrimaryKey), subpacket(t, 29, reasonOK)), keyMaterial(t, kk.p.PrimaryKey, kk.k.PublicKey))}), pgperrors.ErrKeyRevoked},
		{"P's unreadable 0x20 naming P by fingerprint alone (no key ID): not counted", kk.ring(t, [][]byte{rsaKeySigUnhashed(t, kk.p, byte(packet.SigTypeKeyRevocation), crypto.RIPEMD160, hashRIPEMD160, joined(createdAt(t, kr14Created), issuerFpr(t, kk.p.PrimaryKey), subpacket(t, 29, reasonOK)), nil, keyMaterial(t, kk.p.PrimaryKey))}, nil), pgperrors.ErrKeyRevoked},
		{"P's readable 0x20 naming X by fingerprint and P by key ID that does not verify: the key's own by either issuer, not counted", kk.ring(t, [][]byte{corruptMismatch}, nil), pgperrors.ErrKeyRevoked},
	} {
		v := kr14Verifier(t, map[string][]byte{"00-a.gpg": binaryKey(t, a), "20-p.gpg": tc.ring})
		_, err := v.VerifyInline(context.Background(), newSuite(), inlineWithSignatures(t, kGood))
		switch {
		case tc.want == nil && err != nil:
			t.Errorf("%s: K's good signature: %v; want it adopted", tc.name, err)
		case tc.want != nil && (!errors.Is(err, tc.want) || !strings.Contains(err.Error(), "is not counted:")):
			t.Errorf("%s: K's good signature: %v; want %v, the reader's mark", tc.name, err, tc.want)
		}
		if _, err := v.VerifyInline(context.Background(), newSuite(), inlineWithSignatures(t, aGood, kBad)); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: A's good signature and K's bad one: %v; want ErrBadSignature (S1 knows K)", tc.name, err)
		}
	}
}

// TestParseKeyring_MarksWhatItCannotRead: the reader's marks themselves
// (keyFile.uncountable): a revocation of K marks K's fingerprint, one of P
// marks P's, each with the signature's type and why; the keyring as
// exported has none; and the entity is read whole either way.
func TestParseKeyring_MarksWhatItCannotRead(t *testing.T) {
	kk := newKR14Keys(t)
	fpP, fpK := upperFP(kk.p.PrimaryKey.Fingerprint), upperFP(kk.k.PublicKey.Fingerprint)
	for _, row := range kr14Rows(t, kk) {
		kf, err := parseKeyringBytes(row.ring)
		if err != nil || len(kf.entities) != 1 || len(kf.entities[0].Subkeys) != 2 {
			t.Errorf("%s: %v; want P read with its two subkeys", row.name, err)
			continue
		}
		var want string
		var typ string
		switch {
		case !row.marked:
		case strings.Contains(row.name, "subrev"):
			want, typ = fpK, "0x28"
		case strings.Contains(row.name, "keyrev"):
			want, typ = fpP, "0x20"
		default:
			want, typ = fpK, "0x18"
		}
		if want == "" {
			if len(kf.uncountable) != 0 {
				t.Errorf("%s: marks %v; want none", row.name, kf.uncountable)
			}
			continue
		}
		why := kf.uncountable[want]
		if len(kf.uncountable) != 1 || why == nil || !errors.Is(why, row.want) || !strings.Contains(why.Error(), typ) {
			t.Errorf("%s: marks %v; want %s alone marked %v, naming %s", row.name, kf.uncountable, want, row.want, typ)
		}
	}
}

// TestVerifier_ThirdPartyRevocationsMarkOnlyWhenDesignated: the fifth
// re-review's V-R5M1, the controller's ruling. A revocation of P (0x20) or
// of K (0x28) by another key, X, marks its component only when P designated
// X as a revoker: a revocation-key subpacket (12) naming X in one of P's
// own direct-key signatures (0x1f, as gpg's addrevoker writes it) or
// self-certifications, readable or not (an unreadable one fails closed; one
// whose subpackets cannot be walked at all makes any third party a
// revoker). Undesignated, the revocation is ignored, as every supported
// apt ignores it (kr17_tp_keyrev: ok in all six images); designated, the
// five gpgv-based apt honour it when they hold X's key (the round's kr19
// rows: REVKEYSIG with X's key in the keyring, ok without; sqv ok), so the
// cache's mark, made whether or not X's key is loaded, fails closed. A
// designation of another key, Y, is no designation of X. S1 knows K
// throughout.
func TestVerifier_ThirdPartyRevocationsMarkOnlyWhenDesignated(t *testing.T) {
	kk := newKR14Keys(t)
	x := newTestEntity(t, "X", "x@example.com")
	y := newTestEntity(t, "Y", "y@example.com")
	a := newTestEntity(t, "A", "a@example.com")
	kGood, kBad := signedBy(t, kk.k, fakeReleasePlaintext), signedBy(t, kk.k, "Origin: Other\n")
	aGood := signaturePacketsOf(t, clearsignWith(t, a, []byte(fakeReleasePlaintext)))
	pMaterial, pkMaterial := keyMaterial(t, kk.p.PrimaryKey), keyMaterial(t, kk.p.PrimaryKey, kk.k.PublicKey)
	uid := firstUID(kk.p)
	reason := subpacket(t, 29, []byte("\x02designated revocation"))
	xRevP := rsaKeySig(t, x, byte(packet.SigTypeKeyRevocation), crypto.SHA256, hashSHA256, joined(createdAt(t, kr14Created), issuerFpr(t, x.PrimaryKey), reason), pMaterial)
	xRevK := rsaKeySig(t, x, byte(packet.SigTypeSubkeyRevocation), crypto.SHA256, hashSHA256, joined(createdAt(t, kr14Created), issuerFpr(t, x.PrimaryKey), reason), pkMaterial)
	xRevPv3 := rsaKeySigV3(t, x, byte(packet.SigTypeKeyRevocation), pMaterial)
	designating := func(revoker *openpgp.Entity, hash crypto.Hash, hashID byte) []byte { // P's direct-key signature naming revoker
		return rsaKeySig(t, kk.p, byte(packet.SigTypeDirectSignature), hash, hashID,
			joined(createdAt(t, time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC)), issuerFpr(t, kk.p.PrimaryKey), subpacket(t, 12, joined([]byte{0x80, byte(packet.PubKeyAlgoRSA)}, revoker.PrimaryKey.Fingerprint))),
			pMaterial)
	}
	designatingCert := rsaKeySig(t, kk.p, byte(packet.SigTypePositiveCert), crypto.SHA256, hashSHA256, // a newer self-certification of P's user ID naming X
		joined(createdAt(t, time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC)), issuerFpr(t, kk.p.PrimaryKey), subpacket(t, 27, []byte{byte(packet.KeyFlagCertify | packet.KeyFlagSign)}), subpacket(t, 12, joined([]byte{0x80, byte(packet.PubKeyAlgoRSA)}, x.PrimaryKey.Fingerprint))),
		joined(pMaterial, uidMaterial(uid)))
	// A direct-key signature by P whose hashed area is no whole subpackets
	// (a subpacket length running past it): go-crypto refuses it, its issuer
	// cannot be read, and it may name any revoker.
	garbled := rsaKeySig(t, kk.p, byte(packet.SigTypeDirectSignature), crypto.SHA256, hashSHA256, []byte{0xbf, 0x02, 0x00}, pMaterial)
	tags, pkts := rawPacketList(t, binaryKey(t, kk.p))
	if tags[1] != 13 || tags[2] != 2 {
		t.Fatalf("test premise: P's packets have tags %v", tags)
	}
	withCert := func(cert []byte, afterPrimary ...[]byte) []byte {
		return joined(pkts[0], joined(afterPrimary...), pkts[1], pkts[2], cert, joined(pkts[3:]...))
	}
	for _, tc := range []struct {
		name string
		ring []byte
		want error // nil: K's good signature adopted
	}{
		{"X's 0x20 of P, X never designated (kr17_tp_keyrev): K counts", kk.ring(t, [][]byte{xRevP}, nil), nil},
		{"X's 0x28 of K, X never designated: K counts", kk.ring(t, nil, [][]byte{xRevK}), nil},
		{"kr21_tp_v3rev_xin: X's v3 0x20 of P, X never designated: refused (version 2 and 3 fail closed whoever the issuer; ok in all six apt: over-strict, R3)", kk.ring(t, [][]byte{xRevPv3}, nil), pgperrors.ErrKeyRevoked},
		{"kr21_dr_v2rev_xin: P designates X (0x1f), X's v2 0x20 of P: P marked", kk.ring(t, [][]byte{designating(x, crypto.SHA256, hashSHA256), rsaKeySigLegacy(t, 2, x, byte(packet.SigTypeKeyRevocation), kr14Created, pMaterial)}, nil), pgperrors.ErrKeyRevoked},
		{"P designates X (0x1f), X's 0x20 of P (kr19_dr_rev): P marked", kk.ring(t, [][]byte{designating(x, crypto.SHA256, hashSHA256), xRevP}, nil), pgperrors.ErrKeyRevoked},
		{"P designates X (0x1f), X's 0x28 of K: K marked", kk.ring(t, [][]byte{designating(x, crypto.SHA256, hashSHA256)}, [][]byte{xRevK}), pgperrors.ErrKeyRevoked},
		{"kr21_dr_v3rev_xin: P designates X (0x1f), X's v3 0x20 of P: P marked", kk.ring(t, [][]byte{designating(x, crypto.SHA256, hashSHA256), xRevPv3}, nil), pgperrors.ErrKeyRevoked},
		{"P designates X in a newer self-certification of its user ID, X's 0x20 of P: P marked", withCert(designatingCert, xRevP), pgperrors.ErrKeyRevoked},
		{"P designates X in a 0x1f the cache cannot read (RIPEMD-160), X's 0x20 of P: P marked", kk.ring(t, [][]byte{designating(x, crypto.RIPEMD160, hashRIPEMD160), xRevP}, nil), pgperrors.ErrKeyRevoked},
		{"a 0x1f by P whose subpackets cannot be walked, X's 0x20 of P: P marked (any third party may be designated)", kk.ring(t, [][]byte{garbled, xRevP}, nil), pgperrors.ErrKeyRevoked},
		{"P designates Y (0x1f), X's 0x20 of P: K counts", kk.ring(t, [][]byte{designating(y, crypto.SHA256, hashSHA256), xRevP}, nil), nil},
		{"P designates X (0x1f), no revocation (kr19_dr_desig): K counts", kk.ring(t, [][]byte{designating(x, crypto.SHA256, hashSHA256)}, nil), nil},
		// Issuer subpackets that disagree (the sixth re-review's V-R6I3:
		// gpgv 2.2 reads a revocation's issuer from its key ID, gpgv 2.4 from
		// its fingerprint; REVKEYSIG in apt 2.2.4, 2.4.14 and 2.6.1): a
		// revocation whose fingerprint (33) names Y while its key ID (16)
		// names the revoker, readable or not, and the key's own unreadable
		// revocation whose fingerprint names Y while its key ID names P.
		{"kr21_dr_fprmis_xin: P designates X, X's readable 0x20 naming Y by fingerprint and X by key ID: P marked", kk.ring(t, [][]byte{designating(x, crypto.SHA256, hashSHA256), rsaKeySig(t, x, byte(packet.SigTypeKeyRevocation), crypto.SHA256, hashSHA256, joined(createdAt(t, kr14Created), issuerFpr(t, y.PrimaryKey), reason), pMaterial)}, nil), pgperrors.ErrKeyRevoked},
		{"kr23_dr_er_fprmis_xin: the same, unreadable (RIPEMD-160): P marked", kk.ring(t, [][]byte{designating(x, crypto.SHA256, hashSHA256), rsaKeySig(t, x, byte(packet.SigTypeKeyRevocation), crypto.RIPEMD160, hashRIPEMD160, joined(createdAt(t, kr14Created), issuerFpr(t, y.PrimaryKey), reason), pMaterial)}, nil), pgperrors.ErrKeyRevoked},
		{"kr24_dr_er_fpronly_xin: P designates X, X's unreadable 0x20 naming X by fingerprint alone: P marked, the mark naming X's fingerprint", kk.ring(t, [][]byte{designating(x, crypto.SHA256, hashSHA256), rsaKeySigUnhashed(t, x, byte(packet.SigTypeKeyRevocation), crypto.RIPEMD160, hashRIPEMD160, joined(createdAt(t, kr14Created), issuerFpr(t, x.PrimaryKey), reason), nil, pMaterial)}, nil), pgperrors.ErrKeyRevoked},
	} {
		v := kr14Verifier(t, map[string][]byte{"00-a.gpg": binaryKey(t, a), "20-p.gpg": tc.ring})
		_, err := v.VerifyInline(context.Background(), newSuite(), inlineWithSignatures(t, kGood))
		switch {
		case tc.want == nil && err != nil:
			t.Errorf("%s: K's good signature: %v; want it adopted", tc.name, err)
		case tc.want != nil && (!errors.Is(err, tc.want) || !strings.Contains(err.Error(), "designated revoker") && !strings.Contains(err.Error(), "whoever its issuer")):
			t.Errorf("%s: K's good signature: %v; want %v, the reader's mark naming a designated revoker (or a version 2 or 3 signature, marked whoever its issuer)", tc.name, err, tc.want)
		case strings.Contains(tc.name, "fpronly") && (!strings.Contains(err.Error(), upperFP(x.PrimaryKey.Fingerprint)) || strings.Contains(err.Error(), ", ,")):
			t.Errorf("%s: %v; want the mark to name X by its fingerprint (the sixth re-review's V-R6M2)", tc.name, err)
		}
		if _, err := v.VerifyInline(context.Background(), newSuite(), inlineWithSignatures(t, aGood, kBad)); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: A's good signature and K's bad one: %v; want ErrBadSignature (S1 knows K)", tc.name, err)
		}
	}
}

// TestVerifier_V3RevocationsAreMarked: version 2 and 3 signatures fail
// closed (the fifth re-review's V-R5I1, the sixth's V-R6I1 and V-R6I2, the
// controller's design ruling for fix round 6: one broad rule, not a copy of
// gpgv). A version 3 or 2 revocation by the key itself, of the key (0x20)
// or of a subkey (0x28), is honoured by every supported apt
// (kr17_v3_keyrev, kr17_v3_subrev, kr20_v2_keyrev, kr20_v2_subrev:
// REVKEYSIG in the five gpgv-based apt); a newer v3 or v2 binding of the
// signing subkey makes them refuse its signatures (kr21_v3_bind_newer,
// kr21_v2_bind_newer: gpgv takes the newest binding, which carries no
// back-signature, ERRSIG "not cross-certified"). Fix round 4 dropped every
// such signature before the marks were made; round 5 marked v3
// revocations alone. Now every v2 or v3 binding, back-signature, direct-key
// signature or revocation of the key or a subkey marks the component it
// stands on, whoever its issuer and whatever its age (an older binding too:
// over-strict, Minor under R3, costing no real file, since none holds a v2
// or v3 signature); S1 still knows K. A v2 or v3 certification is still
// passed over (kr21_v3_cert_newer: ok in all six images), a v3 0x30 follows
// the self rule, and a subkey bound by a v3 binding alone is dropped (never
// counted), the rest of the key read.
func TestVerifier_V3RevocationsAreMarked(t *testing.T) {
	kk := newKR14Keys(t)
	x := newTestEntity(t, "X", "x@example.com")
	a := newTestEntity(t, "A", "a@example.com")
	kGood, kBad := signedBy(t, kk.k, fakeReleasePlaintext), signedBy(t, kk.k, "Origin: Other\n")
	aGood := signaturePacketsOf(t, clearsignWith(t, a, []byte(fakeReleasePlaintext)))
	pMaterial, pkMaterial := keyMaterial(t, kk.p.PrimaryKey), keyMaterial(t, kk.p.PrimaryKey, kk.k.PublicKey)
	uid := firstUID(kk.p)
	tags, pkts := rawPacketList(t, binaryKey(t, kk.p))
	if tags[1] != 13 || tags[2] != 2 || tags[len(tags)-2] != 14 {
		t.Fatalf("test premise: P's packets have tags %v", tags)
	}
	v3Binding := rsaKeySigV3(t, kk.p, byte(packet.SigTypeSubkeyBinding), pkMaterial)
	// K2, a subkey with a v3 binding alone, after K's packets.
	k2 := signingSubkey(t, newTestEntity(t, "Q", "q@example.com"), time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC))
	var k2Pkt bytes.Buffer
	if err := k2.PublicKey.Serialize(&k2Pkt); err != nil {
		t.Fatal(err)
	}
	k2Binding := rsaKeySigV3(t, kk.p, byte(packet.SigTypeSubkeyBinding), keyMaterial(t, kk.p.PrimaryKey, k2.PublicKey))
	for _, tc := range []struct {
		name string
		ring []byte
		want error // nil: K's good signature adopted
	}{
		{"kr17_v3_keyrev: P revoked by a v3 0x20 by P", kk.ring(t, [][]byte{rsaKeySigV3(t, kk.p, byte(packet.SigTypeKeyRevocation), pMaterial)}, nil), pgperrors.ErrKeyRevoked},
		{"kr17_v3_subrev: K revoked by a v3 0x28 by P", kk.ring(t, nil, [][]byte{rsaKeySigV3(t, kk.p, byte(packet.SigTypeSubkeyRevocation), pkMaterial)}), pgperrors.ErrKeyRevoked},
		{"kr20_v2_keyrev: P revoked by a v2 0x20 by P", kk.ring(t, [][]byte{rsaKeySigLegacy(t, 2, kk.p, byte(packet.SigTypeKeyRevocation), kr14Created, pMaterial)}, nil), pgperrors.ErrKeyRevoked},
		{"kr20_v2_subrev: K revoked by a v2 0x28 by P", kk.ring(t, nil, [][]byte{rsaKeySigLegacy(t, 2, kk.p, byte(packet.SigTypeSubkeyRevocation), kr14Created, pkMaterial)}), pgperrors.ErrKeyRevoked},
		{"kr21_v3_bind_newer: a newer v3 binding of K by P: gpgv takes it, ERRSIG; refused", kk.ring(t, nil, [][]byte{v3Binding}), pgperrors.ErrKeyExpired},
		{"kr21_v2_bind_newer: the same, version 2; refused", kk.ring(t, nil, [][]byte{rsaKeySigLegacy(t, 2, kk.p, byte(packet.SigTypeSubkeyBinding), kr14Created, pkMaterial)}), pgperrors.ErrKeyExpired},
		{"an older v3 binding of K by P: refused too (over-strict, R3)", kk.ring(t, nil, [][]byte{rsaKeySigLegacy(t, 3, kk.p, byte(packet.SigTypeSubkeyBinding), time.Date(2023, 12, 1, 0, 0, 0, 0, time.UTC), pkMaterial)}), pgperrors.ErrKeyExpired},
		{"a v3 back-signature (0x19) by K on K: refused", kk.ring(t, nil, [][]byte{rsaKeySigV3(t, &openpgp.Entity{PrimaryKey: kk.k.PublicKey, PrivateKey: kk.k.PrivateKey}, byte(packet.SigTypePrimaryKeyBinding), pkMaterial)}), pgperrors.ErrKeyExpired},
		{"a v3 direct-key signature (0x1f) on P: refused", kk.ring(t, [][]byte{rsaKeySigV3(t, kk.p, byte(packet.SigTypeDirectSignature), pMaterial)}, nil), pgperrors.ErrKeyExpired},
		{"another key's v3 certification of P's user ID (kr21_v3_cert_newer's kin): passed over, K counts", joined(joined(pkts[:3]...), rsaKeySigV3(t, x, byte(packet.SigTypeGenericCert), joined(pMaterial, uidMaterial(uid))), joined(pkts[3:]...)), nil},
		{"P's own newer v2 certification of its user ID: passed over, K counts", joined(joined(pkts[:3]...), rsaKeySigLegacy(t, 2, kk.p, byte(packet.SigTypePositiveCert), kr14Created, joined(pMaterial, uidMaterial(uid))), joined(pkts[3:]...)), nil},
		{"another key's v3 revocation of its certification (0x30) of P's user ID: not the key's own, not marked, K counts", joined(joined(pkts[:3]...), rsaKeySigV3(t, x, byte(packet.SigTypeCertificationRevocation), joined(pMaterial, uidMaterial(uid))), joined(pkts[3:]...)), nil},
		{"P's own v3 revocation of its user ID (0x30): marked, as an unreadable v4 one is", joined(joined(pkts[:3]...), rsaKeySigV3(t, kk.p, byte(packet.SigTypeCertificationRevocation), joined(pMaterial, uidMaterial(uid))), joined(pkts[3:]...)), pgperrors.ErrKeyRevoked},
		{"K2 bound by a v3 binding alone after K: K2 dropped, K counts", kk.ring(t, nil, [][]byte{k2Pkt.Bytes(), k2Binding}), nil},
	} {
		v := kr14Verifier(t, map[string][]byte{"00-a.gpg": binaryKey(t, a), "20-p.gpg": tc.ring})
		_, err := v.VerifyInline(context.Background(), newSuite(), inlineWithSignatures(t, kGood))
		switch {
		case tc.want == nil && err != nil:
			t.Errorf("%s: K's good signature: %v; want it adopted", tc.name, err)
		case tc.want != nil && (!errors.Is(err, tc.want) || !strings.Contains(err.Error(), "version ")):
			t.Errorf("%s: K's good signature: %v; want %v, the reader's mark naming the signature's version", tc.name, err, tc.want)
		}
		if _, err := v.VerifyInline(context.Background(), newSuite(), inlineWithSignatures(t, aGood, kBad)); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: A's good signature and K's bad one: %v; want ErrBadSignature (S1 knows K)", tc.name, err)
		}
		if strings.HasPrefix(tc.name, "K2") {
			kf, err := parseKeyringBytes(tc.ring)
			if err != nil || len(kf.entities) != 1 || len(kf.entities[0].Subkeys) != 2 {
				t.Errorf("%s: %v; want P read with K and its encryption subkey, K2 dropped", tc.name, err)
			}
		}
	}
}
