package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestLoadTrustedProxies(t *testing.T) {
	for _, tt := range []struct {
		name, setting string
		wantLen       int
		wantError     bool
	}{
		{name: "omitted"},
		{name: "disabled", setting: `trusted_proxies = []`},
		{name: "addresses and CIDRs", setting: `trusted_proxies = ["10.0.0.2", "192.0.2.0/24", "2001:db8::1", "fd00::/8"]`, wantLen: 4},
		{name: "hostname", setting: `trusted_proxies = ["proxy.example"]`, wantError: true},
		{name: "bad CIDR", setting: `trusted_proxies = ["10.0.0.0/33"]`, wantError: true},
		{name: "empty entry", setting: `trusted_proxies = [""]`, wantError: true},
		{name: "partly invalid", setting: `trusted_proxies = ["10.0.0.2", "typo"]`, wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeTOML(t, dir, "config.toml", fmt.Sprintf("[cache]\ndir = %q\n%s\n", dir, tt.setting))
			cfg, err := Load(path)
			if tt.wantError {
				if err == nil || !strings.Contains(err.Error(), "cache.trusted_proxies[") {
					t.Fatalf("expected actionable config error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.Cache.TrustedProxies) != tt.wantLen {
				t.Fatalf("trusted_proxies = %v, want %d entries", cfg.Cache.TrustedProxies, tt.wantLen)
			}
		})
	}
}
