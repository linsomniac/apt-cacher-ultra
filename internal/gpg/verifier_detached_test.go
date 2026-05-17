package gpg

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// VerifyDetached covers SPEC2 §7.6.3's "detached Release + Release.gpg"
// path. The structural and trust-set logic mirrors VerifyInline (and
// reuses verifyAnyTrusted), so these tests focus on:
//   - both armored and binary signature inputs
//   - tampered Release / tampered signature rejection
//   - empty-input guards (programmer-error category, not a fail-open)
//   - per-suite pin propagation
//   - multi-signature packet handling (concat'd binary signatures)

func TestVerifyDetached_HappyPath_Armored(t *testing.T) {
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

	release := []byte(fakeReleasePlaintext)
	sig := detachSignWith(t, signer, release, true /*armored*/)

	got, err := v.VerifyDetached(context.Background(), newSuite(), release, sig)
	if err != nil {
		t.Fatalf("VerifyDetached: %v", err)
	}
	if !bytes.Equal(got, release) {
		t.Fatalf("Release passthrough mismatch:\ngot=%q\nwant=%q", got, release)
	}
}

func TestVerifyDetached_HappyPath_Binary(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		Logger:           silentLogger(),
	})

	release := []byte(fakeReleasePlaintext)
	sig := detachSignWith(t, signer, release, false /*binary*/)

	got, err := v.VerifyDetached(context.Background(), newSuite(), release, sig)
	if err != nil {
		t.Fatalf("VerifyDetached: %v", err)
	}
	if !bytes.Equal(got, release) {
		t.Fatalf("Release passthrough mismatch")
	}
}

func TestVerifyDetached_HappyPath_PinnedSubset(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	other := newTestEntity(t, "Other", "other@example.com")
	keyring := newKeyring(t, signer, other)

	pin := SignerPin{
		HostRegex: regexp.MustCompile(`^archive\.example\.com$`),
		Fingerprints: map[string]struct{}{
			upperFP(signer.PrimaryKey.Fingerprint): {},
		},
	}
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		Pins:             []SignerPin{pin},
		RequireSignature: true,
		RequirePinned:    true,
		Logger:           silentLogger(),
	})

	release := []byte(fakeReleasePlaintext)
	sig := detachSignWith(t, signer, release, true)

	if _, err := v.VerifyDetached(context.Background(), newSuite(), release, sig); err != nil {
		t.Fatalf("VerifyDetached: %v", err)
	}
}

func TestVerifyDetached_KeyOutsideKeyring(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	other := newTestEntity(t, "Other", "other@example.com")
	keyring := newKeyring(t, other) // signer NOT in keyring
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		Logger:           silentLogger(),
	})

	release := []byte(fakeReleasePlaintext)
	sig := detachSignWith(t, signer, release, true)

	_, err := v.VerifyDetached(context.Background(), newSuite(), release, sig)
	if err == nil {
		t.Fatal("expected verification failure for untrusted signer")
	}
	if !errors.Is(err, ErrUntrustedSigner) {
		t.Fatalf("err type wrong: %v (want ErrUntrustedSigner)", err)
	}
}

func TestVerifyDetached_TamperedRelease(t *testing.T) {
	// Sign a Release; then bit-flip a byte in the Release before
	// passing both to VerifyDetached. The signature is correctly
	// formed, the signer is trusted, but the message hash won't match.
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		Logger:           silentLogger(),
	})

	release := []byte(fakeReleasePlaintext)
	sig := detachSignWith(t, signer, release, true)

	tampered := make([]byte, len(release))
	copy(tampered, release)
	// Flip a bit in the middle of the body.
	tampered[len(tampered)/2] ^= 0x01

	if _, err := v.VerifyDetached(context.Background(), newSuite(), tampered, sig); err == nil {
		t.Fatal("expected verification failure on tampered Release")
	}
}

func TestVerifyDetached_TamperedSignature(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		Logger:           silentLogger(),
	})

	release := []byte(fakeReleasePlaintext)
	sig := detachSignWith(t, signer, release, false)

	// Bit-flip a late byte in the signature packet — early bytes
	// would corrupt the packet header itself; we want a body byte so
	// the packet still parses but the cryptographic check fails.
	tampered := make([]byte, len(sig))
	copy(tampered, sig)
	tampered[len(tampered)-5] ^= 0x01

	if _, err := v.VerifyDetached(context.Background(), newSuite(), release, tampered); err == nil {
		t.Fatal("expected verification failure on tampered signature")
	}
}

func TestVerifyDetached_EmptyRelease(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		Logger:           silentLogger(),
	})
	sig := detachSignWith(t, signer, []byte("dummy"), true)

	_, err := v.VerifyDetached(context.Background(), newSuite(), nil, sig)
	if err == nil || !contains(err.Error(), "empty Release body") {
		t.Fatalf("want empty-Release error, got %v", err)
	}
}

func TestVerifyDetached_EmptySignature(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		Logger:           silentLogger(),
	})

	_, err := v.VerifyDetached(context.Background(), newSuite(), []byte("anything"), nil)
	if err == nil || !contains(err.Error(), "empty Release.gpg body") {
		t.Fatalf("want empty-sig error, got %v", err)
	}
}

func TestVerifyDetached_PinnedNoMatch_RequireFailsClosed(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	pin := SignerPin{
		HostRegex: regexp.MustCompile(`^other\.example\.org$`),
		Fingerprints: map[string]struct{}{
			upperFP(signer.PrimaryKey.Fingerprint): {},
		},
	}
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		Pins:             []SignerPin{pin},
		RequireSignature: true,
		RequirePinned:    true,
		Logger:           silentLogger(),
	})

	release := []byte(fakeReleasePlaintext)
	sig := detachSignWith(t, signer, release, true)

	_, err := v.VerifyDetached(context.Background(), newSuite(), release, sig)
	if err == nil {
		t.Fatal("expected ErrUnpinnedSuite")
	}
	if !errors.Is(err, ErrUnpinnedSuite) {
		t.Fatalf("err: %v want ErrUnpinnedSuite", err)
	}
}

func TestVerifyDetached_PinMatchedButNoKeyInKeyring(t *testing.T) {
	// Pin lists a fingerprint that isn't loaded into the keyring.
	// Pin matched the host regex (so the unpinned-suite branch is
	// skipped), but the trust-set intersection is empty → fail closed
	// with ErrNoUsableSignature.
	signer := newTestEntity(t, "Signer", "signer@example.com")
	other := newTestEntity(t, "Other", "other@example.com")
	keyring := newKeyring(t, other) // signer NOT here

	pin := SignerPin{
		HostRegex: regexp.MustCompile(`^archive\.example\.com$`),
		Fingerprints: map[string]struct{}{
			upperFP(signer.PrimaryKey.Fingerprint): {},
		},
	}
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		Pins:             []SignerPin{pin},
		RequireSignature: true,
		RequirePinned:    true,
		Logger:           silentLogger(),
	})

	release := []byte(fakeReleasePlaintext)
	sig := detachSignWith(t, signer, release, true)

	_, err := v.VerifyDetached(context.Background(), newSuite(), release, sig)
	if err == nil || !errors.Is(err, ErrNoUsableSignature) {
		t.Fatalf("err: %v want ErrNoUsableSignature", err)
	}
}

func TestVerifyDetached_GarbageSignatureBytes(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		Logger:           silentLogger(),
	})

	if _, err := v.VerifyDetached(
		context.Background(), newSuite(),
		[]byte(fakeReleasePlaintext),
		[]byte("not a valid pgp signature, just ASCII text"),
	); err == nil {
		t.Fatal("expected error on garbage signature input")
	}
}

func TestVerifyDetached_WrongArmorType(t *testing.T) {
	// Construct an armored block whose Type is "PGP MESSAGE" rather
	// than "PGP SIGNATURE". Real Release.gpg is always SIGNATURE; an
	// upstream feeding a MESSAGE block should be rejected with a
	// useful error rather than misinterpreted.
	armoredOther := []byte(`-----BEGIN PGP MESSAGE-----

owEBPwHA/pANAwAKAdwSv0NICCipAcsBYwBn3jGwbm90IGEgcmVhbCBzaWduYXR1
cmUKiQGzBAABCgAdFiEEZeP9bv2vdGvmqQ7L3BK/Q0gIKKkFAmfeMbAACgkQ3BK/
Q0gIKKkPxQv/RJUL/BQEbZE76q+vKyEfnRzwZ8BWBqoIHGYjcdaRXz5cVOmu0Nd9
=Ab1z
-----END PGP MESSAGE-----`)

	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		Logger:           silentLogger(),
	})

	_, err := v.VerifyDetached(
		context.Background(), newSuite(),
		[]byte(fakeReleasePlaintext), armoredOther,
	)
	if err == nil {
		t.Fatal("expected error on wrong-armor-type input")
	}
}

func TestVerifyDetached_MultiSig_Binary_StaleAccompaniesCurrent(t *testing.T) {
	// Real Release.gpg may bundle multiple signatures (e.g. during a
	// key rollover, both old and new keys sign). The verifier must
	// accept on whichever packet pairs (a) trusted IssuerFingerprint
	// with (b) cryptographic verification — and reject the rest.
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		Logger:           silentLogger(),
	})

	release := []byte(fakeReleasePlaintext)
	stale := []byte("Origin: Stale\n")

	good := detachSignWith(t, signer, release, false /*binary*/)
	bad := detachSignWith(t, signer, stale, false)

	// Concat: bad first (so the verifier must walk past it) then good.
	combined := append([]byte{}, bad...)
	combined = append(combined, good...)

	got, err := v.VerifyDetached(context.Background(), newSuite(), release, combined)
	if err != nil {
		t.Fatalf("multi-sig verify failed: %v", err)
	}
	if !bytes.Equal(got, release) {
		t.Fatalf("Release passthrough mismatch")
	}
}

func TestVerifyDetached_MultiSig_AllStaleRejects(t *testing.T) {
	// Inverse of the above: every signature is over stale cleartext,
	// so no packet verifies against the actual Release. The loop must
	// not accept on IssuerFingerprint alone.
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		Logger:           silentLogger(),
	})

	release := []byte(fakeReleasePlaintext)
	stale1 := []byte("Origin: STALE-1\n")
	stale2 := []byte("Origin: STALE-2\n")

	bad1 := detachSignWith(t, signer, stale1, false)
	bad2 := detachSignWith(t, signer, stale2, false)
	combined := append([]byte{}, bad1...)
	combined = append(combined, bad2...)

	if _, err := v.VerifyDetached(context.Background(), newSuite(), release, combined); err == nil {
		t.Fatal("expected rejection when all signatures are over stale cleartext")
	}
}

// keyring path is covered explicitly in keyring_test.go; here we
// simply confirm a freshly-created Verifier with RequirePinned=false
// and an empty pins list resolves trust set without surprises.
func TestVerifyDetached_BroadTrustEmptyPins(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		Pins:             nil,
		RequireSignature: true,
		RequirePinned:    false,
		Logger:           silentLogger(),
	})
	release := []byte(fakeReleasePlaintext)
	sig := detachSignWith(t, signer, release, true)
	if _, err := v.VerifyDetached(context.Background(), newSuite(), release, sig); err != nil {
		t.Fatalf("VerifyDetached: %v", err)
	}
}

// silentLogger / newKeyring / newSuite / fakeReleasePlaintext are
// defined in verifier_test.go; testkeys_test.go provides
// detachSignWith. We only need a tiny string contains helper here
// for the "expected substring" error checks above.
func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && bytes.Contains([]byte(haystack), []byte(needle)))
}

// TestVerifyDetached_AcceptAnySigner_NoPin_PassThrough asserts that
// under accept_any_signer = true, a Release + Release.gpg pair signed
// by a key NOT in the host keyring is adopted: Release bytes are
// returned verbatim and no cryptographic verification runs. The
// signature blob must still parse structurally — that is the only
// integrity guard left when trust is delegated to apt clients.
func TestVerifyDetached_AcceptAnySigner_NoPin_PassThrough(t *testing.T) {
	signer := newTestEntity(t, "Untrusted", "untrusted@example.com")
	other := newTestEntity(t, "Other", "other@example.com")
	keyring := newKeyring(t, other) // signer NOT in keyring

	v, err := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		AcceptAnySigner:  true,
		Logger:           silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	release := []byte(fakeReleasePlaintext)
	sig := detachSignWith(t, signer, release, true /*armored*/)

	got, err := v.VerifyDetached(context.Background(), newSuite(), release, sig)
	if err != nil {
		t.Fatalf("VerifyDetached: %v (want nil under accept_any_signer)", err)
	}
	if !bytes.Equal(got, release) {
		t.Fatalf("Release passthrough mismatch:\ngot=%q\nwant=%q", got, release)
	}
}

// TestVerifyDetached_AcceptAnySigner_RawGarbage_StillRejected asserts
// the requireParseableSignature guard catches raw non-armored garbage
// under 64 KiB: decodeMaybeArmoredSignature only size-caps non-armored
// input, so without the packet-parse check, arbitrary bytes would
// propagate untouched through the bypass branch into the pool. This
// regression case was missed by the original
// TestVerifyDetached_AcceptAnySigner_GarbageSignature_StillRejected,
// which only exercised the armored-frame structural rejection.
func TestVerifyDetached_AcceptAnySigner_RawGarbage_StillRejected(t *testing.T) {
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

	release := []byte(fakeReleasePlaintext)
	garbage := []byte("not a valid pgp signature, just plain text bytes")

	_, err = v.VerifyDetached(context.Background(), newSuite(), release, garbage)
	if err == nil {
		t.Fatal("expected error on raw non-armored garbage Release.gpg under accept_any_signer")
	}
}

// TestVerifyDetached_AcceptAnySigner_GarbageSignature_StillRejected
// asserts the structural decode guard: a Release.gpg that does not
// parse as a PGP signature is rejected even with accept_any_signer on.
// The bypass relaxes the trust + crypto checks; it does not silently
// adopt arbitrary garbage as if it were a signature.
func TestVerifyDetached_AcceptAnySigner_GarbageSignature_StillRejected(t *testing.T) {
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

	release := []byte(fakeReleasePlaintext)
	// "Garbage" that starts with an armor frame so decodeMaybeArmoredSignature
	// tries armor.Decode first; the armor frame is malformed (no body),
	// triggering the decode error path.
	garbage := []byte("-----BEGIN PGP SIGNATURE-----\nthis is not a real signature\n-----END PGP SIGNATURE-----\n")

	_, err = v.VerifyDetached(context.Background(), newSuite(), release, garbage)
	if err == nil {
		t.Fatal("expected decode error on garbage Release.gpg under accept_any_signer")
	}
}

// TestVerifyDetached_AcceptAnySigner_WithMatchingPin_StillEnforced
// mirrors the inline version: a [[trusted_signer]] block matching the
// suite remains authoritative; accept_any_signer is inert for pinned
// suites.
func TestVerifyDetached_AcceptAnySigner_WithMatchingPin_StillEnforced(t *testing.T) {
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

	release := []byte(fakeReleasePlaintext)
	sig := detachSignWith(t, other, release, true)
	_, err = v.VerifyDetached(context.Background(), newSuite(), release, sig)
	if err == nil {
		t.Fatal("expected ErrUntrustedSigner for non-pinned signer under matching pin")
	}
	if !errors.Is(err, ErrUntrustedSigner) {
		t.Fatalf("err type wrong: %v (want ErrUntrustedSigner)", err)
	}
}

// Compile-time anchor: these tests rely on openpgp.ArmoredDetachSign,
// detachSignWith, etc. — keep imports honest.
var _ = openpgp.NewEntity
var _ = filepath.Join
