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
	if !bytes.Contains([]byte(err.Error()), []byte("BEGIN marker")) {
		t.Fatalf("error doesn't mention BEGIN marker: %v", err)
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
	if !bytes.Contains([]byte(err.Error()), []byte("END marker")) {
		t.Fatalf("error doesn't mention END marker: %v", err)
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
// refuses it too (F0, the 2026-09 security review's VC-2).
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
		if !errors.Is(err, ErrMalformedClearsign) || !strings.Contains(err.Error(), "after the END marker") {
			t.Errorf("END line, then %q: err = %v; want ErrMalformedClearsign, extraneous bytes after the END marker", tail, err)
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
	// Premise: go-crypto verifies E's signature and reports the expiry.
	block, err := decodeClearsigned(eSigned)
	if err != nil || block == nil {
		t.Fatalf("test premise: E's InRelease does not decode (block %v, err %v)", block, err)
	}
	if got, _, verr := openpgp.VerifyDetachedSignature(openpgp.EntityList{e}, bytes.NewReader(block.Bytes), bytes.NewReader(eGood), nil); got == nil || !errors.Is(verr, pgperrors.ErrKeyExpired) {
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

// TestVerifyInline_ACriticalNotationFailsS1: a signature from a known key
// that verifies but carries a critical notation go-crypto does not know
// fails S1 — go-crypto returns it with a SignatureError, the one error
// beside a non-nil signature that S1 counts; gpgv reports BADSIG — so A's
// critical-notation signature beside A's good one is refused, in either
// order (the corpus' xi_crit_plus_good).
func TestVerifyInline_ACriticalNotationFailsS1(t *testing.T) {
	a := newTestEntity(t, "A", "a@example.com")
	v, err := NewVerifier(VerifierConfig{Keyring: newKeyring(t, a), RequireSignature: true, Logger: silentLogger()})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	critSigned := clearsignWithConfig(t, a, []byte(fakeReleasePlaintext), &packet.Config{
		DefaultHash: crypto.SHA256,
		Time:        time.Now,
		SignatureNotations: []*packet.Notation{
			{Name: "crit@example.invalid", Value: []byte("1"), IsCritical: true, IsHumanReadable: true},
		},
	})
	crit := signaturePacketsOf(t, critSigned)
	good := signaturePacketsOf(t, clearsignWith(t, a, []byte(fakeReleasePlaintext)))
	// Premise: go-crypto verifies the signature and reports the notation.
	block, err := decodeClearsigned(critSigned)
	if err != nil || block == nil {
		t.Fatalf("test premise: the InRelease does not decode (block %v, err %v)", block, err)
	}
	if got, _, verr := openpgp.VerifyDetachedSignature(openpgp.EntityList{a}, bytes.NewReader(block.Bytes), bytes.NewReader(crit), nil); got == nil || !errors.As(verr, new(pgperrors.SignatureError)) {
		t.Fatalf("test premise: go-crypto on the critical-notation signature: signature %v, err %v; want the signature and a SignatureError", got != nil, verr)
	}
	for name, packets := range map[string][][]byte{
		"critical first":  {crit, good},
		"critical second": {good, crit},
	} {
		got, err := v.VerifyInline(context.Background(), newSuite(), inlineWithSignatures(t, packets...))
		if !errors.Is(err, ErrBadSignature) || ClassifyVerifyErr(err) != "bad_signature" || got != nil {
			t.Errorf("%s: got %q, err %v (reason %q); want ErrBadSignature (bad_signature)", name, got, err, ClassifyVerifyErr(err))
		}
	}
}

// TestVerifyInline_AnUnhashedIssuerContradictingTheHashedOneFailsS1 pins
// S1's contradiction rule (DN-5), which no corpus row pins: there
// go-crypto's own lookup by the rewritten unhashed issuer fails the packet
// anyway. Here the unhashed issuer names another key of the same entity,
// the one that made the signature. X's signing subkey signs with a hashed
// issuer fingerprint that names X's primary key (the signer controls the
// hashed area), and an unhashed issuer key ID naming the subkey is added
// (no key needed). go-crypto looks the key up by that unhashed key ID and
// verifies the packet; gpgv looks it up by the hashed fingerprint, X's
// primary key, and reports BADSIG (measured 2026-10-29 with the oracle's
// in-container.sh: every gpgv-based apt refuses this shape, debian:13's sqv
// accepts it). So the anchor, whose only signature this is, is refused
// (ErrBadSignature); without the rule S1 would pass it and S2 accept it.
func TestVerifyInline_AnUnhashedIssuerContradictingTheHashedOneFailsS1(t *testing.T) {
	x := newTestEntity(t, "X", "x@example.com")
	if err := x.AddSigningSubkey(&packet.Config{
		Algorithm: packet.PubKeyAlgoRSA,
		RSABits:   2048,
		Time:      func() time.Time { return time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC) },
	}); err != nil {
		t.Fatalf("AddSigningSubkey: %v", err)
	}
	sub := x.Subkeys[len(x.Subkeys)-1]
	v, err := NewVerifier(VerifierConfig{Keyring: newKeyring(t, x), RequireSignature: true, Logger: silentLogger()})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	// The subkey signs; its private key names X's primary key as the
	// issuer fingerprint while it signs (clearsign takes the issuer from
	// the signing key).
	fpr := sub.PublicKey.Fingerprint
	sub.PublicKey.Fingerprint = x.PrimaryKey.Fingerprint
	signed := clearsignWithConfig(t, &openpgp.Entity{PrivateKey: sub.PrivateKey}, []byte(fakeReleasePlaintext),
		&packet.Config{DefaultHash: crypto.SHA256, Time: time.Now})
	sub.PublicKey.Fingerprint = fpr
	unhashed16 := append([]byte{9, 16}, binary.BigEndian.AppendUint64(nil, sub.PublicKey.KeyId)...)
	contradicting := withUnhashedSubpacket(t, signaturePacketsOf(t, signed), unhashed16)
	body := inlineWithSignatures(t, contradicting)

	// Premises: the hashed issuer is X's primary key, go-crypto's is the
	// subkey, and go-crypto verifies the packet against X.
	p, err := packet.Read(bytes.NewReader(contradicting))
	sig, ok := p.(*packet.Signature)
	if err != nil || !ok {
		t.Fatalf("test premise: the packet parses as %T, %v", p, err)
	}
	var goCryptoID uint64
	if sig.IssuerKeyId != nil {
		goCryptoID = *sig.IssuerKeyId
	}
	if id, hashed := hashedIssuer(sig); !hashed || id != x.PrimaryKey.KeyId || goCryptoID != sub.PublicKey.KeyId {
		t.Fatalf("test premise: hashed issuer %016X (%v), go-crypto's %016X; want X's primary key %016X and the subkey %016X",
			id, hashed, goCryptoID, x.PrimaryKey.KeyId, sub.PublicKey.KeyId)
	}
	block, err := decodeClearsigned(body)
	if err != nil || block == nil {
		t.Fatalf("test premise: the InRelease does not decode (block %v, err %v)", block, err)
	}
	if got, _, verr := openpgp.VerifyDetachedSignature(v.keyring.EntityList(), bytes.NewReader(block.Bytes), bytes.NewReader(contradicting), nil); got == nil || verr != nil {
		t.Fatalf("test premise: go-crypto on the packet: signature %v, err %v; want it verified (by the unhashed key ID)", got != nil, verr)
	}

	got, err := v.VerifyInline(context.Background(), newSuite(), body)
	if !errors.Is(err, ErrBadSignature) || !strings.Contains(err.Error(), "contradicts") || got != nil {
		t.Fatalf("got %q, err %v (reason %q); want ErrBadSignature: the unhashed issuer contradicts the hashed one", got, err, ClassifyVerifyErr(err))
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
	signedAndPackets := func(body []byte) ([]byte, []byte) {
		t.Helper()
		block, err := decodeClearsigned(body)
		if err != nil || block == nil {
			t.Fatalf("test premise: the InRelease does not decode (block %v, err %v)", block, err)
		}
		sigBytes, err := readArmoredSignature(block.ArmoredSignature.Body)
		if err != nil {
			t.Fatalf("readArmoredSignature: %v", err)
		}
		return block.Bytes, sigBytes
	}
	signed, sigBytes := signedAndPackets(clearsignWith(t, signer, []byte(fakeReleasePlaintext)))
	if got, _, err := openpgp.VerifyDetachedSignature(keyring.EntityList(), bytes.NewReader(signed), bytes.NewReader(sigBytes), nil); got != nil || err == nil {
		t.Fatalf("test premise: go-crypto over the whole keyring, collider first: signature %v, err %v; want it to fail the valid signature", got != nil, err)
	}
	if err := v.requireKnownSignaturesVerify(signed, sigBytes); err != nil {
		t.Fatalf("a valid signature, a key-ID collider loaded first: %v; want it passed", err)
	}
	// Each entity is still checked: the signer's signature of another text,
	// which no entity holding the key ID verifies, fails.
	_, otherSigBytes := signedAndPackets(clearsignWith(t, signer, []byte("Origin: Other\n")))
	if err := v.requireKnownSignaturesVerify(signed, otherSigBytes); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a bad signature, a key-ID collider loaded first: %v; want ErrBadSignature", err)
	}
}

// TestHashedIssuer pins S1's issuer (DN-5): the key ID the hashed subpacket
// area names, read from sig.HashSuffix — an issuer fingerprint (subpacket
// 33) first, else an issuer key ID (16), the first of two as gpgv reads
// them — and false when it names neither. go-crypto's own sig.IssuerKeyId
// is the last issuer subpacket it parsed, the unhashed area's after the
// hashed one's, which an on-path attacker rewrites without a key. The v4
// packets are framed by hand (a creation time, the subpackets given, a
// dummy signature value) and parsed by go-crypto, which builds the
// HashSuffix hashedIssuer reads; the last two cases are go-crypto's own v4
// and v6 signatures.
func TestHashedIssuer(t *testing.T) {
	sub := func(typ byte, data ...byte) []byte { return append([]byte{byte(1 + len(data)), typ}, data...) }
	created := sub(2, 0x65, 0x00, 0x00, 0x00) // signature creation time, 2023-09-12
	keyIDOf := func(id uint64) []byte { return binary.BigEndian.AppendUint64(nil, id) }
	fprNaming := func(id uint64) []byte { // a v4 fingerprint: its key ID is its last 8 bytes
		return append(append([]byte{4}, bytes.Repeat([]byte{0xab}, 12)...), keyIDOf(id)...)
	}
	const hashedID, unhashedID = uint64(0x1111222233334444), uint64(0x5555666677778888)
	parse := func(hashed, unhashed []byte) *packet.Signature {
		t.Helper()
		body := []byte{4, byte(packet.SigTypeBinary), byte(packet.PubKeyAlgoRSA), 8} // v4, SHA-256
		body = binary.BigEndian.AppendUint16(body, uint16(len(hashed)))
		body = append(body, hashed...)
		body = binary.BigEndian.AppendUint16(body, uint16(len(unhashed)))
		body = append(body, unhashed...)
		body = append(body, 0x12, 0x34, 0x00, 0x08, 0xff) // the hash tag, an 8-bit MPI
		if len(body) >= 192 {
			t.Fatalf("test premise: a %d-byte body needs a longer length encoding", len(body))
		}
		p, err := packet.Read(bytes.NewReader(append([]byte{0xc2, byte(len(body))}, body...)))
		sig, ok := p.(*packet.Signature)
		if err != nil || !ok {
			t.Fatalf("test premise: go-crypto parses %T, %v; want a *packet.Signature", p, err)
		}
		return sig
	}
	for _, tc := range []struct {
		name             string
		hashed, unhashed []byte
		wantID           uint64
		wantOK           bool
		goCryptoID       uint64 // *sig.IssuerKeyId, 0 when nil
	}{
		{"hashed 33 only", joined(created, sub(33, fprNaming(hashedID)...)), nil, hashedID, true, hashedID},
		{"hashed 16 only", joined(created, sub(16, keyIDOf(hashedID)...)), nil, hashedID, true, hashedID},
		{"hashed 33, unhashed 16", joined(created, sub(33, fprNaming(hashedID)...)), sub(16, keyIDOf(unhashedID)...), hashedID, true, unhashedID},
		{"hashed 16 then 33 naming another key", joined(created, sub(16, keyIDOf(unhashedID)...), sub(33, fprNaming(hashedID)...)), nil, hashedID, true, hashedID},
		{"two hashed 16s: the first, as gpgv", joined(created, sub(16, keyIDOf(hashedID)...), sub(16, keyIDOf(unhashedID)...)), nil, hashedID, true, unhashedID},
		{"hashed nothing, unhashed 16", created, sub(16, keyIDOf(unhashedID)...), 0, false, unhashedID},
		{"hashed nothing, unhashed 33", created, sub(33, fprNaming(unhashedID)...), 0, false, unhashedID},
	} {
		sig := parse(tc.hashed, tc.unhashed)
		var goCryptoID uint64
		if sig.IssuerKeyId != nil {
			goCryptoID = *sig.IssuerKeyId
		}
		if goCryptoID != tc.goCryptoID {
			t.Errorf("%s: test premise: go-crypto's IssuerKeyId %016X, want %016X", tc.name, goCryptoID, tc.goCryptoID)
		}
		if id, ok := hashedIssuer(sig); id != tc.wantID || ok != tc.wantOK {
			t.Errorf("%s: hashedIssuer = %016X, %v; want %016X, %v", tc.name, id, ok, tc.wantID, tc.wantOK)
		}
	}
	v6, err := openpgp.NewEntity("V6", "", "v6@example.com", &packet.Config{V6Keys: true, Algorithm: packet.PubKeyAlgoEd25519})
	if err != nil {
		t.Fatalf("NewEntity (v6): %v", err)
	}
	for _, e := range []*openpgp.Entity{newTestEntity(t, "V4", "v4@example.com"), v6} {
		p, err := packet.Read(bytes.NewReader(detachSignWith(t, e, []byte(fakeReleasePlaintext), false)))
		sig, ok := p.(*packet.Signature)
		if err != nil || !ok || sig.Version != e.PrimaryKey.Version {
			t.Fatalf("test premise: go-crypto's v%d signature parses as %T (err %v)", e.PrimaryKey.Version, p, err)
		}
		if id, ok := hashedIssuer(sig); id != e.PrimaryKey.KeyId || !ok {
			t.Errorf("go-crypto's v%d signature: hashedIssuer = %016X, %v; want the signer's key ID %016X, true",
				sig.Version, id, ok, e.PrimaryKey.KeyId)
		}
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
		{"malformed_clearsign_decoder_panic", fmt.Errorf("x: %w", errClearsignPanicked), "malformed_clearsign"},
		{"malformed_signature", ErrMalformedSignature, "malformed_signature"},
		{
			"malformed_signature_as_verify_detached_wraps_it",
			fmt.Errorf("decode signature: %w", fmt.Errorf("%w: %v", ErrMalformedSignature, errors.New("no END line"))),
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
// clearsign.Decode validates the BEGIN/END envelope, and armor.Decode
// validates base64 framing, but neither proves the armored body
// contains a parseable OpenPGP signature packet. Without a strict
// guard, a clearsigned envelope whose armored body is garbage would
// silently adopt under the bypass. The guard was requireParseableSignature;
// F6 (requireSignaturePacketsOnly, structural, so on the bypass too) now
// refuses it first, as ErrMalformedClearsign. requireParseableSignature
// still refuses an armor with no packet at all, which F6 passes:
// TestVerifyInline_AcceptAnySigner_EmptyArmorStillRejected pins it.
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
	if !errors.Is(err, ErrMalformedClearsign) {
		t.Fatalf("err = %v; want ErrMalformedClearsign on clearsigned envelope with malformed signature body under accept_any_signer", err)
	}
}

// TestVerifyInline_AcceptAnySigner_EmptyArmorStillRejected is the inline
// twin of TestVerifyDetached_AcceptAnySigner_EmptyArmorStillRejected: an
// InRelease whose signature armor holds no packet at all passes the shape
// checks — F1 and F0, the decode, and F2–F6 (F6 refuses every packet that is
// not a parseable signature, and there is none) — so on the
// accept_any_signer bypass requireParseableSignature is what refuses it
// ("clearsigned signature body: … no signature packet"), and on the trusted
// path verifyAnyTrusted (ErrNoUsableSignature).
func TestVerifyInline_AcceptAnySigner_EmptyArmorStillRejected(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	body, err := assembleClearsignedBlock([]byte(fakeReleasePlaintext), nil)
	if err != nil {
		t.Fatalf("assembleClearsignedBlock: %v", err)
	}
	if err := requireBareClearsigned(body); err != nil {
		t.Fatalf("test premise: F1 or F0 refuses the empty armor: %v", err)
	}
	block, err := decodeClearsigned(body)
	if err != nil || block == nil {
		t.Fatalf("test premise: the envelope does not decode (block %v, err %v)", block, err)
	}
	sigBytes, err := readArmoredSignature(block.ArmoredSignature.Body)
	if err != nil || len(sigBytes) != 0 {
		t.Fatalf("test premise: the armor holds %d bytes (err %v), want none", len(sigBytes), err)
	}
	if err := requireInlineSignatureShape(body, sigBytes); err != nil {
		t.Fatalf("test premise: F2–F6 refuse the empty armor (%v); want them to pass it", err)
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
		switch {
		case err == nil || got != nil:
			t.Errorf("accept_any_signer=%v: got %q, err %v; want an armor with no packet refused", anySigner, got, err)
		case anySigner && (!strings.Contains(err.Error(), "clearsigned signature body") || !strings.Contains(err.Error(), "no signature packet")):
			t.Errorf("bypass: err = %v; want requireParseableSignature's refusal (clearsigned signature body: … no signature packet)", err)
		case !anySigner && !errors.Is(err, ErrNoUsableSignature):
			t.Errorf("trusted path: err = %v; want ErrNoUsableSignature", err)
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
