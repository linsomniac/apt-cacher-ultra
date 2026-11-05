package freshness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"runtime/debug"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// signerGuardAnchorCap bounds the read of a current anchor the signer
// downgrade guard re-verifies: far above the fetch caps an anchor was
// adopted under (defaultMaxInReleaseBytes), so a stored anchor is never
// too large unless its pool file is not the anchor at all. A variable for
// the tests.
var signerGuardAnchorCap int64 = 64 << 20

// The reasons adoption_signer_guard_skipped gives: why the guard could
// not read the current snapshot's anchor and stood down (ruling G2).
const (
	guardSkipAnchorMissing    = "anchor_missing"       // the snapshot names no anchor blob, or its pool file is gone
	guardSkipAnchorUnreadable = "anchor_unreadable"    // its pool file could not be read
	guardSkipAnchorTooLarge   = "anchor_too_large"     // over signerGuardAnchorCap
	guardSkipHashMismatch     = "anchor_hash_mismatch" // the bytes read are not the snapshot's anchor
	guardSkipUnverified       = "anchor_unverified"    // the verifier refuses it now, or cannot read it (a legacy anchor)
	guardSkipUnknownForm      = "unknown_form"         // a form the guard does not know
)

// signerGuard is the signer downgrade guard (the hardening plan's Task 9d,
// ruling HD-19): it refuses a candidate whose verified anchor has no strong
// signer (fresh, the verifier's report of it; the unsigned form has none)
// when the suite's current snapshot's stored anchor, verified again now by
// the same verifier and keyring, has one. The refusal is
// ErrAdoptionSignerDowngrade (signer_downgrade, a refusal state), logged
// at WARN as adoption_signer_downgrade; the current snapshot keeps
// serving.
//
// It stands down — the adoption goes on as before Task 9d — when there is
// no current snapshot (a first adoption, or one after /forget: ruling G6),
// when the candidate has a strong signer (the current anchor is then not
// read), when the verifier reports no signers (not a SignerVerifier), when
// the current anchor is unsigned or no longer has a strong signer, and when
// it cannot be read or verified (ruling G2, logged at INFO as
// adoption_signer_guard_skipped).
//
// runShared calls it at Step 4b: after the Date rules and Step 4's
// rollback refusal, before any member download.
//
// AIDEV-NOTE: why after the rollback check (the task review's 9d-M1): a
// flapping mirror that serves an adopted, displaced snapshot again is a
// rollback (rollback_refused: the cache is current, no lag), even when
// that older anchor is weak-only after a strong current one; only
// genuinely new content that lost its strong signer is signer_downgrade.
//
// AIDEV-NOTE: the guard fails open on its own errors (ruling G2): it is
// defence in depth behind a candidate that has already passed full
// verification, so a guard fault must never freeze a suite; a strong flag
// that is present and computed is always enforced. Both flags come from
// one verifier under one per-host configuration (ruling G1): under
// accept_any_signer's bypass no packet is verified, neither anchor has a
// strong signer, and the guard never fires. Its recourse (ruling G4; HD-17:
// no override, no config key), in this order (the final review's F2-I2):
//   - a vendor that rotated to a new strong key the keyring lacks still
//     signs with its weak co-signer: the refusal is the rotation, not a
//     strip, and the WARN's uncounted_signers names the new key as "not in
//     the trust set". Installing the vendor's new key, confirmed out of
//     band, and restarting clears it and keeps the guard armed: the next
//     candidate has a strong signer again. A vendor signing again with a
//     strong key the keyring holds clears it by itself.
//   - POST /forget (forget.go) deletes the suite's row, so its next
//     adoption is a first one and the guard stands down (ruling G6): it
//     adopts the anchor the guard refused, the very thing it refused.
//   - Removing the old strong key from the keyring (and restarting): the
//     current anchor's strong packet is then a key the cache lacks, so it
//     has no strong signer and the guard stands down for the suite until a
//     strong key is installed again. Where the strong and weak keys share
//     one key file, that means editing the vendor's file.
//
// accept_any_signer would clear the refusal too, by verifying no signature
// for the host's suites (no anchor has a strong signer under the bypass):
// that is not a recourse. Its Open gap: a strip at a suite's first adoption
// is not caught (docs/open-decisions.md, *Open*: "The signer downgrade
// guard cannot see a strip at a suite's first adoption").
func (a *Adopter) signerGuard(ctx context.Context, suite SuiteRef, prior *cache.SuiteSnapshot, fresh Signers) error {
	if a.signers == nil || prior == nil || fresh.Strong != "" {
		return nil
	}
	current, ok := a.currentSigners(ctx, suite, prior)
	if !ok || current.Strong == "" {
		return nil
	}
	a.logger.Warn("adoption_signer_downgrade",
		"canonical_host", suite.CanonicalHost,
		"suite_path", suite.SuitePath,
		"current_snapshot_id", prior.SnapshotID,
		"current_strong_signer", current.Strong,
		"new_signers", fresh.Counted,
		"uncounted_signers", fresh.Uncounted,
	)
	return fmt.Errorf("%w: the current snapshot %d's anchor has the strong signer %s, the candidate none (its counted signers: %v; not counted: %v)",
		ErrAdoptionSignerDowngrade, prior.SnapshotID, current.Strong, fresh.Counted, fresh.Uncounted)
}

// currentSigners verifies prior's stored anchor again, with the adoption's
// verifier, and returns its Signers; ok is false when the guard must stand
// down because it cannot (logged: adoption_signer_guard_skipped). An
// unsigned anchor has no signers (ok).
//
// AIDEV-NOTE: the final review's F2-M1. The guard is the one place the
// verifier runs over a stored anchor it may never have run over (a 1.0.x
// snapshot's after an upgrade, the current anchor under a changed
// keyring). A panic there must not become the adoption's panic failure,
// which the next weak-only candidate would hit again and freeze the suite
// (against ruling G2): it is recovered here, logged at WARN as a bug
// (adoption_signer_guard_panicked), and stands the guard down as an
// anchor_unverified.
func (a *Adopter) currentSigners(ctx context.Context, suite SuiteRef, prior *cache.SuiteSnapshot) (signers Signers, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			a.logger.Warn("adoption_signer_guard_panicked",
				"canonical_host", suite.CanonicalHost,
				"suite_path", suite.SuitePath,
				"current_snapshot_id", prior.SnapshotID,
				"panic", fmt.Sprint(r),
				"stack", string(debug.Stack()),
			)
			signers, ok = Signers{}, false
			a.logger.Info("adoption_signer_guard_skipped",
				"canonical_host", suite.CanonicalHost,
				"suite_path", suite.SuitePath,
				"current_snapshot_id", prior.SnapshotID,
				"reason", guardSkipUnverified,
				"err", fmt.Sprintf("the verifier panicked: %v", r),
			)
		}
	}()
	skip := func(reason string, err error) (Signers, bool) {
		a.logger.Info("adoption_signer_guard_skipped",
			"canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath,
			"current_snapshot_id", prior.SnapshotID,
			"reason", reason,
			"err", err,
		)
		return Signers{}, false
	}
	var err error
	switch prior.Form {
	case cache.FormUnsigned:
		return Signers{}, true
	case cache.FormInline:
		inline, reason, rerr := a.readAnchorBlob(prior.SingleAnchorHash)
		if rerr != nil {
			return skip(reason, rerr)
		}
		_, signers, err = a.signers.VerifyInlineSigners(ctx, suite, inline)
	case cache.FormDetached:
		release, reason, rerr := a.readAnchorBlob(prior.ReleaseHash)
		if rerr != nil {
			return skip(reason, rerr)
		}
		sig, reason, rerr := a.readAnchorBlob(prior.ReleaseGPGHash)
		if rerr != nil {
			return skip(reason, rerr)
		}
		_, signers, err = a.signers.VerifyDetachedSigners(ctx, suite, release, sig)
	default:
		return skip(guardSkipUnknownForm, fmt.Errorf("anchor form %q", prior.Form))
	}
	if err != nil {
		return skip(guardSkipUnverified, err)
	}
	return signers, true
}

// readAnchorBlob reads the pool file of a stored anchor whose sha256 is
// *hash, checked to be those bytes; on an error, the reason the guard
// stands down for (adoption_signer_guard_skipped).
func (a *Adopter) readAnchorBlob(hash *string) ([]byte, string, error) {
	if hash == nil {
		return nil, guardSkipAnchorMissing, errors.New("the snapshot names no anchor blob")
	}
	b, err := readCapped(a.cache.BlobPath(*hash), signerGuardAnchorCap)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, guardSkipAnchorMissing, err
	case errors.Is(err, errAnchorTooLarge):
		return nil, guardSkipAnchorTooLarge, err
	case err != nil:
		return nil, guardSkipAnchorUnreadable, err
	}
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != *hash {
		return nil, guardSkipHashMismatch, fmt.Errorf("pool file %s holds bytes of sha256 %x", *hash, sum)
	}
	return b, "", nil
}
