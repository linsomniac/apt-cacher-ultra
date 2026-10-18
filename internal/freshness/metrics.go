package freshness

import (
	"context"
	"errors"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
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
// Outcome inventory: success, refused (spec C5 refusal states; the
// reason says which), release_date_in_future, timed_out
// (adoption.max_duration), unpinned_suite, gpg_failed, parse_failed,
// member_mismatch, member_fetch_failed, db_failed, run_failed, and
// panicked (Checker.recoverAdoption).
//
// AIDEV-NOTE: metrics added by the flat-repository work, outside the
// SPEC5 §10.4.3 inventory: acu_clock_steps_detected_total (spec C4) and
// acu_legacy_snapshots_quarantined_total (spec §6.1, startup legacy
// trust audit), both unlabelled counters; acu_adoption_failing_suites
// (spec C7, unlabelled gauge); acu_freshness_waiting_for_slot (spec C4,
// unlabelled gauge). The flip barrier's acu_flip_hold_skipped_total,
// acu_flip_hold_expired_total, acu_flip_waiters_over_cap_total,
// acu_flip_slow_anchor_reads_total and acu_flip_gate_panics_total (spec
// C8, unlabelled counters) are declared in internal/flipgate.
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

	// clockStepsTotal counts past-event stamps found in the future
	// (backward wall-clock steps, spec C4).
	clockStepsTotal = metrics.NewCounter(
		"acu_clock_steps_detected_total",
		"Past-event timestamps found in the future (backward wall-clock step).",
	)

	// legacyQuarantinedTotal counts pre-v7 snapshots the startup trust
	// audit quarantined (spec §6.1); the reason is in the WARN line.
	legacyQuarantinedTotal = metrics.NewCounter(
		"acu_legacy_snapshots_quarantined_total",
		"Pre-v7 snapshots quarantined by the startup legacy trust audit.",
	)

	// adoptionFailingSuites is the number of suites whose latest adoption
	// attempt failed (refusal states excluded, spec C7), refreshed every
	// scheduler tick. 0 when no adopter is wired: records left by an
	// earlier run with adoption enabled are inert.
	adoptionFailingSuites = metrics.NewGauge(
		"acu_adoption_failing_suites",
		"Suites whose latest adoption attempt failed, refusal states excluded; refreshed every scheduler tick (spec C7).",
	)

	// freshnessWaitingForSlot is the number of suites waiting for an
	// adoption slot; the scheduler re-checks each as soon as capacity it
	// can use is released (spec C4).
	freshnessWaitingForSlot = metrics.NewGauge(
		"acu_freshness_waiting_for_slot",
		"Suites waiting for an adoption slot; each is re-checked as soon as one it can use is released (spec C4).",
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

	// adoptionMemberRepairsTotal counts SPEC6_7 §3 freshness-tick
	// repair attempts on integrity-skipped members, by outcome
	// ("success" = member promoted into the snapshot, "failure" =
	// fetch/validation/DB failure; retried next tick). Closed 2-value
	// enum.
	adoptionMemberRepairsTotal = metrics.NewCounterWithCap(
		"acu_adoption_member_repairs_total",
		"Freshness-tick repairs of integrity-skipped members, by outcome (SPEC6_7 §10).",
		metrics.DefaultMaxSeries,
		"outcome",
	)

	// adoptionServeTargetMissingTotal counts SPEC6_8 serve-contract guard
	// blocks: an adoption deferred because a requestable IndexTarget group
	// (an allowlisted arch, or the always-required "all" pseudo-arch) the
	// signed Release declared was absent from the snapshot. Steady state is
	// ZERO — any nonzero value means the prior snapshot is being held back
	// because committing would 404 `apt update`. Labeled by arch; closed
	// low-cardinality enum (allowlist arches ∪ {all, source}). Alert on it.
	adoptionServeTargetMissingTotal = metrics.NewCounterWithCap(
		"acu_adoption_serve_target_missing_total",
		"Adoptions deferred because a requestable IndexTarget group the Release declared was absent from the snapshot, by arch (SPEC6_8).",
		metrics.DefaultMaxSeries,
		"arch",
	)

	// adoptionReconciledTotal counts SPEC6_8 in-place reconcile heals — a
	// requestable IndexTarget fetched into an existing degraded snapshot —
	// by architecture (bounded enum). Pairs inversely with
	// acu_serve_snapshot_index_target_404_total, which falls to zero as a
	// suite heals. The arch label comes from the GPG-verified Release
	// declaration (not client-controlled), so no bounding helper is needed.
	adoptionReconciledTotal = metrics.NewCounterWithCap(
		"acu_adoption_reconciled_total",
		"Requestable IndexTargets healed into an existing snapshot by in-place reconcile, by architecture (SPEC6_8).",
		metrics.DefaultMaxSeries,
		"architecture",
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
	case errors.Is(err, ErrAdoptionUpstreamOlder), errors.Is(err, ErrAdoptionRollbackRefused):
		return "refused"
	case errors.Is(err, ErrAdoptionReleaseDateInFuture):
		return "release_date_in_future"
	case errors.Is(err, ErrAdoptionPanicked):
		return "panicked"
	// timed_out stays ahead of the member and DB sentinels: a deadline that
	// surfaces through a %w chain is timed_out — including a commit that
	// hit commitTimeout. Member-fetch errors flatten their cause with %v,
	// so they never match here; spawnAdoption reclassifies as timed_out an
	// attempt whose adoption.max_duration passed in the member pipeline,
	// never one whose deadline passed in the flip barrier (ruling R-C2).
	case errors.Is(err, context.DeadlineExceeded):
		return "timed_out"
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
	switch outcome {
	case "refused":
		if errors.Is(err, ErrAdoptionUpstreamOlder) {
			return cache.ReasonUpstreamOlder
		}
		return cache.ReasonRollbackRefused
	case "panicked":
		return cache.ReasonPanic
	}
	if outcome == "gpg_failed" && gpgClassifier != nil {
		if r := gpgClassifier(err); r != "" {
			return r
		}
	}
	return outcome
}
