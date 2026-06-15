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

	// SPEC6_5 §10.3: serve-time hash validation counter, the operational
	// counterpart to the serve_hash_mismatch Warn. Operators alarm on
	// outcome="mismatch" rate. path_class is the §6.1 closed enum (8
	// values); outcome is "match" or "mismatch" (2 values). Cardinality
	// is well within metric_series_cap (1024).
	serveHashValidatedTotal = metrics.NewCounterWithCap(
		"acu_serve_hash_validated_total",
		"Serve-time hash validations against package_hash, by path class and outcome (SPEC6_5 §10.3).",
		metrics.DefaultMaxSeries,
		"path_class", "outcome",
	)

	// SPEC6_8: authoritative "not in snapshot" 404s served for an apt
	// IndexTarget (per-arch Packages* / per-component Sources*). This is
	// the cause-agnostic SYMPTOM signal — a client's `apt update` is broken
	// against the current snapshot, whatever dropped the index (arch-filter
	// bug, 4xx_index_target skip, GC of an anchor, …). Steady state ZERO;
	// alert on ANY increase. Labeled by architecture ("all", "amd64",
	// "source", …) — a closed, low-cardinality enum.
	serveSnapshotIndexTarget404Total = metrics.NewCounterWithCap(
		"acu_serve_snapshot_index_target_404_total",
		"Authoritative not-in-snapshot 404s served for an apt IndexTarget (broken apt update), by architecture (SPEC6_8).",
		metrics.DefaultMaxSeries,
		"architecture",
	)
)
