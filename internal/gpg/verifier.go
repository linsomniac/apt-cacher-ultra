package gpg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	pgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/linsomniac/apt-cacher-ultra/internal/apttext"
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
	// [[trusted_signer]] block matches the suite, the verifier parses
	// the anchor structurally — the text, armor and packet layers (the
	// hardening plan's DN-11: T, A, P), as on every path — and returns
	// the signed text (or Release bytes verbatim in detached mode)
	// without consulting the host keyring. Pinned suites are
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

	// now, when set, is the time S2 checks signatures at (go-crypto's
	// expiry and revocation checks: sigConfig); nil, as NewVerifier leaves
	// it, is the wall clock. A test's clock: real vendors' keys expire
	// (vendors_test.go, the re-review's V-RM3).
	now func() time.Time
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
	// but is not a clearsigned message in the shape the cache accepts
	// (the hardening plan's DN-11): apt's own text layer refuses it (T1–T4,
	// internal/apttext: the marker not on line 1, a line after the END line,
	// an undashed "-" line), or the cache's text rules do (T5, F0, one
	// armor block), or its armor or packets fall outside the canonical
	// grammar (A1–A6, armor_canon.go: a forged checksum, a damaged armor;
	// P1–P5, sigpacket.go: packets apt refuses or that some apt parses
	// otherwise). Its detail names the layer and the rule. It is never
	// treated as text.
	ErrMalformedClearsign = errors.New("InRelease clearsign envelope is malformed")

	// ErrMalformedSignature means a Release.gpg is not one ASCII-armored
	// signature block in the shape the cache accepts (DN-11's T, A and P
	// for the detached form: bytes before its BEGIN line — a binary
	// signature included — or after its END line, a damaged armor, a
	// forged armor checksum, packets apt refuses or that some apt parses
	// otherwise). The detached twin of ErrMalformedClearsign. Its text
	// starts with the file's name, capital R and all (the hardening plan's
	// DN-4), hence the ST1005 exemption.
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

	// ErrBadSignature means a signature packet whose issuer names a
	// signing key in the host keyring failed its cryptographic check
	// against every entity holding that key, or verified with a
	// SignatureError (S1, requireKnownSignaturesVerify): gpgv's BADSIG,
	// which makes apt refuse the anchor even beside a good signature (the
	// 2026-09 security review's VC-1).
	ErrBadSignature = errors.New("a signature from a known key failed verification")
)

// VerifyInline implements freshness.Verifier. It returns M, the signed
// text as apt's own splitter reads it (layer T), which is what the
// client's apt hands its verifier and then its Release parser.
//
// Order of checks (each fails closed), the hardening plan's DN-11:
//  0. Resolve the per-suite trust set (resolveTrustSet); if RequirePinned
//     and no pin matches, abort (ErrUnpinnedSuite), whatever the anchor's
//     shape (the re-review's V-RM1).
//  1. T (internal/apttext): apt's SplitClearSignedFile, ported, and the
//     cache's text rules: M and the one armor block. A body no line of
//     which is the clearsign marker is ErrMissingSignature; any other
//     refusal is ErrMalformedClearsign, in every mode (the "verified
//     bytes == stored bytes" rule of SPEC2 §7.5 step 2: nothing around
//     the clearsigned message, no line the cache would ignore and apt
//     would read).
//  2. A (armor_canon.go) and P (sigpacket.go): the armor, as a
//     canonical whitelist, and its signature packets, by the cache's own
//     v4 parser: ErrMalformedClearsign.
//  3. Under accept_any_signer with no pin matched (step 0), accept here
//     (SPEC2 §7.6.2's bypass: T, A and P passed, so at least one
//     signature packet).
//  4. S1 (requireKnownSignaturesVerify): every signature packet gpgv
//     checks whose issuer names a key in the host keyring verifies, as
//     apt's BADSIG refuses the anchor even beside a good signature:
//     ErrBadSignature. Not on the bypass path (the hardening plan's DN-8).
//  5. S2 (verifyAnyTrusted): one packet gpgv checks passes BOTH the
//     long-form-fingerprint trust check AND verifies cryptographically
//     against the trust set, and S2 counts it (discount: SHA-256, -384 or
//     -512, ruling HD-18; not erased by apt at a later SHA-1 packet by its
//     key). Without the coupling, a multi-sig block could satisfy
//     the policy with one packet (decoy) and verify a different packet
//     (the actual cryptographic anchor).
//
// AIDEV-NOTE: I-1 (the 2026-09 security review): go-crypto v1.4.1's
// clearsign.Decode panics (slice bounds [:-1]) on a signed text with no
// lines (fixed in v1.5.0). The verifier no longer calls it (nor
// armor.Decode, nor packet.Reader): go-crypto reads one packet at a time,
// only after layer P has validated its bytes. The freshness goroutines still recover, as
// defence in depth (TestVerifyInline_ZeroPlaintextIsAnErrorNotAPanic pins
// the empty signed text).
//
// ctx is accepted for interface compatibility but VerifyInline does
// no I/O — verification is CPU-bound and short.
func (v *Verifier) VerifyInline(ctx context.Context, suite freshness.SuiteRef, inRelease []byte) ([]byte, error) {
	_ = ctx

	// Step 0: the suite's trust set; an unpinned suite under
	// require_pinned_signer is refused here, whatever the anchor.
	trust, err := v.resolveTrustSet(suite)
	if err != nil {
		return nil, err
	}

	// Step 1: layer T. Only a clearsigned envelope is inline metadata,
	// whatever require_signature says (spec C5 rev 4).
	//
	// AIDEV-NOTE: plan review finding 4. The old require_signature=false
	// branch returned the raw body here, which let a plain-text or
	// undecodable "InRelease" become a trust-bearing inline snapshot.
	// Never reintroduce a text fallback: unsigned repositories go through
	// the explicit unsigned form, which never vouches.
	cs, err := apttext.Inline(inRelease)
	if errors.Is(err, apttext.ErrNotClearsigned) {
		// apt reads such a body as not clearsigned. One that holds the
		// marker elsewhere — after a leading blank, space, tab or BOM
		// (ruling R-D2) — is reported as a damaged envelope, as before
		// the port: what the operator should hear about it.
		if bytes.Contains(inRelease, []byte(apttext.ClearsignBeginMarker)) {
			return nil, fmt.Errorf("%w: %w", ErrMalformedClearsign,
				refuse("T1", "line 1 is not %q, which the body holds elsewhere (apt reads it as not clearsigned)", apttext.ClearsignBeginMarker))
		}
		return nil, ErrMissingSignature
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformedClearsign, err)
	}

	// Step 2: layers A and P.
	pkts, err := armorPackets(cs.Armor, cs.ArmorLine)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformedClearsign, err)
	}

	// Steps 3-5.
	if err := v.checkSignatures(suite, trust, cs.Message, pkts); err != nil {
		return nil, err
	}
	return cs.Message, nil
}

// VerifyDetached verifies a detached signature (Release.gpg bytes)
// against the given Release bytes. Returns releaseBytes verbatim on
// success — there is no "extracted plaintext" the way clearsign has;
// the Release file IS the verified plaintext.
//
// sigBytes must be one ASCII-armored signature block, as apt-ftparchive's
// recommended workflow emits it (`gpg --detach-sign --armor`), in the
// shape the cache accepts (the hardening plan's DN-11): apt's own check of
// the file (layer T: its first line the BEGIN line, no line outside the
// armor; a binary Release.gpg is refused, as apt 2.2 to 3.2 refuse it,
// "unsupported binary format"), one armor block, the canonical armor (A)
// and v4 signature packets (P). Each refusal is ErrMalformedSignature.
//
// Order of checks (each fails closed):
//  1. Empty inputs are a programming error in the caller (the
//     freshness checker only routes here when both Release and
//     Release.gpg fetched non-empty bodies).
//  2. Resolve the per-suite trust set; if RequirePinned and no match,
//     abort, whatever the signature's shape (VerifyInline's step 0).
//  3. Layers T, A and P on sigBytes, on the bypass path too. Under
//     accept_any_signer with no pin matched, accept here.
//  4. S1 and S2 over the Release bytes, as VerifyInline's steps 4 and 5.
//
// As in VerifyInline, a missing signature never passes through: a
// detached path that arrived without Release.gpg never reaches this
// function (the freshness checker only switches to detached mode
// after fetching both bodies).
//
// AIDEV-NOTE: the detached form's "bare armor" guard (internal/apttext's
// history note): until the 2026-10-28 hardening plan it enforced none, and
// so adopted what apt refuses (VC-2, C-4) while every client of the suite
// then failed `apt update`. Enforced now by apt's own grammar, ported (T),
// before any parser reads the armor. Still open: weak keys (the security
// review's C-3, its key half: VerifyDetachedSignature runs with a nil
// config, so RSA-1024 passes); C-3's digest half is closed by S2's
// countability (ruling HD-18).
func (v *Verifier) VerifyDetached(ctx context.Context, suite freshness.SuiteRef, releaseBytes, sigBytes []byte) ([]byte, error) {
	_ = ctx

	if len(releaseBytes) == 0 {
		return nil, errors.New("empty Release body")
	}
	if len(sigBytes) == 0 {
		return nil, errors.New("empty Release.gpg body")
	}
	trust, err := v.resolveTrustSet(suite)
	if err != nil {
		return nil, err
	}
	lines, err := apttext.Detached(sigBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformedSignature, err)
	}
	pkts, err := armorPackets(lines, 1)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformedSignature, err)
	}
	if err := v.checkSignatures(suite, trust, releaseBytes, pkts); err != nil {
		return nil, err
	}
	return releaseBytes, nil
}

// armorPackets is layers A and P: the armor block's lines (first is
// lines[0]'s line number in the file) to its signature packets.
func armorPackets(lines [][]byte, first int) ([]sigPacket, error) {
	b, err := parseArmor(lines, first)
	if err != nil {
		return nil, err
	}
	return parseSigPackets(b)
}

// checkSignatures is layer S over the packets layer P read, signed being
// what they sign (M inline, the Release detached), under the suite's
// trust set (resolveTrustSet): the bypass, or S1 and S2.
func (v *Verifier) checkSignatures(suite freshness.SuiteRef, trust suiteTrust, signed []byte, pkts []sigPacket) error {
	if trust.bypass {
		// SPEC2 §7.6.2 bypass: the anchor parsed structurally (T, A and
		// P, so at least one signature packet: parseSigPackets refuses
		// none); trust + crypto check skipped. The original signed bytes
		// are what callers will persist and serve to apt clients, so
		// per-source Signed-By rules on the fleet remain the
		// authoritative trust anchor.
		//
		// AIDEV-NOTE: S1 (requireKnownSignaturesVerify) is deliberately
		// not applied here, for either form: the bypass verifies no
		// signature, and S1 would be its only signature check (the
		// hardening plan's DN-8, recorded Open). Do not add it here
		// without that decision. The structural layers are the same
		// code on both paths (TestVerifier_BypassRefusesEveryMalformedRow).
		v.logUnverifiedSignerOnce(suite, pkts[0])
		return nil
	}
	if len(trust.entities) == 0 {
		// No key can verify the signed metadata — fail closed. Unpinned:
		// the host keyring is empty (require_signature = false admits
		// that at startup). Pinned: the keyring holds none of the
		// pinned fingerprints (we already passed the RequirePinned
		// gate, so this is "configured but un-loadable").
		if !trust.pinned {
			return fmt.Errorf("%w: the host keyring is empty — signed suites cannot adopt until keys are installed or adoption.accept_any_signer is set", ErrNoUsableSignature)
		}
		return fmt.Errorf("%w: pin matched but no key in host keyring satisfies it", ErrNoUsableSignature)
	}
	checked := gpgvChecks(pkts)
	if err := v.requireKnownSignaturesVerify(signed, checked); err != nil {
		return err
	}
	return v.verifyAnyTrusted(suite, trust.entities, trust.fps, signed, checked)
}

// gpgvChecks returns the packets gpgv checks: every one when they share
// one signature type, else the first only. gpgv hashes the data as the
// first packet's type says (text or binary) and, when another packet's
// type differs, warns "multiple signatures detected. Only the first will
// be checked." (gnupg 2.2.27 to 2.4.8, g10/mainproc.c, proc_tree's
// multiple_ok).
//
// AIDEV-NOTE: Task 9b's ruling on n_i_unknown00_first (batch fx12): every
// gpgv-based apt refuses an unknown key's binary-mode packet before a good
// text-mode one, because gpgv then checks the unknown key's alone; DN-11's
// P2 accepts both types. Measured both ways round (batch fx14): when that
// first packet is a good one, every apt accepts whatever follows
// (n_i_good_then_unknown00, n_i_good_then_badA00, n_d_good_then_badA01:
// sqv checks every packet and tolerates a bad one beside a good one); when
// it is not, every gpgv-based apt refuses (n_d_unknown01_first,
// n_i_badA00_then_good, n_d_badA01_then_good). So S1 and S2 read exactly
// the packets gpgv checks, and a lone binary-mode packet in an InRelease
// (o2_binary_sig_in_clearsign, every apt) still counts.
func gpgvChecks(pkts []sigPacket) []sigPacket {
	for _, p := range pkts[1:] {
		if p.sigType != pkts[0].sigType {
			return pkts[:1]
		}
	}
	return pkts
}

// suiteTrust is a suite's trust set (resolveTrustSet).
type suiteTrust struct {
	entities openpgp.EntityList  // to verify against (nil under bypass)
	fps      map[string]struct{} // their uppercase fingerprints (nil under bypass)
	pinned   bool                // at least one [[trusted_signer]] block matched
	bypass   bool                // accept_any_signer and no pin matched
}

// resolveTrustSet implements SPEC2 §7.6.2. Returns:
//   - entities: the openpgp.EntityList to verify against (nil under bypass)
//   - fps:      the uppercase-fingerprint set (for IssuerFingerprint check;
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
//
// AIDEV-NOTE: the verifier resolves it before layers T, A and P (the
// re-review's V-RM1): an unpinned suite under require_pinned_signer is a
// configuration fault, which the operator hears about (unpinned_suite,
// the rollout signal for integrity.require_pinned_signer, and the WARN
// below) whatever the anchor's shape; when Task 9b moved the layers
// first, a malformed anchor hid it. It decides nothing about the anchor:
// a pinned or unrequired suite's malformed anchor keeps its reason.
func (v *Verifier) resolveTrustSet(suite freshness.SuiteRef) (suiteTrust, error) {
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
			return suiteTrust{}, fmt.Errorf("%w: %w: no [[trusted_signer]] block matches %q",
				ErrUnpinnedSuite, freshness.ErrAdoptionUnpinnedSuite, suite.CanonicalHost)
		}
		if v.acceptAnySigner {
			// SPEC2 §7.6.2 bypass branch. Caller short-circuits the
			// trust + crypto check; only the structural decode
			// remains. trustSet / trustFPs are nil because they have
			// no consumer on this path.
			return suiteTrust{bypass: true}, nil
		}
		// Broad trust: the entire host keyring. Build a copy of the
		// host fingerprint set so the IssuerFingerprint check has
		// something to consult.
		fps := make(map[string]struct{}, len(v.keyring.fingerprints))
		for fp := range v.keyring.fingerprints {
			fps[fp] = struct{}{}
		}
		return suiteTrust{entities: v.keyring.EntityList(), fps: fps}, nil
	}
	union := make(map[string]struct{})
	for _, m := range matched {
		for fp := range m.Fingerprints {
			union[fp] = struct{}{}
		}
	}
	return suiteTrust{entities: v.keyring.Subset(union), fps: union, pinned: true}, nil
}

// requireKnownSignaturesVerify is S1, apt's rule that a bad signature from
// a known key is fatal (VC-1; rulings HD-3, HD-8): every packet of pkts
// (the packets gpgv checks) whose issuer key ID names a signing key in the
// host keyring must pass its cryptographic check against one of the
// keyring's entities holding that key ID. A packet whose check passes but
// whose key or signature is expired or revoked passes (gpgv's EXPKEYSIG and
// REVKEYSIG, which apt ignores beside a good signature); a SignatureError
// fails (an unknown critical notation, on the packet or the key's
// self-signatures: gpgv's BADSIG); a packet whose issuer the keyring lacks
// is ignored (gpgv's NO_PUBKEY), and the next is checked (V-I2:
// TestRequireKnownSignaturesVerify_ContinuesPastAnUnknownKeysPacket).
//
// AIDEV-NOTE: the issuer is the packet's one issuer key ID: layer P
// (sigpacket.go's P4) refuses a packet with none, two, or a fingerprint
// that names another key, so gpgv 2.2, gpgv 2.4, sqv and go-crypto all
// look the key up by the same ID, and the contradiction rule S1 had
// (rulings HD-3, HD-14: an unhashed issuer contradicting the hashed one,
// xi_badsig_issuer_rewritten) is moot; sigpacket.go's note keeps that
// history. openpgp.VerifyDetachedSignature returns a non-nil signature
// exactly when the cryptographic check passed, then reports expiry,
// revocation or an unknown critical notation as its error (go-crypto
// v1.5.2 openpgp/read.go, verifyDetachedSignature and
// checkMessageSignatureDetails, unchanged since v1.4.1). It reads the
// packet's bytes as layer P framed them, never a re-serialization (go-crypto's Serialize re-framed
// lengths in the shortest form, which failed a packet gpgv verifies:
// strict, gone with it). Each candidate entity is tried alone: the
// library hashes once and tries every key with that one hash, which only
// the first candidate can pass. The host keyring, not the suite's trust
// set: a client that installed any of the cache's keys sees the BADSIG.
// Re-check all of this when go-crypto is upgraded (the table test and this
// file's tests pin it).
func (v *Verifier) requireKnownSignaturesVerify(signed []byte, pkts []sigPacket) error {
	keyring := v.keyring.EntityList()
	for _, p := range pkts {
		keys := keyring.KeysByIdUsage(p.issuer, packet.KeyFlagSign)
		if len(keys) == 0 {
			continue
		}
		passed := false
		tried := make(map[*openpgp.Entity]bool, len(keys))
		for _, k := range keys {
			if tried[k.Entity] {
				continue
			}
			tried[k.Entity] = true
			got, _, verr := openpgp.VerifyDetachedSignature(openpgp.EntityList{k.Entity},
				bytes.NewReader(signed), bytes.NewReader(p.raw), nil)
			if got != nil && !errors.As(verr, new(pgperrors.SignatureError)) {
				passed = true
				break
			}
		}
		if !passed {
			return fmt.Errorf("%w: S1: packet %d, issuer %016X", ErrBadSignature, p.n, p.issuer)
		}
	}
	return nil
}

// s2Hashes are the hash algorithms whose signatures S2 counts (ruling
// HD-18, the security review's C-3, its digest half): SHA-256, SHA-384 and
// SHA-512. Layer P also reads SHA-1 (2), so that a SHA-1 packet beside a
// good one is checked by S1 as gpgv checks it, but it never makes an
// anchor adoptable: every measured apt refuses an anchor whose only
// signature is SHA-1 (x3i_sha1_only).
var s2Hashes = map[byte]bool{8: true, 9: true, 10: true}

// verifyAnyTrusted is S2: it walks pkts (the packets gpgv checks) and
// accepts on the FIRST one that passes BOTH (a) a trust check —
// long-form IssuerFingerprint in trustFPs, or (when AllowShortKeyID
// is enabled) the legacy 8-byte issuer keyid maps to a single entity
// in trustSet — AND (b) cryptographic verification against the
// matched entity, and that S2 counts (discount: its hash, apt's erasure
// of it). Coupling these checks to the same packet is the SPEC2 §7.6.3
// invariant: a multi-signature block must not satisfy the policy with one
// packet while the library's verifier accepts a different packet. Each
// packet is verified as layer P framed it (p.raw), alone.
//
// Error precedence when no packet is accepted:
//   - If a trusted packet was discounted for a reason that outranks the
//     rest (discount: apt erases it), return the first such reason: it
//     names why an otherwise good signature did not count.
//   - If at least one packet's IssuerFingerprint was trusted but
//     none verified (or none was countable), return the last
//     underlying verification error.
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
	pkts []sigPacket,
) error {
	var (
		anyShortKeyID   bool
		anyUntrusted    bool
		sawSignature    bool
		lastVerifyErr   error
		discounted      error // the first reason that outranks lastVerifyErr (discount)
		anyTrustedSeen  bool
		rejectedSigners []string
	)
	// skip records why S2 does not count pkts[i], a packet it trusts, and
	// says whether it does not (discount).
	skip := func(i int) bool {
		outranks, err := discount(pkts, i)
		switch {
		case err == nil:
			return false
		case !outranks:
			lastVerifyErr = err
		case discounted == nil:
			discounted = err
		}
		return true
	}
	for i, p := range pkts {
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
		if p.fpr == nil {
			anyShortKeyID = true
			if !v.allowShortKeyID {
				rejectedSigners = append(rejectedSigners, fmt.Sprintf("keyid:%016X", p.issuer))
				continue
			}
			matched, err := findUniqueEntityByKeyID(trustSet, p.issuer)
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
				rejectedSigners = append(rejectedSigners, fmt.Sprintf("keyid:%016X", p.issuer))
				continue
			}
			anyTrustedSeen = true
			if skip(i) {
				continue
			}
			// The entity whose keyid resolved the trust check is the
			// one whose key is used for the cryptographic check.
			if _, _, verr := openpgp.VerifyDetachedSignature(openpgp.EntityList{matched},
				bytes.NewReader(signed), bytes.NewReader(p.raw), v.sigConfig()); verr != nil {
				lastVerifyErr = verr
				continue
			}
			v.logShortKeyIDFallbackOnce(suite, matched, p.issuer)
			return nil
		}

		issuerFP := upperFP(p.fpr)
		if _, ok := trustFPs[issuerFP]; !ok {
			anyUntrusted = true
			rejectedSigners = append(rejectedSigners, "fpr:"+issuerFP)
			continue
		}
		anyTrustedSeen = true
		if skip(i) {
			continue
		}
		// Verify this one packet in isolation, as framed: the packet
		// that satisfied the IssuerFingerprint policy is the packet
		// whose hash is cryptographically validated.
		_, _, verr := openpgp.VerifyDetachedSignature(
			trustSet,
			bytes.NewReader(signed),
			bytes.NewReader(p.raw),
			v.sigConfig(),
		)
		if verr == nil {
			return nil
		}
		lastVerifyErr = verr
	}
	switch {
	case !sawSignature:
		return ErrNoUsableSignature
	case anyTrustedSeen && discounted != nil:
		return fmt.Errorf("signature verification failed: %w", discounted)
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

// sigConfig is the go-crypto configuration S2 verifies a packet with: nil
// (the defaults, the wall clock) unless a test set the verifier's clock
// (now). go-crypto's detached verification reads only its time and its
// known notations (none either way). S1 needs none: it passes an expired
// or revoked key's good signature whatever the time, as gpgv's EXPKEYSIG
// and REVKEYSIG.
func (v *Verifier) sigConfig() *packet.Config {
	if v.now == nil {
		return nil
	}
	return &packet.Config{Time: v.now}
}

// uncountable is S2's refusal of a trusted packet whose hash it does not
// count (s2Hashes).
func uncountable(p sigPacket) error {
	return fmt.Errorf("S2: packet %d: hash algorithm %d is never counted (SHA-1: ruling HD-18)", p.n, p.hashAlgo)
}

// discount says why S2 does not count pkts[i] (pkts are the packets gpgv
// checks; S2 trusts pkts[i]), or nil when it counts it:
//
//   - its hash is not one of s2Hashes (SHA-1: ruling HD-18): uncountable;
//   - debian:13's sqv does not read it (the re-review's A-RC1): Sequoia's
//     parser refuses one of its subpackets (sigPacket.sqvFault), so sqv
//     reads the packet as unknown and does not count it;
//   - apt erases it (the re-review's A-RC2): a later packet of pkts, by
//     the same key, has a hash apt distrusts (erasedBy).
//
// The last two outrank a verification error at the refusal: each names why
// an otherwise good signature did not count. Not counting a packet sqv
// does not read, rather than refusing the anchor, is what every apt does
// beside another good packet (y_goodmalformed_plus_good, accepted by all
// six: sqv counts the other one, gpgv reads both).
//
// AIDEV-NOTE: the erasure is apt's, not gpgv's: methods/gpgv.cc (2.2.4
// :245-250, 3.2.0 :293-302) erases every Signers.Good entry of a key at
// that key's VALIDSIG with an untrusted digest, and gpgv writes each
// packet's GOODSIG before its VALIDSIG, packet by packet, so only good
// signatures BEFORE the SHA-1 packet are erased (x3i_sha1_plus_good, SHA-1
// first, is accepted by every apt; e_good_then_sha1A is refused by the five
// gpgv-based apt; debian:13's sqv ignores the SHA-1 packet and accepts
// both). The key is the signing (sub)key: apt's IsTheSameKey compares the
// VALIDSIG fingerprint's long key ID with the GOODSIG's, and P4 makes a
// packet's one issuer key ID the key every reader takes, so the issuers
// are compared. The later packet need not be trusted for the suite: gpgv
// writes a VALIDSIG for every good packet of a key in its keyring, and a
// key whose good signature S2 trusts is one; S1 has verified the later
// packet (it is gpgv-checked and its key is known), so it is a good one.
func discount(pkts []sigPacket, i int) (outranks bool, err error) {
	p := pkts[i]
	if !s2Hashes[p.hashAlgo] {
		return false, uncountable(p)
	}
	if p.sqvFault != "" {
		return true, fmt.Errorf("S2: packet %d: sqv does not read it: %s, which Sequoia's parser refuses, so debian:13's sqv reads the packet as unknown and does not count it", p.n, p.sqvFault)
	}
	if q, ok := erasedBy(pkts, i); ok {
		return true, fmt.Errorf("S2: packet %d: apt erases it: packet %d, by the same key %016X, has hash algorithm %d (SHA-1), "+
			"and apt drops every good signature of a key it has read when that key's VALIDSIG has a hash it distrusts", p.n, q.n, p.issuer, q.hashAlgo)
	}
	return false, nil
}

// erasedBy returns the first packet after pkts[i] in pkts (the packets
// gpgv checks) by the same key whose hash apt distrusts
// (aptUntrustedDigest), and whether there is one: the packet at whose
// VALIDSIG apt erases pkts[i]'s good signature (discount).
func erasedBy(pkts []sigPacket, i int) (sigPacket, bool) {
	for _, q := range pkts[i+1:] {
		if q.issuer == pkts[i].issuer && aptUntrustedDigest(q.hashAlgo) {
			return q, true
		}
	}
	return sigPacket{}, false
}

// aptUntrustedDigest reports whether apt's methods/gpgv.cc distrusts a
// VALIDSIG whose hash algorithm is h, by default: its Digests table, the
// same in apt 2.2.4 to 3.2.0, trusts ids 4 to 11 (four reserved ids,
// SHA-256, SHA-384, SHA-512 and SHA-224) and distrusts every other id — 0,
// MD5, SHA-1, RIPE-MD/160, and any id past the table (FindDigest's
// fallback, "Invalid digest"). Of the hashes layer P reads, SHA-1 alone.
func aptUntrustedDigest(h byte) bool { return h < 4 || h > 11 }

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
// stay silent. The signer field names the first signature packet's
// IssuerFingerprint ("fpr:<40-hex>") or, when it has none, its issuer key
// ID ("keyid:<16-hex>": layer P gives every packet one), so operators have
// an audit trail of which signers their fleet has effectively trusted
// under the relaxation.
func (v *Verifier) logUnverifiedSignerOnce(suite freshness.SuiteRef, first sigPacket) {
	key := suite.CanonicalHost + "|" + suite.SuitePath
	if !v.unverifiedSignerLogged.first(key) {
		return
	}
	signer := fmt.Sprintf("keyid:%016X", first.issuer)
	if first.fpr != nil {
		signer = "fpr:" + upperFP(first.fpr)
	}
	v.logger.Info("adoption_unverified_signer",
		"canonical_host", suite.CanonicalHost,
		"suite_path", suite.SuitePath,
		"signer", signer,
	)
}
