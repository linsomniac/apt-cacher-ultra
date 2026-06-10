package freshness

import (
	"errors"

	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
)

// Freshness / adoption metrics declared per SPEC5 §10.4.3. Registered
// on metrics.Default at package init so the admin /metrics scraper
// sees them. Counter/histogram label sets must align with SPEC5
// §10.4.3 — adding a new outcome string here means updating the spec
// inventory.
//
// AIDEV-NOTE: classifyAdoptionOutcome is a TOTAL function over
// non-nil errors returned by Adopter.Run / RunDetached. The sum of
// acu_adoption_total across all outcome values equals the number of
// adoption attempts, so a missing classification would silently
// under-count a category. Adding a new ErrAdoption* sentinel means
// classifying it AND extending the SPEC5 §10.4.3 outcome enum.
var (
	freshnessCheckTotal = metrics.NewCounterWithCap(
		"acu_freshness_check_total",
		"Total freshness checks completed, labeled by result and host (SPEC5 §10.4.3).",
		metrics.DefaultMaxSeries,
		"result", "host",
	)

	// freshnessAnchorReconstructedTotal counts SPEC2 §7.4 recoveries:
	// an adopted suite whose InRelease url_path anchor was reaped
	// (SPEC4 §5 GC TTL) and which the checker reconstructed in-place
	// instead of freezing. A non-zero value means anchors are being
	// reaped out from under live suites — the GC identity guard
	// (SPEC4 §5 guard (d)) should drive this back to zero over time.
	freshnessAnchorReconstructedTotal = metrics.NewCounterWithCap(
		"acu_freshness_anchor_reconstructed_total",
		"Metadata anchor url_path rows reconstructed during freshness recovery, by host (SPEC2 §7.4).",
		metrics.DefaultMaxSeries,
		"host",
	)

	// freshnessStaleSuites is the per-tick gauge of suites whose
	// last_success_at has aged past staleSuiteFactor*refresh (SPEC §7.4
	// observability). Labeled adopted=true|false so a frozen ADOPTED
	// suite — the case that silently served stale metadata for a week in
	// the field — is directly alertable.
	freshnessStaleSuites = metrics.NewGaugeWithCap(
		"acu_freshness_stale_suites",
		"Suites whose last_success_at is older than staleSuiteFactor*refresh, by adopted state (SPEC §7.4).",
		metrics.DefaultMaxSeries,
		"adopted",
	)

	adoptionTotal = metrics.NewCounterWithCap(
		"acu_adoption_total",
		"Total adoption attempts, labeled by outcome and host (SPEC5 §10.4.3).",
		metrics.DefaultMaxSeries,
		"outcome", "host",
	)

	// adoptionDurationBuckets are SPEC5 §10.4.3 — 1s..1h covers fast
	// inline adoptions and slow large-archive flips with hot prefetch.
	adoptionDurationBuckets = []float64{1, 5, 10, 30, 60, 300, 600, 1800, 3600}

	adoptionDurationSeconds = metrics.NewHistogramWithCap(
		"acu_adoption_duration_seconds",
		"Wall-clock adoption duration in seconds, by outcome and host (SPEC5 §10.4.3).",
		adoptionDurationBuckets,
		metrics.DefaultMaxSeries,
		"outcome", "host",
	)

	adoptionFormDriftTotal = metrics.NewCounterWithCap(
		"acu_adoption_form_drift_total",
		"Adoptions whose signature form changed from the prior current snapshot (SPEC5 §10.4.3).",
		metrics.DefaultMaxSeries,
		"prior_form", "new_form", "host",
	)

	hotPrefetchTotal = metrics.NewCounterWithCap(
		"acu_hot_prefetch_total",
		"Hot-prefetch outcomes during adoption, labeled by outcome and host (SPEC5 §10.4.3).",
		metrics.DefaultMaxSeries,
		"outcome", "host",
	)

	adoptionHeartbeatFailuresTotal = metrics.NewCounterWithCap(
		"acu_adoption_heartbeat_failures_total",
		"Per-host count of adoption heartbeat-write failures (SPEC5 §10.4.3 / SPEC4 §10.2).",
		metrics.DefaultMaxSeries,
		"host",
	)

	// SPEC6_5 §10.3 / §7.4: adoption-member-skip / Sources-parse /
	// pdiff-Index-parse counters. Cardinality is a closed enum per
	// label (≤ 4 values for reason, ≤ 2 for outcome) so the totals
	// stay well under metric_series_cap.
	adoptionMembersSkippedTotal = metrics.NewCounterWithCap(
		"acu_adoption_members_skipped_total",
		"Release members skipped during adoption, by reason (SPEC6_5 §10.3).",
		metrics.DefaultMaxSeries,
		"reason",
	)

	// adoptionMemberRetriesTotal counts SPEC6_7 §1 in-adoption member
	// retries by outcome: "success" = a retry attempt recovered the
	// member (the stale-mirror window closed in time), "exhausted" =
	// every retry failed and the member proceeded down the pre-retry
	// path (tolerant skip or fatal). Closed 2-value enum.
	adoptionMemberRetriesTotal = metrics.NewCounterWithCap(
		"acu_adoption_member_retries_total",
		"In-adoption member fetch retries, by outcome (SPEC6_7 §10).",
		metrics.DefaultMaxSeries,
		"outcome",
	)

	adoptionSourcesParsedTotal = metrics.NewCounterWithCap(
		"acu_adoption_sources_parsed_total",
		"Sources index files processed during adoption, by outcome (SPEC6_5 §10.3).",
		metrics.DefaultMaxSeries,
		"outcome",
	)

	adoptionPdiffIndexesParsedTotal = metrics.NewCounterWithCap(
		"acu_adoption_pdiff_indexes_parsed_total",
		"pdiff Index files processed during adoption, by outcome (SPEC6_5 §10.3).",
		metrics.DefaultMaxSeries,
		"outcome",
	)

	// AIDEV-NOTE: SPEC6_5 §10.3 also lists
	// acu_package_hash_rows_by_kind{kind} (gauge) and
	// acu_serve_hash_validated_total{path_class,outcome} (counter).
	// The gauge is wired by the §2.4 status-surface refresher (a
	// separate task). The validated counter requires the validated_hash
	// log field to flow from the handler validation sites (deferred
	// per §2.3 follow-up).
)

// classifyAdoptionOutcome maps the err return from Adopter.Run /
// RunDetached to one of the SPEC5 §10.4.3 outcome labels. nil → success.
//
// Specific sentinels first; everything else collapses into run_failed
// so the sum across outcomes equals the number of completed adoptions.
// Adding a new ErrAdoption* sentinel that should surface as a distinct
// outcome must extend this switch AND the SPEC5 §10.4.3 enum.
func classifyAdoptionOutcome(err error) string {
	if err == nil {
		return "success"
	}
	switch {
	// unpinned_suite is checked BEFORE gpg_failed: the verifier wraps
	// its return with both ErrAdoptionGPGFailed (the runShared
	// category sentinel) and ErrAdoptionUnpinnedSuite (the specific
	// SPEC5 §10.4.3 unpinned-pin reason). Without this ordering all
	// unpinned errors collapse into gpg_failed and the rollout signal
	// for integrity.require_pinned_signer is invisible to operators.
	case errors.Is(err, ErrAdoptionUnpinnedSuite):
		return "unpinned_suite"
	case errors.Is(err, ErrAdoptionGPGFailed):
		return "gpg_failed"
	case errors.Is(err, ErrAdoptionParseFailed):
		return "parse_failed"
	case errors.Is(err, ErrAdoptionMemberMismatch):
		return "member_mismatch"
	case errors.Is(err, ErrAdoptionMemberFetchFailed):
		// SPEC5 §10.4.3: broken out of the run_failed catch-all so the
		// dominant real-world failure (a declared member the upstream
		// won't serve intact — size/content-length mismatch, transport
		// error, or 5xx) is operator-distinguishable. The reason chip
		// carries the specific member via AdoptionMemberError.
		return "member_fetch_failed"
	case errors.Is(err, ErrAdoptionDBFailed):
		// SPEC5 §10.4.3: a local cache/DB fault (blob write, rehash,
		// snapshot insert) is operationally distinct from an upstream
		// member problem — surface it separately, not as run_failed.
		return "db_failed"
	}
	return "run_failed"
}

// classifyAdoptionReason produces the SPEC5 §10.5
// `recent_adoptions[].reason` short tag for one completed adoption.
// Returns "" on success.
//
// The outcome bucket determines the dominant tag for every category
// except gpg_failed; that bucket is broken out into the specific
// verifier sentinel via the injected gpgClassifier (production wires
// gpg.ClassifyVerifyErr). When gpgClassifier is nil or returns "",
// the reason falls back to the bucket label so callers always get a
// usable string for non-success rows.
//
// AIDEV-NOTE: keep the gpg-bucket reasons in lockstep with
// gpg.ClassifyVerifyErr. Adding a new gpg sentinel without extending
// that function would surface as crypto_verify_failed on the UI even
// when the underlying error is more specific.
func classifyAdoptionReason(err error, outcome string, gpgClassifier func(error) string) string {
	if err == nil {
		return ""
	}
	if outcome == "gpg_failed" && gpgClassifier != nil {
		if r := gpgClassifier(err); r != "" {
			return r
		}
	}
	return outcome
}
