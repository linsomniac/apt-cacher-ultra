package aptpath

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// nonTestImports returns the import paths of dir's non-test Go files.
func nonTestImports(t *testing.T, dir string) map[string][]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			out[name] = append(out[name], p)
		}
	}
	return out
}

// aptpath is a leaf: standard library only (a stdlib path has no dot in
// its first element).
func TestAptpathImportsOnlyStdlib(t *testing.T) {
	for file, imps := range nonTestImports(t, ".") {
		for _, p := range imps {
			if strings.Contains(strings.SplitN(p, "/", 2)[0], ".") {
				t.Errorf("%s imports non-stdlib %q", file, p)
			}
		}
	}
}

// internal/cache must not import internal/proxy; shared path logic lives here.
func TestCacheDoesNotImportProxy(t *testing.T) {
	for file, imps := range nonTestImports(t, filepath.Join("..", "cache")) {
		for _, p := range imps {
			if strings.HasSuffix(p, "/internal/proxy") {
				t.Errorf("internal/cache/%s imports %q", file, p)
			}
		}
	}
}
