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
// '#', '?' and '%' (S-3). The pattern covers "://" +, "http://" +,
// "https://" + and a Sprintf format with %s:// (deferred minor A-14);
// exempt are the two that build no URL from a decoded path.
func TestNoAdHocUpstreamURLConcatenation(t *testing.T) {
	concat := regexp.MustCompile(`"(https?)?://"\s*\+|Sprintf\("[^"]*%s://`)
	exempt := map[string]bool{
		// The proxy's own address, for apt's configuration.
		filepath.Join("..", "..", "cmd", "apt-cacher-ultra", "print_apt_conf.go"): true,
		// The CONNECT tunnel's inner request: host and RequestURI as the
		// client sent them, re-parsed by net/http like any request line.
		filepath.Join("..", "..", "internal", "proxy", "connect.go"): true,
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
			if concat.Match(b) && !exempt[path] {
				t.Errorf("%s: builds a URL by concatenation; use aptpath.BuildUpstreamURL", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
