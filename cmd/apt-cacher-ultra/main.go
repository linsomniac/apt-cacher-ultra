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
	"sync"
	"syscall"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
	"github.com/linsomniac/apt-cacher-ultra/internal/handler"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
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
	shutdownTimeout = 30 * time.Second

	// tmpSweepMaxAge is the SPEC §4.2 staleness threshold for orphaned
	// partial downloads from previous crashes.
	tmpSweepMaxAge = 5 * time.Minute
)

func main() {
	configPath := flag.String("config", "/etc/apt-cacher-ultra/config.toml", "path to TOML config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(Version)
		return
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

	return serveListeners(ctx, cfg, logger, plainLn, tlsLn)
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
		}
	}()

	logger.Info("apt-cacher-ultra starting",
		"version", Version,
		"listen", plainLn.Addr().String(),
		"listen_tls", tlsAddrString(tlsLn),
		"cache_dir", cfg.Cache.Dir,
	)

	c, err := cache.Open(ctx, cfg.Cache.Dir)
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

	parser, err := proxy.New(cfg.Remap, cfg.Mirror)
	if err != nil {
		return fmt.Errorf("build proxy parser: %w", err)
	}

	fetchClient, err := fetch.New(fetch.Options{
		ConnectTimeout:   cfg.Upstream.ConnectTimeout.Duration,
		TotalTimeout:     cfg.Upstream.TotalTimeout.Duration,
		IdleReadTimeout:  cfg.Upstream.IdleReadTimeout.Duration,
		MaxRetries:       cfg.Upstream.MaxRetries,
		AllowedHostRegex: cfg.Upstream.AllowedHostRegex,
		DenyTargetRanges: cfg.Upstream.DenyTargetRanges,
		UserAgent:        "apt-cacher-ultra/" + Version,
	})
	if err != nil {
		return fmt.Errorf("build fetch client: %w", err)
	}

	// Single per-host limiter shared between the handler's miss path
	// and the freshness checker. SPEC §9.3 budget covers all upstream
	// pressure to a host, regardless of which code path generates it.
	hostLimiter := hostsem.New(cfg.Upstream.MaxConcurrentPerHost)

	freshChecker, err := freshness.New(freshness.Config{
		Cache:       c,
		Fetcher:     fetchClient,
		HostLimiter: hostLimiter,
		Cooldown:    cfg.Freshness.Cooldown.Duration,
		Refresh:     cfg.Freshness.PeriodicRefresh.Duration,
		Logger:      logger,
	})
	if err != nil {
		return fmt.Errorf("build freshness checker: %w", err)
	}

	h, err := handler.New(handler.Config{
		Parser:      parser,
		Cache:       c,
		Fetch:       fetchClient,
		HostLimiter: hostLimiter,
		Logger:      logger,
		Freshness:   freshChecker,
		Serve:       cfg.Serve,
	})
	if err != nil {
		return fmt.Errorf("build handler: %w", err)
	}

	// SPEC §7.4: start the periodic refresh scheduler. Scoped to a
	// dedicated ctx so we can stop it deterministically before
	// h.Close() — the scheduler dispatches into Checker.Check, which
	// uses the cache + fetch client, so it must not outlive the
	// handler-side drain.
	freshCtx, freshCancel := context.WithCancel(context.Background())
	defer freshCancel()
	var freshWG sync.WaitGroup
	freshWG.Add(1)
	go func() {
		defer freshWG.Done()
		freshChecker.Run(freshCtx)
	}()

	plainSrv := newHTTPServer(h, logger)
	var tlsSrv *http.Server
	if tlsLn != nil {
		tlsSrv = newHTTPServer(h, logger)
	}

	// AIDEV-NOTE: errCh is buffered to (number of servers) so a goroutine
	// can always deliver its terminal error without blocking even if the
	// main goroutine has already moved on to shutdown.
	errCh := make(chan error, 2)
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

	// Stop the periodic freshness scheduler before h.Close so it does
	// not dispatch new Check() calls (which would Add to activeWG)
	// against a handler that's about to drain. Scheduler goroutines
	// already in flight in Check() are tracked by activeWG via the
	// handler's T1 wiring path, so they'll be drained by h.Close.
	freshCancel()
	freshWG.Wait()

	// SPEC §9.5 step 3: cancel any in-flight upstream fetches (which
	// outlive the request ctx by design — see handler.serveCacheMiss)
	// and wait for the goroutines to exit. After this returns, no
	// goroutine is using the cache, so the deferred c.Close() can run
	// without racing live writes.
	h.Close()

	wg.Wait()
	logger.Info("apt-cacher-ultra stopped")
	return listenerErr
}

func tlsAddrString(ln net.Listener) string {
	if ln == nil {
		return ""
	}
	return ln.Addr().String()
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
