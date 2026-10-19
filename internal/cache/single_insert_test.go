package cache

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// insertSuiteFreshnessRE matches a statement that inserts into
// suite_freshness: INSERT (OR IGNORE / OR REPLACE …) or REPLACE INTO,
// across line breaks, any case, with the table name quoted ("…", `…`,
// […]) or schema-qualified (main.suite_freshness) too.
var insertSuiteFreshnessRE = regexp.MustCompile("(?is)\\b(?:INSERT(?:\\s+OR\\s+\\w+)?|REPLACE)\\s+INTO\\s+" +
	"(?:[\"`\\[]?\\w+[\"`\\]]?\\s*\\.\\s*)?[\"`\\[]?suite_freshness\\b")

// productionGoFiles calls fn for every non-test .go file of the module
// (internal/cache is two levels below its root).
func productionGoFiles(t *testing.T, fn func(path string, src []byte)) {
	t.Helper()
	root := filepath.Join("..", "..")
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
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fn(filepath.ToSlash(p), b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// stringLiterals calls fn with every string literal of a Go source file,
// unquoted, and with every concatenation made only of string literals
// ("INSERT INTO " + "suite_freshness …") folded into one string, each with
// the name of the function declaration that holds it. Comments are not
// literals, so prose that mentions an INSERT never counts.
func stringLiterals(t *testing.T, path string, src []byte, fn func(funcName, lit string)) {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var fold func(e ast.Expr) (string, bool)
	fold = func(e ast.Expr) (string, bool) {
		switch x := e.(type) {
		case *ast.BasicLit:
			if x.Kind != token.STRING {
				return "", false
			}
			s, err := strconv.Unquote(x.Value)
			return s, err == nil
		case *ast.ParenExpr:
			return fold(x.X)
		case *ast.BinaryExpr:
			if x.Op != token.ADD {
				return "", false
			}
			l, okL := fold(x.X)
			r, okR := fold(x.Y)
			return l + r, okL && okR
		}
		return "", false
	}
	for _, d := range f.Decls {
		name := "(top level)"
		if fd, ok := d.(*ast.FuncDecl); ok {
			name = fd.Name.Name
		}
		ast.Inspect(d, func(n ast.Node) bool {
			if e, ok := n.(ast.Expr); ok {
				if s, ok := fold(e); ok {
					fn(name, s)
					return false // a folded concatenation's parts are not visited again
				}
			}
			return true
		})
	}
}

// Spec C3: CreateSuite's createSuiteTx holds the only INSERT INTO
// suite_freshness in production code, so no other writer can create — or
// resurrect — a suite row.
func TestSuiteFreshnessHasOneInsertSite(t *testing.T) {
	var sites []string
	productionGoFiles(t, func(p string, src []byte) {
		stringLiterals(t, p, src, func(funcName, lit string) {
			for range insertSuiteFreshnessRE.FindAllStringIndex(lit, -1) {
				sites = append(sites, p+":"+funcName)
			}
		})
	})
	const want = "../../internal/cache/suite_rows.go:createSuiteTx"
	if len(sites) != 1 || sites[0] != want {
		t.Fatalf("INSERT INTO suite_freshness sites = %v, want exactly [%s]", sites, want)
	}
}

// Spec C3: only the handler creates suite rows. Outside internal/cache no
// production code but internal/handler calls CreateSuite or StoreFetched,
// and no production code calls a test-only seeding helper.
func TestOnlyTheHandlerCreatesSuiteRows(t *testing.T) {
	creators := map[string]bool{"CreateSuite": true, "StoreFetched": true}
	seeders := map[string]bool{"SeedSuiteForTest": true, "EnsureSuiteForTest": true, "InsertTrackedCandidateForTest": true}
	productionGoFiles(t, func(p string, src []byte) {
		inCache := strings.HasPrefix(p, "../../internal/cache/")
		inHandler := strings.HasPrefix(p, "../../internal/handler/")
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, p, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				name = fn.Sel.Name
			case *ast.Ident:
				name = fn.Name
			}
			switch {
			case creators[name] && !inCache && !inHandler:
				t.Errorf("%s calls %s: only internal/handler creates suite rows", fset.Position(call.Pos()), name)
			case seeders[name] && (!inCache || !strings.HasSuffix(p, "/testing_helpers.go")):
				t.Errorf("%s calls the test-only %s", fset.Position(call.Pos()), name)
			}
			return true
		})
	})
}
