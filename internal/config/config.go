// Package config loads and validates the apt-cacher-ultra TOML configuration.
// See SPEC.md §5 for the full reference.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
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

// architectureNameRE is the SPEC6_5 §5.2 shape predicate for a single
// entry in [adoption].architectures: lowercase ASCII letters and digits,
// must start with a letter. Matches "amd64", "arm64", "i386", "ppc64el",
// "s390x", and the pseudo-arch "source"; rejects "AMD64", "x86_64", and
// hyphenated alternative-OS arches like "kfreebsd-amd64". Operators
// needing alternative-OS arches amend the spec.
var architectureNameRE = regexp.MustCompile(`^[a-z][a-z0-9]*$`)

// MaxArchitecturesEntries caps the [adoption].architectures list size
// per SPEC6_5 §5.2 H2. Real fleets care about ≤ 5 arches; the cap is an
// anti-foot-gun guard.
const MaxArchitecturesEntries = 32

// Config is the top-level structure of config.toml.
type Config struct {
	Cache          CacheConfig       `toml:"cache"`
	Upstream       UpstreamConfig    `toml:"upstream"`
	Freshness      FreshnessConfig   `toml:"freshness"`
	Adoption       AdoptionConfig    `toml:"adoption"`
	HotPackages    HotPackagesConfig `toml:"hot_packages"`
	Integrity      IntegrityConfig   `toml:"integrity"`
	GC             GCConfig          `toml:"gc"`
	Admin          AdminConfig       `toml:"admin"`
	Serve          ServeConfig       `toml:"serve"`
	Log            LogConfig         `toml:"log"`
	TlsMitm        TlsMitmConfig     `toml:"tls_mitm"`
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

	// AdvertiseHost is the SPEC6 §14.2 client-facing host (or
	// host:port) the `--print-apt-conf` snippet emits. Empty
	// default; the daemon never reads this — listener bind, request
	// handling, and URL canonicalization use Listen exclusively.
	// When non-empty, must parse as a host or host:port (no scheme,
	// no path). Useful when Listen binds 0.0.0.0/:: and the snippet
	// would otherwise emit a non-routable target.
	AdvertiseHost string `toml:"advertise_host"`
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

	// AllowHTTPSToHTTPRedirect controls whether the upstream fetcher
	// follows a 3xx that drops scheme from https:// to http://. The
	// host-allowlist gate (and the dial-side deny-CIDR check) still
	// runs regardless. Default true.
	//
	// Why default-allow: real-world apt mirrors (notably
	// packages.microsoft.com) 30x to a CDN that may be reached over
	// HTTP. The threat surface is narrow because apt's signature
	// chain hash-pins every artifact through the GPG-signed
	// InRelease — bytes that arrive over the plaintext hop are still
	// rejected by the client if they don't match the pinned hash.
	// Operators caching content that is NOT covered by an apt-secure
	// chain (e.g. unsigned third-party tarballs) should set this to
	// false; the cache then refuses the redirect with
	// ErrRedirectBlocked → 502 and the operator must add a Remap
	// rule pointing at the redirect target instead.
	AllowHTTPSToHTTPRedirect bool `toml:"allow_https_to_http_redirect"`
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

	// Architectures is the SPEC6_5 §5.1 per-arch adoption allowlist.
	// Empty (default) preserves Phase 6 behavior: every Release-listed
	// Packages / Sources / *.diff/Index file is adopted. Non-empty:
	// only members under binary-<arch>/ or source/ where <arch> is
	// listed are adopted. The pseudo-arch "source" controls Sources
	// adoption; non-arch members (Release.gpg, Contents-*, i18n) are
	// not subject to the filter. Validated against architectureNameRE.
	Architectures []string `toml:"architectures"`
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

// AdminConfig holds the SPEC5 §5.1 [admin] block. Drives the Phase 5
// admin listener: /metrics, /, /healthz endpoints; htpasswd Basic
// auth; refresher goroutine; per-metric series cap.
type AdminConfig struct {
	// Enabled is the master switch. When false, the admin listener
	// is not bound; /metrics, /, and /healthz are unreachable. The
	// cache continues to serve proxy traffic normally — the
	// operator has implicitly opted out of all observability. A
	// startup admin_disabled Warn fires when false (parallel to
	// gc_disabled SPEC4 §10.2). Default true.
	Enabled bool `toml:"enabled"`

	// Listen is the bind address for the admin HTTP listener.
	// Default 127.0.0.1:6789 — loopback by default, port chosen to
	// avoid colliding with the proxy (3142). Reuses
	// validateListenAddr().
	Listen string `toml:"listen"`

	// HtpasswdFile is the optional Apache htpasswd file (bcrypt-only)
	// for HTTP Basic auth on every admin request. Empty (default)
	// means "no auth — operator relies on bind-address as the trust
	// boundary." Non-empty path must exist, be readable, and parse
	// as bcrypt-only htpasswd ($2a$, $2b$, $2y$). Older formats
	// ($apr1$ Apache MD5, {SHA} SHA-1, crypt(3) DES) rejected at
	// startup with a config error naming the offending line.
	HtpasswdFile string `toml:"htpasswd_file"`

	// GaugeRefresh is the period of the in-process refresher
	// goroutine that recomputes expensive gauges (acu_blobs_db_count,
	// acu_pool_disk_bytes, acu_per_host_inflight, etc.). Default
	// 30s. A scrape can read a cell up to GaugeRefresh seconds
	// stale; the refresher does an immediate first recompute at
	// startup so the first /metrics scrape is not zeros. Must be
	// > 0 and ≤ 1h.
	GaugeRefresh Duration `toml:"gauge_refresh"`

	// ReadTimeout bounds how long the admin server waits for a
	// request line + headers (HTTP ReadHeaderTimeout). Default 5s.
	// Must be > 0 and ≤ 1m.
	ReadTimeout Duration `toml:"read_timeout"`

	// IdleTimeout bounds keep-alive idle wait on the admin
	// listener. Default 30s. Must be > 0 and ≤ 10m.
	IdleTimeout Duration `toml:"idle_timeout"`

	// MetricSeriesCap is the per-metric series cap applied to
	// labeled metrics. When a new label-value tuple would push the
	// metric's series count past this, the increment is silently
	// dropped and a one-shot metrics_series_cap_reached Warn fires.
	// Default 1024. Must be ≥ 1.
	MetricSeriesCap int `toml:"metric_series_cap"`
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

// TlsMitmConfig holds the SPEC6 §5.1 [tls_mitm] block. Default OFF;
// when enabled, the CONNECT method is upgraded from "405 Method Not
// Allowed" to a transparent MITM tunnel that signs leaf certs from
// the configured CA. See SPEC6 §2.2 for the CONNECT pipeline and
// §5.1.1 / §5.1.1.1 for the auto-generated CA's Name Constraints
// translation contract.
type TlsMitmConfig struct {
	// Enabled is the master switch. False = SPEC §2.6 method
	// dispatch (CONNECT → 405). Default false; flip-to-true is a
	// per-deployment decision because MITM rewrites the trust
	// posture of every cached HTTPS upstream.
	Enabled bool `toml:"enabled"`

	// CaCert / CaKey are operator-supplied paths. Both empty =
	// auto-generate path (uses CaStorageDir). Both set = supplied
	// path (cert/key parsed and validated at startup).
	// One-set-one-empty is rejected at validation.
	CaCert string `toml:"ca_cert"`
	CaKey  string `toml:"ca_key"`

	// CaStorageDir is the auto-gen materialization directory. Empty
	// = `<cache.dir>/ca`. Holds ca.crt, ca.key, ca.ready, .ca.lock.
	// SPEC6 §4.2.
	CaStorageDir string `toml:"ca_storage_dir"`

	// CertCacheSize bounds the in-memory leaf-cert LRU. Must be ≥ 1.
	// Default 256.
	CertCacheSize int `toml:"cert_cache_size"`

	// LeafCertLifetime is the validity window stamped onto every
	// signed leaf. Range [5m, 5y]. Default 720h (30 days). 5y upper
	// bound exists because leaf revocation is "flush the in-memory
	// cache via daemon restart" — no CRL/OCSP path.
	LeafCertLifetime Duration `toml:"leaf_cert_lifetime"`

	// CACertLifetime is the validity window of the auto-generated
	// CA. Range [1d, 50y]. Default 87600h (10 years). Has no effect
	// on the operator-supplied path.
	CACertLifetime Duration `toml:"ca_cert_lifetime"`

	// LeafAlgorithm selects the leaf cert key type. SPEC6 §5.1.3
	// only blesses two values: "ecdsa-p256" (default) and "rsa2048"
	// (legacy clients). Any other value rejected at validation.
	LeafAlgorithm string `toml:"leaf_algorithm"`

	// AllowedHostRegex is the §5.1.2 signing predicate AND, when
	// translatable per §5.1.1.1, the source of the auto-generated
	// CA's RFC 5280 Name Constraints. Empty = no MITM-side narrowing
	// (the upstream fetch gate alone applies). Must compile as a
	// Go RE2 regex if non-empty.
	AllowedHostRegex string `toml:"allowed_host_regex"`

	// AllowUnconstrainedCA opts the auto-generated CA path into
	// running without RFC 5280 Name Constraints when the regex is
	// empty or untranslatable. Default false (fail-closed per
	// §5.1.1). Has no effect on operator-supplied CAs.
	AllowUnconstrainedCA bool `toml:"allow_unconstrained_ca"`
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
// be written as "30s", "5m", "2d", "1d12h", etc. Go's time.ParseDuration
// has no "d" unit; we pre-rewrite any "<number>d" segments to their hour
// equivalent (×24) and let ParseDuration handle the rest.
type Duration struct {
	time.Duration
}

// durationDayRe matches a decimal number (with optional fractional part)
// immediately followed by the "d" unit. The unit set understood by
// time.ParseDuration (ns, us, µs, ms, s, m, h) contains no "d", so the
// rewrite is unambiguous.
var durationDayRe = regexp.MustCompile(`(\d+(?:\.\d+)?)d`)

func (d *Duration) UnmarshalText(text []byte) error {
	orig := string(text)
	s := durationDayRe.ReplaceAllStringFunc(orig, func(m string) string {
		n, err := strconv.ParseFloat(m[:len(m)-1], 64)
		if err != nil {
			return m
		}
		return strconv.FormatFloat(n*24, 'f', -1, 64) + "h"
	})
	parsed, err := time.ParseDuration(s)
	if err != nil {
		// Report the operator-supplied value, not the post-rewrite form:
		// "1day" becomes "24hay" after the day-rewrite pass and the raw
		// time.ParseDuration error mentions the rewritten string, which
		// is confusing in operator-facing diagnostics.
		return fmt.Errorf("invalid duration %q: %w", orig, err)
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
	// SPEC5 §5.2 presence-sensitive defaults for [admin]. Non-bool
	// ints/durations need IsDefined-gated defaults so an operator's
	// explicit value (or explicit zero — though zero is invalid for
	// most of these) survives Defaults().
	if !md.IsDefined("admin", "listen") {
		cfg.Admin.Listen = "127.0.0.1:6789"
	}
	if !md.IsDefined("admin", "gauge_refresh") {
		cfg.Admin.GaugeRefresh.Duration = 30 * time.Second
	}
	if !md.IsDefined("admin", "read_timeout") {
		cfg.Admin.ReadTimeout.Duration = 5 * time.Second
	}
	if !md.IsDefined("admin", "idle_timeout") {
		cfg.Admin.IdleTimeout.Duration = 30 * time.Second
	}
	if !md.IsDefined("admin", "metric_series_cap") {
		cfg.Admin.MetricSeriesCap = 1024
	}
	// SPEC6 §5.2 presence-sensitive defaults for [tls_mitm]. None of
	// these fields document a 0 / "" semantic — the spec rejects 0
	// outright for cert_cache_size and lifetimes, and rejects ""
	// for leaf_algorithm. Routing through IsDefined preserves an
	// operator's explicit (and invalid) value so Validate() can
	// surface it as a config error rather than Defaults() silently
	// rescuing the zero to 256 / 720h / 87600h / "ecdsa-p256".
	if !md.IsDefined("tls_mitm", "cert_cache_size") {
		cfg.TlsMitm.CertCacheSize = 256
	}
	if !md.IsDefined("tls_mitm", "leaf_cert_lifetime") {
		cfg.TlsMitm.LeafCertLifetime.Duration = 720 * time.Hour
	}
	if !md.IsDefined("tls_mitm", "ca_cert_lifetime") {
		cfg.TlsMitm.CACertLifetime.Duration = 87600 * time.Hour
	}
	if !md.IsDefined("tls_mitm", "leaf_algorithm") {
		cfg.TlsMitm.LeafAlgorithm = "ecdsa-p256"
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
		Upstream: UpstreamConfig{
			// upstream.allow_https_to_http_redirect defaults true.
			// Same bool-pre-populate rationale as gc.enabled / admin.enabled
			// — no zero-value sentinel can distinguish "absent" from
			// "explicit false." Operators opt out of following downgrade
			// redirects with `allow_https_to_http_redirect = false`.
			AllowHTTPSToHTTPRedirect: true,
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
		Admin: AdminConfig{
			// SPEC5 §5.1: admin.enabled defaults true. Same
			// bool-pre-populate rationale as gc.enabled — no zero-
			// value sentinel can distinguish "absent" from "explicit
			// false." Operators opt out with explicit
			// `enabled = false`, which fires admin_disabled Warn.
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

	// SPEC6 §14.2 cache.advertise_host: empty default; otherwise must
	// parse as a host or host:port (no scheme, no path). Affects only
	// `--print-apt-conf`; never read by the request-handling daemon.
	if c.Cache.AdvertiseHost != "" {
		if err := validateAdvertiseHost(c.Cache.AdvertiseHost); err != nil {
			errs = append(errs, fmt.Errorf("cache.advertise_host %q: %w", c.Cache.AdvertiseHost, err))
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

	// SPEC6_5 §5.2: [adoption].architectures shape and cardinality.
	if len(c.Adoption.Architectures) > MaxArchitecturesEntries {
		errs = append(errs, fmt.Errorf("adoption.architectures: architectures_too_many (%d entries; max %d)",
			len(c.Adoption.Architectures), MaxArchitecturesEntries))
	}
	for _, arch := range c.Adoption.Architectures {
		if !architectureNameRE.MatchString(arch) {
			errs = append(errs, fmt.Errorf("adoption.architectures: architectures_invalid_value %q (expected lowercase letters/digits, must start with a letter)", arch))
		}
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
		// blob.refcount_zeroed_at every heartbeat_interval. We need
		// 2 × heartbeat_interval < blob_grace, not just
		// heartbeat_interval < blob_grace: a single missed heartbeat
		// (writer-queue stall, transient DB lock — both observable
		// via adoption_heartbeat_blobs_failed) extends the gap to
		// 2 × heartbeat_interval, and that worst-case gap must still
		// fit inside the grace window. With only the weaker bound
		// (e.g. heartbeat_interval=4m, blob_grace=5m), one missed
		// heartbeat lets a member blob age 8m → past grace → reaped
		// before CommitAdoption Step 4 can bump refcount. The
		// stricter bound preserves safety across one missed
		// heartbeat without operator action; further missed
		// heartbeats are signalled at Warn so the operator notices
		// before grace is at risk.
		if c.GC.BlobGrace.Duration > 0 && 2*c.GC.HeartbeatInterval.Duration >= c.GC.BlobGrace.Duration {
			errs = append(errs, fmt.Errorf(
				"gc.heartbeat_interval (%s) × 2 must be strictly less than gc.blob_grace (%s) — i.e. heartbeat_interval < blob_grace/2 — so one missed heartbeat (writer-queue stall, DB lock) does not let an in-flight member blob age past grace before the next refresh",
				c.GC.HeartbeatInterval.Duration, c.GC.BlobGrace.Duration))
		}
	}

	// SPEC5 §5.2: [admin] block validation. Only when admin.enabled
	// is true — when disabled, the listener is not bound, htpasswd
	// is not parsed, the refresher does not run, so per-key
	// constraints are inert. Validating only-when-enabled mirrors
	// gc.enabled gating in §5.2 (see SPEC4 §5.2).
	if c.Admin.Enabled {
		if err := validateListenAddr(c.Admin.Listen); err != nil {
			errs = append(errs, fmt.Errorf("admin.listen: %w", err))
		}
		if c.Admin.HtpasswdFile != "" {
			if err := validateHtpasswdFile(c.Admin.HtpasswdFile); err != nil {
				errs = append(errs, fmt.Errorf("admin.htpasswd_file: %w", err))
			}
		}
		if c.Admin.GaugeRefresh.Duration <= 0 {
			errs = append(errs, errors.New("admin.gauge_refresh must be > 0"))
		} else if c.Admin.GaugeRefresh.Duration > time.Hour {
			errs = append(errs, errors.New("admin.gauge_refresh must be <= 1h"))
		}
		if c.Admin.ReadTimeout.Duration <= 0 {
			errs = append(errs, errors.New("admin.read_timeout must be > 0"))
		} else if c.Admin.ReadTimeout.Duration > time.Minute {
			errs = append(errs, errors.New("admin.read_timeout must be <= 1m"))
		}
		if c.Admin.IdleTimeout.Duration <= 0 {
			errs = append(errs, errors.New("admin.idle_timeout must be > 0"))
		} else if c.Admin.IdleTimeout.Duration > 10*time.Minute {
			errs = append(errs, errors.New("admin.idle_timeout must be <= 10m"))
		}
		if c.Admin.MetricSeriesCap < 1 {
			errs = append(errs, errors.New("admin.metric_series_cap must be >= 1"))
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

	// SPEC6 §5.2 — [tls_mitm] block. Only checked when enabled=true;
	// disabled keeps fields unvalidated so a future config staging
	// upgrade path with `enabled = false` is silent (§5.2 last
	// paragraph). Cert/key file CONTENT validation (parse, IsCA,
	// not_after, key match, supported algorithm) lives in
	// internal/proxy/tlsmitm/ca.go validateSuppliedCA — config-load
	// time only checks file readability so the failure mode at
	// startup is "config error" rather than "panic at first
	// CONNECT".
	if c.TlsMitm.Enabled {
		if c.TlsMitm.CertCacheSize < 1 {
			errs = append(errs, fmt.Errorf("tls_mitm.cert_cache_size %d must be >= 1", c.TlsMitm.CertCacheSize))
		}
		const (
			minLeafLifetime = 5 * time.Minute
			maxLeafLifetime = 5 * 365 * 24 * time.Hour
			minCALifetime   = 24 * time.Hour
			maxCALifetime   = 50 * 365 * 24 * time.Hour
		)
		if c.TlsMitm.LeafCertLifetime.Duration < minLeafLifetime || c.TlsMitm.LeafCertLifetime.Duration > maxLeafLifetime {
			errs = append(errs, fmt.Errorf(
				"tls_mitm.leaf_cert_lifetime %s out of range [%s, %s]",
				c.TlsMitm.LeafCertLifetime.Duration, minLeafLifetime, maxLeafLifetime))
		}
		if c.TlsMitm.CACertLifetime.Duration < minCALifetime || c.TlsMitm.CACertLifetime.Duration > maxCALifetime {
			errs = append(errs, fmt.Errorf(
				"tls_mitm.ca_cert_lifetime %s out of range [%s, %s]",
				c.TlsMitm.CACertLifetime.Duration, minCALifetime, maxCALifetime))
		}
		switch c.TlsMitm.LeafAlgorithm {
		case "ecdsa-p256", "rsa2048":
		default:
			errs = append(errs, fmt.Errorf(
				"tls_mitm.leaf_algorithm %q invalid (ecdsa-p256|rsa2048)", c.TlsMitm.LeafAlgorithm))
		}
		if c.TlsMitm.AllowedHostRegex != "" {
			if _, err := regexp.Compile(c.TlsMitm.AllowedHostRegex); err != nil {
				errs = append(errs, fmt.Errorf("tls_mitm.allowed_host_regex: %w", err))
			}
		}
		// Both-set-or-both-empty rule per SPEC6 §5.2.
		caCertSet := c.TlsMitm.CaCert != ""
		caKeySet := c.TlsMitm.CaKey != ""
		if caCertSet != caKeySet {
			errs = append(errs, errors.New("tls_mitm.ca_cert and tls_mitm.ca_key must both be set or both empty"))
		} else if caCertSet {
			if err := checkReadableFile(c.TlsMitm.CaCert); err != nil {
				errs = append(errs, fmt.Errorf("tls_mitm.ca_cert %q: %w", c.TlsMitm.CaCert, err))
			}
			if err := checkReadableFile(c.TlsMitm.CaKey); err != nil {
				errs = append(errs, fmt.Errorf("tls_mitm.ca_key %q: %w", c.TlsMitm.CaKey, err))
			}
		}
		// Auto-gen path: storage dir must be creatable. The fail-closed
		// rule (allow_unconstrained_ca + empty/untranslatable regex)
		// is enforced at LoadOrGenerate time, not here, because
		// translatability depends on regexp/syntax inspection that the
		// tlsmitm package owns. Config-time validation just guarantees
		// the storage dir works.
		if !caCertSet {
			dir := c.TlsMitm.CaStorageDir
			if dir == "" {
				dir = filepath.Join(c.Cache.Dir, "ca")
			}
			if err := checkCreatableDir(dir); err != nil {
				errs = append(errs, fmt.Errorf("tls_mitm.ca_storage_dir %q: %w", dir, err))
			}
		}
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

// validateAdvertiseHost accepts "host" or "host:port" — no scheme, no
// path. Used by SPEC6 §14.2 to validate cache.advertise_host. The host
// portion is permissive (any non-empty token without ":" or "/"); the
// port portion, when present, must be a numeric value in 1-65535.
func validateAdvertiseHost(s string) error {
	if strings.Contains(s, "://") {
		return fmt.Errorf("must not contain a scheme")
	}
	if strings.Contains(s, "/") {
		return fmt.Errorf("must not contain a path")
	}
	// Bracketed IPv6: "[::1]" or "[::1]:3142".
	if strings.HasPrefix(s, "[") {
		end := strings.Index(s, "]")
		if end < 0 {
			return fmt.Errorf("unbalanced bracket in IPv6 literal")
		}
		host := s[1:end]
		if host == "" {
			return fmt.Errorf("empty IPv6 literal")
		}
		rest := s[end+1:]
		if rest == "" {
			return nil
		}
		if !strings.HasPrefix(rest, ":") {
			return fmt.Errorf("expected ':port' after IPv6 literal, got %q", rest)
		}
		return validatePortString(rest[1:])
	}
	// Bare host or host:port. SplitHostPort fails on a host with no
	// port, so we test that shape first.
	if !strings.Contains(s, ":") {
		return nil
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return fmt.Errorf("invalid host:port: %w", err)
	}
	if host == "" {
		return fmt.Errorf("empty host")
	}
	return validatePortString(port)
}

func validatePortString(port string) error {
	if port == "" {
		return fmt.Errorf("empty port")
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

// checkCreatableDir reports whether `dir` is usable as a future
// daemon-managed directory. Two cases are accepted:
//  1. `dir` already exists, is a directory, and is writable.
//  2. `dir` does not yet exist; its parent exists, is a directory,
//     and is writable (so a later os.MkdirAll call will succeed).
//
// SPEC6 §5.2 calls for `tls_mitm.ca_storage_dir` to be "creatable" —
// the daemon will MkdirAll on first start (under the §4.2.2 flock).
// Config-load validation surfaces the obvious failure modes (typo'd
// path under a read-only parent, parent doesn't exist) up-front.
func checkCreatableDir(dir string) error {
	if dir == "" {
		return errors.New("empty path")
	}
	if info, err := os.Stat(dir); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("exists but is not a directory")
		}
		return checkWritable(dir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(dir)
	info, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("parent %q: %w", parent, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("parent %q is not a directory", parent)
	}
	return checkWritable(parent)
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
	closeErr := probe.Close()
	// Always attempt the unlink, even if Close failed — leaving a
	// stray probe file behind would be worse than the close error.
	rmErr := os.Remove(name)
	if closeErr != nil {
		return fmt.Errorf("not writable: %w", closeErr)
	}
	return rmErr
}

// validateHtpasswdFile parses the given path's contents as Apache
// htpasswd format with bcrypt-only entries. SPEC5 §9.7.5 / §5.2:
// each line is either empty, comment-only (`#...`), or
// `user:hash` where hash starts with $2a$, $2b$, or $2y$. Any other
// hash prefix ($apr1$ Apache MD5, {SHA} SHA-1, crypt(3) DES) is
// rejected at startup so a weak credential scheme cannot be used
// silently.
//
// AIDEV-NOTE: this is the startup-time validation; the runtime
// reload in internal/admin uses the same parser shape so a
// successful startup parse implies subsequent reload-of-same-content
// works. Returns an error naming the first offending line so
// operators can fix it without scanning the whole file.
func validateHtpasswdFile(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	users := 0
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			return fmt.Errorf("line %d: missing user:hash separator", i+1)
		}
		user := line[:colon]
		hash := line[colon+1:]
		if user == "" {
			return fmt.Errorf("line %d: empty username", i+1)
		}
		// User must not contain whitespace. Apache's htpasswd does
		// not allow it, and our middleware splits on the first
		// colon — embedded whitespace would silently authenticate
		// nobody.
		if strings.ContainsAny(user, " \t") {
			return fmt.Errorf("line %d: username %q contains whitespace", i+1, user)
		}
		switch {
		case strings.HasPrefix(hash, "$2a$"),
			strings.HasPrefix(hash, "$2b$"),
			strings.HasPrefix(hash, "$2y$"):
			// Acceptable bcrypt prefixes.
		case strings.HasPrefix(hash, "$apr1$"):
			return fmt.Errorf("line %d: Apache MD5 ($apr1$) hash rejected — use bcrypt (`htpasswd -B`)", i+1)
		case strings.HasPrefix(hash, "{SHA}"):
			return fmt.Errorf("line %d: SHA-1 ({SHA}) hash rejected — use bcrypt (`htpasswd -B`)", i+1)
		default:
			return fmt.Errorf("line %d: unrecognized hash format %q — only bcrypt ($2a$/$2b$/$2y$) is accepted", i+1, hash)
		}
		users++
	}
	if users == 0 {
		return fmt.Errorf("no users defined")
	}
	return nil
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

	// SPEC6 §5.1: [tls_mitm] defaults are applied in Load() via
	// TOML's MetaData.IsDefined so an operator's explicit 0 / ""
	// (which Validate rejects loudly) is not silently rescued to
	// the SPEC default. Bool fields (Enabled, AllowUnconstrainedCA)
	// default to false at the Go zero-value level, no pre-population
	// needed. Path fields (CaCert, CaKey, AllowedHostRegex) default
	// to empty. CaStorageDir resolves to <cache.dir>/ca at runtime
	// via EffectiveCaStorageDir() if empty — Defaults() leaves it
	// alone so operators see the effective path in startup logs
	// without it being silently expanded into the config struct.
	//
	// This method (called by non-Load callers without a TOML source)
	// cannot distinguish "explicit 0" from "absent" so it
	// deliberately leaves these fields alone — same rationale as
	// the §5.1 max_concurrent_adoptions / validate_at_rest_interval
	// presence-sensitive comment above.
}

// EffectiveCaStorageDir returns the SPEC6 §5.1 ca_storage_dir for
// auto-generation: the operator-supplied path if set, otherwise
// `<cache.dir>/ca`. Callers that need to know the on-disk location
// (daemon main, `ca print` subcommand, status page) use this rather
// than re-deriving the default.
func (c *Config) EffectiveCaStorageDir() string {
	if c.TlsMitm.CaStorageDir != "" {
		return c.TlsMitm.CaStorageDir
	}
	if c.Cache.Dir == "" {
		return ""
	}
	return filepath.Join(c.Cache.Dir, "ca")
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
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}
