package config

import "testing"

func TestAdminProfilingOptIn(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        bool
	}{
		{"default", "", false},
		{"enabled", "[admin]\npprof_enabled = true\n", true},
		{"disabled", "[admin]\npprof_enabled = false\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadConfigInTempDir(t, "[cache]\ndir = \""+DefaultCacheDir+"\"\n"+tc.input, t.TempDir())
			if cfg.Admin.PprofEnabled != tc.want {
				t.Errorf("admin.pprof_enabled = %v, want %v", cfg.Admin.PprofEnabled, tc.want)
			}
		})
	}
}
