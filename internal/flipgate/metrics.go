package flipgate

import "github.com/linsomniac/apt-cacher-ultra/internal/metrics"

// Spec C8 metrics, unlabelled (spec C11: no suite labels): a counter per
// documented residual, the expiry timer's recovered panics, and a gauge of
// the flips in the barrier now. Listed in the flat-repository inventory
// note in internal/freshness/metrics.go.
var (
	barrierActive = metrics.NewGauge(
		"acu_flip_barrier_active",
		"Adoption flips in the flip barrier now: waiting for quiet or for the hold token, holding, or committing (spec C8).",
	)
	holdSkippedTotal = metrics.NewCounter(
		"acu_flip_hold_skipped_total",
		"Flips that committed without a hold because the hold token was not free within 60 s (spec C8).",
	)
	holdExpiredTotal = metrics.NewCounter(
		"acu_flip_hold_expired_total",
		"Flip holds that reached their 20 s bound before the commit finished (spec C8).",
	)
	quietCutShortTotal = metrics.NewCounter(
		"acu_flip_quiet_cut_short_total",
		"Flip holds that committed 15 s into the hold, before the suite was quiet: an InRelease/Release response still being sent, or finished within the quiet period, may straddle (spec C8).",
	)
	waitersOverCapTotal = metrics.NewCounter(
		"acu_flip_waiters_over_cap_total",
		"Anchor requests served at once during a flip hold because 256 requests already waited in it (spec C8).",
	)
	slowAnchorReadsTotal = metrics.NewCounter(
		"acu_flip_slow_anchor_reads_total",
		"InRelease/Release responses that took 30 s or longer to send; the flip barrier does not wait for them (spec C8).",
	)
	gatePanicsTotal = metrics.NewCounter(
		"acu_flip_gate_panics_total",
		"Panics recovered in the flip barrier's hold-expiry timer: a bug, the barrier stays usable (spec C8).",
	)
)
