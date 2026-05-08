// SPEC6 §10.3 acu_mitm_* metric family.
//
// Per the §15 disabled-mode parity contract, these metrics are
// REGISTERED unconditionally at package init so /metrics scrapes
// produce a stable shape across enabled/disabled boots — only
// observation is gated on MITM being enabled. With
// `tls_mitm.enabled = false` no CONNECT pipeline is wired, so the
// observation sites in connect.go / mitm_metrics.go never fire and
// counters/histograms remain at zero. Gauges that main.go writes
// (cert_cache_size, cert_cache_capacity, ca_not_after_unixtime)
// stay at zero unless main.go explicitly sets them when MITM is
// enabled.
//
// Outcome enum (`acu_mitm_connect_total{outcome}`) matches the
// `mitm_connect.outcome` log field — the same string is emitted to
// both. Cardinality is bounded by ConnectOutcome's closed enum.

package proxy

import (
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
)

// connectDurationBuckets covers the full CONNECT lifecycle including
// the inner GET, from sub-second hits to multi-second misses on a
// remote upstream. 1ms..60s like acu_request_duration_seconds.
var connectDurationBuckets = []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30, 60}

// handshakeDurationBuckets covers TLS handshake only — typically
// <100ms on a fast client, occasionally seconds when the leaf cert
// is freshly generated under singleflight contention. 1ms..30s.
var handshakeDurationBuckets = []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30}

var (
	// acu_mitm_connect_total counts every CONNECT outcome the
	// handler emits. Outcome enum matches mitm_connect.outcome
	// (bad_target, bad_host, denied_host, ..., tunneled).
	mitmConnectTotal = metrics.NewCounterWithCap(
		"acu_mitm_connect_total",
		"Total CONNECTs the MITM handler observed, by outcome (SPEC6 §10.3).",
		metrics.DefaultMaxSeries,
		"outcome",
	)

	// acu_mitm_connect_duration_seconds — full CONNECT lifecycle,
	// including the inner GET. Unlabeled.
	mitmConnectDurationSeconds = metrics.NewHistogramWithCap(
		"acu_mitm_connect_duration_seconds",
		"CONNECT lifecycle duration in seconds, including inner GET (SPEC6 §10.3).",
		connectDurationBuckets,
		metrics.DefaultMaxSeries,
	)

	// acu_mitm_cert_cache_size — current entries in the leaf cert
	// cache. Updated by the gauge refresher in main.go.
	mitmCertCacheSize = metrics.NewGaugeWithCap(
		"acu_mitm_cert_cache_size",
		"Current entries in the MITM leaf cert cache (SPEC6 §10.3).",
		0,
	)

	// acu_mitm_cert_cache_capacity — configured cert_cache_size.
	// Set once at startup by main.go.
	mitmCertCacheCapacity = metrics.NewGaugeWithCap(
		"acu_mitm_cert_cache_capacity",
		"Configured MITM leaf cert cache capacity (SPEC6 §10.3).",
		0,
	)

	// acu_mitm_cert_cache_lookups_total{outcome=hit|miss} — the
	// hit-rate signal. Outcome enum is closed: "hit" or "miss".
	mitmCertCacheLookupsTotal = metrics.NewCounterWithCap(
		"acu_mitm_cert_cache_lookups_total",
		"MITM leaf cert cache lookups, by outcome (SPEC6 §10.3).",
		metrics.DefaultMaxSeries,
		"outcome",
	)

	// acu_mitm_cert_issued_total{algorithm} — leaf certs issued
	// lifetime. Algorithm enum matches LeafAlgorithm.String()
	// ("ecdsa-p256", "rsa2048").
	mitmCertIssuedTotal = metrics.NewCounterWithCap(
		"acu_mitm_cert_issued_total",
		"MITM leaf certs issued lifetime, by algorithm (SPEC6 §10.3).",
		metrics.DefaultMaxSeries,
		"algorithm",
	)

	// acu_mitm_cert_evicted_total{reason} — evictions lifetime.
	// Reason enum matches EvictReason ("lru", "expired").
	mitmCertEvictedTotal = metrics.NewCounterWithCap(
		"acu_mitm_cert_evicted_total",
		"MITM leaf cert cache evictions lifetime, by reason (SPEC6 §10.3).",
		metrics.DefaultMaxSeries,
		"reason",
	)

	// acu_mitm_ca_not_after_unixtime — CA expiry as a gauge.
	// Drives the operator's Prometheus alert when < now + 30d.
	mitmCANotAfterUnixtime = metrics.NewGaugeWithCap(
		"acu_mitm_ca_not_after_unixtime",
		"MITM CA cert NotAfter as Unix timestamp (SPEC6 §10.3).",
		0,
	)

	// acu_mitm_handshake_duration_seconds — TLS handshake only,
	// excluding inner GET. Unlabeled.
	mitmHandshakeDurationSeconds = metrics.NewHistogramWithCap(
		"acu_mitm_handshake_duration_seconds",
		"TLS handshake duration in seconds, excluding inner GET (SPEC6 §10.3).",
		handshakeDurationBuckets,
		metrics.DefaultMaxSeries,
	)
)

// RecordCertCacheLookup is the LookupHook target — bumps the
// hit-or-miss counter once per Get AND feeds the §10.4 60s rolling
// hit-rate counter the status page reads. Exported so main.go
// (different package) can install it as the cache callback.
func RecordCertCacheLookup(hit bool) {
	if hit {
		mitmCertCacheLookupsTotal.Inc("hit")
	} else {
		mitmCertCacheLookupsTotal.Inc("miss")
	}
	certHitRateMu.RLock()
	r := certHitRate
	certHitRateMu.RUnlock()
	r.Note(hit)
}

// RecordCertIssued bumps acu_mitm_cert_issued_total{algorithm}
// after each successful leaf-cert generation. main.go wraps the
// GenFunc to call this.
func RecordCertIssued(algorithm string) {
	mitmCertIssuedTotal.Inc(algorithm)
}

// RecordCertEvicted is the EvictHook target. Reason values are the
// tlsmitm.EvictReason enum strings ("lru", "expired").
func RecordCertEvicted(reason string) {
	mitmCertEvictedTotal.Inc(reason)
}

// SetCertCacheSize / SetCertCacheCapacity / SetCANotAfterUnixtime
// let main.go drive the §10.3 gauges from outside this package.
// main.go is the place that knows when MITM is enabled.
func SetCertCacheSize(n int)        { mitmCertCacheSize.Set(float64(n)) }
func SetCertCacheCapacity(n int)    { mitmCertCacheCapacity.Set(float64(n)) }
func SetCANotAfterUnixtime(t int64) { mitmCANotAfterUnixtime.Set(float64(t)) }
