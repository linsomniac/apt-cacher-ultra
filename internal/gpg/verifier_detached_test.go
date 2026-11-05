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
	// cryptographic one is what refuses it: S1's, since the packet names
	// a key in the host keyring (ErrBadSignature, bad_signature; it was
	// verifyAnyTrusted's SignatureError, crypto_verify_failed, before S1).
	tampered := make([]byte, len(sig))
	copy(tampered, sig)
	tampered[len(tampered)-5] ^= 0x01

	_, err := v.VerifyDetached(context.Background(), newSuite(), release, armorSignature(t, tampered))
	if !errors.Is(err, ErrBadSignature) || ClassifyVerifyErr(err) != "bad_signature" {
		t.Fatalf("err = %v (reason %q); want S1's ErrBadSignature (bad_signature)", err, ClassifyVerifyErr(err))
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
	); !errors.Is(err, ErrMalformedSignature) || ruleOf(err) != "T1" {
		t.Fatalf("garbage signature input: %v (rule %q); want ErrMalformedSignature, refused by T1 (no BEGIN line)", err, ruleOf(err))
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
	if !errors.Is(err, ErrMalformedSignature) || ruleOf(err) != "T1" {
		t.Fatalf("wrong-armor-type input: %v (rule %q); want ErrMalformedSignature, refused by T1 (no signature BEGIN line)", err, ruleOf(err))
	}
}

func TestVerifyDetached_MultiSig_Armored_StaleAccompaniesCurrent(t *testing.T) {
	// Real Release.gpg may bundle multiple signatures (e.g. during a
	// key rollover, both old and new keys sign). The verifier must
	// accept on whichever packet pairs (a) trusted IssuerFingerprint
	// with (b) cryptographic verification — and walk past the rest.
	// (Formerly …_MultiSig_Binary_…: the packets are now armored, one
	// block, since a binary Release.gpg is refused before this loop.)
	// The stale packet is the old key's, which has expired since it
	// signed: it passes S1 (gpgv's EXPKEYSIG, which apt ignores beside a
	// good signature) and fails verifyAnyTrusted's check
	// (ErrKeyExpired). It used to be the signer's own signature of
	// another text, which S1 now refuses:
	// TestVerifyDetached_MultiSig_Armored_StaleAndCurrentRejects.
	signer := newTestEntity(t, "Signer", "signer@example.com")
	old := newExpiredEntity(t, "Old", "old@example.com")
	keyring := newKeyring(t, signer, old)
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		Logger:           silentLogger(),
	})

	release := []byte(fakeReleasePlaintext)

	good := detachSignWith(t, signer, release, false /*binary*/)
	// The old key's signature of the same Release, made while it was
	// valid (its first second).
	stale := detachSignAt(t, old, release, old.PrimaryKey.CreationTime)
	// Premise: the stale packet alone fails verifyAnyTrusted's check.
	if _, err := v.VerifyDetached(context.Background(), newSuite(), release, armorSignature(t, stale)); !errors.Is(err, pgperrors.ErrKeyExpired) {
		t.Fatalf("test premise: the old key's signature alone: %v; want verifyAnyTrusted's ErrKeyExpired", err)
	}

	// Concat: stale first (so the verifier must walk past it) then good,
	// armored as one block.
	combined := joined(stale, good)

	got, err := v.VerifyDetached(context.Background(), newSuite(), release, armorSignature(t, combined))
	if err != nil {
		t.Fatalf("multi-sig verify failed: %v", err)
	}
	if !bytes.Equal(got, release) {
		t.Fatalf("Release passthrough mismatch")
	}
}

// TestVerifyDetached_MultiSig_Armored_StaleAndCurrentRejects:
// TestVerifyDetached_MultiSig_Armored_StaleAccompaniesCurrent's former
// shape, a stale signature by the trusted key (over another text) before
// the current one, is refused: a bad signature from a known key beside a
// good one is fatal, as apt's BADSIG is (S1, ErrBadSignature; the corpus'
// d_badsig_badfirst).
func TestVerifyDetached_MultiSig_Armored_StaleAndCurrentRejects(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	v, _ := NewVerifier(VerifierConfig{
		Keyring:          newKeyring(t, signer),
		RequireSignature: true,
		Logger:           silentLogger(),
	})
	release := []byte(fakeReleasePlaintext)
	good := detachSignWith(t, signer, release, false /*binary*/)
	bad := detachSignWith(t, signer, []byte("Origin: Stale\n"), false)
	got, err := v.VerifyDetached(context.Background(), newSuite(), release, armorSignature(t, joined(bad, good)))
	if !errors.Is(err, ErrBadSignature) || ClassifyVerifyErr(err) != "bad_signature" || got != nil {
		t.Fatalf("got %q, err %v (reason %q); want ErrBadSignature (bad_signature)", got, err, ClassifyVerifyErr(err))
	}
}

func TestVerifyDetached_MultiSig_AllStaleRejects(t *testing.T) {
	// Inverse of TestVerifyDetached_MultiSig_Armored_StaleAccompaniesCurrent:
	// every signature is over stale cleartext, so no packet verifies
	// against the actual Release. The verifier must not accept on
	// IssuerFingerprint alone; since S1, a signature from a known key that
	// fails is refused before S2's loop runs (ErrBadSignature).
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
	if !errors.Is(err, ErrBadSignature) || ClassifyVerifyErr(err) != "bad_signature" {
		t.Fatalf("err = %v (reason %q); want ErrBadSignature (bad_signature) when all signatures are over stale cleartext",
			err, ClassifyVerifyErr(err))
	}
}

// TestVerifyDetached_ABadSignatureFromAKnownKeyRejects is the detached twin
// of TestVerifyInline_ABadSignatureFromAKnownKeyRejects (S1, VC-1): a
// Release.gpg that carries, beside A's good signature, A's signature of
// another text is refused in either order (ErrBadSignature, bad_signature;
// the corpus' d_badsig_badfirst and d_badsig_goodfirst).
func TestVerifyDetached_ABadSignatureFromAKnownKeyRejects(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	v, err := NewVerifier(VerifierConfig{Keyring: newKeyring(t, signer), RequireSignature: true, Logger: silentLogger()})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	release := []byte(fakeReleasePlaintext)
	good := detachSignWith(t, signer, release, false /*binary*/)
	bad := detachSignWith(t, signer, []byte("Origin: Other\n"), false)
	for name, packets := range map[string][]byte{
		"bad first":  joined(bad, good),
		"good first": joined(good, bad),
	} {
		got, err := v.VerifyDetached(context.Background(), newSuite(), release, armorSignature(t, packets))
		if !errors.Is(err, ErrBadSignature) || ClassifyVerifyErr(err) != "bad_signature" || got != nil {
			t.Errorf("%s: got %q, err %v (reason %q); want ErrBadSignature (bad_signature)", name, got, err, ClassifyVerifyErr(err))
		}
	}
}

// TestVerifyDetached_AKnownKeyOutsideThePinsStillCounts is the detached
// twin of TestVerifyInline_AKnownKeyOutsideThePinsStillCounts: S1's "known"
// is the host keyring, so C's bad signature beside A's good one is refused
// although the suite's pin names A only.
func TestVerifyDetached_AKnownKeyOutsideThePinsStillCounts(t *testing.T) {
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
	release := []byte(fakeReleasePlaintext)
	good := detachSignWith(t, a, release, false /*binary*/)
	// Premises: the pin trusts A for the suite and not C.
	if _, err := v.VerifyDetached(context.Background(), newSuite(), release, armorSignature(t, good)); err != nil {
		t.Fatalf("test premise: A's good signature alone: %v; want it accepted under the pin", err)
	}
	if _, err := v.VerifyDetached(context.Background(), newSuite(), release, detachSignWith(t, c, release, true)); !errors.Is(err, ErrUntrustedSigner) {
		t.Fatalf("test premise: C's good signature alone: %v; want ErrUntrustedSigner (C is outside the pin)", err)
	}
	cBad := detachSignWith(t, c, []byte("Origin: Other\n"), false)
	got, err := v.VerifyDetached(context.Background(), newSuite(), release, armorSignature(t, joined(good, cBad)))
	if !errors.Is(err, ErrBadSignature) || ClassifyVerifyErr(err) != "bad_signature" || got != nil {
		t.Fatalf("A's good signature and C's bad one: got %q, err %v (reason %q); want ErrBadSignature (bad_signature): C is in the host keyring",
			got, err, ClassifyVerifyErr(err))
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
// never propagated untouched into the pool. It was the bypass's
// parseable-signature guard's case while the decoder passed non-armored
// input through; since the shape checks (now layer T's T1: no BEGIN line,
// apt's NODATA) it is ErrMalformedSignature before any packet is parsed.
// This regression case was missed by the original
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
	if !errors.Is(err, ErrMalformedSignature) || ruleOf(err) != "T1" {
		t.Fatalf("err = %v (rule %q); want ErrMalformedSignature from T1 on raw non-armored garbage Release.gpg under accept_any_signer", err, ruleOf(err))
	}
}

// TestVerifyDetached_AcceptAnySigner_EmptyArmorStillRejected: an armor
// that holds no packet at all is refused in both modes by layer A (A4: no
// base64 line), as ErrMalformedSignature, before the trust set is read; the
// bypass's "at least one signature packet" is layer A's and P's (A4 decodes
// to one byte or more, P1 to one packet or more).
func TestVerifyDetached_AcceptAnySigner_EmptyArmorStillRejected(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	release := []byte(fakeReleasePlaintext)
	empty := []byte("-----BEGIN PGP SIGNATURE-----\n\n-----END PGP SIGNATURE-----\n")
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
		if !errors.Is(err, ErrMalformedSignature) || ruleOf(err) != "A4" || got != nil {
			t.Errorf("accept_any_signer=%v: got %q, err %v (rule %q); want ErrMalformedSignature from A4", anySigner, got, err, ruleOf(err))
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
	// "Garbage" inside an armor frame: layer T passes it (no line outside
	// the block, none inside starting with "-"), layer A refuses it (A2:
	// a line that is no armor header, and no empty line before a body).
	garbage := []byte("-----BEGIN PGP SIGNATURE-----\nthis is not a real signature\n-----END PGP SIGNATURE-----\n")

	_, err = v.VerifyDetached(context.Background(), newSuite(), release, garbage)
	if !errors.Is(err, ErrMalformedSignature) || ruleOf(err) != "A2" {
		t.Fatalf("err = %v (rule %q); want ErrMalformedSignature from A2 on garbage Release.gpg under accept_any_signer", err, ruleOf(err))
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

// TestMalformedSentinels_ClaimNoAptVerdict: the malformed_* sentinels'
// texts begin every structural refusal's log line, deviations included
// (shapes every apt accepts and the cache's canonical grammar refuses), so
// they must name what the cache refuses and claim nothing of apt (the Task
// 9c review's 9c-M10; the controller's ruling R-9c-1).
func TestMalformedSentinels_ClaimNoAptVerdict(t *testing.T) {
	for _, err := range []error{ErrMalformedClearsign, ErrMalformedSignature} {
		if regexp.MustCompile(`\bapt\b`).MatchString(err.Error()) {
			t.Errorf("%q claims an apt verdict; name what the cache refuses", err.Error())
		}
	}
}
