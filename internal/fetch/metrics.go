package fetch

import (
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
)

// Fetch-path metrics declared per SPEC5 §10.4.2. Registered on
// metrics.Default at package init so the admin /metrics scraper sees
// them. Counter/histogram label keys must stay aligned with the
// outcome strings ClassifyFetchOutcome / ClassifyConditionalOutcome
// produce — adding a new sentinel error means classifying it AND
// updating SPEC5 §10.4.2's precedence table (per the AIDEV-NOTE in
// classify.go).
//
// AIDEV-NOTE: emission is wired via deferred closures inside Fetch
// and Conditional so every terminal return — including the early
// validation errors before any network I/O — drives exactly one
// counter increment plus one duration observation. The classifiers
// are total functions, so the counter sums across all outcome values
// equal the number of Fetch/Conditional calls completed.
var (
	// fetchDurationBuckets are SPEC5 §10.4.2 — 10ms..300s wide enough
	// to cover both fast metadata fetches and slow large-blob downloads.
	fetchDurationBuckets = []float64{0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30, 60, 300}

	fetchTotal = metrics.NewCounterWithCap(
		"acu_fetch_total",
		"Total upstream fetch attempts, labeled by outcome and host (SPEC5 §10.4.2).",
		metrics.DefaultMaxSeries,
		"outcome", "host",
	)

	fetchDurationSeconds = metrics.NewHistogramWithCap(
		"acu_fetch_duration_seconds",
		"Wall-clock fetch duration in seconds (SPEC5 §10.4.2).",
		fetchDurationBuckets,
		metrics.DefaultMaxSeries,
		"outcome", "host",
	)

	fetchRetriesTotal = metrics.NewCounterWithCap(
		"acu_fetch_retries_total",
		"Per-retry counter; one increment per `fetch retry` Info emit (SPEC5 §10.4.2).",
		metrics.DefaultMaxSeries,
		"host",
	)
)

// hostLabel returns the canonical-host string fit for a metric label.
// nil-target returns "" — the classifier surfaces those as
// invalid_url / nil-target catch-alls regardless.
func hostLabel(t *Target) string {
	if t == nil {
		return ""
	}
	return t.CanonicalHost
}
