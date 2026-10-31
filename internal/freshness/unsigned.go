package freshness

// Spec C5 "Unsigned safeguards" (D6), the adopter's side — and the
// checker's refusals before a slot is taken (unsignedRefusal): the
// unsigned form adopts a Release no signature covers, so it never vouches
// a .deb, never counts for strict mode, never writes a signed-seen marker,
// and is refused whenever the suite has signed history.

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
	// never requests an unsigned adoption then (it records the refusal
	// unsigned_disabled, ruling PE-12); this guards RunUnsigned.
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

// AdoptsUnsigned reports adoption.adopt_unsigned as the adopter was built
// with (the startup line logs it).
func (a *Adopter) AdoptsUnsigned() bool { return a.adoptUnsigned }

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

// unsignedTooLarge is spec C5 safeguard 4 on an unsigned Release before a
// slot is taken (design review M-5): unsignedCap over ParseRelease's merged
// members. A Release that does not parse is not refused here: the adoption
// reports the parse failure, and runShared applies the cap again.
func unsignedTooLarge(release []byte) error {
	members, err := ParseRelease(release)
	if err != nil {
		return nil
	}
	return unsignedCap(len(members))
}

// unsignedRefusal applies spec C5's refusals to an unsigned request before
// a slot is taken — the first adoption's (unsignedFirst) and an unsigned
// suite's update (checkUnsigned, anchorFoundAgain): the knob
// (adoption.adopt_unsigned off: unsigned_disabled, ruling PE-12),
// never-downgrade (Adopter.unsignedEligible's signed history:
// unsigned_downgrade_refused) and the member cap (unsignedTooLarge:
// unsigned_too_large; design review M-5). It records the refusal and
// returns its reason, "" to go on, or an error — a read error — for which
// the caller fails the check.
//
// AIDEV-NOTE: ruling PE-12 (final fix wave FW-2). The knob off is a
// refusal state, never a failed check: the record makes it visible
// ("refused: unsigned_disabled") and paces the suite — a never-adopted one
// by PE-3 (pacedFirst), an established unsigned one by pacedByRefusal and
// suiteDue — so checks inside its window ask upstream nothing. The
// established suite keeps serving its last snapshot, coherent and never
// updated unsigned, until the knob is turned back on, the suite is
// forgotten (POST /forget) or it is reaped as idle — forgotten or reaped
// with the knob still off, it is then a never-adopted suite, which is
// worse: the forget or reap deletes its cached index files first
// (DeleteSuiteMetadata), so its next requests fetch upstream's current
// files, but each then stays as first stored, never refreshed, and an
// upstream publish can bring Hash Sum mismatches. Ruling PF-5 dropped the
// retirement PE-12 left to phase F for the same reason — returned to the
// ordinary cache, each index file would be frozen at its own first fetch,
// and every upstream publish would bring Hash Sum mismatches that never
// clear (TestServeHTTP_KnobOffKeepsServingTheUnsignedSnapshot). Before
// PE-12 the knob off failed every check, with no record: an established
// suite froze unpaced and invisible (final reviews E1, E2).
func (c *Checker) unsignedRefusal(ctx context.Context, req *adoptionRequest) (string, error) {
	switch err := c.adopter.unsignedEligible(ctx, req.suite); {
	case errors.Is(err, errUnsignedDisabled):
		c.recordAdoptionFailure(req, cache.ReasonUnsignedDisabled)
		return cache.ReasonUnsignedDisabled, nil
	case errors.Is(err, ErrAdoptionUnsignedDowngradeRefused):
		c.recordAdoptionFailure(req, cache.ReasonUnsignedDowngradeRefused)
		return cache.ReasonUnsignedDowngradeRefused, nil
	case err != nil:
		return "", err
	}
	if err := unsignedTooLarge(req.releaseBytes); err != nil {
		c.recordAdoptionFailure(req, cache.ReasonUnsignedTooLarge)
		return cache.ReasonUnsignedTooLarge, nil
	}
	return "", nil
}

// markSignatureSeen is the checker's signed-seen marker writer (spec C3,
// revision 8; ruling PE-5, design review I-2): a probe found a sniff-passing
// signature — InRelease, or Release.gpg — for a suite without a signed
// current snapshot, so the cache has seen the suite signed even when no
// client fetched the signature and no signed adoption commits. It marks
// the suite's base (aptpath.MarkerBase, shared by the /. twin), stamped
// seenAt, before the check admits any request; the caller fails the check
// when it cannot.
//
// AIDEV-NOTE: it writes at every such probe, with no read first: the
// marker keeps the newest stamp, its last sighting, which phase F's
// orphan-marker sweep reads; the check's pacing bounds the writes. Without
// this writer a signature only the checker ever saw (an unsigned suite's
// clients never fetch one) could be stripped later and the suite adopted
// unsigned.
func (a *Adopter) markSignatureSeen(ctx context.Context, suite SuiteRef, seenAt int64) error {
	return a.cache.MarkSignedSeenBatch(ctx, []cache.SignedSeenMark{{CanonicalScheme: suite.CanonicalScheme,
		CanonicalHost: suite.CanonicalHost, Base: aptpath.MarkerBase(suite.SuitePath), SeenAt: seenAt}})
}

// errRemarkPoolRead marks a re-mark that failed reading a pool file: an I/O
// fault, logged at ERROR (any other re-mark failure is a WARN).
var errRemarkPoolRead = errors.New("read the pool file")

// remarkSchemes are the schemes the re-mark reads cached anchors under:
// both, as never-downgrade counts a marker under either (design review
// M-7; final fix wave FW-1 (b)).
var remarkSchemes = []string{"http", "https"}

// remarkSignedAnchors re-runs, for an unsigned commit, the cached
// signed-anchor mark of the suite (ruling R-E1, phase D's backstop rule):
// it reads the InRelease and Release.gpg url_path rows of the suite's base
// and of its /. twin (aptpath.MarkerBase(s) and MarkerBase(s)+"/.", which
// share the marker) under both schemes, sniffs their pool files here —
// never on the writer — and marks the base, under the row's own scheme,
// for each one that passes, stamped with the row's last use. The mark is
// enqueued and committed before the commit is enqueued, so
// the commit's in-transaction re-read (FIFO writer) sees it. A pool file
// that does not exist is no evidence (as in the startup marker pass); any
// other read error (errRemarkPoolRead for a pool file), and a failed mark,
// is an error: the caller refuses the commit.
//
// AIDEV-NOTE: this closes phase D's backstop residual where it matters — a
// tracked suite whose sniff-passing signed anchor stayed unmarked after a
// failed backstop, including one cached under the /. twin's path (design
// review I-3) or under the other scheme (FW-1 (b): the commit's authority
// is cross-scheme, M-7, so the evidence it surfaces must be too — an https
// anchor cached while its suite was untracked, or whose backstop failed,
// otherwise let the same base commit unsigned over plain http, the scheme
// a man in the middle can strip). Cost: four more indexed reads per
// unsigned commit. Residual (spec C3, C5): a signed anchor cached after
// the re-mark ran — the other scheme's suite refused at the cap (no row)
// and served signed during this adoption's barrier — earns no marker
// before the commit, since markers are written only for suites with rows
// (the next unsigned commit's re-mark finds it). Fail closed: never commit
// unsigned while signed evidence could not be ruled out, so a refusal from
// here can mean "could not rule out" (an I/O or database fault), not only
// "signed".
func (a *Adopter) remarkSignedAnchors(ctx context.Context, suite SuiteRef) error {
	base := aptpath.MarkerBase(suite.SuitePath)
	var marks []cache.SignedSeenMark
	for _, scheme := range remarkSchemes {
		for _, dir := range []string{base, base + "/."} {
			for _, name := range []string{"InRelease", "Release.gpg"} {
				p := dir + "/" + name
				row, err := a.cache.LookupURL(ctx, scheme, suite.CanonicalHost, p)
				switch {
				case errors.Is(err, cache.ErrNotFound):
					continue
				case err != nil:
					return fmt.Errorf("read %s %s: %w", scheme, p, err)
				case row.BlobHash == nil:
					continue
				}
				signed, err := sniffCachedAnchor(a.cache, p, *row.BlobHash)
				switch {
				case errors.Is(err, fs.ErrNotExist):
					continue
				case err != nil:
					return fmt.Errorf("%w of %s %s: %w", errRemarkPoolRead, scheme, p, err)
				case !signed:
					continue
				}
				seen := row.LastUse()
				if seen <= 0 {
					seen = wallclock.Unix()
				}
				marks = append(marks, cache.SignedSeenMark{CanonicalScheme: scheme,
					CanonicalHost: suite.CanonicalHost, Base: base, SeenAt: seen})
			}
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
