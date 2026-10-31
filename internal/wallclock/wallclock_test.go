package wallclock

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNow_TracksRealTimeWithZeroOffset(t *testing.T) {
	before := time.Now().Add(-time.Second)
	got := Now()
	after := time.Now().Add(time.Second)
	if got.Before(before) || got.After(after) {
		t.Fatalf("Now() = %v, want between %v and %v", got, before, after)
	}
}

func TestSetOffsetForTest_ShiftsAndRestores(t *testing.T) {
	restore := SetOffsetForTest(-2 * time.Hour)
	t.Cleanup(restore) // a failing assertion must not leak the offset (deferred minor A-1)
	shifted := Now()
	if d := time.Since(shifted); d < 2*time.Hour-time.Minute || d > 2*time.Hour+time.Minute {
		t.Fatalf("offset not applied: time.Since(Now()) = %v, want ~2h", d)
	}
	n, u := Now().Unix(), Unix() // read once each: a second boundary may fall between them
	if u != n && u != n+1 {
		t.Fatalf("Unix() = %d disagrees with Now().Unix() = %d", u, n)
	}
	restore()
	if d := time.Since(Now()); d > time.Minute || d < -time.Minute {
		t.Fatalf("offset not restored: %v", d)
	}
}

// Deferred minor A-2: production code never steps the wall clock — only
// tests call SetOffsetForTest.
func TestSetOffsetForTest_OnlyTestsCallIt(t *testing.T) {
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join("..", "..", dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fn := call.Fun.(type) {
				case *ast.SelectorExpr:
					ok = fn.Sel.Name == "SetOffsetForTest"
				case *ast.Ident:
					ok = fn.Name == "SetOffsetForTest"
				default:
					ok = false
				}
				if ok {
					t.Errorf("%s calls SetOffsetForTest outside a test", path)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestNow_HasNoMonotonicReading(t *testing.T) {
	// A wall-clock value must compare by wall time only, so test offsets
	// behave like real clock steps.
	if s := Now().String(); containsMonotonic(s) {
		t.Fatalf("Now() carries a monotonic reading: %s", s)
	}
}

func containsMonotonic(s string) bool {
	for i := 0; i+2 < len(s); i++ {
		if s[i:i+3] == "m=+" || s[i:i+3] == "m=-" {
			return true
		}
	}
	return false
}
