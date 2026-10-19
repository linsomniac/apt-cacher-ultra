package freshness

// Spec C5 "Unsigned safeguards" (D6), the adopter's side: the unsigned
// form adopts a Release no signature covers, so it never vouches a .deb,
// never counts for strict mode, never writes a signed-seen marker, and is
// refused whenever the suite has signed history.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// unsignedMemberCap is spec C5 safeguard 4 (a C11 constant): an unsigned
// Release declaring more members than this is refused.
const unsignedMemberCap = 1000

var (
	// ErrAdoptionUnsignedDowngradeRefused refuses unsigned metadata for a
	// suite with signed history: a signed-seen marker for its base, a
	// signed current snapshot, or signed evidence the adopter could not
	// rule out (spec C5 safeguard 3; ruling R-E1). A refusal state, not a
	// failure (spec C7): recorded as unsigned_downgrade_refused.
	ErrAdoptionUnsignedDowngradeRefused = errors.New("adoption_unsigned_downgrade_refused")

	// ErrAdoptionUnsignedTooLarge refuses an unsigned Release that declares
	// more than unsignedMemberCap members (spec C5 safeguard 4). A refusal
	// state: recorded as unsigned_too_large.
	ErrAdoptionUnsignedTooLarge = errors.New("adoption_unsigned_too_large")

	// errUnsignedDisabled: adoption.adopt_unsigned is false. The checker
	// never requests an unsigned adoption then; this guards RunUnsigned.
	errUnsignedDisabled = errors.New("unsigned adoption is disabled (adoption.adopt_unsigned = false)")
)

// RunUnsigned executes the adoption flow for an unsigned suite: release is
// the Release body no signature covers (spec C5 "unsigned" form). Like
// Run and RunDetached it is synchronous, and the caller holds the suite
// mutex. The checker calls it only for a confirmed candidate (two
// observations, spec C5 safeguard 2); runShared re-checks eligibility and
// the commit re-checks never-downgrade in its transaction.
func (a *Adopter) RunUnsigned(ctx context.Context, suite SuiteRef, release []byte, etag, lastmod string) error {
	return a.runShared(ctx, suite, &adoptionPayload{
		form:         adoptionFormUnsigned,
		releaseBytes: release,
	}, etag, lastmod)
}

// unsignedEligible is spec C5's unsigned eligibility, checked before
// anything is downloaded (the "order of checks in runShared") and by the
// check before an unsigned request takes a slot: the knob is on, and the
// suite has no signed history — cache.CheckUnsignedDowngrade, the rule
// CommitAdoption re-reads in its transaction (a signed-seen marker for the
// base, or an adopted signed snapshot of the suite or its /. twin, under
// either scheme). It is the first of the two never-downgrade points; the
// second, the authority, is the commit's in-transaction recheck.
func (a *Adopter) unsignedEligible(ctx context.Context, suite SuiteRef) error {
	if !a.adoptUnsigned {
		return errUnsignedDisabled
	}
	err := a.cache.CheckUnsignedDowngrade(ctx, suite.CanonicalHost, suite.SuitePath)
	switch {
	case errors.Is(err, cache.ErrUnsignedDowngradeRefused):
		// %v, not %w: cache.ErrUnsignedDowngradeRefused in the chain means
		// the commit itself refused (runShared's Step 11).
		return fmt.Errorf("%w: %v", ErrAdoptionUnsignedDowngradeRefused, err)
	case err != nil:
		return fmt.Errorf("%w: %w", ErrAdoptionDBFailed, err)
	}
	return nil
}

// unsignedCap is spec C5 safeguard 4: ErrAdoptionUnsignedTooLarge when an
// unsigned Release declares more than unsignedMemberCap members (n is
// ParseRelease's count of merged member paths, the members runShared
// would fetch).
func unsignedCap(n int) error {
	if n <= unsignedMemberCap {
		return nil
	}
	return fmt.Errorf("%w: %d declared members, at most %d", ErrAdoptionUnsignedTooLarge, n, unsignedMemberCap)
}

// errRemarkPoolRead marks a re-mark that failed reading a pool file: an I/O
// fault, logged at ERROR (any other re-mark failure is a WARN).
var errRemarkPoolRead = errors.New("read the pool file")

// remarkSignedAnchors re-runs, for an unsigned commit, the cached
// signed-anchor mark of the suite (ruling R-E1, phase D's backstop rule):
// it reads the InRelease and Release.gpg url_path rows of the suite's base
// and of its /. twin (aptpath.MarkerBase(s) and MarkerBase(s)+"/.", which
// share the marker), sniffs their pool files here — never on the writer —
// and marks the base for each one that passes, stamped with the row's last
// use. The mark is enqueued and committed before the commit is enqueued, so
// the commit's in-transaction re-read (FIFO writer) sees it. A pool file
// that does not exist is no evidence (as in the startup marker pass); any
// other read error (errRemarkPoolRead for a pool file), and a failed mark,
// is an error: the caller refuses the commit.
//
// AIDEV-NOTE: this closes phase D's backstop residual where it matters — a
// tracked suite whose sniff-passing signed anchor stayed unmarked after a
// failed backstop, including one cached under the /. twin's path (design
// review I-3). Fail closed: never commit unsigned while signed evidence
// could not be ruled out, so a refusal from here can mean "could not rule
// out" (an I/O or database fault), not only "signed".
func (a *Adopter) remarkSignedAnchors(ctx context.Context, suite SuiteRef) error {
	base := aptpath.MarkerBase(suite.SuitePath)
	var marks []cache.SignedSeenMark
	for _, dir := range []string{base, base + "/."} {
		for _, name := range []string{"InRelease", "Release.gpg"} {
			p := dir + "/" + name
			row, err := a.cache.LookupURL(ctx, suite.CanonicalScheme, suite.CanonicalHost, p)
			switch {
			case errors.Is(err, cache.ErrNotFound):
				continue
			case err != nil:
				return fmt.Errorf("read %s: %w", p, err)
			case row.BlobHash == nil:
				continue
			}
			signed, err := sniffCachedAnchor(a.cache, p, *row.BlobHash)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				continue
			case err != nil:
				return fmt.Errorf("%w of %s: %w", errRemarkPoolRead, p, err)
			case !signed:
				continue
			}
			seen := anchorLastUse(row)
			if seen <= 0 {
				seen = wallclock.Unix()
			}
			marks = append(marks, cache.SignedSeenMark{CanonicalScheme: suite.CanonicalScheme,
				CanonicalHost: suite.CanonicalHost, Base: base, SeenAt: seen})
		}
	}
	if len(marks) == 0 {
		return nil
	}
	if err := a.cache.MarkSignedSeenBatch(ctx, marks); err != nil {
		return fmt.Errorf("mark: %w", err)
	}
	return nil
}

// anchorLastUse is a url_path row's last use — the newer of its
// last_requested_at and last_fetched_at (0 when neither is set) — the
// stamp a marker from it carries, as in the startup marker pass and the
// handler's backstop.
func anchorLastUse(row *cache.URLPath) int64 {
	var seen int64
	if row.LastRequestedAt != nil {
		seen = *row.LastRequestedAt
	}
	if row.LastFetchedAt != nil && *row.LastFetchedAt > seen {
		seen = *row.LastFetchedAt
	}
	return seen
}
