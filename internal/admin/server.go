// Package admin implements the SPEC5 Phase 5 admin HTTP listener:
// /metrics (Prometheus exposition), / (status page; HTML or JSON),
// /healthz (liveness probe). The listener is read-only — no
// mutating endpoints in Phase 5. Optional HTTP Basic auth via
// htpasswd file.
//
// The package is constructed once by cmd/apt-cacher-ultra/main.go
// and bound between the proxy/TLS listeners and cache.Open
// (SPEC5 §9.7.1). Graceful shutdown runs Server.Shutdown FIRST
// (SPEC5 §9.5) so a scrape mid-shutdown sees Connection: close
// rather than mid-write race against a closing DB.
//
// AIDEV-NOTE: the admin listener cannot import main, so cmd
// passes a BuildInfo value type into New(). Likewise, cache and gc
// packages are imported here only for their public APIs
// (LastRunSummary, ListSuitesWithAdoption, hostsem.Snapshot, etc.).
// No reverse imports.
package admin

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/gc"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
	"github.com/linsomniac/apt-cacher-ultra/internal/observability"
)

// BuildInfo carries the version information that cmd reads from
// main.Version (Makefile-injected via -ldflags) and
// runtime/debug.ReadBuildInfo(). The internal/admin package cannot
// import main directly (Go's internal/ rule), so cmd composes this
// struct at startup and passes it to New(). SPEC5 §10.4.7.
type BuildInfo struct {
	Version     string // "v0.x.y"; empty if not set by ldflags
	GoVersion   string // "go1.22.1"; from debug.ReadBuildInfo()
	VCSRevision string // commit SHA short or full; from debug.ReadBuildInfo()
}

// Config wires the admin listener to its dependencies. All fields
// are required; New returns an error on any nil dependency.
type Config struct {
	Cache       *cache.Cache
	GC          *gc.GC
	HostLimiter *hostsem.Sem
	Ring        *observability.Ring
	Registry    *metrics.Registry
	Logger      *slog.Logger
	BuildInfo   BuildInfo
	Admin       config.AdminConfig
	StartTime   time.Time

	// ProxyAddr / TLSAddr / AdminAddr surface the listener
	// addresses for the status page's "listeners" section. cmd
	// passes whatever it bound; admin does not bind these itself.
	ProxyAddr string
	TLSAddr   string // "" if TLS not configured
	AdminAddr string

	// TLSMITM provides the SPEC6 §10.4 status-page TLS MITM
	// snapshot. nil is treated as MITM disabled — the status page
	// emits the abbreviated `{"enabled": false}` JSON form and
	// skips the HTML section. Lives behind an interface so admin
	// does not import internal/proxy or internal/proxy/tlsmitm.
	TLSMITM TLSMITMProvider

	// AdoptionArchitectures echoes the operator's
	// [adoption].architectures setting onto the SPEC6_5 §2.4
	// repo_coverage.architectures_filter field. nil / empty slice
	// renders as `[]` in JSON. cmd/main passes whatever was loaded
	// from the config file; admin does not re-read the value.
	AdoptionArchitectures []string
}

// TLSMITMProvider supplies the SPEC6 §10.4 status-page TLS MITM
// snapshot. cmd/apt-cacher-ultra constructs an implementation that
// closes over the materialized CA + leaf cache + observability state.
type TLSMITMProvider interface {
	TLSMITMSnapshot() TLSMITMSnapshot
}

// TLSMITMSnapshot is the SPEC6 §10.4 status-page payload. When
// Enabled is false, every other field is ignored and the abbreviated
// `{"enabled": false}` form is rendered.
type TLSMITMSnapshot struct {
	Enabled             bool
	CASource            string // "generated" or "supplied"
	CAFingerprintSHA256 string
	CANotAfterUnixTime  int64
	EffectiveAllowlist  string // regex string, or "" for no narrowing
	CertCacheSize       int
	CertCacheCapacity   int
	LastIssuedHost      string    // empty when no issuance recorded
	LastIssuedAt        time.Time // zero when no issuance recorded
	HitRate60sHits      int       // raw counts so the renderer can decide on n/a vs 0%
	HitRate60sMisses    int
}

// Server is the SPEC5 §9.7 admin HTTP server. Owns the *http.Server,
// the auth middleware (with mtime+size driven htpasswd reload), and
// the refresher goroutine (SPEC5 §9.7.6) that recomputes expensive
// gauges. Construct via New; start with Serve; stop with Shutdown.
type Server struct {
	cfg    Config
	server *http.Server
	logger *slog.Logger

	// shuttingDown is read by the /healthz handler so a probe sees
	// 503 once SIGINT/SIGTERM has begun the graceful sequence
	// (SPEC5 §9.7.4 check 3). atomic.Bool because the read path
	// must not contend with shutdown's lock.
	shuttingDown atomic.Bool

	// auth is the optional htpasswd middleware. nil when
	// admin.htpasswd_file is empty — middleware short-circuits to
	// the bare handler in that case.
	auth *htpasswdAuthenticator

	// refresher coordinates the §9.7.6 refresher goroutine. Closed
	// on Shutdown. refresherCancel cancels the context that
	// in-flight queries inside runRefreshOnce inherit, so a slow
	// query unblocks promptly when shutdown begins.
	refresherStop   chan struct{}
	refresherDone   chan struct{}
	refresherCancel context.CancelFunc

	// poolScanInProgress is the §9.7.6 "refresh in progress" guard
	// for the du-style pool/ walk. The walk runs in its own
	// goroutine (so a slow filesystem does not block the cheap
	// gauges from updating); CAS=false means a walk goroutine is
	// already running and this tick skips spawning a new one.
	// Tracked by walkWg so Shutdown waits for the walk to drain.
	poolScanInProgress atomic.Bool
	walkWg             sync.WaitGroup

	// gauges owns every refresher-recomputed metric. Set once in
	// New(); the refresher goroutine reads/writes these without
	// holding s.mu (the metrics package handles its own locking).
	gauges *refresherGauges

	// startup holds the §10.4.7 build_info / process_start gauges.
	// Set once in New() and never mutated thereafter.
	startup *startupGauges

	// proc holds the §10.4.7 Prometheus-standard process collector
	// metrics (process_cpu_seconds_total etc.). Refreshed on the
	// same cadence as the refresher gauges; values stale by at
	// most admin.gauge_refresh.
	proc *processGauges

	// self holds the §10.4.8 admin-listener self-metrics
	// (scrape/status/healthz/auth_failures). Emitted at the
	// corresponding handler entry points.
	self *selfMetrics

	// repoCoverage caches the SPEC6_5 §2.4 repo_coverage payload
	// across status-page renders. The §9.7.6 refresher recomputes the
	// value on each tick and Store()s it here; the renderer reads via
	// Load() without touching the DB. nil before the first refresh
	// completes — renderer treats nil as the zero-value RepoCoverage
	// (architectures_seen: [], counts: 0).
	//
	// AIDEV-NOTE: the migration from live-query to cached path means
	// the JSON value can be stale by up to admin.gauge_refresh
	// (default 30s). Status consumers that need real-time row counts
	// run their own query; the JSON contract documents this latency.
	repoCoverage atomic.Pointer[cache.RepoCoverage]

	// cacheSummaryByHostArch caches the SPEC6_5 §2.4 per-(host, arch)
	// summary the renderer surfaces under cache_summary.by_host[*].
	// Same refresh cadence + stale tolerance as repoCoverage.
	cacheSummaryByHostArch atomic.Pointer[map[string]map[string]cache.CacheSummaryEntry]

	// mu guards refresherStop / refresherDone / refresherCancel —
	// Shutdown must be idempotent, and the refresher goroutine
	// must not be started twice.
	mu sync.Mutex
}

// New validates Config and constructs a Server. Returns an error
// when a required dependency is nil or the htpasswd file (when
// configured) fails to parse — the same parse the config-validate
// path runs at load time, repeated here because the file may have
// been replaced between Validate and New.
func New(cfg Config) (*Server, error) {
	if cfg.Cache == nil {
		return nil, errors.New("admin: nil Cache")
	}
	if cfg.GC == nil {
		return nil, errors.New("admin: nil GC")
	}
	if cfg.HostLimiter == nil {
		return nil, errors.New("admin: nil HostLimiter")
	}
	if cfg.Ring == nil {
		return nil, errors.New("admin: nil Ring")
	}
	if cfg.Registry == nil {
		return nil, errors.New("admin: nil Registry")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.AdminAddr == "" {
		return nil, errors.New("admin: AdminAddr is required")
	}

	s := &Server{
		cfg:    cfg,
		logger: logger,
	}
	s.gauges = newRefresherGauges(cfg.Registry, cfg.Admin.MetricSeriesCap)
	s.startup = newStartupGauges(cfg.Registry, cfg.Admin.MetricSeriesCap,
		cfg.BuildInfo, cfg.StartTime)
	s.proc = newProcessGauges(cfg.Registry)
	s.self = newSelfMetrics(cfg.Registry, cfg.Admin.MetricSeriesCap)

	if cfg.Admin.HtpasswdFile != "" {
		auth, err := newHtpasswdAuthenticator(cfg.Admin.HtpasswdFile, logger)
		if err != nil {
			return nil, err
		}
		s.auth = auth
	}

	s.server = &http.Server{
		Addr:              cfg.AdminAddr,
		Handler:           s.buildHandler(),
		ReadHeaderTimeout: cfg.Admin.ReadTimeout.Duration,
		IdleTimeout:       cfg.Admin.IdleTimeout.Duration,
	}
	return s, nil
}

// allowMethods is the SPEC5 §9.7.1 Allow header for the read-only
// admin endpoints. Used in 405 responses on known paths and 204
// responses on OPTIONS. Centralized here so a typo cannot drift
// the two sites apart.
const allowMethods = "GET, HEAD, OPTIONS"

// buildHandler constructs the route dispatcher per SPEC5 §9.7.1:
//   - OPTIONS on any path → 204 with Allow header (no body).
//   - GET/HEAD on /metrics, /healthz, / → the corresponding handler.
//   - Any other method on /metrics, /healthz, / → 405 with Allow.
//   - Any other path → 404.
//
// Go 1.22+ enhanced patterns auto-emit 405 only when *another*
// method is registered for the same path, and the auto-Allow header
// only lists explicitly-registered methods (so `GET /metrics`
// produces `Allow: GET`, not the spec's `GET, HEAD, OPTIONS`).
// Hand-rolling the dispatch is shorter than registering 3×3 method
// patterns and gives one site to update if the Allow set changes.
//
// The auth middleware (when configured) wraps the dispatcher; the
// request-log middleware wraps both.
func (s *Server) buildHandler() http.Handler {
	routes := map[string]http.HandlerFunc{
		"/metrics": s.handleMetrics,
		"/healthz": s.handleHealthz,
		"/":        s.handleStatus,
	}

	dispatch := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.Header().Set("Allow", allowMethods)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h, known := routes[r.URL.Path]
		if !known {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", allowMethods)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h(w, r)
	})

	var h http.Handler = dispatch
	if s.auth != nil {
		h = s.auth.middleware(h, func(reason string) {
			s.self.authFailuresTotal.Inc(reason)
		})
	}
	return s.requestLogMiddleware(h)
}

// Serve blocks running the admin listener on the given net.Listener.
// Returns nil on graceful shutdown (http.ErrServerClosed); any
// other listener error is returned. cmd is responsible for bind
// (net.Listen); Serve owns the Accept loop.
//
// SPEC5 §3.2 / §9.7.6: the first gauge refresh runs synchronously
// BEFORE the HTTP server begins accepting requests, so the very
// first /metrics scrape sees populated values rather than the
// zero-state of every gauge. The pool walk is exempt: it spawns a
// goroutine even on the first call (a multi-GiB cache can take
// seconds, and SPEC5 §9.7.6 explicitly handles overrun async).
func (s *Server) Serve(ln net.Listener) error {
	s.runRefreshOnce(context.Background())
	s.startRefresher()
	if err := s.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown stops the listener gracefully and the refresher
// goroutine. Idempotent; safe to call multiple times.
//
// SPEC5 §9.5 / §9.7.4: Shutdown sets shuttingDown so /healthz
// returns 503 with X-Acu-Check-Failed: shutdown for any in-flight
// or freshly-arriving probe. The HTTP server then drains in-flight
// scrapes within ctx's deadline.
func (s *Server) Shutdown(ctx context.Context) error {
	s.shuttingDown.Store(true)

	s.mu.Lock()
	if s.refresherCancel != nil {
		s.refresherCancel()
	}
	if s.refresherStop != nil {
		select {
		case <-s.refresherStop:
			// already closed
		default:
			close(s.refresherStop)
		}
	}
	doneCh := s.refresherDone
	s.mu.Unlock()

	if err := s.server.Shutdown(ctx); err != nil {
		return err
	}
	if doneCh != nil {
		select {
		case <-doneCh:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// Wait for any in-flight pool walk goroutine to drain. The walk
	// inherits lifecycleCtx (cancelled above) so filepath.Walk
	// returns early; this just synchronizes the goroutine's exit
	// with Shutdown's return.
	walkDone := make(chan struct{})
	go func() {
		s.walkWg.Wait()
		close(walkDone)
	}()
	select {
	case <-walkDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// reqState is the per-request mutable scratchpad the request-log
// middleware seeds and the auth middleware (when present) updates.
// It is stored in the request context as a pointer so a downstream
// mutation is visible to the outer logger after ServeHTTP returns —
// auth's `r.WithContext(ctx)` swap is invisible at the outer scope,
// but a pointed-at struct's fields survive any context replacement.
//
// AIDEV-NOTE: SPEC5 §10.1 requires admin_request to carry the
// authenticated user (when htpasswd is configured) AND a per-request
// scrape_id for correlating with the §10.4.8 self-metrics. Both are
// emitted from one log site — keep this struct narrow.
type reqState struct {
	authUser string
	scrapeID uint64
}

type reqStateKey struct{}

// requestLogMiddleware emits the SPEC5 §10.1 admin_request log line
// per request and updates the §10.4.8 self-metrics. Wraps the auth
// middleware so the log sees auth_user only when the request was
// actually authorized.
func (s *Server) requestLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		cw := &countingWriter{ResponseWriter: w}
		state := &reqState{scrapeID: rand.Uint64()}
		ctx := context.WithValue(r.Context(), reqStateKey{}, state)
		next.ServeHTTP(cw, r.WithContext(ctx))
		s.logger.Info("admin_request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", cw.statusCode(),
			"bytes", cw.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
			"auth_user", state.authUser,
			"scrape_id", state.scrapeID,
		)
	})
}

// countingWriter wraps http.ResponseWriter to capture status and
// byte count for the per-request log line. Mirrors the proxy
// listener's countingWriter pattern (handler/handler.go) — kept
// independent so the admin package does not import handler.
type countingWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (c *countingWriter) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	n, err := c.ResponseWriter.Write(p)
	c.bytes += int64(n)
	return n, err
}

func (c *countingWriter) statusCode() int {
	if c.status == 0 {
		return http.StatusOK
	}
	return c.status
}

// resolveHostPort splits a `host:port` listener address into the
// host portion (or empty for `:port`-style binds) and the
// numeric port. Used by the non-loopback warning path.
func resolveHostPort(addr string) (host string, port int, err error) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, err
	}
	pn, err := strconv.Atoi(p)
	if err != nil {
		return "", 0, err
	}
	return h, pn, nil
}

// UserCount returns the number of htpasswd users the auth
// middleware currently authenticates against, or 0 when auth is
// disabled. cmd uses this to populate user_count on the
// admin_authenticated Info line AFTER admin.New has succeeded —
// emitting before admin.New could log "authenticated" against a
// file that subsequently fails to parse (a sub-second TOCTOU
// window between the startup config-dump count and admin.New).
func (s *Server) UserCount() int {
	if s.auth == nil {
		return 0
	}
	return s.auth.userCount()
}

// IsNonLoopback reports whether addr binds to anything other than
// 127.0.0.1, ::1, or localhost. Used by cmd to decide whether to
// fire the SPEC5 §5.2 admin_unauthenticated_non_loopback warning.
// Empty host (i.e. ":6789") is treated as non-loopback because the
// listener accepts on every interface.
func IsNonLoopback(addr string) bool {
	h, _, err := resolveHostPort(addr)
	if err != nil {
		return true // err on the side of warning
	}
	switch strings.ToLower(h) {
	case "", "127.0.0.1", "::1", "localhost":
		return strings.ToLower(h) == "" // empty host = all-interfaces
	default:
		return true
	}
}
