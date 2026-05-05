// Package config loads and validates the apt-cacher-ultra TOML configuration.
// See SPEC.md §5 for the full reference.
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the top-level structure of config.toml.
type Config struct {
	Cache     CacheConfig     `toml:"cache"`
	Upstream  UpstreamConfig  `toml:"upstream"`
	Freshness FreshnessConfig `toml:"freshness"`
	Serve     ServeConfig     `toml:"serve"`
	Log       LogConfig       `toml:"log"`
	Remap     []RemapRule     `toml:"remap"`
	Mirror    []MirrorRule    `toml:"mirror"`
}

type CacheConfig struct {
	Dir       string `toml:"dir"`
	Listen    string `toml:"listen"`
	ListenTLS string `toml:"listen_tls"`
	TLSCert   string `toml:"tls_cert"`
	TLSKey    string `toml:"tls_key"`
}

type UpstreamConfig struct {
	ConnectTimeout       Duration `toml:"connect_timeout"`
	TotalTimeout         Duration `toml:"total_timeout"`
	IdleReadTimeout      Duration `toml:"idle_read_timeout"`
	MaxRetries           int      `toml:"max_retries"`
	MaxConcurrentPerHost int      `toml:"max_concurrent_per_host"`
	AllowedHostRegex     []string `toml:"allowed_host_regex"`
	DenyTargetRanges     []string `toml:"deny_target_ranges"`
}

type FreshnessConfig struct {
	Cooldown        Duration `toml:"cooldown"`
	PeriodicRefresh Duration `toml:"periodic_refresh"`
}

type ServeConfig struct {
	ServeStaleWhenUpstreamDown bool `toml:"serve_stale_when_upstream_down"`
	LogStaleServes             bool `toml:"log_stale_serves"`
}

type LogConfig struct {
	Level  string `toml:"level"`
	Format string `toml:"format"`
}

type RemapRule struct {
	MatchHostRegex string `toml:"match_host_regex"`
	CanonicalHost  string `toml:"canonical_host"`
}

type MirrorRule struct {
	Prefix   string `toml:"prefix"`
	Upstream string `toml:"upstream"`
}

// Duration wraps time.Duration with TOML text unmarshaling so durations can
// be written as "30s", "5m", etc.
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	d.Duration = parsed
	return nil
}

// Load reads, parses, and validates a TOML config file.
func Load(path string) (*Config, error) {
	cfg := &Config{}
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return nil, fmt.Errorf("decode %q: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate enforces the rules in SPEC.md §5.2.
func (c *Config) Validate() error {
	var errs []error

	if c.Cache.Dir == "" {
		errs = append(errs, errors.New("cache.dir is required"))
	} else if st, err := os.Stat(c.Cache.Dir); err != nil {
		errs = append(errs, fmt.Errorf("cache.dir %q: %w", c.Cache.Dir, err))
	} else if !st.IsDir() {
		errs = append(errs, fmt.Errorf("cache.dir %q is not a directory", c.Cache.Dir))
	}

	if c.Cache.Listen == "" {
		errs = append(errs, errors.New("cache.listen is required"))
	}

	tlsAnySet := c.Cache.ListenTLS != "" || c.Cache.TLSCert != "" || c.Cache.TLSKey != ""
	tlsAllSet := c.Cache.ListenTLS != "" && c.Cache.TLSCert != "" && c.Cache.TLSKey != ""
	if tlsAnySet && !tlsAllSet {
		errs = append(errs, errors.New("cache.listen_tls / tls_cert / tls_key must all be set or all empty"))
	}

	for i, r := range c.Remap {
		if r.MatchHostRegex == "" {
			errs = append(errs, fmt.Errorf("remap[%d].match_host_regex is required", i))
		} else if _, err := regexp.Compile(r.MatchHostRegex); err != nil {
			errs = append(errs, fmt.Errorf("remap[%d].match_host_regex: %w", i, err))
		}
		if r.CanonicalHost == "" {
			errs = append(errs, fmt.Errorf("remap[%d].canonical_host is required", i))
		}
	}

	for i, re := range c.Upstream.AllowedHostRegex {
		if _, err := regexp.Compile(re); err != nil {
			errs = append(errs, fmt.Errorf("upstream.allowed_host_regex[%d]: %w", i, err))
		}
	}

	for i, cidr := range c.Upstream.DenyTargetRanges {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			errs = append(errs, fmt.Errorf("upstream.deny_target_ranges[%d] %q: %w", i, cidr, err))
		}
	}

	seenPrefix := map[string]int{}
	for i, m := range c.Mirror {
		if !strings.HasPrefix(m.Prefix, "/") {
			errs = append(errs, fmt.Errorf("mirror[%d].prefix %q must start with /", i, m.Prefix))
		}
		if prev, exists := seenPrefix[m.Prefix]; exists {
			errs = append(errs, fmt.Errorf("mirror[%d].prefix %q duplicates mirror[%d]", i, m.Prefix, prev))
		}
		seenPrefix[m.Prefix] = i
		if m.Upstream == "" {
			errs = append(errs, fmt.Errorf("mirror[%d].upstream is required", i))
		}
	}

	switch c.Log.Level {
	case "", "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log.level %q invalid (debug|info|warn|error)", c.Log.Level))
	}

	switch c.Log.Format {
	case "", "json", "text":
	default:
		errs = append(errs, fmt.Errorf("log.format %q invalid (json|text)", c.Log.Format))
	}

	return errors.Join(errs...)
}

// Defaults populates zero-valued fields with the SPEC defaults. Call after
// Load so unspecified config keys get sensible behavior.
func (c *Config) Defaults() {
	if c.Cache.Listen == "" {
		c.Cache.Listen = "0.0.0.0:3142"
	}
	if c.Upstream.ConnectTimeout.Duration == 0 {
		c.Upstream.ConnectTimeout.Duration = 30 * time.Second
	}
	if c.Upstream.TotalTimeout.Duration == 0 {
		c.Upstream.TotalTimeout.Duration = 5 * time.Minute
	}
	if c.Upstream.IdleReadTimeout.Duration == 0 {
		c.Upstream.IdleReadTimeout.Duration = 60 * time.Second
	}
	if c.Upstream.MaxRetries == 0 {
		c.Upstream.MaxRetries = 3
	}
	if c.Upstream.MaxConcurrentPerHost == 0 {
		c.Upstream.MaxConcurrentPerHost = 8
	}
	if c.Freshness.Cooldown.Duration == 0 {
		c.Freshness.Cooldown.Duration = 60 * time.Second
	}
	if c.Freshness.PeriodicRefresh.Duration == 0 {
		c.Freshness.PeriodicRefresh.Duration = 15 * time.Minute
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Log.Format == "" {
		c.Log.Format = "json"
	}
}
