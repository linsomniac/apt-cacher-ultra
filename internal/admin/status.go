package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
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
	Listeners       []listenerInfo   `json:"listeners"`
	Suites          []suiteEntry     `json:"suites"`
	GC              *gcInfo          `json:"gc"`
	HotURLPaths     []hotURLEntry    `json:"hot_url_paths"`
	RecentAdoptions []adoptionEntry  `json:"recent_adoptions"`
	ActiveHosts     []activeHostInfo `json:"active_hosts"`
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
	Host              string  `json:"host"`
	SuitePath         string  `json:"suite_path"`
	Outcome           string  `json:"outcome"`
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
		Listeners:       buildListenerInfo(s.cfg),
		Suites:          buildSuiteEntries(suitesRaw.([]cache.SuiteWithAdoption)),
		HotURLPaths:     buildHotURLEntries(hotPaths.([]cache.HotURLPath)),
		RecentAdoptions: buildAdoptionEntries(s.cfg.Ring.Snapshot()),
		ActiveHosts:     buildActiveHostEntries(s.cfg.HostLimiter.Snapshot()),
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
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(model); err != nil {
			s.logger.Warn("admin_status_render_failed",
				"err", err.Error(),
				"format", "json",
				"query", "json.Encode")
		}
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := statusHTMLTemplate.Execute(w, model); err != nil {
		s.logger.Warn("admin_status_render_failed",
			"err", err.Error(),
			"format", "html",
			"query", "template.Execute")
	}
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
var statusHTMLTemplate = template.Must(template.New("status").Funcs(template.FuncMap{
	"unixTime":    formatUnixTime,
	"unixTimePtr": formatUnixTimePtr,
	"formatBytes": formatBytes,
	"durationOf":  durationOf,
	"i64Ptr":      formatInt64Ptr,
}).Parse(statusHTML))

const statusHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>apt-cacher-ultra status</title>
<style>
body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
  margin: 1.5em; color: #1a1a1a; }
h1 { font-size: 1.4em; margin-bottom: 0.3em; }
h2 { font-size: 1.1em; margin-top: 2em; border-bottom: 1px solid #ccc;
  padding-bottom: 0.2em; }
table { border-collapse: collapse; margin-top: 0.5em; }
th, td { padding: 4px 12px; text-align: left;
  border-bottom: 1px solid #eee; vertical-align: top; }
th { background: #f6f6f6; }
.muted { color: #888; }
.json-link { float: right; font-size: 0.9em; }
code { background: #f3f3f3; padding: 1px 4px; border-radius: 3px; }
</style>
</head>
<body>
<h1>apt-cacher-ultra <span class="muted">{{.Process.Version}}</span>
<a class="json-link" href="/?format=json">View as JSON →</a></h1>

<p class="muted">
  Started {{unixTime .Process.StartedUnixTime}} UTC,
  uptime {{durationOf .Process.UptimeSeconds}}.
  Build {{.Process.VCSRevision}} ({{.Process.GoVersion}}).
</p>

<h2>Listeners</h2>
<table>
<tr><th>Role</th><th>Address</th></tr>
{{range .Listeners}}<tr><td>{{.Role}}</td><td><code>{{.Addr}}</code></td></tr>
{{end}}
</table>

<h2>Cache</h2>
<table>
<tr><td>Directory</td><td><code>{{.Cache.Dir}}</code></td></tr>
<tr><td>Blob count</td><td>{{.Cache.BlobCount}}</td></tr>
<tr><td>URL paths</td><td>{{.Cache.URLPathCount}}</td></tr>
<tr><td>Bytes used</td><td>{{formatBytes .Cache.BytesUsed}}</td></tr>
<tr><td>Zero-refcount backlog</td><td>{{.Cache.ZeroRefcountBacklog}}</td></tr>
</table>

<h2>Suites</h2>
{{if .Suites}}
<table>
<tr><th>Host</th><th>Suite path</th><th>Last check</th><th>Last success</th>
    <th>Current snapshot</th><th>Adopted at</th><th>Lagging</th></tr>
{{range .Suites}}<tr>
  <td><code>{{.Host}}</code></td>
  <td><code>{{.SuitePath}}</code></td>
  <td>{{unixTimePtr .LastCheckUnixTime}}</td>
  <td>{{unixTimePtr .LastSuccessUnixTime}}</td>
  <td>{{i64Ptr .CurrentSnapshotID}}</td>
  <td>{{unixTimePtr .CurrentSnapshotAdoptedAtUnixTime}}</td>
  <td>{{unixTimePtr .InReleaseChangeSeenAtUnixTime}}{{if .Lagging}} <span class="muted">{{.Lagging}}</span>{{end}}</td>
</tr>
{{end}}</table>
{{else}}<p class="muted">No suites tracked yet.</p>{{end}}

<h2>Garbage collection</h2>
{{if .GC.LastRunUnixTime}}
<table>
<tr><td>Last run</td><td>{{unixTimePtr .GC.LastRunUnixTime}} UTC ({{.GC.LastRunPhase}})</td></tr>
<tr><td>Duration</td><td>{{.GC.LastRunDurationSeconds}}s</td></tr>
<tr><td>Blobs reaped</td><td>{{.GC.LastRunBlobsReaped}}</td></tr>
<tr><td>Bytes reclaimed</td><td>{{formatBytes .GC.LastRunBytesReclaimed}}</td></tr>
<tr><td>Orphan candidates reaped</td><td>{{.GC.OrphanCandidatesReaped}}</td></tr>
<tr><td>Displaced reaped</td><td>{{.GC.DisplacedReaped}}</td></tr>
<tr><td>Pool orphans repaired</td><td>{{.GC.PoolOrphansRepaired}}
  ({{formatBytes .GC.PoolOrphanBytesRepaired}})</td></tr>
<tr><td>Pool unlink errors</td><td>{{.GC.PoolUnlinkErrors}}</td></tr>
<tr><td>Deadline reached</td><td>{{.GC.LastRunDeadlineReached}}</td></tr>
</table>
{{else}}<p class="muted">GC has not run yet.</p>{{end}}

<h2>Active hosts</h2>
{{if .ActiveHosts}}
<table>
<tr><th>Host</th><th>Inflight</th><th>Slot capacity</th></tr>
{{range .ActiveHosts}}<tr>
  <td><code>{{.Host}}</code></td>
  <td>{{.Inflight}}</td>
  <td>{{.SlotCapacity}}</td>
</tr>
{{end}}</table>
{{else}}<p class="muted">No hosts have held a slot since process start.</p>{{end}}

<h2>Hot URL paths</h2>
{{if .HotURLPaths}}
<table>
<tr><th>Host</th><th>Path</th><th>Metadata?</th><th>Requests</th>
    <th>Last requested</th></tr>
{{range .HotURLPaths}}<tr>
  <td><code>{{.Host}}</code></td>
  <td><code>{{.Path}}</code></td>
  <td>{{.IsMetadata}}</td>
  <td>{{.RequestCount}}</td>
  <td>{{unixTime .LastRequestedUnixTime}}</td>
</tr>
{{end}}</table>
{{else}}<p class="muted">No URL paths requested yet.</p>{{end}}

<h2>Recent adoptions</h2>
{{if .RecentAdoptions}}
<table>
<tr><th>Host</th><th>Suite path</th><th>Outcome</th>
    <th>Completed</th><th>Duration</th></tr>
{{range .RecentAdoptions}}<tr>
  <td><code>{{.Host}}</code></td>
  <td><code>{{.SuitePath}}</code></td>
  <td>{{.Outcome}}</td>
  <td>{{unixTime .CompletedUnixTime}}</td>
  <td>{{.DurationSeconds}}s</td>
</tr>
{{end}}</table>
{{else if lt .Process.UptimeSeconds 300}}<p class="muted">(empty since last process start)</p>{{end}}

<p><a href="/metrics">/metrics</a> &middot; <a href="/healthz">/healthz</a></p>
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
