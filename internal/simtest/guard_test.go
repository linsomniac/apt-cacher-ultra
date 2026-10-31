package simtest

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// simtest imports "testing", so production code must never import it.
func TestNoProductionCodeImportsSimtest(t *testing.T) {
	const root = "../.."
	const imp = `"github.com/linsomniac/apt-cacher-ultra/internal/simtest"`
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		if strings.HasPrefix(filepath.ToSlash(p), root+"/internal/simtest/") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), imp) {
			t.Errorf("%s imports internal/simtest", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
