package gpg

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	pgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"
)

// VerifyDetached covers SPEC2 §7.6.3's "detached Release + Release.gpg"
// path. The structural and trust-set logic mirrors VerifyInline (and
// reuses verifyAnyTrusted), so these tests focus on:
//   - armored signature inputs, and the refusal of binary ones
//   - tampered Release / tampered signature rejection
//   - empty-input guards (programmer-error category, not a fail-open)
//   - per-suite pin propagation
//   - multi-signature packet handling (signature packets concatenated in
//     one armor block)
//
// The shapes apt refuses (F0–F6) are held to apt's measured verdicts by
// TestVerifier_FollowsTheAptShapeTable (aptshape_test.go).

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

// TestVerifyDetached_RefusesABinarySignature (formerly
// TestVerifyDetached_HappyPath_Binary, which pinned acceptance): a binary
// Release.gpg is refused as ErrMalformedSignature, as apt 2.2 to 3.2
// refuse it ("Detached signature file … is in unsupported binary format";
// the corpus' d_binary) — F1, the armor's BEGIN line at byte 0 — and on
// the accept_any_signer bypass too, F1 being structural. The same packets
// armored verify: only the form is refused.
func TestVerifyDetached_RefusesABinarySignature(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	release := []byte(fakeReleasePlaintext)
	sig := detachSignWith(t, signer, release, false /*binary*/)

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
		got, err := v.VerifyDetached(context.Background(), newSuite(), release, sig)
		if !errors.Is(err, ErrMalformedSignature) || ClassifyVerifyErr(err) != "malformed_signature" || got != nil {
			t.Errorf("accept_any_signer=%v: got %q, err %v (reason %q); want ErrMalformedSignature (malformed_signature)",
				anySigner, got, err, ClassifyVerifyErr(err))
		}
		got, err = v.VerifyDetached(context.Background(), newSuite(), release, armorSignature(t, sig))
		if err != nil || !bytes.Equal(got, release) {
			t.Errorf("accept_any_signer=%v: the same signature armored: got %q, err %v; want the Release", anySigner, got, err)
		}
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
	// the packet still parses but the cryptographic check fails. The
	// tampered packet is armored afterwards (a binary Release.gpg is
	// refused before any cryptographic check), with the checksum of the
	// tampered bytes, so the structural checks pass and the
	// cryptographic one is what refuses it.
	tampered := make([]byte, len(sig))
	copy(tampered, sig)
	tampered[len(tampered)-5] ^= 0x01

	_, err := v.VerifyDetached(context.Background(), newSuite(), release, armorSignature(t, tampered))
	if ClassifyVerifyErr(err) != "crypto_verify_failed" || !errors.As(err, new(pgperrors.SignatureError)) {
		t.Fatalf("err = %v (reason %q); want the cryptographic check's SignatureError (crypto_verify_failed)", err, ClassifyVerifyErr(err))
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

func TestVerifyDetached_MultiSig_Armored_StaleAccompaniesCurrent(t *testing.T) {
	// Real Release.gpg may bundle multiple signatures (e.g. during a
	// key rollover, both old and new keys sign). The verifier must
	// accept on whichever packet pairs (a) trusted IssuerFingerprint
	// with (b) cryptographic verification — and reject the rest.
	// (Formerly …_MultiSig_Binary_…: the packets are now armored, one
	// block, since a binary Release.gpg is refused before this loop.)
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

	// Concat: bad first (so the verifier must walk past it) then good,
	// armored as one block.
	combined := append([]byte{}, bad...)
	combined = append(combined, good...)

	got, err := v.VerifyDetached(context.Background(), newSuite(), release, armorSignature(t, combined))
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

	// Armored as one block: a binary Release.gpg would be refused before
	// any cryptographic check, and this test would pass vacuously.
	_, err := v.VerifyDetached(context.Background(), newSuite(), release, armorSignature(t, combined))
	if ClassifyVerifyErr(err) != "crypto_verify_failed" || !errors.As(err, new(pgperrors.SignatureError)) {
		t.Fatalf("err = %v (reason %q); want rejection by the cryptographic check (crypto_verify_failed) when all signatures are over stale cleartext",
			err, ClassifyVerifyErr(err))
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
// raw non-armored garbage under 64 KiB is refused on the bypass branch,
// never propagated untouched into the pool. It was the
// requireParseableSignature guard's case while decodeMaybeArmoredSignature
// passed non-armored input through; since the shape checks (F1: the armor's
// BEGIN line at byte 0) it is ErrMalformedSignature before any packet is
// parsed. This regression case was missed by the original
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
	if !errors.Is(err, ErrMalformedSignature) {
		t.Fatalf("err = %v; want ErrMalformedSignature on raw non-armored garbage Release.gpg under accept_any_signer", err)
	}
}

// TestVerifyDetached_AcceptAnySigner_EmptyArmorStillRejected: an armor
// that holds no packet at all passes the shape checks — F6 refuses every
// packet that is not a parseable signature, and there is none — so on the
// bypass branch requireParseableSignature is what still refuses it, and
// verifyAnyTrusted (ErrNoUsableSignature) on the trusted one.
func TestVerifyDetached_AcceptAnySigner_EmptyArmorStillRejected(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	release := []byte(fakeReleasePlaintext)
	empty := []byte("-----BEGIN PGP SIGNATURE-----\n\n-----END PGP SIGNATURE-----\n")
	if _, err := decodeMaybeArmoredSignature(empty); err != nil {
		t.Fatalf("test premise: the shape checks refuse an empty armor (%v); want them to pass it", err)
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
		got, err := v.VerifyDetached(context.Background(), newSuite(), release, empty)
		if err == nil || got != nil {
			t.Errorf("accept_any_signer=%v: got %q, err %v; want an armor with no packet refused", anySigner, got, err)
		}
		if !anySigner && !errors.Is(err, ErrNoUsableSignature) {
			t.Errorf("trusted path: err = %v; want ErrNoUsableSignature", err)
		}
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
	// triggering the decode error path: ErrMalformedSignature.
	garbage := []byte("-----BEGIN PGP SIGNATURE-----\nthis is not a real signature\n-----END PGP SIGNATURE-----\n")

	_, err = v.VerifyDetached(context.Background(), newSuite(), release, garbage)
	if !errors.Is(err, ErrMalformedSignature) {
		t.Fatalf("err = %v; want ErrMalformedSignature on garbage Release.gpg under accept_any_signer", err)
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
