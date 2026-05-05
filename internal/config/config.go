// Package config loads and validates the apt-cacher-ultra TOML configuration.
// See SPEC.md §5 for the full reference.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// DefaultCacheDir is the on-disk cache root when not overridden.
const DefaultCacheDir = "/var/cache/apt-cacher-ultra"

// DefaultAllowedHostRegex is the SPEC §6.6 allow-list applied when
// upstream.allowed_host_regex is unset (nil). An explicit empty list in
// config means "deny everything"; that is preserved.
var DefaultAllowedHostRegex = []string{
	`^([a-z0-9-]+\.)*ubuntu\.com$`,
	`^([a-z0-9-]+\.)*debian\.org$`,
	`^ppa\.launchpadcontent\.net$`,
	`^apt\.corretto\.aws$`,
	`^repo\.charm\.sh$`,
	`^pkg\.haproxy\.com$`,
	`^download\.docker\.com$`,
}

// DefaultDenyTargetRanges is the post-DNS deny list applied when
// upstream.deny_target_ranges is unset. Covers loopback, RFC1918,
// link-local, and IPv4-mapped loopback.
var DefaultDenyTargetRanges = []string{
	"127.0.0.0/8", "::1/128",
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
	"169.254.0.0/16", "fe80::/10",
	"::ffff:127.0.0.0/104",
}

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

// Load reads, parses, applies defaults to, and validates a TOML config
// file. Defaults are applied before validation so SPEC §6.6 hardening
// (allowed_host_regex, deny_target_ranges) is in effect even for minimal
// configs that omit those keys entirely. To preserve the "deny everything"
// semantics of an explicit empty list, slice defaults only fill in nil
// (key absent), never empty (key set to []).
//
// Bool defaults are pre-populated *before* the TOML decode rather than
// in Defaults(): bools have no zero-value sentinel that can distinguish
// "key absent" from "key explicitly set to false," so a post-decode
// Defaults() call would either clobber an operator's explicit `false`
// or never fire. Pre-population gives the right semantics — the decoder
// only writes fields the TOML actually contains, leaving the seeded
// default in place when the key is absent.
func Load(path string) (*Config, error) {
	cfg := defaultConfig()
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return nil, fmt.Errorf("decode %q: %w", path, err)
	}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// defaultConfig returns a Config seeded with the SPEC §5.1 defaults that
// cannot be applied after decode (bool fields, where the zero value is
// indistinguishable from "explicitly set to false"). Callers other than
// Load (mostly tests) that bypass the file path must seed bools by hand.
func defaultConfig() *Config {
	return &Config{
		Serve: ServeConfig{
			ServeStaleWhenUpstreamDown: true,
			LogStaleServes:             true,
		},
	}
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
	} else if err := checkWritable(c.Cache.Dir); err != nil {
		errs = append(errs, fmt.Errorf("cache.dir %q: %w", c.Cache.Dir, err))
	}

	// AIDEV-NOTE: cache.listen is not "required" because Defaults() fills in
	// 0.0.0.0:3142. Validate runs *after* defaults, so this only checks the
	// effective value (user-supplied or default).
	if err := validateListenAddr(c.Cache.Listen); err != nil {
		errs = append(errs, fmt.Errorf("cache.listen: %w", err))
	}

	tlsAnySet := c.Cache.ListenTLS != "" || c.Cache.TLSCert != "" || c.Cache.TLSKey != ""
	tlsAllSet := c.Cache.ListenTLS != "" && c.Cache.TLSCert != "" && c.Cache.TLSKey != ""
	if tlsAnySet && !tlsAllSet {
		errs = append(errs, errors.New("cache.listen_tls / tls_cert / tls_key must all be set or all empty"))
	} else if tlsAllSet {
		if err := validateListenAddr(c.Cache.ListenTLS); err != nil {
			errs = append(errs, fmt.Errorf("cache.listen_tls: %w", err))
		}
		if err := checkReadableFile(c.Cache.TLSCert); err != nil {
			errs = append(errs, fmt.Errorf("cache.tls_cert %q: %w", c.Cache.TLSCert, err))
		}
		if err := checkReadableFile(c.Cache.TLSKey); err != nil {
			errs = append(errs, fmt.Errorf("cache.tls_key %q: %w", c.Cache.TLSKey, err))
		}
	}

	if c.Upstream.ConnectTimeout.Duration < 0 {
		errs = append(errs, errors.New("upstream.connect_timeout must not be negative"))
	}
	if c.Upstream.TotalTimeout.Duration < 0 {
		errs = append(errs, errors.New("upstream.total_timeout must not be negative"))
	}
	if c.Upstream.IdleReadTimeout.Duration < 0 {
		errs = append(errs, errors.New("upstream.idle_read_timeout must not be negative"))
	}
	if c.Upstream.MaxRetries < 0 {
		errs = append(errs, errors.New("upstream.max_retries must not be negative"))
	}
	if c.Upstream.MaxConcurrentPerHost < 0 {
		errs = append(errs, errors.New("upstream.max_concurrent_per_host must not be negative"))
	}
	if c.Freshness.Cooldown.Duration < 0 {
		errs = append(errs, errors.New("freshness.cooldown must not be negative"))
	}
	if c.Freshness.PeriodicRefresh.Duration < 0 {
		errs = append(errs, errors.New("freshness.periodic_refresh must not be negative"))
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

	for i, m := range c.Mirror {
		if !strings.HasPrefix(m.Prefix, "/") {
			errs = append(errs, fmt.Errorf("mirror[%d].prefix %q must start with /", i, m.Prefix))
		}
		if m.Upstream == "" {
			errs = append(errs, fmt.Errorf("mirror[%d].upstream is required", i))
		}
	}
	// AIDEV-NOTE: SPEC §5.2 forbids overlapping mirror prefixes (not just
	// duplicates), so /ubuntu must not coexist with /ubuntu/dists. A bare
	// duplicate is a special case of shadow with itself.
	for i := range c.Mirror {
		for j := i + 1; j < len(c.Mirror); j++ {
			a, b := c.Mirror[i].Prefix, c.Mirror[j].Prefix
			if a == "" || b == "" {
				continue
			}
			if shadowsPath(a, b) || shadowsPath(b, a) {
				errs = append(errs, fmt.Errorf(
					"mirror[%d].prefix %q overlaps mirror[%d].prefix %q", i, a, j, b))
			}
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

// validateListenAddr accepts either ":3142" or "host:3142" style addresses
// and verifies that the port is a numeric value in the legal range.
func validateListenAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid host:port %q: %w", addr, err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("port %q is not numeric", port)
	}
	if p < 1 || p > 65535 {
		return fmt.Errorf("port %d out of range 1-65535", p)
	}
	return nil
}

// checkWritable verifies the directory accepts writes by creating and
// removing a probe file. We use this rather than relying on permissions
// bits because Linux capabilities and ACLs can override the mode.
func checkWritable(dir string) error {
	probe, err := os.CreateTemp(dir, ".acu-writetest-*")
	if err != nil {
		return fmt.Errorf("not writable: %w", err)
	}
	name := probe.Name()
	probe.Close()
	return os.Remove(name)
}

// checkReadableFile verifies the path exists, is a regular file, and is
// readable by the current process.
func checkReadableFile(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("not readable: %w", err)
	}
	return f.Close()
}

// shadowsPath reports whether path prefix a "shadows" path prefix b — that
// is, every URL path matched by b is also matched by a, with proper path
// boundary semantics so /ubuntu does not shadow /ubuntu-extra. a and b must
// both start with "/".
func shadowsPath(a, b string) bool {
	if a == b {
		return true
	}
	if !strings.HasPrefix(b, a) {
		return false
	}
	// b == a + tail. The boundary must be a "/" so /ubuntu and /ubuntu-extra
	// are unrelated, but /ubuntu and /ubuntu/dists overlap.
	if strings.HasSuffix(a, "/") {
		return true
	}
	return b[len(a)] == '/'
}

// Defaults populates zero-valued fields with the SPEC defaults. Call after
// Load so unspecified config keys get sensible behavior.
//
// AIDEV-NOTE: Slice defaults (allowed_host_regex, deny_target_ranges) only
// apply when the slice is nil — i.e. the key is absent from TOML. An
// explicit empty list `[]` keeps the empty value, which §6.6 defines as
// "deny everything" / "no IP-range filter".
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
	if c.Upstream.AllowedHostRegex == nil {
		c.Upstream.AllowedHostRegex = append([]string(nil), DefaultAllowedHostRegex...)
	}
	if c.Upstream.DenyTargetRanges == nil {
		c.Upstream.DenyTargetRanges = append([]string(nil), DefaultDenyTargetRanges...)
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
