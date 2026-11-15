package freshness

import (
	"context"
	"errors"
	"fmt"

	"github.com/linsomniac/apt-cacher-ultra/internal/indexrecovery"
)

func memberDeclarations(members []ReleaseMember) map[string]ReleaseMember {
	out := make(map[string]ReleaseMember, len(members))
	for _, m := range members {
		out[m.Path] = m
	}
	return out
}

func newRepresentationErr(kind error, path, detail string) *AdoptionMemberError {
	e := newMemberErr(kind, path, detail)
	e.representationMismatch = true
	return e
}

// adoptMemberFromRelease recovers full plain indexes whose upstream response
// is the wrong representation (issue #32, Apache MultiViews). The declaration
// map MUST come from the very Release supplying m, never a newer upstream
// anchor. Repairs/reconciliation pass the stored snapshot's declarations.
//
// An explicit sibling fetch avoids trusting response headers and gives that
// file its own declared download limit, even when gzip is larger than plain.
// Siblings already fetched by this adoption are reused through adoptMember's
// rehash-on-reuse path. No nested host slot is held and no recursive fallback
// occurs. HTTP authorization, transport and local storage failures keep their
// existing behavior; only a completed wrong representation can be recovered.
func (a *Adopter) adoptMemberFromRelease(ctx context.Context, suite SuiteRef, m ReleaseMember, probe memberProbe, declared map[string]ReleaseMember) (string, bool, error) {
	hash, reused, originalErr := a.adoptMember(ctx, suite, m, probe)
	var me *AdoptionMemberError
	if originalErr == nil || ctx.Err() != nil || !indexrecovery.PlainIndex(m.Path) ||
		m.Size < 0 || m.Size > indexrecovery.MaxBytes || declared[m.Path] != m ||
		!errors.As(originalErr, &me) || !me.representationMismatch {
		return hash, reused, originalErr
	}
	for _, suffix := range []string{".gz", ".xz"} {
		sibling, ok := declared[m.Path+suffix]
		if !ok || sibling.Size > indexrecovery.MaxBytes {
			continue
		}
		// The sibling keeps the full sequence for the suite's form: by hash
		// first where the plain member was asked by path first (it is a
		// member in its own right, fetched or reused under its own lock).
		siblingProbe := probe
		if siblingProbe == probePathFirst {
			siblingProbe = probeByHashFirst
		}
		blob, siblingReused, err := a.adoptMember(ctx, suite, sibling, siblingProbe)
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		if errors.Is(err, ErrAdoptionDBFailed) {
			return "", false, err
		}
		if err == nil {
			hash, err = indexrecovery.Reconstruct(ctx, a.cache, indexrecovery.Member(m), indexrecovery.Member(sibling), blob)
			if errors.Is(err, indexrecovery.ErrStorage) {
				return "", false, fmt.Errorf("%w: %w", ErrAdoptionDBFailed, err)
			}
			if err == nil {
				// Derived plaintext has no origin Last-Modified. Content ETags
				// remain authoritative; never copy the compressed file's date.
				if err := a.cache.PutRepresentation(ctx, suite.CanonicalScheme, suite.CanonicalHost, suite.SuitePath+"/"+m.Path, hash, ""); err != nil {
					return "", false, fmt.Errorf("%w: recovered member metadata: %w", ErrAdoptionDBFailed, err)
				}
				a.logger.Info("index_reconstructed", "canonical_host", suite.CanonicalHost,
					"suite_path", suite.SuitePath, "path", m.Path, "compressed_path", sibling.Path)
				// Reused when the sibling was: nothing was downloaded for
				// this member beyond the response that failed.
				return hash, siblingReused, nil
			}
		}
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		a.logger.Debug("index_reconstruction_failed", "canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath, "path", m.Path, "compressed_path", sibling.Path, "err", err)
	}
	return "", false, originalErr
}
