package gpg

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"regexp"
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
