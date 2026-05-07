package handler

import (
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
)

// Request-path metrics declared per SPEC5 §10.4.1. Registered on
// metrics.Default at package init so the admin /metrics scraper sees
// them. The labeled metrics carry metrics.DefaultMaxSeries as the
// per-metric series cap; the unlabeled inflight gauge is uncapped (it
// only ever has one series).
//
// AIDEV-NOTE: emission is centralized in logRequest so that every
// outcome string passed to logRequest also drives a metric, and the
// ~15 logRequest call sites in handler.go don't need individual
// metric calls. The unvouched_deb_passthrough_no_coverage outcome is
// a separate rate-limited Info log line (not via logRequest); its
// counter is emitted explicitly at logUnvouchedPassthrough.
var (
	requestsTotal = metrics.NewCounterWithCap(
		"acu_requests_total",
		"Total HTTP requests served, labeled by outcome and upstream host (SPEC5 §10.4.1).",
		metrics.DefaultMaxSeries,
		"outcome", "host",
	)

	// requestDurationBuckets are SPEC5 §10.4.1 — 1ms..60s wide enough
	// to cover both fast hits and slow miss-path fetches.
	requestDurationBuckets = []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30, 60}

	requestDurationSeconds = metrics.NewHistogramWithCap(
		"acu_request_duration_seconds",
		"Wall-clock request duration in seconds (SPEC5 §10.4.1).",
		requestDurationBuckets,
		metrics.DefaultMaxSeries,
		"outcome", "host",
	)

	// responseBytesBuckets are SPEC5 §10.4.1 — 1 KiB..1 GiB.
	responseBytesBuckets = []float64{1024, 4096, 65536, 262144, 1048576, 10485760, 104857600, 1073741824}

	responseBytes = metrics.NewHistogramWithCap(
		"acu_response_bytes",
		"Bytes written to the client per response (SPEC5 §10.4.1).",
		responseBytesBuckets,
		metrics.DefaultMaxSeries,
		"outcome", "host",
	)

	// inflightRequests tracks ServeHTTP invocations currently executing.
	// Mirrors handler.activeWG — Inc/Dec wrap the WaitGroup Add/Done so
	// the gauge reflects the same set of in-flight invocations the
	// SPEC §9.5 drain waits on.
	inflightRequests = metrics.NewGaugeWithCap(
		"acu_inflight_requests",
		"Number of HTTP requests currently being served (SPEC5 §10.4.1).",
		0,
	)
)
