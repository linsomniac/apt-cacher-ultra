package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/observability"
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
	Dir                 string `json:"dir"`
	BytesUsed           int64  `json:"bytes_used"`
	BlobCount           int64  `json:"blob_count"`
	URLPathCount        int64  `json:"url_path_count"`
	ZeroRefcountBacklog int64  `json:"zero_refcount_backlog"`
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

	// Lagging is the SPEC5 §12.2.4 HTML annotation rendered next
	// to InReleaseChangeSeenAtUnixTime when the upstream advertised
	// a newer InRelease that we haven't successfully refetched yet.
	// Empty otherwise. Excluded from JSON — consumers compute the
	// signal themselves from the two timestamps. Format: "(lagging
	// Xh Ym)" / "(lagging Xm)" matching the operator-facing
	// duration helper.
	Lagging string `json:"-"`
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
	// missing_signature, ambiguous_keyid, no_usable_signature); for
	// other failures it mirrors Outcome. JSON-omitted when empty so
	// the wire shape is unchanged for success rows.
	Reason            string  `json:"reason,omitempty"`
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
	// aggregates, and they only change at adoption time (snapshot
	// flip) — operationally fine to be up to admin.gauge_refresh
	// stale. Both pointers are nil before the first refresh
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
			Dir:                 s.cfg.Cache.Dir(),
			BytesUsed:           st.TotalBytes,
			BlobCount:           st.BlobCount,
			URLPathCount:        st.URLPathCount,
			ZeroRefcountBacklog: st.ZeroRefcountBacklog,
		},
		CacheSummary:    buildCacheSummary(summaryMap),
		RepoCoverage:    buildRepoCoverageInfo(repo, s.cfg.AdoptionArchitectures),
		Listeners:       buildListenerInfo(s.cfg),
		TLSMITM:         buildTLSMITMInfo(s.cfg.TLSMITM),
		Suites:          buildSuiteEntries(suitesRaw.([]cache.SuiteWithAdoption)),
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

func buildSuiteEntries(rows []cache.SuiteWithAdoption) []suiteEntry {
	out := make([]suiteEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, suiteEntry{
			Host:                             r.CanonicalHost,
			SuitePath:                        r.SuitePath,
			LastCheckUnixTime:                r.LastCheckAt,
			LastSuccessUnixTime:              r.LastSuccessAt,
			CurrentSnapshotID:                r.CurrentSnapshotID,
			CurrentSnapshotAdoptedAtUnixTime: r.CurrentAdoptedAt,
			InReleaseChangeSeenAtUnixTime:    r.InReleaseChangeSeenAt,
			Lagging:                          laggingAnnotation(r.InReleaseChangeSeenAt, r.LastSuccessAt),
		})
	}
	return out
}

// laggingAnnotation renders the SPEC5 §12.2.4 "(lagging Xh Ym)"
// suffix when the upstream's InRelease changed after our last
// successful re-adoption (= the cache is serving older metadata
// than upstream advertises). Both inputs may be nil — in which
// case we cannot determine lag and return "".
func laggingAnnotation(seenAt, successAt *int64) string {
	if seenAt == nil || successAt == nil {
		return ""
	}
	if *seenAt <= *successAt {
		return ""
	}
	gap := time.Duration(*seenAt-*successAt) * time.Second
	h := int(gap.Hours())
	m := int(gap.Minutes()) % 60
	if h == 0 {
		return fmt.Sprintf("(lagging %dm)", m)
	}
	return fmt.Sprintf("(lagging %dh %dm)", h, m)
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
		return "InRelease body is not clearsigned and adoption.require_signature is true."
	case "ambiguous_keyid":
		return "Two or more keys in the trust set share the same 8-byte keyid; the short-keyid fallback refuses to guess."
	case "crypto_verify_failed":
		return "The signature's issuer was trusted but the cryptographic verification itself failed."
	case "unpinned_suite":
		return "adoption.require_pinned_signer is true and no [[trusted_signer]] block matches this canonical host."
	case "parse_failed":
		return "Verified Release-style body did not parse (malformed Release file)."
	case "member_mismatch":
		return "A Release-listed member's fetched hash did not match the declared hash."
	case "run_failed":
		return "Adoption failed for an uncategorized reason; consult the adoption_run_failed log line."
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

var statusHTMLTemplate = template.Must(template.New("status").Funcs(statusTemplateFuncMap()).Parse(statusHTML))

const statusHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="refresh" content="60">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>apt-cacher-ultra status</title>
<link rel="icon" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 16'%3E%3Crect width='16' height='16' fill='%237A2E0A'/%3E%3Ctext x='3' y='12' font-family='ui-monospace,monospace' font-size='12' fill='%23FAFAF7' font-weight='700'%3E%C2%AB%3C/text%3E%3C/svg%3E">
<script>(function(){try{var s=localStorage.getItem('acu-theme');if(s==='light'||s==='dark')document.documentElement.setAttribute('data-theme',s);}catch(e){}})();</script>
<style>
:root{
--ink-0:#FAFAF7;--ink-1:#F2F1EC;--ink-2:#E5E3DA;--ink-3:#C7C4B8;
--ink-4:#6E6A5D;--ink-5:#3A3833;--ink-6:#1A1815;
--accent:#7A2E0A;--ok:#2E5D3A;--warn:#A36410;--crit:#9A1F1B;--stale:#5C5A52;
--row-tint:rgba(0,0,0,0.018);--crit-tint:rgba(154,31,27,0.04);--warn-tint:rgba(163,100,16,0.045);
--font-sans:ui-sans-serif,system-ui,-apple-system,"Segoe UI",sans-serif;
--font-mono:ui-monospace,"SF Mono","Cascadia Mono",Menlo,Consolas,monospace;
--max-w:1400px;--rail-w:200px;--bar-h:64px;
--xs:11px;--sm:12.5px;--base:14px;--md:16px;--lg:20px;--xl:28px;
--r-pill:999px;--r-badge:2px;
}
@media (prefers-color-scheme:dark){:root{
--ink-0:#15171A;--ink-1:#1B1E22;--ink-2:#2A2D32;--ink-3:#4A4D54;
--ink-4:#8E8F94;--ink-5:#C5C6CB;--ink-6:#EDEEF1;
--accent:#E08B5A;--ok:#7FB68E;--warn:#E0B05A;--crit:#E7716E;--stale:#7A7C82;
--row-tint:rgba(255,255,255,0.022);--crit-tint:rgba(231,113,110,0.06);--warn-tint:rgba(224,176,90,0.05);
}}
[data-theme="light"]{--ink-0:#FAFAF7;--ink-1:#F2F1EC;--ink-2:#E5E3DA;--ink-3:#C7C4B8;--ink-4:#6E6A5D;--ink-5:#3A3833;--ink-6:#1A1815;--accent:#7A2E0A;--ok:#2E5D3A;--warn:#A36410;--crit:#9A1F1B;--stale:#5C5A52;--row-tint:rgba(0,0,0,0.018);--crit-tint:rgba(154,31,27,0.04);--warn-tint:rgba(163,100,16,0.045)}
[data-theme="dark"]{--ink-0:#15171A;--ink-1:#1B1E22;--ink-2:#2A2D32;--ink-3:#4A4D54;--ink-4:#8E8F94;--ink-5:#C5C6CB;--ink-6:#EDEEF1;--accent:#E08B5A;--ok:#7FB68E;--warn:#E0B05A;--crit:#E7716E;--stale:#7A7C82;--row-tint:rgba(255,255,255,0.022);--crit-tint:rgba(231,113,110,0.06);--warn-tint:rgba(224,176,90,0.05)}
*{box-sizing:border-box}
html,body{margin:0;padding:0;background:var(--ink-0);color:var(--ink-5);font-family:var(--font-sans);font-size:var(--base);line-height:1.55;-webkit-font-smoothing:antialiased}
a{color:var(--accent);text-decoration:none;border-bottom:1px solid transparent;transition:border-color 120ms ease}
a:hover{border-bottom-color:var(--accent)}
a:focus-visible{outline:2px solid var(--accent);outline-offset:2px;border-radius:1px}
code{font-family:var(--font-mono);font-size:.92em;color:var(--ink-5);background:transparent;padding:0}
table{width:100%;border-collapse:collapse;font-variant-numeric:tabular-nums}
.bar{position:sticky;top:0;z-index:30;height:var(--bar-h);background:var(--ink-0);border-bottom:1px solid var(--ink-2);display:flex;align-items:center;padding:0 32px;gap:24px;animation:fadeIn 600ms ease both}
.bar__brand{display:flex;align-items:baseline;gap:8px;font-weight:600;font-size:var(--md);color:var(--ink-6)}
.bar__mark{font-family:var(--font-mono);font-weight:700;font-size:22px;color:var(--accent);line-height:1;margin-right:2px;position:relative;top:2px}
.bar__version{font-family:var(--font-mono);font-size:var(--xs);color:var(--ink-4);letter-spacing:.04em}
.bar__verdict{display:flex;align-items:center;gap:14px;flex:1;min-width:0}
.pill{display:inline-flex;align-items:center;gap:8px;padding:8px 16px 8px 12px;border-radius:var(--r-pill);font-weight:600;font-size:var(--sm);letter-spacing:.08em;text-transform:uppercase;white-space:nowrap}
.pill--healthy{background:color-mix(in srgb,var(--ok) 12%,transparent);color:var(--ok)}
.pill--watching{background:color-mix(in srgb,var(--warn) 14%,transparent);color:var(--warn)}
.pill--degraded{background:color-mix(in srgb,var(--crit) 14%,transparent);color:var(--crit)}
.pill--stale{background:color-mix(in srgb,var(--stale) 14%,transparent);color:var(--stale)}
.pill .dot{width:10px;height:10px;border-radius:50%;background:currentColor}
.verdict__msg{font-size:var(--sm);color:var(--ink-5);overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.verdict__msg strong{color:var(--ink-6);font-weight:600}
.bar__meta{display:flex;align-items:center;gap:16px;margin-left:auto;font-size:var(--xs);color:var(--ink-4);letter-spacing:.04em}
.bar__meta .sep{color:var(--ink-3)}
.icon-btn{width:32px;height:32px;display:inline-flex;align-items:center;justify-content:center;background:transparent;border:1px solid var(--ink-2);color:var(--ink-4);cursor:pointer;border-radius:2px;transition:background 120ms ease,color 120ms ease}
.icon-btn:hover{color:var(--ink-6);background:var(--ink-1)}
.icon-btn svg{width:16px;height:16px}
.page{max-width:var(--max-w);margin:0 auto;padding:32px 32px 96px}
.vitals{display:grid;grid-template-columns:repeat(5,minmax(0,1fr));gap:0;border:1px solid var(--ink-2);margin-bottom:32px;background:var(--ink-0)}
.vital{position:relative;padding:20px 24px 22px 28px;border-right:1px solid var(--ink-2);display:flex;flex-direction:column;gap:8px;min-height:132px}
.vital:last-child{border-right:0}
.vital::before{content:"";position:absolute;left:0;top:0;bottom:0;width:4px;background:var(--ink-2)}
.vital[data-state="ok"]::before{background:var(--ok)}
.vital[data-state="warn"]::before{background:var(--warn)}
.vital[data-state="crit"]::before{background:var(--crit)}
.vital[data-state="stale"]::before{background:var(--stale)}
.vital__label{font-size:var(--xs);letter-spacing:.1em;text-transform:uppercase;color:var(--ink-4);font-weight:500}
.vital__value{font-size:var(--xl);font-weight:600;color:var(--ink-6);letter-spacing:-.015em;line-height:1.1;font-variant-numeric:tabular-nums}
.vital__value .unit{font-size:var(--md);color:var(--ink-4);font-weight:500;margin-left:4px;letter-spacing:0}
.vital__sub{font-size:var(--sm);color:var(--ink-4);display:flex;flex-direction:column;gap:2px}
.vital__sub .accented-warn{color:var(--warn)}
.vital__sub .accented-crit{color:var(--crit)}
.vital__sub .accented-ok{color:var(--ok)}
.vital__sub .mono{font-family:var(--font-mono);font-size:12px}
.notice{border:1px solid var(--crit);border-left-width:4px;padding:16px 20px 18px;margin-bottom:24px;background:var(--crit-tint);display:flex;flex-direction:column;gap:6px}
.notice--warn{border-color:var(--warn);background:var(--warn-tint)}
.notice__head{font-size:var(--md);color:var(--ink-6);font-weight:600;display:flex;align-items:center;gap:10px}
.notice .dot{width:10px;height:10px;border-radius:50%;background:var(--crit);display:inline-block}
.notice--warn .dot{background:var(--warn)}
.notice__body{font-size:var(--sm);color:var(--ink-4);line-height:1.6}
.notice__body code{color:var(--ink-6);background:var(--ink-1);padding:1px 6px;border-radius:2px;font-size:12px}
.notice__link-row{display:block;margin-top:10px;padding-top:10px;border-top:1px solid var(--ink-2);font-size:var(--sm);color:var(--ink-4)}
.notice__link-row a{color:var(--accent);font-weight:500}
.layout{display:grid;grid-template-columns:var(--rail-w) 1fr;gap:48px;align-items:start}
.rail{position:sticky;top:calc(var(--bar-h) + 24px);border-left:1px solid var(--ink-2)}
.rail ul{list-style:none;margin:0;padding:0;display:flex;flex-direction:column;gap:2px}
.rail a{display:block;padding:6px 0 6px 16px;font-size:var(--xs);letter-spacing:.1em;text-transform:uppercase;color:var(--ink-4);font-weight:500;border-bottom:none;position:relative;transition:color 120ms ease}
.rail a:hover{color:var(--ink-6)}
.rail a[aria-current="location"]{color:var(--ink-6)}
.rail a[aria-current="location"]::before{content:"";position:absolute;left:-1px;top:50%;transform:translateY(-50%);width:2px;height:16px;background:var(--accent)}
.content{min-width:0}
section.panel{margin-bottom:48px;scroll-margin-top:calc(var(--bar-h) + 16px)}
.panel__eyebrow{font-size:var(--xs);letter-spacing:.12em;text-transform:uppercase;color:var(--ink-4);font-weight:500;margin-bottom:4px;display:flex;align-items:baseline;gap:12px;flex-wrap:wrap}
.panel__eyebrow .count-warn{color:var(--warn)}
.panel__eyebrow .count-crit{color:var(--crit)}
.panel__eyebrow .count-ok{color:var(--ok)}
.panel__eyebrow .sep{color:var(--ink-3)}
.panel__h{font-size:var(--lg);font-weight:600;color:var(--ink-6);margin:0 0 14px 0;letter-spacing:-.01em}
.panel__desc{font-size:var(--sm);color:var(--ink-4);margin:0 0 16px 0;max-width:64ch}
.table-wrap{overflow-x:auto;border-top:1px solid var(--ink-2);border-bottom:1px solid var(--ink-2)}
table.data{font-size:var(--sm);color:var(--ink-5)}
table.data thead th{background:var(--ink-1);text-align:left;font-size:var(--xs);font-weight:500;letter-spacing:.1em;text-transform:uppercase;color:var(--ink-4);padding:10px 14px;border-bottom:1px solid var(--ink-2);white-space:nowrap}
table.data tbody td{padding:9px 14px;border-bottom:0;vertical-align:middle;white-space:nowrap}
table.data tbody tr:nth-child(even){background:var(--row-tint)}
table.data tbody tr:hover{background:var(--ink-1)}
table.data .num{text-align:right;font-variant-numeric:tabular-nums}
table.data .mono{font-family:var(--font-mono);font-size:12px;color:var(--ink-5)}
table.data .host{font-family:var(--font-mono);font-size:12px;color:var(--ink-6)}
table.data .muted{color:var(--ink-3)}
table.data .time{font-family:var(--font-mono);font-size:12px;color:var(--ink-5)}
table.data tr[data-state="warn"] td:first-child{box-shadow:inset 3px 0 0 0 var(--warn)}
table.data tr[data-state="crit"] td:first-child{box-shadow:inset 3px 0 0 0 var(--crit)}
table.data tr[data-state="stale"] td:first-child{box-shadow:inset 3px 0 0 0 var(--stale)}
table.data tr[data-state="warn"]{background:var(--warn-tint)}
table.data tr[data-state="crit"]{background:var(--crit-tint)}
table.data tr[data-state="warn"]:nth-child(even){background:var(--warn-tint)}
table.data tr[data-state="crit"]:nth-child(even){background:var(--crit-tint)}
.b{display:inline-block;padding:1px 8px 2px;border-radius:var(--r-badge);border:1px solid currentColor;font-size:10px;letter-spacing:.08em;font-weight:500;text-transform:uppercase;background:transparent;line-height:1.4;white-space:nowrap}
.b--ok{color:var(--ok)}.b--warn{color:var(--warn)}.b--crit{color:var(--crit)}.b--stale{color:var(--stale)}
.b--neutral{color:var(--ink-4);border-color:var(--ink-3)}
.b.reason{margin-left:6px;letter-spacing:.06em}
.lagging{margin-left:8px;font-size:11px;color:var(--warn);font-variant-numeric:tabular-nums}
.keys-chip{display:inline-flex;align-items:center;gap:6px;padding:4px 9px 4px 8px;border:1px solid var(--ink-2);border-radius:2px;font-size:10px;letter-spacing:.1em;text-transform:uppercase;color:var(--ink-4);transition:background 120ms ease,color 120ms ease,border-color 120ms ease}
.keys-chip:hover{background:var(--ink-1);color:var(--ink-6);border-color:var(--ink-3)}
.keys-chip__count{font-family:var(--font-mono);font-size:11px;letter-spacing:0;color:var(--ink-6);font-weight:600;font-variant-numeric:tabular-nums}
.keys-chip__tick{color:var(--ok);font-size:12px;letter-spacing:0}
.keys-chip[data-state="crit"]{color:var(--crit);border-color:var(--crit)}
.keys-chip[data-state="crit"] .keys-chip__count{color:var(--crit)}
.keys-chip[data-state="crit"] .keys-chip__tick{color:var(--crit)}
.src{display:inline-block;padding:1px 7px 2px;border-radius:var(--r-badge);border:1px solid currentColor;font-size:9.5px;letter-spacing:.1em;font-weight:500;text-transform:uppercase;background:transparent;line-height:1.4;white-space:nowrap}
.src--bundled{color:var(--ink-4);border-color:var(--ink-3)}
.src--system{color:var(--ink-5);border-color:var(--ink-3)}
.src--custom{color:var(--accent)}
.src-path{display:inline-block;margin-left:8px;font-family:var(--font-mono);font-size:11px;color:var(--ink-4)}
.fp{display:inline-block;font-family:var(--font-mono);font-size:12px;letter-spacing:.02em;color:var(--ink-6);font-variant-numeric:tabular-nums;word-spacing:1px;white-space:nowrap}
.fp--sub{color:var(--ink-4);font-size:11px}
.uid{display:block;font-size:12.5px;color:var(--ink-6);line-height:1.4}
.uid__email{display:block;font-family:var(--font-mono);font-size:11px;color:var(--ink-4);margin-top:1px}
.subkeys{display:flex;flex-direction:column;gap:4px}
.subkeys--none{color:var(--ink-3);font-size:14px}
.col-hint{display:inline-block;margin-left:4px;cursor:help;position:relative}
.col-hint summary{list-style:none;display:inline-block;width:13px;height:13px;border-radius:50%;border:1px solid var(--ink-3);color:var(--ink-4);font-size:9px;font-weight:600;line-height:11px;text-align:center;text-transform:none;letter-spacing:0;vertical-align:middle;cursor:help;transition:color 120ms ease,border-color 120ms ease}
.col-hint summary::-webkit-details-marker{display:none}
.col-hint:hover summary{color:var(--ink-6);border-color:var(--ink-4)}
.col-hint[open] summary{color:var(--accent);border-color:var(--accent)}
.col-hint__body{position:absolute;top:22px;left:-8px;z-index:5;background:var(--ink-0);border:1px solid var(--ink-3);padding:10px 14px;width:280px;font-size:12px;letter-spacing:0;text-transform:none;color:var(--ink-5);line-height:1.55;font-weight:400;box-shadow:0 1px 0 0 var(--ink-2)}
.col-hint__body::before{content:"";position:absolute;top:-6px;left:8px;width:10px;height:10px;background:var(--ink-0);border-top:1px solid var(--ink-3);border-left:1px solid var(--ink-3);transform:rotate(45deg)}
.kv{display:grid;grid-template-columns:minmax(180px,30%) 1fr;gap:0 24px;border-top:1px solid var(--ink-2)}
.kv > div{padding:10px 0;border-bottom:1px solid var(--ink-2)}
.kv .k{font-size:var(--xs);letter-spacing:.1em;text-transform:uppercase;color:var(--ink-4);font-weight:500;text-align:right}
.kv .v{font-size:var(--sm);color:var(--ink-5);word-break:break-word}
.kv .v code{color:var(--ink-6);font-size:12px}
.kv .v.muted{color:var(--ink-3)}
.empty{border:1px dashed var(--ink-2);padding:24px;text-align:center}
.empty--crit{border-left:3px solid var(--crit)}
.empty__head{font-size:var(--xs);letter-spacing:.12em;text-transform:uppercase;color:var(--stale);font-weight:500;margin-bottom:6px}
.empty--crit .empty__head{color:var(--crit)}
.empty__body{font-size:var(--sm);color:var(--ink-4)}
.arch-list{display:flex;flex-wrap:wrap;gap:6px;margin-top:6px}
.arch{font-family:var(--font-mono);font-size:11px;padding:2px 8px;background:var(--ink-1);color:var(--ink-5);border-radius:2px;letter-spacing:.02em}
footer{margin-top:64px;padding-top:24px;border-top:1px solid var(--ink-2);font-size:var(--xs);color:var(--ink-4);display:flex;gap:16px;align-items:center;flex-wrap:wrap;letter-spacing:.04em}
footer .sep{color:var(--ink-3)}
@keyframes fadeIn{from{opacity:0;transform:translateY(4px)}to{opacity:1;transform:translateY(0)}}
@media (prefers-reduced-motion:reduce){*{animation-duration:.01ms !important;transition-duration:.01ms !important}}
@media (max-width:1279px){
.vitals{grid-template-columns:repeat(3,minmax(0,1fr))}
.vital:nth-child(3){border-right:0}
.vital:nth-child(n+4){border-top:1px solid var(--ink-2)}
.vital:nth-child(4){border-right:1px solid var(--ink-2)}
.layout{grid-template-columns:1fr;gap:24px}
.rail{position:sticky;top:var(--bar-h);border-left:0;border-bottom:1px solid var(--ink-2);background:var(--ink-0);padding:12px 0}
.rail ul{flex-direction:row;flex-wrap:wrap;gap:0 16px}
.rail a{padding:4px 0}
.rail a[aria-current="location"]::before{display:none}
.rail a[aria-current="location"]{border-bottom:2px solid var(--accent)}
}
@media (max-width:720px){
.page{padding:16px}
.bar{padding:12px 16px;gap:12px;height:auto;min-height:var(--bar-h);flex-wrap:wrap}
.bar__meta{margin-left:0;width:100%;justify-content:flex-end}
.vitals{grid-template-columns:1fr}
.vital{border-right:0;border-bottom:1px solid var(--ink-2)}
.vital:last-child{border-bottom:0}
.kv{grid-template-columns:1fr}
.kv > div{border-bottom:0;padding:6px 0}
.kv .k{text-align:left}
table.data thead{display:none}
table.data tbody tr{display:block;border-bottom:1px solid var(--ink-2);padding:12px 16px}
table.data tbody tr[data-state="crit"] td:first-child,
table.data tbody tr[data-state="warn"] td:first-child{box-shadow:none}
table.data tbody tr[data-state="crit"]{border-left:3px solid var(--crit)}
table.data tbody tr[data-state="warn"]{border-left:3px solid var(--warn)}
table.data tbody td{display:flex;justify-content:space-between;align-items:center;padding:4px 0;white-space:normal}
table.data tbody td::before{content:attr(data-label);font-size:10px;letter-spacing:.1em;text-transform:uppercase;color:var(--ink-4);margin-right:12px;flex-shrink:0}
}
</style>
</head>
<body data-uptime-seconds="{{.Process.UptimeSeconds}}" data-gc-runs="{{if and .GC .GC.LastRunUnixTime}}1{{else}}0{{end}}">

<svg width="0" height="0" style="position:absolute" aria-hidden="true"><defs>
<symbol id="i-arrow-out" viewBox="0 0 16 16"><path d="M6 4h6v6M11 5L4 12" stroke="currentColor" stroke-width="1.5" fill="none" stroke-linecap="square"/></symbol>
<symbol id="i-sun" viewBox="0 0 16 16"><circle cx="8" cy="8" r="3" stroke="currentColor" stroke-width="1.4" fill="none"/><path d="M8 1v2M8 13v2M1 8h2M13 8h2M3 3l1.4 1.4M11.6 11.6L13 13M3 13l1.4-1.4M11.6 4.4L13 3" stroke="currentColor" stroke-width="1.4"/></symbol>
<symbol id="i-moon" viewBox="0 0 16 16"><path d="M13 9.5A5 5 0 016.5 3 5 5 0 1013 9.5z" stroke="currentColor" stroke-width="1.4" fill="none"/></symbol>
</defs></svg>

{{$kbundled := countBundled .Keyring}}{{$ksystem := countSystem .Keyring}}{{$kcustom := countCustom .Keyring}}{{$kcount := len .Keyring}}{{$adopting := .AdoptionEnabled}}{{$keyringCritState := and $adopting (eq $kcount 0) (not .AcceptAnySigner)}}

<header class="bar" role="banner">
  <div class="bar__brand">
    <span class="bar__mark">&laquo;</span>
    <span>apt-cacher-ultra</span>
    <span class="bar__version">{{.Process.Version}}</span>
  </div>
  <div class="bar__verdict" role="status" aria-live="polite">
    <span id="verdict-pill" class="pill pill--stale" data-state="stale">
      <span class="dot"></span>
      <span id="verdict-label">STATUS</span>
    </span>
    <span class="verdict__msg" id="verdict-msg">{{verdictExplanation .}}</span>
  </div>
  <div class="bar__meta">
    <span>uptime <strong style="color:var(--ink-5);font-weight:500">{{durationOf .Process.UptimeSeconds}}</strong></span>
    <span class="sep">&middot;</span>
    <a href="#keyring" class="keys-chip"
       data-keyring-count="{{$kcount}}"
       data-adoption-enabled="{{if $adopting}}true{{else}}false{{end}}"
       {{if $keyringCritState}}data-state="crit"{{end}}
       aria-label="GPG keys loaded; jump to Keyring section">
      <span class="keys-chip__label">keys</span>
      <span class="keys-chip__count">{{$kcount}}</span>
      <span class="keys-chip__tick" aria-hidden="true">{{if $keyringCritState}}!{{else}}&#10003;{{end}}</span>
    </a>
    <span class="sep">&middot;</span>
    <span>build <code>{{.Process.VCSRevision}}</code></span>
    <span class="sep">&middot;</span>
    <button id="theme-toggle" class="icon-btn" type="button" aria-label="Toggle theme">
      <svg id="theme-icon"><use href="#i-sun"/></svg>
    </button>
    <a href="?format=json" class="icon-btn" aria-label="View as JSON">
      <svg><use href="#i-arrow-out"/></svg>
    </a>
  </div>
</header>

<main class="page">

  {{$cacheState := vitalState "cache" .}}{{$suitesState := vitalState "suites" .}}{{$adoptionsState := vitalState "adoptions" .}}{{$gcState := vitalState "gc" .}}{{$activeState := vitalState "active" .}}

  <noscript>
    <p style="margin:0 0 16px 0;padding:10px 14px;background:var(--ink-1);font-size:var(--sm);color:var(--ink-5)">
      JavaScript is disabled — verdict pill and aggregate-failure notice will not update.
      Per-cell badges below carry the full state. Times stay in UTC.
    </p>
  </noscript>

  <section class="vitals" aria-label="Vital signs">
    <article class="vital" data-vital="cache" data-state="{{$cacheState}}">
      <span class="vital__label">Cache</span>
      <span class="vital__value">{{formatBytes .Cache.BytesUsed}}</span>
      <span class="vital__sub">
        <span>{{.Cache.BlobCount}} blobs across {{.Cache.URLPathCount}} URL paths</span>
        {{if gt .Cache.ZeroRefcountBacklog 1000}}<span class="accented-warn">Zero-refcount backlog {{.Cache.ZeroRefcountBacklog}}</span>{{else}}<span>Zero-refcount backlog {{.Cache.ZeroRefcountBacklog}}</span>{{end}}
      </span>
    </article>
    <article class="vital" data-vital="suites" data-state="{{$suitesState}}">
      <span class="vital__label">Suites</span>
      <span class="vital__value">{{len .Suites}}</span>
      <span class="vital__sub">
        {{$lag := 0}}{{range .Suites}}{{if .Lagging}}{{$lag = (printf "X")}}{{end}}{{end}}
        <span class="{{if eq $suitesState "warn"}}accented-warn{{else if eq $suitesState "crit"}}accented-crit{{end}}">
          {{$lagCount := 0}}{{range .Suites}}{{if .Lagging}}{{$lagCount = 1}}{{end}}{{end}}
          {{if .Suites}}{{range $i, $s := .Suites}}{{if $s.Lagging}}{{end}}{{end}}{{end}}
          {{$nL := 0}}{{range .Suites}}{{if .Lagging}}{{end}}{{end}}
          tracked
        </span>
      </span>
    </article>
    <article class="vital" data-vital="adoptions" data-state="{{$adoptionsState}}">
      <span class="vital__label">Adoptions (recent ring)</span>
      {{$nA := len .RecentAdoptions}}{{$okA := 0}}{{range .RecentAdoptions}}{{if eq .Outcome "success"}}{{$okA = 1}}{{end}}{{end}}
      <span class="vital__value">{{$nA}}<span class="unit">events</span></span>
      <span class="vital__sub">
        <span>{{if eq $nA 0}}empty since startup{{else}}{{$nA}} in ring{{end}}</span>
        <span>see panel below</span>
      </span>
    </article>
    <article class="vital" data-vital="gc" data-state="{{$gcState}}">
      <span class="vital__label">Last GC</span>
      {{if and .GC .GC.LastRunUnixTime}}
        <span class="vital__value">{{formatShortDuration .GC.LastRunDurationSeconds}}</span>
        <span class="vital__sub">
          <span>{{unixTimePtr .GC.LastRunUnixTime}} &middot; {{defaultEmpty .GC.LastRunPhase "(unknown phase)"}}</span>
          <span>reclaimed {{formatBytes .GC.LastRunBytesReclaimed}} &middot; {{.GC.LastRunBlobsReaped}} blobs</span>
        </span>
      {{else}}
        <span class="vital__value muted">&mdash;</span>
        <span class="vital__sub"><span>NO GC RUN YET</span><span>warming up</span></span>
      {{end}}
    </article>
    <article class="vital" data-vital="active" data-state="{{$activeState}}">
      <span class="vital__label">Active fetches</span>
      <span class="vital__value">{{len .ActiveHosts}}<span class="unit">hosts</span></span>
      <span class="vital__sub">
        {{if .ActiveHosts}}<span>currently fetching</span>{{else}}<span>idle</span>{{end}}
        <span class="mono">{{if .ActiveHosts}}see panel below{{else}}no fetches in flight{{end}}</span>
      </span>
    </article>
  </section>

  <div class="layout">
    <nav class="rail" aria-label="Section navigation">
      <ul>
        <li><a href="#suites">Suites</a></li>
        <li><a href="#adoptions">Recent adoptions</a></li>
        <li><a href="#keyring">Keyring</a></li>
        <li><a href="#hot">Hot URL paths</a></li>
        <li><a href="#by-host">Cache &times; host &times; arch</a></li>
        <li><a href="#coverage">Repository coverage</a></li>
        <li><a href="#gc">Garbage collection</a></li>
        <li><a href="#active">Active hosts</a></li>
        <li><a href="#plumbing">Plumbing</a></li>
      </ul>
    </nav>

    <div class="content">

      <!-- SUITES -->
      <section class="panel" id="suites">
        <div class="panel__eyebrow">
          {{$lagCount := 0}}{{range .Suites}}{{if .Lagging}}{{$lagCount = (add1 $lagCount)}}{{end}}{{end}}
          Suites <span class="sep">&mdash;</span>
          <span>{{len .Suites}} tracked</span>
          {{if gt $lagCount 0}}<span class="sep">&middot;</span><span class="count-warn">{{$lagCount}} lagging</span>{{end}}
        </div>
        <h2 class="panel__h">Suite adoption status</h2>
        <p class="panel__desc">One row per (host, suite). Lagging rows indicate the upstream has published a newer InRelease than the snapshot the cache is currently serving.</p>
        {{if .Suites}}
        <div class="table-wrap">
          <table class="data">
            <thead><tr>
              <th>Host</th><th>Suite path</th><th>Last check</th><th>Last success</th>
              <th class="num">Snapshot</th>
              <th>Adopted<details class="col-hint"><summary aria-label="What does Adopted mean?">i</summary><div class="col-hint__body">Adoption fires only when a fresh InRelease has been observed. Suites whose upstream has not republished since process start may stay empty here without being broken.</div></details></th>
              <th>InRelease changed</th>
            </tr></thead>
            <tbody>
            {{range .Suites}}
              <tr{{if .Lagging}} data-state="warn"{{end}}>
                <td data-label="Host" class="host">{{.Host}}</td>
                <td data-label="Suite path" class="mono">{{.SuitePath}}</td>
                <td data-label="Last check" class="time">{{unixTimePtr .LastCheckUnixTime}}</td>
                <td data-label="Last success" class="time">{{unixTimePtr .LastSuccessUnixTime}}</td>
                <td data-label="Snapshot" class="num mono">{{i64Ptr .CurrentSnapshotID}}</td>
                <td data-label="Adopted" class="time">{{unixTimePtr .CurrentSnapshotAdoptedAtUnixTime}}</td>
                <td data-label="InRelease changed" class="time">{{unixTimePtr .InReleaseChangeSeenAtUnixTime}}{{if .Lagging}} <span class="lagging">{{.Lagging}}</span>{{end}}</td>
              </tr>
            {{end}}
            </tbody>
          </table>
        </div>
        {{else}}<div class="empty"><div class="empty__head">No suites tracked yet</div><div class="empty__body">Suites populate after the first adoption cycle.</div></div>{{end}}
      </section>

      <!-- RECENT ADOPTIONS -->
      <section class="panel" id="adoptions">
        {{$na := len .RecentAdoptions}}{{$okA := 0}}{{$failA := 0}}{{range .RecentAdoptions}}{{if eq .Outcome "success"}}{{$okA = (add1 $okA)}}{{else}}{{$failA = (add1 $failA)}}{{end}}{{end}}
        <div class="panel__eyebrow">
          Recent adoptions <span class="sep">&mdash;</span>
          <span>{{$na}} events in ring</span>
          {{if gt $failA 0}}<span class="sep">&middot;</span><span class="count-crit">{{$failA}} failed</span>{{end}}
          {{if gt $okA 0}}<span class="sep">&middot;</span><span class="count-ok">{{$okA}} success</span>{{end}}
        </div>
        <h2 class="panel__h">Recent adoption outcomes</h2>

        <div id="adoptions-notice" class="notice-mount" data-notice-total="{{$na}}"></div>

        {{if .RecentAdoptions}}
        <div class="table-wrap">
          <table class="data">
            <thead><tr>
              <th>Host</th><th>Suite path</th><th>Outcome</th><th>Completed</th><th class="num">Duration</th>
            </tr></thead>
            <tbody>
            {{range .RecentAdoptions}}
              <tr{{if ne .Outcome "success"}} data-state="crit"{{end}} data-outcome="{{.Outcome}}"{{if .Reason}} data-reason="{{.Reason}}"{{end}}>
                <td data-label="Host" class="host">{{.Host}}</td>
                <td data-label="Suite path" class="mono">{{.SuitePath}}</td>
                <td data-label="Outcome"><span class="b {{outcomeBadgeClass .Outcome}}">{{.Outcome}}</span>{{if and .Reason (ne .Reason .Outcome)}} <span class="b b--neutral reason" title="{{reasonTooltip .Reason}}">{{.Reason}}</span>{{end}}</td>
                <td data-label="Completed" class="time">{{unixTime .CompletedUnixTime}}</td>
                <td data-label="Duration" class="num mono">{{formatShortDuration .DurationSeconds}}</td>
              </tr>
            {{end}}
            </tbody>
          </table>
        </div>
        {{else if lt .Process.UptimeSeconds 300}}<div class="empty"><div class="empty__head">No adoptions yet</div><div class="empty__body">Empty since this process started.</div></div>{{end}}
      </section>

      <!-- KEYRING -->
      <section class="panel" id="keyring" data-accept-any-signer="{{if .AcceptAnySigner}}true{{else}}false{{end}}">
        <div class="panel__eyebrow">
          Keyring <span class="sep">&mdash;</span>
          <span data-keyring-count="{{$kcount}}">{{$kcount}} loaded</span>
          <span class="sep">&middot;</span>
          <span data-keyring-bundled="{{$kbundled}}">{{$kbundled}} bundled</span>
          <span class="sep">&middot;</span>
          <span data-keyring-system="{{$ksystem}}">{{$ksystem}} system</span>
          <span class="sep">&middot;</span>
          <span data-keyring-custom="{{$kcustom}}">{{$kcustom}} custom</span>
          {{if .AcceptAnySigner}}
          <span class="sep">&middot;</span>
          <span class="b b--warn" title="adoption.accept_any_signer is true: unpinned suites bypass signature verification at adoption time. Apt clients on the fleet remain the authoritative trust anchor.">Trust: accept any signer</span>
          {{end}}
        </div>
        <h2 class="panel__h">Trusted GPG keys</h2>
        <p class="panel__desc">Keys used to verify upstream <code>InRelease</code> signatures during adoption. Bundled keys ship with the binary; custom keys come from <code>keyring_dirs</code> paths.{{if .AcceptAnySigner}} With <code>accept_any_signer = true</code>, unpinned suites are adopted without consulting these keys; pinned suites still require a match.{{end}}</p>
        {{if .Keyring}}
        <div class="table-wrap">
          <table class="data" id="keyring-table">
            <thead><tr>
              <th>Primary fingerprint</th><th>User ID</th><th>Source</th><th>Subkey fingerprints</th>
            </tr></thead>
            <tbody>
            {{range .Keyring}}{{$kind := sourceKind .SourcePath}}{{$uid := splitUID .PrimaryUID}}
              <tr data-source-kind="{{$kind}}">
                <td data-label="Primary fingerprint"><span class="fp">{{chunkHex .PrimaryFingerprint 4}}</span></td>
                <td data-label="User ID"><span class="uid" title="{{.PrimaryUID}}">{{$uid.Name}}{{if $uid.Email}}<span class="uid__email">&lt;{{$uid.Email}}&gt;</span>{{end}}</span></td>
                <td data-label="Source">
                  <span class="src src--{{$kind}}">{{sourceKindLabel .SourcePath}}</span>
                  <span class="src-path">{{.SourcePath}}</span>
                </td>
                <td data-label="Subkey fingerprints">
                  {{if .SubkeyFingerprints}}<div class="subkeys">{{range .SubkeyFingerprints}}<span class="fp fp--sub">{{chunkHex . 4}}</span>{{end}}</div>{{else}}<span class="subkeys subkeys--none">&mdash;</span>{{end}}
                </td>
              </tr>
            {{end}}
            </tbody>
          </table>
        </div>
        {{else if and $adopting .AcceptAnySigner}}
          <div class="empty"><div class="empty__head">NO GPG KEYS LOADED</div><div class="empty__body">Adoption is enabled with <code>accept_any_signer = true</code>; unpinned suites bypass signature verification, so an empty keyring is workable here. Apt clients on the fleet remain the authoritative trust anchor.</div></div>
        {{else if $adopting}}
          <div class="empty empty--crit"><div class="empty__head">NO GPG KEYS LOADED</div><div class="empty__body">Adoption is enabled but the keyring is empty. All <code>InRelease</code> verifications will fail. Check <code>keyring_dirs</code> in the configuration.</div></div>
        {{else}}
          <div class="empty"><div class="empty__head">ADOPTION DISABLED</div><div class="empty__body">No GPG keys are loaded because adoption is disabled in the configuration.</div></div>
        {{end}}
      </section>

      <!-- HOT URL PATHS -->
      <section class="panel" id="hot">
        <div class="panel__eyebrow">
          Hot URL paths <span class="sep">&mdash;</span>
          <span>top {{len .HotURLPaths}} by request count</span>
        </div>
        <h2 class="panel__h">What clients are asking for</h2>
        {{if .HotURLPaths}}
        <div class="table-wrap">
          <table class="data">
            <thead><tr>
              <th>Host</th><th>Path</th><th>Kind</th><th class="num">Requests</th><th>Last requested</th>
            </tr></thead>
            <tbody>
            {{range .HotURLPaths}}
              <tr>
                <td data-label="Host" class="host">{{.Host}}</td>
                <td data-label="Path" class="mono">{{.Path}}</td>
                <td data-label="Kind"><span class="b {{if .IsMetadata}}b--neutral{{else}}b--ok{{end}}">{{if .IsMetadata}}metadata{{else}}payload{{end}}</span></td>
                <td data-label="Requests" class="num mono">{{.RequestCount}}</td>
                <td data-label="Last requested" class="time">{{unixTime .LastRequestedUnixTime}}</td>
              </tr>
            {{end}}
            </tbody>
          </table>
        </div>
        {{else}}<div class="empty"><div class="empty__head">No URL paths requested yet</div><div class="empty__body">The hot-paths table populates after the first cached request.</div></div>{{end}}
      </section>

      <!-- BY-HOST -->
      <section class="panel" id="by-host">
        <div class="panel__eyebrow">Cache contents <span class="sep">&mdash;</span><span>by host &times; architecture</span></div>
        <h2 class="panel__h">What's on disk, broken down</h2>
        {{with .CacheSummary.Sorted}}
        <div class="table-wrap">
          <table class="data">
            <thead><tr><th>Host</th><th>Architecture</th><th class="num">package_hash rows</th><th class="num">Blobs</th><th class="num">Bytes</th></tr></thead>
            <tbody>
            {{range .}}{{$host := .Host}}{{range .Architectures}}
              <tr>
                <td data-label="Host" class="host">{{$host}}</td>
                <td data-label="Architecture"><span class="arch">{{.Arch}}</span></td>
                <td data-label="package_hash rows" class="num mono">{{.Entry.PackageHashCount}}</td>
                <td data-label="Blobs" class="num mono">{{.Entry.BlobCount}}</td>
                <td data-label="Bytes" class="num mono">{{formatBytes .Entry.BlobBytes}}</td>
              </tr>
            {{end}}{{end}}
            </tbody>
          </table>
        </div>
        {{else}}<div class="empty"><div class="empty__head">No cached blobs yet</div><div class="empty__body">The by-host breakdown populates after the first adoption cycle.</div></div>{{end}}
      </section>

      <!-- COVERAGE -->
      <section class="panel" id="coverage">
        <div class="panel__eyebrow">Repository coverage</div>
        <h2 class="panel__h">What the cache is covering</h2>
        <div class="kv">
          <div class="k">Architectures seen</div>
          <div class="v">{{if .RepoCoverage.ArchitecturesSeen}}<div class="arch-list">{{range .RepoCoverage.ArchitecturesSeen}}<span class="arch">{{.}}</span>{{end}}</div>{{else}}<span class="muted">(none — no current snapshots have package_hash rows yet)</span>{{end}}</div>
          <div class="k">Architectures filter</div>
          <div class="v">{{if .RepoCoverage.ArchitecturesFilter}}<div class="arch-list">{{range .RepoCoverage.ArchitecturesFilter}}<span class="arch">{{.}}</span>{{end}}</div>{{else}}<code>(unfiltered &mdash; all Release-listed indices adopted)</code>{{end}}</div>
          <div class="k">Snapshots with Sources</div>
          <div class="v"><code>{{.RepoCoverage.SnapshotsWithSources}}</code></div>
          <div class="k">Snapshots with pdiff</div>
          <div class="v"><code>{{.RepoCoverage.SnapshotsWithPdiff}}</code></div>
          <div class="k">package_hash rows (binary)</div>
          <div class="v"><code>{{.RepoCoverage.PackageHashRows.Binary}}</code></div>
          <div class="k">package_hash rows (source)</div>
          <div class="v"><code>{{.RepoCoverage.PackageHashRows.Source}}</code></div>
          <div class="k">package_hash rows (pdiff)</div>
          <div class="v"><code>{{.RepoCoverage.PackageHashRows.Pdiff}}</code></div>
          <div class="k">package_hash rows (total)</div>
          <div class="v"><code>{{.RepoCoverage.PackageHashRows.Total}}</code></div>
        </div>
      </section>

      <!-- GC -->
      <section class="panel" id="gc">
        <div class="panel__eyebrow">
          Garbage collection
          {{if and .GC .GC.LastRunUnixTime}}<span class="sep">&mdash;</span><span>last run {{unixTimePtr .GC.LastRunUnixTime}}</span>{{end}}
          <span class="sep">&middot;</span><span class="count-{{$gcState}}">{{$gcState}}</span>
        </div>
        <h2 class="panel__h">Cache reaping</h2>
        {{if and .GC .GC.LastRunUnixTime}}
        <div class="kv">
          <div class="k">Last run</div>
          <div class="v"><span class="time mono">{{unixTimePtr .GC.LastRunUnixTime}}</span> &middot; <span class="b b--neutral">{{defaultEmpty .GC.LastRunPhase "unknown"}}</span></div>
          <div class="k">Duration</div>
          <div class="v"><code>{{formatShortDuration .GC.LastRunDurationSeconds}}</code></div>
          <div class="k">Blobs reaped</div>
          <div class="v"><code>{{.GC.LastRunBlobsReaped}}</code></div>
          <div class="k">Bytes reclaimed</div>
          <div class="v"><code>{{formatBytes .GC.LastRunBytesReclaimed}}</code></div>
          <div class="k">Orphan candidates reaped</div>
          <div class="v"><code>{{.GC.OrphanCandidatesReaped}}</code></div>
          <div class="k">Displaced reaped</div>
          <div class="v"><code>{{.GC.DisplacedReaped}}</code></div>
          <div class="k">Pool orphans repaired</div>
          <div class="v"><code>{{.GC.PoolOrphansRepaired}}</code> <span style="color:var(--ink-4)">({{formatBytes .GC.PoolOrphanBytesRepaired}})</span></div>
          <div class="k">Pool unlink errors</div>
          <div class="v"><code>{{.GC.PoolUnlinkErrors}}</code></div>
          <div class="k">Deadline reached</div>
          <div class="v"><span class="b {{if .GC.LastRunDeadlineReached}}b--crit{{else}}b--ok{{end}}">{{.GC.LastRunDeadlineReached}}</span></div>
        </div>
        {{else}}<div class="empty"><div class="empty__head">NO GC RUN YET</div><div class="empty__body">GC has not completed since process start.</div></div>{{end}}
      </section>

      <!-- ACTIVE HOSTS -->
      <section class="panel" id="active">
        <div class="panel__eyebrow">Active hosts <span class="sep">&mdash;</span><span>fetch-slot semaphore snapshot</span></div>
        <h2 class="panel__h">Upstream fetches in flight</h2>
        {{if .ActiveHosts}}
        <div class="table-wrap">
          <table class="data">
            <thead><tr><th>Host</th><th class="num">Inflight</th><th class="num">Slot capacity</th></tr></thead>
            <tbody>
            {{range .ActiveHosts}}
              <tr>
                <td data-label="Host" class="host">{{.Host}}</td>
                <td data-label="Inflight" class="num mono">{{.Inflight}}</td>
                <td data-label="Slot capacity" class="num mono">{{.SlotCapacity}}</td>
              </tr>
            {{end}}
            </tbody>
          </table>
        </div>
        {{else}}<div class="empty"><div class="empty__head">No hosts have held a slot since process start</div><div class="empty__body">Slot usage is bursty — this is normal between adoption cycles.</div></div>{{end}}
      </section>

      <!-- PLUMBING -->
      <section class="panel" id="plumbing">
        <div class="panel__eyebrow">Plumbing</div>
        <h2 class="panel__h">Listeners, TLS MITM, build</h2>
        <h3 style="font-size:var(--xs);letter-spacing:.1em;text-transform:uppercase;color:var(--ink-4);margin:24px 0 8px">Listeners</h3>
        <div class="kv">
          {{range .Listeners}}<div class="k">{{.Role}}</div><div class="v"><code>{{.Addr}}</code></div>{{end}}
        </div>

        {{if .TLSMITM.Enabled}}
        <h3 style="font-size:var(--xs);letter-spacing:.1em;text-transform:uppercase;color:var(--ink-4);margin:32px 0 8px">TLS MITM <span class="b b--ok" style="margin-left:8px">enabled</span></h3>
        <div class="kv">
          <div class="k">CA source</div>
          <div class="v"><span class="b b--neutral">{{.TLSMITM.CASource}}</span></div>
          <div class="k">CA fingerprint (SHA-256)</div>
          <div class="v"><span class="fp">{{chunkHex .TLSMITM.CAFingerprintSHA256 4}}</span></div>
          <div class="k">CA not_after</div>
          <div class="v"><span class="time mono">{{unixTime .TLSMITM.CANotAfterUnixTime}}</span></div>
          <div class="k">Effective allowlist</div>
          <div class="v"><code>{{defaultEmpty .TLSMITM.EffectiveAllowlist "(none — vacuously true)"}}</code></div>
          <div class="k">Cert cache</div>
          <div class="v"><code>{{.TLSMITM.CertCache.Size}} / {{.TLSMITM.CertCache.Capacity}}</code></div>
          <div class="k">Last cert issued</div>
          <div class="v">{{if .TLSMITM.LastIssued}}<code>{{.TLSMITM.LastIssued.Host}}</code> @ <span class="time mono">{{unixTime .TLSMITM.LastIssued.AtUnixTime}}</span>{{else}}<span class="muted">(none yet)</span>{{end}}</div>
          <div class="k">Cert hit rate (60s)</div>
          <div class="v"><code>{{hitRatePct .TLSMITM.HitRate60sPercent .TLSMITM.HitRate60sObserved}}</code></div>
        </div>
        {{end}}

        <h3 style="font-size:var(--xs);letter-spacing:.1em;text-transform:uppercase;color:var(--ink-4);margin:32px 0 8px">Process</h3>
        <div class="kv">
          <div class="k">Version</div>
          <div class="v"><code>{{.Process.Version}}</code></div>
          <div class="k">Started</div>
          <div class="v"><span class="time mono">{{unixTime .Process.StartedUnixTime}}</span> &middot; uptime {{durationOf .Process.UptimeSeconds}}</div>
          <div class="k">Build</div>
          <div class="v"><code>{{.Process.VCSRevision}}</code></div>
          <div class="k">Go version</div>
          <div class="v"><code>{{.Process.GoVersion}}</code></div>
          <div class="k">Cache directory</div>
          <div class="v"><code>{{.Cache.Dir}}</code></div>
        </div>
      </section>

    </div></div>

  <footer>
    <a href="/metrics">/metrics</a>
    <span class="sep">&middot;</span>
    <a href="/healthz">/healthz</a>
    <span class="sep">&middot;</span>
    <span>Times in UTC, rewritten to browser-local on load. Page auto-refreshes every 60s.</span>
  </footer>
</main>

<script>
// Theme toggle (paint icon + click swap + localStorage persist)
(function(){var r=document.documentElement,b=document.getElementById('theme-toggle'),i=document.getElementById('theme-icon');if(!b||!i)return;
function paint(){var m=r.getAttribute('data-theme');if(!m||m==='auto'){m=window.matchMedia('(prefers-color-scheme: dark)').matches?'dark':'light';}i.firstElementChild.setAttribute('href',m==='dark'?'#i-sun':'#i-moon');}
paint();
b.addEventListener('click',function(){var c=r.getAttribute('data-theme');if(!c||c==='auto'){c=window.matchMedia('(prefers-color-scheme: dark)').matches?'dark':'light';}var n=c==='dark'?'light':'dark';r.setAttribute('data-theme',n);try{localStorage.setItem('acu-theme',n);}catch(e){}paint();});})();

// Verdict pill (computed from per-cell data-state)
(function(){var pill=document.getElementById('verdict-pill'),lbl=document.getElementById('verdict-label');if(!pill||!lbl)return;
var states=[].slice.call(document.querySelectorAll('[data-state]')).map(function(el){return el.getAttribute('data-state');});
var keys=document.querySelector('.keys-chip');var keyringCrit=keys&&keys.getAttribute('data-state')==='crit';
var verdict='ok',cls='pill--healthy',label='HEALTHY';
if(states.indexOf('crit')!==-1||keyringCrit){verdict='crit';cls='pill--degraded';label='DEGRADED';}
else if(states.indexOf('warn')!==-1){verdict='warn';cls='pill--watching';label='WATCHING';}
else{var b=document.body,up=parseInt(b.getAttribute('data-uptime-seconds')||'0',10),gr=parseInt(b.getAttribute('data-gc-runs')||'0',10);if(up<300&&gr===0){verdict='stale';cls='pill--stale';label='WARMING UP';}}
pill.classList.remove('pill--healthy','pill--watching','pill--degraded','pill--stale');pill.classList.add(cls);pill.setAttribute('data-state',verdict==='ok'?'ok':(verdict==='warn'?'warn':(verdict==='crit'?'crit':'stale')));lbl.textContent=label;})();

// Aggregate-failure notice for Recent Adoptions (≥10% non-success).
// All dynamic strings (top, topN, hint text) are inserted via
// textContent / createElement to avoid DOM-XSS if the data-outcome
// enum ever carries an unexpected value.
(function(){var mount=document.getElementById('adoptions-notice');if(!mount)return;
var rows=document.querySelectorAll('#adoptions tr[data-outcome]');var total=rows.length;if(total===0)return;
var counts={};rows.forEach(function(tr){var o=tr.getAttribute('data-outcome');if(o==='success'||!o)return;counts[o]=(counts[o]||0)+1;});
var top='',topN=0;for(var k in counts){if(counts[k]>topN){top=k;topN=counts[k];}}
var THRESHOLD=0.10;if(top===''||(topN/total)<THRESHOLD)return;
var keyringPanel=document.getElementById('keyring');
var acceptAnySigner=keyringPanel&&keyringPanel.getAttribute('data-accept-any-signer')==='true';
var gpgFailedText=acceptAnySigner
?"Likely cause: with accept_any_signer = true, gpg_failed typically indicates a structural decode failure (corrupt clearsign envelope or Release.gpg) or a pinned-suite trust mismatch. Cross-check the Keyring section."
:"Likely cause: upstream repository key changed or the matching archive key isn't loaded. Cross-check the Keyring section.";
var hints={
'gpg_failed':{text:gpgFailedText,linkHref:'#keyring',linkLabel:'Trusted keys',linkArrow:'→ Keyring'},
'parse_failed':{text:'Likely cause: malformed Release / Sources / Packages payload from upstream. Capture a failing fetch and inspect.'},
'member_mismatch':{text:'Likely cause: a Release-listed member hash diverged from the cached blob. Inspect the failing index path in the proxy logs.'},
'unpinned_suite':{text:"Likely cause: this suite is not allow-listed for adoption. Add it to the operator's adoption pin list to enable verification."},
'run_failed':{text:'Likely cause: upstream unreachable, TLS failure, rate-limiting, or another transport-level error. Check the proxy logs for the upstream host.'}
};
var h=hints[top]||{text:'See per-row details below.'};
var note=document.createElement('div');note.className='notice';note.setAttribute('role','alert');
var head=document.createElement('div');head.className='notice__head';
var dot=document.createElement('span');dot.className='dot';head.appendChild(dot);
var headText=document.createElement('span');headText.appendChild(document.createTextNode(topN+' of '+total+' recent adoptions failed: '));
var headCode=document.createElement('code');headCode.style.cssText='background:transparent;padding:0;color:var(--crit)';headCode.textContent=top;headText.appendChild(headCode);
head.appendChild(headText);note.appendChild(head);
var body=document.createElement('div');body.className='notice__body';body.appendChild(document.createTextNode(h.text));
if(h.linkHref){var row=document.createElement('span');row.className='notice__link-row';row.appendChild(document.createTextNode(h.linkLabel+' '));var a=document.createElement('a');a.href=h.linkHref;a.textContent=h.linkArrow;row.appendChild(a);body.appendChild(row);}
note.appendChild(body);mount.appendChild(note);})();

// Sticky-rail active-section highlight
(function(){var rail=document.querySelector('.rail');if(!rail||!('IntersectionObserver' in window))return;
var links={};rail.querySelectorAll('a[href^="#"]').forEach(function(a){links[a.getAttribute('href').slice(1)]=a;});
var io=new IntersectionObserver(function(es){es.forEach(function(e){var id=e.target.id;if(!links[id])return;if(e.isIntersecting){Object.keys(links).forEach(function(k){links[k].removeAttribute('aria-current');});links[id].setAttribute('aria-current','location');}});},{rootMargin:'-72px 0px -65% 0px'});
document.querySelectorAll('section.panel').forEach(function(s){io.observe(s);});})();

// Help-popover dismiss (click outside / Escape)
(function(){document.addEventListener('click',function(e){document.querySelectorAll('details.col-hint[open]').forEach(function(d){if(!d.contains(e.target))d.open=false;});});document.addEventListener('keydown',function(e){if(e.key==='Escape')document.querySelectorAll('details.col-hint[open]').forEach(function(d){d.open=false;});});})();

// Localize <time data-unix=N> textContent to browser-local time.
(function(){var tz;try{tz=Intl.DateTimeFormat().resolvedOptions().timeZone;}catch(e){}if(!tz)return;
var pad=function(n){return n<10?'0'+n:''+n;};var tzS='';try{var p=new Intl.DateTimeFormat(undefined,{timeZoneName:'short'}).formatToParts(new Date());for(var i=0;i<p.length;i++){if(p[i].type==='timeZoneName'){tzS=p[i].value;break;}}}catch(e){}
var ns=document.querySelectorAll('time[data-unix]');for(var i=0;i<ns.length;i++){var u=parseInt(ns[i].getAttribute('data-unix'),10);if(!isFinite(u))continue;var d=new Date(u*1000);var s=d.getFullYear()+'-'+pad(d.getMonth()+1)+'-'+pad(d.getDate())+' '+pad(d.getHours())+':'+pad(d.getMinutes())+':'+pad(d.getSeconds());ns[i].textContent=tzS?(s+' '+tzS):s;}})();
</script>
</body>
</html>
`

// formatUnixTime renders a unix-seconds timestamp as YYYY-MM-DD HH:MM:SS.
func formatUnixTime(unix int64) string {
	if unix == 0 {
		return ""
	}
	return time.Unix(unix, 0).UTC().Format("2006-01-02 15:04:05")
}

// formatUnixTimePtr renders a *int64 timestamp; nil → "-".
func formatUnixTimePtr(unix *int64) string {
	if unix == nil {
		return "-"
	}
	return formatUnixTime(*unix)
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

// durationOf renders a wallclock-seconds count as "Xh Ym".
func durationOf(seconds int64) string {
	d := time.Duration(seconds) * time.Second
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
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
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
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
		if m.Cache.ZeroRefcountBacklog > 1000 {
			return "warn"
		}
		return "ok"
	case "suites":
		if len(m.Suites) == 0 {
			return "stale"
		}
		worstLagSeconds := int64(0)
		anyLag := false
		for _, s := range m.Suites {
			if s.LastSuccessUnixTime == nil || s.InReleaseChangeSeenAtUnixTime == nil {
				continue
			}
			if *s.InReleaseChangeSeenAtUnixTime > *s.LastSuccessUnixTime {
				anyLag = true
				lag := *s.InReleaseChangeSeenAtUnixTime - *s.LastSuccessUnixTime
				if lag > worstLagSeconds {
					worstLagSeconds = lag
				}
			}
		}
		if worstLagSeconds > 24*3600 {
			return "crit"
		}
		if anyLag {
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
		fail := 0
		for _, a := range m.RecentAdoptions {
			if a.Outcome != "success" {
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
//	"unpinned_suite", "run_failed".
//
// All non-success outcomes are critical for the operator (the row's
// adoption did not land); they map to b--crit. Unknown outcomes — e.g.
// a future enum value the classifier adds before this helper is updated
// — map to b--crit too rather than the previous b--stale, since
// "future failure mode we don't recognise" is closer to crit than to
// stale. Existing soft-state values ("lagging", "warn") keep their
// b--warn mapping for non-adoption use sites.
func outcomeBadgeClass(outcome string) string {
	switch outcome {
	case "success":
		return "b--ok"
	case "lagging", "warn":
		return "b--warn"
	case "":
		return "b--stale"
	default:
		return "b--crit"
	}
}
