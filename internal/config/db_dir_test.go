package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDatabaseDirectory(t *testing.T) {
	for _, mode := range []string{"omitted", "empty", "same", "separate", "missing", "file", "unwritable"} {
		t.Run(mode, func(t *testing.T) {
			dir, dbDir := t.TempDir(), t.TempDir()
			wantErr := ""
			switch mode {
			case "omitted", "empty":
				dbDir = ""
			case "same":
				dbDir = dir
			case "missing":
				dbDir = filepath.Join(dbDir, "missing")
				wantErr = "cache.db_dir"
			case "file":
				dbDir = filepath.Join(dbDir, "file")
				if err := os.WriteFile(dbDir, nil, 0600); err != nil {
					t.Fatal(err)
				}
				wantErr = "not a directory"
			case "unwritable":
				if os.Geteuid() == 0 {
					t.Skip("root bypasses directory permissions")
				}
				if err := os.Chmod(dbDir, 0500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(dbDir, 0700) })
				wantErr = "not writable"
			}
			body := fmt.Sprintf("[cache]\ndir = %q\n", dir)
			if mode != "omitted" {
				body += fmt.Sprintf("db_dir = %q\n", dbDir)
			}
			cfg, err := Load(writeTOML(t, dir, "config.toml", body))
			if wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), "cache.db_dir") || !strings.Contains(err.Error(), wantErr) {
					t.Fatalf("Load error = %v; want cache.db_dir and %q", err, wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := dbDir
			if want == "" {
				want = dir
			}
			if cfg.Cache.DatabaseDir() != want || cfg.Cache.Dir != dir {
				t.Fatalf("unexpected directories: %+v", cfg.Cache)
			}
		})
	}
}
