package gpg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"

	"github.com/ProtonMail/go-crypto/openpgp"
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
	Logger           *slog.Logger
}

// Verifier implements freshness.Verifier using the host apt keyring
// narrowed per-suite by SPEC2 §7.6.2 trust-set rules. One Verifier
// instance is shared across all suites.
type Verifier struct {
	keyring          *Keyring
	pins             []SignerPin
	requireSignature bool
	requirePinned    bool
	logger           *slog.Logger
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
		logger:           logger,
	}, nil
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
	trustSet, trustFPs, _, err := v.resolveTrustSet(suite)
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
	if err := v.verifyAnyTrusted(trustSet, trustFPs, block.Bytes, sigBytes); err != nil {
		return nil, err
	}

	return block.Plaintext, nil
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
//   - trustSet: the openpgp.EntityList to verify against
//   - trustFPs: the uppercase-fingerprint set (for IssuerFingerprint check)
//   - pinned:   true iff at least one [[trusted_signer]] block matched
//   - err:      ErrUnpinnedSuite when RequirePinned and no pin matched
func (v *Verifier) resolveTrustSet(suite freshness.SuiteRef) (openpgp.EntityList, map[string]struct{}, bool, error) {
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
			return nil, nil, false, fmt.Errorf("%w: no [[trusted_signer]] block matches %q",
				ErrUnpinnedSuite, suite.CanonicalHost)
		}
		// Broad trust: the entire host keyring. Build a copy of the
		// host fingerprint set so the IssuerFingerprint check has
		// something to consult.
		fps := make(map[string]struct{}, len(v.keyring.fingerprints))
		for fp := range v.keyring.fingerprints {
			fps[fp] = struct{}{}
		}
		return v.keyring.EntityList(), fps, false, nil
	}
	union := make(map[string]struct{})
	for _, m := range matched {
		for fp := range m.Fingerprints {
			union[fp] = struct{}{}
		}
	}
	subset := v.keyring.Subset(union)
	return subset, union, true, nil
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
// accepts on the FIRST one that passes BOTH (a) the long-form
// IssuerFingerprint trust check and (b) cryptographic verification
// against trustSet. Coupling these checks to the same packet is the
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
//   - Else if all packets lacked IssuerFingerprint, return ErrShortKeyID.
//   - Else (no signature packets at all), ErrNoUsableSignature.
func (v *Verifier) verifyAnyTrusted(
	trustSet openpgp.EntityList,
	trustFPs map[string]struct{},
	signed []byte,
	sigBytes []byte,
) error {
	reader := packet.NewReader(bytes.NewReader(sigBytes))
	var (
		anyShortKeyID  bool
		anyUntrusted   bool
		sawSignature   bool
		lastVerifyErr  error
		anyTrustedSeen bool
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
		if len(sig.IssuerFingerprint) == 0 {
			anyShortKeyID = true
			continue
		}
		issuerFP := upperFP(sig.IssuerFingerprint)
		if _, ok := trustFPs[issuerFP]; !ok {
			anyUntrusted = true
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
		return fmt.Errorf("%w: IssuerFingerprint not in trust set", ErrUntrustedSigner)
	case anyShortKeyID:
		return ErrShortKeyID
	default:
		return ErrNoUsableSignature
	}
}
