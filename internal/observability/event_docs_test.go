package observability

import "testing"

// mainEventsWithoutARow are the snake_case events main already logs that
// have no docs/log-fields.md row (some are described in its prose
// sections; they predate the flat-repository branch). Every other event
// must have a row there (review sweep M-11 of the H1 plan); giving one of
// these a row removes it here.
var mainEventsWithoutARow = []string{
	"admin_authenticated",
	"admin_cpu_profile_finished",
	"admin_cpu_profile_started",
	"admin_disabled",
	"admin_profile_write_failed",
	"admin_refresh_complete",
	"admin_request",
	"admin_scrape_error",
	"admin_status_render_failed",
	"admin_unauthenticated_non_loopback",
	"adoption_blocked_serve_target_missing",
	"adoption_byhash_fallback",
	"adoption_form_drift",
	"adoption_gpg_failed",
	"adoption_heartbeat_blobs_failed",
	"adoption_heartbeat_failed",
	"adoption_hot_prefetch_complete",
	"adoption_hot_prefetch_partial",
	"adoption_hot_prefetch_started",
	"adoption_member_repair_failed",
	"adoption_member_repaired",
	"adoption_member_retry",
	"adoption_parse_failed",
	"adoption_reconcile_member_failed",
	"adoption_reconcile_snapshot_displaced",
	"adoption_repair_bump_failed",
	"adoption_repair_pass",
	"adoption_repair_snapshot_displaced",
	"adoption_required_index_target_missing",
	"adoption_short_keyid_fallback",
	"adoption_snapshot_reconciled",
	"adoption_success",
	"adoption_unpinned_suite",
	"adoption_untrusted_signer",
	"adoption_unverified_signer",
	"at_rest_corruption",
	"at_rest_scan_finished",
	"at_rest_scan_started",
	"freshness_check",
	"gc_disabled",
	"gc_pool_malformed_name",
	"gc_pool_misplaced_file",
	"gc_pool_scan_dir_failed",
	"gc_pool_scan_lookup_failed",
	"gc_pool_unexpected_root_file",
	"gc_pool_unexpected_subdir",
	"gc_pool_unlink_failed",
	"gc_pool_unlink_skipped_invalid_hash",
	"gc_run_complete",
	"gc_tick_deadline_reached",
	"gc_tick_failed",
	"hash_validation_failure",
	"hit_path_hash_evicted",
	"hot_prefetch_budget_unbounded",
	"hot_prefetch_deb_failed",
	"hot_prefetch_hash_mismatch",
	"htpasswd_reload_failed",
	"metrics_series_cap_reached",
	"mitm_ca_generated",
	"mitm_ca_generation_failed",
	"mitm_ca_load_failed",
	"mitm_ca_loaded",
	"mitm_ca_lock_timeout",
	"mitm_ca_name_constraints_skipped",
	"mitm_ca_unconstrained_refused",
	"mitm_cert_cache_evicted",
	"mitm_cert_issued",
	"mitm_clock_skew",
	"mitm_connect",
	"package_hash_incomplete_binary_skipped",
	"package_hash_miss",
	"pdiff_index_parse_failed",
	"pdiff_index_parsed",
	"refresher_query_failed",
	"refuse_unvouched_debs_inert",
	"request",
	"schema_migrating",
	"snapshot_member_refetch_mismatch",
	"source_hash_conflict",
	"source_parsed",
	"stale_serve",
	"tls_mitm_enabled",
	"tls_mitm_enabled_ca_undistributed",
	"tls_mitm_narrowing_regex_set",
	"unvouched_deb_passthrough_no_coverage",
	"unvouched_deb_refused",
}

// Review sweep M-11 of the H1 plan: every snake_case event production code
// logs has a docs/log-fields.md row (documentedEvents), main's events
// without one excepted; each of those is still logged and still without a
// row, so the list stays exact.
func TestEveryEventIsDocumented(t *testing.T) {
	documented := documentedEvents(t)
	known := map[string]bool{}
	for _, e := range mainEventsWithoutARow {
		known[e] = true
	}
	msgs := logMessages(t)
	for msg, where := range msgs {
		if !snakeCase.MatchString(msg) {
			continue
		}
		switch {
		case !documented[msg] && !known[msg]:
			t.Errorf("%s: event %s has no row in docs/log-fields.md; add one", where, msg)
		case documented[msg] && known[msg]:
			t.Errorf("%s has a docs/log-fields.md row now: remove it from mainEventsWithoutARow", msg)
		}
	}
	for _, e := range mainEventsWithoutARow {
		if _, ok := msgs[e]; !ok {
			t.Errorf("%s is no longer logged: remove it from mainEventsWithoutARow", e)
		}
	}
}
