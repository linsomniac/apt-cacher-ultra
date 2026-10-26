package cache

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Spec §6 / overview Global Constraints: production SQL never reads
// SQLite's clock, because the Go wall clock (internal/wallclock) is the
// only clock simulations can control.
func TestNoSQLiteClockFunctionsInProductionCode(t *testing.T) {
	// A substring match: CURRENT_TIME also catches CURRENT_TIMESTAMP
	// (deferred minor A-20).
	forbidden := []string{
		"strftime(", "julianday(", "unixepoch(", "datetime(",
		"CURRENT_TIME", "CURRENT_DATE", "'now'",
	}
	for _, root := range []string{filepath.Join("..", "..", "cmd"), filepath.Join("..", "..", "internal")} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, f := range forbidden {
				if strings.Contains(string(b), f) {
					t.Errorf("%s uses SQLite clock function %q", path, f)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
