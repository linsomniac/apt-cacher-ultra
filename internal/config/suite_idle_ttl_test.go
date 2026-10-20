package config

import (
	"strings"
	"testing"
	"time"
)

// Spec C11: gc.suite_idle_ttl defaults to 720h; 0s (disabled) and values
// of at least 24h load; a shorter or a negative one is refused, naming the
// key, whatever gc.enabled says (the reaper runs with the checker).
func TestLoad_SuiteIdleTTL(t *testing.T) {
	dir := t.TempDir()
	load := func(name, extra string) (*Config, error) {
		t.Helper()
		return Load(writeTOML(t, dir, name, "[cache]\ndir = \""+dir+"\"\n"+extra))
	}
	cfg, err := load("default.toml", "")
	if err != nil || cfg.GC.SuiteIdleTTL.Duration != 720*time.Hour {
		t.Fatalf("default: %v, %v; want 720h", cfg.GC.SuiteIdleTTL.Duration, err)
	}
	for i, ok := range []time.Duration{0, 24 * time.Hour, 8760 * time.Hour} {
		cfg, err := load("ok.toml", "[gc]\nsuite_idle_ttl = \""+ok.String()+"\"\n")
		if err != nil || cfg.GC.SuiteIdleTTL.Duration != ok {
			t.Errorf("case %d: suite_idle_ttl %s loaded as %v, %v", i, ok, cfg.GC.SuiteIdleTTL.Duration, err)
		}
	}
	for _, bad := range []string{"23h59m", "-1h"} {
		for _, enabled := range []string{"true", "false"} {
			_, err := load("bad.toml", "[gc]\nenabled = "+enabled+"\nsuite_idle_ttl = \""+bad+"\"\n")
			if err == nil || !strings.Contains(err.Error(), "gc.suite_idle_ttl") {
				t.Errorf("suite_idle_ttl %s with gc.enabled = %s: %v; want a refusal naming the key", bad, enabled, err)
			}
		}
	}
}
