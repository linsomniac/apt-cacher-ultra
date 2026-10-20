package cache

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// insertSuiteFreshnessRE matches a statement that inserts into
// suite_freshness: INSERT (OR IGNORE / OR REPLACE …) or REPLACE INTO,
// across line breaks, any case, with the table name quoted ("…", '…',
// `…`, […]: SQLite takes a single-quoted string as an identifier where
// one is expected) or schema-qualified (main.suite_freshness) too. SQL
// built at runtime is beyond any literal scan.
var insertSuiteFreshnessRE = regexp.MustCompile("(?is)\\b(?:INSERT(?:\\s+OR\\s+\\w+)?|REPLACE)\\s+INTO\\s+" +
	"(?:[\"'`\\[]?\\w+[\"'`\\]]?\\s*\\.\\s*)?[\"'`\\[]?suite_freshness\\b")

// deleteSuiteFreshnessRE matches a statement that deletes from
// suite_freshness, with the same quoting and schema forms.
var deleteSuiteFreshnessRE = regexp.MustCompile("(?is)\\bDELETE\\s+FROM\\s+" +
	"(?:[\"'`\\[]?\\w+[\"'`\\]]?\\s*\\.\\s*)?[\"'`\\[]?suite_freshness\\b")

// deleteSignedSeenRE matches a statement that deletes from
// suite_signed_seen, with the same quoting and schema forms.
var deleteSignedSeenRE = regexp.MustCompile("(?is)\\bDELETE\\s+FROM\\s+" +
	"(?:[\"'`\\[]?\\w+[\"'`\\]]?\\s*\\.\\s*)?[\"'`\\[]?suite_signed_seen\\b")

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

// Ruling R-F1 (spec C3, C9): deleteSuiteTx — the compare-and-delete cap
// eviction, the reaper and POST /forget share — holds the only DELETE FROM
// suite_freshness in production code.
func TestSuiteFreshnessHasOneDeleteSite(t *testing.T) {
	var sites []string
	productionGoFiles(t, func(p string, src []byte) {
		stringLiterals(t, p, src, func(funcName, lit string) {
			for range deleteSuiteFreshnessRE.FindAllStringIndex(lit, -1) {
				sites = append(sites, p+":"+funcName)
			}
		})
	})
	const want = "../../internal/cache/suite_reap.go:deleteSuiteTx"
	if len(sites) != 1 || sites[0] != want {
		t.Fatalf("DELETE FROM suite_freshness sites = %v, want exactly [%s]", sites, want)
	}
}

// callSites returns "file:function" for every reference to the package
// function name in internal/cache's production code, sorted: a call, and
// equally the function taken as a value (f := name, a callback argument),
// from which a call could be made anywhere, and any selector x.name. A
// reference outside any function — a package-level var, or a func literal
// assigned to one — is attributed to "(top level)". The function's own
// declaration is not a reference (a recursive call in it adds no caller).
//
// AIDEV-NOTE: every identifier spelled name counts, not only call.Fun
// (Task 2 review minor 4): counting only direct calls in FuncDecls let a
// function value or a package-level func literal reach the guarded
// writer unseen. A local variable or field that happens to share the name
// is flagged too — rename it rather than widen the allowlist.
func callSites(t *testing.T, name string) []string {
	t.Helper()
	var out []string
	productionGoFiles(t, func(p string, src []byte) {
		if !strings.HasPrefix(p, "../../internal/cache/") {
			return
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			where := "(top level)"
			if fd, ok := d.(*ast.FuncDecl); ok {
				if fd.Recv == nil && fd.Name.Name == name {
					continue // the declaration itself
				}
				where = fd.Name.Name
			}
			ast.Inspect(d, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && id.Name == name {
					if fd, ok := d.(*ast.FuncDecl); ok && id == fd.Name {
						return true // a method's own name is its declaration, not a reference
					}
					out = append(out, filepath.Base(p)+":"+where)
				}
				return true
			})
		}
	})
	slices.Sort(out)
	return out
}

// Deferred minor D-8 and ruling R-F1: the one INSERT and the one DELETE
// of suite_freshness are reached only from known callers, so a new writer
// that "re-creates on 0 rows", or a second delete path, fails here.
func TestSuiteRowWritersHaveKnownCallers(t *testing.T) {
	for _, c := range []struct {
		fn   string
		want []string
	}{
		{"createSuiteTx", []string{"fetched_row.go:storeFetchedTx", "suite_rows.go:CreateSuite", "testing_helpers.go:SeedSuiteForTest"}},
		{"deleteSuiteTx", []string{"suite_reap.go:ForgetSuite", "suite_reap.go:ReapIdleSuite", "suite_rows.go:createSuiteTx"}},
	} {
		if got := callSites(t, c.fn); !slices.Equal(got, c.want) {
			t.Errorf("%s callers = %v, want %v", c.fn, got, c.want)
		}
	}
}

// Spec C3: only the handler creates suite rows. Outside internal/cache no
// production code but internal/handler calls CreateSuite or StoreFetched,
// and no production code calls a test-only seeding helper, nor
// ClearSignedSeenForTest, which deletes every signed-seen marker (design
// review M-5).
func TestOnlyTheHandlerCreatesSuiteRows(t *testing.T) {
	creators := map[string]bool{"CreateSuite": true, "StoreFetched": true}
	seeders := map[string]bool{"SeedSuiteForTest": true, "EnsureSuiteForTest": true, "InsertTrackedCandidateForTest": true,
		"ClearSignedSeenForTest": true}
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

// Ruling R-F1 across packages (design review M-5): outside internal/cache
// only the suite reaper (internal/freshness/reaper.go) and POST /forget
// (internal/freshness/forget.go) name the cache's suite deleters — each
// holds the suite through the SuiteGuard first — so a new caller that
// skips the guard fails here. Any selector naming a deleter counts, not
// only a call: a method value (f := c.ForgetSuite) or a method expression
// ((*cache.Cache).DeleteSuiteMetadata) handed elsewhere could be called
// through a func value (the Task 3 review's minor 5).
func TestSuiteDeletersHaveKnownCallers(t *testing.T) {
	deleters := map[string]bool{"ReapIdleSuite": true, "ForgetSuite": true, "DeleteSuiteMetadata": true}
	allowed := map[string]bool{"../../internal/freshness/reaper.go": true, "../../internal/freshness/forget.go": true}
	productionGoFiles(t, func(p string, src []byte) {
		if strings.HasPrefix(p, "../../internal/cache/") || allowed[p] {
			return
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, p, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && deleters[sel.Sel.Name] {
				t.Errorf("%s names %s: only the reaper and POST /forget delete suites", fset.Position(sel.Pos()), sel.Sel.Name)
			}
			return true
		})
	})
}

// Design review M-5: the never-downgrade authority (spec C5) is deleted
// only by POST /forget (ForgetSuite), the year-old orphan sweep
// (deleteOrphanMarkers, ruling R-F5) and the test-only
// ClearSignedSeenForTest, so a new path that deletes signed-seen markers
// fails here.
func TestSignedSeenHasKnownDeleteSites(t *testing.T) {
	var sites []string
	productionGoFiles(t, func(p string, src []byte) {
		stringLiterals(t, p, src, func(funcName, lit string) {
			for range deleteSignedSeenRE.FindAllStringIndex(lit, -1) {
				sites = append(sites, p+":"+funcName)
			}
		})
	})
	slices.Sort(sites)
	want := []string{"../../internal/cache/marker_sweep.go:deleteOrphanMarkers", "../../internal/cache/suite_reap.go:ForgetSuite",
		"../../internal/cache/testing_helpers.go:ClearSignedSeenForTest"}
	if !slices.Equal(sites, want) {
		t.Fatalf("DELETE FROM suite_signed_seen sites = %v, want %v", sites, want)
	}
}
