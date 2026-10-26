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
// "https://" +, a Sprintf format with %s:// and one with a %s after
// its "://" (deferred minor A-14); exempt are the two lines that build
// no URL from a decoded path — each its exact text, once, so any other
// match in those files still fails (review I-1). An exemption that
// matched nothing fails too, so a deleted line takes its exemption with
// it (the H1 final review's tests M-6).
func TestNoAdHocUpstreamURLConcatenation(t *testing.T) {
	concat := regexp.MustCompile(`"(https?)?://"\s*\+|Sprintf\("[^"]*%s://|Sprintf\("[^"]*://[^"]*%s`)
	exempt := map[string]string{
		// print_apt_conf.go:70: the proxy's own address, host:port from
		// the configuration (resolveAdvertisedHostPort), for apt's
		// configuration; no path at all.
		filepath.Join("..", "..", "cmd", "apt-cacher-ultra", "print_apt_conf.go"): `proxyURL := "http://" + advertised`,
		// connect.go:591: the CONNECT tunnel's inner request: host and the
		// raw RequestURI as the client sent them (never a decoded path),
		// re-parsed by net/http like any request line.
		filepath.Join("..", "..", "internal", "proxy", "connect.go"): `syntheticURI := "https://" + target.LiteralHost + innerReq.RequestURI`,
	}
	used := map[string]bool{}
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
			src := string(b)
			allowed := exempt[path] // "" for every other file
			for _, m := range concat.FindAllStringIndex(src, -1) {
				start := strings.LastIndexByte(src[:m[0]], '\n') + 1
				end := len(src)
				if i := strings.IndexByte(src[m[0]:], '\n'); i >= 0 {
					end = m[0] + i
				}
				if allowed != "" && strings.TrimSpace(src[start:end]) == allowed {
					allowed, used[path] = "", true // the exempt line, once
					continue
				}
				t.Errorf("%s:%d: builds a URL by concatenation; use aptpath.BuildUpstreamURL",
					path, strings.Count(src[:m[0]], "\n")+1)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for path, line := range exempt {
		if !used[path] {
			t.Errorf("%s: the exempt line %q matched nothing; remove its exemption", path, line)
		}
	}
}
