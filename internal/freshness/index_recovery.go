package freshness

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
// anchor. Reconciliation passes the stored snapshot's declarations.
//
// An explicit sibling fetch avoids trusting response headers and gives that
// file its own declared download limit, even when gzip is larger than plain.
// Siblings already fetched by this adoption are reused through adoptMember's
// rehash-on-reuse path. No nested host slot is held and no recursive fallback
// occurs. HTTP authorization, transport and local storage failures keep their
// existing behavior; only a completed wrong representation can be recovered.
func (a *Adopter) adoptMemberFromRelease(ctx context.Context, suite SuiteRef, m ReleaseMember, byHash bool, declared map[string]ReleaseMember) (string, error) {
	hash, originalErr := a.adoptMember(ctx, suite, m, byHash)
	var me *AdoptionMemberError
	if originalErr == nil || ctx.Err() != nil || !indexrecovery.PlainIndex(m.Path) ||
		m.Size < 0 || m.Size > indexrecovery.MaxBytes || declared[m.Path] != m ||
		!errors.As(originalErr, &me) || !me.representationMismatch {
		return hash, originalErr
	}
	for _, suffix := range []string{".gz", ".xz"} {
		sibling, ok := declared[m.Path+suffix]
		if !ok || sibling.Size > indexrecovery.MaxBytes {
			continue
		}
		blob, err := a.adoptMember(ctx, suite, sibling, byHash)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if errors.Is(err, ErrAdoptionDBFailed) {
			return "", err
		}
		if err == nil {
			hash, err = indexrecovery.Reconstruct(ctx, a.cache, recoveryMember(m), recoveryMember(sibling), blob)
			if errors.Is(err, indexrecovery.ErrStorage) {
				return "", fmt.Errorf("%w: %w", ErrAdoptionDBFailed, err)
			}
			if err == nil {
				a.logger.Info("index_reconstructed", "canonical_host", suite.CanonicalHost,
					"suite_path", suite.SuitePath, "path", m.Path, "compressed_path", sibling.Path)
				return hash, nil
			}
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		a.logger.Debug("index_reconstruction_failed", "canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath, "path", m.Path, "compressed_path", sibling.Path, "err", err)
	}
	return "", originalErr
}

// 1.0.x parses SHA256 declarations; preserve that verification policy.
func recoveryMember(m ReleaseMember) indexrecovery.Member {
	return indexrecovery.Member{Path: m.Path, SHA256: m.SHA256, Size: m.Size}
}

func recoveryDownloadLimit(m ReleaseMember) int64 {
	plain := strings.TrimSuffix(strings.TrimSuffix(m.Path, ".gz"), ".xz")
	if indexrecovery.PlainIndex(plain) && m.Size >= 0 {
		return max(m.Size, 1)
	}
	return 0
}
