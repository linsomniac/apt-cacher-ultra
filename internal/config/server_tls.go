package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/linsomniac/apt-cacher-ultra/internal/servertls"
)

type ACMEConfig struct {
	Enabled      bool   `toml:"enabled"`
	DirectoryURL string `toml:"directory_url"`
	DNSName      string `toml:"dns_name"`
	Listen       string `toml:"listen"`
	AcceptTerms  bool   `toml:"accept_terms"`
}

func (a AdminConfig) TLSEnabled() bool { return a.TLSCert != "" || a.TLSKey != "" || a.UseACME }

// ValidateServerTLS also protects programmatically constructed daemon configs.
// It performs structural checks only; serving sources retain the validated
// certificate material loaded during startup.
func (c *Config) ValidateServerTLS() error {
	var errs []error
	filePair := func(name, cert, key string) {
		if (cert == "") != (key == "") {
			errs = append(errs, fmt.Errorf("%s.tls_cert / %s.tls_key must both be set or both empty", name, name))
			return
		}
		if cert != "" {
			for field, path := range map[string]string{"tls_cert": cert, "tls_key": key} {
				if _, err := servertls.ReadFile(path); err != nil {
					errs = append(errs, fmt.Errorf("%s.%s: %w", name, field, err))
				}
			}
		}
	}
	if c.Cache.UseACME {
		if c.Cache.ListenTLS == "" {
			errs = append(errs, errors.New("cache.use_acme requires cache.listen_tls"))
		}
		if c.Cache.TLSCert != "" || c.Cache.TLSKey != "" {
			errs = append(errs, errors.New("cache.use_acme conflicts with cache.tls_cert / cache.tls_key"))
		}
	} else {
		any := c.Cache.ListenTLS != "" || c.Cache.TLSCert != "" || c.Cache.TLSKey != ""
		if any && (c.Cache.ListenTLS == "" || c.Cache.TLSCert == "" || c.Cache.TLSKey == "") {
			errs = append(errs, errors.New("cache.listen_tls / tls_cert / tls_key must all be set or all empty"))
		} else {
			filePair("cache", c.Cache.TLSCert, c.Cache.TLSKey)
		}
	}
	if c.Cache.ListenTLS != "" {
		if err := validateListenAddr(c.Cache.ListenTLS); err != nil {
			errs = append(errs, fmt.Errorf("cache.listen_tls: %w", err))
		}
	}
	if c.Admin.UseACME {
		if !c.Admin.Enabled {
			errs = append(errs, errors.New("admin.use_acme requires admin.enabled"))
		}
		if c.Admin.TLSCert != "" || c.Admin.TLSKey != "" {
			errs = append(errs, errors.New("admin.use_acme conflicts with admin.tls_cert / admin.tls_key"))
		}
	} else if c.Admin.Enabled {
		filePair("admin", c.Admin.TLSCert, c.Admin.TLSKey)
	}
	if !c.ACME.Enabled {
		if c.Cache.UseACME || c.Admin.UseACME {
			errs = append(errs, errors.New("use_acme requires acme.enabled"))
		}
		return errors.Join(errs...)
	}
	if !c.Cache.UseACME && !c.Admin.UseACME {
		errs = append(errs, errors.New("acme.enabled requires a listener with use_acme"))
	}
	if !c.ACME.AcceptTerms {
		errs = append(errs, errors.New("acme.accept_terms must be true when ACME is enabled"))
	}
	if err := ValidateACMEDirectory(c.ACME.DirectoryURL); err != nil {
		errs = append(errs, err)
	}
	if err := ValidateACMEName(c.ACME.DNSName); err != nil {
		errs = append(errs, err)
	}
	if err := validateListenAddr(c.ACME.Listen); err != nil {
		errs = append(errs, fmt.Errorf("acme.listen: %w", err))
	}
	for _, addr := range []string{c.Cache.Listen, c.Cache.ListenTLS} {
		if ListenOverlap(c.ACME.Listen, addr) {
			errs = append(errs, errors.New("acme.listen conflicts with a cache listener"))
		}
	}
	if c.Admin.Enabled && ListenOverlap(c.ACME.Listen, c.Admin.Listen) {
		errs = append(errs, errors.New("acme.listen conflicts with admin.listen"))
	}
	return errors.Join(errs...)
}

func ValidateACMEDirectory(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") || strings.TrimSpace(raw) != raw {
		return errors.New("acme.directory_url must be an absolute HTTPS URL without credentials, query or fragment")
	}
	if strings.HasSuffix(u.Host, ":") {
		return errors.New("acme.directory_url has an invalid port")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("acme.directory_url has an invalid port")
		}
	}
	return nil
}

var acmeLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func ValidateACMEName(name string) error {
	labels := strings.Split(name, ".")
	_, ipErr := netip.ParseAddr(name)
	if len(name) > 253 || len(labels) < 2 || ipErr == nil {
		return errors.New("acme.dns_name must be one lowercase DNS name")
	}
	for _, label := range labels {
		if !acmeLabel.MatchString(label) {
			return errors.New("acme.dns_name must be one lowercase DNS name without wildcard or port")
		}
	}
	return nil
}

// ListenOverlap catches definite overlaps without DNS lookups. Actual socket
// reservation catches aliases and OS-specific dual-stack conflicts at startup.
func ListenOverlap(a, b string) bool {
	ah, ap, ae := net.SplitHostPort(a)
	bh, bp, be := net.SplitHostPort(b)
	if ae != nil || be != nil || ap != bp || ap == "0" {
		return false
	}
	wild := func(h string) bool { return h == "" || h == "0.0.0.0" || h == "::" }
	return strings.EqualFold(ah, bh) || wild(ah) || wild(bh)
}
