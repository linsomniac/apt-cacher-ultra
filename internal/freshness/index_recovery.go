package freshness

import (
	"context"
	"errors"
	"fmt"

	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
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
// Any other plain member answered with a compressed sibling's bytes is not
// rebuilt: it is skipped as not published, with
// errAdoptionMemberSiblingServed (servedAsSibling says why).
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
	if originalErr == nil || ctx.Err() != nil || declared[m.Path] != m ||
		!errors.As(originalErr, &me) || !me.representationMismatch {
		return hash, reused, originalErr
	}
	if !indexrecovery.PlainIndex(m.Path) {
		// Any other plain file is not rebuilt. When the answer was a
		// compressed sibling's bytes, upstream does not publish it.
		sibling, ok := a.servedAsSibling(ctx, suite, m, me, declared)
		if !ok {
			return hash, reused, originalErr
		}
		a.adoptionLogger(ctx).Warn("adoption_member_skipped",
			"canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath,
			"path", m.Path,
			"declared_sha256", m.SHA256,
			"declared_sha512", m.SHA512,
			"reason", skipReasonSiblingServed,
			"served_as", sibling.Path,
		)
		adoptionMembersSkippedTotal.Inc(skipReasonSiblingServed)
		return "", false, errAdoptionMemberSiblingServed
	}
	if m.Size < 0 || m.Size > indexrecovery.MaxBytes {
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

// skipReasonSiblingServed is adoption_member_skipped's reason (and the
// acu_adoption_members_skipped_total label) for a plain member whose URL
// upstream answers with a compressed sibling's bytes.
const skipReasonSiblingServed = "compressed_sibling_served"

// compressedSuffixes are the extensions a Release lists a file's
// compressed forms under (apt's Acquire::CompressionTypes).
var compressedSuffixes = []string{".gz", ".xz", ".bz2", ".lzma", ".zst", ".lz4"}

// servedAsSibling reports which compressed sibling of m, if any, upstream
// answers m's own URL with: the bytes must be exactly the sibling's, by the
// length and every hash the same Release declares for it. me is the
// failure of m's fetch.
//
// AIDEV-NOTE: this is how a repository behind Apache's content negotiation
// (MultiViews) looks (packages.icinga.com, 2026-11; issue #32 was the same
// server behaviour for Packages and Sources). Its Release lists
// Contents-amd64 and Contents-amd64.gz and it publishes the .gz only, as
// Ubuntu does; but where Ubuntu answers the plain name 404, it answers 200
// with the .gz. apt never asks for the plain name while a compressed one
// is listed, so nothing is wrong for apt. Treated as a failure that may
// heal, the member cost every publication two retry rounds and was fetched
// again by the repair pass at every unchanged check, without end: 5,483
// adoption_member_repair_failed lines in a month on one cache. Treated as
// what it is, the plain file not being published, it is skipped like a 404
// (adoptMemberFromRelease) and a record the repair pass holds for it is
// retired (RepairSkippedMembers).
//
// Only the sibling's exact bytes count: a length alone is no evidence, and
// an upstream that can alter responses gains nothing it does not have with
// a 404. The answer that failed decides when the fetch read it to its end
// (me.served); it costs no request then, and bytes that are no sibling's
// (another generation of the file, mid-sync) cost none either. When the
// fetch stopped at m's download limit, because the sibling is longer than
// the plain file (an empty or tiny one), m's URL is asked once more with
// each such sibling's own limit.
func (a *Adopter) servedAsSibling(ctx context.Context, suite SuiteRef, m ReleaseMember, me *AdoptionMemberError, declared map[string]ReleaseMember) (ReleaseMember, bool) {
	for _, suffix := range compressedSuffixes {
		sibling, ok := declared[m.Path+suffix]
		if !ok {
			continue
		}
		if me.served != nil {
			if me.served.are(sibling) {
				return sibling, true
			}
			continue
		}
		// The body ran over m's limit: a sibling no longer than m is not it.
		if sibling.Size <= m.Size || ctx.Err() != nil {
			continue
		}
		if a.urlServes(ctx, suite, m.Path, sibling) {
			return sibling, true
		}
	}
	return ReleaseMember{}, false
}

// urlServes fetches relPath of the suite and reports whether the answer is
// exactly want's bytes (memberWriter.holds), reading no further than
// want's length. Nothing is kept: the bytes never reach pool/, no row is
// written, and any failure is "no".
func (a *Adopter) urlServes(ctx context.Context, suite SuiteRef, relPath string, want ReleaseMember) bool {
	release, err := a.hostSem.Acquire(ctx, suite.CanonicalHost)
	if err != nil {
		return false
	}
	defer release()
	w, err := newMemberWriter(a.cache)
	if err != nil {
		return false
	}
	defer func() { _ = w.Abort() }()
	target := &fetch.Target{
		CanonicalHost: suite.CanonicalHost,
		URL:           buildMemberURL(suite, relPath),
		MaxBytes:      memberMaxBytes(want),
		NoIdleRetry:   true,
	}
	if _, err := a.fetcher.Fetch(fetch.WithPurpose(ctx, "adoption_member"), target, w); err != nil {
		return false
	}
	return w.holds(want)
}
