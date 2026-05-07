package admin

import (
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
)

// selfMetrics holds the SPEC5 §10.4.8 admin-listener self-metrics.
// These are registered on the configured registry (cfg.Registry) so
// admin tests using an isolated metrics.NewRegistry() see fresh
// counters per test, mirroring the pattern in gauges.go.
//
// AIDEV-NOTE: emission happens at the four handler entry points
// (handleMetrics, handleStatus, handleHealthz, htpasswd middleware).
// The htpasswd middleware path is the only one that has to fight
// timing-parity: SPEC5 §10.4.8 splits auth failures into
// unknown_user vs wrong_password, but the response is identical for
// both — the metric is the only operator-visible distinction. The
// auth.go layer plumbs that distinction up via authenticate's second
// return without affecting wall-clock parity.
type selfMetrics struct {
	scrapeTotal           *metrics.Counter
	scrapeDurationSeconds *metrics.Histogram
	statusTotal           *metrics.Counter
	statusDurationSeconds *metrics.Histogram
	healthzTotal          *metrics.Counter
	authFailuresTotal     *metrics.Counter
}

// newSelfMetrics declares and registers the §10.4.8 self-metrics on
// the given registry. capLimit applies to the labeled metrics; the
// unlabeled scrape_total / scrape_duration histograms use cap=0
// (single-series anyway).
func newSelfMetrics(r *metrics.Registry, capLimit int) *selfMetrics {
	scrapeBuckets := []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1}
	return &selfMetrics{
		scrapeTotal: metrics.NewCounterWithCapIn(r,
			"acu_admin_scrape_total",
			"Total /metrics scrapes served (SPEC5 §10.4.8).",
			0),
		scrapeDurationSeconds: metrics.NewHistogramWithCapIn(r,
			"acu_admin_scrape_duration_seconds",
			"/metrics scrape duration in seconds (SPEC5 §10.4.8).",
			scrapeBuckets,
			0),
		statusTotal: metrics.NewCounterWithCapIn(r,
			"acu_admin_status_total",
			"Total / status-page renders served (SPEC5 §10.4.8).",
			0),
		statusDurationSeconds: metrics.NewHistogramWithCapIn(r,
			"acu_admin_status_duration_seconds",
			"/ status-page render duration in seconds, by format (SPEC5 §10.4.8).",
			scrapeBuckets,
			capLimit, "format"),
		healthzTotal: metrics.NewCounterWithCapIn(r,
			"acu_admin_healthz_total",
			"Total /healthz probes served, labeled by status (`ok` or `degraded`) (SPEC5 §10.4.8).",
			capLimit, "status"),
		authFailuresTotal: metrics.NewCounterWithCapIn(r,
			"acu_admin_auth_failures_total",
			"Admin-listener auth failures by reason (`no_credentials`, `unknown_user`, `wrong_password`) (SPEC5 §10.4.8).",
			capLimit, "reason"),
	}
}
