package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestLoad_UpstreamProxy(t *testing.T) {
	for _, tt := range []struct {
		name     string
		upstream string
		want     string
	}{
		{name: "omitted"},
		{name: "explicit direct", upstream: `proxy = ""`},
		{name: "HTTP", upstream: `proxy = "http://proxy.example:3142"`, want: "http://proxy.example:3142"},
		{name: "HTTPS credentials", upstream: `proxy = "https://user:p%40ss@proxy.example/"`, want: "https://user:p%40ss@proxy.example/"},
		{name: "IPv6", upstream: `proxy = "http://[::1]:3142"`, want: "http://[::1]:3142"},
		{name: "direct retains deny ranges", upstream: "proxy = \"\"\ndeny_target_ranges = [\"127.0.0.0/8\"]"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeTOML(t, dir, "config.toml", fmt.Sprintf("[cache]\ndir = %q\n[upstream]\n%s\n", dir, tt.upstream))
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Upstream.Proxy != tt.want {
				t.Errorf("upstream.proxy = %q, want %q", cfg.Upstream.Proxy, tt.want)
			}
			if tt.name == "direct retains deny ranges" && len(cfg.Upstream.DenyTargetRanges) != 1 {
				t.Errorf("direct mode lost deny ranges: %v", cfg.Upstream.DenyTargetRanges)
			}
		})
	}
}

func TestLoad_UpstreamProxyRejectsInvalidWithoutCredentials(t *testing.T) {
	const credentials = "proxy-user-secret:proxy-password-secret@"
	for _, tt := range []struct {
		name  string
		proxy string
		extra string
	}{
		{name: "relative", proxy: "proxy.example:3142"},
		{name: "unsupported scheme", proxy: "socks5://" + credentials + "proxy.example:1080"},
		{name: "malformed escape", proxy: "http://" + credentials + "proxy.example/%zz"},
		{name: "out of range port", proxy: "http://" + credentials + "proxy.example:65536"},
		{name: "empty port", proxy: "http://" + credentials + "proxy.example:"},
		{name: "path", proxy: "http://" + credentials + "proxy.example/cache"},
		{name: "query", proxy: "http://" + credentials + "proxy.example?"},
		{name: "fragment", proxy: "http://" + credentials + "proxy.example#"},
		{name: "deny range conflict", proxy: "http://" + credentials + "proxy.example:3142", extra: `deny_target_ranges = ["10.0.0.0/8"]`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeTOML(t, dir, "config.toml", fmt.Sprintf("[cache]\ndir = %q\n[upstream]\nproxy = %q\n%s\n", dir, tt.proxy, tt.extra))
			_, err := Load(path)
			if err == nil {
				t.Fatal("Load accepted invalid upstream.proxy")
			}
			if !strings.Contains(err.Error(), "upstream.proxy") {
				t.Errorf("error does not identify upstream.proxy: %v", err)
			}
			if tt.extra != "" && !strings.Contains(err.Error(), "deny_target_ranges") {
				t.Errorf("error does not identify deny_target_ranges conflict: %v", err)
			}
			for _, secret := range []string{"proxy-user-secret", "proxy-password-secret"} {
				if strings.Contains(err.Error(), secret) {
					t.Error("validation error disclosed proxy credentials")
				}
			}
		})
	}
}
