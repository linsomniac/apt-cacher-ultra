package gpg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"sync"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

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
	Keyring          *Keyring
	Pins             []SignerPin
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
	// emits one INFO log per (host, suite_path) per process the
	// first time it accepts a signature, so operators can inventory
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
	keyring          *Keyring
	pins             []SignerPin
	requireSignature bool
	requirePinned    bool
	allowShortKeyID  bool
	acceptAnySigner  bool
	logger           *slog.Logger

	// shortKeyIDLogged tracks suites for which a short-keyid-fallback
	// acceptance has already been logged this process. Keyed by
	// "canonical_host|suite_path"; values are unused. Concurrent-safe
	// via sync.Map's LoadOrStore.
	shortKeyIDLogged sync.Map

	// untrustedSignerLogged tracks suites for which an
	// adoption_untrusted_signer rejection has already been logged this
	// process. Same dedupe shape as shortKeyIDLogged so the log surface
	// is a finite inventory of "which (host, suite) pairs need new keys"
	// rather than a per-tick noise stream.
	untrustedSignerLogged sync.Map

	// unverifiedSignerLogged tracks suites for which an
	// adoption_unverified_signer acceptance has already been logged this
	// process. Fires under accept_any_signer when the bypass branch
	// runs. Same dedupe shape as the other once-loggers.
	unverifiedSignerLogged sync.Map
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
		keyring:          cfg.Keyring,
		pins:             cfg.Pins,
		requireSignature: cfg.RequireSignature,
		requirePinned:    cfg.RequirePinned,
		allowShortKeyID:  cfg.AllowShortKeyID,
		acceptAnySigner:  cfg.AcceptAnySigner,
		logger:           logger,
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
	}
	return "crypto_verify_failed"
}

// Sentinel errors returned by VerifyInline. Callers (the Adopter) wrap
// these into ErrAdoptionGPGFailed for the SPEC2 §10.2 categorized log.
var (
	// ErrUnpinnedSuite means RequirePinned is true and no
	// [[trusted_signer]] block matched the suite's canonical host.
	ErrUnpinnedSuite = errors.New("adoption_unpinned_suite")

	// ErrMissingSignature means the InRelease body is not clearsigned
	// and RequireSignature is true.
	ErrMissingSignature = errors.New("InRelease is not clearsigned")

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
)

// VerifyInline implements freshness.Verifier. Returns the verified
// Release-equivalent plaintext (the cleartext between BEGIN/END
// markers) on success.
//
// Order of checks (each fails closed):
//  1. Reject extraneous bytes around the clearsigned block.
//     clearsign.Decode silently discards prefix data, so we enforce
//     "exactly one clearsigned message, with at most whitespace
//     before/after" before invoking the decoder. Without this, an
//     adversary could prepend forged content to a real InRelease;
//     the verifier would happily verify the embedded message while
//     §7.5 step 2 stored the original (prefix-bearing) bytes as the
//     pool blob — a cache-pollution path.
//  2. Resolve per-suite trust set; if RequirePinned and no match, abort.
//  3. Decode clearsigned block; if absent and RequireSignature, abort.
//     If absent and !RequireSignature, return body verbatim.
//  4. Iterate signature packets, attempting cryptographic verification
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
	// around the clearsigned block. requireBareClearsigned returns
	// nil when no BEGIN marker is present, so plain-text bodies
	// (which the require_signature=false path returns verbatim
	// later) reach the no-clearsign branch unobstructed.
	if err := requireBareClearsigned(inRelease); err != nil {
		return nil, err
	}

	// Step 2: per-suite trust-set resolution.
	trustSet, trustFPs, _, bypass, err := v.resolveTrustSet(suite)
	if err != nil {
		return nil, err
	}

	// Step 3: clearsign decode.
	block, _ := clearsign.Decode(inRelease)
	if block == nil {
		if v.requireSignature {
			return nil, ErrMissingSignature
		}
		// Operator opted into the loud "unsigned OK" mode at startup.
		// Return the body verbatim — the parser downstream treats
		// this as Release-style text.
		return inRelease, nil
	}

	if bypass {
		// SPEC2 §7.6.2 bypass: clearsign envelope decoded structurally;
		// trust + crypto check skipped. The original signed inRelease
		// bytes are what callers will persist and serve to apt clients,
		// so per-source Signed-By rules on the fleet remain the
		// authoritative trust anchor.
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
		if err := requireParseableSignature(bytes.NewReader(sigBytes)); err != nil {
			return nil, fmt.Errorf("clearsigned signature body: %w", err)
		}
		v.logUnverifiedSignerOnce(suite, bytes.NewReader(sigBytes))
		return block.Plaintext, nil
	}

	if len(trustSet) == 0 {
		// Pinned with non-empty fingerprints, but the host keyring
		// contains no key with any of those fingerprints. The signed
		// metadata exists but cannot be verified — fail closed. (Note
		// we already passed the RequirePinned gate, so this is
		// "configured but un-loadable.")
		return nil, fmt.Errorf("%w: pin matched but no key in host keyring satisfies it", ErrNoUsableSignature)
	}

	// Step 4: per-packet verify-and-trust loop. A signature packet is
	// accepted iff it carries a long-form IssuerFingerprint that is
	// in trustFPs AND verifies cryptographically against trustSet.
	sigBytes, err := readArmoredSignature(block.ArmoredSignature.Body)
	if err != nil {
		return nil, fmt.Errorf("read signature: %w", err)
	}
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
// sigBytes may be either binary or ASCII-armored. apt-ftparchive's
// recommended workflow emits ASCII-armored Release.gpg
// (`gpg --detach-sign --armor`), but binary Release.gpg also exists in
// the wild; both forms are accepted.
//
// Order of checks (each fails closed):
//  1. Empty inputs are a programming error in the caller (the
//     freshness checker only routes here when both Release and
//     Release.gpg fetched non-empty bodies).
//  2. Resolve per-suite trust set; if RequirePinned and no match, abort.
//  3. De-armor sigBytes if it looks armored; otherwise treat as binary.
//  4. Iterate signature packets, accepting on the first packet that
//     BOTH passes the long-form-fingerprint trust check AND verifies
//     cryptographically against trustSet — same per-packet coupling
//     as VerifyInline (SPEC2 §7.6.3).
//
// Unlike VerifyInline there is no "no signature, !RequireSignature
// passes through" branch: a detached path that arrived without
// Release.gpg never reaches this function (the freshness checker
// only switches to detached mode after fetching both bodies).
//
// AIDEV-NOTE: we deliberately do NOT enforce a "bare armor" guard
// equivalent to requireBareClearsigned. clearsign.Decode silently
// strips prefix bytes around its envelope, which can alias verified-
// bytes vs stored-bytes when the InRelease blob lands in the pool.
// armor.Decode also strips, but for Release.gpg the stored bytes
// (whatever they are) are served verbatim to apt clients on every
// subsequent fetch, which re-verify against the same key. So
// "verified bytes == stored bytes" holds trivially: whatever bytes
// we accept here are the bytes apt will also accept downstream.
func (v *Verifier) VerifyDetached(ctx context.Context, suite freshness.SuiteRef, releaseBytes, sigBytes []byte) ([]byte, error) {
	_ = ctx

	if len(releaseBytes) == 0 {
		return nil, errors.New("empty Release body")
	}
	if len(sigBytes) == 0 {
		return nil, errors.New("empty Release.gpg body")
	}

	trustSet, trustFPs, _, bypass, err := v.resolveTrustSet(suite)
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
		// decodeMaybeArmoredSignature only structurally validates the
		// ASCII-armor frame when present; for non-armored input it
		// merely size-caps the bytes. requireParseableSignature is the
		// invariant that "the body actually decodes as at least one
		// OpenPGP signature packet" — without it, raw non-armored
		// garbage under 64 KiB would propagate untouched into the pool.
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
		return nil, fmt.Errorf("%w: pin matched but no key in host keyring satisfies it", ErrNoUsableSignature)
	}

	binarySig, err := decodeMaybeArmoredSignature(sigBytes)
	if err != nil {
		return nil, fmt.Errorf("decode signature: %w", err)
	}

	if err := v.verifyAnyTrusted(suite, trustSet, trustFPs, releaseBytes, binarySig); err != nil {
		return nil, err
	}
	return releaseBytes, nil
}

// decodeMaybeArmoredSignature returns the binary signature packet
// bytes for a Release.gpg blob. If the input begins with an armor
// frame ("-----BEGIN PGP SIGNATURE-----" after optional whitespace),
// it is de-armored; otherwise the bytes are returned as-is.
//
// Caps the binary output at 64 KiB — real Release.gpg signatures are
// well under 1 KiB; the cap bounds memory if a hostile upstream feeds
// a giant fake signature.
func decodeMaybeArmoredSignature(b []byte) ([]byte, error) {
	const maxBinary = 64 << 10
	if isArmoredSignature(b) {
		block, err := armor.Decode(bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("armor decode: %w", err)
		}
		if block.Type != "PGP SIGNATURE" {
			return nil, fmt.Errorf("unexpected armor type %q (want \"PGP SIGNATURE\")", block.Type)
		}
		out, err := io.ReadAll(io.LimitReader(block.Body, int64(maxBinary)+1))
		if err != nil {
			return nil, err
		}
		if len(out) > maxBinary {
			return nil, fmt.Errorf("signature too large (>%d bytes after de-armor)", maxBinary)
		}
		return out, nil
	}
	if len(b) > maxBinary {
		return nil, fmt.Errorf("signature too large (>%d bytes)", maxBinary)
	}
	return b, nil
}

// isArmoredSignature returns true iff b (after skipping leading
// whitespace) begins with the standard "PGP SIGNATURE" armor header.
func isArmoredSignature(b []byte) bool {
	for len(b) > 0 {
		c := b[0]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			b = b[1:]
			continue
		}
		break
	}
	return bytes.HasPrefix(b, []byte("-----BEGIN PGP SIGNATURE-----"))
}

// requireBareClearsigned returns an error if b contains anything
// other than (optional whitespace) + clearsigned message + (optional
// whitespace). clearsign.Decode silently strips prefix bytes; we
// reject them here to preserve the invariant that "verified bytes ==
// stored bytes (modulo trailing newlines)" — see SPEC2 §7.5 step 2.
//
// If b doesn't contain a clearsigned marker at all, this returns nil
// — let the downstream "no clearsign" path handle the !RequireSignature
// case.
func requireBareClearsigned(b []byte) error {
	const beginMarker = "-----BEGIN PGP SIGNED MESSAGE-----"
	const endSig = "-----END PGP SIGNATURE-----"
	idx := bytes.Index(b, []byte(beginMarker))
	if idx < 0 {
		return nil
	}
	if !isWhitespaceOnly(b[:idx]) {
		return errors.New("clearsigned message has extraneous bytes before BEGIN marker")
	}
	endIdx := bytes.Index(b, []byte(endSig))
	if endIdx < 0 {
		// Missing END marker — let clearsign.Decode produce the real
		// error. Don't fabricate one here.
		return nil
	}
	tail := b[endIdx+len(endSig):]
	if !isWhitespaceOnly(tail) {
		return errors.New("clearsigned message has extraneous bytes after END marker")
	}
	return nil
}

func isWhitespaceOnly(b []byte) bool {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			return false
		}
	}
	return true
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
// suite_path) per process the first time we reject every signature on
// a suite for "issuer not in trust set." Subsequent rejections for the
// same suite stay silent — the goal is a one-line breadcrumb the
// operator can grep for to learn WHICH key fingerprint they need to
// install, not a per-tick log flood. rejected carries the rejected
// signers as "fpr:<40-hex>" (long fingerprint form) or "keyid:<16-hex>"
// (short-keyid form, when subpacket 33 was absent and the keyid did
// not resolve in the trust set).
func (v *Verifier) logUntrustedSignerOnce(suite freshness.SuiteRef, rejected []string) {
	key := suite.CanonicalHost + "|" + suite.SuitePath
	if _, loaded := v.untrustedSignerLogged.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	v.logger.Info("adoption_untrusted_signer",
		"canonical_host", suite.CanonicalHost,
		"suite_path", suite.SuitePath,
		"rejected_signers", rejected,
	)
}

// logShortKeyIDFallbackOnce emits one INFO log per (canonical_host,
// suite_path) per process the first time we accept a signature via
// the short-keyid fallback. Subsequent acceptances for the same
// suite stay silent — this keeps the log surface a finite inventory
// of "which repos rely on the legacy form" rather than a per-check
// noise stream.
func (v *Verifier) logShortKeyIDFallbackOnce(suite freshness.SuiteRef, matched *openpgp.Entity, keyID uint64) {
	key := suite.CanonicalHost + "|" + suite.SuitePath
	if _, loaded := v.shortKeyIDLogged.LoadOrStore(key, struct{}{}); loaded {
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
// suite_path) per process the first time accept_any_signer's bypass
// branch accepts a suite. Subsequent acceptances for the same suite
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
	if _, loaded := v.unverifiedSignerLogged.LoadOrStore(key, struct{}{}); loaded {
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
// contain a parseable PGP signature packet must still be rejected —
// otherwise raw non-armored garbage (which decodeMaybeArmoredSignature
// only size-caps) would silently pass through to the pool.
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
