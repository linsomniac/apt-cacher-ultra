package freshness

// The spec C8 flip barrier, the adopter's side: the commit waits behind the
// barrier (flip), the shutdown signal and the context the barrier waits
// under (stopErr, barrierCtx), the barrier event's outcome, and the first
// adoption that skips it (servedAnchorUnchanged). Split out of adoption.go
// unchanged (deferred minor C-19).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"
)

// FlipGate returns the spec C8 flip barrier the adopter commits behind:
// nil when none is wired, or for a nil adopter (adoption disabled). The
// handler registers anchor serves with the same gate.
func (a *Adopter) FlipGate() *flipgate.Gate {
	if a == nil {
		return nil
	}
	return a.flipGate
}

// stopErr reports a shutdown before the commit: the lifetime context
// ended or, for Run/RunDetached callers, ctx was cancelled. ctx's deadline
// (adoption.max_duration) is not a shutdown.
//
// AIDEV-NOTE: read the lifetime context itself, never only ctx: once the
// deadline fired, ctx.Err() stays DeadlineExceeded and a later shutdown
// would go unseen (deferred minor B-65).
func (p *adoptionPayload) stopErr(ctx context.Context) error {
	if p.stop != nil {
		return p.stop.Err()
	}
	if err := ctx.Err(); errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// barrierCtx is what the flip barrier waits under: the shutdown signal,
// never the adoption deadline (ruling R-C2). Run/RunDetached callers have
// no shutdown signal: their barrier runs to its own bounds.
func (p *adoptionPayload) barrierCtx(ctx context.Context) context.Context {
	if p.stop != nil {
		return p.stop
	}
	return context.WithoutCancel(ctx)
}

// flip runs commit behind the spec C8 flip barrier and logs
// adoption_flip_barrier (design note 10). Without an enabled gate it
// commits at once (the pre-C8 behaviour). A first adoption whose anchor is
// the url_path anchor clients have been served skips the barrier: the
// switch changes nothing they hold. It still honours a closed gate.
func (a *Adopter) flip(ctx context.Context, suite SuiteRef, p *adoptionPayload, first bool, snapshotID int64, commit func() error) error {
	if !a.flipGate.Enabled() {
		return commit()
	}
	attrs := []any{"canonical_host", suite.CanonicalHost, "suite_path", suite.SuitePath, "snapshot_id", snapshotID}
	// first is defensive: Step 3c keeps the url_path anchor at the previous
	// snapshot's bytes, so a later adoption never matches it today; it keeps
	// the skip correct should a later phase write url_path anchors for
	// adopted suites.
	if first && a.servedAnchorUnchanged(ctx, suite, p) {
		// A closed gate (the shutdown signal, design note 18) abandons
		// this commit as Flip would: no commit starts late in the stop
		// window, and spawnAdoption never records ErrAborted.
		if a.flipGate.Closed() {
			return fmt.Errorf("%w: %w", flipgate.ErrAborted, flipgate.ErrClosed)
		}
		err := commit()
		a.logger.Info("adoption_flip_barrier", append(attrs, "outcome", barrierOutcome("not_needed", err))...)
		return err
	}
	res, err := a.flipGate.Flip(p.barrierCtx(ctx), flipgate.Key(suite.CanonicalScheme, suite.CanonicalHost, suite.SuitePath), commit)
	if res.Outcome != flipgate.OutcomeAborted {
		a.logger.Info("adoption_flip_barrier", append(attrs,
			"outcome", barrierOutcome(string(res.Outcome), err),
			"phase_a_seconds", res.PhaseA.Seconds(),
			"token_wait_seconds", res.TokenWait.Seconds(),
			"hold_seconds", res.HoldEnd.Sub(res.HoldStart).Seconds(),
			"held_requests", res.Waiters)...)
	}
	return err
}

// barrierOutcome is adoption_flip_barrier's outcome: the barrier's own;
// refused when the commit refused the candidate (ruling R-E2: an unsigned
// downgrade found inside the commit transaction is a state, not a failed
// commit); or commit_failed when the commit ran and failed. Either way the
// snapshot did not switch, and the timing fields still describe the
// barrier.
func barrierOutcome(outcome string, commitErr error) string {
	switch {
	case errors.Is(commitErr, cache.ErrUnsignedDowngradeRefused):
		return "refused"
	case commitErr != nil:
		return "commit_failed"
	}
	return outcome
}

// servedAnchorUnchanged reports whether the anchor this adoption commits
// is byte-for-byte the url_path anchor clients have been served: the
// primary anchor of its form (InRelease inline; Release for the detached
// and unsigned forms — index consistency depends on the Release text, not
// on its signature). A read error answers false, so the barrier runs (the
// safe direction); one other than "no row" is logged (deferred minor
// C-16): at WARN, or at DEBUG when a shutdown cut the read short, as the
// check's local steps are (deferred minor E-13, FW-3 (d)). The read runs
// under the shutdown signal, like the barrier's own waits (ruling R-C2),
// never unbounded.
func (a *Adopter) servedAnchorUnchanged(ctx context.Context, suite SuiteRef, p *adoptionPayload) bool {
	path, want := suite.SuitePath+inReleaseFilename, p.inlineHash
	if p.form == adoptionFormDetached || p.form == adoptionFormUnsigned {
		path, want = suite.SuitePath+releaseFilename, p.releaseHash
	}
	rctx, cancel := context.WithTimeout(p.barrierCtx(ctx), commitTimeout)
	defer cancel()
	row, err := a.cache.LookupURL(rctx, suite.CanonicalScheme, suite.CanonicalHost, path)
	if err != nil && !errors.Is(err, cache.ErrNotFound) {
		level := slog.LevelWarn
		if p.stopErr(ctx) != nil || errors.Is(err, context.Canceled) {
			level = slog.LevelDebug
		}
		a.logger.Log(ctx, level, "adoption_served_anchor_read_failed",
			"canonical_host", suite.CanonicalHost, "suite_path", suite.SuitePath, "path", path, "err", err)
	}
	return err == nil && row.BlobHash != nil && *row.BlobHash == want
}
