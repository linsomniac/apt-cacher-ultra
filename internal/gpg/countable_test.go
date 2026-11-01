package gpg

// Tests of S2's countability (verifier.go: notCounted): which trusted,
// verifying packets S2 counts, beyond their hash (ruling HD-18).

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math/bits"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// signedWith is e's v4 signature packet of message, made now with hash h:
// of type sigType, binary (over message's bytes) or text (over their
// canonical form, as clearsigning hashes the signed text), with subpacket
// 33 or without it. go-crypto's signing calls refuse SHA-1 (its HashById
// leaves it out); packet.Signature.Sign does not, for a v4 signature.
func signedWith(t *testing.T, e *openpgp.Entity, message []byte, sigType packet.SignatureType, h crypto.Hash, fingerprint bool) []byte {
	t.Helper()
	return signedAt(t, e, message, sigType, h, fingerprint, time.Now(), 0)
}

// signedAt is signedWith made at created, valid lifetime seconds from then
// (subpacket 3; 0: no expiry).
func signedAt(t *testing.T, e *openpgp.Entity, message []byte, sigType packet.SignatureType, h crypto.Hash, fingerprint bool, created time.Time, lifetime uint32) []byte {
	t.Helper()
	if !fingerprint {
		origFP := e.PrimaryKey.Fingerprint
		e.PrimaryKey.Fingerprint = nil
		defer func() { e.PrimaryKey.Fingerprint = origFP }()
	}
	sig := &packet.Signature{
		SigType:      sigType,
		PubKeyAlgo:   e.PrivateKey.PubKeyAlgo,
		Hash:         h,
		CreationTime: created,
		IssuerKeyId:  &e.PrivateKey.KeyId,
	}
	if lifetime > 0 {
		sig.SigLifetimeSecs = &lifetime
	}
	raw := h.New()
	w := raw
	if sigType == packet.SigTypeText {
		w = openpgp.NewCanonicalTextHash(raw)
	}
	if _, err := w.Write(message); err != nil {
		t.Fatalf("hash: %v", err)
	}
	// No salt notation: go-crypto adds one by default, and has no salt
	// size for SHA-1.
	noSalt := false
	if err := sig.Sign(raw, e.PrivateKey, &packet.Config{NonDeterministicSignaturesViaNotation: &noSalt}); err != nil {
		t.Fatalf("sign: %v", err)
	}
	var buf bytes.Buffer
	if err := sig.Serialize(&buf); err != nil {
		t.Fatalf("serialize: %v", err)
	}
	return buf.Bytes()
}

// inlineSHA1 is e's SHA-1 text-mode signature packet of
// fakeReleasePlaintext, the M of an InRelease inlineWithSignatures makes.
func inlineSHA1(t *testing.T, e *openpgp.Entity) []byte {
	t.Helper()
	return signedWith(t, e, []byte(fakeReleasePlaintext), packet.SigTypeText, crypto.SHA1, true)
}

// inlineSHA1WithoutFingerprint is inlineSHA1 without subpacket 33.
func inlineSHA1WithoutFingerprint(t *testing.T, e *openpgp.Entity) []byte {
	t.Helper()
	return signedWith(t, e, []byte(fakeReleasePlaintext), packet.SigTypeText, crypto.SHA1, false)
}

// binarySigned is e's binary-mode (0x00) signature packet of message, with
// hash h.
func binarySigned(t *testing.T, e *openpgp.Entity, message []byte, h crypto.Hash) []byte {
	t.Helper()
	return signedWith(t, e, message, packet.SigTypeBinary, h, true)
}

// TestSignedWith_MakesTheHashAsked is the helpers' premise: layer P reads
// each packet signedWith makes with the hash, type and issuer subpackets
// asked, and it verifies over the message.
func TestSignedWith_MakesTheHashAsked(t *testing.T) {
	a := newTestEntity(t, "A", "a@example.com")
	msg := []byte(fakeReleasePlaintext)
	for _, tc := range []struct {
		sigType packet.SignatureType
		h       crypto.Hash
		fpr     bool
		want    byte
	}{
		{packet.SigTypeText, crypto.SHA1, true, 2},
		{packet.SigTypeText, crypto.SHA1, false, 2},
		{packet.SigTypeBinary, crypto.SHA1, true, 2},
		{packet.SigTypeBinary, crypto.SHA256, true, 8},
	} {
		raw := signedWith(t, a, msg, tc.sigType, tc.h, tc.fpr)
		pkts, err := parseSigPackets(raw)
		if err != nil || len(pkts) != 1 {
			t.Fatalf("%v/%v: layer P: %v", tc.sigType, tc.h, err)
		}
		p := pkts[0]
		if p.hashAlgo != tc.want || p.sigType != byte(tc.sigType) || (p.fpr != nil) != tc.fpr || p.issuer != a.PrimaryKey.KeyId {
			t.Errorf("%v/%v: hash %d, type %d, fingerprint %v, issuer %X; want %d, %d, %v, %X",
				tc.sigType, tc.h, p.hashAlgo, p.sigType, p.fpr != nil, p.issuer, tc.want, tc.sigType, tc.fpr, a.PrimaryKey.KeyId)
		}
		if _, _, err := openpgp.VerifyDetachedSignature(openpgp.EntityList{a}, bytes.NewReader(msg), bytes.NewReader(raw), nil); err != nil {
			t.Errorf("%v/%v: go-crypto does not verify it: %v", tc.sigType, tc.h, err)
		}
	}
}

// TestAptUntrustedDigest_FollowsAptsTable pins aptUntrustedDigest to apt's
// Digests table (methods/gpgv.cc, the same in apt 2.2.4 to 3.2.0): ids 4 to
// 11 trusted, every other id untrusted (FindDigest falls back to "Invalid
// digest" past the table).
func TestAptUntrustedDigest_FollowsAptsTable(t *testing.T) {
	for h := 0; h < 256; h++ {
		want := h < 4 || h > 11
		if got := aptUntrustedDigest(byte(h)); got != want {
			t.Errorf("aptUntrustedDigest(%d) = %v, want %v", h, got, want)
		}
	}
}

// TestVerifyInline_ALaterSameKeySHA1SignatureErasesTheGoodOne: the
// re-review's A-RC2. apt's methods/gpgv.cc erases every good signature of a
// key it has seen so far when that key's VALIDSIG carries a digest it
// distrusts (SHA-1), and gpgv reports each packet's GOODSIG then its
// VALIDSIG, in packet order: so a SHA-1 packet AFTER the key's good one
// leaves apt no good signature (the five gpgv-based apt refuse
// e_good_then_sha1A), and one BEFORE it does not (x3i_sha1_plus_good,
// accepted by every apt). S2 does not count a packet a later packet gpgv
// checks erases; the refusal names the erasure.
func TestVerifyInline_ALaterSameKeySHA1SignatureErasesTheGoodOne(t *testing.T) {
	a := newTestEntity(t, "A", "a@example.com")
	b := newTestEntity(t, "B", "b@example.com")
	u := newTestEntity(t, "U", "u@example.com") // not in the keyring
	v, err := NewVerifier(VerifierConfig{Keyring: newKeyring(t, a, b), RequireSignature: true, AllowShortKeyID: true, Logger: silentLogger()})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	aGood := signaturePacketsOf(t, clearsignWith(t, a, []byte(fakeReleasePlaintext)))
	bGood := signaturePacketsOf(t, clearsignWith(t, b, []byte(fakeReleasePlaintext)))
	uGood := signaturePacketsOf(t, clearsignWith(t, u, []byte(fakeReleasePlaintext)))
	aSHA1, bSHA1 := inlineSHA1(t, a), inlineSHA1(t, b)
	aSHA1Binary := binarySigned(t, a, []byte(fakeReleasePlaintext), crypto.SHA1)
	aGoodNo33 := signaturePacketsOf(t, clearsignWithoutIssuerFingerprint(t, a, []byte(fakeReleasePlaintext)))
	aSHA1No33 := inlineSHA1WithoutFingerprint(t, a)
	// A's good SHA-256 signature that has expired since (made in 2025,
	// valid one second): gpgv's EXPSIG, with a VALIDSIG whose hash apt
	// trusts, so no erasure.
	aExpiredSig := signedAt(t, a, []byte(fakeReleasePlaintext), packet.SigTypeText, crypto.SHA256, true,
		time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), 1)
	// Premises: every SHA-1 packet verifies (S1 passes it): it is good, and
	// only its hash keeps S2 from counting it; the expired signature alone
	// is refused by S2's verification, not S1.
	if _, err := v.VerifyInline(context.Background(), newSuite(), inlineWithSignatures(t, aSHA1, aGood)); err != nil {
		t.Fatalf("test premise: A's SHA-1 packet before its good one: %v; want it adopted (x3i_sha1_plus_good)", err)
	}
	if _, err := v.VerifyInline(context.Background(), newSuite(), inlineWithSignatures(t, aExpiredSig)); err == nil || errors.Is(err, ErrBadSignature) {
		t.Fatalf("test premise: A's expired signature alone: %v; want S2's refusal", err)
	}
	for _, tc := range []struct {
		name    string
		packets [][]byte
		erased  bool // refused by the erasure; else adopted
	}{
		{"good, then SHA-1, same key", [][]byte{aGood, aSHA1}, true},
		{"SHA-1, then good, same key", [][]byte{aSHA1, aGood}, false},
		{"good, SHA-1, good", [][]byte{aGood, aSHA1, aGood}, false},
		{"SHA-1, good, SHA-1", [][]byte{aSHA1, aGood, aSHA1}, true},
		{"good, an unknown key's, SHA-1", [][]byte{aGood, uGood, aSHA1}, true},
		{"good, then another key's SHA-1", [][]byte{aGood, bSHA1}, false},
		{"good, SHA-1, another key's good", [][]byte{aGood, aSHA1, bGood}, false},
		{"good text-mode, then SHA-1 binary-mode: gpgv checks the first only", [][]byte{aGood, aSHA1Binary}, false},
		{"good, then SHA-1, both without subpacket 33", [][]byte{aGoodNo33, aSHA1No33}, true},
		{"good with 33, then SHA-1 without, same key", [][]byte{aGood, aSHA1No33}, true},
		{"good, then the same key's expired SHA-256 one: a trusted hash erases nothing", [][]byte{aGood, aExpiredSig}, false},
	} {
		got, err := v.VerifyInline(context.Background(), newSuite(), inlineWithSignatures(t, tc.packets...))
		switch {
		case tc.erased && (err == nil || !strings.Contains(err.Error(), "apt erases it") || ClassifyVerifyErr(err) != "crypto_verify_failed"):
			t.Errorf("%s: got %q, err %v (reason %q); want refused, crypto_verify_failed, naming the erasure", tc.name, got, err, ClassifyVerifyErr(err))
		case !tc.erased && err != nil:
			t.Errorf("%s: err %v; want it adopted", tc.name, err)
		}
	}
}

// TestVerifyDetached_ALaterSameKeySHA1SignatureErasesTheGoodOne: A-RC2's
// detached form (e_d_text_good_then_sha1A's binary twin): a Release.gpg
// holding A's good packet then A's SHA-1 one is refused; the other order is
// adopted.
func TestVerifyDetached_ALaterSameKeySHA1SignatureErasesTheGoodOne(t *testing.T) {
	a := newTestEntity(t, "A", "a@example.com")
	v, err := NewVerifier(VerifierConfig{Keyring: newKeyring(t, a), RequireSignature: true, Logger: silentLogger()})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	release := []byte(fakeReleasePlaintext)
	good := binarySigned(t, a, release, crypto.SHA256)
	sha1 := binarySigned(t, a, release, crypto.SHA1)
	if _, err := v.VerifyDetached(context.Background(), newSuite(), release, armorSignature(t, joined(sha1, good))); err != nil {
		t.Errorf("SHA-1 then good: %v; want it adopted", err)
	}
	_, err = v.VerifyDetached(context.Background(), newSuite(), release, armorSignature(t, joined(good, sha1)))
	if err == nil || !strings.Contains(err.Error(), "apt erases it") {
		t.Errorf("good then SHA-1: err %v; want refused, naming the erasure", err)
	}
}

// TestVerifyInline_ASHA1SignatureWithoutAFingerprintIsNeverCounted: ruling
// HD-18 in S2's short key ID branch (the re-review's V-RM2): A's SHA-1
// signature without subpacket 33, A trusted under allow_short_keyid, alone,
// is refused as never counted (every apt refuses a SHA-1-only anchor:
// x3i_sha1_only); A's SHA-256 one in the same shape is adopted.
func TestVerifyInline_ASHA1SignatureWithoutAFingerprintIsNeverCounted(t *testing.T) {
	a := newTestEntity(t, "A", "a@example.com")
	v, err := NewVerifier(VerifierConfig{Keyring: newKeyring(t, a), RequireSignature: true, AllowShortKeyID: true, Logger: silentLogger()})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	good := signaturePacketsOf(t, clearsignWithoutIssuerFingerprint(t, a, []byte(fakeReleasePlaintext)))
	if _, err := v.VerifyInline(context.Background(), newSuite(), inlineWithSignatures(t, good)); err != nil {
		t.Fatalf("test premise: A's SHA-256 signature without 33: %v; want it adopted", err)
	}
	_, err = v.VerifyInline(context.Background(), newSuite(), inlineWithSignatures(t, inlineSHA1WithoutFingerprint(t, a)))
	if err == nil || !strings.Contains(err.Error(), "never counted") || errors.Is(err, ErrBadSignature) {
		t.Fatalf("A's SHA-1 signature without 33: err %v; want S2's refusal, never counted", err)
	}
}

// signedWithHashed is e's v4 binary-mode SHA-256 signature packet of
// message (e's key RSA), made by hand so that its hashed area can hold any
// subpacket: a creation time, e's fingerprint (33), then extra (whole
// subpackets, tsub); its unhashed area e's key ID (16). go-crypto writes
// only the subpackets it knows.
func signedWithHashed(t *testing.T, e *openpgp.Entity, message, extra []byte) []byte {
	t.Helper()
	priv, ok := e.PrivateKey.PrivateKey.(*rsa.PrivateKey)
	if !ok {
		t.Fatalf("test premise: %T is no RSA key", e.PrivateKey.PrivateKey)
	}
	created := binary.BigEndian.AppendUint32(nil, uint32(time.Now().Unix()))
	hashed := joined(tsub(2, created...), tsub(33, append([]byte{4}, e.PrimaryKey.Fingerprint...)...), extra)
	unhashed := tsub(16, tkeyID(e.PrimaryKey.KeyId)...)
	head := binary.BigEndian.AppendUint16([]byte{4, 0x00, byte(packet.PubKeyAlgoRSA), 8}, uint16(len(hashed)))
	h := sha256.New()
	h.Write(message)
	h.Write(head)
	h.Write(hashed)
	h.Write(binary.BigEndian.AppendUint32([]byte{4, 0xff}, uint32(len(head)+len(hashed))))
	digest := h.Sum(nil)
	s, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	s = bytes.TrimLeft(s, "\x00")
	mpi := binary.BigEndian.AppendUint16(nil, uint16(8*(len(s)-1)+bits.Len8(s[0])))
	body := joined(head, hashed, binary.BigEndian.AppendUint16(nil, uint16(len(unhashed))), unhashed, digest[:2], mpi, s)
	return tpacket(body)
}

// TestVerifyInline_AGoodPacketSqvDoesNotReadIsNotCounted: the re-review's
// A-RC1. debian:13's sqv parses every subpacket of a signature packet, in
// both areas, and reads a packet holding one its parser refuses as an
// unknown packet, which it ignores: so a good packet with a malformed
// subpacket appended to its unhashed area (no key needed: the area is not
// signed) is refused by debian:13 when it is the only good one
// (u_good_unh_created_3), and accepted by every apt beside another good one
// (y_goodmalformed_plus_good); gpgv and go-crypto never read the unhashed
// subpackets but 16, 32 and 33. S2 does not count such a packet, and the
// refusal names it; a well-formed unhashed subpacket changes nothing
// (u_good_unh_created_ok). The hashed area's twin needs the key (a
// malformed subpacket the key holder signed): sqv refuses it alike, and so
// does S2.
func TestVerifyInline_AGoodPacketSqvDoesNotReadIsNotCounted(t *testing.T) {
	a := newTestEntity(t, "A", "a@example.com")
	v, err := NewVerifier(VerifierConfig{Keyring: newKeyring(t, a), RequireSignature: true, Logger: silentLogger()})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	good := signaturePacketsOf(t, clearsignWith(t, a, []byte(fakeReleasePlaintext)))
	malformed := withUnhashedSubpacket(t, good, tsub(2, 1, 2, 3))
	wellFormed := withUnhashedSubpacket(t, good, tsub(2, 0x65, 0, 0, 0))
	hashedBad := signedWithHashed(t, a, []byte(fakeReleasePlaintext), tsub(12, 0x80, 1, 2, 3, 4))
	hashedGood := signedWithHashed(t, a, []byte(fakeReleasePlaintext), tsub(12, append([]byte{0x80, 1}, bytes.Repeat([]byte{0x77}, 20)...)...))
	// A's good signature that has expired since: S2's verification refuses
	// it after the malformed one, whose reason must still be the one named.
	aExpiredSig := signedAt(t, a, []byte(fakeReleasePlaintext), packet.SigTypeText, crypto.SHA256, true,
		time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), 1)
	for _, tc := range []struct {
		name    string
		packets [][]byte
		refused bool
	}{
		{"the good packet with a 3-octet unhashed creation time, alone", [][]byte{malformed}, true},
		{"that packet, then A's expired signature", [][]byte{malformed, aExpiredSig}, true},
		{"that packet, then a good one", [][]byte{malformed, good}, false},
		{"a good packet, then that one", [][]byte{good, malformed}, false},
		{"a well-formed unhashed creation time", [][]byte{wellFormed}, false},
		{"a hashed revocation key of 5 octets, signed", [][]byte{hashedBad}, true},
		{"a hashed revocation key of 22 octets, signed", [][]byte{hashedGood}, false},
	} {
		got, err := v.VerifyInline(context.Background(), newSuite(), inlineWithSignatures(t, tc.packets...))
		switch {
		case tc.refused && (err == nil || !strings.Contains(err.Error(), "sqv does not read it") || ClassifyVerifyErr(err) != "crypto_verify_failed"):
			t.Errorf("%s: got %q, err %v (reason %q); want refused, crypto_verify_failed, naming sqv", tc.name, got, err, ClassifyVerifyErr(err))
		case !tc.refused && err != nil:
			t.Errorf("%s: err %v; want it adopted", tc.name, err)
		}
	}
	// Detached (u_d_good_unh_created_3).
	release := []byte(fakeReleasePlaintext)
	det := detachSignWith(t, a, release, false)
	if _, err := v.VerifyDetached(context.Background(), newSuite(), release, armorSignature(t, withUnhashedSubpacket(t, det, tsub(2, 1, 2, 3)))); err == nil || !strings.Contains(err.Error(), "sqv does not read it") {
		t.Errorf("detached, the good packet with a 3-octet unhashed creation time: err %v; want refused, naming sqv", err)
	}
	if _, err := v.VerifyDetached(context.Background(), newSuite(), release, armorSignature(t, det)); err != nil {
		t.Errorf("test premise: the detached good packet: %v; want it adopted", err)
	}
}
