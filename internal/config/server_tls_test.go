package config

import (
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/testutil/certfixture"
)

func TestServerTLSConfiguration(t *testing.T) {
	cert, key := certfixture.New(t).Write(t, t.TempDir())
	cases := []struct {
		name    string
		change  func(*Config)
		problem string
	}{
		{"defaults", func(*Config) {}, ""},
		{"admin files", func(c *Config) { c.Admin.TLSCert = cert; c.Admin.TLSKey = key }, ""},
		{"partial admin", func(c *Config) { c.Admin.TLSCert = cert }, "must both"},
		{"partial cache", func(c *Config) { c.Cache.ListenTLS = ":8443" }, "must all"},
		{"missing ACME", func(c *Config) { c.Admin.UseACME = true }, "requires acme.enabled"},
		{"managed both", func(c *Config) { enableTestACME(c); c.Cache.ListenTLS = ":8443"; c.Cache.UseACME = true }, ""},
		{"file conflict", func(c *Config) { enableTestACME(c); c.Admin.TLSCert = cert; c.Admin.TLSKey = key }, "conflicts"},
		{"terms", func(c *Config) { enableTestACME(c); c.ACME.AcceptTerms = false }, "accept_terms"},
		{"disabled target", func(c *Config) { enableTestACME(c); c.Admin.Enabled = false }, "requires admin.enabled"},
		{"no target", func(c *Config) { enableTestACME(c); c.Admin.UseACME = false }, "requires a listener"},
		{"admin collision", func(c *Config) { enableTestACME(c); c.ACME.Listen = c.Admin.Listen }, "conflicts with admin"},
		{"cache wildcard collision", func(c *Config) { enableTestACME(c); c.Cache.Listen = ":8080" }, "conflicts with a cache"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{}
			c.Defaults()
			c.Admin.Enabled = true
			c.Admin.Listen = "127.0.0.1:6789"
			tt.change(c)
			err := c.ValidateServerTLS()
			if tt.problem == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.problem) {
				t.Fatalf("got %v, want %s", err, tt.problem)
			}
		})
	}
}

func enableTestACME(c *Config) {
	c.Admin.UseACME = true
	c.ACME = ACMEConfig{Enabled: true, DirectoryURL: "https://ca.example/directory", DNSName: "cache.example.net", Listen: "127.0.0.1:8080", AcceptTerms: true}
}

func TestACMEIdentifiers(t *testing.T) {
	for _, s := range []string{"http://ca.example/directory", "https://user:pass@ca.example/directory", "https://ca.example/directory?token=secret", "https://ca.example/#", "https://ca.example:0/directory", "https://ca.example:65536/directory", "https://ca.example:/directory"} {
		if ValidateACMEDirectory(s) == nil {
			t.Errorf("accepted directory %q", s)
		}
	}
	for _, s := range []string{"*.example.com", "127.0.0.1", "Cache.example.com", "example.com.", "localhost", "-x.example.com", "x..example.com", "x.example.com:443", "xé.example.com"} {
		if ValidateACMEName(s) == nil {
			t.Errorf("accepted name %q", s)
		}
	}
}

func TestDNS01Configuration(t *testing.T) {
	for _, tt := range []struct {
		name      string
		change    func(*Config)
		wantError bool
	}{
		{"no HTTP listener", func(c *Config) { c.ACME.Listen = "" }, false},
		{"ignored HTTP overlap", func(c *Config) { c.ACME.Listen = c.Cache.Listen }, false},
		{"recursive resolvers", func(c *Config) { c.ACME.DNSResolvers = []string{"1.1.1.1:53", "[2606:4700:4700::1111]:53"} }, false},
		{"missing provider", func(c *Config) { c.ACME.DNSProvider = "" }, true},
		{"interactive provider", func(c *Config) { c.ACME.DNSProvider = "manual" }, true},
		{"command hook", func(c *Config) { c.ACME.DNSProvider = "exec" }, true},
		{"invalid resolver", func(c *Config) { c.ACME.DNSResolvers = []string{"https://dns.example/"} }, true},
		{"zero resolver port", func(c *Config) { c.ACME.DNSResolvers = []string{"127.0.0.1:0"} }, true},
		{"wrong challenge", func(c *Config) { c.ACME.ChallengeType = "dns-02" }, true},
		{"mixed HTTP settings", func(c *Config) { c.ACME.ChallengeType = "http-01" }, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{}
			c.Defaults()
			c.Admin.Enabled = true
			enableTestACME(c)
			c.ACME.ChallengeType = "dns-01"
			c.ACME.DNSProvider = "cloudflare"
			tt.change(c)
			if err := c.ValidateServerTLS(); (err != nil) != tt.wantError {
				t.Fatalf("unexpected result %v", err)
			}
		})
	}
}
