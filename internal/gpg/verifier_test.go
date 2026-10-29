package gpg

import (
	"bytes"
	"context"
	"crypto"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	pgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

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

func TestVerifier_MissingSignature_RequireFalseStillRejected(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: false, // no longer relaxes inline verification (spec C5 rev 4)
		Logger:           silentLogger(),
	})

	plain, err := v.VerifyInline(context.Background(), newSuite(), []byte(fakeReleasePlaintext))
	if !errors.Is(err, ErrMissingSignature) {
		t.Fatalf("err = %v, want ErrMissingSignature", err)
	}
	if plain != nil {
		t.Fatalf("expected nil result on rejection, got %q", plain)
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
	if ruleOf(err) != "T1" { // apt: does not start with a signed message block
		t.Fatalf("err = %v (rule %q); want layer T's T1, the BEGIN marker's line", err, ruleOf(err))
	}
	if ClassifyVerifyErr(err) != "malformed_clearsign" { // deferred minor A-51
		t.Fatalf("reason = %q, want malformed_clearsign", ClassifyVerifyErr(err))
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
	if ruleOf(err) != "T4" { // apt: unsigned lines
		t.Fatalf("err = %v (rule %q); want layer T's T4, a line after the END line", err, ruleOf(err))
	}
	if ClassifyVerifyErr(err) != "malformed_clearsign" { // deferred minor A-51
		t.Fatalf("reason = %q, want malformed_clearsign", ClassifyVerifyErr(err))
	}
}

// TestVerifier_AcceptsOneFinalNewlineRefusesALineAfterEND (formerly
// TestVerifier_AcceptsTrailingNewlines, which accepted "\n\n  \r\n" after
// the END marker): an InRelease conventionally ends with one newline after
// its END line, LF or CRLF, and that is accepted; but every apt 2.2–3.2
// refuses any line after the END line, an empty or blank one included (the
// corpus' i_trail_lf, i_trail_ws, x3i_after_end_cr_line), so the verifier
// refuses it too (the 2026-09 security review's VC-2; F0 refused it until
// the hybrid verifier, layer T's T4 now: apt's "unsigned lines").
func TestVerifier_AcceptsOneFinalNewlineRefusesALineAfterEND(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		Logger:           silentLogger(),
	})
	body := clearsignWith(t, signer, []byte(fakeReleasePlaintext))
	if !bytes.HasSuffix(body, []byte("-----END PGP SIGNATURE-----")) {
		t.Fatalf("test premise: the clearsigned body does not end with the END marker: %q", body[len(body)-40:])
	}
	for _, tail := range []string{"", "\n", "\r\n"} {
		if _, err := v.VerifyInline(context.Background(), newSuite(), append(append([]byte{}, body...), tail...)); err != nil {
			t.Errorf("END line, then %q: %v; want accepted", tail, err)
		}
	}
	for _, tail := range []string{"\n\n", "\n\n  \r\n", "\n\r", "\n \n"} {
		_, err := v.VerifyInline(context.Background(), newSuite(), append(append([]byte{}, body...), tail...))
		if !errors.Is(err, ErrMalformedClearsign) || ruleOf(err) != "T4" {
			t.Errorf("END line, then %q: err = %v (rule %q); want ErrMalformedClearsign from T4, a line after the END line", tail, err, ruleOf(err))
		}
	}
}

func TestVerifier_MarkerlessBodyRejectedWhenSignatureOptional(t *testing.T) {
	// require_signature = false no longer passes plain bodies through;
	// spec C5 rev 4. A body without any clearsign marker is
	// ErrMissingSignature in every mode: the structural guard lets it
	// through to the decode step, which rejects it.
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: false,
		Logger:           silentLogger(),
	})
	plain := []byte("Origin: Plain\nSuite: noble\nSHA256:\n abc 12 main/Sources\n")
	got, err := v.VerifyInline(context.Background(), newSuite(), plain)
	if !errors.Is(err, ErrMissingSignature) {
		t.Fatalf("err = %v, want ErrMissingSignature", err)
	}
	if got != nil {
		t.Fatalf("expected nil result on rejection, got %q", got)
	}
}

func TestVerifier_MultiSig_StaleAndCurrent(t *testing.T) {
	// SPEC2 §7.6.3: per-packet verify-and-trust binding. Construct
	// a clearsigned block with TWO signature packets, both by trusted
	// keys: the FIRST is a stale key's — an archive key that has expired
	// since it signed (as the corpus' E), as during a key rollover — and
	// the second is the current one, the signer's.
	//
	// This exercises the verifyAnyTrusted loop: the stale packet fails
	// its check there (ErrKeyExpired), and the loop must move on to the
	// current one. Without per-packet iteration the verifier would fail
	// at the stale packet rather than discovering the current one. (The
	// stale packet used to be the signer's own signature of another
	// text; a bad signature from a known key now fails S1 before this
	// loop runs: TestVerifier_MultiSig_StaleAndCurrentRejects. E's good
	// signature passes S1, as gpgv's EXPKEYSIG passes apt.)
	signer := newTestEntity(t, "Signer", "signer@example.com")
	stale := newExpiredEntity(t, "Stale", "stale@example.com")
	keyring := newKeyring(t, signer, stale)
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		Logger:           silentLogger(),
	})

	// Build the real block over fakeReleasePlaintext.
	realBlock := clearsignWith(t, signer, []byte(fakeReleasePlaintext))
	// Build the stale key's signature of the same text, made while it
	// was valid (its first second).
	staleBlock := clearsignAt(t, stale, []byte(fakeReleasePlaintext), stale.PrimaryKey.CreationTime)
	// Premise: the stale packet alone fails verifyAnyTrusted's check.
	if _, err := v.VerifyInline(context.Background(), newSuite(), staleBlock); !errors.Is(err, pgperrors.ErrKeyExpired) {
		t.Fatalf("test premise: the stale key's signature alone: %v; want verifyAnyTrusted's ErrKeyExpired", err)
	}

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

// TestVerifier_MultiSig_StaleAndCurrentRejects: TestVerifier_MultiSig_
// StaleAndCurrent's former shape, a stale signature by the trusted key
// (over another text) before the current one, is refused: a bad signature
// from a known key beside a good one is fatal, as apt's BADSIG is (S1,
// ErrBadSignature; the corpus' i_badsig_badfirst).
func TestVerifier_MultiSig_StaleAndCurrentRejects(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          newKeyring(t, signer),
		RequireSignature: true,
		Logger:           silentLogger(),
	})
	realBlock := clearsignWith(t, signer, []byte(fakeReleasePlaintext))
	staleBlock := clearsignWith(t, signer, []byte("Origin: STALE\n"))
	combined, err := buildMultiSigBlock(realBlock, staleBlock)
	if err != nil {
		t.Fatalf("buildMultiSigBlock: %v", err)
	}
	got, err := v.VerifyInline(context.Background(), newSuite(), combined)
	if !errors.Is(err, ErrBadSignature) || ClassifyVerifyErr(err) != "bad_signature" || got != nil {
		t.Fatalf("got %q, err %v (reason %q); want ErrBadSignature (bad_signature)", got, err, ClassifyVerifyErr(err))
	}
}

func TestVerifier_MultiSig_AllStaleRejects(t *testing.T) {
	// Inverse of TestVerifier_MultiSig_StaleAndCurrent: every signature
	// in the block is over stale cleartext, so none verify against the
	// block's actual cleartext. The verifier must reject — never on the
	// basis of a packet's IssuerFingerprint alone. Since S1, a signature
	// from a known key that fails is refused before S2's loop runs
	// (ErrBadSignature, bad_signature).
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

	if _, err := v.VerifyInline(context.Background(), newSuite(), combined); !errors.Is(err, ErrBadSignature) || ClassifyVerifyErr(err) != "bad_signature" {
		t.Fatalf("err = %v (reason %q); want ErrBadSignature (bad_signature) when all signatures are over stale cleartext", err, ClassifyVerifyErr(err))
	}
}

// TestVerifyInline_ABadSignatureFromAKnownKeyRejects: S1 (DN-5,
// requireKnownSignaturesVerify), the 2026-09 security review's VC-1. An
// InRelease that carries, beside A's good signature, a signature that names
// A and does not verify — A's signature of another text — is refused in
// either order (ErrBadSignature, reason bad_signature), as gpgv's BADSIG
// makes every gpgv-based apt refuse it (the corpus' i_badsig_badfirst and
// i_badsig_goodfirst). S2 alone accepts both, on the good packet.
func TestVerifyInline_ABadSignatureFromAKnownKeyRejects(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	v, err := NewVerifier(VerifierConfig{Keyring: newKeyring(t, signer), RequireSignature: true, Logger: silentLogger()})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	good := signaturePacketsOf(t, clearsignWith(t, signer, []byte(fakeReleasePlaintext)))
	bad := signaturePacketsOf(t, clearsignWith(t, signer, []byte("Origin: Other\n")))
	for name, packets := range map[string][][]byte{
		"bad first":  {bad, good},
		"good first": {good, bad},
	} {
		got, err := v.VerifyInline(context.Background(), newSuite(), inlineWithSignatures(t, packets...))
		if !errors.Is(err, ErrBadSignature) || ClassifyVerifyErr(err) != "bad_signature" || got != nil {
			t.Errorf("%s: got %q, err %v (reason %q); want ErrBadSignature (bad_signature)", name, got, err, ClassifyVerifyErr(err))
		}
	}
}

// TestVerifyInline_AKnownKeyOutsideThePinsStillCounts: S1's "known" is the
// host keyring, not the suite's trust set (DN-5). A pin for the suite's host
// names A only; the keyring also holds C; an InRelease with A's good
// signature and C's bad one is refused (ErrBadSignature): a client that
// installed C, which the cache holds, sees gpgv's BADSIG from it. The pin's
// S2 alone accepts it (C's packet is outside the trust set; A's verifies).
func TestVerifyInline_AKnownKeyOutsideThePinsStillCounts(t *testing.T) {
	a := newTestEntity(t, "A", "a@example.com")
	c := newTestEntity(t, "C", "c@example.com")
	pin := SignerPin{
		HostRegex:    regexp.MustCompile(`^archive\.example\.com$`),
		Fingerprints: map[string]struct{}{upperFP(a.PrimaryKey.Fingerprint): {}},
	}
	v, err := NewVerifier(VerifierConfig{Keyring: newKeyring(t, a, c), Pins: []SignerPin{pin}, RequireSignature: true, Logger: silentLogger()})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	good := signaturePacketsOf(t, clearsignWith(t, a, []byte(fakeReleasePlaintext)))
	cGood := signaturePacketsOf(t, clearsignWith(t, c, []byte(fakeReleasePlaintext)))
	cBad := signaturePacketsOf(t, clearsignWith(t, c, []byte("Origin: Other\n")))
	// Premises: the pin trusts A for the suite and not C.
	if _, err := v.VerifyInline(context.Background(), newSuite(), inlineWithSignatures(t, good)); err != nil {
		t.Fatalf("test premise: A's good signature alone: %v; want it accepted under the pin", err)
	}
	if _, err := v.VerifyInline(context.Background(), newSuite(), inlineWithSignatures(t, cGood)); !errors.Is(err, ErrUntrustedSigner) {
		t.Fatalf("test premise: C's good signature alone: %v; want ErrUntrustedSigner (C is outside the pin)", err)
	}
	got, err := v.VerifyInline(context.Background(), newSuite(), inlineWithSignatures(t, good, cBad))
	if !errors.Is(err, ErrBadSignature) || ClassifyVerifyErr(err) != "bad_signature" || got != nil {
		t.Fatalf("A's good signature and C's bad one: got %q, err %v (reason %q); want ErrBadSignature (bad_signature): C is in the host keyring",
			got, err, ClassifyVerifyErr(err))
	}
}

// TestVerifyInline_AnExpiredKeysGoodSignatureBesideAGoodOneAdopts: a good
// signature from a known key that has expired since it signed passes S1 —
// go-crypto verifies it and reports ErrKeyExpired, gpgv's EXPKEYSIG, which
// apt ignores beside a good signature — so E's good signature beside A's is
// adopted in either order (the corpus' i_expired_good_plus_good and
// i_good_plus_expired_good). E's alone is still refused, by S2, not S1
// (i_expired_only).
func TestVerifyInline_AnExpiredKeysGoodSignatureBesideAGoodOneAdopts(t *testing.T) {
	a := newTestEntity(t, "A", "a@example.com")
	e := newExpiredEntity(t, "E", "e@example.com")
	v, err := NewVerifier(VerifierConfig{Keyring: newKeyring(t, a, e), RequireSignature: true, Logger: silentLogger()})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	// E signs at its creation, inside its one-second lifetime.
	eSigned := clearsignAt(t, e, []byte(fakeReleasePlaintext), e.PrimaryKey.CreationTime)
	eGood := signaturePacketsOf(t, eSigned)
	aGood := signaturePacketsOf(t, clearsignWith(t, a, []byte(fakeReleasePlaintext)))
	// Premise: go-crypto verifies E's signature over M and reports the
	// expiry.
	m, _ := inlineParts(t, eSigned)
	if got, _, verr := openpgp.VerifyDetachedSignature(openpgp.EntityList{e}, bytes.NewReader(m), bytes.NewReader(eGood), nil); got == nil || !errors.Is(verr, pgperrors.ErrKeyExpired) {
		t.Fatalf("test premise: go-crypto on E's signature: signature %v, err %v; want the signature and ErrKeyExpired", got != nil, verr)
	}
	for name, packets := range map[string][][]byte{
		"expired first":  {eGood, aGood},
		"expired second": {aGood, eGood},
	} {
		plain, err := v.VerifyInline(context.Background(), newSuite(), inlineWithSignatures(t, packets...))
		if err != nil || !bytes.Equal(plain, []byte(fakeReleasePlaintext)) {
			t.Errorf("%s: got %q, err %v; want the Release adopted", name, plain, err)
		}
	}
	_, err = v.VerifyInline(context.Background(), newSuite(), inlineWithSignatures(t, eGood))
	if !errors.Is(err, pgperrors.ErrKeyExpired) || errors.Is(err, ErrBadSignature) {
		t.Fatalf("E's signature alone: err %v (reason %q); want S2's refusal, ErrKeyExpired (crypto_verify_failed), not S1's",
			err, ClassifyVerifyErr(err))
	}
}

// TestVerifyInline_ACriticalNotationIsRefusedByP4: a signature that carries
// a critical notation is refused by layer P (P4: a critical subpacket 20,
// which gpgv 2.2's can_handle_critical takes only for notations it knows,
// and refuses as BADSIG otherwise), in either order beside a good one and
// on the bypass too (the corpus' xi_crit_plus_good and xi_crit_only). S1
// would refuse it next (go-crypto verifies it and reports a SignatureError:
// the premise below): before the hybrid verifier S1 was what refused it.
func TestVerifyInline_ACriticalNotationIsRefusedByP4(t *testing.T) {
	a := newTestEntity(t, "A", "a@example.com")
	critSigned := clearsignWithConfig(t, a, []byte(fakeReleasePlaintext), &packet.Config{
		DefaultHash: crypto.SHA256,
		Time:        time.Now,
		SignatureNotations: []*packet.Notation{
			{Name: "crit@example.invalid", Value: []byte("1"), IsCritical: true, IsHumanReadable: true},
		},
	})
	crit := signaturePacketsOf(t, critSigned)
	good := signaturePacketsOf(t, clearsignWith(t, a, []byte(fakeReleasePlaintext)))
	// Premise: go-crypto verifies the signature over M and reports the
	// notation.
	m, _ := inlineParts(t, inlineWithSignatures(t, good))
	if got, _, verr := openpgp.VerifyDetachedSignature(openpgp.EntityList{a}, bytes.NewReader(m), bytes.NewReader(crit), nil); got == nil || !errors.As(verr, new(pgperrors.SignatureError)) {
		t.Fatalf("test premise: go-crypto on the critical-notation signature: signature %v, err %v; want the signature and a SignatureError", got != nil, verr)
	}
	for _, anySigner := range []bool{false, true} {
		v, err := NewVerifier(VerifierConfig{Keyring: newKeyring(t, a), RequireSignature: true, AcceptAnySigner: anySigner, Logger: silentLogger()})
		if err != nil {
			t.Fatalf("NewVerifier: %v", err)
		}
		for name, packets := range map[string][][]byte{
			"critical alone":  {crit},
			"critical first":  {crit, good},
			"critical second": {good, crit},
		} {
			got, err := v.VerifyInline(context.Background(), newSuite(), inlineWithSignatures(t, packets...))
			if !errors.Is(err, ErrMalformedClearsign) || ruleOf(err) != "P4" || got != nil {
				t.Errorf("accept_any_signer=%v, %s: got %q, err %v (rule %q); want ErrMalformedClearsign from P4", anySigner, name, got, err, ruleOf(err))
			}
		}
	}
}

// TestRequireKnownSignaturesVerify_ASignatureErrorFails pins S1's other
// clause, which no corpus row reaches now that P4 refuses a critical
// notation on the packet: a known key's packet that verifies with a
// SignatureError fails S1 (DN-5, DN-11). go-crypto also reports one for an
// unknown critical notation on the key's own self-signature
// (checkMessageSignatureDetails), which is what this key carries; its data
// signature carries none, so layer P passes the packet. Beside A's good
// packet it is refused (ErrBadSignature), in either order; S2 alone would
// accept A's.
func TestRequireKnownSignaturesVerify_ASignatureErrorFails(t *testing.T) {
	a := newTestEntity(t, "A", "a@example.com")
	n, err := openpgp.NewEntity("N", "", "n@example.com", &packet.Config{
		Algorithm:   packet.PubKeyAlgoRSA,
		RSABits:     2048,
		DefaultHash: crypto.SHA256,
		SignatureNotations: []*packet.Notation{
			{Name: "crit@example.invalid", Value: []byte("1"), IsCritical: true, IsHumanReadable: true},
		},
	})
	if err != nil {
		t.Fatalf("NewEntity: %v", err)
	}
	v, err := NewVerifier(VerifierConfig{Keyring: newKeyring(t, a, n), RequireSignature: true, Logger: silentLogger()})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	nSig := signaturePacketsOf(t, clearsignWith(t, n, []byte(fakeReleasePlaintext)))
	aSig := signaturePacketsOf(t, clearsignWith(t, a, []byte(fakeReleasePlaintext)))
	// Premises: layer P passes N's packet; go-crypto verifies it with N's
	// key as the keyring loaded it and reports a SignatureError.
	m, pkts := inlineParts(t, inlineWithSignatures(t, nSig))
	keys := v.keyring.EntityList().KeysByIdUsage(pkts[0].issuer, packet.KeyFlagSign)
	if len(keys) != 1 {
		t.Fatalf("test premise: the keyring holds %d keys with N's key ID, want 1", len(keys))
	}
	if got, _, verr := openpgp.VerifyDetachedSignature(openpgp.EntityList{keys[0].Entity}, bytes.NewReader(m), bytes.NewReader(pkts[0].raw), nil); got == nil || !errors.As(verr, new(pgperrors.SignatureError)) {
		t.Fatalf("test premise: go-crypto on N's signature: signature %v, err %v; want the signature and a SignatureError", got != nil, verr)
	}
	for name, packets := range map[string][][]byte{
		"N first":  {nSig, aSig},
		"N second": {aSig, nSig},
	} {
		got, err := v.VerifyInline(context.Background(), newSuite(), inlineWithSignatures(t, packets...))
		if !errors.Is(err, ErrBadSignature) || ClassifyVerifyErr(err) != "bad_signature" || got != nil {
			t.Errorf("%s: got %q, err %v (reason %q); want ErrBadSignature (bad_signature)", name, got, err, ClassifyVerifyErr(err))
		}
	}
}

// TestVerifyInline_AnIssuerFingerprintNamingAnotherKeyIsRefusedByP4 pins
// P4's one-issuer rule on the shape that S1's contradiction rule (DN-5,
// rulings HD-3 and HD-14, deleted with s1Issuer) was written for. X's
// signing subkey signs with a hashed issuer fingerprint that names X's
// primary key (the signer controls the hashed area); go-crypto's hashed
// issuer key ID names the subkey. go-crypto takes its issuer from the last
// issuer subpacket it parses, gpgv from the hashed fingerprint (measured
// 2026-10-29 with the oracle's in-container.sh: every gpgv-based apt
// refuses such a shape as BADSIG, debian:13's sqv accepts it). As signed,
// the fingerprint's key ID is not the 16's; with an unhashed 16 naming the
// subkey added (no key needed), go-crypto even verifies the packet (the
// premise below). Layer P refuses both, as ErrMalformedClearsign, on the
// bypass too: a packet names one issuer, which every reader takes.
func TestVerifyInline_AnIssuerFingerprintNamingAnotherKeyIsRefusedByP4(t *testing.T) {
	x := newTestEntity(t, "X", "x@example.com")
	if err := x.AddSigningSubkey(&packet.Config{
		Algorithm: packet.PubKeyAlgoRSA,
		RSABits:   2048,
		Time:      func() time.Time { return time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC) },
	}); err != nil {
		t.Fatalf("AddSigningSubkey: %v", err)
	}
	sub := x.Subkeys[len(x.Subkeys)-1]
	// The subkey signs; its private key names X's primary key as the
	// issuer fingerprint while it signs (clearsign takes the issuer from
	// the signing key).
	fpr := sub.PublicKey.Fingerprint
	sub.PublicKey.Fingerprint = x.PrimaryKey.Fingerprint
	signed := clearsignWithConfig(t, &openpgp.Entity{PrivateKey: sub.PrivateKey}, []byte(fakeReleasePlaintext),
		&packet.Config{DefaultHash: crypto.SHA256, Time: time.Now})
	sub.PublicKey.Fingerprint = fpr
	asSigned := signaturePacketsOf(t, signed)
	unhashed16 := append([]byte{9, 16}, binary.BigEndian.AppendUint64(nil, sub.PublicKey.KeyId)...)
	withSecond16 := withUnhashedSubpacket(t, asSigned, unhashed16)

	// Premise: go-crypto verifies the packet with the unhashed 16 added
	// (it looks the key up by that 16, the last issuer subpacket).
	m, _ := inlineParts(t, clearsignWith(t, x, []byte(fakeReleasePlaintext)))
	if got, _, verr := openpgp.VerifyDetachedSignature(openpgp.EntityList{x}, bytes.NewReader(m), bytes.NewReader(withSecond16), nil); got == nil || verr != nil {
		t.Fatalf("test premise: go-crypto on the packet: signature %v, err %v; want it verified (by the unhashed key ID)", got != nil, verr)
	}
	for _, anySigner := range []bool{false, true} {
		v, err := NewVerifier(VerifierConfig{Keyring: newKeyring(t, x), RequireSignature: true, AcceptAnySigner: anySigner, Logger: silentLogger()})
		if err != nil {
			t.Fatalf("NewVerifier: %v", err)
		}
		for name, pkt := range map[string][]byte{
			"its fingerprint names X's primary key":    asSigned,
			"and a second issuer key ID, the subkey's": withSecond16,
		} {
			got, err := v.VerifyInline(context.Background(), newSuite(), inlineWithSignatures(t, pkt))
			if !errors.Is(err, ErrMalformedClearsign) || ruleOf(err) != "P4" || got != nil {
				t.Errorf("accept_any_signer=%v, %s: got %q, err %v (rule %q); want ErrMalformedClearsign from P4", anySigner, name, got, err, ruleOf(err))
			}
		}
	}
}

// TestRequireKnownSignaturesVerify_AKeyIDCollisionDoesNotFailAValidSignature:
// S1 verifies each packet against one keyring entity at a time (DN-5). Two
// entities share the signer's key ID, the colliding one loaded first, and
// the signer's valid signature passes. go-crypto's VerifyDetachedSignature
// over both hashes the message once and tries each candidate key with that
// one hash, which only the first can pass, so the whole keyring at once
// fails it (the premise below). The test calls requireKnownSignaturesVerify
// directly: VerifyInline's S2 (verifyAnyTrusted, unchanged) keeps that
// quirk and refuses this anchor (crypto_verify_failed; ruling HD-10,
// recorded Open). The collision is made as TestVerifier_ShortKeyID_
// AmbiguousKeyID makes it: the collider's KeyId patched, the keyring built
// by hand (a load would recompute the key ID).
func TestRequireKnownSignaturesVerify_AKeyIDCollisionDoesNotFailAValidSignature(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	collider := newTestEntity(t, "Collider", "collider@example.com")
	collider.PrimaryKey.KeyId = signer.PrimaryKey.KeyId
	keyring := &Keyring{
		entries: []KeyringEntry{
			{Entity: collider, PrimaryFingerprint: upperFP(collider.PrimaryKey.Fingerprint)},
			{Entity: signer, PrimaryFingerprint: upperFP(signer.PrimaryKey.Fingerprint)},
		},
		entities: openpgp.EntityList{collider, signer},
		fingerprints: map[string]struct{}{
			upperFP(collider.PrimaryKey.Fingerprint): {},
			upperFP(signer.PrimaryKey.Fingerprint):   {},
		},
	}
	v, err := NewVerifier(VerifierConfig{Keyring: keyring, RequireSignature: true, Logger: silentLogger()})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	m, pkts := inlineParts(t, clearsignWith(t, signer, []byte(fakeReleasePlaintext)))
	if got, _, err := openpgp.VerifyDetachedSignature(keyring.EntityList(), bytes.NewReader(m), bytes.NewReader(pkts[0].raw), nil); got != nil || err == nil {
		t.Fatalf("test premise: go-crypto over the whole keyring, collider first: signature %v, err %v; want it to fail the valid signature", got != nil, err)
	}
	if err := v.requireKnownSignaturesVerify(m, pkts); err != nil {
		t.Fatalf("a valid signature, a key-ID collider loaded first: %v; want it passed", err)
	}
	// Each entity is still checked: the signer's signature of another text,
	// which no entity holding the key ID verifies, fails.
	_, other := inlineParts(t, clearsignWith(t, signer, []byte("Origin: Other\n")))
	if err := v.requireKnownSignaturesVerify(m, other); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a bad signature, a key-ID collider loaded first: %v; want ErrBadSignature", err)
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
		{"malformed_clearsign", ErrMalformedClearsign, "malformed_clearsign"},
		{
			"malformed_clearsign_as_verify_inline_wraps_a_rule",
			fmt.Errorf("%w: %w", ErrMalformedClearsign, refuse("A4", "line 7: 80 base64 characters, the limit is 76")),
			"malformed_clearsign",
		},
		{"malformed_signature", ErrMalformedSignature, "malformed_signature"},
		{
			"malformed_signature_as_verify_detached_wraps_a_rule",
			fmt.Errorf("%w: %w", ErrMalformedSignature, refuse("T4", "the file ends inside the signature armor")),
			"malformed_signature",
		},
		{"short_keyid", ErrShortKeyID, "short_keyid"},
		{"untrusted_signer", ErrUntrustedSigner, "untrusted_signer"},
		{"ambiguous_keyid", ErrAmbiguousKeyID, "ambiguous_keyid"},
		{"no_usable_signature", ErrNoUsableSignature, "no_usable_signature"},
		{"bad_signature", ErrBadSignature, "bad_signature"},
		{
			"bad_signature_as_s1_wraps_it",
			fmt.Errorf("%w: issuer %016X", ErrBadSignature, uint64(0x0123456789ABCDEF)),
			"bad_signature",
		},
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

// TestVerifier_AcceptAnySigner_NotClearsigned_RequireSigFalseRejected
// asserts that a body without a clearsign envelope is rejected with
// ErrMissingSignature even with require_signature = false and
// accept_any_signer = true (spec C5 rev 4): accept_any_signer relaxes
// trust, never the presence of a clearsigned envelope.
func TestVerifier_AcceptAnySigner_NotClearsigned_RequireSigFalseRejected(t *testing.T) {
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
	if !errors.Is(err, ErrMissingSignature) {
		t.Fatalf("err = %v, want ErrMissingSignature", err)
	}
	if plain != nil {
		t.Fatalf("expected nil result on rejection, got %q", plain)
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

// TestVerifier_AcceptAnySigner_MalformedSignatureBody_StillRejected
// asserts the inline bypass refuses an armored body that is no packet.
// The envelope (layer T) and the base64 (layer A) are sound, but neither
// proves the armored body holds a signature packet. Without a strict
// guard, a clearsigned envelope whose armored body is garbage would
// silently adopt under the bypass. The guard was a parseable-signature
// check, then F6 (structural, so on the bypass too); layer P is the same
// code on both paths and refuses it (P1: no packet header), as
// ErrMalformedClearsign. An armor with no packet at all is layer A's
// (TestVerifyInline_AcceptAnySigner_EmptyArmorStillRejected).
func TestVerifier_AcceptAnySigner_MalformedSignatureBody_StillRejected(t *testing.T) {
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

	// Build a clearsigned envelope whose armored signature body is
	// arbitrary bytes that don't form a valid OpenPGP packet header
	// (PGP packet tag's high bit is required; ASCII text typically
	// fails the parse at the first byte).
	body, err := assembleClearsignedBlock([]byte(fakeReleasePlaintext), []byte("not a signature packet"))
	if err != nil {
		t.Fatalf("assembleClearsignedBlock: %v", err)
	}

	_, err = v.VerifyInline(context.Background(), newSuite(), body)
	if !errors.Is(err, ErrMalformedClearsign) || ruleOf(err) != "P1" {
		t.Fatalf("err = %v (rule %q); want ErrMalformedClearsign from P1 on clearsigned envelope with malformed signature body under accept_any_signer", err, ruleOf(err))
	}
}

// TestVerifyInline_AcceptAnySigner_EmptyArmorStillRejected is the inline
// twin of TestVerifyDetached_AcceptAnySigner_EmptyArmorStillRejected: an
// InRelease whose signature armor holds no packet at all (go-crypto's
// armor writer gives it a CRC line and no base64 line) is refused in both
// modes by layer A (A4: no base64 line), as ErrMalformedClearsign, before
// the trust set is read. Layer T passes it (the premise below): the
// bypass's "at least one signature packet" is layer A's and P's.
func TestVerifyInline_AcceptAnySigner_EmptyArmorStillRejected(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	body, err := assembleClearsignedBlock([]byte(fakeReleasePlaintext), nil)
	if err != nil {
		t.Fatalf("assembleClearsignedBlock: %v", err)
	}
	if _, err := inlineTextLayer(body); err != nil {
		t.Fatalf("test premise: layer T refuses the empty armor: %v", err)
	}
	for _, anySigner := range []bool{false, true} {
		v, err := NewVerifier(VerifierConfig{
			Keyring:          keyring,
			RequireSignature: true,
			AcceptAnySigner:  anySigner,
			Logger:           silentLogger(),
		})
		if err != nil {
			t.Fatalf("NewVerifier: %v", err)
		}
		got, err := v.VerifyInline(context.Background(), newSuite(), body)
		if !errors.Is(err, ErrMalformedClearsign) || ruleOf(err) != "A4" || got != nil {
			t.Errorf("accept_any_signer=%v: got %q, err %v (rule %q); want ErrMalformedClearsign from A4", anySigner, got, err, ruleOf(err))
		}
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
