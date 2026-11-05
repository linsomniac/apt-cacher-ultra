package gpg

// Unit tests of the first-only rule's liveness check (lens V2's re-review,
// V2-R1-M1: requireKnownSignerEveryGpgvChecks, liveSigner, keyLive) on
// branches the corpus does not reach: a key that cannot sign and is
// revoked or expired, and a key the key reader marks.

import (
	"errors"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// TestKeyLive_RefusesARevokedOrExpiredKey: keyLive, for a key that cannot
// sign (an encryption subkey here), refuses it when it or its primary is
// revoked or expired at now, as go-crypto's checkMessageSignatureDetails
// refuses a signing key, and passes it otherwise.
func TestKeyLive_RefusesARevokedOrExpiredKey(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Hour)
	raw := detachSignWith(t, newTestEntity(t, "Packet", "packet@example.com"), []byte(fakeReleasePlaintext), false)
	for _, tc := range []struct {
		name string
		edit func(e *openpgp.Entity, k *openpgp.Key)
		live bool
	}{
		{"live", func(*openpgp.Entity, *openpgp.Key) {}, true},
		{"the subkey revoked", func(_ *openpgp.Entity, k *openpgp.Key) {
			k.Revocations = []*packet.Signature{{CreationTime: past}}
		}, false},
		{"the primary revoked", func(e *openpgp.Entity, _ *openpgp.Key) {
			e.Revocations = []*packet.Signature{{CreationTime: past}}
		}, false},
		{"the subkey expired", func(_ *openpgp.Entity, k *openpgp.Key) {
			sig := *k.SelfSignature
			secs := uint32(1)
			sig.KeyLifetimeSecs = &secs
			k.SelfSignature = &sig
		}, false},
		{"the primary expired", func(e *openpgp.Entity, _ *openpgp.Key) {
			self, _ := e.PrimarySelfSignature()
			secs := uint32(1)
			self.KeyLifetimeSecs = &secs
		}, false},
	} {
		e := newTestEntity(t, "Enc", "enc@example.com")
		if len(e.Subkeys) != 1 || e.Subkeys[0].Sig == nil || e.Subkeys[0].Sig.FlagSign {
			t.Fatalf("test premise: want one encryption subkey, got %d", len(e.Subkeys))
		}
		k := openpgp.Key{Entity: e, PublicKey: e.Subkeys[0].PublicKey, SelfSignature: e.Subkeys[0].Sig}
		tc.edit(e, &k)
		if got := keyLive(k, raw, now); got != tc.live {
			t.Errorf("%s: keyLive %v, want %v", tc.name, got, tc.live)
		}
	}
}

// TestLiveSigner_ReadsTheKeyReadersMarks: liveSigner passes a good packet
// by a live signing key, and refuses it when the key reader marks that key
// (Keyring.uncountableKey: a revocation or a binding the cache cannot read,
// which gpgv honours), as gpgv 1.4 would not report a GOODSIG for it.
func TestLiveSigner_ReadsTheKeyReadersMarks(t *testing.T) {
	e := newTestEntity(t, "Signer", "signer@example.com")
	v, err := NewVerifier(VerifierConfig{Keyring: newKeyring(t, e), RequireSignature: true, Logger: silentLogger()})
	if err != nil {
		t.Fatal(err)
	}
	signed := []byte(fakeReleasePlaintext)
	pkts, err := parseSigPackets(detachSignWith(t, e, signed, false))
	if err != nil {
		t.Fatal(err)
	}
	keys := v.keyring.EntityList().KeysById(pkts[0].issuer)
	if len(keys) != 1 {
		t.Fatalf("test premise: %d keys of the packet's issuer, want 1", len(keys))
	}
	now := time.Now()
	if !v.liveSigner(keys[0], signed, pkts[0], now) {
		t.Fatal("a good packet by a live signing key: not live, want live")
	}
	v.keyring.uncountable = map[string]keyMarks{
		upperFP(keys[0].PublicKey.Fingerprint): {{why: errors.New("a mark the test sets")}},
	}
	if v.liveSigner(keys[0], signed, pkts[0], now) {
		t.Error("a good packet by a marked key: live, want not live")
	}
}
