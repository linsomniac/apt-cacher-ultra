package flipgate

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The gate is a leaf: the standard library and internal/metrics only
// (spec C8: the handler and the adopter both import it).
func TestImportsAreLeaf(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			stdlib := !strings.Contains(strings.SplitN(p, "/", 2)[0], ".")
			if !stdlib && p != "github.com/linsomniac/apt-cacher-ultra/internal/metrics" {
				t.Errorf("%s imports %q", name, p)
			}
		}
	}
}
