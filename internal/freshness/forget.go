package freshness

// Spec C9 (D9): POST /forget, the reaper's steps for one suite on demand.

import (
	"context"
	"errors"
	"fmt"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// Forget is POST /forget (spec C9, D9; admin.Forgetter): the reaper's
// steps for one suite, run to completion regardless of idleness within
// suiteHoldTimeout — under the suite's mutex every metadata batch, then
// the final compare-and-delete with no idle check (cache.ForgetSuite),
// which also deletes the signed-seen markers of the suite's base, and the
// cached anchor rows of the base and its "/." twin under both schemes,
// when no suite row of its host is left for the base or the twin, under
// either scheme (design note 13, ruling PF-2). Its result's Deleted is
// false, with a nil error, when the suite has no row or is busy (the
// endpoint answers 409 for both: no existence oracle); KeptBy names the
// rows that kept the markers (ruling PF-7). An error is a database
// failure.
//
// AIDEV-NOTE: ruling R-F1 — the one suite-row delete path: the row read
// first (I-4: a lock-map entry is stored only for a suite with a row),
// HoldSuite, the re-read under the hold, deleteSuiteTx through
// cache.ForgetSuite, release(gone) — gone being ForgetSuite's answer,
// settled by a fresh read after a failed submit, or the re-read's finding
// no row (design review M-2). A panic under the hold is logged with the
// suite (freshness_check_panicked, where=forget: final review spec m1),
// then released as the reaper's per-candidate recovery releases one
// (reapSuite): with the cache's fresh answer (settledGone), since it may
// have struck after ForgetSuite's commit — a deleted suite's lock-map
// entry never leaks, a kept one's is never dropped (design note 23) — and
// is then re-panicked, so net/http's recovery still ends the request (the
// Task 6 re-review's out-of-scope 3); its "http: panic serving" line goes
// through the admin server's ErrorLog, slog at ERROR. A final delete that
// committed although its submit failed (ForgetSuite's settled Gone) is
// logged suite_forgotten with settled=true and the metadata rows, its
// marker and anchor counts unknown (final review spec m2, as the reaper's
// suite_reaped settled=true), before the error goes back (the endpoint's
// 500, or 503 at shutdown). The suite's next request re-creates it
// never adopted, so unchanged upstream content re-adopts at once (signed)
// or after the transition's confirmation (unsigned), and a deliberate
// rollback adopts: the escape hatch for refusal states. Forgetting an
// active suite bypasses the flip barrier (spec C8): a client in the
// middle of an update then can see one Hash Sum mismatch (design review
// M-7, a documented residual).
func (r *Reaper) Forget(ctx context.Context, scheme, host, suitePath string) (cache.ForgetResult, error) {
	ctx, cancel := context.WithTimeout(ctx, suiteHoldTimeout)
	defer cancel()
	key := cache.SuiteKey{CanonicalScheme: scheme, CanonicalHost: host, SuitePath: suitePath}
	switch _, err := r.cache.GetSuiteFreshness(ctx, scheme, host, suitePath); {
	case errors.Is(err, cache.ErrNotFound):
		return cache.ForgetResult{}, nil
	case err != nil:
		return cache.ForgetResult{}, fmt.Errorf("forget: read suite row: %w", err)
	}
	release, ok := r.checker.HoldSuite(key)
	if !ok {
		return cache.ForgetResult{}, nil
	}
	gone := false
	defer func() {
		if v := recover(); v != nil {
			unloggedPanic(func() {
				r.checker.reportPanic("freshness_check_panicked", "forget",
					SuiteRef{CanonicalScheme: scheme, CanonicalHost: host, SuitePath: suitePath}, v)
			})
			release(r.settledGone(key))
			panic(v)
		}
		release(gone)
	}()
	if r.reapHook != nil {
		r.reapHook(ctx, key)
	}
	cur, err := r.cache.GetSuiteFreshness(ctx, scheme, host, suitePath)
	switch {
	case errors.Is(err, cache.ErrNotFound):
		gone = true
		return cache.ForgetResult{}, nil
	case err != nil:
		return cache.ForgetResult{}, fmt.Errorf("forget: read suite row: %w", err)
	}
	rows, err := r.cache.DeleteSuiteMetadata(ctx, key)
	if err != nil {
		return cache.ForgetResult{}, fmt.Errorf("forget: %w", err)
	}
	res, err := r.cache.ForgetSuite(ctx, key, cur.CurrentSnapshotID)
	gone = res.Gone
	if err != nil {
		if res.Gone {
			// The delete committed and its submit failed: under the hold
			// no other deleter runs, so the cache's fresh read finding no
			// row means this call deleted it — and, if no row kept them,
			// the base's markers. Leave a record of it.
			r.logger.Info("suite_forgotten",
				"canonical_scheme", scheme,
				"canonical_host", host,
				"suite_path", suitePath,
				"metadata_rows", rows,
				"settled", true,
			)
		}
		return cache.ForgetResult{}, fmt.Errorf("forget: %w", err)
	}
	if !res.Deleted {
		return res, nil
	}
	r.logger.Info("suite_forgotten",
		"canonical_scheme", scheme,
		"canonical_host", host,
		"suite_path", suitePath,
		"metadata_rows", rows,
		"markers_deleted", res.MarkersDeleted,
		"anchor_rows", res.AnchorRowsDeleted,
	)
	return res, nil
}
