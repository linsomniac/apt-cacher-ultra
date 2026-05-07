package config

import (
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func writeTOML(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestLoad_Minimal(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "config.toml", `
[cache]
dir = "`+dir+`"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Cache.Listen != "0.0.0.0:3142" {
		t.Errorf("listen default not applied: %q", cfg.Cache.Listen)
	}
	// Security defaults must be populated even from a minimal config (§6.6).
	if len(cfg.Upstream.AllowedHostRegex) == 0 {
		t.Errorf("AllowedHostRegex was not populated by Defaults")
	}
	if len(cfg.Upstream.DenyTargetRanges) == 0 {
		t.Errorf("DenyTargetRanges was not populated by Defaults")
	}
	// SPEC §5.1 [serve] defaults — bools default to true when [serve]
	// is absent. Without pre-populating before decode, omitted bool keys
	// silently land on the zero value (false), and the SPEC §6.4
	// HIT-STALE behavior would never fire in production unless an
	// operator explicitly enabled it.
	if !cfg.Serve.ServeStaleWhenUpstreamDown {
		t.Errorf("serve_stale_when_upstream_down default not applied")
	}
	if !cfg.Serve.LogStaleServes {
		t.Errorf("log_stale_serves default not applied")
	}
}

// TestLoad_ServeFlagsExplicitFalseRespected proves that an explicit
// `false` in TOML survives the pre-population. The pre-decode seed sets
// the SPEC default to true, but the decoder must overwrite that for any
// key actually present in the file — otherwise operators have no way to
// disable the feature.
func TestLoad_ServeFlagsExplicitFalseRespected(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "config.toml", `
[cache]
dir = "`+dir+`"
[serve]
serve_stale_when_upstream_down = false
log_stale_serves               = false
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Serve.ServeStaleWhenUpstreamDown {
		t.Errorf("explicit serve_stale_when_upstream_down=false was clobbered to true")
	}
	if cfg.Serve.LogStaleServes {
		t.Errorf("explicit log_stale_serves=false was clobbered to true")
	}
}

// TestLoad_ServeFlagsExplicitTrueRespected covers the trivial third
// case: a TOML that explicitly sets the flags to true is unchanged.
// This guards against a future Load refactor that flipped the seed
// value to false (which would silently break the omitted-key path
// covered by TestLoad_Minimal but pass any test that explicitly set
// true).
func TestLoad_ServeFlagsExplicitTrueRespected(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "config.toml", `
[cache]
dir = "`+dir+`"
[serve]
serve_stale_when_upstream_down = true
log_stale_serves               = true
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Serve.ServeStaleWhenUpstreamDown {
		t.Errorf("explicit serve_stale_when_upstream_down=true not preserved")
	}
	if !cfg.Serve.LogStaleServes {
		t.Errorf("explicit log_stale_serves=true not preserved")
	}
}

// An explicit empty list MUST be preserved — §6.6 defines [] as "deny
// everything" / "no IP-range filter", not "use defaults".
func TestLoad_ExplicitEmptyListsArePreserved(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "config.toml", `
[cache]
dir = "`+dir+`"
[upstream]
allowed_host_regex = []
deny_target_ranges = []
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Upstream.AllowedHostRegex == nil {
		t.Errorf("explicit [] should produce non-nil empty slice, got nil")
	}
	if len(cfg.Upstream.AllowedHostRegex) != 0 {
		t.Errorf("explicit [] should not be replaced by defaults: %v", cfg.Upstream.AllowedHostRegex)
	}
	if len(cfg.Upstream.DenyTargetRanges) != 0 {
		t.Errorf("explicit [] should not be replaced by defaults: %v", cfg.Upstream.DenyTargetRanges)
	}
}

// Sanity-check that the package-level defaults themselves are valid: every
// entry should compile / parse. This catches typos in the constants above.
func TestPackageDefaultsAreValid(t *testing.T) {
	for i, re := range DefaultAllowedHostRegex {
		if _, err := regexp.Compile(re); err != nil {
			t.Errorf("DefaultAllowedHostRegex[%d] %q does not compile: %v", i, re, err)
		}
	}
	for i, cidr := range DefaultDenyTargetRanges {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			t.Errorf("DefaultDenyTargetRanges[%d] %q does not parse: %v", i, cidr, err)
		}
	}
}

func TestLoad_FullSpecExample(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "config.toml", `
[cache]
dir = "`+dir+`"
listen = "0.0.0.0:3142"

[upstream]
connect_timeout         = "30s"
total_timeout           = "5m"
idle_read_timeout       = "60s"
max_retries             = 3
max_concurrent_per_host = 8
allowed_host_regex      = ['^archive\.ubuntu\.com$']
deny_target_ranges      = ["127.0.0.0/8", "10.0.0.0/8"]

[freshness]
cooldown          = "60s"
periodic_refresh  = "15m"

[serve]
serve_stale_when_upstream_down = true
log_stale_serves               = true

[log]
level  = "info"
format = "json"

[[remap]]
match_host_regex = '^([a-z]{2}\.)?archive\.ubuntu\.com$'
canonical_host   = "archive.ubuntu.com"

[[mirror]]
prefix   = "/corretto"
upstream = "https://apt.corretto.aws/"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Upstream.ConnectTimeout.Duration != 30*time.Second {
		t.Errorf("connect_timeout = %v", cfg.Upstream.ConnectTimeout.Duration)
	}
	if got := len(cfg.Remap); got != 1 {
		t.Errorf("remap entries = %d, want 1", got)
	}
	if got := len(cfg.Mirror); got != 1 {
		t.Errorf("mirror entries = %d, want 1", got)
	}
}

func TestLoad_InvalidConfigs(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "missing cache.dir",
			body:    `[cache]` + "\n" + `listen = "0.0.0.0:3142"`,
			wantErr: "cache.dir",
		},
		{
			name: "cache.listen invalid host:port",
			body: `[cache]
dir = "DIR"
listen = "not-a-host-port"
`,
			wantErr: "cache.listen",
		},
		{
			name: "cache.listen port out of range",
			body: `[cache]
dir = "DIR"
listen = "0.0.0.0:99999"
`,
			wantErr: "out of range",
		},
		{
			name: "tls half-set",
			body: `[cache]
dir = "DIR"
listen = "0.0.0.0:3142"
listen_tls = "0.0.0.0:3443"
`,
			wantErr: "tls_cert",
		},
		{
			name: "remap regex bad",
			body: `[cache]
dir = "DIR"
listen = "0.0.0.0:3142"
[[remap]]
match_host_regex = "[unclosed"
canonical_host = "x"
`,
			wantErr: "match_host_regex",
		},
		{
			name: "deny_target_ranges bad CIDR",
			body: `[cache]
dir = "DIR"
listen = "0.0.0.0:3142"
[upstream]
deny_target_ranges = ["not-a-cidr"]
`,
			wantErr: "deny_target_ranges",
		},
		{
			name: "allowed_host_regex bad",
			body: `[cache]
dir = "DIR"
listen = "0.0.0.0:3142"
[upstream]
allowed_host_regex = ["[unclosed"]
`,
			wantErr: "allowed_host_regex",
		},
		{
			name: "mirror prefix missing slash",
			body: `[cache]
dir = "DIR"
listen = "0.0.0.0:3142"
[[mirror]]
prefix = "ubuntu"
upstream = "http://archive.ubuntu.com/ubuntu/"
`,
			wantErr: "must start with /",
		},
		{
			name: "duplicate mirror prefix",
			body: `[cache]
dir = "DIR"
listen = "0.0.0.0:3142"
[[mirror]]
prefix = "/x"
upstream = "http://a/"
[[mirror]]
prefix = "/x"
upstream = "http://b/"
`,
			wantErr: "overlaps",
		},
		{
			name: "overlapping mirror prefixes",
			body: `[cache]
dir = "DIR"
listen = "0.0.0.0:3142"
[[mirror]]
prefix = "/ubuntu"
upstream = "http://archive.ubuntu.com/"
[[mirror]]
prefix = "/ubuntu/dists"
upstream = "http://other/"
`,
			wantErr: "overlaps",
		},
		{
			name: "negative duration",
			body: `[cache]
dir = "DIR"
listen = "0.0.0.0:3142"
[upstream]
connect_timeout = "-30s"
`,
			wantErr: "connect_timeout",
		},
		{
			name: "negative max_concurrent_per_host",
			body: `[cache]
dir = "DIR"
listen = "0.0.0.0:3142"
[upstream]
max_concurrent_per_host = -1
`,
			wantErr: "max_concurrent_per_host",
		},
		{
			name: "tls cert path missing",
			body: `[cache]
dir = "DIR"
listen = "0.0.0.0:3142"
listen_tls = "0.0.0.0:3443"
tls_cert = "/nonexistent/cert.pem"
tls_key = "/nonexistent/key.pem"
`,
			wantErr: "tls_cert",
		},
		{
			name: "log level invalid",
			body: `[cache]
dir = "DIR"
listen = "0.0.0.0:3142"
[log]
level = "verbose"
`,
			wantErr: "log.level",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			body := strings.ReplaceAll(tc.body, "DIR", dir)
			path := writeTOML(t, dir, "config.toml", body)
			_, err := Load(path)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestDefaults(t *testing.T) {
	cfg := &Config{}
	cfg.Defaults()
	if cfg.Cache.Listen != "0.0.0.0:3142" {
		t.Errorf("default listen = %q", cfg.Cache.Listen)
	}
	if cfg.Upstream.ConnectTimeout.Duration != 30*time.Second {
		t.Errorf("default connect_timeout = %v", cfg.Upstream.ConnectTimeout.Duration)
	}
	if cfg.Upstream.MaxConcurrentPerHost != 8 {
		t.Errorf("default max_concurrent_per_host = %d", cfg.Upstream.MaxConcurrentPerHost)
	}
	if cfg.Freshness.Cooldown.Duration != 60*time.Second {
		t.Errorf("default cooldown = %v", cfg.Freshness.Cooldown.Duration)
	}
	if cfg.Freshness.PeriodicRefresh.Duration != 15*time.Minute {
		t.Errorf("default periodic_refresh = %v", cfg.Freshness.PeriodicRefresh.Duration)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("default log.level = %q", cfg.Log.Level)
	}
	if cfg.Log.Format != "json" {
		t.Errorf("default log.format = %q", cfg.Log.Format)
	}
	if !reflect.DeepEqual(cfg.Upstream.AllowedHostRegex, DefaultAllowedHostRegex) {
		t.Errorf("default allowed_host_regex not applied")
	}
	if !reflect.DeepEqual(cfg.Upstream.DenyTargetRanges, DefaultDenyTargetRanges) {
		t.Errorf("default deny_target_ranges not applied")
	}
}

func TestDefaults_DoNotOverrideSet(t *testing.T) {
	cfg := &Config{}
	cfg.Cache.Listen = "127.0.0.1:9999"
	cfg.Upstream.MaxConcurrentPerHost = 16
	cfg.Defaults()
	if cfg.Cache.Listen != "127.0.0.1:9999" {
		t.Errorf("listen overridden: %q", cfg.Cache.Listen)
	}
	if cfg.Upstream.MaxConcurrentPerHost != 16 {
		t.Errorf("max_concurrent_per_host overridden: %d", cfg.Upstream.MaxConcurrentPerHost)
	}
}

// TestPhase2_AdoptionDefaults — defaults match SPEC2 §5.1's secure
// posture: require_signature defaults true; enabled and
// require_pinned_signer default false during rollout.
func TestPhase2_AdoptionDefaults(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "config.toml", `[cache]
dir = "`+dir+`"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Adoption.Enabled {
		t.Errorf("adoption.enabled default should be false")
	}
	if !cfg.Adoption.RequireSignature {
		t.Errorf("adoption.require_signature default should be true")
	}
	if cfg.Adoption.RequirePinnedSigner {
		t.Errorf("adoption.require_pinned_signer default should be false")
	}
	if cfg.Integrity.ValidateAtRestInterval.Duration != 24*time.Hour {
		t.Errorf("integrity.validate_at_rest_interval default = %v, want 24h",
			cfg.Integrity.ValidateAtRestInterval.Duration)
	}
	if cfg.Integrity.ValidateAtRestWorkers != 4 {
		t.Errorf("integrity.validate_at_rest_workers default = %d, want 4",
			cfg.Integrity.ValidateAtRestWorkers)
	}
}

// TestPhase2_AdoptionLoadsTOML — every Phase 2 key parses correctly,
// including the [[trusted_signer]] array.
func TestPhase2_AdoptionLoadsTOML(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "config.toml", `[cache]
dir = "`+dir+`"

[freshness]
max_concurrent_adoptions = 8

[adoption]
enabled = true
require_signature = false
require_pinned_signer = true

[integrity]
validate_at_rest_interval = "12h"
validate_at_rest_workers = 2

[[trusted_signer]]
match_canonical_host = '^archive\.ubuntu\.com$'
fingerprints = ['F6ECB3762474EDA9D21B7022871920D1991BC93C']

[[trusted_signer]]
match_canonical_host = '^deb\.debian\.org$'
fingerprints = ['648ACFD622F3D138B83D49C7DDF4D7C5C5E3A7B6', '0123456789abcdef0123456789ABCDEF01234567']
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Freshness.MaxConcurrentAdoptions != 8 {
		t.Errorf("max_concurrent_adoptions = %d, want 8", cfg.Freshness.MaxConcurrentAdoptions)
	}
	if !cfg.Adoption.Enabled || cfg.Adoption.RequireSignature || !cfg.Adoption.RequirePinnedSigner {
		t.Errorf("adoption: %+v not parsed correctly", cfg.Adoption)
	}
	if cfg.Integrity.ValidateAtRestInterval.Duration != 12*time.Hour {
		t.Errorf("interval = %v, want 12h", cfg.Integrity.ValidateAtRestInterval.Duration)
	}
	if cfg.Integrity.ValidateAtRestWorkers != 2 {
		t.Errorf("workers = %d, want 2", cfg.Integrity.ValidateAtRestWorkers)
	}
	if len(cfg.TrustedSigners) != 2 {
		t.Fatalf("trusted_signer count = %d, want 2", len(cfg.TrustedSigners))
	}
	if cfg.TrustedSigners[0].MatchCanonicalHost != `^archive\.ubuntu\.com$` {
		t.Errorf("ts[0] regex = %q", cfg.TrustedSigners[0].MatchCanonicalHost)
	}
	if len(cfg.TrustedSigners[0].Fingerprints) != 1 {
		t.Errorf("ts[0] fp count = %d", len(cfg.TrustedSigners[0].Fingerprints))
	}
	if len(cfg.TrustedSigners[1].Fingerprints) != 2 {
		t.Errorf("ts[1] fp count = %d", len(cfg.TrustedSigners[1].Fingerprints))
	}
}

func TestPhase2_RejectsBadValues(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			"negative max_concurrent_adoptions",
			"[cache]\ndir=\"%DIR%\"\n[freshness]\nmax_concurrent_adoptions = -1\n",
			"max_concurrent_adoptions",
		},
		{
			"negative validate_at_rest_interval",
			"[cache]\ndir=\"%DIR%\"\n[integrity]\nvalidate_at_rest_interval = \"-1h\"\n",
			"validate_at_rest_interval",
		},
		{
			"negative validate_at_rest_workers",
			"[cache]\ndir=\"%DIR%\"\n[integrity]\nvalidate_at_rest_workers = -1\n",
			"validate_at_rest_workers",
		},
		{
			// SPEC2 §5.2: workers >= 1 when interval > 0. Load uses
			// MetaData.IsDefined to preserve explicit 0, so this
			// rejection actually fires.
			"validate_at_rest_workers must be >= 1 when interval > 0",
			"[cache]\ndir=\"%DIR%\"\n[integrity]\nvalidate_at_rest_interval = \"1h\"\nvalidate_at_rest_workers = 0\n",
			"validate_at_rest_workers",
		},
		{
			"trusted_signer empty fingerprints",
			"[cache]\ndir=\"%DIR%\"\n[[trusted_signer]]\nmatch_canonical_host = \".*\"\nfingerprints = []\n",
			"fingerprints is empty",
		},
		{
			"trusted_signer short-form fingerprint",
			"[cache]\ndir=\"%DIR%\"\n[[trusted_signer]]\nmatch_canonical_host = \".*\"\nfingerprints = [\"DEADBEEF\"]\n",
			"40 hex chars",
		},
		{
			"trusted_signer non-hex fingerprint",
			"[cache]\ndir=\"%DIR%\"\n[[trusted_signer]]\nmatch_canonical_host = \".*\"\nfingerprints = [\"" + strings.Repeat("Z", 40) + "\"]\n",
			"40 hex chars",
		},
		{
			"trusted_signer bad regex",
			"[cache]\ndir=\"%DIR%\"\n[[trusted_signer]]\nmatch_canonical_host = \"(\"\nfingerprints = [\"" + strings.Repeat("a", 40) + "\"]\n",
			"trusted_signer",
		},
		{
			"trusted_signer empty match",
			"[cache]\ndir=\"%DIR%\"\n[[trusted_signer]]\nmatch_canonical_host = \"\"\nfingerprints = [\"" + strings.Repeat("a", 40) + "\"]\n",
			"match_canonical_host is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			body := strings.ReplaceAll(tc.body, "%DIR%", dir)
			path := writeTOML(t, dir, "c.toml", body)
			_, err := Load(path)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

// TestPhase2_MaxConcurrentZeroIsValid — operators who explicitly
// want unlimited concurrency set 0; this must NOT be a validation
// error (the SPEC2 §9.3.1 documented meaning is "no cap"), and
// the explicit value must survive Defaults() unchanged.
func TestPhase2_MaxConcurrentZeroIsValid(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "config.toml", `[cache]
dir = "`+dir+`"

[freshness]
max_concurrent_adoptions = 0
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Freshness.MaxConcurrentAdoptions != 0 {
		t.Errorf("explicit 0 not preserved: got %d", cfg.Freshness.MaxConcurrentAdoptions)
	}
}

// TestPhase2_MaxConcurrentAbsentDefaultsToTwo — SPEC2 §9.3.1 documents
// "default 2, 0 = unlimited." A config with no explicit value must
// inherit the default-2, distinguishing it from an operator-chosen 0.
// Driven by TOML's MetaData.IsDefined in Load().
func TestPhase2_MaxConcurrentAbsentDefaultsToTwo(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "config.toml", `[cache]
dir = "`+dir+`"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Freshness.MaxConcurrentAdoptions != 2 {
		t.Errorf("absent max_concurrent_adoptions = %d, want 2 (SPEC2 §9.3.1 default)",
			cfg.Freshness.MaxConcurrentAdoptions)
	}
}

// TestPhase2_IntegrityIntervalZeroDisablesScan — SPEC2 §5.1 documents
// "0 = disabled" for validate_at_rest_interval. An explicit 0 must
// survive Defaults() unchanged so the operator can actually disable
// the scan; previously Defaults() rewrote any 0 to 24h, making
// "disabled" unreachable.
func TestPhase2_IntegrityIntervalZeroDisablesScan(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "config.toml", `[cache]
dir = "`+dir+`"

[integrity]
validate_at_rest_interval = "0s"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Integrity.ValidateAtRestInterval.Duration != 0 {
		t.Errorf("explicit 0 interval not preserved: got %v",
			cfg.Integrity.ValidateAtRestInterval.Duration)
	}
}

// TestUpstream_UnreachableDefaultsApplied — SPEC §1 fast-fail. An
// absent [upstream] block must inherit the default 30s cooldown / 1s
// probe timeout. Documented "0 = disable"; tested separately below.
func TestUpstream_UnreachableDefaultsApplied(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "config.toml", `[cache]
dir = "`+dir+`"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Upstream.UnreachableCooldown.Duration; got != 30*time.Second {
		t.Errorf("absent unreachable_cooldown = %v, want 30s default", got)
	}
	if got := cfg.Upstream.UnreachableProbeTimeout.Duration; got != time.Second {
		t.Errorf("absent unreachable_probe_timeout = %v, want 1s default", got)
	}
}

// TestUpstream_UnreachableExplicitZeroDisables — explicit 0 must NOT
// be rewritten to the default. 0 is the documented disable signal so
// operators can opt out of fast-fail and recover the legacy
// connect_timeout × max_retries budget.
func TestUpstream_UnreachableExplicitZeroDisables(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "config.toml", `[cache]
dir = "`+dir+`"

[upstream]
unreachable_cooldown = "0s"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Upstream.UnreachableCooldown.Duration; got != 0 {
		t.Errorf("explicit 0 cooldown not preserved: got %v", got)
	}
}

// TestPhase2_GPGFingerprintCaseSensitivity — TOML accepts both upper
// and lower hex case; canonicalization happens at trust-set load time.
func TestPhase2_GPGFingerprintCaseSensitivity(t *testing.T) {
	for _, fp := range []string{
		strings.Repeat("a", 40),
		strings.Repeat("A", 40),
		"F6ECB3762474EDA9D21B7022871920D1991BC93C",
		"f6ecb3762474eda9d21b7022871920d1991bc93c",
	} {
		if !validGPGFingerprint(fp) {
			t.Errorf("validGPGFingerprint(%q) = false, want true", fp)
		}
	}
	for _, bad := range []string{
		"",
		"abc",
		strings.Repeat("a", 39),
		strings.Repeat("a", 41),
		strings.Repeat("g", 40),
		"  " + strings.Repeat("a", 38),
	} {
		if validGPGFingerprint(bad) {
			t.Errorf("validGPGFingerprint(%q) = true, want false", bad)
		}
	}
}

// TestLoad_HotPackagesWindowDefaultApplied: omitted hot_packages.window
// gets the SPEC3 §5.2 default of 24h.
func TestLoad_HotPackagesWindowDefaultApplied(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "config.toml", `
[cache]
dir = "`+dir+`"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HotPackages.Window.Duration != 24*time.Hour {
		t.Errorf("hot_packages.window default = %s, want 24h", cfg.HotPackages.Window.Duration)
	}
}

// TestLoad_HotPackagesWindowExplicitZeroRespected: SPEC3 §5.2 — an
// operator-written 0s must survive Defaults() (it disables proactive
// refresh). Same IsDefined pattern as Phase 2's
// freshness.max_concurrent_adoptions.
func TestLoad_HotPackagesWindowExplicitZeroRespected(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "config.toml", `
[cache]
dir = "`+dir+`"
[hot_packages]
window = "0s"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HotPackages.Window.Duration != 0 {
		t.Errorf("explicit hot_packages.window=0s was clobbered to %s", cfg.HotPackages.Window.Duration)
	}
}

// TestLoad_HotPrefetchBudgetDefaultApplied: omitted
// adoption.hot_prefetch_budget gets the SPEC3 §5.2 default of 5m.
func TestLoad_HotPrefetchBudgetDefaultApplied(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "config.toml", `
[cache]
dir = "`+dir+`"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Adoption.HotPrefetchBudget.Duration != 5*time.Minute {
		t.Errorf("hot_prefetch_budget default = %s, want 5m", cfg.Adoption.HotPrefetchBudget.Duration)
	}
}

// TestLoad_HotPrefetchBudgetExplicitZeroRespected: an operator-written
// 0s for the budget disables the wall-clock guard. SPEC3 §5.2.
func TestLoad_HotPrefetchBudgetExplicitZeroRespected(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "config.toml", `
[cache]
dir = "`+dir+`"
[adoption]
hot_prefetch_budget = "0s"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Adoption.HotPrefetchBudget.Duration != 0 {
		t.Errorf("explicit hot_prefetch_budget=0s was clobbered to %s", cfg.Adoption.HotPrefetchBudget.Duration)
	}
}

// TestLoad_RefuseUnvouchedDebsDefault: omitted
// integrity.refuse_unvouched_debs defaults to false (SPEC3 §1.3 —
// the default-flip to true is gated on production data).
func TestLoad_RefuseUnvouchedDebsDefault(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "config.toml", `
[cache]
dir = "`+dir+`"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Integrity.RefuseUnvouchedDebs {
		t.Errorf("refuse_unvouched_debs = true by default; want false")
	}
}

// TestLoad_RefuseUnvouchedDebsExplicitTrue: operator opt-in is
// observed (otherwise the strict-mode predicate is unreachable).
func TestLoad_RefuseUnvouchedDebsExplicitTrue(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "config.toml", `
[cache]
dir = "`+dir+`"
[integrity]
refuse_unvouched_debs = true
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Integrity.RefuseUnvouchedDebs {
		t.Errorf("explicit refuse_unvouched_debs=true was not applied")
	}
}

// TestValidate_RejectsNegativeHotPackagesWindow: SPEC3 §5.2 — window
// must be ≥ 0.
func TestValidate_RejectsNegativeHotPackagesWindow(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "config.toml", `
[cache]
dir = "`+dir+`"
[hot_packages]
window = "-1s"
`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "hot_packages.window") {
		t.Errorf("got %v, want hot_packages.window error", err)
	}
}

// TestValidate_RejectsNegativeHotPrefetchBudget: SPEC3 §5.2 —
// hot_prefetch_budget must be ≥ 0.
func TestValidate_RejectsNegativeHotPrefetchBudget(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "config.toml", `
[cache]
dir = "`+dir+`"
[adoption]
hot_prefetch_budget = "-1s"
`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "hot_prefetch_budget") {
		t.Errorf("got %v, want hot_prefetch_budget error", err)
	}
}

// SPEC4 §12.1: gc.heartbeat_interval cross-key validation.

func TestValidate_RejectsZeroHeartbeatInterval(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "config.toml", `
[cache]
dir = "`+dir+`"
[gc]
heartbeat_interval = "0s"
`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "gc.heartbeat_interval must be > 0") {
		t.Errorf("got %v, want gc.heartbeat_interval > 0 error", err)
	}
}

// heartbeat_interval = 30m with upstream.total_timeout × max_retries = 5m
// (so grace_effective = 30m floor) → rejected. The Validate cross-key
// predicate compares with strict-less-than against grace_effective; the
// 30m floor and the 30m heartbeat_interval are equal, so this triggers.
func TestValidate_RejectsHeartbeatIntervalAtGraceFloor(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "config.toml", `
[cache]
dir = "`+dir+`"
[upstream]
total_timeout = "1m"
max_retries = 5
[gc]
heartbeat_interval = "30m"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "gc.heartbeat_interval") ||
		!strings.Contains(err.Error(), "strictly less than") {
		t.Errorf("error %q does not name the cross-key violation", err)
	}
}

// heartbeat_interval = 60s with default upstream config (total_timeout=5m,
// max_retries=3 → derived 15m; grace_effective = max(15m, 30m) = 30m) →
// 60s < 30m, accepted.
func TestValidate_AcceptsHeartbeatIntervalUnderDefaultGrace(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "config.toml", `
[cache]
dir = "`+dir+`"
[gc]
heartbeat_interval = "60s"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GC.HeartbeatInterval.Duration != 60*time.Second {
		t.Errorf("heartbeat_interval = %s, want 60s", cfg.GC.HeartbeatInterval.Duration)
	}
}

// SPEC4 §12.1: gc.blob_grace must reject sub-second values, not just
// zero/negative — int64(d.Seconds()) silently truncates "500ms" to 0,
// which would make refcount=0 blobs immediately reapable.
func TestValidate_RejectsSubSecondBlobGrace(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "config.toml", `
[cache]
dir = "`+dir+`"
[gc]
blob_grace = "500ms"
`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "gc.blob_grace must be >= 1s") {
		t.Errorf("got %v, want gc.blob_grace >= 1s error", err)
	}
}

func TestValidate_RejectsZeroBlobGrace(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "config.toml", `
[cache]
dir = "`+dir+`"
[gc]
blob_grace = "0s"
`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "gc.blob_grace must be >= 1s") {
		t.Errorf("got %v, want gc.blob_grace >= 1s error", err)
	}
}

func TestValidate_RejectsZeroInterval(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "config.toml", `
[cache]
dir = "`+dir+`"
[gc]
interval = "0s"
`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "gc.interval must be > 0") {
		t.Errorf("got %v, want gc.interval > 0 error", err)
	}
}

func TestValidate_RejectsZeroBatchSize(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "config.toml", `
[cache]
dir = "`+dir+`"
[gc]
batch_size = 0
`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "gc.batch_size") {
		t.Errorf("got %v, want gc.batch_size error", err)
	}
}

func TestValidate_RejectsNegativeKeepDisplaced(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "config.toml", `
[cache]
dir = "`+dir+`"
[gc]
keep_displaced = -1
`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "gc.keep_displaced") {
		t.Errorf("got %v, want gc.keep_displaced error", err)
	}
}

func TestHeartbeatStaleGraceEffective_FloorsAt30m(t *testing.T) {
	// derived = 1s × 1 = 1s; grace_effective = max(1s, 30m) = 30m floor.
	cfg := defaultConfig()
	cfg.Upstream.TotalTimeout.Duration = 1 * time.Second
	cfg.Upstream.MaxRetries = 1
	if got := cfg.HeartbeatStaleGraceEffective(); got != 30*time.Minute {
		t.Errorf("HeartbeatStaleGraceEffective = %s, want 30m floor", got)
	}
}

func TestHeartbeatStaleGraceEffective_DerivedAboveFloor(t *testing.T) {
	// derived = 30m × 5 = 150m; grace_effective = max(150m, 30m) = 150m.
	cfg := defaultConfig()
	cfg.Upstream.TotalTimeout.Duration = 30 * time.Minute
	cfg.Upstream.MaxRetries = 5
	want := 150 * time.Minute
	if got := cfg.HeartbeatStaleGraceEffective(); got != want {
		t.Errorf("HeartbeatStaleGraceEffective = %s, want %s", got, want)
	}
}
