package config

import (
	"os"
	"path/filepath"
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
listen = "0.0.0.0:3142"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Cache.Listen != "0.0.0.0:3142" {
		t.Errorf("listen = %q", cfg.Cache.Listen)
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
			name:    "missing cache.listen",
			body:    `[cache]` + "\n" + `dir = "DIR"`,
			wantErr: "cache.listen",
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
			wantErr: "duplicates",
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
