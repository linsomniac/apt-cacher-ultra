// Command apt-cacher-ultra is a robust apt repository cache. See SPEC.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/admin"
	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
	"github.com/linsomniac/apt-cacher-ultra/internal/gc"
	"github.com/linsomniac/apt-cacher-ultra/internal/gpg"
	"github.com/linsomniac/apt-cacher-ultra/internal/handler"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/integrity"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
	"github.com/linsomniac/apt-cacher-ultra/internal/observability"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy/tlsmitm"

	"crypto/tls"
)

// Version is set at build time via -ldflags.
var Version = "dev"

// Tunables for HTTP timeouts and shutdown. Exported as vars so tests can
// shorten them; production runs at the defaults.
var (
	// readHeaderTimeout caps how long we wait on a slow client's headers.
	// Go's default is unlimited, which is a slowloris vulnerability.
	readHeaderTimeout = 10 * time.Second

	// idleTimeout caps how long an inbound keep-alive connection can sit
	// idle between requests. With a public listener (default 0.0.0.0:3142)
	// this prevents an unauthenticated client from holding sockets — and
	// thus file descriptors — open indefinitely. Apt re-establishes
	// connections cheaply, so 60s is generous without being abusable.
	idleTimeout = 60 * time.Second

	// shutdownTimeout is the SPEC §9.5 drain budget on SIGTERM.
	// SPEC6 §9.4 step 4.4 adds a bounded grace (≤ 1s — see
	// tunnelDrainGrace below) AFTER the budget for the tunnel
	// manager to drain post-force-close, so the worst-case wall
	// time from cancel to serveListeners returning is
	// shutdownTimeout + tunnelDrainGrace + a small constant for
	// non-MITM teardown (gauge refresher waits, GC drain, cache
	// close). A pathologically wedged tunnel goroutine could
	// extend shutdown further via the unbounded h.Close →
	// activeWG.Wait at the end of the sequence, but force-close
	// of the conn is expected to unblock that within
	// tunnelDrainGrace in practice; ErrDrainDeadline from Drain
	// is the operator signal that a tunnel goroutine is wedged
	// in something conn.Close cannot unblock (deadlocked channel
	// write, etc.).
	shutdownTimeout = 30 * time.Second

	// tunnelDrainGrace is the SPEC6 §9.4 step 4.4 grace window
	// for the tunnel-manager WG to drain AFTER force-close fires
	// at budget expiry. Bounded because conn.Close yields
	// immediately on any pending Read/Write — tunnels unwind in
	// tens of microseconds typically. 1s is a generous ceiling.
	tunnelDrainGrace = time.Second

	// tmpSweepMaxAge is the SPEC §4.2 staleness threshold for orphaned
	// partial downloads from previous crashes.
	tmpSweepMaxAge = 5 * time.Minute

	// keyringDirs is the SPEC2 §7.6.1 trusted-keyring search path.
	// Variable rather than const so tests can point it at a tempdir
	// without requiring root.
	keyringDirs = []string{gpg.DefaultTrustedGPGDir, gpg.DefaultKeyringsDir}

	// mitmUndistributedRefreshInterval is the SPEC6 §9.7.6 cadence
	// the `tls_mitm_enabled_ca_undistributed` Warn refresher wakes
	// at — once per uptime hour in production. Variable so tests
	// can shorten it.
	mitmUndistributedRefreshInterval = 1 * time.Hour

	// mitmCertCacheSizeRefreshInterval is the cadence of the §10.3
	// acu_mitm_cert_cache_size gauge refresher. 30s in production
	// since the gauge is observational only — no operator action
	// depends on sub-30s freshness. Variable so tests can shorten it.
	mitmCertCacheSizeRefreshInterval = 30 * time.Second
)

func main() {
	// SPEC6 §14.3 subcommand routing. `ca print` is a positional
	// pre-flag form (matches the established `program subcommand
	// args` shell idiom and avoids polluting the daemon's flag set).
	// Anything else falls through to standard flag parsing — which
	// recognizes the daemon's own flags AND the §14.2
	// `--print-apt-conf` toggle.
	if len(os.Args) >= 3 && os.Args[1] == "ca" && os.Args[2] == "print" {
		os.Exit(runCAPrint(os.Args[3:], os.Stdout, os.Stderr))
	}

	configPath := flag.String("config", "/etc/apt-cacher-ultra/config.toml", "path to TOML config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	printAptConf := flag.Bool("print-apt-conf", false, "print recommended /etc/apt/apt.conf.d snippet and exit (SPEC6 §14.2)")
	flag.Parse()

	if *showVersion {
		fmt.Println(Version)
		return
	}

	if *printAptConf {
		os.Exit(runPrintAptConf(*configPath, os.Stdout, os.Stderr))
	}

	if err := run(*configPath); err != nil {
		slog.Error("startup failed", "err", err)
		os.Exit(1)
	}
}

// run loads config, configures the default logger, installs SIGINT/SIGTERM
// handling, and hands off to serve. Factored thin so tests can drive serve
// directly without sending real signals.
func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger := newLogger(cfg.Log)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return serve(ctx, cfg, logger)
}

// serve wires every internal package into a running http.Server (plus an
// optional TLS server) and blocks until ctx is cancelled or a listener
// fails. On exit, SPEC §9.5 graceful shutdown is performed.
//
// The actual listening sockets are bound here (per cfg.Cache.Listen and
// cfg.Cache.ListenTLS) before being handed to serveListeners. Tests bypass
// this and call serveListeners directly with their own listeners — that is
// the only practical way to learn a `:0`-bound port without racing.
func serve(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	plainLn, err := net.Listen("tcp", cfg.Cache.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Cache.Listen, err)
	}

	var tlsLn net.Listener
	if cfg.Cache.ListenTLS != "" {
		tlsLn, err = net.Listen("tcp", cfg.Cache.ListenTLS)
		if err != nil {
			_ = plainLn.Close()
			return fmt.Errorf("listen %s: %w", cfg.Cache.ListenTLS, err)
		}
	}

	// SPEC5 §9.7.1: admin listener bound after the proxy/TLS
	// listeners and before cache.Open, so a bind failure (port in
	// use, permission denied) fails-fast before we touch the cache
	// directory. Skipped when admin.enabled = false.
	var adminLn net.Listener
	if cfg.Admin.Enabled {
		adminLn, err = net.Listen("tcp", cfg.Admin.Listen)
		if err != nil {
			_ = plainLn.Close()
			if tlsLn != nil {
				_ = tlsLn.Close()
			}
			return fmt.Errorf("listen %s: %w", cfg.Admin.Listen, err)
		}
	}

	return serveListeners(ctx, cfg, logger, plainLn, tlsLn, adminLn)
}

// serveListeners is the inner serve loop. It owns its listeners (closing
// them on shutdown) and runs until ctx is cancelled or a listener errors
// out. Both production (via serve) and tests construct listeners and hand
// them in.
//
// The wiring order matters: cache.Open before SweepTmp (SweepTmp reads
// cache.Dir from the open handle); fetch.New before handler.New (handler
// holds the fetch client). On any wiring failure, listeners passed in are
// closed before returning so the caller does not have to track them.
func serveListeners(
	ctx context.Context,
	cfg *config.Config,
	logger *slog.Logger,
	plainLn net.Listener,
	tlsLn net.Listener,
	adminLn net.Listener,
) (retErr error) {
	defer func() {
		// Defensive: if we error out before the goroutines start serving,
		// the listeners would otherwise leak — Serve() never runs to
		// observe the listener and close it. Closing twice is OK; net's
		// listeners no-op on a second close.
		if retErr != nil {
			_ = plainLn.Close()
			if tlsLn != nil {
				_ = tlsLn.Close()
			}
			if adminLn != nil {
				_ = adminLn.Close()
			}
		}
	}()

	// SPEC5 §10.2: count htpasswd users once up-front so both the
	// startup config dump and the admin_authenticated Info line carry
	// user_count. A parse failure here is fatal — refusing to start
	// matches the behavior admin.New would surface seconds later, and
	// failing fast keeps the operator's signal at one log line.
	htpasswdUsers := 0
	if cfg.Admin.Enabled && cfg.Admin.HtpasswdFile != "" {
		n, err := admin.CountHtpasswdUsers(cfg.Admin.HtpasswdFile)
		if err != nil {
			return fmt.Errorf("admin htpasswd %q: %w", cfg.Admin.HtpasswdFile, err)
		}
		htpasswdUsers = n
	}

	// SPEC §10 / SPEC2 §10.3: startup config dump. Capture every
	// operationally-relevant tunable so a single boot log line tells
	// the operator what policy they're running under (timeouts,
	// concurrency caps, allowlist, freshness cadence, Phase 2
	// adoption + integrity policy + trusted-signer block count).
	// Secrets do not appear in cfg, so the dump is safe to emit at
	// Info.
	logger.Info("apt-cacher-ultra starting",
		"version", Version,
		"listen", plainLn.Addr().String(),
		"listen_tls", tlsAddrString(tlsLn),
		"cache_dir", cfg.Cache.Dir,
		"upstream_connect_timeout", cfg.Upstream.ConnectTimeout.Duration,
		"upstream_total_timeout", cfg.Upstream.TotalTimeout.Duration,
		"upstream_idle_read_timeout", cfg.Upstream.IdleReadTimeout.Duration,
		"upstream_max_retries", cfg.Upstream.MaxRetries,
		"upstream_max_concurrent_per_host", cfg.Upstream.MaxConcurrentPerHost,
		"upstream_unreachable_cooldown", cfg.Upstream.UnreachableCooldown.Duration,
		"upstream_unreachable_probe_timeout", cfg.Upstream.UnreachableProbeTimeout.Duration,
		"upstream_allowed_host_regex", cfg.Upstream.AllowedHostRegex,
		"upstream_deny_target_ranges", cfg.Upstream.DenyTargetRanges,
		"freshness_cooldown", cfg.Freshness.Cooldown.Duration,
		"freshness_periodic_refresh", cfg.Freshness.PeriodicRefresh.Duration,
		"freshness_max_concurrent_adoptions", cfg.Freshness.MaxConcurrentAdoptions,
		"adoption_enabled", cfg.Adoption.Enabled,
		"adoption_require_signature", cfg.Adoption.RequireSignature,
		"adoption_require_pinned_signer", cfg.Adoption.RequirePinnedSigner,
		"adoption_hot_prefetch_budget", cfg.Adoption.HotPrefetchBudget.Duration,
		"hot_packages_window", cfg.HotPackages.Window.Duration,
		"integrity_validate_at_rest_interval", cfg.Integrity.ValidateAtRestInterval.Duration,
		"integrity_validate_at_rest_workers", cfg.Integrity.ValidateAtRestWorkers,
		"integrity_refuse_unvouched_debs", cfg.Integrity.RefuseUnvouchedDebs,
		"gc_enabled", cfg.GC.Enabled,
		"gc_interval", cfg.GC.Interval.Duration,
		"gc_batch_size", cfg.GC.BatchSize,
		"gc_snapshot_batch_size", cfg.GC.SnapshotBatchSize,
		"gc_max_tick_duration", cfg.GC.MaxTickDuration.Duration,
		"gc_blob_grace", cfg.GC.BlobGrace.Duration,
		"gc_keep_displaced", cfg.GC.KeepDisplaced,
		"gc_pool_scan_workers", cfg.GC.PoolScanWorkers,
		"gc_heartbeat_interval", cfg.GC.HeartbeatInterval.Duration,
		"gc_heartbeat_stale_grace_effective", cfg.HeartbeatStaleGraceEffective(),
		"admin_enabled", cfg.Admin.Enabled,
		"admin_listen", cfg.Admin.Listen,
		"admin_htpasswd_file", cfg.Admin.HtpasswdFile,
		"admin_htpasswd_users", htpasswdUsers,
		"admin_gauge_refresh", cfg.Admin.GaugeRefresh.Duration,
		"admin_read_timeout", cfg.Admin.ReadTimeout.Duration,
		"admin_idle_timeout", cfg.Admin.IdleTimeout.Duration,
		"admin_metric_series_cap", cfg.Admin.MetricSeriesCap,
		"trusted_signer_blocks", len(cfg.TrustedSigners),
		"serve_stale_when_upstream_down", cfg.Serve.ServeStaleWhenUpstreamDown,
		"log_format", cfg.Log.Format,
		"log_level", cfg.Log.Level,
	)

	// SPEC2 §5.1 documents require_signature = false as a loud
	// configuration: emit at WARN so the operator's choice is visible
	// in the journal even before adoption is actually wired.
	if !cfg.Adoption.RequireSignature {
		logger.Warn("adoption.require_signature = false: unsigned upstream metadata would be adopted (auditable per SPEC2 §5.1)")
	}

	// SPEC3 §5.2 loud configurations. Both fire once at startup so an
	// operator scanning the journal sees the configuration's actual
	// behavior — not just the defaults the spec mentions.
	if cfg.Adoption.HotPrefetchBudget.Duration == 0 {
		logger.Warn("hot_prefetch_budget_unbounded",
			"detail", "adoption.hot_prefetch_budget = 0: hot prefetch loop runs until every hot deb terminates; worst-case wait is N × upstream.total_timeout × upstream.max_retries")
	}
	if cfg.Integrity.RefuseUnvouchedDebs && !cfg.Adoption.Enabled {
		logger.Warn("refuse_unvouched_debs_inert",
			"detail", "integrity.refuse_unvouched_debs = true with adoption.enabled = false: strict mode predicate explicitly checks adoption.enabled and is therefore inert (SPEC3 §6.1, §10.2)")
	}

	// SPEC4 §10.2: gc_disabled Warn names the operator's choice when
	// the master switch is off. The cache still works, but disk usage
	// will grow unbounded as adoptions roll.
	if !cfg.GC.Enabled {
		logger.Warn("gc_disabled",
			"detail", "gc.enabled = false: orphan blobs and displaced snapshots will not be reaped; pool/ size grows unboundedly with rolling adoptions")
	}

	// SPEC5 §5.2 / §10.2: admin-listener startup events.
	if !cfg.Admin.Enabled {
		logger.Warn("admin_disabled",
			"detail", "admin.enabled = false: /metrics, /, /healthz unreachable; the cache still serves proxy traffic but observability is off")
	} else {
		if cfg.Admin.HtpasswdFile == "" && admin.IsNonLoopback(cfg.Admin.Listen) {
			logger.Warn("admin_unauthenticated_non_loopback",
				"listen", cfg.Admin.Listen,
				"detail", "admin.listen binds non-loopback AND admin.htpasswd_file is empty: the admin port is reachable from the network without authentication")
		}
		// SPEC5 §10.2: admin_authenticated Info is emitted AFTER
		// admin.New (below) so the user_count comes from the parse
		// that the live authenticator will actually use, not the
		// pre-parse for the config dump. Closes the sub-second
		// TOCTOU window where a mid-startup htpasswd swap could
		// otherwise produce a "authenticated" log line against a
		// file that admin.New fails to parse moments later.
	}

	c, err := cache.Open(ctx, cfg.Cache.Dir, logger)
	if err != nil {
		return fmt.Errorf("open cache: %w", err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			logger.Error("cache close failed", "err", err)
		}
	}()

	// SPEC §4.2: reap orphan partials from a prior crash. Best-effort —
	// a failure here means the disk is unhappy in some way, but the
	// cache itself opened fine, so let the operator see the warning and
	// continue serving cache hits.
	if err := c.SweepTmp(tmpSweepMaxAge); err != nil {
		logger.Warn("startup tmp sweep failed", "err", err)
	}

	// SPEC4 §4.2 steps 5 + 6: pool/ orphan scan + one-shot GC pass.
	// Both are blocking — the cache does not begin answering requests
	// until they complete (we have not yet called Serve on any
	// listener). When gc.enabled = false, gcsvc.StartupPass returns
	// immediately and the periodic goroutine below is never started.
	gcsvc, err := gc.New(gc.Config{
		Cache:               c,
		Logger:              logger,
		Enabled:             cfg.GC.Enabled,
		Interval:            cfg.GC.Interval.Duration,
		BatchSize:           cfg.GC.BatchSize,
		SnapshotBatchSize:   cfg.GC.SnapshotBatchSize,
		MaxTickDuration:     cfg.GC.MaxTickDuration.Duration,
		BlobGrace:           cfg.GC.BlobGrace.Duration,
		KeepDisplaced:       cfg.GC.KeepDisplaced,
		PoolScanWorkers:     cfg.GC.PoolScanWorkers,
		HeartbeatStaleGrace: cfg.HeartbeatStaleGraceEffective(),
	})
	if err != nil {
		return fmt.Errorf("build gc: %w", err)
	}
	if err := gcsvc.StartupPass(ctx); err != nil {
		// A startup-pass failure (DB error, fs error reading pool/)
		// is loud but not fatal — the daemon can still serve. Log
		// and continue; the next periodic tick retries the GC pass,
		// and the next process restart re-runs the pool scan.
		logger.Warn("gc startup pass failed", "err", err)
	}

	parser, err := proxy.New(cfg.Remap, cfg.Mirror)
	if err != nil {
		return fmt.Errorf("build proxy parser: %w", err)
	}

	fetchClient, err := fetch.New(fetch.Options{
		ConnectTimeout:          cfg.Upstream.ConnectTimeout.Duration,
		TotalTimeout:            cfg.Upstream.TotalTimeout.Duration,
		IdleReadTimeout:         cfg.Upstream.IdleReadTimeout.Duration,
		MaxRetries:              cfg.Upstream.MaxRetries,
		UnreachableCooldown:     cfg.Upstream.UnreachableCooldown.Duration,
		UnreachableProbeTimeout: cfg.Upstream.UnreachableProbeTimeout.Duration,
		AllowedHostRegex:        cfg.Upstream.AllowedHostRegex,
		DenyTargetRanges:        cfg.Upstream.DenyTargetRanges,
		UserAgent:               "apt-cacher-ultra/" + Version,
		Logger:                  logger,
	})
	if err != nil {
		return fmt.Errorf("build fetch client: %w", err)
	}

	// Single per-host limiter shared between the handler's miss path
	// and the freshness checker. SPEC §9.3 budget covers all upstream
	// pressure to a host, regardless of which code path generates it.
	hostLimiter := hostsem.New(cfg.Upstream.MaxConcurrentPerHost)

	// SPEC §7.4: the periodic freshness scheduler ctx — also the
	// LifetimeCtx for SPEC2 §7.5 adoption goroutines. Cancelling it
	// during shutdown propagates into in-flight verifier and member
	// fetches; SPEC2 §9.5 step 5 says staging dirs are then abandoned
	// for the next start-up sweep.
	freshCtx, freshCancel := context.WithCancel(context.Background())
	defer freshCancel()

	// SPEC2 §7.6 Phase 2 adoption wiring. When adoption.enabled = true,
	// build the keyring + verifier + adopter here; nil adopter means
	// freshness continues Phase 1 behavior (record divergence, do not
	// flip).
	var adopter *freshness.Adopter
	if cfg.Adoption.Enabled {
		adopter, err = buildAdopter(cfg, c, fetchClient, hostLimiter, logger)
		if err != nil {
			return fmt.Errorf("build adopter: %w", err)
		}
	}

	// SPEC5 §9.7.7 in-memory adoption ring. Process-local; dropped
	// on restart. Capacity 50 events. The freshness package Records
	// into this ring at every adoption-completion site (success
	// AND failure) so the admin status page can show recent activity
	// even for failures, which leave no DB row.
	adoptionRing := observability.NewRing(50)

	freshChecker, err := freshness.New(freshness.Config{
		Cache:        c,
		Fetcher:      fetchClient,
		HostLimiter:  hostLimiter,
		Cooldown:     cfg.Freshness.Cooldown.Duration,
		Refresh:      cfg.Freshness.PeriodicRefresh.Duration,
		Adopter:      adopter,
		LifetimeCtx:  freshCtx,
		Logger:       logger,
		AdoptionRing: adoptionRing,
	})
	if err != nil {
		return fmt.Errorf("build freshness checker: %w", err)
	}

	h, err := handler.New(handler.Config{
		Parser:              parser,
		Cache:               c,
		Fetch:               fetchClient,
		HostLimiter:         hostLimiter,
		Logger:              logger,
		Freshness:           freshChecker,
		Serve:               cfg.Serve,
		RefuseUnvouchedDebs: cfg.Integrity.RefuseUnvouchedDebs,
		AdoptionEnabled:     cfg.Adoption.Enabled,
	})
	if err != nil {
		return fmt.Errorf("build handler: %w", err)
	}

	// SPEC6 §2.2: when tls_mitm.enabled = true, materialize the CA
	// (load supplied or auto-generate per §4.2), build the leaf-cert
	// cache and the CONNECT pipeline, and install it on the handler
	// BEFORE Server.Serve starts accepting traffic. h.connect is read
	// concurrently from ServeHTTP and writing it after the listener
	// is up would race those reads — see Handler.SetConnectHandler.
	var mitmHandles *tlsMitmHandles
	if cfg.TlsMitm.Enabled {
		hs, err := wireTlsMitm(ctx, cfg, parser, fetchClient, h, logger)
		if err != nil {
			return fmt.Errorf("build tls_mitm: %w", err)
		}
		mitmHandles = hs
	}

	// SPEC §7.4: start the periodic refresh scheduler. The scheduler
	// dispatches into Checker.Check, which uses the cache + fetch
	// client, so it must not outlive the handler-side drain — see
	// the freshCancel() / freshWG.Wait() ordering near shutdown.
	var freshWG sync.WaitGroup
	freshWG.Add(1)
	go func() {
		defer freshWG.Done()
		freshChecker.Run(freshCtx)
	}()

	// SPEC2 §6.5 at-rest integrity scanner. Shares the freshCtx so the
	// shutdown sequence below cancels and drains it the same way as
	// the freshness scheduler — neither must outlive the cache. A zero
	// interval disables the scan; Scanner.Run returns immediately and
	// the WaitGroup completes without doing anything.
	scanner, err := integrity.New(integrity.Config{
		Cache:    c,
		Interval: cfg.Integrity.ValidateAtRestInterval.Duration,
		Workers:  cfg.Integrity.ValidateAtRestWorkers,
		Logger:   logger,
	})
	if err != nil {
		return fmt.Errorf("build integrity scanner: %w", err)
	}
	var scannerWG sync.WaitGroup
	scannerWG.Add(1)
	go func() {
		defer scannerWG.Done()
		scanner.Run(freshCtx)
	}()

	// SPEC4 §9.6 periodic GC goroutine. Shares freshCtx so the
	// shutdown sequence below cancels and drains it the same way as
	// the freshness scheduler and integrity scanner. When
	// gc.enabled = false, gcsvc.Run returns immediately.
	var gcWG sync.WaitGroup
	gcWG.Add(1)
	go func() {
		defer gcWG.Done()
		gcsvc.Run(freshCtx)
	}()

	// SPEC6 §9.7.6 tls_mitm_enabled_ca_undistributed refresher.
	// Wakes once per uptime hour, reads the rolling 30-minute
	// CONNECT outcome counter, and emits a Warn when the predicate
	// holds (failures >= 1 AND successes == 0) — surfacing
	// "operator turned on MITM but no client trusts the CA" to the
	// journal where it's actionable. Started only when MITM is
	// enabled (mitmHandles nil otherwise).
	var mitmWatchWG sync.WaitGroup
	if mitmHandles != nil {
		mitmWatchWG.Add(1)
		go func() {
			defer mitmWatchWG.Done()
			proxy.RunUndistributedCAWatch(freshCtx, mitmHandles.Stats, mitmUndistributedRefreshInterval, func(successes, failures int) {
				logger.Warn("tls_mitm_enabled_ca_undistributed",
					"window_minutes", 30,
					"tls_success_count", successes,
					"tls_failure_count", failures,
					"detail", "MITM is enabled and at least one TLS handshake failed in the last 30 minutes with zero successes — clients likely don't yet trust the CA; verify CA distribution to clients")
			})
		}()

		// SPEC6 §10.3 acu_mitm_cert_cache_size gauge refresher.
		// Cheap periodic sample (default 30s) since the gauge is
		// strictly observational; an off-by-30s reading is fine
		// for /metrics and the §10.4 status page. Drives the
		// gauge from the cache's true Size() so insert/evict
		// callbacks don't have to maintain a parallel counter.
		mitmWatchWG.Add(1)
		go func() {
			defer mitmWatchWG.Done()
			runCertCacheSizeRefresher(freshCtx, mitmHandles.CertCacheSize, mitmCertCacheSizeRefreshInterval)
		}()
	}

	// SPEC5 §9.7 admin listener. Built only when admin.enabled.
	// BuildInfo is composed here because internal/admin cannot
	// import main (Go's internal/ rule) — main reads main.Version
	// + debug.ReadBuildInfo() and passes the value-type bridge.
	var adminSrv *admin.Server
	if cfg.Admin.Enabled {
		adminSrv, err = admin.New(admin.Config{
			Cache:       c,
			GC:          gcsvc,
			HostLimiter: hostLimiter,
			Ring:        adoptionRing,
			Registry:    metrics.Default,
			Logger:      logger,
			BuildInfo:   buildInfo(),
			Admin:       cfg.Admin,
			StartTime:   time.Now(),
			ProxyAddr:   plainLn.Addr().String(),
			TLSAddr:     tlsAddrString(tlsLn),
			AdminAddr:   adminLn.Addr().String(),
			TLSMITM:     &tlsMitmProvider{h: mitmHandles},
		})
		if err != nil {
			return fmt.Errorf("build admin: %w", err)
		}
		// SPEC5 §10.2: admin_authenticated emits the live user_count
		// from the post-admin.New authenticator. Skipped when
		// htpasswd_file is empty (admin is unauthenticated).
		if cfg.Admin.HtpasswdFile != "" {
			logger.Info("admin_authenticated",
				"htpasswd_file", cfg.Admin.HtpasswdFile,
				"user_count", adminSrv.UserCount())
		}
	}

	plainSrv := newHTTPServer(h, logger)
	var tlsSrv *http.Server
	if tlsLn != nil {
		tlsSrv = newHTTPServer(h, logger)
	}

	// AIDEV-NOTE: errCh is buffered to (number of servers) so a goroutine
	// can always deliver its terminal error without blocking even if the
	// main goroutine has already moved on to shutdown.
	errCh := make(chan error, 3)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Info("http listener accepting", "addr", plainLn.Addr().String())
		err := plainSrv.Serve(plainLn)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http: %w", err)
			return
		}
		errCh <- nil
	}()

	if tlsSrv != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			logger.Info("https listener accepting", "addr", tlsLn.Addr().String())
			err := tlsSrv.ServeTLS(tlsLn, cfg.Cache.TLSCert, cfg.Cache.TLSKey)
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("https: %w", err)
				return
			}
			errCh <- nil
		}()
	}

	if adminSrv != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			logger.Info("admin listener accepting", "addr", adminLn.Addr().String())
			err := adminSrv.Serve(adminLn)
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("admin: %w", err)
				return
			}
			errCh <- nil
		}()
	}

	// Block until either ctx is cancelled (signal received in production,
	// test-driven cancel in tests) or a listener fails before we asked it
	// to stop.
	var listenerErr error
	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-errCh:
		if err != nil {
			listenerErr = err
			logger.Error("listener exited unexpectedly", "err", err)
		}
	}

	// SPEC §9.5 step 2: stop accepting (Shutdown closes the listener
	// before draining), then wait up to shutdownTimeout for in-flight
	// handlers to drain. Both listeners shut down concurrently under
	// the same deadline — sequential Shutdown would let the second
	// listener keep accepting while the first drained.
	//
	// Use a fresh ctx (not derived from the signal ctx, which is
	// already cancelled) so the deadline is real.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	// SPEC5 §9.5: admin listener shuts down FIRST. Its endpoints
	// read DB state (status page, refresher gauges) and a partial
	// shutdown of the proxy listener should not surface inconsistent
	// observability data to a scraper. Once shutdown begins,
	// /healthz starts returning 503 with X-Acu-Check-Failed:
	// shutdown so reverse-proxy probes steer traffic away.
	if adminSrv != nil {
		if err := adminSrv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("admin shutdown returned error", "err", err)
		}
	}

	var sg sync.WaitGroup
	sg.Add(1)
	go func() {
		defer sg.Done()
		if err := plainSrv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("http shutdown returned error", "err", err)
		}
	}()
	if tlsSrv != nil {
		sg.Add(1)
		go func() {
			defer sg.Done()
			if err := tlsSrv.Shutdown(shutdownCtx); err != nil {
				logger.Warn("https shutdown returned error", "err", err)
			}
		}()
	}
	sg.Wait()

	// Force-close any connection that did not drain within the budget
	// (e.g. a cache-hit ServeContent whose client is no longer reading,
	// wedging the handler write). This MUST happen before h.Close():
	// activeWG.Wait inside h.Close cannot finish while a handler is
	// stuck writing to a wedged client, and lifecycleCancel does not
	// help — only closing the conn does. Running this first bounds
	// slow-client shutdown by the drain budget instead of blocking
	// h.Close() indefinitely.
	if err := plainSrv.Close(); err != nil {
		logger.Warn("http force-close returned error", "err", err)
	}
	if tlsSrv != nil {
		if err := tlsSrv.Close(); err != nil {
			logger.Warn("https force-close returned error", "err", err)
		}
	}

	// SPEC6 §9.4 hijacked-CONNECT tunnel drain. http.Server.Close
	// neither waits for nor closes hijacked conns, so a stalled
	// CONNECT (client hijacks but never sends ClientHello) would
	// otherwise block h.Close → activeWG.Wait below for the full
	// 30s default HandshakeTimeout — well past any reasonable
	// drain budget. Manager.Drain cancels the manager parent ctx
	// (propagating to live tunnels' synthetic inner request ctxs),
	// waits up to the remaining shutdown budget for the in-flight
	// WG to drain, and on deadline expiry force-closes every
	// still-tracked conn so wedged tls.Handshake / bufio.Read
	// goroutines unwind. The 1s grace covers the post-force-close
	// unwind window. When MITM is disabled, mitmHandles is nil
	// and this block is skipped.
	if mitmHandles != nil && mitmHandles.Manager != nil {
		drainBudget := time.Duration(0)
		if dl, ok := shutdownCtx.Deadline(); ok {
			drainBudget = time.Until(dl)
			if drainBudget < 0 {
				drainBudget = 0
			}
		}
		if err := mitmHandles.Manager.Drain(drainBudget, tunnelDrainGrace); err != nil {
			// AIDEV-NOTE: ErrDrainDeadline here means a tunnel
			// goroutine remained wedged AFTER force-closing its
			// conn — i.e. blocked on something conn.Close does not
			// unblock (deadlocked channel write, etc.). h.Close
			// below still calls activeWG.Wait unbounded; in the
			// pathological case the daemon will appear hung on
			// shutdown. Surfacing this Warn is the operator's
			// signal to investigate; the alternative — bounding
			// h.Close — would mean dropping the cache integrity
			// guarantee that no goroutine is using the cache when
			// c.Close runs.
			logger.Warn("tunnel manager drain returned error", "err", err)
		}
	}

	// Stop the periodic freshness scheduler before h.Close so it does
	// not dispatch new Check() calls (which would Add to activeWG)
	// against a handler that's about to drain. Scheduler goroutines
	// already in flight in Check() are tracked by activeWG via the
	// handler's T1 wiring path, so they'll be drained by h.Close.
	freshCancel()
	freshWG.Wait()
	scannerWG.Wait()
	// SPEC4 §9.5 step 6a: GC goroutine is drained on the same
	// freshCtx cancellation. Wait for it to exit before c.Close()
	// so the writer goroutine isn't asked to run a GC batch
	// against a closing cache.
	gcWG.Wait()
	// SPEC6 §9.7.6 mitm refresher rides on the same freshCtx —
	// drained alongside the rest of the periodic-goroutine cohort.
	// When MITM is disabled, mitmWatchWG.Add was never called and
	// Wait returns immediately.
	mitmWatchWG.Wait()

	// SPEC §9.5 step 3: cancel any in-flight upstream fetches (which
	// outlive the request ctx by design — see handler.serveCacheMiss)
	// and wait for the goroutines to exit. After this returns, no
	// goroutine is using the cache, so the deferred c.Close() can run
	// without racing live writes.
	h.Close()

	// SPEC2 §9.5 step 5: drain in-flight adoption goroutines spawned
	// by the freshness Checker. freshCancel above already cancelled
	// the LifetimeCtx those goroutines run under, so they should be
	// returning shortly; this Wait makes the shutdown deterministic
	// before the cache is closed by the deferred c.Close().
	freshChecker.WaitForAdoptions()

	wg.Wait()
	logger.Info("apt-cacher-ultra stopped")
	return listenerErr
}

// buildAdopter constructs the Phase 2 §7.5 adopter: load the host apt
// keyring, compile [[trusted_signer]] regexes, and wire a Verifier into
// a freshness.Adopter. Called only when adoption.enabled = true.
//
// SPEC2 §7.6.1: an empty resulting keyring is a startup error iff
// require_signature = true (no key would satisfy any verification —
// the cache would never adopt and the operator would never see why
// without this gate). With require_signature = false the empty
// keyring is allowed; signature checks fall through to "trust the
// body verbatim" mode for InRelease bodies that lack a clearsign
// block, and any clearsign-bearing body would simply fail to verify.
func buildAdopter(
	cfg *config.Config,
	c *cache.Cache,
	fetchClient *fetch.Client,
	hostLimiter *hostsem.Sem,
	logger *slog.Logger,
) (*freshness.Adopter, error) {
	keyring, err := gpg.LoadKeyring(keyringDirs, logger)
	if err != nil {
		return nil, fmt.Errorf("load apt keyring: %w", err)
	}
	if keyring.Empty() && cfg.Adoption.RequireSignature {
		return nil, errors.New("apt keyring is empty and adoption.require_signature = true; refusing to start (no key would satisfy any verification — populate /etc/apt/trusted.gpg.d/ or /etc/apt/keyrings/)")
	}

	pins, err := compilePins(cfg.TrustedSigners)
	if err != nil {
		return nil, fmt.Errorf("compile [[trusted_signer]] blocks: %w", err)
	}

	verifier, err := gpg.NewVerifier(gpg.VerifierConfig{
		Keyring:          keyring,
		Pins:             pins,
		RequireSignature: cfg.Adoption.RequireSignature,
		RequirePinned:    cfg.Adoption.RequirePinnedSigner,
		Logger:           logger,
	})
	if err != nil {
		return nil, fmt.Errorf("build verifier: %w", err)
	}

	adopter, err := freshness.NewAdopter(freshness.AdoptionConfig{
		Cache:             c,
		Fetcher:           fetchClient,
		Verifier:          verifier,
		HostLimiter:       hostLimiter,
		MaxConcurrent:     cfg.Freshness.MaxConcurrentAdoptions,
		HotPackagesWindow: cfg.HotPackages.Window.Duration,
		HotPrefetchBudget: cfg.Adoption.HotPrefetchBudget.Duration,
		HeartbeatInterval: cfg.GC.HeartbeatInterval.Duration,
		Logger:            logger,
	})
	if err != nil {
		return nil, fmt.Errorf("new adopter: %w", err)
	}

	logger.Info("phase2 adoption enabled",
		"keyring_keys", keyring.Size(),
		"trusted_signer_blocks", len(pins),
		"require_pinned_signer", cfg.Adoption.RequirePinnedSigner,
		"max_concurrent_adoptions", cfg.Freshness.MaxConcurrentAdoptions,
		"hot_packages_window", cfg.HotPackages.Window.Duration,
		"hot_prefetch_budget", cfg.Adoption.HotPrefetchBudget.Duration,
	)
	return adopter, nil
}

// compilePins translates the validated config.TrustedSigner blocks
// into runtime gpg.SignerPin values: regex compiled once, fingerprints
// canonicalized to uppercase. config.Validate has already enforced
// regex syntax and 40-char-hex fingerprints, so unexpected errors
// here are surface-level and worth bubbling up.
func compilePins(in []config.TrustedSigner) ([]gpg.SignerPin, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]gpg.SignerPin, 0, len(in))
	for i, ts := range in {
		re, err := regexp.Compile(ts.MatchCanonicalHost)
		if err != nil {
			return nil, fmt.Errorf("trusted_signer[%d].match_canonical_host: %w", i, err)
		}
		fps := make(map[string]struct{}, len(ts.Fingerprints))
		for _, fp := range ts.Fingerprints {
			fps[strings.ToUpper(fp)] = struct{}{}
		}
		out = append(out, gpg.SignerPin{
			HostRegex:    re,
			Fingerprints: fps,
		})
	}
	return out, nil
}

func tlsAddrString(ln net.Listener) string {
	if ln == nil {
		return ""
	}
	return ln.Addr().String()
}

// buildInfo composes the SPEC5 §10.4.7 BuildInfo from main.Version
// (Makefile-injected via -ldflags) and runtime/debug.ReadBuildInfo
// (Go toolchain populates GoVersion + VCS revision automatically).
// Called once at startup and passed into the admin listener.
func buildInfo() admin.BuildInfo {
	bi := admin.BuildInfo{Version: Version}
	if di, ok := debug.ReadBuildInfo(); ok {
		bi.GoVersion = di.GoVersion
		for _, s := range di.Settings {
			if s.Key == "vcs.revision" {
				bi.VCSRevision = s.Value
				break
			}
		}
	}
	return bi
}

// newHTTPServer builds a *http.Server with the timeouts SPEC §9 implies.
// Notably there is no WriteTimeout: a large .deb stream is allowed to take
// arbitrarily long; the per-fetch budget belongs to fetch.TotalTimeout. A
// ReadHeaderTimeout *is* set because slowloris-style header dribbling is
// not part of any legitimate apt workload, and an IdleTimeout caps the
// lifetime of a keep-alive socket that an inbound client leaves idle —
// without it, an unauthenticated peer can hold file descriptors open
// indefinitely.
func newHTTPServer(h http.Handler, logger *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
}

// wireTlsMitm assembles the SPEC6 §2.2 CONNECT pipeline and
// attaches it to the handler. Called from serveListeners when
// `tls_mitm.enabled = true`. The function performs:
//
//  1. CA materialization via tlsmitm.LoadOrGenerate (§4.2 — supplied
//     path or auto-generate under flock).
//  2. Leaf-cert cache wiring with a per-host generator that signs
//     ECDSA P-256 or RSA-2048 leaves per the configured leaf algo.
//  3. Signing-gate construction from the configured RE2 regex
//     (nil when the regex is empty, matching §5.1.2 vacuous-true).
//  4. proxy.NewConnectHandler with Dispatch=h.ServeHTTP — the
//     synthetic GET/HEAD recurses back through the existing
//     handler pipeline (Parse → Remap → cache lookup → fetch).
//  5. handler.SetConnectHandler under the BEFORE-Serve invariant.
//
// Step 5 is the wiring's load-bearing constraint: writing
// h.connect after the listener has started accepting traffic
// would race ServeHTTP's read of the same field.
// tlsMitmHandles bundles what wireTlsMitm hands back to the caller
// — the connect-outcome stats counter (for the §9.7.6 refresher),
// a closure that reports the current cert-cache size (for the §10.3
// acu_mitm_cert_cache_size gauge refresher), and the §10.4 status
// page snapshot inputs (CA source/fingerprint/expiry/regex,
// capacity).
type tlsMitmHandles struct {
	Stats *proxy.ConnectStats
	// CertCacheSize returns the current entry count. Implemented
	// as a closure rather than passing the *tlsmitm.Cache pointer
	// so callers don't need to import tlsmitm just for size.
	CertCacheSize func() int

	// SPEC6 §10.4 status-page inputs. These are stable across the
	// daemon's lifetime (the CA does not rotate without a restart),
	// so the snapshot copies them rather than holding the *CA.
	CASource            string
	CAFingerprintSHA256 string
	CANotAfterUnixTime  int64
	EffectiveAllowlist  string
	CertCacheCapacity   int

	// Manager is the SPEC6 §9.4 hijacked-CONNECT tunnel manager.
	// Constructed here with a parent ctx derived from the daemon
	// ctx so master-ctx cancellation propagates to live tunnels;
	// the daemon's shutdown sequence calls Manager.Drain between
	// http.Server.Close and Handler.Close to bound hijacked-conn
	// drain by the shutdown deadline (force-closing still-tracked
	// conns at deadline expiry).
	Manager *proxy.TunnelManager
}

func wireTlsMitm(ctx context.Context, cfg *config.Config, parser *proxy.Parser, fetchClient *fetch.Client, h *handler.Handler, logger *slog.Logger) (*tlsMitmHandles, error) {
	tmCfg := cfg.TlsMitm

	ca, err := tlsmitm.LoadOrGenerate(tlsmitm.LoadOptions{
		SuppliedCertPath:     tmCfg.CaCert,
		SuppliedKeyPath:      tmCfg.CaKey,
		StorageDir:           cfg.EffectiveCaStorageDir(),
		AllowedHostRegex:     tmCfg.AllowedHostRegex,
		AllowUnconstrainedCA: tmCfg.AllowUnconstrainedCA,
		CALifetime:           tmCfg.CACertLifetime.Duration,
		LogFn: func(level, event string, fields map[string]any) {
			emitTlsMitmLog(logger, level, event, fields)
		},
	})
	if err != nil {
		return nil, err
	}

	leafAlg, err := tlsmitm.ParseLeafAlgorithm(tmCfg.LeafAlgorithm)
	if err != nil {
		return nil, fmt.Errorf("invalid leaf_algorithm: %w", err)
	}
	leafLifetime := tmCfg.LeafCertLifetime.Duration
	leafAlgoLabel := leafAlg.String()
	// Wrap the gen function so each successful issuance bumps
	// acu_mitm_cert_issued_total{algorithm}. Errors are not
	// counted — they're already captured by acu_mitm_connect_total
	// with outcome=cert_gen_failed.
	leafCache, err := tlsmitm.NewCache(tmCfg.CertCacheSize, func(host string) (*tls.Certificate, error) {
		cert, err := tlsmitm.GenerateLeaf(host, ca.TLSCert, leafAlg, leafLifetime, time.Now())
		if err != nil {
			return nil, err
		}
		proxy.RecordCertIssued(leafAlgoLabel)
		// SPEC6 §10.4: status-page "Last cert issued" feed. The
		// host literal is the CONNECT target (single-SAN per
		// §5.1.4), so this is the same string the operator typed
		// (or what their client requested).
		proxy.NoteCertIssued(host, time.Now())
		// §11 F17 clock-skew detection lives in ServeCONNECT
		// after LeafCache.Get so it fires on every CONNECT
		// (issuance AND cache-reuse). Doing it here would skip
		// the cache-hit path, which is the more likely site for
		// a cached cert with future NotBefore to surface.
		return cert, nil
	})
	if err != nil {
		return nil, err
	}
	// SPEC6 §10.3 cache-side metric hooks + §10.2 eviction log.
	// Lookup hook fires hit/miss; evict hook fires lru/expired;
	// capacity is set once. Size is updated periodically by the
	// gauge refresher goroutine started in serveListeners.
	//
	// SPEC6 §10.2 mandates a `mitm_cert_cache_evicted` Info log
	// with fields {host, reason, age_seconds} on every eviction.
	// Both fires from this single callback so the metric counter
	// and the log line stay 1:1 with the cache's internal evict
	// event — no chance of one firing without the other.
	leafCache.SetOnLookup(proxy.RecordCertCacheLookup)
	leafCache.SetOnEvict(func(host string, reason tlsmitm.EvictReason, ageSeconds float64) {
		proxy.RecordCertEvicted(string(reason))
		emitTlsMitmLog(logger, "info", "mitm_cert_cache_evicted", map[string]any{
			"host":        host,
			"reason":      string(reason),
			"age_seconds": ageSeconds,
		})
	})
	proxy.SetCertCacheCapacity(tmCfg.CertCacheSize)
	proxy.SetCANotAfterUnixtime(ca.Cert.NotAfter.Unix())
	proxy.SetCertCacheSize(leafCache.Size())

	var signingGate func(string) bool
	if tmCfg.AllowedHostRegex != "" {
		re, err := regexp.Compile(tmCfg.AllowedHostRegex)
		if err != nil {
			// Validate already compiled it; this should be unreachable.
			return nil, fmt.Errorf("compile tls_mitm.allowed_host_regex: %w", err)
		}
		signingGate = func(literalHost string) bool {
			return re.MatchString(literalHost)
		}
	}

	tlsTemplate := &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
	}
	stats := proxy.NewConnectStats()
	// SPEC6 §9.4 tunnel manager. Parent ctx is derived from the
	// daemon ctx so SIGINT/SIGTERM-driven cancellation reaches every
	// live tunnel via the synthetic inner request ctx; the daemon's
	// shutdown sequence also calls Manager.Drain explicitly to bound
	// drain by the shutdown deadline and force-close stalled conns.
	tunnelMgr := proxy.NewTunnelManager(ctx)
	connectHandler, err := proxy.NewConnectHandler(proxy.HandlerDeps{
		CA:           ca,
		LeafCache:    leafCache,
		SigningGate:  signingGate,
		FetchGate:    fetchClient.HostAllowed,
		Canonicalize: parser.CanonicalHost,
		Dispatch:     h.ServeHTTP,
		TLSConfig:    tlsTemplate,
		Stats:        stats,
		Manager:      tunnelMgr,
		LogFn: func(level, event string, fields map[string]any) {
			emitTlsMitmLog(logger, level, event, fields)
		},
	})
	if err != nil {
		return nil, err
	}
	h.SetConnectHandler(connectHandler)

	// SPEC6 §5.3 tls_mitm_enabled startup loud-config Info. The
	// match_count / total_canonical_hosts pair is a sanity-check, NOT
	// a correctness primitive — a regex that matches zero of the
	// closed canonical-host set may still legitimately match the
	// operator's intended hostname (which need not be among the
	// known canonicals). When the regex is empty (vacuous-true),
	// match_count == total_canonical_hosts.
	canonicalHosts := parser.CanonicalHosts()
	matchCount := len(canonicalHosts)
	if signingGate != nil {
		matchCount = 0
		for _, h := range canonicalHosts {
			if signingGate(h) {
				matchCount++
			}
		}
	}
	logger.Info("tls_mitm_enabled",
		"source", ca.Source.String(),
		"fingerprint_sha256", ca.FingerprintSHA256,
		"not_after_unixtime", ca.Cert.NotAfter.Unix(),
		"name_constraints", len(ca.NameConstraints) > 0,
		"allowed_host_regex_set", tmCfg.AllowedHostRegex != "",
		"match_count", matchCount,
		"total_canonical_hosts", len(canonicalHosts),
	)
	if tmCfg.AllowedHostRegex != "" {
		logger.Info("tls_mitm_narrowing_regex_set",
			"allowed_host_regex", tmCfg.AllowedHostRegex,
			"upstream_allowed_host_regex", cfg.Upstream.AllowedHostRegex,
		)
	}
	return &tlsMitmHandles{
		Stats:               stats,
		CertCacheSize:       leafCache.Size,
		CASource:            ca.Source.String(),
		CAFingerprintSHA256: ca.FingerprintSHA256,
		CANotAfterUnixTime:  ca.Cert.NotAfter.Unix(),
		EffectiveAllowlist:  tmCfg.AllowedHostRegex,
		CertCacheCapacity:   tmCfg.CertCacheSize,
		Manager:             tunnelMgr,
	}, nil
}

// tlsMitmProvider adapts a *tlsMitmHandles into the
// admin.TLSMITMProvider interface. Stable inputs (CA source,
// fingerprint, capacity) are captured at construction; live signals
// (cert cache size, last-issued, hit rate) are sampled per call so
// the status page reflects current state. nil receiver returns the
// disabled snapshot — supports the tls_mitm.enabled = false branch
// uniformly.
type tlsMitmProvider struct {
	h *tlsMitmHandles
}

func (p *tlsMitmProvider) TLSMITMSnapshot() admin.TLSMITMSnapshot {
	if p == nil || p.h == nil {
		return admin.TLSMITMSnapshot{Enabled: false}
	}
	hits, misses := proxy.CertHitRate60s()
	host, at, _ := proxy.LastCertIssued()
	return admin.TLSMITMSnapshot{
		Enabled:             true,
		CASource:            p.h.CASource,
		CAFingerprintSHA256: p.h.CAFingerprintSHA256,
		CANotAfterUnixTime:  p.h.CANotAfterUnixTime,
		EffectiveAllowlist:  p.h.EffectiveAllowlist,
		CertCacheSize:       p.h.CertCacheSize(),
		CertCacheCapacity:   p.h.CertCacheCapacity,
		LastIssuedHost:      host,
		LastIssuedAt:        at,
		HitRate60sHits:      hits,
		HitRate60sMisses:    misses,
	}
}

// runCertCacheSizeRefresher wakes at every `interval` boundary and
// writes the current cache size to the §10.3
// acu_mitm_cert_cache_size gauge. Returns when ctx is cancelled.
// One initial sample fires on entry so the gauge is populated
// immediately rather than after the first tick.
func runCertCacheSizeRefresher(ctx context.Context, sizer func() int, interval time.Duration) {
	if sizer == nil || interval <= 0 {
		return
	}
	proxy.SetCertCacheSize(sizer())
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			proxy.SetCertCacheSize(sizer())
		}
	}
}

// emitTlsMitmLog forwards a structured event from internal/proxy or
// internal/proxy/tlsmitm to the daemon's slog.Logger. The event
// names + level strings are spec-locked (§10.1, §10.2) — main.go
// is just a transport.
func emitTlsMitmLog(logger *slog.Logger, level, event string, fields map[string]any) {
	attrs := make([]any, 0, len(fields)*2)
	for k, v := range fields {
		attrs = append(attrs, k, v)
	}
	switch level {
	case "error":
		logger.Error(event, attrs...)
	case "warn":
		logger.Warn(event, attrs...)
	case "debug":
		logger.Debug(event, attrs...)
	default:
		logger.Info(event, attrs...)
	}
}

func newLogger(c config.LogConfig) *slog.Logger {
	var level slog.Level
	switch c.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if c.Format == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}
