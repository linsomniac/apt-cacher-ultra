package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	testutil "github.com/linsomniac/apt-cacher-ultra/internal/testutil/tlscert"
)

// acmeValidBody is the minimal valid ACME-enabled configuration:
// both surfaces select ACME, every required [acme] key is set.
const acmeValidBody = `[cache]
dir = "DIR"
listen = "0.0.0.0:3142"
listen_tls = "0.0.0.0:8443"
use_acme = true

[admin]
enabled = true
listen = "127.0.0.1:6789"
use_acme = true

[acme]
enabled = true
directory_url = "https://acme-v02.api.letsencrypt.org/directory"
dns_name = "cache.example.net"
listen = "0.0.0.0:80"
`

func loadAcmeConfig(t *testing.T, body string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	body = strings.ReplaceAll(body, "DIR", dir)
	return Load(writeTOML(t, dir, "config.toml", body))
}

func TestLoad_AcmeValid(t *testing.T) {
	cfg, err := loadAcmeConfig(t, acmeValidBody)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Acme.Enabled {
		t.Error("Acme.Enabled = false, want true")
	}
	if cfg.Acme.DirectoryURL != "https://acme-v02.api.letsencrypt.org/directory" {
		t.Errorf("DirectoryURL = %q", cfg.Acme.DirectoryURL)
	}
	if cfg.Acme.DNSName != "cache.example.net" {
		t.Errorf("DNSName = %q", cfg.Acme.DNSName)
	}
	if cfg.Acme.Listen != "0.0.0.0:80" {
		t.Errorf("Listen = %q", cfg.Acme.Listen)
	}
	if !cfg.Cache.UseAcme || !cfg.Admin.UseAcme {
		t.Errorf("UseAcme = %v/%v, want true/true", cfg.Cache.UseAcme, cfg.Admin.UseAcme)
	}
}

// TestLoad_AcmeDefaults pins the presence-sensitive defaults applied by
// Load(): listen = "0.0.0.0:80", renew_before_days = 7 and
// check_interval = 12h when the keys are absent, and an explicit value
// surviving when present.
func TestLoad_AcmeDefaults(t *testing.T) {
	cfg, err := loadAcmeConfig(t, acmeValidBody)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Acme.RenewBeforeDays != 7 {
		t.Errorf("RenewBeforeDays = %d, want 7 (default)", cfg.Acme.RenewBeforeDays)
	}
	if cfg.Acme.CheckInterval.Duration != 12*time.Hour {
		t.Errorf("CheckInterval = %s, want 12h (default)", cfg.Acme.CheckInterval.Duration)
	}

	// listen absent from the [acme] block: defaults to 0.0.0.0:80.
	withoutListen := strings.Replace(acmeValidBody, "listen = \"0.0.0.0:80\"\n", "", 1)
	cfgNoListen, err := loadAcmeConfig(t, withoutListen)
	if err != nil {
		t.Fatalf("Load without listen: %v", err)
	}
	if cfgNoListen.Acme.Listen != "0.0.0.0:80" {
		t.Errorf("Listen = %q, want 0.0.0.0:80 (default)", cfgNoListen.Acme.Listen)
	}

	withExplicit := strings.Replace(acmeValidBody,
		`listen = "0.0.0.0:80"`,
		`listen = "0.0.0.0:80"
renew_before_days = 30
check_interval = "1h"`, 1)
	cfg2, err := loadAcmeConfig(t, withExplicit)
	if err != nil {
		t.Fatalf("Load explicit: %v", err)
	}
	if cfg2.Acme.RenewBeforeDays != 30 {
		t.Errorf("RenewBeforeDays = %d, want 30 (explicit)", cfg2.Acme.RenewBeforeDays)
	}
	if cfg2.Acme.CheckInterval.Duration != time.Hour {
		t.Errorf("CheckInterval = %s, want 1h (explicit)", cfg2.Acme.CheckInterval.Duration)
	}

	// Explicit non-default listen survives.
	withCustomListen := strings.Replace(acmeValidBody,
		`listen = "0.0.0.0:80"`,
		`listen = "127.0.0.1:8080"`, 1)
	cfg3, err := loadAcmeConfig(t, withCustomListen)
	if err != nil {
		t.Fatalf("Load custom listen: %v", err)
	}
	if cfg3.Acme.Listen != "127.0.0.1:8080" {
		t.Errorf("Listen = %q, want 127.0.0.1:8080 (explicit)", cfg3.Acme.Listen)
	}
}

// TestLoad_AcmeInvalidCases pins each configuration validation rule of
// docs/acme-certificates-spec.md § "Validation de la configuration",
// plus the use_acme-without-acme.enabled guard.
func TestLoad_AcmeInvalidCases(t *testing.T) {
	// withAcmeField returns the valid body with one [acme] line replaced.
	withAcmeField := func(old, new string) string {
		return strings.Replace(acmeValidBody, old, new, 1)
	}
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		// Rule 1: directory_url.
		{name: "directory_url not https", body: withAcmeField(`directory_url = "https://acme-v02.api.letsencrypt.org/directory"`, `directory_url = "http://acme-v02.api.letsencrypt.org/directory"`), wantErr: "acme.directory_url"},
		{name: "directory_url with credentials", body: withAcmeField(`directory_url = "https://acme-v02.api.letsencrypt.org/directory"`, `directory_url = "https://user:pass@acme-v02.api.letsencrypt.org/directory"`), wantErr: "acme.directory_url"},
		{name: "directory_url with fragment", body: withAcmeField(`directory_url = "https://acme-v02.api.letsencrypt.org/directory"`, `directory_url = "https://acme-v02.api.letsencrypt.org/directory#frag"`), wantErr: "acme.directory_url"},
		{name: "directory_url with query", body: withAcmeField(`directory_url = "https://acme-v02.api.letsencrypt.org/directory"`, `directory_url = "https://acme-v02.api.letsencrypt.org/directory?x=1"`), wantErr: "acme.directory_url"},
		{name: "directory_url missing path", body: withAcmeField(`directory_url = "https://acme-v02.api.letsencrypt.org/directory"`, `directory_url = "https://acme-v02.api.letsencrypt.org"`), wantErr: "acme.directory_url"},
		{name: "directory_url empty", body: withAcmeField(`directory_url = "https://acme-v02.api.letsencrypt.org/directory"`, `directory_url = ""`), wantErr: "acme.directory_url"},
		// Rule 2: dns_name.
		{name: "dns_name with scheme", body: withAcmeField(`dns_name = "cache.example.net"`, `dns_name = "https://cache.example.net"`), wantErr: "acme.dns_name"},
		{name: "dns_name with port", body: withAcmeField(`dns_name = "cache.example.net"`, `dns_name = "cache.example.net:8443"`), wantErr: "acme.dns_name"},
		{name: "dns_name IP literal", body: withAcmeField(`dns_name = "cache.example.net"`, `dns_name = "192.0.2.10"`), wantErr: "acme.dns_name"},
		{name: "dns_name wildcard", body: withAcmeField(`dns_name = "cache.example.net"`, `dns_name = "*.example.net"`), wantErr: "acme.dns_name"},
		{name: "dns_name single label", body: withAcmeField(`dns_name = "cache.example.net"`, `dns_name = "cache"`), wantErr: "acme.dns_name"},
		{name: "dns_name trailing dot", body: withAcmeField(`dns_name = "cache.example.net"`, `dns_name = "cache.example.net."`), wantErr: "acme.dns_name"},
		{name: "dns_name empty", body: withAcmeField(`dns_name = "cache.example.net"`, `dns_name = ""`), wantErr: "acme.dns_name"},
		// Rule 3: acme.listen.
		{name: "acme.listen invalid", body: withAcmeField(`listen = "0.0.0.0:80"`, `listen = "not-a-host-port"`), wantErr: "acme.listen"},
		{name: "acme.listen empty", body: withAcmeField(`listen = "0.0.0.0:80"`, `listen = ""`), wantErr: "acme.listen"},
		// Rule 4: at least one surface.
		{name: "enabled without any use_acme", body: strings.Replace(acmeValidBody, `use_acme = true`, `use_acme = false`, 2), wantErr: "at least one of cache.use_acme or admin.use_acme"},
		// Rule 5: cache.use_acme requires cache.listen_tls.
		{name: "cache.use_acme without listen_tls", body: strings.Replace(acmeValidBody, `listen_tls = "0.0.0.0:8443"`+"\n"+`use_acme = true`, `use_acme = true`, 1), wantErr: "cache.use_acme requires cache.listen_tls"},
		// Rule 6: admin.use_acme requires admin.enabled.
		{name: "admin.use_acme without admin.enabled", body: strings.Replace(acmeValidBody, "[admin]\nenabled = true", "[admin]\nenabled = false", 1), wantErr: "admin.use_acme requires admin.enabled"},
		// Rule 7: ACME target devoid of manual PEM.
		{name: "cache.use_acme with tls_cert", body: strings.Replace(acmeValidBody, `listen_tls = "0.0.0.0:8443"`, `listen_tls = "0.0.0.0:8443"`+"\n"+`tls_cert = "CERT"`, 1), wantErr: "cache.use_acme is incompatible with cache.tls_cert"},
		{name: "admin.use_acme with tls_key", body: strings.Replace(acmeValidBody, "[admin]\nenabled = true", "[admin]\nenabled = true\ntls_key = \"KEY\"", 1), wantErr: "admin.use_acme is incompatible with admin.tls_cert / admin.tls_key"},
		// Rule 8: renew_before_days >= 1.
		{name: "renew_before_days zero", body: withAcmeField(`listen = "0.0.0.0:80"`, `listen = "0.0.0.0:80"`+"\n"+`renew_before_days = 0`), wantErr: "acme.renew_before_days must be >= 1"},
		{name: "renew_before_days negative", body: withAcmeField(`listen = "0.0.0.0:80"`, `listen = "0.0.0.0:80"`+"\n"+`renew_before_days = -3`), wantErr: "acme.renew_before_days must be >= 1"},
		// Rule 9: check_interval > 0.
		{name: "check_interval zero", body: withAcmeField(`listen = "0.0.0.0:80"`, `listen = "0.0.0.0:80"`+"\n"+`check_interval = "0s"`), wantErr: "acme.check_interval must be > 0"},
		// acme disabled: any use_acme = true is an error.
		{name: "use_acme without acme.enabled", body: strings.Replace(acmeValidBody, "enabled = true\ndirectory_url", "enabled = false\ndirectory_url", 1), wantErr: "require acme.enabled = true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadAcmeConfig(t, tc.body)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestLoad_AcmeMixedSources pins that the ACME/PEM exclusivity is per
// surface: the cache may keep a manual PEM pair while the admin uses
// ACME (and vice versa) — there is exactly one certificate source per
// surface, not per daemon.
func TestLoad_AcmeMixedSources(t *testing.T) {
	root := t.TempDir()
	cacheDir := t.TempDir()
	certPath, keyPath := testutil.WriteSelfSignedCert(t, filepath.Join(root, "cache"))
	body := `[cache]
dir = "` + cacheDir + `"
listen = "0.0.0.0:3142"
listen_tls = "0.0.0.0:8443"
tls_cert = "` + certPath + `"
tls_key = "` + keyPath + `"

[admin]
enabled = true
listen = "127.0.0.1:6789"
use_acme = true

[acme]
enabled = true
directory_url = "https://acme-v02.api.letsencrypt.org/directory"
dns_name = "cache.example.net"
listen = "0.0.0.0:80"
`
	cfg, err := Load(writeTOML(t, root, "config.toml", body))
	if err != nil {
		t.Fatalf("Load mixed: %v", err)
	}
	if cfg.Cache.UseAcme {
		t.Error("cache.use_acme = true, want false (manual PEM)")
	}
	if !cfg.Admin.UseAcme {
		t.Error("admin.use_acme = false, want true")
	}
}

// TestAdminConfig_TLSEnabled_UseAcme pins that use_acme alone flips the
// admin listener to HTTPS, exactly like a manual pair.
func TestAdminConfig_TLSEnabled_UseAcme(t *testing.T) {
	if !(AdminConfig{UseAcme: true}).TLSEnabled() {
		t.Error("TLSEnabled() = false with UseAcme, want true")
	}
	if !(AdminConfig{TLSCert: "c.pem", TLSKey: "k.pem"}).TLSEnabled() {
		t.Error("TLSEnabled() = false with manual pair, want true")
	}
	if (AdminConfig{}).TLSEnabled() {
		t.Error("TLSEnabled() = true with neither source, want false")
	}
}

// TestAcmeListenConflicts pins the startup-warning helper: same port
// with identical hosts or a wildcard on either side conflicts; different
// ports or different specific hosts do not.
func TestAcmeListenConflicts(t *testing.T) {
	base := func() *Config {
		cfg := &Config{}
		cfg.Cache.Listen = "0.0.0.0:3142"
		cfg.Acme.Enabled = true
		cfg.Acme.Listen = "0.0.0.0:80"
		return cfg
	}
	cases := []struct {
		name   string
		mutate func(*Config)
		want   []string
	}{
		{name: "disabled acme", mutate: func(c *Config) { c.Acme.Enabled = false }, want: nil},
		{name: "different port", mutate: func(c *Config) {}, want: nil},
		{
			name:   "same port wildcard vs wildcard",
			mutate: func(c *Config) { c.Cache.Listen = "0.0.0.0:80" },
			want:   []string{"cache.listen"},
		},
		{
			name:   "wildcard acme vs specific cache",
			mutate: func(c *Config) { c.Cache.Listen = "127.0.0.1:80" },
			want:   []string{"cache.listen"},
		},
		{
			name:   "specific acme vs wildcard cache",
			mutate: func(c *Config) { c.Acme.Listen = "127.0.0.1:80"; c.Cache.Listen = "0.0.0.0:80" },
			want:   []string{"cache.listen"},
		},
		{
			name:   "same specific host and port",
			mutate: func(c *Config) { c.Acme.Listen = "127.0.0.1:80"; c.Cache.Listen = "127.0.0.1:80" },
			want:   []string{"cache.listen"},
		},
		{
			name:   "different specific hosts same port",
			mutate: func(c *Config) { c.Acme.Listen = "127.0.0.1:80"; c.Cache.Listen = "192.0.2.1:80" },
			want:   nil,
		},
		{
			name: "cache.listen_tls conflict",
			mutate: func(c *Config) {
				c.Cache.ListenTLS = "0.0.0.0:80"
				c.Cache.TLSCert = "c.pem"
				c.Cache.TLSKey = "k.pem"
			},
			want: []string{"cache.listen_tls"},
		},
		{
			name: "admin.listen conflict when enabled",
			mutate: func(c *Config) {
				c.Admin.Enabled = true
				c.Admin.Listen = "0.0.0.0:80"
			},
			want: []string{"admin.listen"},
		},
		{
			name: "admin.listen ignored when disabled",
			mutate: func(c *Config) {
				c.Admin.Enabled = false
				c.Admin.Listen = "0.0.0.0:80"
			},
			want: nil,
		},
		{
			name: "multiple conflicts reported",
			mutate: func(c *Config) {
				c.Cache.Listen = "0.0.0.0:80"
				c.Admin.Enabled = true
				c.Admin.Listen = "0.0.0.0:80"
			},
			want: []string{"cache.listen", "admin.listen"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mutate(cfg)
			got := cfg.AcmeListenConflicts()
			if len(got) != len(tc.want) {
				t.Fatalf("AcmeListenConflicts() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("AcmeListenConflicts() = %v, want %v", got, tc.want)
				}
			}
		})
	}
}
