package gpg

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	pgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
)

// SignerPin is a parsed [[trusted_signer]] block. The HostRegex is
// pre-compiled at construction; the Fingerprints set is uppercase
// 40-char hex.
type SignerPin struct {
	HostRegex    *regexp.Regexp
	Fingerprints map[string]struct{}
}

// VerifierConfig bundles dependencies for NewVerifier.
type VerifierConfig struct {
	Keyring *Keyring
	Pins    []SignerPin
	// RequireSignature no longer affects verification (spec C5 rev 4); cmd
	// uses adoption.require_signature only for its empty-keyring startup gate.
	RequireSignature bool
	RequirePinned    bool

	// AllowShortKeyID controls the SPEC2 §7.6.3 fallback for signatures
	// that omit the IssuerFingerprint subpacket (subpacket 33) and
	// carry only the legacy 8-byte issuer keyid (subpacket 16). When
	// true (the default), the verifier looks up the keyid in the
	// per-suite trust set and, on unique match, verifies against that
	// entity — matching apt's own broad behavior. When false, signatures
	// without IssuerFingerprint are rejected with ErrShortKeyID
	// regardless of whether the keyid would match. The fallback
	// emits one INFO log per (host, suite_path) per process (and again
	// once its set has started over past onceSetMax suites) the first
	// time it accepts a signature, so operators can inventory
	// which repos rely on the legacy form.
	//
	// Real-world third-party signers (notably Docker, Microsoft) still
	// emit signatures without subpacket 33; default-true preserves
	// snapshot adoption for those repos while operators that prefer
	// the stricter policy set the toggle false.
	AllowShortKeyID bool

	// AcceptAnySigner relaxes the trust + cryptographic checks for
	// unpinned suites (SPEC2 §7.6.2 bypass branch). When true and no
	// [[trusted_signer]] block matches the suite, the verifier decodes
	// the clearsigned envelope (or Release.gpg) structurally and
	// returns the cleartext (or Release bytes verbatim in detached
	// mode) without consulting the host keyring. Pinned suites are
	// unaffected — the matched fingerprints remain authoritative and
	// the full trust + crypto path runs as before.
	//
	// This is the SPEC2 §5.1 adoption.accept_any_signer toggle. The
	// security argument is that the proxy stores and serves the
	// original signed bytes; apt clients on the fleet remain the
	// authoritative trust anchor via their own per-source Signed-By
	// rules.
	AcceptAnySigner bool

	Logger *slog.Logger
}

// Verifier implements freshness.Verifier using the host apt keyring
// narrowed per-suite by SPEC2 §7.6.2 trust-set rules. One Verifier
// instance is shared across all suites.
type Verifier struct {
	keyring         *Keyring
	pins            []SignerPin
	requirePinned   bool
	allowShortKeyID bool
	acceptAnySigner bool
	logger          *slog.Logger

	// shortKeyIDLogged tracks suites for which a short-keyid-fallback
	// acceptance has already been logged this process. Keyed by
	// "canonical_host|suite_path"; at most onceSetMax keys, then it starts
	// over (spec C9).
	shortKeyIDLogged onceSet

	// untrustedSignerLogged tracks suites for which an
	// adoption_untrusted_signer rejection has already been logged since
	// the set last started over (at most onceSetMax keys, as
	// shortKeyIDLogged). Same dedupe shape as shortKeyIDLogged so the log
	// surface is a finite inventory of "which (host, suite) pairs need new
	// keys" rather than a per-tick noise stream.
	untrustedSignerLogged onceSet

	// unverifiedSignerLogged tracks suites for which an
	// adoption_unverified_signer acceptance has already been logged since
	// the set last started over (at most onceSetMax keys). Fires under
	// accept_any_signer when the bypass branch runs. Same dedupe shape as
	// the other once-loggers.
	unverifiedSignerLogged onceSet
}

// Compile-time interface check: Verifier satisfies freshness.Verifier.
var _ freshness.Verifier = (*Verifier)(nil)

// NewVerifier validates dependencies and constructs a Verifier.
func NewVerifier(cfg VerifierConfig) (*Verifier, error) {
	if cfg.Keyring == nil {
		return nil, errors.New("gpg: nil Keyring")
	}
	for i, p := range cfg.Pins {
		if p.HostRegex == nil {
			return nil, fmt.Errorf("gpg: pins[%d] has nil HostRegex", i)
		}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Verifier{
		keyring:         cfg.Keyring,
		pins:            cfg.Pins,
		requirePinned:   cfg.RequirePinned,
		allowShortKeyID: cfg.AllowShortKeyID,
		acceptAnySigner: cfg.AcceptAnySigner,
		logger:          logger,
	}, nil
}

// ClassifyVerifyErr maps an error returned from VerifyInline /
// VerifyDetached (possibly wrapped by the adopter) to the SPEC5 §10.5
// `recent_adoptions[].reason` short tag. Returns "" when err is nil.
// Unknown errors that chain through ErrAdoptionGPGFailed without a
// recognized gpg sentinel return "crypto_verify_failed" — the only
// remaining route after the trust check is the cryptographic verify
// inside verifyAnyTrusted.
//
// freshness imports observability and depends on this function through
// an injected classifier so this gpg package remains the single owner
// of the gpg-sentinel → reason mapping.
func ClassifyVerifyErr(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, ErrUnpinnedSuite):
		return "unpinned_suite"
	case errors.Is(err, ErrMalformedClearsign):
		return "malformed_clearsign"
	case errors.Is(err, ErrMalformedSignature):
		return "malformed_signature"
	case errors.Is(err, ErrMissingSignature):
		return "missing_signature"
	case errors.Is(err, ErrShortKeyID):
		return "short_keyid"
	case errors.Is(err, ErrUntrustedSigner):
		return "untrusted_signer"
	case errors.Is(err, ErrAmbiguousKeyID):
		return "ambiguous_keyid"
	case errors.Is(err, ErrNoUsableSignature):
		return "no_usable_signature"
	case errors.Is(err, ErrBadSignature):
		return "bad_signature"
	}
	return "crypto_verify_failed"
}

// Sentinel errors returned by VerifyInline. Callers (the Adopter) wrap
// these into ErrAdoptionGPGFailed for the SPEC2 §10.2 categorized log.
var (
	// ErrUnpinnedSuite means RequirePinned is true and no
	// [[trusted_signer]] block matched the suite's canonical host.
	ErrUnpinnedSuite = errors.New("adoption_unpinned_suite")

	// ErrMissingSignature means the InRelease body carries no clearsign
	// marker at all. Since spec C5 rev 4 this is an error whatever
	// adoption.require_signature says: unsigned metadata is adopted only
	// through the explicit unsigned form (phase E), never as inline.
	ErrMissingSignature = errors.New("InRelease is not clearsigned")

	// ErrMalformedClearsign means the body carries the clearsign marker
	// but is not a decodable clearsigned message (no END line, no
	// signature frame, zero signed lines), or not one in the shape apt
	// accepts (F0–F6, armor_shape.go and packet_shape.go: a line after
	// the END line, a forged armor checksum, a damaged armor, packets apt
	// refuses). It is never treated as text.
	ErrMalformedClearsign = errors.New("InRelease clearsign envelope is malformed")

	// ErrMalformedSignature means a Release.gpg is not one ASCII-armored
	// signature block in the shape apt accepts (F0–F6, both forms' rules:
	// bytes before its BEGIN line — a binary signature included — or after
	// its END line, a damaged armor, a forged armor checksum, packets apt
	// refuses). The detached twin of ErrMalformedClearsign. Its text starts
	// with the file's name, capital R and all (the hardening plan's DN-4),
	// hence the ST1005 exemption.
	ErrMalformedSignature = errors.New("Release.gpg is not one ASCII-armored signature block apt accepts") //nolint:staticcheck // ST1005: starts with the file name Release.gpg

	// ErrShortKeyID means the signature lacks the long-form
	// IssuerFingerprint subpacket. SPEC2 §7.6.3 rejects short-keyid-
	// only signatures.
	ErrShortKeyID = errors.New("signature lacks IssuerFingerprint subpacket (long-form fingerprint required)")

	// ErrUntrustedSigner means the signature's IssuerFingerprint is
	// not in the per-suite trust set.
	ErrUntrustedSigner = errors.New("signing key not in trust set")

	// ErrNoUsableSignature means none of the signatures in the
	// clearsigned block had a valid IssuerFingerprint within the
	// trust set.
	ErrNoUsableSignature = errors.New("no signature with trusted IssuerFingerprint")

	// ErrAmbiguousKeyID means the short-keyid fallback found more
	// than one key in the per-suite trust set with the same 8-byte
	// keyid. Acceptance on the first match would let a colliding
	// key (an "Evil 32"-style adversarial fingerprint, or a
	// legitimate but redundant operator-staged duplicate) silently
	// substitute for the intended signer. Fail closed.
	ErrAmbiguousKeyID = errors.New("ambiguous short issuer keyid in trust set")

	// ErrBadSignature means a signature packet whose issuer, as gpgv reads
	// it, names a signing key in the host keyring failed its cryptographic
	// check, carried a critical notation go-crypto does not know, or
	// carried another issuer subpacket that contradicts it (S1,
	// requireKnownSignaturesVerify): gpgv's BADSIG, which makes apt refuse
	// the anchor even beside a good signature (the 2026-09 security
	// review's VC-1).
	ErrBadSignature = errors.New("a signature from a known key failed verification")
)

// errClearsignPanicked reports a clearsigned body that made the go-crypto
// decoder panic. It wraps ErrMalformedClearsign, so it classifies as
// malformed_clearsign: the body claims to be clearsigned and is not a
// valid clearsigned message.
var errClearsignPanicked = fmt.Errorf("%w (decoder panicked)", ErrMalformedClearsign)

// decodeClearsigned is clearsign.Decode with a panic turned into
// errClearsignPanicked. It decodes aptpath.DecodableClearsign(b), so a
// marker line with trailing blanks — accepted by apt — decodes too
// (ruling R-D2).
//
// AIDEV-NOTE: I-1. go-crypto v1.4.1 panics (slice bounds [:-1]) when the
// signed text has no lines. Every clearsign.Decode call on untrusted bytes
// in this package goes through this wrapper; the freshness goroutines
// also recover, as defence in depth.
func decodeClearsigned(b []byte) (block *clearsign.Block, err error) {
	defer func() {
		if r := recover(); r != nil {
			block, err = nil, fmt.Errorf("%w: %v", errClearsignPanicked, r)
		}
	}()
	block, _ = clearsign.Decode(aptpath.DecodableClearsign(b))
	return block, nil
}

// VerifyInline implements freshness.Verifier. Returns the verified
// Release-equivalent plaintext (the cleartext between BEGIN/END
// markers) on success.
//
// Order of checks (each fails closed):
//  1. Reject extraneous bytes around the clearsigned block.
//     clearsign.Decode silently discards prefix data, so we enforce
//     "exactly one clearsigned message: its first line is the BEGIN
//     marker (apt's rule, ruling R-D2 — no leading byte, whitespace
//     included), and the first END marker begins its last line" before
//     invoking the decoder (requireBareClearsigned: F1 and F0). Without
//     this, an adversary could prepend forged content to a real
//     InRelease; the verifier would happily verify the embedded message
//     while §7.5 step 2 stored the original (prefix-bearing) bytes as the
//     pool blob — a cache-pollution path.
//  2. Resolve per-suite trust set; if RequirePinned and no match, abort.
//  3. Decode the clearsigned block; a body without the marker is
//     ErrMissingSignature, one that does not decode is
//     ErrMalformedClearsign, in every mode.
//  4. Refuse the armor and packet shapes apt refuses (F2–F6,
//     requireInlineSignatureShape), on the bypass path too: the END
//     line's form, the armor checksum, the lines inside the armor,
//     signature packets only. ErrMalformedClearsign.
//  5. Refuse a signature from a key in the host keyring that does not
//     verify (S1, requireKnownSignaturesVerify), as apt's BADSIG makes
//     it refuse the anchor even beside a good signature: ErrBadSignature.
//     Not on the bypass path (the hardening plan's DN-8).
//  6. Iterate signature packets, attempting cryptographic verification
//     of each candidate (one with a trusted IssuerFingerprint)
//     against the trust-set EntityList. Accept only on a packet that
//     BOTH passes the long-form-fingerprint trust check AND verifies
//     cryptographically. Without this coupling, a multi-sig block
//     could satisfy the policy with one packet (decoy) and verify
//     a different packet (the actual cryptographic anchor).
//
// ctx is accepted for interface compatibility but VerifyInline does
// no I/O — verification is CPU-bound and short.
func (v *Verifier) VerifyInline(ctx context.Context, suite freshness.SuiteRef, inRelease []byte) ([]byte, error) {
	_ = ctx

	// Step 1: structural input guard — no prefix or suffix data
	// around the clearsigned block (F1, F0). requireBareClearsigned
	// returns nil when no BEGIN marker is present, so plain-text bodies
	// reach Step 3, which rejects them with ErrMissingSignature.
	if err := requireBareClearsigned(inRelease); err != nil {
		return nil, err
	}

	// Step 2: per-suite trust-set resolution.
	trustSet, trustFPs, pinned, bypass, err := v.resolveTrustSet(suite)
	if err != nil {
		return nil, err
	}

	// Step 3: clearsign decode. Only a decodable clearsigned envelope is
	// inline metadata, whatever require_signature says (spec C5 rev 4).
	//
	// AIDEV-NOTE: plan review finding 4. The old require_signature=false
	// branch returned the raw body here, which let a plain-text or
	// undecodable "InRelease" become a trust-bearing inline snapshot.
	// Never reintroduce a text fallback: unsigned repositories go through
	// the explicit unsigned form, which never vouches.
	block, err := decodeClearsigned(inRelease)
	if err != nil {
		return nil, err
	}
	if block == nil {
		if bytes.Contains(inRelease, []byte(clearsignBeginMarker)) {
			return nil, ErrMalformedClearsign
		}
		return nil, ErrMissingSignature
	}

	if bypass {
		// SPEC2 §7.6.2 bypass: clearsign envelope decoded structurally;
		// trust + crypto check skipped. The original signed inRelease
		// bytes are what callers will persist and serve to apt clients,
		// so per-source Signed-By rules on the fleet remain the
		// authoritative trust anchor.
		//
		// AIDEV-NOTE: S1 (requireKnownSignaturesVerify) is deliberately
		// not applied here, nor on VerifyDetached's bypass: the bypass
		// verifies no signature, and S1 would be its only signature check
		// (the hardening plan's DN-8, recorded Open). Do not add it here
		// without that decision.
		//
		// clearsign.Decode proved a BEGIN/END PGP SIGNATURE frame is
		// present, but armor.Decode (lazy, on Body read) and the
		// armored body itself can still contain no parseable signature
		// packet. Require at least one *packet.Signature before
		// accepting so the relaxation does not silently adopt
		// clearsigned envelopes whose armored body is garbage.
		sigBytes, err := readArmoredSignature(block.ArmoredSignature.Body)
		if err != nil {
			return nil, fmt.Errorf("read signature: %w", err)
		}
		// Step 4 on the bypass path: F2–F6 are structural, so the
		// relaxation of trust leaves them in force (DN-3).
		if err := requireInlineSignatureShape(inRelease, sigBytes); err != nil {
			return nil, err
		}
		// F6 passes an armor with no packet at all: this refuses it
		// (TestVerifyInline_AcceptAnySigner_EmptyArmorStillRejected).
		if err := requireParseableSignature(bytes.NewReader(sigBytes)); err != nil {
			return nil, fmt.Errorf("clearsigned signature body: %w", err)
		}
		v.logUnverifiedSignerOnce(suite, bytes.NewReader(sigBytes))
		return block.Plaintext, nil
	}

	if len(trustSet) == 0 {
		// No key can verify the signed metadata — fail closed. Unpinned:
		// the host keyring is empty (require_signature = false admits
		// that at startup). Pinned: the keyring holds none of the
		// pinned fingerprints (we already passed the RequirePinned
		// gate, so this is "configured but un-loadable").
		if !pinned {
			return nil, fmt.Errorf("%w: the host keyring is empty — signed suites cannot adopt until keys are installed or adoption.accept_any_signer is set", ErrNoUsableSignature)
		}
		return nil, fmt.Errorf("%w: pin matched but no key in host keyring satisfies it", ErrNoUsableSignature)
	}

	sigBytes, err := readArmoredSignature(block.ArmoredSignature.Body)
	if err != nil {
		return nil, fmt.Errorf("read signature: %w", err)
	}
	// Step 4: the armor and packet shapes apt refuses (F2–F6).
	if err := requireInlineSignatureShape(inRelease, sigBytes); err != nil {
		return nil, err
	}
	// Step 5: every signature from a key in the host keyring verifies (S1;
	// F6 has left signature packets only).
	if err := v.requireKnownSignaturesVerify(block.Bytes, sigBytes); err != nil {
		return nil, err
	}
	// Step 6: per-packet verify-and-trust loop. A signature packet is
	// accepted iff it carries a long-form IssuerFingerprint that is
	// in trustFPs AND verifies cryptographically against trustSet.
	if err := v.verifyAnyTrusted(suite, trustSet, trustFPs, block.Bytes, sigBytes); err != nil {
		return nil, err
	}

	return block.Plaintext, nil
}

// VerifyDetached verifies a detached signature (Release.gpg bytes)
// against the given Release bytes. Returns releaseBytes verbatim on
// success — there is no "extracted plaintext" the way clearsign has;
// the Release file IS the verified plaintext.
//
// sigBytes must be one ASCII-armored signature block, as apt-ftparchive's
// recommended workflow emits it (`gpg --detach-sign --armor`), in the
// shape apt accepts: its first line is the BEGIN line, nothing follows
// its END line, and its armor and packets pass F0–F6 (armor_shape.go,
// packet_shape.go). A binary Release.gpg is refused, as apt 2.2 to 3.2
// refuse it ("unsupported binary format"). Each refusal is
// ErrMalformedSignature.
//
// Order of checks (each fails closed):
//  1. Empty inputs are a programming error in the caller (the
//     freshness checker only routes here when both Release and
//     Release.gpg fetched non-empty bodies).
//  2. Resolve per-suite trust set; if RequirePinned and no match, abort.
//  3. De-armor sigBytes, refusing the shapes apt refuses (F1, F0, F2–F6:
//     decodeMaybeArmoredSignature), on the bypass path too.
//  4. Refuse a signature from a key in the host keyring that does not
//     verify (S1, requireKnownSignaturesVerify: ErrBadSignature), as
//     VerifyInline does; not on the bypass path (DN-8).
//  5. Iterate signature packets, accepting on the first packet that
//     BOTH passes the long-form-fingerprint trust check AND verifies
//     cryptographically against trustSet — same per-packet coupling
//     as VerifyInline (SPEC2 §7.6.3).
//
// As in VerifyInline, a missing signature never passes through: a
// detached path that arrived without Release.gpg never reaches this
// function (the freshness checker only switches to detached mode
// after fetching both bodies).
//
// AIDEV-NOTE: the detached form's "bare armor" guard, the twin of
// requireBareClearsigned's. Until the 2026-10-28 hardening plan this
// deliberately enforced none — armor.Decode strips bytes around the
// armor, and the stored bytes are what apt clients re-verify — and so
// adopted what apt refuses (the 2026-09 security review's VC-2: bytes or
// lines around the armor, two concatenated armor blocks, a binary
// Release.gpg; C-4: a forged `=CRC` line), and every client of the suite
// then failed `apt update`. Enforced now (the plan's DN-3), by
// decodeMaybeArmoredSignature on the bypass path too: the BEGIN line at
// byte 0 (F1: a binary signature fails it), the first END marker
// beginning the last line (F0), the END line's form (F2), the armor
// checksum (F3), no "-" line inside the armor (F4), headers with ": "
// (F5), signature packets only (F6). Besides that: both bodies
// non-empty, the suite's trust set (pins first), every signature from a
// key in the host keyring verifying (S1: a bad one beside a good one, the
// security review's VC-1, is refused as apt's BADSIG refuses it; enforced
// since the hardening plan's Task 7, on the trusted path only: DN-8), and
// one signature packet that passes the trust check and verifies — except
// under accept_any_signer with no pin matched, where one parseable
// signature packet is enough (SPEC2 §7.6.2's bypass, below). Still open:
// weak digests or keys (C-3: VerifyDetachedSignature runs with a nil
// config, no algorithm policy, so SHA-1 and RSA-1024 pass).
func (v *Verifier) VerifyDetached(ctx context.Context, suite freshness.SuiteRef, releaseBytes, sigBytes []byte) ([]byte, error) {
	_ = ctx

	if len(releaseBytes) == 0 {
		return nil, errors.New("empty Release body")
	}
	if len(sigBytes) == 0 {
		return nil, errors.New("empty Release.gpg body")
	}

	trustSet, trustFPs, pinned, bypass, err := v.resolveTrustSet(suite)
	if err != nil {
		return nil, err
	}

	if bypass {
		// SPEC2 §7.6.2 bypass: Release.gpg must still parse as a PGP
		// signature blob (cheap structural guard against garbage that
		// would otherwise propagate untouched into the pool), but
		// trust + crypto verification is skipped. Release bytes are
		// returned verbatim, matching the verified-bytes-equal-stored-
		// bytes invariant.
		//
		// decodeMaybeArmoredSignature refuses the shapes apt refuses
		// (F1, F0, F2–F6: structural, so in force on this path too),
		// and requireParseableSignature adds that the armor holds at
		// least one OpenPGP signature packet — F6 alone passes an armor
		// with no packet at all. S1 is not applied here (DN-8: the
		// AIDEV-NOTE in VerifyInline's bypass).
		binarySig, err := decodeMaybeArmoredSignature(sigBytes)
		if err != nil {
			return nil, fmt.Errorf("decode signature: %w", err)
		}
		if err := requireParseableSignature(bytes.NewReader(binarySig)); err != nil {
			return nil, fmt.Errorf("parse Release.gpg signature body: %w", err)
		}
		v.logUnverifiedSignerOnce(suite, bytes.NewReader(binarySig))
		return releaseBytes, nil
	}

	if len(trustSet) == 0 {
		if !pinned {
			return nil, fmt.Errorf("%w: the host keyring is empty — signed suites cannot adopt until keys are installed or adoption.accept_any_signer is set", ErrNoUsableSignature)
		}
		return nil, fmt.Errorf("%w: pin matched but no key in host keyring satisfies it", ErrNoUsableSignature)
	}

	binarySig, err := decodeMaybeArmoredSignature(sigBytes)
	if err != nil {
		return nil, fmt.Errorf("decode signature: %w", err)
	}

	// Every signature from a key in the host keyring verifies (S1; F6, in
	// decodeMaybeArmoredSignature, has left signature packets only).
	if err := v.requireKnownSignaturesVerify(releaseBytes, binarySig); err != nil {
		return nil, err
	}
	if err := v.verifyAnyTrusted(suite, trustSet, trustFPs, releaseBytes, binarySig); err != nil {
		return nil, err
	}
	return releaseBytes, nil
}

// decodeMaybeArmoredSignature returns the binary signature packet
// bytes of a Release.gpg that is one ASCII-armored signature block in
// the shape apt accepts, and refuses anything else with
// ErrMalformedSignature: a first line that is not the BEGIN line (F1 —
// bytes before the armor, or a binary signature), an armor go-crypto
// cannot decode or of another type, and the F0, F2–F5 and F6 failures.
// (The name is older than the refusal of binary signatures, which it
// used to return as they were.)
//
// Caps the binary output at 64 KiB — real Release.gpg signatures are
// well under 1 KiB; the cap bounds memory if a hostile upstream feeds
// a giant fake signature. A body that does not de-armor (illegal
// base64) or runs past the cap keeps its plain error, as an InRelease's
// does in readArmoredSignature.
func decodeMaybeArmoredSignature(b []byte) ([]byte, error) {
	const maxBinary = 64 << 10
	if !isArmoredSignature(b) {
		return nil, fmt.Errorf("%w: its first line is not %q followed only by blanks (bytes before the armor, or a binary signature, which apt refuses)", ErrMalformedSignature, sigArmorBegin)
	}
	block, err := armor.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("%w: armor decode: %v", ErrMalformedSignature, err)
	}
	if block.Type != "PGP SIGNATURE" {
		return nil, fmt.Errorf("%w: unexpected armor type %q (want \"PGP SIGNATURE\")", ErrMalformedSignature, block.Type)
	}
	out, err := io.ReadAll(io.LimitReader(block.Body, int64(maxBinary)+1))
	if err != nil {
		return nil, err
	}
	if len(out) > maxBinary {
		return nil, fmt.Errorf("signature too large (>%d bytes after de-armor)", maxBinary)
	}
	// F0 and F2–F5 on the raw bytes beside the decode, then F6 on what
	// it decoded (armor_shape.go's AIDEV-NOTE: go-crypto's armor reader
	// skips what apt refuses). The armor's BEGIN line is line 0 (F1).
	lines := splitLines(b)
	if err := requireEndMarkerBeginsLastLine(b, lines); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedSignature, err)
	}
	if err := checkSignatureArmor(b, lines, 0, out); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedSignature, err)
	}
	if err := requireSignaturePacketsOnly(out); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedSignature, err)
	}
	return out, nil
}

// isArmoredSignature is F1 for the detached form: b's first line is the
// armor's BEGIN line, "-----BEGIN PGP SIGNATURE-----" followed only by
// spaces, tabs and CRs, with nothing before it — no leading whitespace or
// blank line (d_lead_ws, d_lead_nl), no BOM (x3d_bom), and so no binary
// signature, which apt 2.2 to 3.2 refuse (d_binary).
func isArmoredSignature(b []byte) bool {
	first := b
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		first = b[:i]
	}
	return isMarkerLine(first, sigArmorBegin)
}

// clearsignBeginMarker opens a clearsigned message (deferred minor A-50:
// one literal).
const clearsignBeginMarker = "-----BEGIN PGP SIGNED MESSAGE-----"

// requireBareClearsigned enforces apt's shape on a body that carries the
// clearsign marker: its FIRST line is exactly the marker (trailing CR,
// space and tab ignored — aptpath.SniffInRelease, apt's
// StartsWithGPGClearTextSignature: F1), and the first END marker in the
// body begins its last line (F0: after the END line nothing, or one LF).
// clearsign.Decode silently strips bytes around the message; we reject
// them to keep "verified bytes == stored bytes" — SPEC2 §7.5 step 2. Both
// failures are ErrMalformedClearsign (deferred minor A-51).
//
// AIDEV-NOTE: ruling R-D2 (phase D). A snapshot anchor is served to apt
// clients, which reject an InRelease whose first line is not the marker
// (leading whitespace or a blank line included), so adoption refuses that
// too — the first-line rule the handler's sniff applies before a body
// earns a suite row or a signed-seen marker (F1). The 2026-09 security
// review found shapes apt rejects that this adopted: VC-2, any line after
// the END line, an empty one included (this guard let whitespace through
// there until the 2026-10-28 hardening plan), and C-4, a forged `=CRC`
// line. Enforced for the inline form now (the plan's DN-3, F0–F6, on the
// bypass path too): that first line (F1); the first END marker beginning
// the last line (F0, here: no line after the END line, no dash-escaped END
// in the signed text); a decodable envelope; after the decode, the armor
// and packet shapes apt accepts (requireInlineSignatureShape: the END
// line's form, no text after its marker, F2; the armor checksum F3; no "-"
// line inside the armor F4; headers with ": " F5; signature packets only
// F6); every signature from a key in the host keyring verifying (S1,
// requireKnownSignaturesVerify: VC-1, a bad signature beside a good one,
// enforced since the plan's Task 7, on the trusted path only: DN-8); and
// one trusted signature packet that verifies (under accept_any_signer with
// no pin matched, one parseable signature packet: SPEC2 §7.6.2's bypass).
// Still open: C-3 (no algorithm policy: a SHA-1 digest or an RSA-1024 key
// verifies).
//
// A body without the marker returns nil: the decode step rejects it with
// ErrMissingSignature. So does one without an END marker: the decoder
// then reports it.
func requireBareClearsigned(b []byte) error {
	if !bytes.Contains(b, []byte(clearsignBeginMarker)) {
		return nil
	}
	if !aptpath.SniffInRelease(b) {
		return fmt.Errorf("%w: the first line is not the BEGIN marker (apt rejects it)", ErrMalformedClearsign)
	}
	if !bytes.Contains(b, []byte(sigArmorEnd)) {
		// Missing END marker — let clearsign.Decode produce the real
		// error. Don't fabricate one here.
		return nil
	}
	if err := requireEndMarkerBeginsLastLine(b, splitLines(b)); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformedClearsign, err)
	}
	return nil
}

// requireInlineSignatureShape applies F2–F6 to an InRelease whose envelope
// decoded (requireBareClearsigned applied F1 and F0 before the decode):
// F2–F5 to the armor that follows the signed text, F6 to sigBytes, the
// signature packets the decoder read from it. A failure is
// ErrMalformedClearsign.
//
// The armor's BEGIN line is the first line from line 1 on that is
// "-----BEGIN PGP SIGNATURE-----" followed only by spaces, tabs and CRs,
// as apt reads it. go-crypto ends the signed text at the first line that
// is exactly the marker (one CR stripped) and takes a line with trailing
// blanks as text, since dash-escaping is optional to it; such an
// unescaped line in the signed text is found here first, and F4 then
// refuses the real BEGIN line inside the armor it opens.
// aptpath.DecodableClearsign rewrites only line 0, so these line indexes
// on inRelease are the decoder's too.
func requireInlineSignatureShape(inRelease, sigBytes []byte) error {
	lines := splitLines(inRelease)
	begin := -1
	for i := 1; i < len(lines); i++ {
		if isMarkerLine(lineAt(inRelease, lines, i), sigArmorBegin) {
			begin = i
			break
		}
	}
	if begin < 0 {
		// The decoder found an armor, at a line this search accepts too:
		// unreachable, but never assumed.
		return fmt.Errorf("%w: no signature BEGIN line", ErrMalformedClearsign)
	}
	if err := checkSignatureArmor(inRelease, lines, begin, sigBytes); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformedClearsign, err)
	}
	if err := requireSignaturePacketsOnly(sigBytes); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformedClearsign, err)
	}
	return nil
}

// resolveTrustSet implements SPEC2 §7.6.2. Returns:
//   - trustSet: the openpgp.EntityList to verify against (nil under bypass)
//   - trustFPs: the uppercase-fingerprint set (for IssuerFingerprint check;
//     nil under bypass)
//   - pinned:   true iff at least one [[trusted_signer]] block matched
//   - bypass:   true iff acceptAnySigner is on AND no pin matched — the
//     caller must skip the trust + cryptographic verification entirely
//     and accept the body after a structural decode. requirePinned is
//     evaluated first, so a require_pinned_signer + accept_any_signer
//     combination still fails-closed on unpinned suites (pins remain a
//     hard requirement; the relaxation only governs verification within
//     the broad-trust branch).
//   - err:      ErrUnpinnedSuite when RequirePinned and no pin matched
func (v *Verifier) resolveTrustSet(suite freshness.SuiteRef) (openpgp.EntityList, map[string]struct{}, bool, bool, error) {
	var matched []SignerPin
	for _, p := range v.pins {
		if p.HostRegex.MatchString(suite.CanonicalHost) {
			matched = append(matched, p)
		}
	}
	if len(matched) == 0 {
		if v.requirePinned {
			v.logger.Warn("adoption_unpinned_suite",
				"canonical_host", suite.CanonicalHost,
				"suite_path", suite.SuitePath,
			)
			// Multi-wrap with freshness.ErrAdoptionUnpinnedSuite
			// so the freshness-layer classifier can route this
			// to acu_adoption_total{outcome=unpinned_suite}
			// (SPEC5 §10.4.3) without depending on gpg.
			// errors.Is(err, gpg.ErrUnpinnedSuite) still matches
			// for any caller that prefers the gpg-package
			// sentinel.
			return nil, nil, false, false, fmt.Errorf("%w: %w: no [[trusted_signer]] block matches %q",
				ErrUnpinnedSuite, freshness.ErrAdoptionUnpinnedSuite, suite.CanonicalHost)
		}
		if v.acceptAnySigner {
			// SPEC2 §7.6.2 bypass branch. Caller short-circuits the
			// trust + crypto check; only the structural decode
			// remains. trustSet / trustFPs are nil because they have
			// no consumer on this path.
			return nil, nil, false, true, nil
		}
		// Broad trust: the entire host keyring. Build a copy of the
		// host fingerprint set so the IssuerFingerprint check has
		// something to consult.
		fps := make(map[string]struct{}, len(v.keyring.fingerprints))
		for fp := range v.keyring.fingerprints {
			fps[fp] = struct{}{}
		}
		return v.keyring.EntityList(), fps, false, false, nil
	}
	union := make(map[string]struct{})
	for _, m := range matched {
		for fp := range m.Fingerprints {
			union[fp] = struct{}{}
		}
	}
	subset := v.keyring.Subset(union)
	return subset, union, true, false, nil
}

// readArmoredSignature consumes the (already-de-armored) reader from
// clearsign.Block.ArmoredSignature.Body and returns the binary
// signature packet bytes.
func readArmoredSignature(r io.Reader) ([]byte, error) {
	const max = 64 << 10 // signatures are tiny — 64 KiB is generous
	limited := io.LimitReader(r, max+1)
	out, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(out) > max {
		return nil, fmt.Errorf("signature too large (>%d bytes)", max)
	}
	return out, nil
}

// requireKnownSignaturesVerify applies apt's rule that a bad signature from a
// known key is fatal (VC-1; rulings HD-3, HD-8, HD-14): every signature
// packet whose issuer, read as the strictest supported gpgv reads it
// (s1Issuer), names a signing key in the host keyring must pass its
// cryptographic check against one of the keyring's entities holding that key
// ID, and the issuer go-crypto verifies by must not contradict it. A packet
// whose check passes but whose key or signature is expired or revoked passes
// (gpgv's EXPKEYSIG and REVKEYSIG, which apt ignores beside a good
// signature); an unknown critical notation fails (gpgv's BADSIG); a packet
// whose issuer the keyring lacks is ignored (gpgv's NO_PUBKEY).
//
// AIDEV-NOTE: go-crypto takes the issuer from the last issuer subpacket it
// parses, unhashed after hashed, and looks keys up by that key ID only
// (sig.IssuerKeyId); gpgv reads another subpacket (s1Issuer's AIDEV-NOTE).
// So "known" is S1's issuer, and a packet whose sig.IssuerKeyId differs
// from it fails — else an on-path attacker rewrites a bad signature's
// issuer subpackets and it is ignored here, or verified by another key,
// while every gpgv-based apt reports BADSIG (xi_badsig_issuer_rewritten,
// TestVerifyInline_AnUnhashedIssuerContradictingTheHashedOneFailsS1).
// openpgp.VerifyDetachedSignature returns a non-nil signature exactly when
// the cryptographic check passed, then reports expiry, revocation or an
// unknown critical notation as its error (go-crypto v1.4.1 openpgp/read.go,
// verifyDetachedSignature and checkMessageSignatureDetails). Each candidate
// entity is tried alone: the library hashes once and tries every key with
// that one hash, which only the first candidate can pass. The host keyring,
// not the suite's trust set: a client that installed any of the cache's keys
// sees the BADSIG. Re-check all of this when go-crypto is upgraded (the
// table test and this file's tests pin it).
func (v *Verifier) requireKnownSignaturesVerify(signed, sigBytes []byte) error {
	keyring := v.keyring.EntityList()
	reader := packet.NewReader(bytes.NewReader(sigBytes))
	for {
		pkt, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read signature packet: %w", err)
		}
		sig, ok := pkt.(*packet.Signature)
		if !ok {
			continue // unreachable: F6 has refused every other packet
		}
		// AIDEV-NOTE: S1 verifies the packet, and reads its unhashed area,
		// as go-crypto's Serialize writes it again, not as it came: the
		// hashed area as it came (sig.HashSuffix) and the unhashed
		// subpackets in their order, but the packet's length declared from
		// every subpacket's length in its shortest form. A packet whose
		// hashed area writes a length in more octets than it needs thus
		// comes out framed wrong, and fails here although gpgv verifies it:
		// strict, not unsafe (nothing passes that should fail); S2
		// (verifyAnyTrusted, verifyAgainst) shares it; no signer writes such
		// lengths.
		var one bytes.Buffer
		if err := sig.Serialize(&one); err != nil {
			return fmt.Errorf("re-serialize signature packet: %w", err)
		}
		id, keys := s1Issuer(keyring, sig, one.Bytes())
		if len(keys) == 0 {
			continue
		}
		if sig.IssuerKeyId == nil || *sig.IssuerKeyId != id {
			return fmt.Errorf("%w: issuer %016X: another issuer subpacket contradicts it", ErrBadSignature, id)
		}
		passed := false
		tried := make(map[*openpgp.Entity]bool, len(keys))
		for _, k := range keys {
			if tried[k.Entity] {
				continue
			}
			tried[k.Entity] = true
			got, _, verr := openpgp.VerifyDetachedSignature(openpgp.EntityList{k.Entity},
				bytes.NewReader(signed), bytes.NewReader(one.Bytes()), nil)
			if got != nil && !errors.As(verr, new(pgperrors.SignatureError)) {
				passed = true
				break
			}
		}
		if !passed {
			return fmt.Errorf("%w: issuer %016X", ErrBadSignature, id)
		}
	}
}

// s1Issuer returns the key ID S1 checks sig against and the host keyring's
// signing keys that hold it — none when sig names no key the keyring holds
// (gpgv's NO_PUBKEY: S1 ignores the packet) — reading sig's issuer as the
// strictest supported gpgv reads it, in its order:
//  1. the first hashed issuer fingerprint, in the signature's version form
//     (hashedIssuer), if the keyring holds its key ID;
//  2. else the first issuer key ID, hashed then unhashed
//     (firstIssuerKeyID), if the keyring holds it;
//  3. else, when neither names an issuer, go-crypto's sig.IssuerKeyId.
//
// serialized is sig as go-crypto's Serialize writes it.
//
// AIDEV-NOTE: ruling HD-14, measured on the corpus' batch fx9. The cache
// refuses what any supported apt refuses (HD-2), so S1 reads the issuer as
// the strictest supported gpgv does. gpgv looks the key up by the first
// hashed issuer fingerprint (33) when it is in its version's form, and else
// by the first issuer key ID (16), hashed then unhashed: gpgv 2.4 when there
// is no such fingerprint, gpgv 2.2 also when the fingerprint names a key it
// lacks (debian:11, ubuntu:22.04 and debian:12 refuse t7g6 and t7g6b; 2.4
// reports NO_PUBKEY). It reads the first matching subpacket at each step:
// a 33 in another version's form names no key (t7g4), a later 33 is not
// read (t7g7), nor a second 16 (t7g3 refused, as by every gpgv-based apt;
// t7g3b adopted, as by every apt). go-crypto's sig.IssuerKeyId is instead
// the last issuer subpacket it parsed, a 33 of any form or a 16. Read so,
// a forged packet whose last issuer subpacket names an unknown key would be
// ignored here while the one gpgv reads names a known key and every
// gpgv-based apt reports BADSIG (xi_badsig_issuer_rewritten,
// x3i_badsig_unhashed_fpr_added: the hashed fingerprint's; t7g1, t7g3: the
// first 16's). t7g4 and t7g7 are refused through their 16 either way —
// the form and first-33 rules leave step 1 empty, the 2.2 fallback passes
// its unknown fingerprint on — so the table shows neither rule's loss
// alone: TestHashedIssuer pins step 1's. Step 3 takes go-crypto's issuer
// where gpgv attributes the packet to no key: strict, a forged packet only
// (t7g7b, a listed deviation). Whatever S1 reads, go-crypto verifies by
// sig.IssuerKeyId, so requireKnownSignaturesVerify fails a packet whose
// IssuerKeyId differs.
func s1Issuer(keyring openpgp.EntityList, sig *packet.Signature, serialized []byte) (uint64, []openpgp.Key) {
	named := false
	if id, ok := hashedIssuer(sig); ok {
		if keys := keyring.KeysByIdUsage(id, packet.KeyFlagSign); len(keys) > 0 {
			return id, keys
		}
		named = true // gpgv 2.2 then looks the key up by the first 16
	}
	if id, ok := firstIssuerKeyID(sig, serialized); ok {
		if keys := keyring.KeysByIdUsage(id, packet.KeyFlagSign); len(keys) > 0 {
			return id, keys
		}
		named = true
	}
	if !named && sig.IssuerKeyId != nil {
		id := *sig.IssuerKeyId
		return id, keyring.KeysByIdUsage(id, packet.KeyFlagSign)
	}
	return 0, nil
}

// hashedIssuer returns the key ID of sig's first hashed issuer fingerprint
// (subpacket 33) when that subpacket is in the signature's version form — a
// v4 fingerprint (version 4, 20 bytes) in a v4 signature, its key ID bytes
// 12–19; a v6 one (version 6, 32 bytes) in a v6 signature, bytes 0–7 — and
// false when the hashed area has no 33 or its first is in another form (a
// later 33 is never read, as by gpgv). S1's step 1 (s1Issuer). The area is
// read from sig.HashSuffix, where go-crypto keeps it verbatim (hashedArea).
func hashedIssuer(sig *packet.Signature) (id uint64, ok bool) {
	area, have := hashedArea(sig)
	if !have {
		return 0, false
	}
	seen33 := false
	for len(area) > 0 {
		typ, data, rest, valid := nextSubpacket(area)
		if !valid {
			return 0, false
		}
		if typ == 33 && !seen33 {
			switch {
			case sig.Version == 4 && len(data) == 21 && data[0] == 4:
				id, ok = binary.BigEndian.Uint64(data[13:21]), true
			case sig.Version == 6 && len(data) == 33 && data[0] == 6:
				id, ok = binary.BigEndian.Uint64(data[1:9]), true
			}
			seen33 = true // gpgv reads the first 33 only
		}
		area = rest
	}
	return id, ok
}

// firstIssuerKeyID returns sig's first issuer key ID (subpacket 16): the
// hashed area's, else the unhashed area's, read from serialized (sig as
// go-crypto's Serialize writes it: unhashedArea), as gpgv reads it. S1's
// step 2 (s1Issuer).
//
// AIDEV-NOTE: the first 16 of an area counts, as in gpgv (whose
// parse_sig_subpkt returns the first match): of two the first names the
// issuer, so a forged packet that puts a known key's ID before an unknown
// one is still known here, and then contradicts sig.IssuerKeyId, the last
// (t7g3). The security review's prototype of S1 kept the last 16; no real
// signer writes two.
func firstIssuerKeyID(sig *packet.Signature, serialized []byte) (uint64, bool) {
	if area, ok := hashedArea(sig); ok {
		if id, found := firstKeyID(area); found {
			return id, true
		}
	}
	if area, ok := unhashedArea(sig.Version, serialized); ok {
		return firstKeyID(area)
	}
	return 0, false
}

// firstKeyID returns the first issuer key ID subpacket (16) of a subpacket
// area, false when it has none or does not parse.
func firstKeyID(area []byte) (id uint64, found bool) {
	for len(area) > 0 {
		typ, data, rest, valid := nextSubpacket(area)
		if !valid {
			return 0, false
		}
		if typ == 16 && len(data) == 8 && !found {
			id, found = binary.BigEndian.Uint64(data), true
		}
		area = rest
	}
	return id, found
}

// hashedArea returns sig's hashed subpacket area, which go-crypto keeps
// verbatim in sig.HashSuffix: bytes [6:6+len] of a v4 signature's, [8:8+len]
// of a v6's; false for another version or a suffix too short.
func hashedArea(sig *packet.Signature) ([]byte, bool) {
	hs := sig.HashSuffix
	switch {
	case sig.Version == 4 && len(hs) >= 6:
		if n := int(binary.BigEndian.Uint16(hs[4:6])); n <= len(hs)-6 {
			return hs[6 : 6+n], true
		}
	case sig.Version == 6 && len(hs) >= 8:
		if n := uint64(binary.BigEndian.Uint32(hs[4:8])); n <= uint64(len(hs)-8) {
			return hs[8 : 8+int(n)], true
		}
	}
	return nil, false
}

// unhashedArea returns the unhashed subpacket area of serialized, one v4 or
// v6 signature packet as go-crypto's Serialize writes it: a new-format
// header, then the body — version, type and algorithms, the hashed area's
// length (two octets; four in v6) and the area, then the unhashed area's
// length and the area. false when serialized is not so.
func unhashedArea(version int, serialized []byte) ([]byte, bool) {
	if len(serialized) < 2 || serialized[0] != 0xc0|packetTagSignature {
		return nil, false
	}
	var header int
	switch l0 := serialized[1]; {
	case l0 < 192:
		header = 2
	case l0 < 224:
		header = 3
	case l0 == 255:
		header = 6
	default:
		return nil, false // a partial length: go-crypto never writes one
	}
	lenOctets := 2
	if version == 6 {
		lenOctets = 4
	}
	if len(serialized) < header+4 || serialized[header] != byte(version) {
		return nil, false
	}
	body := serialized[header:]
	at := 4
	area := func() ([]byte, bool) { // the area whose length is at body[at:]
		if len(body) < at+lenOctets {
			return nil, false
		}
		var n uint64
		for _, b := range body[at : at+lenOctets] {
			n = n<<8 | uint64(b)
		}
		at += lenOctets
		if n > uint64(len(body)-at) {
			return nil, false
		}
		a := body[at : at+int(n)]
		at += int(n)
		return a, true
	}
	if _, ok := area(); !ok { // the hashed area
		return nil, false
	}
	return area()
}

// nextSubpacket cuts the first subpacket off a subpacket area: its type (the
// critical bit masked), its data and the rest of the area; valid is false
// when the area does not start with a whole subpacket (go-crypto's own
// parse, which F6 requires, refuses such an area first).
func nextSubpacket(area []byte) (typ byte, data, rest []byte, valid bool) {
	// The length in one, two or five octets (RFC 9580 §5.2.3.7), then the
	// type octet and the data.
	var n, hl int
	switch l0 := int(area[0]); {
	case l0 < 192:
		n, hl = l0, 1
	case l0 < 255:
		if len(area) < 2 {
			return 0, nil, nil, false
		}
		n, hl = (l0-192)<<8+int(area[1])+192, 2
	default:
		if len(area) < 5 {
			return 0, nil, nil, false
		}
		l := uint64(binary.BigEndian.Uint32(area[1:5]))
		if l > uint64(len(area)) {
			return 0, nil, nil, false
		}
		n, hl = int(l), 5
	}
	if n == 0 || n > len(area)-hl {
		return 0, nil, nil, false
	}
	return area[hl] & 0x7f, area[hl+1 : hl+n], area[hl+n:], true
}

// verifyAnyTrusted enumerates every signature packet in sigBytes and
// accepts on the FIRST one that passes BOTH (a) a trust check —
// long-form IssuerFingerprint in trustFPs, or (when AllowShortKeyID
// is enabled) the legacy 8-byte issuer keyid maps to a single entity
// in trustSet — AND (b) cryptographic verification against the
// matched entity. Coupling these checks to the same packet is the
// SPEC2 §7.6.3 invariant: a multi-signature block must not satisfy
// the policy with one packet while the library's verifier accepts a
// different packet.
//
// Error precedence when no packet is accepted:
//   - If at least one packet's IssuerFingerprint was trusted but
//     none verified, return the last underlying verification error.
//   - Else if at least one trusted-fingerprint packet existed but a
//     candidate failed cryptographically with no usable alternative,
//     return that verification error.
//   - Else if all packets had IssuerFingerprint outside trustFPs,
//     return ErrUntrustedSigner.
//   - Else if all packets lacked IssuerFingerprint AND AllowShortKeyID
//     is disabled, return ErrShortKeyID.
//   - Else (no signature packets at all, or short-keyid fallback ran
//     and matched no key in trustSet), ErrNoUsableSignature.
func (v *Verifier) verifyAnyTrusted(
	suite freshness.SuiteRef,
	trustSet openpgp.EntityList,
	trustFPs map[string]struct{},
	signed []byte,
	sigBytes []byte,
) error {
	reader := packet.NewReader(bytes.NewReader(sigBytes))
	var (
		anyShortKeyID   bool
		anyUntrusted    bool
		sawSignature    bool
		lastVerifyErr   error
		anyTrustedSeen  bool
		rejectedSigners []string
	)
	for {
		pkt, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read signature packet: %w", err)
		}
		sig, ok := pkt.(*packet.Signature)
		if !ok {
			continue
		}
		sawSignature = true

		// Short-keyid fallback (SPEC2 §7.6.3): a real-world signer
		// (notably Docker, Microsoft) may omit the IssuerFingerprint
		// subpacket 33 and carry only the legacy 8-byte issuer keyid.
		// When AllowShortKeyID is enabled, attempt to resolve the
		// keyid against trustSet; on a single match we treat that
		// entity as the trust anchor for this packet. The narrowing
		// from any matched [[trusted_signer]] block still applies —
		// the fallback search runs over trustSet, not the broader
		// keyring.
		if len(sig.IssuerFingerprint) == 0 {
			anyShortKeyID = true
			if !v.allowShortKeyID || sig.IssuerKeyId == nil {
				if sig.IssuerKeyId != nil {
					rejectedSigners = append(rejectedSigners, fmt.Sprintf("keyid:%016X", *sig.IssuerKeyId))
				}
				continue
			}
			matched, err := findUniqueEntityByKeyID(trustSet, *sig.IssuerKeyId)
			if errors.Is(err, ErrAmbiguousKeyID) {
				// More than one trust-set key claims this keyid:
				// refuse rather than guess. Record as a verify
				// error so the switch below surfaces it with
				// proper precedence.
				anyTrustedSeen = true
				lastVerifyErr = err
				continue
			}
			if matched == nil {
				anyUntrusted = true
				rejectedSigners = append(rejectedSigners, fmt.Sprintf("keyid:%016X", *sig.IssuerKeyId))
				continue
			}
			anyTrustedSeen = true
			if verr := v.verifyAgainst(matched, signed, sig); verr == nil {
				v.logShortKeyIDFallbackOnce(suite, matched, *sig.IssuerKeyId)
				return nil
			} else {
				lastVerifyErr = verr
			}
			continue
		}

		issuerFP := upperFP(sig.IssuerFingerprint)
		if _, ok := trustFPs[issuerFP]; !ok {
			anyUntrusted = true
			rejectedSigners = append(rejectedSigners, "fpr:"+issuerFP)
			continue
		}
		anyTrustedSeen = true
		// Re-serialize this single packet and verify it in
		// isolation. This guarantees the packet that satisfied the
		// IssuerFingerprint policy is the same packet whose hash is
		// cryptographically validated.
		var sb bytes.Buffer
		if err := sig.Serialize(&sb); err != nil {
			lastVerifyErr = fmt.Errorf("re-serialize signature packet: %w", err)
			continue
		}
		_, _, verr := openpgp.VerifyDetachedSignature(
			trustSet,
			bytes.NewReader(signed),
			&sb,
			nil,
		)
		if verr == nil {
			return nil
		}
		lastVerifyErr = verr
	}
	switch {
	case !sawSignature:
		return ErrNoUsableSignature
	case anyTrustedSeen && lastVerifyErr != nil:
		return fmt.Errorf("signature verification failed: %w", lastVerifyErr)
	case anyUntrusted:
		v.logUntrustedSignerOnce(suite, rejectedSigners)
		return fmt.Errorf("%w: IssuerFingerprint not in trust set", ErrUntrustedSigner)
	case anyShortKeyID && !v.allowShortKeyID:
		return ErrShortKeyID
	default:
		return ErrNoUsableSignature
	}
}

// findUniqueEntityByKeyID looks up a short 8-byte issuer keyid against
// the given EntityList — checking each entity's primary key and every
// subkey. Returns (entity, nil) on exactly one match, (nil, nil) on no
// match, and (nil, ErrAmbiguousKeyID) when two or more distinct
// entities claim the same keyid.
//
// Uniqueness matters because the short-keyid fallback exists precisely
// to make trust decisions on the 8-byte keyid alone — collisions
// (whether adversarial "Evil 32"-style fingerprints or operator-
// staged duplicates) would let the wrong entity stand in for the
// intended signer. Failing closed on ambiguity is the only safe
// behavior; AllowShortKeyID=false (the strict posture) can be set if
// the ambiguity itself is intentional.
func findUniqueEntityByKeyID(list openpgp.EntityList, keyID uint64) (*openpgp.Entity, error) {
	var matched *openpgp.Entity
	for _, e := range list {
		hit := false
		if e.PrimaryKey != nil && e.PrimaryKey.KeyId == keyID {
			hit = true
		}
		if !hit {
			for _, sub := range e.Subkeys {
				if sub.PublicKey != nil && sub.PublicKey.KeyId == keyID {
					hit = true
					break
				}
			}
		}
		if !hit {
			continue
		}
		if matched != nil && matched != e {
			return nil, ErrAmbiguousKeyID
		}
		matched = e
	}
	return matched, nil
}

// verifyAgainst re-serializes one signature packet and verifies it
// against an EntityList containing only the matched entity. Mirrors
// the per-packet isolation step in the strict path so the entity
// whose keyid resolved the trust check is the same entity whose key
// is used for the cryptographic check.
func (v *Verifier) verifyAgainst(e *openpgp.Entity, signed []byte, sig *packet.Signature) error {
	var sb bytes.Buffer
	if err := sig.Serialize(&sb); err != nil {
		return fmt.Errorf("re-serialize signature packet: %w", err)
	}
	_, _, err := openpgp.VerifyDetachedSignature(
		openpgp.EntityList{e},
		bytes.NewReader(signed),
		&sb,
		nil,
	)
	return err
}

// logUntrustedSignerOnce emits one INFO log per (canonical_host,
// suite_path) per process (and again once its set has started over past
// onceSetMax suites) the first time we reject every signature on a suite
// for "issuer not in trust set." Subsequent rejections for the same suite
// stay silent — the goal is a one-line breadcrumb the
// operator can grep for to learn WHICH key fingerprint they need to
// install, not a per-tick log flood. rejected carries the rejected
// signers as "fpr:<40-hex>" (long fingerprint form) or "keyid:<16-hex>"
// (short-keyid form, when subpacket 33 was absent and the keyid did
// not resolve in the trust set).
func (v *Verifier) logUntrustedSignerOnce(suite freshness.SuiteRef, rejected []string) {
	key := suite.CanonicalHost + "|" + suite.SuitePath
	if !v.untrustedSignerLogged.first(key) {
		return
	}
	v.logger.Info("adoption_untrusted_signer",
		"canonical_host", suite.CanonicalHost,
		"suite_path", suite.SuitePath,
		"rejected_signers", rejected,
	)
}

// logShortKeyIDFallbackOnce emits one INFO log per (canonical_host,
// suite_path) per process (and again once its set has started over past
// onceSetMax suites) the first time we accept a signature via the
// short-keyid fallback. Subsequent acceptances for the same
// suite stay silent — this keeps the log surface a finite inventory
// of "which repos rely on the legacy form" rather than a per-check
// noise stream.
func (v *Verifier) logShortKeyIDFallbackOnce(suite freshness.SuiteRef, matched *openpgp.Entity, keyID uint64) {
	key := suite.CanonicalHost + "|" + suite.SuitePath
	if !v.shortKeyIDLogged.first(key) {
		return
	}
	v.logger.Info("adoption_short_keyid_fallback",
		"canonical_host", suite.CanonicalHost,
		"suite_path", suite.SuitePath,
		"issuer_keyid", fmt.Sprintf("%016X", keyID),
		"matched_fingerprint", upperFP(matched.PrimaryKey.Fingerprint),
	)
}

// logUnverifiedSignerOnce emits one INFO log per (canonical_host,
// suite_path) per process (and again once its set has started over past
// onceSetMax suites) the first time accept_any_signer's bypass branch
// accepts a suite. Subsequent acceptances for the same suite
// stay silent. The signer-info field, when extractable from the
// signature, names the IssuerFingerprint (preferred) or short
// IssuerKeyId so operators have an audit trail of which signers their
// fleet has effectively trusted under the relaxation.
//
// sigSource is the (already de-armored, for inline) signature byte
// stream; for inline it's block.ArmoredSignature.Body, for detached
// it's a Reader over the binary signature bytes. The helper is
// tolerant of malformed input — if signer extraction fails the log
// line still fires, with signer_info elided.
func (v *Verifier) logUnverifiedSignerOnce(suite freshness.SuiteRef, sigSource io.Reader) {
	key := suite.CanonicalHost + "|" + suite.SuitePath
	if !v.unverifiedSignerLogged.first(key) {
		return
	}
	signerInfo := extractSignerInfo(sigSource)
	attrs := []any{
		"canonical_host", suite.CanonicalHost,
		"suite_path", suite.SuitePath,
	}
	if signerInfo != "" {
		attrs = append(attrs, "signer", signerInfo)
	}
	v.logger.Info("adoption_unverified_signer", attrs...)
}

// requireParseableSignature reads OpenPGP packets from r and returns
// nil as soon as it sees at least one *packet.Signature. Returns an
// error if r contains no signature packet (EOF before any signature)
// or any packet fails to parse.
//
// This is the structural-integrity guard for the accept_any_signer
// bypass branches (SPEC2 §7.6.3): the trust + cryptographic check is
// skipped, but a clearsign envelope or Release.gpg blob that doesn't
// contain a parseable PGP signature packet must still be rejected.
// F6 (requireSignaturePacketsOnly), which runs before it on both
// branches, refuses every packet that is not a parseable signature;
// this adds that there is at least one.
func requireParseableSignature(r io.Reader) error {
	reader := packet.NewReader(r)
	for {
		pkt, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return errors.New("signature body contains no signature packet")
		}
		if err != nil {
			return fmt.Errorf("parse signature packet: %w", err)
		}
		if _, ok := pkt.(*packet.Signature); ok {
			return nil
		}
	}
}

// extractSignerInfo reads the first signature packet from r and
// returns a human-readable signer reference: "fpr:<40-hex>" when the
// IssuerFingerprint subpacket is present, "keyid:<16-hex>" when only
// the legacy short keyid is present, or "" on any parse failure. The
// reader is consumed; callers must not reuse it.
//
// Best-effort: this is for the audit log only, never for trust
// decisions. A malformed signature returning "" still produces an
// adoption_unverified_signer line, just without the signer field.
func extractSignerInfo(r io.Reader) string {
	reader := packet.NewReader(r)
	for {
		pkt, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return ""
		}
		if err != nil {
			return ""
		}
		sig, ok := pkt.(*packet.Signature)
		if !ok {
			continue
		}
		if len(sig.IssuerFingerprint) > 0 {
			return "fpr:" + upperFP(sig.IssuerFingerprint)
		}
		if sig.IssuerKeyId != nil {
			return fmt.Sprintf("keyid:%016X", *sig.IssuerKeyId)
		}
		return ""
	}
}
