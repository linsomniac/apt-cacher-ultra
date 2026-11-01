package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/acme"
	"github.com/linsomniac/apt-cacher-ultra/internal/acmedns"
	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
	"github.com/linsomniac/apt-cacher-ultra/internal/servertls"
)

type certificateSource interface {
	TLSConfig() *tls.Config
	Snapshot() servertls.Snapshot
	Run(context.Context, *slog.Logger)
}

type serverTLS struct {
	ctx       context.Context
	cancel    context.CancelFunc
	logger    *slog.Logger
	sources   map[string]certificateSource
	files     []*servertls.Files
	manager   *acme.Manager
	challenge *acme.Challenge
	err       chan error
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// prepareServerTLS runs after all application sockets have been reserved, but
// before any application handler or refresh worker starts. The optional
// transport is a test seam; production always passes nil (system trust).
func prepareServerTLS(ctx context.Context, cfg *config.Config, logger *slog.Logger, transport http.RoundTripper) (*serverTLS, error) {
	if err := cfg.ValidateServerTLS(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	r := &serverTLS{ctx: ctx, cancel: cancel, logger: logger, sources: make(map[string]certificateSource), err: make(chan error, 1)}
	success := false
	defer func() {
		if !success {
			r.Close()
		}
	}()
	loaded := make(map[[2]string]*servertls.Files)
	add := func(role, cert, key string) error {
		pair := [2]string{cert, key}
		f := loaded[pair]
		if f == nil {
			var err error
			f, err = servertls.NewFiles(cert, key)
			if err != nil {
				return err
			}
			loaded[pair] = f
			r.files = append(r.files, f)
		}
		r.sources[role] = f
		return nil
	}
	if cfg.Cache.ListenTLS != "" && !cfg.Cache.UseACME {
		if err := add("cache", cfg.Cache.TLSCert, cfg.Cache.TLSKey); err != nil {
			return nil, err
		}
	}
	if cfg.Admin.Enabled && cfg.Admin.TLSEnabled() && !cfg.Admin.UseACME {
		if err := add("admin", cfg.Admin.TLSCert, cfg.Admin.TLSKey); err != nil {
			return nil, err
		}
	}
	if cfg.ACME.Enabled {
		if err := os.MkdirAll(cfg.Cache.Dir, 0750); err != nil {
			return nil, errors.New("acme: cannot create cache directory")
		}
		managerCfg := acme.Config{DirectoryURL: cfg.ACME.DirectoryURL, DNSName: cfg.ACME.DNSName,
			StateDir: filepath.Join(cfg.Cache.Dir, "acme"), AcceptTerms: cfg.ACME.AcceptTerms, Transport: transport}
		if cfg.ACME.ChallengeMethod() == "dns-01" {
			if !acmedns.Supported(cfg.ACME.DNSProvider) {
				return nil, errors.New("acme: DNS provider is not available; see acme dns-providers")
			}
			managerCfg.DNSProviderName = cfg.ACME.DNSProvider
			managerCfg.DNSFactory = func(ctx context.Context) (acme.DNSProvider, error) {
				return acmedns.New(ctx, cfg.ACME.DNSProvider, cfg.ACME.DNSResolvers)
			}
		} else {
			ln, err := net.Listen("tcp", cfg.ACME.Listen)
			if err != nil {
				return nil, errors.New("acme: cannot reserve challenge listener; check listen address and permissions")
			}
			r.challenge = acme.StartChallenge(ln, cfg.ACME.DNSName)
			r.wg.Add(1)
			go func() {
				defer r.wg.Done()
				select {
				case <-ctx.Done():
				case err := <-r.challenge.Errors():
					r.err <- err
					cancel()
				}
			}()
			managerCfg.Challenge = r.challenge
		}
		var err error
		r.manager, err = acme.NewManager(ctx, managerCfg)
		if err != nil {
			return nil, err
		}
		if err := r.manager.EnsureCertificate(ctx); err != nil {
			return nil, err
		}
		if cfg.Cache.UseACME {
			r.sources["cache"] = r.manager
		}
		if cfg.Admin.UseACME {
			r.sources["admin"] = r.manager
		}
	}
	success = true
	return r, nil
}

func (r *serverTLS) Config(role string) *tls.Config {
	if s := r.sources[role]; s != nil {
		return s.TLSConfig()
	}
	return nil
}
func (r *serverTLS) Snapshots() map[string]servertls.Snapshot {
	if len(r.sources) == 0 {
		return nil
	}
	out := make(map[string]servertls.Snapshot, len(r.sources))
	for role, s := range r.sources {
		out[role] = s.Snapshot()
	}
	return out
}
func (r *serverTLS) ACMEStatus() *acme.Status {
	if r.manager == nil {
		return nil
	}
	s := r.manager.Status()
	return &s
}

func (r *serverTLS) Start() {
	launch := func(s certificateSource) { r.wg.Add(1); go func() { defer r.wg.Done(); s.Run(r.ctx, r.logger) }() }
	for _, f := range r.files {
		launch(f)
	}
	if r.manager != nil {
		launch(r.manager)
	}
	if len(r.sources) == 0 {
		return
	}
	expiry := metrics.NewGauge("acu_server_tls_not_after_unixtime", "Server certificate expiry by listener.", "listener")
	available := metrics.NewGauge("acu_server_tls_available", "Whether a listener has a currently valid certificate.", "listener")
	degraded := metrics.NewGauge("acu_server_tls_degraded", "Whether certificate renewal or file reload is failing.", "listener")
	var attempts, successes, failures *metrics.Gauge
	if r.manager != nil {
		attempts = metrics.NewGauge("acu_acme_attempts", "ACME issuance attempts since process start.")
		successes = metrics.NewGauge("acu_acme_successes", "ACME certificates committed since process start.")
		failures = metrics.NewGauge("acu_acme_failures", "ACME attempts that failed since process start.")
	}
	refresh := func() {
		for role, s := range r.Snapshots() {
			expiry.Set(float64(s.NotAfter), role)
			good := 0.
			if s.State == "usable" || s.State == "degraded" {
				good = 1
			}
			available.Set(good, role)
			bad := 0.
			if s.Error != "" {
				bad = 1
			}
			degraded.Set(bad, role)
		}
		if s := r.ACMEStatus(); s != nil {
			attempts.Set(float64(s.Attempts))
			successes.Set(float64(s.Successes))
			failures.Set(float64(s.Failures))
		}
	}
	refresh()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		timer := time.NewTicker(30 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-r.ctx.Done():
				return
			case <-timer.C:
				refresh()
			}
		}
	}()
}

func (r *serverTLS) Close() {
	r.closeOnce.Do(func() {
		r.cancel()
		if r.challenge != nil {
			r.challenge.Close()
		}
		r.wg.Wait()
		if r.manager != nil {
			r.manager.Close()
		}
	})
}
