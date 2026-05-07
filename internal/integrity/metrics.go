package integrity

import (
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
)

// Integrity metrics declared per SPEC5 §10.4.4. All registered on
// metrics.Default at package init so the admin /metrics scraper sees
// them. The scanner counter and corruption counter are unlabeled
// (process-level), and the hash-validation-failure counter carries a
// `phase` label whose values map to where the failure was detected
// (`fetch` from internal/handler, `at_rest` from this package).
//
// AIDEV-NOTE: PoolCorruptionDuringAdoptionTotal and
// HashValidationFailureFetch are exported so other packages
// (internal/freshness, internal/handler) can increment them at their
// own emit sites — SPEC5 §10.4.4 lists them under integrity, but
// the actual log emit lives in the package that detected the
// corruption.
var (
	atRestScansTotal = metrics.NewCounterWithCap(
		"acu_at_rest_scans_total",
		"Total at-rest integrity scans completed (one per ScanOnce, regardless of corruption count) (SPEC5 §10.4.4).",
		0,
	)

	atRestCorruptionTotal = metrics.NewCounterWithCap(
		"acu_at_rest_corruption_total",
		"Total at-rest corruption events detected (one per at_rest_corruption log emit) (SPEC5 §10.4.4).",
		0,
	)

	hashValidationFailureTotal = metrics.NewCounterWithCap(
		"acu_hash_validation_failure_total",
		"Hash validation failures by detection phase (`fetch` or `at_rest`) (SPEC5 §10.4.4).",
		metrics.DefaultMaxSeries,
		"phase",
	)

	poolCorruptionDuringAdoptionTotal = metrics.NewCounterWithCap(
		"acu_pool_corruption_during_adoption_total",
		"Pool blobs found corrupt while validating reuse during adoption (SPEC5 §10.4.4 / SPEC2 §10).",
		0,
	)
)

// IncHashValidationFailureFetch increments the fetch-phase variant of
// acu_hash_validation_failure_total. Exported so internal/handler can
// emit alongside its own hash_validation_failure Error log without
// importing the metric instance directly.
func IncHashValidationFailureFetch() {
	hashValidationFailureTotal.Inc("fetch")
}

// IncHashValidationFailureAtRest increments the at_rest-phase variant.
// Used at the at_rest_corruption Error log site inside this package.
func IncHashValidationFailureAtRest() {
	hashValidationFailureTotal.Inc("at_rest")
}

// IncPoolCorruptionDuringAdoption increments
// acu_pool_corruption_during_adoption_total. Exported so
// internal/freshness can emit at the pool_corruption_during_adoption
// Warn log site.
func IncPoolCorruptionDuringAdoption() {
	poolCorruptionDuringAdoptionTotal.Inc()
}
