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
//  1. Resolve per-suite trust set; if RequirePinned and no match, abort.
//  2. Decode clearsigned block; if absent and RequireSignature, abort.
//     If absent and !RequireSignature, return body verbatim.
//  3. Inspect signature packet(s); reject any without IssuerFingerprint.
//  4. Confirm IssuerFingerprint is in the trust set fingerprints.
//  5. Cryptographic verification against the trust-set EntityList.
//     This step also catches expired/revoked signing keys (the
//     openpgp library skips keys that fail the validity gate).
//
// ctx is accepted for interface compatibility but VerifyInline does
// no I/O — verification is CPU-bound and short.
func (v *Verifier) VerifyInline(ctx context.Context, suite freshness.SuiteRef, inRelease []byte) ([]byte, error) {
	_ = ctx

	// Step 1: per-suite trust-set resolution.
	trustSet, trustFPs, pinned, err := v.resolveTrustSet(suite)
	if err != nil {
		return nil, err
	}

	// Step 2: clearsign decode.
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

	// Step 3: read armored signature once into a buffer so we can
	// both inspect its packets and re-verify cryptographically.
	sigBytes, err := readArmoredSignature(block.ArmoredSignature.Body)
	if err != nil {
		return nil, fmt.Errorf("read signature: %w", err)
	}

	// Step 3+4: enforce IssuerFingerprint and trust-set membership.
	matched, err := v.findUsableSignature(sigBytes, trustFPs, pinned)
	if err != nil {
		return nil, err
	}
	_ = matched

	// Step 5: cryptographic verification. The trust-set EntityList
	// is constructed so only entities holding a trusted fingerprint
	// participate; the openpgp library returns ErrUnknownIssuer if
	// the signature's keyid resolves to an entity outside the set,
	// and rejects expired/revoked signing keys at the same gate.
	signer, verr := openpgp.CheckDetachedSignature(
		trustSet,
		bytes.NewReader(block.Bytes),
		bytes.NewReader(sigBytes),
		nil,
	)
	if verr != nil {
		return nil, fmt.Errorf("signature verification failed: %w", verr)
	}
	_ = signer

	return block.Plaintext, nil
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

// findUsableSignature walks the signature packets in sigBytes and
// returns the first one whose IssuerFingerprint is in trustFPs. If
// any packet lacks IssuerFingerprint, the function returns ErrShortKeyID
// — this is a per-signature policy check, NOT "any signature without
// IssuerFingerprint is rejected": if the block contains multiple
// signatures we accept the first that satisfies long-form + trust.
//
// SPEC2 §7.6.3 says short-keyid matches are insufficient and rejected.
// We interpret this as: the long-form fingerprint MUST be present on
// every signature we'd consider. A signature lacking IssuerFingerprint
// cannot be considered, so if it's the only signature and the others
// also lack IssuerFingerprint, we return ErrShortKeyID; if at least one
// signature has IssuerFingerprint but it's outside trustFPs, we return
// ErrUntrustedSigner; if all signatures fail one of those filters, we
// return ErrNoUsableSignature.
func (v *Verifier) findUsableSignature(sigBytes []byte, trustFPs map[string]struct{}, pinned bool) (*packet.Signature, error) {
	reader := packet.NewReader(bytes.NewReader(sigBytes))
	var (
		anyShortKeyID bool
		anyUntrusted  bool
	)
	for {
		pkt, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read signature packet: %w", err)
		}
		sig, ok := pkt.(*packet.Signature)
		if !ok {
			continue
		}
		if len(sig.IssuerFingerprint) == 0 {
			anyShortKeyID = true
			continue
		}
		issuerFP := upperFP(sig.IssuerFingerprint)
		if _, ok := trustFPs[issuerFP]; !ok {
			anyUntrusted = true
			continue
		}
		return sig, nil
	}
	switch {
	case anyShortKeyID && !anyUntrusted:
		return nil, ErrShortKeyID
	case anyUntrusted:
		_ = pinned // explanatory: the error semantics are the same with or without pinning
		return nil, fmt.Errorf("%w: IssuerFingerprint not in trust set", ErrUntrustedSigner)
	default:
		return nil, ErrNoUsableSignature
	}
}
