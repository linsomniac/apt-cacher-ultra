package config

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// defaultConfigFile is the shipped operator-facing config, installed to
// /etc/apt-cacher-ultra/config.toml by the packages. These tests are the
// regression guard the missing [admin] block (and the other undocumented
// keys) slipped past: the file is documentation as much as configuration,
// so a config knob that does not appear there is effectively undiscoverable.
const defaultConfigFile = "../../packaging/config/config.toml.default"

// TestDefaultConfigFileDocumentsEveryKey walks the Config type and asserts
// that every TOML key the loader understands is present (uncommented) in the
// shipped default file. Without this, a new field added to Config is invisible
// to operators unless the author remembers to document it here — which is
// exactly how admin.listen & friends went missing.
//
// Array-of-tables ([[trusted_signer]], [[remap]], [[mirror]]) are skipped:
// they have no meaningful default entry and are instead documented as
// commented examples.
func TestDefaultConfigFileDocumentsEveryKey(t *testing.T) {
	md, err := toml.DecodeFile(defaultConfigFile, &Config{})
	if err != nil {
		t.Fatalf("decode %s: %v", defaultConfigFile, err)
	}

	durationType := reflect.TypeOf(Duration{})
	var missing []string

	var walk func(rt reflect.Type, prefix []string)
	walk = func(rt reflect.Type, prefix []string) {
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			tag := f.Tag.Get("toml")
			if tag == "" {
				continue
			}
			path := append(append([]string(nil), prefix...), strings.Split(tag, ",")[0])

			// Array-of-tables are documented as commented examples only.
			if f.Type.Kind() == reflect.Slice && f.Type.Elem().Kind() == reflect.Struct {
				continue
			}
			// Duration is a struct wrapper but serializes as a scalar.
			if f.Type != durationType && f.Type.Kind() == reflect.Struct {
				walk(f.Type, path)
				continue
			}
			if !md.IsDefined(path...) {
				missing = append(missing, strings.Join(path, "."))
			}
		}
	}
	walk(reflect.TypeOf(Config{}), nil)

	if len(missing) > 0 {
		t.Errorf("%s is missing %d config key(s); every key the loader accepts must appear there as documentation:\n  %s",
			defaultConfigFile, len(missing), strings.Join(missing, "\n  "))
	}
}

// TestDefaultConfigFileLoads proves the shipped default file is itself a
// valid config: it parses and passes Validate() against a real (temp) cache
// directory. A typo in the documented defaults (or a documented default that
// violates a cross-key invariant like heartbeat_interval vs blob_grace) would
// otherwise only surface on an operator's first start.
func TestDefaultConfigFileLoads(t *testing.T) {
	raw, err := os.ReadFile(defaultConfigFile)
	if err != nil {
		t.Fatalf("read %s: %v", defaultConfigFile, err)
	}
	dir := t.TempDir()
	// The shipped file points at the production cache directory; redirect it
	// so Load's writability + directory checks run against a temp dir.
	patched := strings.ReplaceAll(string(raw), DefaultCacheDir, dir)
	path := writeTOML(t, dir, "config.toml", patched)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%s): %v", defaultConfigFile, err)
	}
	// Spot-check that the newly-documented sections actually took effect,
	// rather than being swallowed as unknown keys.
	if cfg.Admin.Listen != "127.0.0.1:6789" {
		t.Errorf("admin.listen = %q, want 127.0.0.1:6789", cfg.Admin.Listen)
	}
	if cfg.Retention.MaxVersionsPerPackage != 3 {
		t.Errorf("retention.max_versions_per_package = %d, want 3", cfg.Retention.MaxVersionsPerPackage)
	}
	if cfg.HoldPackages.Window.Duration == 0 {
		t.Errorf("hold_packages.window not parsed from default file")
	}
	if cfg.TlsMitm.CertCacheSize != 256 {
		t.Errorf("tls_mitm.cert_cache_size = %d, want 256", cfg.TlsMitm.CertCacheSize)
	}
}
