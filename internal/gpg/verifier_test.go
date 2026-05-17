package gpg

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"

	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
)

// fakeReleasePlaintext is the body the verifier returns on success;
// content irrelevant beyond "is the same bytes the user signed."
const fakeReleasePlaintext = `Origin: Test
Suite: noble
SHA256:
 abc 12 main/Sources
`

func newKeyring(t *testing.T, ents ...*openpgp.Entity) *Keyring {
	t.Helper()
	dir := makeTestDir(t)
	for i, e := range ents {
		writeArmoredPubKey(t, filepath.Join(dir, "k"+string(rune('a'+i))+".asc"), e)
	}
	k, err := LoadKeyring([]string{dir}, silentLogger())
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	return k
}

func newSuite() freshness.SuiteRef {
	return freshness.SuiteRef{
		CanonicalScheme: "http",
		CanonicalHost:   "archive.example.com",
		SuitePath:       "/ubuntu/dists/noble",
	}
}

func TestVerifier_HappyPath_BroadTrust(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)

	v, err := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		Logger:           silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	body := clearsignWith(t, signer, []byte(fakeReleasePlaintext))
	plain, err := v.VerifyInline(context.Background(), newSuite(), body)
	if err != nil {
		t.Fatalf("VerifyInline: %v", err)
	}
	if !bytes.Equal(plain, []byte(fakeReleasePlaintext)) {
		t.Fatalf("plaintext mismatch:\ngot=%q\nwant=%q", plain, fakeReleasePlaintext)
	}
}

func TestVerifier_HappyPath_PinnedSubset(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	other := newTestEntity(t, "Other", "other@example.com")
	keyring := newKeyring(t, signer, other)

	pin := SignerPin{
		HostRegex: regexp.MustCompile(`^archive\.example\.com$`),
		Fingerprints: map[string]struct{}{
			upperFP(signer.PrimaryKey.Fingerprint): {},
		},
	}

	v, err := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		Pins:             []SignerPin{pin},
		RequireSignature: true,
		RequirePinned:    true,
		Logger:           silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	body := clearsignWith(t, signer, []byte(fakeReleasePlaintext))
	plain, err := v.VerifyInline(context.Background(), newSuite(), body)
	if err != nil {
		t.Fatalf("VerifyInline: %v", err)
	}
	if !bytes.Equal(plain, []byte(fakeReleasePlaintext)) {
		t.Fatalf("plaintext mismatch")
	}
}

func TestVerifier_KeyOutsideKeyring(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	other := newTestEntity(t, "Other", "other@example.com")
	// Keyring contains 'other' but not 'signer'.
	keyring := newKeyring(t, other)

	v, err := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		Logger:           silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	body := clearsignWith(t, signer, []byte(fakeReleasePlaintext))
	_, err = v.VerifyInline(context.Background(), newSuite(), body)
	if err == nil {
		t.Fatal("expected verification failure for untrusted signer")
	}
	if !errors.Is(err, ErrUntrustedSigner) {
		t.Fatalf("err type wrong: %v (want ErrUntrustedSigner)", err)
	}
}

func TestVerifier_PinnedNoMatch_RequireFailsClosed(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)

	pin := SignerPin{
		HostRegex: regexp.MustCompile(`^other\.example\.org$`),
		Fingerprints: map[string]struct{}{
			upperFP(signer.PrimaryKey.Fingerprint): {},
		},
	}

	v, err := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		Pins:             []SignerPin{pin},
		RequireSignature: true,
		RequirePinned:    true,
		Logger:           silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	body := clearsignWith(t, signer, []byte(fakeReleasePlaintext))
	_, err = v.VerifyInline(context.Background(), newSuite(), body)
	if !errors.Is(err, ErrUnpinnedSuite) {
		t.Fatalf("err type wrong: %v (want ErrUnpinnedSuite)", err)
	}
}

func TestVerifier_PinnedNoMatch_FailOpenFallback(t *testing.T) {
	// require_pinned_signer = false: when no [[trusted_signer]] block
	// matches, fall back to the entire host keyring.
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)

	pin := SignerPin{
		HostRegex: regexp.MustCompile(`^never-matches\.example$`),
		Fingerprints: map[string]struct{}{
			upperFP(signer.PrimaryKey.Fingerprint): {},
		},
	}

	v, err := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		Pins:             []SignerPin{pin},
		RequireSignature: true,
		RequirePinned:    false,
		Logger:           silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	body := clearsignWith(t, signer, []byte(fakeReleasePlaintext))
	plain, err := v.VerifyInline(context.Background(), newSuite(), body)
	if err != nil {
		t.Fatalf("VerifyInline: %v", err)
	}
	if !bytes.Equal(plain, []byte(fakeReleasePlaintext)) {
		t.Fatalf("plaintext mismatch")
	}
}

func TestVerifier_PinnedSubset_KeyInKeyringNotInPin(t *testing.T) {
	// Both keys are in the keyring, but the pin only authorizes 'good'.
	// Signature by 'evil' must be rejected even though 'evil' is in
	// the host keyring (this is the SPEC2 §7.6.5 attack scenario:
	// compromised PPA key signing a forged Ubuntu Release).
	good := newTestEntity(t, "Good", "good@example.com")
	evil := newTestEntity(t, "Evil", "evil@example.com")
	keyring := newKeyring(t, good, evil)

	pin := SignerPin{
		HostRegex: regexp.MustCompile(`^archive\.example\.com$`),
		Fingerprints: map[string]struct{}{
			upperFP(good.PrimaryKey.Fingerprint): {},
		},
	}

	v, err := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		Pins:             []SignerPin{pin},
		RequireSignature: true,
		RequirePinned:    true,
		Logger:           silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	body := clearsignWith(t, evil, []byte(fakeReleasePlaintext))
	_, err = v.VerifyInline(context.Background(), newSuite(), body)
	if err == nil {
		t.Fatal("expected rejection for in-keyring-but-not-in-pin key")
	}
	if !errors.Is(err, ErrUntrustedSigner) {
		t.Fatalf("err type wrong: %v (want ErrUntrustedSigner)", err)
	}
}

func TestVerifier_MissingSignature_RequireTrue(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		Logger:           silentLogger(),
	})

	// Plaintext that is not clearsigned at all.
	_, err := v.VerifyInline(context.Background(), newSuite(), []byte(fakeReleasePlaintext))
	if !errors.Is(err, ErrMissingSignature) {
		t.Fatalf("err type wrong: %v (want ErrMissingSignature)", err)
	}
}

func TestVerifier_MissingSignature_RequireFalse(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: false, // operator opted into unsigned mode
		Logger:           silentLogger(),
	})

	plain, err := v.VerifyInline(context.Background(), newSuite(), []byte(fakeReleasePlaintext))
	if err != nil {
		t.Fatalf("VerifyInline: %v", err)
	}
	if !bytes.Equal(plain, []byte(fakeReleasePlaintext)) {
		t.Fatalf("expected verbatim body return, got %q", plain)
	}
}

func TestVerifier_ExpiredKey(t *testing.T) {
	expired := newExpiredEntity(t, "Expired", "expired@example.com")
	keyring := newKeyring(t, expired)

	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		Logger:           silentLogger(),
	})

	body := clearsignWith(t, expired, []byte(fakeReleasePlaintext))
	_, err := v.VerifyInline(context.Background(), newSuite(), body)
	if err == nil {
		t.Fatal("expected expired-key signature to be rejected")
	}
	// We don't pin to a specific sentinel here — the openpgp library's
	// expired-key error path is internal. The important property is
	// that VerifyInline returns SOME error.
}

func TestVerifier_PinMatchedButNoKeyInKeyring(t *testing.T) {
	// Pin matches the host and lists fingerprints, but none of those
	// fingerprints are present in the host keyring. We should not
	// fall through to broad trust — fail closed.
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer) // signer is in keyring

	pin := SignerPin{
		HostRegex: regexp.MustCompile(`^archive\.example\.com$`),
		Fingerprints: map[string]struct{}{
			// A fingerprint that is NOT in the host keyring.
			"DEADBEEFDEADBEEFDEADBEEFDEADBEEFDEADBEEF": {},
		},
	}

	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		Pins:             []SignerPin{pin},
		RequireSignature: true,
		RequirePinned:    true,
		Logger:           silentLogger(),
	})

	body := clearsignWith(t, signer, []byte(fakeReleasePlaintext))
	_, err := v.VerifyInline(context.Background(), newSuite(), body)
	if err == nil {
		t.Fatal("expected failure when pin matches host but no key in keyring satisfies it")
	}
	// Could be either ErrNoUsableSignature (empty trust set) or
	// ErrUntrustedSigner (signer's fp not in pin's union). Both are
	// fail-closed; either is acceptable.
}

func TestVerifier_MultiplePinsUnion(t *testing.T) {
	// Multiple [[trusted_signer]] blocks with overlapping host regex
	// produce a UNION of fingerprints (SPEC2 §7.6.2).
	signerA := newTestEntity(t, "A", "a@example.com")
	signerB := newTestEntity(t, "B", "b@example.com")
	keyring := newKeyring(t, signerA, signerB)

	// Two pins, both match the host. Each names ONE fingerprint.
	// Either signer should verify.
	pinA := SignerPin{
		HostRegex: regexp.MustCompile(`^archive\.example\.com$`),
		Fingerprints: map[string]struct{}{
			upperFP(signerA.PrimaryKey.Fingerprint): {},
		},
	}
	pinB := SignerPin{
		HostRegex: regexp.MustCompile(`example\.com`),
		Fingerprints: map[string]struct{}{
			upperFP(signerB.PrimaryKey.Fingerprint): {},
		},
	}

	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		Pins:             []SignerPin{pinA, pinB},
		RequireSignature: true,
		RequirePinned:    true,
		Logger:           silentLogger(),
	})

	for _, signer := range []*openpgp.Entity{signerA, signerB} {
		body := clearsignWith(t, signer, []byte(fakeReleasePlaintext))
		if _, err := v.VerifyInline(context.Background(), newSuite(), body); err != nil {
			t.Fatalf("union-pin verification failed for %v: %v", signer.PrimaryKey.KeyId, err)
		}
	}
}

func TestVerifier_TamperedPlaintext(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		Logger:           silentLogger(),
	})

	body := clearsignWith(t, signer, []byte(fakeReleasePlaintext))
	// Tamper with the plaintext between BEGIN/END markers.
	tampered := bytes.Replace(body, []byte("Origin: Test"), []byte("Origin: Evil"), 1)
	if bytes.Equal(tampered, body) {
		t.Fatal("tamper expected to change body")
	}
	_, err := v.VerifyInline(context.Background(), newSuite(), tampered)
	if err == nil {
		t.Fatal("tampered plaintext must not verify")
	}
}

func TestNewVerifier_RequiresKeyring(t *testing.T) {
	_, err := NewVerifier(VerifierConfig{Keyring: nil})
	if err == nil {
		t.Fatal("expected error for nil Keyring")
	}
}

func TestNewVerifier_RejectsNilHostRegex(t *testing.T) {
	dir := makeTestDir(t)
	k, _ := LoadKeyring([]string{dir}, silentLogger())
	_, err := NewVerifier(VerifierConfig{
		Keyring: k,
		Pins:    []SignerPin{{HostRegex: nil, Fingerprints: nil}},
	})
	if err == nil {
		t.Fatal("expected error for nil HostRegex")
	}
}

func TestVerifier_RejectsPrefixGarbage(t *testing.T) {
	// SPEC2 §7.5 step 2 stores the original inRelease bytes; if we
	// silently accept prefix-garbage clearsigned blocks, those bytes
	// would land in the pool blob without ever being verified —
	// cache-pollution path.
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		Logger:           silentLogger(),
	})
	body := clearsignWith(t, signer, []byte(fakeReleasePlaintext))
	withPrefix := append([]byte("ATTACKER PREFIX BYTES\n"), body...)

	_, err := v.VerifyInline(context.Background(), newSuite(), withPrefix)
	if err == nil {
		t.Fatal("expected rejection for prefix-bearing clearsigned message")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("BEGIN marker")) {
		t.Fatalf("error doesn't mention BEGIN marker: %v", err)
	}
}

func TestVerifier_RejectsSuffixGarbage(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		Logger:           silentLogger(),
	})
	body := clearsignWith(t, signer, []byte(fakeReleasePlaintext))
	withSuffix := append(append([]byte{}, body...), []byte("\nATTACKER SUFFIX\n")...)

	_, err := v.VerifyInline(context.Background(), newSuite(), withSuffix)
	if err == nil {
		t.Fatal("expected rejection for suffix-bearing clearsigned message")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("END marker")) {
		t.Fatalf("error doesn't mention END marker: %v", err)
	}
}

func TestVerifier_AcceptsTrailingNewlines(t *testing.T) {
	// Whitespace-only trailing bytes are conventional (apt's own
	// InRelease frequently ends with a final newline after the END
	// marker). The strict guard must allow this.
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		Logger:           silentLogger(),
	})
	body := clearsignWith(t, signer, []byte(fakeReleasePlaintext))
	withWS := append(append([]byte{}, body...), []byte("\n\n  \r\n")...)
	if _, err := v.VerifyInline(context.Background(), newSuite(), withWS); err != nil {
		t.Fatalf("trailing whitespace should be accepted: %v", err)
	}
}

func TestVerifier_AllowsUnsignedBodyWithMarkerlessJunk(t *testing.T) {
	// require_signature=false + body without any clearsign marker:
	// return the body verbatim. The structural guard must not
	// inadvertently reject plaintext bodies just because they
	// happen to contain non-whitespace bytes.
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: false,
		Logger:           silentLogger(),
	})
	plain := []byte("Origin: Plain\nSuite: noble\nSHA256:\n abc 12 main/Sources\n")
	got, err := v.VerifyInline(context.Background(), newSuite(), plain)
	if err != nil {
		t.Fatalf("expected verbatim return for plain body: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("verbatim mismatch")
	}
}

func TestVerifier_MultiSig_StaleAndCurrent(t *testing.T) {
	// SPEC2 §7.6.3: per-packet verify-and-trust binding. Construct
	// a clearsigned block with TWO signature packets — both by the
	// same trusted key, but the FIRST is over different content
	// (so its hash mismatches when verified against the block's
	// cleartext). The second is the real one.
	//
	// This exercises the verifyAnyTrusted loop: a "stale" signature
	// fails crypto, the loop must move on to the "current" one.
	// Without per-packet iteration the verifier would fail at the
	// stale packet rather than discovering the current one.
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		Logger:           silentLogger(),
	})

	// Build the real block over fakeReleasePlaintext.
	realBlock := clearsignWith(t, signer, []byte(fakeReleasePlaintext))
	// Build a "stale" sig over different cleartext.
	staleBlock := clearsignWith(t, signer, []byte("Origin: STALE\n"))

	combined, err := buildMultiSigBlock(realBlock, staleBlock)
	if err != nil {
		t.Fatalf("buildMultiSigBlock: %v", err)
	}

	plain, err := v.VerifyInline(context.Background(), newSuite(), combined)
	if err != nil {
		t.Fatalf("multi-sig verify failed: %v", err)
	}
	if !bytes.Equal(plain, []byte(fakeReleasePlaintext)) {
		t.Fatalf("plaintext mismatch")
	}
}

func TestVerifier_MultiSig_AllStaleRejects(t *testing.T) {
	// Inverse of the above: every signature in the block is over
	// stale cleartext, so none verify against the block's actual
	// cleartext. The verifier must reject — the loop must NOT
	// accept on the basis of a packet's IssuerFingerprint alone.
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		Logger:           silentLogger(),
	})

	stale1 := clearsignWith(t, signer, []byte("Origin: ONE\n"))
	stale2 := clearsignWith(t, signer, []byte("Origin: TWO\n"))

	// Take stale1's cleartext but stale2's signature(s). The block's
	// cleartext is from stale1; signatures (the policy says trusted
	// fp) all verify only against stale2's cleartext, not stale1's.
	combined, err := substituteSignatures(stale1, stale2)
	if err != nil {
		t.Fatalf("substituteSignatures: %v", err)
	}

	if _, err := v.VerifyInline(context.Background(), newSuite(), combined); err == nil {
		t.Fatal("expected rejection when all signatures are over stale cleartext")
	}
}

// TestVerifier_ShortKeyID_AcceptedWithFallback covers the SPEC2 §7.6.3
// short-keyid fallback path: a clearsigned block whose signature(s)
// carry only the legacy 8-byte issuer keyid (no IssuerFingerprint
// subpacket) must verify when AllowShortKeyID is true, because the
// keyid resolves to a single trust-set entity that signs the cleartext.
func TestVerifier_ShortKeyID_AcceptedWithFallback(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		AllowShortKeyID:  true,
		Logger:           silentLogger(),
	})

	stripped := clearsignWithoutIssuerFingerprint(t, signer, []byte(fakeReleasePlaintext))
	plain, err := v.VerifyInline(context.Background(), newSuite(), stripped)
	if err != nil {
		t.Fatalf("short-keyid fallback rejected unexpectedly: %v", err)
	}
	if !bytes.Equal(plain, []byte(fakeReleasePlaintext)) {
		t.Fatalf("plaintext mismatch")
	}
}

// TestVerifier_ShortKeyID_RejectedWhenDisabled confirms the stricter
// posture: with AllowShortKeyID=false a short-keyid-only signature
// fails with ErrShortKeyID even when the keyid would map to a loaded
// key. This preserves the original SPEC2 §7.6.3 wording as an opt-in
// for operators who want it.
func TestVerifier_ShortKeyID_RejectedWhenDisabled(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		AllowShortKeyID:  false,
		Logger:           silentLogger(),
	})

	stripped := clearsignWithoutIssuerFingerprint(t, signer, []byte(fakeReleasePlaintext))
	if _, err := v.VerifyInline(context.Background(), newSuite(), stripped); !errors.Is(err, ErrShortKeyID) {
		t.Fatalf("err = %v, want ErrShortKeyID", err)
	}
}

// TestVerifier_ShortKeyID_AmbiguousKeyID covers the fallback's
// fail-closed property when two distinct entities in the trust set
// claim the same 8-byte keyid. In production this is astronomically
// rare for high-entropy keys, but the failure mode (an adversarial
// "Evil 32" colliding fingerprint, or an operator-staged duplicate)
// would let the wrong entity substitute for the intended signer —
// the whole point of the short-keyid fallback is to make a trust
// decision on the keyid alone, so any ambiguity must abort rather
// than guess.
func TestVerifier_ShortKeyID_AmbiguousKeyID(t *testing.T) {
	// Hand-build a Keyring with two distinct entities that share the
	// same primary-key KeyId. We bypass LoadKeyring (which would
	// dedupe by full fingerprint, not by short keyid) and stitch the
	// internal map ourselves. Both entities are real signing keys;
	// only the KeyId field is patched.
	signer := newTestEntity(t, "Signer", "signer@example.com")
	other := newTestEntity(t, "Collider", "collider@example.com")
	// Stamp the other key's KeyId to match signer's. Fingerprints
	// stay distinct (they're 20-byte SHA-1 of the key material) so
	// trust-set narrowing by fingerprint still treats them as
	// separate entries.
	other.PrimaryKey.KeyId = signer.PrimaryKey.KeyId

	keyring := &Keyring{
		entries: []KeyringEntry{
			{Entity: signer, PrimaryFingerprint: upperFP(signer.PrimaryKey.Fingerprint)},
			{Entity: other, PrimaryFingerprint: upperFP(other.PrimaryKey.Fingerprint)},
		},
		entities: openpgp.EntityList{signer, other},
		fingerprints: map[string]struct{}{
			upperFP(signer.PrimaryKey.Fingerprint): {},
			upperFP(other.PrimaryKey.Fingerprint):  {},
		},
	}
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		AllowShortKeyID:  true,
		Logger:           silentLogger(),
	})

	stripped := clearsignWithoutIssuerFingerprint(t, signer, []byte(fakeReleasePlaintext))
	_, err := v.VerifyInline(context.Background(), newSuite(), stripped)
	if err == nil {
		t.Fatal("expected rejection on ambiguous short keyid; got success")
	}
	if !errors.Is(err, ErrAmbiguousKeyID) {
		t.Fatalf("err = %v, want errors.Is ErrAmbiguousKeyID", err)
	}
}

// TestVerifier_ShortKeyID_KeyNotInTrustSet covers the fallback's
// safety property: even with AllowShortKeyID=true, a signature whose
// keyid maps to no entity in trustSet is rejected (anyUntrusted →
// ErrNoUsableSignature in the absence of any acceptable packet).
func TestVerifier_ShortKeyID_KeyNotInTrustSet(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	other := newTestEntity(t, "Other", "other@example.com")
	// Trust ONLY `other`, signature is from `signer`.
	keyring := newKeyring(t, other)
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		AllowShortKeyID:  true,
		Logger:           silentLogger(),
	})

	stripped := clearsignWithoutIssuerFingerprint(t, signer, []byte(fakeReleasePlaintext))
	if _, err := v.VerifyInline(context.Background(), newSuite(), stripped); err == nil {
		t.Fatalf("expected rejection when fallback keyid maps to no trust-set entity")
	}
}

// TestClassifyVerifyErr pins the SPEC5 §10.5 recent_adoptions[].reason
// short-tag mapping for every error sentinel the verifier can return.
// The set is closed (any new sentinel must extend this test and
// status.go reasonTooltip()); the default branch returns
// crypto_verify_failed for raw verify failures that bubble up via
// `fmt.Errorf("signature verification failed: %w", lastVerifyErr)`.
func TestClassifyVerifyErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil_returns_empty", nil, ""},
		{"unpinned_suite", ErrUnpinnedSuite, "unpinned_suite"},
		{"missing_signature", ErrMissingSignature, "missing_signature"},
		{"short_keyid", ErrShortKeyID, "short_keyid"},
		{"untrusted_signer", ErrUntrustedSigner, "untrusted_signer"},
		{"ambiguous_keyid", ErrAmbiguousKeyID, "ambiguous_keyid"},
		{"no_usable_signature", ErrNoUsableSignature, "no_usable_signature"},
		{
			"wrapped_untrusted_signer_walks_chain",
			errors.New("adoption_gpg_failed: " + ErrUntrustedSigner.Error()),
			"crypto_verify_failed",
		},
		{
			"wrapped_untrusted_signer_via_fmt_w",
			// What the verifier actually returns from verifyAnyTrusted.
			// errors.Is walks the chain so the sentinel is recovered.
			fmtErr(ErrUntrustedSigner),
			"untrusted_signer",
		},
		{
			"unknown_falls_through_to_crypto_verify_failed",
			errors.New("openpgp: hash mismatch"),
			"crypto_verify_failed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyVerifyErr(tc.err)
			if got != tc.want {
				t.Errorf("ClassifyVerifyErr(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// fmtErr wraps a sentinel the way verifyAnyTrusted does, so the test
// for "wrapped via %w" exercises the realistic shape.
func fmtErr(sentinel error) error {
	return errorsIsWrap{inner: sentinel}
}

type errorsIsWrap struct{ inner error }

func (e errorsIsWrap) Error() string { return "wrapped: " + e.inner.Error() }
func (e errorsIsWrap) Unwrap() error { return e.inner }

// TestVerifier_UntrustedSigner_LogsOncePerSuite asserts the
// adoption_untrusted_signer Info log fires exactly once for repeated
// rejections of the same (canonical_host, suite_path) within a
// process, but a different suite still gets its own log line. The
// log carries the rejected signer fingerprint(s) so operators can
// install the missing key without grepping the binary.
func TestVerifier_UntrustedSigner_LogsOncePerSuite(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	other := newTestEntity(t, "Other", "other@example.com")
	keyring := newKeyring(t, other)

	var logbuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logbuf, nil))
	v, err := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		Logger:           logger,
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	body := clearsignWith(t, signer, []byte(fakeReleasePlaintext))
	suiteA := freshness.SuiteRef{CanonicalHost: "packages.example.com", SuitePath: "/dists/stable"}
	suiteB := freshness.SuiteRef{CanonicalHost: "packages.example.com", SuitePath: "/dists/testing"}

	for i := 0; i < 3; i++ {
		if _, err := v.VerifyInline(context.Background(), suiteA, body); err == nil {
			t.Fatalf("call %d: expected ErrUntrustedSigner", i)
		}
	}
	if _, err := v.VerifyInline(context.Background(), suiteB, body); err == nil {
		t.Fatal("expected ErrUntrustedSigner on second suite")
	}

	got := logbuf.String()
	occA := strings.Count(got, `"suite_path":"/dists/stable"`)
	occB := strings.Count(got, `"suite_path":"/dists/testing"`)
	if occA != 1 {
		t.Errorf("expected exactly 1 untrusted_signer log for stable, got %d; full log: %s", occA, got)
	}
	if occB != 1 {
		t.Errorf("expected exactly 1 untrusted_signer log for testing, got %d; full log: %s", occB, got)
	}
	if !strings.Contains(got, `"adoption_untrusted_signer"`) {
		t.Errorf("log missing adoption_untrusted_signer message: %s", got)
	}
	wantFP := "fpr:" + upperFP(signer.PrimaryKey.Fingerprint)
	if !strings.Contains(got, wantFP) {
		t.Errorf("log missing rejected fingerprint %q in: %s", wantFP, got)
	}
}

// TestVerifier_AcceptAnySigner_NoPin_InlinePassThrough asserts that
// under accept_any_signer = true, a clearsigned InRelease signed by a
// key NOT present in the host keyring is adopted: the cleartext is
// returned and no trust / crypto check runs. This is the core
// relaxation the SPEC2 §7.6.2 bypass branch implements.
func TestVerifier_AcceptAnySigner_NoPin_InlinePassThrough(t *testing.T) {
	signer := newTestEntity(t, "Untrusted", "untrusted@example.com")
	other := newTestEntity(t, "Other", "other@example.com")
	// Keyring contains `other` but NOT `signer`.
	keyring := newKeyring(t, other)

	v, err := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		AcceptAnySigner:  true,
		Logger:           silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	body := clearsignWith(t, signer, []byte(fakeReleasePlaintext))
	plain, err := v.VerifyInline(context.Background(), newSuite(), body)
	if err != nil {
		t.Fatalf("VerifyInline: %v (want nil under accept_any_signer)", err)
	}
	if !bytes.Equal(plain, []byte(fakeReleasePlaintext)) {
		t.Fatalf("plaintext mismatch:\n got=%q\nwant=%q", plain, fakeReleasePlaintext)
	}
}

// TestVerifier_AcceptAnySigner_WithMatchingPin_StillEnforced asserts
// that a [[trusted_signer]] block matching the suite remains
// authoritative even when accept_any_signer is true. The relaxation
// only governs unpinned suites; explicit per-suite pins must still
// reject signers outside the pin's fingerprint list (otherwise the
// "respect the pin" interaction documented in the spec amendment is
// silently broken).
func TestVerifier_AcceptAnySigner_WithMatchingPin_StillEnforced(t *testing.T) {
	pinned := newTestEntity(t, "Pinned", "pinned@example.com")
	other := newTestEntity(t, "Other", "other@example.com")
	keyring := newKeyring(t, pinned, other)

	pin := SignerPin{
		HostRegex: regexp.MustCompile(`^archive\.example\.com$`),
		Fingerprints: map[string]struct{}{
			upperFP(pinned.PrimaryKey.Fingerprint): {},
		},
	}
	v, err := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		Pins:             []SignerPin{pin},
		RequireSignature: true,
		AcceptAnySigner:  true,
		Logger:           silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	// `other` is loaded in the host keyring but NOT in the pin; pinned
	// trust is the narrow set, so signatures from `other` must be
	// rejected as untrusted even with accept_any_signer on.
	body := clearsignWith(t, other, []byte(fakeReleasePlaintext))
	_, err = v.VerifyInline(context.Background(), newSuite(), body)
	if err == nil {
		t.Fatal("expected ErrUntrustedSigner for non-pinned signer under matching pin")
	}
	if !errors.Is(err, ErrUntrustedSigner) {
		t.Fatalf("err type wrong: %v (want ErrUntrustedSigner)", err)
	}

	// Sanity: the pinned signer still passes.
	bodyOK := clearsignWith(t, pinned, []byte(fakeReleasePlaintext))
	if _, err := v.VerifyInline(context.Background(), newSuite(), bodyOK); err != nil {
		t.Fatalf("pinned signer should still verify: %v", err)
	}
}

// TestVerifier_AcceptAnySigner_MissingClearsign_RequireSig_StillRejected
// asserts that the require_signature presence check is preserved under
// the bypass. accept_any_signer relaxes the trust/crypto step, not the
// "signature must be present" rule.
func TestVerifier_AcceptAnySigner_MissingClearsign_RequireSig_StillRejected(t *testing.T) {
	other := newTestEntity(t, "Other", "other@example.com")
	keyring := newKeyring(t, other)

	v, err := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		AcceptAnySigner:  true,
		Logger:           silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	// Plain Release-style bytes with no clearsign envelope.
	_, err = v.VerifyInline(context.Background(), newSuite(), []byte(fakeReleasePlaintext))
	if !errors.Is(err, ErrMissingSignature) {
		t.Fatalf("err = %v, want ErrMissingSignature", err)
	}
}

// TestVerifier_AcceptAnySigner_NotClearsigned_RequireSigFalse asserts
// the existing "unsigned-OK" passthrough is preserved: a body without
// a clearsign envelope is returned verbatim when require_signature is
// false, regardless of accept_any_signer.
func TestVerifier_AcceptAnySigner_NotClearsigned_RequireSigFalse(t *testing.T) {
	other := newTestEntity(t, "Other", "other@example.com")
	keyring := newKeyring(t, other)

	v, err := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: false,
		AcceptAnySigner:  true,
		Logger:           silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	body := []byte(fakeReleasePlaintext)
	plain, err := v.VerifyInline(context.Background(), newSuite(), body)
	if err != nil {
		t.Fatalf("VerifyInline: %v", err)
	}
	if !bytes.Equal(plain, body) {
		t.Fatalf("plaintext mismatch under require_signature=false passthrough")
	}
}

// TestVerifier_AcceptAnySigner_RequirePinned_StillFailsClosed asserts
// that require_pinned_signer wins over accept_any_signer for unpinned
// suites. Operators that turned on require_pinned_signer expect every
// suite to have an explicit pin; the accept_any_signer relaxation does
// not negate that requirement.
func TestVerifier_AcceptAnySigner_RequirePinned_StillFailsClosed(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)

	v, err := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		RequirePinned:    true,
		AcceptAnySigner:  true,
		Logger:           silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	body := clearsignWith(t, signer, []byte(fakeReleasePlaintext))
	_, err = v.VerifyInline(context.Background(), newSuite(), body)
	if err == nil {
		t.Fatal("expected ErrUnpinnedSuite under require_pinned_signer + no matching pin")
	}
	if !errors.Is(err, ErrUnpinnedSuite) {
		t.Fatalf("err type wrong: %v (want ErrUnpinnedSuite)", err)
	}
}

// TestVerifier_AcceptAnySigner_LogsOncePerSuite asserts the
// adoption_unverified_signer INFO log fires exactly once for repeated
// bypass acceptances of the same (canonical_host, suite_path), and a
// different suite produces its own line. The signer field carries the
// signer's long fingerprint when present.
func TestVerifier_AcceptAnySigner_LogsOncePerSuite(t *testing.T) {
	signer := newTestEntity(t, "Untrusted", "untrusted@example.com")
	other := newTestEntity(t, "Other", "other@example.com")
	keyring := newKeyring(t, other)

	var logbuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	v, err := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		AcceptAnySigner:  true,
		Logger:           logger,
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	body := clearsignWith(t, signer, []byte(fakeReleasePlaintext))
	suiteA := freshness.SuiteRef{CanonicalHost: "third.example.com", SuitePath: "/dists/stable"}
	suiteB := freshness.SuiteRef{CanonicalHost: "third.example.com", SuitePath: "/dists/testing"}

	for i := 0; i < 3; i++ {
		if _, err := v.VerifyInline(context.Background(), suiteA, body); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if _, err := v.VerifyInline(context.Background(), suiteB, body); err != nil {
		t.Fatalf("second suite: %v", err)
	}

	got := logbuf.String()
	occA := strings.Count(got, `"suite_path":"/dists/stable"`)
	occB := strings.Count(got, `"suite_path":"/dists/testing"`)
	if occA != 1 {
		t.Errorf("expected exactly 1 unverified_signer log for stable, got %d; full log: %s", occA, got)
	}
	if occB != 1 {
		t.Errorf("expected exactly 1 unverified_signer log for testing, got %d; full log: %s", occB, got)
	}
	if !strings.Contains(got, `"adoption_unverified_signer"`) {
		t.Errorf("log missing adoption_unverified_signer message: %s", got)
	}
	wantSigner := "fpr:" + upperFP(signer.PrimaryKey.Fingerprint)
	if !strings.Contains(got, wantSigner) {
		t.Errorf("log missing signer fingerprint %q in: %s", wantSigner, got)
	}
}

// TestVerifier_AcceptAnySigner_EmptyKeyring asserts the bypass branch
// works when the host keyring is empty — the deployment posture where
// the operator has deliberately chosen "delegate all trust to apt
// clients" and supplied no host keys.
func TestVerifier_AcceptAnySigner_EmptyKeyring(t *testing.T) {
	signer := newTestEntity(t, "Untrusted", "untrusted@example.com")
	empty, err := LoadKeyring(nil, silentLogger())
	if err != nil {
		t.Fatalf("LoadKeyring(nil): %v", err)
	}

	v, err := NewVerifier(VerifierConfig{
		Keyring:          empty,
		RequireSignature: true,
		AcceptAnySigner:  true,
		Logger:           silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	body := clearsignWith(t, signer, []byte(fakeReleasePlaintext))
	plain, err := v.VerifyInline(context.Background(), newSuite(), body)
	if err != nil {
		t.Fatalf("VerifyInline: %v", err)
	}
	if !bytes.Equal(plain, []byte(fakeReleasePlaintext)) {
		t.Fatalf("plaintext mismatch")
	}
}
