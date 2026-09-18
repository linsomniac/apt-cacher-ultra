package config

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

const packagedConfigPath = "../../packaging/config/config.toml.default"

// The packaged config is also the concise option reference. Check actual TOML
// keys, including the commented array-of-tables examples, rather than prose.
func TestDefaultConfigFileDocumentsEveryKey(t *testing.T) {
	raw := readPackagedConfig(t)
	defaults, err := toml.Decode(raw, &Config{})
	if err != nil {
		t.Fatal(err)
	}
	examples, err := toml.Decode(optionalConfigExamples(t, raw), &Config{})
	if err != nil {
		t.Fatalf("optional examples: %v", err)
	}
	keys := make(map[string]map[string]bool)
	for name, md := range map[string]toml.MetaData{"defaults": defaults, "optional examples": examples} {
		if unknown := md.Undecoded(); len(unknown) > 0 {
			t.Errorf("%s contain unknown TOML keys: %v", name, unknown)
		}
		keys[name] = make(map[string]bool)
		// IsDefined cannot descend into array-of-tables entries; Keys includes
		// their member paths as well as ordinary table keys.
		for _, key := range md.Keys() {
			keys[name][key.String()] = true
		}
	}

	var checkKeys func(reflect.Type, []string, map[string]bool)
	checkKeys = func(typ reflect.Type, prefix []string, documented map[string]bool) {
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			key, _, _ := strings.Cut(field.Tag.Get("toml"), ",")
			if !field.IsExported() || key == "-" {
				continue
			}
			if key == "" {
				key = field.Name // TOML also accepts exported fields without tags.
			}
			path := append(append([]string(nil), prefix...), key)
			switch {
			case field.Type.Kind() == reflect.Slice && field.Type.Elem().Kind() == reflect.Struct:
				checkKeys(field.Type.Elem(), path, keys["optional examples"])
			case field.Type.Kind() == reflect.Struct && field.Type != reflect.TypeFor[Duration]():
				checkKeys(field.Type, path, documented)
			default:
				if !documented[strings.Join(path, ".")] {
					t.Errorf("%s does not document %s", packagedConfigPath, strings.Join(path, "."))
				}
			}
		}
	}
	checkKeys(reflect.TypeFor[Config](), nil, keys["defaults"])
}

func TestDefaultConfigFilePreservesProfile(t *testing.T) {
	dir := t.TempDir()
	got := loadConfigInTempDir(t, readPackagedConfig(t), dir)
	// The shipped settings currently have the same effective values as a
	// minimal config. Record intentional shipped overrides here if that changes.
	want := loadConfigInTempDir(t, "[cache]\ndir = \""+DefaultCacheDir+"\"\n", dir)

	var compare func(string, reflect.Value, reflect.Value)
	compare = func(path string, got, want reflect.Value) {
		if got.Kind() == reflect.Struct && got.Type() != reflect.TypeFor[Duration]() {
			for i := 0; i < got.NumField(); i++ {
				compare(path+"."+got.Type().Field(i).Name, got.Field(i), want.Field(i))
			}
			return
		}
		// After Load has applied presence-sensitive defaults, absent and
		// explicit empty lists have the same effective value here.
		if got.Kind() == reflect.Slice && got.Len() == 0 && want.Len() == 0 {
			return
		}
		if !reflect.DeepEqual(got.Interface(), want.Interface()) {
			t.Errorf("%s = %v, want %v", path, got.Interface(), want.Interface())
		}
	}
	compare("config", reflect.ValueOf(*got), reflect.ValueOf(*want))
}

func TestDefaultConfigFileOptionalExamplesLoad(t *testing.T) {
	raw := readPackagedConfig(t)
	loadConfigInTempDir(t, raw+"\n"+optionalConfigExamples(t, raw), t.TempDir())
}

func readPackagedConfig(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(packagedConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Only this explicitly marked block is uncommented. Other commented settings
// are alternatives, and uncommenting them together could produce duplicate keys.
func optionalConfigExamples(t *testing.T, raw string) string {
	t.Helper()
	const begin = "# BEGIN OPTIONAL TABLE EXAMPLES"
	const end = "# END OPTIONAL TABLE EXAMPLES"
	if strings.Count(raw, begin) != 1 || strings.Count(raw, end) != 1 {
		t.Fatal("expected exactly one marked optional table examples block")
	}
	_, rest, _ := strings.Cut(raw, begin)
	body, _, ok := strings.Cut(rest, end)
	if !ok {
		t.Fatal("optional table examples end marker precedes begin marker")
	}
	var example strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		uncommented, ok := strings.CutPrefix(line, "#")
		if !ok {
			t.Fatalf("optional table example must be commented: %q", line)
		}
		example.WriteString(strings.TrimPrefix(uncommented, " "))
		example.WriteByte('\n')
	}
	return example.String()
}

// Load only parses/defaults/validates; it does not start listeners or fetch
// anything. Validation does probe directory writability, so redirect cache.dir
// structurally and reject external file paths before calling the real loader.
func loadConfigInTempDir(t *testing.T, raw, dir string) *Config {
	t.Helper()
	var cfg Config
	if _, err := toml.Decode(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Cache.Dir != DefaultCacheDir {
		t.Fatalf("packaged cache.dir = %q, want %q", cfg.Cache.Dir, DefaultCacheDir)
	}
	if cfg.Cache.TLSCert != "" || cfg.Cache.TLSKey != "" || cfg.Admin.HtpasswdFile != "" ||
		cfg.TlsMitm.CaCert != "" || cfg.TlsMitm.CaKey != "" || cfg.TlsMitm.CaStorageDir != "" {
		t.Fatal("packaged config must not require external TLS, CA, or htpasswd paths")
	}
	// A generic map preserves omitted keys and duration strings. Encoding the
	// Config struct would insert zero values for absent settings before Load.
	var document map[string]any
	if _, err := toml.Decode(raw, &document); err != nil {
		t.Fatal(err)
	}
	cache, ok := document["cache"].(map[string]any)
	if !ok {
		t.Fatal("packaged config must contain a [cache] table")
	}
	cache["dir"] = dir
	var rewritten strings.Builder
	if err := toml.NewEncoder(&rewritten).Encode(document); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(writeTOML(t, dir, "config.toml", rewritten.String()))
	if err != nil {
		t.Fatalf("Load packaged config in temp directory: %v", err)
	}
	return loaded
}
