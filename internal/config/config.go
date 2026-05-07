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
	Cache          CacheConfig       `toml:"cache"`
	Upstream       UpstreamConfig    `toml:"upstream"`
	Freshness      FreshnessConfig   `toml:"freshness"`
	Adoption       AdoptionConfig    `toml:"adoption"`
	HotPackages    HotPackagesConfig `toml:"hot_packages"`
	Integrity      IntegrityConfig   `toml:"integrity"`
	GC             GCConfig          `toml:"gc"`
	Serve          ServeConfig       `toml:"serve"`
	Log            LogConfig         `toml:"log"`
	Remap          []RemapRule       `toml:"remap"`
	Mirror         []MirrorRule      `toml:"mirror"`
	TrustedSigners []TrustedSigner   `toml:"trusted_signer"`
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

	// UnreachableCooldown gates the per-host fast-fail path: a host
	// whose dial just failed has subsequent dials within this window
	// collapsed to a single short-deadline probe (UnreachableProbeTimeout)
	// with retries suppressed on probe failure. Default 30s. Set to 0
	// to disable (legacy behavior — full ConnectTimeout × MaxRetries
	// budget on every miss). SPEC §1 "never hang."
	UnreachableCooldown     Duration `toml:"unreachable_cooldown"`
	UnreachableProbeTimeout Duration `toml:"unreachable_probe_timeout"`
}

type FreshnessConfig struct {
	Cooldown        Duration `toml:"cooldown"`
	PeriodicRefresh Duration `toml:"periodic_refresh"`

	// MaxConcurrentAdoptions caps how many SPEC2 §7.5 adoption
	// goroutines may run at once across the whole cache. SPEC2 §9.3.1.
	// 0 = unlimited; default 2.
	MaxConcurrentAdoptions int `toml:"max_concurrent_adoptions"`
}

// AdoptionConfig holds the SPEC2 §5.1 [adoption] block. The defaults
// match the SPEC2 §1.1 / §7.6 secure posture: signatures required,
// no per-suite pinning. Operators flip Enabled to true once the
// shadow-deploy cycle confirms behavior.
type AdoptionConfig struct {
	// Enabled is the master switch. False = Phase 1 behavior (record
	// the divergence, do not adopt). Default false during rollout.
	Enabled bool `toml:"enabled"`

	// RequireSignature rejects an InRelease that fails GPG verification.
	// Default true. Setting false is loud (WARN at startup) and is
	// only sensible for explicitly trusted unsigned upstreams.
	RequireSignature bool `toml:"require_signature"`

	// RequirePinnedSigner causes adoption to fail closed when a suite
	// has no matching [[trusted_signer]] block. Default false (matches
	// apt's broad-trust default); flip true once every active suite
	// has an explicit pin (recommended for production — SPEC2 §7.6.5).
	RequirePinnedSigner bool `toml:"require_pinned_signer"`

	// HotPrefetchBudget caps the wall-clock spent in the SPEC3 §7.5
	// hot-deb prefetch loop. 0 = no wall-clock guard (loop runs until
	// every hot deb has terminated; per-deb fetches still respect
	// upstream.total_timeout × upstream.max_retries). Default 5m.
	// Presence-sensitive: explicit 0 means "operator opted out of the
	// wall-clock guard" and is preserved through Load.
	HotPrefetchBudget Duration `toml:"hot_prefetch_budget"`
}

// HotPackagesConfig holds the SPEC3 §5.1 [hot_packages] block. The hot
// set drives proactive prefetch: a .deb path is "hot" if a client has
// requested it within the configured window.
type HotPackagesConfig struct {
	// Window is the look-back the hot-set query uses against
	// url_path.last_requested_at. 0 = disable hot-package proactive
	// refresh entirely (adoption falls back to Phase 2 behavior).
	// Default 24h. Presence-sensitive: an operator-written 0 must
	// survive Defaults().
	Window Duration `toml:"window"`
}

// IntegrityConfig holds the SPEC2 §5.1 [integrity] block. The at-rest
// scan periodically re-hashes pool blobs to detect on-disk corruption
// independently of the adoption-time rehash defense (SPEC2 §12.5).
type IntegrityConfig struct {
	// ValidateAtRestInterval is the cadence for the scan. 0 disables.
	// Default 24h.
	ValidateAtRestInterval Duration `toml:"validate_at_rest_interval"`

	// ValidateAtRestWorkers bounds the worker pool for the scan so a
	// large pool/ doesn't starve request handling. Default 4. Must
	// be >= 1 when interval > 0.
	ValidateAtRestWorkers int `toml:"validate_at_rest_workers"`

	// RefuseUnvouchedDebs is the SPEC3 §6.1 strict-mode flag. When
	// true, .deb GETs whose canonical (host, path) lacks a package_hash
	// row under any current snapshot are refused with 502 + Retry-After
	// — but only when the host's current snapshots have proven complete
	// coverage (every snapshot has package_coverage_complete = 1) and
	// adoption.enabled is true. Default false; the default-flip to
	// true is gated on production observational data (SPEC3 §1.3).
	// Inert when adoption.enabled = false (startup warning emitted in
	// that combination).
	RefuseUnvouchedDebs bool `toml:"refuse_unvouched_debs"`
}

// GCConfig holds the SPEC4 §5.1 [gc] block. Drives the Phase 4 garbage
// collection subsystem: the periodic goroutine, blob/snapshot reap
// passes, the startup pool/ orphan scan, and the per-adoption heartbeat
// ticker.
type GCConfig struct {
	// Enabled is the master switch. False = goroutine not started,
	// startup pool scan skipped, startup GC pass skipped. A
	// gc_disabled Warn fires at startup when false.
	Enabled bool `toml:"enabled"`

	// Interval is the cadence of the periodic GC tick. Default 1h.
	// 0 is rejected at load (use enabled = false to disable).
	Interval Duration `toml:"interval"`

	// BatchSize bounds the per-batch DELETE in the blob GC pass.
	// Default 100. Must be >= 1.
	BatchSize int `toml:"batch_size"`

	// SnapshotBatchSize bounds the per-batch cascade DELETE in the
	// snapshot GC pass. Smaller than BatchSize because each
	// snapshot's cascade can touch tens of thousands of
	// snapshot_member + package_hash rows. Default 10. Must be >= 1.
	SnapshotBatchSize int `toml:"snapshot_batch_size"`

	// MaxTickDuration is the hard upper bound on a single GC tick
	// (periodic OR startup). Default 5m. 0 is rejected at load.
	MaxTickDuration Duration `toml:"max_tick_duration"`

	// BlobGrace is the "since refcount reached 0" grace before a
	// blob becomes reapable. Default 5m. 0 is rejected at load — a
	// 0s grace makes refcount=0 blobs immediately reapable, which
	// is unsafe under the FK-INSERT race.
	BlobGrace Duration `toml:"blob_grace"`

	// KeepDisplaced is the per-suite forensic retention count for
	// displaced snapshots. Default 3. 0 is permitted (no retention,
	// every displaced snapshot is reapable on the next tick).
	KeepDisplaced int `toml:"keep_displaced"`

	// PoolScanWorkers is the worker pool size for the startup
	// pool/ orphan-file scan. Default 4. Must be >= 1.
	PoolScanWorkers int `toml:"pool_scan_workers"`

	// HeartbeatInterval is the period of the in-process per-adoption
	// heartbeat ticker (SPEC4 §7.5.2 site 6). Default 60s. Must be
	// > 0 AND strictly less than the runtime-derived
	// heartbeat_stale_grace_effective (= max(upstream.total_timeout
	// × upstream.max_retries, 30m)). A heartbeat_interval >= grace
	// can't bound the heartbeat-gap and would let GC reap live
	// adoptions; rejected at load with gc_heartbeat_interval_unsafe
	// Error.
	HeartbeatInterval Duration `toml:"heartbeat_interval"`
}

// HeartbeatStaleGraceEffective returns the runtime-derived grace
// max(upstream.total_timeout × upstream.max_retries, 30m) the snapshot
// GC pass uses for sub-job A's stale-heartbeat reap predicate. Surfaced
// here so the cross-key validation in Validate() and the §10.3 startup
// dump can consult one source of truth.
func (c *Config) HeartbeatStaleGraceEffective() time.Duration {
	derived := c.Upstream.TotalTimeout.Duration *
		time.Duration(c.Upstream.MaxRetries)
	const floor = 30 * time.Minute
	if derived < floor {
		return floor
	}
	return derived
}

// TrustedSigner is one entry of the SPEC2 §5.1 [[trusted_signer]] array.
// When MatchCanonicalHost matches a suite's canonical host, the
// adoption trust set narrows to keys whose fingerprint is in the
// Fingerprints list (§7.6.2 hybrid model).
type TrustedSigner struct {
	MatchCanonicalHost string   `toml:"match_canonical_host"`
	Fingerprints       []string `toml:"fingerprints"`
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
	md, err := toml.DecodeFile(path, cfg)
	if err != nil {
		return nil, fmt.Errorf("decode %q: %w", path, err)
	}
	// Phase 2 presence-sensitive defaults: int/duration fields where 0
	// is a documented meaningful value (max_concurrent_adoptions=0 →
	// unlimited per SPEC2 §9.3.1; validate_at_rest_interval=0 → scan
	// disabled per SPEC2 §5.1) need to be distinguished from "key
	// absent." TOML's MetaData.IsDefined gives us that signal — the
	// post-decode Defaults() call cannot, because the Go zero value
	// has no nil sentinel for ints/durations. Apply these BEFORE
	// Defaults() runs, so Defaults() sees a non-zero value and
	// leaves it alone.
	if !md.IsDefined("freshness", "max_concurrent_adoptions") {
		cfg.Freshness.MaxConcurrentAdoptions = 2
	}
	if !md.IsDefined("integrity", "validate_at_rest_interval") {
		cfg.Integrity.ValidateAtRestInterval.Duration = 24 * time.Hour
	}
	if !md.IsDefined("integrity", "validate_at_rest_workers") {
		cfg.Integrity.ValidateAtRestWorkers = 4
	}
	// upstream.unreachable_cooldown / unreachable_probe_timeout: 0 means
	// "disable" (legacy behavior), so apply defaults only when the key
	// is absent. SPEC §1 fast-fail.
	if !md.IsDefined("upstream", "unreachable_cooldown") {
		cfg.Upstream.UnreachableCooldown.Duration = 30 * time.Second
	}
	if !md.IsDefined("upstream", "unreachable_probe_timeout") {
		cfg.Upstream.UnreachableProbeTimeout.Duration = 1 * time.Second
	}
	// SPEC3 §5.2 presence-sensitive defaults: hot_packages.window = 0
	// disables proactive refresh, adoption.hot_prefetch_budget = 0
	// disables the wall-clock guard. Both are documented meaningful
	// values that must survive Defaults().
	if !md.IsDefined("hot_packages", "window") {
		cfg.HotPackages.Window.Duration = 24 * time.Hour
	}
	if !md.IsDefined("adoption", "hot_prefetch_budget") {
		cfg.Adoption.HotPrefetchBudget.Duration = 5 * time.Minute
	}
	// SPEC4 §5.2 presence-sensitive defaults: gc.enabled is bool
	// (pre-populated in defaultConfig); the rest of the [gc] keys
	// default to non-zero values that don't collide with documented
	// 0 semantics, but we still apply them via IsDefined so an
	// operator who writes `interval = "0s"` (rejected by Validate)
	// is not silently rescued by Defaults() to "1h".
	if !md.IsDefined("gc", "interval") {
		cfg.GC.Interval.Duration = 1 * time.Hour
	}
	if !md.IsDefined("gc", "batch_size") {
		cfg.GC.BatchSize = 100
	}
	if !md.IsDefined("gc", "snapshot_batch_size") {
		cfg.GC.SnapshotBatchSize = 10
	}
	if !md.IsDefined("gc", "max_tick_duration") {
		cfg.GC.MaxTickDuration.Duration = 5 * time.Minute
	}
	if !md.IsDefined("gc", "blob_grace") {
		cfg.GC.BlobGrace.Duration = 5 * time.Minute
	}
	if !md.IsDefined("gc", "keep_displaced") {
		cfg.GC.KeepDisplaced = 3
	}
	if !md.IsDefined("gc", "pool_scan_workers") {
		cfg.GC.PoolScanWorkers = 4
	}
	if !md.IsDefined("gc", "heartbeat_interval") {
		cfg.GC.HeartbeatInterval.Duration = 60 * time.Second
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
//
// Phase 2 additions: adoption.require_signature defaults true (the
// secure posture; operators must explicitly opt out via the loud
// require_signature = false config). adoption.enabled and
// require_pinned_signer remain false (match apt's broad-trust default
// during rollout; flip-to-true is a per-deployment decision).
func defaultConfig() *Config {
	return &Config{
		Serve: ServeConfig{
			ServeStaleWhenUpstreamDown: true,
			LogStaleServes:             true,
		},
		Adoption: AdoptionConfig{
			RequireSignature: true,
		},
		GC: GCConfig{
			// SPEC4 §5.1: gc.enabled defaults true. Bool fields must
			// be pre-populated in defaultConfig (no zero-value
			// sentinel can distinguish "absent" from "explicit
			// false"). Operators opt out with explicit
			// `enabled = false`, which fires gc_disabled Warn.
			Enabled: true,
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
	if c.Upstream.UnreachableCooldown.Duration < 0 {
		errs = append(errs, errors.New("upstream.unreachable_cooldown must not be negative"))
	}
	if c.Upstream.UnreachableProbeTimeout.Duration < 0 {
		errs = append(errs, errors.New("upstream.unreachable_probe_timeout must not be negative"))
	}
	if c.Freshness.Cooldown.Duration < 0 {
		errs = append(errs, errors.New("freshness.cooldown must not be negative"))
	}
	if c.Freshness.PeriodicRefresh.Duration < 0 {
		errs = append(errs, errors.New("freshness.periodic_refresh must not be negative"))
	}
	if c.Freshness.MaxConcurrentAdoptions < 0 {
		errs = append(errs, errors.New("freshness.max_concurrent_adoptions must not be negative"))
	}
	if c.Integrity.ValidateAtRestInterval.Duration < 0 {
		errs = append(errs, errors.New("integrity.validate_at_rest_interval must not be negative"))
	}
	if c.Integrity.ValidateAtRestInterval.Duration > 0 && c.Integrity.ValidateAtRestWorkers < 1 {
		errs = append(errs, errors.New("integrity.validate_at_rest_workers must be >= 1 when interval > 0"))
	}
	if c.Integrity.ValidateAtRestWorkers < 0 {
		errs = append(errs, errors.New("integrity.validate_at_rest_workers must not be negative"))
	}
	if c.HotPackages.Window.Duration < 0 {
		errs = append(errs, errors.New("hot_packages.window must not be negative"))
	}
	if c.Adoption.HotPrefetchBudget.Duration < 0 {
		errs = append(errs, errors.New("adoption.hot_prefetch_budget must not be negative"))
	}

	// SPEC4 §5.2: [gc] block validation.
	if c.GC.Interval.Duration <= 0 {
		errs = append(errs, errors.New("gc.interval must be > 0 (use gc.enabled = false to disable)"))
	}
	if c.GC.BatchSize < 1 {
		errs = append(errs, errors.New("gc.batch_size must be >= 1"))
	}
	if c.GC.SnapshotBatchSize < 1 {
		errs = append(errs, errors.New("gc.snapshot_batch_size must be >= 1"))
	}
	if c.GC.MaxTickDuration.Duration <= 0 {
		errs = append(errs, errors.New("gc.max_tick_duration must be > 0"))
	}
	if c.GC.BlobGrace.Duration < time.Second {
		// AIDEV-NOTE: the §9.6.2 reap predicate compares
		// refcount_zeroed_at (unix epoch seconds) against now -
		// blob_grace.Seconds(); a sub-second value silently
		// truncates to 0, making refcount<=0 blobs immediately
		// reapable on the very next tick — exactly the safety
		// failure mode SPEC4 §5.1 names as "0s is rejected at
		// load." Reject the truncate-to-zero region too. (>0 alone
		// would let "500ms" through.)
		errs = append(errs, errors.New("gc.blob_grace must be >= 1s"))
	}
	if c.GC.KeepDisplaced < 0 {
		errs = append(errs, errors.New("gc.keep_displaced must not be negative"))
	}
	if c.GC.PoolScanWorkers < 1 {
		errs = append(errs, errors.New("gc.pool_scan_workers must be >= 1"))
	}
	if c.GC.HeartbeatInterval.Duration <= 0 {
		errs = append(errs, errors.New("gc.heartbeat_interval must be > 0"))
	} else {
		if grace := c.HeartbeatStaleGraceEffective(); c.GC.HeartbeatInterval.Duration >= grace {
			// AIDEV-NOTE: SPEC4 §10.2 names this Error
			// gc_heartbeat_interval_unsafe — refusing to start is safer
			// than starting with a configuration that can silently reap
			// live adoptions. The heartbeat-gap upper bound is
			// heartbeat_interval + writer-queue depth; if heartbeat_interval
			// alone meets-or-exceeds the grace, the safety argument for
			// the §9.6.3 reap predicate collapses.
			errs = append(errs, fmt.Errorf(
				"gc.heartbeat_interval (%s) must be strictly less than heartbeat_stale_grace_effective (%s = max(upstream.total_timeout × upstream.max_retries, 30m))",
				c.GC.HeartbeatInterval.Duration, grace))
		}
		// AIDEV-NOTE: parallel constraint for the §7.5.1 Rule 1 race
		// window. Adoption's heartbeat-blobs ticker (§7.5.2 site 6,
		// adoption.go heartbeat()→HeartbeatBlobs) refreshes
		// blob.refcount_zeroed_at every heartbeat_interval. If
		// heartbeat_interval >= blob_grace, an in-flight member blob
		// can age past the §9.6.2 reap predicate's
		// `refcount_zeroed_at < now - blob_grace` between two
		// consecutive heartbeats — i.e. before CommitAdoption Step 4
		// can bump refcount and clear the grace clock — and be
		// reaped by GC mid-adoption. Refuse to start.
		if c.GC.BlobGrace.Duration > 0 && c.GC.HeartbeatInterval.Duration >= c.GC.BlobGrace.Duration {
			errs = append(errs, fmt.Errorf(
				"gc.heartbeat_interval (%s) must be strictly less than gc.blob_grace (%s) — otherwise in-flight member blobs can be reaped between heartbeats before CommitAdoption lands",
				c.GC.HeartbeatInterval.Duration, c.GC.BlobGrace.Duration))
		}
	}

	for i, ts := range c.TrustedSigners {
		if ts.MatchCanonicalHost == "" {
			errs = append(errs, fmt.Errorf("trusted_signer[%d].match_canonical_host is required", i))
		} else if _, err := regexp.Compile(ts.MatchCanonicalHost); err != nil {
			errs = append(errs, fmt.Errorf("trusted_signer[%d].match_canonical_host: %w", i, err))
		}
		// SPEC2 §5.2: empty fingerprint list is a footgun — no key
		// would ever match. Reject loudly. Each entry must be a
		// 40-char hex fingerprint (long-form). Short 16-char IDs
		// are cryptographically insufficient and apt also rejects
		// them.
		if len(ts.Fingerprints) == 0 {
			errs = append(errs, fmt.Errorf("trusted_signer[%d].fingerprints is empty (no key would satisfy verification)", i))
		}
		for j, fp := range ts.Fingerprints {
			if !validGPGFingerprint(fp) {
				errs = append(errs, fmt.Errorf("trusted_signer[%d].fingerprints[%d] %q: must be 40 hex chars (long-form fingerprint)", i, j, fp))
			}
		}
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
	// SPEC2 §9.3.1: max_concurrent_adoptions defaults 2, 0 = unlimited.
	// SPEC2 §5.1:   validate_at_rest_interval defaults 24h, 0 = disabled.
	//               validate_at_rest_workers defaults 4.
	// SPEC3 §5.2:   hot_packages.window defaults 24h, 0 = hot prefetch off.
	//               adoption.hot_prefetch_budget defaults 5m, 0 = unbounded.
	// All five are presence-sensitive — explicit 0 has documented
	// meaning that differs from the default. Defaults are applied in
	// Load() via TOML's MetaData.IsDefined; this method (called by
	// non-Load callers without a TOML source) cannot distinguish
	// "explicit 0" from "absent" so it deliberately leaves the zero
	// value alone. Tests that bypass Load and want the SPEC defaults
	// must set these fields by hand.
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Log.Format == "" {
		c.Log.Format = "json"
	}
}

// validGPGFingerprint reports whether s is a 40-character hex string
// (the long-form GPG fingerprint apt expects). Both upper- and lower-
// case hex are accepted; the canonical-form normalization (uppercase,
// no whitespace) happens at GPG-trust-set load time.
func validGPGFingerprint(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}
