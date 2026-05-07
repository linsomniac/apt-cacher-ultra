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
	case errors.Is(err, ErrAdoptionGPGFailed):
		return "gpg_failed"
	case errors.Is(err, ErrAdoptionParseFailed):
		return "parse_failed"
	case errors.Is(err, ErrAdoptionMemberMismatch):
		return "member_mismatch"
	}
	return "run_failed"
}
