package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"math"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/observability"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// statusModel is the SPEC5 §10.5 status-page JSON shape, also used
// as the html/template input. Field tags match the locked JSON
// schema; new fields are additive only.
//
// The `gc` key is always present per SPEC5 §10.5 — when no GC run
// has completed since process start, the JSON renders as
// `"gc": {"last_run_unixtime": null}` with the rest of the gc.*
// block omitted (a custom MarshalJSON on gcInfo handles this).
type statusModel struct {
	Process         processInfo      `json:"process"`
	Cache           cacheInfo        `json:"cache"`
	CacheSummary    cacheSummary     `json:"cache_summary"`
	RepoCoverage    repoCoverageInfo `json:"repo_coverage"`
	Listeners       []listenerInfo   `json:"listeners"`
	TLSMITM         *tlsMITMInfo     `json:"tls_mitm"`
	Suites          []suiteEntry     `json:"suites"`
	GC              *gcInfo          `json:"gc"`
	HotURLPaths     []hotURLEntry    `json:"hot_url_paths"`
	RecentAdoptions []adoptionEntry  `json:"recent_adoptions"`
	ActiveHosts     []activeHostInfo `json:"active_hosts"`
	Keyring         []keyringEntry   `json:"keyring"`
}

// htmlRenderModel is the additive presentation wrapper used by the
// admin status HTML template path only. It embeds statusModel verbatim
// (so every JSON-contract field remains reachable from the template
// via Go's field promotion: `.Cache.BytesUsed`, etc.) and adds
// operator-facing context the template needs but the JSON contract
// does not expose. See docs/admin-ui-spec.md §0.7 — this wrapper is
// the single allowed deviation from the "template input == statusModel"
// rule and exists strictly so the template can answer questions like
// "is adoption enabled?" and "what is the GC interval?" without
// polluting the wire contract.
//
// The wrapper is constructed in renderStatus() immediately before
// template.Execute. The JSON path is unchanged and never touches the
// wrapper.
type htmlRenderModel struct {
	statusModel               // embedded; every JSON field reachable as before
	AdoptionEnabled   bool    // cfg.Keyring != nil at server build time
	GCIntervalSeconds float64 // cfg.GC.Interval() expressed in seconds; 0 when unknown
	AcceptAnySigner   bool    // cfg.AdoptionAcceptAnySigner — drives the relaxation chip and hint
}

// keyringEntry is one loaded GPG entity surfaced on the status page.
// SourcePath identifies the .gpg/.asc file the key came from on disk,
// or the pseudo-path "embedded:<name>" when the key is one of the
// canonical archive keys baked into the binary. SubkeyFingerprints
// is non-nil (empty slice for keys with no subkeys) so JSON consumers
// always see the schema key.
type keyringEntry struct {
	PrimaryFingerprint string   `json:"primary_fingerprint"`
	PrimaryUID         string   `json:"primary_uid"`
	SourcePath         string   `json:"source_path"`
	SubkeyFingerprints []string `json:"subkey_fingerprints"`
}

// cacheSummary is the SPEC6_5 §2.4 cache_summary block. Keyed by
// canonical host; each host carries its by_architecture breakdown.
// Always present in the JSON; empty `by_host` when no adoption has
// populated package_hash rows yet (so consumers always see the schema
// key with at least the by_host: {} shape).
//
// AIDEV-NOTE: the JSON shape nests by_host under cache_summary —
// a flat top-level keyed-by-host map would collide with future
// summary-level fields (totals, etc.). The by_host indirection is the
// SPEC6_5 §2.4 contract.
type cacheSummary struct {
	ByHost map[string]cacheSummaryHost `json:"by_host"`
}

// cacheSummaryHost is one host's cache_summary entry. Its only
// content is the per-architecture map; the wrapper struct exists so
// future per-host fields (e.g. total bytes, percentage of cache) can
// be added without re-shaping the top-level map.
type cacheSummaryHost struct {
	ByArchitecture map[string]cacheSummaryArchEntry `json:"by_architecture"`
}

type cacheSummaryArchEntry struct {
	PackageHashCount int64 `json:"package_hash_count"`
	BlobCount        int64 `json:"blob_count"`
	BlobBytes        int64 `json:"blob_bytes"`
}

// sortedHostSummary is one HTML-template row: a host plus its
// architectures pre-sorted by (host, arch) name so the rendered page
// is deterministic regardless of Go's randomized map iteration order.
type sortedHostSummary struct {
	Host          string
	Architectures []sortedArchSummary
}

type sortedArchSummary struct {
	Arch  string
	Entry cacheSummaryArchEntry
}

// Sorted returns the cache_summary contents flattened into a host-then-
// arch sorted slice for the HTML template. Used by status.html — JSON
// consumers read the map form via ByHost directly.
func (cs cacheSummary) Sorted() []sortedHostSummary {
	hosts := make([]string, 0, len(cs.ByHost))
	for h := range cs.ByHost {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	out := make([]sortedHostSummary, 0, len(hosts))
	for _, h := range hosts {
		archMap := cs.ByHost[h].ByArchitecture
		names := make([]string, 0, len(archMap))
		for a := range archMap {
			names = append(names, a)
		}
		sort.Strings(names)
		arches := make([]sortedArchSummary, 0, len(names))
		for _, n := range names {
			arches = append(arches, sortedArchSummary{
				Arch:  n,
				Entry: archMap[n],
			})
		}
		out = append(out, sortedHostSummary{Host: h, Architectures: arches})
	}
	return out
}

// repoCoverageInfo is the SPEC6_5 §2.4 status-page repo_coverage
// section. ArchitecturesSeen is sourced from current snapshots'
// package_hash rows; ArchitecturesFilter echoes the operator's
// [adoption].architectures setting (empty list when unset). The two
// counts and the per-kind row totals come from one cache.GetRepoCoverage
// call. Always present in the JSON; populated zero-values when no
// adoption has run yet.
type repoCoverageInfo struct {
	ArchitecturesSeen    []string            `json:"architectures_seen"`
	ArchitecturesFilter  []string            `json:"architectures_filter"`
	SnapshotsWithSources int64               `json:"snapshots_with_sources"`
	SnapshotsWithPdiff   int64               `json:"snapshots_with_pdiff"`
	PackageHashRows      packageHashRowsInfo `json:"package_hash_rows"`
}

type packageHashRowsInfo struct {
	Binary int64 `json:"binary"`
	Source int64 `json:"source"`
	Pdiff  int64 `json:"pdiff"`
	Total  int64 `json:"total"`
}

// tlsMITMInfo is the SPEC6 §10.4 status-page TLS MITM section.
// JSON shape mirrors the gcInfo abbreviated/full split: when
// Enabled is false the MarshalJSON below emits exactly
// `{"enabled": false}`; full payload otherwise.
//
// LastIssued and HitRate60sPercent are pointer-typed so encoding/json
// renders nil as JSON null when no issuance has been recorded /
// no lookups in the 60s window — the same "absent vs present"
// distinction gcInfo uses for last_run_unixtime, so consumers can
// tell "no data" apart from a real zero/empty.
type tlsMITMInfo struct {
	Enabled             bool            `json:"enabled"`
	CASource            string          `json:"ca_source"`
	CAFingerprintSHA256 string          `json:"ca_fingerprint_sha256"`
	CANotAfterUnixTime  int64           `json:"ca_not_after_unixtime"`
	EffectiveAllowlist  string          `json:"effective_allowlist"`
	CertCache           certCacheInfo   `json:"cert_cache"`
	LastIssued          *lastIssuedInfo `json:"last_cert_issued"`
	HitRate60sPercent   *float64        `json:"cert_hit_rate_60s_percent"`
	// HitRate60sObserved is the raw (hits+misses) sample count for
	// the percentage. Surfaced in JSON so an operator scraping the
	// page can tell "0% of 800 lookups" apart from "no data".
	HitRate60sObserved int `json:"cert_hit_rate_60s_observed"`
}

type certCacheInfo struct {
	Size     int `json:"size"`
	Capacity int `json:"capacity"`
}

type lastIssuedInfo struct {
	Host       string `json:"host"`
	AtUnixTime int64  `json:"at_unixtime"`
}

// MarshalJSON renders `{"enabled": false}` when the section is
// disabled (matching SPEC6 §10.4's contract that the JSON top-level
// `tls_mitm` key is always present, abbreviated when MITM is off).
// The type-alias trick avoids infinite recursion when delegating
// to encoding/json for the populated case.
func (t *tlsMITMInfo) MarshalJSON() ([]byte, error) {
	if t == nil || !t.Enabled {
		return []byte(`{"enabled":false}`), nil
	}
	type alias tlsMITMInfo
	return json.Marshal((*alias)(t))
}

type processInfo struct {
	Version         string `json:"version"`
	StartedUnixTime int64  `json:"started_unixtime"`
	UptimeSeconds   int64  `json:"uptime_seconds"`
	VCSRevision     string `json:"vcs_revision"`
	GoVersion       string `json:"go_version"`
}

type cacheInfo struct {
	Dir                   string `json:"dir"`
	BytesUsed             int64  `json:"bytes_used"`
	BlobCount             int64  `json:"blob_count"`
	URLPathCount          int64  `json:"url_path_count"`
	ZeroRefcountBacklog   int64  `json:"zero_refcount_backlog"`
	ActuallyReapableBlobs int64  `json:"actually_reapable_blobs"`
}

type listenerInfo struct {
	Role string `json:"role"`
	Addr string `json:"addr"`
}

type suiteEntry struct {
	Host                             string `json:"host"`
	SuitePath                        string `json:"suite_path"`
	LastCheckUnixTime                *int64 `json:"last_check_unixtime"`
	LastSuccessUnixTime              *int64 `json:"last_success_unixtime"`
	CurrentSnapshotID                *int64 `json:"current_snapshot_id"`
	CurrentSnapshotAdoptedAtUnixTime *int64 `json:"current_snapshot_adopted_at_unixtime"`
	InReleaseChangeSeenAtUnixTime    *int64 `json:"inrelease_change_seen_at_unixtime"`

	// Lagging renders "(lagging Xh Ym)" / "(lagging Xm)" while an
	// upstream InRelease change first seen at
	// InReleaseChangeSeenAtUnixTime has not been adopted — the lag runs
	// from that first sighting to now (spec C7), not the retired
	// two-stamp comparison against last_success_at (which every check
	// bumps, so it could never fire). Empty when there is nothing to
	// show: no sighting yet, a future stamp (clock step), or a refusal
	// reason where the cache already serves content at least as new as
	// upstream (see refusalKeepsCurrent) — there the "refused: …" text
	// in AdoptionStatus is the signal, not a lag. Excluded from JSON —
	// consumers compute the signal themselves from the record.
	Lagging string `json:"-"`

	// Spec C7 "Visibility" (phase B). AnchorForm is "" for a suite with no
	// current snapshot. The adoption_fail_* fields mirror the failure
	// record; adoption_refused marks a refusal state (not a failure).
	AnchorForm                string  `json:"anchor_form"`
	LastRequestedUnixTime     *int64  `json:"last_requested_unixtime"`
	CheckFailCount            int64   `json:"check_fail_count"`
	AdoptionFailReason        *string `json:"adoption_fail_reason"`
	AdoptionFailCount         int64   `json:"adoption_fail_count"`
	AdoptionFailFirstUnixTime *int64  `json:"adoption_fail_first_unixtime"`
	AdoptionNextRetryUnixTime *int64  `json:"adoption_next_retry_unixtime"`
	AdoptionRefused           bool    `json:"adoption_refused"`

	// HTML-only: AdoptionFailing marks a failing (not refused) record;
	// AdoptionStatus is its one-line summary; LagSeconds feeds the panel
	// state. JSON consumers compute these from the fields above.
	AdoptionFailing bool   `json:"-"`
	AdoptionStatus  string `json:"-"`
	LagSeconds      int64  `json:"-"`
	// LagWarn marks a lag beyond 1 h: an adoption normally completes in
	// minutes, so a longer lag means at least one attempt failed. Rows and
	// the LAGGING count use it; the "(lagging …)" text shows any lag.
	LagWarn bool `json:"-"`
	// RefusedLong marks a refusal whose streak began more than
	// refusalEscalateAge ago: still a state, not a failure, but one that
	// may be a silent freeze, so its row and the SUITES vital warn (never
	// crit) and the REFUSED 24H+ count shows it.
	RefusedLong bool `json:"-"`
}

// gcInfo carries the SPEC5 §10.5 gc.* block. The pointer-typed
// LastRunUnixTime distinguishes "no run yet" (nil → JSON null) from
// "ran at unix epoch" (which is operationally impossible but
// type-distinct). When LastRunUnixTime is nil, the custom
// MarshalJSON below emits ONLY {"last_run_unixtime": null}, omitting
// every other field — SPEC5 §10.5 / §11 explicitly requires the
// pre-first-run shape to be the abbreviated form.
//
// AIDEV-NOTE: the abbreviated-vs-full split cannot be expressed via
// `json:",omitempty"` on the other fields because a real GC run
// might legitimately produce zero-valued fields (e.g. blobs_reaped=0
// on a clean cache), and omitempty would silently drop them.
// MarshalJSON is the only correct shape-control here.
type gcInfo struct {
	LastRunUnixTime         *int64  `json:"last_run_unixtime"`
	LastRunPhase            string  `json:"last_run_phase"`
	LastRunBlobsReaped      int     `json:"last_run_blobs_reaped"`
	LastRunBytesReclaimed   int64   `json:"last_run_bytes_reclaimed"`
	LastRunDeadlineReached  bool    `json:"last_run_deadline_reached"`
	LastRunDurationSeconds  float64 `json:"last_run_duration_seconds"`
	OrphanCandidatesReaped  int     `json:"orphan_candidates_reaped"`
	DisplacedReaped         int     `json:"displaced_reaped"`
	URLPathRowsReaped       int     `json:"url_path_rows_reaped"`
	PoolOrphansRepaired     int     `json:"pool_orphans_repaired"`
	PoolOrphanBytesRepaired int64   `json:"pool_orphan_bytes_repaired"`
	PoolUnlinkErrors        int     `json:"pool_unlink_errors"`
}

// MarshalJSON renders the abbreviated form when no GC run has
// completed (LastRunUnixTime == nil) per SPEC5 §10.5 / §11; full
// form otherwise. The type-alias trick avoids infinite recursion
// when delegating back to encoding/json for the populated case.
func (g *gcInfo) MarshalJSON() ([]byte, error) {
	if g == nil || g.LastRunUnixTime == nil {
		return []byte(`{"last_run_unixtime":null}`), nil
	}
	type alias gcInfo
	return json.Marshal((*alias)(g))
}

type hotURLEntry struct {
	Host                  string `json:"host"`
	Path                  string `json:"path"`
	IsMetadata            bool   `json:"is_metadata"`
	RequestCount          int64  `json:"request_count"`
	LastRequestedUnixTime int64  `json:"last_requested_unixtime"`
}

type adoptionEntry struct {
	Host      string `json:"host"`
	SuitePath string `json:"suite_path"`
	Outcome   string `json:"outcome"`
	// Reason is the SPEC5 §10.5 additive sub-classification. Empty on
	// success; for gpg_failed it carries the specific verifier
	// sentinel (untrusted_signer, short_keyid, crypto_verify_failed,
	// missing_signature, malformed_clearsign, ambiguous_keyid,
	// no_usable_signature); for other failures it mirrors Outcome.
	// JSON-omitted when empty so the wire shape is unchanged for
	// success rows.
	Reason string `json:"reason,omitempty"`
	// MemberPath is the SPEC5 §10.5 additive field naming the declared
	// Release member that failed (member_fetch_failed / member_mismatch),
	// e.g. "Contents-amd64". JSON-omitted when empty so success rows keep
	// the unchanged wire shape.
	MemberPath string `json:"member_path,omitempty"`
	// Detail is the SPEC5 §10.5 additive short description of a member
	// failure, e.g. "served 114572 vs declared 1664594". JSON-omitted
	// when empty.
	Detail            string  `json:"detail,omitempty"`
	CompletedUnixTime int64   `json:"completed_unixtime"`
	DurationSeconds   float64 `json:"duration_seconds"`
}

type activeHostInfo struct {
	Host         string `json:"host"`
	Inflight     int    `json:"inflight"`
	SlotCapacity int    `json:"slot_capacity"`
}

// buildStatusModel composes the renderModel from the data sources.
// Each DB query runs under its OWN 5s context.WithTimeout (SPEC5
// §9.7.3 per-query timeout — not a shared 5s for the whole render).
// On any DB error, buildStatusModel returns the failing query name
// alongside the error so the §9.7.3 admin_status_render_failed Warn
// can name the specific call that timed out.
//
// In-memory accessors (hostsem.Snapshot, gc.LastRunSummary, ring
// Snapshot) cannot fail and are not deadlined.
//
// SPEC5 §10.5: the cache.* block (blob_count, total_bytes, etc.) is
// sourced from cache.GetCacheStats — three cheap DB queries running
// under their own deadline. The §9.7.6 refresher uses the same
// helper to populate the corresponding metric gauges; the two views
// always agree on the same row counts.
func (s *Server) buildStatusModel(r *http.Request) (statusModel, string, error) {
	suitesRaw, err := s.runDBQuery(r, "ListSuitesWithAdoption", func(ctx context.Context) (any, error) {
		return s.cfg.Cache.ListSuitesWithAdoption(ctx)
	})
	if err != nil {
		return statusModel{}, "ListSuitesWithAdoption", err
	}
	hotPaths, err := s.runDBQuery(r, "ListHotURLPaths", func(ctx context.Context) (any, error) {
		return s.cfg.Cache.ListHotURLPaths(ctx, 20)
	})
	if err != nil {
		return statusModel{}, "ListHotURLPaths", err
	}
	stats, err := s.runDBQuery(r, "GetCacheStats", func(ctx context.Context) (any, error) {
		return s.cfg.Cache.GetCacheStats(ctx)
	})
	if err != nil {
		return statusModel{}, "GetCacheStats", err
	}
	st := stats.(cache.CacheStats)

	// SPEC6_5 §9.7.6: repo_coverage and cache_summary read from the
	// refresher-populated atomic.Pointers, NOT a live DB query. The
	// renderer cannot stall a slow /metrics scraper behind these
	// aggregates. Their observations can lag by admin.gauge_refresh
	// plus refresh work time. Both pointers are nil before the first refresh
	// completes; the render uses zero-value defaults in that window
	// so the JSON contract (top-level keys always present) holds.
	var repo cache.RepoCoverage
	if rcp := s.repoCoverage.Load(); rcp != nil {
		repo = *rcp
	}
	var summaryMap map[string]map[string]cache.CacheSummaryEntry
	if smp := s.cacheSummaryByHostArch.Load(); smp != nil {
		summaryMap = *smp
	}

	uptime := time.Since(s.cfg.StartTime)
	model := statusModel{
		Process: processInfo{
			Version:         s.cfg.BuildInfo.Version,
			StartedUnixTime: s.cfg.StartTime.Unix(),
			UptimeSeconds:   int64(uptime.Seconds()),
			VCSRevision:     s.cfg.BuildInfo.VCSRevision,
			GoVersion:       s.cfg.BuildInfo.GoVersion,
		},
		Cache: cacheInfo{
			Dir:                   s.cfg.Cache.Dir(),
			BytesUsed:             st.TotalBytes,
			BlobCount:             st.BlobCount,
			URLPathCount:          st.URLPathCount,
			ZeroRefcountBacklog:   st.ZeroRefcountBacklog,
			ActuallyReapableBlobs: st.ActuallyReapableBlobs,
		},
		CacheSummary:    buildCacheSummary(summaryMap),
		RepoCoverage:    buildRepoCoverageInfo(repo, s.cfg.AdoptionArchitectures),
		Listeners:       buildListenerInfo(s.cfg),
		TLSMITM:         buildTLSMITMInfo(s.cfg.TLSMITM),
		Suites:          buildSuiteEntries(suitesRaw.([]cache.SuiteWithAdoption), wallclock.Now().Unix()),
		HotURLPaths:     buildHotURLEntries(hotPaths.([]cache.HotURLPath)),
		RecentAdoptions: buildAdoptionEntries(s.cfg.Ring.Snapshot()),
		ActiveHosts:     buildActiveHostEntries(s.cfg.HostLimiter.Snapshot()),
		Keyring:         buildKeyringEntries(s.cfg.Keyring),
	}

	// SPEC5 §10.5: gc is always present at the top level. When no GC
	// run has completed, gcInfo.MarshalJSON emits the abbreviated
	// {"last_run_unixtime":null} form. The HTML template guards on
	// LastRunUnixTime != nil to choose the populated-vs-empty branch.
	gci := &gcInfo{}
	if summary, ok := s.cfg.GC.LastRunSummary(); ok {
		ts := summary.AtUnixTime
		gci.LastRunUnixTime = &ts
		gci.LastRunPhase = summary.Phase
		gci.LastRunBlobsReaped = summary.BlobsReaped
		gci.LastRunBytesReclaimed = summary.BytesReclaimed
		gci.LastRunDeadlineReached = summary.DeadlineReached
		gci.LastRunDurationSeconds = summary.DurationSeconds
		gci.OrphanCandidatesReaped = summary.OrphanCandidatesReaped
		gci.DisplacedReaped = summary.DisplacedReaped
		gci.URLPathRowsReaped = summary.URLPathRowsReaped
		gci.PoolOrphansRepaired = summary.PoolOrphansRepaired
		gci.PoolOrphanBytesRepaired = summary.PoolOrphanBytesRepaired
		gci.PoolUnlinkErrors = summary.PoolUnlinkErrors
	}
	model.GC = gci

	return model, "", nil
}

// runDBQuery wraps a DB call in its own 5s context.WithTimeout
// derived from the request context. SPEC5 §9.7.3 per-query timeout.
// The label is for diagnostic logging only; runDBQuery itself does
// not log.
func (s *Server) runDBQuery(r *http.Request, _ string, fn func(context.Context) (any, error)) (any, error) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	return fn(ctx)
}

// buildTLSMITMInfo translates the cmd-supplied snapshot into the
// status-page model. nil provider → disabled section (the snapshot's
// Enabled field also being false would produce the same shape;
// the nil short-circuit avoids interface-method dispatch).
func buildTLSMITMInfo(p TLSMITMProvider) *tlsMITMInfo {
	if p == nil {
		return &tlsMITMInfo{Enabled: false}
	}
	snap := p.TLSMITMSnapshot()
	if !snap.Enabled {
		return &tlsMITMInfo{Enabled: false}
	}
	out := &tlsMITMInfo{
		Enabled:             true,
		CASource:            snap.CASource,
		CAFingerprintSHA256: snap.CAFingerprintSHA256,
		CANotAfterUnixTime:  snap.CANotAfterUnixTime,
		EffectiveAllowlist:  snap.EffectiveAllowlist,
		CertCache: certCacheInfo{
			Size:     snap.CertCacheSize,
			Capacity: snap.CertCacheCapacity,
		},
		HitRate60sObserved: snap.HitRate60sHits + snap.HitRate60sMisses,
	}
	if !snap.LastIssuedAt.IsZero() {
		ts := snap.LastIssuedAt.Unix()
		out.LastIssued = &lastIssuedInfo{
			Host:       snap.LastIssuedHost,
			AtUnixTime: ts,
		}
	}
	if obs := snap.HitRate60sHits + snap.HitRate60sMisses; obs > 0 {
		pct := 100 * float64(snap.HitRate60sHits) / float64(obs)
		out.HitRate60sPercent = &pct
	}
	return out
}

// buildCacheSummary translates the refresher-cached per-(host, arch)
// map into the SPEC6_5 §2.4 cache_summary block. by_host is always
// non-nil so the JSON renders as `"by_host": {}` (not `null`) when no
// adoption has populated package_hash rows yet — the schema contract
// is "always present, possibly empty".
func buildCacheSummary(m map[string]map[string]cache.CacheSummaryEntry) cacheSummary {
	out := cacheSummary{ByHost: map[string]cacheSummaryHost{}}
	for host, byArch := range m {
		entry := cacheSummaryHost{
			ByArchitecture: make(map[string]cacheSummaryArchEntry, len(byArch)),
		}
		for arch, e := range byArch {
			entry.ByArchitecture[arch] = cacheSummaryArchEntry{
				PackageHashCount: e.PackageHashCount,
				BlobCount:        e.BlobCount,
				BlobBytes:        e.BlobBytes,
			}
		}
		out.ByHost[host] = entry
	}
	return out
}

// buildRepoCoverageInfo translates the cache.RepoCoverage query result
// into the SPEC6_5 §2.4 status-page payload, splicing in the operator's
// architectures filter. Both ArchitecturesSeen and ArchitecturesFilter
// are guaranteed non-nil so the JSON renders as `[]` (not `null`)
// when empty — the spec contract is "empty list when unset."
func buildRepoCoverageInfo(rc cache.RepoCoverage, filter []string) repoCoverageInfo {
	seen := rc.ArchitecturesSeen
	if seen == nil {
		seen = []string{}
	}
	filterCopy := make([]string, 0, len(filter))
	if len(filter) > 0 {
		filterCopy = append(filterCopy, filter...)
	}
	return repoCoverageInfo{
		ArchitecturesSeen:    seen,
		ArchitecturesFilter:  filterCopy,
		SnapshotsWithSources: rc.SnapshotsWithSources,
		SnapshotsWithPdiff:   rc.SnapshotsWithPdiff,
		PackageHashRows: packageHashRowsInfo{
			Binary: rc.PackageHashRowsBinary,
			Source: rc.PackageHashRowsSource,
			Pdiff:  rc.PackageHashRowsPdiff,
			Total:  rc.PackageHashRowsTotal,
		},
	}
}

func buildListenerInfo(cfg Config) []listenerInfo {
	out := []listenerInfo{
		{Role: "proxy", Addr: cfg.ProxyAddr},
	}
	if cfg.TLSAddr != "" {
		out = append(out, listenerInfo{Role: "tls", Addr: cfg.TLSAddr})
	}
	out = append(out, listenerInfo{Role: "admin", Addr: cfg.AdminAddr})
	return out
}

func buildSuiteEntries(rows []cache.SuiteWithAdoption, now int64) []suiteEntry {
	out := make([]suiteEntry, 0, len(rows))
	for _, r := range rows {
		e := suiteEntry{
			Host:                             r.CanonicalHost,
			SuitePath:                        r.SuitePath,
			LastCheckUnixTime:                r.LastCheckAt,
			LastSuccessUnixTime:              r.LastSuccessAt,
			CurrentSnapshotID:                r.CurrentSnapshotID,
			CurrentSnapshotAdoptedAtUnixTime: r.CurrentAdoptedAt,
			InReleaseChangeSeenAtUnixTime:    r.InReleaseChangeSeenAt,
			AnchorForm:                       r.CurrentAnchorForm,
			LastRequestedUnixTime:            r.LastRequestedAt,
			CheckFailCount:                   r.CheckFailCount,
			AdoptionFailReason:               r.AdoptionFailReason,
			AdoptionFailCount:                r.AdoptionFailCount,
			AdoptionFailFirstUnixTime:        r.AdoptionFailFirstAt,
			AdoptionNextRetryUnixTime:        r.AdoptionNextRetryAt,
		}
		if r.AdoptionFailCount > 0 && r.AdoptionFailReason != nil {
			e.AdoptionRefused = cache.IsRefusalReason(*r.AdoptionFailReason)
			e.AdoptionFailing = !e.AdoptionRefused
			e.RefusedLong = e.AdoptionRefused && streakAge(r.SuiteFreshness, now) > refusalEscalateAge
			e.AdoptionStatus = adoptionStatus(r.SuiteFreshness, e.AdoptionRefused, e.RefusedLong, now)
		}
		// A refusal whose reason means the cache already serves content
		// at least as new as upstream (refusalKeepsCurrent) keeps its
		// first-sighting inrelease_change_seen_at forever — RecordAdoptionFailure
		// never clears it, and deferred checks keep the first sighting.
		// Showing that as growing lag would eventually push the Suites
		// vital to warn/crit for a suite that isn't actually behind (spec
		// C7: a refusal is a state, not a failure). Suppress the lag for
		// those rows; the "refused: …" text in AdoptionStatus is the
		// signal, and RefusedLong escalates one older than a day.
		if !e.AdoptionRefused || !refusalKeepsCurrent(*r.AdoptionFailReason) {
			e.Lagging, e.LagSeconds = laggingAnnotation(r.InReleaseChangeSeenAt, now)
			e.LagWarn = e.LagSeconds > 3600
		}
		out = append(out, e)
	}
	return out
}

// laggingAnnotation renders "(lagging …)" while an upstream change first
// seen at seenAt has not been adopted, and returns the lag in seconds.
// The lag runs from the first sighting to now (spec C7): the old rule
// compared it with last_success_at, which every check bumps, so it could
// never fire. A stamp in the future (clock step) shows no lag.
func laggingAnnotation(seenAt *int64, now int64) (string, int64) {
	if seenAt == nil || *seenAt > now {
		return "", 0
	}
	lag := now - *seenAt
	return "(lagging " + durationOf(lag) + ")", lag
}

// refusalEscalateAge (seconds) is how old a refusal streak gets before
// the page escalates it (RefusedLong). It matches the 24 h after which
// the checker logs WARN adoption_refused_persistently
// (freshness.refusalEscalateAge).
const refusalEscalateAge = 24 * 3600

// streakAge is how long the record's streak has lasted, in seconds. A
// streak start in the future (clock step) counts as now: age 0.
func streakAge(s cache.SuiteFreshness, now int64) int64 {
	if s.AdoptionFailFirstAt == nil || *s.AdoptionFailFirstAt >= now {
		return 0
	}
	return now - *s.AdoptionFailFirstAt
}

// adoptionStatus summarises the spec C7 record for the suites table:
// "adoption failing 3d 2h · untrusted_signer · retry in 41m",
// "refused: upstream_older · retry due", or, for a refusal older than
// refusalEscalateAge, "refused: upstream_older · since 1d 1h · retry due".
func adoptionStatus(s cache.SuiteFreshness, refused, long bool, now int64) string {
	retry := "retry due"
	if s.AdoptionNextRetryAt != nil && *s.AdoptionNextRetryAt > now {
		retry = "retry in " + durationOf(*s.AdoptionNextRetryAt-now)
	}
	if refused {
		if long {
			return "refused: " + *s.AdoptionFailReason + " · since " + durationOf(streakAge(s, now)) + " · " + retry
		}
		return "refused: " + *s.AdoptionFailReason + " · " + retry
	}
	return "adoption failing " + durationOf(streakAge(s, now)) + " · " + *s.AdoptionFailReason + " · " + retry
}

// suitesForHTML adapts the suite rows to the HTML page. With adoption
// disabled nothing is attempted or adopted, so a record stored while it
// was enabled is inert: its text gains " (adoption disabled)" and it marks
// no warn (final review S I3). A lag is truthful but expected — Phase 1
// never refreshes metadata — so rows keep their lag text without warning,
// and vitalState shows the SUITES vital stale with "ADOPTION DISABLED —
// METADATA NOT REFRESHED" (ruling A4). The JSON keeps the raw fields.
func suitesForHTML(suites []suiteEntry, adoptionEnabled bool) []suiteEntry {
	if adoptionEnabled {
		return suites
	}
	out := slices.Clone(suites)
	for i := range out {
		e := &out[i]
		if e.AdoptionStatus != "" {
			e.AdoptionStatus += " (adoption disabled)"
		}
		e.AdoptionFailing, e.RefusedLong, e.LagWarn = false, false, false
	}
	return out
}

// refusalKeepsCurrent reports whether an adoption refusal for the given
// reason leaves the cache already serving content at least as new as
// upstream, so there is nothing to catch up on and buildSuiteEntries
// must not render it as lag (spec C7: a refusal is a state, not a
// failure; RecordAdoptionFailure never clears inrelease_change_seen_at
// and deferred checks keep the first sighting, so an unsuppressed row
// would accrue lag forever and eventually push the Suites vital to warn
// and then crit for a suite that isn't actually behind).
//
//   - ReasonUpstreamOlder: upstream's Release Date is older than (or
//     missing after) the current snapshot's — the cache is ahead or tied.
//     A missing Date says nothing about the content's age, which is why
//     a refusal older than refusalEscalateAge warns anyway (RefusedLong):
//     a vendor that stops emitting Date must not freeze silently.
//   - ReasonRollbackRefused: upstream is serving content identical to an
//     older snapshot the cache already replaced (a flapping mirror) —
//     not a step backward for the cache.
//
// Every other refusal reason — the phase E reasons
// (unsigned_downgrade_refused, unsigned_unconfirmed, unsigned_too_large,
// anchor_absent, unsigned_disabled) — means the cache really is behind
// (e.g. a vendor that stopped signing, or adopt_unsigned turned off), so
// those keep the ordinary lag behaviour; the operator must act (the
// spec's POST /forget escape hatch, or the knob). Extend the
// case list here, not at the call site, when phase E adds reasons that
// also leave the cache current.
func refusalKeepsCurrent(reason string) bool {
	switch reason {
	case cache.ReasonUpstreamOlder, cache.ReasonRollbackRefused:
		return true
	}
	return false
}

func buildHotURLEntries(rows []cache.HotURLPath) []hotURLEntry {
	out := make([]hotURLEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, hotURLEntry{
			Host:                  r.Host,
			Path:                  r.Path,
			IsMetadata:            r.IsMetadata,
			RequestCount:          r.RequestCount,
			LastRequestedUnixTime: r.LastRequestedAtUnix,
		})
	}
	return out
}

func buildAdoptionEntries(events []observability.AdoptionEvent) []adoptionEntry {
	out := make([]adoptionEntry, 0, len(events))
	for _, e := range events {
		out = append(out, adoptionEntry{
			Host:              e.Host,
			SuitePath:         e.SuitePath,
			Outcome:           e.Outcome,
			Reason:            e.Reason,
			MemberPath:        e.MemberPath,
			Detail:            e.Detail,
			CompletedUnixTime: e.CompletedUnixSec,
			DurationSeconds:   e.DurationSeconds,
		})
	}
	return out
}

// buildActiveHostEntries converts hostsem.Snapshot output to the
// status-page schema. The map iteration order is randomized; for
// Phase 5 we accept the random order and let the JSON consumer
// sort if it cares.
func buildActiveHostEntries(stats map[string]hostsem.HostStat) []activeHostInfo {
	out := make([]activeHostInfo, 0, len(stats))
	for host, st := range stats {
		out = append(out, activeHostInfo{
			Host:         host,
			Inflight:     st.Inflight,
			SlotCapacity: st.Capacity,
		})
	}
	return out
}

// buildKeyringEntries renders the loaded GPG keyring inventory for
// the status page. nil provider (e.g. adoption disabled) yields an
// empty slice so the JSON contract — top-level `keyring` key always
// present — holds. Entries are sorted by primary fingerprint for a
// deterministic order regardless of load sequence.
func buildKeyringEntries(p KeyringProvider) []keyringEntry {
	if p == nil {
		return []keyringEntry{}
	}
	snaps := p.KeyringSnapshot()
	out := make([]keyringEntry, 0, len(snaps))
	for _, s := range snaps {
		subFPs := s.SubkeyFingerprints
		if subFPs == nil {
			subFPs = []string{}
		}
		out = append(out, keyringEntry{
			PrimaryFingerprint: s.PrimaryFingerprint,
			PrimaryUID:         s.PrimaryUID,
			SourcePath:         s.SourcePath,
			SubkeyFingerprints: subFPs,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].PrimaryFingerprint < out[j].PrimaryFingerprint
	})
	return out
}

// renderStatus renders the SPEC5 §10.5 status page in either JSON
// or HTML per content negotiation (§9.7.3).
func (s *Server) renderStatus(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	format := chooseFormat(r)
	defer func() {
		s.self.statusTotal.Inc()
		s.self.statusDurationSeconds.Observe(time.Since(start).Seconds(), format)
	}()
	model, failingQuery, err := s.buildStatusModel(r)
	if err != nil {
		s.logger.Warn("admin_status_render_failed",
			"err", err.Error(),
			"format", format,
			"query", failingQuery)
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		gw, closeGz := gzipIfAccepted(w, r)
		defer func() { _ = closeGz() }()
		if err := writeStatusJSON(gw, model); err != nil {
			s.logger.Warn("admin_status_render_failed",
				"err", err.Error(),
				"format", "json",
				"query", "json.Encode")
		}
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	gw, closeGz := gzipIfAccepted(w, r)
	defer func() { _ = closeGz() }()
	if err := statusHTMLTemplate.Execute(gw, s.buildHTMLRenderModel(model)); err != nil {
		s.logger.Warn("admin_status_render_failed",
			"err", err.Error(),
			"format", "html",
			"query", "template.Execute")
	}
}

// writeStatusJSON emits the SPEC5 §10.5 status payload to w using the
// canonical wire encoding (two-space indent, default HTML escaping).
// This is the SINGLE production JSON path — TestJSONContractPreserved
// (DoD #2) calls the same helper so the regression gate exercises the
// real bytes operators see, not a duplicated encoder configuration.
// Any future change to the wire encoding (indent, escape, etc.) must
// happen here.
func writeStatusJSON(w io.Writer, m statusModel) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}

// buildHTMLRenderModel composes the docs/admin-ui-spec.md §0.7 wrapper
// from the wire-shaped statusModel plus the bits of server config the
// HTML template needs but the JSON contract intentionally does not
// expose. JSON path never calls this.
//
// AIDEV-NOTE: AdoptionEnabled — the spec hint at §0.7 says
// `cfg.Keyring != nil at server build time`, but in production the cmd
// entry point always passes a non-nil KeyringProvider whose snapshot is
// nil when adoption is disabled (see cmd/apt-cacher-ultra/main.go
// keyringProvider adapter). §2.3 forbids editing cmd and admin/server.go
// to add an explicit AdoptionEnabled field, so the wrapper uses the
// snapshot-nil convention as the operator-facing signal: a nil snapshot
// means "no keyring wired at all" → adoption disabled, while a non-nil
// (possibly empty) slice means "adoption enabled, keys may not be loaded
// yet". See .phase-loop-notes.md "Spec issues" for the followup.
func (s *Server) buildHTMLRenderModel(m statusModel) htmlRenderModel {
	w := htmlRenderModel{statusModel: m}
	if s.cfg.Keyring != nil && s.cfg.Keyring.KeyringSnapshot() != nil {
		w.AdoptionEnabled = true
	}
	w.Suites = suitesForHTML(m.Suites, w.AdoptionEnabled)
	if s.cfg.GC != nil {
		w.GCIntervalSeconds = s.cfg.GC.Interval().Seconds()
	}
	w.AcceptAnySigner = s.cfg.AdoptionAcceptAnySigner
	return w
}

func chooseFormat(r *http.Request) string {
	if wantsJSON(r) {
		return "json"
	}
	return "html"
}

// statusHTMLTemplate renders the SPEC5 §9.7.3 HTML status page.
// html/template auto-escapes every interpolated value; never
// switch to text/template without a full security review.
//
// AIDEV-NOTE: visual ground truth is docs/admin-ui-mockup.html; the
// implementation contract is docs/admin-ui-spec.md. When the mockup and
// spec disagree the mockup wins (per the spec's opening paragraph). The
// data-* attribute contract this template emits is the §7 contract the
// inline JS below reads — keep them in sync in the same change.

// statusTemplateFuncMap returns the html/template func map used by the
// admin status page. Existing helpers are preserved verbatim; the
// docs/admin-ui-spec.md §6.1 helpers (chunkHex … verdictExplanation) are
// added here.
func statusTemplateFuncMap() template.FuncMap {
	return template.FuncMap{
		"unixTime":            formatUnixTimeTag,
		"unixTimePtr":         formatUnixTimePtrTag,
		"formatBytes":         formatBytes,
		"durationOf":          durationOf,
		"i64Ptr":              formatInt64Ptr,
		"hitRatePct":          formatHitRatePercent,
		"defaultEmpty":        defaultEmpty,
		"chunkHex":            chunkHex,
		"sourceKind":          sourceKind,
		"sourceKindLabel":     sourceKindLabel,
		"countBundled":        countBundled,
		"countSystem":         countSystem,
		"countCustom":         countCustom,
		"formatShortDuration": formatShortDuration,
		"outcomeBadgeClass":   outcomeBadgeClass,
		"reasonTooltip":       reasonTooltip,
		"vitalState":          vitalState,
		"verdictExplanation":  verdictExplanation,
		"add1":                func(n int) int { return n + 1 },
		"splitUID":            splitUID,
	}
}

// reasonTooltip maps a SPEC5 §10.5 recent_adoptions[].reason short tag
// to a one-line human explanation for the title= attribute on the
// reason chip. Unknown tags pass through verbatim so future reasons
// surface without a code change (just less helpful hover text until
// the table is extended).
func reasonTooltip(reason string) string {
	switch reason {
	case "untrusted_signer":
		return "The InRelease signature's issuer key is not in the host keyring. Install the upstream's signing key under /usr/share/keyrings/ or /etc/apt/keyrings/."
	case "short_keyid":
		return "Signature lacks the long-form IssuerFingerprint subpacket and adoption.allow_short_keyid is false."
	case "no_usable_signature":
		return "No signature packet in the clearsigned block carried a trusted IssuerFingerprint."
	case "missing_signature":
		return "InRelease body is not clearsigned. Plain-text InRelease files are never adopted as signed metadata, whatever adoption.require_signature says."
	case "malformed_clearsign":
		return "InRelease starts like a clearsigned message but its signature frame does not decode."
	case "ambiguous_keyid":
		return "Two or more keys in the trust set share the same 8-byte keyid; the short-keyid fallback refuses to guess."
	case "crypto_verify_failed":
		return "The signature's issuer was trusted but the cryptographic verification itself failed."
	case "unpinned_suite":
		return "adoption.require_pinned_signer is true and no [[trusted_signer]] block matches this canonical host."
	case "parse_failed":
		return "Verified Release-style body did not parse (malformed Release file)."
	case "member_mismatch":
		return "A Release-listed member's fetched hash did not match the declared hash. The failing member and digests are shown in the row detail."
	case "member_fetch_failed":
		return "A Release-listed member could not be fetched intact — size/content-length mismatch (often an index republished mid-fetch, or a non-essential member the upstream serves with the wrong body), a transport error, or a 5xx. The failing member and detail are shown in the row."
	case "db_failed":
		return "Adoption hit a local cache/DB error (blob write, rehash, or snapshot insert), not an upstream problem. It is retried on the backoff schedule."
	case "run_failed":
		return "Adoption failed for a reason not otherwise categorized; consult the adoption_run_failed log line."
	case "refused":
		return "Not a failure: the adoption was refused by a rule (see the reason). The current snapshot keeps serving; the same content is not retried until its retry time, new content is tried at once."
	case "upstream_older":
		return "The upstream Release's Date is older than the current snapshot's, or missing after a dated one: apt would refuse it too. Refused, not failed."
	case "rollback_refused":
		return "Upstream serves content identical to an older snapshot the cache already replaced (a flapping mirror). Refused, not failed."
	case "release_date_in_future":
		return "The Release's Date is more than 10 s ahead of the cache's clock (apt's Acquire::Max-FutureTime). Check the cache host's clock."
	case "timed_out":
		return "The adoption exceeded adoption.max_duration and was abandoned; it is retried on the backoff schedule."
	case "panic", "panicked":
		return "The adoption goroutine panicked; it was recovered and recorded. See the adoption_panicked log line."
	}
	return reason
}

// uidParts is the parsed shape of an OpenPGP user-id string, exposed
// to the status template so the keyring panel can render the name and
// the email on separate lines (per docs/admin-ui-mockup.html).
type uidParts struct {
	Name  string
	Email string
}

// splitUID parses a UID of the form "Display Name (Comment) <local@domain>"
// into its parts. UIDs without a trailing "<…>" segment return the whole
// string as Name with Email = "". Leading/trailing whitespace on Name is
// trimmed; the email is taken verbatim from inside the angle brackets.
func splitUID(s string) uidParts {
	s = strings.TrimSpace(s)
	if !strings.HasSuffix(s, ">") {
		return uidParts{Name: s}
	}
	i := strings.LastIndex(s, "<")
	if i < 0 {
		return uidParts{Name: s}
	}
	return uidParts{
		Name:  strings.TrimSpace(s[:i]),
		Email: strings.TrimSuffix(s[i+1:], ">"),
	}
}

// AIDEV-NOTE: formatUnixTimeTag returns template.HTML, bypassing
// html/template auto-escaping (per the security note at the
// statusHTMLTemplate definition). This is safe ONLY because every
// byte of output is produced from int64 + time.Format with fixed
// ASCII layouts — there is no path for user-controlled data to
// reach the rendered string. Do not extend the format to include
// any externally-sourced field without re-escaping.
//
// The emitted <time data-unix=…> element carries the raw unix
// seconds; the inline script at the bottom of the status page
// rewrites textContent to browser-local time at load. The
// server-rendered UTC string is the fallback when JS is disabled
// or Intl.DateTimeFormat cannot resolve a timezone.
func formatUnixTimeTag(unix int64) template.HTML {
	if unix == 0 {
		return ""
	}
	t := time.Unix(unix, 0).UTC()
	utc := t.Format("2006-01-02 15:04:05")
	iso := t.Format("2006-01-02T15:04:05Z")
	return template.HTML(fmt.Sprintf(
		`<time datetime="%s" data-unix="%d" title="%s UTC">%s UTC</time>`,
		iso, unix, utc, utc,
	))
}

// formatUnixTimePtrTag renders a *int64 as a <time> tag; nil → "-".
func formatUnixTimePtrTag(unix *int64) template.HTML {
	if unix == nil {
		return "-"
	}
	return formatUnixTimeTag(*unix)
}

// formatInt64Ptr renders a *int64; nil → "-".
func formatInt64Ptr(v *int64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%d", *v)
}

// formatBytes renders a byte count as a human-readable size.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// formatHitRatePercent renders the SPEC6 §10.4 hit-rate cell. nil
// percent + zero observations means no lookups in the 60s window —
// render "n/a" rather than a misleading "0%". Otherwise: "X.X%
// (N lookups)" with one decimal place.
func formatHitRatePercent(pct *float64, observed int) string {
	if pct == nil {
		return "n/a (no lookups in window)"
	}
	return fmt.Sprintf("%.1f%% (%d lookups)", *pct, observed)
}

// defaultEmpty returns fallback when s is the empty string.
// Used by the SPEC6 §10.4 effective_allowlist cell so an unset
// regex renders as "(none — vacuously true)" rather than blank.
func defaultEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// durationOf renders a wallclock-seconds count as the two most
// significant units: "Xm" under an hour, "Xh Ym" under a day, and
// "Xd Yh" once it reaches a day (minutes dropped at that scale).
func durationOf(seconds int64) string {
	d := time.Duration(seconds) * time.Second
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h >= 24 {
		return fmt.Sprintf("%dd %dh", h/24, h%24)
	}
	if h == 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh %dm", h, m)
}

// AIDEV-NOTE: helpers below (chunkHex … verdictExplanation) are registered
// in statusTemplateFuncMap() per docs/admin-ui-spec.md §6.1. All are pure
// functions over their inputs — no package-level state, no I/O. Tests in
// status_test.go are the implementation contract.

// chunkHex groups a hex string into n-character chunks separated by a
// single ASCII space. Input is lowercased. Non-hex input (anything
// containing a non-[0-9a-f] byte after lowercasing) is returned verbatim
// to make the helper safe on already-formatted or non-fingerprint values.
// n ≤ 0 returns the lowercased input unchunked.
func chunkHex(s string, n int) string {
	lower := strings.ToLower(s)
	if n <= 0 || lower == "" {
		return lower
	}
	for i := 0; i < len(lower); i++ {
		c := lower[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return s
		}
	}
	var b strings.Builder
	b.Grow(len(lower) + len(lower)/n)
	for i := 0; i < len(lower); i += n {
		end := i + n
		if end > len(lower) {
			end = len(lower)
		}
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(lower[i:end])
	}
	return b.String()
}

// sourceKind classifies a keyring source_path into one of three values
// per docs/admin-ui-spec.md §1.3:
//
//	"embedded:…"   → "bundled"
//	"/usr/share/…" → "system"
//	anything else  → "custom"
func sourceKind(p string) string {
	switch {
	case strings.HasPrefix(p, "embedded:"):
		return "bundled"
	case strings.HasPrefix(p, "/usr/share/"):
		return "system"
	default:
		return "custom"
	}
}

// sourceKindLabel returns the uppercase badge text for a source path.
func sourceKindLabel(p string) string {
	switch sourceKind(p) {
	case "bundled":
		return "BUNDLED"
	case "system":
		return "SYSTEM"
	default:
		return "CUSTOM"
	}
}

// countBundled counts keyring entries whose source path classifies as bundled.
func countBundled(ks []keyringEntry) int { return countSource(ks, "bundled") }

// countSystem counts keyring entries whose source path classifies as system.
func countSystem(ks []keyringEntry) int { return countSource(ks, "system") }

// countCustom counts keyring entries whose source path classifies as custom.
func countCustom(ks []keyringEntry) int { return countSource(ks, "custom") }

func countSource(ks []keyringEntry, kind string) int {
	n := 0
	for _, k := range ks {
		if sourceKind(k.SourcePath) == kind {
			n++
		}
	}
	return n
}

// formatShortDuration formats a duration (in seconds, possibly fractional)
// as the most human-friendly short form per docs/admin-ui-spec.md §6.1:
//
//	< 1s     → "%d ms" (rounded to nearest ms)
//	< 60s    → "%.1f s"
//	< 3600s  → "%dm %ds"
//	otherwise → "%dh %dm"
//
// Negative inputs are clamped to zero; NaN/Inf return "—".
func formatShortDuration(seconds float64) string {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return "—"
	}
	if seconds < 0 {
		seconds = 0
	}
	if seconds < 1 {
		return fmt.Sprintf("%d ms", int(math.Round(seconds*1000)))
	}
	if seconds < 60 {
		return fmt.Sprintf("%.1f s", seconds)
	}
	if seconds < 3600 {
		m := int(seconds) / 60
		s := int(seconds) % 60
		return fmt.Sprintf("%dm %ds", m, s)
	}
	h := int(seconds) / 3600
	m := (int(seconds) % 3600) / 60
	return fmt.Sprintf("%dh %dm", h, m)
}

// vitalState returns the data-state value for one of the five hero-strip
// "vital" cells: "cache", "suites", "adoptions", "gc", "active". Returned
// values are "ok", "warn", "crit", or "stale". Unknown kind returns "ok"
// so a typo in the template never surfaces as a panic during rendering.
//
// AIDEV-NOTE: the returned values are the markup-side / CSS-selector
// vocabulary (`ok` / `warn` / `crit` / `stale`); the operator-facing
// verdict labels (HEALTHY / WATCHING / DEGRADED) are a separate
// vocabulary computed in §8.1's JS from these tokens. See
// docs/admin-ui-spec.md §7.1 for the canonical mapping.
//
// Takes the htmlRenderModel wrapper because the warn threshold for
// GC (last run age > 2 × interval) needs GCIntervalSeconds, which is
// not on statusModel (per the JSON-contract preservation rule §0.4).
func vitalState(kind string, m htmlRenderModel) string {
	switch kind {
	case "cache":
		if m.GC != nil && m.GC.PoolUnlinkErrors > 0 {
			return "crit"
		}
		if m.Cache.BytesUsed == 0 {
			return "stale"
		}
		if m.Cache.ActuallyReapableBlobs > 1000 {
			return "warn"
		}
		return "ok"
	case "suites":
		if len(m.Suites) == 0 {
			return "stale"
		}
		if !m.AdoptionEnabled {
			// ADOPTION DISABLED — METADATA NOT REFRESHED (§9.1, the
			// keyring panel's precedent): the lag is expected, not a
			// fault, so it must not hold the page at DEGRADED.
			return "stale"
		}
		var worstLag int64
		anyWarn := false
		for _, s := range m.Suites {
			worstLag = max(worstLag, s.LagSeconds)
			anyWarn = anyWarn || s.AdoptionFailing || s.RefusedLong
		}
		switch {
		case worstLag > 24*3600:
			return "crit"
		case worstLag > 3600 || anyWarn:
			// An adoption normally completes within minutes; a lag beyond
			// the 1 h retry cap means at least one attempt failed. A
			// refusal older than a day warns, never crits (ruling A5).
			return "warn"
		}
		return "ok"
	case "adoptions":
		n := len(m.RecentAdoptions)
		if n == 0 {
			if m.Process.UptimeSeconds < 300 {
				return "stale"
			}
			return "ok"
		}
		// A "refused" outcome is a state, not a failure (spec C7): it is
		// excluded from the failure side of the ratio, same as "success".
		// The denominator stays the full window (refusals are real events,
		// just not failing ones), so a window of only refusals + successes
		// keeps this cell "ok".
		fail := 0
		for _, a := range m.RecentAdoptions {
			if a.Outcome != "success" && a.Outcome != "refused" {
				fail++
			}
		}
		ratio := float64(fail) / float64(n)
		if ratio >= 0.5 {
			return "crit"
		}
		if ratio >= 0.1 {
			return "warn"
		}
		return "ok"
	case "gc":
		if m.GC == nil || m.GC.LastRunUnixTime == nil {
			return "stale"
		}
		if m.GC.LastRunDeadlineReached {
			return "crit"
		}
		// AIDEV-NOTE: GCIntervalSeconds==0 means "interval unknown" —
		// per §9.1 the warn branch is suppressed in that case and the
		// cell goes straight from ok to stale.
		if m.GCIntervalSeconds > 0 {
			now := m.Process.StartedUnixTime + m.Process.UptimeSeconds
			ageSeconds := now - *m.GC.LastRunUnixTime
			if float64(ageSeconds) > 2*m.GCIntervalSeconds {
				return "warn"
			}
		}
		return "ok"
	case "active":
		if len(m.ActiveHosts) == 0 && m.Process.UptimeSeconds < 300 {
			return "stale"
		}
		return "ok"
	default:
		return "ok"
	}
}

// keyringCrit reports whether the keyring panel is in the §5.1.1 / §8.1
// "adoption enabled but no keys loaded" critical condition. Mirrors the
// JS verdict's keys-chip read so the noscript fallback stays consistent
// with the live pill.
//
// accept_any_signer suppresses the crit: under that mode unpinned suites
// adopt via the bypass branch (no key consulted), so an empty keyring
// does not block adoption. Pinned suites with no matching key still
// fail at adoption time with no_usable_signature — but that surfaces as
// a per-row error in the adoptions table rather than as a vital-cell
// crit on the keyring chip.
func keyringCrit(m htmlRenderModel) bool {
	return m.AdoptionEnabled && len(m.Keyring) == 0 && !m.AcceptAnySigner
}

// verdictExplanation produces the server-side fallback verdict string
// emitted inside <noscript> per docs/admin-ui-spec.md §8.1 — when JS is
// off, the live JS-computed pill never runs, and this string is what the
// operator sees. JS computes the live version from the same per-cell
// state attributes at runtime.
//
// The output mirrors §9.2's verdict order (crit → watching → warming-up
// → healthy) collapsed into one sentence; the per-cell badges below
// carry the specific diagnostic. Keyring crit (§5.1.1: adoption enabled
// with no keys loaded) is folded into the crit count so the noscript
// verdict cannot say "nominal" while the keyring chip would be tinted
// red under JS.
func verdictExplanation(m htmlRenderModel) string {
	cells := []string{"cache", "suites", "adoptions", "gc", "active"}
	crit, watching := 0, 0
	for _, k := range cells {
		switch vitalState(k, m) {
		case "crit":
			crit++
		case "warn":
			watching++
		}
	}
	if keyringCrit(m) {
		crit++
	}
	if crit > 0 {
		return fmt.Sprintf("Degraded — %d signal(s) in critical state; see badges below.", crit)
	}
	if watching > 0 {
		return fmt.Sprintf("Watching — %d of %d vital cells elevated; see badges below.", watching, len(cells))
	}
	if m.Process.UptimeSeconds < 300 && (m.GC == nil || m.GC.LastRunUnixTime == nil) {
		return fmt.Sprintf("Warming up — uptime %s, awaiting first GC run.", durationOf(m.Process.UptimeSeconds))
	}
	return fmt.Sprintf("All systems nominal — uptime %s.", durationOf(m.Process.UptimeSeconds))
}

// outcomeBadgeClass maps an adoption outcome enum string to one of the
// `.b--*` badge classes used in the admin template. Enum values are
// produced by internal/freshness/metrics.go's classifier:
//
//	"success", "gpg_failed", "parse_failed", "member_mismatch",
//	"member_fetch_failed", "db_failed", "unpinned_suite", "run_failed",
//	"refused", "release_date_in_future", "timed_out", "panicked".
//
// "refused" is a state, not a failure (spec C7): the adoption was
// refused by a rule (upstream_older, rollback_refused, …), the current
// snapshot keeps serving, and the row maps to b--warn like the soft-state
// values below. Every other non-success outcome is critical for the
// operator (the row's adoption did not land); they map to b--crit.
// Unknown outcomes — e.g. a future enum value the classifier adds before
// this helper is updated — map to b--crit too rather than the previous
// b--stale, since "future failure mode we don't recognise" is closer to
// crit than to stale. Existing soft-state values ("lagging", "warn")
// keep their b--warn mapping for non-adoption use sites.
func outcomeBadgeClass(outcome string) string {
	switch outcome {
	case "success":
		return "b--ok"
	case "lagging", "warn", "refused":
		return "b--warn"
	case "":
		return "b--stale"
	default:
		return "b--crit"
	}
}
