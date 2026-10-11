package aptpath

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Spec C1.3: every upstream URL built from a decoded path goes through
// BuildUpstreamURL. Concatenating "://" with a path re-parses decoded
// '#', '?' and '%' (S-3).
func TestNoAdHocUpstreamURLConcatenation(t *testing.T) {
	concat := regexp.MustCompile(`"://"\s*\+`)
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
			if concat.Match(b) {
				t.Errorf("%s: builds a URL by concatenation; use aptpath.BuildUpstreamURL", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
